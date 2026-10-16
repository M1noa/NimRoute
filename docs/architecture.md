# Architecture

NimRoute sits between OpenAI/Anthropic clients and five free-tier LLM upstreams (`opencode/`, `freeepi/`, `antseed/`, `nous/`, `nvidia/`), plus a cross-provider `route/` fan-out layer.

## Request pipeline

1. **Ingress (`ServeHTTP` in `main.go`, `handleAnthropic` in `anthropic.go`)**:
   - Caps request bodies at 10 MB (`bodyLimit`) with a 30s read timeout.
   - Open endpoints: `/status`, `/health`, `/v1/models`.
   - Protected endpoints (when `auth.tokens` is set): `/v1/chat/completions`, `/v1/responses`, `/v1/messages`, `/v1/messages/count_tokens`, `/v1/messages/classifier`.
2. **Pre-processing**:
   - Normalizes camelCase parameters and strips internal `"_nim"` fields (`injectParams`).
   - Repairs mismatched base64 image MIME labels (`fixDataURLMime`).
   - Optionally runs **Guardrails** (`guardrails.go`), **RTK** tool-result compression (`rtk.go`), and **PII Anonymization** (`piiwiring.go`).
3. **Dispatch & rotation**:
   - Acquires a concurrency slot (`sem_limit`, default 15) and dispatches by model prefix.
   - Translates between Anthropic `/v1/messages`, OpenAI `/v1/chat/completions`, and OpenAI `/v1/responses` as needed.
4. **Egress**:
   - Restores any PII surrogates across both streaming SSE chunks and non-streaming JSON bodies.
   - Records token usage (`prompt_tokens`, `completion_tokens`, `total_tokens` straight from upstream) to `nim-usage.jsonl` and in-memory `/status` counters.

## Zen lanes & proxy pool (`zenlanes.go`, `zenproxy.go`)

OpenCode Zen enforces rate limits per IP exit and per session. NimRoute spreads load across **lanes**:

- **One exit per lane**: Each lane owns a distinct verified proxy exit and `ses_*` session ID. Two lanes never share the same proxy exit (`lanePinnedElsewhere`, `pinUniqueProxy`).
- **Instant rotation**: On a `429`, `529`, or `403 FreeTierError`, the request hands off to another lane (`laneFailover`, preferring lanes that already hold a working exit) and rotates exit + session immediately (`laneRotate`).
- **Burn tracking**:
  - `403 FreeTierError` burns that `(exit, model)` pair for `exit_burn_minutes` (default 30m).
  - Repeated `429` streaks with no recent success long-burn the exit across all models for 12h (`noteExit429`) and drop it from the pool.
  - TLS MITM errors ban the exit for 1h (`banTLSExit`).
  - When all pool exits are taken or burned, `recheckAllExits` clears transient burns and triggers an async pool refresh.

## Key pools (`main.go`, `nous.go`)

NVIDIA NIM (`nvidia_keys`) and Nous Portal (`nous_keys`) each manage independent key pools:
- Keys cool down on `429` with exponential backoff (`5s` up to `60s`) and track a 50-minute sliding failure window.
- Key selection prefers idle keys with the lowest recent failure weight.

## PII vault (`pii/`)

When `anonymize.enabled: true`, outbound text is scanned by regex heuristics (`pii/detect.go`) and optionally the Rampart ONNX NER model (`pii/ner_ort.go`, built with `-tags ner`). Detected spans are replaced with deterministic session surrogates stored in `pii/vault.go` and restored on the response stream.

## RTK compression (`rtk.go`, `rtkcompress.go`, `rtkdetect.go`)

RTK inspects `tool_result` blocks over `rtk.min_bytes` (default 1500B), classifies their content (git diffs, logs, test output, JSON, source code, directory trees), and compacts boilerplate. Errors (`is_error: true`) are never compressed, and any compressor failure returns the original text untouched.
