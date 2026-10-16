package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// laneGate must serialize concurrent senders one min-gap apart: the second
// caller's slot is claimed before the first finishes sleeping, so two
// goroutines never burst the same exit together.
func TestLaneGateSerializesConcurrentSenders(t *testing.T) {
	c := testConfig()
	c.Zen.LaneMinGapMs = 300
	applyConfig(c)
	defer applyConfig(testConfig())

	resetLanes()
	defer resetLanes()
	l := &zenLane{id: "test", session: zenSession()}

	var wg sync.WaitGroup
	sends := make([]time.Time, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			laneGate(context.Background(), l)
			sends[i] = time.Now()
		}(i)
	}
	wg.Wait()

	// serialization property: one of the two callers waited the full
	// min-gap, so the absolute gap stays large. (which goroutine won the
	// first slot is scheduling, not contract: assert the gap, not the
	// order.) the floor is 100ms, not 250: sleep wakeup jitter under CI
	// load must not red a correct implementation.
	gap := sends[1].Sub(sends[0])
	if gap < 0 {
		gap = -gap
	}
	if gap < 100*time.Millisecond {
		t.Fatalf("laneGate gap %v far below the 300ms min-gap", gap)
	}
}

// laneRotate never waits: a lane hit by 100 consecutive 429s keeps a
// fresh exit + session on every rotation, no backoff, no overflow.
func TestLaneRotateNeverWaits(t *testing.T) {
	applyConfig(testConfig())
	defer applyConfig(testConfig())

	resetLanes()
	defer resetLanes()
	l := &zenLane{id: "test", session: zenSession()}
	zenProxiesMu.Lock()
	zenProxies = []string{"http://10.99.0.1:1"}
	zenProxiesMu.Unlock()
	defer func() {
		zenProxiesMu.Lock()
		zenProxies = nil
		zenProxiesMu.Unlock()
	}()
	sess := map[string]bool{}
	for i := 0; i < 100; i++ {
		s := laneRotate(l, "")
		if sess[s] {
			t.Fatalf("laneRotate iteration %d reused session %s: must mint fresh", i, s)
		}
		sess[s] = true
	}
}

// same contract for the nvidia key backoff.
func TestKeyRateLimitBackoffNeverNegative(t *testing.T) {
	p := newPool(map[string]string{"a": "nvapi-x"})
	k := p.keys[0]
	for i := 0; i < 100; i++ {
		p.rateLimit(k)
		if k.CooldownUntil.Before(time.Now()) {
			t.Fatalf("rateLimit iteration %d produced a past cooldown: backoff overflow", i)
		}
	}
	if k.CooldownUntil.Sub(time.Now()) > maxBackoff+time.Minute {
		t.Fatalf("backoff exceeds maxBackoff: %v", k.CooldownUntil.Sub(time.Now()))
	}
}

// zenClient must reuse one transport per proxy instead of leaking a fresh
// transport (and its idle connections) on every call.
func TestZenClientTransportsCachedPerProxy(t *testing.T) {
	a1 := zenClient("http://1.2.3.4:8080")
	a2 := zenClient("http://1.2.3.4:8080")
	if a1.Transport != a2.Transport {
		t.Fatal("zenClient built two transports for the same proxy (fd leak)")
	}
	b := zenClient("http://5.6.7.8:8080")
	if a1.Transport == b.Transport {
		t.Fatal("different proxies must not share a transport")
	}
	direct := zenClient("")
	if direct.Transport != nil {
		t.Fatal("direct client should use the default transport")
	}
	closeZenTransport("http://1.2.3.4:8080")
	a3 := zenClient("http://1.2.3.4:8080")
	if a3.Transport == a1.Transport {
		t.Fatal("closeZenTransport did not drop the cached transport")
	}
}

// forwarded zen requests must not carry client auth headers.
func TestZenHeaderForwardingDropsCredentials(t *testing.T) {
	out := http.Header{}
	hdrs := http.Header{}
	hdrs.Set("X-Api-Key", "sk-secret")
	hdrs.Set("Authorization", "Bearer sk-secret")
	hdrs.Set("Cookie", "session=xyz")
	hdrs.Set("X-Custom", "keepme")
	for k, v := range hdrs {
		if zenHeaderForwarded(k) {
			out[k] = v
		}
	}
	if out.Get("X-Api-Key") != "" || out.Get("Authorization") != "" || out.Get("Cookie") != "" {
		t.Fatalf("credentials forwarded upstream: %v", out)
	}
	if out.Get("X-Custom") != "keepme" {
		t.Fatalf("custom headers must survive forwarding, got %v", out)
	}
}

