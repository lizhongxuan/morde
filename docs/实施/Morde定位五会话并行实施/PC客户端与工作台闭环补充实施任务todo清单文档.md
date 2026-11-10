# PC 客户端与工作台闭环补充实施 TODO

## 目标与范围

依据 [Morde PC 客户端与工作台端到端闭环补充设计方案](../../Morde%20PC客户端与工作台端到端闭环补充设计方案.md)，补齐 PC 客户端与工作台的三个闭环：Workspace 历史差异与安全恢复、Task 只读观察、桌面端同步状态与安装包可信校验。

本清单是 [02-PC 客户端与工作台实施任务 TODO](02-PC客户端与工作台实施任务todo清单文档.md) 的增量补充。它不替代原清单，也不表示原清单中 PC05、PC07、PC10 已完成。完成对应实现和验收后，再按证据更新原清单中的对应子项；PC10 若仍有其他未完成工作，不得因此整体标记完成。

当前任务状态保留既有实施进度，以任务表为准；PCX-00 至 PCX-08 已完成本地实现与验收，PCX-09 已完成集成并按证据回写原 PC 客户端 TODO。真实远端文件传输、Ubuntu 实机和生产签名身份仍未验收，不据此扩大完成范围。

状态仅使用：未开始、进行中、待验证、完成、阻塞。每项完成时补充实现位置和验收证据；阻塞项记录原因及解除条件。

## 会话与文件边界

| 会话 | 负责范围 | 本清单相关文件边界 |
|---|---|---|
| Session 1 | Task 领域授权与业务 API | `server/internal/domain/task/**`、`server/internal/app/task*`、`server/internal/api/tasks*`、`server/internal/store/postgres/task*` 及对应测试 |
| Session 5 | Gateway、API 装配、数据库、OpenAPI | `server/internal/gateway/**`、`server/internal/api/api.go`、`server/internal/api/services.go`、`server/migrations/**`、`server/openapi/**`、相关 SQL 查询及生成代码、`server/cmd/**` 及对应测试 |
| Session 2 | Portal、Workbench、桌面客户端 | `web/portal/**`、`web/workbench/**`、`project/nanoMuse/harness/desktop/**` 及对应测试 |

共享接口先由 Session 1、2、5 对齐，OpenAPI 和数据库迁移由 Session 5 维护，Portal/Workbench 客户端类型与调用由 Session 2 维护。不得越过文件边界直接改动其他会话负责的文件；确需调整时先完成接口交接。旧版 `project/morde-desktop/**` 仅用于核对既有同步行为，本清单不要求删除或迁移该目录。

## 接口约定

以下路径表达设计意图。PCX-00 实施时须先对照现有路由与 OpenAPI，确定最终路径、请求/响应结构及错误语义，并同步给调用方。

