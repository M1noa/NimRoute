package main

// uno.go is the unorouter backend (api.unorouter.com), built on the shared
// keyedchat engine. the catalog GET /models is unauthenticated but the edge
// mimics openrouter's, so the nimroute UA goes on it too. only models whose
// id carries the :free suffix and whose supported_endpoint_types include
// openai are served; embedding/image-generation/aihorde rows are excluded.

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// unoBase is the openai-shaped root plus the /models prefix used by handleChat.
var unoBase = "https://api.unorouter.com/v1"

// unoCatalogTTL matches nous: the :free filter is slender, so a stale list
// beats hammering the endpoint.
const unoCatalogTTL = 6 * time.Hour

var (
	unoCatMu sync.Mutex
	unoCat   []keyedModel
	unoCatAt time.Time
)

// unoCatalogBackoff gates background refetches. a 30s floor keeps a broken
// upstream from turning every /v1/models call into a dial.
var unoCatalogBackoff struct {
	sync.Mutex
	next time.Time
}

// unoPool is the uno key pool, fed from uno_keys in config.yml.
var unoPool = &keyedPool{name: "uno"}

// unoSyncKeys replaces the uno key pool on config load.
func unoSyncKeys(m map[string]string) { unoPool.syncKeys(m) }

// unoEnabled gates routing and listing: the config flag plus at least one
// configured key.
func unoEnabled() bool { return cfg().Uno.Enabled && unoPool.size() > 0 }

// unoModels lists the catalog through the :free + openai-chat filter.
func unoModels() []keyedModel {
	unoCatMu.Lock()
	cached, at := unoCat, unoCatAt
	unoCatMu.Unlock()
	if len(cached) > 0 && time.Since(at) < unoCatalogTTL {
		return cached
	}
	return fetchUnoCatalog()
}

// unoChatFree fits the row filter: :free id suffix and openai among the
// supported endpoints, minus the non-chat rows (:free embeddings/images
// carry the suffix but can't be served by /chat/completions).
func unoChatFree(row struct {
	ID      string   `json:"id"`
	Types   []string `json:"supported_endpoint_types"`
	Context int      `json:"context_length"`
	MaxOut  int      `json:"max_output_tokens"`
}) bool {
	if !strings.HasSuffix(row.ID, ":free") {
		return false
	}
	var openai bool
	for _, t := range row.Types {
		switch t {
		case "openai":
			openai = true
		case "embedding", "image-generation", "aihorde":
			return false
		}
	}
	return openai
}

// fetchUnoCatalog refreshes the uno catalog, falling back to the last good
// list on any failure (the pattern nous.go runs).
func fetchUnoCatalog() []keyedModel {
	unoCatalogBackoff.Lock()
	if time.Now().Before(unoCatalogBackoff.next) {
		unoCatalogBackoff.Unlock()
		return unoCached()
	}
	unoCatalogBackoff.next = time.Now().Add(30 * time.Second)
	unoCatalogBackoff.Unlock()

	req, err := http.NewRequest("GET", unoBase+"/models", nil)
	if err != nil {
		acclog.Printf("!! uno catalog: bad request: %v", err)
		return unoCached()
	}
	nousSetUA(req)
	req.Header.Set("Accept", "application/json")
	// the catalog is key-gated (anon answers 401); any pool key opens it.
	if k := unoPool.pick(nil); k != nil {
		req.Header.Set("Authorization", "Bearer "+k.Key)
	}
	cl := &http.Client{Timeout: 10 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		acclog.Printf("!! uno catalog: fetch failed: %v", err)
		return unoCached()
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		acclog.Printf("!! uno catalog: upstream %d", resp.StatusCode)
		return unoCached()
	}
	var payload struct {
		Data []struct {
			ID      string   `json:"id"`
			Types   []string `json:"supported_endpoint_types"`
			Context int      `json:"context_length"`
			MaxOut  int      `json:"max_output_tokens"`
		} `json:"data"`
	}
	// cap the body: an upstream blob must not feed unbounded allocation.
	if json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&payload) != nil {
		acclog.Printf("!! uno catalog: decode failed")
		return unoCached()
	}
	out := make([]keyedModel, 0, len(payload.Data))
	for _, m := range payload.Data {
		if !unoChatFree(m) {
			continue
		}
		out = append(out, keyedModel{ID: m.ID, Context: m.Context, MaxOut: m.MaxOut})
	}
	if len(out) == 0 {
		return unoCached()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	unoCatMu.Lock()
	unoCat, unoCatAt = out, time.Now()
	unoCatMu.Unlock()
	return out
}

// unoCached hands back the last good list; nil on a first-fetch failure.
func unoCached() []keyedModel {
	unoCatMu.Lock()
	defer unoCatMu.Unlock()
	return unoCat
}

// unoBackend is the wired uno keybackend: the chat handler and listing go
// through keyedchat with this descriptor.
var unoBackend = &keybackend{
	name:    "uno",
	base:    unoBase,
	pool:    unoPool,
	enabled: unoEnabled,
	models:  unoModels,
}
