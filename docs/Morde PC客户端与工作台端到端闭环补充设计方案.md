# Morde PC客户端与工作台端到端闭环补充设计方案

本方案补充 [PC客户端与工作台实施清单](实施/Morde定位五会话并行实施/02-PC客户端与工作台实施任务todo清单文档.md) 和 [Morde定位对齐与功能精简设计方案](Morde定位对齐与功能精简设计方案.md)，为 PC05、PC07、PC10 给出可实现的授权、接口和恢复流程。

代码中已有 Workspace diff/restore、Host Workspace 解绑和只读 Workbench 票据的基础；缺口集中在 Workbench 如何取得历史与 Task 权限、如何编排已存在的 Host 生命周期，以及如何把旧桌面端的实时同步状态迁入 Morde-client。补齐时复用已有服务和安全边界，不新建第二套 Workspace 版本权威或同步引擎。

## 1. 当前基础与未闭环项

| 能力 | 已有实现 | 需要补齐 |
| --- | --- | --- |
| PC05 Workspace 版本 | W05 的 diff/restore API 已实现；Portal 使用 owner Session 调用。Gateway 已提供经 Host Observation Ticket 进入的只读 Workspace 页面。 | Gateway 没有历史 diff 路由；Workspace 当前文件读取权限不包含历史权限；Workbench 尚未接通解绑、停止 Host、恢复和重绑流程。 |
| PC05 Host 生命周期 | Portal 已有 Host Workspace 绑定和解绑 API。解绑要求 Host 正在运行、提交绑定版本和幂等键、由用户确认写入者已停止，之后进入异步 unbinding；Portal 会展示并刷新解绑状态。Host Release 停止计算但保留 Host 身份和数据卷。 | PC05 尚未把解绑、等待 detached、Release、W05 restore、重绑和 Host Restore 编排成可恢复流程。客户端确认不能代替服务端的在途写入检查。 |
| PC07 Task 读取 | Portal/Assistant Task API 支持 Session 或适用的 Client Credential；Workbench Host Observation Ticket 能携带操作者身份并动态复核 Host 权限。 | Host Cookie 不是 Task principal；Workbench 没有 Task 专用只读票据或 Task 事件路由。 |
| PC10 同步和发行物 | Morde-client 通过主进程 IPC 手动读取 Workspace 同步节点汇总状态，并保存绑定与恢复检查点。旧 Morde Desktop 有 Workspace 级实时进度、冲突和恢复状态。Portal schema v3 暂存适配只校验 sidecar 结构。 | 把旧端实时状态和冲突恢复接到当前主进程；sidecar 不能证明签名或验收证据真实；当前 arm64 目录包未签名。 |

截至 2026-11-03，其他会话在推进相关基础：会话 1 正在处理 R14 的 Task/Workspace 围栏；会话 5 正在处理 W20、W22 和 W14；会话 3 正在盘点 iOS 缺口。当前记录没有把本方案中的 Workbench 历史 diff、Task 只读票据或 Morde-client 状态等价迁移分配给其他会话。iOS 端如需 Task 观察，应复用同一个 Task 只读合同。

## 2. 设计选择

身份和权限保持分离。Host Observation Ticket 只建立带操作者身份的 Host 会话；其中 workspace:file:read 只授权当前已应用 Workspace 的只读文件浏览。它不自动扩大为历史 diff、Task 读取、恢复或电脑控制。

PC05 第一阶段采用最小权限：Host/Workspace 所有者可在 Workbench 查看历史 diff；共享成员仍按现有权限读取当前已应用文件，不读取历史版本。恢复仍由 Portal 的 owner Session 确认和提交。若之后需要让共享成员读历史，新增独立 workspace:history:read 权限，不扩张 workspace:file:read 的含义。

Task 观察使用独立的一次性 TaskReadTicket 和 Gateway 只读会话，不复用 Host Cookie 作为 Task 身份。Task 只读票据不能执行任务操作、控制 Host 或读文件内容。

同步迁移复用当前 Morde-client 主进程、morde-sync 操作和本机加密 Profile，不复制旧 UI 或建立第二个同步服务。无服务端确认、授权不明、冲突或操作结果未知时，保持未知/阻断状态，不自动覆盖数据。

## 3. PC05 Workspace 历史 diff 与恢复

### 3.1 Workbench 历史 diff

