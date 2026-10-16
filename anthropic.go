package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptrace"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// --- Types ---

type anthropicRequest struct {
	Model         string             `json:"model"`
	System        json.RawMessage    `json:"system,omitempty"`
	Messages      []anthropicMsg     `json:"messages"`
	Tools         []anthropicTool    `json:"tools,omitempty"`
	ToolChoice    any                `json:"tool_choice,omitempty"`
	MaxTokens     int                `json:"max_tokens"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	TopK          *int               `json:"top_k,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	Thinking      *anthropicThinking `json:"thinking,omitempty"`
}

type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

// thinkingToEffort maps Claude Code's thinking field to a reasoning effort.
// Returns "" when the client did not send an explicit budget, so the model
// default (from model_params) applies.
func thinkingToEffort(t *anthropicThinking) string {
	if t == nil {
		return ""
	}
	switch t.Type {
	case "enabled":
		switch {
		case t.BudgetTokens <= 0:
			return "medium"
		case t.BudgetTokens <= 4000:
			return "low"
		case t.BudgetTokens <= 16000:
			return "medium"
		default:
			return "high"
		}
	default:
		return ""
	}
}

type anthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Source    *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	} `json:"source,omitempty"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
	Function    *struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// --- Config ---

type claudeModelEntry struct {
	Pattern string `json:"pattern"`
	Model   string `json:"model"`
}

var (
	claudeModels   []*claudeModelEntry
	claudeModelsMu sync.RWMutex
)

// claude mapping lives in config.yml models.claude_map now; applyConfig
// rebuilds it, so there is no separate loader anymore.

func mapClaudeModel(model string) string {
	claudeModelsMu.RLock()
	defer claudeModelsMu.RUnlock()
	ml := strings.ToLower(model)
	for _, e := range claudeModels {
		if globMatch(e.Pattern, ml) {
			return e.Model
		}
	}
	return model
}

// toolNames are the tool names the client declared, in the client's spelling.
// Upstream models (zen, spark, and other OpenAI-compatible gateways) frequently
// return tool call names lowercased ("bash" for "Bash"), which makes Claude Code
// reject the call with "No such tool available". Response emitters run the
// upstream name back through resolve so tool_use carries the declared spelling.
type toolNames []string

// zen aliases: the ballast toolset ensureZenTools embeds uses the opencode
// CLI's naming. when the model fires one of those whose client had a
// different spelling (shell vs Bash, subagent vs Task), translate the call
// back only when the client actually declares the target name.
var zenToolAliases = map[string]string{
	"shell":    "bash",
	"subagent": "task",
	"question": "askuserquestion",
}

// resolve maps an upstream tool call name back to the client's spelling: exact
// match first, then case-insensitive zen-ballast alias, then case-insensitive.
// Unknown names pass through unchanged.
func (tn toolNames) resolve(name string) string {
	if !cfg().Translate.restoreToolNames() {
		return name
	}
	for _, c := range tn {
		if c == name {
			return c
		}
	}
	if alias, ok := zenToolAliases[strings.ToLower(name)]; ok {
		for _, c := range tn {
			if strings.EqualFold(c, alias) {
				return c
			}
		}
	}
	for _, c := range tn {
		if strings.EqualFold(c, name) {
			return c
		}
	}
	return name
}

// --- Request Conversion ---

func anthropicRequestToOpenAI(body []byte) (oai []byte, clientModel, upstreamModel string, tnames toolNames, isStream bool, err error) {
	var req anthropicRequest
	if err = json.Unmarshal(body, &req); err != nil {
		return nil, "", "", nil, false, fmt.Errorf("invalid JSON: %w", err)
	}

	clientModel = req.Model
	upstreamModel = mapClaudeModel(clientModel)
	// claude code 1m alias: no backend serves a [1m] id, so strip the
	// suffix unconditionally (a claude_map hit on the bare base still
	// applies). catalog lookups stay downstream, never gate the strip:
	// a missing catalog file must not break routing.
	if base, ok := baseModelID(upstreamModel); ok {
		upstreamModel = mapClaudeModel(base)
	}
	isStream = req.Stream

	messages := make([]map[string]any, 0)

	if sysText := extractSystemText(req.System); sysText != "" {
		sysText, _ = stripSomeGuardrails(sysText)
		if line := cfg().Inject.HelpfulText; cfg().Inject.HelpfulLine && line != "" && !strings.Contains(sysText, line) {
			sysText = line + "\n" + sysText
		}
		if dl := discloseLine(); dl != "" && !strings.Contains(sysText, dl) {
			sysText += "\n" + dl
		}
		messages = append(messages, map[string]any{"role": "system", "content": sysText})
	} else if dl := discloseLine(); dl != "" {
		messages = append(messages, map[string]any{"role": "system", "content": dl})
	}

	for _, msg := range req.Messages {
		switch msg.Role {
		case "user":
			messages = append(messages, convertUserMessage(msg.Content)...)
		case "assistant":
			messages = append(messages, convertAssistantMessage(msg.Content)...)
		}
	}

	out := map[string]any{
		"model":      upstreamModel,
		"messages":   messages,
		"max_tokens": req.MaxTokens,
	}

	if req.MaxTokens == 0 {
		out["max_tokens"] = 4096
	}

	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if req.TopK != nil {
		out["top_k"] = *req.TopK
	}
	if len(req.StopSequences) > 0 {
		out["stop"] = req.StopSequences
	}

	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			name := t.Name
			desc := t.Description
			params := json.RawMessage(t.InputSchema)
			// Also accept OpenAI-format tools sent to /v1/messages:
			// {"type":"function","function":{...}}
			if t.Function != nil {
				if t.Function.Name != "" {
					name = t.Function.Name
				}
				if t.Function.Description != "" {
					desc = t.Function.Description
				}
				if len(t.Function.Parameters) > 0 && len(params) == 0 {
					params = t.Function.Parameters
				}
			}
			if strings.TrimSpace(name) == "" {
				continue // zen rejects empty tool names
			}
			tnames = append(tnames, name)
			// zen 400s when parameters is null/missing/non-object, and 400s
			// the whole request with "Recursive JSON schemas are not currently
			// supported" when a client tool ships $defs/$ref cycles. sanitize
			// here so both the chat and responses paths inherit it.
			var obj map[string]any
			if err := json.Unmarshal(params, &obj); err != nil || obj == nil {
				obj = map[string]any{"type": "object"}
			}
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        name,
					"description": desc,
					"parameters":  sanitizeZenSchema(obj),
				},
			})
		}
		if len(tools) > 0 {
			out["tools"] = tools
		}
	}

	if req.ToolChoice != nil {
		out["tool_choice"] = convertToolChoice(req.ToolChoice)
	}

	if eff := thinkingToEffort(req.Thinking); eff != "" {
		out["reasoning_effort"] = eff
	}

	if isStream {
		out["stream"] = true
		out["stream_options"] = map[string]any{"include_usage": true}
	}

	ob, _ := json.Marshal(out)
	if cfg().Inject.Params {
		injectParams(&ob)
	}

	return ob, clientModel, upstreamModel, tnames, isStream, nil
}

func extractSystemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []anthropicBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func convertUserMessage(raw json.RawMessage) []map[string]any {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []map[string]any{{"role": "user", "content": s}}
	}
	var blocks []anthropicBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return []map[string]any{{"role": "user", "content": ""}}
	}

	var result []map[string]any
	var textParts []string
	var imgParts []map[string]any

	// flush emits the text/images gathered so far as one user message. block
	// order matters: parts written before a tool_result must land before the
	// tool message and parts after it after, or a later user message splits
	// the tool_result from the assistant turn that requested it.
	flush := func() {
		if len(textParts) == 0 && len(imgParts) == 0 {
			return
		}
		result = append(result, map[string]any{"role": "user", "content": contentOf(textParts, imgParts)})
		textParts, imgParts = nil, nil
	}

	for _, b := range blocks {
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
		case "tool_result":
			// empty ids carry no correlation upstream: skip instead of
			// sending tool_call_id "".
			if b.ToolUseID == "" {
				continue
			}
			text, imgs := toolResultParts(b.Content)
			if b.IsError {
				text = "Error: " + text
			}
			flush()
			result = append(result, map[string]any{
				"role":         "tool",
				"tool_call_id": b.ToolUseID,
				"content":      text,
			})
			// a tool_result can carry images (screenshot tools). role:"tool"
			// is text-only upstream, so the images become a trailing
			// multimodal user message instead of being pasted into the tool
			// content as raw json, which zen /chat/completions 400s on.
			if len(imgs) > 0 {
				result = append(result, map[string]any{
					"role":    "user",
					"content": contentOf(nil, imgs),
				})
			}
		case "image":
			if u := imageURL(b.Source); u != "" {
				imgParts = append(imgParts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
			}
		case "document":
			// text/plain documents are just text: inline them. every other
			// document type (pdf, docx...) has no openai chat/completions
			// equivalent, so it rides as a file part and falls back to a
			// data url, which is what multimodal providers accept.
			if p := documentPart(b); p != nil {
				imgParts = append(imgParts, p)
			}
		}
	}
	flush()

	if len(result) == 0 {
		return []map[string]any{{"role": "user", "content": ""}}
	}
	return result
}

func convertAssistantMessage(raw json.RawMessage) []map[string]any {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []map[string]any{{"role": "assistant", "content": s}}
	}
	var blocks []anthropicBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return []map[string]any{{"role": "assistant", "content": ""}}
	}

	var textParts []string
	var toolCalls []map[string]any
	var thinkingParts []string
	var thinkingSigs []string
	thinkingRedacted := false

	for _, b := range blocks {
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
		case "thinking", "redacted_thinking":
			// zen's interleaved-reasoning models (space-bunny, big-pickle)
			// expect prior reasoning back on assistant messages as
			// reasoning_content, matching opencode's transform. the
			// signature rides along so providers that verify the chain
			// keep working; redacted blocks stay typed.
			if b.Thinking != "" {
				thinkingParts = append(thinkingParts, b.Thinking)
			}
			if b.Signature != "" {
				thinkingSigs = append(thinkingSigs, b.Signature)
			}
			if b.Type == "redacted_thinking" {
				thinkingRedacted = true
			}
		case "tool_use":
			input := "{}"
			if len(b.Input) > 0 && string(b.Input) != "null" {
				input = string(b.Input)
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   b.ID,
				"type": "function",
				"function": map[string]any{
					"name":      b.Name,
					"arguments": input,
				},
			})
		}
	}

	// empty turns stay valid upstream: nil content is rejected, "" is not.
	msg := map[string]any{"role": "assistant", "content": strings.Join(textParts, "\n")}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	if len(thinkingParts) > 0 {
		msg["reasoning_content"] = strings.Join(thinkingParts, "\n")
	}
	// preserve the provider signature chain and the redacted marker so
	// verified-reasoning providers keep working across the conversion.
	if len(thinkingSigs) > 0 {
		msg["reasoning_content_signature"] = strings.Join(thinkingSigs, "\n")
	}
	if thinkingRedacted {
		msg["reasoning_content_redacted"] = true
	}
	return []map[string]any{msg}
}

// imageURL converts an anthropic image source block to a url usable as an
// openai image_url part. returns "" when the source carries no usable data.
func imageURL(src *struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url"`
}) string {
	if src == nil {
		return ""
	}
	mt := src.MediaType
	if mt == "" {
		mt = "image/jpeg"
	}
	// claude code sends base64 without an explicit type sometimes, so prefer
	// whichever field is populated rather than trusting src.Type.
	if src.Data != "" {
		return "data:" + mt + ";base64," + src.Data
	}
	return src.URL
}

