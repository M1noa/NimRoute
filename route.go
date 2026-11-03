package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// route backend: one name fans out across providers in strategy order.
// route/<key> tries opencode, openrouter, uno, antseed, nvidia, freepi,
// nous in turn (or by strategy), first 200 wins. keys match loosely where
// it is safe: lowercase, one trailing -free stripped from opencode ids,
// one trailing :free from uno/openrouter ids (or -free from orcarouter), and a trailing dated build
// stamp peeled from any id ("...-1017" -> "...") when the four digits
// decode as mmdd, so dated refreshes like uno's deepseek-v4.1-flash-1017
// hit opencode's deepseek-v4.1-flash. nvidia ids keep the org prefix.
// most backends get retries in the ladder (routeAttemptsFor); race fans
// out once per backend.

// routeBackends is the fixed priority order.
var routeBackends = []string{"opencode", "openrouter", "orcarouter", "uno", "antseed",
	"nvidia", "freepi", "nous"}

// routeAttemptCap is the sequential-ladder retry budget per backend.
// opencode tries three times, uno and antseed and nvidia two (one retry
// each), openrouter and freepi and nous once.
var routeAttemptCap = map[string]int{
	"opencode":   3,
	"openrouter": 1,
	"orcarouter": 2,
	"uno":        2,
	"antseed":    2,
	"nvidia":     2,
	"freepi":     1,
	"nous":       1,
}

// routeAttemptsFor is the ladder budget for one backend, 1 when unknown.
func routeAttemptsFor(backend string) int {
	if n, ok := routeAttemptCap[backend]; ok && n > 0 {
		return n
	}
	return 1
}

// expandRouteAttempts repeats each candidate by its backend's ladder budget
// so opencode gets three tries and uno/antseed/nvidia two before the order
// falls through to the next backend. race mode stays one candidate per
// backend: three vacuums on one exit would only fan ignition wider.
func expandRouteAttempts(cands []string) []string {
	expanded := cands[:0:0]
	for _, c := range cands {
		backend, _, _ := strings.Cut(c, "/")
		for i := 0; i < routeAttemptsFor(backend); i++ {
			expanded = append(expanded, c)
		}
	}
	return expanded
}

// routeKey normalizes one backend model id to its route name: lowercase,
// opencode's single trailing -free dropped, uno/openrouter's trailing
// :free dropped, then one trailing dated build stamp (mmdd) peeled. nvidia
// ids keep the org slash, so "route/moonshotai/kimi-k3" stays exact.
func routeKey(backend, id string) string {
	k := strings.ToLower(strings.TrimSpace(id))
	switch backend {
	case "opencode": // opencode lists both base and -free ids for one model
		k = strings.TrimSuffix(k, "-free")
	case "uno", "openrouter": // keyed catalogs carry vendor/model:free rows
		k = strings.TrimSuffix(k, ":free")
	case "orcarouter": // orcarouter marks free rows with -free
		k = strings.TrimSuffix(k, "-free")
	}
	return peelDateStamp(k)
}

// routeExclude reports whether a route key is dropped by config globs.
func routeExclude(key string) bool {
	for _, pat := range cfg().Route.ExcludeModels {
		if pat != "" && globMatch(strings.ToLower(pat), key) {
			return true
		}
	}
	return false
}

// routeBackendOff reports whether a backend is excluded from every route.
func routeBackendOff(backend string) bool {
	for _, b := range cfg().Route.ExcludeProviders {
		if strings.EqualFold(strings.TrimSpace(b), backend) {
			return true
		}
	}
	return false
}

// routeBackendLive reports whether a backend can serve right now.
func routeBackendLive(backend string, p *Pool) bool {
	switch backend {
	case "opencode":
		return zenEnabled()
	case "antseed":
		return antseedEnabled()
	case "freepi":
		return freepiEnabled()
	case "nvidia":
		return cfg().Nvidia.Enabled && p.keyCount() > 0
	case "uno":
		return unoEnabled()
	case "openrouter":
		return orChatEnabled()
	case "orcarouter":
		return orcaEnabled()
	case "nous":
		return nousEnabled()
	}
	return false
}

// routeTable maps route key -> backend -> exact backend model id.
type routeTable map[string]map[string]string

