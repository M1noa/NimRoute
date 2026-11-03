package main

import (
	"reflect"
	"testing"
)

// wrouteName/buildWrouteIndex own the grouping (which patches fold into one
// line, which lines stay apart); resolve owns what a requested key hits.
func TestWrouteName(t *testing.T) {
	cases := []struct{ id, want string }{
		{"deepseek-v4.1-flash-free", "deepseek-v4-flash"},
		{"deepseek-v4-flash-0731", "deepseek-v4-flash"},
		{"deepseek-v4-flash", "deepseek-v4-flash"},
		{"deepseek-flash", "deepseek-flash"},
		{"glm-4.6v-flash", "glm-4-flash"},
		{"glm-5.3-flash-search", "glm-5-flash-search"},
		{"gemma-4-26b-a4b-it", "gemma-4-26b-a4b-it"},
		{"google/gemma-4-26b-a4b-it:free", "gemma-4-26b-a4b-it"},
		{"mistral-small-3.2", "mistral-small-3"},
		{"mistral-high-3-675b-instruct-2512", "mistral-high-3-675b-instruct"},
		{"nemotron-3.5-super-vl-free", "nemotron-3-super-vl"},
		{"whisper-large-v3-turbo", "whisper-large-v3-turbo"},
		// the version is glued to the name here, so it stays part of it
		{"qwen3.6-27b", "qwen3.6-27b"},
		// a name made only of versions has no wide route
		{"v4", ""},
		{"2512", ""},
	}
	for _, c := range cases {
		if got := wrouteName(c.id); got != c.want {
			t.Errorf("wrouteName(%q) = %q, want %q", c.id, got, c.want)
		}
	}
}

