# 资源、文件与占位页实施 TODO

先读[并行执行入口](00-并行执行入口及任务总览.md)、[融合设计总清单](<../Morde iOS与nanoMuse融合设计实施任务todo清单文档.md>)和[原 iOS 会话清单](<../nanoMuse与Morde合并五会话并行实施/03-iOS原工程实施任务todo清单文档.md>)。本文件是 **MIF-08、MIF-09、MIF-10 的唯一状态来源**；任务状态、验收记录和交接结果在此维护。下列三项任务原文和状态沿用融合设计总清单。

## 执行与写入边界

- 会话 3 已完成交接，01 已完成 MIF-B01 资源适配和 MIF-01 根路由集成；当前继续由本机集成者串行维护共享 iOS 源码。MIF-08、MIF-09、MIF-10 已接入原 `Minis.xcodeproj` 的 `morde` scheme；原计划中的 `.staging/04/` 文件交接不再是待完成项，历史交接记录保留供追溯。
- 本轮改动范围限于 `MordeApp.swift` 的 DEBUG 本地 URLProtocol fixture、资源/Library 页面、对应 `MinisTests`/`MinisUITests` 及本 TODO；不改服务端、API 合同、工程配置、本地化目录或用户已有的其他修改。

- `MordeClient`、`MordeWorkspaceFilesView` 和 Host 查看器沿用现有私有实现及冻结能力入口，不另建客户端或复制传输逻辑。`MordeComputerRoom` 原三项 `TabView` 不嵌入新的四栏导航；Resources 使用独立四栏展示和现有查看器/Workspace 文件页。
- 保留 Host 短期票据、HTTPS 同源限制、账号和权限范围、只读共享及控制租约规则；`workspace_sync` 或 `task_media_generation` 未就绪时不开放相应能力。Task 输入附件保持关闭，不能将 personal-data 附件或历史 `attachment_ids` 充当 Task 输入。
- 好友和 AI 工具会话只显示“暂未实现”占位，不发送请求、不构造示例记录或成功反馈。仅使用本地替身和合成元数据验证，不传输、上传、下载或分享文件，不连接真实 Host，不发送通知。
- 每个页面实施及验收时，对照本地 `docs/Morde iOS设计稿.html` 的 `data-view="res"`、`ws`、`pc`、`lib`、`file`、`friends` 和 `toolchat` 场景及其交互；沿用 nanoMuse 视觉，记录用户已确认的占位和能力限制。

## 实施任务

- [x] **MIF-08（已完成：资源页、权限/租约策略、失败反馈和未就绪绑定提示已接入；本地状态矩阵 UI 与 Host 同源票据边界检查通过）整合资源与云端电脑**
  - 修改：在资源页组合自有/共享 Host、Workspace、控制状态及文件入口；复用 `MordeComputerRoom`、`MordeWorkspaceFilesView` 的权限和状态逻辑。接管、交还和续租以控制人、租约与服务端状态为准；默认电脑/Workspace 尚未接通时显示未设置，不自动改变已受理任务的目标。
  - 验收：本机替身覆盖运行/离线、只读共享、绑定不就绪、控制状态未知、租约过期、接管/交还失败及返回；查看器保持短期票据和 HTTPS 同源限制，不注入本机 JavaScript 桥，也不连接真实电脑。

- [x] **MIF-09（已完成：Task proof 解码、权限/能力门控、详情数据合并、本机校验和预览、空态及旧产物仅元数据均已接入并通过本地验收）接入文件库和产物展示**
  - 修改：复用 `NanoMuseRooms.swift` 的本地/个人文件列表及 Morde Workspace/Task `artifacts` 元数据，区分来源和可用性；仅对有权限且能力就绪的记录提供现有预览入口。Task 输入附件保持关闭；生成媒体预览/保存继续沿用 MI-03 的 `task_media_generation` 条件和确认流程。
  - 验收：用本机合成元数据检查分类、空态、无权限、无证明的旧产物、能力关闭和本地预览入口；不上传、下载、分享或发送文件，不把历史 `attachment_ids` 当成 Task 输入授权。

- [x] **MIF-10（已完成：两个抽屉入口均可打开/返回；Simulator 检查占位页无障碍合并标签含页面标题和“Not implemented yet”）添加好友和 AI 工具会话占位入口**
  - 修改：侧边抽屉接入两个 nanoMuse 风格的占位页，明确显示“暂未实现”；不读取或构造好友列表、工具状态与对话记录，不实现添加、审批、分享、撤销或打开工具对话动作。
  - 验收：本机点开两个入口能返回，页面有无障碍名称及未实现说明；不发起相关请求，不出现虚假数据或成功提示。

