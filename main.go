package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"os/signal"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"nimroute/pii"
)

// nvidiaBase is a var so a test can point it at a slow fake upstream.
var nvidiaBase = "https://integrate.api.nvidia.com/v1"

const (
	cooldown429   = 1 * time.Minute
	maxBackoff    = 16 * time.Minute
	failWindow    = 50 * time.Minute
	idleScale     = 10 * time.Minute
	modelLockout  = 30 * time.Second
	burstCooldown = 5 * time.Second
	bodyLimit     = 10 << 20
	checkInterval = 5 * time.Second
	// upstreamBodyLimit caps bodies read fully into memory (non-streamed
	// completions, error bodies, nudges) and a single SSE line for the
	// scanners. streams are piped and never buffered, so the only bodies
	// that hit this are ones a client asked for whole: 8mb is orders past
	// a legitimate completion and bounds per-request ram (x16 slots).
	upstreamBodyLimit = 8 << 20
)

// versionStr is overridden at build time via -ldflags "-X main.versionStr=x.y.z".
var versionStr = "2.6.0"

// OpencodeBase is a var so tests can point the zen path at a fake upstream.
// ZEN_BASE_URL overrides it for benchmarking against a local mock.
var OpencodeBase = "https://opencode.ai/inference/v1"

func init() {
	if u := os.Getenv("ZEN_BASE_URL"); u != "" {
		OpencodeBase = strings.TrimRight(u, "/")
	}
}

type Key struct {
	Name           string
	Key            string
	CooldownUntil  time.Time
	CooldownReason string
	LastUsed       time.Time
	FailCount      int
	Consec429      int
	LastFail       time.Time
}

type ModelLock struct {
	mu   sync.Mutex
	data map[string]map[string]time.Time
}

func (m *ModelLock) isLocked(key, model string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data == nil {
		return false
	}
	if perKey, ok := m.data[key]; ok {
		return time.Now().Before(perKey[model])
	}
	return false
}

func (m *ModelLock) lock(key, model string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data == nil {
		m.data = make(map[string]map[string]time.Time)
	}
	if m.data[key] == nil {
		m.data[key] = make(map[string]time.Time)
	}
	// client-supplied model ids are unbounded: evict expired entries on
	// write so the map can't grow forever on unique-model loops.
	now := time.Now()
	for mdl, until := range m.data[key] {
		if !now.Before(until) {
			delete(m.data[key], mdl)
		}
	}
	if len(m.data[key]) >= 4096 {
		return
	}
	m.data[key][model] = now.Add(d)
}

func (m *ModelLock) lockedModels(key string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	var out []string
	if perKey, ok := m.data[key]; ok {
		for mdl, until := range perKey {
			if now.Before(until) {
				out = append(out, mdl)
			}
		}
	}
	sort.Strings(out)
	return out
}

// lastKeyEntry is one model's pinned key plus a write sequence. seq breaks
// ties inside a single clock tick so eviction is deterministic.
type lastKeyEntry struct {
	key string
	seq uint64
}

type Pool struct {
	keys  []*Key
	mu    sync.RWMutex
	start time.Time
	locks ModelLock
	// sem caps in-flight requests. mutex+cond, not a chan, so a config
	// reload can resize the cap without dropping in-flight slots.
	semMu      sync.Mutex
	semCond    sync.Cond
	semUsed    int
	semLimit   int
	concurrent atomic.Int64
	// lastKey pins a model to the key that last served it, so a
	// conversation stays on one key and NIM's per-key prompt cache stays
	// warm. the entry's seq is what lets noteKey evict the stalest one
	// instead of refusing every new id once the map is full.
	lastKey    map[string]lastKeyEntry
	lastKeySeq uint64

	// lastZen records the most recent zen upstream session/proxy for /status.
	lastZenSession string
	lastZenProxy   string
	lastZenAt      time.Time
	// per-backend in-flight counts: nvidia keys vs opencode zen lanes.
	concurrentNV  atomic.Int64
	concurrentZen atomic.Int64
}

func newPool(entries map[string]string) *Pool {
	p := &Pool{start: time.Now(), lastKey: make(map[string]lastKeyEntry)}
	p.semCond.L = &p.semMu
	for name, k := range entries {
		p.keys = append(p.keys, &Key{Name: name, Key: k})
	}
	p.semLimit = semLimitFor(entries)
	limit := p.semLimit
	if len(entries) == 0 {
		log.Printf("  Concurrency limit: %d (keyless, = zen lanes)", limit)
	} else {
		log.Printf("  Concurrency limit: %d (75%% of %d keys)", limit, len(entries))
	}
	sort.Slice(p.keys, func(i, j int) bool { return p.keys[i].Name < p.keys[j].Name })
	return p
}

// semLimitFor resolves the in-flight cap: keyless follows zen lanes so the
// two move together, keyed keeps the 75%-of-keys rule.
func semLimitFor(entries map[string]string) int {
	if len(entries) == 0 {
		return laneCap()
	}
	return (len(entries)*3 + 3) / 4 // ceil(75%)
}

// syncSemLimit re-resolves the cap after a config reload. shrinking never
// drops in-flight requests; new ones just queue until drain.
func (p *Pool) syncSemLimit(entries map[string]string) {
	n := semLimitFor(entries)
	p.semMu.Lock()
	old := p.semLimit
	p.semLimit = n
	p.semMu.Unlock()
	if n != old {
		p.semCond.Broadcast()
		log.Printf("  Concurrency limit: %d -> %d", old, n)
	}
}

// semAcquire blocks until an in-flight slot frees. ctx-aware: a client
// disconnect stops queueing instead of holding a slot nobody needs.
// the watcher goroutine spawns only on the wait path, not per request.
func (p *Pool) semAcquire(ctx context.Context) bool {
	p.semMu.Lock()
	defer p.semMu.Unlock()
	if p.semUsed < p.semLimit {
		p.semUsed++
		return true
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			p.semCond.Broadcast()
		case <-done:
		}
	}()
	for p.semUsed >= p.semLimit {
		p.semCond.Wait()
		if ctx.Err() != nil {
			return false
		}
	}
	p.semUsed++
	return true
}

// semRelease frees a slot and wakes one waiter.
func (p *Pool) semRelease() {
	p.semMu.Lock()
	p.semUsed--
	p.semMu.Unlock()
	p.semCond.Signal()
}

// semStats reports (used, limit) for the queue log and /status.
func (p *Pool) semStats() (int, int) {
	p.semMu.Lock()
	defer p.semMu.Unlock()
	return p.semUsed, p.semLimit
}

// semCap reports the limit alone.
func (p *Pool) semCap() int {
	_, n := p.semStats()
	return n
}

// keyCount snapshots the key pool size: hot reload swaps p.keys under p.mu,
// so reading len(p.keys) directly is a data race.
func (p *Pool) keyCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.keys)
}

// copyZenChatStream pipes an upstream zen chat SSE body to the client,
// flushing each read. it stops as soon as the client context is done, so a
// cancelled request closes upstream instead of draining generation nobody
// reads. usage tokens are teed off the passthrough. extra, when non-nil,
// receives a copy of everything written (debug mode).
func copyZenChatStream(ctx context.Context, w http.ResponseWriter, resp *http.Response, extra *bytes.Buffer, prompT, compT int) (written int64, promptT, completionT int) {
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	// carry: a usage chunk split across reads is still caught, usage json
	// is small and lands wholly in one final chunk.
	var carry []byte
	for {
		if ctx.Err() != nil {
			resp.Body.Close()
			return written, prompT, compT
		}
		n, err := resp.Body.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			if extra != nil {
				extra.Write(buf[:n])
			}
			blk := buf[:n]
			if len(carry) > 0 {
				blk = append(carry, buf[:n]...)
				carry = nil
			}
			if p, c := sseUsageTee(blk); p > 0 || c > 0 {
				prompT, compT = p, c
			} else if len(blk) > 512 {
				carry = append(carry[:0], blk[len(blk)-512:]...)
			} else {
				carry = append(carry[:0], blk...)
			}
			if fl != nil {
				fl.Flush()
			}
			written += int64(n)
		}
		if err != nil {
			return written, prompT, compT
		}
	}
}

func (p *Pool) Reload(entries map[string]string) (added, removed int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	old := make(map[string]*Key, len(p.keys))
	for _, k := range p.keys {
		old[k.Name] = k
	}
	seen := make(map[string]bool, len(entries))
	fresh := make([]*Key, 0, len(entries))
	for name, k := range entries {
		seen[name] = true
		if existing, ok := old[name]; ok {
			existing.Key = k
			fresh = append(fresh, existing)
		} else {
			fresh = append(fresh, &Key{Name: name, Key: k})
			added++
		}
	}
	for name := range old {
		if !seen[name] {
			removed++
		}
	}
	sort.Slice(fresh, func(i, j int) bool { return fresh[i].Name < fresh[j].Name })
	p.keys = fresh
	return
}

// PickSticky picks a key, preferring the last key that served model (sticky)
// when it is still available, so prompt caches stay warm. Sticky keys only win
// if usable; otherwise fall back to the normal weighted pick. returns a
// snapshot copy: callers must never dereference pool-owned memory outside
// the lock (a hot-reload write would tear the read). mutations go through
// rateLimit/Clear429/Postpone, which resolve by name under the lock.
func (p *Pool) PickSticky(exclude map[string]bool, model, sticky string) *Key {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	var avail []*Key
	var stickyKey *Key
	for _, k := range p.keys {
		if exclude[k.Name] {
			continue
		}
		if now.Before(k.CooldownUntil) {
			continue
		}
		if p.locks.isLocked(k.Name, model) {
			continue
		}
		avail = append(avail, k)
		if k.Name == sticky {
			stickyKey = k
		}
	}
	if len(avail) == 0 {
		return nil
	}
	pick := avail[len(avail)-1]
	if stickyKey != nil {
		pick = stickyKey
	} else {
		// weighted pick: favor idle keys, sink recently-failed/hot keys
		total := 0.0
		weights := make([]float64, len(avail))
		for i, k := range avail {
			w := idleWeight(k, now)
			total += w
			weights[i] = w
		}
		r := rand.Float64() * total
		for i, w := range weights {
			r -= w
			if r <= 0 {
				pick = avail[i]
				break
			}
		}
	}
	pick.LastUsed = now
	cp := *pick
	return &cp
}

// noteKey records that model was last served by key name, pinning stickiness.
// the map is capped: client-supplied ids are unbounded.
func (p *Pool) noteKey(model, name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.lastKey[model]; !ok && len(p.lastKey) >= 4096 {
		evictStalestKeyLocked(p)
	}
	p.lastKeySeq++
	p.lastKey[model] = lastKeyEntry{key: name, seq: p.lastKeySeq}
}

// evictStalestKeyLocked drops the map's oldest write so a model loop past
// the cap keeps working instead of silently pinning nothing. caller holds p.mu.
func evictStalestKeyLocked(p *Pool) {
	var worst string
	var worstSeq uint64
	first := true
	for model, e := range p.lastKey {
		if first || e.seq < worstSeq {
			worst, worstSeq, first = model, e.seq, false
		}
	}
	if !first {
		delete(p.lastKey, worst)
	}
}

// stickyKey returns the name of the key that last served model, if any.
func (p *Pool) stickyKey(model string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lastKey[model].key
}

// idleWeight scores a key 0..1: more idle -> higher, failed within window -> crushed.
func idleWeight(k *Key, now time.Time) float64 {
	w := 1.0
	if !k.LastUsed.IsZero() {
		idle := now.Sub(k.LastUsed)
		if idle < 0 {
			idle = 0
		}
		// 0 at 0 idle, approach 1 as idle grows past idleScale
		f := float64(idle) / float64(idleScale)
		if f > 1 {
			f = 1
		}
		// balanced: floor at 0.3 so never-idle keys still have a shot
		w = 0.3 + 0.7*f
	}
	if !k.LastFail.IsZero() && now.Sub(k.LastFail) < failWindow {
		// failed within the window: steep penalty, escalates with consecutive 429s
		pen := 0.2
		for i := 0; i < k.Consec429 && i < 4; i++ {
			pen *= 0.5
		}
		w *= pen
	}
	return w
}

// poolKey resolves k to the pool-owned entry by name (k itself may be a
// PickSticky snapshot copy). test-constructed keys outside the pool fall
// back to the passed pointer.
func (p *Pool) poolKey(k *Key) *Key {
	for _, c := range p.keys {
		if c.Name == k.Name {
			return c
		}
	}
	return k
}

func (p *Pool) rateLimit(k *Key) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k = p.poolKey(k)
	k.Consec429++
	k.FailCount++
	k.LastFail = time.Now()
	// clamp the shift: unbounded Consec429 overflows the duration into the
	// negative (past) after ~31 doublings, silently disabling backoff.
	shift := k.Consec429 - 1
	if shift > 8 {
		shift = 8
	}
	d := cooldown429 << shift
	if d > maxBackoff {
		d = maxBackoff
	}
	k.CooldownUntil = time.Now().Add(d)
	k.CooldownReason = "rate_limited_429"
}

func (p *Pool) Clear429(k *Key) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.poolKey(k).Consec429 = 0
}

func (p *Pool) Postpone(k *Key, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.poolKey(k)
	t.CooldownUntil = time.Now().Add(d)
	t.CooldownReason = "post_request"
}

// callUpstream runs the retry loop against NIM and returns the result.
// Returns (nil, nil) when all keys exhausted, (nil, error) on hard error.
// ctx cancels the upstream send: a client disconnect stops the request on
// nvidia's end instead of burning tokens on output nobody reads.
func (p *Pool) callUpstream(ctx context.Context, method, target string, body []byte, fwd http.Header, model, remoteAddr string) (*upstreamResult, error) {
	exclude := make(map[string]bool)
	cl := dbgClient(&http.Client{Timeout: 300 * time.Second}, "nvidia")
	var retries int
	var rateLimited bool
	var used *Key

	sticky := p.stickyKey(model)
	keyCount := p.keyCount()
	for attempt := 0; attempt < max(keyCount*2+2, 4); attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		key := p.PickSticky(exclude, model, sticky)
		if key == nil {
			if attempt < 3 {
				select {
				case <-time.After(200 * time.Millisecond):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				continue
			}
			dbglog.Printf("no keys available at attempt %d (excluded=%d, model=%s)", attempt, len(exclude), model)
			break
		}
		used = key

		req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("NewRequest: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+key.Key)
		req.Header.Set("Host", "integrate.api.nvidia.com")
		req.Header.Set("X-Forwarded-For", remoteAddr)
		for k, v := range fwd {
			req.Header[k] = v
		}

		resp, err := cl.Do(req)
		if err != nil {
			return nil, fmt.Errorf("upstream: %w", err)
		}

		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == 529 {
			rateLimited = true
			p.rateLimit(key)
			p.locks.lock(key.Name, model, modelLockout)
			resp.Body.Close()
			rh := rlHeaders(resp.Header)
			// clamp before shifting: see rateLimit — unbounded shifts overflow
			// the duration negative, disabling backoff entirely.
			shift := key.Consec429 - 1
			if shift > 8 {
				shift = 8
			}
			cd := cooldown429 << shift
			if cd > maxBackoff {
				cd = maxBackoff
			}
			acclog.Printf("!! %d [%s] model=%s key-backoff=%v model-lockout=%v headers=%v", resp.StatusCode, key.Name, model, cd, modelLockout, rh)
			exclude[key.Name] = true
			if attempt > 0 {
				retries++
			}
			continue
		}

		rb, _ := io.ReadAll(io.LimitReader(resp.Body, upstreamBodyLimit))
		resp.Body.Close()

		bodyStr := string(rb)
		if resp.StatusCode == http.StatusForbidden || (resp.StatusCode != http.StatusOK && strings.Contains(bodyStr, "ResourceExhausted")) {
			p.locks.lock(key.Name, model, burstCooldown)
			acclog.Printf("!! 403/ResourceExhausted [%s] model=%s burst-backoff=%v", key.Name, model, burstCooldown)
			exclude[key.Name] = true
			if attempt > 0 {
				retries++
			}
			continue
		}

		if attempt > 0 {
			retries++
		}
		p.noteKey(model, used.Name)
		return &upstreamResult{
			StatusCode:  resp.StatusCode,
			Header:      resp.Header.Clone(),
			Body:        rb,
			Key:         used.Name,
			KeyObj:      used,
			Retries:     retries,
			RateLimited: rateLimited,
		}, nil
	}

	return nil, nil
}

type KeyStatus struct {
	Name           string `json:"name"`
	Suffix         string `json:"suffix"`
	Cooldown       string `json:"cooldown,omitempty"`
	CooldownReason string `json:"cooldown_reason,omitempty"`
	LastUsed       string `json:"last_used,omitempty"`
	FailCount      int    `json:"fail_count"`
	Consec429      int    `json:"consec_429"`
	Available      bool   `json:"available"`
}

type StatusResponse struct {
	OK            bool                   `json:"ok"`
	Version       string                 `json:"version"`
	Uptime        string                 `json:"uptime"`
	Total         int                    `json:"total"`
	Available     int                    `json:"available"`
	Concurrent    int                    `json:"concurrent"`
	ConcurrentNV  int                    `json:"concurrent_nvidia"`
	ConcurrentZen int                    `json:"concurrent_opencode"`
	SemLimit      int                    `json:"sem_limit"`
	Keys          []KeyStatus            `json:"keys,omitempty"`
	Locks         interface{}            `json:"model_locks,omitempty"`
	Opencode      *OpencodeInfo          `json:"opencode,omitempty"`
	ZenSession    string                 `json:"zen_session,omitempty"`
	ZenProxy      string                 `json:"zen_proxy,omitempty"`
	ZenAgo        string                 `json:"zen_ago,omitempty"`
	ZenLanes      []laneStatus           `json:"zen_lanes,omitempty"`
	LaneCap       int                    `json:"lane_cap,omitempty"`
	Pool          *poolSummary           `json:"pool,omitempty"`
	Usage         *usageSummary          `json:"usage,omitempty"`
	Health        map[string]modelHealth `json:"health,omitempty"`
	Freepi        *freepiSummary         `json:"freepi,omitempty"`
	Antseed       *antseedSummary        `json:"antseed,omitempty"`
	Nous          *nousSummary           `json:"nous,omitempty"`
	Uno           *keyedSummary          `json:"uno,omitempty"`
	OpenRouter    *keyedSummary          `json:"openrouter,omitempty"`
	Orcarouter    *keyedSummary          `json:"orcarouter,omitempty"`
	Route         *routeSummary          `json:"route,omitempty"`
	Wroute        *wrouteSummary         `json:"wroute,omitempty"`
	Benchmarks    *benchSummary          `json:"benchmarks,omitempty"`
	Prices        map[string]pricePair   `json:"prices,omitempty"`
}

// pricePair is one model's usd price per million tokens as its own provider
// quotes it. a present row means the price is known: 0/0 is free, absent is
// unknown. it feeds the status model table, never the request path.
type pricePair struct {
	In  float64 `json:"in"`
	Out float64 `json:"out"`
}

// freepiAcctStatus is one account's /status row. jwt never leaves config.
type freepiAcctStatus struct {
	Name       string            `json:"name"`
	Models     []string          `json:"models,omitempty"`
	Mults      map[string]string `json:"mults,omitempty"`
	Requests   int               `json:"requests"`
	Prompt     int               `json:"prompt_tokens"`
	Completion int               `json:"completion_tokens"`
	Inflight   bool              `json:"inflight"`
	Tier       string            `json:"tier,omitempty"`
	Credits    float64           `json:"credits,omitempty"`
	SpentPct   float64           `json:"spent_pct,omitempty"`
	ReqToday   int               `json:"req_today,omitempty"`
}

// freepiSummary groups per-account freepi state, separate from zen.
type freepiSummary struct {
	Enabled  bool               `json:"enabled"`
	Accounts []freepiAcctStatus `json:"accounts,omitempty"`
}

// usageSummary is the /status view of the in-memory counters.
type usageSummary struct {
	Requests   int                    `json:"requests"`
	Prompt     int                    `json:"prompt_tokens"`
	Completion int                    `json:"completion_tokens"`
	ByModel    map[string]usageTotals `json:"by_model,omitempty"`
}

type OpencodeInfo struct {
	Enabled bool     `json:"enabled"`
	Base    string   `json:"base"`
	Models  []string `json:"models"`
}

// upstreamResult holds the outcome of a callUpstream attempt.
type upstreamResult struct {
	StatusCode  int
	Header      http.Header
	Body        []byte
	Key         string
	KeyObj      *Key
	Retries     int
	RateLimited bool
}

func (p *Pool) Status() StatusResponse {
	// keys + locks snapshot under p.mu; everything below takes other
	// locks (lanes, usage, freepi...), so holding RLock across them
	// blocks PickSticky/Reload/rateLimit for the whole /status build.
	now := time.Now()
	p.mu.RLock()
	sr := StatusResponse{
		OK:            true,
		Version:       versionStr,
		Uptime:        now.Sub(p.start).Round(time.Second).String(),
		Total:         len(p.keys),
		Concurrent:    int(p.concurrent.Load()),
		ConcurrentNV:  int(p.concurrentNV.Load()),
		ConcurrentZen: int(p.concurrentZen.Load()),
		SemLimit:      p.semCap(),
	}
	if len(p.keys) > 0 {
		sr.Keys = make([]KeyStatus, len(p.keys))
	}
	locks := make(map[string][]string)
	for i, k := range p.keys {
		avail := true
		ks := KeyStatus{Name: k.Name, Suffix: short(k.Key), FailCount: k.FailCount, Consec429: k.Consec429}
		if now.Before(k.CooldownUntil) {
			ks.Cooldown = k.CooldownUntil.Sub(now).Round(time.Second).String()
			ks.CooldownReason = k.CooldownReason
			avail = false
		}
		if !k.LastUsed.IsZero() {
			ks.LastUsed = now.Sub(k.LastUsed).Round(time.Second).String()
		}
		ks.Available = avail
		if avail {
			sr.Available++
		}
		sr.Keys[i] = ks
		if lms := p.locks.lockedModels(k.Name); len(lms) > 0 {
			locks[k.Name] = lms
		}
	}
	lastZenSession, lastZenProxy, lastZenAt := p.lastZenSession, p.lastZenProxy, p.lastZenAt
	p.mu.RUnlock()
	if len(locks) > 0 {
		sr.Locks = locks
	}
	if zenEnabled() {
		if oi := ocInfo.Load(); oi != nil {
			// copy: ocInfo is shared with /v1/models
			cp := *oi
			cp.Models = foldStatusIDs("", cp.Models)
			sr.Opencode = &cp
		}
	}
	if zenEnabled() {
		sr.ZenLanes = laneSnapshot()
		sr.LaneCap = laneCap()
	}
	if zenEnabled() && cfg().Status.ShowPool {
		ps := poolSnapshot()
		sr.Pool = &ps
	}
	if zenEnabled() && !lastZenAt.IsZero() {
		sr.ZenSession = lastZenSession
		sr.ZenProxy = redactProxyUserinfo(lastZenProxy)
		sr.ZenAgo = now.Sub(lastZenAt).Round(time.Second).String()
	}
	if cfg().Usage.Enabled && cfg().Status.ShowUsage {
		reqs, prompt, comp, byModel := usageSnapshot()
		sr.Usage = &usageSummary{Requests: reqs, Prompt: prompt, Completion: comp, ByModel: byModel}
		sr.Health = modelHealthSnapshot()
	}
	if freepiEnabled() {
		sr.Freepi = freepiSnapshot()
	}
	if antseedEnabled() && cfg().Status.ShowAntseed {
		sr.Antseed = antseedSnapshot()
	}
	if nousEnabled() && cfg().Status.ShowNous {
		sr.Nous = nousSnapshot()
	}
	if unoBackend.enabled() && cfg().Status.ShowUno {
		sr.Uno = keyedSnapshot(unoBackend)
	}
	if orChatBackend.enabled() && cfg().Status.ShowOpenRouter {
		sr.OpenRouter = keyedSnapshot(orChatBackend)
	}
	if orcaBackend.enabled() && cfg().Status.ShowOrcarouter {
		sr.Orcarouter = keyedSnapshot(orcaBackend)
	}
	if cfg().Status.ShowRoute {
		sr.Route = routeSnapshot(p)
	}
	if cfg().Status.ShowWroute {
		sr.Wroute = wrouteSnapshot(p)
	}
	if cfg().Status.ShowBenchmarks {
		sr.Benchmarks = buildBenchSummary()
	}
	if cfg().Status.ShowPrices {
		sr.Prices = priceSnapshot()
	}
	return sr
}

// priceSnapshot maps served id -> the price its own catalog quotes. only
// backends that publish one take part; the rest stay absent (unknown), which
// is what the model table draws as a dot.
func priceSnapshot() map[string]pricePair {
	out := map[string]pricePair{}
	for _, b := range []*keybackend{unoBackend, orChatBackend, orcaBackend} {
		if !b.enabled() || b.priceFor == nil {
			continue
		}
		for _, m := range b.models() {
			if in, o, ok := b.priceFor(m.ID); ok {
				out[b.name+"/"+m.ID] = pricePair{In: roundPrice(in), Out: roundPrice(o)}
			}
		}
	}
	for _, m := range nousFreeModels() {
		if m.PriceOK {
			out["nous/"+m.ID] = pricePair{
				In:  roundPrice(m.PriceIn * 1e6),
				Out: roundPrice(m.PriceOut * 1e6),
			}
		}
	}
	return out
}

// noteZenSuccess records the last working zen session/proxy for /status,
// and pins it sticky so the next request skips the race probe.
func (p *Pool) noteZenSuccess(session, proxy string) {
	p.mu.Lock()
	p.lastZenSession = session
	p.lastZenProxy = proxy
	p.lastZenAt = time.Now()
	p.mu.Unlock()
	noteZenSuccessAt()
}

var (
	acclog    *log.Logger = log.New(io.Discard, "", 0)
	dbglog    *log.Logger = log.New(io.Discard, "", 0)
	debugMode bool
)

// reqSeq hands every inbound model request a short id so verbose log lines
// can be followed from arrival through routing to the final response.
var reqSeq atomic.Int64

// ocInfo caches the opencode zen free-model list fetched at startup.
var ocInfo atomic.Pointer[OpencodeInfo]

// fetchZenModelIDs returns the raw ids from the live zen /models endpoint,
// nil when unreachable so callers keep serving stale/cached entries.
func fetchZenModelIDs() []string {
	return fetchZenModelIDsFrom(OpencodeBase)
}

// nvModelsCache holds the last NVIDIA /models list: /v1/models is
// pre-auth, so every anonymous hit must not spend key quota upstream.
var (
	nvModelsMu    sync.Mutex
	nvModelsCache []string
	nvModelsAt    time.Time
)

// filterExtraFreeModels gates hard-coded extras on the live api: an extra
// lists only when not already seen and (zen off, or the live list is empty,
// or the live list still serves it). extracted for testing: the old test
// re-implemented this inline and could not catch regressions.
func filterExtraFreeModels(extras []string, seenIDs map[string]bool, liveIDs []string, zenOn bool) []string {
	live := make(map[string]bool, len(liveIDs))
	for _, m := range liveIDs {
		live[m] = true
	}
	var out []string
	for _, id := range extras {
		if seenIDs["opencode/"+id] {
			continue
		}
		if !zenOn || (len(liveIDs) > 0 && !live[id]) {
			continue
		}
		out = append(out, id)
	}
	return out
}

func fetchZenModelIDsFrom(base string) []string {
	cl := &http.Client{Timeout: 10 * time.Second}
	resp, err := cl.Get(base + "/models")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var oc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&oc) != nil {
		return nil
	}
	ids := make([]string, 0, len(oc.Data))
	for _, m := range oc.Data {
		ids = append(ids, m.ID)
	}
	return ids
}

