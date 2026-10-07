-- 期 2 §B：usage 增量 rollup 水位 + 冷层归档。
-- 水位：聚合器按全局事件 id 增量累加（重启续跑，不重扫）；
CREATE TABLE IF NOT EXISTS usage_watermark (
  id            int PRIMARY KEY DEFAULT 1,
  last_event_id bigint NOT NULL DEFAULT 0,
  updated_at    timestamptz NOT NULL DEFAULT now()
);
INSERT INTO usage_watermark (id, last_event_id) VALUES (1, 0) ON CONFLICT (id) DO NOTHING;

-- 归档清单：冷层对象引用（对象存 RustFS archives/{org}/{session}/）。
CREATE TABLE IF NOT EXISTS archives (
  session_id   text PRIMARY KEY REFERENCES sessions (id),
  bucket_path  text NOT NULL DEFAULT '',
  events_count bigint NOT NULL DEFAULT 0,
  messages_count bigint NOT NULL DEFAULT 0,
  created_at   timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS archived_at timestamptz;