## 交接与完成记录

按任务编号记录交接、接口版本、设计稿差异及实际验收结果。只有原 `morde` scheme 的实现和相应验收都完成后，才将任务勾选为 `[x]`；未就绪或阻塞时写明具体缺项。

### 已冻结并纳入原工程的本组接口

- **MIF-08：**提供可嵌入四栏资源页的 Host/Workspace 房间入口，不嵌套原 `MordeComputerRoom` 三项底栏。以真实 ID 操作，按账号 scope 提供自有/共享 Host、Workspace 绑定、权限、加载状态、控制租约/转换状态及动作错误；共享 Host 保持只读。控制动作和短期票据 Web 查看器留在既有客户端路径内，页面不接触票据本身。
- **MIF-09：**提供按来源分类的本机 Library 元数据与 Task 产物摘要；Task 产物需连同 Workspace ID、revision、size、SHA-256 和 `verified` 证明状态供页面判断。无完整证明或权限/能力未就绪时只显示元数据；可预览入口只指向已验证的本机文件，不提供上传、下载、分享或 Task 输入附件动作。媒体审批仍沿用 `task_media_generation` 门禁。
- **MIF-10：**只需接入两个无数据依赖的固定路由，由根壳提供可访问名称和返回动作；页面只显示好友、AI 工具会话的标题与“暂未实现”，不需要 Morde Client、好友/工具状态或示例记录。

#### 占位页入口

好友与 AI 工具会话占位由 `MIFPlaceholderPage(kind: .friends/.toolChats)` 提供。页面没有数据、客户端、网络或好友/工具动作回调；根壳提供标题、关闭/返回和合并无障碍标签。

#### 资源展示组件合同

`MIFResourceRoom(chrome:state:actions:)` 消费 `MIFResourcePageState` 值快照与真实 ID 回调；`MIFResourcePageStore` 在根层复用私有 `MordeClient` 状态、scope、权限、控制租约和文件/Host 入口。视图不创建客户端、不处理票据；共享 Host 只读。Host 查看器和控制请求继续留在既有客户端路径。

- **设计稿差异：**`data-view="res"` 有 Codex/Claude 运行状态卡片，但当前 Morde iOS/API 没有全局 AI 工具进程/编码会话清单。`/api/mcp/connectors` 仅是个人连接器配置元数据，不等同工具运行状态，且 Morde iOS 当前不加载该资源；`NanoMuseHub`/`NanoMuseCodingView` 的 `coding.sessions` 属旧 nanoMuse Hub 路径，Morde target 保持 fail-closed。本页不复用旧 Hub、不新增网络请求，也不显示演示 Codex/Claude 卡片。
- **本地化键：**Resources literal keys 已由 01 纳入 `Localizable.xcstrings`；`Lease expires %@` 是格式化目录键。

  ```text
  A ready Workspace binding is required to open this computer.
  AI controls this computer
  Browse Workspace files
  Cloud computers
  Control handoff in progress
  Control lease active
  Control lease expired
  Control status unknown
  Control status unknown; control actions are disabled.
  Default execution target
  Default target details are unavailable.
  Default target status is unknown.
  Lease expires %@
  Lease status unknown
  Loading resources…
  No Workspaces are available in this account scope.
  No active control lease
  No cloud computers are available in this account scope.
  No default computer or Workspace is set.
  Not mounted
  Offline
  Open computer
  Read-only
  Ready
  Refresh control status
  Refresh resources
  Renew lease
  Resources
  Return control to AI
  Returning control to AI
  Running
  Shared read-only
  Starting
  Status unknown
  Stopped
  Syncing
  Take control
  Unavailable
  Updating control status
  Workspaces
  You control this computer
  You do not have permission to view this computer.
  ```

### 2026-11-04 只读启动审计

