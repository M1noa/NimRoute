# ANTSEED-DOCS.md — AntSeed reverse engineering: API, protocol, VPN

> source: `git clone --depth 1 https://github.com/Antseed/antseed` + its
> `apps/website/docs/`, at the pinned rev in git log (re-clone to verify).
> file:line refs point into that clone AT THAT REV; line numbers drift upstream.
> this file should be enough on its own to reimplement the buyer side in Go.

## 0. what antseed is

A peer-to-peer marketplace for LLM inference. No central API server, no accounts, no
platform-issued API keys. Two node roles:

- **seller** — runs upstream models (vendor OpenAI/Anthropic-compatible endpoints,
  local GPUs), advertises them on a DHT with a price, serves inference over an
  encrypted P2P connection.
- **buyer** — runs a local HTTP proxy speaking the OpenAI and Anthropic wire formats,
  discovers sellers, ranks them by price + trust, forwards each request to a chosen
  seller over P2P, settles payment in USDC on Base (or not, for free offers).

One node can be both. Protocol v1.9, `METADATA_VERSION = 12`
(`docs/protocol/spec/00-conventions.md`). Everything below covers the buyer side,
because that is what a reimplementation needs to get models working.

## 1. component map

| component | what it is | needed for reimplementation? |
|---|---|---|
| buyer proxy (`apps/cli`, port **8377**) | the product: local OpenAI/Anthropic HTTP API → P2P seller. binds 127.0.0.1, no auth | **yes — this is the thing to clone** |
| gateway (`antseed gateway start`, port 8379) | bearer-key reverse proxy in front of the buyer | no — only for sharing a buyer |
| tunnel (`antseed tunnel start`) | gateway behind Cloudflare Tunnel/ngrok | no |
| desktop app "Antseed AI VPN" (Electron) | GUI: router switch, model/seller picker, credits, tool wiring | no — see §7 |
| DHT discovery (`packages/node/src/discovery`) | BitTorrent Mainline DHT, UDP 6881 | one of two discovery options (§3) |
| relay (`apps/relay`) | hosted DHT snapshot `GET /sellers` + WebRTC signaling bridge | useful shortcut (§3.3) |
| network-stats (`apps/network-stats`) | `GET https://network.antseed.com/stats` → `{peers: PeerMetadata[], updatedAt}` | useful shortcut (§3.3) |
| api-adapter (`packages/api-adapter`) | Anthropic ↔ OpenAI Chat ↔ OpenAI Responses on the fly | yes — nimroute already has both zen directions |
| protocol codec (`packages/protocol`) | framing, message types, HTTP-over-P2P codecs, signing | yes — the wire spec (§4) |

## 2. buyer HTTP API (port 8377)

Base `http://127.0.0.1:8377`, loopback only. **No local API-key validation** — any
non-empty placeholder satisfies clients that demand a key
(`apps/website/docs/guides/using-the-api.md:365`).

### 2.1 endpoints

| method + path | served | notes |
|---|---|---|
| `POST /v1/messages` | routed | Anthropic Messages (Claude Code, anthropic SDKs) |
| `POST /v1/messages/count_tokens` | **local** | `estimateAnthropicPromptTokens` (`count-tokens.ts:17-45`), never routed, never billed |
| `POST /v1/chat/completions` | routed | OpenAI Chat Completions |
| `POST /v1/responses` | routed | OpenAI Responses (Codex); seller side requires streaming, adapter re-assembles non-streaming replies |
| `POST /v1/images/generations` | routed | only sellers advertising `openai-images`; never translated |
| `POST /v1/images/edits` | routed | multipart; pin via header only (model-prefix rewrite needs a JSON body) |
| `POST /v1/systemone` | routed | decision models (`typesafe-systemone`) |
| `GET /v1/models` | **local** | whole-network catalog from the peer cache, `{"object":"list","data":[…]}` |
| `GET /v1/models?type=text\|images\|decisions` | local | unknown value → 400, `error.param="type"` |
| `GET /v1/models/<id>` | local | alias-resolved via canonical key; miss → 404 `model_not_found` |

