package thinruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// LedgerClient 对账客户端（M5 薄嵌入）：查询平台的 effect ledger
// （/runs/{id}/calls）——第三方框架在崩溃恢复时用它对账
// （result 同 hash → 回读原结果；unknown → 停住人工裁决）。
type LedgerClient struct {
	BaseURL string
	HTTP    *http.Client
}

// CallRow 账本行。
type CallRow struct {
	Kind         string `json:"kind"`
	Step         int    `json:"step"`
	State        string `json:"state"` // prepared | dispatched | result | unknown
	RequestHash  string `json:"request_hash,omitempty"`
	TokensIn     int64  `json:"tokens_in,omitempty"`
	TokensOut    int64  `json:"tokens_out,omitempty"`
	UsageUnknown bool   `json:"usage_unknown,omitempty"`
	Err          string `json:"err,omitempty"`
	DispatchSeq  int    `json:"dispatch_seq,omitempty"`
}

// Reconcile 对账查询（run 的全部账本行）。
func (c *LedgerClient) Reconcile(ctx context.Context, runID string) ([]CallRow, string, error) {
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/runs/"+runID+"/calls", nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("ledger: status %d", resp.StatusCode)
	}
	var out struct {
		Admission string    `json:"admission_state"`
		Calls     []CallRow `json:"calls"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, "", err
	}
	return out.Calls, out.Admission, nil
}

// Decide 对账判定（薄嵌入的恢复协议）：
//   - result + hash 同 → 回读原结果（不执行）
//   - unknown → 停住（人工裁决）
//   - 其他 → 可派发
func Decide(row *CallRow, reqHash string) string {
	if row == nil {
		return "run"
	}
	switch row.State {
	case "result":
		if row.RequestHash != "" && row.RequestHash != reqHash {
			return "conflict"
		}
		return "replay"
	case "unknown":
		return "halt"
	case "dispatched":
		return "halt" // 无接受证据不重派发
	}
	return "run"
}
