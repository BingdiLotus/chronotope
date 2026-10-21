package targets

import (
	"context"
	"errors"
	"testing"

	"github.com/bingdilotus/chronotope/internal/appconfig"
	"github.com/bingdilotus/chronotope/internal/core/timer"
	"github.com/bingdilotus/chronotope/internal/store"
)

type fakeSource struct {
	rows map[string]store.ApplicationTarget
}

func newFakeSource() *fakeSource {
	return &fakeSource{rows: map[string]store.ApplicationTarget{}}
}

func (f *fakeSource) put(row store.ApplicationTarget) {
	row.Digest = store.ApplicationTargetDigest(row)
	f.rows[row.Kind+"/"+row.Ref] = row
}

func (f *fakeSource) GetApplicationTarget(_ context.Context, kind, ref string) (*store.ApplicationTarget, error) {
	row, ok := f.rows[kind+"/"+ref]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &row, nil
}

func (f *fakeSource) ListApplicationTargets(_ context.Context, tenantID string) ([]store.ApplicationTarget, error) {
	var out []store.ApplicationTarget
	for _, row := range f.rows {
		if row.TenantID == tenantID && row.State == "active" {
			out = append(out, row)
		}
	}
	return out, nil
}

func testRegistry(t *testing.T) (*Registry, *fakeSource) {
	t.Helper()
	t.Setenv("TARGET_TEST_TOKEN", "service-token")
	source := newFakeSource()
	static := appconfig.Config{
		Bridges: []appconfig.Bridge{{Ref: "static", TenantID: "tenant-a", URL: "http://static.example/tools", BearerToken: "static-token"}},
		TimerTargets: []timer.Target{{
			Ref: "static-timer", TenantID: "tenant-a", SubjectRef: "bot", ScopeID: "channel",
			CallbackURL: "http://static.example/fire", BearerToken: "static-token",
		}},
	}
	return New(static, source), source
}

func TestBridgeDynamicRegistrationAndRevocation(t *testing.T) {
	registry, source := testRegistry(t)
	source.put(store.ApplicationTarget{Kind: "bridge", Ref: "dynamic", TenantID: "tenant-a", URL: "http://dynamic.example/tools", TokenEnv: "TARGET_TEST_TOKEN", State: "active", Revision: 1})
	bridge, err := registry.ResolveBridge(context.Background(), "dynamic")
	if err != nil || bridge.URL != "http://dynamic.example/tools" || bridge.BearerToken != "service-token" {
		t.Fatalf("resolve dynamic bridge: %+v %v", bridge, err)
	}
	// A dynamic row shadowing a static ref fails closed.
	source.put(store.ApplicationTarget{Kind: "bridge", Ref: "static", TenantID: "tenant-a", URL: "http://attacker.example/tools", TokenEnv: "TARGET_TEST_TOKEN", State: "active", Revision: 1})
	if _, err := registry.ResolveBridge(context.Background(), "static"); !errors.Is(err, ErrStaticTargetConflict) {
		t.Fatalf("static shadow must fail closed: %v", err)
	}
	// Revocation is visible without restart.
	source.rows["bridge/dynamic"] = store.ApplicationTarget{Kind: "bridge", Ref: "dynamic", TenantID: "tenant-a", URL: "http://dynamic.example/tools", TokenEnv: "TARGET_TEST_TOKEN", State: "revoked", Revision: 2}
	if _, err := registry.ResolveBridge(context.Background(), "dynamic"); !errors.Is(err, ErrBridgeUnavailable) {
		t.Fatalf("revoked bridge must be unavailable: %v", err)
	}
	// Missing credential env fails closed rather than sending unauthenticated.
	source.put(store.ApplicationTarget{Kind: "bridge", Ref: "no-token", TenantID: "tenant-a", URL: "http://dynamic.example/tools", TokenEnv: "TARGET_MISSING_TOKEN", State: "active", Revision: 1})
	if _, err := registry.ResolveBridge(context.Background(), "no-token"); !errors.Is(err, ErrBridgeUnavailable) {
		t.Fatalf("missing token env must fail closed: %v", err)
	}
}

