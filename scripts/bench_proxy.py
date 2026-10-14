#!/usr/bin/env python3
"""Bounded Claude-style load test; never targets the primary port by default."""
import argparse
import concurrent.futures
import json
import math
import os
import threading
import time
import urllib.error
import urllib.request


def percentile(values, p):
    values = sorted(values)
    return round(values[max(0, math.ceil(len(values) * p) - 1)], 2) if values else None


def run(args):
    stop = threading.Event()
    health = []

    def sample_health():
        while not stop.is_set():
            start = time.perf_counter()
            try:
                with urllib.request.urlopen(args.base + "/health", timeout=3) as r:
                    r.read()
                    health.append((time.perf_counter() - start) * 1000)
            except (OSError, ValueError):
                health.append(3000)
            stop.wait(1)

    def session(index):
        rows = []
        for turn in range(args.turns):
            body = {"model": args.model, "max_tokens": args.max_tokens, "stream": True,
                    "messages": [{"role": "user", "content": " " * args.padding +
                                  "Reply with exactly PONG. Do not call any tools."}],
                    "tools": [{"name": "Read", "description": "Read a file",
                               "input_schema": {"type": "object", "properties": {
                                   "path": {"type": "string"}}}}]}
            headers = {"Content-Type": "application/json",
                       "x-session-id": "bench-session-%d" % index}
            if os.environ.get("NIM_AUTH"):
                headers["Authorization"] = "Bearer " + os.environ["NIM_AUTH"]
            req = urllib.request.Request(args.base + "/v1/messages",
                                         data=json.dumps(body).encode(), headers=headers)
            start = time.perf_counter()
            row = {"session": index, "turn": turn, "status": 0, "ttft_ms": None,
                   "complete": False, "text": ""}
            try:
                with urllib.request.urlopen(req, timeout=args.timeout) as r:
                    row["status"] = r.status
                    for line in r:
                        if not line.startswith(b"data: ") or line.strip() == b"data: [DONE]":
                            continue
                        event = json.loads(line[6:])
                        delta = event.get("delta") or {}
                        text = delta.get("text", "")
                        if text or delta.get("partial_json") or delta.get("thinking"):
                            if row["ttft_ms"] is None:
                                row["ttft_ms"] = round((time.perf_counter() - start) * 1000, 2)
                        row["text"] += text
                        if event.get("type") == "message_stop":
                            row["complete"] = True
            except urllib.error.HTTPError as e:
                row["status"] = e.code
                row["error"] = e.read(1024).decode(errors="replace")
            except (OSError, ValueError) as e:
                row["error"] = str(e)
            row["total_ms"] = round((time.perf_counter() - start) * 1000, 2)
            row["correct"] = row["complete"] and row["text"].strip() == "PONG"
            rows.append(row)
            if args.pause:
                time.sleep(args.pause)
        return rows

    sampler = threading.Thread(target=sample_health)
    sampler.start()
    start = time.perf_counter()
    try:
        with concurrent.futures.ThreadPoolExecutor(max_workers=args.sessions) as pool:
            rows = [r for group in pool.map(session, range(args.sessions)) for r in group]
    finally:
        stop.set()
        sampler.join()
    ttft = [r["ttft_ms"] for r in rows if r["ttft_ms"] is not None]
    result = {"base": args.base, "model": args.model, "sessions": args.sessions,
              "turns": args.turns, "elapsed_s": round(time.perf_counter() - start, 2),
              "requests": len(rows), "correct": sum(r["correct"] for r in rows),
              "ttft_p50_ms": percentile(ttft, .5), "ttft_p95_ms": percentile(ttft, .95),
              "total_p95_ms": percentile([r["total_ms"] for r in rows], .95),
              "health_p95_ms": percentile(health, .95), "rows": rows}
    print(json.dumps(result, indent=2))
    return 0 if result["correct"] == len(rows) else 1


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base", default="http://127.0.0.1:5420")
    parser.add_argument("--model", default="opencode/step-5-preview-free")
    parser.add_argument("--sessions", type=int, default=1)
    parser.add_argument("--turns", type=int, default=1)
    parser.add_argument("--padding", type=int, default=0)
    parser.add_argument("--pause", type=float, default=0)
    parser.add_argument("--timeout", type=float, default=90)
    parser.add_argument("--max-tokens", type=int, default=512)
    args = parser.parse_args()
    if not 1 <= args.sessions <= 30 or not 1 <= args.turns <= 1000:
        parser.error("sessions must be 1..30 and turns 1..1000")
    raise SystemExit(run(args))
