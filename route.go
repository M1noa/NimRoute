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
	"time"
)

// route backend: one name fans out across providers in strategy order.
// route/<key> tries opencode, antseed, freepi, nvidia, uno, openrouter,
// nous in turn (or by strategy), first 200 wins. keys match strictly:
// lowercase, one trailing -free stripped from opencode ids, one trailing
// :free stripped from uno/openrouter ids, nvidia ids keep the org prefix.

// routeBackends is the fixed priority order.
var routeBackends = []string{"opencode", "antseed", "freepi", "nvidia",
	"uno", "openrouter", "nous"}

// routeKey normalizes one backend model id to its route name: lowercase,
// opencode's single trailing -free dropped. nvidia ids keep the org slash,
// so "route/moonshotai/kimi-k3" stays exact.
func routeKey(backend, id string) string {
	k := strings.ToLower(strings.TrimSpace(id))
	switch backend {
	case "opencode": // opencode lists both base and -free ids for one model
		k = strings.TrimSuffix(k, "-free")
	case "uno", "openrouter": // keyed catalogs carry vendor/model:free rows
		k = strings.TrimSuffix(k, ":free")
	}
	return k
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
	case "nous":
		return nousEnabled()
	}
	return false
}

// routeTable maps route key -> backend -> exact backend model id.
type routeTable map[string]map[string]string

// routeTableTTL is how long one built table stays fresh. /status polls at 1hz
// and every build unions each live backend's catalog, so the table is reused
// instead of rebuilt per request.
const routeTableTTL = 2 * time.Second

// routeTableCache holds the last built table per pool. the entry is pinned to
// the config pointer, so a hot reload rebuilds at once.
var routeTableCache struct {
	sync.Mutex
	t   routeTable
	cfg *appConfig
	p   *Pool
	at  time.Time
}

