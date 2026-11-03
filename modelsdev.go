package main

// modelsdev.go fills model_info.jsonc gaps from https://models.dev/api.json:
// a per-provider catalog of model facts (window, output cap, modalities,
// capabilities, reasoning options, release date). it is the last overlay
// behind the file and the openrouter index, so it only ever turns an unknown
// id or an empty field into a known one, and it never writes to the file.
//
// the fetch is ~6MB, so it runs in the background at boot and then on the
// configured tick; a failed fetch keeps the previous index.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const mdDefaultURL = "https://models.dev/api.json"

// mdMaxBytes caps the download (~6MB today) at a size that still leaves room
// for the catalog to double.
const mdMaxBytes = 96 << 20

// mdPrefixes are our backend prefixes, which are not model-id prefixes in
// models.dev.
var mdPrefixes = []string{"opencode/", "freeepi/", "antseed/", "nous/", "uno/",
	"openrouter/", "orcarouter/", "nvidia/"}

type mdReasoningOption struct {
	Type   string   `json:"type"`
	Values []string `json:"values"`
}

// mdInterleaved is an object on most rows and a bare bool on a few. only the
// field name matters here, so both shapes decode.
type mdInterleaved struct{ Field string }

func (i *mdInterleaved) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || b[0] != '{' {
		return nil
	}
	var obj struct {
		Field string `json:"field"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return err
	}
	i.Field = obj.Field
	return nil
}

// mdModel is one models.dev model row; only the facts the catalog uses.
type mdModel struct {
	ID               string              `json:"id"`
	Name             string              `json:"name"`
	Description      string              `json:"description"`
	Attachment       bool                `json:"attachment"`
	Reasoning        bool                `json:"reasoning"`
	ReasoningOptions []mdReasoningOption `json:"reasoning_options"`
	ToolCall         bool                `json:"tool_call"`
	Interleaved      mdInterleaved       `json:"interleaved"`
	StructuredOutput bool                `json:"structured_output"`
	Temperature      bool                `json:"temperature"`
	Knowledge        string              `json:"knowledge"`
	ReleaseDate      string              `json:"release_date"`
	Modalities       struct {
		Input  []string `json:"input"`
		Output []string `json:"output"`
	} `json:"modalities"`
	OpenWeights bool `json:"open_weights"`
	Limit       struct {
		Context int `json:"context"`
		Output  int `json:"output"`
	} `json:"limit"`
}

// mdProvider holds the rows raw: a row whose shape drifts must be skippable on
// its own instead of failing the whole catalog.
type mdProvider struct {
	Models map[string]json.RawMessage `json:"models"`
}

// mdIndex resolves a nim id to a models.dev row. every provider carries its
// own copy of the same model, so byID keeps the first and byKey matches the
// normalized name across the suffix/date noise.
type mdIndex struct {
	byID  map[string]*mdModel
	byKey map[string]*mdModel
	count int
}

// mdKey normalizes one id for the cross-provider index: lowercase, the free
// marker and a dated build stamp dropped.
func mdKey(id string) string {
	s := strings.ToLower(strings.TrimSpace(id))
	s = strings.TrimSuffix(s, ":free")
	s = strings.TrimSuffix(s, "-free")
	if b, ok := baseModelID(s); ok {
		s = b
	}
	return peelDateStamp(s)
}

// buildMDIndex folds the per-provider rows into one index and reports how
// many rows were dropped as undecodable.
func buildMDIndex(providers map[string]mdProvider) (*mdIndex, int) {
	ix := &mdIndex{
		byID:  make(map[string]*mdModel, 4096),
		byKey: make(map[string]*mdModel, 4096),
	}
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	// sorted so a duplicate id resolves the same way on every boot.
	sort.Strings(names)
	skipped := 0
	for _, name := range names {
		for id, raw := range providers[name].Models {
			if strings.TrimSpace(id) == "" {
				continue
			}
			var m mdModel
			if json.Unmarshal(raw, &m) != nil {
				skipped++
				continue
			}
			if m.ID == "" {
				m.ID = id
			}
			if _, dup := ix.byID[strings.ToLower(id)]; !dup {
				ix.byID[strings.ToLower(id)] = &m
				ix.count++
			}
			if k := mdKey(id); k != "" {
				if _, dup := ix.byKey[k]; !dup {
					ix.byKey[k] = &m
				}
			}
		}
	}
	return ix, skipped
}

// get resolves one nim id: exact, normalized, then with our backend prefix
// stripped (opencode/glm-5-free -> glm-5-free).
func (ix *mdIndex) get(id string) *mdModel {
	if ix == nil || strings.TrimSpace(id) == "" {
		return nil
	}
	low := strings.ToLower(strings.TrimSpace(id))
	if m := ix.byID[low]; m != nil {
		return m
	}
	if k := mdKey(id); k != "" {
		if m := ix.byKey[k]; m != nil {
			return m
		}
	}
	for _, p := range mdPrefixes {
		s := strings.TrimPrefix(low, p)
		if s == low {
			continue
		}
		if m := ix.byID[s]; m != nil {
			return m
		}
		if k := mdKey(s); k != "" {
			if m := ix.byKey[k]; m != nil {
				return m
			}
		}
	}
	return nil
}

var (
	mdMu   sync.RWMutex
	mdLive *mdIndex
)

func setMDIndex(ix *mdIndex) {
	mdMu.Lock()
	defer mdMu.Unlock()
	if ix != nil {
		mdLive = ix
	}
}

// modelsDevIndex returns the live index, nil before the first fetch.
func modelsDevIndex() *mdIndex {
	mdMu.RLock()
	defer mdMu.RUnlock()
	return mdLive
}

func modelsDevEnabled() bool { return cfg().ModelsDev.Enabled }

func fetchModelsDev(url string) (map[string]mdProvider, error) {
	if url == "" {
		url = mdDefaultURL
	}
	cl := &http.Client{Timeout: 120 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "nimroute/"+versionStr)
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: http %d", url, resp.StatusCode)
	}
	var providers map[string]mdProvider
	if err := json.NewDecoder(io.LimitReader(resp.Body, mdMaxBytes)).Decode(&providers); err != nil {
		return nil, err
	}
	if len(providers) == 0 {
		return nil, fmt.Errorf("%s: empty catalog", url)
	}
	return providers, nil
}

// refreshModelsDev is one fetch cycle. a failure keeps the previous index.
func refreshModelsDev() {
	if !modelsDevEnabled() {
		return
	}
	providers, err := fetchModelsDev(cfg().ModelsDev.URL)
	if err != nil {
		log.Printf("WARN: models.dev fetch failed, keeping previous: %v", err)
		return
	}
	ix, skipped := buildMDIndex(providers)
	setMDIndex(ix)
	if skipped > 0 {
		log.Printf("  models.dev: %d models (%d rows skipped as unparseable)", ix.count, skipped)
		return
	}
	log.Printf("  models.dev: %d models", ix.count)
}

func watchModelsDev() {
	if !modelsDevEnabled() {
		return
	}
	go refreshModelsDev()
	h := cfg().ModelsDev.RefreshHours
	if h <= 0 {
		h = 24
	}
	go func() {
		t := time.NewTicker(time.Duration(h) * time.Hour)
		for range t.C {
			refreshModelsDev()
		}
	}()
}

// mdEntry turns a models.dev row into a catalog entry: the donor for a
// synthesized entry and the source in mergeMDEntry.
func mdEntry(m *mdModel) modelInfoEntry {
	e := modelInfoEntry{
		Source:           "models.dev",
		Name:             m.Name,
		Desc:             m.Description,
		Ctx:              m.Limit.Context,
		MaxOut:           m.Limit.Output,
		Inputs:           m.Modalities.Input,
		OutMods:          m.Modalities.Output,
		Tools:            m.ToolCall,
		Temp:             m.Temperature,
		Structured:       m.StructuredOutput,
		Attachment:       m.Attachment,
		OpenWeights:      m.OpenWeights,
		Knowledge:        m.Knowledge,
		Released:         m.ReleaseDate,
		InterleavedField: m.Interleaved.Field,
	}
	if m.Reasoning {
		r := true
		e.Reasoning = &r
	}
	for _, o := range m.ReasoningOptions {
		switch o.Type {
		case "toggle":
			e.ReasoningToggle = true
		case "budget_tokens":
			e.ReasoningBudget = true
		case "effort":
			if len(o.Values) > 0 {
				e.EffortLevels = o.Values
			}
		}
	}
	return e
}

// mergeMDEntry fills the zero fields of dst from src and reports whether
// anything moved. dst always wins; capability bools are carried by a
// synthesized entry, not merged into a curated one.
func mergeMDEntry(dst *modelInfoEntry, src modelInfoEntry) bool {
	changed := mergeOREntry(dst, src)
	if src.ReasoningToggle && !dst.ReasoningToggle {
		dst.ReasoningToggle = true
		changed = true
	}
	if src.ReasoningBudget && !dst.ReasoningBudget {
		dst.ReasoningBudget = true
		changed = true
	}
	if dst.InterleavedField == "" && src.InterleavedField != "" {
		dst.InterleavedField = src.InterleavedField
		changed = true
	}
	return changed
}

// withMDOverlay returns id's entry with models.dev filling the gaps. an id the
// file never listed gets a synthesized entry, so it serves with real numbers
// instead of the 131072 fallback.
func withMDOverlay(id string, e *modelInfoEntry) *modelInfoEntry {
	m := modelsDevIndex().get(id)
	if m == nil {
		return e
	}
	src := mdEntry(m)
	if e == nil {
		return &src
	}
	out := *e
	if mergeMDEntry(&out, src) {
		return &out
	}
	return e
}
