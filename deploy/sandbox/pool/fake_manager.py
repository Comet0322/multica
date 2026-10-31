#!/usr/bin/env python3
"""Stand-in for the pool manager, used by verify.sh to exercise the pod image.

It answers the calls a daemon makes at startup (token renewal, workspace list,
runtime registration, heartbeat, task claim) and serves the supervisor's
control channel. Admin endpoints, for the test script only:

  POST /_admin/reset?id=r-1   queue a reset command
  GET  /_admin/state          registrations, heartbeats and reset reports
"""
import json
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

WORKSPACE_ID = "11111111-2222-4333-8444-555555555555"

lock = threading.Lock()
state = {"registrations": [], "heartbeats": 0, "commands": [], "reports": [], "calls": []}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def _send(self, status, body=None):
        raw = b"" if body is None else json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def _body(self):
        n = int(self.headers.get("Content-Length") or 0)
        return json.loads(self.rfile.read(n) or b"{}") if n else {}

    def do_GET(self):
        path = self.path.split("?")[0]
        with lock:
            state["calls"].append("GET " + path)
        if path == "/_admin/state":
            with lock:
                return self._send(200, state)
        if path == "/api/pod/control":
            with lock:
                done = {r["id"] for r in state["reports"]}
                pending = [c for c in state["commands"] if c["id"] not in done]
            return self._send(200, pending[0]) if pending else self._send(204)
        if path == "/api/workspaces":
            return self._send(200, [{"id": WORKSPACE_ID, "name": "pool-test", "slug": "pool-test"}])
        self._send(404, {"error": "not found"})

    def do_POST(self):
        path, _, query = self.path.partition("?")
        body = self._body()
        with lock:
            state["calls"].append("POST " + path)
        if path == "/_admin/reset":
            cid = dict(p.split("=") for p in query.split("&")).get("id", "r-1")
            with lock:
                state["commands"].append({"id": cid, "command": "reset"})
            return self._send(200, {})
        if path == "/api/pod/control":
            with lock:
                state["reports"].append(body)
            return self._send(200, {})
        if path == "/api/tokens/current/renew":
            return self._send(200, {})
        if path == "/api/daemon/register":
            with lock:
                state["registrations"].append(body)
            runtimes = [
                {"id": "runtime-%d" % i, "provider": r.get("type"), "name": r.get("name")}
                for i, r in enumerate(body.get("runtimes", []))
            ]
            return self._send(200, {"runtimes": runtimes})
        if path == "/api/daemon/heartbeat":
            with lock:
                state["heartbeats"] += 1
            return self._send(200, {"status": "ok"})
        if path == "/api/daemon/tasks/claim":
            return self._send(200, {"tasks": []})
        if path.endswith("/tasks/claim"):
            return self._send(200, {"task": None})
        self._send(200, {})


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", int(sys.argv[1])), Handler).serve_forever()
