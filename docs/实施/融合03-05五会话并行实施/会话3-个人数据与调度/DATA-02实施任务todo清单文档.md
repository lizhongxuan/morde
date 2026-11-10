# DATA-02：个人资料与内容类型实施 TODO

> 开发量：按一个 AI 会话、前置就绪后的连续开发估算，最多一天；只计实现与交接记录。测试编写、运行和验收单独进行，不计入这一天。
> 范围估算不是完成承诺；发现超出一天时把剩余实现按文件/接口继续合并拆开，保持原验收条件。
> 当前状态：DATA-02.01/.02/.03 的服务端实现及定向验收完成。OpenAPI 已定义个人对象枚举、逐类型正文 schema、个人同步 capability 和同步水位语义。附件正文和引用由 DATA-04 单独实现。
> 直接前置：依赖 DATA-01 的对象及变更接口；共享 API/schema 向会话 5 提交具体变更。
> 修改边界：个人资料、记忆/SOUL/日记、Feed/Ideas/Library、技能和连接器非秘密配置。
> 写入规则：[五会话执行入口](../并行执行入口.md)；只修改本会话的指定文件与本会话文档。公共合同/路由/迁移变更在本文件交接给会话 5。
> 本地约束：只在本地修改和验证，不发送或上传文件，不向远端部署。

## 本次开发

- [x] **DATA-02.01 接入资料和记忆**：复用 profile 对象事务；开放 memory、SOUL、diary 白名单对象，按账号 owner、版本/CAS 和冲突回执处理。domain/app/store 与定向 PostgreSQL race 验收通过。
- [x] **DATA-02.02 接入动态、点子和资源库**：Feed 帖子/偏好、Ideas 用户标记和 Library 元数据共用同一 mutation/change stream；不接受文件路径、会话 ID 或未归属的附件引用，文件正文/引用留给 DATA-04。定向 PostgreSQL race 验收覆盖全部十种对象。
- [x] **DATA-02.03 限制非秘密配置范围**：只开放内置 skill 启用位和 catalogue connector 的 auth/enabled 偏好；技能包、自定义端点、device、grant、OAuth/API 凭据、绝对路径均拒绝。domain 白名单测试通过。

## 原任务与已有进度

下列编号用于追溯完整业务规则和拆分前证据；同一原编号涉及多个会话时，各会话只完成自身范围。原项全部实现及验收通过后才可视为完成。