// refreshOpencodeModels pulls the free model list from opencode zen and caches it.
// models the previous refresh listed and this one does not are marked dead
// in /status (reason: no longer listed upstream) instead of vanishing: a
// preview id that keeps answering while the listing drops it is exactly the
// case that reads as "why does this model still work / where did it go".
func refreshOpencodeModels() {
	info := &OpencodeInfo{Enabled: true, Base: OpencodeBase}
	if !zenEnabled() {
		info.Enabled = false
		ocInfo.Store(info)
		return
	}
	prev := map[string]bool{}
	if old := ocInfo.Load(); old != nil {
		for _, id := range old.Models {
			prev[id] = true
		}
	}
	for _, m := range fetchZenModelIDs() {
		// big-pickle is free but lacks the -free suffix; everything
		// else free matches -free.
		if !strings.HasSuffix(m, "-free") && m != "big-pickle" {
			continue
		}
		info.Models = append(info.Models, "opencode/"+m)
	}
	sort.Strings(info.Models)
	ocInfo.Store(info)
	log.Printf("  Opencode zen: %d free models", len(info.Models))
	if len(prev) > 0 {
		now := map[string]bool{}
		for _, id := range info.Models {
			now[id] = true
		}
		var gone []string
		for id := range prev {
			if !now[id] {
				gone = append(gone, id)
			}
		}
		sort.Strings(gone)
		for _, id := range gone {
			noteModelRemoved(id, "no longer listed upstream")
		}
		if len(gone) > 0 {
			log.Printf("  Opencode zen: %d model(s) left the listing: %s", len(gone), strings.Join(gone, ", "))
		}
	}
	for _, miss := range missingModelInfo(info.Models) {
		log.Printf("  WARN: %s in /v1/models has no model_info.jsonc entry (falls back to 131072 ctx + derived name)", miss)
	}
}

// missingModelInfo returns listed ids with no catalog entry: warn, don't
// fail. callers pass display ids (handleModels output); lookup strips the
// nvidia/ prefix and [1m] alias the same way serving does.
func missingModelInfo(ids []string) []string {
	var out []string
	for _, id := range ids {
		if lookupModelInfo(id) == nil {
			out = append(out, id)
		}
	}
	return out
}

func initLogging() {
	acclog = log.New(os.Stdout, "", log.LstdFlags)
	// fresh log per boot: stdout is redirected to nimroute.log by the
	// service scripts, so truncate it here instead of appending forever.
	// stat first: never create it in a foreign cwd, only reset our own.
	if _, err := os.Stat("nimroute.log"); err == nil {
		if f, err := os.OpenFile("nimroute.log", os.O_WRONLY|os.O_TRUNC, 0644); err == nil {
			f.Close()
		}
	}
	df := "nimroute-debug.log"
	if e := os.Getenv("DEBUG_FILE"); e != "" {
		df = e
	}
	f, err := os.OpenFile(df, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		acclog.Printf("WARN: cannot open debug log %s: %v", df, err)
		dbglog = log.New(io.Discard, "", 0)
	} else {
		// mirrored to stdout: the skip/refusal reasons are what explain a
		// request that took a substitute or failed, and they belong in the
		// running log instead of a second file nobody opens. body dumps stay
		// debug-only, so this does not grow per-request output.
		dbglog = log.New(io.MultiWriter(f, os.Stdout), "", log.LstdFlags)
	}
	debugMode = os.Getenv("DEBUG") == "1" || os.Getenv("DEBUG") == "true"
}

func short(s string) string {
	if len(s) > 8 {
		return s[len(s)-8:]
	}
	return s
}

// model facts live in model_info.jsonc (modelinfo.go); the old
// config.yml models.params glob table is gone.

// openRouterModel is the openrouter /v1/models entry shape. clients that
// already parse openrouter find every field they expect here; anything extra
// we know lives under Benchmarks or Nim so it cannot collide.
type openRouterModel struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Description   string            `json:"description,omitempty"`
	Created       int64             `json:"created,omitempty"`
	ContextLength int               `json:"context_length"`
	Pricing       map[string]string `json:"pricing"`
	// Price1M is the same price scaled to usd per million tokens: the form
	// a person reads, and the one route ranks by.
	Price1M          map[string]any `json:"price_1m,omitempty"`
	Architecture     map[string]any `json:"architecture"`
	TopProvider      map[string]any `json:"top_provider"`
	PerRequestLimits any            `json:"per_request_limits"`
	CanonicalSlug    string         `json:"canonical_slug,omitempty"`
	HuggingFaceID    string         `json:"hugging_face_id,omitempty"`
	// SupportedParams is the openrouter request-parameter list. empty =
	// no record, not "nothing supported".
	SupportedParams []string       `json:"supported_parameters,omitempty"`
	DefaultParams   map[string]any `json:"default_parameters,omitempty"`
	KnowledgeCutoff string         `json:"knowledge_cutoff,omitempty"`
	ExpirationDate  string         `json:"expiration_date,omitempty"`
	Reasoning       map[string]any `json:"reasoning,omitempty"`
	// Benchmarks is the artificial analysis score block, Benchmarks=nil when
	// aa has no row for this model.
	Benchmarks map[string]any `json:"benchmarks,omitempty"`
	// Nim carries the served-contract facts only this proxy knows.
	Nim map[string]any `json:"nim,omitempty"`
	// OpenRouter is openrouter's own view, relayed verbatim.
	OpenRouter map[string]any `json:"openrouter,omitempty"`
}

// extraFreeModels are free models that don't end in "-free" (just
// big-pickle). only listed when also present on the live /models endpoint.
var extraFreeModels = []string{"big-pickle"}

// unlistedFreeModels are free ids zen still serves but the proxy does not
// list: jev only answers on /systemone, the other two are deprecated
// upstream (upstream_status in model_info.jsonc). requests still route.
var unlistedFreeModels = map[string]bool{
	"jev-1.13-free":                   true,
	"deepseek-v4-flash-free":          true,
	"muse-spark-1.2-contributor-free": true,
}

// zenModelListed reports whether an opencode/<id> request names a model zen
// actually serves: the live catalog, the unlisted ids that still route, or
// big-pickle. false when the id is in none of them. unknown true: the
// catalog is loaded at startup and refreshed on a timer, and a fetch
// failure must not 404 every zen request until it recovers.
func zenModelListed(model string) bool {
	id := strings.TrimPrefix(model, "opencode/")
	if unlistedFreeModels[id] {
		return true
	}
	for _, e := range extraFreeModels {
		if e == id {
			return true
		}
	}
	info := ocInfo.Load()
	if info == nil {
		return true
	}
	return slices.Contains(info.Models, model)
}

// isEmptyMessageList reports whether v is absent, not a list, or a list
// with no entries: the shape every chat upstream rejects on arrival.
func isEmptyMessageList(v any) bool {
	arr, ok := v.([]any)
	if !ok {
		// a string input on /v1/responses is a valid prompt
		if s, isStr := v.(string); isStr {
			return strings.TrimSpace(s) == ""
		}
		return true
	}
	return len(arr) == 0
}

// endpointForModel returns the Zen API endpoint path for a model.
// Muse Spark models use /responses; all others use /chat/completions.
func endpointForModel(model string) string {
	if strings.HasPrefix(model, "muse-spark") {
		return "/responses"
	}
	return "/chat/completions"
}

// normalizeResponsesContent converts chat-style message content into the
// content types /responses accepts (input_text for user, output_text
// for assistant, plain string for system, input_image for images).
// mirrors upstream toOpenaiRequest in console zen provider.
func normalizeResponsesContent(role string, content any) any {
	textType := "input_text"
	if role == "assistant" {
		textType = "output_text"
	}
	switch c := content.(type) {
	case string:
		if c == "" {
			return nil
		}
		if role == "system" {
			return c
		}
		return []any{map[string]any{"type": textType, "text": c}}
	case []any:
		out := make([]any, 0, len(c))
		for _, p := range c {
			pm, ok := p.(map[string]any)
			if !ok {
				if s, ok := p.(string); ok {
					out = append(out, map[string]any{"type": textType, "text": s})
				}
				continue
			}
			pt, _ := pm["type"].(string)
			switch pt {
			case "text", "":
				txt, _ := pm["text"].(string)
				out = append(out, map[string]any{"type": textType, "text": txt})
			case "input_text", "output_text", "input_image", "input_file", "refusal":
				out = append(out, pm)
			case "file":
				// chat/completions file part -> responses input_file. both
				// spellings exist upstream: a plain url string, or
				// {filename, file_data}.
				f, _ := pm["file"].(map[string]any)
				if f == nil {
					continue
				}
				if u, _ := f["file_url"].(string); u != "" {
					out = append(out, map[string]any{"type": "input_file", "file_url": u})
					continue
				}
				if d, _ := f["file_data"].(string); d != "" {
					name, _ := f["filename"].(string)
					out = append(out, map[string]any{
						"type": "input_file", "filename": name, "file_data": d})
				}
			case "image_url":
				switch iu := pm["image_url"].(type) {
				case string:
					out = append(out, map[string]any{"type": "input_image", "image_url": iu})
				case map[string]any:
					if u, _ := iu["url"].(string); u != "" {
						out = append(out, map[string]any{"type": "input_image", "image_url": u})
					}
				}
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}
	return nil
}

// convertToResponses rewrites a chat.completions body into the /responses shape.
func convertToResponses(m map[string]any) {
	if msgs, ok := m["messages"].([]any); ok {
		// match the real client: the leading system prompt rides as the
		// top-level instructions string, not as an input item.
		var sysText string
		if len(msgs) > 0 {
			if sm, ok := msgs[0].(map[string]any); ok && sm["role"] == "system" {
				if c, ok2 := sm["content"].(string); ok2 && c != "" {
					sysText = c
					msgs = msgs[1:]
				}
			}
		}
		if _, exists := m["instructions"]; !exists && sysText != "" {
			m["instructions"] = sysText
		}
		input := make([]any, 0, len(msgs))
		var pending []any
		openCalls := 0
		for _, ms := range msgs {
			mm, ok := ms.(map[string]any)
			if !ok {
				continue
			}
			role, _ := mm["role"].(string)
			switch role {
			case "tool":
				out, _ := mm["content"].(string)
				cid, _ := mm["tool_call_id"].(string)
				input = append(input, map[string]any{
					"type":    "function_call_output",
					"call_id": cid,
					"output":  out,
				})
				openCalls--
				if openCalls <= 0 {
					openCalls = 0
					input = append(input, pending...)
					pending = nil
				}
			case "assistant":
				tc, hasTools := mm["tool_calls"].([]any)
				if hasTools {
					if c := normalizeResponsesContent(role, mm["content"]); c != nil {
						if s, ok := c.(string); !ok || s != "" {
							input = append(input, map[string]any{"role": "assistant", "content": c})
						}
					}
					for _, t := range tc {
						tm, ok := t.(map[string]any)
						if !ok {
							continue
						}
						fn, _ := tm["function"].(map[string]any)
						name, _ := fn["name"].(string)
						if len(name) > 64 {
							name = name[:64]
						}
						args, _ := fn["arguments"].(string)
						input = append(input, map[string]any{
							"type":      "function_call",
							"call_id":   tm["id"],
							"name":      name,
							"arguments": args,
						})
						openCalls++
					}
				} else {
					item := map[string]any{"role": role}
					if c := normalizeResponsesContent(role, mm["content"]); c != nil {
						item["content"] = c
					} else {
						continue // assistant items need content or tool calls
					}
					if openCalls > 0 {
						pending = append(pending, item)
					} else {
						input = append(input, item)
					}
				}
			default:
				item := map[string]any{"role": role}
				if c := normalizeResponsesContent(role, mm["content"]); c != nil {
					item["content"] = c
				} else {
					item["content"] = ""
				}
				if openCalls > 0 {
					pending = append(pending, item)
				} else {
					input = append(input, item)
				}
			}
		}
		m["input"] = input
		delete(m, "messages")
	}
	if mt, ok := m["max_tokens"]; ok {
		m["max_output_tokens"] = mt
		delete(m, "max_tokens")
	}
	if re, ok := m["reasoning_effort"].(string); ok {
		m["reasoning"] = map[string]any{"effort": re}
		delete(m, "reasoning_effort")
	}
	// mirror upstream toOpenaiRequest defaults. responses 400s without them
	// on some models and the free-tier fingerprint expects the real shape.
	// zen's responses validator only accepts tool_choice "auto": the "none"
	// ensureZenTools puts on tool-less bodies would 400, so drop it (the
	// gate stubs stay, auto by default). chat keeps "none".
	if tc, ok := m["tool_choice"]; ok && tc == "none" {
		delete(m, "tool_choice")
	}
	delete(m, "stream_options")
	if _, ok := m["text"]; !ok {
		m["text"] = map[string]any{"verbosity": "low"}
	}
	if _, ok := m["reasoning"]; !ok {
		// default high (or closest) for reasoning models, from the catalog:
		// a request that names an effort keeps its own.
		if mid, _ := m["model"].(string); mid != "" {
			if eff := modelDefaultEffort(mid); eff != "" {
				m["reasoning"] = map[string]any{"effort": eff}
			}
		}
	}
	if tools, ok := m["tools"].([]any); ok {
		cleaned := make([]any, 0, len(tools))
		for _, t := range tools {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			if fn, ok := tm["function"].(map[string]any); ok {
				name, _ := fn["name"].(string)
				if strings.TrimSpace(name) == "" {
					continue // spark rejects empty tool names
				}
				if len(name) > 64 {
					name = name[:64]
				}
				tm["name"] = name
				if desc, ok := fn["description"]; ok {
					tm["description"] = desc
				}
				if params, ok := fn["parameters"]; ok && params != nil {
					tm["parameters"] = params
				}
				delete(tm, "function")
			} else {
				// already in responses shape
				if name, _ := tm["name"].(string); strings.TrimSpace(name) == "" {
					continue
				}
				// an anthropic-shaped tool that skipped the openai rewrite
				// (ensureZenTools runs before this): carry input_schema over,
				// or spark receives a null schema and 400s.
				if _, ok := tm["parameters"]; !ok {
					if is, ok := tm["input_schema"]; ok {
						tm["parameters"] = is
						delete(tm, "input_schema")
					}
				}
			}
			// zen/spark 400 on null/non-object parameters, and on recursive
			// schemas ("Recursive JSON schemas are not currently supported").
			if pm, ok := tm["parameters"].(map[string]any); !ok || pm == nil {
				tm["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
			} else {
				tm["parameters"] = sanitizeZenSchema(pm)
			}
			delete(tm, "input_schema")
			cleaned = append(cleaned, tm)
		}
		if len(cleaned) > 0 {
			m["tools"] = cleaned
		} else {
			delete(m, "tools")
		}
	}
}

// chatFromResponses rewrites a /responses-shaped body (input array) into
// chat.completions shape (messages array). mirror of convertToResponses,
// for clients that speak responses to a chat-only model like space-bunny.
func chatFromResponses(m map[string]any) {
	// string input (plus optional instructions) is the simple responses
	// shape: one user message, instructions as the system message.
	if s, ok := m["input"].(string); ok {
		msgs := []any{}
		if ins, _ := m["instructions"].(string); ins != "" {
			msgs = append(msgs, map[string]any{"role": "system", "content": ins})
		}
		msgs = append(msgs, map[string]any{"role": "user", "content": s})
		m["messages"] = msgs
		delete(m, "input")
		delete(m, "instructions")
		if mt, ok := m["max_output_tokens"]; ok {
			m["max_tokens"] = mt
			delete(m, "max_output_tokens")
		}
		delete(m, "reasoning")
		delete(m, "text")
		return
	}
	in, ok := m["input"].([]any)
	if !ok {
		return
	}
	msgs := make([]any, 0, len(in)+1)
	if ins, _ := m["instructions"].(string); ins != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": ins})
	}
	delete(m, "instructions")
	for _, raw := range in {
		it, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := it["type"].(string)
		switch typ {
		case "function_call_output":
			out, _ := it["output"].(string)
			cid, _ := it["call_id"].(string)
			msgs = append(msgs, map[string]any{
				"role": "tool", "content": out, "tool_call_id": cid,
			})
		case "function_call":
			name, _ := it["name"].(string)
			args, _ := it["arguments"].(string)
			cid, _ := it["call_id"].(string)
			if cid == "" {
				cid = "call_" + randHex(12)
			}
			msgs = append(msgs, map[string]any{
				"role": "assistant", "content": nil,
				"tool_calls": []any{map[string]any{
					"id": cid, "type": "function",
					"function": map[string]any{"name": name, "arguments": args},
				}},
			})
		default:
			role, _ := it["role"].(string)
			if role == "" {
				role = "user"
			}
			if role != "system" && role != "developer" && role != "assistant" && role != "user" {
				continue
			}
			msg := map[string]any{"role": role}
			switch c := it["content"].(type) {
			case string:
				msg["content"] = c
			case []any:
				var parts []any
				for _, p := range c {
					pm, ok := p.(map[string]any)
					if !ok {
						continue
					}
					pt, _ := pm["type"].(string)
					if t, ok := pm["text"].(string); ok && (pt == "input_text" || pt == "output_text" || pt == "text") {
						parts = append(parts, map[string]any{"type": "text", "text": t})
					} else if pt == "input_image" {
						// mirror of normalizeResponsesContent: responses
						// input_image back to a chat image_url part.
						u, _ := pm["image_url"].(string)
						if u == "" {
							continue
						}
						iu := map[string]any{"url": u}
						if d, _ := pm["detail"].(string); d != "" {
							iu["detail"] = d
						}
						parts = append(parts, map[string]any{"type": "image_url", "image_url": iu})
					} else if pt == "input_file" {
						// same, back to a chat file part. a bare file
						// id has no bytes to forward, so it is skipped.
						if u, _ := pm["file_url"].(string); u != "" {
							f := map[string]any{"file_url": u}
							if n, _ := pm["filename"].(string); n != "" {
								f["filename"] = n
							}
							parts = append(parts, map[string]any{"type": "file", "file": f})
						} else if d, _ := pm["file_data"].(string); d != "" {
							f := map[string]any{"file_data": d}
							if n, _ := pm["filename"].(string); n != "" {
								f["filename"] = n
							}
							parts = append(parts, map[string]any{"type": "file", "file": f})
						}
					}
				}
				msg["content"] = parts
			default:
				msg["content"] = ""
			}
			msgs = append(msgs, msg)
		}
	}
	m["messages"] = msgs
	delete(m, "input")
	if mt, ok := m["max_output_tokens"]; ok {
		m["max_tokens"] = mt
		delete(m, "max_output_tokens")
	}
	// chat shape carries these flat, not nested.
	delete(m, "reasoning")
	delete(m, "text")
}

// chatToResponses wraps a chat.completions result body in /responses shape
// (output array with a message item + usage.input/output_tokens). clients
// that posted to /v1/responses get a responses body back even when the model
// only speaks chat upstream.
func chatToResponses(rb []byte) []byte {
	var c struct {
		ID      string `json:"id"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content   any `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rb, &c); err != nil || len(c.Choices) == 0 {
		return rb
	}
	text := ""
	switch t := c.Choices[0].Message.Content.(type) {
	case string:
		text = t
	case []any:
		for _, p := range t {
			if pm, ok := p.(map[string]any); ok {
				if s, _ := pm["text"].(string); s != "" {
					text += s
				}
			}
		}
	}
	output := []any{map[string]any{
		"type": "message", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": text}},
	}}
	for _, tc := range c.Choices[0].Message.ToolCalls {
		output = append(output, map[string]any{
			"type": "function_call", "call_id": tc.ID,
			"name": tc.Function.Name, "arguments": tc.Function.Arguments,
		})
	}
	out := map[string]any{
		"id": c.ID, "object": "response", "created_at": c.Created,
		"model": c.Model, "status": "completed", "output": output,
		"usage": map[string]any{
			"input_tokens": c.Usage.PromptTokens, "output_tokens": c.Usage.CompletionTokens,
			"total_tokens": c.Usage.TotalTokens,
		},
	}
	if b, err := json.Marshal(out); err == nil {
		return b
	}
	return rb
}

// writeResponsesSSE emits a buffered /responses body as a responses SSE
// sequence. used when the client asked for stream=true but the model only
// speaks chat upstream, so the chat result is folded first.
func writeResponsesSSE(w http.ResponseWriter, body []byte) int64 {
	fl, _ := w.(http.Flusher)
	var written int64
	flush := func() {
		if fl != nil {
			fl.Flush()
		}
	}
	var r struct {
		ID     string `json:"id"`
		Model  string `json:"model"`
		Output []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		n, _ := w.Write(body)
		return int64(n)
	}
	if r.ID == "" {
		r.ID = zenID(false)
	}
	emit := func(t string, v any) {
		b, err := json.Marshal(v)
		if err != nil {
			return
		}
		n, _ := w.Write([]byte("event: " + t + "\ndata: " + string(b) + "\n\n"))
		written += int64(n)
		flush()
	}
	inflight := map[string]any{
		"id": r.ID, "object": "response", "status": "in_progress",
		"model": r.Model, "output": []any{},
	}
	emit("response.created", map[string]any{"type": "response.created", "response": inflight})
	emit("response.in_progress", map[string]any{"type": "response.in_progress", "response": inflight})
	emit("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": 0,
		"item": map[string]any{"type": "message", "role": "assistant", "status": "in_progress", "id": r.ID},
	})
	for _, out := range r.Output {
		for i, c := range out.Content {
			if c.Type != "" && c.Type != "output_text" {
				continue
			}
			emit("response.content_part.added", map[string]any{
				"type": "response.content_part.added", "item_id": r.ID, "output_index": 0,
				"content_index": i, "part": map[string]any{"type": "output_text", "text": ""},
			})
			emit("response.output_text.delta", map[string]any{
				"type": "response.output_text.delta", "item_id": r.ID, "output_index": 0,
				"content_index": i, "delta": c.Text,
			})
			emit("response.output_text.done", map[string]any{
				"type": "response.output_text.done", "item_id": r.ID, "output_index": 0,
				"content_index": i, "text": c.Text,
			})
			emit("response.content_part.done", map[string]any{
				"type": "response.content_part.done", "item_id": r.ID, "output_index": 0,
				"content_index": i,
				"part":          map[string]any{"type": "output_text", "text": c.Text},
			})
		}
	}
	emit("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": 0,
		"item": map[string]any{"type": "message", "role": "assistant", "status": "completed", "id": r.ID},
	})
	emit("response.completed", map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id": r.ID, "object": "response", "status": "completed", "model": r.Model,
			"output": r.Output,
			"usage": map[string]any{
				"input_tokens": r.Usage.InputTokens, "output_tokens": r.Usage.OutputTokens,
				"total_tokens": r.Usage.InputTokens + r.Usage.OutputTokens,
			},
		},
	})
	n, _ := w.Write([]byte("data: [DONE]\n\n"))
	written += int64(n)
	flush()
	return written
}

