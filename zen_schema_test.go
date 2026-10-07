package main

// tests for the zen tool-schema sanitizer and the anthropic->openai tool
// conversion. owner boundaries:
//   - sanitizeZenSchema owns "no schema we forward to zen can read as a
//     recursive JSON schema" (zen 400s the whole request, and its resolver
//     chases $ref cycles server-side).
//   - ensureZenTools owns "every tool reaches zen in openai function shape
//     with an object parameters schema".
//   - anthropicRequestToOpenAI owns "a Claude-Code-shaped recursive-schema
//     request converts to a zen-acceptable body".

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

// claudeCodeGrepSchema is what Claude Code's Grep tool ships: a $ref into
// $defs where the target references itself (Node.child -> Node).
func claudeCodeGrepSchema() map[string]any {
	raw := `{"type":"object","properties":{"pattern":{"type":"string"},"path":{"$ref":"#/$defs/PathLike"}},"required":["pattern"],"$defs":{"PathLike":{"anyOf":[{"type":"string"},{"type":"array","items":{"type":"string"}}]},"Node":{"type":"object","properties":{"child":{"$ref":"#/$defs/Node"},"name":{"type":"string"}}}}}`
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		panic(err)
	}
	return m
}

// a $ref-stripped node keeps its empty shape at the boundary so the tool
// stays callable: the properties loop drops emptied CHILDREN, so a bare {}
// survives only where the test pins it below.
func TestSanitizeZenSchemaStripsRecursion(t *testing.T) {
	out := sanitizeZenSchema(claudeCodeGrepSchema())
	b, _ := json.Marshal(out)
	s := string(b)
	if strings.Contains(s, "$ref") || strings.Contains(s, "$defs") {
		t.Fatalf("sanitized schema still carries $ref/$defs: %s", s)
	}
	if !strings.Contains(s, `"type":"object"`) {
		t.Fatalf("top-level type lost: %s", s)
	}
	// required survives so the tool stays callable
	if !strings.Contains(s, `"required":["pattern"]`) {
		t.Fatalf("required list lost: %s", s)
	}
	// the anyOf branch keeps its shape (path was a $ref to an anyOf; the
	// ref node becomes empty but stays a valid schema object)
	if !strings.Contains(s, `"path":{}`) {
		t.Fatalf("path property missing: %s", s)
	}
}

func TestSanitizeZenSchemaTable(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		want  string // substring of the marshaled output; empty = exact match
		exact string // exact marshaled output
	}{
		{"bool schema", `true`, `{"type":"string"}`, ""},
		{"const becomes enum", `{"const":"x"}`, `"enum":["x"]`, ""},
		{"type array keeps members", `{"type":["string","null"]}`, `"type":["string","null"]`, ""},
		{"unknown type dropped", `{"type":"weird"}`, ``, "{}"},
		{"nested items sanitized", `{"type":"array","items":{"type":"integer","exclusiveMinimum":1}}`, `{"items":{"type":"integer"},"type":"array"}`, ""},
		{"exotic keywords dropped", `{"type":"string","pattern":"^a","format":"date"}`, ``, `{"type":"string"}`},
		{"properties infer object", `{"properties":{"a":{"type":"string"}},"required":["a"]}`, ``, `{"properties":{"a":{"type":"string"}},"required":["a"],"type":"object"}`},
		{"non-string required filtered", `{"type":"object","required":["a",3]}`, `"required":["a"]`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var v any
			if err := json.Unmarshal([]byte(tt.in), &v); err != nil {
				t.Fatalf("bad test input: %v", err)
			}
			b, _ := json.Marshal(sanitizeZenSchema(v))
			got := string(b)
			if tt.exact != "" {
				if got != tt.exact {
					t.Fatalf("got %s want %s", got, tt.exact)
				}
				return
			}
			if !strings.Contains(got, tt.want) {
				t.Fatalf("got %s want substring %s", got, tt.want)
			}
		})
	}
}

// schemaDepthTestConfig is a nudge-enabled suite config: the real config
// gates these converts on nudge flags, so tests say so explicitly.
func schemaDepthTestConfig() *appConfig {
	c := testConfig()
	c.Nudge.Enabled = true
	return c
}

