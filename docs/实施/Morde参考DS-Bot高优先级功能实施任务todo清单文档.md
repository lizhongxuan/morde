# Morde 对话体验与 DS-Bot 融合实施任务 TODO

> 来源：[融合设计方案](../Morde对话体验与DS-Bot融合设计方案.md)、[DS-Bot 高优先级需求](../Morde参考DS-Bot高优先级功能需求文档.md)和[原设计](../Morde参考DS-Bot高优先级功能设计方案.md)。本清单保留原 T01–T14 的编号和状态，新增 T15 起的任务以覆盖融合方案；实现及本项验收都完成后才勾选。

## 修改边界

- 基于现有 Portal、PC Electron、iOS、Go 服务、DSH Runtime 和 Host/Workspace 链路补缺；主会话显示主 Agent 面向用户的信息，计划、工具过程和子 Agent 活动进入任务动态。保留多会话、授权、模型网关、审计、检查点及恢复机制。
- 调整 Agent 能力前检查 `runtime/morde-cloud-runtime/package.json`、锁文件及 `project/deepseek-harness/` 对应上游组件；有兼容插件优先复用，接入时固定版本并核查许可证，不符合条件时记录原因。验证受影响的会话、检查点、取消和恢复，不能绕过 Morde 授权与工具代理。
- [第一里程碑 A/B/C 清单](Morde第一里程碑收口三会话并行实施/)已有实现及本地测试记录，本清单复用其完成部分；发现缺口时在对应模块修复，不重新实现一套任务或同步系统。保留工作区已有修改和各清单的状态。
- 按用户要求只在本地修改，不向服务、电脑、设备或远端发送文件，不部署或上传。测试仅使用不传文件的本地替身、合成元数据和隔离环境；不能安装或发送应用包到设备。真实文件数据面保持待验证，`project_upload_enabled` 保持关闭。
- 涉及 PC 页面时同时使用内置浏览器和**实际 PC 客户端**验收；网页替身通过不能代替 PC 客户端验收。iOS 只使用现有本地可运行条件；需要安装或发送文件时记待验证。
- P0 优先完成本地实现和能执行的验收；受文件发送限制的真实数据面保留待验证，不阻断相互独立的 P1 本地工作。方案提出的万人在线对话压测单列，不能用替身或本机容量推断生产能力。

### 并行时的文件责任

- 服务端与云端 DSH：`server/**`、`runtime/morde-cloud-runtime/**`、迁移及 OpenAPI 由同一负责人管理；不同任务需要同一文件时顺序处理。
- PC：`web/portal/**` 和 `clients/desktop/**` 由同一负责人管理，包括 Portal API 类型生成；不能同时修改 `Assistant.tsx` 或 `api.ts`。
- iOS：`clients/nanoMuse/android/src/ios/**` 由同一负责人管理，新增可见文字同步 `Localizable.xcstrings`。Host 与同步 Runtime：`runtime/morde-runtime/**`、`runtime/morde-sync/**` 和相关运行环境由独立负责人管理。
- 本清单由集成负责人更新。公共合同先由服务端负责人固定，再由 PC/iOS 同步调用方；跨边界修改交对应负责人，不并发覆盖同一文件。

## 接口变化