// documentPart converts an anthropic document block into an openai content
// part. text-ish documents are inlined as text (universally understood);
// binary ones become a file part carrying a data url, and a bare url source
// stays a plain url. returns nil when there is nothing usable.
func documentPart(b anthropicBlock) map[string]any {
	// anthropic ships some documents as {"type":"text","data":"..."} with no
	// source, and some as source.type=="text".
	if b.Source == nil {
		if b.Type == "text" && b.Text != "" {
			return map[string]any{"type": "text", "text": b.Text}
		}
		return nil
	}
	s := b.Source
	mt := s.MediaType
	if mt == "" {
		mt = "application/octet-stream"
	}
	if s.Type == "text" || strings.HasPrefix(mt, "text/") {
		if s.Data == "" {
			return nil
		}
		if dec, err := base64.StdEncoding.DecodeString(s.Data); err == nil {
			return map[string]any{"type": "text", "text": string(dec)}
		}
		// already plain text, not base64
		return map[string]any{"type": "text", "text": s.Data}
	}
	if s.Data != "" {
		return map[string]any{
			"type": "file",
			"file": map[string]any{"filename": docName(mt), "file_data": "data:" + mt + ";base64," + s.Data},
		}
	}
	if s.URL != "" {
		return map[string]any{"type": "file", "file": map[string]any{"filename": docName(mt), "file_url": s.URL}}
	}
	return nil
}

// docName is a filename hint from a media type; zen ignores it but some
// providers reject a file part without one.
func docName(mt string) string {
	if i := strings.IndexByte(mt, '/'); i > 0 {
		return "attachment" + mt[i:]
	}
	return "attachment"
}

// contentOf builds a user message body: a plain string when there are no
// parts, a parts array once there are.
func contentOf(text []string, imgs []map[string]any) any {
	if len(imgs) == 0 {
		return strings.Join(text, "\n")
	}
	arr := make([]any, 0, len(text)+len(imgs))
	for _, t := range text {
		arr = append(arr, map[string]any{"type": "text", "text": t})
	}
	for _, im := range imgs {
		arr = append(arr, im)
	}
	return arr
}

// toolResultParts splits a tool_result body into its text and its images.
// images need a separate multimodal
// user message because role:"tool" cannot carry them.
func toolResultParts(raw json.RawMessage) (string, []map[string]any) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var blocks []anthropicBlock
	if json.Unmarshal(raw, &blocks) != nil {
		// not a known shape: pass it through as text, never as a half-parsed
		// structure. upstream sees a plain string either way.
		return string(raw), nil
	}
	var parts []string
	var imgs []map[string]any
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "image":
			if u := imageURL(b.Source); u != "" {
				imgs = append(imgs, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
			}
		case "document":
			if p := documentPart(b); p != nil {
				imgs = append(imgs, p)
			}
		}
	}
	return strings.Join(parts, "\n"), imgs
}

func convertToolChoice(tc any) any {
	switch v := tc.(type) {
	case string:
		switch v {
		case "auto", "none":
			return v
		case "any":
			return "required"
		default:
			return v
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		switch typ {
		case "auto", "none":
			return typ
		case "any":
			return "required"
		case "tool":
			name, _ := v["name"].(string)
			if name != "" {
				return map[string]any{
					"type":     "function",
					"function": map[string]any{"name": name},
				}
			}
			return "auto"
		}
	}
	return "auto"
}

// --- Response Conversion (non-stream) ---

func openAIToAnthropic(body []byte, clientModel string, tnames toolNames) (out []byte, errMsg string, msgID string) {
	msgID = "msg_" + randHex(24)

	var oaiResp struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				Reasoning *string `json:"reasoning"`
				// ReasoningContent is the reasoning_content spelling; upstreams
				// send one or the other.
				ReasoningContent *string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}

	if err := json.Unmarshal(body, &oaiResp); err != nil {
		return nil, "failed to parse upstream response: " + err.Error(), msgID
	}

	if oaiResp.Error != nil {
		return nil, oaiResp.Error.Message, msgID
	}

	content := make([]map[string]any, 0)

	if oaiResp.Choices != nil && len(oaiResp.Choices) > 0 {
		msg := oaiResp.Choices[0].Message

		// thinking block first, then the answer. reasoning and
		// reasoning_content are the two spellings for the same trace.
		thinking := ""
		if msg.Reasoning != nil && *msg.Reasoning != "" {
			thinking = *msg.Reasoning
		} else if msg.ReasoningContent != nil && *msg.ReasoningContent != "" {
			thinking = *msg.ReasoningContent
		}
		if thinking = stripWaitSentinel(thinking); thinking != "" {
			content = append(content, map[string]any{"type": "thinking", "thinking": thinking})
		}

		text := ""
		if msg.Content != nil && *msg.Content != "" {
			text = *msg.Content
		}
		text = stripWaitSentinel(text)
		if text != "" {
			content = append(content, map[string]any{"type": "text", "text": text})
		}

		for _, tc := range msg.ToolCalls {
			// unparseable args mean a corrupt turn: emit {} and let the
			// model re-ask, instead of inventing a client-visible field.
			input := map[string]any{}
			if tc.Function.Arguments != "" {
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
					acclog.Printf("!! tool args unparseable for %s, sending {}", tnames.resolve(tc.Function.Name))
					input = map[string]any{}
				}
			}
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    tc.ID,
				"name":  tnames.resolve(tc.Function.Name),
				"input": input,
			})
		}
	}

	if len(content) == 0 {
		content = []map[string]any{{"type": "text", "text": ""}}
	}

	stopReason := "end_turn"
	if oaiResp.Choices != nil && len(oaiResp.Choices) > 0 && oaiResp.Choices[0].FinishReason != nil {
		switch *oaiResp.Choices[0].FinishReason {
		case "stop":
			stopReason = "end_turn"
		case "length":
			stopReason = "max_tokens"
		case "tool_calls", "function_call":
			stopReason = "tool_use"
		case "content_filter":
			stopReason = "refusal"
		}
	}

	inputTokens, outputTokens := 0, 0
	if oaiResp.Usage != nil {
		inputTokens = oaiResp.Usage.PromptTokens
		outputTokens = oaiResp.Usage.CompletionTokens
	}

	result := map[string]any{
		"id":            msgID,
		"type":          "message",
		"role":          "assistant",
		"model":         clientModel,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
		},
	}

	out, err := json.Marshal(result)
	if err != nil {
		return nil, "failed to marshal response: " + err.Error(), msgID
	}
	return out, "", msgID
}

func streamAnthropic(w http.ResponseWriter, body []byte, clientModel string, tnames toolNames) (written int64, promptT, compT int) {
	return streamAnthropicReader(context.Background(), w, bytes.NewReader(sseNormalize(stripSentinelSSE(body))), clientModel, tnames, nil)
}