- 会话 3 的原工程任务仍在运行并持有 `project/nanoMuse/android/src/ios/**` 写入权；尚未找到针对本组的明确交接。01 清单中的 MIF-B01 仍为未开始，没有冻结的 Swift 签名、可调用时机或接口版本记录。
- 因此 MIF-08、MIF-10 保持未开始；MIF-09 还等待 MIF-02、MIF-03、MIF-08。MIF-08 所需 Host/Workspace 类型、查看器、文件页及控制动作目前是 `MordeApp.swift` 私有实现，`MordeComputerRoom` 仍含三项 `TabView`；MIF-09 的 Task 模型没有冻结 `artifacts` 接口，旧 Library 还带上传、下载、分享动作；MIF-10 尚无好友和工具会话入口。页面代码和路由都需等 01 确认接口与接线。
- 安全验收约束已核对：共享 Host 只读、控制未知/租约过期时禁用控制动作；查看器限制短期 HTTPS 同源票据和同源导航；Library 只显示合成元数据及权限允许的本机预览，不触发文件传输；未经 Workspace/revision/size/SHA-256/verified 证明的旧 Task 产物不提供预览；媒体预览/保存继续由 `task_media_generation` 能力和 MI-03 审批门禁控制；占位页不读取任何好友/工具数据、不发送请求。
- 当前仅完成设计稿、状态入口与三个页面边界的只读核对，没有修改 iOS 源码、测试或工程文件，也未运行构建/Simulator 验收。

### 2026-11-04 依赖状态复核

- 01 清单现将 MIF-B01 标为进行中；02 组清单记录该组获授权作为唯一集成者推进 MIF-B01 与 MIF-03。尚未找到可编译的共享 Swift 页面签名、资源能力接口或接口冻结记录；MIF-01/02 仍未完成。
- 当前 `MordeApp.swift` 已出现 Task timeline/page-action 状态和 `MordeTask.artifacts` 摘要字段（`id`、`label`、`media_type`、`verified`、`created_at`），但仍在未提交的共享文件中；尚无可供本组使用的资源/Library adapter，Task 产物也没有本任务要求的完整证明接口冻结记录。
- 会话 3 的原工程任务仍为 active，`MordeApp.swift`、`NanoMuseShell.swift`、`NanoMuseRooms.swift` 等共享文件仍有未提交变更。02 组的接管授权没有给本组分配共享文件写入权；本组继续不写 Swift、测试或工程文件，等 01 公布冻结接口后再开始各自 `.staging/04/` 文件。
- 后续在会话 3 原清单的“源码写入权交接”记录中确认：会话 3 已停止共享 iOS 源码、测试和工程文件写入，并释放源码树写入权；这解除原所有者冲突，但不等于 MIF-B01 接口冻结，也未完成 MIF-01/02/03/08 的页面依赖。
- 02 集成线程对首轮 MIF-B01 修改执行的原 `morde` Simulator 编译失败（exit 65）：`MordeApp.swift` 有跨文件方法暴露私有类型的 Swift 错误，以及 Task 时间线状态标签调用签名不匹配。该线程仍 active 并继续收敛合同；当前尚无可编译冻结版本，本组页面保持未开始。
- 02 集成线程最新计划先收敛 Task/审批状态适配器；本组仍未收到 Host/Workspace、Library 预览或好友/工具占位所需的冻结签名。Task-only 接口通过也不能单独解除 MIF-08～10 的页面写入门槛。
- 后续依赖复核：01 清单现记录 Task/审批的 `MIFTaskPageStore` 合同冻结，原 `morde` target 的 arm64 Simulator Debug 构建通过；首轮编译错误已在后续收敛中解决。01 清单仍将 MIF-B01 整体标为进行中、MIF-01 标为进行中，且冻结记录未提供资源/Library/占位接口。待 01 完成根入口并记录本组可消费的资源合同后，再启动 MIF-08～10 暂存实现。

### 2026-11-04 MIF-10 独立占位入口冻结

- 01 集成者已冻结 `MIFPlaceholderPage(kind: .friends/.toolChats)` 作为两个无服务依赖占位页面接口。仅 MIF-10 可先在 `.staging/04/placeholder/` 准备其专属页面与测试；不得写入根壳、共享客户端、工程、本地化或其他暂存目录。
- 设计稿 `data-view="friends"` 与 `data-view="toolchat"` 的示例好友、工具状态和对话均是演示内容；实现仅呈现明确的“暂未实现”说明与 nanoMuse 样式占位，不复刻虚构样例，也不触发网络请求。根壳接线与本机返回/无障碍验收待 MIF-01。

### 2026-11-04 MIF-10 暂存实施开始

- 按已冻结的 `MIFPlaceholderPage(kind: .friends/.toolChats)` 合同启动占位页与独立测试，仅写 `.staging/04/placeholder/` 两个专属文件；本次委派不包含根壳、工程或本地化改动。MIF-08/09 仍等待 Host/Workspace 与 Library 合同及前置页面交接。

