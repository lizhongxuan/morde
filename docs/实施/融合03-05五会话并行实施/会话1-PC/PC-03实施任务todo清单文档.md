# PC-03：PC Host、工作台与普通用户资源实施 TODO

> 开发量：按一个 AI 会话、前置就绪后的连续开发估算，最多一天；只计实现与交接记录。测试编写、运行和验收单独进行，不计入这一天。
> 范围估算不是完成承诺；发现超出一天时把剩余实现按文件/接口继续合并拆开，保持原验收条件。
> 当前状态：进行中（相关原任务已有实现，按现有代码继续）。代码完成后标“待验证”；对应验证完成后再勾选并记证据。
> 直接前置：可先用既有 Host/Workspace 合同开发；真实文件状态依赖 F02-T10 与 PC-02。
> 修改边界：Portal 普通用户页面、桌面 Host 容器和共享 Workbench；管理员页面由会话 5 负责。
> 写入规则：[五会话执行入口](../并行执行入口.md)；只修改本会话的指定文件与本会话文档。公共合同/路由/迁移变更在本文件交接给会话 5。
> 本地约束：只在本地修改和验证，不发送或上传文件，不向远端部署。

## 本次开发

- [ ] **PC-03.01（待验证：本地实现及模拟验证完成；真实服务/权限联验未完成）接入目标及控制**：保持在途 Task 目标固定，区分 Host 状态、查看、接管与交还，未知结果不伪报成功。
- [ ] **PC-03.02（待验证：本地普通用户入口、受权 Host 及 Workspace 只读路径完成；真实账号、撤权和跨账号验收未完成）接入工作台和用户资源**：补齐受权 Host 会话、资源/设置入口、成员权限及 Workbench 状态。
- [ ] **PC-03.03（进行中：同一路由重复入口已移除；其余导航收敛依赖 PC-03.02、F03-T07.05 和 F03-T07.06）收敛重复入口**：对应新入口实际完成后再调整旧导航，未迁功能保留。

## 原任务与已有进度

下列编号用于追溯完整业务规则和拆分前证据；同一原编号涉及多个会话时，各会话只完成自身范围。原项全部实现及验收通过后才可视为完成。

| 原编号 | 原任务 | 拆分前状态 |
|---|---|---|
| [F03-T03.03](../../融合03-PC与iOS客户端融合实施任务todo清单文档.md) | 接入 PC 电脑和工作区目标 | 待验证：本地实现完成，真实权限/任务目标联验待服务端，依赖 F03-T02.01 |
| [F03-T05.04](../../融合03-PC与iOS客户端融合实施任务todo清单文档.md) | PC 接入人工接管和交还 | 待验证：PC 本地实现完成；依赖 F03-T05.01 |
| [F03-T07.01](../../融合03-PC与iOS客户端融合实施任务todo清单文档.md) | 准备唯一共享 Workbench 模块 | 完成 |
| [F03-T07.02](../../融合03-PC与iOS客户端融合实施任务todo清单文档.md) | PC 接入受权电脑和工作台 | 本地 MVP 实现完成，待真实过期/跨账号验收，依赖 F03-T07.01、F03-T05.04 |
| [F03-T07.04](../../融合03-PC与iOS客户端融合实施任务todo清单文档.md) | 迁入 PC 资源和普通用户设置 | 本地 PC 用户入口部分完成，待真实服务与权限验收；依赖 F03-T02.01 |
| [F03-T07.06](../../融合03-PC与iOS客户端融合实施任务todo清单文档.md) | 接入真实 Workspace 文件与同步状态 | 进行中：Host Gateway 固定 applied revision 的只读 manifest/文件读取及显式成员权限已在本地实现；PC/iOS 共用合同、真实同步验收及 F02-T10 仍未完成，依赖 F03-T06.06、F03-T07.02、F03-T07.03、F02-T10 |
| [F03-T07.07](../../融合03-PC与iOS客户端融合实施任务todo清单文档.md) | 收敛已迁普通用户重复入口 | 未开始，依赖 F03-T07.04、F03-T07.05、F03-T07.06 |

