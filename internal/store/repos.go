package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
	"github.com/jackc/pgx/v5"
)

// ErrNotFound 是资源不存在的哨兵错误（api 层映射为 404）。
var ErrNotFound = errors.New("not found")

// CreateOrg 幂等创建 org（MVP 单 org 跑通，quota 字段先预留）。
func (s *Store) CreateOrg(ctx context.Context, id, name string) error {
	const q = `INSERT INTO orgs (id, name) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`
	if _, err := s.Pool.Exec(ctx, q, id, name); err != nil {
		return fmt.Errorf("store: create org: %w", err)
	}
	return nil
}

// Agent 是 agents 表行（config 版本化：每次变更 version+1，run 启动时绑定）。
type Agent struct {
	ID      string
	OrgID   string
	Name    string
	Config  sessionapi.AgentConfig
	Version int
}

// CreateAgent 创建 agent（id 由调用方生成，保证「api 生成 id」的单一职责）。
func (s *Store) CreateAgent(ctx context.Context, id, orgID, name string, cfg *sessionapi.AgentConfig) error {
	cfg.Version = 1
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("store: marshal agent config: %w", err)
	}
	const q = `
INSERT INTO agents (id, org_id, name, config, version)
VALUES ($1, $2, $3, $4, $5)`
	if _, err := s.Pool.Exec(ctx, q, id, orgID, name, cfgJSON, cfg.Version); err != nil {
		return fmt.Errorf("store: create agent: %w", err)
	}
	return nil
}

// GetAgent 读取 agent（含当前 config）。
func (s *Store) GetAgent(ctx context.Context, id string) (*Agent, error) {
	const q = `SELECT id, org_id, name, config, version FROM agents WHERE id = $1`
	var (
		a       Agent
		cfgJSON json.RawMessage
	)
	err := s.Pool.QueryRow(ctx, q, id).Scan(&a.ID, &a.OrgID, &a.Name, &cfgJSON, &a.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get agent: %w", err)
	}
	if err := json.Unmarshal(cfgJSON, &a.Config); err != nil {
		return nil, fmt.Errorf("store: unmarshal agent config: %w", err)
	}
	return &a, nil
}

// Session 是 sessions 表行。
type Session struct {
	ID           string
	OrgID        string
	AgentID      string
	Status       sessionapi.SessionPhase
	RestateKey   *string
	LastActiveAt *time.Time
	DeletedAt    *time.Time
}

// CreateSession 创建 session（id 由调用方生成；status=created，沙箱懒创建完成后 → ready）。
func (s *Store) CreateSession(ctx context.Context, id, orgID, agentID string) error {
	const q = `
INSERT INTO sessions (id, org_id, agent_id, status, restate_key)
VALUES ($1, $2, $3, 'created', $1)`
	if _, err := s.Pool.Exec(ctx, q, id, orgID, agentID); err != nil {
		return fmt.Errorf("store: create session: %w", err)
	}
	return nil
}

// GetSession 读取会话。
func (s *Store) GetSession(ctx context.Context, id string) (*Session, error) {
	const q = `SELECT id, org_id, agent_id, status, restate_key, last_active_at, deleted_at FROM sessions WHERE id = $1`
	var sess Session
	err := s.Pool.QueryRow(ctx, q, id).Scan(
		&sess.ID, &sess.OrgID, &sess.AgentID, &sess.Status, &sess.RestateKey, &sess.LastActiveAt, &sess.DeletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get session: %w", err)
	}
	return &sess, nil
}

// UpdateSessionStatus 迁移会话状态（状态机权威定义见 sessionapi）。
func (s *Store) UpdateSessionStatus(ctx context.Context, id string, status sessionapi.SessionPhase) error {
	const q = `UPDATE sessions SET status = $2, last_active_at = now() WHERE id = $1`
	if _, err := s.Pool.Exec(ctx, q, id, string(status)); err != nil {
		return fmt.Errorf("store: update session status: %w", err)
	}
	return nil
}

// SoftDeleteSession 两段式删除第一步：tombstone（立即可见性移除；异步 compaction 后置）。
func (s *Store) SoftDeleteSession(ctx context.Context, id string) error {
	const q = `UPDATE sessions SET status = 'deleted', deleted_at = now() WHERE id = $1`
	if _, err := s.Pool.Exec(ctx, q, id); err != nil {
		return fmt.Errorf("store: soft delete session: %w", err)
	}
	return nil
}

// SessionOrg 读取会话所属 org（executor 创建沙箱行时回填 org_id 用）。
func (s *Store) SessionOrg(ctx context.Context, sessionID string) (string, error) {
	const q = `SELECT org_id FROM sessions WHERE id = $1`
	var orgID string
	err := s.Pool.QueryRow(ctx, q, sessionID).Scan(&orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: session org: %w", err)
	}
	return orgID, nil
}