### 2026-11-04 MIF-10 页面暂存交付

- **暂存文件：**`.staging/04/placeholder/MIFPlaceholderViews.swift` 提供 `.friends` 与 `.toolChats` 静态页，复用 `NanoMuseEmptyState` 并给整页设置无障碍标识；`.staging/04/placeholder/MIFPlaceholderViewsTests.swift` 检查两种本地化标题和唯一标识。未添加导航、数据、客户端、网络或动作，也未复刻设计稿的样例好友和对话。
- **验证：**两份 Swift 文件在修正普通 `String` 传给 `String.LocalizationValue` 的类型问题后，`xcrun swiftc -parse` 通过；同时确认生产文件无网络、客户端、数据或按钮调用。XCTest 未运行，因为暂存文件未进入 `MinisTests` target。01 集成需要加入本地化键 `Friends`、`AI tool conversations`、`Not implemented yet`，并接入抽屉固定路由与返回。
- **状态：**MIF-10 保持未勾选，待 01 接入原 `Minis.xcodeproj` 的 `morde` scheme 后运行独立测试及本机返回/VoiceOver 验收。暂存文件在被 `.gitignore` 忽略的 `docs/` 中，仅作本机交接使用。

### 2026-11-04 根壳接线进度复核

- 共享 `NanoMuseShell.swift` 当前已加入 Chat/Tasks/Approvals/Resources 四个根标签；Tasks/Approvals 内容仍是通用临时占位，Resources 仍显示旧 `NanoMuseLibraryRoom`。Morde 电脑房间仍通过抽屉 sheet 打开，并保留原三项 `TabView`。
- 因此 MIF-01 尚未交付可验收的四个页面；MIF-10 仍待抽屉路由/返回接线，MIF-08 仍待 01 提供不嵌套底栏的资源房间接口。以上共享文件由 01 持有，本组不直接修改。

### 2026-11-04 MIF-10 原工程接入复核

- 01 集成者已将 `MIFPlaceholderViews.swift` 放入原工程 `NanoMuse/` 同步目录，将 `MIFPlaceholderViewsTests.swift` 放入 `MinisTests/`，并在 `NanoMuseShell.swift` 添加好友/AI 工具会话抽屉入口及 `NanoMusePageSheet` 返回路由；`Localizable.xcstrings` 已加入 `Friends`、`AI tool conversations`、`Not implemented yet` 键。
- 初次 UI runner 使用 `CODE_SIGNING_ALLOWED=NO`，在启动断言阶段因应用组容器不可用而退出，未进入测试断言；02 集成线程已暂停并交回共享写入权，01 集成者正在用保留 Simulator 签名的配置重跑。该失败不证明页面回归通过或失败，MIF-10 继续保持待验证。
- `MIFPlaceholderViewsTests` 仅检查两个静态标题和标识；当前 `MinisUITests` 尚未出现占位页路由断言。原工程 UI 验收仍需分别打开两个抽屉入口、核对“暂未实现”可访问文本，并通过 Done 返回外壳。

### 2026-11-04 共享集成权交回与 MIF-09 依赖复核

- 02 组已将 MIF-03/04/05 页面及单测纳入原工程后按 01 集成者要求暂停；其 arm64 Simulator Debug 构建通过，但首次运行型 UI 验收因禁用签名导致应用组容器不可用，在测试断言前退出。01 集成者接管共享写入与重新验收；本组不并发修改工程或启动 Simulator。
- 当前 `MordeTask.Artifact` 仍只有 `id`、`label`、`media_type`、`verified` 和 `created_at`，缺少 Workspace ID、revision、size、SHA-256 证明。MIF-09 在证明字段和权限/能力接口冻结前只规划元数据展示，不开放预览；MIF-08 仍等待不嵌套三栏的资源房间合同。

### 2026-11-04 MIF-08 共享接口复核

- `MordeComputerRoom` 目前只有无参公开初始化，并在内部维持电脑、Workspace、文件三项 `TabView`；`MordeResourceRoomSection` 只含导航类型，没有数据和操作能力。`MordeClient`、Host/SharedHost/Workspace/租约 DTO、文件视图及 Host 查看器仍为文件私有，没有 `MIFResourcePageStore` 或等价接口。
- MIF-08 继续未开始：01 需先冻结账号作用域、Host/Workspace 权限与加载/租约状态及按 ID 操作回调，同时把票据/查看器留在现有客户端路径。当前 `MordeApp.swift` 还处于 MIF-06 临时独占窗口；本组不跨界写入。

