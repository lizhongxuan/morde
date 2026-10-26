# 云端主机平台后端编码规范（server/）

适用于 `server/` 下全部 Go 代码、测试、迁移、SQL 查询与 OpenAPI 契约，AI Agent 与人类开发者都必须遵守。实施文档与本文不一致时，按第 1 节"冲突处理"办，不要自行挑一种解释。

## 1. 规则来源与职责

| 文档 | 管什么 |
|---|---|
| 本文 | 技术选型、分层、编码、测试规则（唯一来源） |
| `docs/实施/00-总览与公共约定.md` | 目录归属、配置格式、错误码、表结构、跨文档接口签名、日志与指标约定、组件与依赖版本 |
| `server/openapi/portal.yaml` | Portal HTTP 接口（03 契约定稿后为唯一事实来源） |
| `云端主机平台设计方案.md` | 基础版架构决策。本项目不单设 ADR 目录：架构变更登记在 00 第 16 节并回改设计方案 |

- **契约同步**：改变 HTTP 接口、表结构、状态机或跨文档接口签名的变更，必须在同一变更里同时改权威文档（OpenAPI / 00）、实现代码和测试。
- **冲突处理**：文档与代码不一致时，先在所属实施文档的"问题记录"里写明冲突点和归属，再统一修正；禁止按最方便的理解直接写代码。
- **单一来源**：禁止为绕开既有契约再建第二套状态枚举、错误码、API、Store 或测试捷径；改造时不保留新旧两套逻辑。

## 2. 技术基线

本节是后端技术选型的唯一来源；精确版本以 `server/go.mod` 和 00 第 11 节为准。

| 范围 | 选型 | 约束 |
|---|---|---|
| 语言 | Go，版本与 `toolchain` 写在 `go.mod` | 开发机和 CI 用同一版本 |
| HTTP 框架 | `github.com/gin-gonic/gin` | 只在 HTTP Adapter（`api`、`gateway`、`httpserver`）和 `cmd/` 使用；用 `gin.New()`，不用 `gin.Default()` |
| 反向代理 | 标准库 `net/http/httputil.ReverseProxy` | 只在 `gateway` 使用 |
| MCP | `github.com/modelcontextprotocol/go-sdk` | 只在 `mcp` 使用，产出的 `http.Handler` 经 `gin.WrapH` 挂载 |
| PostgreSQL | `github.com/jackc/pgx/v5`（`pgxpool`） | 唯一数据库连接池 |
| SQL 访问 | `sqlc` + `pgx/v5` | sqlc 生成简单 CRUD/查询；事务、行锁、advisory lock、CAS、状态推进手写 pgx（第 7 节） |
| 迁移 | `github.com/pressly/goose/v3`，版本化 SQL | 放在 `server/migrations/`，由 `portal migrate` 显式执行 |
| 日志 | `go.uber.org/zap` | 强类型字段；Logger 构造后注入，禁用全局 Logger |
| 指标 | `github.com/prometheus/client_golang` | 用 `cmd/portal` 创建的 `prometheus.Registry` 注入，不用默认全局 Registry |
| 链路追踪 | 本期不引入 Trace SDK | 用 `context.Context` 传递取消与截止时间；`X-Request-Id` 贯穿日志 |
| 配置 | `gopkg.in/yaml.v3` | 只在 `config` 使用 |
| API 契约 | OpenAPI 3.1 | HTTP 接口声明的唯一事实来源 |
| 测试 | Go `testing`、临时 PostgreSQL、`-race` | 见第 9 节 |
| 依赖注入 | 手写构造函数注入 | 禁用 Wire、Dig 等框架 |

### 负面约束

未经架构决策变更（00 第 16 节登记并回改设计方案），禁止：

1. 引入 GORM、Ent 等任何 ORM，或用 `database/sql` 开第二条数据库访问路径。
2. 创建 `pgxpool` 之外的第二个 PostgreSQL 连接池。
3. 并行引入另一套 HTTP 框架（chi、echo、用 `http.ServeMux` 做业务路由等）、日志（`log/slog`、logrus、zerolog）、Trace（OpenTelemetry SDK）、迁移或 SQL 生成工具；运行时自动建表或迁移。
4. 引入未在 00 第 11 节登记、未固定精确版本的第三方依赖。
5. 使用可变的包级状态：全局 Logger（`zap.L()`、`zap.ReplaceGlobals`）、全局连接池、默认 Prometheus Registry（`prometheus.MustRegister`）、全局单例。哨兵错误、只读的 `embed.FS`、常量表不在此列。

