# TODO

merged from TODO.md + IMPROV_TODO.md (lanes, ttft, proxy rotation).
open items first, done work and notes at the bottom. numbering stays
stable — AGENTS.md references items 9 and 10 — so new items append at
the end instead of re-sorting by size.

sorted simplest first within that constraint. each item keeps current
defaults unchanged.

## 1. config: merge new keys on update, never wipe user config [small] DONE

`loadOrCreateConfig` only seeds `config.yml` when the file is missing. when
`config.yml.example` gains keys (new feature), existing configs never get
them and reload keeps running on stale defaults.

- on startup, compare example keys against the user file; append missing
  sections/keys as commented defaults, preserving everything the user set.
- malformed user yaml must still keep the old running config (already does
  in `watchConfig`; keep that).
- test: seed an old config, start, assert new keys appear commented and
  user values untouched.

done: `mergeMissingKeys` in config.go appends missing example keys as
commented defaults with real values, append-only, idempotent via
`# added:` header. test `TestMergeMissingKeys`.

## 2. status: proxy-pool + lane section, with config flags [small] DONE

`/status` shows lanes (`zen_lanes`) but not the underlying pool: verified
proxy count, per-exit latency/country, pool refresh age, lane cap/usage.

- add `pool: {verified, candidates, last_refresh_ago}` + per-exit rows
  (proxy, country, latency_ms, pinned_lane?) to `StatusResponse`.
- `status:` flags: `show_pool` (default true). respect the existing
  unauthenticated redaction in `StatusFor` (hide when tokens set + no token).
- dead exits already drop via `dropProxy`; surface `dropped_total` counter.

done: `poolSnapshot` in zenproxy.go, `Pool` in StatusResponse,
`status.show_pool` flag, redaction in StatusFor. test `TestPoolSnapshot`.

## 3. guardrail tuning knobs in config [small] DONE

threshold `0.6` (`guardrailMatchThreshold`), window/density weights
(`0.7 + 0.3*density` in `guardrailScore`), and the enable switch exist but
only `enabled/file/extra` are configurable.

- add `guardrails.threshold`, `guardrails.window_weight`,
  `guardrails.density_weight`, keep defaults `0.6` / `0.7` / `0.3`.
- `enabled: false` already short-circuits in `stripSomeGuardrails`; keep.
- test: same input strips at default, passes at `threshold: 1.0`.

done: `guardrailThreshold()`/`guardrailWeights()` read config with same
defaults. test `TestGuardrailThresholdKnob`.

## 4. required zen params on every request shape [medium] SUPERSEDED

title-gen/summary calls 403 (`FreeTierError`) because they skip what
agentic calls carry: the four gate tools (`ensureZenTools` only fills
`tools` when the key exists and is non-empty) plus `stream: true` +
`stream_options.include_usage`.

- always run `ensureZenTools` shape fixup: when `tools` is missing/empty,
  still inject the four gate stubs (they are never invoked; zen only
  checks presence). verify against the current 403: title-gen body vs
  agentic body.
- audit `handleOpenCode` + `handleOpenCodeAnthropic` + jev path: every
  outbound body must carry model, stream, stream_options, tools, and the
  `x-opencode-*` header set. add a `assertZenBody` test helper.
- `nudge_no_tools`/cache keys must keep being stripped (`stripCacheFields`).

superseded 2026-10-01: the no-tools skip in `ensureZenTools` is
deliberate — giving the summarizer callable stubs makes it emit tool
calls that zen rejects with "tool call not allowed while generating
summary" (main.go comment). live 403s are per-exit free-tier blocks,
survived by rotation (exit burns), not a request-shape gap: every
outbound path already carries model, stream, stream_options, and
`x-opencode-*` headers. do NOT inject stubs into tool-less bodies.

## 5. zen lanes: 6m session-idle eviction + scan-cache TTL [medium] DONE

lanes expire by `lane_ttl_minutes` (default 30m) swept on the pool tick;
the ask is 6m of no activity clears the session hash/id from cache.

- add `zen.session_idle_minutes` (default 6): `sweepLanes` drops lanes
  idle past it (keep `lane_ttl_minutes` as the hard cap, default 30).
- scan-cache entries keyed by paragraph hash already exist in `pii/scan.go`;
  give them a last-access timestamp and evict after 6m idle on the same
  sweep. `vault_ttl_hours` (24h) stays for the vault itself.
