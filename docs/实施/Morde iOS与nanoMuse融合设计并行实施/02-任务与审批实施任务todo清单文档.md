# 并行 02：任务、事件链与审批实施 TODO

先读[并行执行入口](<00-并行执行入口及任务总览.md>)、[融合设计总清单](<../Morde iOS与nanoMuse融合设计实施任务todo清单文档.md>)及[设计稿](<../../Morde iOS设计稿.html>)。本文是 MIF-03、MIF-04、MIF-05 的唯一任务状态来源；下文完整保留总清单原任务和状态，实施结果记录在本文。总清单不另行维护这些任务的第二份状态。

## 并行边界

- 原五会话中的会话 3 仍独占 `project/nanoMuse/android/src/ios/**`。其明确交接相关写入权，且 01 集成者冻结共享接口之前，本文工作者只能只读分析，不得预写 Swift 或测试文件。
- 上述两个门槛满足后，只在本目录下的本机暂存目录 `.staging/02/` 准备界面和独立测试：任务与事件链使用 `MIFTaskViews.swift`、`MIFTaskViewsTests.swift`；审批使用 `MIFApprovalViews.swift`、`MIFApprovalViewsTests.swift`。任务和审批各自独占自己的两个文件，不并发修改同一文件。
- 公共客户端、DTO、`URLProtocol` 与共享测试替身、`Localizable.xcstrings`、`Minis.xcodeproj` 和根壳只由 01 集成者修改；本文工作者不改 `MordeApp.swift`、`MordeCredentialExpiry.swift`、`NanoMuseShell.swift`、服务端或其他工作者文件。冻结的接口不能满足页面时写明输入、输出和调用场景交 01 处理，不复制客户端或自行放宽权限。
- 本文由 02 负责人统一更新任务状态；页面工作者把暂存结果交 02 负责人和 01 集成者。暂存文件由 01 串行纳入原 `Minis.xcodeproj` / `morde` target，并在整合后用本机替身验收；只有实现及本项验收通过才勾选。暂存完成但尚未整合的任务保持未完成，可注明待验证。
- 先对照设计稿的 `data-view="tasks"`、`task`、`chain`、`appr` 及相关交互脚本，采用 nanoMuse iOS 风格。预算和费用不展示；好友与 AI 工具会话记录维持占位。只在本地工作，不外发、上传、下载、分享或发送文件。

## 原任务与状态

- [ ] **MIF-03（进行中：Task 页面接口已冻结，列表与详情正在暂存）实现任务列表和详情**
  - 修改：在现有 `MordeClient` 增加任务摘要分页及按 ID 取详情，扩展 `MordeTask` 对真实步骤和产物的解码；展示目标、状态、当前步骤、结果及允许的暂停、继续、取消、重试。聊天摘要和列表跳转使用同一 Task ID；动作复用现有重试及账号作用域保护，不因查看另一任务覆盖正在聊天的选择或草稿。
  - 验收：本机替身覆盖超过一页、去重、空列表、未知状态、加载失败、账号切换和 202 后刷新；不可用动作隐藏或禁用，不显示预算或费用，也不编造进度。

- [ ] **MIF-04（进行中：共享事件链接口已冻结，与 MIF-03 并行暂存）实现任务步骤和事件链**
  - 修改：在任务详情使用 Task 快照的步骤和现有事件流按序展示；扩展 `MordeApp.swift` 的 SSE 消费以保留可展示事件，不绕过服务端字段过滤。事件只展示真实可见的步骤和状态，不展示预算类型的结构化数据。
  - 验收：本机替身覆盖事件排序、重复事件、断线重连、`task.snapshot_required`、204、无历史和权限变化；早期事件不可用时有说明，退出详情不停止 Task，聊天原有 SSE/快照恢复仍通过。

- [ ] **MIF-05（进行中：审批回调接口已冻结，收件箱正在暂存）实现审批收件箱**
  - 修改：分页读取 `waiting_user` 任务摘要，再读取详情中的 `pending_request`；澄清问题不计入审批。展示来源、对象、内容、影响、当前状态和批准/拒绝，风险及授权范围只在服务端明确提供时展示；复用媒体审批 capability 门禁。历史只展示任务事件或通知中能证实的记录。按任务 ID、审批 ID 和版本处理，保留 Morde 与 nanoMuse 长期授权的区别。
  - 验收：本机替身覆盖多页、澄清混入、部分详情失败、项目任务权限、批准/拒绝、版本冲突、重复点击及响应未知重试。未完成读取时提示部分/未知，不能显示完整的零待办或错误数量；决策不改变其他会话的草稿。

原任务中涉及 `MordeClient`、DTO、SSE 消费和共享替身的修改由 01 集成者实施；02 页面工作者负责上述暂存视图和独立测试。审批响应 202 仅表示已受理，状态以刷新结果为准；结果未知时沿用原请求和幂等键。

## 交接记录

### 2026-11-04 02 组只读交接核查

