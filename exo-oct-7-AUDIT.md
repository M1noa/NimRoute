# exo-oct-7 audit — NimRoute

read-only audit, 2026-10-07. no source files touched. nothing below is fixed.
conventions: `file:line` = issue (one sentence). fix = suggested fix (one sentence).
line numbers are against the working tree at audit time (tree had uncommitted changes).

## critical

- `main.go:2767` — `ServeHTTP` sets `Access-Control-Allow-Origin/Headers/Methods: *` on every response, so any website can spend this proxy from a browser. scope CORS to configured origins or require tokens for cross-origin calls.
- `main.go:4157` — `captureRequest` (gated by `ZEN_CAPTURE=1`) records `r.Header` including `Authorization`/`x-api-key`/`cookie` to world-readable `/tmp/oc-capture` (`MkdirAll 0755`, `WriteFile 0644`). redact credential headers and write `0600`.
- `main.go:4157` — same function does uncapped `io.ReadAll(r.Body)` on a debug path. cap with `io.LimitReader` at `bodyLimit`.
- `rtkcompress.go:122` — `setTextParts` does unchecked `pm["text"].(string)`; a non-string text part panics and crashes the proxy. guard with comma-ok and return false.
- `antseedp2p.go:769` — `mustHex(ack.Pub)` panics on seller-controlled hex, one malicious peer crashes the proxy. validate and return error instead of panicking.
- `anthropic.go:2384` — `ZEN_PASSTHROUGH` clones the client header map verbatim, forwarding client `Authorization`/`Cookie` upstream. strip credential headers even in passthrough mode.
- `piiwiring.go:50` — with empty `auth.tokens` every request maps to `sess:default`, sharing one PII vault across all clients so restores cross-talk. derive per-client sessions or bypass masking without identity.
- `pii/scan.go:172` — `restoreText` does global `ReplaceAll` of every surrogate, so genuine upstream text matching a surrogate becomes another user's original. record masked ranges at mask time, restore only those.
- `pii/pii.go:172` — `MaskBody` fail-open returns the fully unmasked body when masking breaks json, silently. emit a log/metric on fallback and retry narrower scope before shipping unmasked.
- `pii/custom.go:114` — duplicate lines anchor via `strings.Index(text, line)` (first occurrence), masking the wrong bytes while real PII later leaks. split with running offsets instead of re-searching.
- `config.yml:284` — live freepi JWT in plaintext on disk (file is gitignored, not committed). rotate the JWT, `chmod 600`, never copy it elsewhere.

## high

- `main.go:3008` — NIM retry loop builds upstream requests with `http.NewRequest` (no context), client disconnect never cancels upstream work. use `http.NewRequestWithContext(r.Context(), ...)`.
- `main.go:3150`, `main.go:3589`, `freepi.go:666` — NIM, zen, and freepi paths copy all upstream response headers to the client, including `Content-Length`/`Content-Encoding`/`Set-Cookie` after body transforms. forward an allowlisted subset only.
- `anthropic.go:663` — unparseable tool arguments wrapped as `{"_raw": string}`, inventing a client-visible field. emit `{}` and surface the parse error instead.
- `main.go:895` — `convertToResponses` reads tool content with `out, _ := mm["content"].(string)`, turning array tool outputs into `""`. flatten array content parts like `scanChatJSON` does.
- `zenproxy.go:504,510` — `dropProxy` logs raw `proxyURL`, which may embed `user:pass`. wrap with `redactProxyUserinfo`.
- `zenlanes.go:960` — `laneEscalate` logs the fresh proxy URL unredacted. wrap with `redactProxyUserinfo`.
- `scripts/freebuff_login.py:76` — managed-block merge opens config in append mode then writes the full old text, duplicating the file every login. rewrite with a single atomic write (tmp + rename).
- `scripts/opencode_prompt.py:16` — `PROMPT_FILE` points at `scripts/system-prompts/...` which does not exist, script always crashes. point at repo-root `system-prompts/...` and handle missing file.
- `.github/workflows/build.yml:33` — CI pins `go-version: "1.22"` while `go.mod` requires `go 1.26.0`. set `go-version: "1.26"`.
- `nim:63` — `svc_stop` runs `pkill -f "nim-proxy$"` / `pkill -f "nimroute$"`, can kill unrelated processes. kill only the recorded pid plus the launchd label.
- `antseedp2p.go:298` — `antReadFrame` honors up to a 64MB length prefix from 4 peer bytes, letting a malicious seller force large allocs. cap reads at `antMaxPlain` + overhead, reject larger.
- `antseedp2p.go:760` — enc-ack timestamp/nonce never checked for freshness or reuse, replayed handshakes accepted. enforce a clock-skew window and cache seen nonces per peer.
- `antseedp2p.go:1069` — `answerNeedAuth` adds seller-supplied in/out counters to signed cumulative usage with unchecked parse, seller inflates what you sign. validate counters against actual sizes before signing.