## 3. 工程边界与分层

```text
server/
├── cmd/
│   └── portal/            # 配置加载、依赖装配、启动、健康检查、优雅关闭；子命令 serve / migrate / user add
├── internal/
│   ├── domain/            # 纯内核：ErrorCode、ID 生成与校验（根包）及各实体子包，只依赖标准库
│   │   ├── template/      # 模板值对象、校验、单位换算
│   │   ├── host/          # Host、State、状态转换、预览端口规则
│   │   ├── workspace/     # Workspace、State、状态转换
│   │   ├── identity/      # User、Role、配额、Token、凭证 Kind 与格式、Principal
│   │   └── audit/         # 审计 Entry、Action、ActorKind
│   ├── app/               # 应用层：用例服务、Command/Query 类型、端口接口
│   ├── api/               # HTTP Adapter：Portal API
│   ├── gateway/           # HTTP Adapter：主机网关
│   ├── mcp/               # Adapter：平台 MCP
│   ├── httpserver/        # HTTP Adapter 共用：中间件链、错误码映射、Server 超时
│   ├── worker/            # 后台任务：创建推进、对账、续期、空闲回收、活跃度落库
│   ├── store/postgres/    # 持久化 Adapter，实现 app 的 Store 端口
│   │   ├── queries/       # sqlc 输入
│   │   └── sqlcgen/       # sqlc 生成代码，禁止手改
│   ├── orchestrator/      # 出站 Adapter：OpenSandbox 客户端与档位路由
│   ├── aio/               # 出站 Adapter：AIO HTTP 客户端
│   ├── storage/           # 出站 Adapter：工作目录卷删除（docker）
│   └── config/            # portal.yaml 与模板文件的加载与校验
├── migrations/            # goose SQL 迁移
├── openapi/portal.yaml    # OpenAPI 3.1
└── sqlc.yaml
```

依赖方向（`make lint` 里的 `depguard` 强制检查）：

```text
cmd/portal ─▶ api / gateway / mcp / worker ─▶ app ─▶ domain
                  │                            ▲
                  └─▶ httpserver             端口
                                               │
              store/postgres、orchestrator、aio、storage（实现 app 端口）
```

- `cmd/`：只做配置加载、依赖装配、启动、健康检查与优雅关闭，不写业务规则。
- `domain`：只依赖标准库，禁止导入 Gin、pgx、zap、Prometheus、yaml 或任何传输 DTO。状态与状态转换规则只写在这里。
- `app`：依赖 `domain` 和自己定义的端口；禁止导入 Gin、pgx、`sqlcgen`、`config`（配置由 `cmd` 转成 app 的 Options 结构体传入）。
- Adapter 之间不互相调用业务逻辑。唯一例外：`mcp` 可以直接用 `aio` 客户端调用主机接口（纯协议转换），但权限、状态判断与审计必须经过 `app`。
- 跨文档使用的类型与接口签名以 00 第 9、10 节为准。

## 4. Go 编码规范

### 4.1 格式与命名

- 必须通过 `gofmt`、`goimports`、`go vet` 与 `golangci-lint`（配置文件 `server/.golangci.yml`）。查看格式问题用只读的 `gofmt -l` / `gofmt -d`，修改用编辑工具；不用 `gofmt -w`、`golangci-lint --fix` 批量改写。
- 包名小写、单数、有明确业务含义；禁止 `util`、`common`、`helper`、`base`、`misc` 这类包名。
- 标识符用 `MixedCaps`；缩写统一大写：`ID`、`HTTP`、`API`、`URL`、`JSON`、`SQL`，以及本项目常用的 `AIO`、`MCP`、`CDP`、`TTL`、`CPU`（如 `HostID`、`AIOKey`、`SandboxID`）。
- 状态、类型类取值用命名类型加常量（如 `host.State`、`identity.Kind`、`domain.ErrorCode`）；零值 `""` 一律视为无效，入参、出参、落库前都要校验，不得把零值当作某个合法状态。
- 不跨包复用的标识符不导出。

### 4.2 数据结构与类型安全

- 跨包使用或字段多于 3 个的结构体字面量必须写字段名。
- 对外 JSON 里的集合字段严格按契约：为空时返回 `[]` 或 `{}`，禁止返回 `null`（如 `items`、`error.details`）。
- 跨层传递用强类型 Command/Query 与 domain 模型。Store 的行模型和 `sqlcgen` 类型不出 `store/postgres`；HTTP 响应用 Adapter 内的 DTO，不直接序列化 domain 结构或数据库行。

