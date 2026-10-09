-- M1 防篡改审计：事件哈希链（sha256(prev_hash || event)）+ 法定保留 hold。
ALTER TABLE events ADD COLUMN IF NOT EXISTS prev_hash text;
ALTER TABLE events ADD COLUMN IF NOT EXISTS event_hash text;
ALTER TABLE events ADD COLUMN IF NOT EXISTS hold boolean NOT NULL DEFAULT false;
