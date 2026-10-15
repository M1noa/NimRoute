# NimRoute — full read-only audit

- **Target**: `/Users/minoa/Documents/Projects/NimRoute`, commit `5f87378`, working tree dirty (7 modified, `antseed_test.go` untracked)
- **Scope**: 88 source files (Go 23.6k LOC, Python 1.8k, shell/deploy/CI, data files, docs)
- **Mode**: read-only. No source file was modified, created, or deleted. This report is the only write.
- **Method**: 14 parallel auditors split by module, each reading its whole scope end-to-end. Every finding is quoted from the source at the cited line. Every CRITICAL/HIGH claim was then re-read independently against the source before inclusion; three auditor claims were corrected in that pass (noted inline). The four areas whose subagents did not return usable reports — `pii/`, zen pool/lane internals, the RTK transform, and the test suite — were read end-to-end by the audit driver instead. No source file was modified, created, or deleted.
- **Baseline**: `gofmt -l .` clean, `go vet ./...` clean, 5 direct dependencies, no hardcoded secrets by regex sweep. `gosec`, `govulncheck` and `staticcheck` are not installed, so dependency and scanner coverage is absent.
- **Not run**: `go test ./...`. The audit was read-only by instruction; findings are static-only, and no runtime PoC was executed.

Severity: CRITICAL (exploitable / data loss / crash) · HIGH (real bug or security control failure) · MEDIUM (should fix) · LOW (nice to fix) · INFO (observation).

---

## CRITICAL

None confirmed. The most serious class of issue found (H1-H5) requires the antseed p2p backend to be enabled and a hostile peer or a compromised stats indexer; it is serious but not unauthenticated-and-unconditional.

---

## HIGH

### H1. Snapshot peer metadata is trusted with no signature or freshness check
- **location** `antseed.go:380-386`, struct at `antseed.go:45-52`
- **code** `var snap struct { Peers []antPeer \`json:"peers"\` }; if json.Unmarshal(body, &snap) != nil {`
- **issue**: `antPeer` has no `signature` and no `timestamp` field. Every field that steers behavior (`publicAddress`, `services`, `servicePricing`) is taken as-is from an unauthenticated JSON body from `https://network.antseed.com/stats` and decides where the user's full prompt is sent. `ANTSEED-DOCS.md:181-183` says PeerMetadata carries a signature and buyers must "Verify signature + freshness"; §3.3 and §9.2 mark the stats snapshot as the shortcut that skips verification. The *transport* handshake is verified (`antseedp2p.go:763` recovers the signer and requires it to equal the advertised `peerId`), so an attacker cannot impersonate a peer's id, but it can advertise any id it owns with any service and pricing.
- **scenario**: anything that can alter the stats response (indexer compromise, DNS/BGP/MITM, a hostile mirror via `antseed.stats_url`) returns `{"peers":[{"peerId":"<attacker>","publicAddress":"attacker.tld:6882","providers":[{"services":["gpt-5.6-sol"],"defaultPricing":{"inputUsdPerMillion":0}}]}]}`. `antseed/gpt-5.6-sol` then routes every request to the attacker, with no error and no distinguishing log line.
- **fix**: add `Signature` + `Timestamp` to `antPeer`, recover the signer with the existing `antRecoverAddr`/`antEthHash` helpers, require it to equal `peerId`, and reject stale timestamps. Failing that, treat the snapshot as untrusted hints and fetch + verify `GET /metadata` per docs §3.1.

### H2. Snapshot `publicAddress` is dialed with no private/loopback filter (SSRF)
- **location** `antseed.go:392`, vs the filter at `antseed.go:233`
- **code** (232-234) `ip := net.IP(data[:len(data)-2]); if !ip.IsGlobalUnicast() || ip.IsPrivate() { return "" }` — and (392) `hp := antHostPort(peer.Addr)`
- **issue**: the DHT path filters endpoints hard; the snapshot path applies no equivalent check, and `antOpen` (`antseedp2p.go:675`) dials whatever string it is given. Peer-supplied address plus H1's missing signature is a server-side request primitive out of the local proxy.
- **scenario**: a snapshot entry with `"publicAddress":"169.254.169.254:80"` or `"127.0.0.1:5432"` makes nimroute POST the user's prompt body to a cloud metadata or local service. On a cloud VM with IMDSv1 the response comes back as a "model reply". The repo's own test (`antseed_test.go:56`) uses `127.0.0.1:6882`, so the loopback path is already exercised.
- **fix**: after `antHostPort`, `net.SplitHostPort` and reject non-global-unicast or private/loopback/link-local IPs, reusing the predicate at `antseed.go:233`. Extract it so both paths share it.

### H3. `answerNeedAuth` EIP-712-signs token counters supplied by the peer
- **location** `antseedp2p.go:1069-1096`
- **code** (1074-1079) `fmt.Sscanf(m.In, "%d", &in); fmt.Sscanf(m.Out, "%d", &out); fs.in += in; fs.out += out; fs.reqs++` → (1096) `sig := antSignTyped(id, antFreeAuthType, enc)`
- **issue**: the peer tells the buyer how many tokens *the buyer* consumed, the buyer adds those numbers to its own counters, then signs them as an on-chain-settleable `FreeUsageAuth`. Nothing cross-checks `inputTokens`/`outputTokens`/`service` against the response actually served. `ANTSEED-DOCS.md:313-315` describes the counters as locally derived, not echoed. The `Sscanf` errors are discarded too, so a non-numeric value reads as 0 and an overflowing one saturates.
- **scenario**: an attacker sells a free model and, per request, sends a `NeedFreeUsageAuth` with `"inputTokens":"1000000000000"`. The buyer returns a valid signature attesting to 1e12 cumulative tokens plus a `metadataHash`/`metadata` blob it can settle or dispute. Counters up to 2^64 are accepted per message with no ceiling.
- **fix**: derive `in`/`out` from the local `usage` block of the response that was served (`respUsage(rb)`, as `antseed.go:645` already does) and sign only a locally computed delta.

### H4. `mustHex` panics on peer-supplied `ack.Pub`
- **location** `antseedp2p.go:769`, definition `antseedp2p.go:659-665`
- **code** `shared, err := curve25519.X25519(priv, mustHex(ack.Pub))` / `func mustHex(s string) []byte { b, err := hex.DecodeString(...); if err != nil { panic(err) }; return b }`
- **issue**: `ack.Pub` comes straight out of the peer's JSON `enc-ack`. Any non-hex or odd-length value panics. Reached only after the signature check at 762-768 passes, so the attacker must be the peer itself, which is the point of an untrusted marketplace. `antOpen` runs in the `net/http` handler goroutine, so `net/http`'s per-connection recover catches it: no process crash, but the connection is dropped with no response and a stack trace is dumped per attempt.
- **scenario**: seller sends `"pub":"zz"` with a correctly signed `sig`. Every connection attempt to that seller panics, `antConnFor` never caches the conn, so each request re-dials and re-panics. One hostile offer is permanent 5xx plus log flooding for that model.
- **fix**: decode explicitly and reject on error or `len(pub) != 32`, closing `raw` and returning an error.

### H5. Unbounded per-peer work in the auth path: goroutine per message, unbounded `bySvc`, two ECDSA signs
- **location** `antseedp2p.go:876`, `:1086`, `:1092-1097`, sort at `:248-252`
- **issue**: `svc` is a raw peer string used as a map key that is never evicted, so `bySvc` grows one entry per distinct value. Each accepted message spawns a goroutine, takes two secp256k1 signs (`antFreeMetadataHash` at 1092 and `antEncodeFreeMetadata` at 1097), and walks an insertion sort over the growing map, which is O(n) per call.
- **scenario**: one seller sends 50k `NeedFreeUsageAuth` frames with increasing `requiredSequence` and unique `service` strings: 50k goroutines, 50k map entries, 100k signatures, quadratic shuffling. The free-inference path pins a core.
- **fix**: handle `0x55` inline in `readLoop` (`fs.mu` serializes it anyway), cap `bySvc`, and compute the metadata once instead of encoding twice.

### H6. The NIM upstream call ignores the client context, so slots can be pinned for ~20 minutes
- **location** `main.go:3011`, compare the correct `main.go:443`
- **code** (3011) `req, err := http.NewRequest(r.Method, target, bytes.NewReader(body))` — vs (443) `req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))`
- **issue**: `callUpstream` (the Anthropic path) threads the context; the OpenAI-compat NIM loop in `ServeHTTP` does not. On client disconnect nothing cancels the in-flight `cl.Do`; only the 300s client timeout (`main.go:2991`) ends it, and the retry loop `for attempt := 0; attempt < max(len(p.keys)*2+2, 4)` (`main.go:2999`) can run that wait four times. The semaphore slot taken at `main.go:2818` is released only by the deferred `p.semRelease()` when the handler returns. `auth.tokens` ships commented out (`config.yml.example:11-17`) and the listener binds all interfaces (`main.go:4267`).
- **scenario**: 25 concurrent unauthenticated `POST /v1/chat/completions` with a valid body, each client sending `RST` (e.g. `curl --max-time 0.1` or SO_LINGER 0). Each goroutine holds its slot for up to 4x300s; every later request blocks in `semAcquire`. Repeatable sustained unavailability.
- **fix**: use `http.NewRequestWithContext(r.Context(), ...)`, and replace the `time.Sleep(200 * time.Millisecond)` at `main.go:3003` with the ctx-aware select `callUpstream` already uses at `main.go:431-435`.

