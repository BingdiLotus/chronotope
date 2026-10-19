package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
	"github.com/bingdilotus/chronotope/internal/store"
)

func TestApplicationCreationAndConcurrentAdmission(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ns := "admission_" + randSuffix()
	if err := s.CreateOrg(ctx, ns, "admission"); err != nil {
		t.Fatal(err)
	}
	cfg := sessionapi.AgentConfig{Model: "test"}
	for i := range 2 {
		fresh, err := s.CreateAgentRequest(ctx, ns+"a", ns, "agent", &cfg, "agent-hash")
		if err != nil || fresh != (i == 0) {
			t.Fatalf("agent replay: fresh=%v err=%v", fresh, err)
		}
	}
	if _, err := s.CreateAgentRequest(ctx, ns+"a", ns, "changed", &cfg, "different"); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("agent mismatch: %v", err)
	}
	for i := range 2 {
		fresh, err := s.CreateSessionRequest(ctx, ns+"s", ns, ns+"a", "bot", "channel", "session-hash")
		if err != nil || fresh != (i == 0) {
			t.Fatalf("session replay: fresh=%v err=%v", fresh, err)
		}
	}
	if _, err := s.CreateSessionRequest(ctx, ns+"s", ns, ns+"a", "other", "channel", "different"); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("session mismatch: %v", err)
	}
	sess, err := s.GetSession(ctx, ns+"s")
	if err != nil || sess.SubjectRef != "bot" || sess.ScopeID != "channel" {
		t.Fatalf("session binding: %+v %v", sess, err)
	}
	results := make(chan error, 8)
	fresh := make(chan bool, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			created, err := s.AdmitRun(ctx, ns+"r", ns+"s", "input", "topic", nil, map[string]any{"tenant_id": ns}, "run-hash")
			results <- err
			fresh <- created
		})
	}
	wg.Wait()
	close(results)
	close(fresh)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	count := 0
	for created := range fresh {
		if created {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("created %d duplicate runs", count)
	}
	if _, err := s.AdmitRun(ctx, ns+"r", ns+"s", "input", "changed", nil, nil, "different"); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("same run mismatch: %v", err)
	}
	if _, err := s.AdmitRun(ctx, ns+"other", ns+"s", "input", "", nil, nil, "other"); !errors.Is(err, store.ErrSessionBusy) {
		t.Fatalf("new run while busy: %v", err)
	}
	var outbox int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM admission_outbox WHERE run_id=$1`, ns+"r").Scan(&outbox); err != nil || outbox != 1 {
		t.Fatalf("outbox=%d err=%v", outbox, err)
	}
	payload := json.RawMessage(`{"final":"nonce-result","error":null,"usage":{"tokens_total":3},"steps":2}`)
	for range 2 {
		if err := s.FinalizeRun(ctx, ns+"r", ns+"s", sessionapi.RunCompleted, "result", payload); err != nil {
			t.Fatalf("terminal replay: %v", err)
		}
	}
	if err := s.FinalizeRun(ctx, ns+"r", ns+"s", sessionapi.RunCompleted, "result", json.RawMessage(`{"final":"wrong"}`)); !errors.Is(err, store.ErrFinalizeConflict) {
		t.Fatalf("terminal mismatch: %v", err)
	}
	run, err := s.GetRun(ctx, ns+"r")
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(run.Result, &result); err != nil || run.Status != sessionapi.RunCompleted || result["final"] != "nonce-result" {
		t.Fatalf("terminal/result split: %+v %s %v", run, run.Result, err)
	}
	if _, err := s.AdmitRun(ctx, ns+"next", ns+"s", "next", "", nil, nil, "next"); err != nil {
		t.Fatalf("terminal did not release slot: %v", err)
	}
}

func TestRequestHashCanonicalJSON(t *testing.T) {
	a, err := store.RequestHash(json.RawMessage(`{"z":{"b":2,"a":1},"a":[3]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.RequestHash(json.RawMessage(`{ "a":[3],"z":{"a":1,"b":2}}`))
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("equivalent JSON hashes differ: %s != %s", a, b)
	}
	c, _ := store.RequestHash(json.RawMessage(`{"z":{"b":3,"a":1},"a":[3]}`))
	if c == a {
		t.Fatal("different requests have same hash")
	}
}
