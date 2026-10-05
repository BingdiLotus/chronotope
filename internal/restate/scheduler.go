package restate

import (
	"time"

	restate "github.com/restatedev/sdk-go"
)

// ScheduleInput 是 scheduler workflow 的输入。
// W1 简化形态：延迟唤醒（durable timer → session_object.Wake）；
// W3 接 cron 表（契约规范 §2 schedules + 边界语义 §5 时区语义）。
type ScheduleInput struct {
	SessionID string `json:"session_id"`
	DelayMs   int64  `json:"delay_ms"`
}

// schedulerDef 注册调度器（Workflow，key=schedule_id；durable timer 跨重启，worker-架构设计 §2）。
func schedulerDef() restate.ServiceDefinition {
	return restate.NewWorkflow(SchedulerName).
		Handler("run", restate.NewWorkflowHandler[ScheduleInput, SessionState](
			func(ctx restate.WorkflowContext, in ScheduleInput) (SessionState, error) {
				if err := restate.Sleep(ctx, time.Duration(in.DelayMs)*time.Millisecond); err != nil {
					return SessionState{}, err
				}
				// 到点唤醒：session_object.Wake() 迁移 phase → 由 api 提交新一轮 run（W3）
				return restate.Object[SessionState](ctx, SessionObjectName, in.SessionID, "Wake").
					Request(WakeInput{Payload: map[string]any{"source": "scheduler"}})
			}))
}
