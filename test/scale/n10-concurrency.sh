#!/usr/bin/env bash
# ② N=10 并发实测（可信度专项）：10 个并发 run 的吞吐/compute 计时——
# duration_ms 落数据的并发路径验证。
set -euo pipefail
cd "$(dirname "$0")/../.."
API="${1:-http://localhost:8080}"
RUN_ID="n10-$(date +%s)"
AID=$(curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d '{"name":"n10","config":{"model":"m","instructions":"i","tools":[],"version":1}}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
echo "== N=10 并发实测 (API=$API RUN_ID=$RUN_ID) =="
# 10 个独立 session（同 session 的并发被单写者串行——429 实证的语义；
# 并发实测 = 独立会话的并行吞吐）
for i in $(seq 1 10); do
  SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
  echo "$SID" > /tmp/n10-sid-$i
done
T0=$(python3 -c 'import time;print(time.time())')
for i in $(seq 1 10); do
  SID=$(cat /tmp/n10-sid-$i)
  curl -fsS -m 120 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
    -H "Idempotency-Key: $RUN_ID-$i" -d "{\"input\":\"并发任务 $i\"}" -o /tmp/n10-$i.out 2>&1 &
done
wait
T1=$(python3 -c 'import time;print(time.time())')
OK=$(python3 -c '
import json
n=0
for i in range(1,11):
    try:
        d=json.load(open(f"/tmp/n10-{i}.out"))
        if d["status"]=="completed": n+=1
    except Exception: pass
print(n)')
DUR=$(python3 -c 'import sys,json
rows=json.load(open("/dev/stdin")) if False else []
import subprocess
out=subprocess.run(["docker","exec","chronotope-postgres-1","psql","-U","chronotope","-d","chronotope","-tAc","SELECT COALESCE(SUM((payload->>%s)::numeric),0) FROM events WHERE type=%s AND at > now() - interval %s","duration_ms","llm.call","10 minutes"],capture_output=True,text=True).stdout.strip()
print(out or "0")')
echo "完成 $OK/10，总耗时 $(python3 -c "print(f'{$T1-$T0:.2f}')")s，llm.call 累计 duration_ms=$DUR"
[ "$OK" = "10" ]
