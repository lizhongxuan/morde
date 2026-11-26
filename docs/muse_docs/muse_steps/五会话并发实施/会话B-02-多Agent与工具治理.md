# 会话 B-02：多 Agent 与工具治理

**目标：**完成 R03/R12 及 R15；B-01 的身份 / 计划接口稳定后开始，不必等待其他 `01` 全部结束。先读[并发总览](<00-并发实施总览.md>)、[体验目标](<../Morde AI对话目标体验.md>)和[原方案](<../Morde 复刻 Muse 一体化实施与验收 TODO.md>)中 R03/R12/R15。

**写入范围：**B 专属的 `server/internal/app/agent_sessions.go`、`server/internal/store/postgres/agent_sessions.go`、`server/internal/app/cloud_dsh_mcp_builtin.go`、`cloud_dsh_skills.go`、`server/internal/store/postgres/cloud_dsh_skill_reads.go`、意图 / 工具与 Skill 选择专属文件及测试；`runtime/morde-cloud-runtime/` 和 `cloud_dsh_session_tree.go` 始终由 C 写，Broker / 预算 / schema 由 A 写。将 Runtime 调整写成交接需求，不自行改 C 的文件。

**设计与交接：**按[多 Agent 与共享资源](<01-复杂链路设计与交接.md#agents-tools>)绑定父子身份、工具摘要与资源互斥；按[受控 Skill 设计](<01-复杂链路设计与交接.md#memory-skill>)核对来源版本和动态提供方式，不把模型选择或 Skill 当作授权。

- [ ] **B02-0 普通问答、新闻搜索、追问续接与任务计划优先闭环（进行中）。**先收口这四类主链路：普通问答使用直接回复，不创建 Task 或要求选择 Host；新闻与时效请求进入 DSH Task，通过授权的只读搜索 / 阅读工具发现候选、读取发布方页面并核验来源和日期，失败时如实说明，不编造来源；追问按同一会话内经授权的 Task 与当前修订续接，不能重复建任务或误续接；复杂请求按需创建和更新计划，服务端校验 Task、计划、授权和来源，简单问答及一次可答的普通网页查找不强制建计划。新闻 `category=news` 在 Broker 内固定转为 `!mgn` 引擎选择并保留时间范围；精确标题/发布方查找使用受限 `lookup_purpose=publisher_verification` 并固定选择 SearXNG `!yh`，不叠加分类或时间范围。两个引擎标记均为内部实现细节，不向用户泄漏；来源查找本身不证明链接或日期。已有局部实现与单测不代表真实模型或 PC/DSH 闭环通过。
  - 验收：App / Store / Worker 定向回归覆盖四类流程及无普通问答 Task、无误续接、计划依赖拒绝；真实模型路由、搜索结果与来源核验单独记录；在实际 `morde-pc` 操作代表性流程并记录服务端实际状态。新闻查询带 `category=news` 及所需时间范围；近期搜索不得在无结果时静默移除时间范围，也不得把栏目首页、RSS 聚合包装页或过期条目冒充已核验新闻。服务不可用、PC 无法启动或历史 / Task 关联证据不足时明确待验，不用预置文案代替实际结果。B02-1 / B02-6 只依赖各自所需的 B01/A 身份、Task、计划和结算合同稳定；网络出口或 PC/真实 DSH 验收阻塞不阻止相互独立的代码开发。当前按用户要求只执行步骤 2，不启动后续任务。
  - **本地验证（2026-11-26）：**`cd server && GOPROXY=off GOSUMDB=off go test ./internal/app -run 'ConversationRoute|TaskPlan|BuiltinSearch' -count=1 -timeout=8m` 通过（0.881s）；`go test ./internal/worker -run 'CloudDSHWorkerPlanToolDispatchAndPersistencePath|ConversationRoute|InternalTool' -count=1 -timeout=8m` 通过（2.108s）。既有定向用例覆盖普通问答不预留 Task、只读搜索路由、授权候选续接、计划 Brief / 来源 / 依赖校验。Worker 计划回归使用 scripted ModelClient，只证明 Worker→Broker 分发及计划持久化，不证明生产模型会按语义选择工具。`go test ./internal/domain/task -count=1 -timeout=8m`、`go test ./internal/mcpconnector -run 'BuiltinSearch' -count=1 -timeout=8m`、`go test ./internal/config -run 'BuiltinSearch' -count=1 -timeout=8m` 及 `npm --prefix deploy/single/search run test:morde-patches` 通过（日期补丁 10/10）；本项 TODO 与计划测试 fixture 的 `git diff --check` 通过。
  - **此前实际 PC（2026-11-26 快照）：**用 `make pc` 启动并操作源码 `morde-pc`。普通问答 `2+2` 得到 `4`；本地路由测试证明 taskless 分支，但这次 PC 会话未从权威账本核实无 Task / Host。实际新闻请求因搜索工具未返回已确认结果而停止，未伪造来源。向尚未确认计划的任务询问状态，PC 回答任务等待用户处理、0 成功 / 0 失败 / 0 取消 / 1 未确认，原因为 `reconciliation_required`，且没有重试或新建任务；答复与当前界面状态相符，但 Task ID / revision 的权威关联尚未核实。复杂计划请求的 PC Task 因 `The previous cloud task checkpoint could not be reconciled safely.` 停在 reconciliation，计划正文没有交付，不能计为通过。本次步骤 2 复核没有启动 PC；上方最新复核说明当前待验项。
  - **早期搜索服务验证及待验：**`scripts/setup-builtin-search.sh` 在本机启动固定版 SearXNG `2026.11.5+3e95f6cd2`，JSON health check 通过；但 `TestBuiltinSearchWithLocalSearxng` 对 `latest AI news` 返回 `isError=true`、无来源链接。直接查询 SearXNG 得 0 条结果，`brave` 超时、DuckDuckGo / Google CSE 的一次性 TLS 探测失败（根因未证实；下方最新严格 HTTPS 检查未证实 CA 校验错误）；本机及测试主机对 DuckDuckGo 的 HTTPS 连接均超时。此次启动的本地临时容器已停止。此前对测试主机只读检查称 `cloud_dsh.builtin_search.enabled: false`、容器未安装且 8189 不可达；这些是历史环境记录，本轮没有连接或检查远端。最新本地搜索证据与出口限制见下方。
  - **计划与后续顺序：**计划恢复仍需 C-01 核查 checkpoint / Task 关联并重验；追问仍需 PC/真实模型的权威 Task ID / revision 关联证据。B02-0 保持进行中。搜索环境和 PC/DSH 只阻塞相应的真实端到端验收；B02-1 / B02-6 的独立实现可在各自接口合同稳定后继续，本步骤不会启动它们。
  - **2026-11-26 本轮继续复验：**App/Worker/MCP 的路由、计划、工具定向组通过；9 项隔离 PostgreSQL 用例覆盖直接回复幂等、并发 join 和授权/范围复核、计划修订及真实操作结算门控，全部通过。新闻源集成经过严格门禁后再跑通过：新闻发现、标题/发布方候选搜索、直接 HTTPS 页面读取成功，页面有与 RSS 时间相同的日期文本；现若找不到直接来源或日期文本会失败，不会把子测试跳过算作 PASS。相同一轮的通用 `latest AI news` JSON 探针仍返回 0 条，说明搜索服务有波动，不能声称普遍可用。临时 SearXNG 与 PostgreSQL 容器已清理；7080/7180/8189 当前无监听，未启动真实 DSH 或 PC 对话。普通问答、新闻自然语言综合、追问的权威 Task/revision 及复杂计划实际恢复仍待实际源码 `morde-pc` + Portal/DSH/真实模型验收；B02-0 不勾选。详见[步骤 2 继续验证记录](../../../验证记录/Morde_Harness_最终流程测试用例.md#step-2-continued-2026-11-26)。

### 2026-11-26 步骤 2 最新复核

- **新闻工具本地修复（早期实现，现已由下方 `!mgn` 复核更新）：**模型合同增加唯一允许值 `category="news"`。初版把它映射为 SearXNG `categories="news"`，但实测会扇出多个新闻引擎并出现超时；最终 Broker 改为固定 `!mgn` 单引擎选择，并保留 Morde 原始路由参数和时间范围。普通网页搜索不传分类时保持原行为。
- **定向自动化：**App `ConversationRoute|TaskPlan|BuiltinSearch|TaskToolInputSummary` 组通过（1.301s）；Worker `CloudDSHWorkerPlanToolDispatchAndPersistencePath|ConversationRoute|InternalTool` 组通过（2.188s）；`mcpconnector` `BuiltinSearch` 本地合同组通过（1.972s）。覆盖新闻参数验证/映射、分类路由原文持久化、普通问答与 route、Task Plan 及 Worker 分发。上述均不证明真实模型自己选对工具或给出正确总结。
- **真实搜索/来源读取（本机受控 SearXNG，非远端配置）：**固定版 `mcp-searxng 2.10.0` → `CloudDSHToolBroker` → 本地 SearXNG `2026.11.5+3e95f6cd2`，实际用 `category=news,time_range=week` 搜索 `artificial intelligence news`；随后 `morde_web_read` 读取搜索结果页面。App 集成测试通过（4.48s），任务操作已持久化并能按 checkpoint 安全回放；来源为 The Motley Fool 的 AI 股票文章，URL 日期段和页面日期元数据候选均为 `2026-11-25`。元数据明确标记“unverified”，因此只证明页面可读和候选可取，不证明模型已核验页面展示日期、相关性或完成用户级新闻摘要。
- **搜索环境与 TLS：**同一新闻引擎曾返回 `Search Degraded`：Google News access denied、Bing News timeout、Reuters HTTP error；DuckDuckGo News 的周范围请求返回 0 条，无时间范围时曾混入 2024 年 Wikinews 结果，所以不得无过滤回退。主机直接 HTTPS 严格验证结果：Google Search 200、Google News RSS 最终可读、Reuters 401 均为 `tls_verify=0`；DuckDuckGo 与 Brave 在 TLS 握手前连接超时。未关闭证书校验；问题是出口/引擎可用性与授权响应，不是已证实的 CA 失败。新闻服务有波动，单次通过不能保证长期可用。
- **追问与计划 PostgreSQL：**独立本机 PostgreSQL 16.15，步骤 2 定向 11 项通过（13.046s）：覆盖状态查询绑定授权 Task、无新 Task/无停止，join 关联与并发、跨 Task 依赖、计划 revision replay，以及 Hub command 依赖绑定。初跑发现迁移 `00251` 重建约束时漏回 `hub_operation` 分支，使计划内 Hub dispatch 被拒；新增 `00254_task_plan_hub_binding_constraint_repair.sql`，单项和完整 11 项复验通过。该修复未部署，真实 DSH Task 恢复与实际 PC 计划仍未验。
- **仍待验：**实际源码 `morde-pc` 已通过 loopback synthetic fixture 验证普通问答直回不建 Task、固定搜索样本的 Task 结果投影；fixture 使用 synthetic model/search，不算真实新闻或 DSH 语义证据。当前工作区 Portal/DSH 未启动，本机 `7080/7180` 无 Portal listener。源码 PC 旧观察中的计划 checkpoint reconciliation、PC 追问的权威 Task/revision 绑定未被该 UI 夹具解决。新闻已由路由送入 DSH Task，但真实 DSH/模型的日期核验、新闻筛选和自然语言综合回答未验；因此 B02-0 不勾选，B02-1/6 代码工作不受搜索网络阻塞。PC fixture 细节见[步骤 2 继续验证记录](../../../验证记录/Morde_Harness_最终流程测试用例.md#step-2-continued-2026-11-26)。

### 2026-11-26 新闻路径复核补充

- **路由与任务动态：**普通网页查询保持单次只读直达；新闻/时效参数触发 `new` Task，提供来源阅读与合成空间。内部新闻引擎标记不再显示为查询前缀，动态摘要显示 `News search: <query> (time range: <range>)`。
- **上游超时修复：**`category=news` 只表达搜索分类，曾使 MCP 扇出全部新闻引擎并超时。Broker 现将已校验的新闻调用转换成固定的 `!mgn` 引擎选择，并保留用户时间范围；不接受任意 SearXNG 引擎选择，也不移除时间过滤。曾有一次实际 MCP 搜索发现、精确标题/发布方二次搜索及直接页面读取完整通过（5.48s）；重复运行时新闻发现成功，但常规来源搜索返回上游失败，发布方解析子测试 SKIP（明确待验）。
- **不准确日期的防线：**RSS 发布时间候选 `2026-11-25` 与发布方页面 metadata 候选 `2025-06-06` 不一致，页面日期候选仍为 unverified。网页读取指引已要求以发布方页面核验，未验证不得称为“今天/最近”。真实模型是否遵循、能否为该需求找到多条准确新闻仍待真实 DSH/Provider 测试；不把这个工具集成用例算成新闻体验通过。
- **复验结果：**最新 App 定向组通过（1.053s），Worker 定向组通过（2.053s），MCP connector 组通过（1.064s），setup 脚本全部模拟场景通过，日期/URL 补丁测试 10/10。宽泛 news fan-out 曾在 MCP 10 秒窗口失败；固定 `!mgn` 新闻发现可用，但常规来源搜索会波动。TLS 证书验证保持开启，当前证据仍未发现 CA 错误。
- **离线引擎测试与最新网络复查（2026-11-26）：**新增 `scripts/test-builtin-search-engine.sh`，在固定 SearXNG 镜像、无网络容器中运行 4 项引擎单测，验证 day/week/month/year 时间范围、RSS 来源和日期解析、HTTP 链接拒绝及 20 条上限，全部通过。实际 Google News RSS 使用系统默认证书校验返回 3 条候选；逐条请求其聚合链接都仍停留在 `news.google.com`，不能当作发布方原文。随后通过 MCP→Broker→本机 SearXNG 的新闻发现仍成功，但发布方解析子测试再次 SKIP；SearXNG 搜索账本显示 Google CSE `Suspended: too many requests`、DuckDuckGo `CAPTCHA`、Brave `unexpected crash`、Wikidata `Suspended: access denied`。另一次普通精确标题查询曾由 Google CSE 返回 20 个结果，说明可用性间歇而非完全不可达。临时本机搜索容器已停止，未连接远端，未关闭 TLS。
- **最终状态：**本地路由、搜索发现和 RSS 解析路径已有证据；可重复的发布方来源核验、真实新闻时效/摘要、真实 PC、PC 中追问 Task ID/revision、DSH checkpoint 恢复仍待验，B02-0 保持未勾选。

### 2026-11-26 搜索来源解析修复复验

- **实现：**新增结构化 `lookup_purpose=publisher_verification`，只允许与新闻类别、时间范围互斥的标题/发布方查询；Broker 固定转换为 Yahoo `!yh` 单引擎查询，setup readiness 检查同时确认 `!mgn` 新闻发现与 `!yh` 来源查询引擎已加载。任务动态将内部 bang 隐藏为“Publisher source search”，不向用户暴露内部引擎选择。
- **误判防护：**此前来源集成用例会因为 Yahoo 截断展示标题、或短标题碰到栏目页而无法确认/误判。现用例跳过过短标题，只接受结果标题/摘要含完整候选标题、直接 HTTPS 非聚合 URL，且发布方读取正文再次包含候选标题；搜索成功或可读栏目页本身不算通过。
- **受控实测：**`TestBuiltinSearchIntegrationWithLocalSearxng` 最新运行通过（4.75s）。固定版 MCP → Morde Broker → 本机 SearXNG `!mgn` 实际发现近一周候选，随后通过 `!yh` 按完整标题/发布方解析到 MIT News 直接文章并由 `morde_web_read` 读取；返回正文包含完整标题和 `November 20, 2026`，与 RSS 日期候选 `2026-11-20T20:00:00+00:00` 的 UTC 日期相符。操作写入 Morde MCP 账本并可安全 checkpoint 回放。该证据验证来源候选查询和页面读取；它不证明真实模型已完成新闻相关性判断、独立日期判断或自然语言总结，RSS 时间仍不是已验证日期。
- **搜索/TLS：**同一固定 SearXNG 容器曾观察到 Google CSE 限流、DuckDuckGo CAPTCHA、Brave engine crash；Yahoo 本次搜索可用。对发布方页面的 HTTPS 请求使用默认严格证书验证成功；未关闭 TLS、未去掉 `time_range`，没有把聚合链接当成来源。不同搜索引擎/网站仍会变化，setup 仅能证明所需引擎加载，不能保证外网长期可用。
- **定向验证：**后续运行记录在[步骤 2 最新验证](../../../验证记录/Morde_Harness_最终流程测试用例.md#step-2-local-route-news-plan-2026-11-26)。源码 `morde-pc` 在隔离 profile 下启动并显示登录页；本机 Portal 7080/7180 与 DSH 未运行，未登录或发送对话。新闻端到端语义、普通问答账本、追问 Task/revision 权威关联与复杂计划恢复仍待验；B02-0 保持未勾选。本步骤没有启动其他任务。
- **2026-11-26 当前轮复验：**最新 App/Worker/MCP 搜索与路由定向组、补丁 10 项、离线 RSS 引擎 4 项和独立 PostgreSQL 路由/计划 16 项通过；公开服务健康检查与 Google News RSS 均为 HTTP 200，系统默认 TLS 校验通过。实际源码 Electron 在独立 profile 下仍停在登录页；本机无 Portal/DSH/SearXNG 监听，未发送消息或创建 Task。真实模型新闻总结、来源日期判断、追问目标绑定和计划恢复仍待有认证且连通 DSH 的实际 PC 现场证据，B02-0 不勾选。PostgreSQL 编译时发现的既有 H07 未提交实现只有两处 Go 短变量声明语法错误，本轮仅修复语法；H07 专项验收仍 pending。

- [x] **B02-1 本地多 Agent 协作与结果汇总（R03 本地范围）。**复用现有 DSH 委派与 root → coordinator → worker 两层持久子会话；独立模型回合并行，计划依赖按已结算子任务解锁，工具权限不超过直接父级。失败和 unknown 作为结果交回父级，未确认结果不报成功；重启后沿用原身份和 checkpoint 对账。
- [ ] **B02-1-PC 实际体验验收。**用实际 `morde-pc` 与真实模型验证语义委派、用户可见结果及 T10/T43；本地合成模型/工具不能替代。
- [x] **B02-2 本地工具选择、授权、预算、取消和恢复（R12 本地范围）。**本地回归覆盖 65 项 connector 目录通过固定选择工具访问、64 项模型工具定义上限、当前 Task 授权与 connector/schema 变更复核、Host/工具调用预算、重复调用提醒、checkpoint 工具集合恢复和 unknown 副作用不重放。底层执行停止无法确认时继续保持 unknown，不把 IPC 等待超时当作硬取消。
- [ ] **B02-2-PC 实际体验验收。**使用 PC/真实 DSH 和模型核验按语义选工具、用户可见授权/取消反馈及真实工具副作用；R12 整体不因本地测试勾为完成。
- [ ] **B02-3 受控 Skill 候选。**先完成 B02-1/2；C-02 的 R09 记忆 / 来源 / 授权合同稳定后实施 R15。核对现有 Skill 读取与 DSH 上游 provider 的接入、版本及隔离能力，适用时优先复用；候选只在授权范围内形成、验证、启用和撤销。
  - 验收：A01 覆盖未批准不可用、明确启用后可用、撤权即时失效、不同账号 / Task 隔离；插件兼容及许可证核对记录在案。新 schema 交 A。
  - **2026-11-26 步骤 8 继续开发：**在 Morde 现有个人对象 API/Store 中接入 `skill_candidate` 严格类型、格式校验状态、明确启用、启用版本不可改和撤销；Personal Data 页面提供候选编辑/格式校验/确认/撤销。复用既有 `nanomuse_skill_read`，上下文只列 active Skill 元信息，Broker 按当前 owner 获取正文，Task 事件记录版本/摘要，PostgreSQL 同事务复核 owner 对象仍为同一 active revision。定向领域、App、PostgreSQL 回归见[步骤 8 记录](../../../验证记录/Morde_Harness_最终流程测试用例.md#step-8-controlled-skills-memory-evaluation-2026-11-26)。未直接安装 `dsh-tool-skill`/filesystem provider：上游 consumer 有自己的 Session catalog/loader，但未接 Morde Broker/Task 账本；filesystem provider 也不满足目录隔离。官方上游仍标注 developer preview 且快速变化，本轮不升级；新集成仍使用 DSH 唯一 Runtime。
  - **仍待验：**R15 本地数据/API/Broker 切片已有代码，但 Portal 仅有组件/API mock；没有实际 `morde-pc`、真实 DSH/模型和跨重启端到端验收。B02-3 不勾选。
- [ ] **B02-4 交接给 C 和 A。**交出 Skill 候选 / 启用 / 撤销的可运行合同给 C-02 的 R16，并将协作图、资源归属和测试结果交 A-02。
  - **2026-11-26 步骤 8：**R15 的候选状态、owner/revision、Broker 读取与撤销栅栏已写入此 TODO 和[步骤 8 测试记录](../../../验证记录/Morde_Harness_最终流程测试用例.md#step-8-controlled-skills-memory-evaluation-2026-11-26)，C-02 可据此实施 R16；A-02 所需的工作图/资源与完整稳定快照证据尚未形成，故保留未勾选。
  - 验收：C 可在不等本文件所有 PC 实测关闭的情况下使用已稳定合同；未运行真实模型或 PC 的部分明确待验。
- [ ] **B02-5 资料来源与工具边界（R26）。**复核 `cloud_dsh_mcp_builtin.go` 及文件/检索工具是否能传递真实来源、可用位置和不完整信息，按工具自身现有合同补缺；将 Runtime 调整交 C、权限和跨层字段交 A。
  - 验收：T57/T59 的本地受控来源与伪造授权样本中，正常分析仍可执行，资料内命令不能增加权限；无法读取和定位如实反馈。真实模型和实际 PC 分开记录。
- [x] **B02-6a 本地子任务监视与可靠交接（M03-05）。**按[最终设计 L4](<../Morde 复刻 Muse 最终流程与开发设计.md>)复用子 Session、结算、后台 handoff 与恢复；普通进度不反复唤醒，成功、确定失败、取消、unknown 和必要阻塞均交回直接父 Session，父级持久 checkpoint 记录已处理结果。跨层持久身份/事务由 A、Runtime/checkpoint 由 C、用户可见终态由 D 负责；本地合同已用 Store + DSH 子进程验证。
- [ ] **B02-6b 实际体验与故障验收（M03-06）。**按 M03-06 在实际 `morde-pc`、当前服务和真实 DSH/模型中核验 T09–T12/T43、用户补充、结果呈现和恢复。统计比例不是验收门槛。
- [x] **B02-7 本地已授权 Skill/connector 选择（L1）。**`nanomuse_skill_read` 与 `mcp_discover` / `mcp_call` 只暴露当前 Task 可用能力；发现和执行分别重验授权，选择本身不授予权限、不创建 connector，也不要求普通问答进入 Task。Skill 候选/确认仍归 B02-3。
- [ ] **B02-7-PC 语义选择与缺失能力反馈。** PC/真实模型需验证语义选择、无匹配能力时的用户说明和授权内替代；App 门禁测试不能替代。
  - 验收：已有授权工具可被正确选择并完成任务，用户无需记住 Skill/工具名；无匹配、未选择、撤权、目录/调用不一致时，说明缺失能力，存在可用配置入口或授权内替代时给出具体选择，不能编造工具、静默换账号/目标或越权派发。App/Broker 回归与实际 PC + DSH/模型语义分开记录。
  - **本地回归（2026-11-26）：**App `SelectedMCP`、`BuiltinSearch`、`AgentChildSession` 和 Skill 相关定向组通过，覆盖懒发现、所选 connector、schema、撤权和调用门禁；该证据完成 B02-7 本地合同，不证明真实 DSH/模型会按语义选择工具或 PC 无匹配提示。

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
- **B02-1 整体未关闭；本地范围已完成，PC 项待验。**本地父级工具子集、会话隔离与 App/Worker/PG/Runtime 路径回归通过；仍缺真实 PC + DSH 两个 worker 的语义委派、用户可见汇总与资源冲突场景。是否启动后续并行任务按用户的步骤安排执行；本轮未开始。
- **B02-2 整体未关闭；本地范围已完成，PC 项待验。**64 项工具定义上限与重复调用提醒已做定向回归；预算及必需工具仲裁、重启后的工具集一致性、底层取消确认、unknown 副作用不重放的真实 PC/DSH 体验仍待验。
- **B02-3 未完成。**当前工作树已加入个人 Skill 候选同步类型、候选状态转移和 Broker 的 active Skill 读取，并通过域/App/定向 PG 回归；个人候选 UI、候选审核与跨账号/Task 的 PC + 真实 DSH 完整验收仍缺，不能据此关闭 R15。
- **B02-4 未完成。**缺少可运行 R16 Skill 合同交接与 A-02 协作/资源验收交接记录；等待 B02-3 合同稳定。
- **B02-5 未完成。**本地工具边界已有部分来源提示、日期校验和凭据过滤测试；T57/T59 正常与伪造授权样本、无法读取/定位时的端到端反馈及真实 PC/模型证据未完成。
- **B02-6 整体未关闭；B02-6a 本地已完成，B02-6b 待验。**Store 已有子 Session 结算、取消、子树隔离和 checkpoint 绑定的局部能力；仍缺真实 DSH/PC 的端到端 handoff、可回查事实和用户可见终态。
- **B02-7 整体未关闭；本地范围已完成，PC 项待验。**App/Worker 的选定 MCP、Skill schema、撤权和父级工具子集测试通过；仍缺真实 DSH/模型按语义选择已授权能力及 PC 对无匹配能力的用户反馈。
- **缺陷审计结论：**只读审计未确认可独立安全修复并关闭上述 TODO 的生产缺陷。曾提出的根 Session 委派工具集合可能扩大的疑点，经 `TestCloudDSHDelegateToolsAreRestrictedToParentDefinitions`、嵌套委派子集和不扩张已存储选择等定向回归未复现；不放宽任何授权门。测试主机/PC profile 与其他并行验收仍在协调，本轮没有远端写操作或 Store/PostgreSQL 集成操作。

### 2026-11-26 Step 5 本地自动化验证（最新）

- **应用工具治理：**在 `server/` 运行 `GOPROXY=off GOSUMDB=off go test ./internal/app -run '^(TestCloudDSHToolBroker|TestCloudDSHMCPBroker|TestCloudDSHMCPExternalCall|TestCloudDSHMCPApproval|TestSelectedMCP|TestBuiltinSearch|TestBuiltinWebRead|TestCloudDSHSkill|TestOfficeSkills)' -count=1 -timeout=8m` 通过（4.589s）。`TestSelectedMCPCallFailsClosedWhenSelectionChangesAfterDiscovery` 的 grant 撤销、connector revision 变化和 credential version 变化 3 个子用例均未触达 remote client、未创建 operation。另运行 `TestAgentChildSession` / MCP / budget 定向组通过（1.221s）。
- **Worker 与 Runtime：**Worker 定向的 64 定义上限、直接父级工具 allowlist、嵌套深度及 pinned tool-set 恢复组通过（2.265s）；`cd runtime/morde-cloud-runtime && node --test --test-concurrency=1` 全量通过（255/255，93.666s），包含重复调用提醒、依赖执行、取消隔离、unknown 不结算、handoff 恢复和相同调用不重复启动。`repeat-tool-reminder` 是 advisory，不阻止已授权调用。
- **PostgreSQL 并发与计划依赖：**使用仅监听 `127.0.0.1:55477` 的一次性 `postgres:16.15` 容器。`CH_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:55477/postgres?sslmode=disable' GOPROXY=off GOSUMDB=off go test ./internal/store/postgres -run '^(TestStep5IndependentAgentChildSpawnsPersistDistinctSessionsStepsAndEvents|TestStep5ConcurrentIdenticalAgentChildSpawnCreatesOneChild|TestStep5PlanBoundChildSuccessUnlocksAndUnknownBlocksDependencies|TestNestedAgentSessionsEnforceDepthSubtreeAndCancellation|TestNestedCancellationFencesAdmissionAndFinalDispatch|TestNestedCancellationRacesFinalDispatch|TestPostgresAgentChildPlanBindingIsAtomicVersionedAndIdempotent|TestPostgresTaskPlanStepChildBindingUsesSettledChildStep)$' -count=1 -timeout=10m` 通过（8 项，10.099s）；新增 3 项 `go test -race` 通过（6.350s）。测试后容器已停止并自动删除。
- **DSH 子进程集成：**同一 `CH_TEST_DATABASE_URL` 下，`go test ./internal/worker -run '^(TestCloudDSHNestedCoordinatorRunsIndependentWorkersConcurrently|TestCloudDSHNestedWorkerFailureReachesCoordinator|TestCloudDSHNestedCompletedEffectIsNotRepeatedAfterRestart|TestCloudDSHNestedCancelledBackgroundWorkerDoesNotRestart|TestCloudDSHNestedTaskCancellationLeavesIndependentTaskRunning|TestCloudDSHBackgroundOutputContinuationAfterLeaseReclaim)$' -count=1 -timeout=12m` 通过（6 个入口，19.383s；失败汇总含 3 个分类子用例）。证据覆盖双 worker 模型回合实际重叠、失败/unknown 逐级交回、后台输出 lease reclaim 后续读、取消隔离和已完成副作用不重放。
- **边界：**以上使用本地确定性合成模型/工具，不是 Provider 语义验收。共享 Host/Workspace Broker 操作仍串行；模型回合重叠不表示外部副作用并行。B02-1/2/6/7 的本地合同完成；B02-1-PC、B02-2-PC、B02-6b 及 R03/R12 的实际 PC/真实模型体验仍待验，未访问或修改远端。

### 2026-11-26 步骤 9：总体状态

- B02-1/2/6a/7 的本地合同与自动化已完成，PC/真实 DSH 子项仍未完成；B02-3 的个人 Skill 候选状态机、同步对象和 active read 已有本地实现及域/App/PG 回归，但 R15 完整用户审核与真实 Runtime 体验未通过。B02-0、B02-1-PC、B02-2-PC、B02-3/4/5/6b/7-PC 保持未勾选。

### 2026-11-26 当前工作树 B-02 复验

- **基线：**当前 `HEAD=07fecb1d`；本轮未修改 B-02 源码，也未连接远端或启动 Portal/DSH/PC。
- **复验：**`cd server && GOPROXY=off GOSUMDB=off go test ./internal/app -run 'AgentChildSession|SelectedMCP|BuiltinSearch|DSHSkill|CloudDSH.*Skill' -count=1 -timeout=8m` 通过（1.060s）；Worker 工具上限、父级 allowlist、嵌套委派和已存选择不扩张 4 项通过（0.847s）；`cd runtime/morde-cloud-runtime && node --test --test-concurrency=1 --test-name-pattern='nudges the Agent after three identical tool calls' test/runtime.test.mjs` 通过（1/1，1.598s）。
- **结论与待办：**未发现可在 B-02 文件边界内安全独立修复的新问题，不能因本地合同回归通过而勾选实际体验验收。B02-0、B02-1-PC、B02-2-PC、B02-3/4/5/6b/7-PC 仍需按各自验收条件补真实 PC、DSH/模型及稳定 Skill 交接证据；PostgreSQL/重启场景沿用上文已有记录，本轮没有重跑。

### 2026-11-26 步骤 2 本机新闻来源复验

- 完整 `TestBuiltinSearchIntegrationWithLocalSearxng` 在当前工作树通过（10.94s）。固定版本机 SearXNG 与 Morde MCP/Broker 完成 `!mgn` 新闻发现、`!yh` 精确标题/发布方查找及 `morde_web_read` 原文读取；返回 MIT News 直接 HTTPS 文章，正文标题匹配候选、正文可见日期与 RSS 日期候选同日。各操作的持久化结果已核对；首条搜索结果的 checkpoint replay 比对一致。测试后清理本机容器和网络。
- 这是本机受控工具/来源链路证据，不代表真实模型/DSH 已在 PC 端生成可靠新闻摘要，也不证明生产端搜索开启或外网长期稳定。较早的失败记录仍作为波动证据保留；B02-0 继续待真实模型与 PC 端来源展示验收。

### 2026-11-26 步骤 2 搜索出口只读复核

- Portal `/healthz` 为 HTTP 200；本机到 Google News RSS 的只读请求 HTTP 200、TLS 校验通过。服务器 `eth0` resolver 对 Google 首页的 DNS 回答被 ARIN 登记为 Meta 的地址，对 Yahoo / DuckDuckGo 搜索域名的回答被登记为 Twitter 的地址；`/etc/hosts` 无匹配覆盖，Google Public DNS over HTTPS 返回不同的官方服务地址。服务器 `news.google.com` 解析属 Google 地址范围但 443 超时；Bing 查询未返回可验证结果。服务器 `cloud_dsh.builtin_search.enabled=false`，SearXNG 为 `Exited (0)`。
- `dig` 直询 eth0 DNS `100.100.2.136/.138` 无 UDP 响应；直询系统级配置 `223.5.5.5/.6` 有响应但给出与公共 DoH 不同、彼此不一致的部分搜索域名地址。未切换 DNS，也未绕过服务商策略。
- 直接查询 resolver 显示 eth0 的 `100.100.2.136/.138` 无 DNS 响应；系统级配置的 `223.5.5.5/.6` 对搜索域名返回与公共 DoH 不同、且彼此不一致的 A 记录。未切换 DNS 或绕过服务商策略。
- 因此服务器并非完全断网，但当前存在 DNS 回答异常线索、搜索目标连接失败及内置搜索服务关闭；超时发生在 TLS 握手前，不能归因于证书验证。没有改远端 DNS/网络；生产新闻与 PC/DSH 语义验收保持待验，B02-0 不勾选。
