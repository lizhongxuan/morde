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

## T01 · P0：区分「轮到用户」和「系统停住了」（实现完成；事故根因未定位）

**入口：** `server/internal/app/cloud_dsh_tool_broker.go`（`pauseRecovery`）、`server/internal/app/task_terminal_delivery.go`、任务详情 API。保留原 DB 状态和诊断记录，先新增兼容的展示语义，字段名按现有 API 惯例落定。

- [x] 定义五类状态：需要你操作、正在恢复、已暂停可重试、已中断、结果待核对。输出归类原因、合法操作（可空）及状态版本或事件序号。未知原因保守降级；不暴露内部工具名、原始错误、凭据。
- [x] 单测：只有**当前有效、当前用户可操作**的审批/澄清才是「需要你操作」；只有确有持久化恢复安排才是「正在恢复」；没有服务器确认的动作时，返回空操作。
- [ ] 截图事故排查并行进行：拿到 Task ID 后查第一条失败事件、模型账本、末个有效 checkpoint；区分运行失败与通知失败。**未定位：** PC 可见的历史任务详情没有可关联到截图事故的 Task ID/首条失败事件，不能据任务文案认定根因；部署前未拿到可核查的对应账本和 checkpoint。

## T02 · P0：让 PC 标签和按钮始终匹配（实现完成；实机状态验收完成）

**入口：** `web/portal/src/routes/Assistant.tsx`、`Assistant.test.tsx`。现有中断 fixture `shows a recovery interruption without task action buttons` 可用作回归起点。

- [x] 任务卡、右侧详情和对话共用同一状态映射；把 `waiting_user / dsh_recovery_unresolved` 且无入口的场景显示为「已中断」或「结果待核对」；不显示「等待你处理」。
- [x] 真实审批/澄清在原对话尽可能就地操作；无权限、请求过期或已处理时不渲染可点击入口。服务端兼容回退只信有效的审批/澄清，不自行推断能重试。
- [ ] PC 测试：自动化已覆盖未知/过期状态、权限不足、快答与确认标记、刷新与事件游标缺口；实机已看到「结果待核对」且无普通重试入口。跨端同时操作与全部旧事件乱序场景尚未完成实机验收。

## T03 · P0：停止在原对话发内部日志，保证中断也能回告（实现完成；新闻 E2E 待补）

**入口：** `server/internal/worker/task_terminal_delivery_worker.go`、`server/internal/app/task_terminal_delivery.go`、PC 原对话任务事件订阅/重载。

- [x] 主对话用服务端确认的状态显示「拿没拿到结果、接下来由谁做、能点什么」。移除失败/等待消息里的步骤计数、通用「对账」和内部名字；诊断留任务详情。正常结果与现有验收证据保持绑定。
- [x] 用已提交任务状态作为无模型的回告来源。终态文案调用失败、超时或账本结果未知时，原对话仍有可重载的状态；不重新派发结果未知的文案调用。用任务修订与来源事件去重，避免状态卡和模型兜底重复。
- [x] 验收：新闻任务没有已核实结果时，只说「这次没能拿到已核实的新闻，任务已中断」；不可用的「再试一次」不显示。只有已登记真实恢复时才说「我会再查一次」。自动化用例覆盖无核实结果和无恢复安排；实际新闻查询 E2E 尚未完成。

## T04 · P1：把 👌🏻 接回原请求气泡（实现完成；跨页实机回归待补）

**入口：** `web/portal/src/routes/Assistant.tsx`、`server/internal/store/postgres/task_revision_start.go`、`conversation_route_actions.go`。

- [x] PC 渲染服务器确认的 `agent_acknowledged`，无障碍名称「已接下后台任务」。快答、只排队、未开始、别人的消息都不标。
- [x] 先核对合并、修订及重复投递是否能产生多个确认；持久化唯一展示锚点，PC 刷新会话消息，不新加确认 API。PC 自动化和 PostgreSQL 路由集成用例覆盖原消息绑定及重复确认约束；分页/重开完整实机回归仍在 T07。
- [ ] 验收：立即能给结果时直接回答；确认为耗时后台任务后打 👌🏻。实机已观察到原消息的无障碍标记；快答与后台耗时任务组合的真实端到端场景待补。需要报计划时只发一句有用的话，不把 emoji 写进助手正文。

