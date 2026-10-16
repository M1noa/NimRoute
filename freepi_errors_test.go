package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// freepi error policy: the account fault classes another account or a
// resend can fix rotate; everything else forwards to the client with its
// own status and detail instead of a deadline-long retry storm. owner:
// freepiFetch error classification (freepiRetryable).
func TestFreepiRetryableClassification(t *testing.T) {
	cases := []struct {
		name string
		fe   freepiErr
		want bool
	}{
		{"account rate limit", freepiErr{Status: 429, Code: "rate_limited"}, true},
		{"stale lease", freepiErr{Status: 409, Code: "concurrent_session"}, true},
		{"ads gate", freepiErr{Status: 403, Code: "ads_required"}, true},
		{"plain 429", freepiErr{Status: 429}, true},
		{"hourly cap on a 429", freepiErr{Status: 429, Code: "hourly_ceiling"}, false},
		{"bad request is ours", freepiErr{Status: 400, Msg: "bad body"}, false},
		{"bad jwt", freepiErr{Status: 401, Code: "unauthorized"}, false},
		{"unknown model", freepiErr{Status: 404, Code: "model_not_found"}, false},
		{"server-side exec", freepiErr{Status: 500, Code: "exec_failed"}, false},
		{"upstream dead", freepiErr{Status: 502, Msg: "bad gateway"}, false},
	}
	for _, tc := range cases {
		if got := freepiRetryable(tc.fe); got != tc.want {
			t.Errorf("%s: freepiRetryable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// a hard error must reach the caller on the first strike, not after a
// queue-wait of retries: one upstream request, real status + detail.
func resetFreepiAccountsForTest() func() {
	freepiMu.Lock()
	freepiAccts = map[string]*freepiAcct{}
	freepiMu.Unlock()
	return func() {
		freepiMu.Lock()
		freepiAccts = map[string]*freepiAcct{}
		freepiMu.Unlock()
	}
}

func TestFreepiHardErrorForwardsImmediately(t *testing.T) {
	c := testConfig()
	c.Freepi.QueueWaitSecs = 5
	c.Freepi.Accounts = []freepiAccount{{Name: "t1", JWT: "fake"}}
	useConfig(t, c)
	defer resetFreepiAccountsForTest()()

	var hits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ad-cycle telemetry rides the same server: only the inference
		// endpoint counts as an attempt.
		if strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			atomic.AddInt32(&hits, 1)
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"unauthorized","message":"jwt expired"}`))
	}))
	defer up.Close()
	oldBase := freepiBase()
	defer freePiBase.Store(oldBase)
	freePiBase.Store(up.URL)

	start := time.Now()
	_, _, _, fault, ok := freepiFetch(context.Background(), http.MethodPost,
		[]byte(`{"model":"m","messages":[]}`), "m")
	if ok {
		t.Fatal("401 upstream must not report success")
	}
	if fault.Status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 forwarded", fault.Status)
	}
	if !strings.Contains(fault.Msg, "jwt expired") || !strings.Contains(fault.Msg, "unauthorized") {
		t.Fatalf("detail lost: %q", fault.Msg)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("upstream hit %d times, want exactly 1 (no retry storm)", n)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("hard error took %v, want immediate forward", d)
	}
}

// the cap maps on the parsed code, never on the upstream wording: a reworded
// message cannot drop the 503, and a message that merely mentions the cap
// without the code cannot invent one. owner: freepiClientStatus, which
// consumes the fault freepiFetch returns.
func TestFreepiClientStatusKeysOnCode(t *testing.T) {
	cases := []struct {
		name   string
		fault  freepiFault
		want   int
		sub    string
		notSub string
	}{
		{"cap on 429", freepiFault{Status: 429, Code: "hourly_ceiling", Msg: "out of completions"}, 503, "hourly completions", ""},
		{"cap on 500", freepiFault{Status: 500, Code: "hourly_ceiling", Msg: "quota"}, 503, "hourly completions", ""},
		{"plain rate limit", freepiFault{Status: 429, Msg: "slow down"}, 503, "busy or limited", ""},
		{"busy conflict", freepiFault{Status: 409, Msg: "one stream per account"}, 503, "busy or limited", ""},
		{"cap only in the wording", freepiFault{Status: 400, Msg: "hourly_ceiling reached"}, 400, "hourly_ceiling reached", "hourly completions"},
		{"real fault forwards", freepiFault{Status: 401, Code: "unauthorized", Msg: "jwt expired"}, 401, "jwt expired", "proxy:"},
	}
	for _, tc := range cases {
		gotStatus, gotMsg := freepiClientStatus(tc.fault)
		if gotStatus != tc.want {
			t.Errorf("%s: status = %d, want %d", tc.name, gotStatus, tc.want)
		}
		if !strings.Contains(gotMsg, tc.sub) {
			t.Errorf("%s: msg = %q, want it to name %q", tc.name, gotMsg, tc.sub)
		}
		if tc.notSub != "" && strings.Contains(gotMsg, tc.notSub) {
			t.Errorf("%s: msg = %q, must not carry %q", tc.name, gotMsg, tc.notSub)
		}
	}
}

// an hourly-capped account fails fast and never leaks its 429 to the client:
// the fetch stops on the first strike and the cap becomes a 503 naming
// itself. owner: the freepi rotation contract in AGENTS.md.
func TestFreepiHourlyCeilingNoRetryNoForward(t *testing.T) {
	c := testConfig()
	c.Freepi.QueueWaitSecs = 5
	c.Freepi.Accounts = []freepiAccount{{Name: "t1", JWT: "fake"}}
	useConfig(t, c)
	defer resetFreepiAccountsForTest()()

	var hits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			atomic.AddInt32(&hits, 1)
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"code":"hourly_ceiling","message":"out of completions for this hour"}`))
	}))
	defer up.Close()
	oldBase := freepiBase()
	defer freePiBase.Store(oldBase)
	freePiBase.Store(up.URL)

	start := time.Now()
	_, _, _, fault, ok := freepiFetch(context.Background(), http.MethodPost,
		[]byte(`{"model":"m","messages":[]}`), "m")
	if ok {
		t.Fatal("rate-limited upstream must not report success")
	}
	if fault.Status != http.StatusTooManyRequests || fault.Code != "hourly_ceiling" {
		t.Fatalf("fault = %+v, want the 429 hourly_ceiling envelope", fault)
	}
	if !strings.Contains(fault.Msg, "out of completions") || !strings.Contains(fault.Msg, "hourly_ceiling") {
		t.Fatalf("detail lost: %q", fault.Msg)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("upstream hit %d times, want exactly 1 (the hourly cap is not retryable)", n)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("hourly cap took %v, want fail-fast", d)
	}
	status, msg := freepiClientStatus(fault)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("client status = %d, want 503 (a rate limit is never forwarded)", status)
	}
	if !strings.Contains(msg, "hourly completions") {
		t.Fatalf("cap not named for the client: %q", msg)
	}
}

