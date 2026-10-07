# NimRoute — full read-only audit

- **Target**: `/Users/minoa/Documents/Projects/NimRoute`, commit `5f87378`, working tree dirty (7 modified, `antseed_test.go` untracked)
- **Scope**: 88 source files (Go 23.6k LOC, Python 1.8k, shell/deploy/CI, data files, docs)
- **Mode**: read-only. No source file was modified, created, or deleted. This report is the only write.
- **Method**: 14 parallel auditors split by module, each reading its whole scope end-to-end. Every finding below is quoted from the source at the cited line.
- **Baseline**: `gofmt -l .` clean, `go vet ./...` clean, 5 direct dependencies, no hardcoded secrets by regex sweep.

Severity: CRITICAL (exploitable / data loss / crash) · HIGH (real bug or security control failure) · MEDIUM (should fix) · LOW (nice to fix) · INFO (observation).

> Status: sections are being appended as audit waves complete. Sections marked *(pending)* have not yet reported.

---

## CRITICAL

_None confirmed._

---

## HIGH

### H1. `freebuff_login.py` can duplicate a top-level YAML key, which makes the next start overwrite `config.yml` with the example
- **file:line** `scripts/freebuff_login.py:65`, `scripts/freebuff_login.py:76` (triggered through `config.go:511`)
- **issue**: The script recognises only its own `# --- freebuff (managed by ...) ---` block and then appends a second top-level `freebuff:` key. `config.yml.example:281-291` also documents a plain hand-editable `freebuff:` section, so a user who uncomments that (the documented path) and then runs the helper ends up with two top-level keys. `yaml.v3` decodes with `uniqueKeys: true`, so `loadConfigFile` fails; `config.go:511-517` then responds to the parse error by writing `config.yml.example` over the user's file with mode 0600.
- **impact**: total config loss. Every `nvidia_keys` entry, every freepi jwt, every local edit. The process starts on `defaultConfig()` and the user finds out at the next request.
- **fix**: Before appending, strip any existing top-level `freebuff:` block (`re.sub(r"(?m)^freebuff:\n(?:[ \t].*\n|\n)*", "", text)`), then write once with `open(cfg, "w")`. Separately and independently: `config.go:511` must seed only on `os.IsNotExist(err)`, never on a parse error — that alone is the data-loss bug.

### H2. `nimstatus.py` re-parses a 20 MB JSONL once per second while tailing
- **file:line** `nimstatus.py:37`, `nimstatus.py:319`
- **issue**: `render()` calls `jsonl_stats()`, which opens `nim-usage.jsonl` and runs `json.loads` on every line. The tail loop calls `render()` once per second. The file is currently ~19.9 MB / 75,996 lines and grows one line per request forever, so cost is O(file size) per second and grows without bound.
- **impact**: roughly 10-20% of a core burned continuously, plus churn from re-reading 20 MB every tick. `./nim status-tail` left open overnight on a busy proxy pegs a core and the render loop falls behind.
- **fix**: cache the aggregate keyed on `(path, mtime, size)` and only re-parse when the file changed; in the tail loop reuse the cached value between polls.

---

## MEDIUM

### M1. `model` is interpolated unescaped into the SSE preamble JSON (3 sites)
- **file:line** `anthropic.go:741`, `anthropic.go:1147`, `anthropic.go:1209`
- **code** `preamble := fmt.Sprintf(`{"type":"message_start",...,"model":"%s",...}`, msgID, clientModel)`
- **issue**: Every other variable in this file passes through `jsonStr` before being placed in a JSON string (lines 864, 870, 900, 903, 997, 1018). These three sites do not, and `clientModel` is `req.Model` straight off the wire (line 162), passed through `mapClaudeModel` unchanged when no pattern matches.
- **scenario**: `POST /v1/messages {"model":"x\",\"content\":[{\"type\":\"text\",\"text\":\"INJECTED\"}]}` emits a `message_start` carrying an attacker-shaped `content` array. With `\n\n` in the model, arbitrary extra SSE frames are appended to the stream. Impact is confined to the requesting client's own response — response splitting, not cross-user leakage. Secondary: the same unescaped value reaches `acclog` at 1664, 2343, 2406, which is log forging from an unauthenticated client when `auth.tokens` is unset.
- **fix**: wrap all three sites in `jsonStr(clientModel)`.

