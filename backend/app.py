"""Minimal HTTP entry point. Credit decisions are intentionally not implemented."""

from http.server import BaseHTTPRequestHandler, HTTPServer
import json


APPLICATION_STATUSES = (
    "draft", "submitted", "processing", "approved", "counter_offer",
    "manual_review", "rejected", "signed", "closed",
)


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path != "/health":
            self.send_error(404)
            return
        payload = json.dumps({"status": "ok", "service": "backend"}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


if __name__ == "__main__":
    HTTPServer(("127.0.0.1", 8000), Handler).serve_forever()