Dispatch order: control plane → path allow-list (else 404 `invalid_request_error`) →
local `/v1/models` + `count_tokens` → body collect → route
(`buyer-proxy.ts:2242-2288`).

### 2.2 routing: how `model` picks a seller

1. **Pin precedence** (highest first): `x-antseed-pin-peer: <peerId>` header →
   `"<peerId>@<model>"` in the model field → session pin
   (`antseed buyer connection set --peer`, survives restart). Pinned requests never
   fail over. Peer id = 40 hex, optional `0x`, case-insensitive. The prefix is
   stripped before routing so the seller sees the bare model.
2. **No pin**: canonicalize the model (§6.1), collect eligible offers, apply
   `buyer.routingPreferences`, rank (§6.3), rewrite `model` to the seller's
   advertised `serviceId`, translate wire format if seller protocol ≠ client format
   (`PROTOCOL_TRANSFORMS`, six pairs among `anthropic-messages` /
   `openai-chat-completions` / `openai-responses`; `buyer-proxy.ts:491-516`), forward
   over P2P.
3. **Alias** `model: "antseed"` follows the desktop picker's current route; unset →
   400 `no_default_route` (`buyer-proxy.ts:2388-2410`).
4. **Soft conversation affinity**: a recognized conversation prefers the seller that
   served its last turn while healthy; hard pins stay pinned.
5. **Retries**: `RETRYABLE_STATUS_CODES = {401,403,408,429,500,502,503,504}`
   (`buyer-proxy.ts:176-190`); 429 gets up to 3 attempts on the same peer (250/750 ms,
   honors `retry-after` capped 2 s) then the next ranked offer. Never fail over after
   streaming started, on client cancel (499), on non-retryable responses, or when
   pinned. No model and no pin → 400 `missing_routing_target`.

### 2.3 routing preferences (config, hot-reloaded)

```jsonc
"buyer": {
  "minPeerReputation": 0,
  "routingPreferences": {            // shared with the desktop app
    "preferFreePeers": false,        // +25 score bonus for zero-price offers only
    "maxInputUsdPerMillion": 25,
    "minTrustScore": 60,             // HARD eligibility gate; 0 disables
    "allowedPeerIds": [],            // non-empty = allow-list only
    "blockedPeerIds": []
  },
  "maxPricing": { "defaults": { "inputUsdPerMillion": 100, "outputUsdPerMillion": 100 } },
  "proxyPort": 8377
}
```

Defaults: `apps/cli/src/config/defaults.ts:12-59`, ranking prefs
`packages/node/src/routing/model-route-ranking.ts:11-17`. `preferFreePeers` is
ranking only; it never decides whether payment is required (§5).

### 2.4 response telemetry headers

Non-streaming (`apps/cli/src/proxy/telemetry.ts:317-347`):

```
x-antseed-request-id, x-antseed-latency-ms, x-antseed-peer-id, x-antseed-service,
x-antseed-provider, x-antseed-input-tokens, x-antseed-output-tokens,
x-antseed-cached-input-tokens, x-antseed-estimated-cost-usd, x-antseed-peer-reputation,
x-antseed-{input,output,cached-input,total}-usd-per-million, x-antseed-token-source
```

Streaming: request-id + peer identity only (token counts unknown at header flush).
`x-antseed-fault-attribution: buyer` marks buyer-local failures (never fail those
over).

Stripped from inbound requests before forwarding: `host`, `x-antseed-system-*`,
`x-antseed-buyer-identity` (resolved to a stored wallet or dropped; unknown → 400
`unknown_buyer_identity`). Everything else — including a client's `authorization` —
is forwarded verbatim (open issue in `docs/security-review-buyer-proxy.md:55-80`).

### 2.5 error surface

