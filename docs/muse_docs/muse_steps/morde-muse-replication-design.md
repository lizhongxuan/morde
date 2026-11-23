# Morde 复刻 Muse Harness 设计方案

> 版本：复刻稿 v1.0 · 2026-11-23
> 主依据：`muse-harness-reverse-engineering.md`（逆向工程，700 行，证据分级）+ `muse-harness-replication-test-cases.md`（复刻验收用例 156 条）
> 阅读规则：每条“Muse 是这样”都带证据等级。**已证实**才当结论用；**合理推断**标“待验证”，设计里只能写成“先按推断做、评测验”；**未知**标“无对照可抄，须自研”；**hermes 补齐**只能写成“hermes 做法，可作补齐参考”，绝不写成“Muse 就是这样”。

## 0. 这份方案是什么、不是什么

- **是什么**：以逆向工程的 Muse harness 为蓝本的复刻方案。每一模块写清五件事：Muse 是怎么做的（带证据等级）、Morde 现在什么样（已实现 / 部分 / 缺失）、改成什么样、改哪些文件、按哪几条测试用例验收。
- **不是什么**：不是设计方案 v1.0 的重写。v1.0（`morde-design-proposal/morde-design-proposal.md`，2026-11-21）已经定好的六层结构、任务模型四层、brief 四件套、审批证据卡字段、事件本体 9 种、工具注册表与门禁四道、多 Agent 形态、DSH 插件接线、P0 → P1 → P2 顺序，本稿不再重复，只在相关模块写“见 v1.0 §X”。两处有出入的地方，以本稿的核实为准：(a) 子 agent 深度上限：v1.0 §9 写“深度限一层，与 Muse 实测行为一致”，逆向工程已推翻，全量数据证明 Muse 嵌套到 2 层（230 次派生里 depth=2 有 71 次，占 31%），3 层是否存在未知，Morde 限 1 只是 Morde 自己的设计选择，不是 Muse 事实；(b) 事件链：v1.0 写“自由文本 step”，核实后写侧已有 11 个固定 `task.*` 记录类型白名单，缺的是 9 种语义本体，不是“无法无天”。
- **与目标文档的关系**：目标文档（`morde-target-design/morde-target-design.md`）定终态（一句话：Muse 的对话体验 + Grok Bot 的多 Agent 机器 + Hermes 的成本账 + OpenClaw 式 Markdown 记忆，装在能复制给另一个人的云电脑上，服务端撑上万人并发）。本稿是其中“Muse 对话体验”这一块的复刻施工图。
- **与 hermes 需求文档的关系**：hermes 需求文档（`morde-hermes-copy-requirements.md`，2026-11-21，用户已定完全照搬 hermes 对话处理，19 步搬 13 步）是另一条线。本稿第 8 节单列两者重叠与冲突的取舍，不写成两套。

## 1. 目标与非目标

**一句话目标**：把 Muse 的 harness（1 个 root agent + 树形 subagent + 有序事件账本 + 卡片制审批 + Markdown 与数据库双轨记忆）复刻成 Morde 的 Harness + DSH + Broker，让用户在 Morde 里得到与 Muse 同样的对话体验：主对话只见最终结果，任务可拆可派可回放，失败留痕不丢消息。

**复刻什么**：五条原则的全部机制（第 2 节）；收件、路由拆分、并发派发、工具执行、审批、验证、交付、记忆沉淀、失败处理、上下文管理的行为与验收口径（第 4 节）；Bug 发现处理五步法（第 5 节）。

**不复刻什么**：

- Muse 的服务端形态：Muse 是单用户单机，每 Muse 属于一个用户，无多用户支持。Morde 是多租户服务端，目标上万人并发，租户隔离、配额、限额必须另起一层（U16，未知，须自研）。
- Muse 的模型分工与计价：Wonder vs Spark 的计划分工只知其然，模型选择逻辑未知（U3）；成本记账在 Muse 文档里不存在（U9）。Morde 的成本账按 hermes 的做法抄（第 8 节），不冒充 Muse 证据。
- Muse 的标题随意写法：同一动作两次标题写法不同（“对用户消息点赞 👍”vs“标记用户消息：👍🏻”）。Morde 用“动词 + 对象”固定规则生成标题。
- Muse 的内部调度表：`runtime.work_items` 与 `agent_todo_snapshots` 是内部调度/计划表，不是 UI 工作项。UI 的唯一事实来源是 activity actions，Morde 同理：todo 清单与运行时内部调度表不是 UI 的工作项。

## 2. 五条原则在 Morde 的落地（2026-11-13 锁定，不可改）

| 原则 | Muse 实证（证据等级） | Morde 落法 | 验收 |
|---|---|---|---|
| ① 主对话只出最终结果，过程噪音收进任务详情 | 已证实：assistant 回复中位 142–160 字符；事件 75% internal；54 条工作项全在任务详情里，主对话只有“图正在画”和最后交付 | 路由 `reply` 直接回文本；任务过程进 `task_events` 与 `task.Step`；对话只出文字（设计方案 §3.7 已定）；P0-8 做一次 UI 审计，清掉对话流里残留的内联任务卡/状态条/任务入口行 | TC-7.1–7.12；附录 A |
| ② 并发靠后台 agent 匹配，主 agent 只做简单对话，第一个任务不挡第二个 | 已证实：主 agent 派发两个子 agent 后自己继续做 git 检查，时间戳重叠；单日最多 63 个任务 | 任务级并发已有（`MAX_CONCURRENT_TASKS = 16`）；缺的是“匹配/派发决策”，即 Planner（P0-3）；派发判断条件（复杂度阈值）Muse 侧未知，须自研 | TC-3.1–3.13 |
| ③ 任务是记录单位，同话题多轮归成一个任务 | 只证实一半：一条消息一个 activity thread；31% 的任务链 ≥2 条消息（829 链接 / 352 任务），但归并规则未知（U2） | 一消息一任务的基础已有；同话题合并只有“聚合”没有“合并窗口”，P0-4 补：60s 可配窗口 + 轻量同话题判定（关键词重叠 + 分类器 `join`/`steer` 信号） | TC-2.1–2.12；MRG-01–03 |
| ④ 每个任务记录完整 agent 过程 | 已证实：16,182 条 actions，成功 98.7%、失败 1.3%；子 agent 工作项是主 agent 的 4.6 倍 | `task.Step` 行已有载体；缺的是 report 固定结构，P0-8 定七要素（第 6 节）写进 `DetailState` 的 schema 约定 | TC-4.1–4.14 |
| ⑤ 每个工具调用可钻取到代码/命令/结果 | 已证实：report 里有完整命令、退出码、耗时毫秒、工作目录、输出原文 | Broker `Execute`/`Reconcile` + operation ledger + checkpoint 已有追踪能力；“点开看”的 UI 侧只做到 HTML 稿，P0-8 补落地核对；大工具结果只留预览+编号取回尚未实现 | TC-4.1–4.14 |

补充：原则③后半句在 Muse 文档与实测中均无支撑，是本稿最大的自研点。合并窗口时长与同话题判定阈值不在本稿定死，P0 实测再调。

## 3. 总体架构

Muse 的形态是 1 个 root agent + 树形 subagent（最深 2 层）+ 有序事件账本 + 卡片制审批 + Markdown 与数据库双轨记忆。Morde 的落点沿用设计方案 v1.0 的六层结构（L1 入口 → L2 Harness 决策层 → L3 内核 → L4 能力模块 → L5 DSH 执行层 → L6 云电脑），只补三处与复刻直接相关的定稿：

```
用户消息（PC 主端 / iOS 看问批）
  → Portal 网关 dispatch（持久收件 + client_message_id 幂等）
  → Harness：合并窗口 + 路由分类（失败走启发式兜底）→ Planner 出 brief
  → DSH 执行层：1 主 Agent + 树形子 Agent（Morde 暂定深度限 1，见 4.3）
       → Tool Broker：注册表 + top-k 检索 + 门禁四道 + 并行 ≤10 按序回填
  → 独立验证者（换模型家族，verdict 锁版本）→ 交付进 Workspace
  → 事件账本：每步一条事件（9 种本体），成本实时进账本
  → 记忆沉淀：Markdown 个人记忆 + PostgreSQL claims 双轨（会话/个人/场景三层分开存）
```

三条硬边界（目标文档已定，本稿重申）：DSH 是唯一执行层，不换不重写；Planner 在 Harness，不在 DSH；多租户服务端形态，任务、事件、账本落库，worker 租约认领。

**一端逻辑，两端显示**（2026-11-21 用户定）：PC 和 iOS 共用同一套 agent 逻辑，只差显示。全部逻辑在服务端（Harness、内核、DSH、Broker、记忆、Skill、钩子），两端调同一套 API、收同一套事件流。iOS 端 `clients/nanoMuse/.../Agent/` 的本机循环不再是第二条 runtime，它的机制（并行调度上限 10 按序回填、ToolLoopDetector、Preflight、RequestBudget）搬到服务端 Broker 与护栏里实现。

