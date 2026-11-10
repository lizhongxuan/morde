# HUB-02：云端 Task 设备动作实施 TODO

> 开发量：按一个 AI 会话、前置就绪后的连续开发估算，最多一天；只计实现与交接记录。测试编写、运行和验收单独进行，不计入这一天。
> 范围估算不是完成承诺；发现超出一天时把剩余实现按文件/接口继续合并拆开，保持原验收条件。
> 当前状态：服务端 Task-Hub 调度、授权围栏、动作合同、Host 外操作账本和启动装配已实现。桌面端复用独立 Morde Client Installation 登录（用户确认沿用现有登录流程），截图与所选目录文件读取由目标授权版本围栏保护。Linux 命令 runner 在本机 Lima VM 的 systemd 用户服务中完成正常、超时、取消及脱离后代集成测试；Workspace 扫描也拒绝已有硬链接和嵌套挂载点，但 `stat/access` 路径元数据仍可读取，macOS/Windows 未验收，因此按用户选择继续关闭 Hub 命令能力，HUB-02.02 命令项待验。dsh 全量测试 404/404、desktop 232/232、morde-sync 全包及 Hub 域 race 检查通过；Task 账本用本地临时 PostgreSQL 实测通过。服务端全量测试可编译运行，当前仍有 Workspace current 安全路径和 Docker Workspace Sync Node 测试失败，详见验证记录。
> 直接前置：依赖 HUB-01 的授权/投递；截图、文件查看、命令按能力分别接入。
> 修改边界：服务端 cloud dsh Broker、Hub 专属动作和 dsh 适配；PC 目标端由 PC-05 实现。
> 写入规则：[五会话执行入口](../并行执行入口.md)；只修改本会话的指定文件与本会话文档。公共合同/路由/迁移变更在本文件交接给会话 5。
> 本地约束：只在本地修改和验证，不发送或上传文件，不向远端部署。

## 本次开发

- [x] **HUB-02.01 绑定云端来源和目标**：服务端按账号查询同账号的在线安装，以目标专属工具名固定 installation 和 grant version，不提供模型可填写的目标 ID；派发前校验运行中的 server Task、Task 授权/租约、账号归属、安装状态与目标声明的 action/grant version。调用帧携带 Task ID、授权版本、目标授权版本和唯一操作 ID；桌面目标端再按本地授权与能力检查截图、文件动作。
- [ ] **HUB-02.02 接入受控动作合同**：协议层分别校验截图、文件列表/读取和命令输入/结果。动作工具只在目标 `info` 声明 action 且带正数 grant version 时公开。Host 外账本记录 `prepared/dispatched/succeeded/failed/unknown` 与请求/结果 SHA-256，不保存截图或文件正文；撤权竞争、目标断线、超时、错配与恢复均按未知结果处理，不盲目重发。
  - [x] 截图和文件列表/读取：分别授权，目标仅接受带当前 Task 围栏的调用；文件 helper 使用所选目录身份及相对路径，跳过符号链接，并对每次调用重新验证目录身份。
  - [ ] 命令：服务端协议校验和 Linux 沙箱候选实现已完成；DSH 端在平台验收完成前固定不宣告 `command`、拒绝命令授权。必须完成对应平台的授权、文件保护、网络隔离、超时/撤权与后代停止验收后再开放。

### 命令平台隔离审计（2026-11-02，本地只读）

