#!/usr/bin/env python3
"""C3 application integration gate (test receiver, not the Bots product).

Default: start the SQLite fixture, build/up api/worker/harness, register worker,
exercise authenticated HTTP + real Python provider against a fake HTTP model,
then restart fixture and worker while a timer waits. Keeps SQLite/report artifacts.
--mode real uses the existing LiteLLM route and labels evidence separately.
--no-build reuses images; --no-compose requires an already matching deployment.
Never uses down -v. API_AUTH_MODE is on; bootstrap/bridge test tokens are separate.
"""

import argparse
import datetime as dt
import json
import os
from pathlib import Path
import secrets
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request


ROOT = Path(__file__).resolve().parents[2]
TENANT, SUBJECT, SCOPE = "application-e2e", "app-e2e", "channel-e2e"


def request(base, path, body=None, key="", method=None, idem=None, expected=(200,), timeout=30):
    headers = {"Accept": "application/json"}
    if key:
        headers["Authorization"] = "Bearer " + key
    if idem:
        headers["Idempotency-Key"] = idem
    if body is not None:
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(base.rstrip("/") + path, data=None if body is None else json.dumps(body).encode(),
                                 headers=headers, method=method or ("POST" if body is not None else "GET"))
    try:
        with urllib.request.urlopen(req, timeout=timeout) as response:
            status, data = response.status, response.read()
    except urllib.error.HTTPError as exc:
        status, data = exc.code, exc.read()
    result = json.loads(data) if data else {}
    if status not in expected:
        raise AssertionError(f"{req.method} {path}: HTTP {status}, expected {expected}: {result}")
    return result


def wait_for(label, fn, timeout=120):
    deadline = time.monotonic() + timeout
    last_error = None
    while time.monotonic() < deadline:
        try:
            value = fn()
            if value:
                return value
        except (OSError, AssertionError) as exc:
            last_error = exc
        time.sleep(0.3)
    raise AssertionError(f"timed out waiting for {label}: {last_error}")


