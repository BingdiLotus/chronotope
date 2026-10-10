package restate

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/bingdilotus/chronotope/internal/store"
	"time"

	restate "github.com/restatedev/sdk-go"

	"github.com/bingdilotus/chronotope/internal/core/event"
)

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ScheduleInput 是 scheduler workflow 的输入。
// W3 一次性形态：延迟唤醒（durable timer → session_object.Wake → child run → 回睡）；
// cron 表与时区语义（边界语义 §5）后置，输入形状已预留 payload。
type ScheduleInput struct {
	SessionID string         `json:"session_id"`
	DelayMs   int64          `json:"delay_ms"`
	Payload   map[string]any `json:"payload,omitempty"`
}

// ScheduleOutput 是 scheduler 的执行结果。
type ScheduleOutput struct {
	Woken   bool   `json:"woken"`
	RunID   string `json:"run_id"`
	Final   string `json:"final"`
	Decided string `json:"decided,omitempty"` // M2 DECIDE 结论（quiet/wait/ask 时）
	Reason  string `json:"reason,omitempty"`
}

// schedulerDef 注册调度器（Workflow，key=schedule_id；durable timer 跨重启，worker-架构设计 §2）。
func schedulerDef(deps *Deps) restate.ServiceDefinition {
	return restate.NewWorkflow(SchedulerName).
		Handler("run", restate.NewWorkflowHandler[ScheduleInput, ScheduleOutput](
			func(ctx restate.WorkflowContext, in ScheduleInput) (ScheduleOutput, error) {
				// 期 6 ①：WaitFor 收敛——timer 等待注册 intent/expect（为什么停 +
				// 等什么条件——DECIDE 可判定的地基）
				_ = deps.Store.RegisterWait(ctx, in.SessionID, store.WaitTimer, restate.Key(ctx),
					"定时唤醒", fmt.Sprintf("delay=%dms payload=%s", in.DelayMs, in.Payload["schedule_id"]))
				defer func() { _ = deps.Store.ResolveWait(ctx, in.SessionID, store.WaitTimer, restate.Key(ctx)) }()

				// 到点唤醒：durable timer（跨重启存活）
				if err := restate.Sleep(ctx, time.Duration(in.DelayMs)*time.Millisecond); err != nil {
					return ScheduleOutput{}, err
				}
				// M2 DECIDE 先行：先判定本轮是否值得运行（run/wait/quiet——
				// 定时器到点不再直接等价「必须让 Agent 工作」）；判定 journaled
				// ——「不作为的可问责性」（每次不运行都有依据与记录）
				decision := store.Decision{Hint: store.HintRun, Reason: "no policy"}
				if deps.SchedulerPolicy != nil {
					decision = deps.SchedulerPolicy.Decide(ctx, in.SessionID)
				}
				emit := &Emitter{Store: deps.Store}
				_ = emit.Emit(ctx, in.SessionID, "", 0, event.SessionWoken, "woken", restate.Key(ctx), map[string]any{
					"schedule": restate.Key(ctx), "phase": "decide",
				})

				_, err := restate.Object[SessionState](ctx, SessionObjectName, in.SessionID, "Wake").
					Request(WakeInput{Payload: in.Payload})
				if err != nil {
					return ScheduleOutput{}, err
				}
				_ = emit.Emit(ctx, in.SessionID, "", 0, event.SchedulerDecide, "decide", restate.Key(ctx), map[string]any{
					"schedule": restate.Key(ctx), "hint": string(decision.Hint),
					"reason": decision.Reason,
				})
				if decision.Hint != store.HintRun {
					// quiet/wait/ask：不派发 child run——零 token 消耗的判定
					//（经济学基线的 DECIDE 节省实证）
					return ScheduleOutput{Decided: string(decision.Hint), Reason: decision.Reason}, nil
				}

				// 执行：child run（run_workflow 递归复用——可组合主张的兑现）。
				// run id 非确定性生成必须 journaled（Run 闭包内），重放回放同一 id。
				input, _ := in.Payload["input"].(string)
				suffix, err := restate.Run(ctx, func(rc restate.RunContext) (string, error) {
					return randHex(8), nil
				}, restate.WithName("run-id"))
				if err != nil {
					return ScheduleOutput{}, err
				}
				runID := "r_" + suffix
				if _, err := deps.Store.CreateRun(ctx, runID, in.SessionID, nil, map[string]any{
					"trigger": "schedule", "schedule_id": restate.Key(ctx),
				}); err != nil {
					return ScheduleOutput{}, err
				}
				out, err := restate.Workflow[RunOutput](ctx, RunWorkflowName, runID, "run").
					Request(RunInput{SessionID: in.SessionID, Input: input})
				if err != nil {
					return ScheduleOutput{}, err
				}

				// 回睡：休眠唤醒循环（下次 schedule 再唤醒）
				if _, err := restate.Object[SessionState](ctx, SessionObjectName, in.SessionID, "Pause").
					Request(restate.Void{}); err != nil {
					return ScheduleOutput{}, err
				}
				return ScheduleOutput{Woken: true, RunID: runID, Final: out.Final}, nil
			}))
}
