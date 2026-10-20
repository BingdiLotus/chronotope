# chronotope-harness（Python 无状态服务）

> 定位：**单段执行器**——loop 在 Go worker 的 run_workflow（持久层），harness 每次
> `/runs` 调用只发起一次模型请求。全部工具调用交给 worker，harness 不执行工具。
> 协议权威定义：[runs-protocol.md](../docs/contracts/runs-protocol.md)
> 与 [契约规范.md](../契约规范.md) §3；本期范围见[应用接入契约](../docs/应用接入契约.md)。

## 三条纪律（违反即破坏架构）

1. **无状态**：无 Session、无本地存储；消息历史/工具 schema/模型名全部来自入参
   （OpenAI Agents SDK 的 Session 特性故意不用）。
2. **无密钥**：模型调用走 LiteLLM 网关（OpenAI 协议），密钥只在 LiteLLM 侧；
   harness 仅持网关 master key。
3. **固定工具表**：`tools[].name` 是合法模型 alias（字母、数字、下划线或连字符，
   1–64 字符，不能重复）。内部引用、目的地址与执行授权由 worker 管理。
   `description`、`schema` 原样提供给模型。

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

## 单段语义（`app/main._run`）

- `beat` 帧开头 → 模型流式调用 → `delta` 帧边收边发；
- 一次模型流结束后按 index 顺序输出全部 `tool_call`，然后输出带 usage 的 `done`；
- `tool_call` 保留原 `id`、alias 和 `raw_arguments`。非法 JSON 或非对象参数输出
  `arguments:null` 和 `arguments_error`，由 worker 关联原 call ID 回喂错误，不能执行 `{}`；
- worker 构造包含全部 calls 的 assistant 历史，再回喂每条工具成功/错误结果；
  harness 只序列化输入历史，工具消息的 `tool_call_id` 原样保留；
- 文本及工具参数超 `max_output_bytes`，或 provider 因 token 上限结束 →
  `done{truncated:true}`，不交付不完整工具。缺失 usage 单列 `usage_partial:true`，保留已知结果；
- 可选正数 `max_output_tokens` 传为 provider 的 `max_completion_tokens`；
  `timeout_ms` 同时约束请求 timeout 和整段流。SDK 自动重试关闭，派发/重试由 worker 管理；
- 成功段以 `done` 结束；失败以 `error` 结束。没有 harness 内部模型/工具循环。

Provider 字段依据 [OpenAI Chat Completions 官方文档](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create)：
`max_completion_tokens` 限制生成 token，流末 `include_usage` 可能在中断时缺失，工具结果以 `tool_call_id` 关联。

Fake 脚本按 `step` 选段，同 run/step 重发保持同结果；既支持单条 `tool_call`，
也支持一段多条 `tool_calls`。`raw_arguments` 可用于非法参数故障 fixture。

## 测试

```bash
uv run pytest -q
```

- `tests/test_contract.py`：/runs 协议样例（fake provider，无密钥）；
- `tests/test_loop.py`：一次推理、全部工具交棒、非法参数、usage 缺失、字节/token 上限、
  timeout、流关闭与协议版本防御；
- `tests/test_llm_provider.py`：MockTransport 桩替 LiteLLM 网关——请求形状（model/stream/
  include_usage/tools/鉴权/限额）与流式解析，以及完整 assistant/tool 历史和原 call ID。
