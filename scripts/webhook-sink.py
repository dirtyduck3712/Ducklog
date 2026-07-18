#!/usr/bin/env python3
# webhook-sink.py — 測試用:收 Alertmanager webhook POST,把 body 追加寫到 /out/hits.log。
from http.server import BaseHTTPRequestHandler, HTTPServer

class H(BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(n)
        with open("/out/hits.log", "ab") as f:
            f.write(body + b"\n")
        self.send_response(200); self.end_headers()
    def log_message(self, *a): pass

HTTPServer(("0.0.0.0", 8080), H).serve_forever()
