package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"nimroute/pii"
)

// zen lanes: pinned proxy+session pairs so concurrent conversations spread
// across exit ips instead of hammering one into 429s. lane identity falls
// back down a chain: x-session-id header -> conversation hash (first user
// + last assistant block) -> shared lru pool. there is no waiting: a dead
// exit rotates to a fresh proxy+session immediately, never a cooldown.

type zenLane struct {
	id        string // lane key (header value, convo hash, or shared-n)
	session   string // ses_* for this lane, re-minted on every rotation
	proxy     string // pinned exit
	country   string
	lastUsed  time.Time
	requests  int
	rateLimit int
	// consec429 counts consecutive limit events on this lane; laneTouch
	// clears it on success. two in a row burns the pin for that model:
	// one 429 is the exit being busy, two means it refuses the model.
	consec429 int
	// limitedSince stamps the first of a consecutive run of limits; a
	// success clears it. a run lasting past stale_pool_secs marks the
	// pool stale (refresh + session rotation).
	limitedSince time.Time
	// lastSend is the pacing clock: a lane waits lane_min_gap_ms between
	// sends so one exit never sees a burst from a single conversation.
	lastSend time.Time
	// escalated latches once a direct attempt proved unusable, so the
	// lane keeps using a proxy even when always_proxy is off.
	escalated bool
	// inflight counts active sends on the lane. >0 means a request is
	// still using the pinned exit: failover prefers idle lanes.
	inflight int
	// tokens served since process start, by model. lives on the lane object
	// rather than keyed by lane key, so totals survive laneRotate (new
	// session, same lane) and convertSharedLocked (key renamed, same lane).
	tokens map[string]laneTokens
}

// laneTokens is one model's contribution to a lane's lifetime totals.
// DurMs sums finished-request durations for the avg_ms in /status.
type laneTokens struct {
	Requests   int   `json:"requests"`
	Prompt     int   `json:"prompt_tokens"`
	Completion int   `json:"completion_tokens"`
	Total      int   `json:"total_tokens"`
	DurMs      int64 `json:"duration_ms"`
}

type laneStatus struct {
	ID        string `json:"id"`
	Proxy     string `json:"proxy"`
	Country   string `json:"country"`
	LastUsed  string `json:"last_used_ago"`
	Requests  int    `json:"requests"`
	RateLimit int    `json:"rate_limited"`
	// Prompt/Completion/Tokens are lifetime totals since process start.
	Prompt     int `json:"prompt_tokens"`
	Completion int `json:"completion_tokens"`
	Tokens     int `json:"total_tokens"`
	// AvgMs is mean finished-request duration across all models.
	AvgMs int `json:"avg_ms"`
	// ByModel breaks the totals down per model served on this lane.
	ByModel map[string]laneTokens `json:"by_model,omitempty"`
}

var (
	lanesMu sync.Mutex
	lanes   = map[string]*zenLane{}
	// convoSeen counts convo-hash sightings so lanes pin only from the
	// second request. tiny: hashes evicted on the sweep with the lanes.
	convoSeenMu sync.Mutex
	convoSeen   = map[string]int{}
)

// seenConvoLocked reports whether a convo hash was seen before.
// caller holds lanesMu (same critical section as lane assignment).
func seenConvoLocked(h string) bool {
	convoSeenMu.Lock()
	defer convoSeenMu.Unlock()
	return convoSeen[h] > 0
}

// noteConvoLocked records a first sighting. caller holds lanesMu.
func noteConvoLocked(h string) {
	convoSeenMu.Lock()
	defer convoSeenMu.Unlock()
	convoSeen[h]++
	if len(convoSeen) > 4*laneCap()+64 {
		for k := range convoSeen {
			delete(convoSeen, k)
			break
		}
	}
}

// dropConvoSeen forgets a sighting when its lane is swept. caller holds
// lanesMu; takes convoSeenMu (leaf lock, no inversion: convoSeenMu never
// acquires lanesMu).
func dropConvoSeen(k string) {
	convoSeenMu.Lock()
	defer convoSeenMu.Unlock()
	delete(convoSeen, k)
}

// exit burns: an exit that 403s FreeTierError for a model is skipped for
// that model until the burn lapses. per (exit, model), not per exit: a
// burned-for-pickle exit can still serve space-bunny. without this every
// lane retries the same fast (and burned) exits and a servable model 403s.
var (
	burnMu sync.Mutex
	burned = map[string]time.Time{}
)

// long burns: an exit that keeps 429ing (twice, 20s+ since its last
// success) is out for every model for 12h. per-exit, not per-model:
// a saturated exit is saturated for everything.
var (
	longBurnMu sync.Mutex
	longBurned = map[string]time.Time{}
	// exitTrack holds per-exit health: last success, 429 streak, slow strikes.
	exitTrackMu sync.Mutex
	exitTrack   = map[string]*exitHealth{}
)

type exitHealth struct {
	lastOK   time.Time
	streak   int // consecutive 429s
	strikes  []time.Time
	slowWins int
}

func exitHealthFor(proxy string) *exitHealth {
	exitTrackMu.Lock()
	defer exitTrackMu.Unlock()
	h, ok := exitTrack[proxy]
	if !ok {
		h = &exitHealth{}
		exitTrack[proxy] = h
	}
	return h
}

// noteExit429 records a 429 on an exit. two in a row with 20s+ since the
// last success — or three straight regardless — means long-term
// saturation: 12h ban for every model, the exit is dropped from the pool,
// and every lane pinned to it migrates to a spare so no lane keeps using
// a rate-limited exit. a new exit still gets the streak>=3 rule, but two
// fast 429s alone are the exit being busy, not proof it is dead: banning
// fresh exits on a pair empties the usable pool until only a restart
// clears the bans.
func noteExit429(proxy string) bool {
	if proxy == "" {
		return false
	}
	h := exitHealthFor(proxy)
	exitTrackMu.Lock()
	h.streak++
	streak, lastOK := h.streak, h.lastOK
	exitTrackMu.Unlock()
	saturated := (streak >= 2 && !lastOK.IsZero() && time.Since(lastOK) > 20*time.Second) || streak >= 3
	if !saturated {
		return false
	}
	longBurnMu.Lock()
	already := longBurned[proxy]
	longBurned[proxy] = time.Now().Add(12 * time.Hour)
	longBurnMu.Unlock()
	if already.After(time.Now().Add(11 * time.Hour)) {
		return true // ban already stood: no churn on every 429 past the bar
	}
	acclog.Printf("  zen exit %s long-burned 12h (429 streak %d, no success 20s+)", redactProxyUserinfo(proxy), streak)
	dropProxy(proxy)
	evacuateProxyPins(proxy)
	return true
}

