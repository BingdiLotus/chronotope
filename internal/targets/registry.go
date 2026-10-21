// Package targets resolves application bridge and timer receivers from the
// static deployment bootstrap plus the runtime-managed target registry.
//
// Resolution reads the registry on every use. That is deliberate for the first
// cut: a deleted/updated target is visible without restarting API or worker, and
// timer fencing compares the frozen target digest before any callback is sent.
package targets

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/bingdilotus/chronotope/internal/appconfig"
	"github.com/bingdilotus/chronotope/internal/core/timer"
	"github.com/bingdilotus/chronotope/internal/store"
)

var (
	ErrBridgeUnavailable      = errors.New("application bridge is unavailable")
	ErrTimerTargetUnavailable = errors.New("timer target is unavailable")
	ErrAmbiguousTarget        = errors.New("application target is ambiguous")
	ErrStaticTargetConflict   = errors.New("dynamic target conflicts with static bootstrap")
)

// Bridge is a resolved, trusted receiver configuration. BearerToken is never
// included in API responses or logs.
type Bridge struct {
	Ref         string
	TenantID    string
	URL         string
	BearerToken string
	Digest      string
}

// ResolvedTarget freezes the routing digest used by a timer row. Err marks a
// matching active dynamic row that cannot be used (missing credential, invalid
// route/digest); callers fail closed instead of falling through to another
// same-scope static receiver.
type ResolvedTarget struct {
	Target timer.Target
	Digest string
	Err    error
}

// Source is the dynamic registry read side.
type Source interface {
	GetApplicationTarget(ctx context.Context, kind, ref string) (*store.ApplicationTarget, error)
	ListApplicationTargets(ctx context.Context, tenantID string) ([]store.ApplicationTarget, error)
}

// Registry merges static env bootstrap with dynamic rows.
type Registry struct {
	source        Source
	staticBridges map[string]appconfig.Bridge
	staticTimers  []timer.Target
	lookupEnv     func(string) (string, bool)
}

// New builds a registry. A nil source means static-only (tests/backcompat).
func New(cfg appconfig.Config, source Source) *Registry {
	bridges := make(map[string]appconfig.Bridge, len(cfg.Bridges))
	for _, bridge := range cfg.Bridges {
		bridges[bridge.Ref] = bridge
	}
	return &Registry{
		source:        source,
		staticBridges: bridges,
		staticTimers:  append([]timer.Target(nil), cfg.TimerTargets...),
		lookupEnv:     os.LookupEnv,
	}
}

// StaticRef reports whether a ref is owned by deployment env. Dynamic
// registration must not shadow it.
func (r *Registry) StaticRef(kind, ref string) bool {
	if r == nil {
		return false
	}
	switch kind {
	case "bridge":
		_, ok := r.staticBridges[ref]
		return ok
	case "timer":
		for _, target := range r.staticTimers {
			if target.Ref == ref {
				return true
			}
		}
	}
	return false
}

// StaticTimerScope reports whether deployment env already owns this timer
// scope. Dynamic enrollment for the same scope would be permanently ambiguous.
func (r *Registry) StaticTimerScope(tenantID, subjectRef, scopeID string) bool {
	if r == nil {
		return false
	}
	for _, target := range r.staticTimers {
		if target.TenantID == tenantID && target.SubjectRef == subjectRef && target.ScopeID == scopeID {
			return true
		}
	}
	return false
}

func (r *Registry) dynamic(ctx context.Context, kind, ref string) (*store.ApplicationTarget, error) {
	if r == nil || r.source == nil {
		return nil, store.ErrNotFound
	}
	return r.source.GetApplicationTarget(ctx, kind, ref)
}

// ResolveBridge resolves one bridge ref. Static and dynamic ownership of the
// same ref fails closed instead of silently preferring one address.
func (r *Registry) ResolveBridge(ctx context.Context, ref string) (Bridge, error) {
	static, hasStatic := r.staticBridges[ref]
	row, err := r.dynamic(ctx, "bridge", ref)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return Bridge{}, fmt.Errorf("resolve application bridge: %w", err)
	}
	if hasStatic && row != nil {
		return Bridge{}, ErrStaticTargetConflict
	}
	if hasStatic {
		if static.Ref == "" || static.TenantID == "" || static.URL == "" || static.BearerToken == "" {
			return Bridge{}, ErrBridgeUnavailable
		}
		return Bridge{Ref: static.Ref, TenantID: static.TenantID, URL: static.URL, BearerToken: static.BearerToken,
			Digest: store.ApplicationTargetDigest(store.ApplicationTarget{Kind: "bridge", Ref: static.Ref, TenantID: static.TenantID, URL: static.URL})}, nil
	}
	if row == nil {
		return Bridge{}, ErrBridgeUnavailable
	}
	if row.Kind != "bridge" || row.Ref != ref || row.State != "active" || row.TenantID == "" || row.URL == "" {
		return Bridge{}, ErrBridgeUnavailable
	}
	if row.Digest != store.ApplicationTargetDigest(*row) || !validHTTPURL(row.URL) {
		return Bridge{}, ErrBridgeUnavailable
	}
	credential, ok := r.lookupEnv(row.TokenEnv)
	if !ok || strings.TrimSpace(credential) == "" {
		return Bridge{}, ErrBridgeUnavailable
	}
	return Bridge{Ref: row.Ref, TenantID: row.TenantID, URL: row.URL, BearerToken: credential, Digest: row.Digest}, nil
}

