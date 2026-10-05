"""契约测试（W1 起随契约落库）：/runs 协议样例——请求校验 + done 终帧语义。

与 Go 侧 test/contract 及 docs/contracts/runs-protocol.md 样例同源。
"""

import json

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
    "max_turns": 8,
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


def test_vocabulary_names():
    """工具名词汇表硬约束（契约规范 §3）。"""
    from app import protocol

    for name in ["bash", "write_file", "request_approval", "mcp:github:search"]:
        assert protocol.is_vocabulary_name(name), name
    assert not protocol.is_vocabulary_name("run_shell")