- per-session surrogate maps already live in the vault; evicting the lane
  must not evict vault entries (different TTLs).

done: `session_idle_minutes` (default 6, 0 disables) evicts idle lanes
in `sweepLanes`; scan cache has `lastAccess` + `sweepIdle`, called from
the same sweep via `pii.SweepScanIdle` (vault untouched). tests
`TestLaneSessionIdleEviction`, `TestLaneSessionIdleDisabled`,
`TestScanSweepIdle`.

## 6. anonymize config wiring pass [medium] DONE

most knobs exist (`mode`, `fuzzy_threshold`, `detect_secrets`,
`detect_pii`, `keep_labels`, `include_system`, `on_timeout`,
`vault_ttl_hours`, `vault_max_size`, entities/terms, ner block). gaps:

- `replacement` on entities is accepted but undocumented in the example;
  document it. `terms` flat list duplicates entity names; keep both,
  document precedence (entities win).
- `label` mode ([LABEL_N] rampart-style) is in the mode comment but
  verify the vault actually emits it; add a round-trip test per mode.
- `disclose_text: ""` falls back to built-in; document that empty means
  default, not silent (silent = `disclose: false`).
- confirm `include_system: true` also covers the injected helpful line,
  not just client system prompts.

done: `replacement` wired (fixed surrogate, seeded into vault for
restore); entity spans tagged `custom-entity`, win overlaps via
`prefer()`; round-trip tests per mode; `include_system` covers the
helpful line (injected before masking), test
`TestIncludeSystemCoversHelpfulLine`. docs in config.yml.example.

## 7. rampart NER: wire the existing build, verify cache [large]

status: rampart IS implemented (`pii/` package: `detect.go` ports the
heuristics, `wordpiece.go` is the tokenizer, `ner_ort.go` loads the model
behind `-tags ner`, `vault.go`/`scan.go` are vault + scan cache, NOTICE
credits rampart CC BY 4.0). what remains is wiring + verification:

- confirm `-tags ner` builds and the model lazy-loads once (no per-request
  load); NER timeout (`timeout_ms`, default 500) follows `on_timeout`.
- verify the scan cache keys on paragraph hash + config version and skips
  rescan of unchanged history; add a cache-hit counter to `/status`
  (fits item 2 work).
  partial 2026-10-08: `-tags ner` builds clean; singleton loads once per
  (model, lib); bogus path degrades to nil (`TestNerMissingWeightsDegrades`
  both builds). key = sha256(ver + 0 + paragraph), `Ver: "v1"`
  (`TestScanCacheKeysOnVersion`). heuristics benchmark: `RegexUSPerKB`
  logged at startup when anonymize on (live: 574.9 us/KB, masked turn
  verified); `nim-perf.jsonl` already gitignored. left: weights + NER
  ms/request + hit counter.
- benchmark: regex us/KB + NER ms/request, log at startup when ner enabled.
- `fetch_model.sh` downloads weights; never commit them.

## 8. proxy pool hardening per the rough outline [large]

lanes exist (`zenlanes.go`) but several outline behaviors are missing:

- per-(proxy,session) lock held during the call + 2s after (like nvidia
  key postpone), so concurrent same-lane requests serialize instead of
  bursting one exit.
- two-tier refresh: full refresh every 30m (`pool_refresh_...`, new
  `pool_refresh_minutes` default 30) PLUS early refresh when all exits
  429 for >15s with no success.
- exit health check: on start and before pinning, probe must complete
  within `max_response_ms + 150ms` AND a zen probe call must not be
  rate-limited before the exit is pinned.
- lane cap default is 5; outline asks 6. change default `lanes: 5` -> `6`
  in example + `laneCap` fallback, keep user values.
  stale 2026-10-08: live default is 25 (`laneCap` fallback, example,
  live config 18-25). the outline's 5->6 predates shared-lane spread;
  do not apply. remaining: per-(proxy,session) lock, two-tier refresh
  knob, pre-pin health probe.
- overflow past cap: assign to least-recently-used lane (already does via
  shared pool fallback; verify under a 8-concurrent test).
  done 2026-10-08: `TestOverflowSpreadsConcurrently` (8 distinct hdr
  keys vs cap 4: table stays capped, lanes spread).

## 9. model_info coverage audit [large]

