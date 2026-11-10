# DATA-01：个人对象与原子变更基础实施 TODO

> 开发量：按一个 AI 会话、前置就绪后的连续开发估算，最多一天；只计实现与交接记录。测试编写、运行和验收单独进行，不计入这一天。
> 范围估算不是完成承诺；发现超出一天时把剩余实现按文件/接口继续合并拆开，保持原验收条件。
> 当前状态：DATA-01.01/.02 的个人对象事务、授权读写、版本/变更索引、会话/消息写入接入及迁移已实现，定向 PostgreSQL 和 race 验收通过。DATA-01 首批 mutation 合同以 profile 为准；DATA-02 扩展了其他个人对象，DATA-03 已接入十种个人对象的 snapshot/changes，DATA-06 又加入 Goal/Schedule 同步投影。最新本地 make lint 为 0 issues；全量 make test 仍受 workspacecurrent 路径、Store Hub file.put 用例和 worker 配额 socket 权限失败影响，详见验收记录。
> 直接前置：可先按现有设计实现；共享 schema/SQL/路由交由 INT-01 在同一变更中整合。
> 修改边界：个人数据 domain/app/store、会话/消息原业务写事务和必要的专属 API handler。
> 写入规则：[五会话执行入口](../并行执行入口.md)；只修改本会话的指定文件与本会话文档。公共合同/路由/迁移变更在本文件交接给会话 5。
> 本地约束：只在本地修改和验证，不发送或上传文件，不向远端部署。

## 本次开发

- [x] **DATA-01.01 建立个人对象和变更事务**：首批 DATA-01 mutation 合同限定 profile 白名单，按已鉴权 owner 执行条件版本更新；同一事务写对象、版本、账号序号、change log 和最小幂等回执。profile GET/PUT/DELETE handler 与 OpenAPI 合同已接入。真实 PostgreSQL 覆盖并发同 key 重放、异内容冲突、CAS、墓碑拒绝复活、撤销凭据后拒绝重放、失败回滚、同账号序号串行及不同账号并行；本轮全仓覆盖率命令显示 `domain/personal` 96.8%。后续类型扩展见 DATA-02。
- [x] **DATA-01.02 接入会话和消息版本**：会话创建、任务提交/追加消息、最终 assistant 消息与定时任务写入在原业务事务中接入版本索引/变更日志；迁移为已有会话/消息建立 revision=1 基线并为账号初始化 counter。真实 PostgreSQL 验证任务提交/重放、assistant 最终消息、schedule 会话/消息图及 change-log 顺序。assistant 流式快照继续由 Task 事件序号驱动，不单独进入账号同步流。

## 原任务与已有进度

下列编号用于追溯完整业务规则和拆分前证据；同一原编号涉及多个会话时，各会话只完成自身范围。原项全部实现及验收通过后才可视为完成。

