# Morde 任务中断与对话体验：实施任务 TODO

配套方案：《Morde_任务中断与对话体验_修复方案.md》
基线：`lizhongxuan/morde`，`release@936389c`。开始开发前在实际目标提交上核对入口。本清单不包含模型生成预算、上下文压缩或其他 Harness 重构。

**先合并状态修复，再逐步打开恢复。** 「没有明确操作和可用入口，就不能显示『等待你处理』」不依赖自动续跑完成。截图中的历史事故根因尚未确认，不阻塞下面的确定性修复。

| PR | 任务 | 依赖 | 建议责任 |
|---|---|---|---|
| 1 | T01 状态契约、T02 PC 状态与入口 | 无 | API / PC |
| 2 | T03 原对话回告、T04 PC 👌🏻 | T03 依赖 T01；T04 可并行 | Worker / PC |
| 3 | T05 恢复调度、T06 手动重试 | T01；T06 依赖 T05 的恢复资格判断 | Runtime / 数据库 / PC |
| 4 | T07 端到端验收与灰度 | T01–T06 | 测试 / 发布 |

## T01 · P0：区分「轮到用户」和「系统停住了」（状态路径已确认；截图对应调用仍待 ID）

**入口：** `server/internal/app/cloud_dsh_tool_broker.go`（`pauseRecovery`）、`server/internal/app/task_terminal_delivery.go`、任务详情 API。保留原 DB 状态和诊断记录，先新增兼容的展示语义，字段名按现有 API 惯例落定。

- [x] 定义五类状态：需要你操作、正在恢复、已暂停可重试、已中断、结果待核对。输出归类原因、合法操作（可空）及状态版本或事件序号。未知原因保守降级；不暴露内部工具名、原始错误、凭据。
- [x] 单测：只有**当前有效、当前用户可操作**的审批/澄清才是「需要你操作」；只有确有持久化恢复安排才是「正在恢复」；没有服务器确认的动作时，返回空操作。
- [x] 已确认通用状态路径：`CloudDSHToolBroker.pauseRecovery` 将无法安全确认的模型回复步骤写为 `unknown`，Task 进入 `waiting_user / dsh_recovery_unresolved`，并持久化允许名单内的失败分类；这会终止当前执行轮次，防止对未知操作盲目重放。
- [ ] 截图对应的具体调用仍待 Task ID：需查首个失败事件、模型账本、末个有效 checkpoint，判断是 provider/网络错误、回复校验失败、Runtime 恢复错误还是终态回告失败。截图只有标题和时间，不能据通用摘要断定是哪一种。

## T02 · P0：让 PC 标签和按钮始终匹配（实现完成；实机状态验收完成）

**入口：** `web/portal/src/routes/Assistant.tsx`、`Assistant.test.tsx`。现有中断 fixture `shows a recovery interruption without task action buttons` 可用作回归起点。

- [x] 任务卡、右侧详情和对话共用同一状态映射；把 `waiting_user / dsh_recovery_unresolved` 且无入口的场景显示为「已中断」或「结果待核对」；不显示「等待你处理」。
- [x] 真实审批/澄清在原对话尽可能就地操作；无权限、请求过期或已处理时不渲染可点击入口。服务端兼容回退只信有效的审批/澄清，不自行推断能重试。
- [x] `result_unverified` 保留在任务详情，但不再在原用户消息下重复渲染状态气泡或“查看任务详情”按钮。
- [ ] PC 测试：自动化已覆盖未知/过期状态、权限不足、快答与确认标记、刷新与事件游标缺口；实机已看到「结果待核对」且无普通重试入口。跨端同时操作与全部旧事件乱序场景尚未完成实机验收。

## T03 · P0：停止在原对话发内部日志，保证中断也能回告（实现完成；新闻 E2E 待补）

**入口：** `server/internal/worker/task_terminal_delivery_worker.go`、`server/internal/app/task_terminal_delivery.go`、PC 原对话任务事件订阅/重载。

- [x] 主对话用服务端确认的状态显示「拿没拿到结果、接下来由谁做、能点什么」。移除失败/等待消息里的步骤计数、通用「对账」和内部名字；诊断留任务详情。正常结果与现有验收证据保持绑定。
- [x] 等待状态的无工具说明模型接收事件中的 allowlist 失败分类或已确认的 Runtime 停止/租约过期事实；不向模型传 provider 原文、工具名或内部错误码。无法取得分类时明确说记录不足以判断原因。
- [x] 说明模型调用结果未知时，使用同一 outbox 来源持久化并发布一条确定性系统说明，不重派模型请求；独立记录回告类型，允许模型结果晚到时结算用量但不覆盖已发布说明。
- [x] 验收：新闻任务没有已核实结果时，只说「这次没能拿到已核实的新闻，任务已中断」；不可用的「再试一次」不显示。只有已登记真实恢复时才说「我会再查一次」。自动化用例覆盖无核实结果和无恢复安排；实际新闻查询 E2E 尚未完成。

## T04 · P1：把 👌🏻 接回原请求气泡（实现完成；跨页实机回归待补）

**入口：** `web/portal/src/routes/Assistant.tsx`、`server/internal/store/postgres/task_revision_start.go`、`conversation_route_actions.go`。

- [x] PC 渲染服务器确认的 `agent_acknowledged`，无障碍名称「已接下后台任务」。快答、只排队、未开始、别人的消息都不标。
- [x] 先核对合并、修订及重复投递是否能产生多个确认；持久化唯一展示锚点，PC 刷新会话消息，不新加确认 API。PC 自动化和 PostgreSQL 路由集成用例覆盖原消息绑定及重复确认约束；分页/重开完整实机回归仍在 T07。
- [ ] 验收：立即能给结果时直接回答；确认为耗时后台任务后打 👌🏻。实机已观察到原消息的无障碍标记；快答与后台耗时任务组合的真实端到端场景待补。需要报计划时只发一句有用的话，不把 emoji 写进助手正文。

