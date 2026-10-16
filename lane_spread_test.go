package main

import (
	"testing"
)

// one exit per lane is the invariant every pin path has to hold, and a
// lane's counters describe the exit it holds: swapping exits resets them.
// owner: stampLaneProxy (via laneReproxy/laneRotate).
func TestReproxyResetsCountersOnPinChange(t *testing.T) {
	resetLanes()
	defer resetLanes()
	resetBurns()
	defer resetBurns()
	setPool("http://10.0.0.1:1", "http://10.0.0.2:2", "http://10.0.0.3:3")
	defer setPool()
	c := testConfig()
	c.Zen.AlwaysProxy = true
	c.Zen.ExitBurnMinutes = 30
	const model = "m"
	applyConfig(c)
	defer applyConfig(testConfig())

	l := &zenLane{id: "x", session: zenSession()}
	lanesMu.Lock()
	lanes["x"] = l
	lanesMu.Unlock()
	first := laneReproxy(l, model)
	if first == "" {
		t.Fatal("lane must pin an exit")
	}
	lanesMu.Lock()
	l.rateLimit, l.consec429 = 7, 3
	lanesMu.Unlock()
	// retire the first exit+model pair and rotate: the fresh pin starts clean.
	noteExit429(first, model)
	noteExit429(first, model)
	second := laneReproxy(l, model)
	if second == "" || second == first {
		t.Fatalf("lane must move to a healthy exit, got %q (was %q)", second, first)
	}
	lanesMu.Lock()
	rate, strikes := l.rateLimit, l.consec429
	lanesMu.Unlock()
	if rate != 0 || strikes != 0 {
		t.Fatalf("pin change must reset the lane counters, got rate=%d strikes=%d", rate, strikes)
	}
	if !exitRateLimited(first) {
		t.Fatalf("exit %s must be rate-limited after two 429s", first)
	}
}

// a success clears the lane's 429 run without touching the exit's own mark.
func TestLaneTouchResetsConsec429(t *testing.T) {
	resetLanes()
	defer resetLanes()
	resetBurns()
	defer resetBurns()
	setPool("http://10.0.0.1:1", "http://10.0.0.2:2", "http://10.0.0.3:3")
	defer setPool()
	c := testConfig()
	c.Zen.AlwaysProxy = true
	applyConfig(c)
	defer applyConfig(testConfig())

	l := &zenLane{id: "x", session: zenSession()}
	lanesMu.Lock()
	lanes["x"] = l
	lanesMu.Unlock()
	pin := laneReproxy(l, "m")
	laneRotate(l, "m")
	laneTouch(l, pin, "m")
	lanesMu.Lock()
	strikes := l.consec429
	lanesMu.Unlock()
	if strikes != 0 {
		t.Fatalf("a success must reset the lane's 429 run, got %d", strikes)
	}
}

// dupPin returns the first exit held by two or more lanes, "" when every
// pin is unique. the invariant every pin path has to hold: one exit per
// lane, always.
func dupPin() string {
	lanesMu.Lock()
	defer lanesMu.Unlock()
	seen := map[string]bool{}
	for _, l := range lanes {
		if l.proxy == "" {
			continue
		}
		if seen[l.proxy] {
			return l.proxy
		}
		seen[l.proxy] = true
	}
	return ""
}

// owner boundary: laneProxyFor + lanePinnedElsewhere. a pin is one lane's
// only while no other lane holds it, so a duplicate re-picks onto a spare
// rather than riding the exit the other lane already owns.
func TestLanePinHeldElsewhereRepicks(t *testing.T) {
	resetLanes()
	defer resetLanes()
	resetBurns()
	defer resetBurns()
	setPool("http://10.0.0.1:1", "http://10.0.0.2:2")
	defer setPool()
	c := testConfig()
	c.Zen.AlwaysProxy = true
	applyConfig(c)
	defer applyConfig(testConfig())

	l := &zenLane{id: "x", session: zenSession(), proxy: "http://10.0.0.1:1"}
	lanesMu.Lock()
	lanes["x"] = l
	lanes["y"] = &zenLane{id: "y", session: zenSession(), proxy: "http://10.0.0.1:1"}
	lanesMu.Unlock()

	if !lanePinnedElsewhere("http://10.0.0.1:1", l) {
		t.Fatal("a pin another lane holds must read held elsewhere")
	}
	got := laneProxyFor(l, "m")
	if got != "http://10.0.0.2:2" {
		t.Fatalf("duplicate pin must re-pick the free spare, got %q", got)
	}
	if l.proxy != got {
		t.Fatalf("lane must keep the new pin %q, still on %q", got, l.proxy)
	}
	if !lanePinnedElsewhere("http://10.0.0.1:1", l) {
		t.Fatal("the other lane must keep its exit")
	}
}

