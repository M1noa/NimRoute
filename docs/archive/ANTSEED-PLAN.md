# ANTSEED-PLAN.md — AntSeed provider for nimroute

status: SHIPPED — implemented in `antseed.go` (config, routes, handlers) + `antseedp2p.go` (wire, handshake, frames), wired at `ServeHTTP` (`freeepi/`-style `antseed/` prefix). tracker: `ANTSEED-TODO.md` (all items done). docs in `ANTSEED-DOCS.md` (protocol/API reverse engineering).

## 0. goal

- consume the AntSeed P2P network **directly from Go** — no `antseed` CLI, no desktop
  app, no VPN, no Node. their buyer proxy protocol reimplemented to the extent needed
  for free LLM offers (the required subset is ANTSEED-DOCS.md §9.2).
- clients hit nimroute exactly like the other backends: model id `antseed/<model>`
  in `/v1/messages`, same anthropic wire format in and out.
- `/v1/models` in nimroute lists **all** antseed text (LLM) models with live peer info
  (price, reputation, context, protocols); the status table shows **only the
  top-used** models in a fixed order.
- forced-proxy option for this provider, same shape as nvidia/zen.
- free models first and only: zero-price offers work with no wallet, no USDC, no
  deposits (proven in ANTSEED-DOCS.md §5). paid channels are out of scope (§non-goals).

## 1. new files (4 max)

| file | contents |
|---|---|
| `antseed.go` | config struct + defaults, `antseed/` route entry from `ServeHTTP`, peer cache + metadata poller (`network.antseed.com/stats`), request dispatch, model catalog (no separate `antseedcat.go`; catalog lives here) |
| `antseedp2p.go` | identity (secp256k1 + EIP-191 `antseed-msg-v1:`), intro/enc-ack handshake, X25519→HKDF→AES-256-GCM records, 9-byte frame codec, HTTP-over-P2P codec, ping/keepalive, free-usage 0x52/0x55/0x53 exchange (EIP-712) |
| `antseedcat.go` | canonical model key (ANTSEED-DOCS.md §6.1), offer ranking (§6.3), `/v1/models` builder (conservative caps §6.2), model table (§model curation) |
| `*_test.go` | table-driven: canonical-key cases, ranking, free-vs-paid classification, frame/codec round-trips, models builder, proxy override |

deps to add (checked against `go.mod` first): secp256k1 + keccak for EIP-191/EIP-712
(decent choice: `decred/dcrd/dcrec/secp256k1` + `golang.org/x/crypto/sha3` — verify
what's already vendored before picking). x/crypto is likely present. no other new deps.

## 2. config.yml additions

```yaml
antseed:
  enabled: false            # default-off: no raw-doc presence check needed
  stats_url: https://network.antseed.com/stats   # snapshot; skips DHT entirely
  refresh_secs: 300
  (plan showed a nested `discovery:` block; shipped flat — see `antseedConfig` in config.go.)
    metadata_timeout_ms: 1500
  trust_min: 0              # mirror of their minTrustScore gate; start at 0, tune live
  prefer_free: true         # zero-price offers only
  request_timeout_sec: 300
  proxies: []               # forced-proxy list (see §forced proxy), same shape as nvidia
```

plumbing per AGENTS.md §3: example entry with comment in `config.yml.example`,
matching defaults in `defaultConfig()`, default-false `enabled` (no presence check),
`mergeMissingKeys` append-only comment. new `/status` section behind
`status.show_antseed` (default off) + `StatusFor` redaction (peer ids / proxy
userinfo via `redactProxyUserinfo`).

## 3. request lifecycle (one `/v1/messages` turn with `antseed/deepseek-v4-flash`)

1. `ServeHTTP` strips `antseed/` prefix → `serveAntseed` (same shape as `opencode/`).
2. resolve model: canonical key → offer set from the peer cache; rank (trust gate,
   prefer free, cheapest, reputation tiebreak); pick top healthy peer. stale cache →
   trigger refresh, don't block the turn.
3. get/reuse pooled TCP connection to the peer (`publicAddress:6882`): intro line +
   enc-ack + AES-GCM session (ANTSEED-DOCS.md §4.1); cache keyed by peerId, lazy dial.
4. best-effort `FreeUsageOpen` (0x52) on first free request; answer any
   `NeedFreeUsageAuth` (0x55) with `FreeUsageAuth` (0x53). failures logged, never fatal.
5. convert inbound anthropic body → seller's advertised protocol (`anthropic-messages`
   passthrough; else reuse the existing anthropic↔openai conversion from the zen path),
   rewrite `model` to the seller's `serviceId`, add `x-antseed-provider` /
   `x-antseed-service`, send as one `0x20` frame (>240 KiB → chunked 0x25/0x26).
6. read response: `0x21` single shot or `0x21`(`x-antseed-streaming: "1"`) + `0x22`…
   `0x23` stream → convert back to anthropic SSE → write to client; record ttft +
   `x-antseed-*` telemetry into headers/usage log.
7. failure policy: 429 → next offer for the same model (up to 3 same-peer retries
   first), transport error → drop connection + blacklist cooldown, pinned/`trust_min`
   failures → 502-style error. never retry after bytes reached the client.
8. log one `logUsage` line (model, peer, tokens, cost-estimate 0, latency).

## model curation (table vs /v1/models, version rules)

- **`/v1/models` (all LLMs)**: every antseed offer with `type == "text"` (drop image
  and decision endpoints), exposed as `antseed/<observed-id>` plus aliases. live
  metadata: context/maxout from offer capabilities, price + reputation + peer count.