| 接口 / 调用方 | 变化与约束 |
| --- | --- |
| `POST /api/conversations/{id}/messages` 与消息路由 | **已有**；保持 202 持久受理、`Idempotency-Key=client_message_id` 和已有 `reference_task_id`。T15 在服务端按账号、会话与执行范围持久聚合已受理消息，结算幂等；不改变普通问答和停止的即时处理。 |
| `GET /api/conversations`、Portal `conversations(cursor, limit, q)` | 服务端 `q` 已有，按当前账号授权匹配标题；Portal 调用新增可选 `q`，查询改变重置游标并忽略旧响应。 |
| `GET /api/personal-data/{type}/{id}/history` | **拟新增**，仅 `type=memory`；沿用当前个人对象读取的 Portal Session / Client 授权，接收 `cursor`、`limit`，按版本倒序返回 `items`（`revision`、`changed_at`、`body` 或 null、`restorable`、来源状态、已授权来源）及 `next_cursor`，游标绑定账号和记忆。参数无效 400、凭证无效 401，不存在、已删除或无权访问 404；失权来源不返回内容。未保存正文的旧版本不可恢复。 |
| `PUT /api/personal-data/{type}/{id}` | **已有**接口，`type=memory` 时复用为恢复：提交所选历史正文、当前 `base_revision` 和幂等键；成功生成新版本，版本冲突不覆盖。删除保留墓碑和历史抑制，同时清理该记忆历史正文。 |
| `GET /api/tasks/{id}/artifacts/{artifact_id}/content` | **已有** Portal Session 下载接口；PC 复用 Task 和固定 Workspace 版本、路径、大小、摘要的校验。iOS 使用 Task 产物元数据及已有受限预览；预览条件不满足时只显示元数据，不扩展 Client 凭证访问。 |
| `GET /api/tasks/{id}`、`GET /api/tasks/{id}/steps/{step_id}` | **已有，拟扩展**；分别增加可用计划、优先级、费用状态及步骤错误/尝试/可重试投影。未知或未记录时返回明确状态，步骤详情继续按用户和任务授权过滤。 |
| `POST /api/tasks/{id}/steps/{step_id}/retries` | **拟新增，路径待 OpenAPI 固定**；携带当前任务修订、预期尝试及幂等键，成功返回受理身份/尝试，不代表执行完成；授权失效、修订过期或不可安全重试应拒绝。结果未知的操作不得重跑。 |
| `POST /api/conversations/{id}/messages/{message_id}/variants` | **拟新增，路径待 OpenAPI 固定**；请求包含编辑重发、重新生成或继续生成的动作、来源消息身份及预期版本/序号和幂等键，编辑重发还需新内容；返回新消息/版本的受理身份。消息列表拟增加版本关联字段。来源不可访问或状态冲突时拒绝，保留旧版本，不自动重做原任务的副作用。 |
| Task 审批、`POST /api/tasks/{id}/actions` | **已有，拟扩展**审批的可用风险、费用和过期状态，以及携带目标优先级、预期版本和幂等键的优先级调整；仍绑定具体任务/操作、版本和权限，费用未知不伪造，账号硬限额不被审批绕过。 |
| Task 事件、Host 控制、项目上传和 Desktop IPC | **已有**；沿用当前合同。若实施确认现有字段无法表达必要事实，先同步 OpenAPI/公共约定和所有调用方，再修改实现。 |

新增或扩展接口须同步 `server/openapi/portal.yaml`、`docs/实施/00-总览与公共约定.md`、受影响的 Client 凭证路由白名单、服务端、Portal 生成类型及 iOS 调用方。新增持久数据使用新迁移；实施时核对最新迁移编号，避免与现有工作冲突。

## P0：第一里程碑

- [ ] **T01（待验证）调整 PC 对话界面**〔需求 1〕
  - 修改：`web/portal/src/routes/Assistant.tsx`、`web/portal/src/routes/AssistantSidebar.css`、`web/portal/src/components/PortalLayout.tsx` 和相关样式；必要时调整 `clients/desktop/harness/desktop/` 的窗口或桥。迁移并适配 DS-Bot 的布局、消息、输入和卡片，接 Morde 真实数据；主会话显示主 Agent 答复与必要进度，工具和子 Agent 过程进入任务动态，保留主会话/旁聊、任务、电脑、文件及草稿。
  - 验收：用内置浏览器和实际 PC 客户端检查常规/窄窗口、键盘、侧栏、滚动锚点、输入框和两个会话的草稿与任务归属；任务执行时主会话无工具日志，仍可答复追问，网页与 PC 结果分别记录。
  - 当前证据：Portal Assistant 144 项测试、ESLint、TypeScript 检查通过；本机浏览器已检查空主会话欢迎态及主会话/旁聊草稿隔离。Electron 同步页新增「返回 Morde 对话」入口，并在通用 API 错误中显示 HTTP 方法/路径；桌面构建及核心/渲染器/主进程定向测试 43 项通过。测试服务无凭据只读探测：`/assistant` 携带 HTML Accept 返回 200，`/api/workspaces` 返回 401（路由可达）；未以已登录 PC 会话复现截图中的 404。实际 PC 对话页、窄窗口和 HTTP 404 来源仍待验收。

