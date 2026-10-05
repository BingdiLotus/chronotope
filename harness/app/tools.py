"""工具分流（落地方案 §9.2）：API 类工具内联执行并继续循环；
CODE 类工具发出 tool_call 帧交棒（worker → executor，harness 从不直连 executor）。"""

from __future__ import annotations

import json
from typing import Any

import httpx

# API 类工具（harness 内联执行）；web_search 需要检索密钥（W1 后置，见 run_api_tool）。
# 注意：控制类工具（request_approval/spawn_subagent）与代码类工具一样交棒，
# 定义在 protocol.CONTROL_TOOLS / protocol.CODE_TOOLS，不在此处内联。
API_TOOLS = {"http_request", "web_search"}


async def run_api_tool(name: str, arguments: dict[str, Any]) -> str:
    """执行 API 类工具，返回 JSON 字符串结果（作为 tool 消息回喂模型）。"""
    if name == "http_request":
        return await _http_request(arguments)
    if name == "web_search":
        return json.dumps({"error": "web_search 未配置（W1 后置，需检索 API key）"}, ensure_ascii=False)
    return json.dumps({"error": f"未知 API 工具 {name}"}, ensure_ascii=False)


async def _http_request(arguments: dict[str, Any]) -> str:
    url = arguments.get("url", "")
    if not url:
        return json.dumps({"error": "http_request 缺 url"}, ensure_ascii=False)
    method = arguments.get("method", "GET").upper()
    if method != "GET":
        # 副作用类请求（POST/PUT/DELETE）必须走平台 executor——边界语义 §3：API 工具只读/幂等
        return json.dumps(
            {"error": f"http_request 仅支持 GET（{method} 属副作用，请走 bash/executor）"},
            ensure_ascii=False,
        )
    timeout = float(arguments.get("timeout", 10))
    try:
        async with httpx.AsyncClient(timeout=timeout) as client:
            resp = await client.get(url)
        body = resp.text[:2000]
        return json.dumps({"status": resp.status_code, "body": body}, ensure_ascii=False)
    except httpx.HTTPError as exc:
        return json.dumps({"error": str(exc)}, ensure_ascii=False)
