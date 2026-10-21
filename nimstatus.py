#!/usr/bin/env python3
"""nimstatus: rich renderer for ./nim status/log commands. stdlib fallback."""
import datetime
import json
import os
import re
import sys
import urllib.request

PORT = os.environ.get("PORT", "5419")
BASE = "http://localhost:" + PORT
ROOT = os.path.dirname(os.path.abspath(__file__))


def fetch(path):
    req = urllib.request.Request(BASE + path)
    if os.environ.get("NIM_AUTH"):
        req.add_header("Authorization", "Bearer " + os.environ["NIM_AUTH"])
    return json.loads(urllib.request.urlopen(req, timeout=3).read())


try:
    from rich.console import Console, Group
    from rich.live import Live
    from rich.table import Table
    from rich import box
    from rich.text import Text

    RICH = True
except ImportError:
    RICH = False


def today_str():
    return datetime.date.today().isoformat()


_jsonl_cache = {"mtime": 0, "size": 0, "out": None}


def jsonl_stats():
    """All-time + today totals from nim-usage.jsonl. Returns dict.

    append-only reads keep polling cheap while traffic grows the log.
    rotation, truncation, and midnight reset the cached totals.
    """
    path = os.path.join(ROOT, "nim-usage.jsonl")
    try:
        st = os.stat(path)
    except OSError:
        return {"all": {"req": 0, "tok": 0}, "today": {"req": 0, "tok": 0},
                "since": ""}
    today = today_str()
    identity = (path, st.st_dev, st.st_ino)
    cached = _jsonl_cache.get("out")
    same = (cached is not None and _jsonl_cache.get("identity") == identity
            and _jsonl_cache.get("day") == today
            and st.st_size >= _jsonl_cache.get("size", 0))
    if same and st.st_size == _jsonl_cache["size"] and st.st_mtime_ns == _jsonl_cache.get("mtime"):
        return cached
    if same and st.st_size == _jsonl_cache["size"]:
        same = False
    out = ({"all": dict(cached["all"]), "today": dict(cached["today"]), "since": cached["since"]}
           if same else {"all": {"req": 0, "tok": 0}, "today": {"req": 0, "tok": 0}, "since": ""})
    offset = _jsonl_cache.get("offset", 0) if same else 0
    try:
        with open(path, "rb") as f:
            f.seek(offset)
            while True:
                line = f.readline()
                if not line or not line.endswith(b"\n"):
                    break
                offset = f.tell()
                try:
                    r = json.loads(line)
                except (ValueError, UnicodeError):
                    continue
                if not isinstance(r, dict):
                    continue
                tok = r.get("total_tokens") or (r.get("prompt_tokens", 0) + r.get("completion_tokens", 0))
                out["all"]["req"] += 1
                out["all"]["tok"] += tok
                ts = str(r.get("ts", ""))
                if not out["since"] and ts:
                    out["since"] = ts[:10]
                if ts[:10] == today:
                    out["today"]["req"] += 1
                    out["today"]["tok"] += tok
    except OSError:
        return out
    _jsonl_cache.update(mtime=st.st_mtime_ns, size=st.st_size, out=out,
                        identity=identity, day=today, offset=offset)
    return out


def fmt_int(n):
    return f"{n:,}"


def fmt_tok(n):
    if n >= 1_000_000_000:
        return "%.1fB" % (n / 1_000_000_000)
    if n >= 1_000_000:
        return "%.1fM" % (n / 1_000_000)
    if n >= 1_000:
        return "%.1fk" % (n / 1_000)
    return str(n)


def bench_rows(d, limit):
    """Scored models, best first. AA has no row for some, so the list is
    only what matched."""
    b = d.get("benchmarks") or {}
    rows = list(b.get("models") or [])
    rows.sort(key=lambda r: (-(r.get("intelligence_index") or 0),
                             r.get("id") or ""))
    if limit:
        rows = rows[:limit]
    return b, rows


def bench_table(rows):
    t = Table(show_header=True, header_style="bold", box=box.SIMPLE,
              pad_edge=False)
    t.add_column("intel", justify="right", style="bold white")
    t.add_column("model")
    t.add_column("backend", style="dim")
    t.add_column("ctx", justify="right", style="dim")
    t.add_column("maxout", justify="right", style="dim")
    t.add_column("match", style="dim")
    for r in rows:
        # a basename or family match is weaker evidence than an exact slug,
        # so it is dimmed rather than presented as a fact.
        kind = r.get("match") or "?"
        t.add_row("%.1f" % (r.get("intelligence_index") or 0),
                  r.get("id") or "?", r.get("backend") or "·",
                  str(r.get("ctx") or 0), str(r.get("maxout") or 0),
                  Text(kind, style="dim" if kind != "exact" else "green"))
    return t


