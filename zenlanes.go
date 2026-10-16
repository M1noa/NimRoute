package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
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
	// consec429 counts consecutive limit events on this lane since its last
	// success, and rateLimit the total. both describe the exit the lane is
	// pinned to, so a pin change resets them (stampLaneProxy).
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

// stampLaneProxy sets a lane's pin. the counters are reset when the exit
// actually changes: they describe one exit's health, and carrying a dead
// exit's 429s onto its replacement would write off a fresh exit. caller
// holds lanesMu (limit runs are deliberately not cleared: limitedSince is
// what trips the stale-pool refresh).
func stampLaneProxy(l *zenLane, p string) {
	if p == l.proxy {
		return
	}
	l.proxy = p
	l.country = proxyCountry(p)
	l.consec429 = 0
	l.rateLimit = 0
}

// setLaneProxy re-pins a lane under the lock.
func setLaneProxy(l *zenLane, p string) {
	lanesMu.Lock()
	stampLaneProxy(l, p)
	lanesMu.Unlock()
}

// ensureLanePins gives every lane a usable exit: lanes with no pin, and
// lanes whose pin left the pool or got marked, get a fresh unique pick.
// runs after each pool refresh, so a rotation that emptied the table comes
// back with every lane serving on its own exit instead of limping direct.
func ensureLanePins() {
	lanesMu.Lock()
	var stale []*zenLane
	for _, l := range lanes {
		if l.proxy == "" || !exitUsable(l.proxy) || exitOutOfPool(l.proxy) {
			stale = append(stale, l)
		}
	}
	lanesMu.Unlock()
	for _, l := range stale {
		laneReproxy(l, "")
	}
}

func countRateLimited() int {
	exitStateMu.Lock()
	defer exitStateMu.Unlock()
	now := time.Now()
	n := 0
	for _, st := range exitStates {
		for model, mk := range st.rl {
			if mk.lapsed(now) {
				delete(st.rl, model)
				continue
			}
			if mk.marked(now) {
				n++
				break
			}
		}
	}
	return n
}

func countDeadExits() int {
	exitStateMu.Lock()
	defer exitStateMu.Unlock()
	now := time.Now()
	n := 0
	for _, st := range exitStates {
		if now.Before(st.deadUntil) {
			n++
		}
	}
	return n
}

// exitMark is one marked exit as /status reports it.
type exitMark struct {
	Proxy  string
	State  string
	Reason string
}

// markedExits lists the exits carrying a live mark, pool member or not, so
// /status can explain a pool that looks thin and an operator can tell a
// rate-limited exit from a dead one without reading the log.
func markedExits() []exitMark {
	exitStateMu.Lock()
	defer exitStateMu.Unlock()
	now := time.Now()
	var out []exitMark
	for p, st := range exitStates {
		switch {
		case exitRateLimitedLocked(st, now):
			out = append(out, exitMark{Proxy: p, State: "rate_limited", Reason: st.reason})
		case now.Before(st.deadUntil):
			out = append(out, exitMark{Proxy: p, State: "dead", Reason: st.reason})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Proxy < out[j].Proxy })
	return out
}

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

// exit health: one registry for every exit the pool has seen. it replaces
// the four overlapping ban maps that used to live here (per-model burns,
// 12h saturation bans, tls bans, slow strikes), which fought each other:
// an exit could be dead by one rule and "unbanned" by another, and lanes
// ended up with no usable exit at all.
//
// two states, both temporary:
//
//	rate_limited: the exit answered 429 for one model too often (twice in a
//	  row, or six times since the last pool refresh). the mark is per
//	  exit+model: zen's free tier limits the pair, not the exit, so an exit
//	  that is busy on one model keeps serving every other model. a successful
//	  pool refresh clears the marks, so a busy pair gets a clean slate every
//	  refresh cycle.
//	dead: the exit failed at transport/tls/free-tier level. it stays out
//	  for deadTTL and only comes back if it verifies again.
//
// owner: the pool rotation. the invariant every caller relies on is
// exitUsableFor: a live pool exit that is neither dead, marked for that
// model, nor refusing that model.
type exitState struct {
	// rl is the per-model 429 mark. keyed by model, cleared by the pool
	// refresh that starts a new cycle.
	rl        map[string]exitMark429
	deadUntil time.Time            // dead until
	reason    string               // last mark, for /status and logs
	models    map[string]time.Time // model -> refusing it until (403 free-tier)
	lastOK    time.Time
	strikes   []time.Time // >6s first-byte stalls in the last hour
}