- [ ] **T02（待验证）核对跨端会话与长上下文**〔需求 2〕
  - 修改：按缺口调整 `server/internal/app/conversation_*`、`server/internal/store/postgres/conversation_*`、`runtime/morde-cloud-runtime/src/engine.mjs`、Portal outbox 及 `clients/nanoMuse/android/src/ios/Morde/` / `clients/nanoMuse/android/src/ios/NanoMuse/`。复用已有主会话、持久消息、压缩、授权历史和检查点；项目记忆只供关联项目且获权的任务读取，PC 同步目录不自动取得项目权限。
  - 验收：不发送文件地用合成消息和模型替身覆盖超窗口续聊、原文分页、两账号/会话及个人/项目隔离、记忆关闭或撤权及关闭客户端后重连；纯问答不要求电脑。真实 PC/iOS 与真实模型结果单独记录，未运行不标完成。
  - 当前证据：Runtime 压缩/恢复 7 项、服务端会话历史与授权测试、PostgreSQL 长历史/项目记忆 11 项、Portal outbox 7 项通过；真实 PC/iOS 与真实模型闭环待验收。

- [x] **T03（完成，依赖 T02）核对执行中消息路由和去重**〔需求 3〕
  - 修改：按缺口调整 `server/internal/app/conversation_route_*`、`server/internal/store/postgres/conversation_route_*`、Portal 消息 outbox 和 iOS 消息投影。保留消息受理与 Task 创建的区别，按目标、附件、项目、电脑及授权范围关联；可选使用已有 `reference_task_id` 指名任务，明确重跑可新建工作，修订迟到结果不能覆盖新版本。
  - 验收：本地替身覆盖追问、修改、状态、停止、独立任务、关联不明和重复请求；同话题不同目标不合并，两个任务中停止一个不影响另一个，同键重试不重复创建或执行。
  - 验收证据：conversation route/app/API 定向 Go 测试通过；独立本机 PostgreSQL 上同范围路由、并发修订、停止隔离和消息重试围栏集成测试通过；Portal Assistant 与 outbox 定向测试 152 项通过。未连接真实模型/远端 Host。

- [ ] **T04（待验证，依赖 T02）核对子 Agent 并行与结果恢复**〔需求 4〕
  - 修改：按缺口调整 `runtime/morde-cloud-runtime/src/engine.mjs`、`server/internal/app/agent_sessions.go`、`server/internal/worker/cloud_dsh_session_tree.go` 及相关结果汇总代码；参考 DS-Bot 的投递、完整结果和恢复行为，沿用 DSH、授权子集、租约和检查点。
  - 验收：本地 DSH 与模型替身实际派发两个互不冲突的子任务，检查并行、完整结果/失败回传、依赖衔接、重启与取消隔离；结果未知时不重复执行。真实模型协作未验收时单独列出。
  - 当前证据：本地双子任务并行、结果/失败回传、检查点恢复、取消隔离，以及 app/worker 幂等围栏测试通过；新增断言确认恢复后父 Agent 收到失败摘要且不会重放工具。DSH 上游子 Agent 插件不覆盖 Morde 的任务授权、租约与恢复边界，因此未新增依赖。真实模型验收待补。

- [ ] **T05（待验证，依赖 T01、T03、T04）接通进度、审批和控制展示**〔需求 5〕
  - 修改：`web/portal/src/routes/Assistant.tsx`、消息/Task 投影和 `clients/nanoMuse/android/src/ios/NanoMuse/MIFTaskViews.swift`；按现有快照与事件序号显示当前会话的任务、子任务、审批、澄清和控制结果，游标失效时重新取快照。
  - 验收：内置浏览器及实际 PC 客户端使用本地替身检查两个任务的归属、审批/拒绝、停止、断线续读、快照恢复和未知状态；iOS 在现有可运行条件下检查同一合同。更新本地化文字。
  - 当前证据：Portal Assistant 任务归属、审批/澄清、停止隔离、断线续读、快照恢复定向测试 10 项通过；服务端任务事件游标、过期快照、终态恢复及字段白名单 API 测试通过。当前实现已包含 iOS 事件链和未知状态展示；本轮未安装或发送应用包到设备，实际 PC 客户端、内置浏览器与 iOS 页面验收仍待执行。

