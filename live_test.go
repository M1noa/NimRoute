package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// handleAnthropic end to end minus the network hop, which is the only part
// not ours. covers routing, RTK, PII ordering and the tool-name restore in
// one pass. asserts on the body handed to the response translator.
// local chains over httptest servers (never live upstreams despite the
// old TestLive* names).
func TestLocalAnthropicChain(t *testing.T) {
	c := defaultConfig()
	c.Rtk.Enabled = true
	c.Nvidia.Enabled = true
	c.NvidiaKeys = map[string]string{"k1": "fake"}
	useConfig(t, &c)

	var big strings.Builder
	big.WriteString("On branch main\n")
	for i := 0; i < 40; i++ {
		big.WriteString("\tmodified:   file" + string(rune('a'+i%26)) + ".go\n")
	}
	raw := big.String()
	anthropic := []byte(`{"model":"test","max_tokens":100,"messages":[
		{"role":"user","content":"go"},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":` + jsonStr(raw) + `}]}
	],"tools":[{"name":"Bash","input_schema":{"type":"object"}}]}`)

	// step 1: what the handler produces for the upstream call
	compressed := rtkCompressBody(append([]byte(nil), anthropic...))
	oai, clientModel, _, tnames, _, err := anthropicRequestToOpenAI(compressed)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	t.Logf("tool_result %d bytes -> oai body %d bytes", len(raw), len(oai))
	if len(oai) > 1400 {
		t.Errorf("tool_result not compressed: oai body %d bytes", len(oai))
	}
	if !strings.Contains(string(oai), "main") {
		t.Errorf("branch lost: %s", oai)
	}
	if len(tnames) != 1 || tnames[0] != "Bash" {
		t.Fatalf("tool names = %v, want [Bash]", tnames)
	}

	// step 2: upstream replies lowercase, client must get PascalCase back
	up := []byte(`{"choices":[{"message":{"content":"","tool_calls":[
		{"id":"c1","type":"function","function":{"name":"bash","arguments":"{}"}}]},
		"finish_reason":"tool_calls"}]}`)
	out, errMsg, _ := openAIToAnthropic(up, clientModel, tnames)
	if errMsg != "" {
		t.Fatalf("response translate: %s", errMsg)
	}
	if !strings.Contains(string(out), `"name":"Bash"`) {
		t.Errorf("client got lowercase tool name: %s", out)
	}
	t.Logf("client sees: %s", out)

	// step 3: the compressed tool result must still be valid JSON in the
	// exact shape the upstream expects
	var m map[string]any
	if err := json.Unmarshal(compressed, &m); err != nil {
		t.Fatalf("compressed body is not valid JSON: %v", err)
	}
	if _, ok := m["stream_options"]; ok {
		t.Errorf("unexpected field survived")
	}
}

// the full streaming chain, same coverage through the SSE writer.
func TestLocalStreamChain(t *testing.T) {
	c := defaultConfig()
	c.Rtk.Enabled = true
	useConfig(t, &c)

	body := rtkCompressBody([]byte(`{"model":"m","messages":[{"role":"user","content":[
		{"type":"tool_result","tool_use_id":"t1","content":` +
		jsonStr("On branch main\n"+strings.Repeat("\tmodified:   x.go\n", 40)) + `}]}]}`))
	_, _, _, tnames, _, err := anthropicRequestToOpenAI(body)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(tnames) != 0 {
		t.Logf("tool names: %v", tnames)
	}

	sse := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"bash","arguments":"{}"}}]},"finish_reason":null}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	w := httptest.NewRecorder()
	streamAnthropic(w, []byte(sse), "m", toolNames{"Bash"})
	got := w.Body.String()
	if !strings.Contains(got, `"name":"Bash"`) {
		t.Errorf("stream: client got lowercase: %s", got)
	}
	// every SSE line must be parseable JSON
	for _, line := range strings.Split(got, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		d := strings.TrimPrefix(line, "data: ")
		if d == "[DONE]" {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(d), &v); err != nil {
			t.Errorf("malformed SSE line %q: %v", line, err)
		}
	}
}

