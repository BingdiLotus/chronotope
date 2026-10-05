package restate

import (
	"fmt"

	restate "github.com/restatedev/sdk-go"
)

// WebhookResolveInput 是 webhook 服务的输入（HITL 审批回调，worker-架构设计 §2：
// POST /webhooks/approval/{run_id} → resolve awakeable，幂等）。
//
// spike 关键发现（spike/README.md 实验②）：resolve 必须来自挂起对象之外的
// invocation——同对象 exclusive handler 挂起时仍占排他锁，对象内 Resolve 会死锁。
// 因此 webhook 是独立 Service（无 key、无状态），经 run_id → session →
// PendingAwakeable 定位并解析。
type WebhookResolveInput struct {
	RunID   string `json:"run_id"`
	Payload string `json:"payload"`
}

// webhookDef 注册 HITL 回调服务（幂等：重复 resolve 无害）。
func webhookDef(deps *Deps) restate.ServiceDefinition {
	return restate.NewService(WebhookName).
		Handler("Resolve", restate.NewServiceHandler[WebhookResolveInput, string](
			func(ctx restate.Context, in WebhookResolveInput) (string, error) {
				return resolveApproval(ctx, deps, in)
			}))
}

// resolveApproval 按 run_id 定位会话挂起审批并解析（独立函数便于单测）。
func resolveApproval(ctx restate.Context, deps *Deps, in WebhookResolveInput) (string, error) {
	if in.RunID == "" {
		return "", restate.ToTerminalError(fmt.Errorf("missing run_id"))
	}
	run, err := deps.Store.GetRun(ctx, in.RunID)
	if err != nil {
		return "", restate.ToTerminalError(fmt.Errorf("run %s: %w", in.RunID, err))
	}
	state, err := deps.Sessions.GetState(ctx, run.SessionID)
	if err != nil {
		return "", err
	}
	if state.PendingAwakeable == "" {
		return "", restate.ToTerminalError(fmt.Errorf("run %s 无挂起审批", in.RunID))
	}
	restate.ResolveAwakeable[string](ctx, state.PendingAwakeable, in.Payload)
	return "resolved", nil
}