## 5. 错误处理与 panic

- 用 `%w` 补充操作上下文，例如 `fmt.Errorf("create sandbox for host %s: %w", hostID, err)`。错误消息小写、末尾不加句号；错误消息里不放凭证、`aio_key`、密码（00 第 5 节）。
- 稳定的业务错误用 `domain.ErrorCode` 表达（00 第 8 节），由 `httpserver` 统一映射为 HTTP 状态和 JSON 错误体；`domain`、`app` 里不出现 HTTP 状态码。端口返回的哨兵错误（如 `app.ErrSandboxUnavailable`）由 app 转成对应的 ErrorCode。
- 判断错误用 `errors.Is` / `errors.As`，不比较错误字符串。
- 错误要么返回给上层，要么在最终边界记录一次日志，禁止逐层重复打印。最终边界是：HTTP 请求（`httpserver.WriteError` 对 5xx 记录）、MCP 工具调用、worker 的每次任务执行。
- panic 在最终边界捕获：HTTP 用 `httpserver` 的 Recovery 中间件，worker 和后台 goroutine 在每次任务执行的外层 `recover`。捕获后用 zap 记录堆栈，HTTP 返回 500 `internal`，响应里不出现 panic 信息或堆栈。客户端中断转发时的 `http.ErrAbortHandler` 不当作错误记录。

## 6. HTTP 与 Gin

### 6.1 中间件顺序

```text
Recovery → RequestID → AccessLog → Metrics → 鉴权 → Handler
```

前四个由 `httpserver` 提供（本期不引入 Trace，不占位）。鉴权由各 Adapter 追加：Portal API 是门户 Cookie / API Key，网关是 Host 解析 + 主机 Cookie/ticket，平台 MCP 在自己的 handler 里校验 API Key。

### 6.2 Handler 职责

- Handler 只做三件事：解析与校验请求、调用 app、把结果或错误写成响应。禁止直接访问 pgx 或 Store、计算状态转换、直接写审计、直接修改主机或工作目录状态。
- `gin.Context` 只存在于 Adapter；传给 app 的是 `c.Request.Context()` 和强类型 Command/Query。
- 日志和 Metrics 的路由标签用 `c.FullPath()`，禁止用含主机 ID、令牌或查询串的原始路径；没有 Gin 路由的请求（网关转发、端口预览）用固定标签（`proxy`、`preview`）。

### 6.3 超时、请求体与关闭

- `http.Server` 必须显式设置 `ReadHeaderTimeout`、`ReadTimeout`、`WriteTimeout`、`IdleTimeout`，取值来自 `portal.yaml` 的 `http` 段（00 第 6 节），统一用 `httpserver.NewServer` 构造。
- 所有请求体都限制最大字节数：Portal API、平台 MCP、网关转发各有上限（00 第 6 节），超限返回 413 `request_too_large`。
- 网关转发的 WebSocket、SSE、大文件上传下载会超过 Server 级超时：只在鉴权通过后的转发请求上，用 `http.ResponseController` 清除该请求的读写截止时间，并用测试证明长连接不被切断；不得为此把整个 Server 的超时设为 0。
- 优雅关闭：收到 SIGTERM 先停止接收新请求，`Shutdown` 等待进行中的请求（`http.shutdown_timeout`），再停 worker、落库活跃度、关闭 `pgxpool`、`logger.Sync()`。被 hijack 的长连接由网关自己跟踪并在关闭时断开。

## 7. 持久化（pgx 与 PostgreSQL）

### 7.1 连接

- `pgxpool.Pool` 是唯一的数据库入口：在 `cmd/portal` 启动时创建，优雅关闭时释放，注入 `store/postgres`。
- `database.dsn` 必填，为空或只有空白时启动失败。不提供内存 Store，测试替身写在 `_test.go` 里。
- `portal serve` 启动时检查库的迁移版本等于 `migrations/` 的最新版本，不一致就退出并提示先执行 `portal migrate up`。

### 7.2 sqlc 与手写 SQL

