# Morde 对话体验与 DS-Bot 融合实施任务 TODO

> 依据：[融合设计方案](../Morde对话体验与DS-Bot融合设计方案.md)、[PC 布局需求](<../Morde PC端布局与云端电脑体验需求文档.md>)和根目录 [AGENTS.md](../../AGENTS.md)。
>
> 任务编号和初始状态承接[原高优先级实施清单](Morde参考DS-Bot高优先级功能实施任务todo清单文档.md)的 2026-11-14 工作树快照；原清单保留详细测试记录，后续状态与新证据见各任务条目。工作树已有未提交的改动，实施前先核对现状，避免重复开发或覆盖。

## 修改边界与执行约定

- 面向 PC 的长期主会话，复用 Portal、Desktop 外壳和 iOS 云端数据；每个会话由主 Agent 负责，任务、消息与结果归属清楚。主会话呈现必要回复、进展和结果，工具、计划和子 Agent 过程进入任务动态；不引入 Bot 群聊、团队、IM 或本地 Agent CLI。
- 云端统一使用 DSH 和 Morde 模型网关，沿用消息路由、Task、工具代理、授权、审批、审计、租约与检查点。调整 Agent 前检查 `runtime/morde-cloud-runtime/package.json`、锁文件与 `project/deepseek-harness/` 对应组件；优先复用兼容插件，新增或升级时核对版本和许可证，并验证会话、取消与恢复。
- 五条原则分别落实为：主会话简洁（T01、T05）；主 Agent 持续交互和按需协作（T03、T04）；过程按任务归属（T03、T15、T16）；真实过程可追溯（T05、T16）；工具详情按权限下钻（T16、T17）。无记录或无法确认的信息显示未知，不展示凭据或模型内部推理。
- 依照用户约束只在本地修改，不向服务、电脑、设备或远端发送文件，也不部署或上传。可用本地替身、合成文本和元数据验证；真实文件同步、产物跨端传输或需要安装应用包的验收保持待验证，`project_upload_enabled` 保持关闭。
- PC 功能优先；涉及 PC 页面须分别在内置浏览器和实际 PC 客户端验收。iOS 在现有本地可运行条件下验收，不能用网页结果代替两端结果。P0 优先；被真实文件传输限制的验收不阻断相互独立的 P1 本地实现。
- 状态仅用“未开始、进行中、待验证、完成、阻塞”；实现和本项验收均完成才勾选。依赖表示所需实现或合同先可用，汇总验收则要求相关项全部验收；本地替身、真实设备、真实模型和 Host 的证据分别记录。

### 并行修改边界

并行实施时，`server/**` 与 `runtime/morde-cloud-runtime/**`（含迁移、OpenAPI）由一个负责人串行修改；`web/portal/**` 与 `clients/desktop/**` 由一个负责人管理；`clients/nanoMuse/android/src/ios/**` 与 `runtime/morde-runtime/**`、`runtime/morde-sync/**` 分别由各自负责人管理。本清单和公共约定由集成负责人维护；先固定服务端合同，再更新 PC/iOS 调用方，避免同时修改同一文件。

## 接口与数据变化

“已有”指当前工作树存在，不代表已部署或完成真实验收。拟新增接口的最终路径和错误码以实施时的 OpenAPI 为准。

