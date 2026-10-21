package restate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/bingdilotus/chronotope/internal/core/runs"
	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
	"github.com/bingdilotus/chronotope/internal/observability"
	restate "github.com/restatedev/sdk-go"
)

var (
	ErrApplicationResultUnknown        = errors.New("application tool result unknown")
	ErrApplicationBridgeUnavailable    = errors.New("application bridge unavailable")
	ErrApplicationBridgeTenantMismatch = errors.New("application bridge tenant mismatch")
)

type ApplicationCallContext struct {
	TenantID        string `json:"tenant_id"`
	SubjectRef      string `json:"subject_ref"`
	ScopeID         string `json:"scope_id"`
	OriginCommandID string `json:"origin_command_id"`
	RunID           string `json:"run_id"`
	OperationID     string `json:"operation_id"`
	ToolCallID      string `json:"tool_call_id"`
}

type ApplicationToolCall struct {
	Context   ApplicationCallContext `json:"context"`
	ToolRef   string                 `json:"tool_ref"`
	Arguments json.RawMessage        `json:"arguments"`
}

type ApplicationToolCaller interface {
	Call(context.Context, string, ApplicationToolCall) (json.RawMessage, error)
}

// ApplicationBridgeConfig is private worker configuration, excluded from run snapshots.
type ApplicationBridgeConfig struct {
	TenantID    string
	URL         string
	BearerToken string
}

// ApplicationBridgeSource resolves the current bridge configuration on every
// call. The dynamic registry uses this seam; a static map is the bootstrap and
// test implementation.
type ApplicationBridgeSource interface {
	ResolveBridge(context.Context, string) (ApplicationBridgeConfig, error)
}

type staticBridgeSource map[string]ApplicationBridgeConfig

func (s staticBridgeSource) ResolveBridge(_ context.Context, ref string) (ApplicationBridgeConfig, error) {
	bridge, ok := s[ref]
	if !ok {
		return ApplicationBridgeConfig{}, ErrApplicationBridgeUnavailable
	}
	return bridge, nil
}

type applicationToolClient struct {
	source ApplicationBridgeSource
	client *http.Client
}

func validateApplicationBridge(ref string, bridge ApplicationBridgeConfig) error {
	u, err := url.Parse(bridge.URL)
	if ref == "" || bridge.TenantID == "" || err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" || bridge.BearerToken == "" {
		return fmt.Errorf("invalid application bridge configuration for %q", ref)
	}
	return nil
}

func NewApplicationToolClient(bridges map[string]ApplicationBridgeConfig) (ApplicationToolCaller, error) {
	copyOf := make(staticBridgeSource, len(bridges))
	for ref, bridge := range bridges {
		if err := validateApplicationBridge(ref, bridge); err != nil {
			return nil, err
		}
		copyOf[ref] = bridge
	}
	return NewApplicationToolClientWithSource(copyOf)
}

// NewApplicationToolClientWithSource wires a dynamic bridge source. The source
// is consulted per call, so target revocation/rotation needs no worker restart.
func NewApplicationToolClientWithSource(source ApplicationBridgeSource) (ApplicationToolCaller, error) {
	if source == nil {
		return nil, fmt.Errorf("application bridge source is required")
	}
	return &applicationToolClient{source: source, client: &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (c *applicationToolClient) Call(ctx context.Context, bridgeRef string, call ApplicationToolCall) (json.RawMessage, error) {
	bridge, err := c.source.ResolveBridge(ctx, bridgeRef)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrApplicationBridgeUnavailable, err)
	}
	if err := validateApplicationBridge(bridgeRef, bridge); err != nil {
		return nil, ErrApplicationBridgeUnavailable
	}
	if call.Context.TenantID == "" || call.Context.TenantID != bridge.TenantID {
		return nil, ErrApplicationBridgeTenantMismatch
	}
	body, err := json.Marshal(call)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, bridge.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build application tool request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bridge.BearerToken)
	req.Header.Set("Idempotency-Key", call.Context.OperationID)
	response, err := c.client.Do(req)
	if err != nil {
		return nil, ErrApplicationResultUnknown
	}
	defer response.Body.Close()
	result, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(result) > 1<<20 {
		return nil, ErrApplicationResultUnknown
	}
	if response.StatusCode >= 500 || response.StatusCode < 200 || response.StatusCode >= 300 && response.StatusCode < 400 {
		return nil, ErrApplicationResultUnknown
	}
	if !json.Valid(result) {
		return nil, ErrApplicationResultUnknown
	}
	// Both success and application validation/business errors are model-visible results.
	return result, nil
}

func ValidateApplicationConfig(cfg sessionapi.AgentConfig) error { return cfg.ValidateApplication() }

func applicationToolFromConfig(cfg sessionapi.AgentConfig, name string) (sessionapi.ApplicationTool, bool) {
	for _, tool := range cfg.ApplicationTools {
		if tool.ModelAlias == name {
			return tool, true
		}
	}
	return sessionapi.ApplicationTool{}, false
}