## medium

- `main.go:3016` — `X-Forwarded-For` set before the forward-header copy loop, client-supplied value overwrites the proxy value. set after the loop or deny-list it.
- `main.go:3017` — client `X-Forwarded-Host` forwarded to NIM while the zen path drops it. drop it in the NIM path too.
- `main.go:3059`, `main.go:487` — `strings.Contains(body, "ResourceExhausted")` on any status discards a 200 completion containing that word. gate the string match on non-200.
- `main.go:712` — `fetchZenModelIDsFrom` decodes upstream body with no status check and no size cap. check `StatusCode == 200`, decode through `io.LimitReader` (~1MB).
- `main.go:2012` — `handleModels` decodes NIM `/models` body uncapped on an unauthenticated endpoint. cap with `io.LimitReader`; consider caching so strangers can't burn key quota per request.
- `main.go:3317` — `ZEN_DUMP` writes full request/response bodies to `/tmp` with `0644`. use `0600` and note PII risk.
- `main.go:2834`, `anthropic.go:1905` — body-read watchdog goroutine closes `r.Body` on context-done with no sync against the handler's own close. remove the goroutine or guard with `sync.Once` / cancel after successful read.
- `main.go:2846` — `fmt.Sprintf("{\"error\":\"%s\"}", err)` embeds raw error text unescaped, invalid JSON plus internals reflected. marshal a struct with `json.Marshal`.
- `main.go:4185` — `strings.Split(string(rb), "\n")` on up to 128MB SSE bodies doubles peak memory. scan lines with `bufio` over bytes.
- `main.go:902` — `convertToResponses` tracks outstanding tool calls with an `openCalls` counter, extra outputs flush pending out of order. track outstanding `call_id` values instead.
- `main.go:978` — `convertToResponses` deletes client `tool_choice: none` while keeping injected gate stubs, letting the model call tools the client forbade. drop the gate stubs when the client asked for none.
- `main.go:1349` — `responsesToChat` copies only `output_text` parts, dropping refusal parts. carry refusal through or map to text.
- `main.go:1635` — `mergeChatJSON`'s `textOf` handles only string content, dropping array-part text on nudge merges. reuse the `scanChatJSON` array-flattening.
- `main.go:2456` — `ZEN_DUMP` bodies to `/tmp` with `0644` expose prompts/tool data on shared hosts. restrict perms and redact secrets, or remove the dump.
- `anthropic.go:207` — `top_k` parsed but `anthropicRequestToOpenAI` forwards only temperature/top_p/stop, silently dropping it. forward `top_k` or delete the field.
- `anthropic.go:330` — `convertUserMessage` forwards tool_result with empty `tool_use_id` as `""`. skip empty ids or preserve a correlation id.
- `anthropic.go:387` — thinking/redacted_thinking keeps only `Thinking`, drops `Signature` and the redacted type. preserve signature and redacted type instead of collapsing to `reasoning_content`.
- `anthropic.go:415` — `convertAssistantMessage` sets content nil when no text, invalid upstream. emit `""`.
- `anthropic.go:874` — `streamAnthropic` drops text deltas once `hasToolCalls`, losing trailing text in mixed turns. keep streaming text into the open block.
- `anthropic.go:1115` — `respStreamer.feed` opens calls with ItemID-or-ID fallback but closes by `evt.ItemID` only, empty-ItemID tools never close. use the same resolved id on close.
- `anthropic.go:1110` — `function_call_arguments` deltas for unknown item ids silently dropped. open the tool slot on demand or buffer the delta.
- `model_info.jsonc:110` — freeepi entries advertise reasoning/effort/image inputs while `freepi_declared_reasoning` is false and declared inputs are `["text"]`. gate advertisement on the `freepi_declared_*` fields.
- `modelinfo.go:65` — `stripJSONComments` truncates at the first `//` anywhere in a line, corrupting values containing it (no URLs in the file today, so latent). strip only full-line comments outside strings.
- `config.go:281` — `modelsConfig` has no `thinking` field, live `config.yml` `models.thinking` block silently ignored. add the field + wiring or delete the documented key.
- `config.go:335` — `defaultConfig` sets a long `DiscloseText` while the example documents `""`. set both to the same value.
- `antseed.go:467` — `antRoutes` reads `PreferFree` but both sort branches compare identically, knob does nothing. implement the documented bonus or remove the knob.
- `freepi.go:222` — `freepiAdCycle` (run via `go` at freepi.go:597,635) mutates `fa.turns`/`fa.lastAd` without `fa.mu` while other paths lock it. guard with the mutex or atomics.
- `freepi.go:548` — `freepiFetch` hot-loops claim-fail-retry with no backoff until the 60s deadline. add small backoff after retryable failures.
- `freepi.go:335` — `freepiCatalog` stamps `catalogAt` even on failed parse (mitigated only by the single-model fallback). update `catalogAt` only on successful parse.
- `zenlanes.go:669` — `convertSharedLocked` mutates the victim lane in place, in-flight holders see session/proxy/id change mid-request. allocate a new lane instead.
- `zenlanes.go:883` — `pickLaneProxyFor` snapshots `taken` then releases the lock, concurrent rotations can pin the same exit. reserve under `lanesMu` or re-validate in `laneReproxy`.
- `zenlanes.go:1281` — `laneFailover` doesn't stamp `lastUsed` on hand-out, concurrent retries converge on the same stalest lane. stamp it like `sharedLaneLocked` does.
- `pii/scan.go:278` — `noScanZones` matches skip strings by substring, user text matching any 24+ char system string is wrongly excluded. compute zone byte ranges during json traversal.
- `pii/pii.go:89` — session ids truncate to 128 chars for map keys, long ids with common prefix share guard + vault. hash the full session id for the key.
- `pii/scan.go:126` — paragraph split assumes `"\n\n"` with fixed +2 stride, misaligns offsets on `\r\n` or 3+ newlines. record each separator's real length.
- `pii/vault.go:219` — guards and vaults live in two independent LRU maps with no TTL sweep, eviction diverges and `VaultTTLHours` never bounds lifetime. unify into one session registry with TTL eviction.
- `pii/vault.go:136` — `SeedFixed` silently drops a second entity reusing one `Replacement`, it masks to a derived surrogate yet restores to the first entity's original. reject duplicate `Replacement` at config load.
- `pii/span.go:81` — `mergeSpans` unions partially overlapping spans, destroying one value on restore. keep the winning span's exact bounds.
- `pii/pii.go:95` — `ForRequest` returns cached guard on config change, `Ver` hardcoded `"v1"`, new detectors never apply to live sessions. version the cache key on detector config.
- `pii/ner_ort.go:61` — `nerSingleton` pins first caller's model/timeout via `sync.Once`, later config ignored. key on (modelPath, ortLib) or rebuild on change.
- `pii/ner_ort.go:212` — result tensor picked via random go-map iteration order, can misread logits. select the logits tensor by name + shape.
- `pii/ner_ort.go:134` — timed-out infer goroutines keep running behind `e.mu`, a leaked goroutine per timeout plus mutex convoy. check abandonment or use a cancellable pool.
- `pii/ner_ort.go:283` — offsets from lowercased text slice the raw body, misaligns/panics on length-changing lowercases (e.g. U+0130). fold without changing byte length or bounds-check.
- `pii/detect.go:77` — digit entities need exact whole-run lengths, SSN glued to extra digit and 13/19-digit PANs bypass. slide windows inside longer runs; add 13/19-digit card lengths.
- `pii/detect.go:109` — IBAN regex has no boundaries, fires on base64/ids, each false hit risks fail-open passthrough. anchor with boundaries + mod-97 check.
- `pii/detect.go:107` — JWT regex demands two `eyJ` segments, misses JWE/non-claims/padded tokens. broaden to general three-segment base64url shape.
- `pii/detect.go:105` — no IPv6 recognizer, IPv6 literals pass through. add an IPv6 pattern.
- `pii/detect.go:110` — first phone rule needs separators (bare 10-digit bypasses), second overfires on `+` math. add bounded bare-digit rule with area-code guards; tighten the plus rule.
- `pii/detect.go:135` — secret regexes cover only nvapi/gh-op/sk, miss `ghr_`/`ghs_`/`AKIA`/slack/`github_pat`. add those independent of the keyword gate.
- `pii/detect.go:161` — generic-key matching gated per-paragraph, keyword and secret in adjacent paragraphs bypass. run the gate over a sliding window or whole text.
- `pii/custom.go:85` — exact custom matching is case-sensitive, `"Minoa"` bypasses entity `"minoa"`. match case-insensitively with case-preserving restore.
- `pii/scan.go:227` — detector hit on top-level model id triggers full-body json remarshal with reordered keys. add the model value range to no-scan zones instead.
- `pii/scan.go:435` — `NewRevealer` snapshots max surrogate length once, later longer surrogates (uniquify `~n`) split across chunks and miss restore. recompute on vault growth or over-provision.
- `pii/scan.go:470` — `unsplit` copies the full surrogate set per chunk, O(surrogates x cut) per Push. index by first byte or trie.
- `pii/scan.go:172` — `restoreText` costs O(surrogates x body) ReplaceAll plus O(n^2) sort per response. prefilter candidates, sort once per vault version.
- `pii/wordpiece.go:477` — `piece()` backtracks rune-by-rune per position, quadratic on long CJK/`[UNK]` runs. cap word length or fast-path `[UNK]` runs.
- `pii/wordpiece.go:73` — `foldWithMap` allocates two int slices per folded byte, balloons on 10MB single-paragraph bodies. fold in windows matching the NER budget.
- `pii/fetch_model.sh:9` — curl `-sL` without `--fail` and no sha256 check, an HTTP error page becomes the model and NER silently degrades. add `-fSL` plus pinned sha256 verification.
- `pii/pii_test.go:10` — no tests for cross-session isolation, hot-reload staleness, expired-vault restore, duplicate-line anchoring, concurrent Mask/Restore under `-race`. add those regression tests.
- `pii/wordpiece_ref_test.go:10` — reference/NER tests skip without env (`TOK_REF`/`RAMPART_*`/`ORT_LIB`), CI never exercises them. vendor a tiny fixture corpus or gate CI on the env.
- `scripts/freebuff_login.py:33`, `scripts/freepi_login.py:93`, `scripts/freepi_probe.py:86` — hand-rolled argv parsing with off-by-slice indexing misreads flags/values. use stdlib `argparse`.
- `scripts/freepi_probe.py:83` — default `--jwt freepi_jwt.txt` disagrees with `freepi_login.py --into-config` (writes into `config.yml`), probe finds nothing after login. read JWT from `config.yml` or document the two-step.
- `scripts/probe_zen_proxies.py:134` — default `--out zen_probe.tsv` not in `.gitignore`, output gets committed by accident. default into `/tmp` or ignore the pattern.
- `.gitignore:1` — missing `freepi_jwt.txt`, `*.jwt`, `zen_probe.tsv`, `config.test.yml` scratch patterns, scratch binaries/usage files. add them.
- `guardrails.json:1` — root `guardrails.json` and `system-prompts/guardrails.json` differ (different md5), one is stale. delete the copy or document which `guardrails.file` loads.
- `config.yml:91,105,159,178` — live config drifts from `config.yml.example` with no comment: `zen.always_proxy: true` vs false, `max_response_ms 350`/`pool_size 1000` vs 250/400, `status:` missing `show_pool`/`show_usage` + whole `usage:` section, `models.nudge "*": true` vs commented-out. reconcile each and comment the chosen values.
- `deploy/com.user.nimroute.plist:15` — `KeepAlive true` with no `ThrottleInterval` respawns a crash-looping binary without pause. add `ThrottleInterval 10`.
- `nim:52` — nohup pidfile at world-writable `/tmp/nimroute.pid` with unchecked `kill $(cat ...)`. write under the proxy dir with 600 and validate contents.
- `.github/workflows/build.yml:36` — CI runs bare `go test ./...`, no `-tags ner` job, no `gofmt`/`go vet`/`govulncheck`; AGENTS.md gates unenforced. add those steps.
- `go.mod:7` — `onnxruntime-purego` pins a future-dated pseudo-version, not a release tag. pin a tagged release and verify the hash.
- `fuzz_test.go:11` — file named `fuzz_*` defines zero `Fuzz*` functions, no fuzzing ever runs. add `FuzzRtkCompressBody` or rename the file.
- `zen_schema_test.go:379` — `TestSanitizeZenSchemaCCRealTool` skips when `/tmp/failing_input_schema.json` absent, permanently green without coverage. check in a redacted fixture.
- `zen_schema_test.go:307` — `TestZenValidatorBoundaryLive` hits real `opencode.ai` on `ZEN_CALIBRATE=1` with no `testing.Short()` guard, burns free-tier quota. skip in `-short` too.
- `todo_small_test.go:349` — `TestSweepBurns` mutates global `burned` without save/restore, leaking state into other tests. snapshot/restore like neighboring tests.

