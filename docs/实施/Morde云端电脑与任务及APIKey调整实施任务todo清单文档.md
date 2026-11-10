# Morde 云端电脑、任务与 API Key 调整实施任务 TODO 清单

来源：[需求文档](../Morde云端电脑与任务及APIKey调整需求文档.md)、[设计方案](../Morde云端电脑与任务及APIKey调整设计方案.md)。本清单只规划实施；任务完成并通过对应验收后再勾选。

## 修改边界与依赖

涉及账号/主机持久化、登录与创建服务、OpenSandbox 编排适配、管理员与用户页面、API Key 授权、任务/计划 HTTP 与 MCP 入口及 Worker。历史任务、计划、电脑和工作区数据保留。现有 `quota_hosts` 未在主机创建事务中校验；停机电脑也须占台数。

**前置 D0**：“AI 助手”按[独立设计](../Morde即时对话设计方案.md)改为即时对话；它的原发送流程会创建后台任务。发布前须在配置可用模型的环境中验证可发送且不生成任务，随后才能在已部署环境执行 T07 的存量排空与 T08 的任务能力切换。

## 关键接口变化

| 接口/模块 | 实施后的合同 |
| --- | --- |
| `POST /api/auth/login`、`POST /api/auth/client-login`、`OnboardingService.EnsureDefaultHost` | 可用账号登录时确保一台默认关机电脑；并发只建一台，已有电脑不新增。默认电脑操作不触发启动。 |
| `POST /api/hosts`、MCP `host_create`、`HostService.Create` | 请求不接收用户选定的 `template_id`；服务端取账号默认规格。创建事务按未删除电脑数校验 `quota_hosts`，达到上限返回 `quota_exceeded`；幂等重放仍返回原电脑。 |
| `PATCH /api/admin/users/{id}`（现有） | 沿用 `quota.hosts`，增加 `default_host_resources`（`cpu_milli`、`mem_mib`）；保留现有账号状态/角色条件和审计。 |
| `PATCH /api/admin/hosts/{id}/resources`（拟新增） | 仅管理员可提交 `cpu_milli`、`mem_mib`、`expected_version`；运行中必须完成在线调整与回读后返回新规格，失败保留旧值或明确待核验，不能假成功。 |
| `POST /api/apikeys`、`CredentialService.VerifyAPIKey` | 创建请求只需名称、可选有效期；所有有效 Key 按动态账号权限运行，验证时仍检查账号状态、主机所有权/共享授权。迁移时撤销旧的按范围授权 Key，不留双逻辑。 |
| `/api/tasks*`、`/api/schedules*`、任务型对话提交、MCP `task_*`/`schedule_*` | 停用并拒绝任务/计划请求；OpenAPI、生成类型、Portal 和手机端调用同步移除。即时对话接口由 D0 独立确定。 |

## 可执行任务

- [ ] **D0（待验证）将 AI 助手改为即时对话。**
  - 修改：独立即时对话服务和存储、Portal 接口、`web/portal/src/routes/Assistant.tsx` 及其调用；直接生成并保存消息，不再创建后台任务。校验登录账号与模型连接权限；纯文字对话不访问云端电脑。
  - 验收：内置浏览器发送并刷新后仍能看到回复；直接查询确认没有新增任务或计划，异常请求不会伪装成功。
  - 本地进展：内置浏览器在本地 mock 门户发送消息、收到回复，刷新并重新打开后仍见回复；旧 `/tasks` 为不存在页面。手机 Web 已切换同一即时对话合同，23 个测试文件/146 项测试与构建通过。应用服务与 PostgreSQL 联验已核对回复落库、幂等重试、模型失败不保存假回复，且任务/计划表不新增记录。迁移 00053 保存原请求对话目标，目标、模型或正文变化时同一重试键返回冲突；应用测试与全量 PostgreSQL 测试通过。真实服务的 AI 助手页面可打开，但本地测试账号未配置模型，发送按钮不可用；真实模型回复仍待验证。

