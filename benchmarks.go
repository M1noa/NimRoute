package main

// benchmark scores from Artificial Analysis. the data is which-llm's
// published aa_data.json (their workflow re-scrapes AA daily), fetched in the
// background and cached on disk.
//
// AA's own api (artificialanalysis.ai/api/v2/data/llms/models) wants a key
// and already carries deprecation + sunset: 2026-11-04 headers. scraping
// their next.js page means regexing the rsc flight stream and aes-gcm
// decrypting a gzip manifest, which breaks whenever they rebuild. the
// published json avoids all three.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// aaRoots are full file URLs, tried in order: the refresh branch carries the
// current data, main is the fallback when the bot has not pushed today.
var aaRoots = []string{
	"https://raw.githubusercontent.com/ariobarin/which-llm/automation/daily-data-refresh/skills/which-llm/artifacts/aa_data.json",
	"https://raw.githubusercontent.com/ariobarin/which-llm/main/skills/which-llm/artifacts/aa_data.json",
}

// aaMaxBytes caps the download. the file is ~7MB today; 32MB leaves room to
// grow without letting a bad response stream forever.
const aaMaxBytes = 32 << 20

// aaRow is the subset of an AA catalog row we read. the file carries every
// upstream field, but coding/agentic indices were dropped upstream and
// scoring eleven benchmarks is all we need.
type aaRow struct {
	Slug    string `json:"slug"`
	Name    string `json:"name"`
	Release struct {
		Slug string `json:"slug"`
	} `json:"release"`
	Creator struct {
		Slug string `json:"slug"`
		Name string `json:"name"`
	} `json:"creator"`
	IntelligenceIndex *float64 `json:"intelligenceIndex"`
	ContextWindow     int      `json:"contextWindowTokens"`
	Price1mInput      *float64 `json:"price1mInputTokens"`
	Price1mOutput     *float64 `json:"price1mOutputTokens"`
	Reasoning         *bool    `json:"isReasoning"`
	OpenWeights       bool     `json:"isOpenWeights"`
	Deprecated        bool     `json:"deprecated"`
	ReleaseDate       string   `json:"releaseDate"`
	KnowledgeCutoff   *string  `json:"knowledgeCutoffDate"`
	GPQA              *float64 `json:"gpqa"`
	HLE               *float64 `json:"hle"`
	AIME25            *float64 `json:"aime25"`
	LiveCodeBench     *float64 `json:"livecodebench"`
	Scicode           *float64 `json:"scicode"`
	TerminalBenchHard *float64 `json:"terminalbenchHard"`
	Tau2              *float64 `json:"tau2"`
}

// aaIndex is the matched-once view of a fetched catalog: one row per slug
// plus every row grouped by its release family.
type aaIndex struct {
	bySlug   map[string]aaRow
	byFamily map[string][]aaRow
	updated  string
	fetched  time.Time
}

func buildAAIndex(rows []aaRow, updated string) *aaIndex {
	ix := &aaIndex{
		bySlug:   make(map[string]aaRow, len(rows)),
		byFamily: make(map[string][]aaRow),
		updated:  updated,
		fetched:  time.Now(),
	}
	for _, r := range rows {
		// a row with no score can't be matched on or sorted by one.
		if r.IntelligenceIndex == nil {
			continue
		}
		if s := normModelID(r.Slug); s != "" {
			if _, dup := ix.bySlug[s]; !dup {
				ix.bySlug[s] = r
			}
		}
		if s := normModelID(r.Release.Slug); s != "" {
			ix.byFamily[s] = append(ix.byFamily[s], r)
		}
	}
	return ix
}

var (
	benchMu      sync.RWMutex
	benchIndex   *aaIndex
	benchLastErr string
)

func setBenchIndex(ix *aaIndex, err error) {
	benchMu.Lock()
	defer benchMu.Unlock()
	if ix != nil {
		benchIndex = ix
	}
	benchLastErr = ""
	if err != nil {
		benchLastErr = err.Error()
	}
}

// benchmarksEnabled reports whether AA scores are fetched and served.
func benchmarksEnabled() bool { return cfg().Benchmarks.Enabled }

// benchMatch is one matched row plus how it was found. the kind travels all
// the way to /status and /v1/models: a basename match is weaker evidence than
// an exact slug, and the reader should be able to tell them apart.
type benchMatch struct {
	Row  aaRow
	Kind string // "exact", "slug", or "family"; "" when nothing matched
}

