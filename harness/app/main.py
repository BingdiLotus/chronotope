"""chronotope-harness：无状态单段执行器（POST /runs → SSE）。

纪律（mvp-落地方案 §9）：
- harness 不拥有 loop 所有权之持久面——每次 /runs 调用跑「一段」agent 循环
  （模型调用 + API 工具内联），遇到代码类工具即停下交棒（tool_call 帧）；
- 无 Session、无本地存储（OpenAI Agents SDK 的 Session 特性故意不用）；
- 模型调用永远走 LiteLLM 网关（OpenAI 协议）；密钥只在 LiteLLM 侧；
- 三种返回：done（终答）/ tool_call（交棒）/ error（max_turns）。
"""

from __future__ import annotations

import asyncio
import json
import logging
from collections.abc import AsyncIterator

from fastapi import FastAPI
from sse_starlette.sse import EventSourceResponse

from . import protocol as p
from .llm import LLMProvider, build_provider
from .tools import run_api_tool

# 应用日志直通根 logger（uvicorn 默认不配置根 logger，否则 INFO 记录被丢弃）
logging.basicConfig(level=logging.INFO)
logger = logging.getLogger("chronotope-harness")

app = FastAPI(title="chronotope-harness", version="0.1.0")

_provider: LLMProvider | None = None


@app.post("/embed")
async def embed_endpoint(body: dict):
    """文本嵌入（期 3 §D：共享知识库检索的向量来源；真实模式 LiteLLM）。"""
    text = body.get("text", "")
    if not text:
        return {"embedding": []}
    from .llm import embed

    return {"embedding": embed(text)}


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
    # 请求开始日志（chaos 套件的重调/重发计数依据；日志非状态，不破坏无状态纪律）
    logger.info("runs start run_id=%s step=%s model=%s", req.run_id, req.step, req.model)
    # 消息载荷日志（期 3 §D e2e 断言：共享知识注入可见；仅打 system 消息文本）
    systems = [m.content for m in req.messages if m.role == "system"]
    logger.info("runs systems run_id=%s systems=%s", req.run_id, json.dumps(systems, ensure_ascii=False))
    try:
        provider = get_provider()
    except RuntimeError as exc:  # 配置错误：明确报错而非静默降级
        return EventSourceResponse(_misconfigured(str(exc)), ping=15)
    return EventSourceResponse(_run(req, provider), ping=15)


async def _misconfigured(message: str) -> AsyncIterator[dict[str, str]]:
    yield _sse(p.error("harness_misconfigured", message))


async def _stream_frames(stream_iter, p, beat_interval: float):
    """流帧循环（beat 保任务——wait_for 取消语义修复，审计 #6）：
    产出 delta/tool_call/beat 帧；错误帧产出后由调用方处理。"""
    next_task = None
    while True:
        if next_task is None:
            next_task = asyncio.create_task(anext(stream_iter))
        heartbeat = asyncio.create_task(asyncio.sleep(beat_interval))
        done, _ = await asyncio.wait({next_task, heartbeat}, return_when=asyncio.FIRST_COMPLETED)
        if next_task in done:
            heartbeat.cancel()
            try:
                chunk = next_task.result()
            except StopAsyncIteration:
                break
            next_task = None
        else:
            heartbeat.cancel()  # sleep 已到点——anext 任务保持（模型挂起恢复后继续）
            yield _sse(p.frame("beat", 0, {}))
            continue
        yield chunk


async def _safe_stream(req: p.RunRequest, llm: LLMProvider):
    """流异常捕获（真实 e2e 实证：模型 400 时流中断 EOF 而非 error 帧——
    worker 收到 unexpected EOF → restate 僵尸 invocation 无限重试；此处转
    error 帧让 worker 显式失败）。"""
    try:
        async for chunk in llm.stream(req):
            yield chunk
    except Exception as exc:  # noqa: BLE001——流异常统一转 error 帧
        logger.exception("stream error run_id=%s", req.run_id)
        yield llm.stream_error_chunk(f"model stream failed: {exc}")


async def _run(req: p.RunRequest, llm: LLMProvider, beat_interval: float = 30.0) -> AsyncIterator[dict[str, str]]:
    """执行一段 agent 循环：模型流式调用 → delta 帧 → 工具分流 → done/交棒/error。"""

    if req.protocol not in p.SUPPORTED_PROTOCOLS:
        # 错误码稳定（契约规范 §6）；worker 按协议版本路由本不会送错版本，此分支是防御
        yield _sse(p.error("protocol_unsupported", f"protocol {req.protocol} not in {p.SUPPORTED_PROTOCOLS}"))
        return

    # 心跳帧：开场一帧 + 流式期间每 30s 周期补发（契约：每 ≥30s 一发——
    # 模型挂起 196s 实证期间无帧违反契约；worker 侧 30s 无帧即超时）
    yield _sse(p.frame("beat", 0, {}))

    async def beat_timer(stream):
        """流迭代期间每 30s 无产出补一帧 beat（与流并发；流结束即停）。"""
        while True:
            await asyncio.sleep(30)
            yield _sse(p.frame("beat", 0, {}))

    messages = [m.model_dump(exclude_none=True) for m in req.messages]
    seq = 1
    total_bytes = 0
    truncated = False

    for turn in range(req.max_turns):
        deltas: list[str] = []
        tool_calls: dict[int, dict] = {}
        usage = p.LLMUsage()

        stream_iter = _safe_stream(req, llm)
        async for chunk in _stream_frames(stream_iter, p, beat_interval):
            if isinstance(chunk, dict):
                # 心跳帧（_stream_frames 的 beat 产出 SSE dict）——直接透传，
                # 不得按 StreamChunk 解引用（审计 #1：chunk.delta 曾 AttributeError）
                yield chunk
                continue
            if getattr(chunk, "error", None):
                yield _sse(p.error("model_stream_failed", str(chunk.error)))
                return
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
                # 流式片段按 index 累计合并（首个片段参数常为空——setdefault 首片段
                # 会丢后续参数，真实模型 e2e 实证 arguments={}）
                idx = tc.get("index", 0)
                cur = tool_calls.setdefault(idx, {"id": "", "type": "function", "function": {"name": "", "arguments": ""}})
                if tc.get("id"):
                    cur["id"] = tc["id"]
                fn = tc.get("function") or {}
                if fn.get("name"):
                    cur["function"]["name"] += fn["name"]
                if fn.get("arguments"):
                    cur["function"]["arguments"] += fn["arguments"]
            if chunk.usage:
                usage.tokens_in += chunk.usage.get("tokens_in", 0)
                usage.tokens_out += chunk.usage.get("tokens_out", 0)

        if truncated:
            # 截断时 usage 不完整——契约要求标 usage_partial:true（预设落地）
            usage.usage_partial = True
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
                if name in p.CODE_TOOLS or name in p.CONTROL_TOOLS or name.startswith("mcp:"):
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
