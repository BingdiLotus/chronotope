// chronotope-api：HTTP/SSE 网关（REST + SSE + 事件投影 + 计量 + outbox + 幂等）。
//
// 职责边界：api 只读投影事件（worker 是事件唯一写入者），控制面经 Restate ingress
// 调用 worker（session_object.Create / run_workflow 启动）。
package main

import (
	"context"
	"flag"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/bingdilotus/chronotope/internal/api"
	"github.com/bingdilotus/chronotope/internal/events"
	"github.com/bingdilotus/chronotope/internal/store"
)

func main() {
	var (
		addr        = flag.String("addr", envOr("API_ADDR", ":8080"), "listen address")
		databaseURL = flag.String("database-url", envOr("DATABASE_URL", ""), "Postgres DSN（事件投影/元数据）")
		restateURL  = flag.String("restate-url", envOr("RESTATE_URL", "http://localhost:8081"), "Restate ingress（worker 控制面）")
	)
	flag.Parse()

	if *databaseURL == "" {
		log.Fatal("DATABASE_URL 未设置：api 需要 Postgres 投影事件与元数据")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, *databaseURL)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	h := api.New(st, events.NewHub(), api.NewHTTPIngress(*restateURL))
	h.DeliveryAllowPrivate = os.Getenv("OUTBOX_ALLOW_PRIVATE") == "true"
	if v := os.Getenv("EXECUTOR_URL"); v != "" {
		h.Executor = api.NewHTTPExecutor(v)
	}
	h.StartPoller(ctx)

	// 三轴计量聚合（活跃秒 / token / 计算秒，1min 桶；重建式，W4）。
	// 周期可经环境变量调小（e2e 预算冻结依赖聚合结果；默认 1min）。
	aggInterval := time.Minute
	if v := os.Getenv("API_AGGREGATE_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			aggInterval = d
		}
	}
	agg := &api.Aggregator{Store: st, Logger: slog.Default(), Interval: aggInterval}
	go agg.Run(ctx)

	// 事件投递 worker（outbox，§5）：webhook/email 通道，指数退避
	deliverInterval := 5 * time.Second
	if v := os.Getenv("OUTBOX_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			deliverInterval = d
		}
	}
	// 接纳屏障恢复（评审 #6）：queued 遗留 run 重投扫描
	admission := &api.AdmissionRecovery{Store: st, Ingress: h.Ingress, Logger: slog.Default()}
	go admission.Run(ctx, time.Minute)

	deliverer := &api.Deliverer{
		Store:        st,
		Logger:       slog.Default(),
		Batch:        100,
		AllowPrivate: os.Getenv("OUTBOX_ALLOW_PRIVATE") == "true",
		SMTP: api.SMTPConfig{
			Host: os.Getenv("SMTP_HOST"),
			Port: firstNonEmpty(os.Getenv("SMTP_PORT"), "25"),
			From: firstNonEmpty(os.Getenv("SMTP_FROM"), "chronotope@localhost"),
		},
	}
	go deliverer.Run(ctx, deliverInterval)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(api.CorsMiddleware(os.Getenv("API_CORS_ORIGINS")))
	r.Use(h.AuthMiddleware(os.Getenv("API_AUTH_MODE"), os.Getenv("API_ADMIN_KEY")))
	r.Use(middleware.Recoverer)
	// 不设全局 Timeout：SSE 时间轴是长连接（由客户端断开控制生命周期）；
	// 提交任务的阻塞时长由 ingress 客户端超时（10min）约束
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","service":"chronotope-api"}`))
	})
	r.Mount("/", h.Router())

	srv := &http.Server{Addr: *addr, Handler: r}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("chronotope-api listening on %s", *addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
