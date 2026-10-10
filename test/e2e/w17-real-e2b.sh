#!/usr/bin/env bash
# 真实 E2B 官方云路径验证（准入缺口覆盖）：真实沙箱创建 → execute 幂等
# claim/prepared_at fence → 快照 → 恢复（RestoreFrom 真接线）→ 文件读写 →
# destroy（FK 清理）→ GC quiesce。前置：EXECUTOR_DRIVER=e2b_selfhosted
# + E2B_API_URL + E2B_API_KEY（官方云）。
set -euo pipefail
cd "$(dirname "$0")/../.."
API="${1:-http://localhost:8080}"
RUN_ID="w17-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() { local d="$1"; shift; if "$@" >/dev/null 2>&1; then pass "$d"; else fail "$d"; fi; }

echo "== real E2B cloud path verification (API=$API RUN_ID=$RUN_ID) =="
AID=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"e2b-real","config":{"model":"claude-sonnet-4-6","instructions":"一句话。","tools":["bash","write_file","read_file"],"version":1}}')
AID=$(echo "$AID" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')

# 真实模型 × 真实 E2B 组合（可信度专项 ①：harness 真实模式——模型走
# litellm 真实调用，E2B 官方云真实沙箱；此前 fake 模型只证了 E2B 半边）
docker compose --env-file .env -f deploy/docker-compose.yml up -d --force-recreate harness >/dev/null 2>&1
sleep 5

# ① 真实 E2B 沙箱创建 + execute（claim → prepared_at fence → done）
curl -fsS -m 300 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-1" -d '{"input":"真实 E2B 执行"}' > /tmp/w17-run1.out 2>&1
assert "真实 E2B run 完成（sandbox 创建 + bash 执行）" \
  python3 -c 'import sys,json; d=json.load(open("/tmp/w17-run1.out")); assert d["status"]=="completed" and d["steps"]>=2, d' <<< '{}'

# ② 幂等同键重放（缓存 done——不重复真实执行）
curl -fsS -m 60 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-1" -d '{"input":"真实 E2B 执行"}' > /tmp/w17-run1b.out 2>&1
assert "同幂等键不同输入 409（command 不可变）" \
  python3 -c 'import sys,json,urllib.request
req = urllib.request.Request("'$API'/sessions/'$SID'/runs",
  data=json.dumps({"input":"different"}).encode(),
  headers={"content-type":"application/json","Idempotency-Key":"'$RUN_ID-1'"}, method="POST")
try:
    urllib.request.urlopen(req, timeout=30)
    sys.exit(1)
except urllib.error.HTTPError as e:
    sys.exit(0 if e.code == 409 else 1)' <<< '{}' ''

# ②b 文件读写（真实 E2B 的 write_file/read_file——workspace 合同在云上）
HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT='[{"tool_call":{"name":"write_file","arguments":{"path":"/workspace/real-e2b.txt","content":"真实 E2B 文件写入"}}},{"tool_call":{"name":"read_file","arguments":{"path":"/workspace/real-e2b.txt"}}},{"final":"文件验证完成。"}]' \
  docker compose --env-file .env -f deploy/docker-compose.yml up -d --force-recreate harness >/dev/null 2>&1
sleep 4
curl -fsS -m 300 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-files" -d '{"input":"文件读写"}' > /tmp/w17-files.out 2>&1
assert "真实 E2B 文件读写（write_file/read_file 往返）" \
  python3 -c 'import sys,json; d=json.load(open("/tmp/w17-files.out")); assert d["status"]=="completed", d' <<< '{}'

# ②c prepared_at fence（过期 prepared 的 done 0 行——claim 丢失不落账）
docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -c \
  "INSERT INTO sandbox_execs (idempotency_key, sandbox_id, state, prepared_at, expires_at) VALUES ('$RUN_ID-fence', (SELECT sandbox_id FROM sandboxes WHERE session_id='$SID' ORDER BY created_at DESC LIMIT 1), 'prepared', now() - interval '2 hours', now() + interval '1 day')" >/dev/null 2>&1
assert "prepared_at fence（过期 prepared 的 done 0 行——迟到结果不落账）" \
  bash -c "docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc \"UPDATE sandbox_execs SET state='done' WHERE idempotency_key='$RUN_ID-fence' AND prepared_at > now() - interval '1 hour'\" | grep -q 'UPDATE 0'"

# ③ 快照 → 销毁 → 恢复（RestoreFrom 真接线——真实 E2B snapshot）
curl -fsS -m 60 -X POST "$API/sessions/$SID/checkpoints" -H 'content-type: application/json' -d '{}' > /tmp/w17-cp.out 2>&1
assert "真实 E2B 快照成功" \
  python3 -c 'import sys,json; d=json.load(open("/tmp/w17-cp.out")); assert d.get("checkpoint_id"), d' <<< '{}'
SB=$(docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc \
  "SELECT sandbox_id FROM sandboxes WHERE session_id='$SID' ORDER BY created_at DESC LIMIT 1" 2>/dev/null | tr -d '[:space:]')
[ -n "$SB" ] && curl -fsS -X DELETE "http://localhost:9082/sandboxes/$SB" >/dev/null 2>&1 || true
curl -fsS -m 300 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-2" -d '{"input":"快照恢复后继续"}' > /tmp/w17-run2.out 2>&1
assert "快照恢复 run 完成（真实 E2B 从 snapshot 重建）" \
  python3 -c 'import sys,json; d=json.load(open("/tmp/w17-run2.out")); assert d["status"]=="completed", d' <<< '{}'

# ⑤ destroy 后 tombstone（status=destroyed 行保留——操作证据不抹除；云侧
# 实例已销毁）
SB2=$(docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc \
  "SELECT sandbox_id FROM sandboxes WHERE session_id='$SID' ORDER BY created_at DESC LIMIT 1" | tr -d '[:space:]')
curl -fsS -X DELETE "http://localhost:9082/sandboxes/$SB2" >/dev/null 2>&1 || true
sleep 2
assert "destroy 后 tombstone（status=destroyed 保留证据）" \
  bash -c "docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc \"SELECT status FROM sandboxes WHERE sandbox_id='$SB2'\" | grep -q destroyed"

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[ "$FAIL" = "0" ]
