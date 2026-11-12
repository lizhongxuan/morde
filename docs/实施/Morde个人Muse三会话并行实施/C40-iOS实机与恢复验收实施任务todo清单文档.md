# C40 iOS 实机与恢复验收实施任务 TODO

**会话：C｜阶段：P4｜状态：进行中（2026-11-12；Simulator 与本地 Portal 恢复、loopback 合成账户离线消息隔离已通过；云电脑、实机、锁屏与跨端项仍待验证）。**

来源：[需求与架构](../../Morde个人Muse需求与架构设计方案.md)；共同规则：[执行总览](00-三会话执行总览.md)；接口：[共享合同](01-共享接口与联调合同.md)。

## 开始条件与修改边界

G3；与 A40/B40 并发，前台 UI 控制按总览预约。

既有 iOS 工程、UI/状态恢复、测试和本地构建配置，仅修本轮必要缺口；不发布商店、不引入 APNs 新功能，也不代改 A/B 代码。

## 接口变化

检查实际消费者与 K1–K4 一致，不预设接口新增。现有凭据、审批、Host ticket、文件引用和事件游标继续走原授权。

## 任务

- [ ] C40-T1（待验证）验证完整 iOS 使用与云电脑交互
  - 修改：在独立模拟器及已具备授权条件的真机上验证登录、连续对话、并发任务、审批、产物和云电脑查看/接管；修复触控/键盘/安全区域问题。
  - 验收：实际入口可完成任务，接管与等待状态真实；模拟器和真机证据分别记录。真机/签名条件缺失时明确待验证，不能以页面截图替代。
  - 进展（2026-11-11）：iOS 27.2 独立 Simulator 的 9 项合成验收通过；本地 G3 Portal + PC Hub 文件获取与 UI 展示通过 1/1。真机连接和签名身份不可用；本轮未验证真实桌面查看/接管/交还。G3 合成模型不支持 C10/C20 通用任务路由，因此没有把 `waiting_user` 结果计作任务成功。证据摘要：`.local/c40-sim/C40-T1-T2-fixtures-rerun-2026-11-11.summary.txt`、`.local/c40-sim/C40-G3-Portal-file-fetch-final-2026-11-11.summary.txt`。

