# Harness 装配设计（接入真实 Harness + Agent 可装配/替换/升级）

> 2026-10-10 设计定稿（先文档后编码）。目标：① 接入真实 harness（第三方按
> /runs 契约实现的 Claude/OpenAI/Pi/dsh harness）；② agent 可装配 harness——
> 绑定、替换、升级、回滚，运行中的 run 不被升级打断。
> 原则：并发原语进层 0、业务语义留层 2（三层分离纪律延续）；run 绑定的
> harness 快照不可变（与 spec digest 同族）。

## 一、现状盘点

| 现状 | 位置 |
|---|---|
| harness 是 worker 级全局单例 | cmd/worker/main.go:57 `NewHarnessClient(*harnessURL)`（HARNESS_URL env） |
| /runs 契约已「0 平台改动换 harness」 | docs/contracts/runs-protocol.md（三铁律：无状态/幂等由 worker/journal/done 唯一终态） |
| spec digest 已有 | store.SpecDigestOf（AgentConfig 的 canonical hash——P2-⑩「accepted spec digest + run 绑定 config 快照」） |
| executor 注册表先例 | executors 表（org 级注册/心跳/能力）——多宿主池的装配可复用同款模式 |
| 一致性套件已有 | test/contract 的 TestHarnessFrameConformance（第三方自证） |
| 薄嵌入 SDK 已有 | sdk/thinruntime（M5——EffectKey/LedgerClient/Decide） |

**缺口**：harness 只有全局 URL 一个装配点——agent 不能绑定不同 harness、
不能在运行中替换升级、升级会波及所有 run（无 run 级快照路由）。

## 二、核心对象设计

### 2.1 HarnessRef（harness 的稳定身份）

```
HarnessRef = { org_id, name, endpoint, version, capabilities, registered_at, heartbeat_at }
```

- `capabilities`：声明支持的模型列表 / 协议版本 / 可选特性（与 Seam 接缝同族——
  第三方自报能力，worker 按需路由）
- 心跳：可观测性（harness 不可用时的显式状态——不静默）

### 2.2 harness_registry 表（org 级注册——与 executors 同款）

```sql
CREATE TABLE harness_registry (
  org_id, name, endpoint, version, capabilities jsonb,
  registered_at, heartbeat_at, state text -- active | draining | retired
  PRIMARY KEY (org_id, name, version)
)
```

- 注册 = 可路由；`draining` = 不再接新 run（旧 run 继续）；`retired` = 完全下线
- 版本语义：同 name 多版本并存（升级 = 注册新版本 + 切 active）

### 2.3 agent 绑定（AgentConfig 加 harness_ref 字段）

```
AgentConfig.harness_ref = { name, version? }   // 空 = org 默认（全局 fallback）
```

- 绑定进 **spec digest**（harness 绑定是 config 的一部分——绑定变更 = digest
  变更 = 新 run 走新绑定，旧 run 沿旧快照——与 P2-⑩ 的语义完全同族）
- 默认解析链：agent 绑定 → org 默认 → 全局 HARNESS_URL（fallback 不破坏现状）

### 2.4 run 级 harness 快照

- run 创建时把「当时解析的 harness (endpoint, version)」冻结进 run 行
  （或经 bound 快照）——**升级不中断旧 run**：升级 = 新 run 走新版本；
  旧 run 的每个 step 继续打旧 endpoint
- 旧 harness 的 `draining` 时长 = 最长 run 寿命的上界（配置参数）

## 三、策略缝与分层

| 层 | 组件 | 职责 |
|---|---|---|
| 层 0 | HarnessResolver 接口 + run 快照原语 | 「run 绑定的 harness 不可变」——不可绕过 |
| 层 1 | policy.HarnessPolicy（策略缝） | 解析链（agent→org→全局）与升级策略的钩子 |
| 层 2 | 参考实现 | 默认解析链 + 注册/替换/升级/回滚的 REST 管理面 |

## 四、替换/升级/回滚生命周期

