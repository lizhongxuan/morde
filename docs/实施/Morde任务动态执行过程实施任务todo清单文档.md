# Morde 任务动态执行过程实施任务 TODO

> 来源：[Morde 任务动态执行过程设计方案](../Morde任务动态执行过程设计方案.md)、[任务动态需求](../Morde任务动态需求文档.md)。本文按实际开发和验收结果更新状态。

## 范围和依赖

- 以现有 Task、`task_steps`、`task_agent_sessions`、`task_events` 和各工具的操作账本为依据，接通主／子 Agent 工作过程、工具详情及 PC、iOS 展示。任务状态、审批、停止、恢复和结果交付继续使用现有机制。
- 重复消息是否属于同一任务由会话路由决定，依赖[对话与任务执行体验优化清单](<Morde AI助手对话与任务执行体验优化实施任务todo清单文档.md>)中的消息关联和 `join` 工作；本清单不隐藏真实存在的重复 Task，也不自行实现另一套路由。不同会话的任务按当前授权和已持久化的归属展示，不凭内容相似跨会话合并。
- 仅复用当前锁定的 DSH Runtime、会话与工具能力；`presentCall`／`presentResult` 仍需实际接线和服务端校验。现有依赖版本为 `0.2.2-rc.1`，本方案不要求升级或引入第二套 Runtime。
- T01–T04 的公共约定、OpenAPI、数据库迁移、服务端实现与对应测试作为同一组联调变更保持一致；任务顺序用于拆分工作，不代表可以单独发布不完整合同。T05、T06 在接口稳定后可分别推进，最终由 T07 联验。修改已有人在工作的文件前保留并整合现有变更。
- 状态使用“未开始、进行中、待验证、完成、阻塞”；只有实现及该项验收均完成才勾选。缺少真实运行环境或外部依赖时记录待验证，不把跳过的检查当作通过。

## 问题记录

1. **详情正文存储已定案。** 采用 `task_step_details` 一对一存储经白名单投影的正文密文；每项最多 64 KiB，AES-GCM AAD 绑定 owner、Task、step、Task revision，密文 SHA-256 一并校验；仅按需 API 返回，删除时级联清理，SSE／日志不含详情正文。该受控例外及生命周期已登记于[公共约定](00-总览与公共约定.md)第 7 节。
2. **重复消息归并是外部依赖。** 上述对话优化清单规划了 `join` 和消息关联，当前不能据此认定它们已经完成。T07 需检查真实服务结果；若该能力未完成，仅将重复请求的一任务验收标为待验证，继续验证本功能其他部分。

## 接口和数据变化

| 接口／位置 | 实施变化与约束 |
|---|---|
| `GET /api/conversations/{id}/tasks`、`GET /api/projects/{id}/conversations/{conversation_id}/tasks` | 沿用当前会话的 active/recent、授权和分页合同；列表消费任务摘要，不逐卡读取步骤详情。项目范围不得借个人会话入口绕过校验。 |
| `GET /api/tasks/{id}` | 在现有 `TaskStep` 中增加可选执行者引用、任务修订、可确认的执行阶段和详情可用性；增加执行者及父子关系投影。Task、步骤、关系和 `event_sequence` 在同一数据库读快照内获取。既有 `steps`、`child_jobs` 和其他字段语义保持不变。 |
| `GET /api/tasks/{id}/steps/{step_id}`，**拟新增** | 返回 Task／步骤身份、同一快照的事件序号、可展示的工具和对象、时间、输入及结果分段，以及未记录、过期、受限、截断等状态。每次校验身份、任务读取权限、会话／项目及具体资源权限；任务或步骤不存在、无权访问时沿用隐藏资源的 404 语义，部分资源受限时不得泄露其内容。响应禁止共享缓存。 |
| `GET /api/tasks/{id}/events` | 沿用 SSE 游标及 `task.step`、`task.operation`、`task.snapshot_required` 等既有事件，仅传安全标识和更新通知，不传原始工具参数、结果或二进制。客户端从快照的序号订阅，遇到缺口或要求重取快照时恢复。 |
| `task_steps`、`task_agent_sessions` 和可选步骤详情 | 迁移增加可空的会话、修订、稳定调用关联和冲突校验元数据；会话增加受围栏保护的最近状态观察；详情与步骤一对一，正文按问题记录定案后的受保护存储保存。历史数据不清空，无法证实归属时保持未知。 |
| DSH Runtime → Worker 状态帧，**拟新增内部协议** | 传递根会话 `agent/status` 的 `idle`／`running` 观察及任务、会话、代次、授权围栏；实施时明确字段和单调次序，服务端记录观察时间。旧代次、失效租约／授权或乱序状态不得覆盖新观察。观察值只用于展示，不改变任务状态。 |
| Worker → CloudDSH 工具代理（内部） | 传递经校验的执行会话、首次受理修订、规范化调用身份及原始调用摘要；现有授权、审批、操作账本和恢复规则继续生效。MCP 别名转换前后保持同一调用身份。 |