// noteExitOK clears an exit's 429 streak on any success.
func noteExitOK(proxy string) {
	if proxy == "" {
		return
	}
	h := exitHealthFor(proxy)
	exitTrackMu.Lock()
	h.streak = 0
	h.lastOK = time.Now()
	exitTrackMu.Unlock()
}

// exitLongBurned reports whether an exit is under a 12h saturation ban.
func exitLongBurned(proxy string) bool {
	if proxy == "" {
		return false
	}
	longBurnMu.Lock()
	defer longBurnMu.Unlock()
	until, ok := longBurned[proxy]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(longBurned, proxy)
		return false
	}
	return true
}

// noteExitSlow records a >6s first-byte stall on an exit. two strikes
// within an hour drops the exit from the pool: consistently slow exits
// waste every request they touch.
func noteExitSlow(proxy string) {
	if proxy == "" {
		return
	}
	h := exitHealthFor(proxy)
	exitTrackMu.Lock()
	now := time.Now()
	h.strikes = append(h.strikes, now)
	// keep only the last hour
	var kept []time.Time
	for _, t := range h.strikes {
		if now.Sub(t) < time.Hour {
			kept = append(kept, t)
		}
	}
	h.strikes = kept
	n := len(kept)
	exitTrackMu.Unlock()
	if n >= 2 {
		acclog.Printf("  zen exit %s dropped: 2 slow strikes in 1h", redactProxyUserinfo(proxy))
		dropProxy(proxy)
	}
}

func burnTTL() time.Duration {
	if m := cfg().Zen.ExitBurnMinutes; m > 0 {
		return time.Duration(m) * time.Minute
	}
	return 30 * time.Minute
}

func burnKey(proxy, model string) string { return proxy + "\x00" + model }

// burnExit marks proxy as refusing model until the burn lapses. "" proxy
// (direct) is not burnable: there is only one direct exit and laneBlock
// already handles it.
func burnExit(proxy, model string) {
	if proxy == "" || model == "" {
		return
	}
	burnMu.Lock()
	burned[burnKey(proxy, model)] = time.Now().Add(burnTTL())
	burnMu.Unlock()
}

func exitBurned(proxy, model string) bool {
	if proxy == "" || model == "" {
		return false
	}
	burnMu.Lock()
	defer burnMu.Unlock()
	until, ok := burned[burnKey(proxy, model)]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(burned, burnKey(proxy, model))
		return false
	}
	return true
}

// sweepBurns drops lapsed burns. exitBurned deletes on read, but entries
// for dropped exits or renamed models are never read again and would
// linger: the pool churns hourly, so this runs on the same sweep.
func sweepBurns() {
	burnMu.Lock()
	defer burnMu.Unlock()
	now := time.Now()
	for k, until := range burned {
		if now.After(until) {
			delete(burned, k)
		}
	}
	longBurnMu.Lock()
	defer longBurnMu.Unlock()
	for k, until := range longBurned {
		if now.After(until) {
			delete(longBurned, k)
		}
	}
	tlsBurnMu.Lock()
	defer tlsBurnMu.Unlock()
	for k, until := range tlsBurned {
		if now.After(until) {
			delete(tlsBurned, k)
		}
	}
}

// bodyCancelCloser cancels a hedged side's request context when its
// response body is closed: the winner keeps streaming after hedgedDo
// returns, and the context is still released.
type bodyCancelCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b bodyCancelCloser) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// zenSideResult is one hedged side's outcome.
type zenSideResult struct {
	resp *http.Response
	err  error
}

// zenSide runs send under its own cancellable context and returns the
// cancel immediately, so hedgedDo can kill a losing side mid-flight. the
// response body cancels the context on close, so a winning side streams
// after zenSide returns and the context is freed when the caller is done.
func zenSide(ctx context.Context, send func(context.Context) (*http.Response, error)) (context.CancelFunc, <-chan zenSideResult) {
	pctx, pcancel := context.WithCancel(ctx)
	ch := make(chan zenSideResult, 1)
	go func() {
		resp, err := send(pctx)
		if err != nil || resp == nil || resp.Body == nil {
			pcancel() // nothing will own the context: release now
		} else {
			resp.Body = bodyCancelCloser{resp.Body, pcancel}
		}
		ch <- zenSideResult{resp, err}
	}()
	return pcancel, ch
}

// fires hedge concurrently: first non-retryable response wins, the loser is
// cancelled (context + body). chat completions are read-only from our side,
// so double-send is safe. returns which side won (true = hedge).
// both sides derive from ctx, so a client disconnect cancels the upstream
// request instead of burning an exit on output nobody reads.
func hedgedDo(ctx context.Context, primary, hedge func(context.Context) (*http.Response, error), hedgeAfter time.Duration) (*http.Response, error, bool) {
	if hedgeAfter <= 0 || hedge == nil {
		resp, err := primary(ctx)
		return resp, err, false
	}
	closeResult := func(r zenSideResult) {
		if r.resp != nil && r.resp.Body != nil {
			r.resp.Body.Close()
		}
	}
	retryable := func(r zenSideResult) bool {
		if r.err != nil || r.resp == nil {
			return true
		}
		st := r.resp.StatusCode
		return st == 429 || st == 529 || st == 502 || st == 503 || st == 504
	}
	pcancel, primaryCh := zenSide(ctx, primary)
	timer := time.NewTimer(hedgeAfter)
	defer timer.Stop()
	var failed *zenSideResult
	failedHedge := false
	select {
	case r := <-primaryCh:
		if !retryable(r) {
			return r.resp, r.err, false
		}
		failed = &r
		primaryCh = nil
	case <-timer.C:
	case <-ctx.Done():
		pcancel()
		go func() { closeResult(<-primaryCh) }()
		return nil, ctx.Err(), false
	}
	hcancel, hedgeCh := zenSide(ctx, hedge)
	cleanup := func(cancel context.CancelFunc, ch <-chan zenSideResult) {
		cancel()
		if ch != nil {
			go func() { closeResult(<-ch) }()
		}
	}
	for {
		var result zenSideResult
		var wonHedge bool
		select {
		case result = <-primaryCh:
			primaryCh = nil
		case result = <-hedgeCh:
			hedgeCh = nil
			wonHedge = true
		case <-ctx.Done():
			cleanup(pcancel, primaryCh)
			cleanup(hcancel, hedgeCh)
			if failed != nil {
				closeResult(*failed)
			}
			return nil, ctx.Err(), false
		}
		if !retryable(result) {
			if wonHedge {
				cleanup(pcancel, primaryCh)
			} else {
				cleanup(hcancel, hedgeCh)
			}
			if failed != nil {
				closeResult(*failed)
			}
			return result.resp, result.err, wonHedge
		}
		if failed == nil {
			failed = &result
			failedHedge = wonHedge
			continue
		}
		// both failed: preserve an http error rather than a transport
		// error. when both sides died on transport, prefer the dial-stage
		// one: the retry ladders stop on a dial-dead re-pick, and a hedge
		// artifact ("no distinct zen hedge exit") must not mask a
		// refused/unreachable dial or they re-dial the corpse for every
		// remaining hop.
		if failed.resp != nil {
			closeResult(result)
			return failed.resp, failed.err, failedHedge
		}
		if !zenDialDead(result.err) && zenDialDead(failed.err) {
			closeResult(result)
			return failed.resp, failed.err, failedHedge
		}
		closeResult(*failed)
		return result.resp, result.err, wonHedge
	}
}

