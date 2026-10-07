package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/bingdilotus/chronotope/internal/core/event"
	"github.com/bingdilotus/chronotope/internal/store"
)

func row(id int64, sessionID, runID string, typ event.Type, payload string, at time.Time) store.EventRow {
	return store.EventRow{ID: id, SessionID: sessionID, RunID: runID, Type: typ, Payload: json.RawMessage(payload), At: at}
}

func TestComputeUsageThreeAxes(t *testing.T) {
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC) // 10:00:00

	events := []store.EventRow{
		// run r_1：10:00:00 开始、10:00:30 完成 → 活跃 30s 入 10:00 桶
		row(1, "s_1", "r_1", event.RunStarted, `{"v":1}`, base),
		// llm.call：10:00:05，tokens 120/80 → 10:00 桶
		row(2, "s_1", "r_1", event.LLMCall, `{"v":1,"usage":{"tokens_in":120,"tokens_out":80}}`, base.Add(5*time.Second)),
		// sandbox.exec：10:00:10，duration 2500ms → 计算 2.5s 入 10:00 桶
		row(3, "s_1", "r_1", event.SandboxExec, `{"v":1,"duration_ms":2500}`, base.Add(10*time.Second)),
		// 10:01:10 的 llm.call → 10:01 桶
		row(4, "s_1", "r_1", event.LLMCall, `{"v":1,"usage":{"tokens_in":10,"tokens_out":5}}`, base.Add(70*time.Second)),
		// run 完成 → 活跃秒按 run 起始桶归集
		row(5, "s_1", "r_1", event.RunCompleted, `{"v":1}`, base.Add(30*time.Second)),
	}

	usage := computeUsage("s_1", events)
	if len(usage) != 2 {
		t.Fatalf("应产出 2 个桶，得 %d: %+v", len(usage), usage)
	}
	byBucket := map[time.Time]store.UsageRow{}
	for _, u := range usage {
		byBucket[u.Bucket] = u
	}
	b0 := byBucket[base.Truncate(time.Minute)]
	if b0.ActiveSeconds != 30 {
		t.Fatalf("活跃秒应为 30（run 时长按起始桶），得 %v", b0.ActiveSeconds)
	}
	if b0.TokensIn != 120 || b0.TokensOut != 80 {
		t.Fatalf("token 应 120/80，得 %d/%d", b0.TokensIn, b0.TokensOut)
	}
	if b0.ComputeSeconds != 2.5 {
		t.Fatalf("计算秒应为 2.5，得 %v", b0.ComputeSeconds)
	}
	b1 := byBucket[base.Add(time.Minute).Truncate(time.Minute)]
	if b1.TokensIn != 10 || b1.TokensOut != 5 || b1.ActiveSeconds != 0 || b1.ComputeSeconds != 0 {
		t.Fatalf("10:01 桶不符: %+v", b1)
	}
}

func TestComputeUsageUnclosedRunAndClockSkew(t *testing.T) {
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	events := []store.EventRow{
		row(1, "s_1", "r_1", event.RunStarted, `{"v":1}`, base),
		// 未闭合 run：不计活跃秒
		// 时钟回拨防御：completed 早于 started → 活跃秒 0，不 panic
		row(2, "s_1", "r_2", event.RunStarted, `{"v":1}`, base),
		row(3, "s_1", "r_2", event.RunCompleted, `{"v":1}`, base.Add(-10*time.Second)),
	}
	usage := computeUsage("s_1", events)
	var total float64
	for _, u := range usage {
		total += u.ActiveSeconds
	}
	if total != 0 {
		t.Fatalf("未闭合 + 时钟回拨应贡献 0 活跃秒，得 %v", total)
	}
}

func TestAggregatorPassRebuildsSessions(t *testing.T) {
	fs := newFakeStore()
	fs.addEvent("s_1", event.RunStarted)
	fs.addEvent("s_1", event.RunCompleted)
	a := &Aggregator{Store: fs, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Interval: time.Minute}
	if err := a.pass(context.Background()); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if len(fs.usageBuckets) == 0 {
		t.Fatal("应产出 usage 行")
	}
	if fs.watermark == 0 {
		t.Fatal("水位应推进")
	}
	// 幂等：同水位重跑 → 零增量（delta 不变，usage 不变）
	var totalBefore float64
	for _, u := range fs.usageBuckets {
		totalBefore += u.ActiveSeconds
	}
	if err := a.pass(context.Background()); err != nil {
		t.Fatalf("重跑 pass: %v", err)
	}
	var totalAfter float64
	for _, u := range fs.usageBuckets {
		totalAfter += u.ActiveSeconds
	}
	if totalAfter != totalBefore {
		t.Fatalf("同水位重跑应零增量: %v → %v", totalBefore, totalAfter)
	}
	// 新事件 → delta 累加
	fs.addEvent("s_1", event.RunStarted)
	fs.addEvent("s_1", event.RunCompleted)
	if err := a.pass(context.Background()); err != nil {
		t.Fatalf("新事件 pass: %v", err)
	}
	var totalNew float64
	for _, u := range fs.usageBuckets {
		totalNew += u.ActiveSeconds
	}
	if totalNew <= totalAfter {
		t.Fatalf("新事件应累加: %v → %v", totalAfter, totalNew)
	}
}
