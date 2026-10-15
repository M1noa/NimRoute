# audit 2026-10-07 — NimRoute

> read-only. no source files modified. subagents stalled on upstream 502s, so code sections were audited by direct inspection (grep + targeted reads). security-audit skill unavailable, manual pass done instead. `go vet ./...` clean, `gofmt -l .` clean, no TODO/FIXME/XXX in non-test code.

## critical

- `antseedp2p.go:769` — `mustHex(ack.Pub)` panics on malformed hex after sig check. `ack` is peer-controlled; dialed seller signs non-hex `pub` and crashes the whole proxy. fix: error-returning decode here, delete `mustHex`.
- `antseedp2p.go:111,662` — `panic(err)` on `crypto/rand` failure and in `mustHex`. any panic takes down :5419. fix: return errors.
- `anthropic.go:2044-2051` — `ZEN_PASSTHROUGH=1` copies client `X-Opencode-*` headers verbatim upstream, bypassing the `zenHeaderForwarded` blocklist (`main.go:3663`). client spoofs session/project identity to zen. fix: delete passthrough or drop `x-opencode-*` even in passthrough mode.

## high

- `main.go:4107-4130` — `captureRequest` (`ZEN_CAPTURE`) uncapped `io.ReadAll(r.Body)` bypasses the 10MB `bodyLimit` (`:2838`), dumps full `r.Header` (auth) + body to `/tmp/oc-capture` at 0644. fix: `LimitReader` + skip auth headers + 0600.
- `anthropic.go:2027-2036,2094-2095` — `zenDebugCapture` writes inbound `r.Header` to capture dir at 0644. same cred-exposure shape. fix: redact auth/cookie/session before marshal, 0600.
- `anthropic.go:2120-2127,2348-2351,2420-2423` — `ZEN_DUMP` writes upstream req/resp bodies at 0644. fix: 0600 + doc note that flag persists traffic.
- `main.go:2767-2769` — `Access-Control-Allow-Origin: *` + `Allow-Headers: *` on all routes incl authenticated `/v1/*`. any site with a leaked token spends quota. fix: restrict origin or config-gate.
- `main.go:2962-2963` — `dbglog` prints full `r.Header` + body when `debugMode`. auth material in debug log. fix: redact authorization/x-api-key/cookie/session.
- `main.go:3020-3022` — NIM path forwards client `X-Forwarded-Host` upstream; zen list drops it (`:3667`) as client-claimed. fix: drop it in NIM too, stop setting `X-Forwarded-For` from `RemoteAddr` without opt-in.
- `scripts/freepi_login.py:67` — JWT saved via bare `open(out,"w")`, umask-dependent; `config.go:512` uses 0600 for seeds. fix: `os.chmod(out, 0o600)`.
- `config.go:878-917` — `StatusFor` has `ShowAntseed` but no `show_freepi` flag; `sr.Freepi` gated only on unauth (`:910`), violates AGENTS §4 every-section-needs-flag. fix: add `show_freepi` + redaction + docs.
- `README.md:4` — lists NIM + zen only; `config.go:222,238` + routing (`main.go:2906-2939`) ship freepi + antseed. fix: rewrite backend paragraph.
- `AGENTS.md:23` — routing says `opencode/` → zen else NIM; code routes `freeepi/`, `antseed/` too. fix: document all four.
- `TODO.md:186,206` — two `## 11` sections, breaks stable-numbering invariant (`:4`). fix: renumber 11-13 + xrefs.
- `AGENTS.md:85` — redaction list omits freepi/antseed/`LaneCap`/`ZenLanes` though `config.go:906-914` hides them. fix: sync list.

## medium

