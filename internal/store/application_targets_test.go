package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bingdilotus/chronotope/internal/appconfig"
	"github.com/bingdilotus/chronotope/internal/store"
	"github.com/bingdilotus/chronotope/internal/targets"
)

func TestApplicationTargetRegistryAndFencing(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	org := "org_target_" + randSuffix()
	other := "org_target_other_" + randSuffix()
	if err := s.CreateOrg(ctx, org, "target tests"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOrg(ctx, other, "other target tests"); err != nil {
		t.Fatal(err)
	}
	bridge := store.ApplicationTarget{Kind: "bridge", Ref: "bridge-" + org, TenantID: org,
		URL: "http://app.example/tools", TokenEnv: "APP_TEST_TOKEN"}
	created, fresh, err := s.UpsertApplicationTarget(ctx, bridge, "test-actor")
	if err != nil || !fresh {
		t.Fatalf("create bridge: %+v %v %v", created, fresh, err)
	}
	if created.Revision != 1 || created.Digest == "" || created.State != "active" {
		t.Fatalf("created target shape: %+v", created)
	}
	firstDigest := created.Digest
	rotated := bridge
	rotated.TokenEnv = "APP_TEST_TOKEN_ROTATED"
	updated, fresh, err := s.UpsertApplicationTarget(ctx, rotated, "test-actor")
	if err != nil || fresh || updated.Revision != 2 {
		t.Fatalf("credential rotation: %+v %v %v", updated, fresh, err)
	}
	if updated.Digest != firstDigest {
		t.Fatal("credential rotation must not change routing digest")
	}
	routeChange := bridge
	routeChange.URL = "http://attacker.example/tools"
	if _, _, err := s.UpsertApplicationTarget(ctx, routeChange, "test-actor"); !errors.Is(err, store.ErrTargetImmutableRoute) {
		t.Fatalf("bridge route change must be rejected: %v", err)
	}

	timerTarget := store.ApplicationTarget{Kind: "timer", Ref: "timer-" + org, TenantID: org,
		URL: "http://app.example/fire", SubjectRef: "bot", ScopeID: "channel", TokenEnv: "APP_TEST_TOKEN"}
	timerRow, _, err := s.UpsertApplicationTarget(ctx, timerTarget, "test-actor")
	if err != nil {
		t.Fatal(err)
	}
	oldDigest := timerRow.Digest
	conflict := timerTarget
	conflict.Ref = "timer-other-" + org
	conflict.URL = "http://other.example/fire"
	if _, _, err := s.UpsertApplicationTarget(ctx, conflict, "test-actor"); !errors.Is(err, store.ErrTargetConflict) {
		t.Fatalf("duplicate tenant/scope must conflict: %v", err)
	}
	moved := timerTarget
	moved.URL = "http://moved.example/fire"
	movedRow, _, err := s.UpsertApplicationTarget(ctx, moved, "test-actor")
	if err != nil || movedRow.Digest == oldDigest || movedRow.Revision != 2 {
		t.Fatalf("timer route update: %+v %v", movedRow, err)
	}
	if err := s.DeleteApplicationTarget(ctx, "timer", timerTarget.Ref, other, "test-actor"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross tenant delete: %v", err)
	}
	if err := s.DeleteApplicationTarget(ctx, "timer", timerTarget.Ref, org, "test-actor"); err != nil {
		t.Fatal(err)
	}
	revoked, err := s.GetApplicationTarget(ctx, "timer", timerTarget.Ref)
	if err != nil || revoked.State != "revoked" || revoked.Revision != 3 {
		t.Fatalf("revocation row: %+v %v", revoked, err)
	}
	list, err := s.ListApplicationTargets(ctx, org)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range list {
		if row.Ref == timerTarget.Ref {
			t.Fatal("revoked target remains active")
		}
	}
	if err := s.DeleteApplicationTarget(ctx, "bridge", bridge.Ref, org, "test-actor"); err != nil {
		t.Fatal(err)
	}
	revokedBridge, err := s.GetApplicationTarget(ctx, "bridge", bridge.Ref)
	if err != nil || revokedBridge.State != "revoked" {
		t.Fatalf("bridge revocation: %+v %v", revokedBridge, err)
	}
	// Re-enrolling the bridge with the same ref and route reactivates it; a new
	// route remains impossible even after revocation.
	reactivated, _, err := s.UpsertApplicationTarget(ctx, bridge, "test-actor")
	if err != nil || reactivated.State != "active" || reactivated.Revision != 4 {
		t.Fatalf("bridge re-enroll: %+v %v", reactivated, err)
	}
	afterDelete := bridge
	afterDelete.URL = "http://new.example/tools"
	if _, _, err := s.UpsertApplicationTarget(ctx, afterDelete, "test-actor"); !errors.Is(err, store.ErrTargetImmutableRoute) {
		t.Fatalf("re-enrolled bridge route change: %v", err)
	}
	var auditRows int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM application_target_audit WHERE ref=$1`, bridge.Ref).Scan(&auditRows); err != nil {
		t.Fatal(err)
	}
	if auditRows < 4 {
		t.Fatalf("audit rows = %d, want create/update/delete/re-enroll", auditRows)
	}
}

func TestTargetRegistryReadsStoreChangesWithoutRestart(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	t.Setenv("P0_REGISTRY_TOKEN", "service-token")
	org := "org_registry_" + randSuffix()
	if err := s.CreateOrg(ctx, org, "registry tests"); err != nil {
		t.Fatal(err)
	}
	registry := targets.New(appconfig.Config{}, s)
	bridge := store.ApplicationTarget{Kind: "bridge", Ref: "bridge-" + org, TenantID: org,
		URL: "http://app.example/tools", TokenEnv: "P0_REGISTRY_TOKEN"}
	if _, _, err := s.UpsertApplicationTarget(ctx, bridge, "test-actor"); err != nil {
		t.Fatal(err)
	}
	resolved, err := registry.ResolveBridge(ctx, bridge.Ref)
	if err != nil || resolved.URL != bridge.URL || resolved.BearerToken != "service-token" {
		t.Fatalf("registry did not observe store row without reconstruction: %+v %v", resolved, err)
	}
	if err := s.DeleteApplicationTarget(ctx, "bridge", bridge.Ref, org, "test-actor"); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ResolveBridge(ctx, bridge.Ref); !errors.Is(err, targets.ErrBridgeUnavailable) {
		t.Fatalf("revocation not observed without restart: %v", err)
	}
}