// responsesToChat converts an OpenAI /responses body to /chat/completions shape.
func responsesToChat(rb []byte) []byte {
	var r struct {
		ID      string `json:"id"`
		Created int64  `json:"created_at"`
		Model   string `json:"model"`
		Output  []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rb, &r); err != nil {
		return rb
	}
	var text strings.Builder
	var toolCalls []map[string]any
	for _, item := range r.Output {
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				if c.Type == "output_text" {
					text.WriteString(c.Text)
				}
			}
		case "function_call":
			toolCalls = append(toolCalls, map[string]any{
				"id":   item.CallID,
				"type": "function",
				"function": map[string]any{
					"name":      item.Name,
					"arguments": item.Arguments,
				},
			})
		}
	}
	msg := map[string]any{"role": "assistant", "content": stripWaitSentinel(text.String())}
	finish := "stop"
	if r.Status == "incomplete" {
		finish = "length"
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
		finish = "tool_calls"
	}
	out := map[string]any{
		"id":      r.ID,
		"object":  "chat.completion",
		"created": r.Created,
		"model":   r.Model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
		"usage": map[string]any{
			"prompt_tokens":     r.Usage.InputTokens,
			"completion_tokens": r.Usage.OutputTokens,
			"total_tokens":      r.Usage.TotalTokens,
		},
	}
	if b, err := json.Marshal(out); err == nil {
		return b
	}
	return rb
}

// responsesSSEToJSON folds a buffered /responses SSE body into a single
// /responses-shaped JSON object so responsesToChat can consume it. the wait
// sentinel is proxy-internal and stripped from folded text.
func responsesSSEToJSON(body []byte) []byte {
	body = stripSentinelSSE(body)
	asStr := func(v any) string {
		s, _ := v.(string)
		return s
	}
	var (
		id      string
		created int64
		model   string
		status  string
		output  []map[string]any
		usage   map[string]any
		pos     = map[string]int{}
	)
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var evt struct {
			Type     string         `json:"type"`
			Delta    string         `json:"delta"`
			ItemID   string         `json:"item_id"`
			Item     map[string]any `json:"item"`
			Response struct {
				ID        string           `json:"id"`
				CreatedAt int64            `json:"created_at"`
				Model     string           `json:"model"`
				Status    string           `json:"status"`
				Output    []map[string]any `json:"output"`
				Usage     map[string]any   `json:"usage"`
			} `json:"response"`
		}
		if json.Unmarshal([]byte(data), &evt) != nil {
			continue
		}
		switch evt.Type {
		case "response.created", "response.in_progress":
			if evt.Response.ID != "" {
				id = evt.Response.ID
			}
			if evt.Response.CreatedAt != 0 {
				created = evt.Response.CreatedAt
			}
			if evt.Response.Model != "" {
				model = evt.Response.Model
			}
		case "response.output_item.added":
			if evt.Item != nil {
				pos[asStr(evt.Item["id"])] = len(output)
				output = append(output, evt.Item)
			}
		case "response.output_text.delta":
			if i, ok := pos[evt.ItemID]; ok {
				item := output[i]
				content, _ := item["content"].([]any)
				var part map[string]any
				if len(content) > 0 {
					part, _ = content[len(content)-1].(map[string]any)
				}
				if part == nil || asStr(part["type"]) != "output_text" {
					part = map[string]any{"type": "output_text", "text": ""}
					content = append(content, part)
				}
				part["text"] = asStr(part["text"]) + evt.Delta
				item["content"] = content
			}
		case "response.function_call_arguments.delta":
			if i, ok := pos[evt.ItemID]; ok {
				item := output[i]
				item["arguments"] = asStr(item["arguments"]) + evt.Delta
			}
		case "response.output_item.done":
			if evt.Item != nil {
				if i, ok := pos[asStr(evt.Item["id"])]; ok {
					output[i] = evt.Item
				}
			}
		case "response.completed", "response.incomplete", "response.failed":
			if evt.Response.Usage != nil {
				usage = evt.Response.Usage
			}
			if len(evt.Response.Output) > 0 {
				output = evt.Response.Output
			}
			if evt.Response.ID != "" {
				id = evt.Response.ID
			}
			if evt.Response.Model != "" {
				model = evt.Response.Model
			}
			if evt.Response.Status != "" {
				status = evt.Response.Status
			}
		}
	}
	out := map[string]any{
		"id":         id,
		"object":     "response",
		"created_at": created,
		"model":      model,
		"status":     status,
		"output":     stripFoldedSentinel(output),
		"usage":      usage,
	}
	if b, err := json.Marshal(out); err == nil {
		return b
	}
	return body
}

// stripFoldedSentinel removes the internal wait marker from folded
// /responses text parts. split-across-delta sentinels reassemble during
// the fold, so the raw pre-strip can't catch them.
func stripFoldedSentinel(output []map[string]any) []map[string]any {
	for _, item := range output {
		content, _ := item["content"].([]any)
		for _, c := range content {
			part, _ := c.(map[string]any)
			if part == nil {
				continue
			}
			if t, _ := part["type"].(string); t != "output_text" && t != "text" {
				continue
			}
			if s, _ := part["text"].(string); strings.Contains(s, waitSentinel) {
				part["text"] = strings.ReplaceAll(s, waitSentinel, "")
			}
		}
	}
	return output
}

// oaiToolNames pulls the tool names a chat/completions or /responses request
// declared, so response translators can restore the client's spelling. used by
// the OpenAI-format path; /v1/messages gets its names from the anthropic parse.
func oaiToolNames(body []byte) toolNames {
	var m struct {
		Tools []struct {
			Name     string `json:"name"`
			Function *struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if json.Unmarshal(body, &m) != nil {
		return nil
	}
	var tn toolNames
	for _, t := range m.Tools {
		name := t.Name
		if t.Function != nil && t.Function.Name != "" {
			name = t.Function.Name
		}
		if strings.TrimSpace(name) != "" {
			tn = append(tn, name)
		}
	}
	return tn
}

// streamResponsesToChat converts a Responses API SSE stream to chat/completions SSE.
func streamResponsesToChat(w http.ResponseWriter, body []byte, tnames toolNames) (written int64, promptT, compT int) {
	fl, _ := w.(http.Flusher)

	// proxy-internal marker must never reach the client.
	body = stripSentinelSSE(body)

	var strip sentinelStripper
	var sawToolCall bool
	lines := strings.Split(string(body), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "event: ") || line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			n, _ := w.Write([]byte("data: [DONE]\n\n"))
			written += int64(n)
			if fl != nil {
				fl.Flush()
			}
			return
		}

		var evt struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
			Item  *struct {
				Type      string `json:"type"`
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"item"`
			Response *struct {
				Usage *struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(data), &evt); err != nil {
			continue
		}

		switch evt.Type {
		case "response.output_text.delta":
			if evt.Delta != "" {
				if clean := strip.push(evt.Delta); clean != "" {
					chunk := fmt.Sprintf(`{"choices":[{"index":0,"delta":{"content":%s},"finish_reason":null}],"usage":null}`, jsonStr(clean))
					n, _ := fmt.Fprintf(w, "data: %s\n\n", chunk)
					written += int64(n)
					if fl != nil {
						fl.Flush()
					}
				}
			}
		case "response.output_item.added":
			if evt.Item != nil && evt.Item.Type == "function_call" {
				sawToolCall = true
				tid := evt.Item.CallID
				if tid == "" {
					tid = "toolu_" + randHex(24)
				}
				chunk := fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":%s,"type":"function","function":{"name":%s,"arguments":""}}]},"finish_reason":null}],"usage":null}`, jsonStr(tid), jsonStr(tnames.resolve(evt.Item.Name)))
				n, _ := fmt.Fprintf(w, "data: %s\n\n", chunk)
				written += int64(n)
				if fl != nil {
					fl.Flush()
				}
			}
		case "response.function_call_arguments.delta":
			if evt.Delta != "" {
				chunk := fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":%s}}]},"finish_reason":null}],"usage":null}`, jsonStr(evt.Delta))
				n, _ := fmt.Fprintf(w, "data: %s\n\n", chunk)
				written += int64(n)
				if fl != nil {
					fl.Flush()
				}
			}
		case "response.completed":
			if tail := strip.flush(); tail != "" {
				chunk := fmt.Sprintf(`{"choices":[{"index":0,"delta":{"content":%s},"finish_reason":null}],"usage":null}`, jsonStr(tail))
				n, _ := fmt.Fprintf(w, "data: %s\n\n", chunk)
				written += int64(n)
			}
			// openai chat clients (and every agent harness) require a terminal
			// finish_reason before [DONE]; without it they error "stream ended
			// without finish_reason". a tool-call turn terminates with
			// "tool_calls", matching the non-streaming path.
			finish := "stop"
			if sawToolCall {
				finish = "tool_calls"
			}
			n, _ := w.Write([]byte(fmt.Sprintf("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":%q}],\"usage\":null}\n\n", finish)))
			written += int64(n)
			if evt.Response != nil && evt.Response.Usage != nil {
				promptT = evt.Response.Usage.InputTokens
				compT = evt.Response.Usage.OutputTokens
				totalT := promptT + compT
				chunk := fmt.Sprintf(`{"choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`, promptT, compT, totalT)
				n, _ := fmt.Fprintf(w, "data: %s\n\n", chunk)
				written += int64(n)
			}
			n, _ = w.Write([]byte("data: [DONE]\n\n"))
			written += int64(n)
			if fl != nil {
				fl.Flush()
			}
			return
		}
	}
	// stream ended without response.completed
	if tail := strip.flush(); tail != "" {
		chunk := fmt.Sprintf(`{"choices":[{"index":0,"delta":{"content":%s},"finish_reason":null}],"usage":null}`, jsonStr(tail))
		n, _ := fmt.Fprintf(w, "data: %s\n\n", chunk)
		written += int64(n)
	}
	n, _ := w.Write([]byte("data: [DONE]\n\n"))
	written += int64(n)
	if fl != nil {
		fl.Flush()
	}
	return
}

// streamResponsesLive converts an upstream /responses SSE body to OAI chat
// chunks while reading: each upstream chunk is converted and flushed
// immediately, so client TTFT is zen's first byte, not the full turn. line
// framing survives chunk splits via carry. ctx aborts the read (upstream
// body close cancels the in-flight request, freeing the exit).
func streamResponsesLive(ctx context.Context, w http.ResponseWriter, body io.Reader, tnames toolNames) (written int64, promptT, compT int) {
	fl, _ := w.(http.Flusher)
	flush := func() {
		if fl != nil {
			fl.Flush()
		}
	}
	var strip sentinelStripper
	var sawToolCall bool
	var carry []byte
	buf := make([]byte, 4096)
	// evt mirrors the subset streamResponsesToChat parses; kept local so
	// the live path never blocks on the buffered converter's shape.
	feed := func(blk []byte) {
		lines := strings.Split(string(blk), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "event: ") || line == "" {
				continue
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				n, _ := w.Write([]byte("data: [DONE]\n\n"))
				written += int64(n)
				flush()
				return
			}
			var evt struct {
				Type  string `json:"type"`
				Delta string `json:"delta"`
				Item  *struct {
					Type      string `json:"type"`
					CallID    string `json:"call_id"`
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"item"`
				Response *struct {
					Usage *struct {
						InputTokens  int `json:"input_tokens"`
						OutputTokens int `json:"output_tokens"`
					} `json:"usage"`
				} `json:"response"`
			}
			if err := json.Unmarshal([]byte(data), &evt); err != nil {
				continue
			}
			switch evt.Type {
			case "response.output_text.delta":
				if evt.Delta != "" {
					if clean := strip.push(evt.Delta); clean != "" {
						chunk := fmt.Sprintf(`{"choices":[{"index":0,"delta":{"content":%s},"finish_reason":null}],"usage":null}`, jsonStr(clean))
						n, _ := fmt.Fprintf(w, "data: %s\n\n", chunk)
						written += int64(n)
						flush()
					}
				}
			case "response.output_item.added":
				if evt.Item != nil && evt.Item.Type == "function_call" {
					sawToolCall = true
					tid := evt.Item.CallID
					if tid == "" {
						tid = "toolu_" + randHex(24)
					}
					chunk := fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":%s,"type":"function","function":{"name":%s,"arguments":""}}]},"finish_reason":null}],"usage":null}`, jsonStr(tid), jsonStr(tnames.resolve(evt.Item.Name)))
					n, _ := fmt.Fprintf(w, "data: %s\n\n", chunk)
					written += int64(n)
					flush()
				}
			case "response.function_call_arguments.delta":
				if evt.Delta != "" {
					chunk := fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":%s}}]},"finish_reason":null}],"usage":null}`, jsonStr(evt.Delta))
					n, _ := fmt.Fprintf(w, "data: %s\n\n", chunk)
					written += int64(n)
					flush()
				}
			case "response.completed":
				if tail := strip.flush(); tail != "" {
					chunk := fmt.Sprintf(`{"choices":[{"index":0,"delta":{"content":%s},"finish_reason":null}],"usage":null}`, jsonStr(tail))
					n, _ := fmt.Fprintf(w, "data: %s\n\n", chunk)
					written += int64(n)
				}
				// terminal finish_reason before [DONE]; openai chat clients and
				// every agent harness require it (else "stream ended without
				// finish_reason"). a tool-call turn terminates with "tool_calls",
				// matching the non-streaming path.
				finish := "stop"
				if sawToolCall {
					finish = "tool_calls"
				}
				n, _ := w.Write([]byte(fmt.Sprintf("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":%q}],\"usage\":null}\n\n", finish)))
				written += int64(n)
				if evt.Response != nil && evt.Response.Usage != nil {
					promptT = evt.Response.Usage.InputTokens
					compT = evt.Response.Usage.OutputTokens
					chunk := fmt.Sprintf(`{"choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`, promptT, compT, promptT+compT)
					n, _ := fmt.Fprintf(w, "data: %s\n\n", chunk)
					written += int64(n)
				}
				n, _ = w.Write([]byte("data: [DONE]\n\n"))
				written += int64(n)
				flush()
				return
			}
		}
	}
	for {
		if ctx.Err() != nil {
			return
		}
		n, err := body.Read(buf)
		if n > 0 {
			blk := buf[:n]
			if len(carry) > 0 {
				blk = append(carry, buf[:n]...)
				carry = nil
			}
			// hold back the trailing partial line: only convert complete
			// lines, keep the rest for the next read.
			if i := bytes.LastIndexByte(blk, '\n'); i >= 0 {
				feed(blk[:i+1])
				if i+1 < len(blk) {
					carry = append(carry[:0], blk[i+1:]...)
				}
			} else {
				carry = append(carry[:0], blk...)
				if len(carry) > 65536 {
					// absurdly long line: convert anyway rather than
					// buffering forever.
					feed(carry)
					carry = nil
				}
			}
		}
		if err != nil {
			break
		}
	}
	if len(carry) > 0 {
		feed(append(carry, '\n'))
	}
	if tail := strip.flush(); tail != "" {
		chunk := fmt.Sprintf(`{"choices":[{"index":0,"delta":{"content":%s},"finish_reason":null}],"usage":null}`, jsonStr(tail))
		n, _ := fmt.Fprintf(w, "data: %s\n\n", chunk)
		written += int64(n)
	}
	n, _ := w.Write([]byte("data: [DONE]\n\n"))
	written += int64(n)
	flush()
	return
}

// isTerminalResponsesEvent reports whether a /responses SSE payload ends the
// turn, after which streamResponsesToChat returns and ignores the rest.
func isTerminalResponsesEvent(data string) bool {
	if data == "[DONE]" {
		return true
	}
	var evt struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(data), &evt); err != nil {
		return false
	}
	switch evt.Type {
	case "response.completed", "response.incomplete", "response.failed":
		return true
	}
	return false
}