| 接口 / 数据 | 修改与调用方 |
| --- | --- |
| `POST /api/conversations/{id}/messages`、消息列表 | 已有。保留消息持久受理后的 202、`Idempotency-Key=client_message_id` 和 `reference_task_id`；T03/T15 在现有路由上关联、修订和短时聚合，不改变每条原始消息身份。PC/iOS 显示受理、失败及关联状态。 |
| `GET /api/conversations?q=...` | 已有标题搜索；T09 核对 Portal 的 `q`、分页和旧响应隔离，只返回当前账号有权查看的会话。 |
| `GET /api/personal-data/{type}/{id}/history`、`PUT /api/personal-data/{type}/{id}` | 当前工作树已有。T10/T11 保持仅 `memory` 历史分页、账号与记忆绑定的游标、授权来源和可恢复状态；恢复复用 PUT 的当前 `base_revision` 与幂等键。分页参数无效返回 400，凭证无效返回 401，无权或已删除返回 404，版本冲突不覆盖。 |
| `GET /api/tasks/{id}`、`GET /api/tasks/{id}/events`、`GET /api/tasks/{id}/steps/{step_id}` | 已有。T05/T16 继续按 Task、事件序号和权限读取快照、事件与步骤详情；T17/T19/T21/T22 按需扩展步骤错误和尝试、计划、用量及队列投影，未记录的数据标明未知。 |
| `POST /api/tasks/{id}/steps/{step_id}/retries` | 当前工作树已实现（T17）：客户端提交任务修订、预期尝试和幂等键；服务端从原步骤读取调用摘要，校验授权、前置结果与副作用后返回 attempt。重复请求复用同一 attempt，受理不代表执行成功；全链路实际 UI 验收仍待完成。 |
| `POST /api/conversations/{id}/messages/{message_id}/variants` 及项目会话对应路由 | 当前工作树已实现（T18）：请求 `{action, expected_version, content?}`，`action` 为 `edit_resend`、`regenerate_text` 或 `continue_generation`；以 `Idempotency-Key` 受理，返回 `operation_id`、`variant_group_id`、`variant_version`、`action`、`state` 及可用的 Task/消息 ID。`GET /messages` 返回全部版本行并投影 `variant_group_id`、`variant_version`、`variant_action`。旧内容保留；文本重生成不自动重做工具，继续生成只恢复可核实的 DSH 检查点。 |
| Task 审批及 `POST /api/tasks/{id}/approvals/{approval_id}/responses` | 已有；T20 核对与任务、请求版本和具体动作绑定的审批上下文。当前工作树已有可空 `expires_at` 与 `expired` 状态；无可信期限时不伪造，过期、权限变化和重复提交须按事务事实处理。 |
| Task 动作、队列投影 | 已有 Task 动作；T22 拟增加带目标优先级、预期版本和幂等键的调整，以及可计算时的队列位置。不得突破现有用户并发、授权和资源约束。 |
| 项目上传 session、Host 应用回执和 Desktop IPC | 已有；T07 核对每次手动同步的 Host、Workspace、目标版本、清单摘要与运行代次，只有对应 session 的应用回执才能显示完成。 |
| `GET /api/tasks/{id}/artifacts/{artifact_id}/content` | 已有 Portal Session 下载；T13 复用固定 Workspace 版本、路径、大小和摘要校验。iOS 沿用现有授权元数据与受限预览，不新增越权下载通道。 |

涉及 HTTP、状态机或持久结构的变化，同步 `server/openapi/portal.yaml`、[公共约定](00-总览与公共约定.md)、服务端、受影响的 Client 凭证路由白名单、Portal 生成类型和 iOS 调用方；新增持久数据时先核对最新迁移编号。

## P0：第一里程碑

- [ ] **T01（待验证）PC 对话工作界面**
  - 修改：`web/portal/src/routes/Assistant.tsx`、`AssistantSidebar.css`、`components/PortalLayout.tsx` 及 `clients/desktop/harness/desktop/`。参考 DS-Bot：左侧放导航、主会话和旁聊，中间放当前对话，右侧放任务动态、电脑和文件；接入真实数据并保留个人数据、目标与计划、模型与工具及设置入口，以及各会话草稿、侧栏切换、键盘操作和阅读锚点。
  - 验收：内置浏览器和实际 PC 分别检查常规/窄窗口、主会话与旁聊、两个会话的草稿和任务归属、滚动及任务动态；从文件同步页可返回对话，加载失败显示真实原因。
  - 当前证据与缺口：Portal 的 `FloatingError`、`FloatingNotice` 和 toast 统一进入默认收起的顶栏提示中心，不改变页面布局；修正顶栏与任务侧栏/Composer 的层叠顺序，Playwright 验证提示可点击关闭。WorkBench 的错误与成功提示共用固定提示层，手机端网络错误使用绝对定位；登录、注册和表单字段校验仍贴近对应表单。原完整 Workspace 文件、版本与恢复视图已集成到 PC 用户端“云端电脑 > 文件同步”，旧 `/workspaces`、`/local-projects` 地址重定向到此入口，电脑管理页默认电脑区域也可直接进入。Portal 全量 Vitest 741 项、WorkBench 定向 14 项通过；三条布局/通知 Playwright E2E、默认电脑同步入口与云端电脑授权失败两条 E2E 通过；13 个 Portal 路由的桌面与窄屏布局检查通过。内置浏览器控制接口超时，当前运行的 Electron 仍展示旧版远端页面，因此实际 PC 客户端尚未确认；文件上传开关仍关闭，真实文件同步及安装包验收待补。

- [ ] **T02（待验证）跨端会话与长上下文**
  - 修改：按缺口调整 `server/internal/app/conversation_*`、`server/internal/store/postgres/conversation_*`、`runtime/morde-cloud-runtime/src/engine.mjs`、Portal outbox 和 iOS 会话投影。复用摘要、原文分页、授权历史和检查点；历史不可取得时说明，同步目录不自动授予项目记忆权限。
  - 验收：覆盖超模型窗口续聊、原文查看、两个账号和会话隔离、记忆关闭/撤权、关闭客户端后重连及无电脑的普通问答；实际 PC/iOS 与真实模型结果单独记录。
  - 当前证据与缺口：已有本地压缩、历史和 outbox 测试记录；iOS 会话分页已校验账号、服务端 scope 和会话代次，拒绝 conversation_id 不匹配的历史页，并对 cursor 重复、消息去重及迟到响应作防护。当前重跑 `swift test --filter MordeConversationMessageTests` 21 项通过；历史分页 UI 测试因缺少 `ProviderCustomization.xcconfig` 与 FFmpeg/Rclone 等 XCFrameworks 未能执行，先前模拟器双架构构建记录不能代替该 UI 验收。真实 PC/iOS 接续同一服务端会话及真实模型长会话待验收。

