# AGENTS.md — contributor guidance for LLM coding agents

Module `nimroute`. NimRoute is an OpenAI/Anthropic-compatible LLM
proxy/router in Go. Primary purpose: pool free-model access with
key/proxy/lane rotation to survive per-exit rate limits.

## 0. purpose

The point of this project is one thing: proxy requests fast,
transparently, and seamlessly — get around rate limits, keep every free
model working, hand errors straight through to the caller, make the user
feel like it just works so they can go about their day. That is the goal;
nothing else is structural.

Everything else in the tree is incidental: leftover scaffolding, awkward
flows, historical workarounds. None of it is mandatory, deliberate, or
"the shape of the project" — it is just how things landed. When touching
anything, fix the handling rather than perpetuating its pattern, improve
bad code as you meet it, and never assume pre-existing behavior was
chosen on purpose. The quality bar is measured on the request path
(speed, honesty of errors, model availability), not on preservation of
incidental machinery.

## 1. backends and routing

- **opencode zen**: free `opencode/<model>` ids, keyless. upstream
  `https://opencode.ai/inference/v1` (the old `/zen/v1` path now answers
  403 FreeTierError for everyone, real client included). every request
  mimics the real opencode client (`setZenHeaders` in anthropic.go):
  `Authorization: Bearer public`, `x-opencode-client: cli`,
  `x-opencode-session: ses_*` + `x-opencode-session-id` (same descending
  id) + `x-session-affinity`, `x-opencode-project: global`,
  `User-Agent: opencode/<version> ai-sdk/provider-utils/4.0.23
  runtime/bun/1.3.14` (live tag from anomalyco/opencode releases — the
  repo moved off sst/ — refreshed every 6h, default in zenproxy.go).
  `x-opencode-request` carries a fresh descending-id message id per
  attempt (`zenMessageID`), as the installed 1.19.x client does.
  (`anthropic-version` is NOT
  sent; an earlier doc revision claimed it.) muse-spark models use
  `/responses`, everything else `/chat/completions`; shape follows the
  model, converted both ways (`convertToResponses` / `chatFromResponses`).
  the free-tier gate fingerprints the body's tools array (verified live:
  the system prompt is unconstrained once `ensureZenTools`' embedded
  opencode toolset — `zenOCDefsJSON` in zentools.go, the 11 tools the
  1.19.3 CLI sends (bash, edit, glob, grep, read, skill, task, todowrite,
  webfetch, websearch, write) — is present; the previous 12-tool set
  (shell/subagent/execute) is stale and 403s FreeTierError). zen also
  enforces a per-IP reputation check and ~50-100 req/day cap (~1.5s
  between requests): GET `/models` does not run the IP reputation gate, so
  `verifyProxy` probes `POST /chat/completions` with an invalid message
  role (`verifyProbeBody` — 0 tokens/0 inference cost, returns 400 when
  the IP passes the gate and 403 FreeTierError when the IP is blocked or
  daily-capped). a working exit stays pinned to its lane across turns
  until it actually hits a 429 or 403 cap (`noteExitGate` parks + drops
  the exit and evacuates its lane onto a fresh one). upstream tool call names pointing at the ballast map back
  via `zenToolAliases` in `toolNames.resolve`.- **freepi**: ad-supported `freeepi/<model>` ids via `https://api.freepi.ai`.
  one live stream per account jwt (server 409s a second); accounts rotate
  on failure, queue up to `freepi.queue_wait_secs` (default 60s). jwt only
  ever rides as a request header, never logged, never in `/status`. ads run
  silently per account (banner + inline impressions). a rate limit is never
  forwarded: `hourly_ceiling` is not retryable (waiting on the queue cannot
  clear an hourly cap, so it fails fast and lets siblings take their shot),
  and every exhausted-account limit becomes a 503 naming the cap
  (`freepiClientStatus`).