// mergeResponsesSSE concatenates buffered /responses SSE bodies into one
// stream: terminal events are stripped from every body but the last so the
// merged body plays as a single turn.
func mergeResponsesSSE(bodies [][]byte) []byte {
	var out bytes.Buffer
	for i, b := range bodies {
		last := i == len(bodies)-1
		for _, line := range strings.Split(string(b), "\n") {
			t := strings.TrimSpace(line)
			if !last && strings.HasPrefix(t, "data: ") && isTerminalResponsesEvent(strings.TrimPrefix(t, "data: ")) {
				continue
			}
			out.WriteString(line)
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}

// nudgePostResponses re-POSTs a body to zen on the caller's lane, with the
// same pacing and rotation policy as the main request. best effort: nil on
// any failure. client disconnect cancels the upstream send.
func (p *Pool) nudgePostResponses(r *http.Request, target string, nb []byte, sessionID *string, lane *zenLane) []byte {
	if lane == nil {
		return nil
	}
	var cl *http.Client
	lastProxy := ""
	var dialDeadErr error
	dialDeadProxy := ""
	rotating := true
	// hoisted: the body never changes across retries.
	nbModel := reqModel(nb)
	poolRecoverUntil := time.Time{}
	for zenRetries := 0; zenRetries < zenMaxRetries(); zenRetries++ {
		proxy := ""
		if rotating {
			proxy = laneProxyFor(lane, nbModel)
			if proxy == "" && laneNeedsProxy(lane) {
				nl, p := laneProxyWait(r.Context(), lane, nbModel, &poolRecoverUntil)
				if nl != lane {
					lane = nl
					*sessionID = laneSession(lane)
				}
				proxy = p
			}
			if proxy == "" && laneNeedsProxy(lane) {
				// dry pool: back off instead of hot-spinning the loop.
				if !zenLastRetry(zenRetries, zenMaxRetries()) {
					zenBackoffCtx(r.Context(), zenRetries)
					continue
				}
				return nil
			}
			// rotation handed back the exit whose dial just failed: a
			// second dial of the same corpse cannot succeed, so end the
			// ladder here instead of spending every remaining hop on it.
			if dialDeadErr != nil && proxy == dialDeadProxy {
				acclog.Printf("  opencode dial-dead %s re-picked by lane %s, ending retry ladder", redactProxyUserinfo(proxy), lane.id)
				return nil
			}
			cl = zenClient(proxy)
			lastProxy = proxy
		} else {
			// service overloaded: retry on the same lane proxy + session.
			proxy = lastProxy
			// killswitch: always_proxy must never send direct. lastProxy is
			// "" only if the first attempt went direct, which the flag
			// rules out, so this is belt and braces.
			if proxy == "" && cfg().Zen.AlwaysProxy {
				return nil
			}
			cl = zenClient(proxy)
		}
		exitGate(r.Context(), lane, proxy)
		if r.Context().Err() != nil {
			return nil
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(nb))
		if err != nil {
			return nil
		}
		zenApplyHeaders(req, *sessionID, r.Header, "nudge", "nudge", target, proxy, nbModel)
		for k, v := range r.Header {
			if zenHeaderForwarded(k) {
				req.Header[k] = v
			}
		}
		up, err := cl.Do(req)
		if err != nil {
			if r.Context().Err() != nil {
				return nil // caller gone: nothing to blame on the exit
			}
			if isTLSFailure(err) {
				banTLSExit(proxy)
			} else if noteExitTransportErr(proxy) {
				dropProxy(proxy)
			} else {
				lane = laneFailover(lane)
				*sessionID = laneSession(lane)
			}
			if zenDialDead(err) {
				dialDeadErr, dialDeadProxy = err, proxy
			} else {
				dialDeadErr = nil
			}
			rotating = true
			acclog.Printf("!! opencode zen error (retry %d/%d) nudge country=%s proxy=%s %s: %v", zenRetries, zenRetryLabel(), proxyCountry(proxy), redactProxyUserinfo(proxy), target, err)
			if !zenLastRetry(zenRetries, zenMaxRetries()) {
				zenBackoffCtx(r.Context(), zenRetries)
				continue
			}
			return nil
		}
		if up.StatusCode == http.StatusTooManyRequests || up.StatusCode == 529 {
			up.Body.Close()
			noteExit429(proxy, nbModel)
			lane = laneFailover(lane)
			*sessionID = laneRotate(lane, nbModel)
			rotating = true
			zenBackoffCtx(r.Context(), zenRetries)
			continue
		}
		if up.StatusCode != http.StatusOK {
			eb, _ := io.ReadAll(io.LimitReader(up.Body, 16<<10))
			up.Body.Close()
			if zenServiceOverloaded(eb) {
				// same policy as the main loops: rotate, never re-pin.
				lane = laneHandoff(lane, laneFailover(lane))
				*sessionID = laneRotate(lane, nbModel)
				rotating = true
				if !zenLastRetry(zenRetries, zenMaxRetries()) {
					zenBackoffCtx(r.Context(), zenRetries)
					continue
				}
			}
			if zenGeoBlocked(eb) {
				acclog.Printf("  opencode nudge geo-blocked %d country=%s proxy=%s err=%q",
					up.StatusCode, proxyCountry(proxy), redactProxyUserinfo(proxy), errSnippet(eb, 160))
				dropProxy(proxy)
				rotating = true
				if !zenLastRetry(zenRetries, zenMaxRetries()) {
					zenBackoffCtx(r.Context(), zenRetries)
					continue
				}
			}
			acclog.Printf("!! opencode nudge upstream %d country=%s proxy=%s err=%q",
				up.StatusCode, proxyCountry(proxy), redactProxyUserinfo(proxy), errSnippet(eb, 160))
			return nil
		}
		nb2, _ := io.ReadAll(io.LimitReader(up.Body, upstreamBodyLimit))
		up.Body.Close()
		p.noteZenSuccess(*sessionID, proxy)
		return nb2
	}
	return nil
}

// maybeNudgeResponses applies the nudge_no_tools reprompt (same gates as the
// /v1/messages path) to a buffered /responses SSE body on the OAI path. it
// returns the body to stream, merged with the retry when the nudge fires.
// nudgeEnabled reports whether the no-tools continue is on: master switch
// plus the per-model nudge_no_tools param (default on for spark-shaped
// models when the param is absent).
func nudgeEnabled(model string) bool {
	if !cfg().Nudge.Enabled {
		return false
	}
	return modelNudge(model)
}

func (p *Pool) maybeNudgeResponses(r *http.Request, target string, posted, rb []byte, sessionID *string, clientModel string, lane *zenLane) []byte {
	sid := ""
	if sessionID != nil {
		sid = *sessionID
	}
	post := func(nb []byte) []byte {
		return p.nudgePostResponses(r, target, nb, sessionID, lane)
	}
	if retry := tryNudgeResponses(sid, clientModel, posted, rb, post); retry != nil {
		_, nHasCalls := scanResponsesSSE(retry)
		acclog.Printf("  opencode nudge model=%s calls=%v bytes=%d [oai]", clientModel, nHasCalls, len(retry))
		return mergeResponsesSSE([][]byte{rb, retry})
	}
	return rb
}

func (p *Pool) handleModels(w http.ResponseWriter, r *http.Request) {
	// cached 60s: the endpoint is pre-auth, so anonymous callers must
	// not trigger an authenticated upstream fetch per request.
	nvModelsMu.Lock()
	nvModels, nvFresh := nvModelsCache, time.Since(nvModelsAt) < time.Minute
	nvModelsMu.Unlock()
	if !nvFresh {
		var key *Key
		p.mu.RLock()
		for _, k := range p.keys {
			if time.Now().After(k.CooldownUntil) {
				cp := *k
				key = &cp
				break
			}
		}
		if key == nil && len(p.keys) > 0 {
			cp := *p.keys[0]
			key = &cp
		}
		p.mu.RUnlock()
		if key != nil && nvidiaEnabled() {
			mc := &http.Client{Timeout: 10 * time.Second}
			req, _ := http.NewRequest("GET", nvidiaBase+"/models", nil)
			req.Header.Set("Authorization", "Bearer "+key.Key)
			resp, err := mc.Do(req)
			if err == nil {
				if resp.StatusCode == 200 {
					var nv struct {
						Data []struct {
							ID string `json:"id"`
						} `json:"data"`
					}
					if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&nv) == nil {
						var fresh []string
						for _, m := range nv.Data {
							fresh = append(fresh, m.ID)
						}
						nvModelsMu.Lock()
						nvModelsCache, nvModelsAt = fresh, time.Now()
						nvModelsMu.Unlock()
						nvModels = fresh
					}
				}
				resp.Body.Close()
			}
		}
	}

	var out struct {
		Data []openRouterModel `json:"data"`
	}
	out.Data = []openRouterModel{}
	seen := make(map[string]bool)
	for _, mid := range nvModels {
		if seen[mid] {
			continue
		}
		seen[mid] = true
		out.Data = append(out.Data, infoModel("nvidia/"+mid, mid))
	}
	// disabled or keyless: no live list, fall back to the catalog file
	// (nim-side entries only; zen lists from the live api below).
	if len(out.Data) == 0 && nvidiaEnabled() {
		modelInfoMu.RLock()
		for id := range modelInfo {
			if strings.HasPrefix(id, "opencode/") || strings.HasPrefix(id, "freeepi/") || strings.HasPrefix(id, "antseed/") {
				continue
			}
			pl := strings.ToLower(id)
			if strings.HasSuffix(pl, "-free") || pl == "big-pickle" || strings.HasPrefix(pl, "muse-spark") {
				continue
			}
			disp := id
			if !strings.Contains(disp, "/") {
				disp = "nvidia/" + disp
			} else if !strings.HasPrefix(disp, "nvidia/") {
				disp = "nvidia/" + disp
			}
			out.Data = append(out.Data, infoModel(disp, id))
		}
		modelInfoMu.RUnlock()
	}

	// opencode zen free models (no auth) -> opencode/<id>. served from the
	// startup + 45-minute cache: this endpoint is pre-auth, so a live fetch
	// here would let anonymous callers spawn an upstream request per hit.
	var liveIDs []string
	if zenEnabled() {
		if info := ocInfo.Load(); info != nil {
			for _, id := range info.Models {
				liveIDs = append(liveIDs, strings.TrimPrefix(id, "opencode/"))
			}
		}
	}
	ocSeen := make(map[string]bool)
	for _, m := range liveIDs {
		if (!strings.HasSuffix(m, "-free") && m != "big-pickle") || ocSeen[m] || unlistedFreeModels[m] {
			continue
		}
		ocSeen[m] = true
		out.Data = append(out.Data, infoModel("opencode/"+m, "opencode/"+m))
	}

	// hard-coded extra free models, listed only when the live api still
	// serves them (dead entries 404 upstream and confuse clients).
	seenIDs := make(map[string]bool, len(out.Data))
	for _, e := range out.Data {
		seenIDs[e.ID] = true
	}
	for _, id := range filterExtraFreeModels(extraFreeModels, seenIDs, liveIDs, zenEnabled()) {
		seenIDs["opencode/"+id] = true
		out.Data = append(out.Data, infoModel("opencode/"+id, "opencode/"+id))
	}
	appendFreepiModels(&out, seenIDs)
	appendAntseedModels(&out, seenIDs)
	appendNousModels(&out, seenIDs)
	appendKeyedModels(unoBackend, &out, seenIDs)
	appendKeyedModels(orChatBackend, &out, seenIDs)
	appendKeyedModels(orcaBackend, &out, seenIDs)
	appendRouteModels(p, &out, seenIDs)
	dedupVariants(&out)
	sortModelsByBench(out.Data)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// sortModelsByBench orders the listing best-score-first so a client that
// just walks the array gets the strongest models first. models aa has no row
// for keep their relative order at the tail, and the tiebreak on id makes the
// whole listing deterministic.
func sortModelsByBench(list []openRouterModel) {
	if !benchmarksEnabled() {
		return
	}
	score := func(m openRouterModel) float64 {
		if m.Benchmarks == nil {
			return 0
		}
		v, _ := m.Benchmarks["intelligence_index"].(float64)
		return v
	}
	sort.SliceStable(list, func(i, j int) bool {
		a, b := score(list[i]), score(list[j])
		if (a > 0) != (b > 0) {
			return a > 0
		}
		if a != b {
			return a > b
		}
		return list[i].ID < list[j].ID
	})
}

// appendFreepiModels lists freeepi/<id> for every catalog model across
// accounts. dedupes by id; per-account availability is a status concern.
func appendFreepiModels(out *struct {
	Data []openRouterModel `json:"data"`
}, seenIDs map[string]bool) {
	if !freepiEnabled() {
		return
	}
	seen := map[string]bool{}
	for _, fa := range freepiAccounts() {
		for _, m := range freepiModels(fa) {
			if seen[m] {
				continue
			}
			seen[m] = true
			id := "freeepi/" + m
			if seenIDs[id] {
				continue
			}
			seenIDs[id] = true
			out.Data = append(out.Data, infoModel(id, id))
		}
	}
}

// infoModel builds the /v1/models entry from the catalog file: name,
// description, served context, served max output, and modality all come from
// model_info. freepi entries advertise the gateway's own limits, not the
// upstream model's. unknown ids get the derived name and a default context.
//
// three layers stack on top: the openrouter overlay fills any gap the file
// left, the artificial analysis block scores the model, and the nim block
// states what the proxy itself serves.
func infoModel(displayID, infoID string) openRouterModel {
	cl := modelServedCtx(infoID)
	if cl == 0 {
		cl = 131072
	}
	var maxOut any
	if mo := modelServedMaxOut(infoID); mo > 0 {
		maxOut = mo
	}
	mod := modelModality(infoID)
	m := openRouterModel{
		ID:            displayID,
		Name:          modelName(infoID),
		Description:   modelDesc(infoID),
		ContextLength: cl,
		Pricing:       map[string]string{"prompt": "0", "completion": "0", "request": "0"},
		Architecture: map[string]any{
			"modality":      mod,
			"tokenizer":     "Other",
			"instruct_type": nil,
		},
		TopProvider: map[string]any{
			"context_length":        cl,
			"max_completion_tokens": maxOut,
			"is_moderated":          false,
		},
		PerRequestLimits: nil,
		Benchmarks:       benchBlock(benchFor(infoID)),
		OpenRouter:       orExtras(infoID),
	}
	if e := lookupModelInfo(infoID); e != nil {
		if len(e.Inputs) > 0 {
			m.Architecture["input_modalities"] = e.Inputs
		}
		if len(e.OutMods) > 0 {
			m.Architecture["output_modalities"] = e.OutMods
		}
		m.SupportedParams = e.SupportedParams
		m.KnowledgeCutoff = e.Knowledge
		if e.Released != "" {
			m.Created = releaseUnix(e.Released)
		}
		if e.Reasoning != nil {
			r := map[string]any{"mandatory": *e.Reasoning}
			if len(e.EffortLevels) > 0 {
				r["supported_efforts"] = e.EffortLevels
			}
			if d := modelDefaultEffort(infoID); d != "" {
				r["default_effort"] = d
			}
			m.Reasoning = r
		}
		m.Nim = nimBlock(infoID, e)
	}
	return m
}

// nimBlock is the served contract: what this proxy will actually do with the
// model, as opposed to what the upstream model is capable of.
func nimBlock(infoID string, e *modelInfoEntry) map[string]any {
	n := map[string]any{
		"served_context":    modelServedCtx(infoID),
		"served_max_output": modelServedMaxOut(infoID),
		"source":            e.Source,
	}
	if e.Free {
		n["free"] = true
	}
	if e.Family != "" {
		n["family"] = e.Family
	}
	if e.Released != "" {
		n["released"] = e.Released
	}
	if e.OpenWeights {
		n["open_weights"] = true
	}
	if e.Nudge != nil {
		n["nudge_no_tools"] = *e.Nudge
	}
	if e.Mult != "" {
		n["freepi_usage_multiplier"] = e.Mult
	}
	if e.UpstreamStatus != "" {
		n["upstream_status"] = e.UpstreamStatus
	}
	if e.Params != "" {
		n["params"] = e.Params
	}
	return n
}

// releaseUnix turns a yyyy-mm-dd catalog date into the unix seconds
// openrouter's "created" field carries. 0 when unparseable.
func releaseUnix(date string) int64 {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return 0
	}
	return t.Unix()
}

func modelDisplayName(id string) string {
	parts := strings.Split(id, "/")
	short := parts[len(parts)-1]
	short = strings.ReplaceAll(short, "-", " ")
	short = strings.ReplaceAll(short, "_", " ")
	if len(short) > 0 {
		short = strings.ToUpper(short[:1]) + short[1:]
	}
	return short
}

var camelToSnake = map[string]string{
	"topP": "top_p", "topK": "top_k",
	"maxTokens": "max_tokens", "minP": "min_p",
	"frequencyPenalty":  "frequency_penalty",
	"presencePenalty":   "presence_penalty",
	"repetitionPenalty": "repetition_penalty",
	"stopSequences":     "stop",
	"reasoningEffort":   "reasoning_effort",
	"reasoningBudget":   "reasoning_budget",
}

// injectParams normalizes a client body: camelCase -> snake_case renames
// plus the _nim opt-out strip. no default params are injected anymore:
// sampling knobs come from the client, model facts from model_info.jsonc.
func injectParams(body *[]byte) {
	if len(*body) == 0 {
		return
	}
	var m map[string]any
	if err := json.Unmarshal(*body, &m); err != nil {
		return
	}

	for camel, snake := range camelToSnake {
		if v, ok := m[camel]; ok {
			if _, exists := m[snake]; !exists {
				m[snake] = v
			}
			delete(m, camel)
		}
	}

	delete(m, "_nim")

	out, err := json.Marshal(m)
	if err != nil {
		return
	}
	*body = out
}

// injectHelpfulLine prepends a helpful assistant line to the first system
// message in an OpenAI-format body, or inserts a system message at the start.
func injectHelpfulLine(body *[]byte) {
	if !cfg().Inject.HelpfulLine || len(*body) == 0 {
		return
	}
	var m map[string]any
	if err := json.Unmarshal(*body, &m); err != nil {
		return
	}
	msgs, ok := m["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return
	}
	line := cfg().Inject.HelpfulText
	if line == "" {
		return
	}
	// find first system message
	for i, raw := range msgs {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if msg["role"] != "system" {
			continue
		}
		content, _ := msg["content"].(string)
		content, _ = stripSomeGuardrails(content)
		if strings.Contains(content, line) {
			return
		}
		msg["content"] = line + "\n" + content
		msgs[i] = msg
		m["messages"] = msgs
		b, err := json.Marshal(m)
		if err != nil {
			return
		}
		*body = b
		return
	}
	// no system message found — insert one at start
	sys := map[string]any{"role": "system", "content": line}
	msgs = append([]any{sys}, msgs...)
	m["messages"] = msgs
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	*body = b
}

// b64Head decodes the first quads of a base64 payload: enough bytes to
// sniff an image signature without decoding a megabyte payload.
func b64Head(payload string) []byte {
	n := len(payload)
	if n > 64 {
		n = 64
	}
	n -= n % 4
	hb, err := base64.StdEncoding.DecodeString(payload[:n])
	if err != nil {
		return nil
	}
	return hb
}

// sniffImageMime reports the real image format from the magic bytes: a
// client's data-url label is a claim, and providers hard-fail on a
// label/bytes mismatch (a png declared image/jpeg comes back as "Provider
// returned error" 400, which the client then retries blindly).
func sniffImageMime(b []byte) string {
	if len(b) >= 8 && string(b[:8]) == "\x89PNG\r\n\x1a\n" {
		return "image/png"
	}
	if len(b) >= 3 && b[0] == 0xff && b[1] == 0xd8 && b[2] == 0xff {
		return "image/jpeg"
	}
	if len(b) >= 6 && (string(b[:6]) == "GIF87a" || string(b[:6]) == "GIF89a") {
		return "image/gif"
	}
	if len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP" {
		return "image/webp"
	}
	return ""
}

// fixDataURLMimes rewrites image parts whose declared type disagrees with
// the payload: openai-style image_url (string or {url}) and anthropic-style
// image{source{base64}} blocks both end up here. non-image content and
// unknown signatures pass untouched; only the label moves, never the bytes.
func fixDataURLMimes(body *[]byte) {
	if len(*body) == 0 {
		return
	}
	var m map[string]any
	if err := json.Unmarshal(*body, &m); err != nil {
		return
	}
	msgs, ok := m["messages"].([]any)
	if !ok {
		return
	}
	changed := false
	for _, raw := range msgs {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, praw := range parts {
			pm, ok := praw.(map[string]any)
			if !ok {
				continue
			}
			switch pm["type"] {
			case "image_url", "input_image":
				var cur map[string]any
				var url string
				switch iu := pm["image_url"].(type) {
				case string:
					url = iu
				case map[string]any:
					cur = iu
					url, _ = iu["url"].(string)
				default:
					continue
				}
				rest, ok := strings.CutPrefix(url, "data:image/")
				if !ok {
					continue
				}
				decl, payload, ok := strings.Cut(rest, ";base64,")
				if !ok {
					continue
				}
				mime := sniffImageMime(b64Head(payload))
				if mime == "" || mime == "image/"+decl {
					continue
				}
				newURL := "data:" + mime + ";base64," + payload
				if cur != nil {
					cur["url"] = newURL
					pm["image_url"] = cur
				} else {
					pm["image_url"] = map[string]any{"url": newURL}
				}
				changed = true
			case "image":
				src, ok := pm["source"].(map[string]any)
				if !ok {
					continue
				}
				if t, _ := src["type"].(string); t != "base64" {
					continue
				}
				data, _ := src["data"].(string)
				mime := sniffImageMime(b64Head(data))
				mt, _ := src["media_type"].(string)
				if mime == "" || mt == mime {
					continue
				}
				src["media_type"] = mime
				changed = true
			}
		}
	}
	if !changed {
		return
	}
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	*body = b
}

// injectDiscloseLine prepends the anonymize notice to the system prompt so
// the model treats masks as verbatim placeholders. runs before anonymize
// so any terms inside the notice itself get masked too.
func injectDiscloseLine(body *[]byte) {
	line := discloseLine()
	if line == "" || len(*body) == 0 {
		return
	}
	var m map[string]any
	if err := json.Unmarshal(*body, &m); err != nil {
		return
	}
	// OAI /responses shape: input array with role items.
	if in, ok := m["input"].([]any); ok {
		for i, raw := range in {
			it, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := it["type"].(string)
			role, _ := it["role"].(string)
			if typ != "" && typ != "message" {
				continue
			}
			if role != "" && role != "system" && role != "developer" {
				continue
			}
			if role == "" && typ == "" {
				continue
			}
			if c, ok := it["content"].(string); ok {
				if strings.Contains(c, line) {
					return
				}
				it["content"] = line + "\n" + c
				in[i] = it
			} else if arr, ok := it["content"].([]any); ok {
				it["content"] = append([]any{map[string]any{"type": "input_text", "text": line}}, arr...)
				in[i] = it
			} else {
				continue
			}
			m["input"] = in
			if b, err := json.Marshal(m); err == nil {
				*body = b
			}
			return
		}
	}
	msgs, ok := m["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return
	}
	for i, raw := range msgs {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if r, _ := msg["role"].(string); r != "system" && r != "developer" {
			continue
		}
		if c, ok := msg["content"].(string); ok {
			if strings.Contains(c, line) {
				return
			}
			msg["content"] = line + "\n" + c
			msgs[i] = msg
		} else if arr, ok := msg["content"].([]any); ok {
			msg["content"] = append([]any{map[string]any{"type": "text", "text": line}}, arr...)
			msgs[i] = msg
		} else {
			continue
		}
		m["messages"] = msgs
		if b, err := json.Marshal(m); err == nil {
			*body = b
		}
		return
	}
	// no system message — insert one at the front.
	msgs = append([]any{map[string]any{"role": "system", "content": line}}, msgs...)
	m["messages"] = msgs
	if b, err := json.Marshal(m); err == nil {
		*body = b
	}
}

func reqModel(body []byte) string {
	var j struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &j) == nil && j.Model != "" {
		return j.Model
	}
	return ""
}

// setReqModel rewrites the model field, preserving the rest of the body.
func setReqModel(body []byte, model string) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	m["model"] = model
	return json.Marshal(m)
}

// --- Guardrail removal ---

var guardrailList []string

// guardrails live in config.yml (guardrails.file + guardrails.extra);
// applyConfig loads them, so there is no separate loader anymore.

// byteSpan is a [start,end) byte range in the original text.
type byteSpan struct{ start, end int }

// joinCut splices a removal: trims leading whitespace off the tail and keeps
// a single space at the seam so sentences don't fuse or leave double spaces.
func joinCut(left, right string) string {
	right = strings.TrimLeft(right, " \n\t")
	if strings.HasSuffix(left, " ") && strings.HasPrefix(right, " ") {
		right = strings.TrimPrefix(right, " ")
	}
	return left + right
}

// normGuardrail canonicalizes text for matching: lowercase, collapse runs of
// whitespace, drop leading bullet/markdown and trailing punctuation.
func normGuardrail(s string) string {
	n, _ := normSpans(s)
	return n
}

