#!/usr/bin/env python3
"""w8-notify 断言夹具：校验接收器文件的事件行（会话归属 + 时间戳）。"""
import json
import sys

sid = sys.argv[1]
rows = []
for line in open("/tmp/notify-sink.ndjson"):
    try:
        d = json.loads(line)
    except json.JSONDecodeError:
        continue  # 探针行跳过
    if "session_id" in d:
        rows.append(d)
mine = [r for r in rows if r["session_id"] == sid]
assert len(mine) >= 2, f"本会话事件行不足: {len(mine)}"
assert all(r.get("at") for r in mine), mine[:1]
print(f"本会话事件行 {len(mine)} 条，会话归属与时间戳校验通过")