func (m benchMatch) ok() bool { return m.Kind != "" }

func (m benchMatch) index() float64 {
	if m.Row.IntelligenceIndex == nil {
		return 0
	}
	return *m.Row.IntelligenceIndex
}

// benchFor matches one model id against the live catalog. the exact
// org/name form first, then the bare and variant-stripped forms, then the
// release family (highest scoring member wins).
func benchFor(id string) benchMatch {
	benchMu.RLock()
	ix := benchIndex
	benchMu.RUnlock()
	if ix == nil {
		return benchMatch{}
	}
	forms := idForms(id)
	if len(forms) == 0 {
		return benchMatch{}
	}
	// the full id is only an exact hit when aa itself carries a slash.
	if r, ok := ix.bySlug[forms[0]]; ok {
		return benchMatch{Row: r, Kind: "exact"}
	}
	for _, cand := range forms[1:] {
		if r, ok := ix.bySlug[cand]; ok {
			return benchMatch{Row: r, Kind: "slug"}
		}
	}
	// family: the catalog row may be one effort level or modality of the
	// model we serve, and those rows disagree on score.
	for _, cand := range forms {
		var best aaRow
		for _, r := range ix.byFamily[cand] {
			if best.Slug == "" || *r.IntelligenceIndex > *best.IntelligenceIndex {
				best = r
			}
		}
		if best.Slug != "" {
			return benchMatch{Row: best, Kind: "family"}
		}
	}
	return benchMatch{}
}

// benchSnapshot returns the current index metadata for /status.
func benchSnapshot() (updated string, fetched time.Time, rows int, ok bool) {
	benchMu.RLock()
	defer benchMu.RUnlock()
	if benchIndex == nil {
		return "", time.Time{}, 0, false
	}
	return benchIndex.updated, benchIndex.fetched, len(benchIndex.bySlug), true
}

func benchLastError() string {
	benchMu.RLock()
	defer benchMu.RUnlock()
	return benchLastErr
}

// normModelID folds the two catalogs' separator habits into one form: aa
// slugs are all dashes, our ids and openrouter keep dots in version numbers.
// "/" survives as "/": both catalogs use it the same way, org/name, and
// folding it away would erase the boundary we match on.
func normModelID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	b.Grow(len(s))
	lastDash := false
	for _, r := range s {
		if r == '/' {
			b.WriteRune('/')
			lastDash = true
			continue
		}
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
			}
			lastDash = true
			continue
		}
		b.WriteRune(r)
		lastDash = false
	}
	return strings.Trim(b.String(), "-")
}

// idForms is the candidate list every catalog lookup walks, best first:
// the full org/name, the bare name, and the bare name with the serving
// suffixes off. aa indexes one model under many slugs, so the bare forms
// are what usually hit.
func idForms(id string) []string {
	n := normModelID(id)
	if n == "" {
		return nil
	}
	forms := []string{n}
	base := n
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	for _, f := range []string{base, stripModelVariants(base)} {
		if f != "" && f != n && !slices.Contains(forms, f) {
			forms = append(forms, f)
		}
	}
	return forms
}

// modelVariants are the suffixes the free backends bolt onto an upstream
// model. AA indexes the upstream model, so these come off before the lookup.
// Order matters: longer suffixes first so "-non-reasoning" is not clipped by
// "-reasoning".
var modelVariants = []string{
	"-default-fallback", "-non-reasoning", "-reasoning", "-thinking",
	"-xhigh", "-high", "-medium", "-low", "-minimal", "-max",
	"-preview", "-latest", "-vision", "-vl", "-free",
	// opencode's free-tier branding: same muse-spark 1.3 weights upstream.
	"-contributor",
}

// stripModelVariants removes serving suffixes from a normalized id. the core
// must stay at least four characters, so "gemma-4" never decays to "gemma".
func stripModelVariants(s string) string {
	for changed := true; changed; {
		changed = false
		for _, suf := range modelVariants {
			if strings.HasSuffix(s, suf) && len(s)-len(suf) >= 4 {
				s = strings.TrimSuffix(s, suf)
				changed = true
			}
		}
	}
	return s
}

// aaBlockRe finds a dataset header line: "catalog":{...,"rows":[
var aaBlockRe = regexp.MustCompile(`^"([a-z/-]+)":\{`)
var aaStampRe = regexp.MustCompile(`"source_updated_at_utc":\s*"([^"]*)"`)

