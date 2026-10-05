// Package api 是 HTTP/SSE 网关（Session API，契约规范 §2）。
//
// 职责（mvp-架构设计 §3）：REST/SSE 网关；Agent/Session 管理；事件投影（SSE 分发）；
// 计量与 outbox 后置。api 不写事件（worker 是事件唯一写入者），只读投影。
// SSE 分发策略（落地方案 §5）：poller 轮询 events 表 → hub 发 (session_id, seq) 提示
// → 订阅者回查后推送；心跳 + after=seq 对账兜底；seq 允许 gap（LISTEN/NOTIFY 后置替换轮询）。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
	"github.com/bingdilotus/chronotope/internal/events"
	"github.com/bingdilotus/chronotope/internal/store"
)

// Store 是 api 侧 store 的最小接口（消费者侧定义；只读投影 + 控制面元数据）。
type Store interface {
	CreateOrg(ctx context.Context, id, name string) error
	CreateAgent(ctx context.Context, id, orgID, name string, cfg *sessionapi.AgentConfig) error
	GetAgent(ctx context.Context, id string) (*store.Agent, error)
	CreateSession(ctx context.Context, id, orgID, agentID string) error
	GetSession(ctx context.Context, id string) (*store.Session, error)
	UpdateSessionStatus(ctx context.Context, id string, status sessionapi.SessionPhase) error
	SoftDeleteSession(ctx context.Context, id string) error
	CreateRun(ctx context.Context, id, sessionID string, trigger json.RawMessage, bound map[string]any) (bool, error)
	GetRun(ctx context.Context, id string) (*store.Run, error)
	UpdateRunStatus(ctx context.Context, id string, status sessionapi.RunStatus) error
	ListEvents(ctx context.Context, sessionID string, afterSeq int64, limit int) ([]store.EventRow, error)
}

// RestateIngress 是 worker 控制面的最小接口（api → Restate ingress，worker-架构设计 §8）。
// 生产实现是 HTTP 客户端；测试用 httptest 替身。
type RestateIngress interface {
	Call(ctx context.Context, path, method string, body any, out any) error
}

// Handler 是网关依赖集。
type Handler struct {
	Store        Store
	Hub          *events.Hub
	Ingress      RestateIngress
	PollInterval time.Duration // 事件轮询间隔（默认 500ms）
	Logger       *slog.Logger
}

// New 构造默认配置的 Handler。
func New(st Store, hub *events.Hub, ingress RestateIngress) *Handler {
	return &Handler{
		Store:        st,
		Hub:          hub,
		Ingress:      ingress,
		PollInterval: 500 * time.Millisecond,
		Logger:       slog.Default(),
	}
}

// Router 挂载 Session API 路由（契约规范 §2）。
func (h *Handler) Router() chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Route("/orgs/{orgID}", func(r chi.Router) {
		r.Post("/agents", h.createAgent) // 创建/升级 agent（config 全量，version+1）
	})
	r.Route("/agents/{agentID}", func(r chi.Router) {
		r.Post("/sessions", h.createSession) // 创建 session → ready（沙箱懒创建）
	})
	r.Route("/sessions/{sessionID}", func(r chi.Router) {
		r.Get("/", h.getSession)            // session 状态 + 最近事件
		r.Get("/events", h.streamEvents)    // SSE 时间轴，after=seq 断线续读
		r.Post("/runs", h.submitRun)        // 提交任务；Idempotency-Key 必带
		r.Post("/actions", notImplemented)  // pause|resume|wake|cancel|steer（W3）
		r.Post("/messages", notImplemented) // 人类消息注入（W5+）
		r.Post("/skills", notImplemented)   // skill 安装（W6）
		r.Post("/mcp", notImplemented)      // MCP 连接（W6）
		r.Delete("/", h.deleteSession)      // tombstone 两段式删除（边界语义 §4）
		r.Get("/export", notImplemented)    // 标准 tar 导出（W8）
	})
	return r
}

// StartPoller 启动事件轮询（poller 模式；LISTEN/NOTIFY 后置替换）。
// 对每个有订阅者的 session：查 max(seq) 变化 → hub.Publish 提示。
func (h *Handler) StartPoller(ctx context.Context) {
	ticker := time.NewTicker(h.PollInterval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := h.pollOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
					h.Logger.Warn("event poll failed", "err", err)
				}
			}
		}
	}()
}

func (h *Handler) pollOnce(ctx context.Context) error {
	for _, sessionID := range h.Hub.SessionIDs() {
		rows, err := h.Store.ListEvents(ctx, sessionID, h.Hub.LastPublished(sessionID), 100)
		if err != nil {
			return fmt.Errorf("api: poll events for %s: %w", sessionID, err)
		}
		for _, row := range rows {
			h.Hub.Publish(sessionID, row.Seq)
		}
	}
	return nil
}
