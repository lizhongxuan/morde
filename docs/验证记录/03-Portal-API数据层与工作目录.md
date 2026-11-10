# 最终资源与会话收尾（2026-10-20）

专用QA数据库在断言仅含basic-admin和已禁用探针账户后，撤销45条遗留session；内置浏览器确认旧会话跳转登录，再重新登录保留一个有意的本地演示会话。最终有效API Key为0、活跃Host为0；仅保留合成Workspace we4htluw（detached）及其ws-we4htluw卷，供查看重挂结果。临时TLS Host/卷/Caddy/Portal/CA均已清理，主Portal readyz200。

# 当前结论：基础版数据与真实生命周期通过（2026-10-20）

单一初始迁移仅含 users/workspaces/hosts/tokens/audit_logs 五表。真实本地PostgreSQL已执行两次migrate up确认幂等、user add、serve；sqlc重新生成7文件前后哈希相同。全量build/lint/test/race/PG/cover通过，internal汇总84.7%，各包均≥80%。

实际Portal→固定OSB/runsc→Gateway→Workbench的创建/进入/续期/删除，以及保留Workspace→新Host同卷重挂→中文内容读回通过；真实Playwright从UI创建到终端写文件、文件读取、清理通过。双账号实际越权、ticket重放、Key撤销与跨用户MCP session均拒绝。限流、过期、角色更新、结果不明Create不重试和删卷CAS由回归覆盖。测试只使用合成内容。最终管理员跨owner签票回归确认not_found且不产生票据；app覆盖率80.2%，Portal26项测试通过。

真实修复：共享Gin WriteError补Abort，阻止错误响应后继续执行；MCP当前请求身份与session绑定。后文六表/旧接口/缺OSB等均为历史阶段，已删除的功能不恢复；当前实现与状态以实施00–07为准。

---

# 03 Portal API、数据层与工作目录验证记录

> 日期：2026-10-20（Asia/Shanghai）
> 范围：记录本次已完成的契约项；未执行或依赖环境的验收不视为通过。

## A03.01 OpenAPI 契约

- 命令：`REDOCLY_TELEMETRY=off npx --yes @redocly/cli@2.64.1 lint server/openapi/portal.yaml`
- 结果：退出码 0，OpenAPI 3.1.0 文档通过，共 23 个路径；包括 11.3 全部 API、平台 `/mcp` 与健康检查。
- 资源集合均声明为数组类型；统一错误结构包含 15 个错误码和 413 `request_too_large`。
- Redocly 报 3 条 warning，不影响有效性：`info.license` 未提供（项目未声明许可证）；`/healthz`、`/readyz` 未列 4xx（按实际健康检查行为保留响应语义）。

## A03.02 数据库迁移与 sqlc 生成

- `server/migrations/00001_init.sql` 含 users、workspaces、hosts、tokens、notifications、audit_logs 六表、`workspaces_one_per_host` 部分唯一索引与 goose Up/Down；没有触发器。
- `migrations.FS` 以 `embed` 只读嵌入迁移 SQL；`server/sqlc.yaml` 指向 migrations/ 与 queries/，生成 `pgx/v5` 代码。
- 命令：`make generate`、`go mod verify`、`go test ./...`（工作目录 `server/`）。结果：sqlc 生成成功、全部模块校验成功、全部 Go 包编译与测试通过。

## 本地测试与未执行边界

- 最终本地 Go 门禁：临时 PostgreSQL 已运行，`CH_TEST_DATABASE_URL=… make it`、含数据库的 `go test -race ./...`、`go vet ./...`、`go build ./...`、`make lint`、`go mod verify` 均通过。Store 迁移、原子操作及数据库集成测试已实际运行，包括 session advisory lock。`CH_OSB_URL`、`CH_OSB_API_KEY` 未配置，OpenSandbox 集成测试仍 Skip，真实 Portal→OSB/AIO 生命周期未执行。
- 含 DB `make cover` 汇总 84.3%，所有 internal 包均 ≥80%；`internal/store/postgres` 84.4%、`internal/worker` 81.9%、`internal/app` 80.2%。
- `deploy/single/verify/03-portal-api.sh`、`deploy/single/install.sh`、`deploy/single/systemd/cloudhost-portal.service` 已存在；所有 `deploy/**/*.sh` 脚本均通过 `bash -n`。ShellCheck 当前不可用，install/systemd 流程没有在 Linux root/systemd 环境实跑。
- 公开预览端口的并发增删已改为 Store 原子操作（`HostStore.SetHostPreviewPublic`），`internal/app/host_service_test.go` 的并发回归覆盖此行为；先前记录的读后写覆盖风险已解除。

## T03.19 worker 修复与复验边界

