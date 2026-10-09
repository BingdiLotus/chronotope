# 合规验收 Demo（M3——长周期带审批的资金/工单类 Agent）

> 2026-10-09。场景：受监管场景的典型需求——「处理退款工单：为回调补充幂等键」。
> 验收 = 模拟合规评审的断言清单（w19-compliance.sh 6/6 实证）。
> 弱实现补课状态见文末（诚实标注——「标了 ✅ 其实是桩」比没做更伤）。

## 全链路（w19-compliance.sh 6 断言）

| 步骤 | 断言 | 合规对应 |
|---|---|---|
| Goal | version 1 + state_hash | 目标有独立于聊天的权威表示（慢变量可追踪） |
| WorkItem | 可领取切片（priority/task_class） | 「下一步」是机器可检查的工作对象 |
| 审批 Gate | run.awaiting_approval + action_digest 绑定 | 生产写动作必须停在高权威判断上 |
| 批准后执行 | effect ledger 落账（completed） | 副作用有幂等键与账本——可对账 |
| Evidence | valid_for/source_revision/method | 「为什么相信进展」有新鲜度与来源 |
| 审计链 | 独立验证工具通过（12 条事件链完整） | 历史未改写可向审计师证明 |

## 模拟合规评审的断言清单（客户视角）

1. **目标可追踪**：Goal 的每次变更 version 递增 + state_hash 变化——旧目标不再残留
2. **决策不蒸发**：审批 Gate 有稳定 ID/digest/TTL——「问过但忘了」不可能
3. **证据不短链**：Evidence 绑定 source_revision 与验证方法——「三周前的测试通过」不再被当永久事实
4. **副作用可对账**：effect ledger 的 prepared/dispatched/result/unknown + 幂等键——重复执行不可能
5. **历史不可篡改**：事件哈希链 + 独立验证——审计师可自证
6. **不作为可问责**：DECIDE 的每次「不运行」判定 journaled——「为什么没动」有记录与依据

## 弱实现补课状态（诚实标注）

| 项 | 状态 |
|---|---|
| deliveries ack | ✅ 真实化（RowsAffected 校验 + 409；留痕 = delivered_at） |
| memory 嵌入检索 | ⏳ 降级（topic+recency——与 pgvector 知识库不对称；本场景不依赖语义记忆） |
| fork 父指针合流 | ⏳ 降级（forked_from 字段已有——事件投影继承与空间面合流待演进） |
| internal/policy 测试 | ✅ 补齐（TestQuietWhenIdle） |
| PG LISTEN/NOTIFY | ⏳ 降级（多副本正确性靠 poller DB 轮询已保证——LISTEN 是延迟优化） |
