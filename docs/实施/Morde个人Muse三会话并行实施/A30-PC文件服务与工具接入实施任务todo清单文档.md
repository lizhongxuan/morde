# A30 PC 文件服务与工具接入实施任务 TODO

**会话：A｜阶段：P3 / G3｜状态：已完成（2026-11-11；A30-T1/T2/T3、B30-T4 v2 联验及 A30-T4 G3 真服务联验均通过；K4 v3 撤权/commit 边界已验收）。**

来源：[需求与架构](../../Morde个人Muse需求与架构设计方案.md)；共同规则：[执行总览](00-三会话执行总览.md)；接口：[共享合同](01-共享接口与联调合同.md)。

## 开始条件与修改边界

G2；与 B30、C30 并发。

server/internal/domain/hub/、app/hub_task_actions.go、相关 API/存储/工具适配/文件生命周期和 worker；新增 task_file_fetch 相关文件（拟新增）仍归 A。不得修改 PC helper 或客户端。

## 接口变化

按冻结 K4 实现 file.fetch 控制动作、完整文件传输、Task 文件投影和清理/删除合同。最终采用的路由与 Go 实现、OpenAPI、测试同步落地；不将普通附件权限直接借给设备传输。

## 任务

- [x] A30-T1（已完成）建立任务绑定的取回记录与授权控制
  - 修改：先在 K4 发布 `muse-k4-v1` 首批格式/解析器、大小/配额、临时副本期限与清理参数，供 B30-T2/C30-T2 使用。复用安装/目录授权版本、Hub 和 Task 操作身份，增加持久 fetch 记录及受控工具；按当前 Task 修订生成只给目标 PC 的短期传输凭证。
  - 验收：参数有唯一版本，仍沿用 G0 字段合同；未授权/旧修订/错误设备请求被拒绝，同操作重试返回同一 fetch。设备离线记录真实等待，不把 Token 暴露给模型或 iOS。

### A30-T1 完成证据

- 冻结共享合同 `muse-k4-v1`：16 MiB 单文件、512 KiB 分块（最多 32 块）、账号 staging 64 MiB / retained 300 MiB、每 Task 最多 16 条 fetch、32 字节随机票据（仅存 SHA-256，5 分钟有效）、staging 1 小时 / 副本 7 天、每 15 分钟最多清理 500 条；文本/Markdown/CSV/JSON 按不可信 UTF-8，DOCX/XLSX/PPTX 走安全 OOXML 解析，PDF 为 raw-only。
- 新增迁移 `00178_task_file_fetches.sql`、持久取回记录和 Portal Session 状态查询 `GET /api/tasks/{id}/file-fetches/{fetch_id}`。Hub 仅向目标 PC 发送短期票据；服务端绑定 Task revision、目标授权版本、fetch/operation ID，验收 PC 完成回执时再对账数据库状态。
- 定向 app/domain/API 测试及 Postgres 集成测试通过；`make generate`、全量普通 Go 测试通过。全量 race 与 lint 检查发现的失败位于本任务边界外的并发改动；全量 `make it` 按并发协调停止，未作为通过项记录。定向 race 测试通过。
- A30-T1 不包含 manifest/chunk/commit 数据面、逐块撤权校验、解析、保留清理 worker；这些留给 A30-T2/T3。

- [x] A30-T2（已完成）实现完整传输、校验与幂等提交
  - 前置：A30-T1。
  - 修改：按 K4 实现 manifest、chunk、commit、cancel，复用现有分块/限额/摘要原语并补用途校验。每块及提交都检查撤权/取消，只有完整版本可形成文件引用。
  - 验收：合成二进制文件超过 32 KiB 可完整取得；分块重放不重复占用，版本或摘要不符拒绝，断点续传不混版本。撤权与 commit 竞态不能交付失效的新副本。

- [x] A30-T3（已完成）接通 Task 文件、解析和副本生命周期
  - 前置：A30-T2。
  - 修改：服务端文件工具、附件/产物引用、K2 file_fetches/文件查询，以及既有保留/删除入口；补等待/失败/解析结果，供两端对账。默认只读，不新增本机命令或写回。
  - 验收：使用替身 PC 验证文本、PDF 和选定办公格式；传输成功/解析失败可区分，临时副本清理和用户删除可见，重试不重复产物。发布 B30/C30 真联调所需接口。

