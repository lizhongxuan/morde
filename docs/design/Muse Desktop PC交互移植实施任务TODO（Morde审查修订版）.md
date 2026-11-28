# Muse Desktop PC 交互移植：实施任务 TODO

配套：[修订后的设计方案](<Muse Desktop PC交互移植设计方案（Morde审查修订版）.md>)。审查基线 `release@936389c`；开工时确认目标提交。保留原 M01–M12 编号方便对照，但改变发布顺序。E1–E4 是方案验证，不是打包 PC 验收。不把之前的中断修复或模型预算修复塞进此轮。

**本轮开工记录：**当前检出基线提交 `8c87122f`，与文档审查基线不同；候选改动尚在未提交工作树中。后续验收以当前代码为基线，不将旧基线的 E1–E4 视为本轮实测。

| 建议 PR | 本次交付 | 强前置 |
|---|---|---|
| A | M01 工具活动折叠、M08 附件拖放 | 无；同改 `Assistant.tsx`，先后集成 |
| B | M05/M06/M07 结构化追问；M09a 语音取消可独立 | M05 服务端合同、M06 DSH 恢复验收 |
| C | M10/M11 完成通知 | M10 增量可靠性、平台通知实机 |
| D | M09b 真波形、M02/M03/M04 长输出 | 各自单独开关；分别通过权限、性能或隐私验收 |
| 每个 PR | M12 联合回归 | 各 PR 自带自动和实机记录，不留到最后补验 |

## PR A · 先交付两个纯交互收益

- [ ] **待 PC 实测 · M01 折叠工具活动。** `Assistant.tsx`：保留 MAIN / SUBAGENT 和现有 `TaskStepDetails`，二级分组按 Task、Agent、修订、相邻原序号；保守拆开身份缺失、修订不同或原日志有断点的项。同一失败步骤仍有详情入口，等候/失败/待核对不被默认折叠遮住。**验收：**12 条同组成功步骤折叠时只见 1 项，展开是原 12 个 ID；0 个提前详情请求；失败最多一次展开可达；覆盖序号断点、修订变化、主/子 Agent、重复事件与断线重连（基线 E1）。定向测试、类型检查通过；尚未用 `make pc` 实测。
- [ ] **待 PC 实测 · M08 附件拖放。** `Assistant.tsx`、`assistant-drafts.ts`：仅拦截输入区文件拖放并阻止导航；复用 `addComposerFiles` 的 staging、16 项上限和既有校验。预览对象 URL 生命周期跟随草稿；保留原文件选择、移除、错误恢复。**验收：**用合成文件对同一组执行「点击选文件」与「拖入」得到相同接受/拒绝结果；慢准备期间切会话仍进原草稿。拖入、移除和开草稿不触发附件发送或 Office POST；显式点既有 Office 预览才走受提示的服务端解析（E2）。联测用替身，不上传个人文件。自动化测试通过；尚未用 `make pc` 实测。

## PR B · 追问沿用 Morde；语音先修正确性

- [x] **M05 待答与答复合同。** `task_pending.go`、Task domain/API/OpenAPI：增加可选结构化题组、稳定 ID/版本、单选/多选选择、自由文本、明确 skip、答复幂等；服务端保留单题和原请求授权。保持旧 `/api/tasks/{id}/inputs`；结构化答复使用 v2 接口，旧客户端不能以自由文本误答题组。**验收：**DB/API 覆盖重复、过期、跨账号拒绝；必答 skip、路由/审批/文件交付/批任务题组均拒绝，不改变授权。**验证：**`go test ./internal/domain/task ./internal/app ./internal/api` 通过；PostgreSQL 16 临时容器中，隔离 worktree 应用 M05 补丁后 `TestPostgresStructuredClarificationIsVersionBoundIdempotentAndIsolated` 通过。主工作区未提交的 migration 00270 改动有 SQL 语法错误，故 PostgreSQL 验收在隔离基线执行。
- [ ] **代码完成，主要恢复闭环已验、矩阵待补 · M06 DSH 原调用恢复。** `cloud_dsh_tool_broker.go`、`engine.mjs`：使用当前 Morde 贯通结构化输入，将同轮独占、子 Agent 禁提问和锁定工具定义覆盖到扩展入口；不引入第二套待答状态。上游实包只作 E6 参考，暂不改包和锁文件；若另做插件试验，单列 PR 与 go/no-go。**验收：**真实 Broker + DSH + 临时 PostgreSQL 测试等待期间重启 Worker、单任务取消、并发两个 Task、答复版本冲突和旧 checkpoint；原 call ID 的结果只交付一次，同一 Task 不因新用户消息重新创建。插件试验失败不阻塞 M05/M07。**验证：**Runtime `node --test --test-concurrency=1 test/runtime.test.mjs` 235/235；`go test ./internal/app` 通过。新建真实 `TaskWorker + DSH + CloudDSHToolBroker + PostgreSQL 16` 测试 `TestCloudDSHStructuredClarificationResumesAfterWorkerRestart` 首次 1/1、稳定性复跑 `-count=3` 3/3 通过；验证了旧版本答复冲突不改变 pending、结构化答复持久并重排队、Worker 重启后恢复同一 DSH session、原 `ask_user` CallID 执行后只 reconcile 一次，且 assistant message 与 `task.result` 各一条。发现并修复 PostgreSQL 创建澄清行时遗漏 `answer_format` 的缺陷。完整 Worker 套件结果及单任务取消、两个 Task 并发隔离、旧 checkpoint 兼容仍待验证。
- [ ] **待 PC 实测 · M07 PC 结构化追问。** `Assistant.tsx`、`src/app/api.ts`：原聊天气泡就地提交；草稿按 Task/请求/版本隔离。提交中、过期、失败和结果未知均有明确状态；侧栏只读详情不重复创建操作入口。**验收：**选择题相对同一个合成场景不用再打一条自由文本，单选/多选/自填、可跳过、权限与审批回归通过；无真实操作不显示「需要你操作」。自动测试 380 项与类型检查通过；尚未用 `make pc` 实测。
- [ ] **待 PC 实测 · M09a 语音取消。** `AssistantVoiceInput.tsx` 和草稿归属：停止与取消分开，取消注销当前代次后 abort，已写入文字保留。**验收：**模拟迟到回调、切会话、快速重开、关闭窗口，0 次旧文字写入新草稿；不修改浏览器识别在线披露。组件、路由及草稿测试通过；尚未用 `make pc` 实测。

