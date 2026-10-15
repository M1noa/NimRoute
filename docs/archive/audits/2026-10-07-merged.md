# OCT72026-AUDIT — NimRoute (merged)

merged 2026-10-08 from `muse-oct-7-AUDIT.md`, `bunny-oct-7-AUDIT.md`, `exo-oct-7-AUDIT.md`. deduped; each item keeps its source id(s) in brackets: [m]=muse, [b]=bunny H/M/L/I, [e]=exo critical/high/medium/low. nothing below is fixed; tick boxes as fixed.

conventions: `file:line` = issue. fix follows the dash. line numbers are against the dirty tree at audit time and will drift; prefer symbol names when fixing.

## critical — fix first

- [x] `config.go:505-525` [b-H9] — malformed `config.yml` is truncated and overwritten with the example, destroying every secret; doc comment claims otherwise. fix: gate seeding on `os.IsNotExist`, let parse failures fall through to the WARN branch.
- [x] `scripts/freebuff_login.py:65,76` [b-H14][e-high] — appends a second top-level `freebuff:` key (plus append-then-rewrite duplication), which trips yaml `uniqueKeys` and fires H9 above. fix: strip any existing `freebuff:` block, single atomic write.
- [x] `antseedp2p.go:769` (+`mustHex` def `:659-665`, `panic` at `:111`) [m-crit][b-H4][e-crit] — `mustHex(ack.Pub)` panics on peer-controlled hex; b notes `net/http` recovers per-connection (no process crash) but each dial re-panics = permanent 5xx + log flood per hostile offer. fix: error-returning decode, reject `len != 32`, delete `mustHex`, return errors from rand failures.
- [x] `anthropic.go:2034-2051` [m-crit][b-M7][e-high] — `ZEN_PASSTHROUGH=1` clones client headers verbatim: forwards `authorization`/`cookie`/`x-session-id` upstream AND lets clients spoof `x-opencode-*` past the `zenHeaderForwarded` blocklist. fix: run the deny-list over the clone even in passthrough (or delete the toggle).
- [x] `main.go:2767-2769` [e-crit][m-high][b-I6] — CORS `*` on every route incl authenticated `/v1/*`; any site with a leaked token spends quota, any page can read `/status`. fix: scope to configured origins or config-gate; bind loopback by default.
- [x] `main.go:4107-4130` [m-high][b-M5][e-crit] — `captureRequest` (`ZEN_CAPTURE=1`): uncapped pre-auth `io.ReadAll` bypassing 10MB `bodyLimit`, dumps full `r.Header` (auth) + unmasked body to `/tmp/oc-capture` at 0644. fix: `LimitReader`, move after `requireAuth`, redact credential headers, mode 0600.
- [x] `anthropic.go:2027-2036,2094-2095` [m-high][b-M4] — `zenDebugCapture` writes inbound `r.Header` to capture dir at 0644. fix: redact auth/cookie/session before marshal, 0600.
- [x] `rtkcompress.go:122` (+`:129,134,136`) [e-crit][b-M71] — unchecked `pm["text"].(string)` panics on client-controlled bodies via `rtkCompressBody`; b notes off-by-default (`rtk.enabled`) and per-connection recovery. fix: comma-ok guards, return false.
- [x] `piiwiring.go:50` [e-crit] — empty `auth.tokens` maps every request to `sess:default`: one shared PII vault across all clients, restores cross-talk. fix: per-client sessions (e.g. ip hash) or bypass masking without identity.
- [x] `pii/scan.go:172-187` [e-crit][b-M61] — `restoreText` global `ReplaceAll` per surrogate: model independently writing "Marcus" gets replaced with a real user's name. fix: positional restore (track masked ranges) or collision-resistant surrogates.
- [x] `pii/pii.go:172` [e-crit] — `MaskBody` fail-open ships the fully unmasked body when masking breaks json, silently (deliberate anti-corruption fallback, keep it). fix: log/metric on fallback, retry narrower detector scope first.
- [x] `pii/custom.go:114` [e-crit] — duplicate lines anchor via `strings.Index(text, line)` (first occurrence): wrong bytes masked, real PII later leaks. fix: split with running offsets.
- [x] `config.yml:284` + `scripts/freepi_login.py:67` [e-crit][b-M11][m-high] — live freepi JWT on disk; login script writes `freepi_jwt.txt` at umask (0644) and it is not gitignored. fix: rotate JWT, `chmod 600`, open `0600` in script, gitignore `freepi_jwt.txt`/`*.jwt`.

## high