// exitMark429 is one exit+model pair's 429 bookkeeping: the streak and cycle
// total always, and the mark itself once the policy trips. at/until stay zero
// while the pair is merely counting, which is how a lapsed mark is told apart
// from a pair that has not tripped yet.
type exitMark429 struct {
	streak int
	total  int
	at     time.Time // when the mark was placed (secondChancePick's ordering)
	until  time.Time
}

// lapsed drops a tripped mark whose cycle is over, leaving the counters in
// place. caller holds exitStateMu. returns false when no mark is live.
func (mk exitMark429) lapsed(now time.Time) bool {
	return !mk.until.IsZero() && now.After(mk.until)
}

// marked reports whether the pair carries a live mark right now.
func (mk exitMark429) marked(now time.Time) bool {
	return !mk.until.IsZero() && !now.After(mk.until)
}

var (
	exitStateMu sync.Mutex
	exitStates  = map[string]*exitState{}
)

// exitStateFor returns the registry row for an exit, creating it on first use.
func exitStateFor(proxy string) *exitState {
	exitStateMu.Lock()
	defer exitStateMu.Unlock()
	st, ok := exitStates[proxy]
	if !ok {
		st = &exitState{}
		exitStates[proxy] = st
	}
	return st
}

// exitRateLimited reports whether an exit carries a live 429 mark for any
// model. the model-specific question is exitRateLimitedModel: an exit marked
// for one model still serves the rest.
func exitRateLimited(proxy string) bool {
	if proxy == "" {
		return false
	}
	exitStateMu.Lock()
	defer exitStateMu.Unlock()
	st, ok := exitStates[proxy]
	if !ok {
		return false
	}
	now := time.Now()
	any := false
	for model, mk := range st.rl {
		if mk.lapsed(now) {
			delete(st.rl, model)
			continue
		}
		if mk.marked(now) {
			any = true
		}
	}
	return any
}

// exitRateLimitedModel reports whether an exit is cooling off a 429 burst on
// one model. a lapsed mark is forgotten here, so the pair gets an honest
// retry rather than an ever-green mark.
func exitRateLimitedModel(proxy, model string) bool {
	if proxy == "" || model == "" {
		return false
	}
	exitStateMu.Lock()
	defer exitStateMu.Unlock()
	st, ok := exitStates[proxy]
	if !ok {
		return false
	}
	mk, ok := st.rl[model]
	if !ok {
		return false
	}
	if mk.lapsed(time.Now()) {
		delete(st.rl, model)
		return false
	}
	return mk.marked(time.Now())
}

// exitDead reports whether an exit is out for a hard failure: transport
// reset, tls interception, or a free-tier burn for a model.
func exitDead(proxy string) bool {
	if proxy == "" {
		return false
	}
	exitStateMu.Lock()
	defer exitStateMu.Unlock()
	st, ok := exitStates[proxy]
	if !ok || st.deadUntil.IsZero() {
		return false
	}
	if time.Now().After(st.deadUntil) {
		st.deadUntil = time.Time{}
		return false
	}
	return true
}

// exitUsable reports whether an exit may be pinned right now. "" is never
// usable: it means "no proxy", which is what lanes must not hold.
func exitUsable(proxy string) bool {
	return proxy != "" && !exitDead(proxy) && !exitRateLimited(proxy)
}

// exitModelBurned reports whether an exit is refusing one model this cycle.
// the free-tier 403 is per-exit-per-model: a burned exit still serves every
// other model, so it stays in the pool instead of being written off.
func exitModelBurned(proxy, model string) bool {
	if proxy == "" || model == "" {
		return false
	}
	exitStateMu.Lock()
	defer exitStateMu.Unlock()
	st, ok := exitStates[proxy]
	if !ok || st.models == nil {
		return false
	}
	until, ok := st.models[model]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(st.models, model)
		return false
	}
	return true
}

