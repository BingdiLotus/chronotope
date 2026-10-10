// Package ledger 是效果账本的行类型（期 7：从 store 下沉）。
package ledger

import "time"

// LLMCallRow LLM 调用账本行（仲裁合同：prepared/dispatched/result/unknown）。
type LLMCallRow struct {
	RunID        string
	Step         int
	DispatchSeq  int
	State        string
	RequestHash  string
	Result       string
	TokensIn     int64
	TokensOut    int64
	UsagePartial bool
	UsageUnknown bool
	Err          string
	PreparedAt   time.Time
	DispatchedAt *time.Time
	ResultAt     *time.Time
}

// MCPCallRow MCP 调用账本行（调用身份 call_key）。
type MCPCallRow struct {
	RunID       string
	Step        int
	Server      string
	Tool        string
	State       string
	Err         string
	RequestHash string
	Result      string
	PreparedAt  time.Time
}
