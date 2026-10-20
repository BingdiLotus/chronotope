"""契约测试（W1 起随契约落库）：/runs 协议样例——请求校验 + done 终帧语义。

与 Go 侧 test/contract 及 docs/contracts/runs-protocol.md 样例同源。
"""

import json
import os
import pytest

# 契约测试走 FakeProvider（无模型密钥；真实 LiteLLM 路径由 test_llm_provider 覆盖）
os.environ.setdefault("HARNESS_FAKE_MODEL", "1")

from fastapi.testclient import TestClient

from app.main import app

client = TestClient(app)

SAMPLE_REQUEST = {
    "protocol": "1.0",
    "run_id": "r_1",
    "session_id": "s_1",
    "step": 12,
    "model": "claude-sonnet-4-6",
    "messages": [{"role": "user", "content": "hello"}],
    "tools": [{"type": "function", "name": "bash", "schema": {}, "risk_class": 1}],
    "max_output_bytes": 524288,
}


def _frames(response) -> list[dict]:
    frames = []
    for line in response.iter_lines():
        if line.startswith("data: "):
            frames.append(json.loads(line[len("data: "):]))
    return frames


def test_runs_streams_done_terminal_frame():
    """唯一合法终态：done 帧（契约规范 §3）。"""
    with client.stream("POST", "/runs", json=SAMPLE_REQUEST) as resp:
        assert resp.status_code == 200
        frames = _frames(resp)
    assert frames, "应至少收到一帧"
    assert frames[-1]["type"] == "done", f"终帧必须是 done，实际 {frames[-1]['type']}"
    assert frames[-1]["payload"]["final"]
    assert "usage" in frames[-1]["payload"]


def test_runs_request_missing_required_fields_rejected():
    """run_id/session_id 必填（幂等键贯穿全链路）。"""
    bad = {**SAMPLE_REQUEST, "run_id": ""}
    with client.stream("POST", "/runs", json=bad) as resp:
        assert resp.status_code == 422


def test_runs_unsupported_protocol_errors():
    """协议版本防御：错误帧 code 稳定。"""
    bad = {**SAMPLE_REQUEST, "protocol": "9.9"}
    with client.stream("POST", "/runs", json=bad) as resp:
        frames = _frames(resp)
    assert frames[-1]["type"] == "error"
    assert frames[-1]["payload"]["code"] == "protocol_unsupported"


@pytest.mark.parametrize("name", ["mcp:github:search", "a b", "", "x" * 65])
def test_invalid_model_alias_rejected(name):
    bad = {**SAMPLE_REQUEST, "tools": [{"name": name}]}
    assert client.post("/runs", json=bad).status_code == 422


def test_registered_application_alias_is_accepted():
    req = {**SAMPLE_REQUEST, "tools": [{"name": "knowledge_search", "description": "Search knowledge", "schema": {"type": "object"}}]}
    assert client.post("/runs", json=req).status_code == 200


def test_duplicate_model_alias_rejected():
    bad = {**SAMPLE_REQUEST, "tools": [{"name": "same"}, {"name": "same"}]}
    assert client.post("/runs", json=bad).status_code == 422


@pytest.mark.parametrize("field", ["max_output_tokens", "max_output_bytes", "timeout_ms"])
def test_nonpositive_limits_rejected(field):
    assert client.post("/runs", json={**SAMPLE_REQUEST, field: 0}).status_code == 422
