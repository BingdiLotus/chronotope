-- 消息写回幂等（journal 重放安全）：run 重放会重新执行 AppendMessage，
-- 无去重会重复插入 → 历史污染（真实模型 e2e 实证 400 'tool_call_id' 洪泛）。
-- 先清历史重复行（保留最小 id），再建唯一索引。
DELETE FROM messages a
USING messages b
WHERE a.id > b.id
  AND a.run_id IS NOT DISTINCT FROM b.run_id
  AND a.step = b.step
  AND a.role = b.role
  AND a.content = b.content;

CREATE UNIQUE INDEX IF NOT EXISTS messages_dedupe_idx
  ON messages (run_id, step, role, md5(content::text));
