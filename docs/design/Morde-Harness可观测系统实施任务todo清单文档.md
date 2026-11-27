# Morde Harness 可观测系统实施任务 TODO

依据[设计方案](Morde-Harness可观测系统设计方案.md)、[服务端公共约定](../实施/00-总览与公共约定.md)和用户确认：允许引入 OpenTelemetry Trace SDK，并更新 `server/AGENTS.md`。当前仅 O01 进行中，其余未开始；每项实现并验证后立即更新状态和证据。

## 单机架构

| 能力 | 方案 |
|---|---|
| Trace SDK | Portal 进程内 OpenTelemetry Go SDK，有界异步 BatchSpanProcessor；W3C `traceparent` 传播。 |
| Span 存储 | 自定义 SpanExporter 复用 Portal 现有 `pgxpool` 写入业务 PostgreSQL 的独立观测表；仅保留 span 元数据 7 天。 |
| Harness 细节 | 从现有 AEAD 加密 DSH checkpoint 读取完整已持久化事件流；不复制、修改或清理恢复 checkpoint。未 flush 的事件明确标记未持久化/不完整。 |
| 指标和日志 | 不部署 Prometheus、Collector、Phoenix、Loki 或独立时序库；保留现有进程内 `prometheus/client_golang` 与 `/metrics`。服务端 zap 日志带 request/trace/span ID；管理页展示已采集的 HTTP、模型和工具 span 事件。 |
| 故障边界 | Trace 队列有界，导出错误不阻断任务或工具；span 缺失可见。核心 PostgreSQL、Portal 或整机故障仍是单机故障边界。管理后台各区块独立请求和错误态。 |

部署只增加 Portal 依赖与 PostgreSQL 迁移，不增加守护进程或容器。单机目标为 4 vCPU / 8 GiB；结合现有 PostgreSQL OOM 记录，必须限制批次、span 属性大小、查询时间和保留期，并测量 Portal/DB 峰值开销。七天清理只作用于观测 span 表，不能删除 DSH recovery checkpoint。SpanProvider 关闭并 flush 后再关闭 pgxpool。

## 接口合同

- 客户端为每个请求生成 W3C `traceparent`；Portal 校验/新建上下文并返回 `X-Trace-Id`。保留 `X-Request-Id`，不把任何追踪 ID 当授权或幂等键。
- 同步 HTTP span 记录 route、method、status、duration、request ID 及脱敏业务 ID；异步任务使用持久消息/Task 身份与 span link 关联，重试/恢复不伪装成同一父子调用。
- `GET /api/admin/observability/overview`、`/traces`、`/traces/{trace_id}`、`/traces/{trace_id}/events` 管理员专用；事件接口按 DSH session/sequence 分页。每个来源返回 `available|partial|unavailable|disabled`、更新时间和安全原因。
- 保留现有 `/api/admin/users/{id}/tasks/{task_id}/workflow-trace` 语义；它的业务快照 ID 不等于 W3C trace ID。
- Trace 内容不得包含模型思考、prompt、工具参数/结果或凭据。完整内容只从 AEAD checkpoint 经管理员 API 按现有用户/Task/Agent 身份校验读取；接口 `no-store`，查看详情写审计。

## 实施任务

- [ ] **O01（进行中）更新架构和依赖约定**
  - 修改：`server/AGENTS.md`、`docs/实施/00-总览与公共约定.md`、`server/go.mod` / `go.sum`；固定 OTel `otel` 与 `sdk` 版本，登记本地 PostgreSQL exporter 方案、许可证、生命周期和禁止全局 Provider。
  - 验收：依赖版本/许可证与仓库 Go 工具链匹配；规范说明 traceparent、span/日志脱敏、单机故障和 DSH checkpoint 边界。

- [ ] **O02（未开始，依赖 O01）实现有界 PostgreSQL SpanExporter**
  - 修改：`server/internal/tracing/`、`server/migrations/`、`server/internal/store/postgres/`、`server/cmd/portal/main.go`；Span schema 按 trace/span/parent、名称、时间、状态、受控属性与资源字段存储；同批事务写入并去重；7 天分批清理；单条与查询大小设上限。
  - 验收：导出/查询/重复、过大属性、DB 错误、队列满、进程关停 flush、7 天清理；失败仅产生可见缺口，不影响业务事务，不新增 pgxpool。

