# C10 iOS 消息与跨端基线实施任务 TODO

**会话：C｜阶段：P1｜状态：阻塞（C10-T3 的本机 Simulator runtime 不可用，且登录凭据写入失败；G1 未通过）。**

来源：[需求与架构](../../Morde个人Muse需求与架构设计方案.md)；共同规则：[执行总览](00-三会话执行总览.md)；接口：[共享合同](01-共享接口与联调合同.md)。

## 开始条件与修改边界

G0 已于 2026-11-09 通过，合同版本 `muse-a00-v1`。C10-T1/T2 已完成；C10-T3 的旧成功记录在 2026-11-10 的独立复核中未能复现，模型偏好选择已实现，仍需恢复 Simulator 后重新验收。

clients/nanoMuse/android/src/ios/ 内现有 Morde 工程、实际聊天调用、MordeCredentialExpiry/消息 DTO、outbox、测试与 Localizable.xcstrings；遵守目录 AGENTS.md。不要改 Android App 或另建 iOS 产品。

## 接口变化

消费 K1 和既有 Task/事件合同；保持客户端消息标识、前序引用、账号/服务实例及凭据归属。使用现有工程与 scheme，不能只改 MordeApp 内的展示 fixture。

## 任务

- [x] C10-T1（完成）对齐 iOS 实际消息调用与数据类型
  - 修改：实际 Swift API/DTO、凭据与消息对账路径；依据 G0 固定入口和正式 OpenAPI 接线，保留安全存储及账号边界，新增文案同步全部现有语言。
  - 验收：Swift 单测验证解码、过期身份、服务实例变更与稳定消息 ID；请求进入真实生产路径，没有本机 Agent 或第二套消息 API。

- [x] C10-T2（完成）完善连续发送和离线待发状态
  - 前置：C10-T1。
  - 修改：聊天输入、消息 outbox 和历史/任务状态接续；正在执行、Host 离线或状态加载失败不使普通文字发送失效，未受理时不虚报成功。
  - 验收：模拟器验证键盘和输入法、连续发送、断网重试、响应丢失及 App 重启；消息不丢不重复，切账号不把旧待发内容投给新账号。
  - 本地验证（2026-11-09）：独立 Simulator `Codex C10 Isolated 20261109`（iPhone 18 Pro / iOS 27.0）与 DerivedData `.local/c10-ios-20261109/DerivedData`。Swift 包单测 8/8 通过。UI 验收通过：`testMordeChatComposerSubmitsFromKeyboardReturn`、`testMordeChatComposerSendsSequentialPlainTextMessages`（本地接收替身拒绝错误前序 ID，两条消息均确认且无重复）、`testMordeChatComposerKeepsQueuedAttachmentsAfterLostResponseForRetry`、`testMordeChatComposerReconcilesAcceptedMessageAfterAppRestart`、`testMordeChatComposerRecoversPlainTextAfterOfflineRestart`、`testMordeChatDraftIsClearedWhenAccountChanges`、`testMordeChatComposerQueuesFollowUpOnActiveTask`。真实 Portal/云端 API 联调不计入本任务，留待 C10-T3。

