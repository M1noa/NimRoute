package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// resetNous clears the portal pools so a test never inherits another's.
func resetNous(t *testing.T) {
	t.Helper()
	nousSyncKeys(nil)
	nousCatMu.Lock()
	nousCat, nousCatAt = nil, time.Time{}
	nousCatMu.Unlock()
	nousCatalogBackoff.Lock()
	nousCatalogBackoff.next = time.Time{}
	nousCatalogBackoff.Unlock()
	t.Cleanup(func() { nousSyncKeys(nil) })
}

// live rows captured from https://inference-api.nousresearch.com/v1/models
// on 2026-10-08. the free set is 11 of 423.
//
// pricing carries per-modality strings, an "overrides" ARRAY and an
// "original" OBJECT; the first model reproduces both so a map[string]string
// regression fails the whole decode instead of passing quietly. free rows
// send "0.0000000000", not "0".
const nousModelsFixture = `{"data":[
{"id":"inclusionai/ling-3.1-flash","context_length":262144,
 "top_provider":{"max_completion_tokens":235929},
 "pricing":{"prompt":"0.0000000000","completion":"0.0000000000",
  "web_search":"0.0025000000","input_cache_read":"0.0000002000",
  "overrides":[{"name":"tier","pricing":{"prompt":"0.5000000000"}}],
  "original":{"prompt":"0.0000000000"}}},
{"id":"poolside/laguna-s-2.1:free","context_length":262144,
 "top_provider":{"max_completion_tokens":131072},
 "pricing":{"prompt":"0.0000000000","completion":"0.0000000000"}},
{"id":"meituan/longcat-2.0:free","context_length":1048756,
 "top_provider":{"max_completion_tokens":262144},
 "pricing":{"prompt":"0.0000000000","completion":"0.0000000000"}},
{"id":"stealth/missingno","context_length":262144,
 "top_provider":{"max_completion_tokens":131072},
 "pricing":{"prompt":"0.0000000000","completion":"0.0000000000"}},
{"id":"anthropic/claude-haiku-5.5","context_length":200000,
 "top_provider":{"max_completion_tokens":64000},
 "pricing":{"prompt":"0.0000030000","completion":"0.0000150000"}},
{"id":"perplexity/pplx-embed-v1-0.6b","context_length":512,
 "top_provider":{"max_completion_tokens":0},
 "pricing":{"prompt":"0.0000000040","completion":"0.0000000000",
  "overrides":["a","b"]}}]}`

// nousFreeModels must keep every zero-priced row and drop every paid one,
// including an embedding model whose completion price is exactly 0.
func TestNousFreeModelsFilter(t *testing.T) {
	resetNous(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(nousModelsFixture))
	}))
	defer srv.Close()

	old := nousBase
	nousBase = srv.URL
	t.Cleanup(func() { nousBase = old })

	got := fetchNousCatalog()
	want := []string{
		"inclusionai/ling-3.1-flash",
		"meituan/longcat-2.0:free",
		"poolside/laguna-s-2.1:free",
		"stealth/missingno",
	}
	if len(got) != len(want) {
		t.Fatalf("free models = %d (%v), want %d", len(got), ids(got), len(want))
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("free[%d] = %q, want %q", i, got[i].ID, id)
		}
	}
	if got[0].Context != 262144 || got[0].MaxOut != 235929 {
		t.Errorf("ctx/maxout not carried through: %+v", got[0])
	}
}

func ids(ms []nousModel) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

// the ":free" suffix is not the filter: two free ids carry none, and no paid
// id carries one. pinning both directions stops a future "simplification".
func TestNousFreeFilterIsNotTheFreeSuffix(t *testing.T) {
	resetNous(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(nousModelsFixture))
	}))
	defer srv.Close()
	old := nousBase
	nousBase = srv.URL
	t.Cleanup(func() { nousBase = old })

	for _, m := range fetchNousCatalog() {
		if strings.HasPrefix(m.ID, "anthropic/") || strings.Contains(m.ID, "embed") {
			t.Errorf("paid model %q leaked into the free list", m.ID)
		}
	}
	found := map[string]bool{}
	for _, m := range nousFreeModels() {
		found[m.ID] = true
	}
	if !found["inclusionai/ling-3.1-flash"] || !found["stealth/missingno"] {
		t.Errorf("suffix-less free models missing: %v", found)
	}
}

// an unreachable portal keeps the last good list instead of emptying it.
func TestNousCatalogKeepsLastGoodOnFailure(t *testing.T) {
	resetNous(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(nousModelsFixture))
	}))
	old := nousBase
	nousBase = srv.URL
	t.Cleanup(func() { nousBase = old })

	if len(fetchNousCatalog()) == 0 {
		t.Fatal("fixture fetch returned nothing")
	}
	srv.Close()
	// force a refetch against the dead server. arm the backoff so the
	// assertion reads the cached list instead of racing a background
	// goroutine on nousBase while this test's cleanup restores it.
	nousCatMu.Lock()
	nousCatAt = time.Now().Add(-2 * nousCatalogTTL)
	nousCatMu.Unlock()
	nousCatalogBackoff.Lock()
	nousCatalogBackoff.next = time.Now().Add(time.Hour)
	nousCatalogBackoff.Unlock()

	if len(nousFreeModels()) == 0 {
		t.Fatal("dead portal emptied the cached free list")
	}
}

