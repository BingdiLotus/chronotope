package restate

import (
	"fmt"

	restate "github.com/restatedev/sdk-go"
)

// WebhookResolveInput 是 webhook 服务的输入（HITL 审批回调，worker-架构设计 §2）。
//
// spike 关键发现（spike/README.md 实验②）：resolve 必须来自挂起对象之外的
// invocation——同对象 exclusive handler 挂起时仍占排他锁，对象内 Resolve 会死锁。
// 因此 webhook 是独立 Service（无 key、无状态）。
type WebhookResolveInput struct {
	AwakeableID string `json:"awakeable_id"`
	Payload     string `json:"payload"`
}

// webhookDef 注册 HITL 回调服务：resolve awakeable（幂等，重复 resolve 无害）。
func webhookDef() restate.ServiceDefinition {
	return restate.NewService(WebhookName).
		Handler("Resolve", restate.NewServiceHandler[WebhookResolveInput, string](
			func(ctx restate.Context, in WebhookResolveInput) (string, error) {
				if in.AwakeableID == "" {
					return "", restate.ToTerminalError(fmt.Errorf("missing awakeable_id"))
				}
				restate.ResolveAwakeable[string](ctx, in.AwakeableID, in.Payload)
				return "resolved", nil
			}))
}
