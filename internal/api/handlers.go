package api

import (
	"encoding/json"
	"errors"
	"net/http"

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
	if err := h.Store.UpdateSessionStatus(r.Context(), id, sessionapi.PhaseReady); err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "agent_id": agentID, "status": "ready"})
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
		map[string]any{"session_id": sessionID, "input": req.Input}, &out)
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
	writeJSON(w, http.StatusOK, map[string]any{
		"id": sess.ID, "agent_id": sess.AgentID, "status": sess.Status,
		"last_active_at": sess.LastActiveAt, "recent_events": rows,
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