// scanAACatalog pulls the catalog dataset out of the bundle without holding
// the file. which-llm writes one row per line, so the block header names the
// dataset and every following line that opens with '{' is a row.
func scanAACatalog(r io.Reader) ([]aaRow, string, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	var rows []aaRow
	var stamp string
	inBlock := false
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			line = strings.TrimRight(line, "\r\n")
			switch {
			case !inBlock:
				if m := aaBlockRe.FindStringSubmatch(line); m != nil && m[1] == "catalog" {
					inBlock = true
					if s := aaStampRe.FindStringSubmatch(line); s != nil {
						stamp = s[1]
					}
				}
			default:
				switch {
				case line == "]}" || line == "}":
					return rows, stamp, nil
				case strings.HasPrefix(line, `"`):
					// next dataset: this one is over.
					return rows, stamp, nil
				case strings.HasPrefix(line, "{"):
					// the publisher appends the closing "]}" (and the
					// bundle's "}}" on the very last block) to the last
					// row of a dataset, so retry while peeling closers
					// off or we drop one model per dataset.
					if row, ok := unmarshalRow(line); ok {
						rows = append(rows, row)
					}
				}
			}
		}
		if err != nil {
			if err == io.EOF && inBlock {
				return rows, stamp, nil
			}
			if err != io.EOF {
				return nil, "", err
			}
			return rows, stamp, nil
		}
	}
}

// unmarshalRow decodes one row line, tolerating the closers the publisher
// appends to the last row of a dataset: "]}" (end of block) and, for the
// final block in the file, "}}". peel one "]" or "}" at a time and retry;
// at most three peels are ever needed.
func unmarshalRow(line string) (aaRow, bool) {
	s := strings.TrimSuffix(strings.TrimSpace(line), ",")
	for i := 0; i < 4; i++ {
		var row aaRow
		if json.Unmarshal([]byte(s), &row) == nil {
			return row, true
		}
		if !strings.HasSuffix(s, "]") && !strings.HasSuffix(s, "}") {
			break
		}
		s = s[:len(s)-1]
	}
	return aaRow{}, false
}

func aaCachePath() string {
	if e := os.Getenv("AA_BENCH_FILE"); e != "" {
		return e
	}
	return "aa_benchmarks.json"
}

// aaDiskCache is the trimmed slice we keep: the rows plus their source date.
type aaDiskCache struct {
	Updated string  `json:"updated_at"`
	Rows    []aaRow `json:"rows"`
}

// readAACache loads a cached catalog. maxAge <= 0 means "any age"; the
// file's own mtime is the freshness signal, since that is when we fetched it.
func readAACache(path string, maxAge time.Duration) *aaIndex {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() == 0 {
		return nil
	}
	if maxAge > 0 && time.Since(fi.ModTime()) > maxAge {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var dc aaDiskCache
	if json.Unmarshal(raw, &dc) != nil || len(dc.Rows) == 0 {
		return nil
	}
	ix := buildAAIndex(dc.Rows, dc.Updated)
	ix.fetched = fi.ModTime()
	return ix
}

func writeAACache(path string, rows []aaRow, updated string) {
	body, err := json.Marshal(aaDiskCache{Updated: updated, Rows: rows})
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, body, 0o644) != nil {
		return
	}
	os.Rename(tmp, path)
}

// fetchBenchmarks pulls aa_data.json from the first root that answers and
// returns its catalog. keeps the last good index when every root fails.
func fetchBenchmarks(url string) ([]aaRow, string, error) {
	roots := aaRoots
	if url != "" {
		roots = []string{url}
	}
	client := &http.Client{Timeout: 60 * time.Second}
	var lastErr error
	for _, full := range roots {
		req, err := http.NewRequest("GET", full, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("User-Agent", "nimroute/"+versionStr)
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		rows, stamp, serr := func() ([]aaRow, string, error) {
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return nil, "", fmt.Errorf("%s: http %d", full, resp.StatusCode)
			}
			return scanAACatalog(io.LimitReader(resp.Body, aaMaxBytes))
		}()
		if serr != nil {
			lastErr = serr
			continue
		}
		if len(rows) == 0 {
			lastErr = fmt.Errorf("%s: no catalog rows", full)
			continue
		}
		return rows, stamp, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no aa roots configured")
	}
	return nil, "", lastErr
}

