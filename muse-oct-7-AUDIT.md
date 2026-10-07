# audit 2026-10-07 — NimRoute

> read-only. no source files modified. subagents stalled partway, so code sections were audited by direct inspection (grep + targeted reads) instead. docs section by subagent with cross-checks into config.go, config.yml.example, model_info.jsonc, main.go:1-150.

## critical

- `antseedp2p.go:769` — `mustHex(ack.Pub)` panics on malformed hex, and `ack` arrives from the network peer. a malicious seller sends a validly-signed ack with non-hex `pub` and crashes the whole proxy. fix: replace `mustHex` with an error-returning decode at this call site.
- `antseedp2p.go:111,662` — `panic(err)` on `crypto/rand` failure and in `mustHex`. any panic takes down the single proxy process serving :5419. fix: return errors instead of panicking.
- `FREEBUFF-DOCS.md:113` vs `FREEBUFF-PLAN.md:30`, `FREEBUFF-TODO.md:21` — acc2 suspended for third-party proxy use, doc bans live tests until client-legitimacy solved, but plan/todo still schedule sweeps and streaming turns with no legitimacy design. fix: halt phases B-D until a legitimacy approach is documented.
- `FREEBUFF-PLAN.md:30` vs `FREEBUFF-DOCS.md:103` — per-turn lifecycle forwards only `model` + metadata, but turns require agent-run `runId`, server-UUID instance, Buffy system opening, `cost_mode:'free'` or they 403. fix: add the full gate recipe to the plan lifecycle.

## high

- `main.go:4106-4129` — `captureRequest` (gated on `ZEN_CAPTURE`) writes full request headers including `Authorization`/`x-api-key` plus bodies to `/tmp/oc-capture` with mode 0644. any local user reads proxy credentials. fix: skip auth headers in the dump and write 0600.
- `anthropic.go:2021-2124,2339-2438` — `ZEN_DUMP` writes upstream request/response bodies to disk 0644, same credential-exposure shape on a debug path. fix: 0600 and document that the flag persists sensitive traffic.
- `main.go:4107` — `captureRequest` uses uncapped `io.ReadAll(r.Body)` before the 10MB `bodyLimit` read at `:2838`, so a client bypasses the body cap by setting `ZEN_CAPTURE`. fix: wrap in `io.LimitReader` with `bodyLimit`.
- `scripts/freepi_login.py:67` — JWT saved with bare `open(out, "w")`, honoring umask instead of 0600 like `config.go:512` does for seeds. fix: `os.chmod(out, 0o600)` after write.
- `README.md:4` — lists only NIM + opencode zen, but `config.go:222,238` + `model_info.jsonc:110` ship freepi and antseed. fix: rewrite backend paragraph, link per-backend docs.
- `AGENTS.md:23` — routing says `opencode/` → zen, else NIM, but example defines `freepi/`, `antseed/` prefixes. fix: document all four prefixes + handlers.
- `TODO.md:186,207` — two sections numbered `## 11`, breaks the stable-numbering invariant at `TODO.md:4`. fix: renumber 11-13, fix xrefs.
- `config.go:452` — presence checks restore only `show_usage`/`show_pool`; `ShowKeys`, `ShowOpencodeModels`, `ShowZen`, `ShowLocks`, `guardrails.enabled`, `inject.params`, `nudge.enabled` also default true unchecked, so explicit `false` is silently dropped on reload. fix: extend raw-doc checks or convert to `*bool`.
- `config.go:878-911` — `StatusFor` hides `Freepi` on unauthenticated calls but no `status.show_freepi` flag exists, violating `AGENTS.md:82` (every new section needs a flag). fix: add `show_freepi` + redaction + docs.
- `AGENTS.md:85` — redaction list omits freepi, antseed, `LaneCap`, `ZenLanes` though `config.go:906` hides them. fix: update list to match code.
- `config.yml.example:362` — `claude_map` defaults to muse-spark-1.3 / bare `moonshotai/kimi-k3`, but only space-bunny serves (`TODO.md:396`) and kimi-k3 503s keyless. fix: repoint keyless defaults at a serving model.
- `ANTSEED-PLAN.md:3` — header says nothing implemented, but `ANTSEED-TODO.md:7` records full impl + e2e + primary restart. fix: flip status, point to files.
- `CHANGED-SINCE-682b2eb.md:1` — scratch pins HEAD `b4fa0b3` (2026-10-05), project renamed twice since, file says delete itself at line 123. fix: delete the file.
- `PLAN.md:56` — freepi as future work with cooldown/burn lane mechanics, but implemented as one-stream-per-account with no shared lanes (`TODO.md:347`). fix: mark done or archive.

