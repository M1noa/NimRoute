package main

// nous: the Nous Portal inference API. openai-compatible, bearer api key,
// base https://inference-api.nousresearch.com/v1. only the zero-priced
// models are served and listed: the paid catalog is never proxied.
//
// auth is api-key only. the portal also speaks x402 (solana usdc, pay per
// request, no api key), which is not implemented here.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// nousBase is the portal's openai-compatible root. var, not const, so tests
// can point it at a local server.
var nousBase = "https://inference-api.nousresearch.com/v1"

// nousCatalogTTL is how long the free-model list is cached. GET /v1/models
// is unauthenticated, so a stale entry costs one cheap refetch, not an outage.
const nousCatalogTTL = 6 * time.Hour

// nousKey is one portal credential. the pool is separate from the nim keys:
// different accounts on a different host, and one host's cooldown must never
// park the other's traffic.
type nousKey struct {
	Name        string
	Key         string
	CooldownTil time.Time
	Consec429   int
}

var (
	nousKeyMu sync.RWMutex
	nousKeys  []*nousKey
)

// nousSyncKeys rebuilds the portal key pool from config, on load and on every
// hot reload. cooldowns reset with it: a reload is an operator action, not a
// reason to keep punishing a key.
func nousSyncKeys(m map[string]string) {
	out := make([]*nousKey, 0, len(m))
	for name, k := range m {
		if strings.TrimSpace(k) == "" {
			continue
		}
		out = append(out, &nousKey{Name: name, Key: k})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	nousKeyMu.Lock()
	nousKeys = out
	nousKeyMu.Unlock()
}

// nousEnabled reports whether portal requests are served: flag on, and the
// key pool actually holds something. it reads the pool, not cfg().NousKeys,
// so the two can never disagree about whether there is a usable key.
func nousEnabled() bool {
	if !cfg().Nous.Enabled {
		return false
	}
	nousKeyMu.RLock()
	defer nousKeyMu.RUnlock()
	return len(nousKeys) > 0
}

// nousPickKey returns one key off cooldown, nil when all are cooling or
// excluded. math/rand is sampling only; no secret or id is drawn from it.
func nousPickKey(exclude map[string]bool) *nousKey {
	nousKeyMu.RLock()
	defer nousKeyMu.RUnlock()
	now := time.Now()
	live := make([]*nousKey, 0, len(nousKeys))
	for _, k := range nousKeys {
		if exclude[k.Name] || now.Before(k.CooldownTil) {
			continue
		}
		live = append(live, k)
	}
	if len(live) == 0 {
		return nil
	}
	return live[rand.Intn(len(live))]
}

// nousCoolKey backs a key off after a 429. the portal free tier is 50 rpm /
// 500k tpm per key, so even a single-key pool needs this. the log happens
// after the unlock: a file write under the pool mutex would serialize every
// request that happens to pick a cooling key.
func nousCoolKey(name string) {
	var cd time.Duration
	nousKeyMu.Lock()
	for _, k := range nousKeys {
		if k.Name != name {
			continue
		}
		k.Consec429++
		shift := k.Consec429 - 1
		if shift > 8 {
			shift = 8
		}
		cd = cooldown429 << shift
		if cd > maxBackoff {
			cd = maxBackoff
		}
		k.CooldownTil = time.Now().Add(cd)
		break
	}
	nousKeyMu.Unlock()
	if cd > 0 {
		acclog.Printf("  nous key-backoff [%s] %v", name, cd)
	}
}

// nousClearKey drops the 429 streak after a 200.
func nousClearKey(name string) {
	nousKeyMu.Lock()
	defer nousKeyMu.Unlock()
	for _, k := range nousKeys {
		if k.Name == name {
			k.Consec429 = 0
			k.CooldownTil = time.Time{}
		}
	}
}

// nousModel is one free model row from GET /v1/models.
type nousModel struct {
	ID      string
	Context int
	MaxOut  int
}

var (
	nousCatMu sync.RWMutex
	nousCat   []nousModel
	nousCatAt time.Time
)

// nousPriceZero reports a per-token price string as zero. the portal sends
// these as strings ("0", "0.0000040000"); "" and junk both mean "not free".
func nousPriceZero(s string) bool {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return err == nil && f == 0
}

// nousSetUA tags every portal request. the portal's edge 403s Go's default
// "Go-http-client/1.1", which silently emptied the catalog and would have
// 403'd inference the same way.
func nousSetUA(req *http.Request) {
	req.Header.Set("User-Agent", "nimroute/"+versionStr)
	req.Header.Set("Accept", "application/json")
}

// nousCached returns the last good list without refetching. an unreachable
// portal must not empty the listing.
func nousCached() []nousModel {
	nousCatMu.RLock()
	defer nousCatMu.RUnlock()
	return nousCat
}

// nousFreeModels returns the zero-priced catalog rows, cached.
//
// the ":free" id suffix is NOT the filter. several free ids carry no suffix
// (inclusionai/ling-3.1-flash, meituan/longcat-2.0, stealth/missingno) and
// no paid id carries one, so pricing is the only reliable signal. both
// directions must be zero: the embedding models price prompt > 0 with
// completion exactly 0, and those are not free.
func nousFreeModels() []nousModel {
	nousCatMu.RLock()
	cached, at := nousCat, nousCatAt
	nousCatMu.RUnlock()
	if len(cached) > 0 && time.Since(at) < nousCatalogTTL {
		return cached
	}
	// stale or cold. /status polls this once a second (status-tail), so the
	// fetch goes to the background and the caller gets the cached copy: a
	// 10s upstream GET must never sit on the status path. the listing
	// catches up when the fetch lands.
	tryRefreshNousCatalog()
	return cached
}

// nousCatalogBackoff gates background refetches. a 30s floor keeps a
// failing portal from refetching on every poll; a success re-arms the
// 6h ttl through nousCatAt instead.
var nousCatalogBackoff struct {
	sync.Mutex
	next time.Time
}

func tryRefreshNousCatalog() {
	nousCatalogBackoff.Lock()
	defer nousCatalogBackoff.Unlock()
	if time.Now().Before(nousCatalogBackoff.next) {
		return
	}
	nousCatalogBackoff.next = time.Now().Add(30 * time.Second)
	go fetchNousCatalog()
}

// fetchNousCatalog pulls the portal listing and swaps it in on success,
// keeping the previous list on any failure.
func fetchNousCatalog() []nousModel {
	req, err := http.NewRequest("GET", nousBase+"/models", nil)
	if err != nil {
		acclog.Printf("!! nous catalog: bad request: %v", err)
		return nousCached()
	}
	nousSetUA(req)
	cl := &http.Client{Timeout: 10 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		acclog.Printf("!! nous catalog: fetch failed: %v", err)
		return nousCached()
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// the portal's edge 403s the default Go user-agent, so a 403 here
		// usually means the UA regressed, not that the list is gone.
		acclog.Printf("!! nous catalog: upstream %d", resp.StatusCode)
		return nousCached()
	}
	var payload struct {
		Data []struct {
			ID      string `json:"id"`
			Context int    `json:"context_length"`
			// only the two prices that decide free. pricing also carries
			// per-modality strings, an "overrides" ARRAY and an "original"
			// object, so a map[string]string would fail on the whole body.
			Pricing struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
			Top struct {
				MaxOut int `json:"max_completion_tokens"`
			} `json:"top_provider"`
		} `json:"data"`
	}
	// cap the body: a compromised or bloated portal must not feed
	// unbounded allocation into a /v1/models call.
	if json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&payload) != nil {
		acclog.Printf("!! nous catalog: decode failed")
		return nousCached()
	}
	out := make([]nousModel, 0, len(payload.Data))
	for _, m := range payload.Data {
		if m.ID == "" || !nousPriceZero(m.Pricing.Prompt) ||
			!nousPriceZero(m.Pricing.Completion) {
			continue
		}
		out = append(out, nousModel{ID: m.ID, Context: m.Context, MaxOut: m.Top.MaxOut})
	}
	if len(out) == 0 {
		return nousCached()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	nousCatMu.Lock()
	nousCat, nousCatAt = out, time.Now()
	nousCatMu.Unlock()
	return out
}

// nousResult is one settled portal response.
type nousResult struct {
	Status      int
	Body        []byte
	Header      http.Header
	KeyName     string
	RateLimited bool
	Retries     int
}

// nousSend calls the portal, rotating off a key that 429s. every attempt is
// bounded: when all keys are cooling the last real 429 goes back to the
// client rather than a synthesized 503, so opencode retries on the portal's
// own schedule.
func nousSend(ctx context.Context, method, target string, body []byte,
	hdr http.Header, model string) *nousResult {
	exclude := map[string]bool{}
	attempts := len(cfg().NousKeys) + 1
	if attempts < 2 {
		attempts = 2
	}
	if attempts > 6 {
		attempts = 6
	}
	cl := dbgClient(&http.Client{Timeout: 300 * time.Second}, "nous")
	var last429 *nousResult
	for attempt := 0; attempt < attempts; attempt++ {
		k := nousPickKey(exclude)
		if k == nil {
			break
		}
		req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
		if err != nil {
			return nil
		}
		for h, vs := range hdr {
			for _, v := range vs {
				req.Header.Add(h, v)
			}
		}
		// set after the copy loop: a client-supplied Authorization must
		// never reach the portal, and the proxy credential always wins.
		req.Header.Set("Authorization", "Bearer "+k.Key)
		nousSetUA(req)
		resp, err := cl.Do(req)
		if err != nil {
			acclog.Printf("!! 502 nous upstream-error [%s] model=%s: %v", k.Name, model, err)
			return nil
		}
		rb, rerr := io.ReadAll(io.LimitReader(resp.Body, upstreamBodyLimit))
		resp.Body.Close()
		if rerr != nil {
			// a truncated body is not a settled response: report a transport
			// failure instead of handing the client half an answer.
			acclog.Printf("!! 502 nous body-read [%s] model=%s: %v", k.Name, model, rerr)
			return nil
		}

		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == 529 {
			nousCoolKey(k.Name)
			exclude[k.Name] = true
			last429 = &nousResult{
				Status:      resp.StatusCode,
				Body:        rb,
				Header:      resp.Header.Clone(),
				KeyName:     k.Name,
				RateLimited: true,
				Retries:     attempt,
			}
			acclog.Printf("!! %d [nous:%s] model=%s", resp.StatusCode, k.Name, model)
			continue
		}
		if resp.StatusCode == http.StatusOK {
			nousClearKey(k.Name)
		}
		return &nousResult{
			Status:  resp.StatusCode,
			Body:    rb,
			Header:  resp.Header.Clone(),
			KeyName: k.Name,
			Retries: attempt,
		}
	}
	if last429 != nil {
		return last429
	}
	// every key was already cooling before the first attempt, so the portal's
	// own 429 never arrived. report the pool's rate limit (with the wait)
	// instead of letting the caller synthesize a 502.
	if d := nousSoonestRetry(); d > 0 {
		secs := int(d/time.Second) + 1
		h := http.Header{}
		h.Set("Retry-After", strconv.Itoa(secs))
		h.Set("Content-Type", "application/json")
		return &nousResult{
			Status:      http.StatusTooManyRequests,
			Body:        []byte(`{"error":{"type":"rate_limit_error","message":"nous: all keys rate limited"}}`),
			Header:      h,
			RateLimited: true,
		}
	}
	return nil
}

// nousSoonestRetry is how long until the first cooling key is usable again.
func nousSoonestRetry() time.Duration {
	nousKeyMu.RLock()
	defer nousKeyMu.RUnlock()
	now := time.Now()
	var best time.Duration
	for _, k := range nousKeys {
		d := k.CooldownTil.Sub(now)
		if d <= 0 {
			continue
		}
		if best == 0 || d < best {
			best = d
		}
	}
	return best
}

// nousFwdHeaders copies the client request, dropping the credentials and
// internal headers. same deny-list as the nim path: authorization is the
// proxy's own credential here, x-session-id links the client upstream.
func nousFwdHeaders(r *http.Request) http.Header {
	fwd := http.Header{}
	for k, v := range r.Header {
		lk := strings.ToLower(k)
		if lk == "authorization" || lk == "host" || lk == "origin" ||
			lk == "cookie" || lk == "x-api-key" || lk == "x-session-id" {
			continue
		}
		fwd[k] = v
	}
	if fwd.Get("Content-Type") == "" {
		fwd.Set("Content-Type", "application/json")
	}
	return fwd
}

// handleNous serves the openai-shaped /v1/chat/completions path against the
// portal. split from ServeHTTP so the route dispatcher can try it as one
// candidate among backends.
func (p *Pool) handleNous(w http.ResponseWriter, r *http.Request, body []byte,
	model string, isStream bool, start time.Time, reqID string) {
	target := nousBase + strings.TrimPrefix(r.URL.Path, "/v1")
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	acclog.Printf("%s routed to nous target=%s model=%s", reqID, target, model)

	res := nousSend(r.Context(), r.Method, target, body, nousFwdHeaders(r), model)
	elapsed := time.Since(start)
	if res == nil {
		acclog.Printf("!! nous 502 model=%s", model)
		logUsage(UsageRecord{
			Ts:           time.Now().UTC().Format(time.RFC3339Nano),
			Model:        model,
			Method:       r.Method,
			Path:         r.URL.Path,
			StatusCode:   http.StatusBadGateway,
			DurationMs:   elapsed.Milliseconds(),
			Stream:       isStream,
			Error:        "nous: upstream transport error",
			ContentBytes: int64(len(body)),
		})
		http.Error(w, `{"error":"nous: upstream unavailable"}`, http.StatusBadGateway)
		return
	}

	var prompT, compT, totalT int
	var recErr string
	if res.Status != http.StatusOK {
		recErr = extractErrMessage(string(res.Body))
		if len(recErr) > 200 {
			recErr = recErr[:200]
		}
		acclog.Printf("!! nous upstream %d [%s] model=%s err=%q", res.Status, res.KeyName, model, recErr)
	} else if !isStream {
		prompT, compT, totalT = respTokens(res.Body)
	}

	logUsage(UsageRecord{
		Ts:               time.Now().UTC().Format(time.RFC3339Nano),
		Model:            model,
		KeyName:          "nous:" + res.KeyName,
		Method:           r.Method,
		Path:             r.URL.Path,
		StatusCode:       res.Status,
		DurationMs:       elapsed.Milliseconds(),
		PromptTokens:     prompT,
		CompletionTokens: compT,
		TotalTokens:      totalT,
		Stream:           isStream,
		RetryAttempt:     res.Retries,
		RateLimited:      res.RateLimited,
		Error:            recErr,
		ContentBytes:     int64(len(body)),
		Headers:          rlHeaders(res.Header),
	})

	// allowlisted response headers only: upstream Content-Length / Encoding /
	// Cookies are wrong after body transforms.
	for k, v := range res.Header {
		lk := strings.ToLower(k)
		if lk == "content-type" || strings.HasPrefix(lk, "x-ratelimit") ||
			lk == "retry-after" {
			w.Header()[k] = v
		}
	}
	w.WriteHeader(res.Status)
	w.Write(res.Body)
}

// handleNousAnthropic serves /v1/messages via the portal. oaiBody is already
// anthropic->openai converted by the caller; split out so the route dispatcher
// can treat nous as one candidate among backends.
func (p *Pool) handleNousAnthropic(w http.ResponseWriter, r *http.Request,
	oaiBody []byte, clientModel, upstreamModel string, tnames toolNames,
	isStream bool, start time.Time, reqID string) {
	target := nousBase + "/chat/completions"
	acclog.Printf("%s routed to nous target=%s model=%s", reqID, target, upstreamModel)

	fwd := http.Header{
		"Content-Type": {"application/json"},
		"Accept":       {"application/json"},
	}
	if isStream {
		fwd.Set("Accept", "text/event-stream")
	}
	res := nousSend(r.Context(), r.Method, target, oaiBody, fwd, upstreamModel)
	elapsed := time.Since(start)

	if res == nil {
		errMsg := "nous: upstream unavailable"
		logUsage(UsageRecord{
			Ts:         time.Now().UTC().Format(time.RFC3339Nano),
			Model:      clientModel,
			Method:     "POST",
			Path:       "/v1/messages",
			StatusCode: http.StatusBadGateway,
			DurationMs: elapsed.Milliseconds(),
			Stream:     isStream,
			Error:      errMsg,
		})
		writeAnthropicError(w, http.StatusBadGateway, "api_error", errMsg)
		return
	}

	if res.Status != http.StatusOK {
		errMsg := extractErrMessage(string(res.Body))
		acclog.Printf("!! nous upstream %d [%s] model=%s err=%q", res.Status, res.KeyName, clientModel, errMsg)
		logUsage(UsageRecord{
			Ts:           time.Now().UTC().Format(time.RFC3339Nano),
			Model:        clientModel,
			KeyName:      "nous:" + res.KeyName,
			Method:       "POST",
			Path:         "/v1/messages",
			StatusCode:   res.Status,
			DurationMs:   elapsed.Milliseconds(),
			Stream:       isStream,
			Error:        errMsg,
			RateLimited:  res.RateLimited,
			RetryAttempt: res.Retries,
		})
		writeAnthropicError(w, res.Status, "api_error", errMsg)
		return
	}

	if isStream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		written, promptT, compT := streamAnthropic(w, res.Body, clientModel, tnames)
		logUsage(UsageRecord{
			Ts:               time.Now().UTC().Format(time.RFC3339Nano),
			Model:            clientModel,
			KeyName:          "nous:" + res.KeyName,
			Method:           "POST",
			Path:             "/v1/messages",
			StatusCode:       200,
			DurationMs:       time.Since(start).Milliseconds(),
			PromptTokens:     promptT,
			CompletionTokens: compT,
			TotalTokens:      promptT + compT,
			Stream:           true,
			ContentBytes:     written,
		})
		return
	}

	out, errMsg, _ := openAIToAnthropic(res.Body, clientModel, tnames)
	if errMsg != "" {
		logUsage(UsageRecord{
			Ts:         time.Now().UTC().Format(time.RFC3339Nano),
			Model:      clientModel,
			KeyName:    "nous:" + res.KeyName,
			Method:     "POST",
			Path:       "/v1/messages",
			StatusCode: http.StatusInternalServerError,
			DurationMs: time.Since(start).Milliseconds(),
			Error:      errMsg,
		})
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", errMsg)
		return
	}
	prompT, compT, _ := respTokens(res.Body)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(out)

	logUsage(UsageRecord{
		Ts:               time.Now().UTC().Format(time.RFC3339Nano),
		Model:            clientModel,
		KeyName:          "nous:" + res.KeyName,
		Method:           "POST",
		Path:             "/v1/messages",
		StatusCode:       200,
		DurationMs:       time.Since(start).Milliseconds(),
		PromptTokens:     prompT,
		CompletionTokens: compT,
		TotalTokens:      prompT + compT,
		ContentBytes:     int64(len(out)),
	})
}

