# Morde 复刻 Muse 总计划

> 版本：总计划 v1.0 · 2026-11-24
> 用户指令：从用户体验、工具与连接器、Harness、整体方向四个维度复刻 Muse，基于全部已有材料与数据做逆向工程，把功能与逻辑移植到 Morde。
> 基线：服务器 998f13d0（2026-11-23 21:37 部署，healthz ok）；main 998f13d0；DB 229；对话摘要版本数仍为 0。

## 一句话目标

把 Muse 的 harness（1 个 root agent + 树形 subagent + 有序事件账本 + 卡片制审批 + Markdown 与数据库双轨记忆）复刻成 Morde 的 Harness + DSH + Broker，让用户在 Morde 里得到与 Muse 同样的对话体验：主对话只见最终结果，任务可拆可派可回放，失败留痕不丢消息。

验收总标准（对话什么时候算复刻完了）：路由 CLS-01 通过；最近 20 次路由决策 `invalid_output` 为 0；P0 smoke 10 条每次部署后全过，周全量 44 条 P0 100% PASS；审批卡面有本次成本与累计成本，数字与账单一致；一次失败对话能在可观测系统里 10 分钟内定位到具体步骤与证据；同一错误第二次出现时，已有对应的 lint / 检查 / skill，而不是第二次犯。

## 四个维度

### 维度一：用户体验——操作习惯与功能，尽量往 Muse 靠

复刻点（每条都有 Muse 实测数据支撑，见逆向工程文档）：

- 五条锁死的原则就是 UX 验收标准：主对话只出最终结果（Muse assistant 回复中位 142–160 字符，75% 事件用户看不到）；并发靠后台 agent 匹配，第一个任务不挡第二个；任务是记录单位；每个任务记录完整过程；每个工具调用可钻取到代码、命令、结果。
- 对话只返回文字：任务在任务栏里看，对话流里不放内联任务卡、状态条、任务入口行（2026-11-21 已定；P0-8 做一次 UI 审计清残留）。
- 只有三页：任务列表、任务详情、工作项详情，不放用量计数（2026-11-21 已定）。
- 审批是卡片制：卡面六项（待批动作、证据、本次成本、累计成本、触发规则、拒绝后果）齐全；成本有数给数，无数明确标 unknown，不伪造为 0。
- 工作项点开是固定七要素 report：在哪执行 + 完整命令 / 参数原文 + 退出码 + 耗时（毫秒）+ 工作目录 + 输出原文（长输出截断并注明截了多少行）+ 一句人话结论；失败的 step 保留不删不改写。
- 提问只在阻塞或需拍板时出现（Muse 两个独立口径都是 6.8%），载体用选项卡；递文件要点贴在消息里，不只给链接。
- 👍 是“我收到了、正在做”的回执，只给大活儿。

对应施工：muse_steps TODO 01（收件与会话）、07（交付与提问）；复刻设计 §2、§4.7；P0-8。

### 维度二：工具与连接器——能移植的机制都移植，不照搬数量

现状对照：

- Muse 侧：文档里列出的 skill 约 58 个、connector 约 143 个（其中成文的 connector 文档 83 份）。关键不是数量，是机制：skill 按任务路由（`skill_search`，SKILL.md 写明触发条件）、connector 按动作配作用域权限、凭证绝不经聊天（只进 Secure Vault，一次性码只走 `[credential:<uuid>]` 保护引用）。
- Morde 侧：64 个工具硬上限（`maxCloudDSHToolDefinitions = 64`，runtime 侧 `validTools` 同样 cap 64）；Skill 只有一个只读工具 `nanomuse_skill_read`；没有工具注册表。

移植顺序（机制先行，连接器按需补，不 1:1 照抄 143 个）：

1. P0-7：工具注册表（描述、权限、风险级、成本档、失败怎么办四项必填）+ 64 硬上限改为“注册表内工具数不限，每轮给模型 top-k”；门禁先落风险分级 + 预算检查骨架。
2. P1-6：门禁补齐（PreToolUse 参数拦截、防循环检测近 30 条比对、单次调用超时、并行上限 10 按序回填）。
3. P2-1：Skill 可写六要素 + `write_approval` stage-then-replay + curator 周期（7 天 / 2h / 14 天 / 30 天，永不删除）。
4. DSH 插件（已核完，按 P0 能补缺口的先接）：tool-gate / tool-surface、skill 注册表、单次调用沙箱、CodeAct、todo-write、超时、官方防循环 guard、cost-meter；P1 再接 bash / fs / workflow / subagent / agent-teams / otel / spill-store / approval / session-query。插件解决不了独立验证者和 eval harness、多租户 seam 得自己写。
5. 连接器只移植机制（作用域权限、Secure Vault、凭证保护引用、购买每次新批），具体接哪个 connector 按 Morde 用户真实需要排，不按 Muse 的清单照抄。