// ResolveTimerTarget selects the unique timer receiver for an admission. Static
// and dynamic candidates are merged; duplicate refs/scope fail closed.
func (r *Registry) ResolveTimerTarget(ctx context.Context, req timer.Request) (*timer.Target, string, error) {
	candidates, err := r.timerCandidates(ctx, req.TenantID)
	if err != nil {
		return nil, "", err
	}
	var matches []ResolvedTarget
	seenRefs := map[string]bool{}
	for _, candidate := range candidates {
		if !candidate.Target.Matches(req) {
			continue
		}
		if candidate.Err != nil {
			return nil, "", candidate.Err
		}
		if seenRefs[candidate.Target.Ref] {
			return nil, "", ErrAmbiguousTarget
		}
		seenRefs[candidate.Target.Ref] = true
		matches = append(matches, candidate)
	}
	if len(matches) == 0 {
		return nil, "", ErrTimerTargetUnavailable
	}
	if len(matches) > 1 {
		return nil, "", ErrAmbiguousTarget
	}
	return &matches[0].Target, matches[0].Digest, nil
}

// ResolveTimerTargetByRef resolves the current target for a frozen timer row.
// The caller must still compare the returned digest with the row's digest.
func (r *Registry) ResolveTimerTargetByRef(ctx context.Context, tenantID, ref string) (*timer.Target, string, error) {
	candidates, err := r.timerCandidates(ctx, tenantID)
	if err != nil {
		return nil, "", err
	}
	var found *ResolvedTarget
	for i := range candidates {
		if candidates[i].Target.Ref != ref {
			continue
		}
		if found != nil {
			return nil, "", ErrAmbiguousTarget
		}
		candidate := candidates[i]
		found = &candidate
	}
	if found == nil {
		return nil, "", ErrTimerTargetUnavailable
	}
	if found.Err != nil {
		return nil, "", found.Err
	}
	return &found.Target, found.Digest, nil
}

func (r *Registry) timerCandidates(ctx context.Context, tenantID string) ([]ResolvedTarget, error) {
	var out []ResolvedTarget
	for _, target := range r.staticTimers {
		if tenantID != "" && target.TenantID != tenantID {
			continue
		}
		out = append(out, ResolvedTarget{Target: target, Digest: target.Digest()})
	}
	if r == nil || r.source == nil || tenantID == "" {
		return out, nil
	}
	rows, err := r.source.ListApplicationTargets(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("resolve timer targets: %w", err)
	}
	seenRefs := map[string]bool{}
	for _, row := range rows {
		if row.Kind != "timer" || row.State != "active" {
			continue
		}
		target := timer.Target{Ref: row.Ref, TenantID: row.TenantID, SubjectRef: row.SubjectRef,
			ScopeID: row.ScopeID, CallbackURL: row.URL}
		if seenRefs[target.Ref] {
			return nil, ErrAmbiguousTarget
		}
		seenRefs[target.Ref] = true
		candidate := ResolvedTarget{Target: target, Digest: row.Digest}
		if row.Digest != store.ApplicationTargetDigest(row) || target.Validate() != nil {
			candidate.Err = ErrTimerTargetUnavailable
			out = append(out, candidate)
			continue
		}
		credential, ok := r.lookupEnv(row.TokenEnv)
		if !ok || strings.TrimSpace(credential) == "" {
			candidate.Err = ErrTimerTargetUnavailable
			out = append(out, candidate)
			continue
		}
		target.BearerToken = credential
		candidate.Target = target
		out = append(out, candidate)
	}
	return out, nil
}

// ValidBridgeURL validates an operator-supplied bridge address. Credentials and
// userinfo are never accepted in the URL.
func ValidBridgeURL(raw string) bool { return validHTTPURL(raw) }

func validHTTPURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Host != "" && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.User == nil && parsed.Fragment == ""
}
