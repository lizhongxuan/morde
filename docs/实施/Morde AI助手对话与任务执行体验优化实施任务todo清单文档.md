# Morde AI 助手对话与任务执行体验优化实施 TODO

日期：2026-11-13。来源：[优化设计方案](<../Morde AI助手对话与任务执行体验优化设计方案.md>)、[对话与任务执行体验需求](<../Morde AI助手对话与任务执行体验需求文档.md>)。OPT-01 至 OPT-09 已完成。旧的[对话体验实施清单](<Morde AI助手对话与任务执行体验实施任务todo清单文档.md>)保留其编号、状态和历史记录。

## 范围与执行约定

本次修改服务端消息关联、主 Agent 分流与重判、DSH 协调调用的账本和恢复、消息接口，以及 PC/iOS 的关联状态展示。每次主动发送仍保存独立消息；重复请求通过原请求执行一次。任务修订、工具授权、审批、事件和 DSH 检查点继续复用现有机制。

实施前以 `runtime/morde-cloud-runtime/package.json` 和锁文件核对已接入 DSH 版本，并查看本地 DSH 上游源码中的相关会话、工具和插件；本地源码副本的版本不能代替锁文件。优先复用现有能力；若确需新增或升级插件，先说明原因、核验版本与许可证，并覆盖授权、取消和恢复验证。

仅在本地修改和验证，不外发、上传或发送本机文件、截图或项目源码。工具与项目读取测试使用本机隔离环境和合成数据；如验证真实模型，只使用合成文字，不发送本地文件或源码。实施前检查现有未提交修改并保留；接口、状态机、迁移、权威公共约定和对应代码/测试须在同一变更中同步，不能发布只改合同或只改实现的半成品。

## 接口与数据变化

| 现有接口或数据 | 本次变化及调用方 |
| --- | --- |
| `POST /api/conversations/{id}/messages`、`POST /api/projects/{id}/conversations/{conversation_id}/messages` | 请求字段不变，仍先持久受理并返回 202；相同 `client_message_id` 的网络重试复用原回执，不同主动发送保留不同消息。返回消息可选增加下述关联字段。 |
| `GET /api/conversations/{id}/messages`、`GET /api/projects/{id}/conversations/{conversation_id}/messages`，含 `client_message_id` 回查 | `ConversationMessageBase` 拟增加可选 `duplicate_of_message_id`、`related_task_id`；未有关联或未产生任务时省略相应字段。原 `task_id` 仍是实际投递任务的消息，原 `delivery_status` 枚举不变；个人和项目调用方按当前权限读取，不暴露其他成员未授权任务。 |
| `POST /api/conversations/{id}/messages/{message_id}/retry-processing`、`POST /api/projects/{id}/conversations/{conversation_id}/messages/{message_id}/retry-processing` | 重试重复消息时定位原请求，以原请求和幂等键归一重试身份；只允许原 actor 在现有授权和可恢复条件下操作，已派发/结果未知的调用不能盲目重发。仅重试尚无任务的分流失败；已有任务沿用任务操作合同。保持 202、鉴权及现有错误语义。 |
| `ConversationRouteContext` / `ConversationRouteDecision`，`server/internal/app`（内部） | 拟增加 `join`、`target_message_id`，以及 `new` 的 `explicit_new` 判断；候选包括授权的原始请求、未处理前序及必要的已完成任务。模型只提出意图，服务端校验归属、条件、目标及状态后提交。 |
| `conversation_message_routes`、`conversation_route_model_calls`（既有表） | 拟增 `duplicate_of_message_id`、`route_attempt`、自动重试计数及调用 `attempt`；用同服务实例/owner/actor/会话复合约束保护关联，调用唯一身份扩展为消息、阶段、尝试号，并维持每消息至多一个未解决调用。旧记录为尝试 0，不因内容相似迁移合并旧消息。 |
| DSH 主协调会话和模型调用身份（内部） | 尝试 0 保持旧 ID、提示词版本和摘要算法；新尝试使用绑定上下文摘要的新稳定身份。同一尝试重启复用原记录，未知调用先对账；结果回传的既有重试身份保持独立。 |

