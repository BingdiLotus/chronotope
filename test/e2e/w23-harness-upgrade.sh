#!/usr/bin/env bash
# Harness 装配阶段 3（升级）：旧 run 沿旧、新 run 走新——两个真实 harness
# 实体（8001/8002）+ bound 快照冻结断言 + 路由标记差异。
set -euo pipefail
cd "$(dirname "$0")/../.."
API="${1:-http://localhost:8080}"
RUN_ID="hu-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() { local d="$1"; shift; if "$@" >/dev/null 2>&1; then pass "$d"; else fail "$d"; fi; }

echo "== W23 Harness 升级 (API=$API RUN_ID=$RUN_ID) =="
# ① 两个真实 harness 实体（同镜像、不同端口与 fake 脚本标记）
docker run -d --rm --name hs-v1 -p 8001:8000 --network chronotope_default \
  -e HARNESS_FAKE_MODEL=1 -e 'HARNESS_FAKE_SCRIPT=[{"final":"来自 V1 的回复"}]' chronotope-harness > /dev/null 2>&1 || true
docker run -d --rm --name hs-v2 -p 8002:8000 --network chronotope_default \
  -e HARNESS_FAKE_MODEL=1 -e 'HARNESS_FAKE_SCRIPT=[{"final":"来自 V2 的回复"}]' chronotope-harness > /dev/null 2>&1 || true
sleep 5
for i in $(seq 1 20); do curl -fsS http://localhost:8001/healthz > /dev/null 2>&1 && curl -fsS http://localhost:8002/healthz > /dev/null 2>&1 && break; sleep 1; done
assert "两 harness 实体就绪（8001/8002）" \
  bash -c "curl -fsS http://localhost:8001/healthz && curl -fsS http://localhost:8002/healthz"

# ② 注册 v1 + agent 绑定
curl -fsS -X POST "$API/orgs/org-$RUN_ID/harnesses" -H 'content-type: application/json' \
  -d '{"name":"upgrade","endpoint":"http://host.docker.internal:8001","version":"v1","capabilities":[]}' > /dev/null
AID=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"hu","config":{"model":"m","instructions":"i","tools":[],"harness_ref":{"name":"upgrade"},"version":1}}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')

# ③ run A（旧版本 v1）
curl -fsS -m 60 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-a" -d '{"input":"hi"}' -o /tmp/hu-a.out 2>&1 || true
RID_A=$(docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc \
  "SELECT id FROM runs WHERE session_id='$SID' ORDER BY created_at DESC LIMIT 1" | tr -d '[:space:]')
assert "run A 完成（final 含 V1 标记——旧版本路由）" \
  python3 -c 'import json; d=json.load(open("/tmp/hu-a.out")); assert "V1" in d.get("final",""), d' <<< '{}'
assert "run A 快照冻结 8001（旧 run 沿旧）" \
  bash -c "docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc \"SELECT bound->>'harness_endpoint' FROM runs WHERE id='$RID_A'\" | grep -q 8001"

# ④ 升级：注册 v2 + v1 切 draining
curl -fsS -X POST "$API/orgs/org-$RUN_ID/harnesses" -H 'content-type: application/json' \
  -d '{"name":"upgrade","endpoint":"http://host.docker.internal:8002","version":"v2","capabilities":[]}' > /dev/null
curl -fsS -X PUT "$API/orgs/org-$RUN_ID/harnesses/upgrade/v1/state" -H 'content-type: application/json' \
  -d '{"state":"draining"}' > /dev/null

# ⑤ run B（新版本 v2）
curl -fsS -m 60 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-b" -d '{"input":"hi2"}' -o /tmp/hu-b.out 2>&1 || true
RID_B=$(docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc \
  "SELECT id FROM runs WHERE session_id='$SID' ORDER BY created_at DESC LIMIT 1" | tr -d '[:space:]')
assert "run B 完成（final 含 V2 标记——新版本路由）" \
  python3 -c 'import json; d=json.load(open("/tmp/hu-b.out")); assert "V2" in d.get("final",""), d' <<< '{}'
assert "run B 快照冻结 8002（新 run 走新）" \
  bash -c "docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc \"SELECT bound->>'harness_endpoint' FROM runs WHERE id='$RID_B'\" | grep -q 8002"

docker rm -f hs-v1 hs-v2 > /dev/null 2>&1 || true
echo "== 结果: $PASS 通过, $FAIL 失败 =="
[ "$FAIL" = "0" ]
