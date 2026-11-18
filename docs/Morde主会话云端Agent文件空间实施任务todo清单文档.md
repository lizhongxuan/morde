# Morde 主会话云端 Agent 文件空间实施任务 TODO

> 来源：[Morde主会话云端Agent文件空间设计方案.md](Morde主会话云端Agent文件空间设计方案.md)
> 状态：T1–T5 的本地实现及 PC 文件页验收已完成；T2 实时目录开关仍默认关闭；T3 挂载开关关闭、共享 Sandbox 未验，且已要求挂载与实时目录源开关必须同时开启；T6 已新增 PC 项目上传直写共享目录、稳定目录注册和 Host 切换后来源重绑，并通过应用层、PostgreSQL 定向验收及 LinuxKit 文件存储测试；旧文件编辑/恢复、真实来源核对和生产切流仍未完成；T7 等待 T3、T6。

## 实施边界

- 每位用户拥有一个稳定的服务端 Agent Workspace 真实目录；当前云端电脑必须强绑定该 Workspace 并挂载到 `~/workspace/`，不允许显式绑定其他 Workspace。切换电脑时将同一个 Workspace 绑定切到新电脑，不复制或更换目录。
- 运行时不维护服务端与云端电脑之间的副本或同步队列。保留独立的历史快照能力，以及用户在 PC 主动触发的项目文件上传入口。
- 用户入口交付在 morde-pc；可以复用内嵌 Portal / Workbench 代码，不作为独立普通用户 Web 端交付。

## 问题记录

- 当前 workspacefs 块和 revision 仍承载历史快照及部分旧写入路径；workspacecurrent 同时被主会话文件 API 和云端电脑 HostPath 使用。HostPath 直接写目录会绕过旧 manifest 写入路径。Agent 的 DSH 实时读写及 PC 项目上传已有直接目录实现，但需显式开启服务端开关且默认关闭；旧文件操作和恢复仍待改造。
- 已在 revision 物化前校验当前目录是否仍匹配已应用 revision；目录漂移时返回冲突并保留现场。此校验是数据保护措施，不能替代唯一实时数据源改造，也不能消除无共同文件锁时的跨容器并发写入风险。
- `workspacecurrent.Materialize` 已改为保持 Workspace 根目录 inode，并在同一根目录内分阶段更新；服务端读写 API 会在更新锁内等待，检测到中断备份时失败关闭并保留现场。直接目录模式下，DSH list/read/search 改为读取实时目录，Agent 写入使用同一目录内原子替换、预期内容摘要检查，并记录独立历史快照和幂等回执，不触发目录物化。该模式尚未启用；Agent Runtime 容器仍只读挂载，写入由服务端授权工具执行。云端电脑不遵循服务端锁，跨容器的同路径并发仍可能冲突。
- 当前服务端 Host 生命周期允许为电脑创建独立 Workspace；新规则要求唯一 Agent Workspace 与当前电脑强绑定。创建 Host 必须使用该用户 Agent Workspace，其他 Workspace 绑定请求拒绝；切换电脑只迁移绑定，保留同一目录。旧 per-Host 数据来源在 T6 逐用户核对并迁移，不得猜测或覆盖冲突数据。
- 既有“复制电脑”会先复制出独立 Workspace，再创建新 Host，与强绑定规则冲突。该 Portal 入口已移除，兼容 API 改为在任何复制副作用前返回 409；历史的复制 reservation/Workspace 只在 T6 核对处理，不自动删除。

## 接口变化

| 接口 | 变化与约束 |
| --- | --- |
| GET /api/me/agent-workspace（已新增） | Portal 用户会话解析本人稳定 Workspace，返回 ID、逻辑根路径和可用状态；不返回服务端绝对路径，不依赖云端电脑运行。 |
| GET /api/workspaces/{id}/entries?path=&cursor=&limit=（已新增） | 在本人 Workspace 根目录内分页列出实时目录项；拒绝越界路径及符号链接逃逸，空目录与读取失败可区分。 |
| GET /api/workspaces/{id}/files/{path}（已新增） | 为现有文件下载能力提供同一授权边界内的只读文件流；如果复用其他路由，须保证不依赖 Host Cookie 或 Host 状态。 |
| GET /api/workspaces/{id}/manifest?revision=…（现有） | 继续服务不可变 revision / 快照场景；主会话实时文件面板不再调用该接口。 |
| POST /api/hosts（现有） | 客户端不能指定 Workspace；服务端绑定当前用户稳定 Agent Workspace。主机删除只解除绑定并释放计算资源，不删除该 Workspace。 |
| POST /api/hosts/{id}/workspace-bindings（现有、已禁用） | 继续拒绝将其他 Workspace 绑定到 Host；Workspace 与当前电脑强绑定，电脑切换由服务端在生命周期内复用同一 Agent Workspace。 |

