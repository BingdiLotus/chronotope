package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
	"github.com/bingdilotus/chronotope/internal/store"
)

func responseObject(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("invalid JSON response %d: %s: %v", rec.Code, rec.Body.String(), err)
	}
	return response
}

func TestCreateResourcesRequireIdempotencyKeys(t *testing.T) {
	h, fs, ing := setup(t)
	agentID, _ := seedAgentSession(t, h, fs)
	for _, req := range []struct{ path, body string }{
		{"/orgs/org_seed/agents", `{"name":"bot","config":{"model":"m"}}`},
		{"/agents/" + agentID + "/sessions", `{}`},
	} {
		rec := doJSON(t, h.Router(), http.MethodPost, req.path, req.body, map[string]string{"Idempotency-Key": ""})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("missing creation key must be 400: %s: %d %s", req.path, rec.Code, rec.Body.String())
		}
	}
	if len(fs.agents) != 1 || len(fs.sessions) != 1 || ing.callCount() != 0 {
		t.Fatal("missing key must not create resources or initialize a workflow")
	}
}

func TestCreateAgentIdempotencyAndPayloadConflict(t *testing.T) {
	h, fs, _ := setup(t)
	path := "/orgs/tenant/agents"
	headers := map[string]string{"Idempotency-Key": "immutable-agent"}
	first := doJSON(t, h.Router(), http.MethodPost, path, `{"name":"bot","config":{"model":"m","instructions":"original"}}`, headers)
	if first.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", first.Code, first.Body.String())
	}
	id := responseObject(t, first)["id"]
	// Formatting/order changes do not create a second immutable resource.
	retry := doJSON(t, h.Router(), http.MethodPost, path, `{"config":{"instructions":"original","model":"m"},"name":"bot"}`, headers)
	if retry.Code != http.StatusOK || responseObject(t, retry)["id"] != id || responseObject(t, retry)["idempotent"] != true {
		t.Fatalf("same request must return original ID: %d %s", retry.Code, retry.Body.String())
	}
	for _, body := range []string{
		`{"name":"other","config":{"model":"m","instructions":"original"}}`,
		`{"name":"bot","config":{"model":"m","instructions":"changed"}}`,
	} {
		rec := doJSON(t, h.Router(), http.MethodPost, path, body, headers)
		if rec.Code != http.StatusConflict || responseObject(t, rec)["error_code"] != "idempotency_conflict" {
			t.Fatalf("changed immutable request must conflict: %d %s", rec.Code, rec.Body.String())
		}
	}
	if len(fs.agents) != 1 || fs.agents[id.(string)].Config.Instructions != "original" || fs.agentRequestHashes[id.(string)] == "" {
		t.Fatal("idempotent creation must retain original config and hash")
	}
}

func TestApplicationAgentRequiresTenantBoundBridge(t *testing.T) {
	body := `{"name":"bot","config":{"model":"m","application_profile":true,"application_bridge_ref":"bots","application_tools":[{"model_alias":"knowledge_search","tool_ref":"knowledge.search","description":"Search allowed knowledge","input_schema":{"type":"object","properties":{}}}]}}`
	for _, entry := range []struct {
		name, owner string
		want        int
	}{
		{"unregistered", "", http.StatusUnprocessableEntity},
		{"another tenant", "other-org", http.StatusUnprocessableEntity},
		{"registered owner", "org-app", http.StatusCreated},
	} {
		t.Run(entry.name, func(t *testing.T) {
			h, fs, _ := setup(t)
			h.ApplicationBridgeTenants = map[string]string{"bots": entry.owner}
			rec := doJSON(t, h.Router(), http.MethodPost, "/orgs/org-app/agents", body, map[string]string{"Idempotency-Key": "app-create"})
			if rec.Code != entry.want {
				t.Fatalf("bridge registration: %d %s", rec.Code, rec.Body.String())
			}
			if entry.want != http.StatusCreated && len(fs.agents) != 0 {
				t.Fatal("rejected bridge must not persist agent")
			}
		})
	}
}

