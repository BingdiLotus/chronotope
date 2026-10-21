-- Machine-readable timer failure classification. The diagnostic text remains in
-- last_error; this column lets applications branch without parsing free text.
ALTER TABLE application_timers ADD COLUMN IF NOT EXISTS last_error_code text NOT NULL DEFAULT '';