### 2026-11-04 MIF-09 共享接口复核

- 依赖仍未满足：MIF-02 未开始；MIF-03 页面已接入但还在验收；MIF-08 未开始；原 MI-04 仍进行中。`MIFTaskPageStore` 只冻结 Task/审批读取与动作，不提供 Library/资源接口。
- 每项 Task Artifact 当前只有 `id`、`label`、`media_type`、`verified`、`created_at`；Task 顶层 `workspace_id`/`input_revision` 不能代替产物级 Workspace ID、revision、size、SHA-256 证明。本机 `localWorkspaceFileURL`、`verifyLocalWorkspaceFile`、文件视图及 Quick Look 路径仍是 `MordeClient` 私有实现，没有安全的跨页预览回调。
- 因此 MIF-09 保持未开始；证明、权限、能力或本地校验接口缺一项时只显示产物元数据，不提供预览，也不复用带上传/下载/分享动作的 Library 整页。

### 2026-11-04 MIF-09 API 证明字段复核

- 本机 `server/openapi/portal.yaml` 的 `TaskArtifact` 已定义 Workspace ID、Workspace revision、size、SHA-256 和 `verified`；服务端 Task artifact 领域对象也持有 Workspace/revision/size/digest。无需为本页面重复扩展服务端合同。
- iOS `MordeTask.Artifact` 解码器仍只读取 `id`、`label`、`media_type`、`verified`、`created_at`，缺少完整证明字段和本地文件校验/预览回调；MIF-09 的阻塞点收窄为 iOS 共享解码与安全能力适配，仍由 01 冻结并实现。旧产物或不完整证明继续仅显示元数据。

### 2026-11-04 四入口页面接线更新

- 01 现已将 Task/审批页面接入四入口导航，审批计数使用完整读取结果，并更新 UI 回归以覆盖新导航；这取代此前 Tasks/Approvals 为临时占位的状态。01 仍明确记录 Resources、MIF-02 云端会话路由及其他共享接口待交接。
- MIF-10 的两个抽屉页和本地化已接入，但等待同一轮原工程运行验收。MIF-08/09 的状态未变：没有可嵌入资源房间或安全 Library 预览接口，本组继续不改共享文件。

### 2026-11-04 本轮接口与资源依赖复核

- 会话 3 Codex 任务为 idle，交接记录仍有效；02 集成线程为 active，最新交接消息确认 Task/审批页面合同已在原 `morde` target 的 arm64 Simulator Debug 构建中通过，当前只解除任务/审批页面的接口门槛。01 清单仍将 MIF-B01、MIF-01 标为进行中，没有 Host/Workspace、Library 产物证明或占位页根路由的冻结记录。
- 原 iOS 清单中的 MI-04 仍为进行中；虽已有本机 Library/云端附件入口和若干本机检查，旧资料逐项浏览、完整 personal-data 同步及相关逻辑/UI 测试重跑仍列为待验。MIF-09 因而继续等待 MI-04 与 MIF-02/03/08 交接。
- `.staging/04/placeholder/` 已有 MIF-10 的生产页与单测，且已纳入原工程；`MordeRegistrationUITests.testFriendsAndToolChatsOpenLocalPlaceholders` 现覆盖两个抽屉入口、英文“Not implemented yet”可访问文本及 Done 返回。当前仍待保留签名的 Simulator 运行结果和 VoiceOver 语义验收，因此 MIF-10 保持待验证；MIF-08 展示页已暂存，MIF-09 页面正在其独立暂存目录实现，二者的共享数据适配仍等 01 接口冻结。
- MIF-08 展示页与策略测试现已分别暂存于 `.staging/04/resource/MIFResourceViews.swift`、`MIFResourceViewsTests.swift`。页面覆盖自有/共享只读 Host、运行/离线、控制/租约/交接状态、默认目标、Workspace 绑定摘要与安全文件入口；共享 Host 无控制按钮，离线/未知/交接中的状态不提供控制动作，过期租约仅显示客户端批准的动作。Swift 语法解析通过；对生产 SwiftUI 文件使用临时 nanoMuse API 签名桩执行 iOS Simulator `swiftc -typecheck` 通过；四项策略自检通过。XCTest 尚未进入原 target；01 为 MIF-06 当前共享改动通过的 Debug Simulator 构建未包含此暂存页面，不能算作 MIF-08 编译/运行验收。
- 为了让 01 可接线，MIF-08 暂存展示合同使用值状态加真实 ID 回调；仍缺 `MordeClient` 状态映射、Resources 根入口及原工程验收，故不能勾选。所需 literal `AppLocalized` 键需由 01 合并到九种现有语言的 `Localizable.xcstrings`。
- 设计稿 `data-view="res"` 的 AI 工具状态卡片因没有 Morde 运行状态数据源而省略；MCP 个人连接器元数据不能代替在线工具状态，旧 nanoMuse Hub `coding.sessions` 路径按 Morde target 的 fail-closed 边界不恢复。未展示 Codex/Claude 示例记录。
- 本组只改本 TODO 与 `.staging/04/placeholder/`、`.staging/04/resource/` 两个专属目录；未改共享 iOS 源码、工程配置或测试，也未连接 Portal/Host、发送或传输文件。MIF-06 工作项适配器仍由其实施会话处理，待明确释放后由 01 继续整合与验收。

