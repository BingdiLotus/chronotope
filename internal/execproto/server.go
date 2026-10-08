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

	"github.com/bingdilotus/chronotope/internal/blobstore"
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
	// 执行状态机（评审 #1：prepared claim → done 落账）
	PutExecPrepared(ctx context.Context, idempotencyKey, sandboxID, inputDigest string) (bool, error)
	PutExecDone(ctx context.Context, idempotencyKey, sandboxID string, result json.RawMessage) error
	// ComputeLease（正确性二期 ⑨）
	AcquireLease(ctx context.Context, sandboxID, runID string, ttl time.Duration) (*store.LeaseRow, error)
	ReleaseLease(ctx context.Context, sandboxID string, generation int64) (bool, error)
	HasActiveLease(ctx context.Context, sandboxID string) (bool, error)
	GetLease(ctx context.Context, sandboxID string) (*store.LeaseRow, error)
	// 工作区 blob 合同（期 2 §A）
	UpsertWorkspaceFile(ctx context.Context, f store.WorkspaceFile) error
	ListWorkspaceFiles(ctx context.Context, sessionID string, limit int) ([]store.WorkspaceFile, error)
	SessionOrg(ctx context.Context, sessionID string) (string, error)
	// 孤儿 GC（W8）：过期沙箱扫描 + 行删除。
	ListExpiredSandboxes(ctx context.Context, now time.Time, executorID string) ([]*store.SandboxRow, error)
	DeleteSandbox(ctx context.Context, sandboxID string) error
}

// Server 实现 Executor 协议 HTTP 端点（契约规范 §4）。
type Server struct {
	Driver Driver
	Store  SandboxStore
	// Blob 是工作区 blob 合同（期 2 §A）；nil = 禁用（无 S3 时回退纯卷语义）。
	Blob *blobstore.BlobStore
	// WorkspaceRoot 与 docker driver 同源（大输出外置的宿主目录）。
	WorkspaceRoot string
	Logger        *slog.Logger
	// ExecutorID 本 executor 的注册 id（审计 P0-3：GC 扫描按归属过滤；空 = 全库）
	ExecutorID string
}