- [x] **T03（完成，依赖 T02 的会话基础）执行中消息路由和去重**
  - 修改：`server/internal/app/conversation_route_*`、`server/internal/store/postgres/conversation_route_*`、Portal outbox 和 iOS 消息投影；按目标、附件、项目、电脑和授权区分追问、补充、修改、停止、独立任务与澄清，保留消息身份及修订围栏。
  - 验收：同话题不同目标不合并，停止一个任务不影响另一个，同键重试不重复创建或执行，迟到的旧结果不覆盖新修订。
  - 已有证据：原清单记录了本地路由、PostgreSQL 并发和 Portal 定向测试通过；本轮在当前工作树无 overlay 重跑隔离 PostgreSQL、worker 与 app 的关联、停止、重试及迁移定向回归通过。真实模型与远端 Host 不属于该记录。

- [ ] **T04（待验证，依赖 T02）主 Agent 委派与结果恢复**
  - 修改：`runtime/morde-cloud-runtime/src/engine.mjs`、`server/internal/app/agent_sessions.go`、`server/internal/worker/cloud_dsh_session_tree.go` 和结果汇总。复用 DSH 子会话、授权子集、租约和检查点；独立任务按资源条件并行，有依赖的任务等前置结果，主 Agent 汇总实际成功、失败或未知状态。
  - 验收：本地 DSH/模型替身派发至少两个无冲突子任务，检查并行、依赖、失败回传、重启和取消隔离；真实模型协作另行验收。
  - 当前证据与缺口：本地模型替身定向测试覆盖双子任务并行、依赖任务在前一结果结算后启动、主 Agent 汇总、失败/未知结果、子任务取消及检查点恢复、独立任务取消隔离；另完整运行 `runtime/morde-cloud-runtime` 的 109 项测试，全部通过。已检查 DSH 上游插件：`dsh-subagent`/`dsh-tool-subagent` 使用进程内 managed spawn，不能替代 Morde 的服务端持久任务、授权子集与恢复边界；现有实现沿用 DSH AgentRegistry/AgentLoop，并由 Morde IPC 保持持久任务所有权，没有发现需要重复接入第二套 Agent Runtime 的缺口。iOS 子任务状态及父子归属投影通过本地替身断言。真实模型协作及完整 iOS UI 仍需验收。

- [ ] **T05（待验证，依赖 T01、T03、T04）任务进度、审批和控制**
  - 修改：`Assistant.tsx`、任务快照/事件投影及 iOS `MIFTaskViews.swift`；展示当前会话的任务、子任务、审批、澄清和控制状态，断线按游标续读，失效时重新取快照并核权。
  - 验收：内置浏览器和实际 PC 检查两个任务的归属、批准/拒绝、指定停止、断线续读、快照恢复和未知状态；iOS 核对相同合同与本地化。
  - 当前证据与缺口：iOS 已修复快照请求失败时游标提前推进、到期恢复、快照回退和撤权后残留任务投影；隔离源码副本的 11 项 SwiftPM XCTest 与实际方法提取后的本地替身检查通过。实际 PC 页面、当前完整 iOS 工程构建及设备验收待补。

- [ ] **T06（待验证，依赖 T05）指定电脑与常用工具**
  - 修改：按缺口调整 `server/internal/app/runtime_tool_executor.go`、`cloud_dsh_*`、`runtime/morde-runtime/runtimebrowser/`、`runtime_desktop/`、`runtimeactions/` 和 PC 电脑面板。复用 Host、Workspace、授权及控制版本校验。
  - 验收：本地替身覆盖指定目标、接管/交还、撤权、搜索来源、命令输出和不可用/未知状态；内置浏览器及实际 PC 检查面板。真实 Host 界面操作、终端和真实搜索另记证据。
  - 当前证据与缺口：本轮本地 `-race` 运行 13 项浏览器任务隔离/接管/恢复、桌面截图与 Host 围栏、命令参数/输出上限测试通过；已有服务端工具和面板替身测试记录。真实 Host、真实搜索及实际 PC 验收待补。

