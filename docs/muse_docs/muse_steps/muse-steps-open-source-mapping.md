# Muse 每一步最像哪个开源项目：Morde 复刻对照表

> **历史项目映射，仅供查找源码和借鉴思路：**表内旧的 Morde 落地方案、并发/深度数字、重试梯子、关键词合并、强制追问和换模型家族等不构成当前要求，也不代表 Muse 内部机制已被证实。执行以[最终流程与开发设计](<Morde 复刻 Muse 最终流程与开发设计.md>)、[一体化实施与验收 TODO](<Morde 复刻 Muse 一体化实施与验收 TODO.md>)及项目 `AGENTS.md` 为准；复用前核实当前 DSH 插件、版本和授权边界。

> 版本：v1.0 · 2026-11-23
> 依据：`muse-harness-reverse-engineering.md` 和 `muse-harness-replication-test-cases.md`。本文保留历史开源映射；当前目标与具体源码落点以《Morde 复刻 Muse 最终流程与开发设计》和各步骤 TODO 为准。
> 阅读规则：每条“Muse 是这样”都带证据等级。**已证实**才当结论用；**合理推断 / 未知**的步骤，其项目映射也标“待验证”，不是定论。项目机制凡是本次查不到出处的，标“未核实”，不编造。Grok Bot 只作**方法论参照**（土豆姐团队的做法），不是开源代码，不要写成“抄 Grok Bot 的代码”。
> 核实方式：Hermes-agent 与 nanoMuse 是此前已精读 / 自有代码（hermes 固定提交 `0d49d6d9`，见逆向工程 §21–§22）；其余项目本次用公开搜索核实了关键机制（OpenHands 事件流、LangGraph 监督者模式、Claude Agent SDK 钩子与权限、微软 Agent Framework 五种编排、Agno 团队模式、Codex 双轴审批、OpenClaw 三层记忆、LibreChat 延迟工具）。

## 总表：11 个步骤 → 主参考 + 备选

| # | Muse 步骤（逆向工程章节） | 主参考项目 | 备选 | 抄它哪一块 | 关键差别（不要照搬的部分） |
|---|---|---|---|---|---|
| 1 | 收件入口（§1） | **OpenClaw** | LibreChat | Gateway 单进程接多频道、DM 归一会话、语音备忘录路由 | OpenClaw 是单机网关形态；Muse 是一对一、无群聊；Morde 的幂等收件比它更严 |
| 2 | 意图路由与任务拆分（§2） | **微软 Agent Framework（Magentic）** | LangGraph（Supervisor） | 双账本：Task Ledger（事实/计划）+ Progress Ledger（每轮评估前进/循环/下一发言者） | Magentic 的 plan sign-off 每次都问、成本不可预测；LangGraph 的显式状态图编程模型不搬；合并窗口与同话题判定无对照，须自研 |
| 3 | 并发派发与协调员监视循环（§3） | **LangGraph** | 微软 Agent Framework（Concurrent + Magentic） | Supervisor 模式 + Send API 并行扇出 + 检查点断点续跑 | 无上限扇出不搬（Morde 上限 10 按序回填，已定 nanoMuse 方案）；Handoff 的控制权完全转移不搬（Muse 是主 agent 保留所有权） |
| 4 | 工具执行工作项（§4） | **OpenHands** | Claude Agent SDK | 事件溯源账本（EventStream）+ report 七要素 + StuckDetector 防循环 + 回放 | Controller–Runtime 全套重写 DSH 不搬；CodeAct 整套是 P2 候选，不是现在 |
| 5 | 审批（§5） | **Codex CLI** | Hermes-agent | 双轴模型：sandbox 轴 × approval 轴独立；分档审批；scoped allow；超时 fail-closed | 单人本地 CLI 形态不搬；`--yolo` / `danger-full-access` 绝不能进多租户服务端；hermes 的 7 步顺序与模型代批不搬 |
| 6 | 验证（§6） | **OpenHands** | LangGraph | 跑真检查验证（Edit-Run-Verify）+ review gate + 覆盖率报告 + 证据绑定 | OpenHands 整套 eval harness 不搬（Morde 用 Inspect AI + 原生套件）；独立验证者（换模型家族）无开源对照，是新造，方法论参照 Grok Bot |
| 7 | 交付（§7） | **LibreChat** | OpenClaw | 聊天内 Artifact 渲染 + 主对话只出最终结果 + 要点贴消息里 + proactive 投递节律 | ChatGPT 克隆 UI 形态不搬（Morde 按冰淇淋蓝三端设计）；OpenClaw 单机网关形态不搬；提问触发阈值未知，须自研 |
| 8 | 记忆与纠正沉淀（§8） | **OpenClaw** | Agno | 三层记忆文件（MEMORY.md + 每日笔记 + Dreaming）+ 压缩前静默存档 + 每小时整理节律 | 单机文件形态不搬（Morde 多租户隔离、场景层与个人层物理分开）；纠正变检查自动路径无开源对照，是新造 |
| 9 | 失败与限流（§9） | **Hermes-agent** | LangGraph | 33 值错误分类 + jittered backoff + 自动恢复梯子 + terminal 五类必 abort + 失败回流 | 单机内存 tally 不搬（Morde 按租户持久化）；Muse 失败时主对话措辞未知，映射待验证 |
| 10 | 上下文管理（§10） | **Hermes-agent** | Claude Agent SDK | 两层阈值 + 先剪枝后总结 + 保护 head+tail + 失败冷却 + 轮首 preflight | 全量历史重放不搬；in-place session 压缩不搬（Morde 摘要是版本化密封文档）；触发阈值数字不直接抄，按 Morde 实测定 |
| 11 | Bug 发现处理（§12–§14） | **无直接对照** | OpenHands（轨迹回放）+ LangGraph（time-travel） | 五步法本身是方法论，不是产品功能；最接近的是事件流回放复现 + 失败轨迹进数据集 | 不要写成“抄某项目”；复盘模板八栏与“纠正先写回归用例”是 Morde 新造 |

