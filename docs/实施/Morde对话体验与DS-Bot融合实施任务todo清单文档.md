# Morde 对话体验与 DS-Bot 融合实施任务 TODO

> 依据：[融合设计方案](../Morde对话体验与DS-Bot融合设计方案.md)、[PC 布局需求](<../Morde PC端布局与云端电脑体验需求文档.md>)和根目录 [AGENTS.md](../../AGENTS.md)。
>
> 任务编号和初始状态承接[原高优先级实施清单](Morde参考DS-Bot高优先级功能实施任务todo清单文档.md)的 2026-11-14 工作树快照；原清单保留详细测试记录，后续状态与新证据见各任务条目。工作树已有未提交的改动，实施前先核对现状，避免重复开发或覆盖。

## 修改边界与执行约定

- 面向 PC 的长期主会话，复用 Portal、Desktop 外壳和 iOS 云端数据；每个会话由主 Agent 负责，任务、消息与结果归属清楚。主会话呈现必要回复、进展和结果，工具、计划和子 Agent 过程进入任务动态；不引入 Bot 群聊、团队、IM 或本地 Agent CLI。
- 云端统一使用 DSH 和 Morde 模型网关，沿用消息路由、Task、工具代理、授权、审批、审计、租约与检查点。调整 Agent 前检查 `runtime/morde-cloud-runtime/package.json`、锁文件与 `project/deepseek-harness/` 对应组件；优先复用兼容插件，新增或升级时核对版本和许可证，并验证会话、取消与恢复。
- 五条原则分别落实为：主会话简洁（T01、T05）；主 Agent 持续交互和按需协作（T03、T04）；过程按任务归属（T03、T15、T16）；真实过程可追溯（T05、T16）；工具详情按权限下钻（T16、T17）。无记录或无法确认的信息显示未知，不展示凭据或模型内部推理。
- 依照用户约束只在本地修改，不向外部服务、设备或远端部署/上传文件；用户明确要求时可更新本机 Lima 服务和 PC 构建。可用本地替身、合成文本和元数据验证；真实文件同步、产物跨端传输或需要安装应用包的验收保持待验证，`project_upload_enabled` 保持关闭。
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
  - 当前证据与缺口：已全局检查 Morde PC Portal、Workbench、移动端 Web、iOS 主聊天页与 DSH Muse 的服务状态/错误展示。Portal 的 `FloatingError`、`FloatingNotice` 均在所属页面内容流内；对话服务状态默认收进会话标题的紧凑按钮，展开后位于标题与消息区域之间；通知中心位于 PC 顶栏操作区，列表按文档流展开，不使用覆盖页面的固定提示层。Workbench、移动端 Web 和 DSH Muse 的错误/短时提示均在所属内容流内；移动 Web 的全局状态 toast 与 Morde 错误提示已取消固定定位。Morde iOS 根级登录到期提示通过 `safeAreaInset` 占用安全区，并移除对话页重复的顶部 overlay；iOS `AIChatView` 原将错误和临时状态叠在消息列表上方，本轮已移入消息列表前的纵向布局，并在提示显示时关闭重复的列表顶部遮罩/内边距。用户主动决策弹窗、菜单、黑屏恢复等必要交互层仍保留；iOS 一次性复制/保存等操作反馈仍是短时系统 toast，不属于持续错误或服务状态条。此前 E2E 覆盖 1440、1024、800、480px 及长文案；本轮 Portal 5 个定向 Vitest 文件 235 项、Portal typecheck/lint 通过，Workbench 14 项测试及 lint/typecheck 有前序通过记录，移动端 Morde App 4 项测试与 typecheck 通过，DSH Muse 样式 13 项与 typecheck 通过。原完整 Workspace 文件、版本与恢复视图已集成到 PC 用户端“云端电脑 > 文件同步”，旧 `/workspaces`、`/local-projects` 地址重定向到此入口；Electron 桥接已接受 `/hosts/sync`，桥接定向测试 6 项通过。Playwright 路由 E2E 1/1 覆盖桌面与窄屏。本轮 CUA 实际操作 `morde-pc`：服务状态默认收起，展开详情位于会话标题与消息区域之间，不覆盖聊天区；“云端电脑 > 文件同步”从 PC 子导航进入，沿用同一外壳和卡片样式。当前只有空会话，无法验收多会话任务归属、窄窗口及任务动态；服务端还提示本机文件同步未开放，因此未执行同步。未向远端部署或发送文件；文件上传开关仍关闭，真实文件同步待补。2026-11-15 本轮完成 Minis arm64 Simulator app target 构建，并在 iPhone 17 Simulator 对 `MIFTaskViewsTests` 与 `NanoMuseMordePersonalDataTests` 的 45 项定向用例全部通过；iOS 实际交互、多会话/窄窗口与真实云端同步验收仍待补。

  - 2026-11-15 再次复核提示与同步入口：Portal `FloatingError`、`PortalLayout`、Assistant、Hosts 定向测试此前通过；移动 Web 全量测试此前通过；本轮将 Assistant 多条服务提示收为标题栏状态入口，并将全局通知中心放在 PC 顶栏操作区，避免自动提示覆盖聊天或挤占对话组件。实际 morde-pc 客户端已加载本轮 Portal 资源并验证展开状态详情仍处于内容流；没有真实任务数据和可用云端同步服务，相关端到端验收仍待补，见 T08。
  - 2026-11-15 本轮全量 Portal 回归：`npm test` 的 61 个 Vitest 文件 745 项及 Node 文件复制测试 2 项通过；Portal typecheck、lint 通过，`Assistant.test.tsx` 171 项通过。保留路由晚到的 Idea 提示预填逻辑，并对对应的一处同步状态更新加行级 lint 例外，确保不覆盖已有用户草稿。
  - 2026-11-15 全局提示布局补充：移动端 nanoMuse toast、Morde 页面错误、DSH Muse Ideas/Coding 短提示改为所属页面/面板内容流；Morde iOS 登录到期提示改用根视图 `safeAreaInset` 并删除对话页重复 overlay。移动端 Web 全量 158 项测试、Morde App 定向 4 项测试及类型检查通过；DSH Muse 全量 444 项测试、类型检查通过。Portal 与 Workbench 生产构建通过，Electron ARM64 目录包构建通过；本机实际 `morde-pc` 中检查服务状态展开不遮挡消息，并从“云端电脑 > 文件同步”子导航进入同一 PC 外壳。Morde iOS iPhone 17 Simulator target 构建成功；未运行 iOS UI 交互测试。服务端未开放同步，本轮未触发文件同步或传输；真实文件同步、多个会话与窄窗口验收仍待补。

  - 本轮更新：发送栏只保留模型选择，移除逐消息电脑/Workspace 选择；目标取账号默认电脑，无默认且仅有一台自有电脑时使用该台。手动新增个人模型后自动检查文本与工具能力；文本能力通过后即可在对话选择，工具检查未通过时保留文字能力并明确提示未验证。对话页进入时刷新个人模型列表，未验证文字模型仍不可选。DeepSeek Responses 探针按官方协议在命名工具探测时关闭 thinking。
  - 2026-11-15 本机验收：构建并启动 macOS ARM64 Electron 客户端 `clients/desktop/harness/desktop/dist/mac-arm64/morde-pc.app`，登录本机 Lima 服务 `http://127.0.0.1:7080` 的 admin 账号；更新本机 Portal Linux ARM64 二进制并重启成功（`/readyz` HTTP 200）。PC 实测 `DeepSeek Flash · deepseek-flash` 的文本和工具调用均可用，视觉未验证，且对话页发送栏没有云端电脑或 Workspace 选择项。已发送一条无敏感内容的 PC 聊天验证消息，但服务状态显示服务端未确认支持自动对话处理、云端工具当前不可用；消息被保留在本机，未收到模型回复，故不能据此验收真实对话或自动电脑工具调用。T01 的多会话、窄窗口、任务动态和 iOS 完整 UI 验收仍待补。
  - 本轮需求点状态：[x] 个人模型文本检查通过后可在 PC AI 对话选择；[x] 对话发送栏移除云端电脑/Workspace 选择；[x] DeepSeek 工具能力探测在本机通过；[ ] 通过 PC 实际触发云端电脑工具并收到真实结果（`cloud_dsh.enabled=false`，见 T06）。
  - 2026-11-15 本轮实际 `morde-pc` 复验：主会话与新建旁聊间切换后，各自未发送草稿均正确保存并恢复；清空测试文本后未发送消息或创建服务端 Task，留下一个空的本地旁聊草稿条目。标题搜索无匹配时显示正确空态，清空搜索后恢复原列表。页面提示未遮挡聊天区；多会话任务归属、窄窗口、任务动态、iOS UI 与真实服务端行为仍未验收，因此保持待验证。
  - 2026-11-15 iOS Simulator UI 验收：修正 Work Item 登录夹具身份不一致（登录接口返回 `u_localtest1`、`/api/me` 返回 `u0000001`），并让 PC/iOS 壳层切换主标签时释放输入焦点，使键盘收起且草稿保留。`testCloudConversationDraftsStayWithTheirConversation` 与 `testTaskNavigationPreservesChatDraftAndSelectedTabAccessibility` 同轮 **2/2 通过**。本轮再次检查本地服务 `/readyz` 仍连接失败，真实 `morde-pc` 服务交互未验收；T01 继续待验证。
  - 2026-11-15 补测：全新 iPhone 18 Pro Simulator 上 `testTaskNavigationPreservesChatDraftAndSelectedTabAccessibility` **1/1 通过**；保留过期登录态的独立 Simulator 上同用例 **1/1 通过**，`testCloudConversationDraftsStayWithTheirConversation` **1/1 通过**。覆盖登录恢复、聊天草稿保留、会话切换、任务标签切换和无障碍选中状态。实际 PC 与 iOS 真机、多会话任务动态和窄窗口验收仍待补。

