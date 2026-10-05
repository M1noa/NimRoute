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

## not done

- [ ] **tokens served per lane and per model, since process start.**
      right now `laneStatus` carries `requests` + `rate_limited` only, and
      `usageTotals` is keyed by model and by KeyName (freepi account / nvidia
      key), never by lane. what is wanted: in-memory counters for tokens
      served, broken down by lane and by model, covering everything since
      the proxy started (not the rolling jsonl). shows in `/status` and the
      `nimstatus.py` lane table. notes:
      - lane ids are ephemeral (`shared:...` / `hdr:...` / `pck:...`), and
        `laneRotate` mints a new `ses_*` on the same lane object, so keying
        on the lane key means a converted lane leaves its old totals behind.
        keying on the `*zenLane` pointer keeps them across rotation, and
        sidesteps the question for lanes that get swept.
      - decide whether totals follow a lane across `convertSharedLocked`
        (renames the key, same object) and across `sweepLanes` (drops the
        lane). keeping them on the pointer answers both for free.
      - per-model needs a map on the lane, so `laneTouch` needs the model
        and token counts passed in. `laneTouch(lane, proxy)` is the single
        success hook on the zen path, so that is the one call site to widen.
      - in-memory only (resets on restart), matching the ask. if it should
        survive restarts, the jsonl already has the raw rows.
      - needs a `status.show_*` flag per the /status rules, plus redaction
        in `StatusFor`.
- [ ] live restart of the primary (:5419). deliberately left to the user.

## notes

- the "model says part of a thing then stops and runs no tools" symptom was
  reproduced once and turned out to be upstream `max_tokens` truncation on
  reasoning-heavy prompts, not a streaming bug: space-bunny spends its
  budget on `reasoning_content` and returns zero visible content at low
  `max_tokens`. with headroom it answers fully. worth remembering before
  chasing it again.
- muse-spark still 403s `FreeTierError` on most exits. known, per-exit
  per-model, rotation cannot fix it.
- `hedgedDo` winner must keep streaming. any change there needs a test that
  asserts the body still yields bytes after it returns — that is the bug
  that broke real sessions.