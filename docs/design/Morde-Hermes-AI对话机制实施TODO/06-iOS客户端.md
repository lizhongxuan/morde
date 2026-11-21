# H06 iOS 客户端

> 状态：待验证。H06-1～H06-3 已完成；H06-4 的消息状态、会话切换、重试及图片/PDF 上传、聊天回显、聊天与文件库预览本地 Simulator 专项均已通过。真实 Portal/DSH/Guardian 及同账号 PC+iOS 跨端接续尚未验证。H01–H04 的本地专项实现和协议联调已有记录；完整跨栈验收仍需隔离本地 Portal、独立 DSH Docker endpoint、Guardian 和可用的测试模型账号。先阅读[总览与协作契约](00-总览与协作契约.md)。

## 修改边界与接口

独占 `clients/nanoMuse/android/src/ios/Morde/` 内对话消息、状态映射与本地化相关文件，以及对应的 Swift/UI 测试。不要修改 PC、服务端或 DSH Runtime 文件。

默认沿用现有消息 API 和 `routing_status`、`delivery_status`、`task_id` / `related_task_id`。`pending` 不表示模型已领取，`applied` 不表示任务完成，`unknown` 需要对账；若 H03 确认必须扩展公开契约，再适配已合入的字段。

## 任务

- [x] **H06-1（完成）支持运行中继续对话**：在同一会话发送普通问题、任务补充和独立请求；保持消息、Task 状态和结果归属。失败或不可确认时保留原文和真实状态。验证：Simulator 覆盖顺序发送、运行中补充、独立任务及失败路由原位重试。
- [x] **H06-2（完成）重试与恢复**：关闭、重开或断网恢复后读取服务端消息与任务事实；重试同一消息使用原幂等 ID，不因客户端重启取消已受理任务或重复创建任务。验证：丢回执重试、断网重开恢复和已受理消息重开对账均通过；重试保留原 client message ID 和原文。
- [x] **H06-3（完成）完成本地化与状态映射**：保持现有多语言规范，不把等待或 unknown 显示成已完成；只展示用户需要的状态和任务详情入口。验证：状态策略单测通过；修复全量目录检查发现的 47 条缺译，并补齐本轮调整的两条目录项，等待、发送中、unknown、失败及成功状态均覆盖 9 种语言。
- [ ] **H06-4（待验证）验证与交接**：本地替身已覆盖消息状态、账户/会话切换和重试。H01–H04 已完成本地专项实现及协议联调；真实 Portal 验收尚未运行。当前本机没有 Portal、独立 DSH Docker endpoint 或 Guardian，不能安全启动真实消息路由。待隔离本地栈可用后运行 `IOS03SimulatorAcceptanceUITests.testC10PortalMessagePersistsAcrossAppRestart`，并在 `morde-pc` 与 iOS 中验证同账号接续同一会话。

## 尽量移植hermes-agent/项目的代码
Hermes-agent有很多写好的函数,特别是在harness这一块都是比较强的,能移植过来的话,能减少工作量.

复用核查：检查了本地 Hermes gateway 的幂等、会话授权和 admission 实现；这些实现属于 Python 服务端，未发现可直接移植的 Swift 客户端代码。Morde iOS outbox 已有稳定 client message ID 与回执对账机制，本次只修正其调度边界，没有复制另一套实现。


## 完成证据

构建入口见 [iOS 开发文档](../../../clients/nanoMuse/docs/ios.md)。记录 iOS 构建、Simulator/设备、登录与交互步骤及结果；PC/iOS 同账号跨端结论由真实两端联合验证。只做 Swift 单测或编译时标记待验证。

