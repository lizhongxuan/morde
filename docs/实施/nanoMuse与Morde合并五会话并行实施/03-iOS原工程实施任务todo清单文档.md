# 会话 3：iOS 原工程实施 TODO

先读[并行入口](00-并行执行入口及任务总览.md)、[iOS 迁移方案](../../iOS端迁移到Morde服务端方案.md)。本会话独占 `project/nanoMuse/android/src/ios/**` 和本文，按该仓库 `AGENTS.md` 处理上游子树、rebrand 和多语言；共享 CHANGELOG/文档交会话 5。迁移主体是当前工作区的 `Minis.xcodeproj`，不能以独立 `Morde.xcodeproj` 的 App 壳或 `.local` 旧副本代替。

- [ ] **MI-01（进行中：原工程 `morde` 与 `AgentWidgetExtension` 已构建；fresh iPhone 18 Pro Simulator 启动并检查 Morde 首屏，无定位请求；旧 ChatStore 只读检查有 2 个会话/23 条消息，Morde 本地历史 UI 替身 1/1 通过；原数据模拟器未覆盖安装；ActivityKit 目前仍由本机 Agent/BackgroundKeepAlive 调用，Morde Task 未接 Live Activity，且服务端/OpenAPI 无 ActivityKit push-token 合同；实际展示、远端更新、生产签名及旧资料连续性仍待验）在原 App 中完成 morde 品牌与正式入口**
  - 修改：原 `Minis` 工程、target/scheme、显示名称、图标、启动页、扩展及本地化；显示名为小写 `morde`，形成唯一的原生正式发行入口。首版保留现有 Bundle ID、App Group 和 Keychain access group，以保持旧安装与数据；若发现发布身份不允许，记录具体差异交用户决定。
  - 验收：从原工程构建，检查 App 与扩展显示名、目标与启动入口；旧本地资料仍可读。独立 Morde target 的成功构建不算本项通过。
- [ ] **MI-02（进行中：原工程 Simulator 已验注册→待审登录提示→替身批准登录→退出；拒绝/停用错误码本地化及待审批准回归 UI 共 2/2 通过；账号键/outbox 隔离有单测覆盖；快速重复提交 UI 1/1、账号 A→退出→B 会话和 Task 状态隔离 UI 1/1 通过；MCP OAuth 动态注册现发送 `morde` 与默认 Portal 元数据，原工程单测 1/1 通过；旧 nanoMuse Cloud/Relay/Hub、更新/媒体/提醒直连调用及 Cloud model discovery 在 Morde target fail-closed；2026-11-04 无认证只读 GET：根路径 200、`/admin/login` 在 `Accept: text/html` 下 200、`/api/capabilities` 在 `Accept: application/json` 下仍为 404；本机源码路由已注册，需会话 5 核对部署版本/反代，真实注册登录暂被阻塞；更广个人缓存范围仍待验；依赖 MX-01、MX-02）接 Morde 账号注册和登录**
  - 修改：原 nanoMuse 登录/首次启动页面及其请求调用；以 `https://portal.120.25.178.93.sslip.io` 为默认 HTTPS origin，注册只输入用户名/密码/确认密码，调用 Morde 注册；待审核、拒绝、禁用明确提示。登录使用有序 Client installation 合同，批准和默认额度分配完成后才能使用。
  - 验收：原 App 在本机替身上完成注册→待审核→批准后登录，验证失败、重复提交、退出和账号隔离；不显示邀请码/短信字段，不连接 nanoMuse Cloud。切换入口时保留待确认删除的旧源文件。
- [ ] **MI-03（进行中，依赖 MR-01：原工程 Simulator UI 对 `task.updated`、`task.snapshot_required`→快照读取→按快照序号重连、SSE 临时失败重试、运行中 Task 在 App 重启后恢复、未知状态及不支持操作隐藏、账号 A→退出→B 会话/Task 隔离、两轮历史读取、连续发送及审批冲突均通过；模型选择在列表刷新期间保留的修复通过连续发送回归；媒体审批仅在 `task_media_generation` 能力开启时展示精确 operation 预览及保存/拒绝确认、能力关闭时隐藏预览/保存并可拒绝，进程内替身 UI 共 2/2 通过；未确认发送沿用 Workspace revision 的重试逻辑有单测，登录/Task 状态 host checks 通过；2026-11-04 无认证只读 GET 显示默认 Portal `/api/capabilities` 返回 404，而本机源码路由已注册；会话 5 需核对部署版本/反代，真实 API/SSE 链路仍待验）将原生聊天与会话接到云端**
  - 修改：`NanoMuseShell.swift`、原聊天/Composer/会话界面及其 Morde 数据调用；承接 I02–I04/IOS-S01，使用服务端主会话、Task、消息、审批/澄清、停止及 SSE，不启动本机 Agent/模型循环。
  - 验收：在原 target 用本机替身覆盖多轮消息、重开、断线、重复提交、状态 unknown、审批冲突及账号隔离；关闭 App 不取消已受理 Task。
- [ ] **MI-04（进行中，依赖 MP-01、MP-02、MX-03：备份密钥导入、旧 rootfs archive、存储路径、Host 导航与 Workspace transfer 本机检查通过；新增 Morde 对话页本机历史与 Library 入口，复用 ChatStore/Library 本地读取及 Morde 云端附件视图；本轮补入显式确认的纯文本历史导入、请求分批/回执校验和本机 URLProtocol fixture，相关逻辑/UI 测试尚未在本轮重新编译或模拟器运行；personal-data/content 合同单测既有 13/13；未上传/下载文件；旧资料逐项浏览及完整 personal-data 同步仍待验）保留并迁移个人内容和资源体验**
  - 修改：原 Feed、Ideas、Goals、Library、个人资料/记忆、例程、附件、分享和 Host/Workspace 页面；沿用现有 Morde personal-data/outbox，未同步草稿按服务实例和账号隔离；文件只由用户显式选择，缺 capability 的入口显示未就绪。
  - 验收：本机合成数据覆盖本地历史可读、用户选择导入、冲突、撤权、Schedule 不重复运行和受限资源；不发送真实或合成文件。承接 I05–I08、I11/I12 和 IOS-S02–S11 的未完成部分。
- [x] **MI-05（候选清单已列并已提交用户逐项确认；等待确认范围）提交旧逻辑删除明细供用户确认**
  - 修改：在本文交接记录列出 `NanoMuseCloud*`、旧 Relay/Hub/CloudKit 同步、本机 Agent/iSH、直连模型/Key、旧品牌/链接及独立 Morde target 中拟删除的具体文件、target 引用、替代能力、用户数据影响和回退条件。核对共享资源、个人数据/历史、outbox、MCP OAuth 和扩展调用；不能按 `NanoMuse/` 目录整体删除。
  - 验收：清单可逐项审阅，明确保留原 `Minis` 工程及原生产品能力。提交用户确认；确认前 MI-06 保持阻塞，MI-01–MI-04 和非删除验证继续。
- [ ] **MI-06（阻塞：等待用户确认 MI-05 的具体删除范围）清理已确认的旧入口**
  - 修改：仅在确认及对应 Morde 替代能力、数据迁移/回退满足后，删除获确认的旧 Cloud/Relay、旧本机执行、直连模型和失效品牌入口，并退役独立 Morde App target；保留原工程、共享资源和未迁移用户数据。
  - 验收：按 target/call-site、构建及运行时网络目的地核对无旧 nanoMuse 服务请求/回退；旧数据可读且新功能可用。未经确认的候选保留并记录。
- [x] **MI-07（已完成本地原工程/模拟器验收：会话 5 的双平台 XCFramework/iSH Simulator 产物接入原 `Minis.xcodeproj`；原 `morde` scheme generic iOS Debug 与 Simulator `build-for-testing` 均成功；七个 FFmpeg/libav Simulator slice 均为 IOSSIMULATOR、最低 iOS 16.0；专用 iPhone 18 Pro/iOS 27.0 模拟器中媒体审批 UI 2/2、注册待审/本机批准登录及重复提交 UI 2/2 通过；新装原工程 App 已启动并检查 Morde 首屏；默认 Portal 只读路由检查已记录。远端注册审批/反代、生产签名、旧数据覆盖安全仍分别由 MI-02/03/01 跟进）构建原 App 并验证模拟器**
  - 修改：原工程构建配置及本文状态；先排查 device-only `libish_emu.a` 对模拟器链接的影响，在不删除未确认内容的前提下处理可行的本地构建问题。
  - 验收：记录原 App 的 generic iOS 和 Simulator 构建、安装、启动、注册登录界面及网络检查结果；无法完成的链接/环境项明确标待验证，不能以独立 Morde App 冒充通过。

## 交接记录

只追加本会话任务 ID、目标会话、接口/文件、输入输出、权限与错误、验证结果；删除清单及用户确认范围也记录于此。

### MI-05 清理候选明细（待用户逐项确认；工作树存在未确认源文件删除）

下表只列 iOS 边界中的候选。确认可以按类别选择；数据默认保留。只有用户确认且迁移/回退条件就绪后才进入 MI-06。`Minis.xcodeproj` 中已有的 `E5EXC0002` 文件排除属于工作区原有状态，不是本轮删除，也不代表对应源文件可直接删除。2026-11-04 只读状态审计发现当前工作树已有 12 个源文件处于删除状态，详见本文末尾；不能将这些状态当作用户已确认。

