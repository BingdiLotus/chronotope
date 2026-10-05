package store_test

// store 集成测试：需真实 Postgres（契约 schema 见 migrations/001_init.sql）。
// 本地：docker compose -f deploy/docker-compose.yml up -d postgres
//       STORE_TEST_DATABASE_URL=postgres://chronotope:chronotope_dev@localhost:5432/chronotope?sslmode=disable go test ./internal/store/
// 未设置环境变量时自动跳过（CI 的 e2e job 会设置）。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/bingdilotus/chronotope/internal/core/event"
	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
	"github.com/bingdilotus/chronotope/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	url := os.Getenv("STORE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("STORE_TEST_DATABASE_URL 未设置：跳过 store 集成测试")
	}
	s, err := store.Open(context.Background(), url)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(s.Close)
	// 每个测试独立 namespace：用随机后缀避免历史数据干扰
	return s
}

func TestAppendEventIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	key := "t_events_" + randSuffix()

	sessID := key
	if err := s.CreateOrg(ctx, "org_"+key, "org"); err != nil {
		t.Fatalf("create org: %v", err)
	}
	agentID := "agent_" + key
	if err := s.CreateAgent(ctx, agentID, "org_"+key, "agent", &sessionapi.AgentConfig{Model: "m", Version: 1}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if err := s.CreateSession(ctx, sessID, "org_"+key, agentID); err != nil {
		t.Fatalf("create session: %v", err)
	}

	payload := json.RawMessage(`{"v":1,"step":1}`)
	dedupe := event.DedupeKey("r_"+key, 0, "llm.call", "")

	seq1, err := s.AppendEvent(ctx, sessID, "r_"+key, event.LLMCall, payload, dedupe)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	// 幂等：同 dedupe_key 重复插入返回同一 seq
	seq2, err := s.AppendEvent(ctx, sessID, "r_"+key, event.LLMCall, payload, dedupe)
	if err != nil {
		t.Fatalf("append again: %v", err)
	}
	if seq1 != seq2 {
		t.Fatalf("dedupe 应返回同一 seq: %d != %d", seq1, seq2)
	}

	// 新事件 seq 递增
	seq3, err := s.AppendEvent(ctx, sessID, "r_"+key, event.RunCompleted, payload, event.DedupeKey("r_"+key, 0, "run.completed", ""))
	if err != nil {
		t.Fatalf("append third: %v", err)
	}
	if seq3 <= seq2 {
		t.Fatalf("seq 应递增: %d <= %d", seq3, seq2)
	}

	// after=seq 续读语义：dedupe 命中的第二次插入不产生新行（seq2==seq1），
	// after=seq1 应只读到 seq3 一条；after=0 读到全部唯一行。
	rows, err := s.ListEvents(ctx, sessID, seq1, 100)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(rows) != 1 || rows[0].Type != event.RunCompleted {
		t.Fatalf("after=%d 应只读到 run.completed 一条，得 %d", seq1, len(rows))
	}
	rowsAll, err := s.ListEvents(ctx, sessID, 0, 100)
	if err != nil {
		t.Fatalf("list all events: %v", err)
	}
	if len(rowsAll) != 2 {
		t.Fatalf("after=0 应读到 2 条唯一事件，得 %d", len(rowsAll))
	}
}

func TestSessionRunMessageRoundtrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	key := "t_round_" + randSuffix()

	cfg := &sessionapi.AgentConfig{
		Model:        "gpt-4o-mini",
		Instructions: "你是助手。",
		Tools:        []string{"bash"},
		Version:      1,
	}
	if err := s.CreateOrg(ctx, "org_"+key, "org"); err != nil {
		t.Fatalf("create org: %v", err)
	}
	agentID := "a_" + key
	if err := s.CreateAgent(ctx, agentID, "org_"+key, "agent", cfg); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	got, err := s.GetAgent(ctx, agentID)
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if got.Config.Model != cfg.Model || got.Version != 1 {
		t.Fatalf("agent roundtrip mismatch: %+v", got)
	}

	sessID := "s_" + key
	if err := s.CreateSession(ctx, sessID, "org_"+key, agentID); err != nil {
		t.Fatalf("create session: %v", err)
	}
	sess, err := s.GetSession(ctx, sessID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.Status != sessionapi.PhaseCreated {
		t.Fatalf("初始状态应为 created，得 %s", sess.Status)
	}
	if err := s.UpdateSessionStatus(ctx, sessID, sessionapi.PhaseReady); err != nil {
		t.Fatalf("update status: %v", err)
	}

	runID := "r_" + key
	created, err := s.CreateRun(ctx, runID, sessID, json.RawMessage(`{"kind":"user"}`), map[string]any{
		"agent_config_version": 1, "protocol_version": "1.0", "model": "gpt-4o-mini",
	})
	if err != nil || !created {
		t.Fatalf("create run: created=%v err=%v", created, err)
	}
	// 幂等：同 run_id 重复创建返回 created=false
	created2, err := s.CreateRun(ctx, runID, sessID, json.RawMessage(`{"kind":"user"}`), nil)
	if err != nil || created2 {
		t.Fatalf("create run 应幂等: created=%v err=%v", created2, err)
	}
	if err := s.UpdateRunStatus(ctx, runID, sessionapi.RunRunning); err != nil {
		t.Fatalf("update run status: %v", err)
	}

	// 消息真相层：append + 正序回读
	msg := json.RawMessage(`{"content":"hello"}`)
	if err := s.AppendMessage(ctx, sessID, runID, 0, "user", msg); err != nil {
		t.Fatalf("append message: %v", err)
	}
	msgs, err := s.ListMessages(ctx, sessID, 10)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Role != "user" {
		t.Fatalf("message roundtrip mismatch: %+v", msgs)
	}
	// jsonb 会重序列化（空格差异），按语义比较
	var gotMsg, wantMsg map[string]any
	if err := json.Unmarshal(msgs[0].Content, &gotMsg); err != nil {
		t.Fatalf("unmarshal read message: %v", err)
	}
	if err := json.Unmarshal(msg, &wantMsg); err != nil {
		t.Fatalf("unmarshal want: %v", err)
	}
	if gotMsg["content"] != wantMsg["content"] {
		t.Fatalf("message content mismatch: %v != %v", gotMsg, wantMsg)
	}
}

