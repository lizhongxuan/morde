# Morde Workspace 文件夹同步设计方案

> 2026-10-30 设计复核稿；2026-10-31 状态更新：用户依据 Ubuntu 原生探针最终确认每账号普通 bridge、启动前 nftables 默认拒绝、仅允许 relay TCP 22067。`internal` 网络加 `/32` 路由不可达，普通 bridge 到私网绑定 relay 可达；公网回环与真实 Syncthing 尚未测。F02 仍未切换生产文件权威或部署，完整策略生命周期和端到端验收待完成。本文不包含测试主机凭证。

## 选型结论

**优先使用完整 Syncthing 进程作为同步引擎，由 Morde 封装授权、生命周期、目录映射、限额和文件就绪检查。** 这有双机实测支撑，但原版 Syncthing 不能执行全部限额规则，仍须先通过限额扩展原型。当前没有同条件证据表明 `morde-sync` 同步效果更好；此前以现有 Morde revision 和配额架构改造量较小为由直接推荐它，结论下得过早。不移植 Syncthing 核心；`morde-sync` 留作限额原型或生产联验失败后的备选。

## 1. 产品目标与边界

用户在 Morde PC 端选择一个本机文件夹并打开“同步”后，只要 PC 应用进程仍运行，就持续把该目录中的普通文件与同一 Morde Workspace 双向同步。服务端有一个对应的持久普通目录；云端电脑能在文件就绪后读取并修改 Workspace，修改也会返回本机。同步与 Agent 无关，本机可以使用任意编辑器或 AI 工具。

| 规则 | 确定的含义 |
|---|---|
| 单文件上限 | **300 MiB = 314,572,800 字节，含等于上限**；超过 1 字节就不参与同步，本机文件不删除。隐藏的普通文件同样参与。 |
| Workspace 上限 | **5 GiB = 5,368,709,120 字节，含等于上限**，按该 Workspace 当前参与同步的文件逻辑大小之和计算；被排除的超大文件不计入。不同设备和云端写入共用同一上限。 |
| 超额 | 若候选同步内容超过 5 GiB，保留最后已提交的服务端文件与本机文件，暂停新同步并明确显示超额量；用户使目录回到限额内后自动重新扫描、对账和继续。额度拒绝须持久终结原幂等 key，额度后来变化也不能让迟到的同 key 请求创建会话或提交；只有收到明确拒绝后客户端才为新尝试持久保存新 key，超时或结果未知仍用原 key 恢复。不得上传一部分后把界面标为“同步完成”。 |
| 本机安全副本 | 用户于 2026-10-29 首次确认、2026-10-30 再确认：同步覆盖或删除本机已有文件前保留可恢复的本机安全副本；副本不自动上传、不计入 5 GiB，但额外占用磁盘，原路径可能显示新版本。安全副本与参与同步、计入额度的冲突副本是不同对象；删除安全副本需用户另行明确确认。 |
| 运行时间 | 同步开关关闭或 PC 应用真正退出后，本机不保留后台同步进程；下次打开应用且开关仍开启时，验证目录和授权后继续。服务端和云端电脑仍可保存文件。 |
| 文件状态 | “本机已保存”“Workspace 已提交”“服务端普通目录已更新”“云端电脑已就绪”是不同状态，不用一个 100% 进度替代。 |

符号链接、socket 等非普通文件不跟随，也不扫描到所选目录之外；界面列出这些未同步项。目录结构和空目录都需保留，跨系统的大小写、Unicode 和权限差异在验收中处理。

“实时”表示文件保存后自动触发、断网后自动补齐的**最终一致**同步。正在写入的文件需等待稳定后扫描；健康网络中的小文件目标是数秒到达，具体上限以 5 GiB 和多文件验收决定。

用户确认：PC 断网或同步停止后，本机未上报的文件修改无法被云端实时获知；云端 Task 继续使用已确认的输入版本。PC 恢复同步且服务端确认存在外部变更后，相关 Task 安全暂停，用户按新版本重新提交；独立产物校验发现同一版本冲突时也必须暂停，不能凭路径、大小或摘要相同归因给原任务。界面应区分“云端已确认版本”和“离线设备的最新内容未知”。

