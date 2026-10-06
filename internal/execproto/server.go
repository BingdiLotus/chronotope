package execproto

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/bingdilotus/chronotope/internal/store"
)

// maxInlineOutput 是 exec 结果内联上限（>此值外置到工作区文件，output_ref 引用——
// 与 journal 大小策略同源的「摘要 + 引用」纪律）。
const maxInlineOutput = 256 * 1024

// SandboxStore 是 executor 侧 store 接口（沙箱事实状态 + exec 幂等缓存，契约规范 §4）。
type SandboxStore interface {
	UpsertSandbox(ctx context.Context, sb *store.SandboxRow) error
	GetSandbox(ctx context.Context, sandboxID string) (*store.SandboxRow, error)
	UpdateSandboxStatus(ctx context.Context, sandboxID, status string) error
	UpdateSandboxTier(ctx context.Context, sandboxID string, tier int, snapshotRef *string) error
	GetExec(ctx context.Context, idempotencyKey string) (*store.ExecRow, error)
	PutExec(ctx context.Context, idempotencyKey, sandboxID string, result json.RawMessage) error
	// 执行状态机（评审 #1：prepared claim → done 落账）
	PutExecPrepared(ctx context.Context, idempotencyKey, sandboxID, inputDigest string) error
	PutExecDone(ctx context.Context, idempotencyKey, sandboxID string, result json.RawMessage) error
	// ComputeLease（正确性二期 ⑨）
	AcquireLease(ctx context.Context, sandboxID, runID string, ttl time.Duration) (*store.LeaseRow, error)
	ReleaseLease(ctx context.Context, sandboxID string, generation int64) (bool, error)
	HasActiveLease(ctx context.Context, sandboxID string) (bool, error)
	SessionOrg(ctx context.Context, sessionID string) (string, error)
	// 孤儿 GC（W8）：过期沙箱扫描 + 行删除。
	ListExpiredSandboxes(ctx context.Context, now time.Time) ([]*store.SandboxRow, error)
	DeleteSandbox(ctx context.Context, sandboxID string) error
}

// Server 实现 Executor 协议 HTTP 端点（契约规范 §4）。
type Server struct {
	Driver Driver
	Store  SandboxStore
	// WorkspaceRoot 与 docker driver 同源（大输出外置的宿主目录）。
	WorkspaceRoot string
	Logger        *slog.Logger
}

// GC 执行一轮孤儿清理（W8）：扫描 ttl 过期沙箱 → 销毁容器（尽力）→ 删行。
// 返回清理数；单沙箱失败不阻断其余。
func (s *Server) GC(ctx context.Context) (int, error) {
	expired, err := s.Store.ListExpiredSandboxes(ctx, time.Now())
	if err != nil {
		return 0, err
	}
	cleaned := 0
	for _, sb := range expired {
		// 容器名 = 沙箱 id（docker driver 约定）：无条件销毁（container_ref 缺失的
		// 历史行同样按 id 清理——否则孤儿容器永久残留，w8 e2e 实证）
		if err := s.Driver.Destroy(ctx, sb.SandboxID); err != nil {
			s.Logger.Warn("gc: destroy failed（行保留待下轮）", "sandbox", sb.SandboxID, "err", err)
			continue
		}
		if err := s.Store.DeleteSandbox(ctx, sb.SandboxID); err != nil {
			s.Logger.Warn("gc: delete row failed", "sandbox", sb.SandboxID, "err", err)
			continue
		}
		s.Logger.Info("gc: removed expired sandbox", "sandbox", sb.SandboxID, "ttl", sb.TTL)
		cleaned++
	}
	return cleaned, nil
}

// Router 挂载 executor 协议路由。
func (s *Server) Router() chi.Router {
	r := chi.NewRouter()
	r.Post("/sandboxes", s.createSandbox)
	r.Post("/sandboxes/{sandboxID}/lease", s.acquireLease)
	r.Delete("/sandboxes/{sandboxID}/lease", s.releaseLease)
	r.Post("/execute", s.execute)
	r.Get("/files/{sandboxID}/*", s.readFile)
	r.Put("/files/{sandboxID}/*", s.writeFile)
	r.Post("/sandboxes/{sandboxID}/freeze", s.freeze)
	r.Post("/sandboxes/{sandboxID}/unfreeze", s.unfreeze)
	r.Post("/sandboxes/{sandboxID}/snapshot", s.snapshot)
	r.Delete("/sandboxes/{sandboxID}", s.destroy)
	return r
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status, code int, msg string) {
	writeJSON(w, status, map[string]any{"code": code, "error": msg})
}