- [ ] **T06（待验证，依赖 T05）核对指定电脑和常用工具**〔需求 6〕
  - 修改：按缺口调整 `server/internal/app/runtime_tool_executor.go`、`server/internal/app/cloud_dsh_*`、`runtime/morde-runtime/runtimebrowser/`、`runtime/morde-runtime/runtime_desktop/`、`runtime/morde-runtime/runtimeactions/` 和 PC 电脑面板；继续按 Host、授权和控制版本围栏执行。
  - 验收：本地替身检查指定目标、接管/交还、权限撤销、搜索来源、命令输出及不可用/未知状态；内置浏览器和实际 PC 客户端分别查看面板。真实指定 Host 操作和真实搜索未验证时标明。
  - 当前证据：`runtime_tool_executor.go`、runtimebrowser/runtime_desktop、runtimeactions 定向替身测试已通过，覆盖 Task 目标、授权/Host 版本、接管交还、未知操作不重放和命令结果。代码已提供受任务授权约束的 `morde_web_search` / `morde_web_read`；已有本地替身用例验证搜索结果含公开来源 URL、读取来源页面、结果绑定 Task 持久化并可安全回放。Portal 侧已有目标电脑选择、一次性工作台票据、共享电脑权限白名单及停止电脑状态测试。真实 Host 操作、真实搜索、实际 PC 页面和搜索集成用例仍待验收。

- [ ] **T07（待验证，依赖 T06）核对 PC 手动项目同步**〔需求 7〕
  - 修改：按缺口调整 `clients/desktop/harness/desktop/src/morde-main.ts`、`clients/desktop/harness/desktop/src/morde-sync.ts`、`clients/desktop/harness/desktop/resources/morde.js`、`server/internal/app/workspace_sync_project_upload.go`、`server/internal/store/postgres/workspace_sync*.go`、`runtime/morde-sync/` 和 `runtime/morde-runtime/workspacereplica/`。沿用现有 source binding、Host 确认基线和 session 回执。
  - 验收：内置浏览器及实际 PC 客户端配合合成元数据/替身检查选择、确认、手动触发、冲突、不可访问、撤权、待应用及已应用；不监听/定时、不反向写 PC，未知回执不报完成。真实文件同步受当前限制保持待验证。
  - 当前证据：PC 已有显式选择本机文件夹与云端电脑、确认后单向上传、冲突/Host 回执状态及重启恢复入口；服务端上传能力默认关闭，现有同步及应用回执保留版本、授权和恢复点围栏。桌面 renderer 的同步状态/项目回执定向测试通过。按“只在本地修改、不发送文件”的约束，本轮未开启 `project_upload_enabled`、未执行文件数据面；实际 PC、真实上传/应用与冲突验收仍待验证。

- [ ] **T15（进行中，依赖 T03）实现连发消息的安全短时聚合**〔机制 10〕
  - 修改：`server/internal/app/conversation_route_*`、`server/internal/store/postgres/conversation_route_*`、必要的迁移/事件投影及 PC/iOS 消息状态。每条消息先受理并保持身份；仅将目标、范围及授权一致的补充放入按账号/会话隔离的有界窗口，复用路由队列及幂等结算；任务运行后的补充走原修订链。窗口时长通过样本回放确定并允许提前结算。
  - 验收：本地模型替身覆盖连发补充只关联一次、不同目标不合并、窗口内明确停止/普通对话/独立任务及时处理、服务重启恢复和重复结算；两账号、不同附件及目标电脑不串用。