| status | code / message | meaning |
|---|---|---|
| 400 | `missing_routing_target` | no model and no pin |
| 400 | `no_default_route` | alias `antseed`, desktop route unset |
| 400 | `unsupported_protocol` | decision model on a text endpoint (hint: `/v1/systemone`) |
| 400 | `unknown_buyer_identity` | `x-antseed-buyer-identity` names a missing wallet |
| 402 | `insufficient_deposits` | seller demanded payment, buyer balance zero (has `suggestedAmount`, `peerId`) |
| 402 | `channel_exhausted` | per-channel budget spent |
| 402 | `spend_limit_reached` | gateway key hit its USD limit |
| 413 | `upload_body_too_large` | exceeds seller `maxUploadBodyBytes` |
| 422 | `required_capability_unavailable` | seller can't run the requested shape |
| 429 | `free_tier_exhausted` | seller-side opt-in free-tier limiter |
| 499 | `Request cancelled` | client aborted; upstream aborted too |
| 502 | `model_not_found` | no policy-eligible peer advertises the model |
| 502 | `Pinned peer … is not reachable` / `… outside your buyer routing policy` | pin target down or over cap |
| 503 | `buyer_payments_inactive` | seller demanded payment, buyer has no payments config |
| 503 | `spend_tracking_unavailable` | capped gateway key, buyer unreachable (fails closed) |

### 2.6 control plane `/_antseed/*` (loopback, unauthenticated)

`GET /status` (DHT nodes, peer count, uptime), `GET /peers`, `POST /peers/refresh`,
`GET /peer-health` (+ `POST /peer-health/clear`), `GET|POST /route`,
`GET /conversations[/:id]`, `POST /conversations/update`, `POST /connect`,
`GET /buyer-identities`, `GET /channels` (+ `POST /channels/close`),
`GET /attributed-spend`, `GET /buyer-usage`, `GET /metering/:peerId`, `POST /sweep`,
`GET /deposits/status` (+ `POST /deposits/watch`), `GET /verification` (+ `POST
/verification/check`), `GET /sweep/0x<nonce>` — full table `buyer-proxy.ts:1633-2174`.

### 2.7 gateway (port 8379) — optional

`antseed gateway start` adds bearer keys (`antseed_…`, hash stored only) with per-key
USD spend limits (daily/monthly/total), per-key wallet identities
(`x-antseed-buyer-identity` set by the gateway, client value ignored), `GET /v1/key`
usage/limits, x402 top-ups. `antseed tunnel start` publishes the same behind
Cloudflare Tunnel or ngrok. `/v1/models` and `count_tokens` stay free
(`apps/website/docs/guides/gateway-api-keys.md`).

## 3. discovery

### 3.1 DHT (default)

- BitTorrent Mainline DHT (BEP 5), UDP **6881**, bootstrap `dht1.antseed.com:6881`,
  `dht2.antseed.com:6881` (`packages/node/src/discovery/bootstrap.ts:7-10`).
- Sellers re-announce every 5 min (`dht-node.ts:25-28`) on three topic shapes: a
  **wildcard topic** (general discovery), a **/16 subnet shard** (spreads announcer
  load), and a **per-peer topic** `peerTopic(peerId)` for direct lookups
  (`dht-node.ts:57-117`).
- DHT yields `{host, port}` per peer. Then plain HTTP
  **`GET http://host:port/metadata`** returns the signed `PeerMetadata` JSON (≤256
  KiB, default timeout 1500 ms; port = peer port + `metadataPortOffset`, default
  **0** — same port; `http-metadata-resolver.ts:48,61`).
- `PeerMetadata` (`packages/protocol/src/peer-metadata.ts:177-213`): `peerId`,
  `version`, `displayName?`, `publicAddress?`, `providers[]` (announcements),
  `region`, `timestamp`, `stakeAmountUSDC?`, `capabilities?`, `sellerContract?`,
  `signature`. Verify signature + freshness; buyers also read the seller's HTTP
  `Date` header to survive a wrong local clock.

### 3.2 addressing

Buyer dials `publicAddress`, default port **6882**
(`outbound-connection.ts:7-10`). Seller ports: signaling 6882, DHT 6881
(`node.ts:256-257`).

### 3.3 shortcuts that skip the DHT entirely

- `GET https://network.antseed.com/stats` → `{peers: PeerMetadata[], updatedAt}`, no
  auth (indexer snapshot; consumed by the desktop + payments web app).
- Relay `GET /sellers` — same DHT-discovered snapshot including addresses
  (`apps/relay/README.md`).

