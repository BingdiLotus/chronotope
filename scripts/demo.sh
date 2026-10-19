#!/usr/bin/env bash
# Current fake gate: W1 asynchronous conversation + application runtime recovery.
# Historical scenarios are preserved in demo-legacy.sh and are not this gate.
set -euo pipefail
cd "$(dirname "$0")/.."

DC=(docker compose)
if [ -f .env ]; then DC+=(--env-file .env); fi
DC+=(-f deploy/docker-compose.yml)
API=http://localhost:8080
ADMIN=http://localhost:9070
export EXECUTOR_DRIVER=docker

wait_http() {
  local url="$1"
  for _ in $(seq 1 90); do
    if curl -fsS -m 2 "$url" > /dev/null 2>&1; then return 0; fi
    sleep 1
  done
  echo "service did not become ready: $url" >&2
  return 1
}

echo '== Chronotope current fake gate: W1 + application runtime =='
# W1 uses the deterministic provider without an application fixture. The C3
# driver then configures the real Python provider against a fake HTTP model.
API_AUTH_MODE=off HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT= HARNESS_FAKE_SCRIPT_ALT= \
  APPLICATION_BRIDGES_JSON='[]' APPLICATION_TIMER_TARGETS_JSON='[]' \
  "${DC[@]}" up -d --build postgres restate litellm harness worker api
wait_http "$API/healthz"
wait_http http://localhost:8000/healthz
wait_http http://localhost:9080/healthz
curl -fsS -m 15 -X POST "$ADMIN/deployments" -H 'content-type: application/json' \
  -d '{"uri":"http://worker:9080","version":"v1","use_http_11":true,"force":true}' > /dev/null
bash test/e2e/w1-loop.sh "$API"

# This driver owns its sink/model fixture and preserves volumes while restarting
# API/worker. A successful fake gate is not a real-model or Slack receipt result.
APPLICATION_ARGS=(--api "$API" --mode fake --no-build)
if [ -n "${E2E_STATE_DIR:-}" ]; then APPLICATION_ARGS+=(--state-dir "$E2E_STATE_DIR"); fi
python3 test/e2e/application-runtime.py "${APPLICATION_ARGS[@]}"

echo '== Current fake gate passed; historical scenarios were not run =='