**前置条件已满足**：设计方案 v1.0 §2 写“唯一云电脑主机仍是 failed，需用户在页面点创建电脑”，该状态已过时。云电脑主机 hdqokxzv（标准 2 核 4G）已于 2026-11-19 建好，state=running，沙箱 healthy，是 `user_preferences.default_host_id`。P0 端到端验收不再被云电脑卡住。

## 4. 逐模块设计

每模块五项：Muse 怎么做（证据等级）→ Morde 现状 → 改成什么样 → 改哪些文件 → 验收（TC 编号出自 `muse-harness-replication-test-cases.md`）。

### 4.1 收件入口

- **Muse 怎么做（已证实）**：入口面很宽但都是一对一：web / iOS / Android / Mac 应用 + Chrome 扩展侧边栏 + WhatsApp（唯一有指南的侧边 provider；Discord、SMS 无此连接）+ 语音（实时对话、备忘录转写、多声部生成，无环境监听）。每会话一个 chat UUID，transport 不产生第二份 transcript。任务行是 `activity_monitor_threads` 的一行（name、subtitle、status、finish_status、created_at/finished_at），442 个任务全部 finished；副标题随进展改写。收件的幂等、去重、定序机制未知。
- **Morde 现状（已有，且比 hermes 更严）**：`AcceptConversationMessage` + `client_message_id` 幂等（同 ID 不同内容拒绝）+ 服务端 `sequence` 定序。这是 Morde 自研，不是从 Muse 抄的（TC-1.7）。入口是 PC 主端 + iOS 看问批。
- **改成什么样**：不新增机制。补一条端到端验收：同一账号在 PC 与 iOS 接续同一会话，历史、进度、结果一致，不重复建任务。
- **改哪些文件**：无新增代码。验收走测试用例。
- **验收**：TC-1.1–1.8（多端同一会话、WhatsApp 唯一侧边通道、语音转写、电话限制、任务行五字段、副标题改写、幂等收件、配对设备能力以 `device.describe` 为准）。

### 4.2 意图路由与任务拆分（含合并窗口）

- **Muse 怎么做（已证实 + 未知）**：一条消息不严格等于一个任务，31% 的任务链 ≥2 条消息，但归并规则未知（U2，最大缺口）。Skill 路由有明确机制（`skill_search` 按任务找 skill，SKILL.md 写明触发条件，无 skill 走 `custom-connector-setup`）。Connector 路由边界明确（只有文档列出的命令才存在）。请求分类字段存在（`request_origin`、`transcript_surface`、`root_work_class`），分类算法未知（U1）。Coordinator + worker 是文档级推荐模式（大文档审阅：一个 coordinator + 若干 extraction worker + 一个 checking worker；coordinator 定分工、合并发现、保留最终判断权）。Wide-research 模式：先去重归一化输入，再派 manager，输出合同固定为 total / success_count / failure_count / results / failures / notes。
- **Morde 现状**：路由分类 7 步链已证实（快照最近 8 条 + 最近 4 条已发布任务结果 + 候选任务 + prior_requests ≤100 + 聚合 ≤8 → 128 KiB 单消息门 → 摘要尝试 → 密封摘要装入 → fit 裁剪 → 分类模型调用 → 聚合输入合并 32 KiB）。分类失败兜底已合入（P0-1，见第 7 节）。“短时聚合”机制已有（`join` 动作、`aggregation_waiting`、`MaxConversationRouteAggregateMessages = 8`、合并预算 32 KiB、超预算拆补充单独路由），但那是“等一等、合起来一次分类”，不是“同话题多轮归进同一任务”。Planner 缺失：Go 代码里 `planner` 零命中，只有执行 Agent 自报的 `morde_task_plan_update` + `task.Plan`。
- **改成什么样**：P0-3 Planner 最小方案 + brief 四件套（目标 / 范围 / 验收标准 / 验证方法 + 成本估算；无验收标准的 brief 不派发，直接 Clarify 问用户要验收标准；执行 Agent 自报的 plan 降级为参考）。P0-4 合并窗口最小版：在聚合机制上加时间窗口常量（先定 60s 可配，P0 实测再调）+ 同话题判断（窗口内新消息先与进行中任务的 brief 目标做轻量判定：关键词重叠 + 分类器 `join`/`steer` 信号，命中追加到同一任务，不命中开新任务；窗口外的消息不再合并）。
- **改哪些文件**：新建 `server/internal/app/conversation_planner.go`；改 `server/internal/app/conversation_route_decision.go`（路由 `new` 后调 Planner；合并窗口判定）；改 `server/internal/store/postgres/conversation_route_actions.go`；改 `server/internal/worker/conversation_route_worker.go`；改 `server/internal/worker/cloud_dsh_coordinator.go`（按 brief 派发，拒绝无 brief 派发）；`server/internal/domain/task/types.go`（加 `Brief` 结构）；测试新建 `conversation_planner_test.go`，窗口内 / 窗口外 / 同话题 / 不同话题各一例。
- **验收**：TC-2.1–2.12；窗口内同话题补充只产生一个任务、一次最终答案；窗口外补充开新任务；不同话题不合并；聚合等待超 32 KiB 仍拆补充单独路由（2e86e33e 回归不破）。待验证：合并窗口时长与同话题判定阈值（U2 未知，须自研，P0 实测定数）。

### 4.3 并发派发与协调员监视循环

- **Muse 怎么做（已证实）**：主 agent 只做简单对话，复杂/并发工作派后台 agent，时间戳重叠证明不是排队。子 agent 嵌套到 2 层（230 次派生：depth=1 有 159 次、depth=2 有 71 次占 31%；depth=2 工作项 6,013 条；3 层是否存在未知）。子 agent 类型有分工：通用 54%（`agent_type` 为空 124 次）、`artifact_builder` 27 次、`spaces_builder_lite_edit` 24 次、`spaces_builder_lite` 22 次、`browser_task` 12 次、`deep_research` 11 次。协调员每事件决策一次（20,787 条：`nothing_to_do` 61%、`worker_or_coordinator_completed` 38%、`notify_main_agent` 仅 1.2%、`inference_error` 3 条）。派生本身也是一条工作项（`ga-root-subagent-dispatch-…`）。官方推荐“一个 coordinator per objective”，明确反对从 root 直接扇出多个平级 worker。后台结果以 hidden handoff 交回主 agent，由主 agent 决定用户看到什么；worker 不能要求主 agent 做更多工作。Browser 并发：每个 browser task 一个窗口，一次只跑一个 checkout。
- **Morde 现状（部分有）**：任务级并发有（`MAX_CONCURRENT_TASKS = 16`）；DSH 持久子 Session 有；`dsh-subagent` / AgentTeam 插件未注册；深度限 1 未核对；handoff 三件套与共享工作图缺失；本地回归通过，真实入口未验。
- **改成什么样**：Planner 做派发决策（P0-3）；协调员监视循环照 Muse 的比例做：每个子 agent 事件触发一次决策，默认 `nothing_to_do`，只有完成或需通知主 agent 才上报；handoff 三件套（已完成什么、证据在哪、还差什么）；一个 coordinator per objective，不从 root 直接扇出多个平级 worker；后台结果以 hidden handoff 交回，主 agent 决定用户看到什么。并行调度用 nanoMuse 方案（上限 10、按序回填），不用 hermes 的 8 workers（hermes 需求文档 §2.6 已定）。深度上限：Morde 暂定限 1（设计选择，见第 0 节 (a)；是否放开到 2 层待用户拍板，见第 9 节）。
- **改哪些文件**：`server/internal/worker/cloud_dsh_coordinator.go`（监视循环 + handoff）；Planner 文件见 4.2；`server/internal/app/task_execution_helpers.go`（派发记 step）；测试补“派发后主 agent 继续做自己的事时间戳重叠”与“子 agent 完成只通知一次”两例。
- **验收**：TC-3.1–3.13；第一个任务不挡第二个（主 agent 在子 agent 未完成时能接新消息）；协调员决策里 `notify_main_agent` 占比接近 1% 量级，不是对每条子 agent 事件都打扰主 agent。待验证：主 agent 派发判断条件（复杂度阈值、耗时预估）无直接证据（U5）；最大并发数、子 agent 配额、排队策略须自定（wide-research 只给“先跑一项再放量”的启发式）。

### 4.4 工具执行工作项