// nousPriceZero: the portal sends prices as strings, and "" / junk are not free.
// free rows send "0.0000000000"; paid rows send a short decimal.
func TestNousPriceZero(t *testing.T) {
	cases := map[string]bool{
		"0":              true,
		"0.0000000000":   true,
		"0.0":            true,
		" 0.0000000000 ": true,
		"":               false,
		"0.0000000040":   false,
		"0.0000030000":   false,
		"1e-9":           false,
		"not-a-number":   false,
	}
	for in, want := range cases {
		if got := nousPriceZero(in); got != want {
			t.Errorf("nousPriceZero(%q) = %v, want %v", in, got, want)
		}
	}
}

// nousEnabled needs both the flag and at least one key.
func TestNousEnabledNeedsFlagAndKeys(t *testing.T) {
	c := testConfig()
	useConfig(t, c)
	resetNous(t)
	if nousEnabled() {
		t.Error("enabled with no keys")
	}
	nousSyncKeys(map[string]string{"main": "sk-a"})
	if !nousEnabled() {
		t.Error("disabled with a key set and the default-true flag")
	}
	c.Nous.Enabled = false
	useConfig(t, c)
	if nousEnabled() {
		t.Error("enabled with nous.enabled: false")
	}
}

// blank keys never enter the pool.
func TestNousSyncKeysDropsBlanks(t *testing.T) {
	resetNous(t)
	nousSyncKeys(map[string]string{"a": "sk-a", "b": "  ", "c": ""})
	nousKeyMu.RLock()
	n := len(nousKeys)
	nousKeyMu.RUnlock()
	if n != 1 {
		t.Fatalf("pool size = %d, want 1", n)
	}
}

// a 429 cools the key and the next pick moves off it.
func TestNousCoolKeyMovesOff429(t *testing.T) {
	resetNous(t)
	nousSyncKeys(map[string]string{"a": "sk-a", "b": "sk-b"})
	k := nousPickKey(nil)
	if k == nil {
		t.Fatal("no key available on a cold pool")
	}
	nousCoolKey(k.Name)

	// the cooled key must not come back while it is parked
	for i := 0; i < 20; i++ {
		n2 := nousPickKey(nil)
		if n2 == nil {
			t.Fatal("both keys went cold from one 429")
		}
		if n2.Name == k.Name {
			t.Fatalf("picked cooled key %q", n2.Name)
		}
	}
	// a success clears the streak: exclude the other key so the cleared one
	// is the only candidate.
	nousClearKey(k.Name)
	other := "b"
	if k.Name == "b" {
		other = "a"
	}
	if got := nousPickKey(map[string]bool{other: true}); got == nil || got.Name != k.Name {
		t.Fatalf("cleared key not selectable, got %+v", got)
	}
}

// the key pool is its own mutex domain; concurrent picks must not race.
func TestNousPickKeyConcurrent(t *testing.T) {
	resetNous(t)
	nousSyncKeys(map[string]string{"a": "sk-a", "b": "sk-b", "c": "sk-c"})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				k := nousPickKey(nil)
				if k != nil {
					nousCoolKey(k.Name)
				}
				nousClearKey(k.Name)
			}
		}()
	}
	wg.Wait()
}

// nousFwdHeaders must drop every credential and identity header, the same
// deny-list the nim path uses.
func TestNousFwdHeadersDropsCredentials(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer client-secret")
	r.Header.Set("X-Api-Key", "client-secret")
	r.Header.Set("Cookie", "session=abc")
	r.Header.Set("Origin", "https://evil.test")
	r.Header.Set("X-Session-Id", "sess-1")
	r.Header.Set("Host", "proxy.local")
	r.Header.Set("X-Custom", "keep-me")

	fwd := nousFwdHeaders(r)
	for _, banned := range []string{"Authorization", "X-Api-Key", "Cookie",
		"Origin", "X-Session-Id", "Host"} {
		if v := fwd.Get(banned); v != "" {
			t.Errorf("%s forwarded upstream: %q", banned, v)
		}
	}
	if fwd.Get("X-Custom") != "keep-me" {
		t.Error("allowlisted header was dropped")
	}
	if fwd.Get("Content-Type") != "application/json" {
		t.Error("Content-Type not defaulted")
	}
}

