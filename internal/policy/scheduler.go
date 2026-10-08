package policy

import "context"

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
}

// RoundRobin 是默认实现：按 sessionID 哈希轮转（同会话稳定轮转位——
// 无状态、确定性）。
type RoundRobin struct{}

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
