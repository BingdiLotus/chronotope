// Package extension 是扩展层的领域类型（B2——tools/prompts/hooks 的可
// 装配包；期 7 纪律：类型下沉 core）。
package extension

// Definition 是扩展包的定义（kind 决定 definition 的语义）。
type Definition struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"` // tools | prompts | hooks
	Version string `json:"version"`
	State   string `json:"state"`

	// Tools 是工具清单（kind=tools——合并进 AgentConfig.Tools）。
	Tools []string `json:"tools,omitempty"`
	// Prompt 是系统提示片段（kind=prompts——拼接进 system prompt）。
	Prompt string `json:"prompt,omitempty"`
	// Hooks 是 hook 配置（kind=hooks——进审批上下文的注入通道）。
	Hooks []string `json:"hooks,omitempty"`
}