- 已实现按主机持有 PostgreSQL session advisory lock，避免多个 Portal 实例同时推进同一创建任务；锁占用时当前实例跳过推进，保留 creating 状态与 Workspace 挂载。相关实现位于 `server/internal/app/host_create.go`、`server/internal/store/postgres/tx_leader.go`，替身锁回归由 `TestHostServiceAdvanceCreateSkipsWhenAnotherWorkerHoldsCreateLock` 覆盖。
- 对账现在把远端 `SandboxFailed` 按失败 sandbox 处理并进入安全清理；`server/internal/app/worker_test.go` 的 `TestWorkerAppReconcileFailsHostWithFailedSandbox` 覆盖该状态。创建队列路径的 `AdvanceCreate` 会回报 `create_timeout` action，由 worker 计入 `cloudhost_reconcile_actions_total`；`server/internal/worker/worker_test.go` 的 `TestWorkerRecordsCreateTimeoutFromAdvanceCreate` 断言该路径计数为 1。
- 以上 worker 实现与回归测试已通过本地全量 race/vet/build/lint；PostgreSQL session advisory lock 双实例集成由临时数据库 `TestAtomicPostgresOperations` 覆盖并通过（经 `CH_TEST_DATABASE_URL=… make it` 及含数据库 race 全量测试运行）。真实 OpenSandbox/AIO 生命周期仍因 OSB 配置缺失未执行，因此只确认本地与数据库集成通过，不宣称真实编排服务验收完成。

## 2026-10-20 基础版最终复核补记

本轮根任务在本机临时 PostgreSQL 可用时运行全量 build/test/race/cover；均通过，覆盖率汇总 84.4%，所有 `internal/` 包均 ≥80%。`CH_TEST_DATABASE_URL=… make it` 中的迁移、Store 与原子操作集成测试实际连接临时数据库。未配置 `CH_OSB_URL`、`CH_OSB_API_KEY`，真实 OSB/Host 生命周期没有执行；因此 T03.24 保持待验证，不将本地单测、数据库集成或浏览器 mock 记为真实 Host E2E。

