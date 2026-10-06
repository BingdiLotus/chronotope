"""/runs 协议类型（契约规范 §3 的 Python 侧镜像）。

权威定义：仓库根目录 契约规范.md 与 docs/contracts/runs-protocol.md。
纪律：harness 无状态——每次调用全部上下文来自入参；`done` 是唯一合法终态。
"""

from __future__ import annotations

from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field

PROTOCOL_VERSION = "1.0"

# 平台同时支持最近 2 个协议版本；run 绑定协议版本路由（蓝绿）
SUPPORTED_PROTOCOLS = {"1.0"}

Role = Literal["system", "user", "assistant", "tool"]
Source = Literal["trusted", "sandbox", "network"]
FrameType = Literal["delta", "tool_call", "turn_end", "done", "error", "beat"]

# 工具名词汇表（硬约束，只增不改）——框架自有工具名必须映射后发出
VOCABULARY = {
    "bash",
    "run_python",
    "read_file",
    "write_file",
    "list_files",
    "web_search",
    "http_request",
    "request_approval",
    "spawn_subagent",
}

# 代码类工具：harness 只发 tool_call 交棒，由 worker 转 executor（harness 从不直连 executor）
CODE_TOOLS = {"bash", "run_python", "read_file", "write_file", "list_files"}

# 控制类工具：harness 交棒给 worker 的控制面（awakeable/子 Agent），harness 不内联
CONTROL_TOOLS = {"request_approval", "spawn_subagent", "next_speaker"}

DEFAULT_MAX_TURNS = 8
DEFAULT_MAX_OUTPUT_BYTES = 512 * 1024


class Message(BaseModel):
    role: Role
    content: str
    tool_calls: list[dict[str, Any]] | None = None
    source: Source = "trusted"


class Tool(BaseModel):
    model_config = ConfigDict(populate_by_name=True)

    type: Literal["function", "mcp"] = "function"
    name: str
    tool_schema: dict[str, Any] = Field(default_factory=dict, alias="schema")
    risk_class: int = 0


class RunRequest(BaseModel):
    protocol: str = PROTOCOL_VERSION
    run_id: str = Field(min_length=1)
    session_id: str = Field(min_length=1)
    step: int
    model: str
    messages: list[Message]
    tools: list[Tool] = Field(default_factory=list)
    max_turns: int = DEFAULT_MAX_TURNS
    max_output_bytes: int = DEFAULT_MAX_OUTPUT_BYTES


class LLMUsage(BaseModel):
    tokens_in: int = 0
    tokens_out: int = 0
    usage_partial: bool = False


class DonePayload(BaseModel):
    final: str
    usage: LLMUsage = Field(default_factory=LLMUsage)
    truncated: bool | None = None
    usage_partial: bool | None = None


class ErrorPayload(BaseModel):
    code: str
    message: str


class Frame(BaseModel):
    type: FrameType
    seq: int
    payload: dict[str, Any]


def frame(t: FrameType, seq: int, payload: dict[str, Any]) -> Frame:
    return Frame(type=t, seq=seq, payload=payload)


def delta(text: str, seq: int) -> Frame:
    return frame("delta", seq, {"text": text})


def done(final: str, usage: LLMUsage, *, truncated: bool | None = None, seq: int = 0) -> Frame:
    payload: dict[str, Any] = {"final": final, "usage": usage.model_dump()}
    if truncated is not None:
        payload["truncated"] = truncated
    return frame("done", seq, payload)


def error(code: str, message: str, seq: int = 0) -> Frame:
    return frame("error", seq, {"code": code, "message": message})


def is_vocabulary_name(name: str) -> bool:
    return name.startswith("mcp:") or name in VOCABULARY