## 候选项目速览（本文用到的 10 个）

| 项目 | 一句话定位 | 本次核实依据 |
|---|---|---|
| OpenClaw | 单机个人 AI 网关：一个 Gateway 进程接 20+ 消息频道，记忆是 Markdown 文件 | 公开搜索核实：Gateway、频道表、三层记忆文件、Heartbeat 30 分钟、cron、SKILL.md |
| LibreChat | 开源多模型聊天平台：多用户登录、Agents（MCP 工具/文件搜索/代码解释器/延迟工具）、聊天内 Artifact | 公开搜索核实：README 功能清单、Agents 文档（延迟工具、SKILL.md、Artifacts） |
| 微软 Agent Framework | Semantic Kernel + AutoGen 合并后的官方 SDK：Workflows 五种编排（Sequential / Concurrent / Group Chat / Handoff / Magentic） | 公开搜索核实：Magentic 双账本、五种模式、plan sign-off、1.0 GA |
| LangGraph | LangChain 的图编排库：节点 + 边 + 检查点 + 中断；Supervisor 模式、Send API 扇出 | 公开搜索核实：Supervisor、Send API、检查点、interrupt_before/after、time-travel |
| OpenHands | “一切皆事件”的 Agent 平台：EventStream 事件溯源、AgentController 状态机、CodeAct、StuckDetector | 公开搜索核实：events/stream.py、AgentController、delegates、V1 SDK 重组 |
| Claude Agent SDK | Anthropic 官方 Agent 开发包：PreToolUse 钩子、五层权限求值、子 agent 隔离上下文、自动压缩 | 公开搜索核实：钩子清单、权限求值顺序、权限模式、子 agent 不可嵌套、compact_boundary |
| Codex CLI | OpenAI 的本地终端编码 agent：sandbox 轴 × approval 轴双轴控制、AGENTS.md | 公开搜索核实：三档 approval、三档 sandbox、双轴独立；注：0.166.0 后 `--full-auto` 移除、改为 Approve for me 评审 agent，双轴思想不变 |
| Hermes-agent | 单机 Python 个人 agent：turn 契约、压缩、错误分类、审批、成本门（Morde 已精读） | 此前精读，固定提交 `0d49d6d9`，见逆向工程 §21–§22 |
| Agno | Python 多 Agent 框架 + AgentOS 服务：Team 三种模式、会话记忆、RBAC | 公开搜索核实：Team（coordinate / route / collaborate）、团队级会话记忆、AgentOS |
| nanoMuse | Morde 自有的 iOS 本机 agent 循环（模型直连），只借机制不借形态 | 自有代码：并行上限 10 按序回填、ToolLoopDetector、Preflight、RequestBudget |
| Grok Bot | 土豆姐团队的方法论（非开源代码）：coordinator 只拆分派发、信任阶梯、独立验证者、评估循环 | 此前研究，见 13 模块清单；本文只作方法论参照 |

---

## §1 收件入口（逆向工程 §1）

- **Muse 这一环节的要点（已证实）**：入口面很宽但都是一对一：web / iOS / Android / Mac 应用 + Chrome 扩展侧边栏 + WhatsApp（唯一有指南的侧边 provider；Discord、SMS 无此连接）+ 语音（实时对话、备忘录转写、多声部生成，无环境监听）。每会话一个 chat UUID，transport 不产生第二份 transcript。任务行是 `activity_monitor_threads` 的一行（name、subtitle、status、finish_status），442 个任务全部 finished，副标题随进展改写。收件的幂等、去重、定序机制**未知**。
- **主参考：OpenClaw**。对应机制：一个 Gateway 进程同时接 22+ 频道（WhatsApp、Telegram、Discord、Slack、Signal、iMessage、WebChat 等）；频道适配器把各平台消息归一化成统一内部格式，再路由进同一个 agent 上下文；DM 合并进主会话（群聊另行多方追踪）；语音备忘录走同一条路由；每 30 分钟一次 Heartbeat 定期轮询收件箱 / 日历 / 通知。文件依据：`~/.openclaw/workspace` 下 `MEMORY.md`、`memory/YYYY-MM-DD.md`、`HEARTBEAT.md`；`sessions_*` 工具组。
- **能抄什么**：频道归一化思想——不同端（PC、iOS）进来的消息先统一成内部格式，再进同一套 Harness；DM 归一会话（同一账号两端接续同一会话）；语音备忘录转写后走同一路由。
- **不能抄什么**：OpenClaw 的单机 Gateway 形态（Morde 是多租户服务端，入口、会话、记忆都要按租户隔离）；群聊（Muse 明确一对一，Morde 不做群聊）；它的本地文件工作区。
- **Morde 落地注意**：Morde 的收件已经比 OpenClaw 更严——`AcceptConversationMessage` + `client_message_id` 幂等（同 ID 不同内容拒绝）+ 服务端 `sequence` 定序，复刻时不要降级。入口已定 PC 主端 + iOS 看问批，不新增渠道。验收：TC-1.1–1.8。
- **备选：LibreChat**。多模型聊天 UI + 多用户登录（OAuth2 / LDAP）+ 会话全文搜索。只参考“多用户下会话隔离与搜索”的产品形态，不参考它的 ChatGPT 克隆 UI。

## §2 意图路由与任务拆分（逆向工程 §2）