- [x] **T01（已完成，本地验收）统一规格、配额和接口合同。**
  - 修改：`server/openapi/portal.yaml`、`docs/实施/00-总览与公共约定.md`、`server/migrations/` 新版本迁移、`server/internal/domain/identity/types.go`、`server/internal/app/user.go`、`deploy/single/portal.yaml` 与 1 核 2 GiB 模板、用户/主机数据转换及生成类型。新用户默认 `quota_hosts=1`、1 核 2 GiB；账号保存新建默认规格，主机保存在线调整的版本/核验状态。通过配额变更审计保留管理员明确设定的既有上限，其余旧默认值改为 1；既有主机不删。
  - 验收：空库与旧库迁移后默认值、原有主机及管理员明确设置的上限正确；OpenAPI 生成和类型检查通过。
  - 证据：PostgreSQL 全量集成测试通过；迁移测试覆盖既有账号/电脑及管理员配额；`make generate`、OpenAPI 类型生成和 Portal typecheck 通过。

- [x] **T02（已完成，本地验收）登录自动分配关机电脑。**
  - 修改：`server/internal/app/credentials.go`、`client_login.go`、`onboarding.go`，`server/internal/store/postgres/` 的原子确保操作及 `server/internal/api/auth.go`；成功登录时共用同一确保逻辑，创建 `stopped` 记录并设置默认电脑，不创建 Sandbox。
  - 验收：Web/客户端初次登录各得一台未开机的 1 核 2 GiB 电脑；重复和并发登录不多建；上限为 0 时不自动创建；存储失败时不返回虚假的成功结果。
  - 证据：Go 登录/并发单测及 PostgreSQL 集成测试通过；内置浏览器确认首次登录出现一台已停止的 1 核 2 GiB 电脑。

- [x] **T03（已完成，本地验收）统一用户创建限制。**
  - 修改：`server/internal/app/host_create.go`、`server/internal/store/postgres/tx_host.go`、`server/internal/api/hosts.go`、`server/internal/mcp/tools.go`、`web/portal/src/routes/NewHost.tsx`、`Hosts.tsx` 及 API 调用；服务端取账号规格，开机/恢复从主机保存的规格生成运行时请求，事务内校验未删除电脑总数。用户端不提供规格选择，已达上限点击创建只显示指定文案。
  - 验收：停机/失败/删除中的电脑仍占台数；网页、直接 API 和 MCP 均不能超额或提交自选规格；管理员调高上限后可创建至新值；并发创建与幂等重试不超额。
  - 证据：Go 创建事务、HTTP/MCP 边界及 PostgreSQL 集成测试通过；内置浏览器点击“创建电脑”显示“达到云端电脑上限,无法再创建”。

- [x] **T04（已完成，本地真实运行环境验收）打通在线规格调整。**
  - 修改：`server/internal/app/` 主机管理用例、`server/internal/store/postgres/` 容量预留与版本校验、`server/internal/orchestrator/client.go`；若当前 OpenSandbox 无在线调整能力，在本地 `project/OpenSandbox/` 补齐实际 CPU/内存调整与回读，再接入拟新增管理员主机接口。停机电脑只更新目标规格，运行中电脑不重启。
  - 验收：真实运行环境中管理员调大/调小运行中电脑规格后，运行时 CPU/内存限制和页面值立即一致，主机进程未重启；容量不足、并发冲突、运行时失败及结果不明时均不报告成功，也不覆盖原有效规格。
  - 证据：OpenSandbox Docker runtime 已补在线 CPU/内存调整与回读，Python 289 项、Go 定向测试和 PostgreSQL 集成测试通过。本地 Ubuntu 虚拟机运行当前 Portal、PostgreSQL 与挂载当前源码的 OpenSandbox，内置浏览器经管理员页面将运行中电脑从 1500m/3072MiB 调至 2000m/4096MiB，再调至 1000m/2048MiB；页面、数据库和 Docker 实际限制一致，容器 ID 与启动时间未变。旧版 OpenSandbox 不支持改配时页面报错、数据库保留原值；超额请求被拒绝，刷新后原规格不变。并发冲突、结果不明由 Go/PostgreSQL 测试覆盖。Kubernetes/FSB runtime 不支持在线调整；正式镜像构建与发布仍需在 T09 核对。`project/OpenSandbox/` 是独立 Git 仓，源码改动需与主仓一同交付。

