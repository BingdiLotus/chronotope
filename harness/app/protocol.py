"""/runs 协议类型（契约规范 §3 的 Python 侧镜像）。

权威定义：仓库根目录 契约规范.md 与 docs/contracts/runs-protocol.md。
纪律：harness 无状态——每次调用全部上下文来自入参；`done` 是唯一合法终态。
"""

from __future__ import annotations

from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field

PROTOCOL_VERSION = "1.0"

# 当前仅支持协议 1.0（预留蓝绿升级）；run 绑定协议版本路由（蓝绿）
SUPPORTED_PROTOCOLS = {"1.0"}

Role = Literal["system", "user", "assistant", "tool"]
Source = Literal["trusted", "sandbox", "network", "untrusted"]
FrameType = Literal["delta", "tool_call", "turn_end", "done", "error", "beat"]

# 工具名词汇表（硬约束，只增不改）——框架自有工具名必须映射后发出
VOCABULARY = {
    "next_speaker",  # 群聊主持（W7 控制工具——CONTROL_TOOLS 同列，词汇表补录）
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
    tool_call_id: str | None = None  # tool 消息回喂关联（Anthropic 兼容 API 硬校验）
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


class Frame(BaseModel):
    type: FrameType
    seq: int
    payload: dict[str, Any]


def frame(t: FrameType, seq: int, payload: dict[str, Any]) -> Frame:
    return Frame(type=t, seq=seq, payload=payload)


def delta(text: str, seq: int) -> Frame:
    return frame("delta", seq, {"text": text})


class DonePayload(BaseModel):
    """done 帧载荷类型化形状（契约完整性——帧构造走类型而非手拼 dict；
    曾误删：手拼 dict 使帧形状脱离类型检查）。"""

    final: str
    usage: LLMUsage = Field(default_factory=LLMUsage)
    truncated: bool | None = None
    usage_partial: bool | None = None


class ErrorPayload(BaseModel):
    """error 帧载荷类型化形状。"""

    code: str
    message: str


def done(final: str, usage: LLMUsage, *, truncated: bool | None = None, seq: int = 0) -> Frame:
    payload = DonePayload(final=final, usage=usage, truncated=truncated,
                          usage_partial=usage.usage_partial or None)
    return frame("done", seq, payload.model_dump(exclude_none=True))


def error(code: str, message: str, seq: int = 0) -> Frame:
    return frame("error", seq, ErrorPayload(code=code, message=message).model_dump())


def is_vocabulary_name(name: str) -> bool:
    return name.startswith("mcp:") or name in VOCABULARY
