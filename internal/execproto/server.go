package execproto

import (
	"context"
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
	SessionOrg(ctx context.Context, sessionID string) (string, error)
}

// Server 实现 Executor 协议 HTTP 端点（契约规范 §4）。
type Server struct {
	Driver Driver
	Store  SandboxStore
	// WorkspaceRoot 与 docker driver 同源（大输出外置的宿主目录）。
	WorkspaceRoot string
	Logger        *slog.Logger
}

// Router 挂载 executor 协议路由。
func (s *Server) Router() chi.Router {
	r := chi.NewRouter()
	r.Post("/sandboxes", s.createSandbox)
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

	// 幂等缓存（防重试双执行）：同键重发返回缓存结果，绝不复跑命令
	if req.IdempotencyKey != "" {
		if cached, err := s.Store.GetExec(r.Context(), req.IdempotencyKey); err == nil {
			writeJSON(w, http.StatusOK, json.RawMessage(cached.Result))
			return
		} else if !errors.Is(err, store.ErrNotFound) {
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
	if sb.TTL != nil && time.Since(sb.CreatedAt) > *sb.TTL {
		writeError(w, http.StatusConflict, 409, "sandbox expired（TTL 空闲回收）")
		return
	}

	// SSE 流式日志 + exit 结果帧
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, 500, "streaming unsupported")
		return
	}
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
		if err := s.Store.PutExec(r.Context(), req.IdempotencyKey, req.SandboxID, raw); err != nil {
			s.Logger.Warn("put exec cache failed", "err", err)
		}
	}
	s.writeSSE(w, fl, map[string]any{"type": "exit", "payload": cached})
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
	path := "/" + chi.URLParam(r, "*")
	data, err := s.Driver.ReadFile(r.Context(), sandboxID, path)
	if err != nil {
		writeError(w, http.StatusNotFound, 404, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(data)
}

func (s *Server) writeFile(w http.ResponseWriter, r *http.Request) {
	sandboxID := chi.URLParam(r, "sandboxID")
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
