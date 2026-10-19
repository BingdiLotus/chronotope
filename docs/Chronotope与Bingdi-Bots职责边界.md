# Chronotope 与 Bingdi Bots：职责、数据权威及集成边界

> MVP 实施约束（2026-10-19）：Chronotope 未上生产，Bots 项目位于 `/Users/yangziwen/workspace/git/BingdiLotus/bots`，规划远程为 [BingdiLotus/bots](https://github.com/BingdiLotus/bots)，当前只有启动设计，本地 Git 已初始化、remote 尚未绑定。下面定义能力所有权，**不要求本期完整实现所有对象/接口**；下一里程碑以[期 8 G0/G1/G2](./期8-细化落地方案.md)为准：Bots 自有简单知识/任务表、单 provider/账号/harness、单次提醒，允许清库重建，无旧协议或数据迁移前置。

> 2026-10-19。状态：**三方边界设计待实施**。Bingdi Bots 尚未创建项目，本轮不迁移代码、数据或运行状态。
> 最新定位：Bingdi Bots 是 ToB 企业 Bot 治理控制台，并提供组织 Bot 的原生知识、任务、计划、调度和自我承诺能力。Bot 属于企业、组织或团队；成员交互 UI 留在 Slack、GitHub、Linear 等外部应用，外部系统自有对象、规则和 ACL 仍由其权威决定。
> 本文是三方 owner 的 canonical 定义。[期 8 方案](./期8-细化落地方案.md)、[身份与 RSI 设计](./持久Agent身份与非模型RSI设计.md)及既有调研若将 Bot 内化记忆限制为缓存/引用，或要求原生任务/承诺必须先在外部立账，以本文为准。反之，原生能力也不能接管外部 issue/calendar/订单/合同的状态权威。
> 研究参照：[前沿调研](./前沿调研-持久Agent与非模型自改进-2026-10.md)、[工业产品调研](./工业产品调研-持久Agent与托管运行平台-2026-10.md)。研究和竞品能力不改变本项目的数据权威，也不表示已通过生产验收。

## 1. 核心分工

**Bingdi Bots 治理企业自己的 Bot，并拥有其原生知识与工作能力；外部应用拥有各自的协作和业务系统；Chronotope 提供通用执行 infra。** 可类比系统内置应用与外部安装应用：原生能力不必先有一个外部同名对象，接入外部系统也不改变其规则和权限权威。

- **外部应用及其指定权威服务**：成员/客户/项目等业务身份与账号关系、该系统的业务白名单/黑名单、issue/calendar/订单/合同等对象状态及验收条件、源数据权限和披露授权。Slack 等应用也是源消息、频道成员和会话历史的权威。
- **Bingdi Bots**：企业所有的 BotID、管理员治理，以及 BotKnowledge、Experience、Skill、BotTask、Plan、Schedule 和 BotCommitment。Bot 可自建研究、生成报告、安排提醒/跟进，按自身 task contract/verifier 判定原生工作完成；也可持久内化业务记忆与新归纳。它不能因此修改外部系统规则、扩大其 ACL 或自行宣称外部对象已经完成。
- **Chronotope**：runtime agent/session/run、持久接纳与邮箱、有限 activation、父子执行、等待/恢复、操作账本、资源隔离、版本绑定、预算执行及受信批准后的技术生效。

管理员配置“Bot 只能调用哪些工具、读哪些已授权数据源、花多少预算、何时需要治理审批”，属于 Bot 治理；“外部系统哪些客户可享某项服务、哪些 issue 允许关闭、外部合同是否履行”，属于该系统的业务规则。Bots 自己的管理面 RBAC 及原生 task/commitment 契约由 Bots 管理，不与外部权限混同。SOP 可定义 Bot 的工作、规划和调用过程，知识也可保留带来源的业务理解；但不得把外部业务白黑名单或 allow 条件复制成取代源端判定的本地授权权威。

Bot 可以在 Chronotope 的 durable activation 中执行，也可以通过受管 adapter 接收外部消息。执行位置不改变 owner。Bots 控制台可管理原生知识、任务/计划/调度和承诺，并提供配置、监控、治理审批与审计；不另建成员聊天 composer/群聊，也不复制外部工单/项目系统。原生任务视图与外部对象的引用/投影分别展示。

Bot 管理员能配置、发布或暂停 Bot，不因此取得成员私聊或外部业务内容的读取权。审计默认展示其有权管理范围内的运行元数据、hash 和引用；展开、检索或导出内容仍须取得源应用的当前授权，并满足审计访问策略，不能以管理权限或后台服务账号绕过。

## 2. 三方职责矩阵

| 领域 | 外部应用 | Bingdi Bots | Chronotope |
|---|---|---|---|
| Bot 身份与管理 | 提供企业目录/可信主体引用 | 企业/组织/团队拥有稳定 BotID；管理员管理人格、维护角色及生命周期 | 注册 runtime AgentID、执行主体及技术配置地址 |
| 成员交互与会话 | 聊天、评论、频道/仓库/项目成员、源消息及 revision | 管理接入、路由、reply target 与外部会话引用/session index；不建聊天产品 | 通用 envelope、邮箱、去重、消费领取及恢复 |
| 业务账号与授权 | 账号关系、成员私有授权、服务账号实际权限、业务允许/拒绝 | 配置 Bot 允许绑定的账号、工具与数据访问上限；不能扩大外部权限 | 验证可信授权引用、scope/epoch、有效期及派发 fencing |
| 白名单与规则 | 外部系统自有对象的业务名单、条件、授权和验收权威 | 原生知识应用/计划/调度规则及 Bot 治理；可带来源内化外部规则供推理，不替代源端当前许可 | 强制技术目标/工具/资源约束，不解释外部业务资格 |
| 原生任务、计划与承诺 | 可提供请求、资料和外部结果，不是创建原生任务的前置 | BotTask/Plan/Schedule/BotCommitment 的 ID、契约、期限、状态、验收和修订；可自主创建并推进 | run/attempt、child、wait、定时唤醒、取消、receipt、unknown 与结果回投 |
| 外部工作对象 | 该系统 issue/calendar/订单/合同的 ID、状态、规则和最终验收 | 保存 ExternalWorkRef/BotWorkLink，可经授权调用外部 API；不冒充其状态权威 | 通用工具调用、操作账本及可核对结果 |
| loop/SOP 与技能 | 暴露业务动作及规则接口、反馈/验收结果 | 管理 Bot 工作方法、协作策略、harness/skills 和准入配置；不改外部规则 | 通用执行协议、隔离宿主、工具桥接、版本与恢复 |
| Memory 与知识 | 源业务记录、revision、纠错/删除/共享决议 | BotKnowledge/Experience/Skill 是持久知识资产，可有新归纳和内部版本，不限于缓存；不改外部源记录 | 通用存储/索引、来源、lineage、版本/CAS、授权读取及冻结快照；存储不改变语义 owner |
| 失效与修复 | 提供源数据/权限失效事件及其权威水位 | 按依赖策略处理内化知识/经验、缓存、skills、候选和原生工作；可施加更严格治理限制 | 安装读取/派发/发布屏障，执行有界修复 job，报告覆盖与回执 |
| 资源隔离与恢复 | 决定源服务账号和数据访问约束 | 配置 Bot 间可共享范围、隔离要求和受管部署方式 | 强制 workspace/volume/browser/credential 真实边界及 snapshot/journal 恢复合同 |
| Routine/调度 | 可选事件/日历同步、外部动作的授权与结果 | 原生 Schedule/automation 可自行立项，不需先建外部 calendar；固定执行身份、触发/SOP/目标及治理版本 | 定时/事件唤醒、幂等接纳、等待、资源和预算执行 |
| 改进与发布 | 提供源资料及外部对象的反馈/验收，不把规则权威交给候选 | 评测内化知识、方法、原生任务完成、成本与合规；企业受信角色批准 release | 隔离 trial、不可变制品、执行证据、技术版本生效 CAS 及回滚 |
| 运营 | 业务运营、协作 UI 与业务状态查询 | Bot 治理控制台、账号/策略/预算/部署/审计管理 | 运行/对账/资源管理、SLO、容量、灾备与故障诊断 |

三方可共用物理 PG/对象存储，但每类记录只有一个写入权威，不因共库而跨 DAO/schema 任意修改。Chronotope 可附带参考策略和最小 consumer；示例不使业务语义成为内核强制模型。

## 3. 最少治理对象与身份绑定

### 3.1 企业所有的 Bot，不是个人助理

`ManagedBot` 是 BotIdentity 的治理登记/管理视图命名，共用同一个 `bot_id`，不另建第二个身份对象或 active 指针。该登记至少保存 `tenant_id/bot_id/owning_org_unit_ref/lifecycle_state`。`owning_org_unit_ref` 指企业、组织或团队；`created_by`、管理员、维护人员、请求者和审批者分别记录，不以某个人创建或首次私聊就产生个人 BotOwner。人员离职不自动删除企业 Bot；其个人授权撤销仍立即影响相关调用。

治理和原生 Bot 能力保留以下对象；不因其与业务有关就要求外部已有同名对象：

| 对象 | 必要内容 |
|---|---|
| BotIdentity（ManagedBot 管理视图） | 企业所有权、稳定 BotID、管理角色、启停/归档状态 |
| BotRelease（企业治理 release） | 人格/profile、工具/数据访问上限、loop/SOP/harness/memory 策略及评测/批准的不可变引用；沿用既有资源名 |
| AppBinding | 外部应用/安装/namespace、受信账号和授权权威引用、工具 schema、事件路由、唯一技术 sender/receipt owner、撤销状态 |
| ParticipationSessionPolicy | 谁可请求/补充/取消 Bot 执行、上下文边界、DM/共享频道策略；不替代源应用成员/数据 ACL |
| BotKnowledge / Experience / Skill | 内化知识、经历和可复用方法的稳定 ID/版本、证据、置信度/适用性、来源与授权范围 |
| BotTask / Plan | 原生目标、步骤/依赖、task contract/verifier、状态及执行关联；外部工作引用可为空 |
| BotSchedule / BotCommitment | 原生提醒/唤醒与 Bot 自身责任、有效修订、期限、兑现条件及证据；无需外部 calendar/commitment 对象 |
| AutomationDefinition | 企业所属 automation、维护角色、固定 execution identity、触发/SOP、输入范围、预算及报告目标 |
| BotRuntimeBinding | BotID 到 runtime provider/tenant/AgentID 的版本化路由及历史 |
| RequestExecutionIndex / BotWorkLink | 关联原生任务/计划/承诺、可选外部 conversation/work、requester、授权、runtime handle 与操作/receipt；不以索引替代原生状态 |
| ImprovementProposal / GovernanceApproval | 候选、评测、来源范围、批准及生效回执；只修改受准许的 Bot 自身实现 |

`ExternalWorkRef` 是可选的应用/资源 ID、revision、授权与状态查询引用；其只读投影不重定义外部对象的期限、修订或验收。BotTask/Plan/Schedule/BotCommitment 自有独立状态和关联，不因没有 ExternalWorkRef 就无效，也不需要为每项原生工作先创建外部 issue/calendar。账号关联仍由外部应用或企业身份权威证明；Bots 保存验证引用和接纳时的 mapping revision，不接管外部客户/成员账号关系主库。

### 3.2 两类 binding 与迁移

```text
BotRuntimeBinding { // Bots 的治理路由
  binding_id, binding_version, tenant_id, bot_id, role,
  runtime_provider, runtime_tenant_id, runtime_agent_id,
  lifecycle_state
}

RuntimeReleaseBinding { // Chronotope 的技术生效资源
  runtime_binding_id, runtime_agent_id, environment_ref, scope_ref,
  spec_revision, effective_revision
}
```

Bots 保存 BotRuntimeBinding 及历史，并投影 RuntimeReleaseBinding 的生效回执。BotID 不由模型、渠道或技术注册 ID 派生；迁移 infra、重建 AgentID 或升级 release 不改变企业 Bot 身份。克隆为独立受管 Bot 才产生新 BotID。

命令首次派发冻结两份引用。已接纳命令/attempt 的重试、查询、取消、订阅和回执走原执行 owner；改绑只影响新工作。不同 provider 不共享幂等账本，同 command/operation ID 不使跨 infra 重发安全；在途迁移须先排空或显式交接运行、claim、结果与 unknown，再创建受控的新 attempt。旧回执及有效 child 不能因当前路由变化被丢弃。

BotID metadata 不授予权限。请求者、企业 Bot、实际 execution identity、维护/审批角色分别认证和审计；解绑/重绑不追溯改写历史作者，也不转移旧批准或个人账号权限。

### 3.3 上下文身份不等于物理安全域

Bot、Conversation、session、child 和 screen 均不天然隔离文件、浏览器登录、网络或凭据。Bots 配置受管资源的共享/隔离要求，且不得超过源应用授权；Chronotope 落实真实 workspace、volume、容器/虚拟机、browser-state 和 credential namespace 边界。不能因同 Bot/同账号而自动共享，也不将工业产品的共享电脑布局照搬为默认。

技术 spec 声明可用资源和隔离模式；每次 run/child 冻结实际 ExecutionResourceBinding、策略 revision、scope 和 epoch。跨边界交接使用有权 artifact 引用或受控复制，不通过挂载、登录或环境变量继承绕开检查；服务凭据仍由代理使用。

恢复报告包含 snapshot/manifest、实际恢复范围、源水位与恢复水位、缺失/未同步项及一致性结果。磁盘、文件快照和 workflow journal 分别说明；文件保留不等于在途 turn 已恢复。不一致时隔离派发，由受信治理流程和必要的外部业务 owner 决定重建/continuation；不能静默恢复为空目录。恢复不复活撤销授权，浏览器状态及凭据也按当前身份/epoch 重验。

## 4. 数据权威、来源与派生状态

| 记录/决议 | 单一权威 | Bots 与 infra 的边界 |
|---|---|---|
| 源消息、外部会话、外部业务对象/事实及 revision | 外部应用 | Bots 可引用、学习或形成自己的知识，不能改写源记录或冒充外部最新状态 |
| BotKnowledge/Experience/Skill 的内部内容及版本 | Bots | 可持久存在 Bots 存储或 Chronotope 通用 store；infra 承载记录/来源/CAS，不接管知识语义 |
| 外部系统自有规则、账号关系、数据读取/披露与业务审批 | 外部应用或其指定权威 | Bots 可加更严治理上限；原生知识应用/任务规则另归 Bots，infra 验证适用的决议和派发约束 |
| Bot 配置、治理 release、应用 binding、参与/预算策略 | Bots 的企业管理员/受信服务 | 外部成员消息不直接修改；Chronotope 只执行已批准技术配置 |
| Runtime mailbox、run/attempt、等待、执行 claim 和工具账本 | Chronotope | Bots 保存句柄与审计投影，不自行覆盖执行事实 |
| BotTask/Plan/Schedule/BotCommitment 及原生验收 | Bots | 由自身 contract/verifier/receipt 判定；infra 提供运行/调度机制，外部对象可不存在 |
| 外部 issue/calendar/订单/合同的状态及验收 | 外部应用 | 通过 BotWorkLink/ExternalWorkRef 关联，不能用 BotTask completed 或原生承诺 fulfilled 覆盖外部状态 |
| provider 消息接受/送达/阅读的技术 receipt | AppBinding 声明的唯一 sender/receipt owner | 源消息和成员可见性仍归外部应用；工具接纳 ACK 不等于 provider delivered |
| Bot 改进候选、治理评测及发布批准 | Bots 受信评测/发布角色 | 原生工作用 Bot 自身 verifier，外部对象用外部验收；infra 返回隔离 job/制品/生效证据 |
| RuntimeReleaseBinding/spec 实际生效版本 | Chronotope | Bots 保存 desired/approved 并投影 effective receipt，不另建 active 真相 |

### 4.1 知识、经验、技能与两类工作对象

- **BotKnowledge**：Bot 可从业务工作、用户反馈和自我反思形成新知识、归纳、假设与专业理解，不要求外部先有同一条记录，也不限于对外部数据做缓存。内部版本记录来源/证据、置信度、适用性、有效性、scope 及冲突/修订关系；Bot 对自身知识负责，但内部断言不自动成为外部系统的最新事实或业务许可。
- **BotExperience**：持久保存某次经历的业务上下文、方法、反馈与教训；引用 Chronotope 的执行事件/结果和实际 sender 的 receipt。BotTask 成败按自身契约验证，涉及外部对象的结果另查外部权威，不能只凭模型自述统一标成功。
- **Skill**：可持久内化业务方法、步骤与检查表，按版本化准入和当前适用性复用；外部白黑名单/ACL 的记忆不取代源端当前判定。技能内容及语义 owner 归 Bots，物理存储可使用 Chronotope 的通用制品/记录服务。
- **BotTask/Plan/Schedule**：Bot 可在已配置的治理范围内自主创建、拆解、修订和调度业务相关工作，如研究、生成报告和会前提醒；不需要先在 Linear、GitHub 或外部 calendar 建对象。
- **BotCommitment**：Bot 自身承接的执行责任，如“明天交报告”或“会前提醒”，可在 Bots 原生立账，绑定有效契约、期限、修订与证据。谁可修订/释放及何时兑现由该原生契约决定，不依赖外部 commitment API；外部合同、订单或工单自身的履约仍按其权威处理。

以上原生知识资产和工作对象可存 Bots 的数据库，也可由 Chronotope 的通用 namespace/record/blob/index/CAS API 承载；**存储位置不改变 Bots 的语义 owner**。不要求在两边各建一套主记录。组织所有不等于全组织成员自动可读。内化不是自动去来源化：外部 source/revision、私有输入的 ACL/删除约束及派生链仍需保留，纠正、撤权与删除按依赖策略阻断新使用并修复；新归纳也要标明证据和适用范围，不能借“知识资产”解除限制。

DM 仅决定上下文隔离和交互范围，不产生个人 Bot、个人 owner 或隐式私人凭据访问权。隐私标签可以由模型建议，但不能创造授权；扩大披露范围仍需相应数据权威许可，并满足 Bot 治理限制。取消 run、解绑渠道、压缩记忆或回滚 release 不自动删除原生任务/承诺，也不替外部应用改变其对象状态。

### 4.2 DerivationManifest 与 coverage

原生知识/经验及其摘要、embedding、缓存、skill、prompt/harness 候选和评测诊断至少关联：

```text
DerivationManifest {
  artifact_ref, artifact_revision, artifact_digest,
  source_refs_and_revisions, predecessor_artifact_refs,
  producer_run_ref, bot_release_ref, policy_revision,
  external_authority_decision_refs, governance_decision_refs,
  native_contract_or_acceptance_refs,
  allowed_scope_refs, lineage_coverage, capture_boundary, known_gaps
}
```

外部应用定义源记录/权限的语义与变化；Bots 定义自身知识、原生任务和方法的依赖、跟随/修复策略及治理决议，不能重写源业务规则。原生归纳可引用多个来源、内部 Experience 或推理过程，不要求与某个外部对象逐条同构。Chronotope 保存通用 lineage、实际输入引用和不可变 manifest，核对版本/digest、计算已记录边的闭包。每份制品的写入 owner 负责自己的 manifest，副本不产生第二个可改写权威。

coverage 区分声明捕获边界内完整、partial 和 unknown；实际消费的来源不能只用最终 citation 代替。“未引用”不证明“未受影响”，模型自报 complete 也不是证明。覆盖范围不含未观测的外部过程或模型权重知识。缺边时只得到已知闭包，Bots 保守隔离/重建自己的知识和制品并报告缺口；原生依赖由 Bots 处理，涉及外部来源/对象权威的歧义才回到相应外部服务，不宣称零泄露或完整机器遗忘。

### 4.3 先撤下，再修复

1. 外部应用按能力提供有权来源/权限失效事件或当前校验结果，以及实际可得的 revision/epoch。Bots 按依赖策略阻断受影响知识/经验、缓存、路由和技能的新使用；缺少源端版本/撤销通知的覆盖限制须明确，不能自造源 epoch 或宣称完整即时传播。企业管理员也可施加更严格 Bot 治理禁用，但不能反向授予外部访问。
2. Chronotope 的读取/派发/发布门，以及实际 memory/tool 服务和最终 sender，分别安装屏障并返回目标范围、水位及回执。仅在相关门均确认后，才报告该范围的后续新 claim 已阻断；未确认保持 pending/blocked。
3. 后续异步清理/修复；Bots 可修订自己的知识/经验、上下文、技能及原生任务依赖，修改外部源记录或对象仍调用其 API。infra 运行受限 job、保存 successor，按当前来源屏障、该对象写入规则及必要的技术/发布批准 CAS 生效；不把日常知识积累强制变成逐条 release 发布。

纠错/删除/撤权不等待 canary，也不随 release rollback 撤销。已看见的上下文、第三方副本和已发生效果不能承诺抹除；必要时隔离运行并用新上下文/模型调用继续，不改旧 journal 冒充重放。已派发或 unknown 的 operation 保持原身份、原 sender/执行 owner 对账，不能换 attempt/ID 盲目重做。Bot 的原生任务/承诺如何阻塞、重规划或修订由其契约和依赖策略决定；外部对象状态仍由外部权威决定，两者都不因撤下记忆而被默默删除。

### 4.4 原生工作与外部对象分别寻址和验收

| 对象 | ID/状态权威 | 关键合同 |
|---|---|---|
| BotTask / Plan | Bots 的 `bot_task_id/plan_id`、contract revision、步骤/依赖及 task 状态 | 可无外部对象；按 task contract/verifier、产物和证据判定内部完成，不由 run.completed 自动推导 |
| BotSchedule | Bots 的 `bot_schedule_id`、schedule revision、发生次序和启停状态 | 原生闹钟/周期任务不依赖外部 calendar；Chronotope 承载具体 timer/wait/occurrence 的执行与回执 |
| BotCommitment | Bots 的 `bot_commitment_id`、有效 revision、期限及 fulfilled/overdue 等原生状态 | 保留承担、延期、释放和兑现记录；方法升级不改旧验收条件，缺证据/发送 unknown 不自报履责 |
| 外部 issue/calendar/订单/合同 | `app/namespace/resource_type/external_id`、外部 revision 和状态 | 通过 API 读取/操作；本地投影注明观察时间，不改写其权威状态或验收 |
| Runtime run/attempt | Chronotope 的 run/attempt、等待、取消和终态 | 表示执行发生了什么，不代替 BotTask 或外部对象的验收 |
| Outbound operation/delivery | AppBinding 指定 sender 的稳定操作 ID、provider receipt/unknown | 区分准备、发出、provider 接受/送达/已读；不能凭一次工具接纳就判提醒送达 |

BotWorkLink 以原生任务/计划/承诺 ID 关联可选 ExternalWorkRef、关系类型、外部 revision 和观察时间；允许多对多，不要求 ID 或状态一一对应。生成报告的 BotTask 可以完成而关联 issue 仍 open；若任务契约包含“向指定受众发出提醒”，则按其要求的实际 receipt 判定，unknown 保持待核验，不能宣称已送达。外部合同成功也不自动证明 Bot 自己约定的全部工作都完成。

BotSchedule 记录固定时间或跟随外部事件的策略，以及 timezone、来源变化和错过触发的处理。外部会议改期/取消，不使内部 task/alarm 自动同态变更：按其固定/跟随策略显式更新、暂停、取消或保留跟进，并保存版本与原因。来源权限撤销先阻断新使用；任务和自身责任是否终止另依原生契约处理。

普通 memory 积累、原生 Task/Plan 创建/更新及已授权 Schedule 操作是日常内置能力，按对象自身的单 owner、版本/CAS、来源和治理限制提交；不要求每条经过 release、独立评测或管理员批准。改变未来行为的方法、可复用 skill、SOP/harness/memory policy 才走相应候选/晋升合同，不能将降低旧 task/commitment 验收口径伪装成日常更新。

## 5. 治理 release 与受控非模型改进

- **BotRelease（Bots 的企业治理 release）**：沿用既有资源名，组合管理员配置的人格/profile、loop/SOP/harness/skills、上下文/memory 方法、工具/数据访问上限、参与策略、预算及治理评测/批准引用。原生知识应用、计划和调度规则可由 Bots 管理；外部规则/名单可带来源和版本被内化，但其当前许可及源对象状态仍调用外部权威，不新增另一套治理 release/active 资源。
- **RuntimeSpecRevision/Deployment（Chronotope）**：协议、资源和制品 digest、技术兼容及部署配置，保存 opaque BotRelease 引用。运行中的动态资源/租约另由 ExecutionResourceBinding 冻结。

Bots 决定治理 release 的 desired/approved、cohort 和晋升；Chronotope 校验受信批准、制品及 expected-effective-revision，以 RuntimeReleaseBinding CAS 返回实际 effective receipt。BotRuntimeBinding 仍决定 provider 路由，不与 spec 生效混为一个对象。旧 run 沿旧 bound，但所有新读取、派发和输出受当前外部授权、Bot 治理及 infra epoch 约束。

受控方法改进覆盖上下文检索/压缩、程序性技能、工具选择、协作节奏和受限 harness 补丁；Bot 内化业务知识/规则、经验及创建原生工作也是允许的持续能力，不限于配置调优。方法候选不得自行扩大企业管理权、治理硬限制或发布权，不得把内化名单当外部当前许可，或追溯降低既有原生任务/承诺及外部对象的验收条件。外部规则/对象的实际变更须走其授权 API，不能靠改记忆冒充已变更。低风险试验/晋升范围由企业管理员预先设置，不能靠成员聊天或模型自报扩大。

Bots 的方法候选生成、独立评测及批准分别使用受信角色。评测可包含原生研究/报告/提醒任务，按 Bots 自己的 task contract/verifier 检查质量、证据、来源、权限、重复效果及成本，不要求外部存在同名任务或 verifier。涉及外部对象的状态/规则则以其验收和可信 fixtures 为准；原生成功口径不能改写外部结果。Development、有限 Selection regression 和独立 Promotion audit 分开，反复查看的失败样本不能再冒充盲测。普通知识/经验写入和原生任务推进按 §4.4 处理，不逐条走此发布流程。

每个 proposal/promotion 固定 base/candidate digest、来源/输入水位、原生 task contract/verifier 及适用的外部验收引用、模型/评测版本、scope、总预算和批准者。Bots 记录语义/治理 verdict，Chronotope 记录实际 job、制品、隔离/成本和 expected/effective revision；共享稳定 promotion ID，不用 A 报告发布 B。合并补丁重评最终 digest。来源或批准失效后，拒绝旧候选生效。

Skill 从 episode 提炼后先进入候选；写入准入与复用分别检查适用/禁用条件、来源、工具版本、scope 和 fallback。方法经验不继承原任务的账号授权、私人数据或业务批准。跨 Bot/团队复用须同时满足源授权及企业治理，正式库与 candidate/scratch 分开。回滚治理 release 不回滚外部事实、已发生效果、失效屏障或业务义务。

## 6. 原生与外部请求的执行链及授权门

1. **产生工作**：Bot 可基于已授权知识/目标自主创建 BotTask/Plan/Schedule/BotCommitment，也可接收成员在外部应用中的请求或外部事件。涉及外部入口时，由 adapter/bridge 验证并记录源引用、按协议 ACK；原生工作不强制先发外部消息或建外部 task/calendar。
2. **治理与原生接纳**：Bots 按企业配置确定任务目的、原生契约、计划/依赖、预算、session 策略、固定执行身份及可选外部工作引用，持久保存自己的 ID/状态。频道成员身份和原生任务已接纳均不等于获得外部操作许可。
3. **适用的外部授权**：访问外部资源或执行外部动作时，按其合同核验源系统业务规则、对象权限及账号授权。支持预授权就使用可核对决议；只在 API 执行时校验则保留该门，不要求所有应用新增 PDP/proof/epoch。原生任务创建、本地计划和闹钟无需外部对象许可，但对所使用来源的权限/删除限制仍有效。
4. **Runtime 接纳/派发**：Chronotope 用稳定 command/operation ID、原 runtime binding 和当前治理/技术授权接纳执行请求；涉及外部系统的动作仍经上一步的适用外部授权门。请求被接纳或已发往 API 不证明外部动作获准/生效。child、等待和取消按 run/attempt/operation 寻址，完成结果可靠回投。
5. **产物、外部动作与回复**：原生产物/知识和任务进度由 Bots 记录，可使用 infra 通用存储；外部工具更新外部对象。回复由 AppBinding 的唯一 sender 发出，重验读取/披露、当前受众、治理及派发 epoch。工具“已接纳”不等于 provider 接受或已读。
6. **分别验收**：Bots 按原生 task/commitment 契约检查报告、研究、提醒或跟进是否完成；外部 issue/calendar/订单/合同按其源端状态/验收查询。内部完成、发送确认、外部成功分别记录，反馈进入知识/经验或方法候选，不改写各对象权威。

涉及外部资源的动作必须同时满足：**适用的外部许可 ∩ Bot 治理上限 ∩ Chronotope 执行约束**；原生任务/计划/调度的创建和推进不以存在外部对象或 PDP 为前置。外部门可在 API 执行时校验，不要求所有 provider 提前三份许可。任一适用门拒绝或必要保证无法核验，不能用另一门替代；能力不足明确限制，不以本地 allow stub 补齐外部权威。

外部应用返回未授权时，保存拒绝响应及实际可得的 decision/source revision；重规划、换工具/账号、派发 child 或新 attempt 都不能绕过该边界。后续动作须有外部新的有效许可或经外部权威确认的条件变化，并重新满足治理与执行门，不因模型换一种解释就重试同一被拒业务意图。

Bot 可以把原生任务置为 blocked/partial，或在契约允许的范围改用不访问被拒资源的替代工作；这不是授权绕过，也不能伪报原要求已满足。新建原生 Task/Plan 不重置先前未知外部 operation 或取消其对账责任。

### 6.1 Session、应用账号与 routine

成员 DM、共享频道、GitHub PR 或 Linear issue 都引用外部会话/对象。Bots 可配置 observation/command、可接纳主体、session 隔离、reply target 和审计保留；源应用的成员关系、源数据 ACL 与业务授权仍单独检查。首版 D3 仍限定外部 DM 中的请求者委派；这是 session 范围，不提供个人 Bot 或 Bots 内私聊产品。

DM 按外部参与者和 scope 建立 session index 与上下文隔离，仍绑定同一组织 ManagedBot。另一个成员的 DM、共享频道或管理员审计不能因 BotID 相同就读取该上下文；内化知识/经验、长期缓存和派生状态也持续按来源权限过滤，不派生个人 BotOwner。

个人连接器/成员委派的授权由外部账号权威提供，Bots 仅配置是否允许该模式及更严上限。私有读取与披露给目标受众分别核验；受控调用记录绑定 requester/execution identity、purpose、tool/action、canonical input digest、来源范围、受众及有效期限。外部 epoch/proof 只记录 provider 实际提供的内容；没有该能力时使用源端执行时校验等已验证合同，不把 Bots 自己的 epoch 冒充外部授权证明。最终 sender 核验实际 payload 或可验证披露条件；所需披露权无法确认时不发布。scope 从受信管理流程按需建立/引用，不由模型随意参数创建。DM 结果不自动进入共享知识。

BotSchedule/AutomationDefinition 属于企业/组织/团队，固定 execution identity、触发/SOP、输入 scope、预算和输出目标；Bot 可在已批准范围自主设置闹钟/调度，不要求先创建外部日历事件或逐次申请发布。created_by/维护人/当前请求者与所有权分开。唤醒、重试和 continuation 不继承最后发言者凭据；涉及已撤销委派时停止相关新派发，不回退其他账号。身份/受众变更重新满足适用授权，unknown 仍按原 owner 对账。

### 6.2 唯一 sender 与跨服务一致性

AppBinding 声明实际发送由 Bots-managed adapter 还是外部 bridge 承担，并固定 sender/receipt authority、查询能力、幂等及未知结果合同。Bots 不因管理 adapter 就拥有外部源消息或源对象状态；源应用仍是这些记录的权威，Bots 自己形成的知识资产另有内部版本。一个实际发送不允许两个 dispatcher 各自直接重试。

领域/治理事务与 Chronotope 执行事务分别提交，使用稳定 command/outbox、operation ID、查询和幂等恢复协调，不宣称跨库原子提交。若 infra 调用 sender 服务仅取得接纳 handle，其账本记录该 hop；provider receipt 由实际 sender 记录。pending/dispatched/confirmed/unknown 分开，明确未接纳的失败才按策略重试；unknown 先查询或人工对账，不把新 RPC ID 当新业务动作。

撤权也有多门生效回执：外部业务/数据权威更新授权状态，Bots 阻断自己掌控的接入/缓存/治理发布，infra 和唯一 sender 分别阻断后续新 claim。未确认门保持 pending；已派发在途效果继续对账，不能用 infra 回执声称远端动作已撤销。

## 7. 现有模块与历史数据的处置

| 当前模块/计划 | 三方边界下的处置 |
|---|---|
| run/session、journal、effect ledger、executor、blob、wait/signal、mailbox/child | 保留 Chronotope；只提供通用执行/证据/恢复机制 |
| agents/config/harness registry、runtime release | 保留技术注册与不可变 spec；企业 Bot 产品身份及治理 release 归 Bots |
| MemoryPolicy、知识/经验/skills、反思及发布计划 | Bots 拥有内化内容和方法的语义权威，可持久使用自身或 infra 通用存储；不覆盖外部源记录/规则/ACL |
| core/commitment、agent_commitments、decider | 按记录语义盘点；Bot 自身责任可复用为原生 BotCommitment，修正事件/投影、作用域、receipt/unknown 及验收缺口。外部合同/工单对象自身状态另保留源引用；Bot 承诺处理该工单可仍是原生责任，不一概迁出或禁用模块 |
| Goal/WorkItem/计划与完成判定 | 可适配 BotTask/Plan 的原生契约/状态及通用执行 task/claim；原生工作无需外部对象，外部 issue/calendar 等另用链接关联 |
| Evidence | 实际执行记录由 infra/技术 sender 保存；原生任务/承诺由 Bots 的契约/verifier 采信，外部对象由其权威验收 |
| apps/team-agent、原生成员 Web Chat/群聊计划 | 成员交互仍留外部，不建设聊天产品；Bots 控制台可管理原生知识、任务/计划/调度、承诺及治理审计 |

本轮只调整设计，未执行代码删除或清库。当前没有生产历史：实施可直接删除未选路径、改 schema/API，并全栈重建开发数据，不做历史分类迁移、双读/双写或兼容路由。首版原生责任放在 BotTask 字段中，无需先改完旧 Commitment/Goal 模块或建设通用 Record 平台。物理存储不决定语义 owner；同版本运行中的故障恢复仍必须保留数据卷，已经派发/unknown 的结果不能在一次恢复测试内靠清库掩盖。真正第二 provider/生产迁移出现前，不建设通用交接框架。

## 8. 三方交付与联合验收

| 交付面 | 范围 |
|---|---|
| Chronotope infra | 通用授权/凭据/效果/接纳/故障正确性；执行上下文、不可变 spec、邮箱/activation/child、资源隔离、lineage/barrier、预算与发布 CAS |
| Bingdi Bots 企业治理与原生能力 | 组织所有的 Bot；知识/经验/技能资产；原生 task/plan/schedule/自我承诺及验收；管理员配置、账号/路由/session policy、loop/SOP、治理 release 和受控改进 |
| 外部应用集成 | 成员聊天/评论/协作、系统自有 issue/calendar/订单/合同、源业务规则/ACL/状态/验收；通过 API/MCP/webhook 或 bridge 接入 |
| 联合验收 | 原生任务/知识自有权威，适用的外部许可、Bot 治理与 runtime 约束共同生效；跨渠道、故障恢复、sender/unknown、失效传播及治理发布一致 |

既有 G-C/G-D/I 系列场景按本表归属。Bots 原生知识/任务/承诺不是对外部系统主库的重复，也不意味着要建设成员聊天应用；Chronotope 的通用存储/执行不因承载这些对象就拥有其领域语义。

## 9. 最小集成合同

以下是待实施能力分类，不是当前已有端点，也不要求每个外部应用新增同名接口：

```text
外部应用：VerifyExternalSubject / QuerySourceRevision / AuthorizeBusinessAction
          QueryExternalWork / EvaluateBusinessOutcome / SubscribeSourceChanges
          ReadConversation / PostMessage（由具体 provider/bridge 合同定义）
Bots：    ConfigureManagedBot / PublishBotRelease / BindApplication
          ConfigureParticipationAndAutomation / QueryGovernanceAudit
          PutBotKnowledge / RecordBotExperience / AdmitBotSkill
          CreateBotTask / ReviseBotPlan / SetBotSchedule
          RecordOrReviseBotCommitment / EvaluateBotTask
Chronotope：RegisterRuntimeAgent / RegisterImmutableSpec / AcceptCommand
            SendToMailbox / SpawnChild / Wait / Cancel / QueryRun / ReadResult
            InvokeTool(operation_id, grant_ref, tool_ref, frozen_input)
            RunIsolatedJob / ApplyRuntimeReleaseBinding(expected_revision, spec_ref, approval_ref)
            Put/GetVersionedRecord / Blob / Artifact / ScopedSearch
            Put/GetDerivationManifest / InvalidateRecordRevision / QueryBarrierReceipts
            DescribeExecutionBoundary / QuerySnapshotRestoreReport
```

外部业务工具可以由 API、MCP 或独立 bridge 提供；Bots 管理绑定与 Bot 能力上限，不因托管工具端点就拥有其业务决策。Chronotope 不按工具名中的“承诺”“工单”产生隐式业务副作用。

AppBinding 声明各 provider 是否支持预授权、执行时权限校验、状态查询、epoch/撤销通知及 receipt。只提供执行时授权的应用可以按该模式接入；缺少预先决议不等于默认允许，也不强迫其引入独立 PDP。对无法验证的限制标注 unsupported/unknown，并据部署所需保证拒绝或限制能力。

## 10. 联合验收重点

| 场景 | 必须证明 |
|---|---|
| 员工创建 Bot、发 DM、离职或换管理员 | Bot 仍归企业/组织/团队；created_by/requester/maintainer 不成为个人 owner，成员私有委派撤销不被继承 |
| Bot 管理员打开成员 DM 或外部业务内容的审计引用 | 管理权限只提供受管运行元数据/hash/引用；内容展开/导出须有源应用授权，DM 上下文不因同 Bot 或管理员身份而串读 |
| 外部应用拒绝但 Bot 管理员已配置账号/工具 | 操作仍拒绝；Bots 不能通过 prompt、SOP 或自建业务白名单覆盖外部权限 |
| 外部应用未授权后模型重规划、换工具/账号或委派 child | 原拒绝可追溯，不通过换路径/身份绕过；外部授权/条件实际变化后仍按 provider 原有授权门重新核验，不伪造预先 allow |
| 外部应用允许但 Bot 治理/预算/隔离不允许 | 仍拒绝，三类批准不互相替代；admin 治理批准不是外部对象的业务验收 |
| 无外部 task/calendar 对象时自建研究、报告和提醒 | 原生 BotTask/Plan/Schedule 可持久创建、按自身契约推进和验收；无需先创建外部对象或逐条发 release |
| Bot 形成外部没有同名记录的新知识/经验 | 可持久存 Bots 或 infra 通用 store，语义 owner 均为 Bots；保留来源/证据、置信度、适用性和访问约束，不降格为仅缓存 |
| 外部规则/名单被记忆并用于任务推理 | 允许有来源/版本的持久内化；不作为外部当前许可，源变化/撤权按依赖策略处理，不禁止所有业务相关记忆或原生判断 |
| 两个渠道接续同一 Bot 的原生工作及关联外部对象 | BotTask/Plan/Commitment ID 与可选 BotWorkLink/ExternalWorkRef 独立可追溯；不建成员聊天主库，也不自动镜像外部状态 |
| runtime.completed、BotTask completed、外部 issue open、发送 unknown 并存 | 按各自契约分别展示；如 BotTask 只要求报告产物可独立完成，如要求发出提醒则 unknown 不能宣称送达；不得推进外部完成 |
| 原生 BotCommitment 逾期、task 取消或 release 回滚 | 原生责任、有效修订和证据保留；不自动释放、降低兑现条件，缺发送回执不自报履责 |
| 外部会议改期/取消或来源权限撤销 | 固定/跟随策略决定内部 alarm/task 更新，并记录原因；不机械复制外部状态，权限撤销先阻断新使用而非遗忘任务 |
| 同 Bot/不同 session 或 child 使用私有来源 | 显式实际资源 binding，workspace/volume/browser/credential 隔离可核对，persona 不作安全证明 |
| snapshot 落后、缺文件、与 journal 水位不一致或跨 infra 迁移 | 隔离危险续跑并报告覆盖/缺口；原 attempt/unknown 留原 owner，对账和交接后再续跑 |
| 来源撤权、账号解绑、治理 release 回滚并发 | 外部 source revision 与三方执行门保持可解释水位；不复活来源或权限，无法抹除的既有观察明确披露 |
| Bots-managed adapter 与外部 bridge 切换 | AppBinding 保持唯一技术 sender/receipt owner；排空/交接发送状态，不双派发，源消息权威不迁入 Bots |
| routine 中途收到另一人的消息 | 企业所有权、固定 execution identity/范围/预算/目标不变；不借用最后发言者凭据 |
| RSI 试图用内化名单绕过源端授权或降低外部验收标准 | 越权候选被拒；存储/推理外部规则本身允许，不能凭内部更新宣称源规则/许可或对象状态已改变 |
| 日常知识积累/原生任务推进与方法发布并行 | 普通写入按对象 owner/CAS 和现有策略执行，不逐条管理员审批；skill/SOP/harness/memory 方法变更走相应准入/晋升，不能借日常更新降低旧契约 |
| 治理发布与语义反馈 | 原生 task contract/verifier、适用的外部验收引用及实际制品/批准/effective receipt 对应；候选不能自授权、用 A 报告发布 B |

上述是设计与联合验收计划。Bots 尚无实现；外部集成、恢复、隔离和改进收益均须在实现后按各自 owner 的真实合同验证。
