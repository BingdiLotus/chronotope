#!/usr/bin/env bash
# M1 审计链独立验证（审计师工具）：读导出 JSON 验证 sha256 链完整——
# 任一行被改/删即失败。用法: bash scripts/audit-verify.sh <export.json>
set -euo pipefail
FILE="${1:?用法: audit-verify.sh <export.json>}"
python3 - "$FILE" <<'PYEOF'
import sys, json, hashlib
d = json.load(open(sys.argv[1]))
events = d["events"]
prev = ""
ok = 0
for e in events:
    h = hashlib.sha256()
    h.update(prev.encode())
    h.update(b"|")
    h.update(e["type"].encode())
    h.update(b"|")
    if isinstance(e["payload"], str):
        h.update(e["payload"].encode())
    else:
        h.update(json.dumps(e["payload"], ensure_ascii=False).encode())
    want = h.hexdigest()
    if e.get("prev_hash", "") != prev or e.get("event_hash", "") != want:
        print(f"链断裂 @seq {e['seq']}: prev 期望 {prev} 实 {e.get('prev_hash','')} / hash 期望 {want} 实 {e.get('event_hash','')}")
        sys.exit(1)
    prev = want
    ok += 1
print(f"链验证通过：{ok} 条事件，末 hash {prev}")
PYEOF
