# Morde Web 端账号注册审核与权限实施任务 TODO 清单

来源：[Morde Web 端账号注册、审核与权限需求文档](../Morde-Web端账号注册审核与权限需求文档.md)。本清单只规划实施，不表示功能已完成。状态使用“未开始、进行中、待验证、完成、阻塞”；完成代码和本项验收后再勾选。

## 范围与现状

修改现有 Morde Server、用户 Web、管理后台 Web 和单机部署配置。现有 `/login` 已有注册入口，`POST /api/auth/register` 已创建可立即登录的普通用户；用户表只有 `role=user|admin` 和 `disabled`，启动时从文件同步唯一管理员。上述行为须改成一套“审核状态 + 角色 + 固定超级管理员”规则，不保留第二套管理员判定或启动时覆写密码的逻辑。PC、手机客户端继续基于 nanoMuse 二开，本清单不另建客户端。

现有数据库若有账号，版本化迁移将非超级管理员中原本禁用的账号保持禁用，其余账号置为待审核并撤销旧会话；这些账号需由超级管理员审核后才能继续使用，数据不删除。用户名为 `admin` 且原本是管理员时升级为超级管理员并保留现有密码，首次进入仍要求改密。若 `admin` 已被普通账号或大小写变体占用，启动应明确失败并要求人工处理，不能暗中提权。全新数据库才初始化 `admin/admin`。这是一次性数据转换，转换后只保留新规则。

## 接口与数据合同

| 接口或数据 | 目标变化 |
|---|---|
| `users`、`identity.User` | 用单一 `status=pending|rejected|approved|disabled` 取代 `disabled`；角色扩为 `user|admin|super_admin`，新增仅超级管理员使用的首次改密标记。保留账号和资源，不因禁用删除数据。 |
| `POST /api/auth/register`（现有） | 只创建 `pending/user`，不发会话；大小写不敏感地保留 `admin`，重复用户名返回 409。响应带审核状态。 |
| `POST /api/auth/login`、`GET /api/me`（现有） | 先验证密码：错误密码统一 401；正确密码但待审核、拒绝、禁用时返回可供页面提示的状态错误，不发可用会话。`/api/me` 返回角色、状态和是否必须改密。默认密码下的超级管理员只获得限权改密会话。 |
| `GET /api/admin/users`、`PATCH /api/admin/users/{id}`（现有） | 列表支持状态筛选并返回状态/角色；更新支持审核通过、拒绝、禁用、恢复及 `user↔admin`。服务端原子检查当前状态、操作者与目标；过期或不合法转换返回 409，越权返回 403。移除可绕过新规则的 `disabled`/代改密码字段。 |
| `POST /api/me/password`（拟新增） | 超级管理员用当前密码和新密码自行修改；首次限权会话只允许 `/api/me`、本接口和退出。成功后撤销旧会话，需用新密码重新登录；密码不得出现在响应或审计中。 |
| `POST /api/admin/users`、`portal user add`、`security.admin_credentials_file`（现有） | 移除直接创建可登录账号和“重启时同步唯一管理员”的旧入口与配置；新账号统一注册后审核，超级管理员仅在首次初始化时建立。 |

## 可执行任务

- [x] **WA-01（完成）统一合同与状态模型。** 验证：OpenAPI 类型生成通过；新增账号状态错误的 HTTP 映射测试通过。
  - 修改：`server/openapi/portal.yaml`、`docs/实施/00-总览与公共约定.md`；确定上述状态、角色、请求/响应和错误码，删去旧 `disabled` 与文件唯一管理员合同。前端生成类型归 WA-08 更新。
  - 验收：OpenAPI 校验和类型生成通过；注册、审核、改密及超级管理员保护在合同中只有一套定义。