// POST /sandboxes —— 创建 → ready（沙箱事实状态入 PG，不随进程死）。
func (s *Server) createSandbox(w http.ResponseWriter, r *http.Request) {
	var req CreateSandboxRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Image == "" {
		writeError(w, http.StatusUnprocessableEntity, 422, "invalid sandbox request（image 必填）")
		return
	}
	sb, err := s.Driver.CreateSandbox(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	orgID, err := s.Store.SessionOrg(r.Context(), req.SessionID)
	if err != nil {
		orgID = "single-org" // MVP 单 org（worker 未传 session 时兜底）
	}
	row := &store.SandboxRow{
		SandboxID:     sb.ID,
		OrgID:         orgID,
		SessionID:     req.SessionID,
		Driver:        "docker",
		Image:         req.Image,
		Limits:        map[string]string{"cpu": req.Limits.CPU, "mem": req.Limits.Mem, "disk": req.Limits.Disk},
		FileSyncState: "syncing",
		Status:        "ready",
	}
	if req.TTL != "" {
		if ttl, err := time.ParseDuration(req.TTL); err == nil {
			row.TTL = &ttl
		}
	}
	if err := s.Store.UpsertSandbox(r.Context(), row); err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"sandbox_id": sb.ID, "status": "ready", "image": req.Image, "driver": "docker",
	})
}

// POST /execute —— 幂等缓存命中即回缓存；否则流式日志 + 结果（契约规范 §4）。
func (s *Server) execute(w http.ResponseWriter, r *http.Request) {
	var req ExecuteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SandboxID == "" || req.Input == "" {
		writeError(w, http.StatusUnprocessableEntity, 422, "invalid execute request")
		return
	}

	// 幂等缓存状态机（评审 #1 闭合）：
	//   done + digest 匹配 → 回缓存（SSE 同形流——客户端固定解析 SSE，评审 #2）
	//   done + digest 不匹配 → 400（同键不同输入是契约违约）
	//   prepared 新鲜（<2min）→ 409 in-flight（并发/未知窗口停派发）
	//   prepared 过期 → unknown：命令可能已执行——按「结果查询优先」原则重跑覆盖
	//     （exec 级语义：沙箱命令不可回查，重跑是唯一可确定路径；业务层由
	//     worker 的 Restate journal 兜底）
	if req.IdempotencyKey != "" {
		cached, err := s.Store.GetExec(r.Context(), req.IdempotencyKey)
		switch {
		case err == nil && cached.State == "done" && (cached.InputDigest == "" || cached.InputDigest == inputDigest(req)):
			// 缓存命中发同形 SSE 流（beat + exit），绝不发裸 JSON
			s.writeCacheHit(w, cached.Result)
			return
		case err == nil && cached.State == "done":
			writeError(w, http.StatusBadRequest, 400, "idempotency key 与输入摘要不匹配")
			return
		case err == nil && cached.PreparedAt != nil && time.Since(*cached.PreparedAt) < 2*time.Minute:
			writeError(w, http.StatusConflict, 409, "同键执行进行中（in-flight）")
			return
		case err == nil:
			s.Logger.Warn("exec prepared 过期视为 unknown，重跑覆盖", "key", req.IdempotencyKey)
		case !errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusInternalServerError, 500, err.Error())
			return
		}
	}

	sb, err := s.Store.GetSandbox(r.Context(), req.SandboxID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, 404, "sandbox not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	if sb.Status == "destroyed" {
		writeError(w, http.StatusConflict, 409, "sandbox destroyed")
		return
	}
	// ComputeLease（正确性二期 ⑨）：TTL 过期且无有效租约 → 409 lease expired
	//（worker 走重建路径）；有租约（挂起/等待的 run 持有）→ 放行——依赖安全
	if sb.TTL != nil && time.Since(sb.CreatedAt) > *sb.TTL {
		if active, err := s.Store.HasActiveLease(r.Context(), req.SandboxID); err != nil || !active {
			writeError(w, http.StatusConflict, 409, "sandbox lease expired")
			return
		}
	}

	// 执行前 claim（prepared；并发同键第二个请求在 GetExec 分支被 409）
	if req.IdempotencyKey != "" {
		if err := s.Store.PutExecPrepared(r.Context(), req.IdempotencyKey, req.SandboxID, inputDigest(req)); err != nil {
			writeError(w, http.StatusInternalServerError, 500, err.Error())
			return
		}
	}

	// SSE 流式日志 + exit 结果帧
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, 500, "streaming unsupported")
		return
	}
	s.Logger.Info("execute", "sandbox_id", req.SandboxID, "name", req.Name, "idempotency_key", req.IdempotencyKey)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	var output strings.Builder
	lineWriter := &sseLogWriter{out: &output, sink: w, flusher: fl}
	result, err := s.Driver.Execute(r.Context(), req, lineWriter)
	if err != nil {
		s.writeSSE(w, fl, map[string]any{"type": "error", "payload": map[string]any{"message": err.Error()}})
		return
	}
	lineWriter.Flush()

	// 结果「摘要 + 引用」：≤256KB 内联；更大外置工作区文件（MinIO 后置替换）
	cached := map[string]any{"exit": result.Exit}
	body := output.String()
	if len(body) <= maxInlineOutput {
		cached["output"] = body
	} else {
		ref, err := s.externalize(req.SandboxID, body)
		if err != nil {
			s.writeSSE(w, fl, map[string]any{"type": "error", "payload": map[string]any{"message": err.Error()}})
			return
		}
		cached["output_ref"] = ref
		cached["truncated_output"] = body[:maxInlineOutput]
	}
	if req.IdempotencyKey != "" {
		raw, _ := json.Marshal(cached)
		// 落账失败必须发 error 帧（不回成功 exit）：否则未知窗口内重试会双执行
		// （评审 #1——曾仅 Warn 后继续发成功 exit）
		if err := s.Store.PutExecDone(r.Context(), req.IdempotencyKey, req.SandboxID, raw); err != nil {
			s.Logger.Error("exec 落账失败（发 error 帧，重试由客户端 unknown 停派发）", "err", err)
			s.writeSSE(w, fl, map[string]any{"type": "error", "payload": map[string]any{"message": "exec result persistence failed: " + err.Error()}})
			return
		}
	}
	s.writeSSE(w, fl, map[string]any{"type": "exit", "payload": cached})
}

