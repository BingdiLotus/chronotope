// chronotope-api：HTTP/SSE 网关（REST + SSE + 事件投影 + 计量 + outbox + 幂等）。
//
// 骨架阶段（W1 D3–D4）：按 契约规范 §2 把 Session API 路由铺开，handler 统一返回
// 501 占位；各端点实现随 W1–W4 逐周补全。SSE 时间轴的分发策略见 internal/events
// （PG LISTEN/NOTIFY 只发 (session_id, seq) 提示，api 回查后推送，落地方案 §5）。
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

func main() {
	addr := flag.String("addr", envOr("API_ADDR", ":8080"), "listen address")
	flag.Parse()

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(60 * time.Second))

	r.Get("/healthz", healthz("chronotope-api"))

	// Session API（契约规范 §2）
	r.Route("/orgs/{orgID}", func(r chi.Router) {
		r.Post("/agents", notImplemented) // 创建/升级 agent（config 全量，version+1）
	})
	r.Route("/agents/{agentID}", func(r chi.Router) {
		r.Post("/sessions", notImplemented) // 创建 session → ready（沙箱懒创建）
	})
	r.Route("/sessions/{sessionID}", func(r chi.Router) {
		r.Get("/", notImplemented)          // session 状态 + 最近事件
		r.Get("/events", notImplemented)    // SSE 时间轴，after=seq 断线续读
		r.Post("/runs", notImplemented)     // 提交任务；Idempotency-Key 必带，409 双开
		r.Post("/actions", notImplemented)  // pause|resume|wake|cancel|steer
		r.Post("/messages", notImplemented) // 人类消息注入
		r.Post("/skills", notImplemented)   // skill 安装（幂等）
		r.Post("/mcp", notImplemented)      // MCP 连接（幂等）
		r.Delete("/", notImplemented)       // tombstone 两段式删除（边界语义 §4）
		r.Get("/export", notImplemented)    // 事件日志+工件+快照引用的标准 tar
	})

	// TODO(W4)：/usage 三轴计量（活跃秒 / token / 计算秒，1min 桶聚合）

	log.Printf("chronotope-api listening on %s", *addr)
	if err := http.ListenAndServe(*addr, r); err != nil {
		log.Fatal(err)
	}
}

func healthz(service string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": service})
	}
}

func notImplemented(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotImplemented)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code":  501,
		"error": "not_implemented",
		"path":  r.URL.Path,
		"note":  "骨架路由：实现随 W1–W4 逐周补全（契约规范 §2）",
	})
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
