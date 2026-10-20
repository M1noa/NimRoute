// antseed p2p backend: discovery via the stats snapshot plus a slow dht
// crawl (subnet + wildcard topics), free text offers only, offer failover
// on 429 with identity rotation. mirrors the freepi handler shape
// (fetch whole, convert here).

package main

import (
	"context"
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

// antPeer mirrors PeerMetadata (fields we route on only). snapshot
// metadata carries no signature (see H1): addresses are gated by
// antEndpointOK and the transport handshake verifies the peer id.
type antPeer struct {
	PeerID    string        `json:"peerId"`
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
	antDHTTried time.Time // last crawl attempt (success or fail)
	antDHTBusy  bool      // crawl in flight: one at a time
	antDHTPeers []antPeer // dht-found peers merged into the offer loop
	antDHTBan   time.Time // bootstrap silent until: back off the crawl
	antDHTMu    sync.Mutex
	// antCool keys peer-wide ("<peerID>") and per-model
	// ("<peerID>\x00<canon-service>") cooldowns. a 429 on one model
	// must not hide the seller's other models.
	antCool     = map[string]time.Time{}
	antConns    = map[string]*antConn{}
	antProxyIdx uint64
	antIDBorn   time.Time
	antBornMu   sync.Mutex
)

// antCanonFamilies are the vendor paths whose compaction is trusted:
// dropping the path is only safe when the base name is unambiguous.
// other families keep the full "vendor/model" id (no silent remap).
var antCanonFamilies = map[string]bool{
	"opencode": true, "together": true, "groq": true, "fireworks": true,
	"deepinfra": true, "anyscale": true, "lepton": true,
}

// antCanon is the match key: vendor path dropped for known families,
// alphanumerics only.
func antCanon(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		if antCanonFamilies[strings.ToLower(s[:i])] {
			s = s[i+1:]
		}
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
// unbracketed IPv6 literals get brackets + port via JoinHostPort.
// "" when the address is unusable.
func antHostPort(addr string) string {
	if addr == "" {
		return ""
	}
	if h, p, err := net.SplitHostPort(addr); err == nil {
		if p == "" {
			p = "6882"
		}
		if pi, perr := strconv.Atoi(p); perr != nil || pi <= 0 || pi > 65535 {
			return ""
		}
		return net.JoinHostPort(h, p)
	}
	// no port: bare host or unbracketed ipv6.
	if ip := net.ParseIP(addr); ip != nil {
		return net.JoinHostPort(addr, "6882")
	}
	if strings.Contains(addr, ":") {
		return ""
	}
	return net.JoinHostPort(addr, "6882")
}

// antEndpointOK rejects non-global-unicast / private / loopback endpoints:
// a hostile snapshot must not turn the proxy into an SSRF primitive.
func antEndpointOK(hostPort string) bool {
	h, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		return false
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsGlobalUnicast() && !ip.IsPrivate()
	}
	// dns name: DHT path only ever yields ips, but snapshot names are
	// allowed (sellers on dynamic dns). resolution happens at dial.
	return h != "" && !strings.ContainsAny(h, " \t\r\n/") && !strings.HasPrefix(h, "-")
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
	// hoisted: allocating 64k per hop (up to 32) trashes the heap.
	var reply [65535]byte
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

// antDHTTopics are the upstream crawl topics: one wildcard plus the 16
// subnet shards sellers announce on (subnetOf(peerID) = first byte % 16).
// per-peer topics serve address fallback only, never enumeration.
func antDHTTopics() []string {
	t := make([]string, 0, 17)
	t = append(t, "antseed:*")
	for i := 0; i < 16; i++ {
		t = append(t, "antseed:subnet:"+strconv.Itoa(i))
	}
	return t
}

// antDHTCrawl runs one get_peers fan-out over the crawl topics, oldest
// endpoints first, then fetches /metadata from each new endpoint.
// endpoints merge into antDHTPeers; snapshot-known peers are filtered by
// the caller. errors keep the last good peers: a silent bootstrap must
// not wipe discovery. runs on its own slow cadence, never per request.
func antDHTCrawl(topics []string, budget time.Duration, meta func(string) *antPeer) {
	if len(topics) == 0 {
		return
	}
	bootstrap := cfg().Antseed.DHTBootstrap
	if len(bootstrap) == 0 {
		bootstrap = []string{"dht1.antseed.com:6881", "dht2.antseed.com:6881"}
	}
	until := time.Now().Add(budget)
	found := map[string]bool{}
	var endpoints []string
	for _, topic := range topics {
		if time.Now().After(until) {
			break
		}
		for _, ep := range antDHTTopicScan(topic, bootstrap, until) {
			if !found[ep] {
				found[ep] = true
				endpoints = append(endpoints, ep)
			}
		}
	}
	// metadata fetch is the expensive half: cap it per crawl so one big
	// network wave can't pin a poll loop for minutes.
	cap := len(endpoints)
	if cap > 64 {
		cap = 64
	}
	var peers []antPeer
	sem := make(chan struct{}, 8)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, ep := range endpoints[:cap] {
		if time.Now().After(until) {
			break
		}
		wg.Add(1)
		go func(ep string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if p := meta(ep); p != nil && p.PeerID != "" {
				mu.Lock()
				peers = append(peers, *p)
				mu.Unlock()
			}
		}(ep)
	}
	wg.Wait()
	antDHTMu.Lock()
	if len(peers) > 0 {
		antDHTPeers = peers
	}
	antDHTTried = time.Now()
	antDHTBusy = false
	antDHTMu.Unlock()
}

// antDHTTopicScan does one iterative get_peers walk for a single topic:
// closest-first over the bootstrap, then over nodes the replies point at.
// returns global-unicast endpoints only (see antDHTEndpoint). single-query
// helper antDHTLookupAt stays the address-fallback path; this is the
// enumeration path.
func antDHTTopicScan(topic string, bootstrap []string, until time.Time) []string {
	hash := sha1.Sum([]byte(topic))
	var id [20]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil
	}
	query := append([]byte("d1:ad2:id20:"), id[:]...)
	query = append(query, []byte("9:info_hash20:")...)
	query = append(query, hash[:]...)
	query = append(query, []byte("e1:q9:get_peers1:t2:aa1:y1:qe")...)
	type node struct {
		addr string
		id   []byte
	}
	dist := func(a, b []byte) []byte {
		d := make([]byte, 20)
		for i := range d {
			var x, y byte
			if i < len(a) {
				x = a[i]
			}
			if i < len(b) {
				y = b[i]
			}
			d[i] = x ^ y
		}
		return d
	}
	var queue []node
	seen := map[string]bool{}
	for _, b := range bootstrap {
		queue = append(queue, node{addr: b})
		seen[b] = true
	}
	var found []string
	foundSet := map[string]bool{}
	var reply [65535]byte
	queries := 0
	for len(queue) > 0 && queries < 200 && time.Now().Before(until) {
		// closest-first: nodes carry their id from prior replies.
		best := 0
		for i := 1; i < len(queue); i++ {
			if string(dist(queue[i].id, hash[:])) < string(dist(queue[best].id, hash[:])) {
				best = i
			}
		}
		cur := queue[best]
		queue = append(queue[:best], queue[best+1:]...)
		queries++
		conn, err := net.DialTimeout("udp", cur.addr, 500*time.Millisecond)
		if err != nil {
			continue
		}
		conn.SetDeadline(time.Now().Add(800 * time.Millisecond))
		_, err = conn.Write(query)
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
					if ep := antDHTEndpoint(entry); ep != "" && !foundSet[ep] {
						found = append(found, ep)
						foundSet[ep] = true
					}
				}
			}
		}
		if nodes, ok := response["nodes"].([]byte); ok {
			// v4 compact nodes are 26 bytes (20 id + 4 ip + 2 port);
			// skip trailing fragments instead of failing the batch.
			for i := 0; i+26 <= len(nodes); i += 26 {
				if ep := antDHTEndpoint(nodes[i+20 : i+26]); ep != "" && !seen[ep] {
					seen[ep] = true
					queue = append(queue, node{addr: ep, id: nodes[i : i+20]})
				}
			}
		}
	}
	return found
}

