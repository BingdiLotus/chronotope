#!/usr/bin/env python3
"""Codex 官方 SDK 适配器（批 2——官方最佳实践的本地接入形态）。

经 /runs 协议接 Chronotope：
- 官方 openai-codex（Python 稳定版）本地调用：Codex().thread_start() +
  thread.run(prompt) → final_response
- 三铁律：无状态（每次 run 新建 thread——SDK 线程不持久化到 Chronotope）
- 最佳实践映射：Sandbox 预设 ↔ class 分级取严（read_only↔class 0、
  workspace_write↔class 1 默认、full_access↔class 2 审批在平台侧）；
  usage → done 帧（账本自动落）

官方参考：https://learn.chatgpt.com/docs/codex-sdk
用法: python3 adapters/codex/harness.py [端口]
"""
import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from openai_codex import Codex, Sandbox

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 8020
MARK = "CodexSDK"  # 回复标记（路由证据——e2e 断言用）


def run_codex(body):
    """官方调用：thread 新建 → run → final_response（不魔改官方 API）。"""
    messages = body.get("messages", [])
    system = next((m.get("content", "") for m in messages if m.get("role") == "system"), "")
    prompt = "\n".join(m.get("content", "") for m in messages if m.get("role") == "user")
    if system:
        prompt = f"{system}\n\n{prompt}"
    sandbox = Sandbox.workspace_write  # 默认 class 1 等价（取严——平台分级为准）
    try:
        with Codex() as codex:
            thread = codex.thread_start(sandbox=sandbox)
            result = thread.run(prompt)
            final = getattr(result, "final_response", "") or ""
            usage = {}
            if hasattr(result, "usage"):
                u = result.usage
                usage = {"tokens_in": getattr(u, "input_tokens", 0) or 0,
                         "tokens_out": getattr(u, "output_tokens", 0) or 0}
            return final, usage, None
    except Exception as e:
        return "", {}, str(e)


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
        final, usage, err = run_codex(body)
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.end_headers()

        def sse(obj):
            self.wfile.write(f"data: {json.dumps(obj, ensure_ascii=False)}\n\n".encode())
            self.wfile.flush()

        sse({"type": "beat", "seq": 0, "payload": {}})
        if err:
            sse({"type": "error", "seq": 1, "payload": {"error": err}})
            return
        sse({"type": "delta", "seq": 1, "payload": {"text": final}})
        sse({"type": "done", "seq": 2, "payload": {
            "final": f"{final} [{MARK}]".strip(),
            "usage": usage, "truncated": False}})

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    srv = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    print(f"codex-sdk-harness listening on :{PORT}", flush=True)
    srv.serve_forever()