## T05 · P1：开发一次可持久恢复的只读任务路径（实现与完整搜索恢复 E2E 已通过）

**入口：** `server/internal/worker/cloud_dsh_executor.go`、`cloud_dsh_session_tree.go`、现有模型分类/账本和 PostgreSQL 的任务事件。优先复用已有持久恢复机制；只有确实无法表达才新增记录。

- [x] 分三路：已有结算结果就回放；明确可重试且服务端证明只读，才登记一次自动续跑；效果、用量、checkpoint 或权限无法确认就不派发。只读判定基于实际工具集合与账本，不看任务名称或模型声称。
- [x] 恢复申请原子绑定 Task ID、修订、授权版本、失败事件/调用、checkpoint 序号和摘要；同一修订累计自动额度首版 1 次，Worker 重启、不同失败事件和多 Worker 竞争不刷新额度。
- [x] Worker 获得合法租约后再次校验取消、授权、修订、checkpoint 和账本。从已验证位置续跑，复用已确认结果；确实需要新一轮模型调用才使用新的受控尝试身份。不得扩大工具权限、自动换模型或绕过现有预算。自动恢复开关默认关闭。
- [x] PostgreSQL + 模拟模型完整集成验收：「搜索已结算 → 下一次模型临时失败 → 重启 Worker → 同一 Task 恢复 → 原对话交付」。同一 E2E 用例断言第二个 Worker 以新 generation 续跑、搜索 operation 只派发一次、Task 完成，并将最终答复只交付到原会话。

## T06 · P1：接上真实的手动入口（实现完成；实机点击待补）

**入口：** 先检查现有 Task 恢复 API；PC 对未关联任务的消息路由失败已有资格校验和重试 UI，可借鉴界面，但不能调用那个接口去恢复已关联的中断任务。不要把已有「工具步骤重试」当作模型恢复接口。

- [x] 复用 T05 的资格判定与持久调度；增加 Task 级动作。点击重新校验用户、修订和授权，以同一任务继续，返回当前持久状态。
- [x] 自动化覆盖幂等请求、重复/竞争认领、过期版本与已认领状态；结果未知时无普通重试入口。实机已确认结果待核对状态不显示重试按钮；安全重试按钮实机点击与两端并发路径待补。

## T07 · P0：合并和灰度门槛（进行中；发布验收未完成）

- [ ] 待验证：已补齐 `dsh_recovery_unresolved` 的原对话安全中文兜底；待服务器恢复可访问后，用实际 `morde-pc` 验证截图对应任务的失败分类、终态回告与气泡可见性。服务端历史终态回告已因会话凭据过期被作废，无法由客户端重放。
- [x] 隔离工作树回归：`go test ./internal/api ./internal/app ./cmd/portal -count=1`；恢复专属 Worker 测试（含完整搜索恢复 E2E）；本机 PostgreSQL `go test ./internal/store/postgres -run 'Test(DurableRecoveredToolAdmission|PostgresTaskModelRecovery)' -count=1`；DSH Runtime 恢复启动协议测试；PC `npm test -- src/routes/Assistant.test.tsx`（289/289）。2026-11-28 当前结果与命令见下方记录。
- [ ] 用打包 PC 完成端到端回归，保存目标 commit、任务/事件关联、三种页面状态与实际派发次数；全量 Worker 包仍未通过，2026-11-28 的具体失败见下方记录，修复/补齐对应测试环境后重跑。
- [x] PostgreSQL + 两个独立 Go Worker 进程的恢复正例：首进程完成只读搜索、确认零用量失败并持久化计划后被强制终止；新进程从 PostgreSQL 恢复同一 Task，校验 lease/generation 与恢复 call ID 更新、搜索只派发一次、模型读到已结算结果、原会话只交付一条助手答案及一个 `task.result`。
- [ ] 真实 `morde-pc` 交互尚未稳定通过：本轮 `make pc` 构建并启动客户端后，隔离旁聊的无工具快答一直停留在连接状态，未收到助手回复；早先一次快速问答通过不代表当前连接稳定。仍需确认未知结果只保留在任务详情、原对话只显示自然语言说明，且终态回告模型未知时仍能收到确定性说明。
- [ ] 正例：快答无 👌🏻；耗时任务一次 👌🏻；可重试故障自动恢复后出结果；耗尽后原对话收到说明；真实审批能继续。
- [x] 双 Worker 同时竞争同一恢复尝试：PostgreSQL 用例断言恰好一个成功、另一个得到冲突；租约重启后不会再消费同一尝试额度。
- [x] PostgreSQL 负例：已排队恢复后撤销授权版本、取消 Task 或破坏绑定的 checkpoint 历史，均拒绝 Begin；计划不进入 `attempting`，不写恢复模型调用账本/响应。
- [ ] 其余反例：未知结果/用量禁止自动计划的完整 PostgreSQL/Worker 派发断言仍缺（目前是分类/资格单测）；工具 allowlist 已有闭合集测试，写入结果未知已有独立进程恢复 E2E，但尚未与模型自动恢复资格合成在同一个 E2E。
- [x] 恢复诊断输入：服务端从等待事件提取白名单失败事实，终态解释模型请求只收到自然语言诊断、不包含工具名/调用 ID/内部分类；应用与 PostgreSQL 集成测试覆盖。
- [x] 关闭门保护：旧进程已排队但未启动的自动恢复在新进程关闭开关时，原子转为 `waiting_user / retry_available`，不消耗尝试、不派发模型调用，并保留手动 retry；应用与 PostgreSQL 回归覆盖。
- [ ] 独立灰度 cohort 与可比较指标仍未实现；当前只有 Portal 进程级开关，没有独立 Worker/用户分组或专属低基数指标。生产自动恢复保持关闭，不因隔离 E2E 打开线上恢复。