// antFetchMeta pulls one seller /metadata, the same shape the snapshot
// carries. nil on any failure: sellers churn, dead endpoints are normal.
func antFetchMeta(hostPort string) *antPeer {
	cl := &http.Client{Timeout: 8 * time.Second}
	resp, err := cl.Get("http://" + hostPort + "/metadata")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil
	}
	var p antPeer
	if err := json.Unmarshal(body, &p); err != nil || p.PeerID == "" {
		return nil
	}
	p.Addr = hostPort
	return &p
}

// antMaybeDHT runs the crawl when due: dht_secs cadence (0 = off),
// skipped while the bootstrap is backed off (silent last time).
// snapshot-known peer addrs are passed in so endpoints already covered
// skip the metadata fetch.
func antMaybeDHT(known map[string]bool) {
	secs := cfg().Antseed.DHTSecs
	if secs <= 0 {
		return
	}
	now := time.Now()
	antDHTMu.Lock()
	if !antDHTBan.IsZero() && now.Before(antDHTBan) {
		antDHTMu.Unlock()
		return
	}
	// tried stamps every attempt: a crawl that finds nothing still waits
	// out the cadence instead of refiring on every poll tick. seen stamps
	// only successes: last-good peers must not count as fresh forever.
	if !antDHTTried.IsZero() && now.Sub(antDHTTried) < time.Duration(secs)*time.Second {
		antDHTMu.Unlock()
		return
	}
	if antDHTBusy {
		antDHTMu.Unlock()
		return
	}
	antDHTBusy = true
	antDHTMu.Unlock()
	before := len(antDHTPeersSnapshot())
	antDHTCrawl(antDHTTopics(), 90*time.Second, func(ep string) *antPeer {
		if known[ep] {
			return nil
		}
		return antFetchMeta(ep)
	})
	after := len(antDHTPeersSnapshot())
	antDHTMu.Lock()
	if after == 0 && before == 0 {
		// silent bootstrap: back off hard (2h). the snapshot path keeps
		// serving; this only gates the crawl, never offers.
		antDHTBan = time.Now().Add(2 * time.Hour)
		acclog.Printf("!! antseed dht crawl found no peers, backing off 2h")
	}
	antDHTMu.Unlock()
}

