package main

import (
	"encoding/json"
	"os"
	"testing"
)

// loadModelInfo parses path into a lookup map, empty on any failure. test
// helper only: production goes through loadModelInfoKeep, which keeps the
// live catalog when the file is broken.
func loadModelInfo(path string) map[string]modelInfoEntry {
	raw, err := os.ReadFile(path)
	if err != nil {
		return map[string]modelInfoEntry{}
	}
	var m map[string]modelInfoEntry
	if err := json.Unmarshal(stripJSONComments(raw), &m); err != nil {
		return map[string]modelInfoEntry{}
	}
	return m
}

// globMatch anchors both ends, so a pattern matches what a glob should mean:
// claude-* never catches my-claude-x, and a wildcard-free pattern is exact
// rather than a substring. every config surface that reads a pattern
// (route.exclude_models, models.claude_map, models.nudge) skips or guards the
// empty pattern, so an empty pattern matching nothing is safe. owner: the
// three readers listed above.
func TestGlobMatchAnchored(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"claude-opus-4", "claude-opus-4", true},
		{"claude-opus-4", "claude-opus-45", false},
		{"claude-opus-4", "xxclaude-opus-4", false},
		{"claude-*", "claude-sonnet-4", true},
		{"claude-*", "my-claude-x", false}, // the first part is a prefix, not a substring
		{"*mid*", "a-mid-b", true},
		{"*mid*", "amidb", true}, // a * may match nothing
		{"a*c", "ac", true},
		{"a*c", "abc", true},
		{"*-suffix", "prefix-suffix", true},
		{"*-suffix", "suffix", false},
		{"spark", "muse-spark-x", false}, // no star means exact, not "contains"
		{"spark", "spark", true},
		{"", "", true},
		{"", "anything", false},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "axxc", false},
		{"*", "anything-at-all", true},
		{"*", "", true},
		{"*-free", "opencode/muse-spark-free", true},
	}
	for _, tc := range cases {
		if got := globMatch(tc.pattern, tc.s); got != tc.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tc.pattern, tc.s, got, tc.want)
		}
	}
}

// owner: model_info.jsonc. the catalog is hand-maintained, so guard the fact
// the listing depends on: an output ceiling can never exceed the context
// window. ctx 0 means "not published" (embedding/reward/ocr entries), so the
// relation is only checkable when ctx is known.
func TestModelInfoCatalogInvariants(t *testing.T) {
	m := loadModelInfo(modelInfoPath())
	if len(m) == 0 {
		t.Fatal("catalog did not load")
	}
	for id, e := range m {
		if e.Ctx < 0 || e.MaxOut < 0 {
			t.Errorf("%s: negative ctx/maxout (%d/%d)", id, e.Ctx, e.MaxOut)
		}
		if e.Ctx > 0 && e.MaxOut > e.Ctx {
			t.Errorf("%s: maxout %d exceeds ctx %d", id, e.MaxOut, e.Ctx)
		}
	}
}