Both hand back `PeerMetadata[]` with `publicAddress`, so a Go buyer can poll one,
then jump straight to §4. The real buyer proxy does DHT only
(`buyer-proxy.ts:1542`), but nothing protocol-wise requires it.

## 4. P2P wire protocol

### 4.1 handshake (connection auth)

First line on the TCP socket is JSON + `\n` (`connection-manager.ts:721-810`
outbound, `:856-940` inbound):

```jsonc
{"type":"intro",
 "auth":{"peerId":"<40hex>","ts":<unix-ms>,"nonce":"<32hex>","sig":"<130hex>","v":2},
 "capabilities":["transport.tcp-enc.v1", ...],
 "enc":{"pub":"<64hex X25519>","sig":"<130hex>"}}
```

- `sig` = EIP-191 `personal_sign` over
  `"antseed-msg-v1:" + "intro|<peerId>|<ts>|<nonce>|caps=<json>|enc=<pub|->"`
  (`connection-auth.ts:24-51`, `signing.ts:67-73`). ±30 s skew, nonce replay guard.
- Encrypted path: responder replies one line
  `{"type":"enc-ack","peerId","ts","nonce","pub","sig"}` signed over
  `enc-ack|<peerId>|<ts>|<nonce>|<pub>|<initiatorNonce>` (`connection-auth.ts:100-108`).
- Session keys: X25519 DH →
  `HKDF-SHA256(ikm=shared, salt=sha256("i|r|ni|nr|pi|pr"), info="antseed-tcp-enc-v1", 64B)`
  → two AES-256-GCM keys, one per direction (`secure-channel.ts:70-98`). Records after
  the newline: `u32be(len) | ciphertext||tag(16)`, IV = 12 bytes with u64be counter at
  offset 4, max plaintext 16 MiB (`secure-channel.ts:100-165`).
- The spec's `HandshakeInit`/`HandshakeAck` frames (0x01/0x02) are never sent; the
  intro line is the authentication. WebRTC DataChannel (label `"antseed-data"`) is the
  fallback transport and carries the same frames.

### 4.2 frame format

```
offset 0: u8    message type
offset 1: u32be messageId    (per-connection counter; correlation is by requestId)
offset 5: u32be payloadLength
offset 9: payload            (max 64 MiB)
```

`framing.ts:17-69` (decoder `:75-114`, mux `:122-159`, `FRAME_HEADER_SIZE = 9`).

### 4.3 message types (`packages/protocol/src/messages.ts:1-47`)

```
0x01 HandshakeInit  0x02 HandshakeAck           (unused in practice)
0x10 Ping  0x11 Pong                            (every 15s; 5s pong timeout, 3 misses = dead)
0x20 HttpRequest        0x21 HttpResponse
0x22 HttpResponseChunk  0x23 HttpResponseEnd
0x24 HttpResponseError  0x25 HttpRequestChunk   0x26 HttpRequestEnd
0x50 SpendingAuth  0x51 AuthAck
0x52 FreeUsageOpen 0x53 FreeUsageAuth  0x54 FreeUsageAck
0x55 NeedFreeUsageAuth  0x56 PaymentRequired  0x58 NeedAuth
0x59 CloseChannelRequest  0x5A CloseChannelResult
0x60 PeerReport 0x61 ReportAck  0x70 PeerRating 0x71 RatingQuery 0x72 RatingResponse
0x80 VerificationResponseAuth
0xA0 SweepRequest 0xA1 SweepReceipt
0xF0 Disconnect 0xFF Error
```

Demux per connection: 0x50–0x5F → payment mux, 0x80 → verification mux, 0xA0 → sweep
mux, rest → proxy mux (`node.ts:1600-1655`).

### 4.4 HTTP-over-P2P payloads

```ts
SerializedHttpRequest  { requestId, method, path, headers, body: Uint8Array }
SerializedHttpResponse { requestId, statusCode, headers, body }
SerializedHttpResponseChunk { requestId, data, done }
```

Binary codecs (`packages/protocol/src/request-codec.ts`):