func TestCreateSessionIdempotencyPreservesContext(t *testing.T) {
	h, fs, ing := setup(t)
	agentID, _ := seedAgentSession(t, h, fs)
	path := "/agents/" + agentID + "/sessions"
	headers := map[string]string{"Idempotency-Key": "session-create"}
	body := `{"subject_ref":"bot-1","scope_id":"channel-1"}`
	first := doJSON(t, h.Router(), http.MethodPost, path, body, headers)
	if first.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", first.Code, first.Body.String())
	}
	id := responseObject(t, first)["id"].(string)
	retry := doJSON(t, h.Router(), http.MethodPost, path, `{"scope_id":"channel-1","subject_ref":"bot-1"}`, headers)
	if retry.Code != http.StatusOK || responseObject(t, retry)["id"] != id || ing.callCount() != 1 {
		t.Fatalf("retry must reuse session and initialization: %d %s calls=%v", retry.Code, retry.Body.String(), ing.allCalls())
	}
	for _, body := range []string{
		`{"subject_ref":"bot-2","scope_id":"channel-1"}`,
		`{"subject_ref":"bot-1","scope_id":"channel-2"}`,
		`{"subject_ref":"bot-1","scope_id":"channel-1","participants":[{"agent_id":"a_peer"}]}`,
	} {
		rec := doJSON(t, h.Router(), http.MethodPost, path, body, headers)
		if rec.Code != http.StatusConflict {
			t.Fatalf("changed session request must conflict: %d %s", rec.Code, rec.Body.String())
		}
	}
	session := fs.sessions[id]
	if session.SubjectRef != "bot-1" || session.ScopeID != "channel-1" || fs.sessionRequestHashes[id] == "" || len(fs.sessions) != 2 {
		t.Fatalf("original bound context must remain intact: %+v", session)
	}
}

func TestSessionInitializationUnknownRetriesOriginalResource(t *testing.T) {
	h, fs, ing := setup(t)
	agentID, _ := seedAgentSession(t, h, fs)
	path := "/agents/" + agentID + "/sessions"
	headers := map[string]string{"Idempotency-Key": "lost-create-response"}
	ing.failErr = context.DeadlineExceeded
	first := doJSON(t, h.Router(), http.MethodPost, path, `{}`, headers)
	if first.Code != http.StatusServiceUnavailable {
		t.Fatalf("initialization pending: %d %s", first.Code, first.Body.String())
	}
	id := resourceID("s_", agentID, headers["Idempotency-Key"])
	if fs.sessions[id] == nil || fs.sessions[id].Status != sessionapi.PhaseCreated {
		t.Fatal("failed initialization must retain original creation responsibility")
	}
	ing.failErr = nil
	retry := doJSON(t, h.Router(), http.MethodPost, path, `{}`, headers)
	if retry.Code != http.StatusOK || responseObject(t, retry)["id"] != id || len(fs.sessions) != 2 {
		t.Fatalf("retry must initialize original session: %d %s", retry.Code, retry.Body.String())
	}
	if fs.sessions[id].Status != sessionapi.PhaseReady || ing.callCount() != 2 || ing.allCalls()[0] != ing.allCalls()[1] {
		t.Fatalf("initialization retries must keep stable object identity: %v", ing.allCalls())
	}
}

func TestRunIdempotencyHashesCompleteRequest(t *testing.T) {
	h, fs, _ := setup(t)
	_, sessionID := seedAgentSession(t, h, fs)
	path := "/sessions/" + sessionID + "/runs"
	headers := map[string]string{"Idempotency-Key": "full-request"}
	body := `{"input":"remember","topic":"knowledge","trigger":{"source":"slack","event":{"id":"e1","channel":"c1"}},"origin_command_id":"origin-1"}`
	first := doJSON(t, h.Router(), http.MethodPost, path, body, headers)
	if first.Code != http.StatusAccepted {
		t.Fatalf("admit: %d %s", first.Code, first.Body.String())
	}
	id := responseObject(t, first)["run_id"].(string)
	reordered := `{"origin_command_id":"origin-1","trigger":{"event":{"channel":"c1","id":"e1"},"source":"slack"},"topic":"knowledge","input":"remember"}`
	retry := doJSON(t, h.Router(), http.MethodPost, path, reordered, headers)
	if retry.Code != http.StatusOK || responseObject(t, retry)["run_id"] != id {
		t.Fatalf("canonical retry: %d %s", retry.Code, retry.Body.String())
	}
	for _, replacement := range []struct{ before, after string }{
		{`"input":"remember"`, `"input":"forget"`},
		{`"topic":"knowledge"`, `"topic":"other"`},
		{`"id":"e1"`, `"id":"e2"`},
		{`"origin_command_id":"origin-1"`, `"origin_command_id":"origin-2"`},
	} {
		rec := doJSON(t, h.Router(), http.MethodPost, path, strings.Replace(body, replacement.before, replacement.after, 1), headers)
		if rec.Code != http.StatusConflict || responseObject(t, rec)["error_code"] != "idempotency_conflict" {
			t.Fatalf("complete request mismatch must conflict: %s: %d %s", replacement.before, rec.Code, rec.Body.String())
		}
	}
	run := fs.runs[id]
	if len(fs.runs) != 1 || run.Input != "remember" || run.Topic != "knowledge" || run.RequestHash == "" || run.Bound["origin_command_id"] != "origin-1" || !strings.Contains(string(fs.runTriggers[id]), `"id":"e1"`) {
		t.Fatalf("original command/hash/context must be durable: %+v trigger=%s", run, fs.runTriggers[id])
	}
}

