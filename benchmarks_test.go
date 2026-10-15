package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// aaFixture builds a bundle in the exact on-disk layout which-llm publishes:
// each dataset header starts a line, rows are one per line, and a block
// closes with "]}" appended to its last row so the next header lands on a
// fresh line.
func aaFixture(updated string, rows ...string) string {
	var b strings.Builder
	b.WriteString("{\"schema_version\":1,\"refresh_errors\":{},\"datasets\":{\n")
	b.WriteString("\"capabilities\":{\"source_url\": \"https://artificialanalysis.ai\", \"source_updated_at_utc\": \"2026-01-01T00:00:00Z\",\"rows\":[\n")
	b.WriteString("{\"slug\":\"ignored-capability-row\",\"intelligenceIndex\":1.0}]},\n")
	b.WriteString("\"catalog\":{\"source_url\": \"https://artificialanalysis.ai/models\", \"source_updated_at_utc\": \"" + updated + "\",\"rows\":[\n")
	b.WriteString(strings.Join(rows, ",\n"))
	b.WriteString("]},\n")
	b.WriteString("\"providers\":{\"source_url\": \"x\", \"source_updated_at_utc\": \"" + updated + "\",\"rows\":[\n")
	b.WriteString("{\"slug\":\"ignored-provider-row\",\"intelligenceIndex\":2.0}]}}\n")
	return b.String()
}

func row(slug, family string, idx float64) string {
	return `{"slug":"` + slug + `","name":"` + slug + `","release":{"slug":"` + family +
		`"},"creator":{"slug":"x","name":"X"},"intelligenceIndex":` +
		strings.TrimSuffix(strings.TrimSpace(fmtFloat(idx)), ".") + `}`
}

func fmtFloat(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// TestScanAACatalog pins the line-oriented read: only the catalog block is
// kept, rows from neighbouring datasets are dropped, and the source date
// comes from that block's own header.
func TestScanAACatalog(t *testing.T) {
	body := aaFixture("2026-10-11T07:38:14Z",
		row("glm-5-3-flash", "glm-5-3", 41.8),
		row("kimi-k3", "kimi-k3", 43.5))
	rows, stamp, err := scanAACatalog(strings.NewReader(body))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if stamp != "2026-10-11T07:38:14Z" {
		t.Fatalf("stamp = %q", stamp)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (capabilities/providers must not leak in)", len(rows))
	}
	if rows[0].Slug != "glm-5-3-flash" || rows[1].Slug != "kimi-k3" {
		t.Fatalf("rows = %q, %q", rows[0].Slug, rows[1].Slug)
	}
	if rows[1].IntelligenceIndex == nil || *rows[1].IntelligenceIndex != 43.5 {
		t.Fatalf("index = %v", rows[1].IntelligenceIndex)
	}
}

func TestScanAACatalogEmpty(t *testing.T) {
	rows, _, err := scanAACatalog(strings.NewReader(`{"datasets":{}}`))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %d, want 0", len(rows))
	}
}