def sleep_evidence(admin_url, timer_handle, due_at, pending=True):
    """Read actual server journal entries, not the application's POST receipt."""
    target = "application_timer/" + timer_handle + "/run"
    quoted_target = target.replace("'", "''")
    rows = request(admin_url, "/query", {"query":
        "SELECT id,target,status FROM sys_invocation WHERE target='" + quoted_target + "'"})["rows"]
    if len(rows) != 1 or pending and rows[0]["status"] not in ("running", "suspended"):
        return None
    invocation = rows[0]
    invocation_id = invocation["id"].replace("'", "''")
    journal = request(admin_url, "/query", {"query":
        "SELECT index,entry_type,name,entry_json FROM sys_journal WHERE id='" + invocation_id +
        "' AND (entry_type='Command: Sleep' OR entry_type='Notification: Sleep') ORDER BY index"})["rows"]
    sleeps = [entry for entry in journal if entry.get("name") == "timer-until-due" and entry["entry_type"] == "Command: Sleep"]
    if len(sleeps) != 1:
        return None
    entry = sleeps[0]
    sleep = json.loads(entry["entry_json"])["Command"]["Sleep"]
    completed = any(json.loads(row["entry_json"]).get("Notification", {}).get("Completion", {}).get("Sleep", {}).get("completion_id")
                    == sleep["completion_id"] for row in journal if row["entry_type"] == "Notification: Sleep")
    observed = dt.datetime.now(dt.timezone.utc)
    wake_at = dt.datetime.fromtimestamp(sleep["wake_up_time"] / 1000, tz=dt.timezone.utc)
    if pending and (completed or wake_at <= observed or due_at <= observed):
        return None
    assert abs((wake_at - due_at).total_seconds()) < 1, "Sleep deadline drifted from absolute due_at"
    return {"invocation_id": invocation["id"], "target": target, "status": invocation["status"],
            "sleep_index": entry["index"], "sleep_name": entry["name"], "completion_id": sleep["completion_id"],
            "wake_up_time": sleep["wake_up_time"], "sleep_completed": completed,
            "due_at": due_at.isoformat(), "observed_at": observed.isoformat()}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--api", default=os.environ.get("API_URL", "http://localhost:8080"))
    parser.add_argument("--mode", choices=("fake", "real"), default="fake")
    parser.add_argument("--model", default=None)
    parser.add_argument("--fixture-port", type=int, default=18089)
    parser.add_argument("--state-dir", type=Path)
    parser.add_argument("--timer-delay", type=float, default=45)
    parser.add_argument("--no-compose", action="store_true")
    parser.add_argument("--no-build", action="store_true")
    parser.add_argument("--skip-restart", action="store_true", help="debug only; report restart verification skipped")
    args = parser.parse_args()
    state_dir = args.state_dir or Path(tempfile.mkdtemp(prefix="chronotope-application-runtime-"))
    state_dir.mkdir(parents=True, exist_ok=True)
    env = os.environ.copy()
    env["API_AUTH_MODE"] = "on"
    env.setdefault("API_ADMIN_KEY", "application-e2e-bootstrap-test-only")
    env.setdefault("APPLICATION_SERVICE_TOKEN", "application-e2e-service-test-only")
    service_token, admin_key = env["APPLICATION_SERVICE_TOKEN"], env["API_ADMIN_KEY"]
    fixture_url = f"http://127.0.0.1:{args.fixture_port}"
    docker_fixture = f"http://host.docker.internal:{args.fixture_port}"
    env["APPLICATION_BRIDGES_JSON"] = json.dumps([{"ref": "application-e2e", "tenant_id": TENANT,
        "url": docker_fixture + "/tools", "token_env": "APPLICATION_SERVICE_TOKEN"}])
    env["APPLICATION_TIMER_TARGETS_JSON"] = json.dumps([{"ref": "application-e2e-fire", "tenant_id": TENANT,
        "subject_ref": SUBJECT, "scope_id": SCOPE, "callback_url": docker_fixture + "/fire", "token_env": "APPLICATION_SERVICE_TOKEN"}])
    env["HARNESS_FAKE_MODEL"] = "0"
    env["LITELLM_BASE_URL"] = docker_fixture + "/v1" if args.mode == "fake" else "http://litellm:4000"
    if args.mode == "fake":
        env["LITELLM_API_KEY"] = "application-http-fixture-test-only"
    delay = max(args.timer_delay, 120 if args.mode == "real" else 15)
    compose = ["docker", "compose"]
    if (ROOT / ".env").exists():
        compose += ["--env-file", str(ROOT / ".env")]
    compose += ["-f", "deploy/docker-compose.yml"]
    fixture = None
    fixture_log = open(state_dir / "fixture.log", "ab")
    checks = []
    restate_admin = os.environ.get("RESTATE_ADMIN_URL", "http://localhost:9070")
    journal_before = journal_after = None
    admission_evidence = None
    api_restart_verified = timer_restart_verified = False
    admission_services_paused = False

    def passed(label):
        checks.append(label)
        print("PASS " + label, flush=True)

    def start_fixture():
        process = subprocess.Popen([sys.executable, str(ROOT / "test/fixtures/application-sink.py"),
            "--port", str(args.fixture_port), "--db", str(state_dir / "fixture.sqlite"), "--api", args.api,
            "--timer-delay", str(delay), "--drop-first-ack"], env=env, stdout=fixture_log, stderr=fixture_log)
        wait_for("application fixture", lambda: request(fixture_url, "/healthz"), 15)
        if process.poll() is not None:
            raise AssertionError("fixture process exited; inspect " + str(state_dir / "fixture.log"))
        return process

    def stop_fixture(process):
        process.terminate()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)

    def state():
        return request(fixture_url, "/state", key=service_token)

    def poll_run(run_id):
        row = request(args.api, "/runs/" + run_id, key=tenant_key)
        if row["status"] in ("failed", "canceled", "cancelled"):
            raise RuntimeError(f"run {run_id} failed: {row.get('error')}")
        return row if row["status"] == "completed" else None

    try:
        fixture = start_fixture()
        if not args.no_compose:
            up = compose + ["up", "-d"] + ([] if args.no_build else ["--build"]) + ["api", "worker", "harness"]
            subprocess.run(up, cwd=ROOT, env=env, check=True)
        wait_for("API health", lambda: request(args.api, "/healthz"), 120)
        if not args.no_compose:
            wait_for("worker registration", lambda: request(restate_admin,
                "/deployments", {"uri": "http://worker:9080", "force": True, "use_http_11": True}, expected=(200, 201, 202)), 90)
        # Admin bootstraps org/key only. Every application resource and timer
        # request below uses the tenant key; the service token is independent.
        tenant_key = request(args.api, f"/orgs/{TENANT}/keys", {}, key=admin_key, expected=(201,))["key"]
        request(fixture_url, "/configure", {"tenant_key": tenant_key, "hold_tools": True}, key=service_token)
        other_tenant = "application-e2e-other"
        other_key = request(args.api, f"/orgs/{other_tenant}/keys", {}, key=admin_key, expected=(201,))["key"]
        passed("API_AUTH_MODE=on and separate tenant/service credentials")
        scenario = "case_" + secrets.token_hex(6)
        schemas = {
            "remember": {"content": {"type": "string"}, "kind": {"type": "string"}},
            "recall": {"query": {"type": "string"}},
            "create_reminder": {"due_at": {"type": "string", "format": "date-time"}, "text": {"type": "string"}},
        }
        tools = [{"model_alias": name, "tool_ref": name, "description": description,
            "input_schema": {"type": "object", "properties": schemas[name], "required": list(schemas[name]), "additionalProperties": False}}
            for name, description in [("remember", "Persist the scoped test value and return its unpredictable nonce."),
                                      ("recall", "Read the prior scoped test nonce from the application."),
                                      ("create_reminder", "Persist one test reminder and durable timer submission responsibility.")]]
        agent_body = {"name": "application-runtime-fixture", "config": {
            "model": args.model or ("application-http-fixture" if args.mode == "fake" else os.environ.get("REAL_MODEL", "claude-sonnet-4-6")),
            "instructions": "Use only the supplied tools. For REMEMBER invoke remember and create_reminder, preferably in one turn, and echo the exact nonce from remember in the final. For RECALL invoke recall and echo its exact nonce. Never invent a nonce or claim an unconfirmed send.",
            "tools": [], "version": 1, "application_profile": True, "application_bridge_ref": "application-e2e",
            "application_tools": tools, "max_output_tokens": 2048, "max_steps": 8, "max_duration_ms": 180000}}
        agent = request(args.api, f"/orgs/{TENANT}/agents", agent_body, key=tenant_key, idem=scenario, expected=(201,))
        repeat = request(args.api, f"/orgs/{TENANT}/agents", agent_body, key=tenant_key, idem=scenario)
        assert agent["id"] == repeat["id"]
        session_body = {"subject_ref": SUBJECT, "scope_id": SCOPE}
        session = request(args.api, f"/agents/{agent['id']}/sessions", session_body, key=tenant_key, idem=scenario + "-thread-a", expected=(201,))
        repeat = request(args.api, f"/agents/{agent['id']}/sessions", session_body, key=tenant_key, idem=scenario + "-thread-a")
        assert session["id"] == repeat["id"]
        passed("agent/session creation is idempotent")
        if not args.skip_restart:
            # Deterministically leave the first command committed in PG while
            # Restate cannot accept it. No production test hook is required.
            subprocess.run(compose + ["stop", "restate"], cwd=ROOT, env=env, check=True)
            admission_services_paused = True
        due = (dt.datetime.now(dt.timezone.utc) + dt.timedelta(seconds=delay)).isoformat()
        run_body = {"input": f"REMEMBER {scenario}: remember this test information and create one reminder at {due}; return the actual random nonce.", "origin_command_id": scenario}
        run = request(args.api, f"/sessions/{session['id']}/runs", run_body, key=tenant_key, idem=scenario, expected=(202,))
        repeat = request(args.api, f"/sessions/{session['id']}/runs", run_body, key=tenant_key, idem=scenario)
        assert run["run_id"] == repeat["run_id"]
        busy = request(args.api, f"/sessions/{session['id']}/runs", {"input": "new input while busy", "origin_command_id": scenario + "-busy"}, key=tenant_key, idem=scenario + "-busy", expected=(409,))
        assert busy.get("error_code") == "session_busy"
        request(args.api, "/runs/" + run["run_id"], key=other_key, expected=(403,))
        request(args.api, "/runs/" + run["run_id"], expected=(401,))
        request(args.api, f"/sessions/{session['id']}/runs", run_body, key=other_key, idem="foreign", expected=(403,))
        if not args.skip_restart:
            queued = request(args.api,"/runs/"+run["run_id"],key=tenant_key)
            assert queued["status"] == "queued", "run escaped unavailable Restate before API crash"
            subprocess.run(compose + ["kill", "-s", "SIGKILL", "api"], cwd=ROOT, env=env, check=True)
            subprocess.run(compose + ["start", "restate", "api"], cwd=ROOT, env=env, check=True)
            wait_for("API after committed-admission crash",lambda:request(args.api,"/healthz"),120)
            admission_services_paused = False
            replay = request(args.api,f"/sessions/{session['id']}/runs",run_body,key=tenant_key,idem=scenario)
            assert replay["run_id"] == run["run_id"]
            admission_evidence = {"run_id_before": run["run_id"], "status_before_api_kill": queued["status"],
                                  "restate_stopped_before_submit": True, "api_kill_signal": "SIGKILL",
                                  "run_id_after": replay["run_id"], "status_after_restart": replay["status"],
                                  "idempotent_after_restart": replay.get("idempotent",False)}
            api_restart_verified = True
            passed("PG admission survived API SIGKILL before Restate accepted it; same run ID recovered")
        request(fixture_url,"/configure",{"hold_tools":False},key=service_token)
        passed("202 run, idempotency before busy, and cross-tenant denial")
        final = wait_for("model/tool run", lambda: poll_run(run["run_id"]), 200)
        current = state()
        remembered = [json.loads(op["result_json"]) for op in current["operations"] if op["tool_ref"] == "remember" and json.loads(op["context_json"])["origin_command_id"] == scenario]
        assert len(remembered) == 1
        nonce = remembered[0]["nonce"]
        assert nonce in final["final"]
        created = [json.loads(op["result_json"]) for op in current["operations"] if op["tool_ref"] == "create_reminder" and json.loads(op["context_json"])["origin_command_id"] == scenario]
        assert len(created) == 1
        if args.mode == "fake":
            assert any(row["results_count"] == 2 and row["verified"] for row in current["model_requests"])
        passed(f"{args.mode} model through Python HTTP provider consumed actual tool nonce and both results")
        timer_row = wait_for("application timer submission", lambda: next((row for row in state()["timers"] if row["timer_id"] == created[0]["timer_id"] and row["accepted"]), None), 30)
        timer_request = json.loads(timer_row["request_json"])
        query = urllib.parse.urlencode({k: timer_request[k] for k in ("tenant_id", "subject_ref", "scope_id", "revision")})
        timer_path = "/timers/" + timer_request["timer_id"] + "?" + query
        request(args.api, "/timers", {**timer_request, "tenant_id": other_tenant}, key=tenant_key, expected=(403,))
        request(args.api, timer_path, key=other_key, expected=(403,))
        request(args.api, timer_path.replace("scope_id=" + SCOPE, "scope_id=private"), key=tenant_key, expected=(404,))
        request(args.api, "/timers", timer_request, key=tenant_key, expected=(202,))
        request(args.api, "/timers", {**timer_request, "opaque_action_ref": "different"}, key=tenant_key, expected=(409,))
        passed("timer idempotency, payload conflict, tenant and scope checks")
        before = len(current["operations"])
        if not args.skip_restart:
            absolute_due = dt.datetime.fromisoformat(timer_request["due_at"])
            timer_status = request(args.api,timer_path,key=tenant_key)
            journal_before = wait_for("pending Sleep journal before restart",
                lambda:sleep_evidence(restate_admin,timer_status["timer_handle"],absolute_due),30)
            assert absolute_due - dt.datetime.now(dt.timezone.utc) > dt.timedelta(seconds=5), "too little time before restart; increase --timer-delay"
            stop_fixture(fixture)
            fixture = start_fixture()
            assert len(state()["operations"]) == before
            subprocess.run(compose + ["restart", "worker"], cwd=ROOT, env=env, check=True)
            journal_after = wait_for("same Sleep journal after restart",
                lambda:sleep_evidence(restate_admin,timer_status["timer_handle"],absolute_due,pending=False),30)
            assert all(journal_before[key] == journal_after[key] for key in ("invocation_id","sleep_index","completion_id","wake_up_time")), "worker restart replaced the durable Sleep"
            timer_restart_verified = True
            passed("fixture SQLite and worker restarted while timer waits; volumes preserved")
        second = request(args.api, f"/agents/{agent['id']}/sessions", session_body, key=tenant_key, idem=scenario + "-thread-b", expected=(201,))
        recalled = request(args.api, f"/sessions/{second['id']}/runs", {"input": "RECALL the scoped runtime nonce from the earlier thread", "origin_command_id": scenario + "-recall"}, key=tenant_key, idem=scenario + "-recall", expected=(202,))
        recalled_final = wait_for("new-session recall", lambda: poll_run(recalled["run_id"]), 200)
        assert nonce in recalled_final["final"]
        assert second["id"] != session["id"]
        assert any(op["tool_ref"] == "recall" and json.loads(op["context_json"])["run_id"] == recalled["run_id"]
                   and json.loads(op["result_json"]).get("nonce") == nonce for op in state()["operations"])
        passed("new session in same scope reads application-owned persisted value")
        def callback_complete():
            fires = state()["fires"]
            row = request(args.api, timer_path, key=tenant_key)
            matches = [f for f in fires if json.loads(f["payload_json"])["timer_id"] == timer_request["timer_id"]]
            return {"fire":matches[0],"timer":row} if len(matches) == 1 and matches[0]["attempts"] >= 2 and row["callback_attempts"] >= 2 and row["state"] == "acknowledged" else None
        completed = wait_for("Restate callback retry and durable receipt", callback_complete, delay + 120)
        fire = completed["fire"]
        passed("lost callback ACK caused Restate retry with same fire ID; one durable application event")
        report = {"mode": args.mode, "real_model": args.mode == "real", "model": agent_body["config"]["model"],
                  "config_version": agent["config"]["version"], "spec_digest": agent.get("spec_digest"), "scenario": scenario, "run_id": run["run_id"],
                  "timer_id": timer_request["timer_id"], "fire_id": fire["fire_id"], "callback_requests": fire["attempts"],
                  "timer_callback_attempts": completed["timer"]["callback_attempts"],
                  "restart_verified": api_restart_verified and timer_restart_verified,
                  "api_admission_restart_verified": api_restart_verified, "timer_restart_verified": timer_restart_verified,
                  "api_admission_restart_evidence": admission_evidence,
                  "sleep_journal_before_restart": journal_before, "sleep_journal_after_restart": journal_after,
                  "checks": checks, "sqlite": str(state_dir / "fixture.sqlite")}
        (state_dir / "report.json").write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n")
        print("Application runtime gate passed (" + args.mode + "); report=" + str(state_dir / "report.json"), flush=True)
    finally:
        if admission_services_paused:
            subprocess.run(compose + ["start", "restate", "api"], cwd=ROOT, env=env, check=False)
        if fixture is not None:
            stop_fixture(fixture)
        fixture_log.close()


if __name__ == "__main__":
    main()
