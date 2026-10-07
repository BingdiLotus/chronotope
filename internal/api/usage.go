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
// 策略（期 2 §B）：增量 rollup——持久水位（usage_watermark）+ delta 累加。
// 每轮只处理水位后的新事件：token/计算秒直接加算；活跃秒在 run 终态事件
// 到达时经 RunStartedAt 查起始时间，计入起始桶。ApplyUsageDelta 同事务推进
// 水位——崩溃时水位未推进 → 重放同批 delta（幂等，同水位重跑零增量）。
//
// 聚合语义：
//   - 活跃秒：run.started → run.completed/failed/cancelled 的时长，计入 run 起始桶；
//   - token：llm.call 载荷 usage.{tokens_in,tokens_out}，计入事件桶；
//   - 计算秒：sandbox.exec 载荷 duration_ms，计入事件桶。
type Aggregator struct {
	Store    Store
	Logger   *slog.Logger
	Interval time.Duration
}

// Run 周期执行聚合（每 Interval 一轮）。
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

// pass 一轮：排空水位后的全部事件（分批扫描直至清空）并按批应用增量。
// 排空语义：进程重启后从持久水位续跑（不重扫历史）；积压事件一轮追平
// （每批 1000 条，上限 100 批防御）。
func (a *Aggregator) pass(ctx context.Context) error {
	watermark, err := a.Store.GetUsageWatermark(ctx)
	if err != nil {
		return err
	}
	for batch := 0; batch < 100; batch++ {
		rows, err := a.Store.ListEventsAfterID(ctx, watermark, 1000)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		toID := watermark
		for _, row := range rows {
			if row.ID > toID {
				toID = row.ID
			}
		}
		deltas, err := a.computeDelta(ctx, rows)
		if err != nil {
			return err
		}
		if err := a.Store.ApplyUsageDelta(ctx, watermark, toID, deltas); err != nil {
			return err
		}
		watermark = toID
	}
	return nil
}

// computeDelta 纯增量：本批新事件 → usage 增量行。活跃秒跨度经 RunStartedAt
// 查起始时间（run.started 可能在水位之前——增量语义下不回放旧事件）。
func (a *Aggregator) computeDelta(ctx context.Context, rows []store.EventRow) ([]store.UsageRow, error) {
	buckets := map[time.Time]*store.UsageRow{}
	getBucket := func(sessionID string, at time.Time) *store.UsageRow {
		key := at.Truncate(usageBucketDuration)
		u, ok := buckets[key]
		if !ok {
			u = &store.UsageRow{SessionID: sessionID, Bucket: key}
			buckets[key] = u
		}
		return u
	}
	for _, ev := range rows {
		switch ev.Type {
		case event.RunCompleted, event.RunFailed, event.RunCancelled:
			if started, err := a.Store.RunStartedAt(ctx, ev.RunID); err == nil {
				dur := ev.At.Sub(started).Seconds()
				if dur < 0 {
					dur = 0 // 时钟回拨防御（边界语义 §5）
				}
				getBucket(ev.SessionID, started).ActiveSeconds += dur
			}
		case event.LLMCall:
			var p struct {
				Usage struct {
					TokensIn  int64 `json:"tokens_in"`
					TokensOut int64 `json:"tokens_out"`
				} `json:"usage"`
			}
			if err := json.Unmarshal(ev.Payload, &p); err == nil {
				u := getBucket(ev.SessionID, ev.At)
				u.TokensIn += p.Usage.TokensIn
				u.TokensOut += p.Usage.TokensOut
			}
		case event.SandboxExec:
			var p struct {
				DurationMs int64 `json:"duration_ms"`
			}
			if err := json.Unmarshal(ev.Payload, &p); err == nil && p.DurationMs > 0 {
				getBucket(ev.SessionID, ev.At).ComputeSeconds += float64(p.DurationMs) / 1000
			}
		}
	}
	out := make([]store.UsageRow, 0, len(buckets))
	for _, u := range buckets {
		out = append(out, *u)
	}
	sortUsageRows(out)
	return out, nil
}

func sortUsageRows(rows []store.UsageRow) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && rows[j].Bucket.Before(rows[j-1].Bucket); j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
}
