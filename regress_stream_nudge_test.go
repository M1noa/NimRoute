package main

import (
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
