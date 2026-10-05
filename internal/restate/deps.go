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
}

// SessionSource 是会话状态的读取接缝：run_workflow 经它读 session_object
// （Virtual Object 单写者串行）；测试以 fake 替换，避免在单测中依赖 Restate 语境。
type SessionSource interface {
	GetState(ctx restate.Context, sessionID string) (SessionState, error)
}

// Deps 是 worker 服务层的依赖集（worker-架构设计 §1：HC 客户端 / EM+ST 经 Store / MO 后置）。
type Deps struct {
	Store    Store
	Harness  Harness
	Sessions SessionSource
}

// RestateSessionSource 是生产实现：经 Restate virtual object 调用读会话状态。
type RestateSessionSource struct{}

// GetState 读 session_object 状态（未初始化时返回零值状态，由调用方校验）。
func (RestateSessionSource) GetState(ctx restate.Context, sessionID string) (SessionState, error) {
	return restate.Object[SessionState](ctx, SessionObjectName, sessionID, "GetState").
		Request(restate.Void{})
}
