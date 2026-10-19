"""工具参数解码；harness 不执行工具，所有调用交给 worker。"""

from __future__ import annotations

import json
from typing import Any

def decode_arguments(raw: str) -> tuple[dict[str, Any] | None, str | None]:
    """保留非法参数的错误事实，绝不把非法输入变成可执行的空对象。"""
    try:
        arguments = json.loads(raw, parse_constant=_reject_constant)
    except (json.JSONDecodeError, ValueError) as exc:
        return None, f"invalid tool arguments JSON: {exc}"
    if not isinstance(arguments, dict):
        return None, "tool arguments must be a JSON object"
    return arguments, None


def _reject_constant(value: str) -> None:
    raise ValueError(f"non-JSON numeric constant {value}")
