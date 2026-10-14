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

func resetTLSBurns() {
	tlsBurnMu.Lock()
	defer tlsBurnMu.Unlock()
	tlsBurned = map[string]time.Time{}
}

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
	if !exitTLSBanned("http://9.9.9.9:1") {
		t.Fatal("banned exit must report banned")
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

// the refresh must not re-admit banned exits: a mitm exit passes verify
// intermittently, so the candidate filter is the only gate.
func TestRefreshSkipsTLSBanned(t *testing.T) {
	resetTLSBurns()
	defer resetTLSBurns()
	banTLSExit("http://9.9.9.9:1")
	got := skipTLSBanned([]string{"http://9.9.9.9:1", "http://10.0.0.2:2"})
	if len(got) != 1 || got[0] != "http://10.0.0.2:2" {
		t.Fatalf("banned exit must be filtered, got %v", got)
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

// a thin pool must share, not starve: with 1 exit and 2 lanes the second
// lane takes the taken exit (least-loaded) instead of "". owner: lane
// picker (pickLaneProxyFor). regression for cold-start 502s where every
// lane past the first failed until the sweep finished.
func TestThinPoolSharesExit(t *testing.T) {
	resetLanes()
	defer resetLanes()
	resetBurns()
	defer resetBurns()
	resetTLSBurns()
	defer resetTLSBurns()
	setPool("http://10.0.0.1:1")
	defer setPool()

	body := []byte(`{"messages":[{"role":"user","content":"a"}]}`)
	a := laneFor("share-a", body)
	b := laneFor("share-b", body)
	if p := laneReproxy(a, "m"); p == "" {
		t.Fatal("first lane pinned nothing")
	}
	// every exit taken: must share the live exit, never "".
	if p := pickLaneProxyFor("m"); p != "http://10.0.0.1:1" {
		t.Fatalf("thin pool must share the exit, got %q", p)
	}
	if p := laneReproxy(b, "m"); p != "http://10.0.0.1:1" {
		t.Fatalf("second lane must share the exit, got %q", p)
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

// leastLoadedExit balances: the exit with fewer pins wins, burned exits
// never win. owner: lane picker.
func TestLeastLoadedExit(t *testing.T) {
	resetLanes()
	defer resetLanes()
	resetBurns()
	defer resetBurns()
	setPool("http://10.0.0.1:1", "http://10.0.0.2:2")
	defer setPool()

	lanesMu.Lock()
	lanes["x"] = &zenLane{id: "x", session: zenSession(), proxy: "http://10.0.0.1:1"}
	lanes["y"] = &zenLane{id: "y", session: zenSession(), proxy: "http://10.0.0.1:1"}
	lanes["z"] = &zenLane{id: "z", session: zenSession(), proxy: "http://10.0.0.2:2"}
	lanesMu.Unlock()

	if got := leastLoadedExit("m"); got != "http://10.0.0.2:2" {
		t.Fatalf("want the 1-pin exit, got %q", got)
	}
	burnExit("http://10.0.0.2:2", "m")
	if got := leastLoadedExit("m"); got != "http://10.0.0.1:1" {
		t.Fatalf("burned exit must lose, got %q", got)
	}
	burnExit("http://10.0.0.1:1", "m")
	if got := leastLoadedExit("m"); got != "" {
		t.Fatalf("all burned must be dry, got %q", got)
	}
}
