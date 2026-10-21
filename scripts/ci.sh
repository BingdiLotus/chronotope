#!/usr/bin/env bash
# 本地 CI 门禁（.github/workflows/ci.yml 的本地等价物）：
#   gofmt → build → vet → test（store 集成 + golden journal 重放）→ harness → spike
#   --e2e：追加当前应用执行门禁（W1 202/GET + application runtime 故障恢复）
# 用法: bash scripts/ci.sh [--e2e]
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

step "application contract（fixture + fake receiver）"
go test ./test/contract/... ./internal/targets/... -count=1
ok "application contract"

step "go test（单测 + store 集成 + golden journal 重放）"
if [ -n "${STORE_TEST_DATABASE_URL:-}" ]; then
  go test ./...
elif docker exec chronotope-postgres-1 pg_isready -U chronotope > /dev/null 2>&1; then
  # Keep integration-test outboxes away from the running application dispatcher.
  if [ "$(docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc "SELECT 1 FROM pg_database WHERE datname='chronotope_test'")" != "1" ]; then
    docker exec chronotope-postgres-1 createdb -U chronotope chronotope_test
  fi
  STORE_TEST_DATABASE_URL='postgres://chronotope:chronotope_dev@localhost:5432/chronotope_test?sslmode=disable' \
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
  step "e2e：当前应用门禁（scripts/demo.sh，W1 + application runtime，fake 模型）"
  bash scripts/demo.sh
  ok "e2e"
fi

echo -e "\n\033[1;32m== 本地 CI 全部通过 ==\033[0m"
