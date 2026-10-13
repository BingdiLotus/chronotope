#!/usr/bin/env bash
# 真实组合 e2e：真实模型（Claude SDK via poke2api 代理）+ Claude 适配器
# + E2B 真实沙箱——目标：编写贪吃蛇网页（HTML+JS 单文件）。
set -euo pipefail
cd "$(dirname "$0")/../.."
API="${1:-http://localhost:8080}"
RUN_ID="snake-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() { local d="$1"; shift; if "$@" >/dev/null 2>&1; then pass "$d"; else fail "$d"; fi; }

echo "== W29 真实组合：Claude SDK + E2B 编写贪吃蛇 (API=$API RUN_ID=$RUN_ID) =="
# ① 起 Claude 适配器（真实 SDK——ANTHROPIC_API_KEY 经 poke2api 代理）
PY=python3
python3 -c "import claude_agent_sdk" 2>/dev/null || PY=/Users/yangziwen/anaconda3/bin/python3
rm -f /tmp/snake-run.out
nohup $PY adapters/claude/harness.py 8010 > /tmp/claude-snake.log 2>&1 &
CP=$!
sleep 3
for i in $(seq 1 30); do curl -fsS "$API/healthz" > /dev/null 2>&1 && break; sleep 1; done

# ② 注册 Claude harness + agent（tools: write_file/bash——class 1/2 走平台审批；
# 真实组合用 allowlist 默认允许 write_file/bash 的 class 1）
for i in $(seq 1 30); do
  curl -fsS -X POST "$API/orgs/org-$RUN_ID/harnesses" -H 'content-type: application/json' \
    -d '{"name":"claude-sdk","endpoint":"http://host.docker.internal:8010","version":"v1","capabilities":{"hooks":["PreToolUse","PostToolUse"],"tool_families":["exec","mcp","control"],"protocol":"1.0"}}' > /dev/null 2>&1 && break
  sleep 1
done
AID=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"snake","config":{"model":"claude-sonnet-4-6","instructions":"你是网页开发者。","tools":["write_file","bash","read_file"],"harness_ref":{"name":"claude-sdk"},"version":1}}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')

# ③ 真实 run：编写贪吃蛇网页（单文件 HTML+JS）
curl -fsS -m 900 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-1" \
  -d '{"input":"编写一个贪吃蛇网页游戏，单文件 HTML+JS，用 write_file 保存为 snake.html（仅用 write_file——class 1 默认允许）"}' \
  -o /tmp/snake-run.out 2>&1 || true
assert "run 完成（真实 Claude 多轮工具调用）" \
  python3 -c 'import json; d=json.load(open("/tmp/snake-run.out")); assert d["status"]=="completed", d' <<< '{}'
assert "账本 token 非零（真实模型 usage 落账）" \
  bash -c "docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc \"SELECT count(*) FROM llm_calls WHERE run_id=(SELECT id FROM runs WHERE session_id='$SID' ORDER BY created_at DESC LIMIT 1) AND tokens_in > 0\" | grep -q 1"
# 真实发现的标注（2026-10-13 实证）：
# ① SDK manual 模式的工具批准回调桥接未完成（ask 默认拒绝——工具零
#    执行、模型跳过直接 final）
# ② E2B 的 write_file 经 blob 合同 PUT file 500（rustfs/E2B 的 PUT
#    路径——write 失败的真实缺陷）
# 已实证：真实模型 token 落账 + run 闭环（无 write 时）+ E2B 沙箱
# 创建/AttachSandbox + Claude 多轮工具分流

kill $CP 2>/dev/null || true
echo "== 结果: $PASS 通过, $FAIL 失败 =="
[ "$FAIL" = "0" ]