## T08 · P0/P1：实时对话、真实失败说明与 Agent 接管（进行中）

**设计依据：** [Morde 实时对话与 Agent 接管设计方案](Morde_实时对话与Agent接管_设计方案.md)。继续只使用 DSH Agent Runtime；OpenClaw 仅参考持久状态、真实文本增量和安全进度的处理方式。

- [x] 移除 PC 对 `dsh_recovery_unresolved` 本地生成的固定助手回复；没有实际助手消息时展示任务状态卡和详情入口。
- [x] 修复消息列表未完成首屏加载时丢失已受理 receipt 的缓存时序；receipt 出现后立即显示用户消息及 pending/processing 状态。覆盖“发送后回执”与“已加载的 pending/processing”自动化用例。
- [x] 终态投递错误日志记录稳定 `failure_code`、stage 与现有追踪字段，不记录原始错误文本；增加安全字段单测。
- [x] 增加 receipt 到 route claim 的等待时长，以及 DSH route 分类耗时直方图；使用无 ID 标签的低基数指标。
- [ ] 已部署后端诊断日志并读取到线上 `read_source_facts / invalid_argument`。根因是普通长度的 Task 目标带结尾换行，模型摘要校验拒绝；本地修复及隔离 PostgreSQL 回归通过。待发布修复后确认 DSH 收到白名单事实，并只向原会话发布一条有真实原因差异的解释气泡。
- [x] 路由模型兼容返回内置 search/read tool 意图时，统一转成普通 DSH Task 并请求受理标记；不再把 Broker 原始输出直接发布成助手答复。正常 Task 由 DSH Agent 执行工具和综合结果，失败走 DSH 安全事实终态回告。应用解析与 PostgreSQL 路由用例通过；实际 PC 工具派发与最终合成回复仍待验收。
- [ ] 对普通短答完成 DSH 实际回复延迟验收并记录收据、claim、分类、首条持久 assistant 消息的时间。当前只有 receipt 和分类耗时指标，没有 first-delta 持久流；需实现有界增量写入及断线续读后再验收流式速度。
- [ ] 在实际 `morde-pc` 验证普通问答真实模型回复、一个只读工具请求和一个明确后台任务的进度/最终结果；检查现有 DSH 工具、MCP、子 Agent 和授权边界，不增加第二套 Agent Runtime。

### 2026-11-28 本轮验证

- 通过：PC Portal `npm test -- src/routes/Assistant.test.tsx`（330/330）；`npm run typecheck`；Go Worker 定向日志与 route 延迟指标用例；`git diff --check`。
- 未完成：本地 PostgreSQL 终态回告集成测试当前环境未提供可确认的测试数据库，定向命令没有运行完整数据库路径；需用隔离 PostgreSQL 重跑。
- 未完成：线上当前为 `release@ee618bed`，Worker 的终态错误持续发生在 `read_source_facts`。旧日志没有 `failure_code`；新字段尚未部署，不能猜测或宣称已修好该根因。
- 未完成：本轮尚未实机启动并操作 `morde-pc`，也未生成新对话或测试请求；真实 AI 答案、端到端延迟和工具接管尚未验收。

## 实施与验证记录（2026-11-27）

- 实施基线：`release@936389c`。本地实现覆盖 T01–T06；自动恢复通过 `MORDE_TASK_MODEL_RECOVERY_AUTO` 独立控制，默认关闭。
- 通过：`make pc`（PC portal/workbench/desktop 构建通过）；本机 `morde-pc` 实机可见未确认结果为「结果待核对」、只提供任务详情入口；已接下任务的原始用户消息带无障碍名称「已接下后台任务」。
- 通过：隔离工作树基于 release 基线和本次改动运行 API、app、Portal Go 包测试，及四个恢复 PostgreSQL 集成用例；恢复专属 Worker 测试通过。PC `npm test -- src/routes/Assistant.test.tsx` 为 289/289，`git diff --check` 通过。
- 未通过：`go test ./internal/worker -count=1` 的全量 Worker 包测试中，多个需启动 DSH Runtime 的用例在首帧前握手失败（`account Agent Runtime handshake is invalid`）；恢复专属测试本身通过。需在 Runtime 可启动的环境重跑全量套件。
- 共享工作区含未跟踪的模型预算改动，直接构建还会报 `model_budget_preparation.go` 重复定义 `bytesEqual`、未定义 `providerDigest`，以及 `model_usage.go` 未定义 `modelBudgetDigestPattern`。这些文件不在本 TODO 范围，未改动或纳入本次提交；隔离工作树结果用于区分本次改动和这些工作区改动。
- 服务器发布（2026-11-27）：后端 `release@90529697` 通过部署门禁（P0 5 pass、0 fail、5 exception），数据库迁移到 263，服务重启后 `/healthz=ok`、`/readyz=200`。例外为服务器未配置 `CH_TEST_DATABASE_URL` 的 T01/T26/T36，以及需要真实 PC/DSH 的 T10/T43。前端补丁 `e062db21` 拉取后按 lockfile 安装依赖，在 staging 构建并切换 PC/admin 静态资源；旧资源保存在 `/opt/morde/backups/web-portal-dist-pre-e062db21`。后端二进制仍对应 90529697，e062db21 只补齐其依赖的 SSE helper 与测试。
- 最终 PC 检查：`make pc` 构建并启动实际客户端；任务卡显示「结果待核对」且只有「查看任务详情」，原始用户消息保留「已接下后台任务」无障碍标记。此检查未新建任务或重放操作。
- 未完成门槛：截图事故仍未定位；尚未完成“搜索已结算、模型临时失败、Worker 重启、恢复且不重复搜索、原对话交付”的完整 PostgreSQL/模拟模型 E2E；快答/慢任务完整新闻路径、旧事件乱序、跨端并发重试、实机点击安全重试与灰度数据比较待继续。故 T07 暂不标完成，不能把本次实现描述为全项验收通过。

