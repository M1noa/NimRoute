#!/usr/bin/env python3
"""model_bench.py - capability benchmarks across every model nimroute serves.

four benchmarks, deterministic scoring, no LLM judge and no extra dependencies:

  gsm8k   grade-school math          numeric match on the final answer
  mmlu    knowledge, 4-way mc        letter match
  ifeval  instruction following      google's own instruction checkers
  cyber   CyberMetric, 4-way mc      letter match

usage:
  scripts/benchdata/.venv/bin/python scripts/model_bench.py --n 20 --concurrency 4
  scripts/benchdata/.venv/bin/python scripts/model_bench.py --limit 2 --n 2   # smoke

ifeval runs google's own checkers, which import nltk + immutabledict, so the
runner wants the venv: python3 -m venv scripts/benchdata/.venv &&
scripts/benchdata/.venv/bin/pip install immutabledict nltk &&
scripts/benchdata/.venv/bin/python -c "import nltk; nltk.download('punkt'); nltk.download('punkt_tab')"

datasets are fetched once into scripts/benchdata/ (gitignored) and cached.
results stream into <out>/results.jsonl as they land, so an interrupted run
resumes where it stopped: a (model, bench, item) already scored is not repeated.
"""
import argparse
import csv
import http.client
import json
import os
import random
import re
import sys
import threading
import time
import urllib.parse
import urllib.request
from concurrent.futures import ThreadPoolExecutor

HERE = os.path.dirname(os.path.abspath(__file__))
DATA = os.path.join(HERE, "benchdata")
SEED = 1337

BENCHES = ("gsm8k", "mmlu", "ifeval", "cyber")

# ---------------------------------------------------------------- datasets

GSM8K_URL = ("https://raw.githubusercontent.com/openai/grade-school-math/"
             "master/grade_school_math/data/test.jsonl")
MITRE_IFE = ("https://raw.githubusercontent.com/google-research/google-research/"
             "master/instruction_following_eval/")
HF_ROWS = "https://datasets-server.huggingface.co/rows"


def cache_path(name):
    os.makedirs(DATA, exist_ok=True)
    return os.path.join(DATA, name)


def download(url, name, binary=False):
    path = cache_path(name)
    if os.path.exists(path) and os.path.getsize(path) > 0:
        return path
    os.makedirs(os.path.dirname(path), exist_ok=True)
    req = urllib.request.Request(url, headers={"User-Agent": "nimroute-bench/1"})
    with urllib.request.urlopen(req, timeout=120) as r:
        body = r.read()
    with open(path, "wb" if binary else "w") as f:
        f.write(body if binary else body.decode("utf-8"))
    return path


def hf_rows(dataset, config, split, offsets, length):
    """rows from the datasets-server api, cached per request."""
    out = []
    for off in offsets:
        name = "hf_%s_%s_%s_%d_%d.json" % (
            dataset.replace("/", "_"), config, split, off, length)
        path = cache_path(name)
        if not (os.path.exists(path) and os.path.getsize(path) > 0):
            q = urllib.parse.urlencode({
                "dataset": dataset, "config": config,
                "split": split, "offset": off, "length": length})
            req = urllib.request.Request(HF_ROWS + "?" + q,
                                         headers={"User-Agent": "nimroute-bench/1"})
            with urllib.request.urlopen(req, timeout=120) as r:
                body = r.read()
            with open(path, "wb") as f:
                f.write(body)
        with open(path) as f:
            out.extend(row["row"] for row in json.load(f)["rows"])
    return out


def load_gsm8k(n, rnd):
    path = download(GSM8K_URL, "gsm8k_test.jsonl")
    items = []
    with open(path) as f:
        for line in f:
            d = json.loads(line)
            gold = d["answer"].split("####")[-1].strip().replace(",", "")
            items.append({"id": d["question"][:40], "prompt": d["question"], "gold": gold})
    rnd.shuffle(items)
    return items[:n]


