# 并行组 01：共享接口与 iOS 外壳实施 TODO

先读[并行执行入口](<00-并行执行入口及任务总览.md>)、[融合实施总清单](<../Morde iOS与nanoMuse融合设计实施任务todo清单文档.md>)及[设计稿](<../../Morde iOS设计稿.html>)。本文件是 **MIF-B01、MIF-01、MIF-02、MIF-11、MIF-12 的唯一状态来源**；总清单保留原始任务内容作为依据，后续状态以本文为准。

## 边界与交接

- 2026-11-04 用户明确指定当前会话为唯一集成者；原会话 3 已暂停对 `project/nanoMuse/android/src/ios/**` 的源码、测试和工程文件写入，并在其 iOS TODO 记录释放该源码树的写入权。当前会话核对原任务已 idle 后，开始 MIF-B01；其他组仍只写各自暂存文件，不能修改共享文件。
- 唯一集成者持有既有 `NanoMuse/NanoMuseShell.swift`、`NanoMuse/NanoMuseShellChrome.swift`、`Morde/MordeApp.swift`、`NanoMuse/NanoMuseRooms.swift`、`NanoMuse/NanoMuseSettings.swift`、`Localizable.xcstrings`、原 `Minis.xcodeproj` 及共享 fixture 和共享测试。其他组在交接、共享接口冻结后，只在各自本地独立暂存区准备各自的新视图及测试文件，不写这些共享文件；集成者按依赖顺序串行纳入和接线。
- `NanoMuse/` 是原工程同步编译目录，交接前不能把半成品 Swift 文件放进共享源码树。工作基线必须包含会话 3 的实际未提交和未跟踪工程内容；普通 Git worktree 不会自动包含这些内容，不以旧副本或独立 `Morde.xcodeproj` 替代原工程。不得覆盖或清理用户数据。
- 每个页面实施及验收都参考本地 `Morde iOS设计稿.html` 中对应页面、控件和交互脚本，按融合方案使用 nanoMuse iOS 风格，并记录实际处理与差异。好友和 AI 工具会话只保留“暂未实现”入口及占位页；任务、审批和项目工作项不展示预算或费用。
- 动态、点子、目标及目标页内例程入口按原清单 MI-08/MI-09 处理，普通例程独立设置按 MI-08 范围处理；保留既有数据和未确认的清理项。用户已授权聊天附件 API 并确认首期图片与 PDF；图片转 JPEG，PDF 通过 PDFKit 提取文本并生成页图，服务端只允许任务读取当前个人 Task 消息实际关联的文件。
- 仅本地工作；实施及验收使用进程内替身与合成元数据，不访问真实 Portal、远端设备，不上传、下载、分享或发送文件，不发送通知。

## 实施任务

- [x] **MIF-B01（已完成：共享 Task/审批、工作项、聊天抽屉和资源状态由同一 MordeClient 提供；审批服务端已提交但响应丢失时可刷新确认；Task 未知结果同键重试、在途防重复及同账号切换服务实例后幂等键隔离均通过本机验收）冻结并接入共享状态和页面接口**
  - 修改：由会话 3 本人或明确交接后的唯一集成者在共享边界内梳理并实现 `MordeClient` 的最小 Swift 接口，向任务/审批、资源/文件、占位页三组提供各自需要的账号作用域状态、真实对象 ID、能力/权限状态及按 ID 打开或操作的回调。聊天抽屉分开云端会话与本机只读历史的路由和回调，不把云端 ID 交给本机历史页；占位页不依赖好友/工具服务。
  - 修改：让会话加载、当前 Task SSE 和恢复只有一个生命周期所有者，页面切换不重复启动监听或重置聊天草稿；账号/服务实例变化时隔离状态和异步回调。保留任务/审批版本、幂等键和未知结果重试语义及事件游标失效后的快照恢复。资源视图通过房间接口接入，不嵌套第二个根级 `TabView`，不放宽 Host 权限、票据和同源边界。
  - 交接：在本文记录最终 Swift 输入、输出、目标文件及可调用时机；接口冻结后其他组才能按各自文件边界准备页面，随后由集成者串行纳入。不复制另一套 Morde 客户端，也不恢复被原 target 排除的旧本机 Agent/聊天执行路径。
  - 验收：在独立本机替身覆盖同一客户端生命周期、云端/旧历史分路、切换账号时迟到响应、重复请求、审批/Task 未知结果重试、SSE 断线和 `task.snapshot_required`；资源页面无嵌套根标签栏。

- [x] **MIF-01（已完成：聊天/任务/审批/资源四入口与 nanoMuse 外壳已接入；键盘显示时底栏隐藏、收起后恢复、任务页往返草稿、四入口本地化无障碍标签/选中态及关键底栏安全留白已通过本机 UI 验收）统一四个根入口和 nanoMuse 外壳**
  - 修改：由会话 3 本人或交接后的唯一集成者在 `NanoMuse/NanoMuseShell.swift`、必要的 `Morde/MordeApp.swift` 和本地化中接入聊天、任务、审批、资源四个目的地；复用 `NanoMuseBottomBar`、`NanoMuseTones` 和已编入原 target 的头像/页面组件。Morde 页面作为房间嵌入，不再嵌套独立根标签栏。
  - 徽标：审批图标只显示完整分页与详情读取后确认的待办数；账号或服务作用域变化、部分失败或结果未知时清除/隐藏计数，不把未知当作零。
  - 登录：首次登录只使用认证界面；认证成功后回到同一 nanoMuse 外壳，不将用户带入旧 Morde 独立根页或预算页。
  - 验收：原 App 的四个入口可切换和返回，选中态、审批提示、键盘/安全区及 VoiceOver 标签正确；任务页返回后不丢聊天草稿。已移除板块不出现在新抽屉，旧路由与数据按 MI-08/MI-09 处理。

- [ ] **MIF-02（进行中：Task 状态头部、云端主/旁聊、新建、分页搜索、本机历史单独路由、空态/失败重试和草稿/消息隔离已接入并通过本机 UI 验收；图片/PDF 的 picker→上传→消息附件 ID 往返、首次响应丢失后的幂等重试、选择取消、上传失败、16 项上限及干净模拟器 A→B 附件隔离 UI 验收已通过；附件预览仍待 MDA-12 接线与验收）融合聊天头部和抽屉**
  - 修改：在现有 `MordeConversationRoom` 使用 nanoMuse 头像、名称、消息气泡、输入胶囊和状态视觉；状态由当前 Morde Task 驱动。抽屉接入 Morde 主会话、分页最近会话、新建与标题搜索，保留本地历史只读入口、设置及智能体资料；搜索只声称已实际加载的范围。
  - 修改：附件入口继续走既有长按/菜单，不增加聊天页常驻按钮；允许图片与 PDF，图片在本机规范化为 JPEG，PDF 原件与提取文本、前 10 页 JPEG 一起加入消息（提取文本上限 48 KiB、文本解析最多 100 页），一条消息合计最多 16 个附件 ID（含 PDF 原件、文本和页图）。超限时明确提示，不静默截断。
  - 修改：服务端只向个人 Task 暴露 `morde_read_task_attachment`，读取前再次验证附件 ID 关联到当前 Task 的用户消息、当前账号授权与附件归属；项目 Task 不开放此工具。PDF 原件返回读取提示，Agent 读取其文本和页图附件；文本限 64 KiB，JPEG 限 512 KiB 且不超过 1280×720。Agent 每个 Task 最多发现最近 1,000 条消息中的 64 个附件；API 每条消息仍独立限制 16 个 ID。
  - 验收：本机替身覆盖主会话、旁聊、新建、搜索、返回与空/失败态；切换会话、切页和账号时不串消息、草稿或 Task，已受理 Task 不因关闭页面而取消。
  - 附件验收：用本机 fixture 与合成 JPEG/PDF 覆盖规范化、PDF 文本/页图、16 项限制、消息关联及服务端工具仅能读取当前 Task 附件，并覆盖工具列表最多返回 64 项；严禁使用用户文件或真实 Portal/Host。完整 picker→上传 API→消息发送替身往返作为单独未验项记录，不以解析/服务端单测替代。