| 任务 | 本轮本地证据 | 边界 |
|---|---|---|
| T03.01 | `REDOCLY_TELEMETRY=off npx --yes @redocly/cli@2.64.1 lint server/openapi/portal.yaml` | 本地 OpenAPI lint 通过 |
| T03.02 | `TestApplyMigrations`、`TestStoreRoundTrips`、`make generate` | 当前初始化迁移与 sqlc 在本地/临时 PostgreSQL 验证 |
| T03.03 | `TestValidateTransition`、`TestNewAndParseCredential`、`TestActionValid`、`TestHostServiceConstructorRejectsMissingDependencies` | domain 与 app 接口本地验证 |
| T03.04 | `TestWriteErrorMappings`、`TestWriteErrorBodyRetryAfterAndLogOnce`、`TestWriteErrorStopsDownstreamHandlers`、`TestRecoveryHidesPanicAndFinalizesRequest`、`TestNewServerUsesHTTPTimeouts` | httpserver 错误、Abort 终止语义、恢复和超时本地验证；回归先复现错误响应后仍运行下游，再由共享 `WriteError` Abort 修复 |
| T03.05 | `go build ./...`、`TestStreamableHTTPAuthToolsAndAIO`、`TestLifecycleAndPrivatePreviewTools` | 本地构建和 Adapter 测试通过；不代表真实 Host 链路 |
| T03.06 | `TestRequireLoopbackMetrics`、`TestMetricsHandlerOnlyExposesMetrics`、`TestPortalCommandUsageErrors` | Portal 入口与 metrics 本地测试通过 |
| T03.07 | OpenAPI lint、`TestWriteErrorMappings`、`TestDomainErrorResponsesAreMappedAndSanitized` | 当前 HTTP/domain 合同本地检查通过 |
| T03.08 | `TestStoreQueriesAndConversions`、`TestStoreDatabaseErrors`、`TestDomainConversions`、`TestStoreRoundTrips` | Store/行模型转换及数据库读写本地验证 |
| T03.09 | `TestAtomicPostgresOperations`、`TestDetachWorkspaceRequiresCurrentHostAttachment`、`TestHostServiceAdvanceCreateSkipsWhenAnotherWorkerHoldsCreateLock` | 临时 PostgreSQL 下原子操作和并发测试通过 |
| T03.10 | `TestValidateTransition`、`TestValidateTransitionRejectsIllegalEdge`、`TestNewAndParseCredential`、`TestVerifySecret` | 状态边界和凭证哈希本地测试通过 |
| T03.11 | `TestCredentialServiceLoginVerifyLogoutAndRateLimit`、`TestCredentialServiceTicketConsumesOnceAndPropagatesStoreErrors`、`TestCoverageAPIKeyLifecycleAndCurrentUser`、`TestLoginMeAndLogoutKeepCredentialOutOfJSON` | app/API 身份与用户管理测试通过 |
| T03.12 | `TestAuditServiceValidatesAndRedactsBeforeWrite`、`TestAuditServiceListsOnlyForAdminWithBoundedFilters`、`TestCountedAuditWriterCountsFailures` | 审计查询、脱敏及失败计数本地验证 |
| T03.13 | `TestDeleteVolumeUsesDockerUnixSocketAPI`、`TestDeleteVolumeHonorsCanceledContext`、`TestWorkspaceServiceAttachedIsBusyAndVolumeFailureCanRetry` | Docker 卷删除与占用重试本地验证 |
| T03.14 | `TestSealerAuthenticatesCiphertext`、`TestRuntimeResolvesCachesAndInvalidatesEndpoint`、`TestRuntimeFlushActivityPreservesFailedBatch` | 加密、Endpoint 缓存及活跃度本地验证 |
| T03.15 | `TestHostServiceAdvanceCreateNeverRetriesAmbiguousCreate`、`TestHostServiceAdvanceCreateKeepsWorkspaceWhenRemoteStateIsUnknown`、`TestHostServiceAdvanceCreateRecoversMetadataAndDeletesDuplicates` | Create 结果不明和 Workspace 保护由替身测试覆盖；真实 OSB 待验证 |
| T03.16 | `TestHostServiceAdminListAndOwnershipHideForeignHosts`、`TestHostServiceDeleteClaimsOnlyWorkspaceStillAttachedToHost`、`TestHostServiceRenew`、`TestHostServiceIssueTicket` | HostService 权限、生命周期和 ticket 本地验证 |
| T03.17 | `TestCoverageRuntimeLookupAndSandboxErrors`、`TestHostOperationsAndMissingDependencyResponses`、`TestWriteErrorMappings` | `host_not_ready` 与不可操作状态本地验证 |
| T03.18 | `TestWorkspaceServiceHidesDeletedAndRejectsForeignOwner`、`TestWorkspaceServiceAttachedIsBusyAndVolumeFailureCanRetry`、`TestDetachWorkspaceRequiresCurrentHostAttachment` | Workspace owner、删除保护及 PG 认领约束本地验证 |
| T03.19 | `TestWorkerAppReconcileDeletesOnlyWorkspaceStillClaimedByHost`、`TestWorkerAppDeletesIdleHostAndRenewsActiveHost`、`TestWorkerAppReconcileFailsHostWithFailedSandbox`、`TestAtomicPostgresOperations` | worker 对账与 DB 锁/原子操作测试通过；真实 OSB 清理待验证 |
| T03.20 | `TestInvalidRequestsAndOriginAreRejected`、`TestAuthenticationAndHandlerFailureBranches`、`TestAdminRoutesAndValidation` | Origin、Content-Type、鉴权和 admin 本地验证 |
| T03.21 | `TestTemplateWorkspaceAndAPIKeyRoutes`、`TestHostOperationsAndMissingDependencyResponses`、`TestLoginMeAndLogoutKeepCredentialOutOfJSON`、`TestDisabledMCPReturnsNotFoundWithoutAuthentication` | 当前路由/API 错误本地测试通过 |
| T03.22 | `TestPortalCommandUsageErrors`、`TestReadPassword`、`TestRequireLoopbackMetrics`、`TestLoadReadsConfigExpandsEnvironmentAndConvertsDurations` | 命令与配置本地验证；未在 Linux root/systemd 环境运行安装流程 |
| T03.23 | `TestAtomicPostgresOperations`、`TestDetachWorkspaceRequiresCurrentHostAttachment`、`TestWriteErrorStopsDownstreamHandlers`、`TestRejectedGatewayRequestsDoNotReachHostService`、全量 `go test -race ./...`、`make cover` | 本地与 PostgreSQL 验证通过；httpserver/gateway/api 包覆盖率分别为 90.8%/86.7%/81.7%；真实 OSB/AIO/Host 仍待验证 |
| T03.24 | `bash -n deploy/single/verify/03-portal-api.sh`；未运行该脚本的真实主机流程 | 脚本本地语法通过；缺少运行中的 OSB/Host，真实登录、创建、续期、进入、删除与 Workspace 复用待验证 |

更正早期 A03.02 和预览端口记录：上述旧记录描述了过渡分支的 notifications 表与 `SetHostPreviewPublic`；当前基础版最终迁移仅含 users、workspaces、hosts、tokens、audit_logs 五表，没有 notifications、public_ports 或 host_token schema，也没有公有预览写接口。当前迁移/Store 验收以 `TestApplyMigrations`、`TestAtomicPostgresOperations`、`TestStoreRoundTrips` 和本轮 PostgreSQL 全量集成为准。

