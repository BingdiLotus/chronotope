// chronotope-executor：沙箱编排服务（execute 协议，契约规范 §4）。
//
// dev 档：DockerDriver（受限容器：read-only root、tmpfs、network none|bridge、CPU/内存
// 限额、无 secrets、会话作用域工作区卷）；prod 档 E2B driver（W4 验证切流，同一接口）。
// 沙箱事实状态在 PG（sandboxes 表），exec 幂等缓存在 PG（sandbox_execs，防重试双执行）。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bingdilotus/chronotope/internal/blobstore"
	"github.com/bingdilotus/chronotope/internal/execproto"
	"github.com/bingdilotus/chronotope/internal/store"
)

func main() {
	var (
		addr          = flag.String("addr", envOr("EXECUTOR_ADDR", ":9082"), "listen address")
		databaseURL   = flag.String("database-url", envOr("DATABASE_URL", ""), "Postgres DSN（沙箱事实状态 + 幂等缓存）")
		workspaceRoot = flag.String("workspace-root", envOr("EXECUTOR_WORKSPACE_ROOT", ""), "宿主机工作区根（文件快路径暂存，空则系统临时目录）")
	)
	flag.Parse()

	if *databaseURL == "" {
		log.Fatal("DATABASE_URL 未设置：executor 需要 Postgres 存沙箱事实状态与幂等缓存")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, *databaseURL)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	var driver execproto.Driver
	workspace := ""
	switch os.Getenv("EXECUTOR_DRIVER") {
	case "e2b":
		// prod 档（落地方案 §12）：E2B 沙箱；E2B_API_URL 指向自托管网关（同协议）
		driver = execproto.NewE2BDriver(
			execproto.NewHTTPE2BAPI(os.Getenv("E2B_API_URL"), os.Getenv("E2B_API_KEY")),
			os.Getenv("E2B_TEMPLATE"),
		)
		log.Printf("chronotope-executor（e2b driver）listening on %s", *addr)
	default:
		d := execproto.NewDockerDriver(nil, *workspaceRoot)
		driver = d
		workspace = d.WorkspaceRoot
		log.Printf("chronotope-executor（docker driver）listening on %s", *addr)
	}
	server := &execproto.Server{
		Driver:        driver,
		Store:         st,
		WorkspaceRoot: workspace,
		Logger:        slog.Default(),
	}
	// 工作区 blob 合同（期 2 §A）：RUSTFS_ENDPOINT 未配置时禁用（纯卷语义回退）
	if endpoint := envOr("RUSTFS_ENDPOINT", ""); endpoint != "" {
		blob, err := blobstore.NewBlobStore(endpoint,
			envOr("RUSTFS_ACCESS_KEY", "rustfsadmin"),
			envOr("RUSTFS_SECRET_KEY", "rustfsadmin"),
			envOr("RUSTFS_BUCKET", "workspaces"),
			envOr("RUSTFS_SECURE", "") == "true")
		if err != nil {
			slog.Error("blob store 初始化失败（禁用 blob 合同）", "err", err)
		} else {
			if err := blob.EnsureBucket(context.Background()); err != nil {
				slog.Warn("blob bucket 初始化失败（禁用 blob 合同）", "err", err)
			} else {
				server.Blob = blob
				slog.Info("工作区 blob 合同已启用", "endpoint", endpoint)
			}
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": "chronotope-executor"})
	})
	mux.Handle("/", server.Router())

	srv := &http.Server{Addr: *addr, Handler: mux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	// 孤儿 GC（W8）：周期扫描 ttl 过期沙箱 → 销毁容器（尽力）+ 删行
	gcInterval := 5 * time.Minute
	if v := os.Getenv("EXECUTOR_GC_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			gcInterval = d
		}
	}
	go func() {
		ticker := time.NewTicker(gcInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if n, err := server.GC(ctx); err != nil {
					slog.Error("gc pass failed", "err", err)
				} else if n > 0 {
					slog.Info("gc pass", "cleaned", n)
				}
			}
		}
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