// exitUsableFor reports whether an exit may serve one model: not dead, not
// marked for that model, and not refusing it this cycle. a mark for a
// different model never blocks this one.
func exitUsableFor(proxy, model string) bool {
	return proxy != "" && !exitDead(proxy) &&
		!exitRateLimitedModel(proxy, model) && !exitModelBurned(proxy, model)
}

// noteExitModelBurned marks an exit refusing one model for the rest of the
// cycle. the exit stays poolable for every other model, and the next pool
// refresh forgets the refusal.
func noteExitModelBurned(proxy, model string) {
	if proxy == "" || model == "" {
		return
	}
	st := exitStateFor(proxy)
	exitStateMu.Lock()
	if st.models == nil {
		st.models = map[string]time.Time{}
	}
	_, had := st.models[model]
	st.models[model] = time.Now().Add(poolInterval())
	st.reason = "free-tier 403 on " + model
	exitStateMu.Unlock()
	if !had {
		acclog.Printf("  zen exit %s refuses %s until next refresh (free-tier 403)", redactProxyUserinfo(proxy), model)
	}
}

// exitRateLimitedLocked is exitRateLimited for a row already under
// exitStateMu, dropping lapsed marks as it reads.
func exitRateLimitedLocked(st *exitState, now time.Time) bool {
	any := false
	for model, mk := range st.rl {
		if mk.lapsed(now) {
			delete(st.rl, model)
			continue
		}
		if mk.marked(now) {
			any = true
		}
	}
	return any
}

// exitStateOf is the /status read of one exit: "working", "rate_limited",
// "dead", or "" when the exit is unknown to the registry.
func exitStateOf(proxy string) string {
	if proxy == "" {
		return ""
	}
	if exitDead(proxy) {
		return "dead"
	}
	if exitRateLimited(proxy) {
		return "rate_limited"
	}
	exitStateMu.Lock()
	_, seen := exitStates[proxy]
	exitStateMu.Unlock()
	if !seen {
		return ""
	}
	return "working"
}

// poolInterval is the scheduled refresh period. the pool refetches on this
// tick whether or not anything is wrong, so a rate-limited or dead exit
// always gets another chance within one interval. also the rate-limit
// cooldown: a mark lasts exactly one cycle and the sweep clears it.
func poolInterval() time.Duration {
	if m := cfg().Zen.RefreshIntervalMinutes; m > 0 {
		return time.Duration(m) * time.Minute
	}
	return 30 * time.Minute
}

// deadTTL is how long a dead exit stays out. exit_burn_minutes is the
// existing "keep this exit out for a while" knob (30m default); dead exits
// still have to verify again on the next sweep before they are used.
func deadTTL() time.Duration {
	if m := cfg().Zen.ExitBurnMinutes; m > 0 {
		return time.Duration(m) * time.Minute
	}
	return 30 * time.Minute
}

// markExitRateLimited pulls one exit+model pair out of rotation for a
// refresh cycle: log it, mark it, and leave every other model on that exit
// alone. the exit stays in the pool on purpose: it still serves other models
// and it will verify again, and the lane that tripped this already rotated
// off it, so no other lane is moved.
func markExitRateLimited(proxy, model, reason string) {
	if proxy == "" {
		return
	}
	st := exitStateFor(proxy)
	exitStateMu.Lock()
	if st.rl == nil {
		st.rl = map[string]exitMark429{}
	}
	mk := st.rl[model]
	mk.at = time.Now()
	mk.until = mk.at.Add(poolInterval())
	st.rl[model] = mk
	st.reason = reason + " on " + model
	exitStateMu.Unlock()
	acclog.Printf("  zen exit %s rate-limited for %s until next refresh (%s: %d in a row, %d this cycle)",
		redactProxyUserinfo(proxy), model, reason, mk.streak, mk.total)
}

