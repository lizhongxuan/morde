# HUB-01：Hub 会话和设备授权基础实施 TODO

> 开发量：按一个 AI 会话、前置就绪后的连续开发估算，最多一天；只计实现与交接记录。测试编写、运行和验收单独进行，不计入这一天。
> 范围估算不是完成承诺；发现超出一天时把剩余实现按文件/接口继续合并拆开，保持原验收条件。
> 当前状态：实现与定向验收完成。服务端 Hub/Device 路由和撤权闭环、attach 竞态修复、dsh 独立 Morde Client Installation 登录及 Hub 适配均已在本地完成；本次针对 PostgreSQL 安装/撤权用例使用本机临时容器验证，没有访问远端服务。
> 直接前置：可立即按现有身份与合同开发；共享路由/schema 由 INT-01 承接。
> 修改边界：服务端 Hub/设备专属 handler、业务用例与 dsh 协议适配；dsh 登录卡片由会话 1 在其 `src/client/**` 边界内实现。
> 写入规则：[五会话执行入口](../并行执行入口.md)；只修改本会话的指定文件与本会话文档。公共合同/路由/迁移变更在本文件交接给会话 5。
> 本地约束：只在本地修改和验证，不发送或上传文件，不向远端部署。

## 本次开发

- [x] **HUB-01.01（完成）接入 Hub 连接和设备身份**：绑定 Morde 安装、presence、设备列表及安装撤权；队列和连接保持有界。安装撤权入口提交后关闭 Hub；身份/状态/密码撤权会关闭 owner 会话；attach 后复核安装状态，避免与撤权并发时留下连接。
- [x] **HUB-01.02（完成）实现基础调用合同**：服务端验证来源和目标安装仍有效且属于同一账号，按目标注册的 action 有界投递 info/open/notify，关联请求、取消、超时和未知结果。dsh 通过独立 Morde Client 登录流程拿到安装凭据，使用 Authorization bearer header 和无凭据 hello 帧；旧 nanoMuse Cloud token 与 Hub 登录保持隔离。网页 open 每次由目标端本人允许一次；不继承旧 remote-control 开关或 trusted 设备授权。

## 本会话实现与验证记录

- `server/internal/domain/hub/{protocol.go,protocol_test.go}`：严格解析单帧 JSON，限制 info/open/notify 参数和动作结果；open 只接受 HTTP(S)，info 结果仅允许有界设备元数据，不能借结果回传路径等额外字段。
- `server/internal/app/hub.go`、`hub_test.go`：使用已认证的 Client Installation 身份，检查双方同账户且非自身调用；限制帧率、presence、队列、连接数和待处理调用；关联结果、转发取消、超时/断线/无效结果回报未知，不把未确认操作报告为成功。
- `server/internal/app/hub_devices.go`、`hub_devices_test.go`，`server/internal/api/devices.go`、`devices_test.go`：设备列表复用 ClientLogin 安装记录；支持安装重命名和撤销并关闭对应 Hub 会话。`server/internal/api/hub_session.go`、`hub_session_test.go`：仅接受已认证的 Morde Client principal，限制 Origin/帧长/读写时限并拒绝 query 参数。
- INT-01 已在 `api/api.go`、`cmd/portal/main.go`、`store/postgres/client_login.go` 和 `queries/client_installations.sql`/sqlcgen 中接通 Hub/Device 正式路由、Client 凭证 allowlist、DeviceService 注入，以及按 owner/未撤销/未过期约束的安装名更新；无需新增迁移。
- `project/nanoMuse/harness/dsh-nanomuse/src/cloud.ts`、`src/hub.ts`：新增独立 Morde 安装状态和登录/注销路由；登录前要求 `/api/capabilities` 声明 `ordered_client_login` 与 `hub`，每次 client-login 用新 Idempotency-Key，并在请求前落盘递增的 attempt generation。`MORDE_CLIENT_CREDENTIAL` 单独保存在 Credential Store，旧 Cloud token 不复用；Morde Hub WebSocket 使用 `ws` 设置 Authorization header，首帧只发 `{type:"hello"}`。目标动作仅提供有界 `info`、HTTP(S) `open` 和 in-app `notify`；`open` 每次使用 once-only 目标端确认，并拒绝旧 remote-control/trusted 授权、拒绝“始终允许”结果。
- `project/nanoMuse/harness/dsh-nanomuse/src/client/**`：会话 1 新增独立 Morde Hub 登录卡片，展示安装状态并在登录结束清空密码；`Live.mordeHub` 单独更新状态。Capsule 对 Morde `open` 的一次性确认仅显示“允许一次/暂不”，不展示“始终允许”。
- dsh 验证：`pnpm typecheck`、`pnpm build`、`pnpm test`（400 项）通过；`node --test tests/hub.test.mjs tests/cloud-morde-hub.test.mjs tests/api-methods.test.mjs tests/locales.test.mjs src/client/MordeHubCard.test.mjs src/client/Capsule.test.mjs`（30 项）通过。
- 服务端当前验证：`go test ./internal/domain/hub ./internal/app ./internal/api ./cmd/portal -count=1`、同范围 `go test -race` 和 `go vet` 通过。独立本机 PostgreSQL 容器上定向运行 `TestUserUpdateUsesExpectedStatusAndRevokesCredentials`、ClientLogin 原子凭据/旋转/竞态/迁移测试和 `TestRenameClientInstallationRequiresOwnerAndActiveInstallation`，全部通过；容器已停止并移除。
- 已运行 `cd server && go test ./internal/domain/hub ./internal/app -count=1`、`go test ./internal/api -count=1`、`go test ./cmd/portal -count=1`，均通过；Hub/Device 定向 API 测试通过；`go test -race ./internal/domain/hub ./internal/app ./internal/api ./cmd/portal -count=1`、`go vet` 同范围通过。新 `domain/hub` 覆盖率为 86.0%。
- 之前的全量 `go test ./... -count=1` 曾在未触及 Hub 的 `internal/storage/workspacecurrent` 2 项路径安全断言和 `internal/worker` 10 项目录/socket 权限断言失败；`make lint` 也曾报告个人数据/workspace sync 等其他文件问题。本次未重跑全仓测试与 lint；这些历史问题不阻塞 HUB-01 的定向验收。

