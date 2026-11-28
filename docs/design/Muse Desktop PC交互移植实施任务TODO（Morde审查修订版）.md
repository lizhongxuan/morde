# Muse Desktop PC 交互移植：实施任务 TODO

配套：[修订后的设计方案](<Muse Desktop PC交互移植设计方案（Morde审查修订版）.md>)。审查基线 `release@936389c`；开工时确认目标提交。保留原 M01–M12 编号方便对照，但改变发布顺序。E1–E4 是方案验证，不是打包 PC 验收。不把之前的中断修复或模型预算修复塞进此轮。

**本轮开工记录：**开工时检出基线为 `8c87122f`，与文档审查基线不同；不将旧基线的 E1–E4 视为本轮实测。2026-11-28 本轮功能提交 `7acb032c` 已在 `release`；本地 `release` 随后合入预算分支，HEAD 为 `0c906232`，当前比 `origin/release` 超前 2 个提交。后续 M02/M03/M06 工作继续基于 `release`，最终推送前会核对完整差异和验收结果。

| 建议 PR | 本次交付 | 强前置 |
|---|---|---|
| A | M01 工具活动折叠、M08 附件拖放 | 无；同改 `Assistant.tsx`，先后集成 |
| B | M05/M06/M07 结构化追问；M09a 语音取消可独立 | M05 服务端合同、M06 DSH 恢复验收 |
| C | M10/M11 完成通知 | M10 增量可靠性、平台通知实机 |
| D | M09b 真波形、M02/M03/M04 长输出 | 各自单独开关；分别通过权限、性能或隐私验收 |
| 每个 PR | M12 联合回归 | 各 PR 自带自动和实机记录，不留到最后补验 |

## PR A · 先交付两个纯交互收益

- [ ] **待 PC 实测 · M01 折叠工具活动。** `Assistant.tsx`：保留 MAIN / SUBAGENT 和现有 `TaskStepDetails`，二级分组按 Task、Agent、修订、相邻原序号；保守拆开身份缺失、修订不同或原日志有断点的项。同一失败步骤仍有详情入口，等候/失败/待核对不被默认折叠遮住。**验收：**12 条同组成功步骤折叠时只见 1 项，展开是原 12 个 ID；0 个提前详情请求；失败最多一次展开可达；覆盖序号断点、修订变化、主/子 Agent、重复事件与断线重连（基线 E1）。2026-11-28 定向测试复跑 2/2、Portal 类型检查通过；仍未用 `make pc` 实测。
- [ ] **PC 局部实测，拖放未观察到附件入草稿 · M08 附件拖放。** `Assistant.tsx`、`assistant-drafts.ts`：仅拦截输入区文件拖放并阻止导航；复用 `addComposerFiles` 的 staging、16 项上限和既有校验。预览对象 URL 生命周期跟随草稿；保留原文件选择、移除、错误恢复。**验收：**用合成文件对同一组执行「点击选文件」与「拖入」得到相同接受/拒绝结果；慢准备期间切会话仍进原草稿。拖入、移除和开草稿不触发附件发送或 Office POST；显式点既有 Office 预览才走受提示的服务端解析（E2）。联测用替身，不上传个人文件。2026-11-28 定向测试复跑 3/3、Portal 类型检查通过。使用源码 `morde-pc` 和合成文件实测：选择器可将文件加入“待发送附件”并可移除；Finder 拖放尝试未见草稿变化，拖放验收仍未完成，未发送消息或打开 Office 预览。

## PR B · 追问沿用 Morde；语音先修正确性