- [x] **MIF-11（已完成：本机 UI 覆盖取消确认、在途防重复、202→停用、显式恢复、409 刷新及结果未知后的幂等重试）接入设置中的全局急停**
  - 修改：在 `NanoMuseSettings.swift` 和现有 `MordeClient` Portal Session 路径接入执行状态、停止全部与用户主动恢复执行；明确这是当前账号的全部云端任务及后续执行开关，确认前不提交。保存未确认请求的原 epoch/幂等键，按合同处理 202、409、403、断网与恢复。
  - 验收：本机替身覆盖取消确认、重复点击、202 停止中到 disabled、冲突刷新、结果未知重试和显式恢复；不将 202 或网络失败显示为已停止，不展示额度/预算字段。

- [ ] **MIF-12（进行中：225 个 MIF/Shell `AppLocalized` 字面键及本轮 7 个新增键覆盖 9 种语言；文件库单测 8/8 通过；原工程 generic iOS Debug 最新构建通过，generic Simulator x86_64 仍受本机 FFmpeg slice 限制；此前浅色九页面审计记录 16 项对比度失败、2 项接近失败和 4 项 Dynamic Type 部分支持；深色九页审计仍有 15 项对比度失败和 4 项 Dynamic Type 部分支持，最大字体复验 5 项对比度失败和 3 项 Dynamic Type 部分支持，VoiceOver 开启路径复验 15 项对比度失败、2 项接近失败和 3 项 Dynamic Type 部分支持；系统附件未给颜色测量值，截图显示 `morde` 胶囊和任务状态为高对比字色，仍不判定误报；未报告触控目标或文字裁切；Reduce Motion 开关已开启并覆盖九页导航，未逐转场视觉核验；VoiceOver 开启时九页可自动化访问，但焦点顺序和语音播报未人工验收；设计稿逐页视觉与完整流程回归仍待补；`Invalid frame dimension` 仍在工作项标题输入框点击期间出现且无业务栈；依赖 MIF-01～06、MIF-08～11）统一视觉、本地化与原工程验收**
  - 修改：补齐 `Localizable.xcstrings` 所有既有语言，按 iOS 16 兼容方式处理状态监听；对新页面落实浅色/深色、Dynamic Type、VoiceOver、减少动态效果、长标题及错误/空态。更新受影响的 `MinisTests/NanoMuseLogicTests.swift`、Morde 客户端测试和 `MinisUITests/MordeRegistrationUITests.swift` 或同目录测试。
  - 验收：在交接后的原 `Minis.xcodeproj` / `morde` scheme 完成 generic iOS Debug 构建和专用干净 Simulator 的本机替身验收，按“逐页参考设计稿”映射核对各场景，并走通聊天→任务→事件链→审批→资源及两个占位页；覆盖账号 A/退出/B、分页、权限变化、断线、未知状态、深链和数据保留。记录设计稿对照结果、实际结果与未验项，使用会话 5 交付的可复现 Simulator 依赖；不碰已有用户数据模拟器，不访问真实 Portal、发送文件或通知。原生页面在 Simulator 验收，涉及的本地 Web 页面另用内置浏览器检查。

## 急停替身和最终整合

`MordeClient.portalData` 使用客户端的 `urlSession`：DEBUG 构建在启动参数含本地 fixture 标记时使用配置了 `MordeAuthURLProtocol` 的临时 session，生产构建使用 `URLSession.shared`。MIF-11 的本机替身验收须只在 DEBUG 本地 fixture 模式运行，并通过替身请求计数/响应验证已拦截；任何失败或未确定拦截时停止该验收，不请求真实服务。停止与恢复仍使用当前账号的短期 Portal Session、`expected_epoch` 和稳定幂等键，202 只表示停止中。

各组交付后，唯一集成者依次纳入新文件、补齐共享本地化和原工程引用，在原 `Minis.xcodeproj` 的 `morde` scheme 串行完成构建与专用干净 Simulator 验收；不将其他组暂存目录直接加入共享编译目录。MIF-12 可以先验已就绪内容；MIF-07 的类别/认领实现与验收按 03 子清单记录为完成，最终整合走查仍核对其可达性。每项实现与相应验收均完成后才勾选。实际结果、未验证项及跨组接口变化追加在本文交接记录，并由集成者与各组记录保持一致。

## 交接记录

### 2026-11-04 本任务写入权交接

- 用户明确授权当前 Codex 会话作为本任务唯一集成者，接管 MIF-B01、MIF-01 及 MIF-06 的共享接口与串行接线；MIF-06 的独占新视图/测试仍只写在 `.staging/03/`，由本轮在接口冻结后实现并纳入原 target。MIF-03～05 以及 MIF-08～10 的页面组仍保持各自暂存文件边界；不得改动其文件，也不改变旧 MI 清单的整体状态。
- 原会话 3 已在 [会话 3 iOS TODO](<../nanoMuse与Morde合并五会话并行实施/03-iOS原工程实施任务todo清单文档.md>) 记录：暂停并释放 `project/nanoMuse/android/src/ios/**` 写入权；未提交源码、未跟踪的原 `Minis.xcodeproj`/共享文件以及既有 12 个删除状态均原样保留。本会话通过 Codex 任务状态核实原任务已 idle；交接没有重置、清理或提交工作树。
- MIF-B01 先提供跨文件 Task/approval 页面接口；02 组只消费冻结签名，页面状态与操作仍由现有 Morde 客户端和服务端授权/幂等处理负责。完成接口及本机自检后记录精确 Swift 签名并解除 02 组页面写入门槛。

### 2026-11-04 Task/审批页面接口冻结

- **冻结范围：**`MordeApp.swift` 保留文件私有 `MordeClient`；跨文件只暴露 `MIFTaskPageStore.shared`（`ObservableObject`），不扩大旧客户端和私有 DTO 的可见范围。
- **只读状态：**`isSignedIn: Bool`、`scopeKey: String?`、`tasks: [MordeTask]`、`details: [String: MordeTask]`、`timelineEvents: [String: [MordeTaskTimelineEvent]]`、`timelineErrors: [String: String]`、`expiredTimelines: Set<String>`、`loadingTimelines: Set<String>`、`emptyTimelines: Set<String>`、`actionErrors: [String: String]`。
- **调用签名：**`taskPage(status: String? = nil, cursor: String? = nil) async throws -> MordePage<MordeTask>`；`task(id: String) async throws -> MordeTask`；`actOnTask(taskID: String, action: MordeTaskPageAction) async -> Bool`；`respondToApproval(taskID: String, approvalID: String, version: Int, decision: String) async -> Bool`；`watchTimeline(taskID: String) async`。动作限于 `pause(expectedStatus:)`、`resume`、`cancel(expectedStatus:)`、`retryBatchItem(id:)`。
- **边界与自检：**事件页面只使用 `MordeTaskTimelineEvent.display`，原始 payload 私有；Task 动作和审批继续复用既有权限、版本、幂等键及结果未知刷新逻辑，页面操作不切换聊天。`xcodebuild -quiet -project project/nanoMuse/android/src/ios/Minis.xcodeproj -scheme morde -sdk iphonesimulator -configuration Debug -arch arm64 CODE_SIGNING_ALLOWED=NO build` 通过。Mock/UI 验收未完成，MIF-B01 总项继续未勾选。

