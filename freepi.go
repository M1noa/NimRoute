package main

import (
	"sync/atomic"

	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// freepi backend: ad-supported free models via https://api.freepi.ai.
// mimics the official free-pi-cli: bearer jwt, one stable x-session-id
// per account, x-client-version, silent ad cycle (next + impression),
// meter polls. per-account lanes, never shared with zen lanes.

// freePiBase is a var so tests can point the whole backend at a local
// httptest server. atomic because a test restores the real base while a
// leftover ad-cycle goroutine reads it (freepiAdCycle outlives freepiTry).
var freePiBase atomic.Value

const freePiBaseDefault = "https://api.freepi.ai"

func init() { freePiBase.Store(freePiBaseDefault) }

// freepiBase reads the base url; swaps are actor-safe under -race.
func freepiBase() string {
	b, _ := freePiBase.Load().(string)
	return b
}

// leaseGrace is how long a locally-claimed lease is treated as live before
// a 409 is attributed to a crashed run instead of an in-flight request.
const leaseGrace = 3 * time.Second

// freepiCatalogTTL is how long /client-version ids are trusted. the picker
// catalog churns, so a boot-time list goes stale: /v1/models would advertise
// dead ids and miss new ones.
const freepiCatalogTTL = 5 * time.Minute

func freepiEnabled() bool {
	c := cfg().Freepi
	return c.Enabled && len(c.Accounts) > 0
}

// freepiVerDefault is the newest released cli version, used until
// /client-version reports something higher. the server 426s
// upgrade_required below its minimum, so a stale number breaks the
// backend for everyone until someone edits this line.
const freepiVerDefault = "0.4.10"

// freepiVerUpstream is the newest version /client-version has reported
// (latest, or min as the fallback). empty until the first catalog fetch.
var freepiVerUpstream atomic.Value

// freepiVerNewer reports whether dotted version a beats b. missing or
// non-numeric segments count as 0, so "0.4.10" > "0.4.7" and "" loses.
func freepiVerNewer(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		if x, y := freepiSeg(as, i), freepiSeg(bs, i); x != y {
			return x > y
		}
	}
	return false
}

