package main

// orcarouter.go is the orcarouter backend (api.orcarouter.ai), an openai-
// compatible gateway with provider-prefixed ids (deepseek/deepseek-v4-
// flash-free). free rows carry the -free suffix and price at zero; they run
// under their own per-workspace limits and never fall back to a paid model.
// orcarouter.paid_models serves the paid catalog, .model_whitelist adds exact
// ids the listing omits.
//
// the catalog answers anonymous callers but authenticates strictly once a key
// rides along, so a pool key is sent only when one exists.

import (
	"strings"
	"time"
)

// orcaBase is the openai-shaped root plus the /models prefix.
var orcaBase = "https://api.orcarouter.ai/v1"

// orcaPool is the orcarouter key pool, fed from orcarouter_keys.
var orcaPool = &keyedPool{name: "orcarouter"}

// orcaCatalog caches the listing for 6h; the free lineup rotates as capacity
// allows, so a stale list is worse here than for a stable catalog, but a
// refetch per status poll is worse still.
var orcaCatalog = &keyedCatalog{
	name: "orcarouter",
	url:  orcaBase + "/models",
	pool: orcaPool,
	ttl:  6 * time.Hour,
}

// orcaSyncKeys replaces the key pool on config load.
func orcaSyncKeys(m map[string]string, paid []string) { orcaPool.syncKeys(m, paid) }

// orcaEnabled gates routing and listing: flag on plus one key.
func orcaEnabled() bool { return cfg().Orca.Enabled && orcaPool.size() > 0 }

// orcaFreeID reports a free-tier row: the -free suffix is the whole marker.
func orcaFreeID(id string) bool { return strings.HasSuffix(id, "-free") }

// orcaPolicies is the config state one call needs.
func orcaPolicies() (bool, map[string]bool) {
	c := cfg().Orca
	return c.PaidModels, keyedWhitelist("orcarouter/", c.Whitelist)
}

// orcaAllowed reports whether one id may be forwarded under the policy.
func orcaAllowed(id string) bool {
	paid, wl := orcaPolicies()
	return keyedWanted(id, orcaFreeID(id), paid, wl)
}

// orcaModels lists the catalog through the free/openai-chat filter.
func orcaModels() []keyedModel {
	paid, wl := orcaPolicies()
	return keyedServeRows(orcaCatalog.models(), orcaFreeID, paid, wl)
}

// orcaBackend is the wired orcarouter keybackend.
var orcaBackend = &keybackend{
	name:    "orcarouter",
	base:    orcaBase,
	pool:    orcaPool,
	enabled: orcaEnabled,
	models:  orcaModels,
	allows:  orcaAllowed,
	paidFor: func(id string) bool { return !orcaFreeID(id) },
}
