# Morde Harness 可观测系统实施任务 TODO

依据[设计方案](Morde-Harness可观测系统设计方案.md)和用户确认：允许引入 OpenTelemetry Trace SDK，并更新 `server/AGENTS.md`。当前工作区没有旧的 `docs/实施/00-总览与公共约定.md`；依赖版本以 `server/go.mod` 为准。`[x]` 完成，`[~]` 实现已接通、验收有缺口，`[ ]` 未完成。代码先在本机修改与验证；部署仅限用户指定的测试机，使用合成对话验收。

## 单机架构

| 能力 | 方案 |
|---|---|
| Trace SDK | Portal 进程内 OpenTelemetry Go SDK，有界异步 BatchSpanProcessor；W3C `traceparent` 传播。 |
| Span / 日志存储 | 自定义 SpanExporter 与 trace 关联 zap 日志镜像复用 Portal 现有 `pgxpool` 写入观测表；Span、trace link、日志各有界并保留 7 天。 |
| Harness 细节 | 从现有 AEAD 加密 DSH checkpoint 读取完整已持久化事件流；不复制、修改或清理恢复 checkpoint。未 flush 的事件明确标记未持久化/不完整。 |
| 指标和日志 | 不部署 Prometheus、Collector、Phoenix、Loki 或独立时序库；保留现有进程内 `prometheus/client_golang` 与 `/metrics`。服务端 zap 日志带 request/trace/span ID；管理页展示已采集的 HTTP、模型和工具 span 事件。 |
| 故障边界 | Trace 队列有界，导出错误不阻断任务或工具；span 缺失可见。核心 PostgreSQL、Portal 或整机故障仍是单机故障边界。管理后台各区块独立请求和错误态。 |

部署只增加 Portal 依赖与 PostgreSQL 迁移，不增加守护进程或容器。单机目标为 4 vCPU / 8 GiB；结合现有 PostgreSQL OOM 记录，必须限制批次、span 属性大小、查询时间和保留期，并测量 Portal/DB 峰值开销。七天清理只作用于观测 span 表，不能删除 DSH recovery checkpoint。SpanProvider 关闭并 flush 后再关闭 pgxpool。

## 接口合同

- 客户端为每个请求生成 W3C `traceparent`；Portal 校验/新建上下文并返回 `X-Trace-Id`。保留 `X-Request-Id`，不把任何追踪 ID 当授权或幂等键。
- 同步 HTTP span 记录 route、method、status、duration、request ID 及脱敏业务 ID；异步任务使用持久消息/Task 身份与 span link 关联，重试/恢复不伪装成同一父子调用。
- `GET /api/admin/observability/overview`、`/traces`、`/traces/{trace_id}`、`/traces/{trace_id}/events`、`/traces/{trace_id}/logs` 管理员专用；事件按 DSH session/sequence、日志按时间游标分页。每个来源返回 `available|partial|unavailable|disabled`、更新时间和安全原因。
- 保留现有 `/api/admin/users/{id}/tasks/{task_id}/workflow-trace` 语义；它的业务快照 ID 不等于 W3C trace ID。
- Trace 内容不得包含模型思考、prompt、工具参数/结果或凭据。完整内容只从 AEAD checkpoint 经管理员 API 按现有用户/Task/Agent 身份校验读取；接口 `no-store`，查看详情写审计。

## 实施任务

- [x] **O01 更新架构和依赖约定**
  - 完成：根 `AGENTS.md` 与 `server/AGENTS.md` 允许注入式 OpenTelemetry SDK，固定版本引用 `go.mod`；记录 trace/log 关联、隐私白名单和 shutdown 顺序。OTel Go 模块固定为 `v1.47.0`，单机只用本地 PostgreSQL exporter，不依赖全局 Provider。

- [~] **O02（依赖 O01）实现有界 PostgreSQL Span / 日志写入**
  - 修改：`server/internal/tracing/`、`server/migrations/`、`server/internal/store/postgres/`、`server/cmd/portal/main.go`；Span 与带有效 trace ID 的结构化日志复用同一 pgxpool，各自限队列、批次、载荷、查询和 7 天分批清理。
  - 已验收：本地 PostgreSQL 16 临时库执行所有 goose migrations；Span exporter 与关联日志写入、读取、分页和 retention 定向集成测试通过。
  - 已部署：测试机单机 Portal + 现有 PostgreSQL，Span、日志和清理器没有新增进程或容器；首次发布前保存 Portal、后台资源与数据库备份，迁移版本到 262。
  - 已验收：测试机实际 PC 请求成功写入 Span 与关联日志；后台可检索。首次部署后观测到 Span identity/name 校验失败，导致最近批次部分不可用、历史记录仍可查；诊断版已将失败分类并在管理页说明。代码检查发现固定路由标签与 HTTP 方法拼接可能形成 `GET preview` 这类非法 span 名；本地已改为稳定格式 `http.get.preview`，HTTP middleware/tracing 包测试通过。部署当前工作区构建时，Portal 因测试机 schema=262、当前迁移最新版本=264 而拒绝启动；已回滚至诊断版。迁移 263/264 涉及独立 Task 恢复和模型输出上限功能，本次未擅自应用。修复版尚未上线。未做 4 核 / 8 GiB 压力、队列溢出与资源峰值测量。