| 原编号 | 原任务 | 拆分前状态 |
|---|---|---|
| [F04-T01.02](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 确定个人数据及同步合同 | 未开始 |
| [F04-T02.02](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 建立个人对象与原子变更流 | 未开始，依赖 F04-T01.02 |
| [F04-T02.11a](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 会话及消息接入变更流 | 未开始，依赖 F04-T02.02、F04-T02.12a |
| [F04-T02.12a](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 为会话和消息补齐同步版本 | 未开始，依赖 F04-T02.02 |
| [F04-T02.13](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 保护长期离线重试与删除 | 未开始，依赖 F04-T02.02 |

## 交接

- 本地实现文件：`server/internal/domain/personal/`、`server/internal/app/personal_service.go`、`server/internal/app/personal_service_test.go`、`server/internal/api/personal_data.go`、`server/internal/api/personal_data_test.go`、`server/internal/store/postgres/personal_mutations.go`、`personal_mutations_integration_test.go`、`personal_objects.go`、`personal_mutations_test.go`、`personal_objects_integration_test.go`、`account_changes.go`、`account_changes_integration_test.go`，以及会话 3 所有的 `task_conversation.go`、`task_execution.go`、`task_store_integration_test.go`、`schedule.go`、`schedule_integration_test.go`。个人 mutation app 接口为 `PersonalMutationService.ApplyPersonalMutation`；owner 只取自当前 Principal，返回的回执不含正文。
- **给 INT-01 的首批 API 合同**：`GET/PUT/DELETE /api/personal-data/{type}/{id}` 与 profile mutation body-free 回执已实现；`Idempotency-Key` 是稳定 mutation ID，body 提供 `base_revision` 与 profile 正文，不接受客户端 `owner_id` 或 `request_hash`。后续 DATA-02/03 已扩展个人对象 schema 和 snapshot/changes；DATA-06 增加 Goal/Schedule 投影。会话/消息 change log 已落库，但这些类型的同步正文读取仍未开放，不应据此宣称会话历史已加入个人对象同步。
- **profile 正文白名单**：只允许 `name`（1–60 字符）、`avatar`（`dragon` 或 `emoji`）、`emoji`（最多 16 字符）、`color`（`#RRGGBB`）、`description`（最多 200 字符）和 `style`（空值或 `muse|flat|clay|watercolor|pixel|line|sticker`）。`avatar=emoji` 必须提供 emoji 与颜色；`avatar=dragon` 不得附带两者。暂不接受 `face`，待个人附件存在并可验证引用归属后再开放；`connectors`、任何凭据/路径、未知字段以及未启用类型均拒绝。该字段集对齐本地旧 `LiveProfile`/`Profile` 结构；共享工作区现由 DATA-02 在 `server/internal/domain/personal/content.go` 中为后续类型定义独立白名单和正文校验。
- **权限、错误与冲突回显**：仅当前 `status=approved` 的 Session/Client Principal 可读写；服务端重新检查用户、token、安装有效性。错误使用现有 `unauthenticated`/`forbidden`/`invalid_argument`/`unsupported`/`request_too_large`/`not_found`/`conflict`/`idempotency_conflict` 码。版本冲突只回显 `base_revision`、`current_revision` 与 `deleted`，绝不回显正文；共享 `api.writeError` 已按冲突 reason 有限暴露这些字段。
- 共享迁移 `00091_personal_data_sync.sql` 已建立以下表，`service_instance_id` 外键指向 `service_identity`、`owner_id` 外键指向 `users`，所有主键包含实例和账号：`personal_objects` 主键 `(service_instance_id, owner_id, object_type, object_id)`，字段含 `body jsonb NULL, created_at, updated_at`；`personal_mutation_receipts` 主键 `(service_instance_id, owner_id, mutation_id)`，字段含 `request_hash bytea CHECK (octet_length(request_hash)=32), object_type, object_id, result_revision > 0 NOT NULL, change_sequence > 0 NOT NULL`；`account_change_counters` 主键 `(service_instance_id, owner_id)`，字段 `last_sequence >= 0`；`account_object_versions` 主键 `(service_instance_id, owner_id, object_type, object_id)`，字段含 `revision > 0, is_deleted, updated_at`；`account_change_log` 主键 `(service_instance_id, owner_id, change_sequence)`，字段含 `change_sequence > 0, object_type, object_id, revision > 0, operation, mutation_id NULL, created_at`。回执仅存请求摘要、对象标识、结果 revision/sequence，不存正文；在同一业务事务中完成正文、版本索引、变更日志后再插入完整回执，不能提交占位/半成品回执。删除应保留带版本墓碑；正文行可清理时，旧 ID 仍须由 `account_object_versions.is_deleted` 阻止复活。现有 conversations/messages 只在 `account_object_versions` 中补版本索引，不复制正文；迁移为现有会话/消息建立初始版本行，并为账号初始化序号屏障。
- 当前 `00091_personal_data_sync.sql` 已为所有新表添加 `service_identity`/`users` 外键，并为已有 conversations/messages 建立 revision=1 版本行及每账号初始 counter；receipt 的结果 revision/sequence 为非空正整数。历史对象没有逐条 change-log 记录，DATA-03 需通过 snapshot 确立客户端基线，再从后续 sequence 读取增量。
- 当前个人 handler 位于 `server/internal/api/personal_data.go`，同步 handler 位于 `server/internal/api/personal_sync.go`，启动注入、路由及 OpenAPI 均已接入共享工作区。snapshot/changes 现覆盖 DATA-02 十种个人对象以及 DATA-06 的 Goal/Schedule 投影；conversation/message 只占账号序号，不返回正文或标识。现有 `conversations` API 没有标题更新、消息更新或删除写用例；后续增加这些写路径时仍需接同一版本索引/变更流。assistant 流式快照继续由 Task 事件序号驱动；最终消息落入会话历史时与会话一起写同步变更。
- 本轮 PostgreSQL 验收覆盖个人对象/版本/日志/回执原子提交、并发同键重放与异内容冲突、撤权后拒绝重放、失败回滚，以及 `TestPostgresAccountChangeCountersSerializePerOwner` 对同账号 row-lock 串行和不同账号独立推进的断言；会话/消息/schedule 写入也有同事务 change-log 顺序断言。
- 不要直接修改会话 5 负责的迁移、查询/生成、OpenAPI、公共路由和启动装配；在本文件交接具体公共改动。
- 对应验证在 [独立验收清单](../验收资料/验收入口.md) 执行并记录，不计入本开发文档的一天预算。
- 历史定向验证记录（后续复核见下方）：`go build ./cmd/portal ./internal/store/postgres`、`go vet ./internal/api ./internal/app ./internal/domain/personal ./internal/store/postgres`、相关 API/app/domain 普通与 race 测试通过；`domain/personal` 覆盖率 97.7%。临时本地 PostgreSQL 16 上 `TestPostgresGetPersonalObjectAndRechecksAuthorization`、个人 mutation 并发/重放/CAS/墓碑/回滚、snapshot/changes 固定水位、同账号序号串行与不同账号并行、任务提交/重放、assistant 最终消息、schedule 会话/消息原子写入等定向用例及 `-race` 均通过。测试启动的容器已删除。该次全仓 `go test -count=1 ./...` 因 `storage/workspacecurrent` 路径安全检查和 `worker` quota socket 权限检查失败；当时 `make lint` 有 4 项范围外诊断。后续最新结果见 DATA-01 至 DATA-07 验收记录。`make generate` 当时未运行：本切片未改 sqlc 查询，生成物/查询由会话 5 负责。TODO 文档位于 `.gitignore` 忽略的 `docs/` 下，状态更新只在本地生效。
- `server/AGENTS.md` 要求接口、实现和测试在同一变更交付；可先完成代码并标“待验证”，合入/提交前再按独立验收清单补齐测试与所需检查。
- 早期 2026-11-02 本地复核：`go test -count=1 ./internal/domain/personal ./internal/app ./internal/api`、上述包 `-race`、`go build ./cmd/portal ./internal/store/postgres`、`go vet ./internal/api ./internal/app ./internal/domain/personal ./internal/store/postgres` 与相关文件 `gofmt -d` 均通过。临时 PostgreSQL 16 容器上，个人对象/授权/同步/变更序号、任务提交与消息、schedule 会话图、assistant 最终消息相关定向集成测试 `-race` 通过，容器已删除。该次全仓 `go test -count=1 ./...` 在 `storage/workspacecurrent` 路径安全与 `worker` quota socket 权限用例失败；当时 `make lint` 报 4 条诊断。当前工作区结果见 DATA-01 至 DATA-07 验收记录。
- 早期 2026-11-02 复核记录：`go test -count=1 ./internal/domain/personal ./internal/app ./internal/api`、相关包 `-race`、`go build ./cmd/portal ./internal/store/postgres`、定向 `go vet` 及 DATA-01 定向 PostgreSQL race 集成通过。完整 `go test -race ./internal/store/postgres` 当时观察到 10 项与 DATA-01 无关的 Workspace sync/receive 集成失败；当时 `make test`、`make race`、`make cover` 在 `storage/workspacecurrent` 路径和 `worker` socket 权限用例失败，`domain/personal` 覆盖率 96.8%；当时 `make lint` 有 4 项范围外 staticcheck 诊断。当前状态见 DATA-01 至 DATA-07 验收记录。完整包测试的临时 PostgreSQL 容器已停止并自动删除；`make generate` 当时未运行，因为本轮没有改 SQL 查询。
