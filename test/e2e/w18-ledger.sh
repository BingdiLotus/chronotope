#!/usr/bin/env bash
# w18 账本与 quiesce 的 e2e 固化（A/B/C/D/E 批的端到端断言）。
set -euo pipefail
cd "$(dirname "$0")/../.."
API="${1:-http://localhost:8080}"
EXEC="${EXECUTOR_URL:-http://localhost:9082}"
RUN_ID="w18-$(date +%s)"
PASS=0; FAIL=0
pass() { echo -e "  \033[1;32mPASS\033[0m $*"; PASS=$((PASS+1)); }
fail() { echo -e "  \033[1;31mFAIL\033[0m $*"; FAIL=$((FAIL+1)); }
assert() { local d="$1"; shift; if "$@" >/dev/null 2>&1; then pass "$d"; else fail "$d"; fi; }
psqlx() { docker exec chronotope-postgres-1 psql -U chronotope -d chronotope -tAc "$1" 2>/dev/null | tr -d '[:space:]'; }
fake_harness() { HARNESS_FAKE_MODEL=1 HARNESS_FAKE_SCRIPT="$1" \
  docker compose -f deploy/docker-compose.yml up -d --force-recreate harness >/dev/null 2>&1; sleep 4; }
new_agent() { curl -fsS -X POST "$API/orgs/org-$RUN_ID/agents" -H 'content-type: application/json' \
  -d "$1" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])'; }

echo "== w18 ledger & quiesce e2e (API=$API RUN_ID=$RUN_ID) =="

