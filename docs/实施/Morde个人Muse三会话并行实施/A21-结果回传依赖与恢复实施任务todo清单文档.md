# A21 结果回传、依赖与恢复实施任务 TODO

**会话：A｜阶段：P2 后半 / G2｜状态：已完成（2026-11-10；T1/T2/T3/T4 完成，G2 本地验收通过）。**

来源：[需求与架构](../../Morde个人Muse需求与架构设计方案.md)；共同规则：[执行总览](00-三会话执行总览.md)；接口：[共享合同](01-共享接口与联调合同.md)。

## 开始条件与修改边界

A20-T4；B20/C20 同期继续。

server 内结果汇总/发布、Task 修订、依赖和操作账本相关模块；云端 DSH 的结果投递/消费和恢复；必要数据库迁移及 OpenAPI。客户端仍只读。

## 接口变化

落实 K2/K3：来源任务/修订、子 Job 状态、result_review、dependencies、回复来源及消费去重；已有 TaskStatus 不改变为另一套工作流枚举。

## 任务

- [x] A21-T1（完成）持久回传结果并统一最终回复发布
  - 前置：A20-T4。
  - 修改：在现有任务结果提交中补待回传关联；复用持久发布/幂等机制，增加协调唤醒与消费。原始结果可查，主助手解释单独关联；旧拼接发布器不得与主助手重复发最终回复。
  - 验收：主助手空闲、忙碌及重启三种情况下都能接到已完成子结果；重复事件只产生一次动作/回复。整理失败保留原始结果并显示真实状态，不能重跑子任务掩盖失败。
  - 完成：Task 结果、revision/event/auth 摘要与摘要哈希在 PostgreSQL 持久化；同一结果经隐藏路由进入现有主协调 worker 生成解释回复，原始结果仍在 Task 账本，不从 `Task.result` 再造可见回复。处理、发布、重放、超时恢复、修订替换和失败状态由 `conversation_result_batches/sources` 统一管理；消息/Checkpoint/任务上下文不暴露内部触发消息。结果审阅与重放 Postgres 集成、worker 路由回传用例均通过；公开 OpenAPI/K2 投影已同步。
  - 复开与完成证据（2026-11-10）：G1 真实服务验收发现隐藏结果审阅消息未带模型选择，owner 默认模型未就绪时回传以 `model_unavailable` 失败。现由隐藏结果审阅路由从同会话最新来源 Task 继承 `model_id`，查询按 service instance、owner、actor 与 conversation 隔离；`TestConversationResultReviewUsesLatestSourceTaskModel` 和结果聚合/回传 worker 定向测试通过。PC 本地真实服务运行 `G2PC-20261110-013` 完成 A 修订、B 取消、主助手发布 A 结果；重载并重新打开会话后最终回复仍恰好一条，来源为 Task A revision 1 / event 9。

- [x] A21-T2（完成）补齐任务依赖、修订与停止边界
  - 前置：A21-T1。
  - 修改：既有 Task/子会话记录中补依赖所需结果版本及有界关系，统一归属与循环检查。延用修订、租约和停止证明；冲突工具访问经现有 RuntimeControl/操作账本。
  - 验收：依赖未满足不提前执行，前置失败/取消会明确阻塞；连续修改 A 只更新最新版，旧结果无效；停止 B 只停止其执行树，共享桌面/写操作不能并发抢占。
  - 完成：新增有 owner/conversation 归属、`required_revision` 与自依赖约束的依赖记录；候选由协调器限制为同会话活动任务，创建时在事务内重验 initiator/project/revision。依赖未满足的 Task 不可认领；满足后将确切原始结果作为不可信上下文交给 DSH。K2 投影仅暴露同 owner/initiator/project 范围，依赖 PostgreSQL 集成和 worker 单任务停止/同 Runtime 并行测试通过。

- [x] A21-T3（完成）验证事件重放与未知操作恢复并交付 API
  - 前置：A21-T2。
  - 修改：对应 Go/Node/数据库隔离测试和必要缺口修复；正式 K2 字段、事件回放与两端样例同步。运行现有相关真实 adapter 测试，不以测试文件存在替代通过。
  - 验收：覆盖 A3–A6：进程重启、重复认领、迟到结果、修订、取消和副作用未知；确认不重复操作。发布 B20-T3/C20-T3 的真实服务版本。
  - 完成：Go/Node/数据库回放与恢复路径、正式 K2 DTO 投影和 OpenAPI/共享样例已同步；事件重放、进程重启、重复认领、迟到结果、修订、取消、未知副作用恢复及幂等由现有集成测试覆盖。列表/active 与 detail 使用同一投影规则，权限过滤后不泄露依赖或结果审阅信息。
  - 验收证据：DSH Runtime 94/94、Portal typecheck、worker 全包通过。K2 API/App 定向用例 `TestListTasksReturnsStepCounts`、`TestConversationActiveTasksIncludesCursor`、`TestTaskDetailMapsDependenciesChildJobsAndResultReview`、`TestTaskServiceListHidesTasksWithoutHostReadAccess` 通过；PG 定向组 `Test(ConversationResultAggregation.*|ConversationRouteTaskDependencyWaitsForAndReceivesExactResult|PostgresTaskLeaseFencesOperationAndRecoversUnknown|PostgresTaskK2Projection)` 在 loopback PostgreSQL 上无竞争复跑通过（34.417s）。`make test race cover` 通过；`make it` 使用专用 loopback PostgreSQL、`GOFLAGS=-timeout=20m` 和 `TMPDIR=/private/tmp` 全包通过（PostgreSQL 824.827s、worker 170.438s）。OpenSandbox 集成按 Makefile 因未配置服务地址而跳过。sqlc 固定版本 v1.31.1 从本地 module cache 执行 `make generate`，临时副本生成 22 个文件并与工作区 `sqlcgen` 完全一致；工作区未被生成器覆盖。已安装 `golangci-lint` 直接检查为 0 issues。C10/G1 Simulator App-close 用例已用批准的本地 owner 通过，重启与对账有独立结果包。
  - 边界：仅本地交付与验证，未向远端发布服务。真实 Portal+DSH 的跨端运行环境由 C20-T3 准备，G2 结果见 A21-T4。

