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
  id), `x-opencode-project: global`, bare `User-Agent: opencode/<version>`
  (live tag from anomalyco/opencode releases — the repo moved off sst/ —
  refreshed every 6h, default in zenproxy.go). source of truth is the
  released v1.19.3 `session/llm/request.ts`, which branches on providerID:
  opencode models get this set with no `ai-sdk`/`runtime` suffix, and
  `x-session-affinity` + `X-Session-Id` belong to the *other* branch, so
  they are not sent. `x-opencode-request` carries a fresh descending-id
  message id per attempt (`zenMessageID`), as the installed 1.19.x client does.
  (`anthropic-version` is NOT
  sent; an earlier doc revision claimed it.) muse-spark models use
  `/responses`, everything else `/chat/completions`; shape follows the
  model, converted both ways (`convertToResponses` / `chatFromResponses`).
  the free-tier gate reads **two** fingerprints and answers
  `429 FreeUsageLimitError` to a request missing either (measured
  2026-10-26 on one exit, one minute: `ensureZenGate` body + `zentls.go`
  hello = 200, either half removed = 429):
  (1) the body — a system text carrying a valid `<env>` block
  (`zenEnvPrompt`) **and** a tool named `read` in its tools array
  (`zenOCDefsJSON` in zentools.go). the measured minimum (one variable per
  request, two exit ips, 2026-10-26): the *first* `<env>...</env>` pair has
  to hold a `Working directory` line. the heading, the other four fields,
  their order and their values are not read (an empty value passes, so does
  a bogus platform), the block can sit anywhere in a system text or in
  responses `instructions`, and the prose around it does not matter. the
  tags are case-sensitive and need closing, `<env></env>` with no field line
  is refused, env pasted into the user turn is refused, and a valid block
  placed *after* an invalid one still fails because the gate reads the first
  pair — which is why `ensureZenEnv` repairs a client `<env>` in place
  instead of appending a second block. tools only need the name: `read`
  with any description, with an empty or absent schema, name-only, or
  anthropic-shaped `{name,input_schema}` all pass, while `bash`, `shell` or
  an unrelated tool is refused. it is the *name*, exact and lowercase, and
  it is required even when the caller brought tools of its own: `get_weather`
  alone answers 429, as do `Read` (capital) and `fileread`. nothing past the
  name is read, so the declaration the proxy adds is a placeholder — a
  description saying it is not callable — not the client's real file-reading
  text, which invited calls to a tool the caller had never declared (a model
  asked to read a file now answers in text instead of calling it). the
  11-tool 1.19.3 set is stale — its `read` is the v1 declaration (`filePath`)
  and no longer passes. so `zenEnvPrompt` emits `<env>` + `  Working directory:`
  + `</env>` and nothing else, and `ensureZenTools` adds one tool named `read`
  (plus `tool_choice: "none"` when the body had no tools at all). the block
  has to survive the shape conversion: `chatFromResponses` rebuilds
  `messages` from `input`, so a `/v1/responses` body without `instructions`
  lost the injected system message and every such request read as a spent
  pool (429 from the gate, 503 from the ladder). `ensureZenEnv` therefore
  runs again on the converted body; keep that call when touching either
  converter.
  `scripts/zengate` is the probe for both halves (`go run ./scripts/zengate
  -sweep`), and it cannot be replaced by reading a 429: see below.
  (2) the TLS ClientHello — go's `crypto/tls` is refused on every exit
  while the released client's bun/BoringSSL hello serves, so every zen
  connect goes through `zentls.go` (utls, the captured hello: BoringSSL
  extension order, ALPN `http/1.1` only, hybrid X25519MLKEM768 key share,
  SNI first). the transport sets `DialTLSContext`, never `DialContext`,
  or net/http wraps the tunnel with crypto/tls again and every send 429s.
  headers, ua and session shape are not part of the decision — the v1
  header set passes with a v2 body.
  zen also
  enforces a per-IP reputation check (~1.5s between requests). admission to
  the pool needs both halves: `verifyProxy` dials the exit and asks
  `GET /models` through it, then runs the zero-cost gate probe
  (`zenGateProbe`: the invalid-role body `scripts/zengate -probe` sends, 400
  = gate passed, 429 = this ip is spent). /models skips the free-tier gate,
  so a 200 there alone admitted exits that answered the cap to every real
  request (2026-11-01: a 182-exit pool with no usable member; requests rode 15
  hops for 190s into a 503 while the same list held exits that served). a
  probe-429 parks the exit like a real tier-cap 429, so a spent pool answers
  the 503 at once instead of hopping through it. the sweep never names a
  model, because whichever one it named would be the pool's whole opinion:
  /models skips the free-tier gate, and a completion probe answers for one
  bucket only — measured 2026-10-28, one exit answered the cap for
  big-pickle, deepseek, glm and mimo and then served a real beam-free
  completion from the same ip in the same minute. capacity is learned where
  the model is actually known, from real requests:`noteExit429` marks the exit+model pair, `noteExit429Cap` honours the 429's own
  `retry-after` for that pair (a cap mark survives the 30m sweep, since the
  upstream named the deadline), `noteExitGate` parks an ip on a 403,
  `noteExitTierCap` parks it on a long `retry-after`, and the ladder rotates off
  each one. every one of those marks is a daily meter: `watchZenMidnight` drops
  them at 00:00 utc and sweeps, so the pool comes back the moment the allowance
  does instead of when a 30m refresh happens to land.the price is honest: when the tier is out for every exit the list can offer
  (2026-11-01: 1294 candidates, ~140 reachable and every one capped) the sweep
  verifies nothing, keeps the old pool, and holds the next sweep for 10m; the
  probe-parks make the request answer the 503 immediately (2.7ms measured)
  instead of spending `max_retries` riding capped exits. no
  account-level verdict is ever drawn from a 429 (section 5).
  the sweep once completed a token first (`verifyCapacity` on
  `zenVerifyModel`, `deepseek-v4.1-flash-free`) and later gate-probed a single
  model; both were the same mistake — a canary decides for models the caller
  did not ask for (2026-10-27: deepseek 429 while `beam-free` served 200 on
  three of four gate-passing exits).
  a working exit stays pinned to its lane across turns
  until it actually hits a 403 cap (`noteExitGate`) or a free-tier 429 with a
  long `retry-after` (`noteExitTierCap`): both park + drop the exit and
  evacuate its lane onto a fresh one. a short-window 429 only marks the pair.
  upstream tool call names pointing at the ballast map back
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
- **orcarouter**: openai-compatible gateway at `https://api.orcarouter.ai/v1`
  via `orcarouter/<model>` ids, provider-prefixed upstream
  (`deepseek/deepseek-v4-flash-free`). free rows carry the `-free` suffix and
  run under their own per-workspace limits; they never fall back to a paid
  model. keys in `orcarouter_keys` (own pool); a 429 cools one key and
  rotates. the catalog answers anonymous callers but authenticates strictly
  once a key rides along, so a pool key is sent only when one exists.