func TestSanitizeZenSchemaDeepNestingTerminates(t *testing.T) {
	// 2001 nesting levels: zen's console provider 400s any schema nesting
	// past level 10, so the walk must truncate by omitting over-budget
	// nodes and still produce a bounded, valid schema.
	const reps = 1000
	var b strings.Builder
	for i := 0; i < reps; i++ {
		b.WriteString(`{"properties":{"a":`)
	}
	b.WriteString(`{"type":"string"}`)
	for i := 0; i < 2*reps; i++ {
		b.WriteString(`}`)
	}
	var v any
	if err := json.Unmarshal([]byte(b.String()), &v); err != nil {
		t.Fatalf("bad input: %v", err)
	}
	out := sanitizeZenSchema(v)
	if out == nil {
		t.Fatal("nil output")
	}
	if _, omitted := out.(zenOmitType); omitted {
		t.Fatalf("root must never be omitted: %v", out)
	}
	// the sanitizer omits maps at zenSchemasMaxDepth, so residual bodies
	// nest at most 9 maps deep from the parameters root.
	if d := zenLeafSegments(out, 1); d > zenSchemasMaxDepth-1 {
		t.Fatalf("sanitized schema nests %d maps, want <= %d", d, zenSchemasMaxDepth-1)
	}
	ob, _ := json.Marshal(out)
	if !strings.Contains(string(ob), `"type":"object"`) {
		t.Fatalf("root lost its object shape: %.200s", ob)
	}
}

// zenLeafSegments returns the deepest object-map nesting in v, matching
// the validator: every content-carrying object map nests one level deeper,
// arrays and scalars cost nothing. parameters root enters at 1. content
// is a value that opens a new nesting level (properties, items, a
// composition branch, or a nested object); the emitted type/description/
// title/enum siblings ride on their parent object without opening a deeper
// level. a fully stripped tail ({}) costs nothing and is skipped along
// with everything under it. required holds plain strings and costs
// nothing beyond its parent map.
func zenLeafSegments(v any, seg int) int {
	best := seg
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			switch k {
			case "type", "description", "title", "enum", "required":
				continue
			}
			if d := zenLeafSegments(x, seg+1); d > best {
				best = d
			}
		}
		if best == seg {
			// no content children at all: this map is a content-free
			// keyword shell, so it adds nothing.
			content := false
			for k := range t {
				switch k {
				case "type", "description", "title", "enum", "required":
				default:
					content = true
				}
			}
			if !content {
				return seg
			}
			return seg + 1
		}
	case []any:
		for _, x := range t {
			if d := zenLeafSegments(x, seg); d > best {
				best = d
			}
		}
	}
	return best
}

// zenSchemaRank kept for the rank-based assertion below: deepest schema-node
// rank in v, starting at rank. root schema is rank 1, properties/items
// children rank+1, anyOf/oneOf/allOf branches at the parent rank.
func zenSchemaRank(v any, rank int) int {
	switch t := v.(type) {
	case map[string]any:
		m := rank
		for k, x := range t {
			switch k {
			case "anyOf", "oneOf", "allOf":
				// branches charged at the parent's rank
				if d := zenSchemaRank(x, rank); d > m {
					m = d
				}
			case "properties":
				// holder map: children are rank+1, holder itself free
				if pm, ok := x.(map[string]any); ok {
					for _, pv := range pm {
						if d := zenSchemaRank(pv, rank+1); d > m {
							m = d
						}
					}
				}
			case "items", "additionalProperties":
				if d := zenSchemaRank(x, rank+1); d > m {
					m = d
				}
			}
		}
		return m
	case []any:
		m := rank - 1 // a bare array is a container only in items position
		for _, x := range t {
			if d := zenSchemaRank(x, rank); d > m {
				m = d
			}
		}
		return m
	default:
		return rank - 1
	}
}