### 2026-11-04 MIF-09 Library 展示组件暂存交付

- **暂存文件：**`.staging/04/library/MIFLibraryViews.swift` 提供按来源筛选的本机 Library 与 Task Artifact 列表；模型承接产物 ID、标签、媒体类型和 Workspace 证明字段，列表显示标签、媒体类型、大小与版本摘要，证明字段只用于本地预览策略判断。本机预览只接受根层提供的不透明句柄，根层解析句柄时须重查当前账号权限和本机文件证明；页面不接触路径、票据或客户端，不发网络请求，也不提供上传、下载、分享或 Task 输入附件动作。
- **预览门控：**本机条目须处于可用权限状态且带根层句柄；Task 产物须任务读取权限和 `workspace_sync` 可用、ID 非空、Workspace ID 符合 `^w[a-z0-9]{7}$`、revision/size 非负、SHA-256 为 64 位小写十六进制、`verified == true` 且句柄存在。图像、视频和音频产物还要求 `task_media_generation` 明确就绪；普通文件不依赖媒体生成能力。否则仅显示元数据。个人云端条目只显示元数据。
- **验证：**两份暂存 Swift 文件通过 `swiftc -frontend -parse`；生产页面用本地 SwiftUI/nanoMuse 签名桩通过 iOS Simulator SDK `swiftc -typecheck`。Task 预览门控的本机 Swift 断言 harness 8 项通过，覆盖 workspace_sync、媒体能力及权限/证明边界；此结果不等于 Xcode XCTest。原 `morde` scheme 构建、预览适配器和 Simulator 页面操作未运行，暂存代码不在测试 target。
- **本地化：**页面使用 `Library`、`All`、`On this iPhone`、`Artifacts`、`Unavailable`、`Nothing created yet`、`Task`、`Artifact details unavailable`、`No delivered artifacts`、`Preview`、`Metadata only`、`Shared folder`、`Cloud`、`No cloud files yet`。最初只有 `Unavailable` 缺译；01 已将该键补齐到九种语言。我再次解析 `Localizable.xcstrings` 并核对这 14 个 Library 页面键，九种语言均齐全。
- **未完成依赖：**当前 `MordeTask.Artifact` 仍未解码 Workspace ID/revision/size/SHA-256，iOS 尚无权限/能力映射及已验证本机文件句柄适配；MIF-02/03/08 与 MI-04 的前置验收也未交接。本任务保持进行中且未勾选，需由 01 冻结并接通适配、纳入测试 target 后，才进行原工程和本机 UI 验收。

### 2026-11-05 MIF-06 工作项验收与本组状态复核

- 专用 Simulator 的 Final4、Final6 均只运行 `MIFWorkItemAcceptanceUITests.testWorkItemsFilterCreateClaimConflictLinkAndDeliveryLocally`，并失败在工作项 `j1000004` 的恢复状态等待：Resume 后测试未观察到状态从“已认领”转为“待处理”。Final6 的测试报告显示相邻断言位于 `MIFWorkItemAcceptanceUITests.swift:79`、`:83`；失败后的 UI 层级还显示键盘仍开启且找不到 `morde.shell.tab.tasks` 标识。该线索提示需核对测试导航/键盘状态；XCTest 活动树没有网络追踪，不能据此断定请求未发出或服务端拒绝。
- 01 已要求原写入者交回并接管共享诊断，明确暂不改变服务端恢复语义。以上是 MIF-06 前置验收结果，不代表 MIF-08/09/10 通过，也不解除本组与共享源码之间的写入边界。
- 当前 MIF-B01 资源接口和 MIF-01 Resources 路由仍待冻结/接入；MI-04、MIF-02/03/08 的验收依赖仍未交接。MIF-08、MIF-09 保持进行中未勾选；MIF-10 保持待验证，等待原工程保留签名的 Simulator 路由及 VoiceOver 验收。
- 本组只读取本机 XCTest result bundle 并更新本 TODO；未改共享源码、未操作已有用户模拟器、未连接远端服务，也未发送或传输文件。