| 类别 / 候选 | 替代能力与调用边界 | 数据影响、保护与回退条件 | 状态 |
| --- | --- | --- | --- |
| nanoMuse Cloud 账号与 Relay：`NanoMuse/NanoMuseCloud.swift`、`NanoMuse/NanoMuseCloudHeaders.swift`、`NanoMuse/NanoMuseCloudView.swift`、`NanoMuse/NanoMuseAccount.swift`、`NanoMuse/NanoMuseAccountView.swift`、`NanoMuse/NanoMuseAccountData.swift`、`NanoMuse/NanoMuseRelayPicker.swift`、`NanoMuse/NanoMuseRelayRefusal.swift`、`NanoMuse/NanoMuseRelayRefusalCard.swift`；核对 `MinisApp.swift`、`NanoMuseShell.swift` 启动/账户切换调用点。 | Morde 注册/登录、默认 portal、client installation、Task/usage 和 personal-data API 已接入原 target。Morde Relay 仅覆盖 Morde API 当前支持的操作。 | 不删除 `nanomuse.cloud.*` UserDefaults、`NanoMuseCloud.instance` provider 行、云账户/配额缓存、旧 Relay Keychain 项或本地会话。nanoMuse 账户、Relay 历史未迁入 Morde；确认前需确定旧账户/历史的保留或导出办法，并保留文件/路由供回退。 | 待确认 |
| 旧 Hub / Presence / 远程任务：`NanoMuse/NanoMuseHub.swift`、`NanoMuse/NanoMuseHubTasks.swift`、`NanoMuse/NanoMusePresence.swift`、`NanoMuse/NanoMuseRemoteApproval.swift`、`NanoMuse/NanoMuseShell.swift` 中旧事件/路由，以及 `MinisApp.swift` 的事件订阅。`NanoMuseHubTasks.swift` 当前由原有 `E5EXC0002` 排除，但源文件仍在。 | Morde Hub/Host/Workspace 与 Morde Tasks。`Morde/MordeHubRelay.swift` 目前只覆盖 info/open/notify，不等价于所有旧远程动作/审批。 | 保留 `nanomuse.hub.*` ID、名称、`remoteControl`、审批/task 映射、会话映射和本地历史；未迁移动作会失去 iOS 入口。回退需保留旧文件、路由和映射，不批量删除 Hub 数据。 | 待确认 |
| CloudKit 业务同步：`NanoMuse/NanoMuseSync.swift`、`NanoMuse/NanoMuseSyncSettings.swift`、`Agent/Sync/CloudSyncEngine.swift`、`Agent/Sync/SyncDirtyScanner.swift`、`Agent/Sync/V2/**`、`Views/Settings/CloudSyncSettingsView.swift`、`Views/Sync/CloudSyncSettingsV2View.swift`、`Views/Sync/SyncMigrationDetailView.swift` 及 target 调用点。 | Morde personal-data API/outbox；`MordeCloudKitSyncPolicy.isEnabled` 当前为 false（fail-closed）。 | 本地 ChatStore、ProviderConfig、会话与历史保留；CloudKit zone/cursor 不迁移，停止后 iOS 不再读写该业务同步状态。保留用户 iCloud 文档备份：`Agent/Sync/ICloudBackupManager.swift`、`ICloudBackupView.swift`、`Backup/**`；它不同于业务同步。 | 待确认 |
| 本机 Agent、iSH 与直连执行：当前既有排除项 `NanoMuseModelsView.swift`、`NanoMuseVendorSheet.swift`、`NanoMuseChatCards.swift`、`NanoMuseHeader.swift`、`NanoMuseChrome.swift`、`NanoMuseChatModifiers.swift`、`NanoMuseChatGlue.swift`、`NanoMuseDeviceMention.swift`、`NanoMuseDeviceReply.swift`、`NanoMuseAvatarFlow.swift`、`NanoMuseVoicePrompt.swift`、`VoicePromptIntent.swift`、`NanoMuseHubTasks.swift`；另候选 `Agent/Chat/AIChatViewModel*.swift`、`Agent/ISH/ISHExecutionCoordinator.swift`、`Agent/ISH/MinisFsRouter.swift`、`Agent/ISH/ShellCommandRingBuffer.swift`、`iSH/CurrentRoot.*`、`iSH/ISHKernel.*`、`iSH/ISHShellExecutor.*`、`iSH/RootfsManager.swift`、`iSH/Terminal/*.swift` 和逐一核对调用者后的 native bridge。当前工作树已删除 `Agent/Chat/AIChatViewModel+FileTools.swift` 与 `Agent/Chat/AIChatViewModel+ISHCommand.swift`，待确认/决定恢复或保留。 | Morde Task、经授权的 Host/Workspace。 | ChatStore、iSH rootfs/文件/会话、ProviderConfig 行及用户资料留盘且可备份；删除源代码本身未清除数据，但本地文件读写/编辑和 iSH shell 操作可能不可用。Morde Task/Workspace 是远端能力，不等价于本机文件工具和 shell。须确认迁移/只读保留办法；不得删除本地数据目录。 | 待确认 |
| 已存在的 NativeOffloads / Live Activity 删除状态：`NativeOffloads/BrowserUseOffload.h`、`BrowserUseOffload.m`、`ModelUseOffload.h`、`ModelUseOffload.m`、`PlayerOffload.h`、`PlayerOffload.m`、`PlayerOffloadBridge.swift`、`SpeakOffload.h`、`SpeakOffload.m`，以及 `Shared/AudioTogglePlaybackIntent.swift`。本清单此前排除 `NativeOffloads/**` 的目录级删除；以上是当前工作树中的选择性删除状态，仍需用户确认。 | 当前 `BrowserUseOffloadBridge.swift` 仅留下空的 `releasePool` 生命周期 shim；`ModelUseOffloadBridge.swift` 保留 transient retry helper；`Views/Chat/Media/AudioPlayback.swift` 与 `Providers/Voice/VoiceOutputPlayer.swift` 保留聊天内本地媒体/TTS 能力，但没有证据证明它们覆盖旧 iSH `apple-player`/`apple-speak` 接口。Morde Task/Host 只替代经服务端授权的远端工作，不替代本机 shell offload。 | 删除的是源代码，未见相关用户数据清理；但浏览器命令、shell 模型调用、AVPlayer/AVSpeechSynthesizer 命令和 Live Activity 音频切换可能丢失或改变。需要确认是否从基线恢复这些文件，或接受现有替代/功能影响；逐项核对 native callers 后再决定。 | 待确认 |
| Provider API Key 与 OAuth 分开审阅：候选 `Providers/ProviderMigration.swift`、`Providers/ProviderConfigStore.swift` 的 `cleanupOwnProviderAPIKeysAfterMigration()`、`Providers/LLMProviderFactory.swift`、`Providers/LLMProvider.swift`、`Providers/OpenAI/OpenAIProvider.swift`、`Providers/Anthropic/AnthropicProvider.swift`、`Providers/Gemini/GeminiProvider.swift`、`Providers/Antigravity/AntigravityProvider.swift`、`NanoMuse/NanoMuseImageGen.swift`、`NanoMuse/NanoMuseVideoGen.swift`、`NanoMuse/NanoMuseProxy.swift`；另审阅 `BackupSecretsImporter.swift` 旧备份 key 跳过策略。 | Morde 服务端模型调用取代 iOS 直连请求。OAuth 工具/连接由独立 Morde OAuth 路径处理，不能与 API Key 一并移除。 | `io.github.nanomuse.app.provider.<instanceID>` 与旧 Anthropic/Gemini/OpenAI Keychain 项为仅本机凭据，删除即不可恢复；保留 OAuth Keychain、MCP OAuth `io.github.nanomuse.app.mcp-oauth` 及 provider 配置行。旧备份导入器当前跳过 provider API keys，保留该行为就无法用旧备份恢复这些 key。**本轮只移除了自动调用清理 helper 的调用点；helper 和 Keychain 凭据仍在，未执行清理。**先明确 key 导出/确认和备份兼容策略。 | 待确认 |
| 旧品牌、链接与回调：`Assets.xcassets/AppIcon.appiconset/**`、`Resources/AlternateIcons/AppIcon-*.png`、`NanoMuse/NanoMuseLaunch.storyboard`、`NanoMuse/NanoMuseLaunch.xcassets/NanoMuseLaunchMark.imageset/**`、`NanoMuse/NanoMuseUpdateCheck.swift`、`NanoMuse/NanoMuseConnectors.swift` 及 `NanoMuse/NanoMuseSettings.swift` 中旧官网/更新/政策 URL。`NanoMuseConnectors.swift` 的 MCP OAuth `client_name/client_uri` 已更新为 morde/默认 Portal，回调未变。 | 新增 Morde 图标/启动 mark 和 Morde portal 入口；目前没有单独的 Morde 隐私/帮助/更新 URL。 | 旧 URL scheme、MCP/OAuth callback、分享/文件扩展路由不随品牌更新删除；旧更新获取在 `MordeLegacyServicePolicy` 关闭时不会发送请求。不得把隐私/帮助链接臆改到不对应的 Portal 页面；旧视觉资源和链接源文件先保留。 | 待确认 |
| 独立 Morde App target 退役：`Morde.xcodeproj/**`、独立 `Morde` scheme/test target、`Morde/MordeStandaloneApp.swift`、`MordeInfo.plist`、`MordeUITests/**`。 | 正式交付入口为原 `Minis.xcodeproj` 的 `morde` target/scheme；共享 Morde 源码仍由原工程使用。 | 独立 target 的 bundle ID `io.morde.app` 使用不同数据容器；退役不会迁移该容器。确认前需确定该 target 安装数据的保留/导出办法。不得删除原工程、`MinisShare`、`MinisFileProvider`、`MinisUITests`、`MinisTests`。 | 待确认 |

**始终保留的身份与数据边界：**原 app/extension Bundle ID `io.github.nanomuse.app*`、`Minis.entitlements`、Share/FileProvider entitlements、App Group `group.io.github.nanomuse.app`、iCloud container、Share/FileProvider 扩展、Documents/ChatStore/iSH 本地数据、iCloud 用户文档备份、Provider/OAuth 本地记录和 MCP OAuth Keychain。不得以删除 target、改 bundle ID 或重置模拟器数据绕过兼容。

### MI-08 三个 iOS 板块清理范围（待用户确认；数据与服务端对象默认保留）

用户已指定移除 Feed/动态、Ideas/点子、Goals/目标的 iOS 板块与页面入口；本清单确认具体源代码和运行行为。**不删除本地/云端 Feed、Idea mark、Goal、Schedule、例程、历史、outbox 或备份。**拟议最小范围如下：

| 板块 / 候选位置 | 拟停止的 iOS UI/行为 | 必须保留的数据、共享能力与影响 |
| --- | --- | --- |
| 导航/深链：`NanoMuse/NanoMuseShell.swift` 中 `NanoMuseTab`、`NanoMuseRoomDestination.resolve`、`roomLayer`、Goals-from-Ideas 子页及 Feed/Goals 事件路由；`NanoMuse/NanoMuseShellChrome.swift` 的 Goals 快捷按钮；核对 `MinisApp.swift` 的 `feed`/`ideas`/`goals` room notification 入口。 | 底栏只保留 Chat、Library；移除 Feed/Ideas 根页和 Goals 子页。旧深链安全回 Chat 或显示不可用提示，不能崩溃/残留选中态。 | 保留 Library、Chat、资源分享、Morde Hub/Workspace 路由。更新 `MinisTests/NanoMuseLogicTests.swift` 的 tab/room 路由断言。 |
| Feed：`NanoMuse/NanoMuseFeed.swift` 中 `NanoMuseFeedRoom`、post/detail/settings/fence-card UI；`NanoMuseShell.swift` 的 discuss/open-session 和 first-day 调用点；`NanoMuseFeedFlow.writeFirstDayIfNeeded`、`generateNow`、feed 专属 `afterTurn`/routine hooks。 | 移除动态页面、快捷入口与板块触发；停止 Feed 首日自动生成、本机 Feed routine/提醒/Agent 执行。当前 `ensureRoutine()` 已返回 nil 且 Morde local execution policy 关闭，但仍需核验并移除失效调用入口。 | 保留 `NanoMuseDirs.root/feed/**`、`feed-preferences.md`、`nanomuse.feed.*` defaults；保留 `NanoMuseMordePersonalContentStore`、Morde Feed post/preferences API/outbox 和 `NanoMuseMordeFeedPostBody/Post/Preferences` 合同。Feed 历史不清除；无 iOS UI 后暂时不能浏览/编辑。 |
| Ideas：`NanoMuse/NanoMuseRooms.swift` 中 `NanoMuseIdea`、`NanoMuseIdeaSection`、`NanoMuseIdeas`、`NanoMuseIdeasRoom`；`NanoMuseShell.swift` 的 Ideas 路由和 `createRoutine(from:)`/start-goal 调用点。该文件还包含 Library，不能整文件删除。 | 移除点子页、Goals 快捷入口，以及由点子创建本机 routine/目标的入口。 | 保留 Library、内置 catalogue、个人 Idea marks、Morde personal-data outbox/服务端记录。只移除浏览 UI 会使已保存点子暂不能从 iOS 打开，服务端数据不变。 |
| Goals：`NanoMuse/NanoMuseGoals.swift` 中 `NanoMuseGoalsRoom`、本地追踪/进度 UI、`NanoMuseGoalCategorySheet`、`NanoMuseMordeGoalEditor`、`NanoMuseScheduleImportSheet`、`NanoMuseRoutineEditor` 的页面调用；`NanoMuseShell.swift` 的 Goals 子路由/创建入口。 | 移除目标页、追踪/创建入口和该页内 routines 区块。该页还承载 Morde Cloud Goals 与 Cloud schedules 浏览/编辑/导入；若整体移除，这些对象仍在服务端，但 iOS 暂无此页面入口。 | 保留 `goals.json`、`routines.json`、会话/历史、`NanoMuseGoalStore` 读取、账号隔离及 `NanoMuseAccountData.accountPaths` 备份/切换逻辑；不清空、取消或改写云端 Goals/Schedules。确认后需明确是否连同 Cloud Goals/Schedules 的 iOS 管理入口一起隐藏。 |
| 共享调度/提醒/生成钩子：`NanoMuse/NanoMuseScheduler.swift`、`NanoMuse/NanoMuseAccountData.swift`、`NanoMuse/NanoMuseSettings.swift` 的 All routines 入口；`MinisTests/NanoMuseLogicTests.swift` 中 routine/goal/feed 纯逻辑测试。 | 仅停止 Feed/Goal 专属本机自动生成、goal check-in 执行和页面触发；保留普通用户 routine 独立设置入口，除非用户另行指定。更新受影响逻辑测试，不删除 scheduler 源文件。 | Scheduler 同时负责用户 routines、提醒、账号隔离和 Cloud schedule 本地 receipt；禁用整文件会影响无关 routines/已导入 Schedule。保留 routine 数据和通知设置，不自动取消提醒或云 Schedule。 |
| 首次运行说明/测试：`NanoMuse/NanoMuseFirstRun.swift` 的 Feed/Routines 介绍；`MinisTests/NanoMuseLogicTests.swift`、`MinisTests/NanoMuseMordeContentPagesTests.swift` 中相关断言。 | 移除或改写失效功能介绍/入口测试；保留 Morde Feed/Idea mark 数据合同测试。 | 不删除共享 personal-content API/outbox 测试；补充旧 deep link、Chat/Library、数据留存和不再触发本机生成的验证。 |

**确认点：**请确认 MI-05 表中旧逻辑删除类别（可逐项批准或保留），并确认 MI-08 是否按表所列范围移除三个板块。会话 4 已按总入口第 7 节确认：Goals 页移除时同时隐藏 Morde Cloud Goals/Schedules 的 iOS 管理入口，服务端能力和云端对象继续保留；此交接不替代具体范围的用户确认。任何获准范围仍不清除数据、凭据或云端对象；数据销毁或取消 Schedule 需单独确认。

### 2026-11-03 本轮开始

- MI-01/02/07 开始在原 Minis 工程推进。原工程已有 Morde API、默认 HTTPS origin、有序 Client 登录、聊天/Task 与个人数据接入；注册界面缺失，Minis target 仍显示 `nanoMuse`。Bundle ID `io.github.nanomuse.app` 保留以沿用本机容器数据。
- 原工程设备构建已由只读构建审计确认通过；初始 Simulator 链接被 device-only `libish_emu.a`、`libish.a` 阻断。本轮通过仅存于 `/tmp` 的 simulator 依赖与搜索路径覆盖完成原工程 Simulator 构建，继续补充运行验收。
- `AppIcon.appiconset`、`NanoMuseLaunch.storyboard` 引用的旧 NanoMuse mark、旧 Cloud/Relay/Hub/CloudKit/本机执行/Provider 入口及独立 Morde target 尚未删除或替换；具体候选和数据影响待 MI-05 清单列明并提交确认。

### 2026-11-03 会话 3 本轮实施与验证交接

