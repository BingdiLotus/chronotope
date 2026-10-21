// Package contract defines the machine-readable application contract shared by
// the HTTP API, the worker and contract fixtures. It intentionally contains no
// transport code so the same catalog can be validated from tests and docs.
package contract

import "strings"

// ApplicationContractVersion is the SemVer version of the Bots-facing
// application contract (agent/session/run, application bridge, timer and error
// envelope). Additive fields and new error codes are minor/patch changes; a
// breaking shape change requires a major version and a documented Bots upgrade.
const ApplicationContractVersion = "1.0.0"

// VersionHeader is returned on every HTTP response from the application API.
const VersionHeader = "X-Chronotope-Application-Contract"

// Category is a stable, coarse failure class for machine branching. It is not
// free text and never contains provider/model payloads.
type Category string

const (
	CategoryValidation Category = "validation"
	CategoryAuth       Category = "auth"
	CategoryNotFound   Category = "not_found"
	CategoryConflict   Category = "conflict"
	CategoryRateLimit  Category = "rate_limit"
	CategoryProvider   Category = "provider"
	CategoryTool       Category = "tool"
	CategoryTimer      Category = "timer"
	CategoryRuntime    Category = "runtime"
	CategoryLimit      Category = "limit"
	CategoryPolicy     Category = "policy"
	CategoryCanceled   Category = "canceled"
)

// ErrorDefinition is one stable error code. HTTPStatus is zero for run/timer
// codes that are not returned as an HTTP error response.
type ErrorDefinition struct {
	Code       string   `json:"code"`
	HTTPStatus int      `json:"http_status"`
	Category   Category `json:"category"`
	Retryable  bool     `json:"retryable"`
	Scope      string   `json:"scope"` // http | run | timer
}

// ErrorBody is the HTTP error envelope. code/error are retained verbatim from
// the pre-v1 contract; error_code/category/retryable are the stable additions.
type ErrorBody struct {
	Code      int      `json:"code"`
	ErrorCode string   `json:"error_code"`
	Error     string   `json:"error"`
	Category  Category `json:"category"`
	Retryable bool     `json:"retryable"`
}

