# Morde DSH 任务模型响应持久化与恢复实施任务 TODO

日期：2026-11-11。来源：[设计方案](<../Morde DSH任务模型响应持久化与恢复设计方案.md>)、[需求文档](<../Morde DSH任务模型响应持久化与恢复需求文档.md>)。本清单按实现和验收结果回填状态。

## 修改边界与现状

- 修改范围限于 Morde `server/internal/app`、`server/internal/store/postgres`、`server/internal/worker`、版本化迁移及其测试/公共内部合同。继续由 Model Gateway 调用模型，Task 响应通过原 DSH IPC 与检查点恢复。
- 当前摘要模型结果有加密恢复路径，路由模型决策有独立持久化；Task 通用模型账本只保存用量与调用状态，未保存完整 `ModelResponse`。DSH 检查点 ACK 已是 Worker 接受后续工具调用的门槛，本次保留此围栏。
- `CloudDSHAccountRuntimeManager.Acquire` 目前持有 manager 级互斥锁直到 Runtime ready 握手完成，`renewIdleSessions` 也在 manager 锁内执行续租；这会串行化同一 Worker 上不同会话的冷启动/续租。现有 `max_concurrent_tasks` 限制在单个会话 Runtime 内仍须保留。
- 不修改 DSH 上游源码、Agent Loop、IPC 帧格式、公开 HTTP API 或客户端；若验收发现 Morde 自有 Worker 适配无法复用保存结果，只在 Morde 适配层做最小修正，并记录原因。
- 实施代次规则：响应记录绑定原调用代次；重启后只有在旧执行者已确认停止、当前 Agent 租约有效、同一 session/checkpoint 前缀及授权版本仍有效时，才允许合法后继代次恢复。需求文档中的“代次匹配”需按此定义同步，不能要求新旧 generation 数字严格相同，否则跨重启恢复无法成立。
- 模型提供商请求始终在数据库事务外。Provider 已处理但 Morde 未能持久化完整响应时，不盲目重发，也不承诺外部 API 只计费一次。

## 关键接口与数据变化

| 接口或数据 | 目标变化及调用方 |
| --- | --- |
| `TaskModelResponseStore`（拟新增，app） | 以当前 Task 调用围栏、model ID、稳定 call ID 和 request digest 查询已结算的加密响应；返回原 provider/连接版本及响应，供 Model Gateway 校验并恢复。没有结果、身份不符或旧调用无响应时不得把它当成新调用。 |
| `ModelCallSettlement` / `SettleProjectModelCallCommand`（现有，扩展） | 增加仅 `source='task'` 可用的加密响应密文与摘要。账号和项目结算实现须将响应保存、用量结算及相应账本更新放在同一事务；Compaction 原有字段和行为不变。 |
| `task_model_call_responses`（拟新增 PostgreSQL 表） | 与 Task 模型账本调用一对一关联，保存有界密文、响应摘要、Task/session/checkpoint/原始 generation/授权和 provider 连接元数据；`model_call_ledger` 仍是唯一用量账本。 |
| `ModelGatewayService.Complete`（现有，扩展） | Task 调用先验证调用身份并查询保存结果。命中时重验当前 Task/授权与原连接配置后返回原响应，不预留、不结算、不调 provider；未命中时只有无旧派发记录才走现有 reserve/dispatch。 |
| DSH Worker ↔ Runtime IPC | 不变。完整响应持久化确认后，网关才把响应交给 Worker；Worker 仍使用现有完整响应帧与 DSH 检查点 ACK 流程。 |
| `CloudDSHAccountRuntimeManager.Acquire`（现有，锁范围调整） | 公开接口不变。将全局 manager 锁限于会话映射操作，拟增加按 owner/conversation 的启动协调；同一会话竞争 Acquire 复用同一 Runtime，不同会话冷启动互不阻塞。 |

## 实施任务

