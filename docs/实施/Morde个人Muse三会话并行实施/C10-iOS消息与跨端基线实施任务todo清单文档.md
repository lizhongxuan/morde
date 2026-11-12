# C10 iOS 消息与跨端基线实施任务 TODO

**会话：C｜阶段：P1｜状态：完成（C10-T1/T2/T3 均通过；本地 G1 选定模型与 Portal App-close / 跨端对账验收通过）。**

来源：[需求与架构](../../Morde个人Muse需求与架构设计方案.md)；共同规则：[执行总览](00-三会话执行总览.md)；接口：[共享合同](01-共享接口与联调合同.md)。

## 开始条件与修改边界

G0 已于 2026-11-09 通过，合同版本 `muse-a00-v1`。C10-T1/T2/T3 已完成；2026-11-10 在本地 iOS 27.2 Simulator 和 A10 G1 Portal 上重新验收通过，旧复核中的模型偏好、Keychain 与 runtime 阻塞均已解除。

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

- [x] C10-T3（完成）完成 iOS 云端执行基线联调
  - 前置：C10-T2、A10-T2。
  - 修改：使用 C 独立 Simulator、DerivedData 和 A10-T2 版本的本地服务，执行消息与重连流程；通过本机可用的 UI 自动化观察结果。
  - 验收：A1/A2/A12：退出 App 后云端任务继续，重新打开读取原 Task，另一端的消息能对账；实际服务验证与 fixture 验证分开记录。交付 A10-T3。
  - 历史本地真实服务验收（2026-11-09，待复验）：独立 Simulator `Codex C10 Isolated 20261109`（iPhone 18 Pro / iOS 27.0），DerivedData `.local/c10-ios-20261109/DerivedData`；测试 `testC10PortalMessagePersistsAcrossAppRestart` 当时通过（1/1，0 失败、0 跳过，81.791 秒），结果 `.local/c10-ios-20261109/PortalAcceptance4.xcresult`。该结果记录了当次登录、消息发送、App 关闭期间任务完成，以及重启恢复和对账；随后 A10-T3 独立复核发现客户端模型选择与账号偏好不一致，因此本次最终状态以本节 2026-11-10 新复验为准。
  - 模型偏好修复前失败记录（2026-11-10）：本地正式 Portal 上的新合成账号首条消息路由失败（`model_unavailable`），第二条在 60 秒只读轮询后仍为 `processing`；API 共 2 条测试消息、0 个 Task，未重发。账号个人模型连接已检查通过，Portal 返回的 `selected_model_id` 指向该连接，但当时 iOS 仅按管理员 `default_model_id` 选择模型；管理员默认连接不适用于该合成账号。失败结果包 `.local/morde-a10-linux-fixture-20261109/logs/A10PortalAcceptance-E36-owner2.xcresult` 保留作诊断；后续补齐模型偏好选择后，本节所列复验已通过。
  - 历史 Keychain 失败（2026-11-10）：旧 `CODE_SIGNING_ALLOWED=NO` 产物没有 app identifier entitlement，登录路由曾显示 `NSURLErrorDomain -3003`；失败结果包 `.local/c10-model-preference-20261110/results/SelectedModelPreference-4.xcresult` 保留。之后 signed Simulator 构建的选定模型测试通过（1/1，62.118 秒），结果 `.local/c10-signing-diagnosis-20261110/SelectedModelSigned.xcresult`；本轮复验再次通过，Keychain 阻塞解除。
  - 验收环境及结果（2026-11-10）：iOS 27.2 Simulator runtime build `24B8117` 可用；独立 iPhone 18 Pro Simulator `Morde A20 G1 20261110`（UDID `D62EF0D9-9309-401A-B7EB-97B413FB387F`）使用最新 build-for-testing `.local/morde-a10-linux-fixture-20261109/g1/a20-build-derived`，签名诊断 entitlement `.local/morde-a10-linux-fixture-20261109/g1/G1-Keychain.entitlements` 仅用于本地测试。`testMordeChatUsesSelectedModelPreferenceWhenAvailable` 通过（1/1，0 skip/0 fail，42.17s）；结果 `.local/morde-a10-linux-fixture-20261109/g1/evidence/A20-20261110/SelectedModelPreference.xcresult`。本地 G1 Portal loopback `127.0.0.1:24931` 的 `network-policy.sh check` 五项 PASS；合成测试凭据权限 0600，仅存本地。
  - C10-T3 App-close / 跨端复验：`testC10PortalMessagePersistsAcrossAppRestart` 通过（1/1，0 skip/0 fail，62.86s），结果 `.local/morde-a10-linux-fixture-20261109/g1/evidence/A20-20261110/G1-AppClose.xcresult`。主 conversation `c6zwlksp` 从 12 条增至 16 条，新增 user 消息 client IDs `c2e9f088-4b07-40a2-8dde-e1d0e37cdf3b`、`B088C9C9-391C-49C1-B080-640BFEF8D869`；对应 Task `qmnolpvw`、`qn97xcg7` 均 completed，且各有一条完成回复；`active_tasks=0`。因此确认 App 关闭期间云端执行、重启恢复原 Task 及消息对账。
  - 本轮独立复验（2026-11-10）：同一 `testC10PortalMessagePersistsAcrossAppRestart` 再次通过（1 passed、0 failed、0 skipped，62.7s），覆盖本地 Portal 登录、发消息、App 关闭期间执行、重启恢复与对账；结果 `.local/c10-ios-20261109/PortalAcceptance-20261110-approved-owner.xcresult`。先前使用另一个未获准的本地测试账号复验时 Portal 返回 `authentication required`；切换到已批准的 G1 合成 owner 后通过。Simulator launchd 中的临时 loopback origin 已清除，未改源码。
  - 并发本地 fixture activity：同一测试窗口的另一 conversation 还出现 3 条消息/任务（1 completed、2 cancelled），来源未确定，`active_tasks=0`；不归入本次 C10-T3 验收证据。
  - 服务环境：本机 A10 Lima Ubuntu 26.04 ARM64 的 G1 Portal 经本机 loopback `127.0.0.1:24931` 提供给 Simulator；network policy guard 保持开启并通过五项检查。模型端为仅本机的合成服务；未调用外部模型服务或使用用户提供的 API key。仅本地合成账号用于验收。

