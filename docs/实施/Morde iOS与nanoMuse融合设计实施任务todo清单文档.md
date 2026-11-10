# Morde iOS 与 nanoMuse 融合界面实施 TODO

> 本文保留拆分前的任务内容与状态快照。实际执行请从[并行执行入口及任务总览](<Morde iOS与nanoMuse融合设计并行实施/00-并行执行入口及任务总览.md>)进入；各项任务的实时状态只在对应子清单维护。

来源：[融合设计方案](<../Morde iOS与nanoMuse融合设计方案.md>)、[Morde iOS 设计稿](<../Morde iOS设计稿.html>)、[五会话并行入口](<nanoMuse与Morde合并五会话并行实施/00-并行执行入口及任务总览.md>) 和[会话 3 iOS 原工程清单](<nanoMuse与Morde合并五会话并行实施/03-iOS原工程实施任务todo清单文档.md>)。本清单只列融合界面相对会话 3 的增量，不替代 MI-01～MI-09，也不把它们的进行中结果标为完成。

## 实施边界和衔接

- 正式目标是 `project/nanoMuse/android/src/ios/Minis.xcodeproj` 的 `morde` scheme。会话 3 当前独占 `project/nanoMuse/android/src/ios/**` 及其清单：本清单由会话 3 串行承接，或在其明确交接、释放相应文件后实施；不能同时由另一会话修改这些文件。跨服务端合同分别交会话 1、4、5 的文件所有者，不能绕过现有边界改共享 OpenAPI、迁移或生成物。
- MI-01～04 等仍在执行，MI-07 已完成本地原工程及模拟器验收；各项状态以会话 3 最新清单为准。实施各项前读取最新交接，只复用已交付的账号、会话、Task、个人内容及资源能力；不等待无关项全部完成，也不覆盖进行中的改动。涉及的 iOS 文件交接后再写入。
- 根据本次确认，动态、点子、目标及目标页内例程不迁入新侧边栏，按 MI-08/MI-09 处理旧入口和深链。普通用户例程的独立设置入口按 MI-08 既定范围处理；不清空本地或云端数据、不取消云端 Schedule。MI-05/06、MI-08/09 的具体清理确认和状态仍由原清单管理，本清单不擅自删除或恢复文件。
- 好友、AI 工具会话记录只做入口和“暂未实现”占位页，不请求好友/工具数据、不模拟成功。任务、审批和项目工作项界面不展示预算、预算进度或费用；保留服务端已有的额度校验和用户数据。
- 沿用原 App 的 Bundle ID、App Group、Keychain、扩展、历史和账号隔离；不得重新启用已关闭的本机 Agent、旧 nanoMuse Cloud/Relay/Hub 或直连模型。`NanoMuseHeader.swift` 当前被原 target 排除，复用已编入 target 的 nanoMuse 视觉组件，不为了复用旧聊天逻辑解除排除。
- Task 输入保持纯文本：当前合同不接受图片、视频、音频或 personal-data 附件作为 Task 输入。历史附件、Workspace 文件和生成媒体各按现有合同及 capability 处理；`workspace_sync`、`task_media_generation` 未开放时显示未就绪。实施和验收仅用本机替身与合成元数据，不上传、下载、分享或发送文件。

## 逐页参考设计稿

实施每个界面前，先查看 `Morde iOS设计稿.html` 中对应页面、控件和交互脚本，参考其信息层级、入口和操作流程；视觉组件沿用 nanoMuse iOS。设计稿里的示例人物、数据和模拟成功提示不作为真实数据使用。好友、AI 工具会话的占位及预算显示删除，继续按上述已确认范围执行。

| 清单任务 | 设计稿中的参考位置 |
| --- | --- |
| MIF-01、02 | 底部导航、侧边栏、`data-view="chat"` |
| MIF-03、04 | `data-view="tasks"`、`data-view="task"`、`data-view="chain"` |
| MIF-05 | `data-view="appr"` |
| MIF-06、07 | `data-view="work"`、`wiSheet` |
| MIF-08、09 | `data-view="res"`、`ws`、`pc`、`lib`、`file` |
| MIF-10、11 | `data-view="friends"`、`toolchat`、`settings` |

每项验收时对照相应设计稿场景，核对页面入口、展示内容和本清单要求的操作；在任务记录中写明已参考的场景，以及因用户要求或当前能力限制而存在的差异。

## 接口与模块变化