- [x] **T01（已完成，2026-11-11）统一恢复代次合同并定义 Task 响应密封合同**
  - 修改：本需求文档、[设计方案](<../Morde DSH任务模型响应持久化与恢复设计方案.md>)、`server/internal/app/model_gateway.go`，必要时新增 `server/internal/app/model_task_response.go`；同步 [公共约定](00-总览与公共约定.md) 中对应的跨模块接口。明确合法后继 generation 只可恢复相同 Task/session/checkpoint/request/auth 围栏下、绑定原代次的响应。
  - 定义受限的 Task 完整响应类型，覆盖文本、推理、stop reason、工具调用及用量；新增密封/解封与 digest 校验，复用 Morde 现有加密密钥管理和 `ValidDSHModelResponse` 限额。密文关联数据绑定 owner、task、call ID、request digest、model/provider、session、checkpoint 序号及授权版本；不保存 prompt，不记录明文日志。
  - 新增 `TaskModelResponseStore.RecoverTaskModelResponse` 读取合同，并扩展 `ModelCallSettlement` 与项目结算命令的 Task 响应字段；对非 Task 来源、未知用量、无效输出和缺字段输入做严格拒绝。
  - 验收：文档对 generation 变化的规则一致；密封往返保留全部字段，篡改、身份错配、超限响应及 source 不匹配均拒绝；Compaction 既有密封/恢复用例通过。

- [x] **T02（已完成，2026-11-11，依赖 T01）持久化响应并与账号/项目结算原子提交**
  - 修改：新增 PostgreSQL migration `00182_task_model_call_responses.sql`、`server/internal/store/postgres/model_usage.go`、`project_budget.go` 及响应 Store 文件。建立仅用于 Task 响应正文的 `task_model_call_responses`，账本记录继续是唯一用量事实来源；迁移顺序在本地 PostgreSQL 16.15 集成环境验证。
  - `SettleModelCall` 和 `SettleProjectModelCall` 在同一事务中保存密文响应、settled token usage、项目实际费用及 Task 用量；未知或无效响应只结算已有可确认用量，不保存为可恢复成功结果。重复 settlement 必须校验身份、digest 和已有内容，不能覆盖不同结果。
  - 恢复查询核对调用账本已 settled、Task 和 request digest 一致、响应摘要/密文有效、provider connection 元数据与原调用一致。旧账本存在但无响应正文时返回不可恢复/待对账，不重新派发。响应保留至少覆盖 Task 可恢复期，沿用已有终态数据保留规则。
  - 验收：隔离 PostgreSQL 覆盖账号模型与项目模型结算、成功/未知/无效响应、事务回滚无部分写入、重复/冲突 settlement、账本行与响应缺失或损坏、旧 settled 调用无响应；确认 migration 可升级且不会改变既有账本数据。

- [x] **T03（已完成，2026-11-11，依赖 T01–T02）接入网关恢复优先与安全失败路径**
  - 修改：`server/internal/app/model_gateway.go`、`model_gateway_settlement.go` 及其 app 测试。Task 调用完成校验后先查保存响应；命中时重验 Task 租约、当前 authorization/task revision、同一 session/checkpoint 前缀和原模型连接版本，再返回恢复结果。此路径不得再次 reserve、settle 或调用模型。
  - 仅当确认同一 call ID 没有旧派发记录时才 resolve provider、预留并调用。已有 dispatched/unknown 或 settled-but-no-response 调用沿用未知结果/对账路径，不换 call ID 重发。新 provider 响应通过现有响应与工具调用验证、交付围栏后，带密文进入 settlement；持久化确认前不向 Worker 返回响应。
  - 保留请求调用在数据库事务外、取消与超时行为、已知用量和未知用量规则。保存事务结果不确定时按同一 call ID 查询并校验，不能因重试造成重复结算或提前交付。
  - 验收：app 测试证明缓存命中 provider 调用数、预留数及 settlement 数均不增加；新调用只执行一次；request/model/call/task fence 不匹配、权限撤销、连接版本变化、损坏密文、未知响应及保存失败均不返回成功响应或工具调用。

- [x] **T04（已完成，2026-11-11，依赖 T03）验证 Morde Worker 接续与流式输出不重复**
  - 修改：优先只扩展 `server/internal/worker/cloud_dsh_session_tree.go`、`cloud_dsh_text_stream.go` 的定向测试；只有现有行为无法接续时才修改 Morde 自有适配。保持 `runtime/morde-cloud-runtime`、DSH 上游源码和 IPC 合同不变。
  - 验证同 Task/session/checkpoint 前缀在合法后继 generation 下仍生成相同调用 ID；恢复响应经 `writeCloudDSHModel` 原路径交付，DSH assistant 结果必须先 checkpoint ACK，Worker 才接受工具调用。缓存恢复不重发 `OnTextDelta`；已有部分流按现有中断/完成语义收口。
  - 验收：重启 Worker/Account Runtime 后恢复完整响应，无 provider 二次调用、重复可见增量或工具副作用；工具调用发生前 checkpoint 已确认；generation、session、授权版本或 checkpoint 前缀变化时拒绝复用。

