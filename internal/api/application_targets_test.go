package api

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"testing"

	"github.com/bingdilotus/chronotope/internal/appconfig"
	"github.com/bingdilotus/chronotope/internal/core/timer"
	"github.com/bingdilotus/chronotope/internal/store"
	"github.com/bingdilotus/chronotope/internal/targets"
)

type fakeTargetStore struct {
	mu   sync.Mutex
	rows map[string]store.ApplicationTarget
}

func newFakeTargetStore() *fakeTargetStore {
	return &fakeTargetStore{rows: map[string]store.ApplicationTarget{}}
}

func targetStoreKey(kind, ref string) string { return kind + "/" + ref }

func (f *fakeTargetStore) UpsertApplicationTarget(_ context.Context, target store.ApplicationTarget, _ string) (*store.ApplicationTarget, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := targetStoreKey(target.Kind, target.Ref)
	for otherKey, existing := range f.rows {
		if otherKey == key || existing.State != "active" {
			continue
		}
		if target.Kind == "timer" && existing.Kind == "timer" && existing.TenantID == target.TenantID &&
			existing.SubjectRef == target.SubjectRef && existing.ScopeID == target.ScopeID {
			return nil, false, store.ErrTargetConflict
		}
	}
	existing, ok := f.rows[key]
	if ok && existing.TenantID != target.TenantID {
		return nil, false, store.ErrTargetConflict
	}
	if ok && target.Kind == "bridge" && existing.URL != target.URL {
		return nil, false, store.ErrTargetImmutableRoute
	}
	created := !ok || existing.State == "revoked"
	revision := int64(1)
	if ok {
		revision = existing.Revision + 1
	}
	target.Revision = revision
	target.State = "active"
	target.Digest = store.ApplicationTargetDigest(target)
	f.rows[key] = target
	return &target, created, nil
}

