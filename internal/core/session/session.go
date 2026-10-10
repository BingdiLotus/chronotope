// Package session 是会话/run/agent/org 的行类型（期 7：从 store 下沉——
// store 只做 SQL 与映射；kernel 的编译期契约在 core）。
package session

import (
	"encoding/json"
	"time"

	"github.com/bingdilotus/chronotope/internal/core/event"
	sessionapi "github.com/bingdilotus/chronotope/internal/core/sessionapi"
)

// Agent agent 行（config 全量 + version + spec digest）。
type Agent struct {
	ID         string
	OrgID      string
	Name       string
	Config     sessionapi.AgentConfig
	Version    int
	SpecDigest string // canonical JSON(Config) 的 sha256（正确性二期 ⑩）
}

// Org org 行（预算键在 Quotas）。
type Org struct {
	ID     string
	Name   string
	Quotas map[string]any
}

// Session 会话行（分支血缘与归档标记）。
type Session struct {
	ID                string
	OrgID             string
	AgentID           string
	Status            sessionapi.SessionPhase
	RestateKey        *string
	LastActiveAt      *time.Time
	DeletedAt         *time.Time
	ArchivedAt        *time.Time // 冷层归档标记（期 2 §B）
	ForkedFromSession string     // 分支血缘（期 2 §C 面包屑；fork 时记录）
	ForkedAtSeq       int64      // 分支点位（父会话事件水位）
}

// Run run 行（不可变 command + 终态投影）。
type Run struct {
	ID        string
	Input     string // 不可变 command（审计 #3 修复——重投按行重放）
	Topic     string
	SessionID string
	Status    sessionapi.RunStatus
	Bound     map[string]any
}

// EventRow 事件行（append-only 投影）。
type EventRow struct {
	ID        int64
	SessionID string
	RunID     string
	Seq       int64
	Type      event.Type
	Payload   json.RawMessage
	At        time.Time
}

// SessionDiff 事件差集（时间旅行分支对比）。
type SessionDiff struct {
	CommonPrefix int64      `json:"common_prefix"` // 公共事件数（以 A 为基准）
	OnlyA        []EventRow `json:"only_a"`
	OnlyB        []EventRow `json:"only_b"`
}
