# NimRoute

> THIS PROJECT WAS MADE PARTIALLY USING AGENTIC AI CODING TOOLS

[![build](https://github.com/M1noa/NimRoute/actions/workflows/build.yml/badge.svg)](https://github.com/M1noa/NimRoute/actions/workflows/build.yml)
[![go](https://img.shields.io/badge/go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![license](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![openai](https://img.shields.io/badge/API-OpenAI%20%2B%20Anthropic-412991)](docs/backends.md)
[![platforms](https://img.shields.io/badge/platforms-macOS%20%7C%20Linux%20%7C%20Windows-informational)](docs/service.md)

NimRoute is a local OpenAI- and Anthropic-compatible LLM proxy in Go. It pools free models across five providers and rotates API keys, proxy exits, and sessions so per-exit rate limits cost a fast retry instead of a broken turn.

Point your client at `http://localhost:5419` and pick any model from `/v1/models`.

## Quick start

Requires Go 1.26+.

```sh
git clone https://github.com/M1noa/NimRoute && cd NimRoute
go build -o nimroute .
./nim run
```

First run creates `config.yml` from `config.yml.example`. Edits to `config.yml` hot-reload in seconds without restarting.

Test it:

```sh
curl -s localhost:5419/v1/models | head

curl -s localhost:5419/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{"model":"opencode/step-5-preview-free","messages":[{"role":"user","content":"hi"}]}'
```

## Backends

| Prefix | Provider | Auth needed | How it works |
|---|---|---|---|
| `opencode/` | OpenCode Zen | none | keyless free tier; rotates verified proxy exits + sessions across lanes |
| `freeepi/` | FreePI | `./nim freepi auth` | ad-supported free models; 1 stream per account, rotates + queues |
| `antseed/` | AntSeed | none | peer-to-peer sellers discovered via stats snapshot + DHT |
| `nous/` | Nous Portal | `nous_keys` | zero-priced catalog models with per-key 429 rotation |
| `nvidia/` | NVIDIA NIM | `nvidia_keys` | pooled NIM API keys (`40 RPM × N keys`), exponential 429 cooldown |
| `route/` | multi-backend | mixed | tries every backend that serves a model until one answers `200` |

`opencode/*` and `antseed/*` work out of the box with zero keys. See [docs/backends.md](docs/backends.md) for provider setup.

## Connect your client

- **OpenAI-compatible clients** (Cursor, Continue, OpenWebUI, SDKs):
  - Base URL: `http://localhost:5419/v1`
  - API key: `dummy` (or one of `auth.tokens` if you locked down the proxy)
- **Claude Code / Anthropic clients**:
  ```sh
  ANTHROPIC_BASE_URL=http://localhost:5419 claude
  ```

Endpoints: `/v1/chat/completions`, `/v1/responses`, `/v1/messages`, `/v1/messages/count_tokens`, `/v1/models`, `/status`, `/health`.

## CLI & service

```sh
./nim install       # install + start background service (launchd / systemd / nohup)
./nim status        # live dashboard (lanes, proxy pool, model scores, token usage)
./nim status tail   # auto-refreshing dashboard
./nim models 20     # top 20 models ranked by Artificial Analysis intelligence index
./nim logs          # status + last 10 log lines
./nim restart       # restart background service
```

Full CLI and flag reference: [docs/service.md](docs/service.md).

## Optional extras

All configured in `config.yml`:

- **PII anonymizer (`anonymize`, beta, off by default)**: replaces secrets, emails, phones, IPs, and names with reversible session tokens before requests leave your machine, and restores them in the streamed response. May miss spans or alter text you wanted verbatim — verify on your own payloads first. Optional ONNX NER (`go build -tags ner`).
- **RTK (`rtk`, beta, off by default)**: trims huge `tool_result` blocks (git diffs, test logs, file dumps) before sending upstream. Fail-open.
- **Guardrails (`guardrails`, recommended, on by default)**: blocks or strips disallowed prompt patterns.
- **Usage log (`usage`, on by default, toggle with `usage.enabled`)**: appends per-request token stats to `nim-usage.jsonl` for `/status`.

## Docs

- [docs/backends.md](docs/backends.md) — provider setup, keys, and routing rules
- [docs/service.md](docs/service.md) — `./nim` CLI, binary flags, and environment variables
- [docs/troubleshooting.md](docs/troubleshooting.md) — common status codes and debug dumps
- [docs/architecture.md](docs/architecture.md) — request pipeline, lanes, and proxy pool internals
- [docs/development.md](docs/development.md) — building, testing, and contributing
- [docs/reference/](docs/reference/) — protocol notes for AntSeed, FreePI, and RTK

## License

MIT — see [LICENSE](LICENSE) and [NOTICE](NOTICE).
