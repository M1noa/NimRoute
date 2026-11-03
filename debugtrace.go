// debugtrace: dump full inbound + upstream request/response to json when a
// request ends in an error. enabled with -df / --dbg-file (dir optional).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// default home for dump files when --dbg-file is passed bare.
const debugDumpDefaultDir = "nimroute-debug"

// per-side caps: a dump is for reading, not archiving.
const (
	traceInboundCap = 256 << 10
	traceUpReqCap   = 256 << 10
	traceUpRespCap  = 256 << 10
	traceOutCap     = 256 << 10
)

// credential headers never reach a dump file: AGENTS §7 keeps them out of
// logs and /status alike. the marker keeps the length, since header size is
// itself a debugging signal.
var traceSecretHeaders = []string{
	"Authorization", "X-Api-Key", "Cookie", "X-Session-Id", "Proxy-Authorization",
}

var (
	dbgFileMu  sync.RWMutex
	dbgFileOn  bool
	dbgFileDir = debugDumpDefaultDir
)

// setDebugFile records the flag: bare --dbg-file keeps the default dir, a
// value overrides it.
func setDebugFile(on bool, dir string) {
	dbgFileMu.Lock()
	defer dbgFileMu.Unlock()
	dbgFileOn = on
	if dir != "" {
		dbgFileDir = dir
	}
}

func debugFileDir() string {
	dbgFileMu.RLock()
	defer dbgFileMu.RUnlock()
	return dbgFileDir
}

func debugFileEnabled() bool {
	dbgFileMu.RLock()
	defer dbgFileMu.RUnlock()
	return dbgFileOn
}

// traceHop is one upstream attempt: what the proxy sent and what came back.
type traceHop struct {
	Backend      string            `json:"backend,omitempty"`
	Target       string            `json:"target"`
	Method       string            `json:"method"`
	ReqHeaders   map[string]string `json:"req_headers,omitempty"`
	ReqBody      string            `json:"req_body,omitempty"`
	ReqTruncated bool              `json:"req_truncated,omitempty"`
	Status       int               `json:"status"`
	RespHeaders  map[string]string `json:"resp_headers,omitempty"`
	RespBody     string            `json:"resp_body,omitempty"`
	RespTrunc    bool              `json:"resp_truncated,omitempty"`
	Error        string            `json:"error,omitempty"`
	ElapsedMs    int64             `json:"elapsed_ms"`
}

// traceRec is the whole life of one inbound request, dumped as one json.
type traceRec struct {
	ReqID      string            `json:"req_id"`
	Ts         string            `json:"ts"`
	DurationMs int64             `json:"duration_ms"`
	Method     string            `json:"method"`
	Path       string            `json:"path"`
	RemoteAddr string            `json:"remote_addr"`
	Model      string            `json:"model,omitempty"`
	Backend    string            `json:"backend,omitempty"`
	Stream     bool              `json:"stream,omitempty"`
	InHeaders  map[string]string `json:"in_headers,omitempty"`
	InBody     string            `json:"in_body,omitempty"`
	InTrunc    bool              `json:"in_truncated,omitempty"`
	Hops       []*traceHop       `json:"hops,omitempty"`
	OutStatus  int               `json:"out_status"`
	OutBody    string            `json:"out_body,omitempty"`
	OutTrunc   bool              `json:"out_truncated,omitempty"`
	// context the access-log line can't carry: lane id, session fragment,
	// pool state, the proxy in play.
	Notes []string `json:"notes,omitempty"`

	startedAt time.Time
	mu        sync.Mutex
}

type traceKey struct{}

// traceFrom pulls the record off a context, nil when capture is off.
func traceFrom(ctx context.Context) *traceRec {
	if !debugFileEnabled() {
		return nil
	}
	r, _ := ctx.Value(traceKey{}).(*traceRec)
	return r
}

// startTrace builds a record and returns a context carrying it. nil record
// (capture off) keeps the original context.
func startTrace(r *http.Request) (*traceRec, context.Context) {
	if !debugFileEnabled() {
		return nil, r.Context()
	}
	tr := &traceRec{
		Method:     r.Method,
		Path:       r.URL.String(),
		RemoteAddr: r.RemoteAddr,
		InHeaders:  safeHeaderMap(r.Header),
		startedAt:  time.Now(),
	}
	return tr, context.WithValue(r.Context(), traceKey{}, tr)
}

func (t *traceRec) addHop(h *traceHop) {
	if h == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Hops = append(t.Hops, h)
}

func (t *traceRec) setReqID(id string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ReqID = id
}