- [x] **M05 待答与答复合同。** `task_pending.go`、Task domain/API/OpenAPI：增加可选结构化题组、稳定 ID/版本、单选/多选选择、自由文本、明确 skip、答复幂等；服务端保留单题和原请求授权。保持旧 `/api/tasks/{id}/inputs`；结构化答复使用 v2 接口，旧客户端不能以自由文本误答题组。**验收：**DB/API 覆盖重复、过期、跨账号拒绝；必答 skip、路由/审批/文件交付/批任务题组均拒绝，不改变授权。**验证：**`go test ./internal/domain/task ./internal/app ./internal/api` 通过；PostgreSQL 16 临时容器中，隔离 worktree 应用 M05 补丁后 `TestPostgresStructuredClarificationIsVersionBoundIdempotentAndIsolated` 通过。主工作区未提交的 migration 00270 改动有 SQL 语法错误，故 PostgreSQL 验收在隔离基线执行。
- [x] **M06 DSH 原调用恢复。** `cloud_dsh_tool_broker.go`、`engine.mjs`：使用当前 Morde 贯通结构化输入，将同轮独占、子 Agent 禁提问和锁定工具定义覆盖到扩展入口；不引入第二套待答状态。上游实包只作 E6 参考，暂不改包和锁文件；若另做插件试验，单列 PR 与 go/no-go。**验收：**真实 Broker + DSH + 临时 PostgreSQL 测试等待期间重启 Worker、单任务取消、并发两个 Task、答复版本冲突和旧 checkpoint；原 call ID 的结果只交付一次，同一 Task 不因新用户消息重新创建。插件试验失败不阻塞 M05/M07。**验证：**Runtime `node --test --test-concurrency=1 test/runtime.test.mjs` 235/235；`go test ./internal/app` 通过。真实 `TaskWorker + DSH + CloudDSHToolBroker + PostgreSQL 16` 四项定向 PG16 E2E（Worker 重启、取消等待 Task、双 Task 并行隔离、旧 checkpoint 兼容）4/4 通过；主恢复用例稳定性 `-count=3` 3/3 通过，覆盖过期答复冲突、持久化/重排队、Worker 重启恢复原 DSH session/CallID、只 reconcile 一次及 assistant message 与 `task.result` 各一条。全量 `go test ./internal/worker -count=1 -v` 499 PASS / 2 FAIL（345.612s）；退出未知并行 suite 后两项单独重跑仍复现，分别是 `TestConversationSummaryDSHMainAndSideChatsContinueAfterLongHistoryRestart` 的 summary version 2→3 不匹配和 `TestCloudDSHModelBudgetPostgresHTTPWorkerProcessKilledBeforeProviderHTTP` 的 stale recovery marked=0。两项不属于 M06 改动；全量 worker 回归仍未通过，详见 M12。
- [ ] **待 PC 实测 · M07 PC 结构化追问。** `Assistant.tsx`、`src/app/api.ts`：原聊天气泡就地提交；草稿按 Task/请求/版本隔离。提交中、过期、失败和结果未知均有明确状态；侧栏只读详情不重复创建操作入口。**验收：**选择题相对同一个合成场景不用再打一条自由文本，单选/多选/自填、可跳过、权限与审批回归通过；无真实操作不显示「需要你操作」。2026-11-28 结构化答复定向测试复跑 4/4、Portal 类型检查通过；尚未用 `make pc` 实测。
- [ ] **待 PC 实测 · M09a 语音取消。** `AssistantVoiceInput.tsx` 和草稿归属：停止与取消分开，取消注销当前代次后 abort，已写入文字保留。**验收：**模拟迟到回调、切会话、快速重开、关闭窗口，0 次旧文字写入新草稿；不修改浏览器识别在线披露。2026-11-28 组件取消/迟到流及跨会话草稿定向测试复跑 3/3；尚未用 `make pc` 实测。

## PR C · 桌面通知

- [x] **M10 增量合同。** `notification.go`、`notification_source_store.go`、API/OpenAPI：保留现有站内列表/已读协议，新增按账号可持久提交顺序消费的完成增量合约。定义首次基线、最大页长、续页游标、游标过期恢复、授权过滤以及所有写入路径或等价的专用索引。**验收：**迁移与真实双连接 PostgreSQL 集成测试包含反向提交、翻页、断线和撤权；先后顺序不依赖时间。将 E4 的合成漏读案例变成测试；不能只用 `MAX(created_at)`、最新一页或未锁定的预分配序列。**验证：**`go test ./internal/api ./internal/app -count=1`、`go test ./internal/api ./internal/app ./internal/worker` 通过；隔离工作树 PG16 下 `go test ./internal/store/postgres -run 'TestPostgres(NotificationUpdates|TaskCompletionNotification)' -count=1` 3 项通过；变更行 golangci-lint 0 issue；M10 完整列表接口保持原样，增量响应仅含 `id`/`kind`。全仓 `make lint` 仍有 10 项超出本项文件边界的问题。
- [ ] **代码与 mock 验证完成，签名包系统提醒待验 · M11 Electron 提醒。** `morde-main.ts`、受信 preload、设置：只交付已完成事件，灰度默认关闭，开启起算；前台在会话看结果，后台通过 Electron 提醒。按服务实例/账号/通知 ID 去重；默认文案不含标题或内容。账号注销、切换、权限拒绝、未签名或系统不可用走站内兜底。**验证：**桌面逻辑定向测试 51/51、Portal 账户设置 3/3、两端类型检查及 desktop build 通过；过期游标恢复并发回归通过。签名 macOS 包和真实系统弹窗/点击尚未验收。`make pc` 构建后，CUA 绑定到已有默认用户目录的 Electron 进程；新隔离实例无法与同路径进程区分，因此未把可见设置页计为本轮候选验收。该项保持未勾选。

## PR D · 两项风险较高的可选增强