开源对照（每步都有可参考的实现，见 `muse-steps-open-source-mapping.md`）：收件入口 → OpenClaw；意图路由 / 任务拆分 → Microsoft Agent Framework Magentic（双账本）；并发派发 / 监视 → LangGraph（Supervisor + Send）；工具执行工作项 → OpenHands（事件溯源账本）；审批 → Codex CLI（sandbox × approval 双轴）；验证 → OpenHands + Grok Bot 方法论；交付 → LibreChat（Artifacts）；记忆 → OpenClaw（三层 Markdown）；失败 / 限流 → Hermes-agent；上下文 → Hermes-agent；Bug 发现处理 → 无直接对照，自研。

### 维度三：Harness——AI 对话怎么处理的逻辑与机制，逐项复刻

Muse 的对话处理（逆向工程已证实的部分）：

- 收件：每会话一个 chat UUID；任务行五字段（标题、副标题、状态、完成状态、起止时间）；442 个任务全部 finished，成功 398、等用户 30、失败 14（3.2%）。
- 路由与拆分：一条消息不严格等于一个任务，31% 的任务链 ≥2 条消息（平均 2.36 条），但归并规则未知（U2，最大缺口）；Skill 路由有明确机制；coordinator + worker 是文档级推荐模式。
- 并发：主 agent 只做简单对话，复杂工作派后台 agent，时间戳重叠证明不是排队；子 agent 嵌套到 2 层（230 次派生里 depth=2 有 71 次，占 31%）；协调员每事件决策一次（20,787 条，61% 是什么都不做，只有 1.2% 需要通知主 agent）。
- 工具执行：工作项 = 一次工具调用；16,182 条 actions，成功 98.7%；子 agent 的工作项是主 agent 的 4.6 倍。
- 验证：写后读回是统一模式；独立检查 worker 拿 draft 标无支撑断言；wide-research 必须报 verified coverage。
- 上下文：Muse 侧窗口与触发条件未知（U7），只知压缩表存在；Morde 自己的 7 步链已证实（快照最近 8 条 + 密封摘要 ≤16 KiB + fit 裁剪 + 128 KiB 单消息门）。
- 失败：失败留在任务详情里，主对话不受污染；限流是每 connector 每分钟配额 + `terminal_for_attempt` 当次终止。

Morde 的 Harness 落点（muse_steps TODO 00–11 为施工 spine，DSH 是唯一执行层，Planner 在 Harness 不在 DSH）：

| 步骤 | Morde 现状（2026-11-24 核） |
|---|---|
| 01 收件与会话 | 已有，`client_message_id` 幂等 + 服务端定序；双端联调待验证 |
| 02 意图路由与任务拆分 | 分类 7 步链 + 兜底已落地（PR #1）；Planner 缺失；合并窗口缺失 |
| 03 并发派发与协调 | 两层委派本地回归通过；PC 实测未派发 worker，端到端待复验 |
| 04 工具执行与工作项 | Step 行有载体；report 七要素未落 schema；大输出回读待验 |
| 05 审批 | 卡面仍只有对象 / 内容 / 影响三字段；完整 E2E 待做 |
| 06 验证 | 复核 worker 有但结论常 pending；PC 验收未通过 |
| 07 交付与提问 | 回复与交付机制有；交付真实性及双端展示待验证 |
| 08 记忆与纠正沉淀 | 接口有；闭环待核验；纠正变检查未开始 |
| 09 失败、限流与恢复 | 429 重试有；自动恢复梯子缺失；错误分类表缺失 |
| 10 上下文管理 | 摘要代码有但从未跑成（版本数 0）；任务循环内无压缩 |
| 11 Bug 发现与处理 | 五步法已沉淀（两次真实调试）；复盘模板落库在 P2 |

hermes 补齐（用户已定完全照搬对话处理，19 步搬 13 步）：分类失败兜底、错误重试梯子、early result（P0）；循环骨架（P1）；沉淀和拦截（P2）；并行用 nanoMuse 方案（上限 10、按序回填）；多租户四点随各组同步做。独立验证者、routine、信任阶梯、postmortem 是新造，不要写成“抄 hermes”或“抄 Muse”。

### 维度四：整体方向——按复刻 Muse 的方向走

- 定位（v1.5，已定稿未推仓库）：Muse 的对话体验 + Grok Bot 的多 Agent 机器，装在能复制给另一个人的云电脑上；不抄编制数字、不抄三个不一致的自动合并口径、不抄每用户一台云电脑的部署形态。
- 终态（目标文档）：Muse 对话体验 + Grok Bot 多 Agent 机器 + Hermes 成本账 + OpenClaw 式 Markdown 记忆，云电脑可复制给另一个人，服务端撑上万人并发。
- 实施 spine：muse_steps（用户 2026-11-23 定的方向，docs/muse_docs/muse_steps/，TODO 00–11），取代已删除的 Harness flow 文档。
- 三条硬边界：DSH 是唯一执行层，不换不重写；Planner 在 Harness，不在 DSH；多租户服务端形态，任务、事件、账本落库。
- 不复刻的东西：Muse 的单用户单机形态；Wonder vs Spark 的模型分工与计价（逻辑未知）；标题随意写法（Morde 用“动词 + 对象”固定规则）；内部调度表当 UI 工作项。