旧 T03.19 段落对创建超时指标测试的名称 `TestWorkerRecordsCreateTimeoutFromAdvanceCreate` 已过期；当前实际覆盖点是 `server/internal/worker/worker_test.go` 的 `TestWorkerRecoversCreatingHostsAndDeduplicatesNotifications`，它断言 `create_timeout` 指标计数为 1。该测试里的 notifications 指 worker 创建队列通知，不是已移除的 notifications 持久化功能。

## 2026-10-20 宿主 guard 与共享错误终止语义补记

- 宿主网络 guard：profile 配置要求绝对 `network_guard_path`。Portal 启动/readiness 与 OSB Create 前共用 `CheckNetworkPolicy`，只执行 `<path> check`，10 秒超时并丢弃 stdout/stderr；错误、取消、超时均安全失败。guard 失败时 `TestCreateDoesNotCallOpenSandboxWhenNetworkGuardFails` 断言 OSB HTTP 调用数为 0；`TestCreateRequestMatchesGolden` 断言 Create JSON 没有 `networkPolicy`。`TestCheckNetworkPolicyRunsFixedCheckCommand`、`TestCheckNetworkPolicyFailsClosed`、`TestConfigValidateRejectsInvalidFields`、`TestLoadTemplatesValidatesDirectoryAndTemplateFiles` 和 `TestCheckNetworkPoliciesPropagatesGuardFailure` 覆盖脚本、配置与启动边界。宿主专用 bridge/nft 固定策略取代此前任何模板/sidecar 可配置网络策略描述；旧验证文字仅作历史记录。
- 共享 `WriteError` 回归：修复前，middleware 写完错误后 Gin 仍继续执行下游 handler，产生拼接响应；Gateway 的非法 Host 路径会追加第二份 `not_found` JSON，缺少 cookie 的路径会追加 Host 响应，且两者都会调用 `HostService.Get` 一次。加入共享 Abort 后，`TestWriteErrorStopsDownstreamHandlers` 验证下游副作用不执行且只返回一份 JSON，`TestRejectedGatewayRequestsDoNotReachHostService` 验证非法 Host 和缺少 cookie 的请求返回唯一 `not_found` JSON 且 HostService 调用数为 0。两项回归先在旧实现下失败，再在修复后通过。
- 本地验证：`go test -race ./internal/httpserver ./internal/gateway ./internal/api` 通过；包覆盖率依次为 httpserver 90.8%、gateway 86.7%、api 81.7%。这些是本地回归证据，不代表 Linux 上 guard 规则安装或真实 Host 生命周期验收。
- 待验：root 正在 Lima 核验 Linux guard 路径/规则与真实 Host 全链路；真实 OSB/Host E2E、登录到创建/进入/删除及 Workspace 复用目前均保持待验证。

## 2026-10-20 磁盘余量保护与按需 TLS 补记

- Host 创建前调用 Docker Engine `/info`，要求实际 `DockerRootDir` 与配置路径一致，并检查文件系统可用字节；默认低于10GiB即返回 `capacity_exceeded`，数据库写入前拒绝 Host/Workspace。回归覆盖 Docker API、根目录错配、低余量、引擎错误、配置校验和拒绝后无数据库写入。
- Caddy 证书授权仅允许门户域名、运行中 Host 和策略允许的预览端口；不存在/非运行 Host、禁用端口、错误后缀及嵌套域名均拒绝。API listener 只允许绑定127.0.0.1；Caddy 对外门户站点对授权路径固定返回404。
- `make test`、`make race`、`make lint`、`make build`、`make generate`、`go vet ./...`、`go mod verify` 通过。使用本机临时 PostgreSQL（数据仅在 tmpfs，测试后自动删除）运行 `make cover` 与 `make it`；含数据库覆盖率84.7%，所有 `internal/` 包均≥80%。未设置 OpenSandbox 环境，OSB 集成跳过。
- 固定 Caddy 2.11.7 镜像分别验证 HTTP 与多行 `tls { on_demand }` 配置；临时 Caddy 容器实测外部证书授权路径返回404，容器已清理。`sslip.io` 和 `nip.io` DNS 解析到目标机，但 TCP 80/443不可达；内置浏览器访问临时 HTTPS 域名显示连接关闭，目标机没有 Portal/Caddy listener，未声称公网证书或目标机整栈浏览器验收通过。
- 当前单盘余量拦截只是停创阈值，不是 Workspace 硬配额、PostgreSQL 独立保护、磁盘告警或备份。测试机无额外数据盘且项目文件不得上传，远端整栈部署、物理盘故障保护与受信公网证书实测仍未完成。
