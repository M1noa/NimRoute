package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
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