新增或调整的 HTTP 契约须同步更新 server/openapi/portal.yaml、docs/实施/00-总览与公共约定.md、服务端实现和对应测试。

## 实施任务

- [x] T1 建立用户稳定 Agent Workspace 与云端电脑强绑定
  - 修改：server/internal/app/workspace.go、host_create.go、host.go、server/internal/store/postgres、server/migrations/00209_stable_agent_workspaces.sql。每位用户唯一映射稳定 Agent Workspace；创建 Host 前幂等创建目录并只绑定此 Workspace；拒绝显式绑定其他 Workspace。Host 切换仅允许旧绑定已解除且旧 Host 已停止、终止或删除的 Agent Workspace 重绑，目录与文件不变；普通 Workspace 仍禁止跨 Host 重绑。删除 Host 只释放计算资源并保留 Agent Workspace；显式删除 Workspace 才删除数据。兼容旧 Host copy API 返回 409 且不触发复制，Portal 复制入口移除。旧 Host 若仍绑定其他 Workspace 会失败关闭，须等 T6 安全迁移。
  - 验收：无云端电脑的用户也有可浏览的空目录；重复初始化和电脑重建仍解析到同一 Workspace 与目录；其他 Workspace 绑定被拒绝；切换 Host 后同一目录保持原文件；无法访问其他用户目录或服务端根路径。
  - 验证：`go test ./internal/app`、Host copy 的 app/API 定向测试通过；临时本机 PostgreSQL 上的 Agent Workspace 与 Host 删除、Host 停止后切换到另一 Host 并保留同一 Workspace 项目目录/来源基线的集成测试通过。Host 创建仍要求 Agent Workspace provider；仅处理既有 Host 的生命周期服务可不注入该 provider。advisory lock 依赖连接池至少 3 个连接。
- [ ] T2（Agent 实时文件工具已改造，待 T6 切流及运行态验收，依赖 T1）让 Agent 在该目录运行
  - 修改：Runtime cwd 固定为 `/workspace`；服务端 DSH 执行容器以只读方式挂载用户 Workspace，受控文件工具在服务端直接读写同一目录。新增 `agent_workspace_live_directory_source_enabled` 开关，默认关闭，且配置要求同时开启 Agent Workspace Host 挂载开关。开启后仅当前稳定 Agent Workspace 的 list/read/search 读取实时目录；写入使用 Linux 安全目录访问、原子替换和文件摘要冲突检查，并保存当前目录快照与耐久回执，不回写覆盖实时目录。其他 Workspace 继续使用 revision；Agent Workspace 尚未转换的 revision 写入会在落库前失败关闭。`workspacecurrent` 原位更新保持根目录 inode，存储包测试和 Linux 交叉编译通过；跨容器直读不经过服务端锁，跨容器同路径并发行为仍需验收。
  - 验收：Agent 新建和修改文件后，服务端目录内容立即变化；目录不可用时返回明确失败，不在其他路径创建空目录。
- [ ] T3（进行中，依赖 T1）让当前云端电脑直接挂载 Agent Workspace
  - 修改：server/internal/app/workspace_volume.go 与 Sandbox 卷适配。Host 路径解析、OpenSandbox `host.path` 序列化、精确 allowlist 与目录预挂载检查已实现；显式及默认 Host 均在落库前检查挂载门禁。Agent Workspace 现在强制挂载到 `/home/gem/workspace`，模板路径误配时拒绝创建；配置要求 `agent_workspace_host_mount_enabled` 与 `agent_workspace_live_directory_source_enabled` 同时开启，避免只有 Host 可写挂载时绕过 revision 数据源。两个开关默认关闭，未验证时失败且不回退到独立卷。容器 owner/UID/GID 或 ACL、可执行配额边界及跨节点共享文件系统尚未验证，开关保持关闭。
  - 验收：电脑与 Agent 对同一文件的创建、修改可互见；电脑构建失败、停止、删除或重建不删除 Workspace；共享挂载缺失或归属校验失败时启动失败且不生成空挂载点。
