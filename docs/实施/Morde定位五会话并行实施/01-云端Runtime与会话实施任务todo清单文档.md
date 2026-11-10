# 会话 1：云端 Runtime 与会话实施 TODO

> 阅读：[并行执行入口](00-五会话并行执行入口.md)、[设计方案第 10 节](../../Morde定位对齐与功能精简设计方案.md)。本清单覆盖 A02–A05、A11–A14、B01–B04、B07/B09–B11、C02/C03/C05/C07/C08/C10 及相关 P1/P2 的服务端执行部分。
>
> 边界：仅写入口第 3 节中会话 1 的文件和本清单；路由装配、迁移、sqlc、OpenAPI 交给会话 5；个人/项目业务交给会话 4；客户端交给会话 2/3。保留其他会话与既有未提交改动。`[ ]` 指本次增量未验收，不抹除已有成果。

## 接口与交接

- 拟新增主会话三个接口的语义、版本及权限以设计第 10.1 节为准：本会话实现会话 adapter、用例和 Store；会话 5 落数据库、装配和 OpenAPI；会话 2/3 接调用方。
- 现有 `POST /api/conversations/{id}/messages` 增加消息投递状态并接通执行中补充；`POST /api/tasks/{id}/inputs` 仍只回答澄清。新增/调整的 Task 事件、IPC 帧和检查点由本会话负责，公共合同由会话 5 承接。
- `GET /api/execution` 继续表达账号执行控制与 Runtime 状态；停止一条 Task 与停止整个 Runtime 分别走已有控制语义。项目会话、预算和记忆按会话 4 的授权合同接入，不将项目负责人当作执行者。
- 不发送或上传文件；文件场景用进程内模拟/本地读写。目标 Linux、真实文件传输和第三方 AI 的未验证项不标完成。

## 本机验证范围

按设计 11.1–11.4 执行。会话/租约并发以同一 Mac 的两个逻辑账号和进程内模型/工具替身验证；两种 AI 目标可用两个替身，不要求外部账户或第二台机器。Linux Guardian、真实模型及文件传输分别标待验证；不要让测试自动拉取依赖或发送文件。

## P0-A：会话和云端执行

- [x] **R01（已完成，本模块）建立账号唯一主会话**
  - 修改：`domain/conversation`、`app/conversation_service.go`、`api/conversations.go`、`store/postgres/task_conversation.go`。按设计 10.1 实现只读查询、无候选时幂等初始化和带版本切换；已有多候选时保留数据并返回选择状态。
  - 依赖：W02 提供迁移和合同基线；R01 可先完成本模块测试，再交 W02b 装配公开接口，PC02/I02 随后接入。
-  - 验证：主会话 GET 只读；ensure 按候选数绑定/初始化/要求选择；PUT 做版本 CAS 与幂等回放；列表/单会话带 `scope`/`is_main`。`go test ./internal/api -run 'MainConversation|Conversation'`、`go test ./internal/app -run 'TestConversationService'` 通过；本机隔离 PostgreSQL 16 执行 `go test -count=1 ./internal/store/postgres -run '^TestMainConversation'` 通过，覆盖并发初始化/切换、跨账号拒绝、重复键和不创建 Task。

- [x] **R02（已完成，本机会话 Runtime）补齐账号 Runtime 生命周期和状态**
  - 修改：账号 Runtime 与 Host 生命周期分离；沿用账号执行控制和 Guardian 租约，新增白名单 `startup_failed/runtime_failed` 持久失败原因并从 `/api/execution` 返回，不回显底层错误正文。默认执行控制保持关闭；停用、失败和容量状态分别按现有状态/限额报告。
  - 验证：`go test -race ./internal/worker -run '^TestAccountAgentRuntime'`、`go test -race ./internal/dshguardian`、本机 PostgreSQL `TestPostgresAccountAgentRuntimeFailureIsReported` 及 Execution API 用例通过；本机 Linux Docker 以固定 Runtime 镜像运行 Guardian 崩溃重启、双 Task 隔离、PostgreSQL manager 和 Portal Task worker 集成用例通过。未使用远端测试机或外部模型服务。