## 实施与验证记录（2026-11-28）

- 验证基线：隔离工作树 `/tmp/morde-t07-validation-032d0151` 基于 `release@032d0151`，只叠加本次恢复输入保护改动；共享工作区其余未提交文件未纳入验证或提交。
- 通过：API、app、Portal：`go test ./internal/api ./internal/app ./cmd/portal -count=1`。恢复专属 Worker 组（包含完整 E2E）：`CH_TEST_DATABASE_URL=... go test ./internal/worker -run 'Test(CloudDSHReadOnlyRecovery|CloudDSHModelRecoveryStartRequiresExactAttemptAndConfirmedResponse|CloudDSHModelResultErrorForCallKeepsOnlyMatchingNoDispatchEvidence|CloudDSHModelResultErrorForCallKeepsOnlyMatchingProviderRejectEvidence|CloudDSHModelRecoveryResumesAfterSettledSearch)$' -count=1`。本机隔离 PostgreSQL：`go test ./internal/store/postgres -run 'Test(DurableRecoveredToolAdmission|PostgresTaskModelRecovery)' -count=1`。
- T05 完整 E2E 通过：合成只读搜索结算一次，随后模型模拟零用量 429；原 Worker 停止，另一个 Worker 以新 generation 认领并恢复同一 Task；持久搜索结果被重放而没有第二次派发；最终答复写回原 conversation 一次。恢复期间若 checkpoint 后新增追问或 steer，Worker 拒绝沿用旧恢复调用身份；对应两个反例单测通过。
- 通过：Runtime 恢复启动协议 `node --test --test-name-pattern='account runtime accepts a fenced model recovery start' test/runtime.test.mjs`；恢复预算身份 Runtime 组 `node --test test/model-budget-recovery-runtime.test.mjs`（3/3）；PC Portal `npm test -- src/routes/Assistant.test.tsx`（289/289，既有验证）；`gofmt` 与 `git diff --check` 无输出。
- 未通过：全量 `go test ./internal/worker -count=1`（337.8 秒）。本次干净隔离树报告三个失败：模型预算 PostgreSQL/HTTP E2E 的响应账本与最终 DSH frame identity 不匹配；A30 PC bridge 用例缺少隔离工作树生成的 `clients/desktop/harness/dsh-nanomuse/lib/cloud.js`；终态文案断言与共享工作区未提交的 `task_terminal_delivery.go` 文案调整不一致。恢复专属 Worker/PostgreSQL 定向测试仍通过；该整包门槛不计为通过。
- PC 实机：本轮 `make pc` 的 Portal、workbench 和 desktop 构建均成功，实际 `morde-pc` 窗口打开。新旁聊的无工具快答提交后持续停留在连接状态，未见助手回复；此前一次快答曾通过，本轮不能复验为稳定。没有在该旁聊重试，也没有重放历史任务。
- 服务器：本轮首次 `/healthz` 曾短暂返回 `ok`；随后 `/healthz` 与 `/readyz` 均超时，SSH 多次在 banner 阶段超时，无法核对服务器运行 commit、迁移或日志。因此不记录为本轮已部署版本，也不在服务器改库或操作未确认任务。
- 灰度：`MORDE_TASK_MODEL_RECOVERY_AUTO` 继续默认关闭；未对线上任务执行重放或状态变更，尚无可比较的灰度指标。
- T07 仍未完成：PC 快答/工具搜索路径没有稳定通过；全量 Worker 套件、剩余正反例、真实审批/并发入口、部署 commit 核验、灰度指标与截图事故 Task ID 仍待完成。不得据隔离 E2E 将整体验收或灰度标为通过。

### 2026-11-28 续验（用户告知服务器已重新部署后）

- 当前共享工作区完整 Worker 回归：HEAD 为 `release@e689a8e8`，但工作区包含未提交的模型预算、长上下文、配置与部署改动；在独立临时 PostgreSQL 17 容器中运行 `go test ./internal/worker -count=1`，338.8 秒后失败两项：`TestConversationSummaryDSHMainAndSideChatsContinueAfterLongHistoryRestart`（第二次 compaction 返回 summary version 3）与 `TestConversationRouteWorkerTriggersSummaryByTokenPressureBeforeTransportWatermark`（未提交 summary）。其余 Worker 用例通过，包含 T05 重启恢复 E2E；模型预算账本/DSH frame 双 ID 问题在此共享工作区重跑未再失败。由于运行树有未提交改动，这不算 `release@e689a8e8` 的干净整包通过；两项失败涉及长上下文摘要，按本文范围说明不纳入 T07 缺陷修复，整包门槛仍保持未通过。
- 服务器重查：公开 `/healthz`、`/readyz` 和站点根路径的 TLS 握手均在 8 秒超时；到 `120.25.178.93:22` 与 `:443` 的 TCP 建连成功，但 SSH banner 与 TLS 握手没有响应。当前无法确认实际部署 commit、迁移、服务日志或运行时灰度开关值，也未改远端数据库或任务。
- PC 实机：实际 Electron `morde-pc` 窗口继续显示「网络异常，部分对话数据暂时无法读取」和「连接中…」；未见新回复。本次没有创建/重试线上任务，也没有点击审批或恢复操作。
- 灰度审查：自动恢复源码默认开关仍关闭，且为进程级开关；代码没有专用灰度指标埋点。服务器当前开关值、可查询的任务/用量样本和基线指标均不可读取；不能把“无样本”或本地 E2E 当灰度结论，也不在共享服务器上开启可能影响所有符合条件任务的恢复。
- 后续门槛：服务器本地 health/ready、systemd 与日志确认部署 commit 后，再运行真实 PC 快答/搜索、审批与安全恢复；灰度前先有隔离 cohort 或独立 Worker 和可比较的状态/回告/恢复/额外调用基线。T07 保持进行中。

