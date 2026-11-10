# Syncthing 用于 Workspace 同步的可行性调研报告

> 2026-10-28。依据当前本地源码调研；已按用户补充的 Workspace 目标修订。Syncthing 源码位于 `/Users/zhongxuan_li/Desktop/project/syncthing`，检查时为 `v2.1.7-4-ged38528d7`。尚未实施 Morde 集成；后续独立双机试验与选型以[Morde Workspace 文件夹同步设计方案](Morde%20Workspace%20文件夹同步设计方案.md)为准。

> **后续结论更新：** 用户随后明确单文件 300 MiB、每 Workspace 5 GiB 的硬限制。Mac 与 Ubuntu 隔离试验发现原版 Syncthing 会索引超限文件，动态 `.stignore` 会留下远端旧副本且双方仍显示 100%。因此优先方向是完整 Syncthing 节点加必要的准入扩展，原版不能直接上线；具体边界以 [Morde Workspace 文件夹同步设计方案](Morde%20Workspace%20文件夹同步设计方案.md)为准。

## 1. 结论

用户明确的 Workspace 目标是：**文件同步独立于 Agent**；用户可在本机使用任意 AI 工具读写选定目录，PC 客户端把文件持续同步到 Morde 服务端，云端电脑可基于这些文件工作。Workspace 应体现为 PC 与云端电脑共享的同步目录。这里要求同步文件，并不要求迁移本机 AI 会话或让某一种 Agent 接管同步。

**按这个目标，优先验证完整 Syncthing 子进程方案是合理的。** PC、常驻服务端和云端 Host 各有一个 Syncthing 节点；服务端为 Workspace 保留持久目录，Host 在自己的工作目录接收同一 folder。Syncthing 直接负责扫描、传输、重试和冲突副本；Morde 管账号、Workspace/Host 关联、设备准入、状态与必要的交接确认。这样无需把每次文件变化转换成现有 `sync-session/commit`。若部署已证明 Host 能安全挂载同一持久目录，可省去 Host 节点，但当前 OpenSandbox/PVC 结构不能先假定共享挂载可用。

代价是必须把现有 Workspace 的**文件权威**从“数据库 revision + 不可变块”改为“服务端持久同步目录 + Syncthing 索引”，并迁移依赖旧 revision 的 Host 绑定、任务文件证据和相关 API。这在产品概念上是简化，在现有代码中不是小改动。建议先做一 Workspace、一个 PC、一个 Host 的验证，再决定正式迁移；若限额扩展或集成验证不通过，现有 `morde-sync` 可作为备选，但同步效果仍需同条件基准。**不建议移植 Syncthing 核心包：运行其完整节点比拆出同步引擎更直接。**

## 2. 当前架构与代码事实

| 项目 | 本地证据 | 对选型的影响 |
|---|---|---|
| 现行融合稿 | [融合技术架构第 3.2、5.3～5.6 节](nanoMuse客户端与Morde服务端融合技术架构设计方案.md)要求 Workspace 沿用 manifest/revision/chunk/commit；[较早的多端需求](Morde多端托管与共享工作区需求文档.md)明确本机开发、云端接力与双向目录同步 | 新澄清以文件连续性为目标；需要重新审阅 Workspace 实现选择。同步不改变云端 Agent 运行位置的现有决策，本机其他工具可读写用户目录 |
| 服务端存储 | [迁移 00023](../server/migrations/00023_workspace_sync.sql)保存 `workspace_revisions`、文件清单与变更；[workspacefs](../server/internal/storage/workspacefs/store.go)按 Workspace 保存不可变块 | 服务端没有可供 Syncthing 直接复制并自动成为权威数据的普通 Workspace 文件树 |
| 提交边界 | [WorkspaceSyncService](../server/internal/app/workspace_sync.go)负责鉴权、块校验和会话；[事务提交](../server/internal/store/postgres/workspace_sync.go)检查块/文件应用回执、Host binding、配额、并发冲突和未结算 Task operation，再推进 revision | 目录级复制不能绕过这一事务；否则会破坏文件保存与 Task 输入版本屏障 |
| 现有客户端 | [morde-sync](../project/morde-sync/)已有 4 MiB 固定块、SHA-256、断点续传、扫描/原子应用、`fill_missing`/`compare_merge`/`live`、冲突保留与交接检查点；[桌面主进程封装](../project/nanoMuse/harness/desktop/src/morde-sync.ts)通过 stdio JSON-RPC 调用，且有[构建脚本](../project/nanoMuse/harness/desktop/scripts/build-morde-sync.mjs) | 文件夹同步的主要能力已经存在；融合实施主要是迁入唯一源码、接通登录/界面/打包并验收 |
| 现有凭证 | [WorkspaceSyncService.authorize](../server/internal/app/workspace_sync.go)区分 `KindDevice` 和 `KindHostReplica`；[API 白名单](../server/internal/api/api.go)限制同步设备凭证 | Syncthing 设备证书不能自然继承 Morde 的账号、安装、Workspace scope 与撤权 |