- **per-key paid marking** (`uno`, `orcarouter`, `openrouter`):
  `<section>.paid_keys: [name]` names keys from `<section>_keys` that may serve
  billed rows. free ids draw the whole pool; a billed id draws only the named
  keys when any is named (`keyedKey.Paid`, `keyedPool.hasPaid`,
  `pick(paidOnly)`, `keybackend.paidFor`). no key named = every key serves
  both, so a single-key setup behaves exactly as before, and a billed id whose
  paid key is cooling fails rather than billing a free-tier key.
- **paid models on keyed routers** (`uno`, `orcarouter`, `openrouter` chat):
  free rows only by default. `<section>.paid_models: true` serves the paid
  catalog too, and `<section>.model_whitelist` adds exact ids even when the
  upstream listing omits them. both gate the listing *and* the request path
  (`keybackend.allows`), so a paid id with the flag off answers 400
  `model_not_served` instead of billing.
- **models.dev**: `modelsdev` fetches `https://models.dev/api.json` and fills
  `model_info.jsonc` gaps after the openrouter overlay fills its own: empty
  fields only, plus a synthesized entry for an id the file never listed (so it
  serves with real numbers, not the 131072 fallback). background fetch at boot
  and every `refresh_hours` (24). one undecodable row is skipped, not the
  catalog.
- **NVIDIA NIM**: upstream `integrate.api.nvidia.com`, needs
  `nvidia_keys` in config.yml. `nvidia/` prefix is listing-only, stripped
  before forwarding. no keys (or `nvidia.enabled: false`) = NIM 503s,
  `opencode/*` keeps working.