var catalog = map[string]ErrorDefinition{
	// HTTP request/admission errors.
	"invalid_request":                {Code: "invalid_request", HTTPStatus: 400, Category: CategoryValidation, Scope: "http"},
	"unauthorized":                   {Code: "unauthorized", HTTPStatus: 401, Category: CategoryAuth, Scope: "http"},
	"forbidden":                      {Code: "forbidden", HTTPStatus: 403, Category: CategoryAuth, Scope: "http"},
	"tenant_mismatch":                {Code: "tenant_mismatch", HTTPStatus: 403, Category: CategoryAuth, Scope: "http"},
	"admin_required":                 {Code: "admin_required", HTTPStatus: 403, Category: CategoryAuth, Scope: "http"},
	"not_found":                      {Code: "not_found", HTTPStatus: 404, Category: CategoryNotFound, Scope: "http"},
	"conflict":                       {Code: "conflict", HTTPStatus: 409, Category: CategoryConflict, Scope: "http"},
	"idempotency_conflict":           {Code: "idempotency_conflict", HTTPStatus: 409, Category: CategoryConflict, Scope: "http"},
	"session_busy":                   {Code: "session_busy", HTTPStatus: 409, Category: CategoryConflict, Retryable: true, Scope: "http"},
	"target_route_immutable":         {Code: "target_route_immutable", HTTPStatus: 409, Category: CategoryConflict, Scope: "http"},
	"target_conflict":                {Code: "target_conflict", HTTPStatus: 409, Category: CategoryConflict, Scope: "http"},
	"target_ref_static":              {Code: "target_ref_static", HTTPStatus: 409, Category: CategoryConflict, Scope: "http"},
	"validation_failed":              {Code: "validation_failed", HTTPStatus: 422, Category: CategoryValidation, Scope: "http"},
	"application_bridge_unavailable": {Code: "application_bridge_unavailable", HTTPStatus: 422, Category: CategoryValidation, Scope: "http"},
	"rate_limited":                   {Code: "rate_limited", HTTPStatus: 429, Category: CategoryRateLimit, Retryable: true, Scope: "http"},
	"internal_error":                 {Code: "internal_error", HTTPStatus: 500, Category: CategoryRuntime, Retryable: true, Scope: "http"},
	"not_implemented":                {Code: "not_implemented", HTTPStatus: 501, Category: CategoryValidation, Scope: "http"},
	"upstream_error":                 {Code: "upstream_error", HTTPStatus: 502, Category: CategoryProvider, Retryable: true, Scope: "http"},
	"service_unavailable":            {Code: "service_unavailable", HTTPStatus: 503, Category: CategoryRuntime, Retryable: true, Scope: "http"},
	"initialization_pending":         {Code: "initialization_pending", HTTPStatus: 503, Category: CategoryRuntime, Retryable: true, Scope: "http"},
	"upstream_timeout":               {Code: "upstream_timeout", HTTPStatus: 504, Category: CategoryProvider, Retryable: true, Scope: "http"},
	"timer_target_unavailable":       {Code: "timer_target_unavailable", HTTPStatus: 403, Category: CategoryTimer, Scope: "http"},

	// Application run terminal errors. Retryable describes whether Bots may
	// safely re-submit the same logical operation (with stable idempotency
	// keys); it is not a promise that a run can be resumed.
	"invalid_application_config":       {Code: "invalid_application_config", Category: CategoryValidation, Scope: "run"},
	"invalid_application_context":      {Code: "invalid_application_context", Category: CategoryValidation, Scope: "run"},
	"invalid_run_deadline":             {Code: "invalid_run_deadline", Category: CategoryValidation, Scope: "run"},
	"event_error":                      {Code: "event_error", Category: CategoryRuntime, Scope: "run"},
	"store_error":                      {Code: "store_error", Category: CategoryRuntime, Scope: "run"},
	"history_error":                    {Code: "history_error", Category: CategoryRuntime, Scope: "run"},
	"session_error":                    {Code: "session_error", Category: CategoryRuntime, Scope: "run"},
	"credential_error":                 {Code: "credential_error", Category: CategoryValidation, Scope: "run"},
	"deadline_error":                   {Code: "deadline_error", Category: CategoryValidation, Scope: "run"},
	"deadline_exceeded":                {Code: "deadline_exceeded", Category: CategoryLimit, Scope: "run"},
	"session_paused":                   {Code: "session_paused", Category: CategoryConflict, Scope: "run"},
	"harness_error":                    {Code: "harness_error", Category: CategoryProvider, Retryable: true, Scope: "run"},
	"harness_incomplete":               {Code: "harness_incomplete", Category: CategoryProvider, Retryable: true, Scope: "run"},
	"provider_error":                   {Code: "provider_error", Category: CategoryProvider, Retryable: true, Scope: "run"},
	"provider_timeout":                 {Code: "provider_timeout", Category: CategoryProvider, Retryable: true, Scope: "run"},
	"provider_rate_limited":            {Code: "provider_rate_limited", Category: CategoryProvider, Retryable: true, Scope: "run"},
	"provider_invalid_request":         {Code: "provider_invalid_request", Category: CategoryProvider, Scope: "run"},
	"model_output_truncated":           {Code: "model_output_truncated", Category: CategoryProvider, Scope: "run"},
	"application_tool_error":           {Code: "application_tool_error", Category: CategoryTool, Scope: "run"},
	"application_tool_result_unknown":  {Code: "application_tool_result_unknown", Category: CategoryTool, Scope: "run"},
	"output_schema_violation":          {Code: "output_schema_violation", Category: CategoryValidation, Scope: "run"},
	"no_progress":                      {Code: "no_progress", Category: CategoryLimit, Scope: "run"},
	"max_steps":                        {Code: "max_steps", Category: CategoryLimit, Scope: "run"},
	"budget_exceeded":                  {Code: "budget_exceeded", Category: CategoryLimit, Scope: "run"},
	"tool_denied":                      {Code: "tool_denied", Category: CategoryPolicy, Scope: "run"},
	"approver_revoked":                 {Code: "approver_revoked", Category: CategoryPolicy, Scope: "run"},
	"canceled":                         {Code: "canceled", Category: CategoryCanceled, Scope: "run"},
	"run_error":                        {Code: "run_error", Category: CategoryRuntime, Scope: "run"},
	"application_tool_bridge_required": {Code: "application_tool_bridge_required", Category: CategoryTool, Scope: "run"},

	// Timer delivery/status codes stored in application_timers.last_error_code.
	"timer_target_changed":      {Code: "timer_target_changed", Category: CategoryTimer, Scope: "timer"},
	"timer_callback_unknown":    {Code: "timer_callback_unknown", Category: CategoryTimer, Retryable: true, Scope: "timer"},
	"timer_callback_rejected":   {Code: "timer_callback_rejected", Category: CategoryTimer, Retryable: true, Scope: "timer"},
	"timer_ack_persist_failed":  {Code: "timer_ack_persist_failed", Category: CategoryTimer, Retryable: true, Scope: "timer"},
	"timer_submit_failed":       {Code: "timer_submit_failed", Category: CategoryTimer, Retryable: true, Scope: "timer"},
	"timer_configuration_error": {Code: "timer_configuration_error", Category: CategoryTimer, Scope: "timer"},
}