- [ ] **T02（待验证）跨端会话与长上下文**
  - 修改：按缺口调整 `server/internal/app/conversation_*`、`server/internal/store/postgres/conversation_*`、`runtime/morde-cloud-runtime/src/engine.mjs`、Portal outbox 和 iOS 会话投影。复用摘要、原文分页、授权历史和检查点；历史不可取得时说明，同步目录不自动授予项目记忆权限。
  - 验收：覆盖超模型窗口续聊、原文查看、两个账号和会话隔离、记忆关闭/撤权、关闭客户端后重连及无电脑的普通问答；实际 PC/iOS 与真实模型结果单独记录。
  - 当前证据与缺口：已有本地压缩、历史和 outbox 测试记录；iOS 会话分页校验账号、服务端 scope 和会话代次，拒绝 conversation_id 不匹配的历史页，并对 cursor 循环、消息去重及迟到响应作防护。本轮补齐 A→B→A cursor 循环检测，完整 SwiftPM 75 项及 `MordeConversationMessageTests` 22 项通过，Morde standalone arm64 Simulator 增量构建通过。`runtime/morde-cloud-runtime` 全量 109 项通过，覆盖 DSH 压缩、压缩检查点跨重启恢复及只收到来源检查点时恢复等场景。本机依仓库脚本构建了忽略目录中的 FFmpeg/Rclone 模拟器依赖，并用空配置模板补齐本地 Xcode 构建；Minis arm64 Simulator app target 构建通过，`NanoMuseMordePersonalDataTests` 19/19 通过。真实 PC/iOS 接续同一服务端会话及真实模型长会话待验收。

- [x] **T03（完成，依赖 T02 的会话基础）执行中消息路由和去重**
  - 修改：`server/internal/app/conversation_route_*`、`server/internal/store/postgres/conversation_route_*`、Portal outbox 和 iOS 消息投影；按目标、附件、项目、电脑和授权区分追问、补充、修改、停止、独立任务与澄清，保留消息身份及修订围栏。
  - 验收：同话题不同目标不合并，停止一个任务不影响另一个，同键重试不重复创建或执行，迟到的旧结果不覆盖新修订。
  - 已有证据：原清单记录了本地路由、PostgreSQL 并发和 Portal 定向测试通过；本轮在当前工作树无 overlay 重跑隔离 PostgreSQL、worker 与 app 的关联、停止、重试及迁移定向回归通过。修复路由 worker 创建 Task 时未先应用默认值/规范化、导致合法任务被校验拒绝的问题。一次性本地 PostgreSQL 上另通过 9 项路由、聚合、迁移、变体附件和队列优先级定向集成测试；未运行整个 849 项 PostgreSQL 包测试。真实模型与远端 Host 不属于该记录。

- [x] **T04（完成，依赖 T02）主 Agent 委派与结果恢复**
  - 修改：`runtime/morde-cloud-runtime/src/engine.mjs`、`server/internal/app/agent_sessions.go`、`server/internal/worker/cloud_dsh_session_tree.go` 和结果汇总。复用 DSH 子会话、授权子集、租约和检查点；独立任务按资源条件并行，有依赖的任务等前置结果，主 Agent 汇总实际成功、失败或未知状态。
  - 验收：本地 DSH/模型替身派发至少两个无冲突子任务，检查并行、依赖、失败回传、重启和取消隔离；真实模型协作另行验收。
  - 本地验收完成：`npm test` 在 `runtime/morde-cloud-runtime` 中完整运行 109 项测试，覆盖双子任务并行、依赖结算后启动、父 Agent 汇总、失败/未知结果、子任务取消、检查点恢复及取消隔离，全部通过。使用本机临时 PostgreSQL（`CH_TEST_DATABASE_URL` 明确配置）运行 Worker `-race` 集成回归，父任务取消不影响兄弟任务、取消后节点重启失败关闭、租约回收后继续后台结果三项通过；对应执行器重启/取消和恢复单测亦通过。已检查 DSH 上游插件：`dsh-subagent`/`dsh-tool-subagent` 使用进程内 managed spawn，不能替代 Morde 的服务端持久任务、授权子集与恢复边界；现有实现沿用 DSH AgentRegistry/AgentLoop，并由 Morde IPC 保持持久任务所有权，无需重复接入第二套 Agent Runtime。真实模型协作另列于 T08 验收；PC/iOS 实际任务动态 UI 仍按 T01/T05 验收。

