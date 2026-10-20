-- One-shot application wake-ups. The row includes the durable submission outbox;
-- no application task, conversation or final-provider delivery state lives here.
CREATE TABLE IF NOT EXISTS application_timers (
    tenant_id text NOT NULL REFERENCES orgs(id),
    timer_id text NOT NULL,
    revision bigint NOT NULL CHECK (revision > 0),
    subject_ref text NOT NULL CHECK (subject_ref <> ''),
    scope_id text NOT NULL CHECK (scope_id <> ''),
    due_at timestamptz NOT NULL,
    opaque_action_ref text NOT NULL CHECK (opaque_action_ref <> ''),
    request_digest text NOT NULL,
    workflow_key text NOT NULL UNIQUE,
    fire_id text NOT NULL UNIQUE,
    target_ref text NOT NULL,
    target_digest text NOT NULL,
    state text NOT NULL DEFAULT 'pending'
      CHECK (state IN ('pending','scheduled','firing','retrying','blocked','acknowledged')),
    submit_attempts integer NOT NULL DEFAULT 0,
    callback_attempts integer NOT NULL DEFAULT 0,
    last_error text NOT NULL DEFAULT '',
    next_submit_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now(),
    submitted_at timestamptz,
    acknowledged_at timestamptz,
    PRIMARY KEY (tenant_id, timer_id, revision)
);

CREATE INDEX IF NOT EXISTS application_timers_pending_submission_idx
    ON application_timers (next_submit_at, created_at)
    WHERE submitted_at IS NULL AND state <> 'acknowledged';
