// Package fakeapp is a Bots-like application receiver used by contract tests.
// It implements the two HTTP callbacks Chronotope sends: application tool calls
// and timer fire delivery. It is deliberately independent of Chronotope's
// production packages so a contract test can assert the wire shape rather than
// reuse the sender's own types.
package fakeapp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"

	"github.com/bingdilotus/chronotope/internal/core/timer"
)

// ToolCall is one received application bridge request.
type ToolCall struct {
	Authorization  string            `json:"authorization"`
	IdempotencyKey string            `json:"idempotency_key"`
	Context        map[string]string `json:"context"`
	ToolRef        string            `json:"tool_ref"`
	Arguments      json.RawMessage   `json:"arguments"`
}

// Fire is one received timer callback.
type Fire struct {
	Authorization  string     `json:"authorization"`
	IdempotencyKey string     `json:"idempotency_key"`
	Payload        timer.Fire `json:"payload"`
	Raw            []byte     `json:"-"`
}

// Receiver records callback requests and returns application-shaped responses.
// ServiceToken is the only credential it accepts.
type Receiver struct {
	ServiceToken string

	mu        sync.Mutex
	toolCalls []ToolCall
	fires     []Fire

	// FailTools makes the bridge return 503 (an unknown application outcome).
	FailTools bool
	// RejectFire makes the timer receiver return 409. The worker must treat it
	// as a retryable, non-advancing outcome.
	RejectFire bool
}

// New returns an empty receiver accepting serviceToken.
func New(serviceToken string) *Receiver { return &Receiver{ServiceToken: serviceToken} }

// Handler returns the HTTP callback surface.
func (r *Receiver) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	mux.HandleFunc("POST /tools", r.handleTool)
	mux.HandleFunc("POST /timer-fires", r.handleFire)
	return mux
}

// Server starts a test server and registers cleanup.
func (r *Receiver) Server() *httptest.Server {
	server := httptest.NewServer(r.Handler())
	return server
}

// ToolCalls returns a snapshot of accepted tool calls.
func (r *Receiver) ToolCalls() []ToolCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ToolCall(nil), r.toolCalls...)
}

// Fires returns a snapshot of accepted timer fires.
func (r *Receiver) Fires() []Fire {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Fire(nil), r.fires...)
}

func (r *Receiver) handleTool(w http.ResponseWriter, req *http.Request) {
	if !r.authorized(req) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid service token"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body"})
		return
	}
	var call ToolCall
	if err := json.Unmarshal(body, &call); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}
	call.Authorization = req.Header.Get("Authorization")
	call.IdempotencyKey = req.Header.Get("Idempotency-Key")
	if call.IdempotencyKey == "" || call.IdempotencyKey != call.Context["operation_id"] {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "operation identity header mismatch"})
		return
	}
	for _, field := range []string{"tenant_id", "subject_ref", "scope_id", "origin_command_id", "run_id", "operation_id", "tool_call_id"} {
		if call.Context[field] == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing trusted context: " + field})
			return
		}
	}
	if call.ToolRef == "" || len(call.Arguments) == 0 || !json.Valid(call.Arguments) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid tool_ref/arguments"})
		return
	}
	if r.FailTools {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "application tool result unknown"})
		return
	}
	r.mu.Lock()
	r.toolCalls = append(r.toolCalls, call)
	r.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"tool_ref":     call.ToolRef,
		"operation_id": call.Context["operation_id"],
	})
}

func (r *Receiver) handleFire(w http.ResponseWriter, req *http.Request) {
	if !r.authorized(req) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid service token"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body"})
		return
	}
	var fire timer.Fire
	if err := json.Unmarshal(body, &fire); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}
	idempotencyKey := req.Header.Get("Idempotency-Key")
	if idempotencyKey == "" || idempotencyKey != fire.FireID {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "fire identity header mismatch"})
		return
	}
	if fire.TenantID == "" || fire.TimerID == "" || fire.Revision <= 0 || fire.OpaqueActionRef == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing fire identity"})
		return
	}
	if r.RejectFire {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "application rejected fire"})
		return
	}
	r.mu.Lock()
	r.fires = append(r.fires, Fire{Authorization: req.Header.Get("Authorization"), IdempotencyKey: idempotencyKey, Payload: fire, Raw: body})
	r.mu.Unlock()
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true, "fire_id": fire.FireID})
}

func (r *Receiver) authorized(req *http.Request) bool {
	return r.ServiceToken != "" && req.Header.Get("Authorization") == "Bearer "+r.ServiceToken
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
