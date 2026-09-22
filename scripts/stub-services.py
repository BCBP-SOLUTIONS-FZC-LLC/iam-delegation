#!/usr/bin/env python3
"""
Lightweight HTTP stub for iam-delegation curl testing.

Stubs two upstream services on separate ports:
  :8081 — iam-user-profile
  :8082 — iam-org-membership

Endpoints served:

iam-org-membership (:8082)
  GET  /api/v1/internal/tenants/:tid/members/:uid/exists
       → 200 {"exists": true, "active": true}

iam-user-profile (:8081)
  GET  /api/v1/users/:uid/availability
       → 200 {"status": "available", "ooo_until": null}
  PUT  /api/v1/internal/users/:uid/availability
       → 200 {}
  DELETE /api/v1/internal/users/:uid/availability/delegate
       → 204

Usage:
  python3 scripts/stub-services.py
  # runs both stubs in background threads; Ctrl-C to stop
"""

import json
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer


class OrgMembershipStub(BaseHTTPRequestHandler):
    def do_GET(self):
        # GET /api/v1/internal/tenants/:tid/members/:uid/exists
        if "/members/" in self.path and self.path.endswith("/exists"):
            self._json(200, {"exists": True, "active": True})
        else:
            self._json(404, {"error": "not_found"})

    def _json(self, status, body):
        payload = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, fmt, *args):
        print(f"[org-membership :8082] {fmt % args}")


class UserProfileStub(BaseHTTPRequestHandler):
    def do_GET(self):
        # GET /api/v1/users/:uid/availability
        if "/availability" in self.path:
            self._json(200, {"status": "available", "ooo_until": None})
        else:
            self._json(404, {"error": "not_found"})

    def do_PUT(self):
        # PUT /api/v1/internal/users/:uid/availability
        length = int(self.headers.get("Content-Length", 0))
        self.rfile.read(length)
        self._json(200, {})

    def do_DELETE(self):
        # DELETE /api/v1/internal/users/:uid/availability/delegate
        self.send_response(204)
        self.end_headers()

    def _json(self, status, body):
        payload = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, fmt, *args):
        print(f"[user-profile   :8081] {fmt % args}")


def run(handler, port):
    server = HTTPServer(("0.0.0.0", port), handler)
    print(f"  stub listening on :{port}")
    server.serve_forever()


if __name__ == "__main__":
    print("Starting stubs...")
    threading.Thread(target=run, args=(OrgMembershipStub, 8082), daemon=True).start()
    threading.Thread(target=run, args=(UserProfileStub,   8081), daemon=True).start()
    print("  iam-org-membership → :8082")
    print("  iam-user-profile   → :8081")
    print("Press Ctrl-C to stop.\n")
    try:
        threading.Event().wait()
    except KeyboardInterrupt:
        print("\nStopped.")
