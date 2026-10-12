package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// sseIndexCounts collects the content_block_start or _stop indices from an
// emitted anthropic SSE body as a multiset.
func sseIndexCounts(body, kind string) map[int]int {
	m := map[int]int{}
	for _, line := range strings.Split(body, "\n") {
		if !strings.Contains(line, kind) {
			continue
		}
		p := strings.Index(line, `"index":`)
		if p < 0 {
			continue
		}
		rest := line[p+len(`"index":`):]
		n, seen := 0, false
		for i := 0; i < len(rest) && rest[i] >= '0' && rest[i] <= '9'; i++ {
			n = n*10 + int(rest[i]-'0')
			seen = true
		}
		if seen {
			m[n]++
		}
	}
	return m
}

func equalCounts(a, b map[int]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// a turn that opens text/thinking/tool blocks must stop exactly the blocks it
// opened. a stop for an index that never opened (or a missing stop) reads as a
// malformed stream to strict clients like Claude Code.
func TestStreamAnthropicBlockIndicesMatch(t *testing.T) {
	cases := []struct {
		name string
		sse  string
	}{
		{
			name: "text-then-two-tools",
			sse: "data: {\"choices\":[{\"delta\":{\"content\":\"calling\"}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c0\",\"type\":\"function\",\"function\":{\"name\":\"bash\",\"arguments\":\"{}\"}}]}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"glob\",\"arguments\":\"{}\"}}]}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
		},
		{
			name: "two-tools-same-chunk",
			sse: "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c0\",\"type\":\"function\",\"function\":{\"name\":\"bash\",\"arguments\":\"{}\"}},{\"index\":1,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"glob\",\"arguments\":\"{}\"}}]}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
		},
		{
			name: "thinking-then-tool",
			sse: "data: {\"choices\":[{\"delta\":{\"reasoning\":\"hmm\"}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c0\",\"type\":\"function\",\"function\":{\"name\":\"bash\",\"arguments\":\"{}\"}}]}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
		},
		{
			name: "tool-without-finish-reason",
			sse: "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c0\",\"type\":\"function\",\"function\":{\"name\":\"bash\",\"arguments\":\"{}\"}}]}}]}\n\n" +
				"data: [DONE]\n\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			streamAnthropic(rec, []byte(tc.sse), "m", toolNames{"bash", "glob"})
			body := rec.Body.String()
			starts := sseIndexCounts(body, "content_block_start")
			stops := sseIndexCounts(body, "content_block_stop")
			if !equalCounts(starts, stops) {
				t.Fatalf("start indices %v != stop indices %v\n%s", starts, stops, body)
			}
		})
	}
}

// chat sse scan: text-only vs tool_calls deltas; buffered json too.
func TestScanChatSSE(t *testing.T) {
	textOnly := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"just chatting\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n")
	text, hasCalls := scanChatSSE(textOnly)
	if hasCalls {
		t.Fatal("text-only body must not report calls")
	}
	if text != "just chatting" {
		t.Fatalf("text = %q, want %q", text, "just chatting")
	}
	withCalls := []byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"bash\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n")
	if _, hasCalls := scanChatSSE(withCalls); !hasCalls {
		t.Fatal("tool_calls delta must report calls")
	}
	// buffered plain json (freepi shape): no data: lines.
	buf := []byte(`{"choices":[{"message":{"content":"hi","tool_calls":[{"id":"c1"}]},"finish_reason":"tool_calls"}]}`)
	if _, hasCalls := scanChatSSE(buf); !hasCalls {
		t.Fatal("buffered json with tool_calls must report calls")
	}
	if _, hasCalls := scanChatJSON(buf); !hasCalls {
		t.Fatal("scanChatJSON must report calls")
	}
	plain := []byte(`{"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}]}`)
	text, hasCalls = scanChatJSON(plain)
	if hasCalls || text != "hi" {
		t.Fatalf("plain json: text=%q calls=%v", text, hasCalls)
	}
}

// chat continuation appends assistant text + reprompt on messages[].
func TestNudgeContinuationChat(t *testing.T) {
	posted := []byte(`{"model":"m","messages":[{"role":"user","content":"list files"}],"tools":[{"type":"function","function":{"name":"bash"}}]}`)
	nb := nudgeContinuationChat(posted, "I will list the files.")
	if nb == nil {
		t.Fatal("want continuation body, got nil")
	}
	var m struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(nb, &m); err != nil {
		t.Fatalf("continuation is not json: %v", err)
	}
	if len(m.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 (user, assistant, reprompt)", len(m.Messages))
	}
	if m.Messages[1].Role != "assistant" || m.Messages[1].Content != "I will list the files." {
		t.Fatalf("assistant echo wrong: %+v", m.Messages[1])
	}
	if !strings.Contains(m.Messages[2].Content, "You have tools available") {
		t.Fatalf("reprompt missing: %q", m.Messages[2].Content)
	}
}

// mergeChatSSE strips terminal lines from all but the last body.
func TestMergeChatSSE(t *testing.T) {
	first := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n")
	retry := []byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\"}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n")
	merged := mergeChatSSE([][]byte{first, retry})
	if c := strings.Count(string(merged), "data: [DONE]"); c != 1 {
		t.Fatalf("merged has %d [DONE], want 1", c)
	}
	if _, hasCalls := scanChatSSE(merged); !hasCalls {
		t.Fatalf("merged must keep tool calls:\n%s", merged)
	}
}

