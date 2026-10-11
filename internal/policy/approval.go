package policy

import "encoding/json"

import "context"

// ApprovalRequest 是审批路由输入（class 2 危险工具挂起时的路由请求）。
type ApprovalRequest struct {
	TenantID  string `json:"tenant_id"`
	Tool      string `json:"tool"`
	Class     int    `json:"class"`
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id"`
	// Context 是 harness 侧 hook 注入的审批上下文（A1——审批路由的
	// 判断输入之一；也进 HITL digest 的 context 字段供审批者查看）。
	Context json.RawMessage `json:"context,omitempty"`
	// Ownership 是子代理所有权（A5——foreground/background；审批路由可
	// 区分：background 的审批不阻塞父 run 的语义在 spawn 层已保证）。
	Ownership string `json:"ownership,omitempty"`
}

// ApprovalRouter 是审批人路由缝（期 3 §B）：默认 ManualOnly（空集合 =
// 控制台人工）；参考实现 OrgApprovalPolicy（approver 集合 + TTL）。
type ApprovalRouter interface {
	Route(ctx context.Context, req ApprovalRequest) (approvers []string, ttlSeconds int64, err error)
}

// ManualOnly 是默认实现：人工控制台审批（无路由集合）。
type ManualOnly struct{}

func (ManualOnly) Route(context.Context, ApprovalRequest) ([]string, int64, error) {
	return nil, 0, nil
}

// OrgApprovalPolicy 是参考实现（期 3 §B）：org 策略（tool_patterns 匹配 +
// approver 集合 + TTL）——业务方替换审批工作流时实现自己的 Router。
type OrgApprovalPolicy struct {
	Get func(ctx context.Context, tenantID string) (patterns []string, approvers []string, ttl int64, err error)
}

// Route 实现：工具匹配任一 pattern（空 patterns = 全匹配）→ 返回集合+TTL。
func (p OrgApprovalPolicy) Route(ctx context.Context, req ApprovalRequest) ([]string, int64, error) {
	patterns, approvers, ttl, err := p.Get(ctx, req.TenantID)
	if err != nil {
		return nil, 0, err
	}
	if len(patterns) > 0 {
		matched := false
		for _, pat := range patterns {
			if req.Tool == pat {
				matched = true
				break
			}
		}
		if !matched {
			return nil, 0, nil // 未匹配工具：不适用本策略（回落 ManualOnly 语义）
		}
	}
	return approvers, ttl, nil
}
