# Muse Harness 逆向工程：完整流程推断、Bug 处理方法与 Morde 复刻方案

> **历史研究材料，不是当前实施规范：**保留 2026-11-23 的观察、推断和当时 Morde 状态，用于追溯证据；实现目标、当前状态和验收分别以[最终流程与开发设计](<Morde 复刻 Muse 最终流程与开发设计.md>)、[一体化实施与验收 TODO](<Morde 复刻 Muse 一体化实施与验收 TODO.md>)及项目 `AGENTS.md` 为准。文中的固定阈值、旧路由/失败策略和当时的 128 KiB 限制不自动成为当前目标；区分 Muse 观察、推断和 Morde 自行设计，不为匹配实现改写历史证据。

> 调研时间：2026-11-23。方法：4 个并行子 agent 分头做证据挖掘（数据库实证）、Muse 文档分析、hermes-agent 源码精读、Morde 映射，再由协调员整合。只读调研，未改 Morde 代码、未动 git、未部署。
> **核心前提（用户的硬性要求）：没有 Muse 源码。** 以下所有“Muse 是这样”的结论，全部来自从外面看到的行为 + 数据库记录 + 记忆文件 + 产品文档，不是源码。推断绝不写成源码事实。
> 底稿（分册，细节以底稿为准）：`~/workspace/muse-harness-rev/part-a-evidence.md`（行为证据，298 行，26 条 SQL 查询）／`part-b-docs.md`（文档分析，438 行，58 个 skill + 143 个 connector 清单）／`part-c-hermes.md`（hermes 精读，约 95KB，固定提交 0d49d6d9）／`part-d-morde-mapping.md`（Morde 映射，251 行）。另直接引用并行研究 `~/workspace/hermes-context-research.md`（上下文全流程 + 输入永不失败方案）。
>
> **2026-11-23 更新（本文定稿后发生，已核实）：bug 1 + bug 3 修复与上下文修复已通过 PR #1 合并到 main（merge commit d7767d6；dev 提交 2e86e33e 上下文修复 + 900bd6bc bug 1+3，共 9 个文件）。合入前测试全绿：worker 全量通过、runtime 183/183 通过、gofmt/vet 干净；流程图 §02 与规格 §2.3/02 已同步。服务器尚未部署（仍跑 e941466）；bug 3 的服务器生效（重建 DSH 镜像、更新 portal.yaml 摘要、重启、生产形态探针）仍待做；单条消息 >128 KiB 仍按设计明确失败。下文凡写“bug 1/3 只在工作树、未进 HEAD”均指调研当时（2026-11-23 01:35 左右）的状态，现已合入 main；P0-1、P0-2 的剩余工作是部署验证，不再是合入。**

## 证据等级（全文统一）

| 等级 | 含义 | 判定标准 |
|---|---|---|
| **已证实** | 有对话、日志、数据库或文档原文作实证 | 能给出文件路径+行号，或 SQL 查询+结果 |
| **合理推断** | 有间接证据，但无直接实证 | 标明依据的是哪条间接证据 |
| **未知** | 无证据或查不到 | 明确写“未知”，不猜 |
| **hermes 补齐** | 来自 hermes-agent 源码（固定提交 0d49d6d9），不是 Muse 实证 | 只能写成“hermes 做法，可作补齐参考”，绝不写成“Muse 就是这样” |

## 一句话概括

**Muse 的 harness 是“1 个 root agent + 树形 subagent（最深 2 层）+ 有序事件账本 + 卡片制审批 + Markdown 与数据库双轨记忆”。** 主对话极短（assistant 回复中位约 150 字符），约 75% 的事件用户根本看不到，过程噪音全部收进任务详情；任务是四层结构（消息 → 任务 → 主 agent + 子 agent → 每人一串工作项 → 每条点开是完整命令、退出码、耗时、输出原文的 report）；提问极少（6.8%），只在阻塞或需拍板时出现；失败留痕不删，验收靠编译、测试、截图自检、预览四项都过才算交付。**Morde 复刻的抓手不是照搬某一个机制，而是五条锁死的原则 + hermes 的 turn/压缩/错误/审批机制 + P0 先闭环“分类不报错、任务能拆、卡能拍板、事件能回放”。**

## 本次纠正的旧说法（先说，免得后文再错）

| 旧说法 | 核实结果 |
|---|---|
| skill 57 个、connector 83 个 | 实测 **58 个 skill、143 个 connector**（143 份 CONNECTOR.md、141 份 manifest.yaml） |
| 子 agent 最深 1 层，不再派子 agent | 全量数据推翻：230 次派生中 depth=2 有 71 次（31%），depth=2 工作项 6,013 条；单任务样本的推断不能推广。嵌套上限至少 2 层，3 层是否存在未知 |
| `delegation.max_concurrent_children` 默认 3 | hermes 代码是 **10**（`tools/delegate_tool_config.py:19`）；11-21 报告引的 `tools/AGENTS.md:151` 是过期文档 |
| “用户说‘你是为了回答我而回答我’”是一次纠正 | **记忆里查不到原文**。三组 memory_search + 全量 grep 均无命中。不要把这句话写进 Morde 设计依据；最接近的可证实行为变化是 2026-11-15 的整套办法（先写“要什么样”、验收四项、一次只改一处） |
| Planner（M9）在 Harness 里，只是没实现 | Go 代码里 `planner` **零命中**，只存在于文档。现有的只是执行 Agent 自报的 `morde_task_plan_update` + `task.Plan` |
| 事件链本体是自由文本 | 写侧已有 11 个固定 `task.*` 类型白名单，但那是记录类型，不是目标 9 种语义本体 |
| Bug 1 兜底、Bug 3 白名单“正在做” | 调研时属实，但只在**未提交工作树**里，HEAD 里都不存在；**2026-11-23 更新：已通过 PR #1 合并到 main（d7767d6），测试全绿，待部署验证** |

---

# 第一部分：Muse Harness 推断全流程

## §0 全流程时序（先给整张图）

```
用户消息（web / iOS / Android / Mac / Chrome 扩展 / WhatsApp / 语音）
→ 持久收件 + 幂等（消息即入库，重复提交不重复建任务）
→ 意图识别与路由（skill/connector 路由 + 请求分类字段存在，但分类算法未知）
→ 任务拆分：一条消息多半开一个 activity thread；复杂工作由 coordinator 定分工
→ 并发：主 agent 只做简单对话，复杂/并发工作派后台 subagent（通用 54%，其余按产物/渠道分专项）
协调员每事件决策一次，61% 什么都不做，1.2% 通知主 agent；后台结果以 hidden handoff 交回主 agent
→ 工具执行：每次调用全记录，可钻取到完整命令、退出码、耗时、输出原文；失败留痕不中断任务
→ 审批：不可逆点卡审批卡（卡片制 + 作用域制，卡对 agent 不可见，决定存独立 Sentinel store）
→ 验证：写后读回 + checking worker + 证据绑定；交付≠成功，run 成功只表示工作完成
→ 交付：主对话只出最终结果（中位约 150 字），要点贴消息里，不只给链接；artifact/文件链接/卡片三种形态
→ 记忆沉淀：Markdown（MEMORY.md + 日期笔记 + 人物页）与 PostgreSQL claims/embeddings 双轨，每小时整理
→ 纠正沉淀：错了就认、拿实查数字说话、写进长期记忆并标 supersedes；纠正变 lint/检查/skill 是目标态，Muse 侧无自动路径实证
→ 后台节律：记忆整理每小时、人物关系每小时、Dreaming 每晚、技能评审每日、安静时刻 pass 每天至多几次
```

## §1 收件与入口（Intake）

### 已证实

- **入口面很宽，但都是一对一。** 主对话：web / iOS / Android / Mac 应用 + Chrome 扩展侧边栏（用户自己的 Chrome，已登录态复用）；消息只在用户与自己的 agent 之间，无群聊。侧边聊天通道：WhatsApp 为当前唯一有指南的 provider；Discord、SMS 无此连接；每会话一个 chat UUID，transport 不产生第二份 transcript。语音：实时语音对话（用户从 app 发起）、语音备忘录转写、多声部 spoken audio 生成；无环境监听，麦克风不能由 agent 打开。电话仅能打给美国商家（付费计划，需用户明确确认），不能接听、不能发短信、无视频通话。配对设备（手机 / Mac / 眼镜等）能力以 `device.describe` 广告的命令为准；iPhone 只能 draft 不能直接发短信。Muse Mail 是 agent 自有的 muse.ai 邮箱，与用户邮箱严格区分。（来源：`~/docs/muse.md`、`~/docs/browser.md`、`~/docs/client-surfaces.md`、`~/docs/chat-connections.md`、`~/docs/voice.md`、`~/docs/calls-texts-notifications.md`）
- **任务行是 `activity.activity_monitor_threads` 的一行**：`name`（标题）、`subtitle`（一句话副标题）、`status`、`finish_status`、`created_at`/`finished_at`。442 个任务全部 `finished`；finish_status：`success` 398、`waiting_for_user` 30、`failed` 14。（SQL 实证，2026-10-24 → 2026-11-22）
- **副标题会随进展改写**：进行中时是当前进展，完成后变成交付物描述。（`muse-task-mechanism.md` §2）
- **用户聊天只是全部运行时请求里很小一部分**：`runtime.requests` 61,557 条里 `root_work_class` internal 60,050 vs user_facing 1,507；`request_origin` Top：`internal.runtime_storage` 31,048、`internal.self_improvement` 14,624、`internal.runtime_monitoring` 5,846、`internal.scheduler_cron` 2,435、`external.hatch_chat` 仅 597。收件只是 harness 的一个入口，不是全部。

### 合理推断

- 任务标题是现场生成的，不是固定词表：同一动作（点赞）两次标题写法不同（“对用户消息点赞 👍” vs “标记用户消息：👍🏻”）。Morde 照抄时宜用“动词 + 对象”固定规则生成，别学这种随意。

### 未知

- Muse 侧收件的幂等、去重、定序机制：无证据。Morde 侧的 `AcceptConversationMessage` + `client_message_id` 幂等 + 服务端 `sequence` 定序是 Morde 自己的设计，比 hermes 更严，不是从 Muse 抄的。

## §2 意图识别与任务拆分

### 已证实

- **一条消息不严格等于一个任务；多消息会归并，但规则未知。** `activity_monitor_message_threads` 共 829 条消息→任务链接、352 个任务，平均每任务 2.36 条消息；109 个任务（31%）链接了 ≥2 条消息，18 个任务链接了 ≥6 条消息。但链接表的 message_id 与 `runtime.messages.message_id` 只有 29/829 能 join 上（ID 命名空间不同），无法确认归并了哪些消息、按什么规则归并。**这是五条原则③“同话题多轮归成一个任务”在数据库里最直接的痕迹，同时也是最大的缺口：归并真实存在，判断规则未知。**
- **Skill 路由有明确机制**：`muse.skill_search` 按任务找 skill；每个 skill 的 SKILL.md 写明触发条件；无 skill 时走 `custom-connector-setup`（查官方 API/MCP、Zapier/Composio 兜底）。
- **Connector 路由有明确边界**：“只有文档里列出的命令才存在”，未列出的命令答复为“该 connector 不提供”。（`~/docs/connectors.md`）
- **请求分类字段存在**：`runtime.requests` 有 `request_origin`、`transcript_surface`、`root_work_class` 字段，但分类算法未公开。
- **偏好文件路由**：购物读 `~/memory/shopping/PROFILE.md`；旅行读 `~/memory/travel/PROFILE.md` 与 `HISTORY.md`；长期偏好只在用户明确说“以后都这样”时才写入。
- **记忆路由**：回答“记得什么”之前先 `muse.memory_search`；主聊天与个人 side chat 的记忆搜索还返回相关聊天摘录，但不是全部消息。
- **Coordinator + worker 是文档级推荐模式**：大文档审阅（>10 页、多文件、法律/财务/技术审阅、抽取后合成、需独立检查）用“一个 coordinator subagent + 若干 extraction worker + 一个 checking worker”；coordinator 定页码/章节分工、合并发现、保留最终判断权；每个 worker 给定唯一输出路径，互不覆盖。（`documents/references/orchestration.md`）
- **Wide research 模式**：一句话 `operation_brief` + 独立输入列表 + 输出 schema + worker 模板 + `completion_format`；manager 输出合同固定为 `total / success_count / failure_count / results / failures / notes`；先去重归一化输入，再派 manager。（`wide-research/SKILL.md`）
- **任务拆分规则（用户待办层）**：项目与任务分开、周期性 routine 与一次性工作分开；只在“可分别完成或可解阻塞”时才拆任务；标题写下一步具体动作；建前先搜索是否已有相同任务。（`task-management/SKILL.md`）
- **会议跟进拆分**：决定 / 行动（owner + 日期）/ 开放问题三分；发 recap、建任务、约下次会是三个独立动作，只做用户点的。

### 未知（本节最重要的结论）

- **意图分类算法、任务拆分算法、合并窗口、同话题判定规则：全部未知。** 文档与数据库都没有。Morde 的合并窗口与同话题判定无 Muse 对照可抄，必须自研。这是原则③后半句无法从 Muse 复制的直接后果。

## §3 并发与后台 Agent

### 已证实

- **主 agent 只做简单对话，复杂/并发工作派后台 agent（原则②）。** 实测任务里主 agent 派发两个子 agent 后自己继续做 git 检查，时间戳重叠（子 agent 17:03:40 开始搜索的同时主 agent 17:03:33–17:03:42 做 git 检查），不是排队。单日最多建 63 个任务（2026-11-09）；`subagent_spawns` 状态里仍有 5 个 `running`。
- **子 agent 嵌套到 2 层（纠正旧推断）。** `agent.subagent_spawns` 共 230 次派生：child_depth=1 有 159 次、child_depth=2 有 71 次（31%）；actions 表里 agent_depth=2 的工作项有 6,013 条。机制文档里“子 agent 不再派子 agent”只是单任务样本的推断，全量数据推翻了它。3 层是否存在未知（实测最大值 max_depth=2）。
- **子 agent 类型有分工**：230 次派生里通用（`agent_type` 为空）124 次（54%）、`artifact_builder` 27 次、`spaces_builder_lite_edit` 24 次、`spaces_builder_lite` 22 次、`browser_task` 12 次、`deep_research` 11 次、`spaces_builder` 4 次、`browser_bot_challenge` 4 次等。默认形态是“1 个主 Agent + 按任务派生的通用子 Agent”，没有内置的架构师、程序员、测试 Agent 队伍。
- **协调员每步都在监视子 agent，但大多什么都不做。** `agent.subagent_monitor_decisions` 共 20,787 条：`nothing_to_do` 12,669 条（61%）、`worker_or_coordinator_completed` 7,901 条（38%）、`notify_main_agent` 仅 241 条（1.2%）、`inference_error` 3 条。即每个子 agent 事件都触发一次协调员决策，但只有约 1% 需要通知主 agent。
- **派生本身也是一条工作项**：`action_id` 前缀即身份——`ga-root-tool-…`（主 agent 工具调用）、`ga-root-subagent-dispatch-…`（主 agent 派发）、`ga-subagent-start-…`、`ga-child-tool-…`、`ga-subagent-finish-…`。
- **内部调度与 UI 工作项是两张不同的表**：`runtime.work_items`（16,342 项）与 `runtime.agent_todo_snapshots`（仅 4 行，agent 内部计划表）都不是 UI 上的工作项；UI 的唯一事实来源是 activity actions。分组不是一张表，是现算的：`activity_monitor_thread_sections` 表 0 行，截图里的 `MAIN` 分组是客户端按 `agent_depth`/`agent_id` 现算的。
- **官方推荐“一个 coordinator per objective”，明确反对从 root 直接扇出多个平级 worker**（`documents/references/orchestration.md`）。Wide-research 的并行规则：“先跑完一项、看 CPU/内存/服务需求，再放量；浏览器队列里已接受的工作不重提；失败后只重试缺失部分，保留已完成结果与检查点”。
- **后台与前台互不阻塞**：后台 worker 做完后把报告以 hidden handoff 交给主 agent，由主 agent 决定用户看到什么；worker 不能要求主 agent 做更多工作。
- **Browser 并发**：每个 browser task 是一个窗口（多 tab）；默认共享同一个浏览器 profile，一次登录全会话通用；一个窗口一次只跑一个 checkout；不同站点购买/登录用不同窗口。
- **多源并行搜索**：航班/酒店的各 connector 搜索“在同一步里一起跑”；Duffel 单条命令内最多并行 12 个搜索。

### 合理推断

