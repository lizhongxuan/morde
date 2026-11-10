# DATA-03：快照、增量与离线冲突实施 TODO

> 开发量：按一个 AI 会话、前置就绪后的连续开发估算，最多一天；只计实现与交接记录。测试编写、运行和验收单独进行，不计入这一天。
> 范围估算不是完成承诺；发现超出一天时把剩余实现按文件/接口继续合并拆开，保持原验收条件。
> 当前状态：DATA-03.01/.02 服务端实现与聚焦验收完成，覆盖 DATA-02 十种 personal allowlist 类型及 DATA-06 的 Goal/Schedule 投影。快照 H 是增量起点；各页读取自身 REPEATABLE READ 视图，分页期间的写入由 H 后变更流补齐，客户端按对象 revision 合并；K 在快照完成后固定。账号变化日志按账号生命周期保留，因此当前无清理策略或游标过期响应。OpenAPI 已描述该同步范围、分页和水位合同。
> 直接前置：依赖 DATA-01；客户端可按合同并行接 outbox。
> 修改边界：服务端快照、分页、增量、cursor/tombstone 和服务端重试合同；客户端由会话 1/2 接入。
> 写入规则：[五会话执行入口](../并行执行入口.md)；只修改本会话的指定文件与本会话文档。公共合同/路由/迁移变更在本文件交接给会话 5。
> 本地约束：只在本地修改和验证，不发送或上传文件，不向远端部署。

## 本次开发

- [x] **DATA-03.01（完成）实现快照和增量**：范围为 DATA-02 十种个人同步 allowlist 类型。快照使用账号已提交水位 H 与 `(类型, ID)` keyset；HMAC 页游标绑定 owner、service instance、`personal/all` 范围、水位和位置，单页采用 `REPEATABLE READ`。H 是 changes 的起始位置，不表示所有页共享同一历史数据库快照；分页期间已写入的当前 revision 可能出现在后续页，也会出现在 H 后 change feed，客户端按对象 revision 合并。客户端完成 snapshot 分页后才固定本轮上界 K；K 后新写入由下一轮拉取，不能提前报为已同步。change feed 只返回个人 allowlist 事件；账号全局序号中的 conversation/message 事件会被跳过，终页推进到 K。验收：`TestPostgresPersonalSnapshotAndChangesUseFixedWatermarks` 与 `TestPostgresPersonalSyncDoesNotLoseWritesDuringSnapshotPaging` 在临时 PostgreSQL 16 上 `-race` 通过；新用例覆盖分页期间新增/更新/删除及 K 后持续写入。
- [x] **DATA-03.02（完成）处理离线恢复和删除**：游标签名和账号/实例隔离已实现；变化日志按账号生命周期保留，因此当前不会返回 cursor 过期。DATA-01 的幂等回执与持久删除版本确保旧 ID 重试不能复活对象；客户端 outbox/冲突草稿恢复由客户端切片负责。若未来清理变化日志，需先增加可核验保留水位和要求客户端重取快照的过期游标合同。验收：`TestPostgresPersonalMutationCASIdempotencyAndTombstone` 覆盖 CAS、墓碑、已删除对象的同键重放及撤权后拒绝；`TestPostgresAccountChangeCountersSerializePerOwner` 覆盖同账号提交顺序屏障。

## 原任务与已有进度

下列编号用于追溯完整业务规则和拆分前证据；同一原编号涉及多个会话时，各会话只完成自身范围。原项全部实现及验收通过后才可视为完成。

| 原编号 | 原任务 | 拆分前状态 |
|---|---|---|
| [F04-T02.06](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 实现快照与增量续读 | 未开始，依赖 F04-T02.02 |
| [F04-T02.09](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 处理过期游标与中断恢复 | 未开始，依赖 F04-T02.06 |
| [F04-T02.10](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 实现删除标记与冲突保护 | 未开始，依赖 F04-T02.06 |
| [F04-T02.13](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 保护长期离线重试与删除 | 未开始，依赖 F04-T02.02 |

## 交接

- 在本文件记录完成的接口、文件和待会话 5 处理的公共改动；不要直接改其他会话负责的文件。
- **DATA-03 快照与增量语义**：快照 H 是首请求读取到的账号已提交序号，作为 change-feed 起点；由于没有跨 HTTP 请求的数据库事务，各页在各自 `REPEATABLE READ` 视图读取当前对象。分页期间的对象写入序号都大于 H，会在 snapshot 后的增量读取中出现；若对象已在页中以相同或更高 revision 出现，客户端不得应用较旧事件覆盖它。客户端在完成全部 snapshot 页之后才请求首个 changes 页并固定 K；K 后写入保留到下一轮，避免分页持续写入导致无限追赶。固定范围为 DATA-02 的十种 personal allowlist 类型及 DATA-06 的 Goal/Schedule 投影；conversation/message 只占账号 sequence，不返回事件正文。账号 change log 按账号生命周期保留，所以当前不会生成过期游标；未来清理必须先增加保留下界、拒绝低于下界的游标并要求重取快照。
- **公共合同交接状态**：`server/openapi/portal.yaml` 已补齐 snapshot 的当前视图与 H 水位语义、完成分页后读取 changes、changes 固定 through_sequence K、按 revision 合并、不返回正文、过滤 conversation/message 后的 sequence 间隙及终页推进到 K。变更日志按账号生命周期保留，未定义 retention policy；若以后清理，须先增加游标下界和重取快照响应。
- **客户端衔接（会话 1/2）**：snapshot 临时视图、个人 outbox 与冲突草稿按账号隔离；直到 snapshot 与固定 K 增量都成功后才一起提交。upsert 事件按 revision 读取/合并对象，delete 事件应用 tombstone；失败/退出/切换账号时保留旧视图及未发送 outbox。会话 1/2 按客户端任务各自验收。
- **早期 2026-11-02 本地验收记录**：`go test -race -count=1 ./internal/domain/personal ./internal/app`、个人 domain 覆盖率（96.8%）、`go vet ./internal/domain/personal ./internal/app ./internal/store/postgres`、`gofmt -d` 通过；PostgreSQL 16 定向 `go test -race -count=1 -run '^TestPostgres(Personal|AccountChange)' ./internal/store/postgres` 通过。Personal Sync API 路由测试 `-race` 曾通过临时 overlay 运行；当时 `admin_console.go` 未使用 import 和 API 管理端用例影响全量检查，lint 有 4 条诊断。当前复核见本文件后续记录及 DATA-01 至 DATA-07 验收记录。
- 未运行 `make generate`：本任务没有改 SQLC query 或生成输入；会话 5 正持有 query/sqlcgen 文件边界，避免重生成覆盖其未提交改动。
- 对应验证在 [独立验收清单](../验收资料/验收入口.md) 执行并记录，不计入本开发文档的一天预算。
- `server/AGENTS.md` 要求接口、实现和测试在同一变更交付；可先完成代码并标“待验证”，合入/提交前再按独立验收清单补齐测试与所需检查。
- 2026-11-02 后续复核：当前 `make lint` 通过（0 issues），`make test` 中 `internal/api` 通过。此前临时 overlay 修复的 `admin_console.go` 未使用 import 问题已不再复现；全量测试仍有 workspacecurrent、Store Hub file.put 和 worker 配额 socket 权限失败，见 DATA-01 至 DATA-07 验收记录。
