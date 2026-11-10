# INT-02：管理端总览、用户、模型与用量实施 TODO

> 开发量：按一个 AI 会话、前置就绪后的连续开发估算，最多一天；只计实现与交接记录。测试编写、运行和验收单独进行，不计入这一天。
> 范围估算不是完成承诺；发现超出一天时把剩余实现按文件/接口继续合并拆开，保持原验收条件。
> 当前状态：INT-02.01/02 已完成本地实现与定向验收。管理查询、模型连接操作、用量统计及 Portal 页面均已接通；本地 PostgreSQL 定向测试、Portal 测试/构建和 `make lint` 通过。全量套件存在 workspace 路径/权限失败及一项既有迁移测试失败，详见本地验证记录。未连接测试服务器、未使用用户提供的 API key，未外发或上传文件。
> 直接前置：可先复用已有事实源；涉及公共 API/schema 的部分仍由本会话统一处理。
> 修改边界：Morde 管理专属 API/app/store 及 Portal admin 对应页面。
> 写入规则：[五会话执行入口](../并行执行入口.md)；只修改本会话的指定文件与本会话文档。公共合同/路由/迁移变更在本文件交接给会话 5。
> 本地约束：只在本地修改和验证，不发送或上传文件，不向远端部署。

## 本次开发

- [x] **INT-02.01（完成）接入管理查询和操作**：总览/在线/健康、用户账本、模型连接与预算、用量/平台统计已接真实数据和权限；敏感密钥不回显，模型操作写入审计。
- [x] **INT-02.02（完成）迁入对应后台页面**：已接总览、用户列表与详情、平台模型、用量与统计页面，覆盖筛选、分页、空态和受权操作。

### 本地实现与验证记录（2026-11-02）

- 后端在 `server/internal/api/admin_console.go`、`server/internal/app/admin_console.go`、`server/internal/store/postgres/admin_console.go` 增加 admin-only 查询；复用既有安装、授权、`model_call_ledger`、账户额度和 Hub 在线数据。模型凭证不出现在查询响应；新增/更新/检查平台模型连接写入不含密钥的审计事件。OpenAPI 与 Portal schema 已同步。
- Portal 已接入管理总览、用户列表/详情、平台模型、用量与统计页面；用户详情呈现账号、安装、授权、账本、预算和审计摘要。开发模式 MSW 覆盖新增接口，供本地浏览器验收。
- 后端聚焦 API/app/PostgreSQL 测试和 `TestAdminConsoleQueriesAggregateAndPaginateSafely` 本地 PostgreSQL 集成通过；Portal `npm test`（30 个文件、222 项及 2 个脚本测试）、`npm run lint`、`npm run typecheck`、`npm run build`、`npm run generate:api` 均通过；`make generate`、`make lint` 通过。
- 本地 MSW 浏览器验收通过：总览、用户列表/详情、用量区间切换，以及平台模型新增、连接检查、留空密钥编辑后仍保持“已配置”；只输入本地模拟密钥，无外部 API 请求。
- 全量 `make test`、`make race`、`make cover` 被 `internal/storage/workspacecurrent`、`internal/worker` 的 workspace 路径/权限及 symlink 用例阻断。`CH_TEST_DATABASE_URL` 下 `make it` 的管理 API/app 与 INT-02 PostgreSQL 定向用例通过；全量套件还报告 `TestClientTaskSubmissionIsVisibleToDesktopAndIOSInstallations` 缺少 `conversation_message_attachments` 表，以及 workspace sync 用例失败，未在 INT-02 中追查。
- PostgreSQL 定向测试使用临时本地数据库；测试容器已清理。未连接用户提供的测试服务器，未使用用户提供的 GPT API key，未外发或上传文件。

## 原任务与已有进度

下列编号用于追溯完整业务规则和拆分前证据；同一原编号涉及多个会话时，各会话只完成自身范围。原项全部实现及验收通过后才可视为完成。

| 原编号 | 原任务 | 拆分前状态 |
|---|---|---|
| [F04-T08.01](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 提供总览、在线与健康管理 | 未开始 |
| [F04-T08.02](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 提供用户与账本管理 | 未开始 |
| [F04-T08.03](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 提供模型、连接与预算管理 | 未开始 |
| [F04-T08.04](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 提供用量与平台统计 | 未开始 |
| [F04-T09.01](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 迁移总览、在线与健康页面 | 未开始，依赖 F04-T08.01 |
| [F04-T09.02](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 迁移用户详情及账本页面 | 未开始，依赖 F04-T08.02 |
| [F04-T09.03](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 迁移模型和预算页面 | 未开始，依赖 F04-T08.03 |
| [F04-T09.04](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 迁移用量和平台统计页面 | 未开始，依赖 F04-T08.04 |

## 交接

- 在本文件记录完成的接口、文件和待会话 5 处理的公共改动；不要直接改其他会话负责的文件。
- 对应验证在 [独立验收清单](../验收资料/验收入口.md) 执行并记录，不计入本开发文档的一天预算。
- `server/AGENTS.md` 要求接口、实现和测试在同一变更交付；可先完成代码并标“待验证”，合入/提交前再按独立验收清单补齐测试与所需检查。