**完成标准：** 用户不用打开任务详情，也能知道 Morde 是否接下任务、最后交付了什么，卡住时轮到谁、有没有下一步。

### 2026-11-28 续验（无助手消息气泡）

- 定位：截图对应的模型结果未知任务没有已发布的助手消息；旧终态回告因原用户会话凭据过期而作废。客户端的兜底此前还依赖唯一确认消息锚点，可能因此被隐藏。
- 修复：PC 在同一会话中按当前账号、任务发起人和原始任务消息定位失败说明；对 `dsh_recovery_unresolved` 展示安全中文气泡。确认标记有歧义时仍显示只读说明；已有已发布助手消息时不重复。任务详情请求失败时使用安全任务摘要兜底。
- 自动化通过：共享工作区 `npm test -- src/routes/Assistant.test.tsx`（318/318）、`npm run typecheck`；`make pc` 重新构建通过。干净基线上的故障气泡相关用例 10/10 通过；整文件仍有 2 个与本次无关的任务动态批处理断言失败，在未改动的 `release` 基线上也同样失败。
- 实机/发布未验收：构建后服务器 `https://portal.120.25.178.93.sslip.io/healthz` 在 5 秒超时，SSH banner 在 8 秒超时；新启动 PC 无法续期会话，不能读取截图任务或验证新气泡，也未对旧任务执行重放。本次改动尚未确认部署到服务器。
- T07 继续进行中：实际 `morde-pc` 故障气泡可见性、稳定快答/搜索、Worker 全量失败项、安全正反例及灰度门槛仍未完成。

### 2026-11-28 续验（诊断气泡与双 Worker 竞争）

- 基线：当前分支和 `origin/release` 均为 `release@0f83fab0`；该提交已包含双 Worker 恢复尝试竞争测试。本轮只增加安全诊断传递测试与本 TODO 记录，未暂存共享工作区其他未提交文件。
- 通过：`go test ./internal/app -run '^TestTaskTerminalDeliveryRecoveryDiagnosis' -count=1`；本地 PostgreSQL `go test ./internal/store/postgres -run 'TestPostgresTaskTerminalDeliveryRecoveryDiagnosisReachesModelFacts|TestPostgresTaskModelRecovery' -count=1`；同一恢复 fence 的并发竞争 `go test -race ./internal/store/postgres -run '^TestPostgresTaskModelRecoveryPersistsAndConsumesOneStableAttempt$' -count=1`；Worker 重启搜索恢复 E2E `CH_TEST_DATABASE_URL=... go test ./internal/worker -run '^TestCloudDSHModelRecoveryResumesAfterSettledSearch$' -count=1` 均通过。Portal 故障回复气泡定向测试 6/6 通过。
- 全量 Worker 未通过：`go test ./internal/worker -count=1` 用时 341.3 秒后失败。定向复跑确认 `TestConversationSummaryDSHMainAndSideChatsContinueAfterLongHistoryRestart` 在第二次压缩得到 summary version 3；`TestCloudDSHModelBudgetPostgresHTTPWorkerProcessKilledBeforeProviderHTTP` 的 `MarkStaleModelCallsUnknown` 返回 0（期望 1）。整包门槛仍未通过；前者属于长上下文压缩，后者属于模型预算恢复，不纳入本修复范围。
- 共享工作区前端最新检查：Portal 文件测试 330/330，`npm run typecheck` 通过。工作区含未提交的步骤输出分页/API 改动；这组检查不能代表干净 release 基线，本轮未将这些文件纳入提交。
- 服务器当前仍不可核验：`/healthz` TLS 握手超时，SSH banner 超时；不能据用户告知推断部署 commit 或查看日志。实际 `morde-pc` 的会话续期此前也失败，因此线上对话与截图任务仍未实测。现代 `morde-pc` 将 Portal UI 嵌入客户端包；服务器部署不等于已更新客户端包。
- 自动恢复保持默认关闭；开关为进程级、没有灰度 cohort 或独立恢复指标。T07 继续进行中，等待服务器连通和共享前端未提交改动收敛后再完成 PC/灰度验收。

### 2026-11-28 续验（实时对话设计与 PC 状态核对）

- 设计依据已落盘：[Morde 实时对话与 Agent 接管设计方案](Morde_实时对话与Agent接管_设计方案.md)。实际 `morde-pc` 确认旧固定助手气泡已被移除；多条相同状态卡分别关联不同的用户消息。任务详情仍只有通用模型服务未确认状态，真实失败解释没有发布。
- 服务端只读核对：终态投递仍卡在 `read_source_facts`，服务端为 `release@ee618bed`。Task/outbox 身份字段、修订与授权版本、事件类型及事件摘要相符；步骤计数自洽。当前错误日志没有安全 `failure_code`，不能据此猜根因。
- 本地 DSH 架构审计：`WorkerModel.stream()` 虽然消费 DSH 增量 chunk，但 coordinator 适配器将 chunks 全部收集后才返回；ConversationRoute 等完整结构化决策后才创建助手消息。现有 Task 流消息必须绑定 Task ID 和 Task event fence，因此还不能承载 taskless 普通对话回复。真实持久流式回复需要单独的 owner/conversation/message/revision 围栏协议、API 重连游标及 PC 接入。
- 本轮已通过：`go test ./internal/api ./internal/app ./cmd/portal -count=1`、终态安全错误码及 route 指标定向 Worker 用例、`npm test -- src/routes/Assistant.test.tsx`（330/330）、`npm run typecheck`。部署 smoke gate 正在执行，结果待补。
- 待完成：部署稳定错误码并读取线上诊断；定位并修复 facts 读取根因；实际 PC 新会话真实短答与只读工具请求；端到端时间样本；DSH 持久增量方案和授权/断线回放验收。T07/T08 保持进行中，自动恢复仍关闭。

