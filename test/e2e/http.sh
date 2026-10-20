#!/usr/bin/env bash
# Explicit test helpers for the asynchronous API. They never change HTTP status
# or manufacture a terminal result; wait_run only reads GET /runs/{id}.

submit_run() {
  local api="$1" session_id="$2" key="$3" body="$4"
  local response_file status
  local auth_headers=(-H 'Accept: application/json')
  if [ -n "${E2E_API_TOKEN:-}" ]; then auth_headers+=(-H "Authorization: Bearer $E2E_API_TOKEN"); fi
  response_file=$(mktemp)
  if ! status=$(curl -sS -m 15 -o "$response_file" -w '%{http_code}' \
      -X POST "$api/sessions/$session_id/runs" -H 'content-type: application/json' \
      -H "Idempotency-Key: $key" "${auth_headers[@]}" -d "$body"); then
    rm -f "$response_file"
    return 1
  fi
  if [ "$status" != 202 ]; then
    echo "expected durable admission HTTP 202, received $status" >&2
    cat "$response_file" >&2
    rm -f "$response_file"
    return 1
  fi
  cat "$response_file"
  rm -f "$response_file"
}

wait_run() {
  local api="$1" run_id="$2" timeout_seconds="${3:-180}"
  E2E_API_TOKEN="${E2E_API_TOKEN:-}" python3 - "$api" "$run_id" "$timeout_seconds" <<'PY'
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

api, run_id, timeout_seconds = sys.argv[1:]
deadline = time.monotonic() + float(timeout_seconds)
url = api.rstrip("/") + "/runs/" + urllib.parse.quote(run_id, safe="")
headers = {}
if os.environ.get("E2E_API_TOKEN"):
    headers["Authorization"] = "Bearer " + os.environ["E2E_API_TOKEN"]
last_status = "unknown"
while time.monotonic() < deadline:
    try:
        request = urllib.request.Request(url, headers=headers)
        with urllib.request.urlopen(request, timeout=min(10, max(.1, deadline - time.monotonic()))) as response:
            result = json.load(response)
        last_status = result.get("status", "unknown")
        if result.get("run_id") != run_id:
            raise SystemExit("GET returned a different run identity")
        if last_status in {"completed", "failed", "canceled"}:
            print(json.dumps(result, ensure_ascii=False))
            raise SystemExit(0)
    except urllib.error.HTTPError as exc:
        if exc.code < 500:
            raise SystemExit(f"GET run failed with HTTP {exc.code}") from exc
    except (urllib.error.URLError, TimeoutError):
        pass
    time.sleep(.2)
raise SystemExit(f"run {run_id} did not terminate within {timeout_seconds}s; last status={last_status}")
PY
}

run_and_wait() {
  local admission run_id
  admission=$(submit_run "$1" "$2" "$3" "$4") || return
  run_id=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["run_id"])' <<< "$admission") || return
  wait_run "$1" "$run_id" "${5:-180}"
}