- **Muse 怎么做（已证实）**：工作项 = 一次工具调用。点开 report 结构固定：**在哪执行 + 完整命令/参数原文 + 退出码 + 耗时（毫秒）+ 工作目录 + 输出原文（长输出截断并注明截了多少行）+ 一句人话结论**。进行中的调用先以占位行出现（`running`），跑完再换正式标题。失败的工作项留在清单里，不删不改写（failed 214 条占 1.3%，任务没有因此停下）。进行中任务可取消（`cancelled` 96 项是常规终态）。工具调用全记录：`tool_calls` + `tool_outputs` + `events`（`event_seq` 单调的有序账本）。注意：只读角色查 `tool_calls` 为 0 行，两次皆 0，原因未知（权限过滤还是另有写入路径），用户能看到的工具记录唯一事实来源是 actions 的 report。恢复四步：读持久期望态 → 恢复前置条件 → 逐组件对账 → 从检查点续跑。
- **Morde 现状（部分有）**：`task.Step` 行有 ID / TaskID / Sequence / Attempt / Kind / Status / Title / Summary / FailureReason / AgentSessionID / ToolName / ToolCallKey / DetailState；Broker `Execute`/`Reconcile` + operation ledger + checkpoint 已有追踪能力。但 report 七要素在 Step 顶层字段里没有对应项（DetailState 为密封载荷，本次未逐项核对）。工作项 id 带身份，前缀即归属（`root-tool-…` / `root-subagent-dispatch-…` / `subagent-start-…` / `child-tool-…` / `subagent-finish-…`），可直接照抄。
- **改成什么样**：P0-8 定 Step Detail 的 report 固定结构（七要素，照抄本节首段），写入 `task.Step.DetailState` 的 schema 约定（先约定、后补写）；工作项 id 前缀照抄 Muse 的命名；失败 step 保留不删不改写；取消是常规终态。
- **改哪些文件**：新建 `server/internal/app/task_step_report.go`；改 `server/internal/app/cloud_dsh_tool_broker.go`（写 step 时按结构落 Detail）；测试补“任一工具调用点开详情七要素齐全”与“失败 step 保留”两例。
- **验收**：TC-4.1–4.14；任一工具调用点开详情，七要素齐全；失败 step 保留不删不改写。待验证：工具调用的重试策略、超时阈值、熔断、预算上限无 Muse 证据（U6），须自研（见 4.9）。

### 4.5 审批

- **Muse 怎么做（已证实）**：审批是卡片制 + 作用域制，审批卡对 agent 不可见，决定存在独立的 Sentinel approval store，agent 的权限工具只读。普通任务免批，敏感 / 改状态动作要批（登录、购买、发帖等）。购买每次都要新审批，永无 lasting 默认；钱包连接 / 选卡 / 看购物车本身不等于授权花钱。发邮件 / 发消息每次批准，除非有 scoped allow（发给某一人可记“以后都允许”；欧盟用户没有任何 send allowance，每次只有 Allow once / Deny）。发帖每次展示精确文本 + 媒体 + 账户 + 受众 + 发布时间并获批，改动任一要素要重新批准。Artifact 每次发布与每次更新已发布链接都要新的一次性批准。文件上传每次都要新批准。凭证绝不经聊天（密码只进 Secure Vault；一次性码只走 `[credential:<uuid>]` 保护引用）。后台动作无用户在场仍要停下来等批准，除非该 scheduled task 已有 standing Allow（欧盟：每次都停）。T05 审批卡验收至今未完成（2026-11-09 配对成功后只差点一次审批卡，用户说“不开发了”后搁置，不要再主动催）。
- **Morde 现状（缺失本体，部分有披露层）**：`PendingRequest` 除 Object / Content / Impact 外还有 Title / Kind / ToolName / CallID / RequestHash / Version / ExpiresAt（有“动作身份”，无证据、成本、触发规则、拒绝后果）。API 层已有披露对象（Risk / ThisCost / CumulativeCost / Alternatives），但服务端费用常为 unknown、无通用金额限额字段，即披露层搭了架子、数据常空。
- **改成什么样**：P0-5 审批卡证据化（本体）。`PendingRequest` 加字段：证据指针（`EvidenceRefs`）、本次成本（`ThisCostMicros` + 未知态）、累计成本（`CumulativeCostMicros` + 未知态）、触发规则（`TriggerRule`）、拒绝后果（`RejectConsequence`）。卡面文案：待批动作、证据、本次成本、累计成本、触发规则、拒绝后果同一张卡。API 披露层把已有的字段从“常 unknown”改成“有数给数、无数明确标 unknown 且不伪造为 0”。`always` 档位默认值：仅写操作可记，不可逆永远逐次批（等用户拍板再放宽）。卡面格式与成本四行抄 hermes，7 步顺序与 bypass 不抄（规格定为 C 级，见第 8 节）。
- **改哪些文件**：`server/internal/domain/task/types.go`（PendingRequest 加字段）；`server/internal/store/postgres/task_approvals.go` + 新 migration（列 + 回填 unknown）；`server/internal/app/task_approvals.go`（写入 / 校验）；`server/internal/api/tasks.go`（披露层接真实成本）；PC `TaskDetail.tsx` + iOS 审批视图（只改卡面字段，不改流程）。测试补 APR-01–03。
- **验收**：TC-5.1–5.17；审批卡面六项齐全；成本数字与 `model_call_ledger` 对得上（有数时）；无数时明确显示 unknown，不显示 0；不可逆动作每次都出卡（回归）。待验证：审批卡的完整字段、按钮文案、超时行为、默认选项，Muse 侧未知（U8），须自研。

### 4.6 验证

- **Muse 怎么做（已证实）**：写后读回（read-back）是统一验证模式（任务、日历事件、GitHub 变更、家居命令执行后都要求读回确认）。独立检查 worker：文档审阅可配一个 checking worker，拿 draft findings + 抽取文件，标出无支撑断言、歧义、冲突段落、对不上的数字；最终判断权留给 coordinator。证据绑定：每条引用 / 发现必须绑定文件 + 页 / 节 / 幻灯片 / 单元格；PDF 优先看渲染页而非抽取文本。覆盖率报告：wide-research 最终输出必须报 verified coverage（`success_count/total`）并区分 pending / failed / stopped / never-started。支付对账：订单“已提交未确认”时，约 10 分钟后自动排一次 runonce 复查，再 30 分钟后最后一次；仍无确认则停止、不重下单。标识符必须逐字抄自对话 / 工具结果 / 已读文件，禁止凭记忆写；日期先用 `date -d` 核对星期。交付不等于成功：run 成功只表示工作完成，不表示用户收到。**无内置“独立验证者换模型家族复核”机制**：文档里没有与 Grok Bot 式独立验证者对应的机制，最接近的是 checking worker（同一系统内）。
- **Morde 现状（部分有，独立性未证实）**：`TaskValidationReviewWorker` 已有 DSH 模型复核 worker + fence，但 Artifact 内容复核未接通、结论常为 `pending`；“验证模型家族必须与执行模型不同”无强制代码；verdict 锁版本在 H05 进度里已有；Judge 模型 key 未配。
- **改成什么样**：P1-1 独立验证者最小可用。在现有 worker 上加两条硬规则：(a) 验证模型家族必须与执行模型不同家族（加校验，相同家族拒绝复核并记 `pending`）；(b) verdict 锁版本（已有 revision / event_seq / SHA-256 栅栏，补“版本一变 verdict 作废重验”的回归）。Artifact 内容复核仍缺，P1 先做到“文本 / 证据类复核独立”，内容复核放 P2。独立验证者挂在 hermes stop-gate 的钩子位上（只搬钩子位与顺序，验证者本身是新造，hermes 没有独立验证者 agent）。
- **改哪些文件**：`server/internal/worker/task_validation_review_worker.go`（家族校验）；`server/internal/app/cloud_dsh_task_validation*.go`（模型解析）；测试补家族相同拒绝 + 版本变 verdict 作废两例。
- **验收**：TC-6.1–6.9；交付物都有非作者 verdict；验证模型与执行模型同家族时复核拒绝、不放行；版本变了旧 verdict 不复用；FAIL + 理由返工。待验证：模型选择 / Judge 换模型家族，Muse 侧未知（U3）；Judge 模型 key 待用户定（见第 9 节），未配前用已有非 DeepSeek 模型顶着并注明可信度打折。

### 4.7 交付