### M2. Text-then-reasoning reuses block index 0 and drops all subsequent text
- **file:line** `anthropic.go:858-863` (with 761-766, 868)
- **issue**: `closeText()` latches `textClosed = true` and nothing clears it. If a text delta arrives before the first reasoning delta, the text block was opened at index 0, then the thinking block re-opens index 0. Separately, the `!textClosed` guard at 868 silently drops every text delta that follows reasoning.
- **scenario**: an interleaved-reasoning model (the file claims support for space-bunny/big-pickle at line 391) emits text, then reasoning, then more text. The client sees `start(0,text) … stop(0), start(0,thinking)` and the post-reasoning answer text never arrives — a truncated turn.
- **fix**: assign `thinkIdx`/`textIdx` from a running counter instead of hardcoding 0/1, and open a fresh text block rather than latching `textClosed`.

### M3. The live `/responses` stream never terminates the turn when upstream ends without `response.completed`
- **file:line** `anthropic.go:1188-1194`
- **issue**: inside `feed`, `s.finish()` is only reached from the `response.completed` case when `last` is true (1125-1127). Nothing after the loop calls it. If the trailing `carry` does not contain a completion event, the client gets `message_start` plus partial blocks and then a closed connection — no `message_delta`, no `message_stop`, no `[DONE]`. The sibling `streamResponsesMerged` gets this right and calls `s.finish()` unconditionally at 1217.
- **scenario**: a proxy dies mid-event (routine per AGENTS.md §5 — 502/transport churn). `carry` holds the truncated line, `json.Unmarshal` fails at 1090 and `continue`s, `feed` returns unfinished. Claude Code blocks until its own timeout.
- **fix**: call `s.finish()` after `s.feed(append(carry, '\n'), true)`. `finish()` is already idempotent via `sentStop`.

### M4. Freebuff client leaves the retried agent run un-FINISHed, and discards the real error
- **file:line** `scripts/freebuff_client.py:289`, `scripts/freebuff_client.py:300`
- **issue**: the gate-recovery path issues a second START and overwrites the run id in the body, but the `finally` only ever FINISHes the first `run_id`. Second bug in the same block: if `st2 != 200` the code raises `FreebuffError(st, b)` with the *first* response, discarding the actual failure.
- **impact**: one orphaned `agent-runs` START per gate retry, invisible to the caller and to local logs — a direct violation of the module's own rule on line 8 ("FINISH every run").
- **fix**: keep `run_id2` in a local and FINISH it in the same `finally`; raise with `st2`/`b2`.

### M5. Freebuff client requests `stream: true` but never parses SSE, silently returning `{}`
- **file:line** `scripts/freebuff_client.py:230`, `scripts/freebuff_client.py:274`
- **issue**: the `stream` parameter is dead — the body always sets `"stream": True`. The response is decoded with a single `json.loads`; a streamed SSE body fails that parse, a bare `except` swallows it, and `chat()` returns `{}` with `ok=True` and failures reset to 0.
- **scenario**: any upstream honouring `stream: true` yields an empty dict. The caller sees a successful turn with zero content and no error.
- **fix**: set `"stream": stream` and add an SSE reader, or send `"stream": False` and document that only the non-stream shape is supported.

### M6. freepi jwt is written world-readable and is not gitignored
- **file:line** `scripts/freepi_login.py:67`, `.gitignore:1-6`
- **issue**: the account jwt is written to `freepi_jwt.txt` at process umask (typically 0644, readable by every local user). `save_into_config` in the same file correctly chmods `config.yml` to 0600, so this is an inconsistency within one file. `.gitignore` covers `keys.json`, `keys.jsonc`, `config.yml`, `proxies.txt`, `.env` — not `freepi_jwt.txt`.
- **scenario**: shared machine, any local user reads the jwt; or `git add -A` stages and pushes the bearer token.
- **fix**: open with mode 0600 (`os.open(out, os.O_WRONLY|os.O_CREAT|os.O_TRUNC, 0o600)`), and add `freepi_jwt.txt` to `.gitignore`.

