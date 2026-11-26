# 会话 B-02：多 Agent 与工具治理

**目标：**完成 R03/R12 及 R15；B-01 的身份 / 计划接口稳定后开始，不必等待其他 `01` 全部结束。先读[并发总览](<00-并发实施总览.md>)、[体验目标](<../Morde AI对话目标体验.md>)和[原方案](<../Morde 复刻 Muse 一体化实施与验收 TODO.md>)中 R03/R12/R15。

**写入范围：**B 专属的 `server/internal/app/agent_sessions.go`、`server/internal/store/postgres/agent_sessions.go`、`server/internal/app/cloud_dsh_mcp_builtin.go`、`cloud_dsh_skills.go`、`server/internal/store/postgres/cloud_dsh_skill_reads.go`、意图 / 工具与 Skill 选择专属文件及测试；`runtime/morde-cloud-runtime/` 和 `cloud_dsh_session_tree.go` 始终由 C 写，Broker / 预算 / schema 由 A 写。将 Runtime 调整写成交接需求，不自行改 C 的文件。

**设计与交接：**按[多 Agent 与共享资源](<01-复杂链路设计与交接.md#agents-tools>)绑定父子身份、工具摘要与资源互斥；按[受控 Skill 设计](<01-复杂链路设计与交接.md#memory-skill>)核对来源版本和动态提供方式，不把模型选择或 Skill 当作授权。

- [ ] **B02-0 普通问答、新闻搜索、追问续接与任务计划优先闭环（进行中）。**先于两个子 Agent 协作实现和验收这四类主链路：普通问答使用直接回复，不创建 Task 或要求选择 Host；新闻请求使用当前授权的只读搜索 / 阅读工具，核验来源与日期并区分搜索失败；追问按同一会话内经授权的 Task 与当前修订续接，不能重复建任务或误续接；复杂请求按需创建和更新计划，服务端校验 Task、计划、授权和来源，简单问答及单次搜索不强制建计划。已有局部实现与单测不代表真实模型、搜索配置或 PC/DSH 闭环通过。
  - 验收：App / Store / Worker 定向回归覆盖四类流程及无 Task、无误续接、计划依赖拒绝；真实 DeepSeek 路由与搜索结果 / 来源验证单独记录；在实际 `morde-pc` 操作代表性流程，记录服务端实际状态。搜索未启用、PC 无法启动或历史 / Task 关联证据不足时明确待验，不用预置文案代替实际结果。四项闭环完成后再开始 B02-1 / B02-6 的两个子 Agent 协作。
  - **本地验证（2026-11-26）：**`cd server && GOPROXY=off GOSUMDB=off go test ./internal/app -run 'ConversationRoute|TaskPlan|BuiltinSearch' -count=1 -timeout=8m` 通过（0.881s）；`go test ./internal/worker -run 'CloudDSHWorkerPlanToolDispatchAndPersistencePath|ConversationRoute|InternalTool' -count=1 -timeout=8m` 通过（2.108s）。既有定向用例覆盖普通问答不预留 Task、只读搜索路由、授权候选续接、计划 Brief / 来源 / 依赖校验。Worker 计划回归使用 scripted ModelClient，只证明 Worker→Broker 分发及计划持久化，不证明生产模型会按语义选择工具。`go test ./internal/domain/task -count=1 -timeout=8m`、`go test ./internal/mcpconnector -run 'BuiltinSearch' -count=1 -timeout=8m`、`go test ./internal/config -run 'BuiltinSearch' -count=1 -timeout=8m` 及 `npm --prefix deploy/single/search run test:morde-patches` 通过（日期补丁 10/10）；本项 TODO 与计划测试 fixture 的 `git diff --check` 通过。
  - **实际 PC（2026-11-26）：**用 `make pc` 启动并操作源码 `morde-pc`。普通问答 `2+2` 得到 `4`；本地路由测试证明 taskless 分支，但这次 PC 会话未从权威账本核实无 Task / Host。实际新闻请求因搜索工具未返回已确认结果而停止，未伪造来源。向尚未确认计划的任务询问状态，PC 回答任务等待用户处理、0 成功 / 0 失败 / 0 取消 / 1 未确认，原因为 `reconciliation_required`，且没有重试或新建任务；答复与当前界面状态相符，但 Task ID / revision 的权威关联尚未核实。复杂计划请求的 PC Task 因 `The previous cloud task checkpoint could not be reconciled safely.` 停在 reconciliation，计划正文没有交付，不能计为通过。
  - **搜索服务验证及待验：**`scripts/setup-builtin-search.sh` 在本机启动固定版 SearXNG `2026.11.5+3e95f6cd2`，JSON health check 通过；但 `TestBuiltinSearchWithLocalSearxng` 对 `latest AI news` 返回 `isError=true`、无来源链接。直接查询 SearXNG 得 0 条结果，`brave` 超时、DuckDuckGo / Google CSE TLS 验证失败；本机及测试主机对 DuckDuckGo 的 HTTPS 连接均超时。已停止本机临时容器。测试主机只读配置明确 `cloud_dsh.builtin_search.enabled: false`，容器未安装、8189 不可达；本轮不部署或重启远端服务，避免干扰同一测试机上的 C-01 隔离候选联测。新闻搜索须等外网搜索出口 / TLS 配置可用、再配置远端服务后做 PC 来源验收。
  - **计划与后续顺序：**计划恢复需 C-01 核查 checkpoint / Task 关联并重验；追问仍需权威 Task ID / revision 关联证据。故 B02-0 保持进行中，B02-1 / B02-6 的产品子 Agent 协作仍未开始。

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

### 2026-11-26 B-02 TODO 联合审计

- **B02-0 进行中。**普通问答在源码 PC 曾收到 `2+2 → 4`，但未从权威账本确认这次会话没有创建 Task/选择 Host；新闻搜索未得到可核验来源；追问没有现场 settled decision 的目标 Task/revision 证据；复杂计划在 checkpoint reconciliation 停止。四项未闭环前，按用户要求不开始 B02-1/B02-6 的子 Agent 协作验收。
- **B02-1 未完成。**本地父级工具子集、会话隔离与 App/Worker 路径回归通过；仍缺真实两个 worker 的时间重叠、冲突资源协调、子结果逐级汇总及 PC/真实 DSH 证据。
- **B02-2 未完成。**64 项工具定义上限与重复调用提醒已做定向回归；预算及必需工具仲裁、重启后的工具集一致性、底层取消确认和 unknown 副作用不重放仍待验。
- **B02-3 未完成。**受控 Skill 读取不等于候选审批、启用、撤销及跨账号/Task 隔离；且依赖 B02-1/2 和 C-02 R09。
- **B02-4 未完成。**缺少可运行 R16 Skill 合同交接与 A-02 协作/资源验收交接记录；等待 B02-3 合同稳定。
- **B02-5 未完成。**本地工具边界已有部分来源提示、日期校验和凭据过滤测试；T57/T59 正常与伪造授权样本、无法读取/定位时的端到端反馈及真实 PC/模型证据未完成。
- **B02-6 未完成。**Store 已有子 Session 结算、取消、子树隔离和 checkpoint 绑定的局部能力，但缺少覆盖失败/unknown、撤权、陈旧修订、重复事件、进程重启后父级取得可回查事实的完整回归，以及真实 DSH/PC 终态。
- **B02-7 未完成。**App/Worker 的选定 MCP、Skill schema、撤权和父级工具子集测试通过；仍缺真实 DSH/模型按语义选择已授权能力及 PC 对无匹配能力的用户反馈。
- **缺陷审计结论：**只读审计未确认可独立安全修复并关闭上述 TODO 的生产缺陷。曾提出的根 Session 委派工具集合可能扩大的疑点，经 `TestCloudDSHDelegateToolsAreRestrictedToParentDefinitions`、嵌套委派子集和不扩张已存储选择等定向回归未复现；不放宽任何授权门。测试主机/PC profile 与其他并行验收仍在协调，本轮没有远端写操作或 Store/PostgreSQL 集成操作。
