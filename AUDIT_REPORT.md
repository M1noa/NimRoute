# NimRoute — audit report

Date: 2026-10-15. Scope: the whole repository (87 project files, ~57k LOC).
Method: `project-audit` (file-by-file, ten categories) driven by
`audit-tracker.cjs`; findings land in `AUDIT_TRACKER.json` / `AUDIT_TRACKER.csv`.

Three tiers are used and kept separate, because "scanned for a class of bug"
is not the same claim as "read":

| Tier | Meaning |
|---|---|
| **Read** | Read in full, or across every region that carries logic |
| **Targeted** | Read the regions that carry the invariants named below |
| **Scanned** | Swept by pattern for specific classes (secrets, unbounded reads/growth, ignored errors, sleeps, panics, test skips) |

## Coverage

**Read** — `main.go`, `config.go`, `zenproxy.go`, `zenlanes.go`, `freepi.go`,
`antseedp2p.go`, `usage.go`, `route.go`, `anthropic.go`, `zentools.go`,
`piiwiring.go`, `rtkcompress.go`, `rtkdetect.go`, `.github/workflows/build.yml`,
`go.mod`.

**Targeted** — `nous.go` (send, key pool, both handlers, snapshot),
`openrouter.go` (refresh, cache, overlay, extras), `debugtrace.go` (capture,
end, `dumpIfErr`, redaction), `modelinfo.go` (`globMatch`, lookup),
`rtk.go` (filters, guards, helpers), `antseed.go` (`antRefresh` throttle),
`nimstatus.py` (what it prints), `scripts/*.py` (error handling),
`pii/*.go` (TTL, caps, eviction, sweep), the CLI path in `main.go` (`probe`,
`models sync`).

**Scanned** — `benchmarks.go`, `pii/fetch_model.sh`, `nim`, `docs/**`,
`config.yml.example`, and the 27 Go test files (skip inventory, assertion
strength).

**Skipped (vendored / data, not project code)** — `system-prompts/**`,
`guardrails.json`, `aa_benchmarks.json`, `openrouter_models.json`,
`keys.jsonc`, `.zenpool.json`, `testdata/*.json`,
`scripts/opencode_ballast.json`.

Nothing in the tree is unaccounted for: every file is in one of those buckets.

## Findings

### Performance

**P1 — streaming NIM responses are buffered whole. HIGH.** `main.go:3652`
The upstream body is `io.ReadAll`'d (up to 128MB) into `rb`, then re-wrapped as
`io.NopCloser(bytes.NewReader(rb))` and only then written out — including when
the client asked for a stream (`isStream`). So the first byte a streaming
client sees arrives after the model has finished generating, which is the one
cost a proxy must not add. Token accounting only needs the usage tail.
*Fix:* pipe `lastResp.Body` straight through for `isStream` (as the zen path
does with `copyZenChatStream`) and tee just the last few hundred bytes for
`respTokens`.

**P2 — the upstream body cap is too generous to be a bound. MEDIUM.** `main.go:43`
`upstreamBodyLimit = 128 << 20` with up to 16 in-flight slots allows ~2GB
resident, and the comment presents it as protection against memory exhaustion.
Debug mode keeps a second full copy (`respBodyBuf`). A 4–8MB cap for buffered
paths plus streaming for the large ones makes the bound real.

**P3 — the route table is rebuilt per request, and a cold cache fetches upstream on the request path. MEDIUM.** `route.go:76`
`buildRouteTable` unions every live backend's catalog and is called from
`routeCandidates` (every `route/<key>` request), `appendRouteModels` (every
`/v1/models`) and `routeSnapshot` (every `/status` poll — `./nim status-tail`
polls at 1Hz, so that is a full catalog union per second). Worse, when
`ocInfo` is empty it falls back to `fetchZenModelIDs()` (`main.go:753,787`),
which is an uncached `GET /models` with a **10s timeout** — the comment two
lines above (`route.go:93`) says a per-request fetch "would block on upstream",
and the fallback does exactly that. `antRefresh()` in the same function is
throttled (`antseed.go:659`), so the antseed leg is cheap; the zen leg is not.
*Fix:* cache the table with a short TTL (or rebuild on catalog change) and
never block a request on the listing fetch — return what is cached.

**P4 — `--dbg-file` writes an unbounded number of failure files. MEDIUM.** `debugtrace.go:281`
The flag (documented at `docs/service.md:33`, and recommended at
`docs/troubleshooting.md:33` — *for diagnosing 429 storms*) dumps one
`<nano>-<reqid>.json` per failed request into `nimroute-debug/`. There is no
file count cap, no size cap, no rotation and no pruning anywhere in the tree
(the only `os.Remove` is an unrelated temp file in `openrouter.go:488`), and
each dump carries request **and** response bodies up to 256KB per hop
(`traceUpRespCap`). The documented workflow for the failure mode this proxy
lives in is therefore the one that fills the disk.
*Fix:* prune to a max count/size at dump time; note in the docs that dumps
contain client content, not just headers (`safeHeaderMap` redacts credentials,
not bodies).

