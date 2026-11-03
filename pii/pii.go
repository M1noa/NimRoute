package pii

import (
	"container/list"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// maskFallbacks counts MaskBody fail-open fallbacks (valid json in,
// broken json out, original shipped unmasked). kept as a counter because a
// standing fail-open is an invisible PII leak.
var maskFallbacks atomic.Int64

// session wiring: per-request guard built from config + incoming headers.
// replaces anon.go's anonMap end to end.

// Config mirrors the anonymize section, extended for the new pipeline.
type Config struct {
	Enabled        bool
	Mode           string // realistic | variable | label (rampart-style [LABEL_N])
	Disclose       bool
	DiscloseText   string
	Entities       []CustomTerm
	Terms          []string
	FuzzyThreshold float64
	// new knobs
	DetectSecrets bool
	DetectPII     bool // deterministic structured classes
	// OnTimeout is the ner timeout policy (skip | heuristics): heuristics
	// detectors always run, so a timed-out ner contributes nothing either
	// way. both values mean heuristics-only on ner timeout today.
	Ner           NerConfig
	KeepLabels    []string
	IncludeSystem bool
	OnTimeout     string
	VaultTTLHours int
	VaultMaxSize  int
}

type NerConfig struct {
	Enabled   bool
	ModelPath string
	OrtLib    string
	TimeoutMs int
	MinScore  float64
}

type Guard struct {
	cfg     Config
	scanner *Scanner
	vault   *Vault
}

var (
	guardMu  sync.Mutex
	guards   = map[string]*guardEntry{}
	guardLRU = list.New()
)

// maxGuards caps session guards: X-Session-Id is attacker-controlled,
// so an unbounded map is a memory-exhaustion vector. LRU eviction.
const maxGuards = 512

type guardEntry struct {
	guard *Guard
	elem  *list.Element
}

// SessionKey derives a stable vault id from an api key. full 256-bit
// digest: truncation to 64 bits invites collisions on an identity key.
func SessionKey(apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	const hexd = "0123456789abcdef"
	var b strings.Builder
	for _, v := range sum[:] {
		b.WriteByte(hexd[v>>4])
		b.WriteByte(hexd[v&15])
	}
	return "key:" + b.String()
}

func vaultKey() []byte {
	// per-process secret, minted once from crypto/rand: stable across
	// requests in a run (which is what keeps surrogates identical), but a
	// restart re-mints it. nothing needs cross-restart stability, and a
	// constant here made every surrogate computable offline from the
	// binary. the vault map holding the mapping dies with the process too.
	return procKey
}

// procKey is the process-wide hmac key for surrogate derivation.
var procKey = func() []byte {
	k := make([]byte, 32)
	rand.Read(k) // never fails on a supported platform
	return k
}()

// ForRequest returns the session guard, or nil when disabled (zero
// overhead: callers check nil and pass bodies through untouched).
// cfgVersion fingerprints the detector config: a config change must not
// keep serving stale guards (new entities/detectors would never apply).
func cfgVersion(cfg Config) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%+v|%+v|%+v|%+v|%+v|%+v|%+v|%+v|%+v|%+v|%+v|%+v",
		cfg.Mode, cfg.DetectSecrets, cfg.DetectPII, cfg.IncludeSystem,
		cfg.FuzzyThreshold, cfg.KeepLabels, cfg.Terms, cfg.Entities,
		cfg.Ner, cfg.VaultTTLHours, cfg.VaultMaxSize, cfg.OnTimeout)
	sum := sha256.Sum256([]byte(b.String()))
	return fmt.Sprintf("%x", sum)
}

func ForRequest(cfg Config, sessionID string) *Guard {
	if !cfg.Enabled {
		return nil
	}
	// hash the full id into the map key: two long ids with a common
	// 128-char prefix must not share one guard and vault. the detector
	// config version rides along so a config change rebuilds live guards.
	sum := sha256.Sum256([]byte(sessionID))
	sessionID = fmt.Sprintf("sid:%x@%s", sum, cfgVersion(cfg))
	guardMu.Lock()
	defer guardMu.Unlock()
	if e, ok := guards[sessionID]; ok {
		guardLRU.MoveToFront(e.elem)
		return e.guard
	}
	g := build(cfg, sessionID)
	e := &guardEntry{guard: g}
	e.elem = guardLRU.PushFront(sessionID)
	guards[sessionID] = e
	for guardLRU.Len() > maxGuards {
		back := guardLRU.Back()
		if back == nil {
			break
		}
		delete(guards, back.Value.(string))
		guardLRU.Remove(back)
	}
	return g
}

