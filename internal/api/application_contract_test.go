package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bingdilotus/chronotope/internal/core/contract"
	"github.com/bingdilotus/chronotope/internal/store"
)

// TestApplicationContractAgentSessionRun exercises the public application
// contract without Bots or Slack: auth, tenant ownership, idempotency, run
// admission, busy semantics and the stable error envelope.
func TestApplicationContractAgentSessionRun(t *testing.T) {
	h, fs, _ := setup(t)
	fs.apiKeys = []*store.APIKeyRow{
		{ID: "k_a", OrgID: "tenant-a", KeyHash: sha256Hex("key-a")},
		{ID: "k_b", OrgID: "tenant-b", KeyHash: sha256Hex("key-b")},
	}
	h.AuthMode = "on"
	h.ApplicationBridgeTenants = map[string]string{"bots": "tenant-a"}
	router := h.AuthMiddleware("on", "")(h.Router())

	call := func(method, path, body, key string, headers map[string]string) *httptest.ResponseRecorder {
		merged := mergeHeaders(map[string]string{"Authorization": "Bearer " + key}, headers)
		rec := doJSON(t, router, method, path, body, merged)
		if rec.Header().Get(contract.VersionHeader) != contract.ApplicationContractVersion {
			t.Fatalf("%s %s missing contract version header: %v", method, path, rec.Header())
		}
		return rec
	}

	agentBody := `{"name":"bots-like","config":{"model":"fixture-model","instructions":"use only the registered tools","application_profile":true,"application_bridge_ref":"bots","application_tools":[{"model_alias":"knowledge_search","tool_ref":"knowledge.search","description":"Search application knowledge","input_schema":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}}],"max_steps":4,"max_output_tokens":512,"max_duration_ms":60000}}`
	headers := map[string]string{"Idempotency-Key": "contract-agent"}
	first := call(http.MethodPost, "/orgs/tenant-a/agents", agentBody, "key-a", headers)
	assertStatus(t, first, http.StatusCreated)
	agentID := objectOf(t, first)["id"].(string)

	conflict := call(http.MethodPost, "/orgs/tenant-a/agents", `{"name":"other","config":{"model":"fixture-model"}}`, "key-a", headers)
	assertErrorEnvelope(t, conflict, http.StatusConflict, "idempotency_conflict", contract.CategoryConflict, false)

	crossTenant := call(http.MethodPost, "/orgs/tenant-a/agents", agentBody, "key-b", map[string]string{"Idempotency-Key": "cross-tenant"})
	assertErrorEnvelope(t, crossTenant, http.StatusForbidden, "tenant_mismatch", contract.CategoryAuth, false)

	missingKey := call(http.MethodPost, "/orgs/tenant-a/agents", agentBody, "key-a", map[string]string{"Idempotency-Key": ""})
	assertErrorEnvelope(t, missingKey, http.StatusBadRequest, "invalid_request", contract.CategoryValidation, false)

	sessionBody := `{"subject_ref":"bot-1","scope_id":"channel-1"}`
	sessionHeaders := map[string]string{"Idempotency-Key": "contract-session"}
	session := call(http.MethodPost, "/agents/"+agentID+"/sessions", sessionBody, "key-a", sessionHeaders)
	assertStatus(t, session, http.StatusCreated)
	sessionID := objectOf(t, session)["id"].(string)
	if got := objectOf(t, session)["subject_ref"]; got != "bot-1" {
		t.Fatalf("session must freeze subject_ref: %v", got)
	}

	runBody := `{"input":"REMEMBER contract fixture","origin_command_id":"command-1"}`
	runHeaders := map[string]string{"Idempotency-Key": "contract-run"}
	run := call(http.MethodPost, "/sessions/"+sessionID+"/runs", runBody, "key-a", runHeaders)
	assertStatus(t, run, http.StatusAccepted)
	runID := objectOf(t, run)["run_id"].(string)
	runRow := call(http.MethodGet, "/runs/"+runID, "", "key-a", nil)
	assertStatus(t, runRow, http.StatusOK)
	bound, _ := objectOf(t, runRow)["bound"].(map[string]any)
	if bound["application_contract_version"] != contract.ApplicationContractVersion || bound["protocol_version"] != "1.0" {
		t.Fatalf("run must freeze contract/protocol version: %v", bound)
	}
	retry := call(http.MethodPost, "/sessions/"+sessionID+"/runs", runBody, "key-a", runHeaders)
	assertStatus(t, retry, http.StatusOK)
	if objectOf(t, retry)["run_id"] != runID || objectOf(t, retry)["idempotent"] != true {
		t.Fatalf("same key must return original run: %v", objectOf(t, retry))
	}

	busy := call(http.MethodPost, "/sessions/"+sessionID+"/runs", `{"input":"second logical command","origin_command_id":"command-2"}`, "key-a", map[string]string{"Idempotency-Key": "contract-busy"})
	assertErrorEnvelope(t, busy, http.StatusConflict, "session_busy", contract.CategoryConflict, true)
	if objectOf(t, busy)["active_run_id"] != runID {
		t.Fatalf("busy response must expose active_run_id: %v", objectOf(t, busy))
	}

	invalid := call(http.MethodPost, "/sessions/"+sessionID+"/runs", `{"input":""}`, "key-a", map[string]string{"Idempotency-Key": "contract-invalid"})
	assertErrorEnvelope(t, invalid, http.StatusUnprocessableEntity, "validation_failed", contract.CategoryValidation, false)

	foreignRun := call(http.MethodGet, "/runs/"+runID, "", "key-b", nil)
	assertErrorEnvelope(t, foreignRun, http.StatusForbidden, "tenant_mismatch", contract.CategoryAuth, false)

	missingRun := call(http.MethodGet, "/runs/run_missing", "", "key-a", nil)
	assertErrorEnvelope(t, missingRun, http.StatusNotFound, "not_found", contract.CategoryNotFound, false)

	// Application sessions expose only the read/events/submit-run surface.
	disabled := call(http.MethodPost, "/sessions/"+sessionID+"/actions", `{"action":"pause"}`, "key-a", nil)
	assertErrorEnvelope(t, disabled, http.StatusForbidden, "forbidden", contract.CategoryAuth, false)
}

func mergeHeaders(base, extra map[string]string) map[string]string {
	out := map[string]string{}
	for key, value := range base {
		out[key] = value
	}
	for key, value := range extra {
		out[key] = value
	}
	return out
}

func objectOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &object); err != nil {
		t.Fatalf("invalid JSON response %d: %s: %v", rec.Code, rec.Body.String(), err)
	}
	return object
}

func assertStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, want, rec.Body.String())
	}
}

func assertErrorEnvelope(t *testing.T, rec *httptest.ResponseRecorder, status int, errorCode string, category contract.Category, retryable bool) {
	t.Helper()
	assertStatus(t, rec, status)
	body := objectOf(t, rec)
	if body["error_code"] != errorCode || body["category"] != string(category) || body["retryable"] != retryable {
		t.Fatalf("unstable error envelope: %v", body)
	}
	if body["code"] != float64(status) || body["error"] == "" {
		t.Fatalf("legacy error fields must remain: %v", body)
	}
	if _, ok := contract.Lookup(errorCode); !ok {
		t.Fatalf("error code %q missing from catalog", errorCode)
	}
}