## PR C · 桌面通知

- [x] **M10 增量合同。** `notification.go`、`notification_source_store.go`、API/OpenAPI：保留现有站内列表/已读协议，新增按账号可持久提交顺序消费的完成增量合约。定义首次基线、最大页长、续页游标、游标过期恢复、授权过滤以及所有写入路径或等价的专用索引。**验收：**迁移与真实双连接 PostgreSQL 集成测试包含反向提交、翻页、断线和撤权；先后顺序不依赖时间。将 E4 的合成漏读案例变成测试；不能只用 `MAX(created_at)`、最新一页或未锁定的预分配序列。**验证：**`go test ./internal/api ./internal/app -count=1`、`go test ./internal/api ./internal/app ./internal/worker` 通过；隔离工作树 PG16 下 `go test ./internal/store/postgres -run 'TestPostgres(NotificationUpdates|TaskCompletionNotification)' -count=1` 3 项通过；变更行 golangci-lint 0 issue；M10 完整列表接口保持原样，增量响应仅含 `id`/`kind`。全仓 `make lint` 仍有 10 项超出本项文件边界的问题。
- [ ] **代码与 mock 验证完成，签名包系统提醒待验 · M11 Electron 提醒。** `morde-main.ts`、受信 preload、设置：只交付已完成事件，灰度默认关闭，开启起算；前台在会话看结果，后台通过 Electron 提醒。按服务实例/账号/通知 ID 去重；默认文案不含标题或内容。账号注销、切换、权限拒绝、未签名或系统不可用走站内兜底。**验证：**桌面逻辑定向测试 51/51、Portal 账户设置 3/3、两端类型检查及 desktop build 通过；过期游标恢复并发回归通过。签名 macOS 包和真实系统弹窗/点击尚未验收。`make pc` 构建后，CUA 绑定到已有默认用户目录的 Electron 进程；新隔离实例无法与同路径进程区分，因此未把可见设置页计为本轮候选验收。该项保持未勾选。

## PR D · 两项风险较高的可选增强

- [ ] **代码完成，实机电平待验 · M09b 真波形，依赖 M09a。** `morde-main.ts` 同时接入权限检查与权限请求；只允许受信 Assistant 主框架当前录音代次获取音频，保持现有剪贴板规则。Web Audio 仅计算电平，默认不打开开关。**验收：**实际设备有声音时电平变化；无音、摄像头、工作台、子帧、未点击录音、切换和迟到音轨全部符合门禁；关窗与取消后无存活音轨、无采样持久化/上行。并行采样冲突时保留原识别、不显示假波形（E3）。**验证：**Desktop bridge/preload 20/20、Portal voice 19/19、两端 typecheck 通过；`make pc` 在 macOS 启动后操作语音按钮，因系统显示“未获得麦克风权限”未能测得实际电平；签名包未验收。
- [ ] **Runtime/client opt-in transport 已验证，端到端仍未完成 · M02 源头试点。** `runtime/morde-runtime/runtimeactions/shell*.go`：保留现有 64 KiB 默认组合结果与模型输入；只有明确传入 descriptor 才通过独立有界 sideband 传输 Shell 溢出字节，未启用时旧 `Operation.Result` 不变。`go test ./runtimecore ./runtimehttp ./runtimeactions` 与 `go test ./internal/runtimeclient ./internal/app -run 'SupplementalOutputCapture|RuntimeOperationRunner' -count=1` 通过；定向 HTTPS Runner 测试确认 sideband 身份、释放后读取失败且 Shell 只执行一次。服务端默认不发 descriptor，未接入 Worker/Task 持久化；慢接收端、宕机、全局配额、取消和重启验收均未完成。M02 保持未勾选。
- [ ] **尚未实现 · M03 脱敏保存和读取，依赖 M02。** Runtime/client 目前只有 opt-in 页传输和释放接口；没有服务端跨页脱敏、加密 PostgreSQL 持久化、原子完整回执、逐页 Task/Workspace 当前权限复核、保留期清理或读取路由。不得把 Runtime transport 当成可持久读取的日志；验收仍需覆盖跨任意分块敏感串、缺页/重复页、过期、容量断开、撤权、账号隔离、重启与清理。
- [ ] **暂缓，依赖 M03 · M04 PC 分页查看。** `Assistant.tsx`、`src/app/api.ts`：保留原摘要，展开才读取分页；显示已完整保存/源头截断/容量截断/未记录/失败。关闭、切换账号、撤权清掉预读缓存，不显示旧授权结果。**验收：**在打包 PC 能找到旧 64 KiB 之外的合成尾部标记；不完整消息不标「完整」；未记录的旧执行不补采、不暗示取回。M03 的加密存储与逐页授权基础未实现，当前不能连通有效分页。