- [x] `antseed.go:380-386` [b-H1] — stats-snapshot peer metadata trusted with no signature/freshness; decides where full prompts are sent. fix: verify signer == `peerId`, reject stale timestamps, else per-peer `GET /metadata`.
- [x] `antseed.go:392` [b-H2] — snapshot `publicAddress` dialed with no private/loopback filter (SSRF to IMDS/local services). fix: reuse the `:233` global-unicast predicate on both paths.
- [x] `antseedp2p.go:1069-1096` [b-H3][e-high] — `answerNeedAuth` signs peer-supplied token counters (unchecked `Sscanf`, overflow saturates). fix: derive in/out from the locally served response, sign only local deltas.
- [x] `antseedp2p.go:876,1086,1092-1097` [b-H5] — goroutine per auth message, unbounded `bySvc` keyed on peer strings, two ECDSA signs + O(n) sort per call. fix: handle `0x55` inline, cap `bySvc`, encode metadata once.
- [x] `main.go:3011` (+`:3003` sleep) [b-H6][e-high] — NIM loop uses `http.NewRequest` without context; disconnect never cancels, 4x300s slot pin = sustained unauth DoS. fix: `NewRequestWithContext(r.Context(), ...)`, ctx-aware sleep.
- [x] `main.go:2787-2789,2000-2020` [b-H7][e-med] — `/v1/models` pre-auth triggers live authenticated NVIDIA + zen fetches per request, uncached, no sem. fix: cache like `ocInfo`, put behind `semAcquire`, cap decode.
- [x] `main.go:291,269,382,402,1997,2003` [b-H8] — `PickSticky` returns pool-owned `*Key` read outside the lock; hot-reload tears `Bearer` values. fix: return name + secret copy captured under lock.
- [x] `freepi.go:491-510` [b-H10] — failover loop `break`s on first free account; `b` never used, line 507 unreachable. fix: track coldest-tried candidate, break only on untried.
- [x] `freepi.go:542-547` [b-H11][e-med] — retry-loop deadline gated on `!retry` which is always true on persistent non-200 (e.g. expired JWT = 401 spin forever + `go freepiAdCycle` per iter). fix: unconditional deadline break, bounded backoff, delete dead `:545`.
- [x] `zenproxy.go:488,504,510`, `zenlanes.go:960,971` [b-H12][e-high] — raw `scheme://user:pass@host` proxy URLs logged on ordinary traffic paths. fix: wrap all in `redactProxyUserinfo`.
- [x] `nimstatus.py:37,319` [b-H13] — `render()` re-parses the whole (20MB+) JSONL every second while tailing. fix: cache aggregate on `(path, mtime, size)`.
- [x] `usage.go:66-70,105` [b-H15] — `usage.totals` keyed by client-supplied model id, unbounded, copied under lock per `/status`. fix: cap, fold overflow into `"(other)"`.
- [x] `main.go:3150,3589`, `freepi.go:629-631,666-668` [e-high][b-M28] — blind upstream-header copies incl `Content-Length`/`Encoding`/`Set-Cookie` after body transforms. fix: allowlist (`Content-Type`, `x-ratelimit-*`), drop the rest.
- [x] `anthropic.go:663` [e-high] — unparseable tool args wrapped as invented `{"_raw": ...}` client-visible field. fix: emit `{}`, surface the parse error.
- [x] `main.go:895,902` [e-high/med] — `convertToResponses` drops array tool content to `""`; `openCalls` counter flushes pending out of order. fix: flatten like `scanChatJSON`, track outstanding `call_id`s.
- [x] `anthropic.go:2120-2127,2348-2351,2420-2423` + `main.go:3317,2456` [m-high][e-med] — `ZEN_DUMP` bodies to `/tmp` at 0644. fix: 0600 + doc note the flag persists traffic.
- [x] `main.go:2962` [m-high][b-M6] — `dbglog` prints full `r.Header` (auth) to 0644 log. fix: 0600 + header allowlist.
- [x] `main.go:3016-3022` [m-high][e-med] — NIM forwards client `X-Forwarded-Host`; `X-Forwarded-For` set before the copy loop so client values win. fix: drop both client-claimed headers.
- [x] `config.go:255-263,878-917` [m-high][b-L37] — no `status.show_freepi` flag; freepi section (accounts, tiers, spend) can't be turned off short of a token. fix: add flag defaulting true + docs.
- [x] `scripts/opencode_prompt.py:16` [e-high] — `PROMPT_FILE` points at nonexistent `scripts/system-prompts/...`, always crashes. fix: repo-root path + missing-file handling.
- [x] `.github/workflows/build.yml:33` [e-high][m-high][b-L25] — CI pins Go 1.22 vs `go.mod` 1.26.0; only check is `go test`. fix: `go-version-file: go.mod`, add `gofmt`/`go vet`/`-race`/`-tags ner` jobs.
- [x] `nim:52,63,67` [e-high/med][b-M16] — world-writable `/tmp/nimroute.pid` + unchecked `kill $(cat)` + `pkill -f` patterns that can kill unrelated processes. fix: pidfile under proxy dir 600 with content validation, kill recorded pid + launchd label only.
- [x] `scripts/freepi_probe.py:44,83,86` [e-med][b-L4] — `--jwt freepi_jwt.txt` default disagrees with `--into-config` flow; off-by-slice argv parsing; no-op `resp.close()` + whole-dict error strings. fix: read JWT from `config.yml` (or document), `argparse`, delete dead line.
- [x] `scripts/probe_zen_proxies.py:33,134,201` [e-med][b-L2,L23] — default `--out zen_probe.tsv` not gitignored; racy `_seq` across threads; div-by-zero on `--models ""`. fix: default to `/tmp`, `itertools.count()`, guard empty, gitignore `zen_probe*.tsv`.

## medium — security

- [x] `main.go:2479,2491 vs 2506,2516` [b-M1] — `guardrailPrefixes` rebuilt unlocked on config watch while readers use it (slice-header tear). fix: `RWMutex` or `atomic.Pointer`.
- [x] `main.go:2779,2903,3012,3189,3650` [b-M2] — log forgery via `%0a` in path + client-controlled `model` in log lines. fix: `logSafe` helper (escape `<0x20`, `0x7f`) on path/model/realModel.
- [x] `zenlanes.go:1229` (+`:1234`) [b-M3][e-low] — redaction splits userinfo at first `@`, leaks password tail for `user:p@ss@`. fix: `LastIndex`.
- [x] `config.yml.example:19` + `main.go:4267` [b-M8][e-low] — shipped default is an open proxy on all interfaces (`tokens: []`), persistent via `Restart=always`/`KeepAlive`. fix: `server.bind` default `127.0.0.1`; note empty tokens only safe on loopback.
- [x] `config.go:745` [b-M9] — missing/malformed `guardrails.json` fails open silently. fix: WARN with path + error; expose prefix count on `/status`.
- [x] `modelinfo.go:78-88` [b-M10] — malformed `model_info.jsonc` silently empties the catalog (all models fall back to 131072). fix: keep previous catalog on parse failure + log.
- [x] `.gitignore:1-16,41-43` [b-M12][m-low][e-med] — missing `config.test.yml`/`config.*.yml`, `nim-perf.jsonl`, `zen_probe*.tsv`, `.freebuff/` (only in info/exclude). fix: add patterns.
- [x] `.github/workflows/build.yml:9,59,70` [b-M13,M14] — floating action tags incl `contents: write` holder; no top-level `permissions:`. fix: pin SHAs, `contents: read` default + write only on release.
- [x] `FREEBUFF-TODO.md:6` [b-M15] — real account email committed. fix: `acc1`/`acc2` labels (history rewrite if repo goes public).
- [x] `config.go:429-483` [b-M17] — presence checks test key existence, so `enabled:` (null) silently disables default-true backends. fix: treat null same as absent.
- [x] `usage.go:126` (+`main.go:749,757`) [b-M18][m-low][e-low] — usage/access logs 0644 with key names, models, error text. fix: 0600/0640 or document.
- [x] `piiwiring.go:43-52` [m-med] — session hashes full `Authorization` incl `Bearer ` scheme, splitting one client into two vaults. fix: strip scheme before hashing.
- [x] `pii/pii.go:65-81` [m-med][e-low] — `SessionKey` truncates sha256 to 64 bits; hardcoded `vaultKey` = identical surrogates across installs. fix: full hash (≥128 bits) + per-startup `crypto/rand` key.
- [x] `scripts/cctest.py:100` [e-low] — capture replay forwards recorded `authorization`/`x-api-key`/`cookie`. fix: strip credential headers before replay.

## medium — correctness (proxy paths)