消息读取和刷新以实际对话接口及现有消息变更机制为准。`/api/sync/changes` 当前明确过滤会话和消息，不把新增关联强行放入个人数据同步接口。

## 问题记录

- 当前快照变化时会写通用确认，与自动重判的目标不一致；由 OPT-04 处理，保留提交前状态与授权复核。
- 当前无效模型输出可能被转成通用澄清，与“模型失败不等于用户意图含糊”不一致；由 OPT-02、OPT-03 区分无效输出与未知调用，并保留用量证据。
- 当前分流候选历史受最近消息窗口限制，不能仅依此保证未决前序被判断；由 OPT-02 扩展必要上下文，获取不全时不得猜测。

## 实施任务

- [x] **OPT-01（完成）固定合同和持久化约束**
  - 修改：`server/openapi/portal.yaml`、`server/internal/domain/conversation/types.go`、`docs/实施/00-总览与公共约定.md`，以及 `server/migrations/` 新增下一个可用编号的 Goose 迁移。登记“旧快照变化触发通用确认”与新设计的差异；明确关联字段、投递状态语义、重试身份、权限和错误行为。
  - 迁移在路由表加入关联及尝试/重试计数，在调用表加入 `attempt`、调整唯一约束；按 `server/AGENTS.md` 的 Expand/Contract 约定处理已发布数据和调用方，保留旧账本、检查点、消息和每消息最多一个未解决调用的围栏。外键范围覆盖 service instance、owner、actor、conversation、message；不得修改已发布迁移。回退会丢失已使用的关联或尝试证据时阻止回退，不清空数据。
  - 验收：隔离 PostgreSQL 上验证旧数据升级为尝试 0、无历史关联；跨账号/会话关联被拒绝，重复未解决调用被拒绝；空数据安全回退、已有证据时回退受阻。OpenAPI 的个人/项目消息、重试合同和公共约定与实现同步。
  - 验证：`go test ./internal/api -run '^TestConversationMessageAssociationResponseFieldsAreOptionalAndScoped$' -count=1`；隔离 PostgreSQL `TestConversationRouteAssociationMigrationPreservesLegacyRowsAndEvidence` 通过，覆盖旧消息/调用 attempt=0、scope 外键、未解决调用索引及带/不带证据回退。

- [x] **OPT-02（完成，依赖 OPT-01）主 Agent 意图与候选上下文**
  - 修改：`server/internal/app/conversation_route_decision.go`、`conversation_route_decision_test.go`，`server/internal/worker/conversation_route_worker.go` 的决策校验，以及 `server/internal/store/postgres/conversation_route_actions.go` 的快照读取。增加 `join`、`target_message_id`、`explicit_new` 的解析、合法性检查与提示；对非法输出返回处理失败/重试结果，不能转成泛化用户澄清。
  - 候选包含授权的未决前序、原始消息、活动任务及必要的近期已完成任务；不因现有最近 8 条窗口丢失待判断前序。明确区分重复、追问、修改、新工作、明确重做、停止和真正歧义；校验项目、显式目标、附件及引用条件，默认电脑选择不自动改变已有任务。历史不完整时不得猜测，普通文字不因为分流器无工具而声称可完成项目分析。
  - 验收：定向 Go 测试覆盖相同文字、同义表述、不同项目/附件/范围、明确重做、进度追问、停止、多个候选及格式错误；模型返回非候选消息、跨 actor 目标、错误字段或注入内容时不产生越权操作。明确需要搜索/读取的请求给出任务执行意图，普通问答仍可直接回复；实际工具接线由 OPT-06 验证。
  - 验证：`go test ./internal/app -run '^TestConversationRoute' -count=1` 通过；隔离 PostgreSQL `TestConversationRouteJoinWaitsOnOriginalRequestWithoutCreatingTask`、`TestConversationRouteJoinRejectsDifferentExecutionScope` 通过，证明同范围关联保留原消息、不产生重复任务，范围不同时拒绝关联。