- [x] T4 提供实时只读文件访问 API
  - 修改：服务端 app/API/文件存储访问层、`server/openapi/portal.yaml` 和 `docs/实施/00-总览与公共约定.md`。新增本人 Workspace 查询、目录分页和只读文件下载接口；Portal Session 授权、根目录约束、符号链接拒绝及路径不外泄已接通。
  - 验证：`go test ./internal/app`、API 定向测试通过；Linux 安全目录测试此前在本地 Linux 容器通过。当前主机为 macOS，本轮未重跑 Linux 专属用例。
  - 验收：空目录返回空列表，存储故障返回错误；跨用户 ID、路径穿越、符号链接逃逸均被拒绝；分页与文件下载只返回授权目录内容。
- [x] T5（本地实现与 PC 验收完成，依赖 T4）把主会话“文件”页接入本人实时 Workspace
  - 修改：`web/portal/src/routes/Assistant.tsx`、`web/portal/src/app/api.ts`、`web/portal/src/api/schema.d.ts` 与 `AgentWorkspaceBrowser`。只在主会话显示本人 Workspace 文件浏览，旁聊仍显示各自附件/任务成果；云端电脑在线状态不参与读取。
  - 验证：主会话目录与文件呈现、旁聊隔离、附件/成果保留定向用例通过；Portal 全量测试为 786/807 通过，21 项既有失败分布于 Assistant、PersonalData、Reviews、AdminOverview；`npm run build` 和相关 eslint 通过。实际 `morde-pc` 本地验收使用隔离测试配置和临时 Portal/数据库：无 Host 记录时打开文件页，接口返回 `~/workspace`、`ready`，页面显示空目录；临时哨兵文件可见，清理后页面恢复为空。验收过程中发现首次创建目录返回 409，修复 revision 0 初始物化逻辑并补回归测试；本地复验接口 200、workspace 已初始化。未向测试机上传代码。
- [ ] T6（实时目录改造进行中；PC 项目上传已有直写实现，旧文件操作/恢复、真实来源核对与切流待完成，依赖 T1–T5）迁移现有数据并切换唯一实时数据源
  - 审计结论：`workspace import-legacy` 只可导入数据库明确登记的 `workspace_legacy_volumes.volume_name`，会保留源卷；compose 的 `workspace-root`、profile/runtime/apps 及其他 per-Host 卷不能仅凭候选名称推断身份。迁移 `00209` 不回填 Agent Workspace 映射，导入也不自动采用旧 Workspace。需逐用户确认 Host 绑定、目录/revision、登记卷和其他候选卷；多源、多个非 detached 绑定、未停止 Host 或归属歧义时阻止切换。允许的唯一例外是由已停止 Host 保留的一条指向所选来源 Workspace、同步已结清且无冲突的绑定。当前目录仍由 manifest 重建，HostPath 写入会造成漂移，后续物化可能覆盖；须先切换唯一实时数据源并停止这类覆盖路径。
  - 验收发现：服务端 worker 集成测试仍有任务、MCP 与 Host release 用例尝试对已绑定 Host 的 Workspace 做 Portal/Host 根目录同步；当前服务端安全门禁拒绝这些请求。按设计应改为直接共享同一目录并移除同步，不应放宽门禁恢复独立副本。
  - 修改：新增 `portal workspace adopt-agent` 离线预检/采用命令。预检显式指定用户和 Workspace，校验 revision、清单摘要、块内容、当前目录、卷登记/导入状态与 Host 绑定；只允许已登记且已导入/核验的旧卷来源。采用要求 Portal 与该用户 Host 停止、来源审计确认及预检 revision/SHA；数据库事务只允许至多一个已停止 Host 保留指向所选来源 Workspace、同步已结清且无冲突的活动绑定，并复查来源所有权、物化 revision、同步/任务空闲后切换唯一映射，同时刷新该绑定 revision。旧卷与原空 Agent Workspace 均保留，不删除。新增 `00210_task_workspace_direct_directory_source.sql` 和默认关闭的 `agent_workspace_live_directory_source_enabled` 开关；DSH 实时文件工具及任务文件/终端产物路径可直接读取/写入当前 Agent Workspace，并生成独立历史快照和幂等回执，不触发当前目录物化；Host 已直接写入共享目录的产物只核验并登记快照，不再复制。获批媒体使用安全流式写入、固定目标路径和任务回执，测试覆盖提交中断后的重试只写入一次。新增 `00211_workspace_sync_project_source_rebind.sql`：同一 Agent Workspace 的项目文件夹可由新 Host 复用，项目来源基线随 Workspace 保留并在新 Host 注册时重绑。实时目录开关开启时，PC 项目上传从共享目录读取冲突基线，经安全目录操作原语直写授权项目路径，在同一数据库提交中写历史 revision 和已应用回执，不排入当前目录物化；direct upload 不再等待 Host 复制应用。应用层测试覆盖新增/修改冲突保护及嵌套目录删除；PostgreSQL 测试覆盖 Host 切换复用项目目录/来源基线，以及直写提交生成 applied 回执且不产生 materialization。`workspacecurrent` Linux dirfd 测试包已在本机非 root Docker LinuxKit 中通过；针对 `openat2` 返回 `ENOSYS` 增加逐段 `openat`/`O_NOFOLLOW` 安全回退，并针对 overlayfs 的 `AT_EMPTY_PATH` 权限修改限制增加持有 FD 的 `/proc/self/fd` 回退。该结果验证文件存储原语，不代表真实 Sandbox 共享挂载或 PC 项目上传已经验收。旧文件编辑/恢复和通用同步接收仍由门禁失败关闭。启动时发现该 Workspace 有未完成接收或 revision 物化也会拒绝切流，避免旧路径覆盖实时目录。真实用户来源的 Docker 卷清点、文件导入/摘要核对和生产切流未执行，因此实时目录源和 Host 挂载开关保持关闭。
  - 验收：已用应用层测试核对上传写入、冲突时保留当前文件、嵌套目录按子项优先删除；本地 PostgreSQL 用例核对项目来源在 Host 停止并解绑、切换后仍使用同一 Workspace folder 和来源基线，并确认直写提交不排 materialization。LinuxKit `workspacecurrent` 测试包通过；真实 PC 项目上传、共享 Sandbox、迁移前后逐用户核对与生产切流尚未验收；旧文件编辑/恢复也未改造，T6 保持未完成。
