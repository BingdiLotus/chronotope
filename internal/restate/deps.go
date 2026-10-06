package restate

import (
	"context"
	"encoding/json"
	"time"

	restate "github.com/restatedev/sdk-go"

	"github.com/bingdilotus/chronotope/internal/core/event"
	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
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
	// UpdateRunStatus：worker 是 run 终态的记账者（api 中途崩溃后 runs 行仍收敛——
	// 事件才是真相，状态行是投影；chaos 套件 kill9-api 实证）。
	UpdateRunStatus(ctx context.Context, runID string, status sessionapi.RunStatus) error
	// org 级预算（三级熔断 ②）：org 配额 + 当日消费（worker 校验冻结）。
	GetOrg(ctx context.Context, orgID string) (*store.Org, error)
	OrgDailyUsage(ctx context.Context, orgID string, day time.Time) (tokens int64, computeSeconds float64, err error)
	// 子 Agent（W6）：父会话/agent 元数据 + 子会话建行（确定性 id，journaled）。
	GetSession(ctx context.Context, sessionID string) (*store.Session, error)
	GetAgent(ctx context.Context, agentID string) (*store.Agent, error)
	CreateSession(ctx context.Context, id, orgID, agentID string) error
	// 分层记忆（边界语义 §7）：主题摘要与长期记忆条目（派生数据，worker 唯一写入）。
	LatestSummary(ctx context.Context, sessionID, topic string) (*store.Summary, error)
	CreateSummary(ctx context.Context, sum store.Summary) (bool, error)
	CreateMemoryItem(ctx context.Context, item store.MemoryItem) (bool, error)
	ListMemoryItems(ctx context.Context, sessionID, topic string, limit int) ([]store.MemoryItem, error)
}

// SessionSource 是会话状态的读写接缝：run_workflow 经它读/回填 session_object
// （Virtual Object 单写者串行）；测试以 fake 替换，避免在单测中依赖 Restate 语境。
type SessionSource interface {
	GetState(ctx restate.Context, sessionID string) (SessionState, error)
	// AttachSandbox 回填会话作用域沙箱 id（懒创建后调用；幂等）。
	AttachSandbox(ctx restate.Context, sessionID, sandboxID string) error
	// SetPendingAwakeable 记录挂起的审批 awakeable id（HITL；resolve 前可查）。
	SetPendingAwakeable(ctx restate.Context, sessionID, awakeableID string) error
	// Create 初始化子会话对象状态（子 Agent 派发；幂等对象调用）。
	Create(ctx restate.Context, sessionID string, cfg sessionapi.AgentConfig) error
	// SetFrozenAwakeable 记录欠费冻结的 awakeable id（与审批槽独立，避免互踩）。
	SetFrozenAwakeable(ctx restate.Context, sessionID, awakeableID string) error
	// Unfreeze 解析冻结：resolve FrozenAwakeable 并清槽（充值后继续执行）。
	Unfreeze(ctx restate.Context, sessionID string) error
}

// Deps 是 worker 服务层的依赖集（worker-架构设计 §1：HC 客户端 / EC 客户端 /
// EM+ST 经 Store / MO 后置）。
type Deps struct {
	Store    Store
	Harness  Harness
	Executor Executor
	Sessions SessionSource
	// ConsolidateThreshold 是记忆消化触发的消息数阈值（默认 40；测试/演示可调小）。
	ConsolidateThreshold int
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

// Create 初始化子会话对象状态（子 Agent 派发；对象调用幂等）。
func (RestateSessionSource) Create(ctx restate.Context, sessionID string, cfg sessionapi.AgentConfig) error {
	_, err := restate.Object[SessionState](ctx, SessionObjectName, sessionID, "Create").
		Request(cfg)
	return err
}

// SetFrozenAwakeable 记录欠费冻结的 awakeable id（与审批槽独立）。
func (RestateSessionSource) SetFrozenAwakeable(ctx restate.Context, sessionID, awakeableID string) error {
	_, err := restate.Object[SessionState](ctx, SessionObjectName, sessionID, "SetFrozenAwakeable").
		Request(awakeableID)
	return err
}

// Unfreeze 解析冻结（充值后继续执行；对象内部 resolve 并清槽）。
func (RestateSessionSource) Unfreeze(ctx restate.Context, sessionID string) error {
	_, err := restate.Object[SessionState](ctx, SessionObjectName, sessionID, "Unfreeze").
		Request(restate.Void{})
	return err
}
