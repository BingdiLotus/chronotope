#!/usr/bin/env bash
# 单位成本投影器：三账本 SQL 聚合 × 公开价格参数 → 每 session-hour 成本/毛利
# + scheduler_hint 节省估算。用法: bash scripts/econ-project.sh
set -euo pipefail
cd "$(dirname "$0")/.."
# 价格参数（公开参考价——用真实供应商价目替换）
MODEL_IN_PER_M="${1:-3.00}"     # $/M tokens in
MODEL_OUT_PER_M="${2:-15.00}"   # $/M tokens out
COMPUTE_PER_SEC="${3:-0.00003}" # $/计算秒（E2B 类沙箱）
STORAGE_PER_GB_MO="${4:-0.023}" # $/GB/月（S3 类）
PRICE_PER_HOUR="${5:-0.08}"     # 定价：$/session-hour（活跃）

psqlx() { docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc "$1" 2>/dev/null | tr -d '[:space:]'; }

TOK_IN=$(psqlx "SELECT COALESCE(SUM(tokens_in),0) FROM llm_calls")
TOK_OUT=$(psqlx "SELECT COALESCE(SUM(tokens_out),0) FROM llm_calls")
COMPUTE_SEC=$(psqlx "SELECT COALESCE(SUM((payload->>'duration_ms')::numeric/1000),0) FROM events WHERE type='llm.call' AND payload->>'duration_ms' IS NOT NULL")
HOURS=$(psqlx "SELECT COALESCE(EXTRACT(EPOCH FROM (max(finished_at)-min(started_at)))/3600, 1) FROM runs WHERE started_at IS NOT NULL")
STORAGE_GB=$(psqlx "SELECT COALESCE(SUM(size),0)/1024.0/1024.0/1024.0 FROM workspace_files")

python3 - "$TOK_IN" "$TOK_OUT" "$COMPUTE_SEC" "$HOURS" "$STORAGE_GB" "$MODEL_IN_PER_M" "$MODEL_OUT_PER_M" "$COMPUTE_PER_SEC" "$STORAGE_PER_GB_MO" "$PRICE_PER_HOUR" <<'PYEOF'
import sys
tok_in, tok_out, comp, hours, gb = map(float, sys.argv[1:6])
p_in, p_out, p_comp, p_store, p_hour = map(float, sys.argv[6:11])
model_cost = tok_in/1e6*p_in + tok_out/1e6*p_out
compute_cost = comp*p_comp
storage_cost = gb*p_store/730  # 月均到小时（730h/月）
cost = model_cost + compute_cost + storage_cost
revenue = hours*p_hour
print(f"账本聚合: tokens_in={tok_in:.0f} tokens_out={tok_out:.0f} 计算秒={comp:.1f} 活跃时长={hours:.2f}h 存储={gb:.3f}GB")
print(f"成本: 模型=${model_cost:.4f} 计算=${compute_cost:.4f} 存储=${storage_cost:.4f} 合计=${cost:.4f}")
print(f"收入: {hours:.2f}h × ${p_hour:.2f} = ${revenue:.4f}")
print(f"毛利: ${revenue-cost:.4f} (margin={(revenue-cost)/revenue*100:.1f}%)" if revenue > 0 else "毛利: 无收入基准（无活跃时长）")
PYEOF
