# 11 云端 Agent 验证记录

> 状态：T11.04 的本地实现与自动化验证已完成，包括真实 runtime handler 对 Workspace 文件的核验；T11.05a–e（审批、单任务停止、Owner gate stop-all/enable、澄清及 Portal UI）已本地实现并通过自动化验证；MCP 电脑操作已由 T13.05 迁入统一 RuntimeControl。2026-10-22 目标机已通过 OpenSandbox + runsc + 固定 AIO 基础镜像的运行、浏览器和 PVC 重建探针，但没有部署 Portal 或本地构建的 Morde 镜像。真实 Agent、Morde artifact、noVNC 可视画面、接管连续性及 T11.06 真实模型验收仍未完成。

## T11.04 文本任务恢复

- 从有界持久事件重建文本上下文，沿用原始模型 CallID 和 Morde operation_id，不要求模型重新生成工具调用。
- 可恢复已保存的 pending tool call、最终核对声明、运行端 terminal 结果（即使进度事件丢失）及未派发的 prepared operation。prepared operation 在当前任务租约与 Host fence 下显式接管后才可派发。
- unknown 操作使用持久记录的运行实例、generation、control version 与原 runtime lease 查询。只有同一运行端明确返回 operation_not_found，才按同一 operation ID 重派；超时、取消、实例不匹配和其余未知状态继续暂停，不猜测成功/失败。
- 图片上下文无法重建、checkpoint 损坏/超限、操作顺序不一致时 fail-closed；在对应批量项上下文内隔离恢复，不能安全重建时暂停该任务，不会把未处理项报告为完成。

## T11.04 批量逐项执行、重试与产物归属

- 批次按顺序逐项执行；每项执行前持久化由任务目标、批量项 ID、键和输入计算的不可变指纹，并持久记录 queued/running/succeeded/failed/unknown 状态。
- 成功项的文本结果持久化；重启后跳过已成功项，仅继续尚未完成项。若输入与既有指纹不符、checkpoint 缺失/损坏/超限，或模型用量未知而无法安全继续，任务等待处理，不猜测或重放。
- 新增 POST /api/tasks/{id}/actions 的 retry_batch_item 动作；服务端要求 task:control 权限和 Idempotency-Key，校验任务所有权及目标失败项，并由存储层以事务原子地将指定项置为 queued、递增 attempt、重置任务执行状态并记录事件。相同幂等键可安全重放，不同请求体冲突。
- checkpoint、重试 operation key 和产物关联都包含批量项 attempt。旧 checkpoint 不会被后续 attempt 误认；批量项 artifact 保存 batch_item_id 与 batch_attempt，避免重试产物覆盖或串到其他项。
- 批量 artifact 元数据仅在 Agent 收到明确的 verified 结果后持久化；路径、MIME、标签、重复路径和 item/attempt 关联均做校验。Store 对重复相同元数据做幂等处理，对关联或内容变化拒绝覆盖。
- `file.verify` runtime handler 只读取 workspace 中的 regular file，返回路径、大小、SHA-256 与匹配结果，不返回正文；上限为 16 个文件、每个 1 MiB、合计 8 MiB，路径穿越、符号链接、重复 JSON 键和超限输入 fail-closed。Agent 只从唯一配对的成功 `file_write` call/output 推导期望摘要；`RuntimeToolExecutor.Verify` 使用当前 task:observe 授权、租约与 Host fence，先持久登记 operation，再读取运行端结果。只有路径/大小/摘要全部匹配才生成 artifact；unknown/dispatched 恢复只查询并轮询原 operation ID，不重新 Execute。
- 本地集成测试将 server RuntimeToolExecutor 连接到真实 `project/morde-runtime/runtimefiles` Workspace handler，验证当前文件匹配可产出 artifact、文件变化后不能产出 artifact。该测试不是精确 Host 镜像或隔离 Linux 验收。对没有可验证 `file_write` 证据的任务、GUI/浏览器结果或不支持 `file.verify` 的旧运行端仍返回 unknown，不能用模型声明代替。

## 验证结果