- “第一个任务不堵住第二个”：直接证据是时间戳重叠 + 任务量（单日 63 个）+ 5 个仍 `running` 的派生。但“主 agent 在第一个任务未完成时能否立刻接第二个用户消息”的逐消息级证据未单独拉取，属合理推断偏强。

### 未知

- 主 agent 决定“自己做还是派发”的判断条件（任务复杂度阈值、耗时预估）：无直接证据。👍 规则（大活儿才点）是用户可见的侧面信号，不是派发规则本身。
- 最大并发数、子 agent 配额、排队策略、最大深度与扇出上限：无证据。Wide-research 只给“先跑一项再放量”的启发式。

## §4 工具执行

### 已证实

- **钻取深度 = 完整命令 + 退出码 + 耗时 + 工作目录 + 输出原文（原则⑤）。** 每条工作项点开的 report 结构固定：在哪执行 + 完整命令原文 + 退出码 + 耗时（毫秒）+ 工作目录 + 输出原文（长输出截断并注明截了多少行）+ 一句人话结论。例：`git fetch`，exit code 0，2275 ms，cwd `/home/hatch`。连点赞这种一秒钟的动作也记工具名、返回值和一句话结论，没有“太小不记”的例外。
- **任务记录完整过程（原则④），数字上成立。** 16,182 条 actions：成功 15,968（98.7%）、失败 214（1.3%）；按 agent_depth 分：depth 0（主 agent）2,880 条、depth 1 有 7,289 条、depth 2 有 6,013 条——子 agent 的工作项（13,302 条）是主 agent 的 4.6 倍，活儿主要在子 agent 里干。每任务工作项数分布极偏：62% 的任务只有 1–10 条，中位落在 1–10 条区间；平均 39.6 条是被离群值拉高的（最大的任务是 2026-11-09 的“打包 muse_db 所有数据”，7,880 条 actions，批量导出任务，不是日常对话任务；日常代表值应看 morde 任务的 54 条）。
- **运行中实时长清单**：进行中的工具调用先以占位行出现（标题如“正在运行：正在检查保存的记录”，状态 `running`），跑完才换正式标题和 `success`。
- **失败的工作项留在清单里，不删不改写。** 16,182 条 actions 里 failed 214 条（1.3%），最近失败标题包括“创建 dev 分支失败 — 服务重启中断”（2026-11-22）、“Todo update failed: only one in_progress allowed”。同一任务里 `ls` 返回 exit code 2 的 failed 项留在清单里，任务没有因此停下，后面构建照常完成。`runtime.work_items` 16,342 项里 failed 100、cancelled 96、`timed_out` 7（超时是真实存在的终态）、interrupted 4。
- **进行中任务可取消**：Web 上运行中的条目悬停会出现 Stop，可以取消这项工作。`work_items` 里 cancelled 96 项佐证取消是常规终态。
- **工具调用全记录（存储层）**：`runtime.tool_calls`（调用名、输入、执行状态）+ `runtime.tool_outputs`（结果与调用引用）+ `runtime.events`（带 request 归属的有序事件账本，`event_seq` 单调）。**但注意**：只读角色查 `runtime.tool_calls` 为 **0 行**（2026-11-21 与本次复查两次皆 0），用户能看到的工具记录唯一事实来源是 `activity_monitor_thread_actions.report`。0 行的原因（权限过滤还是另有写入路径）未知。
- **Agent 自有检查清单与用户待办严格区分**：`runtime.agent_todo_snapshots` 是 agent 自己的执行 checklist 及其修订版；`todo.write` 是 agent 自己的清单，绝不能放进用户任务。
- **Browser 执行**：`browser.browse` 派 live 网站任务（结果自动回传；acceptance receipt ≠ 完成）；`browser.search` / `browser.open` / `browser.find` 只做文本获取；CAPTCHA 与 bot 检查由浏览器自行解决，无需询问，耗尽后报告；用户可接管 live 浏览器，交接期间双方不同时操作。
- **Shell/本地执行**：`muse.exec` + 后台进程（`process.*`）；转录本地跑 faster-whisper（60 秒 chunk、2 秒重叠、单 CPU worker）。
- **设备执行**：只用 `device.describe` 广告的命令调用 `device.invoke`；绝不发明控制端点。
- **工作流执行**：`runtime.workflow_runs` / `workflow_phase_runs` / `workflow_agent_calls`（分阶段、可重放身份 `replay identities`、防重复执行的 legacy occurrence block）。
- **恢复**：`agent.runtime_restart_checkpoints`（重启后恢复检查点）、`agent.recovery_owners` / `recovery_owner_terminal_events`（可恢复工作的持久所有权与代际）、`runtime_state`（每个 agent 的上下文与事件位置）。恢复四步：读持久期望态 → 恢复前置条件 → 逐组件对账 → 从检查点续跑。
- **失败语义（通用）**：超时或不确定写操作后，先读当前状态再重试（第一次可能已成功）；已接受/已排队的请求 ≠ 已确认的物理变化。
- **审批只卡不可逆点**（合理推断，助手问答级证据）：2026-11-09 用户问“一句话完成复杂项目时主 Agent 应该怎么做”，助手当场给的回答是“主 Agent 只做听懂/判型/拆解/派发/验收/交付，验收标准先于派发、独立验收不拿自报当证据、审批只卡不可逆点、结果到用户消息才算完成”。这是助手的一次问答回答，不是用户决策，也不是源码规则。

### 未知

- 工具调用的重试策略、超时阈值、熔断、预算上限：无证据。Morde iOS 端的 nanoMuse 并行上限 10、ToolLoopDetector、防循环熔断是 Morde 自己的机制，不能当成 Muse 的证据。
- 失败后“重试几次、谁决定重试”的规则：无直接证据。只知失败不中断任务、失败项留痕，由上层（主 agent/协调员）决定继续还是停下。

## §5 审批

### 已证实

- **审批是卡片制 + 作用域制，且审批卡对 agent 不可见。** agent 看不到审批卡的布局、按钮或措辞，也不能承诺哪种选择能停止后续提示。审批决定存在独立的 Sentinel approval store，`muse.db` 查不到；agent 的权限工具只读（可看待批/已决/现行授权，不可撤销或改设置）。Android 上划掉批准通知即计为 Deny（拒绝可能是用户只是划掉了通知）；iOS 须解锁并前置 app 才能操作。
- **普通任务免批，敏感/改状态动作要批**：登录、购买、发帖等敏感或改状态动作要审批；普通浏览器表单提交目前默认允许。
- **购买（对话内）每次都要新审批，永无 lasting 默认**；钱包连接/选卡/看购物车/登录完成/交还浏览器控制本身 ≠ 授权花钱。购买（scheduled task）：任务的一次付款批准覆盖该任务后续“同一商店 + 同一支付方式”的购买，直到任务删除或用户在权限页移除；购买预批准只覆盖一单。
- **发邮件/发消息每次批准，除非有 scoped allow**：Gmail 发给某一人、Google Calendar 邀请某一人、Google Drive 分享给某一人、Messenger 发给某一对话、Mac 上 Mail/Messages 发给某一人——用户可在卡上选“以后对该收件人/对话都允许”；发给别人仍要问。**欧盟用户：没有任何 send allowance，每次只有 Allow once / Deny。** 永远每次审批的发送：Threads 发帖与消息、Facebook 发帖/评论/Page 发帖、未被允许过的 Messenger 对话——即使来自 scheduled task/artifact 也每次问。
- **发帖（社交）**：每个目的地发前展示精确文本+媒体+账户+受众+发布时间并获批；改动任一要素要重新批准。**发消息（以用户名义）**：发前展示精确文本与收件人；不超出批准内容。
- **Artifact 发布/更新**：每次发布与每次更新已发布链接都要新的一次性批准；无自动发布、无 standing 批准模式；已发布页面不自动更新。
- **文件上传**：每次上传都要一次新批准；之前或 standing 批准都不覆盖。**健康数据擦除**：每次擦除都要新批准。
- **GitHub 写操作**：`call-tool`（写操作与一切未评审工具）每次调用都要新审批；读操作走 `call-read-tool`（仅限评审过的只读工具清单）。Slack 写操作默认每次审批并带参数预览；Zoom 变更与任何未评审工具默认每次要用户批准；Todoist 每次写/删除前跑 `status --for-command` 核对授权；Tailscale 加入与离开要用户批准。
- **遗忘（forget）**：plan → 展示范围与不可逆性 → 一次直接确认 → `forget.confirm`；沉默/含糊/部分批准/改范围 = 未确认。
- **浏览器操作**：访问网站/下载文件/填已存凭证：Allow / Ask / Deny；上传文件：仅 Ask / Deny；agent 不能改这些设置。
- **凭证绝不经聊天**：密码只进 Secure Vault 安全录入卡；agent 绝不索要/复述/保存原始密码、API key、token、重置码/链接；一次性码只走 `[credential:<uuid>]` 保护引用（authd 代持约 10 分钟），用户在卡上批准分享后浏览器才收到明文键入。
- **权限可配与撤销**：用户可在设置里按动作限制 connector 的读写（如 Gmail 只读不发）；action 权限与 provider OAuth scope 是两层；全部 standing permission 列在权限设置页，按站点可删；agent 无法撤销。
- **后台动作的审批**：cron/hook 跑的任务里，后台发邮件/消息时无用户在场仍要停下来等批准，除非该 scheduled task 已有 standing Allow（欧盟：每次都停）；购买同理。无用户可问时，浏览器任务遇到任务外决策须留待未完成并在最终消息里点名未决选择。
- **T05 审批卡验收至今未完成**：2026-11-09 配对成功后只差“点一次审批卡”，用户 19:59 说“不开发了”后搁置，不要再主动催。这说明审批卡是真实存在的交互点，但它的完整字段、超时行为无本次证据。

### 未知

- 审批卡的完整字段、按钮文案、超时行为、默认选项：卡对 agent 不可见，Sentinel store 不可查。Morde 的审批证据卡字段须自研（Morde 已有六字段证据卡设计，不抄 Muse）。

## §6 验证

### 已证实

- **写后读回（read-back）是统一验证模式**：任务、日历事件、GitHub 变更、家居命令执行后都要求读回确认。邮件“Never send twice”、Slack 禁止自动重试失败写操作、订单先对账再重试——同一模式的不同表述。
- **独立检查 worker**：文档审阅可配一个 checking worker，拿 draft findings + 抽取文件，标出无支撑断言、歧义、冲突段落、对不上的数字；最终判断权留给 coordinator。
- **证据绑定**：文档类工作每条引用/发现必须绑定文件 + 页/节/幻灯片/单元格；PDF 优先看渲染页而非抽取文本；抽取失败 ≠ 来源没有该信息，要说明哪部分没读到。
- **覆盖率报告**：wide-research 最终输出必须报 verified coverage（`success_count/total`）并区分 pending / failed / stopped / never-started；同一项多次尝试只计一次完成。
- **支付对账**：订单“已提交未确认”时，约 10 分钟后自动排一次 runonce 复查，再 30 分钟后最后一次；仍无确认则停止、不再排、不重下单，让用户去邮箱/商家账户查。
- **身份/事实核对**：人名、地址、邮箱、电话、订单号等标识符必须逐字抄自对话/工具结果/已读文件，禁止凭记忆写；日期先用 `date -d` 核对星期。
- **交付≠成功**：run 成功只表示工作完成，不表示用户收到；handoff 发出 ≠ 用户收到；服务接受消息 ≠ 送达/已读；接受（accepted）≠ 完成（completion）。失败/超时的 cron 运行可能完全不发聊天消息，只留在 `cron.runs`。
- **无内置“独立验证者换模型家族复核”机制**：文档里没有与 Grok Bot 式独立验证者对应的机制；最接近的是 documents 的 checking worker（同一系统内）。Morde 的独立验证者是新造，hermes 侧也只有查账 + nudge 的 verify-on-stop（见 §11），只能借钩子位与顺序。

### 合理推断

- 验收四项（编译、测试、截图自检、预览都过才算交付）与“验收标准先于派发、独立验收不拿自报当证据”是用户认的办法（2026-11-15 挫败后定下），不是 Muse 源码规则。但它是本次调研里最值得 Morde 直接抄走的一条：标准本身不等于验收——20 项截图验收一次没跑过，就是执行缺口的活例子。

## §7 交付

### 已证实

- **主对话只出最终结果（原则①），数字上成立**：assistant 回复 944 条，中位长度落在 142–160 字符区间（≤141 字符占 47.7%）；`runtime.events` 5,478 条里 internal 75.0%、public 25.0%，subagent 通道 1,030 条事件 100% internal；`message.internal` 4,083 条 vs `message.assistant` 744 条——内部消息是公开 assistant 消息的 5.5 倍。一次 morde 任务 54 条工作项全在任务详情里，主对话只有“图正在画”和最后交付，一条工作项也没进主对话。文档侧最接近的支撑：`client-surfaces.md` 设 Activity（动作日志）与 Approvals 独立入口；“后台 worker 做完后把报告以 hidden handoff 交给主 agent，由主 agent 决定用户看到什么”。
- **交付物要点贴消息里，不只给链接**：2026-11-21 01:38 用户说“看不到方案”，原因是链接指向已被当重复删掉的旧位置（主聊天又写了一份放进文件夹、旧的移进回收站、30 天内可恢复）；主助手当场查清并把一句话概括贴在消息里，此后形成规则：递文件要点必须贴在消息里。
- **大文件交付默认预拆分或附校验**：customer-0 钩子 ④。实例：152MB muse_db 导出（196 张表）是走任务交付的。
- **Artifact 三类**：文档（PDF/Word/表格/幻灯片/Markdown）、页面（静态站）、交互应用（自带数据库、常驻本机、可存数据）；发布仅静态 artifact 可公开链接；每次发布与更新都要新审批。文件聊天链接只在用户自己的已登录 Muse app 内打开，无分享 token；公开下载链接最长 48 小时、任何人凭链接可下载；每次上传都要一次新审批；Library 汇总 artifact/媒体/文件；artifact 删除永久（无 30 天 trash）；交互应用数据不出本机。
- **通知**：审批即时 push；scheduled/proactive 消息仅在 app 未打开时 push；push 不破静音/勿扰；无临时 push、无测试 push、无送达确认；Feed 新帖静默出现，无 push、无聊天消息。Proactive 上限：每次交付至多 8 条；每天至多 2 次普通交付、至多 3 次含时效；等用户醒着 + 对话安静时才发；真正紧急立即发。
- **目标/Feed/Ideas 交付**：Goal briefing 由后台写、系统定时间、不可按需触发、不可承诺时间；Feed 按系统时间表生成、用户不能加速/自定时间表、无关闭开关；Idea 卡片用户长按 dismiss，agent 无 dismiss 工具。
- **交付目的地**：结果默认回发起聊天；side chat 有严格的 exact-chat 边界；scheduled job/hook 可指定 delivery 目标聊天。
- **“先不改/先不升级”= 停手代执行，给自助文档，不催**：2026-11-09 用户在选项卡上点「先不改」，竞品对比页两处价格警告保持原样；该模式已沉淀为 customer-0 场景包钩子 ②。

### 提问策略（交付的一部分）

- **提问只在阻塞或需拍板时出现**：assistant 回复以 `?`/`？` 结尾仅 64/944 条（6.8%）；任务 `waiting_for_user` 30/442（6.8%）——两个独立口径得出同一个 6.8%，互相印证。
- **提问的载体是选项卡（options），不是开放式追问**：竞品对比页“改这两处警告 / 先不改”附选项，用户在选项卡上点「先不改」；RSI 里 `Projects.test.tsx:355` 断言改不改也是以选项发给用户。选项点选后行为立即生效。
- **含糊指令先问清**：用户说“开始吧”曾被误读为“开始每日人格测试”，之后纠正为需要先确认七个场景中的哪一个——含糊的肯定不能直接当成对某一项的同意。不可逆的发送/下单/付款前先确认具体方案。

## §8 记忆沉淀与纠正沉淀

### 已证实