- **Muse 怎么做（已证实）**：主对话只出最终结果，数字上成立（assistant 回复中位 142–160 字符；事件 75% internal；内部消息是公开 assistant 消息的 5.5 倍；一次 morde 任务 54 条工作项全在任务详情里，主对话只有“图正在画”和最后交付）。交付物要点贴消息里，不只给链接（2026-11-21 用户说“看不到方案”后形成规则：递文件要点必须贴在消息里）。大文件交付默认预拆分或附校验（152MB muse_db 导出走任务交付）。Artifact 三类：文档、页面、交互应用（自带数据库、常驻本机、可存数据）；发布仅静态 artifact 可公开链接，每次发布与更新都要新审批。通知：审批即时 push；scheduled / proactive 消息仅在 app 未打开时 push；proactive 上限每次交付至多 8 条，每天至多 2 次普通交付、至多 3 次含时效。提问只在阻塞或需拍板时出现（回复以 `?`/`？`结尾 6.8%，`waiting_for_user` 6.8%，两个独立口径一致）；提问的载体是选项卡，不是开放式追问；含糊指令先问清（用户说“开始吧”曾被误读为“开始每日人格测试”，之后纠正为需要先确认七个场景中的哪一个）。
- **Morde 现状（部分有）**：路由 `reply` 动作直接回文本；交付已有等价机制（完成事务行锁重检 + 单次发布回归通过；`ResultReview` 与 `Task.status` 分开展示）；对话只出文字（设计方案 §3.7 已定）。
- **改成什么样**：以 §3.7 为准做一次 UI / 文案审计（P0-8）：PC / iOS 对话流里残留的内联任务卡 / 状态条 / 任务入口行清掉（只删残留，不加功能）；递文件要点贴消息里（已成规则，执行即可）；提问只在阻塞或需拍板时，载体用选项卡，点选后行为立即生效；不可逆的发送 / 下单 / 付款前先确认具体方案。
- **改哪些文件**：PC / iOS 对话流审计（只删残留，不加功能）；20 项截图验收里相关项通过（一次没跑过，见第 9 节）。
- **验收**：TC-7.1–7.12；主对话无内联任务卡 / 状态条；提问率接近 6.8% 量级（不是每条都问，也不是从不问）。待验证：提问的触发阈值（什么算“阻塞”、什么算“需拍板”）无 Muse 证据（U19）；Morde 设计稿的“最多问 3 个关键问题”是 Morde 的设计，不是 Muse 的证据。

### 4.8 记忆沉淀与纠正沉淀

- **Muse 怎么做（已证实）**：记忆是“Markdown 文件 + PostgreSQL claims / embeddings”双轨（`MEMORY.md` 精选长期记忆 + 日期笔记 + 每人 / 每群一页 + 购物 / 旅行 PROFILE；索引层 `memory.claims` 带 claim、支撑引用、重要性）。后台整理节律：记忆整理每小时、人物关系每小时、技能评审每日（可产生新 skill，`skill.yaml` 标 learned；无帮助的退役）、Dreaming 每晚、安静时刻 pass 每天至多几次。沉淀载体带纠正关系（日记条目带类型与权重标记 `[fact|high]`、`[correction|high]`、`[event|medium]`、原文 quote、`supersedes:` 字段）。纠正留痕的规范动作是“认错 + 给实查数字 + 改记录”，实例至少四起（生益价格纠正、“64a39aa 只改壳”纠正、日报名纠正、Bug 3 误诊纠正）。五条原则本身就是一次纠正的沉淀。遗忘有正式流程（`forget.plan` → 展示范围与不可逆性 → 一次直接确认 → `forget.confirm`；沉默 / 含糊 / 部分批准 / 改范围 = 未确认）。推理不可读（`muse.db` 永远读不到模型的私有推理）。👍 是“收到了、正在做”的回执，只给大活儿。
- **Morde 现状（部分有）**：会话记忆有；个人记忆有“近期对话片段摘要”；场景记忆随场景包走是文件形态，未核对代码落地；纠正变检查缺失（TODO H07 未开始）；postmortem 无；记忆 `sync_all()` 后台串行未做。
- **改成什么样**：三层记忆分开存（会话记忆本任务上下文 → 个人记忆习惯 / 人物 / 长期事实，Markdown 可读可改 → 场景记忆随场景包走，复制给别人时只带这一层；个人层和场景层物理分开）。P1-7 纠正变检查最小形态：一条被纠正过的错误先进回归用例（人工先写用例，自动沉淀放 P2）。P2-3 postmortem + playbook 广播（复盘模板见第 5 节）。记忆同步后台串行，不阻塞对话。hermes 的 curator 周期（7 天 / 2h / 14 天 / 30 天，永不删除）照抄进 P2-1；但“纠正变检查自动路径”hermes 也没有（H-18），是新造，不要写成“抄 hermes”。
- **改哪些文件**：`server/internal/evaluation/testdata/`（回归用例追加）；`server/internal/app/cloud_dsh_memory*.go`（新建或扩展记忆同步，后台串行）；新建 `server/internal/app/cloud_dsh_postmortem.go`（P2-3）。
- **验收**：TC-8.1–8.12；被纠正过的错误在回归用例里不再出现；记忆同步不阻塞对话。待验证：记忆写入的触发与去重细节未知（U13），只知每小时整理、新 claim 覆盖旧 claim。

### 4.9 失败与限流

- **Muse 怎么做（已证实）**：失败留在任务详情里，主对话不受污染（failed actions 214 条全部保留；任务 finish_status `failed` 仅 14/442，占 3.2%）。失败也照实记、不改写。限流是“每 connector 每分钟配额 + `connector_rate_limited` + `retry_after_seconds`”；`terminal_for_attempt: true` 时当次 connector 工作立即终止，禁 sleep / 重试 / 委派 / 排替代任务。后台只有 cron / hook / systemd 三种机制，所谓 watch 与事件触发实际都是轮询，两次检查之间发生又反转的变化会漏掉，定时只约不准。任务运行卡在 durable 阻塞（connector 需重连、只有用户能做的动作、浏览器 bot 检查过不去、配对设备离线、浏览器正忙）时任务自己暂停，下一轮再试。Muse 遇到工具失败 / 超时 / 上下文超限时主对话具体怎么措辞、有无自动重试后降级，无直接证据（U18）。
- **Morde 现状（部分有）**：路由失败原来是直接失败、无降级（近 2 天 53 次路由决定里 11 次失败，占 21%；最多的是 `routing_context_unavailable` 9 次），已由 P0-1 兜底链改掉（见第 7 节）。429 限流重试已在 HEAD（最多 3 次）。33 值全枚举没有，`context_length_exceeded` 与通用失败在 worker 里基本同路；`jittered_backoff` 未见移植。自动恢复梯子缺失（路由层只有 429 恢复；DSH Task 层无此梯子；Task 空响应当前不自动重试）。空响应预算与重复守卫缺失。
- **改成什么样**：P1 错误分类表（`context_overflow` 单独一类，只触发压缩，不走通用重试 / 换模型）+ 自动恢复梯子（5 周期 15 / 30 / 60 / 60 / 60s，仅 transient 且未输出文本时）+ 空响应预算 3 次（P2，有真实重复反例再翻译，先旁路记录）。失败 trace 自动进评测数据集（P1-2 / P1-3）。失败首先是任务详情里的记录，其次才是需要用户介入的状态；主对话不受污染。
- **改哪些文件**：`server/internal/worker/conversation_route_worker.go`（错误分类 + 恢复梯子）；`server/internal/evaluation/`（失败回流接任务创建）；`server/internal/app/` 错误分类新文件（P1）。
- **验收**：TC-9.1–9.8；同一错误每次都一样处理，不随机报错；非 transient 错误不重试；已输出文本后不重跑。待验证：Muse 失败时主对话的具体措辞与降级策略未知（U18），不要拿 `tool_calls` 0 行当“没有工具记录”的结论（0 行可能是行级过滤）。

### 4.10 上下文管理

