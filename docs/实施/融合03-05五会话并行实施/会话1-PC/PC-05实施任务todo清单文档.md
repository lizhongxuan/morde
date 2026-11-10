# PC-05：PC Hub 和受控设备动作实施 TODO

> 开发量：按一个 AI 会话、前置就绪后的连续开发估算，最多一天；只计实现与交接记录。测试编写、运行和验收单独进行，不计入这一天。
> 范围估算不是完成承诺；发现超出一天时把剩余实现按文件/接口继续合并拆开，保持原验收条件。
> 当前状态：部分完成。PC-05.01 本地实现与验证已完成；PC-05.02 的 Linux 私有根、systemd/cgroup 运行、超时/取消及后代清理已在本地 Linux 容器验证，命令只在 Linux 隔离探测通过且用户显式授权后开放；macOS 已证实 detached 后代不能可靠回收，Windows 缺原生运行验收，两端命令保持关闭，支持平台范围待确认。PC-05.03 受阻于可信 Workspace 映射桥、Task 内容下行合同和同步回执查询；`file.put` 继续关闭。PC-05.04 已把 Morde 桌面目标的 HTTP(S) 打开及通知接入新 Hub，其他旧 Cloud 入口保留。
> 直接前置：可先做 PC 端独立动作；动作授权依赖 HUB-01/HUB-02，Workspace 写入依赖 PC-02、HUB-03 与 F02-T10。
> 修改边界：桌面 Hub 客户端、目标端受控动作及旧 Cloud 清理；云端 Broker/Hub 服务归会话 4。
> 写入规则：[五会话执行入口](../并行执行入口.md)；只修改本会话的指定文件与本会话文档。公共合同/路由/迁移变更在本文件交接给会话 5。
> 本地约束：只在本地修改和验证，不发送或上传文件，不向远端部署。

## 本次开发

- [x] **PC-05.01（本地实现与验证完成，联调待验收）接入设备连接和基础动作**：使用安装身份、设备列表和双端授权，处理断线、撤权和未知结果；不委派本机 Agent。
- [ ] **PC-05.02（进行中：Linux 命令隔离与运行验收通过；macOS/Windows 命令保持关闭）实现 PC 受控动作**：分别实现获准截图、文件查看与各支持平台的命令边界，不能用命令黑名单替代系统隔离。
- [ ] **PC-05.03（依赖未就绪：缺可信 Workspace 映射桥、Task 内容下行合同和同步回执查询）接入 Workspace 受控写入**：限定已选目录、身份和相对路径，复用安全副本及操作回执；同步未确认不显示完成；`file.put` 保持关闭。
- [ ] **PC-05.04（进行中：Morde 桌面 open/notify 已迁入）清理已迁旧 Cloud 调用**：逐项切换已有且合同兼容的 PC 能力，未就绪入口不提前移除。

## 本轮实现与验证记录