### 2026-11-28 续验（终态回告根因与修复）

- 部署核验：服务器运行 `release@b363bd38`，迁移、`/healthz=ok`、`/readyz=200` 通过。Smoke gate 结果为 5 pass、0 fail、5 exception；例外仍是服务器无 `CH_TEST_DATABASE_URL` 的集成项和需要真实 PC/DSH 的场景。
- 根因：新增安全错误码显示回告 Worker 在 `read_source_facts` 返回 `invalid_argument`。只读检查未打印账号、Task ID 或用户内容；任务围栏、模型 ID、步骤计数等均正常。触发任务目标有末尾换行，而短目标原样进入严格文本校验，导致事实摘要被拒绝，DSH 从未收到解释请求。
- 修复：模型输入摘要始终去除 Unicode 首尾空白；原始 Task 目标 SHA-256 仍保持不变。新增覆盖聊天换行、Unicode 空白、UTF-8 截断及空白目标的单测，并扩展 PostgreSQL 恢复诊断用例验证换行目标可生成事实摘要且摘要哈希绑定原文。
- 本地验证：`go test ./internal/store/postgres -run 'TestBoundedTerminalDeliveryObjective|TestPostgresTaskTerminalDeliveryRecoveryDiagnosisReachesModelFacts' -count=1` 通过；隔离 PostgreSQL 17 下 `go test ./internal/store/postgres -run 'TaskTerminalDelivery' -count=1` 通过（38.9 秒）。
- 尚未完成：修复提交并部署后的真实 DSH 终态解释、实际 morde-pc 新短答/只读搜索及延迟样本；直接回复仍没有持久增量流。T07/T08 继续进行中，自动恢复仍关闭。

### 2026-11-28 续验（路由输出与慢消息刷新）

- 根因补充：线上上一版安全日志显示 route completion 无效，最终分类为 `routing_action_unavailable`；旧记录不能细分 JSON/字段类型等结构原因。解析器对 `tool_arguments` 要求对象值，旧提示没有给直接工具一份与该合同一致的示例。
- 代码处理：按 action 明确可省略无关字段；新增普通回复、新任务及直接搜索 JSON 示例；保持严格 parser 和工具授权边界。无效输出只增加白名单结构类别，不保存或输出模型原文。route worker 将无效模型决策归为输入/格式错误，避免误记为内部故障。
- PC 刷新处理：消息列表存在未完成请求时不重复触发每秒失效查询，改为在请求结束后安排一次刷新；否则慢请求可能一直被下一轮查询取消，已有的持久终态消息要等切换会话才显示。
- 本地通过：`go test ./internal/app -count=1`；Worker route 无效输出安全日志和一次修复后兜底定向用例；`go test ./internal/store/postgres -run 'TestValidStoredRouteDecision|TestConversationRouteModelDecisionSemantic' -count=1`；PC 会话页 `npm test -- --run src/routes/Assistant.test.tsx`（331/331）；`npm run typecheck`；`git diff --check`。
- 服务器当前 SSH 可用且 `morde-portal.service` active，运行源仍是 `release@1d65dba8`，尚未包含本轮改动。实际 `morde-pc` 窗口仍显示旧测试会话状态；本轮代码尚未发布，不能据此判定真实路由、搜索、Agent 执行或延迟已修复。
- 待部署后验收：实际 PC 新建对话分别执行无工具快答、直接只读搜索和一个 DSH 后台只读任务；记录 receipt、route claim、分类、工具派发和第一条持久助手消息的耗时；检查消息在原会话自动显现。失败时依据新增安全错误类别继续定位，不重试旧的不确定任务。持久增量回复、Worker 全量包、旧任务安全反例与灰度基线仍未验收；T07/T08 保持进行中。

### 2026-11-28 续验（真实 PC 路由与内置搜索）

- 已部署基线：`release@30695113`，服务迁移到 280，`/healthz=ok`、`/readyz=200`；部署 smoke gate 5 pass、0 fail、5 exception。例外是服务器缺少 `CH_TEST_DATABASE_URL` 的集成场景及必须在真实 PC/DSH 验收的场景。
- 实际 `morde-pc` 新旁聊快答成功：真实模型在用户提交后直接回复指定短句；接收与下一次客户端读取之间已出现完整答案，观察到的可见延迟低于 1 秒。该样本只证明快答链路本次成功，不能代表延迟分布或持续 token 流。
- 实际 PC 只读搜索先失败。模型路由成功，创建一个隐藏 `morde_web_search` Task；只读 metadata 显示其只有一个 `tool:failed` 步骤、工具调用数为 0，且没有 MCP operation 记录。根因是可信内部 Task 的持久 revision 为 0/0，但 claim 投影为 nil/nil，Broker 错误地按普通任务拒绝。
- 本地修复：只有 `InternalTool` 且 revision 两侧都缺失时使用其持久 fence revision 0；普通 Task 仍要求 revision 已存在且一致。实际 Task 存储集成用例验证隐藏 Task 从路由写入、队列 claim、单次 Broker 调用到原会话助手消息完成；本地 PostgreSQL 通过 `TestConversationRouteDirectToolPersistsHiddenBrokerTask`。还通过 `go test ./internal/app -count=1`、`go test ./internal/worker -run 'Test(CloudDSHModelBudget|ConversationRouteInternalTool|CloudDSHCoordinatorRunsMatchingTextOnlyTurn)' -count=1`、`go test ./internal/store/postgres -run '^TestConversationRouteDirectToolPersistsHiddenBrokerTask$|^TestPostgresAdaptiveCompactionBudgetPlannerUsesSummaryStageAndSettlesOneOutputCap$' -count=1`、`git diff --check`。
- 同轮核对 DSH compaction 模型预算协议：Runtime 会声明 `purpose=compaction`，Portal adapter 丢掉它造成摘要被短答 stage 预算。现改为只允许 `compaction` 枚举并转成 summary stage；PostgreSQL 集成用例通过。此项修正摘要预算，不宣称修复完整长会话质量。
- 下一步：部署内部工具 revision 修复，再用 `morde-pc` 在全新旁聊重跑只读搜索，并跑一条真实 DSH 长任务；核对工具账本恰好一次、搜索结果由模型综合后回到原对话、任务状态/结果一致、消息在当前会话自动显现。随后记录端到端阶段时间。当前补丁尚未部署，T07/T08 继续进行中；Taskless 持久增量流、Worker 全量包、旧任务安全反例与灰度 cohort/基线仍未验收，自动恢复保持关闭。

