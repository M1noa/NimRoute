#!/usr/bin/env python3
"""bench_analyze.py - mine a nimroute access log for latency and retry shape.

parses the acclog lines the proxy already writes (no proxy changes needed):
  req-N --> METHOD PATH from IP (N bytes)          request arrival
  req-N routed to opencode model=... endpoint=...  zen dispatch
    opencode retry N/M session=... proxy=... lane=...
  !! opencode zen error (retry N/M) model=... country=... proxy=... : err
    opencode session=... proxy=... lane=...        exit actually used
  <- STATUS METHOD PATH DURATION BYTES [backend] model=... req-N

usage:
  python3 scripts/bench_analyze.py nimroute.log
  python3 scripts/bench_analyze.py bench-stdout.log --top 15
"""
import argparse
import collections
import re
import sys

RE_ARRIVE = re.compile(r"^(?P<ts>\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}) (req-\d+) --> (\w+) (\S+) from")
RE_DONE = re.compile(
    r"^(?P<ts>\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}) <- (?P<status>\d+) \S+ (?P<path>\S+) "
    r"(?P<dur>[\dm\.]+s) (?P<bytes>\d+) bytes(?: \[(?P<backend>[^\]]+)\])?"
    r"(?: model=(?P<model>\S+))?(?: (?P<req>req-\d+))?")
RE_RETRY = re.compile(r"opencode retry (?P<n>\d+)/(?P<of>\d+) session=(?P<ses>\S+) proxy=(?P<proxy>\S+) lane=(?P<lane>\S+)")
RE_ERR = re.compile(r"opencode zen error \(retry (?P<n>\d+)/(?P<of>\d+)\) model=(?P<model>\S+) country=(?P<cc>\S+) proxy=(?P<proxy>\S+)")
RE_SESSION = re.compile(r"opencode session=(?P<ses>\S+) proxy=(?P<proxy>\S+) lane=(?P<lane>\S+)")
RE_DIALDEAD = re.compile(r"opencode dial-dead")


def dur_to_ms(s):
    total = 0.0
    m = re.match(r"(?:(\d+)m)?([\d.]+)s$", s)
    if not m:
        return None
    if m.group(1):
        total += int(m.group(1)) * 60000
    total += float(m.group(2)) * 1000
    return total


def pct(v, p):
    v = sorted(v)
    if not v:
        return None
    return round(v[max(0, min(len(v) - 1, int(round(len(v) * p)) - 1))], 0)


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("log")
    ap.add_argument("--top", type=int, default=10, help="show N slowest requests")
    args = ap.parse_args()

    durations = []
    by_path = collections.Counter()
    by_status = collections.Counter()
    by_backend = collections.Counter()
    by_model = collections.Counter()
    model_ms = collections.defaultdict(list)
    retries = collections.Counter()
    err_kinds = collections.Counter()
    err_by_model = collections.Counter()
    exits = collections.Counter()
    lanes = collections.Counter()
    dialdead = 0
    arrivals = 0
    slow = []

    with open(args.log, errors="replace") as f:
        for line in f:
            if RE_ARRIVE.match(line):
                arrivals += 1
                continue
            m = RE_DONE.match(line)
            if m:
                ms = dur_to_ms(m.group("dur"))
                if ms is None:
                    continue
                durations.append(ms)
                by_path[m.group("path")] += 1
                by_status[m.group("status")] += 1
                if m.group("backend"):
                    by_backend[m.group("backend")] += 1
                if m.group("model"):
                    by_model[m.group("model")] += 1
                    model_ms[m.group("model")].append(ms)
                slow.append((ms, line.strip()))
                continue
            m = RE_RETRY.search(line)
            if m:
                n = int(m.group("n"))
                retries[n] += 1
                exits[m.group("proxy")] += 1
                lanes[m.group("lane").rstrip(",")] += 1
                continue
            m = RE_ERR.search(line)
            if m:
                tail = line.split(":", 1)[-1].strip()
                if "http2" in tail and "timeout awaiting response headers" in tail:
                    kind = "http2 timeout awaiting response headers"
                elif "connection refused" in tail:
                    kind = "connection refused"
                elif "i/o timeout" in tail:
                    kind = "i/o timeout"
                elif "unexpected EOF" in tail:
                    kind = "unexpected EOF"
                elif "EOF" in tail:
                    kind = "EOF"
                elif "reset by peer" in tail:
                    kind = "reset by peer"
                elif "x509" in tail:
                    kind = "tls x509"
                elif "Too many open connections" in tail:
                    kind = "too many open connections"
                elif "context canceled" in tail:
                    kind = "context canceled"
                elif "Bad Gateway" in tail:
                    kind = "bad gateway"
                else:
                    kind = tail[:60]
                err_kinds[kind] += 1
                err_by_model[m.group("model")] += 1
                continue
            if RE_DIALDEAD.search(line):
                dialdead += 1
                continue
            m = RE_SESSION.search(line)
            if m:
                exits[m.group("proxy")] += 1
                lanes[m.group("lane").rstrip(",")] += 1

    print("=" * 78)
    print("log: %s" % args.log)
    print("completed requests: %d   arrivals logged: %d" % (len(durations), arrivals))
    print("=" * 78)
    if durations:
        print("\nduration ms:")
        for p in (.5, .75, .9, .95, .99):
            print("  p%-4s %10s" % (int(p * 100), pct(durations, p)))
        print("  max   %10s" % round(max(durations), 0))
        over = [d for d in durations if d > 10000]
        print("  >10s: %d (%.0f%%)   >30s: %d   >60s: %d" % (
            len(over), 100.0 * len(over) / len(durations),
            len([d for d in durations if d > 30000]),
            len([d for d in durations if d > 60000])))
    print("\nby path:    %s" % dict(by_path))
    print("by status:  %s" % dict(by_status))
    print("by backend: %s" % dict(by_backend))

    if err_kinds:
        print("\nupstream errors (%d total):" % sum(err_kinds.values()))
        for k, v in err_kinds.most_common():
            wasted = ""
            if "http2" in k or "i/o timeout" in k:
                wasted = "  (~%ds burned at 30s header timeout)" % (v * 30)
            print("  %-45s %4d%s" % (k, v, wasted))
    if retries:
        print("\nretry attempts seen: %d (retry index -> count)" % sum(retries.values()))
        print("  %s" % dict(sorted(retries.items())))
    if dialdead:
        print("dial-dead re-picks: %d" % dialdead)

    if by_model:
        print("\nper-model requests (top 12 by count):")
        for m, c in by_model.most_common(12):
            ms = model_ms[m]
            print("  %-42s %5d  p50=%7sms  max=%8sms" % (m, c, pct(ms, .5), round(max(ms), 0)))

    print("\ndistinct exits used: %d   distinct lanes used: %d" % (len(exits), len(lanes)))

    if slow:
        print("\nslowest %d requests:" % args.top)
        for ms, line in sorted(slow, reverse=True)[:args.top]:
            print("  %-70s" % line[:150])
    return 0


if __name__ == "__main__":
    sys.exit(main())