- **原工程品牌/登录：**只在 iOS 子树和本文修改。原 `Minis.xcodeproj` 的正式 App target/product/scheme 改为 `morde`，保留 Swift module `Minis`、Bundle ID `io.github.nanomuse.app`、App Group/Keychain/iCloud 身份及 Share/FileProvider 扩展；新增 Morde icon/launch mark，不删除旧视觉资源。首启文案为 “Welcome to morde”。
- **注册接口：**`MordeRegistrationPayload` 仅生成用户名和密码字段；原 target 登录表单增加注册模式与确认密码，通过默认 portal 调用 `POST /api/auth/register`。OpenAPI 契约为 201 返回 pending user，不发登录 session；客户端显示待审核状态，不保存凭据、不请求邀请码或短信。未实际注册、未使用提供的管理员/API/SSH 凭据，未发送文件。
- **本地数据保护：**取消 `ProviderMigration` 自动调用 `cleanupOwnProviderAPIKeysAfterMigration()`；helper 仍存在但无调用点，未删除 Keychain 项。保留旧 Cloud/Relay/Hub、本机执行、OAuth、iCloud 文档备份、本地用户数据和所有待确认候选；见 MI-05/MI-08 清单。
- **构建/检查：**从原 `Minis.xcodeproj` 的 `morde` scheme 完成 generic iOS Debug build、Simulator `build-for-testing` 和 `Tests/run-credential-expiry-tests.sh`（含注册 payload 断言）。iPhone 18 Pro Simulator 产物用 `/tmp` 中的 simulator 版 iSH/FFmpeg 和搜索路径覆盖构建，没有替换仓库依赖。最初发现 XCTest 缺少 `UITargetAppPath`，原工程 `MinisUITests` Debug/Release 的 `TEST_TARGET_NAME=Minis` 与已改名的 App target 不一致；已改为 `morde` 并重建。临时签名只带原 App Group，未伪造生产 Team/Application ID；Simulator 中 `codesign --verify --deep --strict` 通过。
- **Simulator 运行验收：**原工程 App 在新建干净的 iPhone 18 Pro 模拟器上安装并启动，首屏、主屏图标和标签 `morde` 可见。最终 `test-without-building` 通过 3/3（注册 UI 1 项、Task 端侧语音策略 2 项）；表单检查默认 Portal、用户名、密码、确认密码和无邀请码/短信字段，未点注册。无安装账号时 `MordeClient.load()` 在发起 capability 请求前返回，因此本次表单测试未访问 Portal。XCTest 报过一次 `Publishing changes from within view updates is not allowed` 运行时警告，来源未定位。
- **Task 语音：**原 Morde Task Composer 新增纯文本听写入口，使用独立 `SpeechRecognitionManager`；启动前要求系统 recognizer 可用且支持端侧识别，录音请求设置 `requiresOnDeviceRecognition=true`，不调用 `VoiceInputPanel` 的远程 provider 候选。停止后只把识别文本放入草稿，仍需用户手动发送；切换/退出时停止录音。`MordeTaskSpeechInputPolicyTests` 在同一原工程 Simulator 通过 2/2，覆盖强制端侧不可用时拒绝启动及既有通用路径行为。实际麦克风录音/端侧模型执行尚未在模拟器验证。
- **Task 图片跨界合同：**当前原生发送只包含 `model_id/content/host_id/workspace_id/input_revision`；消息 DTO 的现有 `attachment_ids` 为 personal `pa_...`，不可复用为 Task 图片。用户转来的 MR-04 临时提案是 JSON `attachments:[{media_type,data_base64}]`，最多 4 张 JPEG/PNG/WebP、raw 总量 ≤8 MiB，bytes/message/Task 原子提交，返回 `ta_...` 并纳入幂等摘要；Session 5 指出账号级容量/保留规则仍待用户决定，OpenAPI/capability 尚未发布。已将接口和 ASR 边界交给会话 1（运行时/API）与会话 5（OpenAPI/公共合同）。图片上传控件尚未添加，当前仍为纯文本；不复用 `pa_...`、不把 Share Extension 自动转入 Task，也不发送图片或语音音频。待 quota/retention 决策和正式 capability 后再接入图片。
- **未完成/状态：**未完成登录/审批/批准后登录的本机替身验收、Task/SSE/重试全流程、旧本地资料读取与网络目的地检查；真实端侧 ASR 也待设备验证。没有用独立 `Morde.xcodeproj` 代替原工程。MI-05/MI-08 的具体清单等待用户确认，MI-06/MI-09 继续阻塞；MI-01/02/03/04/07 保持进行中。保留工作区已有改动，未执行旧功能、target、资源或用户数据清理。

### 2026-11-03 本机注册请求替身验收补充

- **实施：**原 `Minis.xcodeproj` 的 Debug App 在带 `-morde.localRegistrationStub` 启动参数时，令 `MordeClient.responseData` 使用进程内 `URLProtocol` 测试 Session；`MordeAuthURLProtocol` 对默认 Portal 的注册请求返回 201 pending user、对 `/api/capabilities` 返回 ordered-login capability、对 `/api/auth/client-login` 返回待审 403，其余该 Session HTTPS 请求本地拒绝。Release 和普通 Debug 启动仍使用既有 `URLSession.shared`；没有访问 Portal、创建服务器账号或发送文件。
- **合同/界面验证：**原 target 的 `MordeRegistrationUITests.testRegistrationSubmitsAccountPasswordOnlyAndShowsPending` 覆盖默认服务地址、注册表单无邀请码/短信、注册后待审核状态、再次登录收到待审提示且未进入已登录界面；同轮 `MordeRegistrationPayloadTests` 2/2 通过，验证用户名去空白、拒绝空用户名/密码且 JSON 只有 `name`/`password`。批准后的成功 Client login、退出及账号隔离尚未模拟/验证。
- **构建/模拟器：**原 `Minis.xcodeproj` / `morde` scheme 的 iPhone 18 Pro Simulator `build-for-testing` 通过；最终组合 `test-without-building` 在 `Morde Session 3 Review`（iOS 27.0）通过 5/5（注册/待审登录 UI 1、payload 单测 2、Task 端侧语音策略 2）。构建只在 `/tmp` 使用 simulator iSH/FFmpeg 依赖与 framework，应用和扩展用本机临时 ad hoc 签名，主 App 仅保留原 App Group entitlement；没有修改仓库依赖或重置模拟器数据。`Tests/run-credential-expiry-tests.sh`、`git diff --check`（iOS 改动文件）、`plutil -lint Minis.xcodeproj/project.pbxproj` 通过。XCTest 仍报告既有 `Publishing changes from within view updates is not allowed` 运行时警告，未定位。
- **未完成/交接：**服务端批准后 Client login、pending/rejected/disabled 的登录提示、退出和账号隔离、Task/SSE/重试、旧本地资料可读及实际网络目的地检查仍待验；提供的管理员/API/SSH 凭据仍未使用。设备生产签名、真实设备端侧 ASR 未验。MI-05/MI-08 清单仍等用户确认，MI-06/MI-09 未删除任何源文件/target/资源/数据。

## 用户补充任务：移除三个 iOS 板块

> 用户已明确指定移除截图中的“动态”（Feed）、“点子”（Ideas）和“目标”（Goals）三个板块，包括目标页里的追踪、例程和创建目标入口。本决定覆盖本文件 MI-04 及旧迁移方案中“iOS 保留 Feed/Ideas/Goals”的表述；实际删除前仍按用户先前要求列出具体范围供确认。其他旧功能删除仍按 MI-05/MI-06 处理；不得据此删除用户业务数据。

- [x] **MI-08（清单已列；用户已确认仅移除 Feed/Ideas/Goals iOS 页面和入口）盘点三个板块的具体范围**
  - 修改：核对下方 MI-09 涉及的页面、导航、旧深链、自动生成/本机提醒、共享文件和测试，在本清单列出具体候选、保留的数据、对其他功能的影响及回退办法。
  - 验收：向用户提交可审阅的具体清单并记录确认结果；用户已选定这三个板块，不必再次询问是否要移除。确认前继续不涉及删除的任务。
- [ ] **MI-09（进行中：用户确认范围仅含 Feed/Ideas/Goals iOS 页面和入口；导航/入口收口已改，尚待原工程构建与 Simulator 验证）移除截图指定的三个 iOS 板块**
  - 修改：在原 `Minis` target 中移除 Feed、Ideas、Goals 的底栏/页面、入口、设置、快捷操作及专属路由。当前代码中 Goals 是 Ideas 的子页，截图旧版是独立标签，两种导航和 `feed`/`ideas`/`goals` 旧深链都要处理；失效入口安全回到聊天或提示不可用。保留聊天和资源库，当前底栏应只显示这两个现有入口。
  - 修改位置：核对 `NanoMuseShell.swift`、`NanoMuseShellChrome.swift`、`NanoMuseFeed.swift`、`NanoMuseRooms.swift`、`NanoMuseGoals.swift`、`NanoMuseAccountData.swift`、`NanoMuseScheduler.swift`、`NanoMuseSettings.swift`、`NanoMuseSystemFiles.swift` 及相关本地化和 iOS 测试。仅清理三个板块独占的 UI、事件与自动生成流程；`NanoMuseRooms.swift`、调度器等共享文件先核对引用，不整文件删除。
  - 数据和运行边界：不自动创建/运行板块专属 Feed 生成或移除页面关联的 iOS 本机任务/提醒；处理既有板块专属后台触发时保留数据和可恢复记录。不得清空已有动态、点子、目标、例程、历史或 outbox，不删除/取消 Morde 服务端数据或云端 Schedule，也不影响 PC、服务端及其他 iOS 功能。发现必须清除数据或影响无关功能时，先单独列明并取得用户确认。
  - 验收：用原 `Minis` 工程在本机构建并在可用模拟器核对：底栏和页面没有三个板块及其入口，旧深链/旧选中状态不会崩溃或回到已移除页面；聊天、资源库、注册登录及其他功能正常，板块专属自动生成不再触发，既有本地和云端数据未被清除。同步修改相关 iOS UI/导航测试，记录实际构建、模拟器和未验证项；不发送文件。
- [x] **MI-10（已完成：Morde target 启动跳过旧 First Run 设置；登录页不显示服务地址字段；会话加载自动 ensure 主会话并选择默认文本+工具模型；云端其他会话和本地历史从侧栏打开）登录后直达唯一主会话**
  - 修改：原工程默认显示 Muse 风格聊天壳，不进入供应商/模型首次设置；登录表单使用既有默认 Portal，不要求配置服务地址；云端模型列表就绪后默认选首个支持文本与工具的模型。唯一主会话沿用 `/api/me/main-conversation` 与幂等 ensure；其他云端会话只从侧栏打开，历史只从侧栏归档入口打开。保留图像、视频、语音生成与已确认 fail-closed 的 Task 图片上传策略，不删除项目会话、数据或旧代码。
  - 验收：原 `Minis.xcodeproj`/`morde` scheme 在独立 iPhone 18 Pro/iOS 27.0 Simulator 编译通过；定向 UI 用例在 xcresult 中 Passed，覆盖登录后 `uninitialized`→ensure→主会话就绪、默认模型可发送、主页隐藏侧会话并可由侧栏打开。xcodebuild 在用例通过后卡在日志/coverage 收尾，由于清理无进展手动中断（退出码 75，TEST INTERRUPTED）；本机结果包 `/private/tmp/morde-s3-chat-first-20261105.xcresult` 的测试计划、suite 和单例用例均记录 Passed。未访问真实 Portal，未验证真实账号/API 链路；不影响 MI-02/03 的远端交接。
- [ ] **MI-11（进行中：聊天消息分页/重试与设置入口本机 fixture 已通过；S1/S5 模型合同已接入，新增模型选择与默认模型 fail-closed fixture 待复验）复用登录态并补齐聊天历史读取**
  - 修改：在 `MINIS_CLOUD_ONLY` 设置中移出旧 nanoMuse Cloud OTP/密码登录、重复 Cloud tasks 登录壳、旧 Cloud task execution、仅说明“iPhone 不支持”的 Hands 和读取旧 `NanoMuseHub` 的 Computers 设置入口；保留旧数据与非 cloud-only 路径，Morde Account 继续共用 `MordeClient` 登录态。修复当前会话消息 `GET /api/conversations/{id}/messages?limit=100&cursor=...` 分页、针对会话的失败重试和较早消息重试入口。
  - 模型合同与实现：依据 S1/S5 冻结合同，使用主 Morde client bearer 凭证读取 `/api/available-models` 与 `/api/model-connections`；模型选择器只列服务端当前可用的管理员默认模型和 owner 私有、已检查且支持文本+工具的模型，标签区分两者。Cloud Task 发送由客户端和 UI 双重检查 `default_model_configured`；为默认模型缺失或 ID 不在文本+工具目录中的响应 fail-closed。连接的创建/编辑仍由 Morde Account 的同账号短期 Portal WebView 管理；不新建原生配置表单，不持久化临时 Task 模型选择，不向客户端暴露凭据。Task 媒体输入仍关闭。
  - 设置页逐项审计：Morde Cloud、Cloud tasks、Cloud task execution 属旧 OTP/Relay 或重复认证，当前 Morde cloud-only 设置不提供这些入口；Morde account 保留且复用主登录。灵魂、形象、记忆、系统文件、技能、连接器与环境变量是本机资料/本机工具配置，保留并不注入 Cloud Task；动手操作目前是 iPhone 不支持说明，旧 Computers 设置读取旧 Hub 数据，入口收起，Morde Resources 的 Host/Workspace 页面保留。后台通知为本机通知/提醒设置，当前无 Morde Task APNs 合同，保留本机入口；存储、共享文件夹、挂载、iCloud 同步、备份/恢复是本机数据管理入口，保留。外观、数据控制、网络、日志与关于属于应用本机管理，保留。
  - Host/Workspace 权限：现有 `GET /api/hosts`、`/api/shared-hosts`、`/api/workspaces` 经 Morde client credential 读取；HTTP forbidden/失败由 `loadResource` 留在各自 `.failed` 状态并提供重试，Task 目标不可用时显示明确原因且提交禁用，Workspace 文件操作依 `workspace_sync` capability fail-closed。本轮不绕过权限；没有发现需要 S1/S5 更改的合同缺陷。
  - 删除与数据边界：未清除用户历史、模型连接、本地文件、设置、通知或账户数据，也未删除尚未逐一确认的 12 个旧源文件/targets/资源。重复登录/旧 Hub 代码目前仍在源树的非 cloud-only 条件分支中；如要从正式 Minis target 清理，需在 MI-05 按路径核清旧功能依赖、替代能力和残留本机数据影响后再执行引用/target 检查。图片、视频和语音生成策略按用户确认保留，端到端生成仍待真实能力合同启用后验收。
  - 验收：原 `Minis.xcodeproj` / `morde` scheme；本地 URLProtocol UI fixture 覆盖消息首屏临时失败后的针对性重试、加载第 2 页并保留新消息，及模型管理员默认/个人模型选项与缺少管理员默认时的提交关闭。仅在独立本机 Simulator 验证，不访问 Portal/远端、不使用线上账号或 API key、不发送文件。验证结果待记录。