- [ ] C40-T2（进行中）验证关闭 App、断连与会话恢复
  - 前置：C40-T1。
  - 修改：覆盖锁屏/终止 App/重新启动、离线待发、登录过期、服务端重启和旧事件重放；使用合成资料，不操作用户真实在线应用。
  - 验收：服务端已受理任务继续；重连展示同一 Task 和文件版本，消息/审批不重复，旧账号待发内容不会进新账号。后台通知未接入不影响这些结果。
  - 进展（2026-11-11）：9 项 Simulator 用例覆盖 App 终止/重启、离线待发恢复、已受理消息对账、过期事件游标去重及审批重试；最新 `MordeConversationMessageTests` 7/7 通过，包含旧服务/账号 outbox 隔离、重启保留 client/predecessor ID、过期凭据判定。登录过期 UI 闭环新增本地一次性 401 fixture，`testMordeExpiredClientSessionRequiresReauthentication` 通过（1/1）：拒绝后回到登录表单，重新登录恢复主界面与输入框；按 Home 进入后台并返回后输入框仍可用。重启本地 Portal 后，iOS 重新打开同一已完成 Task 与文件路径/SHA-256，UI 1/1 通过；API 对账确认 Task、fetch、5 个事件序号及单条原消息均保持一致。证据：`.local/c40-sim/C40-T1-T2-fixtures-rerun-2026-11-11.summary.txt`、`.local/c40-sim/C40-Portal-restart-recovery-2026-11-11.summary.txt`、`.local/c40-sim/C40-expired-session-background-final-2026-11-11.summary.txt`、`.local/c40-sim/C40-message-unit-tests-updated-2026-11-11.summary.txt`。
  - 进展（2026-11-11 补充）：C40 Simulator 的 loopback `/healthz` 可经 `simctl spawn ... curl` 返回 200。为账号隔离 UI 用例只在 `.local/c40-sim/DerivedData/Build/Products/C40-G3-account-switch-loopback.xctestrun` 创建本地副本，把 `MORDE_G3_PORTAL_ORIGIN` 改为 `http://127.0.0.1:25051`；合成账户环境未改，原 xctestrun 未修改。定向 C40 UUID、禁用并行克隆运行 `testMordeG3PortalOfflineOutboxStaysWithOriginalAccount` 后仍在登录阶段失败（90.96 秒，未进入离线排队/账号切换断言）。App 系统日志记录 GET `/api/capabilities` 和 POST `/api/auth/login` 使用 loopback 地址，TCP 建立后请求被取消，UI 显示 `NSURLErrorDomain -3003`。随后用 Simulator 内 curl 对同一地址及 App 相同的 Origin/Cache-Control/Request-ID 请求头诊断：capabilities 200；合成账号 owner login 200、得到 HttpOnly 根路径 Cookie；登出 204。Cookie 值和响应敏感字段未输出。配置差异：Simulator 的 HTTP/HTTPS/SOCKS 系统代理均关闭；App 的 ephemeral URLSession 未配置自定义代理，但关闭自动 Cookie 存储/发送；App 构建产物的 ATS 例外只覆盖 `hosts.localhost`，没有覆盖 `127.0.0.1`，也没有 `NSAllowsLocalNetworking` 或 `NSLocalNetworkUsageDescription`。`MordeLoginStateStore.serviceOrigin` 在 DEBUG 下允许 loopback HTTP；ATS 仍是独立策略。ATS loopback 例外是当前明确发现的候选差异，尚未证明为 -3003 的原因。建议仅做一次 Debug-only 定向验证：临时为 loopback HTTP 增加 ATS 许可，并从 App URLSession 单独 GET `/api/capabilities`；不重跑完整 UI、不改监听或防火墙。账号隔离端到端仍待验证。证据：`.local/c40-sim/C40-G3-account-switch-loopback-attempt2-2026-11-11.xcresult`、`.local/c40-sim/C40-G3-account-switch-loopback-attempt2-2026-11-11.log`、`.local/c40-sim/C40-G3-account-switch-network-diagnostic-2026-11-11.summary.txt`。
  - 进展（2026-11-11，ATS 定向检查）：按“只访问 loopback”的安全门，仅启动 C40 UUID 后在 Simulator 内查询 `hosts.localhost`。`dscacheutil -q host -a name hosts.localhost` 返回状态 111，未得到 IPv4/IPv6 地址；因此没有向该主机发 HTTP 请求，没有创建新的 xctestrun 副本，也没有重跑 UI。没有改 origin、监听、防火墙或 ATS。C40 已关闭。继续该验证的最小条件是让 Simulator 本地解析 `hosts.localhost` 到 `127.0.0.1`，再检查 `/api/capabilities` 是否 200；当前账号隔离和真机项仍待验证。
  - 进展（2026-11-12，ATS/账号隔离复测）：仅在 `.local/c40-sim` 创建临时 Debug URLSession probe，临时 ATS `NSAllowsLocalNetworking` 允许其访问宿主机 loopback `127.0.0.1:25051/api/capabilities`，Simulator App URLSession 返回 200/JSON。随后仅复制本地 Morde.app，给副本设置同一 Debug-only ATS 许可，并使用 loopback 合成 fixture xctestrun 副本、C40 UUID、单个 `testMordeG3PortalOfflineOutboxStaysWithOriginalAccount` 定向复测；源代码、正式构建及监听配置未改。用例 93.261 秒后在登录阶段失败，未出现 `morde.nav.menu`，因此账号隔离断网/切换断言未运行；报告 1 failed / 0 passed，辅助启动的 0 tests runner 未重跑目标用例。ATS 许可已证明能使最小 probe GET 连通，但无法据此证明 Morde 登录闭环可用；账号隔离仍待验证。证据：`.local/c40-sim/C40-G3-account-switch-ats-local-loopback-2026-11-12.xcresult`、`.local/c40-sim/C40-G3-account-switch-ats-local-loopback-2026-11-12.log`、`.local/c40-sim/C40-G3-account-switch-network-diagnostic-2026-11-11.summary.txt`。
  - 初次失败复核（2026-11-12）：现有 XCTest 日志显示表单提交后等待主菜单超时，应用已显示“请求失败”弹窗且登录按钮禁用；当时存档不含登录 POST 状态。此前 `URLSession` probe 仅证明 loopback GET 可达，不足以归因 ATS、服务端或账号。本项没有进入消息队列、切换账号或隔离断言，保持未完成。
  - 本地 fixture 补验（2026-11-12）：新增纯本地账号隔离/离线队列 UI 用例后，同样在 `launchLocalChat` 登录入口收到 `NSURLErrorDomain -3003`，未进入 A/B 消息断言；xcresult activity 未记录失败请求 URL，无法确认是 fixture 组合还是模拟器网络。该未通过用例已从源码撤回，避免留下已知失败项；失败记录保留于 `.local/c40-sim/C40-G3-offline-account-scope-2026-11-12.xcresult`。目标模拟器已 Shutdown，无测试进程；C40-T2/T3 仍未完成。
  - 登录链路阶段诊断（2026-11-12）：在既有 C40 Simulator 定向用例加入临时 Debug 阶段码后，App 的 Portal owner 登录通过，但 `/api/auth/client-login` 返回 HTTP 401，界面正文为 `authentication required`；用本机同一合成账号、iOS 平台、全新 client instance ID 和有序登录参数直连同一 loopback 接口，返回 200，随后 client logout 为 204。该直接请求创建并撤销了一个临时合成安装，不修改用户状态；不能据新 ID 成功推断 App 持久 client instance ID 无误。随后用新建、无旧 App 数据的 iPhone 18 Pro/iOS 27.2 模拟器复跑，诊断阶段显示 `confirmed-capabilities` 时报告 `NSURLErrorDomain -3003`，未出现 `morde.nav.menu`，离线消息与 A/B 隔离断言仍未执行。源码 `checkCapabilities()` 会将请求错误包装为 `Morde.Capabilities`，而这次外层诊断仍看到 `NSURLErrorDomain`；因此阶段标签不能证明错误来自第二次能力 GET，需在方法边界记录底层与包装后错误再归因。两个失败阶段不同，现有证据不能归因账号、安装记录、ATS 或服务端。临时 App 诊断代码已撤回；UI 用例保留失败时读取弹窗正文并提前退出的改进。临时模拟器已删除，原 C40 Simulator 已关闭。敏感输入动作已从本轮本地日志脱敏，并删除本轮 xcresult；详细证据：`.local/c40-sim/C40-G3-auth-diagnostic-2026-11-12.summary.txt`，脱敏运行日志：`.local/c40-sim/C40-G3-auth-ui-2026-11-12.log`、`.local/c40-sim/C40-G3-account-switch-clean-sim-2026-11-12.log`。
  - 待验证：Simulator 真正锁屏后的恢复、真机验收、Portal 断网排队后切换账号的端到端隔离，以及跨 PC 审批/消息的重复提交对账。当前安装的 Xcode 缺少 Simulator.app，未连接实体 iPhone/iPad 且没有有效签名身份，因此不能把 Home 后台恢复等同于锁屏或真机结果。完成剩余验收需可注册并签名的实体 iPhone/iPad 与 Apple Development 身份；临时 App URLSession `/api/capabilities` GET 已通过本地 ATS probe，但完整 Morde 登录链路仍未通过，账号隔离 UI 未到断网切换断言；需继续定位 client-login 401，并在 `checkCapabilities()` 边界确认 `-3003` 的原始请求与包装错误，保持 Portal 仅绑定宿主机 loopback，不扩大监听面。

  - 账号隔离后续复测（2026-11-12，取代此前“断言未执行”的结论）：系统常量将 `NSURLErrorDomain -3003` 映射为 `NSURLErrorCannotWriteToFile`；Simulator 日志显示 Keychain `SecItemAdd` 返回 `-34018`（`errSecMissingEntitlement`）。此前 UI 构建关闭了签名且未携带 Keychain entitlement。已给 Morde Debug/Release 配置默认 Keychain access group，并使用 ad-hoc signed Simulator 构建；登录各阶段和凭据/安装记录持久化成功。测试录屏显示应用保持在账户设置详情页，旧助手只等待聊天菜单；现已修正助手以先确认认证导航、再切换到聊天页。`testMordeG3PortalOfflineOutboxStaysWithOriginalAccount` 最终通过（1/1，110.882 秒）：Account A 离线消息不显示/不发送到 Account B，回到 A 后服务端只接收一次。测试使用本地 loopback 和合成 A/B 账户，不涉及线上用户。证据摘要：`.local/c40-sim/C40-G3-account-isolation-2026-11-12.summary.txt`。临时诊断 UI/阶段日志参数已从源码撤回；该结果不替代真机签名验收。
  - 待验证：Simulator 真正锁屏后的恢复、真机验收、真实电脑查看/接管/交还，以及跨 PC 审批/消息的重复提交对账。当前没有已连接实体 iPhone/iPad 和可用 Apple Development 身份，故不能把 Home 后台恢复或 ad-hoc Simulator 签名等同于锁屏/真机结果。C40 的本地 loopback 账号隔离 UI 已通过；只要验收仍要求真实电脑和实体设备，C40-T1/T2 不标完成。继续使用 loopback，不扩大监听面。

- [ ] C40-T3（未开始）完成最终版本跨端复核并交付
  - 前置：C40-T2、A40-T2。
  - 修改：等待 A40-T2 对最新服务树完成重验和重新冻结后，接入该本地服务候选版本与已验证桥接版本，复跑受影响案例、Swift 测试和 UI 验收，整理本地结果与未具备条件。
  - 验收：在 C 的环境用冻结的另一端版本或注明范围的协议替身检查跨端对账；A1–A12 的 iOS 责任项有明确结论。不等待 B40-T3，由 A40-T3 统一验证两端同一真实消息/Task/审批/文件；不把等待后续 APNs 当成本次云端执行失败。
  - 当前（2026-11-12）：等待 A40-T2 对最新服务树完成重验和重新冻结；旧候选指纹不作为有效版本。C40-T2 尚未完成，真机、锁屏恢复与真实云电脑交互仍待验证，因此 C40-T3 保持未开始；A40-T3 仍负责最后的三会话集成复核。

## 交付

交付后仅修 C 范围的问题；一周日常反馈由 A40-T4 汇总，C 不自行扩大到 P5。 只汇报本地位置、版本、验证及未完成项；不得外发文件。已有实现通过本任务验证可直接复用，不为勾选任务而重写。

