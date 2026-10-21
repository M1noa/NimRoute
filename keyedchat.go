package main

// keyedchat: the shared engine for the multi-key openai-compatible
// backends (uno, openrouter chat). one key pool per backend; a key that
// 429s rotates off instantly and cools on an exponential backoff, with the
// same rules the nous portal key pool runs (nous.go). each backend is thin
// (uno.go, orchat.go): a base url, a key pool, and a model list. nous keeps
// its own code and stays untouched.

import (
	"bytes"
	"context"
	"io"
	"math/rand"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// keyedKey is one upstream api key and its rotation state.
type keyedKey struct {
	Name        string
	Key         string
	CooldownTil time.Time
	Consec429   int
}

// keyedPool is one backend's key pool, shared by every request it serves.
type keyedPool struct {
	name string
	mu   sync.RWMutex
	keys []*keyedKey
}

func (p *keyedPool) size() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.keys)
}

// syncKeys swaps the configured keys in; an empty map empties the pool and
// reloads reset cooldown state.
func (p *keyedPool) syncKeys(m map[string]string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keys = nil
	for name, key := range m {
		if strings.TrimSpace(key) == "" {
			continue
		}
		p.keys = append(p.keys, &keyedKey{Name: name, Key: key})
	}
	sort.Slice(p.keys, func(i, j int) bool { return p.keys[i].Name < p.keys[j].Name })
}

