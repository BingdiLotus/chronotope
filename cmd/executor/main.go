// chronotope-executor：沙箱编排服务（execute 协议，契约规范 §4）。
//
// 实现同一 internal/execproto.Driver 接口的两个 driver：
//
//	docker driver —— dev/通用，受限容器（read-only root、egress 白名单、无 secrets、
//	                 CPU/内存/TTL 限额），W2 落地；
//	e2b driver    —— prod，Firecracker 微 VM 强隔离，W4 在 Linux(KVM) 主机验证切流。
//
// capability 字段（cpu/gpu/network/browser）从第一天就在协议里（落地方案 §2.4）。
//
// 骨架阶段：协议路由铺开返回 501；docker driver 实现随 W2 接入。
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"
)

func main() {
	addr := flag.String("addr", envOr("EXECUTOR_ADDR", ":9082"), "listen address")
	flag.Parse()

	r := chi.NewRouter()
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": "chronotope-executor"})
	})

	// Executor 协议（契约规范 §4）
	r.Post("/sandboxes", notImplemented)               // 创建沙箱 → ready；restore_from 可选
	r.Post("/execute", notImplemented)                 // 流式日志 + 结果；幂等键 = (run_id, step, tool_id)
	r.Get("/files/{sandboxID}/{path}", notImplemented) // 文件快路径（不经过 shell）
	r.Put("/files/{sandboxID}/{path}", notImplemented)
	r.Post("/sandboxes/{sandboxID}/freeze", notImplemented) // Tier 1 冻结（docker pause）
	r.Post("/sandboxes/{sandboxID}/unfreeze", notImplemented)
	r.Post("/sandboxes/{sandboxID}/snapshot", notImplemented) // Tier 2 快照 → snapshot_ref
	r.Delete("/sandboxes/{sandboxID}", notImplemented)        // Tier 3 拆除（前提 file_sync_state=synced）

	// TODO(W2)：docker driver——受限容器 + 幂等缓存（PG sandbox_execs，防重试双执行）
	// TODO(W4)：e2b driver + capability 路由（executors 表）

	log.Printf("chronotope-executor listening on %s", *addr)
	if err := http.ListenAndServe(*addr, r); err != nil {
		log.Fatal(err)
	}
}

func notImplemented(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotImplemented)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code":  501,
		"error": "not_implemented",
		"path":  r.URL.Path,
		"note":  "骨架路由：docker driver 实现随 W2 接入（契约规范 §4）",
	})
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