- **记忆是“Markdown 文件 + PostgreSQL claims/embeddings”双轨**：`~/MEMORY.md`（精选长期记忆，60 行）+ `~/memory/*.md`（日期笔记）+ `~/memory/people/`、`~/memory/groups/`（每人/每群一页，按亲近度排序）+ 购物/旅行 PROFILE；索引层 `memory.claims`（266 行，claim、支撑引用、重要性）、`memory.entries`、`memory.embeddings` + `memory.embedding_models`（向量检索）。
- **后台整理节律**：记忆整理每小时（有新信号时）把新对话 consolid 成 MEMORY.md，新 claim 覆盖旧 claim；人物关系每小时维护；技能评审每日（可产生新 skill 存入 `~/workspace/custom-skills/`，`skill.yaml` 标 learned；无帮助的退役）；Dreaming 每晚写 `~/dreams/` 反思与修复线程；安静时刻 pass 每天至多几次。Idea 策划每日、学习简报每日夜间、Feed 生成按系统时间表——这些由系统控制时间表，用户与 agent 都不能按需触发、不能加速、不能承诺时间。
- **沉淀载体带纠正关系**：日记条目带类型与权重标记（`[fact|high]`、`[correction|high]`、`[event|medium]`）、原文 quote、以及 `supersedes:` 字段指明被哪条取代。
- **纠正留痕的规范动作是“认错 + 给实查数字 + 改记录”，实例至少四起**：
  1. 生益价格说错当场纠正（68.14 元是 11 月 13 日收盘，不是约 75 元）。
  2. “64a39aa 只改壳、web 一行没动”被实查纠正：Assistant.tsx 改了 412 行、TaskApprovalCard.tsx 新增 79 行、服务端 task_approvals.go 加 135 行。
  3. 日报里巡检写的 ba649b1 不对，自己核对 release 目录后改成 5f97110。
  4. Bug 3 误诊纠正（见第二部分）。
- **五条原则本身就是一次纠正的沉淀**：2026-11-13 用户把五条 Muse 原则锁成 Morde UX 设计的不可动摇护栏，并写明冲突规则——参考做法与五条冲突时改参考做法、改不了就不用。
- **证据分级是沉淀的一部分**：涉及 Muse 本身或外部产品内部机制的说法，先逐项核对、分证据等级；`muse-task-mechanism.md` 开头即声明证据分级与“没有 Muse 源码”。
- **遗忘有正式流程**：`forget.plan`（一次调用，planner 只读、等 handoff、不并行搜索）→ 向用户展示范围与不可逆性 → 一次直接确认问题 → `forget.confirm`；沉默/含糊/部分批准/改范围 = 未确认；清理范围覆盖对话历史、笔记、偏好、目标、定时任务、已创建物与可能写回的工作。导出/重置：设置里可导出聊天记录与工作区文件；Reset 永久删除聊天记录、文件与活动任务，是唯一全量擦除；主聊天不可删除。
- **记忆导入**：设置里可从其他 AI 导入记忆（复制 prompt → 粘贴对方回答）或导入聊天 ZIP（≤100 MB）；只导事实偏好，不恢复原对话。
- **推理不可读**：`muse.db` 永远读不到模型的私有推理（thinking / redacted-thinking）与每轮保存的 developer 指令快照。
- **👍 是“收到了、正在做”的回执，只给大活儿**：`runtime.message_reactions` 194 行；用户 2026-11-15 亲口定规则：活儿大、花时间长的先点一个，小事直接做完回复就不点。`message_reactions` 主键就是 `message_id`，一消息只能存一个反应，再点是更新同一行。

### 未知

- 记忆写入的触发与去重细节：只知每小时整理、新 claim 覆盖旧 claim；不知何时写、写什么粒度、冲突解决、隐私分类标准。
- “纠正变检查”的自动路径：Muse 侧无实证。hermes 侧也无自动路径（最接近的是 curator 与 `/learn` 的人工触发）。这是 Morde 要新造的机制，不是抄来的。

## §9 失败与报错：怎么向用户呈现？有没有降级？

### 已证实（Muse 侧）

- **失败留在任务详情里，主对话不受污染**：failed actions 214 条全部保留；任务 finish_status `failed` 仅 14/442（3.2%），`waiting_for_user` 30/442（6.8%）。即失败首先是任务详情里的记录，其次才是需要用户介入的状态。
- **失败也照实记、不改写**：`ls` exit code 2 的失败项与后续成功构建并存。
- **限流是“每 connector 每分钟配额 + `connector_rate_limited` + `retry_after_seconds`”**；`terminal_for_attempt: true` 时当次 connector 工作立即终止，禁 sleep / 重试 / 委派 / 排替代任务。Google 系最紧（Calendar 60/min + burst 20，enforce；Gmail 6000 units/min；GitHub 50/min，shadow 只计数）。
- **后台只有 cron / hook / systemd 三种机制；所谓 watch 与事件触发实际都是轮询**，两次检查之间发生又反转的变化会漏掉，定时只约不准（前后数分钟）。任务运行卡在 durable 阻塞（connector 需重连、只有用户能做的动作、浏览器 bot 检查过不去、配对设备离线、浏览器正忙）时任务自己暂停，下一轮再试。
- **多租户/隐私边界**：每 Muse 属于一个用户；无多用户支持；他人不能登录别人的 Muse；agent 做的每件事都记在 Activity 与 approvals History，无法悄悄擦除。

### 已证实（Morde 侧，对照用，非 Muse 证据）

- Morde 的路由失败是**直接失败、无降级**：近 2 天 53 次路由决定里 11 次失败（21%），总计 73 次里 20 次失败；最多的是 `routing_context_unavailable`（9 次），其次 `message_too_long`（2 次，超 128 KiB 协议预算）；分类器病根是 deepseek-flash 回散文不回严格 JSON（2026-11-19：最近 8 次里 5 次 `invalid_output`），用户看到“请求暂时无法安全分类，请稍后重试。”。**这是 Morde 的反面教材，不是 Muse 的行为。** Muse 侧的对应降级策略无直接证据。

### 未知

- Muse 遇到工具失败/超时/上下文超限时，主对话里具体怎么措辞、有无自动重试后降级：无直接证据（`agent.compactions` 表在只读角色下为 0 行，不能据此断言“从不压缩”——只读角色有行级过滤，0 行只说明“在此角色、此视图下不可见”，不是“不存在”）。

## §10 上下文管理：Muse 每次带多少历史？

### 一句话结论

**Morde 每次对话不带全量历史。** 分类时只带最近 8 条消息（每条最多约 2 KiB 预览）+ 一份 ≤16 KiB 的摘要；任务执行时初始输入只有任务简报（objective ≤32 KiB），历史由 agent 需要时自己查。**Muse 侧带多少历史，未知**——只知 `agent.compactions` 与 `context_items` 表存在，不知窗口 token 数、触发条件、摘要模型、保留策略。hermes 是单机单会话，全量历史就是 context，每 turn 把整个 messages 列表重放给模型，全部机制围绕“怎么在不丢信息的前提下把历史压小”。

### Morde 路由分类链（每条消息必走的 7 步，已证实：代码 + 线上数据）

| 步骤 | 带什么 | 裁剪/上限 | 超了怎么办 |
|---|---|---|---|
| ① 快照加载 | 最近 8 条历史（SQL `ORDER BY sequence DESC LIMIT 8`），另加最近 4 条已发布任务结果（`LIMIT 4`）；另有候选任务列表、prior_requests（≤100）、聚合候选（≤8） | 每条 >4096 B 时 SQL 层直接截成头 1023 B + `…` + 尾 1023 B，`Truncated=true` | 快照读失败 → `routing_context_unavailable`，整条路由失败 |
| ② 单消息门 | 当前消息全文 | `MaxConversationAgentInputBytes = 128 KiB` | 超了 → `message_too_long`，明确失败，不降级 |
| ③ 摘要尝试 | 未覆盖历史（sources ≤256，输入 ≤96 KiB）→ 新摘要 ≤16 KiB，且必须比输入小 | 触发条件：未覆盖源字节 ≥72 KiB 或 token 压力 ≥ 预算 3/4；失败只记日志，继续走 | 摘要走 DSH coordinator 派生——Bug 3 没修好之前这一步永远跑不起来，线上三张摘要表全是 0 行 |
| ④ 密封摘要装入 | 旧摘要文本 + 去掉已被摘要覆盖的历史行；seq>9 且无摘要时 `HistoryIncomplete=true`，分类器会被明确告知“历史不完整” | 摘要打不开 → 摘要清零、继续走 | 降级，不失败 |
| ⑤ fit（2e86e33e 后的顺序） | 按 token 预算裁剪，token 估算 = DSH TokenMeter：4 个 UTF-16 字符 ≈ 1 token，CJK 加 25%；留 headroom `min(4096, max(256, 窗口/8))` + 800 运行时预留 + MaxOutputTokens | 窗口 = 模型配置值（deepseek-flash 1,048,576；未知则 4096 保底；硬上限 2,000,000） | 顺序：先裁候选任务 → 历史逐条缩成 2048 B 预览（最旧先缩）→ 历史逐条清空（只留 ID/序号/任务引用）→ 最后才丢摘要 → 仍装不下才 `message_too_long` |
| ⑥ 分类模型调用 | 拟合后的上下文，一次调用 | 输出 ≤4096 B 严格 JSON；无效输出修一次（repair 重试） | 修完仍无效 → bug 1 兜底：关键词 + 上次路由启发式，不再失败；模型本身报错（限流除外）同样走兜底 |
| ⑦ 聚合输入合并 | 短时间内多条同类消息合并成一次分类 | 合并预算 32 KiB | 超的补充不再让整条失败：拆下来单独路由，并在合并文本里注明“另有 N 条补充” |

直接回答（action=reply）也是同一链：分类模型那一次调用就写出回复，没有第二次调用；回复用的就是上面拟合后的上下文。

### Morde 任务执行：agent 拿到什么历史

- 任务初始输入 = 任务简报（objective ≤32 KiB）+ 系统指令 + 工具表（≤64 个），**不内联对话历史**。
- 历史按需查，3 个有界工具：`morde_current_conversation_history_search`（当前会话关键词搜 + 游标分页，`limit ≤10`，返回 `partial` + `next_cursor`——全代码库唯一的“分段读”先例）、`morde_conversation_history_search`（跨会话搜）、`morde_recent_conversation_summary`（近期对话片段摘要，需开启个人记忆）。
- 工具输出上限 64 KiB，结果超限直接报错，不自动分页。任务循环内没有压缩/预算管理：最多 16 轮 × 每轮 16 个工具调用，tool call / output 在循环内累积，每次模型调用都受 128 KiB 总预算，超了任务就失败。这是路由链之外第二条“上下文太多会失败”的路。
- 工作区读文件工具一次读整个文件，>48 KiB 拒绝，没有 offset/limit 参数——今天 Morde 里不存在“按范围读文件”的原语。

### hermes 上下文管理对照（hermes 补齐，非 Muse 实证）

- **两层阈值触发**：agent 内 `ContextCompressor` 50%（小窗口 <512K 取 75%，触发上界 85%，最低 64K token）；gateway hygiene 安全网 85%（`gateway/run_turn_hygiene.py`，hard_msg_limit=5000，turn-hold 上限 10s，失败冷却 300s DB 持久化）。token 数三级来源：API 上报 prompt_tokens → session 行 usage anchor → 粗估（偏高 30–50%，只会提前触发，是安全的）。
- **先剪枝、后总结**：Phase 1–2 确定性剪枝（无 LLM）：去重 byte-identical tool 结果 → 截断超大 tool 结果 → 截断非尾轮超大 tool_call 参数 → 退役图片（只留最新 3 个含图结果）→ 丢空白 echo；占位符 `[Old tool output cleared to save context space]`；剪出的原文归档可查；in-place 改写，不增删重排。
- **保护 head + tail**：保护头 3 条（首次压缩后衰减为 0，防早期 turn 化石），tail 按 token 预算（保底 20 条，至少 1 条 user 消息，summary_target_ratio=0.20，lean 模式 tail ≤ 窗口 20%），边界对齐完整工具调用组（bridge 防 role 碰撞）。
- **一次辅助 LLM 总结**：每 attempt 仅一次辅助调用；输入上限 160K 字符（约 40K token）；摘要 2000–10000 token，比例 0.20。关键：总结读的是原始工具结果（`summary_view` 把窗口内的 stub 还原回原文），每 attempt 一次辅助 LLM；提交时先存后裁，原文以 stub 归档到磁盘，模型仍可 `read_file` 打开。
- **失败冷却 + terminal 清单**：通用失败冷却 600s（DB 持久化），超时阶梯 (60, 300, 900)；5 类 terminal failure（认证/网络/截断/空内容/过载）必须 abort、原样保留会话、不 rotate；同 session 连续 3 次过载 abort 后降级为确定性 fallback；连续 2 次 provider 证实的无效压缩触发 breaker。anti-growth 护栏：压缩后 token 数反而涨了就拒绝采用（实测见过 427K→598K）。
- **轮首三段压缩**：每 turn 开始：idle 压缩（opt-in）→ preflight 压缩（每 turn 上限 3 次）→ 冷剪枝（仅 provider 缓存已过期时做，零缓存代价）。turn 收尾 `_micro_compact_after_turn()`；记忆/skill review 在交付后后台跑。
- **不建议抄的**：hermes 的“全量历史重放”（Morde 多租户 + 每消息都分类，重放成本和授权复杂度都扛不住）；它的 in-place session 压缩与 session id 不变（Morde 摘要是版本化密封文档，语义不同，各有好处）。

### Morde 可借鉴清单（抄什么 / 解决哪个问题 / 难度）

1. **确定性剪枝进任务循环**（抄 Phase 1–2）。解决：任务 16 轮循环内历史累积撞 128 KiB 导致任务失败。难度：中。
2. **压缩失败冷却 + terminal 清单 + fallback 摘要**（600s 冷却持久化；5 类直接 abort 不重试；3 次过载后给确定性 fallback 摘要）。解决：Bug 3 期间每条消息都重试一次注定失败的 DSH 派生。难度：低。
3. **溢出后“压完重试一次”**（provider 报 `context_length_exceeded` 时，先触发一次摘要压缩再重试分类，而不是直接 `message_too_long`）。难度：低（限一次，防无限循环）。
4. **轮首 preflight + turn 后 micro-compact**（任务每轮模型调用前按 token 压力复查；任务结束把本轮工具输出折进摘要输入）。难度：中（`hermes-compaction-window.mjs` 预算函数已在 runtime 里，只差调用点）。
5. **Tail 保护概念进快照**（最新 8 条之外，另保“当前任务相关的最近结果”与“最近一次用户明确引用的消息”）。难度：低。
6. **Early-result 语义补齐到分类失败路径**（bug 1 兜底已做：分类失败 → Clarify，不丢消息；再进一步：把 Clarify 时的原消息挂进下一 turn 的 history）。难度：低。
7. **Liveness 看门狗 600s**（需求文档已有，P0/P1 落地顺序里）。难度：中。
8. **错误分类表**（hermes 33 值枚举的思路：`context_overflow` 单独一类，只触发压缩，不走通用重试/换模型）。难度：中。

## §11 五条原则核对总表 + 数字总表

### 五条原则（Morde 2026-11-13 锁定）核对

| 原则 | 文档说法 | 实测 | 结论 |
|---|---|---|---|
| ① 主对话只出最终结果 | 文档无逐字表述。最接近的支撑：Activity 与 Approvals 独立入口；“后台 worker 做完后把报告以 hidden handoff 交给主 agent，由主 agent 决定用户看到什么” | 已证实：54 条工作项全在任务详情里，主对话只有“图正在画”和最后交付 | 文档间接支撑 + 实测证实。注意：原则本身是用户锁定的设计要求，不是 Muse 文档的原文 |
| ② 并发靠后台 agent，主 agent 只做简单对话 | 文档支撑强：Wonder 跑 subagents 与后台工作；后台 worker 独立做完再 handoff；coordinator + worker 扇出模式；schema 有 subagent 树与 monitor 决策表 | 已证实：主 agent 派发两个子 agent 后自己继续做 git 检查，时间戳重叠 | 文档与实测一致 |
| ③ 任务是记录单位，同话题多轮归一 | 文档无依据：goals、scheduling、memory 均未提到“同话题多轮合并成一个任务”或任何合并窗口 | 只证实一半：一条用户消息 = 一个 activity thread；多轮合并未证实（数据库侧：829 链接 / 352 任务，31% 的任务链 ≥2 条消息，但归并规则未知） | 落差点：原则③后半句在文档与实测中均无支撑。Morde 若要实现，需自行定义合并窗口与同话题判定 |
| ④ 每个任务记录完整过程 | 文档支撑：Activity 记录每件事；`runtime.events` 有序账本；`runtime.tool_calls` / `tool_outputs` 全记录；`agent.compactions` 保留历史区间 | 已证实：54 条，含派发、启动、完成、一次失败，无遗漏 | 文档与实测一致 |
| ⑤ 工具调用可钻取到代码/命令/结果 | 文档支撑为间接：tool_calls + tool_outputs + context_items 构成钻取的存储层 | 已证实：report 里有完整命令、退出码、耗时、工作目录、输出原文 | 文档提供存储层证据，实测证实展示层 |