| 接口或模块 | 本次目标和约束 |
| --- | --- |
| `NanoMuseTab`、`NanoMuseRoomDestination`、`NanoMuseBottomBar`，位于 `NanoMuseShell.swift` | 将根目的地改为聊天、任务、审批、资源，继续使用 nanoMuse 胶囊栏、无障碍标签和键盘避让。旧 `feed`/`ideas`/`goals` 深链按 MI-08/MI-09 的确认结果安全回退或提示不可用。 |
| `MordeClient`，位于 `MordeApp.swift` | 现为私有客户端，`load()` 只保留 `/api/tasks` 首 100 条且丢弃游标。拟在现有客户端增加按状态及游标取任务摘要、按 ID 读取详情及事件、按任务 ID 执行操作的内部入口，向新房间提供账号作用域内的状态；不要另建客户端或嵌套第二个 `TabView`。 |
| `GET /api/tasks?status=&cursor=`、`GET /api/tasks/{id}` | 沿用现有 HTTP 合同。列表返回 `TaskSummary` 和 `next_cursor`，不含审批请求、步骤或产物；详情返回 `steps`、`artifacts`、`pending_request` 等。扩展 iOS 解码及分页，审批页需对 `waiting_user` 详情区分审批和澄清。 |
| `GET /api/tasks/{id}/events?after=` | 沿用 SSE 合同及服务端过滤后的事件。游标过期收到 `task.snapshot_required` 时读取快照并提示较早事件可能不可用；204、断线、页面关闭和账号切换不得取消任务，也不伪造完整历史。 |
| `POST /api/tasks/{id}/actions`、`POST /api/tasks/{id}/approvals/{approval_id}/responses` | 沿用原接口。任务动作携带合法 `expected_status`；审批携带请求 `version` 和 `approve`/`reject`；使用 `Idempotency-Key`。响应 202 是已受理，最终状态以刷新为准；未知结果重试原请求及原键，冲突后刷新，不替用户换一种操作。 |
| 项目工作项 `GET/POST /api/projects/{id}/work-items`、`PATCH /api/projects/{id}/work-items/{work_item_id}` | 基础列表、创建和分配沿用当前合同。**拟扩展**列表返回和创建输入：提供缺陷/需求/历史未分类类别，以及明确的未认领/负责人状态；认领优先沿用 PATCH 的目标成员、版本和幂等检查。目前没有类别字段，且 `assignee_id` 必填、创建默认分配给调用者，不能把 `open` 冒充“待认领”。版本兼容和字段命名由会话 4/5 在 MIF-07 冻结。 |
| `GET /api/hosts`、`GET /api/shared-hosts`、`GET /api/workspaces`、Host control/entry 与 Workspace 文件接口 | 沿用现有账号权限、绑定状态、租约及短期票据。资源页复用现有电脑和文件视图；没有 capability 或无权限时显示原因，不伪造数据，也不放宽 WKWebView 同源限制。 |
| `GET /api/execution`、`POST /api/execution/stop-all`、`POST /api/execution/enable` | 现有接口只接受 Portal Session，客户端凭据不能直接调用；复用已有同源短期 Portal Session 调用路径。变更请求带 `expected_epoch` 和 `Idempotency-Key`；停止返回 202 时继续显示“停止中”，读取至 `disabled` 才显示已停止。 |

除 MIF-07 列明的拟扩展项目工作项合同外，本清单不新建 HTTP 协议。Task 输入附件、好友、工具会话记录均不新增接口；媒体输出继续依赖 MR-04/MX-05 已有合同和 capability。

## 实施任务

- [ ] **MIF-01（未开始）统一四个根入口和 nanoMuse 外壳**
  - 修改：由会话 3 在交接后的 `NanoMuse/NanoMuseShell.swift`、必要的 `Morde/MordeApp.swift` 和本地化中接入聊天、任务、审批、资源四个目的地；复用 `NanoMuseBottomBar`、`NanoMuseTones` 和已编入原 target 的头像/页面组件。Morde 页面作为房间嵌入，不再嵌套独立根标签栏。
  - 验收：原 App 的四个入口可切换和返回，选中态、审批提示、键盘/安全区及 VoiceOver 标签正确；任务页返回后不丢聊天草稿。已移除板块不出现在新抽屉，旧路由与数据按 MI-08/MI-09 处理。

- [ ] **MIF-02（未开始，依赖 MIF-01、MI-03 的会话交接）融合聊天头部和抽屉**
  - 修改：在现有 `MordeConversationRoom` 使用 nanoMuse 头像、名称、消息气泡、输入胶囊和状态视觉；状态由当前 Morde Task 驱动。抽屉接入 Morde 主会话、分页最近会话、新建与标题搜索，保留本地历史只读入口、设置及智能体资料；搜索只声称已实际加载的范围。
  - 验收：本机替身覆盖主会话、旁聊、新建、搜索、返回与空/失败态；切换会话、切页和账号时不串消息、草稿或 Task，已受理 Task 不因关闭页面而取消。

