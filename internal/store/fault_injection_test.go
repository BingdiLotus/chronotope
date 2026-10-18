package store

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestMCPDispatchClaimRace 修正方案「真实故障注入回归」之一：MCP 派发权
// CAS 的并发唯一性——N 个并发 ClaimMCPDispatch 只有一个成功（prepared →
// dispatched 原子转换——崩溃恢复不重复外部效果的存储层保证）。
func TestMCPDispatchClaimRace(t *testing.T) {
	url := os.Getenv("STORE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("STORE_TEST_DATABASE_URL 未设置")
	}
	ctx := context.Background()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	runID := fmt.Sprintf("r_mcprace_%d", time.Now().UnixNano())
	step := 0
	server, tool, callKey := "echo", "post_message", "tk1"
	if _, err := s.PutMCPCallPrepared(ctx, runID, step, server, tool, callKey, "hash1"); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// 16 个并发 claim——恰好一个成功
	var wins int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.ClaimMCPDispatch(ctx, runID, step, server, tool, callKey)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			if ok {
				atomic.AddInt32(&wins, 1)
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("并发 claim 应恰好 1 个成功，实 %d", wins)
	}
	// 已 dispatched 的行再 prepare 读回应为 dispatched（不回到 prepared）
	row, err := s.PutMCPCallPrepared(ctx, runID, step, server, tool, callKey, "hash1")
	if err != nil {
		t.Fatalf("prepare readback: %v", err)
	}
	if row.State != "dispatched" {
		t.Fatalf("派发权取得后重放应读到 dispatched（不重派发），实 %s", row.State)
	}
}

// TestSandboxGCKeepsExecEvidence 修正方案「真实故障注入回归」之二：GC 保留
// 合同（十期 #2）——DeleteSandbox 只删非 done/unknown 的 execs；done 与
// unknown 是恢复的幂等证据，跨沙箱生命周期保留（008 合同）。
func TestSandboxGCKeepsExecEvidence(t *testing.T) {
	url := os.Getenv("STORE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("STORE_TEST_DATABASE_URL 未设置")
	}
	ctx := context.Background()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	sbID := fmt.Sprintf("sb_gc_%d", time.Now().UnixNano())
	if _, err := s.Pool.Exec(ctx, `INSERT INTO orgs (id, name) VALUES ('org-gc', 'gc-probe') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO agents (id, org_id, name, config, version) VALUES ('a_gc', 'org-gc', 'gc', '{}', 1) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("insert agent: %v", err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO sessions (id, org_id, agent_id, status) VALUES ('s_gc_probe', 'org-gc', 'a_gc', 'ready') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	// 建沙箱行 + 三种状态的 execs
	if _, err := s.Pool.Exec(ctx, `INSERT INTO sandboxes (sandbox_id, org_id, session_id, driver, image, limits, tier) VALUES ($1, 'org-gc', 's_gc_probe', 'docker', 'python:3.11', '{}', 1) ON CONFLICT DO NOTHING`, sbID); err != nil {
		t.Fatalf("insert sandbox: %v", err)
	}
	insertExec := func(key, state string) {
		t.Helper()
		if _, err := s.Pool.Exec(ctx, `INSERT INTO sandbox_execs (sandbox_id, idempotency_key, input_digest, state, prepared_at, expires_at) VALUES ($1, $2, 'd', $3, now(), now() + interval '1 day')`, sbID, key, state); err != nil {
			t.Fatalf("insert exec %s/%s: %v", key, state, err)
		}
	}
	insertExec("k_done", "done")
	insertExec("k_unknown", "unknown")
	insertExec("k_prepared", "prepared")
	if err := s.DeleteSandbox(ctx, sbID); err != nil {
		t.Fatalf("delete sandbox: %v", err)
	}
	var done, unknown, prepared int
	_ = s.Pool.QueryRow(ctx, `SELECT count(*) FROM sandbox_execs WHERE sandbox_id = $1 AND state = 'done'`, sbID).Scan(&done)
	_ = s.Pool.QueryRow(ctx, `SELECT count(*) FROM sandbox_execs WHERE sandbox_id = $1 AND state = 'unknown'`, sbID).Scan(&unknown)
	_ = s.Pool.QueryRow(ctx, `SELECT count(*) FROM sandbox_execs WHERE sandbox_id = $1 AND state = 'prepared'`, sbID).Scan(&prepared)
	if done != 1 || unknown != 1 {
		t.Fatalf("done/unknown 的 execs 应保留（幂等证据），实 done=%d unknown=%d", done, unknown)
	}
	if prepared != 0 {
		t.Fatalf("prepared 的 execs 应随沙箱清理，实 %d", prepared)
	}
}