- **依赖状态：**原 `03-iOS原工程实施任务todo清单文档.md` 仍将 MI-03 标为进行中，未找到会话 3 对 `project/nanoMuse/android/src/ios/**` 的明确写入权交接；01 清单的 MIF-B01 仍未开始，未记录已冻结的可编译 Swift 页面接口。因此依照本清单边界，本轮未在 `.staging/02/` 写 Swift 或测试，也未触碰原工程。
- **客户端现状：**`MordeApp.swift` 中的 `MordeTask`、`MordeClient` 为文件私有；任务列表当前只加载单页（最多 100 条），无分页游标；`watchCurrentTask()` 只更新当前会话 Task 快照，不留可供事件链页消费的事件历史；审批/澄清处理虽已有版本、幂等键、409/未知结果刷新，但依赖当前聊天 `currentTask`，项目会话使用另一条授权路径。新页面不能直接访问这些类型，也不能另建 SSE/审批状态机。
- **服务端合同已核对：**OpenAPI 提供 `GET /api/tasks` 分页与状态过滤、`GET /api/tasks/{id}` 完整快照、Task 事件 SSE、按版本批准/拒绝及通用动作。审批收件箱需从 `waiting_user` 列表逐项读详情，只取 `pending_request`；`clarification_request` 是澄清，不是审批。202 仅代表受理，未知结果须沿用原请求及幂等键；项目读取须保留其独立授权路径。Task/Event 含预算、usage 和结构化 payload；页面不得展示预算/费用，事件只允许按字段白名单呈现，步骤过滤 `kind=budget`，不得直接展示原始 payload。演示稿的金额、进度、人员、测试数、“灰度”操作均为虚构示例，不接入真实 UI。
- **交给 01 冻结的最小接口：**账号及服务实例作用域变化状态；任务列表的 `status/cursor` 输入和 `items/next_cursor` 输出；按真实 Task ID 加载详情；从单一事件流所有者取得按序、已去重且经字段白名单处理的可展示步骤/事件，并可用 `event_sequence` 恢复及在 `task.snapshot_required` 后刷新快照；按 Task ID 调用现有动作；按 Task ID、approval ID、version 调用现有批准/拒绝处理并暴露刷新/未知结果状态。页面不接管 SSE 生命周期、不持有新客户端、不要求聊天当前 Task 被切换；个人与项目 Task 的授权分路继续由客户端负责。
- **下一步条件：**由会话 3 明确承接本组或交接原工程写入权给唯一集成者；01 完成并记录 MIF-B01 可编译接口；MI-03 Task 合同交接满足后，再开始 MIF-03，随后顺序实现 MIF-04/MIF-05，并由唯一集成者纳入原工程及验收。以上状态更新只保存在本地忽略的 docs/ 中；未访问 Portal/Host、未使用所给凭据、未运行测试或发送文件。

### 2026-11-04 用户授权后的工作接管

- 用户明确授权当前会话作为唯一集成者接管完成 MIF-B01 和 MIF-03～05 所需的共享文件。上一节记录的是授权前状态；本节及 01 清单记录授权后的执行范围。当前开始 MIF-03/MIF-B01，不覆盖整个原 iOS 会话，也不改其 MI-03 总体状态。

在本节记录 03 原会话的文件交接、01 冻结的共享接口、已参考设计稿场景、暂存文件、整合验收结果和剩余限制。不要将接口待定或尚未整合的内容记为完成。

### 2026-11-04 Task/审批页面接口冻结

- 01 集成者在 `MordeApp.swift` 中冻结并编译验证跨文件接口 `MIFTaskPageStore.shared`。页面使用只读状态 `isSignedIn`、`scopeKey`、`tasks`、`details`、`timelineEvents`、`timelineErrors`、`expiredTimelines`、`loadingTimelines`、`emptyTimelines`、`actionErrors`；调用 `taskPage(status:cursor:)`、`task(id:)`、`actOnTask(taskID:action:)`、`respondToApproval(taskID:approvalID:version:decision:)`、`watchTimeline(taskID:)`。精确类型及动作签名见 01 清单冻结记录。
- `MordeClient` 仍为文件私有单一生命周期所有者；详情/事件页不改聊天当前 Task、会话或草稿。事件视图只读取 `MordeTaskTimelineEvent.display`，不可读取原始事件 payload；跨账号/服务作用域通过 `scopeKey` 隔离。
- 原工程 `morde` scheme 的 arm64 iOS Simulator Debug 构建通过。Mock/UI 验收尚未完成，MIF-03～05 均保持未勾选并继续实施。

### 2026-11-05 Task 页面与审批失败项复验

- **修正：**空列表验收改查稳定无障碍标识；失败重试按钮补充无障碍标识。审批冲突和未知状态用例改为从任务入口打开对应详情，再按 Task/审批标识验证页面和已隐藏动作。重试夹具覆盖登录后的任务预载与任务页读取两次初始失败，手动重试才返回空列表；审批状态查询仍返回独立空页。
- **验收：**新建专用 iPhone 17 / iOS 27.0 Simulator 上四个此前失败的 UI 用例 4/4 通过（`/tmp/MIF02TargetedUI20261105.xcresult`）；任务、审批、文件库、工作项和数据作用域单测 31/31 通过（`/tmp/MIF02MIFUnit20261105.xcresult`）。`morde` generic iOS Debug 编译在 `CODE_SIGNING_ALLOWED=NO` 下通过（`/tmp/MIF02GenericBuildNoSigning20261105.log`）；默认签名构建因本机未配置开发团队而失败，不影响无签名编译结论。
- **状态与边界：**MIF-03、MIF-04、MIF-05 保持未勾选；这轮只补验空态、加载失败重试、未知状态详情、审批冲突刷新及 204 无历史跳转，尚未替代原清单中的完整分页、权限、事件恢复、重复决策和跨账号验收。所有 UI 请求由进程内 DEBUG fixture 接管；未访问真实 Portal/Host/API key，也未传输文件。