// markExitDead pulls an exit out for a hard failure and logs why.
func markExitDead(proxy, reason string) {
	if proxy == "" {
		return
	}
	st := exitStateFor(proxy)
	exitStateMu.Lock()
	already := !st.deadUntil.IsZero() && time.Now().Before(st.deadUntil)
	st.deadUntil = time.Now().Add(deadTTL())
	st.reason = reason
	exitStateMu.Unlock()
	if already {
		return // mark already stood: no churn on every failure past the bar
	}
	acclog.Printf("  zen exit %s dead %v (%s)", redactProxyUserinfo(proxy), deadTTL(), reason)
	dropProxy(proxy)
}

// exit429Policy is the 429 rule: a streak in a row, or a cycle total, either
// retires the exit until the next refresh. the streak catches a saturated
// exit fast; the total catches one that limps along all cycle.
func exit429Policy() (streak, total int) {
	streak, total = cfg().Zen.Exit429Streak, cfg().Zen.Exit429Total
	if streak <= 0 && total <= 0 {
		return 0, 0 // both off: never trip
	}
	if streak <= 0 {
		streak = 1 << 30
	}
	if total <= 0 {
		total = 1 << 30
	}
	return streak, total
}

// noteExit429 records a 429 for one exit+model. the policy's streak in a row,
// or cycle total, retires that pair until the next pool refresh: the exit
// stays usable for every other model. returns true when the pair was pulled.
func noteExit429(proxy, model string) bool {
	if proxy == "" || model == "" {
		return false
	}
	st := exitStateFor(proxy)
	exitStateMu.Lock()
	if st.rl == nil {
		st.rl = map[string]exitMark429{}
	}
	mk := st.rl[model]
	mk.streak++
	mk.total++
	st.rl[model] = mk
	exitStateMu.Unlock()
	maxStreak, maxTotal := exit429Policy()
	if mk.streak < maxStreak && mk.total < maxTotal {
		return false
	}
	if exitRateLimitedModel(proxy, model) {
		return true // already pulled this cycle
	}
	reason := "429 x" + strconv.Itoa(maxStreak) + " in a row"
	if mk.streak < maxStreak {
		reason = strconv.Itoa(mk.total) + "+ 429s this cycle"
	}
	markExitRateLimited(proxy, model, reason)
	return true
}

// noteExitOK clears an exit+model's 429 streak on a success. the cycle total
// stays until the next refresh so a flapping pair still gets retired.
func noteExitOK(proxy, model string) {
	if proxy == "" {
		return
	}
	st := exitStateFor(proxy)
	exitStateMu.Lock()
	if mk, ok := st.rl[model]; ok {
		mk.streak = 0
		st.rl[model] = mk
	}
	st.lastOK = time.Now()
	exitStateMu.Unlock()
}

// noteExitSlow records a >6s first-byte stall on an exit. two strikes
// within an hour marks it dead: consistently slow exits waste every request
// they touch.
func noteExitSlow(proxy string) {
	if proxy == "" {
		return
	}
	st := exitStateFor(proxy)
	exitStateMu.Lock()
	now := time.Now()
	st.strikes = append(st.strikes, now)
	var kept []time.Time
	for _, t := range st.strikes {
		if now.Sub(t) < time.Hour {
			kept = append(kept, t)
		}
	}
	st.strikes = kept
	n := len(kept)
	exitStateMu.Unlock()
	if n >= 2 {
		markExitDead(proxy, "2 slow strikes in 1h")
	}
}

// clearExitRateLimits drops every rate-limit mark and resets the per-cycle
// counters. runs after a successful pool refresh: the swept pool gets a
// clean slate, so an exit that was merely busy this cycle is usable again.
// dead marks survive: a dead exit must verify again before it comes back.
func clearExitRateLimits() {
	exitStateMu.Lock()
	defer exitStateMu.Unlock()
	cleared := 0
	for _, st := range exitStates {
		if len(st.rl) > 0 {
			st.rl = nil
			cleared++
		}
		st.models = nil
	}
	if cleared > 0 {
		acclog.Printf("  zen proxies: %d rate-limited exit(s) cleared on refresh", cleared)
	}
}