// routeModel is one servable pair: a live backend and one exact model id.
// wroute matches against these; route folds them by key.
type routeModel struct {
	backend string
	id      string
}

// routeTableTTL is how long one built table stays fresh. /status polls at 1hz
// and every build unions each live backend's catalog, so the table is reused
// instead of rebuilt per request.
const routeTableTTL = 2 * time.Second

// routeTableCache holds the last built catalog and table per pool. the entry
// is pinned to the config pointer, so a hot reload rebuilds at once.
var routeTableCache struct {
	sync.Mutex
	cat []routeModel
	t   routeTable
	cfg *appConfig
	p   *Pool
	at  time.Time
}

// routeTableFor returns the cached table, rebuilding at most once per ttl.
func routeTableFor(p *Pool) routeTable {
	_, t := routeCatalogAndTable(p)
	return t
}

// routeCatalogAndTable returns the flat catalog and its folded table from one
// cache entry: wroute needs the exact ids, route needs the folded keys.
func routeCatalogAndTable(p *Pool) ([]routeModel, routeTable) {
	c := cfg()
	routeTableCache.Lock()
	if t := routeTableCache.t; t != nil && routeTableCache.p == p && routeTableCache.cfg == c && time.Since(routeTableCache.at) < routeTableTTL {
		cat, t := routeTableCache.cat, routeTableCache.t
		routeTableCache.Unlock()
		return cat, t
	}
	routeTableCache.Unlock()
	cat := buildRouteCatalog(p)
	t := buildRouteTable(cat)
	routeTableCache.Lock()
	routeTableCache.cat, routeTableCache.t, routeTableCache.p, routeTableCache.cfg, routeTableCache.at = cat, t, p, c, time.Now()
	routeTableCache.Unlock()
	return cat, t
}

// zenListRefresh admits one background listing refresh at a time: an
// unreachable upstream must not stack goroutines on the 10s client timeout.
var zenListRefresh = make(chan struct{}, 1)

// kickZenListRefresh refreshes the cached zen listing off the request path.
func kickZenListRefresh() {
	select {
	case zenListRefresh <- struct{}{}:
		go func() {
			defer func() { <-zenListRefresh }()
			refreshOpencodeModels()
		}()
	default:
	}
}

// buildRouteCatalog unions every live backend's catalog as exact pairs.
// excluded backends and models never enter. freepi joins only when
// route.include_freepi is set (its window is smaller).
func buildRouteCatalog(p *Pool) []routeModel {
	var cat []routeModel
	add := func(backend, id string) {
		if strings.TrimSpace(id) == "" || routeExclude(routeKey(backend, id)) {
			return
		}
		cat = append(cat, routeModel{backend: backend, id: id})
	}
	if zenEnabled() && !routeBackendOff("opencode") {
		// cached list: per-request fetch would block on upstream.
		var zenIDs []string
		if info := ocInfo.Load(); info != nil {
			for _, m := range info.Models {
				zenIDs = append(zenIDs, strings.TrimPrefix(m, "opencode/"))
			}
		}
		if len(zenIDs) == 0 {
			// never fetch on the request path: serve what is cached and let
			// the background refresh fill the listing.
			kickZenListRefresh()
		}
		for _, m := range zenIDs {
			// same free filter as the /v1/models listing
			if (!strings.HasSuffix(m, "-free") && m != "big-pickle") || unlistedFreeModels[m] {
				continue
			}
			add("opencode", m)
		}
		for _, m := range extraFreeModels {
			add("opencode", m)
		}
	}
	if antseedEnabled() && !routeBackendOff("antseed") {
		antRefresh()
		antMu2.Lock()
		models := append([]string{}, antModels...)
		antMu2.Unlock()
		for _, m := range models {
			add("antseed", m)
		}
	}
	if !routeBackendOff("freepi") && cfg().Route.IncludeFreepi && cfg().Freepi.Enabled {
		for _, fa := range freepiAccounts() {
			for _, m := range freepiModels(fa) {
				add("freepi", m)
			}
		}
	}
	if !routeBackendOff("nvidia") && nvidiaEnabled() {
		modelInfoMu.RLock()
		for id := range modelInfo {
			if strings.HasPrefix(id, "opencode/") || strings.HasPrefix(id, "freeepi/") || strings.HasPrefix(id, "antseed/") {
				continue
			}
			pl := strings.ToLower(id)
			if strings.HasSuffix(pl, "-free") || pl == "big-pickle" || strings.HasPrefix(pl, "muse-spark") {
				continue
			}
			add("nvidia", id)
		}
		modelInfoMu.RUnlock()
	}
	if !routeBackendOff("uno") && unoEnabled() {
		for _, m := range unoModels() {
			add("uno", m.ID)
		}
	}
	if !routeBackendOff("openrouter") && orChatEnabled() {
		for _, m := range orChatModels() {
			add("openrouter", m.ID)
		}
	}
	if !routeBackendOff("orcarouter") && orcaEnabled() {
		for _, m := range orcaModels() {
			add("orcarouter", m.ID)
		}
	}
	if !routeBackendOff("nous") && nousEnabled() {
		for _, m := range nousFreeModels() {
			add("nous", m.ID)
		}
	}
	return cat
}