## 实施任务

- [x] **T01（完成）统一数据、接口合同及迁移**
  - 修改：`docs/实施/00-总览与公共约定.md`、`server/openapi/portal.yaml`、`server/migrations/`、`server/internal/domain/task/`、`server/internal/app/task_store.go` 和 `server/internal/store/postgres/task_execution.go`、`agent_sessions.go` 及相关测试。按问题记录确定受保护详情正文的实际存储和删除方式，明确字段、大小上限、可用性、授权、事件及错误合同；接口中的新字段均为可选或与现有客户端兼容。
  - 在 `task_steps` 记录执行会话、首次受理修订、调用关联及请求摘要，以 Task、会话和规范化调用身份建立唯一约束；执行会话关联实际的 `task_agent_sessions`。步骤序号在持有 Task 行锁的事务中分配。状态观察记录代次、顺序和时间，详情与步骤一对一；数据库和专用存储各自保存的内容遵循定案后的公共约定。
  - 验收：隔离 PostgreSQL 空库与已有数据均可迁移；历史步骤保留，无法唯一证明的归属和详情不猜测。升级时仍在运行的旧调用仅在操作键及步骤事件能唯一对应时关联原步骤；否则保留记录并标明信息不足，不通过重放制造新操作。并发不同 Agent 执行会话的相同调用 ID 不冲突；同执行会话同调用重放复用，工具或参数摘要不同则冲突。验证唯一约束、事务回滚和任务删除时元数据清理；正文清理由 T04 验收。
  - 验收证据：隔离 PostgreSQL 测试覆盖 1.93→1.94 有数据迁移、旧步骤往返迁移后仍保留且不猜测 Agent 归属、回滚不会丢弃新增身份；同一调用在主／两个子 Agent 会话中分别建立步骤，同会话输入冲突未留下步骤，任务删除级联清理密文。旧调用 ID 无法从既有哈希操作键反推时保留为 `not_recorded`。

- [x] **T02（完成，依赖 T01）接通 DSH 会话状态观察**
  - 修改：`runtime/morde-cloud-runtime/src/engine.mjs`、`account-runtime.mjs`、`ipc.mjs` 及 `test/`，`server/internal/worker/cloud_dsh_executor.go`、`cloud_dsh_session_tree.go` 和对应测试。复用 DSH 现有 `agent/status`，在根会话创建和状态变化时通过现有私有 IPC 发送观察帧，并同步严格帧解析、契约与测试 fixture。
  - Worker 按 Task、owner、Agent 实例／代次、会话、租约和授权围栏验证；以可比较的代次和每会话单调顺序防止迟到帧覆盖新值，将观察与现有任务事件在同一事务中提交。进程退出、租约或授权失效后，不再把旧观察当作实时状态；无有效观察时返回状态待确认。
  - 验收：真实 DSH 产生的 `running`、`idle` 可被观察；乱序、旧代次、错误会话、失效租约和授权帧被拒绝或忽略。`idle` 不会使 Task 完成，也不会凭空显示正在等待子 Agent；重启后旧状态不冒充当前状态。
  - 验证证据：Runtime 全量测试（106/106）及 Worker 全量测试通过；本轮隔离 PostgreSQL 上任务活动与详情相关的 4 个定向集成测试通过，其中状态观察用例覆盖递增、重复／乱序、旧代次、错误会话、旧授权、租约过期及过期后投影为 `unknown`。Worker 的终态仍由既有任务完成协议决定。