```
HttpRequest  : [requestIdLen:u16be][id][methodLen:u8][method][pathLen:u16be][path]
               [headerCount:u16be]([kLen:u16be][k][vLen:u16be][v])* [bodyLen:u32be][body]
HttpResponse : [requestIdLen:u16be][id][statusCode:u16be]
               [headerCount:u16be]([k][v])* [bodyLen:u32be][body]
ResponseChunk: [requestIdLen:u16be][id][done:u8][dataLen:u32be][data]
```

Semantics:

- one request = one `0x20` frame; bodies > **240 KiB** switch to chunked upload:
  header frame with `x-antseed-upload: chunked` + empty body, `0x25` slices of 8 KiB,
  final `0x26` with `done=1` (`proxy-mux.ts:99-155`).
- non-streaming response: single `0x21`.
- streaming response: first `0x21` carries header `x-antseed-streaming: "1"` (start
  marker), then `0x22` chunks (`done=0`), terminal `0x23` with `done=1`
  (`proxy-mux.ts:316-341`; marker set at `packages/provider-core/src/http-relay.ts:413`).
- error: `0x24`, decoded as a normal response.
- correlation by `requestId` (uuid), never by messageId.

Required outbound headers: `x-antseed-provider`, `x-antseed-service`; body `model`
must be the seller's advertised `serviceId`.

## 5. free vs paid — why free models need no wallet

**Free is decided purely by advertised pricing**, independently on both sides:

- seller: `isFreeService = isZeroTokenPricing(pricing) && (!unitBillingModel || isFreeUnitBillingModel(unitBillingModel))`
  (`packages/node/src/seller-request-handler.ts:240-241`).
- buyer: `isBillingRouteFree` / `isPeerServiceFree`
  (`packages/buyer-core/src/buyer-request-handler.ts:676-697`), classified before the
  request is sent (`:131-136`).

Free path (`buyer-request-handler.ts:137-144`):

1. best-effort `FreeUsageOpen (0x52)`: 32-byte salt, `channelId =
   computeFreeUsageChannelId(buyerAddr, sellerAddr, salt)`, `deadline = now + defaultAuthDurationSecs`,
   EIP-712 `FreeUsageOpen` sig (`buyer-free-usage-manager.ts:137-183`). Failure is
   swallowed — the request proceeds anyway.
2. request goes out as normal.
3. after serving, seller sends `NeedFreeUsageAuth (0x55)`; buyer answers
   `FreeUsageAuth (0x53)` with cumulative token counters and amount `0`
   (`buyer-free-usage-manager.ts:246-306`); seller `FreeUsageAck (0x54)`.

**No deposit is ever checked on the free path.** The seller skips the whole 402 /
payment-channel handshake for free services (`seller-request-handler.ts:306-346`) and
skips its budget check (`:351`). The buyer's only deposit read lives inside
`handle402` (`buyer-payment-negotiator.ts:666-687`) and runs **only after a 402
arrives** — which a free seller never sends. Tests say it outright: "buyer opens and
signs free usage without deposits" (`packages/node/tests/free-usage-lifecycle.test.ts:131`).

Paid path (for completeness):

1. first paid request → seller 402 `{"error":"payment_required", minBudgetPerRequest,
   suggestedAmount, …}` + optional `PaymentRequired (0x56)` frame
   (`seller-request-handler.ts:316-331`).
2. buyer `handle402` reads the USDC balance of its address in the `AntseedDeposits`
   contract; zero → re-issues 402 `insufficient_deposits` to the client; else EIP-712
   `ReserveAuth`/`SpendingAuth` (`0x50`) with `reserveSalt/reserveMaxAmount/reserveDeadline`
   → seller `AuthAck (0x51)` → request re-sent (`buyer-payment-negotiator.ts:481-705`).
3. steady state: each response advances a **cumulative** spending authorization the
   seller can settle only up to; seller can demand `NeedAuth (0x58)` for the exact
   cumulative. Buyer never needs gas (relayed/lazy settlement), seller locks reserve.
4. contracts on Base (`chainId 8453`): `AntseedDeposits`, `AntseedChannels`, USDC;
   chain picked via `payments.crypto.chainId`, addresses resolved from chain config —
   "do not hardcode contract addresses" (`skills/join-buyer/SKILL.md`).