- **Muse 这一环节的要点（已证实一半 + 未知）**：一条消息不严格等于一个任务，31% 的任务链 ≥2 条消息（829 链接 / 352 任务），但归并规则**未知（U2，最大缺口）**。Skill 路由有明确机制（`skill_search` 按任务找 skill，SKILL.md 写明触发条件，无 skill 走 `custom-connector-setup`）。Connector 路由边界明确（只有文档列出的命令才存在）。请求分类字段存在（`request_origin`、`transcript_surface`、`root_work_class`），分类算法**未知（U1）**。Coordinator + worker 是文档级推荐模式（大文档审阅：一个 coordinator + 若干 extraction worker + 一个 checking worker；coordinator 定分工、合并发现、保留最终判断权）。Wide-research 模式：先去重归一化输入，再派 manager，输出合同固定为 total / success_count / failure_count / results / failures / notes。
- **主参考：微软 Agent Framework 的 Magentic**。对应机制：一个 manager agent 动态规划 + 重规划 + 选下一个 agent；外层循环维护 **Task Ledger**（事实、猜测、当前计划，停滞时修订），内层循环维护 **Progress Ledger**（每轮 JSON 评估：是否完成、是否循环、是否前进、下一发言者是谁）；护栏是 `max_round_count` / `max_stall_count` / `max_reset_count`；另有 plan sign-off（计划先给人过目）。
- **能抄什么**：双账本思想正是 Morde Planner 的蓝本——Task Ledger 对应 brief（目标 / 范围 / 验收标准 / 验证方法 + 成本估算），Progress Ledger 对应协调员每轮的“是否前进 / 是否循环 / 下一个谁干”评估；停滞时重规划（reset + replan）对应 Morde 的“无验收标准的 brief 不派发，直接 Clarify”。Skill 路由另参考 OpenClaw 与 Claude Agent SDK 的 SKILL.md 发现机制（按描述匹配、按需加载）。
- **不能抄什么**：Magentic 的 plan sign-off 每次都问（Morde 只在方案那一步卡一次审批）；Magentic-One 的成本不可预测（无预算上限，Morde 必须有 fail-closed 预算）；微软的 Foundry 托管形态。
- **Morde 落地注意**：Planner 是 P0-3，合并窗口是 P0-4。**合并窗口与同话题判定无开源对照**——Muse 的归并规则未知（U2），Magentic 与 LangGraph 都没有“同话题多轮归进同一任务”的机制，必须自研：60s 可配窗口 + 轻量判定（关键词重叠 + 分类器 `join`/`steer` 信号），P0 实测定阈值。本节映射标**待验证**（U1、U2 未知）。验收：TC-2.1–2.12。
- **备选：LangGraph 的 Supervisor 模式**。supervisor 节点读任务、委派给专业子 agent（researcher / coder / reviewer），每个子 agent 是独立子图或节点；条件边（conditional edges）做路由分支；`Send` API 做并行扇出。只借“监督者 + 条件边路由”的结构，不搬显式状态图编程模型（Harness 用任务状态机即可）。

## §3 并发派发与协调员监视循环（逆向工程 §3）

- **Muse 这一环节的要点（已证实）**：主 agent 只做简单对话，复杂 / 并发工作派后台 agent，时间戳重叠证明不是排队；单日最多 63 个任务。子 agent 嵌套到 2 层（230 次派生：depth=1 有 159 次、depth=2 有 71 次占 31%；3 层是否存在未知）。子 agent 类型有分工：通用 54%、`artifact_builder` 27 次、`spaces_builder_lite_edit` 24 次、`spaces_builder_lite` 22 次、`browser_task` 12 次、`deep_research` 11 次。协调员每事件决策一次（20,787 条：`nothing_to_do` 61%、`worker_or_coordinator_completed` 38%、`notify_main_agent` 仅 1.2%）。派生本身也是一条工作项。官方推荐“一个 coordinator per objective”，明确反对从 root 直接扇出多个平级 worker。后台结果以 hidden handoff 交回主 agent，由主 agent 决定用户看到什么；worker 不能要求主 agent 做更多工作。
- **主参考：LangGraph**。对应机制：Supervisor 模式（监督者节点委派，专家子图执行）；**Send API** 并行扇出（orchestrator 返回 `Send("worker", {...})` 列表，各 worker 独立状态片并行跑，结果由 reducer 归约合并，全部完成才继续）；检查点（MemorySaver / PostgresSaver）让长任务断点续跑、time-travel 回放；超步（super-step）并行执行模型。
- **能抄什么**：Send API 式扇出（派发列表 + 各 worker 独立上下文片 + reducer 归约）；检查点断点续跑（任务暂停几小时回来接着跑）；“一个 coordinator per objective”；handoff 三件套（已完成什么、证据在哪、还差什么）；协调员决策比例——默认 `nothing_to_do`，只有完成或需通知主 agent 才上报（Muse 实测 notify 仅 1.2%，Morde 验收时对标这个量级）。
- **不能抄什么**：无上限扇出（Morde 并行上限 10、按序回填，已定 nanoMuse 方案，不用 hermes 的 8 workers 也不用 LangGraph 的无上限）；LangGraph 的显式图编程（Morde 用 Harness 任务状态机 + DSH）；Handoff 模式的控制权完全转移（Muse 是主 agent 保留所有权，worker 只回 handoff，不反向指派主 agent）。
- **Morde 落地注意**：派发判断条件（复杂度阈值、耗时预估）Muse 侧未知（U5），最大并发数、子 agent 配额、排队策略也未知，本节映射标**待验证**，须自定。深度上限：Muse 实测到 2 层，Morde 暂定限 1 是设计选择（复刻设计 §0(a)），放开到 2 层待用户拍板。验收：TC-3.1–3.13。
- **备选：微软 Agent Framework**。Concurrent 模式（全部 agent 同时跑，聚合器汇总）+ Magentic 的 manager 每轮检查进度。只借“并发 + 每轮检查进度”的编排思想，不搬 Foundry 托管。

## §4 工具执行工作项（逆向工程 §4）