- [ ] **T05（待验证，依赖 T01、T03、T04）任务进度、审批和控制**
  - 修改：`Assistant.tsx`、任务快照/事件投影及 iOS `MIFTaskViews.swift`；展示当前会话的任务、子任务、审批、澄清和控制状态，断线按游标续读，失效时重新取快照并核权。
  - 验收：内置浏览器和实际 PC 检查两个任务的归属、批准/拒绝、指定停止、断线续读、快照恢复和未知状态；iOS 核对相同合同与本地化。
  - 当前证据与缺口：iOS 已修复快照请求失败时游标提前推进、到期恢复、快照回退和撤权后残留任务投影；本轮 `swift test --filter MordeTaskEventIsolationTests` 13 项通过，完整 SwiftPM `swift test` 74 项通过，另用隔离源码副本做了实际方法替身检查。独立 `Morde/Package.swift` 目前不编译 `MordeCodexSessionsView.swift`；实际 Minis app target 此前漏接 `MordeCodexSessionPolicy.swift` 与 `MordeCodexSessionsView.swift`，已补入并通过 arm64 Simulator app target 构建。修正过时的任务测试 fixture 后，iPhone 17 Simulator 的 `MIFTaskViewsTests` 与 `NanoMuseMordePersonalDataTests` 定向用例共 45/45 通过。本轮重跑本地 Playwright 连续消息任务链 1/1，验证两条任务各自归属、审批卡展示、停止一个任务后另一任务仍可操作。2026-11-15 在 iPhone 18 Pro Simulator 用本地 fixture 跑 `testTaskDetailsHandleApprovalAndClarificationRequests` 1/1 通过，覆盖审批响应和澄清回传；修复键盘弹出时任务底部操作栏与澄清发送按钮重叠的问题：输入聚焦时取消底部导航的额外 104pt 预留，键盘收起后仍保留原间距。测试虽通过，Xcode result bundle 仍记录键盘交互期间的 `Invalid frame dimension (negative or non-finite)` runtime warning，尚未定位其来源。CUA 本轮实际打开 `morde-pc` 的本机 Portal，但当前只有空会话，未提供两条可操作任务；iOS 实际设备 UI 操作仍待验收。

- [ ] **T06（待验证，依赖 T05）指定电脑与常用工具**
  - 修改：按缺口调整 `server/internal/app/runtime_tool_executor.go`、`cloud_dsh_*`、`runtime/morde-runtime/runtimebrowser/`、`runtime_desktop/`、`runtimeactions/` 和 PC 电脑面板。复用 Host、Workspace、授权及控制版本校验。
  - 验收：本地替身覆盖指定目标、接管/交还、撤权、搜索来源、命令输出和不可用/未知状态；内置浏览器及实际 PC 检查面板。真实 Host 界面操作、终端和真实搜索另记证据。
  - 当前证据与缺口：本轮本地 `-race` 运行 13 项浏览器任务隔离/接管/恢复、桌面截图与 Host 围栏、命令参数/输出上限测试通过；已有服务端工具和面板替身测试记录。本轮实际 `morde-pc` 可打开本机 Portal，但页面报告云端工具不可用，唯一自有 Host 为已停止状态；没有操作真实 Host、搜索或终端。

  - 本轮更新：PC 对话将默认电脑作为延迟工具目标；DSH 工具说明要求普通文字任务不调用电脑，实际工具调用时再校验目标和权限。电脑 Workspace 由现有 Host 工具链按需解析；未配置默认电脑时提示配置，不擅自换机。实际 PC 确认没有电脑/Workspace 手动选择器；本轮已在本机成功探测 DeepSeek 工具调用能力，并从 PC 发送短测试消息，但服务状态显示服务端尚未确认对话自动处理且云端工具不可用，消息未获服务端确认并保留本机。因本机 `cloud_dsh.enabled=false` 且无可调用的已授权运行中 Host，真实电脑操作、搜索、终端和 Agent 按需调度仍不能端到端验证，T06 保持待验证。
  - 本轮需求点状态：[x] PC 对话不要求用户选择云端电脑或 Workspace；[ ] 真实云端电脑工具调用与回程结果验收。
  - 2026-11-15 只读环境核对：用户指定测试服务匿名 `/api/capabilities` 返回 `api_version=1`、`hub_version=null`、`event_version=null`，声明 `cloud_tasks` 与模型网关能力；本机 Lima 同一路由的 `event_version` 也为 null。随后只读查看远端管理员 Host 列表，看到一台运行中的 `std-2c4g` 主机绑定了 Workspace，归属非管理员账号；已退出管理员会话。没有操作该 Host、写入远端数据或传输文件。匿名能力响应和管理员列表不足以确认被授权用户身份、当前客户端合同兼容或任务事件读取能力，真实 Host 验收仍待核实。

- [ ] **T07（待验证，依赖 T06）PC 手动文件夹同步**
  - 修改：`clients/desktop/harness/desktop/`、`server/internal/app/workspace_sync_project_upload.go`、`server/internal/store/postgres/workspace_sync*.go`、`runtime/morde-sync/` 和 `runtime/morde-runtime/workspacereplica/`。用户选目录、目标电脑并确认项目名/范围后手动单向同步到 `~/workspace/project/<项目名>/`；再次点击才应用本机增改删，删除仅作用于该来源已同步的对应内容，保留相对路径和未协调的云端内容；界面区分进行中、完成、待处理和失败。
  - 验收：本地合成元数据和替身检查目录越界、冲突、根目录不可访问、撤权、删除范围、待应用与匹配 session 的 Host 回执；实际 PC 检查交互。真实远端文件传输和 Host 实际应用仍待验证。
  - 当前证据与缺口：修复 Host 回执忽略项目根目录、父目录或大小写不同的项目路径冲突的问题；本地 `-race` 通过 4 项 Runtime 回执测试、4 项客户端单向/绑定测试、6 项服务端路径/删除范围/回执合同测试及 5 项隔离 PostgreSQL 集成测试，均只使用合成元数据。文件同步入口位于 PC 用户端统一外壳内的“云端电脑 > 文件同步”子导航，并从电脑管理页可达；原完整 Workspace 文件、版本和恢复视图作为同一 Portal 子视图，不再使用独立暗色窗口或桌面客户端独立页面。2026-11-15 重跑 Portal 路由 E2E，确认 `/hosts?sync=1` 和旧地址重定向均使用统一 PC 外壳，桌面及 480px 窄屏无页面溢出。CUA 本地浏览器及实际 `morde-pc` 均确认该入口位于 PC 外壳子导航，当前客户端页面显示“本机文件同步未开放”，未进行任何文件传输。项目上传开关仍关闭，真实 Host 应用回执及文件传输待验收。
  - 2026-11-15 复验：`runtime/morde-sync/client` 的上传绑定、分块传输与冲突保护定向 `-race` 测试通过；服务端 `internal/app` 5 项项目上传/目的地冲突测试、`internal/store/postgres` 4 项项目上传与回执集成测试，以及 `morde-runtime/workspacereplica` 6 项回执校验、冲突、重放与重启恢复测试均通过。测试使用本机临时目录、测试 HTTP 服务、隔离 PostgreSQL 和合成数据；没有连接远端或传输用户文件。
  - 2026-11-15 补充本地字节链路验证：扩展 `TestWorkspaceSyncClientThroughHTTPAndPostgres`，用合成文件经 `morde-sync client → 本地 Portal HTTP API → 隔离 PostgreSQL/本机 block store` 完成项目上传；检查嵌套路径、大小、SHA-256 与源文件字节不变，再模拟 Host 确认和独立云端新增文件，进行第二次手动上传后确认云端文件保留且没有下载到本机。`go test -race ./internal/store/postgres -run '^TestWorkspaceSyncClientThroughHTTPAndPostgres$' -count=1` 通过。它验证本地真实文件字节的完整链路，不替代真实 Host 应用或安装包验收。

