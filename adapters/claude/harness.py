#!/usr/bin/env python3
"""Claude 官方 SDK 适配器（批 1——官方最佳实践的本地接入形态）。

经 /runs 协议接 Chronotope：
- 官方 claude-agent-sdk 的 query() 本地调用（不魔改官方 API）
- SDK 消息流 → /runs SSE 帧（assistant text → delta；tool_use →
  tool_call；result → done + usage）
- 三铁律：无状态（每次 run 从 /runs 入参重建 query——不持久化 SDK 会话）
- 最佳实践映射：permission_mode 与 Chronotope class 分级取严（SDK 权限
  拒绝 → error 帧；Chronotope 侧审批在平台承担）；usage → 账本（done 帧
  的 usage 字段——平台自动落 llm_calls）

官方参考：https://code.claude.com/docs/en/agent-sdk/quickstart
用法: python3 adapters/claude/harness.py [端口]
"""
import asyncio
import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from claude_agent_sdk import ClaudeAgentOptions, query

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 8010
MARK = "ClaudeSDK"  # 回复标记（路由证据——e2e 断言用）


def sse_frames(body):
    """把 /runs 请求体转成 SSE 帧序列的异步生成器（SDK 官方调用）。"""
    async def gen():
        messages = body.get("messages", [])
        system = next((m.get("content", "") for m in messages if m.get("role") == "system"), "")
        prompt = "\n".join(m.get("content", "") for m in messages if m.get("role") == "user")
        tools = body.get("tools", [])
        opts = ClaudeAgentOptions(
            system_prompt=system,
            model=body.get("model", "claude-sonnet-4-6"),
            allowed_tools=[t for t in tools if not t.startswith("mcp:")],
            permission_mode="default",
            # 官方最佳实践：max_turns 有界——SDK 循环不无限（Chronotope 的
            # step 语义在平台侧）
            max_turns=8,
        )
        seq = 0
        yield {"type": "beat", "seq": seq, "payload": {}}
        final_parts = []
        usage = {}
        try:
            async for msg in query(prompt=prompt, options=opts):
                t = type(msg).__name__
                if t == "AssistantMessage":
                    for b in getattr(msg, "content", []) or []:
                        bn = type(b).__name__
                        if bn == "TextBlock":
                            seq += 1
                            final_parts.append(b.text)
                            yield {"type": "delta", "seq": seq, "payload": {"text": b.text}}
                        elif bn == "ToolUseBlock":
                            seq += 1
                            yield {"type": "tool_call", "seq": seq, "payload": {
                                "id": getattr(b, "id", "t_sdk"), "name": b.name,
                                "arguments": getattr(b, "input", {}),
                            }}
                    if isinstance(getattr(msg, "usage", None), dict):
                        usage = msg.usage
                elif t == "ResultMessage":
                    final_parts.append(f"[{MARK}] {msg.subtype}")
                    if isinstance(getattr(msg, "usage", None), dict):
                        usage = msg.usage
                    break
        except Exception as e:  # SDK 错误 → error 帧（Chronotope 的 unknown 账本）
            seq += 1
            yield {"type": "error", "seq": seq, "payload": {"error": str(e)}}
            return
        seq += 1
        final = "".join(final_parts).strip() or f"{MARK} 回复"
        tokens_in = usage.get("input_tokens", 0) or 0
        tokens_out = usage.get("output_tokens", 0) or 0
        yield {"type": "done", "seq": seq, "payload": {
            "final": final,
            "usage": {"tokens_in": tokens_in, "tokens_out": tokens_out},
            "truncated": False,
        }}
    return gen()


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
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.end_headers()

        async def pump():
            async for frame in sse_frames(body):
                self.wfile.write(
                    f"data: {json.dumps(frame, ensure_ascii=False)}\n\n".encode())
                self.wfile.flush()

        asyncio.run(pump())

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    srv = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    print(f"claude-sdk-harness listening on :{PORT}", flush=True)
    srv.serve_forever()