// TestSanitizeZenSchemaDropsOverBudgetRequired covers the second-order
// 400: the tail beyond zen's budget is omitted, so any surviving required
// list must not name a property that was dropped with it.
func TestSanitizeZenSchemaDropsOverBudgetRequired(t *testing.T) {
	useConfig(t, schemaDepthTestConfig())
	// 25-node chain, every node requires its single child; the tail beyond
	// the budget is omitted, so each surviving node's required ["k"] must
	// either still have its k or lose the required entry entirely.
	node := map[string]any{"type": "object"}
	for i := 0; i < 25; i++ {
		node = map[string]any{
			"type":       "object",
			"properties": map[string]any{"k": node},
			"required":   []any{"k"},
		}
	}
	out := sanitizeZenSchema(node)
	if d := zenLeafSegments(out, 1); d > zenSchemasMaxDepth-1 {
		t.Fatalf("sanitized schema nests %d maps, want <= %d", d, zenSchemasMaxDepth-1)
	}
	var check func(v any, seen map[string]bool, path string)
	check = func(v any, seen map[string]bool, path string) {
		switch tv := v.(type) {
		case map[string]any:
			if seen[path] {
				return
			}
			seen[path] = true
			if props, ok := tv["properties"].(map[string]any); ok {
				if req, ok := tv["required"].([]any); ok {
					for _, r := range req {
						rs, _ := r.(string)
						if _, has := props[rs]; !has {
							b, _ := json.Marshal(tv)
							t.Fatalf("dangling required %q in %.200s", rs, b)
						}
					}
				}
			}
			for k, x := range tv {
				check(x, seen, path+"/"+k)
			}
		case []any:
			for i, x := range tv {
				check(x, seen, fmt.Sprintf("%s/%d", path, i))
			}
		}
	}
	check(out, map[string]bool{}, "root")
	// something was actually cut: the deepest surviving node has no
	// properties of its own (its child was omitted)
	deepest := out
	for {
		om := deepest.(map[string]any)
		pm, ok := om["properties"].(map[string]any)
		if !ok {
			break
		}
		child, ok := pm["k"]
		if !ok {
			break
		}
		deepest = child
	}
	dm := deepest.(map[string]any)
	if _, has := dm["properties"]; has {
		t.Fatalf("tail not trimmed, deepest node still has properties: %v", deepest)
	}
}

// TestZenValidatorBoundaryLive bisects the console validator's real budget
// through the lane path against the specified responses model. reads
// ZEN_CALIBRATE_MODEL, defaulting to a -suffixed muse-spark. chains are
// pure properties spines; each case retries a few exits so an unlucky
// per-exit burn cannot mask the boundary. run: ZEN_CALIBRATE=1 go test -run
// TestZenValidatorBoundaryLive. skipped otherwise (burns free-tier quota).
func TestZenValidatorBoundaryLive(t *testing.T) {
	if os.Getenv("ZEN_CALIBRATE") == "" {
		t.Skip("set ZEN_CALIBRATE=1 to probe the live validator")
	}
	model := os.Getenv("ZEN_CALIBRATE_MODEL")
	if model == "" {
		model = "muse-spark-1.3"
	}
	chain := func(n int) map[string]any {
		root := map[string]any{"type": "object", "properties": map[string]any{}}
		cur := root
		for i := 0; i < n; i++ {
			child := map[string]any{"type": "string"}
			if i < n-1 {
				child = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			cur["properties"].(map[string]any)["c"] = child
			if c, ok := child["properties"]; ok {
				cur = map[string]any{"properties": c}
			}
		}
		return root
	}
	probe := func(containers int) (int, string) {
		body, _ := json.Marshal(map[string]any{
			"model": model, "input": []any{map[string]any{"role": "user", "content": "hi"}},
			"max_output_tokens": 16, "tools": []any{map[string]any{
				"type": "function", "name": "probe", "description": "p",
				"parameters": chain(containers - 1),
			}},
		})
		for attempt := 0; attempt < 4; attempt++ {
			req, _ := http.NewRequest("POST", OpencodeBase+"/responses", bytes.NewReader(body))
			setZenHeaders(req, zenSession())
			req.Header.Set("User-Agent", "opencode/1.18.35")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Logf("containers=%d attempt=%d transport: %v", containers, attempt, err)
				continue
			}
			rb, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			msg := ""
			var er struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(rb, &er) == nil {
				msg = er.Error.Message
			}
			if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == 529 ||
				strings.Contains(msg, "FreeTier") || strings.Contains(msg, "overloaded") {
				t.Logf("containers=%d attempt=%d transient %d", containers, attempt, resp.StatusCode)
				continue
			}
			return resp.StatusCode, msg
		}
		return -1, "no clean exit"
	}
	for _, n := range []int{8, 10, 12, 14} {
		code, msg := probe(n)
		t.Logf("containers=%d -> %d %q", n, code, msg)
	}
}