### Improvement

**I1 — `verifyProxy` writes off exits that are merely busy. MEDIUM.** `zenproxy.go:513`
Any non-200 fails verification, including a 429/403 from the shared public key
on `GET /models`. An exit that serves chat fine but is rate-limited on the
listing endpoint is out for the whole sweep, which is exactly how a pool ends
up smaller than the lane table. An independent `curl` probe through 80
candidates showed the same ~1-in-10 dial rate, so today's losses are real dead
proxies, not false negatives — but the status mix is worth logging per sweep so
that a shift in the upstream gate shows up as pool shrinkage instead of a
mystery.

**I2 — 4096-entry caps never evict. LOW.** `main.go:365` (`noteKey`, and
`ModelLock.lock` at `main.go:80`)
Both maps stop inserting past 4096 distinct model ids and never drop expired
entries, so stickiness and per-key model lockouts quietly stop applying to new
ids once a client has cycled that many. Evicting the oldest expired entry keeps
the feature working instead of silently disabling it.

**I3 — `outcomesByModel` is uncapped, so model health grows forever. MEDIUM.** `usage.go:112`
`totals` is capped at 4096 with an `(other)` bucket *precisely because*
"client-supplied model ids are unbounded" (`usage.go:197`) — but the same file
appends to `outcomesByModel[model]` for every id that reaches an upstream
error, with no cap and no eviction. `handleNvidia` (`main.go:3544`) forwards
any client-chosen model string upstream, and `logUsage` records that string, so
a client looping unique ids (`nvidia/x1`, `nvidia/x2`, …) grows the map one
20-entry ring per id, permanently. It also lands in `modelHealthSnapshot()`,
which `StatusFor` serializes into `/status` — a payload polled continuously by
`nimstatus.py` and `./nim status-tail`, so the status response grows with the
attacker's id namespace too.
*Fix:* cap `outcomesByModel` the way `totals` is capped, or only record
outcomes for ids present in the catalog/listing.

**I4 — record only the winner's latency on the anthropic race path. LOW.** `route.go:424`
The openai-compat race path records latency only for a completed winner and
says why (`route.go:323` records under `if won`, `route.go:340`: "only the
winner's latency is recorded: a loser's pollutes the ranking", with `routeRace`
recording through `rec.label`). The anthropic path calls
`noteRouteLatency(backend, time.Since(t0))` unconditionally for every
candidate and never sets `rec.label`, so the EWMA behind the `latency`
strategy is fed by losers and failures on that path.

### Sloppy code

**S1 — freepi error mapping keys off the message text. LOW.** `freepi.go:581`
`freepiClientStatus` matches the substring `hourly_ceiling` in the error
*message*; it only works because `freepiFetch` happens to append `(code X)` to
the text. Passing the parsed `fe.Code` makes the mapping independent of
upstream wording.

**S2 — `globMatch` is not a glob. LOW.** `modelinfo.go:357`
Pattern parts are located anywhere in the target in order, unanchored: `claude-*`
matches `my-claude-x`, and a pattern with no `*` at all (`spark`) matches any id
merely containing it. It backs `route.exclude_models` (`route.go:38`),
`claude_map` (`anthropic.go:123`) and the nudge overrides
(`modelinfo.go:340`), so every one of them over-matches silently. Either anchor
the first and last parts or document the substring semantics.

### Bad code

**B1 — a length bug panics the process. INFO.** `antseedp2p.go:417`
`uint16be` panics on a length above 64KB. Every call site is ours and the
decoder validates lengths, so it is not reachable from peer input today; if a
peer-driven path ever writes a frame, the whole proxy dies instead of dropping
one connection.

**B2 — `zentools` panics at init on a malformed ballast. INFO.** `zentools.go:22`
`panic("zenballast json: " + …)` on an embedded build-time constant. Fail-fast
on a constant is defensible and this one cannot be triggered at runtime; noted
because it is a hard crash inside `init` of the serving binary rather than a
degraded start (the ballast is also load-bearing for the free-tier gate, so a
silent partial start would arguably be worse).

### QoL

**Q1 — mistyped config keys are silently ignored. MEDIUM.** `config.go:547`
`yaml.Unmarshal(raw, &c)` is not strict, so `always_prox: true` is accepted and
does nothing. The file already appends commented defaults for *missing* keys
(`mergeMissingKeys`); an unknown-key warning would catch the other half of the
same mistake class.