// streamAnthropicReader keeps one message open across an optional streamed nudge.
func streamAnthropicReader(ctx context.Context, w http.ResponseWriter, body io.Reader, clientModel string, tnames toolNames, next func(string, bool) io.ReadCloser) (written int64, promptT, compT int) {
	fl, _ := w.(http.Flusher)

	msgID := "msg_" + randHex(24)
	preamble := fmt.Sprintf(`{"type":"message_start","message":{"id":"%s","type":"message","role":"assistant","model":%s,"content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0}}}`, msgID, jsonStr(clientModel))
	written += writeSSE(w, "message_start", preamble)
	written += writeSSE(w, "ping", `{"type":"ping"}`)
	if fl != nil {
		fl.Flush()
	}

	// the text block opens lazily: a reasoning turn claims index 0 for the
	// thinking block and the answer follows at index 1. post-tool text
	// opens a fresh block after the tools (textCount tracks how many text
	// blocks have opened, so indices never collide).
	thinkIdx := -1
	textIdx := 0
	textOpen := false
	blockIdx := 0
	var toolIdxs []int // block index of each opened tool_use, in open order
	thinkClosed := false
	closeThinking := func() {
		if thinkIdx >= 0 && !thinkClosed {
			written += writeSSE(w, "content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, thinkIdx))
			thinkClosed = true
		}
	}
	closeText := func() {
		if textOpen {
			written += writeSSE(w, "content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, textIdx))
			textOpen = false
		}
	}
	openText := func() {
		if textOpen {
			return
		}
		textIdx = blockIdx
		blockIdx++
		written += writeSSE(w, "content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, textIdx))
		textOpen = true
	}
	hasToolCalls := false
	toolIdxMap := map[int]int{}
	var strip sentinelStripper
	flushStrip := func() {
		if tail := strip.flush(); tail != "" {
			openText()
			written += writeSSE(w, "content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%s}}`, textIdx, jsonStr(tail)))
		}
	}

	var text strings.Builder
	finishReason := "stop"
	for segment := 0; ; segment++ {
		segmentPrompt, segmentComp := 0, 0
		scanner := bufio.NewScanner(body)
		scanner.Buffer(make([]byte, 4096), upstreamBodyLimit)
		for scanner.Scan() {
			if ctx.Err() != nil {
				return
			}
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				break
			}

			var chunk struct {
				Choices []struct {
					// non-stream bodies carry the answer in message, not delta.
					Message *struct {
						Content          string `json:"content"`
						Reasoning        string `json:"reasoning"`
						ReasoningContent string `json:"reasoning_content"`
						ToolCalls        []struct {
							Index    int    `json:"index"`
							ID       string `json:"id"`
							Type     string `json:"type"`
							Function struct {
								Name      string `json:"name"`
								Arguments string `json:"arguments"`
							} `json:"function"`
						} `json:"tool_calls"`
					} `json:"message"`
					Delta struct {
						Content string `json:"content"`
						// Reasoning is the reasoning spelling, ReasoningContent the
						// reasoning_content one; both carry the same trace.
						Reasoning        string `json:"reasoning"`
						ReasoningContent string `json:"reasoning_content"`
						ToolCalls        []struct {
							Index    int    `json:"index"`
							ID       string `json:"id"`
							Type     string `json:"type"`
							Function struct {
								Name      string `json:"name"`
								Arguments string `json:"arguments"`
							} `json:"function"`
						} `json:"tool_calls"`
					} `json:"delta"`
					FinishReason *string `json:"finish_reason"`
				} `json:"choices"`
				Usage *struct {
					PromptTokens     int `json:"prompt_tokens"`
					CompletionTokens int `json:"completion_tokens"`
				} `json:"usage"`
			}

			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue
			}

			// a buffered body reports the whole turn in message; fold it into the
			// delta shape so one code path emits it.
			if len(chunk.Choices) > 0 && chunk.Choices[0].Message != nil {
				m := chunk.Choices[0].Message
				chunk.Choices[0].Delta.Content += m.Content
				chunk.Choices[0].Delta.Reasoning += m.Reasoning
				chunk.Choices[0].Delta.ReasoningContent += m.ReasoningContent
				chunk.Choices[0].Delta.ToolCalls = append(chunk.Choices[0].Delta.ToolCalls, m.ToolCalls...)
				if chunk.Choices[0].FinishReason == nil {
					chunk.Choices[0].FinishReason = strPtr("stop")
				}
			}

			if chunk.Usage != nil {
				if chunk.Usage.PromptTokens > 0 {
					segmentPrompt = chunk.Usage.PromptTokens
				}
				if chunk.Usage.CompletionTokens > 0 {
					segmentComp = chunk.Usage.CompletionTokens
				}
			}

			if len(chunk.Choices) == 0 {
				continue
			}
			choice := chunk.Choices[0]

			// text delta
			// thinking delta, ahead of the text block
			think := choice.Delta.Reasoning
			if think == "" {
				think = choice.Delta.ReasoningContent
			}
			if think != "" {
				if thinkIdx < 0 || thinkClosed {
					closeText()
					thinkIdx = blockIdx
					blockIdx++
					thinkClosed = false
					written += writeSSE(w, "content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"thinking","thinking":""}}`, thinkIdx))
				}
				written += writeSSE(w, "content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"thinking_delta","thinking":%s}}`, thinkIdx, jsonStr(think)))
			}

			// text delta. post-tool text opens a fresh block after the
			// tools instead of being dropped (interleaved models emit text
			// after tool calls; the old gate truncated the turn).
			if choice.Delta.Content != "" {
				text.WriteString(choice.Delta.Content)
				openText()
				if clean := strip.push(choice.Delta.Content); clean != "" {
					written += writeSSE(w, "content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%s}}`, textIdx, jsonStr(clean)))
				}
			}

			// tool call deltas
			if len(choice.Delta.ToolCalls) > 0 {
				if !hasToolCalls {
					hasToolCalls = true
					closeThinking()
					closeText()
				}

				// tools follow whatever blocks already opened (thinking, text).
				// each upstream tool index gets its own block index: deltas for a
				// second call in the same chunk must not reuse the first's index.
				// textIdx (not textOpen) so a closed text block keeps its index.
				// every opened index is recorded so the stop loop closes exactly
				// the tool blocks, even when a text block interleaves between two.
				for _, tc := range choice.Delta.ToolCalls {
					idx, open := toolIdxMap[tc.Index]
					if !open {
						idx = blockIdx
						blockIdx++
						toolIdxs = append(toolIdxs, idx)
						toolIdxMap[tc.Index] = idx
						toolID := tc.ID
						if toolID == "" {
							toolID = "toolu_" + randHex(24)
						}
						written += writeSSE(w, "content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%s,"name":%s,"input":{}}}`, idx, jsonStr(toolID), jsonStr(tnames.resolve(tc.Function.Name))))
					}
					if tc.Function.Arguments != "" {
						written += writeSSE(w, "content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`, idx, jsonStr(tc.Function.Arguments)))
					}
				}
			}

			if choice.FinishReason != nil {
				finishReason = *choice.FinishReason
			}
			if fl != nil {
				fl.Flush()
			}
		}
		promptT += segmentPrompt
		compT += segmentComp
		if ctx.Err() != nil {
			return // client disconnected: nothing left to write to
		}
		if scanner.Err() != nil {
			break // upstream truncated: still close the turn cleanly below
		}
		if segment == 0 && next != nil {
			if continuation := next(text.String(), hasToolCalls); continuation != nil {
				defer continuation.Close()
				flushStrip()
				closeText()
				closeThinking()
				body = continuation
				continue
			}
		}
		break
	}

	// finish reason follows trailing usage and any continuation.
	flushStrip()
	closeText()

	closeThinking()

	// close exactly the tool blocks that opened, whatever their indices.
	for _, idx := range toolIdxs {
		written += writeSSE(w, "content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, idx))
	}

	stopReason := "end_turn"
	switch finishReason {
	case "length":
		stopReason = "max_tokens"
	case "tool_calls":
		stopReason = "tool_use"
	}

	written += writeSSE(w, "message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":"%s","stop_sequence":null},"usage":{"output_tokens":%d}}`, stopReason, compT))
	written += writeSSE(w, "message_stop", `{"type":"message_stop"}`)
	n, _ := w.Write([]byte("data: [DONE]\n\n"))
	written += int64(n)
	if fl != nil {
		fl.Flush()
	}
	return
}

// respStreamer carries per-turn state so several buffered /responses SSE
// bodies can be emitted as one continuous Anthropic turn.
type respStreamer struct {
	w           http.ResponseWriter
	fl          http.Flusher
	hasFlusher  bool
	clientModel string
	tnames      toolNames
	written     int64
	promptT     int
	compT       int
	blockIdx    int
	textIdx     int
	textOpen    bool
	strip       sentinelStripper
	toolIdx     map[string]int
	hasToolUse  bool
	sentStop    bool
}

func (s *respStreamer) flush() {
	if s.hasFlusher {
		s.fl.Flush()
	}
}

func (s *respStreamer) openText() {
	if s.textOpen {
		return
	}
	s.textIdx = s.blockIdx
	s.blockIdx++
	s.textOpen = true
	s.written += writeSSE(s.w, "content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, s.textIdx))
}

func (s *respStreamer) closeText() {
	if !s.textOpen {
		return
	}
	s.textOpen = false
	if tail := s.strip.flush(); tail != "" {
		s.written += writeSSE(s.w, "content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%s}}`, s.textIdx, jsonStr(tail)))
	}
	s.written += writeSSE(s.w, "content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, s.textIdx))
}

func (s *respStreamer) openTool(id, callID, name string) {
	if _, ok := s.toolIdx[id]; ok {
		return
	}
	s.toolIdx[id] = s.blockIdx
	s.blockIdx++
	s.hasToolUse = true
	s.written += writeSSE(s.w, "content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%s,"name":%s,"input":{}}}`, s.toolIdx[id], jsonStr(callID), jsonStr(s.tnames.resolve(name))))
}

func (s *respStreamer) closeTool(id string) {
	idx, ok := s.toolIdx[id]
	if !ok {
		return
	}
	s.written += writeSSE(s.w, "content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, idx))
	delete(s.toolIdx, id)
}

// closeBlocks ends open blocks without ending the turn, so another
// buffered body can continue on fresh indices.
func (s *respStreamer) closeBlocks() {
	s.closeText()
	for id := range s.toolIdx {
		s.closeTool(id)
	}
}

func (s *respStreamer) finish() {
	if s.sentStop {
		return
	}
	s.sentStop = true
	s.closeBlocks()
	stopReason := "end_turn"
	if s.hasToolUse {
		stopReason = "tool_use"
	}
	s.written += writeSSE(s.w, "message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%s,"stop_sequence":null},"usage":{"output_tokens":%d}}`, jsonStr(stopReason), s.compT))
	s.written += writeSSE(s.w, "message_stop", `{"type":"message_stop"}`)
	n, _ := s.w.Write([]byte("data: [DONE]\n\n"))
	s.written += int64(n)
	s.flush()
}

func (s *respStreamer) feed(body []byte, last bool) {
	// proxy-internal marker must never reach the client.
	body = stripSentinelSSE(body)
	lines := strings.Split(string(body), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var evt struct {
			Type      string `json:"type"`
			Delta     string `json:"delta"`
			Arguments string `json:"arguments"`
			ItemID    string `json:"item_id"`
			Item      *struct {
				ID        string `json:"id"`
				Type      string `json:"type"`
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"item"`
			Response *struct {
				Usage *struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
					TotalTokens  int `json:"total_tokens"`
				} `json:"usage"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(data), &evt); err != nil {
			continue
		}

		switch evt.Type {
		case "response.output_item.added":
			if evt.Item != nil && evt.Item.Type == "function_call" {
				// resolve once, use the same id for open/delta/close:
				// tools with an empty ItemID never closed before.
				id := evt.ItemID
				if id == "" {
					id = evt.Item.ID
				}
				if id == "" {
					id = evt.Item.CallID
				}
				s.openTool(id, evt.Item.CallID, evt.Item.Name)
			}
		case "response.function_call_arguments.delta":
			// open on demand: arg deltas can precede the added event.
			id := evt.ItemID
			if id == "" && evt.Item != nil {
				id = evt.Item.ID
				if id == "" {
					id = evt.Item.CallID
				}
			}
			if id != "" && evt.Delta != "" {
				name := ""
				if evt.Item != nil {
					name = evt.Item.Name
				}
				s.openTool(id, id, name)
				s.written += writeSSE(s.w, "content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`, s.toolIdx[id], jsonStr(evt.Delta)))
			}
		case "response.output_item.done":
			if evt.Item != nil && evt.Item.Type == "function_call" {
				id := evt.ItemID
				if id == "" {
					id = evt.Item.ID
				}
				if id == "" {
					id = evt.Item.CallID
				}
				s.closeTool(id)
			}
		case "response.output_text.delta":
			s.openText()
			if evt.Delta != "" {
				if clean := s.strip.push(evt.Delta); clean != "" {
					s.written += writeSSE(s.w, "content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%s}}`, s.textIdx, jsonStr(clean)))
				}
			}
		case "response.output_text.done", "response.content_part.done":
			s.closeText()
		case "response.completed":
			if evt.Response != nil && evt.Response.Usage != nil {
				s.compT += evt.Response.Usage.OutputTokens
				s.promptT += evt.Response.Usage.InputTokens
			}
			if last {
				s.finish()
				return
			}
			s.closeBlocks()
		}
	}
}

// streamResponsesLiveAnthropic converts an upstream /responses SSE body to
// Anthropic events while reading: preamble + first delta flush on zen's
// first byte, so client TTFT is upstream TTFT. ctx aborts the read and the
// upstream body close cancels the in-flight request.
func streamResponsesLiveAnthropic(ctx context.Context, w http.ResponseWriter, body io.Reader, clientModel string, tnames toolNames) (written int64, promptT, compT int) {
	return streamResponsesAnthropicReader(ctx, w, body, clientModel, tnames, nil)
}

func streamResponsesAnthropicReader(ctx context.Context, w http.ResponseWriter, body io.Reader, clientModel string, tnames toolNames, next func(string, bool) io.ReadCloser) (written int64, promptT, compT int) {
	fl, _ := w.(http.Flusher)
	s := &respStreamer{w: w, clientModel: clientModel, tnames: tnames, toolIdx: map[string]int{}, textIdx: -1}
	if fl != nil {
		s.fl = fl
		s.hasFlusher = true
	}

	msgID := "msg_" + randHex(24)
	preamble := fmt.Sprintf(`{"type":"message_start","message":{"id":"%s","type":"message","role":"assistant","model":%s,"content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0}}}`, msgID, jsonStr(clientModel))
	s.written += writeSSE(w, "message_start", preamble)
	s.written += writeSSE(w, "ping", `{"type":"ping"}`)
	s.flush()

	var text strings.Builder
	for segment := 0; ; segment++ {
		scanner := bufio.NewScanner(body)
		scanner.Buffer(make([]byte, 4096), upstreamBodyLimit)
		for scanner.Scan() {
			if ctx.Err() != nil {
				return s.written, s.promptT, s.compT
			}
			line := scanner.Text()
			if strings.TrimSpace(line) == "data: [DONE]" {
				break
			}
			if strings.HasPrefix(line, "data: ") {
				var evt struct {
					Type  string `json:"type"`
					Delta string `json:"delta"`
				}
				if json.Unmarshal([]byte(line[6:]), &evt) == nil && evt.Type == "response.output_text.delta" {
					text.WriteString(evt.Delta)
				}
			}
			s.feed([]byte(line+"\n"), false)
			s.flush()
		}
		if scanner.Err() != nil || ctx.Err() != nil {
			return s.written, s.promptT, s.compT
		}
		if segment == 0 && next != nil {
			if continuation := next(text.String(), s.hasToolUse); continuation != nil {
				defer continuation.Close()
				s.closeBlocks()
				body = continuation
				continue
			}
		}
		break
	}
	s.finish()
	return s.written, s.promptT, s.compT
}

// streamResponsesMerged emits several buffered /responses SSE bodies as one
// Anthropic turn: one preamble, continuous block indices, one stop.
func streamResponsesMerged(w http.ResponseWriter, bodies [][]byte, clientModel string, tnames toolNames) (written int64, promptT, compT int) {
	fl, _ := w.(http.Flusher)
	s := &respStreamer{w: w, clientModel: clientModel, tnames: tnames, toolIdx: map[string]int{}, textIdx: -1}
	if fl != nil {
		s.fl = fl
		s.hasFlusher = true
	}

	msgID := "msg_" + randHex(24)
	preamble := fmt.Sprintf(`{"type":"message_start","message":{"id":"%s","type":"message","role":"assistant","model":%s,"content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0}}}`, msgID, jsonStr(clientModel))
	s.written += writeSSE(w, "message_start", preamble)
	s.written += writeSSE(w, "ping", `{"type":"ping"}`)
	s.flush()

	for i, b := range bodies {
		s.feed(b, i == len(bodies)-1)
	}
	s.finish()
	return s.written, s.promptT, s.compT
}

// toolsOffered reports whether a request body offers any non-empty-named
// function tools. works for chat and /responses shapes alike. tool_choice
// "none" offers nothing: the gate stubs on tool-less bodies must never
// trigger a nudge.
func toolsOffered(body []byte) bool {
	var m struct {
		ToolChoice any `json:"tool_choice"`
		Tools      []struct {
			Type     string `json:"type"`
			Name     string `json:"name"`
			Function *struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return false
	}
	switch tc := m.ToolChoice.(type) {
	case string:
		if tc == "none" {
			return false
		}
	case map[string]any:
		if t, _ := tc["type"].(string); t == "none" {
			return false
		}
	}
	for _, t := range m.Tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		name := t.Name
		if t.Function != nil {
			name = t.Function.Name
		}
		if strings.TrimSpace(name) != "" {
			return true
		}
	}
	return false
}

// waitSentinel is what the model is told to emit when it is waiting on
// the user or done. A retry body holding only this is swallowed so the
// turn ends on the first body's output instead of a redundant summary.
const waitSentinel = "SPARK_WAITING_FOR_INPUT"

// nudgeContinuation builds a follow-up /responses body: the prior input plus
// the assistant's text and a nudge to use the offered tools. Tool calls
// always win; if the model is waiting on the user or done, it must emit
// only waitSentinel so the retry can be swallowed.
func nudgeContinuation(oaiBody []byte, priorText string) []byte {
	var m map[string]any
	if err := json.Unmarshal(oaiBody, &m); err != nil {
		return nil
	}
	in, _ := m["input"].([]any)
	in = append(in, map[string]any{"role": "assistant", "content": priorText})
	in = append(in, map[string]any{"role": "user", "content": nudgeReprompt()})
	m["input"] = in
	delete(m, "nudge_no_tools")
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}

// scanChatSSE returns the concatenated delta content and whether any
// tool_calls deltas appeared in a chat/completions SSE body. buffered
// plain-json bodies (a single object, no data: lines) are normalized
// first so the freepi path scans the same way.
func scanChatSSE(body []byte) (text string, hasCalls bool) {
	body = sseNormalize(body)
	var sb strings.Builder
	for _, raw := range bytes.Split(body, []byte("\n")) {
		line := string(bytes.TrimSpace(raw))
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var ch struct {
			Choices []struct {
				Message *struct {
					Content   string `json:"content"`
					ToolCalls []any  `json:"tool_calls"`
				} `json:"message"`
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []any  `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			continue
		}
		for _, c := range ch.Choices {
			if c.Message != nil {
				sb.WriteString(c.Message.Content)
				if len(c.Message.ToolCalls) > 0 {
					hasCalls = true
				}
			}
			sb.WriteString(c.Delta.Content)
			if len(c.Delta.ToolCalls) > 0 {
				hasCalls = true
			}
		}
	}
	return sb.String(), hasCalls
}

// scanChatJSON is scanChatSSE for a folded chat completion object.
func scanChatJSON(body []byte) (text string, hasCalls bool) {
	var c struct {
		Choices []struct {
			Message struct {
				Content   any   `json:"content"`
				ToolCalls []any `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &c); err != nil || len(c.Choices) == 0 {
		return "", false
	}
	m := c.Choices[0].Message
	switch t := m.Content.(type) {
	case string:
		text = t
	case []any:
		for _, p := range t {
			if pm, ok := p.(map[string]any); ok {
				if s, _ := pm["text"].(string); s != "" {
					text += s
				}
			}
		}
	}
	return text, len(m.ToolCalls) > 0
}

// nudgeReprompt is the shared reprompt text for both shapes.
func nudgeReprompt() string {
	return "You have tools available in this conversation. If any tool applies to the task above, call it now instead of describing what you would do. " +
		"Do not narrate progress or restate what you already wrote. " +
		"If you are waiting on the user to answer, or the task is fully done, reply with exactly " + waitSentinel + " and nothing else. " +
		"Do not answer your own questions or assume the user's reply."
}

// nudgeContinuationChat builds a follow-up chat/completions body: the prior
// messages plus the assistant's text and a nudge to use the offered tools.
func nudgeContinuationChat(oaiBody []byte, priorText string) []byte {
	var m map[string]any
	if err := json.Unmarshal(oaiBody, &m); err != nil {
		return nil
	}
	msgs, _ := m["messages"].([]any)
	msgs = append(msgs, map[string]any{"role": "assistant", "content": priorText})
	msgs = append(msgs, map[string]any{"role": "user", "content": nudgeReprompt()})
	m["messages"] = msgs
	delete(m, "nudge_no_tools")
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}

// stripChatTerminal drops the terminal data: lines ([DONE] plus the final
// finish_reason chunk) off a chat SSE body so concatenation with a retry
// body plays as one stream. non-terminal lines pass through untouched.
func stripChatTerminal(body []byte) []byte {
	lines := strings.Split(string(body), "\n")
	end := len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" || strings.HasPrefix(t, "event: ") {
			continue
		}
		if !strings.HasPrefix(t, "data: ") {
			break
		}
		data := strings.TrimPrefix(t, "data: ")
		if data == "[DONE]" {
			end = i
			continue
		}
		var ch struct {
			Choices []struct {
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &ch) == nil && len(ch.Choices) > 0 &&
			ch.Choices[0].FinishReason != nil {
			end = i
			continue
		}
		break
	}
	return []byte(strings.Join(lines[:end], "\n"))
}

// mergeChatSSE concatenates buffered chat SSE bodies into one stream:
// terminal lines are stripped from every body but the last.
func mergeChatSSE(bodies [][]byte) []byte {
	var out bytes.Buffer
	for i, b := range bodies {
		if i < len(bodies)-1 {
			b = stripChatTerminal(b)
		}
		out.Write(b)
		if !bytes.HasSuffix(out.Bytes(), []byte("\n")) {
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}

// nudgeChatGates runs the shared gates: master switch, per-model flag,
// offered tools, streak cap, and non-empty tool-less text. returns the
// text when the nudge may fire, "" when it must not.
func nudgeChatGates(session, clientModel string, posted []byte, text string, hasCalls bool) string {
	if !nudgeEnabled(clientModel) {
		dbglog.Printf("  nudge skip model=%s reason=disabled", clientModel)
		return ""
	}
	if !toolsOffered(posted) {
		dbglog.Printf("  nudge skip model=%s reason=no-tools-offered", clientModel)
		return ""
	}
	if hasCalls {
		nudgeStreaks.note(session, true)
		return ""
	}
	if !nudgeStreaks.allowed(session) {
		dbglog.Printf("  nudge skip model=%s reason=streak-cap", clientModel)
		return ""
	}
	if strings.Contains(text, waitSentinel) {
		return ""
	}
	if strings.TrimSpace(text) == "" {
		dbglog.Printf("  nudge skip model=%s reason=empty-text", clientModel)
		return ""
	}
	return text
}

// noteNudgeRetry records the streak and logs after a nudge repost.
// returns true when the retry called tools (caller keeps it).
func noteNudgeRetry(session, clientModel string, nb2 []byte, text string, hasCalls bool, backend string) bool {
	if !hasCalls {
		nudgeStreaks.note(session, false)
		if strings.TrimSpace(text) == waitSentinel {
			acclog.Printf("  %s nudge model=%s waiting, swallowing retry", backend, clientModel)
		} else {
			acclog.Printf("  %s nudge model=%s no-calls, swallowing retry text=%q", backend, clientModel, errSnippet([]byte(text), 120))
		}
		return false
	}
	nudgeStreaks.note(session, true)
	acclog.Printf("  %s nudge model=%s calls=%v bytes=%d", backend, clientModel, hasCalls, len(nb2))
	return true
}

// tryNudgeChat runs the no-tools continue for chat/completions bodies:
// same gates as the responses path, continuation on messages[], retry
// scanned as SSE or folded JSON. returns the retry body, or nil when the
// nudge doesn't fire (or the retry called nothing) so callers keep the
// original. backend names the log prefix (opencode, freepi, antseed, nvidia).
func tryNudgeChat(session, clientModel, backend string, posted, rb []byte, post func(nb []byte) []byte) []byte {
	text, hasCalls := scanChatSSE(rb)
	if nudgeChatGates(session, clientModel, posted, text, hasCalls) == "" {
		return nil
	}
	nb := nudgeContinuationChat(posted, text)
	if nb == nil {
		return nil
	}
	nb2 := post(nb)
	if nb2 == nil {
		return nil
	}
	text2, nHasCalls := scanChatSSE(nb2)
	if !noteNudgeRetry(session, clientModel, nb2, text2, nHasCalls, backend) {
		return nil
	}
	return nb2
}

// tryNudgeJSON is tryNudgeChat for already-folded chat completion objects
// (backends that fetch with stream=false): scan/merge JSON throughout.
func tryNudgeJSON(session, clientModel, backend string, posted, rb []byte, post func(nb []byte) []byte) []byte {
	text, hasCalls := scanChatJSON(rb)
	if nudgeChatGates(session, clientModel, posted, text, hasCalls) == "" {
		return nil
	}
	nb := nudgeContinuationChat(posted, text)
	if nb == nil {
		return nil
	}
	nb2 := post(nb)
	if nb2 == nil {
		return nil
	}
	text2, nHasCalls := scanChatJSON(nb2)
	if !noteNudgeRetry(session, clientModel, nb2, text2, nHasCalls, backend) {
		return nil
	}
	return nb2
}

// mergeChatJSON folds a nudge retry into a chat completion object: texts
// concatenate, the retry's tool calls win, usage sums when both present.
func mergeChatJSON(first, retry []byte) []byte {
	var f, r struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Role      string `json:"role"`
				Content   any    `json:"content"`
				ToolCalls []any  `json:"tool_calls"`
			} `json:"message"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if json.Unmarshal(first, &f) != nil || json.Unmarshal(retry, &r) != nil ||
		len(f.Choices) == 0 || len(r.Choices) == 0 {
		return first
	}
	textOf := func(v any) string {
		s, _ := v.(string)
		return s
	}
	ct := textOf(f.Choices[0].Message.Content) + textOf(r.Choices[0].Message.Content)
	calls := r.Choices[0].Message.ToolCalls
	if len(calls) == 0 {
		calls = f.Choices[0].Message.ToolCalls
	}
	f.Choices[0].Message.Content = stripWaitSentinel(ct)
	if len(calls) > 0 {
		f.Choices[0].Message.ToolCalls = calls
	}
	if fr := r.Choices[0].FinishReason; fr != nil {
		f.Choices[0].FinishReason = fr
	}
	if len(r.Usage) > 0 && f.Usage == nil {
		f.Usage = map[string]any{}
	}
	for k, rv := range r.Usage {
		if fv, ok := f.Usage[k]; ok {
			if a, ok := fv.(float64); ok {
				if b, ok := rv.(float64); ok {
					f.Usage[k] = a + b
					continue
				}
			}
		}
		f.Usage[k] = rv
	}
	if b, err := json.Marshal(f); err == nil {
		return b
	}
	return first
}

// stripWaitSentinel removes the internal wait sentinel so it never reaches
// the client. the model sometimes echoes it with extra words attached.
func stripWaitSentinel(s string) string {
	return strings.ReplaceAll(s, waitSentinel, "")
}

// stripSentinelSSE removes the internal wait sentinel from a buffered
// /responses SSE body. belt and suspenders behind the swallow-non-calls
// rule: the marker is proxy-internal and must never reach the client.
func stripSentinelSSE(body []byte) []byte {
	if !strings.Contains(string(body), waitSentinel) {
		return body
	}
	return []byte(strings.ReplaceAll(string(body), waitSentinel, ""))
}

// sentinelStripper removes waitSentinel from a stream of text deltas. only
// the trailing overlap with a sentinel prefix is held back, so ordinary
// text flows through unbuffered while a marker split across deltas is
// still caught; flush emits whatever is left at block close.
type sentinelStripper struct {
	pending string
}

func (s *sentinelStripper) push(d string) string {
	s.pending += d
	cleaned := strings.ReplaceAll(s.pending, waitSentinel, "")
	hold := 0
	max := len(cleaned)
	if max > len(waitSentinel)-1 {
		max = len(waitSentinel) - 1
	}
	for n := max; n > 0; n-- {
		if strings.HasPrefix(waitSentinel, cleaned[len(cleaned)-n:]) {
			hold = n
			break
		}
	}
	out := cleaned[:len(cleaned)-hold]
	s.pending = cleaned[len(cleaned)-hold:]
	return out
}

func (s *sentinelStripper) flush() string {
	out := strings.ReplaceAll(s.pending, waitSentinel, "")
	s.pending = ""
	return out
}

// scanResponsesSSE returns the concatenated output text and whether any
// function_call output item appeared in a buffered /responses SSE body.
// it covers delta events plus the done/completed shapes spark actually
// sends when text arrives without deltas or calls complete in one item.
func scanResponsesSSE(body []byte) (text string, hasCalls bool) {
	var sb strings.Builder
	for _, raw := range bytes.Split(body, []byte("\n")) {
		line := string(bytes.TrimSpace(raw))
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var evt struct {
			Type     string         `json:"type"`
			Delta    string         `json:"delta"`
			Text     string         `json:"text"`
			Item     map[string]any `json:"item"`
			Response *struct {
				Output []map[string]any `json:"output"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(data), &evt); err != nil {
			continue
		}
		switch evt.Type {
		case "response.output_text.delta":
			sb.WriteString(evt.Delta)
		case "response.output_text.done", "response.content_part.done":
			sb.WriteString(evt.Text)
		case "response.output_item.added", "response.output_item.done":
			t, _ := evt.Item["type"].(string)
			if t == "function_call" {
				hasCalls = true
			} else if t == "message" {
				sb.WriteString(responseItemText(evt.Item))
			}
		case "response.completed", "response.incomplete", "response.failed":
			if evt.Response != nil {
				for _, it := range evt.Response.Output {
					t, _ := it["type"].(string)
					if t == "function_call" {
						hasCalls = true
					} else if t == "message" {
						sb.WriteString(responseItemText(it))
					}
				}
			}
		}
	}
	return sb.String(), hasCalls
}

// responseItemText pulls output_text out of a /responses message item.
func responseItemText(item map[string]any) string {
	var sb strings.Builder
	content, _ := item["content"].([]any)
	for _, c := range content {
		cm, _ := c.(map[string]any)
		if cm == nil {
			continue
		}
		t, _ := cm["type"].(string)
		if t == "output_text" || t == "text" {
			s, _ := cm["text"].(string)
			sb.WriteString(s)
		}
	}
	return sb.String()
}

// nudgeState tracks consecutive tool-less turns per session, so the
// no-tools reprompt stops after nudgeMaxStreak rounds instead of firing on
// every tool-less turn forever. a tool-calling turn resets the streak.
type nudgeState struct {
	mu sync.Mutex
	n  map[string]int
}

const nudgeMaxStreak = 2

func (s *nudgeState) allowed(session string) bool {
	if session == "" {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n[session] < nudgeMaxStreak
}

func (s *nudgeState) note(session string, calledTools bool) {
	if session == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.n == nil {
		s.n = map[string]int{}
	}
	if calledTools {
		delete(s.n, session)
		return
	}
	// bounded without punishing everyone: evict one arbitrary entry
	// instead of wiping every session's streak cap.
	if len(s.n) > 8192 {
		for k := range s.n {
			delete(s.n, k)
			break
		}
	}
	s.n[session]++
}

var nudgeStreaks = &nudgeState{}

// tryNudgeResponses runs the no-tools continue: gates on the master switch,
// the per-model flag, offered tools, and response text, then posts the
// continuation via post. returns the retry body, or nil when the nudge
// doesn't fire (or the retry is wait-only) so callers keep the original.
// callers merge: stream sites append the retry as a second body,
// buffered sites fold mergeResponsesSSE([rb, retry]) before converting.
func tryNudgeResponses(session, clientModel string, posted, rb []byte, post func(nb []byte) []byte) []byte {
	text, hasCalls := scanResponsesSSE(rb)
	// no question exemption: the reprompt tells the model to emit
	// only waitSentinel when waiting, so nudging past a question
	// is safe. exempting questions let greetings loop forever
	// with no tool calls and no nudge ever firing.
	if nudgeChatGates(session, clientModel, posted, text, hasCalls) == "" {
		return nil
	}
	nb := nudgeContinuation(posted, text)
	if nb == nil {
		return nil
	}
	nb2 := post(nb)
	if nb2 == nil {
		return nil
	}
	text2, nHasCalls := scanResponsesSSE(nb2)
	if !noteNudgeRetry(session, clientModel, nb2, text2, nHasCalls, "opencode") {
		return nil
	}
	return nb2
}

// --- Handlers ---

// handleClassifier answers Claude Code's permission-classifier endpoint.
// Claude Code POSTs a permission_request to {base}/v1/messages/classifier when a
// tool needs approval; the gateway returns a decision. We always approve so the
// session never blocks on an interactive prompt. The request body is ignored.
func handleClassifier(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only POST is supported")
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"result": "decision",
		"decision": map[string]any{
			"allow":  true,
			"reason": "auto-approved by proxy",
		},
	})
}

func (p *Pool) handleAnthropic(w http.ResponseWriter, r *http.Request, start time.Time, reqID string) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	w.Header().Set("Access-Control-Allow-Methods", "*")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only POST is supported")
		return
	}

	// bound the body read: a client that stalls mid-upload otherwise pins
	// this goroutine before the "-> POST" log, which reads as a hang with
	// no trace past "--> POST". closing the body unblocks the stalled read.
	readCtx, readCancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer readCancel()
	go func() {
		<-readCtx.Done()
		r.Body.Close()
	}()
	body, err := io.ReadAll(io.LimitReader(r.Body, bodyLimit))
	if err != nil {
		if readCtx.Err() != nil {
			writeAnthropicError(w, http.StatusRequestTimeout, "invalid_request_error", "request body read timed out")
			return
		}
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	r.Body.Close()

	// RTK: compress tool_result text before the PII guard masks, so filters
	// see original bytes and vault placeholders are never chopped.
	body = rtkCompressBody(body)

	// same deal as ServeHTTP: mask the inbound body, unmask on the way out.
	injectDiscloseLine(&body)
	pg := guardForRequest(r)
	body = pg.MaskBody(body)
	if dw := wrapPII(w, pg); dw != nil {
		w = dw
		defer dw.finish()
	}

	if r.URL.Path == "/v1/messages/count_tokens" {
		handleCountTokens(w, body)
		return
	}

	if used, limit := p.semStats(); used >= limit {
		acclog.Printf("%s ... all %d slots busy, queueing", reqID, limit)
	}
	if !p.semAcquire(r.Context()) {
		acclog.Printf("!! %s client disconnected while queueing", reqID)
		writeAnthropicError(w, 499, "api_error", "client disconnected")
		return
	}
	p.concurrent.Add(1)
	defer func() {
		p.concurrent.Add(-1)
		p.semRelease()
	}()

	oaiBody, clientModel, upstreamModel, tnames, isStream, err := anthropicRequestToOpenAI(body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	acclog.Printf("%s -> POST /v1/messages model=%s upstream=%s stream=%v bytes=%d", reqID, clientModel, upstreamModel, isStream, len(oaiBody))
	if tr := traceFrom(r.Context()); tr != nil {
		tr.setModel(clientModel, backendForModel(upstreamModel), isStream)
	}

	if strings.HasPrefix(upstreamModel, "route/") {
		if !routeEnabled() {
			acclog.Printf("<- 503 POST /v1/messages model=%s (route disabled)", clientModel)
			writeAnthropicError(w, http.StatusServiceUnavailable, "api_error", "route is disabled; set route.enabled: true in config.yml")
			return
		}
		key := strings.TrimPrefix(upstreamModel, "route/")
		if strings.TrimSpace(key) == "" {
			writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "route: empty model name")
			return
		}
		acclog.Printf("%s routed to route=%s", reqID, key)
		p.handleRouteAnthropic(w, r, body, oaiBody, clientModel, key, tnames, isStream, start, reqID)
		return
	}

	// Route opencode/* models to zen endpoint
	if strings.HasPrefix(upstreamModel, "opencode/") {
		if !zenEnabled() {
			acclog.Printf("<- 503 POST /v1/messages model=%s (zen disabled)", clientModel)
			writeAnthropicError(w, http.StatusServiceUnavailable, "api_error", "opencode zen is disabled; set zen.enabled: true in config.yml")
			return
		}
		p.concurrentZen.Add(1)
		defer p.concurrentZen.Add(-1)
		acclog.Printf("%s routed to opencode model=%s", reqID, upstreamModel)
		p.handleOpenCodeAnthropic(w, r, body, oaiBody, clientModel, upstreamModel, tnames, isStream, start, reqID)
		return
	}

	// Route freeepi/* models to api.freepi.ai
	if strings.HasPrefix(upstreamModel, "freeepi/") {
		if !freepiEnabled() {
			acclog.Printf("<- 503 POST /v1/messages model=%s (freepi disabled)", clientModel)
			writeAnthropicError(w, http.StatusServiceUnavailable, "api_error", "freepi is disabled; set freepi.enabled: true with accounts in config.yml")
			return
		}
		acclog.Printf("%s routed to freepi model=%s", reqID, upstreamModel)
		p.handleFreepiAnthropic(w, r, oaiBody, clientModel, upstreamModel, tnames, isStream, start, reqID)
		return
	}

	// Route antseed/* models to the p2p backend
	if strings.HasPrefix(upstreamModel, "antseed/") {
		if !antseedEnabled() {
			acclog.Printf("<- 503 POST /v1/messages model=%s (antseed disabled)", clientModel)
			writeAnthropicError(w, http.StatusServiceUnavailable, "api_error", "antseed is disabled; set antseed.enabled: true in config.yml")
			return
		}
		acclog.Printf("%s routed to antseed model=%s", reqID, upstreamModel)
		p.handleAntseedAnthropic(w, r, oaiBody, clientModel, upstreamModel, tnames, isStream, start, reqID)
		return
	}

	// nous/<model> is the listed form; the portal takes openai chat/completions.
	if strings.HasPrefix(upstreamModel, "nous/") {
		if !nousEnabled() {
			acclog.Printf("<- 503 POST /v1/messages model=%s (nous disabled)", clientModel)
			writeAnthropicError(w, http.StatusServiceUnavailable, "api_error", "nous portal is disabled or has no keys; set nous.enabled: true and add nous_keys to config.yml")
			return
		}
		upstreamModel = strings.TrimPrefix(upstreamModel, "nous/")
		if b, err := setReqModel(oaiBody, upstreamModel); err == nil {
			oaiBody = b
		}
		acclog.Printf("%s routed to nous model=%s", reqID, upstreamModel)
		p.handleNousAnthropic(w, r, oaiBody, clientModel, upstreamModel, tnames, isStream, start, reqID)
		return
	}

	// nvidia/ prefix is the listed form; bare ids stay nvidia for compat.
	if strings.HasPrefix(upstreamModel, "nvidia/") {
		upstreamModel = strings.TrimPrefix(upstreamModel, "nvidia/")
		if b, err := setReqModel(oaiBody, upstreamModel); err == nil {
			oaiBody = b
		}
	}

	// disabled or keyless mode serves opencode/* only; nim models need keys.
	if !cfg().Nvidia.Enabled {
		acclog.Printf("<- 503 POST /v1/messages model=%s (nvidia disabled)", clientModel)
		writeAnthropicError(w, http.StatusServiceUnavailable, "api_error", "nvidia backend is disabled; set nvidia.enabled: true in config.yml")
		return
	}
	if len(p.keys) == 0 {
		acclog.Printf("<- 503 POST /v1/messages model=%s (keyless: no nvidia keys)", clientModel)
		writeAnthropicError(w, http.StatusServiceUnavailable, "api_error", "no nvidia keys configured; use an opencode/<model> free model or add nvidia_keys to config.yml")
		return
	}

	target := nvidiaBase + "/chat/completions"
	acclog.Printf("%s routed to nvidia target=%s model=%s", reqID, target, upstreamModel)
	p.concurrentNV.Add(1)
	defer p.concurrentNV.Add(-1)
	p.handleNvidiaAnthropic(w, r, oaiBody, body, clientModel, upstreamModel, tnames, isStream, start, reqID)
}

// handleNvidiaAnthropic serves /v1/messages via nim. split from
// handleAnthropic so the route dispatcher can try it as one candidate.
func (p *Pool) handleNvidiaAnthropic(w http.ResponseWriter, r *http.Request, oaiBody, body []byte, clientModel, upstreamModel string, tnames toolNames, isStream bool, start time.Time, reqID string) {
	target := nvidiaBase + "/chat/completions"
	fwd := http.Header{
		"Content-Type": {"application/json"},
		"Accept":       {"application/json"},
	}
	if isStream {
		fwd.Set("Accept", "text/event-stream")
	}

	u, upErr := p.callUpstream(r.Context(), r.Method, target, oaiBody, fwd, upstreamModel, r.RemoteAddr)
	elapsed := time.Since(start)

	if u == nil {
		errMsg := "all keys exhausted or on cooldown"
		if upErr != nil {
			errMsg = upErr.Error()
		}
		logUsage(UsageRecord{
			Ts:         time.Now().UTC().Format(time.RFC3339Nano),
			Model:      clientModel,
			Method:     "POST",
			Path:       "/v1/messages",
			StatusCode: 503,
			DurationMs: elapsed.Milliseconds(),
			Stream:     isStream,
			Error:      errMsg,
		})
		writeAnthropicError(w, http.StatusServiceUnavailable, "api_error", errMsg)
		return
	}

	if u.StatusCode != http.StatusOK {
		errMsg := extractErrMessage(string(u.Body))
		acclog.Printf("!! upstream %d [%s] model=%s err=%q", u.StatusCode, u.Key, clientModel, errMsg)

		logUsage(UsageRecord{
			Ts:          time.Now().UTC().Format(time.RFC3339Nano),
			Model:       clientModel,
			KeyName:     u.Key,
			Method:      "POST",
			Path:        "/v1/messages",
			StatusCode:  u.StatusCode,
			DurationMs:  elapsed.Milliseconds(),
			Stream:      isStream,
			Error:       errMsg,
			RateLimited: u.RateLimited,
		})

		writeAnthropicError(w, u.StatusCode, "api_error", errMsg)
		return
	}

	// session for the streak cap: lanes key on x-session-id verbatim.
	session := r.Header.Get("x-session-id")
	nvidiaPost := func(nb []byte) []byte {
		nu, err := p.callUpstream(r.Context(), r.Method, target, nb, fwd, upstreamModel, r.RemoteAddr)
		if err != nil || nu == nil || nu.StatusCode != http.StatusOK {
			return nil
		}
		return nu.Body
	}
	if isStream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		body := u.Body
		if retry := tryNudgeChat(session, clientModel, "nvidia", oaiBody, body, nvidiaPost); retry != nil {
			body = mergeChatSSE([][]byte{body, retry})
		}
		written, promptT, compT := streamAnthropic(w, body, clientModel, tnames)

		p.Postpone(u.KeyObj, 1500*time.Millisecond)
		p.Clear429(u.KeyObj)

		logUsage(UsageRecord{
			Ts:               time.Now().UTC().Format(time.RFC3339Nano),
			Model:            clientModel,
			KeyName:          u.Key,
			Method:           "POST",
			Path:             "/v1/messages",
			StatusCode:       200,
			DurationMs:       elapsed.Milliseconds(),
			PromptTokens:     promptT,
			CompletionTokens: compT,
			TotalTokens:      promptT + compT,
			Stream:           true,
			RetryAttempt:     u.Retries,
			RateLimited:      u.RateLimited,
			ContentBytes:     int64(len(body)),
		})

		acclog.Printf("<- 200 POST /v1/messages %v %d bytes [%s]", elapsed.Round(time.Millisecond), written, u.Key)
	} else {
		// non-stream nvidia may arrive as SSE or plain JSON: fold
		// SSE first, then nudge on the folded JSON throughout.
		foldChat := func(b []byte) []byte {
			if looksSSE(b) {
				return sseToNonStream(b, upstreamModel, tnames)
			}
			return b
		}
		folded := foldChat(u.Body)
		postFolded := func(nb []byte) []byte {
			rb := nvidiaPost(nb)
			if rb == nil {
				return nil
			}
			return foldChat(rb)
		}
		if retry := tryNudgeJSON(session, clientModel, "nvidia", oaiBody, folded, postFolded); retry != nil {
			folded = mergeChatJSON(folded, retry)
		}
		out, errMsg, _ := openAIToAnthropic(folded, clientModel, tnames)
		if errMsg != "" {
			writeAnthropicError(w, http.StatusInternalServerError, "api_error", errMsg)
			return
		}

		prompT, compT, _ := respTokens(folded)

		p.Postpone(u.KeyObj, 1500*time.Millisecond)
		p.Clear429(u.KeyObj)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(out)

		logUsage(UsageRecord{
			Ts:               time.Now().UTC().Format(time.RFC3339Nano),
			Model:            clientModel,
			KeyName:          u.Key,
			Method:           "POST",
			Path:             "/v1/messages",
			StatusCode:       200,
			DurationMs:       elapsed.Milliseconds(),
			PromptTokens:     prompT,
			CompletionTokens: compT,
			TotalTokens:      prompT + compT,
			Stream:           false,
			RetryAttempt:     u.Retries,
			RateLimited:      u.RateLimited,
			ContentBytes:     int64(len(body)),
		})

		acclog.Printf("<- 200 POST /v1/messages %v %d bytes [%s]", elapsed.Round(time.Millisecond), len(out), u.Key)
	}
}

func handleCountTokens(w http.ResponseWriter, body []byte) {
	tokens := estimateTokens(body)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"input_tokens": tokens})
}

// --- Helpers ---

func writeSSE(w io.Writer, event, data string) int64 {
	var n int
	if event != "" {
		n, _ = fmt.Fprintf(w, "event: %s\n", event)
	}
	m, _ := fmt.Fprintf(w, "data: %s\n\n", data)
	return int64(n + m)
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// strPtr boxes a string for the finish_reason field.
func strPtr(s string) *string { return &s }

// looksSSE reports whether a body is an event stream rather than plain json.
// structural, not a substring test: a line must start with "data: ", so a
// non-stream body carrying a data: url inside a json string is not folded.
func looksSSE(body []byte) bool {
	for _, raw := range bytes.Split(body, []byte("\n")) {
		if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("data: ")) {
			return true
		}
	}
	return false
}

// sseNormalize wraps a bare json body as a single sse chunk so the streaming
// converters can parse it. sse input passes through untouched.
func sseNormalize(body []byte) []byte {
	t := bytes.TrimSpace(body)
	if len(t) == 0 || t[0] != '{' && t[0] != '[' {
		return body
	}
	return []byte("data: " + string(t) + "\n\ndata: [DONE]\n\n")
}

func writeAnthropicError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": msg,
		},
	})
}

func extractErrMessage(body string) string {
	var errResp struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &errResp) == nil && errResp.Error.Message != "" {
		return errResp.Error.Message
	}
	var errStr struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(body), &errStr) == nil && errStr.Error != "" {
		return errStr.Error
	}
	msg := strings.TrimSpace(body)
	if msg == "" {
		msg = "upstream error with empty body"
	}
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}