// an all-429 pool hands the client the portal's own 429, not a synthesized
// 503, so the client retries on the upstream's schedule.
func TestNousSendAll429Returns429(t *testing.T) {
	resetNous(t)
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer srv.Close()
	old := nousBase
	nousBase = srv.URL
	t.Cleanup(func() { nousBase = old })

	c := testConfig()
	c.NousKeys = map[string]string{"a": "sk-a", "b": "sk-b"}
	useConfig(t, c)
	nousSyncKeys(c.NousKeys)

	res := nousSend(t.Context(), "POST", srv.URL+"/chat/completions",
		[]byte(`{}`), http.Header{}, "nous/x")
	if res == nil {
		t.Fatal("nil result: a 429 must survive an exhausted pool")
	}
	if res.Status != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", res.Status)
	}
	if !res.RateLimited {
		t.Error("RateLimited not set")
	}
	if res.Header.Get("Retry-After") != "7" {
		t.Errorf("Retry-After lost: %v", res.Header)
	}
	if hits != 2 {
		t.Errorf("upstream hits = %d, want 2 (one per key)", hits)
	}
}

// a 200 clears the key and carries the body through untouched.
func TestNousSendSuccess(t *testing.T) {
	resetNous(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-a" {
			t.Errorf("Authorization = %q, want the pool key", got)
		}
		w.Write([]byte(`{"id":"x"}`))
	}))
	defer srv.Close()
	old := nousBase
	nousBase = srv.URL
	t.Cleanup(func() { nousBase = old })

	c := testConfig()
	c.NousKeys = map[string]string{"a": "sk-a"}
	useConfig(t, c)
	nousSyncKeys(c.NousKeys)
	res := nousSend(t.Context(), "POST", srv.URL+"/chat/completions",
		[]byte(`{}`), http.Header{}, "nous/x")
	if res == nil || res.Status != http.StatusOK {
		t.Fatalf("res = %+v, want a 200", res)
	}
	if string(res.Body) != `{"id":"x"}` {
		t.Errorf("body = %q", res.Body)
	}
	// a 200 clears any streak the key had accumulated
	nousKeyMu.RLock()
	streak := nousKeys[0].Consec429
	nousKeyMu.RUnlock()
	if streak != 0 {
		t.Errorf("Consec429 = %d after a 200, want 0", streak)
	}
}

// the /status section must be gated by its flag and hidden from unauthenticated
// callers, like every other credential-bearing block. pointed at a stub so the
// test never reaches the real portal through Status().
func TestNousStatusGating(t *testing.T) {
	resetNous(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(nousModelsFixture))
	}))
	defer srv.Close()
	old := nousBase
	nousBase = srv.URL
	t.Cleanup(func() { nousBase = old })

	c := testConfig()
	c.Nous.Enabled = true
	c.Auth.Tokens = []string{"secret"}
	useConfig(t, c)
	nousSyncKeys(map[string]string{"main": "sk-a"})

	p := newPool(nil)
	sr := p.StatusFor(true)
	if sr.Nous == nil {
		t.Fatal("authenticated /status dropped the nous section")
	}
	if len(sr.Nous.Keys) != 1 || sr.Nous.Keys[0].Name != "main" {
		t.Fatalf("keys = %+v", sr.Nous.Keys)
	}
	// the key material must never ride along
	b, _ := json.Marshal(sr.Nous)
	if strings.Contains(string(b), "sk-a") {
		t.Errorf("secret leaked into /status: %s", b)
	}

	sr = p.StatusFor(false)
	if sr.Nous != nil {
		t.Error("unauthenticated /status exposed the nous section")
	}

	c.Status.ShowNous = false
	useConfig(t, c)
	if p.StatusFor(true).Nous != nil {
		t.Error("show_nous: false still emitted the section")
	}
}

// nousFreeModels must never put a 10s upstream GET on the /status path a
// stale or cold cache serves what it holds and refetches in the
// background. the backoff gate is armed here so the assertion exercises
// the serve-cached path without racing a live goroutine on nousBase.
func TestNousFreeModelsDoesNotBlock(t *testing.T) {
	resetNous(t)
	nousCatalogBackoff.Lock()
	nousCatalogBackoff.next = time.Now().Add(time.Hour)
	nousCatalogBackoff.Unlock()
	// a hanging upstream: a synchronous call would block on the client
	// timeout, so a fast return proves the fetch is off this path.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	old := nousBase
	nousBase = srv.URL
	t.Cleanup(func() { nousBase = old })

	start := time.Now()
	got := nousFreeModels()
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("nousFreeModels blocked for %v on a hanging portal", d)
	}
	if len(got) != 0 {
		t.Fatalf("cold cache returned %d models, want none until a fetch lands", len(got))
	}
	// a /status poll loop must not stack fetches either.
	for i := 0; i < 5; i++ {
		start = time.Now()
		if m := nousFreeModels(); len(m) != 0 {
			t.Fatalf("poll %d returned %d models, want the cached empty list", i, len(m))
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("poll %d took %v", i, d)
		}
	}
}