- [~] **O03（依赖 O01）打通客户端和 Portal trace context**
  - 修改：`server/internal/httpserver`、`server/internal/api`、`server/internal/gateway`、`web/portal/src/api`、桌面客户端和 iOS 请求封装、`server/openapi/portal.yaml`；新增 Trace middleware/span 与响应头，访问日志带 `trace_id` / `span_id`。客户端继续传 `X-Request-Id`。
  - 已通过：PC/iOS/Portal HTTP 与 SSE trace context 定向测试；PC Host Embed、iOS Hub WebSocket 请求头已补齐；iOS 模拟器构建通过。
  - 已验收：实际 `morde-pc` 合成对话的 Request ID、HTTP trace、服务端日志可在管理员页互相定位；iOS 只完成构建与定向传播测试，未在测试机跑对话闭环。

- [~] **O04（依赖 O03）关联异步 Task、消息、DSH 和子 Agent**
  - 修改：会话消息/Task 持久层、`server/internal/worker`、`runtime/morde-cloud-runtime/src/{account-runtime,engine,ipc}.mjs`；持久 trace/span 上下文，按消息、Task、attempt、Agent session 关联；严格同步 IPC 字段白名单与版本。
  - 已验收：测试机合成 Task trace 能从 HTTP 请求跨 trace link 到异步 attempt，并展开 DSH session、model call、tool call；`morde.status=needs_user` 可见。原始请求 trace 与 Task/Request ID 关联。
  - 未验收：并发、followup/steer、子 Agent、重试/恢复与取消的隔离尚未做测试机端到端验证；此样例的工具处于待确认，未绕过授权执行。

- [~] **O05（依赖 O04）实现管理员 DSH checkpoint 历史读取**
  - 修改：`server/internal/app/agent_checkpoint.go`、`server/internal/store/postgres/agent_checkpoint.go` 或专用 admin read adapter、`server/internal/api`、`server/openapi/portal.yaml`；只读扫描已提交连续 checkpoint，校验 Task/owner/session 树、AAD、digest 与范围，再按 seq/time/type 分页解码事件。
  - 已通过：app checkpoint 历史单元测试、Go 包测试；只读 Repeatable Read、身份/AAD/digest 校验、分段与缺口状态已接入。
  - 已通过：本地 PostgreSQL 16 临时库执行 checkpoint 历史迁移与 `TestAdminAgentCheckpointHistory` 集成用例。
  - 已验收：测试机真实 DSH Task 显示主 Agent 已提交事件、序号与 session；跨 trace Task 的 DSH span event sequence 与最后 checkpoint 比对，覆盖不足会标为部分可用。Task-less 直接对话明确解释“没有 DSH checkpoint Task”。
  - 未验收：尚无测试机子 Agent checkpoint 样例；模型未上报的内部思考不会出现在记录中。

- [~] **O06（依赖 O02、O05）完善敏感数据治理与采集健康**
  - 修改：`server/internal/tracing`、日志字段、checkpoint read projection、健康计数；span 仅保留受控维度，访问/查看记录写管理员审计；恢复 checkpoint 原文密文保持不变，诊断展示做边界验证和必要脱敏。
  - 已完成部分：Span 与 zap 日志字段 allowlist、消息凭据 scrub、载荷/队列/导出健康限制、事件投影脱敏和详情查看审计已实现，并有定向测试。
  - 已验收：实际查看仅由管理员 API 返回并经过审计；后台说明模型内部未记录内容不可见。诊断版导出故障保留历史结果并显示来源缺口，且能说明校验失败类别。路由名称修复目前只通过本地定向测试，尚未在线复验。
  - 未验收：真实凭据扫描、队列溢出/DB 故障演练，以及生产负载下资源、保留与脱敏边界仍待单独验证。

- [x] **O07（依赖 O02–O06）实现管理员查询 API**
  - 修改：`server/internal/api`、`server/internal/app`、`server/internal/store/postgres`、OpenAPI；查询概览、trace 列表/详情、span 树、checkpoint event 与关联日志页。不同来源独立超时/状态，所有 API 服务端 `requireAdmin`。
  - 已通过：管理员鉴权、`no-store`、独立数据源状态、受限分页、audit、事件与日志 API；本地 PostgreSQL 16 验证日志迁移 / 查询。
  - 已验收：测试机管理员可检索 trace、Span、DSH 事件和日志；未登录请求访问观测 API 返回 401，管理员导航可见。
  - 未验收：未用普通登录账号验证 403；管理员审计记录抽查和脱敏扫描未在测试机执行。