// buildRouteTable folds the catalog into key -> backend -> id. min counts
// distinct backends per key.
func buildRouteTable(cat []routeModel) routeTable {
	t := routeTable{}
	for _, m := range cat {
		k := routeKey(m.backend, m.id)
		if k == "" {
			continue
		}
		b := t[k]
		if b == nil {
			b = map[string]string{}
			t[k] = b
		}
		if _, ok := b[m.backend]; !ok {
			b[m.backend] = m.id
		}
	}
	min := cfg().Route.MinOccurrences
	if min < 1 {
		min = 1
	}
	if min > 1 {
		for k, m := range t {
			if len(m) < min {
				delete(t, k)
			}
		}
	}
	return t
}

// routeFree reports whether one catalog id is a free row on its backend. the
// keyed catalogs say so in the id; the rest serve free models by
// construction (opencode's tier pool, antseed's sellers, freepi's ad-supported
// accounts, nous's zero-priced catalog, nvidia's key pool).
func routeFree(backend, id string) bool {
	switch backend {
	case "uno", "openrouter": // keyed catalogs mark free rows with :free
		return strings.HasSuffix(strings.ToLower(id), ":free")
	case "orcarouter": // orcarouter marks them with -free
		return strings.HasSuffix(strings.ToLower(id), "-free")
	}
	return true
}

// routePrice is one candidate's published price per million tokens, input
// plus output. only backends whose catalog quotes a price take part: uno
// keeps a separate pricing catalog, openrouter quotes on every row.
func routePrice(backend, id string) (float64, bool) {
	switch backend {
	case "uno":
		if in, out, ok := unoPriceFor(id); ok {
			return in + out, true
		}
	case "openrouter":
		if in, out, ok := orPrice1M("openrouter/" + id); ok {
			return in + out, true
		}
	}
	return 0, false
}

// routeRank is one candidate's tier inside its provider block: 0 free, 2 paid
// with a published price, 3 paid with no price. cheaper wins at 2, so a priced
// row is always asked before an unpriced one and the cheapest priced row goes
// first. it only ever compares rows of one provider: the provider ladder is
// the heavier term (routeBackendRank), so a free row on a later backend never
// jumps a paid row on an earlier one.
func routeRank(cand string) (int, float64) {
	backend, id, _ := strings.Cut(cand, "/")
	if routeFree(backend, id) {
		return 0, 0
	}
	if p, ok := routePrice(backend, id); ok {
		return 2, p
	}
	return 3, 0
}

