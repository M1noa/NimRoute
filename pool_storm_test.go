package main

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// a mitm exit must be detected from the transport error.
func TestIsTLSFailure(t *testing.T) {
	tls := []string{
		`Post "https://opencode.ai/zen/v1/chat/completions": tls: failed to verify certificate: x509: certificate signed by unknown authority`,
		`x509: certificate has expired`,
		`tls: handshake failure`,
	}
	for _, s := range tls {
		if !isTLSFailure(errors.New(s)) {
			t.Errorf("must detect tls failure: %q", s)
		}
	}
	other := []string{
		`dial tcp 72.37.216.68:4145: connect: connection refused`,
		`EOF`,
		`read: connection reset by peer`,
	}
	for _, s := range other {
		if isTLSFailure(errors.New(s)) {
			t.Errorf("must not flag non-tls error: %q", s)
		}
	}
	if isTLSFailure(nil) {
		t.Error("nil error is not a tls failure")
	}
}

// resetTLSBurns clears the exit-health registry (dead and rate-limited marks
// live in the same rows as the 429 counters).
func resetTLSBurns() { resetBurns() }

func setPool(proxies ...string) {
	zenProxiesMu.Lock()
	defer zenProxiesMu.Unlock()
	zenProxies = append([]string(nil), proxies...)
}

// a tls-banned exit must never be picked, and must not re-enter the pool:
// single-candidate pools bypass weighting, so the pick is deterministic.
func TestTLSBanSkipsPicker(t *testing.T) {
	resetLanes()
	defer resetLanes()
	resetTLSBurns()
	defer resetTLSBurns()
	setPool("http://9.9.9.9:1")
	defer setPool()

	if got := pickLaneProxyFor(""); got != "http://9.9.9.9:1" {
		t.Fatalf("unbanned single exit must be picked, got %q", got)
	}
	banTLSExit("http://9.9.9.9:1")
	if !exitDead("http://9.9.9.9:1") {
		t.Fatal("banned exit must report dead")
	}
	if got := pickLaneProxyFor(""); got != "" {
		t.Fatalf("banned exit must not be picked, got %q", got)
	}
	zenProxiesMu.RLock()
	n := len(zenProxies)
	zenProxiesMu.RUnlock()
	if n != 1 {
		t.Fatalf("last exit must survive the ban-drop, pool = %d", n)
	}
}

// banning must evacuate every lane pinned to the dead exit: with a spare
// in the pool the lane moves, and laneProxyFor never returns the banned pin.
func TestTLSBanEvictsPins(t *testing.T) {
	resetLanes()
	defer resetLanes()
	resetTLSBurns()
	defer resetTLSBurns()
	setPool("http://10.0.0.1:1", "http://10.0.0.2:2", "http://10.0.0.3:3")
	defer setPool()
	lanesMu.Lock()
	lanes["x"] = &zenLane{id: "x", session: zenSession(), proxy: "http://10.0.0.1:1"}
	lanes["y"] = &zenLane{id: "y", session: zenSession(), proxy: "http://10.0.0.2:2"}
	lanesMu.Unlock()

	banTLSExit("http://10.0.0.1:1")

	lanesMu.Lock()
	defer lanesMu.Unlock()
	for k, l := range lanes {
		if l.proxy == "http://10.0.0.1:1" {
			t.Fatalf("lane %s still pinned to the banned exit", k)
		}
	}
	if lanes["x"].proxy != "http://10.0.0.3:3" {
		t.Fatalf("evicted lane must take the spare, got %q", lanes["x"].proxy)
	}
	if lanes["y"].proxy != "http://10.0.0.2:2" {
		t.Fatalf("healthy lane must keep its pin, got %q", lanes["y"].proxy)
	}
}

// a dead exit must never be picked, must not come back through the picker
// after a dry-pool recheck (the recheck clears rate-limit marks only: a
// mitm exit fails every real send), and must not be re-verified into the
// pool by the refresh candidate filter.
func TestDeadExitStaysOutOfRotation(t *testing.T) {
	resetLanes()
	defer resetLanes()
	resetTLSBurns()
	defer resetTLSBurns()
	setPool("http://9.9.9.9:1", "http://10.0.0.2:2")
	defer setPool()

	banTLSExit("http://9.9.9.9:1")
	if !exitDead("http://9.9.9.9:1") {
		t.Fatal("mitm exit must be marked dead")
	}
	for i := 0; i < 20; i++ {
		if p := pickLaneProxyFor(""); p == "http://9.9.9.9:1" {
			t.Fatalf("dead exit picked: %s", p)
		}
	}
	recheckAllExits()
	if !exitDead("http://9.9.9.9:1") {
		t.Fatal("a dry-pool recheck must not revive a dead exit")
	}
	if n := countDeadExits(); n != 1 {
		t.Fatalf("dead count = %d, want 1", n)
	}
}

