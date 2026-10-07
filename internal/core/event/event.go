// Package event 是事件 schema 的 Go 类型（契约规范 §5）。
//
// 纪律：事件 append-only，只插入与 tombstone，不更新；seq 每 session 单调（PG
// BIGSERIAL）且允许 gap（dedupe 冲突烧号/重放跳过所致，after=seq 与 UI 必须容忍）；
// 同 session 按 seq 排序，同 run 内逻辑顺序由 (run_id, step, kind) 表达；payload 带 v 字段。
package event

import (
	"encoding/json"
	"fmt"
	"time"
)

// Type 是事件类型全集（契约规范 §5，只增不改）。
type Type string

const (
	// 运行
	RunStarted   Type = "run.started"
	RunCompleted Type = "run.completed"
	RunFailed    Type = "run.failed"
	RunCancelled Type = "run.cancelled"

	// 循环
	LLMCall        Type = "llm.call"
	ToolCall       Type = "tool.call"
	SandboxExec    Type = "sandbox.exec"
	MCPCall        Type = "mcp.call"
	StepJournaled  Type = "step.journaled"
	EventTruncated Type = "event.truncated"

	// 控制
	RunPaused           Type = "run.paused"
	RunResumed          Type = "run.resumed"
	RunAwaitingApproval Type = "run.awaiting_approval"
	RunFrozen           Type = "run.frozen"
	RunUnfrozen         Type = "run.unfrozen"
	SessionWoken        Type = "session.woken"

	// 能力
	SkillInstall  Type = "skill.install"
	AuditApproval Type = "audit.approval"
	// 期 3 §B：审批策略路由审计
	AuditApprovalDenied  Type = "audit.approval_denied"
	AuditApprovalExpired Type = "audit.approval_expired"
	RunCanceled          Type = "run.canceled"
	// 时间旅行（正式版架构 期 2）
	SessionCheckpoint Type = "session.checkpoint"  // 时间坐标：seq + 沙箱快照引用
	SessionRolledBack Type = "session.rolled_back" // 回退：投影截断 + 追加（事件轴不可变）
	SessionForked     Type = "session.forked"      // 分支派生：父指针 + 新会话
	MCPConnected      Type = "mcp.connected"
	MCPUpdated        Type = "mcp.updated"

	// 多 Agent
	SubagentSpawned   Type = "subagent.spawned"
	SubagentCompleted Type = "subagent.completed"
	GroupTurn         Type = "group.turn"

	// 记忆
	MemoryConsolidated Type = "memory.consolidated"

	// 治理
	AuditToolDenied Type = "audit.tool_denied"
	BudgetExceeded  Type = "budget.exceeded"
)

// Valid 校验事件类型是否为契约全集成员。
func (t Type) Valid() bool {
	switch t {
	case RunStarted, RunCompleted, RunFailed, RunCancelled,
		LLMCall, ToolCall, SandboxExec, MCPCall, StepJournaled, EventTruncated,
		RunPaused, RunResumed, RunAwaitingApproval, RunFrozen, RunUnfrozen, SessionWoken,
		SkillInstall, MCPConnected, MCPUpdated, AuditApproval, AuditApprovalDenied, AuditApprovalExpired, RunCanceled,
		SessionCheckpoint, SessionRolledBack, SessionForked,
		SubagentSpawned, SubagentCompleted, GroupTurn,
		MemoryConsolidated,
		AuditToolDenied, BudgetExceeded:
		return true
	}
	return false
}

// Event 是 events 表的行模型（append-only，单一真相：审计/时间轴回放/计量三用）。
type Event struct {
	ID        int64           `json:"id,omitempty"`
	SessionID string          `json:"session_id"`
	RunID     string          `json:"run_id"`
	Seq       int64           `json:"seq"` // 每 session 单调，允许 gap
	Type      Type            `json:"type"`
	Payload   json.RawMessage `json:"payload"` // 必须携带 v 字段（契约规范 §5）
	At        time.Time       `json:"at"`
}

// DedupeKey 生成事件去重键：run_id:step:kind[:tool]（契约规范 §5）。
// 重放重复发射时靠它幂等吞掉；tool 为空则省略该段。
func DedupeKey(runID string, step int, kind, tool string) string {
	k := fmt.Sprintf("%s:%d:%s", runID, step, kind)
	if tool != "" {
		k += ":" + tool
	}
	return k
}

// LLMUsage 是 llm.call 事件的 usage 载荷（断流时允许 usage_partial:true，契约规范 §3）。
type LLMUsage struct {
	TokensIn     int  `json:"tokens_in"`
	TokensOut    int  `json:"tokens_out"`
	UsagePartial bool `json:"usage_partial,omitempty"`
}