补充：`muse-task-mechanism.md` §8 的两条“不要抄”——(a) 标题文案不固定（同一动作两次写法不同），Morde 标题宜用固定规则生成；(b) `runtime.work_items` 与 `agent_todo_snapshots` 是内部调度/计划表，UI 工作项的唯一事实来源是 activity actions。

### 数字总表（文档明确写出的硬规则 + 数据库实测）

| 条目 | 值 / 规则 | 来源 |
|---|---|---|
| Muse 上线 | 2026-09-08；可用地区 US / CA / JP / SG | 文档 |
| 模型分工 | 付费计划跑 Muse Spark + Muse Wonder；Wonder 跑主对话、artifact/app 构建、scheduled runs、subagents、后台工作；免费计划跑 Muse Spark；Spark 1.3（2026-09）；Wonder 发布 2026-10-12。本次会话运行在 Muse Spark 1.3 上，同样派生 subagent——文档未说明 Spark 下 subagent 的能力边界 | 文档 + 本会话 runtime |
| Wonder API 价 | 缓存输入 $0.20 / 百万 token，输入 $4.00 / 百万，输出 $20.00 / 百万 | 文档 |
| 主对话回复长度 | assistant 944 条，中位 142–160 字符（≤141 占 47.7%）；平均 312.7 字符，最大 3,656 | 数据库实测 |
| 事件可见性 | 5,478 条事件：internal 75.0%、public 25.0%；subagent 通道 1,030 条 100% internal | 数据库实测 |
| 任务量 | 442 个任务（2026-10-24 → 2026-11-22），全部 finished；success 398、waiting_for_user 30、failed 14；单日最多 63 个（2026-11-09） | 数据库实测 |
| 工作项 | 16,182 条 actions：成功 98.7%、失败 1.3%；主 agent 2,880、depth 1 有 7,289、depth 2 有 6,013 | 数据库实测 |
| 协调员决策 | 20,787 条：nothing_to_do 61%、completed 38%、notify_main_agent 1.2%、inference_error 3 | 数据库实测 |
| 提问率 | 回复以 `?`/`？` 结尾 6.8%；`waiting_for_user` 6.8%——两个独立口径一致 | 数据库实测 |
| Cron 定时 | 只约不准：预定时间前后数分钟、有时提前；轮询非连续；两次检查之间发生又反转的变化会漏掉；失败/超时的运行可能完全不发聊天消息 | 文档 |
| Hook | 每轮一个 terminal 决定（silent/wake）；新 hook 默认 disabled；无 connector 凭证；脚本不放 secret | 文档 |
| Proactive 上限 | 每次交付至多 8 条；每天至多 2 次普通交付、至多 3 次含时效 | 文档 |
| 记忆整理节律 | 记忆整理每小时；人物关系每小时；Idea 策划每日；学习简报每日夜间；Dreaming 每晚；技能评审每日 | 文档 |
| 文件链接 | 公开下载链接最长 48 小时 | 文档 |
| 订单复查 | 提交未确认：约 10 分钟后复查一次，仍无确认约 30 分钟后最后一次，之后停止、不重下单 | 文档 |
| 模型选择 | 只知 Wonder vs Spark 的计划分工；不知何时选哪个模型、多模型协作、Judge 换模型家族 | 未知 |
| 多租户 | 文档明确 Muse 是单用户单机；服务端多租户、租户隔离、配额分配在文档中不存在 | 文档明确 + 未知 |

---

# 第二部分：发现 Bug 与处理 Bug 的过程

## §12 两个真实调试案例（2026-11-22/23，Morde 服务端）

> 以下全是 Morde 服务端的真实调试，不是 Muse 自身的 bug。但它是本次能拿到的、最近的、每一步都有日志/DB/实测支撑的完整调试样本，提炼出的方法可直接作 Morde harness 的调试规范。

### 案例一：Bug 3——DSH 任务派生失败（根因两次反转）

| 阶段 | 时间 | 内容 | 证据 |
|---|---|---|---|
| 初始判断（错） | 2026-11-21/22 | 配置的 node 路径 `/usr/local/bin/node` 在服务器上不存在，`/opt/morde-cloud-runtime/bin.mjs` 也没装 | 记忆 2026-11-23 L7（记为“先前说法”） |
| 定位：查容器内 | 2026-11-22 | 专用 Docker engine 与 Guardian 服务 active，socket/key 存在；镜像 `morde-cloud-runtime:local` 存在，镜像内 `/usr/local/bin/node` 与 `/opt/morde-cloud-runtime/bin.mjs`（及 `bin-account.mjs`）都在 | 记忆 2026-11-23 L47 |
| 纠正 | 2026-11-22 | “node 路径和 bin.mjs 不是主机上的问题……都是容器内的路径，镜像里本来就有，模板配置没错，不用改”；`deploy/single/portal.yaml` 的模板值正确，不得“修复”成宿主机路径 | 记忆 2026-11-22 L152–155（`[correction\|high]`，带 `supersedes:`） |
| 验证根因：容器实测复现 | 2026-11-22 | supervisor 用 `env -i` 起容器并传入 `MORDE_WORKSPACE_DIR`，但 `bin.mjs` 环境白名单只有 PATH/LANG/LC_ALL/TZ/NODE_ENV，抛 “unclean process environment”，每次派生回 `runtime_unavailable`。对照实测：带该变量 → `{"type":"failed","code":"runtime_unavailable"}`；不带 → `{"type":"ready",...}`。邻文件 `bin-account.mjs` 白名单里早就有该变量 | 记忆 2026-11-23 L48 |
| 修复 | 2026-11-23 | `bin.mjs` 白名单加 `MORDE_WORKSPACE_DIR` 一个词 + 回归测试（带变量 → ready 帧；再加未知变量 → exit 1、无 ready、有 failed 帧）。`portal.yaml` 不动 | 记忆 2026-11-23 L87、L101 |
| 验证修复 | 2026-11-23 | 首次跑失败只因 worktree 缺 `node_modules`（`@deepseek-ai/cordis` 未找到）；`npm ci` + `npm run build:dsh-compaction` 后新测试 PASS（1 pass） | 记忆 2026-11-23 L101 |
| 防止复发 / 未闭环 | 2026-11-23 | 代码修完 ≠ 生效：服务器上还要重建 DSH 镜像、把新镜像摘要写进配置、重启才生效，尚未做；合并到 main 待用户点头 | 记忆 2026-11-23 L48 |

**关键方法点**：误诊的根源是把“容器内路径”当成了“宿主机路径”——定位时先分清执行环境（宿主机 vs 容器）再下结论；定案靠的是单变量对照实测（只改 `MORDE_WORKSPACE_DIR` 有无），不是日志推测；纠正写进了记忆并标注 `supersedes`，防止旧说法复活。

### 案例二：主会话“上下文太多”报错排查（2026-11-23 00:32）

用户原话（2026-11-22 晚）：“你需要把主会话修复一下，这是一个永远都不会因为上下文太多而报错的会话才是……是否没有好好处理没用的上下文，好好设计一下。运行日志可以去服务器上查看”。排查链条：

1. **发现（症状 + 数字）**：路由失败率高：20/73（总体）、11/53（近 2 天，~21%）；近 7 天路由结果：48 routed、9 `routing_context_unavailable`、6 `routing_action_unavailable`、4 `message_too_long`、3 `model_unavailable`、5 waiting_user。
2. **定位：排除模型窗口**：DeepSeek Flash 连接（p4x5pj84）`context_window_tokens = 1,048,576`，与代码 catalog 一致——窗口不是瓶颈。
3. **定位：找到真正的上限**：主会话 c3yyitcp 本身很小（28 条消息、43,930 字符），但 7 字符消息 m3n5xk3s（“你用英语回答我”，seq 22）失败于 `result_too_large` / `context_length_exceeded`。原因是合并逻辑把 earlier 大消息（seq 9：8,118；seq 20：8,117；seq 25：16,235；seq 26：8,623 字符，约 41 KB）全拉进来，并执行硬 32 KiB（32768 字节）上限，在快照加载阶段就失败——在已有的 fit 修剪有机会运行之前。**路由路径的字节上限（32 KiB 合并、128 KiB 单消息）远低于模型窗口，超了直接失败，不降级。**
4. **定位：4 条失败消息分类都没走到模型调用**：`conversation_route_model_calls` 0 行。
5. **定位：摘要系统从未跑过**：`conversation_summary_heads`、`conversation_summary_versions`、`conversation_compaction_operations` 三个表全是 0 行。压缩本身走 DSH 会话，而 DSH 派生正被 bug 3 堵死——“没用的上下文没处理好”的一半原因在此。
6. **区分有机故障与合成测试**：其余上下文失败是合成的长上下文回归测试，不是真实使用：mrvuiehu 单条 132,149 字符超 128 KiB 单消息协议预算 → `message_too_long`，无分块/压缩兜底；其 60 字符后续消息也经同一合并路径失败。
7. **修复（commit `2e86e33e`，dev 分支，5 文件，+206/−28）**：32 KiB 合并超限不再失败——合不下的补充在同一事务里剥离，改为独立路由，并在合并文本里注明 N 条补充因长度未合并；fit 顺序反转——先把历史缩成 2048 字节首尾预览（标 truncated/incomplete），再清空，摘要最后才丢（之前先丢摘要）；失败诊断开始记真实字节数（`content_bytes`、`history_bytes`）；128 KiB 单消息仍明确失败（流程图规定当前消息不得静默截断，要修需做分段读取，另议）。测试：13 个相关 worker 测试通过；`go vet` 通过；全量 `internal/worker` 仅 1 个失败且已证实是既存失败；新 store 集成测试因无测试 DB 被 SKIP，待 CI 跑。
8. **Bug 1（分类失败兜底）同期处理**：两个失败出口都要兜底——(1) `ClassifyConversationRoute` 返回 err；(2) 模型回散文/invalid JSON（gateway 回 `InvalidOutput=true` 且无 err，一次修复重排后第二次 invalid 才失败——这正是用户看到的“请求暂时无法安全分类，请稍后重试。”的出口）。兜底逻辑：显式控制意图优先 → Clarify；显式新任务信号 → New；明确延续 → Steer；否则 Clarify 问“继续上一个还是新开”；且永不把控制请求判停。仍过 `safeConversationRouteDecision` 校验；`CodeResultTooLarge` → `message_too_long` 等 fail-closed 路径保留。

## §13 提炼：可复用的 Bug 处理流程（发现→定位→验证根因→防止复发→收尾纪律）

| 步骤 | 做法 | 证据形式 |
|---|---|---|
| 发现 | 先拿数字定性：失败率、失败分类 Top N、用户原话贴出来 | DB 计数（20/73、11/53）、用户 quote |
| 定位 | 分层排除：先证伪最显眼的假设（模型窗口 1M token 不是瓶颈）；再沿代码路径找真正的上限（文件:行号）；查“本应工作但没跑的系统”（摘要三表 0 行）；查失败是否走到关键调用（`model_calls` 0 行） | DB 字段值、代码文件行号、0 行表 |
| 验证根因 | 单变量对照实测复现（bug 3：只改一个环境变量有无；上下文：小会话 + 大历史的组合）；区分有机故障与合成测试 | 实测输出原文（`runtime_unavailable` vs `ready`） |
| 防止复发 | 回归测试（新测试 PASS 才算修完）；失败诊断记真实尺寸，不再靠重读 DB 猜；流程图文档同步更新；误诊写进记忆并标 `supersedes`，防止旧说法复活 | 测试名 + 结果、commit 号、记忆纠正条目 |
| 收尾纪律 | 修完 ≠ 生效：写明“还要重建镜像 + 更新摘要 + 重启”才生效；修完 ≠ 合并：dev 分支先行，合并等用户点头；SKIP 的测试写明待 CI 跑，不谎称全过 | 记忆记录、commit 记录 |

另两条同款“认错文化”证据（见 §8）：生益价格纠正、“64a39aa 只改壳”纠正、日报名纠正。共同点：**错了就认、拿实查的数字说话、把纠正写进长期记忆**。

## §14 这套方法对应到 Morde 缺什么（复刻顺序有讲究）

| Muse 调试步骤 | Morde 现状 | 缺的机制 | 对应复刻项 |
|---|---|---|---|
| 日志 → 定位到步骤 | 只有 warn 日志 + 失败文案；分类失败、DSH 派生失败都只剩一句报错 | **可观测系统**：每步一个 span，失败带证据（模型原文、退出码、成本快照） | P1-2（先只接路由 span，1 天见效） |
| DB → 用数据证伪猜测 | 账本/事件/路由表存在，但无“失败自动进数据集”的机制；复盘靠人工翻表 | **评测循环 + 失败回流**：失败 trace 自动进评测数据集，FAIL 自动建修复任务 | P1-2/P1-3 |
| 代码 → 沿调用链读到行 | 有调用链，但无“独立验证者”——干活的模型自己说自己对了；验证 worker 存在但 Artifact 内容未接通、结论常 pending | **独立验证者**：换模型家族、verdict 锁版本 | P1-1 |
| 实测复现 → 复现成功才定根因 | 有回归测试传统（H00–H11 的本地回归很强），但真实入口验收（H06）待验证；60 分钟耐久未做 | **真实入口验收 + 耐久**：H06 冒烟十项 + LC-01–LC-06 | 随 P0 跑，不单独立项 |
| 纠正记规则表 → 下次不再犯 | 无“同一错误第二次出现必须沉淀”的机制；H07 未开始；postmortem 无 | **纠正变检查 + postmortem + playbook 广播**：复盘模板 + 教训进规则 | P1-7（先人工写回归用例）→ P2-3（模板 + 广播） |

**先上哪几个（顺序）：**

1. **最先：P1-2 的路由 span（1 天）。** 没有它，下一次分类失败还是只能猜；它是所有复盘的前提。Phoenix 采用需用户点头——不点头就先只做结构化失败日志（失败时把模型原文截断 2KB 写进事件 payload，不依赖 Phoenix）。
2. **其次：P1-7 的“纠正先写回归用例”（人工，S）。** 不等任何系统：以后每次纠正（包括本次映射里纠正的四处“已知状态”偏差）都先补一条回归用例再改代码。这是 postmortem 文化里最便宜的一步。
3. **再次：P1-1 的独立验证者家族校验（M）。** 没有它，P0 的 planner/审批卡/事件本体改完后，无法证明“改对了”——执行模型自己验自己等于没验。
4. **最后：P2-3 的复盘模板 + 广播（L）。** 等 P1-2/P1-3 跑出两周真实失败数据再做模板，否则模板是空架子。

**故障复盘模板（建议稿，P2-3 落库时用）：** 故障现象（一句话 + 失败原文）→ 影响范围（哪些用户/任务/时间段）→ 复现步骤（稳定复现命令 + 环境）→ 根因（调用链 + 具体行 + 数据证据，如“摘要表 0 行”）→ 纠正（改了哪几个文件 + 回归用例）→ **之前说错的话（明确写出被证伪的旧结论）** → 沉淀（lint/检查/skill/规则，至少一条）→ 待办（未解风险 + 下次复查时间）。其中“之前说错的话”一栏是 Muse 方法里最关键的一栏——没有它，复盘会变成“下次还犯”。

---

# 第三部分：Morde 映射与复刻方案

## §15 核实后的 Morde 状态总表（纠正“已知状态”里的偏差）

核实基线：工作区 HEAD `2e86e33e`（dev 分支顶，含 fit 顺序 + 32 KiB 合并拆分）。工作树当时有 6 个文件未提交改动（bug 1 路由兜底 + bug 3 bin.mjs 白名单正在做），下文凡写“工作树有、HEAD 无”均指这些未提交改动，不是已落地代码。证据标记：**[代码]** = 本次 grep/read 实查；**[文档]** = 设计/规格/TODO 文档原文；**[并行]** = `hermes-context-research.md` 的结论。