// refreshBenchmarks is one fetch cycle: cache when fresh, else network, and
// never drop a good index on failure.
func refreshBenchmarks() {
	c := cfg().Benchmarks
	if !benchmarksEnabled() {
		return
	}
	cache := time.Duration(c.CacheHours) * time.Hour
	path := aaCachePath()
	if ix := readAACache(path, cache); ix != nil {
		setBenchIndex(ix, nil)
		log.Printf("  benchmarks: %d aa models from cache (%s)", len(ix.bySlug), ix.updated)
		return
	}
	rows, stamp, err := fetchBenchmarks(c.URL)
	if err != nil {
		// stale cache beats no scores; at boot there is no previous
		// index to keep. a proxy must still serve without the network.
		if ix := readAACache(path, 0); ix != nil {
			setBenchIndex(ix, err)
			log.Printf("WARN: benchmarks fetch failed, using stale cache: %v", err)
			return
		}
		setBenchIndex(nil, err)
		log.Printf("WARN: benchmarks fetch failed, keeping previous: %v", err)
		return
	}
	ix := buildAAIndex(rows, stamp)
	writeAACache(path, rows, stamp)
	setBenchIndex(ix, nil)
	log.Printf("  benchmarks: %d aa models (%s)", len(ix.bySlug), stamp)
}

// watchBenchmarks refreshes on the configured interval. startup is
// background: a 7MB download must not hold the listener.
func watchBenchmarks() {
	go refreshBenchmarks()
	h := cfg().Benchmarks.RefreshHours
	if h <= 0 {
		h = 12
	}
	go func() {
		t := time.NewTicker(time.Duration(h) * time.Hour)
		for range t.C {
			refreshBenchmarks()
		}
	}()
}

// benchEvals is the benchmark breakdown, omitting evals AA never ran so the
// block says "not measured" rather than "scored zero".
func benchEvals(r aaRow) map[string]float64 {
	out := map[string]float64{}
	add := func(name string, v *float64) {
		if v != nil {
			out[name] = *v
		}
	}
	add("gpqa", r.GPQA)
	add("hle", r.HLE)
	add("aime25", r.AIME25)
	add("livecodebench", r.LiveCodeBench)
	add("scicode", r.Scicode)
	add("terminalbench_hard", r.TerminalBenchHard)
	add("tau2", r.Tau2)
	if len(out) == 0 {
		return nil
	}
	return out
}

// benchBlock is the per-model block on /v1/models. nil when AA has no row.
func benchBlock(m benchMatch) map[string]any {
	if !m.ok() {
		return nil
	}
	r := m.Row
	bl := map[string]any{
		"source":   "artificial-analysis",
		"match":    m.Kind,
		"slug":     r.Slug,
		"aa_model": r.Name,
	}
	if v := m.index(); v != 0 {
		bl["intelligence_index"] = round2(v)
	}
	if r.ContextWindow > 0 {
		bl["context_window"] = r.ContextWindow
	}
	if r.Price1mInput != nil {
		bl["price_1m_input"] = *r.Price1mInput
	}
	if r.Price1mOutput != nil {
		bl["price_1m_output"] = *r.Price1mOutput
	}
	if e := benchEvals(r); e != nil {
		bl["evals"] = e
	}
	if r.Creator.Name != "" {
		bl["creator"] = r.Creator.Name
	}
	if r.OpenWeights {
		bl["open_weights"] = true
	}
	if r.Deprecated {
		bl["deprecated"] = true
	}
	if r.ReleaseDate != "" {
		bl["released"] = r.ReleaseDate
	}
	if r.KnowledgeCutoff != nil && *r.KnowledgeCutoff != "" {
		bl["knowledge_cutoff"] = *r.KnowledgeCutoff
	}
	return bl
}

func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}

// benchRow is one /status line: the model, its backend, and the score.
type benchRow struct {
	ID      string  `json:"id"`
	Name    string  `json:"name,omitempty"`
	Backend string  `json:"backend"`
	Intel   float64 `json:"intelligence_index"`
	Ctx     int     `json:"ctx,omitempty"`
	MaxOut  int     `json:"maxout,omitempty"`
	Match   string  `json:"match"`
	AASlug  string  `json:"aa_slug,omitempty"`
}

type benchSummary struct {
	Enabled   bool   `json:"enabled"`
	Source    string `json:"source_url,omitempty"`
	Updated   string `json:"updated_at,omitempty"`
	Fetched   string `json:"fetched_at,omitempty"`
	Age       string `json:"age,omitempty"`
	AAModels  int    `json:"aa_models"`
	Matched   int    `json:"matched"`
	Unmatched int    `json:"unmatched"`
	// Served is the extra rows scored for ids /v1/models lists that carry
	// no catalog key (unlisted zen ids, antseed sellers).
	Served int        `json:"served_scored"`
	Count  int        `json:"count"`
	Error  string     `json:"error,omitempty"`
	Models []benchRow `json:"models,omitempty"`
}

