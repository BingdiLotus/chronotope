// Package main 是 W1 D1 spike 的 Restate 端点 + 桩 harness（mvp-落地方案 §4 D1）。
//
// 验证五项组合原语（任一阻断 → 切 Temporal Go，见风险登记表）：
//
//	① Run 闭包内分钟级 SSE 长流（journaled side effect；崩溃后已完成 step 不重放、未完成 step 幂等重发）
//	② awakeable 跨 HTTP resolve（HITL 审批 / 欠费冻结的挂起原语）
//	③ child workflow 调用/await（子 Agent 并行委派的原语）
//	④ endpoint versioning（新 invocation 走 v2、在途留 v1）
//	⑤ journal/state 条目大小限制量级
//
// 用法与实验步骤见 README.md；驱动脚本 experiments.sh。
package main

import (
	"flag"
	"log"
	"log/slog"
	"net/http"
	"os"

	"github.com/restatedev/sdk-go/server"
)

var (
	// version 由 SPIKE_VERSION 环境变量注入（实验④：v1/v2 双端点）
	version = func() string {
		if v := os.Getenv("SPIKE_VERSION"); v != "" {
			return v
		}
		return "v1"
	}()

	stubAddr     string
	endpointAddr string
)

func main() {
	addr := flag.String("addr", ":9080", "restate endpoint listen address")
	stub := flag.String("stub", "http://127.0.0.1:19001", "stub harness base URL")
	stubOnly := flag.Bool("stub-only", false, "只运行桩 harness（独立进程，供 kill -9 实验时保持计数）")
	flag.Parse()
	endpointAddr = *addr
	stubAddr = *stub

	// 桩 harness 只以 -stub-only 独立进程运行（实验①③的 kill -9 会杀死端点进程，
	// 桩独立常驻才能跨重启保留命中计数；端点模式统一打外部桩）。
	if *stubOnly {
		log.Printf("stub harness listening on 127.0.0.1:19001")
		if err := http.ListenAndServe("127.0.0.1:19001", newStubHandler()); err != nil {
			log.Fatal(err)
		}
	}

	ep := server.NewRestate()
	ep = ep.WithLogger(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}), true)
	ep.Bind(sseProbeService()).
		Bind(approvalObject()).
		Bind(approverService()).
		Bind(parentWorkflow()).
		Bind(childWorkflow()).
		Bind(versionService()).
		Bind(versionWorkflow()).
		Bind(sizeWorkflow())

	handler, err := ep.Handler()
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("spike endpoint (SPIKE_VERSION=%s) listening on %s", version, *addr)
	if err := http.ListenAndServe(*addr, handler); err != nil {
		log.Fatal(err)
	}
}
