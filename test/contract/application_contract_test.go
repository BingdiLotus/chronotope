package contract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	corecontract "github.com/bingdilotus/chronotope/internal/core/contract"
	"github.com/bingdilotus/chronotope/internal/core/timer"
	"github.com/bingdilotus/chronotope/internal/restate"
)

type contractFixture struct {
	ContractVersion string `json:"contract_version"`
	Status          string `json:"status"`
	VersionHeader   string `json:"version_header"`
	Compatibility   struct {
		Policy             string `json:"policy"`
		BreakingChange     string `json:"breaking_change"`
		AdditiveFields     string `json:"additive_fields"`
		RunProtocolVersion string `json:"run_protocol_version"`
		MinimumBotsVersion string `json:"minimum_bots_version"`
	} `json:"compatibility"`
	ErrorEnvelope struct {
		Required       []string `json:"required"`
		CategoryValues []string `json:"category_values"`
	} `json:"error_envelope"`
	Errors []corecontract.ErrorDefinition `json:"errors"`
}

func loadContractFixture(t *testing.T) contractFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "contracts", "application-contract.v1.json"))
	if err != nil {
		t.Fatalf("read application contract fixture: %v", err)
	}
	var fixture contractFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parse application contract fixture: %v", err)
	}
	return fixture
}

func TestApplicationContractVersionAndCompatibility(t *testing.T) {
	fixture := loadContractFixture(t)
	if fixture.ContractVersion != corecontract.ApplicationContractVersion {
		t.Fatalf("contract fixture version %q != code %q", fixture.ContractVersion, corecontract.ApplicationContractVersion)
	}
	if fixture.Status != "stable" {
		t.Fatalf("application contract must be stable, got %q", fixture.Status)
	}
	if fixture.VersionHeader != corecontract.VersionHeader {
		t.Fatalf("version header drift: %q vs %q", fixture.VersionHeader, corecontract.VersionHeader)
	}
	if fixture.Compatibility.Policy != "semver" || fixture.Compatibility.BreakingChange != "major" {
		t.Fatalf("compatibility policy must be semver/major: %+v", fixture.Compatibility)
	}
	if fixture.Compatibility.MinimumBotsVersion == "" {
		t.Fatal("minimum Bots version must be declared")
	}
	if fixture.Compatibility.RunProtocolVersion != "1.0" {
		t.Fatalf("run protocol version drift: %q", fixture.Compatibility.RunProtocolVersion)
	}
	wantEnvelope := map[string]bool{
		"code": true, "error_code": true, "error": true, "category": true, "retryable": true,
	}
	for _, field := range fixture.ErrorEnvelope.Required {
		delete(wantEnvelope, field)
	}
	if len(wantEnvelope) != 0 {
		t.Fatalf("error envelope missing fields: %v", wantEnvelope)
	}
	if len(fixture.ErrorEnvelope.CategoryValues) == 0 {
		t.Fatal("category enum must not be empty")
	}
}

// TestApplicationErrorCatalogMatchesFixture makes the JSON fixture and Go
// catalog a single source of truth. A code cannot be added to one side only.
func TestApplicationErrorCatalogMatchesFixture(t *testing.T) {
	fixture := loadContractFixture(t)
	catalog := corecontract.Catalog()
	if len(fixture.Errors) != len(catalog) {
		t.Fatalf("fixture has %d codes, catalog has %d", len(fixture.Errors), len(catalog))
	}
	seen := map[string]bool{}
	for _, entry := range fixture.Errors {
		if seen[entry.Code] {
			t.Fatalf("duplicate error code %q", entry.Code)
		}
		seen[entry.Code] = true
		actual, ok := catalog[entry.Code]
		if !ok {
			t.Fatalf("code %q is in fixture but not catalog", entry.Code)
		}
		if actual != entry {
			t.Fatalf("code %q drift: fixture=%+v catalog=%+v", entry.Code, entry, actual)
		}
		if entry.Category == "" || entry.Scope == "" {
			t.Fatalf("code %q must declare category and scope", entry.Code)
		}
		switch entry.Scope {
		case "http":
			if entry.HTTPStatus < 400 || entry.HTTPStatus > 599 {
				t.Fatalf("HTTP code %q has invalid status %d", entry.Code, entry.HTTPStatus)
			}
		case "run", "timer":
			if entry.HTTPStatus != 0 {
				t.Fatalf("non-HTTP code %q must not declare status %d", entry.Code, entry.HTTPStatus)
			}
		default:
			t.Fatalf("code %q has unknown scope %q", entry.Code, entry.Scope)
		}
	}
	for code := range catalog {
		if !seen[code] {
			t.Fatalf("code %q is in catalog but missing from fixture", code)
		}
	}
}