旧的 [workspace 同步草图](workspace同步方案.md)画出了 PC 与服务端两个节点；按已确认目标，还需补云端 Host 如何读写同一目录、Host 离线时的持久副本、设备撤销和交接就绪判断。

## 3. Syncthing 源码调研

### 3.1 可以直接使用的功能

Syncthing 的正式进程具备完整目录同步栈：目录扫描与哈希、设备间块交换、数据库索引、文件监听、冲突副本和发送/接收目录模式。其 [REST 路由](../../project/syncthing/lib/api/api.go)提供设备及文件夹配置、`/rest/events`、`/rest/db/status`、`/rest/db/completion`、`/rest/db/scan` 等入口；[命令行](../../project/syncthing/cmd/syncthing/main.go)可指定独立的 config/data 目录，并关闭浏览器和自动升级。因此，用独立子进程加本地 REST 接口封装桌面生命周期是可行的。

需要把它当成**完整的 Syncthing 节点**：每端持有自己的设备证书和索引数据库，节点之间说 Syncthing 的协议。Morde 当前的 `/api/workspaces/*/sync-sessions` 是 HTTP 会话、块回执和 commit 协议；Syncthing 没有直接消费这组接口的现成适配器。

### 3.2 为什么不能直接指向当前 Morde Workspace

1. **存储形态不同。** Syncthing 的文件夹配置指向文件系统路径；当前 Morde 权威状态在数据库 manifest 与不可变块中。若保留旧存储，服务端目录需要提交桥；若按新目标改为持久目录，Syncthing 可以直接同步该目录，旧文件存储与依赖它的代码必须迁移。
2. **事务语义不同。** Syncthing 的“文件已同步/目录已空闲”不是当前 Morde 的 `commit` 回执。[Morde 提交事务](../server/internal/store/postgres/workspace_sync.go)还保护 Task 输入 revision、配额与并发变更。改用 Syncthing 后，这些使用旧 revision 的业务应改为基于实际目录就绪状态或明确的文件检查点，不能继续读取失效的版本字段。
3. **数据模型不同。** [当前 Morde manifest](../server/internal/domain/workspace/sync.go)只表示普通文件及相对路径、大小、模式、整文件/分块 SHA-256；[morde-sync 扫描器](../project/morde-sync/sync.go)对符号链接和特殊文件报错。Syncthing 会处理目录、符号链接及自身冲突副本；其[块大小](../../project/syncthing/lib/protocol/protocol.go)随文件在 128 KiB～16 MiB 范围选取。若不再转换为旧 manifest，这些差异变成文件策略与迁移问题，而非传输障碍。
4. **授权模型不同。** Syncthing 的设备证书和文件夹共享配置是另一套身份；Morde 的 Workspace owner、设备 scope、Host binding、撤权和配额仍需服务端独立检查。Syncthing 的 REST API key 管理整台节点，不适合直接给 renderer 或普通用户调用。
5. **状态证据不同。** [Syncthing Events](../../project/syncthing/lib/events/events.go)使用有限长度的内存缓冲，适合 UI 更新，不适合当持久交接记录。云端电脑能否开始读文件，应复查目标 folder 的当前状态、缺失项与扫描错误；仅显示 100% 进度不足以证明某次本机保存已到达。若任务要求固定输入，额外记录该次文件检查点。

