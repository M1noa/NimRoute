package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// route key owner: table normalization is the shared-model contract.
func TestRouteKey(t *testing.T) {
	cases := []struct {
		backend, id, want string
	}{
		{"opencode", "muse-spark-1.3-contributor-free", "muse-spark-1.3-contributor"},
		{"opencode", "Muse-Spark-1.3-Contributor-Free", "muse-spark-1.3-contributor"},
		{"opencode", "big-pickle", "big-pickle"},
		{"opencode", "  glm-5.3-flash-free  ", "glm-5.3-flash"},
		{"antseed", "glm-5.3-flash", "glm-5.3-flash"},
		{"freepi", "GLM-5.3-Flash", "glm-5.3-flash"},
		{"nvidia", "moonshotai/kimi-k3", "moonshotai/kimi-k3"},
		{"nvidia", "MoonshotAI/Kimi-K3", "moonshotai/kimi-k3"},
		{"nvidia", "muse-spark-free", "muse-spark-free"}, // non-opencode keeps -free
		{"opencode", "", ""},
	}
	for _, c := range cases {
		if got := routeKey(c.backend, c.id); got != c.want {
			t.Fatalf("routeKey(%q,%q) = %q, want %q", c.backend, c.id, got, c.want)
		}
	}
}

// version segments are distinct keys, never merged.
func TestRouteKeyVersionsDistinct(t *testing.T) {
	if routeKey("opencode", "muse-spark-1.3-contributor-free") == routeKey("opencode", "muse-spark-1.2-contributor-free") {
		t.Fatal("1.3 and 1.2 keys must differ")
	}
}

func TestRouteExclude(t *testing.T) {
	c := defaultConfig()
	c.Route.ExcludeModels = []string{"*muse-spark*"}
	useConfig(t, &c)
	if !routeExclude("muse-spark-1.3-contributor") {
		t.Fatal("expected spark key excluded")
	}
	if routeExclude("glm-5.3-flash") {
		t.Fatal("glm key must not be excluded")
	}
}

func TestRouteBackendOff(t *testing.T) {
	c := defaultConfig()
	c.Route.ExcludeProviders = []string{"NVIDIA"}
	useConfig(t, &c)
	if !routeBackendOff("nvidia") {
		t.Fatal("nvidia should be off (case-insensitive)")
	}
	if routeBackendOff("opencode") {
		t.Fatal("opencode should be on")
	}
}

func TestRouteCandidatesDisabled(t *testing.T) {
	c := defaultConfig()
	c.Route.Enabled = false
	useConfig(t, &c)
	p := &Pool{}
	if got := routeCandidates(p, "glm-5.3-flash"); got != nil {
		t.Fatalf("disabled route must return nil, got %v", got)
	}
	if routeEnabled() {
		t.Fatal("routeEnabled must be false")
	}
}

// route defaults owner: routes only exist when fallback is real.
func TestRouteDefaults(t *testing.T) {
	d := defaultConfig()
	if !d.Route.Enabled {
		t.Fatal("route must be enabled by default")
	}
	if d.Route.Strategy != "priority" {
		t.Fatalf("default strategy = %q, want priority", d.Route.Strategy)
	}
	if d.Route.MinOccurrences != 2 {
		t.Fatalf("default min_occurrences = %d, want 2", d.Route.MinOccurrences)
	}
	if d.Route.IncludeFreepi {
		t.Fatal("freepi must be excluded from routes by default")
	}
}

func TestRouteLatencyEWMA(t *testing.T) {
	routeLatencyMs.Lock()
	routeLatencyMs.ms = map[string]float64{}
	routeLatencyMs.Unlock()
	noteRouteLatency("opencode", 100*time.Millisecond)
	if got := routeLatency("opencode"); got != 100 {
		t.Fatalf("first sample = %v, want 100", got)
	}
	noteRouteLatency("opencode", 200*time.Millisecond)
	// ewma: 100*0.7 + 200*0.3 = 130
	if got := routeLatency("opencode"); got < 129.9 || got > 130.1 {
		t.Fatalf("ewma = %v, want 130", got)
	}
	if got := routeLatency("nvidia"); got != 0 {
		t.Fatalf("unknown backend latency = %v, want 0", got)
	}
	noteRouteLatency("antseed", 0) // non-positive samples ignored
	if got := routeLatency("antseed"); got != 0 {
		t.Fatalf("zero sample must be ignored, got %v", got)
	}
}

