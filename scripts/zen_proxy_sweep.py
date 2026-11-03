#!/usr/bin/env python3
"""Sweep every exit in the proxypool json against opencode zen and classify it.

Each proxy gets one request shaped like the real opencode CLI: the exact
11-tool set zen's free-tier gate fingerprints (read from zentools.go, since
scripts/opencode_ballast.json is the stale 12-tool set), the v1.19.3 header
set nimroute's setZenHeaders sends, a fresh descending session id and message
id, and a randomized multi-turn chat history with tool calls in it. models are
picked at random from the live free catalog, big-pickle excluded.

  scripts/zen_proxy_sweep.py --threads 40                # every proxy, shuffled
  scripts/zen_proxy_sweep.py --sample 600 --threads 40
  scripts/zen_proxy_sweep.py --country US,DE --limit 200
  scripts/zen_proxy_sweep.py --pilot                     # no proxy, one request

writes one tsv row per proxy (/tmp/zen_proxy_sweep.tsv by default) with the
status, class, latency and retry-after, the full response body and headers for
every exit that answered under /tmp/zen_proxy_bodies (--bodies to move it, an
empty string to skip), a list of the exits that served
(/tmp/zen_proxies_working.txt, ready for zen.proxies), and a summary: what
worked, what was rate limited, and what was the request shape's fault rather
than the exit's.
"""
import argparse
import csv
import json
import os
import random
import re
import subprocess
import sys
import tempfile
import time
import urllib.request
from collections import Counter, defaultdict
from concurrent.futures import ThreadPoolExecutor

ZEN = "https://opencode.ai/inference/v1"
LIST_URL = "https://raw.githubusercontent.com/M1noa/proxypool/refs/heads/output/proxies.json"
HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
B62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
MASK = 0xFFFFFFFFFFFF

# the gate fingerprints the whole tools array, so the captured opencode
# toolset comes straight out of zentools.go rather than a json copy that
# drifts (the ballast json in this dir is the stale shell/subagent set).
def load_tools():
    src = open(os.path.join(ROOT, "zentools.go")).read()
    m = re.search(r'const zenOCDefsJSON = ("(?:[^"\\]|\\.)*")', src)
    if not m:
        sys.exit("could not find zenOCDefsJSON in zentools.go")
    return json.loads(json.loads(m.group(1)))


def load_ua():
    src = open(os.path.join(ROOT, "zenproxy.go")).read()
    m = re.search(r'zenVersion\s*=\s*"([^"]+)"', src)
    # v1.19.3 sends a bare opencode/<tag>, no ai-sdk or runtime suffix.
    return "opencode/%s" % (m.group(1) if m else "1.19.3")


_seq = 0


def new_id(rng, prefix, descending=False):
    """opencode identifier.ts shape: 12 hex of ms<<12|counter, 14 base62."""
    global _seq
    _seq += 1
    value = (int(time.time() * 1000) << 12) + _seq
    if descending:
        value = ~value
    hexpart = (value & MASK).to_bytes(6, "big").hex()
    return prefix + "_" + hexpart + "".join(
        B62[rng.randrange(62)] for _ in range(14))


FILES = [
    "internal/router/lanes.go", "internal/router/rotate.go",
    "cmd/nimroute/main.go", "pkg/upstream/client.go",
    "internal/config/load.go", "api/handlers/chat.go",
    "scripts/deploy.sh", "internal/pool/refill_test.go",
]
FUNCS = ["refillPool", "rotateLane", "handleChat", "applyHeaders",
         "loadConfig", "pickExit", "decodeStream", "retryOn429"]