- macOS：本机 Seatbelt 试验确认 profile 可拒绝文件与网络访问，子进程继承限制；但 `sandbox-exec` / `sandbox_init` 已标记 deprecated，尚未验证产品所需的所选目录最小权限 profile。对进程组发 `SIGKILL` 后，调用 `setsid` 的脱离后代仍存活；在 macOS 27.0.1 本机又验证 launchd 用户 Job：子进程确认 `setsid()` 成功，父 Job 退出后仍存活，`launchctl bootout` 后依然存活，探针随后已清理该子进程。现有 DSH `killTree()` 和 launchd Job 生命周期都不能满足停止验收。F04-T04.03i 待验。
- Windows：现有 ACL runner 仅限制部分写入/删除，读取、网络和进程可见性未隔离；Job Object 停止行为没有本项目的实机证据，且该 runner 未接入 Hub 命令。本机未发现 Windows、Wine、虚拟化或本地 CI runner 环境，不能以跨编译/模拟替代 AppContainer 与 Job Object 实机验收。F04-T04.03j 未通过。
- Linux：`morde-sync` 使用 Landlock ABI 8–10 + 同步 seccomp 候选隔离；限制文件读写/执行、网络、进程外控、namespace/mount、ioctl 和 inode 元数据修改，并把请求目录身份绑定到最终打开的 O_PATH FD。euid 0、未知 Landlock ABI、启动条件不满足均 fail closed。执行前递归扫描 Workspace；拒绝已有硬链接、特殊文件、嵌套挂载点、超过 200,000 项或超过 256 层的目录树。命令 runner 由 systemd 用户 transient service 承载，配置 `KillMode=control-group` 与资源上限。本机 Lima Ubuntu 26.04 aarch64/kernel 7.0/systemd 用户服务中，整条 runner 测试通过：工作区内读写、工作区外读写拒绝、`/proc` 读取拒绝、正常退出/超时/取消时清理 `setsid` 后代。ARM64 helper 子进程测试覆盖目录替换身份拒绝、HOME/宿主内容和网络拒绝、策略继承、metadata/ioctl 拒绝；硬链接拒绝、`RESOLVE_NO_XDEV` 跨挂载拒绝和 root 执行拒绝也通过。Linux amd64/arm64 构建与测试二进制交叉编译通过。Landlock 仍允许 `stat/access` 读取路径元数据；因此 F04-T04.03k 继续待验，且 DSH capability 保持关闭。
- 结论：Hub 命令能力在本地显式关闭；Linux runner 集成测试不能代替 macOS、Windows 的实机验收。`stat/access` 仍能泄露外部路径元数据，Linux 工作区扫描有目录项数量和深度限制。尝试用 Lima 非 root systemd 用户服务提供独立根视图时，`TemporaryFileSystem=/tmp` 返回 `226/NAMESPACE`；rootless `unshare --user --map-root-user --mount` 因 uid map 被拒绝；`InaccessiblePaths=/etc` 未隐藏路径。因此当前本机不能用非特权 mount namespace 收敛该风险。macOS/Windows 验收也未完成，须在各平台边界收敛后再开放。

## 原任务与已有进度

下列编号用于追溯完整业务规则和拆分前证据；同一原编号涉及多个会话时，各会话只完成自身范围。原项全部实现及验收通过后才可视为完成。

