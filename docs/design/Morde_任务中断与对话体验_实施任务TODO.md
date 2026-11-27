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

- [x] 隔离工作树回归：`go test ./internal/api ./internal/app ./cmd/portal -count=1`；恢复专属 Worker 测试（含完整搜索恢复 E2E）；本机 PostgreSQL `go test ./internal/store/postgres -run 'Test(DurableRecoveredToolAdmission|PostgresTaskModelRecovery)' -count=1`；DSH Runtime 恢复启动协议测试；PC `npm test -- src/routes/Assistant.test.tsx`（289/289）。2026-11-28 当前结果与命令见下方记录。
- [ ] 用打包 PC 完成端到端回归，保存目标 commit、任务/事件关联、三种页面状态与实际派发次数；全量 Worker 包仍未通过，2026-11-28 的具体失败见下方记录，修复/补齐对应测试环境后重跑。
- [ ] 真实 `morde-pc` 交互尚未稳定通过：本轮 `make pc` 构建并启动客户端后，隔离旁聊的无工具快答一直停留在连接状态，未收到助手回复；早先一次快速问答通过不代表当前连接稳定。仍需确认未知结果只保留在任务详情、原对话只显示自然语言说明，且终态回告模型未知时仍能收到确定性说明。
- [ ] 正例：快答无 👌🏻；耗时任务一次 👌🏻；可重试故障自动恢复后出结果；耗尽后原对话收到说明；真实审批能继续。
- [ ] 反例：结果或用量未知、工具不在授权范围、权限撤销、用户取消、checkpoint 不匹配、写入结果不明、两个 Worker 竞争。断言不发生不安全派发、跨账号展示或重复交付。
- [ ] 自动恢复独立灰度并可关闭；关闭后仍保留状态修复与原对话回告。比较「无入口却提示等待用户」次数、任务回告覆盖率、恢复成功率及额外模型调用。任何重复副作用或跨权限展示立即停用自动恢复。

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

**完成标准：** 用户不用打开任务详情，也能知道 Morde 是否接下任务、最后交付了什么，卡住时轮到谁、有没有下一步。