- [x] **R03（已完成，本模块）为新 Task 装入同一会话上下文**
  - 修改：`domain/task/types.go`、`store/postgres/task_conversation.go`、`task_conversation_context.go`、`task_execution.go`、`schedule.go`、`worker/cloud_dsh_session_tree.go`。新 Task 在初始事件固定 owner/会话范围、历史序号边界、消息数和摘要；只重建同账号、同会话内可信的用户消息、已完成助手快照和导入历史，按 128 条/32 KiB 限额；普通提交、追加新 Task 和 Schedule Task 均写清单。fresh dsh start 只注入一次；已持久化检查点的恢复不重新读取/注入历史。现有 start prompt 足够承载上下文，无需改 engine.mjs。
  - 验证：本机 PostgreSQL 16 定向用例通过，覆盖 SubmitTask/AppendMessage 边界、会话/账号隔离、未完成助手回复过滤、历史/初始事件缺失时失败关闭、检查点恢复不依赖旧历史，以及 Schedule Task 空历史；worker fresh prompt/恢复/工具对账与 UTF-8/消息数/字节限额测试通过。`go test -count=1 ./internal/worker -run '^(TestCloudDSHFreshSessionReceivesBoundedConversationHistoryOnce|TestCloudDSHInitialPromptRejectsInvalidConversationContext|TestCloudDSHExecutorRecoversCompletedCheckpointWithoutReplayingModel|TestCloudDSHExecutorReconcilesEveryPinnedToolCallBeforeSettling)$'` 与本机 PostgreSQL 16 `go test -count=1 ./internal/store/postgres -run '^TestPostgres(TaskConversationContext|TaskClaimWithCheckpoint|ScheduledTaskClaim)'` 通过。

- [x] **R04（已完成，本模块）按序投递运行中的补充消息**
  - 修改：核对 pinned dsh 的注入、事件与恢复实现；通过 `message_delivery` 帧及 `Agent.followup` 按数据库消息 ID 加入下一轮，保存恢复中的非空 inbox 并按 ID/content 去重。追加消息事务按 Task 状态记录 `pending/unknown`，root checkpoint 仅在同 Task 的 `user/message` ID、角色及正文匹配后原子标 `applied`；错配或无法映射时标记未决输入 `unknown` 并拒绝保存 checkpoint。暂停与等待用户时不取 pending 消息；完成前有 pending 则冲突，失败/取消时剩余输入变为 `unknown`。公共 `delivery_status` 已见 W02b OpenAPI 定义，本会话未改 OpenAPI/迁移。
  - 验收：本机 pinned dsh、不调用第三方模型，验证排队、运行、暂停、等待用户、停止、重复消息、冲突消息、Unicode 边界、检查点匹配/错配/未知 ID、恢复 inbox 与同一 Task 的数据库状态；worker/API/App 与隔离 PostgreSQL 16 定向测试通过。`npm test`（morde-cloud-runtime，55 项）、worker fresh/recovery/delivery 用例及 delivery race 用例、`Test(MainConversation|Conversation)` API、AgentCheckpoint app 用例和 Postgres R01/R03/R04 定向用例通过。

- [x] **R05（已完成，本机会话与停止保护）完成一会话一未结束 Task 与并发停止**
  - 修改：`SubmitTask`/`AppendMessage` 在事务中先锁 conversation 行并检查含 `paused` 的未结束 Task；新增迁移 00111，将唯一索引扩展到 `queued/running/waiting_user/pausing/paused/cancelling`，保留只读旧数据预检。Account Runtime 对缺少 Guardian guard 的 Task 在仍有兄弟 Task 时拒绝确认停止；同会话恢复/提交仍由同一行锁串行化。
  - 验收：本机 PostgreSQL 16 双 pool 同会话提交、不同会话并发、paused 恢复与提交竞争、pause/resume/Guardian 停止证明、唯一索引包含 paused 以及合成旧数据的只读预检测试通过；Account Runtime Worker 对应用例以 `-race` 通过。部署前仍须对目标数据库先运行 `migrations/preflight/unfinished_task_conversation_conflicts.sql` 并处理报告的重复记录；本次没有连接或修改远端数据库。

