# Troubleshooting

Most `429`, `403`, and `502` entries in `nimroute.log` are individual proxy exits being rotated out mid-request. As long as the request finishes with `<- 200`, the rotation worked as intended.

## Client HTTP errors

| Status | Cause | Fix |
|---|---|---|
| `401 Unauthorized` | `auth.tokens` is configured in `config.yml` and your client didn't send a matching token | Pass `Authorization: Bearer <token>` or `x-api-key: <token>` |
| `404 Not Found` | Model ID isn't in `/v1/models`, or its backend is disabled | Check `curl -s localhost:5419/v1/models` for the exact ID |
| `429 Too Many Requests` | All keys for that backend are cooling (Nous keys), or a backend we don't retry returned one | Wait a moment, add another key, or use `route/<model>` |
| `502 Bad Gateway` | Every retry failed to reach upstream (e.g., all AntSeed peers failed) | Check `./nim status`; run `./nim restart` if the pool is stuck |
| `503 Service Unavailable` | The proxy ran out of capacity for that model: no usable Zen exit (`proxy_exhausted`, with a `Retry-After`), every FreePI account out of hourly completions, or a backend with no configured keys | Check `./nim status` pool count; for FreePI retry after the hour rolls over, or add another account. Never a wait on your side: a Zen retry already walked the whole pool |

## Log messages (`./nim logs`)

| Log message | What it means |
|---|---|
| `opencode rate-limited 429 (retry X/6) ... rotating exit+session` | Normal per-exit rate limit; lane immediately switched to a new proxy exit and session |
| `opencode free-tier-blocked 403 ... switching lane` | That proxy exit is burned for this model for 30m (`exit_burn_minutes`); lane switched exits |
| `opencode geo-blocked` / `user-blocked` | Proxy exit is in a blocked region or IP-banned; dropped from pool |
| `zen exit ... rate-limited for <model> until next refresh (429 x2 in a row|N+ 429s this cycle)` | That exit is busy for that model only; it keeps serving other models, and the next 30m refresh clears the mark |
| `zen pool dry: N marked exit(s), sweeping for fresh exits` | Every exit is marked for the requested model; the pool is refilled from exits other than the ones that just 429'd |
| `freepi 429 [hourly_ceiling]` | FreePI account hit its hourly free cap; the client sees a 503 naming the cap, not the 429 |

## Capturing failed requests

To inspect the exact request and upstream response on failures:

```sh
./nim run -df
# or on the background service:
./nim restart --dbg-file=/tmp/nim-dumps
```

Each failed hop writes a JSON trace (`0600` permissions). Traces carry request
and response *bodies*, not just headers — credential headers are masked, prompts
are not — so keep the directory private and delete it when you are done.

The directory is pruned at each dump to the newest 200 files / 64MB, so leaving
the flag on cannot fill the disk during a long storm. Only the file written most
recently is immune to pruning.

## Quick health checks

```sh
./nim status                          # service state, lanes, pool, model health
curl -s localhost:5419/health         # basic liveness check
curl -s localhost:5419/v1/models      # list all active models
```