## 交接

- 在本文件记录完成的接口、文件和待会话 5 处理的公共改动；不要直接改其他会话负责的文件。
- 对应验证在 [独立验收清单](../验收资料/验收入口.md) 执行并记录，不计入本开发文档的一天预算。
- `server/AGENTS.md` 要求接口、实现和测试在同一变更交付；可先完成代码并标“待验证”，合入/提交前再按独立验收清单补齐测试与所需检查。

## 本轮复审、实现与验证（2026-11-02，续）

- `web/workbench/src/app/Workbench.tsx`：修复已有控制状态快照下的轮询失败被忽略问题。状态读取失败时标记控制状态暂不可用，桌面切为只读、终端/文件/VS Code 操作受限，并隐藏基于旧快照的控制动作；成功重读后恢复对应权限。`Workbench.test.tsx` 新增用例覆盖状态失联后的失败关闭和重读恢复。
- `project/nanoMuse/harness/desktop/tests/morde-main-recovery.test.mjs`：新增共享 Host Gateway 来源复核和会话撤权后关闭 Host 窗口的回归测试；桌面实现原已包含相应保护，无新增 IPC 或本机权限。
- Portal 普通用户页面本轮未改代码。内置浏览器通过本地 MSW 检查云端电脑、共享成员权限、工作区同步未确认状态、账户设置；Assistant 本地模拟提交后显示固定的 Host 与 Workspace v0。Workbench 本地模拟验证了接管失败重试、成功接管、交还 AI 及只读状态切换。全部操作只到本机开发服务/mock。
- `web/workbench`：`npm run typecheck`、`npm test`（9 个文件/76 项）、`npm run lint`、`npm run build` 通过。
- `web/portal`：首轮 `npm run typecheck`、9 个相关路由测试（85 项）、`npm run lint` 通过；全量结果见下方续审记录。真实服务权限验收仍未完成。
- `harness/desktop`：`npm run build:test` 与 `node --test tests/morde-main-recovery.test.mjs` 通过（28 项）。
- PC-03.01/.02 本地实现与模拟验证完成，但保留“待验证”：目标权限、Task 目标回读、共享 Host 撤权/过期及跨账号行为仍须真实服务/账号联验。F03-T07.06 文件操作依赖 F02-T10 与跨端合同；F03-T07.05 的 iOS 成员/模型/API Key 页面迁移未完成，因此本阶段不收敛其他旧入口。
- 上一项 PC-02 尚有未完成项，本轮只核对未触碰其代码：服务端仍未公布 `workspace_sync` capability；Task 提交仍由 `workspace_id` 与 `input_revision` 选择输入，Task 响应已包含 `workspace_checkpoint_revision`，但没有客户端确认的文件范围及 checkpoint 摘要合同。相关 server/OpenAPI 文件由会话 5 维护，文件同步验收又依赖 F02-T10，不能由本会话客户端改动代替。PC-02.01 和 PC-02.03 继续待相应合同和真实节点联验。

## 继续审查与合同交接（2026-11-02）