- [ ] C10-T3（阻塞）完成 iOS 云端执行基线联调
  - 前置：C10-T2、A10-T2。
  - 修改：使用 C 独立 Simulator、DerivedData 和 A10-T2 版本的本地服务，执行消息与重连流程；通过本机可用的 UI 自动化观察结果。
  - 验收：A1/A2/A12：退出 App 后云端任务继续，重新打开读取原 Task，另一端的消息能对账；实际服务验证与 fixture 验证分开记录。交付 A10-T3。
  - 历史本地真实服务验收（2026-11-09，待复验）：独立 Simulator `Codex C10 Isolated 20261109`（iPhone 18 Pro / iOS 27.0），DerivedData `.local/c10-ios-20261109/DerivedData`；测试 `testC10PortalMessagePersistsAcrossAppRestart` 当时通过（1/1，0 失败、0 跳过，81.791 秒），结果 `.local/c10-ios-20261109/PortalAcceptance4.xcresult`。该结果记录了当次登录、消息发送、App 关闭期间任务完成，以及重启恢复和对账；由于随后在 A10-T3 独立环境的复核发现客户端模型选择与账号偏好不一致，这份旧结果仅作为可复用的历史证据，不代表本轮复核或 G1 已通过。
  - 重新验收发现（2026-11-10）：本地正式 Portal 上的独立 Simulator 复核未通过。新合成账号首条消息路由失败（`model_unavailable`），第二条在 60 秒只读轮询后仍为 `processing`，API 中共 2 条测试消息、0 个 Task；未重发消息。账号的个人模型连接已检查通过，且 Portal API 返回的 `selected_model_id` 指向该连接，但 iOS 当前仅按管理员 `default_model_id` 选择模型，未读取或应用账号偏好；管理员默认连接属于不可用于此次合成账号验收的共享模型。
  - 待完成：重新运行选定个人模型的 Simulator UI 用例与本地正式 Portal 验收，确认唯一 Task、完成回复、重启恢复及 PC/iOS 对账。账号 `selected_model_id` 的读取、有效/已检查/文本模型约束和选择策略已实现；需在 Simulator 恢复后通过 UI 与实际 Portal 证明整条链路。此前 Portal 失败结果包 `.local/morde-a10-linux-fixture-20261109/logs/A10PortalAcceptance-E36-owner2.xcresult` 保留以供诊断。
  - 2026-11-10 iOS 回归：已实现 `selected_model_id` 读取、可用模型约束、模型选择策略及严格本地消息 fixture；`swift test` 9/9 通过，iOS App 与 UI test bundle 的 `xcodebuild build-for-testing` 通过。定向 UI 测试 `testMordeChatUsesSelectedModelPreferenceWhenAvailable` 未能进入已登录路由：本地 `/api/capabilities` 与 `/api/auth/client-login` 均返回 200，之后无 `/api/me` 请求，界面显示 `NSURLErrorDomain -3003`；当前 Keychain `SecItemAdd` 失败路径映射到该错误，需在可运行的 Simulator 上取得 OSStatus 并修复/验证登录存储。失败结果包 `.local/c10-model-preference-20261110/results/SelectedModelPreference-4.xcresult`。
  - 验收环境阻塞（2026-11-10）：iOS 27.0 Simulator runtime 在本机 CoreSimulator 中变为 unavailable（`runtime profile not found`），`xcodebuild -showdestinations` 仅列出占位 Simulator，无法执行 UI test。独立只读探测 `http://127.0.0.1:24931/api/capabilities` 返回 200，本地 Portal API 可用。Apple 提供的 iOS 27.2 runtime 下载为 8.32 GB；此前一次下载在约 1 MB/s 下不到 1% 后停止，未导入或安装。恢复可用 Simulator runtime 后继续处理 Keychain 登录并重跑本任务；不把编译或 Swift 单测记作 Simulator 验收。
  - 签名排查进展（2026-11-10）：G1 本地验收脚本显式设置 `CODE_SIGNING_ALLOWED=NO`，对应旧 Simulator App 产物没有 app identifier entitlement；`MordeKeychain.set` 的写入失败会映射为 `NSURLErrorDomain -3003`。尚未取得失败时的 Keychain OSStatus，签名与错误之间的因果仍待 Simulator 复验。当前源码分别以 Xcode 本地 Simulator 签名、以及仅本地诊断用 Keychain access group 成功构建 `build-for-testing`；G1 合成账号 test-run spec 使用 `127.0.0.1:24931`，文件权限 0600，仅保存在 `.local/`，尚未运行。iOS 27.2 runtime 的系统 MobileAsset 下载已在主机重启后恢复，截至 14:28 缓存约 5.92 GB / 8.32 GB；本机 `xcodebuild -downloadPlatform iOS -exportPath .local/morde-a10-linux-fixture-20261109/g1/runtime-download` 仍活跃，runtime 未注册。主机重启后本机 Lima A/B/C VM 已停止，G1 loopback API 暂不可用；runtime 安装后会先恢复 C10 使用的本地 A10 G1 VM/隧道，再执行模型偏好 UI fixture 和 G1 App-close / 第二安装消息 / 重启对账验收。此进展不代表 C10-T3 或 G1 通过。
  - 服务环境：A10-T2 本地 Lima Ubuntu 26.04 ARM64 VM `morde-c10-a10-20261109`，Portal 当前源码、DB schema 170，loopback API `127.0.0.1:24131`；正式 `network-policy.sh check` 五项通过，未关闭 guard。独立 Guardian 与 DSH Docker daemon 已启用；运行时镜像 digest `sha256:9c280ae7fd308466d7f1a9e9bf3e24261475337eb686a8575ba8cbc1d05610a7`，容器使用 `65532:65532`。模型端为 VM 内 loopback HTTPS 替身，经仅允许本机目的地址的 CONNECT 代理；未调用外部模型服务或使用用户提供的 API key。仅本地合成测试账号通过 Portal 执行控制 API 启用云端执行。

## 交付

C10-T1/T2 已完成；C10-T3 阻塞于本机 Simulator runtime 与 Keychain 登录写入问题，G1 未通过，因此暂不领取依赖 G1 的 C20。恢复 Simulator 后应先获取 Keychain OSStatus、修复登录并通过选定模型 UI 与本地 Portal 重启/跨端对账验收。未具备真机条件不影响模拟器验收，但不宣称真机已验收。只汇报本地位置、版本、验证及未完成项；不得外发文件。已有实现通过本任务验证可直接复用，不为勾选任务而重写。