截至 2026-10-29，本机 Syncthing 原型已用同步根外同卷兄弟目录 `.morde-preserved` 保存覆盖或删除前的旧内容，并以原子重命名保留旧 inode；已验证打开句柄晚写、同名连续替换、文件改目录、远端目录删除、保全目录与同步根重叠时拒绝操作，以及双节点同步不传播安全副本。真实本机双节点还覆盖 300 MiB 边界、超限后重启、缩小恢复、符号链接和离线重连扫描。隔离 Linux 容器实测跨卷 rename 失败及目录 inode 耗尽导致的 ENOSPC，原文件均保留；数据块耗尽未模拟。重启后的安全副本发现、查看与恢复 UI 归 F03-T06，代码也尚未集成到 Morde 发布构建。最终行为仍须保证任何保全、覆盖、删除或恢复失败都不丢原件；无法确认安全时暂停，并提示额外磁盘占用。安全副本不自动上传、不计入 5 GiB，删除副本需用户另行明确确认。

## 2. 实验结果与选型复核

试验使用 Syncthing v2.1.7 的官方 Mac ARM64 与 Linux AMD64 包，先核对下载摘要；测试配置关闭公共发现、局域网发现、Relay、NAT、自动升级和崩溃上报。远端 Syncthing 以低权限用户运行，REST 与同步端口只监听回环，PC 经 SSH 隧道连接。测试目录均为新建临时目录。

| 试验 | 观察结果 | 结论边界 |
|---|---|---|
| Mac 本机两个节点，文件监听延迟设为 1 秒 | 新建、反向新建、修改、删除分别约 2.43、2.24、2.25、2.44 秒到达 | 证明基本双向能力；同机样本不能作为公网延迟承诺 |
| Mac ↔ Ubuntu 测试主机 | 双向各传 1 MiB 新生成文件，双方 SHA-256 一致；断开隧道后两端各写一个文件，重连后都补齐 | 证明跨系统传输与断线补齐；未部署 Morde Portal/Host |
| 断线期间两端修改同一文件 | 重连后保留主文件及 `sync-conflict` 副本 | Syncthing 不会无痕吞掉这次冲突；产品仍需提示和人工解决 |
| 301 MiB 稀疏测试文件 | Syncthing 将其纳入本机索引 | 原生扫描没有 300 MiB 上限；未把这个大文件发送到测试主机 |
| 已同步文件加入 `.stignore` 后再修改 | 远端仍保留旧内容，两端 `completion` 均为 100% | 动态忽略会产生看似完成的旧副本，不能据此实现产品限额 |
| 远端闲置资源样本 | Syncthing 监护/工作进程 RSS 约 28/43 MiB | 仅一个小 Workspace 的瞬时值，不能推算多账号或 5 GiB 成本 |
| 当前 `morde-sync` 扫描 | 对本机 1 GiB 稀疏文件全量扫描约 0.76 秒；源码显示每次 live cycle 重新全量扫描与哈希，传输规划还可能再扫一次 | 现有引擎需增量索引优化，才可在 5 GiB 下承诺低延迟；该样本不是实际项目性能 |

**可复查的第二次双机演示：** 本机目录 `/Users/zhongxuan_li/Desktop/morde-workspace-syncthing-demo-20261028`，测试主机目录 `/var/tmp/morde-workspace-syncthing-demo-20261028`。初始本机创建文本、代码、中文路径、隐藏文件、1 MiB 二进制文件和空目录，均到达主机；随后主机新增文件并修改 `README.txt` 回到本机，本机修改 `src/hello.py` 并删除隐藏文件也到达主机。结束时两端普通文件均为 5 个、合计 1,048,814 字节，按“相对路径 + 大小 + SHA-256”计算的目录摘要同为 `436a14012453e81fb1985ca265207078ebe0ae75c2feeb954adeec18a905c57f`，空目录存在；两端 Syncthing 均为 `idle`、`needFiles=0`、`errors=0`、`completion=100`。`.stfolder` 是 Syncthing 自动生成的目录标记，不计入上述普通文件摘要。演示结束后已停止两端进程并删除配置，保留的文件夹**不会继续同步**。