### M7. `.gitignore` does not cover the scratch config copy that AGENTS.md mandates
- **file:line** `.gitignore:4`
- **issue**: the exact-match `config.yml` does not cover `config.test.yml`, which AGENTS.md instructs the agent to create as a copy of `config.yml` holding real `nvidia_keys` and freepi `jwt`s. The same file already ignores the binary and the usage log, so the intent is clearly "never commit runtime state" — the scratch-config case is a hole in that.
- **scenario**: agent follows AGENTS.md, creates `config.test.yml` with live keys, forgets to delete it, `git add -A` → keys pushed.
- **fix**: change line 4 to also match `config.*.yml`.

### M8. systemd and WinSW units pin `PORT=5419`, silently overriding `config.yml`
- **file:line** `deploy/nimroute.service:12`, `deploy/nimroute.xml:11`, overriding `main.go:4261`
- **issue**: `main.go:4261-4263` reads `PORT` and overwrites `c.Server.Port` unconditionally, so the unit env var wins over the config file the rest of the project treats as source of truth. The launchd plist sets no `PORT`, so macOS and Linux/Windows already behave differently from the same repo.
- **scenario**: `server.port: 5420` + `./nim install` on Linux → process listens on 5419. `nim status` also probes 5419, so it reports healthy while every client configured for 5420 is refused.
- **fix**: drop `Environment=PORT=5419` from both templates (or comment it out like the existing `CONFIG_FILE` line).

### M9. The shipped default is an open proxy bound to every interface
- **file:line** `config.yml.example:19` (`tokens: []`), `main.go:4267` (`addr := fmt.Sprintf(":%d", port)`)
- **issue**: `defaultConfig()` leaves `auth.tokens` empty, the example ships `tokens: []`, and the listener is `:port` — all interfaces, not loopback. The default install is therefore reachable from the whole LAN with no key, and `deploy/nimroute.service` (`Restart=always`) plus the launchd plist (`KeepAlive`) make it a persistent open relay.
- **scenario**: laptop on hotel wifi, `./nim install` → `curl http://<lan-ip>:5419/v1/chat/completions` works with no key and spends the account's quota while third parties burn the zen lane pool.
- **fix**: add a `server.bind` key defaulting to `127.0.0.1`, and note in the example that `tokens: []` is only safe on loopback.

### M10. A missing or malformed `guardrails.json` fails open with no log line
- **file:line** `config.go:745`, `guardrails.json`
- **issue**: both the read error and the unmarshal error are discarded. A truncated or schema-drifted corpus leaves `guardrailPrefixes` empty, `stripSomeGuardrails` returns text untouched, and the only startup log for guardrails (`main.go:4292`) fires solely when `guardrails.enabled` is false. "Stripping silently not running" is indistinguishable from "nothing matched".
- **impact**: a stated security control (config.yml.example:34, strip baked-in system-prompt refusals) can fail open with zero signal.
- **fix**: log a WARN on read and unmarshal error with path and parse error, and expose `len(guardrailPrefixes)` on `/status` so a zero is visible. (The corpus itself is fine — 456/456 entries carry the key the loader reads.)

### M11. `_req` does not catch transport errors, and admission retries are unbounded
- **file:line** `scripts/freebuff_client.py:143`, `scripts/freebuff_client.py:196`
- **issue**: only `HTTPError` is caught, so `URLError`, socket timeouts, TLS errors and connection resets escape as raw exceptions — the documented exp-backoff behaviour covers HTTP statuses only. The retry `while True` has no attempt cap or deadline.
- **scenario**: DNS blip → `URLError` propagates instead of backing off; a permanently-503 endpoint pins a client in `while True` with no way to observe or bound it.
- **fix**: add `except urllib.error.URLError` → synthesize a retryable `FreebuffError(503, str(e))`, and give the loop a deadline or attempt cap.

### M12. `./nim status` always exits 1, even when the service is healthy
- **file:line** `nim:103`
- **code** `status)  pretty_status; [ "$2" = "tail" ] && status_tail ;;`
- **issue**: `pretty_status` deliberately returns 1 when STOPPED so callers can branch. The trailing `[ "$2" = "tail" ]` is the last command of the case branch, so with `$2` unset the AND-list evaluates to 1 and *that* becomes the script's exit status. The status is clobbered in both directions.
- **scenario**: `if ./nim status >/dev/null; then echo up; fi` never prints "up". Any monitoring wrapper is permanently red.
- **fix**: `status)  pretty_status; if [ "${2:-}" = "tail" ]; then status_tail; fi ;;` — an `if` with a false condition and no `else` returns 0, preserving the intended code.