// normSpans is normGuardrail plus the byte span of every emitted token in s,
// so removals can map a normalized match back to the exact original text.
func normSpans(s string) (string, []byteSpan) {
	var b strings.Builder
	var spans []byteSpan
	space := false
	first := true
	start, end := -1, -1
	flush := func() {
		if start >= 0 && end > start {
			spans = append(spans, byteSpan{start, end})
		}
		start, end = -1, -1
	}
	for i := 0; i < len(s); {
		r, sz := utf8.DecodeRuneInString(s[i:])
		if unicode.IsSpace(r) {
			flush()
			space = true
			i += sz
			continue
		}
		if first {
			// skip leading markdown bullets / quotes
			switch r {
			case '-', '*', '#', '"', '>', '`', '\'', '(':
				if start < 0 {
					start = i // keep skipped leading chars inside the span
				}
				i += sz
				continue
			}
			first = false
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		if start < 0 {
			start = i
		}
		space = false
		// sentence punctuation carries no matching signal; drop it so
		// tokens compare cleanly ("circumstances." vs "circumstances").
		// kept out of the builder entirely (not even as space) to keep
		// exact-substring matching tight on both sides.
		switch r {
		case '.', ',', ';', ':', '!', '?', '"', '\'', '(', ')', '[', ']', '{', '}', '`':
			i += sz
			continue
		}
		if unicode.IsUpper(r) {
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
		end = i + sz
		i += sz
	}
	flush()
	out := strings.TrimRight(b.String(), " \t.,;:!?\"'-")
	return strings.TrimSpace(out), spans
}

// isStopword reports whether w is a stopword that carries no matching signal.
func isStopword(w string) bool {
	switch w {
	case "and", "or", "the", "a", "an", "to", "of", "in", "on", "for", "with",
		"that", "this", "these", "those", "is", "are", "was", "were", "be",
		"do", "does", "did", "not", "no", "you", "your", "it", "its", "we",
		"they", "he", "she", "them", "their", "from", "by", "as", "at", "can",
		"could", "should", "would", "may", "might", "will", "shall", "must",
		"has", "have", "had", "shouldn", "wouldn", "don", "doesn", "didn",
		"mustn", "cannot", "so", "if", "then", "than", "but", "also", "only":
		return true
	}
	return false
}

// guardrailPat is one precomputed guardrail: its normalized form, its
// significant tokens, and the plain token count. norm is a view into the
// dedupe blob and sig a view into the shared token slice, so a pattern owns no
// memory of its own. computing these once at reload keeps the request path
// allocation-free.
type guardrailPat struct {
	norm  string
	sig   []string
	gnLen int
}

// guardrailPats holds the normalized distinct guardrails, longest first.
// rebuilt by dedupeGuardrails on config reload and read on the request path,
// so it is swapped under guardrailMu.
var (
	guardrailMu     sync.RWMutex
	guardrailPats   []guardrailPat
	guardrailAnchor map[string][]int // core token -> indices into guardrailPats
	guardrailAlways []int            // patterns with no usable anchor: always tested
)

func guardrailPrefixSnapshot() []guardrailPat {
	guardrailMu.RLock()
	defer guardrailMu.RUnlock()
	return guardrailPats
}

// guardrailSnapshot returns the pattern list plus the index the request path
// needs: which patterns can start on a given core token, and which carry no
// usable anchor so must always be tested.
func guardrailSnapshot() ([]guardrailPat, map[string][]int, []int) {
	guardrailMu.RLock()
	defer guardrailMu.RUnlock()
	return guardrailPats, guardrailAnchor, guardrailAlways
}

// dedupeGuardrails collapses near-equal entries (same first 60 normalized
// chars) in one pass over guardrailList, so removal is not O(N^2) at 455.
// called on config reload, never per request.
func dedupeGuardrails() {
	seen := map[string]bool{}
	var raws [][2]int
	var blob strings.Builder
	for _, g := range guardrailList {
		gn := normGuardrail(g)
		if gn == "" || len(gn) < 20 {
			continue
		}
		key := gn
		if len(key) > 60 {
			key = key[:60]
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		start := blob.Len()
		blob.WriteString(gn)
		raws = append(raws, [2]int{start, blob.Len()})
	}
	// longest first: the longest pattern wins a shared span.
	sort.Slice(raws, func(i, j int) bool {
		return raws[i][1]-raws[i][0] > raws[j][1]-raws[j][0]
	})
	// every pattern is a view into one blob, its tokens a view into one
	// slice, so a pattern allocates nothing of its own. views survive the
	// later appends: a subslice keeps its own header when the parent grows.
	blobStr := blob.String()
	out := make([]guardrailPat, 0, len(raws))
	var sigAll []string
	for _, r := range raws {
		gn := blobStr[r[0]:r[1]]
		at := len(sigAll)
		sigAll = append(sigAll, sigTokens(gn)...)
		out = append(out, guardrailPat{
			norm:  gn,
			sig:   sigAll[at:],
			gnLen: len(strings.Fields(gn)),
		})
	}
	// index each pattern by the plain token a window could start on, so the
	// request path can rule out most patterns in one pass over the body's
	// tokens instead of rescanning the body once per pattern. keys are edge
	// markdown stripped ("**never**" and "never**" both key "never") because
	// normSpans skips leading markdown only at the start of a string, so a
	// guardrail embedded mid-prompt keeps its own markers.
	anchor := make(map[string][]int)
	var always []int
	for i := range out {
		gn := out[i].norm
		sp := strings.IndexByte(gn, ' ')
		// a single-token pattern can sit inside a longer body token; keep it
		// always-tested rather than trust a token anchor.
		if sp < 0 || len(out[i].sig) < minSigTokens {
			always = append(always, i)
			continue
		}
		keys := map[string]bool{}
		for _, t := range []string{gn[:sp], out[i].sig[0]} {
			if k := coreToken(t); k != "" {
				keys[k] = true
			}
		}
		if len(keys) == 0 {
			always = append(always, i)
			continue
		}
		for k := range keys {
			anchor[k] = append(anchor[k], i)
		}
	}
	guardrailMu.Lock()
	guardrailPats = out
	guardrailAnchor = anchor
	guardrailAlways = always
	guardrailMu.Unlock()
}

// stripSomeGuardrails removes matched guardrail spans from a system text.
// Normalizes and tokenizes once, then matches each guardrail against that
// single normalized view. Exact matches cut the exact byte span; fuzzy
// matches cut from the sentence holding the first matched token through the
// sentence holding the last matched token. Returns the cleaned text and a
// removal count.
func stripSomeGuardrails(text string) (string, int) {
	pats, anchorOn, alwaysOn := guardrailSnapshot()
	if !cfg().Guardrails.Enabled || text == "" || len(pats) == 0 {
		return text, 0
	}
	norm, spans := normSpans(text)
	if norm == "" || len(spans) == 0 {
		return text, 0
	}
	words := strings.Fields(norm)

	// one pass over the body's tokens rules out every pattern whose anchor
	// token is absent, so matching costs one walk of the text rather than one
	// per pattern. each core token is also tried as a suffix, since a match
	// is a plain substring: a guardrail glued to the word before it ("unNever
	// ...") still matches and must stay live.
	live := make([]bool, len(pats))
	mark := func(ws []string) {
		for i := range live {
			live[i] = false
		}
		for _, w := range ws {
			for c := coreToken(w); c != ""; {
				for _, i := range anchorOn[c] {
					live[i] = true
				}
				_, sz := utf8.DecodeRuneInString(c)
				c = c[sz:]
			}
		}
		for _, i := range alwaysOn {
			live[i] = true
		}
	}
	mark(words)

	n := 0
	for i, p := range pats {
		if !live[i] {
			continue
		}
		score, first, last := guardrailScorePat(norm, words, p)
		if first < 0 || last < first || first >= len(spans) || score < guardrailThreshold() {
			continue
		}
		if last >= len(spans) {
			last = len(spans) - 1
		}
		if score == 1.0 {
			// exact: cut from span[first].start through the end of span[last]'s
			// sentence, so trailing punctuation is removed with the match.
			start := spans[first].start
			end := spans[last].end
			for end < len(text) {
				c := text[end]
				if c == '\n' || c == '.' || c == '!' || c == '?' {
					end++
					break
				}
				if c == ' ' || c == ',' || c == ';' || c == '\t' {
					end++
					continue
				}
				break
			}
			text = joinCut(text[:start], text[end:])
		} else {
			// fuzzy: cut from the sentence containing span[first] through the
			// sentence containing span[last]; never further into the text.
			s0 := spans[first].start
			for s0 > 0 {
				c := text[s0-1]
				if c == '\n' || c == '.' || c == '!' || c == '?' {
					break
				}
				s0--
			}
			e0 := spans[last].end
			for e0 < len(text) {
				c := text[e0]
				if c == '\n' || c == '.' || c == '!' || c == '?' {
					e0++
					break
				}
				e0++
			}
			if e0 <= s0 {
				continue
			}
			text = joinCut(text[:s0], text[e0:])
		}
		n++
		norm, spans = normSpans(text)
		if norm == "" {
			break
		}
		words = strings.Fields(norm)
		mark(words)
	}
	return text, n
}

// guardrailThreshold is the minimum score for a fuzzy guardrail hit.
// Exact normalized-substring hits always score 1.0. Bias: skipping a real
// guardrail is benign, removing legit text is not. from guardrails.threshold.
func guardrailThreshold() float64 {
	if t := cfg().Guardrails.Threshold; t > 0 {
		return t
	}
	return 0.6
}

// guardrailWeights splits the fuzzy score between coverage and density:
// score = coverage*(window+density*density). from guardrails config.
func guardrailWeights() (window, density float64) {
	c := cfg().Guardrails
	window, density = c.WindowWeight, c.DensityWeight
	if window <= 0 && density <= 0 {
		return 0.7, 0.3
	}
	return window, density
}

// minSigTokens is the minimum significant-token count for a fuzzy candidate.
// Below this the match is too weak to act on.
const minSigTokens = 5

// maxGap is the maximum token gap allowed between consecutive sig matches.
const maxGap = 12

// sigTokens returns the distinguishing tokens of a normalized guardrail.
func sigTokens(gn string) []string {
	var sig []string
	for _, w := range strings.Fields(gn) {
		if len(w) > 2 && !isStopword(w) {
			sig = append(sig, w)
		}
	}
	return sig
}

// coreToken strips leading and trailing non-alphanumeric runes so a token
// compares independent of surrounding markdown ("**never**" -> "never").
func coreToken(w string) string {
	start := 0
	for start < len(w) {
		r, sz := utf8.DecodeRuneInString(w[start:])
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			break
		}
		start += sz
	}
	end := len(w)
	for end > start {
		r, sz := utf8.DecodeLastRuneInString(w[:end])
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			break
		}
		end -= sz
	}
	return w[start:end]
}

// tokenAt returns the word index containing byte pos in a normalized string
// whose tokens are separated by single spaces.
func tokenAt(norm string, pos int) int {
	if pos <= 0 {
		return 0
	}
	if pos >= len(norm) {
		return strings.Count(norm, " ")
	}
	return strings.Count(norm[:pos], " ")
}

// guardrailScorePat scores one precomputed guardrail against a pre-normalized
// token stream. Returns 1.0 for exact substring, else coverage*density of the
// best window, or 0 when no window qualifies. Also returns the word range
// [first,last] covered by the match in the text (for exact spans as well), so
// removals can cut precisely what matched instead of rounding to a sentence.
// Partial matches count: a paraphrase dropping the tail still scores via
// coverage, but the window constraint, threshold, and tail weight keep
// scattered or coincidental tokens out.
func guardrailScorePat(norm string, words []string, p guardrailPat) (float64, int, int) {
	if ci := strings.Index(norm, p.norm); ci >= 0 {
		first := tokenAt(norm, ci)
		last := tokenAt(norm, ci+len(p.norm)-1)
		if first < 0 {
			first = 0
		}
		if last < first {
			last = first
		}
		return 1.0, first, last
	}
	sig := p.sig
	if len(sig) < minSigTokens {
		return 0, -1, -1 // too few distinguishing tokens: exact only
	}
	gnLen := p.gnLen
	// candidate starts: every occurrence of sig[0]
	best, bestFirst, bestLast := 0.0, -1, -1
	for s := 0; s < len(words); s++ {
		if words[s] != sig[0] {
			continue
		}
		matched, last := 1, s
		pos := s
		for _, n := range sig[1:] {
			f := -1
			for i := pos + 1; i < len(words) && i <= last+maxGap; i++ {
				if words[i] == n {
					f = i
					break
				}
			}
			if f < 0 {
				break
			}
			matched++
			last = f
			pos = f
		}
		if matched < minSigTokens {
			continue
		}
		span := last - s + 1
		if span > 2*gnLen {
			continue // window too wide: tokens scattered, not a paraphrase
		}
		coverage := float64(matched) / float64(len(sig))
		if coverage < guardrailThreshold() {
			continue
		}
		density := float64(matched) / float64(span)
		// tight windows score near coverage; sparse windows are penalized
		ww, dw := guardrailWeights()
		score := coverage * (ww + dw*density)
		// distinctiveness floor: short guardrails have few distinguishing
		// tokens, so a partial match is more likely coincidental. require
		// near-total coverage as the guardrail gets shorter.
		floor := 0.9 - 0.03*float64(len(sig))
		if floor > 0.72 {
			floor = 0.72
		}
		if coverage < floor {
			continue
		}
		if score > best {
			best, bestFirst, bestLast = score, s, last
		}
	}
	return best, bestFirst, bestLast
}

func respTokens(body []byte) (prompt, completion, total int) {
	u := respUsage(body)
	return u.prompt, u.completion, u.total
}

// usageVals holds parsed upstream usage + cost (freepi only).
type usageVals struct {
	prompt, completion, total int
	cost                      float64
}

// respUsage parses usage + cost from an upstream body. freepi completions
// carry cost/cost_details in dollars; other backends omit them (0).
func respUsage(body []byte) (u usageVals) {
	var j struct {
		Usage struct {
			PromptTokens     int     `json:"prompt_tokens"`
			CompletionTokens int     `json:"completion_tokens"`
			TotalTokens      int     `json:"total_tokens"`
			Cost             float64 `json:"cost"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &j) == nil && j.Usage.TotalTokens > 0 {
		u.prompt, u.completion, u.total, u.cost =
			j.Usage.PromptTokens, j.Usage.CompletionTokens, j.Usage.TotalTokens, j.Usage.Cost
	}
	return u
}

func rlHeaders(h http.Header) map[string]string {
	m := make(map[string]string)
	for k := range h {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-ratelimit") || lk == "retry-after" {
			m[k] = h.Get(k)
		}
	}
	return m
}

// writeJSONError writes {"error": msg} with proper escaping: Sprintf
// interpolation breaks on quotes and reflects internals raw.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": msg})
}

// logSafe escapes control bytes so a %0a in the path or model id can't
// forge access-log lines.
func logSafe(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == 0x7f {
			fmt.Fprintf(&b, "%%%02X", c)
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// corsOrigins is the allowlist for browser cross-origin calls. empty =
// deny all browser origins (curl/non-browser clients unaffected).
func corsOrigins() []string {
	if e := os.Getenv("CORS_ORIGINS"); e != "" {
		return strings.Split(e, ",")
	}
	return cfg().Server.CORSOrigins
}

func (p *Pool) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// capture the raw client bytes before any pii masking or body rewrite: a
	// dump is only useful if it shows what the proxy actually received.
	tr, tctx := startTrace(r)
	if tr != nil {
		r = r.WithContext(tctx)
		r.Body = &traceBody{tr: tr, r: r.Body}
		dw := &dbgWriter{w: w, tr: tr}
		w = dw
		defer func() {
			tr.end(dw.status(), dw.buf.Bytes())
			tr.dumpIfErr()
		}()
	}

	origin := r.Header.Get("Origin")
	allowed := false
	for _, o := range corsOrigins() {
		if o = strings.TrimSpace(o); o != "" && o == origin {
			allowed = true
			break
		}
	}
	if allowed {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		w.Header().Set("Access-Control-Allow-Methods", "*")
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	reqID := fmt.Sprintf("req-%d", reqSeq.Add(1))
	if tr != nil {
		tr.setReqID(reqID)
	}
	// /status + /health poll every second from status tail: logging them
	// buries real traffic. errors on those paths still log below.
	if r.URL.Path != "/status" && r.URL.Path != "/health" {
		acclog.Printf("%s --> %s %s from %s (%d bytes)", reqID, r.Method, logSafe(r.URL.Path), r.RemoteAddr, r.ContentLength)
	}

	switch r.URL.Path {
	case "/status", "/health":
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(p.StatusFor(checkAuth(r)))
		return
	case "/v1/models":
		p.handleModels(w, r)
		return
	}

	if !strings.HasPrefix(r.URL.Path, "/v1") {
		http.Error(w, `{"error":"use /v1/... paths"}`, http.StatusBadRequest)
		return
	}

	// model endpoints need a token when auth.tokens is configured.
	// /v1/models and /status stay open.
	if !requireAuth(w, r) {
		return
	}

	// capture runs after auth: it records headers + body, never pre-auth.
	if os.Getenv("ZEN_CAPTURE") != "" && r.Method == http.MethodPost {
		captureRequest(r)
	}

	// Anthropic Messages API — route to anthropic.go handler
	if r.URL.Path == "/v1/messages" || r.URL.Path == "/v1/messages/count_tokens" {
		p.handleAnthropic(w, r, start, reqID)
		return
	}
	// Permission classifier — Claude Code asks the gateway to classify tool-use
	// permission requests; always approve so sessions never stall on a prompt.
	if r.URL.Path == "/v1/messages/classifier" {
		handleClassifier(w, r)
		return
	}

	// reject an oversized body before taking a slot or reading it: a
	// truncated read would only surface as a confusing json error.
	if r.ContentLength > bodyLimit {
		acclog.Printf("!! 413 body-too-large %s %s (%d bytes)", r.Method, logSafe(r.URL.Path), r.ContentLength)
		writeJSONError(w, http.StatusRequestEntityTooLarge, "request body too large (max 10MB)")
		return
	}
	if used, limit := p.semStats(); used >= limit {
		acclog.Printf("%s ... all %d slots busy, queueing", reqID, limit)
	}
	if !p.semAcquire(r.Context()) {
		acclog.Printf("!! %s client disconnected while queueing", reqID)
		http.Error(w, `{"error":"client disconnected"}`, 499)
		return
	}
	p.concurrent.Add(1)
	defer func() {
		p.concurrent.Add(-1)
		p.semRelease()
	}()

	// bound the body read: a client that stalls mid-upload otherwise pins
	// this goroutine before the "-> model=" log, which reads as a hang
	// with no trace past "--> POST". closing the body unblocks the read.
	readCtx, readCancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer readCancel()
	go func() {
		<-readCtx.Done()
		r.Body.Close()
	}()
	body, err := io.ReadAll(io.LimitReader(r.Body, bodyLimit+1))
	if err != nil {
		if readCtx.Err() != nil {
			acclog.Printf("!! 408 body-read-timeout %s %s", r.Method, logSafe(r.URL.Path))
			http.Error(w, `{"error":"request body read timed out"}`, http.StatusRequestTimeout)
			return
		}
		acclog.Printf("!! 400 bad-request-body %s %s: %v", r.Method, logSafe(r.URL.Path), err)
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	r.Body.Close()
	if len(body) > bodyLimit {
		acclog.Printf("!! 413 body-too-large %s %s", r.Method, logSafe(r.URL.Path))
		writeJSONError(w, http.StatusRequestEntityTooLarge, "request body too large (max 10MB)")
		return
	}

	if cfg().Inject.Params {
		injectParams(&body)
	}
	injectHelpfulLine(&body)
	fixDataURLMimes(&body)

	// mask pii before anything upstream sees the body; the wrapped
	// writer restores surrogates on every response path below. the
	// disclose line goes in before masking so terms inside it (if
	// any) are masked like everything else.
	injectDiscloseLine(&body)
	pg := guardForRequest(r)
	body = pg.MaskBody(body)
	if dw := wrapPII(w, pg); dw != nil {
		w = dw
		defer dw.finish()
	}

	isStream := contains(r.Header.Get("Accept"), "text/event-stream")
	if !isStream && len(body) > 0 {
		var j struct {
			Stream bool `json:"stream"`
		}
		if json.Unmarshal(body, &j) == nil {
			isStream = j.Stream
		}
	}
	// usage chunks only arrive when asked: request them on streams so
	// the tee in the nvidia path has tokens to count. harmless upstream.
	if isStream && len(body) > 0 {
		var m map[string]any
		if json.Unmarshal(body, &m) == nil {
			if _, ok := m["stream_options"]; !ok {
				m["stream_options"] = map[string]any{"include_usage": true}
				if b, err := json.Marshal(m); err == nil {
					body = b
				}
			}
		}
	}

	model := reqModel(body)
	// claude code 1m alias: no backend serves a [1m] id, so strip the
	// suffix unconditionally. never gate on the catalog: a missing
	// catalog file must not break routing.
	if base, ok := baseModelID(model); ok {
		if b, err := setReqModel(body, base); err == nil {
			body = b
		}
		model = base
	}
	acclog.Printf("%s -> %s %s model=%s stream=%v bytes=%d", reqID, r.Method, r.URL.Path, model, isStream, len(body))
	if tr := traceFrom(r.Context()); tr != nil {
		tr.setModel(model, backendForModel(model), isStream)
	}

	// a body every upstream would reject must fail here, fast: dispatching
	// it burns a retry ladder and the nvidia fallthrough answers a
	// misleading "no nvidia keys" 503 for what is really a bad request.
	if model == "" {
		// judge the path and method before the body: a request to a
		// path this server does not own, or a GET to a chat endpoint,
		// is a routing error, not a JSON error.
		isChatPath := r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/v1/responses" || r.URL.Path == "/v1/messages"
		if !isChatPath {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"only POST is supported"}`, http.StatusMethodNotAllowed)
			return
		}
		var probe any
		if err := json.Unmarshal(body, &probe); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "model is required")
		return
	}
	// the chat endpoints also require a payload no upstream can fill in:
	// empty messages (or empty input on /v1/responses) must 400 on arrival.
	if r.Method == http.MethodPost && (r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/v1/responses") {
		var top map[string]any
		if json.Unmarshal(body, &top) == nil {
			fld := "messages"
			if r.URL.Path == "/v1/responses" {
				fld = "input"
			}
			v, ok := top[fld]
			if !ok || isEmptyMessageList(v) {
				writeJSONError(w, http.StatusBadRequest, fld+" is required and must be a non-empty list")
				return
			}
		}
	}

	if strings.HasPrefix(model, "route/") {
		if !routeEnabled() {
			acclog.Printf("<- 503 %s %s model=%s (route disabled)", r.Method, r.URL.Path, model)
			http.Error(w, `{"error":"route is disabled; set route.enabled: true in config.yml"}`, http.StatusServiceUnavailable)
			return
		}
		key := strings.TrimPrefix(model, "route/")
		if strings.TrimSpace(key) == "" {
			http.Error(w, `{"error":"route: empty model name"}`, http.StatusBadRequest)
			return
		}
		acclog.Printf("%s routed to route=%s", reqID, key)
		p.handleRoute(w, r, body, model, key, isStream, start, reqID)
		return
	}

	if strings.HasPrefix(model, "wroute/") {
		if !wrouteEnabled() {
			acclog.Printf("<- 503 %s %s model=%s (wroute disabled)", r.Method, r.URL.Path, model)
			http.Error(w, `{"error":"wroute is disabled; set route.enabled: true in config.yml"}`, http.StatusServiceUnavailable)
			return
		}
		key := strings.TrimPrefix(model, "wroute/")
		if strings.TrimSpace(key) == "" {
			http.Error(w, `{"error":"wroute: empty model name"}`, http.StatusBadRequest)
			return
		}
		acclog.Printf("%s routed to wroute=%s", reqID, key)
		p.handleWRoute(w, r, body, model, key, isStream, start, reqID)
		return
	}

	if strings.HasPrefix(model, "opencode/") {
		if !zenEnabled() {
			acclog.Printf("<- 503 %s %s model=%s (zen disabled)", r.Method, r.URL.Path, model)
			http.Error(w, `{"error":"opencode zen is disabled; set zen.enabled: true in config.yml"}`, http.StatusServiceUnavailable)
			return
		}
		// a typo'd id must 404, not get silently substituted by the
		// free-budget failover. only 404 when the catalog is loaded;
		// a catalog fetch failure must not break every zen request.
		if !zenModelListed(model) {
			acclog.Printf("<- 404 %s %s model=%s (not in zen catalog)", r.Method, r.URL.Path, model)
			writeJSONError(w, http.StatusNotFound, "unknown model "+model)
			return
		}
		p.concurrentZen.Add(1)
		defer p.concurrentZen.Add(-1)
		acclog.Printf("%s routed to opencode model=%s", reqID, model)
		p.handleOpenCode(w, r, body, model, isStream, start, reqID)
		return
	}

	if strings.HasPrefix(model, "freeepi/") {
		if !freepiEnabled() {
			acclog.Printf("<- 503 %s %s model=%s (freepi disabled)", r.Method, r.URL.Path, model)
			http.Error(w, `{"error":"freepi is disabled; set freepi.enabled: true with accounts in config.yml"}`, http.StatusServiceUnavailable)
			return
		}
		acclog.Printf("%s routed to freepi model=%s", reqID, model)
		p.handleFreepi(w, r, body, model, isStream, start, reqID)
		return
	}

	if strings.HasPrefix(model, "antseed/") {
		if !antseedEnabled() {
			acclog.Printf("<- 503 %s %s model=%s (antseed disabled)", r.Method, r.URL.Path, model)
			http.Error(w, `{"error":"antseed is disabled; set antseed.enabled: true in config.yml"}`, http.StatusServiceUnavailable)
			return
		}
		acclog.Printf("%s routed to antseed model=%s", reqID, model)
		p.handleAntseed(w, r, body, model, isStream, start, reqID)
		return
	}

	// nous/<model> is the listed form. the portal catalog is openai-shaped,
	// so the rest of this path is the same as any other keyed backend.
	if strings.HasPrefix(model, "nous/") {
		if !nousEnabled() {
			acclog.Printf("<- 503 %s %s model=%s (nous disabled)", r.Method, r.URL.Path, model)
			http.Error(w, `{"error":"nous portal is disabled or has no keys; set nous.enabled: true and add nous_keys to config.yml"}`, http.StatusServiceUnavailable)
			return
		}
		model = strings.TrimPrefix(model, "nous/")
		if b, err := setReqModel(body, model); err == nil {
			body = b
		}
		acclog.Printf("%s routed to nous model=%s", reqID, model)
		p.handleNous(w, r, body, model, isStream, start, reqID)
		return
	}

	// uno/<model> is the listed form; uno takes plain openai chat.
	if strings.HasPrefix(model, "uno/") {
		if !unoEnabled() {
			acclog.Printf("<- 503 %s %s model=%s (uno disabled)", r.Method, r.URL.Path, model)
			http.Error(w, `{"error":"uno is disabled or has no keys; set uno.enabled: true and add uno_keys to config.yml"}`, http.StatusServiceUnavailable)
			return
		}
		model = strings.TrimPrefix(model, "uno/")
		if b, err := setReqModel(body, model); err == nil {
			body = b
		}
		p.keyedChat(w, r, body, model, isStream, start, reqID, unoBackend)
		return
	}

	// orcarouter/<model> is the listed form; ids keep their provider prefix.
	if strings.HasPrefix(model, "orcarouter/") {
		if !orcaEnabled() {
			acclog.Printf("<- 503 %s %s model=%s (orcarouter disabled)", r.Method, r.URL.Path, model)
			http.Error(w, `{"error":"orcarouter is disabled or has no keys; set orcarouter.enabled: true and add orcarouter_keys to config.yml"}`, http.StatusServiceUnavailable)
			return
		}
		model = strings.TrimPrefix(model, "orcarouter/")
		if b, err := setReqModel(body, model); err == nil {
			body = b
		}
		p.keyedChat(w, r, body, model, isStream, start, reqID, orcaBackend)
		return
	}

	// openrouter/<model> is the listed form; one leading prefix is stripped.
	if strings.HasPrefix(model, "openrouter/") {
		if !orChatEnabled() {
			acclog.Printf("<- 503 %s %s model=%s (openrouter chat disabled)", r.Method, r.URL.Path, model)
			http.Error(w, `{"error":"openrouter chat is disabled or has no keys; set openrouter.enabled: true and add openrouter_keys to config.yml"}`, http.StatusServiceUnavailable)
			return
		}
		model = strings.TrimPrefix(model, "openrouter/")
		if b, err := setReqModel(body, model); err == nil {
			body = b
		}
		p.keyedChat(w, r, body, model, isStream, start, reqID, orChatBackend)
		return
	}

	// nvidia/ prefix is the listed form; bare ids stay nvidia for compat.
	// the prefix never reaches nim: it names the backend, not the model.
	if strings.HasPrefix(model, "nvidia/") {
		model = strings.TrimPrefix(model, "nvidia/")
		if b, err := setReqModel(body, model); err == nil {
			body = b
		}
	}

	// disabled or keyless mode serves opencode/* only; nim models need keys.
	if !cfg().Nvidia.Enabled {
		acclog.Printf("<- 503 %s %s model=%s (nvidia disabled)", r.Method, r.URL.Path, model)
		http.Error(w, `{"error":"nvidia backend is disabled; set nvidia.enabled: true in config.yml"}`, http.StatusServiceUnavailable)
		return
	}
	if p.keyCount() == 0 {
		acclog.Printf("<- 503 %s %s model=%s (keyless: no nvidia keys)", r.Method, r.URL.Path, model)
		http.Error(w, `{"error":"no nvidia keys configured; use an opencode/<model> free model or add nvidia_keys to config.yml"}`, http.StatusServiceUnavailable)
		return
	}
	p.handleNvidia(w, r, body, model, isStream, start, reqID)
}

