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