## 原任务与已有进度

下列编号用于追溯完整业务规则和拆分前证据；同一原编号涉及多个会话时，各会话只完成自身范围。原项全部实现及验收通过后才可视为完成。

| 原编号 | 原任务 | 拆分前状态 |
|---|---|---|
| [F04-T01.04](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 确定 Hub 与设备合同 | 未开始 |
| [F04-T04.01](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 接入 Hub 会话和 presence | 未开始，依赖 F04-T01.04 |
| [F04-T04.02](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 实现设备与安装关联 | 未开始，依赖 F04-T04.01 |
| [F04-T04.03a](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 实现设备调用的授权和投递基础 | 未开始，依赖 F04-T04.01 |
| [F04-T04.03b](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 接入信息、网页打开和通知合同 | 未开始，依赖 F04-T04.03a |

## 交接

- 在本文件记录完成的接口、文件和待会话 5 处理的公共改动；不要直接改其他会话负责的文件。
- **交给会话 5 / INT-01 的集成项**：
  - **已接通**：`api.go`、`cmd/portal` 已注册 `/api/hub` 与 `/api/devices`，装配 HubService/DeviceService 并设置 Client credential allowlist；`RenameClientInstallation` 使用 Postgres/sqlc 更新既有 `client_installations.display_name`，限定 owner、未撤销、未过期且未命中返回 NotFound。INT-01 记录 API/OpenAPI 与生成验证。
  - **设备 DELETE 语义**：本任务把 Device ID 定义为 Client Installation ID，不创建第二套 Hub 身份；DELETE 撤销整个 Morde Client Installation，复用现有 `ClientLoginService.Revoke`，同时使其 Client/Workspace-sync 凭据失效。设备 UI 可以明确提示这是安装级撤权。
  - **已完成**：`DELETE /api/clients/:id`、`POST /api/auth/client-logout`、客户端 Web session `POST /api/auth/logout`、用户状态/角色变更及密码变更在撤权提交后关闭对应安装或 owner 的 Hub 会话，再执行后续 reconcile。
  - **已完成**：Hub attach 后二次校验安装撤权状态；提供 owner 级会话关闭并覆盖安装/账号撤权竞态。
  - 共享 OpenAPI/Hub frame fixture 已更新；`golang.org/x/net/websocket` 已提升为直接依赖。
  - 用户确认 dsh 复用 Morde Client 登录流程；实现已完成，绝不复用 NanoMuse API key 作为 Morde 身份。Hub WebSocket 需要运行时依赖 `ws`，已在 dsh 包 manifest/lock 中声明。
- HUB-01 定向验证记录在本文件；跨会话的 F04 原编号完整验收仍由[独立验收清单](../验收资料/验收入口.md)跟踪。完成本会话不自动把 PC/iOS 接入、云端 Task 授权或 F04 汇总项标为完成。
- `server/AGENTS.md` 要求接口、实现和测试在同一变更交付；可先完成代码并标“待验证”，合入/提交前再按独立验收清单补齐测试与所需检查。
