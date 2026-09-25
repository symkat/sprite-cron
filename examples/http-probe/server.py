#!/usr/bin/env python3
"""Small Sprite HTTP probe; authentication is enforced by the Sprite proxy."""

import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/":
            status, body = 200, {"service": "sprite-cron-http", "ok": True}
        elif self.path == "/health":
            status, body = 200, {"status": "ok"}
        else:
            status, body = 404, {"error": "not found"}
        data = json.dumps(body).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, format, *args):
        # Do not log request paths or headers, which might contain credentials.
        pass


if __name__ == "__main__":
    server = ThreadingHTTPServer(("0.0.0.0", 8080), Handler)
    print("sprite-cron-http listening on 0.0.0.0:8080", flush=True)
    server.serve_forever()
