package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/bingdilotus/chronotope/internal/core/wait"
	"time"
)

// goalStateHash 对象状态 hash（M2：投影可重建——关键决策有来源）。
func goalStateHash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte("|"))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// UpsertGoal 目标写入（version 递增 + state_hash——慢变量更新需显式理由）。
func (s *Store) UpsertGoal(ctx context.Context, id, orgID, objective, scope, owner string) (*GoalRow, error) {
	hash := goalStateHash(id, objective, scope)
	const q = `
INSERT INTO goals (id, org_id, objective, scope, version, owner, state_hash, updated_at)
VALUES ($1, $2, $3, $4, 1, $5, $6, now())
ON CONFLICT (id) DO UPDATE
SET objective = EXCLUDED.objective, scope = EXCLUDED.scope,
    version = goals.version + 1, owner = EXCLUDED.owner,
    state_hash = EXCLUDED.state_hash, updated_at = now()
RETURNING id, org_id, objective, scope, version, owner, status, state_hash`
	var g GoalRow
	if err := s.Pool.QueryRow(ctx, q, id, orgID, objective, scope, owner, hash).Scan(
		&g.ID, &g.OrgID, &g.Objective, &g.Scope, &g.Version, &g.Owner, &g.Status, &g.StateHash); err != nil {
		return nil, fmt.Errorf("store: upsert goal: %w", err)
	}
	return &g, nil
}

// GoalRow 目标行。
type GoalRow struct {
	ID        string `json:"id"`
	OrgID     string `json:"org_id"`
	Objective string `json:"objective"`
	Scope     string `json:"scope"`
	Version   int    `json:"version"`
	Owner     string `json:"owner"`
	Status    string `json:"status"`
	StateHash string `json:"state_hash"`
}

// GetGoal 读目标（含 version/hash——在过期快照上执行的检测）。
func (s *Store) GetGoal(ctx context.Context, id string) (*GoalRow, error) {
	const q = `SELECT id, org_id, objective, scope, version, owner, status, state_hash FROM goals WHERE id = $1`
	var g GoalRow
	if err := s.Pool.QueryRow(ctx, q, id).Scan(&g.ID, &g.OrgID, &g.Objective, &g.Scope, &g.Version, &g.Owner, &g.Status, &g.StateHash); err != nil {
		return nil, fmt.Errorf("store: get goal: %w", err)
	}
	return &g, nil
}

// CreateWorkItem 工作切片（goal 队列条目）。
func (s *Store) CreateWorkItem(ctx context.Context, id, goalID, description, taskClass string, priority int, dependencies string) error {
	const q = `
INSERT INTO work_items (id, goal_id, description, priority, dependencies, task_class)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (id) DO NOTHING`
	if _, err := s.Pool.Exec(ctx, q, id, goalID, description, priority, dependencies, taskClass); err != nil {
		return fmt.Errorf("store: create work item: %w", err)
	}
	return nil
}

// ListOpenWorkItems goal 的可领取切片（status=open）。
func (s *Store) ListOpenWorkItems(ctx context.Context, goalID string, limit int) ([]map[string]any, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.Pool.Query(ctx, `SELECT id, description, priority, task_class, dependencies FROM work_items WHERE goal_id = $1 AND status = 'open' ORDER BY priority DESC, created_at LIMIT $2`, goalID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list work items: %w", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id, desc, class, deps string
		var prio int
		if err := rows.Scan(&id, &desc, &prio, &class, &deps); err != nil {
			return nil, fmt.Errorf("store: scan work item: %w", err)
		}
		out = append(out, map[string]any{"id": id, "description": desc, "priority": prio, "task_class": class, "dependencies": deps})
	}
	return out, rows.Err()
}

