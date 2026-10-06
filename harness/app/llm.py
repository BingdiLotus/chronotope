"""模型提供层（LLM provider 抽象，mvp-落地方案 §9.2）。

- OpenAIProvider：经 LiteLLM 网关（OpenAI 兼容端点）调模型——「借」openai SDK 的
  流式与工具调用，模型多翻译由网关承担；密钥只在 LiteLLM 侧，harness 仅持网关 key。
- FakeProvider：e2e/演示用——无模型密钥时流式回放固定回复（多段 delta），
  让平台全链路（含 delta 帧 → 事件）在任何环境可演示。
"""

from __future__ import annotations

import asyncio
import json
import os
from abc import ABC, abstractmethod
from collections.abc import AsyncIterator
from dataclasses import dataclass, field

from openai import AsyncOpenAI

from .protocol import RunRequest, Tool


@dataclass
class StreamChunk:
    """流式块：文本增量 / 工具调用（按 index 累积）/ 流末 usage。"""

    delta: str | None = None
    tool_calls: list[dict] = field(default_factory=list)  # 原始 OpenAI 格式片段
    usage: dict | None = None  # tokens_in/tokens_out


class LLMProvider(ABC):
    """模型流接口：逐块产出 StreamChunk。"""

    @abstractmethod
    def stream(self, req: RunRequest) -> AsyncIterator[StreamChunk]:
        raise NotImplementedError


def openai_tools(tools: list[Tool]) -> list[dict] | None:
    """把 /runs 协议工具清单转换为 OpenAI function 格式；空清单返回 None。"""
    if not tools:
        return None
    return [
        {
            "type": "function",
            "function": {
                "name": t.name,
                "description": f"risk_class={t.risk_class}",  # 风险分级提示（边界语义 §2）
                "parameters": t.tool_schema or {"type": "object", "properties": {}},
            },
        }
        for t in tools
    ]


class OpenAIProvider(LLMProvider):
    """经 LiteLLM 网关（OpenAI 兼容端点）的流式调用。"""

    def __init__(self, base_url: str, api_key: str) -> None:
        self.client = AsyncOpenAI(base_url=base_url.rstrip("/"), api_key=api_key)

    async def stream(self, req: RunRequest) -> AsyncIterator[StreamChunk]:
        messages = [
            {
                "role": m.role,
                "content": m.content,
                **({"tool_calls": m.tool_calls} if m.tool_calls else {}),
                **({"tool_call_id": m.tool_call_id} if m.tool_call_id else {}),
            }
            for m in req.messages
        ]
        # stream_options.include_usage：流末返回 usage（截断/断流时 usage_partial 兜底，契约规范 §3）
        response = await self.client.chat.completions.create(
            model=req.model,
            messages=messages,
            tools=openai_tools(req.tools),
            stream=True,
            stream_options={"include_usage": True},
        )
        tool_call_fragments: dict[int, dict] = {}
        async for chunk in response:
            # 流末 usage 块（stream_options.include_usage）可能无 choices，先取 usage
            usage = None
            if chunk.usage is not None:
                usage = {
                    "tokens_in": chunk.usage.prompt_tokens or 0,
                    "tokens_out": chunk.usage.completion_tokens or 0,
                }
            if not chunk.choices:
                if usage:
                    yield StreamChunk(usage=usage)
                continue
            choice = chunk.choices[0]
            delta = choice.delta
            if delta is None:
                continue
            text = delta.content or ""
            calls = []
            if delta.tool_calls:
                for tc in delta.tool_calls:
                    idx = tc.index
                    tool_call_fragments.setdefault(
                        idx, {"index": idx, "id": "", "type": "function", "function": {"name": "", "arguments": ""}}
                    )
                    # 契约（StreamChunk 文档）：yield 原始 delta 片段（非累计），
                    # 由 harness 侧按 index 累计合并。曾误 yield 累计副本 → harness
                    # 再合并 → 工具名双写（"next_speakernext_speaker"，真实模型 e2e 实证）
                    calls.append({
                        "index": idx,
                        "id": tc.id or "",
                        "type": "function",
                        "function": {
                            "name": (tc.function.name if tc.function else "") or "",
                            "arguments": (tc.function.arguments if tc.function else "") or "",
                        },
                    })
            if text or calls or usage:
                yield StreamChunk(delta=text or None, tool_calls=calls, usage=usage)