func (f *fakeTargetStore) ListApplicationTargets(_ context.Context, tenantID string) ([]store.ApplicationTarget, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.ApplicationTarget
	for _, row := range f.rows {
		if row.TenantID == tenantID && row.State == "active" {
			out = append(out, row)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

func (f *fakeTargetStore) GetApplicationTarget(_ context.Context, kind, ref string) (*store.ApplicationTarget, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[targetStoreKey(kind, ref)]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &row, nil
}

func (f *fakeTargetStore) DeleteApplicationTarget(_ context.Context, kind, ref, tenantID, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := targetStoreKey(kind, ref)
	row, ok := f.rows[key]
	if !ok || row.TenantID != tenantID {
		return store.ErrNotFound
	}
	row.State = "revoked"
	row.Revision++
	f.rows[key] = row
	return nil
}

func targetManagementScenario(t *testing.T) (*Handler, *fakeTargetStore, *targets.Registry) {
	t.Helper()
	h, _, _ := setup(t)
	t.Setenv("P0_TARGET_TOKEN", "test-only-service-token")
	static := appconfig.Config{
		Bridges: []appconfig.Bridge{{Ref: "static-bridge", TenantID: "tenant-a", URL: "http://static.example/tools", BearerToken: "static-token"}},
		TimerTargets: []timer.Target{{
			Ref: "static-timer", TenantID: "tenant-a", SubjectRef: "bot-static", ScopeID: "scope-static",
			CallbackURL: "http://static.example/fire", BearerToken: "static-token",
		}},
	}
	fake := newFakeTargetStore()
	registry := targets.New(static, fake)
	h.TargetRegistry = registry
	h.ApplicationTargets = fake
	return h, fake, registry
}

func TestApplicationTargetRegistrationFencingAndTenantIsolation(t *testing.T) {
	h, fake, registry := targetManagementScenario(t)

	// Dynamic refs may not shadow deployment bootstrap.
	staticShadow := doJSON(t, h.Router(), http.MethodPost, "/orgs/tenant-a/application-targets",
		`{"kind":"bridge","ref":"static-bridge","url":"http://new.example/tools","token_env":"P0_TARGET_TOKEN"}`, nil)
	if staticShadow.Code != http.StatusConflict || responseObject(t, staticShadow)["error_code"] != "target_ref_static" {
		t.Fatalf("static ref shadow: %d %s", staticShadow.Code, staticShadow.Body.String())
	}
	staticTimerScope := doJSON(t, h.Router(), http.MethodPost, "/orgs/tenant-a/application-targets",
		`{"kind":"timer","ref":"static-timer-2","url":"http://app.example/fire","subject_ref":"bot-static","scope_id":"scope-static","token_env":"P0_TARGET_TOKEN"}`, nil)
	if staticTimerScope.Code != http.StatusConflict || responseObject(t, staticTimerScope)["error_code"] != "target_conflict" {
		t.Fatalf("static timer scope shadow: %d %s", staticTimerScope.Code, staticTimerScope.Body.String())
	}

	// Inline credentials are rejected; token_env must resolve.
	inline := doJSON(t, h.Router(), http.MethodPost, "/orgs/tenant-a/application-targets",
		`{"kind":"bridge","ref":"dynamic-bridge","url":"http://app.example/tools","token_env":"P0_MISSING_TOKEN"}`, nil)
	if inline.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing token env: %d %s", inline.Code, inline.Body.String())
	}
	badURL := doJSON(t, h.Router(), http.MethodPost, "/orgs/tenant-a/application-targets",
		`{"kind":"bridge","ref":"bad-url","url":"ftp://app.example/tools","token_env":"P0_TARGET_TOKEN"}`, nil)
	if badURL.Code != http.StatusUnprocessableEntity || responseObject(t, badURL)["error_code"] != "validation_failed" {
		t.Fatalf("invalid target URL: %d %s", badURL.Code, badURL.Body.String())
	}
	badTimer := doJSON(t, h.Router(), http.MethodPost, "/orgs/tenant-a/application-targets",
		`{"kind":"timer","ref":"bad-timer","url":"http://app.example/fire","token_env":"P0_TARGET_TOKEN"}`, nil)
	if badTimer.Code != http.StatusUnprocessableEntity {
		t.Fatalf("timer missing subject/scope: %d %s", badTimer.Code, badTimer.Body.String())
	}

	bridgeBody := `{"kind":"bridge","ref":"dynamic-bridge","url":"http://app.example/tools","token_env":"P0_TARGET_TOKEN"}`
	created := doJSON(t, h.Router(), http.MethodPost, "/orgs/tenant-a/application-targets", bridgeBody, nil)
	if created.Code != http.StatusCreated {
		t.Fatalf("create dynamic bridge: %d %s", created.Code, created.Body.String())
	}
	response := responseObject(t, created)
	target := response["target"].(map[string]any)
	if target["revision"] != float64(1) || target["state"] != "active" || target["token_env"] != "P0_TARGET_TOKEN" {
		t.Fatalf("target response shape: %v", target)
	}
	if _, leaked := target["bearer_token"]; leaked {
		t.Fatal("target response must not contain a token")
	}
	if resolved, err := registry.ResolveBridge(context.Background(), "dynamic-bridge"); err != nil || resolved.URL != "http://app.example/tools" {
		t.Fatalf("dynamic bridge not resolvable without restart: %+v %v", resolved, err)
	}
	agentBody := `{"name":"dynamic-bot","config":{"model":"m","application_profile":true,"application_bridge_ref":"dynamic-bridge","application_tools":[{"model_alias":"remember","tool_ref":"memory.remember","description":"Remember","input_schema":{"type":"object","properties":{}}}]}}`
	agent := doJSON(t, h.Router(), http.MethodPost, "/orgs/tenant-a/agents", agentBody, map[string]string{"Idempotency-Key": "dynamic-bridge-agent"})
	if agent.Code != http.StatusCreated {
		t.Fatalf("agent with dynamic bridge: %d %s", agent.Code, agent.Body.String())
	}
	crossAgent := doJSON(t, h.Router(), http.MethodPost, "/orgs/tenant-b/agents", agentBody, map[string]string{"Idempotency-Key": "dynamic-bridge-agent-b"})
	if crossAgent.Code != http.StatusUnprocessableEntity || responseObject(t, crossAgent)["error_code"] != "application_bridge_unavailable" {
		t.Fatalf("cross tenant dynamic bridge: %d %s", crossAgent.Code, crossAgent.Body.String())
	}

	updated := doJSON(t, h.Router(), http.MethodPost, "/orgs/tenant-a/application-targets",
		`{"kind":"bridge","ref":"dynamic-bridge","url":"http://app.example/tools","token_env":"P0_TARGET_TOKEN"}`, nil)
	if updated.Code != http.StatusOK || responseObject(t, updated)["target"].(map[string]any)["revision"] != float64(2) {
		t.Fatalf("bridge update: %d %s", updated.Code, updated.Body.String())
	}
	routeChange := doJSON(t, h.Router(), http.MethodPost, "/orgs/tenant-a/application-targets",
		`{"kind":"bridge","ref":"dynamic-bridge","url":"http://attacker.example/tools","token_env":"P0_TARGET_TOKEN"}`, nil)
	if routeChange.Code != http.StatusConflict || responseObject(t, routeChange)["error_code"] != "target_route_immutable" {
		t.Fatalf("bridge route change must be fenced: %d %s", routeChange.Code, routeChange.Body.String())
	}

	timerBody := `{"kind":"timer","ref":"dynamic-timer","url":"http://app.example/fire","subject_ref":"bot-1","scope_id":"channel-1","token_env":"P0_TARGET_TOKEN"}`
	timerCreated := doJSON(t, h.Router(), http.MethodPost, "/orgs/tenant-a/application-targets", timerBody, nil)
	if timerCreated.Code != http.StatusCreated {
		t.Fatalf("create dynamic timer: %d %s", timerCreated.Code, timerCreated.Body.String())
	}
	oldTimerDigest := responseObject(t, timerCreated)["target"].(map[string]any)["digest"].(string)
	req := timer.Request{TenantID: "tenant-a", SubjectRef: "bot-1", ScopeID: "channel-1", TimerID: "t1", Revision: 1}
	selected, digest, err := registry.ResolveTimerTarget(context.Background(), req)
	if err != nil || selected.Ref != "dynamic-timer" || digest != oldTimerDigest {
		t.Fatalf("timer target resolution: %+v %s %v", selected, digest, err)
	}
	changed := doJSON(t, h.Router(), http.MethodPost, "/orgs/tenant-a/application-targets",
		`{"kind":"timer","ref":"dynamic-timer","url":"http://moved.example/fire","subject_ref":"bot-1","scope_id":"channel-1","token_env":"P0_TARGET_TOKEN"}`, nil)
	if changed.Code != http.StatusOK {
		t.Fatalf("timer target update: %d %s", changed.Code, changed.Body.String())
	}
	newDigest := responseObject(t, changed)["target"].(map[string]any)["digest"].(string)
	if newDigest == oldTimerDigest {
		t.Fatal("timer route change must change the fencing digest")
	}
	current, currentDigest, err := registry.ResolveTimerTargetByRef(context.Background(), "tenant-a", "dynamic-timer")
	if err != nil || current.CallbackURL != "http://moved.example/fire" || currentDigest != newDigest {
		t.Fatalf("updated target resolution: %+v %s %v", current, currentDigest, err)
	}

	// A second active timer target for the same tenant/subject/scope is rejected.
	conflict := doJSON(t, h.Router(), http.MethodPost, "/orgs/tenant-a/application-targets",
		`{"kind":"timer","ref":"dynamic-timer-2","url":"http://other.example/fire","subject_ref":"bot-1","scope_id":"channel-1","token_env":"P0_TARGET_TOKEN"}`, nil)
	if conflict.Code != http.StatusConflict || responseObject(t, conflict)["error_code"] != "target_conflict" {
		t.Fatalf("timer scope conflict: %d %s", conflict.Code, conflict.Body.String())
	}

	// Cross-tenant revocation is not found; the row remains active.
	cross := doJSON(t, h.Router(), http.MethodDelete, "/orgs/tenant-b/application-targets/timer/dynamic-timer", "", nil)
	if cross.Code != http.StatusNotFound {
		t.Fatalf("cross tenant delete: %d %s", cross.Code, cross.Body.String())
	}
	if row, _ := fake.GetApplicationTarget(context.Background(), "timer", "dynamic-timer"); row == nil || row.State != "active" {
		t.Fatal("cross tenant delete changed target")
	}

	// Revocation is visible immediately and old timers resolve to unavailable.
	deleted := doJSON(t, h.Router(), http.MethodDelete, "/orgs/tenant-a/application-targets/timer/dynamic-timer", "", nil)
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete timer target: %d %s", deleted.Code, deleted.Body.String())
	}
	if _, _, err := registry.ResolveTimerTargetByRef(context.Background(), "tenant-a", "dynamic-timer"); err == nil {
		t.Fatal("revoked timer target must not resolve")
	}

	// Revoking and re-enrolling a bridge ref cannot change its route.
	if rec := doJSON(t, h.Router(), http.MethodDelete, "/orgs/tenant-a/application-targets/bridge/dynamic-bridge", "", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete bridge: %d %s", rec.Code, rec.Body.String())
	}
	reEnroll := doJSON(t, h.Router(), http.MethodPost, "/orgs/tenant-a/application-targets",
		`{"kind":"bridge","ref":"dynamic-bridge","url":"http://attacker.example/tools","token_env":"P0_TARGET_TOKEN"}`, nil)
	if reEnroll.Code != http.StatusConflict || responseObject(t, reEnroll)["error_code"] != "target_route_immutable" {
		t.Fatalf("re-enrolled bridge route change: %d %s", reEnroll.Code, reEnroll.Body.String())
	}
}

func TestApplicationTargetManagementRequiresOperator(t *testing.T) {
	h, _, _ := targetManagementScenario(t)
	h.AuthMode = "on"
	key := "ck_tenant_a"
	h.Store.(*fakeStore).apiKeys = []*store.APIKeyRow{{ID: "k", OrgID: "tenant-a", KeyHash: sha256Hex(key)}}
	router := h.AuthMiddleware("on", "")(h.Router())
	body := `{"kind":"bridge","ref":"operator-only","url":"http://app.example/tools","token_env":"P0_TARGET_TOKEN"}`
	rec := doJSON(t, router, http.MethodPost, "/orgs/tenant-a/application-targets", body, map[string]string{"Authorization": "Bearer " + key})
	if rec.Code != http.StatusForbidden || responseObject(t, rec)["error_code"] != "admin_required" {
		t.Fatalf("tenant key must not register targets: %d %s", rec.Code, rec.Body.String())
	}
}
