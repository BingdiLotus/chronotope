"""agent 循环单测：脚本化 LLMProvider 注入，覆盖 done/交棒/内联工具/截断/max_turns。"""

from __future__ import annotations

import json
from collections.abc import AsyncIterator

import pytest

from app import protocol as p
from app.llm import LLMProvider, StreamChunk
from app.main import _run


class ScriptedProvider(LLMProvider):
    """按轮脚本产出（测试替身）：第 n 次 stream() 消费第 n 轮的块序列。"""

    def __init__(self, turns: list[list[StreamChunk]]) -> None:
        self.turns = turns
        self.i = 0

    async def stream(self, req: p.RunRequest) -> AsyncIterator[StreamChunk]:
        assert self.i < len(self.turns), f"模型调用超出脚本（第 {self.i + 1} 轮）"
        turn = self.turns[self.i]
        self.i += 1
        for chunk in turn:
            yield chunk


def base_request(**kwargs) -> p.RunRequest:
    defaults = dict(
        protocol="1.0", run_id="r_1", session_id="s_1", step=0,
        model="claude-sonnet-4-6",
        messages=[p.Message(role="user", content="你好")],
        tools=[p.Tool(name="bash", risk_class=1)],
    )
    defaults.update(kwargs)
    return p.RunRequest(**defaults)


async def collect(provider: LLMProvider, req: p.RunRequest) -> list[p.Frame]:
    out = []
    async for sse in _run(req, provider):
        out.append(p.Frame.model_validate(json.loads(sse["data"])))
    return out


def tc(index: int, id_: str, name: str, arguments: str) -> dict:
    return {"index": index, "id": id_, "type": "function",
            "function": {"name": name, "arguments": arguments}}


async def test_done_path_streams_deltas_in_order():
    provider = ScriptedProvider([[
        StreamChunk(delta="你好"),
        StreamChunk(delta="，我是助手"),
        StreamChunk(usage={"tokens_in": 10, "tokens_out": 5}),
    ]])
    frames = await collect(provider, base_request())
    types = [f.type for f in frames]
    assert types[0] == "beat"
    assert types[1] == "delta" and frames[1].payload["text"] == "你好"
    assert types[2] == "delta" and frames[2].payload["text"] == "，我是助手"
    assert frames[-1].type == "done", "done 是唯一合法终态"
    assert frames[-1].payload["final"] == "你好，我是助手"
    assert frames[-1].payload["usage"]["tokens_in"] == 10


async def test_max_output_bytes_truncates():
    provider = ScriptedProvider([[
        StreamChunk(delta="一二三四五六七八九十"),  # 30 字节
        StreamChunk(usage={"tokens_in": 1, "tokens_out": 1}),
    ]])
    req = base_request(max_output_bytes=12)
    frames = await collect(provider, req)
    assert frames[-1].type == "done"
    assert frames[-1].payload["truncated"] is True, "截断是 journaled 事实"
    total = sum(len(f.payload["text"].encode()) for f in frames if f.type == "delta")
    assert total <= 12


async def test_code_tool_hands_off():
    provider = ScriptedProvider([[
        StreamChunk(delta="我来跑测试。", tool_calls=[tc(0, "t_9", "bash", '{"command":"pytest"}')]),
    ]])
    frames = await collect(provider, base_request())
    assert frames[-1].type == "tool_call", "代码工具必须交棒（不内联执行）"
    assert frames[-1].payload["name"] == "bash"
    assert frames[-1].payload["arguments"] == {"command": "pytest"}, "arguments 应为 JSON 对象（契约样例）"
    assert frames[-1].payload["id"] == "t_9"


async def test_control_tool_hands_off():
    """控制工具（request_approval）同样交棒——worker awakeable，harness 绝不内联。"""
    provider = ScriptedProvider([[
        StreamChunk(delta="需要审批。", tool_calls=[tc(0, "t_2", "request_approval", '{"question":"允许吗？"}')]),
    ]])
    frames = await collect(provider, base_request())
    assert frames[-1].type == "tool_call"
    assert frames[-1].payload["name"] == "request_approval"
    assert frames[-1].payload["arguments"] == {"question": "允许吗？"}


async def test_api_tool_inline_then_done(monkeypatch):
    """API 工具内联执行 → 结果回喂 → 下一轮终答（两轮流式脚本）。"""
    async def fake_http_request(name, arguments):
        return json.dumps({"status": 200, "body": "ok"})

    monkeypatch.setattr("app.main.run_api_tool", fake_http_request)

    script = [
        # 第一轮：工具调用（http_request 属 API 工具，内联）
        [StreamChunk(delta="查一下。", tool_calls=[tc(0, "t_1", "http_request", '{"url":"https://example.com"}')])],
        # 第二轮：模型拿到工具结果后终答
        [StreamChunk(delta="查询成功。"), StreamChunk(usage={"tokens_in": 3, "tokens_out": 2})],
    ]
    frames = await collect(ScriptedProvider(script), base_request())
    types = [f.type for f in frames]
    assert "turn_end" in types, "内联工具完成后应发 turn_end"
    assert frames[-1].type == "done"
    assert frames[-1].payload["final"] == "查询成功。"
    assert not any(f.type == "tool_call" for f in frames), "API 工具不应交棒"


async def test_max_turns_errors(monkeypatch):
    """永远产出 API 工具调用（内联不终结）→ max_turns 封顶 → error 终态。"""
    async def fake_http_request(name, arguments):
        return json.dumps({"ok": True})

    monkeypatch.setattr("app.main.run_api_tool", fake_http_request)
    script = [
        [StreamChunk(tool_calls=[tc(0, f"t_{i}", "http_request", '{"url":"https://example.com"}')])]
        for i in range(8)
    ]
    frames = await collect(ScriptedProvider(script), base_request(max_turns=2))
    assert frames[-1].type == "error"
    assert frames[-1].payload["code"] == "max_turns"


async def test_unsupported_protocol_defensive_error():
    frames = await collect(ScriptedProvider([]), base_request(protocol="9.9"))
    assert frames[-1].type == "error"
    assert frames[-1].payload["code"] == "protocol_unsupported"


async def test_invalid_tool_arguments_json_fallback():
    """arguments 非法 JSON → 内联工具收到 {}，不崩溃。"""
    async def fake_http_request(name, arguments):
        return json.dumps({"got": arguments})

    monkeypatch = pytest.MonkeyPatch()
    monkeypatch.setattr("app.main.run_api_tool", fake_http_request)

    script = [
        [StreamChunk(delta="x", tool_calls=[tc(0, "t_1", "http_request", "{oops")])],
        [StreamChunk(delta="done"), StreamChunk(usage={"tokens_in": 1, "tokens_out": 1})],
    ]
    frames = await collect(ScriptedProvider(script), base_request())
    assert frames[-1].type == "done"
    monkeypatch.undo()
