# improvement todo — lanes, ttft, proxy rotation

one line per item, checkbox marks state. ordered by priority.

## done

- [x] measure zen ttft for space-bunny and muse-spark through the proxy (6s
      sbunny, 403 in 12s on muse) — root cause was not ttft alone, see below
- [x] `lane_hdr` / `lane_convo` / `lane_shared` independent booleans in
      config.yml zen section (`config.go` *bool so absent = default)
- [x] convo off by default with a config disclaimer saying why (hash drifts
      as history grows, one-shots pin lanes)
- [x] shared pool pre-opens `lanes`-count lanes at startup
      (`ensureSharedLanes`, called from `refreshZenProxies`)
- [x] hdr/convo arriving at a full table converts the idlest shared lane
      instead of piling onto one fallback (`convertSharedLocked`)
- [x] no cooldowns at all: 429/geo/block rotate exit+session instantly
      (`laneRotate` mints a new `ses_*` + picks a fresh exit)
- [x] `lane_cooldown_secs` / `lane_wait_max_secs` / `lane_probe_timeout_ms`
      marked unused in the example so nobody tunes them
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
      `zenBackoffCtx`, hedge both sides ctx-derived, stream copy loops abort
      on `r.Context().Err()`
- [x] true streaming for `/responses` (was fully buffered before first byte):
      `streamResponsesLive` (oai) + `streamResponsesLiveAnthropic`
- [x] example config synced with live config, lane toggles documented
- [x] live config set to `lane_hdr: true`, `lane_shared: true`,
      `lane_convo: false`, `lanes: 18`
- [x] AGENTS.md updated for the no-cooldown rotation model
- [x] tests updated for the new signatures, 163 passing
- [x] fixed early stream cutoff (hedge winner's context was canceled on
      return, killing the body mid-stream): `zenSide` hands the cancel to
      `bodyCancelCloser` so the winner streams on and frees ctx on close

## partially done

- [~] shared lane spread. pre-opens and picks lru, but lru picks the
      same lane back when it is the coldest, so one client can monopolize a
      lane while others stack up. want round-robin-ish spread across all
      `lanes` entries (oldest-last-used first, then bump), verified by test
- [~] prompt_cache_key as an identity key. `prompt_cache_key` /
      `promptCacheKey` are currently only stripped for zen upstream
      (`stripCacheFields`) and never read as a lane identity. user asked to
      use it as the hdr substitute on the openai-compat endpoint. note the
      anthropic-side `cache_control` idea was retracted by the user
- [~] cost row in `nimstatus.py`. `cost` column code is present again after
      an accidental revert during this work, but the rendered table has not
      been eyeballed since

## not done

- [ ] low-capacity dual-lane race. when recent lane utilization is below a
      threshold, fire the request on two lanes at once and keep whichever
      gives a first byte sooner. optional + configurable in config, with a
      disclaimer that it burns upstream usage (two inferences per request).
      needs: rolling utilization tracker, first-byte-race winner selection,
      config keys, tests
- [ ] full end-to-end verification on a separate port (:5420 + scratch
      config): both `/v1/messages` and `/v1/chat/completions`, streaming and
      non-streaming, tool calls, multi-turn, long responses, client
      disconnect mid-stream. must show complete responses, no early cutoff
- [ ] live restart of the primary after the above passes

## notes

- lane capacity notion for the dual-lane race: use lane `requests` counters
  over a rolling window rather than in-flight count, since in-flight is
  near-zero between turns.
- the `hedgedDo` winner must keep streaming. any change there needs a test
  that asserts the body still yields bytes after `hedgedDo` returns.