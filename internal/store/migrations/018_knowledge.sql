-- 期 3 §D：tenant 级共享知识库（基础数据服务——存储+检索；业务自行沉淀策略）。
CREATE EXTENSION IF NOT EXISTS vector;
CREATE TABLE IF NOT EXISTS tenant_knowledge (
  id             text PRIMARY KEY,
  tenant_id      text NOT NULL REFERENCES orgs (id),
  content        text NOT NULL,
  embedding      vector(1024) NOT NULL,
  source_session text NOT NULL DEFAULT '',
  created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS knowledge_tenant_idx ON tenant_knowledge (tenant_id);