- [x] **T05（已完成，本地真实运行环境验收）接入管理后台与用户端展示。**
  - 修改：`web/portal/src/routes/admin/AdminUserDetail.tsx`、`AdminHostDetail.tsx`、`AdminUsers.tsx`、`web/portal/src/app/api.ts`、云端电脑列表及相关测试/模拟数据；管理员能改用户上限、默认规格和单台现有电脑规格，服务端同步校验总 CPU/内存配额，用户只查看上限与实际规格。
  - 验收：管理员保存成功后两端刷新显示一致；失败保留旧值并提示；普通用户页面没有修改入口，直接请求管理接口返回拒绝。
  - 证据：内置浏览器连接本地真实服务，管理员将普通用户上限改为 2 台、默认规格改为 1500m/3072MiB；用户随后创建第二台电脑，实际启动时采用该规格，再点创建显示“达到云端电脑上限,无法再创建”。管理员修改停机电脑及运行中电脑规格后，用户端刷新显示一致，且无规格编辑入口。超额 5000m 请求被拒绝并保留原规格；普通用户直接请求管理员改配接口返回 403。截图仅存本地 `.local/`。

- [x] **T06（已完成，本地验收）API Key 统一按账号当前权限授权。**
  - 修改：`server/internal/app/resources.go`、`credentials.go`、`host.go`、`host_sharing.go`、`server/internal/domain/identity/`、`server/internal/store/postgres/`、`server/internal/api/resources.go`、`server/internal/mcp/`、`web/portal/src/routes/ApiKeys.tsx` 及契约/测试；迁移时撤销旧的按范围授权 Key，不保留旧/新双逻辑，去掉主机和权限复选框及任务权限说明。
  - 验收：新 Key 无需勾选即可访问账号当前有权使用的电脑及后续新增电脑；共享撤回、账号禁用立即限制 Key；不能访问管理接口或绕过 T03；迁移前 Key 已失效，任务接口仍不可用。
  - 证据：API Key 授权、共享撤回、账号禁用与旧 Key 迁移的 Go/PostgreSQL 测试通过；创建接口已删除旧 `host_ids`/`scopes` 要求，真实服务页面只需填写名称与可选有效期。隔离测试库中创建的 Key 可读取账号当前两台电脑；额外创建被配额拒绝、管理接口被拒绝，吊销后无法再访问。测试 Key 已吊销，未向外部服务创建 Key。

- [ ] **T07（待验证（本地封口完成，存量运行环境未排空））先停止存量任务与计划。**
  - 修改：`server/internal/app/execution_control.go`、`server/internal/store/postgres/execution_control.go`、`server/internal/worker/task_stop_worker.go` 和本地发布步骤；先在所有实例封住任务新建与重新启用入口，再禁用所有已有计划，使用现有安全停止流程处理排队和运行中的任务，并确认外部操作已停止。保留历史数据。
  - 验收：封口后网页/API/MCP/对话入口均不能再生成任务；计划不再触发；已运行任务有明确终止结果，停止 Worker 无未处理项后才继续 T08。
  - 本地进展：PostgreSQL 集成测试已用真实存储与停止 Worker 排空排队/运行中的测试任务、待处理停止请求，并核对账号执行状态转为禁用；已部署环境的存量任务尚未排空与核对。

