"""期 3 §D 真实 e2e 缺陷回归：embed 凭据回退与端点降级。"""
import os

from app.llm import _deterministic_embed, embed


def test_deterministic_embed_normalized():
    v = _deterministic_embed("hello")
    assert len(v) == 1024
    assert abs(sum(x * x for x in v) - 1.0) < 1e-6
    # 确定性：同输入同向量
    assert _deterministic_embed("hello") == v


def test_embed_no_key_falls_back_deterministic(monkeypatch):
    """真实 e2e 缺陷：空 key 不设非法 Authorization → 确定性向量回退。"""
    monkeypatch.delenv("OPENAI_API_KEY", raising=False)
    monkeypatch.delenv("LITELLM_API_KEY", raising=False)
    v = embed("x")
    assert len(v) == 1024


def test_embed_endpoint_error_falls_back_deterministic(monkeypatch):
    """真实 e2e 缺陷：litellm 无嵌入模型 400 → 确定性向量回退（写入/查询一致）。"""
    monkeypatch.setenv("OPENAI_API_KEY", "sk-x")
    import httpx

    def boom(*a, **k):
        raise httpx.HTTPStatusError("400", request=None, response=None)

    monkeypatch.setattr(httpx, "post", boom)
    v = embed("x")
    assert len(v) == 1024
