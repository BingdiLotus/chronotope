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
        captured["timeout"] = request.extensions.get("timeout")
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
        tools=[p.Tool(name="bash", description="Execute a command", risk_class=1)],
        max_output_tokens=128, timeout_ms=1500,
    )
    chunks = [c async for c in provider.stream(req)]

    # 请求形状：经 LiteLLM 网关（OpenAI 兼容端点）
    assert captured["url"].startswith("http://stub/chat/completions")
    assert captured["body"]["model"] == "claude-sonnet-4-6"
    assert captured["body"]["stream"] is True
    assert captured["body"]["stream_options"]["include_usage"] is True
    assert captured["body"]["tools"][0]["function"]["name"] == "bash"
    assert captured["body"]["tools"][0]["function"]["description"] == "Execute a command"
    assert captured["body"]["max_completion_tokens"] == 128
    assert captured["timeout"]["read"] == 1.5
    assert captured["auth"] == "Bearer sk-test"

    # 流式解析：delta 逐块；tool_calls 为**原始 delta 片段**（契约：harness 侧按
    # index 累计合并——provider 不预合并，否则 harness 再合并会双写工具名）
    texts = [c.delta for c in chunks if c.delta]
    assert texts == ["你好"]
    calls = [c for c in chunks if c.tool_calls]
    assert calls[0].tool_calls[0]["function"]["name"] == "bash"
    assert calls[0].tool_calls[0]["function"]["arguments"] == '{"command":'
    assert calls[1].tool_calls[0]["function"]["name"] == ""  # 续片段无名称
    assert calls[1].tool_calls[0]["function"]["arguments"] == '"pytest"}'
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
    schema = {"type": "object", "properties": {"query": {"type": "string"}}, "required": ["query"]}
    tools = openai_tools([p.Tool(name="knowledge_search", description="Find permitted knowledge", tool_schema=schema)])
    assert tools[0]["type"] == "function"
    assert tools[0]["function"]["name"] == "knowledge_search"
    assert tools[0]["function"]["description"] == "Find permitted knowledge"
    assert tools[0]["function"]["parameters"] == schema


async def test_full_assistant_calls_and_success_error_call_ids_are_forwarded():
    captured = {}

    def handler(request):
        captured.update(json.loads(request.content))
        return httpx.Response(200, headers={"content-type": "text/event-stream"}, content=(
            'data: {"choices":[{"delta":{"content":"nonce accepted"}}]}\n\n'
            'data: [DONE]\n\n'
        ))

    calls = [
        {"id": "call_ok", "type": "function", "function": {"name": "knowledge_search", "arguments": '{"query":"x"}'}},
        {"id": "call_bad", "type": "function", "function": {"name": "knowledge_search", "arguments": "{bad"}},
    ]
    req = p.RunRequest(run_id="r", session_id="s", step=1, model="m", messages=[
        p.Message(role="assistant", content="checking", tool_calls=calls),
        p.Message(role="tool", content='{"nonce":"never in initial prompt"}', tool_call_id="call_ok"),
        p.Message(role="tool", content='{"error":"invalid arguments"}', tool_call_id="call_bad"),
    ])
    provider = make_provider(handler)
    chunks = [c async for c in provider.stream(req)]
    messages = captured["messages"]
    assert messages[0]["tool_calls"] == calls
    assert messages[1]["tool_call_id"] == "call_ok"
    assert "never in initial prompt" in messages[1]["content"]
    assert messages[2]["tool_call_id"] == "call_bad"
    assert "invalid arguments" in messages[2]["content"]
    assert chunks[0].delta == "nonce accepted"


async def test_sdk_does_not_retry_model_request():
    calls = 0

    def handler(request):
        nonlocal calls
        calls += 1
        return httpx.Response(500, json={"error": {"message": "unavailable"}})

    provider = make_provider(handler)
    req = p.RunRequest(run_id="r", session_id="s", step=0, model="m", messages=[])
    with pytest.raises(Exception):
        async for _ in provider.stream(req):
            pass
    assert calls == 1


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


