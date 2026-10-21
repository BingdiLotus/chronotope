package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/bingdilotus/chronotope/internal/core/timer"
	"github.com/jackc/pgx/v5"
)

var ErrTimerConflict = errors.New("timer identity already has a different request")

const applicationTimerColumns = `tenant_id,timer_id,revision,subject_ref,scope_id,due_at,opaque_action_ref,
request_digest,workflow_key,fire_id,target_ref,target_digest,state,submit_attempts,callback_attempts,
last_error,last_error_code,created_at,submitted_at,acknowledged_at`

func scanApplicationTimer(row pgx.Row) (*timer.Record, error) {
	var r timer.Record
	if err := row.Scan(&r.TenantID, &r.TimerID, &r.Revision, &r.SubjectRef, &r.ScopeID, &r.DueAt, &r.OpaqueActionRef,
		&r.RequestDigest, &r.WorkflowKey, &r.FireID, &r.TargetRef, &r.TargetDigest, &r.State,
		&r.SubmitAttempts, &r.CallbackAttempts, &r.LastError, &r.LastErrorCode, &r.CreatedAt, &r.SubmittedAt, &r.AcknowledgedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: scan timer: %w", err)
	}
	return &r, nil
}

// CreateApplicationTimer commits both the timer and its pending submission duty
// in one row. A conflict lookup is a separate statement so concurrent insert
// commit visibility follows READ COMMITTED, rather than an INSERT CTE snapshot.
func (s *Store) CreateApplicationTimer(ctx context.Context, req timer.Request, targetRef, targetDigest string) (*timer.Record, bool, error) {
	req = req.Normalize()
	if err := req.Validate(); err != nil {
		return nil, false, err
	}
	if targetRef == "" || targetDigest == "" {
		return nil, false, fmt.Errorf("store: configured timer target required")
	}
	row, err := scanApplicationTimer(s.Pool.QueryRow(ctx, `INSERT INTO application_timers
(tenant_id,timer_id,revision,subject_ref,scope_id,due_at,opaque_action_ref,request_digest,workflow_key,fire_id,target_ref,target_digest)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
ON CONFLICT (tenant_id,timer_id,revision) DO NOTHING RETURNING `+applicationTimerColumns,
		req.TenantID, req.TimerID, req.Revision, req.SubjectRef, req.ScopeID, req.DueAt, req.OpaqueActionRef,
		req.Digest(), req.Key(), req.Fire().FireID, targetRef, targetDigest))
	if err == nil {
		return row, true, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, false, err
	}
	row, err = s.GetApplicationTimer(ctx, req.Handle())
	if err != nil {
		return nil, false, err
	}
	if row.RequestDigest != req.Digest() {
		return nil, false, ErrTimerConflict
	}
	return row, false, nil
}

func (s *Store) GetApplicationTimer(ctx context.Context, h timer.Handle) (*timer.Record, error) {
	return scanApplicationTimer(s.Pool.QueryRow(ctx, `SELECT `+applicationTimerColumns+` FROM application_timers
WHERE tenant_id=$1 AND timer_id=$2 AND revision=$3`, h.TenantID, h.TimerID, h.Revision))
}

// ListPendingApplicationTimers scans submission duties, never due tasks. Duplicate
// pollers are safe because every submission uses the same workflow key.
func (s *Store) ListPendingApplicationTimers(ctx context.Context, limit int) ([]timer.Record, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+applicationTimerColumns+` FROM application_timers
WHERE submitted_at IS NULL AND state <> 'acknowledged' AND next_submit_at <= now()
ORDER BY next_submit_at,created_at LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: pending timers: %w", err)
	}
	defer rows.Close()
	var out []timer.Record
	for rows.Next() {
		r, err := scanApplicationTimer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func (s *Store) MarkApplicationTimerSubmitted(ctx context.Context, h timer.Handle) error {
	return s.updateApplicationTimer(ctx, h, `UPDATE application_timers SET
submitted_at=COALESCE(submitted_at,now()),submit_attempts=submit_attempts+1,
state=CASE WHEN state='pending' THEN 'scheduled' ELSE state END,
last_error=CASE WHEN state='pending' THEN '' ELSE last_error END,
last_error_code=CASE WHEN state='pending' THEN '' ELSE last_error_code END
WHERE tenant_id=$1 AND timer_id=$2 AND revision=$3`)
}

func (s *Store) RecordApplicationTimerSubmitError(ctx context.Context, h timer.Handle, code, message string) error {
	return s.updateApplicationTimer(ctx, h, `UPDATE application_timers SET submit_attempts=submit_attempts+1,
last_error=CASE WHEN state='pending' THEN $4 ELSE last_error END,
last_error_code=CASE WHEN state='pending' THEN $5 ELSE last_error_code END,
next_submit_at=now()+interval '5 seconds'
WHERE tenant_id=$1 AND timer_id=$2 AND revision=$3`, message, code)
}

func (s *Store) BeginApplicationTimerCallback(ctx context.Context, h timer.Handle) error {
	return s.updateApplicationTimer(ctx, h, `UPDATE application_timers SET
state=CASE WHEN state='acknowledged' THEN state ELSE 'firing' END,
callback_attempts=callback_attempts+CASE WHEN state='acknowledged' THEN 0 ELSE 1 END
WHERE tenant_id=$1 AND timer_id=$2 AND revision=$3`)
}

func (s *Store) RecordApplicationTimerCallbackError(ctx context.Context, h timer.Handle, blocked bool, code, message string) error {
	state := "retrying"
	if blocked {
		state = "blocked"
	}
	return s.updateApplicationTimer(ctx, h, `UPDATE application_timers SET
state=CASE WHEN state='acknowledged' THEN state ELSE $4 END,
last_error=CASE WHEN state='acknowledged' THEN last_error ELSE $5 END,
last_error_code=CASE WHEN state='acknowledged' THEN last_error_code ELSE $6 END
WHERE tenant_id=$1 AND timer_id=$2 AND revision=$3`, state, message, code)
}

func (s *Store) ConfirmApplicationTimer(ctx context.Context, h timer.Handle) error {
	return s.updateApplicationTimer(ctx, h, `UPDATE application_timers SET
state='acknowledged',acknowledged_at=COALESCE(acknowledged_at,now()),last_error='',last_error_code=''
WHERE tenant_id=$1 AND timer_id=$2 AND revision=$3`)
}

func (s *Store) updateApplicationTimer(ctx context.Context, h timer.Handle, q string, args ...any) error {
	params := []any{h.TenantID, h.TimerID, h.Revision}
	params = append(params, args...)
	tag, err := s.Pool.Exec(ctx, q, params...)
	if err != nil {
		return fmt.Errorf("store: update timer: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}