- [x] **R06（已完成）收敛模型密钥和客户端直接模型路径**
  - 修改：`model_connections.go`、`model_gateway*.go`、`api/model*`；移除用户自带模型 key 的新增和使用，保留平台管理员连接、内部模型网关、MCP OAuth 与既有账本。客户端直调接口先核对调用方，再限制或按 R11 条件退役。
  - 交接：本项先完成服务端验证；PC10/I10 再处理各端入口，公共合同和迁移策略由 W01 持续接收并由 W14 核对。
  - 验证：普通用户创建/读取/更新/删除及解析旧个人连接均被拒绝，数据库旧记录保留；共享平台连接网关、探测及账本定向 app/API 测试通过。本机 PostgreSQL 16 探测结算、未知用量和网关账本集成用例通过。Portal 个人连接入口交接给 PC10，原生验收 fixture 交接给 W14。

## P0-B：个人托管

- [x] **R07（已完成）接入个人记忆、目标和例程上下文**
  - 修改：`cloud_dsh_tool_broker.go` 及会话上下文装配；通过会话 4 提供的 PersonalSync、PersonalMutation、Goal 业务服务，按当前账号读取有界个人资料/记忆/SOUL/目标上下文；模型写入只允许个人会话，通过现有 PersonalMutation 校验和同一 Task 事务提交，未复制存储。计划/提醒仍由现有 Schedule/Notification 用例处理。Task 进度锁定查询同时加载项目范围和实际发起人，供后续授权检查使用。
  - 验证：App 定向测试覆盖个人上下文读取、项目/跨账号/过期授权拒绝、更新工具路由及正文不进入事件；本机 PostgreSQL 16 集成用例覆盖写入与 Task 事件原子提交、重复调用不重复计费、旧 revision 冲突、撤权后拒绝及审计正文脱敏。`go test ./internal/app -run 'TestCloudDSH(ToolBroker|InitialContext|PersonalContext|PersonalData)' -count=1`、`go test ./internal/worker -run '^(TestCloudDSHFreshSessionReceivesBoundedConversationHistoryOnce|TestCloudDSHInitialPromptRejectsInvalidConversationContext)$' -count=1` 和本机 PostgreSQL `go test ./internal/store/postgres -run '^TestPostgresCloudDSHPersonalMutation' -count=1` 通过。
  - 验收：获准用户的资料和目标在模型上下文中可用；跨账号、撤权及写冲突拒绝；审计不泄露正文/密钥。

## P0-C：项目和多 AI

- [x] **R08（已完成，依赖 P06、P07、W08、R03）接入项目会话与实际发起人**
  - 修改：接入项目会话和成员权限；Task 保留会话归属 `owner_id`、实际发起人 `initiator_id` 与执行身份，项目查询依据当前成员资格授权，不改复合外键或复制历史。追加消息、Task 列表/详情/事件、暂停/恢复/取消、澄清和连接器授权均保留实际发起人边界；移除成员后立即失去访问权。Portal 工作项关联 Task 的预检复用 TaskService 主体/Host 授权；Workspace 交付授权未接通时保持 fail closed。
  - 验收：本机 PostgreSQL 16 `TestProjectConversationMembersShareHistoryAndTasksKeepInitiator` 通过，覆盖双成员共享历史、Task/事件读取、按发起人控制、非成员与撤权拒绝、个人/项目上下文隔离。项目 API/App 定向测试通过。