// CreateSchedule 记录一次性唤醒计划（W3：delay 形态；cron 表 + 时区语义后置，边界语义 §5）。
func (s *Store) CreateSchedule(ctx context.Context, id, orgID, sessionID string, delay time.Duration, payload json.RawMessage) error {
	const q = `
INSERT INTO schedules (id, org_id, session_id, cron, next_at, payload)
VALUES ($1, $2, $3, '', now() + ($4::text || ' milliseconds')::interval, $5)`
	delayText := fmt.Sprintf("%d", delay.Milliseconds())
	if _, err := s.Pool.Exec(ctx, q, id, orgID, sessionID, delayText, nullableRaw(payload)); err != nil {
		return fmt.Errorf("store: create schedule: %w", err)
	}
	return nil
}

// Run 是 runs 表行；Bound 是 run 启动绑定的 {agent_config_version, protocol_version, model}。
type Run struct {
	ID        string
	SessionID string
	Status    sessionapi.RunStatus
	Bound     map[string]any
}

// CreateRun 幂等创建 run 行（Idempotency-Key → run_id 去重）。
// 已存在时返回 false，不覆盖。
func (s *Store) CreateRun(ctx context.Context, id, sessionID string, trigger json.RawMessage, bound map[string]any) (created bool, err error) {
	boundJSON, err := json.Marshal(bound)
	if err != nil {
		return false, fmt.Errorf("store: marshal run bound: %w", err)
	}
	const q = `
INSERT INTO runs (id, session_id, trigger, status, bound)
VALUES ($1, $2, $3, 'queued', $4)
ON CONFLICT (id) DO NOTHING
RETURNING id`
	var got string
	err = s.Pool.QueryRow(ctx, q, id, sessionID, nullableRaw(trigger), boundJSON).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // 已存在（幂等）
	}
	if err != nil {
		return false, fmt.Errorf("store: create run: %w", err)
	}
	return true, nil
}

// GetRun 读取 run。
func (s *Store) GetRun(ctx context.Context, id string) (*Run, error) {
	const q = `SELECT id, session_id, status, bound FROM runs WHERE id = $1`
	var r Run
	var boundJSON json.RawMessage
	err := s.Pool.QueryRow(ctx, q, id).Scan(&r.ID, &r.SessionID, &r.Status, &boundJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get run: %w", err)
	}
	_ = json.Unmarshal(boundJSON, &r.Bound)
	return &r, nil
}

// UpdateRunStatus 迁移 run 状态。
func (s *Store) UpdateRunStatus(ctx context.Context, id string, status sessionapi.RunStatus) error {
	const q = `
UPDATE runs SET status = $2,
  started_at = COALESCE(started_at, CASE WHEN $2 = 'running' THEN now() ELSE started_at END),
  finished_at = CASE WHEN $2 IN ('completed','failed','cancelled') THEN now() ELSE finished_at END
WHERE id = $1`
	if _, err := s.Pool.Exec(ctx, q, id, string(status)); err != nil {
		return fmt.Errorf("store: update run status: %w", err)
	}
	return nil
}

// Message 是 messages 表行（真相，只增；content 为 JSON 原样）。
type Message struct {
	ID        int64
	SessionID string
	RunID     string
	Step      int
	Role      string
	Content   json.RawMessage
}

// AppendMessage 追加消息（worker-架构设计 §3：msgs.Append 必须落表，重放后历史不缺失）。
func (s *Store) AppendMessage(ctx context.Context, sessionID, runID string, step int, role string, content json.RawMessage) error {
	const q = `
INSERT INTO messages (session_id, run_id, step, role, content)
VALUES ($1, $2, $3, $4, $5)`
	if _, err := s.Pool.Exec(ctx, q, sessionID, nullable(runID), step, role, content); err != nil {
		return fmt.Errorf("store: append message: %w", err)
	}
	return nil
}

// ListMessages 按序读最近 limit 条消息（buildMessages 的真相来源）。
func (s *Store) ListMessages(ctx context.Context, sessionID string, limit int) ([]Message, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	const q = `
SELECT id, session_id, COALESCE(run_id, ''), COALESCE(step, -1), role, content
FROM messages
WHERE session_id = $1
ORDER BY id DESC
LIMIT $2`
	rows, err := s.Pool.Query(ctx, q, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list messages: %w", err)
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.SessionID, &m.RunID, &m.Step, &m.Role, &m.Content); err != nil {
			return nil, fmt.Errorf("store: scan message: %w", err)
		}
		out = append(out, m)
	}
	// 倒序读回 → 正序返回
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

func nullableRaw(b json.RawMessage) any {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	return b
}

// GetActiveRun 取会话的活跃 run（双开 409 依据；边界语义设计 §6）。
// 活跃集 = queued/running/paused/awaiting_approval/frozen。
func (s *Store) GetActiveRun(ctx context.Context, sessionID string) (*Run, error) {
	const q = `
SELECT id, session_id, status, bound FROM runs
WHERE session_id = $1 AND status IN ('queued','running','paused','awaiting_approval','frozen')
ORDER BY created_at DESC LIMIT 1`
	var r Run
	var boundJSON json.RawMessage
	err := s.Pool.QueryRow(ctx, q, sessionID).Scan(&r.ID, &r.SessionID, &r.Status, &boundJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get active run: %w", err)
	}
	_ = json.Unmarshal(boundJSON, &r.Bound)
	return &r, nil
}
