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

// keyedKey is one upstream api key and its rotation state.
type keyedKey struct {
	Name        string
	Key         string
	CooldownTil time.Time
	Consec429   int
	// Paid marks a key whose account may serve billed rows. free rows draw
	// from every key; a billed row draws only these, when any exists.
	Paid bool
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
// reloads reset cooldown state. paid lists the key names that may serve
// billed rows.
func (p *keyedPool) syncKeys(m map[string]string, paid []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	isPaid := make(map[string]bool, len(paid))
	for _, n := range paid {
		if n = strings.TrimSpace(n); n != "" {
			isPaid[n] = true
		}
	}
	p.keys = nil
	for name, key := range m {
		if strings.TrimSpace(key) == "" {
			continue
		}
		p.keys = append(p.keys, &keyedKey{Name: name, Key: key, Paid: isPaid[name]})
	}
	sort.Slice(p.keys, func(i, j int) bool { return p.keys[i].Name < p.keys[j].Name })
}

// hasPaid reports whether any key is marked paid: only then does a billed row
// have a key of its own to draw, and only then is the draw restricted.
func (p *keyedPool) hasPaid() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, k := range p.keys {
		if k.Paid {
			return true
		}
	}
	return false
}

// pick returns one usable key, skipping cooldowns and keys the ladder
// already 429'd on. paidOnly restricts the draw to keys marked paid.
func (p *keyedPool) pick(exclude map[string]bool, paidOnly bool) *keyedKey {
	p.mu.RLock()
	defer p.mu.RUnlock()
	live := p.keys[:0:0]
	now := time.Now()
	for _, k := range p.keys {
		if exclude[k.Name] || now.Before(k.CooldownTil) || (paidOnly && !k.Paid) {
			continue
		}
		live = append(live, k)
	}
	if len(live) == 0 {
		return nil
	}
	return live[rand.Intn(len(live))]
}

// keyBusyOnly reports whether a 429/503 body is about the model's providers
// being busy rather than this key's own quota. the distinction matters: a
// free model that is out of capacity answers 429 with "all providers ... are
// busy", and cooling our key for it takes the whole backend offline — with
// one key that is every uno/openrouter request for the next two minutes —
// while the key itself was never limited. the key is left untouched and the
// answer goes through; the next request may well land on a model that works.
func keyBusyOnly(body []byte) bool {
	s := strings.ToLower(string(body))
	for _, m := range []string{
		"providers for model",
		"are busy right now",
		"rate-limited upstream",
		"no endpoints",
		"temporarily rate-limited",
	} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
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
	// allows gates the request path on the same free/paid policy the
	// listing uses; nil = no gate.
	allows func(id string) bool
	// paidFor reports whether an id is billed, so its send draws a paid key
	// when the pool has one; nil = every id draws the whole pool.
	paidFor func(id string) bool
	// priceFor reports one row's usd price per million tokens, for the
	// backends whose own catalog publishes one. nil = no price known.
	priceFor func(id string) (in, out float64, ok bool)
	// facts is the provider's own metadata row for one id. it wins over
	// model_info.jsonc in /v1/models: the provider knows its exact model and
	// keeps it current, the json is a curated guess. nil = no extra facts.
	facts func(id string) *modelInfoEntry
}

// keyedCatalogModel is one row of an openai-shaped /models listing. uno and
// orcarouter publish the same shape.
type keyedCatalogModel struct {
	ID      string   `json:"id"`
	Types   []string `json:"supported_endpoint_types"`
	Context int      `json:"context_length"`
	MaxOut  int      `json:"max_output_tokens"`
}

// chatCapable reports whether the row can serve /chat/completions: an openai
// endpoint type, and none of the non-chat ones.
func (m keyedCatalogModel) chatCapable() bool {
	var openai bool
	for _, t := range m.Types {
		switch t {
		case "openai":
			openai = true
		case "embedding", "image-generation", "aihorde":
			return false
		}
	}
	return openai
}

// keyedCatalog caches one router's /models listing. the rows keep on any
// fetch failure, and a 30s floor stops a broken upstream from being dialed on
// every /v1/models hit.
type keyedCatalog struct {
	name string
	url  string
	pool *keyedPool
	ttl  time.Duration

	mu   sync.Mutex
	rows []keyedCatalogModel
	at   time.Time
	next time.Time
}

