# S06｜Morde iOS 客户端

**状态：** 本地 iOS 实现核对、动态任务进度修复与模拟器合成 UI 验收完成；真实 Provider 和 PC 创建会话接续待 H06/S07 验证。

**共同开发门禁：** 缺陷必须按[并发总控](00-并发实施总控.md)追到机制根因；复杂方案、取舍和模块/函数边界要留档。不得只绕过失败状态或缩短测试输入。

## 目标与任务来源

完成 H06 中 iOS 消息受理、任务状态/进度、会话恢复与追问的原生入口，并接续同一账号在 PC 创建的会话。具体行为直接引用[根 AGENTS.md](../../AGENTS.md)第一里程碑、[最终规格](../Morde_Harness_最终流程与实现规格.md) §2.4 和[主 TODO](../Morde_Harness_实施任务_TODO.md) H03/H06。

## 文件所有权

独占 `clients/nanoMuse/android/src/ios/**`。遵守 `clients/nanoMuse/AGENTS.md`：只修改 iOS 工程和复用的 iOS 源码，保留上游版权/许可，并更新 `Localizable.xcstrings` 中已支持语言。

不修改 PC、服务端、Node Runtime、共享 API DTO 或主 TODO。需要接口变化时先向对应服务端 owner 提案。UI 沿用仓库 ice-cream-blue 规范及 iOS 原生交互。

## 实施步骤

1. 先检查当前 iOS 工程、已有消息 outbox/会话/Task 事件与工作树改动，不重复实现已有可靠性机制。
2. 对已受理消息呈现真实等待、运行、完成、失败、撤权及可恢复状态；重连/重试使用现有幂等身份，不能重复创建任务或操作。若现有 iOS 审批界面承载 H03 信息，接收 S03 的数据合同并本地化展示额度/未知状态。
3. 复用服务端 `conversation_id`、消息 sequence 和 Task 结果；不在客户端推测/重排服务端意图或跨账号历史。
4. 从实际 Morde iOS App 入口测试；记录模拟器或设备、构建版本和本地证据。只用合成消息，不上传项目文件。

## 验收

按 H06 覆盖 iOS 发出真实请求并收到对应状态、PC 已创建会话的历史/任务续接、后台任务期间快速问答与追问、断网重连和重复发送幂等。运行相关 Xcode 单元/UI 测试；实际目标环境缺失时标待验证，不把 fixture/UI 单测写成真实 Provider 或跨端通过。

## 当前进度（2026-11-22）

- **实现核对：** 现有客户端在清空输入框前先持久化 account-scoped outbox；重启后把中断发送标为 `unknown`，使用原 `client_message_id` 对账并复用幂等身份；切换会话重新加载消息和活跃任务。任务详情重连用例发现，任务列表里的动态详情只读了初始快照、没有订阅 Task 时间线；现已复用 `watchTimeline`，在尚无时间线加载时启动订阅。
- **本地验收：** 在 S06 隔离工作树的 `Morde` scheme / `MordeUITests` target、iPhone 18 Pro / iOS 27.2 模拟器，以正常模拟器签名运行 7 个实际 App UI fixture 用例，7/7 通过：顺序文本发送、失败路由原位重试、断网重启恢复、重启后受理状态对账、运行任务期间追问、运行任务期间独立消息、服务端 204 后仍活跃时重连并取得后续事件。
- **复现与证据：** 初始 `testMordeTaskDetailReconnectsAfter204WhenSnapshotIsStillActive` 因动态详情未订阅而失败；结果为 `/tmp/MordeH06S06-task-recovery-20261122.xcresult`。修复后该用例单独 1/1 通过（`/tmp/MordeH06S06-task-recovery-fixed-20261122.xcresult`），并在同一模拟器上回归 7/7（`/tmp/MordeH06S06-regression-rerun2-20261122.xcresult`）。合成事件夹具要求客户端先收到序号 8、204 后以 `after=8` 重连并收到序号 9，随后快照展示两步。
- **边界：** 请求仅到 loopback 合成服务，不证明真实 Provider 回复、PC 创建会话的 iOS 续接或真实服务端 Task 执行；这些 H06 验收继续进行。完整回归还有来自 `MordeStandaloneApp.swift` 的 UIKit hierarchy runtime warning，未导致测试失败。

## 会话完成回报

按[总控交接格式](00-并发实施总控.md#5-会话交接格式与合并规则)回报工程 scheme、目标设备、实际操作、测试命令、消息/会话/Task 关联证据和未测范围。不要编辑共享 TODO/验证记录。