- **antseed**: p2p `antseed/<model>` ids via dialed sellers (stats snapshot
  discovery, DHT fallback). keyless: identity minted fresh in memory per
  run. snapshot addresses are untrusted claims: `antEndpointOK` rejects
  non-global-unicast/private/loopback before dial. only retryable statuses
  cool a seller; 400s fail fast without parking it.
- **Nous Portal**: free models from `https://inference-api.nousresearch.com/v1`
  via `nous/<model>` ids. openai-compatible chat/completions, bearer api key,
  keys in `nous_keys` (separate pool from `nvidia_keys`, same map shape).
  own key pool in `nous.go`; a 429 cools one key and rotates, and when every
  key is cooling the portal's own 429 reaches the client rather than a
  synthesized 503. only the zero-priced catalog is served and listed: the
  filter is `pricing.prompt == 0 && pricing.completion == 0`, NOT the
  `:free` id suffix (`inclusionai/ling-3.1-flash`, `meituan/longcat-2.0`,
  `stealth/missingno` are free with no suffix; no paid id carries one).
  both directions must be zero or the embedding models (completion 0, prompt
  > 0) leak in. catalog cached 6h, last good list kept on fetch failure.
  x402 (solana usdc, keyless pay-per-request) is NOT implemented — api key
  only.
- **NVIDIA NIM**: upstream `integrate.api.nvidia.com`, needs
  `nvidia_keys` in config.yml. `nvidia/` prefix is listing-only, stripped
  before forwarding. no keys (or `nvidia.enabled: false`) = NIM 503s,
  `opencode/*` keeps working.
- `ServeHTTP` (main.go): `/status` + `/health` -> `StatusFor`;
  `/v1/models`; `/v1/messages`, `/v1/messages/count_tokens`,
  `/v1/messages/classifier`; `freeepi/` -> freepi, `antseed/` -> antseed,
  `opencode/` -> zen, `nous/` stripped, `nvidia/` stripped, else NIM.
  both dispatch tables (ServeHTTP and `handleAnthropic`) carry the prefix;
  a new backend needs a branch in each. `backendForModel` (debugtrace.go)
  needs a case too or traces mislabel the backend.
  inbound bodies capped at 10MB (`bodyLimit`). `route/` is handled in
  `route.go` (dispatch sites in `main.go` and `anthropic.go`). service via
  `./nim run|start|stop|restart|status [tail]|status-tail|logs|log [N|tail]|tail|models [N]|models sync|install|uninstall|freepi auth|help`
  (docs/service.md is the user-facing copy).

## 2. code quality rules

- smallest working diff. surgical edits: every changed line traces to the
  request. never "improve" adjacent code, comments, or formatting.
- no speculative scaffolding: no features beyond the ask, no single-use
  abstractions, no configurability nobody requested, no handling for
  impossible states.
- clean up only your own mess: drop imports/vars/functions your change
  orphaned. mention pre-existing dead code, don't delete it.
- comments: short, all lowercase. match neighboring style.
- behavior changes get verified end to end on a scratch port (reproduce
  first for bug fixes). the repo carries no Go test files as of the
  2026-10-15 cleanup; git history holds the old suite if one is needed.
- green before commit: `gofmt -l .`, `go vet ./...` (and `go vet -tags
  ner ./...`), `go build ./...`.
  ner build is separate: `go build -tags ner`.
- never interrupt the primary instance (:5419 live). test on a separate
  port with a scratch config until the change is verified end to end:
  copy `config.yml` to `config.test.yml`, set `server.port` to an unused
  port (e.g. 5420) and `usage.path` to a scratch file, then
  `CONFIG_FILE=config.test.yml ./nimroute`. only restart the primary
  (`./nim restart`) when done and confident. delete scratch configs,
  binaries, and usage files after.

## 3. config rules

- `config.yml.example` is the schema source of truth. `defaultConfig()`
  (config.go) holds runtime defaults; the two must agree.
