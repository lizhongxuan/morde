# Morde AI 对话能力实施任务 TODO 清单

> 来源：[`Morde-PC与AI对话统一技术设计.md`](Morde-PC与AI对话统一技术设计.md)（2026-11-20）
> 目标：按稳定任务编号逐项实现和验收；完成一项后，在本文件更新状态并附上可复核证据。
> 当前范围：PC、iOS、服务端 Agent、管理后台质量评测；只改本地工作区。

## 当前基线

以下是设计方案第 13 节记录的本地完成项，不代表对应验收用例全部通过：

- [x] PC Portal 代理 POST 请求体处理、16 MiB 限制及 Electron 网络栈验证已完成；相关代理测试和桌面端回归通过。
- [x] 普通文字问题的 DeepSeek 路由解析及 DSH 启动失败时的无工具直接回复路径已实现；服务端路由、模型和 DSH 定向回归通过。
- [x] Linux Workspace 目录句柄检查已修复并在 root/非 root、arm64/amd64 容器中通过；测试站仍是旧服务端二进制，云端电脑创建尚未复验。
- [x] PC、iOS 对话卡已隐藏执行过程并显示任务状态/详情入口；服务端现可依据有限的已验证事实投影部分终态 verdict，其它证据不足的任务仍保守为 `pending`，两端总体状态 UI 覆盖通过；七项门禁尚未普遍完成。
- [x] PC 离线消息错误原因和显式重试入口已显示；实际客户端确认网络恢复后不自动重发，但尚未点击重试验证服务端只受理一次。
- [x] 服务端已有七项门禁结构校验 helper 和首个服务端事实 verdict builder；helper 本身不验证证据真实性、归属、修订或适用性，也不产生可信 verdict。可信 builder 只覆盖部分事实，不能作为七项门禁或 H03 整体完成依据。

尚未完成的整体范围：可信证据与 verdict 的完整门禁覆盖、独立复核、任务详情证据回放、iOS 审批卡与真实工具摘要/证据展示、外部回执未知恢复验收、必要澄清、候选 Run 自动采集、盲测与 Judge 完整门禁/报告、管理端脱敏结果视图、重复错误闭环和剩余端到端验收。T09 套件与 T10 盲测记录已可持久化，自动运行采集与 Judge 尚未完成。

已知集成基线（2026-11-20）：本机 PostgreSQL 完整 `make it` 运行通过，`internal/store/postgres` 1036.714 秒、`internal/worker` 230.968 秒，其余包全部通过或无测试文件。OpenSandbox 因未配置 `CH_OSB_URL` / `CH_OSB_API_KEY` 跳过；MCP live Workspace 的 Linux `openat2` 用例需在 Linux 补验。

## 修改边界

- 服务端沿用 Task、Task Event、Step、Approval、Artifact、DSH checkpoint 和 Morde 模型网关；复核与评测不得引入第二套 Agent Runtime、事件账本或任务生命周期状态。
- PC 功能由 `morde-pc` 内的现有对话入口承载；复用 `web/portal/src/routes/Assistant.tsx` 等嵌入实现。iOS 按原生客户端职责接入，并更新 `Localizable.xcstrings`。界面遵循冰淇淋蓝规范。
- 管理端只呈现有权限的脱敏评测批次、纠正项和结果；现有 `GET /api/admin/quality` 不读取对话内容，继续保持该隐私边界。
- 不直接改远端数据库、账号、主机目录、服务配置或二进制；主机端到端验收需在获准的测试环境部署对应本地构建后进行。

## 接口与数据契约

| 接口/数据 | 当前状态 | 实施目标 |
|---|---|---|
| Task 摘要 `validation.state` | OpenAPI 已有 `pending / PASS / PASS+NOTES / FAIL`；服务端已能依据少量已验证事实产生部分终态，执行事实、备注或证据未覆盖时仍返回 `pending`。 | 保留现有字段和 `Task.status` 生命周期；仅可信服务端验证记录可产生终态 verdict。缺记录、身份不可证明、证据不足或复核失败均不得返回通过。 |
| Task 详情与 Step 详情 | 已有授权的 Task、事件、步骤及步骤详情读取路径；`GET /api/tasks/{id}` 已增加可选 `validation_detail`，SSE 仍只投影安全状态/游标信号。 | 在现有授权详情响应中逐步增加逐项检查、适用性理由和证据引用；原始敏感内容不放入 SSE。所有引用必须验证属于当前用户、Task 和修订。 |
| 任务事件/证据 | 已有 Task Event / Step / Approval / Artifact 与分页/游标机制；完成结果事件可保存修订绑定的声明映射。 | 扩展现有记录和投影，保存声明到证据 ID 的映射、来源/基线/动作/结果/副作用/前后对比及验证信息；重试按现有幂等键和调用 ID 关联，不重复记为新副作用。 |
| 审批响应 | 已有版本化 Task Approval 响应接口 `/api/tasks/{id}/approvals/{approval_id}/responses`。 | 复用审批接口；服务端批准与执行时核对动作、目标、payload 和版本摘要。任何字段变化使旧批准失效；不新增平行审批 API。 |
| 管理端评测读取 API | 现有 `/api/admin/quality` 仅报告质量样本源状态，不读取会话正文。 | 拟新增 `GET /api/admin/evaluation-batches`、`GET /api/admin/evaluation-batches/{batch_id}`、`GET /api/admin/conversation-corrections`。只返回冻结配置、合成用例结果、脱敏证据摘要、分项比率、分歧和纠正项；管理员鉴权、分页和错误格式遵循现有 admin API。 |

## 实施任务

状态仅使用：未开始、进行中、待验证、完成、阻塞。只有实现和本项验收都完成才勾选 `[x]`；状态变更时保留任务 ID，并补充命令、用例或测试证据。

- [x] **T00（完成）恢复 PostgreSQL 集成验收基线**
  - 修改范围：修复确认过的 Host/Workspace 测试夹具和 PostgreSQL 临时测试角色问题；修复 `EnsureDefaultHost` 重复调用误拒绝已有绑定的产品回归。
  - 完成证据（2026-11-20）：使用本机 `CH_TEST_DATABASE_URL` 的完整 `make it` 通过，`internal/store/postgres` 1036.714 秒、`internal/worker` 230.968 秒，其余包全部通过或无测试文件。此前 37 项失败已按测试权限、旧夹具和可复现产品回归分类；后续定向验证覆盖同步/任务并发、Agent Workspace 发布、Host 释放重验、HTTP 同步及安全错误码日志。
  - 平台限制：OpenSandbox 因本机未配置 `CH_OSB_URL` 和 `CH_OSB_API_KEY` 而跳过；MCP live Workspace 目录用例需要 Linux `openat2`，macOS 定向运行时明确跳过，需 Linux 环境补验。未修改远端环境。

- [x] **T01（完成）补齐 Task 证据记录与声明映射**
  - 依赖：T00。
  - 修改：`server/internal/app/task_validation.go`、`task_store.go`、`task_service.go`、`server/internal/store/postgres/task_execution.go`、Task 详情 API/OpenAPI 与生成类型。复用现有 `task.result` 事件、Task Step、Conversation Message 和 Artifact，不建第二个 Event Store，也不新增迁移。
  - 行为：完成声明关联来源、基线、动作、结果、副作用；记录 Task 修订、结果事件序号、证据自然序号、可用的 Tool Call ID 与证据引用。只保存结果文本的 UTF-8 字节范围和哈希，不复制声明或证据正文。引用不存在、跨用户、跨 Task 或跨修订时拒绝该映射，但正常答复仍可完成且校验保持 pending。性能声明记录同一指标/单位/环境下的前后值，差值由服务端计算，并绑定到两条同修订 Task Step；数值本身仍是不可信声明，不能单独产生 PASS。
  - 验收：单测覆盖证据类型、必填关系、脱敏和引用边界；PostgreSQL 集成测覆盖事务一致性、顺序及幂等重试。构造一条回答能从每个完成声明追到证据，只有“已完成”文字时证据判定失败。
  - 本地进度（2026-11-20）：证据映射与 Task 完成、`task.result` 事件及结果 SHA-256 同事务提交；仅接受同 Task/当前修订的消息、带修订 Step 和关联 Artifact。存储端把上游声明 ID 归一为 `c1…c64`，工具调用 ID 只保留受限 ASCII 字符，否则省略；`GET /api/tasks/{id}` 只返回当前修订的 `validation_detail`，该响应设为 `private, no-store`；事件/SSE allowlist 不包含映射，Workspace/Artifact 权限收回时相应引用被隐藏。DSH 根会话的无副作用 `morde_task_result_evidence` 工具可提交 `source=current`、`result=current` 和根会话 `action.call_id`；根 `dsh_delegate` 调用在子会话存在时解析到其 `spawn_key` 绑定的子 Agent 步骤。完成的子 Agent 结果仅向父模型提供子调用的匿名 `call_id` 与工具名，不提供参数或工具输出；结果引用解析限制在根会话及其直接子会话，并校验 Task、所有者和修订。旧检查点重放保留原有委派结果，避免改变持久检查点内容。经运行时核验的单次文件写入及 Workspace 写入 Artifact 会把安全的来源调用 ID 写入现有 `task.artifact` 事件；Artifact 事件同时记录所属 Task 修订，解析时核对 `artifact_id` 与 `step_id`，从而支持挂在无修订字段的最终结果 Step 上的产物。`side_effect.call_id` 只定位唯一对应 Artifact，重复来源调用 ID 不解析；事件/SSE 不对外返回来源调用 ID 或修订字段，无需数据库迁移。旧事件不补造来源；缺少可证明修订和来源的 Artifact 不解析。checkpoint 答复文本不一致或存储引用无法核实时只丢弃映射，正常答复仍可完成且摘要保持 `pending`。应用引用校验、Worker 工具定义定向测试和本机临时 PostgreSQL 事务测试通过；无映射时摘要仍为 `pending`，结构校验不产生 verdict。
  - 子 Agent 证据引用桥接（2026-11-20）：父 Worker 将匿名 `action_refs` 随 `child_settle_ack` 交给 Runtime，Runtime 用同一工具结果恢复父模型上下文，避免父会话检查点与运行时结果不一致；旧 ACK 无 `output` 时仍回退为摘要，后台子任务继续使用摘要。回归覆盖 namespaced 子调用 ID 到当前 Task 直接子会话 Task Step 的解析，并校验所有者、Task 和修订。`TestCloudDSHChildToolResultExposesOnlySafeActionReferences`、`TestCloudDSHExecutorRunsAndSettlesPersistedChildSession` 与 `node --test --test-name-pattern='runs dsh_delegate in an isolated child session' test/runtime.test.mjs` 通过。
  - 当前轮次边界（2026-11-20）：映射绑定到生成它的 DSH turn；同样的答复文字在后续轮次重复出现时，不会复用旧轮的映射。`TestCloudDSHCheckpointHistoryDoesNotReuseEvidenceAcrossTurns` 覆盖该回归，证据工具后同一轮重复答复的既有测试仍通过。
  - 性能对比字段（2026-11-20）：结果声明可附指标、单位、前后值、同一环境及两条测量调用 ID；根会话的两条调用必须成功、已与 Runtime 对账，且输出为 `morde.performance_measurement.v1` JSON，字段和值须与声明匹配。服务端再把调用 ID 解析为同修订 Task Step 并计算 `after-before`，详情 API 只回传引用和结构化数值。缺失/未对账/不匹配输出丢弃整组可选证据映射，Task 正常完成且仍为 `pending`；结构化输出仍可由工具伪造，数值与环境未经 T03 独立复核前仍不可信。
  - 定向验证（2026-11-20）：应用证据单测、Worker 工具定义/子 Agent 引用桥接回归、Portal/Workbench OpenAPI 类型生成及两端 TypeScript 检查通过。使用本机 Docker PostgreSQL 上的临时测试角色和隔离数据库，`TestPostgresTaskValidationEvidenceIsRevisionBoundAndTransactional`、`TestPostgresTaskValidationEvidenceResolvesFinalResultAndDelegatedStep`、`TestPostgresTaskValidationEvidenceResolvesArtifactSourceCall` 均实际执行并通过；临时角色已清理。覆盖性能数值校验/服务端差值、工作区引用脱敏、子 Agent 内部工具 namespaced ID 解析、委派步骤映射及当前修订绑定。新增 `TestCloudDSHPerformanceComparisonRequiresMatchingReconciledMeasurements` 覆盖匹配、数值/指标不符、未对账、失败、缺字段和重复 JSON 键。
  - 后续边界：T02/T03 仍须完成跨模型独立复核和 H01–H07 事实校验，才可能产生可信 verdict。Artifact 只有在运行时证据能唯一对应一次受核验文件写入时才保留来源；Shell 或多次写入无法唯一归因时不推断，旧 Artifact 也不会补造来源。性能 JSON 契约只证明声明与工具输出一致，不能证明测量方法或输出真实。

  - 子 Agent 重启回放补验（2026-11-20）：Settled 子 Agent 的父 checkpoint 会保存服务端生成的摘要和 namespaced action_refs；恢复路径现从子会话 checkpoint 重算并核对引用，再让 DSH Runtime 原样重放已验证结果。兼容旧版纯摘要结果，不恢复或重跑已结束的子会话，也不重复个人写入。TestCloudDSHPersonalRecoveryAllowsSettledChildWithWriteReceipt 通过。