func freepiSeg(v []string, i int) int {
	if i >= len(v) {
		return 0
	}
	n := 0
	for _, r := range v[i] {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// freepiVersion picks the highest of: built-in default, what upstream
// reported, and an explicit config pin. the server only enforces a floor,
// so taking the max is always safe; pinning below it is what 426s.
func freepiVersion() string {
	v := freepiVerDefault
	if u, _ := freepiVerUpstream.Load().(string); freepiVerNewer(u, v) {
		v = u
	}
	if c := cfg().Freepi.ClientVersion; c != "" && freepiVerNewer(c, v) {
		v = c
	}
	return v
}

// freepiVersionSync re-reads /client-version outside the catalog TTL so a
// 426 can pick up the server's new floor before the retry.
func freepiVersionSync(fa *freepiAcct) {
	fa.mu.Lock()
	fa.catalogAt = time.Time{}
	fa.mu.Unlock()
	freepiCatalog(fa)
}

func freepiInlineEvery() int {
	if n := cfg().Freepi.AdInlineEvery; n > 0 {
		return n
	}
	return 5
}

// freepiQueueWait is how long a request waits for a busy account to free up
// (one live stream per jwt). 0 = 60s.
func freepiQueueWait() time.Duration {
	if s := cfg().Freepi.QueueWaitSecs; s > 0 {
		return time.Duration(s) * time.Second
	}
	return 60 * time.Second
}

// freepiAcct wraps one configured account with runtime state.
type freepiAcct struct {
	name      string
	jwt       string
	sessionID string // stable x-session-id, minted once (server 409s a 2nd stream)
	mu        sync.Mutex
	inflight  int
	turns     int
	lastAd    time.Time
	catalog   []freepiCatalogEntry
	catalogAt time.Time
	stats     freepiStats
	statsAt   time.Time
	warming   bool
	warmAt    time.Time
}

// warm refreshes the catalog and stats in the background so /status never
// waits on api.freepi.ai. one flight per account, retried at most every 15s.
func (fa *freepiAcct) warm() {
	fa.mu.Lock()
	stale := len(fa.catalog) == 0 || time.Since(fa.statsAt) >= time.Minute ||
		time.Since(fa.catalogAt) >= freepiCatalogTTL
	if !stale || fa.warming || time.Since(fa.warmAt) < 15*time.Second {
		fa.mu.Unlock()
		return
	}
	fa.warming, fa.warmAt = true, time.Now()
	fa.mu.Unlock()
	go func() {
		freepiCatalog(fa)
		fa.refreshStats()
		fa.mu.Lock()
		fa.warming = false
		fa.mu.Unlock()
	}()
}

// freepiStats mirrors the /me/stats fields we display. cached 60s:
// stable enough for status, cheap enough to stay fresh.
type freepiStats struct {
	Tier        string
	Credits     float64
	SpentPct    float64
	ReqToday    int
	PromptToday int
	CompToday   int
}

// refreshStats pulls /me/stats, cached for a minute.
func (fa *freepiAcct) refreshStats() freepiStats {
	fa.mu.Lock()
	if time.Since(fa.statsAt) < time.Minute && fa.stats.Tier != "" {
		st := fa.stats
		fa.mu.Unlock()
		return st
	}
	fa.mu.Unlock()
	code, body := freepiGet(fa, "/me/stats", 5*time.Second)
	st := freepiStats{}
	if code == 200 {
		var ms struct {
			Tier        string  `json:"tier"`
			Credits     float64 `json:"credit_credits"`
			CapToday    float64 `json:"cap_usd_today"`
			SpentToday  float64 `json:"spent_usd_today"`
			ReqToday    int     `json:"request_count_today"`
			PromptToday int     `json:"prompt_tokens_today"`
			CompToday   int     `json:"completion_tokens_today"`
		}
		if json.Unmarshal(body, &ms) == nil {
			st.Tier = ms.Tier
			st.Credits = ms.Credits
			st.ReqToday = ms.ReqToday
			st.PromptToday = ms.PromptToday
			st.CompToday = ms.CompToday
			if ms.CapToday > 0 {
				st.SpentPct = ms.SpentToday / ms.CapToday * 100
				if st.SpentPct > 100 {
					st.SpentPct = 100
				}
			}
		}
	}
	fa.mu.Lock()
	fa.stats, fa.statsAt = st, time.Now()
	fa.mu.Unlock()
	return st
}

var (
	freepiMu    sync.Mutex
	freepiAccts = map[string]*freepiAcct{}
)

// freepiAccounts returns live account states, creating lane state on
// first use. jwt comes from config (never logged, never in /status).
func freepiAccounts() []*freepiAcct {
	c := cfg().Freepi
	freepiMu.Lock()
	defer freepiMu.Unlock()
	var out []*freepiAcct
	seen := map[string]bool{}
	for _, a := range c.Accounts {
		if a.Name == "" || a.JWT == "" {
			continue
		}
		// duplicate names collapse (second jwt overwrites the first and
		// one lane appears twice): keep the first, warn on the rest.
		if seen[a.Name] {
			log.Printf("WARN: duplicate freepi account name %q, ignoring later entry", a.Name)
			continue
		}
		seen[a.Name] = true
		fa, ok := freepiAccts[a.Name]
		if !ok {
			fa = &freepiAcct{name: a.Name, sessionID: "fp-" + randHex(12)}
			freepiAccts[a.Name] = fa
		}
		fa.mu.Lock()
		fa.jwt = a.JWT
		fa.mu.Unlock()
		out = append(out, fa)
	}
	for name := range freepiAccts {
		if !seen[name] {
			delete(freepiAccts, name)
		}
	}
	return out
}

// freepiHeaders sets the exact official-client headers. no User-Agent:
// fetch default is what the cli sends.
func freepiHeaders(req *http.Request, fa *freepiAcct) {
	fa.mu.Lock()
	jwt := fa.jwt
	fa.mu.Unlock()
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-session-id", fa.sessionID)
	req.Header.Set("x-client-version", freepiVersion())
}

func freepiGet(fa *freepiAcct, path string, timeout time.Duration) (int, []byte) {
	req, err := http.NewRequest("GET", freepiBase()+path, nil)
	if err != nil {
		return 0, nil
	}
	freepiHeaders(req, fa)
	cl := &http.Client{Timeout: timeout}
	resp, err := cl.Do(req)
	if err != nil {
		return 0, nil
	}
	if resp.StatusCode == http.StatusUnauthorized {
		acclog.Printf("!! freepi GET %s: 401, jwt rejected (re-auth the account)", path)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b
}

// freepiAdCycle runs the silent ad support: banner at lane start + 10min,
// inline every Nth turn, impression once per fresh click_token, meter per
// turn. failures swallowed. nothing is displayed anywhere.
func freepiAdCycle(fa *freepiAcct, forceBanner bool) {
	// turns/lastAd are touched from a spawned goroutine per request: hold
	// the account lock for the state, not the network calls.
	fa.mu.Lock()
	now := time.Now()
	doBanner := forceBanner || now.Sub(fa.lastAd) > 10*time.Minute
	if doBanner {
		fa.lastAd = now
	}
	fa.turns++
	doInline := fa.turns%freepiInlineEvery() == 0
	fa.mu.Unlock()
	if doBanner {
		if code, body := freepiGet(fa, "/ads/next?slot=banner", 3*time.Second); code == 200 {
			freepiImpress(fa, body)
		}
	}
	if doInline {
		if code, body := freepiGet(fa, "/ads/next?slot=inline", 3*time.Second); code == 200 {
			freepiImpress(fa, body)
		}
	}
	// meter poll every turn, ignored (server notice handled client-side).
	freepiGet(fa, "/me", 3*time.Second)
}

func freepiImpress(fa *freepiAcct, adBody []byte) {
	var ad struct {
		AdID       string `json:"ad_id"`
		ClickToken string `json:"click_token"`
	}
	if json.Unmarshal(adBody, &ad) != nil || ad.AdID == "" || ad.ClickToken == "" {
		return
	}
	payload, _ := json.Marshal(map[string]string{"ad_id": ad.AdID, "click_token": ad.ClickToken})
	req, err := http.NewRequest("POST", freepiBase()+"/ads/impression", bytes.NewReader(payload))
	if err != nil {
		return
	}
	freepiHeaders(req, fa)
	cl := &http.Client{Timeout: 3 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

// freepiErr classifies the server error envelope.
type freepiErr struct {
	Status int
	Code   string
	Msg    string
	Retry  string // retry-after header when present
}

func parseFreepiErr(status int, body []byte, hdr http.Header) freepiErr {
	fe := freepiErr{Status: status, Retry: hdr.Get("Retry-After")}
	var env struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &env) == nil {
		fe.Code, fe.Msg = env.Code, env.Message
	}
	if fe.Msg == "" {
		fe.Msg = strings.TrimSpace(string(body))
		if len(fe.Msg) > 200 {
			fe.Msg = fe.Msg[:200]
		}
	}
	return fe
}

// freepiFault is what a failed attempt or an exhausted fetch hands back: the
// upstream status, its parsed code, and the client detail. the mapping keys on
// Code, so rewording an upstream message cannot change what the client sees.
type freepiFault struct {
	Status    int    // upstream status, 0 for transport/local failures
	Code      string // upstream code ("hourly_ceiling", ...), "" when absent
	Msg       string // client detail; msgOf keeps the code visible here
	Retryable bool   // another account or a resend can fix it
}

// freepiCatalogEntry is one model row from GET /client-version: the id is
// the exact model string sent in completions, name carries the picker label
// plus the usage multiplier ("GLM 5.3 Flash · ~0.7x usage").
type freepiCatalogEntry struct {
	ID   string
	Name string
	Mult string // parsed from name, "" when the name carries none
}

// freepiModels returns catalog ids via /client-version. no builtin fallback:
// an unreachable catalog means no models, not a stale guess.
func freepiModels(fa *freepiAcct) []string {
	var out []string
	for _, e := range freepiCatalog(fa) {
		out = append(out, e.ID)
	}
	return out
}

// freepiCatalog returns the full catalog rows, caching ids + mults on the
// account for freepiCatalogTTL. the comment above warm() explains why the
// list must expire: the picker catalog churns.
func freepiCatalog(fa *freepiAcct) []freepiCatalogEntry {
	fa.mu.Lock()
	cached, at := fa.catalog, fa.catalogAt
	fa.mu.Unlock()
	if len(cached) > 0 && time.Since(at) < freepiCatalogTTL {
		return cached
	}
	_, body := freepiGet(fa, "/client-version", 5*time.Second)
	var cv struct {
		Models []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"models"`
		Model  string `json:"model"`
		Min    string `json:"min"`
		Latest string `json:"latest"`
	}
	var out []freepiCatalogEntry
	ok := false
	if json.Unmarshal(body, &cv) == nil {
		// /client-version also carries the client version floor the
		// completions endpoint enforces. follow it here so the header
		// tracks upstream instead of a hardcoded release.
		if v := cv.Latest; v != "" {
			freepiVerUpstream.Store(v)
		} else if cv.Min != "" {
			freepiVerUpstream.Store(cv.Min)
		}
		for _, m := range cv.Models {
			if m.ID == "" {
				continue
			}
			out = append(out, freepiCatalogEntry{ID: m.ID, Name: m.Name, Mult: freepiMultFromName(m.Name)})
		}
		if len(out) == 0 && cv.Model != "" {
			out = []freepiCatalogEntry{{ID: cv.Model, Name: cv.Model}}
		}
		ok = len(out) > 0
	}
	// stamp the cache only on a successful parse: a transient error must
	// not hide every model for the full TTL.
	if ok {
		fa.mu.Lock()
		fa.catalog, fa.catalogAt = out, time.Now()
		fa.mu.Unlock()
	}
	return out
}

// freepiSessionReset releases a stale session lease (client's
// /close-other-session). only on 409 concurrent_session.
func freepiSessionReset(fa *freepiAcct) {
	req, err := http.NewRequest("POST", freepiBase()+"/session/reset", nil)
	if err != nil {
		return
	}
	freepiHeaders(req, fa)
	cl := &http.Client{Timeout: 5 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
}

// freepiSnapshot builds the /status freepi section: per-account models,
// request counts (from usage totals), inflight state. jwt never leaves.
func freepiSnapshot() *freepiSummary {
	sum := &freepiSummary{Enabled: true}
	for _, fa := range freepiAccounts() {
		st := freepiAcctStatus{Name: fa.name, Mults: map[string]string{}}
		fa.warm()
		fa.mu.Lock()
		cat, fst := fa.catalog, fa.stats
		st.Inflight = fa.inflight > 0
		fa.mu.Unlock()
		for _, e := range cat {
			st.Models = append(st.Models, e.ID)
			if m := e.Mult; m != "" {
				st.Mults[e.ID] = m
			} else if m := lookupModelInfo("freeepi/" + e.ID); m != nil && m.Mult != "" {
				st.Mults[e.ID] = m.Mult
			}
		}
		st.Models = foldStatusIDs("freeepi/", st.Models)
		st.Tier, st.Credits, st.SpentPct, st.ReqToday =
			fst.Tier, fst.Credits, fst.SpentPct, fst.ReqToday
		ku := keyUsage("freepi:" + fa.name)
		st.Requests = ku.Requests
		st.Prompt = ku.Prompt
		st.Completion = ku.Completion
		sum.Accounts = append(sum.Accounts, st)
	}
	return sum
}

// applyFreepiReasoning injects the configured default reasoning effort
// when the client sent none. only models the free-pi gateway declares
// reasoning-capable (freepi_declared_reasoning, not the underlying
// model's flag) with an explicit level list are touched; a value the
// model doesn't list clamps to its highest level.
func applyFreepiReasoning(m map[string]any, realModel string) {
	want := cfg().Freepi.ReasoningEffort
	if want == "" {
		return
	}
	if _, ok := m["reasoning_effort"]; ok {
		return
	}
	if _, ok := m["reasoning"]; ok {
		return
	}
	if !modelFreepiReasoning("freeepi/" + realModel) {
		return
	}
	levels := modelEffortLevels("freeepi/" + realModel)
	if len(levels) == 0 {
		return
	}
	for _, l := range levels {
		if l == want {
			m["reasoning_effort"] = want
			return
		}
	}
	m["reasoning_effort"] = levels[len(levels)-1]
}

// handleFreepiAnthropic serves /v1/messages for freeepi/<model>: same
// account rotation, then converts the openai answer back to the
// Anthropic Messages shape (stream or not).
func (p *Pool) handleFreepiAnthropic(w http.ResponseWriter, r *http.Request, oaiBody []byte, clientModel, upstreamModel string, tnames toolNames, isStream bool, start time.Time, reqID string) {
	realModel := strings.TrimPrefix(upstreamModel, "freeepi/")
	var m map[string]any
	if json.Unmarshal(oaiBody, &m) == nil {
		m["model"] = realModel
		stripCacheFields(m)
		applyFreepiReasoning(m, realModel)
		m["stream"] = false // fetch the whole answer, convert here
		if b, err := json.Marshal(m); err == nil {
			oaiBody = b
		}
	}
	acclog.Printf("%s routed to freepi model=%s", reqID, realModel)

	_, rb, acct, fault, ok := freepiFetch(r.Context(), r.Method, oaiBody, realModel)
	if !ok {
		if fault.Status == 0 {
			fault.Status = http.StatusBadGateway
		}
		// the account's real fault and status reach the client: a 401 jwt
		// or a 404 unknown model must not wear a generic 502. a rate limit
		// does not: it is our account's cap, so it becomes a 503.
		status, errMsg := freepiClientStatus(fault)
		acclog.Printf("!! freepi failed model=%s err=%q", realModel, errMsg)
		writeAnthropicError(w, status, "api_error", "freepi: "+errMsg)
		return
	}
	// fetch is always buffered (stream=false upstream), so both client
	// branches nudge the same folded JSON.
	fold := func(b []byte) []byte {
		if strings.Contains(string(b), "data: ") {
			return sseToNonStream(b, realModel, tnames)
		}
		return b
	}
	rb = fold(rb)
	post := func(nb []byte) []byte {
		var m map[string]any
		if json.Unmarshal(nb, &m) == nil {
			m["model"] = realModel
			stripCacheFields(m)
			applyFreepiReasoning(m, realModel)
			m["stream"] = false
			if b, err := json.Marshal(m); err == nil {
				nb = b
			}
		}
		_, rb2, _, _, ok := freepiFetch(r.Context(), r.Method, nb, realModel)
		if !ok {
			return nil
		}
		return fold(rb2)
	}
	if retry := tryNudgeJSON(r.Header.Get("x-session-id"), clientModel, "freepi", oaiBody, rb, post); retry != nil {
		rb = mergeChatJSON(rb, retry)
	}
	u := respUsage(rb)
	prompT, compT, totalT := u.prompt, u.completion, u.total
	logReq := func(stream bool, contentBytes int64) {
		logUsage(UsageRecord{
			Ts:    time.Now().UTC().Format(time.RFC3339Nano),
			Model: upstreamModel, KeyName: "freepi:" + acct, Method: r.Method,
			Path: r.URL.Path, StatusCode: 200,
			DurationMs:   time.Since(start).Milliseconds(),
			PromptTokens: prompT, CompletionTokens: compT, TotalTokens: totalT, CostUSD: u.cost,
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
		acclog.Printf("<- 200 POST /v1/messages %v %d bytes [freepi:%s] model=%s %s",
			time.Since(start).Round(time.Millisecond), written, acct, realModel, reqID)
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
	acclog.Printf("<- 200 POST /v1/messages %v %d bytes [freepi:%s] model=%s %s",
		time.Since(start).Round(time.Millisecond), len(out), acct, realModel, reqID)
}

// freepiRetryable reports whether an upstream failure is the kind another
// account or a resend can fix (account rate limit, stale lease, ads gate,
// transport reset). everything else — 400 bad request, 401 bad jwt, 404
// unknown model, 5xx model-side execution — is the endpoint's own fault:
// it is forwarded to the client with its status and detail instead of the
// fetch loop retrying until the queue deadline.
func freepiRetryable(fe freepiErr) bool {
	// the code decides before the status: the hourly cap can ride a 429 and
	// no amount of queueing clears it.
	switch fe.Code {
	case "hourly_ceiling":
		// the account is out of completions for this hour: waiting out the
		// queue deadline cannot fix it, so fail fast and let the sibling
		// accounts get their one shot.
		return false
	case "rate_limited", "concurrent_session", "ads_required":
		return true
	case "upgrade_required":
		// our client version is under the server floor: that is global, so
		// rotating accounts cannot clear it. freepiTry re-reads the version
		// endpoint and resends once; past that the answer goes to the caller.
		return false
	}
	switch {
	case fe.Status == http.StatusTooManyRequests, fe.Status == http.StatusConflict:
		return true
	}
	return false
}

// freepiClientStatus maps an exhausted fetch failure onto what the client
// sees. a rate limit is never forwarded: freeepi limits our accounts, not the
// caller's request, and a client that backs off on 429 stalls on our plumbing
// instead of on the real cause. the cap becomes a 503 that names itself, the
// same way an exhausted zen pool reports proxy capacity.
func freepiClientStatus(fault freepiFault) (int, string) {
	switch {
	case fault.Code == "hourly_ceiling":
		return http.StatusServiceUnavailable, fault.Msg + " (proxy: every freeepi account is out of hourly completions; retry after the hour rolls over)"
	case fault.Status == http.StatusTooManyRequests, fault.Status == http.StatusConflict:
		return http.StatusServiceUnavailable, fault.Msg + " (proxy: every freeepi account is busy or limited)"
	}
	return fault.Status, fault.Msg
}

// freepiFetch performs one inference against a free account. the server
// allows one live stream per jwt, so callers claim a slot (queueing when
// every account is mid-request) and rotate accounts on retryable
// failures. a 409 on a freshly claimed slot means a stale server-side
// lease (a crashed run): this process serializes locally, so no live
// local request owns it and the documented /session/reset is safe.
// errors that retrying cannot fix are forwarded on the first account that
// hits them (or after every account is dead), the fault intact (status, code,
// detail): the client sees the real failure instead of a generic 502 after a
// deadline-long retry storm.
func freepiFetch(ctx context.Context, method string, body []byte, model string) (*http.Response, []byte, string, freepiFault, bool) {
	accts := freepiAccounts()
	if len(accts) == 0 {
		return nil, nil, "", freepiFault{Status: http.StatusServiceUnavailable, Msg: "no freepi accounts configured"}, false
	}
	target := freepiBase() + "/v1/chat/completions"
	deadline := time.Now().Add(freepiQueueWait())
	var last freepiFault
	tried := map[string]time.Time{} // account -> when we last used it
	fails := map[string]int{}       // account -> strikes; hard errors kill the account for this fetch
	attempts, maxAttempts := 0, 3*len(accts)+1

	for {
		// client gone: stop queueing, never burn an account on orphan output.
		if ctx.Err() != nil {
			return nil, nil, "", freepiFault{Msg: "client disconnected"}, false
		}
		if attempts >= maxAttempts {
			break
		}
		// claim a slot: free-and-unstrikes first, else the coldest tried one.
		var fa *freepiAcct
		for _, c := range accts {
			if fails[c.name] >= 2 {
				continue // two strikes: account dead for this fetch
			}
			c.mu.Lock()
			free := c.inflight == 0
			c.mu.Unlock()
			if !free {
				continue
			}
			last, done := tried[c.name]
			if !done {
				fa = c
				break
			}
			if fa == nil {
				fa = c
				continue
			}
			if faLast, fd := tried[fa.name]; fd && last.Before(faLast) {
				fa = c
			}
		}
		if fa != nil {
			fa.mu.Lock()
			if fa.inflight != 0 {
				fa.mu.Unlock()
				fa = nil
			} else {
				fa.inflight++
				fa.mu.Unlock()
			}
		}
		if fa == nil {
			if time.Now().After(deadline) {
				break
			}
			select {
			case <-time.After(150 * time.Millisecond):
			case <-ctx.Done():
				return nil, nil, "", freepiFault{Msg: "client disconnected"}, false
			}
			continue
		}

		resp, rb, acct, fault := freepiTry(ctx, method, target, body, fa, model)
		fa.mu.Lock()
		fa.inflight--
		fa.mu.Unlock()
		attempts++
		if resp != nil {
			return resp, rb, acct, freepiFault{}, true
		}
		last = fault
		tried[fa.name] = time.Now()
		if !fault.Retryable {
			// not fixable by resending or rotating: three-strike forward.
			// one strike already answered the client would keep a live
			// account unused, so hard errors get strikes[2] = dead only
			// after every account has had its one shot.
			fails[fa.name] += 2
			alive := 0
			for _, c := range accts {
				if fails[c.name] < 2 {
					alive++
				}
			}
			if alive == 0 {
				break // everyone hard-failed: forward the last real error
			}
			continue // siblings still get their one honest attempt
		}
		fails[fa.name]++
		// deadline binds every path: a persistent 429 must answer the
		// client, not spin. backoff keeps us from hammering one account
		// at round-trip speed.
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-time.After(500 * time.Millisecond):
		case <-ctx.Done():
			return nil, nil, "", freepiFault{Msg: "client disconnected"}, false
		}
	}
	if last.Msg == "" {
		last = freepiFault{Msg: "all accounts busy (queue wait exceeded)"}
	}
	return nil, nil, "", last, false
}

// freepiTry makes one upstream attempt with lease recovery. a non-nil resp is
// a 200; otherwise the returned fault carries the upstream status, the parsed
// code, and the client detail, and Retryable marks a failure a resend or
// another account can fix (account rate limit, stale lease, ads gate,
// transport reset) as opposed to a hard endpoint fault the client sees as-is.
// ctx cancels the upstream send: one live stream per jwt, so a disconnect must
// free the account immediately.
func freepiTry(ctx context.Context, method, target string, body []byte, fa *freepiAcct, model string) (*http.Response, []byte, string, freepiFault) {
	fa.mu.Lock()
	firstTurn := fa.turns == 0
	fa.mu.Unlock()
	go freepiAdCycle(fa, firstTurn)
	var lease freepiFault
	msgOf := func(fe freepiErr) string {
		if fe.Code != "" {
			return fe.Msg + " (code " + fe.Code + ")"
		}
		return fe.Msg
	}
	faultOf := func(fe freepiErr) freepiFault {
		return freepiFault{Status: fe.Status, Code: fe.Code, Msg: msgOf(fe), Retryable: freepiRetryable(fe)}
	}
	for attempt := 0; attempt < 2; attempt++ {
		if ctx.Err() != nil {
			return nil, nil, "", freepiFault{Msg: ctx.Err().Error()}
		}
		req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
		if err != nil {
			return nil, nil, "", freepiFault{Msg: err.Error()}
		}
		freepiHeaders(req, fa)
		resp, err := dbgClient(&http.Client{Timeout: 300 * time.Second}, "freepi").Do(req)
		if err != nil {
			// transport noise is account-or-network: another account or a
			// resend can beat it, so this is fixable, not forwardable.
			acclog.Printf("!! freepi net error acct=%s model=%s: %v", fa.name, model, err)
			return nil, nil, "", freepiFault{Msg: err.Error(), Retryable: true}
		}
		// a mid-body reset is a retryable failure, not a 200: the
		// client would see a stream that stops and pay twice on retry.
		rb, rerr := io.ReadAll(io.LimitReader(resp.Body, upstreamBodyLimit))
		resp.Body.Close()
		if rerr != nil {
			return nil, nil, "", freepiFault{Msg: rerr.Error(), Retryable: true}
		}
		if resp.StatusCode == http.StatusOK {
			return resp, rb, fa.name, freepiFault{}
		}
		fe := parseFreepiErr(resp.StatusCode, rb, resp.Header)
		if resp.StatusCode == http.StatusConflict && fe.Code == "concurrent_session" && attempt == 0 {
			// this process serializes per account, so a 409 on our own slot
			// is a stale lease from a crashed run. release it.
			acclog.Printf("  freepi acct=%s stale session lease, releasing", fa.name)
			freepiSessionReset(fa)
			select {
			case <-time.After(leaseGrace):
			case <-ctx.Done():
				return nil, nil, "", freepiFault{Msg: ctx.Err().Error()}
			}
			lease = faultOf(fe)
			continue
		}
		if resp.StatusCode == http.StatusUpgradeRequired && attempt == 0 {
			// the server retired the version we sent. /client-version
			// carries the new floor, so re-read it and resend rather than
			// handing the caller an upgrade nag. once per request: a still
			// stale version means our own fetch is lying, not the gate.
			sent := req.Header.Get("x-client-version")
			freepiVersionSync(fa)
			if now := freepiVersion(); now != sent {
				acclog.Printf("  freepi client version %s retired, retrying as %s", sent, now)
				continue
			}
		}
		acclog.Printf("!! freepi %d [%s] acct=%s model=%s code=%s err=%q",
			resp.StatusCode, fe.Code, fa.name, model, fe.Code, fe.Msg)
		if resp.StatusCode == http.StatusForbidden && fe.Code == "ads_required" {
			go freepiAdCycle(fa, true)
		}
		return nil, nil, "", faultOf(fe)
	}
	return nil, nil, "", lease
}

// handleFreepi routes freeepi/<model> to api.freepi.ai with account
// rotation: one concurrent stream per account (server 409s a second),
// failover across accounts on 429/409/502/503.
func (p *Pool) handleFreepi(w http.ResponseWriter, r *http.Request, body []byte, model string, isStream bool, start time.Time, reqID string) {
	realModel := strings.TrimPrefix(model, "freeepi/")
	var m map[string]any
	if json.Unmarshal(body, &m) == nil {
		m["model"] = realModel
		stripCacheFields(m)
		applyFreepiReasoning(m, realModel)
		if b, err := json.Marshal(m); err == nil {
			body = b
		}
	}
	acclog.Printf("%s routed to freepi model=%s", reqID, realModel)

	resp, rb, acct, fault, ok := freepiFetch(r.Context(), r.Method, body, realModel)
	if !ok {
		if fault.Status == 0 {
			fault.Status = http.StatusBadGateway
		}
		// a rate limit is our account's cap, not the caller's request: it is
		// mapped to a 503 that names the cause instead of being forwarded.
		status, errMsg := freepiClientStatus(fault)
		// fixed envelope: upstream detail may quote-break Sprintf json, so
		// the text goes in as a json string, not a Sprintf template.
		acclog.Printf("!! freepi failed model=%s err=%q", realModel, errMsg)
		writeJSONError(w, status, "freepi: "+errMsg)
		return
	}
	u := respUsage(rb)
	prompT, compT, totalT := u.prompt, u.completion, u.total
	for k, v := range resp.Header {
		lk := strings.ToLower(k)
		if lk == "content-type" || strings.HasPrefix(lk, "x-ratelimit") || lk == "retry-after" {
			w.Header()[k] = v
		}
	}
	if !isStream {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(http.StatusOK)
	w.Write(rb)
	logUsage(UsageRecord{
		Ts:    time.Now().UTC().Format(time.RFC3339Nano),
		Model: model, KeyName: "freepi:" + acct, Method: r.Method,
		Path: r.URL.Path, StatusCode: 200,
		DurationMs:   time.Since(start).Milliseconds(),
		PromptTokens: prompT, CompletionTokens: compT, TotalTokens: totalT, CostUSD: u.cost,
		Stream: isStream, ContentBytes: int64(len(body)),
	})
	acclog.Printf("<- 200 %s %s %v [freepi:%s] model=%s %s",
		r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond), acct, realModel, reqID)
}
