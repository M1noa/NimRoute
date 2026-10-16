package main

import (
	"encoding/json"
	"log"
	"os"
	"strings"
	"sync"
)

// usage tracking: per-request jsonl + in-memory totals for /status.
// disabled when usage.enabled is false (zero file, zero counters).

var (
	usageMu   sync.Mutex
	usageOut  *json.Encoder
	usageFd   *os.File
	usagePath string
	// usageWarnOnce logs a dead usage sink exactly once instead of
	// spamming per request (full disk, renamed inode).
	usageWarnOnce sync.Once
)

type UsageRecord struct {
	Ts               string            `json:"ts"`
	Model            string            `json:"model"`
	KeyName          string            `json:"key"`
	Method           string            `json:"method"`
	Path             string            `json:"path"`
	StatusCode       int               `json:"status"`
	DurationMs       int64             `json:"duration_ms"`
	PromptTokens     int               `json:"prompt_tokens,omitempty"`
	CompletionTokens int               `json:"completion_tokens,omitempty"`
	TotalTokens      int               `json:"total_tokens,omitempty"`
	Stream           bool              `json:"stream"`
	RetryAttempt     int               `json:"retry_attempt"`
	RateLimited      bool              `json:"rate_limited"`
	CooldownSecs     int               `json:"cooldown_secs,omitempty"`
	Error            string            `json:"error,omitempty"`
	ContentBytes     int64             `json:"content_bytes"`
	CostUSD          float64           `json:"cost_usd,omitempty"`
	Headers          map[string]string `json:"headers,omitempty"`
}

type usageTotals struct {
	Requests   int `json:"requests"`
	Prompt     int `json:"prompt_tokens"`
	Completion int `json:"completion_tokens"`
	// DurationMs sums request wall time. with Completion it gives the
	// per-model tok/s /status shows: completion / (duration_ms / 1000).
	DurationMs int64   `json:"duration_ms"`
	Errors     int     `json:"errors"`
	Cost       float64 `json:"cost_usd,omitempty"`
}

