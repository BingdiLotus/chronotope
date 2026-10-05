package restate

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bingdilotus/chronotope/internal/core/event"
)

// Emitter 是事件发射层（worker-架构设计 §1 EM）：幂等写入 + dedupe 键（契约规范 §5）。
// 事件插入带去重键，重放重复发射被幂等吞掉；payload 强制携带 v 字段。
type Emitter struct {
	Store Store
}

// Emit 发射一条事件；kind/tool 参与 dedupe 键（run_id:step:kind[:tool]），
// 生命周期类事件（run.started/completed/failed）用事件类型本身作 kind。
// 会话级事件（run_id 为空）以 session_id:kind[:tool] 去重——会话间不得互相吞事件。
func (e *Emitter) Emit(ctx context.Context, sessionID, runID string, step int, typ event.Type, kind, tool string, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	if _, ok := payload["v"]; !ok {
		payload["v"] = 1
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("emit: marshal payload: %w", err)
	}
	if kind == "" {
		kind = string(typ)
	}
	var dedupe string
	if runID == "" {
		dedupe = sessionID + ":" + kind
		if tool != "" {
			dedupe += ":" + tool
		}
	} else {
		dedupe = event.DedupeKey(runID, step, kind, tool)
	}
	if _, err := e.Store.AppendEvent(ctx, sessionID, runID, typ, payloadJSON, dedupe); err != nil {
		return fmt.Errorf("emit %s: %w", typ, err)
	}
	return nil
}