### 2026-11-04 MIF-06 共享文件临时写入窗口

- 为避免并发覆盖，当前唯一集成者与 MIF-06 工作项执行会话确认：该会话暂时独占 `MordeApp.swift` 中工作项适配器和独立刷新路径的实现，完成后须记录精确签名、原工程编译结果并明确释放文件；暂不改其他共享 iOS 文件。
- 当前唯一集成者在释放前不写 `MordeApp.swift`，继续独占 `NanoMuseShell.swift`、`NanoMuseShellChrome.swift`、`NanoMuseSettings.swift`、`Localizable.xcstrings`、原工程引用及共享测试。03 仍只在 `.staging/03/` 写工作项页面与独立测试。
- 对照设计稿 `data-view="work"` 与 `wiSheet`，工作项入口进入任务页分段，允许真实项目工作项的浏览、创建、阻塞/恢复、关联 Task 和交付；预算金额、虚构分类/认领示例不进入新页面。当前合同缺少类别与认领，MIF-07 保持阻塞。

### 2026-11-04 四入口导航骨架

- 已将 nanoMuse 根导航扩为聊天、任务、审批、资源四项，沿用原浮动底栏；旧 `library` 路由映射到资源，移除入口的 `feed`、`ideas`、`goals` 仍解析为无效路由。导航逻辑测试和依赖旧 Library 内容的 UI 测试已改用资源入口。
- 该时点的页面交接状态：任务/审批为短期占位，资源显示现有本机 Library 内容；好友和 AI 工具会话页未接入。后续交接记录覆盖本记录中的状态。

### 2026-11-04 MIF-10 占位入口接入

- 按设计稿 `docs/Morde iOS设计稿.html` 的侧栏「好友」与「AI 工具会话」入口，将 04 组交付的静态占位页纳入原工程同步目录；nanoMuse 抽屉入口分别打开对应页面，显示“暂未实现”，不调用好友或工具会话服务。
- Settings 内的旧「Morde cloud tasks」入口改为切换到同一个外壳的任务页并关闭设置 sheet，不再打开嵌套 Morde 根视图。任务页交付后该入口沿用此路由。
- 当前提交接入两项未实现占位和设置路由；02 组任务/审批页已进入原工程并接到根入口。MordeApp.swift 写入权仍待 03 组明确释放，MIF-01/MIF-B01 未勾选。

### 2026-11-04 Task/审批页面接入 nanoMuse 根入口

- 02 组已将任务页、审批收件箱及各自单测复制到原工程同步编译目录。集成者把任务和审批分别接入根导航独立 `NavigationStack`，详情下钻留在各自栈内，页头使用现有 `NanoMuseRoomHeader` 和抽屉/智能体入口；审批确认数回传底栏。
- 对照设计稿 `data-view="tasks"`、`data-view="appr"`：保留真实任务/审批对象及操作语义，不带入演示记录、预算、测试计数或灰度动作；任务页以底栏入口进入，事件链由详情页下钻查看。
- `MIFTaskViews.swift`、`MIFApprovalViews.swift`、`MIFPlaceholderViews.swift` 的 73 个静态 `AppLocalized` 键现已覆盖九种既有语言；两个动态消息改成目录格式键。独立核对确认键与语言集合完整，目录 JSON 可解析、差异空白检查通过。翻译尚未做母语审校。
- 02 组的 Simulator Debug 构建成功。首次定向 UI 测试因 `CODE_SIGNING_ALLOWED=NO` 导致 app group 容器初始化断言，在测试断言前崩溃；当前尚无可用的 UI 测试结果。待唯一集成者完成共享接口后，使用本机签名 Simulator 重试。MIF-01/MIF-B01 继续未勾选。

### 2026-11-05 共享接口、设计稿映射与本机验收

- 本记录覆盖上文较早的“待接入/未验收”快照。`MordeClient` 仍是唯一会话、Task、审批与资源数据所有者；新增跨文件页面只读共享状态并通过方法/闭包操作，不创建第二套客户端。
- **冻结的页面接口：**
  - `MIFTaskPageStore.shared` 提供账号 `scopeKey`、当前 Task 和能力状态、任务/详情/事件只读集合；调用 `taskPage(status:cursor:)`、`task(id:)`、`actOnTask(taskID:action:)`、`respondToApproval(taskID:approvalID:version:decision:)`、`watchTimeline(taskID:)`。动作继续使用 `MIFTaskPageAction` 和 Morde 现有版本/幂等处理。
  - `MIFConversationDrawerStore.shared` 提供已加载的云端 `conversations`、当前/主会话 ID、下一页游标、加载及错误状态；`loadMore()`、`choose(_:)`、`create()`、`followMain()` 仅作用于 Morde 云端会话。本机历史仍由单独的只读历史入口打开。
  - `MIFResourcePageStore.shared` 提供账号作用域 `scopeKey`、`MIFResourcePageState` 与 Host 票据 URL；`loadIfNeeded()`、`refresh()`、`refreshControl(_:)`、`openHost(_:)`、`control(_:action:)` 保留在现有 Morde 权限和能力判断内。`MIFResourcePageActions` 向 `MIFResourceRoom` 传入 `refresh/openLibrary/openHost/refreshControl/control/openWorkspaceFiles` 回调；资源页通过 `.task(id: scopeKey)` 进入，不嵌套根级标签栏。
  - 好友与 AI 工具会话使用 `MIFPlaceholderPage(kind:)` 静态说明页；不读取服务端对象、不伪造数据，也不提交操作。
- **逐页对照 `docs/Morde iOS设计稿.html`：**`chat` 与抽屉沿用 nanoMuse 聊天外壳、头像状态、云端最近会话和搜索；`tasks/task/chain` 映射到任务列表、详情与事件链；`appr` 映射到审批收件箱；`work` 映射到任务页工作项分段；`res/pc/ws` 映射到资源页的电脑与 Workspace；`lib/file` 映射到资源页打开的文件库；`friends/toolchat` 映射到用户指定的未实现占位页。页面组合使用 nanoMuse 现有底栏、头像、色彩和房间标题，不复制 HTML 演示画布与静态示例数据；预算/费用示例按用户要求删除。
- **有意保留的差异：**文件库当前显示有权限的本地/账号/Task 产物元数据；现有 Task artifact DTO 不含可验证的逐文件预览票据，因此不开放预览、下载、分享或发送。好友和工具会话只有入口及占位文案。上述交互不是 HTML 演示脚本的逐像素复刻。
- **本机替身验证：**在原 `Minis.xcodeproj` 的 `morde` scheme、专用 Simulator 上运行，DEBUG 启动参数由进程内 `MordeAuthURLProtocol` 拦截，使用合成账号与数据；未请求真实 Portal，未上传、下载、分享或发送文件。`MIFTaskViewsTests`、`MIFResourceViewsTests`、`MIFLibraryViewsTests` 共 16 项通过。UI 用例通过：资源/文件库入口与返回、搜索空结果时继续翻页、急停确认与显式恢复、好友/AI 工具占位、账号 A/B 会话和 Task 隔离、SSE 临时断线重连及 `task.snapshot_required` 快照恢复；此前工作项创建/冲突/关联/交付用例也已通过。
- 原工程 arm64 Simulator Debug 构建通过；通用 Simulator 构建在 x86_64 链接阶段失败，原因是本机现有 FFmpeg XCFramework 未包含 x86_64 Simulator slice。该构建限制不影响已通过的 arm64 Simulator 构建和本地 UI 验收。
- **当时尚未验收（后续结果见本文后续补验记录）：**切换账号时人为延迟的迟到响应、急停取消/重复点击/409/结果未知重试已分别补验通过；键盘与聊天草稿保留、完整深色模式/Dynamic Type/VoiceOver/减少动态效果组合及整体验收仍待完成。对应总任务继续未勾选；不以本机当前的部分 UI 测试宣称整套设计稿验收完成。

