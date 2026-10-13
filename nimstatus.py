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

    cached on (mtime, size): the tail loop calls this every second and a
    20MB re-parse per tick pegs a core on a busy proxy.
    """
    path = os.path.join(ROOT, "nim-usage.jsonl")
    try:
        st = os.stat(path)
    except OSError:
        return {"all": {"req": 0, "tok": 0}, "today": {"req": 0, "tok": 0},
                "since": ""}
    if (st.st_mtime, st.st_size) == (_jsonl_cache["mtime"], _jsonl_cache["size"]) \
            and _jsonl_cache["out"] is not None:
        return _jsonl_cache["out"]
    out = {"all": {"req": 0, "tok": 0}, "today": {"req": 0, "tok": 0},
           "since": ""}
    today = today_str()
    try:
        with open(path) as f:
            for line in f:
                try:
                    r = json.loads(line)
                except ValueError:
                    continue
                tok = r.get("total_tokens") or (
                    r.get("prompt_tokens", 0) + r.get("completion_tokens", 0))
                out["all"]["req"] += 1
                out["all"]["tok"] += tok
                ts = str(r.get("ts", ""))
                if not out["since"] and ts:
                    out["since"] = ts[:10]
                if ts[:10] == today:
                    out["today"]["req"] += 1
                    out["today"]["tok"] += tok
    except OSError:
        pass
    _jsonl_cache["mtime"], _jsonl_cache["size"], _jsonl_cache["out"] = \
        st.st_mtime, st.st_size, out
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


def plain_status(d):
    c = []
    c.append("NimRoute %s | up %s | load %d/%d" % (
        d.get("version", "?"), d.get("uptime", "?"),
        d.get("concurrent", 0), d.get("sem_limit", 0)))
    lanes = d.get("zen_lanes") or []
    served = sum(l.get("total_tokens", 0) for l in lanes)
    c.append("lanes %d live · %s tok served" % (len(lanes), fmt_tok(served)))
    p = d.get("pool") or {}
    c.append("pool %d verified, %d dropped" % (
        p.get("verified", 0), p.get("dropped_total", 0)))
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


def merged_table(d):
    """One table: lane + its exit (host, cc) + api latency + req."""
    lanes = d.get("zen_lanes") or []
    pool = d.get("pool") or {}
    exits = {e.get("proxy"): e for e in (pool.get("exits") or [])}
    t = Table(show_header=True, header_style="bold", box=box.SIMPLE,
              pad_edge=False)
    t.add_column("lane", style="slate_blue1")
    t.add_column("exit")
    t.add_column("cc")
    t.add_column("lat", justify="right")
    t.add_column("req", justify="right")
    for l in lanes:
        proxy = l.get("proxy") or ""
        e = exits.get(proxy, {})
        if proxy:
            host = proxy.split("://", 1)[-1]
            exit_t = Text(host)
        else:
            exit_t = Text("—", style="dim")
        t.add_row((l.get("id") or "?")[:14], exit_t,
                  str(e.get("country", "·")),
                  fmt_ms(e.get("latency_ms")),
                  str(l.get("requests", 0)))
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


# muted slate palette: provider names carry the color, values stay dim.
PROVIDER_STYLES = {"opencode": "slate_blue1", "freepi": "medium_purple3",
                   "antseed": "steel_blue", "nvidia": "dark_sea_green",
                   "nous": "medium_spring_green"}

# usage multipliers come from /status per-account "mults" (parsed live from
# the freepi catalog display names, e.g. "GLM 5.3 Flash · ~0.7x usage").


def health_cell(health, model):
    """Status cell: ✓ working, red code when broken, · when unknown."""
    h = (health or {}).get(model) or {}
    state = h.get("state", "unknown")
    if state == "working":
        return Text("✓", style="green")
    if state == "broken":
        code = h.get("code")
        return Text(str(code) if code else "err", style="red")
    return Text("·", style="dim")


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
    parts.append(merged_table(d))
    parts.append(Text("convo = sticky conversation · shared = overflow pool · hdr = pinned header",
                       style="dim"))
    oc = d.get("opencode") or {}
    oc_models = list(oc.get("models") or [])
    health = d.get("health") or {}
    fp = d.get("freepi") or {}
    fp_models, fp_rows, fp_mults = [], [], {}
    for acct in fp.get("accounts") or []:
        ms = acct.get("models") or []
        fp_models.extend(ms)
        fp_mults.update(acct.get("mults") or {})
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
        cols.append(("antseed", ["antseed/" + m for m in antseed_models]
                     or ["discovering…" if not antseed.get("stale_for") else "no free models"]))
    if nv_keys:
        cols.append(("nvidia", ["keys: " + ", ".join(nv_keys)]))
    nous = d.get("nous") or {}
    nous_models = list(nous.get("models") or [])
    if nous_models:
        # portal ids already contain a slash ("poolside/laguna-s-2.1:free"),
        # so the generic name+"/"+val prefixing below would leave them bare
        # and miss the usage.by_model key. prefix them here.
        cols.append(("nous", ["nous/" + m for m in nous_models]))
    if cols:
        by_model = (d.get("usage") or {}).get("by_model") or {}
        t = Table(show_header=True, header_style="bold", box=box.SIMPLE,
                  pad_edge=False)
        for name, _ in cols:
            t.add_column(name, style=PROVIDER_STYLES.get(name, "dim"))
            t.add_column("status", justify="right", style="dim")
            if name == "freepi":
                t.add_column("mult", justify="right", style="dim")
            t.add_column("tok", justify="right", style="dim")
            t.add_column("tps", justify="right", style="dim")
        widest = max(len(ms) for _, ms in cols)
        for row in zip(*[ms + [""] * (widest - len(ms)) for _, ms in cols]):
            cells = []
            for (name, _), val in zip(cols, row):
                cells.append(Text(val, style=PROVIDER_STYLES.get(name, "dim")))
                if val:
                    full = val if "/" in val else name + "/" + val
                    cells.append(health_cell(health, full))
                    if name == "freepi":
                        bare = full.split("/", 1)[-1]
                        cells.append(Text(fp_mults.get(bare, "·"), style="dim"))
                    cells.append(tok_cell(by_model, full))
                    cells.append(tps_cell(by_model, full))
                else:
                    cells.append(Text(""))
                    if name == "freepi":
                        cells.append(Text(""))
                    cells.append(Text(""))
                    cells.append(Text(""))
            t.add_row(*cells)
        parts.append(t)
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
    rt = d.get("route") or {}
    if rt.get("enabled") and rt.get("routes"):
        rt_t = Table(show_header=True, header_style="bold", box=box.SIMPLE,
                     pad_edge=False)
        rt_t.add_column("route", style="gold1")
        rt_t.add_column("via", style="dim")
        lat = rt.get("latency_ms") or {}
        order = {"opencode": 0, "antseed": 1, "freepi": 2, "nvidia": 3}
        for row in rt.get("routes") or []:
            if isinstance(row, dict):
                rid, via = row.get("id", "?"), row.get("via") or []
            else:  # older server shape: bare id strings
                rid, via = row, []
            via = [b for b in sorted(via, key=lambda b: order.get(b, 9))]
            rt_t.add_row(rid, " · ".join(via) or "·")
        lat_s = "  ".join("%s %sms" % (b, lat[b]) for b in
                           sorted(lat, key=lambda b: order.get(b, 9)))
        cap = "route [%s] %d ids" % (rt.get("strategy", "?"), rt.get("count", 0))
        if lat_s:
            cap += "  ·  fastest: " + lat_s
        parts.append(Text(cap, style="dim"))
        parts.append(rt_t)
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
            print("rich not available, install with: pip install rich")
            return 1
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