- `piiwiring.go:43-52` — `piiSessionID` hashes full `Authorization` value incl `Bearer ` scheme, so `Bearer X` vs `X` split one client into two vaults. fix: strip scheme before hashing.
- `pii/pii.go:65-81` — `SessionKey` truncates sha256 to 64 bits; `vaultKey` hardcoded `"nimroute-pii-v1"`, identical surrogates across installs. fix: full hash + `crypto/rand` key at startup (persist only if cross-restart stability wanted).
- `config.yml.example:299` — freepi section omits `queue_wait_secs` (`config.go:230`, `freepi.go:52`). fix: document or remove field.
- `config.yml.example:149` — zen section omits `convo_idle_minutes` (`config.go:159`). fix: add block.
- `config.yml.example:362-370` — `claude_map` defaults to muse-spark-1.3 / bare `moonshotai/kimi-k3`; sweep notes only space-bunny serves (`TODO.md:396`). fix: repoint keyless defaults.
- `ANTSEED-PLAN.md:3` — header says nothing implemented; `ANTSEED-TODO.md:7` + `antseed.go`/`antseedp2p.go` show full impl. fix: flip status, point to files.
- `ANTSEED-PLAN.md:33` vs `config.go:238` — plan shows nested `discovery:` + timeouts, code flattened/dropped. fix: reconcile.
- `ANTSEED-PLAN.md:20` — plans `antseedcat.go`; catalog lives in `antseed.go` unexplained. fix: record canonical location.
- `ANTSEED-TODO.md:63,70` — stale uncommitted-changes warning + `/var/folders/...` absolute path. fix: delete, use reproducible clone cmd.
- `CHANGED-SINCE-682b2eb.md:1` — pins HEAD `b4fa0b3` (2026-10-05), renames since, self-delete note at eof. fix: delete file.
- `PLAN.md:56` — freepi as future work; implemented one-stream-per-account (`TODO.md:347`). fix: mark done or archive.
- `TODO.md:154-156` — item 9 done but subtasks read open; claims 102 entries (81+14+7), jsonc now 108 (33 nvidia + 15 opencode + 7 freeepi + 5 antseed + rest). fix: refresh count + date or reopen.
- `TODO.md:149` — lane cap 5→6; `config.go:366` + `zenlanes.go:389` default 25. fix: update or close.
- `TODO.md:203` — cooldown-skip bullet under item 11 (fetch 502) belongs to item 10. fix: move.
- `TODO.md:208,276` — hardcoded `main.go:4453`, `config.go:387`, `main.go:3388` with no rev, already drifted. fix: symbol names or drop numbers.
- `FREEBUFF-PLAN.md:30` vs `FREEBUFF-DOCS.md:103` — turn lifecycle forwards model+metadata only, but turns need runId/server-UUID/Buffy opening/`cost_mode:free` or 403. fix: add gate recipe.
- `FREEBUFF-DOCS.md:113` vs plan/todo — acc2 suspended, doc bans live tests, plan still schedules sweeps/streaming with no legitimacy design. fix: halt phases B-D until documented.
- `FREEBUFF-PLAN.md:69` — order 7,8b,8; no `show_freebuff` flag though required. fix: renumber + flag.
- `FREEBUFF-TODO.md:19` — B7 names `main_test.go`; repo uses per-area files. fix: name real target.
- `FREEBUFF-PLAN.md:59` + `FREEBUFF-TODO.md:21` — ref `scripts/cctest.py`; `.gitignore:41` ignores it, fresh clones lack it. fix: track or ref committed harness.
- `FREEPI-DOCS.md:90-107` — fingerprint/chat/ads/version guidance duplicated, ethics buried in second copy. fix: dedupe.
- `FREEPI-DOCS.md:1` — no privacy section unlike FREEBUFF-DOCS. fix: document prompt/ads/meter exfil.
- `FREEPI-DOCS.md:12,64` — catalog not live-tested vs live 2026-10-03 bench. fix: clarify catalog vs probed.
- `ANTSEED-DOCS.md:3` — file:line refs into unpinned clone. fix: pin SHA.

## low