- [ ] **T07（待验证，依赖 T06）PC 手动文件夹同步**
  - 修改：`clients/desktop/harness/desktop/`、`server/internal/app/workspace_sync_project_upload.go`、`server/internal/store/postgres/workspace_sync*.go`、`runtime/morde-sync/` 和 `runtime/morde-runtime/workspacereplica/`。用户选目录、目标电脑并确认项目名/范围后手动单向同步到 `~/workspace/project/<项目名>/`；再次点击才应用本机增改删，删除仅作用于该来源已同步的对应内容，保留相对路径和未协调的云端内容；界面区分进行中、完成、待处理和失败。
  - 验收：本地合成元数据和替身检查目录越界、冲突、根目录不可访问、撤权、删除范围、待应用与匹配 session 的 Host 回执；实际 PC 检查交互。真实文件传输和应用仍待验证。
  - 当前证据与缺口：修复 Host 回执忽略项目根目录、父目录或大小写不同的项目路径冲突的问题；本地 `-race` 通过 4 项 Runtime 回执测试、4 项客户端单向/绑定测试、6 项服务端路径/删除范围/回执合同测试及 5 项隔离 PostgreSQL 集成测试，均只使用合成元数据。文件同步入口位于 PC 用户端统一外壳内的“云端电脑 > 文件同步”子导航，并从电脑管理页可达；原完整 Workspace 文件、版本和恢复视图作为同一 Portal 子视图，不再使用独立暗色窗口或桌面客户端独立页面。项目上传开关仍关闭，实际 PC 安装包、真实文件传输及 Host 应用回执仍待验收。

- [ ] **T15（待验证，依赖 T03）连发消息的安全短时聚合**
  - 修改：`server/internal/app/conversation_route_*`、`server/internal/store/postgres/conversation_route_*`、相关迁移及必要的 PC/iOS 消息投影。每条消息先持久受理；只关联同账号、会话、目标、范围、附件和授权一致的同一工作，聚合有上限；在同一事务中冻结候选并按消息身份幂等结算，迟到消息走已有路由，窗口和结算可恢复。明确允许提前结算的条件，普通问答、停止和独立任务不因聚合无谓等待；任务开始后的补充走原修订链。
  - 验收：用代表性连发样本比较窗口时延和误合并情况，再在本地 PostgreSQL/模型替身覆盖窗口内外、不同目标/附件/账号、明确停止、重启、并发结算及重复请求；检查每条原文和任务关联。
  - 当前证据与缺口：已修复可停止标记、慢分类漏聚合和结算时的并发误失败；当前工作树无 overlay 通过本机隔离 PostgreSQL 5 项定向 `-race` 回归，覆盖慢分类、不同目标/授权、候选选择、并发结算和候选上限，应用层 350 ms 固定窗口测试通过。窗口分组测试已补齐恰好 350 ms 的包含边界，与生产候选规则一致。带附件消息当前走已有路由，不参与聚合。350 ms 是收件窗口，不是端到端延迟；受控模型替身预设分类结果，不能用于估计真实误合并率，代表性样本与真实模型/客户端验收待补。

- [ ] **T16（待验证，依赖 T05）任务时间线与工具详情**
  - 修改：`server/internal/api/task_events.go`、`server/internal/app/task_step_detail.go`、`server/internal/store/postgres/task_step_details.go` 和 PC/iOS 任务视图。按时间及步骤/子任务展示同一批有序记录；按工具类型展示已记录且获权的对象、输入、结果、差异或截断提示。
  - 验收：内置浏览器和实际 PC 检查成功、失败、等待、恢复及详情；验证未知安全事件不丢游标，跨账号、撤权、凭据和内部推理不泄露，iOS 核对任务归属。
  - 当前证据与缺口：已有事件字段类型和详情过滤定向测试；本轮补齐 iOS 独立任务时间线的快照缓存、撤权清理及未知状态展示，并以隔离 XCTest 与本地替身验证事件续读。本机隔离 PostgreSQL 通过加密工具详情 HTTP、事件过期及快照并发 3 项 `-race` 测试；实际 PC 页面及 iOS 设备验收待补。

- [ ] **T08（待验证，依赖 T01–T07、T15、T16）第一里程碑整体验收**
  - 修改：[现有 A-07 验收记录](Morde第一里程碑收口三会话并行实施/A-云端Agent与公共合同实施任务todo清单文档.md)或后续本地记录，写明候选版本、场景、实际结果和未完成项。
  - 验收：按根目录 AGENTS.md 核对 PC/iOS 同会话、长会话、指定电脑与工具、双子任务、停止隔离及手动同步全链路；真实文件传输等受限项没有证据前不能宣布里程碑完成。

## P1：对话与产物补强