done (2026-10-05): `models.params` globs replaced by `model_info.jsonc`
(108 entries: 33 nvidia + 15 opencode + 7 freepi + 5 antseed + rest. 2026-10-08 recount; earlier "102" predates antseed). sampling defaults are gone;
`injectParams` only folds camelCase and strips `_nim`.

- on startup, log models in `/v1/models` output with no model_info entry
  (warn, don't fail).
- add a test that every `opencode/*` id returned by `fetchZenModelIDs`
  has a model_info entry.
- keep `inject.params: false` as the escape hatch (skips normalization);
  `_nim` strip is part of normalization.

done 2026-10-08: `missingModelInfo` in main.go warns per id from
`refreshOpencodeModels` (live check: 11/11 served ids resolve, no
warnings); test `TestZenModelIDsHaveModelInfo` (fake zen server +
controlled catalog). escape hatch verified pre-existing
(`injectParams` gate + `TestConfigToggles`).

## 10. pool tuning: calm the refresh storms [medium]

symptoms (2026-10-01 log): `pool_refresh_429s: 2` fires a full pool
refresh on 2 rate-limited lanes, and several 429s cluster within seconds
of each other, so one burst triggers 2-3 refreshes back to back. each
refresh re-probes up to `pool_size` candidates (40-way parallel verify)
and revalidates every lane exit. requests still succeed via rotation,
but the churn is wasteful.

- raise `pool_refresh_429s` default 2 -> 4: a single conversation
  bursting one exit should not refresh the whole pool; 4 distinct
  limited lanes means the pool really is stale.
- add `pool_refresh_cooldown_secs` (default 120): skip refresh when the
  last one finished less than this ago, even if the 429 count trips.
  log the skip at debug so tuning is visible.
- consider `lane_min_gap_ms` 1500 -> 2000 if single-lane bursts keep
  429ing the same exit (check: repeated 429s on one lane with distinct
  proxies = gap too tight; 429s across many lanes = pool stale).

done 2026-10-08: default 2 -> 4 + `pool_refresh_cooldown_secs`
(default 120, 0 = 120, negative disables) in config.go/example;
`noteRateLimit` returns bool, trips inside the window skip with a debug
log and reset the counter (`poolRefreshDue` is the testable decision).
test `TestPoolRefreshCooldown`. log showed 9 refreshes in ~15s before;
gap left at 1500 (no evidence single-lane bursts need 2000).
live 2026-10-08: cooldown skips firing (5 trips skipped 7s-1m54s after
last refresh, debug log). gap stays 1500: 429s spread across lanes in
the log, no single-lane burst pattern.
fix 2026-10-09: production showed 2 fires 7s apart — timestamp stamped
only on verify completion (~30s+), so a trip during an in-flight refresh
saw a stale stamp and logged a duplicate fire (collapsed via TryLock,
harmless but noisy). `noteRateLimit` now stamps at fire time;
completion re-stamps. test `TestRefreshFireStamps` (refresh mutex held,
async fire collapses, no network).

## 11. claude code fetch tool 403 FreeTierError, now 502 [medium]

symptoms (2026-10-07): in claude code, the fetch tool (and a couple
others, not all tools) historically 403 `FreeTierError`, now 502
(`opencode zen: upstream unreachable`). tool-specific, not model-wide.

- verify whether the 502 is the proxy's own failover exhausted (all
  retries transport-fail) or an upstream 502 body passed through:
  check the `!! opencode upstream 502` vs `zen error` lines for the
  failing req id and whether `dropProxy` fired for the exit.
- suspect: fetch pulls page content that trips a per-exit rule
  (long body, redirect chain) so only exits that allow it succeed;
  rotation should still land one, so a persistent 502 means the retry
  loop or hedge is giving up early on this shape.
- repro: same prompt via opencode (openai endpoint) vs claude code
  (/v1/messages) against a scratch port; compare the forwarded bodies
  (url shape, redirect following) and the upstream verdicts.
- verify: unit test for cooldown skip + counter reset; live check that
  refreshes drop to ~1 per burst in the log.

done 2026-10-08: log forensics says proxy-exhausted, not upstream
passthrough — all 5 retries reused one dead proxy (`proxyconnect:
network is unreachable` x5, same exit). root cause: net-error branch
called `dropProxy` but never cleared the lane pin, so `laneProxyFor`
re-picked the dead exit every retry. fix: `laneReproxy` after
`dropProxy` in both paths (anthropic.go, main.go); solo dead exit
still attempts (keep-last + keep-pin fallbacks unchanged). test
`TestNetErrMovesLaneOffDeadExit`; live :5420 turn 200 with 429
rotation onto a fresh exit intact.

