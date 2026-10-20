"""单段推理：全部工具交棒、原始参数、限额与流错误。"""

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
    call = next(f for f in frames if f.type == "tool_call")
    assert call.payload["name"] == "bash"
    assert call.payload["arguments"] == {"command": "pytest"}
    assert call.payload["id"] == "t_9"
    assert frames[-1].type == "done"
    assert provider.i == 1


async def test_control_tool_hands_off():
    """控制工具（request_approval）同样交棒——worker awakeable，harness 绝不内联。"""
    provider = ScriptedProvider([[
        StreamChunk(delta="需要审批。", tool_calls=[tc(0, "t_2", "request_approval", '{"question":"允许吗？"}')]),
    ]])
    frames = await collect(provider, base_request())
    call = next(f for f in frames if f.type == "tool_call")
    assert call.payload["name"] == "request_approval"
    assert call.payload["arguments"] == {"question": "允许吗？"}
    assert frames[-1].type == "done"


async def test_all_tools_handed_off_once_without_inline_execution(monkeypatch):
    async def forbidden_http(*args, **kwargs):
        raise AssertionError("harness must not execute API tools")

    monkeypatch.setattr("httpx.AsyncClient.get", forbidden_http)
    provider = ScriptedProvider([[
        StreamChunk(delta="查一下。", tool_calls=[
            tc(1, "t_2", "knowledge_search", '{"query":"规则"}'),
            tc(0, "t_1", "http_request", '{"url":"https://example.com"}'),
        ]),
        StreamChunk(usage={"tokens_in": 12, "tokens_out": 9}),
    ]])
    req = base_request(tools=[p.Tool(name="http_request"), p.Tool(name="knowledge_search")])
    frames = await collect(provider, req)
    calls = [f.payload for f in frames if f.type == "tool_call"]
    assert [c["id"] for c in calls] == ["t_1", "t_2"]
    assert [c["name"] for c in calls] == ["http_request", "knowledge_search"]
    assert calls[1]["raw_arguments"] == '{"query":"规则"}'
    assert provider.i == 1
    assert frames[-1].type == "done"
    assert frames[-1].payload["final"] == "查一下。"
    assert frames[-1].payload["usage"]["tokens_in"] == 12
    assert "turn_end" not in [f.type for f in frames]
    assert [f.seq for f in frames if f.type != "beat"] == list(range(1, len(frames)))


async def test_unsupported_protocol_defensive_error():
    frames = await collect(ScriptedProvider([]), base_request(protocol="9.9"))
    assert frames[-1].type == "error"
    assert frames[-1].payload["code"] == "protocol_unsupported"


@pytest.mark.parametrize("raw", ["{oops", "", "[]", "null", '{"x":NaN}'])
async def test_invalid_tool_arguments_retained_as_error(raw):
    provider = ScriptedProvider([[
        StreamChunk(tool_calls=[tc(0, "t_bad", "knowledge_search", raw)]),
    ]])
    frames = await collect(provider, base_request())
    call = next(f.payload for f in frames if f.type == "tool_call")
    assert call["id"] == "t_bad"
    assert call["arguments"] is None
    assert call["raw_arguments"] == raw
    assert call["arguments_error"]
    assert frames[-1].type == "done"
    assert provider.i == 1


async def test_missing_usage_preserves_known_result_and_marks_partial():
    frames = await collect(ScriptedProvider([[StreamChunk(delta="known result")]]), base_request())
    assert frames[-1].payload["final"] == "known result"
    assert frames[-1].payload["usage_partial"] is True


async def test_length_finish_keeps_usage_and_does_not_dispatch_incomplete_calls():
    frames = await collect(ScriptedProvider([[
        StreamChunk(tool_calls=[tc(0, "t_1", "bash", '{"command":')]),
        StreamChunk(finish_reason="length"),
        StreamChunk(usage={"tokens_in": 3, "tokens_out": 5}),
    ]]), base_request())
    assert not any(f.type == "tool_call" for f in frames)
    assert frames[-1].payload["truncated"] is True
    assert frames[-1].payload["usage"]["tokens_out"] == 5
    assert frames[-1].payload["usage"]["usage_partial"] is False


async def test_argument_bytes_are_bounded_and_stream_is_closed():
    class LongArguments(LLMProvider):
        closed = False

        async def stream(self, req):
            try:
                yield StreamChunk(tool_calls=[tc(0, "t_1", "bash", '{"command":"' + "x" * 1000)])
                raise AssertionError("stream must be closed at byte limit")
            finally:
                self.closed = True

    provider = LongArguments()
    frames = await collect(provider, base_request(max_output_bytes=16))
    assert provider.closed
    assert not any(f.type == "tool_call" for f in frames)
    assert frames[-1].payload["truncated"] is True
    assert frames[-1].payload["usage_partial"] is True


@pytest.mark.parametrize("first_delta", [False, True])
async def test_timeout_stops_stream_with_explicit_error(first_delta):
    import asyncio

    class SlowProvider(LLMProvider):
        closed = False

        async def stream(self, req):
            try:
                if first_delta:
                    yield StreamChunk(delta="before timeout")
                await asyncio.sleep(1)
                yield StreamChunk(delta="late")
            finally:
                self.closed = True

    provider = SlowProvider()
    frames = await collect(provider, base_request(timeout_ms=10))
    assert frames[-1].type == "error"
    assert "timed out after 10 ms" in frames[-1].payload["message"]
    assert provider.closed


async def test_stream_error_yields_error_frame(monkeypatch):
    """真实 e2e 缺陷回归：模型流异常 EOF → 僵尸 invocation 无限重试——
    _safe_stream 转 error 帧，worker 显式失败而非 unexpected EOF。"""
    from app import main as m
    from app.llm import LLMProvider
    from app import protocol as p

    class BoomProvider(LLMProvider):
        async def stream(self, req):
            raise RuntimeError("model 400 stream broken")
            yield  # pragma: no cover

    async def fake_run_stream(req):
        frames = []
        async for frame in m._run(req, BoomProvider()):
            frames.append(frame)
        return frames

    req = p.RunRequest(run_id="r_boom", session_id="s_1", step=0, model="m", messages=[])
    frames = await fake_run_stream(req)
    joined = "\n".join(f["data"] for f in frames)
    assert "model_stream_failed" in joined
    assert "model 400 stream broken" in joined