USERS = [
    "why does {fn} in {file} stop retrying after the first 429?",
    "add a 5s timeout to the upstream fetch in {file}",
    "this loop in {file} leaks the response body, can you check it",
    "rename {fn} to something clearer and update the call sites",
    "write a test for {fn} that covers an empty pool",
    "what does the error in {file} mean when the proxy dials but tls fails",
    "make {fn} skip exits marked dead instead of retrying them",
    "there is a data race in {file}, find it before we ship",
    "show me the diff between the two retry paths in {file}",
    "can you trim the log line in {fn}, it prints the proxy user:pass",
]
ASSISTANTS = [
    "let me look at {file} first.",
    "that loop starts a fresh client per attempt, which is why it never reuses the connection.",
    "the body is closed only on the success path, so every error branch leaks.",
    "i'll check the call sites under cmd/ before renaming.",
    "the 429 is per exit and model, so the fix is to rotate rather than widen the budget.",
    "here is the smallest change that keeps the old behavior for non-retryable statuses.",
    "that error means the tunnel was established and the handshake failed, so the exit is dead.",
    "the goroutine writes the map while the request path reads it, no mutex in between.",
    "i trimmed the log line and left the rest of the block alone.",
    "that path is guarded by the caller, so the check inside is dead code.",
]
TOOL_CALLS = [
    ("read", {"filePath": "/Users/dev/atlas/{file}"}),
    ("grep", {"pattern": "retry|429", "path": "internal/"}),
    ("glob", {"pattern": "**/*_test.go"}),
    ("bash", {"command": "go vet ./...", "timeout": 120000}),
]


def build_messages(rng):
    """a plausible opencode session: environment prompt, a few turns, a tool
    call or two, then the question actually being asked."""
    project = rng.choice(["atlas", "relay", "zenpool", "edge-proxy", "gateway"])
    msgs = [{
        "role": "system",
        "content": (
            "You are opencode, an interactive CLI tool that helps users with "
            "software engineering tasks. Use the instructions below and the "
            "tools available to you to assist the user.\n\n"
            "# Environment\n"
            "Working directory: /Users/%s/%s\n"
            "Platform: darwin\n"
            "Today's date: 2026-10-25\n"
            "Shell: zsh\n\n"
            "# Style\n"
            "- Be concise. Answer in a few sentences unless asked for detail.\n"
            "- Prefer editing existing files. Never write new files unless asked.\n"
            "- Use the tools to read a file before changing it.\n"
        ) % (rng.choice(["dev", "m1noa", "sasha", "kim"]), project),
    }]
    for _ in range(rng.randrange(1, 5)):
        who = rng.random()
        f = rng.choice(FILES)
        if who < 0.5:
            msgs.append({"role": "user", "content": rng.choice(USERS).format(
                fn=rng.choice(FUNCS), file=f)})
        elif who < 0.85:
            msgs.append({"role": "assistant", "content": rng.choice(ASSISTANTS).format(
                fn=rng.choice(FUNCS), file=f)})
        else:
            name, args = rng.choice(TOOL_CALLS)
            args = json.loads(json.dumps(args).replace("{file}", f))
            msgs.append({"role": "assistant", "content": "", "tool_calls": [{
                "id": "call_" + "".join(rng.choice(B62) for _ in range(24)),
                "type": "function",
                "function": {"name": name, "arguments": json.dumps(args)},
            }]})
            msgs.append({
                "role": "tool",
                "tool_call_id": msgs[-1]["tool_calls"][0]["id"],
                "content": "ok" if name == "bash" else "1 result",
            })
    msgs.append({"role": "user", "content": rng.choice(USERS).format(
        fn=rng.choice(FUNCS), file=rng.choice(FILES))})
    return msgs


def build_body(rng, model, tools):
    body = {
        "model": model,
        "messages": build_messages(rng),
        "tools": tools,
        "stream": True,
        "max_tokens": rng.choice([24, 32, 48, 64, 96]),
        "temperature": round(rng.uniform(0.2, 1.0), 2),
    }
    if rng.random() < 0.5:
        body["top_p"] = round(rng.uniform(0.8, 1.0), 2)
    if rng.random() < 0.5:
        body["stream_options"] = {"include_usage": True}
    return body


def classify(status, body):
    low = body.lower()
    if status == 200:
        if '"choices"' in body or "data:" in body:
            return "ok"
        return "bad-200"
    if "not available in your country" in low:
        return "geo-blocked"
    if "[user_blocked]" in low:
        return "ip-banned"
    if "freetier" in low:
        # the gate refuses the request shape, not the exit: same 403 from every
        # proxy, so it says nothing about this exit's budget.
        if "within opencode" in low or "within open code" in low:
            return "shape-refused"
        if "current network" in low or "abusive" in low or "flagged" in low:
            return "ip-capped"
        return "freetier-other"
    if status in (429, 529):
        return "rate-limited"
    if status == 407:
        return "proxy-auth"
    if status == 400:
        return "bad-request"
    if status in (502, 503, 504):
        return "proxy-error"
    return "other"