- [x] A21-T4（完成）合入 PC/iOS 协调界面并通过 G2
  - 前置：A21-T3、B20-T3、C20-T3。
  - 修改：在 A 独立 fixture 合入 B20/C20 交付，记录事件来源、任务归属、审批和重连显示；公共修正归 A，客户端问题退回所属会话。
  - 验收：同一对话 A/B 并行，A 可被修订，B 可独立停止；子结果回传由主助手处理，两端仅显示同一最终消息。B/C 真联调和必要检查通过后记录 G2。
  - B20-T3/T4 本地证据（2026-11-10）：Portal + DSH 真实服务运行 `G2PC-20261110-013` 完成同会话 A/B 并行、A 修订至 revision 1/1、B 独立取消、主助手消费 A 结果并发布带来源的最终回复；重载并重新打开会话后最终回复仍恰好一条，来源为 Task A/revision 1/event 9。Chromium Playwright 助手 E2E 4/4；Vitest 与 `TestListConversationsOrdersAndPaginatesByLastActivity` 定向测试通过；G1 浏览器验收确认主会话置顶、旁路默认折叠、每页 5 条。B20 已完成交付。
  - C20-T3 本地真服务证据（2026-11-10）：本地 Portal + DSH 与 loopback synthetic model proxy；`Morde C20 T3 Local UI 20261110`（iPhone 18 Pro / iOS 27.2 build `24B8117`）上 `testC20PortalConcurrentResultReviewPersistsAcrossAppRestart` 通过（1/1，0 失败、0 跳过，结果 `.local/c20-t3-20261110/results/C20-G2-local-215B22F971.xcresult`）。同一对话创建 A/B，B 进入 `waiting_user` 后被独立取消而 A 仍运行；A 修订到 revision 1，child 成功，主协调器发布来源为 Task A/revision 1/event 13 的唯一回复。重启 App 后唯一回复、来源卡片、任务详情、结果审阅和 child 状态均恢复。用例使用仅监听 loopback 的本地合成模型，不调用外部模型。
  - A21-T4 同主会话 PC 只读回验（2026-11-10）：同一合成账号通过本地 Portal 页面 `http://127.0.0.1:24951/assistant` 打开该主会话；PC 显示 B 已停止、A 已完成 revision 1，结果审阅为 `published`，来源为 Task A/revision 1/event 13，`reply_message_id=mo3fv258`，聊天中只显示这一条最终回复。与 iOS 用例的唯一回复断言对应，补足两端查看同一主会话的联验。
  - A21 恢复边界回归（2026-11-10）：`go test ./internal/app ./internal/worker ./internal/dshguardian ./cmd/portal`、`go test ./internal/worker` 通过；`TestPostgresExpiredAccountTaskStopRecoveryRequiresExactCurrentFences` 在隔离临时 loopback PostgreSQL 通过；已安装 `golangci-lint` v2.13.2 检查为 0 issues。仓库固定的 `make lint` 版本 v2.15.0 不在本地 module cache，离线环境未下载依赖，故改用已安装版本检查。验证覆盖过期停止认领的精确 task/action/owner、Agent generation、Runtime fence，确认只恢复目标 Task，不停止共享 Runtime 或兄弟任务。
  - 集成环境（2026-11-10）：本地 Linux ARM64 Portal 构建 SHA-256 `b33fbcfdc01a98eae3d8f8e8c943e86809acc44c38cc0f0c1793cc9bb661691a`，迁移至 schema 174 后 `/healthz` 与 `/api/capabilities` 均返回 200。PC 与 iOS 分别通过同一本地 Portal + DSH 功能链路完成真服务验收；未访问用户提供的公网测试机，未部署远端或外发文件。

## 交付

T3、B20-T3、C20-T3 已完成，A21-T4 已验收并记录 G2；P3 的 A30/B30/C30 可按总览继续领取，不把文件功能混入本轮。只汇报本地位置、版本、验证及未完成项；不得外发文件。已有实现通过本任务验证可直接复用，不为勾选任务而重写。