- `usage.go:126`, `main.go:749,757` — usage/access logs 0644; model + key names, not secrets, still world-readable. fix: 0640 or document.
- `live_test.go:173`, `todo_small_test.go:657,677` — uncapped `io.ReadAll` on test bodies; large upstream OOMs test run. fix: `LimitReader`.
- `main.go:4405`, `zenproxy.go:464-466`, `main.go:325`, `zenproxy.go:365` — `math/rand` for key/proxy sampling only, acceptable; go 1.26 auto-seeds. fix: comment sampling-only.
- `.gitignore:16` — ignores `nim-usage.jsonl` + legacy names only; scratch `config.*.yml` + `nim-perf.jsonl` untracked. fix: add patterns.
- `NOTICE:3` — Rampart credit without model/code version. fix: pin HF rev + commit.
- `config.yml.example:260` — freebuff-only inline privacy notes. fix: pointers for all four or one docs section.
- `AGENTS.md:115` — lane modes hdr/convo/shared, omits `lane_cache_key` (`config.go:135`). fix: add fourth mode.
- `FREEBUFF-MODEL-TABLE.md:1` — price `?`/`entitle`/`0`, no rev pin. fix: legend + revision.
- `FREEPI-DOCS.md:5` — stray README disclosure quote. fix: delete line.

## corrections to prior audit (muse-oct-7, verified wrong)

- presence-check gap (`config.go:452`): not a bug. `loadConfigFile` starts from `defaultConfig()` (all show_* true) and `yaml.Unmarshal` leaves absent keys alone, so explicit `false` survives. checks are redundant, not lossy.
- empty-header lane collapse (`zenlanes.go:583`): not a bug. empty `headerSession` falls through to pck/convo/shared spread (`:584-632`), never a single collapsed lane.
- `live_test.go` network use: by design (e2e minus network hop), sleeps are short/fixed-count, acceptable.

## verified clean

- all prod upstream `io.ReadAll` capped (`upstreamBodyLimit` 128MB, `bodyLimit` 10MB, `antseed.go:379` 32MB, `freepi.go:213` 1MB); only uncapped prod read is `captureRequest` (flagged).
- `zenHeaderForwarded` (`main.go:3663`) + NIM deny-list (`:2981`) drop auth/session/cookie; no cred forwarding outside `ZEN_PASSTHROUGH` (flagged).
- `redactProxyUserinfo` (`zenlanes.go:1219`) at every log/status site grepped.
- auth compare `subtle.ConstantTimeCompare` (`config.go:841`); session-id ids (`zenID`/`randHex`/`randBase62`) use `crypto/rand`.
- scan-cache sweep exists (`pii.go:208`, called `zenlanes.go:1185`), scan/vault TTLs separate.
- only two prod `panic` sites, both flagged; `zen_schema_test.go:30` panic is test-only.
- `pii` guard/vault registries bounded (`maxGuards`/`maxVaults` 512, LRU); scan cache bounded 2048 + idle sweep.

## files covered

all root `*.go` (main, config, anthropic, zenproxy, zenlanes, freepi, antseed, antseedp2p, modelinfo, usage, piiwiring, rtk, rtkcompress, rtkdetect + all `*_test.go`: main, todo_small, audit_fixes, fidelity, fuzz, lane_tokens, live, regress_stream_nudge, stream_cancel, usage, zen_schema, antseed), all `pii/*.go` (pii, detect, vault, scan, span, custom, fake, wordpiece, ner_ort, ner_stub + tests + `fetch_model.sh`), `scripts/*.py` (antseed_test, cctest, freebuff_client, freebuff_login, freepi_login, freepi_probe, opencode_prompt, probe_zen_proxies), `nimstatus.py`, `nim`, `model_info.jsonc`, `guardrails.json`, `config.yml.example`, `go.mod/go.sum`, `deploy/*`, `.github/workflows/build.yml`, README.md, AGENTS.md, PLAN.md, TODO.md, ANTSEED-PLAN/TODO/DOCS, FREEBUFF-PLAN/TODO/DOCS/MODEL-TABLE, FREEPI-DOCS, CHANGED-SINCE-682b2eb.md, LICENSE, NOTICE, .gitignore.

## gaps (not reviewed)

- `testdata/`, `pii/testdata/vocab.txt` contents (fixtures).
- `leaked-system-prompts/`, `system-prompts/`, `system_prompts_leaks/` corpora (data dirs).
- `-tags ner` build (`go build -tags ner` not run).
- live e2e against :5419 (primary untouched per AGENTS §2).
