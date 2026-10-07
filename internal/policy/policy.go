// Package policy 是基础设施策略缝（期 3 §A 三层分离）：业务规则经接口注入，
// 运行时只认接口——默认无业务（无限/全过）；参考实现由业务层提供并可整体替换。
package policy

import "context"

// BudgetSnapshot 是预算判定的输入视图（⑧ 确定性语义：入口快照 + run 内累计；
// 与业务字段解耦——配额/用量视图由策略实现自行解释）。
type BudgetSnapshot struct {
	TenantID     string  `json:"tenant_id"`
	QuotaTokens  float64 `json:"quota_tokens"`
	QuotaCompute float64 `json:"quota_compute"`
	UsageTokens  int64   `json:"usage_tokens"`
	UsageCompute float64 `json:"usage_compute"`
}

// Accum 是 run 内累计（token/计算秒——⑧ 快照+累计确定性）。
type Accum struct {
	Tokens  int64   `json:"tokens"`
	Compute float64 `json:"compute"`
}

// Decision 是预算判定：Allow 放行；Hold 挂起等待外部信号（冻结槽——层 0
// 原语：挂起零成本，信号经 awakeable 到达）。
type Decision struct {
	Allow bool   `json:"allow"`
	Key   string `json:"key,omitempty"` // Hold 原因键（如 "daily_token_budget"）
}

// BudgetPolicy 是预算判定缝。实现须确定性（重放同分支——⑧ 纪律）。
type BudgetPolicy interface {
	Check(ctx context.Context, snap BudgetSnapshot, acc Accum) Decision
}

// AllowAll 是默认实现：无业务 = 无限（不设业务层时的底座行为）。
type AllowAll struct{}

func (AllowAll) Check(context.Context, BudgetSnapshot, Accum) Decision { return Decision{Allow: true} }
