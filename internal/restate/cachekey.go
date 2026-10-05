// Package restate 是 Restate 客户端封装与 journal helper（落地方案 §1）。
//
// 缓存键权威定义（契约规范 §1）：缓存键 = journal 位置（run_id + step 名）；
// 模型/协议版本由 run 绑定承载，不得另行拼接 provider/model 进键。
//
// W1 D1 spike 验证五项原语（①分钟级 SSE 长流 ②awakeable 跨 HTTP resolve ③child
// workflow ④endpoint versioning ⑤journal/state 条目大小）后再接入 restate-sdk-go；
// 任一阻断则切 Temporal Go（降级路径，边界已隔离在本包与 run_workflow 主循环内）。
package restate

import "strings"

// CacheKey 返回 journal 位置（run_id + step 名）的字符串形式。
// 崩溃重放时以该键读取缓存输出，不重调 harness / 不重跑沙箱（三条纪律之三）。
func CacheKey(runID, stepName string) string {
	return runID + "/" + stepName
}

// SplitCacheKey 拆解 CacheKey 产物，供测试与诊断使用。
func SplitCacheKey(key string) (runID, stepName string, ok bool) {
	runID, stepName, ok = strings.Cut(key, "/")
	return runID, stepName, ok
}

// StepName 约定：harness 调用 step 名为 "harness:"+step，沙箱执行为 "exec:"+step+":"+toolID，
// 与 worker-架构设计 §3 的 restate.Run(ctx, "harness:"+step, ...) 保持一致。
func StepName(kind string, step int, toolID string) string {
	switch kind {
	case "harness", "await":
		return kind + ":" + itoa(step)
	case "exec", "mcp":
		return kind + ":" + itoa(step) + ":" + toolID
	}
	return kind
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
