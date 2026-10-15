package store

import (
	"context"
	"os"
	"testing"

	"github.com/bingdilotus/chronotope/internal/core/commitment"
)

func newCommitTest(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("STORE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("STORE_TEST_DATABASE_URL 未设置")
	}
	s, err := Open(context.Background(), url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s
}

func TestCommitmentLifecycle(t *testing.T) {
	s := newCommitTest(t)
	ctx := context.Background()
	agentID := "a_commit_probe_1"
	// 建 agent 行（FK）
	s.Pool.Exec(ctx, `INSERT INTO orgs (id, name) VALUES ('org-probe', 'probe-org') ON CONFLICT DO NOTHING`)
	s.Pool.Exec(ctx, `INSERT INTO agents (id, org_id, name, config, version) VALUES ($1, 'org-probe', 'probe', '{}', 1) ON CONFLICT DO NOTHING`, agentID)
	s.Pool.Exec(ctx, `INSERT INTO sessions (id, org_id, agent_id, status) VALUES ('s_commit_probe', 'org-probe', $1, 'ready') ON CONFLICT DO NOTHING`, agentID)
	s.Pool.Exec(ctx, `INSERT INTO runs (id, session_id, trigger, status, bound) VALUES ('r_probe', 's_commit_probe', '{}', 'running', '{}') ON CONFLICT DO NOTHING`, agentID)
	intent := CommitIntent{ID: "c_probe_1", AgentID: agentID, MadeInThread: "s_commit_probe", Content: "我明天给你答复", SpeechAct: "commissive"}
	row, err := s.PutCommitIntent(ctx, intent)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if row.State != commitment.StatePending {
		t.Fatalf("创建后应为 pending，实 %s", row.State)
	}
	// 幂等：同 ID 再写 → 读回同一行
	row2, err := s.PutCommitIntent(ctx, intent)
	if err != nil || row2.ID != row.ID {
		t.Fatalf("幂等回读失败: %+v err=%v", row2, err)
	}
	// 回执 pending → open
	if err := s.ConfirmCommitSent(ctx, intent.ID); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	// 非法转移拒绝
	if ok, err := s.TransitionCommit(ctx, intent.ID, commitment.StateOpen, commitment.StatePending); ok || err == nil {
		t.Fatal("open → pending 应被拒绝")
	}
	// open → expired（reconciler）
	if ok, err := s.TransitionCommit(ctx, intent.ID, commitment.StateOpen, commitment.StateExpired); err != nil || !ok {
		t.Fatalf("open → expired: ok=%v err=%v", ok, err)
	}
}