func TestRouteRecorderFlush(t *testing.T) {
	rec := newRouteRecorder()
	if rec.won() {
		t.Fatal("fresh recorder must not have won")
	}
	rec.Header().Set("Content-Type", "application/json")
	rec.WriteHeader(200)
	rec.Write([]byte(`{"ok":true}`))
	if !rec.won() {
		t.Fatal("200 recorder must have won")
	}
	w := httptest.NewRecorder()
	rec.flush(w)
	res := w.Result()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	if body := w.Body.String(); body != `{"ok":true}` {
		t.Fatalf("body = %q", body)
	}
}

func TestRouteRecorderDefaultStatus(t *testing.T) {
	rec := newRouteRecorder()
	rec.Write([]byte("hi"))
	w := httptest.NewRecorder()
	rec.flush(w)
	if w.Code != 200 {
		t.Fatalf("default status = %d, want 200", w.Code)
	}
}

func TestRouteTryWinLoss(t *testing.T) {
	// win replays
	w := httptest.NewRecorder()
	won := routeTry(t.Context(), w, func(rw http.ResponseWriter) bool {
		rw.Header().Set("X-Win", "1")
		rw.WriteHeader(200)
		rw.Write([]byte("yes"))
		return true
	})
	if !won {
		t.Fatal("routeTry must report win")
	}
	if w.Body.String() != "yes" || w.Header().Get("X-Win") != "1" {
		t.Fatalf("replay missing: body=%q hdr=%q", w.Body.String(), w.Header().Get("X-Win"))
	}
	// loss writes nothing
	w2 := httptest.NewRecorder()
	won = routeTry(t.Context(), w2, func(rw http.ResponseWriter) bool {
		rw.WriteHeader(502)
		rw.Write([]byte("no"))
		return true
	})
	if won {
		t.Fatal("502 must not win")
	}
	if w2.Body.String() != "" {
		t.Fatalf("loss must not replay, body=%q", w2.Body.String())
	}
}

func TestFloatStr(t *testing.T) {
	if got := floatStr(130.0); got != "130" {
		t.Fatalf("floatStr(130) = %q", got)
	}
	if got := floatStr(12.34); !strings.HasPrefix(got, "12.3") {
		t.Fatalf("floatStr(12.34) = %q", got)
	}
}

