package policy

import (
	"context"
	"time"

	"github.com/bingdilotus/chronotope/internal/store"
)

// ExecutorCandidate 是调度候选（期 4 §B：多宿主池的装箱前置——心跳/能力）。
type ExecutorCandidate struct {
	ID       string `json:"id"`
	Endpoint string `json:"endpoint"`
	Kind     string `json:"kind"`
}

// SchedulerPolicy 是 executor 选择策略缝（层 1——与 BudgetPolicy/ApprovalRouter
// 同构；默认 RoundRobin 无业务语义；参考实现 AffinityBySession 按会话亲和
// 保持沙箱卷本地性；装箱/成本优先由业务层替换实现）。
type SchedulerPolicy interface {
	Pick(ctx context.Context, sessionID string, candidates []ExecutorCandidate) ExecutorCandidate
	// Decide M2 DECIDE 先行：定时器到点先判定本轮是否值得运行（run/wait/
	// ask/replan/repair/quiet）——触发器只消费提示不判断业务。默认 RoundRobin
	// 恒 run（现有部署无感）；参考实现（QuietWhenIdle）按新证据/等待状态 quiet。
	Decide(ctx context.Context, sessionID string) store.Decision
}

// RoundRobin 是默认实现：按 sessionID 哈希轮转（同会话稳定轮转位——
// 无状态、确定性）。
type RoundRobin struct{}

// Decide 默认恒 run（现有部署无感——业务语义由参考实现覆盖）。
func (RoundRobin) Decide(_ context.Context, _ string) store.Decision {
	return store.Decision{Hint: store.HintRun, Reason: "default: run"}
}

// QuietWhenIdle 参考实现（层 2 语义）：session 无活跃 run 且近 10 分钟无
// run.completed 事件 → quiet（无新证据不烧 token——经济学基线的空转关闭）。
type QuietWhenIdle struct {
	HasActiveRun    func(ctx context.Context, sessionID string) (bool, error)
	RecentCompleted func(ctx context.Context, sessionID string, within time.Duration) (bool, error)
}

func (q QuietWhenIdle) Decide(ctx context.Context, sessionID string) store.Decision {
	if q.HasActiveRun != nil {
		if active, err := q.HasActiveRun(ctx, sessionID); err == nil && active {
			return store.Decision{Hint: store.HintWait, Reason: "active run in progress"}
		}
	}
	if q.RecentCompleted != nil {
		if recent, err := q.RecentCompleted(ctx, sessionID, 10*time.Minute); err == nil && !recent {
			return store.Decision{Hint: store.HintQuiet, Reason: "no new evidence in 10m"}
		}
	}
	return store.Decision{Hint: store.HintRun, Reason: "evidence fresh"}
}

// WaitHandleDecide 可判定版本（期 6 ①）：查 session_waits——「现在在等什么、
// 条件满足了吗」。有活跃等待 → wait（不 quiet——等待中的会话不该被判空转）；
// 无等待 → 交给启发式（QuietWhenIdle 同款）。
type WaitHandleDecide struct {
	ActiveWaits     func(ctx context.Context, sessionID string) ([]map[string]any, error)
	HasActiveRun    func(ctx context.Context, sessionID string) (bool, error)
	RecentCompleted func(ctx context.Context, sessionID string, within time.Duration) (bool, error)
}

// Pick 委托 RoundRobin（SchedulerPolicy 接口的候选选择部分——DECIDE 只
// 管判定，装箱选择仍轮转）。
func (w WaitHandleDecide) Pick(ctx context.Context, sessionID string, candidates []ExecutorCandidate) ExecutorCandidate {
	return RoundRobin{}.Pick(ctx, sessionID, candidates)
}

func (w WaitHandleDecide) Decide(ctx context.Context, sessionID string) store.Decision {
	if w.ActiveWaits != nil {
		if waits, err := w.ActiveWaits(ctx, sessionID); err == nil && len(waits) > 0 {
			// 有活跃等待：在等 CI/审批/外部事件——不是空转（可判定：wait
			// 而非 quiet——唤醒路径查 WaitHandle 再判定）
			kind, _ := waits[0]["kind"].(string)
			return store.Decision{Hint: store.HintWait, Reason: "waiting: " + kind}
		}
	}
	if w.HasActiveRun != nil {
		if active, err := w.HasActiveRun(ctx, sessionID); err == nil && active {
			return store.Decision{Hint: store.HintWait, Reason: "active run in progress"}
		}
	}
	if w.RecentCompleted != nil {
		if recent, err := w.RecentCompleted(ctx, sessionID, 10*time.Minute); err == nil && !recent {
			return store.Decision{Hint: store.HintQuiet, Reason: "no new evidence in 10m"}
		}
	}
	return store.Decision{Hint: store.HintRun, Reason: "evidence fresh"}
}

func (RoundRobin) Pick(_ context.Context, sessionID string, candidates []ExecutorCandidate) ExecutorCandidate {
	if len(candidates) == 0 {
		return ExecutorCandidate{}
	}
	h := 0
	for _, c := range sessionID {
		h = (h*31 + int(c)) % len(candidates)
	}
	return candidates[h]
}

// AffinityBySession 是参考实现：同会话固定 executor（沙箱卷本地性——
// 会话的沙箱归属 executor 后全部操作路由属主；仅新会话走选择）。
type AffinityBySession struct {
	// Affinity 返回会话已归属的 executor（无归属 → 空 id）。
	Affinity func(ctx context.Context, sessionID string) string
}

func (p AffinityBySession) Pick(ctx context.Context, sessionID string, candidates []ExecutorCandidate) ExecutorCandidate {
	if p.Affinity != nil {
		if id := p.Affinity(ctx, sessionID); id != "" {
			for _, c := range candidates {
				if c.ID == id {
					return c
				}
			}
		}
	}
	return RoundRobin{}.Pick(ctx, sessionID, candidates)
}