- `internal/store/postgres/queries/*.sql` 是简单 CRUD/查询的唯一输入，用固定版本的 sqlc 生成 `sqlcgen/`（`make generate`）；生成文件禁止手改，生成物必须与查询文件一致。
- 下列操作必须手写 pgx，并在代码注释里写明事务边界和原因，不得为了使用生成代码拆散事务：
  - 创建主机：配额与容量校验、写 Host、挂载 Workspace 在同一事务内完成，用 advisory lock 串行化；
  - 主机与工作目录的状态推进：`UPDATE ... WHERE id = $1 AND state = $2` 形式的 CAS，影响 0 行视为冲突；
  - ticket 一次性消费（`used_at IS NULL` 条件更新）；
  - Reconciler 选主（`pg_try_advisory_lock`）；
  - 活跃度批量落库。
- 数据库行模型只存在于 `store/postgres` 内，返回前必须显式转换成 domain 模型。
- Store 端口由 app 按业务原子能力定义（如 `HostStore`、`TokenStore`），禁止建一个包含大量无关方法的大 Repository。

### 7.3 Schema 与迁移

- 表结构只能通过 `server/migrations/NNNNN_<名称>.sql`（goose 格式，含 `-- +goose Up` 与 `-- +goose Down`）变更；运行时不自动建表或迁移。
- 已发布的迁移只增不改。首个版本发布后，生产变更按 Expand/Contract 分步：同一次发布里不删除旧列、不加破坏性约束。
- 禁止用触发器、存储过程或数据库 Hook 推进状态、写业务数据；`updated_at` 由写入 SQL 显式设置。

## 8. 日志与指标

### 8.1 日志

- 只用注入的 `*zap.Logger`，字段用强类型（`zap.String("host_id", id)`），不拼接字符串；生产环境输出 JSON 到 stdout。
- 请求日志都带 `request_id`；涉及主机的日志带 `host_id`；后台任务日志带 `job`。访问日志字段见 00 第 13 节。
- 禁止记录：任何凭证的 `secret`、`aio_key`、密码、`Cookie` 与 `Authorization` 头、带 ticket 的查询串（00 第 5 节）。
- 关键功能必须有日志：

  | 场景 | 级别 | 必带字段 |
  |---|---|---|
  | 启动与关闭（监听地址、实例名、迁移版本，不含密钥） | info | `instance` |
  | 主机、工作目录状态变化 | info | `host_id` 或 `workspace_id`、`from`、`to`、`reason` |
  | OpenSandbox / AIO 调用失败（在最终边界记一次） | warn / error | `profile`、`op`、`host_id` |
  | 对账动作（lost、孤儿删除、续期、空闲处理、删卷） | info | `action`、`host_id` |
  | 鉴权失败、限流触发 | warn | `kind`、`client_ip`（不含凭证值） |
  | 审计写入失败 | error | `action` |
  | panic | error | 堆栈 |

### 8.2 指标

- 指标名统一以 `cloudhost_` 开头，注册到 `cmd/portal` 创建的 Registry，经 `listen.metrics` 的 `/metrics` 暴露（只监听 127.0.0.1）。
- 标签值必须有界：禁止用主机 ID、用户 ID、令牌、原始路径作标签。
- 关键功能必须有指标；最少集合与名称、标签、归属见 00 第 13 节，新增指标先登记到那里。

## 9. 测试

- 用 Go `testing`；`go test -race ./...` 必须通过；`internal/` 新增包覆盖率 ≥ 80%（`cmd/`、`sqlcgen/` 除外）。
- PostgreSQL 集成测试在设置 `CH_TEST_DATABASE_URL` 时运行：每个测试包创建临时数据库、执行 goose 迁移、结束后删除；未设置时 `t.Skip` 并写明原因。真实 OpenSandbox 测试在设置 `CH_OSB_URL`、`CH_OSB_API_KEY` 时运行。
- 并发路径必须同时覆盖竞争成功和竞争失败，至少包括：并发创建不超出配额与容量、ticket 并发消费只有一个成功、同一工作目录并发挂载只有一个成功、主机状态 CAS 冲突（如删除与对账同时发生）、两个 Portal 实例只有一个拿到 Reconciler 锁。
- app 层单测用 `_test.go` 里的端口替身；不为测试在生产代码里加分支或开关。
- HTTP Adapter 用 `httptest` 覆盖每个接口的成功与主要错误码，并断言错误体格式、集合字段不为 `null`。

## 10. 完成任务前的检查

在 `server/` 下执行，全部通过才算完成（00 第 12 节）：

```bash
make lint            # golangci-lint：gofmt、goimports、govet、staticcheck、errcheck、errorlint、bodyclose、depguard、forbidigo
make test race cover
make generate        # 执行后 sqlcgen/ 无差异
CH_TEST_DATABASE_URL=postgres://... make it   # 涉及 store/postgres 或迁移的任务
```