- [x] **OPT-03（完成，依赖 OPT-01、OPT-02）分流尝试、模型账本和 DSH 恢复**
  - 修改：`server/internal/app/model_gateway_route.go`、`conversation_coordinator_checkpoint.go`，`server/internal/store/postgres/model_usage_route.go`、`conversation_coordinator_checkpoints.go`，`server/internal/worker/cloud_dsh_coordinator.go`，以及必要的 `runtime/morde-cloud-runtime/src/engine.mjs` 协议接线。传递尝试号，按消息/阶段/尝试绑定调用、模型版本、上下文摘要、会话和检查点。
  - 尝试 0 保持原 ID、提示词版本和摘要算法；优先恢复已保存决定或可核验旧检查点，无法核对原输入则等待对账。新尝试只在旧调用已结算或未派发预留已安全释放后创建；`dispatched`/`unknown` 不换 ID 重发。输出格式错误但用量已知时正常结算并记录无效结果，不伪造成业务澄清或未知计费。
  - 验收：隔离 PostgreSQL 与本机 Node/DSH 测试覆盖尝试 0 升级恢复、同尝试重启复用、上下文变化生成新身份、调用结果未知、预算与用量结算、结果回传旧重试路径；重复认领最多一个未解决调用，同一尝试不重复派发或结算，不同尝试按实际调用计量。
  - 验证：`go test ./internal/app -run '^TestConversationRoute' -count=1`、PostgreSQL 路由账本/恢复定向测试及 DSH `npm test`（106 项）通过；`go test ./internal/worker -count=1` 通过。

- [x] **OPT-04（完成，依赖 OPT-02、OPT-03）关联提交、并发和状态重判**
  - 修改：`server/internal/store/postgres/conversation_route_actions.go`、`conversation_route_queue.go`、`conversation_route_causality.go`，`server/internal/worker/conversation_route_worker.go`。在现有会话范围锁内提交 `join`、校验同服务实例/owner/actor/会话及无环，沿关联链定位原始消息；同设备前序晚到须有已验证的因果证据。
  - 重复消息保持 `conversation_messages.task_id` 为空，不投递 DSH inbox、不创建修订；原请求未完成分流时持久等待并可恢复唤醒。`new` 提交前对更早、条件完全相同且仍未完成或在新消息受理后才结束的原请求进行窄保护：等待或基于最新状态重判；明确重做和无关任务不受阻。模型调用不放数据库事务内。
  - 快照变化时保留租约、授权及任务修订复核，推进尝试并重判；连续三次冲突或已结算但格式无效仍无法提交时记真实失败，显式重试可重置自动重试计数但不复用旧尝试号。前序不可用、任务已完成/失败/停止、模型异常和真正歧义分别处理，不批量写“继续原任务还是开始新任务”。
  - 若原请求仅产生过无工具的直接答复且从未创建任务/执行工具，在授权和工具可用时按设计开启新尝试，在原请求上创建唯一实际任务并保留旧答复；原消息和重复消息在事务内转为 `pending`，经 DSH 检查点确认后再转 `applied`。存在已执行或结果未知时先核实。
  - 验收：两个 worker 对相同请求同时判断 `new` 并倒序提交、同义消息 `join`、前序迟到、原任务先完成、提交前后重启和过期租约场景中，消息均保留；工具和授权可用时恰好产生一个实际任务。明确重做、不同附件/项目及独立任务不被合并。真正无法确定目标时只问具体问题，不误停任务。
  - 验证：隔离 PostgreSQL 路由与因果测试覆盖重复关联、执行范围不一致、并发创建后的重判、消息顺序及重试；`go test ./internal/worker -count=1` 通过。