增加只读 Gateway 路由：**GET /__ch/api/workspaces/{workspace_id}/diff?from_revision=…&to_revision=…**。它调用现有 diff 计算和内容展示限制，但不能直接把 Portal owner-session Handler 暴露给 Host Cookie。

Gateway 使用现有 Host Observation Ticket 中的 actor 身份校验：

- Host Cookie 的 Host 必须等于请求上下文 Host，操作者必须是 Host/Workspace 所有者。
- Workspace 必须仍绑定该 Host，binding 为 active，Workspace 为 ready，sync 状态健康，applied_revision、desired_revision 和当前 revision 一致。
- from_revision 和 to_revision 必须是该 Workspace 已确认的不可变 revision，且不大于当前 revision。
- unbinding、detached、failed、同步冲突或权限撤销时返回拒绝/冲突，不返回部分差异。
- 复用 W05 的结果字段、文本大小和变更数量上限。二进制、大文件和不可展示内容只返回元数据状态，不提供文件下载或其他 Workspace 的内容。

共享成员只有 workspace:file:read 时仍不能读取历史 diff。此权限继续只对应当前已应用的文件清单和只读下载。Workbench 没有 owner 身份或历史权限时，提示在 Portal 查看，不尝试交换或伪造 Portal Session。

### 3.2 安全恢复顺序

Workbench 提供查看和恢复入口，但最终写操作转到 Portal 的账号授权页面。跳转只带 Workspace ID、目标 revision 和期望当前 revision；链接不带 Cookie、访问令牌或一次性凭据。Portal 页面重新读取状态并要求显式确认，再使用现有 W05 owner-session restore API。

操作顺序固定如下：

1. 在 Portal 检查 Host、Workspace binding、同步状态、Task 和其他写入者。
2. 由于 Morde 无法自动发现所有 GUI/PTY 写入者，用户先关闭 AI 工具、IDE、终端任务及其他会写 Workspace 的应用，再确认停止。
3. Host 必须仍处于 running，调用现有解绑 API，提交 expected_binding_version 和 Idempotency-Key。成功返回 unbinding 后，界面持续刷新状态。
4. 只有 Runtime 确认最终检查点且 binding 变为 detached 才能继续。Runtime 离线、冲突或状态未知时保持等待/阻断，不能靠超时假定解绑成功。
5. 对已 detached 的 Host 调用现有 Release，使计算状态变为 stopped。Release 会保留 Host 身份和数据卷；失败时不执行 Workspace restore。
6. 用户在 Portal 选择目标 revision 和路径，按当前 Workspace revision 作为 expected_current_revision 调用 W05 restore。服务端继续检查无非 detached binding、无在途同步/Task/文件写入并以新 revision 记录恢复结果。
7. 等待新 revision 的 current projection 确认后，使用现有绑定 API 将该 Workspace 绑定到 stopped Host，再调用 Host Restore 启动计算。Host 不能在 Workspace 未就绪或未绑定时启动。

现有解绑 API 要求 Host 运行，因此顺序不能改成先 Release 再解绑。解绑请求只表明用户确认外部写入者已停止；服务端仍须执行已有的 Task、同步、版本和稳定快照检查。Workbench 与 Portal 在每一步展示服务端状态，不把请求已受理显示为操作已完成。

恢复使用相同 Idempotency-Key 重试。若提交结果未知，先查询 W05 restore 回执和 Workspace revision；不生成新 key 重做副作用。物化失败保留旧投影和恢复记录，待同 key 重试或服务端对账。

## 4. PC07 Task 只读观察票据

### 4.1 票据签发和兑换

增加 Portal Session 入口：**POST /api/tasks/{task_id}/workbench-observation-ticket**。它只为当前账号本来就有权读取的 Task 签发票据，并要求 Task 的 host_id 与当前 Workbench Host 一致。没有 Host 的云端 Task 继续在 Portal Assistant 查看。

票据记录至少绑定 Task ID、Host ID、操作者 User ID、Host Runtime generation、只读 scope、过期时间、一次性 nonce 和消费状态。数据库只保存票据摘要。签发时调用现有 Task 读取授权检查，并检查 Task 与 Host 当前关联；兑换时再次检查绑定、Task 可见性和成员权限。一次性兑换码有效期不超过 60 秒，成功兑换后立即作废。