func randHex(n int) string {
	b := make([]byte, n/2+1)
	// crypto/rand failure leaves zero bytes: fail the mint, never hand
	// out a predictable id.
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("crypto/rand failed: %v", err)
	}
	return fmt.Sprintf("%x", b)[:n]
}

const b62chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// zenID mirrors packages/schema/src/identifier.ts: timestamp ms * 0x1000 +
// per-ms counter, 6 bytes big-endian hex (complemented when descending),
// plus 14 base62 random chars from crypto bytes % 62.
var (
	zenIDMu      sync.Mutex
	zenIDLastTs  int64
	zenIDCounter int64
	zenIDMask    int64 = 0xFFFFFFFFFFFF
)

func zenID(descending bool) string {
	ts := time.Now().UnixMilli()
	zenIDMu.Lock()
	if ts != zenIDLastTs {
		zenIDLastTs = ts
		zenIDCounter = 0
	}
	zenIDCounter++
	current := ts*0x1000 + zenIDCounter
	masked := current & zenIDMask
	if descending {
		masked = (^masked) & zenIDMask
	}
	zenIDMu.Unlock()
	timeHex := fmt.Sprintf("%012x", masked)
	// crypto random, same charset/modulo as identifier.ts
	rb := make([]byte, 14)
	if _, err := rand.Read(rb); err != nil {
		log.Fatalf("crypto/rand failed: %v", err)
	}
	rb62 := make([]byte, 14)
	for i := range rb {
		rb62[i] = b62chars[int(rb[i])%62]
	}
	return timeHex + string(rb62)
}