- [ ] **T15（待验证，依赖 T03）连发消息的安全短时聚合**
  - 修改：`server/internal/app/conversation_route_*`、`server/internal/store/postgres/conversation_route_*`、相关迁移及必要的 PC/iOS 消息投影。每条消息先持久受理；只关联同账号、会话、目标、范围、附件和授权一致的同一工作，聚合有上限；在同一事务中冻结候选并按消息身份幂等结算，迟到消息走已有路由，窗口和结算可恢复。明确允许提前结算的条件，普通问答、停止和独立任务不因聚合无谓等待；任务开始后的补充走原修订链。
  - 验收：用代表性连发样本比较窗口时延和误合并情况，再在本地 PostgreSQL/模型替身覆盖窗口内外、不同目标/附件/账号、明确停止、重启、并发结算及重复请求；检查每条原文和任务关联。
  - 当前证据与缺口：已修复可停止标记、慢分类漏聚合和结算时的并发误失败；当前工作树无 overlay 通过本机隔离 PostgreSQL 5 项定向 `-race` 回归，覆盖慢分类、不同目标/授权、候选选择、并发结算和候选上限，应用层 350 ms 固定窗口测试通过。窗口分组测试已补齐恰好 350 ms 的包含边界，与生产候选规则一致。带附件消息当前走已有路由，不参与聚合。350 ms 是收件窗口，不是端到端延迟；受控模型替身预设分类结果，不能用于估计真实误合并率，代表性样本与真实模型/客户端验收待补。

- [ ] **T16（待验证，依赖 T05）任务时间线与工具详情**
  - 修改：`server/internal/api/task_events.go`、`server/internal/app/task_step_detail.go`、`server/internal/store/postgres/task_step_details.go` 和 PC/iOS 任务视图。按时间及步骤/子任务展示同一批有序记录；按工具类型展示已记录且获权的对象、输入、结果、差异或截断提示。
  - 验收：内置浏览器和实际 PC 检查成功、失败、等待、恢复及详情；验证未知安全事件不丢游标，跨账号、撤权、凭据和内部推理不泄露，iOS 核对任务归属。
  - 当前证据与缺口：已有事件字段类型和详情过滤定向测试；本轮补齐 iOS 独立任务时间线的快照缓存、撤权清理及未知状态展示，并以隔离 XCTest 与本地替身验证事件续读。本机隔离 PostgreSQL 通过加密工具详情 HTTP、事件过期及快照并发 3 项 `-race` 测试；本轮 `MordeTaskEventIsolationTests` 13 项及完整 SwiftPM 74 项通过，涉及任务时间线和工具详情的合同用例通过。新增本地 `task-step-detail.e2e.ts`，验证详情仅在展开后读取、返回文本安全渲染、截断提示及 Task 快照不泄露详情，1/1 通过；Portal typecheck 通过。2026-11-15 在 iPhone 17 Simulator 运行 `MIFTaskViewsTests`（含时间线排序、状态标记、快照与详情过滤用例）和记忆同步测试，共 45/45 通过；Minis arm64 Simulator app target 构建成功。实际 Electron 页面及 iOS App 内交互验收待补。

- [ ] **T08（待验证，依赖 T01–T07、T15、T16）第一里程碑整体验收**
  - 修改：[现有 A-07 验收记录](Morde第一里程碑收口三会话并行实施/A-云端Agent与公共合同实施任务todo清单文档.md)或后续本地记录，写明候选版本、场景、实际结果和未完成项。
  - 验收：按根目录 AGENTS.md 核对 PC/iOS 同会话、长会话、指定电脑与工具、双子任务、停止隔离及手动同步全链路；真实文件传输等受限项没有证据前不能宣布里程碑完成。
  - 当前本地验收记录：候选仍是未部署、未提交的本地工作树，不能作为统一版本指纹或发布结论。各项本地替身、Portal/桌面/iOS 定向测试记录见 T01–T07、T15、T16；T07 新增 client→本地 Portal HTTP→隔离 PostgreSQL/block store 的合成字节项目上传 E2E，含二次手动上传时保留独立云端文件且不下载回 PC 的检查。本轮 CUA 已实际打开安装的 `morde-pc` 并连接本机 Portal；验证对话提示处于内容流、文件同步位于“云端电脑”子导航且沿用 PC 外壳。当前会话为空，服务端提示对话自动处理未确认、云端工具不可用、本机文件同步未开放；未访问远端、真实模型/Host、iOS 真机或真实文件数据面，项目上传开关保持关闭。因此第一里程碑仍待验证，不以本机 UI 结果代替完整两端验收。
  - 本轮复核：用户表示将启动本机服务；检查时 `http://127.0.0.1:7080/readyz` 连接失败，当前 macOS 进程列表也没有运行中的 `morde-pc`。因此本轮未新增实际客户端验收，前述 CUA 结果仍是此前的历史证据。
  - 2026-11-15 补验：默认签名配置下，iOS 本地 UI 用例 `testCloudConversationDraftsStayWithTheirConversation` **1/1 通过**，`testTaskNavigationPreservesChatDraftAndSelectedTabAccessibility` 在全新与过期登录态模拟器分别 **1/1 通过**；本机 `/readyz` 再查仍无法连接。此结果只覆盖 iOS 本地界面，不替代服务端、PC、真实 DSH/Host 或跨端闭环验收，里程碑继续待验证。
  - 2026-11-15 新增补验：iPhone 18 Pro Simulator 的审批与澄清 UI 场景 `testTaskDetailsHandleApprovalAndClarificationRequests` **1/1 通过**。这是本地 fixture 验收；本机 `/readyz` 仍拒绝连接，不能替代 morde-pc 和服务端闭环，T08 继续待验证。

## P1：对话与产物补强

