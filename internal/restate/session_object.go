package restate

import (
	"fmt"

	restate "github.com/restatedev/sdk-go"

	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
)

// Restate 服务名（worker-架构设计 §2，与部署注册/ingress 路径一致）。
const (
	SessionObjectName = "session_object"
	RunWorkflowName   = "run_workflow"
	SchedulerName     = "scheduler"
	WebhookName       = "webhook"
)

// SessionState 是 session_object 的持久状态（分层原则：Restate 只放协调态——
// 游标/引用/版本/小配置；大载荷在 PG/MinIO，worker-架构设计 §4）。
type SessionState struct {
	Phase               sessionapi.SessionPhase  `json:"phase"`
	AgentConfig         sessionapi.AgentConfig   `json:"agent_config"`
	LastRunID           string                   `json:"last_run_id,omitempty"`
	PendingAwakeable    string                   `json:"pending_awakeable,omitempty"`     // HITL 审批槽
	PendingActionDigest string                   `json:"pending_action_digest,omitempty"` // 待审批动作摘要（精确绑定，评审 #5）
	FrozenAwakeable     string                   `json:"frozen_awakeable,omitempty"`      // 欠费冻结槽（与审批独立）
	Participants        []sessionapi.Participant `json:"participants,omitempty"`          // 群聊成员（非空 = 群聊会话；moderator 主持）
	MCP                 []MCPConnection          `json:"mcp,omitempty"`                   // MCP 连接（tools 懒缓存）
	Skills              []string                 `json:"skills,omitempty"`                // 已安装 skill 名（沙箱 skills/<name>/）
	CancelRequested     bool                     `json:"cancel_requested,omitempty"`      // 取消请求（run 检查点生效，非抢占——评审 #6）
	SandboxID           string                   `json:"sandbox_id,omitempty"`            // 会话作用域沙箱（懒创建，W2）
}

const sessionStateKey = "state"

// WakeInput 是 Wake 的输入（W1：调度器触发；payload 留给 cron 唤醒载荷）。
type WakeInput struct {
	Payload map[string]any `json:"payload,omitempty"`
}

// sessionObjectDef 注册会话状态机（Virtual Object，key=session_id，单写者串行——
// 每 session 同时只有一个活跃 run，worker-架构设计 §4）。
func sessionObjectDef() restate.ServiceDefinition {
	return restate.NewObject(SessionObjectName).
		Handler("Create", restate.NewObjectHandler[sessionapi.AgentConfig, SessionState](createSessionObject)).
		Handler("GetState", restate.NewObjectSharedHandler[restate.Void, SessionState](getSessionState)).
		Handler("Wake", restate.NewObjectHandler[WakeInput, SessionState](wakeSession)).
		Handler("Pause", restate.NewObjectHandler[restate.Void, SessionState](pauseSession)).
		Handler("Resume", restate.NewObjectHandler[restate.Void, SessionState](resumeSession)).
		Handler("AttachSandbox", restate.NewObjectHandler[string, SessionState](attachSandbox)).
		Handler("ClearSandbox", restate.NewObjectHandler[restate.Void, SessionState](clearSandbox)).
		Handler("Cancel", restate.NewObjectHandler[restate.Void, SessionState](cancelSessionRun)).
		Handler("SetPendingAwakeable", restate.NewObjectHandler[SetPendingApprovalInput, SessionState](setPendingAwakeable)).
		Handler("SetFrozenAwakeable", restate.NewObjectHandler[string, SessionState](setFrozenAwakeable)).
		Handler("Unfreeze", restate.NewObjectHandler[restate.Void, SessionState](unfreezeSession)).
		Handler("SetParticipants", restate.NewObjectHandler[[]sessionapi.Participant, SessionState](setParticipants)).
		Handler("ConnectMCP", restate.NewObjectHandler[MCPConnectRequest, SessionState](connectMCP)).
		Handler("AddSkill", restate.NewObjectHandler[string, SessionState](addSkill))
}