- [ ] **T03（待验证，依赖 T01、T02）将真实调用关联到同一条动作**
  - 修改：`server/internal/worker/cloud_dsh_session_tree.go`、`server/internal/app/model_client.go`、`cloud_dsh_tool_broker.go`、相关 `cloud_dsh_*` 工具分支、`task_execution_helpers.go`、`server/internal/store/postgres/task_execution.go`、`agent_sessions.go` 及相应测试。沿现有 `ModelToolCall.TaskFence` 传递执行会话、修订和调用身份，复用 `AgentOperationKey` 与子会话的调用 ID 命名空间；对 MCP 使用别名改写前已验证的原始调用摘要。覆盖 Workspace、搜索、命令、电脑、MCP、技能、其他现有代理分支和子 Agent 委派，避免只接通某一种工具。
  - `tool/call` 仅形成等待／准备中的动作；完成必要的步骤关联、可展示资料和更新事件的持久提交后才允许派发。实际派发后才显示进行中，成功、失败或未知以代理的结构化结果及各自操作账本为准；审批拒绝或输入校验失败标明未派发。没有操作账本的直接只读分支，也须先持久关联可验证结果，才能显示终态。
  - 恢复同一调用更新原步骤；已入账结果按相同操作键修复投影，未知外部结果走既有对账而不重发。结果已确认但展示资料写入失败时保留账本结果并显示详情暂不可用。旧修订迟到结果只允许更新围栏允许的原步骤，不改变当前修订。子会话创建、子任务完成和最终结果交付分别依据各自记录；主协调动作仅在消息与 Task 的归属可证明时展示。
  - 验收：并行主／子 Agent、同执行会话重放、不同 Agent 执行会话同调用 ID、同 ID 不同输入、MCP 别名转换均得到正确动作及冲突结果；审批拒绝、失败、未知、停止和旧修订迟到均显示真实状态。分别在派发前、派发后结果未知、已入账但步骤未更新时中断恢复，核对实际执行次数和步骤数量，不重复执行、不伪报成功。

- [ ] **T04（待验证，依赖 T01、T03）实现安全投影和步骤详情接口**
  - 修改：`server/internal/api/tasks.go`、`api.go` 的路由及客户端权限白名单、`server/internal/app/` 中工具展示与任务查询、`server/internal/store/postgres/task_execution.go`、定案后的详情存储、`server/openapi/portal.yaml` 及其测试。扩展现有 Task 快照，在同一读快照中投影历史执行者关系、步骤和事件序号；新增按需读取的步骤详情接口，不改变现有 `child_jobs` 含义。
  - 接通或适配已安装 DSH 的展示语义，再由服务端按工具白名单校验和脱敏。文件、搜索／网页、命令、电脑和 MCP 分别给出设计允许的安全字段；其他工具只显示安全名称、状态和详情不可用。列表标题与摘要也必须按资源权限处理；不直接展示原始参数、输出、密钥、模型内部推理或检查点，文本大小受限并标明截断，链接沿用现有安全策略。
  - 详情正文使用现有密钥管理和认证加密，绑定 owner、Task、步骤及修订并校验摘要；保留、删除、过期和源资源撤权遵循设计。检查当前任务、会话／项目、文件／电脑／连接器等权限；详情接口禁止共享缓存，SSE 不发送详情正文。专用存储与数据库不能同事务提交时，先持久化并核验正文，再原子提交步骤引用和事件；失败或中断时不得派发未记录的操作，孤儿内容须安全清理，已确认操作不因展示失败重做。
  - 验收：实际调用文件、搜索和命令，详情与可核验结果一致，空结果、未记录、过期、受限及截断可区分；资源撤权、跨账号／项目访问、混用 Task 和 step ID 均不泄露信息。覆盖恶意 HTML、敏感信息、失效链接、篡改密文、存储写入失败、任务删除后的正文清理及重启恢复；旧客户端仍可读取既有字段，事件和日志不包含原始详情。