// routeCandidates resolves route/<key> to attempt ids in strategy order:
// "backend/id" with the backend's exact spelling, free rows first and priced
// rows cheapest first.
func routeCandidates(p *Pool, key string) []string {
	if !routeEnabled() {
		return nil
	}
	t := routeTableFor(p)
	m, ok := t[strings.ToLower(strings.TrimSpace(key))]
	if !ok {
		// no route row for this key: the wide family below decides, so an old
		// config naming a spelling that never merged
		// (route/nemotron-3.5-lightning-free) still lands on the exact model
		// it points at.
		return wrouteCandidates(p, key)
	}
	order := append([]string{}, routeBackends...)
	switch strings.ToLower(strings.TrimSpace(cfg().Route.Strategy)) {
	case "latency":
		sort.SliceStable(order, func(i, j int) bool {
			return routeLatency(order[i]) < routeLatency(order[j])
		})
	case "random":
		rand.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	case "race", "priority", "":
	default:
	}
	var out []string
	for _, b := range order {
		id, ok := m[b]
		if !ok || !routeBackendLive(b, p) {
			continue
		}
		out = append(out, b+"/"+id)
	}
	if len(out) == 0 {
		// every backend that served this key is down: widen to the family
		// rather than 404 a key that exists.
		return wrouteCandidates(p, key)
	}
	// race still needs the ordered list: the racer fans out over it.
	return out
}

// routeEnabled gates the whole feature.
func routeEnabled() bool {
	return cfg().Route.Enabled
}

// routeLatencyMs tracks per-backend success latency for the latency strategy.
var routeLatencyMs struct {
	sync.Mutex
	ms map[string]float64
}

func routeLatency(backend string) float64 {
	routeLatencyMs.Lock()
	defer routeLatencyMs.Unlock()
	if routeLatencyMs.ms == nil {
		return 0
	}
	return routeLatencyMs.ms[backend]
}

// noteRouteLatency folds one success sample into the backend ewma.
func noteRouteLatency(backend string, d time.Duration) {
	ms := float64(d.Milliseconds())
	if ms <= 0 {
		return
	}
	routeLatencyMs.Lock()
	if routeLatencyMs.ms == nil {
		routeLatencyMs.ms = map[string]float64{}
	}
	prev := routeLatencyMs.ms[backend]
	if prev == 0 {
		routeLatencyMs.ms[backend] = ms
	} else {
		routeLatencyMs.ms[backend] = prev*0.7 + ms*0.3
	}
	routeLatencyMs.Unlock()
}

// routeBufLimit caps one buffered candidate response. route/ replays whole
// bodies (it never streams), so an unbounded upstream response would be
// held entirely in RAM; past the cap the candidate fails and the next one
// gets a turn.
const routeBufLimit = 32 << 20

var errRouteTooLarge = errors.New("route: response exceeds buffer limit")

// routeTTFT and wrouteTTFT bound how long one candidate may go without
// answering at all: a model that has produced nothing when the window closes
// is abandoned for the next one instead of holding the request. wroute is the
// wide fan-out over one line's patches, so it cuts earlier (3s) to keep the
// ladder cheap; a plain route row gives the model 6s.
const (
	routeTTFT  = 6 * time.Second
	wrouteTTFT = 3 * time.Second
)

// routeTTFTFor is the first-response window for one ladder name.
func routeTTFTFor(name string) time.Duration {
	if name == "wroute" {
		return wrouteTTFT
	}
	return routeTTFT
}

// routeUsageKey carries the model a route attempt actually landed on, so its
// usage counts against that model instead of the route id the client sent.
type routeUsageKey struct{}

// withRouteUsage labels attempts with the model they resolved to.
func withRouteUsage(ctx context.Context, model string) context.Context {
	if model == "" {
		return ctx
	}
	return context.WithValue(ctx, routeUsageKey{}, model)
}

// usageModel is the model a usage row belongs to: the routed model when the
// request came through route/, else the client's own id.
func usageModel(r *http.Request, clientModel string) string {
	if m, ok := r.Context().Value(routeUsageKey{}).(string); ok && m != "" {
		return m
	}
	return clientModel
}

// routeRecorder buffers one backend attempt: headers, status, body. flush
// replays it onto the real writer when the attempt wins.
type routeRecorder struct {
	header http.Header
	body   bytes.Buffer
	status int
	// answering flips once the attempt has something to replay (a header, a
	// status or a byte), which is what the ttft watchdog watches.
	answering atomic.Bool
	// aborted marks an attempt the watchdog gave up on: whatever it recorded
	// is a miss, never a win.
	aborted atomic.Bool
	// set by a completed attempt so the race path can record only the
	// winner's latency.
	label   string
	elapsed time.Duration
}