// handleNvidia forwards to nim with key rotation. split from ServeHTTP so
// the route dispatcher can try it as one candidate among backends.
func (p *Pool) handleNvidia(w http.ResponseWriter, r *http.Request, body []byte, model string, isStream bool, start time.Time, reqID string) {
	if debugMode {
		// headers allowlisted: full r.Header carries Authorization.
		safe := http.Header{}
		for _, k := range []string{"Content-Type", "X-Session-Id", "X-Opencode-Client", "User-Agent"} {
			if v := r.Header.Get(k); v != "" {
				safe.Set(k, v)
			}
		}
		dbglog.Printf("=== REQUEST %s %s ===\nHeaders: %+v\nBody: %s", r.Method, logSafe(r.URL.String()), safe, string(body))
	}

	target := nvidiaBase + strings.TrimPrefix(r.URL.Path, "/v1")
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	acclog.Printf("%s routed to nvidia target=%s model=%s", reqID, target, model)
	p.concurrentNV.Add(1)
	defer p.concurrentNV.Add(-1)

	fwd := http.Header{}
	for k, v := range r.Header {
		lk := strings.ToLower(k)
		// auth + internal headers never reach nim: x-api-key is the
		// proxy credential (audit: leaked to upstream pre-fix),
		// x-session-id selects pii/lane state and would link the
		// client across requests for the upstream.
		if lk == "authorization" || lk == "host" || lk == "origin" || lk == "cookie" ||
			lk == "x-api-key" || lk == "x-session-id" {
			continue
		}
		fwd[k] = v
	}
	if r.Method == http.MethodPost && fwd.Get("Content-Type") == "" {
		fwd.Set("Content-Type", "application/json")
	}

	exclude := make(map[string]bool)
	cl := dbgClient(&http.Client{Timeout: 300 * time.Second}, "nvidia")
	// same rule as the zen path: no total deadline on a streamed body.
	if isStream {
		cl.Timeout = 0
	}
	var lastResp *http.Response
	var used *Key
	var retries int
	var rateLimited bool
	sticky := p.stickyKey(model)
	keyCount := p.keyCount()

	for attempt := 0; attempt < max(keyCount*2+2, 4); attempt++ {
		key := p.PickSticky(exclude, model, sticky)
		if key == nil {
			if attempt < 3 {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(200 * time.Millisecond):
				}
				continue
			}
			dbglog.Printf("no keys available at attempt %d (excluded=%d, model=%s)", attempt, len(exclude), model)
			break
		}
		used = key

		req, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
		if err != nil {
			acclog.Printf("!! 500 internal NewRequest-failed %s %s: %v", r.Method, target, err)
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		req.Header.Set("Authorization", "Bearer "+key.Key)
		req.Header.Set("Host", "integrate.api.nvidia.com")
		for k, v := range fwd {
			req.Header[k] = v
		}
		// client-claimed forwarding headers never reach upstream: the
		// proxy sets its own value after the copy loop above.
		req.Header.Set("X-Forwarded-For", r.RemoteAddr)
		req.Header.Del("X-Forwarded-Host")

		resp, err := cl.Do(req)
		if err != nil {
			acclog.Printf("!! 502 upstream-error [%s] %s %s: %v", key.Name, r.Method, target, err)
			writeJSONError(w, http.StatusBadGateway, fmt.Sprintf("upstream: %s", err))
			return
		}

		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == 529 {
			rateLimited = true
			p.rateLimit(key)
			p.locks.lock(key.Name, model, modelLockout)
			resp.Body.Close()
			rh := rlHeaders(resp.Header)
			// clamp before shifting: see rateLimit — unbounded shifts overflow
			// the duration negative, disabling backoff entirely.
			shift := key.Consec429 - 1
			if shift > 8 {
				shift = 8
			}
			cd := cooldown429 << shift
			if cd > maxBackoff {
				cd = maxBackoff
			}
			acclog.Printf("!! %d [%s] model=%s key-backoff=%v model-lockout=%v headers=%v", resp.StatusCode, key.Name, model, cd, modelLockout, rh)
			exclude[key.Name] = true
			if attempt > 0 {
				retries++
			}
			continue
		}

		// a streamed 200 hands the body to the client live. buffering it
		// costs the caller the whole generation before its first byte, and
		// the usage tail is teed off the passthrough anyway, so nothing
		// downstream needs the bytes here.
		streamedOK := isStream && resp.StatusCode == http.StatusOK
		var rb []byte
		nextBody := resp.Body
		if !streamedOK {
			rb, _ = io.ReadAll(io.LimitReader(resp.Body, upstreamBodyLimit))
			resp.Body.Close()
			nextBody = io.NopCloser(bytes.NewReader(rb))
		}

		bodyStr := string(rb)
		if resp.StatusCode == http.StatusForbidden || (resp.StatusCode != http.StatusOK && strings.Contains(bodyStr, "ResourceExhausted")) {
			p.locks.lock(key.Name, model, burstCooldown)
			acclog.Printf("!! 403/ResourceExhausted [%s] model=%s burst-backoff=%v", key.Name, model, burstCooldown)
			exclude[key.Name] = true
			if attempt > 0 {
				retries++
			}
			continue
		}

		if attempt > 0 {
			retries++
		}
		if resp.StatusCode == http.StatusOK {
			p.noteKey(model, key.Name)
		}
		lastResp = &http.Response{
			StatusCode: resp.StatusCode,
			Header:     resp.Header.Clone(),
			Body:       nextBody,
		}
		break
	}

	elapsed := time.Since(start)

	if lastResp == nil {
		record := UsageRecord{
			Ts:          time.Now().UTC().Format(time.RFC3339Nano),
			Model:       model,
			Method:      r.Method,
			Path:        r.URL.Path,
			StatusCode:  503,
			DurationMs:  elapsed.Milliseconds(),
			Stream:      isStream,
			Error:       "all keys exhausted or on cooldown",
			RateLimited: rateLimited,
		}
		if used != nil {
			record.KeyName = used.Name
		}
		logUsage(record)
		acclog.Printf("<- 503 %s %s %v (all keys exhausted: excluded=%d, rate_limited=%v, model=%s)", r.Method, r.URL.Path, elapsed.Round(time.Millisecond), len(exclude), rateLimited, model)
		http.Error(w, `{"error":"all keys exhausted or on cooldown"}`, http.StatusServiceUnavailable)
		return
	}

	var prompT, compT, totalT int
	var recErr string
	streamed := isStream && lastResp.StatusCode == http.StatusOK
	if lastResp.StatusCode != http.StatusOK {
		rb2, _ := io.ReadAll(io.LimitReader(lastResp.Body, upstreamBodyLimit))
		lastResp.Body.Close()
		lastResp.Body = io.NopCloser(bytes.NewReader(rb2))
		recErr = strings.TrimSpace(string(rb2))
		if len(recErr) > 200 {
			recErr = recErr[:200]
		}
		acclog.Printf("!! upstream %d [%s] model=%s err=%q", lastResp.StatusCode, used.Name, model, recErr)
	} else if streamed {
		// usage is teed off the passthrough below, so the body is never read
		// here: a second ReadAll would re-buffer what streams.
	} else {
		bodyCopy, _ := io.ReadAll(io.LimitReader(lastResp.Body, upstreamBodyLimit))
		lastResp.Body.Close()
		lastResp.Body = io.NopCloser(bytes.NewReader(bodyCopy))
		prompT, compT, totalT = respTokens(bodyCopy)
	}

	respHeaders := rlHeaders(lastResp.Header)
	record := UsageRecord{
		Ts:           time.Now().UTC().Format(time.RFC3339Nano),
		Model:        model,
		KeyName:      used.Name,
		Method:       r.Method,
		Path:         r.URL.Path,
		StatusCode:   lastResp.StatusCode,
		DurationMs:   elapsed.Milliseconds(),
		Stream:       isStream,
		RetryAttempt: retries,
		RateLimited:  rateLimited,
		Error:        recErr,
		ContentBytes: int64(len(body)),
		Headers:      respHeaders,
	}
	if !streamed {
		record.PromptTokens = prompT
		record.CompletionTokens = compT
		record.TotalTokens = totalT
		logUsage(record)
	}

	// allowlisted response headers only: upstream Content-Length /
	// Encoding / Cookies are wrong after body transforms and must not
	// reach the client.
	for k, v := range lastResp.Header {
		lk := strings.ToLower(k)
		if lk == "content-type" || strings.HasPrefix(lk, "x-ratelimit") || lk == "retry-after" {
			w.Header()[k] = v
		}
	}
	w.WriteHeader(lastResp.StatusCode)

	var written int64
	var respBodyBuf *bytes.Buffer
	if debugMode && isStream && lastResp.StatusCode == http.StatusOK {
		respBodyBuf = &bytes.Buffer{}
	}

	if isStream && lastResp.StatusCode == http.StatusOK {
		if _, ok := w.(http.Flusher); ok {
			written, prompT, compT = copyZenChatStream(r.Context(), w, lastResp, respBodyBuf, prompT, compT)
		} else {
			written, _ = io.Copy(w, lastResp.Body)
		}
	} else {
		written, _ = io.Copy(w, lastResp.Body)
	}
	lastResp.Body.Close()
	if streamed {
		record.DurationMs = time.Since(start).Milliseconds()
		record.PromptTokens = prompT
		record.CompletionTokens = compT
		record.TotalTokens = prompT + compT
		logUsage(record)
	}
	if lastResp.StatusCode == http.StatusOK {
		p.Postpone(used, 1500*time.Millisecond)
		p.Clear429(used)
	}

	sk := ""
	if used != nil {
		sk = fmt.Sprintf(" [%s]", used.Name)
	}
	acclog.Printf("<- %d %s %s %v %d bytes%s model=%s %s", lastResp.StatusCode, r.Method, r.URL.Path, elapsed.Round(time.Millisecond), written, sk, model, reqID)

	if debugMode {
		respStr := ""
		if respBodyBuf != nil {
			respStr = respBodyBuf.String()
		}
		dbglog.Printf("=== RESPONSE %s ===\nStatus: %d Duration: %v Key: %s\nBody: %s", r.URL.Path, lastResp.StatusCode, time.Since(start), used.Name, respStr)
	}
}

// handleOpenCode routes opencode/<model> requests to the opencode zen API
// (OpenAI-compatible, no auth for -free models).
// Muse Spark models use /responses endpoint; all others use
// /chat/completions.
func (p *Pool) handleOpenCode(w http.ResponseWriter, r *http.Request, body []byte, model string, isStream bool, start time.Time, reqID string) {
	realModel := strings.TrimPrefix(model, "opencode/")
	// shape follows the client path, not the model: a responses-shaped body
	// to a chat-only model (or vice versa) is converted so zen sees the
	// shape its endpoint accepts and the client gets its own shape back.
	wantResponses := strings.HasPrefix(r.URL.Path, "/v1/responses")
	if strings.HasPrefix(r.URL.Path, "/v1/chat/completions") {
		wantResponses = false
	}
	tnames := oaiToolNames(body)
	// lane identity must be read before stripCacheFields drops the field.
	lane := laneFor(r.Header.Get("x-session-id"), body)
	sessionID := laneSession(lane)
	noteLaneUse(true) // capacity tracking for the low-load lane race
	defer noteLaneUse(false)
	// track the final lane, including failover handoffs.
	laneClaim(lane)
	defer func() { laneDone(lane) }()
	var m map[string]any
	if json.Unmarshal(body, &m) == nil {
		m["model"] = realModel
		stripCacheFields(m)
		ensureZenGate(m)
		if !isStream {
			// zen 403s stream=false; stream upstream and fold it back below.
			m["stream"] = true
			m["stream_options"] = map[string]any{"include_usage": true}
		}
		// upstream shape follows the model, not the client path: a
		// responses-shaped body to a chat model is rewritten to chat
		// (and vice versa), so zen always sees its native shape.
		if endpointForModel(realModel) == "/responses" {
			convertToResponses(m)
		} else {
			chatFromResponses(m)
			// chatFromResponses rebuilds messages from input/instructions,
			// so a responses-shaped body drops the system message the gate
			// needs: re-apply it on the converted body. idempotent, and a
			// no-op for bodies that already carried a valid block.
			ensureZenEnv(m)
			sanitizeZenChatMessages(m)
		}
		if b, err := json.Marshal(m); err == nil {
			body = b
		}
	}

	// wrapBack reshapes a chat result into responses shape when the client
	// posted to /v1/responses but the model only speaks chat upstream.
	wrapBack := wantResponses && endpointForModel(realModel) != "/responses"
	endpoint := endpointForModel(realModel)
	target := OpencodeBase + endpoint
	if !strings.HasPrefix(r.URL.Path, "/v1/chat/completions") && !strings.HasPrefix(r.URL.Path, "/v1/responses") {
		target += strings.TrimPrefix(r.URL.Path, "/v1")
	}
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	acclog.Printf("%s routed to opencode model=%s endpoint=%s", reqID, realModel, endpoint)
	if zenDebugDir() != "" {
		zenDebugCapture("in", map[string]any{"side": "chat", "req_id": reqID, "url": r.URL.String(), "headers": r.Header, "model": realModel, "stream": isStream})
	}

	cl := &http.Client{Timeout: 300 * time.Second}
	// a streamed turn can outlast five minutes: the cap is for buffered calls.
	if isStream {
		cl.Timeout = 0
	}
	var resp *http.Response
	var prompT, compT, totalT int
	var recErr string
	var usedProxy string
	var dialDeadErr error
	dialDeadProxy := ""
	// burned = exits this request already drew a 429 from: rotation must not
	// hand one back for a second send (guard in the ladder below).
	burned := map[string]bool{}
	rotating := true
	// attempts is the ladder bound: a hop that finds the pool written off buys
	// extra hops once a refill clears the marks (ladderExtend), so it grows.
	attempts := zenMaxRetries()
	// poolRecoverUntil is this request's pool-recovery budget: it starts on the
	// first wait and every later wait (dry pick, spent ladder) draws on it.
	poolRecoverUntil := time.Time{}
	for zenRetries := 0; zenRetries < attempts; zenRetries++ {
		// pace only: lanes never cool, a limit rotates the exit instead.
		// maybeRefreshStalePool trips only when this lane has been limited
		// 15s+ with no global success (stale pool/sessions).
		if zenRetries > 0 && maybeRefreshStalePool(lane) {
			sessionID = laneSession(lane)
		}
		proxy := ""
		if zenRetries == 0 {
			proxy = laneProxyFor(lane, realModel)
			if proxy == "" && laneNeedsProxy(lane) {
				// this lane holds no exit: wait up to 30s for one to be drawable
				// instead of going direct or failing immediately.
				nl, p := laneProxyWait(r.Context(), lane, realModel, &poolRecoverUntil)
				if nl != lane {
					lane = laneHandoff(lane, nl)
					sessionID = laneSession(lane)
				}
				if p == "" {
					if !time.Now().Before(recoverDeadline(&poolRecoverUntil)) {
						writeJSONError(w, http.StatusServiceUnavailable, proxyUnavailableMsg(model))
						return
					}
					continue
				}
				proxy = p
			}
			cl = zenClient(proxy)
			usedProxy = proxy
		} else if rotating {
			proxy = laneProxyFor(lane, realModel)
			if proxy == "" && laneNeedsProxy(lane) {
				nl, p := laneProxyWait(r.Context(), lane, realModel, &poolRecoverUntil)
				if nl != lane {
					lane = laneHandoff(lane, nl)
					sessionID = laneSession(lane)
				}
				proxy = p
			}
			if proxy == "" && laneNeedsProxy(lane) {
				if !time.Now().Before(recoverDeadline(&poolRecoverUntil)) {
					writeJSONError(w, http.StatusServiceUnavailable, proxyUnavailableMsg(model))
					return
				}
				continue
			}
			// rotation handed back an exit this request already drew a 429
			// from: the re-send is a guaranteed cap answer, not a retry
			// (measured: a 2-exit pool burned 13 of 15 hops this way, ~2s
			// apiece). end the ladder with the same honest 502.
			if proxy != "" && burned[proxy] {
				acclog.Printf("  opencode re-picked %s after its own 429 by lane %s, ending retry ladder", redactProxyUserinfo(proxy), lane.id)
				writeJSONError(w, http.StatusServiceUnavailable, proxyUnavailableMsg(model))
				return
			}
			// rotation handed back the exit whose dial just failed: a
			// second dial of the same corpse cannot succeed, so end the
			// ladder here with the same honest 502 the exhausted ladder
			// hands back.
			if dialDeadErr != nil && proxy == dialDeadProxy {
				acclog.Printf("  opencode dial-dead %s re-picked by lane %s, ending retry ladder: %v", redactProxyUserinfo(proxy), lane.id, dialDeadErr)
				writeJSONError(w, http.StatusServiceUnavailable, proxyUnavailableMsg(model))
				return
			}
			cl = zenClient(proxy)
			usedProxy = proxy
		} else {
			// service overloaded: retry on the same lane proxy + session.
			proxy = usedProxy
			// killswitch, same as the anthropic path: never direct.
			if proxy == "" && cfg().Zen.AlwaysProxy {
				acclog.Printf("!! opencode no proxy under always_proxy model=%s, failing instead of direct", model)
				writeJSONError(w, http.StatusServiceUnavailable, proxyUnavailableMsg(model))
				return
			}
			cl = zenClient(proxy)
		}
		// enforce 1.5s minimum gap per exit and lane on every send.
		exitGate(r.Context(), lane, proxy)
		if r.Context().Err() != nil {
			acclog.Printf("!! opencode client disconnected model=%s (paced)", model)
			http.Error(w, `{"error":"client disconnected"}`, 499)
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if os.Getenv("ZEN_DUMP") != "" && zenRetries == 0 {
			dp := fmt.Sprintf("/tmp/zenoai_%d.json", time.Now().UnixNano())
			os.WriteFile(dp, body, 0o600)
			acclog.Printf("ZEN_DUMP oai request %d bytes -> %s", len(body), dp)
		}
		zenApplyHeaders(req, sessionID, r.Header, reqID, zenRetries, target, usedProxy, realModel)
		if zenRetries > 0 {
			acclog.Printf("  opencode retry %d/%d session=%s proxy=%s lane=%s", zenRetries, zenRetryLabel(), sessionID, redactProxyUserinfo(proxy), lane.id)
		}
		for k, v := range r.Header {
			if zenHeaderForwarded(k) {
				req.Header[k] = v
			}
		}

		// hedged send: normally a second exit starts only if the first
		// gives no first byte in ~2s. when the pool has been idle and
		// lane_race_enabled is set, hedgeDelay collapses that to zero so
		// both lanes race and the first byte wins. first-byte latency over
		// 6s earns the exit a slow strike (2 in 1h drops it); every sample
		// feeds the live rtt weight so fast exits win more.
		hedgeAfter := hedgeDelay()
		hedgeFn := func(hctx context.Context) (*http.Response, error) {
			hl := laneFailover(lane)
			hp := laneProxyFor(hl, realModel)
			if hp == "" || hp == proxy {
				return nil, fmt.Errorf("no distinct zen hedge exit")
			}
			exitGate(hctx, hl, hp)
			if hctx.Err() != nil {
				return nil, hctx.Err()
			}
			hcl := zenClient(hp)
			hreq, herr := http.NewRequestWithContext(hctx, r.Method, target, bytes.NewReader(body))
			if herr != nil {
				return nil, herr
			}
			zenApplyHeaders(hreq, laneSession(hl), r.Header, reqID, "hedge", target, hp, realModel)
			for k, v := range r.Header {
				if zenHeaderForwarded(k) {
					hreq.Header[k] = v
				}
			}
			up, uerr := hcl.Do(hreq)
			if uerr == nil {
				acclog.Printf("  opencode hedge won on %s lane=%s", redactProxyUserinfo(hp), hl.id)
				// transport success is not a served request: only a 200
				// stamps health, else a 429 here arms the 12h long-burn on
				// an exit that never produced a token.
				if up.StatusCode == http.StatusOK {
					noteExitOK(hp, realModel)
				} else if up.StatusCode == http.StatusTooManyRequests {
					noteExit429(hp, realModel)
				}
			} else if hctx.Err() == nil {
				// own proxy label: primary-side vs hedge-side
				// mixups are answerable next time.
				acclog.Printf("  opencode hedge error model=%s country=%s proxy=%s lane=%s: %v",
					model, proxyCountry(hp), redactProxyUserinfo(hp), hl.id, uerr)
			}
			return up, uerr
		}
		var firstByteAt time.Time
		traced := req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
			GotFirstResponseByte: func() { firstByteAt = time.Now() },
		}))
		sendStart := time.Now()
		primaryFn := func(pctx context.Context) (*http.Response, error) {
			return cl.Do(traced.WithContext(pctx))
		}
		resp, err, _ = hedgedDo(r.Context(), primaryFn, hedgeFn, hedgeAfter)
		if err == nil && !firstByteAt.IsZero() {
			fb := firstByteAt.Sub(sendStart)
			noteExitRTT(proxy, float64(fb.Milliseconds()))
			if fb > 15*time.Second {
				noteExitSlow(proxy)
			}
		}
		if resp != nil {
			zenDebugCapture("resp", map[string]any{"req_id": reqID, "status": resp.StatusCode, "headers": resp.Header, "attempt": zenRetries, "model": realModel})
		}
		if err != nil {
			if r.Context().Err() != nil {
				// the caller went away (cancel, timeout): the exit never
				// finished a turn, so it is not blamed, marked, or dropped.
				return
			}
			if isTLSFailure(err) {
				// mitm exit: ban it pool-wide and evacuate every pinned
				// lane, not just this one.
				banTLSExit(proxy)
			} else if noteExitTransportErr(proxy) {
				// same stale-pin trap as the anthropic path: a dropped
				// exit still pinned here must not eat every retry.
				if laneReproxy(lane, realModel) == proxy {
					lane = laneHandoff(lane, laneFailover(lane))
					sessionID = laneRotate(lane, realModel)
				}
			} else {
				lane = laneHandoff(lane, laneFailover(lane))
				sessionID = laneSession(lane)
			}
			if zenDialDead(err) {
				dialDeadErr, dialDeadProxy = err, proxy
			} else {
				dialDeadErr = nil
			}
			rotating = true
			logZenNetErr(zenRetries, model, proxy, target, err)
			maybeRefreshNetErr()
			if !zenLastRetry(zenRetries, attempts) {
				zenBackoffCtx(r.Context(), zenRetries)
				continue
			}
			// a refused exit dial is pool state, not an upstream answer: the
			// other ladders end it as proxy capacity, and the caller's move is
			// to retry against us instead of reading a socks error.
			if dialDeadErr != nil {
				writeJSONError(w, http.StatusServiceUnavailable, proxyUnavailableMsg(model))
				return
			}
			writeJSONError(w, http.StatusBadGateway, fmt.Sprintf("upstream: %s", err))
			return
		}
		if os.Getenv("ZEN_DUMP") != "" {
			acclog.Printf("ZEN_DUMPhdr <-%d %v", resp.StatusCode, req.Header)
		}

		// gaveUp marks a synthesized response: the ladder is over and the
		// remaining checks must not re-treat it as an upstream failure.
		gaveUp := false
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == 529 {
			st := resp.StatusCode
			eb, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
			resp.Body.Close()
			// a free-tier 429 answers on whichever exit took the
			// request, so it is that exit's mark like any other 429:
			// the ladder rotates to a fresh exit and session and tries
			// again. treating it as a pool-wide cap is what parked
			// every exit behind one answer and stopped rotation from
			// ever finding the ones with room left.
			freeCap := zenFreeUsageCap(eb)
			acclog.Printf("  opencode rate-limited %d (retry %d/%d) freetier=%t model=%s country=%s proxy=%s lane=%s, rotating exit+session",
				st, zenRetries, zenRetryLabel(), freeCap, model, proxyCountry(proxy), redactProxyUserinfo(proxy), lane.id)
			if proxy == "" {
				laneEscalate(lane, realModel) // direct got limited: give the lane an exit
			}
			noteExit429(proxy, realModel)
			if proxy != "" && st == http.StatusTooManyRequests {
				// a 429 is this exit's own cap for the window; a 529 is
				// overload and may clear, so it is not written off here.
				burned[proxy] = true
			}
			if freeCap && zenRetryAfterSecs(resp.Header) >= zenTierCapParkSecs {
				noteExitTierCap(proxy) // out for the day: stop drawing this ip
			}
			if zenLastRetry(zenRetries, attempts) {
				// the ladder is spent on a written-off pool: refill and buy
				// more hops instead of handing the caller a 503, as long as
				// the marks (not a merely busy model) are what bounded it.
				if ladderExtend(r.Context(), &poolRecoverUntil) {
					attempts++
					lane = laneHandoff(lane, laneFailover(lane))
					sessionID = laneRotate(lane, realModel)
					rotating = true
					continue
				}
				// the client hears about the proxy pool, never the 429 we
				// just absorbed.
				resp, gaveUp = exhaustedExits(model, st, zenRetries+1), true
			} else {
				// this exit will not serve: switch to another exit with a
				// fresh session and keep going. noteExit429 above already
				// steered the picker away from this pair, so the next hop
				// lands on a different ip instead of the same one again.
				lane = laneHandoff(lane, laneFailover(lane))
				sessionID = laneRotate(lane, realModel)
				rotating = true
				zenBackoffCtx(r.Context(), zenRetries)
				continue
			}
		}

		if !gaveUp && zenRetryableStatus(resp.StatusCode) {
			// the exit (or a proxy in front of it) failed at transport
			// level: same treatment as a rate limit, so a client never
			// sees our plumbing break.
			st := resp.StatusCode
			resp.Body.Close()
			acclog.Printf("  opencode exit failed %d (retry %d/%d) model=%s country=%s proxy=%s lane=%s, rotating exit+session",
				st, zenRetries, zenRetryLabel(), model, proxyCountry(proxy), redactProxyUserinfo(proxy), lane.id)
			if noteExitTransportErr(proxy) {
				dropProxy(proxy)
			}
			if zenLastRetry(zenRetries, attempts) {
				// same extension as the 429 path: a refill replaces the dead
				// exits the ladder just spent itself on.
				if ladderExtend(r.Context(), &poolRecoverUntil) {
					attempts++
					lane = laneHandoff(lane, laneFailover(lane))
					sessionID = laneRotate(lane, realModel)
					rotating = true
					continue
				}
				resp, gaveUp = exhaustedExits(model, st, zenRetries+1), true
			} else {
				lane = laneHandoff(lane, laneFailover(lane))
				sessionID = laneRotate(lane, realModel)
				rotating = true
				zenBackoffCtx(r.Context(), zenRetries)
				continue
			}
		}

		if resp.StatusCode != http.StatusOK {
			eb, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
			resp.Body.Close()
			if zenServiceOverloaded(eb) {
				acclog.Printf("  opencode overloaded %d (retry %d/%d) model=%s country=%s proxy=%s rotating exit+session err=%q",
					resp.StatusCode, zenRetries, zenRetryLabel(), model, proxyCountry(proxy), redactProxyUserinfo(proxy), errSnippet(eb, 160))
				lane = laneHandoff(lane, laneFailover(lane))
				sessionID = laneRotate(lane, realModel)
				rotating = true
				if !zenLastRetry(zenRetries, attempts) {
					zenBackoffCtx(r.Context(), zenRetries)
					continue
				}
			}
			if zenGeoBlocked(eb) {
				logZenBlocked("geo", zenRetries, resp.StatusCode, model, proxy, "", eb)
				dropProxy(proxy)
				lane = laneHandoff(lane, laneFailover(lane))
				sessionID = laneRotate(lane, realModel)
				rotating = true
				if noteZenGeoErr() {
					acclog.Printf("  %d+ geo-blocks, refreshing proxy pool", zenErrorThreshold())
					refreshZenProxiesAsync(false)
					resetZenGeoErrs()
				}
				if !zenLastRetry(zenRetries, attempts) {
					zenBackoffCtx(r.Context(), zenRetries)
					continue
				}
			}
			if zenUserBlocked(eb) {
				logZenBlocked("user", zenRetries, resp.StatusCode, model, proxy, "", eb)
				dropProxy(proxy)
				lane = laneHandoff(lane, laneFailover(lane))
				sessionID = laneRotate(lane, realModel)
				rotating = true
				if noteZenBlockErr() {
					acclog.Printf("  %d+ user-blocks, refreshing proxy pool", zenErrorThreshold())
					refreshZenProxiesAsync(false)
					resetZenBlockErrs()
				}
				if !zenLastRetry(zenRetries, attempts) {
					zenBackoffCtx(r.Context(), zenRetries)
					continue
				}
			}
			if zenFreeTierBlocked(eb) {
				acclog.Printf("  opencode free-tier-blocked %d (retry %d/%d) model=%s country=%s proxy=%s lane=%s, switching lane",
					resp.StatusCode, zenRetries, zenRetryLabel(), model, proxyCountry(proxy), redactProxyUserinfo(proxy), lane.id)
				if zenGateIPBlocked(eb) {
					// the IP itself is capped/blocked: park the exit for a
					// cooldown and rotate the lane onto a fresh one.
					noteExitGate(proxy)
				} else {
					// request-shape refusal: the exit still serves other
					// shapes, so it keeps its seat and only this model is burnt.
					noteExitModelBurned(proxy, realModel)
				}
				if proxy == "" {
					laneEscalate(lane, realModel) // direct is flagged: give the lane an exit
				}
				lane = laneHandoff(lane, laneFailover(lane))
				sessionID = laneRotate(lane, realModel)
				rotating = true
				// keep the refusal body on resp: the branch continues with
				// resp.Body already closed, and a ladder that ends here must
				// still hand the caller zen's own answer.
				resp.Body = io.NopCloser(bytes.NewReader(eb))
				if noteZenFreeTierErr() {
					acclog.Printf("  %d+ free-tier blocks, refreshing proxy pool", zenErrorThreshold())
					refreshZenProxiesAsync(false)
					resetZenFreeTierErrs()
				}
				if !zenLastRetry(zenRetries, attempts) {
					zenBackoffCtx(r.Context(), zenRetries)
					continue
				}
			}
			acclog.Printf("!! opencode upstream %d (retry %d/%d) model=%s country=%s proxy=%s err=%q",
				resp.StatusCode, zenRetries, zenRetryLabel(), model, proxyCountry(proxy), redactProxyUserinfo(proxy), errSnippet(eb, 160))
			if len(bytes.TrimSpace(eb)) == 0 {
				// provider sometimes errors with no body; never hand the
				// client a 0-byte error, synthesize a JSON one.
				if resp.StatusCode == http.StatusForbidden {
					eb = []byte(`{"type":"error","error":{"type":"FreeTierError","message":"OpenCode's free tier can only be used from within OpenCode"}}`)
				} else {
					eb = []byte(fmt.Sprintf(`{"error":"upstream status %d with empty body"}`, resp.StatusCode))
				}
			}
			resp.Body = io.NopCloser(bytes.NewReader(eb))
		}
		resetZenNetErrs()
		if resp.StatusCode == http.StatusOK {

			p.noteZenSuccess(sessionID, usedProxy)
			noteExitOK(usedProxy, realModel)
			laneTouch(lane, usedProxy, realModel)
		}
		break
	}

	if resp == nil {
		acclog.Printf("!! opencode zen unreachable model=%s (no usable exit)", model)
		writeJSONError(w, http.StatusServiceUnavailable, proxyUnavailableMsg(model))
		return
	}

	// a 429 can still leave the ladder (the lane race path returns a response
	// as-is, and a spent attempt budget breaks out with whatever the last hop
	// said). the client contract is that zen's rate limit is never forwarded,
	// so absorb it here too instead of handing the caller the one status the
	// whole pool exists to hide.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == 529 {
		resp.Body.Close()
		resp = exhaustedExits(model, resp.StatusCode, 1)
	}

	if usedProxy != "" {
		acclog.Printf("  opencode session=%s proxy=%s lane=%s", sessionID, redactProxyUserinfo(usedProxy), lane.id)
	}

	if isStream && resp.StatusCode == http.StatusOK {
		// true streaming — pipe through, no token capture
	} else {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, upstreamBodyLimit))
		resp.Body.Close()
		if os.Getenv("ZEN_DUMP") != "" {
			rp := fmt.Sprintf("/tmp/zenraw_%d.json", time.Now().UnixNano())
			os.WriteFile(rp, rb, 0o600)
			acclog.Printf("ZEN_DUMP raw responses json %d bytes -> %s", len(rb), rp)
		}
		if endpoint == "/responses" && resp.StatusCode == http.StatusOK {
			// same no-tools continue as the stream branch, on the raw
			// SSE before folding.
			post := func(nb []byte) []byte {
				return p.nudgePostResponses(r, target, nb, &sessionID, lane)
			}
			if retry := tryNudgeResponses(sessionID, model, body, rb, post); retry != nil {
				_, nHasCalls := scanResponsesSSE(retry)
				acclog.Printf("  opencode nudge model=%s calls=%v bytes=%d [oai]", model, nHasCalls, len(retry))
				rb = mergeResponsesSSE([][]byte{rb, retry})
			}
			rb = responsesToChat(responsesSSEToJSON(rb))
		} else if !isStream && resp.StatusCode == http.StatusOK {
			// chat-model continue on raw SSE before folding (covers
			// wrapback too: the retry is chat-shaped like the first).
			post := func(nb []byte) []byte {
				return p.nudgePostResponses(r, target, nb, &sessionID, lane)
			}
			if retry := tryNudgeChat(sessionID, model, "opencode", body, rb, post); retry != nil {
				_, nHasCalls := scanChatSSE(retry)
				acclog.Printf("  opencode nudge model=%s calls=%v bytes=%d [oai]", model, nHasCalls, len(retry))
				rb = mergeChatSSE([][]byte{rb, retry})
			}
			rb = sseToNonStream(rb, realModel, tnames)
			if wrapBack {
				rb = chatToResponses(rb)
			}
		}
		resp.Body = io.NopCloser(bytes.NewReader(rb))
		if resp.StatusCode != http.StatusOK {
			recErr = strings.TrimSpace(string(rb))
			if len(recErr) > 200 {
				recErr = recErr[:200]
			}
			acclog.Printf("!! opencode upstream %d model=%s err=%q", resp.StatusCode, model, recErr)
		} else {
			prompT, compT, totalT = respTokens(rb)
		}
	}

	// buffered paths (non-stream, responses stream, wrapback) have tokens
	// now; log them. true chat passthrough streams below and logs after.
	deferStream := isStream && resp.StatusCode == http.StatusOK
	if !deferStream {
		logUsage(UsageRecord{
			Ts:               time.Now().UTC().Format(time.RFC3339Nano),
			Model:            model,
			KeyName:          "opencode",
			Method:           r.Method,
			Path:             r.URL.Path,
			StatusCode:       resp.StatusCode,
			DurationMs:       time.Since(start).Milliseconds(),
			PromptTokens:     prompT,
			CompletionTokens: compT,
			TotalTokens:      totalT,
			Stream:           isStream,
			Error:            recErr,
			ContentBytes:     int64(len(body)),
		})
		if resp.StatusCode == http.StatusOK {
			laneTokensAdd(lane, realModel, prompT, compT, totalT, time.Since(start).Milliseconds())
		}
	}

	for k, v := range resp.Header {
		lk := strings.ToLower(k)
		if lk == "content-type" || strings.HasPrefix(lk, "x-ratelimit") || lk == "retry-after" {
			w.Header()[k] = v
		}
	}
	if !isStream && resp.StatusCode == http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.StatusCode)

	var written int64
	if isStream && resp.StatusCode == http.StatusOK {
		if endpoint == "/responses" {
			if nudgeEnabled(model) && toolsOffered(body) {
				// nudge needs the full turn before deciding: buffer, same
				// as before. without tools offered, stream live below.
				rb, _ := io.ReadAll(io.LimitReader(resp.Body, upstreamBodyLimit))
				resp.Body.Close()
				if os.Getenv("ZEN_DUMP") != "" {
					rp := fmt.Sprintf("/tmp/zenraw_%d.sse", time.Now().UnixNano())
					os.WriteFile(rp, rb, 0o600)
					acclog.Printf("ZEN_DUMP raw responses SSE %d bytes -> %s", len(rb), rp)
				}
				rb = p.maybeNudgeResponses(r, target, body, rb, &sessionID, model, lane)
				written, prompT, compT = streamResponsesToChat(w, rb, tnames)
			} else {
				// live: convert + flush each upstream chunk, first byte out
				// as soon as zen sends it. client disconnect aborts the
				// upstream read so the exit stops working for nobody.
				written, prompT, compT = streamResponsesLive(r.Context(), w, resp.Body, tnames)
				resp.Body.Close()
			}
		} else if wrapBack {
			// responses client on a chat model with stream=true: fold the
			// upstream chat SSE to one chat body, then emit it as a responses
			// SSE sequence. buffered json under a text/event-stream
			// content-type breaks every streaming responses client.
			rb, _ := io.ReadAll(io.LimitReader(resp.Body, upstreamBodyLimit))
			resp.Body.Close()
			folded := sseToNonStream(rb, realModel, tnames)
			// chat-model continue on the folded JSON before wrapping.
			postFold := func(nb []byte) []byte {
				rb2 := p.nudgePostResponses(r, target, nb, &sessionID, lane)
				if rb2 == nil {
					return nil
				}
				return sseToNonStream(rb2, realModel, tnames)
			}
			if retry := tryNudgeJSON(sessionID, model, "opencode", body, folded, postFold); retry != nil {
				_, nHasCalls := scanChatJSON(retry)
				acclog.Printf("  opencode nudge model=%s calls=%v bytes=%d [oai]", model, nHasCalls, len(retry))
				folded = mergeChatJSON(folded, retry)
			}
			prompT, compT, _ = respTokens(folded)
			written = writeResponsesSSE(w, chatToResponses(folded))
		} else if _, ok := w.(http.Flusher); ok {
			if nudgeEnabled(model) && toolsOffered(body) {
				// nudge needs the full turn before deciding: buffer,
				// same as the responses branch. without tools offered,
				// stream live below.
				rb, _ := io.ReadAll(io.LimitReader(resp.Body, upstreamBodyLimit))
				resp.Body.Close()
				post := func(nb []byte) []byte {
					return p.nudgePostResponses(r, target, nb, &sessionID, lane)
				}
				if retry := tryNudgeChat(sessionID, model, "opencode", body, rb, post); retry != nil {
					_, nHasCalls := scanChatSSE(retry)
					acclog.Printf("  opencode nudge model=%s calls=%v bytes=%d [oai]", model, nHasCalls, len(retry))
					rb = mergeChatSSE([][]byte{rb, retry})
				}
				if p, c := sseUsageTee(rb); p > 0 || c > 0 {
					prompT, compT = p, c
				}
				n, _ := w.Write(rb)
				written = int64(n)
				if fl, ok := w.(http.Flusher); ok {
					fl.Flush()
				}
			} else {
				written, prompT, compT = copyZenChatStream(r.Context(), w, resp, nil, prompT, compT)
			}
		} else {
			written, _ = io.Copy(w, resp.Body)
		}
	} else {
		written, _ = io.Copy(w, resp.Body)
	}
	resp.Body.Close()
	if deferStream {
		// stream branches parsed tokens above (responses fold, wrapback
		// fold, or tee on passthrough); total follows the parsed pair.
		totalT = prompT + compT
		logUsage(UsageRecord{
			Ts:               time.Now().UTC().Format(time.RFC3339Nano),
			Model:            model,
			KeyName:          "opencode",
			Method:           r.Method,
			Path:             r.URL.Path,
			StatusCode:       resp.StatusCode,
			DurationMs:       time.Since(start).Milliseconds(),
			PromptTokens:     prompT,
			CompletionTokens: compT,
			TotalTokens:      totalT,
			Stream:           true,
			ContentBytes:     int64(len(body)),
		})
		laneTokensAdd(lane, realModel, prompT, compT, totalT, time.Since(start).Milliseconds())
	}
	acclog.Printf("<- %d %s %s %v %d bytes [opencode] model=%s %s", resp.StatusCode, r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond), written, realModel, reqID)
}