// benchBackends are the id prefixes the backends list under. note the
// spelling: freepi's *backend* name is "freepi" but its *id* prefix is
// "freeepi/". anything else (bare nim keys like z-ai/glm-5.3) is nvidia.
var benchBackends = []string{"opencode", "freeepi", "antseed", "nous"}

func benchBackendOf(id string) string {
	if i := strings.Index(id, "/"); i > 0 {
		prefix := id[:i]
		for _, b := range benchBackends {
			if prefix == b {
				return b
			}
		}
	}
	return "nvidia"
}

// buildBenchSummary walks the live catalog and scores every key. sorted by
// intelligence index descending, unscored last, id as the tiebreak so the
// order is stable across requests.
func buildBenchSummary() *benchSummary {
	c := cfg().Benchmarks
	s := &benchSummary{Enabled: benchmarksEnabled()}
	if !s.Enabled {
		return s
	}
	if c.URL != "" {
		s.Source = c.URL
	} else {
		s.Source = aaRoots[0]
	}
	updated, fetched, n, ok := benchSnapshot()
	if !ok {
		s.Error = benchLastError()
		if s.Error == "" {
			s.Error = "not fetched yet"
		}
		return s
	}
	s.Updated = updated
	s.Fetched = fetched.Format(time.RFC3339)
	if !fetched.IsZero() {
		s.Age = time.Since(fetched).Round(time.Minute).String()
	}
	s.AAModels = n

	modelInfoMu.RLock()
	keys := make([]string, 0, len(modelInfo))
	for id := range modelInfo {
		keys = append(keys, id)
	}
	modelInfoMu.RUnlock()
	sort.Strings(keys)

	for _, id := range keys {
		m := benchFor(id)
		if !m.ok() {
			s.Unmatched++
			continue
		}
		s.Matched++
		s.Models = append(s.Models, benchRow{
			ID:      id,
			Name:    modelName(id),
			Backend: benchBackendOf(id),
			Intel:   round2(m.index()),
			Ctx:     modelCtx(id),
			MaxOut:  modelMaxOut(id),
			Match:   m.Kind,
			AASlug:  m.Row.Slug,
		})
	}
	// ids a client can call but the catalog has no entry for: an unlisted
	// zen model or an antseed seller still deserves its score, and the
	// menu lists them next to the catalog entries.
	known := make(map[string]bool, len(keys))
	for _, k := range keys {
		known[k] = true
	}
	for _, id := range servedBenchIDs(known) {
		m := benchFor(id)
		if !m.ok() {
			continue
		}
		s.Served++
		s.Models = append(s.Models, benchRow{
			ID:      id,
			Name:    modelName(id),
			Backend: benchBackendOf(id),
			Intel:   round2(m.index()),
			Ctx:     modelCtx(id),
			MaxOut:  modelMaxOut(id),
			Match:   "served",
			AASlug:  m.Row.Slug,
		})
	}

	s.Count = len(s.Models)
	sortBenchRows(s.Models)
	return s
}

// servedBenchIDs lists the ids /v1/models serves outside the catalog:
// the cached zen list, every freepi account's models, and the antseed
// sellers. known is the catalog key set, mutated to dedupe.
func servedBenchIDs(known map[string]bool) []string {
	var out []string
	add := func(id string) {
		if id == "" || known[id] {
			return
		}
		known[id] = true
		out = append(out, id)
	}
	if info := ocInfo.Load(); info != nil {
		for _, m := range info.Models {
			add(m)
		}
	}
	for _, fa := range freepiAccounts() {
		for _, m := range freepiModels(fa) {
			add("freeepi/" + m)
		}
	}
	antMu2.Lock()
	for _, m := range antModels {
		add("antseed/" + m)
	}
	antMu2.Unlock()
	sort.Strings(out)
	return out
}

// sortBenchRows orders by score desc, then id. only matched rows reach the
// list, so the id tiebreak just keeps a zero score from floating.
func sortBenchRows(rows []benchRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Intel != b.Intel {
			return a.Intel > b.Intel
		}
		return a.ID < b.ID
	})
}