## T05 · P1：开发一次可持久恢复的只读任务路径（实现完成；完整搜索恢复 E2E 待补）

**入口：** `server/internal/worker/cloud_dsh_executor.go`、`cloud_dsh_session_tree.go`、现有模型分类/账本和 PostgreSQL 的任务事件。优先复用已有持久恢复机制；只有确实无法表达才新增记录。

- [x] 分三路：已有结算结果就回放；明确可重试且服务端证明只读，才登记一次自动续跑；效果、用量、checkpoint 或权限无法确认就不派发。只读判定基于实际工具集合与账本，不看任务名称或模型声称。
- [x] 恢复申请原子绑定 Task ID、修订、授权版本、失败事件/调用、checkpoint 序号和摘要；同一修订累计自动额度首版 1 次，Worker 重启、不同失败事件和多 Worker 竞争不刷新额度。
- [x] Worker 获得合法租约后再次校验取消、授权、修订、checkpoint 和账本。从已验证位置续跑，复用已确认结果；确实需要新一轮模型调用才使用新的受控尝试身份。不得扩大工具权限、自动换模型或绕过现有预算。自动恢复开关默认关闭。
- [ ] PostgreSQL + 模拟模型完整集成验收：「搜索已结算 → 下一次模型临时失败 → 重启 Worker → 同一 Task 恢复 → 原对话交付」。现有 PostgreSQL 用例覆盖持久调度、重启后认领、单次额度、不同账本身份和结果回放，但尚未在同一端到端用例中断言搜索不再派发并向原对话交付已核实结果。

## T06 · P1：接上真实的手动入口（实现完成；实机点击待补）

**入口：** 先检查现有 Task 恢复 API；PC 对未关联任务的消息路由失败已有资格校验和重试 UI，可借鉴界面，但不能调用那个接口去恢复已关联的中断任务。不要把已有「工具步骤重试」当作模型恢复接口。

- [x] 复用 T05 的资格判定与持久调度；增加 Task 级动作。点击重新校验用户、修订和授权，以同一任务继续，返回当前持久状态。
- [x] 自动化覆盖幂等请求、重复/竞争认领、过期版本与已认领状态；结果未知时无普通重试入口。实机已确认结果待核对状态不显示重试按钮；安全重试按钮实机点击与两端并发路径待补。

## T07 · P0：合并和灰度门槛（进行中；发布验收未完成）

- [x] 隔离工作树验证：`go test ./internal/api ./internal/app ./cmd/portal -count=1`；恢复专属 `go test ./internal/worker -run 'Test(CloudDSHReadOnlyRecovery|CloudDSHModelRecoveryStartRequiresExactAttemptAndConfirmedResponse|CloudDSHModelResultErrorForCallKeepsOnlyMatchingNoDispatchEvidence|CloudDSHModelResultErrorForCallKeepsOnlyMatchingProviderRejectEvidence)' -count=1`；本机 PostgreSQL `go test ./internal/store/postgres -run TestPostgresTaskModelRecovery -count=1`（4 个集成用例）；PC `npm test -- src/routes/Assistant.test.tsx`（289/289）。
- [ ] 用打包 PC 完成端到端回归，保存目标 commit、任务/事件关联、三种页面状态与实际派发次数；全量 worker 包测试当前未通过，测试 Runtime 在本机握手前退出（`account Agent Runtime handshake is invalid`），需在 DSH Runtime 可启动的环境重跑。
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

**完成标准：** 用户不用打开任务详情，也能知道 Morde 是否接下任务、最后交付了什么，卡住时轮到谁、有没有下一步。
