# zen free tier: what the measurements say

**Dates:** 2026-11-03/04. Midday sweeps 16:45–18:30 UTC, plus the 00:00 UTC
reset window on 11-03 and 11-04.
**Method:** `scripts/zengate` against the real `opencode.ai/inference/v1`
upstream through the pool, with `-probe` (invalid-role body) for zero-cost
gate checks and real completions where a serve had to be proven. Every number
below came back from a response; nothing here is inferred from documentation.

---

## TL;DR

The refusal is not fingerprint detection and not our request shape. The free
tier meters a small allowance per **egress IP × model**, answers spent pairs
with a canned `429 FreeUsageLimitError`, and geo-gates individual models per
country. Two consequences drive every observation in this file:

1. The 429 is **not sticky**. One exit answered 429, 429, then 200 with a real
   completion inside four seconds. Anything that treats a 429 as a verdict on
   an IP is wrong, and that included our tier-cap parking.
2. The 429 carries **no information about the IP's remaining balance**. Its
   `Retry-After` is seconds to 00:00 UTC, identical in shape on every exit.

What does not follow from the measurements — and the earlier version of this
file got this wrong — is that the pool refills at midnight. At 00:00:05 UTC on
11-03, four minutes after every bucket reset, **1 of 1726 exits** passed the
gate and served. A shared-list-drained-by-the-community story cannot produce
that number, because nobody had spent anything yet.

---

## 1. Wire identity

The TLS hello is a fixed spec (`zentls.go`): bun/BoringSSL extension order,
ALPN `http/1.1` only, hybrid X25519MLKEM768 key share first, no GREASE. The
2026-10-26 single-variable A/B on one exit, one minute, identical body:

| client | result |
|---|---|
| go `crypto/tls` | 429 |
| curl | 429 |
| utls + `zentlsSpec` | 200, real completion |

That is a proof about the accept path, and it holds today: the same spec
served real completions from `122.52.157.170`, `203.205.49.2` and
`180.191.229.72` this session, and refused on hundreds of other exits in the
same minute. No wire-level check produces a serve/refuse split across exits
with one hello.

**Limitation, stated plainly:** a JA3/JA4 hash-for-hash comparison against the
released binary was attempted and did not complete. A CONNECT-level tee
recorded zero ClientHello records, so there is no hash table here to show. The
A/B above and the same-hello serve/refuse spread already rule a wire-level
discriminator out; a hash table would only restate it.

## 2. The 429 flaps

Same exit, fresh session each attempt, real completions:

```
180.191.229.72:5050   attempt 1  429
                      attempt 2  429
                      attempt 3  200  data:{"id":"chatcmpl-ef9daac1..."}
203.205.49.2          attempt 1  200
                      attempt 2  200
122.52.157.170:8082   200, 200, then 429
```

Four seconds separate the last refusal from the serve on `180.191.229.72`. Two
mechanisms fit: origin-PoP state that has not converged, or a per-exit
short-window meter that refills immediately. Either way the practical rule is
the same — a single 429 does not justify rotating off an exit. The ladder now
re-sends once on the same pin with a fresh session before it marks anything.

## 3. `Retry-After` is a clock, not a balance

```
17:51:59 UTC   seconds to 00:00 UTC = 22081   Retry-After: 22079
02:46 UTC      seconds to 00:00 UTC = 76440   Retry-After: 76440
```

Two seconds of drift on the first, exact on the second. Every exit reports the
same value at the same instant regardless of how much it has served, so the
header cannot be read as that IP's window. Honouring it parked live exits
until the next midnight; `noteExitTierCap` no longer parks on a 429 at all, and
the value is recorded for logs only.

## 4. Quota is per exit × model

One exit answered `429 FreeUsageLimitError` for `glm-5.3-flash-free` and
`deepseek-v4.1-flash-free` while passing gate probes for
`nemotron-3.5-lightning-free` and `beam-free` in the same minute. The repo's
2026-10-28 measurement is the same shape: one exit answered the cap for
big-pickle, deepseek, glm and mimo, then served a real `beam-free` completion
from the same IP in the same minute.

Consequence for the pool: marking must be per exit+model, and `exitUsableFor`
already scoped it that way. What did not was `noteExitTierCap`, which parked
the whole IP — one model's cap locked the exit for every model. Removed.

## 5. `RegionError` is per model × country

Identical 400-sample sweeps, same minute, same exits:

| model | 403 RegionError |
|---|---|
| beam-free | 119 |
| pulse-alpha-free | 30 |
| the other seven free ids | 0 |

A region block is a property of (model, exit country), not of the exit. The
gate is cached that way (`zenRegionGates`, keyed model+country, one refresh
TTL), the picker skips that model in that country, and the exit keeps serving
every model the region may use. `direct` and unknown countries are never
gated: missing data is not a region.