func TestTimerTargetResolutionTenantScopeAndFencingDigest(t *testing.T) {
	registry, source := testRegistry(t)
	req := timer.Request{TenantID: "tenant-a", SubjectRef: "bot", ScopeID: "channel", TimerID: "t", Revision: 1}
	static, staticDigest, err := registry.ResolveTimerTarget(context.Background(), req)
	if err != nil || static.Ref != "static-timer" || staticDigest == "" {
		t.Fatalf("static timer resolution: %+v %s %v", static, staticDigest, err)
	}
	source.put(store.ApplicationTarget{Kind: "timer", Ref: "dynamic-timer", TenantID: "tenant-a", URL: "http://dynamic.example/fire", SubjectRef: "bot-2", ScopeID: "channel-2", TokenEnv: "TARGET_TEST_TOKEN", State: "active", Revision: 1})
	req2 := req
	req2.SubjectRef, req2.ScopeID = "bot-2", "channel-2"
	dynamic, dynamicDigest, err := registry.ResolveTimerTarget(context.Background(), req2)
	if err != nil || dynamic.Ref != "dynamic-timer" || dynamicDigest == "" {
		t.Fatalf("dynamic timer resolution: %+v %s %v", dynamic, dynamicDigest, err)
	}
	// Cross tenant is unavailable even if the scope matches.
	cross := req2
	cross.TenantID = "tenant-b"
	if _, _, err := registry.ResolveTimerTarget(context.Background(), cross); !errors.Is(err, ErrTimerTargetUnavailable) {
		t.Fatalf("cross tenant timer target: %v", err)
	}
	// Same tenant/scope from two refs is ambiguous and must fail closed.
	source.put(store.ApplicationTarget{Kind: "timer", Ref: "dynamic-timer-2", TenantID: "tenant-a", URL: "http://other.example/fire", SubjectRef: "bot-2", ScopeID: "channel-2", TokenEnv: "TARGET_TEST_TOKEN", State: "active", Revision: 1})
	if _, _, err := registry.ResolveTimerTarget(context.Background(), req2); !errors.Is(err, ErrAmbiguousTarget) {
		t.Fatalf("ambiguous timer target: %v", err)
	}
	// Update changes the digest; an old timer row comparing its frozen digest
	// will be blocked by the worker.
	updated := source.rows["timer/dynamic-timer"]
	updated.URL = "http://moved.example/fire"
	source.put(updated)
	_, movedDigest, err := registry.ResolveTimerTargetByRef(context.Background(), "tenant-a", "dynamic-timer")
	if err != nil || movedDigest == dynamicDigest {
		t.Fatalf("timer route update must change digest: %s %s %v", dynamicDigest, movedDigest, err)
	}
}

func TestInvalidDynamicTimerNeverFallsBackToStaticReceiver(t *testing.T) {
	registry, source := testRegistry(t)
	// The static target also matches bot/channel. The dynamic row claims the
	// same scope but its credential is missing; resolution must fail closed,
	// not quietly use the static address.
	source.put(store.ApplicationTarget{Kind: "timer", Ref: "broken-dynamic", TenantID: "tenant-a",
		URL: "http://broken.example/fire", SubjectRef: "bot", ScopeID: "channel",
		TokenEnv: "TARGET_MISSING_TOKEN", State: "active", Revision: 1})
	req := timer.Request{TenantID: "tenant-a", SubjectRef: "bot", ScopeID: "channel", TimerID: "t", Revision: 1}
	if _, _, err := registry.ResolveTimerTarget(context.Background(), req); !errors.Is(err, ErrTimerTargetUnavailable) {
		t.Fatalf("invalid dynamic target fell through: %v", err)
	}
}