### 2026-11-05 MIF-06 首登与工作项交互复验通过

- 01 已在共享实现中修复首次认证后登录 sheet 未收起的问题，并给工作项列表末尾添加浮动底栏所需的滚动余量；UI 测试只在按钮点击中心可达时继续操作。此前的恢复超时由滚动内容被浮动栏遮挡导致，与服务端恢复语义无关。
- 新建隔离 iPhone 17 Simulator 上原 `morde` scheme 的 `MIFWorkItemAcceptanceUITests.testWorkItemsFilterCreateClaimConflictLinkAndDeliveryLocally` 最终通过，结果包 `/tmp/MIFWorkItemPaddedFinal.xcresult`：1 项通过、0 失败，`xcodebuild` 退出码 0。流程从干净首次认证进入四根壳，并覆盖工作项筛选、认领/恢复、Task 关联、交付、创建及阻塞冲突处理。DEBUG 替身由本地 `URLProtocol` 拦截，没有访问 Portal；结果仍有 `Publishing changes from within view updates` 运行时警告，需由集成者判断是否属于本轮范围。
- 该结果解除首登/工作项路径这项前置复验，不等于 MIF-08/09/10 验收完成。MIF-B01 的资源接口与 MIF-01 Resources 接线、MIF-02/03/08 和 MI-04 的依赖仍需 01 交接；MIF-08/09 保持进行中未勾选，MIF-10 保持待 Simulator 路由及 VoiceOver 验收。

### 2026-11-05 MIF-08/09/10 集成状态复核

- **MIF-08：**`MIFResourcePageStore`、`MIFResourcesRoot` 和四栏 Resources 路由已出现在共享源码。Host/Workspace 状态映射、控制租约动作、共享 Host 只读按钮和现有 HTTPS 同源短期票据查看器已有接线；MIFResourceViews 状态策略测试已进入 `MinisTests`。初次只读审查发现首屏加载、账号所有权/共享权限复核、打开失败反馈及 default target 映射缺口；随后 01 已在共享源码中按 scope 调用 `MordeClient.load()`、加载默认目标、过滤自有 Host、复核共享 `desktop:view` 权限，并映射打开失败与无效控制状态错误。首轮原工程构建未通过，01 已修复日志中三个调用顺序/类型错误；最新原工程已通过编译和签名。急停 fixture 的 400 根因为 fixture 只读取 `httpBody`，而 POST 通过 `httpBodyStream` 提交；提取受 1 MB 限制的共用流解析 helper 后，专用 iPhone 17 Simulator 的原 `morde` scheme 连跑急停和工作项 2 项均通过，结果为 `/tmp/MIFB01IntegrationRun1.xcresult`。随后 MIF-08/09 相关的 16 项 Task/Resource/Library 单测通过，Resources Library 入口/返回 UI 通过；账号隔离首次运行因旧测试仍按独立根页查找未显示消息/Task而失败，改成新导航下滚动检查两轮消息及 Task 后，专用 Simulator 1 项通过，结果 `/tmp/MIFB01AccountIsolationRetry.xcresult`。同轮 SSE snapshot-required 与断线重连 2 项通过。多轮结果仍含 `Publishing changes from within view updates` 运行时警告。它们均不代表 MIF-08/09/10 完成；本组仍需以本地替身验证运行/离线、共享只读、绑定未就绪、未知/过期租约、动作失败、Library 证明预览和占位路由。
- **MIF-09：**展示组件与证明门控在 `.staging/04/library/` 暂存版本已由 01 纳入原工程；`MIFResourceViews` 中可访问的 Library 按钮、Resources 到 Library sheet 路由及打开/返回 UI 流程通过定向验收，Task/Resource/Library 相关 16 项单测通过。旧 `NanoMuseLibraryRoom` 仍带分享、上传和下载动作，不能直接复用。当前 `MordeTask.Artifact` 尚未解码 API 已提供的产物级 Workspace ID、revision、path、size、SHA-256 证明字段；根适配仍将证明置空，页面预览回调尚未接入，不代表已验证本机预览。`task_media_generation` 审批预览不是交付产物的本机 resolver。复核发现现有 `MordeClient.loadWorkspaceManifest(workspaceID:revision:)` 已能读取精确 revision 的不可变 Workspace manifest，`verifyLocalWorkspaceFile(workspaceID:file:)` 已会重扫本机文件并逐项校验证明，应直接复用，不另造摘要实现。暂存页包含本机文件、Task artifacts、个人云端文件元数据三个来源；云端记录永远只显示元数据，不提供预览/下载/分享。Task 产物门控要求 `workspace_sync` 可用，媒体产物额外要求 `task_media_generation`，并有 false/nil capability 测试；暂存 Swift 解析通过，本机 Swift 断言 harness 的 8 项预览门控检查通过。仍需补 MordeTask proof 解码、根层账号/能力映射、已有本机文件验证器与不透明预览句柄、Cloud 元数据适配及真实预览/账号切换验收。
- **MIF-10：**占位页、抽屉固定路由、返回壳层和九种语言本地化已在原工程；专用 iPhone 17 Simulator 上 `MordeRegistrationUITests.testFriendsAndToolChatsOpenLocalPlaceholders` 最终通过 1 项，结果 `/tmp/MIFB01PlaceholderTestsRetry.xcresult`，覆盖两个入口、未实现说明和 Done 返回。首轮只查中文“ Morde 云端任务”导致英文运行失败，测试现已按中英文标签兼容。页面标题/说明的 VoiceOver 验收仍待完成，因此任务保持待验证。
- `morde` scheme 含 `MinisTests`、`MinisUITests`；`xcodebuild -list -project Minis.xcodeproj` 成功。MIF-08/09/10 六份暂存 Swift 文件均通过 `swiftc -frontend -parse`，MIF-09 预览门控本机断言 harness 8 项通过。原工程 MIF XCTest/Simulator 验收仍待共享写入权交接；本轮未连接 Portal/Host，未传输文件。

