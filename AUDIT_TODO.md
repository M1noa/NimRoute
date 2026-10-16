# NimRoute — audit todo

Checkbox list for every finding in `AUDIT_REPORT.md`, grouped by category. One
box per fix. Audit coverage is complete; the boxes below are the work.

## Performance

- [ ] **P1 HIGH** — `main.go:3652`: stream `lastResp.Body` for `isStream` in
      `handleNvidia` instead of `ReadAll` + re-serve; tee only the usage tail.
- [ ] **P2 MEDIUM** — `main.go:43`: lower `upstreamBodyLimit` to 4–8MB once P1
      streams; stop the debug mode second copy on streamed responses.
- [ ] **P3 MEDIUM** — `route.go:76`/`:101`: cache the cross-provider route
      table; never block a request or a `/status` poll on `fetchZenModelIDs()`.
- [ ] **P4 MEDIUM** — `debugtrace.go:281`: prune `--dbg-file` dumps to a max
      count/size; document that dumps contain request/response bodies.

## Improvement

- [ ] **I1 MEDIUM** — `zenproxy.go:513`: count and log verification outcomes
      per sweep (verified / 429 / 403 / other / dial error).
- [ ] **I3 MEDIUM** — `usage.go:112`: cap `outcomesByModel` (or record only
      catalog ids) so model health cannot grow per client-chosen model string.
- [ ] **I2 LOW** — `main.go:365` + `main.go:80`: evict expired entries instead
      of refusing inserts past 4096 (`lastKey`, `ModelLock`).
- [ ] **I4 LOW** — `route.go:424`: record latency only for the winning
      candidate on the anthropic race path, as `route.go:323` does.

## QoL

- [ ] **Q1 MEDIUM** — `config.go:547`: warn on config keys that are not in
      `config.yml.example` (warn only, never fail the load).
- [ ] **Q2 LOW** — `main_test.go:2570`/`:2857`/`:639`, `todo_small_test.go:113`:
      make these tests build their own fixtures instead of skipping; leave the
      env-gated skips (native model files, live quota probe) as they are.

## Sloppy code

- [ ] **S1 LOW** — `freepi.go:557`/`:581`: classify freepi failures by
      `fe.Code`, not by substring of the message.
- [ ] **S2 LOW** — `modelinfo.go:357`: anchor `globMatch` (prefix + suffix) or
      document the substring semantics for `route.exclude_models` /
      `claude_map` / nudge overrides.

## Bad code

- [ ] **B1 INFO** — `antseedp2p.go:417`: `uint16be` returns an error instead
      of panicking, or pin every call site's <64KB invariant with a test.
- [ ] **B2 INFO** — `zentools.go:22`: leave the build-time-constant panic, or
      add a decode test so a malformed ballast fails in CI instead of at start.

## Dependency

- [ ] **D1 INFO** — `go.mod`: `onnxruntime-purego` is pinned to a
      pseudo-version; move to a release tag when upstream publishes one.

## Audit coverage (complete)

### Read in full
- [x] `main.go`, `config.go`, `zenproxy.go`, `zenlanes.go`, `freepi.go`,
      `antseedp2p.go`, `usage.go`, `route.go`, `anthropic.go`, `zentools.go`,
      `piiwiring.go`, `rtkcompress.go`, `rtkdetect.go`.
- [x] `.github/workflows/build.yml`, `go.mod`.

### Targeted review
- [x] `nous.go` — key pool, forwarding, both handlers, `/status` section.
- [x] `openrouter.go` — refresh, stale-cache rule, overlay, extras.
- [x] `debugtrace.go` — capture, end, `dumpIfErr`, redaction.
- [x] `modelinfo.go` — `globMatch`, catalog lookup, fallbacks.
- [x] `rtk.go` — filters, fail-open guards, helpers.
- [x] `antseed.go` — `antRefresh` throttle.
- [x] `nimstatus.py`, `scripts/**` — secret printing, error handling.
- [x] `pii/*.go` — vault/scan TTLs, caps, LRU, sweep isolation.
- [x] `main.go` CLI path (`probe`, `models sync`).

### Scanned
- [x] `benchmarks.go`, `pii/fetch_model.sh`, `nim`, `docs/**`,
      `config.yml.example`.
- [x] The 27 Go test files (353 test funcs, 11 skip sites classified).

### Skipped (vendored / data)
- [x] `system-prompts/**`, `guardrails.json`, `aa_benchmarks.json`,
      `openrouter_models.json`, `keys.jsonc`, `.zenpool.json`,
      `testdata/*.json`, `scripts/opencode_ballast.json`.

## Verification (when fixes land)

- [ ] `gofmt -l .`, `go vet ./...`, `go test ./...` green.
- [ ] `go vet -tags ner ./...`, `go test -tags ner ./pii/...` green.
- [ ] Streamed first-byte latency measured on the NIM path before/after P1.
- [ ] `route/` request with an empty `ocInfo` returns without a 10s stall (P3).
- [ ] `/status` health payload size unchanged after a loop of unique model ids
      (I3).