**Q2 — several tests pass by skipping when their fixture is missing. LOW.** `main_test.go:2570`, `main_test.go:2857`, `main_test.go:639`, `todo_small_test.go:113`
353 test functions across 27 files, 11 `t.Skip` sites. Most are correctly
environment-gated (`pii/ner_ort_test.go` on `RAMPART_MODEL`/`ORT_LIB`,
`zen_schema_test.go` on `ZEN_CALIBRATE`, the live-quota probe on `-short`).
Four are not: "no verified pool available to escalate onto", "no listen", "no
dump file", "cannot chdir" skip when a fixture the test could own did not
materialize, so a broken fixture reads as a pass. The pool one guards the
pin/rotation invariant this whole project rests on — that test should build the
pool it needs (or `t.Fatal`) rather than skip.

### Dependency

**D1 — a pseudo-version pin. INFO.** `go.mod`
`github.com/shota3506/onnxruntime-purego v0.0.0-20260315223538-8db8bd7424b2`
pins a commit rather than a release, so `go get -u` cannot track a release
line and the `-tags ner` path is tied to that snapshot. Four direct
dependencies total, two of them indirect (`purego`, `x/sys`) — deliberately
small, nothing to trim.

## Checked and clean

Recorded so a later pass does not re-litigate it.

- **Auth:** `requireAuth` gates every `/v1/*` path at the single dispatch site;
  `/v1/models`, `/status`, `/health` are open by design; `checkAuth` uses
  `subtle.ConstantTimeCompare`.
- **Status redaction:** keys, locks, zen session/proxy, lanes, lane cap, pool,
  freepi, antseed, nous, usage, health and benchmarks are all cleared when
  `auth.tokens` is set and the caller is unauthenticated; proxy URLs pass
  through `redactProxyUserinfo` in both `poolSnapshot` and `laneSnapshot`;
  lane ids derived from client headers are sha256-truncated, never echoed.
- **Secrets:** the freepi jwt never reaches logs or `/status` (it appears only
  as a request header in `freepi.go`); trace headers are redacted by
  `safeHeaderMap`; the deny-lists are present on the NIM, zen
  (`zenHeaderForwarded`), and nous (`nousFwdHeaders`, same six names) paths,
  and the zen list also drops the four `x-opencode-*` the proxy sets itself.
- **Caps:** every upstream body read observed uses `io.LimitReader`;
  `usage.totals` capped at 4096 with an `(other)` bucket (`usage.go:200`); `outcomesByModel`
  capped per model at 20 (globally uncapped — I3); `routeRecorder` fails a
  candidate past 32MB instead of buffering it; `pii` vault bounded by
  `VaultMaxSize` + LRU, scan cache by its own TTL.
- **`pii/`:** vault TTL and scan-cache TTL are separate and `SweepScanIdle`
  provably never touches vault entries (`pii/scan.go:109`), the invariant
  AGENTS.md calls out; session separation has its own test
  (`pii/isolation_test.go`).
- **`nous.go`:** rotates keys on 429/529, clears on success, and when every key
  was already cooling returns the pool's own 429 with `Retry-After` instead of
  a synthesized 502/503; response headers are allowlisted (no upstream
  `Content-Length`, `Set-Cookie` or `Content-Encoding` relayed after a body
  transform); only key *names* reach `/status`.
- **`openrouter.go`:** a failed fetch keeps the stale cache / previous index
  rather than emptying the listing; catalog facts never overwrite the curated
  file (`mergeOREntry`).
- **`rtk`:** fail-open by construction (`rtkCompressText` returns the input when
  the result is empty or larger, `rtkShrink` refuses non-savings), skips
  `is_error` results and bodies beginning `Error:`, and guards non-string
  `text` parts (`rtkcompress.go:setTextParts`) against a client-shaped panic.
- **`usage.go`:** 0600 sink, dead-sink warn-once, reopen on
  `usage.path` change so a renamed inode cannot eat writes forever.
- **Logging:** no `TODO`/`FIXME`/`HACK` markers anywhere in the Go tree; no
  `time.Sleep` on the request path (only the `probe` CLI path, the config
  watcher and the pool sweep ticker).
- **Python:** no `shell=True`, `os.system`, `eval` or bare `except: pass`
  anywhere in `scripts/**`; `nimstatus.py` prints key *names* and token counts,
  never secret values; ledger errors surface as typed exceptions.
- **CI:** actions pinned to majors, `permissions: contents: read`,
  `gofmt`+`go vet` (both tag sets), `go test -race ./...`, `-tags ner` pii
  tests, `python3 -m unittest scripts.test_nimstatus`, a 6-target
  linux/darwin/windows build matrix with version stamped from the tag, 7-day
  artifact retention.
- **Bind:** `server.bind` defaults to `127.0.0.1`; `/status` unauth exposure is
  opt-in.
