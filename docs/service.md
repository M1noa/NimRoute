# Service & CLI

`./nim` manages the proxy in the foreground or as a user service (`launchd` on macOS, `systemd --user` on Linux, `nohup` fallback elsewhere).

## `./nim` commands

| Command | Description |
|---|---|
| `./nim run [flags]` | Run in the foreground (passes extra flags to `./nimroute`) |
| `./nim start [flags]` | Start the background service |
| `./nim stop` | Stop the background service |
| `./nim restart [flags]` | Restart the background service |
| `./nim status` | Show service state + live dashboard |
| `./nim status tail` | Live-updating dashboard (`./nim status-tail` also works) |
| `./nim models [N]` | Show scored models ranked by intelligence index (optional limit `N`) |
| `./nim models sync` | Refresh `model_info.jsonc` / benchmark catalogs |
| `./nim logs` | Show dashboard + last 10 access log lines |
| `./nim log [N\|tail]` | Show last `N` log lines, or follow the log |
| `./nim tail` | Follow `nimroute.log` |
| `./nim freepi auth` | Run GitHub device login for FreePI and save the account to `config.yml` |
| `./nim install [flags]` | Install and start the user service |
| `./nim uninstall` | Stop and remove the user service |
| `./nim help` | Print CLI usage |

## `./nimroute` binary flags

| Flag | Description |
|---|---|
| `--config PATH` | Config file path (default `config.yml`, or `CONFIG_FILE` env) |
| `--port N` | Listen port `1..65535` (overrides `server.port` and `PORT` env) |
| `--lanes N` | Override `zen.lanes` count |
| `--debug` | Enable verbose debug logging |
| `-df`, `--dbg-file[=DIR]` | Write JSON dumps of failed requests (default dir `nimroute-debug/`, pruned to 200 files / 64MB) |
| `-h`, `--help` | Show help |

Subcommands:
- `./nimroute probe [--config PATH]` — test configured `nvidia_keys` against NIM models
- `./nimroute models sync [--config PATH]` — sync model catalog metadata

## Environment variables

| Variable | Effect |
|---|---|
| `PORT` | Override listen port |
| `CONFIG_FILE` | Override `config.yml` path |
| `NIM_AUTH` | Bearer token used by `./nim status` when querying a locked `/status` |
| `CORS_ORIGINS` | Comma-separated allowed browser origins |
| `DEBUG` | Set to `1` for verbose debug logs |
| `DEBUG_FILE` | Directory for failed-request JSON dumps (`1` uses `nimroute-debug/`) |
| `MODEL_INFO_FILE` | Custom path for `model_info.jsonc` |
| `AA_BENCH_FILE`, `OPENROUTER_FILE` | Custom paths for cached benchmark/OpenRouter JSON |
| `RAMPART_MODEL`, `RAMPART_VOCAB`, `ORT_LIB` | Custom paths for the optional ONNX NER PII model |

## Configuration (`config.yml`)

On first run, NimRoute copies `config.yml.example` to `config.yml` (with `0600` permissions) and migrates any legacy `keys.jsonc`.

- **Hot-reload**: Saving `config.yml` applies changes within ~2 seconds.
- **Safe reload**: If you save invalid YAML, NimRoute logs a warning and keeps the previous working config in memory.
- **Schema upgrades**: New keys added to `config.yml.example` are appended to your `config.yml` as comments without touching your existing settings.
