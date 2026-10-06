"""_run 的工具片段累计合并（真实流式：首个片段参数为空）。"""

import asyncio
import json

from app import main as m
from app.llm import StreamChunk


class FragmentProvider:
    """模拟真实模型：同一 index 的 arguments 分三片段到达。"""

    def __init__(self):
        self.calls = 0

    async def stream(self, req):
        if self.calls > 0:
            return  # 第二轮直接终答（generator 空）——async generator 用 return 结束
        self.calls += 1
        yield StreamChunk(tool_calls=[{"index": 0, "id": "call_1", "type": "function", "function": {"name": "next_speaker", "arguments": ""}}])
        yield StreamChunk(tool_calls=[{"index": 0, "id": "", "type": "function", "function": {"name": "", "arguments": '{"participant": 0,'}}])
        yield StreamChunk(tool_calls=[{"index": 0, "id": "", "type": "function", "function": {"name": "", "arguments": '"instruction": "发言"}'}}])


async def test_fragment_accumulation():
    provider = FragmentProvider()
    frames = []
    async for f in m._run(m.p.RunRequest(protocol="1.0", run_id="r1", session_id="s", step=0, model="m",
                                         messages=[m.p.Message(role="user", content="x")],
                                         tools=[m.p.Tool(type="function", name="next_speaker")],
                                         max_turns=2), provider):
        frames.append(f)
    tool_call = next(f for f in frames if json.loads(f["data"])["type"] == "tool_call")
    payload = json.loads(tool_call["data"])["payload"]
    assert payload["name"] == "next_speaker"
    assert payload["arguments"] == {"participant": 0, "instruction": "发言"}, payload
