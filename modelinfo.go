package main

import (
	"encoding/json"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
)

// model info lives in model_info.jsonc, not config.yml. generated from nim
// cards + opencode data + hf configs; per-entry "source" says where.
// hot-reloaded with the config (applyConfig), tests swap it via setModelInfo.

type modelInfoEntry struct {
	Name      string   `json:"name,omitempty"`
	Desc      string   `json:"desc,omitempty"`
	Ctx       int      `json:"ctx,omitempty"`
	MaxOut    int      `json:"maxout,omitempty"`
	Inputs    []string `json:"inputs,omitempty"`
	OutMods   []string `json:"output_modalities,omitempty"`
	Reasoning *bool    `json:"reasoning,omitempty"`
	// EffortLevels are the accepted reasoning_effort values, in order.
	EffortLevels []string `json:"reasoning_effort_levels,omitempty"`
	// ReasoningToggle marks models with an on/off switch instead of levels.
	ReasoningToggle bool `json:"reasoning_toggle,omitempty"`
	// ReasoningBudget marks models that take a token budget, not levels.
	ReasoningBudget  bool   `json:"reasoning_budget,omitempty"`
	InterleavedField string `json:"interleaved_reasoning_field,omitempty"`
	Tools            bool   `json:"tools,omitempty"`
	Structured       bool   `json:"structured_output,omitempty"`
	Attachment       bool   `json:"attachment,omitempty"`
	Temp             bool   `json:"supports_temperature,omitempty"`
	// SupportedParams is the request-parameter list (openrouter). empty =
	// no record, not "nothing supported".
	SupportedParams []string `json:"supported_parameters,omitempty"`
	Params          string   `json:"params,omitempty"`
	Nudge           *bool    `json:"nudge_no_tools,omitempty"`
	Mult            string   `json:"freepi_mult,omitempty"`
	// freepi served contract from the free-pi-cli provider config: the
	// free-pi proxy layer's own window, not the underlying model's.
	FreepiCtx       int      `json:"freepi_context,omitempty"`
	FreepiMaxOut    int      `json:"freepi_maxout,omitempty"`
	FreepiReasoning bool     `json:"freepi_declared_reasoning,omitempty"`
	FreepiInputs    []string `json:"freepi_declared_inputs,omitempty"`
	Free            bool     `json:"free,omitempty"`
	Knowledge       string   `json:"knowledge_cutoff,omitempty"`
	Released        string   `json:"released,omitempty"`
	OpenWeights     bool     `json:"open_weights,omitempty"`
	Family          string   `json:"family,omitempty"`
	UpstreamStatus  string   `json:"upstream_status,omitempty"`
	Source          string   `json:"source,omitempty"`
}

var (
	modelInfoMu sync.RWMutex
	modelInfo   = map[string]modelInfoEntry{}
)

// stripJSONComments drops full-line // comments. inline truncation is
// wrong (values may contain //), so only lines whose first non-space
// characters are // are dropped.
func stripJSONComments(raw []byte) []byte {
	var out []string
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "//") {
			continue
		}
		if strings.TrimSpace(l) == "" {
			continue
		}
		out = append(out, l)
	}
	return []byte(strings.Join(out, "\n"))
}

// loadModelInfoKeep parses path, keeping the live catalog when the file is
// present but broken: a truncated save must not wipe every model fact.
// returns nil when there is nothing usable (caller keeps the old map).
func loadModelInfoKeep(path string) map[string]modelInfoEntry {
	raw, err := os.ReadFile(path)
	if err != nil {
		return map[string]modelInfoEntry{}
	}
	var m map[string]modelInfoEntry
	if err := json.Unmarshal(stripJSONComments(raw), &m); err != nil {
		log.Printf("WARN: model catalog %s unparseable, keeping previous: %v", path, err)
		return nil
	}
	return m
}

// modelInfoPath is the catalog file next to the working dir.
func modelInfoPath() string {
	if e := os.Getenv("MODEL_INFO_FILE"); e != "" {
		return e
	}
	return "model_info.jsonc"
}