1. Workspace 历史差异：新增只读 Gateway 查询，接收 Workspace ID 和起止 revision；调用者身份来自当前 Host observation ticket。首期仅 Workspace owner 可读；要求 Host actor、有效且健康的 applied binding，以及已确认的 revision；复用 W05 的 diff 范围和大小限制。此接口只返回差异，不授予历史文件或当前文件读取权限。
2. Workspace 恢复：复用现有 Host unbind、Host release、W05 Workspace restore、Host bind、Host restore API，不新增另一套生命周期接口。对现有支持 `Idempotency-Key` 的 unbind、Workspace restore、bind 请求复用持久化的稳定 key；当前 Host release/restore API 不接收该 header，提交结果未知时必须先刷新并协调 Host 状态，只在状态证明该操作尚未生效且仍满足前置条件时重新提交，不能把 `202` 当作完成或盲目重放。
3. Task observation ticket：Portal 登录 Session 签发，Task 必须对当前 actor 可读，且 Task 绑定 Host 与请求目标一致。ticket 绑定 task、host、actor、runtime generation、只读 scope、过期时间和 nonce；服务端只保存 token hash。禁止把 Host Cookie 转换成 Task 身份。
4. Workbench Task 只读会话：ticket 单次兑换为独立的 HttpOnly、Secure、SameSite=Strict Task-read cookie；兑换码建议 60 秒有效、Task-read 会话建议 5 分钟有效。快照和事件接口仅返回授权的 Task 状态、事件及安全的已验证产物元数据；不包含内容、文件、终端、控制或写入操作。每次读取、事件续连及活跃事件流都重新校验授权。
5. 安装包可信校验：本地 acceptance sidecar 使用签名信封或 detached attestation，签名覆盖包 hash、平台、架构、版本、来源、构建标识和验收证据。客户端根据本地受信公钥使用标准库 Ed25519 验签；验签结果由本地计算，不能由 sidecar 自报。缺少受信公钥、签名缺失、未知 key 或包 hash 不符时必须标记为未验证，不得进入已验证产物暂存区。
6. Morde-client 实时同步 IPC：复用主进程与 preload，不向 Renderer 传递绝对路径、授权凭据或文件内容。计划由 `state.liveSyncs` 提供逐 Workspace 状态，由 `startLiveSync(workspaceId, scope, rescanSeconds, confirmRecovery)`、`stopLiveSync(workspaceId)` 和 `onLiveSync(listener)` 启停/订阅；事件包含 `{ workspaceId, event }`。PCX-07 实施时同步固定字段、状态和错误语义。启动前在主进程检查登录、Workspace、目录身份和同步授权；能力关闭或状态未知时，在创建子进程/远端请求前失败关闭。`persisted_uncertain` 只允许用户显式确认后协调既有 session/checkpoint，不自动重放。

## 任务清单

| ID | 任务 | 负责会话 | 依赖 | 状态 |
|---|---|---|---|---|
| PCX-00 | 对齐接口、OpenAPI 与跨会话交接 | Session 5（Session 1、2 参与） | 无 | 完成 |
| PCX-01 | 实现 Workspace 历史 diff Gateway API | Session 5 | PCX-00、W05 diff 能力 | 完成 |
| PCX-02 | 在 Workbench 展示只读历史 diff | Session 2 | PCX-01 | 完成 |
| PCX-03 | 打通 Portal Workspace 安全恢复流程 | Session 2 | 现有 W05/Host 生命周期 API | 完成 |
| PCX-04 | 实现 Task observation ticket 签发与授权 | Session 1 | PCX-00、Task 授权边界 | 完成 |
| PCX-05 | 实现 ticket 兑换、Task-read 会话与只读 Gateway API | Session 5 | PCX-00、PCX-04 | 完成 |
| PCX-06 | 打通 Portal 到 Workbench 的 Task 只读观察 | Session 2 | PCX-04、PCX-05 | 完成 |
| PCX-07 | 将旧 Desktop 的逐 Workspace 实时同步状态与恢复迁入 Morde-client | Session 2 | 现有 MordeSync、Profile 与 binding/checkpoint | 完成 |
| PCX-08 | 完成本地 acceptance 包验签与可执行的暂存入口 | Session 2 | 已有 Ed25519/hash 验签器 | 完成 |
| PCX-09 | 集成验收并回写原 PC 客户端 TODO 状态 | Session 1、2、5 | PCX-01 至 PCX-08 | 完成 |

## 逐项实施与验收

### PCX-00 对齐接口、OpenAPI 与跨会话交接

**范围：** Session 5 维护的 `server/openapi/**` 与接口契约；Session 1、2 提供各自调用和授权要求。

- [x] 对照已有 Gateway、Portal、Workbench 路由，确认 diff、ticket 签发、ticket 兑换、Task snapshot 和事件续连的最终路径与字段；优先复用现有约定。
- [x] 明确 owner、Host actor、Task-read 授权失败、ticket 过期/重放、revision 无效、事件 cursor 失效等错误语义。
- [x] 在 OpenAPI 和客户端交接记录中标出由 Session 1、Session 5、Session 2 各自实现的部分；核实数据库迁移归 Session 5。
- [x] 若已有实现覆盖某项，记录路径和测试证据，将后续重复项缩减为缺口补齐。

**验收：** 三个会话使用同一份请求/响应与错误契约；OpenAPI 中的外部 API 与后端路由、客户端类型一致。

