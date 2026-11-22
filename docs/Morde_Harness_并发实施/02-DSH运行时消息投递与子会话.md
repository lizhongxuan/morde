# S02｜DSH Runtime、运行中消息与子会话

**状态：** 已完成（本地 S02 范围，2026-11-22；PC/iOS 真实入口、真实模型和耐久验收仍待 H06；H 项状态见[主 TODO](../Morde_Harness_实施任务_TODO.md)）。

**共同开发门禁：** 缺陷必须按[并发总控](00-并发实施总控.md)追到机制根因；复杂方案、取舍和模块/函数边界要留档。不得只增大测试阈值或缩短上下文绕过失败。

## 目标与任务来源

完成 H09 的 DSH Session 压缩部分、H10 的 DSH 消息投递/Session 栅栏部分及 H11 子 Agent 恢复与汇总。沿用 DSH AgentLoop；长历史压缩、消息领取、模型中途收到 steer 后旧结果失效、检查点恢复和子会话都留在现有 Node DSH Runtime 与 Morde 持久化边界内。

具体合同直接引用[最终规格](../Morde_Harness_最终流程与实现规格.md) §2.1–2.4、§3 和[主 TODO](../Morde_Harness_实施任务_TODO.md) H09/H10/H11。

## 文件所有权

独占[总控表](00-并发实施总控.md)中 S02 的文件。唯一拥有 `runtime/morde-cloud-runtime/src/engine.mjs`、`account-runtime.mjs`（Task Agent status/activity/heartbeat 帧校验，以及 Task 子进程退出码/信号的有界诊断回传）、`hermes-compaction-window.mjs` 及对应 Node 测试；并拥有 Go DSH executor/session-tree、app/store agent checkpoint 适配及对应测试。为保证已持久化但尚未进入 DSH checkpoint 的 steer 不继续发布旧文本，S02 另拥有 `server/internal/store/postgres/task_stream.go` 与 `task_stream_integration_test.go` 的对应栅栏改动。S00 曾临时交接 `conversation_route_revision_e2e_integration_test.go`、`record_integration_test.go` 用于本地 stderr 诊断；该临时捕获现已移除，且未改动产品协议或持久化。Task Agent 活动协议的生产和 Runtime 帧校验属于 S02；持久化 observation、Task 快照投影及停滞提示属于 S03。

不编辑 `conversation_route_*.go`、服务端摘要/路由文件、Broker、`task_worker*.go`、客户端或共享文档。S01 决定服务端 route state/目标合同；S03 决定授权/预算合同；如果缺口需要改对方文件，提交接口提案，由文件 owner 实施。

## 实施步骤

1. 先读 `runtime/morde-cloud-runtime/AGENTS.md`，审阅共同基线里的 Runtime/测试改动，盘点锁定 DSH Runtime 与可用 plugin/provider；只补已证明的行为缺口，不把新增插件当作功能完成。
2. 核对 Hermes `turn_context.py`、两个 context compressor、`turn_iteration_prep.py`、`session_busy_controls.py` 与 `turn_lease.py` 及其上游测试。尽量移植兼容的窗口/消息边界和确定性辅助函数；共享 Session 状态、持久检查点、授权和副作用仍走 Morde/DSH。
3. 保证 Runtime 投递绑定 task/owner/session/agent generation/authorization version/message identity；只有消息被检查点领取后才能确认 `applied`。模型处理中收到新 steer 时，在工具派发或结果交付前丢弃旧轮输出并重推理。
4. 以稳定 `session_id` 对读历史、执行、检查点写入加跨进程栅栏；不同 Session 和独立 Task 仍可并行。恢复时按检查点与持久化消息对账，不清空去重状态。
5. 沿用现有 `dsh_delegate` 与持久子 Session；实际缺口存在时才对照 DSH subagent/AgentTeam plugin，先证明 Morde 身份、工具子集、预算、取消、恢复和账本兼容。

## 验收

运行 Runtime 的投递、compaction、recovery 与子会话 Node 测试，及 Go DSH executor/session-tree/checkpoint 测试。覆盖模型调用期间投递、序列顺序、回执丢失重放、领取后重启不重复、Session 隔离、至少两个子 Agent 并行与主 Agent据实汇总。长历史 PostgreSQL/Node DSH E2E 属于本域；实际 PC/iOS、真实模型与耐久验收交 S07。