- `ServeHTTP` (main.go): `/status` + `/health` -> `StatusFor`;
  `/v1/models`; `/v1/messages`, `/v1/messages/count_tokens`,
  `/v1/messages/classifier`;  `freeepi/` -> freepi, `antseed/` -> antseed,
  `opencode/` -> zen, `nous/` stripped, `orcarouter/` stripped,
  `nvidia/` stripped, else NIM.
  both dispatch tables (ServeHTTP and `handleAnthropic`) carry the prefix;
  a new backend needs a branch in each. `backendForModel` (debugtrace.go)
  needs a case too or traces mislabel the backend.
  inbound bodies capped at 10MB (`bodyLimit`). `route/` is handled in
  `route.go` (dispatch sites in `main.go` and `anthropic.go`): default
  priority ladder opencode -> openrouter -> orcarouter -> uno -> antseed ->
  nvidia -> freepi -> nous (`routeBackends`), per-backend retry budgets
  (`routeAttemptCap` via `expandRouteAttempts`), loose id matching
  (`routeKey`: `-free`/`:free` trims + `peelDateStamp` date stamps), free
  rows ordered ahead of paid ones (`freeFirst`) while both stay in the ladder. a
  candidate that answers nothing in 6s (3s under `wroute/`) is abandoned for
  the next one (`routeTTFTFor`), and a routed request's usage counts on the
  model it resolved to (`usageModel`), not on the route id.
  openrouter rows carry `price_1m` (usd per million input/output) and their
  own per-token `pricing`; no other catalog we read publishes a price, so
  their rows keep the zero claim. `/v1/models` folds variants
  (`dedupVariants`, models.dedup_variants,
  default on; parseVariantRow compares only keyed-prefix versioned ids;
  a row's shape key is its whole name minus version and size tokens, so
  tier suffixes (lite/mini/pro/turbo) and every other qualifier are their
  own families: gemini-3.5-flash-lite never folds into gemini-3.6-flash,
  and gemini-3.5-flash never folds into gemini-4-argon).
  service via
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
  `nimstatus.go` reads the jsonl for today/all-time display.

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
| 429 | per exit+model rate limit. instant exit+session rotation (`laneRotate`), no cooldown waits, and never forwarded: the ladder retries across lanes (`max_retries` default 15), extends itself when the pool itself is written off (below), and only then ends in a `proxy_exhausted` 503. a hop that rotation hands back an exit *this request* already drew a 429 from ends the ladder right there with the same 503 (`burned` in the ladder loop of `main.go`/`anthropic.go`): a re-send to that pair is a guaranteed cap answer, and on a thin pool it burned most of `max_retries` first (measured: 13 of 15 hops, ~2s apiece) before answering the caller the same way. 2 in a row or 6 a cycle marks that pair `rate_limited` for `exit_429_cool_secs` (default 300, capped at the refresh interval), so a busy exit comes back on its own instead of sitting out the cycle; the next refresh clears the marks and counters too. the mark is per model because zen limits the pair: an exit busy on one model keeps serving the rest | normal, no fix |
| 403 FreeTierError, "current network"/"abusive traffic" | the exit IP hit its daily cap or was flagged (`zenGateIPBlocked`). parked for `exit_burn_minutes` (`noteExitGate`) and dropped from the pool; the lane rotates onto a fresh exit and the request still succeeds | normal, no fix — this is the everyday churn: ~50-100 requests per IP, then move to the next one |
| 403 FreeTierError, "can only be used from within OpenCode" | the request shape itself was refused, so the IP is fine. burns that exit+model for the cycle (`noteExitModelBurned`) and rotates, but never parks the exit | fix the shape (body env/read + `zentls.go` hello), not the pool. do NOT inject gate stubs into tool-less bodies — the summarizer emits calls zen rejects ("tool call not allowed while generating summary"); `ensureZenTools` skips empty `tools` deliberately. if it survives rotation, work through "403 that survives rotation" below. shape gate verified live 2026-10-24 on `/inference/v1/chat/completions`: a body with no `tools` → 403, `stream:false` → 403, and dropping the session headers → 403; `User-Agent: opencode/1.18.29` and an extra `x-opencode-request` still pass. on an ip with budget the gate runs before the quota check, so there a 429 does prove the shape was accepted — but on a spent ip the quota answers first, and a tool-less body gets the same 429 as a perfect one (measured 2026-10-26), so a 429 from a capped exit says nothing about shape |
| 502/503/504 from an exit | dead exit or a proxy in front of it failing (`zenRetryableStatus`). rotated like a 429, also never forwarded | normal noise, no fix |
| 403 geo (`not available in your country`) / `[user_blocked]` | the model is geo-gated for the exit's region, not the ip banned: `noteZenRegionBlock` caches it per model+country (`zenRegionGated`) for one refresh, the exit keeps its seat for every model the region may use, and the ladder picks another region. `[user_blocked]` still drops the exit; threshold (`error_threshold`, default 3) refreshes pool | normal, no fix |
| 502 / transport error | dead exit, dropped from pool (`dropProxy`, transport closed) | normal noise, no fix |
| 529 | treated as 429 in both NIM and zen paths | normal, no fix |
| 429 `FreeUsageLimitError`, short `Retry-After` | the free tier metering this exit's short window. ordinary 429: `noteExit429` marks the pair, the lane rotates onto a fresh exit with a fresh session, and the ladder tries again | rotate. a `freetier=true` field in the rate-limit log line says the body was this cap. never forwarded |
| 429 `FreeUsageLimitError`, long `Retry-After` | this exit cannot serve for a while. `noteExit429` marks the pair, and `noteExitTierCap` parks the whole exit (`exit_burn_minutes`, same as a 403 gate park) because a cap used to be read as "the ip is out". measured 2026-11-02 it is per exit+model: an exit serves about one free id and answers this 429 to the rest, so `noteExitTierCap` never parks on a 429: measured 2026-11-03 the cap is per ip x model (one exit answered it for glm and deepseek while serving nemotron and beam-free in the same minute), so only the pair is marked — parking a known-good exit wrote off the one bucket it does serve, and drained a 59-exit pool to 5 in four minutes of requests. the lane rotates onto a different exit with a fresh session | mark the pair, never the exit: rotation still runs, and the ladder re-sends once on the same exit with a fresh session before marking (`sameExitRetried`, `laneNewSession`) because the answer flaps — 429, 429, 200 seconds apart on one exit, measured 2026-11-03. leaving these in the pool is what made `/status` name 60-95 "working" exits that served nothing, and what made one 503 take 85s of hops to write. after the fingerprint fix (2026-10-26) a sweep of ~970 candidates dials ~150 reachable exits and verifies the handful with budget left |
| persistent all-exit failure | no success for `stale_pool_secs` (default 15s) with a lane limited the whole time = stale pool/sessions: refresh + `resetStaleLanes` | fix the refresh/rotation path, not the retry loop |