- [x] **T05（已完成，2026-11-11）解除无关会话冷启动串行化并验证 Runtime 并发**
  - 修改：`server/internal/worker/cloud_dsh_account_runtime.go` 及其测试。将 `Acquire` 的 manager 级互斥锁缩小为 map 操作；通过按 owner/conversation 的 single-flight 或等价启动协调保持同一会话只有一个 Runtime，同时允许其他会话并行执行状态读取、supervisor 启动和 ready 握手。避免锁住 map 后执行外部 IO。
  - 保留现有 owner 校验、租约/停止证明、同会话 `startMu`、`max_concurrent_tasks` 和 Runtime 容量拒绝语义；一个会话启动失败不能污染其他用户或会话的 manager 状态。不同用户相同 conversation ID 的异常输入仍须拒绝。
  - 验收：阻塞一个会话的 Runtime 启动或续租时，另一个用户/会话可以完成启动；同一会话并发 Acquire 只启动一个 Runtime 并返回同一活动实例；多会话并发 Task 在配置上限内重叠运行，超上限时不超配；竞争失败、启动超时、取消及 Runtime 续租/停止证明的既有测试通过。

- [x] **T06（已完成，2026-11-11，依赖 T01–T05）完成真实恢复故障注入与并发隔离验收**
  - 修改：相关 app、PostgreSQL 与 Worker/Node Account Runtime 集成测试，必要时只更新公共接口合同；不新增客户端/API 改动。用合成模型和隔离 PostgreSQL 注入“provider 返回后/响应提交前、响应提交后/Worker 交付前、DSH checkpoint ACK 前”中断。
  - 并发验收至少包含两个用户、每个用户两个 Conversation，以及同一 Conversation 内多个 Task；不同用户/会话的 checkpoint、模型响应、用量、工具结果和取消互不串线，单会话容量按配置生效。同一 call ID 并发请求最多一个 provider 派发；其他请求不能重复派发，结果提交后重试读取同一响应且不重复结算；未完成旧调用仍走未知/对账。
  - 故障验收：保存成功时恢复逐字段一致、模型调用数不增、工具仅在检查点确认后且只执行一次；无响应/未知/授权变化时安全阻止交付并保留对账状态。回归个人与项目预算、压缩摘要/路由恢复、取消、权限撤销、Task 检查点和部分文本流。
  - 验收结果：串行 `make test race cover` 通过（内部总覆盖率 45.1%），`make generate` 通过；Node DSH Runtime 全量测试 103 项通过。Account Runtime 的两用户四会话并发、同会话复用和跨会话启动/续租/停止恢复隔离测试通过，Worker race 测试通过。PostgreSQL Task 响应保存/恢复、结算回滚、未知/无效响应、同 `call ID` 并发单赢家集成用例在普通与 race 模式通过；此前 `make it` 的 13 个失败用例由新增密封器要求、响应重放语义和本地 migration head 183 的旧测试预期造成，修正后 13 项定向复测全部通过，整条 `make it` 未在夹具修正后再次重跑。
  - lint 记录：全工作树 `make lint` 被未跟踪的 Task File Fetch 集成测试语法错误阻断；排除此文件后，独立 Task File Fetch 改动还报告 QF1001。隔离副本排除该文件并还原该独立改动的基线后，目标变更 lint 为 0 issues。原文件及其相关修改保持未改动。
  - 完成核对：`git diff --check` 和目标 Go 文件 `gofmt -d` 均无差异；工作树其他并行改动与未跟踪文件未清理或回退。

## 执行顺序与状态规则

先完成 T01 的跨代次恢复合同和密封接口，再由 T02 建立加密响应存储与原子结算；T03 接通网关读取与失败围栏，T04 验证 Worker/DSH 现有恢复链路，T05 保证不同会话 Runtime 可并行冷启动，最后用 T06 做真实中断与多用户并发验收。只有实现及该项验收全部完成后才标记 `[x]`；文档、接口或测试尚待核实时使用“待验证”，受外部条件阻塞时写明条件，不以代码合并或单元测试通过替代端到端验收。
