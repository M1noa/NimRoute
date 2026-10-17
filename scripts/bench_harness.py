#!/usr/bin/env python3
"""bench_harness.py - concurrent coding-agent load generator for nimroute.

Simulates N coding harnesses (claude-code shaped) hitting the proxy over both
the Anthropic Messages API and the OpenAI chat-completions API. Each harness
keeps its own stable x-session-id so lane pinning engages, and grows a fake
tool-use history across turns the way a real agent does.

Dependency-free: raw asyncio HTTP/1.1 + SSE parsing, so byte timings are exact.

Per request it records ttfb (first response byte), ttfc (first content delta,
what the user actually feels), total, token counts, tps, per-chunk gap stats
and the response text. A background poller samples /status every second.

usage:
  python3 scripts/bench_harness.py --clients 12 --rounds 3
  python3 scripts/bench_harness.py --mode local --clients 12   # proxy overhead only
"""
import argparse
import asyncio
import json
import os
import random
import statistics
import sys
import time

# ---------------------------------------------------------------- fixtures

TOOLS = [
    {"name": "Bash", "description": "Run a shell command in the working directory.",
     "input_schema": {"type": "object", "properties": {
         "command": {"type": "string"}, "timeout": {"type": "number"},
         "description": {"type": "string"}}, "required": ["command"]}},
    {"name": "Read", "description": "Read a file from the local filesystem.",
     "input_schema": {"type": "object", "properties": {
         "file_path": {"type": "string"}, "offset": {"type": "number"},
         "limit": {"type": "number"}}, "required": ["file_path"]}},
    {"name": "Edit", "description": "Replace an exact string in a file.",
     "input_schema": {"type": "object", "properties": {
         "file_path": {"type": "string"}, "old_string": {"type": "string"},
         "new_string": {"type": "string"}, "replace_all": {"type": "boolean"}},
         "required": ["file_path", "old_string", "new_string"]}},
    {"name": "Write", "description": "Write a file to the local filesystem.",
     "input_schema": {"type": "object", "properties": {
         "file_path": {"type": "string"}, "content": {"type": "string"}},
         "required": ["file_path", "content"]}},
    {"name": "Glob", "description": "Find files matching a glob pattern.",
     "input_schema": {"type": "object", "properties": {
         "pattern": {"type": "string"}, "path": {"type": "string"}},
         "required": ["pattern"]}},
    {"name": "Grep", "description": "Search file contents with a regex.",
     "input_schema": {"type": "object", "properties": {
         "pattern": {"type": "string"}, "path": {"type": "string"},
         "output_mode": {"type": "string"}}, "required": ["pattern"]}},
    {"name": "Task", "description": "Launch a subagent to handle a multi-step task.",
     "input_schema": {"type": "object", "properties": {
         "description": {"type": "string"}, "prompt": {"type": "string"}},
         "required": ["description", "prompt"]}},
    {"name": "TodoWrite", "description": "Update the structured todo list.",
     "input_schema": {"type": "object", "properties": {
         "todos": {"type": "array", "items": {"type": "object"}}},
         "required": ["todos"]}},
    {"name": "WebFetch", "description": "Fetch a URL and return its markdown.",
     "input_schema": {"type": "object", "properties": {
         "url": {"type": "string"}, "prompt": {"type": "string"}},
         "required": ["url", "prompt"]}},
    {"name": "WebSearch", "description": "Search the web for current information.",
     "input_schema": {"type": "object", "properties": {
         "query": {"type": "string"}}, "required": ["query"]}},
    {"name": "NotebookEdit", "description": "Edit a Jupyter notebook cell.",
     "input_schema": {"type": "object", "properties": {
         "notebook_path": {"type": "string"}, "new_source": {"type": "string"}},
         "required": ["notebook_path", "new_source"]}},
    {"name": "ExitPlanMode", "description": "Signal that planning is complete.",
     "input_schema": {"type": "object", "properties": {
         "plan": {"type": "string"}}, "required": ["plan"]}},
]