func newRouteRecorder() *routeRecorder {
	return &routeRecorder{header: http.Header{}}
}

func (r *routeRecorder) Header() http.Header { return r.header }

func (r *routeRecorder) WriteHeader(s int) {
	r.answering.Store(true)
	r.status = s
}

func (r *routeRecorder) Flush() {}

// Write buffers, marking implicit 200 like net/http does when handlers
// never call WriteHeader explicitly.
func (r *routeRecorder) Write(b []byte) (int, error) {
	r.answering.Store(true)
	if r.body.Len()+len(b) > routeBufLimit {
		r.status = http.StatusBadGateway
		return 0, errRouteTooLarge
	}
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(b)
}

func (r *routeRecorder) won() bool { return r.status == http.StatusOK && !r.aborted.Load() }

func (r *routeRecorder) flush(w http.ResponseWriter) {
	for k, v := range r.header {
		w.Header()[k] = v
	}
	if r.status == 0 {
		r.status = http.StatusOK
	}
	w.WriteHeader(r.status)
	w.Write(r.body.Bytes())
}

// routeTry runs fn against a recorder; a 200 replays onto w and wins. an
// attempt that answers nothing within ttft is cancelled and counts as a miss,
// so the ladder tries the next candidate instead of waiting out this one.
func routeTry(ctx context.Context, w http.ResponseWriter, ttft time.Duration, fn func(context.Context, http.ResponseWriter) bool) bool {
	if ctx.Err() != nil {
		return false
	}
	rec := newRouteRecorder()
	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	if ttft > 0 {
		done := make(chan struct{})
		defer close(done)
		go func() {
			t := time.NewTimer(ttft)
			defer t.Stop()
			select {
			case <-t.C:
				if !rec.answering.Load() {
					rec.aborted.Store(true)
					cancel()
				}
			case <-done:
			}
		}()
	}
	fn(actx, rec)
	if !rec.won() {
		return false
	}
	rec.flush(w)
	return true
}

// handleRoute serves route/<key> on openai-compat paths: resolve candidates,
// try each in order (buffered, first 200 replays), or race them all at once.
func (p *Pool) handleRoute(w http.ResponseWriter, r *http.Request, body []byte, model, key string, isStream bool, start time.Time, reqID string) {
	p.serveRoute(w, r, body, model, "route", key, routeCandidates(p, key), isStream, start, reqID, cfg().Route.Strategy, true)
}

// handleWRoute serves wroute/<key>, route's wide sibling. candidates are exact
// models, so the per-backend retry budget does not apply (each candidate is
// already a different model) and race stays off: it would fire one upstream
// call per family member on every request.
func (p *Pool) handleWRoute(w http.ResponseWriter, r *http.Request, body []byte, model, key string, isStream bool, start time.Time, reqID string) {
	p.serveRoute(w, r, body, model, "wroute", key, wrouteCandidates(p, key), isStream, start, reqID, "", false)
}