- [ ] **T08（待验证（本地入口已移除，依赖 T07 真实排空））移除任务入口与执行接线。**
  - 修改：`server/cmd/portal/main.go`、`server/internal/api/api.go`、`server/internal/api/tasks.go`、`schedules.go`、`conversations.go`、`server/internal/mcp/`；`web/portal/src/App.tsx`、`PortalLayout.tsx`、`Dashboard.tsx`、`Notifications.tsx`、`Tasks.tsx`、`TaskDetail.tsx`、`SchedulePanel.tsx`、`ApiKeys.tsx`、`web/portal/src/app/api.ts`，以及 `project/nanoMuse/web/src/morde/api.ts`、`App.tsx`、生成类型和相关测试。停用任务/计划路由、MCP 工具与执行 Worker，清除死链；保留 D0 的即时对话入口。
  - 验收：旧任务路径和 HTTP/MCP 请求均无法执行任务或计划；用户端无任务菜单、页面、卡片或死链；AI 助手即时对话仍可发送且不产生任务；历史数据未删除。
  - 本地进展：手机 Web 已移除 Task/SSE/审批/计划业务入口，沿当前即时聊天 API 发送；Host、Workspace、通知和登录仍可用，23 文件/146 项测试及构建通过。Portal 157 项 Vitest、2 项 Node 脚本测试和包含手机 Web 的整包构建通过；真实服务内置浏览器见旧 `/tasks` 不存在，`/api/tasks` 与 `/api/schedules` 均返回 404，隔离库任务/计划表无新增记录。未接线的任务执行 Worker 与计划 Worker 实现及测试已删除，保留独立的存量停止 Worker；随后 `go test ./...` 与 `make lint` 通过。Workbench 已从当前 OpenAPI 重新生成类型，旧任务/计划路径消失；类型检查、Lint、75 项测试和构建通过。全量 PostgreSQL 集成测试已复跑通过；T07 已部署环境未排空，故不勾选。

- [ ] **T09（待验证，本地完整链路已验，正式部署未联验）本地联验与合同同步。**
  - 修改：各任务对应的 Go/页面测试、OpenAPI 与生成类型，以及必要的本地验收记录；不向外传输文件。
  - 验收：Server 的 `make lint`、`make test race cover`、`make generate` 和 PostgreSQL 集成测试通过；Portal 的 `typecheck/lint/test/build` 通过。内置浏览器核对首次登录、配额提示、管理员在线改规格、API Key、AI 助手即时对话与任务入口消失；在本地真实运行环境核对在线规格和存量任务停止，不能用模拟结果代替。
  - 本地进展：服务端带本机 PostgreSQL 的全量 `go test ./...`、`go test -race ./...`、`make lint`、`make generate`、`make cover` 均通过；生成物前后摘要一致，总覆盖率 71.5%。新增即时对话请求目标迁移后，全量 PostgreSQL 测试及 Lint 再次通过。Portal 157 项 Vitest、2 项 Node 脚本测试及 typecheck/lint/build 通过，nanoMuse Web 146 项测试与构建通过；Workbench 类型从当前 OpenAPI 重生成，75 项测试及 typecheck/lint/build 通过。另在本地 Ubuntu 虚拟机启动当前 Portal、PostgreSQL 和当前源码 OpenSandbox，经内置浏览器及数据库核对：首次登录恰好一台未开机 1 核 2 GiB 电脑，无 Sandbox；管理员调高上限和默认规格后用户可创建第二台，达到上限提示正确；运行中规格双向在线调整，Docker 容器未重启；超额拒绝后原值不变；用户端只读显示一致。真实 API Key 创建后可访问当前电脑、受创建配额限制、不能进入管理接口，吊销后失效；该测试 Key 已立即吊销。旧 `/tasks` 页面和 `/api/tasks`、`/api/schedules` 接口均不存在，测试库任务/计划数为零。真实模型未配置，AI 助手在此环境无法发送并验证回复；已部署环境的存量任务未排空，正式镜像构建与发布未验收。2026-10-27 只读检查所给部署地址的 `/admin/login`，GET 返回 404，未提交凭据或传输文件；因此 T09 保持待验证。