- true-by-default bools need no special handling in `loadConfigFile`: it
  starts from `defaultConfig()`, and yaml.v3 leaves an absent or null key
  untouched, so only an explicit `false` flips a default. the old raw-doc
  presence checks were no-ops and were removed (2026-10-15);
  `TestLoadConfigFileEnabledDefaults` pins the invariant.
- new keys: document in the example with a comment, give a default in
  `defaultConfig()`, wire the presence check if default-true.
  `mergeMissingKeys` is append-only commented (`# added:` header,
  idempotent); never wipe user values, malformed user yaml keeps the old
  running config.
- new `/status` fields need a `status.show_*` flag plus redaction in
  `StatusFor` (section 4).
- model facts (ctx, description, maxout, nudge flags) live in
  `model_info.jsonc`, loaded by `modelinfo.go`, hot-reloaded with the
  config. `injectParams` only folds camelCase and strips `"_nim"`; no
  sampling defaults are ever injected. new models need an entry or they
  fall back to 131072 ctx + derived name (docs/TODO.md item 9: startup warn
  for unmatched `/v1/models` ids).
- usage tracking (`usage.go`): gated on `usage.enabled`, one json line
  per request to `nim-usage.jsonl` plus in-memory totals for `/status`.
  counters only move through `logUsage`; zero overhead when disabled.
  no local tokenizer exists anywhere: token counts are upstream `usage`
  verbatim (prompt/completion/total). never add one.
  `nimstatus.py` reads the jsonl for today/all-time display.

## 4. /status rules

- every new counter/section goes behind a `status.show_*` flag (default
  preserves current output). follow `poolSnapshot` + `show_pool` as the
  pattern: `Status()` builds, `StatusFor()` gates.
- auth redaction lives in `StatusFor`: when `auth.tokens` is set and the
  request is unauthenticated, hide keys, locks, zen session/proxy,
  lanes, lanecap, pool, freepi, antseed, nous, and usage — no matter what
  the flags say. model ids stay visible (always safe). every section needs
  its own `status.show_*` flag (`show_freepi`, `show_nous` included).
- never widen unauthenticated output without a flag defaulting off.

## 5. upstream error guidance

| signal | meaning | action |
|---|---|---|
| 429 | per exit+model rate limit. instant exit+session rotation (`laneRotate`), no cooldown waits, and never forwarded: the ladder retries across lanes (`max_retries` default 15), extends itself when the pool itself is written off (below), and only then ends in a `proxy_exhausted` 503. 2 in a row or 6 a cycle marks that pair `rate_limited` for the cycle (`noteExit429`), cleared by the next refresh. the mark is per model because zen limits the pair: an exit busy on one model keeps serving the rest | normal, no fix |
| 403 FreeTierError, "current network"/"abusive traffic" | the exit IP hit its daily cap or was flagged (`zenGateIPBlocked`). parked for `exit_burn_minutes` (`noteExitGate`) and dropped from the pool; the lane rotates onto a fresh exit and the request still succeeds | normal, no fix — this is the everyday churn: ~50-100 requests per IP, then move to the next one |
| 403 FreeTierError, "can only be used from within OpenCode" | the request shape itself was refused, so the IP is fine. burns that exit+model for the cycle (`noteExitModelBurned`) and rotates, but never parks the exit | fix the shape (headers/tools/stream), not the pool. do NOT inject gate stubs into tool-less bodies — the summarizer emits calls zen rejects ("tool call not allowed while generating summary"); `ensureZenTools` skips empty `tools` deliberately |
| 502/503/504 from an exit | dead exit or a proxy in front of it failing (`zenRetryableStatus`). rotated like a 429, also never forwarded | normal noise, no fix |
| 403 geo (`not available in your country`) / `[user_blocked]` | exit banned or region-blocked. `dropProxy` + failover; threshold (`error_threshold`, default 3) refreshes pool | normal, no fix |
| 502 / transport error | dead exit, dropped from pool (`dropProxy`, transport closed) | normal noise, no fix |
| 529 | treated as 429 in both NIM and zen paths | normal, no fix |
| persistent all-exit failure | no success for `stale_pool_secs` (default 15s) with a lane limited the whole time = stale pool/sessions: refresh + `resetStaleLanes` | fix the refresh/rotation path, not the retry loop |