// routeTableFor returns the cached table, rebuilding at most once per ttl.
func routeTableFor(p *Pool) routeTable {
	c := cfg()
	routeTableCache.Lock()
	if t := routeTableCache.t; t != nil && routeTableCache.p == p && routeTableCache.cfg == c && time.Since(routeTableCache.at) < routeTableTTL {
		routeTableCache.Unlock()
		return t
	}
	routeTableCache.Unlock()
	t := buildRouteTable(p)
	routeTableCache.Lock()
	routeTableCache.t, routeTableCache.p, routeTableCache.cfg, routeTableCache.at = t, p, c, time.Now()
	routeTableCache.Unlock()
	return t
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

// buildRouteTable unions every live backend's catalog. min counts distinct
// backends per key; excluded backends and models never enter. freepi joins
// only when route.include_freepi is set (its window is smaller).
func buildRouteTable(p *Pool) routeTable {
	t := routeTable{}
	add := func(backend, id string) {
		k := routeKey(backend, id)
		if k == "" || routeExclude(k) {
			return
		}
		m := t[k]
		if m == nil {
			m = map[string]string{}
			t[k] = m
		}
		if _, ok := m[backend]; !ok {
			m[backend] = id
		}
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
	if !routeBackendOff("nous") && nousEnabled() {
		for _, m := range nousFreeModels() {
			add("nous", m.ID)
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

// routeCandidates resolves route/<key> to attempt ids in strategy order:
// "backend/id" with the backend's exact spelling.
func routeCandidates(p *Pool, key string) []string {
	if !routeEnabled() {
		return nil
	}
	t := routeTableFor(p)
	m, ok := t[strings.ToLower(strings.TrimSpace(key))]
	if !ok {
		return nil
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

// routeRecorder buffers one backend attempt: headers, status, body. flush
// replays it onto the real writer when the attempt wins.
type routeRecorder struct {
	header http.Header
	body   bytes.Buffer
	status int
	// set by a completed attempt so the race path can record only the
	// winner's latency.
	label   string
	elapsed time.Duration
}

func newRouteRecorder() *routeRecorder {
	return &routeRecorder{header: http.Header{}}
}

func (r *routeRecorder) Header() http.Header { return r.header }
func (r *routeRecorder) WriteHeader(s int)   { r.status = s }
func (r *routeRecorder) Flush()              {}

// Write buffers, marking implicit 200 like net/http does when handlers
// never call WriteHeader explicitly.
func (r *routeRecorder) Write(b []byte) (int, error) {
	if r.body.Len()+len(b) > routeBufLimit {
		r.status = http.StatusBadGateway
		return 0, errRouteTooLarge
	}
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(b)
}

func (r *routeRecorder) won() bool { return r.status == http.StatusOK }

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

// routeTry runs fn against a recorder; a 200 replays onto w and wins.
func routeTry(ctx context.Context, w http.ResponseWriter, fn func(http.ResponseWriter) bool) bool {
	rec := newRouteRecorder()
	if ctx.Err() != nil {
		return false
	}
	fn(rec)
	if !rec.won() {
		return false
	}
	rec.flush(w)
	return true
}

// handleRoute serves route/<key> on openai-compat paths: resolve candidates,
// try each in order (buffered, first 200 replays), or race them all at once.
func (p *Pool) handleRoute(w http.ResponseWriter, r *http.Request, body []byte, model, key string, isStream bool, start time.Time, reqID string) {
	cands := routeCandidates(p, key)
	if len(cands) == 0 {
		acclog.Printf("<- 404 %s %s model=%s (no route backends serve %q)", r.Method, r.URL.Path, model, key)
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("route: no backends serve %q", key))
		return
	}
	try := func(w http.ResponseWriter, cand string) bool {
		backend, id, _ := strings.Cut(cand, "/")
		nb, err := setReqModel(body, cand)
		if err != nil {
			return false
		}
		var dt time.Duration
		won := routeTry(r.Context(), w, func(rw http.ResponseWriter) bool {
			t0 := time.Now()
			p.dispatchRouteOAI(rw, r, nb, cand, backend, id, isStream, start, reqID)
			dt = time.Since(t0)
			return true
		})
		// winners only: a failed attempt's latency must not poison ranking.
		if won {
			noteRouteLatency(backend, dt)
		}
		return won
	}
	if strings.EqualFold(strings.TrimSpace(cfg().Route.Strategy), "race") && len(cands) > 1 {
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
		for _, cand := range cands {
			if try(w, cand) {
				acclog.Printf("%s route=%s won on %s", reqID, key, cand)
				return
			}
		}
	}
	acclog.Printf("<- 502 %s %s model=%s (route %q: all %d backends failed)", r.Method, r.URL.Path, model, key, len(cands))
	writeJSONError(w, http.StatusBadGateway, fmt.Sprintf("route: all backends failed for %q", key))
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
	case "uno", "openrouter", "nous": // keyed handlers expect the bare id in and on the body
		nb := body
		if b, err := setReqModel(body, id); err == nil {
			nb = b
		}
		switch backend {
		case "uno":
			p.keyedChat(w, r, nb, id, isStream, start, reqID, unoBackend)
		case "openrouter":
			p.keyedChat(w, r, nb, id, isStream, start, reqID, orChatBackend)
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
	cands := routeCandidates(p, key)
	if len(cands) == 0 {
		acclog.Printf("<- 404 POST /v1/messages model=%s (no route backends serve %q)", clientModel, key)
		writeAnthropicError(w, http.StatusNotFound, "not_found_error", "route: no backends serve "+key)
		return
	}
	try := func(cand string) bool {
		backend, id, _ := strings.Cut(cand, "/")
		nb, err := setReqModel(oaiBody, cand)
		if err != nil {
			return false
		}
		var dt time.Duration
		won := routeTry(r.Context(), w, func(rw http.ResponseWriter) bool {
			t0 := time.Now()
			p.dispatchRouteAnthropic(rw, r, body, nb, clientModel, cand, backend, id, tnames, isStream, start, reqID)
			dt = time.Since(t0)
			return true
		})
		if won {
			noteRouteLatency(backend, dt)
		}
		return won
	}
	if strings.EqualFold(strings.TrimSpace(cfg().Route.Strategy), "race") && len(cands) > 1 {
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
		for _, cand := range cands {
			if try(cand) {
				acclog.Printf("%s route=%s won on %s", reqID, key, cand)
				return
			}
		}
	}
	acclog.Printf("<- 502 POST /v1/messages model=%s (route %q: all %d backends failed)", clientModel, key, len(cands))
	writeAnthropicError(w, http.StatusBadGateway, "api_error", "route: all backends failed for "+key)
}

// dispatchRouteAnthropic runs one candidate on the messages path.
func (p *Pool) dispatchRouteAnthropic(w http.ResponseWriter, r *http.Request, body, oaiBody []byte, clientModel, cand, backend, id string, tnames toolNames, isStream bool, start time.Time, reqID string) {
	switch backend {
	case "opencode":
		p.concurrentZen.Add(1)
		defer p.concurrentZen.Add(-1)
		p.handleOpenCodeAnthropic(w, r, body, oaiBody, clientModel, cand, tnames, isStream, start, reqID)
	case "freepi":
		p.handleFreepiAnthropic(w, r, oaiBody, clientModel, cand, tnames, isStream, start, reqID)
	case "antseed":
		p.handleAntseedAnthropic(w, r, oaiBody, clientModel, cand, tnames, isStream, start, reqID)
	case "uno", "openrouter", "nous": // keyed handlers expect the bare id in and on the body
		nb := oaiBody
		if b, err := setReqModel(oaiBody, id); err == nil {
			nb = b
		}
		switch backend {
		case "uno":
			p.keyedChatAnthropic(w, r, nb, clientModel, id, tnames, isStream, start, reqID, unoBackend)
		case "openrouter":
			p.keyedChatAnthropic(w, r, nb, clientModel, id, tnames, isStream, start, reqID, orChatBackend)
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

// routeRow is one /status route entry: the id plus which backends serve it.
type routeRow struct {
	ID  string   `json:"id"`
	Via []string `json:"via"`
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
		var via []string
		for _, b := range routeBackends {
			if _, ok := t[k][b]; ok {
				via = append(via, b)
			}
		}
		sum.Routes = append(sum.Routes, routeRow{ID: "route/" + k, Via: via})
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
