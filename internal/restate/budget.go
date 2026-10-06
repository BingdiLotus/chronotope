package restate

import (
	"context"
	"time"

	restate "github.com/restatedev/sdk-go"

	"github.com/bingdilotus/chronotope/internal/core/event"
	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
	"github.com/bingdilotus/chronotope/internal/store"
)

// orgQuotaExceeded 三级熔断 ② 级检查：org 当日用量 vs 预算（0/缺省 = 无限）。
// 返回 (超限, 键, error)；org 读失败按不阻断处理（fail-open，防御配额元数据故障）。
func orgQuotaExceeded(ctx context.Context, deps *Deps, orgID string, now time.Time) (bool, string) {
	org, err := deps.Store.GetOrg(ctx, orgID)
	if err != nil || org == nil {
		return false, ""
	}
	tokens, compute, err := deps.Store.OrgDailyUsage(ctx, orgID, now.UTC().Truncate(24*time.Hour))
	if err != nil {
		return false, ""
	}
	if b, ok := org.Quotas[store.QuotaDailyTokenBudget]; ok {
		if f, ok := b.(float64); ok && f > 0 && float64(tokens) >= f {
			return true, store.QuotaDailyTokenBudget
		}
	}
	if b, ok := org.Quotas[store.QuotaDailyComputeBudget]; ok {
		if f, ok := b.(float64); ok && f > 0 && compute >= f {
			return true, store.QuotaDailyComputeBudget
		}
	}
	return false, ""
}

// freezeRun 欠费冻结（边界语义 §1）：budget.exceeded + run.frozen → awakeable 挂起
// （零成本，不杀 run）→ 充值后 Unfreeze resolve → run.unfrozen → 继续执行。
// 冻结槽独立于审批槽（FrozenAwakeable），二者可共存互不干扰。
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