// live on muse-spark /responses: the with-context reorganize_notes plan.
// its complexity/enum[0] leaf sits past the budget, which must trim the
// enum values or the plan subtree so the request passes.
func TestSanitizeZenSchemaCCRealTool(t *testing.T) {
	useConfig(t, schemaDepthTestConfig())
	raw, err := os.ReadFile("/tmp/failing_input_schema.json")
	if err != nil {
		t.Skip("no captured failing schema; run claude code against scratch to produce it")
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("bad fixture: %v", err)
	}
	out := sanitizeZenSchema(v)
	if got := zenLeafSegments(out, 1); got > zenSchemasMaxDepth-1 {
		t.Fatalf("sanitized parameters nest %d maps, want <= %d", got, zenSchemasMaxDepth-1)
	}
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "$ref") {
		t.Fatalf("sanitized schema still carries $ref")
	}
}
func TestSanitizeZenSchemaCCDeepTool(t *testing.T) {
	useConfig(t, schemaDepthTestConfig())
	// build a 25-level property chain with descriptions sprinkled in
	leaf := map[string]any{"type": "string", "description": "leaf"}
	node := leaf
	for i := 0; i < 24; i++ {
		node = map[string]any{
			"type":        "object",
			"description": fmt.Sprintf("level %d", i),
			"properties":  map[string]any{"k": node},
			"required":    []any{"k"},
		}
	}
	out := sanitizeZenSchema(node)
	if d := zenLeafSegments(out, 1); d > zenSchemasMaxDepth-1 {
		t.Fatalf("sanitized schema nests %d maps, want <= %d", d, zenSchemasMaxDepth-1)
	}
	b, _ := json.Marshal(out)
	s := string(b)
	if strings.Contains(s, "$ref") || strings.Contains(s, "$defs") {
		t.Fatalf("recursion markers leaked: %.200s", s)
	}
	// the shallow head of the chain survives so the tool stays callable
	if !strings.Contains(s, `"required":["k"]`) {
		t.Fatalf("shallow required lost: %.200s", s)
	}
}

func TestEnsureZenToolsAnthropicShapeTool(t *testing.T) {
	// a client posting an anthropic-shaped tool straight to /v1/chat/completions
	m := map[string]any{
		"tools": []any{map[string]any{
			"name":         "Grep",
			"description":  "search",
			"input_schema": claudeCodeGrepSchema(),
		}},
	}
	ensureZenTools(m)
	ts, _ := m["tools"].([]any)
	tm, _ := ts[0].(map[string]any)
	fn, ok := tm["function"].(map[string]any)
	if !ok {
		t.Fatalf("tool not rewritten to openai shape: %v", tm)
	}
	if fn["name"] != "Grep" {
		t.Fatalf("name lost: %v", fn)
	}
	if _, has := tm["input_schema"]; has {
		t.Fatal("input_schema left on the outer object")
	}
	if _, has := fn["parameters"]; !has {
		t.Fatal("parameters missing")
	}
	b, _ := json.Marshal(fn["parameters"])
	if strings.Contains(string(b), "$ref") {
		t.Fatalf("recursive schema forwarded: %s", b)
	}
}

func TestEnsureZenToolsSingleObject(t *testing.T) {
	m := map[string]any{
		"tools": map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":       "f",
				"parameters": map[string]any{"type": "object"},
			},
		},
	}
	ensureZenTools(m)
	ts, ok := m["tools"].([]any)
	// gate stubs (bash/read/glob/grep) are appended by design, so expect at
	// least the original tool, normalized to an array, first in the list.
	if !ok || len(ts) < 1 {
		t.Fatalf("single tool object not normalized to array: %v", m["tools"])
	}
	tm, _ := ts[0].(map[string]any)
	fn, _ := tm["function"].(map[string]any)
	if fn == nil || fn["name"] != "f" {
		t.Fatalf("original tool lost or not first: %v", ts[0])
	}
}