// serveRoute runs one resolved candidate list under name: the sequential
// ladder (repeat adds the per-backend retry budget) or the race fan-out.
func (p *Pool) serveRoute(w http.ResponseWriter, r *http.Request, body []byte, model, name, key string, cands []string, isStream bool, start time.Time, reqID string, strategy string, repeat bool) {
	if len(cands) == 0 {
		acclog.Printf("<- 404 %s %s model=%s (no %s backends serve %q)", r.Method, r.URL.Path, model, name, key)
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("%s: no backends serve %q", name, key))
		return
	}
	ttft := routeTTFTFor(name)
	try := func(w http.ResponseWriter, cand string) bool {
		backend, id, _ := strings.Cut(cand, "/")
		nb, err := setReqModel(body, cand)
		if err != nil {
			return false
		}
		var dt time.Duration
		won := routeTry(r.Context(), w, ttft, func(ctx context.Context, rw http.ResponseWriter) bool {
			t0 := time.Now()
			p.dispatchRouteOAI(rw, r.WithContext(ctx), nb, cand, backend, id, isStream, start, reqID)
			dt = time.Since(t0)
			return true
		})
		// winners only: a failed attempt's latency must not poison ranking.
		if won {
			noteRouteLatency(backend, dt)
		}
		return won
	}
	if strings.EqualFold(strings.TrimSpace(strategy), "race") && len(cands) > 1 {
		fns := make([]func(context.Context, http.ResponseWriter) bool, 0, len(cands))
		for _, cand := range cands {
			cand := cand
			fns = append(fns, func(ctx context.Context, rw http.ResponseWriter) bool {
				backend, id, _ := strings.Cut(cand, "/")
				nb, err := setReqModel(body, cand)
				if err != nil {
					return false
				}
				rr := r.WithContext(ctx)
				t0 := time.Now()
				p.dispatchRouteOAI(rw, rr, nb, cand, backend, id, isStream, start, reqID)
				// only the winner's latency is recorded: a loser's pollutes
				// the ranking, and which attempt won is not known until the
				// race settles.
				if rec, ok := rw.(*routeRecorder); ok && rec.won() {
					rec.label, rec.elapsed = backend, time.Since(t0)
				}
				return true
			})
		}
		if routeRace(r.Context(), w, fns) {
			return
		}
	} else {
		list := cands
		if repeat {
			list = expandRouteAttempts(cands)
		}
		for _, cand := range list {
			if try(w, cand) {
				acclog.Printf("%s %s=%s won on %s", reqID, name, key, cand)
				return
			}
		}
	}
	acclog.Printf("<- 502 %s %s model=%s (%s %q: all %d backends failed)", r.Method, r.URL.Path, model, name, key, len(cands))
	writeJSONError(w, http.StatusBadGateway, fmt.Sprintf("%s: all backends failed for %q", name, key))
}

// dispatchRouteOAI runs one candidate on openai-compat paths.
func (p *Pool) dispatchRouteOAI(w http.ResponseWriter, r *http.Request, body []byte, cand, backend, id string, isStream bool, start time.Time, reqID string) {
	switch backend {
	case "opencode":
		p.concurrentZen.Add(1)
		defer p.concurrentZen.Add(-1)
		p.handleOpenCode(w, r, body, cand, isStream, start, reqID)
	case "freepi":
		p.handleFreepi(w, r, body, cand, isStream, start, reqID)
	case "antseed":
		p.handleAntseed(w, r, body, cand, isStream, start, reqID)
	case "uno", "openrouter", "orcarouter", "nous": // keyed handlers expect the bare id in and on the body
		nb := body
		if b, err := setReqModel(body, id); err == nil {
			nb = b
		}
		switch backend {
		case "uno":
			p.keyedChat(w, r, nb, id, isStream, start, reqID, unoBackend)
		case "openrouter":
			p.keyedChat(w, r, nb, id, isStream, start, reqID, orChatBackend)
		case "orcarouter":
			p.keyedChat(w, r, nb, id, isStream, start, reqID, orcaBackend)
		default:
			p.handleNous(w, r, nb, id, isStream, start, reqID)
		}
	default: // nvidia: strip the listing prefix, keep the bare id upstream
		nb := body
		if b, err := setReqModel(body, id); err == nil {
			nb = b
		}
		p.handleNvidia(w, r, nb, id, isStream, start, reqID)
	}
}

// handleRouteAnthropic serves route/<key> on /v1/messages: same candidate
// order, anthropic-shaped handlers, buffered so fallback can retry.
func (p *Pool) handleRouteAnthropic(w http.ResponseWriter, r *http.Request, body, oaiBody []byte, clientModel, key string, tnames toolNames, isStream bool, start time.Time, reqID string) {
	p.serveRouteAnthropic(w, r, body, oaiBody, clientModel, "route", key, routeCandidates(p, key), tnames, isStream, start, reqID, cfg().Route.Strategy, true)
}

// handleWRouteAnthropic serves wroute/<key> on /v1/messages.
func (p *Pool) handleWRouteAnthropic(w http.ResponseWriter, r *http.Request, body, oaiBody []byte, clientModel, key string, tnames toolNames, isStream bool, start time.Time, reqID string) {
	p.serveRouteAnthropic(w, r, body, oaiBody, clientModel, "wroute", key, wrouteCandidates(p, key), tnames, isStream, start, reqID, "", false)
}