## M12 · 每个 PR 必填的验收记录

- [ ] 仓库内记录：基础 commit、候选 commit、OS/打包版本、测试命令、fixture、预期、实测值及未验证项。跑 PC 定向 React 测试（`npm test -- src/routes/Assistant.test.tsx`）、相应 Go/DSH/PG 测试，并用打包 PC 验收涉及的入口。文档中的 E1–E4 本轮在 Node/PGlite 用合成样例通过；不要写成实机或上线效果。
- [ ] 为每项保留可观察的前后对比：活动初始 12→1、附件等价且 0 隐式请求、追问一次正确提交、延迟可见的通知不漏、无过期转写串草稿、可读取截断外标记且不改变工具行为。基线取当前发布 PC，不用承诺没有测量的数据。
- [ ] 回归普通对话、会话切换、权限、原审批/澄清、消息路由、云端电脑、附件发送和 PC/iOS 同会话。状态、审批与已有任务中断方案并行集成；权限、泄漏、错误续跑任一反例失败即不发布。
- [ ] 对真波形、长日志、桌面提醒分别灰度、可独立关闭。涉及 Task/DSH 或接口变更时更新实际 OpenAPI、总流程、实施约定与验证记录；视觉规范未找到时标待核对，不伪造一套替代标准通过验收。

### 2026-11-28 本轮记录

- 基础提交：`8c87122f`。候选为当前未提交工作树，尚无候选 commit。运行环境：macOS 27.0.1（Build 26A434，arm64）；Morde PC `0.1.0`，Electron `44.7.0`。
- `MORDE_TEST_USER_DATA=/tmp/morde-pc-todo-acceptance make pc` 完成 Portal、Workbench 与 Desktop 构建并启动 Electron；检查后用 Ctrl-C 结束本轮启动。CUA 看到的 `morde-pc` 窗口属于已有默认用户目录进程，未能绑定到隔离启动实例，故不作为本轮候选交互验收。可见客户端中合成文本附件点击选取后能移除且未发送；拖放合成文件没有可见变化；这些观察不计为候选版本通过。
- 当前验证：`web/portal` 中 `npm test -- src/routes/Assistant.test.tsx src/app/api.test.ts src/components/AssistantVoiceInput.test.tsx src/routes/AccountSettings.test.tsx` 409/409、`npm run typecheck` 通过；新增 M08 用例确认超限合成 PNG 经文件选择和拖放都会被同样拒绝，且不会发送消息或上传附件。Desktop `npm run typecheck`、通知/桥接 51/51 及全量 `npm test` 308/308 通过；`server` 中 `go test ./internal/domain/task ./internal/app ./internal/api ./internal/worker` 通过，M02 定向 `go test ./internal/runtimeclient ./internal/app -run 'SupplementalOutputCapture|RuntimeOperationRunner' -count=1` 通过；`runtime/morde-runtime` 中 `go test ./runtimecore ./runtimehttp ./runtimeactions` 通过；`runtime/morde-cloud-runtime` 中 `node --test --test-concurrency=1 test/runtime.test.mjs` 235/235；M02 Runtime 与 Server 变更文件 `gofmt -d` 和 `git diff --check` 通过。Desktop 全量套件首轮暴露 main-path/recovery 的 Electron Session mock 缺少 `setPermissionCheckHandler`；补齐两个测试夹具后，`morde-main-path` 2/2、`morde-main-recovery` 63/63、Desktop 全量 308/308 通过。M05、M10 的隔离 PostgreSQL 16 集成测试分别按上文记录通过；未对测试服务器或生产服务部署改动。
- 未覆盖：当前 PC 候选的原生交互、签名 macOS 通知、真实麦克风电平、M06 Worker 恢复的取消/并发/旧 checkpoint 矩阵与完整 Worker 套件、M02 慢接收端/宕机/配额和服务端集成、M03 durable store/read API 与逐页授权、M04 分页查看和联合跨端回归。视觉规范文件 `docs/design/ice-cream-blue/morde-icecream-blue-ui-spec.html` 当前不存在，相关外观验收待核对。
