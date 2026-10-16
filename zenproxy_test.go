package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// A 429 or a 403 on GET /models means the exit is busy, not gone, so the
// sweep has to keep the two apart from every other rejection. that tally is
// what turns "the pool came back small" into "the gate started refusing".
func TestVerifyProxyClassifiesUpstreamStatus(t *testing.T) {
	var code atomic.Int64
	var body atomic.Pointer[string]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if code.Load() == http.StatusOK {
			io.WriteString(w, *body.Load())
			return
		}
		w.WriteHeader(int(code.Load()))
	}))
	defer srv.Close()

	old := OpencodeBase
	OpencodeBase = srv.URL
	t.Cleanup(func() { OpencodeBase = old })

	cases := []struct {
		name string
		code int
		body string
		want verifyResult
	}{
		{"200 model list verifies", http.StatusOK, `{"object":"list","data":[]}`, verifyOK},
		{"200 foreign body does not", http.StatusOK, `{"nope":1}`, verifyOther},
		{"429 is a rate limit", http.StatusTooManyRequests, "", verifyRateLimited},
		{"403 is the free-tier gate", http.StatusForbidden, "", verifyFreeTier},
		{"502 is an unexplained rejection", http.StatusBadGateway, "", verifyOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.body
			code.Store(int64(tc.code))
			body.Store(&b)
			if got := verifyProxy(""); got != tc.want {
				t.Fatalf("verifyProxy on %d = %d, want %d", tc.code, got, tc.want)
			}
		})
	}
}

// One sweep mixes every answer into a single tally. each bucket holding
// exactly one count is the shape the log line reports.
func TestVerifyMixCountsSweepOutcomes(t *testing.T) {
	stub := func(code int, body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if code == http.StatusOK && body != "" {
				io.WriteString(w, body)
				return
			}
			w.WriteHeader(code)
		}))
	}
	ok := stub(http.StatusOK, `{"object":"list","data":[]}`)
	defer ok.Close()
	limited := stub(http.StatusTooManyRequests, "")
	defer limited.Close()
	gated := stub(http.StatusForbidden, "")
	defer gated.Close()
	other := stub(http.StatusBadGateway, "")
	defer other.Close()
	// a closed listener is the dial-error case: nothing answers at all.
	dead := stub(http.StatusOK, "")
	deadURL := dead.URL
	dead.Close()

	old := OpencodeBase
	t.Cleanup(func() { OpencodeBase = old })

	var mix verifyMix
	for _, tc := range []struct {
		base string
		want verifyResult
	}{
		{ok.URL, verifyOK},
		{limited.URL, verifyRateLimited},
		{gated.URL, verifyFreeTier},
		{other.URL, verifyOther},
		{deadURL, verifyDialErr},
	} {
		OpencodeBase = tc.base
		r := verifyProxy("")
		if r != tc.want {
			t.Fatalf("verifyProxy = %d, want %d", r, tc.want)
		}
		mix.add(r)
	}

	if mix.rateLimited != 1 || mix.freeTier != 1 || mix.other != 1 || mix.dialErr != 1 {
		t.Fatalf("mix = %+v, want one of each", mix)
	}
}