SYSTEM_PROMPT_HEAD = """You are Claude Code, Anthropic's official CLI for Claude.

You are an interactive CLI tool that helps users with software engineering tasks.
Use the instructions below and the tools available to you to assist the user.

IMPORTANT: Assist with defensive security tasks only. Refuse to create, modify, or
improve code that could be used maliciously. Allow security analysis, detection
rules, vulnerability explanation, and defensive tooling.

# Tone and style
- Only use emojis if the user explicitly requests it.
- Your responses should be short and concise.
- When referencing specific functions or pieces of code include the pattern
  file_path:line_number to allow the user to easily navigate to the source code.

# Proactiveness
You are allowed to be proactive, but only when the user asks you to do something.
Strike a balance between doing the right thing when asked, including taking
actions that are irreversible and actions that impact shared systems.

# Following conventions
When making changes to files, first understand the file's code conventions.
Mimic existing code, use existing libraries and utilities, follow existing
patterns. Never assume that a given library is available, even if it is
commonly used.

# Doing tasks
The user will primarily request you perform software engineering tasks. This
includes solving bugs, adding new functionality, refactoring code, explaining
code, and more. For these tasks the following steps are recommended:
- Use the TodoWrite tool to plan the task if required
- Use the available search tools to understand the codebase and the user's query
- Implement the solution using all tools available to you
- Verify the solution if possible with tests
- VERY IMPORTANT: When you have completed a task, you MUST run the lint and
  typecheck commands if available to ensure your code is correct

# Tool usage policy
- Prefer dedicated tools over shell equivalents
- You can call multiple tools in a single response
- If you intend to call multiple tools and there are no dependencies between the
  calls, make all of the independent calls in the same block

# Code References
When referencing code, use the format file_path:line_number.

# Environment
Working directory: /Users/dev/projects/webapp
Platform: darwin
OS Version: Darwin 25.0.0
Today's date: 2026-10-16
"""

# filler that looks like real repo context so the prompt is big and token-dense
CONTEXT_BLOB = """
=== src/server/handlers.go ===
func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) error {
    ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
    defer cancel()
    session, err := s.store.Load(ctx, r.Header.Get("X-Session-Id"))
    if err != nil {
        return fmt.Errorf("load session: %w", err)
    }
    if session.Expired(time.Now()) {
        http.Error(w, "session expired", http.StatusUnauthorized)
        return nil
    }
    w.Header().Set("Content-Type", "application/json")
    return json.NewEncoder(w).Encode(session.Snapshot())
}

=== src/server/middleware.go ===
func withRecovery(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        defer func() {
            if rec := recover(); rec != nil {
                log.Printf("panic: %v\\n%s", rec, debug.Stack())
                http.Error(w, "internal error", http.StatusInternalServerError)
            }
        }()
        next.ServeHTTP(w, r)
    })
}

=== src/store/session.go ===
type Session struct {
    ID        string    `json:"id"`
    UserID    string    `json:"user_id"`
    CreatedAt time.Time `json:"created_at"`
    ExpiresAt time.Time `json:"expires_at"`
    Scopes    []string  `json:"scopes"`
}

func (s *Session) Expired(now time.Time) bool {
    return now.After(s.ExpiresAt)
}

=== tests/handlers_test.go ===
func TestHandleRequestExpired(t *testing.T) {
    srv := newTestServer(t)
    req := httptest.NewRequest("GET", "/api/session", nil)
    req.Header.Set("X-Session-Id", "expired-1")
    rec := httptest.NewRecorder()
    srv.ServeHTTP(rec, req)
    if rec.Code != http.StatusUnauthorized {
        t.Fatalf("got %d, want 401", rec.Code)
    }
}
"""

CODE_CHUNK = "\n".join(
    "    %s\n" % line for line in [
        "if err != nil { return nil, fmt.Errorf(\"wrap: %w\", err) }",
        "for i, item := range items { if item.ID == want { return item, nil } }",
        "buf := make([]byte, 0, 4096); buf = append(buf, prefix...)",
        "select { case <-ctx.Done(): return ctx.Err(); case v := <-ch: out = append(out, v) }",
        "mu.Lock(); defer mu.Unlock(); m[key] = value",
        "switch kind { case KindA: return a(); case KindB: return b(); default: return nil }",
        "go func() { defer wg.Done(); results <- work(ctx, job) }()",
        "if !bytes.Equal(got, want) { t.Errorf(\"got %q want %q\", got, want) }",
    ]
)


