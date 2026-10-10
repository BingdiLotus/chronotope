package api

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/bingdilotus/chronotope/internal/core/event"
	"time"
)

// AdmissionRecovery 接纳屏障恢复（评审 #6）：api 崩溃窗口内「run 行已建但
// ingress 未达/已失」的 queued run——周期扫描重投（run_workflow 幂等键 =
// run_id，重投安全；事件/消息写回全部幂等）。
type AdmissionRecovery struct {
	Store   Store
	Ingress RestateIngress
	Logger  Logger
	Stale   time.Duration // queued 年龄阈值（默认 5min）
}

// Run 周期扫描（interval 默认 1min）。
func (a *AdmissionRecovery) Run(ctx context.Context, interval time.Duration) {
	if a.Stale <= 0 {
		a.Stale = 5 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.pass(ctx); err != nil && a.Logger != nil {
				a.Logger.Warn("admission recovery pass failed", "err", err)
			}
		}
	}
}

func (a *AdmissionRecovery) pass(ctx context.Context) error {
	// 审计 5.1（八期）：pending outbox 是精确派发源（稳定 workflow key 重投）
	if pending, pErr := a.Store.ListPendingAdmissions(ctx, 2*time.Minute, 20); pErr == nil {
		for _, p := range pending {
			var out struct {
				RunID string `json:"run_id"`
			}
			if err := a.Ingress.Call(ctx, "/run_workflow/"+p.RunID+"/run", "POST",
				map[string]any{"session_id": p.SessionID, "input": p.Input, "topic": p.Topic}, &out); err != nil {
				if a.Logger != nil {
					a.Logger.Warn("admission pending redeliver failed", "run_id", p.RunID, "err", err)
				}
				continue
			}
			_ = a.Store.MarkAdmissionDispatched(ctx, p.RunID)
		}
	}
	// 审计 4.2（九期）：completed 事件的补交——终态已提交但事件写失败的
	// run（观察者等不到完成）按原身份补发（持久修复义务）
	if orphans, oErr := a.Store.ListCompletedWithoutEvent(ctx, 20); oErr == nil {
		for _, o := range orphans {
			if _, err := a.Store.AppendEvent(ctx, o.SessionID, o.RunID, event.RunCompleted,
				json.RawMessage(fmt.Sprintf(`{"run_id":%q,"recovered":true}`, o.RunID)),
				fmt.Sprintf("completed-recover:%s", o.RunID)); err != nil {
				if a.Logger != nil {
					a.Logger.Warn("completed 补交失败", "run_id", o.RunID, "err", err)
				}
			}
		}
	}
	runs, err := a.Store.ListStaleQueuedRuns(ctx, time.Now().Add(-a.Stale), 20)
	if err != nil {
		return err
	}
	for _, run := range runs {
		// 重投：幂等 ingress（worker 侧 run_id 幂等键；重复提交无害）
		var out struct {
			RunID string `json:"run_id"`
		}
		// 审计 #3：重投带不可变 command（input/topic 落 Run 行——丢输入反例关闭）
		if err := a.Ingress.Call(ctx, "/run_workflow/"+run.ID+"/run", "POST",
			map[string]any{"session_id": run.SessionID, "input": run.Input, "topic": run.Topic}, &out); err != nil {
			if a.Logger != nil {
				a.Logger.Warn("admission recovery redeliver failed", "run_id", run.ID, "err", err)
			}
			continue
		}
		if a.Logger != nil {
			a.Logger.Info("admission recovery redelivered", "run_id", run.ID)
		}
	}
	return nil
}