Syncthing 提供 [`fs.RegisterFilesystemType`](../../project/syncthing/lib/fs/types.go)，理论上能把自定义文件系统接进其模型；但其 [`fs.Filesystem`](../../project/syncthing/lib/fs/filesystem.go)要实现打开、写入、重命名、删除、目录、监听、权限、链接、扩展属性等完整行为。Morde 的 revision 是整个提交事务，而非每个文件系统调用。这条路仍需要暂存与事务桥，属于较大规模的 Syncthing 内嵌开发。

### 3.3 体量与复用边界

本地 `wc -l` 粗计：Syncthing 全仓约 83,031 行非测试 Go，`lib/model` 约 15,163 行，核心相关包 `lib/model`、`scanner`、`fs`、`protocol`、`internal/db`、`syncthing`、`api`、`config` 合计约 43,635 行；现有 `morde-sync` 约 5,395 行非测试 Go。[`lib/syncthing.App`](../../project/syncthing/lib/syncthing/syncthing.go)同时装配索引库、Model、连接、发现、REST 和 TLS；[`Internals`](../../project/syncthing/lib/syncthing/internals.go)也标明接口不稳定。将其嵌入 Morde server 或拆出 `lib/model`，不是轻量复用。

| 可考虑的源码 | 结论 |
|---|---|
| [`lib/scanner/blocks.go`](../../project/syncthing/lib/scanner/blocks.go) | 哈希函数依赖 Syncthing `protocol.BlockInfo`，块大小语义不同；Morde 已有对应扫描/哈希实现，本期无移植收益 |
| `lib/fs`、`lib/watchaggregator`、`lib/ignore` | 功能完整但依赖 Syncthing 的文件系统、配置与事件类型；现有 `morde-sync` 已用 `fsnotify` 加定期重扫，且由 Morde scope/路径规则约束输入；只在出现明确缺口时单独评估 |
| `lib/model`、BEP 与内部索引库 | 冲突、版本向量、拉取和设备通信彼此耦合；拆出移植会重建完整节点。若选 Syncthing，直接运行完整节点更合适 |

## 4. 方案比较与封装

| 方案 | Syncthing 的角色 | 需要新增的 Morde 工作 | 判断 |
|---|---|---|---|
| **A. 完整 Syncthing 子进程，文件目录成为 Workspace 内容** | PC、服务端、Host 三节点直接同步同一 folder；服务端保有持久副本 | Morde 管 Workspace/设备/Host 映射与准入、目录隔离和配额、交接就绪；迁移旧存储和调用方 | **优先做隔离试点**；最符合已确认的目录连续性目标 |
| **B. Syncthing 子进程 + 现有提交桥** | PC 与服务端暂存目录使用 Syncthing；Morde 旧版本库仍是权威 | 额外的双向导入/发布、冲突和回写环路处理，仍维护两套同步状态 | 仅在必须保留旧文件权威时采用，不作为首选 |
| **C. Go 内嵌或移植 Syncthing 核心** | 将完整模型和索引嵌入代码，或拆出核心 | 仍需节点配置与存储迁移，另承担内部接口和依赖维护 | 不推荐；直接用稳定进程边界更简单 |
| **D. 继续使用 `morde-sync`** | 不使用 Syncthing | 按现行融合 TODO 迁入唯一源码、完成各端验收 | 若 A 试点未达到交接与运维要求，可沿用现有方案 |

方案 A 的最小封装：

```text
本机任意工具 ↔ 用户选定 PC 目录 ↔ PC Syncthing 节点
                                      ↕ 仅连接已授权服务端设备
                             服务端 Syncthing 节点 ↔ 持久 Workspace 目录
                                      ↕ 仅连接已授权 Host 设备
                             Host Syncthing 节点 ↔ 云端电脑工作目录 ↔ 云端工具

Morde：账号、Workspace ID、PC/Host 设备映射、目录准入、授权、同步状态与交接检查点
```