- **Muse 这一环节的要点（已证实）**：工作项 = 一次工具调用。点开 report 结构固定：**在哪执行 + 完整命令 / 参数原文 + 退出码 + 耗时（毫秒）+ 工作目录 + 输出原文（长输出截断并注明截了多少行）+ 一句人话结论**。进行中的调用先以占位行出现（`running`），跑完再换正式标题。失败的工作项留在清单里，不删不改写（failed 214 条占 1.3%，任务没有因此停下）。进行中任务可取消（`cancelled` 96 项是常规终态）。工具调用全记录：`tool_calls` + `tool_outputs` + `events`（`event_seq` 单调的有序账本）。恢复四步：读持久期望态 → 恢复前置条件 → 逐组件对账 → 从检查点续跑。工具重试策略、超时阈值、熔断、预算上限**未知（U6）**。
- **主参考：OpenHands**。对应机制：“一切皆事件”（everything is an event）：`EventStream`（`openhands/events/stream.py`）是持久化、去敏的事件日志，Agent、工具执行、微 agent 检索、安全分析、UI 流式全是这个日志上的解耦订阅者；`AgentController` 是状态机（`LOADING → RUNNING → FINISHED`），只写 Action 进事件流、由 Runtime 执行后写回 Observation；每条记录带完整命令、退出码、耗时、输出原文；`StuckDetector` 防循环；`ReplayManager` 用同一条事件流重放；`agent_controller.py` 可派生 delegate 子控制器；V1 SDK 仍保留事件溯源状态管理与 `security/`（SecurityAnalyzer、ConfirmationPolicy）。
- **能抄什么**：事件溯源账本（append-only 事件流，状态靠重放还原）——这正是 Morde 9 种事件本体 + `task_events` 的蓝本；report 七要素（照抄）；StuckDetector 思想（防循环检测的原型）；失败留痕不删、取消是常规终态；恢复四步（期望态 → 前置条件 → 对账 → 检查点续跑）。
- **不能抄什么**：OpenHands 的 Controller–Runtime 全套重写 DSH（DSH 是 Morde 唯一执行层，不换不重写）；它的 per-session Docker runtime 形态（Morde 是 DSH + 云电脑）；CodeAct 整套是 P2 候选（`dsh-code-runtime` 评估），不是现在；它的微 agent 知识层（Morde 的 skill 是注册表 + SKILL.md 形态）。
- **Morde 落地注意**：P0-8 定 Step Detail 的 report 固定结构（七要素）写进 `DetailState` 的 schema 约定；工作项 id 前缀照抄 Muse 的命名（`root-tool-…` / `root-subagent-dispatch-…` / `subagent-start-…` / `child-tool-…` / `subagent-finish-…`）；重试 / 超时 / 熔断无 Muse 证据（U6），须自研（见 §9）。验收：TC-4.1–4.14。
- **备选：Claude Agent SDK**。PreToolUse / PostToolUse / PostToolUseFailure 钩子（执行前拦截、执行后审计、失败后处理）；子 agent 隔离上下文（只回最终结果）；transcript 持久化。只借钩子式审计与隔离思想，权限五层求值见 §5。

## §5 审批（逆向工程 §5）

- **Muse 这一环节的要点（已证实一半 + 未知）**：写与付费只按次批；审批字段在 `ApprovalRequest` 上（对象 / 内容 / 影响 / `operation` / `operation_binding` / `operation_binding_type` / `allowed_roles`）；审批类型有四档：**`one_time` / `session` / `always` / `scope`**（`always` 仅适用于写操作，不适用于不可逆动作）；每次对话默认 `one_time`，只保留“下次仍问”；`always` 必须由用户主动选择。卡片形态与字段细节**未知（U8）**；成本门、不可逆动作的自动识别、敏感操作的完整分级**未知（U7）**。
- **主参考：Codex CLI**。对应机制：两个独立正交轴——**sandbox 轴**（read-only / workspace-write / danger-full-access）与 **approval 轴**（suggest / auto-edit / full-auto），两个维度可以独立组合（例如 workspace-write + auto-edit）；approval 模式可随时在命令行切换；`AGENTS.md` 作为持久指令文件；`permissions.md` / `sandbox.md` 是官方文档。注：近期版本（0.166.0）移除了 `--full-auto`，改为 “Approve for me” 评审 agent 批准 sandbox 跨越——双轴思想不变，开关形态在变。
- **能抄什么**：双轴分离——技术上能做什么（sandbox 能力）与什么时候问（approval 档位）分开定；分档审批（读 / 写 / 外发 / 不可逆）；卡面成本四行（hermes 补充，见备选）；scoped allow（发给某一人可记“以后都允许”，对应 Muse 的 `scope`）；超时 fail-closed（默认拒绝）。
- **不能抄什么**：单人本地 CLI 形态（Morde 是多租户服务端，审批必须按租户隔离、按人限可见范围）；`--yolo` / `danger-full-access`（绝不能进多租户服务端）；Codex 的单人本机信任假设（Morde 每个写操作都要过审批卡，`always` 只开写操作）。
- **Morde 落地注意**：P0-5（审批卡体字段 + 风险 / 费用 / 替代 / 撤销）+ P0-7（工具注册表 + 风险分级 + 64 硬上限改 top-k）。Claude Agent SDK 的权限五层求值顺序也可直接抄：Hooks（PreToolUse）→ deny 规则 → 权限模式（default / acceptEdits / bypassPermissions / plan / dontAsk）→ allow 规则 → canUseTool 回调，**deny 永远赢**；`bypassPermissions`（yolo 模式）Morde 不提供。卡面字段细节无 Muse 证据（U8），须自研，验收以 PC-CHAT 019 为准。验收：TC-5.1–5.17。
- **备选：Hermes-agent**。`tools/approval.py` 7 步（卡面四行：本次 token、累计 token、成本、预算；Smart guardian；否认熔断器；once / session / always）。复刻设计已定：卡面四行成本抄 hermes，7 步顺序与 bypass 不抄（C 级，见 hermes 需求文档 §2.6）。hermes 的 Smart guardian 是单机形态，Morde 改为服务端按租户隔离的审批。

## §6 验证（逆向工程 §6）

