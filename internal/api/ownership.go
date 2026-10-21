package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/bingdilotus/chronotope/internal/store"
	"github.com/go-chi/chi/v5"
)

// Authentication runs upstream. An empty authenticated org denotes the
// explicit development mode or the bootstrap administrator, not a body field.
func requireOrg(w http.ResponseWriter, r *http.Request, orgID string) bool {
	if org := OrgFrom(r.Context()); org != "" && org != orgID {
		writeErrorCode(w, http.StatusForbidden, "tenant_mismatch", "resource belongs to a different tenant")
		return false
	}
	return true
}

func (h *Handler) authorizeAgent(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent, err := h.Store.GetAgent(r.Context(), chi.URLParam(r, "agentID"))
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, 404, 404, "agent not found")
			return
		}
		if err != nil {
			controlError(w, err)
			return
		}
		if requireOrg(w, r, agent.OrgID) {
			next.ServeHTTP(w, r)
		}
	})
}

func (h *Handler) authorizeSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "sessionID")
		sess, err := h.Store.GetSession(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, 404, 404, "session not found")
			return
		}
		if err != nil {
			controlError(w, err)
			return
		}
		if !requireOrg(w, r, sess.OrgID) {
			return
		}
		agent, err := h.Store.GetAgent(r.Context(), sess.AgentID)
		if err != nil {
			controlError(w, err)
			return
		}
		if agent.Config.ApplicationProfile {
			suffix := strings.TrimPrefix(r.URL.Path, "/sessions/"+id)
			read := r.Method == http.MethodGet && (suffix == "" || suffix == "/" || suffix == "/events")
			submit := r.Method == http.MethodPost && suffix == "/runs"
			if !read && !submit {
				writeError(w, 403, 403, "endpoint is not enabled for application sessions")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) authorizeRun(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		run, err := h.Store.GetRun(r.Context(), chi.URLParam(r, "runID"))
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, 404, 404, "run not found")
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
		if requireOrg(w, r, sess.OrgID) {
			next.ServeHTTP(w, r)
		}
	})
}
