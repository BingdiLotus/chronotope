package restate

import (
	"encoding/json"

	restate "github.com/restatedev/sdk-go"

	"github.com/bingdilotus/chronotope/internal/core/event"
	"github.com/bingdilotus/chronotope/internal/store"
)

// session_ops 工作流：api 侧触发的会话运维动作（MCP 连接 / skill 安装）。
// worker 是唯一事件写者（架构约束）：事件在此发射后转对象更新状态——
// 时序保证：事件先于状态变更（订阅端所见事件序与事实一致）。

const SessionOpsName = "session_ops"

type sessionOpsDeps struct {
	Store Store
}

// Service（非 Workflow）：运维动作可重复调用（workflow 方法单次调用语义——
// 重复安装 skill 会 409，w6 e2e 实证）。
func sessionOpsDef(deps *Deps) restate.ServiceDefinition {
	return restate.NewService(SessionOpsName).
		Handler("ConnectMCP", restate.NewServiceHandler[connectMCPInput, restate.Void](func(ctx restate.Context, in connectMCPInput) (restate.Void, error) {
			return restate.Void{}, connectMCPWorkflow(ctx, deps, in)
		})).
		Handler("InstallSkill", restate.NewServiceHandler[installSkillInput, restate.Void](func(ctx restate.Context, in installSkillInput) (restate.Void, error) {
			return restate.Void{}, installSkillWorkflow(ctx, deps, in)
		})).
		Handler("CreateCheckpoint", restate.NewServiceHandler[createCheckpointInput, restate.Void](func(ctx restate.Context, in createCheckpointInput) (restate.Void, error) {
			return restate.Void{}, createCheckpointWorkflow(ctx, deps, in)
		}))
}

type connectMCPInput struct {
	SessionID string `json:"session_id"`
	Server    string `json:"server"`
	URL       string `json:"url"`
}

type installSkillInput struct {
	SessionID string `json:"session_id"`
	Name      string `json:"name"`
}

type createCheckpointInput struct {
	SessionID    string `json:"session_id"`
	CheckpointID string `json:"checkpoint_id"`
}

// createCheckpointWorkflow 时间旅行（期 2）：checkpoint = 事件水位 + 沙箱快照
// （空间面）——checkpoint(session, seq) 钉住「时间 × 空间」坐标。事件先于
// 行（订阅端所见序一致）。
func createCheckpointWorkflow(ctx restate.Context, deps *Deps, in createCheckpointInput) error {
	seq, err := deps.Store.LatestEventSeq(ctx, in.SessionID)
	if err != nil {
		return err
	}
	snapshotRef := ""
	if state, sErr := deps.Sessions.GetState(ctx, in.SessionID); sErr == nil && state.SandboxID != "" {
		snapshotRef, _ = deps.Executor.Snapshot(ctx, state.SandboxID) // 无快照能力/失败 → 仅时间坐标
	}
	payload, _ := json.Marshal(map[string]any{"checkpoint_id": in.CheckpointID, "seq": seq, "snapshot_ref": snapshotRef})
	if _, err := deps.Store.AppendEvent(ctx, in.SessionID, "", event.SessionCheckpoint, payload, in.SessionID+":checkpoint:"+in.CheckpointID); err != nil {
		return err
	}
	_, err = deps.Store.CreateCheckpoint(ctx, store.Checkpoint{
		ID: in.CheckpointID, SessionID: in.SessionID, Seq: seq, SnapshotRef: snapshotRef,
	})
	return err
}

func connectMCPWorkflow(ctx restate.Context, deps *Deps, in connectMCPInput) error {
	payload, _ := json.Marshal(map[string]any{"server": in.Server, "url": in.URL})
	if _, err := deps.Store.AppendEvent(ctx, in.SessionID, "", event.MCPConnected, payload, in.SessionID+":mcp:connected:"+in.Server); err != nil {
		return err
	}
	_, err := restate.Object[SessionState](ctx, SessionObjectName, in.SessionID, "ConnectMCP").
		Request(MCPConnectRequest{Server: in.Server, URL: in.URL})
	return err
}

func installSkillWorkflow(ctx restate.Context, deps *Deps, in installSkillInput) error {
	payload, _ := json.Marshal(map[string]any{"name": in.Name})
	if _, err := deps.Store.AppendEvent(ctx, in.SessionID, "", event.SkillInstall, payload, in.SessionID+":skill:install:"+in.Name); err != nil {
		return err
	}
	_, err := restate.Object[SessionState](ctx, SessionObjectName, in.SessionID, "AddSkill").
		Request(in.Name)
	return err
}