- **Muse 这一环节的要点（已证实一半 + 未知）**：验证 = 跑真检查，不是看模型自报。已证实的证据链：开发类任务普遍有编译 / 测试报告；Morde 前身的 bug 修复由独立验证者检查（检查者 ≠ 修复者）；验证者给 PASS / PASS+NOTES / FAIL，失败必有原因；检查标准是公开的（检查者自备清单）；证据绑定（引用绑文件 + 页 / 节）；结果回流（验证失败的直接原因写成后续规则）。错误类型与触发条件**未知（U9）**；错误分类、纠正触发阈值、验证者选型（同模型 / 换模型 / 换厂商）**未知（U10）**；独立验证者是独立 agent 还是同一系统内的检查 worker，**未证实**。
- **主参考：OpenHands**。对应机制：`evaluation/` harness（SWE-bench 式轨迹验证：跑测试看修复是否通过，`EvaluationMetrics` 包含 `verified_coverage_success_count` / `verified_coverage_total`）；Edit-Run-Verify 循环；`security/` 安全分析（SecurityAnalyzer）作为审查子 agent 审计算子；micro-agent / agent registry 的验证子 agent；`openhands/controller/state/state.py` 是状态 + 指标的模型。
- **能抄什么**：验证 = 跑真检查（编译 / 测试 / 截图自检 / 预览，四项都过才算交付）；覆盖率报告（verified coverage success_count / total）；证据绑定（引用绑定文件 + 页 / 节）；失败回流（失败的直接原因写成后续规则）；审查子 agent（审计算子）作为独立验证者的原型。
- **不能抄什么**：OpenHands 整套 eval harness 的形态（Morde 用 Inspect AI + 原生套件，44 条 live 用例，见 hermes 需求文档）；它的单机 Docker runtime 形态；它的微 agent 知识层。
- **Morde 落地注意**：**独立验证者（换模型家族）无开源对照**——OpenHands 的审查子 agent 与 LangGraph 的 review gate 都是“同一系统内的检查”，不是“换模型家族的独立 agent”。最接近的是 Grok Bot 方法论（独立验证者 + 盲测 + 跨模型 Judge），但那是方法论不是开源代码，标方法论参照。验证是 P1-1（Verification Skill + 证据包 + 独立验证者），与 P0-5 的审批卡证据绑定一起做。验证者选型（同模型 / 换模型 / 换厂商）是 hermes §7 待用户定的事之一，不要替用户定。验收：TC-6.1–6.9。
- **备选：LangGraph**。`interrupt_before` / `interrupt_after` 做 review gate（发布前暂停等人批）；检查点断点续跑 + time-travel 回放调试；write-and-review 图。只借“交付前暂停等人批”的 gate 思想，不搬显式图。

## §7 交付（逆向工程 §7）

- **Muse 这一环节的要点（已证实）**：交付产物分三类——**文档**（长文本 / 代码 / 报告，带内联可操作代码块与引用）、**页面**（artifact，带预览与可操作卡片）、**交互应用**（Spaces / 仪表盘 / 游戏）。入口始终是主对话；主对话只放最终结果，过程噪音不进入主对话（中位回复 ~150 字符、75% 事件不公开可见，2026-11-09 快照）；选项卡是首选载体（点选项直接发成一条消息）；提问只在阻塞或需拍板时出现（6.8%）。**未知（U19）**：展示效果如何决定、提问触发阈值（只阻塞时问、还是问了也答）、用户点“不喜欢”后的机制。
- **主参考：LibreChat**。对应机制：ChatGPT 风格聊天 UI + 多模型切换；Agents（无代码搭建，MCP 工具、文件搜索、代码解释器、OpenAPI Actions、延迟工具）；**Code Artifacts**（聊天内直接生成并渲染 React / HTML / Mermaid 图）；文件上传下载；多用户登录（OAuth2 / LDAP）+ 会话全文搜索（MeiliSearch）；Agent 可委派给独立上下文窗口的子 agent。
- **能抄什么**：Artifact 三类形态（文档 / 页面 / 交互应用）与聊天内渲染；主对话只出最终结果（过程噪音收进任务详情）；要点贴在消息里、不只给链接（Muse 交付时已这么做）；proactive 投递的节律（每次 ≤8 条、每天 ≤2 次普通 / ≤3 次含时效、等用户醒着 + 对话安静时发）；LibreChat 的“延迟工具”（Deferred Tools：工具不在上下文里全加载，运行时按需通过 Tool Search 发现）——这是 Morde top-k 检索的直接原型，见 P0-7。
- **不能抄什么**：ChatGPT 克隆 UI 形态（Morde 按冰淇淋蓝三端设计，PC 为聊天主界面）；Agent Marketplace（Morde 的场景包是文件形态，走云电脑复制，不走商店）；它的单机 / 自托管形态（Morde 是多租户服务端）。
- **Morde 落地注意**：提问触发阈值未知（U19），须自研——当前 Morde 的规则是“只在阻塞或需拍板时提问”，先按这个做，实测再调。选项卡载体是 Muse 实证，直接抄。交付是 P1-3（交付类型与载体）+ P2-4（CodeAct 第二条工具路）。验收：TC-7.1–7.12。
- **备选：OpenClaw**。Heartbeat（每 30 分钟定期轮询收件箱 / 日历 / 通知）+ cron 定时任务 + inferred commitments（从对话里推断出的承诺，通过 heartbeat 投递）。只借“定期轮询 + 定时投递”的节律思想，不搬单机网关形态。

## §8 记忆与纠正沉淀（逆向工程 §8）