- [ ] **R09（等待 W11 交付；依赖 P08、W11、R08）接通 Todo 到受控外部 AI**
  - 修改：云端 dsh 子 Agent/任务执行、`cloud_dsh_tool_broker.go`；将项目工作项交给获准 Host 上的 Codex、Claude 等受控适配，沿用现有 Task、预算、审批和操作账本，不将同模型子 Agent 误称多工具编排。
  - 当前进度：P08 的 Workspace 交付授权器、Task 关联授权和 Portal 路由已经接通；W16 的已确认 Workspace 产物证明也已实现。Runtime Workspace 清单、服务端 revision 和 Host 发布/同步现在保留文件 mtime；worker 断连后发布自身产物的回归集成用例通过。W11 的 Host 侧受控外部进程接口仍未提供。
  - 阻塞：Runtime 还没有绑定实际发起人、项目和 Workspace 的可撤销执行凭据，也没有可验证的文件/网络隔离与外部进程结果账本。以同模型子 Agent 或通用命令代替 Codex/Claude 会越过授权执行边界，因此本项未接通、未验收。
  - 验收：待 W11 合同可用后，以本地替身覆盖至少两种执行目标、分派、阻塞、取消、恢复与结果关联；无法确认外部进程或副作用时保持未知，不重新派发。

- [x] **R10（已完成，依赖 P09、W12、R08）模型调用接入项目预算和审计**
  - 修改：Task 模型调用发起前原子预留账号与项目预算，按实际发起人/项目/Task 保存版本化费率与用量；结算只在确认用量后入账，未知或超额费用保留待核对。Portal 装配版本化 `models.project_rates`，未知费率和未装配预算均 fail closed；默认空费率表不允许项目模型调用。
  - 验收：配置、API/App/ModelGateway 与 PostgreSQL 定向测试通过；本机 PostgreSQL 16 验证项目双账本预留/结算、并发额度、撤权、费率快照、未知用量核对和个人/项目账本隔离。费率配置字段与单位见 `deploy/single/README.md`。

- [x] **R14（本机服务端完成；真实 Ubuntu/客户端联验留 W14，依赖 W17、W19、W21）接入唯一 Workspace、操作权与移交冻结**
  - 修改：`task_service.go`、`task*` Store/API、`cloud_dsh*`、工具 broker；指定 Host 时核对它唯一的 Workspace 和当前授权，若请求显式指定 Workspace 必须匹配，需要文件时固定已确认版本；纯云端会话可不指定 Host/Workspace。人类控制期间不让 AI 的 Host 操作越过 W19；W21 移交冻结时拒绝新的 Workspace 访问，并排空或阻断既有 Task。
  - 交接：W21 处理 Workspace 所有权、旧外键的分阶段调整；本会话保留历史 Task 的实际发起人和事件，不把旧 Task 改归新所有者。项目授权依 P14、W14 联验。
  - 实现：W21 的移交申请、冻结、接受/取消和旧所有者撤权已交付。Task Workspace Store 通过 Workspace 行锁把活动 Task 访问与移交申请串行化；持久冻结后，Task lease 续期失败，Runtime 在变更 Workspace view/binding 前先过 lease 门禁。Task 仍按实际发起人执行，历史和事件不转移。
  - 验收：`TestRuntimeToolExecutorWorkspaceFreezeBlocksPrepareAndRecoveryBeforeRuntimeMutation` 和本机 PostgreSQL `TestPostgresWorkspaceTransferFreezeBlocksWorkspaceTasksButAllowsCloudTasks`、`TestPostgresWorkspaceTransferFreezeBlocksExistingTaskProgressAndOperations` 通过；覆盖纯云端 Task、提交/续租/进度/Host operation 拒绝，以及 Runtime 不在冻结期间发出 Workspace mutation 请求。
  - 验收：用替身覆盖空 Host 纯云端、Host/Workspace 不匹配、绑定冲突、控制交接与撤权、移交冻结/接受后旧权限失效；旧 Task 历史不被复制、删除或冒名归因。

## 收敛和后续