func TestRunRetryPrecedesPausedAndRateLimitChecks(t *testing.T) {
	h, fs, _ := setup(t)
	_, sessionID := seedAgentSession(t, h, fs)
	h.Limiter = NewLimiter(0.0001, 1)
	path := "/sessions/" + sessionID + "/runs"
	headers := map[string]string{"Idempotency-Key": "retry-first"}
	first := doJSON(t, h.Router(), http.MethodPost, path, `{"input":"x"}`, headers)
	if first.Code != http.StatusAccepted {
		t.Fatalf("admit: %d %s", first.Code, first.Body.String())
	}
	fs.sessions[sessionID].Status = sessionapi.PhasePaused
	retry := doJSON(t, h.Router(), http.MethodPost, path, `{"input":"x"}`, headers)
	if retry.Code != http.StatusOK || responseObject(t, retry)["run_id"] != responseObject(t, first)["run_id"] {
		t.Fatalf("original request must survive busy/paused/exhausted limiter: %d %s", retry.Code, retry.Body.String())
	}
	changed := doJSON(t, h.Router(), http.MethodPost, path, `{"input":"x","topic":"changed"}`, headers)
	if changed.Code != http.StatusConflict || responseObject(t, changed)["error_code"] != "idempotency_conflict" {
		t.Fatalf("mismatched request must conflict before paused checks: %d %s", changed.Code, changed.Body.String())
	}
}

func TestRunBindsTrustedContextInsteadOfTriggerFields(t *testing.T) {
	h, fs, _ := setup(t)
	agentID, sessionID := seedAgentSession(t, h, fs)
	fs.agents[agentID].Config.ApplicationProfile = true
	fs.sessions[sessionID].SubjectRef = "bot-owned"
	fs.sessions[sessionID].ScopeID = "scope-owned"
	rec := doJSON(t, h.Router(), http.MethodPost, "/sessions/"+sessionID+"/runs",
		`{"input":"x","origin_command_id":"trusted-origin","trigger":{"tenant_id":"forged","subject_ref":"forged","scope_id":"forged"}}`, map[string]string{"Idempotency-Key": "bind-context"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("admit: %d %s", rec.Code, rec.Body.String())
	}
	bound := fs.runs[responseObject(t, rec)["run_id"].(string)].Bound
	if bound["tenant_id"] != "org_seed" || bound["subject_ref"] != "bot-owned" || bound["scope_id"] != "scope-owned" || bound["origin_command_id"] != "trusted-origin" {
		t.Fatalf("untrusted trigger must not override execution authority: %v", bound)
	}
}

func TestGetRunAuthenticatesTenantAndReturnsCanonicalResult(t *testing.T) {
	h, fs, _ := setup(t)
	_, sessionID := seedAgentSession(t, h, fs)
	fs.runs["r_private"] = &store.Run{ID: "r_private", SessionID: sessionID, Status: sessionapi.RunCompleted,
		Bound: map[string]any{"subject_ref": "private-bot"}, Result: json.RawMessage(`{"final":"private-result","error":null,"usage":{"tokens_in":7,"tokens_out":3}}`)}
	for _, entry := range []struct{ token, tenant string }{{"owner", "org_seed"}, {"other", "other_org"}} {
		fs.apiKeys = append(fs.apiKeys, &store.APIKeyRow{ID: entry.token, OrgID: entry.tenant, KeyHash: sha256Hex(entry.token)})
	}
	router := h.AuthMiddleware("on", "")(h.Router())
	for _, entry := range []struct {
		token string
		want  int
	}{{"", http.StatusUnauthorized}, {"other", http.StatusForbidden}, {"owner", http.StatusOK}} {
		rec := doJSON(t, router, http.MethodGet, "/runs/r_private", "", map[string]string{"Authorization": "Bearer " + entry.token})
		if rec.Code != entry.want {
			t.Fatalf("token %q: %d %s", entry.token, rec.Code, rec.Body.String())
		}
		if entry.want != http.StatusOK && (strings.Contains(rec.Body.String(), "private-result") || strings.Contains(rec.Body.String(), "private-bot")) {
			t.Fatalf("unauthorized query leaked result/context: %s", rec.Body.String())
		}
		if entry.want == http.StatusOK {
			result := responseObject(t, rec)
			if result["status"] != "completed" || result["final"] != "private-result" || result["usage"] == nil {
				t.Fatalf("canonical status/result must be visible together: %s", rec.Body.String())
			}
		}
	}
	missing := doJSON(t, router, http.MethodGet, "/runs/missing", "", map[string]string{"Authorization": "Bearer owner"})
	if missing.Code != http.StatusNotFound {
		t.Fatalf("unknown run: %d %s", missing.Code, missing.Body.String())
	}
}