**完成证据：** OpenAPI 已覆盖 Workspace diff、Portal ticket、ticket 兑换、Task snapshot 与 SSE；Portal/Workbench schema 已由当前 OpenAPI 重新生成，客户端使用生成类型。

### PCX-01 实现 Workspace 历史 diff Gateway API

**范围：** `server/internal/gateway/**`、相关 API 装配与 OpenAPI、Gateway 测试。

- [x] 实现 PCX-00 确认的只读 revision diff 路由，身份取自现有 Host observation 上下文。
- [x] 限制为 owner；校验 Host actor、Workspace/Host binding 为有效、applied、healthy 状态，并只接受已确认的 revision。
- [x] 复用 W05 diff 的范围、大小和内容限制；不得因此开放 Workbench 文件读取或修改权限。
- [x] 覆盖未授权 actor、非 owner、无效/不健康 binding、未确认 revision、超限 diff 和正常 diff 的测试。

**验收：** Gateway 测试证明授权边界和 diff 限制生效；API 不提供写入或单文件下载能力。

**完成证据：** `go test ./internal/app ./internal/api ./internal/gateway ./internal/store/postgres ./cmd/portal` 通过；Gateway 路由只增加只读 diff 查询。

### PCX-02 在 Workbench 展示只读历史 diff

**范围：** `web/workbench/**` 及对应测试。

- [x] 通过 PCX-01 接口选择并加载两个 revision 的差异。
- [x] 呈现加载中、无差异、无权限、revision 不可用和服务错误状态。
- [x] 将差异作为只读内容安全渲染；不增加编辑、恢复、文件下载入口，也不绕过现有 ticket 权限。
- [x] 覆盖 API 调用、错误状态和只读交互的组件或浏览器测试。

**验收：** 有效授权时显示指定 revision 差异；权限撤销或 binding 失效后不继续显示新读取结果。

**完成证据：** Workbench targeted tests 通过；本地浏览器读取 revision 3→4 的只读元数据差异，页面没有恢复或下载差异的操作。

### PCX-03 打通 Portal Workspace 安全恢复流程

**范围：** `web/portal/**` 及对应测试；复用现有 Portal API。

- [x] 在恢复前明确提示用户停止 GUI、PTY、AI 等外部写入者，并要求显式确认；系统不能检测的写入者不得宣称已自动停止。
- [x] 对运行中的 Host 调用现有 unbind，携带 expected binding version、`writers_stopped=true` 和稳定幂等键；轮询至 unbinding 完成并确认 detached。
- [x] 对确认 detached 的 Host 调用现有 Host release，使计算停止；若请求结果未知，先刷新 Host 状态，按服务端状态协调，不盲目重放。调用 W05 restore，传入 expected current revision 和稳定幂等键；等待 materialized projection 完成。
- [x] 将 Workspace 绑定回已停止的 Host 并复用稳定幂等键；确认绑定和 Workspace projection 已应用后再调用 Host restore。Host restore 请求结果未知时先刷新 Host 状态，仅在仍为 stopped 且前置状态有效时提交；遵循现有 Host 只能在停止状态绑定的约束。
- [x] 超时、未知结果或状态与预期不符时停止后续步骤，刷新服务端状态并让用户可以安全恢复流程；不得自动跨过未确认状态继续。
- [x] 覆盖成功流程、用户取消、受 Idempotency-Key 保护的请求重试、Host release/restore 的状态协调、状态轮询、各阶段失败和未知状态的测试。

**验收：** Portal 只在每个前置状态被服务端确认后执行下一步；支持幂等键的请求在恢复时复用同一 key，Host release/restore 按状态协调，页面刷新或未知响应不会盲目重复产生副作用。

**完成证据：** Portal Workspaces 组件测试验证解绑→release→W05 restore→bind→Host restore 顺序、稳定 key 与未知解绑结果暂停；本地浏览器验证了写入者确认门禁。确认对话框在浏览器中关闭，未提交恢复写操作。

### PCX-04 实现 Task observation ticket 签发与授权

**范围：** `server/internal/domain/task/**`、`server/internal/app/task*`、`server/internal/api/tasks*`、Task store 相关文件及测试。

