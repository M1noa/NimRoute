package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// resetDbgFile restores the capture globals after a test toggled them.
func resetDbgFile(t *testing.T, dir string) {
	t.Helper()
	setDebugFile(true, dir)
	t.Cleanup(func() { setDebugFile(false, "") })
}

// dumpFiles lists the json files written to dir.
func dumpFiles(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read dir: %v", err)
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// TestDbgFileFlagParsing pins the -df / --dbg-file surface: the short form
// never eats a positional, and the value form moves the dump dir.
func TestDbgFileFlagParsing(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantOn  bool
		wantDir string
		rest    []string
	}{
		{"absent", []string{"probe"}, false, "", []string{"probe"}},
		{"short", []string{"-df", "probe"}, true, "", []string{"probe"}},
		{"short before port", []string{"-df", "--port", "5420"}, true, "", nil},
		{"long bare", []string{"--dbg-file"}, true, "", nil},
		{"long eq", []string{"--dbg-file=/tmp/d"}, true, "/tmp/d", nil},
		{"long spaced", []string{"--dbg-file", "/tmp/d2"}, true, "/tmp/d2", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, rest := parseFlags(tc.args)
			if on := f.set["dbgfile"]; on != tc.wantOn {
				t.Fatalf("dbgfile set = %v, want %v", on, tc.wantOn)
			}
			if f.dbgFile != tc.wantDir {
				t.Fatalf("dir = %q, want %q", f.dbgFile, tc.wantDir)
			}
			if strings.Join(rest, ",") != strings.Join(tc.rest, ",") {
				t.Fatalf("rest = %v, want %v", rest, tc.rest)
			}
		})
	}
}

// TestDbgFileDumpOnError runs one request through ServeHTTP and checks the
// dump carries the inbound and outbound sides.
func TestDbgFileDumpOnError(t *testing.T) {
	dir := t.TempDir()
	resetDbgFile(t, dir)

	// keyless nvidia: the handler 503s before touching the network, which
	// keeps the test hermetic while still exercising the whole capture path.
	applyConfig(testConfig())
	defer applyConfig(testConfig())

	p := newPool(nil)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"moonshotai/kimi-k3","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer client-token")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}

	files := dumpFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("got %d dump files, want 1", len(files))
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	var d map[string]any
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("dump is not json: %v", err)
	}
	for _, k := range []string{"in_body", "out_status", "out_body", "in_headers", "req_id", "backend"} {
		if _, ok := d[k]; !ok {
			t.Errorf("dump missing %q", k)
		}
	}
	if !strings.Contains(d["in_body"].(string), "hi") {
		t.Errorf("in_body lost the payload: %v", d["in_body"])
	}
	if d["out_status"].(float64) != 503 {
		t.Errorf("out_status = %v, want 503", d["out_status"])
	}
	if d["backend"].(string) != "nvidia" {
		t.Errorf("backend = %v, want nvidia", d["backend"])
	}
	if d["req_id"] == "" || d["req_id"] == "unknown" {
		t.Errorf("dump has no req id: %v", d["req_id"])
	}
	// the client's credential must not ride along.
	if strings.Contains(string(raw), "client-token") {
		t.Error("client bearer leaked into dump")
	}
}