### 2026-11-05 任务分页、审批和连续消息补验

- **任务/审批 UI：**在新建的本机干净 iPhone 17 Simulator（iOS 27.0）运行原工程 `morde` scheme 的 `testLocalTaskPagesAndApprovalInbox`，1/1 通过。覆盖任务详情、事件摘要不泄露预算字段、任务列表第二页加载、审批收件箱确认数、批准后读取最新详情并将确认数归零。第二页加载按钮和详情操作区已增加底部栏安全留白，避免控件落在 nanoMuse 固定底栏下面。
- **连续消息 UI：**同一 Simulator 上运行 `testSequentialTaskSendsStayInTheSameConversation`，1/1 通过。两次发送均通过仅接受 `c1234567` 的本机替身，第一条和第二条 prompt/reply 都在消息列表中可定位；测试滚动只使用可见消息作为锚点，避免把列表滑过目标消息。
- **本机夹具修正：**审批响应替身改用已有 `jsonPayload(_:)` 读取请求体，兼容 Simulator URLSession 将 JSON 放入 `httpBodyStream` 的行为；修正前夹具误报 409，修正后 202、详情刷新和收件箱归零流程均通过。整个验收由进程内 `MordeAuthURLProtocol` 拦截，未请求真实 Portal，也未传输文件。
- **运行记录：**两项 UI 用例各 1/1 通过；运行期间仍出现 `Publishing changes from within view updates is not allowed, this will cause undefined behavior.` SwiftUI 警告。其来源尚未定位，因此记录为 MIF-12 待办，不据此宣称整体验收完成。MIF-B01、MIF-01、MIF-02 和 MIF-12 保持未勾选，直到各自列出的剩余验收完成。

### 2026-11-05 账号切换迟到任务分页补验

- **本机替身：**DEBUG fixture 只延迟账号 A 的下一页 `GET /api/tasks` 12 秒；请求通过进程内 `MordeAuthURLProtocol` 处理，其他账号和真实服务不参与。页面组装首屏后启动下一页请求，再在同一 App 进程退出账号 A、登录账号 B。
- **UI 验收：**原 `morde` scheme 在专用 iPhone 17 Simulator 上运行 `testLateTaskPageFromPreviousAccountCannotReplaceCurrentTasks`，1/1 通过。测试等待账号 A 的分页请求返回后，确认 B 的任务仍在、A 的首屏和迟到页任务均不显示；结果证明了账号切换时客户端 scope 和页面请求代次共同丢弃旧账号响应。
- **构建修复：**验收构建发现 `MIFLibraryViews.swift` 的 `visibleEntries.map` 多语句闭包缺少显式 `return`，已补回并通过同次 scheme 编译。此为集成阻塞修复，不改变 Library 预览门禁。
- **剩余范围：**UI 启动仍报告 SwiftUI 在 view update 内发布状态警告；键盘草稿和完整辅助功能仍未覆盖。相关总任务继续保持未勾选。

### 2026-11-05 云端急停边界补验

- **本机替身：**扩展 DEBUG `MordeAuthURLProtocol` 的执行 fixture，分别模拟外部状态变化后的 409、停止请求已送达但响应丢失，以及 4 秒延迟的 202。未知结果重试只有沿用首次请求的 `Idempotency-Key` 与 epoch 才会成功；延迟响应仅由对应 stop 请求管理。所有请求由进程内 URLProtocol 接管，不访问真实 Portal。
- **UI 验收：**原 `morde` scheme 在专用 iPhone 17 Simulator 串行验证取消确认、正常停止并显式恢复、在途期间重复触碰、409 后刷新状态、断网后用相同幂等请求重试，5 项均通过。修改 fixture 清理范围后再次运行在途防重复用例，1/1 通过。取消场景按弹出层外区域关闭确认，并断言执行仍启用。
- **阻塞修复：**测试构建暴露 `MIFLibraryViews.localRow` 的预览按钮标识误引用未定义的 `artifact`；已改为当前本机文件的 UUID，并使用 `morde.library.local.preview.*` 标识。其后原工程构建及 UI 用例通过。
- **结果与遗留：**MIF-11 已勾选。UI 运行仍出现 `Publishing changes from within view updates is not allowed, this will cause undefined behavior.` 警告，继续由 MIF-12 追踪；这不阻止急停用例通过。MIF-B01、MIF-01、MIF-02 与 MIF-12 保持未完成。

### 2026-11-05 切换账号时聊天迟到响应补验

- **本机替身：**账号 A 的独立侧聊 `GET /api/conversations/c0000003/messages` 在 DEBUG `MordeAuthURLProtocol` 中延迟 25 秒；主会话正常加载，避免阻塞登录/退出流程。测试等聊天区显示“消息正在读取”后才切换账号；账号 B 的主会话由同一进程 fixture 返回。未访问真实 Portal，也未传输文件。
- **UI 验收：**原 `morde` scheme 在专用 iPhone 17 Simulator 上运行新增 `testLateConversationMessagesFromPreviousAccountCannotReplaceCurrentChat` 与既有 `testSwitchingAccountsDoesNotReuseConversationMessagesOrTasks`，2/2 通过。A 的侧聊响应晚于 B 主会话加载返回；B 消息仍在，A 的侧聊和主会话私有标记均未出现。
- **剩余范围：**上述两项账号隔离回归当时尚未覆盖键盘草稿；后续结果见下方“任务页往返草稿与导航状态补验”。完整辅助功能和其他共享生命周期边界仍未完成；MIF-B01、MIF-01、MIF-02、MIF-12 保持未完成。

### 2026-11-05 任务页往返草稿与导航状态补验

- **设计稿参考：**对照本地 `docs/Morde iOS设计稿.html` 的 `data-view="chat"` 聊天输入区与 `data-view="tasks"` 任务入口/返回流程；外壳采用 nanoMuse 底栏与现有视觉组件。本项验证导航状态，不代表逐页视觉验收。
- **UI 验收：**原 `morde` scheme 在干净 iPhone 17 Simulator 上运行 `testTaskNavigationPreservesChatDraftAndSelectedTabAccessibility` 与 `testTaskRootKeepsTaskAndWorkItemSectionsInsideTheNanoMuseShell`，2/2 通过。输入草稿并收起键盘后切到任务页再返回，草稿仍在；聊天、任务、审批、资源四个底栏入口的本地化无障碍标签可读，当前项的 selected 状态随切换更新。结果包为 `/tmp/MIFVoiceOverTabLabelsFreshSim.xcresult`。
- **剩余范围：**本轮用例覆盖该条路径的键盘收起与草稿保持、四入口无障碍标签和 selected 状态；完整键盘/安全区组合、VoiceOver 全页面标签、Dynamic Type、深浅色和减少动态效果仍待验收。首轮运行曾出现 SwiftUI view update 发布状态警告，后续定位与修复见下方记录。MIF-01、MIF-12 继续未完成。

### 2026-11-05 SwiftUI 更新警告定位与回归