## 6. Reputation does not separate servers from refusers

AbuseIPDB answers 403 to scraping for every IP, so the keyless sources were
Scamalytics and ip-api. Serves and refusals, same day:

| IP | outcome | ASN | hosting | fraud score |
|---|---|---|---|---|
| 122.52.157.170 | 200 | PLDT (isp) | false | 4 |
| 180.191.229.72 | 200 | Globe (isp) | false | 5 |
| 203.205.49.2 | 200 | CMC (isp) | false | 0 |
| 31.76.110.221 | 200 (reset window) | EdgeSec | true | 0 |
| 195.201.246.53 | 429 | Hetzner | — | 5 |
| 42.115.72.145 | 429 | FPT | — | 4 |
| 203.81.69.126 | 429 | Myanma Post | — | 0 |
| 38.19.238.36 | 429 | Red Planet | — | 0 |
| 167.99.3.26 | 429 | DigitalOcean | — | 52 |
| 45.74.31.40–42 | 429 | SULAVI | — | 25 |

Refusers score 0, 4, 5, 25 and 52; servers score 0, 4, 5 and 0. The cleanest
IPs in the pool refuse. One serving IP is hosting-flagged. Reputation, as
measurable without a paid feed, does not predict the answer.

What does correlate is ASN size. Thirty exits each from Google (396982),
Linode (63949) and AWS (16509) served **zero** completions; the serves came
from PLDT, Globe, CMC and one small hosting ASN.

## 7. What the reset actually shows

The 11-03 00:00:05 UTC run, on the whole list:

```
1726 distinct exits, 100 workers
gate accepted 1 of 1726 exits (400 = shape ok, 429 = refused)
served  http://31.76.110.221:1080   glm-5.3-flash-free
```

Four minutes after every bucket refilled, 1 exit of 1726 served. The pool
today, at 17:47–17:54 UTC, was the same order of magnitude: 3,900 probes across
nine models in one hour found exactly **two** exits with budget, and an
800-exit completion sweep found 2 serves against 325 refusals.

The 11-04 reset run was launched (`/tmp/zenmidnight.log`) and never produced a
result: the file holds only `waiting 6h36m36s for 00:00:05 utc ...` and no
process survived to the deadline. That measurement is missing, not negative.

## 8. The one fact that resists a clean model

A privacy IPv6 `/128` on this machine's residential `/64`, an address that has
never spoken to zen, answered `429 FreeUsageLimitError` on its first ever
contact, on two models. Its `/64` had traffic that day, so the meter is
plausibly keyed on something wider than one address. Meanwhile the same host's
IPv4 answers structured errors normally on that same body:

```
{}                       400  "`model` must be a non-empty string"
{"model":"nope"}         400  "Model is unavailable"
paid model + bogus key   401  "Invalid credential"
free model               429  FreeUsageLimitError
```

An IP blocked at the edge or at Cloudflare would not answer validation and
auth errors at all. So the host is not blocked; the free tier refuses it.

Taken with §7, the picture is a tier that grants almost nothing to almost
every egress it sees, with a thin band of ISP space that still serves. That is
an observation about the distribution, not a mechanism I can point at. The
honest statement of confidence: shape, TLS and reputation are excluded; the
address grain and the reset behaviour are not explained by anything measured
here.

## 9. What changed in nimroute

Ladder and pool state, smallest diff that follows from the measurements:

- **Pair-only marking.** `noteExitTierCap` no longer parks an IP or drops it
  from the pool on a 429; `noteExit429` keeps the exit+model mark. A model's
  cap no longer costs the exit for every other model.
- **One same-exit retry.** On the first 429 of a request, with a real pin and
  more than one hop of budget left, both ladders re-mint the session
  (`laneNewSession`, fresh `ses_*`, same pin) and resend once before rotating.
  Only the second refusal marks the pair.
- **Region cache.** `zenRegionGated` joins `exitUsableFor`; the geo branch
  files a (model, country) block instead of dropping the exit.
- **Pool filter.** `refreshZenProxies` skips `ip_type: hosting` candidates,
  which measured 0 serves in 120 attempts and cost verify slots.

`zenlanes_test.go` pins the three state rules: a 429 marks one model only, a
tier cap keeps the exit drawable for other models, and a region block spares
other models and other countries.

## 10. If it looks broken again

1. Sweep **every** free model, not one. A single-model sweep is what made the
   earlier session look like detection: it samples one bucket per exit.
2. Two 400 gate-acceptances anywhere in that matrix mean the shape is fine and
   the tier is out; stop there and let the per-pair marks age out.
3. Zero 400s across every model *and* refusals on real completions means a
   shape drift. Follow AGENTS.md §5 "when the gate breaks again" and re-capture
   the released client rather than re-reading this file.
