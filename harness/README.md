# chronotope-harness（Python 无状态服务）

> 定位：**单段执行器**——loop 在 Go worker 的 run_workflow（持久层），harness 每次
> `/runs` 调用跑一段 agent 循环（模型调用 + API 工具内联），遇到代码类工具即停下交棒。
> 协议权威定义：[../../docs/contracts/runs-protocol.md](../../docs/contracts/runs-protocol.md)
> 与 [../../契约规范.md](../../契约规范.md) §3。

## 三条纪律（违反即破坏架构）

1. **无状态**：无 Session、无本地存储；消息历史/工具 schema/模型名全部来自入参
   （OpenAI Agents SDK 的 Session 特性故意不用）。
2. **无密钥**：模型调用走 LiteLLM 网关（OpenAI 协议），密钥只在 LiteLLM 侧；
   harness 仅持网关 master key。
3. **词汇表**：工具名必须发出规范名（`bash`/`write_file`/…，见协议文档），
   框架自有工具名必须映射后发出。

## 模型提供层（provider 抽象）

| 模式 | 环境变量 | 说明 |
|---|---|---|
| LiteLLM 网关（默认） | `LITELLM_BASE_URL` + `LITELLM_API_KEY` | `app/llm.OpenAIProvider`：借 openai SDK 流式/工具调用，多模型翻译在网关 |
| Fake（e2e/演示） | `HARNESS_FAKE_MODEL=1`（可配 `HARNESS_FAKE_REPLY`） | `app/llm.FakeProvider`：流式回放固定回复，无密钥跑通全链路 |

未配置密钥且非 fake 模式 → `/runs` 返回 `error{harness_misconfigured}`（明确报错，不静默降级）。

## 运行

```bash
uv sync
HARNESS_FAKE_MODEL=1 uv run uvicorn app.main:app --port 8000   # 演示/无密钥
uv run uvicorn app.main:app --port 8000                        # 真实模型（需 LITELLM_* 环境）
```

试跑：

```bash
curl -N -X POST http://localhost:8000/runs \
  -H 'content-type: application/json' \
  -d '{"protocol":"1.0","run_id":"r_1","session_id":"s_1","step":0,"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}'
```

## 循环语义（`app/main._run`）

- `beat` 帧开头 → 模型流式调用 → `delta` 帧边收边发；
- 输出超 `max_output_bytes` → 截断 + `done{truncated:true}`（截断是 journaled 事实）；
- 工具分流：`CODE_TOOLS`（bash/run_python/read_file/write_file/list_files）→ `tool_call`
  帧**交棒**（worker → executor）；`API_TOOLS`（http_request/web_search）→ 内联执行、
  结果回喂、继续循环；副作用请求（非 GET）拒绝并回馈模型；
- 三种终态：`done`（唯一合法终态）/ `tool_call`（交棒）/ `error`（max_turns）。

## 测试

```bash
uv run pytest -q          # 14 项：契约 4 + 循环 7 + provider 3
```

- `tests/test_contract.py`：/runs 协议样例（fake provider，无密钥）；
- `tests/test_loop.py`：脚本化 provider 注入——delta 顺序/截断/交棒/内联工具/非法参数
  兜底/max_turns/协议版本防御；
- `tests/test_llm_provider.py`：MockTransport 桩替 LiteLLM 网关——请求形状（model/stream/
  include_usage/tools/鉴权）与流式解析（delta/tool_calls 按 index 累积/流末 usage）。
