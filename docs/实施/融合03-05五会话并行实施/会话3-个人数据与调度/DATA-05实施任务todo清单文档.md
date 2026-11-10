# DATA-05：个人数据额度实施 TODO

> 开发量：按一个 AI 会话、前置就绪后的连续开发估算，最多一天；只计实现与交接记录。测试编写、运行和验收单独进行，不计入这一天。
> 范围估算不是完成承诺；发现超出一天时把剩余实现按文件/接口继续合并拆开，保持原验收条件。
> 当前状态：DATA-05.01/.02/.03 已完成本地实现和定向验证。Goal 复用个人对象唯一正文、通用 CRUD/同步版本及同事务额度核算；专用 `/api/goals` 合同已由 DATA-06 补齐。2026-11-02 定向 PostgreSQL、app/domain race 验证通过；最新 internal/api 测试和 make lint 通过，全量 Store 测试仍有范围外 Hub file.put 用例失败。
> 直接前置：依赖 DATA-01；附件依赖 DATA-04，Goal 使用 personal object 账本；专用 Goal API 合同交 DATA-06。
> 修改边界：普通个人对象与附件的账号用量、预留及准入；不修改 Workspace、聊天或记录额度。
> 写入规则：[五会话执行入口](../并行执行入口.md)；只修改本会话的指定文件与本会话文档。公共合同/路由/迁移变更在本文件交接给会话 5。
> 本地约束：只在本地修改和验证，不发送或上传文件，不向远端部署。

## 本次开发

- [x] **DATA-05.01 建立个人额度和查询**：默认 5 GiB/账号、300 MiB/附件；管理员按版本 CAS 调整，审计记录成功变更；存量用量按个人对象、Schedule 配置及仍被个人资源库引用的已提交附件回填，并与其他额度隔离。
- [x] **DATA-05.02 接入对象和附件准入**：个人对象正文增量与变更日志同事务，附件支持预留/提交/过期回收；超额写入回滚，保留旧内容并释放失败提交的预留。
- [x] **DATA-05.03 覆盖各类个人写入口**：资料、记忆、动态、点子、资源库、非秘密配置、Schedule 与 Goal 均经统一额度准入。Goal 正文存于 `personal_objects`，使用既有个人数据 CRUD、revision、change log 和幂等回执；资料引用仅允许当前账号仍有效的 `library_item`。

## 原任务与已有进度

下列编号用于追溯完整业务规则和拆分前证据；同一原编号涉及多个会话时，各会话只完成自身范围。原项全部实现及验收通过后才可视为完成。

| 原编号 | 原任务 | 拆分前状态 |
|---|---|---|
| [F04-T02.14](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 落实个人数据存储准入 | 未开始，汇总，依赖 F04-T02.14a、F04-T02.14b、F04-T02.14c、F04-T02.14d、F04-T02.14e、F04-T02.14f、F04-T02.14g、F04-T02.14h |
| [F04-T02.14a](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 建立个人数据额度与用量 | 未开始，依赖 F04-T02.02 |
| [F04-T02.14b](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 将首个个人对象写入接入额度 | 未开始，依赖 F04-T02.14a |
| [F04-T02.14c](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 将个人附件接入额度与预留 | 未开始，依赖 F04-T02.14a、F04-T02.07 |
| [F04-T02.14d](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 将目标和计划接入额度 | 未开始，依赖 F04-T02.14a、F04-T02.11b |
| [F04-T02.14e](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 将个人资料接入额度 | 未开始，依赖 F04-T02.14b、F04-T02.01 |
| [F04-T02.14f](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 将记忆、SOUL 和日记接入额度 | 未开始，依赖 F04-T02.14b、F04-T02.03 |
| [F04-T02.14g](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 将动态、点子和资源库接入额度 | 未开始，依赖 F04-T02.14b、F04-T02.04 |
| [F04-T02.14h](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 将非秘密配置接入额度 | 未开始，依赖 F04-T02.14b、F04-T02.05 |

## 交接

- 在本文件记录完成的接口、文件和待会话 5 处理的公共改动；不要直接改其他会话负责的文件。
- 对应验证在 [独立验收清单](../验收资料/验收入口.md) 执行并记录，不计入本开发文档的一天预算。
- `server/AGENTS.md` 要求接口、实现和测试在同一变更交付；可先完成代码并标“待验证”，合入/提交前再按独立验收清单补齐测试与所需检查。
- 2026-11-02 Goal 接入验证：`go test -race -count=1 ./internal/domain/personal ./internal/app` 通过；临时本地 worktree（叠加本轮 Goal/domain/store 文件）上的 `go test -race -count=1 ./internal/store/postgres -run '^TestPostgres(GoalUsesPersonalQuotaVersionAndOwnerLedger|PersonalContentObjectsAreOwnerScopedVersionedAndSyncable)$'` 通过，覆盖 Goal CRUD/tombstone、同步快照/变更、版本冲突、跨账号读取与资料引用拒绝、超额扩容回滚、缩小/删除释放额度；`go build ./cmd/portal ./internal/store/postgres`、`go vet ./internal/app ./internal/domain/personal ./internal/store/postgres` 通过。当前主工作区 Store 测试被既有 `record_access.go` 参数不匹配阻断；API 测试被既有 `admin_records_test.go` 接口签名不匹配阻断。测试均未访问远端。
- 前序 2026-11-02 本地验证仍有效：临时 PostgreSQL 16 上 DATA-05 quota/admin/附件/Schedule 定向 race 集成通过；`go build ./cmd/portal ./internal/store/postgres`、`make generate`、Portal typecheck 与 4 个 Vitest 文件（40/40）通过。`make lint` 留有范围外 `record_access.go`、`admin_controls.go`、`record_consent.go` 诊断；全仓测试/race/cover 未执行。
