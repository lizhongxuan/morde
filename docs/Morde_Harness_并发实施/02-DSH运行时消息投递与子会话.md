# S02｜DSH Runtime、运行中消息与子会话

**状态：** 未开始（H 项状态见[主 TODO](../Morde_Harness_实施任务_TODO.md)）。

**共同开发门禁：** 缺陷必须按[并发总控](00-并发实施总控.md)追到机制根因；复杂方案、取舍和模块/函数边界要留档。不得只增大测试阈值或缩短上下文绕过失败。

## 目标与任务来源

完成 H09 的 DSH Session 压缩部分、H10 的 DSH 消息投递/Session 栅栏部分及 H11 子 Agent 恢复与汇总。沿用 DSH AgentLoop；长历史压缩、消息领取、模型中途收到 steer 后旧结果失效、检查点恢复和子会话都留在现有 Node DSH Runtime 与 Morde 持久化边界内。

具体合同直接引用[最终规格](../Morde_Harness_最终流程与实现规格.md) §2.1–2.4、§3 和[主 TODO](../Morde_Harness_实施任务_TODO.md) H09/H10/H11。

## 文件所有权

独占[总控表](00-并发实施总控.md)中 S02 的文件。唯一拥有 `runtime/morde-cloud-runtime/src/engine.mjs`、`hermes-compaction-window.mjs` 及对应 Node 测试；并拥有 Go DSH executor/session-tree、app/store agent checkpoint 适配及对应测试。

不编辑 `conversation_route_*.go`、服务端摘要/路由文件、Broker、`task_worker*.go`、客户端或共享文档。S01 决定服务端 route state/目标合同；S03 决定授权/预算合同；如果缺口需要改对方文件，提交接口提案，由文件 owner 实施。

## 实施步骤

1. 先读 `runtime/morde-cloud-runtime/AGENTS.md`，审阅共同基线里的 Runtime/测试改动，盘点锁定 DSH Runtime 与可用 plugin/provider；只补已证明的行为缺口，不把新增插件当作功能完成。
2. 核对 Hermes `turn_context.py`、两个 context compressor、`turn_iteration_prep.py`、`session_busy_controls.py` 与 `turn_lease.py` 及其上游测试。尽量移植兼容的窗口/消息边界和确定性辅助函数；共享 Session 状态、持久检查点、授权和副作用仍走 Morde/DSH。
3. 保证 Runtime 投递绑定 task/owner/session/agent generation/authorization version/message identity；只有消息被检查点领取后才能确认 `applied`。模型处理中收到新 steer 时，在工具派发或结果交付前丢弃旧轮输出并重推理。
4. 以稳定 `session_id` 对读历史、执行、检查点写入加跨进程栅栏；不同 Session 和独立 Task 仍可并行。恢复时按检查点与持久化消息对账，不清空去重状态。
5. 沿用现有 `dsh_delegate` 与持久子 Session；实际缺口存在时才对照 DSH subagent/AgentTeam plugin，先证明 Morde 身份、工具子集、预算、取消、恢复和账本兼容。

## 验收

运行 Runtime 的投递、compaction、recovery 与子会话 Node 测试，及 Go DSH executor/session-tree/checkpoint 测试。覆盖模型调用期间投递、序列顺序、回执丢失重放、领取后重启不重复、Session 隔离、至少两个子 Agent 并行与主 Agent据实汇总。长历史 PostgreSQL/Node DSH E2E 属于本域；实际 PC/iOS、真实模型与耐久验收交 S07。

## 会话完成回报

按[总控交接格式](00-并发实施总控.md#5-会话交接格式与合并规则)返回 Hermes 配对测试、DSH 插件功能证据、消息状态/检查点变化和未测范围。不要编辑共享 TODO/验证记录。
