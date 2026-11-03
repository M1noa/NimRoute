package main

// openrouter model facts. openrouter's /v1/models is the widest free
// catalog of ctx, maxout, modality, supported_parameters and effort levels
// we can get, so it fills the gaps model_info.jsonc leaves behind.
//
// the rule is one-directional: the file wins wherever it already says
// something. network data never overwrites a curated fact, and never writes
// to the file unless `nim models sync` is asked to.

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const orDefaultURL = "https://openrouter.ai/api/v1/models"

// orMaxBytes caps the download (~770KB today).
const orMaxBytes = 8 << 20

// orModel is the openrouter /v1/models shape. field names mirror upstream so
// /v1/models can pass them straight through to a client that knows them.
type orModel struct {
	ID            string `json:"id"`
	CanonicalSlug string `json:"canonical_slug"`
	HuggingFaceID string `json:"hugging_face_id"`
	Name          string `json:"name"`
	Created       int64  `json:"created"`
	Description   string `json:"description"`
	ContextLength int    `json:"context_length"`
	Architecture  struct {
		Modality         string   `json:"modality"`
		InputModalities  []string `json:"input_modalities"`
		OutputModalities []string `json:"output_modalities"`
		Tokenizer        string   `json:"tokenizer"`
		InstructType     any      `json:"instruct_type"`
	} `json:"architecture"`
	Pricing     map[string]any `json:"pricing"`
	TopProvider struct {
		ContextLength       int  `json:"context_length"`
		MaxCompletionTokens *int `json:"max_completion_tokens"`
		IsModerated         bool `json:"is_moderated"`
	} `json:"top_provider"`
	SupportedParameters []string       `json:"supported_parameters"`
	DefaultParameters   map[string]any `json:"default_parameters"`
	SupportedVoices     []string       `json:"supported_voices"`
	KnowledgeCutoff     *string        `json:"knowledge_cutoff"`
	ExpirationDate      *string        `json:"expiration_date"`
	Links               map[string]any `json:"links"`
	Reasoning           *struct {
		Mandatory        bool     `json:"mandatory"`
		SupportedEfforts []string `json:"supported_efforts"`
		DefaultEffort    string   `json:"default_effort"`
	} `json:"reasoning"`
}

// orIndex resolves a nim id to an openrouter row. exact ids are preferred;
// the normalized index is what catches the free backends, whose ids carry a
// prefix openrouter never uses (freeepi/glm-5.3-flash -> z-ai/glm-5.3-flash).
type orIndex struct {
	byID  map[string]*orModel
	byKey map[string]*orModel
	count int
}

func buildORIndex(models []*orModel) *orIndex {
	ix := &orIndex{
		byID:  make(map[string]*orModel, len(models)),
		byKey: make(map[string]*orModel, len(models)),
		count: len(models),
	}
	for _, m := range models {
		if m == nil || m.ID == "" {
			continue
		}
		// openrouter appends ":free" to some rows; the model is the same.
		for _, f := range idForms(m.ID) {
			if f == "" {
				continue
			}
			if _, dup := ix.byKey[f]; !dup {
				ix.byKey[f] = m
			}
		}
		ix.byID[strings.ToLower(strings.TrimSpace(m.ID))] = m
	}
	return ix
}

func (ix *orIndex) get(id string) *orModel {
	if ix == nil {
		return nil
	}
	if m, ok := ix.byID[strings.ToLower(strings.TrimSpace(id))]; ok {
		return m
	}
	// the [1m] suffix is a claude-code alias we add; the base model serves it.
	ids := []string{id}
	if base, ok := baseModelID(id); ok {
		if m, ok := ix.byID[strings.ToLower(strings.TrimSpace(base))]; ok {
			return m
		}
		ids = append(ids, base)
	}
	for _, alt := range ids {
		for _, f := range idForms(alt) {
			if m, ok := ix.byKey[f]; ok {
				return m
			}
		}
	}
	return nil
}

var (
	orMu   sync.RWMutex
	orLive *orIndex
)

func setORIndex(ix *orIndex) {
	orMu.Lock()
	defer orMu.Unlock()
	if ix != nil {
		orLive = ix
	}
}

// openRouterIndex returns the live index, nil before the first fetch.
func openRouterIndex() *orIndex {
	orMu.RLock()
	defer orMu.RUnlock()
	return orLive
}

func openRouterEnabled() bool { return cfg().OpenRouter.Enabled }

func orCachePath() string {
	if e := os.Getenv("OPENROUTER_FILE"); e != "" {
		return e
	}
	return "openrouter_models.json"
}

type orDiskCache struct {
	Fetched time.Time  `json:"fetched_at"`
	Models  []*orModel `json:"models"`
}