def load_mmlu(n, rnd):
    total = 14042
    chunk = 5
    offsets = [int(total * i / 5) for i in range(5)]
    rows = hf_rows("cais/mmlu", "all", "test", offsets, chunk)
    items = []
    for r in rows:
        q = r["question"]
        opts = list(r["choices"])
        gold = "ABCD"[int(r["answer"])]
        body = "%s\n%s" % (q, "\n".join("%s. %s" % ("ABCD"[i], o) for i, o in enumerate(opts)))
        items.append({"id": r["subject"] + "/" + q[:32], "prompt": body, "gold": gold})
    rnd.shuffle(items)
    return items[:n]


def load_cyber(n, rnd):
    total = 9189
    chunk = 5
    offsets = [int(total * i / 5) for i in range(5)]
    rows = hf_rows("khangmacon/cybermetric-10000", "default", "train", offsets, chunk)
    items = []
    for r in rows:
        body = r["input"]
        ans = r["output"].strip()
        gold = ans[0].upper() if ans[:1].upper() in "ABCD" else ""
        items.append({"id": body[:40], "prompt": body, "gold": gold})
    rnd.shuffle(items)
    return items[:n]


def load_ifeval(n, rnd):
    # the reference modules import as a package (from instruction_following_eval
    # import ...), so they land in a dir of that name with an __init__.py.
    pkg = os.path.join(DATA, "instruction_following_eval")
    os.makedirs(pkg, exist_ok=True)
    init = os.path.join(pkg, "__init__.py")
    if not os.path.exists(init):
        open(init, "w").close()
    for f in ("instructions.py", "instructions_registry.py",
              "instructions_util.py", "evaluation_lib.py"):
        download(MITRE_IFE + f, os.path.join("instruction_following_eval", f))
    download(MITRE_IFE + "data/input_data.jsonl",
             os.path.join("instruction_following_eval", "input_data.jsonl"))
    path = cache_path(os.path.join("instruction_following_eval", "input_data.jsonl"))
    items = []
    with open(path) as f:
        for line in f:
            d = json.loads(line)
            items.append({"id": str(d["key"]), "prompt": d["prompt"],
                          "instruction_id_list": d["instruction_id_list"],
                          "kwargs": d["kwargs"]})
    rnd.shuffle(items)
    return items[:n]


# ---------------------------------------------------------------- scoring

NUM_RE = re.compile(r"-?\d[\d,]*\.?\d*")
LETTER_RE = re.compile(r"\b([A-D])\b")


def last_number(text):
    hits = NUM_RE.findall(text or "")
    if not hits:
        return None
    try:
        return float(hits[-1].replace(",", ""))
    except ValueError:
        return None


def last_letter(text):
    m = LETTER_RE.findall(text or "")
    return m[-1].upper() if m else ""


def score_gsm8k(text, item):
    got = last_number(text)
    if got is None:
        return 0
    try:
        want = float(item["gold"])
    except ValueError:
        return 0
    return 1 if abs(got - want) < 1e-4 else 0


def score_letter(text, item):
    return 1 if item["gold"] and last_letter(text) == item["gold"] else 0


_EVAL_LIB = None


def score_ifeval(text, item):
    global _EVAL_LIB
    if _EVAL_LIB is None:
        if DATA not in sys.path:
            sys.path.insert(0, DATA)
        try:
            from instruction_following_eval import evaluation_lib as el
        except Exception as e:  # nltk/immutabledict missing: no ifeval score
            print("ifeval scorer unavailable: %s" % e, file=sys.stderr)
            return None
        _EVAL_LIB = el
    evaluation_lib = _EVAL_LIB
    example = evaluation_lib.InputExample(
        key=item["id"], instruction_id_list=item["instruction_id_list"],
        prompt=item["prompt"], kwargs=item["kwargs"])
    resp = {str(item["id"]): text}
    out = evaluation_lib.test_instruction_following_loose(example, resp)
    return 1 if all(out.follow_instruction_list) else 0


SCORERS = {"gsm8k": score_gsm8k, "mmlu": score_letter,
           "ifeval": score_ifeval, "cyber": score_letter}