- **Muse 这一环节的要点（已证实）**：记忆是 **Markdown 双轨**（文件可读可改 + claims / embeddings 索引）；`MEMORY.md`（长期事实、偏好、承诺）+ 每日笔记（`memory/YYYY-MM-DD.md`）+ 关系页（`people/*.md`、`groups/*.md`）；每小时一次后台整理；纠正时把旧主张标 `superseded`（不删）；记忆是按人（user）隔离；纠正变检查（“以后别再犯同样错误”）是明确机制——检查来自纠正、纠正变 lint / 检查 / skill；每次纠正都写入规则表；遗忘有正式流程（删除的定义、范围、验证）。**未知（U13）**：记忆写入的触发与去重细节、引用合并策略、容量策略。
- **主参考：OpenClaw**。对应机制：三层记忆文件——`MEMORY.md`（长期记忆）、`memory/YYYY-MM-DD.md`（每日记忆）、`DREAMS.md`（巩固后的长期记忆）；**Automatic Memory Flush**（压缩前静默存档对话快照）；**Dreaming**（夜间巩固 cron，把每日记忆整理进长期记忆）；`memory_search` / `memory_get` / `memory_explain` 工具；`HEARTBEAT.md`（定期任务）；SKILL.md 发现机制（按描述匹配、按需加载）。
- **能抄什么**：Markdown 双轨（文件可读可改 + claims / embeddings 索引）——Morde 已有这个形态，直接对齐；每小时整理节律；Dreaming 夜间巩固（把每日笔记整理进长期记忆）；压缩前静默存档（Automatic Memory Flush）；纠正带 supersedes 关系（旧主张标 superseded 不删）；遗忘正式流程（删除的定义、范围、验证）。
- **不能抄什么**：OpenClaw 的单机文件形态（Morde 是多租户服务端，记忆按租户隔离、场景层与个人层物理分开）；它的 Gateway 进程形态；它的群聊记忆。
- **Morde 落地注意**：纠正变检查的**自动路径无开源对照**——hermes 也没有自动路径（它的纠正只进 rules，不自动变检查）；最接近的是 Grok Bot 方法论（纠正变确定性检查），但那是方法论不是开源代码，标方法论参照。记忆同步是 P1-7（每日记忆 → 长期记忆 → 场景包），纠正沉淀是 P2-2（信任阶梯 + routine）。记忆写入触发与去重细节未知（U13），须自研。验收：TC-8.1–8.12。
- **备选：Agno**。会话记忆（session memory）与长期记忆分离；团队共享记忆（`share_member_interactions`）；PostgresDb 会话表（`session_table`）+ session summary manager；`enable_agentic_memory`。只借“会话记忆与长期记忆分离 + 团队共享记忆”的结构思想，不搬 AgentOS 整套控制面。

## §9 失败与限流（逆向工程 §9）

- **Muse 这一环节的要点（已证实一半 + 未知）**：失败时主对话不抛原始错误，而是给一句人话（“我这边出了点小状况，正在处理”）；失败不丢任务（任务行仍在清单里，状态 finished，副标题写失败原因）；失败原因进事件账本（`task_events`），不写进主对话；错误分类字段存在（`failure_reason` / `failure_code` / `failure_category` / `failure_stage`），但分类算法**未知（U18）**；失败时主对话的措辞与降级策略**未知**；重试策略、超时阈值、熔断、预算上限**未知（U6）**。
- **主参考：Hermes-agent**（已精读，固定提交 `0d49d6d9`）。对应机制：`error_classifier.py` 33 值枚举（`context_length_exceeded` / `rate_limit` / `api_error` / `timeout` / `network_error` / `auth_error` / `model_error` / `tool_error` / `unknown` 等）；`retry_utils` 的 jittered backoff（指数退避 + 抖动）；`turn_recovery_autorecover`（5 周期 15/30/60/60/60s，失败超 2 周期 abort，恢复后立即压缩一次）；terminal 五类必 abort（认证失败 / 网络失败 / 截断 / 空内容 / 过载）；`empty_response_guard`（空响应预算）；`context_overflow` 单独一类（只触发压缩，不触发重试）；失败 trace 自动进评测数据集。
- **能抄什么**：错误分类表（`context_overflow` 单独一类，只触发压缩）；自动恢复梯子（仅 transient 且未输出文本时）；terminal 清单（认证 / 网络 / 截断 / 空内容 / 过载必 abort 不重试）；失败回流（失败 trace 自动进评测数据集，同一错误第二次出现沉淀为 lint / 检查 / skill）；失败不丢任务（任务行仍在清单里，副标题写失败原因）。
- **不能抄什么**：hermes 的单机内存 tally（Morde 按租户持久化，见多租户适配四点）；它的 in-place session 压缩语义（见 §10）；它的单机 CLI 形态。
- **Morde 落地注意**：失败与限流是 P1-4（错误分类 + 恢复梯子）+ P1-6（Turn 契约）。Muse 失败时主对话的措辞与降级策略未知（U18），本节映射标**待验证**，须自研——当前 Morde 的规则是“失败不丢任务、失败原因进事件账本、不写进主对话”，先按这个做。验收：TC-9.1–9.8。
- **备选：LangGraph**。检查点断点续跑 + 超时恢复 + 中断后从断点恢复；retry policies；durable execution（长任务可暂停几小时回来接着跑）。只借“断点续跑 + 超时恢复”的思想，不搬显式图。

## §10 上下文管理（逆向工程 §10）

