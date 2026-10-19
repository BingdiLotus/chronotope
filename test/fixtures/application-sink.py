#!/usr/bin/env python3
"""Test-only application receiver and OpenAI-compatible scripted HTTP model.

This is a counting fixture, not Bots product code. SQLite owns its scoped random
test values, operation receipts, pending timer submissions and accepted fires.
Run with APPLICATION_SERVICE_TOKEN and APPLICATION_E2E_TENANT_KEY in the environment.
"""

import argparse
import datetime as dt
import hashlib
import json
import os
import secrets
import socket
import sqlite3
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


def encoded(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False)


def digest(value):
    return hashlib.sha256(encoded(value).encode()).hexdigest()


class Fixture:
    def __init__(self, args):
        self.args = args
        self.token = os.environ.get("APPLICATION_SERVICE_TOKEN", "")
        if not self.token:
            raise ValueError("APPLICATION_SERVICE_TOKEN must be set")
        self.stop = threading.Event()
        self.tools_released = threading.Event()
        self.tools_released.set()
        self.lock = threading.Lock()
        with self.db() as db:
            db.executescript("""
              PRAGMA journal_mode=WAL;
              CREATE TABLE IF NOT EXISTS operations(
                operation_id TEXT PRIMARY KEY, request_hash TEXT NOT NULL,
                tool_ref TEXT NOT NULL, context_json TEXT NOT NULL,
                result_json TEXT NOT NULL);
              CREATE TABLE IF NOT EXISTS test_values(
                tenant TEXT,subject TEXT,scope TEXT,value_json TEXT NOT NULL,
                PRIMARY KEY(tenant,subject,scope));
              CREATE TABLE IF NOT EXISTS timer_outbox(
                timer_id TEXT PRIMARY KEY,request_json TEXT NOT NULL,
                accepted INTEGER NOT NULL DEFAULT 0,last_error TEXT NOT NULL DEFAULT '');
              CREATE TABLE IF NOT EXISTS fires(
                fire_id TEXT PRIMARY KEY,payload_hash TEXT NOT NULL,
                payload_json TEXT NOT NULL,attempts INTEGER NOT NULL DEFAULT 1);
              CREATE TABLE IF NOT EXISTS model_requests(
                id INTEGER PRIMARY KEY,tools_count INTEGER NOT NULL,
                results_count INTEGER NOT NULL,verified INTEGER NOT NULL);
              CREATE TABLE IF NOT EXISTS settings(name TEXT PRIMARY KEY,value TEXT NOT NULL);
            """)

    def db(self):
        db = sqlite3.connect(self.args.db, timeout=10)
        db.row_factory = sqlite3.Row
        db.execute("PRAGMA synchronous=FULL")
        return db

    def call_tool(self, body):
        context = body.get("context", {})
        if (context.get("tenant_id"), context.get("subject_ref"), context.get("scope_id")) != (
            self.args.tenant, self.args.subject, self.args.scope
        ):
            return 403, {"error": "fixture context binding mismatch"}
        op = context.get("operation_id")
        if not all(context.get(k) for k in ("operation_id", "origin_command_id", "run_id", "tool_call_id")):
            return 400, {"error": "missing trusted context"}
        if not self.tools_released.wait(timeout=20):
            return 503, {"error": "fixture tools remain held"}
        request_hash = digest(body)
        tool, arguments = body.get("tool_ref"), body.get("arguments", {})
        with self.lock, self.db() as db:
            old = db.execute("SELECT request_hash,result_json FROM operations WHERE operation_id=?", (op,)).fetchone()
            if old:
                return (200, json.loads(old["result_json"])) if old["request_hash"] == request_hash else (409, {"error": "operation payload conflict"})
            key = (self.args.tenant, self.args.subject, self.args.scope)
            if tool == "remember":
                result = {"nonce": secrets.token_hex(16), "source": context["origin_command_id"]}
                db.execute("INSERT INTO test_values VALUES(?,?,?,?) ON CONFLICT(tenant,subject,scope) DO UPDATE SET value_json=excluded.value_json", (*key, encoded(result)))
            elif tool == "recall":
                stored = db.execute("SELECT value_json FROM test_values WHERE tenant=? AND subject=? AND scope=?", key).fetchone()
                result = json.loads(stored[0]) if stored else {"error": "no scoped test value"}
            elif tool == "create_reminder":
                timer_id = "fixture_" + digest([*key, context["origin_command_id"]])[:24]
                request = {"tenant_id": key[0], "subject_ref": key[1], "scope_id": key[2],
                           "timer_id": timer_id, "revision": 1, "due_at": arguments["due_at"],
                           "opaque_action_ref": "fixture-action:" + timer_id}
                result = {"timer_id": timer_id, "revision": 1, "due_at": request["due_at"], "state": "pending"}
                db.execute("INSERT INTO timer_outbox(timer_id,request_json) VALUES(?,?) ON CONFLICT(timer_id) DO NOTHING", (timer_id, encoded(request)))
            else:
                return 400, {"error": "unsupported fixture tool"}
            db.execute("INSERT INTO operations VALUES(?,?,?,?,?)", (op, request_hash, tool, encoded(context), encoded(result)))
        return 200, result

    def flush_timers(self):
        while not self.stop.wait(0.2):
            with self.db() as db:
                rows = db.execute("SELECT timer_id,request_json FROM timer_outbox WHERE accepted=0").fetchall()
            for row in rows:
                with self.db() as db:
                    configured=db.execute("SELECT value FROM settings WHERE name='tenant_key'").fetchone()
                key = configured[0] if configured else os.environ.get("APPLICATION_E2E_TENANT_KEY", "")
                req = urllib.request.Request(self.args.api.rstrip("/") + "/timers", data=row["request_json"].encode(),
                                             headers={"Content-Type": "application/json", "Authorization": "Bearer " + key})
                try:
                    with urllib.request.urlopen(req, timeout=10) as response:
                        if response.status != 202:
                            raise ValueError("timer admission did not return 202")
                    with self.db() as db:
                        db.execute("UPDATE timer_outbox SET accepted=1,last_error='' WHERE timer_id=?", (row["timer_id"],))
                except (OSError, ValueError) as exc:
                    # Retain responsibility; no due-task scanner or final send.
                    with self.db() as db:
                        db.execute("UPDATE timer_outbox SET last_error=? WHERE timer_id=?", (type(exc).__name__, row["timer_id"]))

    def accept_fire(self, body):
        if body.get("tenant_id") != self.args.tenant:
            return 403, {"error": "fire tenant mismatch"}, False
        with self.lock, self.db() as db:
            expected = db.execute("SELECT request_json FROM timer_outbox WHERE timer_id=?", (body.get("timer_id"),)).fetchone()
            if not expected:
                return 404, {"error": "unknown fixture action"}, False
            request = json.loads(expected[0])
            if any(body.get(k) != request[k] for k in ("tenant_id", "timer_id", "revision", "opaque_action_ref")) or not body.get("fire_id"):
                return 409, {"error": "fire payload mismatch"}, False
            old = db.execute("SELECT payload_hash FROM fires WHERE fire_id=?", (body["fire_id"],)).fetchone()
            if old and old[0] != digest(body):
                return 409, {"error": "fire identity conflict"}, False
            db.execute("INSERT INTO fires(fire_id,payload_hash,payload_json) VALUES(?,?,?) ON CONFLICT(fire_id) DO UPDATE SET attempts=attempts+1", (body["fire_id"], digest(body), encoded(body)))
        # Transaction committed before optional connection loss. Retry is a new
        # HTTP request but the same durable, countable application event.
        return 202, {"accepted": True, "fire_id": body["fire_id"]}, not old and self.args.drop_first_ack

    def state(self):
        with self.db() as db:
            return {"operations": [dict(r) for r in db.execute("SELECT * FROM operations")],
                    "timers": [dict(r) for r in db.execute("SELECT * FROM timer_outbox")],
                    "fires": [dict(r) for r in db.execute("SELECT * FROM fires")],
                    "model_requests": [dict(r) for r in db.execute("SELECT * FROM model_requests")]}

    def model_turn(self, body):
        messages = body.get("messages", [])
        last_user = max((i for i, m in enumerate(messages) if m.get("role") == "user"), default=-1)
        user = messages[last_user].get("content", "") if last_user >= 0 else ""
        if not isinstance(user, str):
            user = encoded(user)
        current = messages[last_user + 1:]
        results = [json.loads(m["content"]) for m in current if m.get("role") == "tool"]
        calls = []
        verified = False
        if not body.get("tools"):
            final = "Application runtime HTTP fixture: completed."
        elif not results:
            time.sleep(self.args.chat_delay)
            if "RECALL" in user:
                calls = [("recall", {"query": "scoped runtime nonce"})]
            else:
                due = dt.datetime.now(dt.timezone.utc) + dt.timedelta(seconds=self.args.timer_delay)
                calls = [("remember", {"content": "runtime fixture value", "kind": "test"}),
                         ("create_reminder", {"due_at": due.isoformat(), "text": "test-only durable wake-up"})]
            final = None
        else:
            nonce = next((r.get("nonce") for r in results if isinstance(r, dict) and r.get("nonce")), None)
            has_timer = any(isinstance(r, dict) and r.get("timer_id") for r in results)
            verified = bool(nonce and ("RECALL" in user or has_timer and len(results) == 2))
            if not verified:
                raise ValueError("model did not receive all actual tool outputs")
            final = "verified nonce=" + nonce
        with self.db() as db:
            db.execute("INSERT INTO model_requests(tools_count,results_count,verified) VALUES(?,?,?)", (len(body.get("tools", [])), len(results), int(verified)))
        if calls:
            return {"tool_calls": [{"index": i, "id": "fixture_" + name, "type": "function",
                                    "function": {"name": name, "arguments": encoded(arguments)}} for i, (name, arguments) in enumerate(calls)]}, "tool_calls"
        return {"content": final}, "stop"


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def reply(self, status, body):
        payload = encoded(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        # A fire must start on a fresh TCP connection. Otherwise Go's Transport
        # can replay an idempotent POST on a reused connection after lost ACK,
        # hiding the Restate-level retry that this fixture is meant to prove.
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(payload)
        self.close_connection = True

    def authorized(self):
        return self.headers.get("Authorization") == "Bearer " + self.server.fixture.token

    def do_GET(self):
        if self.path == "/healthz":
            return self.reply(200, {"status": "alive", "test_fixture": True})
        if not self.authorized():
            return self.reply(401, {"error": "invalid service token"})
        if self.path == "/state":
            return self.reply(200, self.server.fixture.state())
        return self.reply(404, {"error": "unknown path"})

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        if length <= 0 or length > 1 << 20:
            return self.reply(400, {"error": "invalid request length"})
        try:
            body = json.loads(self.rfile.read(length))
            if self.path == "/v1/chat/completions":
                delta, reason = self.server.fixture.model_turn(body)
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Connection", "close")
                self.end_headers()
                chunk = {"id": "fixture-chat", "object": "chat.completion.chunk", "created": int(time.time()),
                         "model": body.get("model", "application-fixture"),
                         "choices": [{"index": 0, "delta": delta, "finish_reason": None}]}
                self.wfile.write(("data: " + encoded(chunk) + "\n\n").encode())
                chunk["choices"] = [{"index": 0, "delta": {}, "finish_reason": reason}]
                self.wfile.write(("data: " + encoded(chunk) + "\n\n").encode())
                chunk["choices"] = []
                chunk["usage"] = {"prompt_tokens": 10, "completion_tokens": 10, "total_tokens": 20}
                self.wfile.write(("data: " + encoded(chunk) + "\n\ndata: [DONE]\n\n").encode())
                self.wfile.flush()
                self.close_connection = True
                return
            if not self.authorized():
                return self.reply(401, {"error": "invalid service token"})
            if self.path == "/configure":
                key=body.get("tenant_key")
                if key is not None:
                    if not isinstance(key,str) or not key:
                        return self.reply(400,{"error":"tenant_key must be nonempty"})
                    with self.server.fixture.db() as db:
                        db.execute("INSERT INTO settings VALUES('tenant_key',?) ON CONFLICT(name) DO UPDATE SET value=excluded.value",(key,))
                hold=body.get("hold_tools")
                if hold is not None:
                    if not isinstance(hold,bool):
                        return self.reply(400,{"error":"hold_tools must be boolean"})
                    if hold:self.server.fixture.tools_released.clear()
                    else:self.server.fixture.tools_released.set()
                return self.reply(200,{"configured":True})
            if self.path == "/tools":
                if self.headers.get("Idempotency-Key") != body.get("context", {}).get("operation_id"):
                    return self.reply(400, {"error": "missing operation identity header"})
                status, result = self.server.fixture.call_tool(body)
                return self.reply(status, result)
            if self.path == "/fire":
                if self.headers.get("Idempotency-Key") != body.get("fire_id"):
                    return self.reply(400, {"error": "missing fire identity header"})
                status, result, drop = self.server.fixture.accept_fire(body)
                if drop:
                    self.close_connection = True
                    self.connection.shutdown(socket.SHUT_RDWR)
                    self.connection.close()
                    return
                return self.reply(status, result)
            return self.reply(404, {"error": "unknown path"})
        except (ValueError, KeyError, TypeError) as exc:
            return self.reply(400, {"error": str(exc)})


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host", default="0.0.0.0")
    parser.add_argument("--port", type=int, default=18089)
    parser.add_argument("--db", required=True)
    parser.add_argument("--api", default="http://localhost:8080")
    parser.add_argument("--tenant", default="application-e2e")
    parser.add_argument("--subject", default="app-e2e")
    parser.add_argument("--scope", default="channel-e2e")
    parser.add_argument("--timer-delay", type=float, default=45)
    parser.add_argument("--chat-delay", type=float, default=0)
    parser.add_argument("--drop-first-ack", action="store_true")
    args = parser.parse_args()
    fixture = Fixture(args)
    server = ThreadingHTTPServer((args.host, args.port), Handler)
    server.daemon_threads = True
    server.fixture = fixture
    threading.Thread(target=fixture.flush_timers, daemon=True).start()
    try:
        server.serve_forever(poll_interval=0.2)
    finally:
        fixture.stop.set()
        server.server_close()


if __name__ == "__main__":
    main()