### H7. `/v1/models` is unauthenticated and performs a live authenticated NVIDIA call per request
- **location** `main.go:2787-2789`, call at `main.go:2000-2020`
- **issue**: `/v1/models` returns before `requireAuth` at `main.go:2799`, so any caller reaching the port triggers `handleModels`, which issues a live HTTPS request to NVIDIA with a real API key plus a live zen `/models` fetch (`main.go:2060`) on every call. Neither response is cached, and the path has no rate limit and no `semAcquire`. The open endpoint is by design (AGENTS.md §4); the amplification is the bug.
- **scenario**: `while true; do curl -s localhost:5419/v1/models; done` → one authenticated NVIDIA request plus one zen fetch per iteration, burning the account's rate-limit quota. The proxy's key can get throttled or banned, taking down the paid NIM path for the legitimate user.
- **fix**: cache the NVIDIA list the way zen's `ocInfo` is cached (the zen list already runs on a 45m ticker but `handleModels` re-fetches inline), and move `handleModels` behind `semAcquire`.

### H8. `PickSticky` returns a `*Key` whose fields every caller reads outside the pool lock
- **location** `main.go:2999-3017`, release at `main.go:291`, writers at `main.go:269`, `:382`, `:402`; also `main.go:467`, `:2003`, `main.go:447`
- **issue**: `PickSticky` releases `p.mu` before returning the pointer. Callers then read `key.Key`, `key.Name` and `key.Consec429` unsynchronized. `Reload` writes `existing.Key` under `p.mu.Lock()`; `rateLimit`/`Clear429` write `CooldownUntil`/`Consec429` under the same lock. `handleModels` even calls `p.mu.RUnlock()` at `main.go:1997` before reading `key.Key` at `main.go:2003`.
- **scenario**: a config hot-reload that changes `nvidia_keys` during traffic writes `existing.Key` while `ServeHTTP` reads it to build the `Authorization` header at `main.go:3017`. A Go string header torn across those writes yields a garbage or panicking `Bearer` value. `-race` flags it on the first run that exercises it.
- **fix**: have `PickSticky` return the name and secret string (or a copy) captured under the lock, so no caller dereferences pool-owned memory outside `p.mu`.

### H9. A malformed `config.yml` is overwritten with the example, destroying every secret in it
- **location** `config.go:505-525`, write at `config.go:512`
- **code** (506-512) `if c, err := loadConfigFile(path); err == nil { ... }` then `if seed, err := os.ReadFile("config.yml.example"); err == nil { if err := os.WriteFile(path, seed, 0600); err != nil {`
- **issue**: `loadConfigFile` returns an error both when the file is *absent* and when it is *present but unparseable*. Both take the same branch, and `os.WriteFile` is `O_WRONLY|O_CREATE|O_TRUNC`. There is no `errors.Is(err, os.ErrNotExist)` distinction. The function's own doc comment at `config.go:504` claims "malformed user yaml keeps the old running config (caller)" — nothing implements that.
- **scenario**: a YAML slip in `config.yml` (a bare `nvidia:` with no children, the form the existing `nvidia_keys:`/`zen.proxies:` blocks already use; or `port: "5419"` → cannot unmarshal !!str into int). Restart. The file is truncated and replaced with `config.yml.example`; every `nvapi-…` key, every freepi jwt, every `http://user:pass@` proxy and `auth.tokens` is gone. The log says `created config.yml from example, edit it to configure`, which reads as a normal first run. No backup is made.
- **fix**: gate the seed on `if _, statErr := os.Stat(path); os.IsNotExist(statErr) { ... }` and let a parse failure fall through to the `WARN: %s unreadable, running on defaults` branch at `config.go:520`.

### H10. freepi failover never reaches a second account
- **location** `freepi.go:491-510`
- **code** (503-509) `if fa == nil { fa = c; break }` / `if faLast, fd := tried[fa.name]; fd && last.Before(faLast) { fa = c }`
- **issue**: both branches that assign `fa` also `break`, so line 507 is unreachable and `last` at 498 is dead. The loop reduces to "first free account in config order", contradicting the comment at `freepi.go:489` ("free-and-untried first, else the coldest tried one"). Verified by reading the branch structure: `fa` can never be non-nil at 507, because the only path that sets it without breaking is 500, which breaks.
- **scenario**: `accounts: [{a}, {b}]` where `a` is rate-limited and `b` is healthy. The request claims `a`, gets 429, releases it; the next iteration sees `a` free and first, picks `a` again. `b` is never used for the life of the request. Same for an account permanently 403.
- **fix**: keep `fa` nil while scanning, track the coldest-tried free candidate separately, and only `break` on the untried case.

### H11. The freepi retry loop has no working deadline check and no backoff
- **location** `freepi.go:542-547`, loop at `freepi.go:484`
- **code** `if !retry && time.Now().After(deadline) { break }` / `if !retry && len(tried) >= len(accts) && time.Now().After(deadline) { break }`
- **issue**: both exits are gated on `!retry`. `freepiTry` returns `retry=true` for every non-200 that is not a handled 409 (`freepi.go:600`) and after the two 409 attempts (`freepi.go:602`), so any persistent non-200 keeps `retry` true forever. The `deadline` is consulted only on the `fa == nil` branch (`freepi.go:521-524`), which never runs while an account is free. Line 545 is fully subsumed by 542. Each iteration spawns `go freepiAdCycle(...)` at `freepi.go:560`, which issues up to three more authenticated calls.
- **scenario**: the 90-day jwt expires (`FREEPI-DOCS.md:18`) → upstream returns 401 → `retry=true` → the loop spins at upstream round-trip speed with no sleep, writing one `!! freepi 401 ...` line per iteration to stdout, and never answers the client. Compounded by H10, it hammers one account only.
- **fix**: drop the `!retry` gate (`if time.Now().After(deadline) { break }`), add a bounded backoff on the retry path, and delete the redundant 545 branch.

### H12. Two log sites print raw `scheme://user:pass@host` proxy URLs
- **location** `zenproxy.go:488`, `zenlanes.go:971`
- **code** (zenproxy.go:488) `log.Printf("  zen proxies: dropped %s (left %d)", proxyURL, len(zenProxies))` — (zenlanes.go:971) `acclog.Printf("  opencode lane %s escalating direct -> %s (%s)", l.id, p, proxyCountry(p))`
- **issue**: every other proxy value that reaches a log or status field goes through `redactProxyUserinfo` (`main.go:1910`, `:1936`, `:1945`, `:3324`, `:3359`, `:3408`, `:3425`, `:3466`, `:3486`, `:3514`, plus `main.go:654`, `zenlanes.go:1245`, `zenproxy.go:735`). These two bypass it and write the full credentialed URL to the world-readable access log. Both are reachable from ordinary traffic: `dropProxy` (`main.go:3386`, `:3434`) on a transport error, `laneEscalate` (`main.go:3410`) on a direct-connect 429.
- **fix**: wrap both arguments in `redactProxyUserinfo`.

### H13. `nimstatus.py` re-parses a 20 MB JSONL once per second while tailing
- **location** `nimstatus.py:37`, `nimstatus.py:319`
- **issue**: `render()` calls `jsonl_stats()`, which opens `nim-usage.jsonl` and runs `json.loads` per line; the tail loop calls `render()` once per second. The file is ~19.9 MB / 75,996 lines and grows one line per request forever, so cost is O(file size) per second and grows without bound. `jsonl_stats()` also runs on every `status` invocation.
- **scenario**: `./nim status-tail` left open overnight on a busy proxy pegs a core and the render loop falls behind.
- **fix**: cache the aggregate keyed on `(path, mtime, size)`; reuse the cached value between tail polls.

### H14. `freebuff_login.py` can create a duplicate top-level key, which triggers H9
- **location** `scripts/freebuff_login.py:65`, `:76`
- **issue**: the script recognises only its own `# --- freebuff (managed by ...) ---` block and then appends a second top-level `freebuff:` key. `config.yml.example:281-291` also documents a plain hand-editable `freebuff:` section, so a user who uncomments that and then runs the helper ends up with two. `yaml.v3` decodes with `uniqueKeys: true`, so `loadConfigFile` fails and H9 fires.
- **impact**: total config loss — the same destruction as H9, reached through the documented path.
- **fix**: strip any existing top-level `freebuff:` block before writing, and write once with `open(cfg, "w")`. H9's own fix removes the data-loss consequence either way.

### H15. `usage.totals` is keyed by the client-supplied model id with no cap
- **location** `usage.go:66-70`, copied at `usage.go:105`
- **code** `t := totals[r.Model]; if t == nil { t = &usageTotals{}; totals[r.Model] = t }`
- **issue**: `r.Model` comes straight off the request body (`main.go:2891`). Every distinct string permanently allocates a `usageTotals` plus a map bucket, never evicted, and `usageSnapshot` copies the whole map under the lock on every `/status` hit. No cap, no TTL. `totalsByKey` is safe, its keys being bounded by config and the peer pool.
- **scenario**: with the shipped `tokens: []`, a loop of requests with unique `model` values grows resident heap linearly and lengthens the `/status` lock hold each time, ending in OOM or an unresponsive status endpoint.
- **fix**: cap the map and fold overflow into a single `"(other)"` bucket.

---

## MEDIUM

### Security

