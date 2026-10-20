#!/usr/bin/env python3
"""ablate zen request shape piece by piece until it 403s.

tests, per model, over a matrix of (system prompt, tools, headers, params)
to find the minimal working fingerprint. direct connection by default;
--proxy URL to route through an exit.

usage:
  python3 zen_ablate.py --models glm-5.3-flash-free,deepseek-v4.1-flash-free
  python3 zen_ablate.py --models big-pickle --proxy http://1.2.3.4:8080
"""
import argparse
import itertools
import json
import secrets
import socket
import ssl
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

HERE = Path(__file__).parent
ZEN = "https://opencode.ai/inference/v1/chat/completions"
B62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
_seq = itertools.count(1)


def new_id(prefix, descending=True):
    ms = int(time.time() * 1000)
    n = next(_seq)
    value = (ms << 12) + n
    if descending:
        value = ~value
    hexpart = (value & 0xFFFFFFFFFFFF).to_bytes(6, "big").hex()
    return f"{prefix}_{hexpart}{''.join(B62[b % 62] for b in secrets.token_bytes(14))}"


def load_ballast():
    with open(HERE / "opencode_ballast.json") as f:
        return json.load(f)


def base_headers(session, ua, variant="full"):
    h = {"Content-Type": "application/json"}
    if variant in ("full", "no-ua"):
        h["Authorization"] = "Bearer public"
    if variant in ("full",):
        h["Accept"] = "*/*"
    if variant in ("full", "no-acc"):
        h["User-Agent"] = ua
    if variant == "full":
        h["x-opencode-client"] = "cli"
        h["x-opencode-session"] = session
        h["x-opencode-session-id"] = session
        h["x-session-affinity"] = session
        h["x-opencode-project"] = "global"
    return h


def post(url, body, headers, proxy=None, timeout=90):
    data = json.dumps(body).encode()
    req = urllib.request.Request(url, data=data, method="POST")
    for k, v in headers.items():
        req.add_header(k, v)
    if proxy:
        opener = urllib.request.build_opener(
            urllib.request.ProxyHandler({"http": proxy, "https": proxy})
        )
    else:
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    t0 = time.time()
    try:
        with opener.open(req, timeout=timeout) as r:
            raw = r.read()
            return r.status, time.time() - t0, raw[:400].decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, time.time() - t0, e.read()[:400].decode("utf-8", "replace")
    except Exception as e:
        return 0, time.time() - t0, f"{type(e).__name__}: {e}"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--models", default="glm-5.3-flash-free")
    ap.add_argument("--proxy")
    ap.add_argument("--ua", default="opencode/1.19.3 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14")
    ap.add_argument("--prompt", default="say hi")
    ap.add_argument("--timeout", type=int, default=90)
    ap.add_argument("--full-matrix", action="store_true")
    args = ap.parse_args()

    ballast = load_ballast()
    prompt_file = HERE.parent / "system-prompts/leaked-system-prompts/opencode_20260902.md"
    system = None
    if prompt_file.exists():
        txt = prompt_file.read_text()
        parts = txt.split("## System Prompt", 1)
        system = (parts[1] if len(parts) > 1 else txt).strip()

    user = {"role": "user", "content": args.prompt}

    def msgs(with_sys):
        return ([{"role": "system", "content": system}] if with_sys and system else []) + [user]

    # ablation ladder: full -> strip one thing at a time
    cases = [
        ("full: sys+tools+hdrs", lambda: (
            {"model": "M", "messages": msgs(True), "stream": False, "tools": ballast},
            base_headers(new_id("ses"), args.ua, "full"))),
        ("no system prompt", lambda: (
            {"model": "M", "messages": msgs(False), "stream": False, "tools": ballast},
            base_headers(new_id("ses"), args.ua, "full"))),
        ("no tools", lambda: (
            {"model": "M", "messages": msgs(False), "stream": False},
            base_headers(new_id("ses"), args.ua, "full"))),
        ("tools, no x-opencode-* hdrs", lambda: (
            {"model": "M", "messages": msgs(False), "stream": False, "tools": ballast},
            base_headers(new_id("ses"), args.ua, "min"))),
        ("bare: auth+ct only", lambda: (
            {"model": "M", "messages": msgs(False), "stream": False},
            {"Content-Type": "application/json", "Authorization": "Bearer public"})),
        ("bare, no auth", lambda: (
            {"model": "M", "messages": msgs(False), "stream": False},
            {"Content-Type": "application/json"})),
        ("half tools (6)", lambda: (
            {"model": "M", "messages": msgs(False), "stream": False, "tools": ballast[:6]},
            base_headers(new_id("ses"), args.ua, "full"))),
        ("sys+tools, no stream field", lambda: (
            {"model": "M", "messages": msgs(True), "tools": ballast},
            base_headers(new_id("ses"), args.ua, "full"))),
    ]

    for model in args.models.split(","):
        model = model.strip()
        print(f"\n=== {model} (proxy={args.proxy or 'direct'}) ===")
        for name, build in cases:
            body, hdrs = build()
            body["model"] = model
            st, dt, snippet = post(ZEN, body, hdrs, args.proxy, args.timeout)
            mark = "OK " if st == 200 else "FAIL"
            print(f"  [{mark}] {st:>3} {dt:6.2f}s  {name}")
            if st != 200:
                print(f"         {snippet[:200]!r}")


if __name__ == "__main__":
    sys.exit(main())