def make_system_prompt(target_bytes):
    parts = [SYSTEM_PROMPT_HEAD]
    while sum(len(p) for p in parts) < target_bytes:
        parts.append(CONTEXT_BLOB)
        parts.append(CODE_CHUNK)
    return "".join(parts)


def make_history(turns, bytes_per_turn, rng):
    """Fake prior tool-use conversation, shape-correct for both APIs."""
    msgs = []
    for i in range(turns):
        pad = "".join(rng.choice(CODE_CHUNK) for _ in range(max(1, bytes_per_turn // 60)))
        if i % 2 == 0:
            msgs.append({"role": "user", "content": "Step %d: investigate the failure.\n%s" % (i, pad)})
            # assistant asks for a tool, user hands back a result
            msgs.append({"role": "assistant", "content": None,
                         "tool_calls": [{"id": "call_%d" % i, "type": "function",
                                        "function": {"name": "Read",
                                                     "arguments": json.dumps({"file_path": "/src/x%d.go" % i})}}]})
            msgs.append({"role": "tool", "tool_call_id": "call_%d" % i,
                         "content": "file contents:\n" + pad})
        else:
            msgs.append({"role": "assistant", "content": "Analysis of step %d.\n%s" % (i, pad)})
            msgs.append({"role": "user", "content": "continue, and check the error path\n%s" % pad})
    return msgs


def to_anthropic(msgs, system, model, max_tokens, stream=True):
    """OpenAI-shaped history -> Anthropic Messages body."""
    out = []
    for m in msgs:
        if m["role"] == "user":
            out.append({"role": "user", "content": [{"type": "text", "text": m["content"]}]})
        elif m["role"] == "assistant":
            if m.get("tool_calls"):
                blocks = []
                if m.get("content"):
                    blocks.append({"type": "text", "text": m["content"]})
                for tc in m["tool_calls"]:
                    args = tc["function"]["arguments"]
                    try:
                        args = json.loads(args)
                    except Exception:
                        args = {"raw": args}
                    blocks.append({"type": "tool_use", "id": tc["id"],
                                   "name": tc["function"]["name"], "input": args})
                out.append({"role": "assistant", "content": blocks})
            else:
                out.append({"role": "assistant", "content": [{"type": "text", "text": m["content"]}]})
        elif m["role"] == "tool":
            out.append({"role": "user", "content": [{"type": "tool_result",
                                                     "tool_use_id": m["tool_call_id"],
                                                     "content": m["content"]}]})
    # anthropic needs alternating roles; merge consecutive same-role turns
    merged = []
    for m in out:
        if merged and merged[-1]["role"] == m["role"]:
            merged[-1]["content"] = merged[-1]["content"] + m["content"]
        else:
            merged.append(m)
    return {"model": model, "max_tokens": max_tokens, "system": system,
            "messages": merged, "tools": TOOLS, "stream": stream}


def to_openai(msgs, system, model, max_tokens, stream=True):
    return {"model": model, "max_tokens": max_tokens,
            "messages": [{"role": "system", "content": system}] + msgs,
            "tools": [{"type": "function", "function": t} for t in TOOLS],
            "stream": stream, "stream_options": {"include_usage": True}}


# ---------------------------------------------------------------- http

class Resp:
    __slots__ = ("status", "headers", "body", "rtt_ms")

    def __init__(self):
        self.status = 0
        self.headers = {}
        self.body = b""
        self.rtt_ms = 0.0


async def raw_request(host, port, path, payload, headers, timeout, method="POST"):
    """single-request HTTP/1.1 exchange; returns headers after headers+body read."""
    body = payload if isinstance(payload, bytes) else json.dumps(payload).encode()
    hdrs = dict(headers)
    hdrs["Content-Length"] = str(len(body))
    hdrs.setdefault("Host", "%s:%d" % (host, port))
    hdrs.setdefault("Connection", "close")
    head = ("%s %s HTTP/1.1\r\n" % (method, path)).encode()
    for k, v in hdrs.items():
        head += ("%s: %s\r\n" % (k, v)).encode()
    head += b"\r\n"

    t0 = time.perf_counter()
    reader, writer = await asyncio.wait_for(asyncio.open_connection(host, port), timeout)
    writer.write(head + body)
    await writer.drain()
    status_line = await asyncio.wait_for(reader.readuntil(b"\r\n"), timeout)
    r = Resp()
    r.rtt_ms = (time.perf_counter() - t0) * 1000
    r.status = int(status_line.split()[1])
    while True:
        line = await asyncio.wait_for(reader.readuntil(b"\r\n"), timeout)
        if line in (b"\r\n", b"\n"):
            break
        k, _, v = line.decode("latin-1").partition(":")
        r.headers[k.strip().lower()] = v.strip()
    if r.headers.get("transfer-encoding", "").lower() == "chunked":
        chunks = []
        while True:
            szline = await asyncio.wait_for(reader.readuntil(b"\r\n"), timeout)
            n = int(szline.strip().split(b";")[0] or b"0", 16)
            if n == 0:
                break
            chunks.append(await asyncio.wait_for(reader.readexactly(n), timeout))
            await asyncio.wait_for(reader.readexactly(2), timeout)
        r.body = b"".join(chunks)
    elif int(r.headers.get("content-length", "0") or 0) > 0:
        r.body = await asyncio.wait_for(reader.readexactly(int(r.headers["content-length"])), timeout)
    else:
        r.body = await asyncio.wait_for(reader.read(), timeout)
    writer.close()
    try:
        await writer.wait_closed()
    except Exception:
        pass
    return r


async def stream_request(host, port, path, payload, headers, timeout, on_event):
    """streaming POST; parses chunked framing and SSE. returns a metrics row."""
    body = json.dumps(payload).encode()
    hdrs = dict(headers)
    hdrs["Content-Length"] = str(len(body))
    hdrs.setdefault("Host", "%s:%d" % (host, port))
    hdrs.setdefault("Accept", "text/event-stream")
    head = ("POST %s HTTP/1.1\r\n" % path).encode()
    for k, v in hdrs.items():
        head += ("%s: %s\r\n" % (k, v)).encode()
    head += b"\r\n"

    row = {"status": 0, "ttfb_ms": None, "ttfc_ms": None, "total_ms": None,
           "in_tok": None, "out_tok": None, "chunks": 0, "content_chunks": 0,
           "gaps_ms": [], "max_gap_ms": None, "text_len": 0, "tool_calls": 0,
           "thinking_chunks": 0, "stop_reason": None, "err": None, "req_bytes": len(body),
           "stream_ms": None, "_last": None}

    t0 = time.perf_counter()
    ms = lambda: round((time.perf_counter() - t0) * 1000, 2)
    writer = None
    try:
        reader, writer = await asyncio.wait_for(asyncio.open_connection(host, port), timeout)
        writer.write(head + body)
        await writer.drain()

        status_line = await asyncio.wait_for(reader.readuntil(b"\r\n"), timeout)
        row["ttfb_ms"] = ms()
        row["status"] = int(status_line.split()[1])
        h = {}
        while True:
            line = await asyncio.wait_for(reader.readuntil(b"\r\n"), timeout)
            if line in (b"\r\n", b"\n"):
                break
            k, _, v = line.decode("latin-1").partition(":")
            h[k.strip().lower()] = v.strip()
        if row["status"] != 200:
            row["err"] = (await asyncio.wait_for(reader.read(4096), timeout))[:400].decode("latin-1", "replace")
            row["total_ms"] = ms()
            return row

        chunked = "chunked" in h.get("transfer-encoding", "").lower()
        clen = int(h.get("content-length") or 0)

        async def pieces():
            """yield (data, ts) body pieces, honoring chunked/content-length framing."""
            buf = b""
            if chunked:
                while True:
                    line = await asyncio.wait_for(reader.readuntil(b"\r\n"), timeout)
                    n = int(line.strip().split(b";")[0] or b"0", 16)
                    if n == 0:
                        return
                    data = await asyncio.wait_for(reader.readexactly(n), timeout)
                    yield data, time.perf_counter()
                    await asyncio.wait_for(reader.readexactly(2), timeout)
            elif clen:
                remaining = clen
                while remaining > 0:
                    data = await asyncio.wait_for(reader.read(min(65536, remaining)), timeout)
                    if not data:
                        return
                    remaining -= len(data)
                    yield data, time.perf_counter()
            else:
                while True:
                    data = await asyncio.wait_for(reader.read(65536), timeout)
                    if not data:
                        return
                    yield data, time.perf_counter()

        buf = b""
        done = False
        async for data, when in pieces():
            buf += data
            while b"\n" in buf and not done:
                raw, buf = buf.split(b"\n", 1)
                raw = raw.rstrip(b"\r")
                if not raw.startswith(b"data: "):
                    continue
                if raw[6:] == b"[DONE]":
                    done = True
                    break
                try:
                    ev = json.loads(raw[6:])
                except Exception:
                    continue
                row["chunks"] += 1
                kind, tok = on_event(ev)
                fold_usage(row, ev)
                if tok:
                    row["content_chunks"] += 1
                    if kind == "thinking":
                        row["thinking_chunks"] += 1
                    elif kind == "tool":
                        row["tool_calls"] += 1
                    elif kind == "text":
                        row["text_len"] += 1
                    if row["ttfc_ms"] is None:
                        row["ttfc_ms"] = round((when - t0) * 1000, 2)
                    elif row["_last"] is not None:
                        row["gaps_ms"].append(round((when - row["_last"]) * 1000, 2))
                    row["_last"] = when
        row["total_ms"] = ms()
        if row["_last"] is not None:
            row["stream_ms"] = round((row["_last"] - t0) * 1000, 2)
    except asyncio.TimeoutError:
        row["err"] = "timeout"
        row["total_ms"] = ms()
    except Exception as e:  # noqa: BLE001
        row["err"] = "%s: %s" % (type(e).__name__, e)
        row["total_ms"] = ms()
    finally:
        if writer is not None:
            writer.close()
            try:
                await writer.wait_closed()
            except Exception:
                pass
    if row["gaps_ms"]:
        row["max_gap_ms"] = max(row["gaps_ms"])
    return row


def anthropic_event(ev):
    t = ev.get("type")
    if t == "message_start":
        u = (ev.get("message") or {}).get("usage") or {}
        return "start", None if not u.get("input_tokens") else "usage"
    if t == "content_block_start":
        cb = ev.get("content_block") or {}
        if cb.get("type") == "tool_use":
            return "tool", "tool"
        return "block", None
    if t == "content_block_delta":
        d = ev.get("delta") or {}
        if d.get("type") == "input_json_delta":
            return "tooljson", "tool"
        if d.get("type") == "thinking_delta":
            return "thinking", "thinking"
        if d.get("text"):
            return "text", "text"
        return "delta", None
    return t or "?", None


def openai_event(ev):
    ch = (ev.get("choices") or [{}])[0]
    d = ch.get("delta") or {}
    if d.get("content"):
        return "text", "text"
    if d.get("reasoning_content") or d.get("reasoning"):
        return "thinking", "thinking"
    if d.get("tool_calls"):
        return "tool", "tool"
    if ev.get("usage"):
        return "usage", None
    return "other", None


# ---------------------------------------------------------------- clients

MODELS = [
    "opencode/deepseek-v4.1-flash-free",
    "opencode/glm-5.3-flash-free",
    "opencode/mimo-v2.6-flash-free",
    "opencode/nemotron-3.5-lightning-free",
    "opencode/big-pickle",
    "opencode/longcat-2.5-preview-free",
    "opencode/nemotron-3.5-super-vl-free",
    "opencode/jev-1.13-free",
]


def fold_usage(row, ev):
    """pull token counts + stop reason out of either api's event shape."""
    u = ev.get("usage") or (ev.get("message") or {}).get("usage") or {}
    if u:
        if u.get("input_tokens") is not None:
            row["in_tok"] = u["input_tokens"]
        if u.get("output_tokens"):
            row["out_tok"] = u["output_tokens"]
        if u.get("prompt_tokens") is not None:
            row["in_tok"] = u["prompt_tokens"]
        if u.get("completion_tokens"):
            row["out_tok"] = u["completion_tokens"]
    sr = (ev.get("delta") or {}).get("stop_reason") or (ev.get("choices") or [{}])[0].get("finish_reason")
    if sr:
        row["stop_reason"] = sr
    return False


async def harness(client_id, args, results, sysp, out_lock):
    host, port = args.host, args.port
    rng = random.Random(1000 + client_id)
    api = ("anthropic" if args.api == "anthropic"
           else "openai" if args.api == "openai"
           else ("anthropic", "openai")[client_id % 2])
    path = "/v1/messages" if api == "anthropic" else "/v1/chat/completions"
    session = "bench-%s-%02d-%s" % (api, client_id, "".join(rng.choice("abcdef0123456789") for _ in range(8)))

    hist = make_history(args.history_turns, args.history_bytes, rng)

    pool = args.models or MODELS
    model = pool[client_id % len(pool)]  # stable per client, like a real session
    for rnd in range(args.rounds):
        max_tokens = args.max_tokens
        if api == "anthropic":
            payload = to_anthropic(hist, sysp, model, max_tokens)
            hdrs = {"Content-Type": "application/json", "x-session-id": session,
                    "anthropic-version": "2023-06-01", "user-agent": "claude-cli/2.1.300"}
        else:
            payload = to_openai(hist, sysp, model, max_tokens)
            hdrs = {"Content-Type": "application/json", "x-session-id": session,
                    "Authorization": "Bearer dummy", "user-agent": "openai-python/1.0"}

        row = {"client": client_id, "round": rnd, "api": api, "model": model,
               "t_start": round(time.time(), 3)}
        row.update(await stream_request(
            host, port, path, payload, hdrs, args.timeout,
            lambda ev: anthropic_event(ev) if api == "anthropic" else openai_event(ev)))
        row["t_end"] = round(time.time(), 3)
        row.pop("_last", None)
        async with out_lock:
            results.append(row)

        # grow the history like a real agent: append the turn back
        hist.append({"role": "user", "content": "follow up on round %d, keep going" % rnd})
        hist.append({"role": "assistant", "content": "done with round %d, next step is the error path" % rnd})


async def local_overhead(args, results, sysp, out_lock):
    """measure pure proxy processing: big bodies, no upstream."""
    rng = random.Random(7)
    for i in range(args.clients * 2):
        hist = make_history(args.history_turns, args.history_bytes, rng)
        payload = to_anthropic(hist, sysp, "opencode/deepseek-v4.1-flash-free", args.max_tokens)
        body = json.dumps(payload).encode()
        t0 = time.perf_counter()
        r = await raw_request(args.host, args.port, "/v1/messages/count_tokens", body,
                              {"Content-Type": "application/json"}, args.timeout)
        dt = (time.perf_counter() - t0) * 1000
        async with out_lock:
            results.append({"client": i, "api": "local", "req_bytes": len(body),
                            "status": r.status, "total_ms": round(dt, 2),
                            "rtt_ms": round(r.rtt_ms, 2), "body": r.body[:120].decode("utf-8", "replace")})


async def status_poller(args, statuses, stop):
    while not stop.is_set():
        try:
            r = await asyncio.wait_for(
                raw_request(args.host, args.port, "/status", b"", {}, 5, method="GET"), 6)
            try:
                statuses.append({"t": time.time(), "raw": json.loads(r.body.decode())})
            except Exception:
                pass
        except Exception:
            pass
        await asyncio.sleep(args.poll)


# ---------------------------------------------------------------- main

def pct(vals, p):
    vals = sorted(v for v in vals if v is not None)
    if not vals:
        return None
    return round(vals[max(0, min(len(vals) - 1, int(round(len(vals) * p)) - 1))], 1)


def summarize(rows, wall):
    good = [r for r in rows if r.get("status") == 200 and not r.get("err")]
    bad = [r for r in rows if r.get("status") != 200 or r.get("err")]
    ttfb = [r.get("ttfb_ms") for r in good]
    ttfc = [r.get("ttfc_ms") for r in good]
    total = [r.get("total_ms") for r in rows]
    gaps = [g for r in good for g in r.get("gaps_ms", [])]
    tps = []
    for r in good:
        tok = r.get("out_tok") or r.get("text_len")
        start = r.get("ttfc_ms")
        end = r.get("stream_ms") or r.get("total_ms")
        if tok and start is not None and end and end > start:
            tps.append(tok / ((end - start) / 1000.0))
    out = {
        "requests": len(rows), "ok": len(good), "failed": len(bad),
        "wall_s": round(wall, 1),
        "ttfb_ms": {"p50": pct(ttfb, .5), "p90": pct(ttfb, .9), "p99": pct(ttfb, .99), "max": max(ttfb) if ttfb else None},
        "ttfc_ms": {"p50": pct(ttfc, .5), "p90": pct(ttfc, .9), "p99": pct(ttfc, .99), "max": max(ttfc) if ttfc else None},
        "total_ms": {"p50": pct(total, .5), "p90": pct(total, .9), "p99": pct(total, .99), "max": max(total) if total else None},
        "chunk_gap_ms": {"p50": pct(gaps, .5), "p90": pct(gaps, .9), "p99": pct(gaps, .99), "max": max(gaps) if gaps else None},
        "tps": {"p50": pct(tps, .5), "p10": pct(tps, .1), "mean": round(statistics.fmean(tps), 2) if tps else None},
        "errors": [{"status": r.get("status"), "err": (r.get("err") or "")[:200]} for r in bad][:20],
    }
    return out


async def amain(args):
    sysp = make_system_prompt(args.prompt_bytes)
    print("system prompt: %d bytes; history: %d turns x ~%d bytes" %
          (len(sysp), args.history_turns, args.history_bytes), file=sys.stderr)
    results, statuses = [], []
    out_lock = asyncio.Lock()
    stop = asyncio.Event()
    t0 = time.perf_counter()
    tasks = []
    if args.mode == "local":
        await local_overhead(args, results, sysp, out_lock)
    else:
        tasks.append(asyncio.create_task(status_poller(args, statuses, stop)))
        tasks += [asyncio.create_task(harness(i, args, results, sysp, out_lock))
                  for i in range(args.clients)]
        await asyncio.gather(*tasks[1:])
    wall = time.perf_counter() - t0
    stop.set()
    for t in tasks:
        t.cancel()

    os.makedirs(args.out, exist_ok=True)
    with open(os.path.join(args.out, "requests.jsonl"), "w") as f:
        for r in results:
            f.write(json.dumps(r) + "\n")
    with open(os.path.join(args.out, "status.jsonl"), "w") as f:
        for s in statuses:
            f.write(json.dumps(s) + "\n")
    summary = summarize(results, wall)
    summary["config"] = vars(args)
    with open(os.path.join(args.out, "summary.json"), "w") as f:
        json.dump(summary, f, indent=2)
    print(json.dumps(summary, indent=2))
    return 0


def main():
    p = argparse.ArgumentParser(description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--host", default="127.0.0.1")
    p.add_argument("--port", type=int, default=5420)
    p.add_argument("--clients", type=int, default=12)
    p.add_argument("--rounds", type=int, default=3)
    p.add_argument("--api", choices=["anthropic", "openai", "mixed"], default="mixed")
    p.add_argument("--mode", choices=["proxy", "local"], default="proxy")
    p.add_argument("--prompt-bytes", type=int, default=24000)
    p.add_argument("--history-turns", type=int, default=8)
    p.add_argument("--history-bytes", type=int, default=3000)
    p.add_argument("--max-tokens", type=int, default=512)
    p.add_argument("--timeout", type=float, default=180)
    p.add_argument("--poll", type=float, default=1.0)
    p.add_argument("--models", nargs="*", default=None)
    p.add_argument("--out", default="bench-out")
    args = p.parse_args()
    if args.clients < 1 or args.clients > 64:
        p.error("clients must be 1..64")
    asyncio.run(amain(args))


if __name__ == "__main__":
    main()