- **M1 — `guardrailPrefixes` is rebuilt without a lock while request goroutines read it.** `main.go:2479`/`2491` (writes) vs `main.go:2506`/`2516` (reads), driven by `applyConfig` on the `watchConfig` goroutine (`config.go:744`, `:762`). A Go slice header is three words written non-atomically, so a reader can see a new pointer with a stale length and index past the array. `claudeModels` in the same function *is* guarded by `claudeModelsMu` (`config.go:733`), so this is an inconsistency rather than house style. Trigger is any config edit under traffic. *Reported independently by three auditors.* **fix**: guard both globals with a `sync.RWMutex`, or build locally and publish via `atomic.Pointer[[]string]`.
- **M2 — Log-line forgery via percent-encoded newline in the request path.** `main.go:2779` logs `r.URL.Path`, the *decoded* path, before `requireAuth` at `:2799`. `net/url` rejects raw control bytes in the request target but not `%0a`, and `%2f`/`%0a` survive `cleanPath`, so the handler sees a literal newline. `GET /v1%0A2026-01-01%2000:00:00%20req-999%20--%3E%20POST%20/v1/messages...` forges a second, byte-identical access-log line with no credential. Same trick reaches `main.go:2903`, `:3012`, `:3189`, `:3650`, and the client-controlled `model` field injects the same way at `:2903` and `:3254`. **fix**: a `logSafe` helper replacing every byte < 0x20 and 0x7f with `%XX`, applied to `r.URL.Path`, `model`, `realModel`.
- **M3 — `redactProxyUserinfo` splits on the first `@`, `net/url` on the last.** `zenlanes.go:1229` uses `strings.Index(rest, "@")`. For `http://user:p@ss@host:1080` it returns `http://***@ss@host:1080`, leaking the password tail to both the log and `/status.zen_proxy`. Requires an operator proxy credential with an unescaped `@`, so likelihood is low, but this function is the only thing between that password and an open endpoint. **fix**: use `strings.LastIndex(rest, "@")`.
- **M4 — `ZEN_CAPTURE` writes client credentials and bodies to a world-readable file.** `anthropic.go:2084` marshals `r.Header` (raw inbound, including `Authorization`) and `os.WriteFile`s it at `0644` into `$ZEN_CAPTURE`. **fix**: redact `authorization`/`x-api-key`/`cookie` before writing; use `0600`.
- **M5 — `captureRequest` does an uncapped pre-auth body read.** `main.go:2763-2765` → `:4107` `b, err := io.ReadAll(r.Body)` runs at the top of `ServeHTTP`, before `requireAuth` and before PII masking, then writes headers plus the unmasked body to `/tmp/oc-capture/*.json` at `0644` (`main.go:4129`). This is the only bare `io.ReadAll` of an inbound body; the other twelve are `LimitReader`-wrapped. **fix**: `io.LimitReader(r.Body, bodyLimit)`, move after `requireAuth`, redact headers, mode `0600`.
- **M6 — `dbglog` records the `Authorization` header at `0644`.** `main.go:2962` writes `r.Header` in full; the log is opened `0644` at `main.go:757`. The body is masked (masking at `:2862` runs earlier) but the header is not. **fix**: open `0600`, log a header allowlist.
- **M7 — `ZEN_PASSTHROUGH` forwards client credentials to zen.** `anthropic.go:2034-2041` clones all client headers, keeping `authorization`, `x-api-key`, `cookie`, `x-session-id`. Opt-in debug, so LOW-ish, but the AGENTS.md §7 landmine applies and there is no deny-list behind the toggle. **fix**: run `zenHeaderForwarded`'s deny-list over the clone.
- **M8 — The shipped default is an open proxy on all interfaces.** `config.yml.example:19` (`tokens: []`) with `main.go:4267` (`addr := fmt.Sprintf(":%d", port)`), made persistent by `Restart=always` and launchd `KeepAlive`. Reachable from the whole LAN with no key, spending the configured `nvidia_keys` quota. **fix**: add `server.bind` defaulting to `127.0.0.1`; note in the example that `tokens: []` is only safe on loopback.
- **M9 — A missing or malformed `guardrails.json` fails open silently.** `config.go:745` discards both the read error and the unmarshal error, leaving `guardrailPrefixes` empty; the only startup log (`main.go:4292`) fires solely when `guardrails.enabled` is false. **fix**: log a WARN with path and parse error; expose `len(guardrailPrefixes)` on `/status`. (The corpus is fine: 456/456 entries carry the key the loader reads.)
- **M10 — A malformed `model_info.jsonc` silently empties the whole catalog.** `modelinfo.go:78-88` returns an empty map on every error path and `applyConfig` installs it unconditionally, so a truncated save makes every model advertise the 131072 fallback ctx and drops all descriptions. No log line. **fix**: distinguish absent from unparseable, log the parse error, and keep the previous catalog when the new one is empty.
- **M11 — freepi jwt is written world-readable and is not gitignored.** `scripts/freepi_login.py:67` writes `freepi_jwt.txt` at process umask (0644); `.gitignore:1-6` does not cover it. The same file correctly chmods `config.yml` at `:109-111`, so this is an inconsistency within one file. **fix**: open with mode `0600`; add the file to `.gitignore`.
- **M12 — `.gitignore` misses `config.test.yml`, which AGENTS.md mandates.** `.gitignore:4` matches `config.yml` exactly; the scratch copy AGENTS.md instructs the agent to create holds real keys. **fix**: also match `config.*.yml`.
- **M13 — CI actions are pinned to floating tags, including the one holding `contents: write`.** `.github/workflows/build.yml:70` (`softprops/action-gh-release@v2`). **fix**: pin all to full SHAs with version comments.
- **M14 — No top-level `permissions:` on the workflow.** `.github/workflows/build.yml:9`, `:59`; the `build` job, including `go test ./...` over fork PR code, inherits the repo default token scope. **fix**: `permissions: contents: read` at top level, `contents: write` only on `release`.
- **M15 — A real account email is committed in `FREEBUFF-TODO.md:6`** (`baccusminoa@gmail.com`, paired with its ban status). Not a credential, but it maps a real account to a server-side enforcement action, and the repo is mirrored to a public-ish GitLab/GitHub pair. **fix**: replace with `acc1`/`acc2` labels; if the repo is or becomes public, the addresses need history rewriting, not just an edit.
- **M16 — `nim` writes a world-writable PID file and kills whatever PID it finds.** `nim:29`, `:52`, `:67`; a fixed `/tmp` path with `kill "$(cat ...)"` and no identity check. **fix**: `${TMPDIR:-/tmp}/nimroute.$$.pid` under `umask 077`, verify with `ps -p` before signalling.
- **M17 — Presence checks test key existence, not value.** `config.go:429-483` uses `_, has :=`, so `enabled:` (null) reads as an explicit `false` and silently disables a default-true backend. Applies to `nvidia.enabled`, `zen.enabled`, `usage.enabled`, `status.show_usage`, `status.show_pool`. **fix**: `if v, has := n["enabled"]; !has || v == nil { ... }`.
- **M18 — usage jsonl is world-readable while config.yml is `0600`.** `usage.go:126` opens at `0644`; records carry upstream error text (first 200 chars, `main.go:3113-3119`), model, and path. **fix**: `0600`.

### Correctness

- **M19 — `model` is interpolated unescaped into three SSE preambles.** `anthropic.go:741`, `:1147`, `:1209`; every other site in that file uses `jsonStr`. `req.Model` is attacker-controlled, so `{"model":"x\",\"content\":[...]}` forges a `message_start` payload and `\n\n` appends extra SSE frames. Confined to the requesting client's own response. **fix**: `jsonStr(clientModel)` at all three.
- **M20 — Text-then-reasoning reuses block index 0 and drops all later text.** `anthropic.go:858-863` with `:761-766`, `:868`; `closeText` latches `textClosed` and the `!textClosed` guard silently drops post-reasoning text. An interleaved-reasoning model truncates the turn. **fix**: running index counter; open a fresh text block.
- **M21 — The live `/responses` stream never terminates when upstream lacks `response.completed`.** `anthropic.go:1188-1194`; `finish()` is only reachable from the `response.completed` case at `:1125-1127`. A truncated `carry` fails unmarshal at `:1090` and continues, leaving the client without `message_stop`. The sibling `streamResponsesMerged` gets it right at `:1217`. **fix**: call `s.finish()` after the final `feed`.
- **M22 — Out-of-range `requiredSequence` wedges the free-usage channel permanently.** `antseedp2p.go:1070-1073`; `fmt.Sscanf` returns `ErrRange` on overflow leaving `seq == MaxUint64`, error discarded, so `fs.seq = MaxUint64` at `:1109` and every later legitimate request is dropped silently. **fix**: check the `Sscanf` error and bound `seq`.
- **M23 — Failed or empty snapshot polls block the request path and never back off.** `antseed.go:343-359`; `antFetched` advances only when `offers != nil`, and `antPollWithLookup` returns nil for a valid snapshot with zero free offers (`antseed.go:387`), so "indexer down" and "nothing free" are indistinguishable from "never polled". **fix**: distinguish fetch success from fetch result; move the poll off the request path.
- **M24 — Streaming clients get a non-streaming body and no Content-Type on the antseed OpenAI path.** `antseed.go:715-719`; `antTry` forces `stream=false` at `:578` and the handler writes raw JSON while skipping `Content-Type` exactly when streaming was requested. **fix**: mirror `handleAntseedAnthropic`, or return a clear 400.
- **M25 — Every non-200/402/429 status cools a healthy peer for 60s.** `antseed.go:604-606`; `ANTSEED-DOCS.md:79` lists the retryable set. A 400 from an over-long context parks a working seller for a minute. **fix**: cool only on the documented retryable statuses.
- **M26 — freepi truncated bodies are returned as 200 success.** `freepi.go:576-579` discards the `io.ReadAll` error, so a mid-body reset takes the `StatusCode == 200` branch and the partial payload is written as success. The client sees a stream that stops and retries, paying twice. **fix**: treat a read error as retryable.
- **M27 — `freepiCatalog` has no single-flight and no negative cache.** `freepi.go:300-307`, reached from `main.go:2108`; `/v1/models` is routed at `main.go:2787` *before* the auth gate, so an anonymous caller fanning out turns one outage into N authenticated upstream calls with the account jwts. **fix**: reuse `warm()`'s in-flight guard and cache the failure.
- **M28 — Truncated upstream body returned as success on the freepi path** overlaps M26; the same defect also lets `Content-Length` from upstream contradict the `LimitReader`-truncated body (`freepi.go:629-631` copies every header verbatim), so a >128 MB response tears the client connection. **fix**: allowlist `Content-Type` and `x-ratelimit-*`; drop `Content-Length`.
- **M29 — Duplicate freepi account names silently collapse.** `freepi.go:161-169`; the map dedupes but `out` still appends the same pointer twice, so the second JWT overwrites the first and one lane appears twice in rotation. **fix**: skip already-seen names and warn at load.
- **M30 — `freebuff_client.chat()` leaves the retried run un-FINISHed.** `scripts/freebuff_client.py:289`, `:300`; the second START's run id is never FINISHed, and a failed second attempt raises with the *first* response. **fix**: keep `run_id2` and FINISH it in the same `finally`.
- **M31 — `freebuff_client` requests `stream: true` but never parses SSE.** `scripts/freebuff_client.py:230`, `:274`; the `stream` parameter is dead, a single `json.loads` fails on an SSE body, a bare `except` swallows it, and `chat()` returns `{}` with `ok=True`. **fix**: honour the parameter and add an SSE reader, or send `stream: false`.
- **M32 — `_req` catches only `HTTPError` and admission retries are unbounded.** `scripts/freebuff_client.py:143`, `:196`; `URLError`, timeouts and resets escape as raw exceptions, and `while True` has no attempt cap or deadline. **fix**: catch `URLError` as retryable; add a deadline.
- **M33 — `freepi_login.py` can append the jwt under the wrong YAML section.** `scripts/freepi_login.py:96`; the lazy `(?:.*\n)*?` runs to EOF when `accounts:` is followed by a section with no blank line. **fix**: locate the `freepi:` block by line index.
- **M34 — `systemd`/`WinSW` units pin `PORT=5419`, silently overriding config.** `deploy/nimroute.service:12`, `deploy/nimroute.xml:11`, overriding `main.go:4261`. `server.port: 5420` yields a process on 5419 that `nim status` still probes, so it reports healthy while clients are refused. macOS (no `PORT` set) already differs. **fix**: drop the env var from both templates.
- **M35 — `./nim status` always exits 1.** `nim:103`; the trailing `[ "$2" = "tail" ]` is the case branch's last command, so it clobbers `pretty_status`'s exit code in both directions. **fix**: use `if [ "${2:-}" = "tail" ]; then status_tail; fi`.
- **M36 — `Status()` holds `p.mu.RLock()` across every snapshot subsystem.** `main.go:597-598` with `:661-666`; blocks `PickSticky`, `Reload`, `rateLimit` for the duration, and `/status` polls ~1 Hz. **fix**: snapshot `p`'s fields under the lock, gather the rest outside it.
- **M37 — `usage` config is never re-applied after startup.** `usage.go:118-135`; `applyConfig` has no usage branch, so `usage.path` changes are ignored and a renamed inode leaves writes going to the old file forever. **fix**: reopen on change.
- **M38 — `model_info.jsonc` has no watcher of its own.** `modelinfo.go:91-96`; `watchConfig` (`config.go:781-813`) stats only the config path, so editing the catalog does nothing despite `config.yml.example:349-350` implying otherwise. **fix**: stat the catalog path on the same tick.

