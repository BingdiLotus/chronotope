-- W5 分层记忆（边界语义设计 §7：真相与派生分离）
-- summaries：主题滚动摘要（派生，可重算；版本绑定引用它的 run）
CREATE TABLE IF NOT EXISTS summaries (
    id            bigserial PRIMARY KEY,
    session_id    text NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    topic         text NOT NULL DEFAULT 'default',
    version       int  NOT NULL,
    summary       text NOT NULL,
    diff          text NOT NULL DEFAULT '',
    created_by_run text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (session_id, topic, version)
);

-- memory_items：长期记忆条目（派生，带来源事件引用；content_hash 去重）
CREATE TABLE IF NOT EXISTS memory_items (
    id            bigserial PRIMARY KEY,
    session_id    text NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    topic         text NOT NULL DEFAULT 'default',
    kind          text NOT NULL DEFAULT 'long_term', -- working | long_term（分层）
    content       text NOT NULL,
    content_hash  text NOT NULL,
    source_run_id text,
    source_step   int,
    version       int  NOT NULL DEFAULT 1,
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (session_id, topic, content_hash)
);

CREATE INDEX IF NOT EXISTS memory_items_session_topic_idx ON memory_items (session_id, topic, created_at DESC);
