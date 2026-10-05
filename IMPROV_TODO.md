# improvement todo — lanes, ttft, proxy rotation

one line per item, checkbox marks state. ordered by priority.

## done

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

## verified end to end (scratch config, port 5420, then torn down)

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
- [x] all 13 non-deepseek models swept on `/v1/messages`. **1 of 13 passes:
      space-bunny-free.** the other 12 fail, and the failure is upstream, not
      the proxy — evidence:
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

## not done

- [x] **tokens served per lane and per model, since process start.**
      `zenLane.tokens` is a `map[string]laneTokens` (requests, prompt,
      completion, total) held on the lane object, not keyed by lane key, so
      totals survive `laneRotate` (new session, same lane) and
      `convertSharedLocked` (key renamed, same lane). `laneTouch` counts the
      request at 200-time; `laneTokensAdd` folds the token counts in once the
      body is parsed, since upstream reports usage in the stream tail.
      `/status` lanes gained `prompt_tokens` / `completion_tokens` /
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
      `streamAnthropic` returns on `finish_reason`, but zen sends usage in a
      chunk *after* it. the body is now scanned up front via the existing
      `sseUsageTee`, so both the usage record and the per-lane totals carry
      real counts. the non-stream fold was already correct. verified live:
      two turns recorded 1040 and 1298 tokens, 2338 total on one lane with
      the per-model breakdown.
- [ ] live restart of the primary (:5419). deliberately left to the user.

## notes

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