### Performance

- **M39 — Redundant whole-body scans and byte→string copies against a 128 MB budget.** `anthropic.go:1397`, `:1547` + `:1554` (the same pair scanned twice, once inside `isWaitOnly`), `:1351-1356` (`string(body)` twice more). `upstreamBodyLimit` is `128 << 20` (`main.go:38`); peak transient memory is roughly 3-5x the body. **fix**: compare `strings.TrimSpace(text2) == waitSentinel`; use `bytes.Split`/`bytes.Contains`/`bytes.ReplaceAll`.
- **M40 — `Status()`'s unauthenticated cost.** `main.go:2785`; `StatusFor` redaction is correct, but an unauthenticated caller forces every snapshot including a `go antRefresh()` per call. Amplification against local CPU only. **fix**: gate or rate-limit the unauthenticated path.
- **M41 — `semAcquire` spawns a goroutine per request even when uncontended.** `main.go:169-189`. **fix**: move the goroutine creation inside the wait branch.
- **M42 — `nudgePostResponses` re-parses the body every retry and skips backoff on a dry pool.** `main.go:1876-1895`; `reqModel(nb)` re-unmarshals per iteration, and the `proxy == "" && AlwaysProxy` branch `continue`s with no `zenBackoffCtx`. **fix**: hoist the parse, add the backoff.
- **M43 — `sseUsageTee` lexes every SSE line and rejects spec-legal `data:` without a space.** `usage.go:139-174`; full `json.Unmarshal` of every delta chunk to find a tail usage block, and a hard-coded 6-byte `"data: "` prefix. **fix**: cheap substring gate, accept both prefixes.
- **M44 — Seller connections are never reaped.** `antseed.go:519`; `antConns` is only ever deleted from `antDrop`. Over days the proxy hits the fd limit and `too many open files` degrades *all* backends in the process. **fix**: idle deadline plus a reap pass.
- **M45 — `antCool` grows without pruning.** `antseed.go:539`. **fix**: delete expired entries during the existing `antRoutes` iteration.

### Docs

- **M46 — `README.md:28` lists `NIM_AUTH` without saying it is client-side only.** *Correction to the auditor's claim:* `NIM_AUTH` is not fictional — `nimstatus.py:16-17` reads it and sends it as a bearer to `/status`. But it authenticates the *status client*, nothing server-side, so a user who sets it expecting server auth still has an open proxy. **fix**: say auth comes from `auth.tokens`; note `NIM_AUTH` only authenticates `nimstatus.py` against `/status`.
- **M47 — `README.md:4` says "two backends"; four are routed.** `main.go:2905` (`opencode/`), `:2918` (`freeepi/`), `:2929` (`antseed/`), `:2942` (`nvidia/`). **fix**: name all four and the two extra prefixes in Setup.
- **M48 — `AGENTS.md:23-25` routing map omits the freepi and antseed branches.** The endpoint list is correct; the prefix map is not. **fix**: `freeepi/` → freepi, `antseed/` → antseed, `opencode/` → zen, `nvidia/` stripped, else NIM.
- **M49 — `AGENTS.md:16-17` claims a header the proxy never sends.** `anthropic-version: 2023-06-01` appears nowhere outside that doc; the real set is `anthropic.go:1996-2006`. It is the most load-bearing claim in the doc (it is what keeps zen from fingerprinting the proxy). **fix**: delete the clause.
- **M50 — `AGENTS.md:125-126` overstates the zen blocklist as "all `x-opencode-*`".** `main.go:3662-3671` is an explicit switch; `x-opencode-session-id` is deliberately allowed and set at `anthropic.go:1999`. An agent following the doc could add it to the deny list and break session continuity. **fix**: name the four dropped headers and note the exception.
- **M51 — `ANTSEED-TODO.md:33-36` marks 11 unit tests done that do not exist.** `antseed_test.go` has exactly 3 test functions; none covers canon/base, textproto filtering, free pricing, trust gate, or failover. The tracker certifies coverage that is absent. **fix**: name the 3 that exist and open an item for the rest.
- **M52 — `ANTSEED-PLAN.md:3-4` says "nothing implemented yet"** while `antseed.go`/`antseedp2p.go` are wired at `main.go:2929-2938` and ANTSEED-TODO marks all 18 items done. **fix**: mark it shipped, point at ANTSEED-TODO.md.
- **M53 — `CHANGED-SINCE-682b2eb.md` gives rollback orders against stale hashes.** HEAD is `5f87378`, two rename commits past the `b4fa0b3` it names; the untracked-file triage is wrong; the whole antseed backend is absent from its commit list. Following it reverts the rename, not the streaming work. **fix**: delete it (it says "delete when triage is done", line 123) or rebase onto `5f87378`.
- **M54 — `TODO.md` has two items numbered 11,** and item 12 cites "item 11" at line 260. **fix**: renumber the second to 14 and update the reference.
- **M55 — `TODO.md:149-150` asserts a lane-cap default of 5; the real default is 25** (`config.go:366`, `config.yml.example:179`), and it names a key that does not exist (`pool_refresh_minutes` vs the real `refresh_interval_minutes`, `config.go:202`). **fix**: rewrite against current values.
- **M56 — `TODO.md:396-413`'s headline model conclusion is stale** (14 non-deepseek opencode ids now, not 13; written in present tense; predates freepi and antseed). **fix**: re-date and scope it to `opencode/*`.
- **M57 — model_info entry counts are wrong in two docs.** `TODO.md:156-158` and `CHANGED-SINCE-682b2eb.md:65-66` both say 102 entries; the file has 108, and the 5 `antseed/*` entries are uncounted in both. **fix**: update or just say 108.
- **M58 — The `./nim` subcommand list in README and AGENTS.md is incomplete.** Both say `./nim run|start|stop|restart|status|logs|tail|install|uninstall`; `nim:96-126` also implements `status-tail`, `log [N|tail]`, and `freepi auth` — the last being the only way to mint the freepi jwt, which `config.yml.example:295` points at. **fix**: use `nim:125`'s own usage string.
- **M59 — `RTK` has zero prose documentation anywhere.** Implemented across `rtk.go`/`rtkcompress.go`/`rtkdetect.go` and wired at `anthropic.go:1628`, with a config block at `config.yml.example:126-148`, but no `.md` mentions it. **fix**: document it.
- **M60 — freepi and antseed have no AGENTS.md coverage.** §1 names two backends; §3's presence-check list, §5's error table and §7's landmines cover only zen/NIM. Missing: the one-live-stream-per-jwt invariant and the `x-session-id` stability rule that `PLAN.md:74-78` calls out but that lands nowhere in the contract. **fix**: add both subsections.

### PII package (self-audited — vault, scan, detectors, NER, wiring)