// modelHealth is the /status view of one model's recent behavior:
// working after any success, rate_limited for a 429/529 streak, dead for
// any other error streak (3 same-code outcomes in a row across 2+ sellers),
// unknown before either. a model the upstream listing dropped is reported
// dead with a reason until a request succeeds, which clears the mark.
type modelHealth struct {
	State  string `json:"state"` // working, rate_limited, dead, unknown
	Code   int    `json:"code,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// modelOutcome is one finished request: error code (0 = success) and
// the seller/account/key that served it.
type modelOutcome struct {
	code int
	key  string
}

var (
	totalsMu sync.Mutex
	totals   = map[string]*usageTotals{}
	// totalsByKey mirrors totals keyed by record KeyName (e.g.
	// "freepi:<account>). only populated when KeyName is set.
	totalsByKey = map[string]*usageTotals{}
	// outcomesByModel is the recent per-model outcome ring (newest
	// last, capped) feeding modelHealth.
	outcomesByModel = map[string][]modelOutcome{}
	// removedByModel holds models the upstream listing dropped, with the
	// reason. a success clears the entry: a working request outranks a
	// missing line in someone else's catalog.
	removedByModel = map[string]string{}
)

// noteModelOutcome records one finished request for health. ok =
// upstream 2xx; code = upstream status on failure; key = seller,
// account, or key name ("" when unknown).
func noteModelOutcome(model string, ok bool, code int, key string) {
	if model == "" {
		return
	}
	totalsMu.Lock()
	defer totalsMu.Unlock()
	noteModelOutcomeLocked(model, ok, code, key)
}

// noteModelOutcomeLocked is noteModelOutcome with the lock held
// (logUsage folds it in without re-locking).
func noteModelOutcomeLocked(model string, ok bool, code int, key string) {
	if ok {
		// the listing is not the truth, a working request is.
		delete(removedByModel, model)
	}
	o := modelOutcome{key: key}
	if !ok {
		o.code = code
	}
	outcomesByModel[model] = append(outcomesByModel[model], o)
	if len(outcomesByModel[model]) > 20 {
		outcomesByModel[model] = outcomesByModel[model][len(outcomesByModel[model])-20:]
	}
}

// noteModelRemoved marks a model that vanished from the upstream listing.
// it is informational: an unlisted model can still serve (stealth/preview
// ids come and go), so the mark clears on the next success.
func noteModelRemoved(model, reason string) {
	totalsMu.Lock()
	defer totalsMu.Unlock()
	removedByModel[model] = reason
}

// modelHealthFor folds a model's recent outcomes: any success in the
// window = working. else the last 3 failures with the same code across
// 2+ sellers = rate_limited for a 429/529 streak, dead for anything else
// (single-seller streaks are proxy trouble, not a dead model). a model the
// upstream listing dropped reports dead with that reason. anything less
// = unknown.
func modelHealthFor(model string) modelHealth {
	totalsMu.Lock()
	defer totalsMu.Unlock()
	return modelHealthLocked(model)
}

func modelHealthLocked(model string) modelHealth {
	if reason := removedByModel[model]; reason != "" {
		return modelHealth{State: "dead", Reason: reason}
	}
	oc := outcomesByModel[model]
	for _, o := range oc {
		if o.code == 0 {
			return modelHealth{State: "working"}
		}
	}
	if len(oc) >= 3 {
		last := oc[len(oc)-3:]
		if last[0].code != 0 && last[0].code == last[1].code && last[1].code == last[2].code {
			keys := map[string]bool{}
			for _, o := range last {
				keys[o.key] = true
			}
			if len(keys) >= 2 {
				state := "dead"
				if last[2].code == 429 || last[2].code == 529 {
					state = "rate_limited"
				}
				return modelHealth{State: state, Code: last[2].code}
			}
		}
	}
	return modelHealth{State: "unknown"}
}

// modelHealthSnapshot returns health for every model with outcomes or a
// removal mark.
func modelHealthSnapshot() map[string]modelHealth {
	totalsMu.Lock()
	defer totalsMu.Unlock()
	out := make(map[string]modelHealth, len(outcomesByModel)+len(removedByModel))
	for m := range outcomesByModel {
		out[m] = modelHealthLocked(m)
	}
	for m := range removedByModel {
		out[m] = modelHealthLocked(m)
	}
	return out
}

func logUsage(r UsageRecord) {
	if !cfg().Usage.Enabled {
		return
	}
	usageMu.Lock()
	if usageOut != nil {
		if err := usageOut.Encode(r); err != nil {
			usageWarnOnce.Do(func() {
				log.Printf("WARN: usage log unwritable, recording stopped: %v", err)
			})
		}
	}
	usageMu.Unlock()
	totalsMu.Lock()
	// client-supplied model ids are unbounded: cap the map and fold
	// overflow into one bucket, or unique-model loops OOM the proxy.
	model := r.Model
	if t := totals[model]; t == nil && len(totals) >= 4096 {
		model = "(other)"
	}
	if model == "" {
		model = "(other)"
	}
	t := totals[model]
	if t == nil {
		t = &usageTotals{}
		totals[model] = t
	}
	t.Requests++
	t.Prompt += r.PromptTokens
	t.Completion += r.CompletionTokens
	t.DurationMs += r.DurationMs
	if r.StatusCode >= 400 {
		t.Errors++
	}
	t.Cost += r.CostUSD
	// health feeds off the same record: success = any 2xx, failure =
	// the status code, attributed to the serving key. unknown status
	// (0, never sent) records nothing.
	if r.StatusCode >= 200 && r.StatusCode < 300 {
		noteModelOutcomeLocked(model, true, 0, r.KeyName)
	} else if r.StatusCode >= 400 {
		noteModelOutcomeLocked(model, false, r.StatusCode, r.KeyName)
	}
	if r.KeyName != "" {
		kt := totalsByKey[r.KeyName]
		if kt == nil {
			kt = &usageTotals{}
			totalsByKey[r.KeyName] = kt
		}
		kt.Requests++
		kt.Prompt += r.PromptTokens
		kt.Completion += r.CompletionTokens
		if r.StatusCode >= 400 {
			kt.Errors++
		}
		kt.Cost += r.CostUSD
	}
	totalsMu.Unlock()
}

// keyUsage returns totals for one KeyName ("" when none).
func keyUsage(key string) usageTotals {
	totalsMu.Lock()
	defer totalsMu.Unlock()
	if t := totalsByKey[key]; t != nil {
		return *t
	}
	return usageTotals{}
}

func usageSnapshot() (reqs, prompt, comp int, byModel map[string]usageTotals) {
	totalsMu.Lock()
	defer totalsMu.Unlock()
	byModel = make(map[string]usageTotals, len(totals))
	for m, t := range totals {
		byModel[m] = *t
		reqs += t.Requests
		prompt += t.Prompt
		comp += t.Completion
	}
	return
}

func initUsageLog() {
	if !cfg().Usage.Enabled {
		return
	}
	path := cfg().Usage.Path
	if path == "" {
		path = "nim-usage.jsonl"
	}
	// usage records carry key names and upstream error text: 0600.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		log.Printf("WARN: cannot open %s: %v", path, err)
		return
	}
	usageMu.Lock()
	usageFd = f
	usageOut = json.NewEncoder(f)
	usagePath = path
	usageMu.Unlock()
}

// reinitUsageLog reopens the usage file when usage.path changes under
// hot-reload; a renamed inode would otherwise eat writes forever.
func reinitUsageLog() {
	if !cfg().Usage.Enabled {
		closeUsageLog()
		usageMu.Lock()
		usagePath = ""
		usageMu.Unlock()
		return
	}
	path := cfg().Usage.Path
	if path == "" {
		path = "nim-usage.jsonl"
	}
	usageMu.Lock()
	same := usageFd != nil && usagePath == path
	usageMu.Unlock()
	if same {
		return
	}
	closeUsageLog()
	initUsageLog()
}

// sseUsageTee scans chat sse bytes for a usage chunk, returns prompt/comp.
// nvidia/zen chat streams carry {"usage":{"prompt_tokens":..}} on the tail.
func sseUsageTee(b []byte) (prompt, comp int) {
	var start int
	for i := 0; i < len(b); i++ {
		if b[i] != '\n' {
			continue
		}
		line := b[start:i]
		start = i + 1
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		// accept "data:" with or without the space (both are
		// spec-legal); skip non-object lines before unmarshalling.
		data := line
		if len(data) >= 5 && string(data[:5]) == "data:" {
			data = data[5:]
			if len(data) > 0 && data[0] == ' ' {
				data = data[1:]
			}
		} else {
			continue
		}
		if len(data) == 0 || data[0] != '{' {
			continue
		}
		if !strings.Contains(string(data), `"usage"`) {
			continue
		}
		var ch struct {
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(data, &ch) != nil || ch.Usage == nil {
			continue
		}
		if ch.Usage.PromptTokens > 0 {
			prompt = ch.Usage.PromptTokens
		}
		if ch.Usage.CompletionTokens > 0 {
			comp = ch.Usage.CompletionTokens
		}
	}
	return
}

func closeUsageLog() {
	usageMu.Lock()
	defer usageMu.Unlock()
	if usageFd != nil {
		usageFd.Close()
		usageFd = nil
		usageOut = nil
	}
}