- [ ] **T05（待验证，依赖 T04）改造 PC 任务动态**
  - 修改：`web/portal/src/routes/Assistant.tsx`、相关组件／样式与 `Assistant.test.tsx`、API 调用及 mock；按 OpenAPI 运行 `npm run generate:api` 更新 `web/portal/src/api/schema.d.ts`。复用现有侧栏和审批、暂停、继续、停止、成果入口。
  - 当前会话列表按“待你处理、进行中、已暂停、最近结束”分组，空组不显示，同组按更新时间排序，最近结束默认折叠；卡片仅展示真实摘要，列表读取不完整时提示状态不可用。任务详情按可证明的主／子关系、委派顺序和步骤顺序展示，可并行显示多个进行中动作；未知归属单独展示。打开动作时才请求详情，显示真实状态、工具、对象、时间、输入和结果的可用部分；保留结果整理状态和实际消息链接。
  - 快照从 `event_sequence` 订阅，重复事件忽略，缺口、`task.snapshot_required` 或断线恢复重新取快照；较旧的快照与详情不得覆盖较新状态。现有最多四条 `TaskStream` 时优先订阅打开的任务，其他活动任务使用实际列表刷新补偿，避免永久陈旧。切换会话／账号、登出或授权失效时清除相应详情并丢弃迟到响应；长列表分段渲染，事件更新不强制滚动或关闭详情，支持键盘和屏幕阅读器；窄屏面板可覆盖聊天历史，但不压住输入框。
  - 验收：运行 Portal 相关测试、类型检查及构建；在 Codex 内置浏览器连接本机隔离服务，验证分组、主／子动作、详情、审批、超过四个活动任务、断线恢复、切换会话及授权撤销。刷新／打开面板不创建任务或重复执行工具。

- [ ] **T06（待验证，依赖 T04）改造 iOS 任务动态**
  - 修改：`clients/nanoMuse/android/src/ios/NanoMuse/MIFTaskViews.swift`、`Morde/MordeApp.swift` 中实际使用的任务模型／请求和事件处理、`Localizable.xcstrings`、`Morde/Tests/MordeTaskEventIsolationTests.swift`、`MinisTests/MIFTaskViewsTests.swift` 等相关文件。复用现有任务页面及操作入口，消费与 PC 相同的任务投影和按需详情接口。
  - 实现与 T05 一致的当前会话分组、真实执行者关系、动作和结果整理状态；详情使用下一级页面或弹层，返回保留分组和滚动位置。按照快照／事件序号恢复、处理缺口及迟到响应；退出、切换账号／会话或撤权后清除受影响的详情，支持原生无障碍与本地化。
  - 验收：先核对 `Morde.xcodeproj` 的 `Morde` scheme、测试 target 和模拟器，再执行相关单元及 UI 测试并确认实际运行数量大于零；在 iOS 模拟器或设备连接本机隔离服务，验证主／子动作、详情、返回、断线重连、账号切换及撤权。未具备模拟器或设备时记录待验证。

- [ ] **T07（待验证，依赖 T02–T06）真实链路与跨端验收**
  - 修改／补充与上述链路对应的 `runtime/morde-cloud-runtime/test/`、`server/internal/worker/` 和 `server/internal/store/postgres/` 集成测试及 PC／iOS 验收记录；复用现有真实 DSH、隔离 PostgreSQL 和本机测试服务，不用纯前端 mock 代替端到端验证。
  - 在隔离环境由主 Agent 实际委派至少两个互不冲突、并行工作的子 Agent，分别调用工具并回传真实结果；PC 和 iOS 使用同一账号查看同一会话／Task，核对文件读取、搜索、命令的状态与可展示详情、主／子层级、最终回复和消息链接。模型和搜索服务可使用本机测试桩，但必须经过真实 DSH 和 Morde 工具代理；测试文件只在本地隔离环境使用，不外发。分别验证无工具纯对话、不启动电脑的任务、真实等待审批、部分失败、停止指定任务，以及账号、项目和会话隔离。
  - 验收并发与恢复：同 Agent 执行会话的同调用恢复一条、不同 Agent 执行会话的相同调用 ID 两条、输入冲突不合并；覆盖三处中断窗口、事件重放／缺口／过期、超过四个活动任务、权限撤销和客户端重连。确认刷新只读、旧版本历史记录不被抹除，实际工具调用次数、步骤、操作账本与界面一致。
  - 对重复发送场景先验证对话优化清单的关联能力：服务端只创建一个实际 Task，消息均持久保留，两端任务动态只呈现该 Task；若关联尚未实现，将该项记为待验证并保留实际任务，不由前端合并。执行 `npm --prefix runtime/morde-cloud-runtime test`、`make -C server test race it`、`npm --prefix web/portal test`、`npm --prefix web/portal run typecheck`、`npm --prefix web/portal run build` 和 iOS 测试；记录实际命令、环境、测试数量和未执行项。缺少 `CH_TEST_DATABASE_URL` 等导致跳过时不得记为通过。

