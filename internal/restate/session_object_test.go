package restate

import (
	"testing"

	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
)

// session_object 纯状态迁移测试（契约规范 §2 状态机唯一权威）。
func TestSessionTransitions(t *testing.T) {
	cfg := sessionapi.AgentConfig{Model: "m", Instructions: "i", Version: 1}

	t.Run("wake 未初始化会话报错", func(t *testing.T) {
		if _, err := wakeTransition(SessionState{}); err == nil {
			t.Fatal("未初始化应报错")
		}
	})

	t.Run("wake 从 ready/sleeping/paused 迁移到 running", func(t *testing.T) {
		for _, from := range []sessionapi.SessionPhase{sessionapi.PhaseReady, sessionapi.PhaseSleeping, sessionapi.PhasePaused} {
			next, err := wakeTransition(SessionState{Phase: from, AgentConfig: cfg})
			if err != nil {
				t.Fatalf("wake(%s): %v", from, err)
			}
			if next.Phase != sessionapi.PhaseRunning {
				t.Fatalf("wake(%s) 应到 running，得 %s", from, next.Phase)
			}
		}
	})

	t.Run("wake 对 running 保持原态（幂等）", func(t *testing.T) {
		next, err := wakeTransition(SessionState{Phase: sessionapi.PhaseRunning, AgentConfig: cfg})
		if err != nil {
			t.Fatalf("wake(running): %v", err)
		}
		if next.Phase != sessionapi.PhaseRunning {
			t.Fatalf("wake(running) 应保持原态，得 %s", next.Phase)
		}
	})

	t.Run("pause 只对 running 生效", func(t *testing.T) {
		if got := pauseTransition(SessionState{Phase: sessionapi.PhaseRunning, AgentConfig: cfg}); got.Phase != sessionapi.PhasePaused {
			t.Fatalf("running → paused 失败: %s", got.Phase)
		}
		if got := pauseTransition(SessionState{Phase: sessionapi.PhaseReady, AgentConfig: cfg}); got.Phase != sessionapi.PhaseReady {
			t.Fatalf("ready 不应被 pause: %s", got.Phase)
		}
	})

	t.Run("resume 只对 paused 生效", func(t *testing.T) {
		if got := resumeTransition(SessionState{Phase: sessionapi.PhasePaused, AgentConfig: cfg}); got.Phase != sessionapi.PhaseRunning {
			t.Fatalf("paused → running 失败: %s", got.Phase)
		}
		if got := resumeTransition(SessionState{Phase: sessionapi.PhaseRunning, AgentConfig: cfg}); got.Phase != sessionapi.PhaseRunning {
			t.Fatalf("running 应保持: %s", got.Phase)
		}
	})
}