// PutEvidence 证据记录（新鲜度字段——valid_for/source_revision/expires_at/method）。
func (s *Store) PutEvidence(ctx context.Context, id, sessionID, runID, blobHash, validFor, sourceRevision, method string, expiresAt *time.Time) error {
	hash := goalStateHash(blobHash, validFor, sourceRevision)
	const q = `
INSERT INTO evidence (id, session_id, run_id, blob_hash, valid_for, source_revision, expires_at, method, state_hash)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (id) DO NOTHING`
	if _, err := s.Pool.Exec(ctx, q, id, sessionID, runID, blobHash, nullIfEmpty(validFor), nullIfEmpty(sourceRevision), expiresAt, nullIfEmpty(method), hash); err != nil {
		return fmt.Errorf("store: put evidence: %w", err)
	}
	return nil
}

// StaleEvidence 过期证据（M2 新鲜度：行动前发现依赖证据已过期）。
func (s *Store) StaleEvidence(ctx context.Context, sessionID string) ([]map[string]any, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, blob_hash, produced_at, COALESCE(valid_for,''), COALESCE(source_revision,''), expires_at, COALESCE(method,'') FROM evidence WHERE session_id = $1 AND (expires_at IS NOT NULL AND expires_at < now())`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: stale evidence: %w", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id, hash, vf, rev, method string
		var pa time.Time
		var exp *time.Time
		if err := rows.Scan(&id, &hash, &pa, &vf, &rev, &exp, &method); err != nil {
			return nil, fmt.Errorf("store: scan stale evidence: %w", err)
		}
		out = append(out, map[string]any{"id": id, "blob_hash": hash, "produced_at": pa, "valid_for": vf, "source_revision": rev, "expires_at": exp, "method": method})
	}
	return out, rows.Err()
}

// ActionClaim 动作级可过期占有权（层 0 原语——与 lease 同族，无业务语义）。
type ActionClaim struct {
	Scope        string
	AgentID      string
	Capabilities string
	Generation   int64
	ExpiresAt    time.Time
}

// AcquireActionClaim 领取动作域（scope 唯一；过期后可接管；活跃的异 agent 拒绝）。
func (s *Store) AcquireActionClaim(ctx context.Context, scope, agentID, capabilities string, ttl time.Duration) (*ActionClaim, error) {
	const q = `
INSERT INTO action_claims (scope, agent_id, capabilities, generation, expires_at)
VALUES ($1, $2, $3, 1, now() + $4)
ON CONFLICT (scope) DO UPDATE
SET agent_id = EXCLUDED.agent_id, capabilities = EXCLUDED.capabilities,
    generation = action_claims.generation + 1, expires_at = now() + $4
WHERE action_claims.agent_id = EXCLUDED.agent_id OR action_claims.expires_at < now()
RETURNING scope, agent_id, capabilities, generation, expires_at`
	var c ActionClaim
	if err := s.Pool.QueryRow(ctx, q, scope, agentID, nullIfEmpty(capabilities), ttl).Scan(&c.Scope, &c.AgentID, &c.Capabilities, &c.Generation, &c.ExpiresAt); err != nil {
		return nil, fmt.Errorf("store: acquire action claim: %w", err)
	}
	return &c, nil
}

// ReleaseActionClaim 释放（代次校验——ABA 语义同 lease）。
func (s *Store) ReleaseActionClaim(ctx context.Context, scope string, generation int64) (bool, error) {
	const q = `UPDATE action_claims SET expires_at = now() WHERE scope = $1 AND generation = $2`
	tag, err := s.Pool.Exec(ctx, q, scope, generation)
	if err != nil {
		return false, fmt.Errorf("store: release action claim: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// SchedulerHint 调度提示（期 7 下沉：core/wait）。
type SchedulerHint = wait.SchedulerHint

// SchedulerHint 常量（core/wait 的别名）。
const (
	HintRun    SchedulerHint = wait.HintRun
	HintWait   SchedulerHint = wait.HintWait
	HintAsk    SchedulerHint = wait.HintAsk
	HintReplan SchedulerHint = wait.HintReplan
	HintRepair SchedulerHint = wait.HintRepair
	HintQuiet  SchedulerHint = wait.HintQuiet
)

// Decision 是 DECIDE 的判定结果（期 7 下沉：core/wait）。
type Decision = wait.Decision
