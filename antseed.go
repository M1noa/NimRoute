// antseed p2p backend: discovery via the stats snapshot, free text offers
// only, offer failover on 429 with identity rotation. mirrors the freepi
// handler shape (fetch whole, convert here).

package main

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// antseedStatsDefault is polled for peer discovery; the dht is skipped.
const antseedStatsDefault = "https://network.antseed.com/stats"

// antTokenPricing mirrors TokenPricingUsdPerMillion in peer-metadata.ts.
type antTokenPricing struct {
	Input  float64  `json:"inputUsdPerMillion"`
	Output float64  `json:"outputUsdPerMillion"`
	Cached *float64 `json:"cachedInputUsdPerMillion"`
}

// antProvider mirrors ProviderAnnouncement (fields we route on only).
type antProvider struct {
	Provider   string                     `json:"provider"`
	Services   []string                   `json:"services"`
	Default    antTokenPricing            `json:"defaultPricing"`
	PerService map[string]antTokenPricing `json:"servicePricing"`
	Protocols  map[string][]string        `json:"serviceApiProtocols"`
}

// antPeer mirrors PeerMetadata (fields we route on only).
type antPeer struct {
	PeerID    string        `json:"peerId"`
	Display   string        `json:"displayName"`
	Addr      string        `json:"publicAddress"`
	Providers []antProvider `json:"providers"`
	Rep       *float64      `json:"reputationScore"`
	EffRep    *float64      `json:"effectiveReputationScore"`
}

// antOffer is one routable free text offer from one seller.
type antOffer struct {
	peerID   string
	hostPort string
	fallback []string
	provider string
	service  string // seller's advertised service id
	model    string // display id (service base form)
	rep      float64
}

// antseedEnabled gates the backend.
func antseedEnabled() bool {
	return cfg().Antseed.Enabled
}

// antPreferFree defaults true when unset (*bool sidesteps presence checks).
func antPreferFree() bool {
	if p := cfg().Antseed.PreferFree; p != nil {
		return *p
	}
	return true
}

var (
	antMu2      sync.Mutex
	antOffers   []antOffer
	antModels   []string // free text model ids, first-seen order
	antFetched  time.Time
	antFetching bool
	antCool     = map[string]time.Time{} // peerID -> cooldown until
	antConns    = map[string]*antConn{}
	antProxyIdx uint64
	antIDBorn   time.Time
	antBornMu   sync.Mutex
)