- [x] 添加 Portal Session 专用 ticket 签发接口；校验 actor 对 Task 的读取权限，并确认 Task 的 Host 与目标 Workbench Host 一致。
- [x] ticket 绑定 task、host、actor、runtime generation、只读 scope、短过期时间和 nonce；只持久化 token hash，兑换后不可再次使用。
- [x] 按现有 Task/Host 权限边界重新检查授权；runtime generation 或 Host 关联变化后使旧 ticket 失效。
- [x] 添加签发成功、越权 Task、Host 不匹配、过期、重复兑换及 runtime generation 变化的测试。

**验收：** 只有有权访问该 Task 的 Portal Session 能签发 ticket；ticket 不携带任何写入、终端或文件权限。

**完成证据：** `go test` app/API/Gateway 包和临时 PostgreSQL 单次兑换/哈希存储集成测试通过。

### PCX-05 实现 ticket 兑换、Task-read 会话与只读 Gateway API

**范围：** `server/internal/gateway/**`、API 装配、迁移、SQL 查询/生成代码、OpenAPI 和对应测试，均由 Session 5 负责。

- [x] 单次兑换 PCX-04 的 ticket；成功后设置与 Host observation 隔离的 Task-read cookie，并重定向到不含 ticket 的干净 URL。
- [x] cookie 使用 HttpOnly、Secure、SameSite=Strict，并设置短有效期；兑换码短时有效且只能消费一次。
- [x] 提供 Task snapshot 与带 cursor 的只读事件接口；只返回授权状态、事件和安全的已验证产物元数据。
- [x] 对 snapshot、每次事件续连和活跃事件流重新检查 Task 权限、Host/runtime 关联及会话有效性；权限撤销后立即停止发送。
- [x] 不提供 Task mutation、重启/取消、终端、文件读取或下载路由；不得接受 Host Cookie 作为 Task-read 会话。
- [x] 添加迁移回滚/升级验证，以及 cookie 属性、跨 Task/Host、伪造、过期、重放、撤权和 cursor 续连测试。

**验收：** 只读会话不能调用写接口；票据不可重放；权限撤销对后续读取和现有事件流都生效。OpenAPI、API 装配、迁移和测试同一交付。

**完成证据：** `00125_task_workbench_observation.sql`、OpenAPI 和 Gateway/API 装配已对齐；本地 PostgreSQL hash-only/single-use 集成及 Gateway 权限/SSE 测试通过。新增迁移往返集成测试，验证 `DownTo(124)` 后观察表删除且 `Up` 后恢复；存在的后续迁移列也核对回滚/恢复，临时 PostgreSQL 16 测试通过。

### PCX-06 打通 Portal 到 Workbench 的 Task 只读观察

**范围：** `web/portal/**`、`web/workbench/**` 及对应测试。

- [x] 在 Portal Task Session 中提供打开 Workbench Task 观察页的入口，通过 PCX-04 签发短时 ticket。
- [x] Workbench 兑换 ticket 后从干净 URL 建立 Task-read 会话，展示 Task 状态、可续接事件和已验证产物元数据。
- [x] 网络中断后按 cursor 续接事件；不得因此重新启动或修改 Task。
- [x] 页面不展示终端、文件内容、写入操作、取消/重启等控制；授权失败时清理页面可见数据并提示重新从 Portal 打开。
- [x] 覆盖 ticket 过期/重放、事件续接、撤权、Task/Host 不匹配和只读交互的浏览器或组件测试。

**验收：** Portal 到 Workbench 的完整 Task 观察链路可用；页面和请求均不产生 Task 副作用。

**完成证据：** Portal ticket API/安全 URL 组件测试、Workbench observation 组件测试及 Gateway/PostgreSQL 授权测试通过；本地浏览器确认 Task 页面只显示状态、事件与核验产物元数据。未在远端 Host 上运行真实浏览器会话。

### PCX-07 将旧 Desktop 的逐 Workspace 实时同步状态与恢复迁入 Morde-client

**范围：** `project/nanoMuse/harness/desktop/**` 及对应测试；复用现有 main IPC、`morde-sync`、Profile 和 SyncCheckpoint。

