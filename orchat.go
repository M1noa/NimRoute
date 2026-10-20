package main

// orchat.go is the openrouter free-tier chat backend, built on the shared
// keyedchat engine. no second catalog fetch: openrouter.go already runs the
// /v1/models index (openRouterIndex); the chat side only serves rows the
// ids of which are listed as openrouter/<id>. one leading openrouter/ is
// stripped before the upstream call, deeper vendor slashes stay verbatim.
//
// unlike the uno catalog, :free ids are the free tier here, so the serving
// set is the index rows carrying a :free suffix.

import (
	"sort"
	"strings"
)

// orChatBase is the openrouter openai root.
const orChatBase = "https://openrouter.ai/api/v1"

// orChatPool is the openrouter chat key pool, fed from openrouter_keys.
var orChatPool = &keyedPool{name: "openrouter"}

// orChatSyncKeys replaces the openrouter chat key pool on config load.
func orChatSyncKeys(m map[string]string) { orChatPool.syncKeys(m) }

// orChatEnabled gates routing: the config flag lets orchat serve with zero
// keys despite openrouter's models listing staying keyless.
func orChatEnabled() bool {
	return cfg().OpenRouter.Enabled && orChatPool.size() > 0
}

// orChatModels derives the free serving set from the live openrouter index.
func orChatModels() []keyedModel {
	ix := openRouterIndex()
	if ix == nil {
		return nil
	}
	out := make([]keyedModel, 0, len(ix.byID))
	for _, m := range ix.byID {
		if !strings.HasSuffix(m.ID, ":free") {
			continue
		}
		km := keyedModel{ID: m.ID, Context: m.ContextLength}
		if m.TopProvider.MaxCompletionTokens != nil && *m.TopProvider.MaxCompletionTokens > 0 {
			km.MaxOut = *m.TopProvider.MaxCompletionTokens
		}
		out = append(out, km)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// orChatBackend is the wired openrouter keybackend.
var orChatBackend = &keybackend{
	name:    "openrouter",
	base:    orChatBase,
	pool:    orChatPool,
	enabled: orChatEnabled,
	models:  orChatModels,
}
