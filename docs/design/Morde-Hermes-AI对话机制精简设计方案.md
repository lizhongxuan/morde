# Morde 移植 Hermes AI 对话机制：精简设计

> 状态：设计方案，尚未实现或验收。Morde 云端 Agent 继续只使用 DSH Runtime。

## 1. 目标

让用户在 PC 或 iOS 持续对话：长历史不会阻断后续消息，任务执行中仍能交流，独立任务可以并行，断线后可以接续。移植 Hermes 的会话、上下文压缩、历史找回和忙碌输入处理逻辑，尽量保持上游流程、边界和失败处理一致。

当前 Morde 已有 PostgreSQL 会话、Task、历史搜索及 DSH 压缩适配，但路由读取旧请求时存在 4,096 字节候选限制，长前文可能使后续短消息失败。DSH 的任务内压缩也不能代替跨 Task 的会话历史管理。

## 2. 会话与任务

```text
PC / iOS → 持久化会话消息 → DSH 会话协调 Agent
                               ├─ 普通问答 / 澄清 → 回复会话
                               ├─ 独立工作 → Task A、Task B 各自执行
                               └─ 追问 / 修改 / 停止 → 关联指定 Task
```

一个会话有多条消息和零个或多个 Task；一项 Task 可关联多条后续消息。每个 Task 有自己的 DSH 执行上下文，可按需使用工具和 DSH 子 Agent；工具调用或子 Agent 不自动成为新的用户 Task。任务进度留在任务详情，结果关联原会话，供协调 Agent 汇总。独立 Task 可并行，停止一个不停止其他独立任务。

当前实现按消息启动独立的 DSH coordinator session，完成的任务通过结果事件再次进入协调路径；图中的“协调 Agent”是会话级职责，不假定同一个进程一直运行。

## 3. 移植方案

| Hermes 机制 | Morde 接入方式 |
|---|---|
| **完整会话与受理** | PC/iOS 消息先经服务端鉴权、幂等保存到 PostgreSQL，再进行处理；会话 ID 保持稳定。前序消息 ID 用于关联，不作为发送锁。删除“旧候选超过 4,096 字节就拒绝新消息”的做法，改用有界上下文和可追溯的原文引用。 |
| **上下文构建** | 按模型的可用 Token 预算组合系统提示、当前消息、近期历史、已有摘要和必要的任务状态；优先使用 Provider 的真实用量，估算时标明来源。原始消息始终保留，摘要不冒充原文。 |
| **双层压缩** | 在新消息进入 Agent 前做 Hermes 式入口检查；在 DSH Session 执行中做压缩。移植 Hermes 的窗口规划、工具结果裁剪、Lean 摘要、用户原话与精确锚点保留逻辑；按实际模型窗口、输出预留及上游阈值规则触发，不能只照抄一个百分比。 |
| **历史找回与记忆** | 复用并按需扩展 Morde 当前会话历史搜索工具，从授权原文中按关键词或消息位置取回片段；摘要和被裁剪的工具结果带恢复线索。跨会话的个人记忆另按现有授权范围处理。 |
| **执行中接收消息** | 先保存新消息，再区分普通问答、进度查询、任务补充、停止和独立新工作。轻量补充通过 Runtime 桥接 DSH 的 `agent.steer()`，在目标 Agent 的下一步边界接收；需要等下一轮时用 `followup()`。持久化输入、投递确认和实际领取状态，恢复时按消息 ID 对账，避免重复或误报送达。 |
| **并发与恢复** | 借鉴 Hermes 按已解析 Session ID 保护“读历史 → 执行 → 写回”的租约；Morde 使用跨进程有效的锁或栅栏。只串行化同一可变 DSH Session 的写入，不阻断消息受理或其他独立 Task。摘要提交校验版本和来源范围；失败保留原文并有限重试，降级时标明不完整。 |

已有 DSH `BasicCompactionEngine`、TokenMeter、ToolResultPruner 和 Agent 收件箱优先复用。若压缩接口不能复刻 Hermes 的窗口规划等行为，在 DSH 的 `CompactionEngine` 接口内实现兼容插件；同一 Session 只启用一套压缩决策，不引入 Hermes Agent Loop 或第二套 Runtime。模型继续通过 Morde 网关，用户隔离、工具授权和审批沿用现有机制。

DSH 已提供 `steer()` / `followup()`。Morde 已有 `message_delivery` 投递通道，当前接收端使用 `followup()`；任务修改路径仍会先暂停旧执行。需要扩展现有通道接入 `steer()`，并做好 Task、Session、授权版本和恢复对账。无法安全投递时继续保留输入，排队或走受控版本切换，不声称已经送达。

## 4. 源码对照与移植原则

- 窗口与压缩：Hermes [`context_compressor.py`](../../project/hermes-agent/agent/context_compressor.py)、[`context_compressor_window.py`](../../project/hermes-agent/agent/context_compressor_window.py) 和 [`run_turn_hygiene.py`](../../project/hermes-agent/gateway/run_turn_hygiene.py)；接入 Morde [DSH 压缩适配](../../runtime/morde-cloud-runtime/src/engine.mjs#L1127)。
- 历史与并发：Hermes [`session_search_tool.py`](../../project/hermes-agent/tools/session_search_tool.py)、[`turn_lease.py`](../../project/hermes-agent/gateway/turn_lease.py) 和 [`session_busy_controls.py`](../../project/hermes-agent/gateway/session_busy_controls.py)；复用 Morde [历史搜索](../../server/internal/app/cloud_dsh_history_tools.go#L31)、[会话路由](../../server/internal/store/postgres/conversation_route_actions.go#L942)和 [DSH Agent 消息接口](../../project/deepseek-harness/packages/core/agent/src/runtime-types.ts#L234)。
- 逐模块保留上游状态机、窗口边界、提示模板、失败及重试分支；只为 DSH、PostgreSQL、Morde 模型网关和权限边界做必要适配。用同一输入和固定结果对照两边的窗口选择、摘要与恢复行为。
- 本地 Hermes 源码目前没有独立 Git 元数据，不能确认上游 commit。实施前应记录来源版本或关键文件的完整哈希，核对逐模块差异，并按 [MIT 许可证](../../project/hermes-agent/LICENSE)保留版权和许可声明。

## 5. 验收

1. 先发送超过 18 KB 的消息，再说“你用英语回答我”：新消息结合前文得到英语回复，不因旧消息大小报上下文不可读。
2. 对话超过模型窗口后，仍能回答早期约束；需要精确细节时可查回授权原文，原始记录在 PC/iOS 可查看。
3. Task A 执行时继续普通对话、补充 A，并发起独立 Task B；补充消息有真实的接收状态，停止 A 后 B 继续，结果和进度归属正确。
4. 两端重连或重试同一消息不重复创建 Task 或重复执行已确认操作；摘要失败或中断恢复时不丢历史、不伪装成已完成。
5. 不同账号不能互相检索会话；工具调用与子 Agent 仍受原有授权和审批约束。涉及 PC 的交互从实际 `morde-pc` 验收，跨端场景在 iOS 客户端另行验证。

实施分工与最终验收见[实施 TODO 总览](Morde-Hermes-AI对话机制实施TODO/00-总览与协作契约.md)；详细背景和对比见[会话与上下文机制移植设计](Morde-Hermes会话与上下文机制移植设计方案.md)及[四项目流程对比图](ice-cream-blue/morde-ai-conversation-architecture-comparison.html)。
