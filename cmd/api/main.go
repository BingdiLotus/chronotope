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

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