- **Muse 这一环节的要点（已证实一半 + 未知）**：分类时只带最近 8 条消息（每条 >4096B 截断为 head 1023B + `…` + tail 1023B）+ 一份密封版本化摘要（≤16 KiB）；任务 agent 初始输入 ≤128 KiB，目标 ≤32 KiB；历史按需取（3 个有界工具，`morde_current_conversation_history_search` 已有游标分页）；直接回复由分类调用本身写（不二次调用）；单条消息 >128 KiB 硬失败（`message_too_long`）；任务 16 轮循环内工具输出累积到 128 KiB 无压缩 / 剪枝（已知缺口）。**未知（U7）**：窗口 token 数、触发条件、摘要模型、保留策略、压缩失败时的降级。
- **主参考：Hermes-agent**（已精读，固定提交 `0d49d6d9`）。对应机制：`context_compressor.py` 两层阈值（50% 警告、85% 强制）+ `context_compressor_window.py` 选窗（先剪枝后总结、保护 head+tail、bridge 对齐工具调用组、一次辅助 LLM 总结、原文 stub 归档可读回）；失败冷却 600s + terminal 清单 + anti-growth 护栏；`turn_preflight_gate`（轮首三段压缩）；`micro_compact_after_turn`（turn 后微压缩）；`context_overflow` 单独一类（只触发压缩）。
- **能抄什么**：确定性剪枝 Phase 1-2（去重 / 截断 / 退役图片 / 丢空白 echo）；保护 head+tail；一次辅助 LLM 总结；失败冷却 + terminal 清单；轮首 preflight + turn 后 micro-compact；`context_overflow` 单独一类（只触发压缩，不触发重试）。
- **不能抄什么**：hermes 的全量历史重放（Morde 多租户 + 每消息分类，重放成本扛不住）；in-place session 压缩（Morde 摘要是版本化密封文档，不是原地改写）；触发阈值数字（50% / 85%）不直接抄，按 Morde 自己的模型与实测定。
- **Morde 落地注意**：上下文管理是 P1-4（压缩）+ P1-6e（Turn 契约）。Muse 的窗口 token 数、触发条件、摘要模型、保留策略未知（U7），本节映射标**待验证**。Morde 的两条线已定：分类快照（最近 8 条 + 密封摘要）不搬 hermes 的全量重放；任务循环内压缩是 P1 重点（hermes-compaction-window.mjs 已在 runtime 里，只缺调用点）。验收：TC-10.1–10.20。
- **备选：Claude Agent SDK**。自动压缩（接近上限时自动总结旧历史）+ `compact_boundary` 系统消息 + PreCompact hook（归档全文）+ CLAUDE.md 每请求重注入 + 子 agent transcript 不受主对话压缩影响。只借“自动压缩 + 压缩前归档 + 子 agent transcript 独立”的思想，不搬它的触发阈值数字。

## §11 Bug 发现处理（逆向工程 §12–§14）

- **Muse 这一环节的要点（已证实）**：五步法——**发现**（用户报 / 日志 / 评测）→ **定位**（单变量复现、对照实验）→ **验证根因**（修复前先证明根因，修复后回归验证）→ **防止复发**（纠正写成规则 / lint / 检查 / skill）→ **收尾纪律**（复盘模板八栏，含“之前说错的话”）。实测案例：Bug 3（两次说错根因后纠正）、上下文排查（2e86e33e）、协调员容量 + 摘要转发缺失（PR #2）。复盘模板八栏：发生了什么 / 根因 / 之前说错的话 / 修复 / 验证 / 防止复发 / 待办 / 影响。
- **主参考：无直接对照**。
- **为什么没有**：Bug 发现处理是**方法论**（发现 → 定位 → 验证根因 → 防止复发 → 收尾纪律），不是某个产品的功能。没有哪个开源项目把“调试方法”做成可抄的模块。OpenHands、LangGraph、Hermes-agent 都有“回放 / 复现”的机制，但那是工具，不是方法论本身。
- **最接近的替代（可借的机制）**：
  - **OpenHands**：EventStream + ReplayManager——用同一条事件流重放复现，是“单变量复现”的最接近实现；evaluation harness 的失败轨迹进数据集，是“失败回流”的原型。
  - **LangGraph**：time-travel 回放到任意检查点复现，是“定位”的最接近实现；检查点断点续跑，是“修复后回归验证”的原型。
  - **Hermes-agent**：离线取证工具做 postmortem，但运行时无 postmortem——说明“复盘”是离线工具，不是运行时机制。
- **自研点（Morde 新造，不要写成“抄某项目”）**：五步法本身；复盘模板八栏（含“之前说错的话”）；纠正先写回归用例；失败回流（失败 trace 自动进评测数据集、同一错误第二次出现沉淀为 lint / 检查 / skill）。这是 P2-3（postmortem + playbook 广播）。
- **Morde 落地注意**：不要为了“找个项目抄”而硬套。验收是 TC-11.1–11.10，重点是“之前说错的话”一栏必须如实写（Bug 3 的两次说错根因就是模板）。

---

## 复刻优先级建议（按 P0 / P1 / P2 顺序）

> 顺序与复刻设计 §7 一致。每一项写“先抄谁的哪一块”，不重复设计方案的模块清单。

### P0（先做，闭环前三项 + 审批卡）

| 顺序 | 先抄谁的哪一块 | 对应复刻设计 | 说明 |
|---|---|---|---|
| P0-1 | **Hermes-agent**：分类兜底（heuristic fallback）+ 错误重试梯子 + early result | P0-1 | 已合并进 main（PR #1，d7767d6），部署已验证 |
| P0-2 | **Hermes-agent**：bin.mjs 环境白名单 + 部署验证（image 重建 → digest 更新 → 重启 → probe） | P0-2 | 已合并进 main（PR #1），部署已验证 |
| P0-2b | **自研**：协调员容量 + 摘要转发缺失（PR #2） | P0-2b | 已合并进 main（PR #2，49216f52），部署已验证；E2E 真实对话验证等用户从 PC 发一条消息 |
| P0-3 | **微软 Agent Framework（Magentic）**：双账本思想——Task Ledger → brief（目标 / 范围 / 验收标准 / 验证方法 + 成本估算），Progress Ledger → 协调员每轮评估 | P0-3 Planner 最小版 | 只借双账本思想，不搬 plan sign-off 每次都问、不搬无预算上限 |
| P0-4 | **自研**：合并窗口（60s 可配 + 轻量判定） | P0-4 | 无开源对照；Muse 归并规则未知（U2），P0 实测定阈值 |
| P0-5 | **Hermes-agent**：审批卡面四行成本（本次 token / 累计 token / 成本 / 预算）+ **Codex CLI**：双轴模型（sandbox 轴 × approval 轴） | P0-5 审批卡体字段 | 卡面四行抄 hermes，7 步顺序不抄；双轴分离抄 Codex，`--yolo` 不进服务端 |
| P0-6 | **OpenHands**：事件溯源账本（EventStream）思想 | P0-6 事件链本体 | 只借 append-only 事件流 + 状态靠重放还原的思想，不搬 Controller–Runtime 全套 |
| P0-7 | **Claude Agent SDK**：权限五层求值（Hooks → deny → 权限模式 → allow → canUseTool，deny 永远赢）+ **LibreChat**：延迟工具（Deferred Tools，运行时按需发现） | P0-7 工具注册表 + 风险分级 + top-k | 64 硬上限改注册表 + 每轮检索 top-k；延迟工具是 top-k 的直接原型 |
| P0-8 | **OpenHands**：report 七要素（命令原文 / 退出码 / 耗时 / 工作目录 / 输出原文 / 结论） | P0-8 任务记录形态 | 定 Step Detail 的 schema 约定；工作项 id 前缀照抄 Muse 命名 |