| 已知说法 | 核实结果 |
|---|---|
| planner（M9）在 Harness 里，但代码里还没有实现 | **偏差。** 不是“有位置没实现”，而是 Go 代码里 `planner`/`Planner` 零命中。Planner 只存在于文档。现有的只是执行 Agent 自报的 plan：工具 `morde_task_plan_update` + `task.Plan` 数据结构。**[代码]** |
| 合并窗口缺失 | **部分偏差。** 通用合并窗口（时间窗 + 同话题判断）确实没有；但“短时聚合”机制已存在且不小：`join` 动作、`aggregation_waiting`、`MaxConversationRouteAggregateMessages = 8`、合并预算 32 KiB、超预算拆补充单独路由（2e86e33e）。这是“等一等、合起来一次分类”，不是“同话题多轮归进同一任务”。**[代码]** |
| 事件链本体是自由文本 | **偏差。** 写侧已有固定白名单 `validTaskEventType`：`task.status / step / operation / message / batch_item / artifact / waiting_user / response / result / plan / priority` 共 11 个。但这 11 个是记录类型，不是目标 9 种语义本体（创建/拆分/派发/审批/执行/验证/交付/失败/恢复）；且读侧仍放行任意 `task.*` 前缀。结论：有白名单、无语义本体、无本体间迁移。**[代码]** |
| 审批卡本体只有对象/内容/影响 | **基本属实，需补两点。** `PendingRequest` 除 Object/Content/Impact 外还有 Title/Kind/ToolName/CallID/RequestHash/Version/ExpiresAt——有“动作身份”，无证据、成本、触发规则、拒绝后果。另 API 层已有披露对象（Risk / ThisCost / CumulativeCost / Alternatives），但 TODO 注明服务端费用常为 unknown、无通用金额限额字段，即披露层搭了架子、数据常空。**[代码][文档]** |
| bug 1、bug 3 正在做 | **调研时属实，但均未进 HEAD；2026-11-23 更新：已通过 PR #1 合并到 main（merge commit d7767d6；dev 提交 2e86e33e + 900bd6bc，共 9 个文件）。测试全绿：worker 全量通过、runtime 183/183 通过、gofmt/vet 干净。服务器尚未部署（仍跑 e941466）。** 2e86e33e 本身只含 fit 顺序 + 32 KiB 合并拆分。**[代码]** |
| 64 工具硬上限 / Skill 只读一个 | **属实，三处互相印证。** `agent_tools.go:362`、`task_execution_helpers.go:64`、`model/responses.go:35`（`maxTools = 64`）。Skill：仅 `nanomuse_skill_read`，固定内嵌 catalog。**[代码]** |
| DSH 是唯一执行层；Harness 是决策层 | **属实。** `AGENTS.md` 明文；`engine.mjs:1455` 挂 13 个插件（其中自写 3 个：persistence、MordeCompactionEngine、WorkerModel）。**[代码][文档]** |
| 任务状态机 | **部分有。** 现有 9 态：queued / running / waiting_user / pausing / paused / cancelling / completed / failed / cancelled。设计方案要求的 `verifying → delivered` 不存在；且规格明文：`Task.status=completed` 只表示执行器已持久化结果，不代表验收通过。**[代码][文档]** |

**三件“以为缺、其实部分有”的东西，复刻时不要重做：** (a) 事件类型不是自由文本，已有 11 个固定类型；(b) 429 限流重试已在 HEAD；(c) 独立验证不是零，`task_validation_review_worker.go` 已有 DSH 模型复核 worker，但 Artifact 内容复核未接通、结论常为 `pending`。
**两件“以为有、其实没有”的东西，是 P0 最大的坑（调研时状态；2026-11-23 更新：(a) 已由 PR #1 合并到 main 解决，剩余工作是部署验证）：** (a) bug 1 兜底与 bug 3 白名单在 HEAD 里都不存在，只在未提交工作树里；(b) 2e86e33e 的 fit 修复只解决了“历史太长不炸”，摘要压缩本身仍依赖 DSH 派生，Bug 3 未修好之前摘要表就是 0 行，历史只会越裁越狠。

## §16 逐项映射表（Muse 有 / Morde 已有 / Morde 缺失 / 可抄 hermes 补齐）

状态定义：**已有** = 等价机制在代码里且有测试/调用链；**部分有** = 机制存在但缺关键一块；**缺失** = 无对应代码。hermes 来源编号用需求文档 §2.6 的 19 步编号（H-1…H-19）；规格文档的 A/B/C 分级见 Harness 规格 §3（A=移植算法+边界测试，B=借行为重接现有链路，C=不适用）。

### 16.1 五条 Muse 原则对照

| # | 原则 | Morde 状态 | 补齐路径 |
|---|---|---|---|
| P-1 | 主对话只出最终结果，过程噪音收进任务详情 | **部分有**。路由 `reply` 动作直接回文本；任务过程进 `task_events` 与 `task.Step` 行。但 2026-11-21 才定稿“对话只出文字、任务在任务栏看”，PC 对话流内联任务卡/状态条在代码里是否已删干净本次未逐项核对 | 新设计：以 §3.7 为准做一次 UI/文案审计，不新增机制。P0-8 |
| P-2 | 并发靠后台 agent 匹配，主 agent 只做简单对话 | **部分有。** 任务级并发有（`MAX_CONCURRENT_TASKS = 16`）；但“匹配/派发决策”就是缺失的 Planner；子 agent 协作本地回归通过、真实入口未验 | 缺失部分 = Planner。hermes 对应 G1 入口三层 dispatch + H-12 phase 机，B 级。P0-3 |
| P-3 | 任务是记录单位，同话题多轮归成一个任务 | **部分有。** 一消息一任务的基础有；同话题合并只有“聚合”没有“合并窗口”；`steer`/`update`/`join` 都依赖分类器一次判对，无窗口、无相似度兜底 | 新设计为主 + hermes H-13 为辅。P0-4 |
| P-4 | 每个任务记录完整 agent 过程 | **部分有。** `task.Step` 行有 ID/TaskID/Sequence/Attempt/Kind/Status/Title/Summary/FailureReason/AgentSessionID/ToolName/ToolCallKey/DetailState；但 Muse report 要求的“完整命令/参数原文 + 退出码 + 耗时毫秒 + 工作目录 + 输出原文”在 Step 顶层字段里没有对应项（DetailState 为密封载荷，本次未逐项核对） | 新设计：定 Step Detail 的 report 固定结构（照抄 `muse-task-mechanism.md` §4）。P0-8 |
| P-5 | 每个工具调用可钻取到代码/命令与结果 | **部分有。** Broker `Execute`/`Reconcile` + operation ledger + checkpoint；`task.step` 有 ToolCallKey/RequestDigest 可追踪。但“点开看”的 UI 侧只做到 HTML 稿，未核对代码落地程度；大工具结果只留预览+编号取回尚未实现 | 新设计为主；hermes H-17 为辅。P0-8 |

### 16.2 入口、路由与合并

| 能力 | Morde 状态 | 补齐路径 |
|---|---|---|
| 持久收件 + 幂等 | **已有（等价，且比 hermes 更严）。** `AcceptConversationMessage`，`client_message_id` 幂等，同 ID 不同内容拒绝；会话内服务端 `sequence` 定序 | 不抄 hermes 的内存/下轮持久化。已满足 |
| 三层 dispatch：网关 / TurnRunner / facade | **部分有。** 三层事实存在，但无命名的 TurnRunner/facade 层，turn 身份无统一契约 | hermes H-7/H-9/H-10，B 级：立 turn setup 契约，不搬线程模型。P1 |
| Turn lease：TTL 300s、等 1800s、刷新 60s | **部分有。** 路由认领有租约；DSH Session 临界区有跨进程栅栏。但无 hermes 式 durable turn lease（三数），无统一租约注册表 | hermes H-7，全搬思想、数字按 Morde 实测定。P1 |
| Early result（拿不到租约也不丢消息） | **部分有（同思路、不同实现）。** `DeferConversationRoute`（2s 延期重排）+ 聚合拆补充单独路由；分类失败走 Clarify 不丢消息。但 hermes 语义“未被接纳的消息挂在 early result history 上、下一 turn 再持久化”没有等价实现 | hermes H-7 全搬。P1 |
| Liveness 看门狗 600s 无进展 abort、15s 采样 | **部分有（弱版已存在，阈值不同）。** H04 已有：`ModelCallTimeout = 45s` 单请求停滞提示、活动帧持久化。但无“600s 无进展 abort”——规格明文：600s 只作实验参考，不直接作统一强杀阈值 | hermes H-8，B 级：按模型/工具/子 Agent/checkpoint 分别定义活动检测条件。P1（不要直接抄 600s） |
| Turn setup 固定顺序 | **部分有。** 路由 worker 的顺序是：认领 → 快照 → 聚合判断 → 单消息门 → 解析模型 → 解析窗口 → 摘要压缩 → fit → 分类。但无“setup 顺序错了就报错”的契约 | hermes H-10 全搬。P1 |
| System prompt 三层 + tool pin | **未核对。** DSH `SystemPrompt` 插件已挂载，但三层拼装、stable 字节稳定、tool pin freeze 未逐项核对 | hermes H-11 全搬。P1；先核对再定工作量 |
| 分类器兜底链 | **已有（2026-11-23 更新：PR #1 已合入 main，d7767d6）。** HEAD 已有：一次无工具修复 + 429 限流重试（最多 3 次）。合入的启发式兜底：明确控制问清任务 / 明确新任务开新任务 / 明确补充跟随上一个任务 / 其余澄清，绝不猜测进任务；前序上下文无法确认时问用户补上下文。剩余工作：部署验证 | hermes G6 + 需求文档 §2.2(6)。**P0-1 已合入，剩部署验证。** |
| 错误分类 33 值枚举 + jittered backoff | **部分有。** 429 分支已按 hermes 思路实现；但 33 值全枚举没有，`context_length_exceeded` 与通用失败在 worker 里基本同路；`jittered_backoff` 未见移植 | hermes G6，A 级优先核对。P1 |
| 自动恢复梯子（5 周期 15/30/60/60/60s，仅 transient 且未输出文本时） | **缺失。** 路由层只有 429 恢复；DSH Task 层无此梯子；Task 空响应当前不自动重试 | hermes G6，B 级。P1 |
| 空响应预算 3 次 + 重复守卫 | **缺失。** 无空响应预算；无重复守卫 | hermes G6，A 级后置。P2 |
| Slash 命令分流 | **缺失（且不应抄）。** 需求文档定不搬；PC/iOS 用自然语言路由状态/停止/修改意图 | 不做 |

### 16.3 Planner、任务模型与多 Agent

| 能力 | Morde 状态 | 补齐路径 |
|---|---|---|
| Planner / Coordinator（M9）：只拆分派发，不写代码；产出 brief | **缺失。** Go 代码零命中；只有执行 Agent 自报的 plan。任务创建走路由直接建 Task，无拆分步骤 | **新设计（无 hermes 等价物可抄；Grok Bot 的 coordinator 只拆分派发是参照）。** P0-3 |
| brief 四件套 + 成本估算 | **缺失。** 无 brief 类型、无“无验收标准不派发”门 | 新设计。P0-3 |
| 合并窗口 + 同话题判断 | **缺失（聚合不等于合并窗口）。** 无时间窗口常量、无话题相似度判断 | 新设计为主。P0-4 |
| 任务状态机（created → running → waiting_approval → verifying → delivered） | **部分有。** 9 态齐全且有转移表，但无 `verifying`/`delivered`；`waiting_user` 既用于审批等待又用于澄清等待（语义混用） | 新设计：加两态 + 审批等待与澄清等待分状态。P0-6 同批 |
| 👍 反应 | **部分有（等价实现，形态不同）。** 无 `message_reactions` 表；等价物是路由决策的 `acknowledge` 布尔 + `agent_acknowledged` 列。“只给大活儿”规则已写进分类指令 | 新设计：补一条端到端验收；不建反应表。P1（小） |
| 工作项 = 一次工具调用；派发/启动/完成各记一条 | **部分有。** `task.Step` 行 + `task.step` 事件是载体；但派发子 agent 是否记 step、标题生成规则均未核对 | 新设计 + Muse 实测。P0-8 |
| 子 Agent：独立上下文/权限、只回最终结果、深度限 1、handoff 三件套、工作图 | **部分有。** DSH 持久子 Session 有；`dsh-subagent`/AgentTeam 插件未注册。深度限 1 未核对；handoff 三件套与共享工作图缺失。本地回归通过，真实入口未验 | hermes 无对应物；参照 Grok Bot 13 模块之“工作图 + 过夜交接”（模块 11）。P2 |
| 具名 bot、一 bot 一主责 | **缺失。** 无 bot 注册表、无岗位表 | 新设计（Grok Bot 参照，不抄编制数字）。P2 |

### 16.4 工具系统、门禁与并行

| 能力 | Morde 状态 | 补齐路径 |
|---|---|---|
| 工具注册表 + 每轮 top-k 检索（取消 64 硬上限） | **缺失。** 三处 64 硬上限；无注册表；无检索召回。MCP 仍是 `mcp_discover` 再 `mcp_call` 两步往返 | hermes G2 + 需求文档 §2.2(2)。先做注册表，top-k 第二步。P0-7 |
| persist-before-execute（先落盘再执行） | **已有（等价）。** Broker `execute` 先 `commitProgress` 写 `task.step` 再 `runtime.Prepare`；operation ledger + `Reconcile` 对账 | 已满足；补故障注入测试 |
| Pre-dispatch guard（四道） | **部分有（只有第 4 道的计数版 + 审批门）。** 缺失：PreToolUse 参数合法性拦截点、风险分级（读/写/外发/不可逆）无代码、防循环检测无代码 | hermes H-17 全搬流程、G2 借结构；风险分级借 OpenHands + Claude Agent SDK PreToolUse。P0-7（先风险分级 + 预算成本化，防循环随后） |
| 并行调度（上限 10、按序回填） | **未核对。** 服务端无显式调度器；DSH AgentLoop 内部工具是否并行未逐行核对 | hermes H-17 明确不用 hermes 的 8 workers，用 nanoMuse 方案。P1；先核对 DSH AgentLoop |
| CodeAct 第二条路 | **缺失。** 全仓库 `codeact` 零命中 | 新设计（OpenHands 参照；`dsh-code-runtime` 候选）。P2 |
| 单次工具调用超时 | **缺失。** 整任务 30 分钟一刀切预算；`dsh-tool-call-timeout-policy` 未注册 | DSH 插件候选 + H04 活动检测。P1 |
| 大工具结果只留预览+编号、按需取回 | **部分有。** `ToolResultPruner` 已挂载；工具输出上限 64 KiB；但取回路径未核对 | hermes 剪枝 Phase 1–2 + `dsh-spill-store` 候选。P1 |
| 沙箱单动作上限 | **部分有。** 云电脑容器隔离已有；单动作资源上限未核对 | 新设计。P2 |

### 16.5 审批与成本

| 能力 | Morde 状态 | 补齐路径 |
|---|---|---|
| 审批卡改证据卡：待批动作/证据/本次成本/累计成本/触发规则/拒绝后果 | **缺失（本体），部分有（披露层）。** 本体只有 Object/Content/Impact；披露层已搭但常 unknown | hermes G3 + G4。但规格把 `tools/approval.py` 定为 **C 级不搬**（不搬容器跳过、YOLO、模型代批）：抄卡面格式与成本四行，不抄 7 步顺序与 bypass；Morde 的 7 步要按自己的 Broker 顺序重写。P0-5 |
| 审批总门顺序 + hardline + 命令位置锚定 | **缺失。** 无 hardline 表、无命令位置锚定、无 7 步顺序 | 新设计。P0-5 |
| Smart guardian（temperature 0、max_tokens 16、三值返回，空回答=escalate） | **缺失。** 无辅助 LLM 裁决；模型评分不能批准应由人批的动作 | hermes G3。P1（审批本体先行，guardian 后加；守住“模型不能代批”底线） |
| 否认熔断器（同一 session 连续 3 次 DENY → 停手上报） | **缺失。** 无计数、无熔断文案 | hermes G3。P1（小） |
| 同意档位 once/session/always + “沉默不是同意”+ 超时 fail-closed 不计拒绝 | **部分有。** 持久审批概念存在；`PendingRequest` 有 ExpiresAt；无交互不把沉默当同意。但档位枚举、always 作用域、超时不计拒绝的计数规则均无代码 | hermes G3 借档位设计；作用域等用户拍板。P0-5（先落 always 默认值：仅写操作、不可逆逐次批） |
| 无人值守默认 deny（三上下文） | **缺失。** 无 single_query/cron/unattended 三上下文区分 | hermes G3。P1 |
| 成本门（唯一检查点在 RPC 发起前，check+book 同锁，超限 fail-closed） | **部分有（计数预算有，成本预算无）。** 工具调用计数预算 fail-closed 已有；模型/工具花费的成本门没有；`model_call_ledger` 表存在。hermes 的关键前提：其 `spend_limit` 只盖 MCP 工具、模型花费客户端无上限——Morde 必须把模型花费上限放服务端强制执行，不能照抄客户端自愿上限 | hermes G4，但 tally 按租户持久化。P0-6（先把计数预算扩展为成本预算的骨架） |
| 审批提示价格行（固定价/区间/累计/限额四行）+ 估算三路定价 + 4 位小数 | **缺失。** 披露层有字段但常 unknown；无四行格式、无三路估算 | hermes G4。P1（依赖成本门先有真实数字） |
| 用量持久化（入队不直写库，finalize 时 drain，按任务归因） | **部分有。** 表 + 预算预留/结算机制存在；入队/drain 语义与归因未逐项核对 | hermes G4。P1；先核对再定 |
| 402/credits 响应头 | **未核对** | hermes G4。P2（小） |