- [ ] **代码完成，实机电平待验 · M09b 真波形，依赖 M09a。** `morde-main.ts` 同时接入权限检查与权限请求；只允许受信 Assistant 主框架当前录音代次获取音频，保持现有剪贴板规则。Web Audio 仅计算电平，默认不打开开关。**验收：**实际设备有声音时电平变化；无音、摄像头、工作台、子帧、未点击录音、切换和迟到音轨全部符合门禁；关窗与取消后无存活音轨、无采样持久化/上行。并行采样冲突时保留原识别、不显示假波形（E3）。**验证：**Desktop bridge/preload 20/20、Portal voice 19/19、两端 typecheck 通过；`make pc` 在 macOS 启动后操作语音按钮，因系统显示“未获得麦克风权限”未能测得实际电平；签名包未验收。
- [ ] **代码与自动化验证完成，PC/部署端到端验收待做 · M02 Shell 长输出源头试点。** `runtime/morde-runtime/runtimeactions/shell*.go` 保留默认 64 KiB 组合结果与模型输入；只有显式 opt-in 才建立有界 sideband，旧执行次数和 `Operation.Result` 形状保持不变。Server 已将 Broker、RuntimeToolExecutor、运行时溢出采集、重启对账及容量状态接通。旧 Shell stdout/stderr 在新结果落库和回给 DSH 前统一脱敏；启用捕获时，原始前缀只在请求内存中与 spill 拼接，整流脱敏后才加密分页保存。定向 Runtime、runtimeclient、Broker、跨边界脱敏和 terminal recovery 测试通过；`node --test --test-concurrency=1 test/runtime.test.mjs` 235/235，`go test ./internal/app ./internal/api -count=1` 通过。真实 PC 获取旧 64 KiB 之外合成尾标记尚未验；慢接收端/宕机等运行环境场景及服务部署端到端仍待补，因此保持未勾选。
- [ ] **服务端实现与 PostgreSQL 16 集成验证完成，配额压力/真实客户端联合验收待做 · M03 脱敏保存和读取，依赖 M02。** `server/internal/app/task_step_output_capture.go` 与 PostgreSQL 实现整段脱敏、AAD 绑定加密分页、状态读取、分页 API/OpenAPI、Project 成员授权版本与资源版本分离校验，以及定时清理。状态区分 `complete/source_truncated/capacity_truncated/not_recorded/failed`；非完整状态绝不返回输出字节。PG16 集成用例验证多页密文读回、跨用户拒绝、授权版本撤销后拒读、不完整状态仅返回元数据及过期清理；`go test ./internal/app ./internal/api -count=1` 与 Portal API 测试通过。每账号最多 10,000 个 receipt/status；status 达上限时淘汰最旧元数据以保留最新状态。高并发 quota 压力和 Project 成员撤权矩阵尚未独立压测，PC 经部署 API 取回尾标记也待 M04 联验，故保持未勾选。
- [ ] **PC 页面和自动化验证完成，真实合成尾标记验收待做 · M04 PC 分页查看，依赖 M03。** `Assistant.tsx`、`src/app/api.ts`：保留原摘要，仅展开时逐页读取脱敏输出；校验 UTF-8 字节偏移与响应边界、可取消请求，并显示五类状态。完整输出遇到 404 会复核详情授权；会话/账号切换清空已读内容。`Assistant`/API 等定向 Portal 测试与类型检查通过。`MORDE_TEST_USER_DATA=/tmp/morde-pc-output-capture-acceptance make pc` 完成 Portal、Workbench、Desktop 构建并启动隔离用户目录的源码 `morde-pc`；已绑定并检查该候选窗口，确认对话页正常渲染。测试服务尚未部署本次分页 API，故没有在客户端验证旧 64 KiB 外尾标记、完整/不完整状态与撤权清缓存；未把本次客户端验收记为通过。

## M12 · 每个 PR 必填的验收记录

- [ ] 仓库内记录：基础 commit、候选 commit、OS/打包版本、测试命令、fixture、预期、实测值及未验证项。跑 PC 定向 React 测试（`npm test -- src/routes/Assistant.test.tsx`）、相应 Go/DSH/PG 测试，并用打包 PC 验收涉及的入口。文档中的 E1–E4 本轮在 Node/PGlite 用合成样例通过；不要写成实机或上线效果。
- [ ] 为每项保留可观察的前后对比：活动初始 12→1、附件等价且 0 隐式请求、追问一次正确提交、延迟可见的通知不漏、无过期转写串草稿、可读取截断外标记且不改变工具行为。基线取当前发布 PC，不用承诺没有测量的数据。
- [ ] 回归普通对话、会话切换、权限、原审批/澄清、消息路由、云端电脑、附件发送和 PC/iOS 同会话。状态、审批与已有任务中断方案并行集成；权限、泄漏、错误续跑任一反例失败即不发布。
- [ ] 对真波形、长日志、桌面提醒分别灰度、可独立关闭。涉及 Task/DSH 或接口变更时更新实际 OpenAPI、总流程、实施约定与验证记录；视觉规范未找到时标待核对，不伪造一套替代标准通过验收。

