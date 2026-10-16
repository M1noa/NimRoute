package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sseUsageTee pulls the usage chunk out of a chat sse stream.
func TestSseUsageTee(t *testing.T) {
	sse := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20,\"total_tokens\":120}}\n\n" +
		"data: [DONE]\n\n"
	p, c := sseUsageTee([]byte(sse))
	if p != 100 || c != 20 {
		t.Fatalf("got prompt=%d comp=%d, want 100/20", p, c)
	}
	if p, c := sseUsageTee([]byte("data: [DONE]\n\n")); p != 0 || c != 0 {
		t.Fatalf("empty stream gave %d/%d, want 0/0", p, c)
	}
}

// logUsage disabled writes nothing and bumps no counters.
func TestLogUsageDisabled(t *testing.T) {
	c := testConfig()
	c.Usage.Enabled = false
	useConfig(t, c)
	before, _, _, _ := usageSnapshot()
	logUsage(UsageRecord{Model: "m", PromptTokens: 5, CompletionTokens: 3})
	after, _, _, _ := usageSnapshot()
	if before != after {
		t.Fatalf("disabled logUsage bumped counters: %d -> %d", before, after)
	}
}

// logUsage enabled appends one json line and aggregates per model.
func TestLogUsageFileAndTotals(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "u.jsonl")
	c := testConfig()
	c.Usage.Enabled = true
	c.Usage.Path = path
	useConfig(t, c)
	initUsageLog()
	t.Cleanup(closeUsageLog)
	totalsMu.Lock()
	totals = map[string]*usageTotals{}
	totalsMu.Unlock()

	logUsage(UsageRecord{
		Ts: "t", Model: "m1", Method: "POST", Path: "/v1/chat/completions",
		StatusCode: 200, PromptTokens: 10, CompletionTokens: 4, TotalTokens: 14,
	})
	logUsage(UsageRecord{
		Ts: "t", Model: "m1", Method: "POST", Path: "/v1/chat/completions",
		StatusCode: 500, Error: "boom",
	})

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 jsonl lines, got %d", len(lines))
	}
	var r UsageRecord
	if err := json.Unmarshal([]byte(lines[0]), &r); err != nil {
		t.Fatal(err)
	}
	if r.Model != "m1" || r.PromptTokens != 10 || r.TotalTokens != 14 {
		t.Fatalf("bad record: %+v", r)
	}

	reqs, prompt, comp, byModel := usageSnapshot()
	if reqs < 2 || prompt < 10 || comp < 4 {
		t.Fatalf("bad totals: reqs=%d prompt=%d comp=%d", reqs, prompt, comp)
	}
	if byModel["m1"].Requests < 2 || byModel["m1"].Errors < 1 {
		t.Fatalf("bad per-model: %+v", byModel["m1"])
	}
}

// /status carries usage totals when enabled, hides them when off.
func TestStatusUsage(t *testing.T) {
	c := testConfig()
	c.Usage.Enabled = true
	c.Status.ShowUsage = true
	useConfig(t, c)
	totalsMu.Lock()
	totals = map[string]*usageTotals{"m1": {Requests: 2, Prompt: 10, Completion: 4}}
	totalsMu.Unlock()
	p := newPool(nil)
	sr := p.StatusFor(true)
	if sr.Usage == nil || sr.Usage.Requests != 2 || sr.Usage.Prompt != 10 {
		t.Fatalf("bad status usage: %+v", sr.Usage)
	}

	off := testConfig()
	off.Usage.Enabled = false
	off.Status.ShowUsage = true
	useConfig(t, off)
	if sr := p.StatusFor(true); sr.Usage != nil {
		t.Fatalf("usage shown while disabled: %+v", sr.Usage)
	}
}

