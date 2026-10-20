"""无状态单段执行器：每次 /runs 只请求一次模型，输出全部 calls 和 done。

历史、工具执行与下一轮派发由 Go worker 持有；harness 从不执行应用/API 工具。
"""

from __future__ import annotations

import asyncio
import json
import logging
from collections.abc import AsyncIterator
from contextlib import aclosing, suppress

from fastapi import FastAPI
from sse_starlette.sse import EventSourceResponse

from . import protocol as p
from .llm import LLMProvider, build_provider
from .tools import decode_arguments

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


async def _stream_frames(stream_iter, p, beat_interval: float, timeout_ms: int | None = None):
    """流帧循环（beat 保任务——wait_for 取消语义修复，审计 #6）：
    产出 delta/tool_call/beat 帧；错误帧产出后由调用方处理。"""
    next_task = None
    heartbeat = None
    loop = asyncio.get_running_loop()
    deadline = loop.time() + timeout_ms / 1000 if timeout_ms is not None else None
    try:
        while True:
            remaining = deadline - loop.time() if deadline is not None else None
            if remaining is not None and remaining <= 0:
                raise TimeoutError(f"model stream timed out after {timeout_ms} ms")
            if next_task is None:
                next_task = asyncio.create_task(anext(stream_iter))
            heartbeat = asyncio.create_task(asyncio.sleep(beat_interval))
            ready, _ = await asyncio.wait({next_task, heartbeat}, timeout=remaining,
                                         return_when=asyncio.FIRST_COMPLETED)
            if not ready:
                raise TimeoutError(f"model stream timed out after {timeout_ms} ms")
            if next_task in ready:
                heartbeat.cancel()
                try:
                    chunk = next_task.result()
                except StopAsyncIteration:
                    break
                next_task = None
            else:
                yield _sse(p.frame("beat", 0, {}))
                continue
            yield chunk
    finally:
        for task in (heartbeat, next_task):
            if task is not None:
                task.cancel()
                with suppress(asyncio.CancelledError, StopAsyncIteration):
                    await task


async def _safe_stream(req: p.RunRequest, llm: LLMProvider):
    """流异常捕获（真实 e2e 实证：模型 400 时流中断 EOF 而非 error 帧——
    worker 收到 unexpected EOF → restate 僵尸 invocation 无限重试；此处转
    error 帧让 worker 显式失败）。"""
    try:
        async with aclosing(llm.stream(req)) as stream:
            async for chunk in stream:
                yield chunk
    except TimeoutError:
        yield llm.stream_error_chunk(f"model stream timed out after {req.timeout_ms} ms")
    except Exception as exc:  # noqa: BLE001——流异常统一转 error 帧
        logger.exception("stream error run_id=%s", req.run_id)
        yield llm.stream_error_chunk(f"model stream failed: {exc}")


async def _run(req: p.RunRequest, llm: LLMProvider, beat_interval: float = 30.0) -> AsyncIterator[dict[str, str]]:
    """一次模型推理；有工具时完整交棒，再以 done 结束本段。"""

    if req.protocol not in p.SUPPORTED_PROTOCOLS:
        # 错误码稳定（契约规范 §6）；worker 按协议版本路由本不会送错版本，此分支是防御
        yield _sse(p.error("protocol_unsupported", f"protocol {req.protocol} not in {p.SUPPORTED_PROTOCOLS}"))
        return

    # 心跳帧：开场一帧 + 流式期间每 30s 周期补发（契约：每 ≥30s 一发——
    # 模型挂起 196s 实证期间无帧违反契约；worker 侧 30s 无帧即超时）
    yield _sse(p.frame("beat", 0, {}))

    seq = 1
    total_bytes = 0
    truncated = False
    deltas: list[str] = []
    tool_calls: dict[int, dict] = {}
    usage = p.LLMUsage()
    usage_received = False

    async with aclosing(_safe_stream(req, llm)) as stream_iter:
        async with aclosing(_stream_frames(stream_iter, p, beat_interval, req.timeout_ms)) as frames:
            # Timeout belongs to this consumer, whose pending anext task changes
            # between chunks. An asyncio.timeout inside a yielding provider would
            # otherwise cancel the already-finished first anext task.
            try:
                async for chunk in frames:
                    if isinstance(chunk, dict):
                        yield chunk
                        continue
                    if getattr(chunk, "error", None):
                        yield _sse(p.error("model_stream_failed", str(chunk.error)))
                        return
                    if chunk.usage is not None:
                        usage_received = True
                        usage.tokens_in = chunk.usage.get("tokens_in", 0)
                        usage.tokens_out = chunk.usage.get("tokens_out", 0)
                    # 工具参数同样受输出字节上限约束；不派发不完整的调用。
                    generated = (chunk.delta or "") + "".join(
                        (tc.get("function") or {}).get("name", "")
                        + (tc.get("function") or {}).get("arguments", "")
                        for tc in chunk.tool_calls
                    )
                    total_bytes += len(generated.encode("utf-8"))
                    if total_bytes > req.max_output_bytes:
                        truncated = True
                        break
                    if chunk.delta:
                        deltas.append(chunk.delta)
                        yield _sse(p.delta(chunk.delta, seq))
                        seq += 1
                    for tc in chunk.tool_calls:
                        # provider 产出原始片段，仅在此按 index 合并一次。
                        idx = tc.get("index", 0)
                        cur = tool_calls.setdefault(idx, {"id": "", "name": "", "arguments": ""})
                        if tc.get("id"):
                            cur["id"] = tc["id"]
                        fn = tc.get("function") or {}
                        cur["name"] += fn.get("name") or ""
                        cur["arguments"] += fn.get("arguments") or ""
                    if getattr(chunk, "finish_reason", None) == "length":
                        truncated = True
            except TimeoutError as exc:
                yield _sse(p.error("model_stream_failed", str(exc), seq=seq))
                return

    usage.usage_partial = not usage_received
    if truncated:
        # 字节上限提前断流时通常无 usage；provider 的 length 终止可仍有完整计量。
        yield _sse(p.done("".join(deltas), usage, truncated=True, seq=seq))
        return

    for idx in sorted(tool_calls):
        tc = tool_calls[idx]
        if not tc["id"] or not tc["name"]:
            yield _sse(p.error("model_tool_call_invalid", "tool call is missing id or name", seq=seq))
            return
        arguments, arguments_error = decode_arguments(tc["arguments"])
        payload = {
            "id": tc["id"], "name": tc["name"], "arguments": arguments,
            "raw_arguments": tc["arguments"],
        }
        if arguments_error:
            payload["arguments_error"] = arguments_error
        yield _sse(p.frame("tool_call", seq, payload))
        seq += 1

    yield _sse(p.done("".join(deltas), usage, seq=seq))


def _sse(f: p.Frame) -> dict[str, str]:
    return {"data": f.model_dump_json()}