### 2026-11-03 会话 3 Hub、Workspace 与云端管理入口补充

- **实施：**按用户明确指令，在 `NanoMuseGoalsRoom` 隐藏 Cloud Goals/Schedules 管理区、移除自动拉取，并隐藏本地 routine 的“Import to cloud”入口；目标/例程本地页面、实现代码、本地数据、服务端 Goal/Schedule API 和云端对象保留。未删除文件、target 或资源，也未取消 Schedule。
- **Hub/Workspace 检查：**原 iOS Hub relay 调用路径保留目标在线及当前设备检查、权限拒绝和断连时不报告成功；UI 仅对在线、未撤销且非当前设备提供打开入口，首次操作先显示含目标与 URL 的授权提示。`Tests/run-morde-hub-relay-tests.sh`、`Tests/run-credential-expiry-tests.sh`、`Tests/run-morde-workspace-transfer-tests.sh` 均通过；覆盖离线/无目标/forbidden/断连不成功、`workspace_sync` capability 缺失关闭相关调用。Hub relay script 是独立 host Swift 检查，**不包含在本次 Xcode Simulator 测试结果内**。
- **原工程构建/模拟器：**`Minis.xcodeproj` / `morde` scheme `build-for-testing` 成功，使用原工程产物及既有本地 Simulator 依赖覆盖；未重置模拟器。`test-without-building -parallel-testing-enabled NO` 中 `MordeRegistrationPayloadTests` 与 `MordeTaskSpeechInputPolicyTests` 通过 4/4。扩展 UI 测试 `testRegistrationApprovalHubConfirmationAndSignOut` 失败于清理已存在账号的前置条件：`morde.account.signedOut` 未在账号页 AX 中出现，随后重启未找到预期的 `Morde 云端任务` 按钮（见 `/private/tmp/morde-session3-hub-auth-confirmation-final-no-clone.log`）；因此本轮没有运行到注册成功登录、Hub 确认弹窗、撤销设备 UI 或 Goal/Schedule 页面断言，不能把这些列为 Simulator 已验收。先前注册待审核界面的定向 UI 用例仍有 5/5 通过记录。
- **环境与检查：**首次本轮 `test-without-building` 在启动 XCTest 前因 CoreSimulator 克隆含 FileProvider 目录时权限错误失败；改为非并行在原模拟器运行后，4 个单测通过、上述 UI 用例暴露前置状态错误。`plutil -lint Minis.xcodeproj/project.pbxproj` 与 `git diff --check` 通过。代码中隐藏云管理区已由原工程成功编译，但因 UI 用例未到达 Goals 页面，运行时入口仍待后续验收。
- **交接/边界：**本轮仅使用进程内 DEBUG URLProtocol/Hub fixture，不请求真实 Portal、不创建账号、不连接真实 Hub 设备、不传输文件或发送通知；未使用用户提供的管理员、SSH 或 API 凭据。未向外部发送任何文件。MI-08/09 的三个板块整体清理范围仍需按清单确认；本次用户直接授权的 Cloud Goals/Schedules iOS 管理入口隐藏已完成，其他旧代码与数据保持原样。

### 2026-11-03 会话 3 最终 UI 诊断与交接

- **最后一轮结果：**原 `Minis.xcodeproj` 的 `morde` scheme `build-for-testing` 通过；`test-without-building -parallel-testing-enabled NO` 中注册 payload 与 Task 语音策略 XCTest 共 4/4 通过。扩展 UI 用例仍失败，未到注册提交、批准登录、Hub 授权或退出断言。首次并行运行在 XCTest 启动前因 CoreSimulator 无法克隆受保护的 FileProvider 数据目录失败；非并行运行进入用例。
- **AX 证据：**本地保存并查看了 `app.debugDescription` attachment。当前旧模拟器已在主壳 Settings→Morde account，Morde Client 实际无登录态，所以退出和 installation 管理控件不存在；测试不应要求单独的 `morde.account.signedOut` 文本。随后通过 `morde.settings.cloudTasks` 进入 Morde 页，但服务地址查询 `app.textFields["https://morde.example"]` 遇到 XCTest 将元素报告为 `SwiftUI.VerticalTextView` 的类型不匹配，字段查找失败。已在工作区加 `morde.auth.server` 标识并改为 any-element 查询，但这两处最后改动尚未重新构建或运行，不能视为已验证。
- **模拟器/范围：**原 `Morde Session 3 Review` 未 reset/erase；Xcode 测试结束后为 Shutdown。按此前指示新建的 `Morde Session 3 UI Clean` 仅启动到系统初始界面，最新收尾指示到达后即 Shutdown，未安装/运行新产物，也没有影响旧模拟器数据。Goals 页面云端管理区的隐藏代码已由此前原工程构建编译；由于 UI 测试未到达该页，运行时隐藏仍待验收。最终 UI source/query 的最后两处调整未构建。
- **可复用通过项：**`Tests/run-morde-hub-relay-tests.sh`（独立 host Swift 检查，含无目标/离线/forbidden/断连不成功）、`Tests/run-credential-expiry-tests.sh`（含 `workspace_sync` 缺失关闭）、`Tests/run-morde-workspace-transfer-tests.sh` 均通过。Hub host 脚本不在原工程 Xcode Simulator 结果内。`plutil -lint Minis.xcodeproj/project.pbxproj` 和 `git diff --check` 通过。
- **后续状态：**先修正 UI 测试的首启/Settings 入口与服务地址控件 AX 查询，再用新建的干净 Simulator 验证注册待审→批准登录→授权确认/撤销设备 gate→正常退出及云 Goal/Schedule 入口隐藏；当前 UI 不得标为通过。服务器、旧功能代码、targets、资源、用户数据、云端 Goals/Schedules 均未删除或改变。

### 2026-11-04 会话 3 clean Simulator 最终 UI 验收

- **测试修正：**仅更新 `MinisUITests/MordeRegistrationUITests.swift` 的列表滚动与 AX 查询：Settings 账户列表滚动到安装管理/登出项；登出确认和 Hub 授权取消按 accessibility identifier 的首个匹配项操作，兼容 iOS 27 将 SwiftUI confirmation dialog 暴露为嵌套 Sheet 项。未改变生产登出、Hub relay 或服务行为。
- **原工程构建：**使用 `/Users/zhongxuan_li/Desktop/morde/project/nanoMuse/android/src/ios/Minis.xcodeproj` 的 `morde` scheme；`build-for-testing` 成功，沿用 `/private/tmp` 的 simulator iSH/FFmpeg 依赖覆盖与临时 ad hoc 签名。一次中间构建因漏传这两个本地搜索路径而链接 device-only `libish_emu.a` 失败，补回既有参数后构建成功；未改仓库依赖。
- **最终结果：**`Morde Session 3 UI Clean`（iPhone 18 Pro、iOS 27.0，UUID `2A74B040-7246-498E-AD48-F79D35028A82`）的原工程 `test-without-building -parallel-testing-enabled NO` 通过 5/5：`MordeRegistrationUITests.testRegistrationApprovalHubConfirmationAndSignOut` 1 项、`MordeRegistrationPayloadTests` 2 项、`MordeTaskSpeechInputPolicyTests` 2 项。结果包：`/private/tmp/morde-session3-hub-auth-clean-scroll-final-c.xcresult`。
- **UI 覆盖：**默认 Portal 地址；用户名/密码/确认密码且无邀请码或短信字段；本地 URLProtocol 替身注册待审与待审登录提示；替身批准后正常登录；Goals 页面无 Cloud goals / Cloud schedules 管理内容；账户页安装管理可见，撤销设备标记且只有一个可打开设备；打开前授权提示可取消；返回后确认退出，本地安装管理入口消失。Morde API 请求由进程内 fixture 响应；没有访问真实 Portal、创建真实账号、连接远端设备或发送文件。
- **边界与未验：**未重置/清除任何模拟器数据；成功流程登出了本轮合成账号，原 `Morde Session 3 Review` 保持未 reset。未验生产签名、Share/FileProvider 扩展实际运行、真实 Portal 审核链路、账号隔离、拒绝/禁用及重复提交、Task/SSE 全流程、真实端侧 ASR 或实际网络目的地。独立 host Swift relay/capability 脚本此前通过，但不属于本次 Xcode 5 项结果。XCTest 仍有 `Publishing changes from within view updates is not allowed` runtime warning，来源未定位。
- **状态/交接：**MI-02、MI-07 仍进行中，MI-03/04 及跨会话依赖按原状态；MI-05/MI-08 的具体清单可在本文审阅，仍待用户确认，MI-06/MI-09 未按确认范围执行清理。后续只读状态审计发现工作树已有 12 个源文件删除状态，不代表获准删除，见下一节；没有恢复或继续删除。没有使用提供的管理员、SSH 或 API 凭据；没有向外部发送文件。

### 2026-11-04 当前工作树删除状态与 target 引用复核

- **文件状态：**`git -C project/nanoMuse status --short -- android/src/ios` 显示以下 12 个未暂存删除（状态来自现有工作树；无法从当前 diff 证明由哪个会话/操作者产生）：`Agent/Chat/AIChatViewModel+FileTools.swift`、`Agent/Chat/AIChatViewModel+ISHCommand.swift`、`NativeOffloads/BrowserUseOffload.h`、`NativeOffloads/BrowserUseOffload.m`、`NativeOffloads/ModelUseOffload.h`、`NativeOffloads/ModelUseOffload.m`、`NativeOffloads/PlayerOffload.h`、`NativeOffloads/PlayerOffload.m`、`NativeOffloads/PlayerOffloadBridge.swift`、`NativeOffloads/SpeakOffload.h`、`NativeOffloads/SpeakOffload.m`、`Shared/AudioTogglePlaybackIntent.swift`。这些文件仍在 Git index/HEAD 中，当前工作树内容可从基线恢复；未发现本机用户数据被这些路径删除。
- **target 引用：**只读 `rg` 检查当前 `Minis.xcodeproj/project.pbxproj` 与 `Morde.xcodeproj/project.pbxproj`，未发现上述 12 个文件名的显式 file/build-phase 引用。当前 `Minis.xcodeproj` 的 pbxproj、`morde` scheme 与 workspace 元数据在 `project/nanoMuse` 中为 untracked；此前 5/5 模拟器结果针对这份本地原 Minis 工程，不能据此声称这些文件功能等价替代。源码仍有过期注释：`Views/Chat/Media/VideoPlayer.swift:167` 提及 `PlayerOffloadBridge`，`Agent/Background/AgentLiveActivityManager.swift:277` 提及 `AudioTogglePlaybackIntent`。
- **功能/数据影响：**前两项删除包含本地文件读/写/编辑和 iSH shell 命令实现；Morde Task/Workspace 是远端能力，不能视为等价本机替代。NativeOffload 删除涉及浏览器命令、模型命令、AVPlayer、AVSpeechSynthesizer 及 Live Activity 音频切换；当前 Swift bridge/聊天播放器只覆盖部分相邻能力，完整兼容性未证明。现有证据仅表明源文件在工作树缺失，未见 ChatStore、iSH 文件、媒体或 Live Activity 持久数据被清除。
- **需用户确认：**请决定上述 12 个文件是否恢复到工作树，或在逐项接受受影响本机功能/替代路径后保留删除；另外 MI-08 仍按表列的 Feed/Ideas/Goals UI、导航和专属触发范围单独确认，MI-05 其余旧 Cloud/Relay、Hub、CloudKit、Key、品牌资源与独立 Morde target 也需逐类选择。没有把任何既有删除视为批准；MI-05/06/08/09 状态不变。本次只做只读核验与 TODO 记录，没有再改源码或运行测试。

### 2026-11-04 原 Minis 工程 Live Activity target 差异

- **核对结果：**当前工作区 `Minis.xcodeproj` 没有 `AgentWidgetExtension` target，也没有把 `AgentWidgetExtension.appex` 嵌入 `morde.app`；只读对照本地原始工程参考副本（未以该副本构建或交付）发现原 target ID 为 `E5H000040`，并包含 `AgentWidgetBundle.swift`、`AgentLiveActivityWidget.swift`、`Info.plist` 和 `AgentWidget.entitlements`。上述四个源/配置文件仍在 iOS 工作树中，但当前 `Minis.xcodeproj` 未编译或打包它们。
- **功能替代与影响：**Morde 云端 Task 状态/Hub 页面不是 Dynamic Island 与锁屏 Live Activity 的等价替代。当前缺少该扩展意味着原 Agent Live Activity 展示与 `AudioTogglePlaybackIntent` 控件不可用；核查未发现对应账户、聊天、iSH 或媒体持久数据被清除。恢复 target 需保留原 App Group entitlement，并核验扩展签名、嵌入关系和目标系统版本。
- **状态/交接：**将 `AgentWidgetExtension` 列为 MI-01 原生扩展保留与发行验收缺口；没有将缺失 target 记为已获准删除，也没有擅自恢复工程引用。MI-01/07 状态不变。本轮账号键核验确认服务端按精确用户名查找、注册仅修剪首尾空白；iOS 安装键同样使用该用户名，大小写行为一致，未发现需要更改的账号隔离缺陷。未运行新增构建或测试。