- **PC 侧：** 由 Electron 主进程启动已校验版本的 Syncthing 二进制，使用安装专属 config/data 目录；REST 仅监听回环或受限本机 socket，API key 留在主进程。用户只选择本机目录与 Morde Workspace，主进程配置设备和 folder、订阅状态、处理停止与重启；renderer 不获得 REST key、设备私钥或任意文件路径能力。
- **服务端：** 建议每个账号使用独立进程、系统身份和 config/data 目录，在该账号下按 Workspace 分 folder，持久保存普通目录；这样单个 Syncthing 进程不会读到其他账号的文件。配额由文件系统或运行环境实际限制，不能只用 Syncthing `minDiskFree` 冒充用户配额。Morde 管设备与 folder 映射、Host 绑定和撤权；REST 管理入口仅对受信任服务开放。同步删除与覆盖会传播，服务端副本还需要独立备份。每账号常驻进程的内存、端口和进程数成本须在试点测量，规模不合格就重新评估隔离部署，而不能退回共用高权限进程。
- **Host 侧：** 云端电脑在自己的持久工作目录运行受控 Syncthing 节点，与服务端节点同步。Host 停止时服务端仍保存文件；恢复后先对齐目录再显示“可基于最新文件工作”。若同一持久目录可安全挂载给 Host，可在经过部署及隔离验证后省去该节点；当前 Host 使用 OpenSandbox/PVC，不能先假定 Portal 可直接共享该卷。
- **网络与隐私：** 此本地仓库的 Syncthing 默认配置含全局/本地发现、Relay、NAT、崩溃报告等开关，参见 [`OptionsConfiguration`](../../project/syncthing/lib/config/optionsconfiguration.go)。试点显式关闭这些外部通道，只允许已知服务端地址；设备证书与 Morde 登录凭证分开保管。撤销安装或 Host 绑定后，Morde 必须同步撤去对应设备的 folder 共享并断开连接。
- **状态显示：** Syncthing 的 `completion`、`status` 和 Events 可显示同步进度。监听与扫描提供持续的最终一致同步，不承诺每次保存瞬时到达；界面区分“本机已保存”“服务端已收到”“云端 Host 已就绪”。“可在云端继续”至少要求目标 Host 无待收项目、目录扫描无错误，并核对本次交接的设备索引/文件集合。源目录仍在变化时重新确认检查点。双方并发编辑时展示 Syncthing 冲突副本，不自动把其中一个当成最终结果。文件同步状态独立于任意 Agent 的任务完成状态。

**最小控制面封装：** PC 登录后选择一个本机目录并明确同步范围，界面说明该目录中未排除的文件都会到达云端；Morde 创建或取得 `workspace_id → folder_id → 服务端持久目录` 映射。PC 主进程生成并保管 Syncthing 设备身份，只将设备 ID 送 Morde 登记；Morde 校验账号和 Workspace 权限后在服务端节点登记该设备与 folder，并向 PC 返回服务端设备 ID、固定连接地址和 folder ID。Host 启动时按相同授权流程加入该 folder。双方配置成功后由各节点自动同步，不经 Morde API 中转每个文件块。Morde 提供“连接/断开目录、查看状态、撤销设备、请求交接”四类业务操作；PC 主进程把它们映射到本机 Syncthing 配置/REST，服务端控制器只操作对应账号节点。取消共享时先撤销节点的 folder 许可并断连，再处理本地保留或删除策略，不能把删除远端副本作为断连的隐式副作用。目录排除项需用户明确设置并在交接时显示，避免云端默默缺文件。

Syncthing 节点上的 [完成度接口](../../project/syncthing/lib/api/api.go)包含缺失字节/项与设备 sequence，[状态接口](../../project/syncthing/lib/model/folder_summary.go)包含 folder 状态、错误和本地/远端 sequence；这些可用于交接检查，但不能只取“100%”一个字段。进度与索引是同步证据；如某个云端任务要求固定版本的输入，再为该任务单独建立只读快照或文件清单，不必让所有实时文件变化都走旧 `sync-session/commit`。

