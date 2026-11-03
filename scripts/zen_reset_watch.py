#!/usr/bin/env python3
"""Watch a fixed set of capped exits until they serve again, to time the reset.

Picks N exits that the last sweep found alive-but-rate-limited, writes them to
a watchlist file, then re-probes exactly those exits every interval, logging
the raw status, headers (date / retry-after / x-opencode-log-id), body and
timing of every single attempt. stops as soon as any of them serves, and
prints the bracket: last cycle where all were capped, first cycle where one
was not.

the point is to test the "midnight utc" theory from the sweep, which inferred
it from date+retry-after. this watches the same exits actually flip and
records the wall clock it flipped at, so the reset instant is measured rather
than derived.

  scripts/zen_reset_watch.py                       # 30 exits, every 5 min
  scripts/zen_reset_watch.py --n 40 --interval 120
  scripts/zen_reset_watch.py --status              # show the watchlist only

watchlist (/tmp/zen_reset_watch.json) is reused verbatim on every later run,
so the same exits are followed across restarts; delete it to re-sample.
one json line per probe in /tmp/zen_reset_watch.jsonl, cycle summaries
included with "kind":"cycle". full bodies + headers land under
/tmp/zen_reset_bodies.
"""
import argparse
import calendar
import csv
import email.utils
import importlib.util
import json
import os
import random
import re
import subprocess
import sys
import tempfile
import time
from collections import Counter, defaultdict
from concurrent.futures import ThreadPoolExecutor

HERE = os.path.dirname(os.path.abspath(__file__))
ZEN = "https://opencode.ai/inference/v1"
DIRECT = "(direct)"
MODELS = []


def load_sweep():
    """reuse the sweep's toolset/ua/body/classify instead of copying them."""
    path = os.path.join(HERE, "zen_proxy_sweep.py")
    spec = importlib.util.spec_from_file_location("zen_proxy_sweep", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


SW = load_sweep()


def safe_name(proxy):
    return re.sub(r"[^A-Za-z0-9.]", "_", proxy)


def parse_headers(text):
    """first block only: curl repeats the block per response, and only the
    upstream answer matters here (proxy 407 blocks are not zen's)."""
    out = {}
    for line in text.splitlines():
        line = line.strip()
        if not line:
            continue
        if ":" not in line:
            continue
        k, v = line.split(":", 1)
        out.setdefault(k.strip().lower(), v.strip())
    return out


def reset_of(headers, ts):
    """the instant this answer claims the cap lifts: response date + retry-after.

    without the date header the countdown is useless (retry-after alone is
    relative to a response we cannot see later), so both must be present.
    """
    ra = headers.get("retry-after", "")
    if not ra.isdigit():
        return ""
    base = ts
    d = headers.get("date", "")
    if d:
        try:
            base = email.utils.parsedate_to_datetime(d).timestamp()
        except Exception:  # noqa: BLE001 - malformed date header
            return ""
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(base + int(ra)))


def raw_probe(proxy, model, tools, ua, rng, max_time):
    """one realistic request, returning everything the response carried.

    proxy=None sends it direct: that is the control exit nobody else shares,
    so a still-capped control after a claimed reset means the deadline the
    response advertises is not the real one.
    """
    body = json.dumps(SW.build_body(rng, model, tools)).encode()
    sess = SW.new_id(rng, "ses", descending=True)
    req_id = SW.new_id(rng, "msg")
    fd, hpath = tempfile.mkstemp(prefix="zenhdr")
    os.close(fd)
    cmd = ["curl", "-sS", "--max-time", str(max_time), "--connect-timeout", "10",
           "-o", "-", "-w", "\n%{http_code}", "-X",
           "POST", ZEN + "/chat/completions", "-D", hpath,
           "-H", "Content-Type: application/json",
           "-H", "Accept: */*",
           "-H", "Authorization: Bearer public",
           "-H", "User-Agent: " + ua,
           "-H", "x-opencode-client: cli",
           "-H", "x-opencode-project: global",
           "-H", "x-opencode-session: " + sess,
           "-H", "x-opencode-session-id: " + sess,
           "-H", "x-opencode-request: " + req_id,
           "--data-binary", "@-"]
    if proxy:
        cmd[cmd.index("-D"):cmd.index("-D")] = ["--proxy", proxy]
    t0 = time.time()
    rec = {"proxy": proxy or DIRECT, "model": model, "ts": t0,
           "ts_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(t0)),
           "sess": sess, "msg": req_id}
    try:
        p = subprocess.run(cmd, input=body, capture_output=True,
                           timeout=max_time + 5)
    except subprocess.TimeoutExpired:
        os.unlink(hpath)
        rec.update(status=0, class_="timeout", ms=int((time.time() - t0) * 1000),
                   error="curl timeout")
        return rec
    except Exception as e:  # noqa: BLE001 - missing curl, dead fork, etc
        os.unlink(hpath)
        rec.update(status=0, class_="net-error", ms=0, error=str(e)[:200])
        return rec
    rec["ms"] = int((time.time() - t0) * 1000)
    if p.returncode != 0:
        err = p.stderr.decode("utf-8", "replace").strip().splitlines()
        os.unlink(hpath)
        rec.update(status=0, class_="net-error",
                   error=(err[-1] if err else "curl rc=%d" % p.returncode)[:200])
        return rec
    out = p.stdout.decode("utf-8", "replace")
    *lines, tail = out.rsplit("\n", 1)
    body_text = "\n".join(lines)
    try:
        status = int(tail.strip())
    except ValueError:
        os.unlink(hpath)
        rec.update(status=0, class_="net-error", body=body_text[:4000],
                   error="bad curl status line: %r" % tail[:80])
        return rec
    try:
        htext = open(hpath, errors="replace").read()
    finally:
        os.unlink(hpath)
    headers = parse_headers(htext)
    rec.update(status=status, class_=SW.classify(status, body_text),
               body=body_text[:4000], body_len=len(body_text),
               body_truncated=len(body_text) > 4000,
               headers=headers, headers_raw=htext[:4000],
               retry_after=headers.get("retry-after", ""),
               date=headers.get("date", ""), log_id=headers.get("x-opencode-log-id", ""),
               cf_placement=headers.get("cf-placement", ""),
               reset_utc=reset_of(headers, t0 + rec["ms"] / 1000.0))
    return rec


