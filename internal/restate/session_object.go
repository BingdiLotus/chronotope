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
	Phase            sessionapi.SessionPhase `json:"phase"`
	AgentConfig      sessionapi.AgentConfig  `json:"agent_config"`
	LastRunID        string                  `json:"last_run_id,omitempty"`
	PendingAwakeable string                  `json:"pending_awakeable,omitempty"`
	SandboxID        string                  `json:"sandbox_id,omitempty"` // 会话作用域沙箱（懒创建，W2）
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
		Handler("SetPendingAwakeable", restate.NewObjectHandler[string, SessionState](setPendingAwakeable))
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

// setPendingAwakeable 记录挂起的审批 awakeable（HITL；webhook resolve 前可查）。
func setPendingAwakeable(ctx restate.ObjectContext, awakeableID string) (SessionState, error) {
	state, err := getSessionState(ctx, restate.Void{})
	if err != nil {
		return SessionState{}, err
	}
	if state.PendingAwakeable != awakeableID {
		state.PendingAwakeable = awakeableID
		restate.Set(ctx, sessionStateKey, state)
	}
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