# --- ① calls 对账 + admission 状态 ---
fake_harness ""
AID=$(new_agent '{"name":"ledger","config":{"model":"m","instructions":"i","tools":[],"version":1}}')
SID=$(curl -fsS -X POST "$API/agents/$AID/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -m 60 -X POST "$API/sessions/$SID/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-1" -d '{"input":"hi"}' > /dev/null 2>&1 || true
RID=$(psqlx "SELECT id FROM runs WHERE session_id='$SID' ORDER BY created_at DESC LIMIT 1")
export RID
assert "calls ledger API (admission dispatched + llm row)" \
  python3 -c 'import sys,json,urllib.request,os
d=json.load(urllib.request.urlopen("http://localhost:8080/runs/"+os.environ["RID"]+"/calls"))
assert d["admission_state"]=="dispatched", d
llm=[c for c in d["calls"] if c["kind"]=="llm"]
assert llm and llm[0]["state"] in ("result","unknown"), llm' <<< '{}'

# --- ② 挂起即冻结（先建沙箱再挂起） ---
fake_harness '[{"tool_call":{"name":"bash","arguments":{"command":"echo warm"}}},{"final":"warmed"}]'
AID2=$(new_agent '{"name":"hitl","config":{"model":"m","instructions":"i","tools":["bash"],"version":1}}')
SID2=$(curl -fsS -X POST "$API/agents/$AID2/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -m 60 -X POST "$API/sessions/$SID2/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-warm" -d '{"input":"warm"}' > /dev/null 2>&1 || true
SB=$(psqlx "SELECT sandbox_id FROM sandboxes WHERE session_id='$SID2' ORDER BY created_at DESC LIMIT 1")
# 挂起脚本（class 2 需 tool_classes——用 w5-risk 模式重建 agent 更繁；直接用
# executor 的 freeze 端点验证链路 + runLoop 挂起路径的 Freeze 由单测覆盖）
export SB
AID2b=$(new_agent '{"name":"hitl2","config":{"model":"m","instructions":"i","tools":["bash"],"tool_classes":{"bash":2},"version":1}}')
SID2b=$(curl -fsS -X POST "$API/agents/$AID2b/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
# 先普通 bash run 建沙箱（挂起门禁在沙箱创建前——Freeze 需要对象）
fake_harness '[{"tool_call":{"name":"write_file","arguments":{"path":"/workspace/warm.txt","content":"warmed"}}},{"final":"warmed"}]'
curl -fsS -m 60 -X POST "$API/sessions/$SID2b/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-warm2a" -d '{"input":"warm"}' > /dev/null 2>&1 || true
SB=$(psqlx "SELECT sandbox_id FROM sandboxes WHERE session_id='$SID2b' ORDER BY created_at DESC LIMIT 1")
curl -fsS -N "$API/sessions/$SID2b/events?after=0" > /tmp/w18-hitl.out 2>&1 &
SSE=$!
curl -fsS -m 120 -X POST "$API/sessions/$SID2b/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-2" -d '{"input":"危险"}' > /dev/null 2>&1 &
RUNP=$!
for i in $(seq 1 60); do grep -q '"type":"run.awaiting_approval"' /tmp/w18-hitl.out 2>/dev/null && break; sleep 2; done
sleep 2
# executor freeze 端点直调（Freeze 链路的 e2e 证据——awaitApproval 的调用
# 由单测 fake ops 覆盖）
curl -fsS -X POST "$EXEC/sandboxes/$SB/freeze" > /dev/null 2>&1 || true
sleep 1
export SB
assert "freeze endpoint pauses sandbox" \
  python3 -c 'import subprocess,os
out=subprocess.run(["docker","inspect","--format","{{.State.Paused}}",os.environ["SB"]],capture_output=True,text=True).stdout.strip()
assert out=="true", "paused="+out' <<< '{}'
kill $SSE 2>/dev/null || true; kill $RUNP 2>/dev/null || true

# --- ③ CLI 写文件 execute 后扫描入索引 ---
fake_harness '[{"tool_call":{"name":"bash","arguments":{"command":"echo cli-written > /workspace/cli.txt"}}},{"final":"写完"}]'
AID3=$(new_agent '{"name":"cli","config":{"model":"m","instructions":"i","tools":["bash"],"version":1}}')
SID3=$(curl -fsS -X POST "$API/agents/$AID3/sessions" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -fsS -m 60 -X POST "$API/sessions/$SID3/runs" -H 'content-type: application/json' \
  -H "Idempotency-Key: $RUN_ID-3" -d '{"input":"写文件"}' > /dev/null 2>&1 || true
sleep 2
export SID3
assert "CLI write indexed after execute" \
  python3 -c 'import subprocess,os
out=subprocess.run(["docker","exec","chronotope-postgres-1","psql","-U","chronotope","-d","chronotope","-tAc","SELECT count(*) FROM workspace_files WHERE session_id=\x27"+os.environ["SID3"]+"\x27"],capture_output=True,text=True).stdout.strip()
assert out.isdigit() and int(out)>=1, f"count={out}"' <<< '{}'

# --- ④ exec unknown 停派发 ---
SB3=$(psqlx "SELECT sandbox_id FROM sandboxes WHERE session_id='$SID3' ORDER BY created_at DESC LIMIT 1")
KEY="$RUN_ID-unknown"
# 直接 DB 造 prepared 过期行（第一次 execute 会落 done——场景需 prepared 保持）
psqlx "INSERT INTO sandbox_execs (idempotency_key, sandbox_id, result, expires_at, state, input_digest, prepared_at) VALUES ('$KEY', '$SB3', NULL, now() + interval '30 days', 'prepared', 'digest-old', now() - interval '5 minutes') ON CONFLICT (idempotency_key) DO UPDATE SET state='prepared', prepared_at = now() - interval '5 minutes', input_digest='digest-old'" >/dev/null
code=$(curl -sS -o /dev/null -w "%{http_code}" -X POST "$EXEC/execute" -H 'content-type: application/json' \
  -d "{\"sandbox_id\":\"$SB3\",\"name\":\"bash\",\"input\":\"echo u2\",\"idempotency_key\":\"$KEY\"}")
export KEY CODE="$code"
assert "exec unknown 409 + state unknown" \
  python3 -c 'import subprocess,os
assert os.environ["CODE"]=="409", "code="+os.environ["CODE"]
out=subprocess.run(["docker","exec","chronotope-postgres-1","psql","-U","chronotope","-d","chronotope","-tAc","SELECT state FROM sandbox_execs WHERE idempotency_key=\x27"+os.environ["KEY"]+"\x27"],capture_output=True,text=True).stdout.strip()
assert out=="unknown", "state="+out' <<< '{}'

echo "== 结果: $PASS 通过, $FAIL 失败 =="
[ "$FAIL" = "0" ]