// setModelInfo swaps the catalog (startup, reload, tests). nil keeps the
// live map: loadModelInfoKeep returns nil on a broken file.
func setModelInfo(m map[string]modelInfoEntry) {
	if m == nil {
		return
	}
	modelInfoMu.Lock()
	defer modelInfoMu.Unlock()
	modelInfo = m
}

// lookupModelInfo finds an entry for a full or bare model id: exact, then
// with the nvidia/ prefix stripped (file keys nim ids bare), then with a
// trailing [1m] stripped when the suffixed id is unknown (claude code 1m
// context alias: the base model serves it).
//
// the openrouter overlay is applied last: it fills fields the catalog leaves
// empty and synthesizes an entry for ids the file never listed, but never
// overrides a curated value.
func lookupModelInfo(id string) *modelInfoEntry {
	modelInfoMu.RLock()
	defer modelInfoMu.RUnlock()
	if e, ok := modelInfo[id]; ok {
		return withOrMD(id, &e)
	}
	if s := strings.TrimPrefix(id, "nvidia/"); s != id {
		if e, ok := modelInfo[s]; ok {
			return withOrMD(id, &e)
		}
	}
	if s, ok := baseModelID(id); ok {
		if e, ok := modelInfo[s]; ok {
			return withOrMD(id, &e)
		}
	}
	return withOrMD(id, nil)
}

// withOrMD applies the network overlays in order: openrouter (per-model, and
// the source of the /v1/models extras) then models.dev (the wider catalog).
// each only fills what the one before it left empty.
func withOrMD(id string, e *modelInfoEntry) *modelInfoEntry {
	return withMDOverlay(id, withOROverlay(id, e))
}

// baseModelID strips the trailing [1m] context alias: ("<id>[1m]", "<id>",
// true), "" when the id carries no such suffix.
func baseModelID(id string) (string, bool) {
	if s := strings.TrimSuffix(id, "[1m]"); s != id {
		return s, true
	}
	return "", false
}

// modelCtx returns the info context length, 0 when unknown.
func modelCtx(id string) int {
	if e := lookupModelInfo(id); e != nil && e.Ctx > 0 {
		return e.Ctx
	}
	return 0
}

// modelMaxOut returns the info max output tokens, 0 when unknown.
func modelMaxOut(id string) int {
	if e := lookupModelInfo(id); e != nil && e.MaxOut > 0 {
		return e.MaxOut
	}
	return 0
}

// modelDesc returns the info description, "" when unknown.
func modelDesc(id string) string {
	if e := lookupModelInfo(id); e != nil {
		return e.Desc
	}
	return ""
}

// modelName prefers the info name, else the derived display name.
func modelName(id string) string {
	if e := lookupModelInfo(id); e != nil && e.Name != "" {
		return e.Name
	}
	return modelDisplayName(id)
}

// modelModality maps info inputs to the openrouter modality string.
func modelModality(id string) string {
	if e := lookupModelInfo(id); e != nil {
		for _, in := range e.Inputs {
			switch in {
			case "image", "video", "audio", "pdf":
				return "text+image->text"
			}
		}
	}
	return "text->text"
}

// modelFreepiReasoning reports whether the free-pi gateway declares the
// model reasoning-capable. the entry's top-level reasoning flag describes
// the underlying model, not the served contract: freepi_declared_reasoning
// wins for freeepi/ ids.
func modelFreepiReasoning(id string) bool {
	if e := lookupModelInfo(id); e != nil {
		return e.FreepiReasoning
	}
	return false
}

// modelServedCtx is the window the proxy layer itself serves: freepi runs
// its own gateway with its own limit, so the free-pi contract wins there.
// otherwise the upstream model's own context.
func modelServedCtx(id string) int {
	if e := lookupModelInfo(id); e != nil && e.FreepiCtx > 0 {
		return e.FreepiCtx
	}
	return modelCtx(id)
}

