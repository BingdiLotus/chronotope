#!/usr/bin/env bash
# chaos 套件骨架（W3 持久性三件套）：kill -9 矩阵的注入脚本。
# 用法: ./kill9.sh <api|worker|executor|harness>
#
# W3 完整版将包含：
#   - run 执行到第 N 步时的时序注入（需要 worker 的步进桩/断点标记）
#   - 组合 kill（worker+executor 同杀、全栈重启）
#   - 事后验证：事件序列连续（允许 seq gap）、无重复 harness/exec 调用
set -euo pipefail

COMPOSE="docker compose -f deploy/docker-compose.yml"
svc="${1:?用法: ./kill9.sh <api|worker|executor|harness>}"

container="$($COMPOSE ps -q "$svc")"
if [[ -z "$container" ]]; then
  echo "错误: 服务 $svc 未运行（先执行 make up）" >&2
  exit 1
fi

echo "[$(date -u +%FT%TZ)] kill -9 $svc (container=$container)"
docker kill --signal=KILL "$container"

echo "预期（mvp-落地方案 §9.4 崩溃矩阵）:"
case "$svc" in
  harness)  echo "  worker 按 Restate 重试策略重发同一 (run_id, step) 的 /runs（幂等）" ;;
  executor) echo "  重发 execute；沙箱按 sandbox_id 恢复或重建" ;;
  worker)   echo "  Restate 重放到断点：已完成 step 读缓存，断点 step 继续" ;;
  api)      echo "  无状态；SSE 客户端按 after=seq 重连续读" ;;
esac