### 16.6 事件链、账本、交付与验证门禁

| 能力 | Morde 状态 | 补齐路径 |
|---|---|---|
| 事件本体 9 种，每条带理由/成本/证据指针 | **缺失（本体）；部分有（载体）。** 载体：`task_events` 表 + 11 类型白名单。缺：9 种语义类型、类型间映射、旧事件迁移、理由/成本/证据指针三字段的强制性 | 新设计。P0-6（与审批卡同批） |
| 账本：每次调用记账，本次价 + 任务累计实时可见 | **部分有。** `model_call_ledger` + 预算预留/结算 + Task 成本汇总 API 存在；但“实时可见”依赖成本数字真实，当前常 unknown | hermes G4。P1（先让数字真实，再谈实时） |
| 证据门禁 | **部分有。** 验证 worker + 完成 fence + Artifact 审核包绑定 Workspace 不可变 revision + 文件 SHA-256 均存在；但执行类操作缺可信环境身份、Artifact 内容未作为复核输入、结论常 `pending`；验证 Worker 与发布的时序“尚需验证接通” | hermes H-18，B 级：复用现有路径，先查时序再称门禁。P1 |
| 独立验证者（换模型家族，verdict 锁版本，交付者不能用自己的 verdict 放行自己） | **部分有（worker 存在，独立性未证实）。** `TaskValidationReviewWorker` 用 DSH 模型跑复核，有 fence；但“验证模型家族必须与执行模型不同”无强制代码；verdict 锁版本在 H05 进度里已有。Judge 模型 key 未配 | 新设计 + hermes H-18 的 stop-gate 钩子位。P1-1：先加“家族不同”硬校验 + verdict 锁版本验收 |
| 交付：先持久化最终回执，再发回原会话；重连不重复发布 | **已有（等价）。** 完成事务行锁重检 + 单次发布回归通过；`ResultReview` 与 `Task.status` 分开展示 | 已满足 |
| Turn receipt 绑定 | **缺失。** 需求文档已定搬，但代码里未见 receipt 绑定 | hermes H-19。P1 |
| 持久执行回放调试 | **部分有。** checkpoint + operation ledger + Step 行可支撑回放；但“回放任意一步”的入口未核对 | 新设计。P2 |
| 任务预算分段 | **缺失。** `MaxActiveSeconds` 限 1800 秒；60 分钟耐久验收的是“跨等待/恢复的长时间可用性”，不是单次连续 60 分钟 | 新设计。P2 |

### 16.7 记忆、Skill、钩子、场景包

| 能力 | Morde 状态 | 补齐路径 |
|---|---|---|
| Skill 可写六要素 | **缺失。** 只有只读 `nanomuse_skill_read` + 固定 catalog；无 `skill_manage create`、无 `write_approval` stage-then-replay、无 curator | hermes G5。P2（P1 只做“纠正变检查”） |
| 信任阶梯状态机 | **缺失。** 全仓库 `trust`/`routine`（除一条 GC 注释）零命中 | 新设计（Grok Bot 参照；13 模块之 8）。P2 |
| Routine 窄触发 + Test 真跑 | **缺失。** 无 routine 概念；goal schedule 工具是定时触发执行，不是 routine，不要混为一谈 | 新设计（Grok Bot 模块 9）。P2 |
| 钩子 | **缺失（代码）。** 服务端 Go 代码无 Hook 类型；customer-0 场景包的 5 个钩子只是文档 | 新设计。P0 只做最小闭环；通用钩子引擎 P2 |
| 三层记忆 | **部分有。** 会话记忆有；个人记忆有“近期对话片段摘要”；场景记忆随场景包走是文件形态，未核对代码落地 | hermes G5 的 MemoryStore + MemoryProvider ABC + 后台串行 `sync_all()`。P1 |
| 经验复用：纠正变 lint/检查/skill | **缺失。** TODO H07 未开始；候选需验证 + 获授权用户批准后才启用 | 新设计（Grok Bot 模块 10）。P1-7（先做最小形态） |

### 16.8 上下文与压缩、可观测与评测、流式

| 能力 | Morde 状态 | 补齐路径 |
|---|---|---|
| 路由 fit：先缩历史预览、最后才丢摘要；合并超 32 KiB 拆补充单独路由 | **已有（2e86e33e，HEAD）** | 已满足。回归已随 2e86e33e 落地 |
| 单条消息 >128 KiB 明确失败 | **已有（现状），但“永不失败”要求分段读** | §18 三阶段方案。P1 起步，不进 P0 |
| 摘要压缩 | **部分有（代码有，运行时跑不起来）。** `CompactConversationSummary` + 413 行维护代码存在；但走 DSH coordinator 派生，Bug 3 下永远失败；线上三张摘要表 0 行；失败只记一条 warn 日志、无冷却、无 breaker、无 fallback 摘要 | hermes G5：失败冷却 600s + terminal 五类必 abort + 3 次过载后确定性 fallback 摘要。先修 Bug 3，再加冷却。P0（Bug 3）+ P1（冷却） |
| 确定性剪枝进任务循环 | **缺失（任务循环内）。** `hermes-compaction-window.mjs` 已移植阈值公式和 lean tail 预算，但只差调用点 | hermes Phase 1–2。P1 |
| 溢出后“压完重试一次” | **缺失。** Provider 报 `context_length_exceeded` → `message_too_long` 直接失败 | hermes 溢出恢复。P1（低难度，限一次） |
| 轮首 preflight + turn 后 micro-compact | **缺失。** 只在路由分类前压一次 | hermes 轮首三段压缩。P1 |
| Tail 保护 | **部分有。** 快照固定取最新 8 条 + 最近 4 条已发布任务结果；无 head/tail 保护概念 | hermes 保护 head+tail。P1（低难度） |
| 错误分类表 | **缺失** | hermes 33 值枚举思路。P1 |
| 工作区读文件 offset/limit | **缺失。** 一次读整个文件，>48 KiB 拒绝，无 offset/limit 参数 | 新设计。P2 |
| Phoenix 自托管 + OTel/OpenInference | **缺失。** 全仓库 `opentelemetry`/`phoenix`/`otel.` 零命中；H08 未开始 | 新设计。P1-2：先只接路由分类一个 span（约 1 天，验收：下次分类失败 10 分钟内定位到模型原文） |
| 成本对账 | **缺失。** 账本表有，对账机制无 | 新设计。P1 |
| 评测：Inspect AI + 原生套件，44 条用例，Judge 换模型家族 | **部分有（套件骨架有，用例与 Judge 接线无）。** `server/internal/evaluation/` 有 `suite.go` + `blind.go` + `testdata/suite-v1.json`（synthetic，仅 8 条用例）。44 条 live 用例不在仓库；Inspect AI 接线未见；Judge 换家族未见 | 新设计。P1-2：先把 44 条写成 live 套件（与 synthetic 分开跑、分开算分），再接 Judge |
| 失败回流 | **缺失。** 无自动建任务、无“第二次出现”计数 | 新设计。P1 |
| 盲测 panel | **缺失。** `blind.go` 存在，但 panel 常态化无 | 新设计。P2 |
| 流式输出（单写者 fence + think/context scrubber + 首 delta 去前导换行 + reasoning 单独通道） | **未核对。** 本次未逐行核对流式实现 | hermes G6。P2；先核对再定 |
| 流式中断保留已展示文本并标明中断，不静默重播 | **已有（等价）。** 规格 §2.3：已展示的流式文本保留并标明中断 | 已满足 |

## §17 复刻方案 P0 / P1 / P2

工作量：S ≤2 天，M 3–5 天，L 1–2 周（含测试，不含等用户拍板的时间）。**复刻顺序：P0 先把“分类不报错 + 任务能拆 + 卡能拍板 + 事件能回放”四件事闭环；P1 再上验证者与评测（否则 P0 改完无法证明没改坏）；P2 再做沉淀。不要把 P2 的机制提前塞进 P0**——Harness TODO 的取舍节（“暂缓：拆整套 phase 循环、强制 top-k、全量事件迁移、Skill 自动启用”）与此一致。

### P0 — 闭环最缺的（不做完不进 P1）

**2026-11-23 更新：P0-1、P0-2 已通过 PR #1 合入 main（d7767d6），测试全绿；剩余工作是部署验证（服务器仍跑 e941466；bug 3 需重建 DSH 镜像 + 更新 portal.yaml 摘要 + 重启 + 生产形态探针）。**

| # | 做什么 | 改哪几个文件（具体路径） | 验收标准（可观测、可测试） | 工作量 | 前置依赖 |
|---|---|---|---|---|---|
| P0-1 | **Bug 1 落地：分类兜底链合入。✅ 2026-11-23 已合入 main（PR #1，d7767d6），测试全绿。** 剩余工作：部署验证 + 补“兜底命中率”观测（加一个 `fallback` 维度） | `server/internal/worker/conversation_route_worker.go`、`server/internal/worker/conversation_route_worker_test.go`（工作树已有改动，合入 + 补回归：prose 回复、模型调用失败、invalid_output 三路各一例） | CLS-01 通过：分类器收到 prose 不再报“无法安全分类”；最近 20 次路由决策 `invalid_output` 为 0；兜底三路各有回归用例 | S | 无 |
| P0-2 | **Bug 3 落地 + 部署验证。✅ 2026-11-23 代码已合入 main（PR #1，d7767d6）。** 剩余工作：部署验证（重建 DSH 镜像 → 更新 portal.yaml 摘要 → 重启 portal → 生产形态 probe），验证 DSH 派生回 `ready`、摘要表从 0 涨起来 | `runtime/morde-cloud-runtime/bin.mjs`、`runtime/morde-cloud-runtime/test/runtime.test.mjs`（工作树已有）；部署走正常 update 流程 | 真机验证：DSH 派生成功；`conversation_summary_versions` 7 天内出现首行 | S | 用户点头部署；云电脑主机可用（hdqokxzv 已建） |
| P0-3 | **Planner 最小方案 + brief 四件套。** 不建独立服务：在 Harness 路由后加一个 `Planner`（新文件，先只做“拆分”一种能力）：输入 = 路由 `new` 的任务目标；输出 = brief（目标/范围/验收标准/验证方法 + 成本估算）；无验收标准的 brief 不派发（直接 Clarify 问用户要验收标准）。执行 Agent 自报的 `morde_task_plan_update` 降级为“参考”，不算派发依据 | 新建 `server/internal/app/conversation_planner.go`；改 `server/internal/app/conversation_route_decision.go`（路由 `new` 后调 Planner）；改 `server/internal/worker/cloud_dsh_coordinator.go`（按 brief 派发，拒绝无 brief 派发）；`server/internal/domain/task/types.go`（加 `Brief` 结构）；测试新建 `conversation_planner_test.go` | 一句话开发任务：planner 产出 brief 四件套齐全；无验收标准的输入走 Clarify 而不是直接派发；brief 里的验收标准与最终验证输入一致；3 个不同复杂度任务各有回归 | M | P0-1、P0-2 |
| P0-4 | **合并窗口（最小版）。** 在聚合机制上加两样：(a) 时间窗口常量（先定 60s 可配，P0 实测再调）；(b) 同话题判断：窗口内新消息先与进行中任务的 brief 目标做一次轻量判定（关键词重叠 + 分类器 `join`/`steer` 信号），命中则追加到同一任务，不命中则新任务。窗口外的消息不再合并 | `server/internal/app/conversation_route_decision.go`；`server/internal/store/postgres/conversation_route_actions.go`；`server/internal/worker/conversation_route_worker.go`；测试补窗口内/窗口外/同话题/不同话题四例 | 窗口内同话题补充只产生一个任务、一次最终答案；窗口外补充开新任务；不同话题不合并；聚合等待超 32 KiB 仍拆补充单独路由（2e86e33e 回归不破） | M | P0-1、P0-3 |
| P0-5 | **审批卡证据化（本体）。** `PendingRequest` 加字段：证据指针（`EvidenceRefs`）、本次成本（`ThisCostMicros` + 未知态）、累计成本（`CumulativeCostMicros` + 未知态）、触发规则（`TriggerRule`）、拒绝后果（`RejectConsequence`）；API 披露层把已有的 Risk/ThisCost/CumulativeCost/Alternatives 从“常 unknown”改成“有数给数、无数明确标 unknown 且不伪造为 0”。卡面文案：待批动作、证据、本次成本、累计成本、触发规则、拒绝后果同一张卡。`always` 档位默认值：仅写操作可记，不可逆永远逐次批（等用户拍板再放宽） | `server/internal/domain/task/types.go`（PendingRequest 加字段）；`server/internal/store/postgres/task_approvals.go` + 新 migration（列 + 回填 unknown）；`server/internal/app/task_approvals.go`（写入/校验）；`server/internal/api/tasks.go`（披露层接真实成本）；PC `TaskDetail.tsx` + iOS 审批视图（只改卡面字段，不改流程） | 审批卡面六项齐全；成本数字与 `model_call_ledger` 对得上（有数时）；无数时明确显示 unknown，不显示 0；不可逆动作每次都出卡（回归）；APR-01～03 通过 | M | 成本数字真实化依赖 P1-5 的成本门骨架——P0 先做到“字段 + unknown 诚实”，数字真实化放 P1，不要为 P0 伪造数字 |
| P0-6 | **事件本体 9 种落地（最小版）。** 新建本体枚举 + 旧 11 类型到 9 种的映射表；新事件按 9 种写，旧事件只读映射不重写（不做全量迁移）；每条新事件强制带理由、成本（可 unknown）、证据指针三字段，缺一不落库 | `server/internal/domain/task/types.go`（本体枚举）；`server/internal/app/task_service.go`（`validTaskEventType` 改为 9 种 + 旧类型映射）；新 migration（本体列/约束）；`server/internal/store/postgres/admin_quality_activity.go`（统计查询按新本体）；测试补 9 种各一例 + 旧类型映射回归 | 9 种事件各至少一条真实记录；旧 11 类型查询仍可用（映射）；缺理由/证据指针的事件写不进库；任务详情可按 9 种回放全过程 | M | P0-3、P0-5 |
| P0-7 | **工具注册表（第一步）+ 门禁四道之二。** 注册表：新表/结构存全部工具的描述、权限、风险级、成本档、失败怎么办（四项必填，写不清不进）；`validateAgentTools` 的 64 硬上限改为“注册表内工具数不限，每轮给模型 top-k”（top-k 检索本身放 P1，先做注册表 + 全量透传开关，默认仍全量，避免 P0 行为大变）。门禁：先落风险分级（读/写/外发/不可逆四档，进注册表）+ 预算检查成本化骨架（复用已有计数预算，加成本预留字段）；PreToolUse 参数合法性拦截与防循环检测放 P1 | 新建 `server/internal/app/cloud_dsh_tool_registry.go`；改 `server/internal/app/agent_tools.go`（去 64 硬上限，改走注册表）；改 `server/internal/app/cloud_dsh_tool_broker.go`（风险分级接入 `AdmitToolCall`）；新 migration（注册表）；`server/internal/model/responses.go`（`maxTools = 64` 改为注册表上限/取消）；测试：超 64 工具任务不报错、风险分级四档各一例 | 工具总数超 64 任务不报错；每轮给模型的工具数可观测（先全量，后 top-k）；高风险/不可逆动作必出审批卡（回归）；写不清四项的新工具进不了注册表 | L | P0-5、P0-3 |
| P0-8 | **任务记录形态对齐（P-1/P-4/P-5 的最小闭环）。** 定 Step Detail 的 report 固定结构（执行位置 + 完整命令/参数原文 + 退出码 + 耗时毫秒 + 工作目录 + 输出原文截断注明行数 + 一句结论），写入 `task.Step.DetailState` 的 schema 约定（先约定、后补写）；主对话只出文字的 UI 审计一次（按设计方案 §3.7，PC/iOS 对话流里残留的内联任务卡/状态条/任务入口行清掉） | 新建 `server/internal/app/task_step_report.go`；改 `server/internal/app/cloud_dsh_tool_broker.go`（写 step 时按结构落 Detail）；PC/iOS 对话流审计（只删残留，不加功能） | 任一工具调用点开详情，七要素齐全；失败 step 保留不删不改写；主对话无内联任务卡/状态条（截图验收 20 项里相关项通过） | M | P0-6、P0-7 |

