# Backends

Every model ID starts with a provider prefix (`opencode/`, `freeepi/`, `antseed/`, `nous/`, `nvidia/`, or `route/`). The proxy picks the upstream from that prefix and strips it before forwarding.

`/v1/models` returns the live merged catalog. Context window sizes, max output limits, and nudge settings live in `model_info.jsonc` and hot-reload with `config.yml`.

## `opencode/` — OpenCode Zen

Keyless free tier. Works immediately on first run with no account or API key.

| | |
|---|---|
| Upstream | `https://opencode.ai/inference/v1` |
| Auth | none (`Authorization: Bearer public`) |
| Config section | `zen` |

NimRoute mimics the official OpenCode CLI headers (`x-opencode-client`, `x-opencode-session`, `x-opencode-session-id`, `x-session-affinity`, `x-opencode-project`, and a live `opencode/<version>` User-Agent refreshed from GitHub every 6h). When a request includes `tools`, NimRoute appends the 12 standard OpenCode CLI tool definitions required by the free-tier gate.

- `muse-spark-*` models talk to `/responses`; all other Zen models use `/chat/completions`. NimRoute converts request and response shapes automatically.
- Each concurrent conversation pins a unique verified proxy exit and session ID ("lane"). On a `429` or `403 FreeTierError`, the lane immediately rotates to a fresh exit and session without waiting.

## `freeepi/` — FreePI

Ad-supported free models from `https://api.freepi.ai`.

```sh
./nim freepi auth    # GitHub device login; writes account JWT into config.yml
```

| | |
|---|---|
| Upstream | `https://api.freepi.ai` |
| Auth | 90-day account JWT in `freepi.accounts` |
| Config section | `freepi` |

FreePI allows one in-flight stream per account at a time (a second stream returns `409`). NimRoute rotates across configured accounts and queues up to `freepi.queue_wait_secs` (default `60s`) when all accounts are busy. Ad fetches and impressions run silently in the background. Account JWTs are never logged or exposed in `/status`.

## `antseed/` — AntSeed

Keyless peer-to-peer inference network.

| | |
|---|---|
| Upstream | Dialed P2P sellers (discovered via stats snapshot + DHT fallback) |
| Auth | none (ephemeral secp256k1 identity generated in memory at startup) |
| Config section | `antseed` |

NimRoute discovers zero-price peers from the AntSeed network snapshot and verifies that every peer address is a global unicast IP before dialing (blocking loopback, RFC1918, and link-local SSRF vectors). Retryable errors cool a seller briefly; `400` bad-request errors return straight to the caller without penalizing the peer.

## `nous/` — Nous Portal

Zero-priced models from `https://inference-api.nousresearch.com/v1`.

| | |
|---|---|
| Upstream | `https://inference-api.nousresearch.com/v1` |
| Auth | Bearer API keys in `nous_keys` |
| Config section | `nous`, `nous_keys` |

NimRoute filters the Nous catalog to models where both `pricing.prompt == 0` and `pricing.completion == 0` (some free Nous models have `:free` in the ID and some don't; paid and non-zero embedding models are excluded). A `429` cools that key and rotates to the next available key.

## `nvidia/` — NVIDIA NIM

Pooled API keys for `https://integrate.api.nvidia.com/v1`.

| | |
|---|---|
| Upstream | `https://integrate.api.nvidia.com/v1` |
| Auth | Bearer API keys in `nvidia_keys` |
| Config section | `nvidia`, `nvidia_keys` |

Each NVIDIA key allows ~40 RPM. NimRoute weights key selection toward idle keys with the fewest recent failures and applies exponential backoff + per-model lockouts on `429`. Both `nvidia/<org>/<model>` and bare `<org>/<model>` IDs work. If `nvidia_keys` is empty or `nvidia.enabled: false`, NIM requests return `503` while other backends keep running.

## `route/` — Cross-backend routing

`route/<model-slug>` tries every backend that serves that model until one returns `200`.

For example, `route/deepseek-v4.1-flash` can try `opencode/deepseek-v4.1-flash-free` and `freeepi/deepseek-v4.1-flash` in order (`priority` or `fastest` strategy in `config.yml`).