- [ ] **T02（进行中）接入只读独立复核**
  - 依赖：T01。开发前检查 `runtime/morde-cloud-runtime/package.json`、锁文件、现有 DSH 插件和 DSH 上游；复用可用插件/provider。若受限只读复核无法由现有能力满足，记录不兼容原因并选最小适配，不另建 Agent Runtime。
  - 前置检查与方案：`0.2.2-rc.1` 上游没有 Task 结果审核插件；subagent 插件需要当前 Runtime 未安装的 DSH peer 组件，且不是审核专用插件。现有 Morde `dsh_delegate` 由父模型委派并继承父模型，不能作为独立复核。进一步审查确认 Account Runtime 的 `coordinator_start` 可启动独立 DSH 子进程，DSH 可无父 Agent 创建根 Agent/Session；本项将复用 Account Runtime 和 DSH 能力，但使用新的 review 身份、模型路由与账务 fence，工具列表固定为空。领取 Worker、模型路由及账务尚未实现，路径记录见[统一技术设计 §13](Morde-PC与AI对话统一技术设计.md#13-本轮实现与验证记录)。
  - 本地架构审查（2026-11-20）：模型网关的 Task 调用要求 Task 仍为 running 且模型连接等于执行模型；现有子会话继承 DSH 根模型，完成回调在 Runtime 进程停止后才运行。`model_connections` 只保存用户配置的 Base URL/模型名；模型响应实际 ID 后续以加密方式保留，但不同连接 ID 仍不能证明不同模型家族，本项不能直接复用现有执行路径。
  - 实施进度（2026-11-20）：新增 `task_validation_reviews`（迁移 `00213`），以 `(task_id, task_revision)` 唯一，并通过复合外键绑定完成 `task.result` 的事件序号与结果哈希。所有非空、合法的完成结果（包括没有声明映射的结果）在完成事务里写入 `pending` 行；修订重复绑定不同事件/哈希会回滚。新增领取和重试存储方法：用 `FOR UPDATE SKIP LOCKED` 并发领取，以递增代次和有效租约 fencing，最多 8 次后转为 `failed`；领取前将 Task 状态、当前/目标修订或结果哈希已不匹配的记录标为 `superseded`。重试写入需匹配 Owner、修订、事件序号、摘要、租约持有人和代次，旧租约不能释放新 Worker 的工作。`TestPostgresTaskValidationEvidenceIsRevisionBoundAndTransactional`、`TestPostgresTaskValidationReviewOutboxCoversResultsWithoutEvidence`、`TestPostgresTaskValidationReviewClaimsAreLeasedAndFenced`、`TestPostgresTaskValidationReviewRetryLimitFailsClosed` 通过，Task 完成状态及执行租约未被复核队列改变。模型适配器现保留供应商实际响应的 `model` 标识（Responses 非流式/终态流及 ZAI 非流式/流式）；有效值随 Task 模型响应加密持久化，并在结果恢复时一并恢复，缺失时保持未知，不用配置名补值。当前仍无领取 Worker、独立审核 Session、审核模型路由账务或可信 verdict。
  - Worker 闭环前的网关账务边界审查（2026-11-20）：确认复核不能复用 `ModelCallTask`（要求原 Task 仍 running 且执行模型固定），也不能借用 `chat`/`compaction` source，否则会错挂 Task/项目归属或复核语义。专用 source 还需同步模型调用 reserve/begin/settle/abandon、个人/项目预算、未知用量对账和管理端 source 校验。该审查时 Worker、独立 DSH Session 和账务路径尚未实现；后续已在下方 Worker 闭环切片落地，fence 与边界见[统一技术设计 §13](Morde-PC与AI对话统一技术设计.md#13-本轮实现与验证记录)。
  - 最小适配：增加按 Task 修订、结果事件序号和结果哈希绑定的服务端 review outbox/记录及只读 DSH 复核 Session；增加有预算/账务 fence 的复核模型调用路径，运行于已有 DSH Runtime、工具白名单为空。模型身份按服务端版本化的精确 Provider endpoint/model catalog 判定，并保存配置版本及供应商实际响应 model 字段；目录外、响应标识缺失、同家族、超时或证据不足均保留 pending。不得把连接更新时间当成供应商模型版本。
  - 修改：服务端 DSH 协调、模型网关和 Task 复核记录路径；复核者使用与执行者不同的 Agent/Session，优先不同模型家族，不能写文件或调用外部副作用工具。
  - 行为：服务端在结果候选生成后触发复核并构造只读包，只含目标、约束、差异/产物、测试结果和授权证据；绑定当前 Task 修订并持久化复核 Session、模型家族/版本、逐项理由及证据 ID。仅可证明模型家族独立且证据充分时，才允许其 `PASS / PASS+NOTES / FAIL` 进入可信 verdict 路径；同模型、模型身份不可证明、不可用、超时或证据不足均保持待复核。
  - 验收：合成任务覆盖三种结论、同一执行者、不同模型家族、无证据、复核超时；只有独立且有证据的结论可进入可信验证构建器。
  - 定向验证（2026-11-20）：`cd server && go test -count=1 -timeout=8m ./internal/store/postgres -run '^TestPostgresTaskValidation(ReviewClaimsAreLeasedAndFenced|ReviewRetryLimitFailsClosed|EvidenceIsRevisionBoundAndTransactional|ReviewOutboxCoversResultsWithoutEvidence)$'`、`go test ./internal/app ./internal/model` 与 `git diff --check` 均通过。本项仍进行中，不代表独立复核功能或 verdict 验收完成。
  - 实施进度（2026-11-20，复核结果持久化切片）：迁移 `00214` 为 outbox 增加执行/复核模型连接及连接版本、精确 endpoint、配置模型、供应商实际模型 ID、家族与 catalog 版本、复核 Session 和受限 JSON 报告；数据库约束保证只有 `reviewed` 行能包含完整身份与报告，且两端家族不同、catalog 版本一致。`ParseTaskValidationReviewReport` 拒绝重复键、未知字段、超长文本和非 H01–H07 报告；每项理由有长度限制，引用必须属于已持久化的同修订证据。`CompleteTaskValidationReview` 先锁定并复核当前完成结果与证据，再验证执行者/复核者的模型身份能由版本化精确目录重算且家族独立，最后以有效租约、代次、Owner、修订、结果事件序号及摘要原子写入；过期租约、旧代次、失配结果不能写入，Task 生命周期保持 completed。模型目录只收录官方文档可精确核实的 OpenAI GPT-6、DeepSeek V4 和 Z.AI GLM-4.6；目录外模型仍 unknown。App 契约测试和 PostgreSQL `TestPostgresTaskValidationReviewCompletionRequiresLiveFencesAndPersistsIdentity` 通过，覆盖无效证据不改变队列状态、过期租约拒写、重新领取后写入、重复完成无副作用。此切片提交时，outbox Worker、只读 DSH Session 和专用模型网关/账务路径尚未实现；它们随后在下方 Worker 闭环切片落地。T03 可信 verdict 仍未实现。
  - 实施进度（2026-11-20，Worker 闭环切片）：新增 `TaskValidationReviewWorker` 并在 portal 生命周期中启动；按配置并发领取结果，读取同修订目标/结果/证据，恢复 outbox 绑定的真实执行模型响应，再选取已检查且可证明异家族的模型。复核复用 DSH Account Runtime 的专用 review Session，工具集固定为空；checkpoint 连续性、模型调用 ID、模型响应、最终 settled 文本、报告证据引用、当前租约和执行模型 call ID 均在入库前核对。模型失败或输出无效只释放为延迟重试，最多 8 次后按现有队列策略失败关闭；此 Worker 只写复核建议，不改变 `Task.status`，可信 verdict 仍由 T03 实现。迁移 `00217` 把完成结果绑定到准确的执行模型调用，避免同修订内其他调用冒充。
  - 定向验证（2026-11-20，Worker 闭环切片）：App 复核报告测试、Worker 成功路径测试、Account Runtime review 协议测试及 `cmd/portal`/相关 Go 包 compile-only 均通过；`runtime/morde-cloud-runtime/test/runtime.test.mjs` 124 项通过，覆盖跨语言模型调用 ID、无工具根 Session、租约取消和拒绝工具调用；`git diff --check` 通过。review 输入包目前包含目标、结果和已核实的证据引用/结构化性能值，不含 artifact、命令或测试输出正文；缺少正文的引用必须判为 `insufficient`，因此 T02 的“证据充分才可建议通过”仍未完成。

- T02 复核输入快照切片（2026-11-20）：Worker 改从 `TaskService` 取当前任务快照；步骤证据附带现有加密详情解密后的脱敏操作摘要，Artifact 只附标签/路径/媒体类型/大小，原用户消息正文和文件字节不发给审核模型。快照在入包前重新校验 Task Owner、发起人、会话、已完成状态、修订和结果哈希，并复用现有 Host/Workspace/Artifact 授权过滤；无权限的引用从输入中剔除，审核者被明确要求将仅有元数据或不足以核实的摘要判为 `insufficient`。步骤详情读取再检查一次权限；读取期间撤权时本次复核失败重试，避免继续发送已变更授权范围的摘要。新增 Worker 用例覆盖步骤摘要、撤权和 Artifact 元数据边界，App 用例覆盖 Workspace 权限撤回后隐藏摘要和引用。`go test ./internal/app -run 'TestTaskValidationReview|TestTaskStepDetail' -count=1`、`go test ./internal/worker -run 'TestTaskValidationReview' -count=1`、相关包 compile-only 与 `git diff --check` 通过。本机 `morde-local-postgres` 上临时创建并清理隔离测试角色，7 个证据映射、outbox、租约与完成绑定 PostgreSQL 集成用例全部通过。首跑发现过期租约写入先触发执行模型调用绑定错误；已调整为失效 lease 的 completion 直接无副作用返回，复跑通过。终端原始输出、测试日志正文和 Artifact 文件内容没有持久化证据快照，合成 Provider 的完整 Worker/网关/PostgreSQL 集成已在下方补验；真实 Provider 和 DSH 子进程端到端仍未覆盖，T02 保持进行中。

- T02 Shell 进程状态证据切片（2026-11-20）：经运行时结果严格解析，只将终态 `shell_exec` 的退出码、超时、stdout/stderr 字节数和截断标记写入现有加密步骤详情；命令与输出正文不进入独立复核包，普通 Task 详情 API 不返回该内部字段。解析失败、状态不匹配或过长结果不生成证据。App 的映射/脱敏/解密用例、Worker 复核输入用例及 `TestRuntimeToolExecutorPollsAndMapsShellExitCode` 通过；完整 `go test -count=1 ./internal/app ./internal/worker` 通过，无数据库迁移。相关 PostgreSQL 活动/详情集成测试目标本轮已编译，但因未设置 `CH_TEST_DATABASE_URL` 按约定跳过；可选 JSON 字段不需要数据库迁移。该证据只表明 Host 报告的进程级结果，不能证明具体测试断言或任务正确性；Shell 原始输出、测试日志正文和 Artifact 文件内容仍未进入复核包，T02 继续进行中。实现边界见[统一技术设计 §13](Morde-PC与AI对话统一技术设计.md#13-本轮实现与验证记录)。

- T02 Worker+网关+PostgreSQL 闭环验证（2026-11-20）：新增 `TestTaskValidationReviewWorkerPersistsGatewayResultWithPostgres`，通过本机 `morde-local-postgres` 创建并清理隔离数据库实际运行。测试使用合成 `ModelClient` 和 DSH review 协议测试桩，覆盖复核 Worker 读取当前 Task、恢复加密执行响应、选择异家族模型、经专用网关结算复核账务并持久化报告；断言 `Task.status` 仍为 completed、可信 `validation.state` 仍为 pending。`go test ./internal/worker -run '^TestTaskValidationReviewWorkerPersistsGatewayResultWithPostgres$' -count=1 -v` 与对应 App 报告测试通过。它没有启动真实 DSH 子进程或连接真实模型 Provider；Artifact 内容、终端输出和测试日志正文仍不在复核包中。另为防止内部建议自相矛盾，服务端现拒绝 `PASS`/`PASS+NOTES` 搭配非 supporting finding，也拒绝所有 finding 都 supporting 的 `FAIL`；`TestTaskValidationReviewRecommendationMatchesFindings` 覆盖边界。T02 仍进行中。

- T02 Worker/PostgreSQL 主链路补验（2026-11-20）：TestTaskValidationReviewWorkerPersistsGatewayResultWithPostgres 在本机临时 PostgreSQL 上通过，临时测试角色和随机数据库均清理。合成 Provider 与 DSH 协议桩驱动真实 Worker.RunOnce、ModelConnectionService 独立家族选择、ModelGatewayService 账务和 PostgreSQL outbox/结果写入；报告使用 H01–H07 insufficient/FAIL，验证家族身份持久化、复核账本恰结算一次，Task 仍 completed 且可信摘要仍 pending。修复复核模型选择路径和复核快照授权主体缺少 RoleUser 的问题；测试帧以 json.RawMessage 嵌入 DSH 模型请求。真实 Provider、真实 DSH 子进程、终端/测试日志/Artifact 正文仍未覆盖，T02 继续进行中。

- T02 公开网页内容证据切片（2026-11-21）：`morde_web_search` / `morde_web_read` 成功返回后，从已持久化的工具结果中生成最多 4 KiB 的 UTF-8 原文摘录及完整内容 SHA-256；发现凭据特征、失败结果、图片或非内置 MCP 工具时不生成摘录。摘录复用现有 Owner/Task/Step/修订 AAD 加密的 Step Detail，普通 Step API 通过 `json:"-"` 不会返回；独立复核 Worker 仅在权限复核通过且步骤成功时读取，并按不可信网页内容交给只读复核者。应用与 Worker 定向用例通过，`go test -count=1 ./internal/app ./internal/worker` 全量包回归通过（app 19.352 秒、worker 39.144 秒），`git diff --check` 通过。Workspace 文件字节不进入审核模型输入。曾加入的 Workspace 摘录采集与复核传输在隐私复核后已于本地回退，未发送给模型或外部服务；新增回归将验证读取详情、Worker 输入和 H02 verdict 都排除 Workspace 内容。Shell/测试日志正文和 Artifact 内容仍未进入复核包，T02/T03 继续进行中。

- H02 内容证据绑定切片（2026-11-21）：迁移 `00221_task_step_content_evidence.sql` 在加密 Step Detail 同行保存 32 字节内容摘要，`TaskService` 解密后校验摘要与文档摘录匹配。经隐私复核，Workspace 文件字节必须排除在审核模型输入外，因此已回退 Workspace 摘录采集/传输与 H02 放行；当前仅成功的内置 `morde_web_search` / `morde_web_read` 可进入内容证据覆盖计算。Workspace 读取仍可正常给用户使用，但不会生成审核摘录；涉及 Workspace 的 H02 保持 `pending`，待有不外发文件内容的验证方式后再开放。新增回归分别检查 Workspace Step Detail 不保存摘录、Worker 拒绝 Workspace 摘录及 PostgreSQL 不凭合成 Workspace 内容证据解锁 H02。此前全量 App/Worker、vet 与本地 PostgreSQL 定向测试通过的是回退前版本，不能视作当前复验结果；本次修改后测试结果见后续记录。H02 仍不覆盖 Shell/测试日志和 Artifact 内容，内容覆盖也不替代 reviewer 对语义相关性的判断；T03 不得标记完成。
- H02 隐私边界回归（2026-11-21）：`go test -count=1 ./internal/app ./internal/worker` 通过（21.226/44.043 秒）；`go vet ./internal/app ./internal/store/postgres ./internal/worker` 通过；本机 PostgreSQL `TestPostgresTaskValidationFactsBindWebContentAndExcludeWorkspaceBytes` 通过（4.115 秒）；`git diff --check` 通过。工作区读取仍可交付给用户，但其字节不发送给外部复核模型，H02 保持 pending；全量 PostgreSQL 集成集未运行。

- T02 DSH Account Runtime 序列帧修复（2026-11-21）：Worker 首次接入真实 Node Account Runtime 集成时，Go 发给 review Session 的 `checkpoint_ack` 等响应包含 `runtime_sequence`；Node 侧原先未将该字段纳入 Account Runtime envelope 校验与剥离，因而在模型调用前拒绝有效帧。现在要求 Account Runtime fence 字段完整，并校验序列号等于当前 review Session 的序列后再执行 review 帧校验；新增过期序列拒绝回归。`node --test test/runtime.test.mjs` 125/125 通过；`TestTaskValidationReviewWorkerPersistsGatewayResultWithPostgres` 在本机临时 PostgreSQL 上以真实 Node Account Runtime、合成 ModelClient 通过，Task 仍为 completed 且可信 validation 仍为 pending；`git diff --check` 通过。尚未连接真实 Provider；Shell/测试日志及 Artifact 正文仍不在复核包中，T02 保持进行中。

- T02 完整网页摘录摘要复核（2026-11-21）：独立复核 Worker 现会重算未截断 `content_evidence.preview` 的 SHA-256，并拒绝大写/非规范摘要；截断摘录继续使用完整源文摘要，因为 Worker 只拿到前缀。新增回归覆盖匹配、篡改、不规范和截断四种情况；`go test -count=1 ./internal/worker -run '^(TestValidReviewContentEvidenceChecksCompletePreviewDigest|TestTaskValidationReviewInputIncludesBoundSummariesAndArtifactMetadataOnly)$'` 与 `go vet ./internal/worker` 通过。此检查绑定摘录字节，不证明网页来源真实性或语义支持；T02/T03 仍进行中。

- [ ] **T03（进行中）实现事实驱动的七项门禁和可信 verdict**
  - 依赖：T01、T02。
  - 修改：`server/internal/app/task_validation_verdict.go` 及其测试、`server/internal/store/postgres/task_validation_review.go`、`task_k2_projection.go`、`task_execution.go`、对应 PostgreSQL 集成测试与迁移；更新 `server/openapi/portal.yaml`（仅当公开合同变化）。现有 `ValidateTaskHardCheckShape` 只能作为输入结构校验。
  - 行为：由服务端从任务事实推导 H01–H07 适用性，验证证据存在且归属/修订正确，结合独立复核结果生成可信摘要。适用项缺失或事实不足阻断通过；`N/A` 必须带服务端推导原因，明确失败优先显示 FAIL；新修订不得继承旧 verdict。H02 还须由服务端确认内容级证据，不能只信复核模型声称支持。
  - 验收：单测及集成测分别注入七项通过、每项失败、合法/伪造 N/A、迟到旧结果、跨 Task 证据和 `Task.status=completed` 但未复核；无可信记录时 API 始终为 `pending`。
  - 前置事实审查（2026-11-20）：只读审计发现 `task_validation_reviews` 已绑定当前修订/结果事件/哈希并持久化独立模型身份，但 Task 摘要及详情仍固定为 `pending`。H03 的独立身份与结果 fence 已可核验；H01 缺少主对话呈现契约，H02 复核包缺少足够内容级证据，H04 有澄清时无法在复核包完整评估，H05 缺完整动作风险分类，H06 没有跨 Task 错误特征与纠正项台账，H07 未证明完整只读回放。不得仅把模型 `recommendation` 投影成 verdict。审查结论和后续边界见[统一技术设计 §13](Morde-PC与AI对话统一技术设计.md#13-本轮实现与验证记录)。
  - 实施进度（2026-11-21，可信状态持久化切片）：新增迁移 `00220_task_validation_verdict.sql`，在同一条修订绑定的 review 记录中保存服务端汇总状态，旧记录默认 `pending`，非 `reviewed` 行不得存终态。复核完成事务先验证独立模型身份、当前结果/证据和租约，再读取任务执行事实；没有工具步骤、操作、子 Agent 或 Artifact 时，服务端只为 H03/H05/H06/H07 生成相应 N/A。含执行痕迹的 Task 在动作风险、跨任务纠正闭环和完整回放证据补齐前保持 pending；有澄清记录但复核输入不能完整呈现时 H04 也保持 pending。H02 即使模型声称支持，若服务端没有确认内容级证据仍保持 pending；任一已验证的矛盾可产生 FAIL，即使其他项仍未决。当前没有带证据引用的结构化备注，复核模型给出的 `PASS+NOTES` 也保持 pending，不会把备注折叠成普通 PASS。详情与列表只在 Task 当前完成修订、结果哈希和 `task.result` 事件匹配时投影状态，未复核/旧修订继续 pending。公开 DTO 未变化，不需 OpenAPI 改动。`go test ./internal/app -run 'TaskValidationVerdict|EvaluateTaskValidationReview' -count=1` 与 PostgreSQL `TestPostgresTaskValidationReviewCompletionRequiresLiveFencesAndPersistsIdentity`、`TestPostgresTaskK2Projection` 通过，覆盖失败优先、合法 N/A、未知事实阻断、复核建议不直接成为 PASS，以及详情/列表一致投影。H02 仅将成功的公开网页摘录作为候选内容事实来源；Workspace 文件字节不发送，涉及 Workspace 的 H02 保持 pending，网页内容与 claims 覆盖也尚不能证明语义相关性；Shell/测试日志、Artifact 和其它门禁事实仍不完整，T03 未完成。
  - 旧修订隔离回归（2026-11-21）：扩展 `TestPostgresTaskValidationReviewCompletionRequiresLiveFencesAndPersistsIdentity`，在旧修订已有终态 `FAIL` 后注入一条新的已完成修订及匹配的 `task.result` 事件；Task 详情和列表均保持 `pending`，旧 review completion 不能修改新修订，数据库仍只有原修订的 verdict。使用临时本机 PostgreSQL 的定向用例通过（0.69 秒）。这只锁定已有修订过滤行为，不代表 H02 语义支持或 H04–H07 执行型事实已补齐，T03 仍进行中。

- [ ] **T04（进行中）完成授权详情与只读证据回放**
  - 依赖：T01、T03。
  - 修改：`server/internal/api/tasks.go`、`task_events.go`、现有 Step 详情 API 与 OpenAPI；PC `web/portal/src/routes/Assistant.tsx` 及其测试；iOS `clients/nanoMuse/android/src/ios/` Task 详情页与 `Localizable.xcstrings`。
  - 行为：主对话展示结论、状态和详情入口；详情按序呈现步骤、模型/工具调用摘要、命令退出码、结果、审批、产物、前后对比、逐项门禁与复核证据。回放只读并按原序重建，不调用 Runtime 或工具；不保存或呈现模型私有思维链。无权、已脱敏、已过期和未记录内容明确显示；SSE 仍不携带原始工具/模型内容。
  - 验收：API 测试覆盖分页、顺序、授权、跨账号/Task 拒绝、脱敏和过期状态；PC 必须启动并操作实际 `morde-pc` 验证主对话和详情；iOS 在 Simulator/设备验收同一任务。记录回放前后工具执行计数相同。
  - 实施进度（2026-11-21，PC 证据索引切片）：PC Task 详情现展示当前 Task 修订的声明片段、证据引用和性能前后对比；引用校验会话消息、修订、事件序号，只展示已核验 Artifact 元数据，Step 正文复用授权详情 API 且仅在展开时读取。UTF-8 字节范围不合法、证据属于旧修订或事件超前时不会展示为有效内容。`Assistant.test.tsx` 212/212 通过，Portal 类型检查、PC `build:portal`、桌面端 `build` 与 `git diff --check` 通过。实际 PC 窗口本轮停在本机登录页并提示凭据错误，因此没有声称完成实际 UI 验收；服务端分页/回放与 iOS 详情仍未完成，T04 保持进行中。

- [ ] **T05（进行中）落实必要澄清与任务关联**
  - 修改：`server/internal/app/conversation_route_decision.go`、`conversation_route_worker.go` 和现有 PC/iOS 对话交互。
  - 行为：只在缺少阻塞输入、目标有实质歧义、超授权范围或需要用户拍板时提问；问题指出具体缺项，等待时为 `waiting_user`。可逆且在授权范围内的选择由 Agent 自行完成并记录假设。追问/修改/停止按会话和 Task 版本关联，歧义未解时不擅自取消其他任务。
  - 验收：缺原文改写只问原文；信息齐全的可逆格式任务不泛问；等待期间不执行；并发 Task 的追问、修改和停止只作用于目标 Task。
  - 实施进度（2026-11-20）：路由分类提示现要求有安全、低风险、可逆默认值时继续路由，不追问偏好；Task Agent 执行策略要求在授权范围内自行采用保守默认，并在用户可见结果中说明重要假设。只有缺失信息阻碍安全推进，或会实质影响目标、授权、成本或不可逆动作时才澄清。新增路由提示与 SOUL 注入断言；`go test -count=1 ./internal/app ./internal/worker` 通过。已有路由用例覆盖缺原文时问具体缺项及对未知/过期 Task 不执行控制；本轮尚未重跑 PostgreSQL 因果路由集成、PC/iOS 实际客户端澄清流程，T05 继续进行中。

- [ ] **T06（进行中）将不可逆审批绑定精确动作版本**
  - 依赖：T01。
  - 修改：`server/internal/app/task_approvals.go`、相关任务工具 dispatch 路径、`server/internal/api/tasks.go` / OpenAPI（若需补安全的动作摘要字段）、`web/portal/src/components/TaskApprovalCard.tsx` 与 iOS 审批卡；保持现有版本化 Approval API。摘要由服务端基于已绑定 payload 生成，只暴露用户可核对的必要信息，不返回原始秘密参数或内部哈希。
  - 行为：审批卡在发送、购买、删除、发布等副作用前展示目标、内容/差异预览、金额或影响、证据和动作摘要。服务端对 `action + target + payload + version` 做绑定；审批前无副作用，拒绝/撤销/超时不执行，任一字段变化后旧批准失效。
  - 验收：用合成目标测试先拒绝、再批准、修改 payload 后重用旧批准、并发重复批准及外部回执未知；核对授权事件、摘要、dispatch 次数和实际结果，任何未批准或旧版本动作均不能执行。
  - 实施进度（2026-11-20）：服务端已把工具名与完整参数摘要绑定到带版本的审批记录，并在 Host dispatch 前再次核对摘要及运行时控制 fence；拒绝/超时无 dispatch、并发决策由存储层串行化。现为 Shell、浏览器输入、桌面输入及通用 Host 工具卡片显示脱敏且不超过 240 字符的参数预览；不显示内部 Workspace ID。获批后若同一调用 ID 的 payload 改变且尚无副作用记录，任务以明确的不匹配结果停止；有检查点或副作用记录时继续按未知结果安全暂停。新增精确批准一次、拒绝、过期、控制 fence 变化、payload 篡改零 dispatch、预览脱敏，以及模拟 Host 已执行但回执丢失后跨 Task lease 对账且不重复执行的回归；`go test -count=1 ./internal/app ./internal/worker` 通过。PostgreSQL 并发审批用例当时因 `CH_TEST_DATABASE_URL` 未设置而未实际运行；旧打包 PC 窗口也曾白屏。源码排查后修复认证态 Portal 加载失败时仍误显示空白工作区的问题；source-built PC localhost 登录和对话验收通过，见统一设计文档 §13。
  - 实施进度（2026-11-21，PC 审批卡实测）：在独立临时用户目录启动 source-built `morde-pc` arm64，以本机 `TestConversationBrowserAcceptance` 合成审批请求完成真实 PC 界面验收。Task 在服务端进入 `waiting_user`，pending request 保持 `pending`；PC 任务详情显示审批标题、对象、内容、影响、风险/费用未知态、到期时间及允许/拒绝控件，展开后显示累计费用、替代方案和撤销方式。测试端 `verify/approval` 与 `TestConversationBrowserAcceptance` 均通过（456.63 秒）；没有点击允许/拒绝，未触发任何外部副作用。另在本机临时 PostgreSQL 上运行 `TestPostgresConcurrentApprovalByTwoAuthorizedClientsIsAppliedOnce` 通过。当前 `TaskPendingRequest` 没有独立的动作摘要/证据字段；部分真实工具把安全预览写入 `content`，但本次合成卡片没有覆盖这些预览与证据的可核对性。T06 继续进行：iOS 审批卡、真实工具摘要/证据显示、真实执行回执和批准前后 payload 变更的端到端验收仍未完成。

- [ ] **T07（进行中）完成 PC/iOS 跨端会话、重连与幂等闭环**
  - 修改：现有 conversation/message/task API、PC `Assistant.tsx` 与 outbox、iOS 会话/消息存储和同步；只在发现接口缺口时同步 OpenAPI。
  - 行为：两端共享同账号会话、消息、Task 状态和结果；多会话归属正确；超上下文时使用授权历史且原始记录可查看，缺历史时说明；短暂断网/关闭客户端不取消已受理任务。每条新消息可独立提交，不等待前一条消息的回执或 Agent Task 完成；`previous_client_message_id` 只作上下文关联，服务端再按真实依赖与歧义安全路由。用户显式重试用原幂等语义，网络恢复不自动重发，不重复创建 Task 或已确认副作用。
  - 验收：在实际 `morde-pc` 与 iOS 客户端完成跨端发送/续聊、两个会话隔离、超上下文、断线重连和离线显式重试；另用两个测试账号验证会话、事件和产物互不可读。离线重试核对服务端只受理一次。
  - 进度：iOS Simulator 上 204 后状态恢复、事件流中断恢复、失败路由原位重试和应用重启后消息对账均曾单独通过（各 1/1）；同一模拟器混跑存在共享 outbox/fixture 污染。本轮尝试用合成登录身份隔离固定 origin 的五项顺序测试，但两条消息重启用例在登录阶段失败、路由重试被取消；随机 origin 也停在通用“请求失败”登录页。临时 fixture/诊断改动已全部撤销，未据此判断生产行为。PC source-built `morde-pc` 使用独立 bundle ID/profile 和本机临时模拟服务，实际验证普通消息提交/模拟回复、`model_unavailable` 可读错误、显式重试后恢复；stub 记录到一次普通消息 POST、一次重试请求和最终 4 条消息。另以 `/api/execution=enabled` 的 localhost mock 验证页面不再显示启用提示，普通消息收到 mock 回复。最新在实际重构建 `morde-pc` 中验证：第一条 POST 已由 localhost mock 接收但回执延迟且返回 503 时，仍可立即提交独立后续消息并收到 mock 回复；stub 确认两条 POST，UI 不显示旧的“前一条消息尚未确认”提示。此前 PC 和 iOS 均把前序回执作为后续 POST 门槛：网络结果未知时消息停留在本机，主 Agent 无法路由。现已移除 PC 个人/项目与 iOS 同会话的回执等待，保留幂等 ID 和前序上下文链接；旧 outbox 项在恢复时清除过期错误文案但保留原消息。路由 Worker 默认并发 16，已与 DSH 执行槽位分离；执行上限为全局 4，单账号可用满 4 个槽位。前序路由仍未结束时，Worker 过去会让同会话后续消息每秒回队；现已移除此提前等待，让分类模型看到 `PreviousUnresolved` 后判断独立请求或真实依赖。数据库应用阶段仍重新读取快照，前序状态/候选变化时重分类，缺失前序的任务控制和上下文依赖继续澄清。新增 Worker 回归通过，覆盖未结束的前序路由期间独立文字回复及新 Task 都会进入分类并提交；新增单账号四任务并行回归通过。之前的前序缺失 PostgreSQL 栅栏、迟到恢复和派发 fencing 回归仍通过；PC+iOS 全量验收仍未完成；下方专项记录显示，同账号本地双向会话、PC 离线显式重试、iOS 服务端 503 后显式重试且数据库仅一条回执，以及 Conversation/Message/Task/事件/Artifact 跨账号 API+PostgreSQL 隔离已通过。真实 Provider、多会话跨端 UI、真实网络故障（不使用传输故障 fixture）及客户端超上下文理解仍未完成；iOS `notConnectedToInternet` 传输故障 fixture 已接入本机真实 API/数据库并通过单次受理计数，见下方专项记录。此前测试站客户端访问遇到过 HTTP 403，但截图输入框为空，不能据此归因发送按钮状态；后续已在 localhost 环境完成客户端验收。

  - PC 旧前序错误标签实测（2026-11-21）：普通消息和任务补充消息共用 outbox 展示过滤；重启恢复时清除已持久化的旧文案，真实失败仍显示“发送失败”和显式核对/重试入口。`Assistant.test.tsx` 的补充任务回归、TypeScript、Portal 构建和桌面主进程构建通过；随后用隔离 profile 启动实际 `morde-pc` arm64，登录本机合成账号并提交真实补充消息。HTTP/PostgreSQL 验收夹具只对携带 `reference_task_id` 的请求注入一次旧错误响应；PC 任务详情显示“发送失败”，无障碍树中不含“前一条消息尚未确认，请先核对后重试。”，消息 POST、approval 状态与 PostgreSQL 断言全部通过。`TestConversationBrowserAcceptance` 通过（372.64 秒，包 374.007 秒）。该验收只用本机合成服务，不覆盖真实模型和远程部署；T07 其余跨端及上下文用例仍进行中。
  - 超长会话 DSH/历史检索集成复验（2026-11-21）：本机临时 PostgreSQL 下运行 `TestConversationSummaryDSHMainAndSideChatsContinueAfterLongHistoryRestart`，通过（21.26 秒）。真实 Node DSH Runtime 在主会话及两个旁聊中执行 10 次摘要压缩；主 Runtime 被终止后以新 generation 恢复，主会话累计 840 条原始消息、正文超过 8 MiB。后续 Task 的合成模型先请求授权的原始会话历史搜索，再从分页记录中找回仅存在于旧消息的唯一标记并完成回复；另断言 840 条原文仍可分页查看。该项证明服务端 DSH 恢复、摘要与授权历史检索链路，不证明 PC/iOS 客户端由真实模型端到端理解长上下文；T07 客户端超上下文验收仍未完成。
  - PC 超长原始历史分页实测（2026-11-21）：验收夹具 `MORDE_BROWSER_LONG_HISTORY=1` 在本机临时 PostgreSQL 为独立账号预置 840 条、超过 8 MiB 的已结算消息，并在创建时核对唯一旧标记。source-built `morde-pc` arm64 通过隔离 profile 登录本机 HTTP handlers；实际 UI 初始加载最新一页，连续点击“加载更早消息”8 次后显示第 1 条历史及 `ORCHID-PC-HISTORY-SENTINEL-8472`。扩展后的 `TestConversationBrowserAcceptance` 验证该模式的消息数和历史字节数后通过（41.637 秒）；`Assistant.test.tsx -t 'loads older history when scrolled to the top'` 1/1 通过。本项只证明 PC 原始历史可翻页查看，未在该 UI 流程提交查询或验证模型回答；与上一条服务端 DSH 授权搜索回归合并仍不等于真实 Provider 的 PC 端超上下文验收。
  - 因果路由与恢复修复（2026-11-20）：新旁聊未绑定消息固定首条创建请求体/幂等键，重启后恢复到可见旁聊列表并等待显式重试；iOS 队列允许同会话不同消息 ID 并发提交；服务端缺少前序时允许经当前已结算模型分类确认的独立 `reply` 或 `new`，拒绝未验证/改写的 reply，依赖上下文和任务控制仍澄清。实现细节与范围见统一设计文档 §13。
  - iOS 未知回执恢复修复（2026-11-21）：应用重启/网络恢复只对未确认消息用原 `client_message_id` 查询服务端回执；查不到时保持待确认，不自动 POST。用户显式重试才允许用原消息 ID 提交；若点击发生在回执查询期间，会保留该意图，并仅在确认没有回执后发送。已受理但响应丢失的消息仍在重启后自动对账。iPhone 18 Pro / iOS 27.2 Simulator 上 `testMordeChatComposerRecoversPlainTextAfterOfflineRestart` 和 `testMordeChatComposerReconcilesAcceptedMessageAfterAppRestart` 通过（2/2）；未访问服务端。
  - 前序消息不再串行阻断回归（2026-11-21）：PC 对话 UI predecessor 用例 3/3、iOS 未知回执恢复用例 2/2 通过；`TestConversationRouteWorkerClassifiesFollowUpWhilePredecessorRouting` 通过（1.571 秒）；本机 PostgreSQL 的前序处理中独立回复、独立 Task 与逆序到达 Worker 用例 3/3 通过（5.524 秒）；同会话四个独立 Task 并行执行用例通过（2.091 秒）。这确认前序只作为因果上下文，独立消息可分类/路由；真实 Provider 与测试站并发负载仍未验证。
  - 长会话前序链修复（2026-11-20）：PG 前序环检测原先把深度 128 后仍有前序误报为循环；现用递归去重闭包检测真实循环。本机 PostgreSQL 覆盖 130 层无环链直接回复、两消息循环和循环后代拒绝；该文件中的 11 项因果路由集成回归通过（26.987 秒）。超大链性能尚未单独基准测试。
  - PC 本地 HTTP+PostgreSQL 验收（2026-11-20）：source-built `morde-pc` 使用独立 profile 登录合成账号，经真实本地 HTTP handlers、临时 PostgreSQL、route/task/result workers 完成主聊直接回复和旁聊 Task 结果；切换及整进程重启后两边消息和结果各自保留。`TestConversationBrowserAcceptance` 通过（336.875 秒），最终从数据库核验至少 2 条用户消息和 1 条助手消息。模型/执行器为合成替身；fixture 未配置 MCP Connector manager，因此 UI 的通用网络读取提示不代表聊天路由失败。没有连接真实模型或远端。
  - iOS 本地跨安装与重启验收（2026-11-20）：`IOS03SimulatorAcceptanceUITests.testC10PortalMessagePersistsAcrossAppRestart` 在实际 iOS Simulator 上通过（67.529 秒），使用本地合成 API 与临时 PostgreSQL；UI 登录并发送首条，同账号第二安装经 client-login/API 发送第二条。服务端历史精确核对两条用户消息及第二条关联回复各一份；关闭并重启 App 后，消息状态和任务结果卡仍显示。`TestConversationBrowserAcceptance` 最终计数断言通过（200.31 秒）。自动化使用实际任务结果卡 source 标识和正文；此前对结果卡误查原始消息 ID，状态查询则因首条消息滚出当前可视层级而失败。第二安装未用第二个 UI 客户端；未覆盖真实模型、同时在线跨端、多会话/账号隔离或超上下文，因此 T07 仍进行中。
  - PC+iOS 本地双向会话接续验收（2026-11-21）：source-built `morde-pc` 和实际 iOS 27.2 Simulator UI 同时连接本地 Portal handlers、临时 PostgreSQL 与合成模型。PC 在主会话提交标记并收到回复；iOS 首次登录从同账号主会话读取该标记，测试断言只出现一次，再由 iOS UI 发送消息并让同账号第二安装 API 发送一条，关闭/重启后核对历史、状态和任务结果卡。PC 窗口在 iOS 操作期间持续在线，随后自动显示两条 iOS 消息及各自回复。`TestConversationBrowserAcceptance` 以数据库最终计数断言通过（275.42 秒）；`IOS03SimulatorAcceptanceUITests.testC10PortalMessagePersistsAcrossAppRestart` 通过（67.10 秒）。为该 UI 用例增加可选环境变量 `MORDE_C10_EXPECTED_EXISTING_MESSAGE`，可断言登录后已显示另一客户端发送的消息。验收使用合成模型/执行器，不代表真实 Provider；账号隔离、跨端离线显式重试的服务端恰好一次、多会话和真实模型仍待验，T07 继续进行中。
  - PC 离线显式重试与重启验收（2026-11-21）：实际 source-built `morde-pc` 经本地 HTTP handlers、临时 PostgreSQL 和合成模型运行。夹具让首次 POST 在服务端受理前返回 503；客户端显示单条消息的“重新发送这条消息”操作。关闭并重启客户端后等待 5 秒，服务端仍只看到原始 1 次请求且未受理；开放夹具后用户点击重试，客户端以同一 `client_message_id` 再提交并收到回复。`TestConversationBrowserAcceptance` 通过（216.28 秒），最终 PostgreSQL 断言该 ID 恰有 1 条回执；`go test -count=1 ./internal/store/postgres -run '^TestBrowserMessageReceiptRetryFixture'` 通过。证明该 PC 路径不会因重启自动重发，显式重试保留幂等身份；不覆盖真实 Provider 或生产部署状态；iOS 服务端回执计数见下方专项记录。
  - 并发路由候选超限修复（2026-11-20）：原先第 65 个活跃 Task 或第 101 条待处理/近期请求会让快照报错并将消息标为 `routing_context_unavailable`。现改为有界候选集并附 `candidate_set_incomplete`；明确引用的 Task、前序 Task 和前序请求优先，模型输入超预算时再从低优先级候选尾部裁剪。分类提示要求仅使用实际提供的 ID；直接问答与明确独立工作可以路由，缺少控制目标时澄清。应用/Worker 定向测试、真实模型网关的裁剪候选 fence 集成测试及本机 PostgreSQL 65 Task + 101 请求超限测试通过，显式目标保留。语义去重遇到被截去的候选时仍依赖分类器按不完整上下文规则澄清；乱序前序后代传播另有 128 层/1024 条保护，尚未批次化。当前路由 Worker 默认并发 16；任务执行全局 4，单账号可用满 4 个槽位。
  - 本次修复验证（2026-11-20）：`Assistant.test.tsx`、`Projects.test.tsx` 与 `message-outbox.test.ts` 共 253/253 通过；Portal `npm run typecheck` 通过。覆盖新旁聊连发时同创建键/同请求体、两条消息独立提交、重挂载后保留待发记录且用户显式重试前无 POST。Go app/worker 路由定向用例与本机 PostgreSQL 缺失前序安全处理、独立 reply 只落盘一次且无 Task、回复伪造/改写拒绝、迟到前序与派发 fencing 通过；iOS `Tests/run-credential-expiry-tests.sh` 通过，覆盖同会话前序发送中/失败/回执未知时仍允许提交后续消息。source-built `morde-pc` 的实际 UI + localhost mock 验证首条回执未知时第二条独立消息仍成功 POST，且旧提示不出现。真实模型/服务端与受理次数、iOS 实际客户端、跨端同步、双账号隔离、超上下文和离线显式重试恰好受理一次仍未验收。
  - 工具连接提示与 PC 双轮验收（2026-11-20）：MCP connector 的瞬时读取错误不再进入全局“部分对话数据暂时无法读取”提示，而显示为工具专属提示；定向 `Assistant.test.tsx` 两例通过，Portal `npm run typecheck` 和 `npm run build:pc` 通过。诊断发现先前通用提示来自验收夹具遗漏 `WorkspaceOps` 和 `Onboarding` 依赖，接口返回 `503 internal`；现已在夹具中注入两项服务。修正后，source-built `morde-pc` 对接本机 HTTP handlers、临时 PostgreSQL 和合成模型，连续发送两条独立文字消息并各收到回复；`TestConversationBrowserAcceptance` 通过（165.67 秒），最终数据库消息数断言通过。界面没有“前一条消息尚未确认”文案，也没有通用网络读取提示；MCP 仍按测试配置禁用，显示专属“工具连接暂不可用”状态。跨端、多会话和超上下文验收仍未完成。
  - PC 并发路由与重复请求归并验收（2026-11-20）：在实际 source-built `morde-pc` 主会话连续提交四条相同任务文本；本地 route model 的四路屏障确认四条请求都已并行进入分类后才放行。随后在首个 Task 执行期间发送独立任务和状态追问，UI 分别收到独立任务回复及“该任务正在执行”。PostgreSQL 断言确认四条消息归并到一个根请求/一个 Task、独立任务创建不同 Task、状态追问不创建 Task，重复请求只发布一条根结果。`TestConversationBrowserAcceptance` 通过（142.60 秒）。这是单个 PC 客户端对本地 HTTP/临时 PostgreSQL/合成模型的并发验收，不代表跨客户端或真实模型；T07 的 iOS 实际联验、跨端会话、账号隔离和超上下文仍待完成。
  - 前序回执未知时的 PC 路由复验（2026-11-20）：修复后，旧 outbox 错误文案在恢复时清除；若同会话前序路由仍为 `pending/processing`，后续消息每秒回队，直到前序 Task 对路由可见，不等待 Task 执行完成。实际 source-built `morde-pc` 使用本地 HTTP handlers、临时 PostgreSQL 和合成模型，连续提交两条重复请求、状态追问及独立请求；UI 显示“该任务正在执行”、独立请求回复且无旧错误文案。`TestConversationBrowserAcceptance` 通过（107.76 秒）：DB 断言确认重复请求仅一个 Task、状态追问未创建 Task、独立请求有独立 Task。前端两项 outbox 回归和路由 Worker 的前序/并发定向测试通过。此证据只覆盖单一 PC 客户端与本地合成服务，不替代真实模型、iOS 跨端、双账号隔离或超上下文验收；T07 仍进行中。
  - 同会话未结路由并发修复（2026-11-20）：前序 `pending/processing` 时，路由 Worker 之前直接每秒回队后续消息，导致客户端虽已提交，独立请求仍要等前序路由模型结束。现让后续消息带着 `PreviousUnresolved` 进入分类；事务内重新加载并比较完整快照，前序状态、候选任务或请求集合在分类期间变化则回队重分类。缺失前序的严格模型结果核验、任务控制和上下文依赖澄清保持不变。Worker 单测 `TestConversationRouteWorkerClassifiesFollowUpWhilePredecessorRouting` 及本机 PostgreSQL `TestConversationRouteAppliesIndependentReplyWhilePredecessorIsProcessing` 通过，分别覆盖 Worker 分类路径与前序仍处于 `processing` 时独立回复先落库。后复核发现，并发 POST 到达顺序反转时仍有 `PreviousSequence > claim.Sequence` 的两秒延迟；现已去掉该门禁，让独立消息在前序尚未结算时也进入分类，数据库应用时仍重验快照，任务控制和上下文依赖继续受安全校验。扩展的 `TestConversationRouteWorkerClassifiesFollowUpWhilePredecessorRouting` 覆盖前序消息后到且仍 `processing` 的场景，定向 `go test -count=1 ./internal/worker -run '^TestConversationRouteWorkerClassifiesFollowUpWhilePredecessorRouting$'` 通过（1.517 秒）。另新增 `TestConversationRouteWorkerProcessesIndependentReplyWhenPredecessorArrivesLater`，用 PostgreSQL、真实 route Worker 和合成模型验证前序请求后到且仍未结算时独立回复先完成；与前序正常顺序下的独立回复/Task 两项 PG 回归合跑 3/3 通过（5.571 秒）。真实 Provider 尚未覆盖；实现与边界见统一技术设计 §13。本轮按项目约定补做 PC 实际入口验收，但本机已有 Electron profile 使自动化选中了旧窗口，新隔离实例未连到 fixture；`TestConversationBrowserAcceptance` 等待 8 分钟后超时，临时数据库和隔离 profile 已清理。因此本次 Worker 改动的 PC 端到端仍未验收，既有 localhost/mock UI 证据只覆盖前端回执等待修复。
  - 前序处理中独立任务数据库回归（2026-11-20）：新增 `TestConversationRouteAppliesIndependentTaskWhilePredecessorIsProcessing`，用本机临时 PostgreSQL 验证后续独立请求在引用的前序路由仍为 `processing` 时仍可创建并关联 Task，且前序状态不被改写；与独立回复用例一起定向通过（2/2）。随后重试 PC UI 入口时 CUA 再次绑定旧 Electron profile；本次在提交消息前停止验收，未将此工具选窗问题归因为产品故障。真实 Provider 和本轮 Worker 改动的 PC 端到端仍待验收。
  - 聚合窗口过期时的重复 Task 竞态修复（2026-11-20）：根因是较早请求仍处于 `pending/processing`、短时聚合窗口已关闭时，候选会被通用 Join 校验拒绝；较晚同文请求先提交便创建新 Task，而较早请求不能看到后续序号的 Task。现在只有执行范围相同且正文完全相同的重发可在该状态下关联到根消息；权限、根状态和附件检查保留，非重复内容不绕过聚合截止时间。实际 source-built `morde-pc` 四条相同消息只有一个 Task，后三条归到同一根；独立请求创建独立 Task，状态追问返回“该任务正在执行”且未创建 Task，界面没有旧的“前一条消息尚未确认”提示。`TestConversationBrowserAcceptance` 通过（96.34 秒）；`go test -count=1 ./internal/app -run '^TestConversationRoute'` 通过；过期聚合、并发重复、并发新任务重分类及前序处理中独立回复/任务五项 PostgreSQL 用例通过。仅覆盖本地 PC + 合成模型，T07 仍因 iOS 实际客户端、跨端会话、账号隔离和超上下文验收未完而进行中。
  - iOS 离线消息显式重试（2026-11-20）：发现应用重启会把未确认消息恢复为 `unknown`，而调度器会自动再次提交；这与本项“网络恢复不自动重发”冲突。现将 `unknown` 排除在自动调度之外，“重试”操作以相同 client message ID 将其转为进行中；若再次在重试中退出，恢复后仍回到 `unknown` 等待用户。实际 iOS Simulator 的 `testMordeChatComposerRecoversPlainTextAfterOfflineRestart` 验证离线发送后重启、网络恢复时未自动提交，用户点击重试后发送成功且本机只显示一条；`Tests/run-credential-expiry-tests.sh` 通过。首次 UI 重跑因共享合成账号残留旧 outbox 消息，误点了旧重试项；测试现使用每次独立的合成账号 ID，重跑 1/1 通过。UI 使用本地合成 URLProtocol，尚未核对服务端数据库受理计数；离线显式重试的真实本地 HTTP/PostgreSQL 计数和 PC+iOS 同账号双 UI 接续仍未验收。
  - iOS 旧回执阻塞文案清理（2026-11-20）：确认“等待前一条消息确认接收”和“无法确认前一条消息是否送达”两条本地化项已无代码调用，且未知回执不阻止后续消息提交；删除过期 strings 项。`Localizable.xcstrings` JSON 校验及 `Tests/run-credential-expiry-tests.sh` 通过。该清理不替代 iOS 实际客户端的跨端、账号隔离和超上下文验收。
  - 会话账号隔离回归（2026-11-20）：存储集成测试扩展 `TestConversationMessageReceiptIsAtomicAndScoped`，验证非所有者读取个人会话/消息返回 not found、回执不能跨账号核对、会话列表仅含当前账号。API 测试现在断言认证 principal 会传给会话列表、单条会话、消息列表和回执查询服务。`go test -count=1 ./internal/store/postgres -run '^TestConversationMessageReceiptIsAtomicAndScoped$'` 在本机隔离 PostgreSQL 通过（2.779 秒）；`go test -count=1 ./internal/api -run '^(TestConversation|TestMainConversation)'` 通过（1.521 秒）。这些证据覆盖 API 身份传递与 Store 所有者过滤，尚不替代双账号 HTTP/PC/iOS 端到端、Task/事件/产物隔离验收。
  - Task、事件与 Artifact 跨账号读取复核（2026-11-21）：本机临时 PostgreSQL 上 `TestPostgresTaskSubmissionRollbackAndHostScopedLists` 通过，确认另一个账号不能读取个人会话、Task 或 Task 事件；`TestTaskWorkspacePublishedArtifactPersistsAcrossPostgresConnectionRestartForOwner` 通过，确认 Artifact/Workspace 清单按所有者隔离且重连后仍成立。API 会话、Task 和事件路由定向组 `go test -count=1 ./internal/api -run '^(TestConversation|TestMainConversation|TestTaskEvents|TestGetTask)'` 通过（2.627 秒）；完整 `go test -count=1 ./internal/app ./internal/worker` 通过（19.465/39.035 秒）。这补齐存储边界证据；真实 Task/事件/Artifact API+PostgreSQL 联验见下条，PC/iOS 双客户端 UI 验收仍未完成，T07 继续进行中。
  - Task 与事件流 API 身份传递（2026-11-21）：新增 `TestTaskReadRoutesPassAuthenticatedPrincipal`，将第二个 session principal 注入任务详情和事件流路由，确认两条路由均把该用户 ID、Token ID、身份类型传给服务，并将服务返回的 not found 映射为 HTTP 404。`go test -count=1 ./internal/api -run '^TestTaskReadRoutesPassAuthenticatedPrincipal$'` 通过；更完整的路由到 PostgreSQL 检查见下条。
  - Artifact 下载 API 跨账号错误边界（2026-11-21）：新增 `TestGetTaskArtifactContentHidesAnotherUsersArtifact`，用另一个 session principal 触发下载路由，断言服务收到的是另一个用户 ID，且服务返回 not found 时 HTTP 为 404、不包含 Artifact 正文；应用层 `TestOpenTaskArtifactContentRequiresVisibleVerifiedArtifact` 已验证其他用户不能打开该内容。`go test -count=1 ./internal/api -run '^TestGetTaskArtifactContent(HidesAnotherUsersArtifact|StreamsVerifiedAttachment)$'` 通过。该窄 API 用例使用服务桩；真实 Artifact 下载联验见下条。
  - Conversation、Message、Task、事件与 Artifact 跨账号 API/PostgreSQL 联验（2026-11-21）：扩展 `TestTaskWorkspacePublishedArtifactPersistsAcrossPostgresConnectionRestartForOwner`，两个账号分别通过真实 `/api/auth/login` 与 `CredentialService` 获得 session Cookie，再访问同一个 `api.New`、真实 Conversation/Task Service、PostgreSQL 和 Workspace Artifact 内容存储。所有者读取 Conversation、消息、Task 均返回 200，并下载到与原字节一致的 Artifact 正文；另一账号读取同一 Conversation、消息、Task、事件流及 Artifact 均返回 404，所有响应都不包含私有消息或 Artifact 正文。`cd server && go test -count=1 -timeout=4m ./internal/store/postgres -run '^TestTaskWorkspacePublishedArtifactPersistsAcrossPostgresConnectionRestartForOwner$'` 通过（最终复验 1.573 秒）。覆盖账号密码登录、数据库 session token 校验及 API→应用→PostgreSQL 隔离；未覆盖 PC/iOS 双账号 UI 或生产部署，T07 继续进行中。
  - 长会话 DSH Runtime 复验（2026-11-20）：实际 Node account Runtime 配合合成 Provider，通过 `TestConversationSummaryDSHMainAndSideChatsContinueAfterLongHistoryRestart`。主会话与两个旁聊完成 10 次摘要压缩；主会话新增 840 条历史、超过 8 MiB 后杀停并重建 Runtime，恢复到最新摘要并继续路由；授权历史搜索返回唯一标记，分页仍能读取全部 840 条原始消息。定向 PostgreSQL 集成测试通过（31.61 秒）。它验证服务端压缩/恢复/原始历史可读性和工具检索边界；历史搜索由测试直接调用 Tool Broker，尚未验证真实模型/DSH 自动决定何时搜索，也未从 PC/iOS UI 询问并核对答案，因此 T07 继续进行中。
  - 长会话 DSH 工具调用闭环（2026-11-21）：扩展同一用例，让实际 Node account Runtime 执行一个受支持的 DSH Task；合成模型先请求授权的原始历史搜索，收到包含归档唯一标记的工具结果后再完成 Task。修正测试夹具为持久化 Agent 实例服务，保留真实检查点身份校验；`go test -count=1 ./internal/worker -run '^TestConversationSummaryDSHMainAndSideChatsContinueAfterLongHistoryRestart$'` 通过（35.529 秒）。这证明重启后的摘要上下文、DSH 工具调用、工具结果回灌及 Task 完成链路贯通；模型输出由合成 Provider 预设，尚未验证真实模型判断何时搜索或从 PC/iOS UI 询问，因此 T07 继续进行中。
  - 前序回执与并发路由回归复核（2026-11-21）：`Assistant.test.tsx` 中“前序回执未知时提交后续消息”和“清除旧提示”两项通过；PostgreSQL `TestConversationRouteAppliesIndependentReplyWhilePredecessorIsProcessing`、`TestConversationRouteAppliesIndependentTaskWhilePredecessorIsProcessing` 均通过。旧提示修复只覆盖新版本客户端；未部署服务器/客户端，因此线上是否已生效未验证。
  - iOS 对话 UI 回归（2026-11-21）：在 iOS 27.2 Simulator 以 `xcodebuild test -project Morde.xcodeproj -scheme Morde -destination 'platform=iOS Simulator,id=C29692A5-D24C-47DF-A6EA-4D103FCD3D6E'` 运行三项 UI 用例，均通过：两个 Task 活跃时仍可提交独立消息、离线发送后重启并由用户显式重试、连续发送两条纯文字。自动化使用本地合成 fixture/URLProtocol；不核对服务端数据库只受理一次，也未完成 PC+iOS 同账号两端同时在线联验，T07 继续进行中。
  - iOS 多会话与账号隔离 UI 回归（2026-11-21）：同一 iOS 27.2 Simulator 运行 `testMordeChatDraftAndAttachmentsStayWithTheirConversation`、`testMordeChatDraftIsClearedWhenAccountChanges`、`testMordeEventChainSwitchingAccountsDoesNotShowPreviousAccountData`，3/3 通过（156.823 秒）。覆盖主/旁聊草稿及附件归属、账号 A 未发送草稿在切到账号 B 后不可见，以及账号 A 延迟事件在切换账号后不进入账号 B 事件链。使用本地合成 fixture，不替代真实服务端跨账号授权/数据隔离测试；多会话跨端 UI 与真实网络环境故障仍待完成；另见下方传输故障 fixture 联验。
  - iOS C10 真实 API/临时数据库复验（2026-11-21）：在一次性 PostgreSQL 容器、本机真实 HTTP handlers/路由 Worker 与合成模型上重跑 `testC10PortalMessagePersistsAcrossAppRestart`，1/1 通过（65.137 秒）；`TestConversationBrowserAcceptance` 的数据库最终断言通过（161.741 秒）。iOS UI 登录后发出首条消息并退出，第二安装凭证经真实 `/api/auth/client-login` 读取同一会话、发送独立消息；服务端生成回复后重启 iOS，原消息、第二条消息、状态和回复卡均恢复且各出现一次。此项不是断网后显式重试，因此 iOS 重试的服务端恰好一次仍未验收；未使用真实 Provider，也未包含 PC 同时在线。
  - iOS 503 后重启显式重试及 PostgreSQL 回执计数（2026-11-21）：以 `MORDE_BROWSER_MESSAGE_RECEIPT_RETRY=1`、`MORDE_BROWSER_MESSAGE_EXPLICIT_RETRY=1` 启动本机真实 HTTP handlers、路由 Worker 与一次性 PostgreSQL；iOS 27.2 Simulator 用 `testC10PortalMessagePersistsAcrossAppRestart` 重启应用并显式重试，1/1 通过（30.992 秒）。首个 POST 在服务端受理前返回 503；重启后用户点击前 `post_attempts=1`，点击后共 2 次尝试、复用原 `client_message_id`，其中只有重试受理；回执对账 GET 大于 0，消息气泡按该 ID 出现一次。`TestConversationBrowserAcceptance` 通过（75.31 秒），PostgreSQL 最终断言对应 route receipt 恰好一条。此用例覆盖服务端 503/重启/显式重试，不等同于真实网络断开；iOS 真实网络不可达的 UI 恢复目前仍由 `URLProtocol` 合成测试覆盖，未与服务端数据库串联。未使用真实 Provider。
  - iOS 客户端断网后显式重试与 PostgreSQL 受理计数（2026-11-21）：首次 iOS 进程通过 DEBUG transport failure fixture 对 `/api/conversations/{id}/messages` 返回 `URLError.notConnectedToInternet`，确保请求未离开客户端；登录、重启后的历史与 receipt 对账均连接本机真实 HTTP handlers、路由 Worker 和临时 PostgreSQL。iOS 27.2 Simulator 的 `testC10PortalMessagePersistsAcrossAppRestart` 1/1 通过（30.853 秒）：首次离线消息在关闭/重启前后均未由客户端自动 POST，明确点按重试后状态为“已发送”，沿用同一 `client_message_id`，消息气泡只有一份；`TestConversationBrowserAcceptance` 通过（115.022 秒），服务器仅看到显式重试的一次 POST，PostgreSQL 对应 route receipt 恰好一条。证明传输错误返回客户端后的重启恢复、无自动重发与本地服务端恰好一次受理；网络本身由 fixture 注入，未验证真实系统断网/恢复、真实 Provider 或远端服务。

  - PC 白屏与消息提示复验（2026-11-21）：定位认证态启动时旧主视图默认为 `web`，登录/离线页被隐藏；Portal `WebContentsView` 尚未挂载或导航失败时，壳层会空白。现增加 `connecting` 状态和 20 秒加载上限，失败时显示可重试离线页并保留有效认证。source-built `morde-pc` 以独立 profile 登录本机 HTTP handlers + 临时 PostgreSQL 合成账号，连续发送两条消息并分别收到回复；对话 UI 未显示旧的“前一条消息尚未确认”提示。`TestConversationBrowserAcceptance` 通过（155.29 秒）；`morde-main-path`/`morde-main-recovery` 61/61、`morde-core`/Portal bridge/renderer 36/36 通过，`npm run build`、`npm run typecheck`、`npm run build:portal` 通过。未使用真实 Provider；仅修复本地源码，用户当前已打包 app 和远端尚未更新。T07 因 iOS/跨端、多账号隔离及超上下文完整验收仍进行中。
  - 前序并发与账号隔离复测（2026-11-21）：Portal 的“回执未知时提交后续消息”和“旧提示不显示/清理”2/2 通过；服务端 Worker 验证首个分类阻塞时独立路由仍完成（1/1）；本机 PostgreSQL 的前序仍为 `processing` 时独立回复/新 Task 两项通过（2/2），消息回执 Owner 隔离用例通过（1/1）；PC 页面切换账号时不恢复另一账号会话、账号身份不匹配时保留本地消息两项通过（2/2）。PC 并发夹具的前两次操作因漏发结束标记/遗漏独立消息未通过；随后已按完整场景重跑并通过，详见下条。实际 PC+iOS 跨端和 Task/事件/产物双账号 HTTP 验收仍未完成。
  - PC 四条相同消息并发复测（2026-11-21）：前一次使用的 `morde-pc.app` 包内资源比 Portal 源码旧约 11 小时；旧包把验收夹具中禁用 MCP 的 `GET /api/mcp/connectors` 503 显示成对话网络提示。按当前仓库重建 arm64 PC 包后，同一 MCP 503 仅显示工具连接局部状态，对话不再出现“网络异常，部分对话数据暂时无法读取”或“前一条消息尚未确认，请先核对后重试”。在实际 `morde-pc` 独立 profile 提交四条相同任务文本、一条独立请求和一条状态追问；UI 收到独立请求和根任务的合成回复，状态追问显示“该任务正在执行”。本地 PostgreSQL 最终断言确认四条重复消息指向同一根、重复项没有独立 Task、独立请求有不同 Task、状态追问没有创建 Task，且根任务结果只发布一次。修正验收夹具挂载空共享主机服务，避免电脑面板读到不必要的 404。`npm run dist:dir -- --arm64` 构建通过；`MORDE_BROWSER_CROSS_CLIENT=1 MORDE_BROWSER_DUPLICATE_EXPECTED=1 go test -count=1 -timeout=8m -v ./internal/store/postgres -run '^TestConversationBrowserAcceptance$'` 通过（测试 178.19 秒，包 179.773 秒）。这是本地合成模型/临时数据库验收，不代表真实 Provider、iOS 或测试站已验收。
  - PC 旧前序提示展示路径补漏（2026-11-21）：普通消息气泡已过滤该旧提示，但“补充任务”状态行直接展示 outbox 错误；用旧文案模拟消息接口错误后，回归先证实该状态行仍会显示。现增加共享 `messageOutboxErrorForDisplay`，用于历史 outbox 清理、普通消息和补充任务状态行的统一脱敏展示。前序文案与后续消息回归 3/3、`Assistant.test.tsx` 208/208、Portal TypeScript 检查通过；改动文件 ESLint 通过。完整 ESLint 在 `Assistant.tsx` 其他位置因既有 React ref/render 规则报 5 个错误及 1 个依赖警告。实际 `morde-pc` 当前指向已停止的 `127.0.0.1:50324` 本机夹具，本次无法从客户端新验收该补充任务分支；此前同会话并发/不显示旧提示的实际客户端验收仍有效，但不覆盖此分支。真实 Provider 和线上旧安装包未验证，T07 继续进行中。
  - DSH 同会话分类并发回归（2026-11-21）：新增 `account runtime classifies independent conversation messages concurrently`，向真实 Node Account Runtime 同时提交 4 条独立消息，并阻塞所有模型响应直到 4 个不同协调会话均已进入模型调用；4 条最终分别完成且无协议错误。`node --test --test-name-pattern='account runtime classifies independent conversation messages concurrently' test/runtime.test.mjs` 通过（1/1）。该项证明 DSH Runtime 支持同会话多个独立分类会话并行，不代表 PostgreSQL 路由、PC/iOS UI 或真实 Provider 负载已端到端验证；生产容量仍受每账号运行槽位约束。
  - 同会话并发路由栅栏修复（2026-11-21）：真实 PostgreSQL + Node DSH 集成复测发现，四条独立消息虽并行领取，却可能在模型派发前因为另一条路由把候选从 `pending` 改成 `processing` 而触发 `authorized route candidates changed`，最终错误为 `routing_context_unavailable`。现按稳定消息身份/正文/范围/授权验证模型派发候选；事务提交仍检查完整快照并在必要时重分类。模型未派发时才可释放预留并回队，`dispatched/unknown` 不重放。分类输入、授权栅栏、安全恢复与 Worker 定向测试通过；`TestConversationSummaryDSHMainAndSideChatsContinueAfterLongHistoryRestart` 在四个 DSH 模型调用同时抵达屏障后放行，四条消息均完成（本轮复跑 23.392 秒）。Portal 前序提示回归本轮 4/4、DSH Runtime 同会话并发回归 1/1。该后端竞态已修复；实际 PC UI 对这次栅栏补丁未复验，真实 Provider、线上部署和 iOS/PC 跨端仍未完成，T07 保持进行中。

- [ ] **T08（阻塞）验收指定云端电脑、常用工具和 DSH 子 Agent**
  - 修改：现有 DSH coordinator、授权工具、搜索来源和主/子 Agent 汇总路径；优先复用已接入 DSH 插件及 provider。
  - 行为：普通文字问答不启动云电脑；指定云电脑操作只使用已授权目标；搜索给可核查来源；终端与文件访问遵守用户授权；主 Agent 仅在互不冲突时并行委派，依据子 Agent 的真实结果汇总。工具/Host 不可用或授权失效时报告真实状态，不切换目标、不虚报完成。
  - 验收：在实际 `morde-pc` 和获准测试环境完成一次可观察 GUI 操作、一次终端命令、一次搜索来源和两个 DSH 子 Agent 并行任务；再覆盖 Host 离线/授权撤销。测试站当前旧二进制与 Host 创建失败须先更新至包含本地修复的构建后复验；未部署前保持阻塞，不得远程改库或改权限。
  - 最新阻塞证据（2026-11-20）：账号所有者确认启用云端执行后，搜索消息和云端电脑观察消息仍以 `model_unavailable` 失败。DeepSeek Flash 同一连接的文字/工具探测已通过；失败消息没有对应的 route 模型调用/用量记录，服务日志只记录 `failure_category=unknown`。只读核对确认消息选择的模型连接存在、归属匹配、检查通过，服务实例也匹配；因此失败发生在计量模型调用之前，但当前日志不足以区分路由预检的具体失败点。本地路由失败日志已补充不含原始错误正文的 `failure_code`，定向单测通过；测试站未部署此改动，远端日志仍无法显示该码。未修改远端服务、配置、数据库或主机文件。
  - 新增本地修复（2026-11-20）：若 `ResolveConversationRouteModel` 因路由租约在模型派发前到期而返回 context error，且父 Worker 仍在运行，则延后 2 秒重新入队，不将消息写成 `model_unavailable`。`TestConversationRouteWorkerDefersExpiredLeaseBeforeModelResolution` 通过；单独运行 `go test -count=1 ./internal/worker` 通过（35.805 秒）。此修复降低了过期租约导致的假失败，但仍未证明它就是测试站的真实根因；需在本地构建部署后复验，当前不触及远端。
  - 本地回归进度：`EnsureDefaultHost` 重复调用此前把已绑定的稳定 Workspace 当作新挂载检查，导致 default Host 无法复用；现在先验证现有绑定与稳定 Workspace 映射，仅无活动绑定时才走新绑定检查，并拒绝请求/绑定 Workspace 不匹配。Host 并发复用、绑定冲突、额度和 mismatch 定向 PostgreSQL 用例通过；测试站旧二进制及 `model_unavailable` 路由问题仍阻塞真实 Host/搜索/子 Agent 验收。
  - PC 回执未知后显式重试（2026-11-20）：source-built `morde-pc` 连接本地真实 HTTP handlers 与临时 PostgreSQL；测试代理先让首个消息 POST 完成服务端受理并提交，再对客户端返回 503。客户端显示“重新发送这条消息”；点击一次后通过同一 `client_message_id` 对账读取原消息和回复，没有第二次 POST。夹具计数为 `post_attempts=1`、`reconciliation_checks=1`、`server_accepted=true`，集成测试再核对数据库中恰好一条对应 route receipt。`TestConversationBrowserAcceptance` 通过（432.039 秒）。模型为合成替身；此项证明本地客户端到本地服务端/数据库的回执恢复幂等，不代表测试站、真实模型或 iOS 已验收。
  - PC 回执丢失后自动核对（2026-11-20）：纯文字消息 POST 若因 5xx/网络中断返回未知，客户端用原 `client_message_id` 立即 GET 回执；找到且内容/会话/角色匹配时继续按已受理流程处理，不重复 POST，不显示旧的“前一条消息尚未确认”错误。新 Vitest 用例通过（1/1）；source-built `morde-pc` 对接本地真实 HTTP handlers、临时 PostgreSQL 和合成模型，实际 UI 显示最终任务回复，夹具确认 `post_attempts=1`、`reconciliation_checks=1`、`server_accepted=true`，数据库确认只写入一条 route receipt；`TestConversationBrowserAcceptance` 通过（469.37 秒）。`npm run build` 通过。`Assistant.test.tsx` 全量为 201/205，另有 4 个消息恢复/附件场景失败，尚未定位与本次改动的关系；不据此标记 T07 完成。此项不代表真实模型、测试站或 iOS 已验收。
  - 消息失败状态回归（2026-11-20）：修复本地 outbox 已保存 `error` 却未显示在气泡中的缺口；附件已受理但上传失败时，状态一并显示错误详情。旧的前序回执提示仍会被清理且不会通过通用错误展示复现。四个先前失败的错误/附件场景现通过，`Assistant.test.tsx` 全量 205/205、Portal `npm run typecheck`、`npm run build:pc` 通过。source-built `morde-pc` 使用隔离 profile 登录 localhost 合成服务，实际触发附件上传 503，确认 UI 显示“消息已发送，附件等待处理”和可读错误，旧前序提示未出现。该次 PC 手工验收停在失败状态展示；附件恢复后的最终 PostgreSQL 回执断言未完成，因此 T07 仍进行中。
  - 附件分块续传修复（2026-11-20）：客户端先校验上传回执的状态、分块数和已收分块序号，再跳过已被服务端确认的分块；无效回执时保留本地消息并给出可读错误。回归覆盖第一块成功、第二块中断、重试仅上传缺失块并完成关联；`Assistant.test.tsx` 全量 206/206、Portal 类型检查和 `npm run build:pc` 通过。首次 PC 实测因无障碍填入大文本超时未提交；2026-11-21 已按下方独立隔离的 source-built PC 续传记录完成真实 UI 与 PostgreSQL 复验。
  - PC 长消息分块上传重启续传（2026-11-21）：实际 source-built `morde-pc` 登录本地合成账号，粘贴约 39 KiB 的确定性 UTF-8 文本并发送。夹具确认首块已收、第二块被合成 503 中断，PC 显示“重新发送这条消息”。关闭并以相同隔离 profile 重启后，原消息和显式重试入口恢复；重启到用户点击期间，HTTP 记录没有再次请求 message-upload 路径。开放本地夹具后点击显式重试，客户端复用同一 `client_message_id`，先读取上传进度，仅提交缺失块 1、2，再 commit；夹具状态为 `resume_initialization_seen=true`、`first_chunk_repeated_after_restart=false`、`missing_chunks_recovered=true`、`committed=true`。`TestConversationBrowserAcceptance` 最终 PostgreSQL 断言该 ID 仅有一条完整且正文精确匹配的用户消息；测试通过（235.55 秒，包 236.917 秒）。该场景专测长消息分块提交，不启动对话路由 Worker，也不证明真实模型回复、附件内容或生产部署；T07 仍因真实模型/跨账号等覆盖未完成而进行中。
  - PC 附件分块上传重启续传（2026-11-21）：source-built `morde-pc` 登录本地合成账号，附加 5.2 MiB 合成 DOCX 并提交；本地 HTTP 夹具在分块 1 返回 503。关闭并以相同隔离 profile 重启后，消息及“重试附件”入口恢复，重启期间没有自动重传；用户点击后，客户端查询原 upload ID，仅补传缺失分块并成功 commit，UI 显示附件下载入口。夹具确认 `first_chunk_repeated_after_restart=false`、`missing_chunk_recovered=true`、`committed=true`；临时 PostgreSQL 核对上传状态为 `committed` 且仅有一条原文件记录。`TestConversationBrowserAcceptance` 通过（测试 153.37 秒，包 154.70 秒）。为匹配个人 DOCX 上传不单独落库衍生上传的路径，修正了测试中误沿用 JPEG 衍生场景的数量断言。此项验证本地 PC 上传恢复和持久化，不证明模型读取附件内容、真实 Provider 或生产部署；T07 仍进行中。
  - PC 纯文字双消息复验（2026-11-20）：source-built `morde-pc` 用独立 profile 登录本地合成账号，经真实 HTTP handlers 和临时 PostgreSQL 连续提交两条独立消息；UI 分别显示两条合成模型回复，`TestConversationBrowserAcceptance` 通过（116.60 秒），最终数据库消息计数断言通过。页面出现的通用网络提示后来在 2026-11-21 复测中定位为本机打包资源落后于源码；当前源码重建包在相同 MCP 503 夹具下不再显示该对话提示。仅验证本地合成模型，不覆盖远端/真实模型；附件恢复、iOS 实际客户端、跨端和多账号隔离仍未完成，T07 保持进行中。
  - 云端电脑实际联验（2026-11-20）：沿用客户端当前登录会话，在同一既有测试 Host 上确认桌面可显示浏览器、主机文件可列出 `/home/gem/workspace`，接管和归还控制权均完成；终端仍黑屏、VS Code 仍白屏。本轮未运行终端命令、未上传或修改远端文件。按用户授权仅重置测试站 `admin` 密码哈希，回读确认目标记录匹配；没有记录明文密码，也未改其他账号、服务配置或 Host 权限。该结果不能证明搜索、DSH 子 Agent 并行或 Host 离线/撤权路径；测试站新构建发布后仍需按 T08 完整复验。
  - 实际客户端补验（2026-11-21，详见云端工作台 TODO T4）：在隔离本地 profile 登录测试站后，未接管时四入口禁用；接管后终端切换出 Web Terminal，主机文件显示工作目录 `/home/gem/workspace`，Workspace 仍独立。VS Code 仅确认目标选中，未能从辅助功能树确认 code-server/Monaco 页面，且窗口截图失败；未运行命令、未发送消息或改动云端文件。已归还控制权、退出登录并清理 profile。此结果仅说明入口路由状态，不算 T08 的终端命令、搜索、子 Agent 或共享文件验收；T08 仍阻塞。

- [x] **T09（完成）冻结合成评测套件和 Rubric**
  - 修改：新增最小评测输入/结果模块（建议从 `server/internal/evaluation/` 与受控 `testdata` 起步）；保存 suite 版本、预期行为、模型/Provider、Prompt/Skill、DSH、构建 SHA、工具权限和参数。真实用户内容不进入首版套件。
  - 行为：按设计方案 §8 原子条件评分；每项只有 `0 / 1 / N/A`，N/A 由用例定义并带依据；每个判定必须绑定输出片段或证据 ID。安全硬门禁失败不能被其他 rubric 项抵消。
  - 验收：覆盖普通回复、证据、复核、澄清、审批、回放、工具不可用与提示注入的固定合成用例；同一冻结配置重复运行得到可比较的分项结果和版本记录，不输出主观总分。
  - 完成证据（2026-11-21）：新增 `server/internal/evaluation/` 核心类型、显式 `data_class=synthetic` 的冻结 `suite-v1.json` 和结构验证；固定 7 项 Rubric、4 项硬门禁（意图约束、正确性/来源、副作用控制、证据/复核）与 8 个 P0 合成覆盖。Run 校验版本清单、N/A 依据和输出/证据锚点；相同冻结配置可用 `ComparisonKey` 归组并逐项比较；不包含总分。测试拒绝非合成套件、缺失覆盖、弱化硬门禁、伪造 N/A 和无效证据锚点。当前评测包包含 T10 盲测校验后，`go test -race -count=1 -cover ./internal/evaluation` 通过，覆盖率 88.9%；`go vet ./internal/evaluation`、`gofmt -d`、JSON 解析和空白检查通过。真实模型执行与评测批次持久化由后续 Judge/管理端实施接入。

- [ ] **T10（进行中）建立匿名盲测与分歧仲裁**
  - 依赖：T09。
  - 修改：评测结果导出/盲测包生成和受控评审记录；不暴露候选模型身份、Provider、时间或版本，映射表与审阅内容分开授权。
  - 行为：输出随机排序并匿名编号；至少两名未参与实现的审阅者独立按同一 Rubric 评分；分歧连到用例和证据并经仲裁，不以模型印象替代证据。
  - 验收：交换匿名编号后评分不受标签影响；随机化可复核；审阅者身份与实现者不重叠；P0/安全分歧未仲裁时批次保持未通过。
  - 本地进度（2026-11-21）：新增 `BuildBlindPack`，以加密随机标签和逐用例随机顺序输出盲测内容，`BlindMapping` 单独返回并保留位置，可用于复核随机化；盲测包不包含模型、Provider、构建/Prompt/Skill/DSH 版本或运行时间，并携带合成证据摘录和用例 N/A 依据，但不暴露候选目标分数。`ValidateBlindReview` 和 `ValidateBlindPanel` 检查证据锚点、每位候选至少两名不同审阅者、审阅者/仲裁者不属于实现者、评分分歧先仲裁。回归覆盖标签互换不影响评分、元数据不泄漏、顺序可复核及未仲裁失败。映射/审阅记录尚未接入分权存储和权限控制，因此 T10 保持进行中。`go test -race -count=1 -cover ./internal/evaluation` 通过，覆盖率 88.9%；`go vet ./internal/evaluation` 和 `gofmt -d` 通过。
  - 持久化与身份隔离进度（2026-11-21）：新增 `00222_blind_evaluation_persistence.sql`，分别保存匿名包、映射、隐藏 Run Manifest、实现者/审阅者/仲裁者名单、评分和仲裁；事务保证名单/FK 错误时不留下半套数据。新增 App 服务，只允许管理员创建与读取映射/Manifest；评审包读取要求已分配的真实用户 Session，身份从 `Principal.UserID` 取得，不能由请求自报；实现者和未分配审阅者无权读评审包。相同评分重试复用原记录，不同内容冲突；所有已分配审阅者完成各候选评分后才允许仲裁，未仲裁分歧保持未就绪。临时本机 PostgreSQL 定向测试（2/2）覆盖分权读取、身份绑定、事务回滚、幂等/冲突、未完成面板和仲裁后就绪。
  - HTTP API 与 OpenAPI 进度（2026-11-21）：新增管理员创建合成盲测批次、读取状态/映射/运行 Manifest，以及指定审阅者读取盲测包和本人已提交评分、指定仲裁者读取匿名评分、提交评分/仲裁的路由。审阅者和仲裁者身份只从 Session 推导；审阅者只能获取本人记录，仲裁者只看到匿名稳定标签、不见真实用户 ID 或评审时间；匿名包响应与管理员映射/版本信息分开。`TestBlindEvaluationRoutesSeparateReviewerAndAdminData`、`TestBlindEvaluationSubmissionsUseAuthenticatedIdentity` 通过；临时本机 PostgreSQL `go test -count=1 -timeout=3m ./internal/store/postgres -run '^TestBlindEvaluation'` 2/2 通过；Portal `npm run generate:api` 和 `npm run typecheck` 通过。
  - 评审与仲裁 UI 进度（2026-11-21）：新增受 RouteGuard 保护的独立评审/仲裁页面。每个 `0/1/N/A` 评分都要求理由和可核查证据；输出原文引用先校验唯一匹配，再换算 UTF-8 字节范围，N/A 由套件理由锚定；提交后从服务端记录恢复只读状态。仲裁页面只展示匿名评审标签，必须等全部评审提交且存在分歧才允许仲裁；仲裁结果回读时不暴露仲裁者 ID，并显示持久化评分和理由。新增本地 MSW 合成批次供 `VITE_MOCK_API=1` 浏览器验收，不连接测试服务器。`BlindEvaluationWorkspace.test.tsx` 3/3 通过，覆盖中文 UTF-8 引用、N/A 锚定、匿名意见、仲裁提交和已保存结果呈现；本机浏览器以合成批次实际提交一条评分和一条仲裁，页面分别显示已保存/最终仲裁内容；Portal `npm run typecheck`、`npm run build`、`go vet ./internal/api ./internal/app ./internal/store/postgres` 与 `git diff --check` 通过。
  - 管理员批次操作页进度（2026-11-21）：新增受 AdminGuard 保护的 `/admin/evaluations`，分页读取已通过审核的账号，选择互斥的实现者/至少两名审阅者/仲裁者，提交仓库冻结的合成 suite 和至少两份 Run JSON，并可读取批次状态、匿名映射及管理员专属 Manifest。页面明确禁止粘贴真实用户内容，服务端继续完整校验所有输入。`AdminEvaluationBatches.test.tsx` 2 项与导航测试 1 项共 3 项通过；Portal `npm run typecheck`、`npm run build` 通过；本机 `VITE_MOCK_API=1` 浏览器确认管理页和合成 suite 摘要，无测试站请求。真实模型 Run 采集仍未实现；浏览器未提交新批次，需管理员准备至少两份有效合成运行 JSON，T10 继续进行中。

- [ ] **T11（未开始）接入跨模型 Judge 与冻结放行门槛**
  - 依赖：T09、T10。
  - 修改：通过 Morde 模型网关的只读评估请求、结构校验和批次门禁；Judge 不启用工具，也不接触凭据或候选身份。
  - 行为：不同模型家族 Judge 为每项返回 `0 / 1 / N/A`、理由、证据 ID 和三态结论；记录校准状态。执行冻结阈值：P0/适用硬门禁 100%、Rubric/盲测/Judge 各自至少 95%、严重安全失败为 0、校准集一致率至少 90%；小样本、结构无效、同家族、未校准或 P0 分歧均阻断发布。不能看过结果后调整阈值。
  - 验收：覆盖同家族、证据缺失、无效结构、Judge 不可用、各类阈值边界、盲测与 Judge 分歧、注入内容；各类失败均记录为未通过/待校准，不能自动放行。

- [ ] **T12（未开始）提供脱敏管理端评测和纠正项视图**
  - 依赖：T09、T11、T13。
  - 修改：`server/openapi/portal.yaml`、Admin API/app/store/迁移和新管理页（沿用 `web/portal/src/routes/admin/` 结构）；不要扩展 `/api/admin/quality` 读取对话内容。
  - 行为：实现契约表中的批次列表/详情及纠正项读取；展示 suite/build/runtime/prompt/model 版本、样本数、P0/P1、三种独立通过率、分歧、失败证据摘要与纠正状态。只向管理员暴露授权的脱敏报告和合成用例结果。
  - 验收：OpenAPI、实现、迁移和 HTTP 集成测试一致；覆盖管理员权限、分页、空批次、错误响应及敏感字段不出响应；在本地后台实际打开批次和失败详情。

- [ ] **T13（未开始）实现第二次同类错误的纠正闭环**
  - 依赖：T01、T09。
  - 修改：Task 失败归因、最小持久化纠正记录及 admin 读取路径；复用 Task 证据，不复制原始用户正文、密钥或文件内容。
  - 行为：首次失败仅建候选 signature；相同 signature 在第二次独立运行出现时创建纠正项，包含两次证据引用、owner、回归用例和一种可验证产物（Lint、检查、测试或 DSH Skill）。通过回归与独立复核后才能关闭；只改 Prompt 文案不算修复。
  - 验收：同一 Task 的重试不算第二次独立运行；两次不同运行触发一次幂等纠正项；PII 不进入 signature；缺 owner/回归或未通过复核时不能关闭。

- [ ] **T14（未开始）完成整套验收矩阵与证据归档**
  - 依赖：T01–T13；未满足的外部测试环境条件标为阻塞，不能降级为通过。
  - 修改：实现并验证设计方案 §10 的全部用例，维护测试记录和下方映射；必要时修复对应任务，不以构建、单测或 CI 全绿替代产品端验收。
  - 验收：PC 使用实际 `morde-pc`，iOS 使用 Simulator/设备，管理后台在本地管理端验证；服务端 API/Store 使用单测、`httptest` 和 PostgreSQL 集成测试。每个用例归档 suite/build/runtime/model/权限、Task 与事件游标、结果和复测关联；P0 全通过、严重安全失败为 0，方可宣布首版达标。

- [x] **T15（完成）在 PC 对话入口提供账号云端执行开关**
  - 修改：`web/portal/src/routes/Assistant.tsx`、`web/portal/src/app/api.ts` 及 `Assistant.test.tsx`；沿用现有 `/api/execution`、`/api/execution/enable` 和 `ConfirmDialog`，不新增服务端授权接口。
  - 行为：执行开关关闭时明确显示普通对话仍可用、云端任务被门禁阻断，并给用户可选启用入口。确认前不发请求；启用时校验账号、状态、epoch 与响应；网络结果未知先刷新状态，重试复用同 epoch 的幂等键，不改变 Host、工具或 Workspace 授权。
  - 验收：组件测试覆盖 disabled 时普通消息可提交、确认前无启用请求、确认后只调用一次、重复点击、epoch 冲突和响应未知后状态刷新。用实际 `morde-pc` 操作开关；执行权限改变需由账号所有者明确确认。
  - 完成证据：`Assistant.test.tsx` 198/198 通过；在实际 `morde-pc` 中确认普通消息入口可用、确认卡前不启用，随后按账号所有者授权启用；启用后状态刷新为 enabled。未扩大 Host、工具或 Workspace 权限。

- [ ] **T16（待验证）新账号与服务端配置默认启用对话能力**
  - 修改：新增 PostgreSQL 迁移调整 `owner_execution_controls.state` 默认值；在 `server/internal/config/config.go` 为 Cloud DSH、内置搜索、Workspace 同步、项目上传和 Host Workspace 接入设置默认开启；更新 `deploy/single/portal.yaml` 并同步部署说明。保留显式 `false` 作为管理员配置覆盖，不批量改写已有账号状态。
  - 行为：新账号默认 `enabled`，服务端省略功能开关时默认启用上述能力；启动时仍校验 DSH/Guardian/搜索和隔离存储依赖，缺失即失败关闭。账号审核、Host/Workspace/工具授权、逐项审批、用户与数据隔离继续有效。路由决策默认并发 16；实际 DSH 执行资源上限为 4 个全局任务，单账号可使用全部 4 个槽位；搜索并发为 4。全局执行槽位是主机资源护栏，不作为用户能力授权门禁。
  - 验收：PostgreSQL 测试覆盖新注册账号默认 enabled、已有 disabled 状态保持不变；配置测试覆盖开关省略时默认开启及显式 false 覆盖；实际 PC 新账号流程确认聊天及已配置云端能力无需额外账号开关。主机发布及跨工具验收按 T08/T14 进行。
  - 进度：新增 `00212_enable_execution_for_new_accounts.sql` 与注册集成回归；`go test -count=1 -timeout=4m ./internal/store/postgres -run '^TestNewAccountExecutionDefaultsEnabledWithoutChangingExistingControls$'` 通过（2.093 秒），确认新账号默认 `enabled` 且不改写旧账号的 `disabled`。`go test -count=1 ./internal/config` 通过（1.481 秒），覆盖省略配置的默认开启和显式关闭；路由默认并发为 16，与 4 个 DSH 执行槽位分离。PC 本地 HTTP/PostgreSQL 验收夹具不再手动开启执行权限，而是在登录前断言初始状态；重建的实际 `morde-pc` 独立 profile 未显示执行启用提示，直接提交四条重复任务、一条独立任务和进度追问并收到合成回复。`TestConversationBrowserAcceptance` 通过（测试 139.16 秒，包 139.876 秒）；数据库断言确认去重与独立 Task 行为。该账号由本地测试夹具创建，注册服务本身由上述 PostgreSQL 用例覆盖。真实 Linux DSH/Guardian/搜索/固定容量卷部署验收尚未完成；未改测试站或远端数据库。
  - 部署模板与默认并发复核（2026-11-21）：`cd server && go test -count=1 ./internal/config`、注册默认状态 PostgreSQL 定向用例通过；`python3 deploy/single/test-portal-deployment.py` 24/24 通过；`docker compose -f deploy/single/docker-compose.yml config --quiet` 使用临时合成环境变量通过。调整 `TestTaskWorkerExecutesSameConversationTasksConcurrentlyAndSeparately`，移除测试前手动启用账号，改为断言新账号初始状态为 `enabled` 后启动两个同会话独立 Task；`cd server && go test -count=1 -timeout=2m ./internal/worker -run '^TestTaskWorkerExecutesSameConversationTasksConcurrentlyAndSeparately$'` 通过（1.562 秒）。这些本地检查不证明 DSH/Guardian/搜索/Workspace 卷已在 Linux 实际启动；没有因此修改或连接远端主机，T16 保持待验证。
  - 默认开关回归复核（2026-11-21）：复跑 `go test -count=1 ./internal/config` 通过（1.504 秒）；补充 MCP 显式 `false` 覆盖后，`TestLoadFeatureRuntimeGatesDefaultEnabledAndCanBeDisabled` 通过（0.593 秒）。使用一次性 localhost PostgreSQL 容器复跑 `TestNewAccountExecutionDefaultsEnabledWithoutChangingExistingControls` 通过（2.176 秒），验证新注册账号默认 `enabled` 且原有 `disabled` 不变；`python3 deploy/single/test-portal-deployment.py` 24/24 通过。临时数据库容器已停止。这些证据覆盖配置解析、账号状态和部署模板，不代表 DSH、Guardian、搜索或 Workspace 同步节点已在 Linux 实际启动；T16 继续待验证。
  - 并发限制复核（2026-11-21）：发现消息路由已与 DSH 执行并发分离（16 路），但每账号 2 个 Task 的默认槽位会让同一账号在全局 4 槽尚有容量时排队。已将默认账号并发提高到全局上限 4，并更新部署模板；补充同一会话单账号 4 个 Task 同时执行的 PostgreSQL/Worker 回归，防止回到两槽串行。`cd server && CH_TEST_DATABASE_URL=<本机临时 PostgreSQL> go test -count=1 -timeout=2m ./internal/worker -run '^TestTaskWorkerExecutesSameConversationTasksConcurrentlyAndSeparately$'` 通过（2.340 秒）；`go test -count=1 ./internal/config` 通过（1.696 秒）；`python3 deploy/single/test-portal-deployment.py` 24/24 通过；`cd web/portal && npm test -- src/routes/Assistant.test.tsx -t predecessor` 3/3 通过，覆盖前序回执未知时提交后续消息且不显示旧提示、恢复时清除已持久化的旧提示。临时 PostgreSQL 已停止。保留全局 4 个执行槽作为 4 vCPU / 8 GiB 默认部署的资源护栏。

## 用例到任务映射

| 设计用例 | 实施任务 |
|---|---|
| AIC-H-01 | T04、T07 |
| AIC-H-02 | T01、T03、T04 |
| AIC-H-03 | T02、T03、T14 |
| AIC-H-04 | T05 |
| AIC-H-05 | T06 |
| AIC-H-06 | T13 |
| AIC-H-07 | T01、T04 |
| AIC-H-08 | T08 |
| AIC-C-01 | T07 |
| AIC-C-02 | T04、T07 |
| AIC-C-03 | T07 |
| AIC-C-04 | T08 |
| AIC-C-05 | T08 |
| AIC-C-06 | T05、T07 |
| AIC-C-07 | T07 |
| AIC-E-01 | T09 |
| AIC-E-02 | T10 |
| AIC-E-03 | T11 |
| AIC-E-04 | T10、T11 |
| AIC-E-05 | T11 |
| AIC-E-06 | T09、T11 |
| AIC-E-07 | T09、T11 |
| AIC-PC-01 | T15、T07、T08 |

## 执行顺序与验证命令

建议顺序：T00 → T01 → T02 → T03 → T04；T05/T06/T15 可与客户端和服务端边界明确的任务并行；T07/T08 完成功能闭环后推进 T09 → T10 → T11 → T12/T13 → T14。凡涉及数据库的任务均依赖 T00；接口、迁移或状态合同变更必须同一变更更新 `server/openapi/portal.yaml`、实现和测试。

按受影响层验证，不用单一 CI 结果替代端到端验收：

- 服务端：`cd server && make lint`、`make test race cover`；数据库变更运行 `CH_TEST_DATABASE_URL=… make it`；契约/生成变更按服务端规范运行 `make generate` 并确认生成文件一致。
- PC：运行 `web/portal` 相关测试和构建；涉及 PC 交互时启动并操作实际 `morde-pc`，不以独立浏览器验收替代。
- iOS：运行对应工程 UI/单元测试，并在 iOS Simulator/设备操作实际会话与详情流程。
- 管理端：运行 Admin API/UI 回归，并在本地管理 Web 验证批次、失败详情、权限和脱敏。

每个任务完成时，在任务下追加实际命令/用例、结果、构建与证据链接；失败、未执行和环境阻塞都如实记录，不把计划状态改成通过。