### 2026-11-28 本轮记录

- 基础提交：`8c87122f`。本轮功能提交：`7acb032c`；共享分支已有 `0f83fab0`（含部分 M06/M02/M03/M04）并已与 `origin/release` 同步；当前工作树继续在 `release` 增补剩余功能、测试与记录。OS macOS 27.0.1（Build 26A434，arm64）；Morde PC `0.1.0`，Electron `44.7.0`。
- `MORDE_TEST_USER_DATA=/tmp/morde-pc-todo-acceptance make pc` 的早期启动完成构建，但窗口身份当时未确认。续做时再次执行 `MORDE_TEST_USER_DATA=/tmp/morde-pc-output-capture-acceptance make pc`；Portal、Workbench、Desktop 构建成功，源码 Electron 使用隔离 `--user-data-dir` 启动。CUA 通过仓库内 Electron 的完整 app path 绑定到本次 `morde-pc` 窗口，确认对话页已渲染；手工观察只证明客户端启动，不证明新分页 API 的交互正确。完成观察后用 Ctrl-C 停止该隔离实例，命令以 SIGINT/130 结束属预期停止，不是构建失败。
- 本次续做基线为本地 `release@79eb5c73`、`origin/release@ee618bed`；服务端/API/PC 分页代码提交 `09bbde23` 已落在本地 `release`。文档随后单独提交；未推送或部署。
- 续做验证：`web/portal` 执行 `npm run generate:api`；`npm test -- src/routes/Assistant.test.tsx src/app/api.test.ts src/components/AssistantVoiceInput.test.tsx src/routes/AccountSettings.test.tsx` 421/421，`npm run typecheck` 通过。`server` 执行 `go test ./internal/app ./internal/api -count=1` 通过，输出捕获 exact terminal recovery 定向测试通过；PG16 临时容器中 `TestPostgresTaskStepOutputCapturePersistsPagesAndRechecksAuthorization` 通过，覆盖分页密文、账号隔离、授权撤销、元数据-only 不完整状态及过期清理；`gofmt` 与 `git diff --check` 通过。Runtime Go、HTTPS Runner、M02 定向 Broker/runtimeclient 测试和 DSH Runtime 235/235 的结果见 M02 行；没有修改测试服务器/生产服务，也没有使用用户提供的模型密钥。
- 未覆盖：M01/M07/M08/M09a 的实际 PC 入口、M02/M04 在客户端读取旧 64 KiB 外合成尾标记、已部署 API 联测、quota 高并发压力；M09b 真实麦克风电平与 M11 签名系统通知；M06 Worker 全量 suite 仍有 499 PASS / 2 FAIL，失败为 `TestConversationSummaryDSHMainAndSideChatsContinueAfterLongHistoryRestart`（summary version 2→3）与 `TestCloudDSHModelBudgetPostgresHTTPWorkerProcessKilledBeforeProviderHTTP`（stale recovery marked=0），其余取消/并发/旧 checkpoint 项的单独验收仍待补。统一视觉规范文件 `docs/design/ice-cream-blue/morde-icecream-blue-ui-spec.html` 当前不存在；外观对照待核对。此 TODO 和 M12 仍未勾选，直至必需的真实客户端路径与未完成矩阵有证据。
- 新增 PC 局部实测：`MORDE_TEST_USER_DATA=/tmp/morde-pc-todo-client-check make pc` 完成 Portal、Workbench、Desktop 构建并启动源码 Electron；隔离用户目录中的对话页已渲染。用 `/tmp/morde-pc-attachment-smoke.txt` 验证附件选择器 staging 和移除；尝试从 Finder 拖放后没有观察到附件进入草稿。未发送消息、未打开 Office 预览；此结果不能替代拖放、M01/M07/M09a 或服务端分页 API 的 PC 联验。随后以 Ctrl-C 停止隔离实例。
- 当前工作区复验：`web/portal` 中 M01 折叠用例定向 2/2、M08 拖放/staging/慢准备/拒绝用例定向 3/3；`npm run typecheck` 退出码 0。均为自动化检查，不替代实际 PC 客户端验收。
- 追加复验：M07 结构化答复/相同幂等键重试/版本冲突/失败恢复定向 4/4；M09a 取消后迟到识别/迟到音轨及跨会话草稿归属定向 3/3。仍是组件和 Portal 合成事件测试，不替代 `morde-pc` 实测。