- **PC-05.01**：桌面端 Hub 状态增加安装设备清单；按服务端 `/api/devices` 的 `items` 响应校验名称、平台、在线、撤销和过期字段。设备撤销禁止撤销当前安装；网络结果未知时重新读取服务端清单，仍无法确认则返回未知状态；Hub 设备变更会刷新清单，凭据失效会停止连接并清除本地安装凭据。UI 提供刷新、状态显示和撤销确认。
- **PC-05.02**：现有 Task 授权围栏继续覆盖截图、文件列表和文件查看。Linux 命令新增私有 user/mount namespace 与 `pivot_root`，只暴露选定 Workspace、只读 runtime、私有临时目录和 `/dev/null`，随后继续应用 Landlock/seccomp；因此绝对宿主路径的 `stat/lstat/readlink` 不再泄露是否存在。morde-sync `hub_command_probe` 通过后才在 Linux 广告命令能力；macOS/Windows helper 明确返回不可用，目标授权默认关闭。Linux 运行验收通过。macOS Seatbelt 的既有本机测试虽能拒绝未授权文件/网络访问，但 `setsid` 脱离后代在进程组终止及 launchd Job 回收后仍可存活；Windows 也没有对应运行隔离环境验收。平台支持范围未确认，PC-05.02 整体仍进行中。
- **PC-05.03**：暂不接入写入。服务端已有 `file.put` 元数据验证和 Task 操作/本机回执/同步阶段持久结构，但生产 Hub action allowlist、DSH 工具和客户端能力仍关闭。Desktop 主进程按登录 profile 保存 `workspace_id → directory/dev/ino` mapping；DSH 的 `fileRoot` 则由另一套设置独立选择，目前没有 Hub IPC/主进程桥将二者可信绑定。morde-sync stdio 没有写入或按 operation ID 查询本机回执入口；客户端和服务端 Task-linked sync 都只允许单文件上传，不允许 Task 内容下行；普通 Workspace 下载不绑定 Task/operation/grant，也没有可验证的 Task 云端同步回执。没有添加接受调用方自报 Workspace 身份的 writer API；须先补齐由 Desktop 主进程核验 profile/mapping/grant 的桥接、F02 内容下行合同及回执查询调用，不能把本机回执冒充云端同步完成。
- **PC-05.04**：`reach.ts` 的设备列表分别标识 nanoMuse Cloud 与 Morde Hub 来源；Morde Hub 列表来自当前登录账号，显示活跃安装；PC `open/notify` 只接受在线的桌面 Installation 精确 ID。`device_open` 先检查无凭据 HTTP(S) URL，保留来源侧 approval，再通过 Morde Hub 调用；目标端原有一次性授权保留。`device_notify` 使用 Morde Hub 桌面通知回执，只有 `notified === true` 才报告成功。Hub 调用取消/超时会发送 `cancel`；目标端取消后撤下等待中的 open approval。`screen/files/shell` 是 Task-only action，不能改为普通 Morde caller；`delegate` 调用目标设备本机 Agent，不迁移。nanoMuse Cloud 的旧 URL、远端路径和通知仍可用，不能整体删除旧入口。
- **本地修改范围**：DSH Hub 状态、受控目标动作及命令能力门禁位于 `project/nanoMuse/harness/dsh-nanomuse/src/cloud.ts`、`src/hub.ts`、`src/morde-hub-actions.ts`、`src/reach.ts` 及对应测试；先前设备管理改动位于 `src/client/api.ts`、`src/client/locales.ts`、`src/client/MordeHubCard.tsx`；桌面 Workspace 同步结果校验位于 `project/nanoMuse/harness/desktop/src/morde-main.ts`、`morde-sync.ts` 及对应测试；Linux 命令隔离位于 `project/morde-sync/client/hub_command_linux.go` 及对应测试。未修改服务端或其他会话文件。
- **验证证据**：DSH `npm run build`、`npm run typecheck`、`npm test`（409/409）；Desktop `npm run typecheck`、`npm test`（234/234）；`project/morde-sync` 的 Linux arm64 与 macOS `go test ./...`、Windows 目标构建；Server `go test ./internal/api ./internal/app`；根目录 `git diff --check`。均为本地验证，未连接测试服务器。
- **2026-11-02 PC-05.02 Linux 追加验收**：`project/morde-sync/client` 的 `TestExecHubCommandLandlockedSubprocess` 在 Ubuntu 26.04 arm64 容器通过；随后以 Go 1.27.1、Ubuntu/Debian arm64 容器、源码和 module cache 只读、网络关闭运行 `go test ./...`，三个 Go 包全部通过。另在本地 arm64 systemd 用户管理器/cgroup 容器中运行 `morde-sync` 命令 helper 集成用例：正常写入/读取、外部路径拒绝、脱离 session 的后代回收、超时与取消均通过。DSH `npm run build`、`npm run typecheck`、`npm test` 为 **409/409**；新增用例验证 Linux sandbox probe 通过后才允许显式开启命令 grant。Mac/Windows 命令未启用，未访问测试服务器、未使用所给账号/API key，未上传或部署。
- **2026-11-02 PC-05.02 非 Linux runner 复核**：生产命令入口 `project/morde-sync/cmd/morde-sync/hub_command_other.go` 对非 Linux 的 probe/run 均失败关闭，DSH 只在 probe 通过时广告命令能力。macOS `sandbox-exec` 已弃用；既有 macOS 27.0.1 本机隔离试验证明虽能限制文件/网络访问，`setsid` detached 后代仍可在父进程组终止、launchd Job 回收及 bootout 后存活，不满足超时/取消清理边界。Windows 无原生运行隔离环境验收，交叉编译不能证明运行时安全。未改平台 runner；`go test ./cmd/morde-sync` 在本机 macOS 通过（缓存结果）。两端保持关闭；PC-05.02 继续未完成。
- **2026-11-02 PC-05.04 基础调用迁移**：Morde Hub reach 设备单独标记来源；只对已认证设备清单中的活跃桌面 Installation 提供 `open/notify`。打开 URL 限 HTTP(S)、无 userinfo/首尾空白/超长，并保留来源一次性 approval 与目标本机 approval；通知只在目标返回 `notified:true` 时报告成功。超时/取消通过 Hub cancel 帧通知目标，等待中的 open approval 会随取消移除。legacy Cloud 路径打开和通知保持原行为；`screen/files/shell/delegate` 未迁入。主 agent 集成验证：DSH `npm run build`、`npm run typecheck` 通过；`npm test -- --test-reporter=dot` 通过 **418/418**；相关源文件 `git diff --check` 无输出。未连接真实设备、未访问测试服务器、未上传或部署。