// outcomesByModel folds overflow into (other) exactly like totals does, so
// a loop of unique client-chosen model ids stops growing model health (and
// the /status health payload) instead of leaking a ring per id forever.
func TestOutcomesByModelCapped(t *testing.T) {
	dir := t.TempDir()
	c := testConfig()
	c.Usage.Enabled = true
	c.Usage.Path = filepath.Join(dir, "u.jsonl")
	useConfig(t, c)
	initUsageLog()
	t.Cleanup(closeUsageLog)
	totalsMu.Lock()
	oldT, oldO, oldR := totals, outcomesByModel, removedByModel
	totals, outcomesByModel, removedByModel =
		map[string]*usageTotals{}, map[string][]modelOutcome{}, map[string]string{}
	totalsMu.Unlock()
	t.Cleanup(func() {
		totalsMu.Lock()
		totals, outcomesByModel, removedByModel = oldT, oldO, oldR
		totalsMu.Unlock()
	})

	hammer := func(from, to int) {
		for i := from; i < to; i++ {
			logUsage(UsageRecord{
				Model: fmt.Sprintf("nvidia/x%d", i), StatusCode: 500,
				KeyName: "k", DurationMs: 1,
			})
		}
	}
	over := usageModelCap + 250
	hammer(0, over)

	totalsMu.Lock()
	gotTotals, gotOutcomes := len(totals), len(outcomesByModel)
	totalsMu.Unlock()
	if gotTotals > usageModelCap+1 {
		t.Fatalf("totals grew past the cap: %d entries, want <= %d", gotTotals, usageModelCap+1)
	}
	if gotOutcomes > usageModelCap+1 {
		t.Fatalf("outcomesByModel grew past the cap: %d entries, want <= %d", gotOutcomes, usageModelCap+1)
	}
	if _, ok := outcomesByModel["(other)"]; !ok {
		t.Fatal("overflow ids did not fold into the (other) health bucket")
	}
	if _, ok := totals["nvidia/x0"]; !ok {
		t.Fatal("ids under the cap must stay individually tracked")
	}

	// /status serializes modelHealthSnapshot: past the cap the payload must
	// stay flat while more unique ids arrive.
	before, err := json.Marshal(modelHealthSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	hammer(over, over+250)
	after, err := json.Marshal(modelHealthSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("health payload grew: %d -> %d bytes after 250 more unique ids", len(before), len(after))
	}
	totalsMu.Lock()
	late := len(outcomesByModel)
	totalsMu.Unlock()
	if late != gotOutcomes {
		t.Fatalf("outcomesByModel kept growing: %d -> %d entries", gotOutcomes, late)
	}
}

// the ring map caps itself: unique ids recorded straight through
// noteModelOutcome (no totals fold to lean on) stop adding entries and fold
// into (other), so one ring per id never accumulates.
func TestNoteModelOutcomeCap(t *testing.T) {
	totalsMu.Lock()
	old := outcomesByModel
	outcomesByModel = map[string][]modelOutcome{}
	totalsMu.Unlock()
	t.Cleanup(func() {
		totalsMu.Lock()
		outcomesByModel = old
		totalsMu.Unlock()
	})

	for i := 0; i < usageModelCap+50; i++ {
		noteModelOutcome(fmt.Sprintf("nvidia/y%d", i), false, 500, "s1")
	}
	totalsMu.Lock()
	got := len(outcomesByModel)
	totalsMu.Unlock()
	if got > usageModelCap+1 {
		t.Fatalf("outcomesByModel grew to %d entries, want <= %d", got, usageModelCap+1)
	}
	if _, ok := outcomesByModel["(other)"]; !ok {
		t.Fatal("overflow ids did not fold into the (other) health bucket")
	}
	if n := len(outcomesByModel["(other)"]); n == 0 {
		t.Fatal("folded bucket carries no outcomes")
	}
}

// usage defaults stay on for old configs without the section.
func TestUsageDefaultsOn(t *testing.T) {
	d := defaultConfig()
	if !d.Usage.Enabled || d.Usage.Path == "" || !d.Status.ShowUsage {
		t.Fatalf("bad defaults: %+v %+v", d.Usage, d.Status)
	}
	c, err := loadConfigFile("config.yml.example")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Usage.Enabled || c.Usage.Path == "" {
		t.Fatalf("example must enable usage: %+v", c.Usage)
	}
}