func readORCache(path string, maxAge time.Duration) *orIndex {
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
	var dc orDiskCache
	if json.Unmarshal(raw, &dc) != nil || len(dc.Models) == 0 {
		return nil
	}
	return buildORIndex(dc.Models)
}

func writeORCache(path string, models []*orModel) {
	body, err := json.Marshal(orDiskCache{Fetched: time.Now(), Models: models})
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, body, 0o644) != nil {
		return
	}
	os.Rename(tmp, path)
}

func fetchOpenRouter(url string) ([]*orModel, error) {
	if url == "" {
		url = orDefaultURL
	}
	// 60s: the list is ~770KB and the fetch runs in the background, so a slow
	// connection costs nothing but latency.
	client := &http.Client{Timeout: 60 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "nimroute/"+versionStr)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: http %d", url, resp.StatusCode)
	}
	var payload struct {
		Data []*orModel `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, orMaxBytes)).Decode(&payload); err != nil {
		return nil, err
	}
	if len(payload.Data) == 0 {
		return nil, fmt.Errorf("%s: empty model list", url)
	}
	return payload.Data, nil
}

// refreshOpenRouter is one fetch cycle. a stale cache is preferred over a
// failed fetch: stale context facts beat none.
func refreshOpenRouter() {
	if !openRouterEnabled() {
		return
	}
	path := orCachePath()
	if ix := readORCache(path, time.Duration(cfg().OpenRouter.RefreshHours)*time.Hour); ix != nil {
		setORIndex(ix)
		log.Printf("  openrouter: %d models from cache", ix.count)
		return
	}
	models, err := fetchOpenRouter(cfg().OpenRouter.URL)
	if err != nil {
		// stale cache beats no index: at boot "keep the previous" has
		// nothing to keep, and a slow openrouter must not empty the
		// models listing.
		if ix := readORCache(path, 0); ix != nil {
			setORIndex(ix)
			log.Printf("WARN: openrouter fetch failed, using stale cache: %v", err)
			return
		}
		setORIndex(nil)
		log.Printf("WARN: openrouter fetch failed, keeping previous: %v", err)
		return
	}
	ix := buildORIndex(models)
	writeORCache(path, models)
	setORIndex(ix)
	log.Printf("  openrouter: %d models", ix.count)
}

func watchOpenRouter() {
	go refreshOpenRouter()
	h := cfg().OpenRouter.RefreshHours
	if h <= 0 {
		h = 6
	}
	go func() {
		t := time.NewTicker(time.Duration(h) * time.Hour)
		for range t.C {
			refreshOpenRouter()
		}
	}()
}

// orEntry turns an openrouter row into a catalog entry. used to synthesize an
// entry for a model the file has never heard of, and as the donor in
// mergeOREntry.
func orEntry(m *orModel) modelInfoEntry {
	e := modelInfoEntry{Source: "openrouter"}
	if m.Name != "" {
		// openrouter prefixes the vendor: "Z AI: GLM 5.3" -> "GLM 5.3".
		if i := strings.Index(m.Name, ": "); i > 0 {
			e.Name = m.Name[i+2:]
		} else {
			e.Name = m.Name
		}
	}
	e.Desc = m.Description
	e.Ctx = m.ContextLength
	if m.TopProvider.MaxCompletionTokens != nil && *m.TopProvider.MaxCompletionTokens > 0 {
		e.MaxOut = *m.TopProvider.MaxCompletionTokens
	}
	e.Inputs = m.Architecture.InputModalities
	e.OutMods = m.Architecture.OutputModalities
	e.SupportedParams = m.SupportedParameters
	if len(m.SupportedParameters) > 0 {
		supports := map[string]bool{}
		for _, p := range m.SupportedParameters {
			supports[p] = true
		}
		e.Tools = supports["tools"]
		e.Temp = supports["temperature"]
		e.Structured = supports["response_format"] || supports["structured_outputs"]
	}
	if len(m.Architecture.InputModalities) > 1 {
		e.Attachment = true
	}
	if m.Reasoning != nil {
		r := m.Reasoning.Mandatory
		e.Reasoning = &r
		e.EffortLevels = m.Reasoning.SupportedEfforts
	}
	if m.KnowledgeCutoff != nil {
		e.Knowledge = *m.KnowledgeCutoff
	}
	if m.Created > 0 {
		e.Released = time.Unix(m.Created, 0).UTC().Format("2006-01-02")
	}
	return e
}

// mergeOREntry fills the zero fields of dst from src and reports whether
// anything moved. dst is the catalog entry and always wins: this only ever
// turns empty into known.
//
// the capability bools (tools, temperature, structured_output, attachment)
// are deliberately not merged: the struct cannot tell an absent key from a
// curated `false`, so merging would contradict stated facts. `models sync`
// adds them by key presence instead.
func mergeOREntry(dst *modelInfoEntry, src modelInfoEntry) bool {
	changed := false
	setStr := func(d *string, v string) {
		if *d == "" && v != "" {
			*d = v
			changed = true
		}
	}
	setInt := func(d *int, v int) {
		if *d == 0 && v > 0 {
			*d = v
			changed = true
		}
	}
	setSlice := func(d *[]string, v []string) {
		if len(*d) == 0 && len(v) > 0 {
			*d = v
			changed = true
		}
	}
	setStr(&dst.Name, src.Name)
	setStr(&dst.Desc, src.Desc)
	setInt(&dst.Ctx, src.Ctx)
	setInt(&dst.MaxOut, src.MaxOut)
	setSlice(&dst.Inputs, src.Inputs)
	setSlice(&dst.OutMods, src.OutMods)
	setSlice(&dst.SupportedParams, src.SupportedParams)
	setStr(&dst.Knowledge, src.Knowledge)
	setStr(&dst.Released, src.Released)
	// a nil pointer is the catalog saying nothing; false is it saying no.
	if dst.Reasoning == nil && src.Reasoning != nil {
		r := *src.Reasoning
		dst.Reasoning = &r
		changed = true
	}
	setSlice(&dst.EffortLevels, src.EffortLevels)
	return changed
}

// withOROverlay returns id's catalog entry with openrouter filling the gaps.
// an id absent from the file gets a synthesized entry so it still serves with
// real numbers instead of the 131072 fallback.
func withOROverlay(id string, e *modelInfoEntry) *modelInfoEntry {
	ix := openRouterIndex()
	if ix == nil {
		return e
	}
	m := ix.get(id)
	if m == nil {
		return e
	}
	src := orEntry(m)
	if e == nil {
		return &src
	}
	out := *e
	if mergeOREntry(&out, src) {
		return &out
	}
	return e
}

// orExtras is the openrouter payload for /v1/models: the fields we relay
// verbatim plus the served-contract facts only this proxy knows.
// orPrice1M is a model's openrouter price in usd per million tokens. the
// catalog quotes usd per *token* as decimal strings, so the readable figure
// is a scale of its own number, not a second source. ok is false when the
// row is unknown or quotes no text price.
func orPrice1M(infoID string) (in, out float64, ok bool) {
	ix := openRouterIndex()
	if ix == nil {
		return 0, 0, false
	}
	m := ix.get(infoID)
	if m == nil || len(m.Pricing) == 0 {
		return 0, 0, false
	}
	pin, iok := orPriceValue(m.Pricing["prompt"])
	pout, ook := orPriceValue(m.Pricing["completion"])
	if !iok && !ook {
		return 0, 0, false
	}
	return roundPrice(pin * 1e6), roundPrice(pout * 1e6), true
}

// orPriceValue reads one quoted price: a decimal string on most rows, a
// number on a few.
func orPriceValue(v any) (float64, bool) {
	switch t := v.(type) {
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	case float64:
		return t, true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	}
	return 0, false
}

// roundPrice trims float noise from the per-million scale (0.37, not
// 0.37000000000000011).
func roundPrice(f float64) float64 { return math.Round(f*1e6) / 1e6 }

func orExtras(infoID string) map[string]any {
	ix := openRouterIndex()
	if ix == nil {
		return nil
	}
	m := ix.get(infoID)
	if m == nil {
		return nil
	}
	ex := map[string]any{"source": "openrouter", "id": m.ID}
	if m.CanonicalSlug != "" {
		ex["canonical_slug"] = m.CanonicalSlug
	}
	if m.HuggingFaceID != "" {
		ex["hugging_face_id"] = m.HuggingFaceID
	}
	if m.Created > 0 {
		ex["created"] = m.Created
	}
	if len(m.SupportedParameters) > 0 {
		ex["supported_parameters"] = m.SupportedParameters
	}
	if len(m.DefaultParameters) > 0 {
		ex["default_parameters"] = m.DefaultParameters
	}
	if len(m.SupportedVoices) > 0 {
		ex["supported_voices"] = m.SupportedVoices
	}
	if len(m.Architecture.InputModalities) > 0 {
		ex["input_modalities"] = m.Architecture.InputModalities
	}
	if len(m.Architecture.OutputModalities) > 0 {
		ex["output_modalities"] = m.Architecture.OutputModalities
	}
	if m.Reasoning != nil {
		r := map[string]any{"mandatory": m.Reasoning.Mandatory}
		if len(m.Reasoning.SupportedEfforts) > 0 {
			r["supported_efforts"] = m.Reasoning.SupportedEfforts
		}
		if m.Reasoning.DefaultEffort != "" {
			r["default_effort"] = m.Reasoning.DefaultEffort
		}
		ex["reasoning"] = r
	}
	return ex
}

// syncModelInfo merges openrouter facts into the catalog file, line by line
// so the header and section comments survive. only empty fields are filled;
// nothing is removed. returns the number of entries changed.
func syncModelInfo(path string, ix *orIndex) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	lines := strings.Split(string(raw), "\n")
	changed := 0
	for i, line := range lines {
		key, body, ok := parseCatalogLine(line)
		if !ok {
			continue
		}
		m := ix.get(key)
		if m == nil {
			continue
		}
		// presence, not zero-value: a key that is there but says false or
		// [] is a curated fact and must not be touched or dropped.
		var have map[string]json.RawMessage
		if json.Unmarshal([]byte(body), &have) != nil {
			continue
		}
		missing := missingORFields(have, orEntry(m))
		if len(missing) == 0 {
			continue
		}
		// append only, so every existing key keeps its exact bytes. inner
		// is the object body without either brace; the emit below restores
		// the catalog's own `"key": {…},` shape.
		inner := strings.TrimPrefix(strings.TrimSuffix(strings.TrimSpace(body), "}"), "{")
		inner += "," + strings.Join(missing, ",")
		lines[i] = "  " + strconv.Quote(key) + ": {" + inner + "},"
		changed++
	}
	if changed == 0 {
		return 0, nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	return changed, nil
}

// parseCatalogLine splits a `  "key": {...},` line into its key and the raw
// json object. returns false for comments, braces and anything else.
func parseCatalogLine(line string) (key, body string, ok bool) {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, `"`) {
		return "", "", false
	}
	// the catalog is `"key": {...},` with a space; the spaceless form is
	// accepted too so either shape parses.
	i := strings.Index(t, `":`)
	if i < 0 || !strings.HasSuffix(t, "},") {
		return "", "", false
	}
	key = t[1:i]
	body = strings.TrimSpace(t[i+2 : len(t)-1])
	var probe map[string]json.RawMessage
	if json.Unmarshal([]byte(body), &probe) != nil {
		return "", "", false
	}
	return key, body, true
}