// appendNousModels lists nous/<id> for every free model the portal currently
// serves. paid ids never reach the listing: the filter is pricing, not the
// ":free" suffix (see nousFreeModels).
func appendNousModels(out *struct {
	Data []openRouterModel `json:"data"`
}, seenIDs map[string]bool) {
	if !nousEnabled() {
		return
	}
	for _, m := range nousFreeModels() {
		id := "nous/" + m.ID
		if seenIDs[id] {
			continue
		}
		seenIDs[id] = true
		entry := infoModel(id, id)
		// the portal publishes the served window and output cap; prefer them
		// over the catalog fallback so the client is told the real numbers.
		if m.Context > 0 {
			entry.ContextLength = m.Context
		}
		if m.MaxOut > 0 {
			entry.TopProvider = map[string]any{"max_completion_tokens": m.MaxOut}
		}
		out.Data = append(out.Data, entry)
	}
}

// nousKeyStatus is one portal key row for /status. only the label ships: the
// secret itself is never in a status payload.
type nousKeyStatus struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Cooldown  string `json:"cooldown,omitempty"`
}

// nousSummary is the /status view of the portal backend.
type nousSummary struct {
	Enabled bool            `json:"enabled"`
	Keys    []nousKeyStatus `json:"keys,omitempty"`
	Models  []string        `json:"models,omitempty"`
}

// nousSnapshot builds the /status portal section: per-key availability plus
// the free-model ids currently served.
func nousSnapshot() *nousSummary {
	sum := &nousSummary{Enabled: true}
	nousKeyMu.RLock()
	now := time.Now()
	for _, k := range nousKeys {
		ks := nousKeyStatus{Name: k.Name, Available: now.After(k.CooldownTil)}
		if !ks.Available {
			ks.Cooldown = k.CooldownTil.Sub(now).Round(time.Second).String()
		}
		sum.Keys = append(sum.Keys, ks)
	}
	nousKeyMu.RUnlock()
	for _, m := range nousFreeModels() {
		sum.Models = append(sum.Models, m.ID)
	}
	return sum
}