func contains(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

// zenHeaderForwarded reports whether a client header should be copied onto
// the zen upstream request. auth-bearing headers are always dropped so the
// proxy's own credentials never leak to the third-party upstream.
// x-session-id selects pii/lane state and would link the client across
// requests for the upstream; x-forwarded-host is client-claimed.
func zenHeaderForwarded(k string) bool {
	switch strings.ToLower(k) {
	case "authorization", "host", "content-type", "accept", "accept-encoding",
		"connection", "content-length", "user-agent", "cookie", "x-api-key",
		"x-session-id", "x-forwarded-host",
		"x-opencode-client", "x-opencode-session", "x-opencode-session-id",
		"x-opencode-request", "x-opencode-project", "x-session-affinity":
		return false
	}
	return true
}

// stripCacheFields removes anthropic cache fields and proxy-local model
// params recursively so zen never sees prompt_cache_key/cache_control/
// nudge_no_tools, which NV/NIM rejects as unrecognized.
func stripCacheFields(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k := range t {
			if k == "prompt_cache_key" || k == "promptCacheKey" || k == "cache_control" || k == "cacheControl" || k == "nudge_no_tools" {
				delete(t, k)
				continue
			}
			t[k] = stripCacheFields(t[k])
		}
		return t
	case []any:
		for i, e := range t {
			t[i] = stripCacheFields(e)
		}
		return t
	}
	return v
}

// ensureZenGate makes a body look like the v2 client: the <env> block in its
// system prompt and the `read` tool in its tools array. the free-tier gate
// answers 429 FreeUsageLimitError to a body missing either half, which is the
// shape every client hit before this landed — see zenEnvPrompt.
func ensureZenGate(m map[string]any) {
	ensureZenEnv(m)
	ensureZenTools(m)
}

// ensureZenEnv appends the CLI's <env> block to whichever system prompt the
// body carries. every shape is covered: responses instructions, anthropic
// top-level system, and the leading system message of a chat body. the caller's
// own prompt is kept — the gate reads the block's presence, not its contents.
func ensureZenEnv(m map[string]any) {
	if ins, ok := m["instructions"].(string); ok {
		m["instructions"] = zenEnvInto(ins)
		return
	}
	if sys, ok := m["system"].(string); ok {
		m["system"] = zenEnvInto(sys)
		return
	}
	if blocks, ok := m["system"].([]any); ok {
		m["system"] = zenEnvBlocks(blocks)
		return
	}
	msgs, _ := m["messages"].([]any)
	for _, raw := range msgs {
		mm, ok := raw.(map[string]any)
		if !ok || mm["role"] != "system" {
			continue
		}
		switch c := mm["content"].(type) {
		case string:
			mm["content"] = zenEnvInto(c)
		case []any:
			mm["content"] = zenEnvBlocks(c)
		}
		return
	}
	m["messages"] = append([]any{map[string]any{"role": "system", "content": zenEnvPrompt()}}, msgs...)
}

// zenEnvOK reports whether a system text satisfies the gate: the first
// <env>...</env> pair has to hold a Working directory line (measured 2026-10-26:
// the other fields are not read, but a block without this one is refused, and
// so is a valid block placed *after* an invalid one).
func zenEnvOK(text string) bool {
	i := strings.Index(text, "<env>")
	if i < 0 {
		return false
	}
	j := strings.Index(text[i:], "</env>")
	return j >= 0 && strings.Contains(text[i:i+j], "Working directory")
}

// zenEnvInto leaves a system text the gate already accepts alone, repairs a
// client <env> that lacks the working-directory line in place, and appends the
// block when there is none. appending next to a partial block does not work:
// the gate reads the first pair and stops there.
func zenEnvInto(text string) string {
	if strings.TrimSpace(text) == "" {
		return zenEnvPrompt()
	}
	if zenEnvOK(text) {
		return text
	}
	if i := strings.Index(text, "<env>"); i >= 0 {
		if j := strings.Index(text[i:], "</env>"); j >= 0 {
			at := i + len("<env>")
			return text[:at] + "\n" + zenEnvLine + text[at:]
		}
	}
	return text + "\n\n" + zenEnvPrompt()
}

// zenEnvBlocks applies zenEnvInto to an anthropic-style system block list.
func zenEnvBlocks(blocks []any) []any {
	for _, b := range blocks {
		bm, ok := b.(map[string]any)
		if !ok {
			continue
		}
		t, ok := bm["text"].(string)
		if !ok || !strings.Contains(t, "<env>") {
			continue
		}
		bm["text"] = zenEnvInto(t)
		return blocks
	}
	return append(blocks, map[string]any{"type": "text", "text": zenEnvPrompt()})
}

// ensureZenTools appends the `read` declaration the zen free tier demands. the
// gate at opencode.ai/inference fingerprints the tools array by name; a body
// with no tool named `read` is answered 429 FreeUsageLimitError like any other
// non-client request. client tools ride along unrestricted, so the client's own spelling
// stays authoritative and response translators map upstream tool call names
// back via tnames.resolve.
// bodies with no tools at all (plain chat clients, title gen, summaries)
// still get the ballast or the gate refuses them, but a callable toolset makes
// a summarizer emit calls opencode rejects ("tool call not allowed while
// generating summary"). so they get the def with tool_choice "none": the gate
// passes and the model cannot call it.

