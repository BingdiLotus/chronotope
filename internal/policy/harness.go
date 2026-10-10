package policy

import (
	"context"

	sessionapi "github.com/bingdilotus/chronotope/internal/core/sessionapi"
)

// HarnessResolution 是解析结果（run 快照冻结的 endpoint+version）。
type HarnessResolution struct {
	Endpoint string
	Version  string
}

// HarnessResolver 是 harness 解析缝（装配设计 §3 层 1）：agent 绑定 →
// org 默认 → 全局 fallback。业务方替换路由策略时实现自己的 Resolver。
type HarnessResolver interface {
	Resolve(ctx context.Context, orgID string, ref *sessionapi.HarnessBinding) (HarnessResolution, error)
}

// HarnessLookup 是解析链的数据视图（DefaultResolver 用）。
type HarnessLookup interface {
	ActiveHarness(ctx context.Context, orgID, name string) (endpoint, version string, err error)
}

// DefaultResolver 是参考实现：agent 绑定（name）→ org 的 active 版本 →
// 全局 fallback（零绑定时全局 URL——阶段 1 语义保持）。
type DefaultResolver struct {
	Lookup     HarnessLookup
	GlobalURL  string
	GlobalName string
}

// Resolve 解析链实现。
func (d DefaultResolver) Resolve(ctx context.Context, orgID string, ref *sessionapi.HarnessBinding) (HarnessResolution, error) {
	if ref != nil && ref.Name != "" {
		endpoint, version, err := d.Lookup.ActiveHarness(ctx, orgID, ref.Name)
		if err == nil {
			return HarnessResolution{Endpoint: endpoint, Version: version}, nil
		}
		// 绑定查不到 → 全局 fallback（fail-open 但显式——不静默换绑定）
		return HarnessResolution{Endpoint: d.GlobalURL, Version: "global"}, nil
	}
	return HarnessResolution{Endpoint: d.GlobalURL, Version: "global"}, nil
}
