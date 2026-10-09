// Package thinruntime 是 Chronotope 的薄嵌入 SDK（M5）：
// 任意 agent 框架（Claude Agent SDK / OpenAI Agents SDK / Pi / dsh）经
// /runs 协议对接时使用的幂等键与对账原语——与平台内部同构（同一个语义，
// 不要求跑完整平台）。
package thinruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// EffectKey 幂等键（与平台内部同构——cachekey 语义）：
//   - HarnessKey: (run_id, step) 的模型调用幂等
//   - ToolKey: exec:{step}:{toolID} 的工具副作用幂等
//
// 第三方 harness 的每次模型调用/工具副作用必须携带对应键——
// 重放不重跑副作用（可对账性的地基）。
type EffectKey struct {
	RunID string
	Step  int
	Tool  string
	ID    string
}

// HarnessKey 模型调用的幂等键。
func (k EffectKey) HarnessKey() string {
	return fmt.Sprintf("%s:%d", k.RunID, k.Step)
}

// ToolKey 工具副作用的幂等键（与平台 execproto 同构）。
func (k EffectKey) ToolKey() string {
	return fmt.Sprintf("exec:%d:%s", k.Step, k.ID)
}

// Digest 输入摘要（prepared 行绑定——同键不同输入检测）。
func Digest(argsJSON string) string {
	sum := sha256.Sum256([]byte(argsJSON))
	return hex.EncodeToString(sum[:])
}