### what the 429 actually is (solved 2026-10-26)

it is the client gate, and it *is* about the request. the section below kept
this tree on a two-session treadmill (including a uTLS transplant that
changed nothing) because the gate mirrors two fingerprints and the tests in
that epoch changed neither: a correct body with go's TLS stack, or a correct
TLS stack with the old body, both answer the same instant 429. the
single-variable A/B that settles it, one exit, one minute, bun as the known
good client:

| body | tls | result |
|---|---|---|
| `<env>` + `read` (v2) | bun | 200, real completion |
| same, env removed | bun | 429 |
| same, tools removed | bun | 429 |
| same, empty `<env></env>` | bun | 429 |
| same, env in the user message | bun | 429 |
| v2 body | go crypto/tls | 429 |
| v2 body | curl | 429 |
| v2 body | utls + the captured hello | 200 |

re-measured 2026-11-02, and the bun row above no longer holds: a local bun
1.4.3 hello is refused where this spec serves. one exit, one model, same body,
same headers, same minute — utls + `zentlsSpec` answered **200 nine times in a
row** while bun got `429 FreeUsageLimitError` five times, through `fetch` *and*
through `node:tls` with a hand-written HTTP/1.1 request. so the refusal is the
ClientHello itself, not the http layer, and the gate tracks the hello family of
the *released* client: re-capture from that client, never from a locally
installed bun. corollary worth keeping: nimroute's own stack currently passes
where the vendor's runtime does not, so "it must be our shape" is not the
first hypothesis for a 429.

the minimum, same exits, utls client, one variable per request:

| variant | result |
|---|---|
| client prompt + `<env>` + `read` | 200 |
| prose dropped, env block alone as the system message | 200 |
| env at the end of the system prompt (what the proxy does) | 200 |
| env at the front, prose after | 200 |
| env as a *second* system message | 200 |
| env keeping only `Working directory:` (empty value) | 200 |
| bogus values (`/nope`, `plan9`, 1970) | 200 |
| `<env>Working directory: /x</env>` on one line | 200 |
| no `<env>`; empty `<env></env>`; heading + tags with no field | 429 |
| `<ENV>`, unclosed `<env>`, or env in the user turn | 429 |
| valid block appended after an invalid one | 429 |
| `read` name only, or with a different description / empty schema | 200 |
| anthropic-shaped `{name,input_schema}` read | 200 |
| `bash` or an unrelated tool instead of `read` | 429 |
| env but no tools; tools but no env | 429 |
| `tool_choice: none` next to the env + `read` | 200 |
| `/responses`: env in `instructions` + a flat `read` | 200 |
| `/responses`: chat-nested tools | gate passes, upstream 503 — shapes are per endpoint |

so `ensureZenGate` + `zentls.go` are the whole answer, and a 429 now means
what it says: this exit is spent, or one of the two fingerprints regressed.
both halves are checked live after any change to either — a sweep that
verifies exits is the proof that the shape still passes, and one real
completion through a verified exit (`scripts/zengate -proxy <exit> -model
<id>`) proves serving. the evidence that used to be read as "the
request does not matter", for the record:

- any body — tools or none, `stream` true or false, our shape or the installed
  client's own captured bytes — the same 429;
- an unknown model id answers `400 Model is unavailable`, malformed json
  answers 400, `{}` answers 500, so the gate sits after parsing and before
  everything else, and it never mentions the request;
