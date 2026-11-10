# INT-01：共享合同与首批服务装配实施 TODO

> 开发量：按一个 AI 会话、前置就绪后的连续开发估算，最多一天；只计实现与交接记录。测试编写、运行和验收单独进行，不计入这一天。
> 范围估算不是完成承诺；发现超出一天时把剩余实现按文件/接口继续合并拆开，保持原验收条件。
> 当前状态：INT-01.01–04 已完成本地实现与定向验收。INT-01.01 已同步 DATA-02 扩展后的个人同步合同，并补充共享快照/变化流 fixture；INT-01.04 已补齐认证身份复核竞态和状态/角色更新后的 Workspace reconcile。完整套件的本机环境限制和其他任务失败见验证记录。所有改动只在本地；未使用测试机或用户提供的 API 凭据。
> 直接前置：可立即根据原需求与 DATA-01/HUB-01 的输入工作；各功能专属文件由其所属会话实现。
> 修改边界：独占公共 OpenAPI、路由、应用装配、数据库迁移与 sqlc 生成文件。
> 写入规则：[五会话执行入口](../并行执行入口.md)；只修改本会话的指定文件与本会话文档。公共合同/路由/迁移变更在本文件交接给会话 5。
> 本地约束：只在本地修改和验证，不发送或上传文件，不向远端部署。

## 本次开发

- [x] **INT-01.01（完成）落地首批共享接口并校正后续合同**：Hub 握手、初始 action、错误及能力声明；profile/default 的 GET/PUT/DELETE、版本冲突和幂等回执；Device 按安装 ID 管理与撤销整个 Client Installation；以及个人对象当前视图分页、固定变更流水位均写入 OpenAPI/共享 fixture。DATA-02 扩展后的同步能力、分页语义及 Client 凭证范围已与实现对齐；API 合同测试、OpenAPI 类型生成和 Portal typecheck 通过。
- [x] **INT-01.02（完成）装配首批数据库与路由**：个人对象/变化流迁移及既有会话、消息版本回填；Hub WebSocket、Device REST、profile 与 sync 路由、Client 凭证门禁、数据游标签名和启动装配均已接通。验收：API/app/domain/store/portal 聚焦测试及相关 PostgreSQL 集成通过；`make generate` 无误。
- [x] **INT-01.03（完成）维护共享交接**：已记录 DATA-01/HUB-01 所需输入、实现边界及待处理问题，见下方交接记录。
- [x] **INT-01.04（完成）联动账号与安装撤权**：账号身份变更、Client 安装凭证轮换/撤销与 Hub 会话关闭已接通；本次补充身份快照防旧密码竞态，并让状态/角色更新在关闭 Hub 后 reconcile。PostgreSQL 定向竞态用例及其 race 测试通过。

## 原任务与已有进度

下列编号用于追溯完整业务规则和拆分前证据；同一原编号涉及多个会话时，各会话只完成自身范围。原项全部实现及验收通过后才可视为完成。

