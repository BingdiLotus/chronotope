# /runs 协议（Go worker ↔ Python harness，唯一协议）

> 权威定义：契约规范 §3。平台只认 `POST /runs` 一条协议，harness 可替换。
> 三铁律：① harness **无状态**（不持久化任何会话数据，全部上下文来自入参）；
> ② 幂等由 worker 侧 journal 缓存承担，harness **不实现缓存**；
> ③ `done` 是唯一合法终态（连接断开未见 done = 未完成 → 幂等重发同 `(run_id, step)` ≤3 次）。

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
    { "role": "assistant", "content": "好的，我先写实现。", "tool_calls": [] },
    { "role": "tool", "content": "exit: 2\n...", "source": "sandbox" }
  ],
  "tools": [
    { "type": "function", "name": "bash", "schema": { }, "risk_class": 1 }
  ],
  "max_turns": 8,
  "max_output_bytes": 524288
}
```

| 字段 | 语义 |
|---|---|
| `protocol` | 协议版本；平台同时支持最近 2 个版本，run 绑定协议版本路由到对应 harness 镜像（蓝绿） |
| `run_id` + `step` | 幂等键（`(run_id, step)`）；journal 缓存键 = journal 位置（`run_id + step 名`），**版本绑定在 run，不拼接 model 进键** |
| `messages[].source` | `trusted\|sandbox\|network` 来源标记（注入防护，边界语义 §2） |
| `tools[].risk_class` | 0 安全 / 1 敏感 / 2 强制审批（边界语义 §2） |
| `max_output_bytes` | 输出限额（默认 512KB），超限截断 + `done{truncated:true}` |

## SSE 帧

响应为 `text/event-stream`，每帧一个 `data:` 行：`{type, seq, payload}`。

| type | payload | 语义 |
|---|---|---|
| `delta` | `{text}` | 文本增量 |
| `tool_call` | `{id, name, arguments}` | 交棒（name 必须是词汇表规范名） |
| `turn_end` | `{summary}` | 一段循环结束（供 UI） |
| `done` | `{final, usage, truncated?}` | **唯一合法终态** |
| `error` | `{code, message}` | 失败终态 |
| `beat` | `{}` | 心跳帧（每 ≥30s 一发；内联长时 API 工具期间必发） |

样例（与 `test/contract/testdata/runs.frames.ndjson` 同源）：

```json
{"type":"delta","seq":0,"payload":{"text":"好的，"}}
{"type":"tool_call","seq":1,"payload":{"id":"t_3","name":"bash","arguments":{"command":"pytest"}}}
{"type":"done","seq":2,"payload":{"final":"已完成。","usage":{"tokens_in":120,"tokens_out":80},"truncated":true}}
```

## 工具名词汇表（硬约束，只增不改）

`bash` · `run_python` · `read_file` · `write_file` · `list_files` · `web_search` ·
`http_request` · `request_approval` · `spawn_subagent` · `mcp:<server>:<tool>`

任何 harness 实现必须发出规范名；框架自有工具名必须映射后发出。

## 终止与截断语义

- 连接断开且未见 `done` = 未完成 → worker 幂等重发同 `(run_id, step)`，最多 3 次 → `run.failed{harness_unavailable}`；
- 帧间隔超时（30s 无帧，beat 缺失）同上述处理；
- 输出超 `max_output_bytes` → 截断 + `done{truncated:true}` + 平台事件 `event.truncated`（截断是 journaled 事实）；
- 截断/断流时 `done.usage` 允许不完整：以已收到分片为准、标 `usage_partial:true`（Anthropic 流断流时无 usage，契约必须容忍）；
- `tool_result` 大 payload 走 `result_ref`（引用外置，见契约规范 §7 journal 大小策略）。

## 工具分流（harness 侧实现要点）

- **API 类工具**（web_search/http_request 等）在 harness 内联执行并继续循环；
- **代码类工具**（bash/run_python/read_file/write_file/list_files）发出 `tool_call` 帧后返回（交棒）——worker 转 executor；
- 三种返回：`done`（终答）/ `tool_call`（交棒）/ `error`（max_turns）。