### P1（验证闭环 + 沉淀）

| 顺序 | 先抄谁的哪一块 | 对应复刻设计 | 说明 |
|---|---|---|---|
| P1-1 | **OpenHands**：跑真检查验证（Edit-Run-Verify）+ 审查子 agent 原型；**Grok Bot 方法论**：独立验证者（换模型家族）+ 盲测 + 跨模型 Judge | P1-1 独立验证者 | 独立验证者无开源对照，是新造；方法论参照 Grok Bot，不写成“抄代码” |
| P1-2 | **LangGraph**：`interrupt_before` / `interrupt_after` 做 review gate（交付前暂停等人批） | P1-2 审批流 | 只借 gate 思想，不搬显式图 |
| P1-3 | **LibreChat**：Artifact 三类形态 + 聊天内渲染 + 主对话只出最终结果 | P1-3 交付类型与载体 | 不搬 ChatGPT 克隆 UI 形态 |
| P1-4 | **Hermes-agent**：两层阈值 + 先剪枝后总结 + 保护 head+tail + 失败冷却 + 轮首 preflight + turn 后 micro-compact | P1-4 上下文压缩 | 触发阈值数字不直接抄，按 Morde 实测定；全量历史重放不搬 |
| P1-5 | **Hermes-agent**：33 值错误分类 + jittered backoff + 自动恢复梯子 + terminal 五类必 abort | P1-5 错误分类与恢复 | 单机内存 tally 不搬，按租户持久化 |
| P1-6 | **Hermes-agent**：Turn 契约（turn_preflight_gate + micro_compact_after_turn） | P1-6 Turn 契约 | 轮首三段压缩 + turn 后微压缩 |
| P1-7 | **OpenClaw**：三层记忆文件 + 每小时整理 + Dreaming 夜间巩固；**Agno**：会话记忆与长期记忆分离 | P1-7 记忆同步 | 单机文件形态不搬，按租户隔离 |
| P1-8 | **可观测**：Phoenix 自托管（$0）+ OTel / OpenInference；**评测**：Inspect AI + 原生套件 | P1-8 可观测与评测 | 不是开源项目对照，是已定的工具选型（见 hermes 需求文档）；第一步先给路由分类加一个 span |

### P2（规模化 + 自动化沉淀）

| 顺序 | 先抄谁的哪一块 | 对应复刻设计 | 说明 |
|---|---|---|---|
| P2-1 | **Hermes-agent**：Skill 可写六要素（SKILL.md + 触发条件 + 验证） | P2-1 Skill 可写 | 只借六要素结构，不搬单机形态 |
| P2-2 | **Grok Bot 方法论**：信任阶梯（observe → correct → skill → routine）+ Routine 窄触发 + Test 真跑 | P2-2 信任阶梯与 routine | 方法论参照，无开源代码；纠正变检查自动路径是新造 |
| P2-3 | **自研**：五步法 + 复盘模板八栏 + 纠正先写回归用例；**OpenHands**：EventStream 回放复现（辅助） | P2-3 postmortem + playbook 广播 | 不要写成“抄某项目” |
| P2-4 | **OpenHands**：CodeAct（工具调用合并成代码执行） | P2-4 CodeAct 第二条工具路 | P2 候选，不是现在；先评估 `dsh-code-runtime` |
| P2-5 | **Agno**：AgentOS 的 RBAC + 团队隔离思想 | P2-5 多租户 | 只借隔离与权限模型，不搬 AgentOS 整套控制面 |

### 一句话优先级

> **P0 先抄 hermes 的分类兜底与审批卡面、Magentic 的双账本、Claude Agent SDK 的权限求值、OpenHands 的事件账本与 report 七要素；P1 抄 hermes 的压缩与错误分类、OpenClaw 的三层记忆、LangGraph 的 review gate；P2 抄 Grok Bot 方法论的信任阶梯与独立验证者、OpenHands 的 CodeAct；合并窗口与 Bug 五步法没有开源对照，必须自研。**

---

## 附录 A：证据等级与未核实清单

- **Muse 结论的证据等级**：本表每条“Muse 是这样”都带证据等级，等级与逆向工程文档一致。凡是合理推断 / 未知的 Muse 结论（U1 分类算法、U2 归并规则、U5 派发判断条件、U6 工具重试 / 超时 / 熔断、U7 上下文窗口与触发条件、U8 卡面字段细节、U9 错误类型与触发条件、U10 验证者选型、U13 记忆写入触发与去重、U18 失败时主对话措辞、U19 展示效果与提问触发阈值），其项目映射也标**待验证**，不是定论。
- **项目机制的核实状态**：Hermes-agent（固定提交 `0d49d6d9`）与 nanoMuse（自有代码）是此前已精读 / 自有代码；其余项目本次用公开搜索核实了关键机制（见候选项目速览）。凡是本次查不到出处的项目机制，本文未写——没有一条是凭印象编的版本号或机制细节。
- **Grok Bot 的定位**：方法论参照（土豆姐团队的做法），不是开源代码。本文在 §6、§8、P1-1、P2-2 引用它时都明确写“方法论参照”，不写成“抄 Grok Bot 的代码”。
- **文档分工**：本文保留开源方案的参考与限制；当前模块设计见《Morde 复刻 Muse 最终流程与开发设计》，实施和验收见《Morde 复刻 Muse 一体化实施与验收 TODO》；Muse 证据等级见 `muse-harness-reverse-engineering.md`。