- [ ] **MIF-03（未开始，依赖 MIF-01、MI-03 的 Task 合同交接）实现任务列表和详情**
  - 修改：在现有 `MordeClient` 增加任务摘要分页及按 ID 取详情，扩展 `MordeTask` 对真实步骤和产物的解码；展示目标、状态、当前步骤、结果及允许的暂停、继续、取消、重试。聊天摘要和列表跳转使用同一 Task ID；动作复用现有重试及账号作用域保护，不因查看另一任务覆盖正在聊天的选择或草稿。
  - 验收：本机替身覆盖超过一页、去重、空列表、未知状态、加载失败、账号切换和 202 后刷新；不可用动作隐藏或禁用，不显示预算或费用，也不编造进度。

- [ ] **MIF-04（未开始，依赖 MIF-03）实现任务步骤和事件链**
  - 修改：在任务详情使用 Task 快照的步骤和现有事件流按序展示；扩展 `MordeApp.swift` 的 SSE 消费以保留可展示事件，不绕过服务端字段过滤。事件只展示真实可见的步骤和状态，不展示预算类型的结构化数据。
  - 验收：本机替身覆盖事件排序、重复事件、断线重连、`task.snapshot_required`、204、无历史和权限变化；早期事件不可用时有说明，退出详情不停止 Task，聊天原有 SSE/快照恢复仍通过。

- [ ] **MIF-05（未开始，依赖 MIF-03）实现审批收件箱**
  - 修改：分页读取 `waiting_user` 任务摘要，再读取详情中的 `pending_request`；澄清问题不计入审批。展示来源、对象、内容、影响、当前状态和批准/拒绝，风险及授权范围只在服务端明确提供时展示；复用媒体审批 capability 门禁。历史只展示任务事件或通知中能证实的记录。按任务 ID、审批 ID 和版本处理，保留 Morde 与 nanoMuse 长期授权的区别。
  - 验收：本机替身覆盖多页、澄清混入、部分详情失败、项目任务权限、批准/拒绝、版本冲突、重复点击及响应未知重试。未完成读取时提示部分/未知，不能显示完整的零待办或错误数量；决策不改变其他会话的草稿。

- [ ] **MIF-06（未开始，依赖 MIF-01）接入项目工作项基础功能**
  - 修改：复用 `MordeProjectsRoom`、`MordeProjectWorkItem` 和现有项目工作项请求，在任务页的“工作项”分段选择项目并展示真实列表、创建、已有阻塞/恢复、关联 Task 和交付操作；按角色限制编辑。抽取或组合页面时不得把现有项目预算展示带入新界面。
  - 验收：本机替身覆盖项目切换、分页、创建、权限不足、版本冲突、关联任务和返回；普通任务与工作项的状态不混用。分类、待认领与认领按 MIF-07 完成后才能视为原稿工作项功能全部完成。

- [ ] **MIF-07（阻塞：工作项分类与认领合同尚未定义，需交会话 4/5；依赖 MIF-06）补齐原稿筛选和认领**
  - 交接：由会话 4/5 为现有工作项列表/创建响应明确缺陷、需求和历史未分类的表示，以及未认领、已认领、待验收与现有 `open`/`blocked`/`delivered` 的关系。拟扩展现有 `GET/POST /api/projects/{id}/work-items` 的字段；认领优先复用现有 `PATCH /api/projects/{id}/work-items/{work_item_id}` 的 `assignee_id`、`expected_version` 和幂等键，由服务端校验当前项目成员及竞争冲突。当前 `assignee_id` 必填：允许未分配前须明确版本/capability 与 PC、Portal、旧 iOS 客户端兼容方案；历史记录不得猜测分类或改变负责人。
  - 修改：合同、迁移和生成物分别由现有所有者交付后，会话 3 接入“全部/缺陷/需求/待认领”筛选、新建类型及认领动作；合同未就绪时不显示可误操作的伪筛选或假成功。
  - 验收：本机替身覆盖历史未分类、两种类型、未认领、并发认领的版本冲突、权限不足及旧客户端响应兼容；不把 `open` 等同未认领。

