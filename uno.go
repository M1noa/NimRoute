package main

// uno.go is the unorouter backend (api.unorouter.com), built on the shared
// keyedchat engine. the catalog GET /models is key-gated and its edge mimics
// openrouter's, so the nimroute UA goes on it too. only models whose id
// carries the :free suffix and whose supported_endpoint_types include openai
// are served by default; embedding/image-generation/aihorde rows never are.
// uno.paid_models serves the paid catalog too, uno.model_whitelist adds exact
// ids the catalog omits.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// unoBase is the openai-shaped root plus the /models prefix used by handleChat.
var unoBase = "https://api.unorouter.com/v1"

// unoPool is the uno key pool, fed from uno_keys in config.yml.
var unoPool = &keyedPool{name: "uno"}

// unoCatalog caches the listing for 6h: the :free filter is slender, so a
// stale list beats hammering the endpoint.
var unoCatalog = &keyedCatalog{
	name: "uno",
	url:  unoBase + "/models",
	pool: unoPool,
	ttl:  6 * time.Hour,
}

// unoSyncKeys replaces the uno key pool on config load.
func unoSyncKeys(m map[string]string, paid []string) { unoPool.syncKeys(m, paid) }

// unoEnabled gates routing and listing: the config flag plus at least one
// configured key.
func unoEnabled() bool { return cfg().Uno.Enabled && unoPool.size() > 0 }

// unoFreeID reports a free-tier row: the suffix marks it, not the price.
func unoFreeID(id string) bool { return strings.HasSuffix(id, ":free") }

// unoPolicies is the config state one call needs.
func unoPolicies() (bool, map[string]bool) {
	c := cfg().Uno
	return c.PaidModels, keyedWhitelist("uno/", c.Whitelist)
}

// unoAllowed reports whether one id may be forwarded under the policy, price
// cap included: a hidden model must not still answer a hand-written id.
func unoAllowed(id string) bool {
	paid, wl := unoPolicies()
	if !keyedWanted(id, unoFreeID(id), paid, wl) {
		return false
	}
	return keyedUnderPriceCap(unoPriceFor, cfg().Uno.MaxPrice1M, id)
}

// unoModels lists the catalog through the free/openai-chat filter and the
// optional price cap.
func unoModels() []keyedModel {
	paid, wl := unoPolicies()
	rows := keyedServeRows(unoCatalog.models(), unoFreeID, paid, wl)
	cap := cfg().Uno.MaxPrice1M
	if cap <= 0 {
		return rows
	}
	kept := rows[:0]
	for _, m := range rows {
		if keyedUnderPriceCap(unoPriceFor, cap, m.ID) {
			kept = append(kept, m)
		}
	}
	return kept
}

// unoPricingURL is uno's full price catalog. it answers without a key and
// carries what /models never does: usd per million input and output tokens
// per row. /models is the only listing we read that quotes no price at all.
const unoPricingURL = "https://api.unorouter.com/api/pricing/catalog"

// unoPriceRow is the subset of a pricing row we serve: the id it belongs to,
// its two per-million prices, and the capability tags uno states for it.
type unoPriceRow struct {
	Name   string   `json:"model_name"`
	Input  float64  `json:"input_price"`
	Output float64  `json:"output_price"`
	Vendor string   `json:"vendor"`
	Tags   []string `json:"tags"`
}

var unoPrice struct {
	mu   sync.Mutex
	rows map[string]unoPriceRow
	at   time.Time
	next time.Time
}

// unoPriceRows returns the cached price catalog, refetching every 6h with a
// 30s floor, and keeps the last good rows when a fetch fails — the same rule
// the model listing follows.
func unoPriceRows() map[string]unoPriceRow {
	unoPrice.mu.Lock()
	rows := unoPrice.rows
	if len(rows) > 0 && time.Since(unoPrice.at) < 6*time.Hour {
		unoPrice.mu.Unlock()
		return rows
	}
	if time.Now().Before(unoPrice.next) {
		unoPrice.mu.Unlock()
		return rows
	}
	unoPrice.next = time.Now().Add(30 * time.Second)
	unoPrice.mu.Unlock()
	// the fetch is a 15s-timeout upstream call and callers ask for a price
	// while building the route table, so it never runs on the request path:
	// kick it and serve the last good rows meanwhile.
	go unoRefreshPrices()
	return rows
}

// unoRefreshPrices pulls the catalog and swaps it in on success. the last
// good map stays on any failure.
func unoRefreshPrices() {
	req, err := http.NewRequest("GET", unoPricingURL, nil)
	if err != nil {
		acclog.Printf("!! uno pricing: bad request: %v", err)
		return
	}
	keyedSetUA(req)
	if k := unoPool.pick(nil, false); k != nil {
		req.Header.Set("Authorization", "Bearer "+k.Key)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		acclog.Printf("!! uno pricing: fetch failed: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		acclog.Printf("!! uno pricing: upstream %d", resp.StatusCode)
		return
	}
	var payload struct {
		Models []unoPriceRow `json:"models"`
	}
	// cap the body: an upstream blob must not feed unbounded allocation.
	if json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&payload) != nil {
		acclog.Printf("!! uno pricing: decode failed")
		return
	}
	out := make(map[string]unoPriceRow, len(payload.Models))
	for _, m := range payload.Models {
		if strings.TrimSpace(m.Name) != "" {
			out[m.Name] = m
		}
	}
	if len(out) == 0 {
		return
	}
	unoPrice.mu.Lock()
	unoPrice.rows, unoPrice.at = out, time.Now()
	unoPrice.mu.Unlock()
}

// unoPriceFor is one row's usd price per million input/output tokens.
func unoPriceFor(id string) (in, out float64, ok bool) {
	if r, hit := unoPriceRows()[id]; hit {
		return r.Input, r.Output, true
	}
	return 0, 0, false
}

// unoFacts is uno's own metadata row for one id, from the pricing catalog: it
// carries what /models never does (reasoning, tools, modalities), so it wins
// over model_info.jsonc for uno rows. nil when uno lists no row for the id.
func unoFacts(id string) *modelInfoEntry {
	r, hit := unoPriceRows()[id]
	if !hit {
		return nil
	}
	e := &modelInfoEntry{}
	// one modality per value: tags overlap (vision + files both say image).
	addMod := func(dst *[]string, v string) {
		for _, x := range *dst {
			if x == v {
				return
			}
		}
		*dst = append(*dst, v)
	}
	for _, t := range r.Tags {
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "tools":
			e.SupportedParams = append(e.SupportedParams, "tools", "tool_choice")
		case "reasoning":
			tr := true
			e.Reasoning = &tr
		case "vision", "files":
			addMod(&e.Inputs, "image")
		case "video":
			addMod(&e.Inputs, "video")
		case "text":
			addMod(&e.OutMods, "text")
		}
	}
	return e
}

// unoBackend is the wired uno keybackend.
var unoBackend = &keybackend{
	name:     "uno",
	base:     unoBase,
	pool:     unoPool,
	enabled:  unoEnabled,
	models:   unoModels,
	allows:   unoAllowed,
	paidFor:  func(id string) bool { return !unoFreeID(id) },
	priceFor: unoPriceFor,
	facts:    unoFacts,
}