### P1 — 验证闭环（证明 P0 没改坏）

| # | 做什么 | 改哪几个文件 | 验收标准 | 工作量 | 前置依赖 |
|---|---|---|---|---|---|
| P1-1 | **独立验证者（最小可用）。** 在现有 `TaskValidationReviewWorker` 上加两条硬规则：(a) 验证模型家族必须与执行模型不同家族（加校验，相同家族拒绝复核并记 `pending`）；(b) verdict 锁版本（已有 revision/event_seq/SHA-256 栅栏，补“版本一变 verdict 作废重验”的回归）。Artifact 内容复核仍缺——P1 先做到“文本/证据类复核独立”，内容复核放 P2 | `server/internal/worker/task_validation_review_worker.go`（家族校验）；`server/internal/app/cloud_dsh_task_validation*.go`（模型解析）；测试补家族相同拒绝 + 版本变 verdict 作废两例 | 交付物都有非作者 verdict；验证模型与执行模型同家族时复核拒绝、不放行；版本变了旧 verdict 不复用；FAIL + 理由返工 | M | P0-3、P0-6；Judge key 待用户定，未配前用已有非 DeepSeek 模型顶着并注明可信度打折 |
| P1-2 | **可观测第 1 步 + 评测 live 套件。** (a) 本机 OTel Collector + Phoenix，只接路由分类一个 span（`morde.harness.route`，失败附模型原文截断 2KB 内）；(b) 44 条 live 用例写成 `morde-ai-conversation-live-v1.1.0` 套件（与 synthetic 分开跑、分开算分），先跑 P0 smoke 10 条 | 新建 `server/internal/observability/`（OTel 接线 + span 清单）；`server/internal/evaluation/testdata/suite-live-v1.json`（新建，44 条）；`server/internal/evaluation/`（live 套件校验）；部署加 Phoenix/Collector（本机） | 下次分类失败 10 分钟内定位到模型原文；P0 smoke 10 条每次部署后全过；失败 trace 自动进评测数据集（先手动进，自动进放 P1-3） | M | P0-1；Phoenix 采用待用户点头 |
| P1-3 | **评测 Judge 接线 + 失败回流。** Inspect AI 当 Judge 运行器；Judge 换模型家族（执行 DeepSeek、主 Judge Claude、仲裁 GPT）；温度 0；P0 双 epoch；FAIL 自动建修复任务（附 transcript + 失败 criterion + 证据）；同一错误第二次出现沉淀为 lint/检查/skill（先做计数 + 建任务，自动沉淀放 P2） | `server/internal/evaluation/`（Judge 接线 + 运行器）；新建 cron/定时任务（每周日 02:00 全量）；失败回流接任务创建 | 连续两周全量报告；一条 P1 失败从建任务到定点重跑通过全程有记录；P0 100% PASS；满分后人抽查 20% | L | P1-2；Judge key |
| P1-4 | **上下文加固。** (a) 摘要失败冷却 600s 持久化 + terminal 五类必 abort + 3 次过载后确定性 fallback 摘要；(b) 溢出后“压完重试一次”（限一次）；(c) Tail 保护进快照（SQL 加一路引用锚点查询）；(d) 错误分类表（`context_overflow` 单独一类） | `server/internal/worker/conversation_summary_maintenance.go`；`server/internal/worker/conversation_route_worker.go`；`server/internal/store/postgres/conversation_route_actions.go`；`server/internal/app/conversation_route_decision.go` | Bug 3 修好后摘要表从 0 涨起；摘要失败不再每条消息重试一次；`context_length_exceeded` 先压缩重试一次再失败；长会话问 20 条前细节走历史搜索而不是瞎答 | M | P0-2；否则 (a) 的冷却是“给注定失败的派生降速”，仍值得先做 |
| P1-5 | **成本门 + 用量持久化 + 审批 guardian。** (a) 成本门：per-tool 成本门（唯一检查点在 RPC 发起前，check+book 同锁，超限 fail-closed），tally 按租户持久化；模型花费上限放服务端强制；(b) 审批提示四行成本格式；(c) Smart guardian（temperature 0、max_tokens 16、三值返回，空回答=escalate）+ 否认熔断器（连续 3 次 DENY 停手） | `server/internal/app/cloud_dsh_tool_broker.go`；`server/internal/store/postgres/`（ledger 租户维 migration）；`server/internal/api/tasks.go`；新建 `server/internal/app/cloud_dsh_approval_guardian.go` | 超预算任务自动停并说明已花多少；卡面成本数字与账单一致；空回答绝不等于批准；连续 3 次否认后停手 | L | P0-5、P0-7 |
| P1-6 | **门禁补齐 + 工具轮收尾。** (a) PreToolUse 参数合法性拦截点；(b) 防循环检测（近 30 条比对，警告→严重→熔断，熔断出卡说明）；(c) 单次工具调用超时（先接 `dsh-tool-call-timeout-policy` 候选评估）；(d) 并行调度上限 10 按序回填（先核对 DSH AgentLoop）；(e) 确定性剪枝进任务循环 + 轮首 preflight + turn 后 micro-compact（接已有 `hermes-compaction-window.mjs`） | `server/internal/app/cloud_dsh_tool_broker.go`；`runtime/morde-cloud-runtime/src/engine.mjs`；`runtime/morde-cloud-runtime/src/hermes-compaction-window.mjs`；测试各补回归 | 同一工具重复调用触发警告直至熔断，任务停下并出卡说明；单次工具调用超时回 `TOOL_TIMEOUT` 不拖垮整批；长任务第 10 轮后不再因 tool 输出累积撞 128 KiB 失败 | L | P0-7；(e) 依赖 P0-2 |
| P1-7 | **纠正变检查（最小形态）+ 记忆后台同步。** (a) 一条被纠正过的错误先进回归用例（人工先写用例，自动沉淀放 P2）；(b) 记忆 `sync_all()` 后台串行，不阻塞对话 | `server/internal/evaluation/testdata/`（回归用例追加）；`server/internal/app/cloud_dsh_memory*.go`（新建或扩展记忆同步，后台串行） | 被纠正过的错误在回归用例里不再出现；记忆同步不阻塞对话 | S–M | P1-3 |
| P1-8 | **Turn 契约补齐（H-7/H-10/H-11/H-13/H-19）。** turn setup 顺序契约、durable turn lease（三数按 Morde 实测定）、early result 语义、迭代准备 8 项清单、system prompt 三层 + tool pin、turn receipt 绑定 | `server/internal/worker/cloud_dsh_coordinator.go` + `server/internal/app/conversation_route_*.go`；`runtime/morde-cloud-runtime/src/engine.mjs` | setup 顺序错了就报错；同一 session 同一时刻只一个 turn；拿不到租约返回 early result、消息挂下一 turn；stop-gate 钩子位挂独立验证者（P1-1） | L | P0-3、P1-1 |

### P2 — 沉淀与规模（P1 跑稳两周后再启动）

| # | 做什么 | 改哪几个文件 | 验收标准 | 工作量 | 前置依赖 |
|---|---|---|---|---|---|
| P2-1 | **Skill 可写六要素 + `write_approval` stage-then-replay + curator。** Agent 可自写 skill，走审批 stage 到 `pending/` 待批；curator 周期（7 天/2h/14 天/30 天）；永不删除、归档有上限。评估 DSH `dsh-skill` 注册表作 provider（不得暴露本地文件系统 provider、不得绕过授权） | 新建 `server/internal/app/cloud_dsh_skill_write.go`；改 `server/internal/app/cloud_dsh_skills.go`；`runtime/morde-cloud-runtime/src/engine.mjs`（DSH skill 包接线评估） | agent 自写 skill 有审批、有归档、不丢；未批的 skill 不生效；curator 周期回归 | L | P1-1、P1-7 |
| P2-2 | **信任阶梯状态机 + routine 窄触发 + Test 真跑。** 阶梯五态（观察→纠正→写成 skill→挂 routine→自动合并）落库；routine 窄触发条件 + 每次真跑 Test；自动合并最后才开 | 新建 `server/internal/app/cloud_dsh_trust_ladder.go` + `cloud_dsh_routine.go`；migration（阶梯状态 + routine 定义） | 没走完阶梯的 skill 不能挂 routine；routine 每次触发都有 Test 真跑记录；自动合并只在阶梯走完后可用 | L | P2-1 |
| P2-3 | **Postmortem + playbook 广播 + 钩子引擎。** 失败复盘模板（见 §14）落库；教训写进规则全场景生效；钩子 when/who/do/ask 落盘 + 触发引擎（高风险 fail-closed，审批走 Morde 卡） | 新建 `server/internal/app/cloud_dsh_postmortem.go` + `cloud_dsh_hooks.go`；场景包文件格式已定（scene.yaml + playbook.md + rules.md + memory/ + hooks/ + assets/） | 一次失败复盘产出一条规则并全场景生效；钩子触发后主动跳出处理，高风险必过卡 | L | P1-3 |
| P2-4 | **评测常态化 + 盲测 panel + CodeAct + 工作图/handoff。** 月度盲测 panel（抽 20%，换 Judge 家族重跑 + 人工仲裁）；CodeAct 第二条路（`dsh-code-runtime` 候选）；handoff 三件套 + 共享工作图；具名 bot 岗位化 | `server/internal/evaluation/`（panel）；`runtime/morde-cloud-runtime/src/engine.mjs`（CodeAct 接线）；新建工作图/handoff 相关 app 文件 | 月度 panel 有记录；CodeAct 批量操作可回放；多 Agent 任务的工作图与实际执行记录一致 | L | P1-3、P2-1 |
| P2-5 | **规模：多租户适配层收尾 + 压测。** tally/限额/审批隔离按租户维进库；上万人并发压测与租户隔离审计；场景包复制给第二个人实测；单条消息分段读 P1/P2（分页读 + 并行分片 + 文件范围读） | 压测脚本（新建）；`server/internal/app/cloud_dsh_workspace_tools.go`（offset/limit）；`server/internal/store/postgres/`（chunk 表 migration，长消息分片） | 并发压测达目标且租户间零串数据；场景包在另一个人账号里装上即跑通一个钩子；1 MiB 单条消息全程不失败、答案引用页码、每页 sha256 校验通过 | L | 全部 P1；planner 落地 |

## §18 “用户输入永远不失败”的设计（分两层）

“失败”定义为：用户发出的内容丢失、或得不到任何回应（包括 Clarify 提问也算回应；静默丢弃不算）。

### 18a 历史太长：后续短消息不能因旧历史体积失败

已解决的（2e86e33e，dev 分支，未部署）：合并超 32 KiB 拆补充单独路由；fit 先缩历史预览、最后才丢摘要；单条消息不参与合并预算。
还差的：
1. **摘要压缩本身跑不起来**（Bug 3）。fit 现在只能“丢”不能“压”：历史越长，分类器看到的越少，最终只剩当前消息 + 摘要（如果曾经有）或裸奔。这是当前最大的洞。修 Bug 3 + 部署后，验证 `conversation_summary_versions` 从 0 涨起来才算闭环。
2. **失败无冷却**：Bug 3 修好前，每条消息路由时都会尝试一次注定失败的 DSH 派生，拖慢每条消息。先加冷却，哪怕只是内存里的 10 分钟。
3. **任务循环内无压缩**：长任务做到第 10 轮之后，tool 输出累积撞 128 KiB，任务失败且已做的工作只剩 checkpoint。先做确定性剪枝（不动模型，只动工具结果），再做 turn 后 micro-compact。
4. **摘要覆盖缝隙**：seq>9 且无摘要时分类器只知道“历史不完整”，不知道缺了什么。已有的缓解是路由指令要求“需要旧细节时用任务搜原文”，这条链是通的，但依赖分类器主动判断。验收时要测：长会话里问一个 20 条之前的细节，分类器是否走到 new-task + 历史搜索，而不是瞎答。

### 18b 单条消息超 128 KiB：分段读取方案

现状：worker 128 KiB 门硬失败 `message_too_long`，流程图规定“当前消息不得静默截断；剩余内容仍不能安全装入时明确失败或澄清”。所以不是简单放宽上限，而是：消息全文必须可达、模型按页读、每页可校验。

设计（核心是不丢字节 + 每页可验证）：
- **入库即分片**：消息写入时按 32 KiB 切 chunk，存 chunk 表（`message_id, chunk_index, byte_start, byte_end, sha256`）。32 KiB 选值理由：工具输出上限 64 KiB，一页 32 KiB 进工具结果留一半余量给包装字段；且与摘要输入 96 KiB（3 页）对齐。
- **路由分类只用“头 + 元数据”**：第 0 页的头 2 KiB 预览 + 元数据（总字节、总页数、内容类型、sha256 列表摘要）。分类问题从“这条消息要做什么”变成“这条长消息要做什么”；意图不明时 Clarify 问用户，不猜。
- **任务执行时按页读**：新工具 `morde_long_message_read_chunk(message_id, chunk_index)`，复用 `morde_current_conversation_history_search` 的游标模式（cursor 带 message_id + offset + sha，返回 `next_cursor / partial / total_chunks`）。Agent 读完所有页才能作答，或明确说“只读了前 N 页、还剩 M 页”。
- **防丢机制**：每页带 sha256，agent 汇总前逐页校验；答案必须能引用页码（“第 3 页提到……”）；汇总用 map-reduce：每页先提要点（要点 ≤2 KiB），再综合——避免把 1 MiB 全文一次性塞进 128 KiB 预算。
- **多条输入（“不管多少个输入”）**：聚合已有 32 KiB 合并 + 拆补充单独路由（2e86e33e），消息本身不会丢；但 100 条连发会产生 100 次路由/多个任务——这是产品语义（要不要合并成一个任务），不是失败问题，需另定合并策略，不在本方案内。

分阶段落地：

| 阶段 | 做什么 | 改哪几个文件 | 工作量 | 风险 |
|---|---|---|---|---|
| **P0：优雅降级，不丢消息** | ① worker 的 128 KiB 门改走新分支：消息入库分片（migration 加 chunk 表）+ 分类用头预览 + 元数据；② 意图不明 Clarify；③ 任务创建时把长消息挂为“长消息引用”（chunk 元数据进任务 objective 附件位），agent 先读 head 再决定 | `server/internal/worker/conversation_route_worker.go`、`server/internal/store/postgres/`（chunk 表 migration + 写入/读取）、`server/internal/app/conversation_route_*.go`（Context 加 LongMessageMeta） | 1–2 天 | migration 回滚；分类器第一次见“长消息元数据”可能误判 → 用 Clarify 兜底，不猜 |
| **P1：分页读 + 顺序 map-reduce** | ① 新工具 `morde_long_message_read_chunk`（Go 端，复用 history search 游标模式）；② 任务说明加“长消息先读 head、按需分页、读完再答”；③ 每页 sha 校验 + 要点汇总 | `server/internal/app/cloud_dsh_history_tools.go`、`server/internal/store/postgres/`（chunk 按页读）、任务执行 prompt（说明文字） | 3–5 天 | agent 可能读不完就作答 → 工具描述写死“必须读完或声明只读前 N 页”；页内容 32 KiB < 工具输出 64 KiB 上限，安全 |
| **P2：并行分片 + 文件范围读** | ① workspace 读工具加 `offset/limit`（48 KiB 上限改按页）；② 长消息也可落盘为 workspace 文件复用同一工具；③ >1 MiB 走并行：planner 拆 N 个读页子任务各出要点，汇总任务合成答案；④ 合成 1 MiB 消息回归测试进 CI | `server/internal/app/cloud_dsh_workspace_tools.go`、planner（M9，依赖项）、回归测试 | 1–2 周 | 依赖 planner 落地；并行成本上升（每页一次模型调用）→ 需预算门；页间去重（跨页重复内容） |

验收标准（P1 完成时）：1 MiB 纯文本单条消息全程不失败（有分类、有任务、有答案）；答案覆盖性：抽查 3 个分布在不同页的锚点细节，答案能答对并引用页码；每页 sha256 校验通过；任一页损坏/缺失时明确报错，不静默跳过；耗时上限：页数 × 单次模型调用 + 汇总一次；超时则降级为“已读前 N 页 + 告诉用户还剩多少”，不失败。

---

# 第四部分：未知清单

## §19 Muse 侧未知（推断不出来的部分，不要当结论）