- [ ] **T09（待验证，依赖 T01）会话标题搜索**
  - 修改：`web/portal/src/app/api.ts`、`Assistant.tsx` 和样式，复用已有 `q`；查询变化重置分页，旧响应不覆盖新结果，搜索和切换不清除草稿。
  - 验收：内置浏览器及实际 PC 检查分页、切换、空结果、失败和账号隔离。当前代码及定向测试已有记录，手动验收待补。
  - 当前证据与缺口：当前实现把标题搜索词放入独立查询键并向 API 传 `q`。Portal 定向测试覆盖旧响应、搜索时保留输入、切走再返回恢复草稿、分页游标、空结果和网络失败；本机隔离 PostgreSQL `-race` 测试覆盖大小写搜索分页与另一账号不可见。本轮重跑 `npx vitest run src/routes/Assistant.test.tsx`，169 项通过；前次全 Portal suite 743 项通过。实际 `morde-pc` 中搜索“ 不存在的测试旁聊标题 ”后显示空结果，清空后正确恢复“还没有旁聊”，没有创建或改写会话。当前账号没有旁聊，故分页和切换仍不能在实际 PC 手工验收；内置浏览器与本地测试已覆盖其交互，实际账号隔离仍依赖数据库测试。状态提示不占用对话布局。

- [x] **T10（完成）记忆历史与来源合同**
  - 修改：`server/internal/domain/personal/`、`server/internal/app/personal_*`、`server/internal/store/postgres/personal_*`、迁移与 API。历史正文、版本、来源与配额原子处理；旧记忆首次修改保存可用旧正文，恢复用当前版本和幂等键，删除清理历史正文并保留墓碑/抑制。
  - 验收：覆盖来源失权、分页、恢复、并发冲突、配额、删除及跨账号隔离；旧版本没有正文时不可恢复。
  - 已有证据：原清单记录了 domain/app/API 及本机 PostgreSQL 相关测试通过；接口和迁移存在于当前工作树。

- [ ] **T11（待验证，依赖 T10）记忆历史界面**
  - 修改：`web/portal/src/routes/PersonalData.tsx`、`personal-data-store.ts`、`app/api.ts` 和类型；展示来源、时间、可恢复状态及冲突，删除前说明历史正文一并清理。iOS 沿用个人数据同步接收恢复后的新版本。
  - 验收：内置浏览器和实际 PC 检查查看、恢复、无来源、冲突和删除；iOS 可运行时确认同步不覆盖未提交草稿。当前为本地 MSW 增加了仅供合成数据使用的记忆历史分页、版本更新和删除路由及隔离测试；Portal 实际页面已走通创建、编辑、查看来源、恢复旧版生成新版本、确认删除，删除后列表为空。记忆界面、API、MSW 定向 136 项测试通过，Portal typecheck、lint 和 diff 检查通过。实际 `morde-pc` 本轮只读打开记忆页，确认空态渲染正常；账号没有现有记忆，因此未在真实账户执行创建、恢复或删除。iOS 同步实现按服务端 revision 更新内存对象，个人数据 outbox 独立保存且刷新不替换未提交草稿；新增回归测试覆盖远端恢复版本更新本地对象时待提交 outbox 保持不变，并在 iPhone 17 模拟器定向 XCTest 中通过。为让 Minis target 编译 Codex 会话主视图，已将现有 `MordeCodexSessionPolicy.swift`、`MordeCodexSessionsView.swift` 接入 `Minis.xcodeproj`；另同步更新 `MIFTaskViewsTests.swift` 的任务和步骤 fixture，以匹配当前模型字段。无临时排除测试文件的情况下，iPhone 17 模拟器 `NanoMuseMordePersonalDataTests` 与 `MIFTaskViewsTests` 共 **45/45 通过**，Minis arm64 Simulator app target 构建通过。实际 PC 记忆有数据时的恢复/冲突/删除，以及 iOS App 内完整跨端页面行为仍未验收，故保持待验证。

- [ ] **T12（待验证，依赖 T06）Office 文件处理能力**
  - 修改：`runtime/morde-cloud-runtime/`、`server/internal/app/cloud_dsh_skills.go`、工具代理、授权执行环境及产物证明链。先为获授权 Host 的 Office 二进制输出建立受限的登记、Host 独立字节校验、固定 Workspace 版本发布和恢复路径；禁止直接信任模型提供的路径/哈希。再复用上游渐进加载技能内容并适配 Morde 工具名、依赖检查和失败说明，不将 Agent 进程的路径传给 Host。本地 `project/deepseek-harness` 中 `dsh-skill-office` 为 `0.2.2-rc.1`、MIT；Morde runtime 同样固定 DSH `0.2.2-rc.1`，但没有安装 skill registry、Office 工具或 LibreOffice Kit。升级上游版本须按 runtime 依赖、平台、许可证和旧检查点恢复约定单独评估。
  - 验收：在本地授权环境分别创建、检查、修改并打开 DOCX、XLSX、PPTX 合成样例；缺依赖、无权限和处理失败时显示真实原因。云端文件数据面待验证。
  - 当前证据与缺口：本机 Codex 附带的文档库和 LibreOffice 曾对三个合成文件完成创建、修改、重新打开和本地导出，并核对导出文本；这是本机环境检查，未接通 Morde。上游 DSH `dsh-skill-office` 为 `0.2.2-rc.1` 技能 provider，但其 skill registry、依赖加载与 LibreOffice CLI 路径运行在 Agent Node 进程；Morde `shell_exec` 在获授权 Host 执行，不能直接安装该 provider 让它使用错误机器或绕开持久化技能审计。本轮扩展 Morde 固定 `nanomuse_skill_read` 目录，以独立 `office` 路由技能和 DOCX、PPTX、XLSX、视觉检查、公式重算、PDF 导出子技能渐进加载；仅有已选择 Host 的任务可见，执行仅使用当前 Task 的授权 `shell_exec`，不安装依赖、不调用 Agent Node 路径、不用外发 `present`，输出仍要求经过已核验 Task Artifact。
  - 本轮服务端产物链新增成功 Host `shell_exec` durable 证据检查；候选文件仅来自 Host 固定 Task Workspace manifest 与固定 base revision 的差异，不信任模型提供的路径、大小或哈希；只接受新增或修改的普通无链接文件，拒绝敏感路径、大小写冲突、symlink/hardlink 和超限候选，再由现有 CAS 发布与 Host 块字节校验形成成功 receipt。成功 receipt 重放使用原 operation 与 `receipt.BaseRevision`，不重置已前进的 Task view。定向 `server/internal/app` 测试通过：1 MiB 以上合成二进制发布及 receipt 重放、敏感路径/链接/文件数/总字节拒绝、Office 技能目录及 Hostless 拒绝，并覆盖原 broker 文件校验流程；`git diff --check` 通过。
  - 尚缺验收：当前未连接真实授权 Host 或 Office CLI；DOCX/XLSX/PPTX 创建、修改、重开、依赖缺失说明与视觉导出仍需在真实 Host 完成。服务端产物链仅在本地合成 Host runtime 上验证，云端文件数据面未验证。Office 技能与工具授权通过单测确认，实际 DSH 技能调用和宿主环境依赖待补。
  - 2026-11-15 本轮复核：重跑技能授权/Hostless 拒绝、固定技能读取、DOCX/XLSX/PPTX 合成解析和已核验产物访问 7 项定向 Go 测试，全部通过；本机 Morde 服务未启动，所以没有进行真实 PC、DSH 服务任务或 Host Office 操作。