// models returns the cached rows, refetching when the ttl has passed.
func (c *keyedCatalog) models() []keyedCatalogModel {
	c.mu.Lock()
	if len(c.rows) > 0 && time.Since(c.at) < c.ttl {
		rows := c.rows
		c.mu.Unlock()
		return rows
	}
	if time.Now().Before(c.next) {
		rows := c.rows
		c.mu.Unlock()
		return rows
	}
	c.next = time.Now().Add(30 * time.Second)
	c.mu.Unlock()
	return c.refresh()
}

// cached is the last good listing, nil on a first-fetch failure.
func (c *keyedCatalog) cached() []keyedCatalogModel {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rows
}

// refresh pulls the listing and swaps it in on success. a pool key is sent
// when there is one: uno's catalog answers anon with 401, orcarouter's is
// anonymous but strict about a key it thinks is stale.
func (c *keyedCatalog) refresh() []keyedCatalogModel {
	req, err := http.NewRequest("GET", c.url, nil)
	if err != nil {
		acclog.Printf("!! %s catalog: bad request: %v", c.name, err)
		return c.cached()
	}
	keyedSetUA(req)
	if c.pool != nil {
		// the listing is free: any key will do, paid ones included.
		if k := c.pool.pick(nil, false); k != nil {
			req.Header.Set("Authorization", "Bearer "+k.Key)
		}
	}
	cl := &http.Client{Timeout: 15 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		acclog.Printf("!! %s catalog: fetch failed: %v", c.name, err)
		return c.cached()
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		acclog.Printf("!! %s catalog: upstream %d", c.name, resp.StatusCode)
		return c.cached()
	}
	var payload struct {
		Data []keyedCatalogModel `json:"data"`
	}
	// cap the body: an upstream blob must not feed unbounded allocation.
	if json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&payload) != nil {
		acclog.Printf("!! %s catalog: decode failed", c.name)
		return c.cached()
	}
	out := make([]keyedCatalogModel, 0, len(payload.Data))
	for _, m := range payload.Data {
		if strings.TrimSpace(m.ID) != "" {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return c.cached()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	c.mu.Lock()
	c.rows, c.at = out, time.Now()
	c.mu.Unlock()
	return out
}

// keyedWanted reports whether one catalog row is part of a backend's served
// set: free rows always, paid rows only with paid_models on, and any explicit
// whitelist entry regardless of both.
func keyedWanted(id string, free, paidAll bool, wl map[string]bool) bool {
	if wl[id] {
		return true
	}
	return free || paidAll
}

// keyedWhitelist folds model_whitelist into bare upstream ids. entries may be
// written with or without the backend's listing prefix.
func keyedWhitelist(prefix string, ids []string) map[string]bool {
	if len(ids) == 0 {
		return nil
	}
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		m[strings.TrimPrefix(id, prefix)] = true
	}
	return m
}