- `retry-after` is *seconds to the next utc midnight*, not a reset instant
  (3456 at 23:05, 76440 at 02:46), and crossing that boundary restored
  nothing;
- ~180 distinct exits, ipv4 and ipv6, residential line and datacenter, all
  answered it, as did the vendor's own CLI;
- 60 random proxies off the pool list, driven by a bun script sending the
  installed client's own header shape: 25 tunnels reached zen and all 25
  answered that 429, the other 35 never completed a CONNECT. a different
  http stack and 25 fresh egress ips changed nothing, which is what rules
  the client out;
- credentials are not consulted at all: `Authorization: Bearer public`, a
  bogus `Bearer nonsense-abc123`, and *no* authorization header all answer
  the identical 429, while a paid model with no key answers
  `401 AuthError: Missing API key`. the free gate runs before auth, so no
  credential we could add or drop changes this answer;
- it is not address-scoped either: three ipv6 /128s in the one residential
  /64 (including the stable `secured` address macOS never uses for outbound,
  so it had never spoken to zen) all answered 429, and 8 tor circuits on 8
  different exit ips answered it 8/8, in under a second each. rotating inside
  a prefix does not help and a brand-new network does not help;
- the released v1.19.4 binary (`opencode-darwin-arm64.zip`, unzipped and
  strings-read) settles the client side: without a key it sets
  `apiKey: "public"` and disables every paid model, sends exactly
  `x-opencode-session-id`, `x-opencode-project`, `x-opencode-session`,
  `x-opencode-request`, `x-opencode-client` and a bare UA, and carries no
  attestation, ticket or signed token for zen (`x-opencode-ticket` is local
  pty auth; `models.opencode.ai` is only the model catalog;
  `api.opencode.ai` is the github-app oidc service). there is nothing left in
  the client to imitate, and its own error handler just prints "Free usage
  exceeded, subscribe to Go" at `https://opencode.ai/go`.

why the earlier rounds read it as a quota: a request missing either
fingerprint is refused *before* auth and before any quota accounting, so
`Bearer public`, a bogus bearer and no header at all answered identically;
an unknown model id still answered `400 Model is unavailable`; the retry-after
countdown always landed on utc midnight whatever the ip; and ~180 networks
(three /128s in one residential /64, 8 tor exits, 60 public proxies) answered
the same body. all of it is consistent with a pre-auth client gate — which is
what it is — and none of it distinguished "spent ip" from "wrong client"
because the gate runs first.

the one real budget signal that survived: an exit with budget answers the
`read`/`<env>` body with 200 and a spent one with this 429, which a real
request is the only thing that measures. so a 429 is still not a licence to
stop rotating —
it means this exit has nothing, park it and move on — but it is no longer a
mystery about the request: if a sweep verifies 0 exits *and* the gate probe
(`zenProbeBody`, invalid role, expected 400) answers 429 on reachable exits,
the fingerprints have drifted and  the two files to re-check are `zentools.go`
(`zenEnvPrompt`, `zenOCDefsJSON`) and `zentls.go` (`zentlsSpec`), and  `go run ./scripts/zengate -sweep` is the tool that answers it (procedure in
  "when the gate breaks again" above). `go run ./scripts/zengate -midnight`
  waits for 00:00:05 utc, probes every distinct exit for free, then spends one
  random free model on each gate-passing exit — the measurement of how much of
  the pool the daily reset actually frees. the probe is free, so use it to A/B a
shape across exits instead of spending completions — but read it only as a
shape answer: `-probe` returning 400 says the gate accepted the body, never
that the ip has budget (measured 2026-11-02: this host answered `-probe` 400
and then the free-usage 429 to all eight free ids within the same minute, which
is why admission is `GET /models` only and capacity is learned from real
requests). `-probe` invalidates the
*role* of the last message and must leave the system message alone — swap the
whole `messages` array and the `<env>` block goes with it, every exit answers
429, and a healthy pool looks like a shrunken request. the reference clients (`9router`,
`dsh-our-free-model`, `OmniRoute`) track this weekly and were what named
both halves.

### when the gate breaks again

what the gate reports, and why that makes it hard: **one** answer,
`429 {"type":"error","error":{"type":"FreeUsageLimitError",...}}`, for a
spent ip, a wrong body and a wrong hello alike. it runs after json parsing
and before model validation, auth and quota, so `400 Model is unavailable`
proves nothing about it and `retry-after` is only seconds to utc midnight.
the invalid-role probe answers 400 for a fresh ip and 429 for a spent one,
so it does **not** separate "spent" from "shape broke" either — during the
2026-10-26 investigation 317 reachable exits answered 429 to every shape
while the same exits answered 200 an hour later with no code change. assume
nothing from a single request; sweep, then compare.
it has changed twice before: v1 wanted the 11-tool set, `x-opencode-request`
and `opencode/1.18.x`; v2 wanted the `read` declaration plus the `<env>`
block; 2026-10-26 added the TLS hello. it will change again, and the vendor
ships on its own schedule, so the shape has to be re-derived, not guessed.