// POST /sandboxes/{sandboxID}/lease —— 获取/续约（body {run_id, ttl}；UPSERT
// generation+1）。DELETE 释放（body {run_id, generation}；代次校验）。
func (s *Server) acquireLease(w http.ResponseWriter, r *http.Request) {
	sandboxID := chi.URLParam(r, "sandboxID")
	var req struct {
		RunID string `json:"run_id"`
		TTL   string `json:"ttl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RunID == "" {
		writeError(w, http.StatusUnprocessableEntity, 422, "run_id 必填")
		return
	}
	ttl, err := time.ParseDuration(req.TTL)
	if err != nil || ttl <= 0 {
		ttl = 10 * time.Minute // 默认租约期
	}
	lease, err := s.Store.AcquireLease(r.Context(), sandboxID, req.RunID, ttl)
	if err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, lease)
}

func (s *Server) releaseLease(w http.ResponseWriter, r *http.Request) {
	sandboxID := chi.URLParam(r, "sandboxID")
	var req struct {
		Generation int64 `json:"generation"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	ok, err := s.Store.ReleaseLease(r.Context(), sandboxID, req.Generation)
	if err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusConflict, 409, "lease generation 不符（旧持有者）")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeCacheHit 缓存命中回放：同形 SSE 流（beat → exit 帧）。
// 客户端固定解析 SSE（评审 #2——曾发裸 JSON 致解析跳过、零值 exit=0 进 journal）。
func (s *Server) writeCacheHit(w http.ResponseWriter, cached json.RawMessage) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	s.writeSSE(w, fl, map[string]any{"type": "beat", "payload": map[string]any{"seq": 0}})
	s.writeSSE(w, fl, map[string]any{"type": "exit", "payload": json.RawMessage(cached)})
}

// inputDigest 计算同键输入摘要（sha256(工具名+输入)，16 hex）。
func inputDigest(req ExecuteRequest) string {
	sum := sha256.Sum256([]byte(req.Name + "\x00" + req.Input))
	return hex.EncodeToString(sum[:8])
}

// sseLogWriter 把 driver 原始字节按行包装为 {"type":"log"} SSE 帧，同时累积完整输出。
type sseLogWriter struct {
	out     *strings.Builder
	sink    http.ResponseWriter
	flusher http.Flusher
	buf     strings.Builder
}

