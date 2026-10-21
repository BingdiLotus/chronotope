// Package timer defines an application-neutral, one-shot durable wake-up.
package timer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"
)

const WorkflowName = "application_timer"

// Request contains no callback address or application-specific task semantics.
type Request struct {
	TenantID        string    `json:"tenant_id"`
	SubjectRef      string    `json:"subject_ref"`
	ScopeID         string    `json:"scope_id"`
	TimerID         string    `json:"timer_id"`
	Revision        int64     `json:"revision"`
	DueAt           time.Time `json:"due_at"`
	OpaqueActionRef string    `json:"opaque_action_ref"`
}

func identifier(s string, max int) bool {
	return s != "" && s == strings.TrimSpace(s) && len(s) <= max && !strings.ContainsFunc(s, unicode.IsControl)
}

// Normalize uses PostgreSQL's timestamp precision for request replay identity.
func (r Request) Normalize() Request {
	r.DueAt = r.DueAt.UTC().Truncate(time.Microsecond)
	return r
}

func (r Request) Validate() error {
	if !identifier(r.TenantID, 256) || !identifier(r.SubjectRef, 256) || !identifier(r.ScopeID, 256) || !identifier(r.TimerID, 256) {
		return fmt.Errorf("tenant_id, subject_ref, scope_id and timer_id must be nonempty identifiers (max 256 bytes)")
	}
	if r.Revision <= 0 || r.DueAt.IsZero() || r.DueAt.Year() < 1 || r.DueAt.Year() > 9999 || !identifier(r.OpaqueActionRef, 2048) {
		return fmt.Errorf("positive revision, absolute due_at and opaque_action_ref are required")
	}
	return nil
}

func digest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (r Request) Digest() string { return digest(r.Normalize()) }

// Key is stable across retries and safe as a Restate workflow URL component.
func (r Request) Key() string {
	return "timer_" + digest([]any{r.TenantID, r.TimerID, r.Revision})
}

func (r Request) Fire() Fire {
	return Fire{TenantID: r.TenantID, TimerID: r.TimerID, Revision: r.Revision,
		FireID: "fire_" + digest([]any{r.TenantID, r.TimerID, r.Revision}), OpaqueActionRef: r.OpaqueActionRef}
}

type Fire struct {
	TenantID        string `json:"tenant_id"`
	TimerID         string `json:"timer_id"`
	Revision        int64  `json:"revision"`
	FireID          string `json:"fire_id"`
	OpaqueActionRef string `json:"opaque_action_ref"`
}

// Target is trusted deployment configuration; tokens never enter timer rows.
type Target struct {
	Ref         string `json:"ref"`
	TenantID    string `json:"tenant_id"`
	SubjectRef  string `json:"subject_ref"`
	ScopeID     string `json:"scope_id"`
	CallbackURL string `json:"callback_url"`
	BearerToken string `json:"-"`
}

func (t Target) Validate() error {
	u, err := url.Parse(t.CallbackURL)
	if !identifier(t.Ref, 256) || !identifier(t.TenantID, 256) || !identifier(t.SubjectRef, 256) || !identifier(t.ScopeID, 256) || err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("invalid configured timer target")
	}
	return nil
}

// Digest freezes routing and authority; credential rotation may keep the route.
func (t Target) Digest() string {
	return digest([]string{t.Ref, t.TenantID, t.SubjectRef, t.ScopeID, t.CallbackURL})
}

func (t Target) Matches(r Request) bool {
	return t.TenantID == r.TenantID && t.SubjectRef == r.SubjectRef && t.ScopeID == r.ScopeID
}

type Handle struct {
	TenantID string `json:"tenant_id"`
	TimerID  string `json:"timer_id"`
	Revision int64  `json:"revision"`
}

func (r Request) Handle() Handle { return Handle{r.TenantID, r.TimerID, r.Revision} }

type Record struct {
	Request
	WorkflowKey      string     `json:"timer_handle"`
	FireID           string     `json:"fire_id"`
	RequestDigest    string     `json:"request_digest"`
	TargetRef        string     `json:"target_ref"`
	TargetDigest     string     `json:"target_digest"`
	State            string     `json:"state"`
	SubmitAttempts   int        `json:"submit_attempts"`
	CallbackAttempts int        `json:"callback_attempts"`
	LastError        string     `json:"last_error,omitempty"`
	LastErrorCode    string     `json:"last_error_code,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	SubmittedAt      *time.Time `json:"submitted_at,omitempty"`
	AcknowledgedAt   *time.Time `json:"acknowledged_at,omitempty"`
}