// route request path owner: a route/<key> request must never wait on a
// listing fetch. with an empty cached listing the request serves what it has
// and the fetch happens in the background.
func TestRouteCandidatesNeverWaitOnZenListing(t *testing.T) {
	oldBase := OpencodeBase
	t.Cleanup(func() { OpencodeBase = oldBase })
	prev := ocInfo.Load()
	t.Cleanup(func() { ocInfo.Store(prev) })

	release := make(chan struct{})
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		hits.Add(1)
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	OpencodeBase = srv.URL

	c := defaultConfig()
	c.Route.Enabled = true
	c.Route.Strategy = "priority"
	c.Route.MinOccurrences = 1
	c.Zen.Enabled = true
	c.Antseed.Enabled = false
	c.Freepi.Enabled = false
	c.Nvidia.Enabled = false
	useConfig(t, &c)
	ocInfo.Store(nil)

	p := &Pool{}
	done := make(chan time.Duration, 1)
	go func() {
		t0 := time.Now()
		routeCandidates(p, "glm-5.3-flash")
		done <- time.Since(t0)
	}()
	select {
	case el := <-done:
		if el > time.Second {
			t.Fatalf("routeCandidates blocked %v on the zen listing fetch", el)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("routeCandidates still blocked on the zen listing fetch after 3s")
	}

	// the one fetch belongs to the background refresh, never the request.
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for ocInfo.Load() == nil && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if ocInfo.Load() == nil {
		t.Fatal("the background refresh never cached the listing")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("listing fetches = %d, want 1 (background only)", got)
	}
}

// route table owner: one table is reused for a short ttl, so a 1hz /status
// poll or a route burst does not rebuild it every time.
func TestRouteTableCachedAcrossPolls(t *testing.T) {
	prev := ocInfo.Load()
	t.Cleanup(func() { ocInfo.Store(prev) })

	c := defaultConfig()
	c.Route.Enabled = true
	c.Route.Strategy = "priority"
	c.Route.MinOccurrences = 1
	c.Zen.Enabled = true
	c.Antseed.Enabled = false
	c.Freepi.Enabled = false
	c.Nvidia.Enabled = false
	useConfig(t, &c)

	p := &Pool{}
	ocInfo.Store(&OpencodeInfo{Enabled: true, Models: []string{"opencode/alpha-free"}})
	if got := routeCandidates(p, "alpha"); len(got) != 1 || got[0] != "opencode/alpha-free" {
		t.Fatalf("first build = %v, want [opencode/alpha-free]", got)
	}
	// a catalog change inside the ttl is not picked up: every request in the
	// window sees the table already built.
	ocInfo.Store(&OpencodeInfo{Enabled: true, Models: []string{"opencode/beta-free"}})
	if got := routeCandidates(p, "beta"); got != nil {
		t.Fatalf("table rebuilt within the ttl, got %v", got)
	}
	if got := routeCandidates(p, "alpha"); len(got) != 1 {
		t.Fatalf("cached table lost alpha: %v", got)
	}
	time.Sleep(routeTableTTL + 50*time.Millisecond)
	if got := routeCandidates(p, "beta"); len(got) != 1 || got[0] != "opencode/beta-free" {
		t.Fatalf("table did not rebuild after the ttl, got %v", got)
	}
}

// route anthropic race owner: only the winner's latency feeds the latency
// strategy ewma. a losing candidate's latency pollutes the ranking, so the
// race path labels the recorder and lets routeRace record the winner.
func TestRouteAnthropicRaceRecordsOnlyWinner(t *testing.T) {
	oldBase := nvidiaBase
	t.Cleanup(func() { nvidiaBase = oldBase })

	c := defaultConfig()
	c.Route.Enabled = true
	c.Route.Strategy = "race"
	c.Route.MinOccurrences = 1
	c.Zen.Enabled = false
	c.Antseed.Enabled = true
	c.Freepi.Enabled = false
	c.Nvidia.Enabled = true
	c.NvidiaKeys = map[string]string{"nv1": "k"}

	// antseed polls a snapshot: the loopback seller is dropped by the
	// endpoint gate, so the candidate lists but never dials.
	antStats := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"peers":[{"peerId":"aa","publicAddress":"127.0.0.1:1","providers":[{"provider":"openai","services":["race-model"],"defaultPricing":{"inputUsdPerMillion":0,"outputUsdPerMillion":0},"servicePricing":{"race-model":{"inputUsdPerMillion":0,"outputUsdPerMillion":0}},"serviceApiProtocols":{"race-model":["openai-chat-completions"]}}]}]}`))
	}))
	defer antStats.Close()
	c.Antseed.StatsURL = antStats.URL
	useConfig(t, &c)

	// applyConfig reloads the catalog: install the single race model after it.
	oldInfo := modelInfo
	setModelInfo(map[string]modelInfoEntry{"race-model": {}})
	t.Cleanup(func() { setModelInfo(oldInfo) })
	antMu2.Lock()
	oldModels, oldFetched, oldOffers := antModels, antFetched, antOffers
	antModels, antFetched, antOffers = []string{"race-model"}, time.Now(), nil
	antMu2.Unlock()
	t.Cleanup(func() {
		antMu2.Lock()
		antModels, antFetched, antOffers = oldModels, oldFetched, oldOffers
		antMu2.Unlock()
	})

	routeLatencyMs.Lock()
	routeLatencyMs.ms = map[string]float64{}
	routeLatencyMs.Unlock()

	// the ewma drops sub-millisecond samples, so the nvidia candidate has to
	// take measurable time in both phases: an instant loser would hide the
	// regression this pins.
	var win atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		if win.Load() {
			w.Write([]byte(`{"id":"1","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
			return
		}
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":"upstream down"}`))
	}))
	defer srv.Close()
	nvidiaBase = srv.URL
	p := newPool(c.NvidiaKeys)

	oai := []byte(`{"model":"race-model","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	anth := []byte(`{"model":"route/race-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	run := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		p.handleRouteAnthropic(w, req, anth, oai, "route/race-model", "race-model", nil, false, time.Now(), "test-route-race")
		return w
	}

	// every candidate loses: nothing may reach the ewma.
	if w := run(); w.Code != http.StatusBadGateway {
		t.Fatalf("all-lose race status = %d, want 502", w.Code)
	}
	if got := routeLatency("nvidia"); got != 0 {
		t.Fatalf("losing nvidia candidate recorded latency %v", got)
	}
	if got := routeLatency("antseed"); got != 0 {
		t.Fatalf("losing antseed candidate recorded latency %v", got)
	}

	// nvidia wins: its latency lands, the loser's still does not.
	win.Store(true)
	if w := run(); w.Code != http.StatusOK {
		t.Fatalf("race status = %d, want 200 (nvidia wins)", w.Code)
	}
	if got := routeLatency("nvidia"); got <= 0 {
		t.Fatal("winning nvidia candidate recorded no latency")
	}
	if got := routeLatency("antseed"); got != 0 {
		t.Fatalf("losing antseed candidate recorded latency %v", got)
	}
}
