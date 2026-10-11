package restate

import (
	"encoding/json"

	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
)

// 工具风险分级（边界语义设计 §2）：内置词汇默认分级。
// class 0 只读 → 默认允许；class 1 敏感 → 默认允许（org allowlist 可收紧，后置）；
// class 2 危险 → 强制 request_approval，永不自动执行。
var defaultToolClasses = map[string]int{
	// class 0：只读
	"read_file":  0,
	"list_files": 0,
	// class 1：写文件 / 沙箱执行 / 外网 / 控制（默认允许）
	"bash":             1,
	"run_python":       1,
	"write_file":       1,
	"http_request":     1,
	"web_search":       1,
	"request_approval": 1,
	"spawn_subagent":   1,
}

// riskClassOf 求工具的风险分级：配置覆盖表优先（1..2 合法，其余忽略），
// 其次内置词汇默认；未知工具保守按 class 1（默认允许但可被覆盖收紧）。
// A3：sandbox 预设取严（read_only 时写工具提级 class 2——双份权限取严）。
func riskClassOf(name string, cfg sessionapi.AgentConfig) int {
	c := 1
	if c0, ok := cfg.ToolClasses[name]; ok && c0 >= 1 && c0 <= 2 {
		c = c0
	} else if c1, ok := defaultToolClasses[name]; ok {
		c = c1
	}
	// A3：sandbox 声明收紧（read_only → 写工具提级审批；full_access 不
	// 放宽 class——平台分级永远取严）
	if cfg.Sandbox == "read_only" && isWriteTool(name) && c < 2 {
		c = 2
	}
	return c
}

// isWriteTool 写工具判定（A3——read_only 预设的提级范围）。
func isWriteTool(name string) bool {
	switch name {
	case "bash", "run_python", "write_file":
		return true
	}
	return false
}

// approvalGranted 解析审批决定：JSON {"approved":false} → 拒绝；
// 其余（纯文本/缺失字段）视为批准（与 W3 HITL 的纯文本 payload 兼容）。
func approvalGranted(decision string) bool {
	var d struct {
		Approved *bool `json:"approved"`
	}
	if err := json.Unmarshal([]byte(decision), &d); err == nil && d.Approved != nil {
		return *d.Approved
	}
	return decision != ""
}