class FakeProvider(LLMProvider):
    """e2e/演示实现：流式回放固定回复（模拟 delta 帧，可配延迟）。

    脚本模式（script 非空）：按轮产出工具调用/终答——
    第 n 次 stream() 消费第 n 轮；tool_call 轮发交棒帧，final 轮流式终答。
    """

    def __init__(
        self,
        reply: str = "你好，我是 Chronotope 演示助手。",
        chunk_ms: int = 20,
        script: list[dict] | None = None,
        alt_script: list[dict] | None = None,
    ) -> None:
        self.reply = reply
        self.chunk_ms = chunk_ms
        self.script = script
        self.alt_script = alt_script
        self._last_key: tuple | None = None
        self._calls = 0
        self._seen_runs: set[str] = set()   # 每个 run_id 只计一次（session 分组见 _run_counts）
        self._run_counts: dict[str, int] = {}  # session_id → run 序数
        self._active_scripts: dict[str, list[dict] | None] = {}
        self._active_script = script

    async def stream(self, req: RunRequest) -> AsyncIterator[StreamChunk]:
        # 记忆消化摘要模式：run_id 带 #consolidation 后缀（worker 分层记忆调用；
        # 确定性文本——重放安全，真实模式不受影响）
        if req.model == "chronotope-participant-a":
            final = "成员发言（架构师）：我建议采用方案 A，理由是简单可控。"
            for i in range(0, len(final), 3):
                await asyncio.sleep(self.chunk_ms / 1000)
                yield StreamChunk(delta=final[i : i + 3])
            yield StreamChunk(usage={"tokens_in": 4, "tokens_out": 6})
            return
        if req.model == "chronotope-participant-b":
            final = "成员发言（评审）：方案 A 可行，但成本偏高，建议限定范围。"
            for i in range(0, len(final), 3):
                await asyncio.sleep(self.chunk_ms / 1000)
                yield StreamChunk(delta=final[i : i + 3])
            yield StreamChunk(usage={"tokens_in": 4, "tokens_out": 6})
            return
        if req.model == "chronotope-subagent":
            # 子 Agent 演示模式（e2e 用）：确定性终答，避免脚本递归
            final = "子任务完成：答案是 42。"
            for i in range(0, len(final), 3):
                await asyncio.sleep(self.chunk_ms / 1000)
                yield StreamChunk(delta=final[i : i + 3])
            yield StreamChunk(usage={"tokens_in": 4, "tokens_out": 4})
            return
        if req.run_id.endswith("#consolidation"):
            final = "摘要：本轮对话已消化——记录用户目标、关键事实与未完成事项。"
            for i in range(0, len(final), 3):
                await asyncio.sleep(self.chunk_ms / 1000)
                yield StreamChunk(delta=final[i : i + 3])
            yield StreamChunk(usage={"tokens_in": 4, "tokens_out": 8})
            return
        if self.script is not None:
            # 脚本选择：进程内首个 run 用主脚本，后续 run 用 ALT（若设置）——
            # e2e 多 run 场景（快照恢复：run1 写、run2 读）无需重启 harness
            if self.alt_script is not None:
                # 按 session 分组的 run 序数：该 session 首个 run 用主脚本，
                # 之后用 ALT——跨 session 独立（e2e 重跑不被进程内状态污染）
                if req.run_id not in self._seen_runs:
                    self._seen_runs.add(req.run_id)
                    self._run_counts[req.session_id] = self._run_counts.get(req.session_id, 0) + 1
                    is_first = self._run_counts[req.session_id] == 1
                    self._active_scripts[req.session_id] = self.script if is_first else self.alt_script
                self._active_script = self._active_scripts.get(req.session_id, self.script)
            # 脚本索引 = run 内 step + 本次 /runs 内的调用序：
            # - 代码/控制工具交棒 → 新 /runs（step+1）→ 下一轮；
            # - 内联 API 工具 → 同一 /runs 内再次 stream() → 调用序推进到下一轮；
            # - 重发（同 run_id+step）→ 从头开始，确定性重放。
            key = (req.run_id, req.step)
            if key != self._last_key:
                self._last_key = key
                self._calls = 0
            else:
                self._calls += 1
            turn = self._active_script[(req.step + self._calls) % len(self._active_script)]
            if "tool_call" in turn:
                tc = turn["tool_call"]
                yield StreamChunk(tool_calls=[{
                    "index": 0,
                    "id": tc.get("id", "t_fake"),
                    "type": "function",
                    "function": {
                        "name": tc["name"],
                        "arguments": json.dumps(tc.get("arguments", {})),
                    },
                }])
                return
            final = turn.get("final", self.reply)
            for i in range(0, len(final), 3):  # 每 3 字符一个 delta
                await asyncio.sleep(self.chunk_ms / 1000)
                yield StreamChunk(delta=final[i : i + 3])
            yield StreamChunk(usage={"tokens_in": 4, "tokens_out": len(final) // 3})
            return
        for i in range(0, len(self.reply), 3):
            await asyncio.sleep(self.chunk_ms / 1000)
            yield StreamChunk(delta=self.reply[i : i + 3])
        yield StreamChunk(usage={"tokens_in": 4, "tokens_out": len(self.reply) // 3})


def build_provider() -> LLMProvider:
    """按环境构造 provider：HARNESS_FAKE_MODEL=1 → Fake（e2e/演示），否则 LiteLLM 网关。"""
    if os.environ.get("HARNESS_FAKE_MODEL") == "1":
        script = None
        if raw := os.environ.get("HARNESS_FAKE_SCRIPT"):
            script = json.loads(raw)  # [{"tool_call":{...}} | {"final":"..."}, ...]
        alt_script = None
        if raw := os.environ.get("HARNESS_FAKE_SCRIPT_ALT"):
            alt_script = json.loads(raw)  # 进程内第二个 run 起使用（多 run e2e）
        # 空串视为未设置（compose 默认透传 "" 不得覆盖默认回复——demo 实证）
        return FakeProvider(
            reply=os.environ.get("HARNESS_FAKE_REPLY") or "你好，我是 Chronotope 演示助手。",
            script=script,
            alt_script=alt_script,
        )
    api_key = os.environ.get("LITELLM_API_KEY", "")
    if not api_key:
        raise RuntimeError(
            "LITELLM_API_KEY 未设置（harness 经 LiteLLM 网关调模型）；"
            "本地 e2e/演示可设 HARNESS_FAKE_MODEL=1 走假模型流"
        )
    return OpenAIProvider(
        base_url=os.environ.get("LITELLM_BASE_URL", "http://localhost:4000"),
        api_key=api_key,
    )