// zenSession mirrors opencode's session id: ses_ + descending().
// (packages/schema/src/session-id.ts)
func zenSession() string {
	return "ses_" + zenID(true)
}

// setZenHeaders applies the header set the real opencode client sends to zen.
// source of truth is the live client capture (packages/core/src/session/
// model-request.ts): authorization Bearer public, x-opencode-client: cli,
// the session identity in both x-opencode-session and x-opencode-session-id
// plus x-session-affinity, x-opencode-project, and the bare
// `opencode/<channel>/<version>/<name>` User-Agent. the free-tier gate on
// /inference/v1 needs the two session ids and drops a request whose session
// value is not a descending opencode id (ses_ + 12 hex time + 14 base62), so
// every session here comes from zenSession(). x-opencode-request is
// deliberately absent: the current client does not send it.
func setZenHeaders(req *http.Request, sessionID string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer public")
	req.Header.Set("x-opencode-session-id", sessionID)
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-session", sessionID)
	req.Header.Set("x-session-affinity", sessionID)
	req.Header.Set("x-opencode-project", "global")
	req.Header.Set("User-Agent", zenUserAgent())
	req.Header.Set("Accept", "*/*")
}

// --- debug capture / passthrough (all no-ops unless env is set) ---

// zenDebugDir returns the ZEN_CAPTURE dir when debug capture is on.
func zenDebugDir() string { return os.Getenv("ZEN_CAPTURE") }