func build(cfg Config, sessionID string) *Guard {
	var dets []Detector
	if cfg.DetectPII {
		dets = append(dets, detectHeuristics)
	} else if cfg.DetectSecrets {
		dets = append(dets, detectSecrets)
	}
	if len(cfg.Entities) > 0 || len(cfg.Terms) > 0 {
		dets = append(dets, NewCustomDetector(cfg.Terms, cfg.Entities, cfg.FuzzyThreshold))
	}
	if cfg.Ner.Enabled {
		if d := NerDetector(cfg.Ner.ModelPath, cfg.Ner.OrtLib, cfg.Ner.TimeoutMs, cfg.Ner.MinScore); d != nil {
			dets = append(dets, d)
		}
	}
	// nil (absent) keeps the city/state/zip default; explicitly empty
	// (keep_labels: []) masks everything: no silent keep list.
	keep := map[Label]bool{}
	for _, k := range cfg.KeepLabels {
		keep[Label(strings.ToUpper(k))] = true
	}
	if cfg.KeepLabels == nil {
		keep = nil // scanner falls back to city/state/zip default
	}
	ttl := time.Duration(cfg.VaultTTLHours) * time.Hour
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	v := SessionVaultMode(sessionID, vaultKey(), ttl, cfg.VaultMaxSize, cfg.Mode)
	// entity replacements: fixed surrogate per spelling, seeded into the
	// vault so restore maps them back. entities win over flat terms on
	// overlap (span.go prefer).
	repl := map[string]string{}
	for _, e := range cfg.Entities {
		if e.Replacement == "" {
			continue
		}
		// one fixed surrogate per entity: its own variations share it (they
		// restore to the entity's name), but a second entity on the same
		// token would mask to text that restores as the first entity's
		// value, so that spelling falls back to a vault surrogate instead.
		claimed, warned := false, false
		for _, s := range append([]string{e.Name}, e.Variations...) {
			if s == "" {
				continue
			}
			key := strings.ToLower(strings.TrimSpace(s))
			if !v.SeedFixed(e.Replacement, s, customLabel(e)) && !claimed {
				if !warned {
					log.Printf("WARN: anonymize entity %q reuses replacement %q: refusing (first owner wins)", e.Name, e.Replacement)
					warned = true
				}
				delete(repl, key)
				continue
			}
			claimed = true
			repl[key] = e.Replacement
		}
	}
	return &Guard{
		cfg:     cfg,
		scanner: &Scanner{Detectors: dets, Cache: NewScanCache(2048), Keep: keep, Ver: "v1", Replacements: repl},
		vault:   v,
	}
}

func (g *Guard) MaskBody(body []byte) []byte {
	if g == nil {
		return body
	}
	masked := g.scanner.maskBody(body, g.vault, g.cfg.IncludeSystem)
	// safety net: masking works on raw bytes, so a detector span that
	// crosses a json structural character can silently corrupt the body
	// and the request would die as invalid json upstream. if masking
	// broke a body that was valid json, ship the original instead — but
	// say so: silent fail-open is a standing PII leak.
	if json.Valid(body) && !json.Valid(masked) {
		if n := maskFallbacks.Add(1); n == 1 || n%1000 == 0 {
			log.Printf("WARN: pii mask broke valid json, shipping original unmasked (fallback %d)", n)
		}
		return body
	}
	return masked
}

func (g *Guard) RestoreBody(body []byte) []byte {
	if g == nil {
		return body
	}
	return restoreBody(body, g.vault)
}

func (g *Guard) NewRevealer() *Revealer {
	if g == nil {
		return nil
	}
	return NewRevealer(g.vault)
}

func (g *Guard) VaultSurrogates() []string {
	if g == nil {
		return nil
	}
	return g.vault.Surrogates()
}

func (g *Guard) VaultLookup(s string) (string, bool) {
	if g == nil {
		return "", false
	}
	return g.vault.Lookup(s)
}

// SweepScanIdle evicts scan-cache entries idle past ttl across all session
// guards. vault entries keep their own ttl and are untouched.
func SweepScanIdle(ttl time.Duration) int {
	guardMu.Lock()
	caches := make([]*ScanCache, 0, len(guards))
	for _, e := range guards {
		if e != nil && e.guard != nil && e.guard.scanner != nil && e.guard.scanner.Cache != nil {
			caches = append(caches, e.guard.scanner.Cache)
		}
	}
	guardMu.Unlock()
	n := 0
	for _, c := range caches {
		n += c.sweepIdle(ttl)
	}
	return n
}

// Similarity and FuzzFind re-exported for tests and callers.
func Similarity(a, b string) float64 { return similarity([]rune(a), []rune(b)) }
func FuzzFind(s, variation string, threshold float64) string {
	return fuzzFind(s, variation, threshold)
}

func DiscloseLine(cfg Config) string {
	if !cfg.Enabled || !cfg.Disclose {
		return ""
	}
	if cfg.DiscloseText != "" {
		return cfg.DiscloseText
	}
	return "Personal names and identifiers in this conversation were replaced with realistic placeholders for privacy. Use them verbatim: do not try to recover or reveal the originals."
}