- [ ] **R11（进行中：旧 Go Agent 循环已移除，兼容入口待 PC10/I10/W13 确认；依赖 R01–R06、PC10、I10、W13）按调用方清理旧服务端循环与兼容入口**
  - 修改：只移除 `agent.go` 等中不再装配的旧 Go 模型循环及专属测试；过渡期的 `task_chat_compat.go` 仅代理统一会话/Task，核对调用方和重试窗口后再退役它、`instant-chat` 与不再受支持的客户端直调模型入口。保留 dsh broker 复用的 Task 刷新、提交、核验等方法、平台模型网关和历史幂等记录。
  - 当前进度：未装配的 `agent.go`、batch、恢复和旧循环专属测试文件已删除；dsh broker 所需共享方法已移入 Task/工具文件，仍被 Cloud DSH 使用的文件证据逻辑及测试保留。App 与 Worker 全包及 race 测试通过。只读调用方核对确认 nanoMuse Morde 页面仍使用 `instant-chat`；模型直调路由仍是公开的 Session/Client API 合同并出现在 OpenAPI。PC10 已完成；I10 的云端替代/数据迁移与发行证据仍未完成，W13 本地备份/恢复已完成但生产调用量/旧 Workspace 归属仍待核实，因此保留 `task_chat_compat`、`instant-chat`、模型入口与历史幂等记录。
  - 验收：全量引用和受支持客户端确认无旧调用；dsh 完成/恢复/审批测试仍通过；数据保留，接口与 OpenAPI 同步。前置未满足只标待验证。

- [x] **R12（本机服务端完成；真实 MCP 服务联验未执行，依赖自身相关 P0、P12）接入 P1 云端技能与连接器**
  - 修改：按 P12 提供的个人/项目授权数据接现有 MCP 工具，不复制凭据或另建工具执行权威。
  - 实现：复用固定 allowlist 的云端技能和既有 MCP 执行器；Project MCP Connector 仅保存非秘密 grant 元数据，Task 快照绑定项目、实际发起人、连接器 owner 和 grant version，准备/dispatch/结果提交均复核当前授权。撤权会取消未 dispatch 操作并把在途副作用保留为可对账的 unknown。
  - 验收：本机 App、API、Portal 路由及 PostgreSQL `TestPostgresProjectMCPConnectorOwnerInitiatorAndRevocationFences` 通过；`TestCloudDSHProjectSkillUsesTaskInitiatorForHostAuthorization` 覆盖项目 Task 的 Host skill 授权使用实际发起人。未连接真实 MCP 服务。

- [ ] **R13（进行中：用户已确认保留云端图片、视频、语音生成；provider、计量和产物合同待会话 5）接入 P2 云端媒体生成与处理**
  - 修改：媒体生成和处理只在云端使用平台管理的模型；产物使用 W16 的 Workspace 归属与确认合同。
  - 当前缺口：ModelGateway 支持文本/工具响应和受权图片输入，但没有图片、视频、语音生成/处理输出、平台模型能力声明、用量结算或产物写入链路。W16 的 artifact API 只能证明已提交文件，不能生成媒体；不能将截图输入或本地占位文件当作媒体生成完成。
  - 验收：用进程内替身核对个人/项目归属、权限和未保存状态；不建立客户端模型/工具循环、不调用真实服务或发送文件。
  - 2026-11-03 本轮核对：原生 Composer 有相机、照片/视频、文件、粘贴图片和语音入口；旧 Image/Video generator 包含客户端 key/relay 路由。Morde `ModelResponse` 仍只有文本/工具响应；Task fence 图片 resolver 只读授权图像输入，没有 Task 上传、音视频输入、媒体生成 job 或持久 artifact 归属。只读检查，未改客户端文件或调用模型。
  - 2026-11-03 范围提案（历史提案；2026-11-04 用户已更新生成范围）：Task 聊天只纳入用户显式选择的图片输入和现有 ModelGateway 图片预算链；原生语音只保留严格 on-device ASR 转写成 Task 文本，云 ASR 需走 Morde 管理连接，设备/语言不支持 on-device 时不得回退联网。Share Extension 图片复用显式 Task 选择，不自动上传。云端 ASR、通用文件/视频模型输入、独立图片/视频生成及持久生成 artifact 在新合同/调用方就绪前保持不可用；不回退旧 key/relay。工具截图仍是当前 Task fence 下独立的内部输入，不混用 Library attachment。
  - 交接：对应新清单 MR-04 已向会话 5 提交 Task 媒体合同需求，区分 Library 与 Task 附件，要求 owner/conversation/task + 授权版本、MIME/大小/摘要、幂等、额度结算、unknown 对账和 capability fail-closed。Task attachment API、云 ASR/media provider 与装配合同仍待会话 5；持久生成 artifact 若后续纳入，需另行定义获准 Workspace/revision 归属。合同/调用方未就绪前保持未完成。