var zenDebugSeq atomic.Int64

// zenDebugCapture writes one metadata json file into the capture dir.
func zenDebugCapture(tag string, v any) {
	dir := zenDebugDir()
	if dir == "" {
		return
	}
	os.MkdirAll(dir, 0o700)
	n := zenDebugSeq.Add(1)
	// captures hold headers + bodies: redact credential headers and keep
	// the files owner-only.
	if m, ok := v.(map[string]any); ok {
		if h, ok := m["headers"].(http.Header); ok {
			rh := h.Clone()
			for _, k := range []string{"Authorization", "X-Api-Key", "Cookie", "X-Session-Id"} {
				if _, present := rh[http.CanonicalHeaderKey(k)]; present {
					rh.Set(k, "REDACTED")
				}
			}
			m["headers"] = rh
		}
	}
	if b, err := json.Marshal(v); err == nil {
		os.WriteFile(fmt.Sprintf("%s/%s-%05d.json", dir, tag, n), b, 0o600)
	}
}

// zenApplyHeaders sets the upstream headers for a zen request: the mimicked
// client set normally, or — debug only (ZEN_PASSTHROUGH=1 and the inbound
// request carries real opencode headers) — the client's own headers verbatim
// so upstream sees the real client fingerprint. hop-by-hop headers are
// dropped; accept-encoding must stay off so the response body stays plain.
func zenApplyHeaders(req *http.Request, sessionID string, client http.Header, reqID string, attempt any, target, proxy, model string) {
	passthrough := os.Getenv("ZEN_PASSTHROUGH") != "" && len(client["X-Opencode-Session"]) > 0
	if passthrough {
		h := client.Clone()
		for _, k := range []string{"Connection", "Accept-Encoding", "Content-Length", "Transfer-Encoding", "Te"} {
			h.Del(k)
		}
		// client credentials and spoofable opencode identity never ride
		// along, even in debug passthrough: the proxy sets its own.
		for k := range h {
			if !zenHeaderForwarded(k) {
				h.Del(k)
			}
		}
		req.Header = h
	} else {
		setZenHeaders(req, sessionID)
	}
	if zenDebugDir() != "" {
		zenDebugCapture("out", map[string]any{
			"req_id": reqID, "attempt": attempt, "url": target, "headers": req.Header,
			"proxy": redactProxyUserinfo(proxy), "session": sessionID, "model": model,
			"passthrough": passthrough,
		})
	}
}