order of work when a sweep verifies 0 exits on a pool that was serving:

1. `go run ./scripts/zengate -sweep -n 0` — every reachable exit 429 and
the clock is not near utc midnight? keep going. a handful of 200s means the
shape is fine and the pool is simply spent; stop here.
2. `go run ./scripts/zengate -proxy <exit that answered 200> -variant
minimal|no-env|no-tools|empty-env|env-in-user|full` — confirms which half
the refusal tracks (env vs tools) on one exit, seconds apart.
3. re-capture the real client, do not re-read this file: install the
released `opencode-ai` npm CLI, run `opencode run -m opencode/big-pickle`,
and either `ZEN_CAPTURE=1` our proxy or a mitm byte-log the client's own
request. diff its body against `go run ./scripts/zengate -variant full
-dump`, and its hello against `zentls.go`'s spec (bun/BoringSSL, ALPN
`http/1.1` only, BoringSSL extension order, X25519MLKEM768 first). capture it
from the released client, not from a local bun — but do not read a bun `429` as
a hello verdict: measured 2026-11-02, a local bun 1.4.3 replica sending the same
body, headers and params served `200` through the same exits this client does
(8 serving exits inside the first 100 probed). one exit refused bun while
serving this client minutes apart, which is exit noise. the shape is not the
constraint; which bucket an exit has is.
4. replay the divergence with `-body <file>` (a captured nimroute body) so
the comparison is bytes, not a description.
5. port the winning shape, then prove it: `-sweep` must find 200s, and
`/v1/chat/completions`, `/v1/messages` and `/v1/responses` must all serve
through :5419. delete scratch captures when done.

never draw a pool verdict from a 429 (section 5), and never answer an
`opencode/<model>` request from another backend — a broken gate is a reason
to fix the shape, not to substitute the model.

client contract: no zen response ever carries a rate limit, and never zen's
own window. every 429/529 an exit returns is absorbed by rotation, and a spent
ladder answers `exhaustedExits` — a `proxy_exhausted` 503 naming the pool
state — so the caller retries against us instead of waiting us out.
`zenClientHeaders` is the one place a client sees upstream headers: only
`content-type` crosses, and a rate-limit answer leaves with
`Retry-After: zenClientRetryAfterSecs` (10s). forwarding zen's `retry-after`
handed the caller a countdown to utc midnight for a pool it cannot see, and
`x-ratelimit-*` described exits it cannot pick. `ProxyUnavailableMsg`
is the shared text for the other "could not place this request on an exit"
exits (dial-dead, no pin under `always_proxy`, empty pool).

`opencode/<model>` is served by zen or not at all. no same-model rescue on
another host, no neighbouring row: answering a deepseek request with a `glm`
one is a different model wearing the name, and answering it from freepi is a
different backend wearing the prefix. cross-backend, cross-model failover is
what `route/<key>` is for, and it is the caller's choice, not a silent
substitution here.

what a "verified" exit means (`verifyProxy`): the tunnel dialed and zen answered
`GET /models` 200 through it. that is the whole test. a sweep never sends a
completion, never names a model nobody asked for, and never spends a caller's
tier budget: admission is reachability, full stop. an earlier revision probed a
served token instead, and it was removed on purpose — it found more exits but it
trafficked in requests the user never made.

`/models` skips the free-tier gate, so admission says nothing about capacity;
capacity is learned only from real requests. it is per exit+model, and an exit
usually serves exactly one bucket: measured 2026-11-02 over the whole list, 267
reachable exits, 20 serving, and 19 of those 20 served **exactly one** of the
eight ids and answered 429 to the rest (the twentieth served all eight). so
`noteExitOK` records the id each exit actually served (`noteExitBucket`) and
`pickLaneProxyExcluding` prefers a matching exit — a request does not spend a 429
hop on an exit known to serve a different bucket — and the same map keeps a
bucket cap from parking an ip that has already served something. a fresh exit is
judged where the caller's model is known:
`noteExit429` marks the exit+model pair, `noteExitGate` parks the ip,
`noteExitTierCap` parks it on a long `retry-after`, `noteExitTransportErr`
marks a dead tunnel, and the ladder rotates onto the next exit. a sweep that
finds nothing reachable keeps the previous pool, minus the members that sweep
just dialed and refused: an exit it could not reach is not a working pin, so
it leaves the pool and any lane holding it re-pins instead of sending through
it. measured 2026-10-28, the cost of that choice: of 1221
candidates, 229 reachable and 1 serving, so a request rides hops until it
finds the one — the marks are what make the next request cheaper, and the
client still only ever sees a completion or a `proxy_exhausted` 503.