- [ ] **T16（待验证，依赖 T05）完善任务事件时间线与工具详情**〔原则 4、5；机制 1〕
  - 修改：`server/internal/api/task_events.go`、`server/internal/app/task_step_detail.go`、`server/internal/store/postgres/task_step_details.go` 及 PC/iOS 任务视图；复用现有写入类型校验、序号和授权投影，按时间/步骤或子任务展示同一批实际记录。按工具类型提供已记录且获准查看的对象、输入、结果、可用差异或截断提示，未知内容不编造。
  - 验收：内置浏览器和实际 PC 客户端查看成功、失败、等待与恢复事件及可用详情；安全未知事件不丢游标，跨账号/撤权、凭据和内部推理不泄露。iOS 在现有可运行条件下核对任务归属和状态。
  - 当前证据：修复 SSE allowlist 字段的 JSON 类型校验及嵌套 `task.step.detail` 子字段过滤；恶意对象/数组字段回归测试先复现再验证修复。定向 API 事件和 app 步骤详情测试通过。PostgreSQL 详情 HTTP 集成测试未运行；Portal/iOS 时间线与实际 PC 页面验收待执行。

- [ ] **T08（待验证，前置任务未完成；依赖 T01–T07、T15、T16）汇总第一里程碑验收**
  - 修改：[现有 A-07 验收记录](Morde第一里程碑收口三会话并行实施/A-云端Agent与公共合同实施任务todo清单文档.md)或其后续记录；列出当前候选版本、实际 PC/iOS、真实模型和 Host 与本地替身的证据及未完成项。
  - 验收：按根目录 AGENTS.md 逐项核对；真实文件发送被禁止，相关同步数据面不能执行、不能宣布里程碑完成。P1 可继续进行不依赖真实文件发送的本地工作。

## P1：优先补强

- [ ] **T09（待验证，依赖 T01）接入会话标题搜索**〔需求 8〕
  - 修改：`web/portal/src/app/api.ts`、`web/portal/src/routes/Assistant.tsx` 和相关样式/测试；复用已有 `q`，查询改变时重置分页，过期请求不能覆盖新结果，搜索不清除草稿。
  - 验收：内置浏览器和实际 PC 客户端检查标题搜索、多个结果页、切换会话、空结果与失败；不同账号或已撤权内容不可见。
  - 当前证据：复用服务端已存在的授权标题查询；查询词纳入 React Query key，变更后从第一页读取，旧查询结果不会覆盖当前查询。Assistant/API 定向测试 191 项、ESLint、TypeScript 检查通过；手动 PC 与内置浏览器验收待补。

- [x] **T10（完成）实现记忆历史与来源合同**〔需求 9〕
  - 修改：`server/migrations/`、`server/internal/domain/personal/`、`server/internal/app/personal_*`、`server/internal/store/postgres/personal_mutations.go` / `server/internal/store/postgres/personal_sync.go` / `server/internal/store/postgres/personal_quota.go`、API、Client 凭证路由白名单和 OpenAPI。复用 `personal_memory_sources` 与变更日志，新增版本正文存储及只读历史接口；首次修改旧记忆时保留修改前可用版本。恢复使用现有 PUT 的 CAS 和幂等键，历史写入/配额与对象修改原子提交；历史正文计入配额，删除时清理正文、释放配额并保留既有墓碑和抑制。
  - 验收：本地隔离数据库及合成文本覆盖来源缺失、历史分页、恢复、并发 409、配额不足、删除/关闭记忆与跨账号隔离；不将历史正文供未授权任务读取。
  - 验收证据：历史查询 API、签名分页游标、来源失权隐藏、首改快照、配额事务及删除清理已实现；domain/app/API 定向测试通过；本机独立 PostgreSQL 上 4 项历史集成测试通过，OpenAPI 可解析。Portal 历史界面由 T11 覆盖；iOS 沿用现有个人数据变更同步。

