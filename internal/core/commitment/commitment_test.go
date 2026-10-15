package commitment

import "testing"

func TestCanTransition(t *testing.T) {
	valid := []struct{ from, to State }{
		{StatePending, StateOpen}, {StatePending, StateOrphaned}, {StatePending, StateCanceled},
		{StateOpen, StateFulfilled}, {StateOpen, StateBroken}, {StateOpen, StateExpired}, {StateOpen, StateCanceled},
		{StateExpired, StateFulfilled},                           // 晚兑现不是违约
		{StateOrphaned, StateOpen}, {StateOrphaned, StateBroken}, // 补发成功 / 人工放弃
	}
	for _, c := range valid {
		if err := CanTransition(c.from, c.to); err != nil {
			t.Errorf("%s → %s 应合法: %v", c.from, c.to, err)
		}
	}
	invalid := []struct{ from, to State }{
		{StatePending, StateFulfilled}, // 未确认发出不能兑现
		{StateOpen, StatePending},      // 不可逆
		{StateFulfilled, StateOpen},    // 终态
		{StateBroken, StateExpired},    // 终态
		{StateCanceled, StateOpen},     // 终态
		{StateOrphaned, StateExpired},  // 孤儿只能补发/放弃
	}
	for _, c := range invalid {
		if err := CanTransition(c.from, c.to); err == nil {
			t.Errorf("%s → %s 应非法", c.from, c.to)
		}
	}
}

func TestTerminal(t *testing.T) {
	for _, s := range []State{StateFulfilled, StateBroken, StateCanceled} {
		if !s.Terminal() {
			t.Errorf("%s 应为终态", s)
		}
	}
	for _, s := range []State{StatePending, StateOpen, StateExpired, StateOrphaned} {
		if s.Terminal() {
			t.Errorf("%s 不应为终态", s)
		}
	}
}
