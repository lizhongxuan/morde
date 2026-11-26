# 会话 B-02：多 Agent 与工具治理

**目标：**完成 R03/R12 及 R15；B-01 的身份 / 计划接口稳定后开始，不必等待其他 `01` 全部结束。先读[并发总览](<00-并发实施总览.md>)、[体验目标](<../Morde AI对话目标体验.md>)和[原方案](<../Morde 复刻 Muse 一体化实施与验收 TODO.md>)中 R03/R12/R15。

**写入范围：**B 专属的 `server/internal/app/agent_sessions.go`、`server/internal/store/postgres/agent_sessions.go`、`server/internal/app/cloud_dsh_mcp_builtin.go`、`cloud_dsh_skills.go`、`server/internal/store/postgres/cloud_dsh_skill_reads.go`、意图 / 工具与 Skill 选择专属文件及测试；`runtime/morde-cloud-runtime/` 和 `cloud_dsh_session_tree.go` 始终由 C 写，Broker / 预算 / schema 由 A 写。将 Runtime 调整写成交接需求，不自行改 C 的文件。

**设计与交接：**按[多 Agent 与共享资源](<01-复杂链路设计与交接.md#agents-tools>)绑定父子身份、工具摘要与资源互斥；按[受控 Skill 设计](<01-复杂链路设计与交接.md#memory-skill>)核对来源版本和动态提供方式，不把模型选择或 Skill 当作授权。

- [ ] **B02-0 普通问答、新闻搜索、追问续接与任务计划优先闭环（进行中）。**先收口这四类主链路：普通问答使用直接回复，不创建 Task 或要求选择 Host；新闻请求使用当前授权的只读搜索 / 阅读工具，明确传入新闻分类，核验来源与日期并区分搜索失败；追问按同一会话内经授权的 Task 与当前修订续接，不能重复建任务或误续接；复杂请求按需创建和更新计划，服务端校验 Task、计划、授权和来源，简单问答及单次搜索不强制建计划。已有局部实现与单测不代表真实模型、远端配置或 PC/DSH 闭环通过。
  - 验收：App / Store / Worker 定向回归覆盖四类流程及无 Task、无误续接、计划依赖拒绝；真实 DeepSeek 路由与搜索结果 / 来源验证单独记录；在实际 `morde-pc` 操作代表性流程，记录服务端实际状态。新闻查询使用 `category=news` 及所需时间范围；近期搜索不得在无结果时静默移除时间范围，也不得把栏目首页或过期条目冒充近期新闻。服务不可用、PC 无法启动或历史 / Task 关联证据不足时明确待验，不用预置文案代替实际结果。B02-1 / B02-6 只依赖各自所需的 B01/A 身份、Task、计划和结算合同稳定；网络出口或 PC/真实 DSH 验收阻塞不阻止相互独立的代码开发。当前按用户要求只执行步骤 2，不启动后续任务。
  - **本地验证（2026-11-26）：**`cd server && GOPROXY=off GOSUMDB=off go test ./internal/app -run 'ConversationRoute|TaskPlan|BuiltinSearch' -count=1 -timeout=8m` 通过（0.881s）；`go test ./internal/worker -run 'CloudDSHWorkerPlanToolDispatchAndPersistencePath|ConversationRoute|InternalTool' -count=1 -timeout=8m` 通过（2.108s）。既有定向用例覆盖普通问答不预留 Task、只读搜索路由、授权候选续接、计划 Brief / 来源 / 依赖校验。Worker 计划回归使用 scripted ModelClient，只证明 Worker→Broker 分发及计划持久化，不证明生产模型会按语义选择工具。`go test ./internal/domain/task -count=1 -timeout=8m`、`go test ./internal/mcpconnector -run 'BuiltinSearch' -count=1 -timeout=8m`、`go test ./internal/config -run 'BuiltinSearch' -count=1 -timeout=8m` 及 `npm --prefix deploy/single/search run test:morde-patches` 通过（日期补丁 10/10）；本项 TODO 与计划测试 fixture 的 `git diff --check` 通过。
  - **此前实际 PC（2026-11-26 快照）：**用 `make pc` 启动并操作源码 `morde-pc`。普通问答 `2+2` 得到 `4`；本地路由测试证明 taskless 分支，但这次 PC 会话未从权威账本核实无 Task / Host。实际新闻请求因搜索工具未返回已确认结果而停止，未伪造来源。向尚未确认计划的任务询问状态，PC 回答任务等待用户处理、0 成功 / 0 失败 / 0 取消 / 1 未确认，原因为 `reconciliation_required`，且没有重试或新建任务；答复与当前界面状态相符，但 Task ID / revision 的权威关联尚未核实。复杂计划请求的 PC Task 因 `The previous cloud task checkpoint could not be reconciled safely.` 停在 reconciliation，计划正文没有交付，不能计为通过。本次步骤 2 复核没有启动 PC；上方最新复核说明当前待验项。
  - **早期搜索服务验证及待验：**`scripts/setup-builtin-search.sh` 在本机启动固定版 SearXNG `2026.11.5+3e95f6cd2`，JSON health check 通过；但 `TestBuiltinSearchWithLocalSearxng` 对 `latest AI news` 返回 `isError=true`、无来源链接。直接查询 SearXNG 得 0 条结果，`brave` 超时、DuckDuckGo / Google CSE 的一次性 TLS 探测失败（根因未证实；下方最新严格 HTTPS 检查未证实 CA 校验错误）；本机及测试主机对 DuckDuckGo 的 HTTPS 连接均超时。此次启动的本地临时容器已停止。此前对测试主机只读检查称 `cloud_dsh.builtin_search.enabled: false`、容器未安装且 8189 不可达；这些是历史环境记录，本轮没有连接或检查远端。最新本地搜索证据与出口限制见下方。
  - **计划与后续顺序：**计划恢复仍需 C-01 核查 checkpoint / Task 关联并重验；追问仍需 PC/真实模型的权威 Task ID / revision 关联证据。B02-0 保持进行中。搜索环境和 PC/DSH 只阻塞相应的真实端到端验收；B02-1 / B02-6 的独立实现可在各自接口合同稳定后继续，本步骤不会启动它们。

### 2026-11-26 步骤 2 最新复核

- **新闻工具本地修复：**之前 `morde_web_search` 没有新闻分类参数，真实查询会返回出版社栏目首页。现在模型合同增加唯一允许值 `category="news"`，只在 Broker 边界映射为 SearXNG `categories="news"`；路由持久化保留 Morde 原始参数，避免映射后再次校验失败。普通网页搜索不传分类时仍保持原行为；不接受其他分类。任务工作项摘要现在会显示“category: news”。对应实现：`cloud_dsh_mcp_builtin.go:cloudDSHBuiltinSearchDefinitions/builtinSearchArguments`、`conversation_route_decision.go:conversationRouteDirectToolInstructions/parseConversationRouteDecision`、`mcpconnector/builtin_search.go:validBuiltinSearchArguments`、`task_step_detail.go:taskToolInputSummary`。
- **定向自动化：**App `ConversationRoute|TaskPlan|BuiltinSearch|TaskToolInputSummary` 组通过（1.301s）；Worker `CloudDSHWorkerPlanToolDispatchAndPersistencePath|ConversationRoute|InternalTool` 组通过（2.188s）；`mcpconnector` `BuiltinSearch` 本地合同组通过（1.972s）。覆盖新闻参数验证/映射、分类路由原文持久化、普通问答与 route、Task Plan 及 Worker 分发。上述均不证明真实模型自己选对工具或给出正确总结。
- **真实搜索/来源读取（本机受控 SearXNG，非远端配置）：**固定版 `mcp-searxng 2.10.0` → `CloudDSHToolBroker` → 本地 SearXNG `2026.11.5+3e95f6cd2`，实际用 `category=news,time_range=week` 搜索 `artificial intelligence news`；随后 `morde_web_read` 读取搜索结果页面。App 集成测试通过（4.48s），任务操作已持久化并能按 checkpoint 安全回放；来源为 The Motley Fool 的 AI 股票文章，URL 日期段和页面日期元数据候选均为 `2026-11-25`。元数据明确标记“unverified”，因此只证明页面可读和候选可取，不证明模型已核验页面展示日期、相关性或完成用户级新闻摘要。
- **搜索环境与 TLS：**同一新闻引擎曾返回 `Search Degraded`：Google News access denied、Bing News timeout、Reuters HTTP error；DuckDuckGo News 的周范围请求返回 0 条，无时间范围时曾混入 2024 年 Wikinews 结果，所以不得无过滤回退。主机直接 HTTPS 严格验证结果：Google Search 200、Google News RSS 最终可读、Reuters 401 均为 `tls_verify=0`；DuckDuckGo 与 Brave 在 TLS 握手前连接超时。未关闭证书校验；问题是出口/引擎可用性与授权响应，不是已证实的 CA 失败。新闻服务有波动，单次通过不能保证长期可用。
- **追问与计划 PostgreSQL：**独立本机 PostgreSQL 16.15，步骤 2 定向 11 项通过（13.046s）：覆盖状态查询绑定授权 Task、无新 Task/无停止，join 关联与并发、跨 Task 依赖、计划 revision replay，以及 Hub command 依赖绑定。初跑发现迁移 `00251` 重建约束时漏回 `hub_operation` 分支，使计划内 Hub dispatch 被拒；新增 `00254_task_plan_hub_binding_constraint_repair.sql`，单项和完整 11 项复验通过。该修复未部署，真实 DSH Task 恢复与实际 PC 计划仍未验。
- **仍待验：**实际 `morde-pc` 与本地 Portal/DSH 未启动；本机 `7080/7180` 无 Portal listener，本步骤未连接远端或操作远端数据。源码 PC 旧观察中的计划 checkpoint reconciliation、PC 追问的权威 Task/revision 绑定未被本地测试解决。单步搜索当前由工具结果直接形成对话文本，真实 DSH 的日期核验与自然语言摘要未验；因此 B02-0 不勾选，B02-1/6 代码工作不受搜索网络阻塞。

- [ ] **B02-1 真实协作与结果汇总。**复用 DSH 委派及 root → coordinator → worker 的既有能力；独立子任务可并行，有依赖的步骤依真实结果衔接，权限不超过父级。
  - 验收：T09–T12 覆盖两层深度、两 worker 实际时间重叠、同一 GUI / 冲突写入不无序并发、成功 / 失败 / unknown 逐级汇总、取消目标不影响独立任务和 checkpoint 恢复；真实 PC + 模型由 E/A 共同核验 T10/T43。
  - **本地复核（2026-11-26）：**App `AgentChildSession` 定向测试通过（见下方 App 命令）；Worker `TestCloudDSHDelegateToolsAreRestrictedToParentDefinitions`、嵌套委派子集及控制工具隔离测试通过（见下方 Worker 命令）。调用 schema 与服务端参数校验均以父 Session 当前可见 definitions 为上限，未复现根委派扩权；此证据不代表真实两 worker 并发、结果汇总或 PC/DSH 验收，B02-1 保持未完成。
- [ ] **B02-2 工具选择、循环和取消。**复核 65 项 MCP 目录、模型每轮工具上限、必需工具 / 预算 / 权限及既有 `repeat-tool-reminder`。工具集恢复保持一致；无法证明底层停止时保持 unknown，不把 IPC 等待超时冒充硬取消。
  - 验收：T42 覆盖超限、必需工具不丢、授权变化、重启、取消及未知副作用不重放；缺失 Runtime 适配交 C、共享 Broker / 预算交 A。
  - **本地复核（2026-11-26）：**Worker 的 `TestCloudDSHToolDefinitionsRejectUnclassifiedOverflow` 通过，覆盖 64 个定义上限；DSH 锁定 `@deepseek-ai/dsh-repeat-tool-reminder@0.2.2-rc.1`，运行时定向测试通过（3 次相同工具调用后发提醒，同时仍允许 Broker 授权调用）。这不覆盖预算/必需工具仲裁、重启后的工具集一致性或底层取消确认，B02-2 保持未完成。
- [ ] **B02-3 受控 Skill 候选。**先完成 B02-1/2；C-02 的 R09 记忆 / 来源 / 授权合同稳定后实施 R15。核对现有 Skill 读取与 DSH 上游 provider 的接入、版本及隔离能力，适用时优先复用；候选只在授权范围内形成、验证、启用和撤销。
  - 验收：A01 覆盖未批准不可用、明确启用后可用、撤权即时失效、不同账号 / Task 隔离；插件兼容及许可证核对记录在案。新 schema 交 A。
- [ ] **B02-4 交接给 C 和 A。**交出 Skill 候选 / 启用 / 撤销的可运行合同给 C-02 的 R16，并将协作图、资源归属和测试结果交 A-02。
  - 验收：C 可在不等本文件所有 PC 实测关闭的情况下使用已稳定合同；未运行真实模型或 PC 的部分明确待验。
- [ ] **B02-5 资料来源与工具边界（R26）。**复核 `cloud_dsh_mcp_builtin.go` 及文件/检索工具是否能传递真实来源、可用位置和不完整信息，按工具自身现有合同补缺；将 Runtime 调整交 C、权限和跨层字段交 A。
  - 验收：T57/T59 的本地受控来源与伪造授权样本中，正常分析仍可执行，资料内命令不能增加权限；无法读取和定位如实反馈。真实模型和实际 PC 分开记录。
- [ ] **B02-6 子任务监视与交接（M03-05/06）。**按[最终设计 L4](<../Morde 复刻 Muse 最终流程与开发设计.md>)核对已有子 Session、结算、后台 handoff 与恢复，补齐关键状态交回直接父 Session 的缺口；普通进度不反复唤醒，失败/取消/unknown 和必要阻塞不能被静默吞掉。B 负责子 Session 和事实组织；跨层持久身份/事务交 A，Runtime 与 checkpoint 接线交 C，用户可见终态交 D，PC 验收交 E。先交付独立可用的合同切片，不等待所有会话关闭。
  - 验收：按 M03-05 覆盖交错完成、失败与未知、取消/撤权、陈旧修订、重复事件及进程重启；父级拿到可回查事实且无重复副作用。T09–T12/T43 的实际 PC + 真实 DSH/模型另由 E/A 记录。统计比例不是验收门槛。
- [ ] **B02-7 已授权 Skill/connector 选择（L1）。**复核 `cloud_dsh_skills.go` 的 `nanomuse_skill_read` 与 `cloud_dsh_mcp_selected.go` 的 `mcp_discover` / `mcp_call`，让执行 Agent 按任务需要从当前用户/Task 可用目录选择并读取真实能力；B-01 负责语义路由，选择不授予权限、不自动创建 connector，也不要求普通问答进入 Task。自定义 Skill 的候选/确认仍归 B02-3，不以其完整验收阻塞已有受控目录使用。
  - 验收：已有授权工具可被正确选择并完成任务，用户无需记住 Skill/工具名；无匹配、未选择、撤权、目录/调用不一致时，说明缺失能力，存在可用配置入口或授权内替代时给出具体选择，不能编造工具、静默换账号/目标或越权派发。App/Broker 回归与实际 PC + DSH/模型语义分开记录。
  - **本地回归（2026-11-26）：**App `SelectedMCP`、`BuiltinSearch`、`AgentChildSession` 和 Skill 相关定向组通过（见下方 App 命令），覆盖懒发现、所选 connector、schema、撤权和调用门禁；它不证明真实 DSH/模型会按语义选择工具，也未在 PC 验证无匹配时的用户可见说明，B02-7 保持待验。

### 2026-11-26 B-02 本地工具治理定向复验

- `cd server && GOPROXY=off GOSUMDB=off go test ./internal/app -run 'AgentChildSession|SelectedMCP|BuiltinSearch|DSHSkill|CloudDSH.*Skill' -count=1 -timeout=8m` 通过（1.602s）。
- `cd server && GOPROXY=off GOSUMDB=off go test ./internal/worker -run '^(TestCloudDSHToolDefinitionsRejectUnclassifiedOverflow|TestCloudDSHDelegateToolsAreRestrictedToParentDefinitions|TestCloudDSHNestedDelegateRejectsSiblingToolsAndThirdLevel|TestCloudDSHChildDelegationToolsDoNotExpandStoredSelection)$' -count=1 -timeout=8m` 通过（0.639s）。
- `cd runtime/morde-cloud-runtime && node --test --test-concurrency=1 --test-name-pattern='nudges the Agent after three identical tool calls' test/runtime.test.mjs` 通过（1/1，0.800s）。仅覆盖本地组件合同；不证明真实并行子 Agent、预算仲裁、checkpoint/重启恢复、真实模型工具选择或实际 PC 终态呈现。

**依赖：**B01 的相关合同及 A01 共享身份先稳定；Runtime 修改由 C 独占。R15 等 C-02 先交付 R09 合同，再把 R15 合同交 C-02 做 R16；不要求 C-02 整份文档先关闭。

**定向回归：**在 `server/` 运行 `go test ./internal/app ./internal/worker -run 'AgentSession|CloudDSHNested|DSHSkill' -count=1`；PostgreSQL 并发与真实模型 / PC 的执行单独登记。

## 交接记录

记录父子身份 / 授权与工具集合合同、Skill 输入输出、C/A 的实现请求、测试命令和真实模型 / PC 未验项。

### 2026-11-26 B-02 TODO 联合审计（本轮步骤 2 验证前快照；最新状态见下方）

- **B02-0 当时进行中。**普通问答在源码 PC 曾收到 `2+2 → 4`，但未从权威账本确认这次会话没有创建 Task/选择 Host；当时新闻搜索未得到可核验来源；追问没有现场 settled decision 的目标 Task/revision 证据；复杂计划在 checkpoint reconciliation 停止。新闻工具链后来已补本地受控搜索和来源读取证据，但真实模型/PC 体验仍待验；详见下方“步骤 2 最新复核”。这里保留的是此前快照，不代表本轮最终状态。
- **B02-1 未完成。**本地父级工具子集、会话隔离与 App/Worker 路径回归通过；仍缺真实两个 worker 的时间重叠、冲突资源协调、子结果逐级汇总及 PC/真实 DSH 证据。是否启动后续并行任务按用户的步骤安排执行；本轮未开始。
- **B02-2 未完成。**64 项工具定义上限与重复调用提醒已做定向回归；预算及必需工具仲裁、重启后的工具集一致性、底层取消确认和 unknown 副作用不重放仍待验。
- **B02-3 未完成。**受控 Skill 读取不等于候选审批、启用、撤销及跨账号/Task 隔离；且依赖 B02-1/2 和 C-02 R09。
- **B02-4 未完成。**缺少可运行 R16 Skill 合同交接与 A-02 协作/资源验收交接记录；等待 B02-3 合同稳定。
- **B02-5 未完成。**本地工具边界已有部分来源提示、日期校验和凭据过滤测试；T57/T59 正常与伪造授权样本、无法读取/定位时的端到端反馈及真实 PC/模型证据未完成。
- **B02-6 未完成。**Store 已有子 Session 结算、取消、子树隔离和 checkpoint 绑定的局部能力，但缺少覆盖失败/unknown、撤权、陈旧修订、重复事件、进程重启后父级取得可回查事实的完整回归，以及真实 DSH/PC 终态。
- **B02-7 未完成。**App/Worker 的选定 MCP、Skill schema、撤权和父级工具子集测试通过；仍缺真实 DSH/模型按语义选择已授权能力及 PC 对无匹配能力的用户反馈。
- **缺陷审计结论：**只读审计未确认可独立安全修复并关闭上述 TODO 的生产缺陷。曾提出的根 Session 委派工具集合可能扩大的疑点，经 `TestCloudDSHDelegateToolsAreRestrictedToParentDefinitions`、嵌套委派子集和不扩张已存储选择等定向回归未复现；不放宽任何授权门。测试主机/PC profile 与其他并行验收仍在协调，本轮没有远端写操作或 Store/PostgreSQL 集成操作。