## low

- `main.go:3109` — OK and non-OK bodies re-read from the buffered wrapper, two copies up to `upstreamBodyLimit`. reuse `rb`/`bodyCopy` directly.
- `main.go:3869` — `sanitizeZenSchemaDepth` defined but never called (anthropic agent: one-line wrapper around `sanitizeZenSchemaSegs`). delete it.
- `main.go:4307` — `fmt.Sscanf(e, "%d", &port)` ignores parse errors. use `strconv.Atoi` with fallback to config/default.
- `main.go:923` — tool names truncated `name[:64]` on bytes, splits UTF-8. truncate by runes.
- `anthropic.go:680` — unknown `finish_reason` (`content_filter`, `function_call`, null) falls through to `end_turn`. map them explicitly.
- `anthropic.go:1822` — `nudgeState.note` wipes the whole session map past 8192 entries, resetting active streak caps. evict one entry instead.
- `anthropic.go:2414` — `estimateTokens` divides raw JSON byte length by 4, base64 images and syntax inflate `count_tokens`. count decoded text only.
- `anthropic.go:516`, `anthropic.go:1460`, `anthropic.go:2283` — `toolResultText`, `isWaitOnlyChat`, `randBase62` have no callers. delete them.
- `anthropic.go:2313` — `rand.Read` error ignored, zero random suffix on failure. check the error and abort id minting.
- `anthropic.go:2519` — retry logs print full rotating `sessionID` while `/status` truncates via `partialSession`. log the truncated form.
- `antseed.go:379` — `antPoll` caps stats snapshot at 32MB, large for a poll loop. lower toward `upstreamBodyLimit` scale.
- `antseedp2p.go:524` — `antDialSocks5` splits host:port on `:` and ignores parse errors, breaks IPv6, dials port 0 on garbage. use `net.SplitHostPort` with error handling.
- `antseedp2p.go:446` — proxy userinfo parsed with naive splits, truncates passwords containing `@`/`:` . parse with `net/url`.
- `zenlanes.go:1141` — `resetStaleLanes` sets `zenSuccessAt = now` with no success, delaying the next stale-pool trip during ongoing failure. leave it untouched.
- `zenlanes.go:1234` — `redactProxyUserinfo` splits userinfo at the first `@`, leaking a password fragment when the password contains `@`. use `strings.LastIndex`.
- `zenlanes.go:714` — `cacheKeyOf` trims whitespace while lane identity is documented verbatim. drop the `TrimSpace` or document the `pck:` exception.
- `zenlanes.go:1250` — `laneSnapshot` exposes `l.id` derived from verbatim client `x-session-id`. hash or omit hdr-derived ids in `/status`.
- `zenlanes.go:127` — `exitTrack` entries never evicted as the pool churns hourly. delete on `dropProxy`/refresh.
- `zenproxy.go:64` — `zenLive` RTT entries never evicted for dropped exits. delete on `dropProxy`/refresh.
- `zenproxy.go:175` — proxy-list `Decode` reads an uncapped third-party body. cap with `io.LimitReader`.
- `zenproxy.go:835` — socks5 dial uses `pu.Host` and drops URL userinfo, credentialed socks5 exits never authenticate. add socks5 user/pass auth or reject loudly.
- `usage.go:126` — usage log opens `0644` world-readable while records carry key names and headers. open `0600`.
- `piiwiring.go:83` — `piiWriter.Write` returns `len(b)` regardless of downstream short writes/failures, violates `io.Writer`. return the actual count, propagate errors.
- `pii/custom.go:102` — fuzzy pass calls `fuzzyEligible("", sp)` ignoring entity Type, short phone/email values get name-style fuzzy matching. pass the entity type through.
- `pii/custom.go:98` — lines over 4096 bytes skip fuzzy entirely, typo'd names in single-line 10MB json bypass. document or run fuzzy on chunked windows.
- `pii/pii.go:128` — empty `KeepLabels` collapses to nil and silently keeps city/state/zip, no way to say mask-everything. distinguish unset (default keeps) from explicitly empty (mask all).
- `pii/vault.go:139` — `SeedFixed` entries get 100-year expiry outside the LRU, never expiring or counting to maxSize. give them normal TTL/LRU membership or a documented bound.
- `pii/vault.go:179` — `Surrogates()`/`MaxSurrogateLen()` include expired entries, streaming holdback and restore scan dead placeholders. filter by expiry.
- `pii/vault.go:169` — expired entries removed only when the same original is re-added, lingering until then. sweep expired entries during put/sweepIdle.
- `pii/vault.go:62` — `normOrig` lowercases/collapses whitespace, restore returns first-seen casing for later variants. store the per-request surface form or document canonicalization.
- `pii/vault.go:35` — `Vault.counts` increments but never read, `remove()` never decrements. delete the field or expose it.
- `pii/scan.go:415` — `restoreBody`'s json guard covers whole bodies only, `Revealer` restores chunk prefixes that can split UTF-8/escapes. cut only on rune/escape boundaries.
- `pii/wordpiece.go:421` — NFKD table covers Latin subsets only, non-Latin names fold to themselves and become `[UNK]`. document the gap or extend tables.
- `pii/wordpiece.go:97` — `decodeRune` skips continuation-byte validation, malformed UTF-8 mis-tokenizes into NER misses. validate, emit U+FFFD per spec.
- `pii/ner_ort.go:267` — a `B-` tag of the same category extends the open entity instead of starting a new one. start a new candidate on `B-`.
- `pii/ner_ort.go:186` — one global mutex serializes all NER inference process-wide. shard sessions or document the throughput ceiling.
- `pii/pii.go:30` — `Config.OnTimeout`, `Scanner.Timeout`, `loadNer` are dead: timeouts return nil with no heuristics fallback. wire `OnTimeout` or delete the knobs.
- `pii/pii.go:65` — `SessionKey` keeps 8 of 32 hash bytes (64-bit) for an identity key. keep at least 128 bits.
- `pii/fake.go:25` — URL fake pool has 5 domains, given names 40, distinct originals visibly collide before uniquify `~n`. enlarge pools or document the realism cliff.
- `piiwiring.go:70` — `piiWriter` stores `g` but never uses it. drop the field.
- `config.yml:201` — live `claude_map` routes haiku/opus differently than the example (haiku `freeepi/glm-5.3-flash` vs `big-pickle`, opus `space-bunny-free` vs `muse-spark`). sync the example or comment the override.
- `config.yml:19` — `auth.tokens` empty, an internet-reachable proxy stays open and burns freepi quota. set tokens or bind localhost.
- `deploy/nimroute.service:10` — `Restart=always` + `RestartSec=3` also restarts on clean stop, masks crashes. use `Restart=on-failure`.
- `nim:108` — `nim log`/`tail` assumes `nimroute.log` exists and errors when absent. test `-f` first, print a friendly message.
- `scripts/cctest.py:100` — capture replay forwards recorded headers including `authorization`, replaying чужой credentials. strip `authorization`/`x-api-key`/`cookie` before replay.
- `live_test.go:18` — `TestLive*` names imply live upstreams but all use local `httptest` servers. rename to `TestLocal*`/`TestChain*`.
- `main_test.go:2604` — conditional `t.Skip("no verified pool available...")` hides a real escalation failure. fail instead or log the pool state.
- `nimstatus.py:329` — `logs` prints raw log lines which can carry proxy userinfo/tokens when redaction fails. redact `://user:pass@` before printing.
- `TODO.md:206` — two sections both numbered `## 11`, AGENTS.md references items 9/10 by number. renumber the second to 12+ and shift the rest.
- `PLAN.md:20` — says `max_response_ms 400->250` but live config is 350 and example is 250. update to shipped values.
- `README.md:28` — advertises `NIM_AUTH`/`DEBUG=1` envs with no mention in `config.yml.example`. document them in the example header.

