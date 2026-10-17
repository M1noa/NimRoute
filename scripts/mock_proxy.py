#!/usr/bin/env python3
"""mock_proxy.py - local forward proxy for pool-recovery testing.

Speaks plain HTTP proxying (absolute-URI request line) and forwards to a local
mock zen. Answers real inference POSTs with 429 until a global budget is spent,
while GET /models (the pool verify probe) always succeeds: that is exactly the
"exits still verify but refuse to serve" shape the ladder extension targets.

usage:
  python3 scripts/mock_proxy.py --ports 5601,5602 --upstream http://127.0.0.1:5599 --posts-429 12
"""
import argparse
import http.client
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit

upstream = ""
post_budget = 0
state = threading.Lock()
posts = 0


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def _upstream(self):
        u = urlsplit(self.path)
        return u.path or "/"

    def _forward(self, method):
        global posts
        n = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(n) if n else b""
        if method == "POST":
            with state:
                posts += 1
                n = posts
            if n <= post_budget:
                msg = b'{"error":{"message":"rate limited","code":"rate_limited"}}'
                self.send_response(429)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(msg)))
                self.end_headers()
                self.wfile.write(msg)
                return
        host = urlsplit(upstream)
        conn = http.client.HTTPConnection(host.hostname, host.port or 80, timeout=30)
        headers = {k: v for k, v in self.headers.items()
                   if k.lower() not in ("host", "content-length", "connection", "proxy-connection")}
        headers["Host"] = "%s:%d" % (host.hostname, host.port or 80)
        conn.request(method, self._upstream(), body=body or None, headers=headers)
        resp = conn.getresponse()
        payload = resp.read()
        self.send_response(resp.status)
        for k, v in resp.getheaders():
            if k.lower() in ("transfer-encoding", "connection", "content-length"):
                continue
            self.send_header(k, v)
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)
        conn.close()

    def do_GET(self):
        self._forward("GET")

    def do_POST(self):
        self._forward("POST")


def main():
    global upstream, post_budget
    ap = argparse.ArgumentParser()
    ap.add_argument("--ports", required=True)
    ap.add_argument("--upstream", default="http://127.0.0.1:5599")
    ap.add_argument("--posts-429", type=int, default=0)
    a = ap.parse_args()
    upstream = a.upstream
    post_budget = a.posts_429
    servers = []
    for p in [int(x) for x in a.ports.split(",")]:
        s = ThreadingHTTPServer(("127.0.0.1", p), Handler)
        servers.append(s)
        threading.Thread(target=s.serve_forever, daemon=True).start()
    print("proxies up on %s -> %s (post budget %d)" % (a.ports, upstream, post_budget), flush=True)
    threading.Event().wait()


if __name__ == "__main__":
    main()
