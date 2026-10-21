package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/bingdilotus/chronotope/internal/core/timer"
	"github.com/bingdilotus/chronotope/internal/observability"
	"github.com/bingdilotus/chronotope/internal/store"
	"github.com/bingdilotus/chronotope/internal/targets"
)

// ApplicationTargetStore is the dynamic target management surface. *store.Store
// implements it; tests use a narrow fake.
type ApplicationTargetStore interface {
	UpsertApplicationTarget(context.Context, store.ApplicationTarget, string) (*store.ApplicationTarget, bool, error)
	ListApplicationTargets(context.Context, string) ([]store.ApplicationTarget, error)
	DeleteApplicationTarget(context.Context, string, string, string, string) error
}

type applicationTargetRequest struct {
	Kind       string `json:"kind"`
	Ref        string `json:"ref"`
	URL        string `json:"url"`
	SubjectRef string `json:"subject_ref"`
	ScopeID    string `json:"scope_id"`
	TokenEnv   string `json:"token_env"`
}

// requireTargetOperator limits dynamic receiver enrollment to the platform
// operator. Tenant keys can use resources but cannot silently point callbacks at
// a new address. Auth-off is the explicit development mode.
func (h *Handler) requireTargetOperator(w http.ResponseWriter, r *http.Request) bool {
	if h.AuthMode != "on" || AdminFrom(r.Context()) {
		return true
	}
	writeErrorCode(w, http.StatusForbidden, "admin_required", "dynamic application targets require the platform operator key")
	return false
}