- [x] 复用现有 MordeSync、main IPC、Profile 和恢复点；保留单次同步/交接检查点及停止、未知结果处理，不新建同步引擎或本机数据库。
- [x] 通过 IPC 暴露现有同步进度与安全状态；renderer 不得取得绝对路径、token、文件内容或其他凭据。
- [x] 将旧 `project/morde-desktop` 的长驻实时同步启停、逐 Workspace 事件/进度/冲突与恢复状态接入 Morde-client；保留 Workspace ID、session、scope、阶段、进度、最近确认 revision、冲突及 recoveryRequired。继续调用 `morde-sync`，不复制扫描或差异算法。
- [x] 按 Workspace 独立保存运行状态和恢复点；开始/恢复前验证登录、有效 binding、目录设备/节点身份和原 scope。冲突时停止自动双向写入并保留两侧数据；未知副作用标记 `persisted_uncertain`，先协调既有 session/revision，不自动重放。
- [x] 提供显式的逐 Workspace 启停和恢复 UI；不在登录或应用启动时自动恢复。与同 Workspace 的其他同步 binding/重叠目录冲突时拒绝启动。
- [x] 主进程在任何实时同步子进程或远端请求前检查同步 capability；当前 `workspace-sync` 关闭时显示未开放并失败关闭。
- [x] 用临时目录、进程/IPC 替身覆盖多 Workspace 状态隔离、冲突、中断、重启恢复、未知结果、目录身份变化和 capability 拒绝；能力关闭时断言没有启动子进程或发出文件操作。

**验收：** 在临时目录和替身下，UI 能显示逐 Workspace 实时状态、进度、冲突和恢复要求；未知状态重启后仍保留，目录身份/授权变化、冲突或 capability 关闭均阻断操作。此项不以真实 peer、云端文件传输或 Ubuntu 实机作为本轮验收条件。

**完成证据：** Morde-client `npm run build` 通过，`npm test` 全量 274/274 通过；新增主进程测试覆盖 capability 关闭、目录身份变化、逐 Workspace 隔离、冲突保留、停止/重启后的 `persisted_uncertain` 和不自动恢复；renderer 测试覆盖 UI 启停、进度/冲突脱敏和恢复确认；`go test -count=1 ./...` 通过。所有同步测试使用本地替身，`workspace-sync` 仍关闭，未执行真实 peer 或文件传输。

### PCX-08 为本地 acceptance 包增加签名与 hash 验证

**范围：** `project/nanoMuse/harness/desktop/scripts/portal-downloads.mjs` 及其测试/fixture。

- [x] 将当前仅做结构检查的 sidecar 扩展为有签名的 acceptance envelope，或使用等效 detached attestation；明确规范化签名字节和字段版本。
- [x] 使用 Node 标准库 `node:crypto` Ed25519 验签并重新计算包 hash；校验包 hash、平台、架构、版本、来源、构建标识和验收证据都在签名覆盖范围内。
- [x] 从本地受信公钥配置读取公钥；sidecar 自带公钥或自报 verified 标志不得建立信任。
- [x] 缺少公钥、无签名、未知 key、无效签名或 hash 不匹配时，明确标记未验证并阻止进入已验证产物暂存区；不得静默降级为可信。
- [x] 使用测试临时密钥覆盖合法签名、篡改包、篡改字段、未知 key 和缺少公钥；不生成或提交生产私钥。
- [x] 脚本只做本地校验/暂存，不增加上传、外发或网络传输。
- [x] 对齐 `stage:portal` npm 命令与 CLI 所需 Portal URL 参数，明确唯一调用方式；用临时目录验证参数正确时只在本地暂存、参数缺失/sidecar 无效时不留下可安装或 verified 产物。

**验收：** 只有使用预配置受信公钥且包 hash 与签名内容匹配的包才会被标记为已验证；旧版未签名 sidecar 不会被误报为可信；本地暂存入口参数可用且不访问远端、不外发文件。正式发布前另行确定生产签名身份和密钥托管方式。