- **定位和修复：**日志回溯分别指向 `SessionLockStore` 在 `UserDefaults.didChangeNotification` 回调中同步发送 `objectWillChange`，以及 Morde 错误弹窗关闭绑定在 SwiftUI 更新事务内同步清空 `client.error`。两处都改为当前事务结束后再通知/清理，保留原来的设置刷新与弹窗清除行为。
- **回归：**原 `morde` scheme 专用 iPhone 17 Simulator 重跑 `testTaskNavigationPreservesChatDraftAndSelectedTabAccessibility`，1/1 通过；错误弹窗仅在待清理的消息仍与关闭时相同时才清除。本次结果包与 Simulator 日志均未再出现 `Publishing changes from within view updates is not allowed`。结果包为 `/tmp/MIFSwiftUIWarningFixFinal.xcresult`。
- **剩余运行时提示：**该轮仍记录一条 `Invalid frame dimension (negative or non-finite)`，其堆栈没有定位到本工程符号，来源待查；不把这条警告视为已修复。Swift 6 隔离、旧桥接头 nullability 与本机 FFmpeg 搜索路径构建警告也仍存在。逐页视觉、完整辅助功能及整体验收未完成，MIF-12 保持未勾选。

### 2026-11-05 MIF-02 聊天界面对照设计稿与重验记录

- **设计稿依据：**直接对照本地 `docs/Morde iOS设计稿.html`：`data-view="chat"`、`.head.tall` / `.ava-stack`、`#chatList`、`#ask`，以及 `#drawer`、`#sideChats`、`data-view="toolchat"`。聊天正文现用可滚动消息气泡，Morde 用户/助手消息复用 nanoMuse 气泡色，输入改用 nanoMuse 输入胶囊；头像和状态仍由 nanoMuse 聊天头部承载。抽屉继续区分云端会话与本机只读历史，侧聊草稿按账号作用域和会话 ID 保存。用户确认保留附件、语音与 Task options，迁至不增加可见按钮的长按/菜单入口；附件端到端能力正在扩展服务端/API，媒体类型待确认。任务摘要仍链接真实 Task 详情。
- **范围差异：**好友和 AI 工具会话按用户要求只保留侧栏入口和“暂未实现”占位，不仿造设计稿里的联系人/工具聊天示例、不调用对应服务。设计稿中的预算示例不进入聊天任务摘要或任务、审批页面。此为按 nanoMuse 视觉与现有权限边界适配，不是对 HTML 样例的逐像素复刻。
- **编译：**原工程 `Minis.xcodeproj` / `morde` scheme 的 `build-for-testing` 在专用 iPhone 17 Simulator 上通过，包含本轮最后一次聊天滚动视图查询定位修改。
- **UI 重验：**首次融合回归 `/tmp/MIFNanoMuseChatFusion.xcresult` 共 3 项，任务页往返实际聊天草稿与选中态测试通过；另两项暴露输入法/断言定位问题。连续发送用例附件显示本地 fixture 已收到两条消息并返回两条助手回复，但中文键盘联想污染第二条输入文本；测试结束时键盘仍开，抽屉设置项因而不可见。草稿切换测试使用抽屉标题断言，未读取聊天消息标识。测试已改为英语键盘并在退出前收起键盘，草稿切换改读消息 ID，连续发送改滚动实际聊天 ScrollView。
- **复验结果：**独立新建的本机 iPhone 17 / iOS 27.0 Simulator（`Codex MIF Chat Recheck 2026-11-05`，`E78B2A46-DA33-45C2-BEAD-8E0ECBC8AE46`）运行 `testTaskNavigationPreservesChatDraftAndSelectedTabAccessibility`、`testCloudConversationDraftsStayWithTheirConversation`、`testSequentialTaskSendsStayInTheSameConversation`，3/3 通过，结果包 `/tmp/MIFNanoMuseChatFusionFresh.xcresult`。旧专用 Simulator 重启后 runner 仍在连接前被系统结束；新建的独立 Simulator 正常完成验收。运行时仍有 `Invalid frame dimension (negative or non-finite)` 警告，按 MIF-12 保留待查。
- **未验范围：**本轮 UI 用例只覆盖聊天草稿、侧聊切换、连续发送和四入口选中态；完整空/失败态、已受理 Task 离开聊天后继续、设计稿逐页视觉、键盘/安全区组合、VoiceOver、Dynamic Type 和减少动态效果仍待验收。MIF-01、MIF-02、MIF-B01、MIF-12 均保持未勾选。

### 2026-11-05 MIF-B01 未知结果与服务实例作用域闭环

- **Fixture 与作用域：**审批替身在首次响应丢失前先提交合成审批决定；同一幂等键重放返回已保存的成功结果。Task pause 替身覆盖在途延迟、已提交后响应丢失、同键重试和异键冲突，并暴露请求次数。新增 `MordeClientDataScopeTests`，覆盖服务实例变化后 scope/storage key 分区、最近会话隔离及旧实例迟到 401 不清除新授权。
- **UI 验收：**原 `morde` scheme 在本机 iPhone 17e / iOS 27.0 Simulator 上验证审批响应丢失后刷新为已完成、Task 未知结果同键重试、在途操作按钮全部禁用且只送达一次、同一账号退出并重新登录不同服务实例后使用新幂等键，分别通过（审批 1/1，Task 操作 2/2，实例切换 1/1）；数据作用域单测 3/3 通过。结果包：`/tmp/MIFB01ApprovalFresh.xcresult`、`/tmp/MIFB01TaskActionsFresh.xcresult`、`/tmp/MIFB01ServiceSwitchFresh6.xcresult`、`/tmp/MIFB01FinalClosure5.xcresult`。早期组合运行曾遇到 UI runner 启动被系统结束，随后各场景分开运行通过。
- **本机边界：**所有场景均由 Debug `MordeAuthURLProtocol` 和合成账号处理，没有访问用户提供的 Portal、主机或 API key，也没有传输文件。MIF-B01 已勾选；MIF-01、MIF-02 已完成。MIF-12 的逐页视觉、全页面辅助功能、完整页面回归及 generic iOS 构建仍待收尾。

### 2026-11-05 工作项键盘可达性与共享边界复验

- **设计稿对照：**回看本地 `docs/Morde iOS设计稿.html` 的 `data-view="work"` 工作项表单与操作流程。nanoMuse 底栏在键盘显示时按原设计隐藏；为避免新建/阻塞表单操作被键盘遮挡，在工作项标题、描述和阻塞原因输入时提供系统键盘“完成”按钮，沿用现有本地化文案和 nanoMuse 样式。
- **修正：**`MIFWorkItemRoom` 将键盘焦点绑定到三类文本输入，并提供可访问 ID `morde.workItems.keyboardDone`。UI 测试在键盘打开时确认“完成”控件存在，收起后再访问创建、阻塞和底栏动作。认证等待与测试侧栏访问也改为等待根外壳可用及抽屉动画完成，避免把模拟器遗留账号提示或过渡帧当作页面失败。
- **构建与验收：**原 `Minis.xcodeproj` / `morde` scheme 的 arm64 Simulator `build-for-testing` 通过。专用空白 iPhone 17 / iOS 27.0 Simulator 串行通过 `testWorkItemsFilterCreateClaimConflictLinkAndDeliveryLocally`（1/1；筛选、认领、创建、阻塞/恢复、关联 Task 与交付记录）；同一设备通过急停在途重复触碰、Task 未知结果同幂等键重试、同账号服务实例切换后生成新幂等键、已受理 Task 离开聊天后由 SSE 完成四项（4/4）。结果包：`/tmp/MIFGroup01FinalDerivedData/Logs/Test/Test-morde-2026.11.05_07-27-31-+0800.xcresult`、`/tmp/MIFGroup01FinalDerivedData/Logs/Test/Test-morde-2026.11.05_07-30-40-+0800.xcresult`。
- **构建扩展：**`xcodebuild -quiet build -project android/src/ios/Minis.xcodeproj -scheme morde -destination 'generic/platform=iOS' -configuration Debug CODE_SIGNING_ALLOWED=NO` 在原工程产出 `Debug-iphoneos/morde.app` 且退出码为 0。仍有既有桥接头 nullability、Swift 并发隔离、App Icon 未分配资源及本机 FFmpeg 搜索路径警告；此前 generic Simulator x86_64 构建仍因 FFmpeg XCFramework 缺少对应 slice 失败。
- **结果与遗留：**本轮五项 UI 验收均通过；UI 结果仍出现 `Invalid frame dimension (negative or non-finite)`，来源未定位，因此 MIF-12 继续未勾选。该轮没有修改或清理已有模拟器数据，所有服务请求仍由进程内 DEBUG fixture 处理。逐页视觉、全页面 VoiceOver/Dynamic Type/深浅色/减少动态效果验收及完整页面回归仍待完成。