pool membership is per **egress ip**, not per listening port
(`dedupeExits`, `exitHost`, `exitSubnet`): the list publishes dozens of ports
per box, and admitting each as its own exit filled 40% of a 295-entry pool
with four addresses in one /24 — rotation looked healthy while it handed out
the same ip, capped at the same instant. one exit per ip, at most
`exitNetCap` (3) per /24, applied to sweep candidates, the saved pool on
load, and every merge.

never an empty answer: a turn that stops inside its reasoning (max_tokens, or
an upstream that only emits `reasoning_content`) used to reach the client as
a `thinking` block with no text, which claude code renders as nothing.
`streamAnthropicReader` and `openAIToAnthropic` promote that trace to a text
block when the turn carries no text and no tool call.

known limit: the substitute handlers (`freepi`, `antseed`) fetch upstream
with `stream:false` and synthesize the SSE afterwards, so their ttfb is the
generation, not the first token. sequential requests land at ~0.5-1s on
antseed and 3-10s on a 600KB context; a long generation or heavy concurrency
is bounded by that backend, not by this chain.

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

a sweep that verifies nothing while exits keep answering the cap means the tier
is out for every ip it could make serve (`zenTierDown`, `mix.cap > 0` and zero
verified): further sweeps are held for `zenTierDownBackoff` (3m) so a request
never waits on a refill that has to return the same answer. the hold is a
throttle, not a verdict — the list refetches its own candidates and capacity
turns over in minutes, and an empty pool answers its 503 in ~1ms, so the price
of sweeping again is dials, not client latency.

measured 2026-11-02, the pool a reachability sweep produces: 3062 list rows →
1278 candidates (856 no-tls http, 174 geo-blocked) → 956 dialed, **323 admitted**
on `GET /models` 200 alone, zero completions sent. a temporary all-model probe run
the same day measured what such a pool can actually serve: of 267 reachable
exits, 20 served, and 19 of those 20 served exactly one of the eight ids (buckets
split 5/5/5/3/3/3/2/1). so a reachability pool is a bench of hops whenever the
tier is out for the caller's model — each hop costs one 429, the pair gets
marked, and the next request pays less. that is the accepted price of not
probing: the proxy never spends a completion the user did not ask for, and a
spent ladder still ends in the honest `proxy_exhausted` 503 that names the pool.

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
lanes are capped at the exits a lane can actually hold (`laneCapFor`, every
lane-creating path including `sharedLaneLocked`'s fallback; `usableExitCount`
drops dead and gate-parked members, since a raw pool count promises pins the
sweep just proved dead), and the table is trimmed back to that when it shrinks
(`trimLanesToPool`, from `ensureLanePins`, the drop path, a spent pool wait,
and every request completion via `repairLanes`): a lane with no exit cannot
send at all under `always_proxy`. a 30m refresh
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

### 403 that survives rotation

when `403 FreeTierError` comes back from several distinct exits with fresh
sessions — i.e. rotation is working and the upstream still refuses — the
answer is our request shape, not the pool. our imitation has drifted before
(tools array, identity headers) and will drift again. do not stop at "the
tier is spent" from our own probes alone: the other clients are on the same
free tier and are evidence of today's accepted shape.

clone all four into one scratch folder outside the tree (`/tmp/zenrefs`),
then work the list in order:

- `https://github.com/anomalyco/opencode` — the client we mimic
  (`session/llm/request.ts` is the source of truth for the zen branch)
- `https://github.com/cheaper-inference/OmniRoute`
- `https://github.com/decolua/9router`
- `https://github.com/Ebony-Vinyl/dsh-our-free-model`

1. find their zen path in each (`grep -rn "opencode\.ai\|x-opencode\|Bearer public"`).
   read how they build the request — upstream path, identity headers, body
   shape, tools array, session/request ids, stream handling.
2. diff the four against each other *and* against ours (`setZenHeaders`,
   `zenOCDefsJSON`, `zenMessageID`, `zenSessionID`, `zenFwdHeaders`). what
   they agree on is the shape zen accepts; what we send that none of them
   send (or omit that they all send) is the bug.
3. reproduce with curl or a scratch script against the live upstream before
   touching Go: one request per variant, record status and body. a variant
   that answers 200 for one model must be re-run across every
   `opencode/<model>` id from `/v1/models` — the gate is per model, and an
   id that only works in one body shape proves nothing about the rest.
4. only then port the winning shape into the proxy, smallest diff (section
   2). done means every listed zen model serves through :5419 on both
   `/v1/chat/completions` and `/v1/messages` — not one probe returning 200.
