package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bingdilotus/chronotope/internal/core/contract"
	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
	"github.com/bingdilotus/chronotope/internal/store"
	"github.com/go-chi/chi/v5"
)

func decodeRequest(w http.ResponseWriter, r *http.Request, request any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(request); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("expected one JSON object")
	}
	return nil
}

func requestKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 256 {
		writeError(w, 400, 400, "Idempotency-Key must contain 1–256 bytes")
		return "", false
	}
	return key, true
}

func controlError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrIdempotencyConflict):
		writeErrorCode(w, 409, "idempotency_conflict", err.Error())
	case errors.Is(err, store.ErrSessionBusy):
		writeErrorCode(w, 409, "session_busy", err.Error())
	default:
		writeErrorCode(w, http.StatusInternalServerError, "internal_error", err.Error())
	}
}

func (h *Handler) createAgent(w http.ResponseWriter, r *http.Request) {
	orgID := chi.URLParam(r, "orgID")
	if !requireOrg(w, r, orgID) {
		return
	}
	key, ok := requestKey(w, r)
	if !ok {
		return
	}
	var req sessionapi.CreateAgentRequest
	if err := decodeRequest(w, r, &req); err != nil || strings.TrimSpace(req.Name) == "" || req.Config.Model == "" {
		writeErrorCode(w, 422, "validation_failed", "invalid agent configuration")
		return
	}
	if err := req.Config.ValidateApplication(); err != nil {
		writeErrorCode(w, 422, "validation_failed", err.Error())
		return
	}
	if req.Config.ApplicationProfile {
		tenant, ok, err := h.applicationBridgeTenant(r.Context(), req.Config.ApplicationBridgeRef)
		if err != nil {
			writeErrorCode(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		if !ok || tenant != orgID {
			writeErrorCode(w, 422, "application_bridge_unavailable", "application bridge is not configured for this tenant")
			return
		}
	}
	req.Config.Version = 1
	hash, err := store.RequestHash(req)
	if err != nil {
		controlError(w, err)
		return
	}
	if err := h.Store.CreateOrg(r.Context(), orgID, orgID); err != nil {
		controlError(w, err)
		return
	}
	id := resourceID("a_", orgID, key)
	created, err := h.Store.CreateAgentRequest(r.Context(), id, orgID, req.Name, &req.Config, hash)
	if err != nil {
		controlError(w, err)
		return
	}
	agent, err := h.Store.GetAgent(r.Context(), id)
	if err != nil {
		controlError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"id": id, "name": agent.Name, "config": agent.Config, "spec_digest": agent.SpecDigest, "idempotent": !created})
}

func (h *Handler) createSession(w http.ResponseWriter, r *http.Request) {
	key, ok := requestKey(w, r)
	if !ok {
		return
	}
	agentID := chi.URLParam(r, "agentID")
	agent, err := h.Store.GetAgent(r.Context(), agentID)
	if errors.Is(err, store.ErrNotFound) {
		writeErrorCode(w, 404, "not_found", "agent not found")
		return
	}
	if err != nil {
		controlError(w, err)
		return
	}
	if !requireOrg(w, r, agent.OrgID) {
		return
	}
	var req sessionapi.CreateSessionRequest
	if err := decodeRequest(w, r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeErrorCode(w, 422, "validation_failed", "invalid session request")
		return
	}
	if agent.Config.ApplicationProfile && (req.SubjectRef == "" || req.ScopeID == "" || len(req.Participants) > 0) {
		writeErrorCode(w, 422, "validation_failed", "application sessions require subject_ref/scope_id and do not accept participants")
		return
	}
	hash, err := store.RequestHash(req)
	if err != nil {
		controlError(w, err)
		return
	}
	id := resourceID("s_", agentID, key)
	created, err := h.Store.CreateSessionRequest(r.Context(), id, agent.OrgID, agentID, req.SubjectRef, req.ScopeID, hash)
	if err != nil {
		controlError(w, err)
		return
	}
	sess, err := h.Store.GetSession(r.Context(), id)
	if err != nil {
		controlError(w, err)
		return
	}
	if sess.DeletedAt != nil {
		writeErrorCode(w, 409, "conflict", "original session was deleted")
		return
	}
	if sess.Status == sessionapi.PhaseCreated {
		if err := h.Ingress.Call(r.Context(), "/session_object/"+id+"/Create", http.MethodPost, agent.Config, nil); err != nil {
			writeErrorCode(w, 503, "initialization_pending", "session initialization pending; retry with the same key")
			return
		}
		if len(req.Participants) > 0 {
			if err := h.Ingress.Call(r.Context(), "/session_object/"+id+"/SetParticipants", http.MethodPost, req.Participants, nil); err != nil {
				writeErrorCode(w, 503, "initialization_pending", "participant initialization pending; retry with the same key")
				return
			}
		}
		if err := h.Store.CompleteSessionInitialization(r.Context(), id); err != nil {
			controlError(w, err)
			return
		}
		sess, err = h.Store.GetSession(r.Context(), id)
		if err != nil {
			controlError(w, err)
			return
		}
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"id": id, "agent_id": agentID, "status": sess.Status, "subject_ref": sess.SubjectRef, "scope_id": sess.ScopeID, "participants": req.Participants, "idempotent": !created})
}