// net-error refreshes must honor the same cooldown as the 429 path: without
// it 12 concurrent lanes trip a threshold of 3 instantly and every burst
// fires a full verify sweep.
func TestNetErrRefreshCooldown(t *testing.T) {
	c := testConfig()
	c.Zen.ErrorThreshold = 2
	c.Zen.PoolRefreshCooldownSecs = 120
	useConfig(t, c)
	resetZenNetErrs()
	// hold the refresh mutex so a trip collapses without network.
	zenRefreshMu.Lock()
	defer zenRefreshMu.Unlock()

	zenProxiesMu.Lock()
	zenPoolRefreshed = time.Now()
	zenProxiesMu.Unlock()
	for i := 0; i < 2; i++ {
		if maybeRefreshNetErr() {
			t.Fatal("trip inside cooldown must skip, not fire")
		}
	}
	zenNetErrMu.Lock()
	left := zenNetErrs
	zenNetErrMu.Unlock()
	if left != 0 {
		t.Fatalf("counter = %d after skipped trip, want reset to 0", left)
	}

	zenProxiesMu.Lock()
	zenPoolRefreshed = time.Now().Add(-time.Hour)
	zenProxiesMu.Unlock()
	if maybeRefreshNetErr() {
		t.Fatal("first error below threshold must not fire")
	}
	if !maybeRefreshNetErr() {
		t.Fatal("threshold trip outside cooldown must fire")
	}
}

// a thin pool never shares: with 1 exit and a second lane asking, the picker
// answers "" instead of the exit the first lane holds. two lanes on one exit
// split its per-exit budget, so the shared exit 429s sooner and both lanes
// eat the limit. owner: lane picker (pickLaneProxyFor) + laneReproxy.
func TestThinPoolNeverShares(t *testing.T) {
	resetLanes()
	defer resetLanes()
	resetBurns()
	defer resetBurns()
	resetTLSBurns()
	defer resetTLSBurns()
	// the pool is thinner than the lane table, so the pin path asks for a
	// refill: hold the refresh mutex so the trip collapses without network.
	zenRefreshMu.Lock()
	defer zenRefreshMu.Unlock()
	setPool("http://10.0.0.1:1")
	defer setPool()

	a := &zenLane{id: "share-a", session: zenSession()}
	b := &zenLane{id: "share-b", session: zenSession()}
	lanesMu.Lock()
	lanes["a"], lanes["b"] = a, b
	lanesMu.Unlock()
	if p := laneReproxy(a, "m"); p != "http://10.0.0.1:1" {
		t.Fatalf("first lane must pin the only exit, got %q", p)
	}
	// every exit taken: no pin, never a second lane on the same exit.
	if p := pickLaneProxyFor("m"); p != "" {
		t.Fatalf("thin pool must not hand the taken exit to a second lane, got %q", p)
	}
	if p := laneReproxy(b, "m"); p != "" {
		t.Fatalf("second lane must get no pin, got %q", p)
	}
	lanesMu.Lock()
	bp := b.proxy
	lanesMu.Unlock()
	if bp != "" {
		t.Fatalf("second lane kept the shared pin %q", bp)
	}
	if got := dupPin(); got != "" {
		t.Fatalf("two lanes hold %q", got)
	}
}

// a fat pool must not double-pin: with exits to spare the picker hands out
// untaken ones, keeping the retry paths apart. owner: lane picker.
func TestFatPoolSpreadsExits(t *testing.T) {
	resetLanes()
	defer resetLanes()
	resetBurns()
	defer resetBurns()
	resetTLSBurns()
	defer resetTLSBurns()
	setPool("http://10.0.0.1:1", "http://10.0.0.2:2", "http://10.0.0.3:3")
	defer setPool()

	body := []byte(`{"messages":[{"role":"user","content":"a"}]}`)
	a := laneFor("spread-a", body)
	b := laneFor("spread-b", body)
	pa := laneReproxy(a, "m")
	pb := laneReproxy(b, "m")
	if pa == "" || pb == "" {
		t.Fatalf("lanes pinned nothing: %q %q", pa, pb)
	}
	if pa == pb {
		t.Fatalf("fat pool double-pinned %q, want distinct exits", pa)
	}
}

