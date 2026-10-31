#!/usr/bin/env python3
"""Stand-ins for the smoke test (pool/k8s/README.md), selected by argv[1]:

  upstream <port>   fake Multica: register / heartbeat / claim / task reports.
                    POST /_admin/enqueue?id=T queues ONE fake task with an
                    initiator email; GET /_admin/state shows what arrived.
  litellm <port>    fake LiteLLM: /user/info, /key/generate, /key/delete.
  other <port>      a destination the pool must NOT reach (answers 200 to all).

Test-only; holds no secrets.
"""
import json
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MODE, PORT = sys.argv[1], int(sys.argv[2])
WORKSPACE_ID = "11111111-2222-4333-8444-555555555555"
lock = threading.Lock()
state = {"registrations": 0, "queue": [], "claims": 0, "taskCalls": [], "keys": [], "deleted": []}


class H(BaseHTTPRequestHandler):
    def log_message(self, fmt, *a):
        print(self.address_string(), self.command, self.path, flush=True)

    def send(self, status, body=None):
        raw = json.dumps({} if body is None else body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def body(self):
        n = int(self.headers.get("Content-Length") or 0)
        try:
            return json.loads(self.rfile.read(n) or b"{}") if n else {}
        except ValueError:
            return {}

    def do_GET(self):
        path = self.path.split("?")[0]
        if path in ("/healthz", "/"):
            return self.send(200, {"ok": True})
        if path == "/_admin/state":
            with lock:
                return self.send(200, state)
        if MODE == "litellm" and path == "/user/info":
            return self.send(200, {"user_info": {"max_budget": 100, "spend": 0}})
        self.send(200, {})

    def do_POST(self):
        path, _, query = self.path.partition("?")
        b = self.body()
        if MODE == "other":
            return self.send(200)
        if MODE == "litellm":
            with lock:
                if path == "/key/generate":
                    k = "sk-fake-%d" % (len(state["keys"]) + 1)
                    state["keys"].append({"key": k, "user_id": b.get("user_id"), "alias": b.get("key_alias")})
                    return self.send(200, {"key": k})
                if path == "/key/delete":
                    state["deleted"].extend(b.get("keys", []))
            return self.send(200)
        # upstream
        if path == "/_admin/enqueue":
            tid = dict(p.split("=") for p in query.split("&")).get("id", "task-1")
            with lock:
                state["queue"].append({
                    "id": tid, "agent_id": "agent-1", "runtime_id": "runtime-0",
                    "workspace_id": WORKSPACE_ID, "issue_id": "22222222-2222-4333-8444-555555555555",
                    "initiator_email": "alice@example.com",
                    "agent": {"id": "agent-1", "name": "smoke-agent", "instructions": "say hi"},
                })
            return self.send(200)
        if path == "/api/daemon/register":
            with lock:
                state["registrations"] += 1
            rts = [{"id": "runtime-%d" % i, "provider": r.get("type"), "name": r.get("name")}
                   for i, r in enumerate(b.get("runtimes", []))]
            return self.send(200, {"runtimes": rts})
        if path == "/api/daemon/heartbeat":
            return self.send(200, {"status": "ok"})
        if path == "/api/daemon/tasks/claim":
            tasks = []
            with lock:
                state["claims"] += 1
                if state["queue"] and b.get("max_tasks", 1) > 0:
                    tasks.append(state["queue"].pop(0))
            return self.send(200, {"tasks": tasks})
        if path.startswith("/api/daemon/tasks/"):
            with lock:
                state["taskCalls"].append(path[len("/api/daemon/tasks/"):])
        self.send(200, {"status": "ok"})


ThreadingHTTPServer(("0.0.0.0", PORT), H).serve_forever()