def safe_name(proxy):
    return re.sub(r"[^A-Za-z0-9.]", "_", proxy)


def probe(proxy, model, tools, ua, rng, max_time, bodies=""):
    body = json.dumps(build_body(rng, model, tools)).encode()
    sess = new_id(rng, "ses", descending=True)
    req_id = new_id(rng, "msg")
    cmd = [
        "curl", "-sS", "--max-time", str(max_time), "--connect-timeout", "10",
        "-o", "-", "-w", "\n%{http_code} %{time_total}",
        "-X", "POST", ZEN + "/chat/completions",
    ]
    if proxy:
        cmd += ["--proxy", proxy]
    # headers always land in a temp file so retry-after is recorded even when
    # full bodies are not kept.
    fd, hpath = tempfile.mkstemp(prefix="zenhdr")
    os.close(fd)
    cmd += ["-D", hpath]
    cmd += [
        "-H", "Content-Type: application/json",
        "-H", "Accept: */*",
        "-H", "Authorization: Bearer public",
        "-H", "User-Agent: " + ua,
        "-H", "x-opencode-client: cli",
        "-H", "x-opencode-project: global",
        "-H", "x-opencode-session: " + sess,
        "-H", "x-opencode-session-id: " + sess,
        "-H", "x-opencode-request: " + req_id,
        "--data-binary", "@-",
    ]
    t0 = time.time()
    try:
        p = subprocess.run(cmd, input=body, capture_output=True, timeout=max_time + 5)
    except subprocess.TimeoutExpired:
        os.unlink(hpath)
        return (proxy, model, 0, "timeout", int((time.time() - t0) * 1000), "", "", t0)
    except Exception as e:  # noqa: BLE001 - missing curl, dead fork, etc
        os.unlink(hpath)
        return (proxy, model, 0, "net-error", 0, str(e)[:140], "", t0)
    if p.returncode != 0:
        os.unlink(hpath)
        err = p.stderr.decode("utf-8", "replace").strip().splitlines()
        return (proxy, model, 0, "net-error", int((time.time() - t0) * 1000),
                (err[-1] if err else "curl rc=%d" % p.returncode)[:140], "", t0)
    out = p.stdout.decode("utf-8", "replace")
    *lines, tail = out.rsplit("\n", 1)
    try:
        status_s, secs = tail.split()
        status = int(status_s)
        ms = int(float(secs) * 1000)
    except ValueError:
        os.unlink(hpath)
        return (proxy, model, 0, "net-error", 0, "bad curl status line", "", t0)
    text = "\n".join(lines)
    retry_after = ""
    try:
        hdrs = open(hpath, errors="replace").read()
        m = re.search(r"(?im)^retry-after:\s*(\S+)", hdrs)
        retry_after = m.group(1) if m else ""
        if bodies and status > 0:
            os.makedirs(bodies, exist_ok=True)
            stem = os.path.join(bodies, "%s-%d" % (safe_name(proxy), status))
            with open(stem + ".headers", "w") as f:
                f.write(hdrs)
            with open(stem + ".body", "w") as f:
                f.write(text)
    finally:
        os.unlink(hpath)
    return (proxy, model, status, classify(status, text),
            ms, " ".join(text.split())[:140], retry_after, t0 + ms / 1000.0)


def proxy_url(e):
    """scheme + host:port for every exit that may tunnel tls.

    the list's `https` flag is not trustworthy: 18 of the 107 exits in
    .zenpool.json (verified by an actual completion) carry https:false, so
    filtering on it drops working proxies. an http proxy that cannot CONNECT
    fails the dial on its own.
    """
    ip = e.get("ip") or ""
    port = e.get("port")
    if not ip or not port:
        return None
    host = "[%s]" % ip if ":" in ip else ip
    protos = e.get("protocols") or []
    if "http" in protos:
        return "http://%s:%s" % (host, port)
    if "socks5" in protos:
        return "socks5h://%s:%s" % (host, port)
    if "socks4" in protos:
        return "socks4a://%s:%s" % (host, port)
    return None


