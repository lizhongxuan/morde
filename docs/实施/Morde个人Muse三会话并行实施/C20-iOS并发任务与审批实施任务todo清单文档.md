# C20 iOS 并发任务与审批实施任务 TODO

**会话：C｜阶段：P2｜状态：已完成（C20-T1/T2/T3 完成）。**

来源：[需求与架构](../../Morde个人Muse需求与架构设计方案.md)；共同规则：[执行总览](00-三会话执行总览.md)；接口：[共享合同](01-共享接口与联调合同.md)。

## 开始条件与修改边界

G1；与 A20/A21、B20 并发；先按 K2 样例实现。

iOS 现有聊天、任务详情、审批/澄清、事件合并、Swift DTO 及相应测试/本地化。服务端、PC 和公共合同全部只读。

## 接口变化

消费 K2。使用服务端 task_id、修订、event_sequence、sources 和结果发布标记；拟新增关系字段只按冻结版本映射，不在 Swift 另造 Task 状态机或由 UI 猜关联。

## 任务

- [x] C20-T1（完成）展示多任务与主助手结果归属
  - 修改：同一会话中分别展示 A/B、子任务状态、依赖等待与待整理结果；保持输入可用，原始结果和主助手回复来源可查。
  - 验收：模拟器样例覆盖交错进度、部分失败、整理失败和旧事件；不会把所有任务混成一个正在输入，也不会重复显示 Task.result 与最终消息。
  - 验证：Swift 测试 11/11 通过；签名 Simulator 定向 UI 验收 1/1 通过（`.local/c20-g1-evidence/C20-T1-K2-final.xcresult`）。

- [x] C20-T2（完成）接通具体任务停止与跨端审批
  - 前置：C20-T1。
  - 修改：停止、修订、查询和审批/澄清使用现有 API 与版本；控件明确指向任务。保留重连后未处理请求，已处理的答复不能再次执行。
  - 验收：停止 B 不影响 A；PC 已批准后 iOS 状态更新，重复点击和旧审批被正确处理；任务忙碌时还能发独立消息。
  - 完成：审批收件箱在页面可见时每 5 秒刷新，不清空现有内容；退出页面或切换账号后轮询取消。停止操作继续使用目标 task_id、expected_status 和既有幂等/版本校验。
  - 验证：Swift 测试 11/11 通过；Simulator T2 目标 UI 验收 7/7 通过（`.local/c20-g1-evidence/C20-T2-all-green.xcresult`），覆盖停止 B 保持 A 运行、并发任务时发送独立消息、PC 状态更新、旧审批冲突及幂等重试。

- [x] C20-T3（完成）联调主协调回传与并发执行
  - 前置：C20-T2、A21-T3。
  - 修改：接 A21-T3 正式服务，验证 Swift 映射和实际 UI，不把本地模拟数据视为端到端证据；问题按归属交 A 或自行修复。
  - 验收：在 A 未完成时发 B、修订 A、停止 B；子 Agent 完成后主助手继续回复，重启 App 仍一致；完成 A3–A6/A11 的 iOS 路径并交付 A21-T4。
  - iOS 客户端映射与 UI：`MordeApp.swift` 的 `MordeTask` 已按 K2 解码 `dependencies`、`child_jobs`、`result_review`，聊天/任务详情沿用 `task_id`、`task_revision`、`event_sequence` 呈现来源。新增 `testC20PortalConcurrentResultReviewPersistsAcrossAppRestart`，使用真实 iOS 登录与 loopback Portal API 读回；通过 app UI 创建 A/B、定向修订 A、停止 B，并在 App 重启后校验唯一最终回复、来源与 K2 任务详情。测试输入使用本地 G2 synthetic hold markers，origin 强制限定为 `127.0.0.1`/`localhost`；测试只报告 release gate，不调用外部模型。
  - 本地客户端验证（2026-11-10）：`xcodebuild ... build-for-testing` 成功；独立 Simulator `Morde C20 T3 Local UI 20261110` 上的 `testMordeChatShowsConcurrentTasksAndK2ResultProvenance` 通过（1/1，0 失败/0 跳过），结果 `.local/c20-t3-20261110/results/C20-T1-K2-fixture.xcresult`。该 UI 用例只使用客户端本地 K2 fixture，不作为真实服务联验依据。
  - 服务联验（2026-11-10）：A21-T3 本地 Portal + loopback synthetic model proxy；独立 Simulator `Morde C20 T3 Isolated 20261110` 的 `testC20PortalConcurrentResultReviewPersistsAcrossAppRestart` 通过（1/1，0 失败、0 跳过），结果 `.local/c20-t3-20261110/results/C20-T3-real-service-CodexT3Local1110-final.xcresult`。验证同一会话先后创建独立 A/B；B 的 child 完成并持久化 `ask_user` 澄清后处于 `waiting_user`，iOS 停止 B 后 A 仍为 `running`；A 修订 1 的 child 成功后，主协调器发布带 task/revision/event 来源的唯一回复。重启 App 后，聊天来源卡片、最终回复、任务结果整理和成功 child 状态均恢复且无重复消息。T2 已另行覆盖停止运行中的 B。
  - 合成 B 在已计量模型回合后进入 `waiting_user` 再停止，避免取消处理中模型请求留下 `usage_unknown` 账目；服务端对此采用 fail-closed 预算保护，本任务未修改该策略。

## 交付

本地 C20-T3 验收证据已记录，可供 A21-T4 联验引用；不把未实现的文件接口混入第二轮。只汇报本地位置、版本、验证及未完成项；不得外发文件。已有实现通过本任务验证可直接复用，不为勾选任务而重写。