client contract: no zen response ever carries a rate limit. every 429/529
an exit returns is absorbed by rotation, and a spent ladder answers
`exhaustedExits` — a `proxy_exhausted` 503 naming the pool state — so the
caller retries against us instead of waiting us out. `ProxyUnavailableMsg`
is the shared text for the other "could not place this request on an exit"
exits (dial-dead, no pin under `always_proxy`, empty pool).

two rules keep that 503 rare, both in zenlanes.go:
- no lane ever sends without an exit. a lane that draws none waits
  (`laneProxyWait`) while `dryPoolRefill` sweeps, then hands the request to a
  lane that owns one; it never falls through to direct under `always_proxy`
  or on an escalated lane (`laneNeedsProxy`). a pin collision re-pins
  (`laneTouch` → `laneReproxy`) rather than leaving the lane pinless.
- a spent ladder asks whether the *pool* was written off, not just the model
  (`marksOut`: half the serving exits marked 429/dead). if so it forces a
  sweep that clears the marks and buys extra hops (`ladderExtend`), so the
  request rides fresh exits instead of 503ing. bounded by one per-request
  `poolRecoverWait` (20s) budget, shared with the dry-pick wait, so a truly
  dead pool still fails honestly instead of stalling. a merely busy model
  (mostly clean pool) does not extend, and never waits.

exit states live in one registry (`exitState` in zenlanes.go):
`rate_limited` keyed by exit+model (2 in a row or 6 a cycle, expires at the
refresh), `dead` (transport/tls, `deadTTL`), and `gate_parked` (per exit IP,
the free-tier daily cap). rate/dead are cleared by
`clearExitRateLimits` when a sweep completes, and *only* there: a dry pick
must never clear marks. it used to, which put the exits that had just 429'd
straight back at the front of the rotation and made the same few exits
ping-pong between limited and usable while the pool looked permanently
dry. instead, a dry pick fires a fresh sweep (cooldown-gated, skipping the
current exits) and hands out the least-recently-marked one
(`secondChancePick`). the one exception is a pool that can serve nobody at
all (`poolUnusable`: empty, or every member dead/parked/limited): then
every mark is dropped (`clearAllExitMarks`) and the sweep re-probes the
whole list, live pool members first (`recheckEveryExit`, `zenRefreshFull`).
marks are what a sweep skips, so keeping them there is what left a dead
pool dead; the probe still decides who comes back.

pool sizing is what keeps that path rare: one sweep aims for 8 exits per
lane (`poolTarget`, floor 24) and stops as soon as it holds them, so the
pool is bigger than the lane table and rotation has spares — each exit is
only good for ~50-100 requests, so a thin bench turns a busy morning into a
parked pool. a pool whose every exit is parked answers the 503 immediately
(`poolAllGateParked`) instead of waiting out the 30s recovery budget. `max_response_ms`
is a coarse pre-filter on the list's own noisy figure, not a latency
promise — tightening it is what starves the pool (only ~1 in 100 candidates
dials at any claimed speed, and the list's own figure tops out around
6.5s, so 10000 admits the whole file). `pool_size` caps candidates per
sweep; 0 (the default) dials the filtered list and fans out to 200 verify
workers, which is what an uncapped pool needs to finish in under a minute.
lanes are capped at the pool size (`laneCapFor`) so every lane can hold
its own exit; a 30m refresh
(`refresh_interval_minutes`) refills the table and re-pins idle lanes
(`ensureLanePins`). the sweep fetches `proxyListURL` and falls back to the
same json straight from the repo (`proxyListFallback`) when the site is
unreachable — a home line that blocks the domain (comcast advanced security
serves a block page for it) would otherwise leave the pool empty forever.
configured `zen.proxies` are still preferred, but the picker falls through
to the pool once they are all taken/dead (`pickLaneProxyExcluding`), and a
transport failure marks a configured exit dead like a pool exit — bare
`dropProxy` is a no-op for one, so it used to be re-picked on every
request.