// backendForModel names the route a model prefix picked, so a dump says
// which backend failed even when the handler bailed before its own log line.
func backendForModel(m string) string {
	switch {
	case strings.HasPrefix(m, "opencode/"):
		return "opencode"
	case strings.HasPrefix(m, "freeepi/"):
		return "freepi"
	case strings.HasPrefix(m, "antseed/"):
		return "antseed"
	case strings.HasPrefix(m, "route/"):
		return "route"
	case strings.HasPrefix(m, "wroute/"):
		return "wroute"
	case strings.HasPrefix(m, "nous/"):
		return "nous"
	case strings.HasPrefix(m, "uno/"):
		return "uno"
	case strings.HasPrefix(m, "openrouter/"):
		return "openrouter"
	case strings.HasPrefix(m, "orcarouter/"):
		return "orcarouter"
	default:
		return "nvidia"
	}
}

func (t *traceRec) setModel(m, backend string, stream bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.Model == "" {
		t.Model = m
	}
	if t.Backend == "" {
		t.Backend = backend
	}
	t.Stream = stream
}

// traceBody tees the inbound request into the record on its way to the
// handler, so every route dumps the same raw bytes the proxy received.
type traceBody struct {
	tr *traceRec
	r  io.Reader
}

func (b *traceBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if n > 0 {
		remaining := traceInboundCap - len(b.tr.InBody)
		b.tr.InBody += string(bytesOf(p[:n], remaining))
		if n > remaining {
			b.tr.InTrunc = true
		}
	}
	return n, err
}

func (b *traceBody) Close() error {
	if c, ok := b.r.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// dbgWriter records the client-visible status + body so a dump shows what
// the caller actually got, not just what the proxy sent upstream.
type dbgWriter struct {
	w   http.ResponseWriter
	tr  *traceRec
	buf bytes.Buffer
	hdr int
}

func (d *dbgWriter) Header() http.Header { return d.w.Header() }

func (d *dbgWriter) WriteHeader(s int) {
	if d.hdr == 0 {
		d.hdr = s
	}
	d.w.WriteHeader(s)
}

func (d *dbgWriter) Write(b []byte) (int, error) {
	n, err := d.w.Write(b)
	// cap the capture: a 300s stream would otherwise pin the whole turn in
	// ram for a dump that never happens.
	if n > 0 && d.buf.Len() < traceOutCap {
		d.buf.Write(bytesOf(b[:n], traceOutCap-d.buf.Len()))
	}
	return n, err
}

func (d *dbgWriter) Flush() {
	if f, ok := d.w.(http.Flusher); ok {
		f.Flush()
	}
}

// status returns the recorded status, defaulting to the implicit 200 that
// net/http writes for a handler that never called WriteHeader.
func (d *dbgWriter) status() int {
	if d.hdr == 0 {
		return http.StatusOK
	}
	return d.hdr
}

// end captures the client-visible outcome; dumpIfErr decides from it.
func (t *traceRec) end(status int, body []byte) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.OutStatus = status
	t.OutBody = string(bytesOf(body, traceUpRespCap))
	t.OutTrunc = len(body) > traceUpRespCap
	t.DurationMs = time.Since(t.startedAt).Milliseconds()
}

// dumpIfErr writes the record when the request did not end cleanly. only
// 4xx/5xx produce files: successes are noise and extra i/o on the request
// path buys nothing.
func (t *traceRec) dumpIfErr() {
	if t == nil || (t.OutStatus > 0 && t.OutStatus < 400) {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	dir := debugFileDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		acclog.Printf("WARN: dbg-file mkdir %s: %v", dir, err)
		return
	}
	t.Ts = time.Now().Format(time.RFC3339Nano)
	out, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return
	}
	p := filepath.Join(dir, fmt.Sprintf("%d-%s.json", time.Now().UnixNano(), sanitizeTraceName(t.ReqID)))
	if err := os.WriteFile(p, out, 0o600); err != nil {
		acclog.Printf("WARN: dbg-file write %s: %v", p, err)
		return
	}
	// this flag exists to debug 429/5xx storms, which is exactly when one
	// file per failure arrives. prune here, not in a cron nobody runs.
	pruneTraceDir(dir)
	acclog.Printf("%s dumped to %s (%d hops)", t.ReqID, p, len(t.Hops))
}

// traceMaxDumps / traceMaxDumpBytes bound dump storage. each dump carries
// request and response bodies, so a storm's disk cost is the real risk.
const (
	traceMaxDumps     = 200
	traceMaxDumpBytes = 64 << 20
)