// GC 执行一轮孤儿清理（W8）：扫描 ttl 过期沙箱 → 销毁容器（尽力）→ 删行。
// 返回清理数；单沙箱失败不阻断其余。
func (s *Server) GC(ctx context.Context) (int, error) {
	expired, err := s.Store.ListExpiredSandboxes(ctx, time.Now(), s.ExecutorID)
	if err != nil {
		return 0, err
	}
	cleaned := 0
	for _, sb := range expired {
		// 审计 P0-3：销毁前二次 lease 校验（扫描与销毁间的窗口——扫描后新建
		// 的 lease 被拦下；无 quiesce 曾杀活跃执行）
		if active, lErr := s.Store.HasActiveLease(ctx, sb.SandboxID); lErr == nil && active {
			s.Logger.Warn("gc: 沙箱有活跃租约，跳过本轮", "sandbox", sb.SandboxID)
			continue
		}
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

// Sweep 启动清理（生命周期闭环 D2）：按 label 扫孤儿容器 vs DB 行——
// 无行/已 destroyed/TTL 过期 → 销毁（D1 连卷）；ready 未过期 → 保留交 GC。
// 崩溃窗口（容器建了行没写/行删了容器没删成）在此闭环。返回清理数。
func (s *Server) Sweep(ctx context.Context) (int, error) {
	names, err := s.Driver.ListOrphanContainers(ctx)
	if err != nil {
		return 0, err
	}
	cleaned := 0
	for _, id := range names {
		row, err := s.Store.GetSandbox(ctx, id)
		shouldDestroy := false
		switch {
		case errors.Is(err, store.ErrNotFound):
			shouldDestroy = true // 孤儿容器无行（崩溃窗口）
		case err != nil:
			s.Logger.Warn("sweep: get row failed", "sandbox", id, "err", err)
			continue
		case row.Status == "destroyed":
			shouldDestroy = true // 行已 destroyed 但容器未删成（历史尽力销毁）
		case row.TTL != nil && time.Since(row.CreatedAt) > *row.TTL:
			// 审计 P0-3：Sweep 的 TTL 分支查 lease（此前完全不查——可杀活跃执行）
			if active, lErr := s.Store.HasActiveLease(ctx, id); lErr == nil && active {
				continue
			}
			shouldDestroy = true // 已过期（GC 尚未轮到的残留）
		}
		if !shouldDestroy {
			continue
		}
		if err := s.Driver.Destroy(ctx, id); err != nil {
			s.Logger.Warn("sweep: destroy failed", "sandbox", id, "err", err)
			continue
		}
		if err == nil && row != nil {
			_ = s.Store.DeleteSandbox(ctx, id)
		}
		s.Logger.Info("sweep: removed orphan sandbox", "sandbox", id)
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
	// blob: 前缀（期 2 §A）：driver 不认该 ref——剥离后用普通镜像启动，
	// 创建完成后从索引拉取对象写回（快照之外第二条恢复链）
	blobRestore := false
	if strings.HasPrefix(req.RestoreFrom, "blob:") {
		blobRestore = true
		req.RestoreFrom = ""
	}
	s.Logger.Info("createSandbox: 请求", "session", req.SessionID, "blob_restore", blobRestore, "restore", req.RestoreFrom)
	sb, err := s.Driver.CreateSandbox(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	orgID, err := s.Store.SessionOrg(r.Context(), req.SessionID)
	if err != nil {
		// 异常路径（session 缺失）：org 归属空 + 告警（不再伪造 single-org——
		// 架构整洁 C4：租户隔离边界不因兜底串而混淆）
		s.Logger.Warn("createSandbox: session org 查询失败", "session", req.SessionID, "err", err)
	}
	if blobRestore {
		if rErr := s.restoreFromBlob(r.Context(), &store.SandboxRow{
			SandboxID: sb.ID, SessionID: req.SessionID,
		}); rErr != nil {
			writeError(w, http.StatusInternalServerError, 500, "blob restore failed: "+rErr.Error())
			return
		}
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
		"sandbox_id": sb.ID, "status": "ready", "image": req.Image, "driver": sb.Driver,
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
			// 审计 #2：prepared 过期重跑曾无条件执行（同 key 不同 input 的
			// 外部效果重复窗口）——safe replay 显式分类：同输入摘要且同沙箱
			// = 幂等重建场景（快照恢复后的重放）；否则 unknown 停派发 409
			sameInput := cached.InputDigest == "" || cached.InputDigest == inputDigest(req)
			if !sameInput || cached.SandboxID != req.SandboxID {
				s.Logger.Warn("exec prepared 过期且输入/沙箱不符——unknown 停派发", "key", req.IdempotencyKey)
				writeError(w, http.StatusConflict, 409, "执行状态 unknown（输入或沙箱与在途执行不符——请对账后重试）")
				return
			}
			s.Logger.Warn("exec prepared 过期视为 unknown，同输入同沙箱重跑（幂等重建）", "key", req.IdempotencyKey)
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
	//（worker 走重建路径）。审计 P0-1：RunID 非空时 lease 持有者必须匹配
	//（旧 owner 在 holder 换代后仍可 dispatch 的反例关闭）。
	if sb.TTL != nil && time.Since(sb.CreatedAt) > *sb.TTL {
		lease, err := s.Store.GetLease(r.Context(), req.SandboxID)
		if err != nil || lease == nil || time.Now().After(lease.ExpiresAt) {
			writeError(w, http.StatusConflict, 409, "sandbox lease expired")
			return
		}
		if req.RunID != "" && lease.RunID != req.RunID {
			writeError(w, http.StatusConflict, 409, "lease holder 不匹配（旧 owner 的 dispatch 被拒）")
			return
		}
	}

	// 执行前 claim（prepared）——获胜者执行；败者（并发同键第二个请求，
	// 都过 GetExec cache miss 后只有一人插入成功——审计 #1 双执行反例）409
	if req.IdempotencyKey != "" {
		won, err := s.Store.PutExecPrepared(r.Context(), req.IdempotencyKey, req.SandboxID, inputDigest(req))
		if err != nil {
			writeError(w, http.StatusInternalServerError, 500, err.Error())
			return
		}
		if !won {
			writeError(w, http.StatusConflict, 409, "同键执行进行中（claim 未获胜——in-flight）")
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

	// 结果「摘要 + 引用」：≤256KB 内联；更大外置工作区文件（RustFS 已落地）
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

// externalize 把大输出写到宿主机工作区文件，返回 file:// 引用（RustFS 已落地）。
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
	sb, err := s.Store.GetSandbox(r.Context(), sandboxID)
	if err != nil || sb.Status == "destroyed" {
		writeError(w, http.StatusNotFound, 404, "sandbox not found")
		return
	}
	path := "/" + chi.URLParam(r, "*")
	data, err := s.Driver.ReadFile(r.Context(), sandboxID, path)
	if errors.Is(err, ErrSandboxNotFound) {
		// 容器已不存在（行未同步）→ 404 哨兵（worker 恢复重建）
		writeError(w, http.StatusNotFound, 404, "sandbox not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(data)
}

func (s *Server) writeFile(w http.ResponseWriter, r *http.Request) {
	sandboxID := chi.URLParam(r, "sandboxID")
	sb, err := s.Store.GetSandbox(r.Context(), sandboxID)
	if err != nil {
		writeError(w, http.StatusNotFound, 404, "sandbox not found")
		return
	}
	if sb.Status == "destroyed" {
		// 已销毁 → 404 哨兵（worker 走恢复重建；blob 合同 e2e 实证 500 循环）
		s.Logger.Info("writeFile: destroyed 哨兵", "sandbox", sandboxID)
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
		if errors.Is(err, ErrSandboxNotFound) {
			writeError(w, http.StatusNotFound, 404, "sandbox not found")
			return
		}
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	// blob 合同（期 2 §A）：写后增量同步——本地暂存 → sha256 → PUT 对象
	//（幂等）→ 索引行 upsert；失败不阻断写（标记 syncing 重试窗口）。
	if s.Blob != nil {
		s.syncBlob(r.Context(), sandboxID, path, body)
	}
	w.WriteHeader(http.StatusNoContent)
}

// syncBlob 写路径增量同步（期 2 §A）：内容寻址 PUT + 索引行；失败仅告警
// （工具返回不阻断——state 标记 syncing，GC 周期重试是后置）。
func (s *Server) syncBlob(ctx context.Context, sandboxID, path string, body []byte) {
	sb, err := s.Store.GetSandbox(ctx, sandboxID)
	if err != nil || sb.SessionID == "" {
		return
	}
	orgID := ""
	if oid, oErr := s.Store.SessionOrg(ctx, sb.SessionID); oErr == nil && oid != "" {
		orgID = oid
	} else if oErr != nil {
		s.Logger.Warn("syncBlob: session org 查询失败", "session", sb.SessionID, "err", oErr)
	}
	tmp, err := os.CreateTemp("", "chronotope-blob-*")
	if err != nil {
		s.Logger.Warn("blob: temp", "err", err)
		return
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(body); err != nil || tmp.Close() != nil {
		s.Logger.Warn("blob: temp write", "err", err)
		return
	}
	hash, size, err := s.Blob.PutFile(ctx, orgID, sb.SessionID, tmp.Name())
	if err != nil {
		s.Logger.Warn("blob: put", "session", sb.SessionID, "path", path, "err", err)
		_ = s.Store.UpdateSandboxStatus(ctx, sandboxID, "file_syncing")
		return
	}
	if err := s.Store.UpsertWorkspaceFile(ctx, store.WorkspaceFile{
		SessionID: sb.SessionID, Path: path, Hash: hash, Size: size,
	}); err != nil {
		s.Logger.Warn("blob: index", "err", err)
		_ = s.Store.UpdateSandboxStatus(ctx, sandboxID, "file_syncing")
		return
	}
	_ = s.Store.UpdateSandboxStatus(ctx, sandboxID, "file_synced")
}

// restoreFromBlob 从索引拉取全部文件写回沙箱（blob: 前缀恢复；快照之外
// 第二条恢复链——销毁无快照也能重建工作区）。
func (s *Server) restoreFromBlob(ctx context.Context, sb *store.SandboxRow) error {
	if s.Blob == nil || sb == nil || sb.SessionID == "" {
		return fmt.Errorf("blob restore 不可用")
	}
	orgID := ""
	if oid, oErr := s.Store.SessionOrg(ctx, sb.SessionID); oErr == nil && oid != "" {
		orgID = oid
	} else if oErr != nil {
		s.Logger.Warn("restoreFromBlob: session org 查询失败", "session", sb.SessionID, "err", oErr)
	}
	files, err := s.Store.ListWorkspaceFiles(ctx, sb.SessionID, 500)
	if err != nil {
		return fmt.Errorf("blob: list index: %w", err)
	}
	s.Logger.Info("restoreFromBlob: 索引行数", "session", sb.SessionID, "files", len(files), "org", orgID)
	for _, f := range files {
		s.Logger.Info("restoreFromBlob: 恢复文件", "path", f.Path, "hash", f.Hash, "size", f.Size)
		tmp, err := os.CreateTemp("", "chronotope-blob-get-*")
		if err != nil {
			return err
		}
		func() { _ = tmp.Close() }()
		if err := s.Blob.GetFile(ctx, orgID, sb.SessionID, f.Hash, tmp.Name()); err != nil {
			os.Remove(tmp.Name())
			return fmt.Errorf("blob: get %s: %w", f.Path, err)
		}
		data, err := os.ReadFile(tmp.Name())
		os.Remove(tmp.Name())
		if err != nil {
			return err
		}
		if err := s.Driver.WriteFile(ctx, sb.SandboxID, f.Path, data); err != nil {
			return fmt.Errorf("blob: write sandbox %s: %w", f.Path, err)
		}
	}
	return nil
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

// Tier 3 拆除（文件已增量同步 RustFS 是前提；W2 简化为直接销毁）。
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