// TestStripModelVariants covers the free-backend suffixes: the serving id
// keeps them, the aa lookup must not.
func TestStripModelVariants(t *testing.T) {
	cases := []struct{ in, want string }{
		{"mimo-v2-6-flash-free", "mimo-v2-6-flash"},
		{"nemotron-3-nano-omni-30b-a3b-reasoning", "nemotron-3-nano-omni-30b-a3b"},
		{"gpt-oss-20b-high", "gpt-oss-20b"},
		{"gemini-3-pro-low", "gemini-3-pro"},
		{"glm-5-3-flash", "glm-5-3-flash"},
		// a short core must not decay: "gemma-4" keeps its -4.
		{"gemma-4", "gemma-4"},
		// double suffix: -reasoning on top of a variant.
		{"deepseek-v4-flash-free", "deepseek-v4-flash"},
		// opencode's free-tier branding: the model is muse-spark 1.3.
		{"muse-spark-1-3-contributor", "muse-spark-1-3"},
		{"muse-spark-1-3-contributor-free", "muse-spark-1-3"},
	}
	for _, c := range cases {
		if got := stripModelVariants(c.in); got != c.want {
			t.Errorf("stripModelVariants(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormModelID(t *testing.T) {
	cases := []struct{ in, want string }{
		{"mimo-v2.6-flash", "mimo-v2-6-flash"},
		// "/" survives: both catalogs mean org/name by it.
		{"z-ai/glm-5.3-flash", "z-ai/glm-5-3-flash"},
		{"GLM_5_3", "glm-5-3"},
		{"  claude-opus-5-5  ", "claude-opus-5-5"},
		{"a--b", "a-b"},
	}
	for _, c := range cases {
		if got := normModelID(c.in); got != c.want {
			t.Errorf("normModelID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestBenchMatchKind is the guard on fuzzy matching. a hit found by stripping
// a serving suffix must report "slug", and a family fallback must report
// "family", so /status and /v1/models never present weak evidence as exact.
func TestBenchMatchKind(t *testing.T) {
	ix := buildAAIndex([]aaRow{
		{Slug: "glm-5-3-flash", IntelligenceIndex: ptr(41.8)},
		{Slug: "gpt-oss-20b", IntelligenceIndex: ptr(8.97)},
		{Slug: "gpt-oss-20b-high", IntelligenceIndex: ptr(20.0)},
		{Slug: "kimi-k3-low", Release: struct {
			Slug string `json:"slug"`
		}{Slug: "kimi-k3"}, IntelligenceIndex: ptr(30.0)},
		{Slug: "kimi-k3-high", Release: struct {
			Slug string `json:"slug"`
		}{Slug: "kimi-k3"}, IntelligenceIndex: ptr(43.5)},
		// no score: cannot be a match target.
		{Slug: "scored-nothing"},
	}, "2026-10-11T00:00:00Z")
	setBenchIndex(ix, nil)
	defer setBenchIndex(nil, nil)

	cases := []struct {
		id, kind string
		idx      float64
	}{
		{"z-ai/glm-5.3-flash", "slug", 41.8},
		{"freeepi/glm-5.3-flash", "slug", 41.8},
		{"opencode/gpt-oss-20b-free", "slug", 8.97},
		{"openai/gpt-oss-20b-high", "slug", 20.0},
		{"moonshotai/kimi-k3", "family", 43.5},
		{"nothing/here", "", 0},
		{"opencode/scored-nothing", "", 0},
	}
	for _, c := range cases {
		m := benchFor(c.id)
		if m.Kind != c.kind {
			t.Errorf("benchFor(%q).Kind = %q, want %q", c.id, m.Kind, c.kind)
		}
		if m.ok() && m.index() != c.idx {
			t.Errorf("benchFor(%q).index() = %v, want %v", c.id, m.index(), c.idx)
		}
	}
}

// TestBenchMatchExactNeedsOrglessID proves the "exact" kind is not handed out
// for a prefix-stripped guess: an aa slug carrying a slash is only matched
// exactly.
func TestBenchMatchExactNeedsOrglessID(t *testing.T) {
	ix := buildAAIndex([]aaRow{
		{Slug: "moonshotai/kimi-k3", IntelligenceIndex: ptr(43.5)},
	}, "")
	setBenchIndex(ix, nil)
	defer setBenchIndex(nil, nil)

	if got := benchFor("moonshotai/kimi-k3").Kind; got != "exact" {
		t.Errorf("Kind = %q, want exact", got)
	}
	if got := benchFor("freeepi/moonshotai/kimi-k3").Kind; got == "exact" {
		t.Error("a freeepi-prefixed id must not claim an exact match")
	}
}

func TestSortBenchRows(t *testing.T) {
	rows := []benchRow{
		{ID: "b", Intel: 10}, {ID: "a", Intel: 41.8}, {ID: "c", Intel: 10},
	}
	sortBenchRows(rows)
	if rows[0].ID != "a" || rows[1].ID != "b" || rows[2].ID != "c" {
		t.Fatalf("order = %v, %v, %v", rows[0].ID, rows[1].ID, rows[2].ID)
	}
}

// TestAACacheRoundTrip covers the boot path: a warm cache loads without the
// network, a stale one is refused.
func TestAACacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aa.json")
	rows := []aaRow{{Slug: "glm-5-3-flash", IntelligenceIndex: ptr(41.8)}}
	writeAACache(path, rows, "2026-10-11T00:00:00Z")

	ix := readAACache(path, 1e9)
	if ix == nil || len(ix.bySlug) != 1 {
		t.Fatalf("cache load: %v", ix)
	}
	if ix.updated != "2026-10-11T00:00:00Z" {
		t.Fatalf("updated = %q", ix.updated)
	}
	if readAACache(path, 0) == nil {
		t.Fatal("maxAge 0 must mean any age, not expired")
	}
	if readAACache(path, 1) != nil {
		t.Fatal("a 1ns maxAge must refuse the file")
	}
	if readAACache(filepath.Join(dir, "nope.json"), 0) != nil {
		t.Fatal("missing file must return nil, not an empty index")
	}
}

func ptr[T any](v T) *T { return &v }

// --- openrouter overlay ---

func orFixture() []*orModel {
	maxOut := 64000
	cut := "2025-06-01"
	return []*orModel{{
		ID:            "z-ai/glm-5.3-flash",
		Name:          "Z AI: GLM 5.3 Flash",
		Description:   "a fast one",
		ContextLength: 200000,
		Architecture: struct {
			Modality         string   `json:"modality"`
			InputModalities  []string `json:"input_modalities"`
			OutputModalities []string `json:"output_modalities"`
			Tokenizer        string   `json:"tokenizer"`
			InstructType     any      `json:"instruct_type"`
		}{Modality: "text->text", InputModalities: []string{"text"}},
		TopProvider: struct {
			ContextLength       int  `json:"context_length"`
			MaxCompletionTokens *int `json:"max_completion_tokens"`
			IsModerated         bool `json:"is_moderated"`
		}{ContextLength: 200000, MaxCompletionTokens: &maxOut},
		SupportedParameters: []string{"tools", "temperature", "reasoning_effort"},
		KnowledgeCutoff:     &cut,
		Created:             1748736000,
		Reasoning: &struct {
			Mandatory        bool     `json:"mandatory"`
			SupportedEfforts []string `json:"supported_efforts"`
			DefaultEffort    string   `json:"default_effort"`
		}{Mandatory: true, SupportedEfforts: []string{"low", "high"}},
	}, {
		ID:            "nvidia/some-nvidia-only",
		Name:          "NVIDIA: Some Model",
		ContextLength: 4096,
	}}
}

func TestORIndexMatch(t *testing.T) {
	ix := buildORIndex(orFixture())
	cases := []struct {
		id  string
		hit bool
	}{
		{"z-ai/glm-5.3-flash", true},
		{"freeepi/glm-5.3-flash", true}, // prefix we add, they do not
		{"freeepi/glm-5.3-flash[1m]", true},
		{"opencode/space-bunny-free", false},
	}
	for _, c := range cases {
		if got := ix.get(c.id) != nil; got != c.hit {
			t.Errorf("get(%q) = %v, want %v", c.id, got, c.hit)
		}
	}
}

func TestOREntryMapping(t *testing.T) {
	ix := buildORIndex(orFixture())
	e := orEntry(ix.get("z-ai/glm-5.3-flash"))
	if e.Name != "GLM 5.3 Flash" {
		t.Errorf("name = %q, want the vendor prefix stripped", e.Name)
	}
	if e.Ctx != 200000 || e.MaxOut != 64000 {
		t.Errorf("ctx/maxout = %d/%d", e.Ctx, e.MaxOut)
	}
	if !e.Tools || !e.Temp {
		t.Errorf("tools/temp = %v/%v", e.Tools, e.Temp)
	}
	if e.Attachment {
		t.Error("a text-only model must not be marked attachment-capable")
	}
	if e.Reasoning == nil || !*e.Reasoning {
		t.Error("reasoning flag not carried")
	}
	if len(e.EffortLevels) != 2 {
		t.Errorf("effort levels = %v", e.EffortLevels)
	}
	if e.Released != "2025-06-01" {
		t.Errorf("released = %q, want the created stamp as a date", e.Released)
	}
}

// TestMergeOREntryFillsGapsOnly is the core invariant: the catalog wins.
// every empty field is filled, nothing already stated is touched.
func TestMergeOREntryFillsGapsOnly(t *testing.T) {
	ix := buildORIndex(orFixture())
	src := orEntry(ix.get("z-ai/glm-5.3-flash"))

	no := false
	dst := modelInfoEntry{
		Ctx: 262144, // curated, must survive
		// Name curated, must survive.
		Name:      "GLM 5.3 Flash (free-pi)",
		Reasoning: &no, // curated false, must survive the true from OR
		MaxOut:    0,
	}
	if !mergeOREntry(&dst, src) {
		t.Fatal("expected a change: ctx/name/reasoning are all stated, maxout is not")
	}
	if dst.Ctx != 262144 {
		t.Errorf("ctx = %d, curated value was overwritten", dst.Ctx)
	}
	if dst.Name != "GLM 5.3 Flash (free-pi)" {
		t.Errorf("name = %q, curated value was overwritten", dst.Name)
	}
	if dst.Reasoning == nil || *dst.Reasoning {
		t.Error("reasoning = true, curated false was overwritten")
	}
	if dst.MaxOut != 64000 {
		t.Errorf("maxout = %d, want the gap filled", dst.MaxOut)
	}
	if dst.Source != "" {
		t.Errorf("source = %q, merge must not rewrite provenance", dst.Source)
	}
}

func TestMergeOREntryNoChange(t *testing.T) {
	ix := buildORIndex(orFixture())
	src := orEntry(ix.get("z-ai/glm-5.3-flash"))
	full := src
	if mergeOREntry(&full, src) {
		t.Fatal("a fully populated entry must report no change")
	}
}

func TestWithOROverlaySynthesizes(t *testing.T) {
	setORIndex(buildORIndex(orFixture()), nil)
	defer setORIndex(nil, nil)
	e := withOROverlay("nvidia/some-nvidia-only", nil)
	if e == nil {
		t.Fatal("an id openrouter knows must synthesize an entry")
	}
	if e.Ctx != 4096 {
		t.Errorf("ctx = %d", e.Ctx)
	}
	if withOROverlay("nothing/at-all", nil) != nil {
		t.Error("an unknown id must stay nil, not become an empty entry")
	}
}

// TestSyncModelInfoPreservesComments is why sync is line-oriented: the
// header and the section comments must survive a rewrite.
func TestSyncModelInfoPreservesComments(t *testing.T) {
	ix := buildORIndex(orFixture())
	dir := t.TempDir()
	path := filepath.Join(dir, "model_info.jsonc")
	// the catalog's shape is `"key": {…},` with a space after the colon.
	src := "// header comment\n{\n  // --- section ---\n" +
		"  \"z-ai/glm-5.3-flash\": {\"ctx\":262144,\"source\":\"nim cards\"},\n" +
		// a stated false and a "no record" [] are curated facts: sync must
		// neither overwrite nor drop them (the struct's omitempty used to).
		"  \"nvidia/some-nvidia-only\": {\"ctx\":4096,\"supports_temperature\":false,\"supported_parameters\":[]},\n" +
		"  \"unrelated/thing\": {\"ctx\":8}\n}\n"
	if os.WriteFile(path, []byte(src), 0o644) != nil {
		t.Fatal("write fixture")
	}
	n, err := syncModelInfo(path, ix)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if n != 2 {
		t.Fatalf("changed = %d, want 2", n)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, want := range []string{"// header comment", "// --- section ---"} {
		if !strings.Contains(got, want) {
			t.Errorf("comment %q was lost:\n%s", want, got)
		}
	}
	// the curated ctx stays, the gap is filled.
	if !strings.Contains(got, `"ctx":262144`) {
		t.Errorf("curated ctx lost:\n%s", got)
	}
	if !strings.Contains(got, `"maxout":64000`) {
		t.Errorf("maxout not filled:\n%s", got)
	}
	// a curated false must survive, not flip to true.
	if !strings.Contains(got, `"supports_temperature":false`) {
		t.Errorf("curated supports_temperature false was overwritten:\n%s", got)
	}
	// a [] (no record) must survive, not be replaced by openrouter's list.
	if !strings.Contains(got, `"supported_parameters":[]`) {
		t.Errorf("curated supported_parameters [] was overwritten:\n%s", got)
	}
	// the unrelated entry keeps its own fields untouched, byte for byte.
	if !strings.Contains(got, `"unrelated/thing": {"ctx":8}`) {
		t.Errorf("unrelated entry was rewritten:\n%s", got)
	}
	// the result must still parse as jsonc.
	if m := loadModelInfo(path); m == nil || len(m) != 3 {
		t.Fatalf("rewritten file does not parse: %v", m)
	}
	// idempotent: a second pass has nothing left to do.
	n2, err := syncModelInfo(path, ix)
	if err != nil || n2 != 0 {
		t.Fatalf("second sync changed %d (err %v), want 0", n2, err)
	}
}

func TestParseCatalogLine(t *testing.T) {
	cases := []struct {
		line string
		ok   bool
		key  string
	}{
		{`  "a/b":{"ctx":1},`, true, "a/b"},
		{`  "a/b": {"ctx":1},`, true, "a/b"},
		{"// a comment", false, ""},
		{"{", false, ""},
		{"", false, ""},
		{`  "no-comma": {"ctx":1}`, false, ""},
		{`  "no-comma":{"ctx":1}`, false, ""},
		{`  "not-json": {ctx},`, false, ""},
	}
	for _, c := range cases {
		key, _, ok := parseCatalogLine(c.line)
		if ok != c.ok {
			t.Errorf("parseCatalogLine(%q) ok = %v, want %v", c.line, ok, c.ok)
		}
		if ok && key != c.key {
			t.Errorf("parseCatalogLine(%q) key = %q, want %q", c.line, key, c.key)
		}
	}
}

// --- status gate ---

// TestBenchmarksStatusGate pins both halves of the /status rule from AGENTS:
// the flag hides the section, and an unauthenticated caller never sees it.
func TestBenchmarksStatusGate(t *testing.T) {
	p := newPool(nil)
	// applyConfig reloads model_info.jsonc from disk, so the fixture has to
	// land after it or the real 108-entry catalog wins.
	base := defaultConfig()
	base.Auth.Tokens = []string{"t"}
	applyConfig(&base)
	setModelInfo(map[string]modelInfoEntry{
		"z-ai/glm-5.3-flash": {Ctx: 200000, MaxOut: 64000},
	})
	setBenchIndex(buildAAIndex([]aaRow{
		{Slug: "glm-5-3-flash", IntelligenceIndex: ptr(41.8)},
	}, "2026-10-11T00:00:00Z"), nil)
	defer func() {
		setBenchIndex(nil, nil)
		setModelInfo(nil)
		reset := defaultConfig()
		applyConfig(&reset)
	}()

	if sr := p.StatusFor(true); sr.Benchmarks == nil {
		t.Fatal("authed status lost the benchmarks section")
	} else if sr.Benchmarks.Matched != 1 {
		t.Errorf("matched = %d, want 1", sr.Benchmarks.Matched)
	}

	// flag off hides it even for an authed caller.
	off := base
	off.Status.ShowBenchmarks = false
	applyConfig(&off)
	if sr := p.StatusFor(true); sr.Benchmarks != nil {
		t.Error("show_benchmarks: false still emitted the section")
	}
	applyConfig(&base)

	// unauthenticated never sees it, flag or not.
	if sr := p.StatusFor(false); sr.Benchmarks != nil {
		t.Error("unauthenticated /status exposed the benchmarks section")
	}

	// disabled entirely: section reports it is off rather than vanishing.
	dis := base
	dis.Benchmarks.Enabled = false
	applyConfig(&dis)
	sr := p.StatusFor(true)
	if sr.Benchmarks == nil || sr.Benchmarks.Enabled {
		t.Errorf("benchmarks.enabled: false -> %+v", sr.Benchmarks)
	}
	applyConfig(&base)
}

// TestSortModelsByBench pins the listing order: scored first by score, then
// everything else, and a stable tiebreak so clients see a deterministic list.
func TestSortModelsByBench(t *testing.T) {
	list := []openRouterModel{
		{ID: "opencode/zzz-free"},
		{ID: "freeepi/glm-5.3-flash", Benchmarks: map[string]any{"intelligence_index": 41.8}},
		{ID: "opencode/aaa-free"},
		{ID: "z-ai/glm-5.3", Benchmarks: map[string]any{"intelligence_index": 44.7}},
		{ID: "antseed/mimo-v2.6-flash", Benchmarks: map[string]any{"intelligence_index": 37.8}},
	}
	sortModelsByBench(list)
	got := []string{list[0].ID, list[1].ID, list[2].ID, list[3].ID, list[4].ID}
	want := []string{"z-ai/glm-5.3", "freeepi/glm-5.3-flash", "antseed/mimo-v2.6-flash", "opencode/aaa-free", "opencode/zzz-free"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// TestInfoModelLayers proves the three layers stack and none of them
// overwrite each other: the served freepi window stays, the aa score
// attaches, and the openrouter facts land under their own key.
func TestInfoModelLayers(t *testing.T) {
	def := defaultConfig()
	applyConfig(&def)
	setModelInfo(map[string]modelInfoEntry{
		"freeepi/glm-5.3-flash": {
			Ctx: 262144, MaxOut: 64000, Name: "GLM 5.3 Flash",
			// the free-pi gateway serves less than the upstream model
			FreepiCtx: 65536, FreepiMaxOut: 8192,
			Free: true, Family: "glm", Source: "free-pi-cli",
		},
	})
	setORIndex(buildORIndex(orFixture()), nil)
	setBenchIndex(buildAAIndex([]aaRow{
		{Slug: "glm-5-3-flash", IntelligenceIndex: ptr(41.8), Creator: struct {
			Slug string `json:"slug"`
			Name string `json:"name"`
		}{Slug: "zai", Name: "Z AI"}},
	}, "2026-10-11T00:00:00Z"), nil)
	defer func() {
		setBenchIndex(nil, nil)
		setORIndex(nil, nil)
		setModelInfo(nil)
		reset := defaultConfig()
		applyConfig(&reset)
	}()

	m := infoModel("freeepi/glm-5.3-flash", "freeepi/glm-5.3-flash")
	if m.ContextLength != 65536 {
		t.Errorf("context_length = %d, want the served window", m.ContextLength)
	}
	if m.TopProvider["max_completion_tokens"] != 8192 {
		t.Errorf("max_completion_tokens = %v", m.TopProvider["max_completion_tokens"])
	}
	if m.Benchmarks == nil || m.Benchmarks["intelligence_index"] != 41.8 {
		t.Errorf("benchmarks = %v", m.Benchmarks)
	}
	if m.Benchmarks["match"] != "slug" {
		t.Errorf("match = %v, want slug", m.Benchmarks["match"])
	}
	if m.OpenRouter == nil || m.OpenRouter["id"] != "z-ai/glm-5.3-flash" {
		t.Errorf("openrouter = %v", m.OpenRouter)
	}
	if m.Nim == nil || m.Nim["free"] != true || m.Nim["served_context"] != 65536 {
		t.Errorf("nim = %v", m.Nim)
	}
	if m.Nim["source"] != "free-pi-cli" {
		t.Errorf("nim.source = %v, want the catalog provenance", m.Nim["source"])
	}
}

// TestRefreshFallsBackToStaleCache pins the boot-time contract: when the
// network is dead and the cache is past its freshness window, the index
// still loads from disk (with the error recorded) instead of staying empty.
// owner boundary: refreshOpenRouter/refreshBenchmarks own "stale beats nil".
func TestRefreshFallsBackToStaleCache(t *testing.T) {
	dir := t.TempDir()
	stale := time.Now().Add(-48 * time.Hour)

	orFile := filepath.Join(dir, "or.json")
	writeORCache(orFile, []*orModel{{ID: "z-ai/glm-5.3-flash", ContextLength: 200000}})
	if err := os.Chtimes(orFile, stale, stale); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENROUTER_FILE", orFile)

	aaFile := filepath.Join(dir, "aa.json")
	writeAACache(aaFile, []aaRow{{Slug: "glm-5-3-flash", IntelligenceIndex: ptr(41.8)}}, "2026-10-11T00:00:00Z")
	if err := os.Chtimes(aaFile, stale, stale); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AA_BENCH_FILE", aaFile)

	// dead upstream: port 9 refuses instantly.
	c := defaultConfig()
	c.OpenRouter.URL = "http://127.0.0.1:9/or.json"
	c.Benchmarks.URL = "http://127.0.0.1:9/aa.json"
	applyConfig(&c)
	defer func() {
		setORIndex(nil, nil)
		setBenchIndex(nil, nil)
		reset := defaultConfig()
		applyConfig(&reset)
	}()

	refreshOpenRouter()
	refreshBenchmarks()

	if ix := openRouterIndex(); ix == nil || ix.count != 1 {
		t.Fatalf("openrouter index = %+v, want the stale cache", ix)
	}
	if openRouterError() == "" {
		t.Error("openrouter error must be recorded alongside the stale index")
	}
	_, _, n, ok := benchSnapshot()
	if !ok || n != 1 {
		t.Fatalf("benchmarks index from stale cache: ok=%v n=%d, want ok with 1", ok, n)
	}
	if benchLastError() == "" {
		t.Error("benchmarks error must be recorded alongside the stale index")
	}
}

// TestBenchSummaryScoresServedIDs pins the gap that made the status menu
// lie by omission: /v1/models lists serving ids (unlisted zen models,
// antseed sellers) that have no catalog key, and only walking the catalog
// left their cells blank even though AA scored the model.
// owner boundary: buildBenchSummary owns the id set it scores.
func TestBenchSummaryScoresServedIDs(t *testing.T) {
	c := defaultConfig()
	c.Benchmarks.Enabled = true
	applyConfig(&c)
	defer func() {
		reset := defaultConfig()
		applyConfig(&reset)
	}()

	setModelInfo(map[string]modelInfoEntry{
		"opencode/known-free": {Ctx: 200000},
	})
	setBenchIndex(buildAAIndex([]aaRow{
		{Slug: "known-free", IntelligenceIndex: ptr(30.0)},
		// the unlisted zen id and the antseed seller both strip to these.
		{Slug: "unlisted-zen-model", IntelligenceIndex: ptr(41.0)},
		{Slug: "antseed-seller-model", IntelligenceIndex: ptr(12.0)},
	}, "2026-10-13T00:00:00Z"), nil)
	defer func() {
		setBenchIndex(nil, nil)
		setModelInfo(nil)
	}()

	ocInfo.Store(&OpencodeInfo{Enabled: true, Models: []string{"opencode/unlisted-zen-model-free"}})
	antMu2.Lock()
	antModels = []string{"antseed-seller-model"}
	antMu2.Unlock()
	defer func() {
		ocInfo.Store(&OpencodeInfo{})
		antMu2.Lock()
		antModels = nil
		antMu2.Unlock()
	}()

	s := buildBenchSummary()
	got := map[string]string{}
	for _, r := range s.Models {
		got[r.ID] = r.Match
	}
	if s.Matched != 1 || s.Served != 2 {
		t.Fatalf("matched=%d served=%d, want 1 and 2", s.Matched, s.Served)
	}
	for id, want := range map[string]string{
		"opencode/known-free":              "slug",
		"opencode/unlisted-zen-model-free": "served",
		"antseed/antseed-seller-model":     "served",
	} {
		if got[id] != want {
			t.Errorf("%s: match=%q, want %q (rows: %v)", id, got[id], want, got)
		}
	}
}