- [ ] **T11（待验证，依赖 T10）接入记忆历史界面**〔需求 9〕
  - 修改：`web/portal/src/routes/PersonalData.tsx`、`web/portal/src/routes/personal-data-store.ts`、`web/portal/src/app/api.ts` 和生成类型；展示来源、时间、可恢复版本及冲突，删除前提示历史正文一并清理且不能恢复。iOS 沿用个人数据同步读取恢复后的新版本。
  - 验收：内置浏览器和实际 PC 客户端检查修改、历史查看、恢复、无来源、并发冲突及删除；iOS 现有同步逻辑在可运行条件下看到已确认的新版本，不覆盖未提交草稿。
  - 当前证据：Portal 已接入历史分页、来源/不可恢复状态显示、沿用现有 CAS/幂等个人数据队列恢复，删除前说明历史正文将被清理且不可恢复；PersonalData、个人数据存储和 API 定向测试 87 项、ESLint、TypeScript 检查通过。手动浏览器/PC/iOS 验收待补。

- [ ] **T12（未开始，依赖 T06）接入 Office 执行能力**〔需求 10〕
  - 修改：`runtime/morde-cloud-runtime/` 依赖与技能适配、`server/internal/app/cloud_dsh_skills.go` / 工具代理、授权执行环境和相关产物代码。先验证上游 `@deepseek-ai/dsh-skill-office@0.2.2-rc.1`、`skills` 依赖及许可；按需适配 DS-Bot Office。解释器、文档库和渲染组件须在授权环境可用，所有文件操作走 Morde 工具和 Workspace。
  - 验收：在本地执行环境内直接创建、检查、修改、打开 DOCX/XLSX/PPTX 合成样例并检查内容；验证缺依赖和无权限时真实失败。不得将样例文件发送到服务或设备；云端文件数据面待验证。

- [ ] **T13（未开始，依赖 T12）接入已核验 Office 产物查看和下载**〔需求 10〕
  - 修改：按缺口调整 `web/portal/src/routes/Assistant.tsx`、`clients/nanoMuse/android/src/ios/NanoMuse/MIFTaskViews.swift`、`clients/nanoMuse/android/src/ios/NanoMuse/MIFLibraryViews.swift` 和本地化。PC 复用现有产物下载入口，展示可用的检查或预览结果；iOS 展示同一已确认产物，满足现有授权和本地校验条件时复用预览，否则显示元数据和提示；未核验产物不能显示为已确认。
  - 验收：本地合成元数据和替身覆盖账号/权限撤销、路径与摘要不符；内置浏览器和实际 PC 客户端检查产物状态、查看/下载入口和失败提示，iOS 在现有可运行条件下检查相同元数据。实际文件查看、下载或跨端传输保持待验证。

- [ ] **T17（未开始，依赖 T04、T16）实现安全的步骤级重试**〔机制 3〕
  - 修改：`server/internal/domain/task/`、`server/internal/app/task_*`、`server/internal/store/postgres/task_*`、`server/internal/api/`、OpenAPI、必要迁移及 PC/iOS 任务视图。为失败步骤保存错误、尝试、可重试状态和可核验的原调用身份；按接口表新增步骤重试，复核账号、授权、任务修订、前置结果及幂等记录，通过 DSH 和现有工具代理恢复。
  - 验收：本地隔离环境对满足条件的失败步骤重试，仅该步骤产生一次新尝试；相同幂等键重试和并发点击不重复执行；不可重试、授权失效、历史输入不可核实或副作用未知时拒绝并显示原因。内置浏览器和实际 PC 客户端检查入口，iOS 在可运行条件下核对状态。

- [ ] **T18（未开始，依赖 T01、T03）实现回复版本与消息操作**〔机制 3、7〕
  - 修改：`server/internal/app/conversation_*`、`server/internal/store/postgres/conversation_*`、`server/internal/api/`、OpenAPI、必要迁移、`web/portal/src/routes/Assistant.tsx`、`web/portal/src/app/message-outbox.ts` 及 iOS 消息投影。按接口表实现编辑重发、回复重新生成和继续生成，保存来源与新消息/版本；已完成 Task 的结果必须重新核权，文字重生成不自动重复工具副作用，继续生成前须核实原调用状态及可用检查点。
  - 验收：内置浏览器和实际 PC 客户端检查编辑后原文仍可查、回复版本切换、继续生成、断线重试与滚动位置；本地替身核对不同会话/账号隔离、重复请求去重及不可核实结果不重做操作。iOS 在可运行条件下读取相同已确认版本。

