#!/usr/bin/env python3
"""MCP 测试夹具：HTTP transport 的最小 JSON-RPC 2.0 MCP 服务器。

工具：echo（回显 text）、add（整数求和）。供 w6-mcp.sh e2e 使用。
用法: python3 test/fixtures/mcp-server.py [port]（默认 9100）
"""
import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(n) or b"{}")
        method = body.get("method")
        if method == "initialize":
            result = {"protocolVersion": "2024-11-05", "serverInfo": {"name": "fixture"}}
        elif method == "tools/list":
            result = {
                "tools": [
                    {
                        "name": "echo",
                        "description": "回显输入文本",
                        "inputSchema": {"type": "object", "properties": {"text": {"type": "string"}}, "required": ["text"]},
                    },
                    {
                        "name": "add",
                        "description": "两整数求和",
                        "inputSchema": {"type": "object", "properties": {"a": {"type": "integer"}, "b": {"type": "integer"}}, "required": ["a", "b"]},
                    },
                ]
            }
        elif method == "tools/call":
            params = body.get("params", {})
            name = params.get("name")
            args = params.get("arguments", {})
            if name == "echo":
                text = f"echo: {args.get('text', '')}"
            elif name == "add":
                text = f"sum: {int(args.get('a', 0)) + int(args.get('b', 0))}"
            else:
                result = {"content": [], "isError": True}
                self._reply(result)
                return
            result = {"content": [{"type": "text", "text": text}], "isError": False}
        else:
            result = None
            err = {"code": -32601, "message": f"unknown method {method}"}
            self._reply(result, err)
            return
        self._reply(result)

    def _reply(self, result, err=None):
        out = {"jsonrpc": "2.0", "id": 1}
        if err:
            out["error"] = err
        else:
            out["result"] = result
        data = json.dumps(out).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *args):
        pass  # 静默访问日志


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 9100
    print(f"mcp fixture listening :{port}", flush=True)
    HTTPServer(("0.0.0.0", port), Handler).serve_forever()