- **后续原工程复验（2026-11-04 13:25）：**以上是较早的只读状态，已被当前工作区后续恢复结果取代。最新 `xcodebuild -list` 与 `morde` build-for-testing 目标图均包含 `AgentWidgetExtension`；原 `Minis.xcodeproj` 产物实际嵌入 `AgentWidgetExtension.appex`、`MinisFileProvider.appex` 与 `MinisShare.appex`，显示名分别为 `morde`、`morde Files`、`Share to morde`。仅在 `/private/tmp/morde-session3-final/Build/Products/Debug-iphonesimulator/morde.app` 为主 App 和三个扩展补签原 App Group entitlement，`codesign --verify --deep --strict` 通过；该产物安装到 `Morde Session 3 UI Clean`（iPhone 18 Pro、iOS 27.0）后成功启动，首屏无 Location 弹窗，截图 `/private/tmp/morde-session3-final/launch-app-group-signed.png`。因此当前不再将扩展缺失记作 MI-01 阻塞。
- **仍未验：**未启动/检查真实 Live Activity 在锁屏、Dynamic Island 的展示与更新；此次为临时 Simulator 切片和 ad hoc 签名，不能代表生产签名，稳定依赖/嵌入路径仍由会话 5 处理。未清除该任务模拟器或任何旧数据，也未删除 target、资源或文件。

### 2026-11-04 Morde 拒绝/停用账号提示修复

- **修复：**`MordeError` 解码服务端 `error.code`，把 `account_pending`、`account_rejected`、`account_disabled` 映射为本地化提示；为未通过审核和账号停用补齐英语、德语、西班牙语、法语、日语、韩语、俄语、简体中文与繁体中文文案。未知错误码继续显示服务端原始消息，兼容旧服务端缺少 `code` 的响应。
- **验证：**`Tests/run-credential-expiry-tests.sh` 通过，覆盖三种状态码映射及未知码；原 `Minis.xcodeproj` 的 `morde` scheme `build-for-testing` 通过；`Morde Session 3 UI Clean`（iPhone 18 Pro、iOS 27.0）本机 URLProtocol fixture 的 `testRejectedAndDisabledAccountsShowLocalizedMessages` 和 `testRegistrationApprovalHubConfirmationAndSignOut` 各通过 1/1。结果包分别为 `/private/tmp/morde-session3-account-status-derived/Logs/Test/Test-morde-2026.11.04_09-57-39-+0800.xcresult`、`/private/tmp/morde-session3-account-status-derived/Logs/Test/Test-morde-2026.11.04_10-02-21-+0800.xcresult`。后一回归用例先因仍断言旧待审原文失败；调整为断言本地化提示后重跑通过。没有请求 Portal 或使用管理员凭据。XCTest 仍报告 `Publishing changes from within view updates is not allowed` 运行时警告。
- **Simulator 依赖交接（会话 5）：**首次 UI 运行时 App 在 dyld 启动阶段崩溃；当前 `project/nanoMuse/android/deps/frameworks/FFmpeg.framework/FFmpeg` 的 `LC_BUILD_VERSION` 为 `IOS`，而 Simulator 需要 `IOSSIMULATOR`。本次只在 `/private/tmp` 原 Minis 构建产物中替换 FFmpeg 与 libav 框架并以原 App Group entitlement 临时签名，随后 UI 用例通过；没有修改仓库依赖或 Session 3 边界外文件。请由 Session 5 核验并提供稳定的 simulator 依赖/嵌入路径，或将该依赖问题登记到集成任务；原 `Minis.xcodeproj` 的 Embed Frameworks 仍指向 `android/deps/frameworks`，所以直接从当前依赖目录启动 Simulator 仍会失败。该跨界事项不阻止继续本边界内账号与聊天工作。
- **状态/边界：**MI-02、MI-07 继续进行中；本地策略已有账号键/outbox 隔离单测，原 App 的跨账号切换和快速重复点击仍未验；真实 Portal 拒绝/停用响应、生产签名、Live Activity 扩展或真实网络目的地也未验。清理确认待办、12 个旧源文件删除状态及 `AgentWidgetExtension` 缺口均未改动。

### 2026-11-04 会话 3 登录隔离与重复提交核对

- **本地覆盖：**`Tests/MordeCredentialExpiryTests.swift` 的 `MordeLoginStateStore` 断言覆盖不同账号/服务得到独立 client instance、较新的登录尝试阻止过期尝试提交凭证、登出轮换安装身份；`MordeClientDataScope`/retry outbox 断言覆盖账号、服务实例及安装不同则不能复用待发送记录。注册与登录方法共享 `busy` guard，UI 提交按钮在 `client.busy` 时禁用。
- **验证：**本轮重跑 `Tests/run-credential-expiry-tests.sh` 通过（登录 fence、账号隔离的 Task 恢复、目标可用性、撤销持久化、凭证、版本、游标、Task 状态与通知检查）；TODO `git diff --check` 和 `Localizable.xcstrings` JSON 校验通过。
- **未完成：**以上是本地策略/状态单测与代码路径核对，尚未在原 App 模拟器中快速双击注册、跨账号登录/登出后检查个人内容缓存，也未向真实 Portal 做注册、审核或登录。MI-02 保持进行中；本轮不改旧功能、target、资源或用户数据。

### 2026-11-04 会话 3 Task SSE 与 Workspace 重试修复

- **SSE 修复：**`watchCurrentTask()` 改用带本机 URLProtocol 的注入 `urlSession`。原实现通过 `AsyncBytes.lines` 等待空行分隔事件；本机探针确认该序列不产出空行，导致事件帧从不处理。现在按原始字节识别 SSE 行/帧边界、限制单行 64 KiB，并在事件后刷新 Task 与消息。DEBUG fixture 增加已完成任务快照和 SSE 响应。
- **未确认发送的幂等重试：**`MordeRetryAttempt` 现在保存可选 `inputRevision`；首次发送把所选 Workspace revision 与幂等键一起持久化，重试沿用原 revision，避免 Workspace 更新后同一幂等键携带不同请求体。可选字段允许旧版无 revision 的待重试记录继续解码；单测覆盖 revision 往返和旧数据解码。服务端 `AppendMessage` 将 revision/context digest 纳入 request hash；同键请求体不同会返回 `idempotency_conflict`，故只把 revision 加入 fingerprint 不足以恢复重试。
- **验证：**原 `Minis.xcodeproj` 的 `morde` scheme `build-for-testing` 通过；`Tests/run-credential-expiry-tests.sh` 通过；`Morde Session 3 UI Clean`（iPhone 18 Pro、iOS 27.0）本机 fixture 的 `MordeRegistrationUITests.testLocalTaskSSEUsesInjectedSession` 通过 1/1，证明 SSE 更新应用到运行中 Task 并显示完成回复。结果包：`/private/tmp/morde-session3-task-sse-fixture-final.xcresult`。测试结束退出合成账号；模拟器没有 reset。构建与运行仅在 `/private/tmp` 用 simulator FFmpeg/libav 产物替换设备版依赖并临时 ad hoc 签名，仓库依赖未改。
- **首次失败及范围：**首轮 SSE UI 失败后确认 `AsyncBytes.lines` 空行行为并修正解析器；修复后的原工程 UI 用例通过。当前只验单个本机 SSE 更新和快照刷新；真实 Portal、断线续读/游标过期、Task 多轮发送、Workspace revision 变化期间的端到端重放仍待验。XCTest 仍有既有 SwiftUI 发布状态运行时警告。MI-03 保持进行中；MI-01/02/07 及未获确认的删除状态不变。

### 2026-11-04 会话 3 恢复原工程 Live Activity 扩展并启动 Simulator

- **改动：**参照 `.local/nanomuse-ios-original` 的原 Minis 工程配置，在当前 `Minis.xcodeproj` 恢复 `AgentWidgetExtension` target、主 App 嵌入与依赖，沿用 `io.github.nanomuse.app.AgentWidget` Bundle ID 和 `group.io.github.nanomuse.app` App Group；扩展显示名及无自定义智能体名时的标记改为 `morde`。在现有 `AudioTogglePlaybackBridge.swift` 中实现 Live Activity 播放切换 Intent 并由扩展与主 App 共用，未恢复或覆盖待确认删除的 `Shared/AudioTogglePlaybackIntent.swift`。同步更新该 Intent 描述在九种语言中的本地化。
- **验证：**原 `Minis.xcodeproj` 的 `morde` scheme `xcodebuild build` 成功，构建图明确包含 `AgentWidgetExtension`；`xcodebuild -list` 显示原 `morde` scheme 和扩展 target；`plutil -lint`、`jq empty` 通过。构建产物检查显示 App/扩展显示名为 `morde`、扩展 Bundle ID 与 WidgetKit extension point 正确、App Group entitlement 保留；ad hoc Simulator 包签名校验通过。`Morde Session 3 UI Clean`（iPhone 18 Pro、iOS 27.0）中原 App 安装副本已启动，`simctl launch` 返回进程；启动前只在 Simulator 安装副本替换设备版 FFmpeg/libav 并重签，没有修改仓库依赖或重置设备。安装前后 App data container UUID 变化；新容器中的 `Library/MinisChat/minis.db` 修改时间早于安装，SQLite `quick_check` 返回 `ok`，会话行数为 0。当前 clean Simulator 没有会话记录可供旧数据逐条对照，未读取聊天正文或发送文件。
- **未完成/风险：**Simulator 未实际创建 Live Activity；未验证真机/生产签名、App Group 容器迁移及旧资料逐项可读。仓库当前 FFmpeg/libav 仍为 device-only，模拟器运行依赖本地临时替换，需会话 5 提供稳定依赖路径。MI-01/07 保持进行中；未确认的 12 项源文件删除和 MI-05/MI-08 清理范围未改动。

### 2026-11-04 会话 3 首次启动定位弹窗与当前 Simulator 链接阻塞

- **修复：**原 `Minis.xcodeproj` 的 `AppDelegate.application(_:didFinishLaunchingWithOptions:)` 每次启动都会无条件创建 `CLBackgroundActivitySession` 并立即失效；此前 fresh Simulator 实际弹出 Location 权限请求。现在该启动清理遵循 `MordeLocalExecutionPolicy.isEnabled`（当前为 false），与 `MinisApp` 中的本机 Agent/后台执行入口一致；未删除定位或后台功能代码。同步将 `Info.plist` 和九种 `InfoPlist.strings` 的用户可见权限说明品牌统一为 Morde，并把 Files 中 FileProvider 根目录显示名改为 `morde`；保留权限用途描述、容器路径和数据。
- **验证：**`Tests/run-credential-expiry-tests.sh` 通过；本轮另跑 `run-backup-secrets-importer-tests.sh`、`run-host-navigation-tests.sh`、`run-legacy-rootfs-archive-tests.sh`、`run-minis-storage-paths-tests.sh`、`run-morde-hub-relay-tests.sh`、`run-morde-portal-navigation-tests.sh`、`run-morde-workspace-transfer-tests.sh`，均退出码为 0（本机临时 harness）。`Info.plist` 与九种 `InfoPlist.strings` 的 `plutil -lint`、改动文件与本文 `git diff --check` 通过；原 `Minis.xcodeproj` 的 `morde` scheme generic iOS Debug build 退出码为 0，未签名；构建产物 App 与嵌入的 `AgentWidgetExtension` 显示名均为 `morde`，简体中文定位说明已编入 App。未对真实 Portal、账号或用户数据发请求。
- **Simulator 交接（会话 5）：**本轮用原 `Minis.xcodeproj` 重建 Simulator 时，fresh 与既有 DerivedData 两种尝试都在链接阶段失败：`android/deps/libs/libish_emu.a[2](emu_tlb.c.o)` 是为 `iOS` 设备构建，不能链接到 `iOS-simulator`。依赖路径超出会话 3 文件边界；请会话 5 提供 simulator slice/可复现构建路径，或在集成任务中处理。此前成功的原 App Simulator 结果仍有效，但本轮定位弹窗修复尚无最新二进制复验，不能标为通过；不修改 `android/deps` 或独立 Morde target。
- **状态/数据边界：**MI-01/07 继续进行中；先前 app data container UUID 变化且新库会话数为 0，仍不足以逐项证明旧资料连续性。本轮未重置或安装覆盖既有用户 Simulator；未确认的旧源文件删除与 MI-05/MI-08 清理范围不变，继续等用户确认。未使用管理员、SSH、API 凭据或发送文件。

### 2026-11-04 会话 3 旧 nanoMuse 网络出口 fail-closed

- **改动：**新增关闭状态的 `MordeLegacyServicePolicy`；`NanoMuseCloud.checkRelay/call`、`NanoMuseProfile` 媒体收发、`NanoMuseNudges`、`NanoMuseUpdateCheck`、`NanoMuseHub.start` 与 `ProviderConfigStore.fetchModelsForInstance` 的旧服务请求现遵循该策略或 `MordeLocalExecutionPolicy`，不请求旧 Cloud、Relay、Hub 或模型发现端点。保留旧 UI、缓存、配置、Keychain 和源文件；Morde API 不受影响。补齐停用提示的九种本地化文案。
- **验证：**`Tests/run-credential-expiry-tests.sh` 通过，断言旧服务策略关闭和错误文案；`Localizable.xcstrings` JSON、改动范围 `git diff --check` 通过；此前用原 `Minis.xcodeproj` 的 `morde` scheme 完成两次 `build-for-testing` 与本地 Simulator 启动。该启动 UI 显示 Morde 欢迎页及云端任务入口，也暴露出每次启动的旧定位权限请求，已在上一条交接修复；本轮最新 Simulator 重建受下述链接问题阻塞。
- **未验及交接：**未向真实 Portal、旧 nanoMuse 服务或任何外部凭据端点发送请求；fail-closed 来源于调用路径检查与本机策略测试，未做网络代理/抓包验证。Simulator 的 `libish_emu.a` 平台不匹配已交会话 5，最新源代码仍需在依赖修正后做原 `Minis.xcodeproj` 安装/启动回归。MI-02/07 保持进行中；没有据此删除旧源文件或清理旧账户、缓存和凭据数据。