### M13. CI actions are pinned to floating tags, including the one holding `contents: write`
- **file:line** `.github/workflows/build.yml:70` (`softprops/action-gh-release@v2`), `:29`
- **issue**: every action is referenced by tag, not by commit SHA. `softprops/action-gh-release` is third-party and runs in the job that uploads release binaries — a retagged `@v2` is release-forgery capability.
- **fix**: pin all actions to full SHAs with a `# vX.Y.Z` comment; enable Dependabot for updates.

### M14. No top-level `permissions:` on the workflow
- **file:line** `.github/workflows/build.yml:9`, `.github/workflows/build.yml:59`
- **issue**: the `release` job scopes `contents: write`, but no top-level `permissions:` is set, so the `build` job — including `go test ./...` over untrusted PR code — inherits the repository default token scope.
- **fix**: add `permissions: contents: read` at workflow top level, keep the `contents: write` override on `release` only.

### M15. Status tooling hardcodes 5419 while the server port is config-driven
- **file:line** `nimstatus.py:9`, `nim:37`
- **issue**: `PORT = os.environ.get("PORT", "5419")` and every `curl` in `nim` fall back to the literal. Nothing reads `server.port`. `port_alive` then reports STOPPED for a proxy on any other port, so `nim start` attempts a second bind and `nim restart` reports a phantom failure.
- **fix**: have `nim` parse `server.port` out of config.yml (or honour `PORT`) and export it.

### M16. 432 lines of freebuff Python plus a `freebuff:` config section have no consumer in the binary
- **file:line** `scripts/freebuff_client.py:1`, `scripts/freebuff_login.py:1`, `config.yml.example:281`
- **issue**: `grep -rl freebuff *.go` and `grep -n freebuff config.go` both return nothing. There is no `freebuff:` field in `appConfig` and no code path in any `.go` file. `freebuff_login.py` is the dangerous half — it writes a bearer token into a section nothing reads, which is also the trigger for H1.
- **fix**: delete both scripts and the commented block until the Go side exists, or add the backend.

### M17. Redundant whole-body scans and byte→string copies against a 128 MB budget
- **file:line** `anthropic.go:1397` (`scanResponsesSSE`), `anthropic.go:1547` + `:1554` (double scan), `anthropic.go:1351-1356` (`stripSentinelSSE`)
- **issue**: `upstreamBodyLimit` is `128 << 20` (main.go:38). Each `scanResponsesSSE` does `strings.Split(string(body), "\n")` — a full copy plus a slice header per line — then `json.Unmarshal` per line. `nb2` is scanned twice: at 1547 and again inside `isWaitOnly` at 1554, which recomputes the exact `text`/`hasCalls` pair already in scope. `stripSentinelSSE` does `string(body)` twice more (`Contains`, then `ReplaceAll`), each a fresh full copy.
- **impact**: peak transient memory per request is roughly 3-5x the body size on the nudge path; `toolsOffered(oaiBody)` at 1515 re-parses the up-to-10 MB request body that line 2088 already parsed.
- **fix**: compare `strings.TrimSpace(text2) == waitSentinel` instead of calling `isWaitOnly(nb2)`; use `bytes.Contains`/`bytes.ReplaceAll` and `bytes.Split`.

---

## LOW

### L1. `nim` writes a world-writable PID file in `/tmp` and kills whatever PID it finds
- **file:line** `nim:29`, `nim:52`, `nim:67`
- **issue**: a fixed path in a world-writable directory, and `kill` trusts its contents with no identity check. Another local user who creates `/tmp/nimroute.pid` first — or a stale file from a `kill -9`'d process whose PID has since recycled — makes `nim stop` signal an unrelated process. `svc_running` has the same trust problem, so `nim start` can also treat a foreign PID as "running".
- **fix**: use `${TMPDIR:-/tmp}/nimroute.$$.pid` under `umask 077`, and verify with `ps -p "$pid" -o comm=` before signalling.