// a sibling account still gets its one honest attempt after the first
// account hard-fails: a bad jwt on t1 must not stop a healthy t2.
func TestFreepiHardErrorSoloAttemptsSibling(t *testing.T) {
	c := testConfig()
	c.Freepi.QueueWaitSecs = 5
	c.Freepi.Accounts = []freepiAccount{{Name: "t1", JWT: "fake"}, {Name: "t2", JWT: "fake2"}}
	useConfig(t, c)
	defer resetFreepiAccountsForTest()

	var hits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			// ad-cycle telemetry: answer, but do not count or rescue
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if atomic.AddInt32(&hits, 1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"unauthorized","message":"jwt expired"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"pong"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer up.Close()
	oldBase := freepiBase()
	defer freePiBase.Store(oldBase)
	freePiBase.Store(up.URL)

	_, rb, _, _, ok := freepiFetch(context.Background(), http.MethodPost,
		[]byte(`{"model":"m","messages":[]}`), "m")
	if !ok {
		t.Fatalf("sibling account must rescue the fetch: %.100s", rb)
	}
	var got map[string]any
	if err := json.Unmarshal(rb, &got); err != nil || got["id"] != "1" {
		t.Fatalf("bad body from sibling: %.200s", rb)
	}
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Fatalf("upstream hit %d times, want 2 (one honest attempt each)", n)
	}
}