- [x] **OPT-05（完成，依赖 OPT-04）投递状态、查询投影和重试**
  - 修改：`server/internal/store/postgres/agent_checkpoint.go`、`task_execution.go`、`task_conversation.go`、`conversation_message_routing.go`、`project_conversation_message_projection.go`、`conversation_route_actions.go` 的重试路径，及 `server/internal/api/conversations.go` 和项目消息 DTO/查询。将原消息的 `pending`、已确认 `applied`、`unknown` 同步给重复消息及其变更记录；必要时从原消息投影 `related_task_id`。
  - 个人/项目列表、按客户端 ID 回查和客户端刷新均返回授权范围内一致的关联；任务结果来源、任务上下文和附件读取仍只使用实际任务消息。失败、等待审批或补充及用户停止反映原请求真实状态；重复消息的分流重试归一到无任务的原请求，并发重试只安排一次。已有任务按原任务状态和现有任务操作合同处理，不借消息重试重启任务，不得恢复已停止任务或重复未知操作。
  - 验收：根消息检查点确认前后、投递结果未知、原请求失败/审批/停止、结果发布、项目成员撤权、分页和服务重启后，个人/项目 API 状态一致；重复消息不进入 Agent inbox、不成为结果来源。旧客户端可解析现有枚举，重试重复消息仍由服务端安全归一。
  - 验证：关联字段 API 测试、PostgreSQL 路由投影/重试和模型账本恢复定向测试通过；Portal 对话与项目单测 178/178 通过，包含旧响应兼容及失败重试。

- [x] **OPT-06（完成，依赖 OPT-02、OPT-04）需要工具的工作进入执行**
  - 修改：`server/internal/worker/cloud_dsh_executor.go`、`server/internal/app/agent_tools.go`、`server/internal/app/cloud_dsh_mcp.go` 和 `server/internal/mcpconnector/` 等实际模型工具能力和 MCP 装配路径中与本问题有关的调用点。复用已有 DSH、模型网关和工具代理，明确区分分流器无工具、模型不支持工具、工具未装配、授权失败和目标不可用；不因为没选云端电脑屏蔽可用搜索/文件能力。
  - 验收：隔离服务使用合成项目和工具，确认查看/搜索/分析请求实际进入已授权 DSH 任务并能返回有依据的结果；仅文本答复误分流的补救只创建一次任务并保留旧回复。无权限、无工具、模型不支持工具、目标不可达和副作用结果未知时显示真实失败或等待，不伪报完成、不绕过审批。普通问答无电脑仍可完成。
  - 验证：真实本机 `mcp-searxng@2.10.0` 与 SearXNG 已完成三层合成数据验收：`TestBuiltinSearchWithLocalSearxng`、`TestBuiltinSearchIntegrationWithLocalSearxng`、`TestDSHSearchToolCompletesThroughWorkerAndPersistsOperation` 均通过，覆盖 Morde Broker、Node/DSH 工具调用、下一轮结果消费和任务操作账本。

- [x] **OPT-07（完成，依赖 OPT-01、OPT-05）PC 个人与项目对话**
  - 修改：`web/portal/src/routes/Assistant.tsx`、`Projects.tsx`、`web/portal/src/app/message-status.ts`、相关 API 类型/模拟数据和测试；由 OpenAPI 生成 `web/portal/src/api/schema.d.ts`。显示原请求/任务关联与真实状态，保留每条气泡，前序等待不显示为等待用户确认；关联目标不在当前页时仍可定位状态。保留发送、附件及网络重试各自语义。
  - 验收：Portal 定向测试和类型检查覆盖个人/项目消息、旧响应缺字段、分页、失败重试和无权限；在 Codex 内置浏览器连接本机隔离服务，连续发送重复请求、追问和独立请求，刷新后核对气泡、状态、实际任务数和结果，不出现重复通用确认。
  - 验证：Portal typecheck、ESLint、Assistant/Projects 单测（178/178）及连续消息 E2E（1/1）通过。本机隔离 PostgreSQL 上运行 `TestConversationBrowserAcceptance`，内置浏览器通过真实 Portal API 验收重复请求、进度追问、独立请求和刷新恢复；数据库核对重复消息关联同一任务、独立请求创建不同任务，单项通过（124.06 秒）。演示模式中的旧待发送消息不作为服务端验收证据。

