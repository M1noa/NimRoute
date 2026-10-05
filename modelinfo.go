package main

import (
	"encoding/json"
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
	Reasoning bool     `json:"reasoning,omitempty"`
	Tools     bool     `json:"tools,omitempty"`
	Temp      bool     `json:"supports_temperature,omitempty"`
	Params    string   `json:"params,omitempty"`
	Nudge     *bool    `json:"nudge_no_tools,omitempty"`
	Mult      string   `json:"freepi_mult,omitempty"`
	Free      bool     `json:"free,omitempty"`
	Source    string   `json:"source,omitempty"`
}

var (
	modelInfoMu sync.RWMutex
	modelInfo   = map[string]modelInfoEntry{}
)

// stripJSONComments drops // line comments (values never contain them:
// the file is machine-generated, one entry per line).
func stripJSONComments(raw []byte) []byte {
	var out []string
	for _, l := range strings.Split(string(raw), "\n") {
		if i := strings.Index(l, "//"); i >= 0 {
			l = l[:i]
		}
		if strings.TrimSpace(l) == "" {
			continue
		}
		out = append(out, l)
	}
	return []byte(strings.Join(out, "\n"))
}

// loadModelInfo parses path into a lookup map. missing/unreadable file =
// empty map, callers fall back to defaults.
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

// modelInfoPath is the catalog file next to the working dir.
func modelInfoPath() string {
	if e := os.Getenv("MODEL_INFO_FILE"); e != "" {
		return e
	}
	return "model_info.jsonc"
}

// setModelInfo swaps the catalog (startup, reload, tests).
func setModelInfo(m map[string]modelInfoEntry) {
	modelInfoMu.Lock()
	defer modelInfoMu.Unlock()
	if m == nil {
		m = map[string]modelInfoEntry{}
	}
	modelInfo = m
}

// lookupModelInfo finds an entry for a full or bare model id: exact, then
// with the nvidia/ prefix stripped (file keys nim ids bare).
func lookupModelInfo(id string) *modelInfoEntry {
	modelInfoMu.RLock()
	defer modelInfoMu.RUnlock()
	if e, ok := modelInfo[id]; ok {
		return &e
	}
	if s := strings.TrimPrefix(id, "nvidia/"); s != id {
		if e, ok := modelInfo[s]; ok {
			return &e
		}
	}
	return nil
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

// modelNudge reports the nudge_no_tools flag: config override first, then
// the catalog entry, else spark-shaped models default on.
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
	return strings.Contains(ml, "spark")
}

// multRe parses the usage multiplier out of a freepi catalog display name:
// "GLM 5.3 Flash · ~0.7x usage" -> "~0.7x".
var multRe = regexp.MustCompile(`(~?[\d.]+x)\s+usage`)

// globMatch is * glob matching, first-match-wins order left to callers.
func globMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	for _, part := range parts {
		if part == "" {
			continue
		}
		idx := strings.Index(s, part)
		if idx < 0 {
			return false
		}
		s = s[idx+len(part):]
	}
	return true
}

// freepiMultFromName extracts the multiplier from a catalog display name,
// "" when the name carries none.
func freepiMultFromName(name string) string {
	if m := multRe.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return ""
}