- **status table (top-used only)**: static curated order in `model_info.jsonc`
  (there is no popularity signal anywhere in antseed — their own catalog sorts
  alphabetically; ANTSEED-DOCS.md §6.2), starting exactly:
  `antseed/glm-5.3-flash`, `antseed/deepseek-v4-flash`, `antseed/minimax-m3`,
  `antseed/gpt-6-luna`, `antseed/mimo-v2.6-flash`, … curated entries fall back to
  live `/v1/models` info (price, ctx, peers).
- **version handling** (three required cases):
  - variant suffixes are **different models, both listed**: `gpt-6-luna` never merges
    with `gpt-6-sol`; `glm-5.3` never replaced by `glm-5.3-flash`. antseed's own
    canonicalizer already keeps these distinct (exact-compact-match grouping only,
    ANTSEED-DOCS.md §6.1) — mirror it, add nothing.
  - genuine successors **supersede in the table only**: `minimax-m3` takes
    `minimax-m2.7`'s slot; the old id stays valid in `/v1/models`. implement as a
    `supersedes:` field on the curated table entry (old id hidden from table, still
    routable).
- **reputation priority with fallback**: for each curated slot, the table shows the
  model's best-ranked offer (their `effectiveReputationScore` ordering already means
  "higher-reputation provider first, others as fallback" — we don't invent a second
  ranking).

## forced proxy

- `antseed.proxies` list, same semantics as the existing providers: rotate/failover
  over the list, applied only when non-empty; each entry a proxy URL that may embed
  `user:pass`.
- difference from NIM/zen: antseed dials raw **TCP to `publicAddress:6882`**, so the
  proxy must be a CONNECT-capable HTTP proxy or SOCKS5 — implement dialer support for
  both (stdlib `golang.org/x/net/proxy` for SOCKS5 if already vendored, else HTTP
  CONNECT only and document the limitation).
- landmines from AGENTS.md §5: run `redactProxyUserinfo` on every proxy value before
  it reaches `/status` **and** logs; never forward client credentials upstream (the
  antseed path builds its own header set: only `x-antseed-provider`,
  `x-antseed-service`, `content-type`, `accept` — do not pass the client's
  `authorization`/`cookie`).

## implementation phases (each shippable + green on its own)

| phase | deliverable | verify |
|---|---|---|
| P1 catalog-only | config + `antseed/` prefix routing stub, stats poller, peer cache, `antseedcat.go` + `/v1/models` merge (all text models, aliases, caps) | unit: canonical-key + models builder; e2e: `/v1/models` shows antseed entries with live peers |
| P2 transport | identity, handshake, secure channel, frame + http codecs, ping | unit: codec round-trips against golden bytes captured from the reference (see test protocol step 1), handshake against a live seller |
| P3 free inference | dispatch, free-usage exchange, anthropic↔openai conversion, streaming, retries, usage log | e2e on scratch config: real `antseed/<model>` turn end-to-end, streaming + non-streaming |
| P4 polish | status section behind `status.show_antseed`, forced proxy, model table (curation + supersession), tuning (`trust_min`) | unit: table/supersession cases; redaction test for proxy userinfo; green gates |

phases touch only their own files; `main.go` gets the prefix branch once (P1) and the
status hook once (P4).

## test protocol (separate port, main untouched) — AGENTS.md §2

1. capture reference bytes: run the official buyer (`npm i -g @antseed/cli`,
   `antseed buyer start` on its default 8377 — does **not** touch nimroute) against
   the live network once; save a real `/metadata` JSON + a successful `/v1/models`
   snapshot into `testdata/` as fixtures (strip signatures? no — keep them, they are
   what verification must accept).
2. unit tests first (table-driven, owner boundary: nimroute owns model-id
   normalization + offer selection invariants): canonical key (the three version
   cases + antseed's own fixtures), frame/codec round-trips, free-vs-paid
   classification, ranking order, models builder conservative caps.
3. scratch config: copy `config.yml` → `config.test.yml`, port **5420**, scratch
   usage file, `antseed.enabled: true`; `CONFIG_FILE=config.test.yml ./nimroute`.
4. e2e: `GET /v1/models` (antseed entries present, LLM-only), one non-streaming +
   one streaming `antseed/<model>` completion, a 429-failover test (or forced bad
   peer), table shows the curated order, `/status` hidden unless
   `status.show_antseed`, proxy userinfo redacted in status + logs.
5. python smoke script `scripts/antseed_test.py` (mirrors the freebuff/freepi
   scripts): pulls every free `antseed/*` model from `/v1/models`, sends a real
   completion to each, asserts 200 + content + reports per-model latency/pass —
   run it against the scratch instance, archive output in the PR notes.
6. gates: `gofmt -l .`, `go vet ./...`, `go test ./...`. only after all green,
   restart the primary (`./nim restart`, :5419). delete `config.test.yml`, scratch
   binaries, and usage files.

## explicit non-goals (v1)

- paid USDC channels (ReserveAuth/SpendingAuth, deposits, sweeps, contracts) — free
  offers only; a paid 402 surfaces as an error with the upstream message.
- gateway / tunnel / desktop / DHT / WebRTC / relay — snapshot discovery only
  (`stats_url`); DHT stays a documented fallback for later.
- image (`/v1/images/*`) and decision (`/v1/systemone`) models — `type=text` only.
- `/v1/responses` endpoint, session pins, conversation affinity, TEE verification,
  ratings/reports, x402, per-key gateway.
- touching any pre-existing uncommitted changes in this repo (anthropic.go, config.go,
  freepi.go, main.go, model_info.jsonc, … from earlier work — not mine to edit around;
  rebase carefully and mention, don't revert).