### 2026-11-05 登录焦点与根标签栏复验

- **问题与修正：**全组回归的 16 项中有 14 项通过，两个失败项都在登录后等待 `morde.shell.tab.tasks`。界面快照已进入聊天、键盘已不显示，但根标签栏仍未出现。`MordeRoot` 现在为登录/注册表单字段显式管理焦点，并在提交时清除焦点，让系统键盘完成收起后再切换到已登录 shell。
- **复验结果：**在新建的本机 iPhone 17 / iOS 27.0 Simulator 上，针对原失败的 `testDrawerSearchCanContinueToTheNextPageWhenCurrentPageHasNoMatches` 与 `testShellAllChatsOpensLocalHistorySeparatelyFromCloudDrawer` 串行复跑，2/2 通过；结果包：`/tmp/MIFGroup01AuthFocusDerivedData/Logs/Test/Test-morde-2026.11.05_08-03-57-+0800.xcresult`。此前同组其余 14 项与本次两项分别已有通过记录；未将分批结果记作一次整组通过。
- **本机边界与未验项：**本轮使用干净新模拟器，所有请求由 Debug `MordeAuthURLProtocol` fixture 处理，没有访问真实 Portal、主机或 API key，也没有传输文件。MIF-12 继续未勾选：尚缺逐页视觉、完整 VoiceOver/Dynamic Type/深浅色/减少动态效果验收与一次整组回归；`Invalid frame dimension (negative or non-finite)` 仍在其他 UI 场景出现，来源未定位。

### 2026-11-05 MIF-12 辅助功能与 MIF-06 最终回归

- **修改：**根视图默认恢复系统 Dynamic Type；头像名称胶囊和页面标题可随大字号自然增长；文件库来源筛选换成可缩放菜单；审批徽标扩大并允许自然缩放；资源、审批链控件达到 44pt 触控尺寸。运行状态和审批链小号链接改用随外观变化的高对比文字色，MIF 页面次级说明改用动态高对比灰。Reduce Motion 关闭顶置提示卡滑入、名称状态动画和头像视频播放，保留静态脸图。
- **浅色审计：**`testMIFPagesPassSystemAccessibilityAudit` 逐项覆盖聊天、任务列表、详情、工作项、审批、资源、文件库、好友占位、工具会话占位。最终结果包 `/tmp/MIF12A11yFinalBadge.xcresult`：17 项 contrast failed、8 项 contrast nearly passed、4 项 Dynamic Type partially unsupported；没有触控目标或裁切问题。截图显示 `nanoMuse` 为黑字白底，系统也对数个原生“完成”按钮给出字号提示，无法从其通用错误文本判断测量值或修复对象。徽标放大前有 5 项 Dynamic Type partially unsupported，此次降为 4 项。
- **深色审计：**最终次级文字色更新后，在深色外观的专用 iPhone 17e / iOS 27.0 Simulator 重跑 `testMIFPagesPassSystemAccessibilityAudit`；结果包 `/tmp/MIF12A11yFinalDark2.xcresult` 为 19 项 contrast failed、2 项 nearly passed、4 项 Dynamic Type partially unsupported。附件仍只描述通用 `SwiftUI.AccessibilityNode`；可见截图中的 nanoMuse 胶囊与工具栏“完成”按钮为白字深底，文件库“不可用”为可缩放文本。系统未提供具体测量值或可定位对象，故记录为审计未通过并保留 MIF-12 未勾选，不宣称误报已获系统确认。未报告触控目标或裁切问题。
- **回归和构建：**文件库本地文件预览及空态 UI 测试 2/2 通过（`/tmp/MIF12LibraryMenuFix2.xcresult`）；原 `Minis.xcodeproj` / `morde` scheme 的 `testWorkItemsFilterCreateClaimConflictLinkAndDeliveryLocally` 通过 1/1（`/tmp/MIFWorkItemFinalRegression2.xcresult`）；最终 Dynamic Type 徽标改动后的 generic iOS Debug build 通过（`/tmp/MIFGenericIOSBuildFinal3`）。工作项回归结果仍记录 `Invalid frame dimension (negative or non-finite)`；XCTest 将它挂在点击工作项标题输入框的模拟事件上，没有业务源码栈。
- **保留未验项：**MIF-12 保持未勾选。完整 VoiceOver 逐控件操作、系统 Reduce Motion 开关下的运行时行为、整组页面回归及逐页设计稿视觉核对仍未完成。浏览器工具拒绝打开本地 `file://` 设计稿；已按本地 HTML 源码和 Simulator 检查，不把它记为浏览器视觉验收通过。全程使用进程内 fixture；未访问测试 Portal、主机或 API key，也未发送文件。

### 2026-11-05 MIF-12 Task 详情审批与澄清流程补验

- **设计稿对照：**重新检查本地 `docs/Morde iOS设计稿.html` 的 `data-view="task"`：保留返回/任务标题、任务元信息、经理事件链和针对当前任务的停止入口；原生详情继续使用 nanoMuse 的页面标题、色彩和卡片组件。设计稿的静态事件链样例不直接复制，页面从当前 Morde Task/事件读取数据。因为真实任务可能等待审批或澄清，详情额外呈现其服务端请求；普通审批提供批准/拒绝，媒体审批沿用受限预览后保存，澄清表单按请求 ID、版本及非空回答校验后提交。任务与详情不显示预算或费用。
- **实现和本地夹具：**任务详情复用 `MordeClient` 的请求、幂等和在途状态；审批只接受已知 approval 类型，媒体仍通过现有预览保护流程，澄清走 Task inputs 路径。新增的 UI 验收只用 DEBUG 进程内替身。首轮测试发现原分页夹具有 100 条任务、导致第二条详情不可达；为新用例增加独立的最小列表夹具，未改变原分页夹具或服务数据。
- **验收：**原 `Minis.xcodeproj` / `morde` scheme 在专用 iPhone 17 / iOS 27.0 Simulator 运行普通审批与澄清回答、媒体预览后保存两个用例，2/2 通过（`/tmp/MIFGroup01TaskDetailTestsFinal.xcresult`）；`MIFTaskViewsTests` 与 `MIFApprovalViewsTests` 共 13 项单测通过（`/tmp/MIFGroup01TaskDetailUnitTests.xcresult`）。所有验收请求均被进程内 fixture 接管，没有访问真实 Portal、主机或 API key，也没有传输文件。
- **遗留：**UI 用例仍出现 `Invalid frame dimension (negative or non-finite)`，未定位到来源。该补验只覆盖任务详情的审批、澄清与媒体保存路径，不代表 MIF-12 视觉、完整辅助功能或整组页面验收完成；MIF-12 继续未勾选。