5. delete the scratch folder and any mock/probe scripts with the rest of the
   scratch artifacts (section 2).

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
  the idle sweep (`sweepLanes`) never drops `shared:` lanes: they are
  pre-opened seats holding a pin and no identity, so sweeping them emptied
  the table (and `/status`, whose lane list is `omitempty`) whenever zen was
  not taking traffic. `trimLanesToPool` is what bounds them.
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
-  `math/rand` is for sampling only (weighted picks, probes). secrets and
  session/message ids use `crypto/rand` (`zenID`); keep it that way. the
  *value* of a `ses_` id is checked, not just its shape: zen decodes the 12
  hex through the client's `descending()` layout and only accepts a low-12
  counter of 0..63 (`0xfff` down to `0xfc0`), so `zenID(true)` wraps the
  per-millisecond counter at 64. an unbounded counter rendered `fbf` past 63
  and every id minted in that millisecond was refused — invisible at low
  rate, and under the 200-worker sweep it poisoned most of the probe set
  (measured 2026-11-02: 1748 of 2000 ids outside the window).
- `mergeMissingKeys` is append-only comments. never reorder, rewrite, or
  reformat the user file.
- pii: `piiwiring.go` + `pii/` (`detect.go` heuristics, `vault.go` +
  `scan.go` vault/scan cache, `wordpiece.go` tokenizer, `ner_ort.go`
  behind `-tags ner`). lane eviction must not evict vault entries
  (different TTLs); scan-cache idle sweep via `pii.SweepScanIdle`.

## 8. adding a new provider

A provider is done when a caller can request it directly, through
route, and see it in `/status` and `./nim status` — a backend wired
into only one of those is half-built. checklist (one bullet per chat
endpoint unless the provider is NIM-style direct upstream):

- **config**: struct fields in `config.go` + defaults in
  `defaultConfig()` + documented keys in `config.yml.example`. keys go
  to the user's `config.yml` only; the example gets placeholder
  comments. an `enabled()` helper gates everything else (see
  `unoEnabled`, `orChatEnabled`, `nousEnabled` — enginess keyed on the
  key pool length, not a bare flag).
- **request path**: `ServeHTTP` (main.go) prefix branch stripping
  `<prefix>/` + a matching branch in `handleAnthropic` (anthropic.go) —
  both dispatch tables carry the prefix or half the clients break.
- **dispatch**: chat handlers in a provider file (see `uno.go`,
  `openrouter.go`, `nous.go`) plus the openai and anthropic variants if
  the catalog is openai-shaped; anthropic-shaped catalogs feed
  `handleAnthropic` directly. new backends that are openai-compatible
  with a key pool reuse `keyedchat.go` (`keyedChat` /
  `keyedChatAnthropic`).
- **route**: `route.go` — `routeBackends` append + the three
  conversions: `dispatchRouteOAI` and `dispatchRouteAnthropic` cases
  (re-set the bare model id on the body before the handler call), and
  `routeBackendLive` wired to the `enabled()` helper. `routeKey` strips
  catalog suffixes (`:free`, `-free`) only when the backend lists both
  base and suffixed rows for one model.
- **debugtrace**: `backendForModel` (debugtrace.go) needs the prefix
  case or traces mislabel the backend.
- **config.yml.example wiring**: the example is the schema source of
  truth; every new key gets a commented line there. nothing extra:
  `mergeMissingKeys` appends absent keys to the user file automatically
  on restart.
- **/status**: `Status()` builds a section, `StatusFor()` gates it
  behind `status.show_*` + redaction per section 4. naked key content
  never reaches `/status` or logs.
- **models list**: serve ids on `/v1/models`; catalog cache TTL
  pattern from `uno.go` (hot reload, closed-stale on fetch failure).
- **bench + AA scoring**: `benchBackends` (benchmarks.go) prefix +
  `servedBenchIDs` seed so scoring works before the first live hit;
  model ids matching bench rows use `benchFor(id)` (normModelID folds
  `:free`/`-free` and vendor-qualified ids; an AA row that names a model but
  carries no score is still a match — `aaIndex.bestScored` takes the number
  from the same release family, which is how `opencode/deepseek-v4.1-flash-free`
  finds `deepseek-v4-1-flash-1017`; family keys are indexed with the date stamp
  peeled off, so a dated build and the plain release meet).
- **`.nim` service**: nothing unless the provider needs auth setup
  (`./nim freepi auth` pattern for interactive logins).
- **docs**: `docs/service.md` copy for any new user-facing command.

verify before commit: green per section 2, then a scratch-port run
(`CONFIG_FILE=config.test.yml`, port in `server.port`, own
`usage:` block, delete after) exercising direct dispatch, one
route/<model> request, and `/status`.