Workbench 通过 Gateway 同源兑换端点提交兑换码。Gateway 返回干净的 Workbench URL，并设置独立的 HttpOnly、Secure、SameSite=Strict TaskRead Cookie；Cookie 只适用于指定 Task 的读取路由，建议 5 分钟过期。兑换 URL 使用 no-store/no-referrer，消费后移除票据参数。Cookie 不写入 localStorage/sessionStorage，也不与 Host Cookie 合并。

### 4.2 读取范围与撤权

只开放两个 GET 操作：Task 当前状态快照，以及按 after/event cursor 续读 Task 事件。响应复用 Portal Task API 的字段和事件脱敏逻辑，只返回对该操作者可见的状态、事件和已验证产物元数据，不返回文件内容、绝对路径、模型密钥或 Host 控制凭据。

TaskRead Cookie 的 principal 只能表达 task.read，不能被识别为 Host owner、Host member、Session 或 Client Credential。每个快照请求、SSE 重连以及持续事件流均复核 Task read 权限、Task/Host 绑定和 Runtime generation；撤权后关闭事件流并使 Cookie 失效。事件 cursor 只控制续读位置，不作为授权凭据。

WorkBench 的 Task 页面由 Portal Assistant 的明确入口打开，初始 URL 只携带 Task ID；未兑换有效票据时不加载 Task 数据。读取失败显示无权、过期或 Host 不匹配；网络断开时保留最后一次已确认事件序号并标记未知，恢复连接只从 cursor 续读，不重新提交 Task。

审批、澄清、暂停、继续、取消、终端输入、键鼠、Workspace 写入、文件下载及控制权申请均不属于只读票据。它们继续走现有独立权限和操作流程。

## 5. PC10 Morde-client 状态等价迁移与本机包验收

### 5.1 Workspace 实时状态

旧 Morde Desktop 已有 WorkspaceConnectionView、LiveSyncEvent、SyncProgress 和 SyncConflict；状态包括 watching、scanning、transferring、conflict、reconnecting、error 与 persisted_uncertain，并带有 Workspace、session、进度和冲突信息。当前 Morde-client 的手动 workspaceNode.workspaceStatus 只是一刻的汇总快照，不等价于这些运行状态。

在 Morde-client 中复用现有主进程 IPC 和 morde-sync 引擎，把每个 Workspace 的运行事件及恢复检查点汇总到现有 workspace-sync-status 读取结果。至少保留 Workspace ID、同步模式、scope、session ID、阶段、文件/字节进度、最近确认 revision、冲突状态和 recoveryRequired。LiveSyncEvent 的状态语义沿用旧端；不要重新实现扫描、差异或传输算法。

敏感字段仍只留在主进程：本机绝对路径、授权 token 和文件内容不发给 Renderer；冲突界面只显示必要的相对路径和冲突原因。复用现有加密 Profile、binding 状态和 SyncCheckpoint 存储，不新建本机数据库。

启动或恢复时先确认 Client 登录、Workspace binding、目录设备/节点身份和原 scope 仍有效。能确认时按原 session/checkpoint 继续；无法确认远端副作用是否完成时显示 persisted_uncertain，先查服务端 session/revision，不生成新的同步请求。冲突时停止自动双向写入，保留本地和云端版本，待用户显式选择恢复方式。

本轮只补本机状态及替身/临时目录验收，不启动真实 peer 或传输文件。workspace-sync capability 继续遵守现有关闭状态；状态迁移不能被解读为真实跨设备同步已验收。

### 5.2 包签名和验收证据

当前 schema v3 sidecar 仅提供字段结构。sidecar 的签名状态、测试摘要和包 hash 都是未验证输入；缺 sidecar、无签名或 hash 不匹配一律为 unverified，不能暂存为可安装发行物。Morde-client 只有在本地验证器计算并验证完整包后才能设置 verified。

本机接受的清单包含应用版本、平台、架构、源码提交、构建标识、完整安装包 SHA-256、签名 key ID 和验收记录摘要。清单采用规范化编码并由受信私钥签名；验证器用本机预先配置的公钥验证签名，再计算安装包 hash 并比对平台、版本和验收记录。未知 key、验收记录缺失、版本回退、签名无效或文件 hash 不一致时拒绝暂存/安装。