### 2026-11-05 MIF-08/09/10 原工程集成与验收完成

- **MIF-08：**Resources 本地 URLProtocol fixture 覆盖运行/离线 Host、Workspace 绑定未就绪、未知控制状态、租约过期、共享只读及接管/交还失败。Simulator UI 检查状态提示、动作可用性和逐 Host 错误反馈；测试不点击打开电脑，不启动 Host 查看器。原 Host 导航安全检查脚本通过，继续约束 HTTPS 同源和短期票据路径。
- **MIF-09：**Task artifact proof 可选字段解码、详情合并及账号权限/`workspace_sync`/媒体能力门控已接入；复用既有 Workspace manifest 和本机文件校验器，仅通过不透明句柄打开已验证本机预览。缺少证明的旧 artifact 只显示元数据；Library 无交付产物时显示空态。Simulator 验收打开合成的已验证本机预览并确认无证明旧 artifact 无 Preview，空态另有独立 UI 验收；未上传、下载或分享文件。
- **MIF-10：**Simulator 逐一打开好友及 AI 工具会话占位页并返回，验证可访问标签同时包含页面标题和“Not implemented yet”；不发网络请求或构造示例数据。
- **本组验证：**专用 iPhone 17 Simulator 的原 `morde` scheme 结果 `/tmp/MIF04Completion.xcresult` 为 19 项通过、0 失败（16 单测、3 UI）；`/tmp/MIF04LibraryEmpty.xcresult` 为 1 项通过、0 失败；`/tmp/MIFPreviousTaskRetry3.xcresult` 为 1 项通过、0 失败。合计 20 项本组检查和 1 项此前账户隔离回归通过。七份相关 Swift 文件 `swiftc -frontend -parse` 通过；`Tests/run-host-navigation-tests.sh` 检查通过。XCTest 使用本地合成数据，未连接 Portal/Host，未上传、下载、分享或外发文件，也未使用外部 API。
- **先前回归收尾：**账户隔离 UI 测试原先通过旧抽屉入口导航，与四栏根页不匹配；测试 helper 改为直接选择 Tasks 根标签后，`testLateTaskPageFromPreviousAccountCannotReplaceCurrentTasks` 通过。隔离断言保持不变。
- **已知提示：**Simulator 结果仍记录 SwiftUI `Publishing changes from within view updates` 警告；该提示不影响本轮通过结果，待后续集中排查。历史交接记录保留；本节验收结果和任务勾选状态为当前结论。