Investigate only when rotation stops working: same rate-limited exit
reused, sessions never refreshing, refresh storms (see docs/TODO.md item
10: raise `pool_refresh_429s`, cooldown between refreshes before touching
retry logic).

## 6. skills

- writing docs, readmes, or reports: invoke the `anti-slop` skill first.
- auditing for vulns/secrets: call the `security-audit` skill, if
  installed; otherwise say so and do a manual pass.

## 7. landmines

- lanes key on `x-session-id` verbatim (`"hdr:" + headerSession` in
  `laneFor`). never normalize, hash, or trim it lane-side; pii session
  derivation (`piiSessionID`) is separate. lane identity modes
  (`lane_hdr`/`lane_convo`/`lane_shared`/`lane_cache_key`) toggle
  independently, convo off by default (buggy hash); shared pre-opens
  `lanes`-count lanes at startup. exception: `cacheKeyOf` trims whitespace
  off `prompt_cache_key` (json padding is not identity). `/status` lane ids
  derived from client headers are sha256-truncated, never echoed verbatim.
  limits rotate exit+session instantly (`laneRotate`), never a cooldown.
  conversions allocate a fresh lane and drop the idlest shared one;
  in-place lane renames are forbidden (in-flight pointer aliasing).
- one proxy exit per lane, always. the picker never hands out a taken exit
  and no pin path keeps a pin another lane holds (`lanePinnedElsewhere`,
  `pinUniqueProxy`, `laneTouch`); a lane with nothing unique left carries no
  pin and sends direct (or hands the request to a lane that owns one).
  when every exit is taken or written off, `recheckAllExits` clears the
  long-burns + per-model burns and refetches the list rather than sharing.
  sharing one exit across lanes is what piles a table onto one ip and
  429s it, so never reintroduce a least-loaded fallback.
- proxy urls may embed `user:pass`. run `redactProxyUserinfo` before any
  proxy value reaches `/status` AND logs.
- never forward client credentials upstream: NIM path has a header
  deny-list (`authorization`, `host`, `origin`, `cookie`, `x-api-key`,
  `x-session-id`); the nous path repeats it in `nousFwdHeaders` (same six
  headers, same reason: authorization is the proxy's own credential there and
  x-session-id would link the client upstream). zen has its own blocklist in
  `zenHeaderForwarded`
  (also drops `content-type`, `accept`, `user-agent`, `accept-encoding`,
  `connection`, `content-length`, `x-forwarded-host`, and every opencode
  identity header the proxy sets itself: client, session, session-id,
  request, project, session-affinity). `ZEN_PASSTHROUGH=1` layers the
  allowed client headers over the proxy's own base set and still runs the
  deny-list, so authorization and the session id stay the proxy's.
  new forwarded headers must pass both lists.
- several upstream reads are uncapped (`io.ReadAll` on response bodies
  in NIM/zen paths). cap any new read with `io.LimitReader`.
- `math/rand` is for sampling only (weighted picks, probes). secrets and
  session/message ids use `crypto/rand` (`zenID`); keep it that way.
- `mergeMissingKeys` is append-only comments. never reorder, rewrite, or
  reformat the user file.
- pii: `piiwiring.go` + `pii/` (`detect.go` heuristics, `vault.go` +
  `scan.go` vault/scan cache, `wordpiece.go` tokenizer, `ner_ort.go`
  behind `-tags ner`). lane eviction must not evict vault entries
  (different TTLs); scan-cache idle sweep via `pii.SweepScanIdle`.
