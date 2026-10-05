"""OpenAIProvider 单测：MockTransport 桩替 LiteLLM 网关，验证请求形状与流式解析。"""

from __future__ import annotations

import json

import httpx
import pytest
from openai import AsyncOpenAI

from app import protocol as p
from app.llm import OpenAIProvider, openai_tools


def make_provider(handler) -> OpenAIProvider:
    """构造指向 MockTransport 的 provider（不走网络）。"""
    client = AsyncOpenAI(
        base_url="http://stub", api_key="sk-test",
        http_client=httpx.AsyncClient(transport=httpx.MockTransport(handler)),
    )
    provider = OpenAIProvider.__new__(OpenAIProvider)
    provider.client = client
    return provider


async def test_request_shape_and_stream_parsing():
    captured = {}

    def handler(request: httpx.Request) -> httpx.Response:
        captured["url"] = str(request.url)
        captured["body"] = json.loads(request.content)
        captured["auth"] = request.headers.get("authorization")
        body = (
            'data: {"choices":[{"delta":{"content":"你好"}}]}\n\n'
            'data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"t_1","function":{"name":"bash","arguments":"{\\"command\\":"}}]}}]}\n\n'
            'data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\\"pytest\\"}"}}]}}]}\n\n'
            'data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}\n\n'
            "data: [DONE]\n\n"
        ).encode("utf-8")
        return httpx.Response(200, headers={"content-type": "text/event-stream"}, content=body)

    provider = make_provider(handler)
    req = p.RunRequest(
        protocol="1.0", run_id="r_1", session_id="s_1", step=0, model="claude-sonnet-4-6",
        messages=[p.Message(role="user", content="跑测试")],
        tools=[p.Tool(name="bash", risk_class=1)],
    )
    chunks = [c async for c in provider.stream(req)]

    # 请求形状：经 LiteLLM 网关（OpenAI 兼容端点）
    assert captured["url"].startswith("http://stub/chat/completions")
    assert captured["body"]["model"] == "claude-sonnet-4-6"
    assert captured["body"]["stream"] is True
    assert captured["body"]["stream_options"]["include_usage"] is True
    assert captured["body"]["tools"][0]["function"]["name"] == "bash"
    assert captured["auth"] == "Bearer sk-test"

    # 流式解析：delta 逐块；tool_calls 按 index 累积拼接；流末 usage
    texts = [c.delta for c in chunks if c.delta]
    assert texts == ["你好"]
    calls = [c for c in chunks if c.tool_calls]
    assert calls[-1].tool_calls[0]["function"]["name"] == "bash"
    assert calls[-1].tool_calls[0]["function"]["arguments"] == '{"command":"pytest"}'
    usages = [c.usage for c in chunks if c.usage]
    assert usages[-1] == {"tokens_in": 10, "tokens_out": 5}


async def test_stream_chat_error_propagates():
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(401, json={"error": {"message": "invalid api key"}})

    provider = make_provider(handler)
    req = p.RunRequest(
        protocol="1.0", run_id="r_1", session_id="s_1", step=0, model="m",
        messages=[p.Message(role="user", content="hi")],
    )
    with pytest.raises(Exception):
        async for _ in provider.stream(req):
            pass


def test_openai_tools_conversion():
    assert openai_tools([]) is None
    tools = openai_tools([p.Tool(name="bash", tool_schema={"type": "object", "properties": {}}, risk_class=2)])
    assert tools[0]["type"] == "function"
    assert tools[0]["function"]["name"] == "bash"
    assert "risk_class=2" in tools[0]["function"]["description"]
    assert tools[0]["function"]["parameters"]["type"] == "object"


async def test_fake_provider_script_turns():
    """脚本模式：按 run 内 step 索引（确定性/重放安全/并发安全，W2/W3 e2e 的模型替身）。"""
    from app.llm import FakeProvider

    provider = FakeProvider(script=[
        {"tool_call": {"name": "write_file", "arguments": {"path": "/workspace/h.py", "content": "print(42)"}}},
        {"final": "完成"},
    ])
    # step=0 → 工具调用（交棒）；step=1 → 流式终答
    req0 = p.RunRequest(
        protocol="1.0", run_id="r_1", session_id="s_1", step=0, model="m",
        messages=[p.Message(role="user", content="x")],
    )
    chunks1 = [c async for c in provider.stream(req0)]
    assert chunks1[-1].tool_calls[0]["function"]["name"] == "write_file"
    assert chunks1[-1].tool_calls[0]["function"]["arguments"] == '{"path": "/workspace/h.py", "content": "print(42)"}'
    req1 = req0.model_copy(update={"step": 1})
    chunks2 = [c async for c in provider.stream(req1)]
    texts = "".join(c.delta for c in chunks2 if c.delta)
    assert texts == "完成"
    assert any(c.usage for c in chunks2)


async def test_fake_provider_inline_tool_advances_within_request():
    """同一 /runs 内多次 stream()（内联工具场景）：调用序推进脚本轮次；
    重发（同 run_id+step）从头开始（确定性重放）。"""
    from app.llm import FakeProvider

    provider = FakeProvider(script=[
        {"tool_call": {"name": "http_request", "arguments": {"url": "http://x"}}},
        {"final": "完成"},
    ])
    req0 = p.RunRequest(
        protocol="1.0", run_id="r_1", session_id="s_1", step=0, model="m",
        messages=[p.Message(role="user", content="x")],
    )
    # 第 1 次调用（同请求）：工具轮
    c1 = [c async for c in provider.stream(req0)]
    assert c1[-1].tool_calls[0]["function"]["name"] == "http_request"
    # 第 2 次调用（同请求，内联工具后继续）：终答轮
    c2 = [c async for c in provider.stream(req0)]
    assert "".join(c.delta for c in c2 if c.delta) == "完成"
    # 重发同 (run_id, step)：重新从头（工具轮）——确定性重放
    c3 = [c async for c in provider.stream(req0)]
    assert c3[-1].tool_calls[0]["function"]["name"] == "http_request"
    # 新 step：step 偏移进下一轮
    req1 = req0.model_copy(update={"step": 1})
    c4 = [c async for c in provider.stream(req1)]
    assert "".join(c.delta for c in c4 if c.delta) == "完成"