- [x] **O08（依赖 O07）完成后台可观测页面和局部降级**
  - 修改：`web/portal/src/routes/admin`、`AdminLayout.tsx`、`App.tsx` 与 API schema/types；概览、搜索、trace 详情、Agent 时间线、思考/工具事件分页；每个区块独立 skeleton/error/retry。
  - 已通过：Portal 全量 72 个测试文件 / 988 个用例、TypeScript、ESLint；span 树显示孤立节点，日志区块独立加载、分页、错误和 coverage；工具 Harness 状态优先于 OTel 通用状态。
  - 已验收：测试机管理员页面展开真实合成 Task 的 attempt / Agent / model / tool Span、事件和日志；`needs_user` 显示“需用户确认”，Task-less trace 显示精确缺口原因。

- [~] **O09（依赖 O02、O06、O07）准备单机发布与回滚方案**
  - 修改：`deploy/single/README.md`、Portal migration/rollback checklist；无额外 Compose/systemd observability stack。迁移可滚动发布，DB query/队列/属性/保留均有界。
  - 已完成：单机发布文档仅依赖 Portal + 现有 PostgreSQL；观测迁移 00258–00261、旧二进制回滚保留表、禁止 Goose Down、队列/批次/保留上限。
  - 已验收：测试机 Linux/amd64 发布；Portal 与 admin assets 保留发布前备份，迁移前另存数据库 dump；为此前部署版本将主机迁移至 262。最新工作区二进制因 schema 版本差异未能启动，从 `/opt/morde/backups/observability-route-name-20261127T100004Z/portal` 成功回滚至诊断版；当前 Portal active、health=`ok`、readiness=200。仅现有 Portal 与 PostgreSQL，无新增服务。
  - 未验收：未实际演练数据库回滚；资源峰值、磁盘增长和关停高负载 flush 尚未测量。首次启动因缺少迁移 262 被服务拒绝，按备份保护流程执行兼容迁移后恢复正常。当前工作区最新迁移 263/264 尚未部署，本次不应用其独立业务数据变更；span 命名修复版尚未成功部署。

- [~] **O10（依赖 O03–O09）端到端验收并更新 Harness 记录**
  - 修改：Harness 总流程、实施任务状态和验证记录；使用合成对话。
  - 已验收：实际 `morde-pc` 发送合成直接对话与 Task 请求；管理员页按 trace 查询到 HTTP → Task attempt → DSH session/model/tool spans、已提交事件与关联日志。Task-less 对话和 `needs_user` 均有明确解释。发布到用户指定单机测试主机后通过 health/readiness 检查。
  - 未完成：iOS 测试机对话、子 Agent、已批准工具调用结果/错误、重试/恢复、普通用户 API 权限、凭据脱敏扫描、组件故障演练与单机压力/资源测试。合成 Task 的 PC 审批列表没有对应待确认项，因此未绕过授权；不能据此声称工具调用完整闭环。

**执行顺序：**O01 → O02 / O03 → O04 → O05 → O06 → O07 → O08 / O09 → O10。O04 与 O05 共改 DSH 运行时及 Worker，按文件串行集成。单机 Portal/PostgreSQL 故障时不承诺后台可用。

**本轮验证：** PostgreSQL 定向集成 `TestAdminObservabilityTraceSessionsExplainTasklessMessage`、`TestAdminObservabilityTraceSessionsDetectSpansBeyondCheckpoint` 通过；API `TestMergeExporterHealthExplainsSpanExportFailures`、`TestExporterHealth`、管理员隔离用例及 tracing `TestMakeSpanRecordClassifiesInvalidSpanNames` 通过。Portal 全量测试 72 文件 / 988 用例通过，TypeScript 与 ESLint 通过；Runtime 全量 258 通过 / 1 跳过。最新代码变更后 `go test ./internal/httpserver ./internal/tracing -count=1` 通过。`make pc` 构建并在实际 Morde PC 发起合成对话。测试机当前运行回滚后的诊断版（SHA-256 `bed1254bc8fd70d79f6c7c1d88d9023e704dc094eeef732a30c33656941d522c`），schema=262、health=`ok`、readiness=200、后台 HTML=200、未认证观测 API=401；诊断版重启后 588 条服务日志中未发现新 Span identity/name 分类错误。管理员页面实际看见 HTTP trace、Task/DSH Span、checkpoint 和关联日志。Span 命名修复版 SHA-256 `59c1ff149ad1b84df169618d3441def61fc24425548fb4d0a5296a0a10b92d0f` 启动与 host 当前 schema 不兼容（工作区最新迁移 264，数据库 262），已回滚；只读 `portal migrate status` 正常退出。未应用会改变 Task/model 数据结构及记录的迁移 263/264。完整 `make test` 仍有既存失败 `TestConversationRouteWorkerTriggersSummaryByTokenPressureBeforeTransportWatermark`；未执行 iOS 远程对话及压力/数据库回滚演练。
