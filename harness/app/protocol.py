"""/runs 协议类型（契约规范 §3 的 Python 侧镜像）。

权威定义：仓库根目录 契约规范.md 与 docs/contracts/runs-protocol.md。
纪律：harness 无状态——每次调用全部上下文来自入参；`done` 是唯一合法终态。
"""

from __future__ import annotations

from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field, model_validator

PROTOCOL_VERSION = "1.0"

# 当前仅支持协议 1.0（预留蓝绿升级）；run 绑定协议版本路由（蓝绿）
SUPPORTED_PROTOCOLS = {"1.0"}

Role = Literal["system", "user", "assistant", "tool"]
Source = Literal["trusted", "sandbox", "network", "untrusted"]
FrameType = Literal["delta", "tool_call", "turn_end", "done", "error", "beat"]

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
    # 模型 alias；worker 持有 alias → 内部工具引用的固定注册表。
    name: str = Field(pattern=r"^[a-zA-Z0-9_-]{1,64}$")
    description: str = ""
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
    max_output_bytes: int = Field(default=DEFAULT_MAX_OUTPUT_BYTES, gt=0)
    max_output_tokens: int | None = Field(default=None, gt=0)
    timeout_ms: int | None = Field(default=None, gt=0)

    @model_validator(mode="after")
    def unique_tool_aliases(self) -> RunRequest:
        names = [tool.name for tool in self.tools]
        if len(names) != len(set(names)):
            raise ValueError("tool aliases must be unique")
        return self


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