- **Muse 怎么做（未知）**：Muse 侧每次带多少历史未知，只知 `agent.compactions` 与 `context_items` 表存在，不知窗口 token 数、触发条件、摘要模型、保留策略（U7）。本节的“Muse 怎么做”只有一句话：不知道。以下写的是 Morde 自己的 7 步链（已证实：代码 + 线上数据）与 hermes 的补齐机制，不要把后者当成 Muse 的行为。
- **Morde 现状（7 步链，已证实）**：① 快照加载（最近 8 条历史，每条 >4096 B 时 SQL 层截成头 1023 B + `…` + 尾 1023 B；另加最近 4 条已发布任务结果、候选任务、prior_requests ≤100、聚合候选 ≤8）；② 单消息门（`MaxConversationAgentInputBytes = 128 KiB`，超了 `message_too_long` 明确失败，不降级）；③ 摘要尝试（未覆盖源字节 ≥72 KiB 或 token 压力 ≥ 预算 3/4 时触发，新摘要 ≤16 KiB）；④ 密封摘要装入（旧摘要文本 + 去掉已被摘要覆盖的历史行；seq>9 且无摘要时 `HistoryIncomplete=true`）；⑤ fit（2e86e33e 后的顺序：先裁候选任务 → 历史逐条缩成 2048 B 预览 → 历史逐条清空只留 ID / 序号 / 任务引用 → 最后才丢摘要 → 仍装不下才 `message_too_long`；token 估算 = DSH TokenMeter，4 个 UTF-16 字符约 1 token，CJK 加 25%；留 headroom `min(4096, max(256, 窗口/8))` + 800 运行时预留 + MaxOutputTokens；窗口 = 模型配置值，deepseek-flash 1,048,576，未知则 4096 保底）；⑥ 分类模型调用（输出 ≤4096 B 严格 JSON，无效输出修一次，修完仍无效走 bug 1 兜底）；⑦ 聚合输入合并（预算 32 KiB，超的补充拆下来单独路由，并在合并文本里注明“另有 N 条补充”）。直接回答也是同一链：分类模型那一次调用就写出回复，没有第二次调用。任务执行时初始输入只有任务简报（objective ≤32 KiB），历史由 agent 需要时自己查（3 个有界工具，`morde_current_conversation_history_search` 有游标分页，limit ≤10，返回 `partial` + `next_cursor`，是全代码库唯一的“分段读”先例）。
- **Morde 现状（缺口）**：摘要压缩代码有（`CompactConversationSummary` + 413 行维护代码），但走 DSH coordinator 派生，Bug 3 没修好之前永远失败，线上三张摘要表全是 0 行；失败只记一条 warn 日志，无冷却、无 breaker、无 fallback 摘要。任务循环内无压缩 / 预算管理：最多 16 轮 × 每轮 16 个工具调用，tool call / output 在循环内累积，每次模型调用都受 128 KiB 总预算，超了任务就失败。工作区读文件工具一次读整个文件，>48 KiB 拒绝，没有 offset / limit 参数。
- **改成什么样**：P1-4 上下文加固：(a) 摘要失败冷却 600s 持久化 + terminal 五类必 abort + 3 次过载后确定性 fallback 摘要；(b) 溢出后“压完重试一次”（provider 报 `context_length_exceeded` 时先触发一次摘要压缩再重试分类，限一次）；(c) Tail 保护进快照（SQL 加一路引用锚点查询）；(d) 错误分类表（`context_overflow` 单独一类）。P1-6(e) 确定性剪枝进任务循环 + 轮首 preflight + turn 后 micro-compact（接已有的 `hermes-compaction-window.mjs`，阈值公式已移植，只差调用点）。单条消息超 128 KiB 走分段读取三阶段（P0 优雅降级：入库分片 + 分类用头预览 + 元数据 + 意图不明 Clarify；P1 分页读 + 顺序 map-reduce，新工具 `morde_long_message_read_chunk`；P2 并行分片 + 文件范围读）。hermes 的补齐机制（两层阈值触发、先剪枝后总结、保护 head + tail、一次辅助 LLM 总结、失败冷却 + terminal 清单）照抄机制，不抄全量历史重放与 in-place session 压缩（Morde 摘要是版本化密封文档，语义不同）。
- **改哪些文件**：`server/internal/worker/conversation_summary_maintenance.go`（冷却 + terminal 清单 + fallback）；`server/internal/worker/conversation_route_worker.go`（溢出重试）；`server/internal/store/postgres/conversation_route_actions.go`（Tail 保护查询 + chunk 表 migration）；`server/internal/app/conversation_route_decision.go`（错误分类）；`server/internal/app/cloud_dsh_history_tools.go`（`morde_long_message_read_chunk`）；`runtime/morde-cloud-runtime/src/hermes-compaction-window.mjs`（接调用点）+ `src/engine.mjs`。
- **验收**：TC-10.1–10.20；摘要表从 0 涨起（Bug 3 修好后验证，见第 7 节）；摘要失败不再每条消息重试一次；`context_length_exceeded` 先压缩重试一次再失败；长会话问 20 条前细节走历史搜索而不是瞎答；P1 完成时 1 MiB 纯文本单条消息全程不失败（TC-10.20：3 个分布在不同页的锚点细节答对并引用页码，每页 sha256 校验通过）。待验证：Muse 窗口 token 数、触发条件、摘要模型、保留策略未知（U7），Morde 的数字须按自己的模型与实测定，不直接把 hermes 的 85% / 50% 配置作为生产默认。

## 5. Bug 发现与处理流程在 Morde 的落地

Muse 自身没有可查的 bug 处理流程文档。本节的方法提炼自 Morde 服务端 2026-11-22/23 的两次真实调试，每一步都有日志 / DB / 实测支撑，可直接作 Morde harness 的调试规范。

### 5.1 五步法

| 步骤 | 做法 | 证据形式 |
|---|---|---|
| 发现 | 先拿数字定性：失败率、失败分类 Top N、用户原话贴出来 | DB 计数（20/73、11/53）、用户 quote |
| 定位 | 分层排除：先证伪最显眼的假设（模型窗口 1M token 不是瓶颈）；再沿代码路径找真正的上限（文件：行号）；查“本应工作但没跑的系统”（摘要三表 0 行）；查失败是否走到关键调用（`model_calls` 0 行） | DB 字段值、代码文件行号、0 行表 |
| 验证根因 | 单变量对照实测复现（只改一个变量有无）；区分有机故障与合成测试 | 实测输出原文 |
| 防止复发 | 回归测试（新测试 PASS 才算修完）；失败诊断记真实尺寸，不再靠重读 DB 猜；流程图文档同步更新；误诊写进记忆并标 `supersedes`，防止旧说法复活 | 测试名 + 结果、commit 号、记忆纠正条目 |
| 收尾纪律 | 修完不等于生效：写明“还要重建镜像 + 更新摘要 + 重启”才生效；修完不等于合并：dev 分支先行，合并等用户点头；SKIP 的测试写明待 CI 跑，不谎称全过 | 记忆记录、commit 记录 |

### 5.2 案例一：Bug 3，DSH 任务派生失败（根因两次反转）

误诊：配置的 node 路径 `/usr/local/bin/node` 在服务器上不存在，`/opt/morde-cloud-runtime/bin.mjs` 也没装。定位：查容器内，专用 Docker engine 与 Guardian 服务 active，镜像内 `/usr/local/bin/node` 与 `/opt/morde-cloud-runtime/bin.mjs` 都在，模板配置没错，不得“修复”成宿主机路径。纠正：误诊的根源是把“容器内路径”当成了“宿主机路径”，定位时先分清执行环境再下结论。验证根因：单变量对照实测，supervisor 用 `env -i` 起容器并传入 `MORDE_WORKSPACE_DIR`，但 `bin.mjs` 环境白名单只有 PATH / LANG / LC_ALL / TZ / NODE_ENV，抛 “unclean process environment”，每次派生回 `runtime_unavailable`；带该变量 → `{"type":"failed","code":"runtime_unavailable"}`，不带 → `{"type":"ready",...}`；邻文件 `bin-account.mjs` 白名单里早就有该变量。修复：`bin.mjs` 白名单加 `MORDE_WORKSPACE_DIR` 一个词 + 回归测试。验证修复：首次跑失败只因 worktree 缺 `node_modules`，`npm ci` 后新测试 PASS。收尾：代码修完不等于生效，服务器上重建 DSH 镜像、把新镜像摘要写进配置、重启、生产形态探针回 `ready` 才算生效（PR #1 合入 main 后已部署验证，见第 7 节）。

### 5.3 案例二：主会话“上下文太多”报错排查（2026-11-23）

发现：路由失败率高，20/73（总体）、11/53（近 2 天，约 21%）。定位：先排除模型窗口（DeepSeek Flash 连接 `context_window_tokens = 1,048,576`，与代码 catalog 一致，窗口不是瓶颈）；再找到真正的上限：主会话 c3yyitcp 本身很小（28 条消息、43,930 字符），但 7 字符消息失败于 `result_too_large` / `context_length_exceeded`，原因是合并逻辑把 earlier 大消息全拉进来，并执行硬 32 KiB 上限，在快照加载阶段就失败，在已有的 fit 修剪有机会运行之前；4 条失败消息分类都没走到模型调用（`conversation_route_model_calls` 0 行）；摘要系统从未跑过（三张摘要表全是 0 行，压缩走 DSH 会话，而 DSH 派生正被 Bug 3 堵死）。区分有机故障与合成测试：其余上下文失败是合成的长上下文回归测试，不是真实使用（单条 132,149 字符超 128 KiB 单消息协议预算 → `message_too_long`）。修复（commit `2e86e33e`）：32 KiB 合并超限不再失败，合不下的补充剥离改独立路由，并在合并文本里注明 N 条补充因长度未合并；fit 顺序反转，先把历史缩成 2048 字节首尾预览，再清空，摘要最后才丢；失败诊断开始记真实字节数（`content_bytes`、`history_bytes`）；128 KiB 单消息仍明确失败（流程图规定当前消息不得静默截断）。

### 5.4 案例三：coordinator 容量与摘要转发缺失（2026-11-23，新增）

发现：b7c7039 部署后用户在 PC 上对话仍报上下文限制，连 3 字节消息都失败。定位：日志里每条失败前都有 `conversation route model capacity is unknown; using minimum supported window`（`context_window_tokens: 4096`，`context_window_known: false`），失败阶段全是 `worker_context_fit`，`conversation_route_model_calls` 0 行；`conversation_summary_versions` 仍是 0。验证根因：`main.go` 把路由 worker 的 Models 接到 CloudDSHConversationCoordinator，但它没有 `ResolveConversationRouteContextWindow` 与 `ResolveConversationSummaryModel`，worker 的两处类型断言静默失败；4096 下限的预算只有 2272 token，固定路由指令就要约 2074 token，裁剪历史反而让请求变大（historyIncomplete 换上更长的 history search 指令），所以每条消息都在分类前失败，与内容长度无关；摘要维护的断言同样失败，摘要从没跑过。修复（PR #2，commit `5e6f9ae0`，合入 main 为 `49216f5`）：给 coordinator 补两个转发方法，都转给它内部的 model gateway；网关报不出容量时回 (4096, false, nil) 保持原有兜底；新增两个测试（接口存在性检查 + 兜底行为），worker 全量通过，gofmt / vet 干净。部署：2026-11-23 部署 release `20261123T003909Z-49216f52`，DB 仍 229，healthz / readyz 正常；无 runtime 文件改动，不用重建 DSH 镜像。**收尾未闭环**：端到端真实对话验证还等用户在 PC 上发一条消息（Muse 不能以 admin 身份发消息，服务器只存凭证哈希；手改数据库路由行会绕过重试的鉴权与幂等检查）。