func TestHTTPErrorEnvelopeIsMachineReadable(t *testing.T) {
	body := corecontract.HTTPError(422, "application_bridge_unavailable", "bridge missing")
	if body.Code != 422 || body.ErrorCode != "application_bridge_unavailable" || body.Category != corecontract.CategoryValidation || body.Retryable {
		t.Fatalf("unexpected envelope: %+v", body)
	}
	// A code declared for a different HTTP status falls back instead of
	// producing an inconsistent status/code pair.
	fallback := corecontract.HTTPError(400, "idempotency_conflict", "bad request")
	if fallback.ErrorCode != "invalid_request" {
		t.Fatalf("status mismatch must fall back: %+v", fallback)
	}
	if got := corecontract.DefaultErrorCode(429); got != "rate_limited" {
		t.Fatalf("unexpected default code: %s", got)
	}
}

func TestProviderCodeNormalization(t *testing.T) {
	for input, want := range map[string]string{
		"rate_limit_exceeded": "provider_rate_limited",
		"429":                 "provider_rate_limited",
		"upstream timeout":    "provider_timeout",
		"invalid_request":     "provider_invalid_request",
		"overloaded":          "provider_error",
		"":                    "provider_error",
	} {
		if got := corecontract.ProviderCode(input); got != want {
			t.Fatalf("ProviderCode(%q)=%q, want %q", input, got, want)
		}
	}
}

func TestRunErrorClassificationIsStable(t *testing.T) {
	provider := corecontract.ClassifyRun("provider_timeout")
	if provider.Category != corecontract.CategoryProvider || !provider.Retryable {
		t.Fatalf("provider timeout must be retryable provider failure: %+v", provider)
	}
	tool := corecontract.ClassifyRun("application_tool_result_unknown")
	if tool.Category != corecontract.CategoryTool || tool.Retryable {
		t.Fatalf("unknown tool result must be terminal tool failure: %+v", tool)
	}
	unknown := corecontract.ClassifyRun("some_new_provider_code")
	if unknown.Code != "run_error" || unknown.Retryable {
		t.Fatalf("unknown run code must fail closed: %+v", unknown)
	}
}

func TestOpenAPIDeclaresApplicationContract(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "contracts", "session-api.openapi.yaml"))
	if err != nil {
		t.Fatalf("read OpenAPI: %v", err)
	}
	text := string(raw)
	for _, required := range []string{
		"x-application-contract-version: " + corecontract.ApplicationContractVersion,
		"X-Chronotope-Application-Contract",
		"error_code",
		"idempotency_conflict",
		"session_busy",
		"application_bridge_unavailable",
		"timer_target_unavailable",
	} {
		if !contains(text, required) {
			t.Fatalf("OpenAPI is missing contract marker %q", required)
		}
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func TestApplicationBridgeAndTimerWireFixtures(t *testing.T) {
	var call restate.ApplicationToolCall
	if err := json.Unmarshal(testdata(t, "application.bridge-call.json"), &call); err != nil {
		t.Fatalf("bridge fixture: %v", err)
	}
	if call.Context.TenantID == "" || call.Context.SubjectRef == "" || call.Context.ScopeID == "" ||
		call.Context.OriginCommandID == "" || call.Context.RunID == "" || call.Context.OperationID == "" ||
		call.Context.ToolCallID == "" || call.ToolRef == "" || len(call.Arguments) == 0 {
		t.Fatalf("bridge fixture missing required context: %+v", call)
	}
	var fire timer.Fire
	if err := json.Unmarshal(testdata(t, "application.timer-fire.json"), &fire); err != nil {
		t.Fatalf("timer fixture: %v", err)
	}
	if fire.TenantID == "" || fire.TimerID == "" || fire.Revision <= 0 || fire.FireID == "" || fire.OpaqueActionRef == "" {
		t.Fatalf("timer fixture missing identity: %+v", fire)
	}
	req := timer.Request{TenantID: fire.TenantID, TimerID: fire.TimerID, Revision: fire.Revision, OpaqueActionRef: fire.OpaqueActionRef}
	if req.Fire().FireID != fire.FireID {
		t.Fatalf("fire fixture identity drift: %s vs %s", req.Fire().FireID, fire.FireID)
	}
}
