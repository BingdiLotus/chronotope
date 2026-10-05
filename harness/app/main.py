"""chronotope-harness：无状态单段执行器（POST /runs → SSE）。

纪律（mvp-落地方案 §9）：
- harness 不拥有 loop 所有权之持久面——每次 /runs 调用跑「一段」agent 循环
  （模型调用 + API 工具内联），遇到代码类工具即停下交棒（tool_call 帧）；
- 无 Session、无本地存储（OpenAI Agents SDK 的 Session 特性故意不用）；
- 模型调用永远走 LiteLLM 网关（OpenAI 协议）；密钥只在 LiteLLM 侧；
- 三种返回：done（终答）/ tool_call（交棒）/ error（max_turns）。
"""

from __future__ import annotations

import json
import logging
from collections.abc import AsyncIterator

from fastapi import FastAPI
from sse_starlette.sse import EventSourceResponse

from . import protocol as p
from .llm import LLMProvider, build_provider
from .tools import run_api_tool

logger = logging.getLogger("chronotope-harness")

app = FastAPI(title="chronotope-harness", version="0.1.0")

_provider: LLMProvider | None = None


def get_provider() -> LLMProvider:
    """惰性构造 provider（import 不依赖环境；蓝绿按 run 协议版本路由——契约规范 §3）。"""
    global _provider
    if _provider is None:
        _provider = build_provider()
    return _provider


@app.get("/healthz")
async def healthz() -> dict[str, str]:
    return {"status": "ok", "service": "chronotope-harness"}


@app.post("/runs")
async def runs(req: p.RunRequest) -> EventSourceResponse:
    """POST /runs → SSE 事件流（契约规范 §3）。"""
    try:
        provider = get_provider()
    except RuntimeError as exc:  # 配置错误：明确报错而非静默降级
        return EventSourceResponse(_misconfigured(str(exc)), ping=15)
    return EventSourceResponse(_run(req, provider), ping=15)


async def _misconfigured(message: str) -> AsyncIterator[dict[str, str]]:
    yield _sse(p.error("harness_misconfigured", message))


async def _run(req: p.RunRequest, llm: LLMProvider) -> AsyncIterator[dict[str, str]]:
    """执行一段 agent 循环：模型流式调用 → delta 帧 → 工具分流 → done/交棒/error。"""

    if req.protocol not in p.SUPPORTED_PROTOCOLS:
        # 错误码稳定（契约规范 §6）；worker 按协议版本路由本不会送错版本，此分支是防御
        yield _sse(p.error("protocol_unsupported", f"protocol {req.protocol} not in {p.SUPPORTED_PROTOCOLS}"))
        return

    # 心跳帧：长时内联 API 工具期间每 ≥30s 必发（契约规范 §3 beat 语义）。
    # 内联工具均有超时（tools._http_request 默认 10s），本轮循环内兜底 beat 一次。
    yield _sse(p.frame("beat", 0, {}))

    messages = [m.model_dump(exclude_none=True) for m in req.messages]
    seq = 1
    total_bytes = 0
    truncated = False

    for turn in range(req.max_turns):
        deltas: list[str] = []
        tool_calls: dict[int, dict] = {}
        usage = p.LLMUsage()

        async for chunk in llm.stream(req):
            if chunk.delta:
                total_bytes += len(chunk.delta.encode("utf-8"))
                if total_bytes > req.max_output_bytes:
                    # 输出限额：截断 + done{truncated:true}——截断是 journaled 事实（契约规范 §3）
                    truncated = True
                    break
                deltas.append(chunk.delta)
                yield _sse(p.delta(chunk.delta, seq))
                seq += 1
            for tc in chunk.tool_calls:
                tool_calls.setdefault(tc["index"], tc)
            if chunk.usage:
                usage.tokens_in += chunk.usage.get("tokens_in", 0)
                usage.tokens_out += chunk.usage.get("tokens_out", 0)

        if truncated:
            yield _sse(p.done("".join(deltas), usage, truncated=True, seq=seq))
            return

        if tool_calls:
            # 本轮产生了工具调用：assistant 消息（含 tool_calls）入历史
            ordered = [tool_calls[i] for i in sorted(tool_calls)]
            messages.append(
                {
                    "role": "assistant",
                    "content": "".join(deltas) or None,
                    "tool_calls": ordered,
                }
            )
            for tc in ordered:
                name = tc["function"]["name"]
                if name in p.CODE_TOOLS or name in p.CONTROL_TOOLS:
                    # 代码类工具 → worker 转 executor；控制类工具 → worker awakeable/子 Agent。
                    # 两者皆交棒（tool_call 帧），本段循环结束。
                    # 契约样例（runs-protocol.md）：arguments 为 JSON 对象（OpenAI 字符串形态在此转换）
                    try:
                        arguments = json.loads(tc["function"]["arguments"] or "{}")
                    except json.JSONDecodeError:
                        arguments = {}
                    yield _sse(
                        p.frame("tool_call", seq, {
                            "id": tc["id"], "name": name, "arguments": arguments,
                        })
                    )
                    return
                # API 类工具：内联执行，结果作为 tool 消息回喂，继续循环
                try:
                    arguments = json.loads(tc["function"]["arguments"] or "{}")
                except json.JSONDecodeError:
                    arguments = {}
                result = await run_api_tool(name, arguments)
                messages.append({"role": "tool", "tool_call_id": tc["id"], "content": result})
                yield _sse(p.frame("turn_end", seq, {"summary": f"{name} done"}))
                seq += 1
            continue  # 下一轮模型调用（携带工具结果）

        # 无工具调用：终答
        yield _sse(p.done("".join(deltas), usage, seq=seq))
        return

    # max_turns 封顶（无进展检测/熔断在 W5 完整落地，边界语义 §1）
    yield _sse(p.error("max_turns", f"超过 {req.max_turns} 轮未产出终答"))
    return


def _sse(f: p.Frame) -> dict[str, str]:
    return {"data": f.model_dump_json()}