- [ ] **MIF-08（未开始，依赖 MIF-01、MI-04 的资源交接）整合资源与云端电脑**
  - 修改：在资源页组合自有/共享 Host、Workspace、控制状态及文件入口；复用 `MordeComputerRoom`、`MordeWorkspaceFilesView` 的权限和状态逻辑。接管、交还和续租以控制人、租约与服务端状态为准；默认电脑/Workspace 尚未接通时显示未设置，不自动改变已受理任务的目标。
  - 验收：本机替身覆盖运行/离线、只读共享、绑定不就绪、控制状态未知、租约过期、接管/交还失败及返回；查看器保持短期票据和 HTTPS 同源限制，不注入本机 JavaScript 桥，也不连接真实电脑。

- [ ] **MIF-09（未开始，依赖 MIF-02、MIF-03、MIF-08）接入文件库和产物展示**
  - 修改：复用 `NanoMuseRooms.swift` 的本地/个人文件列表及 Morde Workspace/Task `artifacts` 元数据，区分来源和可用性；仅对有权限且能力就绪的记录提供现有预览入口。Task 输入附件保持关闭；生成媒体预览/保存继续沿用 MI-03 的 `task_media_generation` 条件和确认流程。
  - 验收：用本机合成元数据检查分类、空态、无权限、无证明的旧产物、能力关闭和本地预览入口；不上传、下载、分享或发送文件，不把历史 `attachment_ids` 当成 Task 输入授权。

- [ ] **MIF-10（未开始，依赖 MIF-01）添加好友和 AI 工具会话占位入口**
  - 修改：侧边抽屉接入两个 nanoMuse 风格的占位页，明确显示“暂未实现”；不读取或构造好友列表、工具状态与对话记录，不实现添加、审批、分享、撤销或打开工具对话动作。
  - 验收：本机点开两个入口能返回，页面有无障碍名称及未实现说明；不发起相关请求，不出现虚假数据或成功提示。

- [ ] **MIF-11（未开始，依赖 MIF-01）接入设置中的全局急停**
  - 修改：在 `NanoMuseSettings.swift` 和现有 `MordeClient` Portal Session 路径接入执行状态、停止全部与用户主动恢复执行；明确这是当前账号的全部云端任务及后续执行开关，确认前不提交。保存未确认请求的原 epoch/幂等键，按合同处理 202、409、403、断网与恢复。
  - 验收：本机替身覆盖取消确认、重复点击、202 停止中到 disabled、冲突刷新、结果未知重试和显式恢复；不将 202 或网络失败显示为已停止，不展示额度/预算字段。

- [ ] **MIF-12（未开始，依赖 MIF-01～06、MIF-08～11；MIF-07 解阻后补验）统一视觉、本地化与原工程验收**
  - 修改：补齐 `Localizable.xcstrings` 所有既有语言，按 iOS 16 兼容方式处理状态监听；对新页面落实浅色/深色、Dynamic Type、VoiceOver、减少动态效果、长标题及错误/空态。更新受影响的 `MinisTests/NanoMuseLogicTests.swift`、Morde 客户端测试和 `MinisUITests/MordeRegistrationUITests.swift` 或同目录测试。
  - 验收：在交接后的原 `Minis.xcodeproj` / `morde` scheme 完成 generic iOS Debug 构建和专用干净 Simulator 的本机替身验收，按“逐页参考设计稿”映射核对各场景，并走通聊天→任务→事件链→审批→资源及两个占位页；覆盖账号 A/退出/B、分页、权限变化、断线、未知状态、深链和数据保留。记录设计稿对照结果、实际结果与未验项，使用会话 5 交付的可复现 Simulator 依赖；不碰已有用户数据模拟器，不访问真实 Portal、发送文件或通知。原生页面在 Simulator 验收，涉及的本地 Web 页面另用内置浏览器检查。

## 交接与完成条件

- MIF-01～06、08～11 由会话 3 或明确交接后的集成者与页面组按[并行执行入口](<Morde iOS与nanoMuse融合设计并行实施/00-并行执行入口及任务总览.md>)的文件归属及依赖推进；MIF-12 可先验已具备接口的流程。MIF-07 的工作项合同未交付时只阻塞原稿分类/认领及其最终验收，不能让其他页面停工。
- 原工程迁移、MI-08/09 的具体清理及用户数据验收由会话 3 原清单继续负责。完成本清单前核对其最新状态；具体删除范围未确认时不得把旧入口清理或整体验收标为完成。
- 每项完成后由实际实施者在对应子清单记录修改文件、本机验收结果、剩余限制和跨会话交接；会话 3、其他会话清单及并行入口由各自所有者更新。所有任务的实现和本项验收完成后才在子清单勾选 `[x]`。