// Catalog returns a copy of the complete stable code catalog.
func Catalog() map[string]ErrorDefinition {
	out := make(map[string]ErrorDefinition, len(catalog))
	for code, def := range catalog {
		out[code] = def
	}
	return out
}

// Lookup returns the definition for a code.
func Lookup(code string) (ErrorDefinition, bool) {
	def, ok := catalog[code]
	return def, ok
}

// DefaultErrorCode maps HTTP status to the generic stable code used when a
// handler does not provide a more specific one.
func DefaultErrorCode(status int) string {
	switch status {
	case 400:
		return "invalid_request"
	case 401:
		return "unauthorized"
	case 403:
		return "forbidden"
	case 404:
		return "not_found"
	case 409:
		return "conflict"
	case 422:
		return "validation_failed"
	case 429:
		return "rate_limited"
	case 500:
		return "internal_error"
	case 501:
		return "not_implemented"
	case 502:
		return "upstream_error"
	case 503:
		return "service_unavailable"
	case 504:
		return "upstream_timeout"
	default:
		return "internal_error"
	}
}

// HTTPError builds a stable HTTP error envelope. An empty or unknown errorCode
// falls back to the status mapping so older handlers still return a
// machine-readable code.
func HTTPError(status int, errorCode, message string) ErrorBody {
	if def, ok := catalog[errorCode]; !ok || def.HTTPStatus != status {
		errorCode = DefaultErrorCode(status)
	}
	def, ok := catalog[errorCode]
	if !ok {
		def = ErrorDefinition{Code: "internal_error", Category: CategoryRuntime, Retryable: true}
	}
	return ErrorBody{Code: status, ErrorCode: def.Code, Error: message, Category: def.Category, Retryable: def.Retryable}
}

// ClassifyRun returns the stable classification for a run terminal code.
func ClassifyRun(code string) ErrorDefinition {
	if def, ok := catalog[code]; ok {
		return def
	}
	return catalog["run_error"]
}

// ProviderCode normalizes a provider/harness error code into the stable run
// error set. Unknown provider codes become provider_error.
func ProviderCode(code string) string {
	lower := strings.ToLower(strings.TrimSpace(code))
	switch {
	case lower == "":
		return "provider_error"
	case strings.Contains(lower, "rate") || strings.Contains(lower, "429") || strings.Contains(lower, "quota"):
		return "provider_rate_limited"
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline") || strings.Contains(lower, "timed_out"):
		return "provider_timeout"
	case strings.Contains(lower, "invalid_request") || strings.Contains(lower, "bad_request") || strings.Contains(lower, "400") || strings.Contains(lower, "401") || strings.Contains(lower, "403"):
		return "provider_invalid_request"
	default:
		return "provider_error"
	}
}

// IsHTTPErrorStatus reports whether a status is part of the HTTP error envelope
// family (used by contract tests and docs).
func IsHTTPErrorStatus(status int) bool { return status >= 400 && status <= 599 }