`preferFreePeers` (+25 score, `model-route-ranking.ts:127-131`) only nudges ranking.
Caveat that can still produce a 402 on a "free" listing: seller config advertising 0
but actually charging (announcement/config mismatch), or image/decision unit billing
with nonzero per-unit price (`billing.ts:122-125`).

## 6. model catalog

### 6.1 canonicalization (one module: `packages/node/src/model-identity.ts`)

`computeCanonicalModelKey` (`:51-62`) + `stripModelDecorations` (`:10-26`):

1. trim/lowercase, drop vendor path after last `/`.
2. strip trailing `latest`, 6–8 digit dates, `YYYY-MM(-DD)`.
3. strip flattened prefixes only before an established family (`anthropic-` before
   `claude`, `openai-` before `gpt`).
4. Claude reorder `claude-<ver>-opus` → `opus-<ver>`, strip `claude-` before
   `opus|sonnet|haiku|fable`; strip `-coding-only` inside those families only.
5. extra vendor prefixes: `google-`→`gemma`, `aion-labs-`, `zai-org-`/`z-ai-`→`glm`,
   `stealth-`→`ox-alpha`.
6. **version punctuation compaction only for established families** (`aion|opus|sonnet|haiku|fable|deepseek|e2ee-glm|gemini|gemma|glm|gpt|grok|hermes|kimi|llama|mimo|minimax|mistral|qwen|venice-uncensored`):
   drop all non-alphanumerics, so `gpt-5.6-sol` ≡ `gpt-56-sol` ≡ `openai-gpt-56-sol`
   ≡ `gpt56sol`. Everything else only drops non-dots.

