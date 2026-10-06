#!/usr/bin/env python3
"""事件投递接收器夹具：记录收到的 POST（事件 JSON），供 w8-notify.sh 断言。

端点：POST / → 追加一行 ndjson 到 OUT（默认 /tmp/notify-sink.ndjson）；GET /dump → 全量。
用法: python3 test/fixtures/notify-sink.py [port] [outfile]
"""
import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

OUT = None


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(n)
        with open(OUT, "a") as f:
            f.write(body.decode("utf-8", "replace") + "\n")
        self.send_response(200)
        self.end_headers()

    def do_GET(self):
        data = ""
        try:
            with open(OUT) as f:
                data = f.read()
        except FileNotFoundError:
            pass
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.end_headers()
        self.wfile.write(data.encode())

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 9300
    OUT = sys.argv[2] if len(sys.argv) > 2 else "/tmp/notify-sink.ndjson"
    open(OUT, "w").close()  # 清空
    print(f"notify sink listening :{port} -> {OUT}", flush=True)
    HTTPServer(("0.0.0.0", port), Handler).serve_forever()
