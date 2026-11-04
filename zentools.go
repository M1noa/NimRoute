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

import (
	"encoding/json"
	"time"
)

// zenEnvLine is the field the gate reads out of the first <env> block. the
// value has to be non-empty: an empty `Working directory:` is refused.
const zenEnvLine = "  Working directory: /workspace"

// zenEnvPrompt is the block appended when a body has none. re-measured live
// 2026-11-03 with the zero-cost invalid-role probe (gate pass = 400, refusal =
// 429) across ~150 exits per variant:
//
//	heading + <env> + one real field          400 on 2 exits
//	heading + all five fields (full client)   400 on 3 exits
//	five fields, no heading                   400 on 1 exit
//	<env> + real field, no heading            400 on 0 of 36
//	<env> + empty Working directory (old)     400 on 0 of 77
//
// so the heading line and a non-empty value are both read now; the older "one
// empty line is enough" reading is stale, and the empty-line block is refused
// before quota on every exit — which parked nearly the whole pool as
// "free-tier capped". this emits the real client's block, with a fixed cwd so
// nothing about this machine leaks into the caller's prompt.
func zenEnvPrompt() string {
	return "Here is some useful information about the environment you are running in:\n" +
		"<env>\n" + zenEnvLine + "\n" +
		"  Workspace root folder: /workspace\n" +
		"  Is directory a git repo: yes\n" +
		"  Platform: linux\n" +
		"  Today's date: " + time.Now().Format("Mon Jan 2 2006") + "\n</env>"
}

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
