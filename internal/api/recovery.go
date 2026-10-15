package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bingdilotus/chronotope/internal/core/commitment"
	"github.com/bingdilotus/chronotope/internal/core/event"
	"github.com/bingdilotus/chronotope/internal/core/wait"
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
	// restate-ai 调研①：审批超时的 SLA 降级——过期的 operator_input
	// 等待 → restate resolve 拒绝（approval_timeout 的显式路径）
	ttl := 3 * time.Hour
	if v := os.Getenv("APPROVAL_TTL"); v != "" {
		if d, pErr := time.ParseDuration(v); pErr == nil && d > 0 {
			ttl = d
		}
	}
	if expired, eErr := a.Store.ListExpiredOperatorWaits(ctx, ttl); eErr == nil {
		for _, w := range expired {
			// restate 的 awakeable resolve（拒绝语义——payload 空 = 拒绝）
			if rErr := a.Ingress.Call(ctx, "/restate/awakeables/"+w["handle"].(string)+"/resolve",
				"POST", map[string]any{"payload": ""}, nil); rErr == nil {
				if resolved, _ := a.Store.ResolveWait(ctx, w["session_id"].(string), wait.OperatorInput, w["handle"].(string)); !resolved && a.Logger != nil {
					a.Logger.Warn("approval timeout resolve 0 行（handle 失配）", "handle", w["handle"])
				}
				if a.Logger != nil {
					a.Logger.Warn("approval timeout rejected", "session", w["session_id"], "intent", w["intent"])
				}
			}
		}
	}
	// 承诺原语（修正版 §2.4 骨架）：反向 reconciler——pending 超时 →
	// orphaned（前向孤儿有状态可寻址）；due_at 过期 → expired
	commitTTL := 30 * time.Minute // pending 的发送确认超时（与发送重试窗口对齐）
	if v := os.Getenv("COMMITMENT_PENDING_TTL"); v != "" {
		if d, pErr := time.ParseDuration(v); pErr == nil && d > 0 {
			commitTTL = d
		}
	}
	if stale, cErr := a.Store.ListStalePendingCommitments(ctx, time.Now().Add(-commitTTL).Format(time.RFC3339), 100); cErr == nil {
		for _, c := range stale {
			ok, _ := a.Store.TransitionCommit(ctx, c.ID, commitment.StatePending, commitment.StateOrphaned)
			if ok && a.Logger != nil {
				a.Logger.Warn("commitment orphaned（发送确认超时）", "id", c.ID, "agent", c.AgentID)
			}
		}
	}
	if expired, eErr := a.Store.ListExpiredCommitments(ctx, time.Now().Format(time.RFC3339), 100); eErr == nil {
		for _, c := range expired {
			ok, _ := a.Store.TransitionCommit(ctx, c.ID, commitment.StateOpen, commitment.StateExpired)
			if ok && a.Logger != nil {
				a.Logger.Warn("commitment expired（due_at 已过无兑现证据）", "id", c.ID, "agent", c.AgentID)
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
			// 409 already invoked = workflow 已调用（幂等命中——重投
			// 成功语义；标记完成不再 stale 循环——api 忙的根源修复）
			if strings.Contains(err.Error(), "already invoked") || strings.Contains(err.Error(), "409") {
				_ = a.Store.MarkAdmissionDispatched(ctx, run.ID)
				continue
			}
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