## 本轮执行与验证记录（2026-11-13）

### T01–T04：Runtime、存储和 API

- 已接通 Runtime 根 Agent 状态观察帧、Worker 围栏校验与服务端状态持久化；工具调用按 Agent 会话、任务修订、调用身份和输入摘要关联到既有步骤。
- 已增加任务步骤的执行者和详情状态投影、按需步骤详情 API、AES-GCM 密文存储与身份 AAD 校验；受限 Workspace 的步骤信息会隐藏并返回 `restricted`。数据库迁移新增为 `00194_task_execution_activity.sql`，保留现有 `00193` 对话路由迁移。
- 验证：`npm --prefix runtime/morde-cloud-runtime test` 全量通过（106/106）；`make -C server test` 与 `make -C server race` 通过。Runtime 全量测试在并行构建时曾出现一次恢复超时，隔离重跑后通过。
- 在本机回环地址 `127.0.0.1:55432` 的隔离 PostgreSQL 16.15 上，`server/internal/store/postgres` 全包曾单独通过（740.927 秒）；任务 Agent 状态顺序／围栏、旧 schema 升级、工具调用持久化及详情密文清理的定向测试通过。新增 `TestPostgresTaskToolStepPersistsFencedDetailAndCascades` 通过。真实 Node Account Runtime、步骤持久化重试、MCP 图片结果、Schedule 工具及长会话重启等 Worker 集成用例通过，Worker 包全量通过（约 220 秒）。
- 更早一次 `make -C server it` 全量尝试在 900 秒命令超时内未结束，不能记为全量通过；早前 PG 包全量结果及本轮定向测试作为独立证据保留。期间修复了旧步骤 SQL `NULL` 摘要转换、Node 测试监督器不接受 `agent_status` 帧，以及长会话集成夹具时间与候选选择规则不匹配的问题。`CH_OSB_URL`、`CH_OSB_API_KEY` 未配置，OpenSandbox 集成项跳过。
- 2026-11-13 隔离集成复验：在单独新建、仅监听 `127.0.0.1` 的 PostgreSQL 16.15 测试集群运行 `CH_TEST_DATABASE_URL=… make -C server it`。首轮 API、app、领域及 Worker 包通过（Worker 332.302 秒）；PostgreSQL 包在 600.904 秒到达 `go test` 默认 10 分钟单包上限，最后栈停在 `TestRouteRecoveryReleasesExpiredUndispatchedReservation` 创建临时库并执行 Goose 迁移。随后使用另一隔离集群单独运行该测试，通过（PostgreSQL 包 4.852 秒），未观察到该用例断言失败。任务动态相关的 `TestPostgresTaskToolStepPersistsFencedDetailAndCascades`、`TestPostgresTaskAgentActivityIsOrderedAndFenced`、`TestTaskStepDetailHTTPReadsAuthorizedEncryptedPostgresRecord`、`TestTaskExecutionActivityMigrationPreservesUnattributedHistory` 定向集成组通过（15.390 秒）。针对测试上限修正 `server/Makefile` 的 `it` 目标，先以 30 分钟单包上限重跑完整 `make it`，所有 Go 包通过：PostgreSQL 1721.770 秒、Worker 548.599 秒，其他包通过或无测试文件；该次全量 PostgreSQL 距超时仅余约 78 秒，因此将目标上限调至 45 分钟以留出负载余量（最终命令由 `make -n it` 确认）。覆盖率测试进程同期运行了一部分时间，不据此推断其为失败原因。所有本轮 PostgreSQL 用例均使用新建隔离集群，未连接或修改已有本地实例。OpenSandbox 因未配置 `CH_OSB_URL`、`CH_OSB_API_KEY` 跳过。
- 新增/复核验证：`go test ./internal/app -count=1` 和 `go test ./internal/api -count=1` 全包通过；步骤详情身份、权限、截断及路由 no-store/拒绝 query/body 定向测试通过。新增 `TestTaskStepDetailHTTPReadsAuthorizedEncryptedPostgresRecord` 在隔离 PostgreSQL 上通过，贯通真实 API 路由、应用授权、加密详情存储与响应解密校验。测试曾发现详情 handler 从错误的路由参数读取 step ID，已改为读取 `:step_id` 并复验通过。搜索详情现展示通过现有校验的查询词和时间范围；凭据样式及多行查询不显示，应用测试覆盖加密投影。详情持久化测试覆盖调用围栏、密文身份校验和级联清理。
- T01、T02 已完成合同及定向验收。T03、T04 维持“待验证”：核心实现、任务动态相关 PostgreSQL 定向集成组及服务端 `make it` 全包均已通过；仍未覆盖设计中的全部恢复窗口、资源撤权与真实主／子 Agent 工具链组合。
- 2026-11-13 服务端复验：`make lint` 为 0 issues；`make test` 全包通过；`make race` 首次在并行负载下触发 `TestCloudDSHExecutorCancellationDuringModelCall` 的 3 秒启动超时，独立复跑该测试及 `internal/worker` 全包 race 均通过，随后完整 `make race` 复跑通过；`make cover` 通过（全包 45.1%）；`make generate` 通过。该次普通／race／coverage 命令未配置 `CH_TEST_DATABASE_URL`，PostgreSQL 集成测试按约定跳过；隔离数据库 `make it` 全量通过证据见上文，不以本次跳过替代。
- 修复 PC 准备态显示：持久化工具动作时将 `phase=prepare` 写入步骤快照；应用层测试 `TestCloudDSHToolActivityPersistsPreparePhaseAndReusesStepOnReplay` 确认准备态显示为“准备中”，同调用重放沿用原步骤 ID 和顺序，终态事件指向同一动作。`go test ./internal/app -count=1` 全包通过（20.404 秒）。
- 补充授权与 MCP 动态回归：`TestTaskServiceStepDetailHidesWorkspaceDataAfterProjectAccessRevocation` 确认项目访问撤销后详情为 `restricted`、不读取密文且 Task 摘要受限；`TestSelectedMCPActivityUsesResolvedToolAndReusesCallIdentity` 确认 MCP 外层调用改写为实际工具别名后，步骤仍使用原调用 ID，审批重复等待及恢复复用原步骤。最新 `go test ./internal/app -count=1` 全包通过（20.407 秒）；`go test ./internal/api -count=1`（2.201 秒）及 `git diff --check` 通过。上述定向覆盖不替代 T03/T04 全部恢复、撤权和安全矩阵，状态继续为“待验证”。
- MCP 工具的任务动态标题现使用连接器已验证目录中的远端工具名；审批等待、重复等待、执行、拒绝和恢复沿用该标题与原动作 ID。内部别名继续作为 `tool_name` 与操作摘要，供账本和幂等校验使用。详情对象显示已授权连接器名称，历史哈希别名在 PC／iOS 展示为通用 MCP 标签，不暴露内部哈希。`TestSelectedMCPActivityUsesResolvedToolAndReusesCallIdentity`、`TestTaskToolCallProgressUsesMCPConnectorLabelAndOmitsAlias`、Assistant 定向 UI 测试、iOS `MordeTaskStepPresentation` 测试覆盖展示及重放身份。

