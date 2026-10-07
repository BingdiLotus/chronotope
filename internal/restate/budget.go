package restate

import (
	"context"

	restate "github.com/restatedev/sdk-go"

	"github.com/bingdilotus/chronotope/internal/core/event"
	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
	"github.com/bingdilotus/chronotope/internal/policy"
)

// budgetSnapshotOf 策略缝快照装配：nil/AllowAll → 零快照（Check 恒 Allow）；
// 参考实现（*policy.OrgDailyBudget）→ SnapshotOrg。业务方实现自己的
// BudgetPolicy 时，若快照装配不同（如订阅席位），在此类型切换扩展。
func budgetSnapshotOf(ctx context.Context, p BudgetPolicy, tenantID string) policy.BudgetSnapshot {
	if p == nil {
		return policy.BudgetSnapshot{}
	}
	if snap, ok := p.(interface {
		SnapshotOrg(context.Context, string) policy.BudgetSnapshot
	}); ok {
		return snap.SnapshotOrg(ctx, tenantID)
	}
	return policy.BudgetSnapshot{TenantID: tenantID}
}

// 预算检查已迁至策略缝（期 3 §A）：internal/policy.BudgetPolicy + 参考实现
// OrgDailyBudget（原 org 日预算逻辑平移）。本文件保留 freezeRun——
// 冻结槽是层 0 运行态原语（挂起零进程占用等待外部信号），策略缝判定
// Hold 后调用；业务语义不在运行时硬编码。

// budgetCheck 策略缝判定：nil = AllowAll（无业务=无限）。
func budgetCheck(ctx context.Context, p BudgetPolicy, snap policy.BudgetSnapshot, acc policy.Accum) policy.Decision {
	if p == nil {
		return policy.Decision{Allow: true}
	}
	return p.Check(ctx, snap, acc)
}

// freezeRun 挂起 run 等待解冻信号（冻结槽原语——欠费/预算只是策略缝的
// 一种 Hold 来源；awaiting_approval 同构）。
func freezeRun(ctx restate.Context, deps *Deps, in RunInput, runID string, emit *Emitter, key string) error {
	awakeable := restate.Awakeable[string](ctx)
	if err := deps.Sessions.SetFrozenAwakeable(ctx, in.SessionID, awakeable.Id()); err != nil {
		return err
	}
	_ = emit.Emit(ctx, in.SessionID, runID, 0, event.BudgetExceeded, "budget", key, map[string]any{
		"run_id": runID, "key": key,
	})
	_ = emit.Emit(ctx, in.SessionID, runID, 0, event.RunFrozen, "", "", map[string]any{
		"key": key, "awakeable_id": awakeable.Id(),
	})
	_ = deps.Store.UpdateRunStatus(ctx, runID, sessionapi.RunFrozen)
	payload, err := awakeable.Result() // 挂起：零进程占用，直到 Unfreeze 跨 HTTP resolve
	if err != nil {
		return err
	}
	_ = emit.Emit(ctx, in.SessionID, runID, 0, event.RunUnfrozen, "", "", map[string]any{
		"payload": payload,
	})
	_ = deps.Store.UpdateRunStatus(ctx, runID, sessionapi.RunRunning)
	return nil
}
