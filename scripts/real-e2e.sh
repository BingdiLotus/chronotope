#!/usr/bin/env bash
# 真实模型全特性 e2e 汇总编排（需 .env 密钥 + litellm + harness 真实模式）：
#   基础（对话/消化/子 Agent/计量）→ real-model.sh
#   组合（群聊主持协议）→ real-group.sh
#   沙箱工具/快照恢复/定时唤醒/交付 → real-tools.sh
#   HITL 审批（批准/拒绝）→ real-hitl.sh
#   预算冻结/解冻 → real-budget.sh
#   MCP 调用/skill/allowlist → real-ecosystem.sh
#   时间旅行/归档 → real-timetravel.sh
#   审批策略路由/TTL → real-policy.sh
#   共享知识挂载 → real-knowledge.sh
# 前置：全栈 compose 运行（harness 真实模式）；worker CONSOLIDATE_THRESHOLD=4
#   （real-model 消化触发）+ api ARCHIVE_MIN_AGE=0s（归档）。
# 用法: bash scripts/real-e2e.sh [API_URL]
set -euo pipefail
cd "$(dirname "$0")/.."
API="${1:-http://localhost:8080}"
TOTAL=0; FAILED=0

run() { # $1=脚本 $2=标签
  echo
  echo "━━━ $2 ━━━"
  if bash "test/e2e/$1" "$API"; then
    echo "✓ $2"
  else
    echo "✗ $2"
    FAILED=$((FAILED+1))
  fi
  TOTAL=$((TOTAL+1))
}

run real-model.sh    "真实对话/消化/子 Agent/计量"
run real-group.sh    "真实群聊主持协议"
run real-tools.sh    "真实沙箱工具/快照/定时/交付"
run real-hitl.sh     "真实 HITL 审批"
run real-budget.sh   "真实预算冻结/解冻"
run real-ecosystem.sh "真实 MCP/skill/allowlist"
run real-timetravel.sh "真实时间旅行/归档"
run real-policy.sh   "真实审批策略路由/TTL"
run real-knowledge.sh "真实共享知识挂载"

echo
echo "== 真实 e2e 汇总: $((TOTAL-FAILED))/$TOTAL 组通过 =="
[ "$FAILED" = "0" ]