// pick returns one usable key, skipping cooldowns and keys the ladder
// already 429'd on.
func (p *keyedPool) pick(exclude map[string]bool) *keyedKey {
	p.mu.RLock()
	defer p.mu.RUnlock()
	live := p.keys[:0:0]
	now := time.Now()
	for _, k := range p.keys {
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

// cool backs one key off for cooldown429 doubled per consecutive 429.
func (p *keyedPool) cool(name string) {
	var cd time.Duration
	p.mu.Lock()
	for _, k := range p.keys {
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
	p.mu.Unlock()
	if cd > 0 {
		acclog.Printf("  %s key-backoff [%s] %v", p.name, name, cd)
	}
}

// clear drops a key's backoff after it came through.
func (p *keyedPool) clear(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, k := range p.keys {
		if k.Name != name {
			continue
		}
		k.Consec429 = 0
		k.CooldownTil = time.Time{}
		break
	}
}

// soonestRetry is how long until some key unlocks again; 0 when none is
// cooling every key is already usable.
func (p *keyedPool) soonestRetry() time.Duration {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	var best time.Duration
	for _, k := range p.keys {
		d := k.CooldownTil.Sub(now)
		if d <= 0 {
			return 0
		}
		if best == 0 || d < best {
			best = d
		}
	}
	return best
}

// keyedModel is one listed model row: served id plus window facts.
type keyedModel struct {
	ID      string
	Context int
	MaxOut  int
}

// keybackend is one keyed openai-compatible backend: base url, key pool,
// enable gate, model list.
type keybackend struct {
	name    string
	base    string
	pool    *keyedPool
	enabled func() bool
	models  func() []keyedModel
}

type keyedResult struct {
	Status      int
	Body        []byte
	Header      http.Header
	KeyName     string
	RateLimited bool
	Retries     int
}

// keyedSetUA reuses nousSetUA: the nimroute UA is what stopped those relays'
// edges 403ing the plain Go default one.
func keyedSetUA(req *http.Request) { nousSetUA(req) }

// keyedFwdHeaders copies the client request, dropping credentials and
// internal headers. same deny-list as nous/nim: authorization is the proxy's
// own credential here, x-session-id links the client upstream.
func keyedFwdHeaders(r *http.Request) http.Header {
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

// keyedSend sends one openai-shaped request, rotating keys across 429/529.
// a spent pool answers the portal's own last 429 (or a synthesized one with
// a retry-after when every key was cooling before the first attempt),
// exactly like nousSend.
func keyedSend(b *keybackend, ctx context.Context, method, target string, body []byte, hdr http.Header, model string) *keyedResult {
	exclude := map[string]bool{}
	attempts := b.pool.size() + 1
	if attempts < 2 {
		attempts = 2
	}
	if attempts > 6 {
		attempts = 6
	}
	cl := dbgClient(&http.Client{Timeout: 300 * time.Second}, b.name)
	var last429 *keyedResult
	for attempt := 1; attempt <= attempts; attempt++ {
		key := b.pool.pick(exclude)
		if key == nil {
			break
		}
		req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
		if err != nil {
			continue
		}
		for k, v := range hdr {
			req.Header[k] = v
		}
		req.Header.Set("Authorization", "Bearer "+key.Key)
		keyedSetUA(req)
		resp, err := cl.Do(req)
		if err != nil {
			acclog.Printf("  !! 502 %s upstream-error [%s] model=%s: %v", b.name, key.Name, model, err)
			return nil
		}
		respBody, err := io.ReadAll(io.LimitReader(resp.Body, upstreamBodyLimit))
		resp.Body.Close()
		if err != nil {
			acclog.Printf("  !! 502 %s body-read [%s] model=%s: %v", b.name, key.Name, model, err)
			return nil
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == 529 {
			b.pool.cool(key.Name)
			last429 = &keyedResult{Status: resp.StatusCode, Body: respBody, Header: resp.Header, KeyName: key.Name, RateLimited: true, Retries: attempt}
			exclude[key.Name] = true
			acclog.Printf("  !! %d [%s:%s] model=%s", resp.StatusCode, b.name, key.Name, model)
			continue
		}
		if resp.StatusCode == http.StatusOK {
			b.pool.clear(key.Name)
		}
		return &keyedResult{Status: resp.StatusCode, Body: respBody, Header: resp.Header, KeyName: key.Name}
	}
	// every key was already cooling before the first attempt, so the
	// upstream's own 429 never arrived. report the pool's rate limit (with
	// the wait) instead of letting the caller synthesize a 502.
	if d := b.pool.soonestRetry(); d > 0 {
		secs := int(d/time.Second) + 1
		h := http.Header{}
		h.Set("Retry-After", strconv.Itoa(secs))
		h.Set("Content-Type", "application/json")
		return &keyedResult{
			Status:      http.StatusTooManyRequests,
			Body:        []byte(`{"error":{"type":"rate_limit_error","message":"` + b.name + `: all keys rate limited"}}`),
			Header:      h,
			RateLimited: true,
		}
	}
	if last429 != nil {
		return last429
	}
	return nil
}

// keyedChat serves the openai-shaped /v1/chat/completions path against one
// keyed backend.
func (p *Pool) keyedChat(w http.ResponseWriter, r *http.Request, body []byte,
	model string, isStream bool, start time.Time, reqID string, b *keybackend) {
	target := b.base + strings.TrimPrefix(r.URL.Path, "/v1")
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	acclog.Printf("%s routed to %s target=%s model=%s", reqID, b.name, target, model)

	res := keyedSend(b, r.Context(), r.Method, target, body, keyedFwdHeaders(r), model)
	elapsed := time.Since(start)
	if res == nil {
		acclog.Printf("  !! %s no-key 502 model=%s", b.name, model)
		logUsage(UsageRecord{
			Ts:           time.Now().UTC().Format(time.RFC3339Nano),
			Model:        model,
			Method:       r.Method,
			Path:         r.URL.Path,
			StatusCode:   http.StatusBadGateway,
			DurationMs:   elapsed.Milliseconds(),
			Stream:       isStream,
			Error:        b.name + ": upstream transport error",
			ContentBytes: int64(len(body)),
		})
		http.Error(w, `{"error":"`+b.name+`: upstream unavailable"}`, http.StatusBadGateway)
		return
	}

	var prompT, compT, totalT int
	var recErr string
	if res.Status != http.StatusOK {
		recErr = extractErrMessage(string(res.Body))
		if len(recErr) > 200 {
			recErr = recErr[:200]
		}
		acclog.Printf("  !! %s upstream %d [%s] model=%s err=%q", b.name, res.Status, res.KeyName, model, recErr)
	} else if !isStream {
		prompT, compT, totalT = respTokens(res.Body)
	}

	logUsage(UsageRecord{
		Ts:               time.Now().UTC().Format(time.RFC3339Nano),
		Model:            model,
		KeyName:          b.name + ":" + res.KeyName,
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

	// allowlisted response headers only: upstream content-length / encoding
	// / cookies are wrong after body transforms.
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

// keyedChatAnthropic serves /v1/messages for one keyed backend. oaiBody is
// already anthropic->openai converted by the caller.
func (p *Pool) keyedChatAnthropic(w http.ResponseWriter, r *http.Request,
	oaiBody []byte, clientModel, upstreamModel string, tnames toolNames,
	isStream bool, start time.Time, reqID string, b *keybackend) {
	target := b.base + "/chat/completions"
	acclog.Printf("%s routed to %s target=%s model=%s", reqID, b.name, target, upstreamModel)

	fwd := http.Header{
		"Content-Type": {"application/json"},
		"Accept":       {"application/json"},
	}
	if isStream {
		fwd.Set("Accept", "text/event-stream")
	}
	res := keyedSend(b, r.Context(), "POST", target, oaiBody, fwd, upstreamModel)
	elapsed := time.Since(start)

	if res == nil {
		errMsg := b.name + ": upstream unavailable"
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
		acclog.Printf("  !! %s upstream %d [%s] model=%s err=%q", b.name, res.Status, res.KeyName, clientModel, errMsg)
		logUsage(UsageRecord{
			Ts:           time.Now().UTC().Format(time.RFC3339Nano),
			Model:        clientModel,
			KeyName:      b.name + ":" + res.KeyName,
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
			KeyName:          b.name + ":" + res.KeyName,
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
			KeyName:    b.name + ":" + res.KeyName,
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
		KeyName:          b.name + ":" + res.KeyName,
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

// appendKeyedModels lists name/<id> for one backend's served row set.
func appendKeyedModels(b *keybackend, out *struct {
	Data []openRouterModel `json:"data"`
}, seenIDs map[string]bool) {
	if !b.enabled() {
		return
	}
	for _, m := range b.models() {
		id := b.name + "/" + m.ID
		if seenIDs[id] {
			continue
		}
		seenIDs[id] = true
		entry := infoModel(id, id)
		// upstream window and output cap prefer over the catalog fallback.
		if m.Context > 0 {
			entry.ContextLength = m.Context
		}
		if m.MaxOut > 0 {
			entry.TopProvider = map[string]any{"max_completion_tokens": m.MaxOut}
		}
		out.Data = append(out.Data, entry)
	}
}

// keyedKeyStatus is one key row for /status. only the label ships: the
// secret itself is never in a status payload.
type keyedKeyStatus struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Cooldown  string `json:"cooldown,omitempty"`
}

// keyedSummary is the /status view of one keyed backend.
type keyedSummary struct {
	Enabled bool             `json:"enabled"`
	Keys    []keyedKeyStatus `json:"keys,omitempty"`
	Models  []string         `json:"models,omitempty"`
}

// keyedSnapshot builds the /status section: per-key availability plus the
// served model ids.
func keyedSnapshot(b *keybackend) *keyedSummary {
	b.pool.mu.RLock()
	sum := &keyedSummary{Enabled: true}
	now := time.Now()
	for _, k := range b.pool.keys {
		ks := keyedKeyStatus{Name: k.Name, Available: now.After(k.CooldownTil)}
		if !ks.Available {
			ks.Cooldown = k.CooldownTil.Sub(now).Round(time.Second).String()
		}
		sum.Keys = append(sum.Keys, ks)
	}
	b.pool.mu.RUnlock()
	for _, m := range b.models() {
		sum.Models = append(sum.Models, m.ID)
	}
	sum.Models = foldVariantIDs(b.name+"/", sum.Models)
	return sum
}