// antCanon is the match key: vendor path dropped, alphanumerics only.
func antCanon(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// antBase is the display id: vendor path dropped, rest verbatim.
func antBase(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// antTextProto reports whether the offer can serve chat completions.
func antTextProto(protos []string) bool {
	if len(protos) == 0 {
		return true // unannounced: try openai chat
	}
	for _, p := range protos {
		switch p {
		case "openai-chat-completions", "anthropic-messages", "openai-responses":
			return true
		}
	}
	return false
}

// antPrice resolves service pricing, falling back to the default.
func antPrice(p antProvider, svc string) antTokenPricing {
	if t, ok := p.PerService[svc]; ok {
		return t
	}
	return p.Default
}

// antFree reports zero-price (cached falls back to input, like the seller).
func antFree(t antTokenPricing) bool {
	cached := t.Input
	if t.Cached != nil {
		cached = *t.Cached
	}
	return t.Input == 0 && t.Output == 0 && cached == 0
}

// antHostPort fills the default seller port when the address has none.
func antHostPort(addr string) string {
	if addr == "" {
		return ""
	}
	if strings.LastIndex(addr, ":") <= strings.LastIndex(addr, "]") ||
		!strings.Contains(addr, ":") {
		return addr + ":6882"
	}
	return addr
}

func antDHTValue(buf []byte, at, depth int) (any, int, bool) {
	if at >= len(buf) || depth > 8 {
		return nil, at, false
	}
	switch buf[at] {
	case 'd', 'l':
		dict := buf[at] == 'd'
		at++
		var entries map[string]any
		var values []any
		if dict {
			entries = map[string]any{}
		}
		for at < len(buf) && buf[at] != 'e' {
			if dict {
				key, next, ok := antDHTValue(buf, at, depth+1)
				if !ok {
					return nil, at, false
				}
				name, ok := key.([]byte)
				if !ok {
					return nil, at, false
				}
				at = next
				value, next, ok := antDHTValue(buf, at, depth+1)
				if !ok {
					return nil, at, false
				}
				entries[string(name)] = value
				at = next
			} else {
				value, next, ok := antDHTValue(buf, at, depth+1)
				if !ok {
					return nil, at, false
				}
				values = append(values, value)
				at = next
			}
		}
		if at >= len(buf) {
			return nil, at, false
		}
		if dict {
			return entries, at + 1, true
		}
		return values, at + 1, true
	case 'i':
		end := at + 1
		for end < len(buf) && buf[end] != 'e' {
			end++
		}
		if end >= len(buf) {
			return nil, at, false
		}
		n, err := strconv.ParseInt(string(buf[at+1:end]), 10, 64)
		return n, end + 1, err == nil
	default:
		end := at
		for end < len(buf) && buf[end] >= '0' && buf[end] <= '9' {
			end++
		}
		if end == at || end >= len(buf) || buf[end] != ':' || end-at > 6 {
			return nil, at, false
		}
		n, err := strconv.Atoi(string(buf[at:end]))
		if err != nil || n > len(buf)-end-1 {
			return nil, at, false
		}
		return buf[end+1 : end+1+n], end + 1 + n, true
	}
}

func antDHTEndpoint(data []byte) string {
	if len(data) != 6 && len(data) != 18 {
		return ""
	}
	ip := net.IP(data[:len(data)-2])
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return ""
	}
	port := binary.BigEndian.Uint16(data[len(data)-2:])
	if port == 0 {
		return ""
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(int(port)))
}

func antDHTLookup(peerID string) []string {
	return antDHTLookupAt(peerID, []string{"dht1.antseed.com:6881", "dht2.antseed.com:6881"})
}

func antDHTLookupAt(peerID string, bootstrap []string) []string {
	peer, err := hex.DecodeString(strings.TrimPrefix(peerID, "0x"))
	if err != nil || len(peer) != 20 {
		return nil
	}
	hash := sha1.Sum([]byte("antseed:peer:" + strings.ToLower(strings.TrimPrefix(peerID, "0x"))))
	var id [20]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil
	}
	query := append([]byte("d1:ad2:id20:"), id[:]...)
	query = append(query, []byte("9:info_hash20:")...)
	query = append(query, hash[:]...)
	query = append(query, []byte("e1:q9:get_peers1:t2:aa1:y1:qe")...)
	queue := append([]string{}, bootstrap...)
	seen := map[string]bool{}
	found := []string{}
	foundSet := map[string]bool{}
	until := time.Now().Add(5 * time.Second)
	for len(queue) > 0 && len(seen) < 32 && time.Now().Before(until) {
		if len(seen) >= len(bootstrap) && len(found) > 0 {
			break
		}
		addr := queue[0]
		queue = queue[1:]
		if seen[addr] {
			continue
		}
		seen[addr] = true
		conn, err := net.DialTimeout("udp", addr, 500*time.Millisecond)
		if err != nil {
			continue
		}
		conn.SetDeadline(time.Now().Add(450 * time.Millisecond))
		_, err = conn.Write(query)
		var reply [65535]byte
		var n int
		if err == nil {
			n, err = conn.Read(reply[:])
		}
		conn.Close()
		if err != nil {
			continue
		}
		value, end, ok := antDHTValue(reply[:n], 0, 0)
		if !ok || end != n {
			continue
		}
		root, ok := value.(map[string]any)
		if !ok || stringValue(root["y"]) != "r" || stringValue(root["t"]) != "aa" {
			continue
		}
		response, ok := root["r"].(map[string]any)
		if !ok {
			continue
		}
		if values, ok := response["values"].([]any); ok {
			for _, value := range values {
				if entry, ok := value.([]byte); ok {
					if endpoint := antDHTEndpoint(entry); endpoint != "" && !foundSet[endpoint] {
						found = append(found, endpoint)
						foundSet[endpoint] = true
					}
				}
			}
		}
		if nodes, ok := response["nodes"].([]byte); ok {
			for i := 0; i+26 <= len(nodes) && len(queue)+len(seen) < 32; i += 26 {
				if endpoint := antDHTEndpoint(nodes[i+20 : i+26]); endpoint != "" && !seen[endpoint] {
					queue = append(queue, endpoint)
				}
			}
		}
	}
	return found
}

func stringValue(v any) string {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return ""
}

// antRefresh polls the stats snapshot and rebuilds offers. stale cache is
// served when the poll fails; one flight at a time.
func antRefresh() {
	antMu2.Lock()
	if antFetching {
		antMu2.Unlock()
		return
	}
	secs := cfg().Antseed.RefreshSecs
	if secs <= 0 {
		secs = 300
	}
	if !antFetched.IsZero() && time.Since(antFetched) < time.Duration(secs)*time.Second {
		antMu2.Unlock()
		return
	}
	antFetching = true
	antMu2.Unlock()

	offers, models := antPoll()

	antMu2.Lock()
	antFetching = false
	if offers != nil {
		antOffers = offers
		antModels = models
		antFetched = time.Now()
	}
	antMu2.Unlock()
}