- **M61 — `restoreText` replaces natural occurrences of surrogates, leaking originals into contexts the user never sent.** `pii/scan.go:172-187` iterates every vault surrogate and `strings.ReplaceAll`s it across the whole response. Realistic-mode fakes are common names (`fake.go:11-17` — "Marcus", "Julie", …). When the model independently writes "Marcus" in an unrelated answer, it is replaced with the vault original, e.g. the user's real name.
- **scenario**: user sends "my name is Xabier Etxebarria" → surrogate "Marcus". Model later writes "Marcus Aurelius said…" → client receives "Xabier Etxebarria Aurelius said…". The original PII is inserted where it never appeared, and the sentence is corrupted. Every common-name surrogate in the vault is a standing instance of this.
- **fix**: restore positionally (track masked ranges through the pipeline) or use collision-resistant surrogate shapes; at minimum require word boundaries and prefer the JSON-escaped form only inside string values.
- **M62 — `noScanZones` searches skip strings globally, so user text identical to a system string is never masked.** `pii/scan.go:262-288` collects system-prompt strings and image values, then zones *every occurrence* of each string in the raw body via `strings.Index` over the whole text. Zones should be positional (the system message's ranges); instead they are textual.
- **scenario**: with `includeSystem: false`, the system prompt contains "contact support@example.com" and the user writes "my email is support@example.com" (their actual address, or any shared string such as a company URL). Both occurrences are zoned, the user's PII is skipped by `detectZoned`, and it is sent upstream raw.
- **fix**: compute zones from the parsed JSON structure (mask only values under system-role nodes) instead of string-searching the serialized body.
- **M63 — secret detectors miss common shapes.** `pii/detect.go:133-138`, `:150-171`. `skRe` (`sk-[A-Za-z0-9]{16,}`) cannot match `sk-ant-…` keys: after `sk-`, "ant" is 3 alphanumerics and `-` breaks the run, so no 16-run follows. There is no `AKIA…` pattern (bare AWS keys match only via `genericKeyRe`, which requires a nearby keyword *and* a `[:=]`), and no `xox[bpas]-…` pattern (`xox` is a keyword but the generic regex needs the `key|secret|token…\s*[:=]` left side).
- **scenario**: `export AWS_SECRET…` / pasting `AKIAIOSFODNN7EXAMPLE` or `xoxb-1234-…` or an `sk-ant-…` key into a prompt with no `password =` style assignment nearby → sent upstream unmasked.
- **fix**: add `sk-ant-[A-Za-z0-9-]{16,}`, `AKIA[0-9A-Z]{16}`, and `xox[bpas]-[A-Za-z0-9-]{10,}` patterns alongside the existing `nvapi_`/`ghp_` direct matchers.
- **M64 — custom fuzzy pass is unbounded in lines × spellings on the request path.** `pii/custom.go:97-120` runs `fuzzFind` (itself O(windows × n²) via `similarity`) per line per spelling. Lines are capped at 4096 chars, but the *count* of lines and the *count* of entities/terms are both unbounded, and the detector only exists when the user configures terms — i.e. exactly when spellings grow.
- **scenario**: a few dozen configured entities plus a multi-MB body of short lines → seconds of CPU per request on the masking path, amplifiable by any caller.
- **fix**: cap total fuzzy work per request (lines × spellings budget, stop after N hits) or prefilter lines with a cheap substring test before the quadratic pass.

---

### Zen pool / lane internals (self-audited — full line-by-line pass)

- **M65 — `convertSharedLocked` mutates a shared lane object in place while it may be in flight.** `zenlanes.go:655-677` reassigns `victim.session`, `victim.proxy`, `victim.limitedSince`, `victim.lastUsed`, `victim.id` on the existing pointer and re-keys the map. The victim is the *idlest* shared lane, not necessarily an unclaimed one: `sharedLaneLocked` stamps `lastUsed` at hand-out (`zenlanes.go:747`), so an in-flight request can hold the same pointer. The first request copied `sessionID` at hand-out, but every later `laneProxyFor(lane)` / `laneRotate(lane)` / `lane.id` log on that pointer now acts on the new identity. Session continuity breaks mid-request and two conversations share one mutated session.
- **scenario**: burst of new `hdr:` keys with a full table converts the idlest shared lane while its owner is mid-stream; the owner's retry loop rotates an identity that no longer belongs to it.
- **fix**: allocate a fresh `zenLane` for the new key instead of renaming the victim in place; let the victim drain naturally.

- **M66 — credentialed SOCKS5 proxies can never authenticate.** `zenproxy.go:843-872` builds the SOCKS5 dialer from `pu.Host` (userinfo stripped) and `socks5Dial` (`zenproxy.go:918-922`) always sends `0x05, 0x01, 0x00` (no-auth). `pu.User` is only consulted for the HTTP CONNECT path (`zenproxy.go:430-434`). Pool candidates are built without userinfo anyway (`zenproxy.go:208`), so this bites custom proxies: a `socks5://user:pass@host:port` entry verifies (`verifyProxy` → `zenClient` → same transport), fails auth, and is dropped as a dead exit with no credential-specific log.
- **fix**: implement RFC 1929 username/password auth in `socks5Dial` when `pu.User` is set, or reject credentialed `socks5://` URLs loudly at config load.

- **M67 — `zenTransports` entries are never closed for exits removed by refresh.** `closeZenTransport` runs only from `dropProxy` (`zenproxy.go:495`). A pool refresh replaces `zenProxies` wholesale (`zenproxy.go:241-245`) and `lanesRevalidate` only clears lane pins; transports for vanished exits stay in the map with their idle connections. The pool churns on its refresh tick, so the map grows by dead transports indefinitely.
- **fix**: after swapping the pool, close transports for URLs not in the verified set.

- **M68 — `exitTrack` and `zenLive` grow without pruning.** `exitHealthFor` (`zenlanes.go:137-146`) and `noteExitRTT` (`zenproxy.go:334-349`) only ever insert; nothing deletes on pool churn. `burned`/`longBurned` are swept (`sweepBurns`), latency/reliability/country maps are replaced per refresh, but these two keyed-by-proxy-URL maps accumulate every exit ever seen.
- **fix**: prune both against the verified set on refresh, the same way `lanesRevalidate` prunes lane pins.

- **M69 — a brand-new exit is long-burned 12h after two fast 429s with zero successes.** `noteExit429` (`zenlanes.go:150-167`) requires `streak >= 2 && time.Since(lastOK) > 20s`; for a never-successful exit `lastOK` is the zero time, so the age check is trivially true. Two immediate 429s — the normal first contact with a saturated exit — bans it for every model for 12h.
- **fix**: require a non-zero `lastOK` (or a first-success grace window) before the 12h ban; keep the per-model `burnExit` path for the never-succeeded case.

- **M70 — the proxy-list fetch decodes with no size cap.** `refreshZenProxies` (`zenproxy.go:166-178`) runs `json.NewDecoder(resp.Body).Decode(&list)` directly on a `limit=0` endpoint (`proxyListURL`, `zenproxy.go:22`). Every upstream `io.ReadAll` in the codebase is `LimitReader`-capped; this one is not, so a compromised or malfunctioning list server can feed unbounded allocation into the hourly refresh.
- **fix**: wrap in `io.LimitReader` (e.g. 8 MiB, orders above any plausible list) and reject over-limit bodies.

### RTK transform (self-audited — full read of `rtk.go`/`rtkcompress.go`/`rtkdetect.go`)

- **M71 — `setTextParts` panics on a `text`-type block whose `text` is not a string.** `rtkcompress.go:118-122` guards the block shape (`pm, ok := p.(map[string]any); if !ok || pm["type"] != "text" { return false }`) but line 122 does `pm["text"].(string)` unchecked. A block like `{"type":"text"}` (missing text) or `{"type":"text","text":123}` panics with an interface conversion. Reachable at `anthropic.go:1639` via `rtkCompressBody` on the request body, which the client controls — the loop at `rtkcompress.go:40-67` calls `rtkToolResultText` first (safe, comma-ok at `:99`) but then `setTextParts` at `:62` on the same unchecked content.
- **scenario**: with `rtk.enabled: true`, `POST /v1/messages` with `{"messages":[{"role":"user","content":[{"type":"tool_result","content":[{"type":"text"}]}]}]}` (500+ bytes of surrounding text to pass `rtkMinCompressSize`) → handler goroutine panics → connection dropped. `net/http` recovers per-connection, so no process crash, but it is a remotely triggerable panic on malformed input. Off by default (`config.yml.example:132`), which is why this is MEDIUM not HIGH.
- **fix**: `s, ok := pm["text"].(string); if !ok { return false }` at line 122, and the same comma-ok guard at lines 129/134/136 where `arr[i].(map[string]any)` is assumed.
- **M72 — the fetch-model script downloads an executable model bundle with no integrity check.** `pii/fetch_model.sh:9-16` uses `curl -sL` with no `--proto`, no pinned CA, and no checksum, then the bytes load as an ONNX graph inside the proxy process (`ner_ort.go`). A MITM (or a compromised `nationaldesignstudio/rampart` revision — the `$REV` pin is a git hash, but nothing verifies the download matches it) swaps the model.
- **fix**: `curl --proto '=https' --tlsv1.2`, plus a `sha256sum -c` against a checked-in digest.

### Test suite (self-audited — all 16 test files read)

- **M73 — `TestExtraFreeModelsGatedOnLiveAPI` tests a copy of the logic, not the code.** `main_test.go:300-318` re-implements the extra-model filter inline (`if seen["opencode/"+id] { continue }; if len(live) > 0 && !live[id] { continue }`) over a literal `live := map[string]bool{"big-pickle": true}` and asserts the result is `[big-pickle]`. No production function is called.
- **scenario**: the real filter in `main.go` regresses (a retired model starts being listed, or a live one stops) → this test still passes, because it exercises its own inline copy. It gives false confidence on exactly the gating it appears to cover.
- **fix**: call the production helper with an injected model list; keep the literal as fixture data, not as the algorithm.

- **M74 — `fuzz_test.go` contains no fuzz target.** Both functions are ordinary table-driven `Test*` (`fuzz_test.go:11`, `:58`); the repo has zero `func FuzzX(f *testing.F)` and zero `f.Add` seeds. `TestRtkHostileBodies` recovers from panics, which is the right idea, but a fixed 20-case slice is not fuzzing — the file name promises coverage that does not exist.
- **fix**: either convert to `func FuzzRtkCompressBody(f *testing.F)` seeded with the existing hostile bodies (Go runs seed corpus in normal `go test` too), or rename the file to what it is.

- **M75 — the RTK panic guard misses the exact case it exists to catch.** `fuzz_test.go:16-39` feeds 20 hostile bodies through `rtkCompressBody` asserting no panic. Every array-shaped `content` case is `[{}]`, `[1,2,3]`, `[null,…]`, or an image block — all of which fail the `pm["type"] != "text"` guard at `rtkcompress.go:119`. None contains a `{"type":"text"}` block with a non-string `text`, which is the shape M71 panics on. The test is one case short of the bug it was written to prevent.
- **fix**: add `{"messages":[{"role":"user","content":[{"type":"tool_result","content":[{"type":"text","text":"long enough to compress…"},{"type":"text"}]}]}]}` (needs ≥500 bytes of text to clear `rtkMinCompressSize`) plus the `{"type":"text","text":123}` variant.

- **MEDIUM/LOW — `loadOrCreateConfig` has no test at all**, which is why H9 (malformed config silently overwritten with the example, destroying every API key) survived. `grep loadOrCreateConfig *_test.go` returns nothing. This is the highest-value missing test in the repo: the function's own doc comment claims a guarantee nothing implements, and the failure is total config loss.
- **fix**: table test over absent / valid / malformed / unreadable configs, asserting the file bytes are unchanged for the malformed cases.

- **LOW — the redaction tests never exercise a password containing `@`.** `TestRedactProxyUserinfo` (`main_test.go:2398-2413`) and `TestProxyCredsRedacted` (`todo_small_test.go:271-285`) cover `user:pass@`, `user@`, no-creds, and socks5. None covers `http://user:p@ss@host:8080`, the shape M3 shows leaks the password tail to `/status`.
- **fix**: add that case to the existing table.

- **LOW — `TestLaneGateSerializesConcurrentSenders` has a 50 ms timing tolerance on a 300 ms contract.** `audit_fixes_test.go:42-44` fails when the observed gap is under 250 ms. Under CI load, sleep wakeup jitter plus scheduling can push a correctly-serialized pair past that and produce a red build for a correct implementation.
- **fix**: assert the *ordering* property (the second claim is scheduled strictly after the first, which `laneGate` guarantees under the lock) rather than wall-clock spacing, or widen the tolerance and keep it as a secondary signal.

**Cleared**: the suite is hermetic — `live_test.go` uses `httptest` servers throughout and `TestZenProxyE2EStreams` rebinds `OpencodeBase` with a `t.Cleanup` restore; the only real-network test (`TestZenValidatorBoundaryLive`, `zen_schema_test.go:306`) is correctly gated behind `ZEN_CALIBRATE` with a skip message explaining it burns free-tier quota; `main_test.go:3571` builds a request against an `https://opencode.ai` URL but never sends it. Global state is handled well: `useConfig` (`main_test.go:46-51`) snapshots and restores the config pointer via `t.Cleanup`, and `resetLanes`/`resetBurns`/`resetConvoSeen` exist for the mutable pools. No `t.Parallel` anywhere, so no order-dependent races. `pii/ner_ort_test.go` is `//go:build ner` plus an env guard, so it never runs by default.

- **L38 — two lanes can draw the same exit concurrently.** `pickLaneProxyFor` (`zenlanes.go:882-928`) snapshots `taken` under `lanesMu`, releases it, then picks; two concurrent `laneReproxy` calls interleave between the snapshot and the pin, so both take the same exit. The design tolerates sharing (no correctness break), but the "lanes spread across distinct exits" invariant silently degrades under burst.
- **fix**: re-check `taken` under the write lock in `laneReproxy`/`laneEscalate`, or accept sharing explicitly in the comment.

- **L39 — `probeProxyFull` and `sampleProxies` are dead weight with live callers absent.** `probeProxyFull` (`zenproxy.go:382-414`) is documented unused in its own comment; `sampleProxies` (`zenproxy.go:449-474`) is "kept for tests" while the lane picker uses `sampleWeighted`. Both use `math/rand` sampling, which is permitted, but they widen the audit surface for zero production benefit.
- **fix**: delete both (tests can target `sampleWeighted` directly).

- **L40 — `noteConvoLocked` eviction deletes a random entry.** `zenlanes.go:89-103` caps `convoSeen` at `4*laneCap()+64` by deleting a single map-iteration-random key, which can be the entry just recorded or a hot conversation's first sighting (resetting its pin-from-second-sighting counter). Bounded and self-healing, but the victim selection is arbitrary.
- **fix**: track insertion order and evict oldest, or accept the reset explicitly in the comment.

**Cleared in this pass**: `lanesMu`/`lanes` discipline is consistent (every `lanes` access and every `l.proxy`/`l.session` mutation goes through `lanesMu`; `laneSession` is the locked accessor); `burned`/`longBurned`/`zenPool`/`zenLatency`/`zenReli`/`zenCountry`/`zenVersion` locking all check out; `hedgedDo` is ctx-aware on both sides with the loser body-closed in a bounded goroutine; `laneGate` claims under lock and aborts on `ctx.Done`; `sweepBurns` + `resetStaleLanes` reset the stale-pool clock so `maybeRefreshStalePool` cannot storm; `poolSnapshot` redacts every exit; error classification matches AGENTS.md §5 in every branch traced.

## LOW

### Security / hygiene
- **L1** `main.go:4458` writes the last 8 chars of a real NVIDIA key into the usage JSONL as `KeyName`. Suffix-as-identifier is by design (`main.go:615-618`); dev tool only. **fix**: none needed, noted.
- **L2** `.gitignore:43` ignores `zen_probe_spark.tsv` and `zen_probe_countries.tsv` but not `zen_probe.tsv`, the default output of `probe_zen_proxies.py:134`; `.freebuff/` is excluded only via `.git/info/exclude`, which is per-clone. **fix**: `zen_probe*.tsv`; move `.freebuff/` into `.gitignore`.
- **L3** `scripts/freepi_login.py:33` and `scripts/freebuff_login.py:33` index `sys.argv[i+2]` with no bound check; the sibling scripts guard. **fix**: copy the guard.
- **L4** `scripts/freepi_probe.py:44`'s `resp.close() if hasattr(resp, "close") else None` is a no-op (`resp` is a dict there) and the error string interpolates the whole response dict. **fix**: delete the line.
- **L5** `nim` deletions are only ever `$PLIST`, `$SYSTEMD` and the PID file. No action; recorded as cleared.

### Correctness
- **L6** `freepi.go:542-547` line 545 is dead once 542 is fixed (subsumed). **fix**: delete with the M11 change.
- **L7** `antseed.go:457-477`: `free` in the ranker is provably dead — branches 1 and 3 compute the identical swap, and every offer already passed `antFree` at `:404`. The header comment claiming "cheapest first" is wrong. **fix**: delete `free`, fix the comment.
- **L8** `antseed.go:47` `Display` is parsed and never read. **fix**: drop the field.
- **L9** `antseed.go:92-103` `antCanon` strips the vendor path and all punctuation for every family, so `together/llama-3-70b` and `groq/llama3-70b` canonicalize alike and `antTry` then sets a serviceId the client never asked for (`antseed.go:576`). `ANTSEED-DOCS.md:359` restricts compaction to an explicit family allow-list. **fix**: gate compaction on that list.
- **L10** `antseed.go:379-385` drops the `ReadAll` error, replaces the unmarshal error with a fixed string, and never checks `resp.StatusCode`, so a 500 HTML page is parsed as a snapshot. **fix**: check status, keep the error.
- **L11** `antseed.go:685`, `:693`, `:708`, `:711` send JSON bodies through `http.Error`, which forces `text/plain`. **fix**: set `Content-Type` and `w.Write`.
- **L12** `antseedp2p.go:751-768` never range-checks `ack.Ts` though `ANTSEED-DOCS.md:218` requires ±30s. Not exploitable: the signature covers a fresh per-dial nonce. **fix**: add the skew check.
- **L13** `antseedp2p.go:970-971` encodes the full body into a `bytes.Buffer` that the chunked branch never uses. **fix**: move the encode inside the `else`.
- **L14** `antseedp2p.go:975-980` mutates the caller's header map. Latent only — both callers build fresh maps. **fix**: `maps.Clone`.
- **L15** `antseedp2p.go:320-337` truncates length prefixes at `uint16` without error. No live caller reaches it. **fix**: error at the encoder boundary.
- **L16** `antseedp2p.go:440`, `:587-591` handles `socks5h://` identically to `socks5://`, always sending ATYP=3. Wrong for `socks5://` under split-horizon DNS. **fix**: resolve locally and emit ATYP=1/4 for `socks5://`.
- **L17** `antseedp2p.go:292-306` `antReadFrame` is dead, and its 64 MiB cap disagrees with `readRecord`'s 16 MiB. **fix**: delete.
- **L18** `main.go:2838`: `LimitReader` *truncates* rather than erroring, and no 413 exists anywhere in the module, so an 11 MB body is forwarded as corrupt JSON. **fix**: `http.MaxBytesReader` and map `*http.MaxBytesError` to 413.
- **L19** `main.go:342`, `main.go:84`, `usage.go:69`: `lastKey`, `ModelLock.data` and `usage.totals` all grow unbounded on client-supplied model ids with no eviction. (The `totals` case is filed as H15.) **fix**: size-bound the first two.
- **L20** `freepi.go:624` builds the client-facing error with `fmt.Sprintf` and is invalid JSON when the upstream message contains quotes; the Anthropic path uses `writeAnthropicError` correctly. **fix**: `json.NewEncoder`.
- **L21** `freepi.go:298-306`: nothing ever clears `fa.catalog`, so the comment "names (and their multipliers) refresh with the stats tick" is false and a newly added model never appears. `TestFreepiSnapshotServesCache` only asserts the cache is served. **fix**: expire it or drop the stale half of the comment.
- **L22** `nimstatus.py:329`, `:333` reads the whole log to slice the last N lines (`nim`'s own fallback correctly uses `tail -10`), and `int(sys.argv[2])` is unguarded so `nimstatus.py logs abc` tracebacks. **fix**: bounded seek, `try` around the int.
- **L23** `probe_zen_proxies.py:33` read-modify-writes module-level `_seq` from `ThreadPoolExecutor` workers without a lock, so two threads in the same millisecond can emit the same `x-opencode-session` — the collision the ids exist to prevent; `:201` divides by `len(models)` which `--models ""` makes zero. **fix**: `itertools.count()`; skip when empty.
- **L24** `scripts/antseed_test.py:115` guards an empty catalog but not an empty `text` list, so a catalog of only non-text entries raises `IndexError`; `:122` splices a network-supplied model id into a URL path unquoted. **fix**: guard `if not text:`; `quote(one, safe="")`.
- **L25** `.github/workflows/build.yml:33` pins Go 1.22 against `go.mod`'s `1.26.0` (harmless under `GOTOOLCHAIN=auto`, but fiction), and `go test ./...` is the only check — neither `gofmt -l .` nor `go vet ./...` runs in CI, and no job uses `-race`. **fix**: `go-version-file: go.mod`; add a vet/fmt job.

### Dead code
- **L26** `main.go:3819-3821` `sanitizeZenSchemaDepth` has zero callers; dead since the `sanitizeZenSchemaSegs` refactor. **fix**: delete.
- **L26b** `pii/ner_ort.go:153-155` computes `foldedHyph` (hyphen→space fold) and immediately discards it with `_ = foldedHyph`, so the NER window runs on unfolded hyphens despite the comment. The `//go:build ner` file also does not compile against its pinned dep — `ort.NewTensorValue` is called with three args (`:189`, `:194`, `:199`) but the signature takes two, and `e.sess.Run(ctx, …)` (`:204`) takes one. **fix**: wire the folded form through, fix the call sites for the current purego API, and get a `-tags ner` build into CI. Until then the NER backend is untested dead weight.
- **L26c** `pii/detect.go:71` `_ = runs` and `detectDigitEntities` requires the *entire* digit run to equal a rule length, so a 9-digit SSN embedded in a longer run (phone + extension, card with spaces) is missed. **fix**: slide windows of the rule lengths over long runs; drop the `_ = runs` line.
- **L27** `modelinfo.go:188-199` `modelSupportsParam` and `modelinfo.go:222-227` `modelIsReasoning` have zero callers. `modelSupportsParam` is the only reader of `modelInfoEntry.SupportedParams`, so the whole `supported_parameters` field in `model_info.jsonc` is carried for nothing. `Reasoning` itself is live. **fix**: delete both; drop `SupportedParams` unless a consumer is coming. Per AGENTS.md this is flagged, not removed.
- **L28** `piiwiring.go:56-63` `piiConfigWithEnabled` is a bare pass-through to `piiConfig()` whose name promises gating it does not do; `pii.DiscloseLine` already gates at `pii/pii.go:231`. It also adds one indirection per request on a path that runs on every chat request. **fix**: inline it and delete.

### Sloppy / polish
- **L29** `config.go:173`'s comment says `max_retries` `0 = single attempt`, but `config.yml.example:230` and `zenlanes.go:432-439` both say 0 is the default. **fix**: correct the Go comment.
- **L30** `config.go:704-710` `sortStrings` is an O(n²) insertion sort where `sort.Strings` is one line (and `main.go` already imports `sort`); `config.go:452-453` asserts `doc["status"]` twice in a row, discarding the outer result. **fix**: import `sort`; collapse the assertions.
- **L31** `config.go:664-679` `mergeMissingKeys` idempotency rests entirely on the single `# added:` line. Deleting that line re-appends the whole block; a short write at `:696` truncates it. The safety property that *does* hold: `seedValue`'s `indent` guarantees every emitted line is a comment, so it can never duplicate a key or corrupt YAML. **fix**: write the header last, or derive `recorded` from the stub text's presence.
- **L32** `nimstatus.py:66` `"%s" % f"{n:,}"` is a no-op round trip through two formatters; `:138` renders every lane green `● live` and never reads a per-lane state field, so the column is decorative. **fix**: simplify; source real state or drop the column.
- **L33** `main.go:615-618` inlines the same 8-char suffix rule that `short()` at `:767-772` already implements. **fix**: call `short(k.Key)`.
- **L34** `PLAN.md` §0 and §1.1/1.2/1.4 are all shipped but still written as open work, including the `svc_stop` fix at `nim:57-64` — a re-implementation would be a regression. **fix**: mark done inline.
- **L35** `TODO.md:208-209` line anchors are 62 and 24 lines stale post-rename; the same drift affects the `main.go:3388` and `zenlanes.go:330` references. **fix**: anchor by symbol.
- **L36** `config.go:316-408` `defaultConfig()` omits `models.claude_map` (4 real entries, example `:362-370`) and `anonymize.ner.model_path`, so the unreadable-config fallback loses them; AGENTS.md §3 requires the two to agree. **fix**: move them into `defaultConfig()`.
- **L37** `config.go:255-263` `StatusFor` has no `status.show_freepi` flag, so the freepi section (account names, tiers, credit spend) is the one `/status` section a user cannot turn off short of requiring a token. AGENTS.md §4 requires a flag per section. **fix**: add `show_freepi` defaulting true.

---

## INFO

- **I1** `anthropic.go:1496-1498` the nudge streak map is wiped wholesale at 8192 entries, resetting the cap for all concurrent sessions. Memory stays bounded; only the heuristic is affected. **fix**: evict one entry.
- **I2** `antseed.go:149-153` `antHostPort` returns an unbracketed IPv6 literal with no port. Only reachable from a hand-written snapshot `publicAddress`; DHT addresses are always bracketed at `:240`. **fix**: detect with `net.ParseIP` and `net.JoinHostPort`.
- **I3** `antseed.go:282` a 64 KiB read buffer is declared inside the DHT hop loop, so it heap-allocates up to 32 times per lookup. **fix**: hoist.
- **I4** `freepi.go:608-636` `handleFreepi` buffers the entire SSE stream while the Anthropic path converts; a streaming client sees time-to-first-token equal to total generation time and the `isStream` flag at `:632` is then a lie. Correctness is fine. **fix**: convert like the other path, or stream the body through.
- **I5** `freepi.go:271-274` when the error envelope has no `message`, the raw upstream body (first 200 chars) is logged at `:596` and returned to the client at `:428`/`:624`. The jwt is a header today, so it is not in the body, but any gateway reflecting `Authorization` into an error string would leak a 90-day credential to stdout and to every client. **fix**: fixed string, detail in the log only.
- **I6** `main.go:2767-2769` sets `Access-Control-Allow-Origin: *` on the unauthenticated status endpoint, so any page can read `/status` cross-origin. `StatusFor` strips every secret when `auth.tokens` is set, leaving version, uptime, counters and the free model list — enough for fingerprinting and localhost port scanning. **fix**: bind loopback by default; scope CORS to configured origins.

### Cleared — recorded so the report is honest about what holds

- **Auth ordering.** Only exact-match `/status`, `/health`, `/v1/models` are open; every other `/v1*` path passes `requireAuth` (`main.go:2799`) before dispatch. Trailing-slash, wrong-method, wrong-case and `%2f`-encoded variants all re-enter the same prefix check on the same `r.URL.Path` the router uses.
- **Token comparison is constant-time** (`subtle.ConstantTimeCompare`, `config.go:841`), with no `==` on a secret anywhere in the auth path. Two auditors noted the early `return true` leaks *which* token matched, not its value; with a handful of tokens that is not exploitable. Filed as a LOW under `config.go:840-844` alongside the case-sensitive `Bearer ` strip, which makes a valid lowercase `bearer <token>` 401.
- **Status redaction cannot be widened.** `config.go:906-915` unconditionally nils keys, locks, zen session/proxy/ago, lanes, lanecap, freepi, antseed, usage and pool *after* the `show_*` flags; the flags can only subtract. Only `Opencode{enabled,base,models}` survives, and `base` is the public `https://opencode.ai/zen/v1`.
- **`bodyLimit` cannot be bypassed.** `io.LimitReader(r.Body, bodyLimit)` bounds chunked, lying-Content-Length and gzip bodies alike; the gateway never decompresses in-process. The only gap is truncation instead of 413 (L18).
- **Every upstream `io.ReadAll` is `LimitReader`-capped** — eleven sites in main.go (483, 1925, 1948, 3057, 3111, 3120, 3421, 3520, 3595, 3616, 4422). The one bare `ReadAll` is the inbound body under `ZEN_CAPTURE` (M5).
- **No CRLF header injection.** `net/http` sanitizes header values at parse time and `http.NewRequest` rejects invalid ones, so the `fwd` copy cannot split a header.
- **No SSRF in main.go/anthropic.go.** Every outbound host is a compile-time constant (`nvidiaBase`, `OpencodeBase`, `freePiBase`, `nvidiaBase` for `handleModels`); only path suffix and raw query are client-influenced, which cannot re-anchor the authority. No redirect is ever emitted. The one SSRF in the codebase is the antseed snapshot path (H2).
- **No attacker-reachable panic or process crash.** There is no `recover()` in any non-test file, but `net/http` recovers handler panics per connection and every slice index in the JSON walkers is length-guarded. `mustHex` (H4) is the one peer-reachable panic; it is recovered per-connection.
- **`math/rand` is used only for sampling**, which AGENTS.md §7 permits: `main.go:325` (`rand.Float64` in `PickSticky`), `main.go:4404` and `zenproxy.go:365`, `:464-466` (weighted picks and key/pool rotation). Every secret, session id, message id, key, salt and nonce goes through `crypto/rand` — `randHex` (`anthropic.go:1924`), `zenID`, DHT node ids, X25519 scalars.
- **The antseed transport handshake is verified correctly.** `antseedp2p.go:762-768` recovers the `enc-ack` signer and requires it to equal the dialed `peerId`; failure closes the conn and returns an error rather than logging and continuing. The signed string carries the domain prefix `antseed-msg-v1:` and the initiator's fresh nonce. Domain separation is correct across EIP-191 and EIP-712 (`antseedp2p.go:131-134`, `:188-210`), with distinct types per message.
- **X25519 low-order points are handled** — `golang.org/x/crypto/curve25519.X25519` returns an error on all-zero output, so a malicious `ack.Pub` is rejected before HKDF. Our scalar is clamped internally.
- **Signature malleability is not reachable.** Verification is address equality against the expected `peerId`, not signature-equality caching, and public keys are only ever recovered via `ecdsa.RecoverCompact`, never parsed from raw bytes.
- **No private key reaches a log, status field, error, or request body.** `antseedp2p.go` contains zero logging calls; `id.priv` flows only into `antPrivKey`/`PrivKeyFromBytes` and the X25519 scalar.
- **p2p frame and record lengths are bounded before allocation.** `readRecord` rejects `n < 16 || n > antMaxPlain+16`; `antDecodeFrame` bounds-checks before slicing; `c.pend` is keyed by the buyer's own request ids, so peer-supplied ids that miss the map are dropped.
- **freepi credential handling is correct.** The jwt is only ever a request header, never in a URL, never logged, never in `/status` — `TestFreepiSnapshotNoLeak` asserts this. Slot claiming is race-free (the unlocked probe at `:493` is re-checked under `fa.mu` at `:512-519` with a bail-out). Request context and timeouts are honored on every call site.
- **Credential denylists match AGENTS.md §7.** NIM (`main.go:2980-2982`) drops exactly `authorization`, `host`, `origin`, `cookie`, `x-api-key`, `x-session-id`; zen (`main.go:3662-3671`) adds `content-type`, `accept`, `user-agent` and the four `x-opencode-*` it sets itself.
- **Python scripts: no injection surface.** No `eval`/`exec`/`os.system`, no `shell=True` with interpolated input, no TLS verification disabled, and `nimstatus.py`'s per-line `try/except ValueError` plus `except OSError` degrades a corrupt or missing jsonl to zeroed counters rather than a crash.
- **`mergeMissingKeys` cannot corrupt the user file.** It opens `O_APPEND|O_WRONLY` (no `O_TRUNC`), never rewrites, and `seedValue` guarantees every emitted line is a comment. It cannot duplicate a key, shadow a user value, or make the YAML invalid.
- **usage.go has no data race and no JSON injection.** `usageMu` guards `Encode` and the fd globals; `totalsMu` guards every map mutation and `usageSnapshot` builds its copy under the lock. `json.Encoder.Encode` escapes control characters, and single-`Write`-per-record on an `O_APPEND` fd is atomic.
- **`usageOut.Encode`'s error is discarded** (`usage.go:62`), so a full disk or renamed inode kills recording silently while `/status` keeps reporting in-memory totals. **fix**: log once via `sync.Once`.
- **`guardrails.json` and `model_info.jsonc` are schema-consistent with their loaders.** 456/456 guardrail entries carry the key `config.go:746` reads; the catalog parses cleanly through `stripJSONComments` and `"reasoning":null` is handled by the `*bool`. Two dead fields: `freepi_source` on all 7 `freeepi/*` entries has no struct field, and `reasoning_budget` (`modelinfo.go:28`) is used by zero entries.
- **`nimstatus.py` auth cannot leak.** `NIM_AUTH` goes to loopback only, and `nim`'s unauthenticated `curl` fallback is already covered by server-side redaction.

---

## Config drift: `config.yml.example` vs `appConfig`

**Keys in `config.go` with no leaf in `config.yml.example`** — undocumented, and `mergeMissingKeys` can never surface them because it diffs against the example:

| key | struct | `defaultConfig()` | effective default | effect |
|---|---|---|---|---|
| `zen.convo_idle_minutes` | config.go:159 | `1` | 1 min | sweep for convo-hash lanes; read at zenlanes.go:416 |
| `zen.hedge_after_ms` | config.go:191 | `0` | 2 s (zenproxy.go:565) | hedge a second request on another exit; `-1` disables |
| `freepi.queue_wait_secs` | config.go:230 | `0` | 60 s (freepi.go:48) | wait for a busy account to free up |

**Keys in the example with no field in `appConfig`:** `freebuff.*` (example:281-291, commented) has no Go struct and no code path (M16/H14).

**Default-value disagreements:**

| key | `defaultConfig()` | example | agrees? |
|---|---|---|---|
| `anonymize.ner.model_path` | `""` (config.go:342 sets only `TimeoutMs`, `MinScore`) | `"pii/model/model_q4.onnx"` (example:114) | no — benign only because a fresh config is seeded from the example; an existing config missing the key gets `""` |
| `freepi.antseed… prefer_free` | nil `*bool` → true (antseed.go:71) | `true` (example:321) | yes, effect identical |
| `anonymize.disclose_text` | full notice string (config.go:335) | `""` (example:97) | yes — `pii/pii.go:234-237` substitutes the same string |
| `translate.restore_tool_name_case` | nil `*bool` → true (config.go:102-107) | `true` (example:146) | yes |
| `zen.max_retries` | `4` (config.go:375) | `4` | value yes; **comment no** — see L29 |

**Presence-check coverage for default-true bools** (AGENTS.md rule): `nvidia.enabled`, `zen.enabled`, `usage.enabled`, `status.show_usage`, `status.show_pool`, `guardrails.{threshold,window_weight,density_weight}` all have raw-doc checks at `config.go:431-482` — but see M17, they test presence rather than value. `status.show_antseed` defaults false and needs none.

---

## Files covered

Every source file in the repository, git-tracked or untracked, excluding `.git/` and the vendored `.opencode/node_modules/`.

**Go — root (24)**
`main.go` (4520) · `anthropic.go` (2472) · `zenlanes.go` (1330) · `antseedp2p.go` (1142) · `zenproxy.go` (993) · `rtk.go` (982) · `config.go` (969) · `antseed.go` (797) · `freepi.go` (647) · `modelinfo.go` (334) · `usage.go` (184) · `piiwiring.go` (106) · `rtkcompress.go` (139) · `rtkdetect.go` (50)

**Go — tests (12)**
`main_test.go` (3601) · `todo_small_test.go` (685) · `zen_schema_test.go` (585) · `live_test.go` (277) · `stream_cancel_test.go` (184) · `regress_stream_nudge_test.go` (135) · `fidelity_test.go` (132) · `audit_fixes_test.go` (131) · `usage_test.go` (123) · `antseed_test.go` (105, untracked) · `lane_tokens_test.go` (85) · `fuzz_test.go` (73)

**Go — `pii/` package (14)**
`pii.go` (238) · `scan.go` (494) · `wordpiece.go` (508) · `ner_ort.go` (286) · `vault.go` (242) · `pii_test.go` (240) · `custom.go` (182) · `detect.go` (179) · `fake.go` (145) · `vault_test.go` (89) · `wordpiece_ref_test.go` (74) · `ner_ort_test.go` (31) · `span.go` (124) · `ner_stub.go` (14)

**Python (8)** `nimstatus.py` (355) · `scripts/freebuff_client.py` (349) · `scripts/probe_zen_proxies.py` (238) · `scripts/antseed_test.py` (176) · `scripts/freepi_probe.py` (126) · `scripts/freepi_login.py` (117) · `scripts/freebuff_login.py` (83)

**Shell / deploy / CI (5)** `nim` (124) · `.github/workflows/build.yml` (75) · `deploy/com.user.nimroute.plist` (22) · `deploy/nimroute.service` (16) · `deploy/nimroute.xml` (15)

**Config / data (5)** `config.yml.example` (370) · `guardrails.json` (8433) · `model_info.jsonc` (123) · `go.mod` · `go.sum`

**Docs (15)** `AGENTS.md` (137) · `ANTSEED-DOCS.md` (532) · `TODO.md` (471) · `FREEBUFF-DOCS.md` (170) · `ANTSEED-PLAN.md` (164) · `PLAN.md` (102) · `FREEPI-DOCS.md` (107) · `CHANGED-SINCE-682b2eb.md` (134) · `FREEBUFF-PLAN.md` (81) · `ANTSEED-TODO.md` (71) · `FREEBUFF-MODEL-TABLE.md` (65) · `FREEBUFF-TODO.md` (45) · `README.md` (34) · `LICENSE` · `NOTICE`

**Other (5)** `pii/fetch_model.sh` · `pii/testdata/vocab.txt` (data) · `.gitignore` (48) · `scripts/` directory listing · git history (`git log`, `git status`, `git show`)

### Coverage gaps

None. Every file in the inventory below was reviewed. Twelve areas went to parallel subagents (`main.go` got two, one general and one adversarial security pass); the four whose agents did not return usable reports — `pii/`, the zen pool/lane internals, the RTK transform, and the test suite — I read end-to-end myself and verified the resulting claims against the source before writing them up.

| area | files | status |
|---|---|---|
| zen pool / lane internals | `zenproxy.go` (993), `zenlanes.go` (1330) | covered — self-audited line-by-line (M65–M70, L38–L40) plus the security-pass findings verified earlier |
| test-suite quality | 16 test files | covered — self-audited end-to-end (M73–M75 and the coverage notes below) |

Spot-checking a random sample of the line references against the current source found no drift: every cited line still holds the quoted code.
