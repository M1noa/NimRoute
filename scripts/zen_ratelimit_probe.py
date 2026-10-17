#!/usr/bin/env python3
"""Characterize opencode zen's short-window rate limit.

Emulates the real opencode CLI fingerprint (the header set nimroute's
setZenHeaders sends) and the exact toolset zen's free-tier gate requires, then
fires requests at a range of inter-request gaps and burst widths and records
the status code each one gets. Used to pick nimroute's per-lane gap: a 429 here
is what laneRotate exists to absorb.

  scripts/zen_ratelimit_probe.py --pilot          one request, confirm the gate accepts us
  scripts/zen_ratelimit_probe.py --levels         the gap sweep (sequential)
  scripts/zen_ratelimit_probe.py --burst 4,8      concurrent bursts of N
"""
import argparse
import json
import os
import re
import ssl
import sys
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor

BASE = "https://opencode.ai/inference/v1"
HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
B62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
MASK = 0xFFFFFFFFFFFF


def load_tools():
    """pull the captured opencode toolset straight out of zentools.go."""
    src = open(os.path.join(ROOT, "zentools.go")).read()
    m = re.search(r'const zenOCDefsJSON = ("(?:[^"\\]|\\.)*")', src)
    if not m:
        sys.exit("could not find zenOCDefsJSON in zentools.go")
    # group(1) is the go string literal: decode it, then parse the inner json
    return json.loads(json.loads(m.group(1)))


def load_ua():
    """opencode/<ver> from the live default in zenproxy.go."""
    src = open(os.path.join(ROOT, "zenproxy.go")).read()
    m = re.search(r'zenVersion\s*=\s*"([^"]+)"', src)
    ver = m.group(1) if m else "1.18.31"
    return "opencode/%s ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14" % ver


def session_id():
    """ses_ + descending(12 hex time + 14 base62), same shape as zenID(true)."""
    ts = int(time.time() * 1000)
    masked = (~(ts << 12)) & MASK
    return "ses_%012x%s" % (masked, "".join(
        B62[b % 62] for b in os.urandom(14)))


def post(model, tools, ua, timeout=30, max_tokens=8):
    body = json.dumps({
        "model": model,
        "stream": True,
        "max_tokens": max_tokens,
        "messages": [
            {"role": "system", "content": "You are a helpful assistant."},
            {"role": "user", "content": "say ok"},
        ],
        "tools": tools,
    }).encode()
    sid = session_id()
    req = urllib.request.Request(BASE + "/chat/completions", data=body, headers={
        "Content-Type": "application/json",
        "Authorization": "Bearer public",
        "x-opencode-client": "cli",
        "x-opencode-session": sid,
        "x-opencode-session-id": sid,
        "x-session-affinity": sid,
        "x-opencode-project": "global",
        "User-Agent": ua,
        "Accept": "*/*",
    })
    t0 = time.time()
    try:
        with urllib.request.urlopen(req, timeout=timeout,
                                    context=ssl.create_default_context()) as r:
            r.read(64)
            return r.status, (time.time() - t0) * 1000, ""
    except urllib.error.HTTPError as e:
        snippet = ""
        try:
            snippet = e.read(200).decode("utf-8", "replace").replace("\n", " ")
        except Exception:
            pass
        return e.code, (time.time() - t0) * 1000, snippet
    except Exception as e:
        return 0, (time.time() - t0) * 1000, str(e)[:120]


def summarize(codes):
    out = {}
    for c in codes:
        out[c] = out.get(c, 0) + 1
    return " ".join("%s=%d" % (k, out[k]) for k in sorted(out))


def run_levels(model, tools, ua, gaps, count):
    print("\n-- sequential sweep (gap between requests, same session per run) --")
    print("%-8s %-6s %s" % ("gap(s)", "n", "statuses (min..max latency ms)"))
    for gap in gaps:
        codes, lats = [], []
        for i in range(count):
            c, ms, snip = post(model, tools, ua)
            codes.append(c)
            lats.append(ms)
            if c not in (200,) and snip:
                print("      %d: %s" % (c, snip[:120]))
            if i + 1 < count:
                time.sleep(gap)
        print("%-8s %-6d %s  (%.0f..%.0f)" % (
            gap, count, summarize(codes), min(lats), max(lats)))


def run_burst(model, tools, ua, widths):
    print("\n-- concurrent bursts --")
    print("%-8s %s" % ("width", "statuses"))
    for w in widths:
        with ThreadPoolExecutor(max_workers=w) as ex:
            res = list(ex.map(lambda _: post(model, tools, ua)[0], range(w)))
        print("%-8s %s" % (w, summarize(res)))
        time.sleep(5)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--model", default="opencode/glm-5.3-flash-free")
    ap.add_argument("--pilot", action="store_true")
    ap.add_argument("--levels", action="store_true")
    ap.add_argument("--burst", default="")
    ap.add_argument("--gaps", default="0,0.25,0.5,1.0,1.5,2.0")
    ap.add_argument("--count", type=int, default=6)
    a = ap.parse_args()

    tools = load_tools()
    ua = load_ua()
    print("tools loaded: %d (gate needs >=6 exact defs)" % len(tools))
    print("model: %s" % a.model)
    print("UA: %s" % ua)

    if a.pilot:
        c, ms, snip = post(a.model, tools, ua)
        print("pilot: status=%s %.0fms %s" % (c, ms, snip[:200]))
        if c != 200:
            print("gate rejected the fingerprint; fix before sweeping")
        return 0

    if a.levels or not a.burst:
        run_levels(a.model, tools, ua,
                   [float(g) for g in a.gaps.split(",")], a.count)
    if a.burst:
        run_burst(a.model, tools, ua, [int(x) for x in a.burst.split(",")])
    return 0


if __name__ == "__main__":
    sys.exit(main())