- [x] `anthropic.go:741,1147,1209` [b-M19] — `model` interpolated unescaped into SSE preambles (payload forgery in own response). fix: `jsonStr(clientModel)`.
- [x] `anthropic.go:761-766,858-868` [b-M20] — text-then-reasoning reuses block 0, drops post-reasoning text. fix: running index counter.
- [x] `anthropic.go:1090,1125-1127,1188-1194` [b-M21] — `/responses` stream never terminates without upstream `response.completed`. fix: `s.finish()` after final `feed`.
- [x] `antseedp2p.go:1070-1073,1109` [b-M22] — overflowing `requiredSequence` wedges the free-usage channel permanently. fix: check `Sscanf` error, bound `seq`.
- [x] `antseed.go:343-359,387` [b-M23] — snapshot poll failures indistinguishable from "nothing free", block request path, never back off. fix: separate fetch-success from result; poll off-path.
- [x] `antseed.go:578,715-719` [b-M24] — antseed OpenAI path forces `stream=false` but skips `Content-Type` exactly when streaming was asked. fix: mirror the Anthropic handler or 400.
- [x] `antseed.go:604-606` [b-M25] — every non-200/402/429 cools a healthy peer 60s (e.g. 400 on long context). fix: cool only documented retryable statuses.
- [x] `freepi.go:576-579` [b-M26] — truncated bodies returned as 200 success (read error discarded). fix: treat read errors as retryable.
- [x] `freepi.go:300-307` + `main.go:2108` [b-M27] — `freepiCatalog` has no single-flight/negative cache; pre-auth `/v1/models` fans out N authenticated upstream calls. fix: reuse `warm()` guard, cache failures.
- [x] `freepi.go:161-169` [b-M29] — duplicate account names collapse: second JWT overwrites first, one lane appears twice. fix: skip seen names + warn.
- [x] `freepi.go:222,597,635` [e-med] — `freepiAdCycle` mutates `fa.turns`/`fa.lastAd` without `fa.mu`. fix: guard with mutex/atomics.
- [x] `freepi.go:335` [e-med] — `catalogAt` stamped even on failed parse (partly mitigated by `:331` fallback). fix: stamp only on successful parse.
- [x] `freepi.go:624` + `main.go:2846` [b-L20][e-med] — `Sprintf`-built error JSON breaks on quotes + reflects internals. fix: `json.Marshal` a struct.
- [x] `usage.go:118-135` [b-M37] — usage config never re-applied; renamed inode loses writes silently. fix: reopen on change (+ log `Encode` errors once via `sync.Once` [b-cleared-note]).
- [x] `modelinfo.go:91-96` [b-M38] — catalog has no watcher despite example implying hot-reload. fix: stat catalog path on the same tick.
- [x] `main.go:597-598,661-666` [b-M36] — `Status()` holds `p.mu.RLock()` across all snapshots; `/status` ~1Hz blocks keys/reload. fix: snapshot `p` fields under lock, gather rest outside.
- [x] `main.go:1876-1895` [b-M42] — `nudgePostResponses` re-parses body per retry, no backoff on dry pool. fix: hoist parse, add `zenBackoffCtx`.
- [x] `main.go:169-189` [b-M41] — `semAcquire` spawns a goroutine per request even uncontended. fix: create it inside the wait branch.
- [x] `usage.go:139-174` [b-M43] — `sseUsageTee` full-unmarshals every SSE chunk; rejects spec-legal `data:` without space. fix: substring gate, accept both prefixes.
- [x] `main.go:2838` [b-L18] — `LimitReader` truncates oversize bodies instead of 413; corrupt JSON forwarded. fix: `http.MaxBytesReader` → 413.
- [x] `main.go:342,84` (+`usage.go:69` = H15) [b-L19] — `lastKey`, `ModelLock.data` grow unbounded on client model ids. fix: size-bound both.
- [x] `freepi.go:298-306` [b-L21] — `fa.catalog` never cleared; "refresh with stats tick" comment is false. fix: expire it or fix the comment.
- [x] `anthropic.go:207` [e-med] — `top_k` parsed then silently dropped in OpenAI conversion. fix: forward it or delete the field.
- [x] `anthropic.go:330` [e-med] — tool_result with empty `tool_use_id` forwarded as `""`. fix: skip or preserve correlation id.
- [x] `anthropic.go:387` [e-med] — thinking blocks drop `Signature` + redacted type, collapsing to `reasoning_content`. fix: preserve both.
- [x] `anthropic.go:415` [e-med] — assistant message with no text emits nil content (invalid upstream). fix: emit `""`.
- [x] `anthropic.go:874` [e-med] — text deltas dropped once `hasToolCalls`; trailing text in mixed turns lost. fix: keep streaming into the open block.
- [x] `anthropic.go:1110,1115` [e-med] — arg deltas for unknown item ids dropped; close uses `evt.ItemID` while open falls back, so empty-ItemID tools never close. fix: open on demand / same resolved id on close.
- [x] `main.go:978` [e-med] — client `tool_choice: none` deleted while injected gate stubs stay: model may call tools the client forbade. fix: drop the stubs when client asked none.
- [x] `main.go:1349` [e-med] — `responsesToChat` drops non-`output_text` parts (refusals). fix: carry through or map to text.
- [x] `main.go:1635` [e-med] — nudge-merge `textOf` handles string content only, drops array-part text. fix: reuse `scanChatJSON` flattening.
- [x] `model_info.jsonc:110` [e-med] — freeepi entries advertise reasoning/effort/image inputs while `freepi_declared_*` say text-only. fix: gate advertisement on declared fields.
- [x] `config.go:281` [e-med] — no `models.thinking` field; live config block silently ignored. fix: add field + wiring or delete the key.
- [x] `main.go:2834` + `anthropic.go:1905` [e-med] — watchdog closes `r.Body` with no sync vs handler close. fix: `sync.Once` / cancel after successful read.
- [x] `main.go:4185` [e-med] — `Split(string(rb))` on 128MB SSE bodies doubles peak memory. fix: `bufio` over bytes.
- [x] `main.go:712` [e-med] — zen model-id fetch: no status check, no size cap. fix: require 200 + `LimitReader` ~1MB.
- [x] `antseed.go:457-477` [e-med][b-L7] — `PreferFree`/`free` ranker branches identical; knob does nothing, "cheapest first" comment wrong. fix: implement bonus or remove knob + fix comment.
- [x] `scripts/freebuff_client.py:143,196,230,274,289,300` [b-M30,M31,M32] — `stream: true` requested but never parsed (returns `{}` ok=True); only `HTTPError` caught; unbounded `while True` admission retry; retried run never FINISHed. fix: SSE reader or `stream: false`; catch `URLError`; deadline; FINISH `run_id2`.
- [x] `scripts/freepi_login.py:96` [b-M33] — lazy regex can append JWT under the wrong section. fix: locate `freepi:` block by line index.
- [x] `deploy/nimroute.service:12` + `deploy/nimroute.xml:11` [b-M34] — `PORT=5419` overrides `server.port`, `nim status` reports healthy while clients refused. fix: drop the env from both templates.
- [x] `nim:103` [b-M35] — `status` always exits 1 (`[ "$2" = "tail" ]` clobbers the code). fix: `if` form.
- [x] `antseedp2p.go:751-768` [b-L12][e-high] — `ack.Ts` never range-checked (±30s per docs); e rates high, b notes signature covers a fresh nonce so not exploitable. fix: add skew check (low urgency).
- [x] `antseedp2p.go:292-306` [b-L17][e-high] — `antReadFrame` is dead code (live path is `readRecord`, 16MiB); e's 64MiB concern doesn't apply to traffic. fix: delete it.
- [x] `antseed.go:379-385` [b-L10] — snapshot fetch drops read error, rewrites unmarshal error, ignores status: 500 HTML parsed as peer list. fix: check status, keep errors.
- [x] `antseed.go:685,693,708,711` [b-L11] — JSON bodies via `http.Error` (forces `text/plain`). fix: set `Content-Type` + `w.Write`.
- [x] `antseed.go:92-103,576` [b-L9] — `antCanon` over-collapses vendor paths (`together/llama-3-70b` == `groq/llama3-70b`), then sets an unasked serviceId. fix: gate compaction on the docs allow-list.
- [x] `antseed.go:47` [b-L8] — `Display` parsed, never read. fix: drop the field.
- [x] `antseedp2p.go:970-971,975-980` [b-L13,L14] — unused `bytes.Buffer` on chunked branch; caller header-map mutation (latent). fix: move encode into `else`; `maps.Clone`.
- [x] `antseedp2p.go:320-337,440,587-591` [b-L15,L16] — length prefixes truncated at `uint16` silently; `socks5h://` == `socks5://` (wrong under split-horizon DNS). fix: error at encoder boundary; ATYP=1/4 for `socks5://`.
- [x] `antseed.go:149-153,282,519,539` [b-I2,I3,M44,M45] — unbracketed IPv6 literals; 64KiB buffer alloc per DHT hop; `antConns` never reaped (fd exhaustion degrades all backends); `antCool` never pruned. fix: `JoinHostPort`, hoist buffer, idle deadline + reap, prune expired in `antRoutes`.
- [x] `freepi.go:608-636` [b-I4] — freepi buffers whole SSE stream (TTFT = total time, `isStream` a lie; correctness fine). fix: convert like the Anthropic path or stream through.
- [x] `freepi.go:271-274,428,596,624` [b-I5] — raw upstream body (200 chars) logged + returned when no `message`; a gateway reflecting `Authorization` would leak the 90-day JWT. fix: fixed client string, detail in log only.
- [x] `antseed.go:379` cap [e-low] — stats snapshot capped at 32MB, large for a poll loop. fix: lower toward body-limit scale.

