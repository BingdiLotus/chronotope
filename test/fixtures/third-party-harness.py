#!/usr/bin/env python3
"""第三方 harness 的最小协议实现（Harness 装配阶段 4 的接入验证）。

模拟 Claude/OpenAI Agent SDK 适配器经 /runs 契约接入的形态：
- 纯标准库（http.server + SSE 响应）——非内置 harness/app 的实现
- 三铁律遵守：无状态（不持久化）、幂等由 worker 侧 journal（不实现缓存）、
  done 唯一终态
- 帧形状与 test/contract/testdata/runs.frames.ndjson 一致

用法: python3 test/fixtures/third-party-harness.py [端口]
"""
import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 9011


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/healthz":
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"ok")
            return
        self.send_response(404)
        self.end_headers()

    def do_POST(self):
        if self.path != "/runs":
            self.send_response(404)
            self.end_headers()
            return
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length) or b"{}")
        run_id = body.get("run_id", "r_unknown")
        step = body.get("step", 0)
        # 第三方的回复标记（证明请求到达了本 harness——路由证据）
        final = f"第三方回复（run={run_id} step={step}）"
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.end_headers()

        def sse(obj):
            self.wfile.write(f"data: {json.dumps(obj, ensure_ascii=False)}\n\n".encode())
            self.wfile.flush()

        sse({"type": "beat", "seq": 0, "payload": {}})
        sse({"type": "delta", "seq": 1, "payload": {"text": final}})
        sse({"type": "done", "seq": 2, "payload": {
            "final": final, "usage": {"tokens_in": 3, "tokens_out": 1}, "truncated": False}})

    def log_message(self, *args):
        pass  # 静默（fixture 日志不污染断言）


if __name__ == "__main__":
    srv = HTTPServer(("0.0.0.0", PORT), Handler)
    print(f"third-party-harness listening on :{PORT}", flush=True)
    srv.serve_forever()