- [x] **WA-02（完成，依赖 WA-01）迁移账号数据并建立超级管理员。**
  - 修改：`server/migrations/`、`server/internal/domain/identity/`、`server/internal/store/postgres/` 的用户查询/事务及生成代码；移除 `server/internal/config/admin_credentials.go`、`server/internal/app/admin_credentials.go`、`server/internal/store/postgres/admin_credentials.go` 中每次启动覆写管理员的路径，同步清理 `server/cmd/portal/`、`deploy/single/` 的旧配置和建用户命令。
  - 验收：隔离 PostgreSQL 从空库启动只建一个 `admin` 超级管理员，默认密码仅首次生效；重启不重置密码；现有非超级管理员按上文转为待审核/禁用并撤销旧会话，冲突用户名安全失败；并发启动不会生成两个超级管理员。
  - 验证记录：PostgreSQL 全套集成测试通过；覆盖固定 `admin` 迁移、旧密码保留、其他旧管理员待审核、旧会话撤销、冲突账号拒绝及并发初始化。

- [x] **WA-03（完成，依赖 WA-02）注册、登录和所有凭证的账号状态门禁。**
  - 修改：`server/internal/app/user.go`、`credentials.go`、`server/internal/api/auth.go`、`server/internal/domain/error.go`、HTTP 错误映射，以及 Session、API Key、MCP、设备凭证和 Gateway 当前依赖 `disabled` 的授权点；注册固定为待审核，登录先验密码再判断状态，非 `approved` 的账号不能获得或继续使用受保护能力。所有现有后台权限检查同时识别 `admin` 和 `super_admin`，不靠用户名在页面内临时放行。
  - 验收：新注册账号不能登录；审核状态提示只在密码正确时出现；错误密码不泄露账号状态。账号被禁用后，已有会话和其他凭证在后续请求失效，活动连接按现有撤权检查关闭；资源数据未删除。模拟已完成改密的 `super_admin` 身份可通过现有后台权限检查。Go 单测、HTTP 测试及隔离 PostgreSQL 测试覆盖这些路径。
  - 验证记录：`make test race cover` 通过；覆盖 Go 单测、HTTP/API 测试及配置的隔离 PostgreSQL 集成测试。

- [x] **WA-04（完成，依赖 WA-03）后台审核、禁用和角色调整。**
  - 修改：`server/internal/app/user.go`、`server/internal/api/admin.go`、`server/internal/store/postgres/` 的账号更新事务与审计；保留现有用户列表/详情元数据，增加状态筛选和目标动作，删除 `POST /api/admin/users` 直接建用户入口。审核、禁用、恢复、角色变更在事务中校验并撤销目标旧会话；审计记录操作者、目标、结果，不记录密码。
  - 验收：待审核可通过/拒绝，拒绝后可重新通过；已通过可禁用/恢复；只有已通过且可用的普通用户可被提升。管理员不能改自己状态/角色，不能操作 `admin`；并发审核/禁用/提权不会留下可越权会话，冲突返回 409。
  - 验证记录：服务层、API 与 PostgreSQL 事务/并发测试通过；Portal 后台组件测试覆盖审核和角色/状态操作。

- [x] **WA-05（完成，依赖 WA-03）超级管理员首次改密与默认密码保护。**
  - 修改：`server/internal/app/credentials.go`、`server/internal/api/auth.go`、`server/internal/api/api.go`、`server/internal/store/postgres/` 的改密事务和限权会话；默认密码期间仅允许回环地址经本机或 SSH 隧道完成首次登录与改密，公网请求不能用 `admin/admin` 取得会话。新密码按正常密码规则校验。
  - 验收：默认凭据在允许的本地入口只能访问改密流程；改密前管理 API 被拒绝。改密成功后旧密码/旧会话无效，新密码重启后仍有效；普通管理员直接请求 `admin` 改密或状态接口均被拒绝。
  - 验证记录：API 限权会话、首次改密、会话撤销和管理员保护测试通过；PostgreSQL 测试验证密码更新后重复初始化不会覆写新密码。