// a nim stream must reach the client while the upstream is still writing.
// the bug: handleNvidia ReadAll'd the 200 body into memory (twice — once
// for the read, once for the token/debug tee) and re-wrapped it, so the
// first byte a streaming caller saw landed after generation finished. the
// upstream here holds the stream open until the first byte is observed.
func TestNvidiaStreamFirstByteBeforeBodyComplete(t *testing.T) {
	release := make(chan struct{})
	finished := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"first\"}}]}\n\n")
		if fl != nil {
			fl.Flush()
		}
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			// the proxy buffered the whole body: it can never deliver the
			// first byte while this write is still open.
			t.Error("upstream held open 5s: the proxy buffered the body instead of streaming it")
		}
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"second\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
		close(finished)
	}))
	defer srv.Close()

	oldBase := nvidiaBase
	nvidiaBase = srv.URL
	t.Cleanup(func() { nvidiaBase = oldBase })

	c := testConfig()
	c.Nvidia.Enabled = true
	c.NvidiaKeys = map[string]string{"nv1": "k"}
	useConfig(t, c)
	p := newPool(c.NvidiaKeys)

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := []byte(`{"model":"nvidia/x","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		p.handleNvidia(w, r, body, "nvidia/x", true, time.Now(), "test-nvstream")
	}))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"nvidia/x","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("stream request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("client status = %d, want 200", resp.StatusCode)
	}

	// the byte that has to arrive early, and the signal that lets the
	// upstream finish.
	buf := make([]byte, 4096)
	n, err := resp.Body.Read(buf)
	if err != nil || n == 0 {
		t.Fatalf("no first byte delivered (n=%d err=%v)", n, err)
	}
	if !strings.Contains(string(buf[:n]), "first") {
		t.Fatalf("first delivered payload = %q, want the opening chunk", string(buf[:n]))
	}
	select {
	case <-finished:
		t.Fatal("upstream finished generating before the proxy delivered its first chunk")
	default:
	}
	close(release)
	<-finished
}

// the 4096 cap must not silently stop pinning new models: past the cap the
// oldest write is dropped so the next id still lands.
func TestNoteKeyEvictsInsteadOfRefusing(t *testing.T) {
	c := testConfig()
	applyConfig(c)
	defer applyConfig(testConfig())
	p := newPool(map[string]string{"nv1": "k1", "nv2": "k2"})

	for i := 0; i < 4096; i++ {
		p.noteKey("model-old-"+strconv.Itoa(i), "nv1")
	}
	p.mu.RLock()
	n := len(p.lastKey)
	p.mu.RUnlock()
	if n != 4096 {
		t.Fatalf("map grew to %d, want it held at the cap", n)
	}
	// a brand-new id must still pin, not be refused.
	p.noteKey("model-fresh", "nv2")
	if got := p.stickyKey("model-fresh"); got != "nv2" {
		t.Fatalf("fresh model pinned to %q, want nv2", got)
	}
	p.mu.RLock()
	n = len(p.lastKey)
	p.mu.RUnlock()
	if n != 4096 {
		t.Fatalf("map grew to %d after eviction, want it held at the cap", n)
	}
	// and the oldest pin is the one that went.
	if got := p.stickyKey("model-old-0"); got != "" {
		t.Fatalf("oldest model still pinned to %q, want it evicted", got)
	}
}

// uint16be is the length prefix for every framed field. it panics past
// 0xffff, so the invariant the framer depends on is pinned here rather than
// discovered in a panic on the live path.
func TestUint16beBoundary(t *testing.T) {
	for _, n := range []int{0, 1, 0x00ff, 0x0100, 0x7fff, 0xfffe, 0xffff} {
		got := uint16be(n)
		if len(got) != 2 {
			t.Fatalf("uint16be(%d) = %d bytes, want 2", n, len(got))
		}
		if int(binary.BigEndian.Uint16(got)) != n {
			t.Fatalf("uint16be(%d) round-tripped to %d", n, binary.BigEndian.Uint16(got))
		}
	}
	for _, n := range []int{0x10000, 0x10001, 1 << 20, -1} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("uint16be(%d) did not panic", n)
				}
			}()
			uint16be(n)
		}()
	}
}

// the frame carries the body as uint32, so an upload larger than the
// uint16 ceiling still round-trips whole: this is what keeps the
// uint16be panic unreachable on the live path.
func TestAntEncodeHTTPReqCarriesBodyAsUint32(t *testing.T) {
	small := []byte(`{"n":1}`)
	q := antHTTPReq{
		id:      "req-1",
		method:  http.MethodPost,
		path:    "/v1/chat/completions",
		headers: map[string]string{"content-type": "application/json"},
		body:    small,
	}
	enc := antEncodeHTTPReq(q)
	off := 2 + len(q.id) + 1 + len(q.method)
	if got := binary.BigEndian.Uint16(enc[off : off+2]); got != uint16(len(q.path)) {
		t.Fatalf("path length prefix = %d, want %d", got, len(q.path))
	}
	off += 2 + len(q.path)
	if got := int(binary.BigEndian.Uint16(enc[off : off+2])); got != len(q.headers) {
		t.Fatalf("header count prefix = %d, want %d", got, len(q.headers))
	}
	off += 2
	for k, v := range q.headers {
		if got := int(binary.BigEndian.Uint16(enc[off : off+2])); got != len(k) {
			t.Fatalf("header key length prefix = %d, want %d", got, len(k))
		}
		off += 2 + len(k)
		if got := int(binary.BigEndian.Uint16(enc[off : off+2])); got != len(v) {
			t.Fatalf("header value length prefix = %d, want %d", got, len(v))
		}
		off += 2 + len(v)
	}
	if got := int(binary.BigEndian.Uint32(enc[off : off+4])); got != len(small) {
		t.Fatalf("body length prefix = %d, want %d", got, len(small))
	}
	if !bytes.Equal(enc[off+4:], small) {
		t.Fatal("body bytes were not carried verbatim")
	}
}