// antPoll fetches the snapshot and extracts free text offers.
func antPoll() ([]antOffer, []string) {
	return antPollWithLookup(antDHTLookup)
}

func antPollWithLookup(lookup func(string) []string) ([]antOffer, []string) {
	url := cfg().Antseed.StatsURL
	if url == "" {
		url = antseedStatsDefault
	}
	cl := &http.Client{Timeout: 20 * time.Second}
	resp, err := cl.Get(url)
	if err != nil {
		acclog.Printf("!! antseed stats poll: %v", err)
		return nil, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	var snap struct {
		Peers []antPeer `json:"peers"`
	}
	if json.Unmarshal(body, &snap) != nil {
		acclog.Printf("!! antseed stats poll: bad snapshot")
		return nil, nil
	}
	var offers []antOffer
	var models []string
	seenModel := map[string]bool{}
	seenOffer := map[string]bool{} // peerID|service: cheapest-shape dedupe
	for _, peer := range snap.Peers {
		hp := antHostPort(peer.Addr)
		var fallback []string
		resolved := false
		rep := 0.0
		if peer.EffRep != nil {
			rep = *peer.EffRep
		} else if peer.Rep != nil {
			rep = *peer.Rep
		}
		pid := strings.ToLower(peer.PeerID)
		for _, p := range peer.Providers {
			for _, svc := range p.Services {
				if !antFree(antPrice(p, svc)) {
					continue
				}
				if !antTextProto(p.Protocols[svc]) {
					continue
				}
				if hp == "" && !resolved {
					if eps := lookup(pid); len(eps) > 0 {
						hp, fallback = eps[0], eps[1:]
					}
					resolved = true
				}
				if hp == "" {
					continue
				}
				key := pid + "|" + antCanon(svc)
				if seenOffer[key] {
					continue
				}
				seenOffer[key] = true
				model := antBase(svc)
				offers = append(offers, antOffer{
					peerID: pid, hostPort: hp, fallback: fallback,
					provider: p.Provider, service: svc,
					model: model, rep: rep,
				})
				if ck := antCanon(model); !seenModel[ck] {
					seenModel[ck] = true
					models = append(models, model)
				}
			}
		}
	}
	return offers, models
}

// antRoutes returns cooled-aware offers for a model, cheapest first.
func antRoutes(model string) []antOffer {
	antRefresh()
	want := antCanon(model)
	now := time.Now()
	var out []antOffer
	antMu2.Lock()
	for _, o := range antOffers {
		if antCanon(o.model) != want && antCanon(o.service) != want {
			continue
		}
		if until, ok := antCool[o.peerID]; ok && now.Before(until) {
			continue
		}
		out = append(out, o)
	}
	antMu2.Unlock()
	trust := cfg().Antseed.TrustMin
	free := antPreferFree()
	eligible := out[:0:0]
	for _, o := range out {
		if trust > 0 && o.rep < trust {
			continue
		}
		eligible = append(eligible, o)
	}
	// free first when preferred, then reputation, then peer id.
	for i := 1; i < len(eligible); i++ {
		for j := i; j > 0; j-- {
			a, b := eligible[j], eligible[j-1]
			swap := false
			if free && a.rep == b.rep {
				swap = a.peerID < b.peerID
			} else if a.rep != b.rep {
				swap = a.rep > b.rep
			} else {
				swap = a.peerID < b.peerID
			}
			if !swap {
				break
			}
			eligible[j], eligible[j-1] = eligible[j-1], eligible[j]
		}
	}
	return eligible
}

// antProxy picks the next forced proxy, "" = direct.
func antProxy() string {
	px := cfg().Antseed.Proxies
	if len(px) == 0 {
		return ""
	}
	return px[int(atomic.AddUint64(&antProxyIdx, 1)-1)%len(px)]
}

// antConnFor dials or reuses the seller connection.
func antConnFor(o antOffer) (*antConn, error) {
	antMu2.Lock()
	c := antConns[o.peerID]
	antMu2.Unlock()
	if c != nil {
		return c, nil
	}
	var err error
	for _, hp := range append([]string{o.hostPort}, o.fallback...) {
		if c, err = antOpen(o.peerID, hp, antProxy(), 15*time.Second); err == nil {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	antMu2.Lock()
	if old := antConns[o.peerID]; old != nil {
		antMu2.Unlock()
		c.conn.Close()
		return old, nil
	}
	antConns[o.peerID] = c
	antMu2.Unlock()
	antStampID()
	return c, nil
}

// antDrop closes and forgets the seller connection.
func antDrop(peerID string) {
	antMu2.Lock()
	c := antConns[peerID]
	delete(antConns, peerID)
	antMu2.Unlock()
	if c != nil {
		c.conn.Close()
	}
}

// antCoolPeer parks a failing seller for a minute.
func antCoolPeer(peerID string) {
	antMu2.Lock()
	antCool[peerID] = time.Now().Add(time.Minute)
	antMu2.Unlock()
}

// antStampID records identity birth (for /status age).
func antStampID() {
	antBornMu.Lock()
	if antIDBorn.IsZero() {
		antIDBorn = time.Now()
	}
	antBornMu.Unlock()
}

// antFailover drops the conn, cools the seller, rotates identity.
func antFailover(peerID string, why string) {
	acclog.Printf("  antseed failover peer=%s: %s", peerID, why)
	antDrop(peerID)
	antCoolPeer(peerID)
	antRotateIdentity()
	antBornMu.Lock()
	antIDBorn = time.Now()
	antBornMu.Unlock()
}

// antTry sends one request to one offer: free-usage open (best effort),
// then the chat completion. returns seller body on 200.
func antTry(o antOffer, oaiBody []byte) ([]byte, int, bool) {
	c, err := antConnFor(o)
	if err != nil {
		antFailover(o.peerID, err.Error())
		return nil, 0, true
	}
	c.antOpenFree(o.peerID)
	var m map[string]any
	if json.Unmarshal(oaiBody, &m) != nil {
		return nil, 0, false
	}
	m["model"] = o.service
	stripCacheFields(m)
	m["stream"] = false
	b, _ := json.Marshal(m)
	q := antHTTPReq{
		id:     randHex(16),
		method: "POST",
		path:   "/v1/chat/completions",
		headers: map[string]string{
			"content-type":       "application/json",
			"x-antseed-provider": o.provider,
			"x-antseed-service":  o.service,
		},
		body: b,
	}
	status, _, rb, err := c.antDoRequest(q, 300*time.Second)
	if err != nil {
		antFailover(o.peerID, err.Error())
		return nil, 0, true
	}
	switch {
	case status == 200:
		return rb, status, false
	case status == 429 || status == 529:
		antFailover(o.peerID, fmt.Sprintf("http %d", status))
		return nil, status, true
	case status == 402:
		return nil, status, false // paid: fail the request, never retry
	default:
		antCoolPeer(o.peerID)
		return nil, status, true
	}
}

// handleAntseedAnthropic serves /v1/messages for antseed/<model>: offer
// failover, then converts the openai answer back to Anthropic shape.
func (p *Pool) handleAntseedAnthropic(w http.ResponseWriter, r *http.Request, oaiBody []byte, clientModel, upstreamModel string, tnames toolNames, isStream bool, start time.Time, reqID string) {
	realModel := strings.TrimPrefix(upstreamModel, "antseed/")
	routes := antRoutes(realModel)
	if len(routes) == 0 {
		writeAnthropicError(w, http.StatusNotFound, "not_found_error", "antseed: no free offers for "+realModel)
		return
	}
	var rb []byte
	var peer string
	var status int
	for _, o := range routes {
		if r.Context().Err() != nil {
			writeAnthropicError(w, 499, "api_error", "client disconnected")
			return
		}
		b, st, cont := antTry(o, oaiBody)
		status = st
		if b != nil {
			rb, peer = b, o.peerID
			break
		}
		if !cont {
			break
		}
	}
	if rb == nil {
		if status == 402 {
			writeAnthropicError(w, http.StatusPaymentRequired, "api_error", "antseed: seller demands payment for "+realModel)
			return
		}
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "antseed: all offers failed for "+realModel)
		return
	}
	u := respUsage(rb)
	logReq := func(stream bool, contentBytes int64) {
		logUsage(UsageRecord{
			Ts:    time.Now().UTC().Format(time.RFC3339Nano),
			Model: upstreamModel, KeyName: "antseed:" + peer, Method: r.Method,
			Path: r.URL.Path, StatusCode: 200,
			DurationMs:   time.Since(start).Milliseconds(),
			PromptTokens: u.prompt, CompletionTokens: u.completion, TotalTokens: u.total,
			Stream: stream, ContentBytes: contentBytes,
		})
	}
	if isStream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		written, _, _ := streamAnthropic(w, rb, clientModel, tnames)
		logReq(true, written)
		acclog.Printf("<- 200 POST /v1/messages %v %d bytes [antseed:%s] model=%s %s",
			time.Since(start).Round(time.Millisecond), written, peer, realModel, reqID)
		return
	}
	out, errMsg, _ := openAIToAnthropic(rb, clientModel, tnames)
	if errMsg != "" {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", errMsg)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(out)
	logReq(false, int64(len(out)))
	acclog.Printf("<- 200 POST /v1/messages %v %d bytes [antseed:%s] model=%s %s",
		time.Since(start).Round(time.Millisecond), len(out), peer, realModel, reqID)
}

// handleAntseed routes antseed/<model> openai-passthrough with failover.
func (p *Pool) handleAntseed(w http.ResponseWriter, r *http.Request, body []byte, model string, isStream bool, start time.Time, reqID string) {
	realModel := strings.TrimPrefix(model, "antseed/")
	routes := antRoutes(realModel)
	if len(routes) == 0 {
		http.Error(w, `{"error":"antseed: no free offers for `+realModel+`"}`, http.StatusNotFound)
		return
	}
	var rb []byte
	var peer string
	var status int
	for _, o := range routes {
		if r.Context().Err() != nil {
			http.Error(w, `{"error":"client disconnected"}`, 499)
			return
		}
		b, st, cont := antTry(o, body)
		status = st
		if b != nil {
			rb, peer = b, o.peerID
			break
		}
		if !cont {
			break
		}
	}
	if rb == nil {
		if status == 402 {
			http.Error(w, `{"error":"antseed: seller demands payment"}`, http.StatusPaymentRequired)
			return
		}
		http.Error(w, `{"error":"antseed: all offers failed for `+realModel+`"}`, http.StatusBadGateway)
		return
	}
	u := respUsage(rb)
	if !isStream {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(http.StatusOK)
	w.Write(rb)
	logUsage(UsageRecord{
		Ts:    time.Now().UTC().Format(time.RFC3339Nano),
		Model: model, KeyName: "antseed:" + peer, Method: r.Method,
		Path: r.URL.Path, StatusCode: 200,
		DurationMs:   time.Since(start).Milliseconds(),
		PromptTokens: u.prompt, CompletionTokens: u.completion, TotalTokens: u.total,
		Stream: isStream, ContentBytes: int64(len(rb)),
	})
	acclog.Printf("<- 200 %s %s %v %d bytes [antseed:%s] model=%s %s",
		r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond), len(rb), peer, realModel, reqID)
}

// antseedSummary is the /status antseed section. peer ids stay on the
// machine only in aggregate; identity age shows rotation is live.
type antseedSummary struct {
	Enabled  bool     `json:"enabled"`
	Models   []string `json:"models,omitempty"`
	Offers   int      `json:"offers"`
	Peers    int      `json:"peers"`
	IDAge    string   `json:"identity_age,omitempty"`
	StaleFor string   `json:"stale_for,omitempty"`
}

func antSortedModels(models []string) []string {
	_, _, _, usage := usageSnapshot()
	sort.Slice(models, func(i, j int) bool {
		a := usage["antseed/"+models[i]].Requests
		b := usage["antseed/"+models[j]].Requests
		if a != b {
			return a > b
		}
		return models[i] < models[j]
	})
	return models
}

func antseedSnapshot() *antseedSummary {
	go antRefresh()
	antMu2.Lock()
	peers := map[string]bool{}
	for _, o := range antOffers {
		peers[o.peerID] = true
	}
	sum := &antseedSummary{Enabled: true, Offers: len(antOffers), Peers: len(peers)}
	sum.Models = append(sum.Models, antModels...)
	if !antFetched.IsZero() {
		sum.StaleFor = time.Since(antFetched).Round(time.Second).String()
	}
	antMu2.Unlock()
	antBornMu.Lock()
	if !antIDBorn.IsZero() {
		sum.IDAge = time.Since(antIDBorn).Round(time.Second).String()
	}
	antBornMu.Unlock()
	sum.Models = antSortedModels(sum.Models)
	return sum
}

// appendAntseedModels lists antseed/<id> for every free text model.
func appendAntseedModels(out *struct {
	Data []openRouterModel `json:"data"`
}, seenIDs map[string]bool) {
	if !antseedEnabled() {
		return
	}
	antRefresh()
	antMu2.Lock()
	models := append([]string{}, antModels...)
	antMu2.Unlock()
	for _, m := range antSortedModels(models) {
		id := "antseed/" + m
		if seenIDs[id] {
			continue
		}
		seenIDs[id] = true
		out.Data = append(out.Data, infoModel(id, id))
	}
}