// full zen round trip through p.ServeHTTP for both client protocols,
// against a fake zen upstream. regression: hedgedDo cancelled the winning
// response's context on return, so every opencode stream came back empty
// and clients saw the session end with no output. the upstream streams its
// second chunk 150ms in, so the body must stay readable after zenPost and
// hedgedDo have returned.
func TestZenProxyE2EStreams(t *testing.T) {
	zsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.Error(w, `{"error":"unexpected path"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello \"}}]}\n\n")
		fl.Flush()
		time.Sleep(150 * time.Millisecond)
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"world\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":3,\"total_tokens\":8}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer zsrv.Close()
	oldBase := OpencodeBase
	OpencodeBase = zsrv.URL
	t.Cleanup(func() { OpencodeBase = oldBase })

	c := loadSuiteConfig()
	c.Zen.Enabled = true
	c.Zen.AlwaysProxy = false // empty pool -> direct send to the fake upstream
	c.Zen.HedgeAfterMs = 2000
	c.Usage.Path = filepath.Join(t.TempDir(), "usage.jsonl")
	useConfig(t, c)

	p := newPool(nil)
	psrv := httptest.NewServer(http.HandlerFunc(p.ServeHTTP))
	defer psrv.Close()
	post := func(path, body, session string) string {
		t.Helper()
		req, err := http.NewRequest("POST", psrv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-session-id", session)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		defer res.Body.Close()
		out, _ := io.ReadAll(io.LimitReader(res.Body, 32<<20))
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s status %d: %s", path, res.StatusCode, out)
		}
		return string(out)
	}

	// openai protocol: /v1/chat/completions passthrough stream
	oaiSSE := post("/v1/chat/completions",
		`{"model":"opencode/test-chat","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "e2e-oai")
	for _, want := range []string{"hello ", "world", `"finish_reason":"stop"`, "data: [DONE]"} {
		if !strings.Contains(oaiSSE, want) {
			t.Errorf("openai stream missing %q, got: %s", want, oaiSSE)
		}
	}

	// anthropic protocol: /v1/messages, what claude code sends
	aSSE := post("/v1/messages",
		`{"model":"opencode/test-chat","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, "e2e-ant")
	for _, want := range []string{"message_start", "content_block_delta", "hello ", "world", "message_stop", "data: [DONE]"} {
		if !strings.Contains(aSSE, want) {
			t.Errorf("anthropic stream missing %q, got: %s", want, aSSE)
		}
	}
}

// owner boundary: zen /v1/messages must deliver content before generation
// finishes, including a streamed nudge, with exactly one terminal message.
func TestZenAnthropicStreamsBeforeCompletion(t *testing.T) {
	for _, responses := range []bool{false, true} {
		for _, nudge := range []bool{false, true} {
			t.Run(fmt.Sprintf("responses=%v/nudge=%v", responses, nudge), func(t *testing.T) {
				resetLanes()
				defer resetLanes()
				firstRelease, retryRelease := make(chan struct{}), make(chan struct{})
				var firstOnce, retryOnce sync.Once
				releaseFirst := func() { firstOnce.Do(func() { close(firstRelease) }) }
				releaseRetry := func() { retryOnce.Do(func() { close(retryRelease) }) }
				defer releaseFirst()
				defer releaseRetry()
				var hits atomic.Int32
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					io.Copy(io.Discard, r.Body)
					retry := hits.Add(1) > 1
					w.Header().Set("Content-Type", "text/event-stream")
					if responses {
						if retry {
							io.WriteString(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"i1\",\"type\":\"function_call\",\"call_id\":\"c1\",\"name\":\"read\"}}\n\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"i1\",\"delta\":\"{}\"}\n\n")
						} else {
							io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"working\"}\n\n")
						}
					} else if retry {
						io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"read\",\"arguments\":\"{}\"}}]}}]}\n\n")
					} else {
						io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"working\"}}]}\n\n")
					}
					w.(http.Flusher).Flush()
					gate := firstRelease
					if retry {
						gate = retryRelease
					}
					select {
					case <-gate:
					case <-r.Context().Done():
						return
					}
					if responses {
						io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n")
					} else {
						finish := "stop"
						if retry {
							finish = "tool_calls"
						}
						fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":%q}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3}}\n\n", finish)
					}
					io.WriteString(w, "data: [DONE]\n\n")
				}))
				defer func() { releaseFirst(); releaseRetry(); up.Close() }()
				old := OpencodeBase
				OpencodeBase = up.URL
				defer func() { OpencodeBase = old }()
				c := testConfig()
				c.Zen.AlwaysProxy = false
				c.Zen.HedgeAfterMs = -1
				c.Zen.LaneMinGapMs = 0
				c.Nudge.Enabled = nudge
				c.Models.Nudge = []nudgeYAML{{Pattern: "*", Nudge: true}}
				useConfig(t, c)
				p := newPool(nil)
				proxy := httptest.NewServer(p)
				defer func() { releaseFirst(); releaseRetry(); proxy.Close() }()
				model := "opencode/step-5-preview-free"
				if responses {
					model = "opencode/muse-spark-1.3-contributor-free"
				}
				body := fmt.Sprintf(`{"model":%q,"max_tokens":128,"stream":true,"messages":[{"role":"user","content":"read files"}],"tools":[{"name":"Read","input_schema":{"type":"object","properties":{}}}]}`, model)
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				req, _ := http.NewRequestWithContext(ctx, "POST", proxy.URL+"/v1/messages", strings.NewReader(body))
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				reader := bufio.NewReader(resp.Body)
				var out strings.Builder
				readUntil := func(marker string) {
					t.Helper()
					for {
						line, err := reader.ReadString('\n')
						out.WriteString(line)
						if strings.Contains(line, marker) {
							return
						}
						if err != nil {
							t.Fatalf("content was buffered until completion: %v", err)
						}
					}
				}
				readUntil(`"text":"working"`)
				releaseFirst()
				if nudge {
					readUntil(`"partial_json":"{}"`)
					releaseRetry()
				}
				rest, err := io.ReadAll(reader)
				if err != nil {
					t.Fatal(err)
				}
				out.Write(rest)
				if strings.Count(out.String(), "event: message_stop") != 1 || strings.Count(out.String(), "event: message_start") != 1 {
					t.Fatalf("turn boundaries malformed: %s", out.String())
				}
				wantHits := int32(1)
				if nudge {
					wantHits = 2
				}
				if hits.Load() != wantHits {
					t.Fatalf("upstream hits %d, want %d", hits.Load(), wantHits)
				}
				if !strings.Contains(out.String(), fmt.Sprintf(`"output_tokens":%d`, 3*wantHits)) {
					t.Fatalf("trailing usage lost: %s", out.String())
				}
				if !equalCounts(sseIndexCounts(out.String(), "content_block_start"), sseIndexCounts(out.String(), "content_block_stop")) {
					t.Fatalf("block boundaries malformed: %s", out.String())
				}
			})
		}
	}
}

// owner boundary: the streamed continuation hides only the internal marker,
// not ordinary text, and usage includes both upstream generations.
func TestStreamedNudgeSentinelAndText(t *testing.T) {
	for _, responseShape := range []bool{false, true} {
		for _, retryText := range []string{waitSentinel, "extra text"} {
			t.Run(fmt.Sprintf("responses=%v/retry=%s", responseShape, retryText), func(t *testing.T) {
				makeBody := func(text string) string {
					var out strings.Builder
					for _, char := range text {
						if responseShape {
							fmt.Fprintf(&out, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\n", string(char))
						} else {
							fmt.Fprintf(&out, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", string(char))
						}
					}
					if responseShape {
						out.WriteString("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n")
					} else {
						out.WriteString("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3}}\n\n")
					}
					out.WriteString("data: [DONE]\n\n")
					return out.String()
				}
				next := func(text string, calls bool) io.ReadCloser {
					if text != "first" || calls {
						t.Errorf("wrong continuation context: %q %v", text, calls)
					}
					return io.NopCloser(strings.NewReader(makeBody(retryText)))
				}
				rec := httptest.NewRecorder()
				var p, c int
				if responseShape {
					_, p, c = streamResponsesAnthropicReader(context.Background(), rec, strings.NewReader(makeBody("first")), "m", nil, next)
				} else {
					_, p, c = streamAnthropicReader(context.Background(), rec, strings.NewReader(makeBody("first")), "m", nil, next)
				}
				if p != 14 || c != 6 {
					t.Errorf("usage %d/%d, want 14/6", p, c)
				}
				out := rec.Body.String()
				if strings.Contains(out, waitSentinel) {
					t.Fatal("internal sentinel leaked")
				}
				if retryText == "extra text" && strings.Count(out, `"type":"text_delta"`) != len("firstextra text") {
					t.Fatal("ordinary continuation text hidden")
				}
				if !equalCounts(sseIndexCounts(out, "content_block_start"), sseIndexCounts(out, "content_block_stop")) {
					t.Fatalf("mismatched block boundaries: %s", out)
				}
			})
		}
	}
}

// owner boundary: a reasoning continuation opens a fresh block, never emits
// a delta into the first generation's already-closed thinking block.
func TestStreamedNudgeThinkingBlocks(t *testing.T) {
	first := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"first thought\",\"content\":\"first answer\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	retry := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"second thought\",\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"read\",\"arguments\":\"{}\"}}]}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	rec := httptest.NewRecorder()
	streamAnthropicReader(context.Background(), rec, strings.NewReader(first), "m", nil, func(string, bool) io.ReadCloser { return io.NopCloser(strings.NewReader(retry)) })
	open := map[int]bool{}
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var e struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
		}
		if err := json.Unmarshal([]byte(line[6:]), &e); err != nil {
			t.Fatal(err)
		}
		switch e.Type {
		case "content_block_start":
			if open[e.Index] {
				t.Fatal("block reopened before closing")
			}
			open[e.Index] = true
		case "content_block_delta":
			if !open[e.Index] {
				t.Fatalf("delta sent into closed block %d: %s", e.Index, line)
			}
		case "content_block_stop":
			if !open[e.Index] {
				t.Fatal("closed block stopped twice")
			}
			open[e.Index] = false
		}
	}
}

// owner boundary: thirty independent clients must stream and drain without
// losing tool calls, usage, or responsiveness of status polling.
func TestZenThirtySessions(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"read\",\"arguments\":\"{}\"}}]}}]}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(20 * time.Millisecond)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n")
	}))
	defer up.Close()
	old := OpencodeBase
	OpencodeBase = up.URL
	defer func() { OpencodeBase = old; resetLanes() }()
	c := testConfig()
	c.Zen.AlwaysProxy = false
	c.Zen.Lanes = 30
	c.Zen.LaneMinGapMs = 0
	c.Zen.HedgeAfterMs = -1
	c.Nudge.Enabled = true
	useConfig(t, c)
	resetDbgFile(t, t.TempDir())
	p := newPool(nil)
	proxy := httptest.NewServer(p)
	defer proxy.Close()
	client := &http.Client{Timeout: 10 * time.Second}
	var wg sync.WaitGroup
	for session := 0; session < 30; session++ {
		wg.Add(1)
		go func(session int) {
			defer wg.Done()
			for turn := 0; turn < 10; turn++ {
				body := `{"model":"opencode/step-5-preview-free","stream":true,"max_tokens":128,"messages":[{"role":"user","content":"read"}],"tools":[{"name":"Read","input_schema":{"type":"object","properties":{}}}]}`
				req, _ := http.NewRequest("POST", proxy.URL+"/v1/messages", strings.NewReader(body))
				req.Header.Set("x-session-id", fmt.Sprintf("session-%d", session))
				res, err := client.Do(req)
				if err != nil {
					t.Errorf("session %d turn %d: %v", session, turn, err)
					return
				}
				out, err := io.ReadAll(res.Body)
				res.Body.Close()
				if err != nil || res.StatusCode != 200 || !strings.Contains(string(out), `"name":"Read"`) || !strings.Contains(string(out), `"output_tokens":3`) || strings.Count(string(out), "event: message_stop") != 1 {
					t.Errorf("session %d turn %d malformed: %d %v %s", session, turn, res.StatusCode, err, out)
					return
				}
			}
		}(session)
	}
	wg.Wait()
	if used, _ := p.semStats(); used != 0 {
		t.Fatalf("slots leaked: %d", used)
	}
	lanesMu.Lock()
	defer lanesMu.Unlock()
	for _, l := range lanes {
		if l.inflight != 0 {
			t.Errorf("lane claim leaked: %d", l.inflight)
		}
	}
}

// benchmark the user interface with a fixed upstream delay, not live exit noise.
func BenchmarkZenAnthropicTTFT(b *testing.B) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"PONG\"}}]}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(100 * time.Millisecond)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer up.Close()
	oldBase, oldConfig := OpencodeBase, cfg()
	OpencodeBase = up.URL
	defer func() { OpencodeBase = oldBase; applyConfig(oldConfig); resetLanes() }()
	c := testConfig()
	c.Zen.AlwaysProxy = false
	c.Zen.HedgeAfterMs = -1
	c.Zen.LaneMinGapMs = 0
	c.Nudge.Enabled = false
	applyConfig(c)
	proxy := httptest.NewServer(newPool(nil))
	defer proxy.Close()
	body := `{"model":"opencode/step-5-preview-free","stream":true,"max_tokens":128,"messages":[{"role":"user","content":"PONG"}]}`
	var ttft time.Duration
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		resp, err := http.Post(proxy.URL+"/v1/messages", "application/json", strings.NewReader(body))
		if err != nil {
			b.Fatal(err)
		}
		scanner := bufio.NewScanner(resp.Body)
		seen := false
		for scanner.Scan() {
			if !seen && strings.Contains(scanner.Text(), `"text":"PONG"`) {
				ttft += time.Since(start)
				seen = true
			}
		}
		resp.Body.Close()
		if !seen || scanner.Err() != nil {
			b.Fatal("incomplete stream", scanner.Err())
		}
	}
	b.ReportMetric(float64(ttft.Microseconds())/float64(b.N), "ttft-us/op")
}

// real HTTP round trip through callUpstream, the nvidia branch of
// handleAnthropic, against a local upstream. this is the path a /v1/messages
// request takes when the model is not opencode/*: request translation, real
// upstream exchange, response translation, tool-name restore.
func TestLocalToolCallRoundTrip(t *testing.T) {
	var gotBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(io.LimitReader(r.Body, 32<<20))
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"let me check","tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"ls\"}"}},
			{"id":"call_2","type":"function","function":{"name":"glob","arguments":"{\"pattern\":\"*.go\"}"}},
			{"id":"call_3","type":"function","function":{"name":"say\"hi","arguments":"{}"}}
		]},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":100,"completion_tokens":20}}`)
	}))
	defer up.Close()

	c := defaultConfig()
	c.Rtk.Enabled = true
	c.Nvidia.Enabled = true
	c.NvidiaKeys = map[string]string{"k1": "fake"}
	useConfig(t, &c)

	declared := `{"model":"test-model","messages":[{"role":"user","content":"list files"}],
		"tools":[{"name":"Bash","input_schema":{"type":"object"}},
		         {"name":"Glob","input_schema":{"type":"object"}},
		         {"name":"Say\"Hi","input_schema":{"type":"object"}}]}`
	oai, clientModel, _, tn, _, err := anthropicRequestToOpenAI([]byte(declared))
	if err != nil {
		t.Fatalf("translate request: %v", err)
	}
	if len(tn) != 3 || tn[0] != "Bash" || tn[1] != "Glob" || tn[2] != `Say"Hi` {
		t.Fatalf("declared names = %v", tn)
	}
	// the tool schemas must reach the upstream request intact
	for _, want := range []string{"Bash", "Glob", `Say\"Hi`} {
		if !strings.Contains(string(oai), want) {
			t.Errorf("tool %s missing from upstream body: %s", want, oai)
		}
	}

	p := newPool(c.NvidiaKeys)
	res, err := p.callUpstream(context.Background(), "POST", up.URL, oai,
		http.Header{"Content-Type": {"application/json"}}, "test-model", "1.2.3.4")
	if err != nil || res == nil {
		t.Fatalf("callUpstream: %v", err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("upstream status %d: %s", res.StatusCode, res.Body)
	}
	_ = gotBody

	out, errMsg, _ := openAIToAnthropic(res.Body, clientModel, tn)
	if errMsg != "" {
		t.Fatalf("translate response: %s", errMsg)
	}
	t.Logf("client sees: %s", out)

	for _, want := range []string{`"name":"Bash"`, `"name":"Glob"`, `"name":"Say\"Hi"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %s in translated response", want)
		}
	}
	for _, bad := range []string{`"name":"bash"`, `"name":"glob"`, `"name":"say\"hi"`} {
		if strings.Contains(string(out), bad) {
			t.Errorf("lowercase name leaked: %s", bad)
		}
	}
	if !strings.Contains(string(out), `"stop_reason":"tool_use"`) {
		t.Errorf("stop_reason should be tool_use:\n%s", out)
	}
	if !strings.Contains(string(out), `"input":{"command":"ls"}`) {
		t.Errorf("tool arguments not parsed:\n%s", out)
	}
	if !strings.Contains(string(out), `"input_tokens":100`) {
		t.Errorf("usage not carried over:\n%s", out)
	}
}
