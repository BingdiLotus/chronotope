#!/usr/bin/env bash
# Chronotope W1–W4 全场景演示：一条 compose up 复现全部闭环。
# 无模型密钥（fake 模型流）；真实模型在 .env 配 OPENAI/ANTHROPIC key 后同样一条命令。
# 用法: bash scripts/demo.sh
set -euo pipefail

cd "$(dirname "$0")/.."
# .env 在仓库根目录；compose 默认只在 deploy/ 查找，须显式指定
ENV_ARGS=()
[ -f .env ] && ENV_ARGS=(--env-file .env)
DC="docker compose ${ENV_ARGS[*]} -f deploy/docker-compose.yml"
API=http://localhost:8080
ADMIN=http://localhost:9070
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32m✓\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31m✗\033[0m $*"; FAIL=$((FAIL+1)); }

echo "== Chronotope 全场景演示 =="

# 1. 全栈启动（fake 模型）
echo "[1/6] docker compose 启动全栈…"
HARNESS_FAKE_MODEL=1 $DC up -d --build postgres restate litellm harness worker executor api
for i in $(seq 1 60); do curl -fsS "$API/healthz" > /dev/null 2>&1 && break; sleep 2; done
pass "全栈启动（api/worker/executor/harness + postgres/restate/minio/litellm）"

# 2. 注册 worker 端点
curl -fsS -X POST "$ADMIN/deployments" -H 'content-type: application/json' \
  -d '{"uri":"http://worker:9080","version":"v1","use_http_11":true,"force":true}' > /dev/null
pass "worker 端点注册（version v1）"

# 3. W1 对话闭环
bash test/e2e/w1-loop.sh "$API" > /tmp/demo-w1.log 2>&1 && pass "W1 对话闭环（11 项断言）" || { fail "W1 对话闭环"; tail -5 /tmp/demo-w1.log; }

# 4. W2 沙箱闭环（harness 切脚本模式重启）
HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT='[{"tool_call":{"name":"write_file","arguments":{"path":"/workspace/hello.py","content":"print(42)"}}},{"tool_call":{"name":"bash","arguments":{"command":"python3 /workspace/hello.py"}}},{"tool_call":{"name":"read_file","arguments":{"path":"/workspace/hello.py"}}},{"final":"沙箱闭环完成，输出 42。"}]' \
  $DC up -d --force-recreate harness
sleep 3
bash test/e2e/w2-sandbox.sh "$API" > /tmp/demo-w2.log 2>&1 && pass "W2 沙箱闭环（9 项断言）" || { fail "W2 沙箱闭环"; tail -5 /tmp/demo-w2.log; }

# 5. W3 HITL（harness 切审批脚本）
HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT='[{"tool_call":{"name":"request_approval","arguments":{"question":"允许删除生产数据吗？"}}},{"final":"已获批准，执行完成。"}]' \
  $DC up -d --force-recreate harness
sleep 3
bash test/e2e/w3-hitl.sh "$API" > /tmp/demo-w3h.log 2>&1 && pass "W3 HITL 审批挂起/恢复（4 项断言）" || { fail "W3 HITL"; tail -5 /tmp/demo-w3h.log; }

# 6. W3 定时唤醒（harness 回默认 fake）
HARNESS_FAKE_SCRIPT= HARNESS_FAKE_MODEL=1 $DC up -d --force-recreate harness
for i in $(seq 1 30); do curl -fsS http://localhost:8000/healthz > /dev/null 2>&1 && break; sleep 1; done
bash test/e2e/w3-schedule.sh "$API" > /tmp/demo-w3s.log 2>&1 && pass "W3 定时唤醒（5 项断言）" || { fail "W3 定时唤醒"; tail -5 /tmp/demo-w3s.log; }

# 7. W5 分层记忆（worker 阈值调小 → 多轮对话触发消化 → 恢复默认）
CONSOLIDATE_THRESHOLD=4 $DC up -d --force-recreate worker
curl -fsS -X POST "$ADMIN/deployments" -H 'content-type: application/json' \
  -d '{"uri":"http://worker:9080","version":"v1","use_http_11":true,"force":true}' > /dev/null
sleep 3
bash test/e2e/w5-memory.sh "$API" > /tmp/demo-w5.log 2>&1 && pass "W5 分层记忆（4 项断言）" || { fail "W5 分层记忆"; tail -5 /tmp/demo-w5.log; }
CONSOLIDATE_THRESHOLD=40 $DC up -d --force-recreate worker

# 8. W5 工具风险分级（class 2 强制审批：批准执行 / 拒绝拦截）
HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT='[{"tool_call":{"name":"bash","arguments":{"command":"sleep 1; echo ok"}}},{"final":"危险操作已批准执行。"}]' \
  $DC up -d --force-recreate harness
for i in $(seq 1 30); do curl -fsS http://localhost:8000/healthz > /dev/null 2>&1 && break; sleep 1; done
bash test/e2e/w5-risk.sh "$API" > /tmp/demo-w5r.log 2>&1 && pass "W5 工具风险分级（4 项断言）" || { fail "W5 工具风险分级"; tail -5 /tmp/demo-w5r.log; }
HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT= $DC up -d --force-recreate harness

# 9. W6 子 Agent（父派发 → 子会话独立执行 → 结果回喂）
CHILD_ID=$(curl -fsS -X POST "$API/orgs/org-demo-child/agents" -H 'content-type: application/json' \
  -d '{"name":"demo-child","config":{"model":"chronotope-subagent","instructions":"子任务助手。","tools":[],"version":1}}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT='[{"tool_call":{"name":"spawn_subagent","arguments":{"agent":"'"$CHILD_ID"'","input":"计算 2+2"}}},{"final":"子任务已完成，父任务收尾。"}]' \
  $DC up -d --force-recreate harness
for i in $(seq 1 30); do curl -fsS http://localhost:8000/healthz > /dev/null 2>&1 && break; sleep 1; done
CHILD_AGENT_ID="$CHILD_ID" bash test/e2e/w6-subagent.sh "$API" > /tmp/demo-w6.log 2>&1 && pass "W6 子 Agent（5 项断言）" || { fail "W6 子 Agent"; tail -5 /tmp/demo-w6.log; }
HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT= $DC up -d --force-recreate harness

# 10. W5 org 预算 + 欠费冻结（api 聚合周期调小 → 超限冻结 → 充值解冻）
API_AGGREGATE_INTERVAL=5s $DC up -d --force-recreate api
for i in $(seq 1 30); do curl -fsS http://localhost:8080/healthz > /dev/null 2>&1 && break; sleep 1; done
bash test/e2e/w5-budget.sh "$API" > /tmp/demo-w5b.log 2>&1 && pass "W5 org 预算冻结（3 项断言）" || { fail "W5 org 预算冻结"; tail -5 /tmp/demo-w5b.log; }
API_AGGREGATE_INTERVAL=1m $DC up -d --force-recreate api

# 11. W7 群聊多 Agent（moderator 主持循环 + 成员 child run 发言）
HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT='[{"tool_call":{"name":"next_speaker","arguments":{"participant":0,"instruction":"请就方案选择发表意见"}}},{"tool_call":{"name":"next_speaker","arguments":{"participant":1,"instruction":"请评审上一轮发言"}}},{"final":"讨论完成：采用方案 A 并限定范围。"}]' \
  $DC up -d --force-recreate harness
for i in $(seq 1 30); do curl -fsS http://localhost:8000/healthz > /dev/null 2>&1 && break; sleep 1; done
bash test/e2e/w7-group.sh "$API" > /tmp/demo-w7.log 2>&1 && pass "W7 群聊多 Agent（5 项断言）" || { fail "W7 群聊多 Agent"; tail -5 /tmp/demo-w7.log; }
HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT= $DC up -d --force-recreate harness

# 12. W6 生态（MCP 连接 + skill 安装；worker 托管客户端 + 沙箱工作区卷）
nohup python3 test/fixtures/mcp-server.py 9100 > /tmp/demo-mcp-fixture.log 2>&1 &
MCP_FIXTURE_PID=$!
HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT='[{"tool_call":{"name":"mcp:echo:echo","arguments":{"text":"你好"}}},{"final":"MCP 回显完成。"}]' \
  $DC up -d --force-recreate harness
for i in $(seq 1 30); do curl -fsS http://localhost:8000/healthz > /dev/null 2>&1 && break; sleep 1; done
bash test/e2e/w6-mcp.sh "$API" > /tmp/demo-w6m.log 2>&1 && pass "W6 生态 MCP/skill（5 项断言）" || { fail "W6 生态 MCP/skill"; tail -5 /tmp/demo-w6m.log; }
kill $MCP_FIXTURE_PID 2>/dev/null || true
HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT= $DC up -d --force-recreate harness

echo "== 演示结果: $PASS 通过, $FAIL 失败 =="
echo "控制台: cd web && pnpm install && pnpm dev（或 compose --profile web up web）→ http://localhost:3000"
[[ "$FAIL" -eq 0 ]]