def build_prompt(bench, item):
    if bench == "ifeval":
        return item["prompt"]
    if bench == "gsm8k":
        return (item["prompt"] + "\n\nReason it through, then end with a line "
                "exactly of the form:\nFinal answer: <number>")
    return (item["prompt"] + "\n\nAnswer with the letter of the correct option, "
            "then finish with a line exactly of the form:\nAnswer: <letter>")


MAX_TOKENS = {"gsm8k": 700, "mmlu": 200, "ifeval": 900, "cyber": 200}


# ---------------------------------------------------------------- http

def http_post(base, payload, timeout):
    u = urllib.parse.urlparse(base)
    conn = http.client.HTTPConnection(u.hostname, u.port or 80, timeout=timeout)
    body = json.dumps(payload).encode()
    path = (u.path or "/").rstrip("/") + "/chat/completions"
    try:
        conn.request("POST", path, body, {
            "Content-Type": "application/json",
            "Content-Length": str(len(body)),
        })
        resp = conn.getresponse()
        raw = resp.read()
        return resp.status, raw
    finally:
        conn.close()


def ask(base, model, bench, item, timeout):
    payload = {
        "model": model,
        "messages": [{"role": "user", "content": build_prompt(bench, item)}],
        "temperature": 0,
        "max_tokens": MAX_TOKENS[bench],
        "stream": False,
    }
    t0 = time.time()
    last = ""
    # one attempt: the proxy already retries a request across its exits (up to
    # max_retries hops), so a client-side retry just doubles a wait that is
    # already bounded by --timeout. a model that cannot answer inside that
    # window is recorded unavailable, which is a result worth having.
    for attempt in range(1):
        try:
            status, raw = http_post(base, payload, timeout)
        except Exception as e:
            last = "transport: %s" % e
            continue
        if status == 200:
            try:
                d = json.loads(raw)
                text = d["choices"][0]["message"].get("content") or ""
                usage = d.get("usage") or {}
                return {"text": text, "ms": int((time.time() - t0) * 1000),
                        "pt": usage.get("prompt_tokens"), "ct": usage.get("completion_tokens")}
            except Exception as e:
                last = "decode: %s: %s" % (e, raw[:120])
        else:
            last = "http %d: %s" % (status, raw[:160].decode("utf-8", "replace"))
            if status not in (429, 500, 502, 503, 504, 529):
                break
            time.sleep(2 + attempt * 3)
    return {"error": last}


# ---------------------------------------------------------------- driver

def served_models(base):
    u = urllib.parse.urlparse(base)
    conn = http.client.HTTPConnection(u.hostname, u.port or 80, timeout=60)
    try:
        conn.request("GET", (u.path or "/").rstrip("/") + "/models")
        raw = conn.getresponse().read()
    finally:
        conn.close()
    return [m["id"] for m in json.loads(raw)["data"]]


def ordered(models, order):
    out = []
    for prefix in order:
        for m in sorted(models):
            if m.startswith(prefix + "/") and m not in out:
                out.append(m)
    return out


def load_done(path, retry_errors=False):
    done = set()
    if os.path.exists(path):
        with open(path) as f:
            for line in f:
                try:
                    r = json.loads(line)
                except ValueError:
                    continue
                if retry_errors and r.get("score") is None:
                    continue  # a 429/503 was availability, not an answer: retry
                done.add((r["model"], r["bench"], str(r["item"])))
    return done


def run_model(base, model, items, done, out_path, lock, timeout, benches, delay=0.0):
    for bench in benches:
        for item in items[bench]:
            key = (model, bench, str(item["id"]))
            if key in done:
                continue
            if delay:
                time.sleep(delay)  # free tiers meter per minute: pace the sample
            res = ask(base, model, bench, item, timeout)
            row = {"model": model, "bench": bench, "item": str(item["id"]),
                   "ts": int(time.time())}
            if "error" in res:
                row.update({"ok": 0, "score": None, "error": res["error"]})
            else:
                score = SCORERS[bench](res["text"], item)
                row.update({"ok": 1, "score": score, "ms": res.get("ms"),
                            "pt": res.get("pt"), "ct": res.get("ct"),
                            "gold": item.get("gold", ""),
                            "answer": (res["text"] or "")[-300:]})
            with lock:
                with open(out_path, "a") as f:
                    f.write(json.dumps(row) + "\n")
                done.add(key)