Merging rule (matches the user's required version handling): grouping happens **only
when the compacted form matches exactly**. `gpt-5.6-sol` vs `gpt-5.6-luna`,
`glm-5.3` vs `glm-5.3-flash`, `deepseek-v4-pro` vs `deepseek-v4-pro:web`,
`claude-opus-4.8` vs `claude-opus-4.8-fast` all stay distinct (tests:
`model-identity.test.ts:39-41`). `minimax-m2.7` → `minimax-m3` style supersession is
**not** done by the protocol at all — both stay separate entries; supersession is a
curation decision on our side.

### 6.2 `/v1/models` response shape

Entry + per-peer offer types (`apps/cli/src/proxy/network-models.ts:28-84`):

```ts
type NetworkModelEntry = {
  id: string            // observed serviceId of the retained offer
  name: string          // display name, e.g. "GPT 5.6 Luna"
  aliases: string[]     // every observed normalized name + canonical key
  object: 'model'; created: number; owned_by: 'antseed'
  type: 'text' | 'image' | 'decision'
  supported_protocols: string[]
  context_length?, max_output_tokens?
  architecture?: { input_modalities?, output_modalities? }
  capabilities?: { reasoning?, tool_use?, structured_output? }
  supported_parameters?: string[]
  capability_coverage: { total_offers, context_length, max_output_tokens, … }
  peers: NetworkModelPeerOffer[]
}
type NetworkModelPeerOffer = {
  peerId, displayName?, provider, serviceId, protocol, protocols[], type,
  capabilities?, categories?,
  reputationScore, effectiveReputationScore, onChainReputationScore,
  inputUsdPerMillion?, outputUsdPerMillion?, cachedInputUsdPerMillion?,
  minImageUsdPerImage?, maxImageUsdPerImage?
}
```

Rules: no model-level `pricing` (pricing lives on offers); model-level capabilities
are **conservative guarantees** — min for numbers, intersection for lists, AND for
booleans, omitted when any offer omits (`:113-177`); one offer per (peer, canonical
model), cheapest alias wins (`service-catalog.ts:227-243`); model order is
**alphabetical by id** (`network-models.ts:313`) — AntSeed has no popularity/top-used
ordering anywhere in the proxy; `preferFreePeers` and price+trust only order `peers[]`.

Missing-`cachedInputUsdPerMillion` penalty: if any other offer for the model
advertises cached pricing and this one doesn't → 50% cut to effective reputation for
that model; no penalty if nobody advertises it (`using-the-api.md:113`).

### 6.3 ranking (`packages/node/src/routing/model-route-ranking.ts:106-199`)

Eligibility first (minTrustScore gate, allow/block lists, maxPricing caps), then:
eligible before cooling, known price before unknown, cheapest `input+output` (or
`minImageUsdPerImage`), `effectiveReputationScore` breaks ties, `preferFreePeers` is
a +25 blend tiebreak. Conversation affinity bumps the previous healthy seller.

### 6.4 model types

| type | trigger | endpoint |
|---|---|---|
| `text` | `anthropic-messages` / `openai-chat-completions` / `openai-responses` | `/v1/messages`, `/v1/chat/completions`, `/v1/responses` |
| `image` | `openai-images` protocol or outputs include `image` | `/v1/images/*` |
| `decision` | `typesafe-systemone` | `/v1/systemone` |

Type derivation: `service-catalog.ts:179-183`. Wrong endpoint for the type → 400
`unsupported_protocol` (`buyer-proxy.ts:2640-2657`).

Real ids seen in repo code/tests: `deepseek-v4-flash` (marked free in tests),
`minimax-m3`, `minimax-m2.7`, `kimi-k2.6`, `gpt-oss-120b`, `glm-5`/`glm-5.2`/
`glm-5-turbo`, `glm-5.3-flash` + `gpt-6-luna` (website hero), `flux.1-schnell` (image),
`jev-latest` (decision), `claude-opus-5`/`opus-5` families, `gpt-5.6-sol/luna/terra`.

## 7. the "VPN" — what it is, what it does, why it exists

**"Antseed AI VPN" is an Electron desktop app, not a VPN.** Confirmed absent: no
TUN/TAP/utun, no Wintun, no NetworkExtension, no packet capture, no route-table
edits, no VPN profile. Build artifacts are named `AntSeed-VPR-*`; VPR = *Virtual
Private Router*, the architecture name for the local router.

What the app actually does (`apps/desktop/src/`):

- runs `antseed buyer start` (port 8377) behind a power button; model/seller picker,
  per-conversation pins, internal chat, floating window, credits/payments UI, TEE
  badges (verified result: 24 h display cache, 5 min routing cache).
- **tool wiring by config rewrite**: patches `~/.claude/settings.json` env,
  `~/.codex/config.toml`, `~/.config/opencode/opencode.jsonc`, WSL configs — all with
  sidecar backups, reverted on disconnect (`config-patch.ts:1641-1664`).
- **optional HTTPS interception** for apps that can't be config-patched: generates a
  local root CA (`CN=AntSeed Local CA`, admin prompt), MITMs app traffic through a
  local proxy on `127.0.0.1:8378`, and can set the machine-wide OS secure-web-proxy
  (mac `networksetup`, Windows registry) with a 15 s fail-open watchdog
  (`system-proxy.ts:162-187`). Linux: env vars only. This is the closest thing to
  "VPN behavior" — it routes app traffic through a local hop — and it is opt-in.
- relaunches tools to pick up settings; Squirrel auto-updater only for persistence.

Why it's called a VPN (marketing, `apps/website/blog/2026-08-04-virtual-private-router-vpr.md`):
a normal VPN encrypts the pipe but the AI provider still sees the prompt and the
account; AntSeed's pitch is spreading each request across independent pseudonymous
sellers so no single company sees identity + prompt, no account exists, prompts never
tie to a billing profile. The blog states explicitly that region-shifting is **not**
its job and a real VPN is the right tool for geo-blocks (and the two stack — the
desktop app actually advises *disconnecting* your VPN when P2P fails,
`NetworkAlertBanner.tsx:17`). Only region constraint found anywhere: Stripe card
checkout is US-only.

Bottom line for us: **you do not need the app, and you do not need any VPN.** The
headless CLI buyer does everything (`antseed buyer start`), and a reimplementation
needs only what §3–§5 describe. Gateway/tunnel only matter if you expose the buyer
beyond loopback.

## 8. identity and signing

- key source: `ANTSEED_IDENTITY_HEX` (64 hex, optional `0x`, deleted from env after
  read) → else `<data-dir>/identity.key` (0600) → else generate `randomBytes(32)`
  (`packages/node/src/p2p/identity.ts:79-133`).
- secp256k1 (ethers `Wallet`); **peerId = lowercase 40-hex EVM address** (no `0x`) =
  both the P2P identity and the payment wallet address.
- signing primitives (`packages/protocol/src/signing.ts`): `signData` = EIP-191 with
  prefix `"antseed-data-v1:"`, `signUtf8` = prefix `"antseed-msg-v1:"`, 65-byte sigs.
- signed when: connection `intro`/`enc-ack` lines (§4.1, ±30 s, nonce-replayed);
  EIP-712 `FreeUsageOpen`/`FreeUsageAuth` per free channel/request (§5); EIP-712
  `ReserveAuth`/`SpendingAuth` (domain `AntseedChannels` v1) per paid step; seller
  signs a `ResponseAuthPayload` per response when it advertises
  `verification.response-auth.v1` (`buyer-core/src/response-auth.ts:37-54`).
- the inference `HttpRequest` frame itself carries **no signature** — authenticity
  comes from the authenticated connection.

## 9. bare minimum to get models working

### 9.1 fastest (reference implementation, free models only)

```bash
node ≥ 20, npm, outbound UDP 6881 + TCP 6882   # no account, no API key, no VPN
npm install -g @antseed/cli
antseed buyer start                              # 127.0.0.1:8377, defaults, no config
curl -s localhost:8377/v1/models | jq '.data[].id'          # network catalog, free
curl localhost:8377/v1/chat/completions -H 'content-type: application/json' \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}'
```

Identity auto-generates (`~/.antseed/identity.key`). Zero-price offers are usable
with **no deposit**; paid offers need `antseed buyer deposit` (USDC on Base).
(`skills/join-buyer/SKILL.md`, `apps/website/docs/guides/using-the-api.md:21-52`.)

### 9.2 minimal Go reimplementation for FREE models (the required subset)

1. **identity**: secp256k1 key + EIP-191 (`antseed-msg-v1:`) signing; peerId = keccak-derived
   address.
2. **discovery**: poll `network.antseed.com/stats` or relay `/sellers`
   (skip the DHT), filter `publicAddress`, fetch + verify `GET /metadata`.
3. **transport**: TCP dial `:6882` → `intro` line → verify `enc-ack` → X25519 →
   HKDF → AES-256-GCM records → 9-byte frames → answer Ping.
4. **request**: `encodeHttpRequest` → `0x20` (chunk >240 KiB) with
   `x-antseed-provider`/`x-antseed-service` and seller's `serviceId` in `model`;
   read `0x21` (`x-antseed-streaming: "1"` → `0x22`…`0x23`).
5. **free usage**: send best-effort `FreeUsageOpen (0x52)` (EIP-712), answer
   `NeedFreeUsageAuth (0x55)` with `FreeUsageAuth (0x53)`. Failure tolerated —
   the request still works.
6. **catalog + routing**: build `/v1/models` from cached metadata (canonical keys §6.1,
   conservative caps §6.2), rank offers (§6.3).
7. **format translation**: Anthropic ↔ OpenAI chat (nimroute already has both for zen).

Can be skipped for free models: gateway, tunnel, desktop, WebRTC fallback, DHT,
payment contracts/USDC/deposits, sweep, ratings/reports, TEE verification, image and
decision endpoints, session pins, conversation affinity, x402.

**Source files for everything above**: `apps/cli/src/proxy/buyer-proxy.ts` (HTTP),
`packages/node/src/{node,discovery/*,p2p/*,routing/model-route-ranking}.ts`,
`packages/protocol/src/{messages,framing,http,request-codec,connection-auth,signing,peer-metadata}.ts`,
`packages/buyer-core/src/{buyer-request-handler,buyer-payment-negotiator,buyer-free-usage-manager,proxy-mux}.ts`,
`apps/website/docs/guides/{using-the-api,gateway-api-keys,vpr,agents,public-tunnels}.md`,
`docs/protocol/spec/00-09`, `skills/join-buyer/SKILL.md`, `apps/relay/README.md`.
