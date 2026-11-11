# Morde AI 对话上下文压缩与长期记忆实施任务 TODO

日期：2026-11-10。来源：[Morde AI 对话上下文压缩与长期记忆设计方案](<../Morde AI对话上下文压缩与长期记忆设计方案.md>)。本清单只规划实施；只有对应实现和验收完成后才勾选。任务编号是本清单内部编号，不覆盖其他清单的状态。

## 修改边界与现状

- 一个账号在同一服务实例中有一个当前个人主会话，也可以有多个旁聊；每个聊天独立拥有可持续更新的摘要与原始记录。压缩不创建用户可见会话、不改变主会话指针，消息仍先持久接收，独立任务仍可并发。
- 继续使用 DSH 作为唯一 Agent Runtime，模型走 Morde 模型网关。当前逐消息协调器、每个 Task 的 DSH Session、鉴权、审批、检查点、工具 Broker、记忆对象与操作账本保留；新增的短期压缩 Session 只是同一聊天的内部维护上下文。
- 属于 DSH 的能力优先使用和迁移 [本地上游源码](../../project/deepseek-harness/)及其测试；能够确认同源且兼容的上游包可直接引用，确需源码构建或修改时迁入最小必要源码。Morde 权限、网关和账本适配尽量留在 provider/Broker/Worker 中；必须修改上游源码时单独记录补丁，以便后续比较和同步 DSH 更新。具体规则见[设计方案 4.1 节](<../Morde AI对话上下文压缩与长期记忆设计方案.md#41-dsh-源码复用与后续同步>)。
- 会话摘要是有损派生数据，不是原始消息、操作回执或长期记忆；项目聊天必须按项目和 actor 授权读取。长期记忆沿用现有个人对象与授权，摘要不自动写入记忆。已有长期记忆功能继续由 [Agent Runtime 实施清单 T09](<./Morde-Agent-Runtime与AI能力实施任务todo清单文档.md>) 记录，本清单负责它与摘要、删除和恢复的交界。
- 现有路由上下文只截取近期消息，Task 初始上下文也有大小限制，均不等于已压缩。现有跨会话检索排除当前会话；需要单独接入当前会话原文检索。既有聊天历史需要按授权分页建立摘要，不能把当前截断结果当作全部历史。
- 本期不放宽 Task 工具结果检查点校验，不接入工具结果裁剪、图片省略或手动压缩命令。长时间运行的单个 Task 自身事件日志上限仍需后续单独处理；本清单保证持续聊天不会依赖一个无限增长的 DSH Session。
- 仅本地读写，不外发或发送文件；验证优先使用本机合成消息、模型替身和隔离数据库。与现有 [对话及任务清单](<./Morde AI助手对话与任务执行体验实施任务todo清单文档.md>)、[公共约定](./00-总览与公共约定.md)发生冲突时先记录并统一合同。修改状态机、跨模块接口、表结构或 HTTP API 时，同一变更同步权威合同、实现和测试。

## 关键接口与数据变化

下表是目标合同，除明确标为现有的名称外，具体类型、字段和拟新增工具名称由实施时确定并同步公共约定；不预设新的公开 HTTP 路径。

| 接口或数据 | 目标变化及调用方 |
| --- | --- |
| DSH Runtime ↔ Worker 内部 IPC（扩展） | 增加受服务端控制的压缩维护上下文、稳定 `compaction_id`、用途、会话/actor、租约和授权版本、检查点身份、模型容量及可安全分类的溢出错误；Runtime、Worker 两端一起更新严格字段校验。压缩上下文无工具，不伪造 Task ID 或放宽普通 Task 的检查点规则。 |
| 模型网关压缩调用（拟新增内部合同） | 请求绑定压缩操作 ID、调用 ID、会话、actor、输入摘要及来源、模型和授权围栏；返回模型结果、实际用量及安全错误类别。复用预留、结算、预算和未知结果对账；调用记录与现有以消息 ID 为键、要求路由决定的 `conversation_route_model_calls` 分离。 |
| 模型容量（扩展网关与 Runtime 内部合同） | 网关提供所选模型可信窗口及来源，Runtime 的 DSH adapter 提供 `contextWindow`；没有可靠窗口时使用已验证的保守配置，否则显式失败。token 与网关 UTF-8 字节预算分别计算、分别校验。 |
| 摘要、来源和压缩操作 Store（拟新增） | 以服务实例、owner、Conversation、作用域和适用 actor/权限版本定位；保存密文摘要、版本和前驱、来源清单及其证明、操作和检查点、状态/模型调用关联。提供按权限读取、来源失效、租约与基准版本 CAS 提交；冲突或权限变化不覆盖旧有效摘要。 |
| `LoadConversationRouteSnapshot` / `ApplyConversationRoute`（现有，扩展） | 分流输入增加安全可用的摘要版本、来源和权限证明及未覆盖的近期原文；调用方在模型派发前及应用结果时重验。来源必须满足该消息的因果与项目可见性条件，不能只凭服务端序号判断。 |
| Task 初始上下文及恢复（现有，扩展） | Task 创建时固定路由可用的摘要版本、来源和授权证明，并与未覆盖原文一起写入其上下文合同；恢复时重验项目/记忆/来源，不能因后续摘要变化或撤权读取不安全内容。 |
| 当前会话原文检索工具（拟新增） | 由现有 DSH Task 经 Broker 调用，入参为关键词和有界数量，不接受任意会话 ID；按 Task 当前会话、身份、项目成员和抑制规则读取。返回消息 ID、时间、有界原文片段及是否部分结果；协调器需要原文时转入现有新 Task 路径。现有跨个人会话检索合同不变。 |
| 消息处理结果与客户端（按需扩展现有合同） | 压缩等待、不可压缩超长输入或结果未知时，通过现有消息/任务处理状态表达安全、可理解的结果；如需新增状态或原因，同步 OpenAPI、Portal/Workbench 生成类型及受影响的 PC、iOS 调用方。已受理消息不重复创建，安全重试沿用已有围栏。 |

## 实施任务

- [x] **T01（完成）复用 DSH 源码并验证压缩插件组合**
  - 修改：`runtime/morde-cloud-runtime/package.json`、`package-lock.json`、`src/engine.mjs`、`Dockerfile`、`test/compaction.test.mjs` 和 `vendor/deepseek-harness/`。Runtime 已接入 DSH Agent、会话、投影、检查点、MCP 和工具执行；本项只补压缩/计量组件，不增加第二套 Agent Runtime。
  - 将 `packages/compaction/compaction-basic/` 的原始 TypeScript、包清单、README 和 MIT 许可证以未修改快照迁入 `runtime/morde-cloud-runtime/vendor/deepseek-harness/`。来源为 DSH checkout `fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159`；`dsh-v0.2.2-rc.1` 指向 `66491b05e3f05ad35bcdd4f6cd7ed26041d9e3db`，快照源文件与该标签逐文件一致。`dsh-compaction`、`dsh-token-meter` 和 Runtime 其余 DSH 包以 `0.2.2-rc.1` 固定在 manifest/锁文件，均核对为 MIT；`esbuild` 是固定版本开发依赖，用于打包迁入的上游 TypeScript，生成目录不入 Git。
  - Runtime 装配上游 `TokenMeter` 与 `BasicCompactionEngine`，显式 `auto: false`；保留 DSH 范围选择、计量、投影和事件实现。构建不依赖 `project/deepseek-harness/`；Docker 构建阶段编译并跑完整测试，最终镜像只包含生成 bundle，不包含上游开发仓库。上游默认容量余量等配置不视为生产配置。
  - 合成适配器用例验证 `compactNow()` 产生 start/summary/end 事件，原始用户事件保持追加式历史，摘要进入消息投影；summary 失败和进行中取消均写出失败闭合事件且不改变投影。实际 Morde IPC 检查点持久化、自动触发、网关用量结算和生产容量策略不在 T01 验收范围，必须在 T03/T04/T06 接通前保持手动引擎不可触发。
  - 验收：已通过本机 `npm test`（97/97）、定向压缩用例（3/3）、Runtime Docker 构建（容器内 96 通过、1 项因模型网关源码不在构建上下文而跳过）和隔离镜像 smoke；本次 Docker context 为 53.24 kB，不包含 DSH checkout。源码来源、包/许可证、构建与压缩事件证据见 `runtime/morde-cloud-runtime/vendor/deepseek-harness/UPSTREAM.md`。该接入仍不是面向用户可用的压缩功能。

- [x] **T02（完成，依赖 T01）建立摘要版本、来源和操作持久化合同**
  - 新增 `00174_conversation_summaries.sql` 及 app Store 端口、PostgreSQL 事务实现，并同步[公共约定](./00-总览与公共约定.md)。以服务实例、owner、actor、会话、personal/project scope 和权限版本隔离摘要；保存 AES-GCM 密文、递增版本/前驱、逐条来源证明、幂等操作输入摘要、状态、租约和模型调用 ID。
  - 读写先重验当前路由收据、会话归属和项目成员版本；提交校验摘要基准 CAS、有效租约、检查点身份及来源是否仍可见。消息来源必须来自同一会话且严格早于当前路由消息；事务不等待模型。来源失效会递归失效后继并回退到同授权版本下更早的有效摘要，不更改原文。
  - 检查点目前记录 DSH session ID、覆盖序号和摘要，模型调用字段仅作稳定关联位；真实 IPC 检查点正文恢复由 T04 接通，模型账本/结算由 T03 接通。本项不开放自动压缩或客户端入口。
  - 验收：隔离 PostgreSQL 用例验证从空库迁移及将现有 schema 173 升级到 174、个人和项目授权、跨账号来源拒绝、并发 Worker 互斥、过期租约接管与旧租约拒绝、操作重放/变更冲突、基准 CAS、摘要加密读回、来源失效回退/递归失效和原文保留；`internal/app` 摘要密封及输入校验用例通过。

- [x] **T03（完成，依赖 T02）接入压缩用途的模型网关与账本**
  - 新增 `00175_conversation_compaction_model_calls.sql`，在现有账号/项目账本中增加 `compaction` 来源；操作行保存模型 ID/provider model/连接版本、请求 digest 和加密模型结果。不增加 Task、第二套预算或路由 JSON 结果表。`ModelCallCompaction` 使用稳定调用 ID，digest 绑定服务实例、owner/actor、Conversation/触发消息、授权凭据/版本、scope 版本、操作基线、输入来源 digest、DSH session/覆盖序号、模型选择和请求正文。
  - `CompleteConversationSummaryModel` 复用现有模型连接/能力校验、个人或平台 Token 预算及项目资金预算，并在 reserve 和 dispatch 前分别重验路由凭据、项目成员、摘要操作租约、checkpoint 身份和模型连接版本。成功结果仅以 AES-GCM 密文和 digest 持久化；同一调用与请求从账本恢复，不二次派发；同一调用改请求返回幂等冲突。无效输出结算已知费用但结束本次操作。超时/未知用量将账本与操作一同标为 `unknown` 并释放写租约；账号/项目账本未知时阻止继续预留，管理员对账后将该操作标为失败，等待新操作 ID 重试。
  - 管理端未知费用枚举加入 compaction；过期但进程崩溃留下的 compaction `dispatched` 调用由未知费用列表暴露并在对账事务内转为 `unknown`。通用 stale-call sweep 排除 compaction，避免只改账号账本而遗漏压缩操作状态。
  - Worker 的压缩 IPC 调用点尚不存在，归入 T04 严格 DSH IPC/运行桥接；本项完成的是可由受控 Worker 调用的网关入口、预留/结算/恢复及账本边界。
  - 验收：`go test ./internal/app -count=1` 通过；隔离 PostgreSQL 的 summary model 集成测试覆盖个人模型不依赖平台额度、账号和项目账本结算、项目额度不足、加密结果恢复、重复请求及请求变化冲突、凭据撤销、超时未知调用、管理员对账重放；`TestRouteModel*`、Task 模型围栏/真实 checkpoint、模型用量管理回归通过。`gofmt -d` 与 `git diff --check` 无差异。测试使用合成模型与临时 PostgreSQL 16。

- [x] **T04（完成，依赖 T01、T03）接通有界 DSH 压缩上下文与双重预算**
  - 修改：`runtime/morde-cloud-runtime/src/engine.mjs`、`server/internal/worker/cloud_dsh_coordinator_protocol.go` 及实际运行/检查点桥接，必要时扩展现有检查点服务。同步两端严格 IPC：维护身份、`purpose`、检查点、模型窗口及安全错误类别均需经服务端校验；压缩 Session 不提供工具，任务检查点校验不为此放宽。
  - 使用 T01 固定的 DSH 压缩及 token meter 实现，并通过 `summarize()` 和现有 provider 适配，不复制实现到 Morde Worker。提供可信模型窗口及来源，配置中文余量、近期尾部、输出预留和有界摘要；另按网关现有输入字节规则校验。模型切换时重新计算，未知窗口无可靠保守值时安全失败；溢出错误仅返回归一化类别并有限重试，不泄露 provider 原始敏感信息。
  - 验收：Runtime 用例覆盖事件顺序、来源、可替换范围、取消、断电式重启、检查点校验和无工具约束，并拒绝伪造身份、用途及多余 IPC 字段；32K 窗口、中文、工具 schema、切小模型、未知窗口和不可拆分长输入都遵守两套预算。旧 Task 会话、取消与恢复回归通过。
  - 验收：Runtime `npm test` 102/102，覆盖 DSH 事件、受限来源、取消、异常字段拒绝和断电后恢复；Worker `go test ./internal/worker -run 'TestCloudDSHCoordinator|TestCloudDSHAccountRuntime' -count=1` 通过；app 定向模型容量/输入证明用例通过；相关 Go 文件 `gofmt -d` 无差异。压缩专用 DSH Session 没有工具，模型窗口由网关授权解析并在 dispatch 再校验，模型输入另受 128 KiB 网关字节预算约束。

- [x] **T05（完成，依赖 T02）按授权分页读取压缩输入和来源证明**
  - 修改：`server/internal/store/postgres/conversation_route_actions.go`、`conversation_route_causality.go`、相关消息查询和 app 端口。针对当前 Conversation 固定上版摘要、来源及可见历史边界，以有界批次分页取得原文与已确认任务结果；保留消息 ID、角色、顺序和结果来源，不伪造工具调用或操作成功。
  - 长消息按安全边界分片并可还原来源和顺序；不能因路由现有单条 4 KiB 限制丢弃。项目按 actor 和成员权限过滤，排除无权、已抑制、当前消息及其不允许看到的因果后代；迟到前序消息不能只靠 `sequence <= watermark` 证明安全。
  - 验收：隔离 PostgreSQL 用例覆盖多页、超 4 KiB 消息、已确认结果、跨账号/项目、抑制内容及后代序号早于前序的乱序写入；每条来源可追溯，非授权内容不进入输入。
  - 完成实现：新增模型窗口绑定的有界分页合同和 PostgreSQL 读取器；支持长消息 UTF-8 字节游标、摘要/授权版本游标围栏、当前消息与乱序因果后代排除、消息抑制过滤、个人/项目来源过滤、普通助手回复及已发布 Task 结果读取；摘要提交时重验来源状态和因果边界。续页游标校验消息/Task ID 与排序形状。
  - 验收：使用本机隔离 PostgreSQL 16 通过 `TestConversationSummaryInputPagesKeepLongSourcesAndExcludeSuppressedDescendants` 和 `TestProjectConversationSummaryInputSharesOnlyProvenMemberHistory`；覆盖多页中文长消息完整回拼、直接助手回复、已发布 Task 结果、抑制消息、乱序因果后代、项目共享已证明结果和未发布跨成员任务排除。`gofmt` 已执行。

- [x] **T06（完成，依赖 T02–T05）编排自动压缩、条件提交和恢复**
  - 修改：`server/internal/worker/` 的对话分流编排、压缩维护执行及其装配。达到任一安全预算时，固定授权来源、旧摘要和基准版本，创建短期 DSH Session，调用 `compactNow()`；只接受格式有效、非空、有界且实际缩小的结果。
  - 检查点和模型账本确认后，重新验证会话、租约、来源、项目/记忆授权和基准，再 CAS 提交新摘要；期间消息继续持久受理，多个会话/独立任务可并发。模型调用不在数据库事务内；结果未知先对账，失败、取消、过期或冲突保留旧有效摘要和原文，不能伪装成功。
  - 验收：两 Worker 同时压缩同一会话只允许一个版本推进；压缩期间新消息可接收；覆盖空/过长/未缩小结果、失败、取消、持久化失败、CAS 冲突、各检查点崩溃及恢复，账本和最终状态一致。
  - 完成实现：路由 Worker 在消息分流前读取授权分页、验证旧摘要并按模型窗口收集完整来源；通过 DSH `compactNow()` 生成摘要，检查模型身份、输出大小和缩减幅度后加密并 CAS 提交。DSH checkpoint 在压缩 Session 外持久化，重启可恢复已完成摘要，或从仅保存了来源的 checkpoint 继续压缩；摘要旧版本作为下一版上下文并携带摘要哈希。压缩模型调用保持在数据库事务外；生产入口已装配摘要 Store、分页读取器和 Sealer。
  - 验收：Runtime `npm test` 通过 103 项；`server/` 的 `go test ./internal/worker -count=1` 通过、`go test ./cmd/portal -run '^$'` 编译通过；隔离 PostgreSQL 16 通过 `TestConversationSummaryPersistenceLeasesReplayAndInvalidation`、T05 两个分页/授权集成用例。覆盖竞争租约、过期操作 ID 复用、摘要 CAS/来源失效、完整摘要恢复、仅来源 checkpoint 恢复、取消/失败 DSH 闭合及有界加密提交。`gofmt` 已执行。
  - 范围边界：本项完成摘要生成和持久化，路由消费摘要由 T07 完成。输入无法在模型预算内完整容纳或来源证明超过 8192 条时会跳过/拒绝压缩，原文仍保留；需在后续任务解决长期证明链的扩展策略。

- [x] **T07（完成，依赖 T05、T06）将摘要接入消息分流并保持因果隔离**
  - 修改：`server/internal/app/conversation_route_*.go`、`server/internal/worker/cloud_dsh_coordinator*.go`、`server/internal/store/postgres/conversation_route_actions.go`。在 `ConversationRouteSnapshot` 中装配该消息可使用的摘要版本、来源证明和未覆盖原文，计入请求指纹；模型派发前及 `ApplyConversationRoute` 前重验现有租约、前序、任务修订和授权。
  - 完成实现：Store 只装配当前权限/作用域版本下、来源逐项重验的有效摘要；因果校验失败时向前查找较早有效版本，Worker 只在解封及哈希校验后把摘要放入 DSH 文本请求，并从未覆盖原文中排除摘要已覆盖的同一消息/任务结果。摘要哈希和来源证明摘要参与网关请求与 DSH 检查点指纹；摘要打不开或超预算时回退完整的最近原文。派发准备、实际 dispatch begin、协调器结果恢复和 `ApplyConversationRoute` 都复核租约及当前快照、前序、任务修订和权限。
  - 验收：app/Worker 定向用例通过；本机隔离 PostgreSQL 16 通过 `TestConversationRouteSelectsOlderCausallyValidSummaryAndRejectsStaleDispatch`、`TestConversationSummaryIsolatedAcrossMainAndSideChats`、`TestConversationSummaryPersistenceLeasesReplayAndInvalidation` 和 `TestConversationRoutePredecessorArrivalFencesModelDispatch`。覆盖摘要指纹绑定、解封失败原文回退、主会话与两个旁聊隔离、迟到前序后代摘要回退、摘要更新后拒绝旧路由派发和已有 dispatch 围栏。
  - 范围边界：旧摘要最多向前检查最近 32 个有效版本；超过该范围时不使用未证明摘要，只回退最近路由历史。摘要与历史作为不可信输入；任务权限和状态始终读取权威 Store。既有检查点如果上下文指纹变化会被拒绝恢复，避免用过期上下文继续执行。

- [x] **T08（完成，依赖 T05、T07）固定 Task 摘要快照并保护恢复**
  - 修改：`server/internal/store/postgres/task_conversation_context.go`、`conversation_route_actions.go`、`server/internal/worker/` Task 上下文/检查点恢复及相关 app 合同。新 Task 固定路由可见的摘要版本、来源和授权证明，合并未覆盖原文，避免重复；固定身份、最新任务修订和操作权限仍来自现有服务端记录。
  - 恢复时核对摘要、传递来源、项目成员和个人记忆版本；后来发布新摘要不能改变正在执行的 Task 输入，权限或来源失效时沿用安全等待/恢复失败路径，不能从旧检查点恢复被撤权内容。
  - 验收：新 Task 能使用协调器已知的旧上下文；并发更新摘要不改变 Task 输入，撤权、记忆关闭、源消息被抑制或证明损坏后恢复失败且无工具副作用；普通 Task 的取消和恢复回归通过。
  - 完成实现：Task 初始事件固定摘要版本、操作 ID、来源/摘要 digest 和授权作用域；仅将引用写入持久事件，密文与来源证明在恢复时按固定版本读取。恢复重新验证服务实例、Task 授权、项目成员版本、个人记忆设置、来源 digest 与原始来源授权，摘要覆盖的原文/Task 结果从未覆盖历史中去重。DSH 执行前解密校验摘要，失败时不启动普通工具；项目恢复失败沿用现有安全暂停路径。
  - 验收：隔离 PostgreSQL 16 通过固定 v1 摘要在 head 推进至 v2 后继续恢复、抑制来源、来源证明损坏、关闭个人记忆和记忆版本更新时恢复失败且未创建 DSH Agent 实例；项目检查点恢复回归通过。Worker 初始提示用例通过。修复恢复来源校验中 nil 后代列表与 SQL `ALL(NULL)` 不匹配的问题，并由固定版本恢复用例覆盖。

- [x] **T09（完成，依赖 T05、T07）接入当前会话原文检索**
  - 修改：`server/internal/app/cloud_dsh_history_tools.go`、`cloud_dsh_tool_broker.go`、相关消息 Store 和协调器分流提示。新增只读的当前会话工具并经过既有 Broker/Task 围栏；个人和项目按当前身份、成员、原文访问及删除/抑制规则校验，返回有界片段、消息 ID、时间及部分结果标识。
  - 协调器保持无工具：需要查证摘要之外的原文时走新 Task 路径；不调整已有 `morde_conversation_history_search` 的跨会话和记忆开关语义。检索内容作为历史数据，不是可执行指令或操作授权。
  - 验收：摘要缺失细节时经真实 Broker 路径找到当前会话授权原文并正确标记截断；任意会话 ID、其他账号、无权项目、已抑制消息和注入式历史均不能越权或触发操作。
  - 完成实现：新增严格只读 DSH Broker 工具 `morde_current_conversation_history_search`，只接受关键词和有界数量，不接受会话 ID；从当前 Task 的个人/项目授权会话分页读取，固定在 Task 的历史序号边界内，最多扫描 800 条并通过 `partial` 标识扫描或命中截断。返回消息 ID、时间和有界片段；排除未完成/待处理、带附件、被抑制及凭据样式内容。工具执行重新核对当前 Task、租约、会话身份和授权，写入现有 Task 步骤/调用预算；历史文本仍按引用数据处理。
  - 验收：app Broker 用例覆盖固定会话、未来消息、删除抑制、附件、待处理消息、凭据样式文本、注入式文本、命中截断和伪造 `conversation_id` 拒绝；隔离 PostgreSQL 16 使用固定摘要 Task 经 Broker 找回摘要覆盖的原始消息。应用与 Worker 定向回归通过。扫描 800 条上限之外的内容需由 `partial=true` 告知。

- [x] **T10（完成，依赖 T02、T05、T08、T09）贯通记忆授权、来源失效和删除**
  - 修改：`server/internal/app/cloud_dsh_personal_data.go`、现有个人记忆写入/设置及 `server/internal/store/postgres/personal_mutations.go`、摘要来源和派发/恢复校验。记忆沿用已有授权、对象版本及 mutation 回执；新增写入保存可核验来源，摘要不得自动成为记忆，也不能把个人记忆永久混入无法失效的摘要。
  - 记忆关闭、修改、删除，消息抑制或项目撤权时，在同一事务或以版本围栏保证相关摘要、后继版本、索引和派发缓存先失效再被读取；在途调用及恢复重新验证。来源不明时保守失效并安全重建或等待，不把现有按字符串匹配当作语义删除证明。
  - 验收：授权个人记忆可在允许的个人会话读取，另一账号和未授权项目不能读取；关闭、修改、删除、改写内容、撤权及并发派发后，旧摘要、检索和检查点均不能召回受影响内容；原始记录保留/删除遵循现有合同。
  - 完成实现：新增 `00180_personal_memory_sources.sql` 保存记忆版本对应的来源证明；DSH 写入绑定当前 Task 已受理用户消息的 ID、会话、序号和内容 SHA-256，直接用户写入绑定 mutation 回执 digest。记忆修改/删除、关闭设置、历史抑制和项目成员降权/移除在事务内失效对应摘要及后继版本、释放压缩租约；账号记忆变化保守清空该账号个人摘要，连尚未提交摘要的在途压缩操作也一并失效。Task 工具执行与检查点恢复持续复验固定记忆版本，失败前不产生工具副作用；原文保留，删除只增加既有抑制记录。
  - 验收：隔离 PostgreSQL 16 通过 `TestPostgresCloudDSHPersonalMutationIsAtomicFencedAndRedacted`（含真实 DSH Task 来源证明）、`TestPostgresDeletedPersonalMemorySuppressesMatchingAIHistoryOnlyAndCannotRollBack`、`TestPostgresCloudDSHMemoryWriteCannotRaceConsentWithdrawal`、`TestTaskConversationSummaryFailsClosedWhenSourcesOrMemoryChange`（含删除、修改、关闭、在途 Task 与未提交压缩操作）及 `TestProjectTaskSummaryIsInvalidatedWhenMemberIsRemoved`；app History Broker、Worker Task 初始上下文定向用例通过。验证了 mutation 回执幂等、无明文记忆进入 Task 审计事件、原始历史不被改写及撤权后恢复/工具访问被拒绝。

- [x] **T11（完成，依赖 T05、T06、T10）迁移历史会话并制定安全回退**
  - 修改：压缩 Worker/Store 的首次初始化和恢复逻辑、迁移及运维说明。以固定授权边界从旧到新分页、有界地建立初始摘要并逐批滚动；恢复进度和来源，不能只使用当前路由的最近 8 条或 Task 的 32 KiB 快照。历史未处理完时明确标记不完整；新的消息仍可受理。
  - 升级先停稳旧执行者，处理已认领消息、在途压缩和未结算/未知模型调用；旧协调器检查点保留原身份。回退先停止新压缩并对账，保留摘要、账本和原文，不让旧版本读取不兼容检查点或重复派发；回到旧有界历史能力时如实说明限制。
  - 验收：含已有主会话、旁聊、活动 Task 与长历史的数据库升级；中断后重复执行、续跑和回退均不丢消息、不改变主会话指针、不重复计费或执行，未处理历史不被描述为完整记忆。
  - 完成实现：旧会话在下一条新消息路由时按授权分页从最早未覆盖来源惰性迁移；摘要来源证明作为进度，Worker 重启后从已提交 head 继续，无额外迁移队列或隐藏消息。受预算/页数限制留下的中间历史会写入加密摘要的 `【历史摘要不完整】` 提示，路由明确不得声称完整；继续覆盖后提示才清除。新消息在模型压缩期间仍可持久受理。增加升级/回退操作手册，要求停旧 Worker、处理未知模型调用、保留摘要和原文，无法验证的 Task checkpoint 走安全等待/停止路径。
  - 验收：隔离 PostgreSQL 通过从 schema 173 升级至当前 schema 180、持久摘要模型调用时拒绝破坏性迁移回退并保留摘要/原文、主会话和旁聊隔离、消息在压缩期间继续受理及 Task 固定摘要恢复测试；Worker 用例验证部分历史被显式标记、已提交来源续跑后可达到完整覆盖，T05 分页测试验证长消息按序无损读出。操作手册：[Morde 对话上下文压缩升级与回退操作手册](../../运维/Morde%20对话上下文压缩升级与回退操作手册.md)。范围说明：不为无人继续使用的旧会话做后台全库预压缩；在下一次对话消息触发时才迁移，原文一直保留。

- [x] **T12（完成，依赖 T06–T09）呈现安全等待、失败与重试结果**
  - 修改：沿用现有消息分流/Task 状态和错误处理；如现有合同不能表达压缩等待、历史不完整、无法压缩的单条长输入或调用结果未知，同步 `server/openapi/portal.yaml`、公共约定、服务端、Portal/Workbench 生成类型及受影响的 PC、iOS 调用方。提示应说明消息已受理及可执行的下一步，不暴露模型原始错误或摘要正文。
  - 使用现有安全重试路径，不能因重试重新接收消息、重复创建 Task 或绕过未知调用对账。无需新建手动压缩入口；若涉及界面改动，沿用现有界面与设计。
  - 验收：实际调用覆盖等待、失败、恢复和重试；客户端保留消息并显示正确状态。涉及 Portal/Workbench 时用内置浏览器验收，涉及 iOS 时按现有模拟器/客户端验收方式验证；受影响的其他 PC 客户端也须验证，不以纯 mock 结果宣称真实服务链路通过。
  - 完成实现：复用现有路由 processing/waiting/failed 状态与任务 `model_context_limit`/`dsh_recovery_unresolved`；压缩失败优先保留旧摘要和原文，路由可用原上下文继续，否则服务端按有界安全失败返回通用类别。Portal 已解释模型待核对、消息超限及 Task 恢复中断；模型/上下文失败沿用原消息 ID 的重试路径，超限消息保留原文并提示缩短，避免重复受理。无需新增 OpenAPI 字段或修改 PC/iOS 调用方。
  - 验收：Portal `Assistant.test.tsx` 定向 3 项通过，覆盖未知模型用量后同键重试、超长消息保留原文且不提供错误重试按钮、Task 开始后触及模型上下文上限的安全提示。现有路径不向客户端暴露 provider 原始错误。没有 Portal 页面改动，因此未新增浏览器验收步骤。

- [x] **T13（完成，依赖 T01–T12、T14）完成端到端验收及发布门禁**
  - 修改：相关 Runtime、Worker、app、PostgreSQL 集成测试和验收记录。使用本机合成模型及隔离数据库，贯通消息受理 → 历史授权 → DSH 压缩 → 网关结算 → 摘要提交 → 分流/Task → 原文找回 → 重启恢复。
  - 验收：一个主会话和至少两个旁聊各自持续聊天并至少三次压缩；单会话累计历史超过现有 8 MiB 恢复门槛后重启仍能继续，原文可分页读取。并发、乱序、项目/账号隔离、权限撤销、记忆删除、异常账本、模型切换及升级回退均通过；能证明 DSH 实际压缩生效，不能以安装插件或单元测试代替闭环。
  - 验证命令：Runtime 执行 `npm test`；`server/` 按约定执行 `make lint`、`make test race cover`、`make generate` 并确认生成物无意外差异；设置隔离的 `CH_TEST_DATABASE_URL` 后执行 `make it`。涉及客户端时执行对应测试并按 T12 验收。缺少环境或失败要保留未完成状态并记录原因。
  - 当前验收记录（2026-11-11）：Runtime `npm test` 103/103、Portal `npm test` 52 个文件 / 553 个 Vitest 用例及 2 个 Node 用例通过；`make lint` 为 0 issues，`make test race cover` 通过（总覆盖率 45.1%），`make generate` 仅生成预期的 `PersonalMemorySource` sqlc 模型；DSH 上游来源校验脚本和 6 项源码 SHA-256 校验通过。设置本机隔离 PostgreSQL 后 `GOFLAGS='-timeout=20m' make it` 全部通过，PostgreSQL 包 1003.728 秒、Worker 包 221.961 秒；00180 来源证明和 00169 删除墓碑的两个回退保护定向用例通过。
  - 新增真实端到端验收：通过 TaskService 接收消息，使用实际 Node DSH Account Runtime、Worker、ModelGateway 和隔离 PostgreSQL；主会话与两个旁聊各连续完成 3 次压缩，压缩模型调用与操作账本共 10 条且全部结算/提交。主会话写入 840 条各 10 KiB 的授权历史，使原文超过 8 MiB；重启并恢复 Account Runtime 后继续路由，产生版本 4 摘要和 Task 固定上下文，再用 Task 的当前会话历史工具命中较早原文，并通过服务端消息分页读回原文。该验收同时发现并修复了摘要提交事务违反 `conversation_compaction_model_response_check` 的缺陷：摘要成功持久化后清除临时模型响应密文，避免已提交操作继续保存仅允许 `checkpointed` 的响应。
  - `make test race cover` 在未设置数据库环境时通过，PostgreSQL 集成部分按约定跳过；T13 的真实数据库链路由单独定向用例及设置 `CH_TEST_DATABASE_URL` 的全量 `make it` 覆盖。可选 OpenSandbox 集成因未设置 `CH_OSB_URL` / `CH_OSB_API_KEY` 跳过；本次没有页面改动，无需浏览器验收。

- [x] **T14（完成，依赖 T01、T04）固化 DSH 源码来源及更新同步**
  - 修改：在 `runtime/morde-cloud-runtime/` 内增加受版本控制的来源与同步说明；有源码迁入时一并维护对应上游快照和必要补丁。逐包记录上游仓库、提交、路径、版本、许可证、直接/传递依赖、Morde 接入点及测试；发布包记录锁文件完整性，源码迁入记录源码校验值、补丁、原因和适用版本。
  - 写明在本地以记录的旧提交为基线，比较新提交的源码、依赖和事件/检查点格式，判断哪些补丁已被上游吸收，再同步依赖、锁文件及构建；更新时重新验证上游相关压缩/计量用例及 Morde 会话隔离、网关、检查点、取消、恢复与升级回退。上游版本和 Morde `ENGINE_VERSION` 分别记录，不能仅因上游版本变化自动视为检查点兼容。
  - 验收：用当前固定上游提交在本地重新核对或重放一次来源及补丁检查，结果可复现且无未解释差异；干净目录构建与 T01 一致。若升级后旧检查点不可安全恢复，记录停稳、恢复或回退方案并保持发布门禁，不把尚未验证的新版本标为可用。
  - 完成实现：在 `runtime/morde-cloud-runtime/vendor/deepseek-harness/UPSTREAM.md` 按来源提交、tag、源码路径、MIT 许可、直接/传递 DSH 依赖、Morde 接入点、测试及升级步骤记录；`source.sha256` 固定迁入源码/manifest 校验值；新增 `scripts/verify-dsh-upstream.sh` 从本地 DSH Git 对象核对 tag 与固定提交、文件清单和内容。记录升级时同步 package/lockfile、源码、checkpoint 兼容和回退的联合审查步骤。
  - 验收：本地 `project/deepseek-harness` checkout 处于固定提交；脚本确认 package 源码和 manifest 与 release tag 无差异、vendor 文件逐项匹配；`shasum -a 256 -c source.sha256` 全部通过。Runtime 干净构建/测试证据沿用 T01、T04。

## 执行顺序和完成条件

先完成 T01 的源码来源、构建、插件兼容与事件恢复验证，再建立 T02–T05 的持久化、模型网关、Runtime 和输入边界；随后实现 T06–T10 的自动压缩、路由、Task、原文检索和权限失效，并在 T04 后完成 T14 的上游同步说明；最后完成 T11、T12 和 T13 的历史迁移、用户可见错误处理和整体验收。可以按模块安排工作，但同一文件及跨模块合同不能被并发改出两套实现；所有安全围栏和恢复测试通过前不得宣称功能可用。

## 问题记录

- T01 已核验并固定 DSH 发布包和许可证；上游源码快照可由 Runtime 自带的 TypeScript 构建及隔离镜像使用，不依赖本机 DSH checkout。真实 IPC 检查点下压缩事件的重启恢复仍待 T04 联验；当前自动触发关闭，尚未接入 Morde 网关和授权边界。