// zenBallastDefs returns the embedded ballast (the placeholder `read`) as a
// fresh slice; the caller may mutate elements, so storage is copied.
func zenBallastDefs() []any {
	src := zenOpenCodeDefs()
	out := make([]any, len(src))
	copy(out, src)
	return out
}
func ensureZenTools(m map[string]any) {
	t, _ := m["tools"].([]any)
	// a client that posts a single anthropic-shaped tool object rather than an
	// array: normalize to a one-element array or it reaches zen as-is.
	if tm, ok := m["tools"].(map[string]any); ok {
		t = []any{tm}
		m["tools"] = t
	}
	if len(t) == 0 {
		t = append(t, zenBallastDefs()...)
		m["tools"] = t
		m["tool_choice"] = "none"
		return
	}
	have := make(map[string]bool, len(t))
	for _, x := range t {
		xm, ok := x.(map[string]any)
		if !ok {
			continue
		}
		name := ""
		if fn, ok := xm["function"].(map[string]any); ok {
			name, _ = fn["name"].(string)
			// zen 400s when parameters is null/missing/non-object.
			if pm, ok := fn["parameters"].(map[string]any); !ok || pm == nil {
				fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
		} else if n, ok := xm["name"].(string); ok {
			// anthropic-shaped tool ({name, input_schema}) reaching us
			// unconverted, e.g. a client that posts it to /v1/chat/completions.
			// rewrite to the openai shape zen expects, or it forwards with a
			// null name and zen 400s the whole request.
			name = n
			fn := map[string]any{"name": n}
			if d, ok := xm["description"].(string); ok && d != "" {
				fn["description"] = d
			}
			if p, ok := xm["input_schema"]; ok {
				fn["parameters"] = p
			}
			if _, ok := fn["parameters"]; !ok {
				fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			xm["type"] = "function"
			delete(xm, "input_schema")
			xm["function"] = fn
		}
		if name != "" {
			have[name] = true
		}
	}
	for _, d := range zenBallastDefs() {
		dm, _ := d.(map[string]any)
		fn, _ := dm["function"].(map[string]any)
		if fn == nil {
			continue
		}
		n, _ := fn["name"].(string)
		if n == "" || have[n] {
			continue
		}
		t = append(t, d)
		have[n] = true
	}
	// sanitize every schema, not just ours: zen 400s the whole request with
	// "Recursive JSON schemas are not currently supported" when a client tool
	// ships $defs/$ref cycles, which Claude Code's own tools do.
	for _, raw := range t {
		if tm, ok := raw.(map[string]any); ok {
			if fn, ok := tm["function"].(map[string]any); ok {
				if p, ok := fn["parameters"]; ok {
					fn["parameters"] = sanitizeZenSchema(p)
				}
			}
		}
	}
	m["tools"] = t
}

// sanitizeZenSchema lowers a JSON schema to what zen's tool api accepts,
// mirroring sanitizeOpenAISchema in the real client
// (packages/opencode/src/provider/transform.ts). keyword-whitelisted, so
// exotic keywords are dropped rather than forwarded. $defs/definitions are
// dropped because zen resolves nothing: a $ref left pointing into a deleted
// $defs reads as a recursive schema and 400s the whole request.
//
// the live console validator counts nested object maps: the
// reorganize_notes tool 400s with 11 nested maps and passes trimmed to
// 10, while enum values and array wrappers pass untouched. so the budget
// is on maps: parameters bodies nest at most 9 maps, 10 with the tool
// wrapper. the root parameters map sits at 1 and is never omitted.
func sanitizeZenSchema(v any) any { return sanitizeZenSchemaSegs(v, 1) }

// zenSchemasMaxDepth is the validator's nesting budget in nested object
// maps from the parameters root (see sanitizeZenSchema). with the holder
// map charged, cutoff keeps bodies at 8 content maps; the wrapper adds the
// tool map for 9 total, one inside the limit.
const zenSchemasMaxDepth = 9

// zenOmit marks a subtree cut by the depth budget; parents drop it instead
// of nesting a replacement.
type zenOmitType struct{}

var zenOmit zenOmitType

func isZenOmit(v any) bool { _, ok := v.(zenOmitType); return ok }

// sanitizeZenSchemaSegs bounds map-nesting from the parameters root at 1:
// every object map nests one level deeper, arrays cost nothing (their
// elements sit at the array's own level), and scalars cost nothing. the
// console validator counts object maps this way and ignores the rest:
// the live reorganize_notes tool 400s with 11 nested maps and passes once
// trimmed to 10, while its enum values and array wrappers pass through
// untouched at any count. maps at zenSchemasMaxDepth are omitted; their
// parents drop the property (dangling required is pruned) so no phantom
// level survives. $defs/$refs never cross the keyword whitelist, so the
// walk only descends the finite input tree and always terminates.
func sanitizeZenSchemaSegs(v any, seg int) any {
	if seg >= zenSchemasMaxDepth {
		switch v.(type) {
		case map[string]any, bool:
			return zenOmit
		}
	}
	switch t := v.(type) {
	case bool:
		return map[string]any{"type": "string"}
	case []any:
		// arrays cost nothing: every element sits at the array's level.
		out := make([]any, 0, len(t))
		for _, e := range t {
			if s := sanitizeZenSchemaSegs(e, seg); !isZenOmit(s) {
				out = append(out, s)
			}
		}
		return out
	case map[string]any:
		r := map[string]any{}
		// $ref is dropped along with $defs below: zen resolves nothing, so a
		// $ref left pointing into a deleted $defs is exactly the "recursive
		// JSON schema" it rejects. the surrounding node keeps its inferred
		// type so the tool stays callable.
		if s, ok := t["description"].(string); ok {
			r["description"] = s
		}
		if c, ok := t["const"]; ok {
			r["enum"] = []any{c}
		} else if e, ok := t["enum"].([]any); ok {
			r["enum"] = e
		}
		hadProps := false
		np := map[string]any{}
		if p, ok := t["properties"].(map[string]any); ok {
			hadProps = true
			// values sit under the properties holder map: they enter two
			// levels deeper (holder + child), not one.
			for k, item := range p {
				if s := sanitizeZenSchemaSegs(item, seg+2); !isZenOmit(s) {
					np[k] = s
				}
			}
			if len(np) > 0 {
				r["properties"] = np
			}
		}
		// the real client keeps `title` alongside description; zen tolerates it.
		if s, ok := t["title"].(string); ok {
			r["title"] = s
		}
		if req, ok := t["required"].([]any); ok {
			nr := []any{}
			for _, item := range req {
				s, ok := item.(string)
				if !ok {
					continue
				}
				if hadProps {
					if _, still := np[s]; !still {
						// property cut by the depth budget: a dangling required
						// would be the next 400
						continue
					}
				}
				nr = append(nr, s)
			}
			if len(nr) > 0 {
				r["required"] = nr
			}
		}
		if it, ok := t["items"]; ok {
			if s := sanitizeZenSchemaSegs(it, seg+1); !isZenOmit(s) {
				r["items"] = s
			}
		}
		if ap, ok := t["additionalProperties"]; ok {
			if b, isBool := ap.(bool); isBool {
				r["additionalProperties"] = b
			} else if s := sanitizeZenSchemaSegs(ap, seg+1); !isZenOmit(s) {
				r["additionalProperties"] = s
			}
		}
		for _, k := range []string{"anyOf", "oneOf", "allOf"} {
			if c, ok := t[k].([]any); ok {
				nc := make([]any, 0, len(c))
				for _, e := range c {
					// the branch array is charged by the []any case; the
					// element sits one segment under the schema node.
					if s := sanitizeZenSchemaSegs(e, seg+1); !isZenOmit(s) {
						nc = append(nc, s)
					}
				}
				if len(nc) > 0 {
					r[k] = nc
				}
			}
		}
		types := []string{"string", "number", "boolean", "integer", "object", "array", "null"}
		var st []string
		switch tv := t["type"].(type) {
		case string:
			for _, x := range types {
				if tv == x {
					st = []string{tv}
				}
			}
		case []any:
			for _, x := range tv {
				if s, ok := x.(string); ok {
					for _, y := range types {
						if s == y {
							st = append(st, s)
						}
					}
				}
			}
		}
		hasComp := false
		for _, k := range []string{"anyOf", "oneOf", "allOf"} {
			if _, ok := r[k]; ok {
				hasComp = true
			}
		}
		// a $ref-stripped node keeps its empty shape so the parent holds
		// its position: the walker truncates through properties instead,
		// which terminates on any finite schema without dropping the slot
		// (see the Recursion test).
		if len(st) > 0 {
			if len(st) == 1 {
				r["type"] = st[0]
			} else {
				r["type"] = st
			}
			return r
		}
		if hasComp {
			return r
		}
		switch {
		case r["properties"] != nil || r["required"] != nil || r["additionalProperties"] != nil:
			r["type"] = "object"
		case r["items"] != nil:
			r["type"] = "array"
		}
		return r
	}
	return v
}

// sanitizeZenChatMessages repairs tool call/result pairing for zen's
// /chat/completions models (space-bunny): upstream 400s a bare
// [invalid_request_error] on dangling tool_calls, orphan tool messages,
// user text interleaved between a call and its result, or empty ids.
// matched results move directly behind their assistant message; dangling
// calls are stripped; orphan/empty-id tool messages become user text.
// responses-path models are untouched.
func sanitizeZenChatMessages(m map[string]any) {
	raw, ok := m["messages"].([]any)
	if !ok || len(raw) == 0 {
		return
	}
	toolText := func(rm map[string]any) string {
		switch c := rm["content"].(type) {
		case string:
			return c
		case nil:
			return ""
		default:
			if b, err := json.Marshal(c); err == nil {
				return string(b)
			}
			return ""
		}
	}
	callID := func(t any) string {
		tm, _ := t.(map[string]any)
		if tm == nil {
			return ""
		}
		id, _ := tm["id"].(string)
		fn, _ := tm["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if id == "" || strings.TrimSpace(name) == "" {
			return ""
		}
		return id
	}
	// first result index per call id wins.
	resIdx := map[string]int{}
	for i, r := range raw {
		rm, ok := r.(map[string]any)
		if !ok || rm["role"] != "tool" {
			continue
		}
		id, _ := rm["tool_call_id"].(string)
		if id == "" {
			continue
		}
		if _, seen := resIdx[id]; !seen {
			resIdx[id] = i
		}
	}
	// match each assistant's calls to later results. dangling (no later
	// result) and empty-id calls are stripped.
	taken := map[int]bool{}    // result indices emitted behind an assistant
	kept := map[int][]any{}    // assistant index -> kept tool_calls
	emitIdx := map[int][]int{} // assistant index -> result indices to emit
	for i, r := range raw {
		rm, ok := r.(map[string]any)
		if !ok || rm["role"] != "assistant" {
			continue
		}
		tc, ok := rm["tool_calls"].([]any)
		if !ok || len(tc) == 0 {
			continue
		}
		var kc []any
		var ei []int
		for _, t := range tc {
			id := callID(t)
			if id == "" {
				continue
			}
			ri, found := resIdx[id]
			if !found || ri <= i || taken[ri] {
				continue
			}
			taken[ri] = true
			kc = append(kc, t)
			ei = append(ei, ri)
		}
		kept[i] = kc
		emitIdx[i] = ei
	}
	out := make([]any, 0, len(raw))
	for i, r := range raw {
		rm, ok := r.(map[string]any)
		if !ok {
			out = append(out, r)
			continue
		}
		if rm["role"] == "tool" {
			if taken[i] {
				continue // already emitted behind its assistant
			}
			out = append(out, map[string]any{"role": "user", "content": toolText(rm)})
			continue
		}
		if kc, isA := kept[i]; isA {
			cp := map[string]any{}
			for k, v := range rm {
				cp[k] = v
			}
			if len(kc) > 0 {
				cp["tool_calls"] = kc
				if _, hasRC := cp["reasoning_content"]; !hasRC {
					cp["reasoning_content"] = ""
				}
			} else {
				delete(cp, "tool_calls")
				// same fill as the plain-assistant branch below, so a
				// stripped dangling turn does not differ on a second pass.
				if _, hasRC := cp["reasoning_content"]; !hasRC {
					cp["reasoning_content"] = ""
				}
			}
			out = append(out, cp)
			for _, ri := range emitIdx[i] {
				out = append(out, raw[ri])
			}
			continue
		}
		if rm["role"] == "assistant" {
			if _, hasRC := rm["reasoning_content"]; !hasRC {
				cp := map[string]any{}
				for k, v := range rm {
					cp[k] = v
				}
				cp["reasoning_content"] = ""
				out = append(out, cp)
				continue
			}
		}
		out = append(out, r)
	}
	dropZenFileParts(out)
	m["messages"] = out
}

// dropZenFileParts turns file parts into a text placeholder. zen's chat
// gateway accepts only text and image_url parts, so a document block (a pdf
// attachment) 400s the whole request with "[invalid_value] content part type
// \"file\" is not supported". the bytes cannot be forwarded, so say so
// instead of losing the turn.
func dropZenFileParts(msgs []any) {
	for _, raw := range msgs {
		rm, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := rm["content"].([]any)
		if !ok {
			continue
		}
		dropped := false
		kept := make([]any, 0, len(parts))
		for _, p := range parts {
			pm, ok := p.(map[string]any)
			if !ok {
				kept = append(kept, p)
				continue
			}
			if t, _ := pm["type"].(string); t == "file" || t == "input_file" {
				dropped = true
				continue
			}
			kept = append(kept, p)
		}
		if !dropped {
			continue
		}
		kept = append(kept, map[string]any{
			"type": "text",
			"text": "[attachment dropped: this upstream accepts text and images only]",
		})
		rm["content"] = kept
	}
}

// captureRequest dumps an inbound POST to /tmp/oc-capture so real client
// traffic can be replayed. enabled with ZEN_CAPTURE=1, only after auth.
// credential headers are redacted, the body is capped, files are 0600.
func captureRequest(r *http.Request) {
	b, err := io.ReadAll(io.LimitReader(r.Body, bodyLimit))
	if err != nil {
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	dir := "/tmp/oc-capture"
	os.MkdirAll(dir, 0o700)
	name := strings.ReplaceAll(strings.Trim(r.URL.Path, "/"), "/", "_")
	if name == "" {
		name = "root"
	}
	hdr := r.Header.Clone()
	for k := range hdr {
		if !zenHeaderForwarded(k) && !strings.EqualFold(k, "Content-Type") {
			hdr.Del(k)
		}
	}
	rec := map[string]any{
		"time":    time.Now().Format(time.RFC3339Nano),
		"method":  r.Method,
		"path":    r.URL.Path,
		"headers": hdr,
		"body":    string(b),
	}
	out, err := json.Marshal(rec)
	if err != nil {
		return
	}
	os.WriteFile(fmt.Sprintf("%s/%d-%s.json", dir, time.Now().UnixNano(), name), out, 0o600)
}

// sseToNonStream folds a chat/completions SSE stream into one completion body.
func sseToNonStream(rb []byte, model string, tnames toolNames) []byte {
	type tcall struct{ id, name, args string }
	var content strings.Builder
	var reasoning strings.Builder
	calls := map[int]*tcall{}
	var order []int
	finish := "stop"
	usage := map[string]any{}
	for _, line := range strings.Split(string(rb), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var ch struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					Reasoning        string `json:"reasoning"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &ch) != nil {
			continue
		}
		if ch.Usage != nil {
			usage = ch.Usage
		}
		for _, c := range ch.Choices {
			content.WriteString(c.Delta.Content)
			if c.Delta.Reasoning != "" {
				reasoning.WriteString(c.Delta.Reasoning)
			} else if c.Delta.ReasoningContent != "" {
				reasoning.WriteString(c.Delta.ReasoningContent)
			}
			if c.FinishReason != nil && *c.FinishReason != "" {
				finish = *c.FinishReason
			}
			for _, t := range c.Delta.ToolCalls {
				e := calls[t.Index]
				if e == nil {
					e = &tcall{}
					calls[t.Index] = e
					order = append(order, t.Index)
				}
				if t.ID != "" {
					e.id = t.ID
				}
				if t.Function.Name != "" {
					e.name = tnames.resolve(t.Function.Name)
				}
				e.args += t.Function.Arguments
			}
		}
	}
	msg := map[string]any{"role": "assistant", "content": stripWaitSentinel(content.String())}
	if rStr := stripWaitSentinel(reasoning.String()); rStr != "" {
		msg["reasoning_content"] = rStr
	}
	if len(order) > 0 {
		arr := make([]any, 0, len(order))
		for _, idx := range order {
			e := calls[idx]
			id := e.id
			if id == "" {
				id = "call_" + randHex(12)
			}
			arr = append(arr, map[string]any{
				"id": id, "type": "function",
				"function": map[string]any{"name": e.name, "arguments": e.args},
			})
		}
		msg["tool_calls"] = arr
		if content.Len() == 0 {
			msg["content"] = nil
		}
	}
	out := map[string]any{
		"id": "chatcmpl-" + randHex(12), "object": "chat.completion",
		"created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
		"usage":   usage,
	}
	b, err := json.Marshal(out)
	if err != nil {
		return rb
	}
	return b
}

// hot-reload is handled by watchConfig in config.go now (single file).

// cliFlags are testing overrides: flags win over config values (and PORT
// for --port). no flags = byte-identical behavior to today.
type cliFlags struct {
	config  string
	port    int
	lanes   int
	debug   bool
	dbgFile string
	set     map[string]bool
	unknown []string // unrecognized -flags, reported by main
}

// parseFlags handles `--flag v` / `--flag=v` anywhere around an optional
// `probe` subcommand: `nimroute --port 5420 probe` and
// `nimroute probe --port 5420` both work. unknown flags are collected so
// main can reject them: a typo'd flag silently doing nothing is worse than
// a startup error.
func parseFlags(args []string) (cliFlags, []string) {
	var f cliFlags
	f.set = map[string]bool{}
	var rest []string
	take := func(i int, name string) (string, int) {
		if v, ok := strings.CutPrefix(args[i], name+"="); ok {
			return v, i
		}
		if args[i] == name && i+1 < len(args) {
			return args[i+1], i + 1
		}
		return "", -1
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--debug" {
			f.debug = true
			f.set["debug"] = true
			continue
		}
		// --dbg-file / -df: boolean with an optional value. `-df` bare keeps
		// the default dir, `--dbg-file=/some/dir` (or `--dbg-file /some/dir`)
		// moves the dumps. `-df` never eats a positional: it's the short bool.
		if a == "-df" {
			f.set["dbgfile"] = true
			continue
		}
		if a == "--dbg-file" {
			if v, ni := take(i, "--dbg-file"); ni >= 0 {
				f.dbgFile, f.set["dbgfile"] = v, true
				i = ni
				continue
			}
			f.set["dbgfile"] = true
			continue
		}
		if v, ok := strings.CutPrefix(a, "--dbg-file="); ok {
			f.dbgFile, f.set["dbgfile"] = v, true
			continue
		}
		if v, ni := take(i, "--config"); ni >= 0 {
			f.config, f.set["config"] = v, true
			i = ni
			continue
		}
		if v, ni := take(i, "--port"); ni >= 0 {
			p, err := strconv.Atoi(v)
			if err != nil || p <= 0 || p > 65535 {
				f.unknown = append(f.unknown, fmt.Sprintf("--port=%q (want 1..65535)", v))
			} else {
				f.port, f.set["port"] = p, true
			}
			i = ni
			continue
		}
		if v, ni := take(i, "--lanes"); ni >= 0 {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				f.unknown = append(f.unknown, fmt.Sprintf("--lanes=%q (want positive integer)", v))
			} else {
				f.lanes, f.set["lanes"] = n, true
			}
			i = ni
			continue
		}
		// a value-taking flag with nothing after it is worth naming: the
		// bare-flag guard has to run after every take() attempt.
		if a == "--config" || a == "--port" || a == "--lanes" {
			f.unknown = append(f.unknown, a+" (missing value)")
			continue
		}
		if a == "--help" || a == "-h" {
			f.set["help"] = true
			continue
		}
		if strings.HasPrefix(a, "-") {
			f.unknown = append(f.unknown, a)
			continue
		}
		rest = append(rest, a)
	}
	return f, rest
}

// serverMainFlags builds the pool and listens: cli flags win over config
// values, applied before pool build and listen.
func serverMainFlags(f cliFlags) {
	initLogging()
	fetchOpenCodeVersion()

	// everything comes from config.yml (auto-created from the example on
	// first run, keys.jsonc migrated once). CONFIG_FILE overrides the path.
	cpath := configPath()
	if f.set["config"] && f.config != "" {
		cpath = f.config
	}
	c := loadOrCreateConfig(cpath)
	if f.set["lanes"] && f.lanes > 0 {
		c.Zen.Lanes = f.lanes
	}
	applyConfig(c)
	if f.set["debug"] && f.debug {
		debugMode = true
	}
	if f.set["dbgfile"] {
		// after initLogging so acclog is live: the first dump on a bad dir
		// warns instead of vanishing.
		setDebugFile(true, f.dbgFile)
		log.Printf("  dbg-file: dumping failed requests to %s", debugFileDir())
	}
	if c.Antseed.Enabled {
		go antRefresh()
	}
	// must follow applyConfig: usage.path/enabled come from the config, so
	// opening the file earlier silently used the defaults.
	initUsageLog()
	if len(c.NvidiaKeys) == 0 {
		log.Printf("  no nvidia keys, running keyless (opencode/* free models only)")
	}

	refreshOpencodeModels()
	go func() {
		t := time.NewTicker(45 * time.Minute)
		for range t.C {
			refreshOpencodeModels()
		}
	}()
	// openrouter + aa both fetch in the background: neither may hold the
	// listener, and both keep their last good data on failure.
	watchOpenRouter()
	watchBenchmarks()
	go watchOpenCodeVersion()
	go watchZenProxies()

	pool := newPool(c.NvidiaKeys)
	nousSyncKeys(c.NousKeys)
	unoSyncKeys(c.UnoKeys, c.Uno.PaidKeys)
	orChatSyncKeys(c.OpenRouterKeys, c.OpenRouter.PaidKeys)
	orcaSyncKeys(c.OrcaKeys, c.Orca.PaidKeys)
	// seed the keyed price catalogs off the request path, so the first
	// /v1/models after a restart already quotes prices.
	go unoPriceRows()
	go watchConfig(pool, cpath)

	port := c.Server.Port
	if e := os.Getenv("PORT"); e != "" {
		if p, err := strconv.Atoi(strings.TrimSpace(e)); err == nil {
			port = p
		}
	}
	if f.set["port"] && f.port > 0 {
		port = f.port
	}
	if port <= 0 {
		port = 5419
	}
	bind := c.Server.Bind
	if bind == "" {
		bind = "127.0.0.1"
	}
	addr := net.JoinHostPort(bind, strconv.Itoa(port))

	stat := pool.Status()
	names := make([]string, 0, len(c.NvidiaKeys))
	for n := range c.NvidiaKeys {
		names = append(names, n)
	}
	sort.Strings(names)
	if stat.Total == 0 {
		log.Printf("NimRoute v%s — keyless (opencode/* free models only)", versionStr)
	} else {
		log.Printf("NimRoute v%s — %d keys: %s", versionStr, stat.Total, strings.Join(names, ", "))
		log.Printf("  429 backoff=%v..%v (exp, reset on success), model-lockout=%v, burst-backoff=%v", cooldown429, maxBackoff, modelLockout, burstCooldown)
		log.Printf("  weighted key pick: idle-preference + 50m failure window")
		log.Printf("  Effective ~%d RPM (40 RPM/key × %d keys)", 40*stat.Total, stat.Total)
	}
	if authRequired() {
		log.Printf("  auth: %d token(s) required on model endpoints", len(c.Auth.Tokens))
	} else {
		log.Printf("  auth: open (set auth.tokens in %s to lock down)", cpath)
	}
	if c.Zen.AlwaysProxy {
		log.Printf("  zen: always_proxy on")
	}
	if !c.Guardrails.Enabled {
		log.Printf("  guardrails: disabled")
	}
	if !c.Inject.HelpfulLine {
		log.Printf("  helpful-line injector: disabled")
	}
	if !c.Inject.Params {
		log.Printf("  body normalization: disabled (bodies pass through as-is)")
	}
	if c.Anonymize.Enabled && (len(c.Anonymize.Terms) > 0 || len(c.Anonymize.Entities) > 0) {
		log.Printf("  anonymize: %d term(s) %d entit(ies) mode=%s", len(c.Anonymize.Terms), len(c.Anonymize.Entities), c.Anonymize.Mode)
	}
	if c.Anonymize.Enabled {
		log.Printf("  pii heuristics: %.1f us/KB", pii.RegexUSPerKB())
		if c.Anonymize.Ner.Enabled && !pii.NerAvailable() {
			log.Printf("  WARN: anonymize.ner.enabled but this build has no ner (rebuild with -tags ner); detection is heuristics-only")
		}
	}
	if !c.Nudge.Enabled {
		log.Printf("  nudge: disabled")
	}
	if c.Usage.Enabled {
		log.Printf("  Usage tracking -> %s", c.Usage.Path)
	}
	log.Printf("  Config hot-reload enabled (%s)", cpath)
	log.Print()
	log.Printf("Listening on %s", addr)
	log.Printf("baseurl = http://%s/v1", addr)
	if authRequired() {
		log.Printf("api_key = one of auth.tokens")
	} else {
		log.Printf("api_key = dummy")
	}
	log.Print()
	log.Printf("Status: http://%s/status", addr)

	mux := http.NewServeMux()
	mux.Handle("/", pool)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// no write deadline: a streamed turn legitimately runs for minutes,
		// and this is what was cutting long answers off mid-response.
		WriteTimeout: 0,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Print("shutting down...")
		srv.Close()
		closeUsageLog()
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		if strings.Contains(err.Error(), "address already in use") {
			log.Fatalf("server error: %s is already in use (is nimroute already running? check `./nim status` or pass `--port N`)", addr)
		}
		log.Fatalf("server error: %v", err)
	}
}

// probe mode

var probeModels = []string{
	"minimaxai/minimax-m3",
	"moonshotai/kimi-k2.6",
	"nvidia/nemotron-3-ultra-550b-a55b",
	"nvidia/nemotron-3.5-lightning-30b-a3b",
	"poolside/laguna-xs-2.1",
	"stepfun-ai/step-3.7-flash",
	"deepseek-ai/deepseek-v4-flash-0731",
}

func runProbe() {
	c, err := loadConfigFile(configPath())
	if err != nil {
		log.Fatalf("Cannot read %s: %v", configPath(), err)
	}
	initUsageLog()
	entries := c.NvidiaKeys
	if len(entries) == 0 {
		log.Fatalf("%s has no nvidia_keys: probe needs nvidia keys", configPath())
	}

	keyList := make([]string, 0, len(entries))
	for _, v := range entries {
		keyList = append(keyList, v)
	}

	payload := `{"model":"%s","messages":[{"role":"user","content":"hi"}],"max_tokens":10,"stream":false}`

	fmt.Println()
	fmt.Println("NVIDIA NIM Rate Limit Probe")
	fmt.Println("============================")
	fmt.Printf("Keys: %d\n", len(keyList))
	fmt.Printf("Testing %d models, 3 requests each (1s apart)\n", len(probeModels))
	fmt.Println()

	type result struct {
		model    string
		attempts []int
		rl       []string
		tokens   []int
		ok       int
		errs     int
	}

	var results []result

	for _, m := range probeModels {
		r := result{model: m}
		fmt.Printf("  %-45s ", m)

		for i := 0; i < 3; i++ {
			if i > 0 {
				time.Sleep(1 * time.Second)
			}

			key := keyList[rand.Intn(len(keyList))]
			body := fmt.Sprintf(payload, m)

			req, _ := http.NewRequest("POST", nvidiaBase+"/chat/completions", bytes.NewReader([]byte(body)))
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("Content-Type", "application/json")

			cl := &http.Client{Timeout: 120 * time.Second}
			start := time.Now()
			resp, err := cl.Do(req)
			if err != nil {
				r.errs++
				fmt.Printf("!")
				r.attempts = append(r.attempts, 0)
				continue
			}
			elapsed := time.Since(start).Milliseconds()

			rb, _ := io.ReadAll(io.LimitReader(resp.Body, upstreamBodyLimit))
			resp.Body.Close()

			rh := rlHeaders(resp.Header)
			prompT, compT, _ := respTokens(rb)

			r.attempts = append(r.attempts, int(elapsed))

			var errBody string
			if resp.StatusCode == 429 {
				var cd string
				if v, ok := rh["Retry-After"]; ok {
					cd = v
				} else if v, ok := rh["retry-after"]; ok {
					cd = v
				}
				rlStr := fmt.Sprintf("429:%s", cd)
				r.rl = append(r.rl, rlStr)
				r.tokens = append(r.tokens, 0)
				fmt.Printf("R")
			} else if resp.StatusCode == 200 {
				r.ok++
				r.tokens = append(r.tokens, prompT+compT)
				fmt.Printf(".")
			} else {
				r.errs++
				errBody = strings.TrimSpace(string(rb))
				if len(errBody) > 200 {
					errBody = errBody[:200]
				}
				r.tokens = append(r.tokens, 0)
				fmt.Printf("%d", resp.StatusCode)
			}

			rec := UsageRecord{
				Ts:               time.Now().UTC().Format(time.RFC3339Nano),
				Model:            m,
				KeyName:          short(key),
				Method:           "POST",
				Path:             "/v1/chat/completions",
				StatusCode:       resp.StatusCode,
				DurationMs:       elapsed,
				PromptTokens:     prompT,
				CompletionTokens: compT,
				TotalTokens:      prompT + compT,
				RetryAttempt:     i,
				RateLimited:      resp.StatusCode == 429,
				Error:            errBody,
				ContentBytes:     int64(len(body)),
				Headers:          rh,
			}
			logUsage(rec)
		}
		results = append(results, r)
		fmt.Println()
	}

	fmt.Println()
	fmt.Println("Results")
	fmt.Println("=======")
	fmt.Printf("%-45s  %-12s  %-12s  %s\n", "Model", "Attempts(ms)", "RateLimited", "Tokens(in+out)")
	fmt.Println(strings.Repeat("-", 100))
	for _, r := range results {
		ats := ""
		for i, a := range r.attempts {
			if i > 0 {
				ats += ","
			}
			ats += fmt.Sprintf("%d", a)
		}
		rlStr := ""
		for i, rl := range r.rl {
			if i > 0 {
				rlStr += ","
			}
			rlStr += rl
		}
		if rlStr == "" {
			rlStr = "-"
		}
		tokStr := ""
		for i, t := range r.tokens {
			if i > 0 {
				tokStr += ","
			}
			tokStr += fmt.Sprintf("%d", t)
		}
		fmt.Printf("%-45s  %-12s  %-12s  %s\n", r.model, ats, rlStr, tokStr)
	}
	fmt.Println()

	closeUsageLog()
}

// cliUsage is printed by --help and on a bad flag. plain text, no colors:
// it also lands in launchd/systemd logs where escapes are noise.
const cliUsage = `nimroute - OpenAI/Anthropic-compatible LLM proxy (default port 5419)

usage:
  nimroute [flags]                 start the proxy
  nimroute probe [flags]           probe upstream reachability, print a table
  nimroute models sync [flags]     refresh the model catalog

flags:
  --config PATH         config file (default config.yml, env CONFIG_FILE)
  --port N              listen port (overrides server.port, env PORT)
  --lanes N             zen lane count (overrides zen.lanes)
  --debug               verbose logging
  -df, --dbg-file[=DIR] dump failed requests as json, optionally into DIR
  -h, --help            print this

config.yml is created on first run and hot-reloaded after that.
`

func main() {
	if nimName() {
		runNim(os.Args[1:])
		return
	}
	f, rest := parseFlags(os.Args[1:])
	if f.set["help"] {
		fmt.Print(cliUsage)
		return
	}
	if len(f.unknown) > 0 {
		fmt.Fprintf(os.Stderr, "nimroute: unknown flag %s\n", strings.Join(f.unknown, " "))
		fmt.Fprint(os.Stderr, "run `nimroute --help` for the flag list\n")
		os.Exit(2)
	}
	if len(rest) > 0 && rest[0] == "probe" {
		runProbeFlags(f)
		return
	}
	if len(rest) > 1 && rest[0] == "models" && rest[1] == "sync" {
		runModelsSyncFlags(f)
		return
	}
	if len(rest) > 0 {
		fmt.Fprintf(os.Stderr, "nimroute: unknown command %q\n", strings.Join(rest, " "))
		fmt.Fprint(os.Stderr, "run `nimroute --help` for the command list\n")
		os.Exit(2)
	}
	serverMainFlags(f)
}

// runModelsSyncFlags is runModelsSync honoring --config and --models.
func runModelsSyncFlags(f cliFlags) {
	if f.set["config"] && f.config != "" {
		os.Setenv("CONFIG_FILE", f.config)
	}
	cpath := configPath()
	if f.set["config"] && f.config != "" {
		cpath = f.config
	}
	c, err := loadConfigFile(cpath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot read %s: %v\n", cpath, err)
		os.Exit(1)
	}
	applyConfig(c)
	os.Exit(runModelsSync(cpath))
}

// runProbeFlags is runProbe honoring --config. --port/--lanes/--debug
// don't apply: probe never listens and builds no pool.
func runProbeFlags(f cliFlags) {
	if f.set["config"] && f.config != "" {
		os.Setenv("CONFIG_FILE", f.config)
	}
	runProbe()
}