## verification notes (spot-checked, not just repeated)

- confirmed in-tree: CORS `*` (main.go:2767-2769), capture with auth headers + `0644` (main.go:4154-4179), unguarded `pm["text"].(string)` (rtkcompress.go:122; line 99 is guarded), `mustHex` panics (antseedp2p.go:659-665), raw `proxyURL` in drop logs (zenproxy.go:504,510), `piiwiring.go:50` `sess:default`, `restoreText` global ReplaceAll (pii/scan.go:172), fail-open `MaskBody` (pii/pii.go:172, deliberate anti-corruption fallback — needs the log/metric, not removal), duplicate-line `Index` anchoring (pii/custom.go:114), `ZEN_PASSTHROUGH` only strips hop-by-hop (anthropic.go:2384-2392), `_raw` invention (anthropic.go:663), `out, _ :=` tool content (main.go:895), `http.NewRequest` without context in NIM loop (main.go:3011), blind header copies (main.go:3157, freepi.go:666-668), `config.yml` gitignored (so the JWT never committed — rotation still warranted), `go 1.26.0` vs CI `1.22`, `pkill -f` in `nim`, `usage.go:126` `0644`, `sanitizeZenSchemaDepth` defined-once at main.go:3869.
- adjusted down from agent reports: `stripJSONComments` URL-corruption is latent (`model_info.jsonc` contains zero URLs today); `freepiCatalog` empty-cache is partly mitigated by the single-model fallback at freepi.go:331; `freepiAdCycle` race stands (called via `go` at freepi.go:597,635 without `fa.mu`).