- [ ] **T13（待验证，依赖 T12）Office 已核验产物查看与下载**
  - 修改：`Assistant.tsx`、iOS `MIFTaskViews.swift` / `MIFLibraryViews.swift` 和本地化；PC 复用已核验产物及可用预览/下载入口，iOS 显示同一已确认产物，不能预览时展示元数据和原因。
  - 验收：用合成元数据覆盖撤权、路径/版本/摘要不符和失败提示；内置浏览器、实际 PC 及可运行的 iOS 分别检查状态。实际跨端文件交付待验证。
  - 本轮本地实现与证据：PC 对话已有完整 verified proof 过滤和 `/api/tasks/{taskID}/artifacts/{artifactID}/content` 下载入口；iOS Library 仅在权限、verified 状态、完整 Workspace revision/size/SHA 与本机文件字节一致时提供 Quick Look。没有预览时保留名称、类型、大小和版本，并新增“证明不完整、Workspace 同步不可用、媒体能力未启用、匹配版本无法在本机核验”原因；Task 详情列表现在只显示完整已核验产物，并展示相对路径、类型、大小和 revision。新增 policy/projection 单元测试及中英本地化。
  - 验收结果：Portal Vitest 全量 744 项通过，下载 API 前端专项 1 项通过，Portal Node 文件复制测试 2 项通过；服务端 task artifact content API 定向测试通过；2026-11-15 `MIFLibraryViewsTests` 在 iPhone 17 Simulator 14/14 通过，前一组 `MIFTaskViewsTests` 与个人数据同步 XCTest 合计 45/45 通过，Minis arm64 Simulator app target 构建通过。实际 PC/iOS 交互及真实跨端 Office 文件交付仍需验收。

- [ ] **T17（待验证，依赖 T04、T16）安全的步骤级重试**
  - 修改：`server/internal/domain/task/`、`app/task_*`、`store/postgres/task_*`、API、OpenAPI、必要迁移及 PC/iOS 任务视图。保存失败原因、尝试次数、可重试状态和原调用身份；仅在任务修订、授权、前置结果与副作用均可核实时，经 DSH 和现有工具代理恢复该步骤。
  - 验收：本地隔离环境检查单步骤新尝试、同键重试和并发点击只执行一次；不可重试、撤权、历史输入或副作用未知时拒绝并显示原因；内置浏览器、实际 PC 和可运行 iOS 检查入口/状态。
  - 2026-11-14 本地实现证据：重试接口统一为 `POST /api/tasks/{id}/steps/{step_id}/retries`，客户端只提交任务修订和预期尝试，调用摘要从服务端原步骤读取；重复尝试的历史源步骤保留证据但投影为不可再试。PostgreSQL 锁控并发用例确认同一幂等键只创建一个 attempt；API/app/PostgreSQL 定向测试及竞态 `-race`、Worker retry `-race`、Runtime 109 项测试通过。Portal 59 个文件/727 项测试、重试流程 Playwright 1/1、Portal 类型检查通过；iOS Swift package 54 项通过，重试错误按 task+step 隔离。2026-11-15 重跑 `task-step-retry.e2e.ts`，本地合成重试流程 1/1 通过；同日 SwiftPM `MordeTaskEventIsolationTests` 13 项及完整包 74 项通过。当前 Minis app target 已编入 Codex 会话源文件并通过 arm64 Simulator 构建；iPhone 17 Simulator 的 `MIFTaskViewsTests` 与个人数据同步定向套件共 45/45 通过，覆盖步骤重试策略和错误展示。另在 iPhone 18 Pro Simulator 的 `MordeRegistrationUITests` 中，`testRetryableTaskStepSubmitsOnceAndRefreshesSameRevisionTaskAndEvents` 与 `testTaskStepRetryFailureIsShownAndRetryCanBeAttemptedAgain` 各 1/1 通过，确认重试状态更新与失败后可再次尝试。
  - 尚缺验收：当前实际 `morde-pc` 已可打开本机 Portal，但演示会话没有可操作的重试任务，故不能手工验收重试入口或提交真实任务；2026-11-15 的 CUA 已验证对话壳与文件同步入口，没有用真实任务执行重试。目标账户无任务记录，iOS 本次通过的是隔离模拟器 XCTest 而非完整交互式任务操作。桌面 Electron `npm test`（含 TypeScript 测试构建）本轮全量 266/266 通过；此前一次全量运行有 1 项窗口重开等待超时，单项重跑及随后全量重跑均通过，判断为偶发时序失败，未改动窗口逻辑或测试。全量 Go suite 未运行。真实 PC/iOS 重试操作仍待验收，故 T17 保持待验证。

- [ ] **T18（待验证，依赖 T01、T03）消息操作与回复版本**
  - 修改：`server/internal/app/conversation_*`、`server/internal/store/postgres/conversation_*`、API/OpenAPI、必要迁移、`Assistant.tsx`、`message-outbox.ts` 和 iOS 投影。实现编辑重发、重新生成和继续生成，保存来源与新版本；继续生成核实原调用和检查点，文字重生成不自动重复工具操作。
  - 验收：内置浏览器及实际 PC 检查原文、版本切换、断线重试和滚动位置；本地替身检查账号/会话隔离、幂等及未知副作用不重做，iOS 读取已确认版本。
  - 实施边界：不可变版本以原始 user message 为 group，当前最高版本由 server 在事务中锁定并校验；每个新版本记录 action、user/assistant message、Task 和幂等状态，原文不改不删。版本请求为 `{action, expected_version, content?}`，编辑重发需提交新正文，且复用来源消息的附件授权；重生成不重新调用工具；续写仅在同一 Task revision 与 DSH checkpoint 可验证时受理。消息列表按原 sequence 返回全部版本行和 `variant_group_id`/`variant_version`/`variant_action`，PC 与 iOS 共享同一投影并本地切换版本。
  - 当前证据与缺口：服务端已实现个人/项目会话变体 API、账号与服务实例幂等、不可变历史行、附件沿用来源授权，以及流式/非流式继续生成投影；操作结果未知时持久返回 `202`，不覆盖旧回复。Portal 已支持历史版本切换、编辑并重发、仅对完成回复重新生成文字、仅对中断回复继续生成；版本操作先持久化到 IndexedDB，刷新不自动重放，重试复用同一幂等键。iOS 已同步版本字段、切换和操作 outbox。当前复跑 Portal 类型检查通过，Assistant/Projects/outbox 定向 Vitest 209 项通过，Playwright 编辑、版本切换与草稿保留 1/1 通过；此前 Portal 321 项、iOS SwiftPM 61 项和本机隔离 PostgreSQL 3 项测试也有通过记录。修复了无 `task_id` 的 assistant 消息因 SQL `NULL` 比较而找不到来源 user message 的问题，并补充来源附件授权回归断言。本轮使用一次性本地 PostgreSQL 重跑变体附件、路由、迁移和聚合相关的 9 项定向集成测试，全部通过；未运行整个 PostgreSQL 包测试。实际 PC/iOS 客户端、真实模型重生成及断线恢复验收仍待完成。

