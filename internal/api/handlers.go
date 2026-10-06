package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
	"github.com/bingdilotus/chronotope/internal/store"
)

// writeJSON 统一响应格式；错误码稳定（契约规范 §6）。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code int, msg string) {
	writeJSON(w, status, map[string]any{"code": code, "error": msg})
}

func notImplemented(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, 501, "not_implemented: "+r.URL.Path+"（后续周迭代实现，契约规范 §2）")
}

// POST /orgs/{orgID}/agents —— 创建 agent（config 全量，version+1 由 store 落库）。
func (h *Handler) createAgent(w http.ResponseWriter, r *http.Request) {
	orgID := chi.URLParam(r, "orgID")
	var req sessionapi.CreateAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		writeError(w, http.StatusUnprocessableEntity, 422, "invalid agent config")
		return
	}
	// MVP 单 org：org 幂等建立（多租户 quota 字段先预留，mvp-落地方案 §5）
	if err := h.Store.CreateOrg(r.Context(), orgID, orgID); err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	id := genID("a_")
	cfg := req.Config
	if err := h.Store.CreateAgent(r.Context(), id, orgID, req.Name, &cfg); err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "name": req.Name, "config": cfg})
}

// POST /agents/{agentID}/sessions —— 创建 session → ready（session_object 初始化）。
func (h *Handler) createSession(w http.ResponseWriter, r *http.Request) {
	agentID := chi.URLParam(r, "agentID")
	var req sessionapi.CreateSessionRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req) // 空体/旧客户端：普通会话
	}
	agent, err := h.Store.GetAgent(r.Context(), agentID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, 404, "agent not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	id := genID("s_")
	if err := h.Store.CreateSession(r.Context(), id, agent.OrgID, agentID); err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	// 会话状态机初始化（worker 控制面：session_object.Create，幂等）
	var state struct {
		Phase string `json:"phase"`
	}
	if err := h.Ingress.Call(r.Context(), "/session_object/"+id+"/Create", http.MethodPost, agent.Config, &state); err != nil {
		writeError(w, http.StatusServiceUnavailable, 503, "session_object unavailable: "+err.Error())
		return
	}
	if len(req.Participants) > 0 {
		// 群聊成员（moderator 主持循环依据；同侪共享消息日志）
		if err := h.Ingress.Call(r.Context(), "/session_object/"+id+"/SetParticipants",
			http.MethodPost, req.Participants, &state); err != nil {
			writeError(w, http.StatusServiceUnavailable, 503, "set participants failed: "+err.Error())
			return
		}
	}
	if err := h.Store.UpdateSessionStatus(r.Context(), id, sessionapi.PhaseReady); err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": id, "agent_id": agentID, "status": "ready",
		"participants": req.Participants,
	})
}