// attachSandbox 记录会话作用域沙箱（懒创建后回填；幂等：同值重复设置无害）。
func attachSandbox(ctx restate.ObjectContext, sandboxID string) (SessionState, error) {
	state, err := getSessionState(ctx, restate.Void{})
	if err != nil {
		return SessionState{}, err
	}
	if state.SandboxID != sandboxID {
		state.SandboxID = sandboxID
		restate.Set(ctx, sessionStateKey, state)
	}
	return state, nil
}

// cancelSessionRun 置取消标志（runLoop 下一步检查点生效——非抢占式取消）。
func cancelSessionRun(ctx restate.ObjectContext, _ restate.Void) (SessionState, error) {
	state, err := getSessionState(ctx, restate.Void{})
	if err != nil {
		return SessionState{}, err
	}
	state.CancelRequested = true
	restate.Set(ctx, sessionStateKey, state)
	return state, nil
}

// clearSandbox 清除会话沙箱绑定（沙箱已销毁 → 下次 ensure 重建，快照恢复）。
func clearSandbox(ctx restate.ObjectContext, _ restate.Void) (SessionState, error) {
	state, err := getSessionState(ctx, restate.Void{})
	if err != nil {
		return SessionState{}, err
	}
	state.SandboxID = ""
	restate.Set(ctx, sessionStateKey, state)
	return state, nil
}

// setPendingAwakeable 记录挂起的审批 awakeable（HITL；webhook resolve 前可查）。
// SetPendingApprovalInput 是挂起审批的槽与动作摘要（评审 #5：精确绑定动作）。
type SetPendingApprovalInput struct {
	AwakeableID  string `json:"awakeable_id"`
	ActionDigest string `json:"action_digest"`
}

func setPendingAwakeable(ctx restate.ObjectContext, in SetPendingApprovalInput) (SessionState, error) {
	state, err := getSessionState(ctx, restate.Void{})
	if err != nil {
		return SessionState{}, err
	}
	if state.PendingAwakeable != in.AwakeableID || state.PendingActionDigest != in.ActionDigest {
		state.PendingAwakeable = in.AwakeableID
		state.PendingActionDigest = in.ActionDigest
		restate.Set(ctx, sessionStateKey, state)
	}
	return state, nil
}

// setFrozenAwakeable 记录欠费冻结的 awakeable id（与审批槽独立，避免互踩）。
func setFrozenAwakeable(ctx restate.ObjectContext, awakeableID string) (SessionState, error) {
	state, err := getSessionState(ctx, restate.Void{})
	if err != nil {
		return SessionState{}, err
	}
	if state.FrozenAwakeable != awakeableID {
		state.FrozenAwakeable = awakeableID
		restate.Set(ctx, sessionStateKey, state)
	}
	return state, nil
}

// unfreezeSession 解析冻结：resolve FrozenAwakeable 并清槽（充值后继续执行）。
// 对象内部可 resolve awakeable（Context 操作在对象 handler 中合法）。
func unfreezeSession(ctx restate.ObjectContext, _ restate.Void) (SessionState, error) {
	state, err := getSessionState(ctx, restate.Void{})
	if err != nil {
		return SessionState{}, err
	}
	if state.FrozenAwakeable == "" {
		return SessionState{}, fmt.Errorf("session %s 无挂起冻结", restate.Key(ctx))
	}
	restate.ResolveAwakeable[string](ctx, state.FrozenAwakeable, "unfrozen")
	state.FrozenAwakeable = ""
	restate.Set(ctx, sessionStateKey, state)
	return state, nil
}

// MCPConnection 是 MCP 服务器连接（tools 列表懒缓存——run 时 worker 建连刷新）。
type MCPConnection struct {
	Server string        `json:"server"`
	URL    string        `json:"url"`
	Tools  []MCPToolSpec `json:"tools,omitempty"`
}

// MCPConnectRequest 是 POST /sessions/:id/mcp 的载荷。
type MCPConnectRequest struct {
	Server string `json:"server"`
	URL    string `json:"url"`
}

// connectMCP 注册 MCP 连接（同 server 幂等覆盖）。
func connectMCP(ctx restate.ObjectContext, req MCPConnectRequest) (SessionState, error) {
	state, err := getSessionState(ctx, restate.Void{})
	if err != nil {
		return SessionState{}, err
	}
	for i := range state.MCP {
		if state.MCP[i].Server == req.Server {
			state.MCP[i].URL = req.URL
			state.MCP[i].Tools = nil // URL 变更后工具缓存失效
			restate.Set(ctx, sessionStateKey, state)
			return state, nil
		}
	}
	state.MCP = append(state.MCP, MCPConnection{Server: req.Server, URL: req.URL})
	restate.Set(ctx, sessionStateKey, state)
	return state, nil
}

