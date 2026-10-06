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
// budgetSnapshot 是 org 预算入口快照（正确性二期 ⑧：journal 化——run 内检查
// 只基于快照 + 本 run 累计，不再读可变 org 用量；重放回放同一快照，确定性）。
// 字段必须导出 + json 标签（journal 序列化；未导出会重放成 nil——e2e 实证过的坑）。
type budgetSnapshot struct {
	OrgID        string  `json:"org_id"`
	QuotaTokens  float64 `json:"quota_tokens"`
	QuotaCompute float64 `json:"quota_compute"`
	UsageTokens  int64   `json:"usage_tokens"`
	UsageCompute float64 `json:"usage_compute"`
}

// snapshotOrgBudget 读 org 配额 + 当日用量（一次；失败 fail-open 零值）。
func snapshotOrgBudget(ctx context.Context, deps *Deps, orgID string) (budgetSnapshot, error) {
	org, err := deps.Store.GetOrg(ctx, orgID)
	if err != nil || org == nil {
		return budgetSnapshot{OrgID: orgID}, nil
	}
	tokens, compute, err := deps.Store.OrgDailyUsage(ctx, orgID, time.Now().UTC().Truncate(24*time.Hour))
	if err != nil {
		return budgetSnapshot{OrgID: orgID}, nil
	}
	snap := budgetSnapshot{OrgID: orgID, UsageTokens: tokens, UsageCompute: compute}
	if b, ok := org.Quotas[store.QuotaDailyTokenBudget]; ok {
		if f, ok := b.(float64); ok && f > 0 {
			snap.QuotaTokens = f
		}
	}
	if b, ok := org.Quotas[store.QuotaDailyComputeBudget]; ok {
		if f, ok := b.(float64); ok && f > 0 {
			snap.QuotaCompute = f
		}
	}
	return snap, nil
}

// quotaExceededSnap 快照判定：快照用量 + 本 run 累计 vs 快照配额（0 = 无限）。
func quotaExceededSnap(snap budgetSnapshot, accTokens int64, accCompute float64) (bool, string) {
	if snap.QuotaTokens > 0 && float64(snap.UsageTokens+accTokens) >= snap.QuotaTokens {
		return true, store.QuotaDailyTokenBudget
	}
	if snap.QuotaCompute > 0 && snap.UsageCompute+accCompute >= snap.QuotaCompute {
		return true, store.QuotaDailyComputeBudget
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