- [ ] **T19（待验证，依赖 T04、T16）复杂任务计划与修订**
  - 修改：复用 DSH 主 Agent 与现有授权/任务租约，增加 `morde_task_plan_update` 工具；计划用有界 DAG 校验并写入 append-only `task.plan` 事件，按 Task revision 加计划 revision，不新增数据库迁移。计划不授予权限、不要求用户确认；成本没有可信数据时固定显示未知。Task GET 只返回当前修订计划，SSE 仅发送步骤 ID、序号和修订号；PC Portal 任务详情与 iOS 任务详情显示目标、步骤、状态、依赖、产出和完成条件，过期 Task revision 不展示。
  - 本地证据：计划 DAG 校验、DSH 工具定义/事件指纹、SSE 内容隔离及 Postgres 包编译通过；Portal 类型检查和计划视图定向测试通过；iOS 相关 Swift 文件解析及本地化 JSON 解析通过。修复了一个已有清理重试测试中错误期待重复删除卷的断言，清理顺序仍是先成功删除 Workspace，再各删除一次 Profile/Runtime 卷。
  - 尚缺验收：本轮新增并通过一次性本地 PostgreSQL 集成测试，覆盖同一计划调用并发重放只落一条事件、改写调用被拒绝、旧计划修订冲突、新修订持久化及 Task 快照投影。`go test ./internal/api -count=1` 当前工作树全量通过；完整 `go test ./internal/app` 先前通过。此前记录的 Codex route allowlist 失败未能复现。实际 PC 和可运行 iOS 客户端 UI 尚未验收，故此项保持待验证。

- [ ] **T20（待验证，依赖 T05）审批上下文与到期状态**
  - 修改：Task 审批 domain、服务、存储/API、OpenAPI、`Assistant.tsx`、iOS 任务/审批视图和本地化。展示有依据的对象、影响、风险、本次预计与累计费用、替代方案、撤销方式及期限；未知或不适用如实标示，计划确认不扩大具体操作授权或硬限额。
  - 验收：本地 PostgreSQL 和工具替身覆盖批准/拒绝、到期与并发响应、重复提交、版本过期、权限失效和未知费用；内置浏览器、实际 PC 及可运行 iOS 检查卡片和恢复。
  - 当前证据与缺口：服务端审批响应增加风险、本次/累计费用、替代方案与撤销方式的结构化披露；缺少可信来源时统一返回 `unknown`，不会编造信息。审批决定写入本身再校验数据库期限，覆盖“响应等待事务锁时到期”的竞态，并在同一事务中过期、拒绝和重排任务。API/OpenAPI、披露状态验证、YAML 解析及本地 PostgreSQL `-race` 8 项审批集成回归通过，含离线、执行槽占满、多扫描器、并发响应和到期等待场景；未增加数据库迁移。Portal 已生成 API 类型并在审批卡显示五项字段、未知/不适用状态及安全校验后的费用金额；Assistant 定向 161 项测试、TypeScript 和 ESLint 通过。iOS 现已同步同一组披露字段、未知态与到期/状态未知时禁用决定的规则；审批视图测试覆盖已知、不适用、缺字段、损坏/过期时间，SwiftPM 审批/项目会话测试 52 项通过，独立 Simulator arm64 构建退出码 0。2026-11-14 将现有 Assistant Portal E2E 审批场景扩展为真实卡片交互，使用本地 MSW 检查已知风险、估算费用、未知累计费用、已知替代方案、不适用撤销及期限，Playwright 定向 1/1 通过。2026-11-15 `MIFApprovalViewsTests` 在 iPhone 17 Simulator 19/19 通过。实际 `morde-pc` 打开本机 Portal，但演示会话没有审批卡，无法手工验收卡片交互；完整 PC/iOS 页面交互和真实设备验收仍未完成。

- [ ] **T21（待验证，依赖 T05）任务用量、费用与预算**
  - 修改：复用 Task 用量与既有 `model_call_ledger` / `project_budget_ledger`，Task 详情按同一数据库快照计算调用次数和费用分组，不新增账本、价格来源或迁移。项目费用保留货币与费率版本；显示已结算实际金额、尚未结算的预留估算、未知费用及 BYOK。个人任务没有可信货币费率时只报告未知或自带模型，不由 Token 数推算费用。PC/iOS 同时显示调用、Token、工具调用、活跃时间及任务预算。
  - 本地证据：新增 Task 账单投影、API/OpenAPI 类型、Portal/iOS 任务详情显示和字段校验；新增项目 Task 结算集成断言，检查实际费用与费率版本来自已存快照。`go test ./internal/app -count=1`、Task billing API 定向测试、PostgreSQL 聚合单测及包编译通过；Portal 类型检查与 Assistant 计划/费用 UI 定向测试通过；iOS 源文件解析及本地化 JSON 解析通过。
  - 尚缺验收：一次性本地 PostgreSQL 已通过项目 Task 双账本预留/结算、持久范围与发起人归属、USD 费率版本和 Task 账单投影集成测试；另通过个人模型不使用平台预算及新 Task 零用量投影测试。2026-11-15 扩展 `TestPostgresProjectTaskModelReservationUsesPersistedScopeAndInitiator`，在真实 `GetTask` 投影中验证多费率已结算汇总、未结算预留、带费率/无费率未知费用，以及个人模型含未知调用均显示 BYOK；隔离 PostgreSQL 集成测试通过。新增本地 Playwright `task-billing.e2e.ts`，检查任务详情展示调用与预算、Token/工具/活跃时间、已结算、预留估算、未知、BYOK 和费率快照，1/1 通过；2026-11-15 重跑该 E2E，1/1 通过。iOS `MIFTaskViewsTests` 的账单投影断言已随定向模拟器测试运行通过；PC/iOS 实际任务详情 UI 及真实费用数据仍未验收，故此项保持待验证。