func laneCap() int {
	if n := cfg().Zen.Lanes; n > 0 {
		return n
	}
	return 25
}

func laneTTL() time.Duration {
	if m := cfg().Zen.LaneTTLMinutes; m > 0 {
		return time.Duration(m) * time.Minute
	}
	return 30 * time.Minute
}

// sessionIdleTTL is the idle time after which a lane's session hash/id is
// evicted on the sweep. lane_ttl_minutes stays the hard cap.
func sessionIdleTTL() time.Duration {
	if m := cfg().Zen.SessionIdleMinutes; m > 0 {
		return time.Duration(m) * time.Minute
	}
	return 0
}

// convoIdleTTL is the shorter idle TTL for convo-hash lanes only: one-shot
// requests must not pin a lane for minutes. hdr: and shared lanes keep
// the longer TTLs above.
func convoIdleTTL() time.Duration {
	if m := cfg().Zen.ConvoIdleMinutes; m > 0 {
		return time.Duration(m) * time.Minute
	}
	return 0
}

func laneMinGap() time.Duration {
	return time.Duration(cfg().Zen.LaneMinGapMs) * time.Millisecond
}

func laneRelease() time.Duration {
	return time.Duration(cfg().Zen.LaneReleaseSecs) * time.Second
}

// zenMaxRetries caps zen upstream attempts per request (loop bound is
// attempts, log labels stay /N on retries).
func zenMaxRetries() int {
	if n := cfg().Zen.MaxRetries; n > 0 {
		return n + 1
	}
	if cfg().Zen.MaxRetries < 0 {
		return 1
	}
	return 7 // default: first try + 6 retries
}

func zenRetryLabel() int { return zenMaxRetries() - 1 }

// zenLastRetry reports whether retry index is the final attempt.
func zenLastRetry(retry int) bool { return retry+1 >= zenMaxRetries() }

// laneGate blocks until the lane may send: pacing gap since its previous
// send only, never a cooldown. the slot is claimed under the lane lock
// before sleeping, so concurrent requests are spaced one gap apart instead
// of all waking together and bursting the same exit. wait is capped so a
// lane never stalls a request indefinitely. ctx-aware: a client disconnect
// aborts the pacing sleep instead of blocking shutdown.
func laneGate(ctx context.Context, l *zenLane) time.Duration {
	now := time.Now()
	maxWait := 10 * time.Second
	if s := cfg().Zen.LaneGateMaxSecs; s > 0 {
		maxWait = time.Duration(s) * time.Second
	}
	lanesMu.Lock()
	var wait time.Duration
	if gap := laneMinGap(); gap > 0 && !l.lastSend.IsZero() {
		if w := gap - now.Sub(l.lastSend); w > 0 {
			wait = w
		}
	}
	if wait > maxWait {
		wait = maxWait
	}
	// claim: the next caller's gap is measured from this send's schedule,
	// so two concurrent callers serialize one gap apart.
	l.lastSend = now.Add(wait)
	lanesMu.Unlock()
	if wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
		}
	}
	return wait
}

// convoHash fingerprints a conversation from the first user text block and
// the last assistant text block: cheap (two blocks, not full history) and
// stable across turns as history grows.
func convoHash(body []byte) string {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	var firstUser, lastAsst string
	feed := func(role, text string) {
		if text == "" {
			return
		}
		if role == "user" && firstUser == "" {
			firstUser = text
		}
		if role == "assistant" {
			lastAsst = text
		}
	}
	if msgs, ok := m["messages"].([]any); ok {
		for _, raw := range msgs {
			mm, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			role, _ := mm["role"].(string)
			feed(role, contentText(mm["content"]))
		}
	}
	if in, ok := m["input"].([]any); ok {
		for _, raw := range in {
			it, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := it["type"].(string)
			if typ == "function_call" || typ == "function_call_output" {
				continue
			}
			role, _ := it["role"].(string)
			feed(role, contentText(it["content"]))
		}
	}
	if firstUser == "" && lastAsst == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(firstUser + "\x00" + lastAsst))
	return "convo:" + hex.EncodeToString(sum[:8])
}