### 2026-11-05 iOS 品牌文案统一

- 按用户要求，将 iOS 用户可见的品牌文案统一为 `morde`，覆盖页面标题、引导/设置说明、会话默认名称、Siri/快捷指令、文件提供器显示名、OpenRouter 标题和外发 User-Agent，以及九种语言的 `Localizable.xcstrings` 键和值。Cloud 展示名使用 `morde Cloud`；SOUL.md 中旧默认名只在内存和提示词展示时映射为 `morde`，不写回用户文件。
- 保留 GitHub 仓库地址、bundle/App Group 标识、URL scheme、通知名、存储键、旧 Cloud 模型分组查找名和 `X-NanoMuse-*` 请求头，以兼容旧安装与服务合同；`NanoMuse` 类型名和文件名仅作为内部源码标识保留；服务路由不变。
- 静态检查确认 `Localizable.xcstrings` JSON 可解析且不含旧品牌。原 `Minis.xcodeproj` / `morde` scheme 的 generic iOS Debug build 通过；仍有既有 Swift 并发/可空性与 FFmpeg 搜索路径警告。本次没有运行测试。MIF-12 的辅助功能审计、深色模式、VoiceOver、Reduce Motion、视觉走查和整组回归仍未完成，任务继续未勾选。

### 2026-11-05 MIF-12 品牌与本地化后复验

- **品牌与代码兼容：**干净模拟器审计截图中的聊天名称胶囊及资源页标题均显示 `morde`。`Localizable.xcstrings` 无旧品牌文案；`NanoMuse` 类型/文件名仅保留为内部源码标识；旧默认存储键、Cloud 模型分组查找名、bundle/URL scheme 和服务头按兼容用途保留。
- **本地化与 iOS 16：**旧聊天文件保留/预览页新增 7 个键，九种语言均有值；文件/产物预览 VoiceOver 名称含具体名称，单测 8/8 通过（`/tmp/MIF12AfterCatalogA11y.xcresult`）。审批标签的 VoiceOver 值改用 iOS 16 可用 API；首次 generic 构建发现 iOS 18 参数后已修正，最终原工程 generic iOS Debug 构建通过（`/tmp/MIF12FinalGenericBuild.log`）。
- **系统辅助功能审计：**新建隔离 iPhone 17e / iOS 27.0 Simulator（`F0B139B8-5665-4310-BB79-08D7EA39D34E`）运行 `testMIFPagesPassSystemAccessibilityAudit`，结果包 `/tmp/MIF12AfterToolbarA11y.xcresult`。九页覆盖中仍有 16 项 contrast failed、2 项 contrast nearly passed、4 项 Dynamic Type partially unsupported；定位到的主要标签为 `morde`、任务状态、“完成”和资源页状态文字。系统附件只返回泛化 `SwiftUI.AccessibilityNode`，未给对比度数值；截图里的黑字白底标题/状态无法据此判断为实际低对比。将占位页“完成”按钮改用系统默认本地化按钮后 finding 数量未下降，因此不将该审计记为通过。
- **遗留：**MIF-12 继续未勾选。全页面 VoiceOver 实际操作、Reduce Motion 系统开关、逐页对照 `Morde iOS设计稿.html` 的视觉验收、整组流程回归和 `Invalid frame dimension` 根因仍未闭环。全程使用新建本地 Simulator 与进程内 fixture，无真实服务访问或文件传输。

### 2026-11-05 MIF-12 品牌核对与失败 UI 用例复验

- **品牌核对：**本轮复查确认 App、分享扩展、Files 扩展、快捷指令、Cloud 页面及 AI 默认名称已显示为 `morde`；SOUL.md 中的旧默认名会在显示时映射为 `morde`，不回写用户文件。余下命中只在旧存储/协议标识、兼容迁移、内部类型名或注释中，保留以免破坏旧安装和服务合同，本轮无需改品牌源码。
- **回归：**原 `Minis.xcodeproj` / `morde` scheme 在新建专用 iPhone 17 / iOS 27.0 Simulator 上，失败项修正后 UI 回归 4/4 通过（`/tmp/MIF02TargetedUI20261105.xcresult`）；任务/审批/文件库/工作项/数据作用域单测 31/31 通过（`/tmp/MIF02MIFUnit20261105.xcresult`）。generic iOS Debug 使用 `CODE_SIGNING_ALLOWED=NO` 构建通过（`/tmp/MIF02GenericBuildNoSigning20261105.log`）。
- **遗留：**MIF-12 仍未勾选。系统对比度审计未定位的 finding、完整 VoiceOver 操作、Reduce Motion 系统开关、逐页对照设计稿、整组流程回归及 `Invalid frame dimension` 根因仍未闭环。验收使用新建本地 Simulator 和进程内 DEBUG fixture；未访问真实服务或传输文件。


### 2026-11-05 MIF-12 深色对比度复验

- **修改：**资源库来源菜单标签显式使用系统自适应主色，避免整页蓝色 tint 覆盖深色模式下的菜单文字。
- **验收：**专用深色 iPhone 17 / iOS 27.0 Simulator 运行 `testMIFPagesPassSystemAccessibilityAudit`，9 页均完成采集；Contrast findings 从 17 项降到 15 项，Dynamic Type findings 仍为 4 项（结果包 `/tmp/MIF12DarkAuditContrastFix20261105.xcresult`）。资源库“资源库 / 全部”两项不再触发；仍有 `morde`、任务状态、空态文字和系统工具栏“完成”项。附件未提供具体颜色值，截图中 `morde` 和任务状态为白字深底，因此不能将其余问题直接归因于实际低对比或认定为误报。
- **遗留：**MIF-12 继续未勾选。系统 Reduce Motion 开关、全页面 VoiceOver 操作、逐页设计稿视觉核对、整组流程回归以及 `Invalid frame dimension` 根因仍未完成；审计仍未通过。全程仅用本地专用 Simulator 和进程内 DEBUG fixture，未访问真实服务或传输文件。

### 2026-11-05 MIF-12 最大字体审计复验

- **测试启动修正：**首次在新建干净模拟器运行时，测试辅助函数只检查抽屉按钮，误把未登录状态当作已登录并尝试登出；在共享 `signOutFromSettings` helper 中增加登录页字段判断，未登录时直接返回，避免影响其他调用该 helper 的用例。
- **验收：**专用 iPhone 17 / iOS 27.0 Simulator（`20A835E1-226E-48FC-BBFA-0AE9C1EE5318`，最大辅助功能字体，浅色外观）重新执行 `testMIFPagesPassSystemAccessibilityAudit`，覆盖 9 个页面后结束。结果包 `/tmp/MIF12MaxDynamicTypeCleanAuditRetry20261105.xcresult`：5 项 Contrast failed（“完成”按钮 2 项、“运行中”状态、“此对话暂无消息。”2 项）和 3 项 Dynamic Type partially unsupported（占位页/资源库工具栏的“完成”）；系统附件仍只给泛化节点，没有提供颜色测量值。运行同时出现 `Invalid frame dimension (negative or non-finite)`。
- **结论与遗留：**该轮审计仍未通过，不把缺少颜色证据的对比度问题归类为真阳性或误报；MIF-12 保持未勾选。全页面 VoiceOver、系统 Reduce Motion 开关、逐页对照设计稿的视觉验收、整组流程回归和 frame warning 根因仍待完成。测试后已将该专用模拟器字体恢复为 `large`；整个过程只用本地 DEBUG fixture，未访问真实服务或传输文件。

