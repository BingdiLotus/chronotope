#!/usr/bin/env bash
# 本地 CI 门禁（.github/workflows/ci.yml 的本地等价物）：
#   gofmt → build → vet → test（store 集成 + golden journal 重放）→ harness → spike
#   --e2e：追加 compose 全链路 W1–W3 场景（scripts/demo.sh）
# 用法: bash scripts/ci.sh [--e2e]
set -euo pipefail
cd "$(dirname "$0")/.."

step() { echo -e "\n\033[1;36m== $* ==\033[0m"; }
ok() { echo -e "  \033[1;32m✓\033[0m $*"; }

# --- go ---
step "gofmt 检查（cmd/internal/test）"
UNFORMATTED=$(gofmt -l cmd internal test || true)
if [ -n "$UNFORMATTED" ]; then
  echo "未格式化文件:"; echo "$UNFORMATTED"; exit 1
fi
ok "gofmt"

step "go build"
go build ./...
ok "build"

step "go vet"
go vet ./...
ok "vet"

step "go test（单测 + store 集成 + golden journal 重放）"
if [ -n "${STORE_TEST_DATABASE_URL:-}" ]; then
  go test ./...
elif docker exec chronotope-postgres-1 pg_isready -U chronotope > /dev/null 2>&1; then
  # 本机 compose postgres 可达 → store 集成测试实跑（与 CI 服务容器一致）
  STORE_TEST_DATABASE_URL='postgres://chronotope:chronotope_dev@localhost:5432/chronotope?sslmode=disable' \
    go test ./...
else
  echo "  未探测到 postgres：store 集成测试将自跳过（CI 中有服务容器，必跑）"
  go test ./...
fi
ok "test"

# --- harness ---
step "harness pytest（/runs 契约测试）"
(cd harness && uv run pytest -q)
ok "harness"

# --- spike（独立 module）---
step "spike（GOWORK=off build + test）"
(cd spike && GOWORK=off go build ./... && GOWORK=off go test ./...)
ok "spike"

# --- e2e（可选，全链路）---
if [ "${1:-}" = "--e2e" ]; then
  step "e2e：compose 全链路 W1–W3（scripts/demo.sh，fake 模型无密钥）"
  bash scripts/demo.sh
  ok "e2e"
fi

echo -e "\n\033[1;32m== 本地 CI 全部通过 ==\033[0m"
