"""审计 #6 反例固化：慢模型流经 beat 后不得被取消——最终 done 完整（final
非空 + usage 非 0）。wait_for 旧实现的 bug：超时 cancel 生成器 → 伪造成功。"""
import asyncio

import pytest

from app import main as m
from app import protocol as p
from app.llm import StreamChunk


class SlowChunks:
    """慢帧流：产出 delta 后挂起（超过 beat 间隔）再产出后续帧——旧 wait_for
    实现的 bug：超时 cancel 生成器 → StopAsyncIteration → 伪造空 done。"""

    def __init__(self):
        self.chunks = [StreamChunk(delta="慢"), StreamChunk(delta="速完成"),
                       StreamChunk(usage={"tokens_in": 7, "tokens_out": 9})]

    def __aiter__(self):
        return self

    async def __anext__(self):
        if not self.chunks:
            raise StopAsyncIteration
        c = self.chunks.pop(0)
        if c.delta == "速完成":
            await asyncio.sleep(2)  # 挂起超过 beat_interval——触发心跳路径
        return c


@pytest.mark.asyncio
async def test_heartbeat_keeps_stream_task():
    """beat 间隔 0.5s + 慢帧 2s：帧流必须在 beat 后继续产出（不得被取消）。"""
    frames = [f async for f in m._stream_frames(SlowChunks(), m.p, 0.5)]
    deltas = [getattr(f, "delta", None) for f in frames]
    beats = [f for f in frames if not hasattr(f, "delta")]
    assert "慢" in deltas and "速完成" in deltas, f"慢流被 beat 取消: {deltas} {beats}"
    last = frames[-1]
    assert last.usage and last.usage["tokens_in"] == 7


@pytest.mark.asyncio
async def test_run_consumer_tolerates_beat_dict():
    """审计 #1 反例固化：_run 与 _stream_frames 的连接——慢流经 beat 后
    完整产出（beat 帧是 SSE dict，_run 必须透传而非按 StreamChunk 解引用；
    旧接线 chunk.delta 曾 AttributeError）。"""
    from app.llm import LLMProvider

    class SlowProvider2(LLMProvider):
        def __init__(self):
            self.calls = 0

        async def stream(self, req):
            self.calls += 1
            yield StreamChunk(delta="慢")
            await asyncio.sleep(1.2)
            yield StreamChunk(delta="速完成")
            yield StreamChunk(usage={"tokens_in": 3, "tokens_out": 5})

    # 缩短 beat 间隔到 0.5s（契约 30s 的测试等价——不得改变取消语义）
    frames = []
    async for frame in m._run(p.RunRequest(
        protocol="1.0", run_id="r_conn", step=0, session_id="s_conn",
        messages=[], tools=[], model="m",
        agent_config={"model": "m", "instructions": "i", "tools": [], "version": 1},
    ), SlowProvider2(), beat_interval=0.5):
        frames.append(frame)
    datas = [f["data"] for f in frames if isinstance(f, dict)]
    done = [d for d in datas if '"type":"done"' in d]
    assert done, f"慢流经 beat 后应产出 done: {[d[:60] for d in datas]}"
    assert "速完成" in done[0], "最终回答不得被 beat 帧截断"