| 原编号 | 原任务 | 拆分前状态 |
|---|---|---|
| [F04-T01.01](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 汇总原功能与去向 | 未开始，依赖 F04-T01.02、F04-T01.03、F04-T01.04、F04-T01.05、F04-T01.06 |
| [F04-T01.02](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 确定个人数据及同步合同 | 未开始 |
| [F04-T01.03](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 确定目标与例程合同 | 未开始 |
| [F04-T01.04](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 确定 Hub 与设备合同 | 未开始 |
| [F04-T01.05](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 确定记录与同意合同 | 未开始 |
| [F04-T01.06](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 按类别汇总管理能力映射 | 未开始 |
| [F03-T04.06](../../融合03-PC与iOS客户端融合实施任务todo清单文档.md) | 补齐 Task 快照与事件序号合同 | 完成 |
| [F03-T04.07](../../融合03-PC与iOS客户端融合实施任务todo清单文档.md) | 补齐消息恢复边界 | 完成：本地实现与合同验证，依赖 F03-T04.06 |

## 交接

- 在本文件记录完成的接口、文件和待会话 5 处理的公共改动；不要直接改其他会话负责的文件。
- 对应验证在 [独立验收清单](../验收资料/验收入口.md) 执行并记录，不计入本开发文档的一天预算。
- `server/AGENTS.md` 要求接口、实现和测试在同一变更交付；可先完成代码并标“待验证”，合入/提交前再按独立验收清单补齐测试与所需检查。

### 接口交接与未完成依赖

- **DATA-01 输入**：首批只开放 `profile/default`，严格限制 `name/avatar/emoji/color/description/style`。INT-01 按提案落地 `GET/PUT/DELETE /api/personal-data/{type}/{id}`；PUT/DELETE 使用 `Idempotency-Key`，body 带 `base_revision`（PUT 另带完整 profile `body`），成功返回不含正文的 `{object_type, object_id, result_revision, account_change_sequence, replay}`；冲突只回显 `base_revision/current_revision/deleted`。账号/对象变更表按 service instance 与 owner 隔离，既有 conversation/message 回填 revision=1；数据写入和读取的授权/CAS/序号锁由 DATA-01 实现。不要把会话正文复制进变更索引；快照与增量读取归 DATA-03。
- **HUB-01 输入与本次落地**：Hub hello/welcome、帧校验、首批 action 和连接管理由 HUB-01 提供。Device ID 即 Client Installation ID；PATCH 改 installation display name；DELETE 按 `DeviceService.Revoke` 撤销整个安装及其凭证并关闭 Hub 连接。INT-01 已注册 Hub/Device REST 路由并在 OpenAPI 明确该语义。Client 凭证绑定的 owner/installation 由服务器验证，不能信任 hello 自报身份；`KindDevice` 仍只用于 Workspace sync。Hub presence 不是可靠数据源，基础调用结果未知时不能伪报成功。
- **本次额外认领的共享路径**：`server/internal/api/{api.go,capabilities.go,capabilities_test.go,api_test.go,hub_route_test.go,personal_data.go,personal_data_test.go,personal_sync.go,personal_sync_test.go}`、`server/cmd/portal/main.go`、`server/migrations/00091_personal_data_sync.sql`、`server/internal/app/personal_sync.go`、`server/internal/app/personal_sync_test.go`、`server/internal/domain/personal/sync.go`、`server/internal/store/postgres/{client_login.go,queries/client_installations.sql,sqlcgen/client_installations.sql.go,sqlcgen/models.go,personal_sync.go,personal_sync_integration_test.go}`、`server/openapi/portal.yaml`、`server/openapi/fixtures/morde-client-v1.json`。`/api/hub`、`/api/devices`、profile API 与 `/api/sync/{snapshot,changes}` 均已装配；DATA-02 后同步 allowlist 包括受支持的个人对象与 Schedule，capability 按该实际范围声明。
- **本次认领的撤权联动路径（INT-01.04，本地实现和数据库集成均已完成）**：限定修改 `server/internal/app/client_login.go`、`server/internal/app/client_login_test.go`、`server/internal/app/user.go`、`server/internal/app/credentials.go`、`server/internal/app/credentials_test.go`、`server/internal/store/postgres/store.go`、`server/internal/store/postgres/admin_user_update_integration_test.go`、`server/internal/store/postgres/client_login_integration_test.go`、`server/cmd/portal/main.go`、`server/go.mod`。安装 Logout、LogoutWebSession、Revoke 在撤权提交后且 Workspace reconcile 前关闭对应 Hub 会话；成功的 ClientLogin 在凭据事务提交后关闭 installation 现有 Hub 会话。账号 status/role/password 变更在 reconcile 前关闭该 owner 的 Hub 会话，即使 reconcile 报错也关闭。身份变更事务同时将该 owner 的 Client Installation 标为撤权，供 Hub 握手复验拒绝撤权后的竞态连接。真实 Postgres 测试覆盖 installation 改名的 owner、撤权、过期边界。生产构造注入 Hub，DeviceService 现有显式关闭保留；`golang.org/x/net` 提升为直接依赖；不扩展到其他撤权场景，也不修改 Hub 实现文件。
- **本地核对证据（2026-11-02）**：DATA-01 首批 profile 提案明确 `Idempotency-Key`、`base_revision`、最小回执和窄冲突回显；Hub Device 实现明确整项安装撤权。个人读取在事务中重新校验账号、安装、token，并在同一查询读出对象正文与 revision；缺失/墓碑对象返回 not_found。迁移为五张个人数据/变更表加入 `service_identity` 外键、强制完整回执，并为现有账号初始化序号屏障、为 conversation/message 补 revision=1。Device 安装改名通过 sqlc `RenameClientInstallation` 查询实现。DATA-03 起初只开放 profile；DATA-02 扩展个人对象 allowlist 后，snapshot 以当前视图作 keyset 分页并固定变更流起点，不承诺跨页点时快照；变化日志暂按账号生命周期保留，因此当前不提供 cursor 过期响应。

### 本地验证记录（2026-11-02）

- `go test ./internal/api ./internal/app ./internal/domain/hub ./internal/domain/personal ./internal/store/postgres ./cmd/portal -count=1` 与 `make race` 中本次相关包通过；`make generate` 通过。早期 `domain/personal` 覆盖率为 97.7%；最终覆盖检查为 95.7%。
- 用一次性本地 PostgreSQL 容器执行账号序号锁、快照/增量固定水位、profile 读取授权、CAS/幂等/墓碑及回执回滚集成测试，普通与 `-race` 均通过；容器已删除。
- OpenAPI YAML 解析、195 个 operationId 唯一性、schema 引用解析、fixture JSON、`gofmt -d` 与 `git diff --check` 均通过。
- 全量 `make test`、`make race`、`make cover` 在 `internal/storage/workspacecurrent` 与 `internal/worker` 失败，错误指向 workspace 路径/权限与 symlink 检查（如“workspace current root path is not a safe regular file or directory”和“quota socket parent must not contain symlinks”）。使用本地 PostgreSQL 的 `make it` 中，INT-01 定向测试通过；全量套件另有 workspace sync 相关失败，以及 `TestClientTaskSubmissionIsVisibleToDesktopAndIOSInstallations` 报 `conversation_message_attachments` 表不存在，未在 INT-01 中追查。
- `make lint` 最初报告 5 项格式/静态检查；对相关 Go 文件作最小修正后复跑通过，结果为 `0 issues`。
- `CH_OSB_URL` 未配置，OpenSandbox 集成未运行；未连接用户提供的测试服务器，未使用用户提供的 GPT API key，未外发或上传文件。
- INT-01.04 定向验证：`go test ./internal/app ./cmd/portal -count=1` 通过；设置本地 `CH_TEST_DATABASE_URL` 后，`TestClientLoginCredentialRotationCancelsDeviceSyncSession`、`TestClientLoginRacingRevocationCannotRestoreInstallation`、`TestClientLoginHTTPPostgresSessionFenceAndLogout`、`TestClientLoginMigrationRetiresLegacyPairingAndCredentials`、`TestRenameClientInstallationRequiresOwnerAndActiveInstallation`、`TestUserUpdateUsesExpectedStatusAndRevokesCredentials` 与 `TestUserUpdateAndAuditAreAtomic` 均通过。目标 Go 文件 `gofmt -d` 无差异。
- 未连接用户提供的测试服务器，未使用用户提供的 GPT API key，未外发或上传文件。

### 最终复核补充（2026-11-02）

- 最终 `make lint` 通过（`0 issues`）；`make generate` 与 `npm run generate:api` 通过。Portal `typecheck`、`lint` 和 `npm test -- --run` 通过（35 个文件、251 项 Vitest 测试及 2 项 Node 脚本测试）。OpenAPI 解析/生成通过，195 个 `operationId` 无重复；fixture JSON 与 `git diff --check` 通过。
- INT-01 定向 `-race` 验收通过：API/app/PostgreSQL 相关用例（含 profile 对象、固定水位、schedule 投影、Client 安装撤权竞态及账号更新）通过；Hub/personal domain 全包以及 gateway/httpserver 全包通过。`domain/personal` 最终覆盖率为 95.7%。
- 全量 `make race` 和 `make cover` 仍未通过：PostgreSQL 全套在 Admin、Host sharing、Task/Workspace sync 等其他场景失败；worker Docker Workspace 测试因本机 `workspace sync quota socket owner directory permissions are unavailable` 失败，另有两项 Cloud Task E2E 超时/状态失败。`make cover` 还遇到 `internal/storage/sitefs/TestStoreSerializesFreeSpaceReservationsAcrossInstances` 失败。全量 `make it` 早先发现的个人对象测试计数和 schedule 测试重复创建 token 已修正，其针对性 race 用例随后通过；其余全量失败不在 INT-01 定向范围内。
- 本地 PostgreSQL 容器仅用于验证，未连接测试服务器、未使用所给 GPT API key、未发送或上传任何文件。

### 共享合同复核补充（2026-11-02）

- DATA-02 扩大同步对象后，OpenAPI 的 `personal_sync` 描述仍称 profile-only；快照描述也把当前视图分页写成稳定点时快照。现已说明支持对象与 Schedule，明确 watermark 只固定变更流起点、分页读取当前对象视图，并要求客户端按 revision/sequence 合并；`/api/clients` 的 GET/DELETE 认证声明收敛到 Portal session 或 Client credential。
- 共享 client fixture 新增 current-view snapshot 与 fixed-watermark change-feed 示例；API fixture test 校验对象 allowlist、正文、变更序号与 cursor。验证通过：`npm run generate:api`、Portal `typecheck`、`go test ./internal/api -count=1`、`make lint`、`make generate`、`go build ./...`、`git diff --check`。
- Portal `lint` 和 `test` 通过（42 个文件、290 个 Vitest 用例及 2 个 Node 用例）；INT-01 相关 PostgreSQL `-race` 定向 8 项通过，测试使用已清理的一次性本机 PostgreSQL 16。
- 服务端联跑在 PostgreSQL 全包仅因 `TestTaskHubFilePutStateValidationAndReceiptBinding` 的 Hub `file.put` allowlist 断言失败；该用例属于 HUB-03，不在 INT-01 范围。未因此开放 `file.put`。未连接测试机或使用用户提供的 API key。

### 本次复核补充（2026-11-02）

- 只读复核发现：密码验证后延迟进入 `LoginClient` 的请求可能在密码更新提交后创建安装；管理员状态/角色更新提交后关闭 Hub，但没有执行清单要求的 Workspace reconcile。现将通过密码验证的 `identity.User` 快照传给安装凭证事务，并在事务内锁定/比较 name、password hash、role、status、must-change-password；冲突按无效凭证拒绝。状态/角色实际变更现在在提交后关闭 Hub，再执行 Workspace reconcile；失败仍保持关闭并返回 upstream error。
- 新增 PostgreSQL 回归用例 `TestClientLoginRacingPasswordChangeCannotRestoreInstallation`；本机临时 PostgreSQL 上，包含该用例的 INT-01 账号撤权、个人快照/增量及相关集成测试普通与 `-race` 均通过。临时数据库仅绑定回环地址，已用于本地验证。
- 本次复核通过：`make lint`（0 issues）、`make generate`、`go build ./...`、`npm run generate:api`、Portal `typecheck` / `lint` / `test`（35 个文件、251 个 Vitest 用例、2 个 Node 用例）；Go API/app/hub/personal 定向测试及同范围 `-race` 通过；OpenAPI YAML 可解析，当前共享合同 203 个 operationId 均唯一；`gofmt -d` 与 `git diff --check` 通过。
- 全量 `make it` 未通过：本机 `workspacecurrent` 根路径/权限、worker quota socket owner 目录权限测试失败；PostgreSQL 全包另有 Host sharing、Task/Workspace sync 与 migration 测试失败，这些不在 INT-01 定向范围内。INT-01.04 定向 PostgreSQL 和 race 结果通过，未受上述全量失败影响。
- 未连接用户提供的测试服务器，未使用用户提供的 GPT API key，未外发或上传文件。