// serveRouteAnthropic runs one resolved candidate list under name.
func (p *Pool) serveRouteAnthropic(w http.ResponseWriter, r *http.Request, body, oaiBody []byte, clientModel, name, key string, cands []string, tnames toolNames, isStream bool, start time.Time, reqID string, strategy string, repeat bool) {
	if len(cands) == 0 {
		acclog.Printf("<- 404 POST /v1/messages model=%s (no %s backends serve %q)", clientModel, name, key)
		writeAnthropicError(w, http.StatusNotFound, "not_found_error", name+": no backends serve "+key)
		return
	}
	ttft := routeTTFTFor(name)
	try := func(cand string) bool {
		backend, id, _ := strings.Cut(cand, "/")
		nb, err := setReqModel(oaiBody, cand)
		if err != nil {
			return false
		}
		var dt time.Duration
		won := routeTry(r.Context(), w, ttft, func(ctx context.Context, rw http.ResponseWriter) bool {
			t0 := time.Now()
			p.dispatchRouteAnthropic(rw, r.WithContext(ctx), body, nb, clientModel, cand, backend, id, tnames, isStream, start, reqID)
			dt = time.Since(t0)
			return true
		})
		if won {
			noteRouteLatency(backend, dt)
		}
		return won
	}
	if strings.EqualFold(strings.TrimSpace(strategy), "race") && len(cands) > 1 {
		fns := make([]func(context.Context, http.ResponseWriter) bool, 0, len(cands))
		for _, cand := range cands {
			cand := cand
			fns = append(fns, func(ctx context.Context, rw http.ResponseWriter) bool {
				backend, id, _ := strings.Cut(cand, "/")
				nb, err := setReqModel(oaiBody, cand)
				if err != nil {
					return false
				}
				rr := r.WithContext(ctx)
				t0 := time.Now()
				p.dispatchRouteAnthropic(rw, rr, body, nb, clientModel, cand, backend, id, tnames, isStream, start, reqID)
				// only the winner's latency is recorded: a loser's pollutes
				// the ranking, and which attempt won is not known until the
				// race settles.
				if rec, ok := rw.(*routeRecorder); ok && rec.won() {
					rec.label, rec.elapsed = backend, time.Since(t0)
				}
				return true
			})
		}
		if routeRace(r.Context(), w, fns) {
			return
		}
	} else {
		list := cands
		if repeat {
			list = expandRouteAttempts(cands)
		}
		for _, cand := range list {
			if try(cand) {
				acclog.Printf("%s %s=%s won on %s", reqID, name, key, cand)
				return
			}
		}
	}
	acclog.Printf("<- 502 POST /v1/messages model=%s (%s %q: all %d backends failed)", clientModel, name, key, len(cands))
	writeAnthropicError(w, http.StatusBadGateway, "api_error", name+": all backends failed for "+key)
}

// dispatchRouteAnthropic runs one candidate on the messages path.
func (p *Pool) dispatchRouteAnthropic(w http.ResponseWriter, r *http.Request, body, oaiBody []byte, clientModel, cand, backend, id string, tnames toolNames, isStream bool, start time.Time, reqID string) {
	// these handlers log usage under the client's model: point the row at the
	// model this attempt resolved to, so a routed request counts on it.
	r = r.WithContext(withRouteUsage(r.Context(), cand))
	switch backend {
	case "opencode":
		p.concurrentZen.Add(1)
		defer p.concurrentZen.Add(-1)
		p.handleOpenCodeAnthropic(w, r, body, oaiBody, clientModel, cand, tnames, isStream, start, reqID)
	case "freepi":
		p.handleFreepiAnthropic(w, r, oaiBody, clientModel, cand, tnames, isStream, start, reqID)
	case "antseed":
		p.handleAntseedAnthropic(w, r, oaiBody, clientModel, cand, tnames, isStream, start, reqID)
	case "uno", "openrouter", "orcarouter", "nous": // keyed handlers expect the bare id in and on the body
		nb := oaiBody
		if b, err := setReqModel(oaiBody, id); err == nil {
			nb = b
		}
		switch backend {
		case "uno":
			p.keyedChatAnthropic(w, r, nb, clientModel, id, tnames, isStream, start, reqID, unoBackend)
		case "openrouter":
			p.keyedChatAnthropic(w, r, nb, clientModel, id, tnames, isStream, start, reqID, orChatBackend)
		case "orcarouter":
			p.keyedChatAnthropic(w, r, nb, clientModel, id, tnames, isStream, start, reqID, orcaBackend)
		default:
			p.handleNousAnthropic(w, r, nb, clientModel, id, tnames, isStream, start, reqID)
		}
	default: // nvidia: strip the listing prefix, keep the bare id upstream
		nb := oaiBody
		if b, err := setReqModel(oaiBody, id); err == nil {
			nb = b
		}
		p.handleNvidiaAnthropic(w, r, nb, body, clientModel, id, tnames, isStream, start, reqID)
	}
}