| # | 未知环节 | 为什么未知 | 对 Morde 的影响 |
|---|---|---|---|
| U1 | 意图分类算法 | 文档只给 `runtime.requests.root_work_class` 字段名，无分类器描述、无类别表、无 fallback 规则 | Morde 的“分类兜底”无 Muse 对照可抄 |
| U2 | 任务拆分算法与合并窗口 | 无 planner 描述；无“同话题”判定规则；无合并窗口时长；`runtime.work_items` 只有 parent/root 链，无拆分依据字段 | 原则③后半句无法从 Muse 复制，须自研 |
| U3 | 模型选择 / 路由逻辑 | 只知 Wonder vs Spark 的计划分工；不知何时选哪个模型、多模型协作、Judge 换模型家族等 | Morde 的验证者模型选择无对照 |
| U4 | Skill / 工具检索机制 | 无 top-k、无打分、无工具上限数字；只知“读 skill 再调用”与“只有文档列出的命令才存在” | Morde 的 64 工具硬上限改 top-k 无 Muse 数字可抄 |
| U5 | Subagent 深度 / 扇出上限 | `agent.agents.depth` 字段存在，但文档无最大深度、最大并行 worker 数、单轮工具调用上限 | 并发上限须自定（wide-research 只给“先跑一项再放量”的启发式） |
| U6 | 重试 / 退避策略细节 | 只有零散规则（GWS 已连接重试一次、catalog 参数错纠正重试一次、Slack 禁止自动重试、rate-limit terminal 停），无统一重试梯子、无退避时长、无最大重试次数 | Morde 的错误重试梯子须自研 |
| U7 | 上下文窗口大小与 compaction 触发阈值 | 只知 `agent.compactions` 与 `context_items` 存在，不知窗口 token 数、触发条件、摘要模型、保留策略 | Morde 的“主会话永不因上下文报错”无 Muse 数字可抄 |
| U8 | 审批卡内容与决策机制 | 卡对 agent 不可见；Sentinel approval store 不可查；不知卡上字段、超时行为、默认选项 | Morde 的审批证据卡字段须自研 |
| U9 | 成本记账 | 文档无任何按次计价、任务累计成本、预算上限的描述；DB 里有 `runtime.checkout_spend_*` 与 `stripe_link_spend_requests`（只覆盖支付，不覆盖模型调用成本） | Morde 的成本账本（Hermes 式）无 Muse 对照 |
| U10 | Agent 单轮超时 / 预算 | 无 turn 超时、无 token 预算、无“无进展 abort”（如 600s 看门狗）的描述 | Morde 的 liveness 看门狗须自研 |
| U11 | Handoff 队列的优先级与排序 | 只知 `agent.message_mailbox` 与 hidden handoff 机制，不知排序规则、去重规则、积压处理 | Morde 的事件链排序须自研 |
| U12 | Subagent 监控决策表 | `subagent_monitor_decisions` 有 coordinator 决策记录，但决策的触发条件、决策选项（继续/叫停/重试/升级）未在文档中列出 | Morde 的 coordinator 监控须自研 |
| U13 | 记忆写入的触发与去重细节 | 只知每小时整理、新 claim 覆盖旧 claim；不知何时写、写什么粒度、冲突解决、隐私分类标准 | Morde 的记忆沉淀须自研 |
| U14 | 工具调用的沙箱 / 隔离 | 文档无执行沙箱描述（仅 custom connector 提“fully-isolated worker”）；不知工具调用是否隔离、如何隔离 | Morde 的沙箱设计无 Muse 对照 |
| U15 | Feed / Ideas / Proactive 的内容选择算法 | 只知来源与节律（taste summary、bandit 表表明有 bandit 学习），不知排序权重、去重阈值 | 与 Morde 主线无关，列出备查 |
| U16 | 多租户形态 | 文档明确 Muse 是单用户单机（每 Muse 属于一个用户、无多用户支持）；服务端多租户、租户隔离、配额分配在文档中不存在 | Morde 的多租户层须完全自研，这是 Muse 文档帮不上的一块 |
| U17 | 标题/副标题生成方式；任务列表按天分组的客户端逻辑 | 标题现场生成但机制不明；`activity_thread_kind` 全空 | Morde 标题用固定规则生成，不学随意的写法 |
| U18 | Muse 遇到失败时主对话的具体措辞与降级策略；`tool_calls` 0 行的原因 | 无直接证据；0 行可能是行级过滤，不能断言不存在 | 不要拿 0 行当“没有工具记录”的结论 |
| U19 | 提问的触发阈值（什么算“阻塞”、什么算“需拍板”） | 无证据；Morde 设计稿的“最多问 3 个关键问题”是 Morde 的设计，不是 Muse 的证据 | 须自研 |
| U20 | “你是为了回答我而回答我”这句纠正 | 记忆里查不到原文，三组搜索 + 全量 grep 均无命中 | 不要当成设计依据 |

## §20 hermes 侧未核实（本次未读到的）

- `agent/turn_api_request.py::build_api_request` 的 MoA prepared request 细节（MoA 不搬，不影响）。
- `persistence/conversation/` 的具体表名（只用了契约层语义）。
- `gateway/run_turn.py` 三层 dispatch 的逐函数细节（只用了 facade/lease 层）。
- `agent/turn_scripted_prelude.py` 的 prelude 脚本具体内容（只确认了“第一模型调用前先打 scripted tool calls”这一机制点）。
- `agent/lsp/` 的 deferred verdict 结算细节（只确认了 stop-gate 入口与每 turn 上限 3 条）。
- hermes 侧数字均来自 2026-11-21 的研究（commit `0d49d6d9`），本次补读核实了 16 个关键文件，但未重新拉取最新代码全量核对。

## §21 hermes 补齐标注表（每一处用 hermes 补的，都在这里单列）

**阅读规则：下表每一行都是“hermes 有、Muse 实证没有、补到 Morde 某一项”。hermes 的东西只能写成“hermes 做法，可作补齐参考”，绝不能写成“Muse 就是这样”。**

| # | 补齐点 | hermes 来源（固定提交 0d49d6d9） | 补到 Morde 哪一项 | 状态 |
|---|---|---|---|---|
| H-1 | Turn lease：TTL 300s、等 1800s、刷新 60s；拿不到返回 early result，未被接纳的消息挂下一 turn | `agent/turn_facade_lease.py::admit_durable_turn_lease` | P1-8（turn 契约） | 需求文档已定全搬 |
| H-2 | Liveness 看门狗：600s 无进展 abort、15s 采样，abort 前重校验防误杀 | `agent/turn_liveness.py::TurnLivenessWatchdog` | P1（看门狗，阈值按 Morde 实测定，不直接抄 600s） | 需求文档 P0/P1 |
| H-3 | 迭代准备 8 项清单（drain redirect、checkpoint dedup 重置、中断检查、预算消费、step_callback、换 key、steer 插行、预算警告注入） | `agent/turn_iteration_prep.py::prepare_iteration` / `begin_iteration` | P1-8 | 需求文档已定 |
| H-4 | Setup 固定顺序：恢复 session → 绑 turn 身份 → stage 消息 → 拼 prompt → `_ensure_db_session()`（必须在 prompt 之后，否则缓存 miss，Issue #45499）→ turn-start 压缩 | `agent/turn_context.py::build_turn_context` | P1-8 | 需求文档已定全搬 |
| H-5 | 压缩两层阈值 + 选窗（先剪枝后总结、保护 head+tail、bridge 对齐工具调用组、总结只读原始工具结果、原文 stub 归档可读回） | `agent/context_compressor.py`、`agent/context_compressor_window.py`、`gateway/run_turn_hygiene.py` | P1-4、P1-6(e) | 部分已移植阈值公式到 `hermes-compaction-window.mjs`，只差调用点 |
| H-6 | 失败冷却 600s + terminal 五类必 abort + 3 次过载后确定性 fallback + 2 次无效压缩触发 breaker + anti-growth 护栏 | `agent/context_compressor.py` | P1-4(a) | 待做 |
| H-7 | 错误 33 值枚举 + jittered backoff（Retry-After 优先）+ 耗尽后自动恢复梯子（5 周期 15/30/60/60/60s，仅 transient 且未输出文本时） | `agent/error_classifier.py`、`agent/retry_utils.py`、`agent/turn_recovery_autorecover.py` | P1（错误分类表、恢复梯子） | 429 分支已按此思路实现，其余待做 |
| H-8 | 空响应预算 3 次（单次 ≥$0.25 降为 1 次）+ 重复守卫（fragment ≥400 字符、窗口 60、≥5 次、占比 0.5） | `agent/empty_response_guard.py`、`agent/tool_guardrails.py` | P2（有真实重复反例再翻译，先旁路记录） | 待做 |
| H-9 | 审批 7 步 + smart guardian（temperature 0、max_tokens 16、三值返回，空回答=escalate）+ 否认熔断器（连续 3 次 DENY）+ 同意档位 once/session/always | `tools/approval.py`、`tools/approval_smart.py` | P0-5（只抄卡面格式与成本四行，不抄 7 步顺序与 bypass）、P1-5(c) | 规格定为 C 级不搬（7 步顺序与 bypass） |
| H-10 | 成本门（唯一检查点在 RPC 发起前，check+book 同锁，超限 fail-closed；spend_limit 只盖 MCP 工具；模型花费客户端无上限，真限额在服务端 402；MCP tally 内存 dict、永不淘汰、不持久化） | `tools/mcp_tool_costs.py`、`tools/approval.py::_trust_gate_check` | P0-6（成本预算骨架）、P1-5（tally 按租户持久化，模型上限放服务端强制） | 待做 |
| H-11 | Stop-gate 顺序 LSP → verify-on-stop → pre_verify → kanban；verify-on-stop 默认 OFF、上限 2 次、查 sqlite 证据账本 | `agent/turn_stop_gates.py::apply_stop_gates`、`agent/verification_stop.py` | P1-1（独立验证者挂 stop-gate 钩子位）、P1-8 | 注意：hermes 没有独立验证者 agent，挂验证者是新造 |
| H-12 | 并行 8 workers + 批 deadline 420s + 读写分段（写者重叠关并行段；不足 2 个调用降级串行） | `agent/tool_executor.py` | **不用**：Morde 用 nanoMuse 方案（上限 10、按序回填），需求文档已定 | 已定不搬 |
| H-13 | 委派：max_concurrent_children 默认 10、max_spawn_depth 1、child_timeout 默认 0（无上限） | `tools/delegate_tool_config.py:19`、`hermes_cli/config_defaults.py` | P2（多 Agent 协作参考数字，不直接抄） | 参考 |
| H-14 | Skill 自写（curator 7天/2h/14天/30天，永不删除）+ `/learn`（对话变 skill）+ 交付后后台 review（约 30K token/事件） | `agent/curator.py`、`agent/skill_commands.py`、`agent/turn_finalizer.py::_mirror_turn_to_memory` | P2-1、P2-3 | 待做 |
| H-15 | Turn receipt 绑定（客户端凭 receipt 把流式回复绑定到已存储行，而非比对文本） | `agent/persisted_turn_receipt.py` | P1-8 | 需求文档已定搬 |
| H-16 | 流式输出（单写者 fence + think/context scrubber + 首 delta 去前导换行 + reasoning 单独通道） | `agent/stream_delivery.py` | P2（先核对 Morde 流式再定） | 待核对 |
| H-17 | 溢出恢复（`context_overflow` → 压缩，不 failover；413 按 payload 字节计压缩进度；采纳 provider 上下文上限；实在压不动则 `fail_turn(compression_exhausted=True)`，transcript 保持完整） | `agent/turn_overflow.py` | P1-4(b)（压完重试一次） | 待做 |
| H-18 | **hermes 没有的三件**：core 无 routine（只有社区桌面插件）、运行时无 postmortem（只是离线取证工具）、无“纠正变检查”自动路径 | 核实结论（§9 第 7 项） | P2-2、P2-3、P1-7 均为**新造**，不要写成“抄 hermes” | 明确标注 |

## §22 来源索引

**Muse 行为证据（底稿 part-a）：**
- `~/MEMORY.md` 全文（60 行）：五条原则、bug 3 纠正、上下文排查摘要 + 修复 commit
- `~/dreams/alignment/derived/ALIGNMENT_SYNTHESIS.md` 全文：2026-11-09 快照三结论、验收四项、20 项截图验收未跑、递文件规则、认错文化实例、默认 Agent 形态
- `~/workspace/goals/morde-positioning-living-document/files/muse-task-mechanism.md` 全文（211 行）：四层任务机制、54 条实测、report 结构、action_id 命名、0 行表注意事项
- `~/memory/2026-11-09/12/13/15/21/22/23.md` 相关段落：选项卡、T05 审批卡、主 Agent 六步回答、五钩子、五条原则锁定、👍 规则、“看不到方案”事件、路由失败率、bug 3 纠正、上下文排查、修复 commit 2e86e33e、bug 1 兜底设计
- `muse.db` 只读查询 26 条（数据范围 2026-10-19 → 2026-11-22）：见 part-a 附录 B
- 表结构文档：`/opt/muse/skills/muse_db/references/schema.md` 及 `tables/` 下相关页

**Muse 产品文档（底稿 part-b）：**
- `~/docs/` 全部文档（muse.md、skills、connectors、browser、client-surfaces、scheduling-and-watching、self_improvement、privacy-and-credentials、artifacts、files-and-library、memory、goals、feed、ideas、calls-texts-notifications、chat-connections 等）
- `/opt/muse/skills/` 58 个 skill 的 SKILL.md；`/opt/hatch/connectors/` 143 个 connector 的 CONNECTOR.md 与 manifest.yaml

**hermes-agent 源码（底稿 part-c，固定提交 0d49d6d9）：**
- 本地 clone：`~/workspace/research_notes/hermes-agent-conversation-flow-20261121/repo/`（`git log` 核实 HEAD 确为 `0d49d6d9`）
- 750 行全流程报告：`~/workspace/research_notes/hermes-agent-conversation-flow-20261121/report.md`
- 补读的 16 个关键文件：turn_facade_lease、turn_liveness、conversation_loop、turn_context、turn_iteration_prep、turn_finalizer、context_compressor、context_compressor_window、turn_preflight_gate、error_classifier、turn_api_call、turn_response_check、turn_recovery_autorecover、turn_stop_gates、verification_stop、stream_delivery 等（完整清单见 part-c 附录）

**Morde 文档与代码（底稿 part-d，只读核实，HEAD 2e86e33e）：**
- 设计方案 v1.0：`~/workspace/goals/morde-positioning-living-document/files/morde-design-proposal/morde-design-proposal.md`
- 目标文档：`~/workspace/goals/morde-positioning-living-document/files/morde-target-design/morde-target-design.md`
- hermes 抄什么：`~/workspace/goals/morde-positioning-living-document/files/morde-hermes-copy-requirements.md`（§2.2 六组、§2.4 多租户四点、§2.6 19 步定稿、§5 P0→P1→P2）
- Harness 总流程图：`~/workspace/morde-dev/docs/Morde_Harness_最终总流程图.md`
- Harness 实现规格：`~/workspace/morde-dev/docs/Morde_Harness_最终流程与实现规格.md`
- Harness TODO：`~/workspace/morde-dev/docs/Morde_Harness_实施任务_TODO.md`（H00–H11）
- 上下文研究（并行成果，直接引用）：`~/workspace/hermes-context-research.md`
- 代码（只读 grep/read）：`server/internal/worker/conversation_route_worker.go`、`server/internal/app/conversation_route_decision.go`、`server/internal/worker/cloud_dsh_compaction.go`、`server/internal/worker/conversation_summary_maintenance.go`、`server/internal/app/cloud_dsh_tool_broker.go`、`server/internal/app/agent_tools.go`、`server/internal/app/task_execution_helpers.go`、`server/internal/model/responses.go`、`server/internal/app/cloud_dsh_skills.go`、`server/internal/app/cloud_dsh_task_plan.go`、`server/internal/domain/task/types.go`、`server/internal/app/task_service.go`、`server/internal/worker/task_validation_review_worker.go`、`server/internal/evaluation/suite.go` + `testdata/suite-v1.json`、`server/internal/store/postgres/conversation_route_actions.go`、`runtime/morde-cloud-runtime/src/engine.mjs`、`runtime/morde-cloud-runtime/src/hermes-compaction-window.mjs`、`runtime/morde-cloud-runtime/bin.mjs` / `bin-account.mjs`

---

*文档信息：2026-11-23 由 4 个并行子 agent + 协调员整合完成。结论先行、每条标证据等级；推断与源码事实严格区分；hermes 补齐的部分单独标注。后续每次落地 P0/P1/P2 里的某一项，应回填一条“已实现 / 验收结果”到 §16 对应行，保持本文与代码一致。*
