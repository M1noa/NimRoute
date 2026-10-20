#!/usr/bin/env python3
"""find the minimal working zen request: compare api paths and header sets."""
import json, secrets, sys, time, urllib.error, urllib.request, itertools
from pathlib import Path

HERE = Path(__file__).parent
B62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
_seq = itertools.count(1)
BALLAST = json.load(open(HERE / "opencode_ballast.json"))
UA = "opencode/1.18.29 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14"


def sid():
    ms = int(time.time() * 1000); n = next(_seq)
    v = ~((ms << 12) + n)
    return "ses_" + (v & 0xFFFFFFFFFFFF).to_bytes(6, "big").hex() + "".join(B62[b % 62] for b in secrets.token_bytes(14))


def call(url, model, tools, client, extra, timeout=60, stream=False):
    body = {"model": model, "messages": [{"role": "user", "content": "say hi"}], "stream": stream}
    if tools:
        body["tools"] = tools
    h = {"Content-Type": "application/json", "Authorization": "Bearer public",
         "Accept": "*/*", "User-Agent": UA,
         "x-opencode-client": client,
         "x-opencode-project": "global",
         "x-opencode-session-id": sid(),
         "x-session-affinity": sid(),
         "x-opencode-session": sid(),
         "X-Session-Id": sid()}
    h.update(extra or {})
    req = urllib.request.Request(url, data=json.dumps(body).encode(), method="POST")
    for k, v in h.items():
        req.add_header(k, v)
    op = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    t0 = time.time()
    try:
        with op.open(req, timeout=timeout) as r:
            return r.status, time.time() - t0, r.read()[:200].decode("utf8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, time.time() - t0, e.read()[:200].decode("utf8", "replace")
    except Exception as e:
        return 0, time.time() - t0, f"{type(e).__name__}: {e}"


PATHS = ["https://opencode.ai/inference/openai/v1/chat/completions",
         "https://opencode.ai/inference/v1/chat/completions"]
MODELS = sys.argv[1].split(",") if len(sys.argv) > 1 else ["glm-5.3-flash-free"]

for url in PATHS:
    for model in MODELS:
        for label, tools, client in [("ballast+cli", BALLAST, "cli"),
                                     ("ballast+opencode", BALLAST, "opencode"),
                                     ("notools+opencode", None, "opencode")]:
            st, dt, sn = call(url, model, tools, client, None)
            tag = "OK  " if st == 200 else "FAIL"
            print(f"[{tag}] {st:>3} {dt:6.2f}s {url.split('/inference')[1][:28]:30s} {model:26s} {label}")
            if st != 200:
                print(f"       {sn[:150]!r}")