def pick_watchlist(tsv, n, rng):
    """rate-limited rows from a sweep: those dialed and zen answered, so a
    dead proxy is not mistaken for a capped one."""
    rows = []
    with open(tsv, newline="") as f:
        for r in csv.DictReader(f, delimiter="\t"):
            if r.get("result") == "rate-limited" and r.get("proxy"):
                rows.append(r["proxy"])
    seen, uniq = set(), []
    for p in rows:
        host = p.split("//", 1)[-1].rsplit(":", 1)[0]
        if host in seen:
            continue
        seen.add(host)
        uniq.append(p)
    if not uniq:
        sys.exit("no rate-limited exits in %s: run zen_proxy_sweep.py first" % tsv)
    rng.shuffle(uniq)
    return uniq[:n]


def show_watchlist(path):
    if not os.path.exists(path):
        sys.exit("no watchlist at %s (run this once without --status)" % path)
    with open(path) as f:
        wl = json.load(f)
    print("%s: %d exits, created %s" % (path, len(wl["exits"]), wl["created_utc"]))
    for e in wl["exits"]:
        print("  %-34s %s" % (e["proxy"], e["model"]))


def load_watchlist(path, tsv, n, rng, refresh):
    if os.path.exists(path) and not refresh:
        with open(path) as f:
            wl = json.load(f)
        print("watchlist: %d exits reused from %s (created %s)" % (
            len(wl["exits"]), path, wl["created_utc"]))
        return wl
    proxies = pick_watchlist(tsv, n, rng)
    wl = {
        "created_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "source": tsv,
        "exits": [{"proxy": p, "model": rng.choice(MODELS)}
                  for p in proxies],
    }
    with open(path, "w") as f:
        json.dump(wl, f, indent=2)
    print("watchlist: %d exits sampled from %s -> %s" % (len(proxies), tsv, path))
    return wl


