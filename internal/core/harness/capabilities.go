// Package harness 是 harness 能力声明的领域类型（B1——dsh Seam 的运行时
// 形态；期 7 纪律：类型下沉 core）。
package harness

// Capabilities 是 harness 的结构化能力声明（B1——capabilities 从自由
// 数组升级为结构化：worker 按声明路由/校验）。
type Capabilities struct {
	// Hooks 是 harness 支持的 hook 族（PreToolUse/PostToolUse 等——
	// A1 的注入通道）。
	Hooks []string `json:"hooks,omitempty"`
	// Steering 是运行中转向支持（A4 的 steer-drain）。
	Steering bool `json:"steering,omitempty"`
	// SandboxPresets 是 sandbox 预设支持（A3——read_only/workspace_write/
	// full_access）。
	SandboxPresets []string `json:"sandbox_presets,omitempty"`
	// ToolFamilies 是工具族的处理能力（exec/mcp/control——worker 按族
	// 路由工具调用）。
	ToolFamilies []string `json:"tool_families,omitempty"`
	// Protocol 是 /runs 协议版本（契约规范的版本声明）。
	Protocol string `json:"protocol,omitempty"`
}

// Default 是平台内置 harness 的默认声明（全能力——参考实现）。
var Default = Capabilities{
	Hooks:          []string{"PreToolUse", "PostToolUse"},
	Steering:       true,
	SandboxPresets: []string{"read_only", "workspace_write", "full_access"},
	ToolFamilies:   []string{"exec", "mcp", "control"},
	Protocol:       "1.0",
}

// Has 判定能力是否存在（路由/校验的判定原语）。
func (c Capabilities) Has(family string) bool {
	switch family {
	case "steering":
		return c.Steering
	}
	for _, h := range c.Hooks {
		if h == family {
			return true
		}
	}
	for _, p := range c.SandboxPresets {
		if p == family {
			return true
		}
	}
	for _, t := range c.ToolFamilies {
		if t == family {
			return true
		}
	}
	return false
}
