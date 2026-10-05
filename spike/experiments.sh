#!/usr/bin/env bash
# W1 D1 spike 实验驱动脚本。
# 前置：docker compose 已起 restate；本脚本负责起桩/端点、注册部署、逐项实验。
# 用法: bash experiments.sh [all|1|2|3|4|5]
set -euo pipefail

INGRESS=http://localhost:8081
ADMIN=http://localhost:9070
STUB=http://localhost:19001
BIN=./spike-server
DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$DIR"

log()  { echo -e "\n\033[1;36m== $*\033[0m"; }
pass() { echo -e "\033[1;32m   PASS: $*\033[0m"; }
fail() { echo -e "\033[1;31m   FAIL: $*\033[0m"; exit 1; }

register() { # register <uri> <version>
  curl -sS -X POST "$ADMIN/deployments" -H 'content-type: application/json' \
    -d "{\"uri\":\"$1\",\"version\":\"$2\",\"use_http_11\":true,\"force\":true}" > /dev/null
}

pids=()
cleanup() {
  for p in "${pids[@]:-}"; do kill "$p" 2>/dev/null || true; done
  # kill -9 场景下由实验自行管理端点进程
}
trap cleanup EXIT

# 每次运行唯一 key：避免历史失败运行遗留的挂起 invocation 占用同名 object key
RUN_ID="$(date +%s)"

# 干净启动：清掉 spike-server 进程与既有部署（孤儿挂起 invocation 随之失效，避免污染计数）
pkill -f spike-server 2>/dev/null || true
sleep 0.5
for dep in $(curl -sS -H 'accept: application/json' "$ADMIN/deployments" \
    | python3 -c 'import sys, json; print(" ".join(d["id"] for d in json.load(sys.stdin)["deployments"]))'); do
  # Restate 1.7 只支持强制删除（?force=true）
  curl -sS -X DELETE "$ADMIN/deployments/$dep?force=true" > /dev/null
done

# 起桩（独立进程，常驻；跨 kill -9 保留命中计数）
nohup "$BIN" -stub-only > /tmp/spike-stub.log 2>&1 &
pids+=($!)
sleep 1
pass "stub harness up"

exp="${1:-all}"

# ---------------------------------------------------------------- 实验①
run_exp1() {
  log "实验① Run 闭包内分钟级 SSE 长流（dur=8s；step2 进行中 kill -9 端点）"
  nohup env SPIKE_VERSION=v1 "$BIN" -addr :9080 > /tmp/spike-v1.log 2>&1 &
  EP=$!
  sleep 1
  register "http://host.docker.internal:9080" "v1"

  curl -sS -X POST "$INGRESS/sseprobe/ProbeTwoSteps" \
    -H 'content-type: application/json' -d '{"dur_sec":8}' > /tmp/exp1.out &
  CPID=$!
  sleep 3                      # step1 已完成、step2（长流）进行中
  kill -9 "$EP"                # 端点进程崩溃
  sleep 1
  nohup env SPIKE_VERSION=v1 "$BIN" -addr :9080 > /tmp/spike-v1.log 2>&1 &
  pids+=($!)
  wait "$CPID" || true

  echo "   result: $(cat /tmp/exp1.out)"
  echo "   stub:   $(curl -fsS "$STUB/stats")"
  # 断言：/once 命中 1（已完成 step 不重放）；/stream 命中 2（未完成 step 幂等重发一次）
  stats=$(curl -fsS "$STUB/stats")
  echo "$stats" | grep -q '"/once":1' || fail "① step1 应只命中一次（不重调）: $stats"
  echo "$stats" | grep -q '"/stream?dur=8":2' || fail "① step2 应重发恰好一次: $stats"
  pass "实验①：长流 journal 成功；崩溃后已完成 step 不重放、未完成 step 幂等重发一次"
}

# ---------------------------------------------------------------- 实验②
run_exp2() {
  log "实验② awakeable 跨 HTTP resolve"
  nohup env SPIKE_VERSION=v1 "$BIN" -addr :9080 > /tmp/spike-v1.log 2>&1 &
  pids+=($!)
  sleep 1
  register "http://host.docker.internal:9080" "v1"
  local key="a1-$RUN_ID"

  curl -sS -X POST "$INGRESS/approval/$key/RequestApproval" \
    -H 'content-type: application/json' -d '"question?"' > /tmp/exp2-wait.out &
  WPID=$!
  sleep 2
  id=$(curl -sS -X POST "$INGRESS/approval/$key/PendingID" \
    -H 'content-type: application/json' -d '""')
  echo "   awakeable id: $id"
  [ -n "$id" ] || fail "② 未取到 awakeable id"
  # resolve 必须来自对象之外的 invocation（独立 approver Service）：
  # 同对象 exclusive handler 挂起时仍占排他锁，对象内 Resolve 会死锁（见 services.go 注释）
  res=$(curl -sS -X POST "$INGRESS/approver/Resolve" \
    -H 'content-type: application/json' -d "{\"id\":$id,\"payload\":\"approved-ok\"}")
  echo "   resolve: $res"
  wait "$WPID" || true
  out=$(cat /tmp/exp2-wait.out)
  echo "   pending result: $out"
  [[ "$out" == '"approved-ok"' ]] || fail "② 挂起方应收到解析值 approved-ok: $out"
  pass "实验②：awakeable 建/挂起/跨 HTTP resolve 全通"
}