// appendRouteModels lists route/<key> for every table key across backends.
func appendRouteModels(p *Pool, out *struct {
	Data []openRouterModel `json:"data"`
}, seenIDs map[string]bool) {
	if !routeEnabled() {
		return
	}
	t := routeTableFor(p)
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		id := "route/" + k
		if seenIDs[id] {
			continue
		}
		seenIDs[id] = true
		// describe via the first candidate's catalog entry
		infoID := ""
		for _, b := range routeBackends {
			if m, ok := t[k][b]; ok {
				infoID = b + "/" + m
				break
			}
		}
		if infoID == "" {
			infoID = id
		}
		out.Data = append(out.Data, infoModel(id, infoID))
	}
}

// routeRow is one /status route entry: the id, which backends serve it, and
// the exact model behind each of them in attempt order.
type routeRow struct {
	ID     string   `json:"id"`
	Via    []string `json:"via"`
	Models []string `json:"models,omitempty"`
}

// routeSummary is the /status view: strategy, per-backend latency, routes.
type routeSummary struct {
	Enabled  bool              `json:"enabled"`
	Strategy string            `json:"strategy"`
	Latency  map[string]string `json:"latency_ms,omitempty"`
	Routes   []routeRow        `json:"routes,omitempty"`
	Count    int               `json:"count"`
}

// routeSnapshot builds the status section behind status.show_route.
func routeSnapshot(p *Pool) *routeSummary {
	sum := &routeSummary{Enabled: routeEnabled(), Strategy: cfg().Route.Strategy}
	routeLatencyMs.Lock()
	for b, ms := range routeLatencyMs.ms {
		if sum.Latency == nil {
			sum.Latency = map[string]string{}
		}
		sum.Latency[b] = floatStr(ms)
	}
	routeLatencyMs.Unlock()
	if !routeEnabled() {
		return sum
	}
	t := routeTableFor(p)
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		var via, models []string
		for _, b := range routeBackends {
			if id, ok := t[k][b]; ok {
				via = append(via, b)
				models = append(models, b+"/"+id)
			}
		}
		// the row states the attempt order, so it shows what the request takes:
		// the provider ladder, exactly as routeCandidates walks it.
		sum.Routes = append(sum.Routes, routeRow{ID: "route/" + k, Via: via, Models: models})
	}
	sum.Count = len(sum.Routes)
	return sum
}

// floatStr renders ms compactly for /status.
func floatStr(f float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.1f", f), "0"), ".")
}

// routeRace runs every candidate at once; the first 200 replays onto w and
// the rest are cancelled. losers already burned upstream inference, same
// cost warning as the zen lane race: opt in via strategy race only.
func routeRace(ctx context.Context, w http.ResponseWriter, fns []func(context.Context, http.ResponseWriter) bool) bool {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	wins := make(chan *routeRecorder, 1)
	var wg sync.WaitGroup
	for _, fn := range fns {
		wg.Add(1)
		go func(fn func(context.Context, http.ResponseWriter) bool) {
			defer wg.Done()
			rec := newRouteRecorder()
			if fn(ctx, rec) && rec.won() {
				select {
				case wins <- rec:
				default:
				}
			}
		}(fn)
	}
	go func() {
		wg.Wait()
		close(wins)
	}()
	rec, ok := <-wins
	if !ok || rec == nil {
		return false
	}
	if rec.label != "" {
		noteRouteLatency(rec.label, rec.elapsed)
	}
	rec.flush(w)
	return ok
}