## 实施顺序 P0 → P1 → P2

P0 闭环最缺的（不做完不进 P1）：

| # | 做什么 | 状态 |
|---|---|---|
| P0-1 | 分类兜底链 | ✅ 已完成并部署（PR #1） |
| P0-2 | Bug 3 落地 + DSH 镜像重建验证 | ✅ 已完成并部署（PR #1） |
| P0-2b | Coordinator 容量与摘要转发修复 | ✅ 代码已完成并部署（PR #2）；端到端真实对话验证待用户在 PC 上发一条消息 |
| P0-3 | Planner 最小方案 + brief 四件套（职责在 Harness 内，落不落独立文件由实现时定；无验收标准时只在影响执行 / 安全 / 成本 / 不可逆结果时才问最少信息，不是一律追问） | ⏳ 待做（建议下一项） |
| P0-4 | 合并窗口最小版：窗口时长按实测调，不先定 60s；轻量同话题判定 | ⏳ 待做 |
| P0-5 | 审批卡证据化（本体） | ⏳ 待做 |
| P0-6 | 事件本体 9 种 + 状态机加 `verifying` / `delivered` 两态 | ⏳ 待做 |
| P0-7 | 工具注册表 + 64 上限改 top-k + 门禁其二 | ⏳ 待做 |
| P0-8 | 任务记录形态对齐 + 主对话只出文字的 UI 审计 | ⏳ 待做（依赖 20 项截图验收，至今一次没跑过） |

P1 验证闭环（证明 P0 没改坏）：P1-1 独立验证者最小可用；P1-2 可观测第 1 步（路由分类一个 span）+ 评测 live 套件（44 条，先跑 P0 smoke 10 条）；P1-3 评测 Judge 接线 + 失败回流；P1-4 上下文加固（摘要冷却 600s + terminal 清单 + fallback，溢出后压完重试一次）；P1-5 成本门 + 用量持久化 + 审批 guardian；P1-6 门禁补齐 + 工具轮收尾；P1-7 纠正变检查最小形态 + 记忆后台同步；P1-8 Turn 契约补齐。

P2 沉淀与规模（P1 跑稳两周后再启动）：P2-1 Skill 可写六要素；P2-2 信任阶梯 + routine 窄触发 + Test 真跑；P2-3 Postmortem + playbook 广播 + 钩子引擎；P2-4 评测常态化 + 盲测 panel + CodeAct + 工作图 / handoff；P2-5 规模（多租户适配层收尾 + 压测 + 场景包复制给第二个人实测 + 单条消息分段读）。

建议顺序：P0-3 / P0-4（任务模型与拆分）→ P0-5 / P0-6（审批 + 事件）→ P0-7 / P0-8（工具 + 记录形态）。

## 已完成 vs 未完成（总账）

已完成：路由分类兜底、DSH 任务派生、coordinator 容量转发、长消息摘要校验（用户自己找到的根因）、DSH 镜像重建流程、云电脑 hdqokxzv 在跑、服务器 998f13d0。
未完成：Planner、合并窗口、审批卡本体字段、事件链本体、工具注册表与 top-k、Skill 可写、独立验证者、评测 Judge、可观测第 1 步、上下文加固（摘要 0 次）、前端仍是 2026-10-26 旧构建、单 DeepSeek 模型兜底、无平台默认模型。

## 待用户拍板的事（只列启动最相关的三件，其余 11 件见复刻设计 §9）

1. P0 从哪一项开始：建议 P0-3 Planner，是否按这个顺序？
2. 合并窗口时长：不先定 60s，按实测调，是否接受这个做法？
3. 子 agent 深度上限：Morde 暂定限 1，但 Muse 实测嵌套到 2 层（31%），是否放开到 2 层，还是坚持限 1？

## 证据与来源索引

- 逆向工程：`muse-harness-reverse-engineering.md`（700 行，证据分级；底稿 `~/workspace/muse-harness-rev/part-a-evidence.md` 26 条 SQL、`part-b-docs.md` 58 skill + 143 connector 清单、`part-c-hermes.md` hermes 精读固定提交 0d49d6d9、`part-d-morde-mapping.md`）
- 复刻设计：`morde-muse-replication-design.md`（v1.0，P0/P1/P2 与 14 件待拍板）
- 验收用例：`muse-harness-replication-test-cases.md`（139 条，2026-11-24 删 17 条不符合方向的）
- 开源映射：`muse-steps-open-source-mapping.md`
- 施工 spine：repo `docs/muse_docs/muse_steps/`（TODO 00–11）
- 目标文档：`morde-target-design/morde-target-design.md`；定位 v1.5：`morde-positioning-v1.5.md`
- hermes 需求：`morde-hermes-copy-requirements.md`（§2.6 19 步定稿）；hermes 设计：`morde-hermes-copy-design.md`
- 上下文研究：`~/workspace/hermes-context-research.md`
- 流程图：`muse-harness-liu-cheng-tu`（13 张详细图，节点带证据等级）