func applicationContext(ctx context.Context, deps *Deps, in RunInput, runID string, step int, tc ToolCall) (ApplicationCallContext, error) {
	run, err := deps.Store.GetRun(ctx, runID)
	if err != nil || run == nil || run.SessionID != in.SessionID || run.Input != in.Input || run.Topic != in.Topic {
		return ApplicationCallContext{}, fmt.Errorf("application run binding missing")
	}
	session, err := deps.Store.GetSession(ctx, in.SessionID)
	if err != nil || session == nil {
		return ApplicationCallContext{}, fmt.Errorf("application session binding missing")
	}
	value := func(key string) string { s, _ := run.Bound[key].(string); return s }
	bound := ApplicationCallContext{TenantID: value("tenant_id"), SubjectRef: value("subject_ref"), ScopeID: value("scope_id"), OriginCommandID: value("origin_command_id"), RunID: runID, ToolCallID: tc.ID}
	if bound.TenantID == "" || bound.TenantID != session.OrgID || bound.SubjectRef == "" || bound.SubjectRef != session.SubjectRef || bound.ScopeID == "" || bound.ScopeID != session.ScopeID || bound.OriginCommandID == "" || tc.ID == "" {
		return ApplicationCallContext{}, fmt.Errorf("invalid application context binding")
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s", runID, step, tc.ID)))
	bound.OperationID = "app_" + hex.EncodeToString(digest[:])
	return bound, nil
}

func dispatchApplicationTool(ctx restate.Context, deps *Deps, in RunInput, runID string, step int, cfg sessionapi.AgentConfig, tc ToolCall) (string, float64, error) {
	tool, ok := applicationToolFromConfig(cfg, tc.Name)
	if !ok {
		return toolErrorResult("tool_not_allowed", "tool is not registered for this agent"), 0, nil
	}
	if tc.ArgumentsError != "" {
		return toolErrorResult("invalid_arguments", tc.ArgumentsError), 0, nil
	}
	schema, err := sessionapi.CompileApplicationSchema(tool.InputSchema)
	if err != nil {
		return "", 0, restate.ToTerminalError(err)
	}
	var arguments map[string]any
	if err := json.Unmarshal(tc.Arguments, &arguments); err != nil || arguments == nil {
		return toolErrorResult("invalid_arguments", "arguments must be a JSON object"), 0, nil
	}
	if err := schema.Validate(arguments); err != nil {
		return toolErrorResult("invalid_arguments", err.Error()), 0, nil
	}
	bound, err := applicationContext(ctx, deps, in, runID, step, tc)
	if err != nil {
		return "", 0, restate.ToTerminalError(err)
	}
	if deps.ApplicationTools == nil {
		return "", 0, restate.ToTerminalError(fmt.Errorf("application tool caller is not configured"))
	}
	type outcome struct {
		Result  string
		Unknown bool
	}
	result, err := restate.Run(ctx, func(rc restate.RunContext) (outcome, error) {
		callCtx, cancel, dErr := boundedCallContext(rc, deps, runID)
		if dErr != nil {
			return outcome{}, restate.ToTerminalError(dErr)
		}
		defer cancel()
		bridgeStarted := time.Now()
		response, callErr := deps.ApplicationTools.Call(callCtx, cfg.ApplicationBridgeRef, ApplicationToolCall{Context: bound, ToolRef: tool.ToolRef, Arguments: tc.Arguments})
		elapsed := time.Since(bridgeStarted)
		if callErr != nil {
			code, outcomeName := "application_tool_error", "error"
			if errors.Is(callErr, ErrApplicationResultUnknown) {
				code, outcomeName = "application_tool_result_unknown", "unknown"
			}
			observability.RecordToolBridge(outcomeName, code, elapsed)
			slog.Default().Warn("tool.bridge", "run_id", runID, "tenant_id", bound.TenantID,
				"operation_id", bound.OperationID, "tool_ref", tool.ToolRef,
				"outcome", outcomeName, "error_code", code, "duration_ms", elapsed.Milliseconds())
			if errors.Is(callErr, ErrApplicationResultUnknown) {
				return outcome{Result: toolErrorResult("result_unknown", "application result is unknown; do not assume failure or create a replacement operation"), Unknown: true}, nil
			}
			return outcome{}, restate.ToTerminalError(callErr)
		}
		observability.RecordToolBridge("success", "", elapsed)
		slog.Default().Info("tool.bridge", "run_id", runID, "tenant_id", bound.TenantID,
			"operation_id", bound.OperationID, "tool_ref", tool.ToolRef,
			"outcome", "success", "duration_ms", elapsed.Milliseconds(), "result_bytes", len(response))
		return outcome{Result: string(response)}, nil
	}, restate.WithName(StepName("application", step, tc.ID)))
	if err != nil {
		return "", 0, err
	}
	if result.Unknown {
		return result.Result, 0, ErrApplicationResultUnknown
	}
	return result.Result, 0, nil
}

func toolErrorResult(code, message string) string {
	b, _ := json.Marshal(map[string]any{"error": map[string]string{"code": code, "message": message}})
	return string(b)
}

func applicationToolsForModel(cfg sessionapi.AgentConfig) []runs.Tool {
	tools := make([]runs.Tool, 0, len(cfg.ApplicationTools))
	for _, tool := range cfg.ApplicationTools {
		tools = append(tools, runs.Tool{Type: "function", Name: tool.ModelAlias, Description: tool.Description, Schema: tool.InputSchema})
	}
	return tools
}
