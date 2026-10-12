-- 凭据服务阶段 ①：llm_calls 的 credential_ref——账本的凭据维度
-- （「谁用了哪个 key」的审计归属；引用名而非值）。
ALTER TABLE llm_calls ADD COLUMN IF NOT EXISTS credential_ref text NOT NULL DEFAULT '';
