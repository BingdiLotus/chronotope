-- Dynamically registered application endpoints. Static env bootstrap remains the
-- deployment baseline; this table only adds a runtime-managed overlay. Tokens are
-- never stored: token_env is an environment-variable reference resolved by both
-- API and worker at use time.
CREATE TABLE IF NOT EXISTS application_targets (
    kind         text NOT NULL CHECK (kind IN ('bridge','timer')),
    ref          text NOT NULL,
    tenant_id    text NOT NULL REFERENCES orgs(id),
    url          text NOT NULL,
    subject_ref  text NOT NULL DEFAULT '',
    scope_id     text NOT NULL DEFAULT '',
    token_env    text NOT NULL CHECK (token_env <> ''),
    revision     bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    state        text NOT NULL DEFAULT 'active' CHECK (state IN ('active','revoked')),
    digest       text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (kind, ref),
    CHECK (
        (kind = 'bridge' AND subject_ref = '' AND scope_id = '') OR
        (kind = 'timer' AND subject_ref <> '' AND scope_id <> '')
    )
);

-- A tenant+subject+scope may have exactly one timer receiver. Multiple bridges
-- per tenant are allowed (ref remains globally unique by primary key).
CREATE UNIQUE INDEX IF NOT EXISTS application_targets_timer_scope_idx
    ON application_targets (tenant_id, subject_ref, scope_id)
    WHERE kind = 'timer' AND state = 'active';

CREATE TABLE IF NOT EXISTS application_target_audit (
    id         bigserial PRIMARY KEY,
    kind       text NOT NULL,
    ref        text NOT NULL,
    tenant_id  text NOT NULL REFERENCES orgs(id),
    action     text NOT NULL CHECK (action IN ('create','update','delete')),
    actor      text NOT NULL,
    revision   bigint NOT NULL,
    digest     text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS application_target_audit_tenant_idx
    ON application_target_audit (tenant_id, created_at);