- [ ] **T22（待验证，依赖 T04）任务排队、公平与能力匹配**
  - 修改：复用现有领取、租约、资源围栏和 DSH 子 Agent 授权能力，不另建 Agent Runtime 或第二套工具匹配逻辑。给任务增加持久化的置顶、普通、后台优先级；两阶段任务领取使用同一排序规则，任务从创建起满 15 分钟按总等待年龄进入公平队列，等待用户、电脑或恢复期间的时间也计入，以免任务恢复后重新从队尾开始。通过带事件序号围栏和幂等键的 `set_priority` 操作修改仍在排队的任务，写入仅公开优先级字段的 `task.priority` 事件；PC/iOS 展示状态与修改入口，不显示无法准确计算的队列位次。
  - 本地证据：增加数据库迁移、Task/API/OpenAPI 字段、存储与服务边界校验、事件脱敏、管理事件统计及 PC/iOS 视图。`go test ./internal/api -run 'QueuePriority|TaskEventPayloadsExposeOnlyTypeAllowlistedFields'` 和 `go test ./internal/app -run Priority` 通过；PostgreSQL 集成用例覆盖置顶/普通/后台排序、15 分钟公平提升、同级 FIFO、幂等重放、旧事件序号冲突及运行后禁止改优先级；此前本机一次性 PostgreSQL 对队列排序用例的运行记录通过。本轮修正队列时长指标：实际 queued 任务使用最近一次持久化 `task.status=queued` 事件时间，优先级更新不重置等待值；租约恢复等非 queued 领取不计为排队等待。Worker 指标定向测试通过；在本机隔离 PostgreSQL 上新增的优先级公平排序与暂停后重新入队计时集成测试 2 项均通过。新增双 Worker 并发领取集成用例，验证首批同时领取包含已等待 15 分钟的后台任务和最早置顶任务，剩余任务继续按置顶、普通、后台顺序领取；T22 三项 PostgreSQL 定向集成测试以 `-race` 运行通过。`go test ./internal/store/postgres -run '^$' -count=1` 编译通过。Portal 类型检查、ESLint 通过，Assistant/Projects 定向测试 209 项通过；SwiftPM 任务事件隔离测试 13 项通过。
  - 尚缺验收：本地 PostgreSQL 集成测试已覆盖双 Worker 同时领取、公平提升和低优先级最终获领；仍未通过实际 `morde-pc` 任务界面操作真实排队任务来验收优先级显示与提交。本机服务当前未启动，无法完成该项实际客户端验收。本轮的 iOS Simulator XCTest 覆盖任务投影相关单测，但未运行 `MinisUITests` 交互 target 或实际 iOS 设备验收，故此项保持待验证。

- [ ] **T23（待验证，依赖 T15、T17、T20–T22）必要指标与降级状态**
  - 修改：复用现有 Prometheus `/metrics`、API 请求指标与持久化任务/账本状态，增加聚合等待与纠正、任务领取等待、事件续读、审批等待、重试结果和已结算模型调用/Token 指标。标签只使用固定来源、状态、优先级和操作类别；不记录正文、凭据、工具输出、账号或任务 ID。指标只作进程内观测，任务、审批、结果和用量账本仍以共享持久化存储为准。
  - 本地证据：Worker 聚合等待/纠正和队列优先级指标、API 事件续读/审批/重试指标、模型网关结算后的已知/未知用量指标均已接入；指标不参与任务接受或结算结果，观测回调异常会被隔离。队列等待直方图现在只计量真实 queued 状态并从持久状态事件读取进入时间，恢复领取不伪装成排队；对应 Worker 单测通过。API、App、Worker、Portal 服务端 Go 包全量测试通过；新增指标测试覆盖固定标签、聚合修正/等待、审批耗时、重试结果、事件续读和 Token 计数。
  - 尚缺验收：任务队列优先级 PostgreSQL 集成用例现已在一次性本地数据库实际通过，覆盖领取排序和数据库时间戳。多实例重启、容量限流、非必要展示降级和授权查询/订阅的组合验收仍未运行；指标按进程累计，重启后归零，账本才是模型费用与用量的权威记录。现有 readiness/能力状态仍是降级入口，本项未增加新的自动降级策略，故保持待验证。

- [ ] **T14（待验证，依赖 T09–T13、T17–T23）P1 整体验收**
  - 修改：本地验收记录，汇总搜索、记忆、Office、计划、重试、审批、费用与队列的候选版本、证据和缺口。
  - 验收：各项实际行为均有对应证据后才勾选；真实产物交付和跨端文件验收受当前文件限制保持待验证。规模验收由 T24 单列。

## 规模验收

- [ ] **T24（待验证，依赖 T23）万人在线对话容量**
  - 修改：在 `server/tools/morde-capacity/` 提供本地 Go 负载 CLI 与运行说明；用对话历史/活动任务轮询模拟在线会话，用 Task SSE 测并行任务、长任务比例和断线恢复，并支持受控短聊连发与审批写入。开测记录环境、成功标准、速率、延迟、状态码和客户端资源；完整验收还需记录模型调用及服务端资源。
  - 验收：记录实际延迟、错误率与资源用量；环境不足或未达到目标时保留待验证，本地替身结果不代表生产容量。不得向远端发送脚本或文件。
  - 当前实现：工具以 `/api/conversations/{id}/messages` 与活动 Task 列表周期轮询虚拟会话，可同时建立活动 Task SSE 流并按 `Last-Event-ID` 重连；长任务比例由任务清单标记，消息支持按用户配置连发，审批需显式 `-enable-writes`。远程目标强制 HTTPS 和精确 `-allow-host`；凭据清单要求本地 0600 文件，报告不含凭据或 Task ID。开测前检查 `cloud_tasks` 并核验会话历史；优先使用 `/api/conversations/{id}/tasks?view=active`，仅当该路由返回 404 时才回退到成功的 `/api/tasks`，报告标明 `task_poll_scope`。其他接口错误会在压测前退出。报告输出延迟分位数、状态码、速率和运行期间采样的客户端峰值 goroutine/堆/系统内存。只读虚拟客户端可复用账号，但并行 SSE 必须使用不同的活动 Task ID；复用同一身份只验证同一会话的数据路径并发，不代表跨用户隔离。
  - 本地验证：`go test -race ./tools/morde-capacity -count=1`、`go vet ./tools/morde-capacity`、`gofmt` 和 `git diff --check` 通过；测试使用本地模拟 API。
  - 测试机基线（2026-11-15，只读）：`/api/capabilities` 返回 200 并声明 `cloud_tasks`；管理后台配额为 2 vCPU/4 GiB，已分配 0.5 vCPU/1.5 GiB，有 1 台 running 云端电脑。SSH 读取到节点 4 个在线 CPU、内存总量 7,886,737,408 bytes（约 7.35 GiB）、可用 5,432,565,760 bytes、无 swap；后台数字是分配池口径，与节点规格不同。公网 `GET /metrics` 返回 404，应用配置要求 Metrics listener 绑定 `127.0.0.1`，压测时需通过节点侧采集服务资源与模型指标。
  - 远端冒烟与缺口：使用管理员账号签发了一个 iOS Client-only 临时安装凭据，清单保存在本机 0600 临时文件；10 个只读虚拟客户端运行 12 秒，消息历史请求 60/60 返回 200，P95 84.926 ms；同一会话活动任务接口 60/60 返回 404，而账号级 `/api/tasks` 返回 200。原会话路由预检因此在负载开始前退出；工具现可在此特定 404 场景使用账号级任务轮询，并会在报告标注范围。用户表有一个已通过的 demo 账号（无云端电脑或 Workspace）和一个待审核账号。未创建账号、会话或 Task，未发送消息/审批写入或文件；注销调用返回 HTTP 500，但后续用临时凭据查询 `/api/clients` 返回 401，确认凭据已失效，本机临时清单已删除。目标会话任务路由与当前 API 契约不一致，需核实服务端版本/路由；并行活动 Task、真实 10,000 客户端负载、模型调用和服务器负载监控仍待验收。

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