// mergeChatJSON: texts concatenate, retry calls win, usage sums.
func TestMergeChatJSON(t *testing.T) {
	first := []byte(`{"id":"a","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	retry := []byte(`{"id":"b","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"bash","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":12,"completion_tokens":3}}`)
	merged := mergeChatJSON(first, retry)
	text, hasCalls := scanChatJSON(merged)
	if !hasCalls {
		t.Fatalf("merged must carry calls: %s", merged)
	}
	if text != "hi" {
		t.Fatalf("merged text = %q, want %q", text, "hi")
	}
	var m struct {
		Usage map[string]any `json:"usage"`
	}
	json.Unmarshal(merged, &m)
	if m.Usage["prompt_tokens"].(float64) != 22 {
		t.Fatalf("prompt tokens must sum: %v", m.Usage)
	}
}

// chat nudge fires on tool-less turns and swallows tool-less retries.
func TestTryNudgeChat(t *testing.T) {
	c := testConfig()
	c.Nudge.Enabled = true
	c.Models.Nudge = []nudgeYAML{{Pattern: "*", Nudge: true}}
	applyConfig(c)
	defer applyConfig(testConfig())

	posted := []byte(`{"model":"opencode/space-bunny-free","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"bash"}}],"stream":true}`)
	first := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"just chatting\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n")
	withCalls := []byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"bash\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n")
	post := func(nb []byte) []byte {
		if !strings.Contains(string(nb), "You have tools available") {
			t.Errorf("nudge body missing reprompt: %q", nb)
		}
		return withCalls
	}
	if got := tryNudgeChat("", "opencode/space-bunny-free", "opencode", posted, first, post); got == nil {
		t.Fatal("want retry body, got nil")
	}
	// tool-calling first body: no nudge.
	if out := tryNudgeChat("", "opencode/space-bunny-free", "opencode", posted, withCalls, post); out != nil {
		t.Fatalf("tool-calling body must not nudge, got %s", out)
	}
	// tool-less retry: swallowed.
	postNone := func(nb []byte) []byte { return first }
	if out := tryNudgeChat("", "opencode/space-bunny-free", "opencode", posted, first, postNone); out != nil {
		t.Fatalf("tool-less retry must yield nil, got %s", out)
	}
	// tool_choice none: gate stubs never nudge.
	noTools := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"bash"}}],"tool_choice":"none"}`)
	called := false
	postTrack := func(nb []byte) []byte { called = true; return withCalls }
	if out := tryNudgeChat("", "opencode/space-bunny-free", "opencode", noTools, first, postTrack); out != nil {
		t.Fatalf("tool_choice none must not nudge, got %s", out)
	}
	if called {
		t.Fatal("post must not run when no tools offered")
	}
}

// nudge fires at most nudgeMaxStreak consecutive tool-less turns per session,
// then stops; a tool-calling turn resets the streak.
func TestTryNudgeResponsesStreakCap(t *testing.T) {
	c := testConfig()
	c.Nudge.Enabled = true
	applyConfig(c)
	defer applyConfig(testConfig())

	posted := []byte(`{"model":"opencode/muse-spark-1.3","input":[{"role":"user","content":"hi"}],"tools":[{"name":"bash","parameters":{"type":"object"}}],"stream":true}`)
	first := []byte("data: {\"type\":\"response.output_text.done\",\"text\":\"just chatting\"}\n\ndata: [DONE]\n\n")
	toolFirst := []byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"bash\",\"arguments\":\"{}\"}}\n\ndata: [DONE]\n\n")
	postNone := func(nb []byte) []byte {
		return []byte("data: {\"type\":\"response.output_text.done\",\"text\":\"still chatting\"}\n\ndata: [DONE]\n\n")
	}

	session := "streak-cap-test"
	calls := 0
	post := func(nb []byte) []byte { calls++; return postNone(nb) }

	for i := 0; i < nudgeMaxStreak; i++ {
		if out := tryNudgeResponses(session, "opencode/muse-spark-1.3", posted, first, post); out != nil {
			t.Fatalf("tool-less retry must yield nil, got %s", out)
		}
	}
	if calls != nudgeMaxStreak {
		t.Fatalf("post calls = %d, want %d", calls, nudgeMaxStreak)
	}
	if out := tryNudgeResponses(session, "opencode/muse-spark-1.3", posted, first, post); out != nil {
		t.Fatalf("capped turn must not nudge, got %s", out)
	}
	if calls != nudgeMaxStreak {
		t.Fatalf("streak cap must stop the nudge, post calls = %d", calls)
	}
	// a tool-calling turn resets the streak.
	if out := tryNudgeResponses(session, "opencode/muse-spark-1.3", posted, toolFirst, post); out != nil {
		t.Fatalf("tool-calling body must not nudge, got %s", out)
	}
	if out := tryNudgeResponses(session, "opencode/muse-spark-1.3", posted, first, post); out != nil {
		t.Fatalf("tool-less retry must yield nil after reset, got %s", out)
	}
	if calls != nudgeMaxStreak+1 {
		t.Fatalf("reset should allow a fresh nudge, post calls = %d", calls)
	}
}
