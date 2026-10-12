// Package observability 是 OpenTelemetry 导出的装配层（B4——事件流的
// span 导出：step/llm_call/tool_exec 三 span 族——Claude cost tracking
// 的对应）。
package observability

import (
	"context"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

var tracer trace.Tracer = otel.Tracer("chronotope")

// Init 按 OTEL_EXPORTER 装配（stdout = 开发用文本导出；默认 noop——
// 零行为变化）。B4 的 OTLP 导出后置（collector 端点配置）。
func Init() {
	switch os.Getenv("OTEL_EXPORTER") {
	case "stdout":
		exp, err := stdouttrace.New(stdouttrace.WithPrettyPrint())
		if err != nil {
			return
		}
		tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp))
		otel.SetTracerProvider(tp)
		tracer = tp.Tracer("chronotope")
	}
}

// safeStart 包装 tracer.Start（mock ctx 的 Value 未设会 panic——测试
// 替身场景吞掉；真实 ctx 正常导出）。
func safeStart(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	defer func() {
		if recover() != nil {
			// mock ctx 无 span context——noop（span 创建失败不阻断业务）
		}
	}()
	return tracer.Start(ctx, name, trace.WithAttributes(attrs...))
}

// Step 是 step span（runLoop 的每步——run_id/session_id/step 属性）。
func Step(ctx context.Context, name string, runID, sessionID string, step int) (context.Context, trace.Span) {
	return safeStart(ctx, name,
		attribute.String("run_id", runID),
		attribute.String("session_id", sessionID),
		attribute.Int("step", step),
	)
}

// LLMCall 是 llm_call span（harness 调用——tokens 属性；Claude cost
// tracking 的对应）。
func LLMCall(ctx context.Context, model string, tokensIn, tokensOut int) (context.Context, trace.Span) {
	return safeStart(ctx, "chronotope.llm_call",
		attribute.String("model", model),
		attribute.Int("tokens_in", tokensIn),
		attribute.Int("tokens_out", tokensOut),
	)
}

// ToolExec 是 tool_exec span（executor/MCP 执行——duration 属性；Pi 的
// tool_execution_end.durationMs 对应）。
func ToolExec(ctx context.Context, tool string, durationMs int64) (context.Context, trace.Span) {
	return safeStart(ctx, "chronotope.tool_exec",
		attribute.String("tool", tool),
		attribute.Int64("duration_ms", durationMs),
	)
}