// addSkill 记录已装 skill（幂等去重）。
func addSkill(ctx restate.ObjectContext, name string) (SessionState, error) {
	state, err := getSessionState(ctx, restate.Void{})
	if err != nil {
		return SessionState{}, err
	}
	for _, s := range state.Skills {
		if s == name {
			return state, nil
		}
	}
	state.Skills = append(state.Skills, name)
	restate.Set(ctx, sessionStateKey, state)
	return state, nil
}

// setParticipants 设置群聊成员（幂等：同值重复设置无害）。
func setParticipants(ctx restate.ObjectContext, participants []sessionapi.Participant) (SessionState, error) {
	state, err := getSessionState(ctx, restate.Void{})
	if err != nil {
		return SessionState{}, err
	}
	state.Participants = participants
	restate.Set(ctx, sessionStateKey, state)
	return state, nil
}

func createSessionObject(ctx restate.ObjectContext, cfg sessionapi.AgentConfig) (SessionState, error) {
	if cfg.Model == "" {
		return SessionState{}, restate.ToTerminalError(fmt.Errorf("agent config missing model"))
	}
	state := SessionState{Phase: sessionapi.PhaseReady, AgentConfig: cfg}
	restate.Set(ctx, sessionStateKey, state)
	return state, nil
}

func getSessionState(ctx restate.ObjectSharedContext, _ restate.Void) (SessionState, error) {
	state, err := restate.Get[SessionState](ctx, sessionStateKey)
	if err != nil {
		return SessionState{}, err
	}
	return state, nil
}

func wakeSession(ctx restate.ObjectContext, in WakeInput) (SessionState, error) {
	state, err := getSessionState(ctx, restate.Void{})
	if err != nil {
		return SessionState{}, err
	}
	next, err := wakeTransition(state)
	if err != nil {
		return SessionState{}, restate.ToTerminalError(err)
	}
	if next.Phase != state.Phase {
		restate.Set(ctx, sessionStateKey, next)
	}
	return next, nil
}

func pauseSession(ctx restate.ObjectContext, _ restate.Void) (SessionState, error) {
	state, err := getSessionState(ctx, restate.Void{})
	if err != nil {
		return SessionState{}, err
	}
	next := pauseTransition(state)
	if next.Phase != state.Phase {
		restate.Set(ctx, sessionStateKey, next)
	}
	return next, nil
}

func resumeSession(ctx restate.ObjectContext, _ restate.Void) (SessionState, error) {
	state, err := getSessionState(ctx, restate.Void{})
	if err != nil {
		return SessionState{}, err
	}
	next := resumeTransition(state)
	if next.Phase != state.Phase {
		restate.Set(ctx, sessionStateKey, next)
	}
	return next, nil
}

// 纯状态迁移（契约规范 §2 状态机唯一权威；handler 只做读改写，迁移逻辑独立可测）：
//
//	session: created → ready → running → sleeping / paused / awaiting_approval
//	         → completed → archived / deleted
func wakeTransition(state SessionState) (SessionState, error) {
	if state.Phase == "" {
		return SessionState{}, fmt.Errorf("session state not initialized")
	}
	// 唤醒即执行：ready/sleeping/paused → running；running 幂等保持
	if state.Phase == sessionapi.PhaseReady ||
		state.Phase == sessionapi.PhaseSleeping ||
		state.Phase == sessionapi.PhasePaused {
		state.Phase = sessionapi.PhaseRunning
	}
	return state, nil
}

func pauseTransition(state SessionState) SessionState {
	if state.Phase == sessionapi.PhaseRunning {
		state.Phase = sessionapi.PhasePaused
	}
	return state
}

func resumeTransition(state SessionState) SessionState {
	if state.Phase == sessionapi.PhasePaused {
		state.Phase = sessionapi.PhaseRunning
	}
	return state
}