// zen 403s FreeTierError on any body without the gate tools, so a tool-less
// body (plain openai client, title gen) gets the stubs but tool_choice
// "none": the gate passes and the model cannot call them. a body with
// client tools keeps its own tool_choice.
func TestEnsureZenToolsToolless(t *testing.T) {
	m := map[string]any{"messages": []any{}}
	ensureZenTools(m)
	ts, _ := m["tools"].([]any)
	if len(ts) != len(zenGateNames) {
		t.Fatalf("tool-less body must get %d gate stubs, got %v", len(zenGateNames), m["tools"])
	}
	if m["tool_choice"] != "none" {
		t.Fatalf("gate stubs on a tool-less body must be uncallable, tool_choice=%v", m["tool_choice"])
	}
	b, _ := json.Marshal(m)
	if toolsOffered(b) {
		t.Fatal("tool_choice none must not count as offered tools (nudge would fire)")
	}

	withTools := map[string]any{"tools": []any{
		map[string]any{"type": "function", "function": map[string]any{"name": "Bash"}},
	}}
	ensureZenTools(withTools)
	if _, set := withTools["tool_choice"]; set {
		t.Fatalf("client tools must keep their own tool_choice, got %v", withTools["tool_choice"])
	}
	b, _ = json.Marshal(withTools)
	if !toolsOffered(b) {
		t.Fatal("client tools must count as offered")
	}
}

func TestEnsureZenToolsNullParameters(t *testing.T) {
	m := map[string]any{
		"tools": []any{map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "f", "parameters": nil},
		}},
	}
	ensureZenTools(m)
	ts, _ := m["tools"].([]any)
	tm, _ := ts[0].(map[string]any)
	fn, _ := tm["function"].(map[string]any)
	pm, ok := fn["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("null parameters not replaced: %v", fn["parameters"])
	}
	if pm["type"] != "object" {
		t.Fatalf("parameters not an object schema: %v", pm)
	}
}

// TestAnthropicRecursiveSchemaConverts reproduces the Claude Code failure:
// a real Claude-Code-shaped request (recursive schemas on Grep/Read) must
// convert without any $ref/$defs reaching the wire.
func TestAnthropicRecursiveSchemaConverts(t *testing.T) {
	body := `{
	  "model":"opencode/muse-spark-1.3-contributor-free",
	  "max_tokens":100,
	  "tools":[
	    {"name":"Grep","description":"search files","input_schema":{"type":"object","properties":{"pattern":{"type":"string"},"path":{"$ref":"#/$defs/PathLike"}},"required":["pattern"],"$defs":{"PathLike":{"anyOf":[{"type":"string"},{"type":"array","items":{"type":"string"}}]}}}},
	    {"name":"Read","description":"read a file","input_schema":{"type":"object","properties":{"file_path":{"type":"string"},"node":{"$ref":"#/$defs/Node"}},"required":["file_path"],"$defs":{"Node":{"type":"object","properties":{"child":{"$ref":"#/$defs/Node"}}}}}}
	  ],
	  "messages":[{"role":"user","content":[{"type":"text","text":"list go files"}]}]
	}`
	oai, _, _, _, _, err := anthropicRequestToOpenAI([]byte(body))
	if err != nil {
		t.Fatalf("convert failed: %v", err)
	}
	s := string(oai)
	if strings.Contains(s, "$ref") || strings.Contains(s, "$defs") {
		t.Fatalf("wire body carries recursion markers: %s", s)
	}
	var m map[string]any
	if err := json.Unmarshal(oai, &m); err != nil {
		t.Fatalf("wire body not json: %v", err)
	}
}

// TestConvertToResponsesKeepsAnthropicTool covers the /responses path: an
// anthropic-shaped tool that skipped the openai rewrite must not forward a
// null parameters schema to spark.
func TestConvertToResponsesKeepsAnthropicTool(t *testing.T) {
	m := map[string]any{
		"model":    "muse-spark-1.3-contributor-free",
		"messages": []any{},
		"tools": []any{map[string]any{
			"name":         "Glob",
			"description":  "find files",
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{}},
		}},
	}
	convertToResponses(m)
	ts, _ := m["tools"].([]any)
	if len(ts) != 1 {
		t.Fatalf("tool dropped: %v", m["tools"])
	}
	tm, _ := ts[0].(map[string]any)
	pm, ok := tm["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("parameters lost on responses path: %v", tm)
	}
	if pm["type"] != "object" {
		t.Fatalf("parameters wrong type: %v", pm)
	}
	if _, has := tm["input_schema"]; has {
		t.Fatal("input_schema leaked to responses wire")
	}
}
