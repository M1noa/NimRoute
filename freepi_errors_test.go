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
	_, _, errMsg, status, ok := freepiFetch(context.Background(), http.MethodPost,
		[]byte(`{"model":"m","messages":[]}`), "m")
	if ok {
		t.Fatal("401 upstream must not report success")
	}
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 forwarded", status)
	}
	if !strings.Contains(errMsg, "jwt expired") || !strings.Contains(errMsg, "unauthorized") {
		t.Fatalf("detail lost: %q", errMsg)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("upstream hit %d times, want exactly 1 (no retry storm)", n)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("hard error took %v, want immediate forward", d)
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

	_, rb, _, status, ok := freepiFetch(context.Background(), http.MethodPost,
		[]byte(`{"model":"m","messages":[]}`), "m")
	if !ok {
		t.Fatalf("sibling account must rescue the fetch: %d %.100s", status, rb)
	}
	var got map[string]any
	if err := json.Unmarshal(rb, &got); err != nil || got["id"] != "1" {
		t.Fatalf("bad body from sibling: %.200s", rb)
	}
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Fatalf("upstream hit %d times, want 2 (one honest attempt each)", n)
	}
}
