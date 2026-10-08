package restate

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/bingdilotus/chronotope/internal/execproto"
	"github.com/bingdilotus/chronotope/internal/policy"
	"github.com/bingdilotus/chronotope/internal/store"
)

// ExecutorPool 是多宿主池（期 4 §B）：实现 Executor 接口——沙箱归属路由
// （sandboxes.executor_id → 属主客户端）+ 新会话经 SchedulerPolicy 选择；
// 单点兼容（无注册行时退回 fallback 客户端——现有部署无感）。
type ExecutorPool struct {
	mu        sync.Mutex
	clients   map[string]Executor  // executor_id → 客户端
	deadUntil map[string]time.Time // 连接失败冷却期（剔除后 30s 内不复选——
	// 否则 clientFor 惰性重建立即复选死节点——w15 下线恢复 FAIL 实证）
	fallback  Executor // 注册表为空时的单点兜底
	Policy    policy.SchedulerPolicy
	Freshness time.Duration // 心跳新鲜窗口（默认 2 分钟）

	// 依赖注入（worker 装配）
	ListExecutors   func(ctx context.Context) ([]store.ExecutorRow, error)
	SandboxOwner    func(ctx context.Context, sandboxID string) (string, error) // 沙箱归属 executor_id（无 → ""）
	SetSandboxOwner func(ctx context.Context, sandboxID, executorID string) error
}

// NewExecutorPool 构造（fallback 单点客户端——EXECUTOR_URL 兼容）。
func NewExecutorPool(fallback Executor, pol policy.SchedulerPolicy) *ExecutorPool {
	if pol == nil {
		pol = policy.RoundRobin{}
	}
	return &ExecutorPool{clients: map[string]Executor{}, deadUntil: map[string]time.Time{}, fallback: fallback, Policy: pol, Freshness: 2 * time.Minute}
}

// clientFor 解析沙箱归属或策略选新——返回 (客户端, executorID)。
func (p *ExecutorPool) clientFor(ctx context.Context, sandboxID, sessionID string) (Executor, string) {
	if p.SandboxOwner != nil {
		if owner, err := p.SandboxOwner(ctx, sandboxID); err == nil && owner != "" {
			if c, ok := p.client(owner); ok {
				return c, owner
			}
		}
	}
	// 候选 = 注册表新鲜行
	var candidates []policy.ExecutorCandidate
	if p.ListExecutors != nil {
		if rows, err := p.ListExecutors(ctx); err == nil {
			for _, r := range rows {
				if p.isCooling(r.ID) {
					continue // 冷却期：连接失败剔除后 30s 内不选中
				}
				candidates = append(candidates, policy.ExecutorCandidate{ID: r.ID, Endpoint: r.Endpoint, Kind: r.Kind})
				if _, ok := p.client(r.ID); !ok {
					p.setClient(r.ID, NewExecutorClient(r.Endpoint))
				}
			}
		}
	}
	if len(candidates) > 0 {
		chosen := p.Policy.Pick(ctx, sessionID, candidates)
		if chosen.ID != "" {
			if c, ok := p.client(chosen.ID); ok {
				return c, chosen.ID
			}
		}
	}
	return p.fallback, "" // 单点兜底（无归属 id）
}

func (p *ExecutorPool) client(id string) (Executor, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.clients[id]
	return c, ok
}

func (p *ExecutorPool) setClient(id string, c Executor) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clients[id] = c
}

func (p *ExecutorPool) CreateSandbox(ctx context.Context, req execproto.CreateSandboxRequest) (string, error) {
	c, owner := p.clientFor(ctx, req.SessionID, req.SessionID)
	id, err := c.CreateSandbox(ctx, req)
	// 连接失败降级（期 4 §B 演练实证：心跳窗口内死节点仍被选中——connection
	// refused）→ 标记冷却 + 剔除客户端 + 重选一次
	if err != nil && owner != "" && p.isConnErr(err) {
		p.mu.Lock()
		delete(p.clients, owner)
		p.deadUntil[owner] = time.Now().Add(30 * time.Second)
		p.mu.Unlock()
		if c2, owner2 := p.clientFor(ctx, req.SessionID, req.SessionID); c2 != nil && owner2 != owner {
			id, err = c2.CreateSandbox(ctx, req)
			owner = owner2
		}
	}
	if err == nil && owner != "" && p.SetSandboxOwner != nil {
		_ = p.SetSandboxOwner(ctx, id, owner) // 归属落库（后续操作路由属主）
	}
	return id, err
}

// isCooling 冷却期判定（连接失败后 30s 内不复选）。
func (p *ExecutorPool) isCooling(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return time.Now().Before(p.deadUntil[id])
}

// isConnErr 连接类错误（dial refused/timeout——候选存活判定而非业务错误）。
func (p *ExecutorPool) isConnErr(err error) bool {
	s := err.Error()
	return strings.Contains(s, "connection refused") || strings.Contains(s, "no such host")
}

func (p *ExecutorPool) Execute(ctx context.Context, sandboxID, name, input, idempotencyKey string) (*ExecResult, error) {
	c, _ := p.clientFor(ctx, sandboxID, "")
	return c.Execute(ctx, sandboxID, name, input, idempotencyKey)
}

func (p *ExecutorPool) ReadFile(ctx context.Context, sandboxID, path string) (string, error) {
	c, _ := p.clientFor(ctx, sandboxID, "")
	return c.ReadFile(ctx, sandboxID, path)
}

func (p *ExecutorPool) WriteFile(ctx context.Context, sandboxID, path, content string) error {
	c, _ := p.clientFor(ctx, sandboxID, "")
	return c.WriteFile(ctx, sandboxID, path, content)
}

func (p *ExecutorPool) AcquireLease(ctx context.Context, sandboxID, runID, ttl string) (int64, error) {
	c, _ := p.clientFor(ctx, sandboxID, "")
	return c.AcquireLease(ctx, sandboxID, runID, ttl)
}

func (p *ExecutorPool) ReleaseLease(ctx context.Context, sandboxID string, generation int64) error {
	c, _ := p.clientFor(ctx, sandboxID, "")
	return c.ReleaseLease(ctx, sandboxID, generation)
}

func (p *ExecutorPool) Snapshot(ctx context.Context, sandboxID string) (string, error) {
	c, _ := p.clientFor(ctx, sandboxID, "")
	return c.Snapshot(ctx, sandboxID)
}
