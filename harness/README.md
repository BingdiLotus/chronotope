# chronotope-harness（Python 无状态服务）

> 定位：**单段执行器**——loop 在 Go worker 的 run_workflow（持久层），harness 每次
> `/runs` 调用跑一段 agent 循环（模型调用 + API 工具内联），遇到代码类工具即停下交棒。
> 协议权威定义：[../../docs/contracts/runs-protocol.md](../../docs/contracts/runs-protocol.md)
> 与 [../../契约规范.md](../../契约规范.md) §3。

## 三条纪律（违反即破坏架构）

1. **无状态**：无 Session、无本地存储；消息历史/工具 schema/模型名全部来自入参
   （OpenAI Agents SDK 的 Session 特性故意不用）。
2. **无密钥**：模型调用走 LiteLLM（OpenAI 协议），密钥只在 LiteLLM 侧。
3. **词汇表**：工具名必须发出规范名（`bash`/`write_file`/…，见协议文档），
   框架自有工具名必须映射后发出。

## 运行

```bash
uv sync
uv run uvicorn app.main:app --port 8000
```

试跑（骨架阶段返回 done 帧占位）：

```bash
curl -N -X POST http://localhost:8000/runs \
  -H 'content-type: application/json' \
  -d '{"protocol":"1.0","run_id":"r_1","session_id":"s_1","step":0,"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}'
```

## 测试

```bash
uv run pytest -q
```

## TODO（W1 D3–D4）

- LiteLLM 流式接入：delta 帧边收边发；API 工具内联执行（max_turns 封顶）；
  CODE 工具 → `tool_call` 帧交棒；`max_output_bytes` 截断 + `done{truncated:true}`。
- beat 心跳帧在长时内联工具期间每 ≥30s 必发。