// keyedServeRows filters an openai-shaped catalog by the free/paid policy and
// appends whitelisted ids the upstream does not list.
func keyedServeRows(rows []keyedCatalogModel, isFree func(string) bool, paidAll bool, wl map[string]bool) []keyedModel {
	out := make([]keyedModel, 0, len(rows))
	seen := make(map[string]bool, len(rows))
	for _, m := range rows {
		if !m.chatCapable() || !keyedWanted(m.ID, isFree(m.ID), paidAll, wl) {
			continue
		}
		seen[m.ID] = true
		out = append(out, keyedModel{ID: m.ID, Context: m.Context, MaxOut: m.MaxOut})
	}
	for id := range wl {
		if !seen[id] {
			seen[id] = true
			out = append(out, keyedModel{ID: id})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

type keyedResult struct {
	Status      int
	Body        []byte
	Header      http.Header
	KeyName     string
	RateLimited bool
	// Local marks a 429 this proxy synthesized because every key was already
	// cooling: no upstream call was made, so a log must not read as one.
	Local   bool
	Retries int
	// Live is the untouched upstream stream on a 200, handed to the caller
	// as it arrives. Body is empty in that case.
	Live io.ReadCloser
}

// keyedSetUA reuses nousSetUA: the nimroute UA is what stopped those relays'
// edges 403ing the plain Go default one.
func keyedSetUA(req *http.Request) { nousSetUA(req) }

// keyedHeaderWait bounds one attempt's wait for response headers. some relays
// hold a request open until their own rate window opens and then refuse it, so
// without this a caller waits ~a minute for a 429 and reads our eventual
// timeout as a 502.
const keyedHeaderWait = 20 * time.Second

// keyedClientGone is not an http status: it says the caller hung up, so no
// answer can be written and the attempt was not an upstream failure.
const keyedClientGone = 499

// timerAfter is time.After, kept behind a name so the header wait reads as
// what it is at every call site.
func timerAfter(d time.Duration) <-chan time.Time { return time.After(d) }

// keyedLive is a passed-through upstream stream that releases its per-attempt
// context when the reader closes.
type keyedLive struct {
	io.ReadCloser
	cancel func()
}

func (l *keyedLive) Close() error {
	err := l.ReadCloser.Close()
	l.cancel()
	return err
}

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
func keyedSend(b *keybackend, ctx context.Context, method, target string, body []byte, hdr http.Header, model string, stream bool) *keyedResult {
	exclude := map[string]bool{}
	attempts := b.pool.size() + 1
	if attempts < 2 {
		attempts = 2
	}
	if attempts > 6 {
		attempts = 6
	}
	// a stream has no total deadline: the cap belongs on waiting for headers
	// (keyedHeaderWait), not on how long a model may talk. a 300s client
	// timeout is what ended long answers mid-stream.
	timeout := 300 * time.Second
	if stream {
		timeout = 0
	}
	cl := dbgClient(&http.Client{Timeout: timeout}, b.name)
	// a billed row bills the keys marked for it, never a free-tier one: free
	// rows keep drawing the whole pool, and a backend with no key marked paid
	// is unchanged.
	paidOnly := b.paidFor != nil && b.paidFor(model) && b.pool.hasPaid()
	var last429 *keyedResult
	// a dial/tls failure is the tunnel, not the key, so it gets one retry on a
	// fresh connection; the reason is kept so a 502 can name what broke
	// instead of saying "unavailable".
	var lastFail string
	fails := 0
	// the last non-200 an attempt got: a ladder that ends here hands the client
	// the upstream's own words rather than a synthesized 502.
	var lastStat int
	var lastBody []byte
	var lastHdr http.Header
	var lastKeyName string
	for attempt := 1; attempt <= attempts; attempt++ {
		if ctx.Err() != nil {
			return &keyedResult{Status: keyedClientGone}
		}
		key := b.pool.pick(exclude, paidOnly)
		if key == nil {
			break
		}
		// the wait is on headers only: a generation may stream for minutes,
		// so the deadline is a timer here, not a context the body inherits.
		actx, cancel := context.WithCancel(ctx)
		req, err := http.NewRequestWithContext(actx, method, target, bytes.NewReader(body))
		if err != nil {
			cancel()
			continue
		}
		for k, v := range hdr {
			req.Header[k] = v
		}
		req.Header.Set("Authorization", "Bearer "+key.Key)
		keyedSetUA(req)
		type attemptRes struct {
			resp *http.Response
			err  error
		}
		done := make(chan attemptRes, 1)
		go func() { resp, err := cl.Do(req); done <- attemptRes{resp, err} }()
		var resp *http.Response
		select {
		case r := <-done:
			resp, err = r.resp, r.err
		case <-timerAfter(keyedHeaderWait):
			// no headers inside the budget: the relay is holding the request
			// until its own window opens, not serving it. cut it, rotate, and
			// leave the caller a real answer instead of a minute of silence.
			cancel()
			<-done
			acclog.Printf("  %s header-timeout [%s] model=%s", b.name, key.Name, model)
			b.pool.cool(key.Name)
			exclude[key.Name] = true
			continue
		}
		if err != nil {
			cancel()
			if ctx.Err() != nil {
				return &keyedResult{Status: keyedClientGone}
			}
			lastFail = err.Error()
			acclog.Printf("  !! %s transport-error [%s] model=%s: %v", b.name, key.Name, model, err)
			fails++
			if fails > 1 {
				break
			}
			continue
		}
		// a stream is handed through as it arrives. buffering it made ttfb the
		// whole generation, and a long answer could die on our own read before
		// the client ever saw a token.
		if stream && resp.StatusCode == http.StatusOK {
			b.pool.clear(key.Name)
			return &keyedResult{Status: resp.StatusCode, Header: resp.Header,
				KeyName: key.Name, Retries: attempt,
				Live: &keyedLive{ReadCloser: resp.Body, cancel: cancel}}
		}
		respBody, err := io.ReadAll(io.LimitReader(resp.Body, upstreamBodyLimit))
		resp.Body.Close()
		cancel()
		if err != nil {
			lastFail = err.Error()
			acclog.Printf("  !! %s body-read [%s] model=%s: %v", b.name, key.Name, model, err)
			fails++
			if fails > 1 {
				break
			}
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == 529 {
			// the model's providers being busy is not this key's limit:
			// cooling for it is what took a whole backend down over one
			// popular free model.
			if keyBusyOnly(respBody) {
				acclog.Printf("  %s model-busy %d [%s] model=%s (key left alone)", b.name, resp.StatusCode, key.Name, model)
				return &keyedResult{Status: resp.StatusCode, Body: respBody, Header: resp.Header, KeyName: key.Name, RateLimited: true, Retries: attempt}
			}
			b.pool.cool(key.Name)
			last429 = &keyedResult{Status: resp.StatusCode, Body: respBody, Header: resp.Header, KeyName: key.Name, RateLimited: true, Retries: attempt}
			exclude[key.Name] = true
			acclog.Printf("  !! %d [%s:%s] model=%s", resp.StatusCode, b.name, key.Name, model)
			continue
		}
		if resp.StatusCode == http.StatusBadGateway ||
			resp.StatusCode == http.StatusServiceUnavailable ||
			resp.StatusCode == http.StatusGatewayTimeout {
			// the relay's own edge failed the call. one retry is worth it (a
			// different key, or a healthy edge), and the response stays as the
			// fallback answer so the client reads the real error.
			lastStat, lastBody, lastHdr, lastKeyName = resp.StatusCode, respBody, resp.Header, key.Name
			fails++
			acclog.Printf("  !! %d %s [%s] model=%s %q", resp.StatusCode, b.name, key.Name, model,
				extractErrMessage(string(respBody)))
			if fails > 1 {
				exclude[key.Name] = true
			}
			continue
		}
		if resp.StatusCode == http.StatusOK {
			b.pool.clear(key.Name)
		}
		return &keyedResult{Status: resp.StatusCode, Body: respBody, Header: resp.Header, KeyName: key.Name, Retries: attempt}
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
			Local:       true,
		}
	}
	if last429 != nil {
		return last429
	}
	if lastStat != 0 {
		return &keyedResult{Status: lastStat, Body: lastBody, Header: lastHdr,
			KeyName: lastKeyName, Retries: attempts}
	}
	// nothing reached upstream at all: name the reason. a bare "upstream
	// unavailable" hid the tls/reset/cancel that actually happened.
	reason := lastFail
	if reason == "" {
		if b.pool.size() == 0 {
			reason = "no api key configured"
		} else {
			reason = "no key available"
		}
	}
	if len(reason) > 160 {
		reason = reason[:160]
	}
	msg := b.name + ": upstream unavailable (" + reason + ")"
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	acclog.Printf("  !! 502 %s %s model=%s", b.name, msg, model)
	return &keyedResult{
		Status: http.StatusBadGateway,
		Body:   []byte(`{"error":{"type":"api_error","message":` + strconv.Quote(msg) + `}}`),
		Header: h,
	}
}

// keyedChat serves the openai-shaped /v1/chat/completions path against one
// keyed backend.
func (p *Pool) keyedChat(w http.ResponseWriter, r *http.Request, body []byte,
	model string, isStream bool, start time.Time, reqID string, b *keybackend) {
	if b.allows != nil && !b.allows(model) {
		msg := b.name + ": " + model + " is not served; set " + b.name + ".paid_models: true or add it to " + b.name + ".model_whitelist"
		acclog.Printf("  !! %s 400 blocked model=%s (paid model, policy off)", b.name, model)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":` + strconv.Quote(msg) + `,"type":"invalid_request_error","code":"model_not_served"}}`))
		return
	}
	target := b.base + strings.TrimPrefix(r.URL.Path, "/v1")
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	acclog.Printf("%s routed to %s target=%s model=%s", reqID, b.name, target, model)

	res := keyedSend(b, r.Context(), r.Method, target, body, keyedFwdHeaders(r), model, isStream)
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
	if res.Status == keyedClientGone {
		acclog.Printf("  %s client-gone model=%s (cancelled, nothing sent)", b.name, model)
		return
	}

	// a live upstream stream: copy it through chunk by chunk. the turn is
	// logged once it ends, with the byte count and no token counts (the
	// caller's sse usage arrives too late to be worth holding the stream for).
	if res.Live != nil {
		defer res.Live.Close()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		buf := make([]byte, 32<<10)
		var written int64
		for {
			n, rerr := res.Live.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					break
				}
				written += int64(n)
				if fl != nil {
					fl.Flush()
				}
			}
			if rerr != nil {
				break
			}
		}
		logUsage(UsageRecord{
			Ts:           time.Now().UTC().Format(time.RFC3339Nano),
			Model:        model,
			KeyName:      b.name + ":" + res.KeyName,
			Method:       r.Method,
			Path:         r.URL.Path,
			StatusCode:   http.StatusOK,
			DurationMs:   time.Since(start).Milliseconds(),
			Stream:       true,
			RetryAttempt: res.Retries,
			ContentBytes: written,
		})
		return
	}

	var prompT, compT, totalT int
	var recErr string
	if res.Status != http.StatusOK {
		recErr = extractErrMessage(string(res.Body))
		if len(recErr) > 200 {
			recErr = recErr[:200]
		}
		if res.Local {
			acclog.Printf("  !! %s all keys cooling, no upstream call model=%s retry_after=%ss", b.name, model, res.Header.Get("Retry-After"))
		} else {
			acclog.Printf("  !! %s upstream %d [%s] model=%s err=%q", b.name, res.Status, res.KeyName, model, recErr)
		}
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
	if b.allows != nil && !b.allows(upstreamModel) {
		msg := b.name + ": " + upstreamModel + " is not served; set " + b.name + ".paid_models: true or add it to " + b.name + ".model_whitelist"
		acclog.Printf("  !! %s 400 blocked model=%s (paid model, policy off)", b.name, upstreamModel)
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", msg)
		return
	}
	target := b.base + "/chat/completions"
	acclog.Printf("%s routed to %s target=%s model=%s", reqID, b.name, target, upstreamModel)

	fwd := http.Header{
		"Content-Type": {"application/json"},
		"Accept":       {"application/json"},
	}
	if isStream {
		fwd.Set("Accept", "text/event-stream")
	}
	res := keyedSend(b, r.Context(), "POST", target, oaiBody, fwd, upstreamModel, isStream)
	elapsed := time.Since(start)

	if res == nil {
		errMsg := b.name + ": upstream unavailable"
		logUsage(UsageRecord{
			Ts:         time.Now().UTC().Format(time.RFC3339Nano),
			Model:      usageModel(r, clientModel),
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
	if res.Status == keyedClientGone {
		acclog.Printf("  %s client-gone model=%s (cancelled, nothing sent)", b.name, upstreamModel)
		return
	}

	if res.Status != http.StatusOK {
		errMsg := extractErrMessage(string(res.Body))
		if res.Local {
			acclog.Printf("  !! %s all keys cooling, no upstream call model=%s retry_after=%ss", b.name, clientModel, res.Header.Get("Retry-After"))
		} else {
			acclog.Printf("  !! %s upstream %d [%s] model=%s err=%q", b.name, res.Status, res.KeyName, clientModel, errMsg)
		}
		logUsage(UsageRecord{
			Ts:           time.Now().UTC().Format(time.RFC3339Nano),
			Model:        usageModel(r, clientModel),
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
		var src io.Reader = bytes.NewReader(res.Body)
		if res.Live != nil {
			defer res.Live.Close()
			src = res.Live
		}
		written, promptT, compT := streamAnthropicReader(r.Context(), w, src, clientModel, tnames, nil)
		logUsage(UsageRecord{
			Ts:               time.Now().UTC().Format(time.RFC3339Nano),
			Model:            usageModel(r, clientModel),
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
			Model:      usageModel(r, clientModel),
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
		Model:            usageModel(r, clientModel),
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
			if entry.TopProvider == nil {
				entry.TopProvider = map[string]any{}
			}
			entry.TopProvider["context_length"] = m.Context
		}
		if m.MaxOut > 0 {
			// keep the window alongside the cap: replacing the map drops it.
			if entry.TopProvider == nil {
				entry.TopProvider = map[string]any{}
			}
			entry.TopProvider["max_completion_tokens"] = m.MaxOut
			entry.TopProvider["context_length"] = entry.ContextLength
		}
		// the provider's own row wins over the curated json and the
		// openrouter overlay; fields it omits stay as they were.
		if b.facts != nil {
			if f := b.facts(m.ID); f != nil {
				applyFacts(&entry, f)
			}
		}
		// a backend whose own catalog quotes prices (uno ships a separate
		// pricing catalog) states them in both forms, like openrouter's rows.
		if b.priceFor != nil {
			if in, out, ok := b.priceFor(m.ID); ok {
				in, out = roundPrice(in), roundPrice(out)
				entry.Price1M = map[string]any{"input": in, "output": out, "source": b.name}
				entry.Pricing = map[string]string{
					"prompt":     strconv.FormatFloat(roundPrice(in/1e6), 'g', -1, 64),
					"completion": strconv.FormatFloat(roundPrice(out/1e6), 'g', -1, 64),
					"request":    "0",
				}
			}
		}
		out.Data = append(out.Data, entry)
	}
}

// applyFacts overlays a provider's own metadata on a listing row. whatever
// the provider states wins over model_info.jsonc and the openrouter overlay,
// because it describes that provider's exact model and stays current; a field
// the provider omits is left as it was.
func applyFacts(m *openRouterModel, e *modelInfoEntry) {
	if e.Name != "" {
		m.Name = e.Name
	}
	if e.Desc != "" {
		m.Description = e.Desc
	}
	if m.TopProvider == nil {
		m.TopProvider = map[string]any{}
	}
	if e.Ctx > 0 {
		m.ContextLength = e.Ctx
		m.TopProvider["context_length"] = e.Ctx
	}
	if e.MaxOut > 0 {
		m.TopProvider["max_completion_tokens"] = e.MaxOut
	}
	if m.Architecture == nil {
		m.Architecture = map[string]any{}
	}
	if len(e.Inputs) > 0 {
		m.Architecture["input_modalities"] = e.Inputs
	}
	if len(e.OutMods) > 0 {
		m.Architecture["output_modalities"] = e.OutMods
	}
	if len(e.SupportedParams) > 0 {
		m.SupportedParams = e.SupportedParams
	}
	if e.Knowledge != "" {
		m.KnowledgeCutoff = e.Knowledge
	}
	if e.Released != "" {
		m.Created = releaseUnix(e.Released)
	}
	if e.Reasoning != nil {
		r := map[string]any{"mandatory": *e.Reasoning}
		if len(e.EffortLevels) > 0 {
			r["supported_efforts"] = e.EffortLevels
		}
		m.Reasoning = r
	} else if len(e.EffortLevels) > 0 {
		if m.Reasoning == nil {
			m.Reasoning = map[string]any{}
		}
		m.Reasoning["supported_efforts"] = e.EffortLevels
	}
}

// keyedUnderPriceCap reports whether one id passes a backend's input price
// cap, in usd per million tokens. 0 = no cap. free rows quote 0 and always
// pass, so a cap only ever hides the expensive paid rows.
func keyedUnderPriceCap(price func(string) (in, out float64, ok bool), cap float64, id string) bool {
	if cap <= 0 || price == nil {
		return true
	}
	if in, _, ok := price(id); ok && in > cap {
		return false
	}
	return true
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
	sum.Models = foldStatusIDs(b.name+"/", sum.Models)
	return sum
}
