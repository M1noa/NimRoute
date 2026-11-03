package main

// orchat.go is the openrouter chat backend, built on the shared keyedchat
// engine. no second catalog fetch: openrouter.go already runs the /v1/models
// index (openRouterIndex); the chat side only serves rows the ids of which
// are listed as openrouter/<id>. one leading openrouter/ is stripped before
// the upstream call, deeper vendor slashes stay verbatim.
//
// ":free" ids are the free tier here, so the default serving set is the index
// rows carrying that suffix. openrouter.paid_models serves the whole chat
// catalog, openrouter.model_whitelist adds exact ids the index omits.

import (
	"sort"
	"strings"
)

// orChatBase is the openrouter openai root.
const orChatBase = "https://openrouter.ai/api/v1"

// orChatPool is the openrouter chat key pool, fed from openrouter_keys.
var orChatPool = &keyedPool{name: "openrouter"}

// orChatSyncKeys replaces the openrouter chat key pool on config load.
func orChatSyncKeys(m map[string]string, paid []string) { orChatPool.syncKeys(m, paid) }

// orChatEnabled gates routing: the config flag lets orchat serve with zero
// keys despite openrouter's models listing staying keyless.
func orChatEnabled() bool {
	return cfg().OpenRouter.Enabled && orChatPool.size() > 0
}

// orChatPolicies is the config state one call needs.
func orChatPolicies() (bool, map[string]bool) {
	c := cfg().OpenRouter
	return c.PaidModels, keyedWhitelist("openrouter/", c.Whitelist)
}

// orChatRow reports whether an index row can answer /chat/completions: text
// output only, so the image and audio generators stay out of the paid
// listing. free rows are always chat rows and skip the check.
func orChatRow(m *orModel) bool {
	if len(m.Architecture.OutputModalities) == 0 {
		return true
	}
	for _, o := range m.Architecture.OutputModalities {
		if o != "text" {
			return false
		}
	}
	return true
}

// orChatAllowed reports whether one id may be forwarded under the policy,
// price cap included: a hidden model must not still answer a hand-written id.
func orChatAllowed(id string) bool {
	paid, wl := orChatPolicies()
	if !keyedWanted(id, strings.HasSuffix(id, ":free"), paid, wl) {
		return false
	}
	return keyedUnderPriceCap(orChatPrice, cfg().OpenRouter.MaxPrice1M, id)
}

// orChatModels derives the serving set from the live openrouter index.
func orChatModels() []keyedModel {
	paid, wl := orChatPolicies()
	out := make([]keyedModel, 0, len(wl)+8)
	seen := make(map[string]bool, len(wl)+8)
	if ix := openRouterIndex(); ix != nil {
		for _, m := range ix.byID {
			free := strings.HasSuffix(m.ID, ":free")
			if !keyedWanted(m.ID, free, paid, wl) || !orChatRow(m) {
				continue
			}
			if seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			km := keyedModel{ID: m.ID, Context: m.ContextLength}
			if m.TopProvider.MaxCompletionTokens != nil && *m.TopProvider.MaxCompletionTokens > 0 {
				km.MaxOut = *m.TopProvider.MaxCompletionTokens
			}
			out = append(out, km)
		}
	}
	// a whitelisted id the index does not know still lists and routes.
	for id := range wl {
		if !seen[id] {
			seen[id] = true
			out = append(out, keyedModel{ID: id})
		}
	}
	// a price cap trims expensive paid rows; free rows quote 0 and pass.
	if cap := cfg().OpenRouter.MaxPrice1M; cap > 0 {
		kept := out[:0]
		for _, m := range out {
			if keyedUnderPriceCap(orChatPrice, cap, m.ID) {
				kept = append(kept, m)
			}
		}
		out = kept
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// orFacts is openrouter's own row for one id: name, description, window,
// modalities, supported parameters and reasoning come from the provider, so
// they win over model_info.jsonc for openrouter/<id> rows. nil when the index
// carries no row.
func orFacts(id string) *modelInfoEntry {
	ix := openRouterIndex()
	if ix == nil {
		return nil
	}
	m := ix.get("openrouter/" + id)
	if m == nil {
		return nil
	}
	e := orEntry(m)
	return &e
}

// orChatPrice is one row's openrouter price per million tokens.
func orChatPrice(id string) (in, out float64, ok bool) {
	return orPrice1M("openrouter/" + id)
}

// orChatBackend is the wired openrouter keybackend.
var orChatBackend = &keybackend{
	name:     "openrouter",
	base:     orChatBase,
	pool:     orChatPool,
	enabled:  orChatEnabled,
	models:   orChatModels,
	allows:   orChatAllowed,
	paidFor:  func(id string) bool { return !strings.HasSuffix(id, ":free") },
	priceFor: orChatPrice,
	facts:    orFacts,
}