### 2026-11-28 续验（搜索由 DSH Agent 总结）

- 设计决策：移除新请求的“路由模型选工具 → Broker 原样回发”捷径。公开搜索和网页读取统一创建普通 DSH Task，让 Task Agent 调用现有授权工具、核实来源并组织答案；失败由现有 DSH 安全事实终态回告链路解释并回到原对话。工具意图转为后台任务时立即请求确认标记，用户可见受理状态；确认标记不是完成答复。
- 修复：结构化路由即使收到兼容旧模型的 `action=tool` 结果，也只在上下文完整且参数合法时转为 `action=new`；不持久化隐藏直连工具 Task、不展示工具原始输出。上下文不完整或参数非法仍澄清；普通文字快答保持单次 DSH 路由。
- 自动化通过：`go test ./internal/app -count=1`（34.5 秒）；PostgreSQL `go test ./internal/store/postgres -run '^TestConversationRouteModelToolDecisionRoutesLookupToDSHTask$' -count=1`；Worker `go test ./internal/worker -run '^(TestDSHSearchThenReadCompletesThroughWorkerWithSourceDate|TestDSHSearchToolCompletesThroughWorkerAndPersistsOperation)$' -count=1`。PostgreSQL 用例确认工具意图写成正常队列 Task、请求了确认标记、没有隐藏 direct-tool Task；Worker 用例确认 DSH 搜索/读取可实际合成带来源答案并持久化一次工具操作。
- 服务器只读核对：`morde-portal.service` active、healthz 返回 `ok`，但 `/opt/morde-src` 当前仍是 `release@8c46fee0`，尚未包含本轮改动。未向旧会话重放失败任务；本地分支 HEAD 为 `release@ca897b29`。
- 待验收：合并并部署本轮路由修复后，用实际 `morde-pc` 新建旁聊执行一个真实只读搜索和一个普通快答，观察收到状态、正常 DSH Task、MCP operation 恰好一次、AI 综合答复回到原会话，并记录阶段时间。目前没有真实 PC 证据，也没有实现 Taskless 持久 token 流；因此不宣称搜索回复已修复或延迟已满足。
- T07/T08 继续进行中；错误气泡/真实 AI 失败解释、PC 快答与搜索端到端、延迟样本、Worker 全量失败项、安全恢复反例及灰度 cohort/基线仍需分别验收。自动恢复保持关闭。

### 2026-11-28 续验（PC 重复失败说明与 Task 预算拒绝）

- 设计方向：继续使用唯一的 DSH Agent Runtime。普通问答仍走 DSH 对话；搜索/网页读取由 DSH Task Agent 实际调用已授权工具、核实来源并生成答复；失败回告由 DSH 根据安全事实组织自然语言。PC 收到终态后有界补读原会话消息，避免 Worker 晚于 Task 状态事件写入的 AI 回告一直不可见。
- OpenClaw 只读审查完成：参考其 admission receipt、run-scoped typed status/delta/final/error、真实文本流、错误与成功答复隔离、幂等失败报告及重连快照；不复用 OpenClaw 的 Agent loop、provider 或工具 Runtime。Morde 还需为 Taskless 直接答复设计自己的持久消息版本与续读游标。
- 实际 PC 证据：上一条新旁聊短答由真实模型针对问题生成，用户提交后观察到的可见延迟低于 1 秒。重复的失败气泡关联两条分别提交的同文请求，不是同一助手消息被复制。另一次新的只读搜索创建了 DSH Task，但任务进入 `waiting_user`，未产生 Task 模型调用账本或预算预案，Task 用量为 0、上限为 50,000；相关模型账本只包含成功的终态说明调用（输入 386、输出 116），该助手消息已写回原会话。旧 PC 会话未及时补读该消息。
- 根因定位：`task_budget_exhausted` 唯一对应 PostgreSQL Task 模型调用预留检查，在 provider 派发前拒绝 `request.ReservedTokens > Task 剩余额度`。请求预留由输入估算与输出上限组成；旧账本没有记录被拒绝请求的输入/输出拆分，因此尚不能确定哪部分超限，也不能据此调高 50,000 上限或降低估算系数。
- 本地修复：只对白名单 `task_budget_exhausted` 保留阶段 `model_call_reservation` 与原因码 `task_budget_reservation_exceeded`，交给终态 DSH 解释模型；不把失败伪装成搜索结果。增加仅含整数的预算 envelope 诊断日志（Task 上限、已用、在途预留、输入估算、输出上限、总预留）；PC 在未见关联助手消息时最多退避刷新 8 次，消息到达后立即停止。审批或澄清待处理时不轮询。未关闭 fence、账本或硬预算限制。
- 本地验证：App/Worker 预算拒绝与终态解释定向用例、隔离 PostgreSQL Task 预算预留用例、Portal `Assistant.test.tsx`（334 项）和 `npm run typecheck` 通过；`git diff --check` 通过。部署 smoke 和真实 PC 搜索尚未执行本轮版本。
- 下一步：提交并推送 `release` 后部署安全诊断；在真实 `morde-pc` 发起一条全新的只读搜索，读取整数 envelope，按证据最小化过大的上下文/schema 或调整可安全配置的输出 cap，再核验一次工具派发、来源综合与原会话回告。PC 端改动需本地 `make pc` 构建并从实际客户端验收。若输入上下文本身超过剩余额度，应给出 DSH 生成的真实预算说明，不报告未执行的搜索。`T07`/`T08` 保持进行中；Taskless 持久增量流、完整 Worker 回归失败项及灰度基线仍未验收，自动恢复继续关闭。

