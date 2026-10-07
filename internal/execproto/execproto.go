// Package execproto 是 execute 协议（契约规范 §4）的 Go 定义：
// docker driver（dev/通用，W2 落地）与 e2b driver（prod，Firecracker 微 VM，W4 在
// Linux(KVM) 主机验证切流）实现同一 Driver 接口，按 capability 路由切换——
// 「任意空间」可组合性的第一次实战验收（mvp-落地方案 §2.4）。
//
// 幂等缓存：executor 对 idempotency_key 的执行结果做缓存（PG sandbox_execs，TTL
// 24h）；exec 已执行但响应丢失 → 同键重发返回缓存结果，绝不复跑命令。
package execproto

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// Limits 是沙箱资源限额。
type Limits struct {
	CPU  string `json:"cpu"`
	Mem  string `json:"mem"`
	Disk string `json:"disk"`
}

// Capabilities 是沙箱能力（cpu/gpu/network/browser 字段从第一天就在协议里，实现可后补）。
type Capabilities struct {
	Network bool `json:"network"`
}

// CreateSandboxRequest 是 POST /sandboxes 的请求体。
// session_id 为平台侧扩展字段（兼容性契约第 3 条：新增字段带默认值，旧端忽略）。
type CreateSandboxRequest struct {
	Image        string       `json:"image"`
	Limits       Limits       `json:"limits"`
	TTL          string       `json:"ttl"`
	Capabilities Capabilities `json:"capabilities"`
	RestoreFrom  string       `json:"restore_from,omitempty"` // snapshot_ref（Tier 2 恢复）
	SessionID    string       `json:"session_id,omitempty"`
}

// Sandbox 是沙箱引用。
// Sandbox 是创建结果的窄视图（仅 ID——Status/Image/Driver 曾写而不读，
// 架构审计死字段删除；行状态以 store.SandboxRow 为准）。
type Sandbox struct {
	ID string `json:"sandbox_id"`
}

// ExecuteRequest 是 POST /execute 的请求体；Name 必须是工具名词汇表规范名。
type ExecuteRequest struct {
	SandboxID      string `json:"sandbox_id"`
	Name           string `json:"name"`
	Input          string `json:"input"`
	TTL            string `json:"ttl,omitempty"`
	IdempotencyKey string `json:"idempotency_key"`
}

// ExecuteResult 是 execute 的结果（大输出外置 MinIO，此处只回引用）。
type ExecuteResult struct {
	Exit      int    `json:"exit"`
	OutputRef string `json:"output_ref"`
}

// ExecuteIdempotencyKey 生成 execute 幂等键 = (run_id, step, tool_id)（契约规范 §4）。
func ExecuteIdempotencyKey(runID string, step int, toolID string) string {
	return fmt.Sprintf("%s:%d:%s", runID, step, toolID)
}

// Driver 是 executor 双实现的统一接口（docker / e2b_selfhosted）。
// ErrSandboxNotFound 沙箱不存在（已销毁/回收）——worker 据此触发重建
// （快照恢复路径，评审 #7「一周前会话今天还能继续」的触发条件）。
var ErrSandboxNotFound = errors.New("sandbox not found")

type Driver interface {
	CreateSandbox(ctx context.Context, req CreateSandboxRequest) (*Sandbox, error)
	// Execute 把流式日志写入 log（SSE 日志帧的原始流），返回最终结果。
	Execute(ctx context.Context, req ExecuteRequest, log io.Writer) (*ExecuteResult, error)
	ReadFile(ctx context.Context, sandboxID, path string) ([]byte, error)
	WriteFile(ctx context.Context, sandboxID, path string, data []byte) error
	Freeze(ctx context.Context, sandboxID string) error // Tier 1
	Unfreeze(ctx context.Context, sandboxID string) error
	Snapshot(ctx context.Context, sandboxID string) (string, error) // Tier 2 → snapshot_ref
	Destroy(ctx context.Context, sandboxID string) error
	// ListOrphanContainers 按 label 列沙箱容器名（D2 启动 sweep）
	ListOrphanContainers(ctx context.Context) ([]string, error) // Tier 3
}