- 全量验证补齐：`web/portal` 的 Vitest 30 个文件/222 项与 2 个 Node 脚本、`npm run typecheck`、`npm run lint` 均通过；桌面测试 232 项通过。Workbench 的 9 个文件/76 项、typecheck、lint、production build 已在上一轮通过。均为本地验证。
- 复核 `server/openapi/portal.yaml` 与 API 路由后确认 Workspace 文件 API 已存在，但当前不能直接接入受权 Host Workbench：manifest 是固定 revision 的分页元数据；内容通过同步会话块接口读取。OpenAPI 将 manifest 标为 Portal session/Bearer 认证；`sync-progress` 明确仅 Portal session，并且只汇总显式同步 revision，不含路径和冲突。Host Gateway 目前只注册受其 cookie session 保护的 Host、control、application 和 terminal 等接口，Workbench `GatewayPaths` 也没有 Workspace manifest、块内容或进度接口。不能把 Host cookie、Portal session 或设备 Bearer 凭据互相替代。
- **交接会话 5：**请补齐受 Host Gateway 会话保护的 Workspace 文件读取合同及权限校验：绑定当前有效的 Host→Workspace；验证 Workspace 所有权或显式文件访问权（桌面共享权限不能自动授予文件权限）；提供固定已应用 revision 的分页 manifest、受控块读取和 Host 可用的同步/应用进度；清楚返回未应用、过期 revision、同步中、冲突、未授权与服务不可用状态。Workbench 不应接触或转发 Portal cookie、Bearer token 或同步设备凭据。另请与 PC-02 对齐 Task 输入文件范围及 checkpoint 摘要合同，再由本会话接入并验收真实文件。
- F03-T07.06 现为“进行中”：Host Gateway 已返回每个 Workspace 的 `quota_bytes`、`sync_state` 和 `sync_conflicts`；Workbench 增加独立只读浏览/下载页，只在运行中 Host、有效 binding、健康且无冲突、服务端/目标/已应用 revision 一致并由所有者接管后读取 Host 上对应 Workspace 目录。该实现复用 Host AIO 当前目录投影，不是 canonical manifest/block API；PC/iOS 共用文件合同、上传/同步、真实跨端验收及 F02-T10 仍未完成。PC-03.03 的完整导航收敛依赖 T07.04–T07.06 实际迁移完成；PC-03.01/.02 保持“待验证”，还需真实账号验证目标/接管权限、撤权/过期及跨账号隔离；本机 mock 不能替代这些验收。`F02-T10` 的实现关闭不代表 Ubuntu 节点互传或运行验收已通过，能力仍关闭。

## 本轮实现（2026-11-02）

- `server/internal/gateway/control.go`：Host response 的 Workspace 增加配额、同步状态与冲突路径；空冲突集合按 `[]` 输出。`TestHostResponseIncludesWorkspaceSyncMetadata` 通过（`go test ./internal/gateway -run '^TestHostResponse' -count=1`）。
- `web/workbench/src/components/WorkspaceBrowser.tsx`：新增只读 Workspace 浏览和下载；按 binding ID 固定路径，只列出/下载当前绑定目录，状态轮询失败、目录信息异常、同步冲突或 revision 未应用时不显示文件；配额和冲突可见。原“主机文件”管理器保持独立。5 项组件测试覆盖正常浏览/下载、冲突、无效目录、越界响应和状态失联失败关闭。
- 配额边界补齐：Workspace 用量超过或配额值无效时不请求/显示文件；新增超配额失败关闭用例。
- `web/workbench/src/app/Workbench.tsx`：新增 Workspace 导航页，并修复创建新终端时旧初始化 effect 竞争复用原会话的问题。
- `docs/实施/00-总览与公共约定.md`：同步 Gateway Workspace 元数据合同，并把 Host 卷路径改为模板的 `Workspace.MountPath`（当前发布模板为 `/home/gem/workspace`）。
- 验收证据：`web/workbench` 的 `npm test`（10 个文件/83 项）、`npm run typecheck`、`npm run lint`、`npm run build` 通过；Host response 定向 Go 测试 `go test ./internal/gateway -run '^TestHostResponse' -count=1`、`gofmt -d` 与 `git diff --check` 通过。内置浏览器用本机 MSW 操作“AI 状态不展示文件→重试接管→列出绑定 Workspace README→触发下载”，浏览器下载事件和成功提示均可见；超配额边界由组件测试验证。
- 测试站只读探测：`GET /api/capabilities` 返回 404 `not_found`，`/healthz` 与 `/readyz` 返回 200；未登录、未连接 Host、未提交凭据或文件。当前部署不能用于本轮真实客户端/Host 权限联验。
- 截至本轮续做前的验收状态：PC-03.01/.02 仍“待验证”，PC-03.03 未开始；F03-T07.06 部分实现后进入“进行中”。真实撤权/跨账号/iOS/文件同步验收未完成；本轮只在本机修改和验证，不上传文件或部署。

## 本轮续做（2026-11-02）