### A30-T2 / A30-T3 完成证据

- 新增迁移 `00179_task_file_fetch_data_plane.sql` 与事务化 PostgreSQL 数据面：manifest 固定源版本并预留 staging 额度；chunk 按序号摘要幂等写入并支持断点续传；commit 在 Task、修订、安装和目录授权仍有效时原子核验整文件并发布；撤权/取消/超时会撤票并清 staging。副本有 7 天保留、owner 显式删除 tombstone、过期状态和 15 分钟/500 条清理 worker。
- `POST manifest`、`PUT chunks/{sequence}`、`POST commit` 使用 PC ticket；Task owner Portal Session 提供列表、单条状态、取消、下载与删除接口。状态投影含 `content_state`、`deleted_at`、`parse_status`、`parse_error`，不含票据及 ticket hash。OpenAPI 与共享 K4 合同同步。
- 已接通 Task `file.fetch` 的安全内容交付：UTF-8 文本/CSV/Markdown/JSON 校验；复用有界 OOXML 提取器处理 DOCX/XLSX/PPTX，并允许 PPTX 有界图片；PDF 只报 `raw_only`、原始字节不进模型。无法解析时保留 `status=completed`，由 parse 字段区分失败/不可用；可交付内容以不可信数据提示传给 DSH，Task 事件只记安全状态。
- 提供只编入测试的 Go HTTP fixture：`CH_K4_FIXTURE=1 go test ./internal/store/postgres -run '^TestK4NodeHTTPFixture$' -count=1 -v`；连接本机 `CH_TEST_DATABASE_URL`，真实迁移/store/API router 和合成 Task/fetch/ticket，只绑定 `127.0.0.1`。启动打印 `K4_FIXTURE_READY` JSON；POST `/__test/k4-fixture/shutdown` 后测试清理临时数据库。生产 API 不注册 shutdown 路由。
- 定向 `go test ./internal/app ./internal/api`、临时本机 PostgreSQL `TestPostgresTaskFileFetchTransferResumeContentLifecycleAndCancel`、fixture 的 manifest/chunk/commit HTTP 实测通过；B30 报告真实 helper 到临时 PostgreSQL/API 的两轮 live 测试通过，涵盖源变化 fail-closed、按 confirmed sequences 续传、16 MiB+1 拒绝、owner 状态/下载/删除及 file.get 文本读取。
- 全量复核（2026-11-11）：修复并发集成后，`make generate`、`make lint`（0 issues）、`make test race cover` 均通过；覆盖率合计 45.1%。C30 的本地模型诊断现默认跳过，仅设置 `CH_C30_MODEL_DIAGNOSTIC=1` 才会访问真实模型，避免普通测试意外发起外部请求。随后在本机临时 PostgreSQL 上运行全量 `make it`，但 `internal/store/postgres` 的 `TestPostgresScheduleOverlapCreatesSkipWithoutSecondTask` 在隔离数据库执行全量迁移期间触发 10 分钟测试超时（退出码 2）；worker 包报告 cached，PostgreSQL 包未完成。A30 自身的 PostgreSQL 撤权/commit、传输生命周期和真实 PC bridge 定向集成均已单独通过；全量迁移超时不作为 A30 验收通过项。
- K4 v2 变更与验收（2026-11-11）：为兑现 C30 页面显示授权根名称的合同要求，manifest 增加仅供显示的安全 basename `source_directory_name`，owner 投影离线/历史记录允许 null；该值不参与鉴权且不包含根路径。服务端校验和持久化由迁移 `00181_task_file_fetch_directory_name.sql` 实施，manifest 幂等重放固定目录名。B30-T4 v2 联验已通过：真实 `morde-sync` helper + Go API/PostgreSQL loopback fixture 1/1，涵盖完整字节/hash、源变化、断线续传、16 MiB+1 拒绝、撤权、目录名投影及 ticket 不泄漏；B30 build、typecheck、442 项测试通过。该链路尚未包含真实 iOS Task 发起，不代表 G3 完成。
- K4 v3 撤权/commit 修复与验收（2026-11-11）：独立审计发现本机目录授权版本变化会中断 Hub call，但不能可靠立即清掉云端 staging。新增仅接受该 fetch 原 ticket 的 `POST /api/tasks/{id}/file-fetches/{fetch_id}/abort`；未完成传输原子转 `failed/access_denied`、撤销 ticket、清空分块和进度；abort 与 commit 通过 fetch 行锁串行化，commit 已先提交时只撤票并保留完成副本。Harness 在本机授权版本/目录变化后以独立 5 秒请求发送 abort；普通断线不 abort，继续复用原 fetch 续传。同步更新 K4 字段版本、共享合同、`docs/实施/00-总览与公共约定.md` 和 OpenAPI。验收：API strict-shape 测试通过；PostgreSQL transfer lifecycle 与 abort/commit 并发测试通过，后者 `-race` 通过；真实 PC bridge + Go/PostgreSQL/Hub 集成用例通过，并断言撤权后 fetch 为 `failed/access_denied`、确认字节为 0、staging 块为 0、ticket hash 已清除、owner 内容不可读；Harness build 与 19 项定向测试通过；`make generate` 通过且 sqlcgen 无差异。