// pruneExitStates forgets exits that are neither in the pool nor marked:
// the pool churns, and the registry would otherwise grow forever.
func pruneExitStates(keep map[string]bool) {
	exitStateMu.Lock()
	defer exitStateMu.Unlock()
	now := time.Now()
	for p, st := range exitStates {
		if keep[p] {
			continue
		}
		if exitRateLimitedLocked(st, now) || now.Before(st.deadUntil) {
			continue // still serving its mark: /status must keep showing it
		}
		delete(exitStates, p)
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
		return zenRetryableStatus(r.resp.StatusCode)
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

// laneCapFor is laneCap trimmed to the pool. one exit per lane means a table
// larger than the pool can never pin every lane, and the pinless leftovers
// are what hammered one exit into 429s. an empty pool keeps the configured
// cap (a cold start is still warming, not a verdict on the pool size).
func laneCapFor() int {
	c := laneCap()
	zenProxiesMu.RLock()
	n := len(zenProxies)
	zenProxiesMu.RUnlock()
	if n > 0 && n < c {
		return n
	}
	return c
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

// zenRetryableStatus reports whether an upstream status means "this exit
// could not serve the request", as opposed to a real answer from the model.
// every one of these is retried on another exit and never forwarded: 429/529
// are the per-exit rate limits, 502/503/504 are a dead exit or a proxy in
// front of it. owner: the retry ladders (handleOpenCode, handleOpenCodeAnthropic)
// and hedgedDo's winner rule.
func zenRetryableStatus(st int) bool {
	return st == 429 || st == 529 || st == 502 || st == 503 || st == 504
}

// zenMaxRetries caps zen upstream attempts per request (loop bound is
// attempts, log labels stay /N on retries). the default walks a full lane
// table: one attempt per lane, so a 429 on every exit still gets the request
// served from the lane that is merely busy, and the ceiling is reached only
// when the pool itself ran out.
func zenMaxRetries() int {
	if n := cfg().Zen.MaxRetries; n > 0 {
		return n + 1
	}
	if cfg().Zen.MaxRetries < 0 {
		return 1
	}
	return 16 // default: first try + 15 retries
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
		if len(lanes) < laneCapFor() {
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
		if len(lanes) < laneCapFor() {
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
	for len(lanes) < laneCapFor() {
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
// past stale_pool_secs trips the stale-pool refresh. the exit+model's own 429
// policy is noteExit429's job (two in a row, or six a cycle, retires the
// pair); rotation only moves this lane off it. returns the new session id.
func laneRotate(l *zenLane, model string) string {
	lanesMu.Lock()
	l.rateLimit++
	if l.limitedSince.IsZero() {
		l.limitedSince = time.Now()
	}
	l.session = zenSession()
	l.lastUsed = time.Now()
	l.consec429++
	lanesMu.Unlock()
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
	refreshZenProxiesAsync(false)
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

// maybeRefreshPoolThin refills the pool when it holds fewer exits than the
// lane table. every exit then ends up pinned, no lane can draw a unique
// one, and the pins that verify pile up instead of spreading. cooldown-gated
// like every other refresh trip, so a rotation storm cannot fire a verify
// sweep per retry.
func maybeRefreshPoolThin() {
	zenProxiesMu.RLock()
	pool := len(zenProxies)
	zenProxiesMu.RUnlock()
	if pool == 0 {
		return // cold start: watchZenProxies owns that sweep
	}
	lanesMu.Lock()
	n := len(lanes)
	lanesMu.Unlock()
	if pool >= n {
		return
	}
	if !poolRefreshDue() {
		zenProxiesMu.RLock()
		ago := time.Since(zenPoolRefreshed)
		zenProxiesMu.RUnlock()
		dbglog.Printf("  zen: pool %d < %d lanes, refresh skipped (last %v ago, cooldown %v)", pool, n, ago.Round(time.Second), poolRefreshCooldown())
		return
	}
	acclog.Printf("  zen: pool %d exits for %d lanes, refreshing proxy pool", pool, n)
	zenProxiesMu.Lock()
	zenPoolRefreshed = time.Now()
	zenProxiesMu.Unlock()
	refreshZenProxiesAsync(false)
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
		// one exit per lane: the observed exit is this lane's only while
		// another lane does not already hold it. a colliding stamp drops
		// the pin instead of letting two lanes ride one exit.
		held := false
		for _, o := range lanes {
			if o != l && o.proxy == proxy {
				held = true
				break
			}
		}
		if held {
			stampLaneProxy(l, "")
		} else {
			stampLaneProxy(l, proxy)
		}
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
// never returns a taken exit: two lanes on one exit share its per-exit
// limit, so the second lane burns it twice as fast and both get 429s. ""
// only when every exit is taken, the pool is empty, or the exits that are
// left are refusing this model outright (free-tier 403, dead).
func pickLaneProxyExcluding(taken map[string]bool, model string) string {
	if cps := getCustomProxies(); len(cps) > 0 {
		var free []string
		for _, c := range cps {
			if !taken[c] && exitUsableFor(c, model) {
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
	// on refresh, and verified-on-success keeps the set fresh. a taken
	// exit is never handed out: the caller rechecks the pool instead.
	var cands []string
	for _, p := range pool {
		if taken[p] || !exitUsableFor(p, model) {
			continue
		}
		cands = append(cands, p)
	}
	if len(cands) == 0 {
		// every exit is taken or marked for this model. lanes never share an
		// exit, so kick off a fresh sweep (exits other than the ones that
		// just 429'd) and hand out the least-recently-marked exit as a
		// second chance. clearing the marks here instead, as this used to,
		// put the exits that had just 429'd straight back at the front of
		// the rotation: the same few exits ping-ponged between limited and
		// usable and the pool never recovered.
		recheckAllExits()
		return secondChancePick(pool, taken, model)
	}
	sort.Slice(cands, func(i, j int) bool { return proxyWeight(cands[i]) > proxyWeight(cands[j]) })
	return sampleWeighted(cands, 1)[0]
}

// secondChancePick is the dry-pool pick: the exit whose mark for this model
// is oldest (unmarked first), never one another lane holds, dead, or refusing
// the model outright. it keeps the retry ladder moving through exits that
// already 429'd once without ever sharing an exit between lanes.
func secondChancePick(pool []string, taken map[string]bool, model string) string {
	best, bestAt := "", time.Time{}
	for _, p := range pool {
		if taken[p] || exitDead(p) || exitModelBurned(p, model) {
			continue
		}
		at := exitLimitMarkedAt(p, model) // zero when the pair is unmarked
		if best == "" || bestAt.IsZero() || (!at.IsZero() && at.Before(bestAt)) {
			best, bestAt = p, at
		}
	}
	return best
}

// exitLimitMarkedAt is when an exit's 429 mark for a model was placed.
// zero means the pair carries no live mark.
func exitLimitMarkedAt(proxy, model string) time.Time {
	if proxy == "" || model == "" {
		return time.Time{}
	}
	exitStateMu.Lock()
	defer exitStateMu.Unlock()
	st, ok := exitStates[proxy]
	if !ok {
		return time.Time{}
	}
	mk, ok := st.rl[model]
	if !ok || !mk.marked(time.Now()) {
		return time.Time{}
	}
	return mk.at
}

// recheckAllExits is the dry-pool recovery: every exit is taken or marked for
// this model, so no lane can draw a unique one and two lanes sharing an exit
// would burn its budget twice as fast. the recovery is a fresh sweep (exits
// other than the ones that just 429'd) whose completion clears the marks.
//
// the marks are deliberately not cleared here. clearing them on every dry
// pick put the exits that had just 429'd straight back into rotation, so the
// same few exits ping-ponged between limited and usable and the pool never
// recovered. dead exits stay out: a mitm exit fails every real send, so
// rechecking it only spends a request. the sweep is cooldown-gated like the
// other refresh trips, so a saturated pool cannot fire one per retry.
// returns whether the dry pick should re-filter the pool: only a sweep that
// actually started can refill it.
func recheckAllExits() bool {
	marked := countRateLimited()
	if marked == 0 {
		// nothing was written off: the pool is simply thinner than the lane
		// table, and only a refill (maybeRefreshPoolThin) can buy room.
		return false
	}
	if !poolRefreshDue() {
		zenProxiesMu.RLock()
		ago := time.Since(zenPoolRefreshed)
		zenProxiesMu.RUnlock()
		dbglog.Printf("  zen pool dry: %d marked exit(s), fresh sweep skipped (last %v ago, cooldown %v)", marked, ago.Round(time.Second), poolRefreshCooldown())
		return false
	}
	zenProxiesMu.Lock()
	zenPoolRefreshed = time.Now()
	zenProxiesMu.Unlock()
	acclog.Printf("  zen pool dry: %d marked exit(s), sweeping for fresh exits", marked)
	refreshZenProxiesAsync(true)
	return true
}

// pinUniqueProxy pins an exit no other lane holds under lanesMu. the
// picker's taken snapshot goes stale between concurrent rotations, so a
// colliding pick is retried with a fresh exclusion set a bounded number of
// times. "" means nothing unique to pin: the caller drops its pin rather
// than ride an exit another lane already holds.
func pinUniqueProxy(l *zenLane, model string) string {
	for i := 0; i < 10; i++ {
		// the picker may lock lanesMu internally (lane bookkeeping for the
		// dry-pool recheck), so it never runs under the write lock:
		// snapshot, pick, then re-check.
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
			// a pool narrower than the lane table cannot hand every lane
			// its own exit: refill after the picker so a thin-pool stamp
			// never starves recheckAllExits from clearing written-off exits.
			maybeRefreshPoolThin()
			return ""
		}
		lanesMu.Lock()
		conflict := false
		for _, o := range lanes {
			if o != l && o.proxy == p {
				conflict = true
				break
			}
		}
		if !conflict {
			stampLaneProxy(l, p)
			lanesMu.Unlock()
			maybeRefreshPoolThin()
			return p
		}
		lanesMu.Unlock() // stale snapshot: re-pick with fresh exclusions
	}
	return ""
}

// laneReproxy swaps a dead exit for a fresh verified one that is not
// burned for model, keeping the lane session pinned (affinity survives
// proxy churn) and every other lane's exit untouched: no exit is ever
// held by two lanes. with nothing unique to swap to the pin is kept only
// while it is still this lane's alone and the pool still holds it;
// otherwise it is dropped ("" = direct, or the caller hands the request
// to a lane that owns an exit).
func laneReproxy(l *zenLane, model string) string {
	if p := pinUniqueProxy(l, model); p != "" {
		return p
	}
	lanesMu.Lock()
	p := l.proxy
	lanesMu.Unlock()
	if exitUsableFor(p, model) && !exitOutOfPool(p) && !lanePinnedElsewhere(p, l) {
		return p
	}
	setLaneProxy(l, "")
	return ""
}

// exitOutOfPool reports whether a non-empty pool has thrown pin away. an
// empty pool is not a verdict: the verify sweep is mid-flight, and pins
// outlive a refresh, so nothing is withheld on that basis.
func exitOutOfPool(pin string) bool {
	if pin == "" {
		return false
	}
	zenProxiesMu.RLock()
	defer zenProxiesMu.RUnlock()
	if len(zenProxies) == 0 {
		return false
	}
	for _, p := range zenProxies {
		if p == pin {
			return false
		}
	}
	return true
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
// only the lane race consumes utilization, so skip the bookkeeping entirely
// when it is off (the default).
func noteLaneUse(opened bool) {
	if !cfg().Zen.LaneRaceEnabled {
		return
	}
	at := time.Now()
	useMu.Lock()
	useLog = append(useLog, laneUse{at: at, opened: opened})
	cut := at.Add(-laneUseWindow())
	i := 0
	for i < len(useLog) && useLog[i].at.Before(cut) {
		i++
	}
	if i > 0 {
		// reslice in place: copying the retained tail on every call made
		// this O(window) per request (O(n^2) over a burst).
		useLog = useLog[i:]
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
	refreshZenProxiesAsync(false)
	resetStaleLanes()
	return true
}

// staleResetMu + staleResetAt stagger resetStaleLanes: concurrent in-flight
// requests each trip the stale path during a sustained outage, and without a
// stamp each one re-mints every lane session, churning the affinity lanes
// exist to provide.
var (
	staleResetMu sync.Mutex
	staleResetAt time.Time
)

const staleResetCooldown = 5 * time.Second

// resetStaleLanes mints fresh sessions after a stale refresh, so retry
// loops re-enter on new sessions instead of dead ones. pins stay: an old
// exit keeps serving until a real send proves it dead, never cleared in
// bulk across every lane at once.
func resetStaleLanes() {
	staleResetMu.Lock()
	if time.Since(staleResetAt) < staleResetCooldown {
		staleResetMu.Unlock()
		return
	}
	staleResetAt = time.Now()
	staleResetMu.Unlock()

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
	// exit-state rows for exits that left the pool and carry no live mark
	// are dropped here: the pool churns and the registry only ever inserts.
	zenProxiesMu.RLock()
	keep := make(map[string]bool, len(zenProxies))
	for _, p := range zenProxies {
		keep[p] = true
	}
	zenProxiesMu.RUnlock()
	pruneExitStates(keep)
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
		// lane ids derive from client-supplied values (x-session-id,
		// prompt_cache_key): hash them on /status instead of echoing client
		// identity back.
		id := l.id
		switch {
		case strings.HasPrefix(id, "hdr:"):
			sum := sha256.Sum256([]byte(id))
			id = "hdr:" + hex.EncodeToString(sum[:])[:12]
		case strings.HasPrefix(id, "pck:"):
			sum := sha256.Sum256([]byte(id))
			id = "pck:" + hex.EncodeToString(sum[:])[:12]
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
// to another exit. lanes that already hold a pin win over pinless ones so
// a thin pool reaches its working exits in one hop instead of burning the
// retry budget walking unpinned lanes; within a tier, idle lanes win, then
// stalest. never nil: a one-lane pool reuses its lane without a cooldown.
func laneFailover(l *zenLane) *zenLane {
	lanesMu.Lock()
	defer lanesMu.Unlock()
	now := time.Now()
	better := func(c, cur *zenLane) bool {
		if cur == nil {
			return true
		}
		cp, bp := c.proxy != "", cur.proxy != ""
		if cp != bp {
			return cp
		}
		if c.inflight != cur.inflight {
			return c.inflight < cur.inflight
		}
		return c.lastUsed.Before(cur.lastUsed)
	}
	var best *zenLane
	for _, c := range lanes {
		if c == l {
			continue
		}
		if better(c, best) {
			best = c
		}
	}
	if best != nil {
		// stamp the hand-out: without it concurrent retries converge on
		// the same stalest lane.
		best.lastUsed = now
		return best
	}
	if len(lanes) < laneCapFor() {
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
// a pin is only this lane's while no other lane holds it: a shared exit
// burns its per-exit limit once per holder, so the crowded lane re-picks
// (or drops to "" when nothing unique is left).
func laneProxyFor(l *zenLane, model string) string {
	lanesMu.Lock()
	p := l.proxy
	escalated := l.escalated
	lanesMu.Unlock()
	if exitUsableFor(p, model) && !lanePinnedElsewhere(p, l) {
		return p
	}
	if !cfg().Zen.AlwaysProxy && !escalated {
		return ""
	}
	return laneReproxy(l, model)
}

// lanePinnedElsewhere reports whether another lane holds pin. one exit per
// lane, always: two lanes on one exit split its per-exit budget and both
// get limited sooner, and a whole table on one ip is the pile-up this
// exists to prevent. a lane's pin is its own only while it is the only
// holder.
func lanePinnedElsewhere(pin string, self *zenLane) bool {
	if pin == "" {
		return false
	}
	lanesMu.Lock()
	defer lanesMu.Unlock()
	for _, o := range lanes {
		if o != self && o.proxy == pin {
			return true
		}
	}
	return false
}