func (h *Handler) submitRun(w http.ResponseWriter, r *http.Request) {
	key, ok := requestKey(w, r)
	if !ok {
		return
	}
	sessionID := chi.URLParam(r, "sessionID")
	sess, err := h.Store.GetSession(r.Context(), sessionID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && sess.DeletedAt != nil) {
		writeErrorCode(w, 404, "not_found", "session not found")
		return
	}
	if err != nil {
		controlError(w, err)
		return
	}
	if !requireOrg(w, r, sess.OrgID) {
		return
	}
	var req sessionapi.SubmitRunRequest
	if err := decodeRequest(w, r, &req); err != nil || strings.TrimSpace(req.Input) == "" {
		writeErrorCode(w, 422, "validation_failed", "invalid run request (input required)")
		return
	}
	hash, err := store.RequestHash(req)
	if err != nil {
		controlError(w, err)
		return
	}
	id := runIDFromIdempotency(sessionID, key)
	if existing, err := h.Store.GetRun(r.Context(), id); err == nil {
		if existing.RequestHash != hash {
			controlError(w, store.ErrIdempotencyConflict)
			return
		}
		writeJSON(w, 200, map[string]any{"run_id": id, "status": existing.Status, "idempotent": true})
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		controlError(w, err)
		return
	}
	if sess.Status == sessionapi.PhaseCreated || sess.Status == sessionapi.PhasePaused || sess.Status == sessionapi.PhaseArchived {
		writeErrorCode(w, 409, "conflict", "session is not accepting new runs")
		return
	}
	agent, err := h.Store.GetAgent(r.Context(), sess.AgentID)
	if err != nil {
		controlError(w, err)
		return
	}
	if agent.Config.ApplicationProfile && (sess.OrgID == "" || sess.SubjectRef == "" || sess.ScopeID == "" || req.OriginCommandID == "") {
		writeErrorCode(w, 422, "validation_failed", "application run requires bound tenant/subject/scope and origin_command_id")
		return
	}
	if h.Limiter != nil {
		for _, bucket := range []struct{ kind, key string }{{"user", PrincipalFrom(r.Context())}, {"session", sessionID}} {
			if bucket.key == "" {
				continue
			}
			if ok, retry := h.Limiter.Take(bucket.kind, bucket.key); !ok {
				w.Header().Set("Retry-After", fmt.Sprintf("%.0f", retry.Seconds()+1))
				writeErrorCode(w, 429, "rate_limited", "run admission rate limited")
				return
			}
		}
	}
	maxDuration := agent.Config.MaxDurationMS
	if maxDuration <= 0 {
		maxDuration = sessionapi.DefaultMaxDurationMS
	}
	origin := req.OriginCommandID
	if origin == "" {
		origin = id
	}
	bound := map[string]any{"agent_config_version": agent.Version, "spec_digest": agent.SpecDigest, "agent_config": agent.Config, "protocol_version": "1.0", "application_contract_version": contract.ApplicationContractVersion, "model": agent.Config.Model, "tenant_id": sess.OrgID, "subject_ref": sess.SubjectRef, "scope_id": sess.ScopeID, "origin_command_id": origin, "deadline_at": time.Now().Add(time.Duration(maxDuration) * time.Millisecond).UTC().Format(time.RFC3339Nano)}
	if agent.Config.CredentialRef != "" {
		bound["credential_ref"] = agent.Config.CredentialRef
	}
	if len(agent.Config.Extensions) > 0 {
		bound["extensions"] = agent.Config.Extensions
	}
	if ref := agent.Config.HarnessRef; ref != nil && ref.Name != "" {
		harness, err := h.Store.GetActiveHarness(r.Context(), sess.OrgID, ref.Name)
		if err != nil {
			writeErrorCode(w, 422, "validation_failed", "configured harness is unavailable")
			return
		}
		bound["harness_endpoint"] = harness.Endpoint
		bound["harness_version"] = harness.Version
	}
	trigger, err := json.Marshal(req.Trigger)
	if err != nil {
		controlError(w, err)
		return
	}
	created, err := h.Store.AdmitRun(r.Context(), id, sessionID, req.Input, req.Topic, trigger, bound, hash)
	if errors.Is(err, store.ErrSessionBusy) {
		response := map[string]any{"code": 409, "error_code": "session_busy", "error": err.Error(), "category": "conflict", "retryable": true}
		if active, getErr := h.Store.GetActiveRun(r.Context(), sessionID); getErr == nil && active != nil {
			response["active_run_id"] = active.ID
		}
		writeJSON(w, 409, response)
		return
	}
	if err != nil {
		controlError(w, err)
		return
	}
	w.Header().Set("Location", "/runs/"+id)
	status := http.StatusAccepted
	runStatus := sessionapi.RunQueued
	if !created {
		status = http.StatusOK
		existing, getErr := h.Store.GetRun(r.Context(), id)
		if getErr != nil {
			controlError(w, getErr)
			return
		}
		runStatus = existing.Status
	}
	writeJSON(w, status, map[string]any{"run_id": id, "status": runStatus, "idempotent": !created})
}

func (h *Handler) getRun(w http.ResponseWriter, r *http.Request) {
	run, err := h.Store.GetRun(r.Context(), chi.URLParam(r, "runID"))
	if errors.Is(err, store.ErrNotFound) {
		writeErrorCode(w, 404, "not_found", "run not found")
		return
	}
	if err != nil {
		controlError(w, err)
		return
	}
	sess, err := h.Store.GetSession(r.Context(), run.SessionID)
	if err != nil {
		controlError(w, err)
		return
	}
	if !requireOrg(w, r, sess.OrgID) {
		return
	}
	response := map[string]any{"run_id": run.ID, "session_id": run.SessionID, "status": run.Status, "final": nil, "error": nil, "usage": nil, "bound": run.Bound}
	if len(run.Result) > 0 && string(run.Result) != "null" {
		var result map[string]any
		if err := json.Unmarshal(run.Result, &result); err != nil {
			controlError(w, err)
			return
		}
		for _, field := range []string{"final", "error", "usage", "steps", "truncated"} {
			if value, ok := result[field]; ok {
				response[field] = value
			}
		}
	}
	writeJSON(w, 200, response)
}