## 原任务与已有进度

下列编号用于追溯完整业务规则和拆分前证据；同一原编号涉及多个会话时，各会话只完成自身范围。原项全部实现及验收通过后才可视为完成。

| 原编号 | 原任务 | 拆分前状态 |
|---|---|---|
| [F04-T04.03d](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 接入云端 Task 的设备调用授权 | 未开始，依赖 F04-T04.03a |
| [F04-T04.03e](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 接入受控 PC 截图 | 未开始，依赖 F04-T04.03d、F04-T04.04a |
| [F04-T04.03f](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 接入受控 PC 文件查看 | 未开始，依赖 F04-T04.03d、F04-T04.04a |
| [F04-T04.03g](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 接入受控 PC 命令 | 未开始，汇总，依赖 F04-T04.03h、F04-T04.03i、F04-T04.03j、F04-T04.03k |
| [F04-T04.03h](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 实现受控命令的共用合同 | 未开始，依赖 F04-T04.03d、F04-T04.04a |
| [F04-T04.03i](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 验证 macOS 命令执行边界 | 未开始，依赖 F04-T04.03h |
| [F04-T04.03j](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 验证 Windows 命令执行边界 | 未开始，依赖 F04-T04.03h |
| [F04-T04.03k](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 验证 Linux 命令执行边界 | 未开始，依赖 F04-T04.03h |
| [F04-T04.04a](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 接入 PC Hub 连接与设备列表 | 未开始，依赖 F04-T04.02 |
| [F04-T04.04b](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 接入 PC 基础设备调用 | 未开始，依赖 F04-T04.03b、F04-T04.04a |
| [F04-T04.04c](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 核对 PC 受控 action 接入 | 未开始，汇总，依赖 F04-T04.03e、F04-T04.03f、F04-T04.03g、F04-T04.04a |
| [F04-T04.06a](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 确定受控文件写入合同 | 未开始，依赖 F04-T04.03d |
| [F04-T04.06b](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 验证本机路径与覆盖保全 | 未开始，依赖 F03-T06.01、F03-T06.04 |
| [F04-T04.06c](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 处理写入回执与未知结果 | 未开始，依赖 F04-T04.06a、F04-T04.06b |
| [F04-T04.06d](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 接入真实 Workspace 文件写入 | 未开始，依赖 F04-T04.06g、F03-T06.06、F02-T10 |
| [F04-T04.06e](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 核验设备写入与 Task 文件围栏 | 未开始，依赖 F04-T04.06d、F02-T10 |
| [F04-T04.06f](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 将 PC 操作接入 Workspace 围栏 | 未开始，依赖 F04-T04.06a |
| [F04-T04.06g](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 拆分本机结算与云端同步 | 未开始，依赖 F04-T04.06c、F04-T04.06f |
| [F04-T05.07a](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | PC 接入设备与连接设置 | 未开始，依赖 F04-T04.04a |
| [F04-T10.01](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 清理 PC 旧 Cloud 调用 | 未开始，依赖 F04-T05.01、F04-T05.03 |

## 交接

- 在本文件记录完成的接口、文件和待会话 5 处理的公共改动；不要直接改其他会话负责的文件。
- 对应验证在 [独立验收清单](../验收资料/验收入口.md) 执行并记录，不计入本开发文档的一天预算。
- `server/AGENTS.md` 要求接口、实现和测试在同一变更交付；可先完成代码并标“待验证”，合入/提交前再按独立验收清单补齐测试与所需检查。