// TestDbgRTRecordsHops: the transport hook captures both sides of an
// upstream call, including the response body the handler later drains.
func TestDbgRTRecordsHops(t *testing.T) {
	setDebugFile(true, t.TempDir())
	t.Cleanup(func() { setDebugFile(false, "") })

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Dbg-Fwd"); got != "kept" {
			t.Errorf("mangled forwarded header: %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "payload") {
			t.Errorf("upstream saw %q", body)
		}
		w.Header().Set("Upstream-Thing", "present")
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, `{"error":{"message":"upstream exploded"}}`)
	}))
	defer up.Close()

	tr := &traceRec{}
	ctx := context.WithValue(context.Background(), traceKey{}, tr)
	req, err := http.NewRequestWithContext(ctx, "POST", up.URL+"/v1/chat/completions", strings.NewReader(`{"messages":"payload"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer proxy-key")
	req.Header.Set("X-Dbg-Fwd", "kept")
	cl := dbgClient(&http.Client{Timeout: 5 * time.Second}, "nvidia")
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	rb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(rb), "upstream exploded") {
		t.Fatalf("upstream round trip broken: %d %s", resp.StatusCode, rb)
	}

	if len(tr.Hops) != 1 {
		t.Fatalf("hops = %d, want 1", len(tr.Hops))
	}
	hop := tr.Hops[0]
	if hop.Status != http.StatusBadGateway {
		t.Errorf("hop status = %d", hop.Status)
	}
	if !strings.Contains(hop.RespBody, "upstream exploded") {
		t.Errorf("hop resp_body = %q", hop.RespBody)
	}
	if !strings.Contains(hop.ReqBody, "payload") {
		t.Errorf("hop req_body = %q", hop.ReqBody)
	}
	if hop.RespHeaders["Upstream-Thing"] != "present" {
		t.Errorf("hop resp_headers = %v", hop.RespHeaders)
	}
	if !strings.Contains(hop.ReqHeaders["Authorization"], "redacted") {
		t.Errorf("hop req authorization not redacted: %q", hop.ReqHeaders["Authorization"])
	}
	if strings.Contains(hop.ReqHeaders["Authorization"], "proxy-key") {
		t.Error("proxy key leaked into hop")
	}
	if hop.Target != up.URL+"/v1/chat/completions" {
		t.Errorf("hop target = %q", hop.Target)
	}
	if hop.Backend != "nvidia" {
		t.Errorf("hop backend = %q", hop.Backend)
	}
}

// owner boundary: dbgRT must cap captures, never the upstream request.
func TestDbgRTLargeRequestUnchanged(t *testing.T) {
	resetDbgFile(t, t.TempDir())
	for _, size := range []int{traceUpReqCap - 1, traceUpReqCap, traceUpReqCap + 1, 1 << 20} {
		for _, replayable := range []bool{true, false} {
			t.Run(fmt.Sprintf("bytes=%d/replayable=%v", size, replayable), func(t *testing.T) {
				body := `{"input":"` + strings.Repeat("x", size-12) + `"}`
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					got, err := io.ReadAll(r.Body)
					if err != nil || string(got) != body || !json.Valid(got) {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					w.WriteHeader(http.StatusOK)
				}))
				defer up.Close()
				tr := &traceRec{}
				ctx := context.WithValue(context.Background(), traceKey{}, tr)
				req, err := http.NewRequestWithContext(ctx, "POST", up.URL, strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				if !replayable {
					req.GetBody = nil
					req.ContentLength = -1
				}
				length := req.ContentLength
				cl := dbgClient(&http.Client{Timeout: 5 * time.Second}, "opencode")
				resp, err := cl.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("upstream status = %d: debug capture corrupted the JSON request", resp.StatusCode)
				}
				if req.ContentLength != length {
					t.Errorf("content length changed: got %d, want %d", req.ContentLength, length)
				}
				if replayable {
					replay, err := req.GetBody()
					if err != nil {
						t.Fatal(err)
					}
					got, err := io.ReadAll(replay)
					replay.Close()
					if err != nil || string(got) != body {
						t.Error("debug capture corrupted the replay body")
					}
				}
				hop := tr.Hops[0]
				if hop.ReqBody != body[:min(len(body), traceUpReqCap)] || hop.ReqTruncated != (len(body) > traceUpReqCap) {
					t.Errorf("capture len=%d truncated=%v, original len=%d", len(hop.ReqBody), hop.ReqTruncated, len(body))
				}
			})
		}
	}
}

// TestDbgRTNoTrace: with capture off the same client behaves exactly as an
// unwrapped one, so the flag cannot change request behavior.
func TestDbgRTNoTrace(t *testing.T) {
	setDebugFile(false, "")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "plain")
	}))
	defer up.Close()
	req, _ := http.NewRequest("POST", up.URL, strings.NewReader("x"))
	cl := dbgClient(&http.Client{Timeout: 5 * time.Second}, "nvidia")
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "plain" {
		t.Fatalf("body = %q", b)
	}
}

// TestDbgFileNoDumpOnSuccess: a clean 200 through ServeHTTP writes nothing.
func TestDbgFileNoDumpOnSuccess(t *testing.T) {
	dir := t.TempDir()
	resetDbgFile(t, dir)

	// /status is unauthenticated and 200s, so a clean path exists without
	// an upstream at all.
	p := newPool(nil)
	req := httptest.NewRequest("GET", "/status", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if files := dumpFiles(t, dir); len(files) != 0 {
		t.Fatalf("clean request wrote %d dump files", len(files))
	}
}

// TestDbgFileOffByDefault: no flag means zero dumps and no transport swap,
// so the capture path cannot regress normal traffic.
func TestDbgFileOffByDefault(t *testing.T) {
	setDebugFile(false, "")
	cl := &http.Client{Timeout: 5 * time.Second}
	if got := dbgClient(cl, "nvidia"); got != cl {
		t.Error("dbgClient must be identity when capture is off")
	}
	if cl.Transport != nil {
		t.Error("dbgClient must not touch Transport when capture is off")
	}
}

// TestDbgClientWrapsOnce: wrapping twice must not stack transports, which
// would double-record every hop.
func TestDbgClientWrapsOnce(t *testing.T) {
	setDebugFile(true, t.TempDir())
	t.Cleanup(func() { setDebugFile(false, "") })

	cl := dbgClient(&http.Client{Timeout: time.Second}, "nvidia")
	if _, ok := cl.Transport.(dbgRT); !ok {
		t.Fatalf("transport = %T, want dbgRT", cl.Transport)
	}
	again := dbgClient(cl, "nvidia")
	if again.Transport != cl.Transport {
		t.Error("second wrap replaced the transport")
	}
}

// TestSafeHeaderMapRedacts: credential headers keep their length, everything
// else is verbatim.
func TestSafeHeaderMapRedacts(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer supersecret")
	h.Set("X-Session-Id", "abc123")
	h.Set("Content-Type", "application/json")
	got := safeHeaderMap(h)

	if !strings.Contains(got["Authorization"], "redacted") || strings.Contains(got["Authorization"], "supersecret") {
		t.Errorf("authorization not redacted: %q", got["Authorization"])
	}
	if strings.Contains(got["X-Session-Id"], "abc123") {
		t.Errorf("session leaked: %q", got["X-Session-Id"])
	}
	if got["Content-Type"] != "application/json" {
		t.Errorf("plain header mangled: %q", got["Content-Type"])
	}
}

// TestTraceBodyCapturesInbound: the tee hands back every byte the handler
// reads while keeping the stream intact.
func TestTraceBodyCapturesInbound(t *testing.T) {
	tr := &traceRec{}
	tb := &traceBody{tr: tr, r: strings.NewReader("hello world")}
	buf := make([]byte, 4)
	var got []byte
	for {
		n, err := tb.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			break
		}
	}
	if string(got) != "hello world" {
		t.Fatalf("read back %q", got)
	}
	if tr.InBody != "hello world" {
		t.Errorf("captured %q", tr.InBody)
	}
}

// owner boundary: debug readers and writers preserve traffic and bound captures.
func TestDebugBodyCaptureBounds(t *testing.T) {
	for _, size := range []int{traceUpRespCap - 1, traceUpRespCap, traceUpRespCap + 1, 1 << 20} {
		t.Run(fmt.Sprintf("bytes=%d", size), func(t *testing.T) {
			body := strings.Repeat("x", size)
			tr := &traceRec{}
			in := &traceBody{tr: tr, r: strings.NewReader(body)}
			got, err := io.ReadAll(in)
			if err != nil || string(got) != body {
				t.Fatal("inbound traffic changed")
			}
			if tr.InBody != body[:min(size, traceInboundCap)] || tr.InTrunc != (size > traceInboundCap) {
				t.Errorf("inbound capture len=%d truncated=%v", len(tr.InBody), tr.InTrunc)
			}

			resetDbgFile(t, t.TempDir())
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, body)
			}))
			defer up.Close()
			ctx := context.WithValue(context.Background(), traceKey{}, tr)
			req, _ := http.NewRequestWithContext(ctx, "GET", up.URL, nil)
			resp, err := dbgClient(&http.Client{Timeout: 5 * time.Second}, "opencode").Do(req)
			if err != nil {
				t.Fatal(err)
			}
			got, err = io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || string(got) != body {
				t.Errorf("response traffic changed: got %d bytes, want %d, err=%v", len(got), size, err)
			}
			hop := tr.Hops[0]
			if hop.RespBody != body[:min(size, traceUpRespCap)] || hop.RespTrunc != (size > traceUpRespCap) {
				t.Errorf("response capture len=%d truncated=%v", len(hop.RespBody), hop.RespTrunc)
			}

			rec := httptest.NewRecorder()
			out := &dbgWriter{w: rec, tr: tr}
			for i := 0; i < size; i += 10000 {
				if _, err := out.Write([]byte(body[i:min(i+10000, size)])); err != nil {
					t.Fatal(err)
				}
			}
			if rec.Body.String() != body || out.buf.String() != body[:min(size, traceOutCap)] {
				t.Errorf("outbound traffic len=%d capture len=%d", rec.Body.Len(), out.buf.Len())
			}
		})
	}
}

// TestSnapBodyRecordsHopOnClose: an undrained stream still lands its hop, so
// a client disconnect that aborts a read still produces a complete dump.
func TestSnapBodyRecordsHopOnClose(t *testing.T) {
	tr := &traceRec{}
	hop := &traceHop{Target: "http://x/y"}
	sb := &snapBody{tr: tr, hop: hop, r: strings.NewReader("part"), c: nopCloser{}}
	p := make([]byte, 4)
	sb.Read(p)
	sb.Close()
	if tr.Hops == nil {
		t.Fatal("close did not record the hop")
	}
	if tr.Hops[0].RespBody != "part" {
		t.Errorf("resp_body = %q, want part", tr.Hops[0].RespBody)
	}
	// a second close must not duplicate the hop.
	sb.Close()
	if len(tr.Hops) != 1 {
		t.Errorf("hops = %d after double close, want 1", len(tr.Hops))
	}
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// TestTraceCaps: oversized bodies are trimmed and flagged, through the
// reader the handlers actually get.
func TestTraceCaps(t *testing.T) {
	tr := &traceRec{}
	big := strings.Repeat("x", traceInboundCap+10)
	if _, err := io.ReadAll(&traceBody{tr: tr, r: strings.NewReader(big)}); err != nil {
		t.Fatal(err)
	}
	if !tr.InTrunc {
		t.Error("oversized inbound body not flagged")
	}
	if len(tr.InBody) != traceInboundCap {
		t.Errorf("in_body len = %d, want %d", len(tr.InBody), traceInboundCap)
	}
}

// TestBackendForModel: route naming so a dump says where it failed.
func TestBackendForModel(t *testing.T) {
	for m, want := range map[string]string{
		"opencode/muse": "opencode", "freeepi/x": "freepi",
		"antseed/x": "antseed", "route/x": "route", "moonshotai/kimi": "nvidia",
	} {
		if got := backendForModel(m); got != want {
			t.Errorf("%s -> %s, want %s", m, got, want)
		}
	}
}

// TestDumpIfErrStatusGate: only non-2xx produce files.
func TestDumpIfErrStatusGate(t *testing.T) {
	dir := t.TempDir()
	resetDbgFile(t, dir)
	for _, st := range []int{200, 204, 302, 400, 500} {
		tr := &traceRec{ReqID: "req-1"}
		tr.end(st, nil)
		tr.dumpIfErr()
	}
	if files := dumpFiles(t, dir); len(files) != 2 {
		t.Fatalf("got %d files, want 2 (400, 500)", len(files))
	}
}
