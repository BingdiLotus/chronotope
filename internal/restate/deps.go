package restate

import (
	"context"
	"encoding/json"

	restate "github.com/restatedev/sdk-go"

	"github.com/bingdilotus/chronotope/internal/core/event"
	"github.com/bingdilotus/chronotope/internal/store"
)

// Store 是 worker 侧 store 的最小接口（消费者侧定义，测试以 fake 替换；
// worker-架构设计 §1 的 ST 模块）。worker 是事件唯一写入者，api 只读投影。
type Store interface {
	AppendEvent(ctx context.Context, sessionID, runID string, typ event.Type, payload json.RawMessage, dedupeKey string) (seq int64, err error)
	AppendMessage(ctx context.Context, sessionID, runID string, step int, role string, content json.RawMessage) error
	ListMessages(ctx context.Context, sessionID string, limit int) ([]store.Message, error)
	// GetRun 供 webhook 解析 run → session（HITL 审批回调按 run_id 定位 awakeable）。
	GetRun(ctx context.Context, runID string) (*store.Run, error)
	// CreateRun 供 scheduler 建 child run 行（events/messages 的 FK 前提）。
	CreateRun(ctx context.Context, id, sessionID string, trigger json.RawMessage, bound map[string]any) (bool, error)
}

// SessionSource 是会话状态的读写接缝：run_workflow 经它读/回填 session_object
// （Virtual Object 单写者串行）；测试以 fake 替换，避免在单测中依赖 Restate 语境。
type SessionSource interface {
	GetState(ctx restate.Context, sessionID string) (SessionState, error)
	// AttachSandbox 回填会话作用域沙箱 id（懒创建后调用；幂等）。
	AttachSandbox(ctx restate.Context, sessionID, sandboxID string) error
	// SetPendingAwakeable 记录挂起的审批 awakeable id（HITL；resolve 前可查）。
	SetPendingAwakeable(ctx restate.Context, sessionID, awakeableID string) error
}

// Deps 是 worker 服务层的依赖集（worker-架构设计 §1：HC 客户端 / EC 客户端 /
// EM+ST 经 Store / MO 后置）。
type Deps struct {
	Store    Store
	Harness  Harness
	Executor Executor
	Sessions SessionSource
}

// RestateSessionSource 是生产实现：经 Restate virtual object 调用读写会话状态。
type RestateSessionSource struct{}

// GetState 读 session_object 状态（未初始化时返回零值状态，由调用方校验）。
func (RestateSessionSource) GetState(ctx restate.Context, sessionID string) (SessionState, error) {
	return restate.Object[SessionState](ctx, SessionObjectName, sessionID, "GetState").
		Request(restate.Void{})
}

// AttachSandbox 回填沙箱 id（对象调用幂等：同值重复设置无害，重放重发安全）。
func (RestateSessionSource) AttachSandbox(ctx restate.Context, sessionID, sandboxID string) error {
	_, err := restate.Object[SessionState](ctx, SessionObjectName, sessionID, "AttachSandbox").
		Request(sandboxID)
	return err
}

// SetPendingAwakeable 记录挂起的审批 awakeable（HITL）。
func (RestateSessionSource) SetPendingAwakeable(ctx restate.Context, sessionID, awakeableID string) error {
	_, err := restate.Object[SessionState](ctx, SessionObjectName, sessionID, "SetPendingAwakeable").
		Request(awakeableID)
	return err
}