async def test_fake_provider_same_step_replays_without_advancing():
    """同 run/step 重发保持同一段；只有 worker 推进 step 才到下一轮。"""
    from app.llm import FakeProvider

    provider = FakeProvider(script=[
        {"tool_call": {"name": "http_request", "arguments": {"url": "http://x"}}},
        {"final": "完成"},
    ])
    req0 = p.RunRequest(
        protocol="1.0", run_id="r_1", session_id="s_1", step=0, model="m",
        messages=[p.Message(role="user", content="x")],
    )
    # 重放同一步三次都必须返回同一个工具轮。
    c1 = [c async for c in provider.stream(req0)]
    assert c1[-1].tool_calls[0]["function"]["name"] == "http_request"
    c2 = [c async for c in provider.stream(req0)]
    assert c2 == c1
    c3 = [c async for c in provider.stream(req0)]
    assert c3 == c1
    # 新 step：step 偏移进下一轮
    req1 = req0.model_copy(update={"step": 1})
    c4 = [c async for c in provider.stream(req1)]
    assert "".join(c.delta for c in c4 if c.delta) == "完成"


async def test_fake_provider_multiple_tool_calls_keep_ids_and_bad_json():
    from app.llm import FakeProvider

    provider = FakeProvider(script=[{"tool_calls": [
        {"id": "a", "name": "knowledge_search", "arguments": {"query": "q"}},
        {"id": "b", "name": "knowledge_remember", "raw_arguments": "{invalid"},
    ]}])
    req = p.RunRequest(run_id="r", session_id="s", step=0, model="m", messages=[])
    chunks = [c async for c in provider.stream(req)]
    assert [tc["id"] for tc in chunks[0].tool_calls] == ["a", "b"]
    assert chunks[0].tool_calls[1]["function"]["arguments"] == "{invalid"
    assert chunks[0].usage is not None


def test_build_provider_empty_reply_env_falls_back():
    """HARNESS_FAKE_REPLY 设空串（compose 默认透传）不得覆盖默认回复（demo 实证）。"""
    import os

    from app.llm import FakeProvider, build_provider

    os.environ["HARNESS_FAKE_MODEL"] = "1"
    os.environ["HARNESS_FAKE_REPLY"] = ""
    os.environ.pop("HARNESS_FAKE_SCRIPT", None)
    try:
        provider = build_provider()
        assert isinstance(provider, FakeProvider)
        assert provider.reply == "你好，我是 Chronotope 演示助手。"
    finally:
        os.environ.pop("HARNESS_FAKE_MODEL", None)
        os.environ.pop("HARNESS_FAKE_REPLY", None)


async def test_fake_provider_consolidation_summary():
    """run_id 带 #consolidation 后缀 → 确定性摘要（worker 记忆消化路径）。"""
    from app.llm import FakeProvider

    provider = FakeProvider()
    req = p.RunRequest(
        protocol="1.0", run_id="r_1#consolidation", session_id="s_1", step=0, model="m",
        messages=[p.Message(role="user", content="转录全文")],
    )
    chunks = [c async for c in provider.stream(req)]
    final = "".join(c.delta for c in chunks if c.delta)
    assert final == "摘要：本轮对话已消化——记录用户目标、关键事实与未完成事项。"
    usage = [c.usage for c in chunks if c.usage]
    assert usage and usage[-1]["tokens_out"] == 8


async def test_fake_provider_subagent_model():
    """model=chronotope-subagent → 确定性子任务终答（子 Agent e2e 防递归）。"""
    from app.llm import FakeProvider

    provider = FakeProvider()
    req = p.RunRequest(
        protocol="1.0", run_id="r_child", session_id="s_child", step=0, model="chronotope-subagent",
        messages=[p.Message(role="user", content="计算 2+2")],
    )
    chunks = [c async for c in provider.stream(req)]
    assert "".join(c.delta for c in chunks if c.delta) == "子任务完成：答案是 42。"