- [ ] **T09（待验证，依赖 T01）会话标题搜索**
  - 修改：`web/portal/src/app/api.ts`、`Assistant.tsx` 和样式，复用已有 `q`；查询变化重置分页，旧响应不覆盖新结果，搜索和切换不清除草稿。
  - 验收：内置浏览器及实际 PC 检查分页、切换、空结果、失败和账号隔离。当前代码及定向测试已有记录，手动验收待补。
  - 当前证据与缺口：当前实现把标题搜索词放入独立查询键并向 API 传 `q`。Portal 定向测试覆盖旧响应、搜索时保留输入、切走再返回恢复草稿、分页游标、空结果和网络失败；本机隔离 PostgreSQL `-race` 测试覆盖大小写搜索分页与另一账号不可见。内置浏览器已确认搜索空态可见、对话区保持原布局且状态提示没有挤占页面内容；实际 PC 页面验收待补。

- [x] **T10（完成）记忆历史与来源合同**
  - 修改：`server/internal/domain/personal/`、`server/internal/app/personal_*`、`server/internal/store/postgres/personal_*`、迁移与 API。历史正文、版本、来源与配额原子处理；旧记忆首次修改保存可用旧正文，恢复用当前版本和幂等键，删除清理历史正文并保留墓碑/抑制。
  - 验收：覆盖来源失权、分页、恢复、并发冲突、配额、删除及跨账号隔离；旧版本没有正文时不可恢复。
  - 已有证据：原清单记录了 domain/app/API 及本机 PostgreSQL 相关测试通过；接口和迁移存在于当前工作树。

- [ ] **T11（待验证，依赖 T10）记忆历史界面**
  - 修改：`web/portal/src/routes/PersonalData.tsx`、`personal-data-store.ts`、`app/api.ts` 和类型；展示来源、时间、可恢复状态及冲突，删除前说明历史正文一并清理。iOS 沿用个人数据同步接收恢复后的新版本。
  - 验收：内置浏览器和实际 PC 检查查看、恢复、无来源、冲突和删除；iOS 可运行时确认同步不覆盖未提交草稿。当前为本地 MSW 增加了仅供合成数据使用的记忆历史分页、版本更新和删除路由及隔离测试；Portal 实际页面已走通创建、编辑、查看来源、恢复旧版生成新版本、确认删除，删除后列表为空。记忆界面、API、MSW 定向 136 项测试通过，Portal typecheck、lint 和 diff 检查通过。实际 PC/iOS 客户端仍未安装/验收，保留待验证。

- [ ] **T12（待验证，依赖 T06）Office 文件处理能力**
  - 修改：`runtime/morde-cloud-runtime/`、`server/internal/app/cloud_dsh_skills.go`、工具代理、授权执行环境及产物证明链。先为获授权 Host 的 Office 二进制输出建立受限的登记、Host 独立字节校验、固定 Workspace 版本发布和恢复路径；禁止直接信任模型提供的路径/哈希。再复用上游渐进加载技能内容并适配 Morde 工具名、依赖检查和失败说明，不将 Agent 进程的路径传给 Host。本地 `project/deepseek-harness` 中 `dsh-skill-office` 为 `0.2.2-rc.1`、MIT；Morde runtime 同样固定 DSH `0.2.2-rc.1`，但没有安装 skill registry、Office 工具或 LibreOffice Kit。升级上游版本须按 runtime 依赖、平台、许可证和旧检查点恢复约定单独评估。
  - 验收：在本地授权环境分别创建、检查、修改并打开 DOCX、XLSX、PPTX 合成样例；缺依赖、无权限和处理失败时显示真实原因。云端文件数据面待验证。
  - 当前证据与缺口：本机 Codex 附带的文档库和 LibreOffice 曾对三个合成文件完成创建、修改、重新打开和本地导出，并核对导出文本；这是本机环境检查，未接通 Morde。上游 DSH `dsh-skill-office` 为 `0.2.2-rc.1` 技能 provider，但其 skill registry、依赖加载与 LibreOffice CLI 路径运行在 Agent Node 进程；Morde `shell_exec` 在获授权 Host 执行，不能直接安装该 provider 让它使用错误机器或绕开持久化技能审计。本轮扩展 Morde 固定 `nanomuse_skill_read` 目录，以独立 `office` 路由技能和 DOCX、PPTX、XLSX、视觉检查、公式重算、PDF 导出子技能渐进加载；仅有已选择 Host 的任务可见，执行仅使用当前 Task 的授权 `shell_exec`，不安装依赖、不调用 Agent Node 路径、不用外发 `present`，输出仍要求经过已核验 Task Artifact。
  - 本轮服务端产物链新增成功 Host `shell_exec` durable 证据检查；候选文件仅来自 Host 固定 Task Workspace manifest 与固定 base revision 的差异，不信任模型提供的路径、大小或哈希；只接受新增或修改的普通无链接文件，拒绝敏感路径、大小写冲突、symlink/hardlink 和超限候选，再由现有 CAS 发布与 Host 块字节校验形成成功 receipt。成功 receipt 重放使用原 operation 与 `receipt.BaseRevision`，不重置已前进的 Task view。定向 `server/internal/app` 测试通过：1 MiB 以上合成二进制发布及 receipt 重放、敏感路径/链接/文件数/总字节拒绝、Office 技能目录及 Hostless 拒绝，并覆盖原 broker 文件校验流程；`git diff --check` 通过。
  - 尚缺验收：当前未连接真实授权 Host 或 Office CLI；DOCX/XLSX/PPTX 创建、修改、重开、依赖缺失说明与视觉导出仍需在真实 Host 完成。服务端产物链仅在本地合成 Host runtime 上验证，云端文件数据面未验证。Office 技能与工具授权通过单测确认，实际 DSH 技能调用和宿主环境依赖待补。

