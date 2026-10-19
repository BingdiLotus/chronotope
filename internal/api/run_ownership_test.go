package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
	"github.com/bingdilotus/chronotope/internal/store"
)

type privateRunStore struct {
	*fakeStore
	privateReads int
}

func (s *privateRunStore) ListCallsForRun(context.Context, string) ([]map[string]any, error) {
	s.privateReads++
	return []map[string]any{{"result": "private-call-result"}}, nil
}

func (s *privateRunStore) ListAuditEventsByRun(context.Context, string, int) ([]store.AuditEventRow, error) {
	s.privateReads++
	return []store.AuditEventRow{{Payload: json.RawMessage(`{"result":"private-audit-result"}`)}}, nil
}

func TestRunAuditAndCallsRequireResourceTenant(t *testing.T) {
	h, fs, _ := setup(t)
	_, sessionID := seedAgentSession(t, h, fs)
	fs.runs["r_private"] = &store.Run{ID: "r_private", SessionID: sessionID, Status: sessionapi.RunCompleted}
	for _, entry := range []struct{ token, tenant string }{{"owner", "org_seed"}, {"other", "other_org"}} {
		fs.apiKeys = append(fs.apiKeys, &store.APIKeyRow{ID: entry.token, OrgID: entry.tenant, KeyHash: sha256Hex(entry.token)})
	}
	private := &privateRunStore{fakeStore: fs}
	h.Store = private
	router := h.AuthMiddleware("on", "")(h.Router())
	for _, suffix := range []string{"audit", "calls"} {
		t.Run(suffix, func(t *testing.T) {
			for _, entry := range []struct {
				token string
				want  int
			}{{"", http.StatusUnauthorized}, {"other", http.StatusForbidden}, {"owner", http.StatusOK}} {
				before := private.privateReads
				rec := doJSON(t, router, http.MethodGet, "/runs/r_private/"+suffix, "", map[string]string{"Authorization": "Bearer " + entry.token})
				if rec.Code != entry.want {
					t.Fatalf("token %q: status=%d body=%s", entry.token, rec.Code, rec.Body.String())
				}
				if entry.want != http.StatusOK {
					if private.privateReads != before || strings.Contains(rec.Body.String(), "private-") {
						t.Fatalf("unauthorized request read or exposed run data: %s", rec.Body.String())
					}
				} else if private.privateReads != before+1 || !strings.Contains(rec.Body.String(), "private-") {
					t.Fatalf("owner lost audit/call access: %s", rec.Body.String())
				}
			}
			rec := doJSON(t, router, http.MethodGet, "/runs/absent/"+suffix, "", map[string]string{"Authorization": "Bearer owner"})
			if rec.Code != http.StatusNotFound {
				t.Fatalf("missing run: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}