func (l *sseLogWriter) Write(p []byte) (int, error) {
	l.buf.Write(p)
	for {
		line, rest, found := strings.Cut(l.buf.String(), "\n")
		if !found {
			break
		}
		l.buf.Reset()
		l.buf.WriteString(rest)
		raw, _ := json.Marshal(map[string]any{"type": "log", "payload": line})
		fmt.Fprintf(l.sink, "data: %s\n\n", raw)
		l.flusher.Flush()
		l.out.WriteString(line + "\n")
	}
	return len(p), nil
}

func (l *sseLogWriter) Flush() {
	if l.buf.Len() > 0 {
		raw, _ := json.Marshal(map[string]any{"type": "log", "payload": l.buf.String()})
		fmt.Fprintf(l.sink, "data: %s\n\n", raw)
		l.flusher.Flush()
		l.out.WriteString(l.buf.String())
		l.buf.Reset()
	}
}

func (s *Server) writeSSE(w http.ResponseWriter, fl http.Flusher, v any) {
	raw, _ := json.Marshal(v)
	fmt.Fprintf(w, "data: %s\n\n", raw)
	fl.Flush()
}

// externalize 把大输出写到宿主机工作区文件，返回 file:// 引用（MinIO 后置替换）。
func (s *Server) externalize(sandboxID, body string) (string, error) {
	path := fmt.Sprintf("%s/%s/output-%d.txt", strings.TrimRight(s.WorkspaceRoot, "/"), sandboxID, time.Now().UnixNano())
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", fmt.Errorf("externalize output: %w", err)
	}
	return "file://" + path, nil
}

// GET/PUT /files/{sandboxID}/* —— 文件快路径（不经过 shell，契约规范 §4）。
func (s *Server) readFile(w http.ResponseWriter, r *http.Request) {
	sandboxID := chi.URLParam(r, "sandboxID")
	// 行级存在性先查（统一 404 契约文本——客户端哨兵据此触发重建，评审 #7）
	if _, err := s.Store.GetSandbox(r.Context(), sandboxID); err != nil {
		writeError(w, http.StatusNotFound, 404, "sandbox not found")
		return
	}
	path := "/" + chi.URLParam(r, "*")
	data, err := s.Driver.ReadFile(r.Context(), sandboxID, path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(data)
}

func (s *Server) writeFile(w http.ResponseWriter, r *http.Request) {
	sandboxID := chi.URLParam(r, "sandboxID")
	if _, err := s.Store.GetSandbox(r.Context(), sandboxID); err != nil {
		writeError(w, http.StatusNotFound, 404, "sandbox not found")
		return
	}
	path := "/" + chi.URLParam(r, "*")
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20)) // 单文件 8MB 上限（MVP）
	if err != nil {
		writeError(w, http.StatusBadRequest, 400, err.Error())
		return
	}
	if err := s.Driver.WriteFile(r.Context(), sandboxID, path, body); err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Tier 1 冻结/解冻（docker pause/unpause，落地方案 §12）。
func (s *Server) freeze(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "sandboxID")
	if err := s.Driver.Freeze(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	_ = s.Store.UpdateSandboxStatus(r.Context(), id, "frozen")
	_ = s.Store.UpdateSandboxTier(r.Context(), id, 1, nil)
	writeJSON(w, http.StatusOK, map[string]string{"sandbox_id": id, "status": "frozen"})
}

func (s *Server) unfreeze(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "sandboxID")
	if err := s.Driver.Unfreeze(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	_ = s.Store.UpdateSandboxStatus(r.Context(), id, "ready")
	_ = s.Store.UpdateSandboxTier(r.Context(), id, 0, nil)
	writeJSON(w, http.StatusOK, map[string]string{"sandbox_id": id, "status": "ready"})
}

// Tier 2 快照（docker commit → snapshot_ref）。
func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "sandboxID")
	ref, err := s.Driver.Snapshot(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	_ = s.Store.UpdateSandboxStatus(r.Context(), id, "snapshotted")
	_ = s.Store.UpdateSandboxTier(r.Context(), id, 2, &ref)
	writeJSON(w, http.StatusOK, map[string]string{"sandbox_id": id, "snapshot_ref": ref})
}

// Tier 3 拆除（文件已增量同步 MinIO 是前提；W2 简化为直接销毁）。
func (s *Server) destroy(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "sandboxID")
	if err := s.Driver.Destroy(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	_ = s.Store.UpdateSandboxStatus(r.Context(), id, "destroyed")
	_ = s.Store.UpdateSandboxTier(r.Context(), id, 3, nil)
	w.WriteHeader(http.StatusNoContent)
}