// concurrent rotations must not converge on one exit: two lanes reproxying
// at the same time both come from a stale taken snapshot, so the pin must
// re-validate under the write lock and re-pick. owner: lane pinning
// (pinUniqueProxy). regression for the /status report of several lanes
// showing the same proxy after a burst.
func TestConcurrentReproxyStaysUnique(t *testing.T) {
	resetLanes()
	defer resetLanes()
	resetBurns()
	defer resetBurns()
	resetTLSBurns()
	defer resetTLSBurns()
	setPool("http://10.0.0.1:1", "http://10.0.0.2:2", "http://10.0.0.3:3", "http://10.0.0.4:4")
	defer setPool()

	body := []byte(`{"messages":[{"role":"user","content":"a"}]}`)
	const n = 4
	lanes := make([]*zenLane, n)
	for i := range lanes {
		lanes[i] = laneFor(fmt.Sprintf("unique-%d", i), body)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := map[string]int{}
	for i, l := range lanes {
		wg.Add(1)
		go func(i int, l *zenLane) {
			defer wg.Done()
			// stagger slightly so some rotations truly interleave
			time.Sleep(time.Duration(i%2) * time.Millisecond)
			p := laneReproxy(l, "m")
			mu.Lock()
			seen[p]++
			mu.Unlock()
		}(i, l)
	}
	wg.Wait()
	for p, c := range seen {
		if c > 1 {
			t.Fatalf("exit %q pinned by %d lanes, want unique pins: %v", p, c, seen)
		}
		if p == "" {
			t.Fatalf("a lane pinned nothing while spares existed: %v", seen)
		}
	}
	if len(seen) != n {
		t.Fatalf("want %d distinct exits, got %v", n, seen)
	}
}

// a pool narrower than the lane table must ask for a refill: every exit
// ends up pinned, so no lane can draw a unique one and the pins that verify
// pile up. cooldown-gated like every other trip, and silent on a cold start
// (watchZenProxies already owns that sweep). owner: pool sizing.
func TestPoolThinTriggersRefill(t *testing.T) {
	c := testConfig()
	c.Zen.PoolRefreshCooldownSecs = 120
	useConfig(t, c)
	// collapse the sweep without network.
	zenRefreshMu.Lock()
	defer zenRefreshMu.Unlock()
	stamp := func(ago time.Duration) {
		zenProxiesMu.Lock()
		zenPoolRefreshed = time.Now().Add(-ago)
		zenProxiesMu.Unlock()
	}
	lanes := func(n int) {
		resetLanes()
		for i := 0; i < n; i++ {
			lanesMu.Lock()
			lanes[fmt.Sprintf("l%d", i)] = &zenLane{id: "x", session: zenSession()}
			lanesMu.Unlock()
		}
	}
	old := zenPoolRefreshed
	defer func() {
		zenProxiesMu.Lock()
		zenPoolRefreshed = old
		zenProxiesMu.Unlock()
	}()

	// fat pool: nothing to refill, no trip.
	setPool("http://10.0.0.1:1", "http://10.0.0.2:2", "http://10.0.0.3:3")
	defer setPool()
	lanes(3)
	stamp(time.Hour)
	before := zenPoolRefreshed
	maybeRefreshPoolThin()
	if !zenPoolRefreshed.Equal(before) {
		t.Fatal("pool covering the lane table must not trip a refresh")
	}

	// thin pool outside the cooldown: trip.
	lanes(5)
	stamp(time.Hour)
	before = zenPoolRefreshed
	maybeRefreshPoolThin()
	if zenPoolRefreshed.Equal(before) {
		t.Fatal("pool narrower than the lane table must trip a refresh")
	}

	// thin pool inside the cooldown: skip, no stamp.
	lanes(5)
	stamp(10 * time.Second)
	before = zenPoolRefreshed
	maybeRefreshPoolThin()
	if !zenPoolRefreshed.Equal(before) {
		t.Fatal("trip inside the cooldown must skip")
	}

	// cold start: the boot sweep owns it.
	setPool()
	resetLanes()
	stamp(time.Hour)
	before = zenPoolRefreshed
	maybeRefreshPoolThin()
	if !zenPoolRefreshed.Equal(before) {
		t.Fatal("empty pool must not trip: watchZenProxies owns the cold sweep")
	}
}

// a pool whose every exit is taken or marked for a model sweeps for fresh
// exits and hands out the least-recently-marked one as a second chance,
// instead of sharing an exit or clearing every mark. clearing the marks put
// the exits that had just 429'd straight back at the front of the rotation.
// owner: pickLaneProxyExcluding's dry branch + recheckAllExits.
func TestDryPoolSecondChanceKeepsMarks(t *testing.T) {
	resetLanes()
	defer resetLanes()
	resetBurns()
	defer resetBurns()
	resetTLSBurns()
	defer resetTLSBurns()
	// hold the refresh mutex so the trip collapses without network.
	zenRefreshMu.Lock()
	defer zenRefreshMu.Unlock()
	setPool("http://10.0.0.1:1", "http://10.0.0.2:2")
	defer setPool()
	oldRefreshed := zenPoolRefreshed
	zenProxiesMu.Lock()
	zenPoolRefreshed = time.Now().Add(-time.Hour)
	zenProxiesMu.Unlock()
	defer func() {
		zenProxiesMu.Lock()
		zenPoolRefreshed = oldRefreshed
		zenProxiesMu.Unlock()
	}()
	c := testConfig()
	c.Zen.AlwaysProxy = true
	c.Zen.PoolRefreshCooldownSecs = 120
	useConfig(t, c)

	// both exits written off for m: one rate-limited, one refusing the model.
	markExitRateLimited("http://10.0.0.1:1", "m", "test")
	noteExitModelBurned("http://10.0.0.2:2", "m")

	// a free-tier refusal is a hard no; the rate-limited exit gets the pick.
	if got := pickLaneProxyFor("m"); got != "http://10.0.0.1:1" {
		t.Fatalf("dry pick = %q, want the least-recently-marked exit http://10.0.0.1:1", got)
	}
	if left := countRateLimited(); left != 1 {
		t.Fatalf("a dry pick must not clear marks, %d left of 1", left)
	}
	if !exitModelBurned("http://10.0.0.2:2", "m") {
		t.Fatal("a dry pick must not clear the model refusals")
	}
	// the dry pick outside the cooldown fired a fresh sweep.
	zenProxiesMu.RLock()
	moved := time.Since(zenPoolRefreshed) < time.Minute
	zenProxiesMu.RUnlock()
	if !moved {
		t.Fatal("a dry pick outside the cooldown must fire a fresh sweep")
	}
}

// a pool thinner than the lane table is not a dry pool: nothing is written
// off, so there is nothing to recheck and the cooldown stamp stays put.
func TestThinPoolIsNotDry(t *testing.T) {
	resetLanes()
	defer resetLanes()
	resetBurns()
	defer resetBurns()
	setPool("http://10.0.0.1:1")
	defer setPool()
	c := testConfig()
	c.Zen.AlwaysProxy = true
	useConfig(t, c)

	before := zenPoolRefreshed
	if recheckAllExits() {
		t.Fatal("nothing written off: recheck must not fire")
	}
	zenProxiesMu.RLock()
	after := zenPoolRefreshed
	zenProxiesMu.RUnlock()
	if !after.Equal(before) {
		t.Fatal("a no-op recheck must not stamp the refresh cooldown")
	}
}

// a dry pick inside the refresh cooldown still answers with an exit (the
// oldest mark) and leaves the cooldown stamp alone: the cooldown gates
// verify sweeps, not the pick itself, and a "no proxy available" 502 is what
// the ladder must never see. owner: pickLaneProxyExcluding + recheckAllExits.
func TestDryPickSecondChanceInsideCooldown(t *testing.T) {
	resetLanes()
	defer resetLanes()
	resetBurns()
	defer resetBurns()
	resetTLSBurns()
	defer resetTLSBurns()
	setPool("http://10.0.0.1:1", "http://10.0.0.2:2")
	defer setPool()
	oldRefreshed := zenPoolRefreshed
	defer func() {
		zenProxiesMu.Lock()
		zenPoolRefreshed = oldRefreshed
		zenProxiesMu.Unlock()
	}()
	c := testConfig()
	c.Zen.AlwaysProxy = true
	c.Zen.PoolRefreshCooldownSecs = 120
	useConfig(t, c)
	// refresh fired 10s ago: the refetch sits inside the cooldown window.
	zenProxiesMu.Lock()
	zenPoolRefreshed = time.Now().Add(-10 * time.Second)
	zenProxiesMu.Unlock()

	// both exits written off for m: one rate-limited, one refusing the model.
	markExitRateLimited("http://10.0.0.1:1", "m", "test")
	noteExitModelBurned("http://10.0.0.2:2", "m")

	before := zenPoolRefreshed
	if got := pickLaneProxyFor("m"); got != "http://10.0.0.1:1" {
		t.Fatalf("dry pick inside cooldown = %q, want the marked exit, not 502", got)
	}
	zenProxiesMu.RLock()
	after := zenPoolRefreshed
	zenProxiesMu.RUnlock()
	if !after.Equal(before) {
		t.Fatal("refetch inside the cooldown window must not stamp the cooldown")
	}
	left, leftBurned := countRateLimited(), 0
	if exitModelBurned("http://10.0.0.2:2", "m") {
		leftBurned = 1
	}
	if left != 1 || leftBurned != 1 {
		t.Fatalf("a dry pick must keep the written-off state: %d marks, %d burns", left, leftBurned)
	}
}

// regression for live 502s with 11 verified exits across 15 lanes: when the
// pool is thinner than the lane table AND every free exit is written off,
// pinUniqueProxy must still land a pin through the dry pick's second chance.
// owner: pinUniqueProxy + pickLaneProxyExcluding.
func TestPinUniqueProxyThinPoolDoesNotStarveRecheck(t *testing.T) {
	resetLanes()
	defer resetLanes()
	resetBurns()
	defer resetBurns()
	resetTLSBurns()
	defer resetTLSBurns()
	zenRefreshMu.Lock()
	defer zenRefreshMu.Unlock()
	setPool("http://10.0.0.1:1", "http://10.0.0.2:2")
	defer setPool()
	oldRefreshed := zenPoolRefreshed
	zenProxiesMu.Lock()
	zenPoolRefreshed = time.Now().Add(-time.Hour)
	zenProxiesMu.Unlock()
	defer func() {
		zenProxiesMu.Lock()
		zenPoolRefreshed = oldRefreshed
		zenProxiesMu.Unlock()
	}()
	c := testConfig()
	c.Zen.AlwaysProxy = true
	c.Zen.Lanes = 5
	c.Zen.PoolRefreshCooldownSecs = 120
	useConfig(t, c)

	// 5 lanes > 2 pool exits (thin pool); exit 1 pinned on the first lane,
	// exit 2 marked for the model so every free exit is written off for it.
	ensureSharedLanes()
	var target *zenLane
	lanesMu.Lock()
	first := true
	for _, l := range lanes {
		if first {
			l.proxy = "http://10.0.0.1:1"
			first = false
		} else if target == nil {
			target = l
		}
	}
	lanesMu.Unlock()
	markExitRateLimited("http://10.0.0.2:2", "m", "test")

	got := pinUniqueProxy(target, "m")
	if got != "http://10.0.0.2:2" {
		t.Fatalf("pinUniqueProxy on thin+written-off pool = %q, want http://10.0.0.2:2", got)
	}
}

// regression for live 502s ("no proxy available") when 14 of 15 lanes are
// unpinned and 1 lane holds the only working exit: laneFailover must prefer
// a pinned lane over unpinned ones so the retry ladder reaches the working
// exit in one hop instead of walking 7 pinless lanes and 502ing.
// owner: laneFailover.
func TestLaneFailoverPrefersPinnedLane(t *testing.T) {
	resetLanes()
	defer resetLanes()
	c := testConfig()
	c.Zen.Lanes = 15
	useConfig(t, c)

	now := time.Now()
	var start, pinned *zenLane
	lanesMu.Lock()
	for i := 0; i < 14; i++ {
		l := &zenLane{
			id:       fmt.Sprintf("shared:p%02d", i),
			session:  zenSession(),
			lastUsed: now.Add(-time.Duration(i+2) * time.Minute),
		}
		lanes[l.id] = l
		if start == nil {
			start = l
		}
	}
	pinned = &zenLane{
		id:       "shared:holder",
		session:  zenSession(),
		proxy:    "socks4://142.54.239.1:4145",
		lastUsed: now.Add(-10 * time.Second), // fresher than every pinless lane
	}
	lanes[pinned.id] = pinned
	lanesMu.Unlock()

	if got := laneFailover(start); got != pinned {
		t.Fatalf("laneFailover picked %s (proxy=%q), want pinned lane %s", got.id, got.proxy, pinned.id)
	}
}