## 14. cli flags that override config [small]

no flag parsing exists today: `os.Args[1] == "probe"` (see `parseFlags` in main.go) and
the `CONFIG_FILE` env var (see `configPath` in config.go) are the only knobs. flags are
for easy testing without editing `config.yml`.

- parse with the stdlib `flag` package at entry: `--config`,
  `--port`, `--lanes`, `--verbose` / `--debug` (log level), `--monitor`
  (wires into item 12). whatever else turns out to be needed while
  testing, keep the list short.
- flags win over config values: apply after config load, before listen /
  pool build, so `--port 5420` and `--lanes 4` are visible everywhere.
- `probe` subcommand must keep working; bare first arg `probe` stays the
  trigger (or accept flags before it — decide once, test both orders).
- no flags = byte-identical behavior to today.
- test: `--port` listens on the override, `--lanes` caps lanes,
  `--debug` flips log verbosity, defaults unchanged without flags.

done 2026-10-08: hand-rolled `parseFlags` in main.go (`--config`,
`--port`, `--lanes`, `--debug`, both `--f v` / `--f=v`, both orders
around `probe`, unknown flags ignored). applied in `serverMainFlags`
after load, before pool build/listen; `--port` wins over PORT env;
`probe` honors `--config`. no `--monitor` (item 12 skipped).
tests `TestParseFlags`, `TestFlagOverrides`; live `--lanes 4` shows
cap 4 on scratch :5420.
plus: `--help`/`-h` prints usage and exits (`TestHelpFlag`).

## 12. perf monitoring: stage timings + resource sampling [large]

optional monitor writing jsonl to its own file, mirroring the
`logUsage` pattern (usage.go:56): gated on `monitor.enabled` (default
false), zero cost when off, one line per record.

per-request rows (wall clock, ms):
- `total`: request accepted -> response fully written to the client.
- `proxy_in`: accept -> upstream request dispatched (pii, guardrails,
  conversion, lane pick — everything the proxy does before the wire).
- `upstream_ttfb`: dispatch -> first upstream byte.
- `ttft` (streaming only): dispatch -> first content delta written to
  the client.
- `model_tps`: completion_tokens / (last delta - first delta).
- `upstream_total`: dispatch -> upstream body finished.
- `drain`: upstream finished -> client write finished (sse rewrite +
  flush time).
