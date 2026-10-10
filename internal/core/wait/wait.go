// Package wait 是等待收敛与调度判定的领域类型（期 7：从 store 下沉——
// 期 6 的 WaitKind/SchedulerHint/Decision 是内核语义而非存储行）。
package wait

import (
	"encoding/json"
)

// Kind 等待类型（Holon 四 wake kind 收敛——期 6 ①）。
type Kind string

// Kind 常量。
const (
	Timer         Kind = "timer"
	TaskResult    Kind = "task_result"
	External      Kind = "external"
	OperatorInput Kind = "operator_input"
)

// SchedulerHint 调度提示（层 0 类型——DECIDE 的机器可读结论）。
type SchedulerHint string

// SchedulerHint 常量（M2 DECIDE 先行——触发器只消费提示不判断业务）。
const (
	HintRun    SchedulerHint = "run"
	HintWait   SchedulerHint = "wait"
	HintAsk    SchedulerHint = "ask"
	HintReplan SchedulerHint = "replan"
	HintRepair SchedulerHint = "repair"
	HintQuiet  SchedulerHint = "quiet"
)

// Decision 是 DECIDE 的判定结果（层 1 策略缝的契约）。
type Decision struct {
	Hint   SchedulerHint `json:"hint"`
	Reason string        `json:"reason"`
}

// DecisionJSON 序列化（journaled 判定事件用）。
func (d Decision) DecisionJSON() json.RawMessage {
	b, _ := json.Marshal(d)
	return b
}