// pruneTraceDir drops the oldest dumps until the directory holds at most
// traceMaxDumps files and traceMaxDumpBytes. best effort: a failed stat must
// never cost the dump that was just written.
func pruneTraceDir(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type file struct {
		name string
		size int64
	}
	var fs []file
	var total int64
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		fs = append(fs, file{name: e.Name(), size: info.Size()})
		total += info.Size()
	}
	// ReadDir sorts by name, and the name starts with a unix-nano
	// timestamp, so index order is oldest-first: no second sort needed.
	// the newest file always survives: pruning what was just written when a
	// single dump alone exceeds the byte bound is worse than being over it.
	oldest := 0
	for oldest < traceMaxDumps && oldest < len(fs) && len(fs)-oldest > traceMaxDumps {
		os.Remove(filepath.Join(dir, fs[oldest].name))
		total -= fs[oldest].size
		oldest++
	}
	for i := oldest; i+1 < len(fs) && total > traceMaxDumpBytes; i++ {
		os.Remove(filepath.Join(dir, fs[i].name))
		total -= fs[i].size
	}
}

func sanitizeTraceName(id string) string {
	if id == "" {
		return "unknown"
	}
	return strings.NewReplacer("/", "_", " ", "_", "\\", "_").Replace(id)
}

func bytesOf(b []byte, cap int) []byte {
	if len(b) <= cap {
		return b
	}
	return b[:cap]
}

// safeHeaderMap flattens headers for json, masking credentials.
func safeHeaderMap(h http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		if isSecretHeader(k) {
			n := 0
			for _, s := range v {
				n += len(s)
			}
			if n > 0 {
				out[k] = fmt.Sprintf("<redacted %d chars>", n)
			}
			continue
		}
		out[k] = strings.Join(v, ", ")
	}
	return out
}

func isSecretHeader(k string) bool {
	for _, s := range traceSecretHeaders {
		if strings.EqualFold(k, s) {
			return true
		}
	}
	return false
}

// --- transport ---

// dbgRT is the instrumented http.RoundTripper. installed on the upstream
// clients only when capture is on, so the zero case costs nothing.
type dbgRT struct {
	base    http.RoundTripper
	backend string
}

func (t dbgRT) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	tr := traceFrom(req.Context())
	if tr == nil {
		return base.RoundTrip(req)
	}
	hop := &traceHop{
		Backend:    t.backend,
		Target:     req.URL.String(),
		Method:     req.Method,
		ReqHeaders: safeHeaderMap(req.Header),
	}
	start := time.Now()
	if req.Body != nil {
		// capture a bounded prefix, then send it plus the unread remainder.
		// content length and replay must still describe the original body.
		body := req.Body
		snap, err := io.ReadAll(io.LimitReader(body, traceUpReqCap+1))
		hop.ReqTruncated = len(snap) > traceUpReqCap
		hop.ReqBody = string(bytesOf(snap, traceUpReqCap))
		req.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(snap), body), body}
		if err != nil {
			body.Close()
			hop.Error = "read req body: " + err.Error()
			hop.ElapsedMs = time.Since(start).Milliseconds()
			tr.addHop(hop)
			return nil, err
		}
	}
	resp, err := base.RoundTrip(req)
	if err != nil {
		hop.Error = err.Error()
		hop.ElapsedMs = time.Since(start).Milliseconds()
		tr.addHop(hop)
		return resp, err
	}
	hop.Status = resp.StatusCode
	hop.RespHeaders = safeHeaderMap(resp.Header)
	if resp.Body != nil {
		// the caller drains the body after RoundTrip returns, so the hop
		// can only be finalized here: snapBody records it on EOF or Close,
		// whichever the handler reaches.
		resp.Body = &snapBody{tr: tr, hop: hop, r: resp.Body, c: resp.Body}
	}
	return resp, nil
}

// snapBody tees the response stream into the hop while passing bytes through.
type snapBody struct {
	tr  *traceRec
	hop *traceHop
	r   io.Reader
	c   io.Closer

	mu        sync.Mutex
	buf       bytes.Buffer
	done      bool
	truncated bool
}

func (s *snapBody) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.mu.Lock()
		remaining := traceUpRespCap - s.buf.Len()
		s.buf.Write(bytesOf(p[:n], remaining))
		if n > remaining {
			s.truncated = true
		}
		s.mu.Unlock()
	}
	if err != nil {
		s.finish()
	}
	return n, err
}

func (s *snapBody) Close() error {
	s.finish()
	return s.c.Close()
}

// finish stamps the hop with the captured body exactly once.
func (s *snapBody) finish() {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return
	}
	s.done = true
	body := s.buf.String()
	s.mu.Unlock()
	s.hop.RespBody = body
	s.hop.RespTrunc = s.truncated
	s.tr.addHop(s.hop)
}

// dbgClient instruments c so its upstream traffic lands in the trace. a
// no-op passthrough when capture is off, and never double-wraps a client
// whose transport already records.
func dbgClient(c *http.Client, backend string) *http.Client {
	if c == nil || !debugFileEnabled() {
		return c
	}
	if _, ok := c.Transport.(dbgRT); ok {
		return c
	}
	c.Transport = dbgRT{base: c.Transport, backend: backend}
	return c
}