### 5.5 复盘模板（八栏，P2-3 落库时用）

故障现象（一句话 + 失败原文）→ 影响范围（哪些用户 / 任务 / 时间段）→ 复现步骤（稳定复现命令 + 环境）→ 根因（调用链 + 具体行 + 数据证据，如“摘要表 0 行”）→ 纠正（改了哪几个文件 + 回归用例）→ **之前说错的话（明确写出被证伪的旧结论）** → 沉淀（lint / 检查 / skill，至少一条）→ 待办（未解风险 + 下次复查时间）。其中“之前说错的话”一栏是这套方法里最关键的一栏，没有它，复盘会变成“下次还犯”。

### 5.6 落地顺序

1. 最先：P1-2 的路由 span（约 1 天）。没有它，下一次分类失败还是只能猜；它是所有复盘的前提。Phoenix 采用需用户点头，不点头就先只做结构化失败日志（失败时把模型原文截断 2KB 写进事件 payload，不依赖 Phoenix）。
2. 其次：P1-7 的“纠正先写回归用例”（人工，S）。不等任何系统：以后每次纠正都先补一条回归用例再改代码。
3. 再次：P1-1 的独立验证者家族校验（M）。没有它，P0 改完后无法证明“改对了”。
4. 最后：P2-3 的复盘模板 + 广播（L）。等 P1-2 / P1-3 跑出两周真实失败数据再做模板，否则模板是空架子。

**验收**：TC-11.1–11.10（含 TC-11.9 八栏验收：用 Bug 3 单变量对照与上下文排查五步定位当考题复现）。

## 6. 数据与事件模型

### 6.1 任务四层结构（照 Muse 实测的样子抄）

```
用户消息（带 👍 反应，只给大活儿，是“我收到了、正在做”的记号）
   └─ 任务 Task / Activity Thread（一行记录，有进行中 / 已完成状态）
        ├─ MAIN（主 agent，depth = 0）
        │    ├─ 工作项 1：标题 + 一句话摘要 + 状态
        │    ├─ 工作项 2：…（点开 → 工具运行记录）
        │    └─ 派发子 agent（本身也是一条工作项）
        ├─ 子 agent A（depth = 1）
        │    └─ 工作项 × N
        └─ 子 agent B（depth = 1）
             └─ 工作项 × N
```

任务行字段：`name`（标题，一句话）、`subtitle`（副标题，当前进展或交付物；完成时改为交付结果）、`status`（`active` / `finished`）、`finish_status`（`success` / `waiting_for_user` / `failed`）、`created_at` / `finished_at`。列表按天分组（今天 / 昨天）。入口：点头像 → 状态屏 → Activity。运行中的任务头像下方显示“分析中”；Web 上悬停运行项可 Stop 取消。

### 6.2 任务记录七要素（工作项点开的 report 固定结构）

缺一不可：**在哪执行 + 完整命令 / 参数原文 + 退出码 + 耗时（毫秒）+ 工作目录 + 输出原文（长输出截断并注明截了多少行）+ 一句人话结论**。工作项 id 带身份，前缀即归属，可直接照抄：`root-tool-…` / `root-subagent-dispatch-…` / `subagent-start-…` / `child-tool-…` / `subagent-finish-…`。分组按 agent 现算，不建 section 表（Muse 的 section 表是空的，`MAIN` 分组是客户端按 `agent_depth` / `agent_id` 现算的；Morde 照抄，depth 0 归 MAIN，depth 1 按子 agent 各归一组）。标题生成规则要比 Muse 更稳：用“动词 + 对象”固定规则生成。

### 6.3 事件本体 9 种（P0-6 落地）

创建、拆分、派发、审批、执行、验证、交付、失败、恢复。每条带理由、成本（可 unknown）、证据指针三字段，缺一不落库。现状核实：写侧已有固定白名单 `validTaskEventType`（`task.status` / `step` / `operation` / `message` / `batch_item` / `artifact` / `waiting_user` / `response` / `result` / `plan` / `priority` 共 11 个），但那是记录类型，不是目标 9 种语义本体；读侧仍放行任意 `task.*` 前缀。落地做法：新建本体枚举 + 旧 11 类型到 9 种的映射表；新事件按 9 种写，旧事件只读映射不重写（不做全量迁移）；统计查询按新本体改。改 `server/internal/domain/task/types.go`（本体枚举）、`server/internal/app/task_service.go`（`validTaskEventType` 改为 9 种 + 旧类型映射）、新 migration（本体列 / 约束）、`server/internal/store/postgres/admin_quality_activity.go`（统计查询按新本体）。验收：9 种事件各至少一条真实记录；旧 11 类型查询仍可用（映射）；缺理由 / 证据指针的事件写不进库；任务详情可按 9 种回放全过程。

### 6.4 任务状态机

现有 9 态：queued / running / waiting_user / pausing / paused / cancelling / completed / failed / cancelled。P0-6 同批加两态：`verifying`（执行完成 → 独立验证）与 `delivered`（验证 PASS → 交付）；`waiting_user` 既用于审批等待又用于澄清等待（语义混用），拆成审批等待与澄清等待两个状态。注意规格明文：`Task.status=completed` 只表示执行器已持久化结果，不代表验收通过。状态流转：created → running → waiting_approval（高风险动作）→ running（允许）/ paused（驳回 / 超时）→ verifying（执行完成）→ delivered（PASS）/ running（FAIL + 理由，返工）→ failed（熔断 / 预算耗尽 / 失败）；paused → running（用户继续）。

### 6.5 账本

每次工具调用记账：本次价 + 任务累计实时可见；超预算 fail-closed 停任务。成本在 brief 阶段先给估算，执行时按实收。成本事实来源是 `model_call_ledger`；Phoenix 的成本只做展示与聚合，两者用 `call_id` 定期对账，差额告警。注意 hermes 的前提：它的 `spend_limit` 只盖 MCP 工具，模型花费客户端无上限；Morde 的模型花费上限必须放服务端强制执行，不能只抄客户端的自愿上限。

## 7. 分阶段落地 P0 → P1 → P2

工作量：S ≤2 天，M 3–5 天，L 1–2 周（含测试，不含等用户拍板的时间）。顺序：P0 先把“分类不报错 + 任务能拆 + 卡能拍板 + 事件能回放”四件事闭环；P1 再上验证者与评测（否则 P0 改完无法证明没改坏）；P2 再做沉淀。不要把 P2 的机制提前塞进 P0。

### P0，闭环最缺的（不做完不进 P1）

