package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/bingdilotus/chronotope/internal/core/sessionapi"
	"github.com/jackc/pgx/v5"
)

var (
	ErrIdempotencyConflict = errors.New("idempotency key reused with a different request")
	ErrSessionBusy         = errors.New("session has an active run")
)

// CompleteSessionInitialization must not overwrite a pause or any later state
// when two retries finish initializing the same remote session out of order.
func (s *Store) CompleteSessionInitialization(ctx context.Context, id string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE sessions SET status='ready',last_active_at=now() WHERE id=$1 AND status='created'`, id)
	return err
}

// RequestHash canonicalizes nested JSON objects before hashing. Authentication,
// connection details and retry timestamps are deliberately not part of a request.
func RequestHash(request any) (string, error) {
	raw, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	raw, err = json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// CreateAgentRequest atomically stores an immutable configuration and its
// request hash. The caller supplies a tenant-scoped deterministic resource ID.
func (s *Store) CreateAgentRequest(ctx context.Context, id, orgID, name string, cfg *sessionapi.AgentConfig, hash string) (bool, error) {
	cfg.Version = 1
	raw, err := json.Marshal(cfg)
	if err != nil {
		return false, err
	}
	tag, err := s.Pool.Exec(ctx, `INSERT INTO agents(id,org_id,name,config,version,spec_digest,request_hash)
VALUES($1,$2,$3,$4,1,$5,$6) ON CONFLICT(id) DO NOTHING`, id, orgID, name, raw, SpecDigestOf(cfg), hash)
	if err != nil {
		return false, fmt.Errorf("store: admit agent: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return true, nil
	}
	var existing string
	if err := s.Pool.QueryRow(ctx, `SELECT request_hash FROM agents WHERE id=$1 AND org_id=$2`, id, orgID).Scan(&existing); err != nil {
		return false, err
	}
	if existing != hash {
		return false, ErrIdempotencyConflict
	}
	return false, nil
}

func (s *Store) CreateSessionRequest(ctx context.Context, id, orgID, agentID, subjectRef, scopeID, hash string) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `INSERT INTO sessions(id,org_id,agent_id,status,restate_key,subject_ref,scope_id,request_hash)
VALUES($1,$2,$3,'created',$1,$4,$5,$6) ON CONFLICT(id) DO NOTHING`, id, orgID, agentID, subjectRef, scopeID, hash)
	if err != nil {
		return false, fmt.Errorf("store: admit session: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return true, nil
	}
	var existing string
	if err := s.Pool.QueryRow(ctx, `SELECT request_hash FROM sessions WHERE id=$1 AND org_id=$2 AND agent_id=$3`, id, orgID, agentID).Scan(&existing); err != nil {
		return false, err
	}
	if existing != hash {
		return false, ErrIdempotencyConflict
	}
	return false, nil
}

// AdmitRun serializes admission on the session, checks the original request
// before busy, and commits run/command/outbox in one short transaction.
func (s *Store) AdmitRun(ctx context.Context, id, sessionID, input, topic string, trigger json.RawMessage, bound map[string]any, hash string) (bool, error) {
	raw, err := json.Marshal(bound)
	if err != nil {
		return false, err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var locked string
	if err := tx.QueryRow(ctx, `SELECT id FROM sessions WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, sessionID).Scan(&locked); err != nil {
		return false, err
	}
	var existing string
	err = tx.QueryRow(ctx, `SELECT request_hash FROM runs WHERE id=$1 AND session_id=$2`, id, sessionID).Scan(&existing)
	if err == nil {
		if existing != hash {
			return false, ErrIdempotencyConflict
		}
		return false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM runs WHERE session_id=$1 AND status IN ('queued','running','paused','awaiting_approval','frozen'))`, sessionID).Scan(&active); err != nil {
		return false, err
	}
	if active {
		return false, ErrSessionBusy
	}
	if _, err := tx.Exec(ctx, `INSERT INTO runs(id,session_id,trigger,status,bound,input,topic,request_hash) VALUES($1,$2,$3,'queued',$4,$5,$6,$7)`, id, sessionID, nullableRaw(trigger), raw, input, topic, hash); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO admission_outbox(run_id,state) VALUES($1,'pending')`, id); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