### 2026-11-04 会话 3 首次启动定位修复 Simulator 复验

- **构建/依赖：**使用原 `android/src/ios/Minis.xcodeproj` 的 `morde` scheme 构建 iOS Simulator App，未使用独立 `Morde.xcodeproj`。为绕过仓库内 device-only `libish_emu.a`，从 `android/deps/ish` 在 `/private/tmp/morde-session3-ish-sim-20261104` 构建 Simulator 版 iSH 静态库；原工程 Simulator build 成功。Xcode 的 Embed Frameworks 阶段仍把 `android/deps/frameworks` 中 device-only FFmpeg/libav 拷入产物，dyld 明确报告平台为 `iOS`、所需为 `iOS-simulator`。仅在 `/private/tmp/morde-session3-sim-run-20261104-1235/morde.app` 验证副本中换入 `/private/tmp/morde-session3-ffmpeg-sim/frameworks` 的 Simulator FFmpeg/libav 七个 framework，并以临时 App Group entitlement ad hoc 重签；`codesign --verify --deep --strict` 通过。未改 `android/deps` 或项目 Embed 配置。
- **运行结果：**在新建的 `Morde Session 3 Latest Source Check`（iPhone 18 Pro、iOS 27.0）安装该原工程 App；`simctl launch` 返回进程且等待 4 秒后仍运行。首屏显示“欢迎使用 morde”和 Morde 云端任务入口，没有出现 Location 权限弹窗；截图为 `/private/tmp/morde-session3-sim-run-20261104-1235/launch-fixed.png`。App 及嵌入的 AgentWidget、FileProvider、Share 扩展显示名均为 morde 系列，AgentWidget Bundle ID 与 App Group 保留。
- **边界/交接：**fresh Simulator 仅作此次验收，未 reset、覆盖或检查任何既有用户 Simulator 的数据。没有请求真实 Portal/旧服务、使用管理员/API/SSH 凭据或发送文件。MI-01/07 继续进行中：未验证 Live Activity 实际呈现、生产签名和旧资料逐项读取；当前工程 Embed Frameworks 仍指向 device-only FFmpeg/libav，Simulator 运行需临时替换，稳定依赖/嵌入路径仍交会话 5 处理。MI-05/06/08/09 及工作树中待确认的 12 个源文件删除状态不变，未删除代码、target、资源或数据。

### 2026-11-04 会话 3 SSE 快照续读 Simulator 覆盖

- **合同核对：**`GET /api/tasks/{id}/events` 的游标过期路径由服务端以 HTTP 200 SSE `task.snapshot_required` 帧结束流，OpenAPI 也要求客户端读取 Task 快照；合同 fixture 中的通用 `event_cursor_expired` 409 不代表此 SSE 路由会返回 409。因此无需改生产错误处理。现有 `watchCurrentTask()` 已读取 Task 与消息快照，并用快照 `event_sequence` 更新 cursor。
- **新增覆盖：**原工程 `MinisUITests/MordeRegistrationUITests.swift` 新增 `testTaskSSERefreshesSnapshotOnSnapshotRequired`；DEBUG URLProtocol fixture 先发送序号 8 的 `task.snapshot_required`，返回 running 快照，再只接受 `after=8` 并返回完成事件/快照，验证 UI 显示最终回复。fixture 仅在本机测试参数启用，不访问真实 Portal。
- **验证：**原 `Minis.xcodeproj` / `morde` scheme `build-for-testing` 通过；临时 Simulator FFmpeg/libav 切片与 ad hoc 签名仅用于 `/private/tmp` 构建产物，签名深检通过。`MordeRegistrationUITests.testLocalTaskSSEUsesInjectedSession` 与 `testTaskSSERefreshesSnapshotOnSnapshotRequired` 在 `Morde Session 3 Latest Source Check`（iPhone 18 Pro、iOS 27.0）合计通过 2/2，结果包 `/private/tmp/morde-session3-task-sse-regression.xcresult`。`sh Tests/run-credential-expiry-tests.sh` 通过。XCTest 仍报告 `Publishing changes from within view updates is not allowed` 运行时警告。
- **未完成/交接：**只覆盖单事件普通更新及快照后续读；多轮发送、断线重开、未知状态 UI、审批冲突和账号切换端到端仍待验，MI-03 保持进行中。请会话 5 在下一次公共总览汇总中纳入新增 SSE 2/2；本轮不改边界外文档、生产服务端、目标或旧数据，也不执行待确认删除。

### 2026-11-04 会话 3 注册快速重复提交 Simulator 回归

- **覆盖：**在 Debug URLProtocol 注册替身中增加本机 busy fixture：首次响应延后，意外的第二次 `POST /api/auth/register` 会返回可见冲突错误。原工程 `MinisUITests/MordeRegistrationUITests.swift` 新增快速连续点击用例，检查提交入口变为禁用、首次注册显示 pending，且第二次请求不会产生错误。该替身只响应本机 UI 测试 launch argument，不会向 Portal 发请求。
- **验证：**原 `Minis.xcodeproj` / `morde` scheme `build-for-testing` 通过；临时 Simulator FFmpeg/libav 切片与 ad hoc 签名仅用于 `/private/tmp` 构建产物，深度签名校验通过。`MordeRegistrationUITests.testRegistrationSubmitCannotBeRepeatedWhileRequestIsInFlight` 在 `Morde Session 3 Latest Source Check`（iPhone 18 Pro、iOS 27.0）通过 1/1，结果包 `/private/tmp/morde-session3-registration-doubletap.xcresult`；`sh Tests/run-credential-expiry-tests.sh` 通过。XCTest 报告既有 SwiftUI 发布状态运行时警告。
- **未完成/数据边界：**该 UI 用例覆盖单次注册时的重复点击；账号切换后的当前会话可见状态隔离由后续回归单独覆盖。真实 Portal 注册、审批和登录链路未访问，MI-02 保持进行中。没有清理凭据、账户数据、目标、资源或待确认旧源文件。

### 2026-11-04 会话 3 App 账号切换会话与 Task 隔离 Simulator 回归

- **覆盖：**原工程 `MinisUITests/MordeRegistrationUITests.swift` 中 `testSwitchingAccountsDoesNotReuseConversationMessagesOrTasks`；本机 Debug URLProtocol fixture 为账号 A/B 返回不同凭据、主会话、两轮用户/助手历史和 Task，执行 A 登录→退出→重启→B 登录，并确认 B 两轮历史/Task 出现、A 的历史/Task 不残留；对跨账号 Task 详情与 SSE 请求返回 403。这里覆盖历史读取，不等同于用户连续发送两轮的端到端验收；也不覆盖 Feed/Ideas/Goals、本机历史、Workspace 文件或每一种个人资料/outbox 缓存迁移。
- **验证：**原 `Minis.xcodeproj` / `morde` scheme `build-for-testing` 成功；只在 Debug fixture 参数下给 client-login 添加 `X-Morde-Local-Account` 标记，不影响生产请求。为应对本仓库 device-only iSH 与 FFmpeg/libav 切片，Simulator 库搜索路径指向 `/private/tmp/morde-session3-ish-sim-20261104` 与 `/private/tmp/morde-session3-ffmpeg-sim/frameworks`；Embed 阶段复制的 7 个 FFmpeg/libav framework 只在 `/private/tmp/morde-session3-final/Build/Products/Debug-iphonesimulator/morde.app` 替换为 Simulator 切片，并以保留原 App Group 的 ad hoc 签名；`codesign --verify --deep --strict` 通过。原用例首次通过 1/1，扩展 Task 与两轮历史断言后 `testSwitchingAccountsDoesNotReuseConversationMessagesOrTasks` 在 `Morde Session 3 UI Clean`（iPhone 18 Pro、iOS 27.0）通过 1/1，结果包 `/tmp/morde-session3-account-task-multiturn-final-20261104.xcresult`。XCTest 报告既有 SwiftUI `Publishing changes from within view updates` 运行时警告。
- **未完成/交接：**真实 Portal 未访问，模拟器未清除；fixture 证明当前 Morde 两轮会话与 Task 不跨账号显示，不代表更广的个人缓存/本地旧数据逐项可读或所有 Task 操作状态均无跨账号影响。MI-02/03 继续进行中。请会话 5 在下一次公共总览汇总中纳入此 UI 1/1；本轮没有修改边界外文件、清理用户数据、旧源文件、target 或资源，也没有发送文件。

### 2026-11-04 会话 3 未知 Task 状态 Simulator 回归

- **覆盖：**原工程 DEBUG URLProtocol 沿用本机 Task fixture，为 `GET /api/tasks` 与详情返回未知 wire 状态 `future_status`，并以 204 结束 SSE 请求以保持快照稳定。`MinisUITests/MordeRegistrationUITests.swift` 新增 `testUnknownTaskStatusIsVisibleWithoutTaskControls`，在中文界面断言显示“任务 · 状态未知”，并且不显示暂停、继续、取消操作。首次运行仅因断言错误使用英文标题失败；依据 UI 快照改为本地化标题后复跑通过。
- **验证：**原 `Minis.xcodeproj` 的 `morde` scheme `build-for-testing` 成功；在 `Morde Session 3 UI Clean`（iPhone 18 Pro、iOS 27.0）运行该用例通过 1/1，结果包 `/tmp/morde-session3-unknown-task-status-final-pass-20261104.xcresult`。FFmpeg/libav Simulator 切片与 App Group ad hoc 签名只用于 `/private/tmp` 临时构建产物，深度签名校验通过。没有请求真实 Portal 或使用提供的管理员/API/SSH 凭据；XCTest 仍报告既有 SwiftUI `Publishing changes from within view updates` 运行时警告。
- **未完成/交接：**未知状态界面和该状态下操作隐藏已覆盖；连续发送与审批冲突仍未验，MI-03 保持进行中。没有修改边界外文件、清理用户数据、旧源文件、target 或资源，也没有发送文件。

### 2026-11-04 会话 3 SSE 临时故障恢复 Simulator 回归

- **覆盖：**新增本机 Task SSE fixture，第一次 `GET /api/tasks/{id}/events` 返回暂时性 500；`watchCurrentTask()` 按原 cursor 重试后收到 `task.updated`，读取完成快照并显示回复。原工程 UI 用例为 `testTaskSSEReconnectsAfterTemporaryFailure`，不请求真实 Portal。
- **验证：**原 `Minis.xcodeproj` / `morde` scheme `build-for-testing` 成功；在 `Morde Session 3 UI Clean`（iPhone 18 Pro、iOS 27.0）通过 1/1，结果包 `/tmp/morde-session3-task-sse-reconnect-final-20261104.xcresult`。仅替换 `/private/tmp/morde-session3-final` 构建产物中的 Simulator FFmpeg/libav 切片并保留原 App Group ad hoc 签名，深度签名校验通过。XCTest 报告既有 SwiftUI 发布状态警告。
- **未完成/交接：**覆盖一次 HTTP 暂时失败后的重试，不代表长时间离线或真实服务负载下的全部恢复策略。连续发送与审批冲突仍待验；本轮不改边界外文件或任何用户数据，也不执行待确认删除。

### 2026-11-04 会话 3 接受的 Task 在 App 重启后恢复 Simulator 回归

- **覆盖：**本机 Task fixture 保持 Task 为 `running` 并对 SSE 返回 204；UI 用例 `testAcceptedTaskIsRestoredAfterAppRelaunch` 登录后等待运行中 Task，终止并重新启动原工程 App，再确认无需重新登录即可从服务端恢复该运行状态。
- **验证：**原 `Minis.xcodeproj` / `morde` scheme `build-for-testing` 成功；在 `Morde Session 3 UI Clean`（iPhone 18 Pro、iOS 27.0）通过 1/1，结果包 `/tmp/morde-session3-task-relaunch-final-20261104.xcresult`。Simulator FFmpeg/libav 临时切片与 App Group ad hoc 签名仅用于 `/private/tmp` 构建产物，深度签名校验通过。XCTest 报告既有 SwiftUI 发布状态警告。
- **未完成/交接：**证明 App 进程重启后本机替身仍返回的已接受 Task 可恢复；真实 Portal 持续运行、设备强制退出期间的服务端执行及生产签名未验证。连续发送与审批冲突仍待验；本轮未清理任务数据、账号数据、旧源文件、target 或资源。

### 2026-11-04 会话 3 Xcode 打开路径排查

- **原因：**用户看到的 Xcode 原先打开 `.local/nanomuse-ios-original/android/src/ios/Minis.xcodeproj` 参考副本，当前 Scheme/Target 为 `Minis`，General 配置的显示名仍是 `nanoMuse`；原 iPhone 18 Pro Simulator 正运行这个旧 App，因此 UI 继续显示旧聊天内容。该参考副本不是本会话的实施目录。
- **核验与处理：**本会话源代码直接修改在 `project/nanoMuse/android/src/ios/**`，TODO 在本文。`xcodebuild -list` 对该目录的 `Minis.xcodeproj` 显示正式 App target/scheme 为 `morde`，其 `INFOPLIST_KEY_CFBundleDisplayName` 是 `morde`。已在 Xcode 打开工作区中的该工程；当前活动 Scheme 为 `morde`。Xcode 窗口标题仍显示 `Minis.xcodeproj` 是工程名，不代表 App 显示名。
- **数据保护/状态：**没有在原 iPhone 18 Pro Simulator 上重新安装或运行，以免影响其中的旧 nanoMuse 安装和本地聊天数据；此前 Morde 首屏验证来自新建的 `Morde Session 3 Latest Source Check`。新旧包沿用同一原 Bundle ID/App Group，重新运行可能访问既有沙盒和共享容器；本会话尚未在该设备覆盖运行以确认旧资料迁移安全。MI-01/07 继续进行中，未删除代码、target、资源或数据，也未发送文件。