### 2026-11-05 MIF-12 Reduce Motion 运行复验

- **验收：**在另建的干净 iPhone 17 / iOS 27.0 Simulator（`8A46B49B-42E7-4A5D-BF3E-4364EE134842`）启用系统 `ReduceMotionEnabled=1`，并使用 `testMIFPagesPassSystemAccessibilityAudit` 完成 9 页导航。结果包 `/tmp/MIF12ReduceMotionFreshAudit20261105.xcresult`；辅助功能审计仍报 15 项 Contrast failed、4 项 Dynamic Type partially unsupported 和 2 项 Contrast nearly passed，但本轮没有 runtime warning。代码中 `NanoMuseShell` 和 `NanoMuseShellChrome` 根据 `accessibilityReduceMotion` 关闭动画并改用淡入淡出过渡；此次模拟器导航覆盖了该系统偏好开启时的路径。
- **结论与遗留：**Reduce Motion 已完成开关与导航路径复验，但动态效果仍未做逐转场的视觉比对；VoiceOver 实际操作、设计稿逐页视觉核对和整组流程回归仍待收尾。两台专用测试模拟器的字体已恢复为 `large`、Reduce Motion 已恢复关闭，并均已关机。MIF-12 不变，继续未勾选。

### 2026-11-05 MIF-12 VoiceOver 开启路径复验

- **验收：**在专用 iPhone 17 / iOS 27.0 Simulator（`A47429BA-C63F-42D8-B323-496E41FC1FE2`）启用 `VoiceOverTouchEnabled` 与 `AccessibilityEnabled`，给 `testMIFPagesPassSystemAccessibilityAudit` 增加 VoiceOver 专用的登录字段双击输入路径后，9 页导航和系统辅助功能采集均完成。结果包 `/tmp/MIF12VoiceOverEnabledAuditRetry20261105.xcresult`：15 项 Contrast failed、2 项 Contrast nearly passed、3 项 Dynamic Type partially unsupported，无 runtime warning；只有系统辅助功能 finding 使测试结果为 Failed。普通模式登录仍走原单击路径。
- **限制与恢复：**测试确认开启 VoiceOver 时九页可被 UI 自动化访问并输出辅助功能树，但没有验证 VoiceOver 焦点顺序和实际语音播报。宿主没有安装 Simulator.app（本机 GUI 入口不可用），所以无法进行手动读屏；保留为未验项。测试后将 VoiceOver、系统辅助功能和 Dynamic Type 恢复默认并关机；只使用本地 fixture，没有访问真实服务或传输文件。
- **结论：**MIF-12 仍未勾选；人工 VoiceOver 操作、设计稿逐页视觉核对、完整流程回归和未定位的 `Invalid frame dimension` warning 仍待完成。

### 2026-11-05 MIF-12 设计稿源码对照与复验状态

- **源码级页面映射：**对照本地 `docs/Morde iOS设计稿.html` 的 chat/task/chain/appr/res/work/friends/lib/file/toolchat/settings/workspace/pc 页面。当前聊天进入 nanoMuse 聊天房间；任务详情承载事件链并保留审批跳转；工作项在任务页分段；资源页提供 Host/Workspace，文件库从资源页进入；好友和 AI 工具会话进入用户确认的“暂未实现”占位页；页面视觉沿用 nanoMuse 组件，预算/费用不显示。AI 工具运行状态卡片因 Morde iOS/API 没有全局运行会话合同而不伪造，依据见 04 组 MIF-08 记录。
- **确认的结构差异：**HTML 侧栏把事件链、资源、文件库和工作项作为独立入口；融合版沿用四个 nanoMuse 根标签，把事件链放在任务详情、工作项放在任务页、文件库放在资源页。HTML 审批卡的灰度动作和当前批准/拒绝合同不同；HTML 文件库的三类横向筛选与当前四类菜单筛选不同；HTML 资源页的云手机项未纳入 MIF-08 的 Host/Workspace 接口范围。聊天附件详情不在本组 MIF/Shell 源码范围内，未据此断言整个 App 缺少该能力。以上是可从源码和实施合同确认的范围差异，不等同于截图逐像素验收。
- **本轮构建：**修正 `NanoMuseAgentPage.paneSwitch` 中数组字面量内的 Swift 条件编译写法后，原 `Minis.xcodeproj` / `morde` scheme generic iOS Debug build 成功（`/tmp/MIF12MordeBuildAfterPaneSwitch20261105.log`）。
- **Simulator 回归阻塞：**全新 DerivedData 的签名 Simulator 测试构建在 `Views/Settings/LogManagementView.swift:149` 因当前代码引用未定义的 `SyncLogView` 失败，未进入本轮 smoke 测试；本轮未修改该日志页面。此前尝试运行整组 UI 时，MIFWorkItem 登录 helper 在登出转场期间只判断用户名字段存在，XCUITest 得到 `{-1,-1}` hit point 后输入失败。已将 MIFWorkItem helper 改为等待字段 `exists && hittable`，但受上述独立编译错误影响尚未运行验证。
- **保留未验项：**MIF-12 继续未勾选。HTML 到原生页面的普通字号截图对照、Host/Workspace 打开路径的本地运行画面、整组回归、人工 VoiceOver 焦点/播报以及 `Invalid frame dimension` 根因均未闭环；不得把源码映射或 generic build 记作视觉/整体验收通过。

### 2026-11-05 聊天图片与 PDF 附件实现及本地验证

- **实现：**聊天长按/菜单选择图片或 PDF；iOS 将图片规范化为有尺寸和体积上限的 JPEG。PDFKit 最多解析 100 页、提取文本最多 48 KiB，并为前 10 页生成 JPEG；原件、文本和页图以个人附件形式上传，关联到用户 Task 消息。每条消息最多 16 个附件 ID，超限明确报错；附件单独发送时使用默认读取提示。解析副本不在聊天气泡重复显示。
- **服务端/API：**任务消息新增 `attachment_ids`，幂等摘要纳入附件 ID，消息关联与响应重放读取均在数据库事务/存储层完成。仅个人 Task 动态提供 `morde_read_task_attachment`，每次读取重新校验账号授权、Task 消息关联、附件归属和库记录；项目 Task 不提供该工具。工具检查最近 1,000 条消息，最多列出最新 64 个附件；PDF 原件引导读取文本/页图，JPEG 校验大小和尺寸后作为模型图像输入，文本内容有输出上限。此 64 项是每个活动 Task 的读取发现上限，不改变 API 的每消息 16 项限制。
- **本机验证：**原 `Minis.xcodeproj` / `morde` scheme 在专用 iPhone 17e / iOS 27.0 Simulator 上运行 `MordeChatAttachmentPreparationTests`，合成图片与 PDF 共 2/2 通过（`/Users/zhongxuan_li/Library/Developer/Xcode/DerivedData/Minis-fhjtbwadoilgxmdetwjkeyewpbxo/Logs/Test/Test-morde-2026.11.05_20-32-42-+0800.xcresult`）。`go test ./internal/app ./internal/api ./internal/worker ./cmd/portal ./internal/store/postgres` 全部通过；服务端附件读取测试包含当前 Task 授权边界与 64 项上限。此前 generic iOS arm64 Debug build 通过。
- **未验收与状态：**完整文件选择器→上传 API→消息发送替身往返、真实服务端互操作及真实图片/PDF 使用尚未验证；本轮没有使用、上传或发送用户文件，也没有访问 Portal/Host。MIF-02 保持未勾选，不能以解析单测和服务端单测代替完整本机往返及聊天流程验收。