- `project/nanoMuse/android/src/ios/Morde/MordeApp.swift`：Host WKWebView 将同源主框架触发的 `blob:` 下载交给 `WKDownload`，拒绝跨源目标、跨源 blob 和子框架下载；重定向仍校验同源。下载先写入隐藏 `.part` 文件，成功后在同一目录原子移动到净化且不冲突的文件名；失败或关闭 Host 页面时清理部分文件。文件保存在 `Documents/Morde Host Downloads`；iOS Documents 文件共享已启用，可在“文件”App 中访问。
- `project/nanoMuse/android/src/ios/Tests/MordeHostNavigationTests.swift`：覆盖下载来源/目标、主框架限制、路径净化、重名与部分文件晋升。`Tests/run-host-navigation-tests.sh` 通过；从 nanoMuse 仓库执行的 `xcodebuild -quiet -project android/src/ios/Morde.xcodeproj -scheme Morde -configuration Debug -destination 'generic/platform=iOS Simulator' CODE_SIGNING_ALLOWED=NO build` 通过；相关 iOS `git diff --check` 通过。
- `web/portal/src/components/PortalLayout.tsx`：移除与主导航“工作区”同路由的“我的工作空间”chip，保留主导航及仍有用途的管理入口；`.morde-space-chip` 样式仍被 AdminLayout 使用，因此保留。组件测试增加唯一 Workspace 路由链接断言。内置浏览器 `/workspaces` 页面确认主导航有 1 个 Workspace 链接、重复 chip 为 0；定向 Vitest 1 项通过。
- 全量 Portal Vitest 本轮为 237 项通过、5 项失败（AdminQuality 1 项、AdminRecords 4 项，均在本轮改动边界之外）；不据此宣称全量 Portal 验收通过。真实设备 WKDownload 下载行为、PC-03.01/.02 真实撤权/跨账号联验、F03-T07.06 canonical manifest/block 合同与真实跨端同步仍待做；仅尝试只读打开测试站管理员登录页，但内置浏览器 30 秒未返回可交互状态，未输入凭据。未提交任何凭据/文件、未上传或部署。
- 本轮续验补充：当前 `web/workbench` 的 `npm run typecheck`、`npm test`（10 文件/83 项）、`npm run lint`、`npm run build` 均通过；Portal `PortalLayout.test.tsx` 1/1 通过；Workbench、Portal 导航及相关 iOS 文件的 `git diff --check` 通过。先前本机内置浏览器的 MSW 操作记录仍有效，本轮未访问远端或提交凭据。
- Host Gateway/Workspace 合同只读审查确认：现有成员权限没有文件读取授权，`desktop:view` 不可推导 Workspace 内容权限；Workspace blocks 读取依附同步会话，Host cookie 不能冒充 device/HostReplica 凭据。按本 TODO 的公共合同边界交接会话 5：新增 Host Gateway 专用只读服务与路由，动态校验 Host cookie、调用者独立文件权限、有效 Host→Workspace binding 和已应用 revision；固定 revision 分页 manifest/文件读取需验证相对路径、块摘要与大小，并将未应用、同步中、冲突、未授权、不可用分开返回。共享成员权限需另增显式 `workspace:file:read` 授权及分享 API/持久化校验；不得复用桌面权限或暴露 Portal/Bearer/同步设备凭据。审查未改服务器文件；PC-03.02 与 F03-T07.06 继续待该合同和真实同步验收。

## 本轮续做（2026-11-02，Host Workspace 文件读取合同）

