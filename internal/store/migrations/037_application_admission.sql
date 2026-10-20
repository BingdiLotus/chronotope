-- Application control-plane requests are immutable and scoped by resource.
ALTER TABLE agents ADD COLUMN IF NOT EXISTS request_hash text NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS request_hash text NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS subject_ref text NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS scope_id text NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS request_hash text NOT NULL DEFAULT '';

DROP INDEX IF EXISTS runs_active_session_idx;
CREATE UNIQUE INDEX runs_active_session_idx ON runs (session_id)
  WHERE status IN ('queued', 'running', 'paused', 'awaiting_approval', 'frozen');