def free_models(ua, want):
    try:
        req = urllib.request.Request(ZEN + "/models", headers={"User-Agent": ua})
        with urllib.request.urlopen(req, timeout=30) as r:
            data = json.load(r)
        ids = [m["id"] for m in data.get("data", [])]
    except Exception as e:  # noqa: BLE001 - offline, fall back to known ids
        print("model list fetch failed (%s); using built-in free ids" % e,
              file=sys.stderr)
        ids = ["beam-free", "deepseek-v4.1-flash-free", "glm-5.3-flash-free",
               "jev-1.13-free", "mimo-v2.6-flash-free",
               "nemotron-3.5-lightning-free", "nemotron-3.5-super-vl-free"]
    keep = [i for i in ids
            if i.endswith("-free") and "pickle" not in i
            and not i.startswith("muse-spark")]
    if want:
        keep = [i for i in keep if i in want]
    if not keep:
        sys.exit("no free chat models matched")
    return keep


def pilot(models, tools, ua, rng, max_time, bodies=""):
    """one direct request per model: proves the shape is accepted before any
    proxy verdict is trusted. 429 still proves the gate passed."""
    print("-- direct pilot (no proxy) --")
    for m in models:
        _, _, status, result, ms, snip, ra, _ = probe(None, m, tools, ua, rng,
                                                     max_time, bodies)
        note = ""
        if result == "rate-limited":
            note = "  (shape accepted, this ip is capped%s)" % (
                ", retry-after %ss" % ra if ra else "")
        elif result == "shape-refused":
            note = "  (this model refuses the shape for everyone)"
        print("  %-32s %s %-15s %5dms %s%s" % (m, status, result, ms, snip[:70], note))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--list-url", default=LIST_URL)
    ap.add_argument("--models", default="", help="comma separated subset")
    ap.add_argument("--country", default="", help="comma separated iso codes")
    ap.add_argument("--sample", type=int, default=0, help="random exits to test")
    ap.add_argument("--limit", type=int, default=0, help="hard cap after shuffle")
    ap.add_argument("--threads", type=int, default=40)
    ap.add_argument("--max-time", type=int, default=30)
    ap.add_argument("--out", default="/tmp/zen_proxy_sweep.tsv")
    ap.add_argument("--working-out", default="/tmp/zen_proxies_working.txt")
    ap.add_argument("--bodies", default="/tmp/zen_proxy_bodies",
                    help="dir for full response bodies + headers (empty to skip)")
    ap.add_argument("--seed", type=int, default=0)
    ap.add_argument("--pilot", action="store_true", help="direct requests only")
    a = ap.parse_args()

    tools = load_tools()
    ua = load_ua()
    rng = random.Random(a.seed or int.from_bytes(os.urandom(8), "big"))
    want = [m.strip() for m in a.models.split(",") if m.strip()]
    models = free_models(ua, want)
    print("tools: %d (gate needs the exact opencode set)  UA: %s" % (len(tools), ua))
    print("models: %s" % ", ".join(models))

    if a.pilot:
        pilot(models, tools, ua, rng, a.max_time, a.bodies)
        return 0

    req = urllib.request.Request(a.list_url, headers={"User-Agent": ua})
    with urllib.request.urlopen(req, timeout=60) as r:
        entries = json.load(r)
    countries = {c.strip().upper() for c in a.country.split(",") if c.strip()}

    # one exit per egress ip: the list publishes many ports per box and the
    # verdict is per ip, not per port.
    seen_ip = set()
    targets = []
    for e in entries:
        ip = e.get("ip") or ""
        if not ip or ip in seen_ip:
            continue
        if countries and (e.get("country") or "").upper() not in countries:
            continue
        url = proxy_url(e)
        if not url:
            continue
        seen_ip.add(ip)
        targets.append((url, (e.get("country") or "??").upper()))
    rng.shuffle(targets)
    if a.sample:
        targets = targets[:a.sample]
    if a.limit:
        targets = targets[:a.limit]
    if not targets:
        print("no proxies matched the filters", file=sys.stderr)
        return 1
    print("%d exits to probe, %d threads" % (len(targets), a.threads))

    jobs = [(url, rng.choice(models)) for url, _ in targets]
    rows = []
    done = 0
    t0 = time.time()
    with ThreadPoolExecutor(max_workers=a.threads) as ex:
        for row in ex.map(
                lambda j: probe(j[0], j[1], tools, ua, rng, a.max_time, a.bodies),
                jobs):
            rows.append(row)
            done += 1
            if done % 100 == 0 or done == len(rows):
                tally = Counter(r[3] for r in rows)
                print("  %d/%d  ok=%d 429=%d net=%d (%.0fs)" % (
                    done, len(rows), tally["ok"], tally["rate-limited"],
                    tally["net-error"] + tally["timeout"], time.time() - t0),
                    flush=True)

    with open(a.out, "w", newline="") as f:
        w = csv.writer(f, delimiter="\t")
        w.writerow(["proxy", "model", "status", "result", "ms", "snippet",
                    "retry_after", "ts"])
        w.writerows(rows)
    print("\nwrote %s" % a.out)
    if a.bodies:
        print("bodies + headers under %s" % a.bodies)

    ok = [r for r in rows if r[3] == "ok"]
    ok.sort(key=lambda r: r[4])
    with open(a.working_out, "w") as f:
        for r in ok:
            f.write(r[0] + "\n")
    if ok:
        print("wrote %s (%d exits served)" % (a.working_out, len(ok)))

    tally = Counter(r[3] for r in rows)
    print("\n-- results (%d exits) --" % len(rows))
    for result, n in tally.most_common():
        print("  %-16s %5d  %5.1f%%" % (result, n, 100.0 * n / len(rows)))
    statuses = Counter(r[2] for r in rows)
    print("  statuses: " + " ".join(
        "%s=%d" % (k, statuses[k]) for k in sorted(statuses)))
    capped = [r for r in rows if r[3] == "rate-limited"]
    if capped:
        ras = Counter(r[6] or "-" for r in capped)
        print("  retry-after (s): " + "  ".join(
            "%s x%d" % (k, v) for k, v in ras.most_common(8)))
        # anchor each countdown on its own response time: a full sweep runs for
        # minutes, so raw retry-after values spread even when every exit points
        # at the same reset instant.
        ends = [r[7] + int(r[6]) for r in capped if r[6].isdigit()]
        if ends and max(ends) - min(ends) <= 120:
            print("  all capped exits lift at %s utc" % time.strftime(
                "%Y-%m-%dT%H:%M:%SZ", time.gmtime(sum(ends) / len(ends))))
    dialable = len(rows) - tally["net-error"] - tally["timeout"]
    if dialable and tally["ok"] == 0 and tally["rate-limited"] == dialable:
        # the shape was accepted (429, not 403) on every exit that dialed, so
        # the cap is not one proxy and not our request: the free tier is spent
        # across the whole exit space, which is what a 95 "verified" pool that
        # still 503s on every attempt looks like from inside nimroute.
        print("\n  every dialable exit answered 429: the shape was accepted, so this\n"
              "  is the free tier being spent, not one bad proxy. re-run later.")

    if ok:
        print("\n-- serving exits (fastest first) --")
        for r in ok[:15]:
            print("  %-32s %-30s %5dms" % (r[0], r[1], r[4]))

    # a model that always answers shape-refused is the model's fault, not the
    # exit's, so surface the per-model split instead of hiding it in "403".
    by_model = defaultdict(Counter)
    for r in rows:
        by_model[r[1]][r[3]] += 1
    if len(by_model) > 1:
        print("\n-- per model --")
        for m, c in sorted(by_model.items()):
            print("  %-32s ok=%-4d 429=%-4d shape=%-4d capped=%-4d net=%d" % (
                m, c["ok"], c["rate-limited"], c["shape-refused"],
                c["ip-capped"], c["net-error"] + c["timeout"]))

    country_of = {url: c for url, c in targets}
    by_country = defaultdict(Counter)
    for r in rows:
        by_country[country_of.get(r[0], "??")][r[3]] += 1
    print("\n-- per country (top 20 by serving exits) --")
    print("  %-8s %5s %5s %5s %5s %6s" % ("cc", "n", "ok", "429", "shape", "net"))
    for c, counts in sorted(by_country.items(),
                            key=lambda kv: -kv[1]["ok"])[:20]:
        n = sum(counts.values())
        print("  %-8s %5d %5d %5d %5d %6d" % (
            c, n, counts["ok"], counts["rate-limited"], counts["shape-refused"],
            counts["net-error"] + counts["timeout"]))
    return 0


if __name__ == "__main__":
    sys.exit(main())
