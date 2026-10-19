# /runs 协议（Go worker ↔ Python harness，唯一协议）

> 权威定义：契约规范 §3。平台只认 `POST /runs` 一条协议，harness 可替换。
> 三铁律：① harness **无状态**（不持久化任何会话数据，全部上下文来自入参）；
> ② 幂等由 worker 侧 journal 缓存承担，harness **不实现缓存**；
> ③ 成功段必须收到 `done`；`error` 是显式失败。只有 `tool_call` 后断线不算完整段。
> 当前开发代次允许破坏性调整，不提供旧版内联工具循环的兼容层。

## 请求

```
POST /runs
Content-Type: application/json
```

```json
{
  "protocol": "1.0",
  "run_id": "r_1",
  "session_id": "s_1",
  "step": 12,
  "model": "claude-sonnet-4-6",
  "messages": [
    { "role": "system", "content": "你是一个编码助手。", "source": "trusted" },
    { "role": "user", "content": "写一个能通过测试的模块。" },
    { "role": "assistant", "content": "先检索。", "tool_calls": [{"id":"prior_call","type":"function","function":{"name":"knowledge_search","arguments":"{\"query\":\"规则\"}"}}] },
    { "role": "tool", "content": "{\"nonce\":\"known-tool-result\"}", "tool_call_id": "prior_call", "source": "trusted" }
  ],
  "tools": [
    { "type": "function", "name": "knowledge_search", "description": "检索当前作用域内的知识", "schema": { "type":"object", "properties":{"query":{"type":"string"}}, "required":["query"] }, "risk_class": 0 }
  ],
  "max_output_bytes": 524288,
  "max_output_tokens": 4096,
  "timeout_ms": 30000
}
```

| 字段 | 语义 |
|---|---|
| `protocol` | 当前只接受 `1.0`；run 绑定实际协议及 harness 配置 |
| `run_id` + `step` | 幂等键（`(run_id, step)`）；journal 缓存键 = journal 位置（`run_id + step 名`），**版本绑定在 run，不拼接 model 进键** |
| `messages[].source` | `trusted\|sandbox\|network` 来源标记（注入防护，边界语义 §2） |
| `messages[].tool_calls` / `tool_call_id` | worker 保存完整 assistant calls，再为每个成功/错误结果关联原 call ID；harness 原样传给 provider |
| `tools[].name` | 模型 alias，匹配 `[a-zA-Z0-9_-]{1,64}`，同一清单不能重复；内部 tool_ref 不进入模型工具名 |
| `tools[].description` / `schema` | 真实描述及输入 JSON Schema；由受信 agent 配置固定 |
| `tools[].risk_class` | 0 安全 / 1 敏感 / 2 强制审批（边界语义 §2） |
| `max_output_bytes` | 文本及工具参数输出限额（默认 512KB），超限截断 + `done{truncated:true}` |
| `max_output_tokens` | 正数，传 provider `max_completion_tokens`；应用 worker 默认 4096 |
| `timeout_ms` | 正数，取 run 剩余 deadline；限制 provider 请求及整段模型流 |

## SSE 帧

响应为 `text/event-stream`，每帧一个 `data:` 行：`{type, seq, payload}`。

| type | payload | 语义 |
|---|---|---|
| `delta` | `{text}` | 文本增量 |
| `tool_call` | `{id, name, arguments, raw_arguments, arguments_error?}` | 本段全部调用按 index 顺序发出；name 是 model alias |
| `turn_end` | `{summary}` | 既有观察帧，当前 Python 单段不产生 |
| `done` | `{final, usage, truncated?, usage_partial?}` | **成功段唯一终态**；有 tools 时不是整个 run 的终答 |
| `error` | `{code, message}` | 失败终态 |
| `beat` | `{}` | 等待模型期间定期心跳；不取消挂起中的模型流 |

样例（与 `test/contract/testdata/runs.frames.ndjson` 同源）：

```json
{"type":"delta","seq":1,"payload":{"text":"先检索并保存。"}}
{"type":"tool_call","seq":2,"payload":{"id":"t_3","name":"knowledge_search","arguments":{"query":"规则"},"raw_arguments":"{\"query\":\"规则\"}"}}
{"type":"tool_call","seq":3,"payload":{"id":"t_4","name":"knowledge_remember","arguments":null,"raw_arguments":"{invalid","arguments_error":"invalid tool arguments JSON"}}
{"type":"done","seq":4,"payload":{"final":"先检索并保存。","usage":{"tokens_in":120,"tokens_out":80}}}
```

## 固定工具表与执行权威

应用 profile 的 `application_tools` 定义 alias → 内部 tool_ref、description、input_schema
的一一映射。只有 worker 持有内部引用、固定 bridge URL 和服务认证；模型只提供业务参数。
旧 executor 工具常量不构成应用工具白名单，带冒号的内部引用不能直接作为 provider function name。
bridge context 从 bound run 派生，字段见[应用接入契约](../应用接入契约.md)。

## 终止与截断语义

- 连接断开且未见 `done/error` = 未完成，不能执行仅收到部分帧的工具；未知模型结果按效果账本处理，不能以新 attempt 暗示原调用未发生；
- 帧间隔超时（30s 无帧，beat 缺失）同上述处理；
- 输出超 `max_output_bytes` → 截断 + `done{truncated:true}` + 平台事件 `event.truncated`（截断是 journaled 事实）；
- provider 因 token 上限结束同样标 `truncated:true`；不派发任何不完整的调用，应用 run 显式失败；
- usage 缺失标 `usage_partial:true`，保留已知模型结果并单列计量未知；完整 usage 以 provider 最终累计快照为准，不重复相加；
- `tool_result` 大 payload 走 `result_ref`（引用外置；实现现状：exec 结果 >256KB 外置 RustFS（store 层），其余 payload 由 worker 侧 4096 字节截断内联）。

## 单段推理与参数错误

- 每段只发起一次模型请求，SDK 自动重试关闭。全部 tool calls 之后输出 `done`；harness 不调用任何 API/应用工具。
- `raw_arguments` 保留模型原始字符串。非法 JSON、空字符串或非对象输入产生 `arguments:null + arguments_error`，绝不替换成 `{}` 执行。
- worker 校验注册表/JSON Schema，按确定顺序处理工具，将成功或参数/业务错误关联原 call ID 回喂下一请求；完整 assistant calls 只写一次。
- steps/deadline/输出 token 的派发门由 worker 管理，harness 不持有另一套 `max_turns` 循环。
