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

-- 修正：去重键必须含 session_id（评审「dedupe 前缀会话维度」同款缺陷——
-- 不含 session_id 时两个会话的相同 run 内容互相去重；store 集成测试实证）
DROP INDEX IF EXISTS messages_dedupe_idx;
CREATE UNIQUE INDEX IF NOT EXISTS messages_dedupe_idx
  ON messages (session_id, run_id, step, role, md5(content::text));
