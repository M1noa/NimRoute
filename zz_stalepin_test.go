package main

import (
	"testing"
	"time"
)

// a dead exit dropped from the pool but still pinned on the lane must not
// eat every retry: laneReproxy keeps the stale pin when no spare exists,
// so the transport-error path must fail the lane over like the 429 path
// does. regression for the nemotron probe case: 5 dials to 192.203.0.250
// in a row while the model served fine on another lane a second later.
func TestStalePinFailsLaneOver(t *testing.T) {
	resetLanes()
	defer resetLanes()
	resetBurns()
	defer resetBurns()
	zenProxiesMu.Lock()
	zenProxies = []string{"http://10.0.0.1:1", "http://10.0.0.2:2"}
	zenProxiesMu.Unlock()
	defer func() {
		zenProxiesMu.Lock()
		zenProxies = nil
		zenProxiesMu.Unlock()
	}()
	c := testConfig()
	c.Zen.AlwaysProxy = true
	applyConfig(c)
	defer applyConfig(testConfig())

	body := []byte(`{"messages":[{"role":"user","content":"a"}]}`)
	a := laneFor("s1", body)
	b := laneFor("s2", body)
	// pin every exit so the picker has nothing spare: a holds .1, b holds .2.
	if p := laneReproxy(a, "m"); p == "" {
		t.Fatal("lane a pinned nothing")
	}
	if p := laneReproxy(b, "m"); p == "" {
		t.Fatal("lane b pinned nothing")
	}
	dead := a.proxy

	// the exit dies mid-request: dropped from the pool, still pinned.
	// thin pool (1 exit left, 2 lanes) shares the live exit instead of
	// keeping the dead pin: a live shared send beats a dead redial.
	dropProxy(dead)
	if got := laneReproxy(a, "m"); got == dead {
		t.Fatalf("reproxy kept the dead pin %s, want the shared live exit", got)
	}
	if a.proxy == dead {
		t.Fatalf("lane still pinned to dropped exit %s", dead)
	}

	// the fixed branch: same pin back means move lanes, not re-dial.
	if laneReproxy(a, "m") == dead {
		a = laneFailover(a)
		laneRotate(a, "m")
	}
	if a.proxy == dead {
		t.Fatalf("lane still pinned to dropped exit %s", dead)
	}
	if a == nil {
		t.Fatal("failover must never be nil")
	}
	_ = time.Now // keep the import honest if the body above shrinks
}