def models_status(d, limit):
    b, rows = bench_rows(d, limit)
    if not b.get("enabled"):
        return Text("benchmarks disabled (benchmarks.enabled: false)",
                    style="yellow")
    if not rows:
        return Text("no benchmark scores yet: %s" %
                    (b.get("error") or "still fetching"), style="yellow")
    cap = Text()
    cap.append("aa ", style="dim")
    cap.append("%d models" % b.get("aa_models", 0), style="white")
    cap.append(" · matched ", style="dim")
    cap.append("%d" % b.get("matched", 0), style="bold white")
    cap.append(" · %d with no aa row" % b.get("unmatched", 0), style="dim")
    cap.append(" · source %s" % (b.get("updated_at") or "?"), style="dim")
    if b.get("age"):
        cap.append(" (%s ago)" % b["age"], style="dim")
    return Group(cap, Text(""), bench_table(rows))


def bench_index(d):
    """Build an id->row lookup for the status tables.

    /status keys the benchmark rows by *catalog* id, but the menu lists
    *serving* ids, and the two differ for models the catalog carries no
    entry for (an unlisted zen id has no catalog key, while the same model
    is keyed under its portal id). fall back to the bare model core so a
    score that exists still shows, dimmed to mark the weaker join.
    """
    rows = (d.get("benchmarks") or {}).get("models") or []
    by_id = {r.get("id"): r for r in rows}

    def get(full):
        r = by_id.get(full)
        if r:
            return r
        core = _model_core(full)
        for k, v in by_id.items():
            if _model_core(k) == core:
                v = dict(v)
                v["match"] = "alias"
                return v
        return None
    return get


def _model_core(mid):
    """bare model id: provider prefix dropped, dots folded, free/portal
    suffixes trimmed. 'nous/stepfun/step-5-preview:free' -> 'step-5-preview'."""
    core = (mid or "").rsplit("/", 1)[-1].replace(".", "-").lower()
    for suf in (":free", "-free"):
        if core.endswith(suf) and len(core) > len(suf):
            core = core[: -len(suf)]
    return core


def score_cell(r):
    """Benchmark score for one /v1/models row: aa's intelligence index, or a
    muted dash when aa has no row for the model."""
    if not r or not r.get("intelligence_index"):
        return Text("·", style="dim")
    kind = r.get("match") or ""
    # "alias" means the renderer joined by model core, not that go matched
    # it that way, so it stays dimmed.
    style = "bold white" if kind == "exact" else "dim"
    return Text("%.1f" % r["intelligence_index"], style=style)


def bench_line(d):
    """One-line benchmark caption for the status menu. Absent when the
    section is redacted (unauthenticated + auth.tokens) or the flag is off,
    which StatusFor already handles server-side."""
    b = d.get("benchmarks") or {}
    if not b.get("enabled") or not b.get("models"):
        return None
    best = b["models"][0]  # /status sorts the rows best-first
    served = b.get("served_scored", 0)
    return "benchmarks %d scored (%d catalog + %d unlisted) · %d aa models · best %s %.1f" % (
        b.get("matched", 0) + served, b.get("matched", 0), served,
        b.get("aa_models", 0), best.get("id", "?"),
        best.get("intelligence_index") or 0)