// modelServedMaxOut is the output ceiling the proxy layer serves.
func modelServedMaxOut(id string) int {
	if e := lookupModelInfo(id); e != nil && e.FreepiMaxOut > 0 {
		return e.FreepiMaxOut
	}
	return modelMaxOut(id)
}

// modelEffortLevels returns the accepted reasoning_effort values, in order.
// empty for models that take a toggle or a token budget instead.
func modelEffortLevels(id string) []string {
	if e := lookupModelInfo(id); e != nil {
		return e.EffortLevels
	}
	return nil
}

// modelReasoningControl describes how thinking is selected: levels, an
// on/off toggle, a token budget, or nothing (not a reasoning model).
func modelReasoningControl(id string) (levels []string, toggle, budget bool) {
	e := lookupModelInfo(id)
	if e == nil || e.Reasoning == nil || !*e.Reasoning {
		return nil, false, false
	}
	return e.EffortLevels, e.ReasoningToggle, e.ReasoningBudget
}

// modelDefaultEffort is the reasoning effort injected when the request
// names none: "high" when listed, else the closest level to high, else
// "medium" for toggle/budget reasoning models. "" for non-reasoning models
// (caller skips the reasoning block then) and unknown ids.
func modelDefaultEffort(id string) string {
	levels, toggle, budget := modelReasoningControl(id)
	if len(levels) > 0 {
		for _, want := range []string{"high", "medium", "low"} {
			for _, l := range levels {
				if l == want {
					return l
				}
			}
		}
		return levels[len(levels)-1]
	}
	if e := lookupModelInfo(id); e != nil && e.Reasoning != nil && *e.Reasoning && (toggle || budget) {
		return "medium"
	}
	return ""
}

// nudge overrides come from config.yml models.nudge (glob, first match
// wins). rebuilt in applyConfig next to the catalog load.
type nudgeOverride struct {
	pattern string
	nudge   bool
}

var (
	nudgeMu        sync.RWMutex
	nudgeOverrides []nudgeOverride
)

// setNudgeOverrides swaps the config nudge list (applyConfig, tests).
func setNudgeOverrides(v []nudgeOverride) {
	nudgeMu.Lock()
	defer nudgeMu.Unlock()
	nudgeOverrides = v
}

// modelNudge reports whether a model takes part in nudging. nudge.enabled is
// the master switch and it means every model: config overrides (models.nudge)
// and an explicit catalog nudge_no_tools are the only ways to opt one out, so
// a provider the catalog has never heard of still gets nudged.
func modelNudge(id string) bool {
	nudgeMu.RLock()
	ml := strings.ToLower(id)
	for _, o := range nudgeOverrides {
		if globMatch(o.pattern, ml) {
			nudgeMu.RUnlock()
			return o.nudge
		}
	}
	nudgeMu.RUnlock()
	if e := lookupModelInfo(id); e != nil && e.Nudge != nil {
		return *e.Nudge
	}
	return true
}

// multRe parses the usage multiplier out of a freepi catalog display name:
// "GLM 5.3 Flash · ~0.7x usage" -> "~0.7x".
var multRe = regexp.MustCompile(`(~?[\d.]+x)\s+usage`)

// globMatch is * glob matching, anchored at both ends: the first part must be
// a prefix, the last part a suffix, and the middle parts keep their order.
// a pattern with no * is an exact match, not a substring -- users write these
// patterns (route.exclude_models, models.claude_map, models.nudge) as globs,
// and locating the parts anywhere in the id silently over-matched them.
// first-match-wins order left to callers.
func globMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	last := parts[len(parts)-1]
	for _, part := range parts[1 : len(parts)-1] {
		idx := strings.Index(s, part)
		if idx < 0 {
			return false
		}
		s = s[idx+len(part):]
	}
	return strings.HasSuffix(s, last)
}

// freepiMultFromName extracts the multiplier from a catalog display name,
// "" when the name carries none.
func freepiMultFromName(name string) string {
	if m := multRe.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return ""
}