func (h *Handler) upsertApplicationTarget(w http.ResponseWriter, r *http.Request) {
	if !h.requireTargetOperator(w, r) {
		return
	}
	orgID := chi.URLParam(r, "orgID")
	if !requireOrg(w, r, orgID) {
		return
	}
	var in applicationTargetRequest
	if err := decodeRequest(w, r, &in); err != nil {
		writeErrorCode(w, http.StatusBadRequest, "invalid_request", "invalid application target request")
		return
	}
	in.Kind = strings.TrimSpace(in.Kind)
	in.Ref = strings.TrimSpace(in.Ref)
	in.TokenEnv = strings.TrimSpace(in.TokenEnv)
	if in.Kind != "bridge" && in.Kind != "timer" {
		writeErrorCode(w, http.StatusUnprocessableEntity, "validation_failed", "kind must be bridge or timer")
		return
	}
	if in.Ref == "" || len(in.Ref) > 256 || strings.ContainsAny(in.Ref, "\x00\r\n/?#") {
		writeErrorCode(w, http.StatusUnprocessableEntity, "validation_failed", "ref is required (max 256 bytes; no /?#)")
		return
	}
	if in.TokenEnv == "" {
		writeErrorCode(w, http.StatusUnprocessableEntity, "validation_failed", "token_env must reference a nonempty environment credential; inline tokens are not accepted")
		return
	}
	if value, ok := os.LookupEnv(in.TokenEnv); !ok || strings.TrimSpace(value) == "" {
		writeErrorCode(w, http.StatusUnprocessableEntity, "validation_failed", "token_env does not resolve to a nonempty service credential")
		return
	}
	switch in.Kind {
	case "bridge":
		if in.SubjectRef != "" || in.ScopeID != "" {
			writeErrorCode(w, http.StatusUnprocessableEntity, "validation_failed", "bridge targets do not accept subject_ref/scope_id")
			return
		}
		if !targets.ValidBridgeURL(in.URL) {
			writeErrorCode(w, http.StatusUnprocessableEntity, "validation_failed", "bridge url must be an absolute http(s) URL without userinfo/fragment")
			return
		}
	case "timer":
		t := timer.Target{Ref: in.Ref, TenantID: orgID, SubjectRef: in.SubjectRef, ScopeID: in.ScopeID, CallbackURL: in.URL}
		if err := t.Validate(); err != nil {
			writeErrorCode(w, http.StatusUnprocessableEntity, "validation_failed", "timer target requires valid subject_ref/scope_id and an absolute http(s) callback url")
			return
		}
	}
	if h.TargetRegistry != nil && h.TargetRegistry.StaticRef(in.Kind, in.Ref) {
		writeErrorCode(w, http.StatusConflict, "target_ref_static", "target ref is owned by static deployment configuration")
		return
	}
	if in.Kind == "timer" && h.TargetRegistry != nil && h.TargetRegistry.StaticTimerScope(orgID, in.SubjectRef, in.ScopeID) {
		writeErrorCode(w, http.StatusConflict, "target_conflict", "timer tenant/subject/scope is owned by static deployment configuration")
		return
	}
	if h.ApplicationTargets == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "service_unavailable", "dynamic application targets are not enabled")
		return
	}
	if err := h.Store.CreateOrg(r.Context(), orgID, orgID); err != nil {
		writeErrorCode(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	target := store.ApplicationTarget{Kind: in.Kind, Ref: in.Ref, TenantID: orgID, URL: in.URL,
		SubjectRef: in.SubjectRef, ScopeID: in.ScopeID, TokenEnv: in.TokenEnv}
	row, created, err := h.ApplicationTargets.UpsertApplicationTarget(r.Context(), target, targetActor(r))
	if err != nil {
		switch {
		case errors.Is(err, store.ErrTargetImmutableRoute):
			writeErrorCode(w, http.StatusConflict, "target_route_immutable", "bridge route is immutable; enroll a new ref")
		case errors.Is(err, store.ErrTargetConflict):
			writeErrorCode(w, http.StatusConflict, "target_conflict", "target ref or tenant/scope conflicts with an existing target")
		default:
			writeErrorCode(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}
	action := "create"
	status := http.StatusCreated
	if !created {
		action = "update"
		status = http.StatusOK
	}
	observability.RecordTargetChange(row.Kind, action)
	slog.Default().Info("target.registered", "action", action, "tenant_id", row.TenantID, "kind", row.Kind,
		"ref", row.Ref, "revision", row.Revision, "actor", targetActor(r))
	writeJSON(w, status, map[string]any{"target": row, "created": created})
}

func (h *Handler) listApplicationTargets(w http.ResponseWriter, r *http.Request) {
	if !h.requireTargetOperator(w, r) {
		return
	}
	orgID := chi.URLParam(r, "orgID")
	if !requireOrg(w, r, orgID) {
		return
	}
	if h.ApplicationTargets == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "service_unavailable", "dynamic application targets are not enabled")
		return
	}
	rows, err := h.ApplicationTargets.ListApplicationTargets(r.Context(), orgID)
	if err != nil {
		writeErrorCode(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"targets": rows})
}

func (h *Handler) deleteApplicationTarget(w http.ResponseWriter, r *http.Request) {
	if !h.requireTargetOperator(w, r) {
		return
	}
	orgID := chi.URLParam(r, "orgID")
	if !requireOrg(w, r, orgID) {
		return
	}
	kind := chi.URLParam(r, "kind")
	ref := chi.URLParam(r, "ref")
	if r.URL.RawPath != "" {
		if unescaped, err := url.PathUnescape(ref); err == nil {
			ref = unescaped
		}
	}
	if kind != "bridge" && kind != "timer" {
		writeErrorCode(w, http.StatusBadRequest, "invalid_request", "kind must be bridge or timer")
		return
	}
	if h.ApplicationTargets == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "service_unavailable", "dynamic application targets are not enabled")
		return
	}
	err := h.ApplicationTargets.DeleteApplicationTarget(r.Context(), kind, ref, orgID, targetActor(r))
	if errors.Is(err, store.ErrNotFound) {
		writeErrorCode(w, http.StatusNotFound, "not_found", "application target not found")
		return
	}
	if err != nil {
		writeErrorCode(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	observability.RecordTargetChange(kind, "revoke")
	slog.Default().Info("target.revoked", "tenant_id", orgID, "kind", kind, "ref", ref, "actor", targetActor(r))
	w.WriteHeader(http.StatusNoContent)
}

func targetActor(r *http.Request) string {
	if principal := PrincipalFrom(r.Context()); principal != "" {
		return principal
	}
	if AdminFrom(r.Context()) {
		return "admin"
	}
	return "dev"
}

// applicationBridgeTenant resolves the owner of a bridge ref from the dynamic
// registry first, then falls back to the static bootstrap map. A dynamic row for
// a static ref fails closed (registry conflict), preventing shadowing.
func (h *Handler) applicationBridgeTenant(ctx context.Context, ref string) (string, bool, error) {
	if h.TargetRegistry != nil {
		bridge, err := h.TargetRegistry.ResolveBridge(ctx, ref)
		if errors.Is(err, targets.ErrBridgeUnavailable) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		return bridge.TenantID, true, nil
	}
	if tenant, ok := h.ApplicationBridgeTenants[ref]; ok {
		return tenant, true, nil
	}
	return "", false, nil
}