def plain_status(d):
    c = []
    c.append("NimRoute %s | up %s | load %d/%d" % (
        d.get("version", "?"), d.get("uptime", "?"),
        d.get("concurrent", 0), d.get("sem_limit", 0)))
    lanes = d.get("zen_lanes") or []
    served = sum(l.get("total_tokens", 0) for l in lanes)
    c.append("opencode %d active · nvidia %d active · nim lanes %d/%d · %s tok served" % (
        d.get("concurrent_opencode", 0), d.get("concurrent_nvidia", 0),
        len(lanes), d.get("lane_cap", len(lanes)), fmt_tok(served)))
    p = d.get("pool") or {}
    c.append("pool %d verified, %d dropped" % (
        p.get("verified", 0), p.get("dropped_total", 0)))
    bl = bench_line(d)
    if bl:
        c.append(bl)
    st = jsonl_stats()
    since = " since " + st["since"] if st["since"] else ""
    c.append("today %s req, %s tok · all-time %s req, %s tok%s" % (
        fmt_int(st["today"]["req"]), fmt_tok(st["today"]["tok"]),
        fmt_int(st["all"]["req"]), fmt_tok(st["all"]["tok"]), since))
    fp = d.get("freepi") or {}
    for acct in fp.get("accounts") or []:
        tok = acct.get("prompt_tokens", 0) + acct.get("completion_tokens", 0)
        c.append("freepi:%s %d req, %s tok%s" % (
            acct.get("name", "?"), acct.get("requests", 0), fmt_tok(tok),
            " INFLIGHT" if acct.get("inflight") else ""))
    antseed = d.get("antseed") or {}
    if antseed:
        c.append("antseed %d peers, %d offers: %s" % (
            antseed.get("peers", 0), antseed.get("offers", 0),
            ", ".join("antseed/" + m for m in antseed.get("models") or [])))
    rt = d.get("route") or {}
    if rt.get("enabled"):
        lat = rt.get("latency_ms") or {}
        lat_s = ", ".join("%s %sms" % (b, lat[b]) for b in sorted(lat)) or "no samples"

        def _rid(r):
            return r.get("id", "?") if isinstance(r, dict) else r
        c.append("route [%s] %d ids (%s): %s" % (
            rt.get("strategy", "?"), rt.get("count", 0), lat_s,
            ", ".join(_rid(r) for r in rt.get("routes") or [])))
    return "\n".join(c)


def fmt_ms(ms):
    try:
        ms = int(ms)
    except (TypeError, ValueError):
        return "·"
    if ms <= 0:
        return "·"
    if ms >= 10000:
        return "%.1fs" % (ms / 1000)
    if ms >= 1000:
        return "%.2fs" % (ms / 1000)
    return "%dms" % ms


def merged_table(d, limit=8):
    """One table: lane + its exit (host, cc) + api latency + req. the cc
    column drops out when no lane carries a country, i.e. custom proxies
    with no geo data."""
    lanes = d.get("zen_lanes") or []
    pool = d.get("pool") or {}
    exits = {e.get("proxy"): e for e in (pool.get("exits") or [])}
    lanes = sorted(lanes, key=lambda l: (-l.get("requests", 0), l.get("id", "")))
    shown = lanes[:limit] if limit else lanes
    rows, any_cc = [], False
    for l in shown:
        proxy = l.get("proxy") or ""
        e = exits.get(proxy, {})
        if proxy:
            exit_t = Text(proxy.split("://", 1)[-1])
        else:
            exit_t = Text("—", style="dim")
        cc = str(e.get("country") or l.get("country") or "").strip()
        if cc in ("", "??"):
            cc = "·"
        else:
            any_cc = True
        rows.append(((l.get("id") or "?")[:14], exit_t, cc,
                     fmt_ms(e.get("avg_ms")),
                     str(l.get("requests", 0)), str(l.get("rate_limited", 0)),
                     fmt_ms(l.get("avg_ms")), fmt_tok(l.get("total_tokens", 0))))
    t = Table(show_header=True, header_style="bold", box=box.SIMPLE,
              pad_edge=False)
    t.add_column("lane", style="slate_blue1")
    t.add_column("exit")
    if any_cc:
        t.add_column("cc")
    t.add_column("ttfb", justify="right")
    t.add_column("req", justify="right")
    t.add_column("429", justify="right", style="yellow")
    t.add_column("avg turn", justify="right")
    t.add_column("tokens", justify="right")
    for r in rows:
        t.add_row(*(r if any_cc else r[:2] + r[3:]))
    if len(shown) < len(lanes):
        t.caption = "%d more lanes · nim status shows the busiest 8" % (len(lanes) - len(shown))
    return t


def fmt_pct(pct):
    try:
        pct = float(pct)
    except (TypeError, ValueError):
        pct = 0
    if pct < 10:
        return "%.1f%%" % pct
    return "%d%%" % round(pct)


def usage_bar(pct, width=10):
    """Colored block bar for daily spend %: green → yellow → red."""
    filled = min(width, max(0, int(round(pct / 100 * width))))
    bar = Text()
    for i in range(filled):
        frac = i / max(width - 1, 1)
        bar.append("█", style="red" if frac >= 0.8 else
                   "yellow" if frac >= 0.5 else "green")
    bar.append("░" * (width - filled), style="dim")
    return bar


