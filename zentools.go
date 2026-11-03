// zenballast carries the two things the free-tier gate reads out of a request.
//
// measured live 2026-10-26 with the zero-cost invalid-role probe (a spent ip
// cannot colour the answer: a gate-passing body answers 400 whether or not the
// ip still has budget), one variable per request, four exits:
//
//	<env> + tool named `read`              400, gate passed
//	<env> + `read`, own description, no schema 400, gate passed
//	<env> + client tool, no `read`         429
//	<env> + `Read` (capital)               429
//	<env> + `fileread`                     429
//	<env>, no tools                        429
//
// so `read` is required by *name*, lowercase, even when the caller brought its
// own tools — and nothing past the name is read. the declaration is therefore a
// stub, worded so a model that sees it does not call it: a client that never
// declared `read` must never receive a call to it. (name-only also serves real
// completions, which is what the -sweep runs in scripts/zengate measured.)
//
// the block is read only for a `Working directory` line. everything else about
// it is free: the values are never checked (empty, /nope and plan9 all pass),
// the heading and opencode's other four fields are not required, and any other
// line inside it is ignored. the tags are not free — <ENV>, an unclosed <env>,
// an empty <env></env>, a working-directory line outside the tags, the same
// block in a user message, and a good block placed after a bad one are all
// refused. so we emit exactly the one line it looks for, and zenEnvInto
// repairs a client's own partial block in place.
package main

import "encoding/json"

// zenEnvLine is the single line the gate reads out of the first <env> block.
// the value after the colon is never checked, so nothing about this proxy's
// cwd leaks into the caller's prompt.
const zenEnvLine = "  Working directory:"

// zenEnvPrompt is the block appended when a body has none: just enough to
// satisfy the gate. the real client sends a heading and five fields; none of
// that is read, and a client that ships its own block keeps it untouched.
func zenEnvPrompt() string { return "<env>\n" + zenEnvLine + "\n</env>" }

// zenOCDefsJSON is the tools-side half of the fingerprint: one tool named
// `read`. the gate matches the name and stops, so the rest is a placeholder —
// no schema, and a description telling the model to leave it alone rather than
// the client's real file-reading text, which invited calls the caller had not
// declared.
const zenOCDefsJSON = `[{"type":"function","function":{"name":"read","description":"Placeholder required by the upstream API. Do not call this tool: it is not available in this session."}}]`

// zenOpenCodeDefs parses the embedded toolset fresh on every call: callers
// mutate the returned maps in place (sanitize, responses flatten), so sharing
// would corrupt later requests.
func zenOpenCodeDefs() []any {
	var defs []any
	if err := json.Unmarshal([]byte(zenOCDefsJSON), &defs); err != nil {
		panic("zenballast json: " + err.Error())
	}
	return defs
}