### L2. `freepi_login.py` can append the jwt under the wrong YAML section
- **file:line** `scripts/freepi_login.py:96`
- **issue**: the lazy `(?:.*\n)*?` runs to end-of-file when `accounts:` is followed by a section with no blank line between them, and there is no check that the insertion landed inside the `freepi:` block. The fallback only fires when the block is absent entirely, which does not happen here.
- **fix**: locate the `freepi:` block by line index and insert inside its span, or parse/re-emit with yaml.

### L3. The managed-block regex drops accounts not written with double quotes
- **file:line** `scripts/freebuff_login.py:67`
- **issue**: only the exact double-quoted form the script itself writes is recognised. Edit a value to single-quote or unquote it and the next run drops it from `accts` and rewrites the block without it — the credential is deleted with no message.
- **fix**: tolerate `['"]?([^'"\n]+)['"]?`, or reuse yaml instead of regex-editing yaml.

### L4. `nimstatus.py` reads the whole log to show the last N lines, and crashes on a non-numeric N
- **file:line** `nimstatus.py:329`, `nimstatus.py:333`
- **issue**: `f.readlines()[-n:]` materialises the entire log (rotation is manual) to slice off the last 10 lines — `nim`'s own fallback correctly shells out to `tail -10`. And `int(sys.argv[2])` is unguarded, so `nimstatus.py logs abc` prints a traceback.
- **fix**: bounded seek or `tail -n "$n"`; wrap the int conversion in a `try`.

### L5. `probe_zen_proxies.py` mutates module state from worker threads and can divide by zero
- **file:line** `scripts/probe_zen_proxies.py:33`, `scripts/probe_zen_proxies.py:201`
- **issue**: `new_id` read-modifies-writes the module-level `_seq` dict without a lock, so two threads in the same millisecond can both take the `n += 1` branch and emit the same `x-opencode-session` — the exact collision the ids exist to prevent. Separately, `--models ""` yields an empty list and `models[k % len(models)]` raises `ZeroDivisionError`.
- **fix**: use `itertools.count()` (thread-safe in CPython); skip the modulo when `models` is empty.

### L6. Arg parsing in the login scripts indexes past the end of `sys.argv`
- **file:line** `scripts/freebuff_login.py:33`, `scripts/freepi_login.py:33`
- **issue**: `cfg = sys.argv[i + 2]` has no bound check, so a trailing flag raises `IndexError` with a traceback instead of the usage message. `freepi_probe.py:86` and `probe_zen_proxies.py` do guard — the two login scripts are the outliers.
- **fix**: copy the `i + 1 < len(args)` guard from the sibling scripts.

### L7. `antseed_test.py` indexes an empty list and interpolates a model id into a URL path
- **file:line** `scripts/antseed_test.py:115`, `scripts/antseed_test.py:122`
- **issue**: `if not data: return 1` guards an empty catalog but not an empty `text` list, so a catalog of only non-text entries raises `IndexError` instead of printing FAIL. And `one` comes from the network catalog and is spliced into the path unquoted, so an id containing `/`, `?` or `#` changes which endpoint is called.
- **fix**: guard `if not text:`; `urllib.parse.quote(one, safe="")`.

### L8. `freepi_probe.py` has a dead close expression operating on the wrong type
- **file:line** `scripts/freepi_probe.py:44`
- **issue**: on the non-200 path `resp` is already a dict, so `resp.close() if hasattr(resp, "close") else None` evaluates to `None` — a no-op statement. The error message also interpolates the whole response dict including the `_raw` body slice.
- **fix**: delete the line; close the response where a streamed response is actually abandoned.

### L9. `nimstatus.py` double-formats integers and hardcodes a lit-lane assumption
- **file:line** `nimstatus.py:66`, `nimstatus.py:138`
- **issue**: `"%s" % f"{n:,}"` is a no-op round trip through two formatters. Separately, `merged_table` renders every lane as a green `● live` and never reads any per-lane state field, so the `state` column is decorative.
- **fix**: simplify to `return f"{n:,}"`; source lane state from `/status` or drop the column.