# muted slate palette: the model id carries the color of its backend,
# the rest of its row stays dim.
PROVIDER_STYLES = {"opencode": "slate_blue1", "freepi": "medium_purple3",
                   "antseed": "steel_blue", "nvidia": "dark_sea_green",
                   "nous": "medium_spring_green", "route": "gold1",
                   "uno": "cornflower_blue", "openrouter": "plum2"}


def health_cell(health, model):
    """Status cell: ✓ working, X when broken, - when unknown."""
    h = (health or {}).get(model) or {}
    state = h.get("state", "unknown")
    if state == "working":
        return Text("✓", style="green")
    if state == "rate_limited":
        return Text("X", style="yellow")
    if state == "dead":
        return Text("X", style="red")
    return Text("-", style="dim")


def model_totals(by_model, model):
    """usage.by_model row for one /status model id. the table's ids and the
    usage keys differ on the opencode prefix (and the client may send the bare
    name), so try the prefixed id, the bare name, then the prefixed bare name."""
    bm = by_model or {}
    t = bm.get(model)
    if t:
        return t
    bare = model.split("/", 1)[-1]
    for cand in (bare, "opencode/" + bare):
        t = bm.get(cand)
        if t:
            return t
    return None


def tok_cell(by_model, model):
    t = model_totals(by_model, model)
    if not t:
        return Text("·", style="dim")
    n = t.get("prompt_tokens", 0) + t.get("completion_tokens", 0)
    return Text(fmt_tok(n), style="dim")


def tps_cell(by_model, model):
    """completion tokens / request wall time. throughput, so prefill and
    time-to-first-token are inside the denominator, not decode speed alone."""
    t = model_totals(by_model, model)
    if not t:
        return Text("·", style="dim")
    dur = t.get("duration_ms") or 0
    if dur <= 0:
        return Text("·", style="dim")
    return Text("%.1f" % (t.get("completion_tokens", 0) / (dur / 1000.0)),
                style="dim")