Syncthing 的[忽略规则](https://docs.syncthing.net/users/ignoring.html)按文件名/路径匹配；本地 [`FolderConfiguration`](../../project/syncthing/lib/config/folderconfiguration.go)没有单文件大小或每 folder 逻辑总量上限。`minDiskFree` 是磁盘剩余空间保护，不能等价于每 Workspace 5 GiB 配额。

| 方案 | 实测同步能力 | 限额与改造 | 当前判断 |
|---|---|---|---|
| 原版 Syncthing 直接同步用户目录 | 双机双向、断线、冲突已实测 | 不能保证 300 MiB / 5 GiB；动态忽略会留下旧副本 | 不能原样上线 |
| **完整 Syncthing + Morde 封装 + 必要的准入扩展** | 复用已实测的同步引擎 | 先验证大小/配额在扫描、索引、接收和并发写入下均有效；再迁移 Morde 的文件权威与 Task 证明 | **优先原型和集成方向** |
| `morde-sync` + 服务端物理目录 | 有现成 live 协议；已做本机接近 5 GiB 串行样本，空目录未保留 | 可参考现有服务端配额事务，但需修复并发/恢复、增量索引、目录镜像和完整联验 | 备选，不能宣称效果更好 |

Syncthing 的强项是成熟的文件监听、索引、块传输、断线补齐、冲突副本和跨平台同步。我们已有 PC ↔ Ubuntu 的实际目录同步证据；`morde-sync` 的现有 [live 路径](../project/morde-sync/client/live_sync.go)多次全量 `Scan`。原版 Syncthing 与旧 Morde 已分别使用同一接近 5 GiB 的合成树做本机串行样本，结果及资源口径见 [原版实验](../.local/f02-stock-syncthing/README.md)和[融合 02 清单](实施/融合02-Morde%20Workspace%20文件夹同步实施任务todo清单文档.md)；两者实际组件、缓存及负载不同，且前者未包含本期补丁，尚不能作为等功能的性能或可靠性结论。当前[提交事务](../server/internal/store/postgres/workspace_sync.go)能约束旧协议的逻辑额度，但 Syncthing 直接写目录时不会经过它；因此选择 Syncthing 也必须重建服务端准入边界。

## 3. 推荐架构：完整节点 + Morde 控制面

```text
本机任意工具 → 用户所选目录 ↔ Syncthing PC 节点 ──────┐
                                                       │ 加密的 Syncthing relay 会话
云端电脑工作目录 ↔ Syncthing Host 节点 ────────────────┤ TCP 22067
                                                       ▼
                                     Morde 自有专用私有 relay
                                                       ▲
                                                       │ 加密的 Syncthing relay 会话
                         Syncthing 服务端节点 ↔ Workspace 持久普通目录

Morde 控制面：目录与设备授权、同步开关、策略准入、额度账本、
             状态/告警、撤权、文件稳定检查点和 Task 文件证明
```

PC 端选择目录后只登记映射，打开同步开关才启动版本固定的 Syncthing 伴随进程。Electron 主进程通过本机 REST API 配置 folder 和读取事件；renderer 不接触 API key 或任意目录读权限。设备证书、索引库、API key 存在应用私有目录，不进入同步目录。应用退出、关闭同步、登出或撤权时停止 PC 进程；下次启动先核对目录身份和 Morde 授权，再恢复连接。可沿用现有 [`checkedDirectory`](../project/nanoMuse/harness/desktop/src/morde-sync.ts)的根目录与重叠目录校验，并同时沿用调用方对系统目录及应用数据目录的限制；重启恢复时也须重新检查当前安全副本位置与全部已启用目录的重叠，不能只验证单个目录的身份。选中目录已有云端内容时先显示差异和冲突风险，不能静默覆盖。

服务端按账号隔离同步进程身份和文件权限；每个 Workspace 有服务端生成的随机 folder ID 和受控持久目录，不接受 PC 指定的服务端绝对路径。PC 与 Host 只同服务端配对，不互相直连，使额度和授权在中心节点生效。Syncthing 的设备 ID 是节点身份，Morde 还须校验账号、设备与 Workspace 权限；撤权时解除 folder 共享并断连。目标实现须将同步提交与撤权顺序可靠地串行化：撤权已生效而提交尚未落库时不能再提交，先于撤权完成的已确认文件保留；仅在请求入口检查一次凭证不足以保证这一点。账号节点使用每账号独立的普通 Docker bridge，容器不发布 GUI/REST 宿主端口；确定性的 msync 前缀桥接接口供 nftables 精确识别。Morde 依据 Docker inspect 返回的节点私网 IPv4 直连 REST API，API 客户端只允许回环或 RFC1918 私网 IPv4。用户已选择 Morde 自有专用私有 relay 作为受控传输入口：不加入公共 relay pool、关闭 status 服务，正式外网接入只开放 TCP 22067；Syncthing 节点数据经加密连接转发。relay 使用独立的 Docker bridge 并关闭 IP masquerading，Compose 默认只发布到宿主回环。账号节点使用每账号独立普通 bridge，relay 不接入账号网络。桌面客户端连接 relay 公告的静态公网 IPv4，服务端账号节点连接 relay 绑定到主机的具体 IPv4，避免依赖云 NAT 回环。Docker 启动前预装 nftables 默认拒绝账号节点到宿主机的新建连接和其他转发；仅 allow-set 中的账号 bridge 可经 Docker DNAT 访问 relay 主机绑定 IPv4 的原始 TCP 22067 端口，并允许匹配同一账号接口的返回包。节点发起的主机入站只允许 Portal REST TCP 8384 的已建立响应。主机防火墙与云安全组仅开放 relay 入站 TCP 22067。Relay 不触达节点内部 GUI/API。单账号/单 Workspace 节点的进程粒度和资源成本要用多账号基准确认，不能从一次 28/43 MiB 样本外推。

**F02-T04 当前实现与验收边界（2026-10-30）：** 桌面安装凭证通过设备证书挑战、服务端重算 Syncthing device ID 与私钥持有证明完成登记；PostgreSQL 保存安装、设备、Workspace folder 的期望状态。源码已包含每账号普通 Docker bridge 节点、host 私网 REST 访问、私有 relay 公网/私网双地址、获准设备与 folder 配置对账，以及安装、token、设备绑定撤权；绑定只有在节点配置应用后才转为 `active`，失败保持待处理并重试。Provisioner 发现节点创建、API 或授权配置失败时会请求 Docker 验证身份并停掉 owner 节点；Docker 重启策略使用 `unless-stopped`，避免显式停止节点在 Docker daemon 重启后自动复活。`workspace_sync_node.enabled` 默认关闭，`GET /api/capabilities` 不公布 workspace-sync，未通过验收前不开放。源码已实现普通 bridge 的空动态 nftables allow-set；只有当前授权快照已应用并提交、且账号仍有有效 folder 时，才放行该账号 bridge 到配置 relay host-bind IPv4/TCP 22067 与同一接口的匹配返回包。节点发起的主机入站仅保留 Portal 访问节点 REST TCP 8384 的已建立响应。owner session advisory lock 串行化 gate 与撤权；Docker/nftables 启动预载后恢复全部 owner，恢复失败会进入周期重试。Portal sudoers 仅允许只读 nft 表查询和固定 relay gate 命令。目标 Ubuntu 主机尚未安装这些策略单元或源码，现有 `cloudhost` 表仍只保护 `chbr0`；本轮未改变远端服务/规则，也未发送文件。主机启动/重启生命周期、真实 PC—relay—服务端 Syncthing 传输及在途撤权仍待 Ubuntu 验收，T04 源码实施完成，验证阻塞；workspace-sync capability 保持关闭。
**F02 MVP 后端切片（2026-10-31）：** 现阶段保留已有 `sync-sessions/commit` 显式上传，上传计划阶段执行 300 MiB 单文件硬拒绝；旧清单中的未变化大文件不因此被拒绝或重传。该入口已有 revision、幂等回执和 Workspace 逻辑配额事务。T05 的 Syncthing 接收 reservation/结算账本、Portal UDS 和节点 folder 配置现已进入本地源码；T06 当前目录物化、空目录 revision、目录变化事件及 Task 摘要已接入。当前目录存在但 marker 缺失/无效时，物化器失败关闭并保留原目录；Host applied receipt 与释放 revision 屏障也已补到服务端事务。Ubuntu 真实传输、Host 生命周期与故障恢复尚未验收，备份恢复核对未完成，workspace-sync capability 保持关闭。

**实现补充（2026-10-31）：** UDS reservation 请求现在附带 Syncthing Folder ID，服务端按持久 Workspace/folder binding、有效设备和安装状态做精确校验；Relay supervisor 也核验 relay 只连接专用 bridge。Workspace 删除清理同锁下的普通当前目录、revision marker 和遗留 staging；活动/待启用/撤销中的 folder、未应用的账号节点 revision 或接收 reservation 会使删除 claim 失败；启动对账跳过软删除的 Workspace。空目录已纳入 revision、任务摘要和接收 reservation；无有效 marker 的现有目录不自动覆盖。applied receipt 在事务内核验 Host generation、binding、精确 ReplicaID、完成下载 session 与目标 revision；Host 释放要求 applied/desired/current revision 一致，并阻止空目录在途变化。对应代码与集成用例尚未运行；备份恢复、Ubuntu 构建与联调仍待完成，workspace-sync capability 保持关闭。

**旧 Workspace 导入补充（2026-10-31）：** 旧 Docker volume 导入器现从只读来源扫描空目录，在读块前预检并在写块后重扫目录集合，之后将文件和空目录放入同一 revision 事务。正式数据盘点、隔离副本、失败回滚及 Syncthing 索引验收仍属于 F02-T09，未完成前不开放同步 capability。

**服务端持久目录是当前文件树的权威。** 若 Host 不能安全共享该卷，Host 作为第三节点同步自己的工作目录；不能假定现有独立 Host 卷天然共享。Syncthing 的 `completion=100` 只说明它自己的传输状态，不等于策略通过、服务端已落盘或 Host 已就绪。Morde 分别显示这些状态，并依据服务端文件摘要、准入账本与节点进度确认任务可读文件。

[融合架构第 5.6 节](nanoMuse客户端与Morde服务端融合技术架构设计方案.md)及现有 Task/Host 代码依赖 Workspace 的 `revision + 不可变块` 作为文件权威和任务证明。接入 Syncthing 必须同步修改手机附件、Portal 文件操作、Host binding/replica、Task 输入与产物证明、回收屏障和旧数据迁移。任务需要稳定输入或产物时，Morde 在服务端文件稳定且所需 Host 已对齐后计算清单/摘要，将**不可变快照**放到现有块库或等价存储作证据；旧 revision 可以保留为历史快照，但不能与目录长期并列为两套可写的当前文件权威。旧 Workspace 先恢复到新目录并核对，切换后保留可回退副本。

Task 的 input_revision 固定提交时的输入版本；独立的 workspace_checkpoint_revision 表示任务已经确认可继续使用的版本，初始与 input 相同。只有 Task 私有 Runtime 工作视图中的变更，经任务/operation 绑定的持久发布回执和 Workspace revision CAS 成功后，才能推进 checkpoint。路径、大小和内容摘要相同不能证明写入来源。Task 产物证明的 revision 必须等于该 Task 当前 checkpoint，历史版本中的同名或同摘要文件不能作为当前任务的产物。迁移 00077 已加入服务端 checkpoint 字段及围栏比较；产物 Store 校验已强制 revision 与 checkpoint 一致。Runtime 私有视图、受约束文件操作及持久 publish receipt/CAS 推进已在本地实现。真实 PC/服务端/Host 三节点 Syncthing 经 Morde LiveSync/API/PostgreSQL 的离线重连联测与独立 Linux ARM64 Morde Runtime 通过三轮 race：旧 Task 读取旧视图，外部变化提交新 revision 后旧 Task 被围栏，新 Task 读取新版本。Host replica controller 经认证 Portal HTTP 实际写入 applied revision receipt；TaskStopWorker 实际驱动旧 Task 暂停，并在 safe-after 窗口验证 Store.BeginHostRelease 未能开始释放。相同内容 Task 发布竞争由独立 PostgreSQL 用例覆盖，尚未与三节点离线/停止/释放竞争合并。生产 Host/Guardian 活动操作停止和容器销毁证明尚未验收。F02-T03 门禁已通过；T04 源码实施已完成、目标 Ubuntu 验证阻塞；T05 接收 reservation/结算账本源码已实现、Ubuntu 运行验收阻塞；T06 空目录 revision/Task 摘要源码已接入、备份恢复待完成；T07 applied receipt 与释放 revision 屏障源码已接入，对应新增/修改的集成用例尚未运行，workspace-sync capability 保持关闭。

所有以 Workspace 为输入的 Task 副作用，包括不直接读写文件的浏览器操作，都必须把 Task Workspace 绑定写入持久 operation。Store 对带 Workspace 绑定的 operation 在创建时检查准入；无绑定的 `prepared` 记录即使被创建，也不能因此执行副作用。所有 Workspace-bound Task operation 在 `prepared → dispatched` 的同一事务再次检查 Task `input_revision`/checkpoint、Workspace 当前 revision、活动 Host binding 和同步状态；只创建 `prepared` 不代表操作可执行。缺少绑定或版本已变化时，派发返回冲突并停止在副作用之前，后续由 Task 围栏进入 `workspace_input_changed` 并要求重新提交。

## 4. 300 MiB / 5 GiB 的技术关口

**原版 Syncthing + PC 预扫描、动态 `.stignore` 或 OS 磁盘配额，都不足以实现严格规则。** 外部预检会与 Syncthing 的扫描和拉取竞争；动态忽略已实测会留下远端旧版并显示 100%；磁盘占用不等于参与同步文件的逻辑总量。

优先做完整 Syncthing 节点上的必要准入扩展，保留原有索引、传输和冲突算法。以下是待验证设计，**不是现有能力**：

1. **单文件准入。** PC、Host、服务端节点在[常规文件扫描入口](../../project/syncthing/lib/scanner/walk.go)和远端文件接受处阻止 `> 300 MiB` 文件入库/下载，同时记录排除原因；原件留在写入端，不把大文件发送出去。文件恰好 300 MiB 可参与。不能只用异步 UI 扫描。
2. **旧文件增长越界。** 已同步的小文件在本机增长到 300 MiB 以上时，不能只忽略新内容而保留云端旧版为“已同步”。原型须证明旧副本被撤销或明确标为不可用，本机大文件不被删除或旧版覆盖，缩小后可以重新加入。这里可能涉及 Syncthing 的删除标记、索引和[拉取处理](../../project/syncthing/lib/model/folder_sendrecv.go)，改动范围必须通过原型确认。
3. **Workspace 总量。** 服务端按已接纳的当前文件清单维护逻辑额度，在接受候选索引/下载/落盘前串行核算增量；两个 PC/Host 同时扩容也不能让已接纳总量超过 5 GiB。超额保留已有内容，暂停新增/扩容，允许删除/缩小来恢复，并向 PC 展示未同步原因。参与同步的冲突副本计入额度。服务端目录仅允许受控 Syncthing/Morde 写入，Portal 等其他写入口必须走同一准入；磁盘剩余空间及临时文件另设保护。
4. **产品状态。** Morde 保存排除/拒绝清单和服务端准入账本，只在相关节点状态与实际目录摘要吻合后显示“服务端已同步”或“Host 已就绪”。Syncthing 的 100% 进度不能覆盖任何超限、旧副本或冲突状态。

这可能需要维护 Syncthing 源码补丁，不能先声称只是几行封装。若原型证明必须大面积改写索引、版本向量或拉取核心，停止该方向，转测 `morde-sync` 备选；不要把改写伪装成“直接用原版”。

## 5. 实施顺序与验收门槛

1. **限额原型先行。** 用新生成的无敏感内容测试文件验证 300 MiB、300 MiB+1 字节、已同步文件增长越界、云端旧副本处理、缩小后恢复、两端同时扩容、冲突副本、5 GiB 和 5 GiB+1 字节；断网/重启后不得把被拒文件报为已完成。未通过不启动文件权威迁移。
2. **同条件对比。** Syncthing 与当前 `morde-sync` 用同一合成测试树记录首次同步、海量小文件、接近 5 GiB 时改一个小文件、重命名/删除、断线恢复的时间、CPU、内存、磁盘和网络量；测多账号常驻服务端成本。在结果出现前不承诺哪方在 Morde 真实负载下更快。
3. **Morde 集成。** 固定 Syncthing 版本和包摘要，处理许可证声明；接入 PC 开关/状态、设备配对和受控连接；部署服务端持久目录和低权限进程；验证 Host 第三节点或共享目录；迁移 Task 文件就绪/快照、Host 交接、手机/Portal 写入及旧 Workspace 数据。
4. **端到端验收。** 用真实 Portal、PC、Host 复核文件摘要、额度账本和快照，覆盖撤权、磁盘满、大小写/Unicode、特殊文件、空目录、冲突、应用关闭、断网、升级和回滚。任务只有在所需文件已在服务端持久落盘且 Host 就绪时才放行。

**备选：** 若限额扩展需要长期维护大规模 Syncthing 分叉，或多账号负载下成本不可接受，再用 `morde-sync` 加持久增量索引、服务端普通目录和同样的端到端验收。它目前也没有达到“同步效果足够好”的证明。

## 6. 与融合 01～03 的开发衔接

**融合 02 先做独立 Syncthing 限额与任务围栏原型。** F02-T01～T03 可在融合 01 开发期间，使用本机 Syncthing 源码的独立分支、隔离目录和新生成测试文件推进；不修改 F01 业务代码，不把实验结果写成已上线能力。原版 Syncthing 的超限索引、旧副本和并发总量问题必须通过可维护的准入扩展解决，原型失败即停在门禁处。

当前 [F01-T09](实施/融合01-服务端融合与云端任务执行实施任务todo清单文档.md)按 `manifest/revision/commit` 完成输入、产物和 Task 证明；F01-T11/T13 验证 Host 释放与服务端联验。F01-T13 完成后留下可回退基线；[融合 02](实施/融合02-Morde%20Workspace%20文件夹同步实施任务todo清单文档.md)的 F02-T04 源码已包含设备登记、每账号服务端节点、普通 bridge 与启动前 fail-closed nftables relay gate；目标 Ubuntu 尚未安装这版策略和源码，真实启动顺序、PC/服务端节点传输及撤权竞态仍待验收，workspace-sync capability 保持关闭。T05～T10 的持久目录、统一额度准入、Host 节点或经验证的共享卷、Task 稳定快照/完成屏障、旧数据迁移和回滚仍待实施。迁移后的 Task/Host 文件保证必须重验，不能沿用旧协议的通过记录。

[融合 03](实施/融合03-PC与iOS客户端融合实施任务todo清单文档.md)接上已确定的服务端合同：F03-T06 负责 PC 的 Syncthing 二进制打包、目录选择、开关、状态及退出清理；F03-T04 负责 iOS 显式上传/下载，F03-T05/T07 验证文件接续和界面。F03-T01/T02 的原工程恢复和登录适配可独立准备。限额或围栏原型失败时，暂停融合 02 后续迁移任务，评估增强 `morde-sync` 备选并同步修订五份清单；不在 PC 端同时运行两套可写同步引擎。

**尚未验证：** 真实 Syncthing 5 GiB/并发准入、服务端权威目录和设备授权、多账号压力、Task 快照迁移、生产 Linux Host/Guardian 活动操作停止与容器销毁证明、Task 同内容发布竞争与三节点停止/释放组合竞态、生产网络入口，以及本轮新增的 applied receipt/release fence 集成用例。已通过的本机联测启动真实 Morde Runtime 与三节点 Syncthing，使用真实 replica controller/认证 Portal HTTP/PostgreSQL applied-revision receipt 和 TaskStopWorker，验证 stale Task 在 safe-after 窗口暂停时 Store.BeginHostRelease 返回未变更；该结果不替代生产 Host 生命周期验收。旧 morde-sync 的本机 5 GiB 串行样本及失败边界记录在融合 02 清单，也不代表两引擎比较或目标链路验收通过。
