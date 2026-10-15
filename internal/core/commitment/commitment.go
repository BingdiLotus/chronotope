// Package commitment 承诺原语（修正版 §2.2）——agent 对外义务的一等实体。
// 事件为真相、投影表为查询面；状态机七态含 INTENT 态（pending/orphaned）。
package commitment

import "fmt"

// State 承诺状态（§2.2 七态——由事件派生）。
type State string

const (
	StatePending   State = "pending"   // 已记录，出站未确认发出（INTENT）
	StateOpen      State = "open"      // 已确认发出，义务生效
	StateFulfilled State = "fulfilled" // 兑现（含晚兑现）
	StateBroken    State = "broken"    // 违约（撤回或自相矛盾）
	StateExpired   State = "expired"   // due_at 已过且无兑现证据
	StateCanceled  State = "canceled"  // 显式撤回（「我不干了」）
	StateOrphaned  State = "orphaned"  // 已记录但发送确认超时
)

// Valid 合法状态。
func (s State) Valid() bool {
	switch s {
	case StatePending, StateOpen, StateFulfilled, StateBroken, StateExpired, StateCanceled, StateOrphaned:
		return true
	}
	return false
}

// Terminal 终态。
func (s State) Terminal() bool {
	switch s {
	case StateFulfilled, StateBroken, StateCanceled:
		return true
	}
	return false
}

// CanTransition 转移规则（§2.2 表格——违反即返回错误）。
func CanTransition(from, to State) error {
	switch from {
	case StatePending:
		switch to {
		case StateOpen, StateOrphaned, StateCanceled:
			return nil
		}
	case StateOpen:
		switch to {
		case StateFulfilled, StateBroken, StateExpired, StateCanceled:
			return nil
		}
	case StateExpired:
		if to == StateFulfilled {
			return nil // 晚兑现不是违约
		}
	case StateOrphaned:
		switch to {
		case StateOpen, StateBroken:
			return nil // 补发成功 / 人工放弃
		}
	case StateFulfilled, StateBroken, StateCanceled:
		// 终态不可转移
	}
	return fmt.Errorf("commitment: illegal transition %s → %s", from, to)
}

// SpeechAct 言语行为（准入四分类）。
type SpeechAct string

const (
	ActCommissive  SpeechAct = "commissive"
	ActAssertive   SpeechAct = "assertive"
	ActDirective   SpeechAct = "directive"
	ActDeclarative SpeechAct = "declarative"
)

// Strength 承诺强度。
type Strength string

const (
	StrengthStrong      Strength = "strong"
	StrengthWeak        Strength = "weak"
	StrengthConditional Strength = "conditional"
)
