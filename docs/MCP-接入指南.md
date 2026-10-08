# MCP 接入指南（期 5 §D）

官方 MCP 服务器接入 Chronotope 会话：连接 → allowlist 配置 → class 分级。

## 1. 连接

```bash
# 会话级连接（URL 为 MCP 服务器的 HTTP/SSE 端点）
curl -X POST http://localhost:8080/sessions/<session_id>/mcp \
  -H 'content-type: application/json' \
  -d '{"server":"echo","url":"http://localhost:9100"}'
```

连接后该 server 的工具经清单下发给模型（工具名 `mcp:<server>:<tool>`）。

## 2. Allowlist 配置（期 3 §C 网关）

```bash
# 只放行 tool_patterns 前缀匹配的工具；未配置 server = 旧路径放行
# （W6 历史段兼容）；配置了即按匹配拦（默认全拒语义）
curl -X PUT http://localhost:8080/orgs/<org_id>/mcp-allowlist \
  -H 'content-type: application/json' \
  -d '{"server":"echo","tool_patterns":["safe."]}'
```

- 执行层防御（期 5 §B 后）：显式配置且不匹配 → 工具拒发（`mcp.call` 不发）
- 审计：允许/拒绝经 audit 事件（`GET /orgs/<org>/audit?kind=mcp`）

## 3. Class 分级建议

| 工具类别 | class | 建议 |
|---|---|---|
| 只读查询（搜索/读取） | 0 | 直接放行 |
| 低风险写（草稿/临时） | 1 | allowlist 放行 |
| 危险操作（删除/生产数据/外部调用） | 2 | `tool_classes: {"mcp:<server>:<tool>": 2}` 强制审批 |
| 网络出口不可信 | 2+ | 审批 + allowlist 双重 |

class 2 触发审批挂起（awaiting_approval——审批策略 approver 集合由
`PUT /orgs/{org}/approval-policy` 配置，TTL 过期自动拒绝）。

## 4. 官方服务器样例

- **Filesystem/Everything**：class 分级——写路径工具 class 2
- **GitHub/GitLab**：allowlist 按仓库前缀（`repo:` 模式）
- **数据库**：默认不接（或只读账号 + class 2 全工具）