- 本地实现了独立成员权限 `workspace:file:read`，不从 `desktop:view` 推导；共享 Host 通过 Portal 申请专用 Workspace ticket 后进入 `/__ch/wb/?tab=workspace`，Workbench 页面只使用 Host Cookie 会话，不转发 Portal、Bearer 或同步设备凭据。Portal 成员权限编辑、入口、API/OpenAPI、错误映射和 schema 已同步。
- Host Gateway 新增 `GET /__ch/api/workspaces`、`GET /__ch/api/workspaces/{workspace_id}/manifest?revision=...`、`GET /__ch/api/workspaces/{workspace_id}/files/{path}?revision=...`。清单要求显式 revision（`0` 是合法固定 revision）；服务检查调用者权限、有效 Host→Workspace binding 与 applied revision，并在文件流分块前后复核授权/绑定，校验块和整文件 SHA-256 与大小。Workspace 元数据/块不可用返回 `workspace_unavailable`（503）；Workbench 的 `?tab=workspace` 不调用所有者控制 API。
- `server/migrations/00100_host_member_workspace_file_read.sql` 扩充成员权限约束。回滚会移除单独的文件读取授权，并从混合授权中去掉该权限，避免回滚扩大既有权限。
- 本地验证：Linux 容器 `make test`、`make race`、`make lint` 均通过；Portal Vitest（35 文件/251 项）、typecheck、lint 通过；Workbench Vitest（10 文件/90 项）、typecheck、lint、build 通过。针对 PostgreSQL 的 `TestHostSharingPersistenceAndTicketActor` 使用本地临时 PostgreSQL 通过，覆盖新权限持久化与迁移 Up。此前完整 PostgreSQL 集成套件有 12 项在 F02/管理员/Workspace sync 其他范围失败，故不记为全量 DB 验收通过。
- 当前状态：PC-03.01、PC-03.02 仍待真实服务/账号的目标权限、撤权、过期和跨账号隔离验收；PC-03.03 仍进行中，导航剩余调整依赖 F03-T07.05/F03-T07.06。F03-T07.06 的本地 Host Gateway 路径已实现，但 PC/iOS 共用合同、真实跨端同步和 F02-T10 Ubuntu 节点验收未完成。全程只修改和验证本地代码/临时本地数据库，未访问测试站、未使用所给远端凭据、未上传或部署。

## 会话1-PC目录复核与本地回归（2026-11-02）

- 逐项对照 PC-01 至 PC-05 清单与当前代码。PC-01、PC-04 的本地实现没有发现待补代码；真实服务与账号验收仍按各自清单保留。PC-02.02 本机副本闭环已完成；PC-02.01/.03、PC-03.01/.02/.03 和 PC-05.02/.03/.04 的待办仍受各自记录的服务、跨会话合同、平台隔离或真实设备验收条件约束。
- 合同复核确认，`TaskInputRequest` 只回答 Task clarification，不代表 Workspace 文件选择；Task 有 `input_revision`/`workspace_checkpoint_revision`，Task-linked sync 也已限制为单文件上传，但还没有用户确认的 Workspace 路径范围与输入检查点摘要。PC-05 的 `file.put` 服务端结算代码仍未进入生产 Hub 工具列表；Desktop Workspace 映射与 DSH `fileRoot` 尚无可信桥。PC-02.03、PC-05.03 继续关闭文件 Task 提交/写入，不能因这些部分结构而标完成。
- F02 清单虽然关闭了实施项，仍明确 `workspace-sync` capability 保持关闭，Ubuntu peer/数据卷验收没有通过证据；PC-02.01 继续等待 capability 和真实节点联验。PC-05.02 的 macOS/Windows 命令继续关闭；PC-05.04 仅已有的 Morde 桌面 `open/notify` 迁移完成，其他旧 Cloud 动作按合同逐项评估。
- 复验通过：Desktop `npm run typecheck`、`npm test`（234 项）；DSH `npm run build`、`npm run typecheck`、`npm test`（418 项）；Portal `npm run typecheck`、`npm run lint`、`npm test`（42 个 Vitest 文件/295 项及 2 个 Node 测试）；Workbench `npm run typecheck`、`npm run lint`、`npm test`（10 个文件/90 项）、`npm run build`；`project/morde-sync` `go test ./... -count=1`；Server Host response 定向测试 `go test ./internal/gateway -run '^TestHostResponse' -count=1`；Workbench 相关 `git diff --check`。
- 复跑首次发现 `WorkspaceBrowser.test.tsx` 在 manifest 请求完成前同步读取文件表格，造成时序竞争；组件仍处于加载状态时，测试误把状态摘要中的容量当作文件单元格。测试改为 `findByRole` 等待目标单元格后，Workbench 全套 90 项通过；未改组件行为。