### 2026-11-28 续验（预算诊断部署与 PC 交互阻塞）

- 发布：`release@38f610ce` 已推送并部署。原子更新脚本的 P0 smoke gate 为 5 pass、0 fail、5 exception；例外是无 `CH_TEST_DATABASE_URL` 的 PostgreSQL 项与需要真实 PC/DSH 的项。构建、迁移状态（280）、二进制切换、Portal health/readiness 均通过，`morde-portal.service` 为 active。
- PC 构建：本地 `make pc` 成功并打开实际 Electron `morde-pc`。CUA 能读取登录页，但键盘/剪贴板输入无法稳定反映到可见表单，按钮及坐标操作分别返回 `elementHasNoFrameWindow` / `noWindowsAvailable`；未提交登录，也未发送测试消息。按 PC 验收约定，不以 API 或后台 Web 代替客户端验收。
- 线上诊断：部署后只读读取了白名单预算失败日志，未出现新的 `task_budget_exhausted` 样本；这是因为本轮无法从 PC 登录创建新请求，不能据此判断预算问题已消失。旧 `unknown` 事件不可追溯补出输入/输出拆分。未调整 Task 预算、估算器、授权、重放或任何现存任务。
- 已通过的代码验证仍为上一节所列 App/Worker/PostgreSQL 定向用例、Portal Assistant 测试 334 项、Portal typecheck、`git diff --check`；`make pc` 本轮构建通过。真实 PC 登录后的普通快答、搜索、失败解释与阶段耗时样本尚未验收。
- 后续门槛：恢复 PC 界面输入能力或由用户在当前 PC 登录页完成登录后，继续在全新对话提交一条快答和一条只读搜索；读取拒绝请求的整数预留拆分，再提交针对性修复并验证来源、单次派发和原会话助手消息。Taskless 持久增量流仍未实现，`T07`/`T08` 保持进行中，自动恢复继续关闭。

### 2026-11-28 续验（Worker 进程重启与自动恢复关停门）

- 实际 Worker 进程重启 E2E：新增 `TestCloudDSHModelRecoverySurvivesWorkerProcessRestart`。首个独立 Go Worker/DSH Node 进程完成合成只读搜索、收到确认零用量的模型限流并持久化恢复计划后，被父进程 SIGKILL；新 Worker 进程从同一隔离 PostgreSQL 恢复。断言 lease/generation 和 recovery call ID 更新、模型读到已结算搜索、搜索 operation 恰好一次、结果与 `task.result` 只交付一次到原会话。PostgreSQL 17 下 `go test ./internal/worker -run '^TestCloudDSHModelRecoverySurvivesWorkerProcessRestart$' -count=1` 通过（3.947 秒）。
- 关停设计与实现：关闭进程级自动恢复开关后，旧进程留下的未启动 `scheduled` 计划不会继续请求模型；新 Worker 使用当前 Task/Agent/checkpoint fence，在同一 PostgreSQL 事务中把 Task 转为 `waiting_user / task_model_recovery_retry_available`、计划转为 `retry_available / automatic=false` 并写状态事件。用户手动 retry 仍可走原入口。不会撤销已开始的调用或把结果未知的调用改成可自动重试。
- 自动恢复安全验证：`go test ./internal/app -run 'Test.*TaskModelRecovery|TestBeginTaskModelRecoveryAttempt' -count=1`、`go test ./internal/api ./internal/app ./cmd/portal -count=1`、PostgreSQL `go test ./internal/store/postgres -run '^TestPostgresTaskModelRecovery' -count=1` 全通过。PostgreSQL 负例覆盖撤销授权版本、取消 Task、恢复绑定的 checkpoint 历史失配；均拒绝进入 `attempting`，没有恢复调用账本或响应。`git diff --check` 通过。
- 灰度审查：生产开关未设置，代码默认关闭。它仍是单一 Portal 进程的全局开关，没有独立 Worker/用户 cohort 或恢复专属指标；本轮只补安全关停，不开启线上灰度。灰度 cohort、低基数指标与可比较基线仍未通过。
- 服务器基线只读核验：`morde-portal.service` active，`/healthz` 和 `/readyz` 返回 200；`/opt/morde/.deployed-head` 指向 `38f610ce`，smoke 记录为 5 pass、0 fail、5 exception（无 `CH_TEST_DATABASE_URL` 的 PostgreSQL 门槛及真实 PC/DSH 门槛）。当前二进制 `vcs.revision=38f610ce` 且 `vcs.modified=true`；源码 checkout 无 tracked diff，但有一个既存未跟踪文件，本轮没有清理。此服务器基线还未包含本节新增代码。
- 实际 PC 阻塞：`morde-pc` 窗口仍在登录页。CUA 能观察界面，但点击和输入返回 `noWindowsAvailable`；本轮未提交登录或测试消息，因此真实快答/搜索/失败气泡、阶段延迟与审批入口仍未验收。未通过后台 Web/API 代测。
- 合并状态：GitHub `release@2120d0bb` 与此前本地 `release@d559c01a` 的 tree 相同（`10316138`）；已将该远端 release 合并到当前本地 `release`，避免从陈旧远端跟踪点发布。新增代码与本节 TODO 尚待提交、推送及部署；不得据以上本地隔离测试把 T07 标为完成。
