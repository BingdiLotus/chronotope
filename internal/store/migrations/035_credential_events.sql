-- 凭据服务变更事件：credential.updated 的事件流（set/revoke/unset——
-- 审计与轮换的可观测；dsh 的 reference-updated 对应）。
CREATE TABLE IF NOT EXISTS credential_events (
  id            bigserial PRIMARY KEY,
  org_id        text NOT NULL,
  credential_id text NOT NULL,
  ref           text NOT NULL,
  action        text NOT NULL, -- set | revoke | unset
  at            timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS credential_events_org_idx ON credential_events (org_id, at);
