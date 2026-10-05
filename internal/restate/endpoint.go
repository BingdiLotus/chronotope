package restate

import (
	"log/slog"
	"net/http"
	"os"

	"github.com/restatedev/sdk-go/server"
)

// BuildEndpoint 组装 worker 的 Restate 端点（四个服务，worker-架构设计 §2）：
// session_object（Virtual Object）/ run_workflow（Workflow）/ scheduler（Workflow）/
// webhook（Service）。返回的 http.Handler 由 cmd/worker 挂到 WORKER_ADDR。
//
// 部署注册（spike 实验④验证的运维语义）：
//
//	curl -X POST :9070/deployments -d '{"uri":"http://<worker>","version":"v1","use_http_11":true,"force":true}'
//
// 新 invocation 走最新 version、在途 invocation 留在旧 version 原地跑完。
func BuildEndpoint(deps *Deps) (http.Handler, error) {
	ep := server.NewRestate()
	ep = ep.WithLogger(
		slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}),
		true, // dropReplayLogs：重放日志静默
	)
	ep.Bind(sessionObjectDef()).
		Bind(runWorkflowDef(deps)).
		Bind(schedulerDef(deps)).
		Bind(webhookDef(deps))
	return ep.Handler()
}