- `go test ./internal/app ./internal/api`：通过，包含重试授权/幂等键、API 严格请求体、attempt checkpoint/operation key 隔离、artifact 输入校验与关联响应测试。
- `go test -race ./... -count=1`：本轮全 server 包通过；最初未设置 `CH_TEST_DATABASE_URL` 的运行跳过 PostgreSQL Store 集成，随后通过本机 PostgreSQL 16.15 容器设置临时 DSN 重跑，全 Store race 集成也通过，测试数据库由 helper 创建并清理。
- `go test ./internal/app -run TestRuntimeToolExecutorVerifyAgainstWorkspaceFileHandler -count=1`：通过，实际跨 server 与 runtime Workspace handler 验证匹配/变更两种结果。
- `go vet ./...`：通过；`make lint`：通过，0 issues。
- `project/morde-runtime`：`go test -race ./...` 与 `go vet ./...` 通过。
- make generate、OpenAPI 生成：通过；Portal 与 Workbench 的 API 类型已同步。
- Portal：typecheck、lint、unit 与 build 已通过；审批/澄清 UI 的本地组件测试见 `web/portal/src/routes/Tasks.test.tsx`，澄清 mock 浏览器流程见 `web/portal/e2e/tasks.e2e.ts`。后续最新验证总数见本清单末尾的 T11.05 记录。
- Workbench：npm run typecheck、npm run lint、npm test 通过（8 个测试文件、63 项）。

## 尚未完成

- 真实 Morde Host 上仍未验证 artifact Verify 和 attempt 归属。目标机的基础 AIO/OpenSandbox 命名卷已通过 Sandbox 删除/重建后的数据保持探针，但没有运行 Morde artifact handler，不能替代真实产物验收。
- T12.09 隔离 Linux 上的 AI→人工接管→交回、旧连接失效与重建后连续性验收，以及 T11.06 真实云端执行验收仍未完成。
- 本地 Portal mock Playwright 与 Workbench mock Playwright 只覆盖 mock 页面。Portal 的内置 CUA Node runtime 启动失败（`No such file or directory`）；目标机 AIO Chromium API 的浏览器探针已通过，但 noVNC 页面未出现 canvas，VNC 帧和人工输入仍未确认。
- 目标机没有部署 Portal/Morde 镜像或运行真实模型任务；本轮只通过 SSH 命令安装基础 OSB 环境，没有上传或发送任何文件/镜像。

### 2026-10-22 目标机基础运行环境验收

- 目标机已有固定 OpenSandbox 1.1.1、runsc 和固定 AIO 基础镜像。真实 OSB Sandbox 的资源约束、AIO authenticated API、浏览器 navigate/evaluate、截图 PNG、公网页面访问及命名 Workspace 卷跨 Sandbox 重建均通过；合成卷和 Sandbox 已清理。
- 这不等于 Agent 闭环：目标机没有 Morde Host 镜像、Portal、PostgreSQL、真实模型凭证或任务数据。T11.03 运行端工具联调、T11.04 artifact/attempt 真实核验和 T11.06 模型执行仍未完成。后续需在目标机本地准备项目副本/镜像，并由用户在 Portal 配置模型凭证；不要把密钥发到聊天中。

## T11.05a 版本化审批请求与答复（2026-10-22）

- 新增 `task_pending_requests` 持久表。Agent 仅在工具执行器明确返回 `approval_required` 时登记请求；保存不可变 ID/version、对象/内容/影响、工具名、CallID、参数 SHA-256，以及 runtime generation/instance/control fence。普通预算等待、恢复不确定和 Host fence 变更不会伪装成待答审批。
- `GET /api/tasks/{id}` 在 `pending_request` 返回活动审批详情。`POST /api/tasks/{id}/actions` 新增 `action=respond`，仅接受 approve/reject，并要求 `task:control`、Idempotency-Key、请求 ID/version 与当前 Host 授权。数据库事务原子消费请求、推进 task event sequence、记录 `task.response` 并把任务重新入队；相同请求重放幂等，陈旧版本或已处理请求冲突。
- 批准仅在工具名、CallID、参数摘要和 Host fence 完全匹配时传给执行器。Fence 变化会生成需重新确认的版本；参数/工具变化会 fail closed。拒绝生成持久 `OperationFailed` 终态并把“用户拒绝、未执行”作为工具输出送回模型，不经过 Prepare/Execute，因此不会创建运行端副作用；重启恢复可从操作终态重建该结果。
- 自动化覆盖请求持久化与读取、批准精确绑定、拒绝不派发、API 请求校验、task:control scope、活动请求序列化、幂等冲突与过期版本冲突。`task.response` 已加入事件类型、OpenAPI 和 Portal/Workbench 生成类型。
- T11.05a 当时的验证：`go test -race ./... -count=1`、`go vet ./...`、`make lint`（0 issues）、`make generate`、Portal/Workbench `npm run generate:api` 与 `npm run typecheck` 通过。随后使用本机 PostgreSQL 16.15 容器提供临时 `CH_TEST_DATABASE_URL` 重跑全 server race，含全部 Store 集成与新 migration、原子答复、幂等重放和 stale-version 冲突测试，通过；测试 helper 创建并清理独立临时数据库。Portal `npm run lint` 与 `npm test` 当时为 11 个测试文件、33 项；后续 T11.05d/e 的界面与测试状态见下方及本记录的本地浏览器验收边界。
- T11.05a 首次记录时的状态：T11.05b 的 pause/resume/cancel、T11.05d 的 Agent 普通澄清与持久化答复、T11.05e 的 Portal 任务列表/详情、审批与澄清 UI 已实现并通过本地自动化验证；当时 T11.05c 仍待 T14.01 门禁合同。后续 Owner gate 实现及验证见本记录的 “T11.05c Owner 级 Agent stop-all/enable” 小节。单任务 pause/cancel 只有在逐项运行端停止得到确认或满足持久 safe-after 条件后才进入终态；该行为尚未通过真实 Host/隔离 Linux 验收。本实现不把任务状态变化当作运行端已停止。