- [x] **OPT-08（完成，依赖 OPT-01、OPT-05）iOS 对话与本地队列**
  - 修改：`clients/nanoMuse/android/src/ios/Morde/MordeApp.swift`、`MordeCredentialExpiry.swift`、相关消息测试及 `clients/nanoMuse/android/src/ios/Localizable.xcstrings`。解码关联字段、展示原请求状态并将失败重试归到原请求；保留非空发送按钮、输入法确认、离线队列、附件、账号/服务实例隔离及同一消息重试原 ID。
  - 验收：先核对 `Morde.xcodeproj` 当前 scheme、目标成员和测试选择器，再运行消息/队列测试及模拟器 UI 用例，确认实际执行数大于 0。覆盖连续发送、前序等待、关联失败/审批、切换账号、断网重连、分页与重启；服务端未确认时不显示已发送，重复消息不伪报等待用户或任务完成。
  - 验证：iOS SPM 消息测试 8/8 通过，Xcode 模拟器构建通过。`testC10PortalMessagePersistsAcrossAppRestart` 在本机 Portal 完成登录、发送、应用重启、第二安装实例读取会话和任务结果，1/1 通过。随后 9 项聊天/队列/审批/分页 UI 回归中 8 项通过；附件丢失回执测试最初因断言只接受附件按钮且错误按页面总量计数而失败，改为在回执消息内按唯一附件 ID 核对原附件仍在后，该用例单项复跑 1/1 通过。临时 scheme 测试环境变量已移除，scheme 恢复干净。

- [x] **OPT-09（完成，依赖 OPT-01 至 OPT-08）本地集成和整体验收**
  - 修改：在同一会话并发改判时，将完全相同且执行范围相同的新请求直接关联至最早可加入的原请求，避免上下文连续变化耗尽自动重判次数或形成重复链；语义相近请求仍走模型重判。iOS 消息恢复按客户端消息 ID 验证单条消息，允许内容相同但消息 ID 不同的独立记录同时显示。
  - 验收：在隔离 PostgreSQL、真实 Portal HTTP 和合成模型/执行器下，覆盖 PC/iOS 同一活动会话交错发送四条相同请求、同义请求及原任务执行期间/完成时的竞态；检查消息保留、单任务、三条重复均指向同一根请求及可追溯结果。再验证进度追问不创建任务、独立任务能完成，并复用已有授权、不同账号与项目成员、老数据升级、重启、取消、账本及未知操作不重做测试。
  - 验证：跨端浏览器验收 202 秒通过，数据库确认四条同文请求、一个分析任务、三条重复关联同一根消息；状态追问无任务归属，独立任务完成。`testC10PortalMessagePersistsAcrossAppRestart` 在同一场景下通过（1/1），覆盖 iOS 发起消息、重启恢复及第二安装实例读取共享会话/结果。`TestConversationRouteConcurrentNewDecisionReclassifiesAgainstCreatedTask` 在普通与 `-race` 模式均通过；服务端 `make lint` 为 0 issues，`git diff --check` 通过。跨端首轮暴露的失败重判/重复链已修复并以新隔离会话重验。旧的全量 `make test`、`make race`、`make cover`、`make it`、`make generate`、DSH runtime 与 Portal 检查结果见下方记录；OpenSandbox 集成因未配置远端环境按约定跳过。

## 验证记录（2026-11-13）