// antDHTPeersSnapshot returns the last good crawl peers.
func antDHTPeersSnapshot() []antPeer {
	antDHTMu.Lock()
	defer antDHTMu.Unlock()
	return append([]antPeer{}, antDHTPeers...)
}

// antRefresh polls the stats snapshot and rebuilds offers. stale cache is
// served when the poll fails; one flight at a time. force repolls even
// inside the throttle window (empty routes on a 404 path).
// the dht crawl merges on its own slow cadence, never per request.
func antRefresh() {
	antRefreshForced(false)
}

func antRefreshForced(force bool) {
	antMu2.Lock()
	if antFetching {
		antMu2.Unlock()
		return
	}
	secs := cfg().Antseed.RefreshSecs
	if secs <= 0 {
		secs = 300
	}
	if !force && !antFetched.IsZero() && time.Since(antFetched) < time.Duration(secs)*time.Second {
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
	// idle-conn reap rides the poll tick (5m default): no extra goroutine.
	antReapIdle()
}

// antPoll fetches the snapshot and extracts free text offers.
func antPoll() ([]antOffer, []string) {
	return antPollWithLookup(antDHTLookup)
}

func antPollWithLookup(lookup func(string) []string) ([]antOffer, []string) {
	return antPollWithPeers(lookup, nil)
}

// antPollWithPeers is the offer extractor over snapshot peers plus the
// last good dht crawl. extra is injected by tests; live dht peers merge
// below. one shared loop: same free/proto/endpoint gates for both.
func antPollWithPeers(lookup func(string) []string, extra []antPeer) ([]antOffer, []string) {
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
	if resp.StatusCode != http.StatusOK {
		acclog.Printf("!! antseed stats poll: http %d", resp.StatusCode)
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		acclog.Printf("!! antseed stats poll: read failed: %v", err)
		return nil, nil
	}
	var snap struct {
		Peers []antPeer `json:"peers"`
	}
	if err := json.Unmarshal(body, &snap); err != nil {
		acclog.Printf("!! antseed stats poll: bad snapshot: %v", err)
		return nil, nil
	}
	peers := snap.Peers
	// crawl on its own cadence (opt-in, off by default), merge last-good
	// peers. snapshot addrs are known so their endpoints skip the
	// metadata fetch.
	if cfg().Antseed.DHTSecs > 0 {
		known := map[string]bool{}
		for _, p := range peers {
			if hp := antHostPort(p.Addr); hp != "" {
				known[hp] = true
			}
		}
		go antMaybeDHT(known)
	}
	peers = append(peers, antDHTPeersSnapshot()...)
	peers = append(peers, extra...)
	var offers []antOffer
	var models []string
	seenModel := map[string]bool{}
	seenOffer := map[string]bool{} // peerID|service: cheapest-shape dedupe
	for _, peer := range peers {
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
				// snapshot addresses are unauthenticated peer claims:
				// never dial loopback / private / metadata ips from them.
				if !antEndpointOK(hp) {
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
// empty on the throttled cache forces one fresh poll: sellers churn and
// a stale snapshot 404s models that are actually free right now.
func antRoutes(model string) []antOffer {
	antRefresh()
	out := antRoutesCached(model)
	if len(out) == 0 {
		antRefreshForced(true)
		out = antRoutesCached(model)
	}
	return out
}

func antRoutesCached(model string) []antOffer {
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
		if until, ok := antCool[antCoolKey(o.peerID, o.service)]; ok && now.Before(until) {
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
	// free first when preferred (+25 score bonus), then reputation,
	// then peer id.
	score := func(o antOffer) float64 {
		s := o.rep
		if free {
			s += 25
		}
		return s
	}
	for i := 1; i < len(eligible); i++ {
		for j := i; j > 0; j-- {
			a, b := eligible[j], eligible[j-1]
			sa, sb := score(a), score(b)
			swap := false
			if sa != sb {
				swap = sa > sb
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

// antConnFor dials or reuses the seller connection. cached conns die on
// seller idle timeouts, so a dead one (read loop exited) is dropped and
// redialed, never returned: reusing it fails the write and burns a
// failover + identity rotation for no reason.
func antConnFor(o antOffer) (*antConn, error) {
	antMu2.Lock()
	c := antConns[o.peerID]
	if c != nil {
		select {
		case <-c.closed:
			delete(antConns, o.peerID)
			c = nil
		default:
		}
	}
	antMu2.Unlock()
	if c != nil {
		c.lastUse.Store(time.Now().UnixNano())
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
	c.lastUse.Store(time.Now().UnixNano())
	antConns[o.peerID] = c
	antMu2.Unlock()
	antStampID()
	return c, nil
}

// antReapIdle closes seller conns idle past the window: the map is
// otherwise only ever deleted from antDrop, leaking fds until the
// process hits "too many open files" and every backend degrades.
func antReapIdle() {
	cutoff := time.Now().Add(-10 * time.Minute).UnixNano()
	antMu2.Lock()
	for id, c := range antConns {
		if c.lastUse.Load() < cutoff {
			select {
			case <-c.closed:
			default:
				c.conn.Close()
			}
			delete(antConns, id)
		}
	}
	antMu2.Unlock()
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

// antCoolKey is the per-model cooldown key: 429s are per (seller,
// service), not per seller. transport deaths use the bare peer id.
func antCoolKey(peerID, service string) string {
	return peerID + "\x00" + antCanon(service)
}

// antCoolPeer parks a failing seller for a minute.
func antCoolPeer(peerID string) {
	antMu2.Lock()
	antCool[peerID] = time.Now().Add(time.Minute)
	pruneAntCoolLocked()
	antMu2.Unlock()
}

// antCoolModel parks one seller service for a minute, leaving the
// seller's other services routable.
func antCoolModel(peerID, service string) {
	antMu2.Lock()
	antCool[antCoolKey(peerID, service)] = time.Now().Add(time.Minute)
	pruneAntCoolLocked()
	antMu2.Unlock()
}

// pruneAntCoolLocked drops expired entries (antMu2 held): the map would
// otherwise grow one entry per failed peer forever.
func pruneAntCoolLocked() {
	now := time.Now()
	for k, until := range antCool {
		if !now.Before(until) {
			delete(antCool, k)
		}
	}
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
// service "" cools the whole peer (transport death); otherwise only
// that service on that peer.
func antFailover(peerID, service, why string) {
	acclog.Printf("  antseed failover peer=%s: %s", peerID, why)
	antDrop(peerID)
	if service == "" {
		antCoolPeer(peerID)
	} else {
		antCoolModel(peerID, service)
	}
	antRotateIdentity()
	antBornMu.Lock()
	antIDBorn = time.Now()
	antBornMu.Unlock()
}

// antTry sends one request to one offer: free-usage open (best effort),
// then the chat completion. returns seller body on 200.
func antTry(ctx context.Context, o antOffer, oaiBody []byte) ([]byte, int, bool) {
	c, err := antConnFor(o)
	if err != nil {
		antFailover(o.peerID, "", err.Error())
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
	// antseed is raw p2p framing, not http.Client, so the transport hook
	// can't see it: record the hop by hand for -df dumps.
	if tr := traceFrom(ctx); tr != nil {
		body, trunc := string(b), false
		if len(b) > traceUpReqCap {
			body, trunc = string(b[:traceUpReqCap]), true
		}
		rbody, rtrunc := string(rb), false
		if len(rb) > traceUpRespCap {
			rbody, rtrunc = string(rb[:traceUpRespCap]), true
		}
		h := &traceHop{
			Backend:      "antseed",
			Target:       o.hostPort + q.path,
			Method:       q.method,
			ReqHeaders:   q.headers,
			ReqBody:      body,
			ReqTruncated: trunc,
			Status:       status,
			RespBody:     rbody,
			RespTrunc:    rtrunc,
		}
		if err != nil {
			h.Error = err.Error()
		}
		tr.addHop(h)
	}
	if err != nil {
		antFailover(o.peerID, "", err.Error())
		return nil, 0, true
	}
	switch {
	case status == 200:
		return rb, status, false
	case status == 429 || status == 529:
		antFailover(o.peerID, o.service, fmt.Sprintf("http %d", status))
		return nil, status, true
	case status == 402:
		return nil, status, false // paid: fail the request, never retry
	case status == 429 || status == 529 || status >= 500:
		// retryable: rate limits and seller-side failures. a 400 is the
		// client's fault (long context, bad shape) and must not park a
		// working seller for a minute.
		antCoolModel(o.peerID, o.service)
		return nil, status, true
	default:
		return nil, status, false
	}
}

// handleAntseedAnthropic serves /v1/messages for antseed/<model>: offer
// failover, then converts the openai answer back to Anthropic shape.
func (p *Pool) handleAntseedAnthropic(w http.ResponseWriter, r *http.Request, oaiBody []byte, clientModel, upstreamModel string, tnames toolNames, isStream bool, start time.Time, reqID string) {
	realModel := strings.TrimPrefix(upstreamModel, "antseed/")
	routes := antRoutes(realModel)
	if len(routes) == 0 {
		acclog.Printf("<- 404 POST /v1/messages model=%s (no free offers) %s", realModel, reqID)
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
		b, st, cont := antTry(r.Context(), o, oaiBody)
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
			acclog.Printf("<- 402 POST /v1/messages model=%s (seller demands payment) %s", realModel, reqID)
			writeAnthropicError(w, http.StatusPaymentRequired, "api_error", "antseed: seller demands payment for "+realModel)
			return
		}
		acclog.Printf("<- 502 POST /v1/messages model=%s (all offers failed, last=%d) %s", realModel, status, reqID)
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "antseed: all offers failed for "+realModel)
		return
	}
	// antTry fetches buffered (stream=false), so both client branches
	// nudge the same folded JSON.
	fold := func(b []byte) []byte {
		if strings.Contains(string(b), "data: ") {
			return sseToNonStream(b, realModel, tnames)
		}
		return b
	}
	rb = fold(rb)
	post := func(nb []byte) []byte {
		for _, o := range routes {
			if r.Context().Err() != nil {
				return nil
			}
			b, _, cont := antTry(r.Context(), o, nb)
			if b != nil {
				return fold(b)
			}
			if !cont {
				break
			}
		}
		return nil
	}
	if retry := tryNudgeJSON(r.Header.Get("x-session-id"), clientModel, "antseed", oaiBody, rb, post); retry != nil {
		rb = mergeChatJSON(rb, retry)
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
		acclog.Printf("<- 404 %s %s model=%s (no free offers) %s", r.Method, r.URL.Path, realModel, reqID)
		writeJSONError(w, http.StatusNotFound, "antseed: no free offers for "+realModel)
		return
	}
	var rb []byte
	var peer string
	var status int
	for _, o := range routes {
		if r.Context().Err() != nil {
			writeJSONError(w, 499, "client disconnected")
			return
		}
		b, st, cont := antTry(r.Context(), o, body)
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
			acclog.Printf("<- 402 %s %s model=%s (seller demands payment) %s", r.Method, r.URL.Path, realModel, reqID)
			writeJSONError(w, http.StatusPaymentRequired, "antseed: seller demands payment")
			return
		}
		acclog.Printf("<- 502 %s %s model=%s (all offers failed, last=%d) %s", r.Method, r.URL.Path, realModel, status, reqID)
		writeJSONError(w, http.StatusBadGateway, "antseed: all offers failed for "+realModel)
		return
	}
	u := respUsage(rb)
	// antTry forces stream=false upstream: the body is always plain json,
	// so say so even when the client asked for a stream.
	w.Header().Set("Content-Type", "application/json")
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
