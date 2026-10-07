package policy

import (
	"context"
	"time"

	"github.com/bingdilotus/chronotope/internal/store"
)

// OrgDailyBudget 是参考业务实现（期 3 §A：现有 org 日预算平移——w5-budget
// e2e 不回归是硬验收）。业务方替换计费模型（订阅/席位）时实现自己的
// BudgetPolicy 注入即可，运行时零改动。
type OrgDailyBudget struct {
	// Org 是租户配额视图（读 orgs.quotas——参考业务层数据；无参考实现时忽略）。
	Org func(ctx context.Context, tenantID string) (quotas map[string]any, err error)
	// Usage 是租户当日用量视图（token/计算秒）。
	Usage func(ctx context.Context, tenantID string, day time.Time) (tokens int64, compute float64, err error)
}

// 策略键与租户配额键一致（store.QuotaDaily*——freeze 事件 key 语义不变）。
const (
	KeyDailyTokenBudget   = store.QuotaDailyTokenBudget
	KeyDailyComputeBudget = store.QuotaDailyComputeBudget
)

// SnapshotOrg 从租户配额+用量构造快照（参考实现的数据装配；fail-open）。
func (p OrgDailyBudget) SnapshotOrg(ctx context.Context, tenantID string) BudgetSnapshot {
	snap := BudgetSnapshot{TenantID: tenantID}
	quotas, err := p.Org(ctx, tenantID)
	if err != nil || quotas == nil {
		return snap
	}
	tokens, compute, err := p.Usage(ctx, tenantID, time.Now().UTC().Truncate(24*time.Hour))
	if err != nil {
		return snap
	}
	snap.UsageTokens, snap.UsageCompute = tokens, compute
	if b, ok := quotas[store.QuotaDailyTokenBudget]; ok {
		if f, ok := b.(float64); ok && f > 0 {
			snap.QuotaTokens = f
		}
	}
	if b, ok := quotas[store.QuotaDailyComputeBudget]; ok {
		if f, ok := b.(float64); ok && f > 0 {
			snap.QuotaCompute = f
		}
	}
	return snap
}

// Check 实现（⑧ 语义平移：快照 + 累计 vs 配额）。
func (p OrgDailyBudget) Check(_ context.Context, snap BudgetSnapshot, acc Accum) Decision {
	if snap.QuotaTokens > 0 && float64(snap.UsageTokens+acc.Tokens) >= snap.QuotaTokens {
		return Decision{Allow: false, Key: KeyDailyTokenBudget}
	}
	if snap.QuotaCompute > 0 && snap.UsageCompute+acc.Compute >= snap.QuotaCompute {
		return Decision{Allow: false, Key: KeyDailyComputeBudget}
	}
	return Decision{Allow: true}
}