// missingORFields renders the openrouter facts the catalog has no key for
// at all. a key that is present but empty still counts as curated: "[]"
// means "no record" for supported_parameters, and a false bool is a stated
// fact. only absent keys are ever added, and only when the value is real.
func missingORFields(have map[string]json.RawMessage, src modelInfoEntry) []string {
	srcJSON, err := json.Marshal(src)
	if err != nil {
		return nil
	}
	var fromOR map[string]json.RawMessage
	if json.Unmarshal(srcJSON, &fromOR) != nil {
		return nil
	}
	var out []string
	for _, k := range syncFieldOrder {
		if _, present := have[k]; present {
			continue
		}
		v, ok := fromOR[k]
		if !ok || !jsonHasContent(v) {
			continue
		}
		out = append(out, strconv.Quote(k)+":"+string(v))
	}
	return out
}

// syncFieldOrder fixes the append order so repeated syncs stay byte-stable.
var syncFieldOrder = []string{
	"name", "desc", "ctx", "maxout", "inputs", "output_modalities",
	"reasoning", "reasoning_effort_levels", "supports_temperature", "tools",
	"structured_output", "attachment", "supported_parameters",
	"knowledge_cutoff", "released",
}

// jsonHasContent reports whether a json value is worth writing: not null,
// not the empty string, not zero, not an empty array or object. a false bool
// is content (a stated fact), so it is kept when absent-keyed.
func jsonHasContent(v json.RawMessage) bool {
	s := strings.TrimSpace(string(v))
	switch s {
	case "", "null", `""`, "0", "[]", "{}":
		return false
	}
	return true
}

// runModelsSync is the `models sync` subcommand: fetch openrouter, merge the
// gaps, and write the catalog back so the facts become permanent.
func runModelsSync(configPath string) int {
	if !openRouterEnabled() {
		fmt.Println("openrouter.enabled is false in", configPath)
		return 1
	}
	models, err := fetchOpenRouter(cfg().OpenRouter.URL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "openrouter fetch failed:", err)
		return 1
	}
	ix := buildORIndex(models)
	path := modelInfoPath()
	changed, err := syncModelInfo(path, ix)
	if err != nil {
		fmt.Fprintln(os.Stderr, "write failed:", err)
		return 1
	}
	fmt.Printf("openrouter: %d models\n", ix.count)
	fmt.Printf("catalog %s: %d entries filled\n", path, changed)
	if changed == 0 {
		fmt.Println("nothing to fill; every openrouter-backed entry already had these fields")
	}
	return 0
}