## medium

- `main.go:2767-2769` — `Access-Control-Allow-Origin: *` with `Allow-Headers: *` on all routes including authenticated `/v1/*`. any website can spend the operator's backend quota if it learns a token. fix: restrict origin or gate CORS behind a config flag.
- `main.go:2962` — `dbglog` prints full `r.Header` (auth material) whenever `DEBUG_FILE` is set. fix: redact authorization/x-api-key/cookie in the debug dump.
- `main.go:2980-2982` — NIM deny-list drops `authorization`, `host`, `origin`, `cookie`, `x-api-key`, `x-session-id` but not `x-forwarded-host`, which the zen list (`:3664`) drops as client-claimed. fix: add it for parity.
- `pii/pii.go:65-73` — `SessionKey` truncates sha256(apiKey) to 64 bits for the vault id; `vaultKey` is a hardcoded constant (`"nimroute-pii-v1"`), so surrogate derivation is identical across restarts and installs. fix: generate the hmac key with `crypto/rand` at startup.
- `piiwiring.go:39-53` — `piiSessionID` hashes the full `Authorization` header value (scheme + token) rather than the token, so `Bearer X` and `X` split one client into two vaults. fix: strip the scheme before hashing.
- `config.yml.example:149` — zen section omits `ConvoIdleMinutes` (`config.go:159`, promised `PLAN.md:22`). fix: add example block.
- `config.yml.example:299` — freepi omits `queue_wait_secs` (`config.go:230`). fix: document or remove field.
- `TODO.md:203` — cooldown-skip bullet under item 11 (fetch-tool 502) belongs to item 10 pool tuning. fix: move it.
- `ANTSEED-PLAN.md:76` — mandates static table with `supersedes:`, but `model_info.jsonc:117` is flat and todo sorts by live count. fix: pick one ordering.
- `TODO.md:154` — item 9 labeled done, subtasks (startup warn, per-id test) read open. fix: check off with evidence or reopen.
- `TODO.md:156` — claims 102 entries (81 nim + 14 zen + 7 freepi); jsonc now has 15 zen + 5 antseed. fix: refresh count + date.
- `TODO.md:149` — item 8 discusses lane cap 5→6; `config.go:366` defaults 25. fix: update or close.
- `ANTSEED-PLAN.md:33` — nested `discovery:` + timeouts, but `config.go:238` flattened/dropped keys. fix: reconcile plan/code/example.
- `ANTSEED-PLAN.md:20` — plans `antseedcat.go`; catalog implemented in `antseed.go` unexplained. fix: record where canonical-key/ranking lives.
- `ANTSEED-TODO.md:63,70` — stale uncommitted-changes warning plus `/var/folders/...` absolute path. fix: delete note, use reproducible clone cmd.
- `ANTSEED-TODO.md:49` — e2e claims `gpt-oss-120b`, no antseed entry in jsonc. fix: add entry or correct note.
- `FREEBUFF-PLAN.md:15`, `FREEBUFF-TODO.md` — `test_port: 5420`, no `model_refresh_sec`; example adds it. fix: unify key list in one place.
- `FREEBUFF-PLAN.md:69` — order 7, 8b, 8; no `show_freebuff` flag though required. fix: renumber, name flag + redaction.
- `FREEBUFF-PLAN.md:96` — chat POST `https://codebuff.com`; canonical is `https://www.codebuff.com`. fix: use www.
- `FREEBUFF-TODO.md:10` — A6 proposes backoff for chat 503s; docs reclassify as pre-ban soft-refuse. fix: update to ban hypothesis, forbid sweeps.
- `FREEBUFF-TODO.md:19` — B7 names `main_test.go`; repo uses per-area files. fix: name real target.
- `FREEBUFF-MODEL-TABLE.md:1` — price `?`/`entitle`/`0`/`10/15pk`, no rev pin for 2026-09-28 source. fix: legend + revision.
- `FREEPI-DOCS.md:5` — stray README disclosure quote in research note. fix: delete line.
- `FREEPI-DOCS.md:90-107` — fingerprint/chat/ads/version guidance duplicated, ethics buried in second copy. fix: dedupe to one block.
- `FREEPI-DOCS.md:1` — no privacy section, unlike FREEBUFF-DOCS. fix: document prompt/ads/meter exfil.
- `FREEPI-DOCS.md:12,64` — says catalog not live-tested, then presents live 2026-10-03 bench. fix: clarify catalog vs probed.
- `ANTSEED-DOCS.md:3` — file:line refs into unpinned clone. fix: pin SHA, move Go subset (:505) to top.
- `FREEBUFF-PLAN.md:59`, `FREEBUFF-TODO.md:21` — reference `scripts/cctest.py`; `.gitignore:41` ignores it so fresh clones lack it. fix: track script or reference committed harness.
- `TODO.md:208,276` — hardcoded `main.go:4453`, `config.go:387`, `main.go:3388` no revision, already drifted. fix: symbol names or drop numbers.