func TestGetMissingReturnsNotFound(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.GetAgent(ctx, "missing_"+randSuffix()); err != store.ErrNotFound {
		t.Fatalf("应返回 ErrNotFound，得 %v", err)
	}
	if _, err := s.GetSession(ctx, "missing_"+randSuffix()); err != store.ErrNotFound {
		t.Fatalf("应返回 ErrNotFound，得 %v", err)
	}
	if _, err := s.GetSandbox(ctx, "missing_"+randSuffix()); err != store.ErrNotFound {
		t.Fatalf("应返回 ErrNotFound，得 %v", err)
	}
}

func TestSandboxAndExecCacheRoundtrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	key := "t_sb_" + randSuffix()
	ttl := time.Hour

	if err := s.CreateOrg(ctx, "o_"+key, "org"); err != nil {
		t.Fatalf("create org: %v", err)
	}
	agentID := "a_" + key
	if err := s.CreateAgent(ctx, agentID, "o_"+key, "agent", &sessionapi.AgentConfig{Model: "m", Version: 1}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	sessID := "s_" + key
	if err := s.CreateSession(ctx, sessID, "o_"+key, agentID); err != nil {
		t.Fatalf("create session: %v", err)
	}

	if err := s.UpsertSandbox(ctx, &store.SandboxRow{
		SandboxID: key, OrgID: "o_" + key, SessionID: sessID, Driver: "docker",
		Image: "python:3.12-slim", Limits: map[string]string{"cpu": "1", "mem": "256m"},
		FileSyncState: "syncing", Status: "ready", TTL: &ttl,
	}); err != nil {
		t.Fatalf("upsert sandbox: %v", err)
	}
	sb, err := s.GetSandbox(ctx, key)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if sb.Image != "python:3.12-slim" || sb.Status != "ready" {
		t.Fatalf("沙箱回读不符: %+v", sb)
	}
	if sb.TTL == nil || *sb.TTL != time.Hour {
		t.Fatalf("TTL 回读不符（interval 扫描回归）: %v", sb.TTL)
	}

	// 幂等缓存（防重试双执行）：写入 → 命中 → 过期视为未执行
	result := json.RawMessage(`{"exit":0,"output":"42\n"}`)
	execKey := "r_" + key + ":0:t_1"
	if err := s.PutExec(ctx, execKey, key, result); err != nil {
		t.Fatalf("put exec: %v", err)
	}
	got, err := s.GetExec(ctx, execKey)
	if err != nil {
		t.Fatalf("get exec: %v", err)
	}
	// jsonb 重序列化（空格差异），按语义比较
	var gotRes, wantRes map[string]any
	if err := json.Unmarshal(got.Result, &gotRes); err != nil {
		t.Fatalf("unmarshal cached result: %v", err)
	}
	if err := json.Unmarshal(result, &wantRes); err != nil {
		t.Fatalf("unmarshal want: %v", err)
	}
	if gotRes["exit"] != wantRes["exit"] || gotRes["output"] != wantRes["output"] {
		t.Fatalf("exec 缓存回读不符: %v != %v", gotRes, wantRes)
	}
	if _, err := s.GetExec(ctx, "r_"+key+":0:t_9"); err != store.ErrNotFound {
		t.Fatalf("未命中应 ErrNotFound，得 %v", err)
	}

	// 状态迁移
	if err := s.UpdateSandboxStatus(ctx, key, "destroyed"); err != nil {
		t.Fatalf("update status: %v", err)
	}
	sb2, _ := s.GetSandbox(ctx, key)
	if sb2.Status != "destroyed" {
		t.Fatalf("状态迁移失败: %+v", sb2)
	}
}

func randSuffix() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
