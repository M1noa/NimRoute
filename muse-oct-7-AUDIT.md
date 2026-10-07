# audit 2026-10-07 (partial — docs section complete, code sections pending)

> status: 1/5 subagents returned. code sections (core go, zen routing, pii, tests/scripts) still running. this file will be extended as they land. no source files touched.

## critical

- `FREEBUFF-DOCS.md:113` — acc2 suspended for third-party proxy use, doc bans live tests until client-legitimacy solved, but plan/todo still schedule sweeps. fix: halt phases B-D until legitimacy approach documented.
- `FREEBUFF-PLAN.md:30` — per-turn lifecycle forwards only `model` + metadata, but docs require `runId`, server-UUID, Buffy opener, `cost_mode:'free'` or turn 403s. fix: add full gate recipe to plan lifecycle.

## high

- `README.md:4` — lists only NIM + opencode zen, but `config.go:222,238` + `model_info.jsonc:110` ship freepi and antseed. fix: rewrite backend paragraph, link per-backend docs.
- `AGENTS.md:23` — routing says `opencode/` → zen, else NIM, but example defines `freepi/`, `antseed/` prefixes. fix: document all four prefixes + handlers.
- `TODO.md:186,207` — two sections numbered `## 11`, breaks stable-numbering invariant at `TODO.md:4`. fix: renumber 11-13, fix xrefs.
- `TODO.md:203` — cooldown-skip bullet under item 11 (fetch-tool 502) belongs to item 10 pool tuning. fix: move it.
- `config.yml.example:149` — zen section omits `ConvoIdleMinutes` (`config.go:159`, promised `PLAN.md:22`). fix: add example block.
- `config.yml.example:299` — freepi omits `queue_wait_secs` (`config.go:230`). fix: document or remove field.
- `config.go:452` — presence checks restore only `show_usage`/`show_pool`; `ShowKeys`, `ShowOpencodeModels`, `ShowZen`, `ShowLocks`, `guardrails.enabled`, `inject.params`, `nudge.enabled` also default true unchecked. fix: extend raw-doc checks or `*bool` pattern.
- `config.go:878` — `StatusFor` hides `Freepi` unauthenticated with no `status.show_freepi` flag, violates `AGENTS.md:82`. fix: add flag + redaction + docs.
- `AGENTS.md:85` — redaction list omits freepi, antseed, `LaneCap`, `ZenLanes` (`config.go:906` hides them). fix: update list to match code.
- `config.yml.example:362` — `claude_map` defaults to muse-spark-1.3 / bare `moonshotai/kimi-k3`, but only space-bunny serves (`TODO.md:396`), kimi-k3 503s keyless. fix: repoint defaults at serving model.
- `ANTSEED-PLAN.md:3` — header says nothing implemented, but `ANTSEED-TODO.md:7` records full impl + e2e + restart. fix: flip status, point to files.
- `ANTSEED-PLAN.md:76` — mandates static table with `supersedes:`, but `model_info.jsonc:117` flat, todo sorts by live count. fix: pick one ordering.
- `CHANGED-SINCE-682b2eb.md:1` — scratch pins HEAD `b4fa0b3` (2026-10-05), project renamed twice since, file says delete at line 123. fix: delete file.
- `PLAN.md:56` — freepi as future work with lane mechanics, but implemented as one-stream-per-account, no shared lanes (`TODO.md:347`). fix: mark done or archive.

## medium