**交接与保存证明：** 请求云端使用某批文件时，PC 先触发扫描并在本机目录稳定后记下文件清单/摘要和索引序号；服务端确认收到该索引，Host 确认无待收项目，再对需要使用的文件在目标目录核对摘要。若期间文件继续变化，就重新生成检查点；任务需要固定输入时从已核对的文件建立只读快照。云端修改要称为“已保存到 Workspace”，应先在服务端持久目录核对目标文件内容及摘要，不能只凭 Host 写入成功或 Syncthing 显示 100%。这些是 Morde 的业务门槛，Syncthing 不会替 Morde 自动提供事务化 Task 完成证明。

## 5. 建议的实施边界与验证

1. **先验证直接使用。** 在隔离的本地测试环境，用一个账号、一个 Workspace、一个 PC 和一个 Host 运行三节点 Syncthing；服务端持久目录独立于 Host 生命周期。验证 PC 文件自动到达服务端和 Host、Host 改动返回 PC、PC 离线时云端继续、Host 停止/恢复后补齐。此次验证不迁移真实项目或改生产 Workspace 数据。
2. **再验证产品边界。** 覆盖首次大目录、断网续传、重命名/删除、空目录、符号链接、大小写/Unicode、并发编辑冲突、磁盘满、额度、撤销 PC/Host 后断连、进程重启、服务端备份/恢复。记录每一端的实际文件摘要及 Syncthing 状态；测量不同文件规模和账号数下的进程内存、端口/连接数、磁盘索引和同步延迟，并与现有 `morde-sync` 比较。PC 打包还需验证目标平台二进制、版本锁定、哈希校验和升级兼容；本地 Syncthing 源码的 `go.mod` 要求 Go 1.26.2。
3. **试点通过后设计一次迁移。** 将旧 revision/块恢复为服务端普通目录并核对摘要，再让 Syncthing 建索引及同步到 PC/Host；同步修改 Workspace API/OpenAPI、Host binding、Task 文件证据、站点文件读取、删除/配额/备份以及客户端构建与测试。迁移期间保留旧数据直到校验通过，不长期保留两套可写文件权威。[融合 02](实施/融合02-Morde%20Workspace%20文件夹同步实施任务todo清单文档.md)负责试点门禁、服务端/Host 与文件权威迁移；[融合 03 的 F03-T06](实施/融合03-PC与iOS客户端融合实施任务todo清单文档.md)负责客户端封装。[融合架构](nanoMuse客户端与Morde服务端融合技术架构设计方案.md)已记录目标合同，试点结论须同步回写。试点未过则继续复用 [morde-sync](../project/morde-sync/) 与现有 [桌面封装](../project/nanoMuse/harness/desktop/src/morde-sync.ts)。

## 6. 为什么上一版坚持 revision

上一版把[最新融合稿](nanoMuse客户端与Morde服务端融合技术架构设计方案.md)的“文件先提交 revision，任务才可读；产物持久提交后才称完成”当成不可调整前提，所以推荐沿用 `morde-sync`，并认为 Syncthing 只能在旧存储前增加提交桥。这个推理对**保持现有存储和 Task 文件证明合同**是成立的，但没有把“Workspace 主要是跨电脑共享目录”作为可以改变文件权威的产品选择。

按现在确认的目标，文件同步应独立于 Agent；本机其他 AI 工具只需正常读写目录，云端电脑在文件就绪后也能读写同一项目。Morde 仍需要一个交接校验点，防止把“正在传输”显示为“云端已可读”；它可以是 Syncthing 设备状态加稳定文件检查点，不必必然是现有块存储 revision。改变这一点要同步修订依赖旧 revision 的业务代码与文档，不能只把 Syncthing 目录接到旁边就宣称已完成替换。

原始调研阶段只读源码与本地测试。`GOPROXY=off GOSUMDB=off go test ./...` 在 `project/morde-sync` 三个包通过；当时未运行 Syncthing 节点。后续双机试验记录在新版设计方案中；尚未做完整 5 GiB 性能或生产网络测试。Syncthing 源码为 MPL-2.0，`morde-sync` 为 GPL-3.0-or-later；若发行 Syncthing 二进制或修改其源码，应保留对应版权/许可证声明并在发布前核对分发义务。
