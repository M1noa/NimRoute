# NimRoute — audit plan

Ordered fix plan for the findings in `AUDIT_REPORT.md`. CRITICAL → INFO; each
item names the owner file, the effort, and what it blocks or depends on.

## 1. P1 — stream the NIM path instead of buffering it (HIGH)

- **File:** `main.go` — `handleNvidia` (~3544), the read at 3652, the write at 3760.
- **Effort:** ~1–2h.
- **Change:** for `isStream && status == 200`, hand `lastResp.Body` to
  `copyZenChatStream` (already does ctx-aware piping, flushing and a usage tee)
  and drop the full `ReadAll`; keep the buffer only for the non-stream branch
  and for token parsing.
- **Blocks:** P2 — the cap can only come down once the big bodies stream.
- **Test:** assert first-byte-before-body-complete with a slow fake upstream
  (`stream_cancel_test.go` is the template).

## 2. P2 — make `upstreamBodyLimit` a real bound (MEDIUM)

- **File:** `main.go:43`, plus each read site.
- **Effort:** ~30m, after P1.
- **Change:** 4–8MB for buffered paths, streaming for the rest; skip the debug
  response copy when the body is streamed.
- **Depends on:** P1.

## 3. P3 — stop rebuilding the route table per request (MEDIUM)

- **File:** `route.go:76` (`buildRouteTable`), the fallback at `route.go:101`.
- **Effort:** ~1h.
- **Change:** cache the table behind a short TTL / catalog-version check, and
  make the `fetchZenModelIDs()` fallback non-blocking (serve what is cached,
  kick a refresh in the background). Route requests must never wait on a
  listing fetch; `/status` polling at 1Hz must not re-union four catalogs.
- **Test:** a `route/` request with an empty `ocInfo` returns promptly instead
  of blocking on the 10s `GET /models` timeout.

## 4. P4 — bound the `--dbg-file` dumps (MEDIUM)

- **File:** `debugtrace.go:281` (`dumpIfErr`), `docs/troubleshooting.md:33`.
- **Effort:** ~30m.
- **Change:** prune the dump dir to a max file count (and/or total bytes) at
  dump time — oldest first, same shape as the usage-file rotation story. Note
  in the docs that dumps carry request/response bodies, not just redacted
  headers.
- **Value:** the flag exists to debug 429/5xx storms, which is precisely when
  one file per failure arrives.

## 5. I3 — cap `outcomesByModel` (MEDIUM)

- **File:** `usage.go:112` (`noteModelOutcomeLocked`), `usage.go:197`/`:200` for
  the existing pattern, `main.go:707` (`StatusFor` health field).
- **Effort:** ~30m.
- **Change:** fold overflow past a cap into `(other)` exactly as `totals` does,
  or record outcomes only for ids that exist in the catalog/listing.
- **Why:** `totals` is capped for this reason and its sibling is not; the leak
  reaches `/status`, which clients poll continuously.

## 6. Q1 — warn on unknown config keys (MEDIUM)

- **File:** `config.go:547` (`loadConfigFile`).
- **Effort:** ~30m.
- **Change:** `yaml.Decoder` with `KnownFields(true)`, or decode into a node
  and diff keys against the example; log unknown keys as warnings, never fail
  the load (a typo must not take the proxy down).
- **Risk:** `KnownFields(true)` rejects the whole document on the first unknown
  key, so the diff-based warning is the safer shape.

## 7. I1 — verification status mix (MEDIUM)

- **File:** `zenproxy.go:513` (`verifyProxy`), `refreshZenProxies`.
- **Effort:** ~30m.
- **Change:** return the status code and count them per sweep
  (`verified / 429 / 403 / non-200 / dial-error`), log the totals with the
  sweep line. Only if 429/403 turns out to be a real share does it become a
  "retry verification later" story.
- **Value:** makes pool shrinkage self-explaining.

## 8. Q2 — make the four silent-skip tests own their fixtures (LOW)

- **File:** `main_test.go:2570`, `:2857`, `:639`, `:3764`,
  `todo_small_test.go:113` (5 of the 10 `t.Skip` sites).
- **Effort:** ~1h (the pool one is the real work).
- **Change:** build the pool the pin/rotation test needs instead of skipping
  when there is none; `t.Fatal` when a fixture the test itself created is
  missing (including the `scripts/opencode_ballast.json` copy, which is
  committed, so its absence is a broken checkout, not a missing environment).
  Leave the five correctly env-gated skips (native model files, live quota
  probes) alone.

## 9. I4 — winner-only latency on the anthropic race path (LOW)

- **File:** `route.go:424` (and `:323` for the guarded shape).
- **Effort:** ~15m.
- **Change:** record latency only for a candidate whose recorder won, as the
  openai-compat path does.
- **Depends on:** nothing; independent of P3.

## 10. S1 — classify freepi failures by code (LOW)

- **File:** `freepi.go:557` (`freepiRetryable`) and `:581` (`freepiClientStatus`).
- **Effort:** ~15m.
- **Change:** thread `fe.Code` (or a small struct) through the exhausted-fetch
  return instead of string-matching the message.

## 11. I2 — evict instead of capping (LOW)

- **File:** `main.go:365` (`noteKey`), `main.go:80` (`ModelLock.lock`).
- **Effort:** ~20m.
- **Change:** on insert past the cap, drop one expired entry; if none expired,
  drop the oldest. Same shape in both maps.

## 12. S2 — anchor `globMatch` or document it (LOW)

- **File:** `modelinfo.go:357`; callers at `route.go:38`, `anthropic.go:123`,
  `modelinfo.go:340`.
- **Effort:** ~20m + a table test.
- **Change:** require the first part to be a prefix and the past part a suffix,
  or keep the substring behaviour and say so in the comment plus
  `config.yml.example` (its callers configure it as if it were a glob).

## 13. B1 / B2 — panics (INFO)

- **File:** `antseedp2p.go:417` (`uint16be`), `zentools.go:22`.
- **Effort:** ~15m.
- **Change:** `uint16be` returns `(b []byte, err error)`, or keep the panic and
  pin the <64KB invariant with a test over every call site. For `zentools`, the
  panic is on a build-time constant — leave it, or add a decode test so a
  malformed ballast fails in CI rather than at start.

## Verification (when fixes land)

- [ ] `gofmt -l .`, `go vet ./...`, `go test ./...` green.
- [ ] `go vet -tags ner ./...`, `go test -tags ner ./pii/...` green.
- [ ] Streamed first-byte latency measured on the NIM path before/after P1.
- [ ] `route/` request with an empty `ocInfo` returns without a 10s stall (P3).
- [ ] `/status` health payload size unchanged after a loop of unique model ids
      (I3).
