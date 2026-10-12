package main

import (
	"testing"
)

// per-lane token totals accumulate per model and survive a rotation,
// which mints a new session on the same lane object.
func TestLaneTokensAccumulatePerModel(t *testing.T) {
	resetLanes()
	defer resetLanes()
	c := testConfig()
	c.Zen.Lanes = 3
	useConfig(t, c)
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	l := laneFor("tok-1", body)

	laneTouch(l, "http://10.0.0.1:1", "space-bunny-free")
	laneTokensAdd(l, "space-bunny-free", 100, 50, 150, 0)
	laneTouch(l, "http://10.0.0.1:1", "space-bunny-free")
	laneTokensAdd(l, "space-bunny-free", 20, 10, 30, 0)
	// a second model on the same lane must not merge into the first
	laneTouch(l, "http://10.0.0.1:1", "big-pickle")
	laneTokensAdd(l, "big-pickle", 7, 3, 10, 0)

	// rotation: new session, same lane object, totals must persist
	before := laneSession(l)
	laneRotate(l, "")
	if laneSession(l) == before {
		t.Fatal("rotate must mint a new session")
	}
	laneTokensAdd(l, "space-bunny-free", 1, 1, 2, 0)

	snap := laneSnapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot has %d lanes, want 1", len(snap))
	}
	st := snap[0]
	if st.Tokens != 192 {
		t.Errorf("total tokens = %d, want 192 (150+30+10+2)", st.Tokens)
	}
	if st.Prompt != 128 || st.Completion != 64 {
		t.Errorf("prompt/completion = %d/%d, want 128/64", st.Prompt, st.Completion)
	}
	if st.Requests != 3 {
		t.Errorf("requests = %d, want 3", st.Requests)
	}
	bm := st.ByModel["space-bunny-free"]
	if bm.Requests != 2 || bm.Total != 182 {
		t.Errorf("space-bunny = %+v, want 2 requests / 182 tokens", bm)
	}
	if bp := st.ByModel["big-pickle"]; bp.Total != 10 {
		t.Errorf("big-pickle total = %d, want 10", bp.Total)
	}
}

// total falls back to prompt+completion when upstream omits it.
func TestLaneTokensTotalFallsBack(t *testing.T) {
	resetLanes()
	defer resetLanes()
	l := laneFor("tok-2", []byte(`{"messages":[{"role":"user","content":"x"}]}`))
	laneTokensAdd(l, "m", 30, 12, 0, 0)
	lanesMu.Lock()
	defer lanesMu.Unlock()
	if got := l.tokens["m"].Total; got != 42 {
		t.Fatalf("total = %d, want 42 (prompt+completion fallback)", got)
	}
}

// zero-token turns (upstream reported no usage) must not create noise,
// but the request count still moves.
func TestLaneTokensIgnoresEmptyTurn(t *testing.T) {
	resetLanes()
	defer resetLanes()
	l := laneFor("tok-3", []byte(`{"messages":[{"role":"user","content":"y"}]}`))
	laneTokensAdd(l, "m", 0, 0, 0, 0)
	laneTokensAdd(nil, "m", 5, 5, 10, 0) // nil lane must not panic
	laneTokensAdd(l, "", 5, 5, 10, 0)    // no model must not record
	lanesMu.Lock()
	defer lanesMu.Unlock()
	m := l.tokens["m"]
	if m.Requests != 0 || m.Total != 0 {
		t.Fatalf("empty turn recorded %+v, want nothing", m)
	}
}
