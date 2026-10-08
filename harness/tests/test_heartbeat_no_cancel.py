"""审计 #6 反例固化：慢模型流经 beat 后不得被取消——最终 done 完整（final
非空 + usage 非 0）。wait_for 旧实现的 bug：超时 cancel 生成器 → 伪造成功。"""
import asyncio

import pytest

from app import main as m
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