- [ ] **T19（未开始，依赖 T04、T16）接入复杂任务计划展示与修订**〔机制 9〕
  - 修改：`runtime/morde-cloud-runtime/`、`server/internal/app/cloud_dsh_*`、Task 服务/存储、OpenAPI、必要迁移和 PC/iOS 任务视图。由 DSH 主 Agent 按需生成任务范围的计划及完成条件；持久保存计划版本、步骤身份、无循环的依赖和实际进度投影，未确认费用/审批标未知。复用现有对话修订和 Task 依赖；计划步骤与实际工具步骤分开，不新建执行循环。
  - 验收：本地 DSH/模型替身完成复杂任务的计划、实际进度与结果；中途修订保留已完成产物和旧记录，旧修订结果不能覆盖新计划；简单任务不被强制计划确认阻塞。内置浏览器和实际 PC 客户端检查计划与修订，iOS 在可运行条件下检查同一投影。

- [ ] **T20（待验证，依赖 T05）补齐审批上下文与到期状态**〔机制 4〕
  - 修改：`server/internal/domain/task/`、Task 审批服务/存储与 API、OpenAPI、必要迁移、`web/portal/src/routes/Assistant.tsx`、`clients/nanoMuse/android/src/ios/NanoMuse/MIFTaskViews.swift` 和本地化。按接口表展示真实风险原因、对象、影响、可用费用、替代方案和撤销方式；为按策略设置有效期的审批持久化并处理过期，无时限时不伪造到期时间，绑定任务、请求版本和具体操作，不扩大授权范围。
  - 验收：本地替身覆盖批准/拒绝、到期、重复提交、版本过期、权限失效和未知费用；批准计划不能自动批准后续敏感动作。内置浏览器与实际 PC 客户端检查卡片及任务恢复，iOS 在可运行条件下核对。
  - 当前证据：新增 `expires_at` 与 `expired` 状态；没有可信到期策略时保持期限为空。批准响应事务在数据库时钟下复核期限，已过期请求原子转为 expired/拒绝并重排 Task，旧客户端审批返回冲突；DSH 工具、MCP、Workspace 写入、媒体保存和恢复路径均 fail closed，不派发过期动作。Task 详情只读保留最新过期快照；PC/iOS 显示期限、过期状态和无可用工具上下文时的费用/替代/撤销未知提示，过期按钮不可用。Go app/API 测试、Portal Assistant 146 项测试和 TypeScript 检查通过；Go PostgreSQL 包通过，但因 `CH_TEST_DATABASE_URL` 未配置，数据库过期事务集成用例未执行。iOS Swift 语法/日期/本地化检查通过；`xcodebuild build-for-testing` 因缺少本机配置与 XCFramework 未进入编译。实际 PC 浏览器、PC 客户端与 iOS 真机验收待补；当前没有策略为请求设置非空期限。

- [ ] **T21（未开始，依赖 T05）接入任务用量、费用与预算展示**〔机制 5〕
  - 修改：`server/internal/app/model_gateway*`、`server/internal/app/project_budget*.go`、`server/internal/store/postgres/project_budget.go`、Task 投影/OpenAPI、必要迁移和 PC/iOS 任务视图。复用现有 Token 账本和项目金额/费率快照，按账号、任务及模型来源汇总调用次数、用量、时长和可获得的费用；区分已结算、估算、未知和自带模型，不改变硬限额或引入虚假扣费。
  - 验收：隔离账本覆盖项目/个人任务、无价格、未知使用、费率变更后的历史快照及跨账号访问；两个实例汇总同一记录一致。内置浏览器与实际 PC 客户端检查显示，iOS 在可运行条件下核对相同状态。