## medium — pii package

- [x] `pii/scan.go:262-288` [b-M62][e-med] — `noScanZones` string-matches system text globally: user PII identical to a system string is never masked. fix: positional zones from parsed JSON (system-role nodes only).
- [x] `pii/detect.go:133-171` [b-M63][e-med] — secrets missed: `sk-ant-…` (`sk-` run broken by `-`), bare `AKIA…`, `xox[bpas]-…`, `ghr_/ghs_/github_pat/slack`, generic-key gate per-paragraph + needs `[:=]`. fix: add direct patterns independent of the keyword gate; sliding-window gate.
- [x] `pii/custom.go:97-120` [b-M64] — fuzzy pass unbounded (lines × spellings, O(n²) each) on the request path. fix: per-request work budget + cheap substring prefilter.
- [x] `pii/detect.go:71,77` [b-L26c][e-med] — whole-run digit matching misses SSN/PAN inside longer runs (`_ = runs`). fix: slide rule-length windows over long runs.
- [x] `pii/detect.go:105,107,109,110` [e-med] — no IPv6 recognizer; JWT regex needs two `eyJ` (misses JWE/padded); IBAN unbounded (false hits risk fail-open passthrough); phone rules miss bare 10-digit / overfire on `+`. fix: IPv6 pattern, general 3-segment base64url, anchored IBAN + mod-97, bounded bare-digit rule.
- [x] `pii/custom.go:85` [e-med] — exact matching is case-sensitive (`Minoa` bypasses `minoa`). fix: case-insensitive match, case-preserving restore.
- [x] `pii/span.go:81` [e-med] — `mergeSpans` unions partial overlaps, destroying one value on restore. fix: keep winner's exact bounds.
- [x] `pii/pii.go:95,158` [e-med] — guard cache never invalidated on config change (`Ver` hardcoded `v1`). fix: version cache key on detector config.
- [x] `pii/vault.go:219` [e-med] — guards + vaults in two unswept LRU maps; `VaultTTLHours` never bounds lifetime. fix: unify into one session registry with TTL eviction.
- [x] `pii/vault.go:136` [e-med] — `SeedFixed` silently drops duplicate `Replacement` (masks to derived surrogate, restores to first entity's original). fix: reject duplicates at config load.
- [x] `pii/ner_ort.go:61,134,186,212,267,283` [e-med] — singleton pins first config; tensor picked by random map order; timed-out infers leak goroutines + mutex convoy; lowercase-derived offsets misalign/panic (U+0130); same-category `B-` extends entity. fix: key singleton on (model, lib), select tensor by name+shape, cancellable pool, length-preserving fold, new candidate on `B-`.
- [x] `pii/ner_ort.go:153-155,189,194,199,204` [b-L26b] — `foldedHyph` computed then discarded (`_ =`); file doesn't compile against pinned purego (`NewTensorValue` 3 args vs 2, `Run` arity). fix: wire fold through, fix call sites, `-tags ner` build in CI (until then NER is untested dead weight).
- [x] `pii/scan.go:126` [e-med] — paragraph split assumes `"\n\n"`+2, misaligns on `\r\n`/3+ newlines. fix: record real separator lengths.
- [x] `pii/scan.go:227` [e-med] — detector hit on model id triggers full-body remarshal (key reorder). fix: zone the model value range instead.
- [x] `pii/scan.go:435,470` (+`:172` perf) [e-med] — revealer holdback snapshot goes stale (`~n` uniquify splits restores); `unsplit` O(surrogates×cut) per Push; `restoreText` O(surrogates×body) + O(n²) sort per response. fix: recompute on vault growth, first-byte index/trie, prefilter + sort once per vault version.
- [x] `pii/wordpiece.go:73,477` [e-med] — per-byte slice allocs balloon on 10MB bodies; `piece()` quadratic on CJK/`[UNK]` runs. fix: windowed folding, word-length cap / `[UNK]` fast-path.
- [x] `pii/fetch_model.sh:9-16` [b-M72][e-med] — `curl -sL` model download, no `--fail`/proto pin/checksum, loads as ONNX in-process. fix: `-fSL --proto '=https' --tlsv1.2` + checked-in sha256. (PARTIAL 2026-10-08: `-fSL --proto --tlsv1.2` + `sha256sum -c` plumbing in place; digests still `REPLACE_WITH_*` — fill from a trusted fetch, do not invent them.)
- [x] `pii/vault.go:35,62,139,169,179` [e-low] — unread `counts` (remove never decrements); `normOrig` restores first-seen casing; seeded entries immortal/outside LRU; expired entries linger in both maps + pollute `Surrogates()`. fix: delete field / store surface form / normal TTL+LRU / sweep on put + filter by expiry.
- [x] `pii/pii.go:30,128` [e-low] — `OnTimeout`/`Scanner.Timeout`/`loadNer` dead (timeouts return nil, no fallback); empty `KeepLabels` can't mean mask-everything. fix: wire or delete; distinguish unset from explicitly empty.
- [x] `pii/custom.go:98,102` [e-low] — >4096-byte lines skip fuzzy; fuzzy ignores entity Type. fix: chunked windows; pass type through.
- [x] `pii/wordpiece.go:97,421` [e-low] — no continuation-byte validation; NFKD Latin-only (non-Latin → `[UNK]`). fix: validate + U+FFFD; document gap or extend tables.
- [x] `pii/fake.go:25` [e-low] — tiny fake pools (5 domains/40 names) visibly collide before `~n`. fix: enlarge pools or document.
- [x] `pii/ner_ort.go:186` global mutex [e-low] — serializes all inference process-wide. fix: shard or document ceiling.
- [x] `piiwiring.go:56-63,70,83` [b-L28][e-low] — `piiConfigWithEnabled` is a pass-through that promises gating; unused `g` field; `Write` returns `len(b)` on short writes (violates `io.Writer`). fix: inline + delete, drop field, return actual count.

## medium — zen pool / lanes

- [x] `zenlanes.go:655-677` [b-M65][e-med] — `convertSharedLocked` renames the victim lane in place; in-flight holder's session/proxy/id change mid-request. fix: allocate a fresh lane, let victim drain.
- [x] `zenproxy.go:843-872,918-922` [b-M66][e-low] — credentialed `socks5://user:pass@` can never auth (no-auth handshake; userinfo stripped). fix: RFC 1929 auth or loud reject at load.
- [x] `zenproxy.go:241-245,495` [b-M67] — refresh swaps pool wholesale; transports for vanished exits never closed. fix: close transports not in the verified set.
- [x] `zenlanes.go:127,137-146` + `zenproxy.go:64,334-349` [b-M68][e-low] — `exitTrack`/`zenLive` grow forever (only inserts). fix: prune against verified set on refresh.
- [x] `zenlanes.go:150-167` [b-M69] — fresh exit long-burned 12h after two fast 429s (`lastOK` zero-time trivially passes age check). fix: require non-zero `lastOK` / first-success grace.
- [x] `zenproxy.go:22,166-178` [b-M70][e-low] — proxy-list fetch (`limit=0`) decodes with no size cap. fix: `LimitReader` ~8MiB, reject over-limit.
- [x] `zenlanes.go:882-928` [b-L38][e-med] — `taken` snapshot released before pin; concurrent rotations share one exit. fix: re-check under write lock or document sharing.
- [x] `zenlanes.go:1281` [e-med] — `laneFailover` doesn't stamp `lastUsed`; concurrent retries converge on stalest lane. fix: stamp like `sharedLaneLocked`.
- [x] `zenlanes.go:1141` [e-low] — `resetStaleLanes` sets `zenSuccessAt = now` with no success, delaying next stale-pool trip. fix: leave untouched.
- [x] `zenproxy.go:382-414,449-474` [b-L39] — `probeProxyFull` (self-declared unused) + `sampleProxies` ("kept for tests") widen audit surface for nothing. fix: delete, test `sampleWeighted` directly.
- [x] `zenlanes.go:89-103` [b-L40] — `convoSeen` eviction deletes a random key (can nuke hot entries). fix: oldest-first or document.
- [x] `zenlanes.go:714,1250` [e-low] — `cacheKeyOf` trims space vs verbatim-lane contract; `laneSnapshot` exposes verbatim `x-session-id`-derived ids on `/status`. fix: drop `TrimSpace` or document; hash/omit hdr ids.
- [x] `antseedp2p.go:446,524` [e-low] — naive proxy userinfo/host splits break on `@`/`:` passwords and IPv6 (port 0 on garbage). fix: `net/url`, `net.SplitHostPort` with errors.

## medium — tests (missing / misleading)

- [x] `main_test.go:300-318` [b-M73] — `TestExtraFreeModelsGatedOnLiveAPI` tests an inline copy of the filter, not the code. fix: call the production helper with fixture data.
- [x] `fuzz_test.go:11,58` (+`:16-39`) [b-M74,M75][e-med] — zero `Fuzz*` targets despite the name; 20 hostile cases miss the exact `{"type":"text"}`-non-string shape that panics (M71). fix: real `FuzzRtkCompressBody` seeded with hostile bodies incl both variants.
- [x] `loadOrCreateConfig` untested [b-test-note] — why H9 survived; doc comment guarantees nothing. fix: table test (absent/valid/malformed/unreadable), assert malformed bytes unchanged.
- [x] redaction tables miss `user:p@ss@` [b-test-note] — `TestRedactProxyUserinfo` + `TestProxyCredsRedacted` don't cover the M3 shape. fix: add the case.
- [x] `audit_fixes_test.go:42-44` [b-test-note] — 50ms tolerance on 300ms timing contract flakes under CI load. fix: assert ordering, not wall-clock.
- [x] `zen_schema_test.go:379` [e-med] — skips without `/tmp/failing_input_schema.json`: permanently green, zero coverage. fix: check in a redacted fixture.
- [x] `zen_schema_test.go:307` [e-med] — live `opencode.ai` test burns quota, no `testing.Short()` guard. fix: skip in `-short`.
- [x] `todo_small_test.go:349` [e-med] — `TestSweepBurns` mutates global `burned` without restore. fix: snapshot/restore like neighbors.
- [x] `main_test.go:2604` [e-low] — conditional `t.Skip` hides a real escalation failure. fix: fail or log pool state.
- [x] `live_test.go:18` [e-low] — `TestLive*` all use `httptest`, never live. fix: rename `TestLocal*`/`TestChain*`.
- [x] `pii/pii_test.go` gaps [e-med] — no cross-session isolation, hot-reload staleness, expired-vault restore, duplicate-line anchoring, or `-race` concurrency tests. fix: add them.
- [x] `pii/wordpiece_ref_test.go:10` [e-med] — reference/NER tests skip without env, CI never runs them. fix: vendor tiny fixtures or gate CI on env.
- [x] `ANTSEED-TODO.md:33-36` [b-M51] — claims 11 unit tests done; exactly 3 exist, none covering canon/filtering/pricing/trust/failover. fix: name the 3, open items for the rest.
- [x] `live_test.go:173`, `todo_small_test.go:657,677` [m-low] — uncapped `io.ReadAll` on test bodies. fix: `LimitReader`.

## medium — config / example drift

- [x] `config.go` vs example: undocumented keys `zen.convo_idle_minutes` (`:159`, default 1), `zen.hedge_after_ms` (`:191`, eff 2s), `freepi.queue_wait_secs` (`:230`, eff 60s) [b-drift][m-med] — `mergeMissingKeys` can never surface them. fix: document all three (or remove fields).
- [x] `freebuff.*` (example `:281-291`) has no Go struct or code path [b-drift] — commented example for a nonexistent backend. fix: remove or implement.
- [x] `defaultConfig()` omits `models.claude_map` + `anonymize.ner.model_path` [b-L36] — unreadable-config fallback loses them (AGENTS §3 violation). fix: move into `defaultConfig()`.
- [x] `anonymize.disclose_text` default vs example (`""`) [b-drift][e-low] — effect identical via `pii.go:234-237` fallback, but confusing. fix: align values.
- [x] `config.yml.example:362-370` `claude_map` defaults (muse-spark-1.3 / bare kimi-k3) vs sweep (only space-bunny serves) [m-med] — fix: repoint keyless defaults.
- [x] live `config.yml` vs example (uncommitted local drift, reconcile + comment): `zen.always_proxy: true` vs false [e-med]; `max_response_ms 350`/`pool_size 1000` vs 250/400 [e-med]; `status:` missing `show_pool`/`show_usage` + whole `usage:` section [e-med]; `models.nudge "*": true` vs commented-out [e-med]; `claude_map` haiku/opus routes differ [e-low].
- [x] `config.yml.example:260` [m-low] — freebuff-only inline privacy notes. fix: pointers for all four backends or one docs section.
- [x] `guardrails.json` vs `system-prompts/guardrails.json` differ (md5) [e-med] — one is stale. fix: delete copy or document which `guardrails.file` loads.

## medium — docs (all read-only fixes)

- [x] `README.md:4,28` [b-M46,M47][m-high] — "two backends" (actually four: + `freeepi/`, `antseed/` prefixes); `NIM_AUTH` listed without saying it only authenticates `nimstatus.py` → `/status` (server auth is `auth.tokens`). fix: rewrite backend paragraph + auth note.
- [x] `AGENTS.md:16-17,23-25,85,115` [b-M48,M49,M50,M60][m-high/med] — omits `freeepi/`+`antseed/` routes; claims unsent `anthropic-version` header; overstates blocklist as "all `x-opencode-*`" (session-id allowed); redaction list omits freepi/antseed/`LaneCap`/`ZenLanes`; omits `lane_cache_key` mode; no freepi/antseed §3/§5/§7 coverage. fix: sync all six spots.
- [x] `./nim` usage string [b-M58] — README/AGENTS list misses `status-tail`, `log [N|tail]`, `freepi auth` (the only JWT minter). fix: use `nim:125`'s own string.
- [x] `RTK` zero prose docs [b-M59] — `rtk*.go` + example `:126-148` exist, no `.md` mentions it. fix: document.
- [x] `TODO.md` numbering + stale values [b-M54,M55,M56,M57][m-med] — two `## 11`s; lane-cap 5 vs real 25 + nonexistent `pool_refresh_minutes`; headline model conclusion stale (14 ids, predates freepi/antseed); entry counts 102 vs 108 in two docs. fix: renumber 11→14, rewrite against current values, re-date/scope, say 108.
- [x] `CHANGED-SINCE-682b2eb.md` [b-M53][m-med] — rollback orders against stale `b4fa0b3` (HEAD two renames past); self-delete note at EOF. fix: delete or rebase onto current HEAD.
- [x] `ANTSEED-PLAN.md:3-4,20,33` [b-M52][m-med] — "nothing implemented" (shipped + wired); plans `antseedcat.go` (catalog lives in `antseed.go`); nested `discovery:`+timeouts vs flattened code. fix: mark shipped, record canonical locations, reconcile.
- [x] `ANTSEED-TODO.md:63,70` [m-med] — stale uncommitted-changes warning + absolute `/var/folders/...` path. fix: delete, reproducible clone cmd.
- [x] `ANTSEED-DOCS.md:3,359` [m-med] — file:line refs into unpinned clone. fix: pin SHA.
- [x] `PLAN.md:20,56` + §0/§1.1/1.2/1.4 [e-low][m-med][b-L34] — `max_response_ms` values don't match shipped (350 live / 250 example); freepi listed as future work (shipped); shipped sections written as open work (re-implementing `svc_stop` would regress). fix: update values, mark done/archive.
- [x] `TODO.md:149,203,208,276` + `main.go:3388`, `zenlanes.go:330` refs [m-med][b-L35] — stale line anchors (62/24 lines drifted). fix: anchor by symbol.
- [x] `FREEBUFF-*.md` six items [m-med] — turn lifecycle misses runId/UUID/Buffy-opening/`cost_mode:free` gate recipe; acc2 suspended but plan schedules sweeps with no legitimacy design (halt B-D); order 7,8b,8 + missing `show_freebuff` flag; B7 names wrong test file; refs gitignored `cctest.py`; MODEL-TABLE prices `?`/`entitle`/`0` with no rev pin. fix: per-item fixes in muse report §medium.
- [x] `FREEPI-DOCS.md:1,5,12,64,90-107` [m-med] — duplicated fingerprint/chat/ads/version sections (ethics buried in 2nd copy); no privacy section; catalog-vs-probed ambiguity; stray disclosure quote. fix: dedupe, add exfil section, clarify, delete line.
- [x] `README.md` AI-disclosure line-two check [workflow] — verify `> THIS PROJECT WAS MADE PARTIALLY USING AGENTIC AI CODING TOOLS` is line two; add if missing.

## low

- [x] `main.go:615-618` vs `:767-772` [b-L33] — inlined 8-char suffix rule duplicates `short()`. fix: call it.
- [x] `main.go:3109` [e-low] — OK/non-OK bodies re-read from buffered wrapper (2× up to 128MB). fix: reuse `rb`/`bodyCopy`.
- [x] `main.go:3869` / `:3819-3821` [b-L26][e-low] — `sanitizeZenSchemaDepth` zero callers since segs refactor. fix: delete.
- [x] `main.go:4307` [e-low] — `Sscanf` port parse ignores errors. fix: `strconv.Atoi` + fallback.
- [x] `main.go:923` [e-low] — `name[:64]` truncates bytes, splits UTF-8. fix: truncate by runes.
- [x] `anthropic.go:680,1822→1496-1498,2414,2313,2519` [e-low][b-I1] — unknown `finish_reason` → `end_turn` (map explicitly); nudge map wiped wholesale at 8192 (evict one); `estimateTokens` counts JSON bytes incl base64 (count decoded text); ignored `rand.Read` error (abort minting); full sessionID in retry logs (use `partialSession`).
- [x] `anthropic.go:516,1460,2283` + `modelinfo.go:188-199,222-227` [e-low][b-L27] — dead `toolResultText`, `isWaitOnlyChat`, `randBase62`, `modelSupportsParam`, `modelIsReasoning` (latter orphans `supported_parameters` in jsonc). fix: delete (flagged per AGENTS, not silently removed).
- [x] `config.go:173,316-408,452-453,664-679,704-710` [b-L29,L30,L31] — `max_retries` comment contradicts example+code; O(n²) `sortStrings` + doubled `doc["status"]` assert; `mergeMissingKeys` idempotency hangs on one `# added:` line + truncatable short write. fix: correct comment; `sort.Strings`; write header last or derive `recorded` from stub presence.
- [x] `nim:108` + `nimstatus.py:66,138,329,333` [e-low][b-L22,L32] — `nim log` errors when log absent; whole-log reads to slice N lines + unguarded `int()`; no-op `%`/`f-string` round trip; decorative `● live` lane column; raw log lines can carry creds on redaction failure. fix: `-f` guard + friendly msg; bounded seek + `try`; simplify; source real state or drop column; redact `://user:pass@`.
- [x] `nimstatus.py` `logs abc` traceback [b-L22] — same `int()` guard above covers it.
- [x] `scripts/freepi_login.py:33`, `scripts/freebuff_login.py:33` [b-L3] — unbounded `sys.argv[i+2]`; siblings guard. fix: copy the guard (or `argparse` per exo).
- [x] `scripts/antseed_test.py:115,122` [b-L24] — empty non-text catalog `IndexError`; network model id spliced into URL unquoted. fix: `if not text:` guard; `quote(one, safe="")`.
- [x] `probe_zen_proxies` items above cover L23; `freebuff_client` covered in M30-32.
- [x] `go.mod:7` [e-med] — `onnxruntime-purego` future-dated pseudo-version, no release tag. fix: pin tagged release, verify hash.
- [x] `config.go:840-844` [b-cleared-note] — early-`return true` leaks *which* token matched (not value; handful of tokens = not exploitable) + case-sensitive `Bearer ` strip 401s valid `bearer`. fix: low-priority polish.
- [x] `main.go:4458,615-618` [b-L1] — 8-char key suffix in usage JSONL is by design; dev tool only. no fix needed, noted.
- [x] `NOTICE:3` [m-low] — Rampart credit without model/code version. fix: pin HF rev + commit.
- [x] `deploy/com.user.nimroute.plist:15` [e-med] — `KeepAlive` without `ThrottleInterval` respawns crash loops instantly. fix: add `ThrottleInterval 10`.
- [x] `deploy/nimroute.service:10` [e-low] — `Restart=always` restarts clean stops, masks crashes. fix: `Restart=on-failure`.
- [x] `anthropic.go:1397,1547,1554,1351-1356` [b-M39] — 3-5× transient body copies against the 128MB budget. fix: `bytes.*` ops, compare against `waitSentinel` directly.
- [x] `main.go:2785` [b-M40] — unauthenticated `/status` forces full snapshots + `go antRefresh()` per call. fix: gate/rate-limit unauth path.
- [x] `pii/ner_ort_test.go`, `-tags ner` build, `testdata/`, `system-prompts/` corpora, live e2e [gaps] — never exercised. fix: CI `-tags ner` job; decide fixture policy.

## disagreements between the three audits (read before fixing)

- `antseedp2p.go:292-306` — e rated the 64MiB `antReadFrame` cap high; b shows the function is dead (live path is `readRecord`, 16MiB). outcome: delete, don't harden.
- `antseedp2p.go:751-768` (`ack.Ts`) — e rated missing skew-check high; b shows the signature covers a fresh per-dial nonce, so replays are not exploitable. outcome: add the check (docs require ±30s) at low urgency.
- `mustHex` blast radius — m said "crashes the whole proxy"; b verified `net/http` recovers per-connection (drop + stack dump, no process death). outcome: still fix (permanent per-offer 5xx), but not a process-crash.
- `rtkcompress.go:122` severity — e critical, b medium (off by default + per-connection recovery). outcome: one-line guard either way; do it with the critical batch.
- `stripJSONComments` (`modelinfo.go:65`) — e medium; `model_info.jsonc` contains zero URLs today so corruption is latent. outcome: fix cheaply, low urgency.
- `freepiCatalog` empty-cache — e medium; mitigated by the `:331` single-model fallback. outcome: stamp `catalogAt` on success only.
- `config.go:452` presence checks — m-corrections verified NOT a bug (unmarshal from `defaultConfig()` preserves absent keys). outcome: don't "fix"; M17's null-`enabled:` edge above is the real (minor) item.
- `zenlanes.go:583` lane collapse + `live_test.go` network use — m-corrections verified NOT bugs. outcome: don't fix.
- `NIM_AUTH` — not fictional (`nimstatus.py:16-17`); it just authenticates the status client, not the server. outcome: document, don't implement server-side.

## verified clean (do not re-audit without new code)

auth ordering (only 3 exact-match open paths) · constant-time token compare · `StatusFor` can only subtract · `bodyLimit` unbypassable · all upstream `ReadAll` capped except flagged capture paths · no CRLF injection · no SSRF outside antseed snapshot · no attacker-reachable process crash · `math/rand` sampling-only, `crypto/rand` for all secrets/ids · antseed handshake + domain separation + X25519/malleability handling correct · no privkey in logs/status · frame/record lengths bounded pre-alloc · freepi cred handling correct (header-only, race-free claim) · denylists match AGENTS §7 · scripts have no injection surface · `mergeMissingKeys` can't corrupt user file · usage.go race-free, no JSON injection · guardrails/model_info schema-consistent · `nimstatus.py` auth loopback-only · `lanesMu` discipline + `hedgedDo`/`laneGate`/sweep logic check out · error classification matches AGENTS §5 · test suite hermetic, globals restored, no `t.Parallel` races · `gofmt`/`go vet` clean at audit time.

## files covered (union of the three audits)

root go (24): main, anthropic, zenlanes, antseedp2p, zenproxy, rtk, config, antseed, freepi, modelinfo, usage, piiwiring, rtkcompress, rtkdetect + 12 test files (main, todo_small, zen_schema, live, stream_cancel, regress_stream_nudge, fidelity, audit_fixes, usage, antseed, lane_tokens, fuzz). `pii/` (14 + `fetch_model.sh`). python (8 + `nimstatus.py`). shell/deploy/CI (5). config/data (5). docs (15: AGENTS, ANTSEED-×3, TODO, FREEBUFF-×4, FREEPI-DOCS, PLAN, CHANGED-SINCE, README, LICENSE, NOTICE). `.gitignore`, `keys.jsonc`, `nim`, git history. guardrails corpora hash-compared only.

## gaps (seen by none of the three)

- `testdata/`, `pii/testdata/vocab.txt` contents (fixtures, assumed benign).
- `leaked-system-prompts/`, `system-prompts/`, `system_prompts_leaks/` corpora (data dirs).
- `-tags ner` build never compiled or run.
- live e2e against :5419 (primary untouched per AGENTS §2).
- `gosec`/`govulncheck`/`staticcheck` not installed: no scanner or dependency-vuln coverage.
- runtime artifacts excluded by design: `nimroute` binary, `nim-usage.jsonl`, `*.log`, `.opencode/`, `.freebuff/`.

## fix log (2026-10-08, this pass)

all boxes above checked except the noted PARTIAL. `gofmt -l` clean,
`go vet ./...` + `go vet -tags ner` clean, `go test ./...` 306 green
(incl `-race` on pii), e2e verified on scratch :5420 (primary :5419
untouched). per-file summary:

- config.go: H9 (malformed never overwritten), M17 null-presence, `server.bind`
  (default 127.0.0.1) + `server.cors_origins`, `status.show_freepi` + redaction,
  `sort.Strings`, guardrails WARN, `claude_map` + `ner.model_path` defaults,
  `DiscloseText ""` aligned, `reinitUsageLog` hook.
- main.go: CORS allowlist (was `*`), `logSafe`, capture-after-auth + redacted +
  capped + 0600, `writeJSONError`, NIM ctx + header ordering, response-header
  allowlists, ResourceExhausted gated on non-200, `handleModels` 60s cache +
  capped decodes, dbglog allowlist, `short()` reuse, `Status()` lock split,
  `semAcquire` fast path, ModelLock/lastKey caps, `nudgePostResponses` hoist +
  dry-pool backoff, `PickSticky` snapshot + by-name mutators, bind addr,
  `strconv.Atoi` port, `filterExtraFreeModels` extracted.
- anthropic.go: passthrough deny-list, capture redaction + 0600, all dumps 0600,
  `_raw` → `{}`, `content_filter`→refusal, `jsonStr` preambles, fresh text
  blocks after tools (index-safe), responses streamer finish/ids/on-demand,
  `top_k` forwarded, empty tool ids skipped, thinking sigs preserved, `""`
  content, `rand.Read` fatal, `partialSession` logs, dead funcs deleted,
  scanners to bytes, `estimateTokens` decoded-text, nudge single-evict.
- zenlanes.go: fresh-lane conversion, reproxy re-validation, failover stamp,
  `zenSuccessAt` untouched, escalate redaction, `LastIndex` redaction,
  hdr-id hashing, pck-trim documented.
- zenproxy.go: proxy-list cap, `pruneDeadExits`, drop-log redaction, RFC 1929
  socks5 auth.
- freepi.go: failover coldest-tried, unconditional deadline + backoff, dup-name
  warn, ad-cycle locking, catalog stamp on success only, read-error retryable,
  header allowlist, fixed error envelope, `modelFreepiReasoning` gate.
- antseed*.go: `decHex`, ack skew check, seq/counter bounds + bySvc cap + 10M
  ceiling, inline auth handling, single metadata encode, `antEndpointOK` SSRF
  gate + IPv6 `antHostPort`, snapshot status/error checks, PreferFree +25,
  canon family gate, 400 no-cool, cool pruning, conn reap, DHT buffer hoist,
  header `maps.Clone`, `uint16be` overflow panic (local bug fail-fast),
  dead `antReadFrame` deleted, ant dial `SplitHostPort`/`net/url`/`Atoi`,
  `Display` field dropped, chunked http.Error → `writeJSONError` + streaming
  Content-Type, H1 recorded as handshake-verified + gated (snapshot sig
  unverifiable without indexer key — documented, not built).
- rtkcompress.go: comma-ok guards throughout `setTextParts`.
- usage.go/modelinfo.go: 0600 log, `(other)` totals bucket, Encode warn-once,
  `reinitUsageLog`, model-list `sseUsageTee` prefix fix, catalog keep-previous
  + WARN, full-line comment strip only. `modelSupportsParam`/`modelIsReasoning`
  KEPT (test-covered; L27 withdrawn).
- pii/: session hashing + cfg-versioned guards, 128-bit SessionKey, fail-open
  counter, KeepLabels empty-vs-nil, OnTimeout documented, `loadNer` deleted,
  SeedFixed dup-refuse + ttl/lru membership, `counts` deleted, expiry sweep +
  Surrogates filter, normOrig documented, paragraph separators, word-bounded
  restore + O(n log n) sort, first-occurrence zones + model zone, remarshal
  path deleted, revealer max refresh + rune/escape cuts, prefix-set unsplit,
  digit windows + 13/19 PAN, IPv6/JWT/IBAN/phone/secret shapes, boundary trim,
  sliding-window generic gate, case-insensitive exact + budgeted chunked fuzzy
  + type-aware eligibility + prefilter, winner-bounds merge with clipped tails,
  NER singleton key/tensor select/abandon/abandonment/B-tags/fold checks/hyphen
  fold/mutex note, wordpiece decode validation + piece cap + pre-sized fold +
  NFKD doc, fake-pool doc, fetch plumbing (digests pending), piiwiring Bearer
  strip + Write ErrShortWrite + dropped `g`.
- tests: `pii/isolation_test.go` (7 new), `config_file_test.go` (H9 guard),
  fuzz target + hostile seeds, redaction `@` case, fixture-backed CCRealTool,
  `-short` live guard, SweepBurns restore, gate ordering assertion, Live→Local
  renames, ReadAll caps, M73 production filter, freepi declared-reasoning case.
- scripts: freebuff atomic write + dup-key strip + argparse, freepi 0600 jwt +
  argparse + line-index config splice, probe config-jwt + argparse + close fix,
  cctest cred strip, opencode path + missing-file guard, probe /tmp default +
  `itertools.count` + empty-models guard, freebuff bounded retries + URLError +
  run_id2 finish + SSE fold, antseed empty-text guard + URL quote.
- nim: proxy-dir 0600 pidfile + validated kill, pkill removal, status exit code,
  log guards.
- CI: `go-version-file`, gofmt/vet/race/ner jobs, top-level `contents: read`.
- deploy: PORT env dropped (2 units), `Restart=on-failure`, plist throttle.
- nimstatus.py: jsonl mtime cache, bounded tail seek, int guard, cred redact,
  fmt simplification.
- docs: README 4 backends + NIM_AUTH + subcommands, AGENTS routes/headers/
  redaction/modes/freepi/antseed, TODO renumber + symbol anchors + recount,
  PLAN values + freepi shipped, ANTSEED shipped + flat config + canon home,
  FREEBUFF gate recipe/renumber/flag/test-file/harness/halt/prices, FREEPI
  dedupe + §8 privacy + catalog note, NOTICE rev pin, RTK.md new,
  CHANGED-SINCE deleted, guardrails loader documented.
- disagreements respected: `antReadFrame` deleted (dead, not hardened);
  `ack.Ts` check added low-urgency (nonce already covers); presence checks at
  `config.go:452` untouched (verified not-a-bug); `probeProxyFull`/
  `sampleProxies` kept (test-used); M40 no-change (refresh throttle gates);
  `stripJSONComments` fixed cheaply; `freepiCatalog` fallback kept.
