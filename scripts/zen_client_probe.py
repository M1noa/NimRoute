#!/usr/bin/env python3
"""tells a refused client apart from an empty free tier.

every probe against zen answers 429 FreeUsageLimitError with retry-after
pointing at the next utc midnight, so a 429 on its own proves nothing: it is
what a spent budget, a blocked client and a rate-limited ip all look like.
what answers the question is a control in the same round. the real opencode
cli goes straight to the zen provider, so when the cli serves (200) and a
candidate probe is still 429 a second later, the candidate was refused and
the message was a decoy.

    python3 scripts/zen_client_probe.py                  # cli control + curl
    python3 scripts/zen_client_probe.py --rounds 5
    python3 scripts/zen_client_probe.py --candidates curl,bun,curl-cli

one json line per probe in /tmp/zen_client_probe.jsonl.
"""
import argparse
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time

ZEN = "https://opencode.ai/inference/v1/chat/completions"
MODEL = "deepseek-v4.1-flash-free"
UA = "opencode/1.18.29 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14"
LOG = "/tmp/zen_client_probe.jsonl"

SMALL = {
    "model": MODEL,
    "max_tokens": 64,
    "messages": [{"role": "user", "content": "reply exactly: PONG"}],
    "stream": True,
}


def cli_config(dirpath):
    """a throwaway opencode config whose only provider is zen, so the control
    goes to the same endpoint the candidates do instead of a local proxy."""
    cfg = {
        "$schema": "https://opencode.ai/config.json",
        "provider": {
            "zen": {
                "npm": "@ai-sdk/openai-compatible",
                "name": "Zen",
                "options": {"baseURL": "https://opencode.ai/inference/v1",
                            "apiKey": "public"},
                "models": {MODEL: {"name": MODEL}},
            }
        },
    }
    path = os.path.join(dirpath, "opencode.json")
    with open(path, "w") as f:
        json.dump(cfg, f, indent=2)
    return path


def status_of(text):
    for tok in text.replace("|", " ").split():
        if tok.isdigit() and len(tok) == 3:
            return int(tok)
    return 0


def control(cfg, timeout=180):
    """the real client. served means the free tier had room this second."""
    env = dict(os.environ, OPENCODE_CONFIG=cfg, XDG_CONFIG_HOME=os.path.dirname(cfg))
    t0 = time.time()
    try:
        p = subprocess.run(["opencode", "run", "-m", "zen/" + MODEL,
                            "reply exactly: PONG"],
                           capture_output=True, timeout=timeout, env=env)
    except subprocess.TimeoutExpired:
        return {"probe": "cli-control", "status": 0, "class": "timeout",
                "ms": int((time.time() - t0) * 1000)}
    out = p.stdout.decode("utf-8", "replace")
    ok = p.returncode == 0 and "PONG" in out
    return {"probe": "cli-control", "status": 200 if ok else 0,
            "class": "ok" if ok else "failed",
            "ms": int((time.time() - t0) * 1000),
            "stderr": p.stderr.decode("utf-8", "replace")[-160:]}


def curl_body(body_path, headers, extra=()):
    cmd = ["curl", "-sS", "--max-time", "45", "-o", "/dev/null",
           "-w", "%{http_code}", "-X", "POST", ZEN] + list(extra)
    for k, v in headers.items():
        cmd += ["-H", "%s: %s" % (k, v)]
    cmd += ["--data-binary", "@" + body_path]
    return cmd


def run_cmd(cmd, timeout=120):
    t0 = time.time()
    try:
        p = subprocess.run(cmd, capture_output=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        return 0, int((time.time() - t0) * 1000)
    return status_of((p.stdout + p.stderr).decode("utf-8", "replace")), int((time.time() - t0) * 1000)


def candidate(name, body_path):
    if name == "curl":
        st, ms = run_cmd(curl_body(body_path, {
            "Authorization": "Bearer public",
            "Content-Type": "application/json",
            "User-Agent": UA,
            "Accept": "*/*",
        }))
    elif name == "curl-cli":
        # the header set the real client sends, still curl underneath.
        sid = "ses_" + "0" * 12 + "abcdefghijklmn"
        st, ms = run_cmd(curl_body(body_path, {
            "Accept": "*/*",
            "Accept-Encoding": "gzip, deflate, br, zstd",
            "Authorization": "Bearer public",
            "Connection": "keep-alive",
            "Content-Type": "application/json",
            "User-Agent": UA,
            "X-Session-Affinity": sid,
            "X-Session-Id": sid,
        }))
    elif name == "bun":
        script = os.path.join(os.path.dirname(body_path), "bun_probe.mjs")
        with open(script, "w") as f:
            f.write(
                'import fx from "fs";\n'
                "const body = fx.readFileSync(%s, \"utf8\");\n"
                "const r = await fetch(%s, { method: \"POST\", body,\n"
                "  headers: { \"Authorization\": \"Bearer public\",\n"
                "    \"Content-Type\": \"application/json\",\n"
                "    \"User-Agent\": %s, \"Accept\": \"*/*\" } });\n"
                "console.log(r.status);\n" % (json.dumps(body_path), json.dumps(ZEN), json.dumps(UA)))
        st, ms = run_cmd(["bun", script])
    else:
        raise SystemExit("unknown candidate %r" % name)
    return st, ms


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--rounds", type=int, default=3)
    ap.add_argument("--candidates", default="curl,curl-cli,bun")
    ap.add_argument("--model", default=MODEL)
    ap.add_argument("--log", default=LOG)
    a = ap.parse_args()

    if not shutil.which("opencode"):
        sys.exit("opencode cli not found: it is the only client known to be served")
    names = [c.strip() for c in a.candidates.split(",") if c.strip()]

    tmp = tempfile.mkdtemp(prefix="zenprobe")
    cfg = cli_config(tmp)
    body_path = os.path.join(tmp, "body.json")
    with open(body_path, "w") as f:
        json.dump(SMALL, f)

    log = open(a.log, "a", buffering=1)
    served = {n: 0 for n in names}
    rounds_with_control = 0
    print("control: opencode cli -> zen   candidates: %s\n" % ", ".join(names))
    for i in range(a.rounds):
        c = control(cfg)
        log.write(json.dumps(c, sort_keys=True) + "\n")
        if c["status"] != 200:
            print("round %-2d control refused (%s) -> tier has no room, candidates "
                  "say nothing" % (i + 1, c["class"]), flush=True)
            time.sleep(8)
            continue
        rounds_with_control += 1
        line = ["round %-2d control=200" % (i + 1)]
        for n in names:
            st, ms = candidate(n, body_path)
            rec = {"probe": n, "status": st, "ms": ms,
                   "class": "ok" if st == 200 else "refused",
                   "control": 200}
            log.write(json.dumps(rec, sort_keys=True) + "\n")
            if st == 200:
                served[n] += 1
            line.append("%s=%d" % (n, st))
        print("  ".join(line), flush=True)
        time.sleep(2)

    print("\n-- verdict --")
    if not rounds_with_control:
        print("  the cli never served in %d rounds: nothing to compare, rerun later."
              % a.rounds)
    else:
        for n in names:
            if served[n]:
                print("  %-9s served %d/%d rounds" % (n, served[n], rounds_with_control))
            else:
                print("  %-9s refused in all %d rounds where the cli served"
                      % (n, rounds_with_control))
        print("\n  a candidate refused while the cli served is not a spent budget:\n"
              "  the 429 it gets is a decoy for something else in the request.")
    print("\nlog: %s" % a.log)
    return 0


if __name__ == "__main__":
    sys.exit(main())