// owner boundary: lanePinnedElsewhere + laneReproxy. a duplicate pin is
// dropped when no unique exit is left: two lanes never share one exit, so
// the second lane gives it up (direct, or a handoff to a lane that owns
// one) instead of splitting a single per-exit budget.
func TestLanePinDuplicateDroppedWhenNoSpare(t *testing.T) {
	resetLanes()
	defer resetLanes()
	resetBurns()
	defer resetBurns()
	setPool("http://10.0.0.1:1")
	defer setPool()
	c := testConfig()
	c.Zen.AlwaysProxy = true
	applyConfig(c)
	defer applyConfig(testConfig())

	l := &zenLane{id: "x", session: zenSession(), proxy: "http://10.0.0.1:1"}
	lanesMu.Lock()
	lanes["x"] = l
	for i := 0; i < 4; i++ {
		lanes[fillerKey(i)] = &zenLane{id: "f", session: zenSession(), proxy: "http://10.0.0.1:1"}
	}
	lanesMu.Unlock()

	if got := laneProxyFor(l, "m"); got != "" {
		t.Fatalf("no unique exit left: want no pin, got %q", got)
	}
	if l.proxy != "" {
		t.Fatalf("lane kept the shared pin %q", l.proxy)
	}
}

// owner boundary: lane picker. a pool thinner than the lane table still
// spreads while an exit is untaken: the crowded lane moves onto it and
// only the lanes with nothing unique left run unpinned.
func TestCrowdedPinSpreadsOntoSpare(t *testing.T) {
	resetLanes()
	defer resetLanes()
	resetBurns()
	defer resetBurns()
	setPool("http://10.0.0.1:1", "http://10.0.0.2:2", "http://10.0.0.3:3")
	defer setPool()
	c := testConfig()
	c.Zen.AlwaysProxy = true
	applyConfig(c)
	defer applyConfig(testConfig())

	const crowded = "http://10.0.0.1:1"
	l := &zenLane{id: "x", session: zenSession(), proxy: crowded}
	lanesMu.Lock()
	lanes["x"] = l
	for i := 0; i < 6; i++ {
		lanes[fillerKey(i)] = &zenLane{id: "f", session: zenSession(), proxy: crowded}
	}
	lanes["y"] = &zenLane{id: "y", session: zenSession(), proxy: "http://10.0.0.2:2"}
	lanesMu.Unlock()

	if got := laneProxyFor(l, "m"); got != "http://10.0.0.3:3" {
		t.Fatalf("crowded lane must take the untaken exit, got %q", got)
	}
	if l.proxy != "http://10.0.0.3:3" {
		t.Fatalf("lane must keep the new pin, still on %q", l.proxy)
	}
}

// owner boundary: laneTouch. a success must not stamp an exit another lane
// already owns: the stamp drops the pin instead, so observed state can
// never hold two lanes on one exit.
func TestLaneTouchDropsSharedPin(t *testing.T) {
	resetLanes()
	defer resetLanes()
	l := &zenLane{id: "x", session: zenSession()}
	lanesMu.Lock()
	lanes["x"] = l
	lanes["y"] = &zenLane{id: "y", session: zenSession(), proxy: "http://10.0.0.1:1"}
	lanesMu.Unlock()

	laneTouch(l, "http://10.0.0.1:1", "m")
	if l.proxy != "" {
		t.Fatalf("shared stamp must not pin %q", l.proxy)
	}
	laneTouch(l, "http://10.0.0.2:2", "m")
	if l.proxy != "http://10.0.0.2:2" {
		t.Fatalf("unshared stamp must pin, got %q", l.proxy)
	}
	if got := dupPin(); got != "" {
		t.Fatalf("two lanes hold %q after a shared stamp", got)
	}
}
func fillerKey(i int) string {
	return "fill:" + string(rune('a'+i))
}

// regression for the reported /status snapshot: 15 of 15 lanes pinned to
// one exit (the only country-ES exit) while 11 other verified exits sat
// idle. every lane re-picks onto an untaken exit, so the table ends up
// with distinct pins instead of riding one ip. owner: laneProxyFor +
// lanePinnedElsewhere.
func TestReportedPileupSpreadsLanes(t *testing.T) {
	resetLanes()
	defer resetLanes()
	resetBurns()
	defer resetBurns()
	setPool("http://10.0.0.1:1", "http://10.0.0.2:2", "http://10.0.0.3:3", "http://10.0.0.4:4")
	defer setPool()
	c := testConfig()
	c.Zen.AlwaysProxy = true
	applyConfig(c)
	defer applyConfig(testConfig())

	const pileup = "http://10.0.0.1:1"
	var ls []*zenLane
	lanesMu.Lock()
	for i := 0; i < 4; i++ {
		l := &zenLane{id: "p" + string(rune('a'+i)), session: zenSession(), proxy: pileup}
		ls = append(ls, l)
		lanes[l.id] = l
	}
	lanesMu.Unlock()
	if got := dupPin(); got != pileup {
		t.Fatalf("fixture must start piled up on %s, got %q", pileup, got)
	}

	for _, l := range ls {
		laneProxyFor(l, "m")
	}
	if got := dupPin(); got != "" {
		t.Fatalf("lanes still share %s", got)
	}
	seen := map[string]bool{}
	lanesMu.Lock()
	for _, l := range ls {
		if seen[l.proxy] {
			t.Fatalf("exit %s held twice", l.proxy)
		}
		seen[l.proxy] = true
	}
	lanesMu.Unlock()
	if len(seen) != 4 {
		t.Fatalf("want 4 distinct exits, got %v", seen)
	}
}