- 本地构建/单测：2026-11-21 重跑 `swift test`（`clients/nanoMuse/android/src/ios/Morde`）77/77 通过；`sh android/src/ios/Tests/run-credential-expiry-tests.sh`（`clients/nanoMuse`）Release 与 Debug 检查通过。
- Simulator：Xcode `Morde` scheme，iOS Simulator `Morde C20 T3 Local UI 20261110`，本地 fixture；定向运行 9 项 `IOS03SimulatorAcceptanceUITests`，9 项通过（连续发送、运行中补充、独立消息、丢回执重试、断网重开、接受回执重开、会话草稿/附件隔离、账户切换、失败路由重试）。结果包：`/tmp/MordeH06Run1.xcresult`。
- 2026-11-21 额外复核：运行中任务追加文字、丢回执后附件重试及图片/PDF 完整布局上传 UI 用例通过。首次运行 `testMordeChatComposerQueuesImageAndPDFAttachmentsOnActiveTask` 时，fixture 已接受消息并返回合成回复，但消息行只显示附件 ID。根因是本地附件 fixture 缺少 `/api/sync/changes` 和个人文件项 GET；已补齐增量变更与当前文件项读取，未放宽产品展示逻辑。旧失败结果 `/tmp/MordeH06AttachmentRun1.xcresult` 保留为根因前基线。
- 2026-11-21 关闭代码签名的 Simulator 登录失败不是应用缺陷：未签名构建缺少 Keychain access-group entitlement，导致 Keychain 写入返回 `-34018`，并使登录回滚；iOS 开发文档明确说明此构建不能用于 Simulator Keychain 登录验收。恢复签名后，`testMordeChatComposerQueuesImageAndPDFAttachmentsOnActiveTask` 通过（1/1），覆盖登录、任务目标、图片/PDF 名称回显、发送、运行中追加和停止任务。测试环境：iOS 27.2 Simulator `E1C86EF7-3EB6-4348-9233-7C197A7E3490`。结果包 `/tmp/morde-h06-signed-derived/Logs/Test/Test-Morde-2026.11.21_18-31-17-+0800.xcresult`。
- 2026-11-21 签名 Simulator 完整附件验收：`testMordeFullLayoutUploadsAndSendsImageAndPDFFixture` 通过（1/1），覆盖图片和 PDF 上传、聊天回显、两种聊天预览、文件库分类及图片预览。测试环境：同一 iOS 27.2 Simulator。结果包 `/tmp/morde-h06-signed-derived/Logs/Test/Test-Morde-2026.11.21_18-35-01-+0800.xcresult`。
- H07 本轮 UI 复跑：旧专用 Simulator 中 2 个用例在本地登录夹具处失败，1 个用例因中断取消；新建的干净 iOS 27.2 Simulator 上单独运行 `testMordeChatComposerSendsSequentialPlainTextMessages` 也在聊天页面之前失败，登录提示为 `NSURLErrorDomain -3003`。失败截图/层级显示本地账号登录没有进入应用；没有执行聊天断言或发送消息。结果包：`/tmp/MordeH07Conversation.xcresult`、`/tmp/MordeH07IOSClean.xcresult`。该现象阻断本地 UI 夹具验收，原因尚未定位，不能据此判定聊天消息逻辑通过或失败。
- 本轮最终源码构建：`cd clients/nanoMuse && xcodebuild build -project android/src/ios/Morde.xcodeproj -scheme Morde -configuration Debug -destination 'generic/platform=iOS Simulator' -derivedDataPath /tmp/morde-h06-signed-derived CODE_SIGNING_ALLOWED=NO`，Xcode 27.2，`BUILD SUCCEEDED`；编译器输出现存 Swift 6 隔离和 AppIcon 资源警告。
- 本地化：`Localizable.xcstrings` 的重复键、NanoMuse 全量 key/8 种目标语言覆盖、测试辅助断言和占位符检查均通过。由于当前 Python 3.15 环境没有 `pytest` 模块，未运行 pytest runner；通过标准库 shim 直接调用仓库 `tests/test_ios_strings.py` 中相同断言函数完成检查。
- 产品边界：移除设备说明中将 Web 控制台描述为可互相委派任务的 Muse 的内容，保留 iPhone 与 morde Desktop 电脑的跨端说明。
- H06-4 环境：现有 `testC10PortalMessagePersistsAcrossAppRestart` 要求 loopback Portal、Cloud DSH Worker、独立 DSH Docker endpoint、Guardian 和含可用个人模型连接的测试账号。本机复核（2026-11-21）：`limactl list` 无实例；标准 `.local/morde-local/start.sh` 在缺少 `morde-f01-u2604` 时明确退出且禁止创建空 VM；Docker 仅有两个 PostgreSQL 容器，`127.0.0.1:24131`、`:7080`、`:5173` 均无监听，本轮进程环境也没有 `MORDE_C10_*` 凭据。当前没有可安全启动的完整本地 Portal/DSH/Guardian 栈，因此 H06-4 保持待验证；遵守本地数据约束，未连接远端测试机或外部模型，也未发送文件。
- 范围：本轮构建、单测和 Simulator fixture 均在本地完成；真实服务端及 PC/iOS 跨端接续仍待隔离本地服务栈验收。`git diff --check` 通过。
