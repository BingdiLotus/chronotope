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

	"github.com/bingdilotus/chronotope/internal/blobstore"
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
	GetActiveRun(ctx context.Context, sessionID string) (*store.Run, error)
	UpdateRunStatus(ctx context.Context, id string, status sessionapi.RunStatus) error
	CreateSchedule(ctx context.Context, id, orgID, sessionID string, delay time.Duration, payload json.RawMessage) error
	ListEvents(ctx context.Context, sessionID string, afterSeq int64, limit int) ([]store.EventRow, error)
	ListEventsAfterID(ctx context.Context, afterID int64, limit int) ([]store.EventRow, error)
	ListSessionsByOrg(ctx context.Context, orgID string, limit int) ([]store.Session, error)
	ListUsage(ctx context.Context, sessionID string) ([]store.UsageRow, error)
	ListMessages(ctx context.Context, sessionID string, limit int) ([]store.Message, error)
	// 交付清单（outbox，W8 后置）
	ListDeliverables(ctx context.Context, sessionID string, limit int) ([]*store.DeliverableRow, error)
	MarkDeliverableDelivered(ctx context.Context, id int64) error
	// 事件投递（outbox，落地方案 §5）
	Subscribe(ctx context.Context, sessionID, channel, target string) error
	// 期 2 §B：usage 增量 rollup + 冷层归档
	GetUsageWatermark(ctx context.Context) (int64, error)
	ApplyUsageDelta(ctx context.Context, fromID, toID int64, deltas []store.UsageRow) error
	RunStartedAt(ctx context.Context, runID string) (time.Time, error)
	CreateUser(ctx context.Context, u store.User) error
	UpsertApprovalPolicy(ctx context.Context, p store.ApprovalPolicy) error
	ListOrgAuditEvents(ctx context.Context, orgID, kind string, limit int) ([]store.AuditEvent, error)
	CreateArchive(ctx context.Context, a store.Archive) error
	GetArchive(ctx context.Context, sessionID string) (*store.Archive, error)
	SessionLastEventAt(ctx context.Context, sessionID string) (time.Time, error)
	// 接纳屏障（评审 #6：queued 遗留重投扫描）
	ListStaleQueuedRuns(ctx context.Context, olderThan time.Time, limit int) ([]*store.Run, error)
	// journal 审计导出（正式版架构 期 1）
	ListAuditEventsByRun(ctx context.Context, runID string, limit int) ([]store.AuditEventRow, error)
	// 时间旅行（期 2）
	LatestEventSeq(ctx context.Context, sessionID string) (int64, error)
	CreateCheckpoint(ctx context.Context, cp store.Checkpoint) (bool, error)
	GetCheckpoint(ctx context.Context, id string) (*store.Checkpoint, error)
	ListCheckpoints(ctx context.Context, sessionID string, limit int) ([]store.Checkpoint, error)
	ForkSession(ctx context.Context, newSessionID, parentSessionID string, atSeq int64, atCheckpoint string) error
	RollbackSession(ctx context.Context, sessionID string, cp *store.Checkpoint) error
	DiffSessions(ctx context.Context, a, b string, limit int) (*store.SessionDiff, error)
	ListPendingOutbox(ctx context.Context, limit int) ([]*store.PendingOutboxRow, error)
	GetEvent(ctx context.Context, eventID int64) (string, []byte, time.Time, error)
	OutboxDelivered(ctx context.Context, id int64) error
	OutboxRetry(ctx context.Context, id, attempts int64) error
	// 多租户认证（api_keys）
	GetAPIKeyByHash(ctx context.Context, keyHash string) (*store.APIKeyRow, error)
	CreateAPIKey(ctx context.Context, id, orgID, keyHash string, scopes []string) error
	CreateAPIKeyForUser(ctx context.Context, id, orgID, userID, keyHash string, scopes []string) error
	ListSummaries(ctx context.Context, sessionID string) ([]store.Summary, error)
	ListMemoryItems(ctx context.Context, sessionID, topic string, limit int) ([]store.MemoryItem, error)
	GetOrg(ctx context.Context, orgID string) (*store.Org, error)
	UpdateOrgQuotas(ctx context.Context, orgID string, quotas map[string]any) error
}

// RestateIngress 是 worker 控制面的最小接口（api → Restate ingress，worker-架构设计 §8）。
// 生产实现是 HTTP 客户端；测试用 httptest 替身。
type RestateIngress interface {
	Call(ctx context.Context, path, method string, body any, out any) error
}

