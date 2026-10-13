# NimRoute
> THIS PROJECT WAS MADE PARTIALLY USING AGENTIC AI CODING TOOLS

OpenAI/Anthropic-compatible proxy with five backends: **opencode zen free models** (`opencode/<model>`, keyless), **freepi ad-supported models** (`freeepi/<model>`, needs `./nim freepi auth`), **antseed p2p models** (`antseed/<model>`, keyless), **Nous Portal free models** (`nous/<model>`, needs keys), and **NVIDIA NIM** (`nvidia/<model>` key pool, needs keys). `route/<model>` fans out across backends.

```sh
go build -o nimroute .
./nimroute          # :5419, creates config.yml on first run
```

```sh
curl -s http://localhost:5419/v1/models            # models
curl -s -X POST http://localhost:5419/v1/chat/completions -H 'Content-Type: application/json' -d '{"model":"opencode/space-bunny-free","messages":[{"role":"user","content":"hi"}]}'
```

## Setup

`config.yml` (hot-reloaded) covers everything: `nvidia_keys`, `nous_keys`, `auth.tokens` (empty = open proxy), guardrails, anonymize, zen pool. Without keys, NIM and Nous models 503 and `opencode/*` keeps working.

Nous Portal serves only the zero-priced models from its catalog, picked by price rather than the `:free` id suffix — several free ids carry no suffix and no paid id carries one.

## Clients

Point any OpenAI client at `http://localhost:5419/v1` with any key. Claude Code via `ANTHROPIC_BASE_URL=http://localhost:5419` (`models.claude_map` routes `claude-*` names, keyless by default). Also speaks `/v1/responses`.

## Service

`./nim run|start|stop|restart|status [tail]|status-tail|logs|log [N|tail]|tail|install|uninstall|freepi auth` (launchd/systemd/nohup; system-wide templates in `deploy/`).

`/status` shows pool health (keys/lanes hidden without a token when auth is on). Requests logged to `nim-usage.jsonl`. Env: `PORT` (overrides `server.port`), `CONFIG_FILE`, `NIM_AUTH` (authenticates `nimstatus.py` against `/status` only; server auth is `auth.tokens`), `DEBUG=1`, `CORS_ORIGINS`, `ZEN_CAPTURE=1` (debug traffic capture, owner-only files).

```sh
go test ./...
```

MIT — see [LICENSE](LICENSE).
