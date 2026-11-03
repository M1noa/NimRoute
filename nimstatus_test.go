package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// dashModel.Update owns refresh scheduling: dashTickMsg is the only message
// that may arm the next tick. a failed /status that also armed one added a
// second poll chain, and every later failure doubled the timers again, so the
// dashboard got slower the longer it ran.
func TestDashUpdateArmsOneTickChain(t *testing.T) {
	cases := []struct {
		name string
		msg  tea.Msg
		tick bool
	}{
		{"tick arms the next poll", dashTickMsg{}, true},
		{"fetch error arms nothing", dashMsg{err: errors.New("timeout")}, false},
		{"good fetch arms nothing", dashMsg{d: &StatusResponse{}}, false},
	}
	for _, c := range cases {
		m := &dashModel{d: &StatusResponse{}}
		_, cmd := m.Update(c.msg)
		if got := cmd != nil; got != c.tick {
			t.Errorf("%s: scheduled=%v, want %v", c.name, got, c.tick)
		}
	}
}

// the in-flight flag is what keeps a slow /status (3s client timeout) from
// stacking a second fetch on the next 2s tick.
func TestDashUpdateTracksFetchInFlight(t *testing.T) {
	m := &dashModel{}
	if _, cmd := m.Update(dashTickMsg{}); cmd == nil || !m.loading {
		t.Fatalf("tick: scheduled=%v loading=%v, want a fetch and the flag set", cmd != nil, m.loading)
	}
	if _, cmd := m.Update(dashTickMsg{}); cmd == nil || !m.loading {
		t.Fatalf("tick while loading: scheduled=%v loading=%v, want the next tick kept and no second fetch", cmd != nil, m.loading)
	}
	m.Update(dashMsg{err: errors.New("timeout")})
	if m.loading {
		t.Error("a failed fetch must clear the in-flight flag")
	}
	m.Update(dashTickMsg{})
	m.Update(dashMsg{d: &StatusResponse{}})
	if m.loading {
		t.Error("a good fetch must clear the in-flight flag")
	}
}

// usageTotalsFor owns the incremental scan: a day rollover resets today's
// window but must not re-read the whole log, which stalls the dashboard for
// seconds on a large file.
func TestUsageDayRollKeepsOffset(t *testing.T) {
	statsCache = struct {
		path  string
		dev   uint64
		ino   uint64
		day   string
		size  int64
		off   int64
		st    nimStats
		valid bool
	}{}
	defer func() { statsCache.valid = false }()

	path := filepath.Join(t.TempDir(), "nim-usage.jsonl")
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	line := `{"ts":"` + ts + `","model":"opencode/x","total_tokens":10}` + "\n"
	if err := os.WriteFile(path, []byte(line+line), 0o600); err != nil {
		t.Fatal(err)
	}
	st := usageTotalsFor(path)
	if st.allReq != 2 || st.todayReq != 2 {
		t.Fatalf("first scan: all=%d today=%d, want 2/2", st.allReq, st.todayReq)
	}
	size := statsCache.off

	statsCache.day = "1999-12-31" // pretend the day rolled over
	st = usageTotalsFor(path)
	if st.allReq != 2 {
		t.Errorf("all-time totals = %d, want the 2 already counted", st.allReq)
	}
	if st.todayReq != 0 {
		t.Errorf("today's requests = %d, want 0 on the new day", st.todayReq)
	}
	if statsCache.off != size {
		t.Errorf("scan offset = %d, want %d (no re-read)", statsCache.off, size)
	}
}
