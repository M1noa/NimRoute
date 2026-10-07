# ANTSEED-TODO.md — antseed p2p backend tracker

keyless free-model backend over the antseed p2p network.
design: fresh random identity per run (never saved), rotate on 429,
offer failover only (no lanes — 28/28 no-429 experiment killed that).

## done

- [x] recon: cloned antseed repo, reverse-engineered buyer proxy, wire
      protocol, free-vs-paid, catalog, vpn (see ANTSEED-DOCS.md)
- [x] plan: go integration plan p1-p4 (see ANTSEED-PLAN.md)
- [x] rate-limit experiment: 8 concurrent + 20 rapid to glm-5.3-flash =
      28/28 ok, zero 429s → no lanes/proxy machinery, failover only
- [x] deps: secp256k1/v4 v4.4.0 + x/crypto v0.57.0 (go 1.25→1.26.0)
- [x] antseedp2p.go: identity (mint/sign/recover), eip-191 + eip-712
      free-usage signing, tcp-enc handshake (x25519/hkdf/aes-gcm),
      9b frames, http-over-p2p codec, connect/socks5 dialers, conns,
      chunked upload, stream reassembly, needauth answering
- [x] antseed.go: stats-snapshot discovery, canon/base ids, free-only
      text filter, ranking, conn pool, anttry failover, both handlers
      (anthropic + openai passthrough), snapshot, models append
- [x] config: antseedConfig (enabled, stats_url, refresh_secs, trust_min,
      prefer_free *bool, proxies) + show_antseed flag + example block
- [x] wiring: antseed/ branches in anthropic.go + servehttp, handleModels
      append, StatusResponse.Antseed + StatusFor gate + unauth redaction,
      fallback filter excludes antseed/ ids
- [x] model_info.jsonc: 5 antseed/ entries (glm-5.3-flash,
      deepseek-v4-flash, minimax-m3, gpt-6-luna, mimo-v2.6-flash)
- [x] build green: gofmt clean, go vet clean, go build ./... ok

## to do

- [ ] unit tests (antseed_test.go): canon/base, textproto filter,
      free pricing (incl. cached fallback), hostport default, ranking
      order + trust gate + cooldown skip, failover cools + rotates
- [ ] full test suite green: `go test ./...`
- [ ] scratch-port e2e: enable antseed on :5420 scratch config,
      /v1/models lists antseed/<id>, chat completion ok, /status
      antseed section shows offers/peers/identity age
- [ ] teardown: delete scratch config/usage, confirm only intended
      files changed (`git status --porcelain`)
- [ ] live restart of primary (:5419) — left to user

## notes

- pre-existing uncommitted changes (anthropic.go, config.go, freepi.go,
  main.go, modelinfo.go, zenproxy.go, *_test.go, TODO.md) are NOT from
  this work — do not revert.
- sellers advertise ids like `z-ai/glm-5.3-flash`; display id strips
  vendor path, canon matches either form.
- minimax-m3 + gpt-6-luna entries are lineage estimates (no openrouter
  record); fix ctx/maxout when live catalog confirms.
- wire clone at /var/folders/nk/qtcnhdz964sg80k2fnz8nnnh0000gp/T/opencode/antseed
  (reference only, not part of repo).
