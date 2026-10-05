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
}

type laneStatus struct {
	ID        string `json:"id"`
	Proxy     string `json:"proxy"`
	Country   string `json:"country"`
	LastUsed  string `json:"last_used_ago"`
	Requests  int    `json:"requests"`
	RateLimit int    `json:"rate_limited"`
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

// noteExit429 records a 429 on an exit. twice in a row with 20s+ since the
// last success means long-term saturation: 12h ban for every model.
func noteExit429(proxy string) bool {
	if proxy == "" {
		return false
	}
	h := exitHealthFor(proxy)
	exitTrackMu.Lock()
	h.streak++
	streak, lastOK := h.streak, h.lastOK
	exitTrackMu.Unlock()
	if streak >= 2 && time.Since(lastOK) > 20*time.Second {
		longBurnMu.Lock()
		longBurned[proxy] = time.Now().Add(12 * time.Hour)
		longBurnMu.Unlock()
		acclog.Printf("  zen exit %s long-burned 12h (429 streak, no success 20s+)", redactProxyUserinfo(proxy))
		return true
	}
	return false
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
		laneDropProxy(proxy)
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

// fires hedge concurrently: first completed response wins, the loser is
// cancelled (context + body). chat completions are read-only from our side,
// so double-send is safe. returns which side won (true = hedge).
// both sides derive from ctx, so a client disconnect cancels the upstream
// request instead of burning an exit on output nobody reads.
func hedgedDo(ctx context.Context, primary, hedge func(context.Context) (*http.Response, error), hedgeAfter time.Duration) (*http.Response, error, bool) {
	if hedgeAfter <= 0 || hedge == nil {
		resp, err := primary(ctx)
		return resp, err, false
	}
	pcancel, primaryCh := zenSide(ctx, primary)
	timer := time.NewTimer(hedgeAfter)
	defer timer.Stop()
	select {
	case r := <-primaryCh:
		// winner: its context dies with its body (bodyCancelCloser).
		return r.resp, r.err, false
	case <-timer.C:
	case <-ctx.Done():
		// client gone before the hedge fired: cancel the primary and
		// return immediately so no orphan request keeps running.
		pcancel()
		if r := <-primaryCh; r.resp != nil && r.resp.Body != nil {
			r.resp.Body.Close()
		}
		return nil, ctx.Err(), false
	}
	hcancel, hedgeCh := zenSide(ctx, hedge)
	select {
	case r := <-primaryCh:
		// loser still running: cancel it now, close its body when it
		// lands to free the conn. winner keeps its context via its body.
		hcancel()
		go func() {
			if hr := <-hedgeCh; hr.resp != nil && hr.resp.Body != nil {
				hr.resp.Body.Close()
			}
		}()
		return r.resp, r.err, false
	case r := <-hedgeCh:
		pcancel()
		go func() {
			if pr := <-primaryCh; pr.resp != nil && pr.resp.Body != nil {
				pr.resp.Body.Close()
			}
		}()
		return r.resp, r.err, true
	case <-ctx.Done():
		pcancel()
		hcancel()
		go func() {
			if pr := <-primaryCh; pr.resp != nil && pr.resp.Body != nil {
				pr.resp.Body.Close()
			}
		}()
		go func() {
			if hr := <-hedgeCh; hr.resp != nil && hr.resp.Body != nil {
				hr.resp.Body.Close()
			}
		}()
		return nil, ctx.Err(), false
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
	return 5 // default: first try + 4 retries
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
	if !wantHdr && !wantConvo && !wantShared {
		wantShared = true // all off: spread anyway, never fail a request
	}
	key := ""
	if headerSession != "" && wantHdr {
		key = "hdr:" + headerSession
	} else if wantConvo {
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

// convertSharedLocked renames the idlest shared lane to key, minting a fresh
// session for the new identity. caller holds lanesMu. nil when no shared
// lane exists to convert.
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
	victim.session = zenSession()
	victim.proxy = ""
	victim.limitedSince = time.Time{}
	victim.lastUsed = time.Now()
	victim.id = shortLaneID(key)
	lanes[key] = victim
	return victim
}

// ensureSharedLanes pre-opens shared lanes up to the cap so a cold start
// does not serialize early requests on lane creation + exit races. runs
// once at startup after the first pool refresh.
func ensureSharedLanes() {
	_, _, wantShared := laneModes()
	if !wantShared {
		return
	}
	lanesMu.Lock()
	defer lanesMu.Unlock()
	for len(lanes) < laneCap() {
		l := &zenLane{id: "shared", session: zenSession(), lastUsed: time.Now()}
		k := "shared:" + l.session[len(l.session)-6:]
		l.id = shortLaneID(k)
		lanes[k] = l
	}
}

func shortLaneID(key string) string {
	if len(key) <= 12 {
		return key
	}
	return key[:8] + "..." + key[len(key)-4:]
}

// sharedLaneLocked picks the lru shared lane, creating shared-n slots up
// to cap. caller holds lanesMu. no health filtering: lanes never cool, a
// limit rotates the exit instead of blocking the lane.
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
// past stale_pool_secs trips the stale-pool refresh. returns the new
// session id.
func laneRotate(l *zenLane, model string) string {
	lanesMu.Lock()
	l.rateLimit++
	if l.limitedSince.IsZero() {
		l.limitedSince = time.Now()
	}
	l.session = zenSession()
	l.lastUsed = time.Now()
	lanesMu.Unlock()
	laneReproxy(l, model)
	noteRateLimit()
	return laneSession(l)
}

// noteRateLimit counts lanes rate-limited since the last pool refresh and
// fires one once the threshold is crossed: when several distinct exits are
// limited at once, the whole pool is stale, not one proxy.
func noteRateLimit() {
	n := cfg().Zen.PoolRefresh429s
	if n <= 0 {
		return
	}
	poolRefresh429Mu.Lock()
	poolRefresh429s++
	hit := poolRefresh429s >= n
	if hit {
		poolRefresh429s = 0
	}
	poolRefresh429Mu.Unlock()
	if hit {
		acclog.Printf("  zen: %d rate-limited lanes, refreshing proxy pool", n)
		refreshZenProxiesAsync()
	}
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
// run, pins the working proxy.
func laneTouch(l *zenLane, proxy string) {
	lanesMu.Lock()
	defer lanesMu.Unlock()
	l.lastUsed = time.Now()
	l.limitedSince = time.Time{}
	l.requests++
	if proxy != "" {
		l.proxy = proxy
		l.country = proxyCountry(proxy)
	}
}

// pickLaneProxy returns a verified proxy not already pinned by another
// lane and not burned for model, so lanes actually spread across distinct
// exits. custom proxies win; otherwise samples the latency-weighted pool
// and excludes taken exits. "" only when every pool exit is taken or the
// pool is empty.
func pickLaneProxy() string { return pickLaneProxyFor("") }

// pickLaneProxyFor is pickLaneProxy skipping exits burned for model.
func pickLaneProxyFor(model string) string {
	taken := map[string]bool{}
	lanesMu.Lock()
	for _, l := range lanes {
		if l.proxy != "" {
			taken[l.proxy] = true
		}
	}
	lanesMu.Unlock()

	if cps := getCustomProxies(); len(cps) > 0 {
		var free []string
		for _, c := range cps {
			if !taken[c] && !exitBurned(c, model) && !exitLongBurned(c) {
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
	// on refresh, and verified-on-success keeps the set fresh.
	var cands []string
	for _, p := range pool {
		if taken[p] || exitBurned(p, model) || exitLongBurned(p) {
			continue
		}
		cands = append(cands, p)
	}
	if len(cands) == 0 {
		return ""
	}
	sort.Slice(cands, func(i, j int) bool { return proxyWeight(cands[i]) > proxyWeight(cands[j]) })
	return sampleWeighted(cands, 1)[0]
}

// laneReproxy swaps a dead exit for a fresh verified one that no other lane
// holds and that is not burned for model, keeping the lane session pinned
// (affinity survives proxy churn).
func laneReproxy(l *zenLane, model string) string {
	p := pickLaneProxyFor(model)
	lanesMu.Lock()
	defer lanesMu.Unlock()
	l.proxy = p
	l.country = proxyCountry(p)
	return p
}

// laneDropProxy tells lanes holding a burned proxy to forget it; they pick
// a fresh exit on next use instead of reusing a dead one.
func laneDropProxy(proxyURL string) {
	if proxyURL == "" {
		return
	}
	lanesMu.Lock()
	defer lanesMu.Unlock()
	for _, l := range lanes {
		if l.proxy == proxyURL {
			l.proxy = ""
		}
	}
}

// laneEscalate pins a fresh proxy to a lane that just got rate-limited or
// free-tier-blocked going direct. without it every lane shares one burned
// home ip and failover is a no-op: the retry loop re-sends direct and gets
// the same 403. after escalation laneProxyFor returns this exit.
func laneEscalate(l *zenLane, model string) string {
	p := pickLaneProxyFor(model)
	if p == "" {
		return ""
	}
	lanesMu.Lock()
	l.proxy = p
	l.country = proxyCountry(p)
	l.escalated = true
	lanesMu.Unlock()
	acclog.Printf("  opencode lane %s escalating direct -> %s (%s)", l.id, p, proxyCountry(p))
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

// resetStaleLanes mints fresh sessions and drops pinned exits after a stale
// refresh, so retry loops re-enter on new sessions instead of dead ones.
func resetStaleLanes() {
	lanesMu.Lock()
	defer lanesMu.Unlock()
	for _, l := range lanes {
		l.limitedSince = time.Time{}
		l.proxy = ""
		l.session = zenSession()
	}
	zenSuccessMu.Lock()
	zenSuccessAt = time.Now()
	zenSuccessMu.Unlock()
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

// lanesRevalidate clears lane exits that are no longer in the freshly
// verified pool, so a refresh actually re-points the lanes instead of
// leaving them pinned to proxies that just died.
func lanesRevalidate(verified []string) {
	if len(verified) == 0 {
		return
	}
	inPool := make(map[string]bool, len(verified))
	for _, v := range verified {
		inPool[v] = true
	}
	lanesMu.Lock()
	stale := 0
	for _, l := range lanes {
		if l.proxy != "" && !inPool[l.proxy] {
			l.proxy = ""
			stale++
		}
	}
	lanesMu.Unlock()
	if stale > 0 {
		acclog.Printf("  zen lanes: %d stale exit(s) cleared by pool refresh", stale)
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
	at := strings.Index(rest, "@")
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
		st := laneStatus{
			ID: l.id, Proxy: redactProxyUserinfo(l.proxy), Country: l.country,
			LastUsed: now.Sub(l.lastUsed).Round(time.Second).String(),
			Requests: l.requests, RateLimit: l.rateLimit,
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

// laneFailover returns the stalest lane other than l so the retry spreads
// to another exit. never nil: worst case the caller reuses l on its fresh
// rotation instead of waiting on a cooldown.
func laneFailover(l *zenLane) *zenLane {
	lanesMu.Lock()
	defer lanesMu.Unlock()
	now := time.Now()
	var best *zenLane
	for _, c := range lanes {
		if c == l {
			continue
		}
		if best == nil || c.lastUsed.Before(best.lastUsed) {
			best = c
		}
	}
	if best != nil {
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
func laneProxyFor(l *zenLane, model string) string {
	lanesMu.Lock()
	p := l.proxy
	escalated := l.escalated
	lanesMu.Unlock()
	if p != "" && !exitBurned(p, model) && !exitLongBurned(p) {
		return p
	}
	if p != "" {
		lanesMu.Lock()
		if l.proxy == p {
			l.proxy = ""
		}
		lanesMu.Unlock()
		if !exitBurned(p, model) && !exitLongBurned(p) {
			dropProxy(p)
			laneDropProxy(p)
		}
	}
	if !cfg().Zen.AlwaysProxy && !escalated {
		return ""
	}
	return laneReproxy(l, model)
}
