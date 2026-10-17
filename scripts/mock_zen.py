#!/usr/bin/env python3
"""mock_zen.py - local stand-in for opencode zen, for measuring proxy overhead.

Answers /models and /chat/completions with valid OpenAI-shaped SSE, streaming N
chunks at a fixed rate. Any latency it adds is under our control, so whatever
is left in the client's wall clock is the proxy's own cost.

usage:
  python3 scripts/mock_zen.py --port 5599 --chunks 40 --gap-ms 0 --ttft-ms 0
"""
import argparse
import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

posts = 0
posts_429 = 0
state = threading.Lock()


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    chunks = 40
    gap_ms = 0
    ttft_ms = 0
    max_chars = 0
    dump_path = ""

    def setup(self):
        # nagle would add ~40ms per write and pollute the measurement
        super().setup()
        import socket
        self.connection.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)

    def log_message(self, *a):
        pass

    def _read_body(self):
        n = int(self.headers.get("Content-Length") or 0)
        return self.rfile.read(n) if n else b""

    def do_GET(self):
        if self.path.endswith("/models"):
            # object:list + a -free id: nimroute's pool verify probe and its
            # free-model filter both check for these.
            body = json.dumps({"object": "list", "data": [
                {"id": "mock-free", "object": "model"},
                {"id": "big-pickle", "object": "model"}]}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        self.send_error(404)

    def do_POST(self):
        global posts
        raw = self._read_body()
        if posts_429:
            with state:
                posts += 1
                n = posts
            if n <= posts_429:
                msg = b'{"error":{"message":"rate limited","code":"rate_limited"}}'
                self.send_response(429)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(msg)))
                self.end_headers()
                self.wfile.write(msg)
                return
        try:
            req = json.loads(raw)
        except Exception:
            req = {}
        if Handler.dump_path:
            with open(Handler.dump_path, "a") as fh:
                fh.write(json.dumps(req.get("messages", req)) + "\n")
        if self.max_chars and len(raw) > self.max_chars:
            pass  # still answer; size is the client's problem, not ours
        if self.ttft_ms:
            time.sleep(self.ttft_ms / 1000.0)
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()

        def chunk(payload):
            b = payload.encode()
            self.wfile.write(b"%x\r\n" % len(b) + b + b"\r\n")
            self.wfile.flush()

        chunk('data: {"id":"mock","object":"chat.completion.chunk","choices":[{"index":0,'
              '"delta":{"role":"assistant","content":""},"finish_reason":null}]}\n\n')
        for i in range(self.chunks):
            if self.gap_ms:
                time.sleep(self.gap_ms / 1000.0)
            chunk('data: {"id":"mock","object":"chat.completion.chunk","choices":[{"index":0,'
                  '"delta":{"content":"tok%d "},"finish_reason":null}]}\n\n' % i)
        chunk('data: {"id":"mock","object":"chat.completion.chunk","choices":[{"index":0,'
              '"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,'
              '"completion_tokens":%d,"total_tokens":%d}}\n\n' % (self.chunks, self.chunks + 100))
        chunk("data: [DONE]\n\n")
        self.wfile.write(b"0\r\n\r\n")
        self.wfile.flush()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=5599)
    ap.add_argument("--posts-429", type=int, default=0)
    ap.add_argument("--chunks", type=int, default=40)
    ap.add_argument("--gap-ms", type=float, default=0)
    ap.add_argument("--ttft-ms", type=float, default=0)
    ap.add_argument("--max-chars", type=int, default=0)
    ap.add_argument("--dump", default="", help="append received messages json per request")
    a = ap.parse_args()
    Handler.chunks, Handler.gap_ms = a.chunks, a.gap_ms
    Handler.ttft_ms, Handler.max_chars = a.ttft_ms, a.max_chars
    Handler.dump_path = a.dump
    global posts_429
    posts_429 = a.posts_429
    srv = ThreadingHTTPServer(("127.0.0.1", a.port), Handler)
    print("mock zen on :%d  chunks=%d gap=%sms ttft=%sms" % (a.port, a.chunks, a.gap_ms, a.ttft_ms))
    srv.serve_forever()


if __name__ == "__main__":
    main()