- `proxy_overhead`: total - upstream_total (the "how much does the
  proxy cost" number).

operation rows (once per call):
- startup phases: config load, model_info load, pool build + proxy
  sort/verify, shared-lane pre-open, listen.
- pool refresh, proxy verification, lane create/evict/rotate, model
  fetch (`fetchZenModelIDs`).

resource rows: rss, cpu %, goroutine count (threads), process net bytes
in/out. sampled on a timer — default `interval_ms: 1000`, document
500-2000 as the sane range; 1s catches refresh storms without drowning
the file, 0.5s if a burst needs resolving. rss/cpu via runtime +
getrusage, no new deps. also attach an rss snapshot to each request
row so a slow request can be correlated with memory pressure.

- config: `monitor.enabled` (false), `monitor.path`
  (`nim-perf.jsonl`), `monitor.interval_ms` (1000). document in
  `config.yml.example` + defaults in `defaultConfig()`; default-false
  bool, so no raw-doc presence check needed. `--monitor` flag from
  item 11 as the quick toggle.
  done 2026-10-08: keys + example + `--monitor` flag (`TestMonitorFlag`).
  rows not yet written (the large remainder).
- verify: unit test that disabled monitor writes nothing and that
  `proxy_overhead = total - upstream_total` on a fake timeline; live
  check a streaming request row carries `ttft`, `model_tps`, and rss.
- no `/status` section unless someone asks for one (that would need a
  `status.show_*` flag + `StatusFor` redaction per AGENTS.md).

## 13. zen: stop handing the client retryable failures while exits remain [medium]

symptoms (2026-10-07 log): opencode shows "failed to process request"
while the log shows rotation working. every failure 429-rotates, but
after 5 attempts (`max_retries: 4`) the loop falls through and writes
the last upstream status to the client: req-256 got `<- 429` with
0 bytes after 1m5s, req-305 the same after 17s. two compounding bugs:

- the 429 path (see the 429 branch in `handleOpenCode` in main.go) closes the body then `continue`s, so the
  post-loop code re-reads a closed body and forwards an empty 429. the
  client sees a bodyless 429 and gives up.
- `hedgedDo` (zenlanes.go) lets a fast 429 win: the primary 429s in
  200ms, the hedge is cancelled, a success 1s behind it is thrown away.

one open question: several `dial tcp 47.90.229.219:443: refused` lines
name a dial target that is not the logged proxy, so a primary-side vs
hedge-side mixup can't be ruled out yet.
resolved 2026-10-11: with per-side proxy labels live, every dial target
matches its logged proxy (primary and hedge alike). socks4 dials show
the exit ip directly, http-connect shows the proxy — no confusion.

- track the last failure kind across the loop (`handleOpenCode`,
  `handleOpenCodeAnthropic`, `zenPost`): retryable (429/529, net error)
  vs terminal (anything else). a retryable tail enters a bounded drain
  after the loop: fresh exits via `pickLaneProxyFor` skipping
  already-tried ones, fresh session per try, bound by client ctx plus a
  new `zen.retry_drain_secs` (default 0 = off, current behavior).
  terminal errors keep today's path.
- hedge: a retryable-status winner waits for the other side (same
  budget) instead of cancelling it. first success wins; both-retryable
  falls into the drain.
- log hedge-side errors with their own proxy label, so the dial-target
  mismatch is answerable next time.
  done 2026-10-08: both hedge funcs log `hedge error` with own proxy
  (skipped when cancelled, so only real failures show).
- never forward an empty upstream error: synthesize JSON with 502/503
  + `Retry-After` on the retryable-exhausted path instead of the stale
  429. check what opencode retries on and match it.
  done 2026-10-08: `exhausted429` (503 + Retry-After: 30, non-empty
  JSON) wired into both 429 branches on last retry; falls through to
  the normal forward. test `TestExhausted429Synthesizes`.
- test: fake upstream 4x429 then 200 -> client 200.
  done 2026-10-08: `TestRetryLoopSurvives429s` (fake zen, 5 hits, 200).
- the refresh-storm half of this log (2-3 refreshes per burst) is
  already item 10's; this item is only the client-facing tail.
- test: fake upstream 4x429 then 200 -> client 200; all-429 -> JSON
  502/503 with a non-empty body, never a bare 429; fast-429 vs
  slow-200 hedge -> 200 wins.
  done 2026-10-09: `TestRetryLoopAll429Synthesizes` (OAI path: 503 +
  Retry-After + non-empty body through the real client write) and
  `TestAnthropicPathAll429Synthesizes` (same on /v1/messages). left:
  drain + hedge-wait (the involved half). note: fast-429-beats-slow-200
  is current `hedgedDo` behavior by design (first response wins);
  changing it IS the hedge-wait rework, do not "fix" without it.

## left open

- [ ] live restart of the primary (:5419). deliberately left to the user.

## done — lanes, ttft, proxy rotation (was IMPROV_TODO.md)

one line per item, checkbox marks state.

### done

- [x] measure zen ttft for space-bunny and muse-spark through the proxy (6s
      sbunny, 403 in 12s on muse) — root cause was not ttft alone, see below
- [x] `lane_hdr` / `lane_convo` / `lane_shared` / `lane_cache_key`
      independent booleans in config.yml zen section (`config.go` *bool so
      absent = default)
- [x] convo off by default with a config disclaimer saying why (hash drifts
      as history grows, one-shots pin lanes)
- [x] shared pool pre-opens `lanes`-count lanes at startup AND on first
      request (`ensureSharedLanes`, `ensureSharedLanesLocked`). verified:
      a cold test instance showed 25 shared lanes immediately, not one lane
      built on demand
- [x] shared lanes actually spread. `lastUsed` is stamped on hand-out, not
      only on success, so concurrent requests no longer all draw the same
      coldest lane. 24 concurrent headerless requests now hit 8 distinct
      lanes (was 1)
- [x] hdr/convo/prompt_cache_key arriving at a full table converts the idlest
      shared lane instead of piling onto one fallback (`convertSharedLocked`)
- [x] no cooldowns at all: 429/geo/block rotate exit+session instantly
      (`laneRotate` mints a new `ses_*` + picks a fresh exit)
- [x] `lane_cooldown_secs` / `lane_wait_max_secs` / `probe_timeout_ms` /
      `race_timeout_ms` marked unused in the example so nobody tunes them
- [x] rate-limited >15s with no global success -> pool refresh + session
      rotation (`maybeRefreshStalePool(lane)`, `stale_pool_secs`)
- [x] deadcode removed: `laneCool`, `laneCoolSoft`, `laneBlock`, `laneWait`,
      `laneHealthy`, `firstByteDo`, `laneCooldownBase`, `probeTimeout`,
      `raceTimeout`, `raceProbe`
- [x] no hot-path exit probe or parallel race (was 1s probe + 3.5s race on
      every pick) — pool verified on refresh, dead exits die on real sends
- [x] faster exits weigh more: live first-byte rtt ewma (`noteExitRTT`) mixed
      into `proxyWeight` with api-reported latency + reliability
- [x] freepi = one lane per account already (`freepiAcct.inflight` gate, one
      jwt = one live stream); nvidia = one key per request, direct, no proxy.
      backends never share lane state — confirmed, no change needed
- [x] cancel propagates upstream: `r.Context()` on every upstream request
      (zen oai, zen anthropic, nudge, nvidia `callUpstream`, freepi
      `freepiTry`/`freepiFetch`), `semAcquire(ctx)`, `laneGate(ctx)`,
      `zenBackoffCtx`, hedge both sides ctx-derived, and the stream copy
      loops abort on ctx — extracted as `copyZenChatStream` so the check
      lives in one place instead of three inline copies
- [x] true streaming for `/responses` (was fully buffered before first byte):
      `streamResponsesLive` (oai) + `streamResponsesLiveAnthropic`
- [x] `prompt_cache_key` pins a lane on the openai-compat endpoint (read
      before `stripCacheFields` deletes it upstream). anthropic-side
      `cache_control` idea was retracted, not implemented
- [x] low-capacity dual-lane race, optional, configurable, documented as
      costing two upstream inferences: `lane_race_enabled`,
      `race_utilization_threshold` (30%), `race_use_window_minutes` (10),
      `race_hedge_after_ms` (0). rides the existing hedge machinery via
      `hedgeDelay()`, so a race is a hedge with a 0ms delay rather than a
      second mechanism. off by default
- [x] example config synced with live config, all lane toggles + race keys
      documented with the usage disclaimer
- [x] live config: `lane_hdr: true`, `lane_shared: true`,
      `lane_convo: false`, `lane_cache_key: true`, `lanes: 18`,
      `lane_race_enabled: false`
- [x] AGENTS.md updated for the no-cooldown rotation model
- [x] cost row in `nimstatus.py` present again after an accidental revert
      during this work
- [x] fixed early stream cutoff (hedge winner's context was canceled on
      return, killing the body mid-stream): `zenSide` hands the cancel to
      `bodyCancelCloser` so the winner streams on and frees ctx on close
- [x] regression tests for the cutoff: whole-stream delivery, cancel stops
      the copy, live streamers carry every delta including tool calls, and
      byte-at-a-time reads reassemble (`stream_cancel_test.go`)

### verified end to end (scratch config, port 5420, then torn down)

- [x] anthropic `/v1/messages` streaming: 275 words, `end_turn`, `[DONE]`
- [x] anthropic `/v1/messages` non-streaming: content present, one body
- [x] oai `/v1/chat/completions` streaming: full body, finish `stop`
- [x] tool call: `Bash` emitted as `tool_use`, client spelling restored,
      args streamed, `tool_use` stop reason
- [x] multi-turn: tool result fed back produces a text answer, `end_turn`
- [x] 8 concurrent streams: 8/8 complete, all correct, spread across lanes
- [x] 25 shared lanes present at startup, distinct exits per lane
- [x] lane race: fires when idle ("busy share 4% < 30%"), responses still
      complete; silent when disabled
- [x] `/health`, `/v1/models`, `/v1/messages/count_tokens`, `claude-*`
      mapping all 200
- [x] (2026-10-07 sweep, `opencode/*` scope) all 13 non-deepseek opencode models swept on `/v1/messages`. **1 of 13 passes:
      space-bunny-free.** the other 12 fail, and the failure is upstream, not
      the proxy — evidence: (14 non-deepseek opencode ids exist now; conclusion predates freepi/antseed backends)
  - 56 distinct exits attempted across the sweep; every other model drew
    403 `FreeTierError` or 429 on all of them
  - space-bunny hit **zero** blocks on those same exits in the same run and
    answered `end_turn` + `[DONE]` on request both before and after the sweep
  - log tally: 32 free-tier-blocked, 46 rate-limited, 1 x 200
  - the lane/rotation machinery is provably working (it rotates on every
    one of those failures and lands on a distinct exit each time); it just
    cannot outrun a per-exit-per-model gate
  - only exception needing its own look: `jev-1.13-free` returns upstream
    **500** `{"type":"error","message":"Internal server error"}` rather than
    403, i.e. the provider 500s on it. not a proxy defect either, but worth
    remembering if jev starts 500ing on a good exit
  - so: only space-bunny-free is usable right now. the rest are listed in
    `/v1/models` but are effectively dead upstream. not something rotation
    can fix.

### not done / done-later (from the original "not done" section)

- [x] **tokens served per lane and per model, since process start.**
      `zenLane.tokens` is a `map[string]laneTokens` (requests, prompt,
      completion, total) held on the lane object, not keyed by lane key, so
      totals survive `laneRotate` (new session, same lane) and
      `convertSharedLocked` (key renamed, same lane). `laneTouch` counts
      the request at 200-time; `laneTokensAdd` folds the token counts in
      once the body is parsed, since upstream reports usage in the stream
      tail. `/status` lanes gained `prompt_tokens` / `completion_tokens` /
      `total_tokens` plus a `by_model` map. rides the existing `show_zen`
      flag and `StatusFor` redaction, no new flag needed. in-memory, resets
      on restart, as asked. `nimstatus.py` shows a tok column (top model
      appended when a lane served more than one) and a `tok served` line.
- [x] found and fixed why the counters read zero: `initUsageLog()` ran
      before `applyConfig`, so `usage.path` / `usage.enabled` came from an
      unloaded config and the jsonl opened at the wrong path (or not at
      all). moved after `applyConfig` in `serverMain`, and after the config
      load in `runProbe`.
- [x] found and fixed why anthropic streams reported zero tokens:
      `streamAnthropic` returns on `finish_reason`, but zen sends usage in
      a chunk *after* it. the body is now scanned up front via the existing
      `sseUsageTee`, so both the usage record and the per-lane totals carry
      real counts. the non-stream fold was already correct. verified live:
      two turns recorded 1040 and 1298 tokens, 2338 total on one lane with
      the per-model breakdown.
- [ ] live restart of the primary (:5419) — moved to "left open" above.

### notes

- the "model says part of a thing then stops and runs no tools" symptom was
  reproduced once and turned out to be upstream `max_tokens` truncation on
  reasoning-heavy prompts, not a streaming bug: space-bunny spends its
  budget on `reasoning_content` and returns zero visible content at low
  `max_tokens`. with headroom it answers fully. worth remembering before
  chasing it again.
- muse-spark and most other zen free models now 403 `FreeTierError` on
  essentially every exit in the pool (see the model sweep above). known,
  per-exit per-model: rotation cannot fix a model-level gate, and only
  space-bunny-free is currently serving.
- `hedgedDo` winner must keep streaming. any change there needs a test that
  asserts the body still yields bytes after it returns — that is the bug
  that broke real sessions.
- per-lane token totals read 0 for turns that hit `max_tokens` before
  upstream emitted a usage chunk. that is upstream behaviour, not a counter
  bug: a prompt short enough to reason briefly reports nothing. check
  `usage` totals alongside the lane numbers before chasing it. the real
  zero-token bug (usage after finish_reason) is fixed and listed above —
  distinguish the two before spending time on it.
- `models.params` was refactored out of config.yml into `model_info.jsonc`
  + `modelinfo.go` while this work was in flight, committed alongside mine.
  checked: it is deliberate and documented (AGENTS.md line ~69 says
  `injectParams` only folds camelCase and no sampling defaults are injected
  any more). `nudge_no_tools`, `context_length` and the thinking→effort
  mapping all read model_info now. no action needed. noting it only because
  the change landed in the same commits as the lane work and is easy to miss
  when reading the log.