def summarize(rows, models, benches):
    agg = {}
    for r in rows:
        a = agg.setdefault((r["model"], r["bench"]), {"n": 0, "ok": 0, "err": 0, "ms": []})
        if r.get("score") is None:
            a["err"] += 1
        else:
            a["n"] += 1
            a["ok"] += r["score"]
        if r.get("ms"):
            a["ms"].append(r["ms"])
    lines = ["| model | " + " | ".join(benches) + " | errors | avg ms |",
             "|---|" + "---|" * len(benches) + "---|---|"]
    for m in models:
        cells, errs, mss = [], 0, []
        for b in benches:
            a = agg.get((m, b))
            if not a or a["n"] == 0:
                cells.append("-")
            else:
                cells.append("%d/%d" % (a["ok"], a["n"]))
            if a:
                errs += a["err"]
                mss.extend(a["ms"])
        avg = str(sum(mss) // len(mss)) if mss else "-"
        lines.append("| %s | %s | %d | %s |" % (m, " | ".join(cells), errs, avg))
    return "\n".join(lines)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="http://127.0.0.1:5419/v1")
    ap.add_argument("--out", default=os.path.join(HERE, "..", "bench-results"))
    ap.add_argument("--n", type=int, default=20, help="items per benchmark")
    ap.add_argument("--concurrency", type=int, default=12, help="models in flight")
    ap.add_argument("--timeout", type=int, default=90)
    ap.add_argument("--order", default="antseed,opencode,freeepi,uno,openrouter")
    ap.add_argument("--bench", default=",".join(BENCHES))
    ap.add_argument("--limit", type=int, default=0, help="cap model count (smoke)")
    ap.add_argument("--delay", type=float, default=0.0,
                    help="seconds to sleep between items (pace metered backends)")
    ap.add_argument("--retry-errors", action="store_true",
                    help="re-attempt items that failed on availability, not answers")
    args = ap.parse_args()

    benches = [b for b in args.bench.split(",") if b in BENCHES]
    order = [p for p in args.order.split(",") if p]

    rnd = random.Random(SEED)
    loaders = {"gsm8k": load_gsm8k, "mmlu": load_mmlu, "ifeval": load_ifeval, "cyber": load_cyber}
    items = {b: loaders[b](args.n, rnd) for b in benches}

    models = ordered(served_models(args.base), order)
    if args.limit:
        models = models[:args.limit]

    os.makedirs(args.out, exist_ok=True)
    out_jsonl = os.path.join(args.out, "results.jsonl")
    done = load_done(out_jsonl, args.retry_errors)
    lock = threading.Lock()

    print("models: %d  benches: %s  items/bench: %d  todo: %d" % (
        len(models), ",".join(benches), args.n,
        sum(1 for m in models for b in benches for i in items[b]
            if (m, b, str(i["id"])) not in done)), flush=True)

    t0 = time.time()
    with ThreadPoolExecutor(max_workers=max(1, args.concurrency)) as pool:
        futs = [pool.submit(run_model, args.base, m, items, done, out_jsonl,
                            lock, args.timeout, benches, args.delay) for m in models]
        for i, f in enumerate(futs, 1):
            f.result()
            print("  [%d/%d] %s  (%ds)" % (i, len(futs), models[i - 1],
                                           int(time.time() - t0)), flush=True)

    rows = [json.loads(l) for l in open(out_jsonl)]
    table = summarize(rows, models, benches)
    with open(os.path.join(args.out, "summary.md"), "w") as f:
        f.write(table + "\n")
    print(table)


if __name__ == "__main__":
    main()