- [ ] **T13（待验证，依赖 T12）Office 已核验产物查看与下载**
  - 修改：`Assistant.tsx`、iOS `MIFTaskViews.swift` / `MIFLibraryViews.swift` 和本地化；PC 复用已核验产物及可用预览/下载入口，iOS 显示同一已确认产物，不能预览时展示元数据和原因。
  - 验收：用合成元数据覆盖撤权、路径/版本/摘要不符和失败提示；内置浏览器、实际 PC 及可运行的 iOS 分别检查状态。实际跨端文件交付待验证。
  - 本轮本地实现与证据：PC 对话已有完整 verified proof 过滤和 `/api/tasks/{taskID}/artifacts/{artifactID}/content` 下载入口；iOS Library 仅在权限、verified 状态、完整 Workspace revision/size/SHA 与本机文件字节一致时提供 Quick Look。没有预览时保留名称、类型、大小和版本，并新增“证明不完整、Workspace 同步不可用、媒体能力未启用、匹配版本无法在本机核验”原因；Task 详情列表现在只显示完整已核验产物，并展示相对路径、类型、大小和 revision。新增 policy/projection 单元测试及中英本地化。
  - 验收结果：Portal Vitest 全量 744 项通过，下载 API 前端专项 1 项通过，Portal Node 文件复制测试 2 项通过；服务端 task artifact content API 定向测试通过；iOS 修改源与新增测试通过 `swiftc -frontend -parse`，本地化 JSON 解析通过。当前 Xcode UI 测试依赖 `ProviderCustomization.xcconfig` 及 FFmpeg/Rclone XCFramework，本轮未能构建或运行这些 XCTest/UI 测试；实际 PC/iOS 与真实跨端文件交付仍需验收。

- [ ] **T17（待验证，依赖 T04、T16）安全的步骤级重试**
  - 修改：`server/internal/domain/task/`、`app/task_*`、`store/postgres/task_*`、API、OpenAPI、必要迁移及 PC/iOS 任务视图。保存失败原因、尝试次数、可重试状态和原调用身份；仅在任务修订、授权、前置结果与副作用均可核实时，经 DSH 和现有工具代理恢复该步骤。
  - 验收：本地隔离环境检查单步骤新尝试、同键重试和并发点击只执行一次；不可重试、撤权、历史输入或副作用未知时拒绝并显示原因；内置浏览器、实际 PC 和可运行 iOS 检查入口/状态。
  - 2026-11-14 本地实现证据：重试接口统一为 `POST /api/tasks/{id}/steps/{step_id}/retries`，客户端只提交任务修订和预期尝试，调用摘要从服务端原步骤读取；重复尝试的历史源步骤保留证据但投影为不可再试。PostgreSQL 锁控并发用例确认同一幂等键只创建一个 attempt；API/app/PostgreSQL 定向测试及竞态 `-race`、Worker retry `-race`、Runtime 109 项测试通过。Portal 59 个文件/727 项测试、重试流程 Playwright 1/1、Portal 类型检查通过；iOS Swift package 54 项通过，重试错误按 task+step 隔离。
  - 尚缺验收：当前连接中的 Morde Electron 可只读打开远端 Portal，但显示的是旧版 Assistant 页面且没有可用模型，无法验收本地重试入口或提交真实任务；本地内置浏览器本轮在展开提示中心后 CUA 超时。iOS Xcode UI 构建仍缺 `ProviderCustomization.xcconfig` 与 FFmpeg/Rclone XCFrameworks。桌面 Node 全量测试未能完成：251 项通过，2 项受工作树中既有 Codex 包装文件/菜单改动影响而失败，3 组因未解决的事件循环被中断；未修改这些文件。全量 Go suite 未运行。故 T17 保持待验证。

