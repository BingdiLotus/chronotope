"""chronotope-harness 骨架（W1 D3–D4）：POST /runs → SSE。

纪律（mvp-落地方案 §9）：
- harness 不拥有 loop，loop 在 Go worker 的 run_workflow（持久层）——harness 是「单段执行器」；
- 无 Session、无本地存储（OpenAI Agents SDK 的 Session 特性故意不用）；
- 模型调用永远走 LiteLLM（OpenAI 协议）；
- 工具分流：API 类工具内联执行并继续循环；CODE 类工具发出 tool_call 帧交棒；
- 三种返回：done（终答）/ tool_call（交棒）/ error（max_turns）。

骨架阶段：/runs 校验协议版本后回 done 帧占位；LiteLLM 流式接入 + API 工具内联见 TODO。
"""

from __future__ import annotations

import asyncio
import logging
import os
from collections.abc import AsyncIterator

from fastapi import FastAPI, HTTPException
from sse_starlette.sse import EventSourceResponse

from . import protocol as p

logger = logging.getLogger("chronotope-harness")

app = FastAPI(title="chronotope-harness", version="0.1.0")

# LiteLLM 网关地址（OpenAI 兼容端点）；模型密钥只在 LiteLLM 侧，harness 不见密钥
LITELLM_BASE_URL = os.environ.get("LITELLM_BASE_URL", "http://localhost:4000")


@app.get("/healthz")
async def healthz() -> dict[str, str]:
    return {"status": "ok", "service": "chronotope-harness"}


@app.post("/runs")
async def runs(req: p.RunRequest) -> EventSourceResponse:
    """POST /runs → SSE 事件流（契约规范 §3）。"""
    return EventSourceResponse(_run(req), ping=15)


async def _run(req: p.RunRequest) -> AsyncIterator[dict[str, str]]:
    """执行一段 agent 循环（骨架：协议校验 + done 占位）。"""

    if req.protocol not in p.SUPPORTED_PROTOCOLS:
        # 错误码稳定（契约规范 §6）；worker 按协议版本路由本不会送错版本，此分支是防御
        yield _sse(p.error("protocol_unsupported", f"protocol {req.protocol} not in {p.SUPPORTED_PROTOCOLS}"))
        return

    # 心跳帧：长时内联 API 工具期间每 ≥30s 必发（契约规范 §3 beat 语义）。
    # 骨架无长时执行，注释保留实现位。
    yield _sse(p.frame("beat", 0, {}))

    # TODO(W1 D3–D4)：LiteLLM 流式调用
    #   1. messages/tools 全部来自入参 → LiteLLM（OpenAI 协议，base_url=LITELLM_BASE_URL）；
    #   2. delta 帧边收边发（seq 递增）；
    #   3. 遇 tool_call：API 类工具内联执行、结果作为 tool 消息继续循环（max_turns 封顶）；
    #      CODE 类工具 → 发 tool_call 帧 {id, name, arguments} 交棒（worker → executor）；
    #   4. 输出累计超 max_output_bytes → 截断 + done{truncated:true}；
    #   5. 无更多工具调用 → done{final, usage}。
    final = (
        f"[chronotope-harness 骨架应答] 已收到 run_id={req.run_id} step={req.step} "
        f"model={req.model} messages={len(req.messages)} tools={len(req.tools)}；"
        "LiteLLM 流式接入待 W1 D3–D4（TODO 见 app/main.py）。"
    )
    yield _sse(p.done(final, p.LLMUsage()))


def _sse(f: p.Frame) -> dict[str, str]:
    return {"data": f.model_dump_json()}