## 本轮实施与验证（2026-11-22）

### 实施结果

- 沿用锁定 DSH `0.2.2-rc.1` 的 AgentLoop、Session、checkpoint、TokenMeter、ToolResultPruner 和现有 Morde `WorkerModel` provider。`dsh-mcp-client` 虽在锁文件中但 Runtime 未注册，实际 MCP 仍经 Morde Broker；锁定依赖没有满足 Morde 子 Agent 身份、工具子集、预算、账本和恢复边界的专用插件，因此沿用持久化 `dsh_delegate` 子 Session，不增加依赖或另建 Agent 循环。
- Hermes 按固定 SHA `0d49d6d9d20e461155d49ba9d188687079c811af` 对照。Morde 复用 DSH `BasicCompactionEngine`、TokenMeter 与 ToolResultPruner，只在窗口选择处保留 Hermes 行为映射；`hermes-compaction-window.mjs` 保留 MIT notice。Morde 用 Task/Agent generation/authorization/session/lease 栅栏管理运行，不宣称与 Hermes resolved-session alias 的跨进程租约完全等价。
- 运行中收到 steer 时，Runtime 在工具预执行门和实际 IPC/子 Agent 派发前复核消息版本；旧模型响应被抑制并保留 usage。PostgreSQL 在已有 Task/会话栅栏内发现同 Task 的 pending steer 后，中止旧文本增量并保留已展示快照。Worker 在把模型结果送回 Runtime 前先投递 pending steer，再将旧结果替换成重新评估提示。
- Task start envelope 携带初始和修订消息的持久 delivery mode；Node 严格验证 `steer`/`followup`，旧 mode-less frame 兼容为 `followup`，去重键仍绑定 mode。恢复打开工具调用时，精确匹配的 v4 supersede 不会伪造工具成功：若工具结果缺失，Runtime checkpoint 写入明确的 outcome-unknown 结果并避免盲目重放；Go checkpoint history 接受该固定标记，但仍将调用视为未对账，阻止 Task 完成。恢复过程中若已有可信 personal mutation 收据，Worker 先验证并复用收据，再处理较新的 steer，保持一次性副作用。
- 长历史集成 fixture 为各 Runtime manager 启动、取消并检查 Run 生命周期；回收候选按测试 LeaseOwner 隔离，模拟 main Runtime 重启前先停止并 join 旧 manager。

### 验证结果

- Node Runtime、compaction、recovery 与 Hermes compaction 五组测试全部通过；`node --check src/engine.mjs`、`gofmt -d` 和 `git diff --check` 无输出。
- Go app checkpoint/inbox 定向测试通过；PostgreSQL `^TestCloudDSHStream` 全套通过，新回归确认 pending steer 后不再追加旧 delta，原可见文本保留为 interrupted。
- Worker `^TestCloudDSH` 全套通过；新协议回归确认 pending steer 先送达 Runtime，旧模型工具调用不执行。840 条长历史重启与授权历史检索 E2E 通过。
- 隔离 PostgreSQL 的 `TestConversationRevisionStopsRealAgentAndRestartsWithLatestMessages` 经 Go Worker、真实本地 Node DSH Runtime 与合成模型跨层复验通过；确认同 Task 按序收到最新 steer、先前 personal mutation revision 仍为 1 且调用计数未重复增加。

### 未覆盖范围

本轮未运行真实模型调用、PC/iOS 客户端入口、跨端接续或 60 分钟耐久任务；这些仍由 H06 验收。完整 Worker 回归中的 `TestCloudDSHBackgroundOutputContinuationAfterLeaseReclaim` 曾间歇性在领取断言失败，单独重跑和后续完整重跑通过；该时序波动记录在[验证记录](../验证记录/Morde_Harness_最终流程测试用例.md)中，仍需留意。
未运行 Hermes 上游 Python 测试；本轮上游比对基于固定 SHA 源码/测试审阅，自动回归使用 Morde 当前锁定的 Node DSH Runtime。

## 会话完成回报

按[总控交接格式](00-并发实施总控.md#5-会话交接格式与合并规则)返回 Hermes 配对测试、DSH 插件功能证据、消息状态/检查点变化和未测范围。不要编辑共享 TODO/验证记录。