| # | 做什么 | 状态 | 验收 |
|---|---|---|---|
| P0-1 | 分类兜底链：分类器回 prose / 模型调用失败 / invalid_output 三路走关键词 + 上次路由启发式，不再直接报“无法安全分类”；仍过 `safeConversationRouteDecision` 校验；`CodeResultTooLarge` 等 fail-closed 路径保留 | ✅ 已完成：PR #1 合入 main（merge commit `d7767d6`；dev 提交 `900bd6bc`），worker 全量通过、runtime 183/183 通过、gofmt / vet 干净；已部署验证（`d7767d6` 部署，DSH 镜像重建，生产形态探针回 `ready`） | CLS-01 通过；最近 20 次路由决策 `invalid_output` 为 0；兜底三路各有回归用例 |
| P0-2 | Bug 3 落地 + 部署验证：`bin.mjs` 白名单加 `MORDE_WORKSPACE_DIR` + 回归测试；部署走正常 update 流程（重建 DSH 镜像 → 更新 portal.yaml 摘要 → 重启 portal → 生产形态 probe） | ✅ 已完成：代码随 PR #1 合入 main（`d7767d6`）；已部署验证，DSH 派生成功，探针回 `ready`；后续 b7c7039 部署又重建一次镜像（新摘要 `b2afcaf9`），探针同样回 `ready` | DSH 派生成功；`conversation_summary_versions` 7 天内出现首行（待真实使用后复核，见 5.4） |
| P0-2b | Coordinator 容量与摘要转发修复（5.4 案例三）：补 `ResolveConversationRouteContextWindow` 与 `ResolveConversationSummaryModel` 两个转发方法 | ✅ 代码已完成：PR #2（commit `5e6f9ae0`，合入 main 为 `49216f5`），worker 全量通过、gofmt / vet 干净；已部署（release `20261123T003909Z-49216f52`，DB 仍 229，healthz / readyz 正常；无 runtime 文件改动，不用重建 DSH 镜像）。⏳ 端到端真实对话验证待用户在 PC 上发一条消息 | 日志不再出现 `capacity is unknown`；c3yyitcp 的真实消息能路由（`conversation_route_model_calls` 出现记录）；`conversation_summary_versions` 从 0 上升 |
| P0-3 | Planner 最小方案 + brief 四件套（见 4.2） | ⏳ 待做 | 一句话开发任务：planner 产出 brief 四件套齐全；无验收标准的输入走 Clarify 而不是直接派发；brief 里的验收标准与最终验证输入一致 |
| P0-4 | 合并窗口最小版（见 4.2）：60s 可配窗口 + 轻量同话题判定 | ⏳ 待做 | 窗口内同话题补充只产生一个任务、一次最终答案；窗口外补充开新任务；不同话题不合并 |
| P0-5 | 审批卡证据化（本体，见 4.5） | ⏳ 待做 | 审批卡面六项齐全；成本数字与 `model_call_ledger` 对得上（有数时）；无数时明确显示 unknown，不显示 0；不可逆动作每次都出卡 |
| P0-6 | 事件本体 9 种落地最小版（见 6.3）+ 任务状态机加 `verifying` / `delivered` 两态 | ⏳ 待做 | 9 种事件各至少一条真实记录；旧 11 类型查询仍可用（映射）；任务详情可按 9 种回放全过程 |
| P0-7 | 工具注册表第一步 + 门禁四道之二：注册表（描述、权限、风险级、成本档、失败怎么办四项必填）；64 硬上限改为“注册表内工具数不限，每轮给模型 top-k”（top-k 检索本身放 P1，先做注册表 + 全量透传开关，默认仍全量，避免 P0 行为大变）；门禁先落风险分级 + 预算检查成本化骨架 | ⏳ 待做 | 工具总数超 64 任务不报错；高风险 / 不可逆动作必出审批卡；写不清四项的新工具进不了注册表 |
| P0-8 | 任务记录形态对齐（见 6.2）+ 主对话只出文字的 UI 审计一次（按设计方案 §3.7，PC / iOS 对话流里残留的内联任务卡 / 状态条 / 任务入口行清掉） | ⏳ 待做 | 任一工具调用点开详情，七要素齐全；失败 step 保留不删不改写；主对话无内联任务卡 / 状态条（20 项截图验收里相关项通过；截图验收一次没跑过，见第 9 节） |

### P1，验证闭环（证明 P0 没改坏）

| # | 做什么 | 状态 | 验收 |
|---|---|---|---|
| P1-1 | 独立验证者最小可用（见 4.6）：家族不同硬校验 + verdict 锁版本回归；Artifact 内容复核仍缺，P1 先做文本 / 证据类复核独立 | ⏳ 待做；Judge 模型 key 待用户定，未配前用已有非 DeepSeek 模型顶着并注明可信度打折 | 交付物都有非作者 verdict；同家族复核拒绝、不放行；版本变了旧 verdict 不复用 |
| P1-2 | 可观测第 1 步 + 评测 live 套件：本机 OTel Collector + Phoenix，只接路由分类一个 span（`morde.harness.route`，失败附模型原文截断 2KB 内）；44 条 live 用例写成 `morde-ai-conversation-live-v1.1.0` 套件（与 synthetic 分开跑、分开算分），先跑 P0 smoke 10 条 | ⏳ 待做；Phoenix 采用待用户点头（见第 9 节） | 下次分类失败 10 分钟内定位到模型原文；P0 smoke 10 条每次部署后全过；失败 trace 自动进评测数据集（先手动进，自动进放 P1-3） |
| P1-3 | 评测 Judge 接线 + 失败回流：Inspect AI 当 Judge 运行器；Judge 换模型家族（执行 DeepSeek、主 Judge Claude、仲裁 GPT）；温度 0；P0 双 epoch；FAIL 自动建修复任务；同一错误第二次出现沉淀为 lint / 检查 / skill | ⏳ 待做；Judge key 待配 | 连续两周全量报告；一条 P1 失败从建任务到定点重跑通过全程有记录；P0 100% PASS；满分后人抽查 20% |
| P1-4 | 上下文加固（见 4.10）：摘要失败冷却 600s 持久化 + terminal 五类必 abort + 3 次过载后确定性 fallback；溢出后压完重试一次；Tail 保护进快照；错误分类表 | ⏳ 待做；前置是 P0-2b 的摘要真正跑起来，否则冷却是“给注定失败的派生降速”，仍值得先做 | 摘要表从 0 涨起；摘要失败不再每条消息重试一次；`context_length_exceeded` 先压缩重试一次再失败 |
| P1-5 | 成本门 + 用量持久化 + 审批 guardian：per-tool 成本门（唯一检查点在 RPC 发起前，check+book 同锁，超限 fail-closed），tally 按租户持久化；模型花费上限放服务端强制；审批提示四行成本格式；Smart guardian（temperature 0、max_tokens 16、三值返回，空回答 = escalate）+ 否认熔断器（连续 3 次 DENY 停手） | ⏳ 待做 | 超预算任务自动停并说明已花多少；卡面成本数字与账单一致；空回答绝不等于批准；连续 3 次否认后停手 |
| P1-6 | 门禁补齐 + 工具轮收尾：PreToolUse 参数合法性拦截点；防循环检测（近 30 条比对，警告 → 严重 → 熔断，熔断出卡说明）；单次工具调用超时；并行调度上限 10 按序回填（先核对 DSH AgentLoop）；确定性剪枝进任务循环 + 轮首 preflight + turn 后 micro-compact | ⏳ 待做 | 同一工具重复调用触发警告直至熔断，任务停下并出卡说明；单次工具调用超时回 `TOOL_TIMEOUT` 不拖垮整批；长任务第 10 轮后不再因 tool 输出累积撞 128 KiB 失败 |
| P1-7 | 纠正变检查最小形态 + 记忆后台同步（见 4.8） | ⏳ 待做 | 被纠正过的错误在回归用例里不再出现；记忆同步不阻塞对话 |
| P1-8 | Turn 契约补齐：turn setup 顺序契约、durable turn lease（三数按 Morde 实测定）、early result 语义、迭代准备 8 项清单、system prompt 三层 + tool pin、turn receipt 绑定 | ⏳ 待做 | setup 顺序错了就报错；同一 session 同一时刻只一个 turn；拿不到租约返回 early result、消息挂下一 turn；stop-gate 钩子位挂独立验证者（P1-1） |

### P2，沉淀与规模（P1 跑稳两周后再启动）

| # | 做什么 | 状态 | 验收 |
|---|---|---|---|
| P2-1 | Skill 可写六要素 + `write_approval` stage-then-replay + curator（7 天 / 2h / 14 天 / 30 天，永不删除，归档有上限）；评估 DSH `dsh-skill` 注册表作 provider（不得暴露本地文件系统 provider、不得绕过授权） | ⏳ 待做 | agent 自写 skill 有审批、有归档、不丢；未批的 skill 不生效；curator 周期回归 |
| P2-2 | 信任阶梯状态机 + routine 窄触发 + Test 真跑：阶梯五态（观察 → 纠正 → 写成 skill → 挂 routine → 自动合并）落库；routine 窄触发条件 + 每次真跑 Test；自动合并最后才开 | ⏳ 待做 | 没走完阶梯的 skill 不能挂 routine；routine 每次触发都有 Test 真跑记录；自动合并只在阶梯走完后可用 |
| P2-3 | Postmortem + playbook 广播 + 钩子引擎：失败复盘模板（见 5.5）落库；教训写进规则全场景生效；钩子 when / who / do / ask 落盘 + 触发引擎（高风险 fail-closed，审批走 Morde 卡） | ⏳ 待做 | 一次失败复盘产出一条规则并全场景生效；钩子触发后主动跳出处理，高风险必过卡 |
| P2-4 | 评测常态化 + 盲测 panel + CodeAct + 工作图 / handoff：月度盲测 panel（抽 20%，换 Judge 家族重跑 + 人工仲裁）；CodeAct 第二条路（`dsh-code-runtime` 候选）；handoff 三件套 + 共享工作图；具名 bot 岗位化 | ⏳ 待做 | 月度 panel 有记录；CodeAct 批量操作可回放；多 Agent 任务的工作图与实际执行记录一致 |
| P2-5 | 规模：多租户适配层收尾 + 压测（tally / 限额 / 审批隔离按租户维进库；上万人并发压测与租户隔离审计；场景包复制给第二个人实测；单条消息分段读 P1 / P2：分页读 + 并行分片 + 文件范围读） | ⏳ 待做 | 并发压测达目标且租户间零串数据；场景包在另一个人账号里装上即跑通一个钩子；1 MiB 单条消息全程不失败、答案引用页码、每页 sha256 校验通过 |