| 原编号 | 原任务 | 拆分前状态 |
|---|---|---|
| [F04-T02.01](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 实现个人资料接口 | 未开始，依赖 F04-T01.02、F04-T02.02 |
| [F04-T02.03](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 接入记忆、SOUL 和日记对象 | 未开始，依赖 F04-T02.02 |
| [F04-T02.04](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 接入 Feed、Ideas 与 Library 数据 | 未开始，依赖 F04-T02.02 |
| [F04-T02.05](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 接入技能和连接器非秘密配置 | 未开始，依赖 F04-T02.02 |

## 交接

- 在本文件记录完成的接口、文件和仍待会话 5 处理的公共改动；不要直接改其他会话负责的文件。
- **DATA-02 类型合同（服务端实现）**：现有 `GET/PUT/DELETE /api/personal-data/{type}/{id}` 与 `/api/sync/snapshot`、`/api/sync/changes` 路由不变；PUT 仍需 `base_revision` 和 `Idempotency-Key`，DELETE 仍写版本墓碑。owner 只来自已重新授权的 Session/Client Principal。所有允许对象都进入 DATA-01 的 `personal_objects`、`account_object_versions`、`account_change_log` 和幂等回执，不新增表/迁移。
- **DATA-02 类型/正文 allowlist**：`profile/default` 沿用既有 `PersonalProfile`。`memory` ID 仅 `global`、`user` 或 `m_<8 位小写十六进制>`，正文 `{content: 1–65536 字符, category?: ≤64}`；`soul` 使用安全 opaque ID，正文 `{name: 1–80, emoji?: ≤16, style?: ≤64, lang?: ≤16 个 ASCII 字母/数字/连字符/下划线, content: 1–65536}`；`diary` 使用安全 opaque ID，正文 `{date: YYYY-MM-DD, content: 1–65536}`。`feed_post` 使用安全 opaque ID，正文 `{title: 1–200, type: 1–32, emoji?: ≤16, created_at?: RFC3339, liked?: bool, body: 1–65536}`；`feed_preferences/default` 正文 `{content: 1–8192}`；`idea_mark/<idea-id>` 正文 `{state: saved|dismissed}`，不包含静态 Ideas 内容或会话 ID；`library_item/<opaque-id>` 仅 `{name: 单个文件名, kind: artifact|media, mime?: ≤128, size_bytes?: 0..MaxInt64}` 元数据；`skill_setting/builtin_<slug>` 仅 `{enabled: bool}`；`connector_setting/catalogue_<slug>` 仅 `{auth: open|oauth|key, enabled: bool}`。非 profile 正文共享 1 MiB 总上限，所有对象拒绝未知字段和 `null` 字段；credential、path、session/device ID、grant、URL、自定义技能正文、附件 ID 均不在 allowlist。
- **本地数据与配置语义**：自然语言正文只接受用户显式写入的 mutation；不扫描或自动读取本机文件。skill/connector 的 `enabled` 只同步用户偏好，不代表技能包或连接器的实际授权、凭据或本机安装状态；连接器授权仍需在目标设备单独完成。
- **DATA-02 快照/增量范围**：snapshot cursor 改为签名版本 v2，绑定 `personal/all` 范围及跨对象 `(object_type, object_id)` keyset；snapshot 与 changes 只读取上述十种个人对象，不读取/投递 `conversation`、`message` 变更正文或标识。change-feed 仍用账号全局 sequence，越过非个人事件时保持固定水位并推进 cursor。
- **公共合同交接状态**：`server/openapi/portal.yaml` 已包含个人对象枚举和逐类型正文 schema；`personal_sync` 已描述 supported personal objects 与 schedules。snapshot/changes 已说明当前视图、首个水位、分页 revision 合并、固定 through_sequence、过滤后序号间隙和终页推进规则。此合同没有新增路由或迁移。
- **附件边界**：DATA-02 的 Library 对象只同步元数据。DATA-04 已建立 owner-bound 附件上传、读取和已提交引用；只有服务端确认归属的已提交附件可经独立附件合同读取，不把本机路径或未提交附件 ID 当作可同步文件。
- **早期 2026-11-02 本地验收记录**：`go test -race -count=1 ./internal/domain/personal ./internal/app ./internal/api`、`go test -cover -count=1 ./internal/domain/personal`（96.8%）、`go build ./cmd/portal ./internal/store/postgres`、`go vet ./internal/domain/personal ./internal/app ./internal/api ./internal/store/postgres`、相关 `gofmt -d`/`goimports -l` 通过。临时 PostgreSQL 16 上对全部 `TestPostgres(Personal|GetPersonal|AccountChange)` 用例做 race 验收通过，覆盖全类型 CRUD/CAS/幂等、跨账号拒绝、snapshot keyset、changes 越过 conversation 事件、版本墓碑及 owner sequence；定向数据库验收的临时 overlay 和 PostgreSQL 容器均已清理，未改其他测试源文件。该次全仓 `make race`/`make cover` 在 `storage/workspacecurrent` 和 `worker` 用例失败，lint 当时有 4 项诊断；当前复核见 DATA-01 至 DATA-07 验收记录。
- 未运行 `make generate`：本任务不改 SQL/query/migration；没有新增依赖。
- 对应验证在 [独立验收清单](../验收资料/验收入口.md) 执行并记录，不计入本开发文档的一天预算。
- `server/AGENTS.md` 要求接口、实现和测试在同一变更交付；可先完成代码并标“待验证”，合入/提交前再按独立验收清单补齐测试与所需检查。