def cycle_summary(cycle, recs, t0, ctrl):
    alive = [r for r in recs if r["status"]]
    capped = [r for r in alive if r["class_"] == "rate-limited"]
    serving = [r for r in alive if r["class_"] == "ok"]
    resets = [r["reset_utc"] for r in capped if r.get("reset_utc")]
    ras = [int(r["retry_after"]) for r in capped if r.get("retry_after", "").isdigit()]
    ctrl_alive = [r for r in ctrl if r["status"]]
    s = {
        "kind": "cycle", "cycle": cycle, "ts": t0,
        "ts_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(t0)),
        "n": len(recs), "alive": len(alive), "capped": len(capped),
        "serving": len(serving),
        "classes": dict(Counter(r["class_"] for r in recs)),
        "retry_after_min": min(ras) if ras else None,
        "retry_after_max": max(ras) if ras else None,
        "reset_utc_min": min(resets) if resets else None,
        "reset_utc_max": max(resets) if resets else None,
        "serving_proxies": [r["proxy"] for r in serving],
        "ctrl": [{"proxy": r["proxy"], "ctrl": r.get("ctrl"), "status": r["status"],
                  "class": r["class_"], "retry_after": r.get("retry_after", ""),
                  "reset_utc": r.get("reset_utc", "")} for r in ctrl],
        "ctrl_serving": [r["proxy"] for r in ctrl_alive if r["class_"] == "ok"],
    }
    line = ("cycle %-3d %s  capped=%-3d serving=%-2d dead=%-3d retry-after %s" % (
        cycle, s["ts_utc"], len(capped), len(serving),
        len(recs) - len(alive), "%s..%s" % (min(ras), max(ras)) if ras else "-"))
    if resets:
        # the response advertises a lift instant; whether the exits actually
        # serve after it is a separate fact, and the one worth watching.
        line += "  claim %s..%s" % (min(resets), max(resets))
    if ctrl:
        got = "%d/%d ok" % (len(s["ctrl_serving"]), len(ctrl_alive))
        line += "  ctrl %s" % ("SERVING " + ",".join(s["ctrl_serving"])
                                if s["ctrl_serving"] else got)
    print(line, flush=True)
    return s


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--from", dest="tsv", default="/tmp/zen_proxy_sweep.tsv")
    ap.add_argument("--watchlist", default="/tmp/zen_reset_watch.json")
    ap.add_argument("--log", default="/tmp/zen_reset_watch.jsonl")
    ap.add_argument("--bodies", default="/tmp/zen_reset_bodies",
                    help="dir for full bodies + headers per cycle (empty to skip)")
    ap.add_argument("--n", type=int, default=30)
    ap.add_argument("--interval", type=int, default=300)
    ap.add_argument("--controls", type=int, default=3,
                    help="fresh capped exits sampled per cycle, outside the watchlist")
    ap.add_argument("--no-direct", action="store_true",
                    help="skip the no-proxy control probe")
    ap.add_argument("--threads", type=int, default=15)
    ap.add_argument("--max-time", type=int, default=12)
    ap.add_argument("--cycles", type=int, default=0, help="stop after N cycles (0 = until one serves)")
    ap.add_argument("--stop-serving", type=int, default=1,
                    help="stop once this many exits serve")
    ap.add_argument("--refresh", action="store_true", help="re-sample the watchlist")
    ap.add_argument("--status", action="store_true", help="print the watchlist and exit")
    ap.add_argument("--seed", type=int, default=0)
    a = ap.parse_args()

    if a.status:
        show_watchlist(a.watchlist)
        return 0

    tools = SW.load_tools()
    ua = SW.load_ua()
    MODELS[:] = SW.free_models(ua, [])
    rng = random.Random(a.seed or int.from_bytes(os.urandom(8), "big"))
    print("tools: %d  UA: %s" % (len(tools), ua))

    wl = load_watchlist(a.watchlist, a.tsv, a.n, rng, a.refresh)
    exits = wl["exits"]
    if not exits:
        sys.exit("empty watchlist")
    watched = {e["proxy"] for e in exits}
    ctrl_pool = [p for p in pick_watchlist(a.tsv, 10 ** 6, rng) if p not in watched]
    print("controls: %s + %d fresh capped exit/cycle (%d unused in the sweep)" % (
        "direct" if not a.no_direct else "none", a.controls, len(ctrl_pool)))

    if a.bodies:
        os.makedirs(a.bodies, exist_ok=True)
    log = open(a.log, "a", buffering=1)
    print("logging to %s (interval %ds, stop when %d serve)\n" % (
        a.log, a.interval, a.stop_serving))

    history = []
    per_exit = defaultdict(list)
    last_all_capped = None
    claim_open = None
    claims_missed = []
    cycle = 0
    try:
        while True:
            cycle += 1
            t0 = time.time()
            jobs = [(e["proxy"], e["model"], "") for e in exits]
            if not a.no_direct:
                jobs.append((None, rng.choice(MODELS), "direct"))
            for _ in range(min(a.controls, len(ctrl_pool))):
                jobs.append((ctrl_pool.pop(), rng.choice(MODELS), "fresh"))
            # one pool for every probe: controls must answer at the same
            # instant as the watched exits or the comparison says nothing.
            with ThreadPoolExecutor(max_workers=a.threads) as ex:
                allrecs = list(ex.map(
                    lambda j: dict(raw_probe(j[0], j[1], tools, ua, rng, a.max_time),
                                   ctrl=j[2]),
                    jobs))
            recs = [r for r in allrecs if not r["ctrl"]]
            crecs = [r for r in allrecs if r["ctrl"]]
            for r in allrecs:
                if not r["ctrl"]:
                    per_exit[r["proxy"]].append(r)
                log.write(json.dumps(r, sort_keys=True) + "\n")
                if a.bodies:
                    stem = os.path.join(a.bodies, "c%03d-%s-%d" % (
                        cycle, safe_name(r["proxy"]), r["status"]))
                    with open(stem + ".body", "w") as f:
                        f.write(r.get("body") or r.get("error", ""))
                    with open(stem + ".headers", "w") as f:
                        f.write(r.get("headers_raw", ""))
            s = cycle_summary(cycle, recs, t0, crecs)
            history.append(s)
            log.write(json.dumps(s, sort_keys=True) + "\n")

            # a claimed lift that passes with every watched exit and the
            # direct control still capped is the finding, not a waiting room:
            # retry-after points at the next utc midnight unconditionally.
            claim = s["reset_utc_min"]
            if claim_open and s["ts_utc"] > claim_open and not s["serving"] \
                    and not s["ctrl_serving"]:
                print("  ... claimed lift %s passed, watchlist + control still capped:\n"
                      "      the advertised deadline is not the reset." % claim_open,
                      flush=True)
                log.write(json.dumps({"kind": "claim-missed", "claim": claim_open,
                                      "at": s["ts_utc"]}, sort_keys=True) + "\n")
                claims_missed.append(claim_open)
                claim_open = claim
            elif not claim_open:
                claim_open = claim

            if s["serving"] >= a.stop_serving:
                break
            if s["ctrl_serving"] and not a.no_direct:
                print("  ... control served (%s): the watchlist exits are spent individually,\n"
                      "      not the tier as a whole. stopping to keep the record clean."
                      % ", ".join(s["ctrl_serving"]), flush=True)
                break
            if not s["serving"]:
                last_all_capped = s
            if a.cycles and cycle >= a.cycles:
                break
            # keep the cadence from the cycle start: probing 30 exits takes a
            # few seconds and should not drift the check times.
            time.sleep(max(0, a.interval - (time.time() - t0)))
    except KeyboardInterrupt:
        print("\ninterrupted")
    finally:
        log.close()

    serving = [h for h in history if h["serving"]]
    print("\n-- reset bracket --")
    if serving:
        first = serving[0]
        print("  first serving cycle: %s (%d/%d exits, %s)" % (
            first["ts_utc"], first["serving"], first["n"],
            ", ".join(first["serving_proxies"][:5])))
        if last_all_capped:
            print("  last all-capped cycle: %s (retry-after %s..%s, claimed reset %s..%s)" % (
                last_all_capped["ts_utc"], last_all_capped["retry_after_min"],
                last_all_capped["retry_after_max"], last_all_capped["reset_utc_min"],
                last_all_capped["reset_utc_max"]))
            print("  reset happened between those two checks (<= %ds apart)" % a.interval)
        ts = calendar.timegm(time.strptime(first["ts_utc"], "%Y-%m-%dT%H:%M:%SZ"))
        midnight = calendar.timegm(time.strptime(
            first["ts_utc"][:10] + "T00:00:00", "%Y-%m-%dT%H:%M:%S"))
        print("  first serving check: %+.0f s from 00:00:00Z" % (ts - midnight))
    else:
        print("  no exit served in %d cycles" % cycle)
        if last_all_capped:
            print("  still capped at %s, retry-after %s..%s" % (
                last_all_capped["ts_utc"], last_all_capped["retry_after_min"],
                last_all_capped["retry_after_max"]))
        for c in claims_missed:
            print("  claimed lift %s passed with everything still capped" % c)

    ctrl = [c for h in history for c in h["ctrl"]]
    if ctrl:
        print("\n-- controls (probed in the same instant as the watchlist) --")
        direct = [c for c in ctrl if c["ctrl"] == "direct"]
        fresh = [c for c in ctrl if c["ctrl"] == "fresh"]
        if direct:
            print("  direct, no proxy (%d probes): %s" % (
                len(direct), " ".join("%s/%s" % (c["class"], c["status"])
                                      for c in direct[-8:])))
        if fresh:
            tally = Counter(c["class"] for c in fresh)
            print("  fresh capped exits (%d): %s" % (
                len(fresh), "  ".join("%s=%d" % kv for kv in tally.most_common())))
            if not any(c["class"] == "ok" for c in ctrl):
                print("  no ip outside the watchlist served either: this is the exit\n"
                      "  space as a whole, not the 30 exits picked here.")

    # per exit: the countdown sequence is the evidence for or against one
    # shared reset instant. a per-exit budget drifts apart across cycles, a
    # shared one keeps pointing at the same second.
    print("\n-- per exit (retry-after by cycle, 0 = no header) --")
    print("  %-34s %-28s %s" % ("proxy", "model", "cycles -> first non-capped"))
    for e in exits:
        p = e["proxy"]
        seq = [r.get("retry_after") or "0" for r in per_exit[p]]
        flips = [r for r in per_exit[p] if r["class_"] != "rate-limited"]
        when = "%s %s" % (flips[0]["ts_utc"], flips[0]["class_"]) if flips else "never"
        print("  %-34s %-28s %s  | %s" % (
            p, e["model"], ",".join(seq[:12]), when))

    return 0 if serving else 1


if __name__ == "__main__":
    sys.exit(main())