### 2026-11-04 会话 3 Task 连续发送与审批冲突 Simulator 回归

- **修复：**Task 发送后列表刷新期间不再清空仍有效的模型选择；刷新中 `selectedTextModelReady` 仍为 false，只有列表成功/失败后才根据当前可用模型清除无效选择。这样第二条消息不会因正常刷新被禁用。
- **覆盖：**原工程 UI 用例 `testSequentialTaskSendsStayInTheSameConversation` 用本机 URLProtocol fixture 按真实发送内容返回两轮用户/助手消息及两个完成 Task，并让第二个 Task 的 SSE 以 204 正常结束；内容标记仅在 Debug 测试参数下附加给本机 fixture。`testApprovalConflictRefreshesTaskAndHidesStaleControls` 验证过期审批 POST 返回 409 后读取完成快照并隐藏旧操作入口。
- **验证：**原 `Minis.xcodeproj` / `morde` scheme 最终 `build-for-testing` 成功；在 `Morde Session 3 Latest Source Check`（iPhone 18 Pro、iOS 27.0）最终连续发送 UI 1/1、审批冲突 UI 1/1 通过，结果包分别为 `/private/tmp/morde-session3-sequential-send-final-20261104.xcresult` 与 `/private/tmp/morde-session3-approval-conflict-verified-20261104.xcresult`。Simulator 所需 FFmpeg/libav 切片和 ad hoc App Group 签名只作用于 `/private/tmp` 构建产物，`codesign --verify --deep --strict` 通过；仓库依赖与项目 Embed 配置未改。
- **范围/未验：**只使用本机合成账号与 URLProtocol fixture，没有访问真实 Portal、远端设备或使用提供的管理员/API/SSH 凭据，也未发送文件。真实服务连续发送、生产签名、旧本地资料逐项读取和长期网络故障仍待验；XCTest 仍报告既有 SwiftUI `Publishing changes from within view updates` 运行时警告。本轮没有删除或恢复旧源文件、target、资源、账号数据或模拟器数据；MI-05/06/08/09 的确认边界维持不变。

### 2026-11-04 会话 3 Morde 本地历史只读入口与 ChatStore 读取验证

- **改动：**Morde 对话页新增“所有聊天”入口，复用 `NanoMuseLegacySessionsList` 的原有账号可见性筛选，并在 `NanoMuseLegacySessionArchive` 中只读展示 ChatStore 历史；不提供发送、重试或工具执行操作。仅在 Debug 启动参数 `-morde.localLegacyHistoryFixture` 下插入合成测试记录，Release 与常规 Debug 不写入该记录。
- **验证：**原 `Minis.xcodeproj` / `morde` scheme `build-for-testing` 成功；`MordeRegistrationUITests.testLocalHistoryArchiveRemainsReadableFromMorde` 在 `Morde Session 3 Latest Source Check`（iPhone 18 Pro、iOS 27.0）通过 1/1，验证入口、列表、用户消息和助手消息可读，结果包 `/private/tmp/morde-session3-local-history-ui-20261104.xcresult`。对原 iPhone 18 Pro（设备 UUID `F90BDF50-B69C-407D-8DDE-08D820AB47A6`）仅只读打开 `Library/MinisChat/minis.db`；SQLite 表检查和 `SELECT` 成功，记录为 2 个 session、23 条 message；未读取消息正文。
- **数据边界/未验：**原模拟器当时处于 Booted 状态，`simctl clone` 拒绝克隆运行中的设备；没有关闭、覆盖安装或重置它，故实际旧数据库升级后的 UI 可读性仍未确认。本轮 URLProtocol/ChatStore 测试记录为合成数据，不上传、不拷贝原模拟器内容，也没有清理任何数据。模拟器 FFmpeg/libav 仅在 `/private/tmp` 使用替代切片与临时 App Group 签名；真实 Portal 和生产签名未验。

### 2026-11-04 会话 3 Morde Library 入口接入与分类浏览验证

- **改动：**在 Morde 对话列表增加 Library 导航入口；确认原 `Minis` 产品嵌入式 Morde 对话布局没有独立 Morde 标签栏，因此不增加不可达标签。复用原 `NanoMuseLibraryRoom` 的本机 workspace 扫描、Morde personal-data 云端附件视图和账号作用域，不重复实现文件读写。Library 在 Morde 路径隐藏 nanoMuse 壳专用菜单，避免跳转到未接入的旧页面；原 nanoMuse Library 菜单行为不变。
- **验证：**原 `Minis.xcodeproj` / `morde` scheme generic iOS Debug build（`CODE_SIGNING_ALLOWED=NO`）和 Simulator `build-for-testing` 均成功；`MordeRegistrationUITests.testLocalHistoryArchiveRemainsReadableFromMorde` 在专用 `Morde Session 3 Latest Source Check`（iPhone 18 Pro、iOS 27.0）通过 1/1，覆盖合成 ChatStore 历史读取、Morde 对话页 Library 入口、Artifacts/Media/Cloud 分类控件显示和退出账号；结果包 `/private/tmp/morde-session3-library-browser-ui-retry2-20261104.xcresult`。原 target Simulator 的 `NanoMuseMordePersonalDataTests` 与 `NanoMuseMordeContentPagesTests` 共 13/13 通过（outbox 账号隔离/冲突、附件分块与校验、内容契约和不含本机路径），结果包 `/private/tmp/morde-session3-personal-data-contracts-20261104.xcresult`。Xcode UI 测试关闭并行克隆后在专用模拟器运行；未使用独立 Morde 工程。
- **数据边界/未验：**仅打开合成历史和 Library 分类控件；没有上传、下载、分享文件或访问真实 Portal。未逐条验证本机资料预览、cloud attachment 的真实下载/上传、personal-data 全量同步和真实服务权限；既有 SwiftUI `Publishing changes from within view updates` 警告仍出现。原始数据模拟器没有安装、关闭或重置，真实 ChatStore 中原有 2 个会话和 23 条消息只读状态不变；未删除任何旧功能、资源、target 或用户数据，也未发送文件。

### 2026-11-04 会话 3 默认 Portal 只读路由检查

- **检查：**对用户指定的 Portal 曾仅发起无认证 GET：根路径返回 200，页面标题为“Morde · PC 用户端”；`/api/capabilities` 与 `/admin/login` 返回 404。后续依据总入口及本机源码只读诊断，确认 `/api/capabilities` 是已注册 API 路由，`/admin/login` 走 Admin SPA 回退，且仅对 `Accept: text/html` 返回页面；先前探测请求的 headers/path 与部署反代信息未知，不能据此判断远端路由缺失。
- **边界/交接：**没有新增远端请求、注册/登录或管理员操作；未使用提供的管理员/API/SSH 凭据，也未发送文件。会话 5 已记录需核对部署/反向代理与服务版本；会话 2 负责 Admin SPA。完成只读部署诊断后再验真实注册审批与客户端登录。MI-02/03/07 的本机替身验证仍有效，真实服务链路保持待验。

### 2026-11-04 会话 3 Library 最新源码收尾回归

- **构建/验证：**在原 `Minis.xcodeproj` / `morde` scheme 对当前源码完成 Simulator `build-for-testing` 和 generic iOS Debug `build`，均成功；构建产物显示名为 `morde`、Bundle ID 为 `io.github.nanomuse.app`，保留并构建 `AgentWidgetExtension`、`MinisShare`、`MinisFileProvider`。`MordeRegistrationUITests.testLocalHistoryArchiveRemainsReadableFromMorde` 在专用 iPhone 18 Pro / iOS 27.0 测试模拟器通过 1/1，结果包 `/private/tmp/morde-session3-library-final-current-20261104.xcresult`；覆盖合成历史读取、Morde Library 分类控件和退出账号。`NanoMuseMordePersonalDataTests` 与 `NanoMuseMordeContentPagesTests` 此前在原工程共 13/13 通过，结果包 `/private/tmp/morde-session3-personal-data-contracts-20261104.xcresult`。`plutil -lint` 与本会话文件 `git diff --check` 通过。
- **数据边界/待验：**只在专用合成数据模拟器上安装和运行；结束后 `simctl list devices booted` 仅显示原始数据模拟器 `F90BDF50-B69C-407D-8DDE-08D820AB47A6` 仍 Booted，本轮未在该设备安装、关闭、重置或读取正文。测试使用 `/private/tmp` 的 simulator iSH/FFmpeg/libav 切片与临时 App Group ad hoc 签名，没有修改仓库依赖。XCTest 仍报告既有 SwiftUI 发布状态警告；真实 Portal、旧资料逐条预览、Cloud 附件真实传输、生产签名及设备依赖仍待会话 5/服务端路由修复后验收。本轮没有删除或恢复旧源文件、target、资源、账户/模拟器数据，也没有发送文件。

### 2026-11-04 会话 3 Task 媒体预览与保存确认 UI 本机替身验收

- **实施：**原 `Minis.xcodeproj` / `morde` 的个人 Task 与项目会话审批统一复用本地 Quick Look；只有服务公布 `task_media_generation` 且待审请求带有效 operation ID 时，才按冻结 OpenAPI 精确路径取预览。预览校验账号范围、MIME、`private/no-store`、`nosniff` 与大小；能力开启时保存/拒绝都在预览页明确确认，保存仅提交带版本的审批决定；能力关闭时不取媒体字节、隐藏预览和保存，只能拒绝。账号退出、预览关闭、审批状态变化或下载完成后发现请求已失效时清理临时文件。
- **构建/验证：**credential/Task response helper 的自检通过；原 `Minis.xcodeproj` / `morde` scheme 最新 Simulator `build-for-testing` 和 generic iOS Debug build 均通过；专用 iPhone 18 Pro / iOS 27.0 测试模拟器中，`MordeRegistrationUITests.testTaskMediaPreviewRequiresSaveDecisionAndConfirmsWorkspaceSave` 与 `testTaskMediaApprovalStaysClosedWithoutCapability` 共 2/2 通过，结果包 `/private/tmp/morde-session3-task-media-ui-final3-20261104.xcresult`。产物显示名 `morde`、Bundle ID `io.github.nanomuse.app`。`jq` 本地化目录、`plutil` 原工程文件检查通过。
- **数据边界/未验：**仅用进程内 URLProtocol 与本机合成 PNG；没有访问 Portal、使用管理员/API/SSH 凭据、上传/发送任何文件，审批请求也未离开本机。原始数据模拟器 `F90BDF50-B69C-407D-8DDE-08D820AB47A6` 继续 Booted 且未触碰。服务端 `task_media_generation` 仍关闭；真实 provider、Token 预留/结算、幂等恢复、Workspace artifact 发布和真实媒体读取未验，不能据本地 UI 结果启用该 capability。没有删除/恢复任何旧源文件、target、资源或数据；MI-05/MI-08 仍待用户确认。

### 2026-11-04 会话 3 MI-07 接入稳定 Simulator 依赖并复验

- **实施：**只改原工程 `project/nanoMuse/android/src/ios/Minis.xcodeproj/project.pbxproj`：七个 FFmpeg/libav 引用改为会话 5 生成的 `android/deps/build/morde-simulator/dependencies/frameworks/*.xcframework`，复用原链接及 Embed Frameworks 阶段；新增 `iphonesimulator` 条件下的 iSH/FFmpeg headers 与静态库搜索路径，设备 SDK 继续使用原 `deps/libs`。没有改动会话 5 的 `android/deps/**`，没有新增 target。
- **构建/平台验证：**原 `Minis.xcodeproj` 的 `morde` scheme generic iOS Debug `build`、Simulator `build-for-testing` 均成功；这次没有 `/private/tmp` 库路径覆盖、框架替换或临时重签。Xcode 生成的 App 显示名 `morde`、Bundle ID `io.github.nanomuse.app`。七个嵌入框架的 Simulator 二进制均由 `vtool` 确认为 `IOSSIMULATOR`、最低 iOS 16.0；`xcodebuild -list` 可解析原工程和 `morde` scheme；Simulator `-showBuildSettings` 实际解析到生成的 iSH include/libs 及 arm64-simulator FFmpeg headers。
- **Simulator UI 验证：**原工程 Simulator `test-without-building` 中 `MordeRegistrationUITests.testTaskMediaPreviewRequiresSaveDecisionAndConfirmsWorkspaceSave` 与 `testTaskMediaApprovalStaysClosedWithoutCapability` 在 `Morde Session 3 Latest Source Check`（iPhone 18 Pro、iOS 27.0）通过 2/2；结果包 `/private/tmp/morde-session3-simulator-dependencies-20261104/Logs/Test/Test-morde-2026.11.04_19-39-52-+0800.xcresult`。原 `testRegistrationApprovalHubConfirmationAndSignOut` 在该设备及新建的 `Morde Session 3 Stable Dependencies`（iPhone 18 Pro、iOS 27.0）三次都在 `morde.registration.pending` 断言失败；此前服务地址默认值和“不含邀请码/短信”的断言已通过，错误快照显示本机提示“两次输入的密码不一致”。尝试改用纯字母数字的同一测试口令未改变结果，已撤回该无效改动。失败结果包 `/private/tmp/morde-session3-simulator-dependencies-registration-20261104.xcresult`、`/private/tmp/morde-session3-simulator-dependencies-registration-clean-20261104.xcresult`、`/private/tmp/morde-session3-simulator-dependencies-registration-retry-20261104.xcresult`。该项仍未验收，需继续排查测试输入与 SecureField 状态。
- **数据/权限边界：**承载原 ChatStore 的 `F90BDF50-B69C-407D-8DDE-08D820AB47A6` 始终 Booted，未安装、关闭、重置或读取；新建测试 Simulator 只装载本地原工程产物。注册 UI 用本机 URLProtocol 替身，未访问 Portal，也未使用管理员、SSH 或 API 凭据；没有上传/发送文件或删除/恢复旧代码、target、资源和用户数据。生产签名、真实 Portal 网络目的地、既有本机数据覆盖安装连续性及 Live Activity 实际展示仍未验；MI-05/MI-08 删除确认边界不变。