### L10. `.gitignore` misses the default probe output and relies on a local-only exclude for `.freebuff/`
- **file:line** `.gitignore:43`
- **issue**: `probe_zen_proxies.py:134` defaults `out_path` to `zen_probe.tsv`, and that exact name is not ignored while its two siblings are. Separately `.freebuff/` is excluded only via `.git/info/exclude`, which is per-clone and not shared — a fresh clone has no rule for it.
- **fix**: change line 43 to `zen_probe*.tsv`; move `/.freebuff/` into `.gitignore`.

### L11. CI pins Go 1.22 against a module requiring 1.26, and has no vet/fmt gate
- **file:line** `.github/workflows/build.yml:33`, `.github/workflows/build.yml:36`
- **issue**: `go.mod` declares `go 1.26.0`. With `GOTOOLCHAIN=auto` the run still works because the toolchain is downloaded on demand, but the pin is fiction — every run installs 1.22 then immediately replaces it, and the real toolchain is whatever Go fetches at runtime. Separately `go test ./...` is the only check; AGENTS.md makes `gofmt -l .` and `go vet ./...` pre-commit gates and neither runs in CI, and no job runs with `-race`.
- **fix**: use `go-version-file: go.mod`; add `gofmt` + `go vet` as a separate non-matrix job.

### L12. `config.go`'s comment on `max_retries` contradicts the example
- **file:line** `config.go:173` vs `config.yml.example:230`
- **issue**: the Go comment says `0 = single attempt, no retry`; the example says `0 = same default, negative = single attempt`. `zenlanes.go:432-439` returns 5 for 0 and 1 for negative, so the example is right and the Go comment is wrong.
- **fix**: correct the comment to `0 = same default, negative = single attempt`.

### L13. Data race on `lane` between the hedge goroutine and the retry loop
- **file:line** `anthropic.go:2187` (read) vs `:2239`, `:2262`, `:2278`, `:2299` (write); declared `:2119`
- **issue**: `lane` is captured by reference into the hedge closure. `hedgedDo` (zenlanes.go:354-363) returns as soon as one side wins and drains the loser in a detached goroutine, so the hedge can still be reading `lane` while the main goroutine reassigns it.
- **scenario**: primary takes >2 s (`hedge_after_ms`), hedge fires, primary then returns 429 → write at 2239 races the hedge's read. Undetected without `-race`.
- **fix**: snapshot the lane into a local before building the hedge closure.

### L14. Held-back sentinel text is dropped when a tool call arrives
- **file:line** `anthropic.go:761-766`, `anthropic.go:773`, `anthropic.go:883`
- **issue**: `sentinelStripper` holds back up to `len(waitSentinel)-1` = 24 trailing characters in case they form a sentinel prefix. `closeText` never flushes that buffer (contrast `respStreamer.closeText` at 1005, which does), and `flushStrip` at 773 then refuses to emit because `hasToolCalls` and `textClosed` are already set.
- **scenario**: deltas `"the answer is 42 SPARK_WAITING_FOR"` followed by a `tool_calls` delta — the 16 held-back characters are lost from the assistant turn.
- **fix**: flush the stripper inside `closeText` before emitting `content_block_stop`.

### L15. Non-stream `tool_use` emits an empty `id` where the stream path synthesises one
- **file:line** `anthropic.go:670-675` (compare `:896-899`)
- **issue**: the streaming emitter guards this (`if toolID == "" { toolID = "toolu_" + randHex(24) }`); the buffered emitter does not. An upstream omitting `tool_calls[].id` produces `"id":""`.
- **fix**: apply the same `toolu_` fallback in `openAIToAnthropic`.

### L16. `ZEN_PASSTHROUGH` forwards the client's full credential headers to zen
- **file:line** `anthropic.go:2034-2041`
- **issue**: the clone keeps `authorization`, `x-api-key`, `cookie` and `x-session-id`, all of which AGENTS.md §7 forbids from reaching an upstream. Opt-in debug behaviour, which is why this is LOW, but the env var is one toggle away and there is no deny-list behind it.
- **scenario**: operator sets `ZEN_PASSTHROUGH=1` while debugging with a real Claude Code client → the client's `Authorization: Bearer sk-ant-...` is transmitted to `opencode.ai`.
- **fix**: run `zenHeaderForwarded`'s deny-list over the cloned headers first.

