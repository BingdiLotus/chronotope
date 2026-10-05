package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	restate "github.com/restatedev/sdk-go"
)

// ============ ① Run 闭包内分钟级 SSE 长流 ============

// ProbeInput 是 sseprobe 的输入（dur_sec 控制桩 SSE 流时长）。
type ProbeInput struct {
	DurSec int `json:"dur_sec"`
}

// ProbeOutput 记录两个 journaled step 的结果。
type ProbeOutput struct {
	Step1Serial string `json:"step1_serial"` // /once 请求序号：重放则序号不变
	Events      int    `json:"events"`       // 长流中收到的 delta 事件数
}

func sseProbeService() restate.ServiceDefinition {
	return restate.NewService("sseprobe").
		Handler("ProbeTwoSteps", restate.NewServiceHandler[ProbeInput, ProbeOutput](
			func(ctx restate.Context, in ProbeInput) (ProbeOutput, error) {
				// Step 1：已完成的 journaled step——崩溃重放时必须从缓存回放、不重执行
				step1, err := restate.Run(ctx, func(rc restate.RunContext) (string, error) {
					resp, err := http.Get(stubAddr + "/once")
					if err != nil {
						return "", err
					}
					defer resp.Body.Close()
					b, _ := io.ReadAll(resp.Body)
					var m struct {
						Serial int `json:"serial"`
					}
					_ = json.Unmarshal(b, &m)
					return fmt.Sprint(m.Serial), nil
				}, restate.WithName("step1"))
				if err != nil {
					return ProbeOutput{}, err
				}

				// Step 2：分钟级 SSE 长流（模拟 harness /runs → SSE delta 流）
				// RunContext 即普通 context.Context，可在闭包内做任意 HTTP 流式读取。
				step2, err := restate.Run(ctx, func(rc restate.RunContext) (int, error) {
					resp, err := http.Get(fmt.Sprintf("%s/stream?dur=%d", stubAddr, in.DurSec))
					if err != nil {
						return 0, err
					}
					defer resp.Body.Close()
					sc := bufio.NewScanner(resp.Body)
					events := 0
					for sc.Scan() {
						if strings.HasPrefix(sc.Text(), "event: delta") {
							events++
						}
					}
					return events, sc.Err()
				}, restate.WithName("step2-long-stream"))
				if err != nil {
					return ProbeOutput{Step1Serial: step1}, err
				}
				return ProbeOutput{Step1Serial: step1, Events: step2}, nil
			}))
}

// ============ ② awakeable 跨 HTTP resolve ============

// approval 是 Virtual Object（key=会话），单写者串行；PendingAwakeable 存 state。
// 注意（spike 重要发现）：同一 object 的 exclusive handler 在挂起时仍占排他锁，
// resolve 必须来自本对象之外的 invocation（独立 Service），否则 Resolve 永远排队 =
// 死锁。这也正是 worker-架构设计 §2 把 webhook 独立成 Service 的原因。
func approvalObject() restate.ServiceDefinition {
	return restate.NewObject("approval").
		Handler("RequestApproval", restate.NewObjectHandler[string, string](
			func(ctx restate.ObjectContext, _ string) (string, error) {
				a := restate.Awakeable[string](ctx)
				restate.Set(ctx, "pending", a.Id())
				result, err := a.Result() // 挂起：零进程占用，直到跨 HTTP resolve
				if err != nil {
					return "", err
				}
				restate.Clear(ctx, "pending")
				return result, nil
			})).
		Handler("PendingID", restate.NewObjectSharedHandler[string, string](
			func(ctx restate.ObjectSharedContext, _ string) (string, error) {
				id, _ := restate.Get[*string](ctx, "pending")
				if id == nil {
					return "", nil
				}
				return *id, nil
			}))
}

// ResolveRequest 是 approver 服务的输入（resolve 方与挂起方必须不同 invocation）。
type ResolveRequest struct {
	ID      string `json:"id"`
	Payload string `json:"payload"`
}

// approverService 是独立 Service：POST /approver/Resolve → resolve awakeable（幂等）。
func approverService() restate.ServiceDefinition {
	return restate.NewService("approver").
		Handler("Resolve", restate.NewServiceHandler[ResolveRequest, string](
			func(ctx restate.Context, req ResolveRequest) (string, error) {
				if req.ID == "" {
					return "", fmt.Errorf("missing awakeable id")
				}
				restate.ResolveAwakeable[string](ctx, req.ID, req.Payload)
				return "resolved", nil
			}))
}

// ============ ③ child workflow 调用/await ============

// ChildInput 是 childflow 的输入。
type ChildInput struct {
	DelayMs int    `json:"delay_ms"`
	Label   string `json:"label"`
}

// ChildOutput 是 childflow 的结果。
type ChildOutput struct {
	Label  string `json:"label"`
	Serial string `json:"serial"` // 子 workflow 触达桩的请求序号
}

func childWorkflow() restate.ServiceDefinition {
	return restate.NewWorkflow("childflow").
		Handler("run", restate.NewWorkflowHandler[ChildInput, ChildOutput](
			func(ctx restate.WorkflowContext, in ChildInput) (ChildOutput, error) {
				serial, err := restate.Run(ctx, func(rc restate.RunContext) (string, error) {
					resp, err := http.Get(stubAddr + "/once?label=" + in.Label)
					if err != nil {
						return "", err
					}
					defer resp.Body.Close()
					b, _ := io.ReadAll(resp.Body)
					var m struct {
						Serial int `json:"serial"`
					}
					_ = json.Unmarshal(b, &m)
					return fmt.Sprint(m.Serial), nil
				}, restate.WithName("child-touch"))
				if err != nil {
					return ChildOutput{}, err
				}
				if err := restate.Sleep(ctx, time.Duration(in.DelayMs)*time.Millisecond); err != nil {
					return ChildOutput{}, err
				}
				return ChildOutput{Label: in.Label, Serial: serial}, nil
			}))
}

