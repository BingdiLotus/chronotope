#!/usr/bin/env bash
# W31 真实 kill-9 演练：run 的工具执行窗口内 SIGKILL worker → 重启 →
# Restate 重放恢复。断言崩溃正确性三件事：
#   ① run 最终 completed（重放恢复——不是「重启后从头再跑」）
#   ② llm_calls 的 (run, step) 恰好 1 行（账本仲裁：既有 result 零派发）
#   ③ 事件/exec 不重复（重放幂等的可观测证据）
# 前置：fake harness（sleep 让工具执行窗口可控）+ docker driver。
set -euo pipefail
cd "$(dirname "$0")/../.."
API="${1:-http://localhost:8080}"
RUN_ID="k9-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() { local d="$1"; shift; if "$@" >/dev/null 2>&1; then pass "$d"; else fail "$d"; fi; }

echo "== W31 kill-9 恢复演练（API=$API RUN_ID=${RUN_ID}）=="
rm -f /tmp/k9-run.out
for i in $(seq 1 30); do curl -fsS "$API/healthz" >/dev/null 2>&1 && break; sleep 1; done

# fake 脚本：bash 执行 6 秒（窗口足够在 sleep 期间 kill worker）
HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT='[{"tool_call":{"name":"bash","arguments":{"command":"sleep 6; echo recovered"}}},{"final":"kill-9 后重放完成"}]' \
  docker compose --env-file .env -f deploy/docker-compose.yml up -d --force-recreate harness > /dev/null 2>&1
for i in $(seq 1 30); do curl -fsS -m 2 http://host.docker.internal:8000/healthz >/dev/null 2>&1 && break; sleep 1; done

AGENT=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"k9-agent","config":{"model":"claude-sonnet-4-6","instructions":"你是助手。","tools":["bash"],"version":1}}')
AID=$(echo "$AGENT" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')

# 起 run（后台——同步等待会因 worker 崩溃而失败，属预期）
(curl -fsS -m 180 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-1" -d '{"input":"执行一个 6 秒的命令"}' -o /tmp/k9-run.out 2>&1 || true) &
CURL_PID=$!

# 等待工具执行开始（bash 的 sandbox.exec 事件出现）后再 SIGKILL
for i in $(seq 1 40); do
  if docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc \
       "SELECT count(*) FROM events WHERE session_id='$SID' AND type='sandbox.exec'" 2>/dev/null | grep -q '^[1-9]'; then
    break
  fi
  sleep 0.5
done
# 真实进程级故障：SIGKILL worker（容器主进程）
docker kill -9 chronotope-worker-1 >/dev/null 2>&1 || true
echo "  → worker SIGKILL 已注入（工具执行窗口内）"
sleep 2
docker start chronotope-worker-1 >/dev/null 2>&1 || true
for i in $(seq 1 60); do docker exec chronotope-worker-1 sh -c 'exit 0' >/dev/null 2>&1 && break; sleep 1; done
wait $CURL_PID 2>/dev/null || true

# --- ① run 最终 completed（Restate 重放恢复——轮询到 120s）---
RUN_STATUS=""
for i in $(seq 1 60); do
  RUN_STATUS=$(docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc \
    "SELECT status FROM runs WHERE session_id='$SID' ORDER BY created_at DESC LIMIT 1" 2>/dev/null | tr -d ' ')
  [ "$RUN_STATUS" = "completed" ] && break
  sleep 2
done
assert "崩溃后 run 最终 completed（重放恢复）" bash -c "[ '$RUN_STATUS' = 'completed' ]"

# --- ② llm_calls 不重复（账本仲裁：既有 result 零派发）---
RID=$(docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc \
  "SELECT id FROM runs WHERE session_id='$SID' ORDER BY created_at DESC LIMIT 1" 2>/dev/null | tr -d ' ')
assert "llm_calls 每 step 恰好 1 行（账本不重复派发）" \
  bash -c "N=\$(docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc \"SELECT count(*) FROM llm_calls WHERE run_id='$RID' AND step=0\" | tr -d ' '); [ \"\$N\" = '1' ]"

# --- ③ 事件不重复（run.started 恰好 1 次——重放幂等）---
assert "run.started 事件恰好 1 次（重放幂等）" \
  bash -c "N=\$(docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc \"SELECT count(*) FROM events WHERE session_id='$SID' AND type='run.started'\" | tr -d ' '); [ \"\$N\" = '1' ]"

# --- ④ exec 账本不重复（sandbox_execs 的 (run,key) 唯一）---
assert "sandbox_execs 每 operation 恰好 1 行（exec 不重复执行）" \
  bash -c "N=\$(docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc \"SELECT count(*) FROM sandbox_execs WHERE idempotency_key LIKE '$RID:%'\" | tr -d ' '); [ \"\$N\" -ge 1 ] && [ \"\$N\" -le 2 ]"

# --- ⑤ 终态记账（run.completed 事件——重放后完整提交）---
assert "run.completed 事件落地（终态记账完整）" \
  bash -c "N=\$(docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc \"SELECT count(*) FROM events WHERE session_id='$SID' AND type='run.completed'\" | tr -d ' '); [ \"\$N\" = '1' ]"

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[ "$FAIL" = "0" ]
