package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
// the table is trimmed to the pool afterwards: a lane that could not draw an
// exit is a lane that cannot send at all.
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
	trimLanesToPool()
}

// trimLanesToPool drops lanes that hold no exit once the table outgrows the
// exits a lane can actually hold. one exit per lane means a table larger than
// that can never pin every lane, and a pinless lane is one that cannot send
// (always_proxy) or sits out the whole pool-recovery wait for an exit no other
// lane will give up. the surplus goes idlest first, pinless only, so a serving
// lane is never touched; the next request opens a fresh lane under the cap
// instead.
func trimLanesToPool() {
	lanesMu.Lock()
	defer lanesMu.Unlock()
	trimLanesToPoolLocked()
}

// trimLanesToPoolLocked is trimLanesToPool with lanesMu already held.
func trimLanesToPoolLocked() {
	// fast path: a table where every idle lane holds an exit has nothing to
	// drop, and the check costs no locks. this runs at request completion.
	surplus := false
	for _, l := range lanes {
		if l.proxy == "" && l.inflight == 0 {
			surplus = true
			break
		}
	}
	if !surplus {
		return
	}
	zenProxiesMu.RLock()
	pool := len(zenProxies)
	zenProxiesMu.RUnlock()
	if pool == 0 {
		return // cold or mid-sweep: an empty pool is not a verdict on the table
	}
	limit := laneCapFor()
	if len(lanes) <= limit {
		return
	}
	type spare struct {
		key  string
		lane *zenLane
	}
	var victims []spare
	for k, l := range lanes {
		// inflight lanes keep their seat: a request is still walking this
		// lane through the pool wait, and dropping it would hide the lane
		// from the taken set while its send is live.
		if l.proxy == "" && l.inflight == 0 {
			victims = append(victims, spare{k, l})
		}
	}
	if len(victims) == 0 {
		return
	}
	sort.Slice(victims, func(i, j int) bool { return victims[i].lane.lastUsed.Before(victims[j].lane.lastUsed) })
	for _, v := range victims {
		if len(lanes) <= limit {
			break
		}
		delete(lanes, v.key)
		dropConvoSeen(v.key)
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

func countGateParked() int {
	exitStateMu.Lock()
	defer exitStateMu.Unlock()
	now := time.Now()
	n := 0
	for _, st := range exitStates {
		if now.Before(st.gateUntil) {
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
	rl         map[string]exitMark429
	deadUntil  time.Time            // dead until
	gateUntil  time.Time            // free-tier IP/daily cap cooldown (per exit IP)
	lastSend   time.Time            // per-exit pacing clock (min 1.5s between sends on this exit)
	consecFail int                  // consecutive transport failures since last success
	reason     string               // last mark, for /status and logs
	models     map[string]time.Time // model -> refusing it until (403 free-tier)
	lastOK     time.Time
	strikes    []time.Time // >15s first-byte stalls in the last hour
	// first-answer ledger: the first reply this process ever got from the
	// exit and the retry-after it carried. a 429 there is the free tier
	// refusing an ip this process never spent anything on. it does not say
	// the exit was already used: every capped answer counts down to utc
	// midnight (22079s at 17:51 utc, measured 2026-11-03), so the header is
	// the daily deadline, never a measure of this exit's own window.
	firstAt    time.Time
	firstState int
	retryAfter int
	spentUntil time.Time
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
	// cap marks an until that came from the 429's own retry-after rather than
	// the burst policy. a sweep clears burst marks to give the next cycle a
	// clean slate; a cap mark is upstream stating when it will serve again, so
	// it survives the sweep and only its own deadline (or utc midnight) lifts it.
	cap bool
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
	return proxy != "" && !exitDead(proxy) && !exitGateCooling(proxy) && !exitRateLimited(proxy)
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
// cooling off the free-tier gate, not marked for that model, not refusing it
// this cycle, and not geo-gated for that model's region. a mark for a
// different model never blocks this one.
func exitUsableFor(proxy, model string) bool {
	return proxy != "" && !exitDead(proxy) && !exitGateCooling(proxy) &&
		!exitRateLimitedModel(proxy, model) && !exitModelBurned(proxy, model) &&
		!zenRegionGated(proxy, model)
}

// zenRegionGates caches zen's 403 RegionError per model+country. some free ids
// are geo-gated per region independently of quota, so an exit in a gated place
// still serves every model the region is allowed — keyed by country rather than
// by exit, one refusal spares a hop on every other exit in the same region
// (measured 2026-11-03: 119 exits refused beam-free for one country while exits
// elsewhere served it).
var (
	zenRegionMu    sync.Mutex
	zenRegionGates = map[string]time.Time{}
)

func zenRegionKey(country, model string) string { return country + "\x00" + model }

// noteZenRegionBlock records that model is geo-gated for this exit's country
// until the next pool refresh, and returns the country it filed under.
func noteZenRegionBlock(proxy, model string) string {
	country := proxyCountry(proxy)
	if model == "" || country == "" || country == "??" || country == "direct" {
		return country
	}
	k := zenRegionKey(country, model)
	zenRegionMu.Lock()
	_, had := zenRegionGates[k]
	zenRegionGates[k] = time.Now().Add(poolInterval())
	zenRegionMu.Unlock()
	if !had {
		acclog.Printf("  zen region gate: %s not available in %s, other regions keep the model", model, country)
	}
	return country
}

// zenRegionGated reports whether model is known geo-gated for this exit's
// country. an unknown country is never gated: ?? is missing data, not a
// region, and treating it as one would write off the whole pool.
func zenRegionGated(proxy, model string) bool {
	if proxy == "" || model == "" {
		return false
	}
	k := zenRegionKey(proxyCountry(proxy), model)
	zenRegionMu.Lock()
	defer zenRegionMu.Unlock()
	until, ok := zenRegionGates[k]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(zenRegionGates, k)
		return false
	}
	return true
}

// clearZenRegionGates forgets the geo-gate cache on a pool refresh and at utc
// midnight: a gate is a region's current catalog, not a permanent verdict.
func clearZenRegionGates() {
	zenRegionMu.Lock()
	n := len(zenRegionGates)
	zenRegionGates = map[string]time.Time{}
	zenRegionMu.Unlock()
	if n > 0 {
		acclog.Printf("  zen region gates: %d model/region pair(s) re-checked", n)
	}
}

// exitGateCooling reports whether the exit IP is parked after hitting its
// free-tier daily cap or an IP-abuse 403.
func exitGateCooling(proxy string) bool {
	if proxy == "" {
		return false
	}
	exitStateMu.Lock()
	defer exitStateMu.Unlock()
	st, ok := exitStates[proxy]
	if !ok || st.gateUntil.IsZero() {
		return false
	}
	if time.Now().After(st.gateUntil) {
		st.gateUntil = time.Time{}
		return false
	}
	return true
}

// poolAllGateParked reports whether the pool holds exits but every one is
// parked by the free-tier gate (or dead). waiting for a drawable exit cannot
// help then — only a fresh sweep can, and its exits show up as a new pool —
// so a lane answers immediately instead of holding the client for the wait.
func poolAllGateParked() bool {
	zenProxiesMu.RLock()
	pool := append([]string(nil), zenProxies...)
	zenProxiesMu.RUnlock()
	if len(pool) == 0 {
		return false
	}
	for _, p := range pool {
		if !exitGateCooling(p) && !exitDead(p) {
			return false
		}
	}
	return true
}

// poolUnusable reports whether the pool can serve nobody right now: every
// member dead, gate-parked, or rate-limited for every model. an empty pool
// counts too: a sweep that keeps finding nothing leaves the marks behind, and
// those marks are what stop it finding anything next time.
func poolUnusable() bool {
	zenProxiesMu.RLock()
	pool := append([]string(nil), zenProxies...)
	zenProxiesMu.RUnlock()
	if len(pool) == 0 {
		return true
	}
	for _, p := range pool {
		if exitUsable(p) {
			return false
		}
	}
	return true
}

// poolEmpty reports whether the verified pool holds no exit at all. a draw
// cannot help and a refill is the whole answer, so a lane must not sit out the
// recovery wait on it (measured 2026-11-01: 30s to a 503 with an empty pool).
func poolEmpty() bool {
	zenProxiesMu.RLock()
	defer zenProxiesMu.RUnlock()
	return len(zenProxies) == 0
}

// gatePoolRefill asks for a full recheck when the pool has nothing left but
// parked exits. a capped IP never comes back inside its window, so re-probing
// the live set buys nothing; only the rest of the list can help. paced by
// dryRefillCooldown so waiting lanes see a fresh sweep within seconds.
func gatePoolRefill() {
	if zenTierDown() {
		return // sweeps are held: the last one found no exit with budget
	}
	dryRefillMu.Lock()
	due := time.Since(dryRefillAt) >= dryRefillCooldown
	if due {
		dryRefillAt = time.Now()
	}
	dryRefillMu.Unlock()
	if !due {
		return
	}
	zenProxiesMu.Lock()
	zenPoolRefreshed = time.Now()
	zenProxiesMu.Unlock()
	recheckEveryExit("every exit free-tier parked")
}

// noteExitGate parks an exit after a free-tier 403 (daily cap or IP-abuse
// block) and evacuates any lane pinned to it onto a fresh exit. the block is
// per exit IP, not per model, so the whole exit sits out for the cycle while
// other lanes keep serving on their own pins.
func noteExitGate(proxy string) {
	if proxy == "" {
		return
	}
	park := deadTTL()
	st := exitStateFor(proxy)
	exitStateMu.Lock()
	was := !st.gateUntil.IsZero() && time.Now().Before(st.gateUntil)
	st.gateUntil = time.Now().Add(park)
	st.reason = "free-tier 403 (ip cap/block)"
	exitStateMu.Unlock()
	if !was {
		acclog.Printf("  zen exit %s parked for %v (free-tier 403)", redactProxyUserinfo(proxy), park)
	}
	dropProxy(proxy)
}

// noteExitTierCap parks an exit whose 429 carried zen's free-tier answer
// with a long retry-after. measured 2026-10-26 over ~180 live exits: that
// cap answers every free model on the ip and outlives the 30m pool refresh,
// so it is the exit's own state, not the one model noteExit429 marks. measured
// 2026-11-02 that is too strong: an exit serves about one free id and answers
// this cap to the others, so the callers park only an exit whose probe found no
// serving bucket (exitBucketKnown) and leave the rest to the pair mark.
// leaving
// such an exit in the pool made the pool lie ("56 verified" naming ips that
// answered nothing) and every hop spend itself on it: a request that should
// fail fast sat 85s before the 503. parking drops it like a 403 gate park, so
// the ladder still rotates but never draws it again until the burn expires.
// the retry-after on that answer is not a window: it is the countdown to the
// next utc midnight from every exit (22079s at 17:51 utc, measured 2026-11-03),
// and the same exit answered 429, 429, 200 seconds apart, so it is a deadline
// display, never a measurement of what this exit has left.
// retry-after ledger counters, one per fresh exit answer.
var (
	zenFreshSeen   atomic.Int64
	zenFreshSpent  atomic.Int64
	zenFreshServed atomic.Int64
)

// noteExitFirst records the first answer an exit gave this process. a fresh
// exit answered 429 before this process spent anything on it; the retry-after
// riding that answer is the countdown to utc midnight, not a measure of the
// exit's window, so the ledger reports the status and leaves the reason to the
// sweep.
func noteExitFirst(proxy string, status, retryAfter int) {
	if proxy == "" {
		return
	}
	st := exitStateFor(proxy)
	exitStateMu.Lock()
	first := st.firstState == 0
	if first {
		st.firstState = status
		st.retryAfter = retryAfter
	}
	exitStateMu.Unlock()
	if !first {
		return
	}
	zenFreshSeen.Add(1)
	switch status {
	case http.StatusOK:
		zenFreshServed.Add(1)
		acclog.Printf("  zen ledger: %s first answer 200 (fresh, served)", redactProxyUserinfo(proxy))
	case http.StatusTooManyRequests, 529:
		zenFreshSpent.Add(1)
		acclog.Printf("  zen ledger: %s first answer %d retry-after=%ds (refused before we spent anything)", redactProxyUserinfo(proxy), status, retryAfter)
	}
}

// zenLedgerLine is the ledger aggregate for logs.
func zenLedgerLine() string {
	return fmt.Sprintf("fresh=%d served=%d refused-on-first=%d",
		zenFreshSeen.Load(), zenFreshServed.Load(), zenFreshSpent.Load())
}

// noteExitTierCap records zen's free-tier 429 for one exit+model.
//
// it never parks the exit: the cap is per ip x model (measured 2026-11-03, one
// exit answered it for glm and deepseek while serving nemotron and beam-free in
// the same minute) and its retry-after counts down to utc midnight from every
// exit (22079s at 17:51 utc), so it measures nothing about this ip. noteExit429
// already marked the pair, the picker skips that pair, and every other model
// keeps drawing the exit — parking here locked models out of an ip that still
// had budget for them.
func noteExitTierCap(proxy, model string, retryAfter int) {
	if proxy == "" {
		return
	}
	st := exitStateFor(proxy)
	exitStateMu.Lock()
	st.retryAfter = retryAfter
	st.reason = fmt.Sprintf("free-tier 429 (usage cap, retry-after %ds) on %s", retryAfter, model)
	exitStateMu.Unlock()
	acclog.Printf("  zen exit %s capped on %s (retry-after %ds = seconds to utc midnight), pair mark only",
		redactProxyUserinfo(proxy), model, retryAfter)
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
	if exitGateCooling(proxy) {
		return "gate_blocked"
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

// rateLimitCooldown is how long a retired exit sits out before the pool may
// hand it out again. a 429 is a burst, not a verdict: the mark lapses on its
// own so a busy exit comes back into rotation without waiting for the next
// refresh. negative (or a refresh cycle shorter than the cooldown) keeps the
// old behaviour of holding the mark until the sweep clears it.
func rateLimitCooldown() time.Duration {
	s := cfg().Zen.Exit429CoolSecs
	if s == 0 {
		return 5 * time.Minute
	}
	if s < 0 {
		return poolInterval()
	}
	d := time.Duration(s) * time.Second
	if d > poolInterval() {
		return poolInterval()
	}
	return d
}

// markExitRateLimited pulls one exit+model pair out of rotation for one
// cooldown: log it, mark it, and leave every other model on that exit
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
	mk.until = mk.at.Add(rateLimitCooldown())
	st.rl[model] = mk
	st.reason = reason + " on " + model
	exitStateMu.Unlock()
	acclog.Printf("  zen exit %s rate-limited for %s for %v (%s: %d in a row, %d this cycle)",
		redactProxyUserinfo(proxy), model, rateLimitCooldown(), reason, mk.streak, mk.total)
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
// or cycle total, retires that pair for one cooldown: the exit stays usable
// for every other model, and comes back when the mark lapses. returns true
// when the pair was pulled.
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

// noteExitTransportErr records a transport failure on an exit. an exit that
// has already served requests gets 2 strikes before being marked dead for 30m
// (stale keep-alive or transient proxy hiccup shouldn't throw away a working
// exit with ~50-100 daily requests left); an unproven exit dies on first fail.
// returns true when the exit was marked dead and dropped.
func noteExitTransportErr(proxy string) bool {
	if proxy == "" {
		return false
	}
	st := exitStateFor(proxy)
	exitStateMu.Lock()
	st.consecFail++
	fails := st.consecFail
	proven := !st.lastOK.IsZero()
	exitStateMu.Unlock()
	if proven && fails < 2 {
		closeZenTransport(proxy) // drop stale keep-alive conn, keep the exit
		acclog.Printf("  zen exit %s transient transport error (%d/2), keeping exit", redactProxyUserinfo(proxy), fails)
		return false
	}
	markExitDead(proxy, "transport closed")
	return true
}

// noteExit429Cap records a 429's own retry-after for one exit+model. the header
// is the upstream saying when this pair will serve again — measured 2026-11-02
// it is seconds to the next utc midnight from every exit — so the pair sits out
// exactly that window instead of the generic burst cooldown, and the sweep no
// longer hands it back out mid-window. capped at a day so a bogus header cannot
// retire an exit forever.
func noteExit429Cap(proxy, model string, secs int) {
	if proxy == "" || model == "" || secs <= 0 {
		return
	}
	d := time.Duration(secs) * time.Second
	if d > 24*time.Hour {
		d = 24 * time.Hour
	}
	st := exitStateFor(proxy)
	now := time.Now()
	exitStateMu.Lock()
	if st.rl == nil {
		st.rl = map[string]exitMark429{}
	}
	mk := st.rl[model]
	mk.streak++
	mk.total++
	mk.at = now
	mk.until = now.Add(d)
	mk.cap = true
	st.rl[model] = mk
	st.reason = "429 cap " + strconv.Itoa(secs) + "s on " + model
	exitStateMu.Unlock()
	acclog.Printf("  zen exit %s capped on %s for %v (retry-after %ds)", redactProxyUserinfo(proxy), model, d.Round(time.Second), secs)
}

// noteExitOK clears an exit+model's 429 streak on a success. the cycle total
// stays until the next refresh so a flapping pair still gets retired.
// the exit stays pinned to its lane and keeps serving (~50-100 requests per IP
// with ~1.5s pacing) until it actually hits a 429 or daily cap 403.
func noteExitOK(proxy, model string) {
	if proxy == "" {
		return
	}
	// a real completion is the only evidence of which bucket an exit serves:
	// admission asks /models and nothing else, so this is where capacity is
	// learned. the picker uses it to skip exits known to serve another model.
	noteExitBucket(proxy, model)
	st := exitStateFor(proxy)
	exitStateMu.Lock()
	if mk, ok := st.rl[model]; ok {
		mk.streak = 0
		st.rl[model] = mk
	}
	st.consecFail = 0
	st.lastOK = time.Now()
	exitStateMu.Unlock()
}

// noteExitSlow records a >15s first-byte stall on an exit. three strikes
// within an hour marks it dead: consistently stalled exits waste requests,
// while large thinking-model prompts (>6s normal TTFT) do not false-trip.
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
	if n >= 3 {
		markExitDead(proxy, "3 slow strikes in 1h")
	}
}

// clearAllExitMarks drops rate limits, per-model burns, and dead stamps for
// the out-of-exits recheck. gateUntil (per-IP free-tier daily cap/block) is
// preserved: an IP that hit its daily cap is still capped, so clearing it only
// re-admits burned exits that immediately 403 again.
func clearAllExitMarks() {
	exitStateMu.Lock()
	cleared := 0
	for _, st := range exitStates {
		if len(st.rl) > 0 || len(st.models) > 0 || !st.deadUntil.IsZero() {
			cleared++
		}
		st.rl = nil
		st.models = nil
		st.consecFail = 0
		st.deadUntil = time.Time{}
	}
	exitStateMu.Unlock()
	if cleared > 0 {
		acclog.Printf("  zen proxies: %d marked exit(s) cleared for a full recheck", cleared)
	}
}

// clearExitRateLimits drops every rate-limit mark and resets the per-cycle
// counters. runs after a successful pool refresh: the swept pool gets a
// clean slate, so an exit that was merely busy this cycle is usable again.
// dead marks survive: a dead exit must verify again before it comes back.
// retry-after marks survive too, for as long as the upstream said: clearing
// them on the 30m tick is what put an exit capped-until-midnight straight back
// in the rotation.
func clearExitRateLimits() {
	exitStateMu.Lock()
	defer exitStateMu.Unlock()
	now := time.Now()
	cleared := 0
	for _, st := range exitStates {
		if len(st.rl) == 0 {
			st.models = nil
			continue
		}
		kept := 0
		for model, mk := range st.rl {
			if mk.cap && !now.After(mk.until) {
				kept++
				continue
			}
			delete(st.rl, model)
		}
		if kept == 0 {
			st.rl = nil
			cleared++
		}
		st.models = nil
	}
	if cleared > 0 {
		acclog.Printf("  zen proxies: %d rate-limited exit(s) cleared on refresh", cleared)
	}
	clearZenRegionGates()
}

// untilNextUTCMidnight is the wait until zen's daily allowance resets. every
// free-tier retry-after counts down to this instant (measured 2026-11-02:
// now+retry-after == next 00:00 utc to the second from every exit), so it is
// the one time the pool is known to come back.
func untilNextUTCMidnight() time.Duration {
	now := time.Now().UTC()
	next := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
	return time.Until(next)
}

// clearDailyExitMarks releases every mark that tracks zen's daily meter: the
// retry-after caps, the per-model 429 marks, the per-model 403 burns and the
// free-tier gate parks. all of them expire at utc midnight and nothing else in
// the tree clears a gate park, so before this a capped exit sat out the whole
// next day too. dead stamps stay: a tunnel that died is not a daily meter.
func clearDailyExitMarks() {
	exitStateMu.Lock()
	n := 0
	for _, st := range exitStates {
		if len(st.rl) > 0 || len(st.models) > 0 || !st.gateUntil.IsZero() {
			n++
		}
		st.rl = nil
		st.models = nil
		st.gateUntil = time.Time{}
	}
	exitStateMu.Unlock()
	if n > 0 {
		acclog.Printf("  zen proxies: daily marks cleared at utc midnight (%d exit(s))", n)
	}
	clearZenRegionGates()
}

// watchZenMidnight runs the midnight release: drop the daily marks and then
// sweep, so the first request after the reset draws exits that just regained
// budget instead of the parked set from yesterday.
func watchZenMidnight() {
	for {
		// a sweep started a hair before the tick lands inside the old day and
		// re-marks everything it finds, so wait out the boundary first.
		time.Sleep(untilNextUTCMidnight() + 5*time.Second)
		if !zenEnabled() {
			continue
		}
		clearDailyExitMarks()
		refreshZenProxiesAsync(true)
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
		if exitRateLimitedLocked(st, now) || now.Before(st.deadUntil) || now.Before(st.gateUntil) {
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

// laneCapFor is laneCap trimmed to the exits a lane can actually hold. one
// exit per lane means a table larger than the pool can never pin every lane,
// and the pinless leftovers are what hammered one exit into 429s. an empty
// pool keeps the configured cap (a cold start is still warming, not a verdict
// on the pool size).
func laneCapFor() int {
	c := laneCap()
	if n := usableExitCount(); n > 0 && n < c {
		return n
	}
	return c
}

// usableExitCount is the pool less the exits no lane can hold right now: dead
// ones and gate-parked ones. per-model marks do not count (the picker still
// hands those out, they only order the pool), so a busy model never shrinks
// the table. reading the pool count alone overstated it: a sweep verifies an
// exit, the exit dies on its first real send, and every lane created against
// it ends up pinless. 0 means an empty pool, not a verdict.
func usableExitCount() int {
	zenProxiesMu.RLock()
	pool := append([]string(nil), zenProxies...)
	zenProxiesMu.RUnlock()
	n := 0
	for _, p := range pool {
		if exitDead(p) || exitGateCooling(p) {
			continue
		}
		n++
	}
	return n
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
	if ms := cfg().Zen.LaneMinGapMs; ms > 0 {
		if ms < 1500 {
			ms = 1500
		}
		return time.Duration(ms) * time.Millisecond
	}
	return 1500 * time.Millisecond
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

// zenLastRetry reports whether retry index is the final attempt of a ladder
// whose bound is attempts. the bound is not always zenMaxRetries(): a hop that
// finds the pool written off can buy extra hops (ladderExtend), so each ladder
// passes its own bound.
func zenLastRetry(retry, attempts int) bool { return retry+1 >= attempts }

// exitGate blocks until the proxy exit itself has had at least laneMinGap()
// (1.5s minimum) since its last send, and advances the lane's send clock too.
// this guarantees that even across lane rotations, failovers, nudges, or
// re-pins, a single exit IP never sends faster than once per 1.5s.
func exitGate(ctx context.Context, l *zenLane, proxy string) time.Duration {
	gap := laneMinGap()
	if gap <= 0 {
		return 0
	}
	maxWait := 30 * time.Second
	if s := cfg().Zen.LaneGateMaxSecs; s > 0 {
		maxWait = time.Duration(s) * time.Second
	}
	now := time.Now()
	var st *exitState
	if proxy != "" {
		st = exitStateFor(proxy)
	}
	// read, compute and claim the slot under both locks: reading the
	// clocks in one critical section and writing them in another let two
	// concurrent callers claim the same instant and burst the exit.
	// lanesMu is always taken before exitStateMu (nothing else nests them).
	if l != nil {
		lanesMu.Lock()
	}
	if st != nil {
		exitStateMu.Lock()
	}
	var wait time.Duration
	if l != nil && !l.lastSend.IsZero() {
		if w := gap - now.Sub(l.lastSend); w > 0 {
			wait = w
		}
	}
	if st != nil && !st.lastSend.IsZero() {
		if w := gap - now.Sub(st.lastSend); w > 0 && w > wait {
			wait = w
		}
	}
	if wait > maxWait {
		wait = maxWait
	}
	sched := now.Add(wait)
	if l != nil && sched.After(l.lastSend) {
		l.lastSend = sched
	}
	if st != nil && sched.After(st.lastSend) {
		st.lastSend = sched
	}
	if st != nil {
		exitStateMu.Unlock()
	}
	if l != nil {
		lanesMu.Unlock()
	}
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
			pinLocked(l, poolSnapshotForPins())
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
			pinLocked(l, poolSnapshotForPins())
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
	// the converted lane takes the victim's seat; pin it here so the swap does
	// not hand the request a lane without an exit.
	pinLocked(l, poolSnapshotForPins())
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
// pick and hand out one per request before any lane is reused. each one is
// pinned as it opens: a lane the top-up leaves without an exit is a lane
// that may never be used and never be repaired, which is how the table
// filled with pinless leftovers while free exits sat in the pool.
func ensureSharedLanesLocked() {
	if len(lanes) >= laneCapFor() {
		return
	}
	pool := poolSnapshotForPins()
	for len(lanes) < laneCapFor() {
		l := &zenLane{id: "shared", session: zenSession(), lastUsed: time.Now()}
		k := "shared:" + l.session[len(l.session)-6:]
		l.id = shortLaneID(k)
		lanes[k] = l
		pinLocked(l, pool)
	}
}

// poolSnapshotForPins copies the pin source for a caller that holds lanesMu
// and cannot take the picker path.
func poolSnapshotForPins() []string {
	if cps := getCustomProxies(); len(cps) > 0 {
		return append([]string(nil), cps...)
	}
	zenProxiesMu.RLock()
	defer zenProxiesMu.RUnlock()
	return append([]string(nil), zenProxies...)
}

// pinLocked stamps a freshly opened lane with an exit no other lane holds.
// caller holds lanesMu: no picker, no reload, just a snapshot pick. dead,
// gate-parked and rate-limited exits stay out; when no spare is left the lane
// opens pinless and the dry-pool path hands it one on its first send.
func pinLocked(l *zenLane, pool []string) {
	taken := map[string]bool{}
	for _, o := range lanes {
		if o != l && o.proxy != "" {
			taken[o.proxy] = true
		}
	}
	var cands []string
	for _, p := range pool {
		if taken[p] || !exitUsable(p) {
			continue
		}
		cands = append(cands, p)
	}
	if len(cands) == 0 {
		return
	}
	sort.Slice(cands, func(i, j int) bool { return proxyWeight(cands[i]) > proxyWeight(cands[j]) })
	stampLaneProxy(l, sampleWeighted(cands, 1)[0])
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
	// laneCapFor, not laneCap: a table larger than the pool can never pin
	// every lane. this branch is how it grew past the pool before: a full
	// table of hdr/pck lanes has no shared lane to convert, so every new
	// request appended another shared lane that no exit could reach.
	if len(lanes) < laneCapFor() {
		l := &zenLane{id: "shared", session: zenSession(), lastUsed: now}
		k := "shared:" + l.session[len(l.session)-6:]
		l.id = shortLaneID(k)
		lanes[k] = l
		pinLocked(l, poolSnapshotForPins())
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
		pinLocked(l, poolSnapshotForPins())
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

// laneNewSession re-mints a lane's session id without moving its pin: a
// transient 429 wants a fresh session on the same exit, not a new exit.
func laneNewSession(l *zenLane) string {
	lanesMu.Lock()
	defer lanesMu.Unlock()
	l.session = zenSession()
	return l.session
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

// poolRecoverWait is the whole-request budget for waiting on pool recovery.
// every lane holds a working exit, so when one cannot be drawn the only honest
// move is to wait for the sweep, not to hand the client a proxyless request.
// the clock starts at the first wait, covers every later one (a dry pick and a
// spent ladder draw from the same budget), and a live sweep publishes exits as
// they verify, so 30s gives the refill sweep plenty of time to land before the
// ladder fails honestly.
func poolRecoverWait() time.Duration { return 30 * time.Second }

// dryRefillCooldown paces the dry-pool refill. the 429/geo trips sit behind
// poolRefreshCooldown (~120s) because the pool is still serving when they fire;
// a dry pick is the pool serving nobody, so the gap here is short enough that a
// waiting lane sees the next sweep land in seconds.
const dryRefillCooldown = 10 * time.Second

var (
	dryRefillMu sync.Mutex
	dryRefillAt time.Time
)

// dryPoolRefill sweeps for exits when the pool hands none out: thin pool, every
// exit taken by a lane, or every exit written off for this model. single-flight
// (refreshZenProxiesAsync) plus dryRefillCooldown keep a burst of dry requests
// from stacking sweeps. fresh: the exits in the pool are the ones that just
// failed to serve, so re-verifying them first buys nothing. force skips the
// pacing, for a caller that is already waiting on the sweep to land.
func dryPoolRefill(force bool) {
	if !force && zenTierDown() {
		return // sweeps are held: the last one found no exit with budget
	}
	dryRefillMu.Lock()
	due := force || time.Since(dryRefillAt) >= dryRefillCooldown
	if due {
		dryRefillAt = time.Now()
	}
	dryRefillMu.Unlock()
	if !due {
		return
	}
	if poolUnusable() {
		recheckEveryExit("no exit can serve")
		return
	}
	acclog.Printf("  zen pool dry: no exit drawable, sweeping for fresh exits")
	refreshZenProxiesAsync(true)
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

// thinPoolLoggedAt and dryPoolLoggedAt pace the cooldown-skip notices below.
var thinPoolLoggedAt, dryPoolLoggedAt atomic.Int64

// skipLogOnce reports whether a skip notice may print now: once per window.
// both callers sit on the pin path, which runs once per retry hop, so a thin
// pool printed the same line dozens of times a second — 292k of the 308k lines
// in one day's debug log. one line per window says the same thing.
func skipLogOnce(at *atomic.Int64, window time.Duration) bool {
	if window <= 0 {
		return true // cooldown disabled: nothing is being skipped, nothing to pace
	}
	now := time.Now().UnixNano()
	last := at.Load()
	if last != 0 && now-last < int64(window) {
		return false
	}
	return at.CompareAndSwap(last, now)
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
		if skipLogOnce(&thinPoolLoggedAt, poolRefreshCooldown()) {
			dbglog.Printf("  zen: pool %d < %d lanes, refresh skipped (last %v ago, cooldown %v)", pool, n, ago.Round(time.Second), poolRefreshCooldown())
		}
		return
	}
	acclog.Printf("  zen: pool %d exits for %d lanes, refreshing proxy pool", pool, n)
	zenProxiesMu.Lock()
	zenPoolRefreshed = time.Now()
	zenProxiesMu.Unlock()
	refreshZenProxiesAsync(false)
}

// marksOut reports whether the ladder spent its attempts on exits that were
// already written off rather than on clean ones. most of the pool carrying a
// mark (or a dead stamp) means the picker had nothing healthy left to hand out
// and the write-offs are what bounds the request, so clearing them is the fix.
// a pool that is mostly clean means the ladder rode fresh exits that all
// answered 429: the model is saturated right now and a sweep buys nothing but
// latency, so the ladder should fail honestly instead of waiting.
func marksOut() bool {
	off := countRateLimited() + countDeadExits() + countGateParked()
	if off == 0 {
		return false
	}
	zenProxiesMu.RLock()
	pool := len(zenProxies)
	zenProxiesMu.RUnlock()
	// custom proxies count: with a configured list the picker only hands
	// those out, so a pool that is empty behind them is not the serving set.
	total := pool + len(getCustomProxies())
	return total == 0 || off*2 >= total
}

// oneExitPool reports whether at most one exit is left to rotate onto, so a
// spent ladder has nowhere to go: extending it only re-sends to the exit that
// already answered, and the sweep it would wait on has nothing to add.
func oneExitPool() bool {
	zenProxiesMu.RLock()
	n := len(zenProxies) + len(getCustomProxies())
	zenProxiesMu.RUnlock()
	return n-countDeadExits()-countGateParked() <= 1
}

// recoverDeadline returns this request's pool-recovery deadline, starting the
// clock on the first wait. every wait of one request (a dry pick, a spent
// ladder) draws on the same budget, so a request never stacks waits into a
// stall: it waits at most poolRecoverWait once, then fails honestly.
func recoverDeadline(until *time.Time) time.Time {
	if until.IsZero() {
		*until = time.Now().Add(poolRecoverWait())
	}
	return *until
}

// ladderExtend is the last hop's ask for a pool that is written off, never for
// one that is merely busy: a completed sweep clears every 429 mark (and
// replaces dead exits), so the extra hops draw clean exits instead of re-rolling
// the set the ladder just spent itself on. fires the sweep, then waits for the
// marks to go, bounded by the request's recovery budget. false means "spend no
// more hops": nothing written off, budget gone, or the client left.
func ladderExtend(ctx context.Context, until *time.Time) bool {
	if !marksOut() && !poolUnusable() {
		return false
	}
	// nothing to rotate onto: extra hops re-send to the exit that just
	// answered. measured 2026-11-02, a pool holding one verified exit whose
	// bucket was capped spent the whole recovery budget (30-38s) before the
	// 503. fire the sweep for the next request and answer this one now.
	if oneExitPool() {
		dryPoolRefill(true)
		return false
	}
	deadline := recoverDeadline(until)
	if !time.Now().Before(deadline) {
		return false
	}
	// forced: the ladder is spent, so the sweep is what everything is
	// waiting on, and the pacing window must not sit on it.
	dryPoolRefill(true)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(150 * time.Millisecond):
		}
		if countRateLimited() == 0 && !poolUnusable() {
			acclog.Printf("  zen pool written off: refilled, extending retry ladder")
			return true
		}
	}
	return false
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
	l.lastUsed = time.Now()
	l.limitedSince = time.Time{}
	l.consec429 = 0
	l.requests++
	collided := false
	if proxy != "" {
		// one exit per lane: the observed exit is this lane's only while
		// another lane does not already hold it. a colliding stamp must not
		// leave this lane pinless, so the exit is dropped here and a distinct
		// one is picked below.
		for _, o := range lanes {
			if o != l && o.proxy == proxy {
				collided = true
				break
			}
		}
		if !collided {
			stampLaneProxy(l, proxy)
		}
	}
	if model != "" {
		if l.tokens == nil {
			l.tokens = map[string]laneTokens{}
		}
		m := l.tokens[model]
		m.Requests++
		l.tokens[model] = m
	}
	lanesMu.Unlock()
	if collided {
		// a shared exit: give this lane one of its own so a working lane
		// never idles without one. pinUniqueProxy never hands out an exit
		// another lane holds, so a dry pool leaves the lane pinless until
		// its next request waits for a refill (laneExitWait).
		laneReproxy(l, model)
	}
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
		if len(free) > 0 {
			sort.Slice(free, func(i, j int) bool { return proxyWeight(free[i]) > proxyWeight(free[j]) })
			return sampleWeighted(free, 1)[0]
		}
		// every configured exit is taken, dead, or refusing this model:
		// fall through to the auto pool rather than starving the lane on a
		// list that no longer answers. a configured proxy that keeps
		// failing gets marked dead by the transport path, so this is what
		// keeps a stale custom list from costing every request.
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
	// prefer an exit already seen serving this model at admission: capacity is
	// per exit+bucket and an exit usually serves one id, so a random pin spends
	// a 429 hop discovering that it serves another model.
	var match []string
	for _, p := range cands {
		if exitBucketServes(p, model) {
			match = append(match, p)
		}
	}
	if len(match) > 0 {
		cands = match
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
		if taken[p] || exitDead(p) || exitGateCooling(p) || exitModelBurned(p, model) {
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
// recovered. the one exception is a pool that cannot serve anybody at all:
// then the marks are all that is left, and the full recheck drops them, with
// each exit's own probe deciding who comes back. the sweep is cooldown-gated
// like the other refresh trips, so a saturated pool cannot fire one per retry.
// returns whether the dry pick should re-filter the pool: only a sweep that
// actually started can refill it.
func recheckAllExits() bool {
	marked := countRateLimited()
	out := poolUnusable()
	if marked == 0 && !out {
		// nothing was written off: the pool is simply thinner than the lane
		// table, and only a refill (maybeRefreshPoolThin) can buy room.
		return false
	}
	if out {
		// no member can serve at all: the marks are not the problem, the exits
		// are. dryPoolRefill paces the full recheck and clears them.
		dryPoolRefill(false)
		return true
	}
	if !poolRefreshDue() {
		zenProxiesMu.RLock()
		ago := time.Since(zenPoolRefreshed)
		zenProxiesMu.RUnlock()
		if skipLogOnce(&dryPoolLoggedAt, poolRefreshCooldown()) {
			dbglog.Printf("  zen pool dry: %d marked exit(s), fresh sweep skipped (last %v ago, cooldown %v)", marked, ago.Round(time.Second), poolRefreshCooldown())
		}
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
//
// when every exit the picker will hand out is marked for model, the lane
// still gets one: pinBurnedProxy ranks the marks instead of refusing. a
// pinless lane cannot send at all (it waits on the pool, then fails), and
// the marks are per model and die with the next refresh.
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
			// marks only order the pool, they never empty it: every lane
			// keeps an exit while a unique one exists.
			p = pinBurnedProxy(l, model)
			maybeRefreshPoolThin()
			return p
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

// laneReproxy swaps a dead exit for a fresh verified one, keeping the lane
// session pinned (affinity survives proxy churn) and every other lane's exit
// untouched: no exit is ever held by two lanes. a mark for model is not a
// reason to drop the pin: the exit is still in the pool, still this lane's
// alone, and still answers the other models. only a dead exit or one the
// pool dropped costs the lane its pin ("" = direct, or the caller hands the
// request to a lane that owns an exit).
func laneReproxy(l *zenLane, model string) string {
	if p := pinUniqueProxy(l, model); p != "" {
		return p
	}
	lanesMu.Lock()
	p := l.proxy
	lanesMu.Unlock()
	if p != "" && !exitDead(p) && !exitGateCooling(p) && !exitOutOfPool(p) && !lanePinnedElsewhere(p, l) {
		return p
	}
	setLaneProxy(l, "")
	return ""
}

// poolExitsForPick is the pin source with marks ignored: custom proxies win,
// else the verified zen pool.
func poolExitsForPick() []string {
	if cps := getCustomProxies(); len(cps) > 0 {
		return append([]string(nil), cps...)
	}
	zenProxiesMu.RLock()
	defer zenProxiesMu.RUnlock()
	return append([]string(nil), zenProxies...)
}

// pinBurnedProxy is the last-resort pin: every exit the picker hands out is
// taken by a peer or marked for model, so it picks from the whole pool and
// lets the marks decide the order only. it prefers an exit that is neither
// marked for model nor the one that just failed, then any unmarked spare,
// then a marked one. dead and gate-cooled exits and exits a peer holds stay
// excluded: those are facts about the exit, not this model's cycle. "" only
// when the pool has nothing left but this lane's own pin, which the caller
// keeps.
func pinBurnedProxy(l *zenLane, model string) string {
	pool := poolExitsForPick()
	dead := map[string]bool{}
	marked := map[string]bool{}
	for _, p := range pool {
		if exitDead(p) || exitGateCooling(p) {
			dead[p] = true
			continue
		}
		if model != "" && (exitRateLimitedModel(p, model) || exitModelBurned(p, model)) {
			marked[p] = true
		}
	}
	lanesMu.Lock()
	defer lanesMu.Unlock()
	taken := map[string]bool{}
	for _, o := range lanes {
		if o != l && o.proxy != "" {
			taken[o.proxy] = true
		}
	}
	var clean, burned []string
	for _, p := range pool {
		if taken[p] || dead[p] || p == l.proxy {
			continue
		}
		if marked[p] {
			burned = append(burned, p)
			continue
		}
		clean = append(clean, p)
	}
	pick := clean
	if len(pick) == 0 {
		pick = burned
	}
	if len(pick) == 0 {
		return ""
	}
	sort.Slice(pick, func(i, j int) bool { return proxyWeight(pick[i]) > proxyWeight(pick[j]) })
	stampLaneProxy(l, pick[0])
	return pick[0]
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
// session_idle_minutes. working exit pins stay on their lanes until the lane
// itself is evicted or the exit fails (~50-100 requests per exit IP).
func sweepLanes() {
	cutoff := time.Now().Add(-laneTTL())
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
		// shared lanes are pre-opened seats, not conversations: they hold
		// a pin and no identity, so an idle sweep that drops them empties
		// the table (and /status) between zen traffic. trimLanesToPool is
		// what bounds them when the pool shrinks.
		if strings.HasPrefix(k, "shared:") {
			continue
		}
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
	if l.inflight > 0 {
		l.inflight--
	}
	lanesMu.Unlock()
	repairLanes()
}

// repairLanes gives idle lanes that lost their exit another one, then drops
// what the pool cannot cover. runs at request completion so a lane that ends
// a request pinless is either one free exit away from serving again or one
// lane too many, instead of sitting in the table until the next sweep.
func repairLanes() {
	lanesMu.Lock()
	var pinless []*zenLane
	for _, l := range lanes {
		if l.proxy == "" && l.inflight == 0 {
			pinless = append(pinless, l)
		}
	}
	lanesMu.Unlock()
	if len(pinless) == 0 {
		return
	}
	for _, l := range pinless {
		laneReproxy(l, "")
	}
	trimLanesToPool()
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
		pinLocked(nl, poolSnapshotForPins())
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

// laneNeedsProxy reports whether this lane may not send direct: always_proxy
// pins every request to an exit, and an escalated lane proved direct was burned
// for it (its own 403/429 came from the home ip). both mean a dry pool is a
// reason to wait for exits, never a reason to go direct.
func laneNeedsProxy(l *zenLane) bool {
	if cfg().Zen.AlwaysProxy {
		return true
	}
	lanesMu.Lock()
	defer lanesMu.Unlock()
	return l.escalated
}

// laneProxyWait hands the request an exit when the pool is dry, instead of
// dropping the ladder into its dry branch: it re-tries this lane's own pin, then
// tries a peer lane (a lane that already holds an exit answers instantly), and
// keeps a refill sweeping while it polls for up to poolRecoverWait (30s). when
// every exit in the current pool is gate-parked, gatePoolRefill triggers a
// full sweep of the rest of the list while we wait here so newly verified
// exits can be picked as soon as they land.
func laneProxyWait(ctx context.Context, l *zenLane, model string, until *time.Time) (*zenLane, string) {
	for {
		if p := laneProxyFor(l, model); p != "" {
			return l, p
		}
		// the last sweeps proved the tier is out for every exit: no draw and no
		// refill can help for the backoff window, so answer now instead of
		// holding the caller through a wait that already spent itself.
		if zenTierDown() && (poolAllGateParked() || poolEmpty()) {
			trimLanesToPool()
			return l, ""
		}
		if poolAllGateParked() {
			gatePoolRefill()
		} else {
			dryPoolRefill(false)
		}
		if hl := laneFailover(l); hl != l {
			if p := laneProxyFor(hl, model); p != "" {
				return hl, p
			}
		}
		// only a wait starts the clock: a request served on the first try
		// draws no budget, so a later wait keeps all of it.
		if !time.Now().Before(recoverDeadline(until)) {
			trimLanesToPool()
			return l, ""
		}
		select {
		case <-ctx.Done():
			return l, ""
		case <-time.After(100 * time.Millisecond):
		}
	}
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