私钥不进仓库或 sidecar；本机密钥存储由系统 Keychain/安全凭据库保护，验收器的公钥信任根与待验包分开配置。此方案只覆盖本机受信验收；当前不上传 Portal、不外发安装包或证据。正式分发签名身份、密钥托管和平台公证属于另一个发行决策；在用户指定可信身份前，发行物保持 unverified，identity 为 null 的包只用于本机开发检查。

## 6. 实施边界和交接顺序

| 阶段 | 建议负责方 | 交付合同 |
| --- | --- | --- |
| Workbench Workspace 历史读取 | 会话 5 提供 Gateway 路由和 Host actor 授权；PC 会话接入 Workbench WorkspaceBrowser。 | 新增的 Gateway diff 路由复用 W05 diff 结果和限制；先只支持所有者。现有 workspace:file:read 不扩权。 |
| Host 恢复流程 | 会话 5 核对 Workspace binding、Host lifecycle 和 W05 guard；PC 会话编排 Portal 页面、状态刷新和错误呈现。 | 复用现有 unbind、Release、restore、bind、Host Restore；不新增平行 Host/Workspace 生命周期。 |
| Task 只读票据 | 会话 1 实现 Task read 授权和票据校验；会话 5 装配 Gateway 兑换与 Task 读取路由；PC 会话接入 Assistant → Workbench 入口。 | TaskReadTicket 与 Host Cookie 分离，Task API/OpenAPI/测试同步；Task ID 不构成授权。 |
| Morde-client 状态迁移 | PC 会话只改 Morde-client 正式入口和对应测试。 | 复用 morde-sync、IPC、Profile 和 checkpoint；旧 Morde Desktop 在对等验收前保留。 |
| 汇总验收 | 会话 5 在 W14 汇总服务端和客户端证据；各会话只更新自己的 TODO。 | 每项分开记录代码、本机单测、Portal/Workbench 浏览器、临时 PostgreSQL 和未覆盖环境。 |

实现前由各文件所有者将接口字段、权限、错误码、幂等键和迁移号写入各自 TODO 交接记录；会话 5 按仓库规则串行更新 OpenAPI、生成类型和数据库迁移。PC 客户端不直接改服务端、迁移或 OpenAPI。

## 7. 验收矩阵

| 能力 | 通过条件 | 必须失败关闭 |
| --- | --- | --- |
| PC05 diff | Host 所有者经 Workbench 票据读取 active/applied Workspace 的两个已确认 revision；内容和规模限制与 W05 一致。 | 只有 workspace:file:read 的共享成员读取历史、Host 不匹配、unbinding、过期 revision、状态不健康、权限撤销。 |
| PC05 restore | Portal 用户按顺序完成写入者确认、异步解绑、Host Release、W05 restore、projection 确认、重绑和 Host Restore。重复请求只得到同一回执。 | Runtime 离线、绑定非 detached、在途写入/Task/同步、revision 冲突、结果未知或物化失败时仍启动后续阶段。 |
| PC07 Task 观察 | 有权用户兑换一次性票据后查看同 Host 的 Task 快照/事件；SSE 从 cursor 续读，撤权关闭流。 | Task ID 单独读取、跨 Host/跨账号、票据过期/重放、只读票据执行写操作或读取文件内容。 |
| PC10 状态迁移 | 临时目录与事件替身验证每 Workspace 进度、冲突、重启恢复及未知状态；Renderer 不接触 token/绝对路径。 | 未知结果自动重跑、冲突覆盖任一侧、授权或目录身份变更后继续同步。 |
| PC10 包验收 | 本机签名有效且包 hash、平台、版本、提交和验收摘要一致后才标 verified。 | 仅有 sidecar、无受信公钥、identity 为 null、签名/包 hash 不一致或验收摘要缺失仍暂存。 |

所有文件相关测试限制在本机进程、临时目录或内存替身，不发送真实或合成业务文件。F02-T10 的 peer 连接超时和 workspace-sync capability 关闭状态不因本方案改变；未进行文件传输、Ubuntu 实测或正式发行签名之前，不宣称对应链路已跑通。

## 8. 需要用户决策的边界

本方案先采用两个安全默认值，不阻塞后续本机实现：共享成员的 workspace:file:read 不包含历史版本权限；Workbench 负责展示差异并将写操作交给 Portal 的 owner Session。

只有进入正式发行前，需要确定可信签名身份和密钥托管方式。此前只可验证本机开发包，不能将其标成正式 verified 发行物。