## files covered

- root go: `main.go` (full, in sections), `anthropic.go` (full), `config.go` (full), `zenproxy.go` (full), `zenlanes.go` (full), `rtk.go` (full), `rtkcompress.go` (full), `rtkdetect.go` (full), `usage.go` + `usage_test.go` (full), `modelinfo.go` (full), `piiwiring.go` (full), `antseed.go` (full), `antseedp2p.go` (full), `freepi.go` (full), `antseed_test.go`, `audit_fixes_test.go`, `fidelity_test.go`, `fuzz_test.go`, `lane_tokens_test.go`, `live_test.go`, `regress_stream_nudge_test.go`, `stream_cancel_test.go`, `zen_schema_test.go`, `todo_small_test.go` (partial + grep), `main_test.go` (partial: helpers, live-gated block, tail, grep sweeps).
- `pii/`: `pii.go`, `scan.go`, `vault.go`, `detect.go`, `span.go`, `custom.go`, `fake.go`, `wordpiece.go`, `ner_ort.go`, `ner_stub.go`, `fetch_model.sh`, `pii_test.go`, `vault_test.go`, `wordpiece_ref_test.go`, `ner_ort_test.go` (all full).
- scripts: all 8 `scripts/*.py` (full). deploy: all 3 units (full). `.github/workflows/build.yml`, `go.mod`, `go.sum`, `.gitignore`, `keys.jsonc`, `config.yml` vs `config.yml.example` (diffed), `nim` (full), `nimstatus.py` (full), `README.md`, `TODO.md`, `PLAN.md`, `AGENTS.md`.
- `guardrails.json` + `system-prompts/guardrails.json`: hash-compared only (differ), not fully read (289KB each).

## gaps (not audited)

- `ANTSEED-DOCS.md`, `ANTSEED-PLAN.md`, `ANTSEED-TODO.md`, `FREEBUFF-DOCS.md`, `FREEBUFF-MODEL-TABLE.md`, `FREEBUFF-PLAN.md`, `FREEBUFF-TODO.md`, `FREEPI-DOCS.md`, `CHANGED-SINCE-682b2eb.md`, `bunny-oct-7-AUDIT.md`, `muse-oct-7-AUDIT.md`: docs, not read.
- `system-prompts/` tree (incl. `leaked-system-prompts/` submodule): not read.
- `pii/testdata/vocab.txt`: not read.
- `pii/wordpiece_ref_test.go` reference behavior beyond the skip-gate: not exercised (no fixtures).
- runtime artifacts (excluded by design): `nimroute` binary, `nim-usage.jsonl`, `*.log`, `.opencode/`, `.freebuff/`.