- [ ] **T22（未开始，依赖 T04）补齐任务排队、公平与能力匹配**〔机制 8〕
  - 修改：`server/internal/store/postgres/task_execution.go`、Task 服务与 API、必要迁移、`runtime/morde-cloud-runtime/` 和 PC/iOS 任务视图。在现有租约、按用户并发限制和资源栅栏上增加用户置顶、普通、后台优先级及防饥饿规则；按实际授权的工具和资源选择子 Agent 执行上下文，只有可计算时显示队列位置。
  - 验收：本地多 worker 检查同级顺序、置顶、低优先级最终被领取、单账号并发限制及取消隔离；无权限的工具/电脑不被匹配。内置浏览器和实际 PC 客户端检查排队及置顶状态，iOS 在可运行条件下核对。

- [ ] **T23（未开始，依赖 T15、T17、T20–T22）补齐必要指标与降级状态**〔机制 1、3、4、5、8、10〕
  - 修改：`server/internal/worker/metrics.go`、`server/cmd/portal/main.go` 中的现有指标注册，以及消息路由、Task 领取/事件、审批与模型账本的观测出口；记录聚合等待/纠正、排队、事件续读、审批等待、重试与用量。指标不包含消息正文、凭据或原始工具输出，容量不足时区分排队、展示降级和明确拒绝。
  - 验收：本地隔离环境使用合成事件检查指标对应的真实状态；重启、限流或禁用非必要展示时，已受理任务、结果、账本和审批不丢失、不被跳过。

- [ ] **T24（待验证，依赖 T23）执行万人在线对话容量验收**
  - 修改：本地负载脚本与验收记录，分别定义在线连接数、消息速率、并行任务数、模型调用和长任务比例；覆盖短聊、连发、审批与断线重连混合场景。
  - 验收：在符合当前本地文件限制的隔离环境记录实际延迟、错误率和资源用量；环境不足或无法达到目标时保留待验证，不能宣称生产已支持万人规模。不向远端发送脚本、样例或文件。

- [ ] **T14（待验证，前置任务未完成；依赖 T09–T13、T17–T23）汇总 P1 验收**
  - 修改：本地验收记录，分别记录搜索、记忆、Office、计划、重试、费用与队列的证据；规模容量由 T24 单独记录。
  - 验收：P1 各项真实行为均有对应证据后才标完成；当前不能发送 Office 样例或产物到服务/设备，相关交付与跨端文件验收保留待验证。

## 机制与参考项目对应

五条原则分别由 T01/T05、T03/T04、T03/T15、T05/T16、T16/T17 落实。以下参考项目的链接见[融合方案第 7 节](../Morde对话体验与DS-Bot融合设计方案.md)；只参考与该任务有关的体验或组件，执行能力继续使用 DSH 和 Morde。

| 融合方案机制 | 实施任务 | 参考方向 |
| --- | --- | --- |
| 1 事件流与任务过程 | T05、T16 | OpenHands、Dify、LobeHub 的事件分组与展示 |
| 2 会话持久化与恢复 | T02、T08 | LibreChat、hermes-agent 的恢复和消息交接 |
| 3 幂等与安全重试 | T03、T17、T18 | AnythingLLM、bolt.diy、Chatbox 的去重、错误和重试体验 |
| 4 审批关口 | T05、T20 | assistant-ui、Chatbox 的审批卡和状态 |
| 5 成本与预算 | T21 | hermes-agent 的用量展示；复用 Morde 现有账本 |
| 6 记忆连续 | T02、T10、T11 | Muse、DS-Bot 的记忆管理与来源 |
| 7 布局与流式稳定 | T01、T18 | DS-Bot、LibreChat、LobeHub、Open WebUI、assistant-ui 的对话交互 |
| 8 多任务与能力匹配 | T04、T22 | Muse、DS-Bot、DSH 的子 Agent 委派与结果 |
| 9 复杂任务规划 | T19 | Muse 的任务规划体验和 DSH 的现有能力 |
| 10 对话任务更新 | T03、T15 | hermes-agent 的消息交接及 Muse 的跟进体验 |

DS-Bot 的 Office 模块和 DSH Office 技能对应 T12–T13；指定电脑和文件夹同步由 T06–T07 覆盖，规模指标与容量由 T23–T24 分别覆盖。