// ParentInput 是 parentflow 的输入。
type ParentInput struct {
	ChildDelayMs int `json:"child_delay_ms"`
}

// ParentOutput 是 parentflow 的结果（含子 workflow 的结果，父的 await 已 journal）。
type ParentOutput struct {
	ChildID string      `json:"child_id"`
	Child   ChildOutput `json:"child"`
}

func parentWorkflow() restate.ServiceDefinition {
	return restate.NewWorkflow("parentflow").
		Handler("run", restate.NewWorkflowHandler[ParentInput, ParentOutput](
			func(ctx restate.WorkflowContext, in ParentInput) (ParentOutput, error) {
				// 子 workflow ID 由父 ID 派生：重放时重复调用被 journal 去重，不会重复派发
				childID := restate.Key(ctx) + "/child"
				child, err := restate.Workflow[ChildOutput](ctx, "childflow", childID, "run").
					Request(ChildInput{DelayMs: in.ChildDelayMs, Label: "of-" + restate.Key(ctx)})
				if err != nil {
					return ParentOutput{}, err
				}
				return ParentOutput{ChildID: childID, Child: child}, nil
			}))
}

// ============ ④ endpoint versioning ============

// VersionInfo 报告当前端点实例的版本与地址（v1/v2 双开时区分路由去向）。
type VersionInfo struct {
	Version string `json:"version"`
	Addr    string `json:"addr"`
}

func versionService() restate.ServiceDefinition {
	return restate.NewService("pingpong").
		Handler("ping", restate.NewServiceHandler[restate.Void, VersionInfo](
			func(ctx restate.Context, _ restate.Void) (VersionInfo, error) {
				return VersionInfo{Version: version, Addr: endpointAddr}, nil
			}))
}

// VersionInput 是 versionflow 的输入（sleep 期间完成 v2 注册，验证在途留 v1）。
type VersionInput struct {
	SleepMs int `json:"sleep_ms"`
}

func versionWorkflow() restate.ServiceDefinition {
	return restate.NewWorkflow("versionflow").
		Handler("run", restate.NewWorkflowHandler[VersionInput, VersionInfo](
			func(ctx restate.WorkflowContext, in VersionInput) (VersionInfo, error) {
				if err := restate.Sleep(ctx, time.Duration(in.SleepMs)*time.Millisecond); err != nil {
					return VersionInfo{}, err
				}
				return VersionInfo{Version: version, Addr: endpointAddr}, nil
			}))
}

// ============ ⑤ journal/state 条目大小限制量级 ============

// SizeInput 指定载荷大小（KB）。
type SizeInput struct {
	KB int `json:"kb"`
}

// SizeOutput 记录各通道的结果/错误（每个 KB 档位一次独立 invocation）。
type SizeOutput struct {
	StateBytes int    `json:"state_bytes"`
	RunBytes   int    `json:"run_bytes"`
	StateErr   string `json:"state_err,omitempty"`
	RunErr     string `json:"run_err,omitempty"`
}

func sizeWorkflow() restate.ServiceDefinition {
	return restate.NewWorkflow("sizeprobe").
		Handler("run", restate.NewWorkflowHandler[SizeInput, SizeOutput](
			func(ctx restate.WorkflowContext, in SizeInput) (SizeOutput, error) {
				payload := strings.Repeat("x", in.KB*1024)
				out := SizeOutput{}

				// 通道 A：workflow state 条目
				if msg := setBlob(ctx, payload); msg != "" {
					out.StateErr = msg
				} else {
					out.StateBytes = len(payload)
				}

				// 通道 B：journal（Run 输出）
				if msg := runBlob(ctx, payload); msg != "" {
					out.RunErr = msg
				} else {
					out.RunBytes = len(payload)
				}
				return out, nil
			}))
}

// setBlob 用 defer/recover 捕获 SDK 对超限 state 的可能 panic（spike 目的：观察行为）。
func setBlob(ctx restate.WorkflowContext, payload string) (errMsg string) {
	defer func() {
		if r := recover(); r != nil {
			errMsg = fmt.Sprintf("panic: %v", r)
		}
	}()
	restate.Set(ctx, "blob", payload)
	got, err := restate.Get[string](ctx, "blob")
	if err != nil {
		return "get: " + err.Error()
	}
	if len(got) != len(payload) {
		return fmt.Sprintf("roundtrip mismatch: got %d bytes, want %d", len(got), len(payload))
	}
	return ""
}

// runBlob 将大输出走 journal（Run 返回值）。
func runBlob(ctx restate.WorkflowContext, payload string) (errMsg string) {
	out, err := restate.Run(ctx, func(rc restate.RunContext) (string, error) {
		return payload, nil
	}, restate.WithName("big-output"))
	if err != nil {
		return err.Error()
	}
	if len(out) != len(payload) {
		return fmt.Sprintf("roundtrip mismatch: got %d bytes, want %d", len(out), len(payload))
	}
	return ""
}