### T05：PC 任务动态

- 已实现任务分组、主／子 Agent 过程展示、按需获取工具详情、事件序号校验及会话／授权切换后的详情清理。
- 验证：`npm --prefix web/portal test` 通过 57 个测试文件、672 个 Vitest 用例及 2 个 Node 用例；`npm run lint`、`npm run typecheck`、`npm run build` 通过。生产构建输出了超过 500 KB chunk 的体积警告。
- 2026-11-13 本轮复验：Assistant 动态相关定向测试、`npm run typecheck`、`npm run lint` 和 `npm run build` 通过。完整 `npm test` 首轮发现 4 项 `Hosts.test.tsx` 断言仍假定旧的卡片折叠交互；已将断言改为检查当前共享控制权摘要与折叠详情状态，并复验 `npm test` 全通过（57/57 文件、679/679 Vitest、2/2 Node）。此次仅同步测试期望，没有改变 Host 行为。
- Portal Playwright 本地 mock 流程中，连续消息维持独立 Task workchain、主会话／旁聊列表隔离两项通过；该浏览器结果不替代真实服务验收。2026-11-13 `npm run generate:api` 和生成后 `npm run typecheck` 通过；完整 Portal 构建复验通过，构建仍提示一个超过 500 KB 的 JS chunk。
- 2026-11-13 UI 修复：折叠外侧栏时，任务面板仍固定在第 3 列，使隐藏的会话栏留下空轨道、聊天区和输入框变窄；现让折叠模式的任务面板落在第 2 列。修复前还发现 `Assistant.tsx` 重复状态声明导致 Vite 编译错误，并补齐现有成果下载路由 URL helper。内置浏览器本机 Vite 演示页打开任务详情后，目视确认聊天区与任务面板并列、输入框恢复可用宽度、没有空白列；没有提交消息或操作任务。
- 当前工作树复验：Portal `npm run typecheck`、`npm run lint`、`npm run build`、`git diff --check` 通过；等待态标签和重复任务关联两个定向 UI 用例通过（2/2）。`Assistant.test.tsx` 全量仍有 4 项失败：Workspace 目标提示、文件分页页数预期、超大失败消息显示、结果整理重试通知；T05 保持“待验证”，这些失败需分别核对，不计入本次布局修复通过项。
- 临时启动隔离 Portal fixture 后，PC 端成功提交本机搜索消息并收到合成 Task `q1ez4jpu` 的 `start`／`progress` 事件；详情显示执行者归属待确认。fixture 到时关闭，这只验证了 PC 本地展示，不代表真实 DSH 工具链或跨端联验。无可用常驻 Portal API 时，真实实时事件恢复和权限撤销仍待验证，T05 保持“待验证”。

