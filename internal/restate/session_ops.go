package restate

import (
	"encoding/json"

	restate "github.com/restatedev/sdk-go"

	"github.com/bingdilotus/chronotope/internal/core/event"
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
