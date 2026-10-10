package policy

import (
	"context"
	"testing"
	"time"
)

// TestOrgDailySnapshot 契约：配额视图的装配（float64 正数才生效、查询失败
// fail-open 空快照——参考业务层的边界）。
func TestOrgDailySnapshot(t *testing.T) {
	p := OrgDailyBudget{
		Org: func(context.Context, string) (map[string]any, error) {
			return map[string]any{"daily_token_budget": 1000.0, "daily_compute_seconds": 60.0}, nil
		},
		Usage: func(context.Context, string, time.Time) (int64, float64, error) { return 100, 3.5, nil },
	}
	snap := p.SnapshotOrg(context.Background(), "o1")
	if snap.QuotaTokens != 1000 || snap.QuotaCompute != 60 || snap.UsageTokens != 100 || snap.UsageCompute != 3.5 {
		t.Fatalf("快照装配: %+v", snap)
	}
	// 查询失败 → fail-open 空快照（不误伤）
	p2 := OrgDailyBudget{
		Org:   func(context.Context, string) (map[string]any, error) { return nil, context.DeadlineExceeded },
		Usage: func(context.Context, string, time.Time) (int64, float64, error) { return 0, 0, nil },
	}
	if s := p2.SnapshotOrg(context.Background(), "o1"); s.QuotaTokens != 0 || s.UsageTokens != 0 {
		t.Fatalf("失败应 fail-open: %+v", s)
	}
}

// TestOrgDailyCheck 熔断判定：累计超配额拒绝（token 与 compute 两轴）、
// 未超放行——⑧ 语义平移的硬验收。
func TestOrgDailyCheck(t *testing.T) {
	p := OrgDailyBudget{}
	// token 超限
	if d := p.Check(context.Background(), BudgetSnapshot{TenantID: "o", QuotaTokens: 100, UsageTokens: 80}, Accum{Tokens: 30}); d.Allow || d.Key != KeyDailyTokenBudget {
		t.Fatalf("token 超限应拒: %+v", d)
	}
	// compute 超限
	if d := p.Check(context.Background(), BudgetSnapshot{TenantID: "o", QuotaCompute: 60, UsageCompute: 50}, Accum{Compute: 20}); d.Allow || d.Key != KeyDailyComputeBudget {
		t.Fatalf("compute 超限应拒: %+v", d)
	}
	// 未超放行
	if d := p.Check(context.Background(), BudgetSnapshot{TenantID: "o", QuotaTokens: 100, UsageTokens: 10}, Accum{Tokens: 20}); !d.Allow {
		t.Fatalf("未超应放行: %+v", d)
	}
	// 配额 0 = 无限（无预算键不熔断）
	if d := p.Check(context.Background(), BudgetSnapshot{TenantID: "o"}, Accum{Tokens: 999}); !d.Allow {
		t.Fatalf("配额 0 应无限: %+v", d)
	}
}