### 2026-11-04 会话 3 MI-07 注册 UI 根因修复与最终验收

- **根因/改动：**前述三次注册 UI 失败来自 iOS 27 在 `.newPassword` SecureField 聚焦时弹出的系统强密码 AutoFill 面板；该面板拦截 XCTest 输入，导致测试中的密码值不一致。`MinisUITests/MordeRegistrationUITests.swift` 现于两个新密码字段获得焦点后检测 `GenerateStrongPasswordButton` 并关闭系统面板，再输入测试口令。未改生产注册/认证逻辑。
- **最终验证：**原 `Minis.xcodeproj` / `morde` scheme 当前源码 Simulator `build-for-testing` 成功；`MordeRegistrationUITests.testRegistrationApprovalHubConfirmationAndSignOut` 与 `testRegistrationSubmitCannotBeRepeatedWhileRequestIsInFlight` 在 `Morde Session 3 Stable Dependencies`（iPhone 18 Pro、iOS 27.0）通过 2/2，结果包 `/private/tmp/morde-session3-simulator-dependencies-registration-fixed-20261104.xcresult`。结果摘要为 `Passed`，0 失败；仍有既有 SwiftUI `Publishing changes from within view updates` 运行时警告。此前三份失败结果包保留作根因诊断历史，不代表最终回归状态。
- **验收边界：**MI-07 的原工程构建、安装/启动及本机替身注册 UI 验收完成；此前媒体审批 UI 2/2、generic iOS Debug build、七个 Simulator slice、显示名 `morde` 与默认 Portal 只读检查均已记录。测试没有访问真实 Portal、使用管理员/API/SSH 凭据或发送文件。真实 Portal 注册审批和反代仍由 MI-02/03 跟进；生产签名、Live Activity、原数据模拟器覆盖安装连续性仍未验证，分别留在 MI-01/未完成验收中。`F90BDF50-B69C-407D-8DDE-08D820AB47A6` 继续 Booted，未安装、关闭、重置或读取；没有删除/恢复旧源文件、target、资源或用户数据，MI-05/MI-06/MI-08 的确认范围不变。

### 2026-11-04 会话 3 默认 Portal 路由二次只读检查

- **检查：**对 `https://portal.120.25.178.93.sslip.io` 并行发起无认证 GET：`/` 返回 200 HTML，`/admin/login` 在 `Accept: text/html` 下返回 200 HTML，`/api/capabilities` 在 `Accept: application/json` 下返回 404 JSON，远端 IP 为 `120.25.178.93`。这次只验证路由，没有注册账号、登录、审批或改动服务器数据；没有使用管理员/API/SSH 凭据，也没有上传/发送文件。
- **交接：**本机会话 5 的服务源码已注册 `/api/capabilities`，因此远端 404 需会话 5 核对部署版本和反向代理路径；会话 2 可确认 Admin SPA `/admin/login` 回退。MI-02/03 真实注册、登录、Task/SSE 验收保持待办；本机 URLProtocol UI 测试不代表真实服务链路通过。

### 2026-11-04 会话 3 MI-05/MI-08 删除候选清单提交确认

- **清单：**MI-05 的旧 Cloud/Relay、Hub/Presence、CloudKit、iSH/本机 Agent、NativeOffloads、Provider Key、品牌资源及独立 Morde target 候选，和 MI-08 的 Feed/Ideas/Goals 导航、页面与调用点，均在上方列出具体位置、Morde 替代能力及本地/云端数据影响。
- **状态/边界：**已向用户提交按清单类别逐项确认的请求。MI-05/MI-08 清单工作完成；MI-06/MI-09 继续阻塞，等待明确授权。没有删除或恢复任何旧源文件、target、资源或数据；工作树已有删除状态仍未视为授权。

### 2026-11-04 会话 3 MI-05/MI-08 候选审查建议

- **MI-05 建议：**暂缓删除旧 Cloud/Relay、旧 Hub、CloudKit、iSH/本机 Agent、NativeOffloads、Provider API Key 代码和独立 Morde target。Morde `/api/capabilities` 当前远端返回 404；Morde Hub 只覆盖 info/open/notify；personal-data 全量同步、旧账号/Relay 历史导出、本机文件/iSH 替代、Key 导出及 `io.morde.app` 数据容器迁移均未验收。保持旧服务入口 fail-closed，保留本地数据、Keychain、OAuth、回调及共享代码；待对应替代/导出和回退条件通过后再提精确删除集。MCP OAuth 注册身份已改为 morde；旧设置中的政策/帮助 URL 暂无 Morde 对应页面，更新检查代码仍 fail-closed，不应猜测替代地址。保留 URL scheme、OAuth callback、分享/文件扩展路径和未引用资源，直到全量引用检查完成。
- **MI-08 原建议（已由用户缩小授权）：**当时建议还包括停止 Feed/Goal 自动化；后续用户明确只授权移除 Feed/Ideas/Goals iOS 页面和入口，因此本轮不改 Routine/Feed 自动运行行为。数据、outbox、stores、备份和服务端 API 继续保留。
- **当前授权状态：**用户确认的范围仅为三个板块的 iOS 页面和入口。MI-06 仍阻塞，旧服务/数据代码和 target/resource 清理未获授权；MI-09 只按页面/入口范围实施。

### 2026-11-04 会话 3 Live Activity 迁移缺口交接

- **核对结果：**`AgentLiveActivityManager` 的调用目前来自 `Agent`/`BackgroundKeepAliveManager` 和 App 生命周期清理；`Morde/MordeApp.swift` 没有 ActivityKit 调用。原工程会构建 `AgentWidgetExtension`，但这不证明 Morde 云端 Task 会显示或更新 Live Activity。iOS 迁移方案指出服务端尚无 APNs 投递合同；本机 `server/openapi/portal.yaml` 和相关 API/App 目录未发现 Live Activity / push-token 路由。
- **建议/交接：**保留现有原生 ActivityKit 代码和扩展，不删除、不将 Morde Task 伪装成本机 Agent。若产品要求云端 Task 在锁屏显示进度，需会话 5 先定义设备 push-token 注册/撤销、ActivityKit 更新/结束推送及授权合同，再由会话 3 接入 Morde Task/SSE；普通 APNs 通知不等于 Live Activity push。当前仅能记录扩展构建成功，模拟器实际呈现、后台更新和生产签名未验，MI-01 保持进行中。

### 2026-11-04 会话 3 Morde MCP OAuth 品牌元数据与旧服务网络门禁复核

- **改动：**`NanoMuseMcpAuthDiscovery` 的 `clientName`/`clientURI` 和注册拒绝提示改为 `morde` 与用户指定 Portal；OAuth redirect URI、已保存 client ID/secret 和 Keychain 未改。
- **验证：**原 `Minis.xcodeproj` / `morde` scheme 在 `Morde Session 3 Stable Dependencies`（iPhone 18 Pro、iOS 27.0）运行 `MinisTests.NanoMuseLogicTests.testMCPOAuthRegistrationUsesMordeClientMetadata` 通过 1/1；结果 `/private/tmp/morde-session3-mcp-brand-signed-20261104.xcresult`。签名使用仅限 Simulator 的 ad hoc 身份以携带 App Group entitlement；首轮 `CODE_SIGNING_ALLOWED=NO` 因缺少该 entitlement 在 `MinisStoragePaths` 初始化崩溃，修正签名后复跑通过。2026-11-04 20:39 使用原工程同一 scheme 的 `generic/platform=iOS` Debug build 退出码 0。构建仍报告仓库既有 warnings：缺少 `android/build/frameworks` 搜索目录、旧 C/Objective-C nullability 与 Swift unused-result/Sendable/deprecation 提示、AppIcon catalog 6 项未分配；本次新增 metadata/test 没有相关 warning。
- **网络边界复核：**`NanoMuseRoot`/旧 Settings 仍保留 `NanoMuseUpdateCheck.checkIfStale()` 调用点，但 `NanoMuseUpdateCheck.fetchLatest()` 在任何 URLSession 前检查 `MordeLegacyServicePolicy.isEnabled`；该策略为 false。`NanoMuseCloud.call` 与 Relay media JSON/raw 请求同样在 URLSession 前 fail-closed。因此旧域名常量和旧 UI 代码仍在源文件，但上述调用路径不会在 Morde target 发出旧服务请求。本轮没有登录外部 MCP、创建 OAuth client、访问 nanoMuse 域名或使用管理员/API/SSH 凭据。

### 2026-11-04 会话 3 源码写入权交接

- **协调要求：**收到共享工作区另一会话的唯一集成者交接指令，已暂停对 `project/nanoMuse/android/src/ios/**` 的源码、测试与工程文件写入；不回滚、不清理、不提交。现仅更新本会话 TODO 交接，已释放该源码树的写入权，等待共享工作区确认后再恢复。
- **本轮最后改动：**原 Minis/Morde 壳已有 Chat/Library-only tab 与失效 room 路由回 Chat；本轮补上 tab accessibility identifiers，在 Agent 日常页隐藏 Goal-check 行、系统文件页隐藏 Feed preference 入口并缩减 HEARTBEAT 展示，移除历史 Fence 卡片的 Feed/Goals 页面按钮。`MordeRegistrationUITests.swift` 改为检查 Chat/Library 与切换；`IOS04CloudAcceptanceUITests.swift` 去掉依赖 Feed/Ideas/Goals 页的 UI 流程。此前正在开发的 `NanoMuseLegacySessionArchive.swift` / `MordeApp.swift` 纯文本历史导入和 `NanoMuseLogicTests.swift` 覆盖仍在工作树；均未在本轮重建或跑 Simulator。
- **最后验证：**当前这些改动的定向 `git diff --check` 无错误，原 `Minis.xcodeproj/project.pbxproj` 的 `plutil -lint` 显示 `OK`。本轮 `xcodebuild -list` 没有完整结果；本轮源码构建、单测和 Simulator UI 验收均未完成。不得引用 2026-11-04 20:39 的旧 generic build 作为这些新改动的验证。
- **未提交基线：**交接时 `git status --short -- android/src/ios` 显示大量既有修改、未跟踪的原 `Minis.xcodeproj`/共享 Morde 源码和测试，以及既有 12 个删除状态（其中含 FileTools/ISHCommand、NativeOffloads 和 AudioTogglePlaybackIntent）；这些状态不能视为本轮产生或获准删除。未 commit、reset、restore、clean、触碰用户 Simulator 数据，未访问真实 Portal，也未发送任何文件。
- **恢复工作范围：**待确认重新获得写权限后，继续只用原 `Minis.xcodeproj` / `morde` scheme；本次用户授权只覆盖 Feed/Ideas/Goals 页面与入口，不包括停用 routine/Feed 自动触发或清理任何服务、数据、target、资源。MI-09 尚待构建与 Simulator 验收，MI-04 历史导入也尚待原工程验证。

### 2026-11-05 会话 3 登录后聊天优先首屏

- **改动：**`NanoMuseRoot.needsSetup` 在 `MINIS_CLOUD_ONLY` target 不再呈现旧的 Provider/模型首次配置页；Morde 登录页直接使用默认 Portal origin，不要求用户输入服务地址。已登录后 `MordeClient.load()` 先 GET `/api/me/main-conversation`，仅在状态为 `uninitialized` 且 Task API 可用时 POST `/api/me/main-conversation/ensure`，之后在聊天主页显示唯一主会话；模型列表就绪后自动选择首个同时支持文本和工具的模型。现有侧栏过滤主会话，保留其他云端会话与本地历史入口；未移除代码、target、数据或页面。保留图像、视频、语音生成和 Task 图片上传 fail-closed 策略。可见首屏/聊天名称使用 `morde`，技术文件名和内部旧标识不做全局替换。
- **验证：**`xcodebuild -list` 识别原 `Minis.xcodeproj` 和 `morde` scheme；`xcodebuild test` 成功完成全量编译，独立 `Morde S3 Chat First UI 2026-11-05` Simulator（iPhone 18 Pro、iOS 27.0）中 `MordeRegistrationUITests.testFreshLaunchOpensMainChatAndSideConversationsStayInDrawer` 通过。结果 `/private/tmp/morde-s3-chat-first-20261105.xcresult`：`xcresulttool get test-results tests` 显示测试计划、suite、case 均 Passed（case 107.775 秒）。Xcode 随后卡在 test log finalize/coverage cleanup，按协调要求停止等待；Ctrl-C 后命令退出码 75 并打印 `TEST INTERRUPTED`，因此命令级清理完成状态未验。运行时报告既有 “Invalid frame dimension (negative or non-finite).” warning。
- **未验证/交接：**本机 URLProtocol fixture 验证的不是 Portal 实际登录和反向代理；首次真实登录仍需会话 5 确认部署 `/api/capabilities` 及会话路由。保留默认 Portal URL，仅从登录表单移除服务地址编辑字段；若部署 origin 需切换，仍可走原设置/服务层配置路径。此次只验证 UI flow 与默认文本/工具模型选择，不验证图、视频、语音生成端到端；上传仍关闭。未使用线上凭据/API key、没有远端请求或数据改动，也未安装到/重置承载旧 ChatStore 的模拟器、未发送文件。
