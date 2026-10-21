package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bingdilotus/chronotope/internal/core/timer"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	// ErrTargetConflict is returned when a target ref or tenant/scope is
	// already owned by another target.
	ErrTargetConflict = errors.New("application target conflicts with an existing target")
	// ErrTargetImmutableRoute is returned when a bridge update attempts to
	// change its tenant/address. Enroll a new ref instead so old agents cannot
	// silently follow a new receiver.
	ErrTargetImmutableRoute = errors.New("application bridge route is immutable; enroll a new ref")
)

// ApplicationTarget is a dynamically registered bridge or timer target. TokenEnv
// is a reference name; token values are resolved from the API/worker environment
// at use time and never stored here.
type ApplicationTarget struct {
	Kind       string    `json:"kind"`
	Ref        string    `json:"ref"`
	TenantID   string    `json:"tenant_id"`
	URL        string    `json:"url"`
	SubjectRef string    `json:"subject_ref,omitempty"`
	ScopeID    string    `json:"scope_id,omitempty"`
	TokenEnv   string    `json:"token_env"`
	Revision   int64     `json:"revision"`
	State      string    `json:"state"`
	Digest     string    `json:"digest"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// ApplicationTargetDigest covers routing/authority fields only. Credential
// rotation via token_env must not change timer fencing identity.
func ApplicationTargetDigest(target ApplicationTarget) string {
	if target.Kind == "timer" {
		return (timer.Target{
			Ref: target.Ref, TenantID: target.TenantID,
			SubjectRef: target.SubjectRef, ScopeID: target.ScopeID,
			CallbackURL: target.URL,
		}).Digest()
	}
	raw, _ := json.Marshal([]string{target.Kind, target.Ref, target.TenantID, target.URL})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func validateApplicationTarget(target ApplicationTarget) error {
	if target.Kind != "bridge" && target.Kind != "timer" {
		return fmt.Errorf("application target kind must be bridge or timer")
	}
	if strings.TrimSpace(target.Ref) == "" || target.Ref != strings.TrimSpace(target.Ref) || len(target.Ref) > 256 || strings.ContainsAny(target.Ref, "/?#") {
		return fmt.Errorf("application target ref is required (max 256 bytes; no /?#)")
	}
	if strings.TrimSpace(target.TenantID) == "" || len(target.TenantID) > 256 {
		return fmt.Errorf("application target tenant_id is required")
	}
	if strings.TrimSpace(target.URL) == "" {
		return fmt.Errorf("application target url is required")
	}
	if strings.TrimSpace(target.TokenEnv) == "" {
		return fmt.Errorf("application target token_env reference is required")
	}
	if target.Kind == "bridge" && (target.SubjectRef != "" || target.ScopeID != "") {
		return fmt.Errorf("bridge targets do not accept subject_ref/scope_id")
	}
	if target.Kind == "timer" && (target.SubjectRef == "" || target.ScopeID == "") {
		return fmt.Errorf("timer targets require subject_ref/scope_id")
	}
	return nil
}

const applicationTargetColumns = `kind,ref,tenant_id,url,subject_ref,scope_id,token_env,revision,state,digest,created_at,updated_at`

func scanApplicationTarget(row pgx.Row) (*ApplicationTarget, error) {
	var target ApplicationTarget
	if err := row.Scan(&target.Kind, &target.Ref, &target.TenantID, &target.URL, &target.SubjectRef,
		&target.ScopeID, &target.TokenEnv, &target.Revision, &target.State, &target.Digest,
		&target.CreatedAt, &target.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: scan application target: %w", err)
	}
	return &target, nil
}

// GetApplicationTarget reads one dynamic target by globally unique ref.
func (s *Store) GetApplicationTarget(ctx context.Context, kind, ref string) (*ApplicationTarget, error) {
	return scanApplicationTarget(s.Pool.QueryRow(ctx,
		`SELECT `+applicationTargetColumns+` FROM application_targets WHERE kind=$1 AND ref=$2`, kind, ref))
}

// ListApplicationTargets lists active dynamic targets for one tenant. Empty
// tenant is rejected so a lookup can never accidentally span tenants.
func (s *Store) ListApplicationTargets(ctx context.Context, tenantID string) ([]ApplicationTarget, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("store: application target tenant required")
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+applicationTargetColumns+` FROM application_targets
WHERE tenant_id=$1 AND state='active' ORDER BY kind,ref`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: list application targets: %w", err)
	}
	defer rows.Close()
	out := []ApplicationTarget{}
	for rows.Next() {
		target, err := scanApplicationTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *target)
	}
	return out, rows.Err()
}

// UpsertApplicationTarget creates or updates one dynamic target and appends an
// audit row in the same transaction. Bridge routes are immutable; timer route
// changes are allowed but invalidate old timers through the frozen digest.
func (s *Store) UpsertApplicationTarget(ctx context.Context, target ApplicationTarget, actor string) (*ApplicationTarget, bool, error) {
	if err := validateApplicationTarget(target); err != nil {
		return nil, false, err
	}
	if actor == "" {
		actor = "unknown"
	}
	target.State = "active"
	target.Digest = ApplicationTargetDigest(target)
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("store: begin target upsert: %w", err)
	}
	defer tx.Rollback(ctx)

	existing, err := scanApplicationTarget(tx.QueryRow(ctx,
		`SELECT `+applicationTargetColumns+` FROM application_targets WHERE kind=$1 AND ref=$2 FOR UPDATE`,
		target.Kind, target.Ref))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, false, err
	}
	if errors.Is(err, ErrNotFound) {
		row, err := scanApplicationTarget(tx.QueryRow(ctx, `INSERT INTO application_targets
(kind,ref,tenant_id,url,subject_ref,scope_id,token_env,revision,state,digest)
VALUES($1,$2,$3,$4,$5,$6,$7,1,'active',$8) RETURNING `+applicationTargetColumns,
			target.Kind, target.Ref, target.TenantID, target.URL, target.SubjectRef, target.ScopeID,
			target.TokenEnv, target.Digest))
		if err != nil {
			return nil, false, targetStoreError(err)
		}
		if err := insertApplicationTargetAudit(ctx, tx, *row, "create", actor); err != nil {
			return nil, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, false, fmt.Errorf("store: commit target create: %w", err)
		}
		return row, true, nil
	}
	if existing.TenantID != target.TenantID {
		return nil, false, ErrTargetConflict
	}
	if target.Kind == "bridge" && (existing.URL != target.URL) {
		return nil, false, ErrTargetImmutableRoute
	}
	row, err := scanApplicationTarget(tx.QueryRow(ctx, `UPDATE application_targets SET
url=$3,subject_ref=$4,scope_id=$5,token_env=$6,revision=revision+1,state='active',digest=$7,updated_at=now()
WHERE kind=$1 AND ref=$2 RETURNING `+applicationTargetColumns,
		target.Kind, target.Ref, target.URL, target.SubjectRef, target.ScopeID, target.TokenEnv, target.Digest))
	if err != nil {
		return nil, false, targetStoreError(err)
	}
	if err := insertApplicationTargetAudit(ctx, tx, *row, "update", actor); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("store: commit target update: %w", err)
	}
	return row, false, nil
}

// DeleteApplicationTarget removes a dynamic target and audits the revocation.
// Existing timer rows keep their frozen digest and fail closed on delivery.
func (s *Store) DeleteApplicationTarget(ctx context.Context, kind, ref, tenantID, actor string) error {
	if actor == "" {
		actor = "unknown"
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin target delete: %w", err)
	}
	defer tx.Rollback(ctx)
	row, err := scanApplicationTarget(tx.QueryRow(ctx,
		`SELECT `+applicationTargetColumns+` FROM application_targets WHERE kind=$1 AND ref=$2 FOR UPDATE`, kind, ref))
	if errors.Is(err, ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if row.TenantID != tenantID {
		return ErrNotFound
	}
	// Revoke instead of hard delete: the ref and its route digest stay reserved,
	// so re-enrolling the same bridge ref cannot silently redirect old agents.
	revoked, err := scanApplicationTarget(tx.QueryRow(ctx, `UPDATE application_targets SET
state='revoked',revision=revision+1,updated_at=now() WHERE kind=$1 AND ref=$2
RETURNING `+applicationTargetColumns, kind, ref))
	if err != nil {
		return err
	}
	if err := insertApplicationTargetAudit(ctx, tx, *revoked, "delete", actor); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit target delete: %w", err)
	}
	return nil
}

func insertApplicationTargetAudit(ctx context.Context, tx pgx.Tx, target ApplicationTarget, action, actor string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO application_target_audit
(kind,ref,tenant_id,action,actor,revision,digest) VALUES($1,$2,$3,$4,$5,$6,$7)`,
		target.Kind, target.Ref, target.TenantID, action, actor, target.Revision, target.Digest); err != nil {
		return fmt.Errorf("store: audit application target: %w", err)
	}
	return nil
}

func targetStoreError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrTargetConflict
	}
	return fmt.Errorf("store: application target: %w", err)
}
