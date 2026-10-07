# CHANGED-SINCE-682b2eb.md

Baseline: `682b2eb` (2026-10-04). HEAD: `b4fa0b3` (2026-10-05 17:19).
7 commits, 19 files. Today is 2026-10-06, so: oct 4 commits = 2 days ago,
oct 5 commits = 1 day ago, worktree edits = 0 days (not committed yet).
There is a separate pre-existing merge on main that is NOT part of this list.

Legend:
- [x] = rollback / undo / remove
- [ ] = good / leave / fine

Usefulness score: my opinion, 10 = must keep, 1 = delete it. Based on the full
audit: gofmt clean, go vet clean, all tests pass (8.6s).

---

## `5cf418a` — big rewrite (2 days ago, oct 4)
14 files. The big one: new rotation, new streaming, cancel support.

- [ ] **Instant rotation on limits** (9/10). When an exit says "slow down", the
  proxy immediately hops to a fresh exit + session instead of waiting in a
  cooldown queue. This is the core fix. Tested.
- [ ] **Fast-exit picking from real timing** (8/10). Remembers how fast each
  exit answered before and prefers fast ones. Tested.
- [ ] **Real streaming** (9/10). Responses now stream out as they generate
  instead of arriving all at once. Biggest visible win. E2E tested both
  protocols.
- [ ] **Cancel support** (8/10). If you close the tab mid-answer, the proxy
  stops asking upstream too. Saves quota. Tested.
- [ ] New streamer code (`respStreamer`/`feed`) (9/10). Part of streaming
  above. This is where block bookkeeping lives — it has regression tests now.
- [ ] `handleOpenCodeAnthropic` churn (7/10). The opencode forward path was
  rewritten. Risky area, but the e2e test covers it and it passes.
- [ ] Whole zenlanes.go rewrite (9/10). If things got flaky, suspect this
  first — but 8-concurrent and failover tests pass.

## `97dd096` — lane modes + stream guards (1 day ago, oct 5)
9 files. Lane identity modes + completeness guards on the streamer.

- [ ] **Lane identity modes** (`lane_hdr`/`lane_convo`/`lane_shared`/`lane_cache_key`) (7/10).
  Four switches for how chats get pinned to lanes. Convo mode is off by
  default because its hash is buggy — that honesty is good. Tested.
- [ ] Cache-key identity (7/10). Chats without a session header can still pin
  to a lane via `prompt_cache_key`. Tested, incl. "off = shared pool".
- [ ] **Stream-completeness guards** (8/10). Make sure the stream ends cleanly
  (stop reason, final block). If streams came out malformed, these guards are
  suspect #1 — but there are byte-split and tool-call tests that pass.
- [ ] Low-load lane race (6/10). Fires the request on two lanes when the pool
  is idle, keeps the faster one. Costs double quota, off by default,
  documented. Fine to leave off.

## `ad685b7` — usage bug fixes + conversions (1 day ago, oct 5)
10 files. Two real bug fixes plus conversion work.

- [ ] **Usage file fix + zero-token fix** (10/10). Usage was logging to the
  wrong file, and anthropic streams counted 0 tokens because zen sends usage
  AFTER the finish chunk. Both fixed, both verified live (1040 + 1298 tokens
  recorded). Pure win, keep.
- [ ] `anthropicRequestToOpenAI` changes (8/10). Body conversion for the
  anthropic→openai direction. Note: recursive schemas ARE now stripped by
  `sanitizeZenSchema` before they reach zen, with a 25-level-deep test. So
  the "nesting depth" error should be handled, not caused, by current code.
- [ ] `openAIToAnthropic` changes (8/10). Tool-name and shape conversion.
  Covered by tests (name case restore, tool calls through streams).
- [ ] model_info/config entries added here (7/10). 102 entries total
  (81 nim + 14 zen + 7 freepi). Keep unless a specific model is wrong.

## `936d822` — token counters (1 day ago, oct 5)
9 files. New per-lane, per-model token counters + `/status` fields.

- [ ] **Per-lane/per-model token counters** (7/10). `/status` now shows how
  many tokens each lane served and for which models. Cheap increments,
  tested, rides existing redaction. Useful, low risk.
- [ ] `nimstatus.py` tok column (7/10). Shows the same in your status bar.

## `6a7390b` — todo notes (1 day ago, oct 5)
- [ ] Notes only, no code. Safe. (n/a)

## `7195d39` — todo notes (1 day ago, oct 5)
- [ ] Notes only — no modelinfo code changed here. Safe. (n/a)

## `b4fa0b3` — todo notes (1 day ago, oct 5)
- [ ] Notes only: the model sweep result. Safe. (n/a)

---

## Your 3 bugs — who to blame

### "JSON schema exceeds the maximum nesting depth of 10 levels" (muse)
1. [x] `anthropicRequestToOpenAI` passing recursive schemas (1 day ago) —
   BUT current code strips recursion via `sanitizeZenSchema` with a test.
   If you still see this error on today's binary, the suspect list is wrong,
   not the code.
2. [x] Streaming/path selection change (2 days ago) — body shape reaching zen.
3. [x] Tool schema canonicalization (1 day ago) — deepening nesting. Covered
   by tests; unlikely.

### "response stream was malformed / incomplete" (space-bunny, intermittent)
1. [x] Stream guards (1 day ago) — first suspect, but byte-split/tool-call
   tests pass.
2. [x] The streaming rewrite itself (2 days ago) — whole-stream delivery test
   passes.
3. [x] `openAIToAnthropic` tool shape (1 day ago) — name-case restore test
   passes.

### "opencode via the proxy doesn't work anymore"
1. [x] `handleOpenCodeAnthropic` churn (2 days ago) — e2e test passes today.
2. [x] Lane identity rewrite (2 days ago + 1 day ago) — spread/failover tests
   pass.
3. [x] Live RTT weights (2 days ago) — routed-to-bad-exit theory. Weighted
   pick test passes; exits still die on real sends and get dropped.

Note: the primary on :5419 is still running the OLD binary. If you saw these
bugs before a restart, you were not running this code. Restart first, then
re-test before rolling anything back.

---

## Untracked files (0 days ago, not committed)
- [ ] `zen_schema_test.go` (9/10) — tests committed code, COMMIT this one.
- [ ] `config.cmp.yml`, `config.debug.yml`, `config.depth.yml`,
  `config.e2e.yml`, `config.test.yml` (2/10) — scratch, delete when done.
- [ ] `CHANGED-SINCE-682b2eb.md` (this file) — delete when triage is done.

## If you want to roll back, this order
1. [ ] Revert `97dd096` guards first — smallest, most tied to malformed
   streams. Re-test space-bunny.
2. [ ] Revert `ad685b7` conversion bits if malformation persists. Keep the
   usage fix if you can split it out.
3. [ ] Revert `5cf418a` last (biggest, everything depends on it).
4. [ ] Leave `936d822` counters (harmless, useful).
5. [ ] Delete scratch configs + this file once stable.

Before any revert: `git branch pre-rollback b4fa0b3` so you can redo pieces.