**完成证据：** `node --test tests/portal-downloads.test.mjs` 通过（7/7）；实际调用 `npm run stage:portal -- https://portal.example.test`，使用临时 Ed25519 密钥和临时 dist/output 目录成功暂存签名测试包；缺少 URL 或不可信 sidecar 时不创建输出。测试没有写入 Portal 目录、访问远端或制作生产发行物。

### PCX-09 集成验收并回写原 PC 客户端 TODO 状态

**范围：** Session 1、2、5 各自负责的实现和测试文件；更新原 PC 客户端 TODO 中与本清单直接对应的子项。

- [x] 分别执行并记录 Task 授权/API、Gateway diff 与 ticket、Portal/Workbench 浏览器、Morde 客户端 IPC/同步状态、下载验签的针对性验证。
- [x] 使用本地 mock、临时目录和按需的本地测试数据库；记录测试命令、结果、提交/实现位置和未覆盖范围。
- [x] 端到端核对 Workspace 恢复顺序、Task ticket 单次兑换与撤权、同步不确定状态恢复、验签失败关闭。
- [x] 验收通过后，仅更新原 TODO 中有直接证据支持的 PC05、PC07、PC10 子项；不得把未实施的原清单工作一并标为完成。
- [x] 若任一项仅通过 mock 或局部测试，状态保留为待验证并写清后续真实环境验收条件。
- [x] 完成 PCX-07 的旧实时同步迁移和 PCX-08 暂存入口后，重跑受影响模块检查，并只把有直接实现与测试证据的原 PC TODO 子项更新为完成；保留 workspace-sync 关闭、Ubuntu 未验和生产签名身份未定的状态。

**验收：** 原清单每个被更新的子项都能追溯到实现与验证证据；无证据的范围保持未完成。

**完成证据（2026-11-03）：** Portal 6 个定向测试文件 161/161 通过，`typecheck` 与修改文件 ESLint 通过；Workbench 5 个定向测试文件 60/60 通过，`typecheck` 与修改文件 ESLint 通过；Morde-client `npm run build` 与全量 `npm test` 通过（274/274）；下载验签与 npm staging 入口测试 7/7 通过；`go test -count=1 ./...` 通过。服务端 app/API/Gateway/PostgreSQL/cmd 测试、本地 PostgreSQL ticket 持久化集成和迁移往返测试已有对应证据。OpenAPI 已生成 Portal/Workbench schema；本地浏览器验收了 Workspace 只读 diff、Portal 恢复确认门禁和 Task 只读观察页。原 PC10 子项只勾选本地实时同步迁移；旧独立 Desktop 打包入口、真实远端同步、Ubuntu 和生产签名身份仍未完成。

**尚未覆盖：** 未访问远端测试机/生产 Host；未执行真实跨端文件传输、Ubuntu 实机验收或生产包签名/staging。生产可信公钥及密钥托管仍需发布前确定；Task ticket 兑换链由服务端集成测试覆盖，本地浏览器未连接真实 Host Gateway。

PCX-07、PCX-08 与 PCX-09 的本地实现和验收已完成；真实远端文件传输、Ubuntu 实机及生产签名身份保留为后续验收条件。

PC10 后续收尾记录（2026-11-03）：在本地独立安全审查与同步端到端验收通过后，已退役旧 Desktop 的发行打包 npm 命令、schema v1 Portal 暂存实现及 Portal 个人模型/API Key 用户表单；旧源码和已有 release 文件保留。具体证据与状态见原 PC 客户端 TODO 的 PC10。真实部署服务、跨设备/文件传输、Ubuntu 和生产签名仍待发布验收。

## 全局约束与发布门槛

- 所有实现、测试和产物仅限本地工作区；不连接测试服务器，不上传或外发文件，不使用真实生产密钥。
- 以本地 mock、临时目录及必要的本地数据库完成开发验证；这些结果不等价于真实远端环境验收。
- `workspace-sync` capability 在单独完成授权、安全和端到端评审前保持关闭。
- 未知状态、授权撤销、超时、ticket 重放、签名不可信等安全边界必须失败关闭。
- 生产安装包验签依赖预先配置的可信公钥；生产签名身份与密钥托管尚未确定时，包只能标为未验证，不能作为已验证产物发布。
