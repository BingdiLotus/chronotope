-- 001_init.sql — MVP 初始 schema
-- 依据：契约规范 §4/§5 + mvp-技术选型 §3 数据模型 + 边界语义设计（tombstone）
-- 纪律：事件/消息 append-only（只插入与 tombstone，不更新）；seq 允许 gap；
--       删除走两段式（deleted_at tombstone → 异步 compaction）。

CREATE TABLE IF NOT EXISTS orgs (
  id         text PRIMARY KEY,
  name       text NOT NULL,
  quotas     jsonb NOT NULL DEFAULT '{}',  -- max_sessions/concurrency/ttl（多租户预留）
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS api_keys (
  id         text PRIMARY KEY,
  org_id     text NOT NULL REFERENCES orgs(id),
  key_hash   text NOT NULL UNIQUE,
  scopes     text[] NOT NULL DEFAULT '{}',
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS agents (
  id         text PRIMARY KEY,
  org_id     text NOT NULL REFERENCES orgs(id),
  name       text NOT NULL,
  config     jsonb NOT NULL,               -- model/instructions/tools/mcp/skills/environment/budget
  version    integer NOT NULL DEFAULT 1,   -- 每次变更 +1；run 启动时绑定
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sessions (
  id             text PRIMARY KEY,
  org_id         text NOT NULL REFERENCES orgs(id),
  agent_id       text NOT NULL REFERENCES agents(id),
  status         text NOT NULL DEFAULT 'created',
  restate_key    text,                     -- session_object 的 key（= id）
  last_active_at timestamptz,
  ttl            interval,
  deleted_at     timestamptz,              -- tombstone（边界语义 §4 两段式删除）
  created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS runs (
  id          text PRIMARY KEY,
  session_id  text NOT NULL REFERENCES sessions(id),
  trigger     jsonb,
  status      text NOT NULL DEFAULT 'queued',
  bound       jsonb NOT NULL DEFAULT '{}', -- {agent_config_version, protocol_version, model}：run 启动绑定
  started_at  timestamptz,
  finished_at timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS runs_session_idx ON runs (session_id);

-- 消息历史（worker-架构设计 §3：msgs.Append 必须落表，重放后历史不缺失）
CREATE TABLE IF NOT EXISTS messages (
  id         bigserial PRIMARY KEY,
  session_id text NOT NULL REFERENCES sessions(id),
  run_id     text REFERENCES runs(id),
  step       integer,
  role       text NOT NULL,                -- system|user|assistant|tool
  content    jsonb NOT NULL,               -- 含 tool_calls / source 标记
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS messages_session_idx ON messages (session_id, id);

-- 事件（append-only，单一真相：审计/时间轴回放/计量三用）
CREATE TABLE IF NOT EXISTS events (
  id         bigserial PRIMARY KEY,
  session_id text NOT NULL REFERENCES sessions(id),
  run_id     text,
  seq        bigserial NOT NULL,           -- 每 session 单调；允许 gap（dedupe 冲突烧号）
  type       text NOT NULL,
  payload    jsonb NOT NULL DEFAULT '{}',
  at         timestamptz NOT NULL DEFAULT now(),
  dedupe_key text UNIQUE                   -- run_id:step:kind[:tool]
);
CREATE INDEX IF NOT EXISTS events_session_seq_idx ON events (session_id, seq);
CREATE INDEX IF NOT EXISTS events_run_idx ON events (run_id);

CREATE TABLE IF NOT EXISTS schedules (
  id         text PRIMARY KEY,
  org_id     text NOT NULL REFERENCES orgs(id),
  session_id text NOT NULL REFERENCES sessions(id),
  cron       text NOT NULL,
  timezone   text NOT NULL DEFAULT 'UTC',  -- IANA 时区（边界语义 §5）
  mode       text NOT NULL DEFAULT 'wall-clock', -- wall-clock | fixed-interval
  next_at    timestamptz,
  payload    jsonb NOT NULL DEFAULT '{}'
);

-- outbox：与 events 同事务写入（落地方案 §5）
CREATE TABLE IF NOT EXISTS outbox (
  id         bigserial PRIMARY KEY,
  session_id text NOT NULL REFERENCES sessions(id),
  event_id   bigint NOT NULL REFERENCES events(id),
  url        text NOT NULL,
  attempts   integer NOT NULL DEFAULT 0,
  next_at    timestamptz NOT NULL DEFAULT now()
);

-- 计量三轴（活跃秒 / token / 计算秒），1min 桶，从事件流聚合
CREATE TABLE IF NOT EXISTS usage (
  session_id      text NOT NULL REFERENCES sessions(id),
  bucket          timestamptz NOT NULL,    -- 1min 桶起点（UTC）
  active_seconds  double precision NOT NULL DEFAULT 0,
  tokens_in       bigint NOT NULL DEFAULT 0,
  tokens_out      bigint NOT NULL DEFAULT 0,
  compute_seconds double precision NOT NULL DEFAULT 0,
  PRIMARY KEY (session_id, bucket)
);

-- executor 注册表（capability 路由：docker | e2b_selfhosted）
CREATE TABLE IF NOT EXISTS executors (
  id           text PRIMARY KEY,
  org_id       text NOT NULL REFERENCES orgs(id),
  kind         text NOT NULL,              -- docker | e2b_selfhosted
  endpoint     text NOT NULL,
  capabilities jsonb NOT NULL DEFAULT '{}',
  status       text NOT NULL DEFAULT 'up'
);

-- 沙箱事实状态（契约规范 §4；进程死而沙箱不随进程死）
CREATE TABLE IF NOT EXISTS sandboxes (
  sandbox_id      text PRIMARY KEY,
  org_id          text NOT NULL REFERENCES orgs(id),
  session_id      text NOT NULL REFERENCES sessions(id),
  driver          text NOT NULL,           -- docker | e2b_selfhosted
  container_ref   text,
  image           text NOT NULL,
  limits          jsonb NOT NULL DEFAULT '{}',
  tier            integer NOT NULL DEFAULT 0, -- 0 运行 / 1 冻结 / 2 快照 / 3 拆除
  snapshot_ref    text,
  file_sync_state text NOT NULL DEFAULT 'syncing', -- synced 是 Tier3 拆除前提
  ttl             interval,
  status          text NOT NULL DEFAULT 'creating', -- creating→ready→frozen→snapshotted→destroyed
  created_at      timestamptz NOT NULL DEFAULT now()
);

-- executor 幂等缓存（防重试双执行，契约规范 §4；TTL 24h；结果 >256KB 外置 MinIO）
CREATE TABLE IF NOT EXISTS sandbox_execs (
  idempotency_key text PRIMARY KEY,        -- (run_id, step, tool_id)
  sandbox_id      text NOT NULL REFERENCES sandboxes (sandbox_id),
  result          jsonb,
  created_at      timestamptz NOT NULL DEFAULT now(),
  expires_at      timestamptz NOT NULL
);