func contentText(c any) string {
	switch t := c.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, p := range t {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			pt, _ := pm["type"].(string)
			if pt == "text" || pt == "input_text" || pt == "output_text" {
				if s, ok := pm["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	}
	return ""
}

// laneFor assigns a lane: explicit header wins, then conversation hash,
// then the least-recently-used healthy lane (shared pool fallback).
// convo-hash lanes pin only from the second sighting: single-turn
// one-shots (title-gen) fall to the shared pool and never pin a lane.
// hdr/convo/convo modes toggle independently in config; all off (or header
// with shared off) falls back to shared with a logged warning.
//
// shared lanes are pre-opened up to the cap on startup (ensureSharedLanes),
// so a cold start does not serialize on lane creation. an hdr or convo key
// that arrives with a full table converts the idlest shared lane instead
// of piling onto one fallback lane.
func laneFor(headerSession string, body []byte) *zenLane {
	lanesMu.Lock()
	defer lanesMu.Unlock()
	wantHdr, wantConvo, wantShared := laneModes()
	// absent key = default on: openai-compat clients with no session
	// header would otherwise all share one lane.
	wantCacheKey := true
	if b := cfg().Zen.LaneCacheKey; b != nil {
		wantCacheKey = *b
	}
	if !wantHdr && !wantConvo && !wantShared && !wantCacheKey {
		wantShared = true // all off: spread anyway, never fail a request
	}
	key := ""
	switch {
	case headerSession != "" && wantHdr:
		key = "hdr:" + headerSession
	case wantCacheKey:
		// openai-compat clients that send no session header pin on
		// prompt_cache_key, which is per conversation.
		if ck := cacheKeyOf(body); ck != "" {
			key = "pck:" + ck
		}
	}
	if key == "" && wantConvo {
		if h := convoHash(body); h != "" {
			if seenConvoLocked(h) {
				key = h
			} else {
				noteConvoLocked(h)
			}
		}
	}
	if key != "" {
		if l, ok := lanes[key]; ok {
			l.lastUsed = time.Now()
			return l
		}
		if len(lanes) < laneCap() {
			l := &zenLane{id: shortLaneID(key), session: zenSession(), lastUsed: time.Now()}
			lanes[key] = l
			return l
		}
		// table full: convert the idlest shared lane into this key so a
		// burst of new conversations spreads instead of piling onto one
		// fallback lane (or evicting a pinned one mid-flight).
		if convertSharedLocked(key) != nil {
			return lanes[key]
		}
	}
	if !wantShared {
		// shared off but nothing else matched (e.g. header with hdr off):
		// create a throwaway lane, capped so one-shots cannot grow the
		// table past cap.
		if len(lanes) < laneCap() {
			l := &zenLane{id: "shared", session: zenSession(), lastUsed: time.Now()}
			k := "shared:" + l.session[len(l.session)-6:]
			l.id = shortLaneID(k)
			lanes[k] = l
			return l
		}
	}
	// top the shared pool up to the cap on first use too, so a cold start
	// that skipped ensureSharedLanes still spreads across all lanes
	// instead of stacking on whichever one was created first.
	ensureSharedLanesLocked()
	return sharedLaneLocked()
}

// laneModes reads the three lane identity toggles. nil (absent key) means
// the default: hdr on, convo off (buggy), shared on.
func laneModes() (hdr, convo, shared bool) {
	c := cfg().Zen
	hdr, convo, shared = true, false, true
	if c.LaneHdr != nil {
		hdr = *c.LaneHdr
	}
	if c.LaneConvo != nil {
		convo = *c.LaneConvo
	}
	if c.LaneShared != nil {
		shared = *c.LaneShared
	}
	return hdr, convo, shared
}

// convertSharedLocked allocates a fresh lane for key and drops the idlest
// shared lane to hold the cap. the victim is never mutated in place: an
// in-flight request may hold its pointer, and renaming it mid-request
// breaks session continuity for both conversations. caller holds lanesMu.
// nil when no shared lane exists to convert.
func convertSharedLocked(key string) *zenLane {
	var victimKey string
	var victim *zenLane
	for k, l := range lanes {
		if !strings.HasPrefix(k, "shared:") {
			continue
		}
		if victim == nil || l.lastUsed.Before(victim.lastUsed) {
			victim, victimKey = l, k
		}
	}
	if victim == nil {
		return nil
	}
	delete(lanes, victimKey)
	l := &zenLane{
		session:  zenSession(),
		lastUsed: time.Now(),
		id:       shortLaneID(key),
	}
	lanes[key] = l
	return l
}

// ensureSharedLanes pre-opens shared lanes up to the cap so a cold start
// does not serialize early requests on lane creation + exit races. runs
// at startup after the first pool refresh and again on first request.
func ensureSharedLanes() {
	_, _, wantShared := laneModes()
	if !wantShared {
		return
	}
	lanesMu.Lock()
	defer lanesMu.Unlock()
	ensureSharedLanesLocked()
}

// ensureSharedLanesLocked is ensureSharedLanes with lanesMu already held.
// new lanes get an epoch lastUsed so they land at the cold end of the lru
// pick and hand out one per request before any lane is reused.
func ensureSharedLanesLocked() {
	for len(lanes) < laneCap() {
		l := &zenLane{id: "shared", session: zenSession(), lastUsed: time.Now()}
		k := "shared:" + l.session[len(l.session)-6:]
		l.id = shortLaneID(k)
		lanes[k] = l
	}
}

// cacheKeyOf reads the openai-compat prompt_cache_key (or camel
// promptCacheKey) off a body. clients set it per conversation, so it works
// as a lane identity where no x-session-id header exists. read before
// stripCacheFields, which deletes the field for upstream. note the
// exception to verbatim lane identity: pck values are whitespace-trimmed
// (json string padding is not identity).
func cacheKeyOf(body []byte) string {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	for _, k := range []string{"prompt_cache_key", "promptCacheKey"} {
		if s, ok := m[k].(string); ok {
			if s = strings.TrimSpace(s); s != "" {
				return s
			}
		}
	}
	return ""
}

// shortLaneID shortens a lane key for display.
func shortLaneID(key string) string {
	if len(key) <= 12 {
		return key
	}
	return key[:8] + "..." + key[len(key)-4:]
}

// sharedLaneLocked picks the coldest shared lane, creating shared-n slots
// up to cap. caller holds lanesMu. no health filtering: lanes never cool, a
// limit rotates the exit instead of blocking the lane. stamps lastUsed on
// hand-out so the next request moves to a different lane: without this,
// lastUsed only advanced on success and every concurrent request drew the
// same coldest lane, stacking clients on one exit.
func sharedLaneLocked() *zenLane {
	var cands []*zenLane
	for k, l := range lanes {
		if strings.HasPrefix(k, "shared:") {
			cands = append(cands, l)
		}
	}
	now := time.Now()
	sort.Slice(cands, func(i, j int) bool { return cands[i].lastUsed.Before(cands[j].lastUsed) })
	if len(cands) > 0 {
		cands[0].lastUsed = now
		return cands[0]
	}
	if len(lanes) < laneCap() {
		l := &zenLane{id: "shared", session: zenSession(), lastUsed: now}
		k := "shared:" + l.session[len(l.session)-6:]
		l.id = shortLaneID(k)
		lanes[k] = l
		return l
	}
	// table full of pinned lanes: reuse the stalest one so the request
	// goes out on a fresh rotation instead of failing.
	var best *zenLane
	for _, l := range lanes {
		if best == nil || l.lastUsed.Before(best.lastUsed) {
			best = l
		}
	}
	if best == nil {
		l := &zenLane{id: "shared", session: zenSession(), lastUsed: now}
		lanes["shared:fallback"] = l
		return l
	}
	return best
}

// laneRotate swaps the lane onto a fresh exit + session after any limit or
// identity block. the old exit keeps serving other lanes; this lane never
// waits, it just moves. stamps limitedSince so a run of consecutive limits
// past stale_pool_secs trips the stale-pool refresh. a second consecutive
// 429 on this lane burns the pin for this model: one 429 is the exit being
// busy, two means it refuses this model, and pickLaneProxyFor would keep
// re-picking the same fast exit otherwise. returns the new session id.
func laneRotate(l *zenLane, model string) string {
	lanesMu.Lock()
	l.rateLimit++
	if l.limitedSince.IsZero() {
		l.limitedSince = time.Now()
	}
	l.session = zenSession()
	l.lastUsed = time.Now()
	l.consec429++
	strikes, pin := l.consec429, l.proxy
	lanesMu.Unlock()
	if strikes >= 2 {
		burnExit(pin, model)
	}
	laneReproxy(l, model)
	noteRateLimit()
	return laneSession(l)
}

// noteRateLimit counts lanes rate-limited since the last pool refresh.
// true when a refresh fired: the counter resets on every trip either way,
// and a trip inside the cooldown window skips the refresh (debug-logged).
func noteRateLimit() bool {
	n := cfg().Zen.PoolRefresh429s
	if n <= 0 {
		return false
	}
	poolRefresh429Mu.Lock()
	poolRefresh429s++
	hit := poolRefresh429s >= n
	if hit {
		poolRefresh429s = 0
	}
	poolRefresh429Mu.Unlock()
	if !hit {
		return false
	}
	if !poolRefreshDue() {
		zenProxiesMu.RLock()
		ago := time.Since(zenPoolRefreshed)
		zenProxiesMu.RUnlock()
		dbglog.Printf("  zen: %d rate-limited lanes, refresh skipped (last %v ago, cooldown %v)", n, ago.Round(time.Second), poolRefreshCooldown())
		return false
	}
	acclog.Printf("  zen: %d rate-limited lanes, refreshing proxy pool", n)
	// stamp at fire time: the completion stamp lands ~30s+ later after
	// verify, so a second trip during an in-flight refresh would see a
	// stale timestamp and log a duplicate fire (harmless via TryLock,
	// but noisy). completion re-stamps with the true finish time.
	zenProxiesMu.Lock()
	zenPoolRefreshed = time.Now()
	zenProxiesMu.Unlock()
	refreshZenProxiesAsync()
	return true
}

// poolRefreshDue reports whether a tripped 429 counter may fire: true when
// the last refresh finished outside the cooldown window.
func poolRefreshDue() bool {
	if cd := poolRefreshCooldown(); cd > 0 {
		zenProxiesMu.RLock()
		ago := time.Since(zenPoolRefreshed)
		zenProxiesMu.RUnlock()
		if ago < cd {
			return false
		}
	}
	return true
}

// waitZenPool blocks until the pool holds an exit or maxWait elapses,
// polling every 100ms. an empty pool means the verify sweep is still
// running (cold start or refresh gap), not that exits are dead: callers
// with an exit budget to spend wait instead of burning retries in ms.
// false on client cancel or timeout.
func waitZenPool(ctx context.Context, maxWait time.Duration) bool {
	t := time.NewTimer(maxWait)
	defer t.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		zenProxiesMu.RLock()
		n := len(zenProxies)
		zenProxiesMu.RUnlock()
		if n > 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return false
		case <-tick.C:
		}
	}
}

// poolRefreshCooldown is the minimum gap between 429-triggered refreshes.
// 0 = 120s default, negative disables (every trip refreshes).
func poolRefreshCooldown() time.Duration {
	s := cfg().Zen.PoolRefreshCooldownSecs
	if s < 0 {
		return 0
	}
	if s == 0 {
		return 120 * time.Second
	}
	return time.Duration(s) * time.Second
}

func resetPoolRefresh429s() {
	poolRefresh429Mu.Lock()
	poolRefresh429s = 0
	poolRefresh429Mu.Unlock()
}

var (
	poolRefresh429Mu sync.Mutex
	poolRefresh429s  int
)

// laneTouch records a successful use: stamps last-used, clears the limit
// run, pins the working proxy, and counts the request against the model.
// token counts are not known yet at this point (upstream reports them in
// the body), so laneTokensAdd folds those in once the turn is parsed.
func laneTouch(l *zenLane, proxy, model string) {
	lanesMu.Lock()
	defer lanesMu.Unlock()
	l.lastUsed = time.Now()
	l.limitedSince = time.Time{}
	l.consec429 = 0
	l.requests++
	if proxy != "" {
		l.proxy = proxy
		l.country = proxyCountry(proxy)
	}
	if model == "" {
		return
	}
	if l.tokens == nil {
		l.tokens = map[string]laneTokens{}
	}
	m := l.tokens[model]
	m.Requests++
	l.tokens[model] = m
}

// laneTokensAdd folds one turn's upstream token counts into the lane's
// lifetime per-model totals. tok.Total falls back to prompt+completion
// when upstream omits it. durMs folds in for avg_ms; 0 skips (callers
// without a measured duration pass 0).
func laneTokensAdd(l *zenLane, model string, prompt, completion, total int, durMs int64) {
	if l == nil || model == "" {
		return
	}
	if total <= 0 {
		total = prompt + completion
	}
	if total == 0 && prompt == 0 && completion == 0 {
		return
	}
	lanesMu.Lock()
	defer lanesMu.Unlock()
	if l.tokens == nil {
		l.tokens = map[string]laneTokens{}
	}
	m := l.tokens[model]
	m.Prompt += prompt
	m.Completion += completion
	m.Total += total
	m.DurMs += durMs
	l.tokens[model] = m
}

// pickLaneProxy returns a verified proxy not already pinned by another
// lane and not burned for model, so lanes actually spread across distinct
// exits. custom proxies win; otherwise samples the latency-weighted pool
// and excludes taken exits. "" only when every pool exit is taken or the
// pool is empty.
func pickLaneProxy() string { return pickLaneProxyFor("") }

// pickLaneProxyFor is pickLaneProxy skipping exits burned for model.
// exclusions are a snapshot: a pin made a moment later can still collide,
// so pin paths re-validate under the write lock (pinUniqueProxy).
func pickLaneProxyFor(model string) string {
	taken := map[string]bool{}
	lanesMu.Lock()
	for _, l := range lanes {
		if l.proxy != "" {
			taken[l.proxy] = true
		}
	}
	lanesMu.Unlock()
	return pickLaneProxyExcluding(taken, model)
}

// pickLaneProxyExcluding is the picker with the taken set supplied, so a
// pin loop can retry under the write lock without re-entering lanesMu.
func pickLaneProxyExcluding(taken map[string]bool, model string) string {
	if cps := getCustomProxies(); len(cps) > 0 {
		var free []string
		for _, c := range cps {
			if !taken[c] && !exitBurned(c, model) && !exitLongBurned(c) && !exitTLSBanned(c) {
				free = append(free, c)
			}
		}
		if len(free) == 0 {
			return ""
		}
		sort.Slice(free, func(i, j int) bool { return proxyWeight(free[i]) > proxyWeight(free[j]) })
		return sampleWeighted(free, 1)[0]
	}

	zenProxiesMu.RLock()
	pool := append([]string(nil), zenProxies...)
	zenProxiesMu.RUnlock()
	if len(pool) == 0 {
		return ""
	}
	// untaken, unburned exits, fastest+most-reliable first. weighting
	// (not strict quartiles) keeps the whole pool in play while fast
	// exits win most draws. no hot-path race: pool entries are verified
	// on refresh, and verified-on-success keeps the set fresh. when the
	// pool is thinner than the lane count, taken exits fall through to
	// leastLoadedExit below instead of starving.
	var cands []string
	for _, p := range pool {
		if taken[p] || exitBurned(p, model) || exitLongBurned(p) || exitTLSBanned(p) {
			continue
		}
		cands = append(cands, p)
	}
	if len(cands) == 0 {
		// pool thinner than the lane count (cold start): share the
		// least-loaded unburned exit instead of starving lanes. one shared
		// exit 429s faster, but a lane with "" never sends at all, and the
		// retry loop rotates pins apart as more exits verify.
		if shared := leastLoadedExit(model); shared != "" {
			return shared
		}
		// dry pool: every exit is pinned or burned. on public proxies the
		// 429s that arm long-burns come from every other user of the exit,
		// so a full ban means the pool is starved, not saturated. give up
		// the freshest long-burns (one per call) rather than leaving the
		// caller with "": the alternative is guaranteed 502s until a restart.
		if unbanned := unbanOldestLongBurn(model); unbanned != "" {
			return unbanned
		}
		return ""
	}
	sort.Slice(cands, func(i, j int) bool { return proxyWeight(cands[i]) > proxyWeight(cands[j]) })
	return sampleWeighted(cands, 1)[0]
}

// leastLoadedExit returns the unburned pool exit held by the fewest lanes,
// "" when every exit is burned or banned. the retry path keeps pins apart
// once spares exist (laneReproxy prefers untaken), so sharing only binds
// while the pool is thinner than the lane count.
func leastLoadedExit(model string) string {
	zenProxiesMu.RLock()
	pool := append([]string(nil), zenProxies...)
	zenProxiesMu.RUnlock()
	if len(pool) == 0 {
		return ""
	}
	counts := map[string]int{}
	lanesMu.Lock()
	for _, l := range lanes {
		if l.proxy != "" {
			counts[l.proxy]++
		}
	}
	lanesMu.Unlock()
	best, bestN := "", -1
	for _, p := range pool {
		if exitBurned(p, model) || exitLongBurned(p) || exitTLSBanned(p) {
			continue
		}
		if n := counts[p]; bestN == -1 || n < bestN {
			best, bestN = p, n
		}
	}
	return best
}

// unbanOldestLongBurn lifts the soonest-lapsing 12h saturation ban that
// fits the model filter and returns that exit ("" when the ban map is
// empty). called only when every pool exit is pinned or burned, so it
// cannot race-select a healthy spare: anything here beats "".
func unbanOldestLongBurn(model string) string {
	longBurnMu.Lock()
	var bestP string
	var bestTime time.Time
	found := false
	pool := make([]string, 0, len(longBurned))
	zenProxiesMu.RLock()
	for _, p := range zenProxies {
		pool = append(pool, p)
	}
	zenProxiesMu.RUnlock()
	// prefer current-pool exits, freshest-lapsing ban wins. order outside
	// the pool does not matter: picking an exit no longer in the pool
	// gives nothing (it never gets probed again).
	for _, p := range pool {
		until, ok := longBurned[p]
		if ok && (!found || until.Before(bestTime)) {
			bestP, bestTime, found = p, until, true
		}
	}
	if !found {
		longBurnMu.Unlock()
		return ""
	}
	delete(longBurned, bestP)
	longBurnMu.Unlock()
	acclog.Printf("  zen long-burn lifted early (dry pool): %s", redactProxyUserinfo(bestP))
	// per-model burn still binds: if the exit is burned for this model,
	// lifting the long-burn gains nothing; report dry and let the caller
	// use the old pin (or fail over) as before.
	if exitBurned(bestP, model) {
		return ""
	}
	return bestP
}

// pinUniqueProxy pins an exit no other lane holds under lanesMu. the
// picker's taken snapshot goes stale between concurrent rotations, so a
// colliding pick is retried with a fresh exclusion set a bounded number of
// times. when the pool is thinner than the lane count sharing is
// deliberate and unavoidable (leastLoadedExit), so the verdict pins
// without a conflict check. "" means nothing pinnable: the caller keeps
// its old pin (a lane with "" never sends).
func pinUniqueProxy(l *zenLane, model string) string {
	zenProxiesMu.RLock()
	pool := len(zenProxies)
	zenProxiesMu.RUnlock()
	for i := 0; i < 10; i++ {
		// the picker may lock lanesMu internally (leastLoadedExit), so it
		// never runs under the write lock: snapshot, pick, then re-check.
		taken := map[string]bool{}
		lanesMu.Lock()
		for _, o := range lanes {
			if o != l && o.proxy != "" {
				taken[o.proxy] = true
			}
		}
		lanesMu.Unlock()
		p := pickLaneProxyExcluding(taken, model)
		if p == "" {
			return ""
		}
		lanesMu.Lock()
		if pool < len(lanes) {
			// thin pool: the pick is a deliberate share, pin it as-is.
			l.proxy = p
			l.country = proxyCountry(p)
			lanesMu.Unlock()
			return p
		}
		conflict := false
		for _, o := range lanes {
			if o != l && o.proxy == p {
				conflict = true
				break
			}
		}
		if !conflict {
			l.proxy = p
			l.country = proxyCountry(p)
			lanesMu.Unlock()
			return p
		}
		lanesMu.Unlock() // stale snapshot: re-pick with fresh exclusions
	}
	return ""
}

// laneReproxy swaps a dead exit for a fresh verified one that is not
// burned for model, keeping the lane session pinned (affinity survives
// proxy churn) and every other lane's exit untouched: two lanes never
// end up on the same exit while spares exist. no spare exit keeps the
// old pin: a lane with "" never sends, so the dead-but-working exit
// stays until a real replacement exists.
func laneReproxy(l *zenLane, model string) string {
	if p := pinUniqueProxy(l, model); p != "" {
		return p
	}
	lanesMu.Lock()
	defer lanesMu.Unlock()
	return l.proxy
}

// laneEscalate pins a fresh proxy to a lane that just got rate-limited or
// free-tier-blocked going direct. without it every lane shares one burned
// home ip and failover is a no-op: the retry loop re-sends direct and gets
// the same 403. after escalation laneProxyFor returns this exit.
func laneEscalate(l *zenLane, model string) string {
	p := pinUniqueProxy(l, model)
	if p == "" {
		return ""
	}
	lanesMu.Lock()
	l.escalated = true
	lanesMu.Unlock()
	acclog.Printf("  opencode lane %s escalating direct -> %s (%s)", l.id, redactProxyUserinfo(p), proxyCountry(p))
	return p
}

// laneStaleRun reports how long the lane's current consecutive limit run
// has lasted. 0 means the lane is not in a run.
func laneStaleRun(l *zenLane) time.Duration {
	lanesMu.Lock()
	defer lanesMu.Unlock()
	if l.limitedSince.IsZero() {
		return 0
	}
	return time.Since(l.limitedSince)
}

// --- capacity tracking (low-load lane race) ---

// laneUse is one sample on a lane: an open marks a request starting, a
// close marks it finishing.
type laneUse struct {
	at     time.Time
	opened bool
}

var (
	useMu  sync.Mutex
	useLog []laneUse
)

// noteLaneUse records a lane taking (opened) or finishing (close) a request.
func noteLaneUse(opened bool) {
	at := time.Now()
	useMu.Lock()
	useLog = append(useLog, laneUse{at: at, opened: opened})
	cut := at.Add(-laneUseWindow())
	i := 0
	for i < len(useLog) && useLog[i].at.Before(cut) {
		i++
	}
	if i > 0 {
		useLog = append([]laneUse(nil), useLog[i:]...)
	}
	useMu.Unlock()
}

// laneUseWindow is how far back utilization is measured. configurable since
// the idle threshold only means something over a stated window.
func laneUseWindow() time.Duration {
	if m := cfg().Zen.RaceUseWindowMinutes; m > 0 {
		return time.Duration(m) * time.Minute
	}
	return 10 * time.Minute
}

// laneUtilization reports the peak share of lanes that were busy at once
// inside the window: busy-when-busiest is exactly what decides whether a
// spare lane is free to race on. returns 0 when nothing was recorded.
func laneUtilization() float64 {
	cut := time.Now().Add(-laneUseWindow())
	type ev struct {
		at   time.Time
		open bool
	}
	var evs []ev
	useMu.Lock()
	for _, u := range useLog {
		if u.at.After(cut) {
			evs = append(evs, ev{at: u.at, open: u.opened})
		}
	}
	useMu.Unlock()
	lanesMu.Lock()
	n := len(lanes)
	lanesMu.Unlock()
	if len(evs) == 0 || n == 0 {
		return 0
	}
	// samples arrive in time order per lane (one open, one close), so a
	// running depth count is the concurrent lane count.
	cur, peak := 0, 0
	for _, e := range evs {
		if e.open {
			cur++
			if cur > peak {
				peak = cur
			}
		} else if cur > 0 {
			cur--
		}
	}
	share := float64(peak) / float64(n)
	if share > 1 {
		share = 1
	}
	return share
}

// lanesIdleEnough reports the measured busy share and the threshold. when
// the share is under the threshold the pool has spare exits, so racing two
// lanes is cheap enough to cut ttft. disabled -> never idle.
func lanesIdleEnough() (share, threshold float64) {
	threshold = 0.30
	if t := cfg().Zen.RaceUtilizationThreshold; t > 0 {
		threshold = float64(t) / 100 // config is a percent
	}
	if !cfg().Zen.LaneRaceEnabled {
		return 1, threshold
	}
	return laneUtilization(), threshold
}

// lastZenSuccess tracks the last working zen response, so the retry loops
// can tell "all exits limited for a while" (stale pool/sessions) from a
// momentary burst.
var (
	zenSuccessMu sync.Mutex
	zenSuccessAt time.Time
)

func noteZenSuccessAt() {
	zenSuccessMu.Lock()
	zenSuccessAt = time.Now()
	zenSuccessMu.Unlock()
}

func zenStaleSince() time.Duration {
	zenSuccessMu.Lock()
	defer zenSuccessMu.Unlock()
	if zenSuccessAt.IsZero() {
		return 0
	}
	return time.Since(zenSuccessAt)
}

// maybeRefreshStalePool fires a pool refresh + session rotation when no
// request has succeeded for 15s and this lane's limit run is that old:
// the pool/sessions are stale, not just busy. returns true when it fired.
func maybeRefreshStalePool(l *zenLane) bool {
	stale := 15 * time.Second
	if s := cfg().Zen.StalePoolSecs; s > 0 {
		stale = time.Duration(s) * time.Second
	}
	if zenStaleSince() < stale {
		return false
	}
	if laneStaleRun(l) < stale {
		return false
	}
	acclog.Printf("  zen: lane %s limited %v+ with no success, refreshing pool + sessions", l.id, stale)
	refreshZenProxiesAsync()
	resetStaleLanes()
	return true
}

// resetStaleLanes mints fresh sessions after a stale refresh, so retry
// loops re-enter on new sessions instead of dead ones. pins stay: an old
// exit keeps serving until a real send proves it dead, never cleared in
// bulk across every lane at once.
func resetStaleLanes() {
	lanesMu.Lock()
	defer lanesMu.Unlock()
	for _, l := range lanes {
		l.limitedSince = time.Time{}
		l.session = zenSession()
	}
	// zenSuccessAt intentionally untouched: no success happened, and
	// stamping it delays the next stale-pool trip during ongoing failure.
}

// sweepLanes runs on the pool refresh tick: drops lanes idle past ttl and
// releases exits idle past lane_release_secs, so a lane that has been cold
// resumes on a fresh proxy instead of a stale one. lanes idle past
// session_idle_minutes are evicted too, clearing the session hash/id from
// cache while lane_ttl_minutes stays the hard cap.
func sweepLanes() {
	cutoff := time.Now().Add(-laneTTL())
	rel := laneRelease()
	idle := sessionIdleTTL()
	var idleCutoff time.Time
	if idle > 0 {
		idleCutoff = time.Now().Add(-idle)
	}
	convo := convoIdleTTL()
	var convoCutoff time.Time
	if convo > 0 {
		convoCutoff = time.Now().Add(-convo)
	}
	lanesMu.Lock()
	for k, l := range lanes {
		if l.lastUsed.Before(cutoff) {
			delete(lanes, k)
			dropConvoSeen(k)
			continue
		}
		if !convoCutoff.IsZero() && strings.HasPrefix(k, "convo:") && l.lastUsed.Before(convoCutoff) {
			delete(lanes, k)
			dropConvoSeen(k)
			continue
		}
		if !idleCutoff.IsZero() && l.lastUsed.Before(idleCutoff) {
			delete(lanes, k)
			dropConvoSeen(k)
			continue
		}
		if rel > 0 && l.proxy != "" && time.Since(l.lastUsed) > rel {
			l.proxy = ""
		}
	}
	lanesMu.Unlock()
	resetPoolRefresh429s()
	sweepBurns()
	// scan-cache entries idle past the same ttl go too; vault entries
	// keep their own (longer) ttl and are untouched.
	if idle > 0 {
		if n := pii.SweepScanIdle(idle); n > 0 {
			acclog.Printf("  pii scan cache: %d idle entr(ies) evicted", n)
		}
	}
}

// redactProxyUserinfo strips user:pass from a proxy url so a credentialed
// proxy never reaches /status, which is open even when auth.tokens is set.
// built by hand: url.String() would percent-escape the mask.
func redactProxyUserinfo(p string) string {
	if p == "" {
		return ""
	}
	i := strings.Index(p, "://")
	if i < 0 {
		return p
	}
	rest := p[i+3:]
	slash := strings.Index(rest, "/")
	// split userinfo at the LAST @ before the first slash: passwords may
	// contain @ (first-@ split leaks the tail, net/url splits last).
	at := strings.LastIndex(rest, "@")
	// an @ before the first slash means the authority carries credentials
	// (user or user:pass) — redact the whole userinfo segment.
	if at < 0 || (slash >= 0 && slash < at) {
		return p
	}
	return p[:i+3] + "***@" + rest[at+1:]
}

func laneSnapshot() []laneStatus {
	lanesMu.Lock()
	defer lanesMu.Unlock()
	now := time.Now()
	out := make([]laneStatus, 0, len(lanes))
	for _, l := range lanes {
		// lane ids derive from the verbatim client x-session-id: hash
		// them on /status instead of echoing client identity back.
		id := l.id
		if strings.HasPrefix(id, "hdr:") {
			sum := sha256.Sum256([]byte(id))
			id = "hdr:" + hex.EncodeToString(sum[:])[:12]
		}
		st := laneStatus{
			ID: id, Proxy: redactProxyUserinfo(l.proxy), Country: l.country,
			LastUsed: now.Sub(l.lastUsed).Round(time.Second).String(),
			Requests: l.requests, RateLimit: l.rateLimit,
		}
		var dur int64
		for _, m := range l.tokens {
			st.Prompt += m.Prompt
			st.Completion += m.Completion
			st.Tokens += m.Total
			dur += m.DurMs
		}
		if st.Requests > 0 {
			st.AvgMs = int(dur / int64(st.Requests))
		}
		if len(l.tokens) > 0 {
			st.ByModel = make(map[string]laneTokens, len(l.tokens))
			for k, v := range l.tokens {
				st.ByModel[k] = v
			}
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// laneSession returns the lane's stable session id under lock.
func laneSession(l *zenLane) string {
	lanesMu.Lock()
	defer lanesMu.Unlock()
	return l.session
}

// laneClaim tracks use so failover prefers lanes without active requests.
func laneClaim(l *zenLane) {
	lanesMu.Lock()
	defer lanesMu.Unlock()
	l.inflight++
}

// laneDone releases this request's claim, so failover sees the lane as
// idle again. no cooldown: the next request paces on laneGate instead.
func laneDone(l *zenLane) {
	lanesMu.Lock()
	defer lanesMu.Unlock()
	if l.inflight > 0 {
		l.inflight--
	}
}

// laneHandoff transfers a request's claim on failover: the lane it leaves
// is idle again, the lane it takes over counts the send.
func laneHandoff(old, new *zenLane) *zenLane {
	if old == new {
		return new
	}
	lanesMu.Lock()
	defer lanesMu.Unlock()
	if old.inflight > 0 {
		old.inflight--
	}
	new.inflight++
	return new
}

// laneFailover returns the stalest lane other than l so the retry spreads
// to another exit. idle lanes win; when all are busy use the least loaded.
// never nil: a one-lane pool reuses its lane without a cooldown.
func laneFailover(l *zenLane) *zenLane {
	lanesMu.Lock()
	defer lanesMu.Unlock()
	now := time.Now()
	var best *zenLane
	for _, c := range lanes {
		if c == l {
			continue
		}
		if best == nil || c.inflight < best.inflight || (c.inflight == best.inflight && c.lastUsed.Before(best.lastUsed)) {
			best = c
		}
	}
	if best != nil {
		// stamp the hand-out: without it concurrent retries converge on
		// the same stalest lane.
		best.lastUsed = now
		return best
	}
	if len(lanes) < laneCap() {
		nl := &zenLane{id: "shared", session: zenSession(), lastUsed: now}
		k := "shared:" + nl.session[len(nl.session)-6:]
		nl.id = shortLaneID(k)
		lanes[k] = nl
		return nl
	}
	return l
}

// laneProxyFor returns the lane's pinned proxy, re-picking when empty or
// burned for model. no hot-path probe: the pool is verified on refresh,
// dead exits die on real sends, and a probe here costs a full round trip
// on every request. "" means direct, correct on a cold lane when
// always_proxy is off; once escalated the pinned exit wins regardless.
// a burned pin rotates this lane only: the pool exit stays (other models
// and lanes still use it) and the pin is never cleared before its
// replacement exists (laneReproxy keeps it when dry).
func laneProxyFor(l *zenLane, model string) string {
	lanesMu.Lock()
	p := l.proxy
	escalated := l.escalated
	lanesMu.Unlock()
	if p != "" && !exitBurned(p, model) && !exitLongBurned(p) && !exitTLSBanned(p) {
		// a cold start pins every lane to the one exit the verify sweep
		// published first. the pin stays valid forever after, so without
		// rebalancing the whole table rides one ip while 12 others sit
		// unused. spread when the pin is crowded and a spare exists.
		if lanePinCrowded(p) {
			return laneReproxy(l, model)
		}
		return p
	}
	if !cfg().Zen.AlwaysProxy && !escalated {
		return ""
	}
	return laneReproxy(l, model)
}

// lanePinCrowded reports whether pin is held by 3+ lanes and some pool exit
// is untaken. the 3 threshold (not 2) leaves hysteresis: with a pool
// thinner than the lane count a pair must share, and re-picking on every
// request would thrash them between the same two exits.
func lanePinCrowded(pin string) bool {
	lanesMu.Lock()
	n := 0
	for _, l := range lanes {
		if l.proxy == pin {
			n++
		}
	}
	lanesMu.Unlock()
	if n < 3 {
		return false
	}
	used := map[string]bool{}
	lanesMu.Lock()
	for _, l := range lanes {
		if l.proxy != "" {
			used[l.proxy] = true
		}
	}
	lanesMu.Unlock()
	zenProxiesMu.RLock()
	defer zenProxiesMu.RUnlock()
	for _, p := range zenProxies {
		if !used[p] {
			return true
		}
	}
	return false
}
