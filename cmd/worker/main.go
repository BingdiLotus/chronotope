// chronotope-worker：Restate 服务端点（核心）。
//
// worker 是「无状态的循环协调者」（worker-架构设计 §0）：进程不持有任何持久状态，
// 只做编排（run_workflow 主循环）、分发（API→harness / 代码→executor / 控制→awakeable /
// MCP→MCP 客户端）、记账（journal + 事件投影，事件唯一写入者）。崩溃即重启，由 Restate
// 重放续跑（spike 实验①③已实证）。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/bingdilotus/chronotope/internal/policy"
	"github.com/bingdilotus/chronotope/internal/restate"
	"github.com/bingdilotus/chronotope/internal/store"
)

func main() {
	var (
		addr        = flag.String("addr", envOr("WORKER_ADDR", ":9080"), "listen address（Restate 端点）")
		databaseURL = flag.String("database-url", envOr("DATABASE_URL", ""), "Postgres DSN（事件/消息真相层）")
		harnessURL  = flag.String("harness-url", envOr("HARNESS_URL", "http://localhost:8000"), "harness 服务地址")
		executorURL = flag.String("executor-url", envOr("EXECUTOR_URL", "http://localhost:9082"), "executor 服务地址")
	)
	flag.Parse()

	if *databaseURL == "" {
		log.Fatal("DATABASE_URL 未设置：worker 是事件唯一写入者，必须连接 Postgres")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, *databaseURL)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	consolidateThreshold := 40
	if v := os.Getenv("CONSOLIDATE_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			consolidateThreshold = n
		}
	}
	deps := &restate.Deps{
		Store:                st,
		Harness:              restate.NewHarnessClient(*harnessURL),
		Executor:             executorPoolOf(st, *executorURL),
		MCP:                  restate.NewHTTPMCPClient(),
		Sessions:             restate.RestateSessionSource{},
		ConsolidateThreshold: consolidateThreshold,
		// 预算策略缝（期 3 §A）：POLICY_BUDGET=none → AllowAll（无业务=无限）；
		// 默认 org-daily → 参考实现（org 日预算，w5 语义）
		BudgetPolicy: budgetPolicyOf(st),
		// 审批路由缝（期 3 §B）：POLICY_APPROVAL=none → ManualOnly 人工；
		// 默认 org-policy → 参考实现（org 策略 approver 集合 + TTL）
		ApprovalRouter: approvalRouterOf(st),
	}
	handler, err := restate.BuildEndpoint(deps)
	if err != nil {
		log.Fatalf("build restate endpoint: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": "chronotope-worker"})
	})
	mux.Handle("/", handler)

	srv := &http.Server{Addr: *addr, Handler: mux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("chronotope-worker（Restate 端点：session_object / run_workflow / scheduler / webhook）listening on %s", *addr)
	log.Printf("注册部署: curl -X POST :9070/deployments -d '{\"uri\":\"http://<worker>\",\"version\":\"v1\",\"use_http_11\":true,\"force\":true}'")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// approvalRouterOf 装配审批路由缝（期 3 §B：env 选择；默认参考实现）。
func approvalRouterOf(st *store.Store) policy.ApprovalRouter {
	if envOr("POLICY_APPROVAL", "org-policy") == "none" {
		return policy.ApprovalRouter(policy.ManualOnly{})
	}
	return &policy.OrgApprovalPolicy{
		Get: func(ctx context.Context, tenantID string) ([]string, []string, int64, error) {
			p, err := st.GetApprovalPolicy(ctx, tenantID)
			if err != nil {
				return nil, nil, 0, err
			}
			return p.ToolPatterns, p.Approvers, p.TTLSeconds, nil
		},
	}
}

// executorPoolOf 装配多宿主池（期 4 §B）：注册表候选 + SchedulerPolicy 缝 +
// 单点 EXECUTOR_URL 兜底（无注册行时现有部署无感）。
func executorPoolOf(st *store.Store, executorURL string) restate.Executor {
	pool := restate.NewExecutorPool(restate.NewExecutorClient(executorURL), nil)
	pool.ListExecutors = func(ctx context.Context) ([]store.ExecutorRow, error) {
		return st.ListHealthyExecutors(ctx, 2*time.Minute)
	}
	pool.SandboxOwner = st.GetSandboxOwner
	pool.SetSandboxOwner = st.SetSandboxOwner
	return pool
}

// budgetPolicyOf 装配预算策略缝（期 3 §A：env 选择；默认参考实现）。
func budgetPolicyOf(st *store.Store) restate.BudgetPolicy {
	if envOr("POLICY_BUDGET", "org-daily") == "none" {
		return restate.BudgetPolicy(policy.AllowAll{})
	}
	return &policy.OrgDailyBudget{
		Org:   st.GetOrgQuotas,
		Usage: st.OrgDailyUsage,
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