def build_renderable(d, stats):
    parts = []
    hdr = Text("NimRoute %s" % d.get("version", "?"), style="bold slate_blue1")
    hdr.append("  up %s" % d.get("uptime", "?"), style="dim")
    hdr.append("  load %d/%d" % (d.get("concurrent", 0),
                                 d.get("sem_limit", 0)))
    if "keys" in d:
        ok = bool(d.get("available"))
        hdr.append("  keys %d/%d" % (d.get("available", 0),
                                     d.get("total", 0)),
                   style="dark_sea_green" if ok else "red")
    parts.append(hdr)
    parts.append(Text("opencode %d active · nvidia %d active · nim lanes %d/%d" % (
        d.get("concurrent_opencode", 0), d.get("concurrent_nvidia", 0),
        len(d.get("zen_lanes") or []), d.get("lane_cap", 0)), style="dim"))
    if d.get("zen_lanes"):
        parts.append(merged_table(d))
    oc = d.get("opencode") or {}
    oc_models = list(oc.get("models") or [])
    health = d.get("health") or {}
    fp = d.get("freepi") or {}
    fp_models, fp_rows = [], []
    for acct in fp.get("accounts") or []:
        ms = acct.get("models") or []
        fp_models.extend(ms)
        req = acct.get("requests", 0)
        tok = acct.get("prompt_tokens", 0) + acct.get("completion_tokens", 0)
        mark = "◆" if acct.get("inflight") else "◇"
        fp_rows.append((mark, acct.get("name", "?"), req, tok, ms,
                        acct.get("tier", ""), acct.get("credits", 0),
                        acct.get("spent_pct", 0), acct.get("req_today", 0)))
    nv_keys = [k.get("name", "?") for k in (d.get("keys") or [])]
    cols = []
    if oc_models:
        cols.append(("opencode", oc_models))
    if fp_models or fp_rows:
        seen, uniq = set(), []
        for m in fp_models:
            full = m if "/" in m else "freeepi/" + m
            if full not in seen:
                seen.add(full)
                uniq.append(full)
        cols.append(("freepi", uniq))
    antseed = d.get("antseed")
    if antseed:
        antseed_models = antseed.get("models") or []
        if not antseed_models:  # a placeholder row, still backend-prefixed
            antseed_models = ["discovering…" if not antseed.get("stale_for")
                              else "no free models"]
        cols.append(("antseed", ["antseed/" + m for m in antseed_models]))
    if nv_keys:
        cols.append(("nvidia", ["nvidia/" + "keys: " + ", ".join(nv_keys)]))
    nous = d.get("nous") or {}
    nous_models = list(nous.get("models") or [])
    if nous_models:
        # portal ids already contain a slash ("poolside/laguna-s-2.1:free"),
        # so the generic name+"/"+val prefixing below would leave them bare
        # and miss the usage.by_model key. prefix them here.
        cols.append(("nous", ["nous/" + m for m in nous_models]))
    uno = d.get("uno") or {}
    if uno.get("models"):
        cols.append(("uno", list(uno["models"])))
    orx = d.get("openrouter") or {}
    if orx.get("models"):
        # same slash-in-id trap as nous: ids are openrouter's own
        # ("cohere/north-mini-code:free"), so the proxy's prefix goes on here.
        cols.append(("openrouter", ["openrouter/" + m for m in orx["models"]]))
    # route ids last: one stable id per model that 2+ backends serve.
    rt = d.get("route") or {}
    rt_via = {}
    if rt.get("enabled"):
        route_ids = []
        for row in rt.get("routes") or []:
            if isinstance(row, dict):
                rid, via = row.get("id", "?"), row.get("via") or []
            else:  # older server shape: bare id strings
                rid, via = row, []
            route_ids.append(rid)
            rt_via[rid] = via
        if route_ids:
            cols.append(("route", route_ids))
    if cols:
        by_model = (d.get("usage") or {}).get("by_model") or {}
        bench = bench_index(d)
        mults = {}
        for acct in (d.get("freepi") or {}).get("accounts") or []:
            mults.update(acct.get("mults") or {})
        t = Table(show_header=True, header_style="bold", box=box.SIMPLE, pad_edge=False)
        t.add_column("", justify="left")
        for label in ("model", "score", "req", "tokens", "tok/s*", ""):
            t.add_column(label, justify="right" if label in ("score", "req", "tokens", "tok/s*") else "left")
        for name, models in cols:
            for val in models:
                # every row is one full id: provider and model never split,
                # so the table matches what a client sends and what /status
                # keys usage.by_model by.
                full = val if "/" in val else name + "/" + val
                totals = model_totals(by_model, full) or {}
                # trailing column: usage multiplier on freepi rows, serving
                # backends on route rows, empty everywhere else.
                tail = ""
                if name == "freepi":
                    tail = mults.get(full.split("/", 1)[-1], "")
                elif name == "route":
                    tail = " · ".join(rt_via.get(full) or [])
                t.add_row(health_cell(health, full),
                          Text(full, style=PROVIDER_STYLES.get(name, "dim")),
                          score_cell(bench(full)),
                          str(totals.get("requests", 0)),
                          tok_cell(by_model, full), tps_cell(by_model, full),
                          Text(tail, style="dim") if tail else Text(""))
        parts.append(t)
        parts.append(Text("* tok/s uses complete request time, not decode speed · · = no sample", style="dim"))
    bl = bench_line(d)
    if bl:
        parts.append(Text(bl, style="dim"))
    for mark, name, req, tok, ms, tier, cred, pct, rtoday in fp_rows:
        line = Text()
        line.append("%s freepi:%s  " % (mark, name),
                    style="yellow" if mark == "◆" else "magenta")
        line.append("%d req · %s tok · %s · %g credits · " % (
            req, fmt_tok(tok), tier or "?", cred))
        line.append(usage_bar(pct))
        line.append(" %s day · %d today" % (fmt_pct(pct), rtoday), style="dim")
        parts.append(line)
    nous_keys = (d.get("nous") or {}).get("keys") or []
    if nous_keys:
        live = [k for k in nous_keys if k.get("available")]
        line = Text()
        line.append("nous  ", style="dim")
        line.append("%d/%d keys up" % (len(live), len(nous_keys)),
                    style="medium_spring_green" if live else "red")
        cooling = [k for k in nous_keys if not k.get("available")]
        if cooling:
            line.append("  ·  cooling " + ", ".join(
                "%s %s" % (k.get("name", "?"), k.get("cooldown", "?"))
                for k in cooling), style="dim")
        parts.append(line)
    p = d.get("pool") or {}
    pool_t = Text()
    pool_t.append("pool  ", style="dim")
    pool_t.append("%d verified" % p.get("verified", 0), style="dark_sea_green")
    pool_t.append(" · ", style="dim")
    pool_t.append("%d dropped" % p.get("dropped_total", 0),
                  style="red" if p.get("dropped_total", 0) else "dim")
    pool_t.append(" · refresh %s ago" % p.get("last_refresh_ago", "?"),
                  style="dim")
    parts.append(pool_t)
    use_t = Text()
    use_t.append("today ", style="dim")
    use_t.append("%s req" % fmt_int(stats["today"]["req"]), style="bold white")
    use_t.append(" · %s tok" % fmt_tok(stats["today"]["tok"]), style="slate_blue1")
    use_t.append("   │   all-time ", style="dim")
    use_t.append("%s req" % fmt_int(stats["all"]["req"]), style="bold white")
    use_t.append(" · %s tok" % fmt_tok(stats["all"]["tok"]), style="slate_blue1")
    if stats["since"]:
        use_t.append(" since %s" % stats["since"], style="dim")
    parts.append(use_t)
    return Group(*parts)