// wrouteLabel names a row after the version its models share, so a row whose
// models are all 2.6 lists as 2.6 and only a row that mixes patches falls back
// to the line.
func TestWrouteLabel(t *testing.T) {
	backends := []string{"antseed", "uno", "opencode"}
	cases := []struct {
		name string
		ids  []string
		want string
	}{
		{"one patch everywhere keeps it", []string{"mimo-v2.6-flash-free", "mimo-v2.6-flash"}, "mimo-v2.6-flash"},
		{"patches split, so the line", []string{"deepseek-v4.1-flash-free", "deepseek-v4-flash", "deepseek-v4-flash-0731"}, "deepseek-v4-flash"},
		{"one 5 patch", []string{"glm-5.3-flash-free", "glm-5.3-flash"}, "glm-5.3-flash"},
		{"4.x builds share 4", []string{"glm-4.6v-flash", "glm-4.5-flash", "glm-4.7-flash"}, "glm-4-flash"},
		{"one minor everywhere keeps it", []string{"laguna-s-2.1:free", "laguna-s-2.1"}, "laguna-s-2.1"},
		{"no version at all", []string{"north-mini-code:free", "north-mini-code"}, "north-mini-code"},
		{"only dated builds", []string{"foo-0731", "foo-2512"}, "foo"},
	}
	for _, c := range cases {
		ms := make([]routeModel, 0, len(c.ids))
		for i, id := range c.ids {
			ms = append(ms, routeModel{backend: backends[i%len(backends)], id: id})
		}
		if got := wrouteLabel(ms); got != c.want {
			t.Errorf("%s: wrouteLabel = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestWrouteRowsFoldVersions(t *testing.T) {
	cat := []routeModel{
		{backend: "antseed", id: "deepseek-v4-flash-free"},
		{backend: "antseed", id: "deepseek-flash"},
		{backend: "antseed", id: "deepseek-v4-flash"},
		{backend: "antseed", id: "deepseek-v4-flash-0731"},
		{backend: "opencode", id: "deepseek-v4.1-flash-free"},
		{backend: "freepi", id: "deepseek-v4.1-flash"},
		{backend: "nous", id: "unrelated-model"},
	}
	ix := buildWrouteIndex(cat)
	if len(ix.rows) != 1 {
		t.Fatalf("rows = %v, want one fold over every deepseek v4 flash patch", ix.rows)
	}
	want := []string{
		"opencode/deepseek-v4.1-flash-free",
		"antseed/deepseek-v4-flash-0731",
		"antseed/deepseek-v4-flash",
		"antseed/deepseek-v4-flash-free",
		"freepi/deepseek-v4.1-flash",
	}
	if got := ix.rows["deepseek-v4-flash"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("deepseek-v4-flash row = %v, want %v", got, want)
	}
	if got := ix.labels["deepseek-v4-flash"]; got != "deepseek-v4-flash" {
		t.Errorf("deepseek-v4-flash label = %q, want the shared v4 line", got)
	}
	// every patch spelling resolves to that one row
	for _, key := range []string{"deepseek-v4.1-flash", "deepseek-v4-flash", "deepseek-v4-flash-free"} {
		if got := ix.resolve(key); !reflect.DeepEqual(got, want) {
			t.Errorf("resolve(%q) = %v, want the same wide row", key, got)
		}
	}
	// the unversioned alias is its own line, not a fourth patch of v4
	if got := ix.resolve("deepseek-flash"); !reflect.DeepEqual(got, []string{"antseed/deepseek-flash"}) {
		t.Errorf("resolve(deepseek-flash) = %v, want just the unversioned id", got)
	}
}

func TestWrouteRowsSeparateLines(t *testing.T) {
	cat := []routeModel{
		{backend: "opencode", id: "glm-5.3-flash-free"},
		{backend: "antseed", id: "glm-5.3-flash"},
		{backend: "antseed", id: "glm-4.7-flash"},
		{backend: "uno", id: "glm-5.3:free"},
		{backend: "uno", id: "glm-4:free"},
	}
	ix := buildWrouteIndex(cat)
	if got := len(ix.rows["glm-5-flash"]); got != 2 {
		t.Fatalf("rows = %v, want the two 5.3 flash ids under glm-5-flash", ix.rows)
	}
	if got := ix.labels["glm-5-flash"]; got != "glm-5.3-flash" {
		t.Errorf("glm-5-flash label = %q, want every member's own 5.3", got)
	}
	for _, name := range []string{"glm", "glm-flash", "glm-5.3-flash"} {
		if _, ok := ix.rows[name]; ok {
			t.Errorf("invented row %q = %v", name, ix.rows[name])
		}
	}
	if got := ix.resolve("glm-4.7-flash"); !reflect.DeepEqual(got, []string{"antseed/glm-4.7-flash"}) {
		t.Errorf("resolve(glm-4.7-flash) = %v, want only the 4.7 build", got)
	}
	if got := ix.resolve("glm-5.3"); !reflect.DeepEqual(got, []string{"uno/glm-5.3:free"}) {
		t.Errorf("resolve(glm-5.3) = %v, want only the plain 5.3 id", got)
	}

	// patches of one line are one row: 5 and 5.3 fold, 4 stays out
	ix = buildWrouteIndex([]routeModel{
		{backend: "uno", id: "glm-5.3:free"},
		{backend: "uno", id: "glm-5:free"},
		{backend: "uno", id: "glm-4:free"},
		{backend: "uno", id: "glm-4.6:free"},
	})
	for name, want := range map[string]int{"glm-5": 2, "glm-4": 2} {
		if got := len(ix.rows[name]); got != want {
			t.Errorf("%s row = %v, want %d ids", name, ix.rows[name], want)
		}
	}
	if got := ix.resolve("glm-5.3"); len(got) != 2 {
		t.Errorf("resolve(glm-5.3) = %v, want the glm-5 row", got)
	}
}

func TestWrouteNoMergeAcrossNames(t *testing.T) {
	// two one-off builds under a shared first token are not one model: no row
	// may invent a "claude" out of sonnet and haiku, but asking for the base
	// name still widens to the variants that sit under it.
	cat := []routeModel{
		{backend: "antseed", id: "claude-sonnet-5.5"},
		{backend: "antseed", id: "claude-haiku-5.5"},
		{backend: "uno", id: "atria-dawn-preview:free"},
		{backend: "uno", id: "atria-dawn:free"},
	}
	ix := buildWrouteIndex(cat)
	if len(ix.rows) != 0 {
		t.Fatalf("rows = %v, want none: one build per name is not a wide route", ix.rows)
	}
	if got := ix.resolve("claude"); len(got) != 2 {
		t.Errorf("resolve(claude) = %v, want both one-off builds", got)
	}
	if got := ix.resolve("atria-dawn"); len(got) != 2 {
		t.Errorf("resolve(atria-dawn) = %v, want both variants", got)
	}
}

func TestWrouteResolveSmallFamily(t *testing.T) {
	// separate fine-tunes are separate names, but asking for the base name
	// still reaches all of them; asking for one tuned build reaches only it.
	cat := []routeModel{
		{backend: "antseed", id: "gemma-4-26b-free"},
		{backend: "uno", id: "gemma-4-26b:free"},
		{backend: "opencode", id: "gemma-4-26b-a4b-it-moonlight-free"},
		{backend: "uno", id: "gemma-4-26b-a4b-it:free"},
		{backend: "uno", id: "gemma-4-26b-a4b-it-chimerax:free"},
	}
	ix := buildWrouteIndex(cat)
	if len(ix.rows["gemma-4-26b"]) != 2 {
		t.Fatalf("rows = %v, want the plain gemma-4-26b twins", ix.rows)
	}
	if _, ok := ix.rows["gemma-4-26b-a4b-it"]; ok {
		t.Fatalf("rows = %v, a single tuned build is not a wide route", ix.rows)
	}
	// the listed row is exactly what a call to that id resolves to
	if got := ix.resolve("gemma-4-26b"); len(got) != 2 {
		t.Errorf("resolve(gemma-4-26b) = %v, want the listed row", got)
	}
	if got := ix.resolve("gemma-4-26b-a4b-it"); len(got) != 3 {
		t.Errorf("resolve(gemma-4-26b-a4b-it) = %v, want just the a4b-it builds", got)
	}
	if got := ix.resolve("gemma-4-26b-a4b-it-moonlight"); len(got) != 1 {
		t.Errorf("resolve(gemma-4-26b-a4b-it-moonlight) = %v, want only that build", got)
	}
	if got := ix.resolve("nope-nothing"); len(got) != 0 {
		t.Errorf("resolve(unknown) = %v, want nothing", got)
	}
}

// wrouteOrder weighs provider rank heaviest: a free row on a later backend
// does not jump a paid row on an earlier one, and free leads inside a
// provider. freepi's rows are free by construction, so class alone used to
// put them second; the provider ladder now keeps them last.
func TestWrouteOrderProviderWeight(t *testing.T) {
	ms := []routeModel{
		{backend: "freepi", id: "glm-5.3-flash"},
		{backend: "uno", id: "glm-5.3-flash"},
		{backend: "uno", id: "glm-5.3-flash:free"},
	}
	want := []string{
		"uno/glm-5.3-flash:free",
		"uno/glm-5.3-flash",
		"freepi/glm-5.3-flash",
	}
	if got := wrouteOrder(ms, "glm-5-flash"); !reflect.DeepEqual(got, want) {
		t.Fatalf("wrouteOrder = %v, want %v", got, want)
	}
}

// wrouteSnapshot owns the listing; wrouteRedundant decides whether a wide row
// is just an exact route row under a second name.
func TestWrouteRedundant(t *testing.T) {
	exact := map[string]string{
		"opencode": "beam-free",
		"uno":      "beam:free",
	}
	cases := []struct {
		name   string
		models []string
		exact  map[string]string
		want   bool
	}{
		{"same set, different order", []string{"uno/beam:free", "opencode/beam-free"}, exact, true},
		{"extra model from a listed backend", []string{"opencode/beam-free", "uno/beam:free", "uno/beam-v2:free"}, exact, false},
		{"different model", []string{"opencode/beam-free", "uno/beam-next:free"}, exact, false},
		{"no route row", []string{"opencode/beam-free", "uno/beam:free"}, nil, false},
		{"single backend route", []string{"opencode/beam-free"}, map[string]string{"opencode": "beam-free"}, true},
	}
	for _, c := range cases {
		if got := wrouteRedundant(c.models, c.exact); got != c.want {
			t.Errorf("%s: wrouteRedundant = %v, want %v", c.name, got, c.want)
		}
	}
}