| 原编号 | 原任务 | 拆分前状态 |
|---|---|---|
| [F04-T04.03c](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 落实 PC 受控设备调用 | 未开始，汇总，依赖 F04-T04.03d、F04-T04.03e、F04-T04.03f、F04-T04.03g |
| [F04-T04.03d](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 接入云端 Task 的设备调用授权 | 未开始，依赖 F04-T04.03a |
| [F04-T04.03e](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 接入受控 PC 截图 | 未开始，依赖 F04-T04.03d、F04-T04.04a |
| [F04-T04.03f](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 接入受控 PC 文件查看 | 未开始，依赖 F04-T04.03d、F04-T04.04a |
| [F04-T04.03g](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 接入受控 PC 命令 | 未开始，汇总，依赖 F04-T04.03h、F04-T04.03i、F04-T04.03j、F04-T04.03k |
| [F04-T04.03h](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 实现受控命令的共用合同 | 未开始，依赖 F04-T04.03d、F04-T04.04a |
| [F04-T04.03i](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 验证 macOS 命令执行边界 | 未开始，依赖 F04-T04.03h |
| [F04-T04.03j](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 验证 Windows 命令执行边界 | 未开始，依赖 F04-T04.03h |
| [F04-T04.03k](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 验证 Linux 命令执行边界 | 未开始，依赖 F04-T04.03h |

## 交接

- **dsh 凭据现状**：`project/nanoMuse/harness/dsh-nanomuse/src/cloud.ts` 已通过 Morde `auth/client-login` 建立 Client Installation，凭据保存在 `MORDE_CLIENT_CREDENTIAL`，Hub WebSocket 使用独立 Bearer 凭据；nanoMuse Cloud token 单独保存。`tests/cloud-morde-hub.test.mjs` 覆盖两者隔离、安装撤销及 Morde `open` 不继承旧 `trusted` 授权。本次不改登录流程，也不读取本地凭据。
- **会话 5 集成结果**：新增 `server/migrations/00092_task_hub_operations.sql` 与 `server/internal/store/postgres/hub_task_operations.go`，并将现有 `HubService` 注入 Cloud DSH worker 和 Broker。账本使用已有 `(id, owner_id)` 约束及 `client_installations(id, owner_id)` 约束；无需公共 HTTP 路由或 OpenAPI 变更。`TestPostgresHubTaskOperation` 在本机临时 PostgreSQL 上通过，测试容器只绑定回环地址，运行后已停止。
- **dsh 调用上下文**：Morde Hub 来电的 `from.kind === "task"` 且 Task ID、Task 授权版本、目标 grant version、operation ID 全部有效时，解析成 `Caller.taskContext`；普通 Caller 不变，坏字段不会生成部分 Task 上下文。现有 `auth/client-login` / `MORDE_CLIENT_CREDENTIAL` 登录保持独立于 nanoMuse Cloud token，Hub WebSocket 继续使用其 Bearer 凭据。
- **文件归属与未完成项**：本会话实现服务端 `cloud_dsh_tool_broker.go`、`hub*`、`domain/hub/**`、dsh 目标动作及对应测试。会话 5 实现迁移/Postgres adapter/`cmd/portal` 装配。桌面 UI、原生目录选择、Host helper 路径注入和 `morde-sync` 本地文件 helper 已接通；旧 `remoteControl/trusted` 动作不复用。Linux 命令候选实现已接线到 Hub 目标，但 capability 固定关闭，直至单独平台验收通过；macOS/Windows 命令尚未接入。Workspace 写入不在本次范围。
- **本地验证**：本机 macOS 上 `project/nanoMuse/harness/dsh-nanomuse` 的 build、typecheck 与全量 Node 测试通过（404/404）；桌面端 build、typecheck 与测试通过（232/232）；`project/morde-sync` 的 `go test ./... -count=1` 通过（本次在 macOS 上运行，Linux 文件受 build tag 排除）；服务端 `go test -race ./internal/domain/hub -count=1` 通过。Linux ARM64 Lima VM runner 集成测试（正常结束、超时、取消及脱离后代清理）通过；helper 子进程测试覆盖硬链接、挂载点、root、文件与网络边界；Linux ARM64/AMD64 与 Windows AMD64 产品/测试交叉编译通过。Task 账本本机临时 PostgreSQL 集成测试先前通过。服务端取消模型调用用例改为等模型调用开始后再取消，`go test ./internal/worker -run '^TestCloudDSHExecutorCancellationDuringModelCall$' -count=20` 通过。最新 `go test ./... -count=1` 可编译运行，只有以下包失败：`internal/storage/workspacecurrent` 的 2 个删除测试因 Workspace current 根路径安全校验失败；`internal/worker` 的 9 个 Docker Workspace Sync Node 测试因 quota socket owner 目录权限不可用。其余包（包括 `internal/app`、`internal/gateway`、Postgres 与 Hub）通过；这些失败不属于本会话 HUB-02 文件，本次未修改。真实 DSH Host 页面验收仍未能启动：本机没有 DSH Host 进程/命令或浏览器页，Morde 桌面进程当前无打开窗口；卡片只有 404 项组件/Node 测试。未在 Windows 实机验证，也未做真实桌面截图权限交互验收。Linux `stat/access` 路径元数据读取仍须处理。
- 在本文件记录完成的接口、文件和待会话 5 处理的公共改动；不要直接改其他会话负责的文件。
- 对应验证在 [独立验收清单](../验收资料/验收入口.md) 执行并记录，不计入本开发文档的一天预算。
- `server/AGENTS.md` 要求接口、实现和测试在同一变更交付；可先完成代码并标“待验证”，合入/提交前再按独立验收清单补齐测试与所需检查。