**验收总标准（对话什么时候算复刻完了）**：CLS-01 通过；最近 20 次路由决策里 `invalid_output` 为 0；P0 smoke 10 条每次部署后全过，周全量 44 条 P0 100% PASS；审批卡面有本次成本与累计成本，数字与账单一致；一次失败对话能在 Phoenix 里 10 分钟内定位到具体步骤与证据；同一错误第二次出现时，已有对应的 lint / 检查 / skill，而不是第二次犯。测试用例总表见 `muse-harness-replication-test-cases.md`（156 条，TC-1.1–TC-12.21）。

## 8. 与 hermes 决定的取舍（重叠与冲突，一张表说清）

hermes 需求文档已定：19 步里 13 步搬（全搬或搬思想），不搬 6 步（CLI 形态 3 步、MoA、沙箱 local、模型调用已有）；并行用 nanoMuse 方案（上限 10、按序回填）不用 hermes 的 8 workers；落地顺序 P0 分类兜底、错误重试梯子、early result → P1 循环骨架 → P2 沉淀和拦截；多租户四点（tally 按租户持久化、服务端限额、租户隔离审批、执行隔离不降级）随各组同步做。本稿的复刻与这份决定的关系：

| 事项 | Muse 侧 | hermes 侧 | 本稿的取舍 |
|---|---|---|---|
| 分类失败兜底 | 未知（U1，无对照可抄） | 多路降级（严格 JSON → 启发式 → 问用户） | 按 hermes 的思路做，已落地 P0-1；写明这是 hermes 补齐，不是 Muse 证据 |
| 上下文压缩 | 未知（U7） | 两层阈值、先剪枝后总结、保护 head + tail、一次辅助 LLM 总结、失败冷却 | 按 hermes 的机制做（P1-4、P1-6(e)）；不抄全量历史重放与 in-place session 压缩（Morde 摘要是版本化密封文档） |
| 审批卡面与成本 | 卡片制 + 作用域制（已证实）；卡面字段未知（U8） | 卡面四行成本格式 + 成本门 + Smart guardian + 否认熔断器 | 只抄卡面格式与成本四行（P0-5、P1-5）；7 步顺序与 bypass 不抄（规格定为 C 级）；Smart guardian 与熔断器放 P1-5 |
| 并行调度 | coordinator + worker 扇出（已证实）；最大并发未知（U5） | 8 workers + 批 deadline 420s | 不用 hermes 的 8 workers，用 nanoMuse 方案（上限 10、按序回填），需求文档已定 |
| Turn 契约（租约、early result、看门狗、setup 顺序、system prompt 三层、turn receipt） | 无对照（Muse 侧无 turn 概念证据） | H-1 / H-2 / H-3 / H-4 / H-15，全搬 | 按 hermes 全搬（P1-8）；写明这是 hermes 机制，不是 Muse 行为 |
| 工具轮（校验、去重、先落盘、pre-dispatch guard、压缩） | 先落盘再执行在 Morde 已有等价实现；其余无 Muse 证据 | H-17 全搬 | 按 hermes 全搬（P1-6、P1-8）；persist-before-execute 复用 Morde 已有路径，不重写 |
| 技能自写与 curator | 无对照 | Skill 自写 + curator 周期 + `/learn` | 照抄进 P2-1 |
| 独立验证者 | 无（最接近 checking worker，同一系统内） | 无（只有查账 + nudge 的 verify-on-stop） | 新造（P1-1），不要写成“抄 hermes”或“抄 Muse” |
| Routine 与信任阶梯 | 无对照 | 无（core 无 routine，只有社区桌面插件） | 新造（P2-2），参照 Grok Bot 13 模块之 8、9 |
| Postmortem 与纠正变检查 | 纠正留痕有实例，但无自动路径实证 | 无（运行时无 postmortem，只是离线取证工具；无纠正变检查自动路径） | 新造（P1-7、P2-3），方法是第 5 节的五步法 |
| 多租户 | 明确单用户单机，无多租户形态（U16） | 单机 Python，无多租户边界 | 另起适配层（§2.4 四点），随各组同步做，不走内存、不走客户端自愿 |

## 9. 待用户拍板的事

以下只列出来，不替用户定。标“沿用”的来自目标文档与设计方案 v1.0，原文保留；标“本稿新增”的是复刻过程中新冒出来的。

**沿用（目标文档 4 件）**：

1. 会话级“总是允许”的范围：只对写操作开放，还是外发也可记？不可逆动作已定永远逐次批。
2. 一句话开发先做哪套脚手架：Web 应用还是小程序先行。
3. 独立验证者用哪个模型家族：须与执行模型不同家族，具体选型待定。
4. 三端设计稿先改 HTML 再落地，还是直接把冰淇淋蓝落进 Morde 代码。

**沿用（设计方案 v1.0 新增 3 件）**：

5. `dsh-mcp-client`：摘掉，还是用它替换手写的 MCP IPC 回调？
6. DSH 版本：继续钉在 0.2.2-rc.1，还是跟进 rc.3（插件 API 会 break，需评估迁移成本）？
7. 工作项标题生成：按“动词 + 对象”固定规则，还是沿用 Muse 的模型现写（本次实测出现同动作两种写法）？

**沿用（hermes 需求文档 §7）**：

8. Phoenix 采用确认：首选已定（自托管 $0），点头就按可观测第 1 步装；不点头就先只做结构化失败日志。
9. Judge 模型 key：Claude（主 Judge）与 GPT（仲裁）的 key 怎么配、费用谁出；没有 key 之前，Judge 先用 DeepSeek 以外已有的模型顶着，但要记一笔“Judge 与执行同家族，结果可信度打折”。
10. 多租户适配层细则：tally 按租户持久化、服务端限额、租户隔离审批，这三处的具体实现方案由谁来定（方向已定，实现未定）。

**本稿新增**：

11. 合并窗口时长：先定 60s 可配，P0 实测再调，是否接受这个起步值？
12. 子 agent 深度上限：Morde 暂定限 1，但 Muse 实测嵌套到 2 层（31%），是否放开到 2 层，还是坚持限 1？
13. 20 项截图验收一次没跑过，是否现在跑一次（P0-8 的 UI 审计验收依赖它）？
14. P0 的下一项从哪一个开始：建议顺序是 P0-3 / P0-4（任务模型与拆分）→ P0-5 / P0-6（审批 + 事件）→ P0-7 / P0-8（工具 + 记录形态），是否按这个顺序，还是先挑其中一项？

**已解决，不再等拍板**：hermes 需求 §7 第 5 项（DSH 派生修复与云电脑创建）已解决。原先的诊断（node 路径不存在、bin.mjs 未装）是错的，已纠正：真正原因是 bin.mjs 环境白名单缺 `MORDE_WORKSPACE_DIR`，修复随 PR #1 合并并部署验证；云电脑 hdqokxzv 已建好。hermes 需求 §7 第 2 项（先抄哪一组，建议 P0-1）也已完成，P0-1 已合入并部署验证。

## 附录：证据与来源索引

- 逆向工程主文档：`muse-harness-reverse-engineering.md`（§1–§11 Muse 流程推断；§12–§14 Bug 处理；§15–§18 Morde 映射与 P0 / P1 / P2；§19–§20 未知清单；§21 hermes 补齐标注表；§22 来源索引）。底稿：`~/workspace/muse-harness-rev/part-a-evidence.md`（行为证据，26 条 SQL 查询）／`part-b-docs.md`（文档分析，58 个 skill + 143 个 connector 清单）／`part-c-hermes.md`（hermes 精读，固定提交 0d49d6d9）／`part-d-morde-mapping.md`（Morde 映射）。
- 复刻验收用例：`muse-harness-replication-test-cases.md`（156 条，TC-1.1–TC-12.21；黑盒 96 条、代码级 60 条；合理推断 3 条、未知 9 条、hermes 补齐 5 条，均已标注）。
- 设计方案 v1.0：`morde-design-proposal/morde-design-proposal.md`（六层结构、任务模型、brief、审批卡、事件本体、工具注册表、门禁、插件接线）。
- 目标文档：`morde-target-design/morde-target-design.md`（终态、五条护栏、实施路线、等用户拍板的 4 件事）。
- hermes 需求文档：`morde-hermes-copy-requirements.md`（§2.2 六组、§2.4 多租户四点、§2.6 19 步定稿、§5 P0 → P1 → P2、§7 待用户定的事）。
- 上下文研究：`~/workspace/hermes-context-research.md`（Morde 路由 7 步链、任务执行拿历史的方式、单条消息分段读三阶段）。
- 流程图页：`muse-harness-liu-cheng-tu`（Muse Harness 流程图，13 张详细图，节点带证据等级标签）。
- 核实基线：逆向工程核实时 Morde HEAD 为 `2e86e33e`（dev 分支顶）；本稿成稿时 main 已到 `49216f5`（PR #1 `d7767d6` + PR #2 `49216f5`），第 7 节的状态按成稿时更新。后续每次落地 P0 / P1 / P2 里的某一项，应回填一条“已实现 / 验收结果”到第 7 节对应行，保持本文与代码一致。