// Handler 是网关依赖集。
type Handler struct {
	// Blob 是冷层归档的 S3 门面（期 2 §B；nil = 归档未启用）。
	Blob *blobstore.BlobStore
	// ArchiveMinAge 是归档前的最小无活动时长（默认 30 天）。
	ArchiveMinAge        time.Duration
	Store                Store
	DeliveryAllowPrivate bool // 事件投递 SSRF 放行（本地/e2e；生产拒绝）
	Hub                  *events.Hub
	Ingress              RestateIngress
	Executor             ExecutorClient // 沙箱文件写入（skill 安装）；nil = 禁用
	PollInterval         time.Duration  // 事件轮询间隔（默认 500ms）
	Logger               *slog.Logger
	Limiter              *Limiter // 三级限流（nil = 禁用；默认在 New 中启用 session 桶）
}

// ExecutorClient 是 executor 协议的最小客户端（api 侧 skill 安装用）。
// WriteFile 发原始字节体（executor 的 PUT /files 读裸 body，非 JSON 封装）。
type ExecutorClient interface {
	Call(ctx context.Context, path, method string, body, out any) error
	WriteFile(ctx context.Context, path, content string) error
}

// New 构造默认配置的 Handler。
func New(st Store, hub *events.Hub, ingress RestateIngress) *Handler {
	return &Handler{
		Store:        st,
		Hub:          hub,
		Ingress:      ingress,
		PollInterval: 500 * time.Millisecond,
		Logger:       slog.Default(),
		// session 桶默认：1 run/5s、突发 3（三级限流的 session 级；org/user 待身份体系）
		Limiter: NewLimiter(0.2, 3),
		// 归档（期 2 §B）：默认 30 天无活动才可归档；Blob 为空时端点 503
		ArchiveMinAge: 30 * 24 * time.Hour,
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
		r.Post("/agents", h.createAgent)                  // 创建/升级 agent（config 全量，version+1）
		r.Get("/sessions", h.listSessions)                // 会话列表（控制台最小页）
		r.Put("/budget", h.updateOrgBudget)               // org 预算（三级熔断 ②：充值入口）
		r.Post("/keys", h.createAPIKey)                   // 多租户认证：生成 key（明文仅此一次）
		r.Post("/users", h.createUser)                    // 技术主体（principal，期 3 §A）
		r.Put("/approval-policy", h.upsertApprovalPolicy) // 审批策略（期 3 §B 参考业务层）
		r.Get("/audit", h.orgAudit)                       // 审计导出（期 3 §B/C）
	})
	r.Route("/agents/{agentID}", func(r chi.Router) {
		r.Post("/sessions", h.createSession) // 创建 session → ready（沙箱懒创建）
	})
	r.Route("/sessions/{sessionID}", func(r chi.Router) {
		r.Get("/", h.getSession)                              // session 状态 + 最近事件
		r.Get("/events", h.streamEvents)                      // SSE 时间轴，after=seq 断线续读
		r.Post("/runs", h.submitRun)                          // 提交任务；Idempotency-Key 必带
		r.Post("/actions", h.sessionAction)                   // pause|resume|wake|cancel|steer
		r.Post("/schedules", h.createSchedule)                // 一次性定时唤醒（W3）
		r.Post("/messages", notImplemented)                   // 人类消息注入（W5+）
		r.Post("/skills", h.installSkill)                     // skill 安装（沙箱 skills/<name>/ + 事件）
		r.Post("/mcp", h.connectMCP)                          // MCP 连接（worker 托管客户端，§11）
		r.Delete("/", h.deleteSession)                        // tombstone 两段式删除（边界语义 §4）
		r.Get("/export", h.exportSession)                     // 标准 tar 导出（W8 交付物）
		r.Get("/usage", h.getUsage)                           // 三轴计量（活跃秒/token/计算秒，1min 桶）
		r.Get("/memory", h.getMemory)                         // 分层记忆（主题摘要 + 长期记忆条目，W5）
		r.Get("/deliveries", h.listDeliveries)                // 交付清单（outbox，W8 后置）
		r.Post("/deliveries/{deliveryID}/ack", h.ackDelivery) // 投递回执
		r.Post("/subscriptions", h.subscribe)                 // 事件投递订阅（webhook/email）
		// 时间旅行（正式版架构 期 2）——须在 /sessions/{sessionID} 子树内
		//（挂主 router 会被该子树 shadow——e2e 实证 404）
		r.Post("/checkpoints", h.createCheckpoint)
		r.Get("/checkpoints", h.listCheckpoints)
		r.Post("/fork", h.forkSession)
		r.Post("/rollback", h.rollbackSession)
		r.Get("/diff", h.diffSessions)
		r.Post("/archive", h.archiveSession) // 冷层归档（期 2 §B）
		r.Get("/archive", h.getArchive)
	})
	// HITL 审批回调（worker-架构设计 §2：webhook 服务；api 为对外入口）
	r.Get("/runs/{runID}/audit", h.runAudit) // journal 审计导出（期 1：重放轨迹 + dedupe 证据链）
	r.Post("/webhooks/approval/{runID}", h.approvalWebhook)
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