- [ ] **T18（待验证，依赖 T01、T03）消息操作与回复版本**
  - 修改：`server/internal/app/conversation_*`、`server/internal/store/postgres/conversation_*`、API/OpenAPI、必要迁移、`Assistant.tsx`、`message-outbox.ts` 和 iOS 投影。实现编辑重发、重新生成和继续生成，保存来源与新版本；继续生成核实原调用和检查点，文字重生成不自动重复工具操作。
  - 验收：内置浏览器及实际 PC 检查原文、版本切换、断线重试和滚动位置；本地替身检查账号/会话隔离、幂等及未知副作用不重做，iOS 读取已确认版本。
  - 实施边界：不可变版本以原始 user message 为 group，当前最高版本由 server 在事务中锁定并校验；每个新版本记录 action、user/assistant message、Task 和幂等状态，原文不改不删。版本请求为 `{action, expected_version, content?}`，编辑重发需提交新正文，且复用来源消息的附件授权；重生成不重新调用工具；续写仅在同一 Task revision 与 DSH checkpoint 可验证时受理。消息列表按原 sequence 返回全部版本行和 `variant_group_id`/`variant_version`/`variant_action`，PC 与 iOS 共享同一投影并本地切换版本。
  - 当前证据与缺口：服务端已实现个人/项目会话变体 API、账号与服务实例幂等、不可变历史行、附件沿用来源授权，以及流式/非流式继续生成投影；操作结果未知时持久返回 `202`，不覆盖旧回复。Portal 已支持历史版本切换、编辑并重发、仅对完成回复重新生成文字、仅对中断回复继续生成；版本操作先持久化到 IndexedDB，刷新不自动重放，重试复用同一幂等键。iOS 已同步版本字段、切换和操作 outbox。当前复跑 Portal 类型检查通过，Assistant/Projects/outbox 定向 Vitest 209 项通过，Playwright 编辑、版本切换与草稿保留 1/1 通过；此前 Portal 321 项、iOS SwiftPM 61 项和本机隔离 PostgreSQL 3 项测试也有通过记录。当前全量 `go test ./internal/app` 通过；本轮环境没有 `CH_TEST_DATABASE_URL`，未重跑 PostgreSQL 集成测试。实际 PC/iOS 客户端、真实模型重生成及断线恢复验收仍待完成。

- [ ] **T19（进行中，依赖 T04、T16）复杂任务计划与修订**
  - 修改：`runtime/morde-cloud-runtime/`、`server/internal/app/cloud_dsh_*`、Task 存储/API/OpenAPI 及 PC/iOS 任务视图。由 DSH 主 Agent 按需生成目标、步骤、产出、依赖和完成条件，有依据时展示费用量级及所需审批，否则标为未知；持久化版本及实际进度，步骤计划与工具步骤、任务间 `task_dependencies` 分别表达；沿用步数、时间和模型预算，超限时显示原因。
  - 验收：本地替身检查计划、无循环依赖和实际结果；修订保留已完成产物和旧记录，旧结果不覆盖新计划，简单任务不被强制确认；内置浏览器、实际 PC 和可运行 iOS 检查展示。

- [ ] **T20（待验证，依赖 T05）审批上下文与到期状态**
  - 修改：Task 审批 domain、服务、存储/API、OpenAPI、`Assistant.tsx`、iOS 任务/审批视图和本地化。展示有依据的对象、影响、风险、本次预计与累计费用、替代方案、撤销方式及期限；未知或不适用如实标示，计划确认不扩大具体操作授权或硬限额。
  - 验收：本地 PostgreSQL 和工具替身覆盖批准/拒绝、到期与并发响应、重复提交、版本过期、权限失效和未知费用；内置浏览器、实际 PC 及可运行 iOS 检查卡片和恢复。
  - 当前证据与缺口：服务端审批响应增加风险、本次/累计费用、替代方案与撤销方式的结构化披露；缺少可信来源时统一返回 `unknown`，不会编造信息。审批决定写入本身再校验数据库期限，覆盖“响应等待事务锁时到期”的竞态，并在同一事务中过期、拒绝和重排任务。API/OpenAPI、披露状态验证、YAML 解析及本地 PostgreSQL `-race` 8 项审批集成回归通过，含离线、执行槽占满、多扫描器、并发响应和到期等待场景；未增加数据库迁移。Portal 已生成 API 类型并在审批卡显示五项字段、未知/不适用状态及安全校验后的费用金额；Assistant 定向 161 项测试、TypeScript 和 ESLint 通过。iOS 现已同步同一组披露字段、未知态与到期/状态未知时禁用决定的规则；审批视图测试覆盖已知、不适用、缺字段、损坏/过期时间，SwiftPM 审批/项目会话测试 52 项通过，独立 Simulator arm64 构建退出码 0。2026-11-14 将现有 Assistant Portal E2E 审批场景扩展为真实卡片交互，使用本地 MSW 检查已知风险、估算费用、未知累计费用、已知替代方案、不适用撤销及期限，Playwright 定向 1/1 通过。内置浏览器此前检查过 PC 对话和任务侧栏，但当前演示数据没有审批卡；本轮本地内置浏览器打开 `/assistant` 后 CUA 超时，不能复验卡片交互。连接中的远端 PC 是旧版且没有可用模型，iOS Xcode UI 构建仍缺 `ProviderCustomization.xcconfig` 与 FFmpeg/Rclone XCFrameworks。实现已到待验证状态，完整 PC/iOS UI 与真实设备验收尚未完成。