- [x] **WA-06（完成，依赖 WA-01/03；页面文件可与 WA-07 分开修改）用户登录与注册页面。**
  - 修改：`web/portal/src/routes/Login.tsx`、`Register.tsx` 及对应页面测试；沿用现有 `/login`→`/register` 入口，把注册成功提示改为“等待审核”，根据正确密码后的状态错误分别提示待审核、拒绝、禁用，不自动登录。
  - 验收：页面组件测试覆盖注册成功待审、重复用户名、两次密码不一致、各账号状态与错误密码提示；后台登录页仍无注册入口。连通真实接口的浏览器流程归 WA-08。
  - 验证记录：Portal 全部 185 项测试通过；本地浏览器模拟流程验证注册后显示待审核、无会话，正确密码显示状态提示、错误密码显示通用凭据错误。

- [x] **WA-07（完成，依赖 WA-04/05；页面文件可与 WA-06 分开修改）后台账号管理与改密页面。**
  - 修改：`web/portal/src/routes/admin/AdminUsers.tsx`、用户详情和拟新增超级管理员改密页及对应测试；替换直接建用户表单，展示审核状态筛选和审核/禁用/恢复/角色操作。`admin` 行保留受保护标识，不显示普通管理员可执行的操作；首次登录引导改密。
  - 验收：页面组件测试覆盖审核、拒绝后重新通过、禁用/恢复、授予/撤销管理员、`admin` 受保护控件和首次改密引导。直接 API 权限及浏览器全流程归 WA-08。
  - 验证记录：Portal 组件测试覆盖账号状态、角色和改密页面；本地浏览器确认后台用户列表展示超级管理员受保护标识及普通用户操作入口。

- [x] **WA-08（完成，依赖 WA-02～07）统一接线与本地端到端验收。**
  - 修改：`web/portal/src/App.tsx`、`src/app/api.ts`、`src/api/schema.d.ts`、`src/mocks/` 的共享路由/客户端/模拟响应；清除旧“管理员只能文件配置”和“注册后立即登录”的提示及残留分支。更新部署说明，标明默认密码仅首次初始化、改密前本地入口限制。
  - 验收：Server `make lint`、`make test race cover`、`make generate` 和隔离 PostgreSQL `make it`；Portal `typecheck/lint/test/build`。内置浏览器分别验证用户与后台主流程，实际接口验证注册、审核、权限变化和重启后的密码持久性；模拟测试结果不替代真实服务端验证。所有文件仅在本地修改，不向测试主机传输。
  - 验证记录：`make generate`、Server `make lint`、`make test race cover`、使用独立本地 PostgreSQL 的 `make it`，Portal `generate:api/typecheck/lint/test/build`（185 项组件测试和 2 项移动 Web 打包测试）、部署脚本 12 项测试、Shell 语法检查及 `git diff --check` 均通过。`make it` 中 OpenSandbox 集成因未配置测试沙箱地址/密钥而跳过。使用隔离 PostgreSQL、生产 `api.New`、账号服务和正式 Portal adapters 接线启动回环 API，并在内置浏览器实测：注册后待审核、待审核账号登录被拒、错误密码不泄露状态、重复用户名被拒；管理员通过审核后用户可登录；禁用用户后原会话失效且再次登录被拒；恢复账号、授予管理员后可访问后台；撤销管理员角色后旧会话失效且账号回到普通用户端；状态筛选有效，普通管理员看不到 `admin` 的操作入口。API 进程重启后用户和管理员会话仍有效；旧 `admin/admin` 失效，新密码有效。数据库最终状态为 `admin/super_admin/approved` 和合成测试用户 `user/approved`，并已撤销临时管理员角色。所有改动与验收仅在本地执行，未向测试主机传输文件。
  - 验收边界：账号及权限流程运行的是生产 API 路由、正式 adapters 和真实 PostgreSQL；未启动包含 Linux network-policy guard、Host/OpenSandbox 的完整 `portal serve` 部署栈，OpenSandbox 集成也未运行。本地首次改密通过生产 CredentialService 完成，浏览器在进程重启后验证新旧密码结果；首次改密页面另由 Portal 组件测试覆盖。

