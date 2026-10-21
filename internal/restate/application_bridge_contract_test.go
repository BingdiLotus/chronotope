package restate

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/bingdilotus/chronotope/test/contract/fakeapp"
)

type mutableBridgeSource struct {
	mu     sync.Mutex
	bridge ApplicationBridgeConfig
}

func (m *mutableBridgeSource) ResolveBridge(_ context.Context, _ string) (ApplicationBridgeConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bridge, nil
}

func (m *mutableBridgeSource) set(bridge ApplicationBridgeConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bridge = bridge
}

func TestApplicationBridgeContractShapeAndIdempotency(t *testing.T) {
	receiver := fakeapp.New("service-token")
	server := receiver.Server()
	defer server.Close()
	client, err := NewApplicationToolClient(map[string]ApplicationBridgeConfig{
		"bots": {TenantID: "tenant-a", URL: server.URL + "/tools", BearerToken: "service-token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	call := ApplicationToolCall{
		Context: ApplicationCallContext{
			TenantID: "tenant-a", SubjectRef: "bot-1", ScopeID: "channel-1",
			OriginCommandID: "command-1", RunID: "run-1", OperationID: "app_op_1", ToolCallID: "call-1",
		},
		ToolRef:   "knowledge.search",
		Arguments: json.RawMessage(`{"query":"contract"}`),
	}
	response, err := client.Call(context.Background(), "bots", call)
	if err != nil {
		t.Fatal(err)
	}
	if string(response) == "" || !json.Valid(response) {
		t.Fatalf("tool response must be JSON for model-visible result: %s", response)
	}
	calls := receiver.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("bridge call count: %d", len(calls))
	}
	got := calls[0]
	if got.Authorization != "Bearer service-token" || got.IdempotencyKey != "app_op_1" {
		t.Fatalf("service auth/idempotency drift: %+v", got)
	}
	if got.ToolRef != "knowledge.search" || string(got.Arguments) != `{"query":"contract"}` {
		t.Fatalf("tool call payload drift: %+v", got)
	}
	if got.Context["run_id"] != "run-1" || got.Context["tenant_id"] != "tenant-a" || got.Context["tool_call_id"] != "call-1" {
		t.Fatalf("trusted context drift: %+v", got.Context)
	}

	// A change to the dynamic source is visible on the next call. Existing runs
	// keep their immutable bridge_ref, while enrollment is no longer static-only.
	other := fakeapp.New("rotated-token")
	otherServer := other.Server()
	defer otherServer.Close()
	source := &mutableBridgeSource{bridge: ApplicationBridgeConfig{TenantID: "tenant-a", URL: server.URL + "/tools", BearerToken: "service-token"}}
	dynamic, err := NewApplicationToolClientWithSource(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dynamic.Call(context.Background(), "bots", call); err != nil {
		t.Fatal(err)
	}
	source.set(ApplicationBridgeConfig{TenantID: "tenant-a", URL: otherServer.URL + "/tools", BearerToken: "rotated-token"})
	if _, err := dynamic.Call(context.Background(), "bots", call); err != nil {
		t.Fatal(err)
	}
	if len(other.ToolCalls()) != 1 || other.ToolCalls()[0].Authorization != "Bearer rotated-token" {
		t.Fatalf("dynamic bridge rotation not applied: %+v", other.ToolCalls())
	}
}

func TestApplicationBridgeTenantMismatchNeverSendsCall(t *testing.T) {
	receiver := fakeapp.New("service-token")
	server := receiver.Server()
	defer server.Close()
	client, err := NewApplicationToolClient(map[string]ApplicationBridgeConfig{
		"bots": {TenantID: "tenant-a", URL: server.URL + "/tools", BearerToken: "service-token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Call(context.Background(), "bots", ApplicationToolCall{Context: ApplicationCallContext{TenantID: "tenant-b"}})
	if err == nil {
		t.Fatal("tenant mismatch must fail closed")
	}
	if len(receiver.ToolCalls()) != 0 {
		t.Fatal("tenant mismatch reached the application receiver")
	}
}
