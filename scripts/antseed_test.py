#!/usr/bin/env python3
"""antseed free-model + API smoke test.

follows ANTSEED-DOCS.md / ANTSEED-PLAN.md test protocol: talks to a running
buyer proxy (default http://127.0.0.1:8377), checks the catalog, the local
endpoints, the error surface, then runs one real chat completion against
every FREE text (LLM) model the network advertises.

usage: python3 scripts/antseed_test.py [base_url]
env:   ANTSEED_BASE_URL (default http://127.0.0.1:8377), ANTSEED_WORKERS (4),
       ANTSEED_TIMEOUT (90s per model), ANTSEED_MAX_MODELS (0 = all)
stdlib only.
"""

import json
import os
import sys
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor

BASE = os.environ.get("ANTSEED_BASE_URL", "http://127.0.0.1:8377").rstrip("/")
WORKERS = int(os.environ.get("ANTSEED_WORKERS", "4"))
TIMEOUT = float(os.environ.get("ANTSEED_TIMEOUT", "90"))
MAX_MODELS = int(os.environ.get("ANTSEED_MAX_MODELS", "0"))
TOP_TABLE = [  # ANTSEED-PLAN.md curated order, presence check only
    "glm-5.3-flash", "deepseek-v4-flash", "minimax-m3", "gpt-6-luna",
    "mimo-v2.6-flash",
]


def req(path, body=None, headers=None, method=None, raw=False, timeout=TIMEOUT):
    url = BASE + path
    data = None
    h = dict(headers or {})
    if body is not None:
        data = json.dumps(body).encode()
        h.setdefault("content-type", "application/json")
    r = urllib.request.Request(url, data=data, headers=h, method=method)
    try:
        with urllib.request.urlopen(r, timeout=timeout) as resp:
            payload = resp.read()
            return resp.status, (payload if raw else json.loads(payload)), dict(resp.headers)
    except urllib.error.HTTPError as e:
        payload = e.read()
        try:
            return e.code, json.loads(payload), dict(e.headers)
        except Exception:
            return e.code, payload[:500], dict(e.headers)
    except Exception as e:
        return 0, {"error": f"{type(e).__name__}: {e}"}, {}


def is_free(entry):
    for p in entry.get("peers") or []:
        if (p.get("inputUsdPerMillion") == 0 and p.get("outputUsdPerMillion") == 0
                and p.get("cachedInputUsdPerMillion") in (None, 0)):
            return True
    return False


def chat_once(model, stream=False):
    body = {
        "model": model,
        "messages": [{"role": "user", "content": "Reply with exactly: ok"}],
        "max_tokens": 512,
        "stream": stream,
    }
    t0 = time.time()
    if stream:
        status, raw, hdrs = req("/v1/chat/completions", body, raw=True)
        dt = time.time() - t0
        chunks = raw.count(b"\ndata:") if isinstance(raw, (bytes, bytearray)) else 0
        return status, f"sse chunks~{chunks}", dt, hdrs
    status, data, hdrs = req("/v1/chat/completions", body)
    dt = time.time() - t0
    if status != 200:
        return status, json.dumps(data)[:200], dt, hdrs
    try:
        content = data["choices"][0]["message"]["content"]
    except Exception:
        return status, f"bad shape: {json.dumps(data)[:200]}", dt, hdrs
    if not content:
        return status, "empty content", dt, hdrs
    return status, content[:60].replace("\n", " "), dt, hdrs


def main():
    fails = []
    print(f"== antseed smoke test vs {BASE} ==")

    # 1. catalog
    t0 = time.time()
    status, cat, _ = req("/v1/models", timeout=20)
    if status != 200:
        print(f"FAIL catalog: {status} {cat}")
        return 1
    data = cat.get("data") or []
    text = [e for e in data if e.get("type") == "text"]
    free = [e for e in text if is_free(e)]
    print(f"catalog: {len(data)} models ({len(text)} text, {len(free)} free text) "
          f"in {time.time()-t0:.1f}s")
    if not data:
        print("FAIL catalog empty")
        return 1

    # 2. top-table presence (informational — network may not carry all)
    ids = {e.get("id") for e in data}
    for m in TOP_TABLE:
        print(f"  top-table {m}: {'present' if m in ids else 'ABSENT on network'}")

    # 3. local endpoints (docs: answered locally, free, never routed)
    status, res, _ = req("/v1/messages/count_tokens", {
        "model": text[0]["id"], "messages": [{"role": "user", "content": "hello world"}]})
    ok_tokens = status == 200 and isinstance(res.get("input_tokens"), int)
    print(f"count_tokens: {'ok' if ok_tokens else f'FAIL {status} {res}'}")
    if not ok_tokens:
        fails.append("count_tokens")

    one = free[0]["id"] if free else text[0]["id"]
    status, res, _ = req(f"/v1/models/{one}")
    ok_single = status == 200 and res.get("id")
    print(f"models/{one}: {'ok' if ok_single else f'FAIL {status}'}")
    if not ok_single:
        fails.append("models/<id>")

    # 4. error surface (docs §2.5)
    status, res, _ = req("/v1/chat/completions",
                         {"model": "no-such-model-xyz", "messages": [{"role": "user", "content": "x"}]})
    ok_err = status in (400, 502)
    print(f"unknown model -> {status} (expect 502/400): {'ok' if ok_err else 'FAIL'}")
    if not ok_err:
        fails.append("error-surface")

    # 5. streaming on one model
    if free:
        status, detail, dt, _ = chat_once(free[0]["id"], stream=True)
        ok_stream = status == 200 and "chunks~0" not in detail
        print(f"streaming ({free[0]['id']}): {status} {detail} in {dt:.1f}s "
              f"{'ok' if ok_stream else 'FAIL'}")
        if not ok_stream:
            fails.append("streaming")

    # 6. one completion per free text model
    targets = free if MAX_MODELS <= 0 else free[:MAX_MODELS]
    print(f"\n== testing {len(targets)} free text models ({WORKERS} workers) ==")
    results = {}

    def run(entry):
        res = chat_once(entry["id"])
        if res[0] != 200 or res[1].startswith(("bad shape", "empty")):
            time.sleep(2)
            res = chat_once(entry["id"])   # retry once (upstream blips)
        return entry["id"], res

    with ThreadPoolExecutor(max_workers=WORKERS) as ex:
        for mid, (status, detail, dt, _) in ex.map(run, targets):
            results[mid] = (status, detail, dt)
            mark = "PASS" if status == 200 and not detail.startswith(("bad shape", "empty")) else "FAIL"
            print(f"  [{mark}] {dt:5.1f}s {status} {mid} :: {detail if mark=='FAIL' else ''}")

    passed = sum(1 for s, d, _ in results.values()
                 if s == 200 and not d.startswith(("bad shape", "empty")))
    print(f"\n== {passed}/{len(results)} free models passed ==")
    if results and passed == 0:
        fails.append("all-models")
    if fails:
        print(f"core failures: {fails}")
        return 1
    print("core api: ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