### T06：iOS 任务动态

- 已实现 MAIN／SUBAGENT 分组、任务步骤时间、按需加载工具详情、事件序号及账号范围校验，并处理 `restricted`、过期、未记录和未知状态。
- 验证：`swift test` 通过 20/20；`xcodebuild -quiet -project Morde.xcodeproj -scheme Morde -configuration Debug -destination 'generic/platform=iOS Simulator' CODE_SIGNING_ALLOWED=NO build` 退出码为 0。构建输出了现有 Swift 并发隔离警告。
- 2026-11-13 本轮复验：Morde Swift Package 单元测试通过 21/21；`Morde` iOS 模拟器构建复跑退出码为 0，编译了任务动态视图与本地化字符串。增量构建未显示新增诊断。
- 模拟器 UI 定向测试 `testMordeTaskDetailUnconfirmedHistoryHasNoRetryButton` 通过；`testMordeTaskDetailShowsRealStepsWithoutBudget` 在调整断言以检查界面实际呈现的步骤标题和“当前”标识后通过。此前一次运行停留于登录页，后续使用本地验收 fixture 重跑成功。
- 本轮尝试让模拟器 UI 测试连接临时本机 Portal fixture，但模拟器请求回环地址失败（`NSURLErrorDomain -3003`）；`testTaskActivityShowsSharedPortalTask` 因此未通过真实服务验收。账号／会话切换和撤权未在真实服务上验证，T06 保持“待验证”。

### T07：跨端真实链路

- 本轮通过真实 DSH Node Runtime 和隔离 PostgreSQL 验证 Worker、状态观察及部分真实工具链；通用 server `make test race` 通过。任务动态定向持久化测试通过（15.390 秒）；全量 `make it` 首轮受 10 分钟默认包级超时截断，随后以延长超时重跑通过（PostgreSQL 1721.770 秒、Worker 548.599 秒）。`server/Makefile` 的 `it` 目标现使用 45 分钟包级上限。Portal Playwright mock 的两项 Task/会话隔离场景通过；完整 Portal 单测复验通过（679 Vitest + 2 Node）。
- 尚未让 PC 与 iOS 连接同一账号和同一真实 Task；未验证并行子 Agent、跨端详情与事件恢复、真实权限撤销、重复发送归并及全部中断恢复窗口。临时 Portal fixture 只能提供合成事件，且 iOS 模拟器无法连接其回环地址（`NSURLErrorDomain -3003`）；不使用它冒充真实 DSH/跨端验收，也不使用其他本地数据库冒充隔离验收库。常驻 Portal API 未配置，`portal.localhost` 返回 502，`127.0.0.1:5173/assistant` 是演示模式。OpenSandbox 凭据未配置，相关集成项跳过；重复请求归并仍依赖对话优化清单，不由前端按相似内容合并。T07 保持“待验证”。
