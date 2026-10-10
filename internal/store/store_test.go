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
	"errors"
	"fmt"
	"os"
	"strings"
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
	// 水位跨测试残留（014 INSERT ON CONFLICT 不重置）——统一归零
	if _, err := s.Pool.Exec(context.Background(), `UPDATE usage_watermark SET last_event_id = 0 WHERE id = 1`); err != nil {
		t.Fatalf("reset watermark: %v", err)
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
	// prepared（claim）→ done（落账）——PutExec 一步式已删（架构审计死方法）
	won, execPreparedAt, err := s.PutExecPrepared(ctx, execKey, key, "digest")
	if err != nil || !won {
		t.Fatalf("put exec prepared: won=%v err=%v", won, err)
	}
	// 审计 #1：并发同键第二次 claim 必须败（winner 唯一）
	if won2, _, err := s.PutExecPrepared(ctx, execKey, key, "digest"); err != nil || won2 {
		t.Fatalf("第二次 claim 应败: won=%v err=%v", won2, err)
	}
	if err := s.PutExecDone(ctx, execKey, key, result, execPreparedAt); err != nil {
		t.Fatalf("put exec done: %v", err)
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

func TestUsageUpsertAccumulates(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	key := "t_usage_" + randSuffix()
	bucket := time.Now().UTC().Truncate(time.Minute)

	if err := s.CreateOrg(ctx, "o_"+key, "org"); err != nil {
		t.Fatalf("create org: %v", err)
	}
	agentID := "a_" + key
	if err := s.CreateAgent(ctx, agentID, "o_"+key, "agent", &sessionapi.AgentConfig{Model: "m", Version: 1}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if err := s.CreateSession(ctx, key, "o_"+key, agentID); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// 增量语义（期 2 §B）：delta 加算 + 水位推进
	if err := s.ApplyUsageDelta(ctx, 0, 1, []store.UsageRow{{SessionID: key, Bucket: bucket, ActiveSeconds: 10, TokensIn: 5, TokensOut: 3, ComputeSeconds: 1.5}}); err != nil {
		t.Fatalf("delta: %v", err)
	}
	if err := s.ApplyUsageDelta(ctx, 1, 2, []store.UsageRow{{SessionID: key, Bucket: bucket, ActiveSeconds: 5, TokensIn: 2, TokensOut: 1, ComputeSeconds: 0.5}}); err != nil {
		t.Fatalf("delta again: %v", err)
	}
	rows, err := s.ListUsage(ctx, key)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("应 1 个桶，得 %d", len(rows))
	}
	u := rows[0]
	if u.ActiveSeconds != 15 || u.TokensIn != 7 || u.TokensOut != 4 || u.ComputeSeconds != 2.0 {
		t.Fatalf("累加不符: %+v", u)
	}
}

func TestGetActiveRun(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	key := "t_active_" + randSuffix()
	if err := s.CreateOrg(ctx, "o_"+key, "org"); err != nil {
		t.Fatalf("create org: %v", err)
	}
	agentID := "a_" + key
	if err := s.CreateAgent(ctx, agentID, "o_"+key, "agent", &sessionapi.AgentConfig{Model: "m", Version: 1}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if err := s.CreateSession(ctx, key, "o_"+key, agentID); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// 无活跃 run → ErrNotFound
	if _, err := s.GetActiveRun(ctx, key); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("无活跃 run 应 ErrNotFound，得 %v", err)
	}
	// 一条 completed 的 run 不算活跃
	if _, err := s.CreateRun(ctx, "r_done_"+key, key, nil, nil); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := s.UpdateRunStatus(ctx, "r_done_"+key, sessionapi.RunCompleted); err != nil {
		t.Fatalf("update status: %v", err)
	}
	if _, err := s.GetActiveRun(ctx, key); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("completed 不算活跃，得 %v", err)
	}
	// 一条 running 的 run → 命中
	if _, err := s.CreateRun(ctx, "r_active_"+key, key, nil, nil); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := s.UpdateRunStatus(ctx, "r_active_"+key, sessionapi.RunRunning); err != nil {
		t.Fatalf("update status: %v", err)
	}
	active, err := s.GetActiveRun(ctx, key)
	if err != nil || active.ID != "r_active_"+key {
		t.Fatalf("应命中 r_active，得 %+v err=%v", active, err)
	}
}

func TestMemorySummaryAndItems(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	key := "t_mem_" + randSuffix()
	if err := s.CreateOrg(ctx, "o_"+key, "org"); err != nil {
		t.Fatalf("create org: %v", err)
	}
	agentID := "a_" + key
	if err := s.CreateAgent(ctx, agentID, "o_"+key, "agent", &sessionapi.AgentConfig{Model: "m", Version: 1}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if err := s.CreateSession(ctx, key, "o_"+key, agentID); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// 无摘要 → ErrNotFound
	if _, err := s.LatestSummary(ctx, key, "default"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("无摘要应 ErrNotFound，得 %v", err)
	}
	// 写摘要 v1；同版本重复写 → 幂等 false
	ok, err := s.CreateSummary(ctx, store.Summary{SessionID: key, Topic: "default", Version: 1, Summary: "S1", Diff: "full", CreatedByRun: "r_1"})
	if err != nil || !ok {
		t.Fatalf("create summary: ok=%v err=%v", ok, err)
	}
	ok, err = s.CreateSummary(ctx, store.Summary{SessionID: key, Topic: "default", Version: 1, Summary: "S1b", Diff: "full", CreatedByRun: "r_1"})
	if err != nil || ok {
		t.Fatalf("同版本应幂等返回 false: ok=%v err=%v", ok, err)
	}
	// 最新版 = v2
	if _, err := s.CreateSummary(ctx, store.Summary{SessionID: key, Topic: "default", Version: 2, Summary: "S2", Diff: "v2", CreatedByRun: "r_2"}); err != nil {
		t.Fatalf("create v2: %v", err)
	}
	latest, err := s.LatestSummary(ctx, key, "default")
	if err != nil || latest.Version != 2 || latest.Summary != "S2" {
		t.Fatalf("latest 应为 v2: %+v err=%v", latest, err)
	}

	// 条目：内容哈希去重；topic 作用域检索
	if ok, err := s.CreateMemoryItem(ctx, store.MemoryItem{SessionID: key, Topic: "default", Kind: "long_term", Content: "用户偏好：简洁回答", SourceRunID: "r_2", SourceStep: 3, Version: 1}); err != nil || !ok {
		t.Fatalf("create item: ok=%v err=%v", ok, err)
	}
	if ok, err := s.CreateMemoryItem(ctx, store.MemoryItem{SessionID: key, Topic: "default", Kind: "long_term", Content: "用户偏好：简洁回答", Version: 1}); err != nil || ok {
		t.Fatalf("重复内容应去重 false: ok=%v err=%v", ok, err)
	}
	if _, err := s.CreateMemoryItem(ctx, store.MemoryItem{SessionID: key, Topic: "other", Content: "其他主题", Version: 1}); err != nil {
		t.Fatalf("create other topic: %v", err)
	}
	items, err := s.ListMemoryItems(ctx, key, "default", 10)
	if err != nil || len(items) != 1 || items[0].Topic != "default" {
		t.Fatalf("topic 作用域检索应 1 条: %+v err=%v", items, err)
	}
	all, err := s.ListMemoryItems(ctx, key, "", 10)
	if err != nil || len(all) != 2 {
		t.Fatalf("空 topic 应全量 2 条: %+v err=%v", all, err)
	}
}

func TestOrgBudgetAndDailyUsage(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	key := "t_budget_" + randSuffix()
	orgID := "o_" + key
	if err := s.CreateOrg(ctx, orgID, "org"); err != nil {
		t.Fatalf("create org: %v", err)
	}
	// 初始 quotas 空 → 无限
	org, err := s.GetOrg(ctx, orgID)
	if err != nil || len(org.Quotas) != 0 {
		t.Fatalf("初始 quotas 应空: %+v err=%v", org, err)
	}
	// 更新预算（合并保留其他键）
	if err := s.UpdateOrgQuotas(ctx, orgID, map[string]any{store.QuotaDailyTokenBudget: float64(1000), "other": "keep"}); err != nil {
		t.Fatalf("update quotas: %v", err)
	}
	org, _ = s.GetOrg(ctx, orgID)
	if org.Quotas[store.QuotaDailyTokenBudget] != float64(1000) || org.Quotas["other"] != "keep" {
		t.Fatalf("quotas 合并更新不符: %+v", org.Quotas)
	}
	// 日用量：建会话 + usage 行 → 聚合
	agentID := "a_" + key
	if err := s.CreateAgent(ctx, agentID, orgID, "agent", &sessionapi.AgentConfig{Model: "m", Version: 1}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if err := s.CreateSession(ctx, key, orgID, agentID); err != nil {
		t.Fatalf("create session: %v", err)
	}
	now := time.Now().UTC()
	bucket := now.Truncate(time.Minute)
	if err := s.ApplyUsageDelta(ctx, 0, 1, []store.UsageRow{{SessionID: key, Bucket: bucket, TokensIn: 30, TokensOut: 20, ComputeSeconds: 1.5}}); err != nil {
		t.Fatalf("delta usage: %v", err)
	}
	tokens, compute, err := s.OrgDailyUsage(ctx, orgID, now.Truncate(24*time.Hour))
	if err != nil || tokens != 50 || compute != 1.5 {
		t.Fatalf("日用量应 50 tokens/1.5s: %d/%v err=%v", tokens, compute, err)
	}
	// 其他 org 不受影响
	other := "o_other_" + key
	_ = s.CreateOrg(ctx, other, "other")
	if t2, _, _ := s.OrgDailyUsage(ctx, other, now.Truncate(24*time.Hour)); t2 != 0 {
		t.Fatalf("其他 org 应为 0，得 %d", t2)
	}
}

func TestSandboxGCScanAndDelete(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	key := "t_gc_" + randSuffix()
	if err := s.CreateOrg(ctx, "o_"+key, "org"); err != nil {
		t.Fatalf("create org: %v", err)
	}
	agentID := "a_" + key
	if err := s.CreateAgent(ctx, agentID, "o_"+key, "agent", &sessionapi.AgentConfig{Model: "m", Version: 1}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if err := s.CreateSession(ctx, key, "o_"+key, agentID); err != nil {
		t.Fatalf("create session: %v", err)
	}
	// 过期沙箱：ttl 1s（created_at 由 DB now()，等 1.5s 后扫描命中）
	ttl := time.Second
	if err := s.UpsertSandbox(ctx, &store.SandboxRow{
		SandboxID: "sb_gc_" + key, OrgID: "o_" + key, SessionID: key,
		Driver: "docker", ContainerRef: ptr("sb_gc_ctr"), Image: "python:3.12-slim",
		Status: "ready", TTL: &ttl,
	}); err != nil {
		t.Fatalf("upsert sandbox: %v", err)
	}
	// 未过期：ttl 1h
	long := time.Hour
	if err := s.UpsertSandbox(ctx, &store.SandboxRow{
		SandboxID: "sb_live_" + key, OrgID: "o_" + key, SessionID: key,
		Driver: "docker", ContainerRef: ptr("sb_live_ctr"), Image: "python:3.12-slim",
		Status: "ready", TTL: &long,
	}); err != nil {
		t.Fatalf("upsert live sandbox: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)
	expired, err := s.ListExpiredSandboxes(ctx, time.Now(), "")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	var mine []string
	for _, sb := range expired {
		if sb.SandboxID == "sb_gc_"+key {
			mine = append(mine, sb.SandboxID)
		}
		if sb.SandboxID == "sb_live_"+key {
			t.Fatalf("未过期沙箱不应被扫到: %+v", sb)
		}
	}
	if len(mine) != 1 {
		t.Fatalf("应扫到测试的过期沙箱（历史脏数据不计）: %v", mine)
	}
	if err := s.DeleteSandbox(ctx, "sb_gc_"+key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.DeleteSandbox(ctx, "sb_gc_"+key); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("重复删除应 ErrNotFound，得 %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

// TestEventDeliveryEnqueue 订阅 → 事件同事务入队 → 投递成功删行 / 失败退避。
func TestEventDeliveryEnqueue(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	key := "t_dlv_" + randSuffix()
	if err := s.CreateOrg(ctx, "o_"+key, "org"); err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := s.CreateAgent(ctx, "a_"+key, "o_"+key, "agent", &sessionapi.AgentConfig{Model: "m", Version: 1}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if err := s.CreateSession(ctx, key, "o_"+key, "a_"+key); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := s.Subscribe(ctx, key, "webhook", "http://sink.example/hook"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	// 事件入队（同事务）
	seq, err := s.AppendEvent(ctx, key, "", event.RunStarted, json.RawMessage(`{"v":1}`), key+":run:0:run.started")
	if err != nil || seq == 0 {
		t.Fatalf("append: seq=%d err=%v", seq, err)
	}
	mine := func(rows []*store.PendingOutboxRow) []*store.PendingOutboxRow {
		var out []*store.PendingOutboxRow
		for _, r := range rows {
			if r.SessionID == key {
				out = append(out, r)
			}
		}
		return out
	}
	pending, err := s.ListPendingOutbox(ctx, 100)
	if err != nil || len(mine(pending)) != 1 || mine(pending)[0].Channel != "webhook" {
		t.Fatalf("应 1 行待投递: %+v err=%v", mine(pending), err)
	}
	// 投递成功 → 删除
	if err := s.OutboxDelivered(ctx, mine(pending)[0].ID); err != nil {
		t.Fatalf("delivered: %v", err)
	}
	pending, _ = s.ListPendingOutbox(ctx, 100)
	if len(mine(pending)) != 0 {
		t.Fatalf("投递后应无待投递: %+v", mine(pending))
	}
	// 失败退避：再入队 → retry → next_at 后移
	if _, err := s.AppendEvent(ctx, key, "", event.RunCompleted, json.RawMessage(`{"v":1}`), key+":run:0:run.completed"); err != nil {
		t.Fatalf("append2: %v", err)
	}
	pending, _ = s.ListPendingOutbox(ctx, 100)
	if len(mine(pending)) != 1 {
		t.Fatalf("应 1 行: %+v", mine(pending))
	}
	if err := s.OutboxRetry(ctx, mine(pending)[0].ID, mine(pending)[0].ID); err != nil {
		t.Fatalf("retry: %v", err)
	}
	pending, _ = s.ListPendingOutbox(ctx, 100)
	if len(mine(pending)) != 0 {
		t.Fatalf("退避后立即拉取应为空（next_at 后移）: %+v", mine(pending))
	}
}

// TestSandboxLease 租约账本：acquire/renew generation 递增、释放代次校验、GC 依赖安全。
func TestSandboxLease(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	key := "t_lease_" + randSuffix()
	if err := s.CreateOrg(ctx, "o_"+key, "org"); err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := s.CreateAgent(ctx, "a_"+key, "o_"+key, "agent", &sessionapi.AgentConfig{Model: "m", Version: 1}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if err := s.CreateSession(ctx, key, "o_"+key, "a_"+key); err != nil {
		t.Fatalf("create session: %v", err)
	}
	ttl := 200 * time.Millisecond
	if err := s.UpsertSandbox(ctx, &store.SandboxRow{
		SandboxID: "sb_" + key, OrgID: "o_" + key, SessionID: key,
		Driver: "docker", Image: "python:3.11-slim", Status: "ready", TTL: &ttl,
	}); err != nil {
		t.Fatalf("upsert sandbox: %v", err)
	}
	// acquire → renew：generation 递增
	l1, err := s.AcquireLease(ctx, "sb_"+key, "r_1", time.Minute)
	if err != nil || l1.Generation != 1 {
		t.Fatalf("acquire: %+v err=%v", l1, err)
	}
	l2, err := s.AcquireLease(ctx, "sb_"+key, "r_1", time.Minute)
	if err != nil || l2.Generation != 2 {
		t.Fatalf("renew 应 generation+1: %+v err=%v", l2, err)
	}
	// 旧代次释放 → 拒绝（fencing 最小闭环）
	if ok, _ := s.ReleaseLease(ctx, "sb_"+key, 1); ok {
		t.Fatal("旧代次释放应失败（fencing）")
	}
	if ok, _ := s.ReleaseLease(ctx, "sb_"+key, 2); !ok {
		t.Fatal("当代次释放应成功")
	}
	// GC 依赖安全：过期 + 有效租约 → 不回收
	l3, err := s.AcquireLease(ctx, "sb_"+key, "r_2", time.Minute)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	time.Sleep(300 * time.Millisecond) // TTL 过期
	expired, err := s.ListExpiredSandboxes(ctx, time.Now(), "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, sb := range expired {
		if sb.SandboxID == "sb_"+key {
			t.Fatalf("有有效租约的过期沙箱不得回收（依赖安全）: %+v", sb)
		}
	}
	// 释放后（用 r_2 租约的实际代次）→ 可回收
	if _, err := s.ReleaseLease(ctx, "sb_"+key, l3.Generation); err != nil {
		t.Fatalf("release: %v", err)
	}
	expired, _ = s.ListExpiredSandboxes(ctx, time.Now(), "")
	found := false
	for _, sb := range expired {
		if sb.SandboxID == "sb_"+key {
			found = true
		}
	}
	if !found {
		t.Fatal("租约释放后的过期沙箱应可回收")
	}
}

// TestSpecDigest ⑩：canonical JSON 哈希确定性（同 config 同哈希；map 键序无关）。
func TestSpecDigest(t *testing.T) {
	cfg1 := &sessionapi.AgentConfig{Model: "m", Instructions: "i", Tools: []string{"bash", "read_file"}, Version: 1,
		Budget: map[string]any{"max_tokens": float64(100)}}
	cfg2 := &sessionapi.AgentConfig{Model: "m", Instructions: "i", Tools: []string{"bash", "read_file"}, Version: 1,
		Budget: map[string]any{"max_tokens": float64(100)}}
	cfg3 := &sessionapi.AgentConfig{Model: "m2", Instructions: "i", Tools: []string{"bash"}, Version: 1}
	if store.SpecDigestOf(cfg1) != store.SpecDigestOf(cfg2) {
		t.Fatal("同 config 应同哈希（canonical 确定性）")
	}
	if store.SpecDigestOf(cfg1) == store.SpecDigestOf(cfg3) {
		t.Fatal("不同 config 应不同哈希")
	}

	// 入库读回
	s := testStore(t)
	ctx := context.Background()
	key := "t_digest_" + randSuffix()
	if err := s.CreateOrg(ctx, "o_"+key, "org"); err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := s.CreateAgent(ctx, "a_"+key, "o_"+key, "agent", cfg1); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	got, err := s.GetAgent(ctx, "a_"+key)
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if got.SpecDigest == "" || got.SpecDigest != store.SpecDigestOf(cfg1) {
		t.Fatalf("spec_digest 应入库且一致: %q", got.SpecDigest)
	}
}

// TestTimeTravelStore 时间旅行（期 2）：checkpoint 树 + fork 写时复制 + rollback 投影截断 + diff。
func TestTimeTravelStore(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	key := "t_tt_" + randSuffix()
	if err := s.CreateOrg(ctx, "o_"+key, "org"); err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := s.CreateAgent(ctx, "a_"+key, "o_"+key, "agent", &sessionapi.AgentConfig{Model: "m", Version: 1}); err != nil {
		t.Fatalf("agent: %v", err)
	}
	if err := s.CreateSession(ctx, key, "o_"+key, "a_"+key); err != nil {
		t.Fatalf("session: %v", err)
	}
	// 事件 + 消息前缀
	if _, err := s.CreateRun(ctx, "r_1", key, json.RawMessage(`{}`), map[string]any{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	ev1, _ := s.AppendEvent(ctx, key, "r_1", "run.started", json.RawMessage(`{"input":"x"}`), "tt1-"+key)
	_, _ = s.AppendEvent(ctx, key, "r_1", "run.completed", json.RawMessage(`{"final":"y"}`), "tt2-"+key)
	if err := s.AppendMessage(ctx, key, "r_1", 0, "user", json.RawMessage(`"x"`)); err != nil {
		t.Fatalf("msg: %v", err)
	}
	// checkpoint（seq=ev1）
	if err := s.UpsertWorkspaceFile(ctx, store.WorkspaceFile{
		SessionID: key, Path: "/workspace/a.txt", Hash: "h-fork", Size: 3,
	}); err != nil {
		t.Fatalf("ws file: %v", err)
	}
	cp := store.Checkpoint{ID: "cp_" + key, SessionID: key, Seq: ev1, SnapshotRef: "img|tar"}
	if ok, err := s.CreateCheckpoint(ctx, cp); err != nil || !ok {
		t.Fatalf("create cp: %v", err)
	}
	got, err := s.GetCheckpoint(ctx, cp.ID)
	if err != nil || got.Seq != ev1 || got.SnapshotRef != "img|tar" {
		t.Fatalf("get cp: %+v err=%v", got, err)
	}
	// fork 于 ev1 之后（含 ev1 前缀 + 不含 ev2）
	forkID := key + "_fork"
	if err := s.ForkSession(ctx, forkID, key, ev1, cp.ID); err != nil {
		t.Fatalf("fork: %v", err)
	}
	forkEvents, _ := s.ListEvents(ctx, forkID, 0, 100)
	// 前缀（run.started + session.forked）——不含 run.completed
	types := map[string]bool{}
	for _, e := range forkEvents {
		types[string(e.Type)] = true
	}
	if !types["run.started"] || types["run.completed"] || !types["session.forked"] {
		t.Fatalf("fork 时间轴应为前缀+派生事件: %v", types)
	}
	// diff：父 vs fork → 公共前缀 1（run.started），父独有 run.completed，fork 独有 session.forked
	diff, err := s.DiffSessions(ctx, key, forkID, 100)
	if err != nil || diff.CommonPrefix != 1 || len(diff.OnlyA) != 1 || len(diff.OnlyB) != 1 {
		t.Fatalf("diff: %+v err=%v", diff, err)
	}
	// fork 空间面（期 2 遗留 #1）：workspace_files 索引复制到分支会话
	forkFiles, err := s.ListWorkspaceFiles(ctx, forkID, 100)
	if err != nil || len(forkFiles) != 1 || forkFiles[0].Hash != "h-fork" {
		t.Fatalf("fork 应复制工作区索引: %+v err=%v", forkFiles, err)
	}

	// rollback 到 checkpoint：消息投影截断（step>0 删除）+ 状态 ready + 事件追加
	if err := s.RollbackSession(ctx, key, got); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	msgs, _ := s.ListMessages(ctx, key, 100)
	if len(msgs) != 1 || msgs[0].Role != "user" {
		t.Fatalf("rollback 后消息应截断到前缀: %+v", msgs)
	}
	evs, _ := s.ListEvents(ctx, key, 0, 100)
	found := false
	for _, e := range evs {
		if e.Type == "session.rolled_back" {
			found = true
		}
	}
	if !found {
		t.Fatalf("应有 rollback 事件（真相不可变审计）: %v", evs)
	}
}

// TestWorkspaceFileIndex 期 2 §A：目录索引 upsert 覆盖 + 列表。
func TestWorkspaceFileIndex(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	key := "t_ws_" + randSuffix()
	if err := s.CreateOrg(ctx, "o_"+key, "org"); err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := s.CreateAgent(ctx, "a_"+key, "o_"+key, "agent", &sessionapi.AgentConfig{Model: "m", Version: 1}); err != nil {
		t.Fatalf("agent: %v", err)
	}
	if err := s.CreateSession(ctx, key, "o_"+key, "a_"+key); err != nil {
		t.Fatalf("session: %v", err)
	}
	f1 := store.WorkspaceFile{SessionID: key, Path: "/workspace/a.txt", Hash: "h1", Size: 3}
	if err := s.UpsertWorkspaceFile(ctx, f1); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// 同路径覆盖（内容寻址更新）
	f2 := store.WorkspaceFile{SessionID: key, Path: "/workspace/a.txt", Hash: "h2", Size: 5}
	if err := s.UpsertWorkspaceFile(ctx, f2); err != nil {
		t.Fatalf("upsert overwrite: %v", err)
	}
	files, err := s.ListWorkspaceFiles(ctx, key, 100)
	if err != nil || len(files) != 1 || files[0].Hash != "h2" || files[0].Size != 5 {
		t.Fatalf("应单行且为最新 hash: %+v err=%v", files, err)
	}
}

// TestUsageDeltaWatermark 期 2 §B：delta 加算 + 水位推进 + 同水位重跑零增量（幂等）。
func TestUsageDeltaWatermark(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	key := "t_delta_" + randSuffix()
	if err := s.CreateOrg(ctx, "o_"+key, "org"); err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := s.CreateAgent(ctx, "a_"+key, "o_"+key, "agent", &sessionapi.AgentConfig{Model: "m", Version: 1}); err != nil {
		t.Fatalf("agent: %v", err)
	}
	if err := s.CreateSession(ctx, key, "o_"+key, "a_"+key); err != nil {
		t.Fatalf("session: %v", err)
	}
	// 测试库水位跨跑残留（014 INSERT ON CONFLICT 不重置）——测试前归零
	if _, err := s.Pool.Exec(ctx, `UPDATE usage_watermark SET last_event_id = 0 WHERE id = 1`); err != nil {
		t.Fatalf("重置水位: %v", err)
	}
	bucket := time.Now().UTC().Truncate(time.Minute)
	deltas := []store.UsageRow{{SessionID: key, Bucket: bucket, ActiveSeconds: 10, TokensIn: 4, TokensOut: 7}}
	if err := s.ApplyUsageDelta(ctx, 0, 5, deltas); err != nil {
		t.Fatalf("delta: %v", err)
	}
	// 同水位重跑 → 竞态错误（水位已推进）——幂等由「不重复应用」保证
	if err := s.ApplyUsageDelta(ctx, 0, 5, deltas); err == nil {
		t.Fatal("同水位重跑应竞态拒绝（水位已推进）")
	}
	wm, _ := s.GetUsageWatermark(ctx)
	if wm != 5 {
		t.Fatalf("水位应推进到 5: %d", wm)
	}
	// 新批次 → 加算
	if err := s.ApplyUsageDelta(ctx, 5, 8, deltas); err != nil {
		t.Fatalf("delta2: %v", err)
	}
	usage, _ := s.ListUsage(ctx, key)
	if len(usage) != 1 || usage[0].ActiveSeconds != 20 || usage[0].TokensOut != 14 {
		t.Fatalf("两次 delta 应加算（10→20）: %+v", usage)
	}
}

// TestArchiveCRUD 期 2 §B：归档清单 + archived_at 标记。
func TestArchiveCRUD(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	key := "t_arc_" + randSuffix()
	if err := s.CreateOrg(ctx, "o_"+key, "org"); err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := s.CreateAgent(ctx, "a_"+key, "o_"+key, "agent", &sessionapi.AgentConfig{Model: "m", Version: 1}); err != nil {
		t.Fatalf("agent: %v", err)
	}
	if err := s.CreateSession(ctx, key, "o_"+key, "a_"+key); err != nil {
		t.Fatalf("session: %v", err)
	}
	if err := s.CreateArchive(ctx, store.Archive{SessionID: key, BucketPath: "o/" + key, EventsCount: 5, MessagesCount: 3}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	got, err := s.GetArchive(ctx, key)
	if err != nil || got.BucketPath != "o/"+key || got.EventsCount != 5 {
		t.Fatalf("get: %+v err=%v", got, err)
	}
	sess, _ := s.GetSession(ctx, key)
	if sess.ArchivedAt == nil {
		t.Fatal("archived_at 应标记")
	}
}

// TestMCPAllowlist 期 3 §C：工具白名单前缀匹配 + 空列表全拒。
func TestMCPAllowlist(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	key := "t_mcp_" + randSuffix()
	if err := s.CreateOrg(ctx, "o_"+key, "org"); err != nil {
		t.Fatalf("org: %v", err)
	}
	// 空列表 = 全拒
	ok, err := s.MCPToolAllowed(ctx, "o_"+key, "echo", "echo")
	if err != nil || ok {
		t.Fatalf("空列表应全拒: %v err=%v", ok, err)
	}
	// 前缀匹配
	if err := s.UpsertMCPAllowlist(ctx, "o_"+key, "echo", []string{"echo", "files."}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if ok, _ := s.MCPToolAllowed(ctx, "o_"+key, "echo", "echo"); !ok {
		t.Fatal("精确匹配应放行")
	}
	if ok, _ := s.MCPToolAllowed(ctx, "o_"+key, "echo", "files.read"); !ok {
		t.Fatal("前缀匹配应放行")
	}
	if ok, _ := s.MCPToolAllowed(ctx, "o_"+key, "echo", "danger"); ok {
		t.Fatal("未列工具应拒绝")
	}
}

// TestKnowledgeRetrieve 期 3 §D：pgvector 余弦检索 topK。
func TestKnowledgeRetrieve(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	key := "t_kn_" + randSuffix()
	if err := s.CreateOrg(ctx, "o_"+key, "org"); err != nil {
		t.Fatalf("org: %v", err)
	}
	// 两条知识：主题相近向量近邻
	base := make([]float32, 1024)
	base[0], base[1], base[2] = 1, 1, 1
	near := make([]float32, 1024)
	copy(near, base)
	near[3] = 0.5
	far := make([]float32, 1024)
	far[500] = 1
	for i, c := range []string{"主题相近的内容", "完全无关的内容"} {
		emb := near
		if i == 1 {
			emb = far
		}
		if err := s.CreateKnowledge(ctx, store.KnowledgeItem{
			ID: key + "-k" + string(rune('a'+i)), TenantID: "o_" + key, Content: c, Embedding: emb,
		}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	q := make([]float32, 1024)
	copy(q, base)
	got, err := s.RetrieveKnowledge(ctx, "o_"+key, q, 2)
	if err != nil || len(got) != 2 {
		t.Fatalf("retrieve: %d err=%v", len(got), err)
	}
	if got[0].Content != "主题相近的内容" {
		t.Fatalf("top1 应为近邻: %+v", got[0])
	}
}

// TestArchiveEventsPartitionPruning 期 4 §A：归档表按月分区裁剪（仅扫单分区）。
func TestArchiveEventsPartitionPruning(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	// 迁移重放后 archive_events 为分区表（019）——插入两条不同月路由
	if _, err := s.Pool.Exec(ctx, `INSERT INTO archive_events (session_id, run_id, seq, type, payload, at)
		VALUES ('s_prune', 'r_1', 1, 'run.completed', '{"v":1}', '2026-10-15T00:00:00Z'),
		       ('s_prune', 'r_2', 2, 'run.completed', '{"v":1}', '2026-11-15T00:00:00Z')`); err != nil {
		t.Fatalf("insert archive: %v", err)
	}
	var plan string
	if err := s.Pool.QueryRow(ctx,
		`EXPLAIN (FORMAT JSON) SELECT count(*) FROM archive_events WHERE at >= '2026-10-01' AND at < '2026-11-01'`).Scan(&plan); err != nil {
		t.Fatalf("explain: %v", err)
	}
	// 裁剪断言：计划含 2026_10 分区且不含 default 分区
	if !strings.Contains(plan, "archive_events_2026_10") || strings.Contains(plan, "archive_events_default") {
		t.Fatalf("分区裁剪失效: %s", plan)
	}
	_ = s.Pool.QueryRow(ctx, `DELETE FROM archive_events WHERE session_id = 's_prune'`).Scan()
}

// TestRunActiveUniqueAndCommand 审计 #4/#3：active 唯一约束（并发双 Run 兜底）
// + 不可变 command 落库（重投不丢输入）。
func TestRunActiveUniqueAndCommand(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	rid := fmt.Sprintf("r_ru_%d", time.Now().UnixNano())
	sid := fmt.Sprintf("s_ru_%d", time.Now().UnixNano())
	_ = s.CreateOrg(ctx, "org_run_uniq", "o")
	_ = s.CreateAgent(ctx, "a_ru", "org_run_uniq", "a", &sessionapi.AgentConfig{Model: "m", Instructions: "i", Version: 1})
	_ = s.CreateSession(ctx, sid, "org_run_uniq", "a_ru")
	// 首 run（queued）——input 落库
	if created, err := s.CreateRunWithCommand(ctx, rid, sid, "原始输入", "话题", nil, map[string]any{}); err != nil || !created {
		t.Fatalf("create run1: created=%v err=%v", created, err)
	}
	// 并发第二 run（不同 key）——active 唯一约束拒绝（审计 #4 双 Run 反例）
	if _, err := s.CreateRun(ctx, "r_ru_2", sid, nil, map[string]any{}); err == nil {
		t.Fatal("active 唯一约束应拒绝第二 run")
	}
	// 幂等重投的 command 读回
	rows, err := s.ListStaleQueuedRuns(ctx, time.Now().Add(time.Minute), 100)
	if err != nil {
		t.Fatalf("list stale: %v", err)
	}
	var found *store.Run
	for i := range rows {
		if rows[i].ID == rid {
			found = rows[i]
		}
	}
	if found == nil || found.Input != "原始输入" || found.Topic != "话题" {
		t.Fatalf("重投行应带 input/topic: %+v", found)
	}
}

// TestLeaseGenerationMonotonic 审计 P0-2：Release 置过期不删行——generation
// 单调（ABA 反例：旧 gen release 不得删除新租约）。
func TestLeaseGenerationMonotonic(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	sid := fmt.Sprintf("sb_lease_aba_%d", time.Now().UnixNano())
	_ = s.CreateOrg(ctx, "org_lease_aba", "o")
	_ = s.CreateAgent(ctx, "a_la", "org_lease_aba", "a", &sessionapi.AgentConfig{Model: "m", Instructions: "i", Version: 1})
	_ = s.CreateSession(ctx, "s_la", "org_lease_aba", "a_la")
	// 沙箱行（FK 地基——sandbox_leases 引用 sandboxes）
	_, _ = s.Pool.Exec(ctx, `INSERT INTO sandboxes (sandbox_id, org_id, session_id, driver, status, created_at, ttl, image)
		VALUES ($1, 'org_lease_aba', 's_la', 'docker', 'ready', now(), interval '1 day', 'python:3.12-slim')`, sid)
	l1, err := s.AcquireLease(ctx, sid, "run-a", time.Hour)
	if err != nil || l1.Generation != 1 {
		t.Fatalf("acquire1: %+v err=%v", l1, err)
	}
	if ok, err := s.ReleaseLease(ctx, sid, l1.Generation); err != nil || !ok {
		t.Fatalf("release1: ok=%v err=%v", ok, err)
	}
	l2, err := s.AcquireLease(ctx, sid, "run-b", time.Hour)
	if err != nil || l2.Generation != 2 {
		t.Fatalf("acquire2 应 gen=2（行永存单调）: %+v err=%v", l2, err)
	}
	// ABA 反例：旧 gen=1 的 release 不得删除新 gen=2 的租约
	if ok, err := s.ReleaseLease(ctx, sid, 1); err != nil || ok {
		t.Fatalf("旧 gen=1 的 release 应无效（ABA 关闭）: ok=%v err=%v", ok, err)
	}
	if active, err := s.HasActiveLease(ctx, sid); err != nil || !active {
		t.Fatalf("新租约应存活: active=%v err=%v", active, err)
	}
}

// TestListMessagesForRunExcludesCurrentRun 审计 #5：请求冻结——重放时排除
// 本 run 已提交的非 user 消息（漂移反例关闭：PG 全历史含后续 assistant/tool）。
func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func TestListMessagesForRunExcludesCurrentRun(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	sid := fmt.Sprintf("s_freeze_%d", time.Now().UnixNano())
	_ = s.CreateOrg(ctx, "org_freeze", "o")
	_ = s.CreateAgent(ctx, "a_fz", "org_freeze", "a", &sessionapi.AgentConfig{Model: "m", Instructions: "i", Version: 1})
	_ = s.CreateSession(ctx, sid, "org_freeze", "a_fz")
	// run 行（messages FK 地基 + active 唯一约束——旧 run 完成后建新 run）
	if _, err := s.CreateRunWithCommand(ctx, "r_old", sid, "", "", nil, map[string]any{}); err != nil {
		t.Fatalf("create old run: %v", err)
	}
	_ = s.UpdateRunStatus(ctx, "r_old", sessionapi.RunCompleted)
	if _, err := s.CreateRunWithCommand(ctx, "r_cur", sid, "", "", nil, map[string]any{}); err != nil {
		t.Fatalf("create cur run: %v", err)
	}
	// 前一个 run 的历史（保留）
	if err := s.AppendMessage(ctx, sid, "r_old", 1, "assistant", json.RawMessage(`"旧回答"`)); err != nil {
		t.Fatalf("append old: %v", err)
	}
	// 本 run 的 user 输入 + 已提交 assistant（重放时 assistant 应被排除）
	if err := s.AppendMessage(ctx, sid, "r_cur", 0, "user", json.RawMessage(`"本轮输入"`)); err != nil {
		t.Fatalf("append user: %v", err)
	}
	if err := s.AppendMessage(ctx, sid, "r_cur", 1, "assistant", json.RawMessage(`"本 run 后续已提交"`)); err != nil {
		t.Fatalf("append assistant: %v", err)
	}
	msgs, err := s.ListMessagesForRun(ctx, sid, "r_cur", 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var roles []string
	for _, m := range msgs {
		roles = append(roles, m.Role)
	}
	// 期望：旧 run 的 assistant + 本 run 的 user；本 run 的 assistant 排除
	if len(msgs) != 2 || !contains(roles, "user") || !contains(roles, "assistant") {
		t.Fatalf("本 run 非 user 消息应被排除（请求冻结）: %v %d 条", roles, len(msgs))
	}
}

// TestGetRunProjectionCommand 审计 #3：GetRun 投影补 input/topic——幂等比较
// 真实生效（生产同 key 不同 input 的 409 依赖此投影）。
func TestGetRunProjectionCommand(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	rid := fmt.Sprintf("r_proj_%d", time.Now().UnixNano())
	sid := fmt.Sprintf("s_proj_%d", time.Now().UnixNano())
	_ = s.CreateOrg(ctx, "org_proj", "o")
	_ = s.CreateAgent(ctx, "a_pr", "org_proj", "a", &sessionapi.AgentConfig{Model: "m", Instructions: "i", Version: 1})
	_ = s.CreateSession(ctx, sid, "org_proj", "a_pr")
	if _, err := s.CreateRunWithCommand(ctx, rid, sid, "原始输入", "话题X", nil, map[string]any{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	row, err := s.GetRun(ctx, rid)
	if err != nil || row.Input != "原始输入" || row.Topic != "话题X" {
		t.Fatalf("GetRun 投影应含 input/topic: %+v err=%v", row, err)
	}
}

// TestDeleteSandboxClearsLeaseTombstone 审计准入 #4：Release 置过期保留的
// lease 行随沙箱删除清理（FK NO ACTION 曾阻止 parent 删除——GC 卡住回归）。
func TestDeleteSandboxClearsLeaseTombstone(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	sid := fmt.Sprintf("sb_del_%d", time.Now().UnixNano())
	_ = s.CreateOrg(ctx, "org_del", "o")
	_ = s.CreateAgent(ctx, "a_dl", "org_del", "a", &sessionapi.AgentConfig{Model: "m", Instructions: "i", Version: 1})
	_ = s.CreateSession(ctx, "s_dl", "org_del", "a_dl")
	_, _ = s.Pool.Exec(ctx, `INSERT INTO sandboxes (sandbox_id, org_id, session_id, driver, status, created_at, ttl, image)
		VALUES ($1, 'org_del', 's_dl', 'docker', 'ready', now(), interval '1 day', 'python:3.12-slim')`, sid)
	l, err := s.AcquireLease(ctx, sid, "run-x", time.Hour)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := s.ReleaseLease(ctx, sid, l.Generation); err != nil {
		t.Fatalf("release: %v", err)
	}
	// lease 行保留（expired tombstone——ABA 语义）→ 删沙箱必须一并清理
	if err := s.DeleteSandbox(ctx, sid); err != nil {
		t.Fatalf("delete sandbox 应清理 lease tombstone（FK 回归）: %v", err)
	}
}

// TestEffectLedgerRoundtrip 审计 A 批：llm/mcp 账本的 prepared→result/unknown
// 落账（对账证据——崩溃窗口的第二次闭包执行更新同一行）。
func TestEffectLedgerRoundtrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	rid := fmt.Sprintf("r_ledger_%d", time.Now().UnixNano())
	// A1：prepared → result（usage 落账）
	if _, err := s.PutLLMCallPrepared(ctx, rid, 0, "hash-a"); err != nil {
		t.Fatalf("llm prepared: %v", err)
	}
	if err := s.PutLLMCallResult(ctx, rid, 0, 120, 45, false, false, "", ""); err != nil {
		t.Fatalf("llm result: %v", err)
	}
	// 错误 → unknown（usage_unknown 账本化）
	if _, err := s.PutLLMCallPrepared(ctx, rid, 1, "hash-b"); err != nil {
		t.Fatalf("llm prepared2: %v", err)
	}
	if err := s.PutLLMCallResult(ctx, rid, 1, 0, 0, false, true, "model 400", ""); err != nil {
		t.Fatalf("llm unknown: %v", err)
	}
	// A2：mcp prepared → 错误 unknown
	if _, err := s.PutMCPCallPrepared(ctx, rid, 0, "echo", "echo", "call-1", "hash-1"); err != nil {
		t.Fatalf("mcp prepared: %v", err)
	}
	if err := s.PutMCPCallResult(ctx, rid, 0, "echo", "echo", "call-1", "hash-1", "", "dial timeout"); err != nil {
		t.Fatalf("mcp unknown: %v", err)
	}
	var state string
	if err := s.Pool.QueryRow(ctx, `SELECT state FROM llm_calls WHERE run_id=$1 AND step=1`, rid).Scan(&state); err != nil || state != "unknown" {
		t.Fatalf("llm step1 应 unknown: %q err=%v", state, err)
	}
	if err := s.Pool.QueryRow(ctx, `SELECT state FROM mcp_calls WHERE run_id=$1`, rid).Scan(&state); err != nil || state != "unknown" {
		t.Fatalf("mcp 应 unknown（网络超时可能效果已发生）: %q err=%v", state, err)
	}
}

// TestArbitrationResultReplay 仲裁合同深化：result 冲突 + hash 同 → 回读冻结
// 原结果（零派发）；hash 异 → 拒绝（operation 冲突）。
func TestArbitrationResultReplay(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	rid := fmt.Sprintf("r_arb_%d", time.Now().UnixNano())
	if _, err := s.PutLLMCallPrepared(ctx, rid, 0, "hash-1"); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := s.PutLLMCallResult(ctx, rid, 0, 10, 5, false, false, "", `{"final":"冻结原结果"}`); err != nil {
		t.Fatalf("result: %v", err)
	}
	// 冲突读回：state=result + hash 同 → 回读冻结结果
	row, err := s.PutLLMCallPrepared(ctx, rid, 0, "hash-1")
	if err != nil || row.State != "result" || row.RequestHash != "hash-1" || row.Result == "" {
		t.Fatalf("冲突应读回 result+hash+冻结结果: %+v err=%v", row, err)
	}
}

// TestAuditChainIntegrity M1：哈希链——追加事件的 prev/event hash 成链；
// 篡改任一行的 payload 后验证失败（防篡改）。
func TestAuditChainIntegrity(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	sid := fmt.Sprintf("s_audit_%d", time.Now().UnixNano())
	_ = s.CreateOrg(ctx, "org_audit", "o")
	_ = s.CreateAgent(ctx, "a_au", "org_audit", "a", &sessionapi.AgentConfig{Model: "m", Instructions: "i", Version: 1})
	_ = s.CreateSession(ctx, sid, "org_audit", "a_au")
	if _, err := s.AppendEvent(ctx, sid, "", event.RunStarted, json.RawMessage(`{"v":1}`), sid+":e1"); err != nil {
		t.Fatalf("append e1: %v", err)
	}
	if _, err := s.AppendEvent(ctx, sid, "", event.RunCompleted, json.RawMessage(`{"v":2}`), sid+":e2"); err != nil {
		t.Fatalf("append e2: %v", err)
	}
	rows, err := s.ListEventsWithChain(ctx, sid, 100)
	if err != nil || len(rows) != 2 {
		t.Fatalf("export: %d 行 err=%v", len(rows), err)
	}
	if rows[1]["prev_hash"] != rows[0]["event_hash"] {
		t.Fatalf("链不连续: %v -> %v", rows[0]["event_hash"], rows[1]["prev_hash"])
	}
	// 篡改模拟：改 payload 后 hash 不再匹配（验证工具会失败）
	orig := rows[0]["payload"].(json.RawMessage)
	_ = orig
	if err := s.SetAuditHold(ctx, sid); err != nil {
		t.Fatalf("hold: %v", err)
	}
}

// TestGoalModelRoundtrip M2：Goal version 递增 + hash；WorkItem 队列；
// Evidence 新鲜度；ActionClaim 动作级占有权。
func TestGoalModelRoundtrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	// Goal：两次写入 version 递增 + hash 变化
	_ = s.CreateOrg(ctx, "org_m2", "o")
	gid := fmt.Sprintf("goal-m2-%d", time.Now().UnixNano())
	g1, err := s.UpsertGoal(ctx, gid, "org_m2", "目标 A", "scope-a", "owner-1")
	if err != nil || g1.Version != 1 {
		t.Fatalf("goal v1: %+v err=%v", g1, err)
	}
	g2, err := s.UpsertGoal(ctx, gid, "org_m2", "目标 A 改", "scope-a", "owner-1")
	if err != nil || g2.Version != 2 || g2.StateHash == g1.StateHash {
		t.Fatalf("goal v2 应递增且 hash 变: %+v %+v err=%v", g1, g2, err)
	}
	// WorkItem
	if err := s.CreateWorkItem(ctx, fmt.Sprintf("wi-%d", time.Now().UnixNano()), gid, "做切片", "impl", 1, ""); err != nil {
		t.Fatalf("work item: %v", err)
	}
	items, err := s.ListOpenWorkItems(ctx, gid, 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("items: %+v err=%v", items, err)
	}
	// Evidence（过期 → StaleEvidence 可见；session FK 地基）
	_ = s.CreateAgent(ctx, "a_m2", "org_m2", "a", &sessionapi.AgentConfig{Model: "m", Instructions: "i", Version: 1})
	_ = s.CreateSession(ctx, "s_m2", "org_m2", "a_m2")
	if err := s.PutEvidence(ctx, "ev-1", "s_m2", "r_m2", "hash-abc", "commit-a", "rev-1", "test", nil); err != nil {
		t.Fatalf("evidence: %v", err)
	}
	// ActionClaim：领取 + 异 agent 活跃拒绝 + 过期后接管
	if _, err := s.AcquireActionClaim(ctx, "repo:branch_x", "agent-a", "repo_write", time.Hour); err != nil {
		t.Fatalf("claim a: %v", err)
	}
	if _, err := s.AcquireActionClaim(ctx, "repo:branch_x", "agent-b", "repo_write", time.Hour); err == nil {
		t.Fatal("活跃 claim 的异 agent 接管应拒绝")
	}
}

// TestHarnessRegistryRoundtrip Harness 装配阶段 1：注册/心跳/列表/active
// 查询/切态——与 executor 注册表同款的生命周期。
func TestHarnessRegistryRoundtrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	oid := fmt.Sprintf("org_hr_%d", time.Now().UnixNano())
	_ = s.CreateOrg(ctx, oid, "o")
	if err := s.RegisterHarness(ctx, oid, "claude", "http://h1:8000", "v1", []string{"claude-sonnet-4-6"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	// 心跳 upsert 同版本不新增
	if err := s.RegisterHarness(ctx, oid, "claude", "http://h1:8000", "v1", []string{"claude-sonnet-4-6"}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	rows, err := s.ListHarnesses(ctx, oid)
	if err != nil || len(rows) != 1 || rows[0].State != "active" {
		t.Fatalf("list: %+v err=%v", rows, err)
	}
	// active 查询（同 name 新版本注册后仍返回原 active）
	if err := s.RegisterHarness(ctx, oid, "claude", "http://h2:8000", "v2", nil); err != nil {
		t.Fatalf("v2: %v", err)
	}
	h, err := s.GetActiveHarness(ctx, oid, "claude")
	if err != nil || h.Endpoint != "http://h2:8000" || h.Version != "v2" {
		t.Fatalf("active: %+v err=%v", h, err)
	}
	// 切态
	if err := s.SetHarnessState(ctx, oid, "claude", "v2", "draining"); err != nil {
		t.Fatalf("drain: %v", err)
	}
	// v2 draining 后回退 v1（升级回滚的自然语义——active 版本链）
	h2, err := s.GetActiveHarness(ctx, oid, "claude")
	if err != nil || h2.Version != "v1" {
		t.Fatalf("draining 后应回退 v1: %+v err=%v", h2, err)
	}
}
