package restate

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/bingdilotus/chronotope/internal/core/commitment"
	"github.com/bingdilotus/chronotope/internal/core/event"
	"github.com/bingdilotus/chronotope/internal/policy"
	"github.com/bingdilotus/chronotope/internal/store"
)

// OutboundGate 出站承诺门（修正版 §2.3 两段式）。
// ① decision 门 fail-open（降频优化不是义务来源——挂了放行 + warning）
// ② 判定为正 → 完整抽取（第一阶段降级为 content=原消息全文）→ 写入
//
//	fail-closed（义务记录失败阻断出站）。
type OutboundGate struct {
	Decider policy.CommitmentDecider
	Store   *store.Store
}

// CommitGateResult 门的判定结果。
type CommitGateResult struct {
	Admitted   bool             // 是否记录承诺
	Commitment *store.CommitRow // 已记录（pending）——回执后 open
	Blocked    bool             // 写入失败阻断出站（fail-closed）
	Warning    string           // gate 失败 fail-open 的 warning
}

// Admit 出站前判定。返回的 CommitRow 是 pending 态——发送成功后调用
// ConfirmCommitSent 转 open；发送失败则留在 pending（反向 reconciler 扫
// orphaned）。
func (g *OutboundGate) Admit(ctx context.Context, sessionID, runID, agentID, message, counterparty string) (*CommitGateResult, error) {
	res := &CommitGateResult{}
	// ① decision 门（fail-open）
	decision, err := g.Decider.Decide(ctx, message, counterparty)
	if err != nil {
		// gate 失败 = 放行 + warning 事件（修正版 §2.3 fail-open——事后
		// 核对；含消息 digest）
		res.Warning = fmt.Sprintf("gate failed: %v", err)
		if g.Store != nil {
			warn, _ := json.Marshal(map[string]any{
				"commitment_gate_failed": err.Error(),
				"message_digest":         store.ContentDigest(message),
			})
			_, _ = g.Store.AppendEvent(ctx, sessionID, runID, event.Warning, warn,
				fmt.Sprintf("gate-fail:%s:%d", runID, time.Now().UnixNano()))
		}
		return res, nil
	}
	if !decision.IsCommissive {
		return res, nil // 判定为负——正常放行，不记承诺
	}
	// ② 判定为正 → 写入（fail-closed）
	if g.Store == nil {
		return res, nil
	}
	intent := store.CommitIntent{
		ID:           "c_" + store.ContentDigest(message)[:16] + "_" + fmt.Sprint(time.Now().UnixNano()%1e9),
		AgentID:      agentID,
		MadeInThread: sessionID,
		MadeInRun:    runID,
		Counterparty: counterparty,
		SpeechAct:    string(commitment.ActCommissive),
		Content:      message, // 第一阶段降级：content = 原消息全文（span 抽取后置）
		Confidence:   decision.Confidence,
	}
	if decision.Strength != "" {
		_ = decision.Strength
	}
	row, err := g.Store.PutCommitIntent(ctx, intent)
	if err != nil {
		// fail-closed：义务记录失败阻断出站（修正版 §2.3 反转）
		res.Blocked = true
		res.Warning = fmt.Sprintf("commitment write failed: %v", err)
		return res, nil
	}
	res.Admitted = true
	res.Commitment = row
	return res, nil
}

// ConfirmSent 发送回执：pending → open。
func (g *OutboundGate) ConfirmSent(ctx context.Context, id string) error {
	if g.Store == nil {
		return nil
	}
	if err := g.Store.ConfirmCommitSent(ctx, id); err != nil {
		// 回执失败不阻断（消息已发出）——留在 pending，反向 reconciler
		// 扫 orphaned（前向孤儿有状态可寻址）
		slog.Warn("commitment confirm failed（留 pending 待 reconciler）", "id", id, "err", err)
	}
	return nil
}

// ConfirmFailed 发送失败：承诺留在 pending（不发回执）——由反向
// reconciler 按超时扫 orphaned。
func (g *OutboundGate) ConfirmFailed(ctx context.Context, id string) {
	if g.Store == nil {
		return
	}
	// 发送失败——承诺未发出：转 canceled（发出前撤回语义）
	_, _ = g.Store.TransitionCommit(ctx, id, commitment.StatePending, commitment.StateCanceled)
}
