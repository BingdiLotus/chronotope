package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/bingdilotus/chronotope/internal/core/event"
	"github.com/bingdilotus/chronotope/internal/store"
)

// usageBucketDuration 是桶宽（契约 §3 技术选型：1min 桶）。
const usageBucketDuration = time.Minute

// Aggregator 从事件流聚合三轴计量（活跃秒 / token / 计算秒，1min 桶）。
//
// MVP 策略（W4）：重建式——按全局事件 id 水位扫描新事件，对涉及的每个会话
// 清空 usage 行后从该会话全部事件重算（幂等；小数据量可行）。增量 rollup
// 与冷层归档（W8）后置。
//
// 聚合语义：
//   - 活跃秒：run.started → run.completed/failed/cancelled 的时长，计入 run 起始桶；
//   - token：llm.call 载荷 usage.{tokens_in,tokens_out}，计入事件桶；
//   - 计算秒：sandbox.exec 载荷 duration_ms，计入事件桶。
type Aggregator struct {
	Store    Store
	Logger   *slog.Logger
	Interval time.Duration

	watermark int64 // 全局事件 id 水位（内存态；重启归零 → 全量重算，幂等）
}

// Run 周期执行聚合（每 Interval 一轮；水位内存态，重启后全量重算，幂等）。
func (a *Aggregator) Run(ctx context.Context) {
	if a.Interval <= 0 {
		a.Interval = usageBucketDuration
	}
	ticker := time.NewTicker(a.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.pass(ctx); err != nil && !errors.Is(err, context.Canceled) {
				a.Logger.Warn("usage aggregation failed", "err", err)
			}
		}
	}
}

// pass 一轮：排空水位后的全部事件（分批扫描直至清空）并对涉及会话重建用量。
// 排空语义：进程重启后水位归零，积压事件在一轮内全部追平（每批 1000 条，
// 上限 100 批防御）。单批实现曾导致 demo 中途重启 api 时预算会话的用量
// 滞后两轮才入桶（w5-budget e2e 实证冻结未触发）。
func (a *Aggregator) pass(ctx context.Context) error {
	for batch := 0; batch < 100; batch++ {
		rows, err := a.Store.ListEventsAfterID(ctx, a.watermark, 1000)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		sessions := map[string]bool{}
		for _, row := range rows {
			sessions[row.SessionID] = true
			if row.ID > a.watermark {
				a.watermark = row.ID
			}
		}
		for sessionID := range sessions {
			if err := a.rebuildSession(ctx, sessionID); err != nil {
				return err
			}
		}
	}
	return nil
}

// rebuildSession 清空会话 usage 行并全量重算（幂等）。
func (a *Aggregator) rebuildSession(ctx context.Context, sessionID string) error {
	if err := a.Store.ResetSessionUsage(ctx, sessionID); err != nil {
		return err
	}
	events, err := a.Store.ListEvents(ctx, sessionID, 0, 10000)
	if err != nil {
		return err
	}
	for _, u := range computeUsage(sessionID, events) {
		if err := a.Store.UpsertUsage(ctx, u); err != nil {
			return err
		}
	}
	return nil
}

// computeUsage 纯函数：事件 → 1min 桶用量（可单测的聚合核心）。
func computeUsage(sessionID string, events []store.EventRow) []store.UsageRow {
	type runSpan struct {
		started time.Time
		closed  bool
	}
	spans := map[string]*runSpan{}
	buckets := map[time.Time]*store.UsageRow{}

	getBucket := func(at time.Time) *store.UsageRow {
		key := at.Truncate(usageBucketDuration)
		u, ok := buckets[key]
		if !ok {
			u = &store.UsageRow{SessionID: sessionID, Bucket: key}
			buckets[key] = u
		}
		return u
	}

	for _, ev := range events {
		switch ev.Type {
		case event.RunStarted:
			spans[ev.RunID] = &runSpan{started: ev.At}
		case event.RunCompleted, event.RunFailed, event.RunCancelled:
			if sp, ok := spans[ev.RunID]; ok && !sp.closed {
				sp.closed = true
				dur := ev.At.Sub(sp.started).Seconds()
				if dur < 0 {
					dur = 0 // 时钟回拨防御（边界语义 §5）
				}
				getBucket(sp.started).ActiveSeconds += dur
			}
		case event.LLMCall:
			var p struct {
				Usage struct {
					TokensIn  int64 `json:"tokens_in"`
					TokensOut int64 `json:"tokens_out"`
				} `json:"usage"`
			}
			if err := json.Unmarshal(ev.Payload, &p); err == nil {
				u := getBucket(ev.At)
				u.TokensIn += p.Usage.TokensIn
				u.TokensOut += p.Usage.TokensOut
			}
		case event.SandboxExec:
			var p struct {
				DurationMs int64 `json:"duration_ms"`
			}
			if err := json.Unmarshal(ev.Payload, &p); err == nil && p.DurationMs > 0 {
				getBucket(ev.At).ComputeSeconds += float64(p.DurationMs) / 1000
			}
		}
	}
	out := make([]store.UsageRow, 0, len(buckets))
	for _, u := range buckets {
		out = append(out, *u)
	}
	sortUsageRows(out)
	return out
}

func sortUsageRows(rows []store.UsageRow) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && rows[j].Bucket.Before(rows[j-1].Bucket); j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
}
