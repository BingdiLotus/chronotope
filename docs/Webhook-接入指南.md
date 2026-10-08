# Webhook 接入指南（期 5 §D）

事件投递订阅（outbox，落地方案 §5）：会话事件实时 POST 到你的端点
（Slack/Feishu incoming webhook 形态或自建接收端点）。

## 1. 订阅

```bash
curl -X POST http://localhost:8080/sessions/<session_id>/subscriptions \
  -H 'content-type: application/json' \
  -d '{"channel":"webhook","target":"https://your-endpoint.example/hook"}'
```

- `channel`：`webhook`（默认）| `email`
- `target`：URL（SSRF 校验——生产环境默认拒绝私网地址，`OUTBOX_ALLOW_PRIVATE=true` 放行）

## 2. 投递载荷

```json
{
  "session_id": "s_xxx",
  "type": "run.completed",
  "payload": { "v": 1, "final": "...", "steps": 3 },
  "at": "2026-10-08T12:00:00Z"
}
```

终态事件（run.completed/failed/cancelled + 审批挂起 awaiting_approval）随包投递。

## 3. 重试语义

- 指数退避：`5s × 2^n`，封顶 1h，**最多 8 次**（outbox maxAttempts）
- 非 2xx/3xx 视为失败（3xx 也视为失败——终态 2xx 才算送达）

## 4. 签名验证（防伪造投递）

每个投递带 `X-Chronotope-Signature: t=<unix秒>,v1=<hex>`：

```
密钥 = HMAC-SHA256(平台密钥, org_id)     # 平台密钥 = OUTBOX_SIGNING_SECRET（生产必配）
签名 = HMAC-SHA256(密钥, 请求体)          # hex 编码
```

接收方验证（Go 样例）：

```go
func verify(body []byte, header, platformSecret, orgID string, maxAge time.Duration) bool {
    parts := map[string]string{}
    for _, kv := range strings.Split(header, ",") {
        k, v, _ := strings.Cut(kv, "=")
        parts[k] = v
    }
    ts, _ := strconv.ParseInt(parts["t"], 10, 64)
    if time.Since(time.Unix(ts, 0)) > maxAge {
        return false // 防重放：时间窗（建议 5 分钟）
    }
    mac := hmac.New(sha256.New, []byte(platformSecret))
    mac.Write([]byte(orgID))
    key := mac.Sum(nil)
    mac2 := hmac.New(sha256.New, key)
    mac2.Write(body)
    return hmac.Equal([]byte(parts["v1"]), []byte(hex.EncodeToString(mac2.Sum(nil))))
}
```

Python 样例：

```python
import hmac, hashlib, time

def verify(body: bytes, header: str, platform_secret: str, org_id: str, max_age: int = 300) -> bool:
    parts = dict(kv.split("=") for kv in header.split(","))
    if abs(time.time() - int(parts["t"])) > max_age:
        return False
    key = hmac.new(platform_secret.encode(), org_id.encode(), hashlib.sha256).digest()
    want = hmac.new(key, body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(parts["v1"], want)
```

## 5. 注意事项

- `org_id`：接收方按「订阅时记录的 session 归属」获得（订阅响应的 session 所在
  org——管理面 `GET /orgs/{org}/sessions` 可查）
- 平台密钥：生产 `OUTBOX_SIGNING_SECRET` 必配（开发默认 `chronotope`——文档化
  的默认仅用于本地）
- 事件顺序：单会话内按 seq 投递（重试可能乱序——接收方按 `payload` 内 seq 容忍）