- [x] A30-T4（已完成）集成真实本机桥接并通过 G3
  - 前置：A30-T3、B30-T4 v2 联验、C30-T3 真服务联验均已完成。
  - 修改：合入 B30/C30 后在 A 独立环境跑真实本机 helper 到本地服务；使用合成目录，所有文件只在本机隔离端点流动。跨模块缺口交对应会话修复。
  - 验收：覆盖 A8–A10：iOS 发起完整文件任务、越界拒绝、离线等待、源变化、断线恢复和撤权；UI、实际字节与服务端状态一致后记录 G3。
  - 联调修复（2026-11-11）：全链路发现新建 Task 的合法初始修订号 `0` 被 `file.fetch` 的应用校验、Hub 参数/回执校验、投影和数据库约束拒绝；已统一允许非负修订号并新增迁移 `00184_task_file_fetch_initial_revision.sql`。PC helper 保留 revision `0`，负值仍拒绝；morde-sync 将源版本变化映射为冲突，Hub 再映射为安全的 `source_changed`。越界路径作为安全工具失败返回，不触发运行时恢复错误。
  - 本机 PC 全链路回归（2026-11-11）：`TestA30TaskFileFetchRunsFromChatThroughPostgresHubAndPCBridge` 通过，覆盖主会话 PDF、断连等待/重连、已写入分块后源变化并清空 staging、超限/越界拒绝及取回中撤权；morde-sync 定向测试和 desktop Harness build/typecheck、`file.fetch` 定向测试 3/3 通过。
  - G3 真服务 UI 联验（2026-11-11）：在隔离本地 Portal v2（当前源码、schema 184、独立数据库和临时工作目录，`127.0.0.1:25051`）及真实 iOS Simulator 中运行 `IOS03SimulatorAcceptanceUITests/testMordeG3RealPortalFetchesSyntheticPCFile`，1/1 通过。iOS 登录后从合成目录发起任务，服务端完成取回；UI 显示目录名、来源路径、27 字节与 SHA-256，实际字节摘要一致；client Web Session 读取 owner fetch 列表得到 404，普通 Portal owner session 可读取。隔离数据库核对 Task `qqxstaeh` 已完成、fetch 内容解析为 `extracted`。本地结果包：`.local/a30-g3-v2/g3-ios-2026-11-11T05-39-00.240Z.xcresult`；运行脚本 `.local/a30-g3-v2/run-ios-g3.mjs`。
  - G3 失败分支由真实 Go/PostgreSQL/Hub/PC helper 集成测试 `TestA30TaskFileFetchRunsFromChatThroughPostgresHubAndPCBridge` 覆盖：离线等待/恢复、分块后源变化并清空 staging、超限/越界拒绝、取回中撤权后通过 abort 清 ticket 与 staging；这些失败分支不是逐项通过 iOS UI 操作触发。专用 Portal v2 与 helper 使用合成账号和合成目录；旧 fixture `127.0.0.1:24951` 及其服务未触碰。

## 交付

G3 是完整文件链路关口；仅有 HTTP 接口或 mock 通过不能完成本文件。 只汇报本地位置、版本、验证及未完成项；不得外发文件。已有实现通过本任务验证可直接复用，不为勾选任务而重写。