func estimateTokens(body []byte) int {
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return 1000
	}

	// decoded text only: raw json bytes count base64 images and
	// syntax as tokens, inflating the estimate several-fold.
	textOf := func(raw json.RawMessage) int {
		if len(raw) == 0 {
			return 0
		}
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return len(s)
		}
		var blocks []anthropicBlock
		if json.Unmarshal(raw, &blocks) != nil {
			return 0
		}
		n := 0
		for _, b := range blocks {
			n += len(b.Text) + len(b.Thinking)
		}
		return n
	}
	chars := textOf(req.System)
	for _, msg := range req.Messages {
		chars += textOf(msg.Content)
	}
	for _, tool := range req.Tools {
		chars += len(tool.Name) + len(tool.Description) + len(tool.InputSchema)
	}

	tokens := chars / 4
	if tokens < 10 {
		tokens = 10
	}
	return tokens
}

// handleOpenCodeAnthropic routes opencode/* models through the zen endpoint.
func (p *Pool) handleOpenCodeAnthropic(w http.ResponseWriter, r *http.Request, origBody, oaiBody []byte, clientModel, upstreamModel string, tnames toolNames, isStream bool, start time.Time, reqID string) {
	zenModel := strings.TrimPrefix(upstreamModel, "opencode/")
	endpoint := endpointForModel(zenModel)
	isResponses := endpoint == "/responses"
	acclog.Printf("%s routed to opencode model=%s endpoint=%s", reqID, zenModel, endpoint)
	if zenDebugDir() != "" {
		zenDebugCapture("in", map[string]any{"side": "anthropic", "req_id": reqID, "url": r.URL.String(), "headers": r.Header, "model": clientModel, "stream": isStream})
	}

	var m map[string]any
	if json.Unmarshal(oaiBody, &m) == nil {
		m["model"] = zenModel
		stripCacheFields(m)
		ensureZenTools(m)
		if !isStream {
			m["stream"] = true
			m["stream_options"] = map[string]any{"include_usage": true}
		}
		if isResponses {
			convertToResponses(m)
		} else {
			sanitizeZenChatMessages(m)
		}
		if b, err := json.Marshal(m); err == nil {
			oaiBody = b
		}
	}

	target := OpencodeBase + endpointForModel(zenModel)
	var resp *http.Response

	if os.Getenv("ZEN_DUMP") != "" {
		t := time.Now().UnixNano()
		p := fmt.Sprintf("/tmp/zenreq_%d.json", t)
		os.WriteFile(p, oaiBody, 0o600)
		acclog.Printf("ZEN_DUMP request %d bytes -> %s", len(oaiBody), p)
		pi := fmt.Sprintf("/tmp/zenin_%d.json", t)
		os.WriteFile(pi, origBody, 0o600)
		acclog.Printf("ZEN_DUMP inbound %d bytes -> %s", len(origBody), pi)
	}

	lane := laneFor(r.Header.Get("x-session-id"), oaiBody)
	sessionID := laneSession(lane)
	noteLaneUse(true) // capacity tracking for the low-load lane race
	defer noteLaneUse(false)
	// track the final lane, including failover handoffs.
	laneClaim(lane)
	defer func() { laneDone(lane) }()

	// zenPost sends body to zen with proxy failover and returns the response.
	zenPost := func(body []byte, stream bool, tag string) *http.Response {
		var cl *http.Client
		lastProxy := ""
		var dialDeadErr error
		dialDeadProxy := ""
		rotating := true
		for zenRetries := 0; zenRetries < zenMaxRetries(); zenRetries++ {
			// pace only: lanes never cool, a limit rotates the exit instead.
			if zenRetries > 0 && maybeRefreshStalePool(lane) {
				sessionID = laneSession(lane)
			}
			// retries always land on a fresh lane (failover/rotate above),
			// so re-pacing against it only stacks the first send's gap
			// onto the retry. the first attempt still paces.
			if zenRetries == 0 {
				laneGate(r.Context(), lane)
			}
			// client gone while pacing: stop before burning an exit.
			if r.Context().Err() != nil {
				return nil
			}
			proxy := ""
			if zenRetries == 0 {
				proxy = laneProxyFor(lane, zenModel)
				if proxy == "" && cfg().Zen.AlwaysProxy {
					// cold pool: the verify sweep publishes exits as they
					// land, so wait for it instead of failing instantly.
					// bounded by the retry budget below, not forever.
					if waitZenPool(r.Context(), 8*time.Second) {
						lane = laneHandoff(lane, laneFailover(lane))
						sessionID = laneSession(lane)
						proxy = laneProxyFor(lane, zenModel)
						// pool is up but this lane drew no free exit (every
						// one taken or written off): another lane is the
						// answer, not a proxy_exhausted 503 from hop 0.
						if proxy == "" {
							continue
						}
					} else {
						return nil
					}
				}
				cl = zenClient(proxy)
				lastProxy = proxy
			} else if rotating {
				proxy = laneProxyFor(lane, zenModel)
				if proxy == "" && cfg().Zen.AlwaysProxy {
					lane = laneHandoff(lane, laneFailover(lane))
					sessionID = laneSession(lane)
					proxy = laneProxyFor(lane, zenModel)
				}
				if proxy == "" && cfg().Zen.AlwaysProxy {
					// same cold-pool wait as retry 0: an empty pool means
					// the sweep is still verifying, not that exits are
					// dead. without the sleep all retries burn in ms.
					if !waitZenPool(r.Context(), 8*time.Second) {
						return nil
					}
					continue
				}
				// rotation handed back the exit whose dial just failed: a
				// second dial of the same corpse cannot succeed, so end the
				// ladder here instead of spending every remaining hop on it.
				if dialDeadErr != nil && proxy == dialDeadProxy {
					acclog.Printf("  opencode dial-dead %s re-picked by lane %s, ending retry ladder", redactProxyUserinfo(proxy), lane.id)
					return nil
				}
				cl = zenClient(proxy)
				lastProxy = proxy
			} else {
				// service overloaded: retry on the same lane proxy + session.
				proxy = lastProxy
				// killswitch: always_proxy must never send direct, even on
				// the pinned path. lastProxy is "" only when the first
				// attempt went direct, which cannot happen under the flag
				// (see the retry-0 branch above), so this is belt and
				// braces, not a live path.
				if proxy == "" && cfg().Zen.AlwaysProxy {
					return nil
				}
				cl = zenClient(proxy)
			}
			req, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
			if err != nil {
				return nil
			}
			zenApplyHeaders(req, sessionID, r.Header, reqID, zenRetries, target, proxy, zenModel)
			if zenRetries > 0 {
				acclog.Printf("  opencode retry %d/%d session=%s proxy=%s lane=%s", zenRetries, zenRetryLabel(), partialSession(sessionID), redactProxyUserinfo(proxy), lane.id)
			}

			hedgeAfter := hedgeDelay()
			var firstByteAt time.Time
			traced := req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
				GotFirstResponseByte: func() { firstByteAt = time.Now() },
			}))
			sendStart := time.Now()
			up, err, _ := hedgedDo(r.Context(),
				func(pctx context.Context) (*http.Response, error) { return cl.Do(traced.WithContext(pctx)) },
				func(hctx context.Context) (*http.Response, error) {
					hl := laneFailover(lane)
					hp := laneProxyFor(hl, zenModel)
					if hp == "" || hp == proxy {
						return nil, fmt.Errorf("no distinct zen hedge exit")
					}
					hcl := zenClient(hp)
					hreq, herr := http.NewRequestWithContext(hctx, r.Method, target, bytes.NewReader(body))
					if herr != nil {
						return nil, herr
					}
					zenApplyHeaders(hreq, laneSession(hl), r.Header, reqID, "hedge", target, hp, zenModel)
					hup, huerr := hcl.Do(hreq)
					if huerr == nil {
						// transport success is not a served
						// request: only a 200 stamps health.
						if hup.StatusCode == http.StatusOK {
							noteExitOK(hp, zenModel)
						} else if hup.StatusCode == http.StatusTooManyRequests {
							noteExit429(hp, zenModel)
						}
					} else if hctx.Err() == nil {
						// own proxy label: primary-side vs hedge-side
						// mixups are answerable next time.
						acclog.Printf("  opencode hedge error model=%s country=%s proxy=%s lane=%s: %v",
							clientModel, proxyCountry(hp), redactProxyUserinfo(hp), hl.id, huerr)
					}
					return hup, huerr
				}, hedgeAfter)
			if err == nil && !firstByteAt.IsZero() {
				fb := firstByteAt.Sub(sendStart)
				noteExitRTT(proxy, float64(fb.Milliseconds()))
				if fb > 6*time.Second {
					noteExitSlow(proxy)
				}
			}
			if up != nil {
				zenDebugCapture("resp", map[string]any{"req_id": reqID, "status": up.StatusCode, "headers": up.Header, "attempt": zenRetries, "model": zenModel})
			}
			if err != nil {
				if isTLSFailure(err) {
					// mitm exit: ban it pool-wide and evacuate every
					// pinned lane, not just this one.
					banTLSExit(proxy)
				} else {
					dropProxy(proxy)
					// the dropped exit may still be pinned here when the
					// rest of the pool is held by other lanes: re-dialing
					// it burns every retry on a corpse, so move lanes the
					// way the 429 path does.
					if laneReproxy(lane, zenModel) == proxy {
						lane = laneHandoff(lane, laneFailover(lane))
						sessionID = laneRotate(lane, zenModel)
					}
				}
				if zenDialDead(err) {
					dialDeadErr, dialDeadProxy = err, proxy
				} else {
					dialDeadErr = nil
				}
				rotating = true
				logZenNetErr(zenRetries, clientModel, proxy, tag+" "+target, err)
				maybeRefreshNetErr()
				if !zenLastRetry(zenRetries) {
					zenBackoffCtx(r.Context(), zenRetries)
					continue
				}
				return nil
			}

			// gaveUp marks a synthesized response: the ladder is over and the
			// remaining checks must not re-treat it as an upstream failure.
			gaveUp := false
			if up.StatusCode == http.StatusTooManyRequests || up.StatusCode == 529 {
				st := up.StatusCode
				up.Body.Close()
				acclog.Printf("  opencode rate-limited %d (retry %d/%d) model=%s country=%s proxy=%s %s lane=%s, rotating exit+session",
					st, zenRetries, zenRetryLabel(), clientModel, proxyCountry(proxy), redactProxyUserinfo(proxy), tag, lane.id)
				if proxy == "" {
					laneEscalate(lane, zenModel) // direct got limited: give the lane an exit
				}
				noteExit429(proxy, zenModel)
				if zenLastRetry(zenRetries) {
					// the ladder is spent: the client hears about the proxy
					// pool, never the 429 we just absorbed.
					up, gaveUp = exhaustedExits(clientModel, st), true
				} else {
					lane = laneHandoff(lane, laneFailover(lane))
					sessionID = laneRotate(lane, zenModel)
					rotating = true
					zenBackoffCtx(r.Context(), zenRetries)
					continue
				}
			}

			if !gaveUp && zenRetryableStatus(up.StatusCode) {
				// the exit (or a proxy in front of it) failed at transport
				// level: same treatment as a rate limit, so a client never
				// sees our plumbing break.
				st := up.StatusCode
				up.Body.Close()
				acclog.Printf("  opencode exit failed %d (retry %d/%d) model=%s country=%s proxy=%s %s lane=%s, rotating exit+session",
					st, zenRetries, zenRetryLabel(), clientModel, proxyCountry(proxy), redactProxyUserinfo(proxy), tag, lane.id)
				dropProxy(proxy)
				if zenLastRetry(zenRetries) {
					up, gaveUp = exhaustedExits(clientModel, st), true
				} else {
					lane = laneHandoff(lane, laneFailover(lane))
					sessionID = laneRotate(lane, zenModel)
					rotating = true
					zenBackoffCtx(r.Context(), zenRetries)
					continue
				}
			}

			if up.StatusCode != http.StatusOK {
				eb, _ := io.ReadAll(io.LimitReader(up.Body, 16<<10))
				up.Body.Close()
				if zenServiceOverloaded(eb) {
					acclog.Printf("  opencode overloaded %d (retry %d/%d) model=%s country=%s proxy=%s %s rotating exit+session err=%q",
						up.StatusCode, zenRetries, zenRetryLabel(), clientModel, proxyCountry(proxy), redactProxyUserinfo(proxy), tag, errSnippet(eb, 160))
					lane = laneHandoff(lane, laneFailover(lane))
					sessionID = laneRotate(lane, zenModel)
					rotating = true
					if !zenLastRetry(zenRetries) {
						zenBackoffCtx(r.Context(), zenRetries)
						continue
					}
				}
				if zenGeoBlocked(eb) {
					logZenBlocked("geo", zenRetries, up.StatusCode, clientModel, proxy, tag, eb)
					dropProxy(proxy)
					lane = laneHandoff(lane, laneFailover(lane))
					sessionID = laneRotate(lane, zenModel)
					rotating = true
					if noteZenGeoErr() {
						acclog.Printf("  %d+ geo-blocks, refreshing proxy pool", zenErrorThreshold())
						refreshZenProxiesAsync(false)
						resetZenGeoErrs()
					}
					if !zenLastRetry(zenRetries) {
						zenBackoffCtx(r.Context(), zenRetries)
						continue
					}
				}
				if zenUserBlocked(eb) {
					logZenBlocked("user", zenRetries, up.StatusCode, clientModel, proxy, tag, eb)
					dropProxy(proxy)
					lane = laneHandoff(lane, laneFailover(lane))
					sessionID = laneRotate(lane, zenModel)
					rotating = true
					if noteZenBlockErr() {
						acclog.Printf("  %d+ user-blocks, refreshing proxy pool", zenErrorThreshold())
						refreshZenProxiesAsync(false)
						resetZenBlockErrs()
					}
					if !zenLastRetry(zenRetries) {
						zenBackoffCtx(r.Context(), zenRetries)
						continue
					}
				}
				if zenFreeTierBlocked(eb) {
					acclog.Printf("  opencode free-tier-blocked %d (retry %d/%d) model=%s country=%s proxy=%s %s lane=%s, switching lane",
						up.StatusCode, zenRetries, zenRetryLabel(), clientModel, proxyCountry(proxy), redactProxyUserinfo(proxy), tag, lane.id)
					// per-model refusal, not a dead exit: it still serves other
					// models, so it stays in the pool and the lane rotates off it.
					noteExitModelBurned(proxy, zenModel)
					if proxy == "" {
						laneEscalate(lane, zenModel) // direct is flagged: give the lane an exit
					}
					lane = laneHandoff(lane, laneFailover(lane))
					sessionID = laneRotate(lane, zenModel)
					rotating = true
					if noteZenFreeTierErr() {
						acclog.Printf("  %d+ free-tier blocks, refreshing proxy pool", zenErrorThreshold())
						refreshZenProxiesAsync(false)
						resetZenFreeTierErrs()
					}
					if !zenLastRetry(zenRetries) {
						zenBackoffCtx(r.Context(), zenRetries)
						continue
					}
				}
				acclog.Printf("!! opencode upstream %d (retry %d/%d) model=%s country=%s proxy=%s %s err=%q",
					up.StatusCode, zenRetries, zenRetryLabel(), clientModel, proxyCountry(proxy), redactProxyUserinfo(proxy), tag, errSnippet(eb, 160))
				up.Body = io.NopCloser(bytes.NewReader(eb))
			}
			resetZenNetErrs()
			if up.StatusCode == http.StatusOK {
				p.noteZenSuccess(sessionID, proxy)
				noteExitOK(proxy, zenModel)
				laneTouch(lane, proxy, zenModel)
			}
			return up
		}
		return nil
	}

	resp = zenPost(oaiBody, isStream, "")
	if resp == nil {
		writeAnthropicError(w, http.StatusServiceUnavailable, "api_error", proxyUnavailableMsg(zenModel))
		return
	}
	defer resp.Body.Close()

	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, upstreamBodyLimit))
		if os.Getenv("ZEN_DUMP") != "" {
			p := fmt.Sprintf("/tmp/zenresp_%d.json", time.Now().UnixNano())
			os.WriteFile(p, rb, 0o600)
			acclog.Printf("ZEN_DUMP response %d bytes -> %s", len(rb), p)
		}
		errMsg := extractErrMessage(string(rb))
		acclog.Printf("!! opencode zen %d model=%s err=%q", resp.StatusCode, clientModel, errMsg)
		writeAnthropicError(w, resp.StatusCode, "api_error", errMsg)
		return
	}

	if isStream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		var written int64
		var promptT, compT int
		next := func(text string, hasCalls bool) io.ReadCloser {
			if r.Context().Err() != nil || nudgeChatGates(sessionID, upstreamModel, oaiBody, text, hasCalls) == "" {
				return nil
			}
			nb := nudgeContinuationChat(oaiBody, text)
			if isResponses {
				nb = nudgeContinuation(oaiBody, text)
			}
			resp.Body.Close()
			nresp := zenPost(nb, true, "nudge")
			if nresp == nil {
				return nil
			}
			if nresp.StatusCode != http.StatusOK {
				nresp.Body.Close()
				return nil
			}
			nudgeStreaks.note(sessionID, false)
			return nresp.Body
		}
		if isResponses {
			written, promptT, compT = streamResponsesAnthropicReader(r.Context(), w, resp.Body, clientModel, tnames, next)
		} else {
			written, promptT, compT = streamAnthropicReader(r.Context(), w, resp.Body, clientModel, tnames, next)
		}
		resp.Body.Close()
		elapsed = time.Since(start)

		logUsage(UsageRecord{
			Ts:               time.Now().UTC().Format(time.RFC3339Nano),
			Model:            clientModel,
			KeyName:          "opencode",
			Method:           "POST",
			Path:             "/v1/messages",
			StatusCode:       200,
			DurationMs:       elapsed.Milliseconds(),
			PromptTokens:     promptT,
			CompletionTokens: compT,
			TotalTokens:      promptT + compT,
			Stream:           true,
			ContentBytes:     int64(len(origBody)),
		})
		laneTokensAdd(lane, zenModel, promptT, compT, promptT+compT, elapsed.Milliseconds())
		acclog.Printf("<- 200 POST /v1/messages %v %d bytes [opencode] model=%s %s", elapsed.Round(time.Millisecond), written, clientModel, reqID)
	} else {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, upstreamBodyLimit))
		if os.Getenv("ZEN_DUMP") != "" {
			p := fmt.Sprintf("/tmp/zenraw_%d.bin", time.Now().UnixNano())
			os.WriteFile(p, rb, 0o600)
			acclog.Printf("ZEN_DUMP nonstream raw %d bytes -> %s", len(rb), p)
		}
		if isResponses && resp.StatusCode == http.StatusOK {
			// same no-tools continue as the stream branch, on the raw
			// SSE before folding. post replays non-stream too.
			post := func(nb []byte) []byte {
				nresp := zenPost(nb, false, "nudge")
				if nresp == nil {
					return nil
				}
				defer nresp.Body.Close()
				if nresp.StatusCode != http.StatusOK {
					return nil
				}
				nb2, _ := io.ReadAll(io.LimitReader(nresp.Body, upstreamBodyLimit))
				return nb2
			}
			if retry := tryNudgeResponses(sessionID, upstreamModel, oaiBody, rb, post); retry != nil {
				rb = mergeResponsesSSE([][]byte{rb, retry})
			}
			rb = responsesToChat(responsesSSEToJSON(rb))
		} else {
			// chat-model continue on raw SSE before folding.
			post := func(nb []byte) []byte {
				nresp := zenPost(nb, false, "nudge")
				if nresp == nil {
					return nil
				}
				defer nresp.Body.Close()
				if nresp.StatusCode != http.StatusOK {
					return nil
				}
				nb2, _ := io.ReadAll(io.LimitReader(nresp.Body, upstreamBodyLimit))
				return nb2
			}
			if retry := tryNudgeChat(sessionID, upstreamModel, "opencode", oaiBody, rb, post); retry != nil {
				rb = mergeChatSSE([][]byte{rb, retry})
			}
			rb = sseToNonStream(rb, zenModel, tnames)
		}
		if os.Getenv("ZEN_DUMP") != "" {
			p := fmt.Sprintf("/tmp/zenfold_%d.json", time.Now().UnixNano())
			os.WriteFile(p, rb, 0o600)
			acclog.Printf("ZEN_DUMP nonstream folded %d bytes -> %s", len(rb), p)
		}
		elapsed = time.Since(start)
		prompT, compT := 0, 0
		out, errMsg, _ := openAIToAnthropic(rb, clientModel, tnames)
		if errMsg != "" {
			writeAnthropicError(w, http.StatusInternalServerError, "api_error", errMsg)
			return
		}
		prompT, compT, _ = respTokens(rb)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(out)

		logUsage(UsageRecord{
			Ts:               time.Now().UTC().Format(time.RFC3339Nano),
			Model:            clientModel,
			KeyName:          "opencode",
			Method:           "POST",
			Path:             "/v1/messages",
			StatusCode:       200,
			DurationMs:       elapsed.Milliseconds(),
			PromptTokens:     prompT,
			CompletionTokens: compT,
			TotalTokens:      prompT + compT,
			Stream:           false,
			ContentBytes:     int64(len(origBody)),
		})
		laneTokensAdd(lane, zenModel, prompT, compT, prompT+compT, elapsed.Milliseconds())
		acclog.Printf("<- 200 POST /v1/messages %v %d bytes [opencode] model=%s %s", elapsed.Round(time.Millisecond), len(out), clientModel, reqID)
	}
}
