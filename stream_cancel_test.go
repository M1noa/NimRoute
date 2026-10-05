package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sseBody builds an upstream chat SSE stream of n text deltas.
func sseBody(n int) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteString("data: " + `{"choices":[{"index":0,"delta":{"content":"chunk` +
			string(rune('a'+i%26)) + `"}}]}` + "\n\n")
	}
	sb.WriteString("data: [DONE]\n\n")
	return sb.String()
}

// slowReader hands out the stream in small pieces with pauses, so a copy
// loop can be caught mid-flight (this is how the reported truncation
// happened: a chunk boundary landing wrong).
type slowReader struct {
	data []byte
	i    int
	step int
	held bool
}

func (s *slowReader) Read(p []byte) (int, error) {
	if s.i >= len(s.data) {
		return 0, io.EOF
	}
	if s.step <= 0 {
		s.step = 7
	}
	n := s.step
	if n > len(p) {
		n = len(p)
	}
	if s.i+n > len(s.data) {
		n = len(s.data) - s.i
	}
	copy(p, s.data[s.i:s.i+n])
	s.i += n
	s.step--
	return n, nil
}

// the zen chat copy loop must deliver every upstream byte and terminate
// cleanly when the client stays connected. the original bug produced a
// well-formed but empty stream that ended early with no tool calls.
func TestZenChatCopyDeliversWholeStream(t *testing.T) {
	body := sseBody(200)
	rec := httptest.NewRecorder()
	resp := &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(&slowReader{data: []byte(body)}),
	}
	n, _, _ := copyZenChatStream(context.Background(), rec, resp, nil, 0, 0)
	if n != int64(len(body)) {
		t.Fatalf("copied %d of %d bytes", n, len(body))
	}
	if got := strings.Count(rec.Body.String(), "chunk"); got != 200 {
		t.Fatalf("delivered %d of 200 chunks: truncated", got)
	}
	if !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Error("missing [DONE] terminator")
	}
}

// a cancelled context must stop the copy promptly rather than draining
// the whole upstream body, so the exit is freed for the next request.
func TestZenChatCopyStopsOnCancel(t *testing.T) {
	body := strings.Repeat(sseBody(1), 5000)
	ctx, cancel := context.WithCancel(context.Background())
	rec := httptest.NewRecorder()
	resp := &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(&slowReader{data: []byte(body)}),
	}
	cancel() // already gone
	n, _, _ := copyZenChatStream(ctx, rec, resp, nil, 0, 0)
	if n >= int64(len(body)) {
		t.Fatalf("cancelled copy still drained %d of %d bytes", n, len(body))
	}
}

// the anthropic-facing live streamer must carry every upstream text delta.
// built as a whole body so the test pins the conversion, not the socket.
func TestStreamResponsesLiveCarriesAllDeltas(t *testing.T) {
	var sb strings.Builder
	for _, d := range []string{"hello ", "world ", "again"} {
		b, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "delta": d})
		sb.WriteString("data: " + string(b) + "\n\n")
	}
	b, _ := json.Marshal(map[string]any{"type": "response.completed"})
	sb.WriteString("data: " + string(b) + "\n\ndata: [DONE]\n\n")
	stream := sb.String()

	rec := httptest.NewRecorder()
	written, _, _ := streamResponsesLive(context.Background(), rec,
		&slowReader{data: []byte(stream)}, nil)
	if written == 0 {
		t.Fatal("no bytes written")
	}
	for _, want := range []string{"hello ", "world ", "again"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("oai live stream dropped delta %q", want)
		}
	}
	if !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Error("oai live stream missing [DONE]")
	}

	rec2 := httptest.NewRecorder()
	_, _, _ = streamResponsesLiveAnthropic(context.Background(), rec2,
		&slowReader{data: []byte(stream)}, "m", nil)
	for _, want := range []string{"hello ", "world ", "again"} {
		if !strings.Contains(rec2.Body.String(), want) {
			t.Errorf("anthropic live stream dropped delta %q", want)
		}
	}
	if !strings.Contains(rec2.Body.String(), "message_stop") {
		t.Error("anthropic live stream missing message_stop")
	}
}

// a stream split one byte at a time must still reassemble: the live
// streamers buffer partial lines, and a wrong split silently drops text.
func TestStreamResponsesLiveHandlesByteSplit(t *testing.T) {
	full := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"split-safe-text\"}\n\n" +
		"data: {\"type\":\"response.completed\"}\n\n"
	rec := httptest.NewRecorder()
	streamResponsesLive(context.Background(), rec, &slowReader{data: []byte(full), step: 1}, nil)
	if !strings.Contains(rec.Body.String(), "split-safe-text") {
		t.Fatalf("byte-split stream lost content: %q", rec.Body.String())
	}
	rec2 := httptest.NewRecorder()
	streamResponsesLiveAnthropic(context.Background(), rec2, &slowReader{data: []byte(full), step: 1}, "m", nil)
	if !strings.Contains(rec2.Body.String(), "split-safe-text") {
		t.Fatalf("anthropic byte-split stream lost content: %q", rec2.Body.String())
	}
}

// tool calls must survive the live path too: that is how a session "says
// part of what it was doing then stops and runs no tools".
func TestStreamResponsesLiveCarriesToolCalls(t *testing.T) {
	var sb strings.Builder
	add := func(o map[string]any) {
		b, _ := json.Marshal(o)
		sb.WriteString("data: " + string(b) + "\n\n")
	}
	add(map[string]any{"type": "response.output_item.added", "item": map[string]any{
		"type": "function_call", "call_id": "call_1", "name": "bash"}})
	add(map[string]any{"type": "response.function_call_arguments.delta", "delta": `{"cmd":"ls"}`})
	add(map[string]any{"type": "response.completed"})

	rec2 := httptest.NewRecorder()
	_, _, _ = streamResponsesLiveAnthropic(context.Background(), rec2,
		strings.NewReader(sb.String()), "m", toolNames{"Bash"})
	out := rec2.Body.String()
	if !strings.Contains(out, "tool_use") {
		t.Errorf("live anthropic stream dropped the tool call: %s", out)
	}
	if !strings.Contains(out, `"name":"Bash"`) {
		t.Errorf("tool name not restored to the client spelling: %s", out)
	}
}

// timeout guard so a regression that stalls shows as a failure, not a hang.
func TestLiveStreamersReturnPromptly(t *testing.T) {
	start := time.Now()
	rec := httptest.NewRecorder()
	streamResponsesLive(context.Background(), rec, strings.NewReader("data: [DONE]\n\n"), nil)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("live streamer took %v on a trivial body", d)
	}
}