```
注册（同 name 新 version + capabilities）──► 切换（org/agent 的 active 指向新版本）
  │
  ├─ 新 run：解析新 active → 快照新版本
  ├─ 旧 run：继续打旧版本（快照冻结——不中断）
  │
  ├─ draining：新版本验证期（健康/质量指标）——失败则回滚（active 指回旧版本，
  │            回滚后新 run 走旧版本、无副作用残留——旧 harness 仍是纯函数/无状态）
  └─ retire：旧版本无活跃 run 引用后下线
```

- **热替换的诚实边界**：run 中途不换 harness（快照语义——比 dsh 的进程内热替换
  保守，但效果账本保证了「换不换都不重跑副作用」——评审二说的「不只是不重启，
  而是副作用不重复、可对账」已由账本兑现）
- **卸载**：retire + 无引用 → 删除注册行（LIFO 语义——dsh 可逆效应的外部版：
  卸载前验证无 run 引用，不留泄漏）

## 五、真实 harness 接入路径

1. 第三方实现 /runs 协议（runs-protocol.md 三铁律——无状态、幂等由 worker、
   done 唯一终态）
2. 自证兼容：`go test ./test/contract/`（帧级一致性）
3. 注册：`POST /orgs/{org}/harnesses`（endpoint + version + capabilities）
4. 绑定：agent config 的 harness_ref
5. 验证：w1-loop 闭环 + 真实模型路径（real-model.sh 的 harness 替换版）

**内置 harness 降级为参考实现之一**（harness/app 仍是默认——注册表兼容）。

## 六、与现有资产的衔接

| 资产 | 衔接 |
|---|---|
| executor 注册表（期 4 §B） | harness 注册表同款模式（org 级/心跳/能力/多版本） |
| spec digest（P2-⑩） | harness 绑定进 digest——升级/回滚的不可变快照语义复用 |
| /runs 契约（M4） | 已发布——接入零平台改动；conformance 套件即验收 |
| sdk/thinruntime（M5） | 第三方 harness 侧的幂等/对账原语——接入方的工具 |
| Seam 接缝 | harness 是 Seam 之一（seam-v1.md 已列）——注册表是 Seam 的运行时形态 |

## 七、分阶段落地（设计后执行时按此序）

1. **装配** ✅ 已落地（2026-10-11——029 注册表 + RegisterHarness/ListHarnesses/
   GetActiveHarness/SetHarnessState + POST/GET/PUT 三端点 + AgentConfig.HarnessRef
   进 spec digest + run 快照 bound.harness_endpoint/version 冻结——默认全局
   fallback 零行为变化；TestHarnessRegistryRoundtrip 反例固化）
2. **替换** ✅ 已落地（2026-10-11——policy.HarnessResolver 策略缝 +
   DefaultResolver 解析链（agent 绑定→org active→全局 fallback）+
   HarnessForRun 快照客户端（bound.harness_endpoint 非全局→快照）+
   w22 e2e 3/3：注册/绑定进 digest/升级切态）
3. **升级**：同 name 多版本 + 新 run 走新版本 + 旧 run 沿旧（e2e：升级后旧 run
   继续完成、新 run 用新 harness）
4. **第三方接入验证**：一个真实第三方 harness（Pi/OpenAI SDK 适配）走注册表
   接入 + conformance 自证

## 八、明确不做（诚实边界）

- 不做 run 中途的热替换（快照语义——升级不中断靠「旧 run 沿旧」而非「切换」）
- 不做 harness 的自动 failover（harness 不可用 = 显式失败/重试——不静默换
  endpoint 掩盖故障）
- 不做 harness 侧的持久化（三铁律：harness 无状态——状态仍在 worker/journal）
- 卸载的进程内可逆效应（dsh/Cordis 的 LIFO 撤销链是组件级——harness 是外部
  进程，卸载 = 停路由 + 无引用验证）

## 九、验收

- 装配：agent 绑定 A 与 B 两 harness——同 org 两 agent 分别路由（e2e）
- 升级：旧 run 进行中注册新版本并切 active——旧 run 完成、新 run 用新版本（e2e）
- 回滚：新版本故障 → active 指回 → 新 run 恢复旧版本
- 第三方：一个非内置 harness 经 conformance + 注册表接入（走通记录）