// POST /sessions/{sessionID}/runs —— 提交任务（Idempotency-Key 必带，幂等链第一跳）。
func (h *Handler) submitRun(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionID")
	idem := r.Header.Get("Idempotency-Key")
	if idem == "" {
		writeError(w, http.StatusBadRequest, 400, "Idempotency-Key header required（契约规范 §2）")
		return
	}
	sess, err := h.Store.GetSession(r.Context(), sessionID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && sess.DeletedAt != nil) {
		writeError(w, http.StatusNotFound, 404, "session not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	// 准入 ①：session 级限流（边界语义设计 §6：429 + Retry-After）
	if h.Limiter != nil {
		if ok, retry := h.Limiter.Take("session", sessionID); !ok {
			w.Header().Set("Retry-After", retry.String())
			writeError(w, http.StatusTooManyRequests, 429, "too many runs for this session, retry later")
			return
		}
	}
	// 准入 ②：同 session 双开 → 409 + active_run_id（边界语义设计 §6）
	if active, err := h.Store.GetActiveRun(r.Context(), sessionID); err == nil && active != nil {
		writeJSON(w, http.StatusConflict, map[string]any{
			"code": 409, "error": "session already has an active run",
			"active_run_id": active.ID,
		})
		return
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	agent, err := h.Store.GetAgent(r.Context(), sess.AgentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	var req sessionapi.SubmitRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Input == "" {
		writeError(w, http.StatusUnprocessableEntity, 422, "invalid run request（input 必填）")
		return
	}

	runID := runIDFromIdempotency(sessionID, idem)
	bound := map[string]any{
		"agent_config_version": agent.Version,
		"protocol_version":     "1.0",
		"model":                agent.Config.Model,
	}
	trigger, _ := json.Marshal(req.Trigger)
	created, err := h.Store.CreateRun(r.Context(), runID, sessionID, trigger, bound)
	if err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	if !created {
		// 幂等命中：返回既有 run，不重复触发（契约规范 §2）
		run, err := h.Store.GetRun(r.Context(), runID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, 500, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"run_id": runID, "status": run.Status, "idempotent": true})
		return
	}

	if err := h.Store.UpdateRunStatus(r.Context(), runID, sessionapi.RunRunning); err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}

	// worker 控制面：run_workflow/{run_id}/run（W1 为同步对话闭环）
	var out struct {
		Final string `json:"final"`
		Steps int    `json:"steps"`
	}
	err = h.Ingress.Call(r.Context(), "/run_workflow/"+runID+"/run", http.MethodPost,
		map[string]any{"session_id": sessionID, "input": req.Input, "topic": req.Topic}, &out)
	if err != nil {
		_ = h.Store.UpdateRunStatus(r.Context(), runID, sessionapi.RunFailed)
		writeError(w, http.StatusBadGateway, 502, "run_workflow failed: "+err.Error())
		return
	}
	_ = h.Store.UpdateRunStatus(r.Context(), runID, sessionapi.RunCompleted)
	writeJSON(w, http.StatusCreated, map[string]any{
		"run_id": runID, "status": "completed", "final": out.Final, "steps": out.Steps,
	})
}

// GET /sessions/{sessionID} —— session 状态 + 最近事件。
func (h *Handler) getSession(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionID")
	sess, err := h.Store.GetSession(r.Context(), sessionID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, 404, "session not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	rows, err := h.Store.ListEvents(r.Context(), sessionID, 0, 20)
	if err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	projected := make([]sseEvent, 0, len(rows))
	for _, row := range rows {
		projected = append(projected, projectEvent(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": sess.ID, "agent_id": sess.AgentID, "status": sess.Status,
		"last_active_at": sess.LastActiveAt, "recent_events": projected,
	})
}

// DELETE /sessions/{sessionID} —— tombstone（两段式删除第一步，边界语义 §4）。
func (h *Handler) deleteSession(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionID")
	if _, err := h.Store.GetSession(r.Context(), sessionID); err != nil {
		writeError(w, http.StatusNotFound, 404, "session not found")
		return
	}
	if err := h.Store.SoftDeleteSession(r.Context(), sessionID); err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /sessions/{sessionID}/actions —— 控制动作（契约规范 §2；经 worker 控制面转发）。
func (h *Handler) sessionAction(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionID")
	var req sessionapi.ActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, 422, "invalid action request")
		return
	}
	switch req.Action {
	case sessionapi.ActionPause, sessionapi.ActionResume, sessionapi.ActionWake:
		// 状态机迁移在 session_object（单写者串行）；handler 名与动作一一对应
		handler := map[sessionapi.ActionName]string{
			sessionapi.ActionPause:  "Pause",
			sessionapi.ActionResume: "Resume",
			sessionapi.ActionWake:   "Wake",
		}[req.Action]
		var state struct {
			Phase string `json:"phase"`
		}
		err := h.Ingress.Call(r.Context(), "/session_object/"+sessionID+"/"+handler,
			http.MethodPost, nil, &state)
		if err != nil {
			writeError(w, http.StatusBadGateway, 502, "session_object action failed: "+err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"session_id": sessionID, "action": req.Action, "phase": state.Phase})
	case sessionapi.ActionUnfreeze:
		// 充值后解析冻结（session_object 内部 resolve FrozenAwakeable 并清槽）
		var state struct {
			Phase string `json:"phase"`
		}
		err := h.Ingress.Call(r.Context(), "/session_object/"+sessionID+"/Unfreeze",
			http.MethodPost, nil, &state)
		if err != nil {
			writeError(w, http.StatusBadGateway, 502, "unfreeze failed: "+err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"session_id": sessionID, "action": req.Action, "phase": state.Phase})
	case sessionapi.ActionCancel, sessionapi.ActionSteer:
		writeError(w, http.StatusNotImplemented, 501, "action "+string(req.Action)+" 未实现（W3+）")
	default:
		writeError(w, http.StatusUnprocessableEntity, 422, "unknown action: "+string(req.Action))
	}
}

// POST /webhooks/approval/{runID} —— HITL 审批回调：转发 worker webhook 服务 resolve awakeable（幂等）。
func (h *Handler) approvalWebhook(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	var req struct {
		Payload string `json:"payload"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	var out string // webhook 服务返回 JSON 字符串 "resolved"
	err := h.Ingress.Call(r.Context(), "/webhook/Resolve", http.MethodPost,
		map[string]any{"run_id": runID, "payload": req.Payload}, &out)
	if err != nil {
		writeError(w, http.StatusBadGateway, 502, "webhook resolve failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": runID, "status": out})
}

// POST /sessions/{sessionID}/schedules —— 一次性定时唤醒（W3）：durable timer 到点
// → session_object.Wake → child run 执行 → 回睡。cron 表与时区语义后置（边界语义 §5）。
func (h *Handler) createSchedule(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionID")
	sess, err := h.Store.GetSession(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusNotFound, 404, "session not found")
		return
	}
	var req struct {
		DelayMs int64          `json:"delay_ms"`
		Payload map[string]any `json:"payload,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.DelayMs <= 0 {
		writeError(w, http.StatusUnprocessableEntity, 422, "invalid schedule request（delay_ms 必填且 >0）")
		return
	}
	id := genID("sch_")
	payload, _ := json.Marshal(req.Payload)
	if err := h.Store.CreateSchedule(r.Context(), id, sess.OrgID, sessionID, time.Duration(req.DelayMs)*time.Millisecond, payload); err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	var out struct {
		Woken bool   `json:"woken"`
		RunID string `json:"run_id"`
		Final string `json:"final"`
	}
	if err := h.Ingress.Call(r.Context(), "/scheduler/"+id+"/run", http.MethodPost,
		map[string]any{"session_id": sessionID, "delay_ms": req.DelayMs, "payload": req.Payload}, &out); err != nil {
		writeError(w, http.StatusBadGateway, 502, "scheduler failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"schedule_id": id, "session_id": sessionID, "woken": out.Woken, "run_id": out.RunID, "final": out.Final,
	})
}

// GET /sessions/{sessionID}/usage —— 三轴计量（活跃秒 / token / 计算秒，1min 桶）。
func (h *Handler) getUsage(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionID")
	if _, err := h.Store.GetSession(r.Context(), sessionID); err != nil {
		writeError(w, http.StatusNotFound, 404, "session not found")
		return
	}
	rows, err := h.Store.ListUsage(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	type bucketOut struct {
		Bucket         string  `json:"bucket"`
		ActiveSeconds  float64 `json:"active_seconds"`
		TokensIn       int64   `json:"tokens_in"`
		TokensOut      int64   `json:"tokens_out"`
		ComputeSeconds float64 `json:"compute_seconds"`
	}
	out := make([]bucketOut, 0, len(rows))
	for _, u := range rows {
		out = append(out, bucketOut{
			Bucket: u.Bucket.UTC().Format(time.RFC3339), ActiveSeconds: u.ActiveSeconds,
			TokensIn: u.TokensIn, TokensOut: u.TokensOut, ComputeSeconds: u.ComputeSeconds,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"session_id": sessionID, "buckets": out})
}

// GET /orgs/{orgID}/sessions —— 会话列表（控制台最小页；契约增量端点）。
func (h *Handler) listSessions(w http.ResponseWriter, r *http.Request) {
	orgID := chi.URLParam(r, "orgID")
	sessions, err := h.Store.ListSessionsByOrg(r.Context(), orgID, 50)
	if err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	type sessionOut struct {
		ID           string                  `json:"id"`
		AgentID      string                  `json:"agent_id"`
		Status       sessionapi.SessionPhase `json:"status"`
		LastActiveAt *time.Time              `json:"last_active_at"`
	}
	out := make([]sessionOut, 0, len(sessions))
	for _, sess := range sessions {
		out = append(out, sessionOut{ID: sess.ID, AgentID: sess.AgentID, Status: sess.Status, LastActiveAt: sess.LastActiveAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"org_id": orgID, "sessions": out})
}

// GET /sessions/{sessionID}/memory —— 分层记忆控制面（边界语义 §7）：主题摘要 + 记忆条目。
func (h *Handler) getMemory(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionID")
	if _, err := h.Store.GetSession(r.Context(), sessionID); err != nil {
		writeError(w, http.StatusNotFound, 404, "session not found")
		return
	}
	summaries, err := h.Store.ListSummaries(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	items, err := h.Store.ListMemoryItems(r.Context(), sessionID, "", 50)
	if err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	type summaryOut struct {
		Topic        string `json:"topic"`
		Version      int    `json:"version"`
		Summary      string `json:"summary"`
		CreatedByRun string `json:"created_by_run,omitempty"`
	}
	type itemOut struct {
		Topic       string `json:"topic"`
		Kind        string `json:"kind"`
		Content     string `json:"content"`
		SourceRunID string `json:"source_run_id,omitempty"`
		SourceStep  int    `json:"source_step"`
	}
	sOut := make([]summaryOut, 0, len(summaries))
	for _, s := range summaries {
		sOut = append(sOut, summaryOut{Topic: s.Topic, Version: s.Version, Summary: s.Summary, CreatedByRun: s.CreatedByRun})
	}
	iOut := make([]itemOut, 0, len(items))
	for _, it := range items {
		iOut = append(iOut, itemOut{Topic: it.Topic, Kind: it.Kind, Content: it.Content, SourceRunID: it.SourceRunID, SourceStep: it.SourceStep})
	}
	writeJSON(w, http.StatusOK, map[string]any{"session_id": sessionID, "summaries": sOut, "items": iOut})
}

// PUT /orgs/{orgID}/budget —— 更新 org 预算（三级熔断 ②；充值入口）。
func (h *Handler) updateOrgBudget(w http.ResponseWriter, r *http.Request) {
	orgID := chi.URLParam(r, "orgID")
	if _, err := h.Store.GetOrg(r.Context(), orgID); err != nil {
		writeError(w, http.StatusNotFound, 404, "org not found")
		return
	}
	var req struct {
		DailyTokenBudget   *float64 `json:"daily_token_budget"`
		DailyComputeBudget *float64 `json:"daily_compute_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, 422, "invalid budget request")
		return
	}
	quotas := map[string]any{}
	if req.DailyTokenBudget != nil {
		quotas[store.QuotaDailyTokenBudget] = *req.DailyTokenBudget
	}
	if req.DailyComputeBudget != nil {
		quotas[store.QuotaDailyComputeBudget] = *req.DailyComputeBudget
	}
	if len(quotas) == 0 {
		writeError(w, http.StatusUnprocessableEntity, 422, "no budget fields")
		return
	}
	if err := h.Store.UpdateOrgQuotas(r.Context(), orgID, quotas); err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"org_id": orgID, "quotas": quotas})
}

// POST /sessions/{sessionID}/mcp —— 注册 MCP 连接（worker 托管客户端；§11）。
// tools/list 由 run 时懒缓存；事件 mcp.connected。
func (h *Handler) connectMCP(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionID")
	var req struct {
		Server string `json:"server"`
		URL    string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Server == "" || req.URL == "" {
		writeError(w, http.StatusUnprocessableEntity, 422, "server 与 url 必填")
		return
	}
	if !strings.HasPrefix(req.URL, "http://") && !strings.HasPrefix(req.URL, "https://") {
		writeError(w, http.StatusUnprocessableEntity, 422, "url 必须为 http(s)://（MVP 仅 HTTP transport；stdio 沙箱内进程后置）")
		return
	}
	// session_ops 工作流：worker 发射 mcp.connected 事件 + 对象 ConnectMCP
	//（worker 是唯一事件写者；对象直调会丢事件）
	var out restateVoid
	if err := h.Ingress.Call(r.Context(), "/session_ops/ConnectMCP", http.MethodPost,
		map[string]string{"session_id": sessionID, "server": req.Server, "url": req.URL}, &out); err != nil {
		writeError(w, http.StatusServiceUnavailable, 503, "connect mcp failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"server": req.Server, "connected": true})
}

// POST /sessions/{sessionID}/skills —— 安装 skill（沙箱 skills/<name>/SKILL.md +
// 会话状态记录；事件 skill.install）。沙箱未创建时提示先提交一次 run。
func (h *Handler) installSkill(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionID")
	var req struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" || req.Content == "" {
		writeError(w, http.StatusUnprocessableEntity, 422, "name 与 content 必填")
		return
	}
	if strings.ContainsAny(req.Name, "/\\") {
		writeError(w, http.StatusUnprocessableEntity, 422, "name 不得含路径分隔符")
		return
	}
	// 沙箱懒创建（复用 run 路径的 ensure 语义：经 executor /sandboxes）
	if h.Executor == nil {
		writeError(w, http.StatusServiceUnavailable, 503, "executor client 未配置")
		return
	}
	sess, err := h.Store.GetSession(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusNotFound, 404, "session not found")
		return
	}
	agent, err := h.Store.GetAgent(r.Context(), sess.AgentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	sb := &struct {
		SandboxID string `json:"sandbox_id"`
	}{}
	if err := h.Executor.Call(r.Context(), "/sandboxes", http.MethodPost,
		map[string]any{"session_id": sessionID, "image": firstNonEmpty(agent.Config.Environment.Sandbox.Image, "python:3.11-slim")}, sb); err != nil {
		writeError(w, http.StatusServiceUnavailable, 503, "sandbox create failed: "+err.Error())
		return
	}
	if err := h.Executor.WriteFile(r.Context(), "/files/"+sb.SandboxID+"/workspace/skills/"+req.Name+"/SKILL.md", req.Content); err != nil {
		writeError(w, http.StatusServiceUnavailable, 503, "write skill failed: "+err.Error())
		return
	}
	// session_ops 工作流：worker 发射 skill.install 事件 + 对象 AddSkill
	var out restateVoid
	if err := h.Ingress.Call(r.Context(), "/session_ops/InstallSkill", http.MethodPost,
		map[string]string{"session_id": sessionID, "name": req.Name}, &out); err != nil {
		writeError(w, http.StatusServiceUnavailable, 503, "add skill failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"name": req.Name, "installed": true})
}

type restateVoid struct{}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// POST /orgs/{orgID}/keys —— 生成 API key（明文仅响应一次；库存 sha256 哈希）。
func (h *Handler) createAPIKey(w http.ResponseWriter, r *http.Request) {
	orgID := chi.URLParam(r, "orgID")
	if _, err := h.Store.GetOrg(r.Context(), orgID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, 404, "org not found")
			return
		}
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	plain, err := newAPIKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	if err := h.Store.CreateAPIKey(r.Context(), genID("k_"), orgID, sha256Hex(plain), []string{}); err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"org_id": orgID,
		"key":    plain, // 只显示一次：服务端仅存 sha256 哈希
		"note":   "请立即保存；再次请求将生成新 key。",
	})
}