def rich_status(d):
    con = Console()
    con.print(build_renderable(d, jsonl_stats()))


def plain_models(d, limit):
    b, rows = bench_rows(d, limit)
    if not b.get("enabled"):
        return "benchmarks disabled (benchmarks.enabled: false)"
    if not rows:
        return "no benchmark scores yet: %s" % (
            b.get("error") or "still fetching")
    out = ["intel   model                              backend  ctx      maxout"]
    for r in rows:
        out.append("%5.1f  %-34s %-8s %7s  %6s" % (
            r.get("intelligence_index") or 0, r.get("id") or "?",
            r.get("backend") or "-", r.get("ctx") or 0,
            r.get("maxout") or 0))
    return "\n".join(out)


def main():
    cmd = sys.argv[1] if len(sys.argv) > 1 else "status"
    if cmd == "status":
        try:
            d = fetch("/status")
        except Exception as e:
            print("(status fetch failed: %s)" % e)
            return 1
        if RICH:
            rich_status(d)
        else:
            print(plain_status(d))
    elif cmd == "tail-status":
        if not RICH:
            import time
            try:
                while True:
                    try:
                        text = plain_status(fetch("/status"))
                    except (OSError, ValueError) as e:
                        text = "status unavailable: %s" % e
                    if sys.stdout.isatty():
                        print("\033[2J\033[H", end="")
                    print(text, flush=True)
                    time.sleep(2)
            except KeyboardInterrupt:
                return 0
        con = Console()

        def render():
            try:
                d = fetch("/status")
            except Exception as e:
                return Text("(status fetch failed: %s)" % e, style="red")
            return build_renderable(d, jsonl_stats())
        with Live(render(), refresh_per_second=0.5, console=con) as live:
            import time
            try:
                while True:
                    time.sleep(1)
                    live.update(render())
            except KeyboardInterrupt:
                pass
    elif cmd == "models":
        # optional arg: a row cap, so `nim models` stays terminal-height.
        limit = 0
        if len(sys.argv) > 2:
            try:
                limit = int(sys.argv[2])
            except ValueError:
                print("usage: nimstatus.py models [N]")
                return 1
        try:
            d = fetch("/status")
        except Exception as e:
            print("(status fetch failed: %s)" % e)
            return 1
        if RICH:
            con = Console()
            con.print(models_status(d, limit))
        else:
            print(plain_models(d, limit))
    elif cmd == "logs":
        try:
            n = int(sys.argv[2]) if len(sys.argv) > 2 else 10
        except ValueError:
            print("usage: nimstatus.py logs [N]")
            return 1
        log = os.path.join(ROOT, "nimroute.log")
        try:
            # bounded tail seek: no whole-file read on a big log.
            with open(log, "rb") as f:
                f.seek(0, 2)
                size = f.tell()
                blk, pos, buf = 8192, size, b""
                while pos > 0 and buf.count(b"\n") <= n:
                    step = blk if pos >= blk else pos
                    pos -= step
                    f.seek(pos)
                    buf = f.read(step) + buf
                lines = buf.splitlines()[-n:]
                lines = [ln.decode("utf-8", "replace") + "\n" for ln in lines]
        except OSError as e:
            print("(log read failed: %s)" % e)
            return 1
        # redact embedded proxy credentials when redaction failed upstream.
        lines = [re.sub(r"://[^/@\s]+@[^\s]+", "://***@", ln) for ln in lines]
        if RICH:
            con = Console()
            for line in lines:
                line = line.rstrip("\n")
                if "!!" in line:
                    con.print(line, style="red")
                elif "retry" in line or "429" in line or "403" in line:
                    con.print(line, style="yellow")
                else:
                    con.print(line, style="dim")
        else:
            print("".join(lines), end="")
    else:
        print("usage: nimstatus.py [status|tail-status|models [N]|logs [N]]")
        return 1


if __name__ == "__main__":
    sys.exit(main())