### L17. `ZEN_CAPTURE` writes the client's credential headers to disk in plaintext
- **file:line** `anthropic.go:2084` (with `:2016-2026`)
- **issue**: `zenDebugCapture` marshals the map and `os.WriteFile`s it at mode `0644` into `$ZEN_CAPTURE`. `r.Header` is the raw inbound header set.
- **scenario**: the debug workflow leaves `in-00007.json` containing the operator's `Authorization` header in a world-readable file that outlives the session.
- **fix**: redact `authorization`/`x-api-key`/`cookie` from the captured header map.

---

## INFO

### I1. Nudge streak map is wiped wholesale at 8192 entries
- **file:line** `anthropic.go:1496-1498`
- **issue**: `if len(s.n) > 8192 { s.n = map[string]int{} }` drops every tracked session, not just the one that pushed the map over. A client cycling session ids resets the streak cap for all concurrent sessions. Memory stays bounded either way, so the only practical effect is on the nudge heuristic.
- **fix**: evict a single entry instead of clearing the map.

---

## Files covered

_(appended as each audit wave reports)_

---

## Config drift: `config.yml.example` vs `appConfig`

**Keys in `config.go` with no leaf in `config.yml.example`** — undocumented, and `mergeMissingKeys` can never surface them because it diffs against the example:

| key | struct | `defaultConfig()` | effective default | effect |
|---|---|---|---|---|
| `zen.convo_idle_minutes` | config.go:159 | `1` | 1 min | sweep for convo-hash lanes; read at zenlanes.go:416 |
| `zen.hedge_after_ms` | config.go:191 | `0` | 2 s (zenproxy.go:565) | hedge a second request on another exit; `-1` disables |
| `freepi.queue_wait_secs` | config.go:230 | `0` | 60 s (freepi.go:48) | wait for a busy account to free up |

**Keys in the example with no field in `appConfig`:**

| key | note |
|---|---|
| `freebuff.*` (example:281-291, commented) | no Go struct, no code path — see M16 / H1 |

**Default-value disagreements:**

| key | `defaultConfig()` | example | agrees? |
|---|---|---|---|
| `anonymize.ner.model_path` | `""` (config.go:342 sets only `TimeoutMs`, `MinScore`) | `"pii/model/model_q4.onnx"` (example:114) | no — benign only because a fresh `config.yml` is seeded from the example; an existing config missing the key gets `""` |
| `freepi.antseed… prefer_free` | nil `*bool` → true (antseed.go:71) | `true` (example:321) | yes, effect identical |
| `anonymize.disclose_text` | full notice string (config.go:335) | `""` (example:97) | yes — `pii/pii.go:234-237` substitutes the same string |
| `translate.restore_tool_name_case` | nil `*bool` → true (config.go:102-107) | `true` (example:146) | yes |
| `zen.max_retries` | `4` (config.go:375) | `4` | value yes; **comment no** — see L12 |

**Presence-check coverage for default-true bools** (AGENTS.md rule): `nvidia.enabled`, `zen.enabled`, `usage.enabled`, `status.show_usage`, `status.show_pool`, `guardrails.{threshold,window_weight,density_weight}` all have raw-doc presence checks at config.go:431-482. `status.show_antseed` defaults false and needs none.

**`guardrails.json` vs its loader:** consistent. All 456 entries carry the `guardrail` key the loader reads (`config.go:746`); `source`/`locations`/`count` are ignored by the struct. Parsed in `applyConfig` on startup and on config mtime change (polled every 5 s, main.go:34) — 289 KB once per reload is not a hot path. The gap is silent failure (M10), not schema.

**`model_info.jsonc` vs `modelinfo.go`:** consistent. Parses cleanly through `stripJSONComments` (108 entries); no value contains `//` so the stripper cannot corrupt one; `"reasoning":null` on `opencode/jev-1.13-free` is handled correctly by the `*bool` field. Two dead fields: `freepi_source` appears on all 7 `freeepi/*` entries but has no struct field and is silently discarded; `reasoning_budget` (modelinfo.go:28, handled in `modelDefaultEffort`) is used by zero entries.
