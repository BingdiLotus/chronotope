package restate

import (
	"encoding/json"
	"fmt"
	"time"

	restate "github.com/restatedev/sdk-go"

	"github.com/bingdilotus/chronotope/internal/core/event"
	"github.com/bingdilotus/chronotope/internal/policy"
)

// containsStr 字符串集合成员判定。
func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

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
	// ActionDigest 待审批动作摘要（评审 #5 精确绑定；空 = 旧客户端后向兼容，
	// 审计事件标记 legacy 放行）
	ActionDigest string `json:"action_digest"`
	Approver     string `json:"approver"`
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
	// 动作精确绑定（评审 #5 + 审计 #8）：空 digest 拒绝——legacy 放行删除
	// （自报 approver + 空 digest 曾绕过授权链）
	if in.ActionDigest == "" {
		return "", restate.ToTerminalError(fmt.Errorf("action_digest 必填（精确绑定）"))
	}
	if state.PendingActionDigest != "" && in.ActionDigest != state.PendingActionDigest {
		return "", restate.ToTerminalError(fmt.Errorf("审批摘要不匹配：请求 %s ≠ 挂起 %s", in.ActionDigest, state.PendingActionDigest))
	}
	// 审批策略路由（期 3 §B）：ApprovalRouter 策略缝——approver 集合校验 +
	// TTL 过期自动拒绝。默认 ManualOnly（无路由集合）放行；参考实现
	// OrgApprovalPolicy（org 策略）。审计事件留痕。
	if sess, sErr := deps.Store.GetSession(ctx, run.SessionID); sErr == nil && deps.ApprovalRouter != nil {
		approvers, ttl, rErr := deps.ApprovalRouter.Route(ctx, policy.ApprovalRequest{
			TenantID: sess.OrgID, Tool: state.PendingTool, Class: 2, SessionID: run.SessionID, RunID: in.RunID,
		})
		if rErr == nil && len(approvers) > 0 {

			allowed := in.Approver != "" && containsStr(approvers, in.Approver)
			if !allowed {
				denied, _ := json.Marshal(map[string]any{
					"run_id": in.RunID, "approver": in.Approver, "reason": "approver_not_in_policy",
				})
				_, _ = deps.Store.AppendEvent(ctx, run.SessionID, in.RunID, event.AuditApprovalDenied, denied, run.SessionID+":audit:approval_denied:"+in.RunID)
				return "", restate.ToTerminalError(fmt.Errorf("审批人 %q 不在策略集合", in.Approver))
			}
		}
		if rErr == nil && ttl > 0 && state.PendingSince != nil &&
			time.Since(*state.PendingSince) > time.Duration(ttl)*time.Second {
			expired, _ := json.Marshal(map[string]any{
				"run_id": in.RunID, "ttl_seconds": ttl, "reason": "approval_expired",
			})
			_, _ = deps.Store.AppendEvent(ctx, run.SessionID, in.RunID, event.AuditApprovalExpired, expired, run.SessionID+":audit:approval_expired:"+in.RunID)
			// 过期自动拒绝：解析 awakeable 为拒绝决议（run 以 tool_denied 终态收场）
			rejectPayload, _ := json.Marshal(map[string]any{"approved": false, "note": "approval expired"})
			restate.ResolveAwakeable[string](ctx, state.PendingAwakeable, string(rejectPayload))
			return "expired", nil
		}
	}
	restate.ResolveAwakeable[string](ctx, state.PendingAwakeable, in.Payload)
	// 审计事件：审批者与决议留痕（幂等 dedupe 按 run）
	audit, _ := json.Marshal(map[string]any{
		"run_id": in.RunID, "approver": in.Approver,
		"action_digest": in.ActionDigest, "legacy": false,
		"decision": approvalGranted(in.Payload),
	})
	if _, err := deps.Store.AppendEvent(ctx, run.SessionID, in.RunID, event.AuditApproval, audit, run.SessionID+":audit:approval:"+in.RunID); err != nil {
		return "", err
	}
	return "resolved", nil
}