## 交付

C10-T1/T2/T3 均已完成；signed 选定模型 UI、G1 本地 Portal App-close / 重启 / 跨端对账验收均通过，可继续处理后续依赖任务。runtime 与 Keychain 阻塞已解除。未具备真机条件不影响本次模拟器验收，但不宣称真机已验收。只汇报本地位置、版本、验证及未完成项；不得外发文件。已有实现通过本任务验证可直接复用，不为勾选任务而重写。

## 补充：PC 自然对话分流验证（2026-11-11）

本节记录此次 PC 端实测，不改变 C10-T1/T2/T3 的 iOS 验收状态。

- [x] 在本机 B40 Portal 发送“讲个笑话”和“现在几点？”。两条普通消息都停在“已受理，等待处理”，没有任务，也没有模型调用记录；路由租约持续重试。临时添加的测试账号模型预算未改变结果，验证后已删除。
- [x] 修复源码：分类阶段遇到 `CodeConflict` 时不再无限延后，改为记录 `routing_context_unavailable`；PC 对话已有“重试处理”入口。缺失前序消息的安全恢复规则未改。
- [x] PostgreSQL 集成验证 `TestRouteModelDirectReplySettlesReplaysAndAppliesOnce`、`TestDSHCoordinatorDirectReplyUsesNoToolsModelAndAppliesOnce` 通过：DSH 的直接回复写入一条助手消息、不建 Task、重放不重复调用；Worker 的直接回复与冲突处理单测通过。集成使用本机临时数据库和合成模型，没有调用外部模型服务。
- `现在几点？` 属于直接回复，不应创建 Task；当前分类提示没有可信实时钟表或客户端时区信息，并要求模型不得编造，因此只能如实说明无法确认实时当地时间。
- [ ] B40 浏览器端需在本地测试服务重建后复验。当前运行的 `/opt/c20-e2e/bin/portal-v2` 是独立于工作区源码的二进制；本次按本机“不得外发文件”约束没有替换或重启它，因此浏览器里两条既有测试消息仍代表旧服务行为，不能算源码修复后的实机通过。