- [ ] T7（等待 T3、T6）完成端到端验收
  - 修改：补齐本功能所需的服务端集成测试和部署验收记录；按 PC 端约定启动并操作实际 morde-pc。
  - 验收：覆盖无电脑时目录存在、Agent 与当前电脑双向即时可见、切换电脑时同 Workspace 目录与文件随绑定切换、电脑失败重建后数据保留、其他 Workspace 绑定被拒绝、用户隔离、空目录与故障区分、越界访问拒绝，以及共享文件系统不可用时拒绝启动。确认运行时没有服务端与 Host 文件同步。

## 依赖与落地阻塞

执行顺序：T1 → T2/T3/T4 → T5 → T6 → T7。T2、T3、T4 可在 T1 完成后并行实现。

上线前必须确认 Agent 节点与 Sandbox 节点能挂载同一共享文件系统。若目标环境不支持，该环境不满足设计，T3/T6 不得用复制或异步同步替代。

## 本轮验证记录

- 通过：`make generate`；`go test ./internal/app`、`go test -race ./internal/app`、`go test -count=1 ./internal/storage/workspacecurrent`、`go test -race ./internal/storage/workspacecurrent`、Linux 测试二进制交叉编译；Agent Workspace / Host 删除、旧 Host 卷只读盘点的 PostgreSQL 定向集成测试；HostPath、配置与部署配置定向测试。
- 本轮服务端回归：`go test ./...` 全部通过；受影响的 `internal/app` 与 `internal/worker` 定向/全包测试通过。此前 `make test race cover` 的失败记录本轮未复现；本轮未运行 race、cover 或 lint。
- `make lint` 既有记录仍包含 4 条位于 conversation/Hub 相关文件的问题；本功能相关的 lint 问题此前已修正。
- Portal 最新验证：Agent Workspace 相关 Assistant 定向用例通过；全量 Portal 测试 786/807 通过，21 项既有失败分布于 Assistant、PersonalData、Reviews、AdminOverview；`npm run build` 和本轮触及 Portal 文件的 `npx eslint` 已通过。
- T3 边界补充：Agent Workspace 的错误挂载目标测试通过；仅验证模板路径约束，不代表真实 Sandbox 的跨容器共享挂载已经验收。
- T3 双门禁：发现 Host mount 开、live directory source 关时原配置可通过；已改为两个开关必须同时启用，并补齐两个失配方向的配置测试。`go test ./internal/config -run TestLoadWorkspaceFeatureGatesDefaultClosedAndOpenExplicitly -count=1` 通过；真实 Sandbox 读写、停止重建后保留及 UID/GID/ACL 未验。
- T3 测试机只读盘点：当前没有运行中的 Sandbox 容器，OpenSandbox Server 容器也未挂载服务端 Workspace 根目录，无法验证直接共享挂载；未读文件内容、未修改远端数据或部署代码。
- T2/T3 共享目录用例：新增 DSH 工具直接读取实时目录、从 live tree 生成历史快照、Agent 原子写入后 Host 文件内容保持可见，以及同一路径与 Host 内容冲突时不覆盖的测试。本轮 `go test ./...` 通过，Linux `workspacecurrent` 测试包交叉编译通过；当前主机为 macOS，Linux 专属写入测试未实际运行，真实 Sandbox 挂载仍未验收。
- 实时目录边界：工具和 revision 写入门禁按当前用户的稳定 Agent Workspace 映射判定，其他 Workspace 继续使用旧 revision 路径；启动恢复跳过已结清的 Agent Workspace 物化，并在仍有接收或物化任务时失败关闭。`go test ./...` 服务端全包通过；未运行真实云端电脑或共享 Sandbox 挂载验收。
- T6 采用流程：`portal workspace adopt-agent` 参数边界测试、Portal/Store 编译检查通过；一次性本机 PostgreSQL 集成测试覆盖空 Agent Workspace 采用、重复采用、保留停止 Host 来源绑定并刷新绑定 revision、活动/失败 Host、未结清同步、同步冲突、过期 revision、pending/ready 卷及缺少导入审计时拒绝。未运行真实 Docker 卷导入或切流。
- 本轮 T6 任务产物/媒体改造：Host 文件/终端产物只从共享实时目录核验并记入历史快照；获批媒体通过 Linux 安全流式原子写入同一目录，数据库提交中断后的重试复用已写文件。新增 Host 绑定 Agent Workspace 的直接快照提交 PostgreSQL 用例，验证 Host 绑定 revision 与任务检查点推进且不依赖目录物化；定向测试通过。新增路径操作原语的 Linux 定向测试并通过当前平台包测试，Linux 测试二进制交叉编译通过；本机 Docker LinuxKit 对现有 `openat2` 安全读取用例也返回 `ENOSYS`，因此 Linux 专属原语未能在此环境运行。PC 项目上传和旧文件编辑/恢复仍被实时源门禁拒绝，未切流。
- 本轮 T6 项目上传与 Host 切换：`go test ./internal/app -count=1` 通过；临时本机 PostgreSQL 下项目上传回归、非 Agent Workspace 跨 Host 禁止、Agent Workspace 停止并解绑后的 Host 切换/来源基线复用、直写提交回执及不排目录物化用例通过。应用层文件系统测试使用受控临时目录替身；本机 macOS 和 Docker LinuxKit 均不能运行 Linux `openat2` 写入实现，真实 Linux 项目文件上传与 PC 端验收未完成。旧文件编辑/恢复、逐用户数据迁移核对和切流未完成。
- 本轮 Linux 路径操作复核：修正符号链接写入的模式校验和符号链接父路径错误映射，并修正越界测试样例。补充低权限旧目录重命名前临时提升所有者访问位、回滚时恢复原权限；实现已通过编译，但当前 Docker LinuxKit v7.0.12 在 `seccomp=unconfined` 与 privileged 容器中仍对 `openat2` 返回 `ENOSYS`，Linux 专属用例无法运行。macOS `go test ./internal/storage/workspacecurrent`、服务端 app 全包、PostgreSQL 项目上传/Host 切换定向用例、`go vet` 与 `git diff --check` 通过；Linux 测试二进制交叉编译通过。低权限目录重命名修复仍待真实 Linux 验证。
- 本轮 LinuxKit 后续复测：交叉编译 `workspacecurrent` Linux amd64 测试二进制后，在本机 `morde-cloud-runtime:local` 容器以非 root 用户运行，整包测试 `PASS`。测试覆盖 `openat2` 返回 `ENOSYS` 时逐段 `openat`/`O_NOFOLLOW` 回退，以及 overlayfs 不支持 `AT_EMPTY_PATH` 时的目录权限回退。该结果更新此前“Linux 专属用例无法运行”的记录；真实 Sandbox 共享挂载仍未验收。
- 本轮 T1/T5 本地验收与回归修复：实际启动源码版 `morde-pc`，连接隔离 Portal 和一次性本机 PostgreSQL 数据库；数据库确认 `hosts=0`。打开主会话“文件”页后，`GET /api/me/agent-workspace` 返回 `200`，创建并初始化唯一 Agent Workspace，UI 显示路径 `~/workspace` 与空目录；加入临时哨兵文件后 UI 可见，清理文件后 UI 恢复为空。初次运行暴露 revision 0 尚无目录时被误判为漂移并返回 409；修正 `materializeWorkspaceCurrentForReceive`，只在无现存 revision 0 目录时直接创建初始目录，现存目录继续验证；新增 `TestInitialCurrentMaterializationCreatesMissingWorkspaceDirectory`，定向 app 测试通过。所有测试数据与服务均为本地临时资源。