- [ ] **T21（未开始，依赖 T05）任务用量、费用与预算**
  - 修改：`server/internal/app/model_gateway*`、`project_budget*.go`、`server/internal/store/postgres/project_budget.go`、Task 投影/OpenAPI 和 PC/iOS 视图。复用现有 Token 账本及项目金额、币种和费率快照，展示调用次数、用量、时长及可获得的预算/费用，区分已结算、估算、未知和自带模型；任务结束后仍可查看汇总。
  - 验收：隔离账本覆盖个人/项目任务、无价格、未知使用、调价后的历史快照、跨账号访问和多实例汇总；内置浏览器、实际 PC 和可运行 iOS 核对相同状态，不把估算当实际扣费。

- [ ] **T22（未开始，依赖 T04）任务排队、公平与能力匹配**
  - 修改：`server/internal/store/postgres/task_execution.go`、Task 服务/API、必要迁移、DSH 上下文及 PC/iOS 任务视图。在现有领取、租约和资源围栏上支持用户置顶、普通和后台优先级，以及防饥饿；子 Agent 只匹配实际可用且获授权的工具/资源，队列位置仅在可计算时显示。
  - 验收：本地多 worker 检查同级顺序、置顶、低优先级最终获领、用户并发限制、取消隔离与撤权；内置浏览器、实际 PC 和可运行 iOS 检查排队状态。

- [ ] **T23（未开始，依赖 T15、T17、T20–T22）必要指标与降级状态**
  - 修改：`server/internal/worker/metrics.go`、`server/cmd/portal/main.go` 和相关路由/Task/审批/账本出口。记录聚合等待与纠正、排队、事件续读、审批等待、重试结果及模型用量；指标不包含消息正文、凭据或原始工具输出。
  - 验收：本地合成事件检查指标与真实状态一致；多实例重启、限流或降级非必要展示时，已受理任务、结果、账本和审批不丢失或被跳过，查询和订阅仍核对授权。

- [ ] **T14（待验证，依赖 T09–T13、T17–T23）P1 整体验收**
  - 修改：本地验收记录，汇总搜索、记忆、Office、计划、重试、审批、费用与队列的候选版本、证据和缺口。
  - 验收：各项实际行为均有对应证据后才勾选；真实产物交付和跨端文件验收受当前文件限制保持待验证。规模验收由 T24 单列。

## 规模验收

- [ ] **T24（待验证，依赖 T23）万人在线对话容量**
  - 修改：本地负载脚本和验收记录；开测前记录环境、成功标准、在线连接数、消息速率、并行任务数、模型调用量及长任务比例，混合短聊、连发、审批和断线重连。
  - 验收：记录实际延迟、错误率与资源用量；环境不足或未达到目标时保留待验证，本地替身结果不代表生产容量。不得向远端发送脚本或文件。

## 机制与参考项目对照

参考方向和完整链接见[融合方案第 7 节](../Morde对话体验与DS-Bot融合设计方案.md#7-对标与参考项目)；参考项目只提供交互或组件思路，执行仍使用 DSH 和 Morde。

| 机制 | 任务 | 参考项目 |
| --- | --- | --- |
| 1. 事件流与任务过程 | T05、T16 | OpenHands、Dify、LobeHub |
| 2. 会话持久化与断点续跑 | T02、T08 | LibreChat、hermes-agent |
| 3. 幂等与安全重试 | T03、T17、T18 | AnythingLLM、bolt.diy、Chatbox |
| 4. 审批关口 | T05、T20 | assistant-ui、Chatbox |
| 5. 成本与预算 | T21 | hermes-agent |
| 6. 记忆连续 | T02、T10、T11 | Muse、DS-Bot |
| 7. 布局与流式展示稳定 | T01、T18 | DS-Bot、LibreChat、LobeHub、Open WebUI、assistant-ui、bolt.diy |
| 8. 多任务并发、排队与能力匹配 | T04、T22 | Muse、DS-Bot、DSH |
| 9. 复杂任务规划 | T04、T19 | Muse、DSH |
| 10. 同一对话的任务更新 | T03、T15 | hermes-agent、Muse |

Office 处理参考 DSH Office 技能和 DS-Bot（T12–T13）；指定电脑及手动文件夹同步由 T06–T07 覆盖。
