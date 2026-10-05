// chronotope-worker：Restate 服务端点（核心）。
//
// worker 是「无状态的循环协调者」（worker-架构设计 §0）：进程不持有任何持久状态，
// 只做编排（run_workflow 主循环）、分发（API→harness / 代码→executor / 控制→awakeable /
// MCP→MCP 客户端）、记账（journal + 事件投影，事件唯一写入者）。崩溃即重启，由 Restate
// 重放续跑。
//
// 骨架阶段（W1 D3–D4）：进程可起 + /healthz。Restate SDK 接入待 W1 D1 spike 结论
// （落地方案 §4：五项原语验证，任一阻断则切 Temporal Go）。四个服务的空壳注册见下方 TODO。
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
)

func main() {
	addr := flag.String("addr", envOr("WORKER_ADDR", ":9080"), "listen address")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": "chronotope-worker"})
	})

	// TODO(D1 spike 后)：restate.NewEndpoint() 注册四个服务（worker-架构设计 §2）：
	//   session_object  Virtual Object, key=session_id —— 会话状态机（phase/plan/记忆游标）
	//   run_workflow    Workflow,        key=run_id     —— agent 主循环 + RunStep journal 缓存
	//   scheduler       Workflow,        key=schedule_id —— cron → durable timer → session_object.wake()
	//   webhook         Service,         —              —— resolve awakeable（HITL 审批）
	//
	// 主循环伪代码见 worker-架构设计 §3；缓存键权威定义（journal 位置 = run_id + step 名）
	// 见 internal/restate.CacheKey；/runs 协议与 SSE 帧见 internal/core/runs。
	// harness 客户端（POST /runs → SSE，边收 delta 边 emit）亦在 spike 后接入。

	log.Printf("chronotope-worker listening on %s", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
