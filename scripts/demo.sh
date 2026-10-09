#!/usr/bin/env bash
# Chronotope 全场景演示：一条 compose up 复现 W1–W8 + 期 2/期 3 全部闭环。
# 无模型密钥（fake 模型流）；真实模型在 .env 配 OPENAI/ANTHROPIC key 后同样一条命令。
# 用法: bash scripts/demo.sh
set -euo pipefail

# 生命周期闭环 D3：收尾清沙箱孤儿（executor 经 Docker API 创建的容器/卷
# 在 compose 生命周期之外——测试结束后显式清理，生产由 boot sweep + GC 兜底）
cleanup_sandboxes() {
  local ids
  ids=$(docker ps -aq --filter 'label=chronotope.sandbox' 2>/dev/null || true)
  if [ -n "$ids" ]; then
    docker rm -f $ids > /dev/null 2>&1 || true
  fi
  local vols
  vols=$(docker volume ls -q --filter 'name=^sb_' 2>/dev/null || true)
  if [ -n "$vols" ]; then
    docker volume rm -f $vols > /dev/null 2>&1 || true
  fi
}
trap cleanup_sandboxes EXIT

cd "$(dirname "$0")/.."
# .env 在仓库根目录；compose 默认只在 deploy/ 查找，须显式指定
ENV_ARGS=()
[ -f .env ] && ENV_ARGS=(--env-file .env)
DC="docker compose ${ENV_ARGS[*]} -f deploy/docker-compose.yml"
# fake CI 固定 docker 档（快照含卷/blob 断言是 docker 语义——.env 的
# EXECUTOR_DRIVER=e2b_selfhosted 泄漏会在云沙箱跑失败实证；真实 e2e 另行
# 按档重建）
export EXECUTOR_DRIVER=docker
API=http://localhost:8080
RUN_ID="demo-$(date +%s)"
ADMIN=http://localhost:9070
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32m✓\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31m✗\033[0m $*"; FAIL=$((FAIL+1)); }

echo "== Chronotope 全场景演示 =="

# 1. 全栈启动（fake 模型）
echo "[1/6] docker compose 启动全栈…"
HARNESS_FAKE_MODEL=1 $DC up -d --build postgres restate litellm harness worker executor api
for i in $(seq 1 60); do curl -fsS "$API/healthz" > /dev/null 2>&1 && break; sleep 2; done
pass "全栈启动（api/worker/executor/harness + postgres/restate/rustfs/litellm）"

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
CHILD_ID=$(curl -fsS -X POST "$API/orgs/org-${RUN_ID}-child/agents" -H 'content-type: application/json' \
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

# 13.6 w19 合规验收 demo（M3：Goal→审批→Evidence→审计链）
bash test/e2e/w19-compliance.sh "$API" > /tmp/demo-w19.log 2>&1 && pass "W19 合规验收（6 项断言）" || { fail "W19 合规验收"; tail -5 /tmp/demo-w19.log; }

# 13.5 w18 账本与 quiesce e2e（A-F 批的端到端固化）
bash test/e2e/w18-ledger.sh "$API" > /tmp/demo-w18.log 2>&1 && pass "W18 账本/quiesce/unknown（4 项断言）" || { fail "W18 账本/quiesce/unknown"; tail -5 /tmp/demo-w18.log; }

# 13. W8 后置：交付清单 outbox（run 完成 → 交付行 → 投递回执）
bash test/e2e/w8-delivery.sh "$API" > /tmp/demo-w8d.log 2>&1 && pass "W8 后置 交付清单（3 项断言）" || { fail "W8 后置 交付清单"; tail -5 /tmp/demo-w8d.log; }

# 15. 快照含卷恢复（评审 #7：named volume 显式打包/解回——一周前会话还能继续）
HARNESS_FAKE_MODEL=1 \
HARNESS_FAKE_SCRIPT='[{"tool_call":{"name":"write_file","arguments":{"path":"/workspace/a.txt","content":"快照前内容"}}},{"final":"已写入。"}]' \
HARNESS_FAKE_SCRIPT_ALT='[{"tool_call":{"name":"read_file","arguments":{"path":"/workspace/a.txt"}}},{"final":"读取完成。"}]' \
  $DC up -d --force-recreate harness
for i in $(seq 1 30); do curl -fsS http://localhost:8000/healthz > /dev/null 2>&1 && break; sleep 1; done
bash test/e2e/w2-snapshot.sh "$API" > /tmp/demo-w2snap.log 2>&1 && pass "快照含卷恢复（4 项断言）" || { fail "快照含卷恢复"; tail -5 /tmp/demo-w2snap.log; }
HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT= HARNESS_FAKE_SCRIPT_ALT= $DC up -d --force-recreate harness

# 14. W8 后置：outbox 事件投递（webhook 通道，事件同事务入队 → 投递 worker）
nohup python3 test/fixtures/notify-sink.py 9300 > /tmp/demo-sink.log 2>&1 &
SINK_PID=$!
OUTBOX_INTERVAL=2s OUTBOX_ALLOW_PRIVATE=true $DC up -d --force-recreate api
for i in $(seq 1 30); do curl -fsS http://localhost:8080/healthz > /dev/null 2>&1 && break; sleep 1; done
bash test/e2e/w8-notify.sh "$API" "http://host.docker.internal:9300" > /tmp/demo-w8n.log 2>&1 && pass "W8 后置 事件投递（4 项断言）" || { fail "W8 后置 事件投递"; tail -5 /tmp/demo-w8n.log; }
kill $SINK_PID 2>/dev/null || true
OUTBOX_INTERVAL=5s OUTBOX_ALLOW_PRIVATE=false $DC up -d --force-recreate api

# 15. 时间旅行（正式版架构 期 2：checkpoint 树 + fork/diff/rollback）
HARNESS_FAKE_SCRIPT='[{"tool_call":{"name":"write_file","arguments":{"path":"/workspace/a.txt","content":"快照前内容"}}},{"final":"已写入。"}]' \
HARNESS_FAKE_SCRIPT_ALT='[{"tool_call":{"name":"read_file","arguments":{"path":"/workspace/a.txt"}}},{"final":"读取完成。"}]' \
HARNESS_FAKE_MODEL=1 $DC up -d --force-recreate harness
for i in $(seq 1 30); do curl -fsS http://localhost:8000/healthz > /dev/null 2>&1 && break; sleep 1; done
bash test/e2e/w10-timetravel.sh "$API" > /tmp/demo-tt.log 2>&1 && pass "时间旅行 checkpoint/fork/diff/rollback（9 项断言）" || { fail "时间旅行 checkpoint/fork/diff/rollback"; tail -5 /tmp/demo-tt.log; }

# 16. 工作区 blob 合同（期 2 §A：内容寻址 + blob: 恢复——第二条恢复链）
HARNESS_FAKE_SCRIPT='[{"tool_call":{"name":"write_file","arguments":{"path":"/workspace/b.txt","content":"blob合同内容"}}},{"final":"已写入。"}]' \
HARNESS_FAKE_SCRIPT_ALT='[{"tool_call":{"name":"bash","arguments":{"command":"cat /workspace/b.txt"}}},{"final":"读取完成。"}]' \
HARNESS_FAKE_MODEL=1 $DC up -d --force-recreate harness
for i in $(seq 1 30); do curl -fsS http://localhost:8000/healthz > /dev/null 2>&1 && break; sleep 1; done
bash test/e2e/w11-blob.sh "$API" > /tmp/demo-blob.log 2>&1 && pass "工作区 blob 合同（5 项断言）" || { fail "工作区 blob 合同"; tail -5 /tmp/demo-blob.log; }

# 17. 冷层归档（期 2 §B：老会话 → RustFS 冷层 + 清单 + archived_at）
ARCHIVE_MIN_AGE=0s HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT= $DC up -d --force-recreate api harness
for i in $(seq 1 30); do curl -fsS http://localhost:8080/healthz > /dev/null 2>&1 && break; sleep 1; done
# PITR 演练重启过 rustfs——GitHub 初始化慢于归档上传（exit 22 实证）：
# 等 rustfs 容器 healthy（S3 根响应）
for i in $(seq 1 60); do
  docker inspect --format '{{.State.Health.Status}}' chronotope-rustfs-1 2>/dev/null | grep -q healthy && break
  sleep 1
done
bash test/e2e/w12-archive.sh "$API" > /tmp/demo-arc.log 2>&1 && pass "冷层归档（4 项断言）" || { fail "冷层归档"; tail -5 /tmp/demo-arc.log; }

# 18. 期 3 治理 e2e（策略缝另一端的真实链路：principal 限流 / 审批路由+TTL /
#     MCP allowlist / 共享知识挂载——脚本自包含本地 on 模式 api + restate 卷重置）
HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT= \
  $DC up -d --force-recreate harness > /dev/null 2>&1
bash test/e2e/w13-policy.sh "$API" > /tmp/demo-w13.log 2>&1 && pass "期 3 治理五项（7 断言）" || { fail "期 3 治理五项"; tail -5 /tmp/demo-w13.log; }

# 19. 真实 e2e 缺陷回归（w14：快照存留/恢复/fork 初始化/knowledge 接线/
#     tool_call_id 配对/write_file 恢复链——7 断言固化 fake 绿真实红缺口）
bash test/e2e/w14-regression.sh "$API" > /tmp/demo-w14.log 2>&1 && pass "真实 e2e 缺陷回归（7 断言）" || { fail "真实 e2e 缺陷回归"; tail -5 /tmp/demo-w14.log; }


echo "== 演示结果: $PASS 通过, $FAIL 失败 =="
echo "控制台: cd web && pnpm install && pnpm dev（或 compose --profile web up web）→ http://localhost:3000"
[[ "$FAIL" -eq 0 ]]