- [ ] **O03（未开始，依赖 O01）打通客户端和 Portal trace context**
  - 修改：`server/internal/httpserver`、`server/internal/api`、`server/internal/gateway`、`web/portal/src/api`、桌面客户端和 iOS 请求封装、`server/openapi/portal.yaml`；新增 Trace middleware/span 与响应头，访问日志带 `trace_id` / `span_id`。客户端继续传 `X-Request-Id`。
  - 验收：有效/缺失/损坏 traceparent，成功与失败响应，PC/iOS/Web 管理 API、gateway；跨 hop trace 连贯、request ID 单独保留；ID 不影响权限、幂等和 CORS。

- [ ] **O04（未开始，依赖 O03）关联异步 Task、消息、DSH 和子 Agent**
  - 修改：会话消息/Task 持久层、`server/internal/worker`、`runtime/morde-cloud-runtime/src/{account-runtime,engine,ipc}.mjs`；持久 trace/span 上下文，按消息、Task、attempt、Agent session 关联；严格同步 IPC 字段白名单与版本。
  - 验收：并发 Task、followup、steer、多个子 Agent、重试、checkpoint 恢复、停止指定任务不串链；缺 trace 仅标缺口，不改变授权/执行/幂等语义。

- [ ] **O05（未开始，依赖 O04）实现管理员 DSH checkpoint 历史读取**
  - 修改：`server/internal/app/agent_checkpoint.go`、`server/internal/store/postgres/agent_checkpoint.go` 或专用 admin read adapter、`server/internal/api`、`server/openapi/portal.yaml`；只读扫描已提交连续 checkpoint，校验 Task/owner/session 树、AAD、digest 与范围，再按 seq/time/type 分页解码事件。
  - 验收：看到实际返回的 reasoning/摘要、assistant message、tool proposal/call/result、失败尝试和子 Agent；无思考返回或 checkpoint 缺口时显示真实原因；只展示服务端确认已持久化的内容，不泄出用户 API。

- [ ] **O06（未开始，依赖 O02、O05）完善敏感数据治理与采集健康**
  - 修改：`server/internal/tracing`、日志字段、checkpoint read projection、健康计数；span 仅保留受控维度，访问/查看记录写管理员审计；恢复 checkpoint 原文密文保持不变，诊断展示做边界验证和必要脱敏。
  - 验收：凭据扫描不出现在 span/API/日志；checkpoint AAD/digest 不变；溢出、导出失败、不可读、截断和未 flush 分别显示 partial/原因。

- [ ] **O07（未开始，依赖 O02–O06）实现管理员查询 API**
  - 修改：`server/internal/api`、`server/internal/app`、`server/internal/store/postgres`、OpenAPI；查询概览、trace 列表/详情、span 树和 checkpoint event 页。不同区块独立超时/状态，所有管理 API 服务端 `requireAdmin`。
  - 验收：管理员成功；非管理员 403；坏 ID/无数据/过期/DB 查询超时/部分 span/缺 checkpoint 都有明确 status 与 reason；分页稳定、`no-store`、查询受限。

- [ ] **O08（未开始，依赖 O07）完成后台可观测页面和局部降级**
  - 修改：`web/portal/src/routes/admin`、`AdminLayout.tsx`、`App.tsx` 与 API schema/types；概览、搜索、trace 详情、Agent 时间线、思考/工具事件分页；每个区块独立 skeleton/error/retry。
  - 验收：内置浏览器使用管理后台完成检索和详情展开；一个接口超时/空数据/权限失效时其他区块保持布局可用，3 秒内说明原因。按统一 UI 规范；规范文件缺失时注明视觉验收限制。

- [ ] **O09（未开始，依赖 O02、O06、O07）验证单机发布与资源保护**
  - 修改：`deploy/single/README.md`、Portal migration/rollback checklist；无额外 Compose/systemd observability stack。迁移可滚动发布，DB query/队列/属性/保留均有界。
  - 验收：本地/隔离环境验证迁移前后兼容、Portal 和 PostgreSQL 资源峰值、关停 flush、磁盘增长与保留清理、备份恢复。不得对业务数据库删除 checkpoint 或导致已有 Task/工具重放。

- [ ] **O10（未开始，依赖 O03–O09）端到端验收并更新 Harness 记录**
  - 修改：Harness 总流程、实施任务状态和验证记录；使用合成对话。
  - 验收：实际 `morde-pc` 和 iOS 生成同一请求链 trace 证据，管理员能查 trace、服务日志、主/子 Agent reasoning、tool 输入/结果、失败/重试；逐项验证组件故障、非管理员、脱敏与恢复。任何未实测项明确标未完成。

**执行顺序：**O01 → O02 / O03 → O04 → O05 → O06 → O07 → O08 / O09 → O10。O04 与 O05 共改 DSH 运行时及 Worker，按文件串行集成。单机 Portal/PostgreSQL 故障时不承诺后台可用。