- `AGENTS.md:115` — lane modes list hdr/convo/shared, omits `lane_cache_key` (`config.go:135`). fix: add fourth mode.
- `TODO.md:154` — item 9 labeled done, subtasks (startup warn, per-id test) read open. fix: check off with evidence or reopen.
- `TODO.md:156` — claims 102 entries (81 nim + 14 zen + 7 freepi), but jsonc has 15 zen + 5 antseed. fix: refresh count + date.
- `TODO.md:149` — item 8 discusses lane cap 5→6, `config.go:366` defaults 25. fix: update or close.
- `ANTSEED-PLAN.md:33` — nested `discovery:` + timeouts, but `config.go:238` flattened/dropped keys. fix: reconcile plan/code/example.
- `ANTSEED-PLAN.md:20` — plans `antseedcat.go`, todo implements catalog in `antseed.go` unexplained. fix: record canonical-key/ranking location.
- `ANTSEED-TODO.md:63` — stale uncommitted-changes warning + `/var/folders/...` path at :70. fix: delete note, reproducible clone cmd.
- `ANTSEED-TODO.md:49` — e2e claims `gpt-oss-120b`, no antseed entry in jsonc. fix: add entry or correct note.
- `FREEBUFF-PLAN.md:15` — `test_port: 5420`, no `model_refresh_sec`, example adds it; key lists disagree. fix: unify key list.
- `FREEBUFF-PLAN.md:69` — order 7, 8b, 8; no `show_freebuff` flag though required. fix: renumber, name flag + redaction.
- `FREEBUFF-PLAN.md:96` — chat POST `https://codebuff.com`, canonical is `https://www.codebuff.com`. fix: use www.
- `FREEBUFF-TODO.md:10` — A6 proposes backoff for chat 503s, docs reclassify as pre-ban soft-refuse. fix: update to ban hypothesis, forbid sweeps.
- `FREEBUFF-TODO.md:19` — B7 names `main_test.go`, repo uses per-area files. fix: name real target.
- `FREEBUFF-MODEL-TABLE.md:1` — price `?`/`entitle`/`0`/`10/15pk`, no rev pin for 2026-09-28 source. fix: legend + revision.
- `FREEPI-DOCS.md:5` — stray README disclosure quote in research note. fix: delete line.
- `FREEPI-DOCS.md:90` — fingerprint/chat/ads/version guidance duplicated 90-107, ethics buried. fix: dedupe to one block.
- `FREEPI-DOCS.md:1` — no privacy section, unlike FREEBUFF-DOCS. fix: document prompt/ads/meter exfil.
- `FREEPI-DOCS.md:12` — says catalog not live-tested, :64 presents live 2026-10-03 bench. fix: clarify catalog vs probed.
- `ANTSEED-DOCS.md:3` — file:line refs into unpinned clone. fix: pin SHA, move Go subset (:505) to top.
- `FREEBUFF-PLAN.md:59`, `FREEBUFF-TODO.md:21` — reference `scripts/cctest.py`, `.gitignore:41` ignores it. fix: track script or reference committed harness.
- `TODO.md:208,276` — hardcoded `main.go:4453`, `config.go:387`, `main.go:3388` no revision, already drifted. fix: symbol names or drop numbers.

## low

- `.gitignore:16` — ignores only `nim-usage.jsonl` + legacy names; scratch `config.*.yml` + `nim-perf.jsonl` untracked. fix: add patterns.
- `NOTICE:3` — credits Rampart without model/code version. fix: pin HF revision + commit.
- `CHANGED-SINCE-682b2eb.md:8` — `[x]` means rollback here vs done in TODO. fix: delete file (see above).
- `config.yml.example:260` — freebuff only backend with inline privacy notes. fix: matching pointers for all four or one docs section.

## files covered so far

README.md, AGENTS.md, PLAN.md, TODO.md, ANTSEED-PLAN.md, ANTSEED-TODO.md, ANTSEED-DOCS.md, FREEBUFF-PLAN.md, FREEBUFF-TODO.md, FREEBUFF-DOCS.md, FREEBUFF-MODEL-TABLE.md, FREEPI-DOCS.md, CHANGED-SINCE-682b2eb.md, LICENSE, NOTICE, .gitignore (+ cross-checks config.yml.example, config.go, model_info.jsonc, main.go:1-150).

## pending (agents running)

- core go: main.go, config.go, modelinfo.go, usage.go, rtk.go, rtkcompress.go, rtkdetect.go, piiwiring.go, guardrails.json, go.mod
- zen routing: anthropic.go, zenproxy.go, zenlanes.go, antseed.go, antseedp2p.go, freepi.go
- pii/: full package + tests
- tests/scripts: *_test.go, scripts/, nimstatus.py, nim, model_info.jsonc, deploy/, .github/workflows/build.yml