## T11.05c Owner 级 Agent stop-all/enable（2026-10-22）

- 新增迁移 `00014_owner_execution_control.sql`：为每个 Owner 建立持久 `enabled/stopping/disabled` gate 与单调递增 `epoch`，新用户自动建 gate；既有用户从 enabled 起始。任务写入执行 epoch，缺失或无效 gate 一律 fail-closed。
- 新增 Portal Session-only `GET /api/execution`、`POST /api/execution/stop-all` 与 `POST /api/execution/enable`。写操作要求正数 `expected_epoch` 和 `Idempotency-Key`，Owner API Key 无权访问；结果写入审计。stop-all 在事务内先关闭派发屏障并 fence queued/running/waiting_user/paused 等任务，再通过已有持久 stop worker 逐 operation 停止/核验；确认全部排空后才进入 disabled。enable 推进 epoch，只开放新任务，不复活 cancelled 任务。
- 同一 PostgreSQL gate 锁与 epoch 校验覆盖任务创建/追加、认领、Agent lease 续期、operation 登记与 `prepared → dispatched`。stop worker 自己的停止租约和排空路径不受 gate 阻止。Host 控制状态与活动租约保持独立。Portal Task 页增加 stop-all 确认、stopping 进度、disabled/enable 状态和错误展示；明确标注 MCP AIO 直连暂不受此开关控制。
- store 集成测试覆盖默认 gate/缺行 fail-closed、Owner stop 状态与 epoch、queued/running 任务入取消队列、认领/续租阻断、operation 派发被 fence、stop worker 排空、幂等重放、过期 epoch 冲突、重新开启不复活任务，以及 stop-all 与 submit 并发时无 queued/running 任务逃逸。app/API 测试覆盖 Session-only、幂等请求字段和审计行为。
- 完整 `go test -race ./...` 使用本机临时 PostgreSQL 16.15 容器并通过（所有 server 包；Store 使用临时测试数据库，测试后清理）；`go vet ./...`、`make lint`（0 issues）、`make generate` 通过。OpenAPI 使用 Redocly 2.64.1 校验有效，保留 5 条既有 warning；Portal API 类型生成通过。
- Portal `npm run typecheck`、`npm run lint`、`npm test`（12 个测试文件、43 项）、`npm run build` 通过；`npm run e2e -- e2e/tasks.e2e.ts` 两项通过，实际操作本地 mock 的“确认停止→显示 stopping→重新开启”，并检查 MCP 边界提示。
- 内置 CUA 重试仍因 Node runtime `No such file or directory` 无法启动；本地 Playwright Chrome mock 不等于用户要求的内置浏览器验收。未连接真实 Host；未验证精确 Host 镜像/runsc、noVNC、计划调度器。T14.01 的 Owner gate 子合同已实现，但 DST/错过执行/通知合同未定稿，T14.02 计划调度尚未开始。
- 重要边界：`server/internal/mcp/host_tools.go` 的电脑工具仍直接访问 AIO，因此本 gate 不能停止这类 MCP 操作。Portal UI 明示此限制；需完成 T12.08/T13.05 迁移后才可把 Owner stop-all 扩为覆盖统一电脑动作。

## 本地浏览器验收边界（2026-10-22）

- 用户授权仅限本地 mock 页面上的协议同意动作。Portal 源码未包含《用户协议》/《隐私协议》勾选控件，因此没有勾选或提交同意。本地 mock 自动化与真实运行验收分开记录：按最新 T11 TODO 验证结果，Portal mock Playwright 14 项通过、1 项真实主机用例按配置跳过；Workbench mock Playwright 4 项通过。它们没有操作真实 Host。
- 内置 CUA 在重试后仍因 Node runtime `No such file or directory` 无法启动，未完成内置浏览器的可视操作验收；Workbench 本地 AIO upstream 未启动，未验证实际 VNC 帧。真实 Host 上的 Agent 执行、审批/澄清交互及 T12.09 接管连续性仍未验收。
- 本轮没有连接外部主机，也没有上传或发送文件。