# ---------------------------------------------------------------- 实验③
run_exp3() {
  log "实验③ child workflow 调用/await（父 await 中 kill -9）"
  nohup env SPIKE_VERSION=v1 "$BIN" -addr :9080 > /tmp/spike-v1.log 2>&1 &
  EP=$!
  pids+=($!)
  sleep 1
  register "http://host.docker.internal:9080" "v1"
  local key="p1-$RUN_ID"

  curl -sS -X POST "$INGRESS/parentflow/$key/run" \
    -H 'content-type: application/json' -d '{"child_delay_ms":6000}' > /tmp/exp3.out &
  CPID=$!
  sleep 2                      # 子 workflow 已开始（已打桩 /once）
  kill -9 "$EP"
  sleep 1
  nohup env SPIKE_VERSION=v1 "$BIN" -addr :9080 > /tmp/spike-v1.log 2>&1 &
  pids+=($!)
  wait "$CPID" || true
  echo "   result: $(cat /tmp/exp3.out)"
  stats=$(curl -fsS "$STUB/stats")
  echo "   stub:   $stats"
  # 断言：子 workflow 的 /once 只命中一次（重放不重复派发 child）
  local label="of-$key"
  echo "$stats" | grep -q "label=$label" || fail "③ 子 workflow 应打桩: $stats"
  [[ $(echo "$stats" | grep -o "\"/once?label=$label\":[0-9]*" | grep -o '[0-9]*$') == "1" ]] \
    || fail "③ 子 workflow 不应重复派发: $stats"
  pass "实验③：child workflow await 全通；父崩溃重放不重复派发子 workflow"
}

# ---------------------------------------------------------------- 实验④
run_exp4() {
  log "实验④ endpoint versioning（新 invocation 走 v2、在途留 v1）"
  nohup env SPIKE_VERSION=v1 "$BIN" -addr :9080 > /tmp/spike-v1.log 2>&1 &
  pids+=($!)
  nohup env SPIKE_VERSION=v2 "$BIN" -addr :9081 > /tmp/spike-v2.log 2>&1 &
  pids+=($!)
  sleep 1
  register "http://host.docker.internal:9080" "v1"
  sleep 1

  # 在途 invocation 落在 v1
  curl -sS -X POST "$INGRESS/versionflow/vf1-$RUN_ID/run" \
    -H 'content-type: application/json' -d '{"sleep_ms":8000}' > /tmp/exp4-inflight.out &
  IPID=$!
  sleep 2

  # 注册 v2 → 新 invocation 应走 v2
  register "http://host.docker.internal:9081" "v2"
  new=$(curl -sS -X POST "$INGRESS/pingpong/ping")   # Void 输入：空 body、无 content-type
  echo "   new invocation → $new"
  [[ "$new" == *'"version":"v2"'* ]] || fail "④ 新 invocation 应走 v2: $new"

  wait "$IPID" || true
  inflight=$(cat /tmp/exp4-inflight.out)
  echo "   in-flight → $inflight"
  [[ "$inflight" == *'"version":"v1"'* ]] || fail "④ 在途 invocation 应留 v1: $inflight"
  pass "实验④：新走 v2、在途留 v1 验证通过"
}

# ---------------------------------------------------------------- 实验⑤
run_exp5() {
  log "实验⑤ journal/state 条目大小量级（每档独立 invocation）"
  nohup env SPIKE_VERSION=v1 "$BIN" -addr :9080 > /tmp/spike-v1.log 2>&1 &
  pids+=($!)
  sleep 1
  register "http://host.docker.internal:9080" "v1"

  printf "   %-8s %s\n" "KB" "结果"
  for kb in 16 64 256 1024 2048 4096 8192 16384; do
    out=$(curl -sS -m 60 -X POST "$INGRESS/sizeprobe/s$kb-$RUN_ID/run" \
      -H 'content-type: application/json' -d "{\"kb\":$kb}" || echo '{"err":"curl failed"}')
    printf "   %-8s %s\n" "$kb" "$out"
  done
  pass "实验⑤：结果见上表（结论填入 README 表格）"
}

# ---------------------------------------------------------------- 启动编排
case "$exp" in
  1) run_exp1 ;;
  2) run_exp2 ;;
  3) run_exp3 ;;
  4) run_exp4 ;;
  5) run_exp5 ;;
  all) run_exp1; run_exp2; run_exp3; run_exp4; run_exp5 ;;
  *) echo "用法: $0 [all|1|2|3|4|5]"; exit 2 ;;
esac

log "全部实验完成（结论汇总到 README.md 结论表格）"