- 服务端 `make lint` 通过（0 issues）；`make generate` 两次连续运行成功，生成物稳定。隔离 PostgreSQL 的完整 `make test` 通过；`internal/store/postgres` 1070.909 秒，`internal/worker` 280.696 秒。全量 Worker 曾有 `TestDSHBrowserUserHandoffResumesSameTaskThroughWorker` 和 prior request candidate 超限的瞬时失败，均经定向和整包复跑通过。
- 隔离 PostgreSQL 的 `make it` 通过：`go test -timeout=45m ./...` 全包通过，`internal/store/postgres` 1303.009 秒、`internal/worker` 253.865 秒；OpenSandbox 集成因未配置远端环境按约定跳过。
- 全量 `make race` 在隔离 PostgreSQL 上通过，设置 `GOFLAGS='-p=1 -timeout=45m'`；PostgreSQL 1879.415 秒、Worker 294.641 秒，其余包通过。最终小幅调整后另以 `-race -count=1` 重跑 `internal/app` 主 Agent 路由组及 PostgreSQL 路由/并发组，均通过（分别 1.791 秒、84.128 秒）。
- 最终隔离 PostgreSQL `make cover` 通过，整体覆盖率 71.6%；PostgreSQL 71.1%，Worker 78.4%。复验期间发现一项并发路由测试在原请求失败后才尝试关联，和目标可关联状态不符；测试改为先关联、再模拟原请求失败、最后从重复消息重试。随后项目存储测试在覆盖率运行时受 30 秒临时数据库初始化上下文限制；将测试夹具上限调至 2 分钟后，全量覆盖复验通过。相关单项覆盖用例也通过。
- DSH runtime `npm test`：106/106 通过。Portal typecheck、ESLint 通过；Assistant/Projects 单测 178/178，连续消息 E2E 1/1 通过。OpenAPI 生成重复运行稳定。
- iOS SPM 消息测试 8/8 通过，Xcode simulator build 通过。最初 UI 登录夹具因认证 API 地址未传入 runner 以 `NSURLErrorDomain -3003` 失败；改用本机临时 scheme 环境后，`testC10PortalMessagePersistsAcrossAppRestart` 通过（1/1）。9 项定向 UI 回归为 8/9；修正附件回执测试的控件类型/重复子节点计数假设后，失败用例独立复跑通过（1/1）。该 UI 用例在 iOS 27.2 Simulator 产生 SwiftUI gravity-well 视图层级运行时警告，测试通过但该警告未在本轮处理。
- 本机浏览器 5173 端口已有演示模式服务占用；未启动或停止该服务。演示模式里提交的 4 条合成消息只存于本机浏览器待发送队列，服务端未收到请求、未创建任务。源码确认蓝色气泡正文直接显示用户消息记录的 `content`；Morde 单独生成重复关联状态，当前格式为“重复请求 · 原消息 … · 关联任务…”。截图中的“合成重复请求：”不是应用生成的状态标签；由于截图处于演示模式，无法据此确认正文是否由真实用户键入。未上传、外发或发送任何项目文件。
- 本轮补跑真实本机 SearXNG/MCP 搜索链路，mcp-searxng 客户端、Morde Broker/网页读取和 Node/DSH Worker 三项合成数据集成测试均通过。扩展 PostgreSQL 竞态集成用例：四条相同/同义请求先基于未创建任务的快照作 `new` 判断，再分别提交重判；隔离 PostgreSQL `-race -count=1` 验证四条消息保留、仅有一个 Task、三条重复消息关联根请求，通过。跨端首轮实测发现第三条消息会因上下文连续变化触发重试上限、第四条可能关联到中间请求；服务端现于上下文过期时先将完全相同且同范围的请求归到最早可加入的根消息，语义近似请求仍重新分类。随后在新的隔离会话中通过真实 PC Portal 页面与 iOS 27.2 模拟器交错发送四条同文请求；页面显示三条重复均关联同一根消息与任务，进度追问得到任务状态且没有新建 Task，独立请求创建并完成独立 Task。Go `TestConversationBrowserAcceptance` 通过（202 秒），iOS `testC10PortalMessagePersistsAcrossAppRestart` 通过（1/1）。共享恢复断言改为按客户端消息 ID 检查，避免将多个独立同文消息误认为同一消息重复渲染。未上传、外发或发送任何项目文件。

执行顺序：OPT-01 → OPT-02 → OPT-03 → OPT-04 → OPT-05/OPT-06 → OPT-07/OPT-08 → OPT-09。PC 与 iOS 客户端文件互不重叠，可在合同确定后分别准备，但端到端验收须等服务端接线完成。任务只有实现及本项验收均完成才可勾选；状态使用未开始、进行中、待验证、完成或阻塞。
