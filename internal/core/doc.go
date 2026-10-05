// Package core 是共享契约的 Go 落地（落地方案 §1）：
//   - event      事件 schema（契约规范 §5，append-only）
//   - runs       /runs 协议（契约规范 §3，worker ↔ harness）
//   - sessionapi Session API 类型与状态机（契约规范 §2）
//
// 权威定义在 docs/contracts/ 与根目录 契约规范.md；本包与其保持一致，兼容性纪律
// （只增不改、新增字段带默认值、错误码稳定）由 test/contract 契约测试守护。
package core
