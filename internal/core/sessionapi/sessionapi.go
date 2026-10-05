// Package sessionapi 是 Session API（对外 REST + SSE）的类型与状态机（契约规范 §2）。
//
// 状态机（唯一权威）：
//
//	session: created → ready → running → sleeping / paused / awaiting_approval
//	         → completed → archived / deleted
//	run:     queued → running → paused / awaiting_approval / frozen
//	         → completed / failed / cancelled
package sessionapi

// SessionPhase 是会话状态机全集。
type SessionPhase string

const (
	PhaseCreated          SessionPhase = "created" // 记录已建、环境未就绪
	PhaseReady            SessionPhase = "ready"   // 沙箱懒创建完成
	PhaseRunning          SessionPhase = "running"
	PhaseSleeping         SessionPhase = "sleeping"
	PhasePaused           SessionPhase = "paused"
	PhaseAwaitingApproval SessionPhase = "awaiting_approval"
	PhaseCompleted        SessionPhase = "completed"
	PhaseArchived         SessionPhase = "archived"
	PhaseDeleted          SessionPhase = "deleted" // tombstone
)

// RunStatus 是 run 状态机全集（frozen 仅存在于 run 级：欠费/预算冻结）。
type RunStatus string

const (
	RunQueued           RunStatus = "queued"
	RunRunning          RunStatus = "running"
	RunPaused           RunStatus = "paused"
	RunAwaitingApproval RunStatus = "awaiting_approval"
	RunFrozen           RunStatus = "frozen"
	RunCompleted        RunStatus = "completed"
	RunFailed           RunStatus = "failed"
	RunCancelled        RunStatus = "cancelled"
)

// ActionName 是 POST /sessions/:id/actions 的动作全集。
type ActionName string

const (
	ActionPause  ActionName = "pause"
	ActionResume ActionName = "resume"
	ActionWake   ActionName = "wake"
	ActionCancel ActionName = "cancel"
	ActionSteer  ActionName = "steer"
)

// AgentConfig 是 agent.config（每次变更 version+1；run 启动时绑定版本）。
type AgentConfig struct {
	Model        string         `json:"model"`
	Instructions string         `json:"instructions"`
	Tools        []string       `json:"tools"`
	MCPServers   []string       `json:"mcp_servers,omitempty"`
	Skills       []string       `json:"skills,omitempty"`
	Environment  SandboxSpec    `json:"environment,omitempty"`
	Budget       map[string]any `json:"budget,omitempty"`
	Version      int            `json:"version"` // 每次变更 +1
}

// SandboxSpec 是 agent.config.environment.sandbox（image/limits/ttl）。
type SandboxSpec struct {
	Image  string            `json:"image"`
	Limits map[string]string `json:"limits,omitempty"` // cpu/mem/disk
	TTL    string            `json:"ttl,omitempty"`
}

// SubmitRunRequest 是 POST /sessions/:id/runs 的请求体（幂等键必带）。
type SubmitRunRequest struct {
	IdempotencyKey string         `json:"-" header:"Idempotency-Key"` // → run_id 去重
	Input          string         `json:"input"`
	Trigger        map[string]any `json:"trigger,omitempty"`
	Queue          bool           `json:"queue,omitempty"` // true 时双开改排队
}

// ActionRequest 是 POST /sessions/:id/actions 的请求体。
type ActionRequest struct {
	Action  ActionName     `json:"action"`
	Payload map[string]any `json:"payload,omitempty"`
}

// CreateAgentRequest 是 POST /orgs/:id/agents 的请求体（config 全量）。
type CreateAgentRequest struct {
	Name   string      `json:"name"`
	Config AgentConfig `json:"config"`
}