- 2026-11-04 用户决策与 R13 交接：用户确认本阶段保留平台受管模型提供的图片、视频、语音生成；此决定不开放 Task 图片上传，后者继续按 MR-04 fail-closed。W16 已交付，但其 API 只证明 Workspace 文件已提交，不会生成或存放媒体。请会话 5 冻结媒体类型/请求与响应、MIME/大小/时长边界、同步或长作业状态/取消/unknown 对账、模型能力声明、用量与预算账本、幂等键、用户显式保存确认、Workspace owner/revision 归属及产物保留/清理规则，并提供 provider 接口、OpenAPI、capability/路由装配与必要迁移。会话 1 在合同就绪后负责 app/model 与 Task CloudDSH 调用、预算授权与进程内替身回归；server/internal/model provider、公共 schema/装配由会话 5 负责；旧客户端调用方由会话 3 接入。未确认保存前生成结果保持未保存，不冒充 W16 已验证产物；本项继续未完成。

## 交接记录

本会话按入口第 4 节追加向会话 4/5 或客户端的请求及完成回执；不在别人的清单写状态。
1. **R06 → 会话 2（PC10）**：`web/portal/src/routes/Connections.tsx` 仍提供个人模型连接表单/探测入口；移除或收敛个人入口时保留管理员 `AdminModels.tsx` 对共享连接 CRUD 的调用。R06 已只在服务端拒绝个人连接。
2. **R06 → 会话 5（W14）**：更新 `server/internal/store/postgres/native_acceptance_test.go` 中普通用户创建个人模型连接的 fixture，改用管理员共享平台连接，并继续验证内部替身与用量账本；R06 未越界修改公共验收文件。
3. **R01 → 会话 5（W02b）**：R01 handler、service、Store 与 00104 schema 前置已通过模块/本机 PostgreSQL 验证；在权威 OpenAPI/生成契约中登记三个 `/api/me/main-conversation` 操作并联验真实路由。客户端由 PC02/I02 后续接入。W02 的消息投递状态和含 `paused` 唯一约束仍按 R04/R05 推进。
4. **R04 → 会话 5（W02b）**：`conversation_messages.delivery_status` 迁移 00105 与 OpenAPI `delivery_status: pending|applied|unknown` 已出现在当前本地工作区；请在 W02b 最终整合时确认生成客户端合同与 API schema 同步，并由 W14 运行完整迁移集成验收。本会话未修改迁移或 OpenAPI。
5. **R05 → 会话 5（W02b）**：当前 00002 partial unique index 未含 `paused`；请先运行 `migrations/preflight/unfinished_task_conversation_conflicts.sql` 并人工处理报告的重复会话，然后新增包含 `queued/running/waiting_user/pausing/paused/cancelling` 的约束并测试迁移。Store 已保留 conversation 行锁，不能用约束代替同会话锁顺序。
6. **R08 → 会话 4/5（P08/W08b/W16）**：工作项关联 Task 的 Portal 预检现调用 TaskService，Store 仍在写事务核对项目、实际发起人及成员资格。工作项公开路由已装配；保存 Workspace 交付证据仍因 W16 授权证明未交付而返回 unsupported，不接受未确认版本或文件正文。