## low

- `main.go:4404` — key probe uses `rand.Intn` (`math/rand`) for key choice; sampling-only so acceptable, but unseeded-source confusion recurs (go ≥1.20 auto-seeds, `go.mod` says 1.26). fix: comment that math/rand is sampling-only here.
- `usage.go:126`, `main.go:749,757` — usage/access logs open 0644; contain model + key *names* (not secrets), still world-readable. fix: 0640 or document.
- `live_test.go:173`, `todo_small_test.go:617,637` — uncapped `io.ReadAll` on test response bodies. test-only, but a large upstream body OOMs the test run. fix: cap with `io.LimitReader`.
- `zenlanes.go:583` + `main.go:3215` — lanes key on raw `x-session-id` verbatim including empty string, so all headerless clients share one lane. known per AGENTS §7, but empty-header collapse is undocumented. fix: note it next to `laneFor`.
- `.gitignore:16` — ignores only `nim-usage.jsonl` + legacy names; scratch `config.*.yml` + `nim-perf.jsonl` untracked (`CHANGED-SINCE-682b2eb.md:121`, `TODO.md:257`). fix: add patterns.
- `NOTICE:3` — credits Rampart without model/code version. fix: pin HF revision + commit.
- `config.yml.example:260` — freebuff only backend with inline privacy notes. fix: matching pointers for all four or one docs section.
- `AGENTS.md:115` — lane modes list hdr/convo/shared, omits `lane_cache_key` (`config.go:135`). fix: add fourth mode.

## verified clean (checked, no finding)

- all other upstream `io.ReadAll` sites capped via `io.LimitReader` (`upstreamBodyLimit` 128MB, `bodyLimit` 10MB); `antseed.go:379` 32MB, `freepi.go:206` 1MB.
- `zenHeaderForwarded` (`main.go:3662`) and NIM deny-list (`main.go:2980`) both drop auth/session/cookie; no credential forwarding found.
- `redactProxyUserinfo` (`zenlanes.go:1219`) applied at every log/status call site grepped.
- auth comparison uses `subtle.ConstantTimeCompare` (`config.go:841`).
- `gofmt -l .` clean, `go vet ./...` clean.
- scan-cache sweep exists (`pii.go:208`, called `zenlanes.go:1185`) and scan/vault TTLs are separate per comments.
- only two `panic` sites, both flagged above; no TODO/FIXME/XXX in non-test code.

## files covered

all root `*.go` (anthropic, antseed, antseedp2p, config, freepi, main, modelinfo, piiwiring, rtk, rtkcompress, rtkdetect, usage, zenlanes, zenproxy + all `*_test.go`), all `pii/*.go`, `scripts/*.py`, `nimstatus.py`, `nim`, `model_info.jsonc`, `guardrails.json`, `config.yml.example`, `go.mod/go.sum`, `deploy/*`, `.github/workflows/build.yml`, README.md, AGENTS.md, PLAN.md, TODO.md, ANTSEED-PLAN.md, ANTSEED-TODO.md, ANTSEED-DOCS.md, FREEBUFF-PLAN.md, FREEBUFF-TODO.md, FREEBUFF-DOCS.md, FREEBUFF-MODEL-TABLE.md, FREEPI-DOCS.md, CHANGED-SINCE-682b2eb.md, LICENSE, NOTICE, .gitignore.

## gaps (not reviewed)

- `testdata/`, `pii/testdata/vocab.txt` contents (treated as fixtures).
- `leaked-system-prompts/`, `system-prompts/`, `system_prompts_leaks/` corpora — data dirs, out of scope for a code audit.
- behavior under `-tags ner` build (`go build -tags ner` not run; only default `go vet`).
- live e2e against :5419 (primary instance untouched per AGENTS §2).
