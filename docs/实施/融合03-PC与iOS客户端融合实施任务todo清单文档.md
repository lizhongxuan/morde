# 融合 03：PC 与 iOS 客户端融合原任务基线与验收

> **五会话执行入口（2026-11-02）：** 本文保留拆分时的原编号、范围、状态和证据，作为需求与验收基线；当前开发状态由[五会话并行执行入口](融合03-05五会话并行实施/并行执行入口.md)中的任务文档维护。开发按 AI 会话最多一天规划，测试和验收单独安排，不计入开发工作量。本文的基线状态不自动代表当前完成状态。

> 2026-11-01 任务细分：保留原编号、完整要求和既有证据；状态按子任务逐项维护。本轮 F03-T02.04 iOS 本地隔离、F03-T02.05 服务端围栏与旧 Android 兼容入口、F03-T02.06 PC 接入已完成当前本地验证；F03-T02.07 iOS 有序登录代码和 helper 验证已完成，F03-T04.03 本地实现与设备构建已完成，二者的模拟器验收仍待本机 CoreSimulator 更新。

> 2026-10-29 初次整理时未执行构建或运行验收；下列 2026-11-01 进展为拆分时保留的历史证据。
> 前置：[01 服务端融合与云端任务执行](融合01-服务端融合与云端任务执行实施任务todo清单文档.md)已按用户确认结项；[02 Workspace 文件夹同步](融合02-Morde%20Workspace%20文件夹同步实施任务todo清单文档.md)仍在实施。PC/iOS 的本地准备、登录及无文件能力可按子任务直接依赖推进；真实文件上传、Syncthing 接入及文件链路联验仍须 F02-T10。03 正式交接 04 前保留原主流程门槛。原 F02-T01～T10 已顺延为 F03-T01～T10，新增 F03-T11；原任务状态与有效记录保留。
> 规则、状态与 D1–D10 沿用 01；依据[云端运行时统一化需求](../Morde云端Agent运行时统一化需求文档.md)、[融合架构](../nanoMuse客户端与Morde服务端融合技术架构设计方案.md)及 [iOS 目标态设计稿](../设计图/Morde-nanoMuse融合后iOS手机端目标态设计稿.html)。只在本地修改和验证，不外发文件。
> 原工程、账号、工作台、事件恢复及 PC 文件保全的具体边界见[客户端与工作台分项设计](融合03-客户端与工作台分项设计.md)。

### 本地测试环境
仅在本机或本地隔离环境验证；目标环境未具备时单列待验证。凭据不写入文档，不向远端发送文件或部署。

## 1. 本阶段目标与修改边界

交付原 nanoMuse PC/iOS 内的主流程：**手机提交云端任务 → 可关闭手机 → 服务端继续 → PC 查看同一任务与文件、继续对话、审批或接管 → 随时撤权**。PC/iOS 均为云端 Agent 客户端；每账号有独立常驻 Runtime，不同会话的任务可按配额并行。客户端不启动本机 Agent、模型与工具循环、本机 Agent 任务执行器或子 Agent；保留本地数据和用户选择的文件传输。目标 PC 响应获准的单次截图、文件查看和命令归 04，不等于本机 Agent 执行，也不在 03 主流程交接前假装可用。页面分别展示云端 Agent 与所操作电脑的状态。

**最新平台范围：PC 继续在本期；手机端只修改 iOS。Android 源码、配置、构建和验收保持不变，也不作为本期交付依赖。** 路径名若同时出现 `android` 与 `ios`，先核实 Xcode target membership，只改 iOS target 实际拥有的文件。

| 位置（相对仓库根目录） | 本文处理 |
|---|---|
| `project/nanoMuse/harness/desktop/` | 恢复原工程中适用的界面启动链；吸收 `morde-main` 的登录、凭证和资源能力，移除本机 Agent 启动链并更新 preload/IPC、资源复制和打包 |
| `project/nanoMuse/harness/dsh-nanomuse/` | 复用原客户端面板、账号适配和任务交互接入 Morde；客户端不启动 dsh，服务端继续复用相关源码 |
| `project/nanoMuse/android/src/ios/`（只限 iOS target） | 恢复 `Minis.xcodeproj`、`MinisApp`、原 `NanoMuse/` 页面、extension 与资源；独立 `Morde/` 壳的有效代码按需迁入。Android target 与其源码保持不变 |
| Syncthing PC 伴随进程、`web/workbench` | 将 02 验证的完整 Syncthing 节点封装为 PC 伴随进程，并将唯一 Workbench 渲染模块纳入 nanoMuse 工程；迁移时更新调用、构建与检查 |
| `web/portal/src/routes/` 普通用户页面 | 将资源、通知、设置等能力迁入原客户端导航；每项迁完并核验后退出重复的正式用户构建。管理员页面留给 04 |
| `server/openapi/portal.yaml`、共享 fixture | 消费 01 的 Task 合同和 02 的 Workspace 文件合同并校验 TS/Swift；必要合同修正同步服务端及调用方，不把缺失的服务端业务留给客户端绕过 |

本文先停用融合版本的 iCloud 业务同步，并按用户确认删除旧模型 API key 的 Keychain 同步项；**个人同步、Hub、云端目标/例程、完整记录及其两端页面接入由 04 一起完成**。这些未完成能力不调用旧 Cloud、不显示假成功；除已确认清理的旧自带模型 API key 外，已有本地数据保留；原本机 Agent 执行入口按新需求停用。03 交接通过只代表主流程可用，不代表完整融合可发布。

### iOS 设计稿的实施分工

设计稿是本期 iOS 页面布局与交互的目标参照，HTML 中的账号、文件、远程控制和示例内容均是演示，不代表功能已实现。iOS 首屏为聊天，底部为“聊天、动态、点子、资源库”四栏；云端电脑从资源库或会话进度进入，不设“工作台”顶级菜单。PC 和现有网页的首页继续进入云端电脑。用户在会话中工作，进度、审批和结果属于会话；内部 Task ID 仅用于接口和验收，不呈现为独立任务页面。

| 页面与交互 | 阶段归属 |
|---|---|
| 聊天、会话侧栏、附件、处理进度、审批、结果及文件预览 | 03 的 T03～T05/T11 接入真实会话、文件和控制合同；显示受理、等待、保存、失败及未知状态 |
| 资源库、工作区文件、云端电脑详情和远程画面 | 03 的 T04/T06/T07/T11 使用 02 文件合同及获准 Host；常用电脑/工作区变更只影响后续发送，不改动正在处理的本轮 |
| 动态、点子、助手/个人数据、目标与例程、同步及记录设置 | 03 的 T11 完成设计中的导航和页面结构，对未接入能力明确提示；04 的 T02～T07 完成真实数据、操作与权限接入 |
| 全部页面与跨端流程 | 03/T09～T10 和 04/T11 分阶段操作检查；05 对照设计稿、真实状态及完整功能做整体验收 |

## 2. 接口与调用方式

下表的 Task 接口须由 01 实际交付，Workspace 文件接口须由 02 实际交付；名称以最终 OpenAPI 为准。拟新增的客户端适配与 IPC 在实施时登记到公共约定，不创建“手机专用任务”或“PC 接续任务”接口。

| 接口/模块 | 客户端变化及约束 | 任务 |
|---|---|---|
| `/api/capabilities` 与现有账号/安装接口 | 提交密码前检查版本；处理待审、停用、撤销和服务不兼容；服务实例、账号、安装分别关联缓存与凭证。不回退验证码或独立 Cloud。`task_api` 表示对话/Task API 已装配、可受理排队任务；`cloud_tasks` 表示 API 与云端执行控制均已装配。旧客户端仅返回 `cloud_tasks` 时兼容推断 Task API 可用 | F03-T02、F03-T09.03 |
| `/api/auth/client-login` 有序重试合同 | `ordered_client_login` 能力要求、`client_instance_id`、单调 `attempt_generation` 和每次尝试的新 Idempotency-Key 已同步 OpenAPI/服务端/PC/iOS；已提交代次围栏阻止旧请求轮换凭证，撤销安装不被登录流复活；旧 feature 不再发布。为遵守 Android 源码不变范围，服务器另保留不宣传能力的旧 Android 到达顺序兼容入口，不提供迟到意图围栏 | F03-T02.05～.07 |
| `/api/available-models`、`model-connections` | 客户端选择 Morde 已配置的云端模型；移除用户自带模型密钥设置和使用，已有设备 key 按 F03-T08 直接清理，不上传 | F03-T03 |
| `/api/conversations`、消息提交及 `/api/tasks/*` | 提交前持久化请求幂等键、输入版本与确认摘要；回执含 Task ID 后才提示可关闭。补充/审批/控制携带预期版本；新目标不改变在途任务 | F03-T03～T05 |
| Task 快照与事件续读 | PC/iOS 共同处理顺序、重复、断线和 cursor 过期；本轮已为 `GET /api/tasks/{id}` 增加与快照同一读取时点的 `event_sequence`，流式正文快照另按 F03-T04.07 验收。页面销毁只结束订阅，不得调用停止、退出或撤权 | F03-T04.06、F03-T04.07、F03-T04、F03-T05 |
| `/api/execution` 及授权的停止/恢复入口 | 使用 01 扩展后的账号 Runtime 归属、可用/控制状态与 Task 汇总；区分 Runtime、Task 和 Host 的状态及操作范围 | F03-T05、F03-T07 |
| Workspace 设备登记、同步状态、检查点与显式上传接口 | PC 显式选择目录后运行受控 Syncthing 节点；iOS 仍显式选文件上传/下载。只有 02 已确认的不可变检查点能作 Task 输入，文件超限、服务端落盘与 Host 就绪分别显示 | F03-T04、F03-T06、F03-T07 |
| Host、成员、control/actions、受权工作台会话 | 查看状态与实际控制分别鉴权；接管先等待停止确认，交还后刷新版本；Web 内容不持有长期 Client 凭证或本机桥接能力 | F03-T05、F03-T07 |
| 通知、`/api/clients`、API Key、用户设置 | 使用各自现有合同及 Client 允许范围；安装撤销与任务停止分开展示。错误不得把无权限/版本缺失当作空列表 | F03-T02、F03-T07 |
| **拟新增/调整** TS、Swift Morde 适配与本机 IPC | 复用原客户端 API 层，统一错误/版本 fixture；目录选择、凭证读取和同步控制由受信任原生层处理，校验调用来源、参数与路径 | F03-T02、F03-T06、F03-T07 |

## 3. 可执行任务

> **执行方式：** 原编号保留作完整范围和总验收。每个会话按其独占文件边界推进；实现完成标待验证，原项全部实现和独立验收通过后才算完成。

> **依赖顺序说明：** F03-T06.01 先完成目录检查，F03-T06.04 据此接入保全，F03-T06.01 的完整恢复验收仍等待 F03-T06.04 与 F02-T10。F03-T05.04 不再依赖包含它自身的 F03-T10 汇总；最终跨端验收仍由 F03-T10 执行。

> **本轮 MVP（2026-11-01）：** 先做不依赖文件同步的纯文本闭环：恢复 PC/iOS 原工程入口，接入 Morde 能力检查与登录，iOS 提交云端消息并恢复 Task，PC 查看同一会话/Task，Task 快照带事件序号。真实文件上传下载、Workspace 同步、电脑接管、完整导航和阶段交接继续按下方依赖验收，不因 MVP 通过而提前勾选。

> **本轮进展（2026-11-01）：** PC 登录前能力/最低版本预检、Portal 纯文本 Task 页面有本地测试；Task API、OpenAPI、TS schema 的事件序号已实现并通过 API 与临时 PostgreSQL 验证。另有 PostgreSQL 集成测试验证 iOS Client 提交并幂等重试后，PC 安装绑定 Web Session 与 iOS 可读取同一 Task、会话消息和快照序号。内置浏览器在隔离本机 mock 上完成登录、选择模型、提交纯文本、刷新后重新打开同一会话，确认消息和排队状态可见；mock 明确提示服务未启用执行，因此不能代替真实云端执行验收。iOS 的 Morde 登录/能力预检、纯文本 Task、快照序号续读和最近会话恢复先由设置中的 `MordeEmbeddedView` 接入 Minis；现已新增 `MordeConversationRoom` 并接入四栏主聊天页，设置页保留完整资源视图，两处共享客户端状态。提交重试可跨进程保存非秘密幂等键和请求指纹。登录重试已新增服务端持久代次围栏；PostgreSQL 测试覆盖丢响应、不同 key 续登、A/B 反序、撤销竞态及未升级 Android 的同键重试/续期兼容。PC 登录和账号隔离测试 53 项通过。iOS 登录与账号隔离 helper 测试、Swift parser 与原生通用构建通过；模拟器仍因本机 CoreSimulator 版本落后无法启动。F03-T04.03 已加入服务端 Task 等待原因的中文说明、通知类型中文标题，并确保打开通知后仍聚焦通知关联的 Task；Swift helper、Host 导航测试和原 Minis 通用设备构建通过。F03-T05.02 已补审批/澄清重试复用幂等键和版本冲突刷新提示；Portal 全量 167 项、类型检查、ESLint、Task API handler 与临时 PostgreSQL 幂等/并发测试通过，真实跨端 UI 仍待 T05.01 联验。F03-T05.03 已补 iOS 审批/澄清幂等动作指纹、冲突后刷新提示及失败时保留澄清答案；Morde credential/action helper 与 Host 导航脚本通过，Morde 通用 iOS 设备构建退出 0。模拟器与真实服务验收仍待环境可用。指定测试站最新复验结果见下文：管理端可登录，`/api/capabilities` 为 404；未录入模型 key 或创建任务。MVP 本地代码与 mock 流已具备，真实服务登录、云端执行和跨端联验仍未结项。
> **本轮增量（2026-11-01）：** 本机临时 PostgreSQL 17 容器重跑 F03-T05.01 跨端测试通过；先由 PC Web Session 启用账号执行后，iOS Client 提交及幂等重试成功，两端读取同一 Task、事件序号、会话消息和任务列表。相关 client-login / legacy Android PostgreSQL 用例全通过。修正已撤销安装的登录错误映射，避免无效凭证被 HTTP 层误报为 500；修复 Workspace sync node 重启后重新检查 Syncthing UID 目录的信任规则。站点文件/目录冲突保留底层原因、Host 活跃同步测试按双向合同运行，socket 测试使用系统允许的短 Unix 路径。macOS `go test ./internal/app -count=1` 与 `go test ./internal/config -count=1` 通过，openat2 配额用例明确跳过。只读 Linux 容器运行 server 全量 `go test ./...` 和 `go test -race ./...` 均通过；全量 PostgreSQL 集成因未设置 `CH_TEST_DATABASE_URL` 按规则跳过，相关跨端/登录用例另在临时 PostgreSQL 17 容器通过。macOS 原生全量套件仍受 openat2、UID/GID 目录权限限制。真实服务、模拟器和跨端 UI 仍未验收。

> **MVP 测试站复验（2026-11-01）：** 指定测试站的 `/admin/login` 可正常展示，使用用户提供的测试管理员账号登录成功后已退出；PC Assistant 显示无可用模型，`GET /api/capabilities` 返回 404 `not_found`。因此当前部署的 Portal 管理界面可用，但不提供工作树所需的客户端能力接口，无法据此验收 Morde Client 登录或 Task 闭环。未录入模型 API key、未创建任务、未发送文件。

> **MVP 本机真实服务探测（2026-11-01）：** 用当前工作树编译的 Linux arm64 server 在临时容器中连接无持久数据卷的 PostgreSQL 17；API 仅监听容器内 `127.0.0.1`，未映射到主机或公网。`GET /api/capabilities` 返回 200（API v1，包含 `task_api`），`GET /healthz` 返回 200；`GET /readyz` 返回 503，因配置的本机 OpenSandbox `127.0.0.1:18080` 未运行。云执行关闭、没有模型连接，因此该探测只验证当前 server 启动与能力响应，不证明模型执行或 PC/iOS UI 联验。测试结束已删除临时服务、数据库容器和数据。

> **MVP 状态（2026-11-01）：** 本地 PC/iOS 纯文本链路实现、Task 事件序号合同、账号/服务实例隔离及恢复逻辑已完成；Portal 测试、mock 浏览器流程、iOS generic build 和 PostgreSQL 17 跨端 Task/消息/序号集成测试通过。MVP 真实运行验收未完成：指定部署缺客户端能力接口和可用模型，本机模拟器不可用，当前本机服务也未启用 OpenSandbox/云执行。相关项继续保持待验状态，不把本地 mock 或数据库合同测试记作真实云端执行。

> **F03-T05.05 本地进展（2026-11-01）：** Portal Assistant 新增版本化 Task stop action 和幂等重试；已受理但未完成的停止只展示等待服务端回执及待核对操作数。Morde iOS 已有相同 cancel action 与 pending/completed/unknown 状态标签。Portal 全量测试 25 个文件/172 项及 2 个脚本、类型检查、相关 ESLint、TaskAction API 定向测试通过；本机 mock 浏览器确认 queued→cancelling 后显示“正在等待服务端确认停止结果”并隐藏停止按钮。真实 PC/iOS 服务回执、应用关闭边界与跨端联验待环境可用后完成。

> **F03-T03.03 本地进展（2026-11-01）：** Portal Assistant 已加入 Host/Workspace 选择，并单独显示账号云端 Agent Runtime 状态。只从当前账号可访问的 Host 列表选择；默认 Host 不可访问时显示提示且不自动附加，手动选中的 Host 无法核验、未运行或非 AI 控制，或 Workspace 未健康应用到当前版本时禁止提交；刷新后已选 Workspace 若不再可访问，保留待核验状态并禁用提交。提交携带 Host、Workspace 与固定 input revision；已受理 Task 显示服务端回传的固定目标，切换选择器只影响下一条消息。MSW 已补回显目标和输入版本。Portal 全量测试 25 个文件/180 项及 2 个脚本、类型检查、全量 ESLint、`git diff --check` 通过；内置浏览器本地 mock 验证选中运行中的 Host、已应用 Workspace 后提交，Task 显示固定 Workspace v0。越权目标服务端拒绝与真实任务联验仍待 Morde 服务环境。

> **F03-T03.01/03.04 本地进展（2026-11-01）：** PC Morde WebView 认证后及登录恢复现在进入同源 `/assistant`，保留跨源导航拦截；桌面路径测试 2 项和 TypeScript 检查通过。Portal Assistant 仅列出已检查且支持文本的 Morde 模型并按选择提交 `model_id`。iOS Morde 现可从客户端获准 Host/Workspace 列表选择目标，校验运行/AI 控制、同步健康与应用版本后提交固定目标；已受理 Task 显示服务端目标。`GET /api/execution` 不向 native client credential 开放，因此 iOS 明确显示“当前客户端无权读取”，不从 Host 状态推断。iOS 登录/目标 helper、导航脚本和 standalone/Minis 通用设备构建通过；真实权限联验及模拟器验收仍待服务环境与新版 CoreSimulator。

- **（基线未完成）** **F03-T01（进行中，汇总）恢复原 PC/iOS 工程及客户端基线**
  - 修改：桌面 `package.json`、`src/main.ts`、启动/测试/打包配置及适用插件注册；iOS 原工程、target membership、`MinisApp.swift`、资源和扩展。正式客户端的构建产物和启动链不承载、不启动本机 Agent Runtime、模型/工具循环、本机任务执行器或子 Agent。原 `Minis.xcodeproj/project.pbxproj`、workspace 与 scheme 已从本地历史恢复，Minis 通用 iOS 设备构建已通过；模拟器无法启动及原客户端本机 Agent 清理仍待验收。不能用精简 `Morde.xcodeproj` 替代原工程。检查相应 AGENTS 与构建说明。
  - 验收：本地可打开原界面并连接云端；逐平台记录页面、技能、连接器、记忆、目标、例程、模型、分享/文件入口的迁移去向及“可用/受平台限制/待验证”证据。检查打包依赖、启动、恢复和扩展入口均不承载或运行本机 Agent。不把源码存在当作功能通过；构建仍覆盖原测试而非仅 `morde*.test.mjs`。
  - 边界：显示名称为 Morde。R3 未决定前采用不会覆盖旧数据的本地测试安装；不擅改正式 Bundle ID、AppGroup、Keychain group、PC appId 或更新渠道。

  - **（基线未完成）** **F03-T01.01（进行中）恢复 PC 原工程入口**：修改：桌面 package、主进程和打包配置恢复原 nanoMuse 入口；使用隔离测试安装，不更改正式 appId；验收：本地构建并打开原界面，启动链不启动本机 Agent。当前证据：`MORDE_TEST_USER_DATA` 可将本地验收数据写入独立绝对路径，未设置仍用默认 `Morde Client`，正式 appId 不变；桌面完整 TypeScript 检查和 `npm test` 212/212 通过。本机启动当前工作区 Electron 后，在内置应用窗口输入不可达 localhost 地址，能力预检提示“未发送账号信息”；隔离资料已退出，未提交账号密码。PC 仍嵌入 Portal `/assistant`，原 nanoMuse/dsh 界面尚未接入；真实服务登录和跨端页面验收仍待环境。
  - **（基线未完成）** **F03-T01.02（本地原工程/target 已恢复且通用设备构建通过，待模拟器/真机打开验收）恢复 iOS 原工程入口**：工作树保留 `Minis.xcodeproj` 原 Minis target 与 `MinisApp.swift → NanoMuseRoot` 入口，并通过当前 Xcode 的 generic iOS `xcodebuild build`；独立 Morde target 也已可构建。模拟器无法启动，本机 CoreSimulator 1174.9.2 低于当前 Xcode 要求 1174.21.0；真实设备、签名和原界面交互仍待验，本项保持未完成。
    - 本地构建记录：已从 `9eb70568` 恢复原工程、workspace 与 scheme；Xcode 27.2 离线解析并列出 6 个 target 成功。修复 `android/deps/prepare_alpine_rootfs.sh` 的 macOS 文件元数据漏打包问题后，生成的 ZIP 与 rootfs 逐项核对为 523/523；本地生成 iSH、FFmpeg、RootfsPatch 与 Rclone 依赖。加入 Morde 共享视图、关闭 iOS 本机 Agent 执行入口后的 Minis 通用 iOS 设备构建均退出 0 并产出 `Minis.app`。原界面尚未启动：本机 CoreSimulator 1174.9.2 低于 Xcode 所需 1174.21.0，模拟器设备支持被禁用。临时 SwiftPM 锁在最终清理。
  - **（基线未完成）** **F03-T01.03（待验证：源码去向盘点完成，依赖 F03-T01.01）核对 PC 原功能与数据去向**：原客户端注册于 `harness/dsh-nanomuse/src/client/index.ts`：Feed/Ideas/Goals/Library/Devices、Models、Connectors、Files、Permissions；记忆与本机数据分别见 `ProfileDrawer.tsx`、`DocEditor.tsx`、`Memory.tsx`、`prefs.ts`/`Sections.tsx`；Goals/Feed routine 见 `GoalsPanel.tsx`/`FeedPanel.tsx`；分享与 Library 见 `AvatarShare.tsx`/`LibraryPanel.tsx`。当前 Morde 仅提供登录、Workspace 同步和 Portal `/assistant`；原 skills 页面未见独立实现，其余上述原页面、记忆和 Rooms/profile 数据未迁入。静态位置盘点完成；原 PC 界面实际入口仍待 F03-T01.01。
  - **（基线未完成）** **F03-T01.04（待验证：源码与 target 去向盘点完成，依赖 F03-T01.02）核对 iOS 原功能与扩展去向**：`MinisApp.swift` 仍以 `NanoMuseRoot` 为根；Feed/Ideas/Goals/Library、Chat、系统 files/settings 等路由见 `NanoMuseShell.swift` 与 `NanoMuse*` 页面。Morde `MordeConversationRoom` 已接入 `NanoMuseShell` 四栏主聊天，`MordeEmbeddedView` 保留在 `NanoMuseSettings.swift`；独立 `MordeStandaloneApp.swift` 仅属 Morde standalone target。Minis target 仍含 Agent/AppIntents/iSH；Share Extension、File Provider、Widget/Live Activity 及其 App Group 数据各自保留原入口。`ChatStore`/CloudKit、provider config/Keychain、routine/goals、iSH rootfs 与 Morde scoped auth/outbox 的存储彼此分离，未发现旧会话数据迁入。源码及 membership 盘点完成；设备路由、签名/entitlement 与实际迁移仍待验；不修改 Android。
  - **（基线已完成）** **F03-T01.05a（完成，依赖 F03-T01.01）核对 PC 不运行本机 Agent**：桌面入口固定为 `out/morde-main.js`；主进程只恢复 Morde 会话、打开同源 Portal `/assistant`，唯一子进程是明确受控的 Workspace 同步节点。`electron-builder.yml` 的应用文件白名单只含 `out/morde-*.js` 和 Morde UI，额外资源只含 `morde-sync` 与许可证；旧 nanoMuse/dsh 主进程、模型与工具循环不进入 Morde 安装包。本机临时目录构建 Linux x64 unpacked app 后检查 `.asar`，仅含 Morde 主进程/界面/同步适配；包内 `morde-sync` 与 SHA-256 清单匹配。桌面全套原有及 Morde 测试 212 项通过；临时构建产物已删除，既有 `dist`、`out` 未清理。
  - **（基线未完成）** **F03-T01.05b（本机执行代码围栏完成，待 target 清理及设备验收；依赖 F03-T01.02）核对 iOS 不运行本机 Agent**：`MordeLocalExecutionPolicy` 默认关闭，在聊天发送/重试/恢复/队列/压缩/标题生成、Agent 与 LLM provider、Hub 收到的本机 task、routine/后台任务、模型及浏览器 native offload、模型 quick test/debug RPC、旧 provider 模型列表刷新入口 fail closed；失败时提示改用 Morde 云端任务，并保留未发送输入。旧图片生成的 relay/DashScope 路径、视频生成/模型探测也在发请求前 fail closed。Shortcuts provider 仅在显式 `MORDE_LOCAL_EXECUTION_ENABLED` 编译条件下加入，普通构建不注册 Siri shortcuts；5 个仍可被调用的 AppIntent 现在在会话、附件、后台任务和通知副作用前调用 `requireEnabled()` 并直接抛出本机执行已关闭的本地化错误。凭据 helper 测试验证错误类型，错误文案覆盖 9 种语言；Minis 通用 iOS device build 退出 0。**未完成**：Minis target 仍编译 Agent/provider/tool/iSH executor 与旧媒体代码并携带 rootfs/扩展；需后续按 target membership 与资源白名单去除，不能将源代码仍在包内记作已清理。定向清理旧模型 Keychain、媒体云端迁移另属 T03/T08，尚未完成。模拟器因本机 CoreSimulator 版本过旧无法启动；设备运行、签名/entitlement 与扩展行为未验收，故本项保持进行中。

- **（基线未完成）** **F03-T02（进行中，汇总）统一账号、能力检查与账号数据隔离**
  - 修改：桌面 `src` 中凭证/登录模块、插件 `src/cloud.ts`、`src/client/AccountPage.tsx`、`CloudSection.tsx`；iOS `NanoMuseCloud.swift`、`NanoMuseCloudHeaders.swift`、`NanoMuseAccount.swift`、`NanoMuseAccountView.swift`、`NanoMuseAccountData.swift`。适配 Morde 密码注册/登录、管理员审核、安装与 Web 会话凭证，更新 DTO、错误提示和安全存储。
  - 验收：相同实际 base URL 下登录、资源、模型和任务请求可用；能力接口缺失时提示版本不支持并保护本地数据，云端功能显示不可用。换账号不显示或发送前账号的缓存/outbox；退出撤销该安装授权、清理凭证并隔离保留业务数据，不走旧默认清空逻辑。离线退出先隔离数据，仅保留待撤销安装的非秘密标识，显示服务端撤销尚未确认；联网或其他有权端完成撤销后再确认全局结果。

  - **（基线未完成）** **F03-T02.01（部分验证，依赖 F03-T01.01）接入 PC 登录和能力检查**：修改：桌面凭证、登录及插件账号页面接 Morde，密码提交前检查能力和版本；验收：本地实际登录，待审、禁用和不兼容明确提示；凭证安全保存。当前证据：`npm test` 42 项通过，包含能力缺失/最低版本检查；本机 Electron 窗口通过 loopback mock 实际完成能力查询、合成账号登录并加载 `/assistant`，登录页 Tab 顺序和空字段校验可见。请求只到 `127.0.0.1`，不代表真实服务、待审/禁用账号及真实权限验收；这些服务联验仍待进行，故本项未勾选。
  - **（基线未完成）** **F03-T02.02（本地代码完成，待真实服务与模拟器验收；依赖 F03-T01.02）接入 iOS 登录和能力检查**：Morde 登录/能力预检可从原 Minis 四栏主聊天页和设置页进入，Keychain 凭证使用独立 service；登录前校验服务实例、API/客户端登录能力和最低版本，避免旧服务收到密码。原 Minis generic iOS device `build-for-testing` 通过；冷启动会预检已保存的 Morde 安装，服务实例校验通过后允许 Morde-only 用户进入四栏主界面；服务端能力预检暂时失败时保留已保存凭据。新增首次启动路由用例已编入完整 `MinisTests` 构建，因 CoreSimulator 版本不兼容尚不能在模拟器运行。待审、禁用、不兼容的真实服务提示和模拟器登录仍待验证；原 NanoMuseCloud 账号入口继续保留为独立旧服务。指定测试站管理端可登录，但 PC Assistant 无可用模型且 `GET /api/capabilities` 为 404 `not_found`；这不证明 Morde iOS Client 登录合同可用。未向该站录入模型 key 或提交 Task。本机当前 server 的能力接口返回 200、`healthz` 返回 200，但 `readyz` 因 OpenSandbox 不可用返回 503，未做客户端登录、模型请求或 Task 创建。
  - **（基线已完成）** **F03-T02.03（完成）隔离 PC 账号数据和退出撤权**：桌面 profile key 含 service instance、账号、installation；Workspace checkpoint 带服务/安装校验。旧的无身份 profile 文件保留但不再读取；在线登出请求服务端撤销，离线时优先清本机授权和会话状态，持久化非秘密 pending-revoke 标识并提示尚未确认，不删除用户工作目录。`harness/desktop` 的 npm 测试 53 项通过，覆盖同 URL 服务实例变化、安装切换、重启及离线退出；diff check 通过。旧 nanoMuse legacy account route 尚未迁入，真实登录待 T02.01 联验。
  - **（基线已完成）** **F03-T02.04（本地完成，依赖 F03-T02.02）隔离 iOS 账号数据和退出撤权**：凭证、最近会话与 Task 重试数据按 origin、服务实例、账号和安装隔离；服务实例或账号不符时不复用旧凭证；在线登出请求撤销，离线时先清本机授权、轮换客户端实例 ID，并在重启后保留未确认撤销提示，业务缓存不删除。`run-credential-expiry-tests.sh`、`run-host-navigation-tests.sh` 与 Minis 通用 iOS 构建通过；模拟器交互验收仍受本机 CoreSimulator 版本限制。集成登录页和真实服务联验仍由 T02.02 验收。
  - **（基线已完成）** **F03-T02.05（完成）补齐登录重试的服务端顺序围栏**：新增迁移 `00089_ordered_client_login.sql` 持久化账号、实例 UUID、最新代次和唯一安装绑定，不存密码或令牌；能力列表改为 `ordered_client_login`，不再发布旧登录 feature。桌面/iOS 缺少有序字段时拒绝。为保持 Android 源码不变，旧 Android 可省略两个有序字段走未宣传的到达顺序兼容流；同键重试会在原安装轮换凭证，但不保证客户端意图顺序。Store 在事务中校验有序客户端严格递增代次，拒绝重复 key、旧代次、其他 stream 绑定和已撤销安装；撤销与登录竞态不会复活凭证。验收：API 与 PostgreSQL 定向测试通过，覆盖丢响应后新 key、不同 key 续登、B(gen 3) 先于 A(gen 2)、同 key 并发、登出后延迟登录、并发撤销及旧 Android 同键重试/续期；相关 `go test -race` 也通过。`make generate` 与 OpenAPI TS schema 生成通过。本轮在临时 PostgreSQL 17 上重跑 `TestClient*`/`TestLegacyAndroid*` 全部通过，并修正撤销安装时原始凭证错误导致 HTTP 500 的映射，现返回未认证。只读 Linux 容器的 server 全量 `go test ./...` 与 `go test -race ./...` 均通过；macOS 原生受 openat2 与 UID/GID 文件系统差异影响。本轮未重跑 `make cover` 与 `make lint`。
  - **（基线已完成）** **F03-T02.06（完成，依赖 F03-T02.01、F03-T02.05）PC 接入可靠登录重试**：桌面主进程按服务与账号持久化非秘密 client instance 与精确 int64 代次，每次网络尝试先递增代次并使用新幂等键；先检查 `password_login`、`client_installations`、`ordered_client_login`，缺能力不发送密码。只有当前代次响应能写凭证，迟到成功/失败响应丢弃；重启续登使用匹配安装，登出退役旧 instance。`harness/desktop` 的 `npm test` 构建及 53 项通过，覆盖重启、乱序、能力拒绝、int64、登出及本地 fence 写失败；diff check 通过。真实服务登录仍由 T02.01 联验。
  - **（基线未完成）** **F03-T02.07（待模拟器验证，依赖 F03-T02.02、F03-T02.05）iOS 接入可靠登录重试**：已在原生登录层加入同一服务端合同，按服务 origin+账号持久化 client instance 与递增代次，每次请求用新幂等键；跨 `MordeClient` 实例共享锁与 intent，只有最新 intent 才写 Keychain；登出退役旧 ID。能力缺失会在提交密码前拒绝。Swift login fence/helper 测试、`git diff --check` 与 Minis 通用设备构建通过；模拟器超时/重启/乱序 UI 验收仍受本机 CoreSimulator 版本落后阻塞，故本项未勾选。

- **（基线未完成）** **F03-T03（进行中，汇总）加入模型、Host 目标和上下文确认**
  - 修改：原 PC 聊天/模型面板与 iOS 聊天/模型选择逻辑接入 Morde；显示账号级云端 Runtime、可选 Host、Workspace、可用云端模型及所需依赖。移除本机/云端执行切换、本机 Agent 模型调用及用户自带模型密钥设置/使用入口，Hub 不委派客户端执行 Agent。
  - 验收：同一会话的新 Task 可切换获准模型和 Host，正在运行的 Task 绑定不变；云端提交只使用 Morde 管理的可用模型和服务端获准的工具/文件，设备私有文件、旧模型密钥或本机连接器不能成为隐含依赖。本机内容上传先预览并确认范围/摘要；换模型、目标或上下文时重新校验。

  - **（基线未完成）** **F03-T03.01（待验证：本地模型选择与桌面入口已接，真实服务校验待环境，依赖 F03-T02.01）接入 PC 云端模型选择**：Morde 桌面认证后及恢复时载入同源 `/assistant`；Portal Assistant 从 Morde 模型连接读取仅已检查且支持文本的模型，新 Task 与续聊均提交所选 `model_id`，没有有效模型时禁用发送。桌面 WebView 路径测试 2 项、TypeScript 检查通过；Portal 组件测试包含模型切换及请求体断言，当前全量 25 个文件/180 项及 2 个脚本通过。真实服务的账号/模型权限提示与嵌入窗口渲染仍待联验。旧 `dsh-nanomuse` 代码仍有独立自带 key 页面，但当前 Morde WebView 不加载该本机客户端；其迁移/清理不在本项已验证范围。
  - **（基线未完成）** **F03-T03.02（本地选择器实现完成，待模型连接、权限和模拟器验收，依赖 F03-T02.02）接入 iOS 云端模型选择**：四栏主聊天页中的 Morde 会话可读取获准云端模型并用于纯文本 Task。当前本机 server API 已启动但无模型连接，OpenSandbox 未运行；指定测试站 PC Assistant 无可用模型且客户端能力接口为 404。未录入 GPT key，未提交模型请求；模拟器实际选择与服务端权限错误仍待验。原 NanoMuse 模型选择与本机调用的清理仍未完成。
  - **（基线未完成）** **F03-T03.03（待验证：本地实现完成，真实权限/任务目标联验待服务端，依赖 F03-T02.01）接入 PC 电脑和工作区目标**：Portal Assistant 按当前账号获准列表选择 Host/Workspace，并分别显示账号级云端 Agent Runtime 和 Host 状态；未核验、未运行、非 AI 控制的 Host、未健康应用到当前版本的 Workspace，或刷新后已不再可访问的 Workspace 均会阻止提交。新消息携带 Host、Workspace 与 input revision；已受理 Task 使用服务端回传的目标，不随选择器改变。MSW 保留目标字段。全量测试 25 个文件/180 项及 2 个脚本、类型检查、全量 ESLint、`git diff --check` 通过；内置浏览器本地 mock 验证 Host+Workspace 提交后固定目标显示为项目工作区 v0。服务端越权拒绝、真实账号数据与 Task 目标回读仍待服务环境验收。
  - **（基线未完成）** **F03-T03.04（待验证：本地实现完成，真实 API 权限与模拟器联验待环境，依赖 F03-T02.02）接入 iOS 电脑和工作区目标**：iOS Morde 任务入口读取当前客户端获准 Host/Workspace；Host 必须运行且处于 AI 控制，Workspace 必须健康应用至当前版本；列表未加载、权限消失或目标未就绪均不提交。请求携带 Host、Workspace 与 input revision，已受理 Task 显示固定目标，选择器只影响后续提交。native client credential 无权读取账号 Runtime，UI 明确显示“当前客户端无权读取”，不从 Host 状态推断；无 Host 仍可提交纯文本任务。`run-credential-expiry-tests.sh`、`run-host-navigation-tests.sh` 及 standalone/原 Minis 通用设备构建通过。服务端权限拒绝与模拟器操作仍待验；本机 CoreSimulator 1174.9.2 低于 Xcode 所需 1174.21.0。
  - **（基线未完成）** **F03-T03.05（未开始，依赖 F02-T10）确认本机上下文和文件范围**：修改：PC/iOS 对用户选择的文件显示范围和摘要，使用已确认检查点；改变选择时重新校验；验收：取消不上传；未完成上传或未确认检查点不能提交相关 Task。

- **（基线未完成）** **F03-T04（进行中，汇总）完成 iOS 云端提交与会话内进度**
  - 修改：在原 `NanoMuse/` 与聊天入口增加 Morde 任务客户端，接 02 的显式文件上传/下载和已确认检查点、请求持久化、提交回执、会话内本轮进度、事件续读与通知导航。所有 Agent 会话均不启动手机 `AIChatViewModel` 的本机执行循环，不依赖 `BackgroundTask` 保活云端执行。本轮 MVP 已将 Morde 会话接入 Minis 四栏主聊天，设置页保留完整资源入口；旧本机 Agent 执行代码仍在 target 中并 fail-closed，需后续按 F03-T01.05b 清理，文件工作流仍待 F02-T10 合同。
  - 验收：上传未完成不能启动；响应丢失重试返回原 Task；300 MiB/5 GiB 超限、撤权、冲突给出明确状态，下载核对摘要。只有持久受理后显示“云端已接收，可关闭应用”；无 Host 且无文件产物的纯模型任务可正常完成。关闭页面、后台、杀进程、断网均不隐式取消；重开后恢复同一 Task。会话内区别运行、等待审批/电脑、文件保存中、完成和停止未确认，不以最后一段模型文字判定完成。

  - **（基线未完成）** **F03-T04.01（待验证：本地实现完成，真实回执丢失联验待本地服务，依赖 F03-T02.02、F03-T03.02）提交无文件的 iOS 云端消息**：Morde 云端 Conversation/Task 已接入四栏主聊天页，设置中的完整 Morde 页面保留；主聊天与设置共用客户端状态，由主聊天负责恢复和 SSE 监听，可提交纯文本。非秘密幂等键与服务/账号/请求 SHA-256 指纹已按 UserDefaults 跨进程保存，原始任务文本不存入指纹记录。关闭 App 后尚未持久化的草稿内容仍需用户重输才能重试；真实服务回执丢失验证未完成。验收仍要求重试只创建一个 Task，并仅在持久受理后提示可关闭。
  - **（基线未完成）** **F03-T04.02（待验证，依赖 F03-T04.01、F03-T04.06）恢复 iOS 会话进度和事件**：iOS 接入带同一快照序号的任务详情、事件续读与断线恢复；最近会话 ID 按服务地址和账号 ID 保存，恢复前确认仍属于当前账号，退出/切服务/切账号清理旧选择。Swift helper 与原 Minis 集成构建通过；真实 SSE 断线、cursor 过期及重开后恢复同一 Task 待本地服务/设备验证。流式正文未就绪时不伪造中间文本，恢复边界归 T04.07。
  - **（基线未完成）** **F03-T04.03（本地实现完成，待模拟器验收；依赖 F03-T04.02）接入 iOS 进度和通知入口**：Task 排队、运行、审批/澄清/电脑同步/工作区变化/操作结果待核对、暂停、停止待确认、完成、失败、取消与未知状态均映射为中文；通知类型也显示中文。打开通知后先载入其会话，再恢复通知关联的 Task，避免显示会话中另一条较新的 Task。Helper 与 Host 导航脚本测试通过，原 Minis 通用 iOS 设备构建退出 0；模拟器仍无法使用（CoreSimulator 1174.9.2 低于 Xcode 要求 1174.21.0），故实际状态操作和通知点击保持待模拟器验收。所有状态来自服务端字段，不以结果文本推断。
  - **（基线未完成）** **F03-T04.04（未开始，依赖 F03-T04.01、F02-T10）接入 iOS 显式文件上传**：修改：使用 02 确认的上传合同、限额及不可变检查点后再提交含文件的请求；验收：未上传完成不能启动；300 MiB/5 GiB、撤权与冲突状态正确。
  - **（基线未完成）** **F03-T04.05（未开始，依赖 F03-T04.04、F02-T10）接入 iOS 文件下载与预览**：修改：iOS 按权限下载和预览已确认检查点内容；验收：读取产物并核对摘要；无权或下载不完整不显示成功。
  - **（基线已完成）** **F03-T04.06（完成）补齐 Task 快照与事件序号合同**：`server/internal/api/tasks.go` 的 Task 详情返回与快照同一读取时点的 `event_sequence`，同步 OpenAPI、TS schema 和 Swift Task DTO/续读游标。API 路由单测、PostgreSQL 16 临时数据库快照并发验证、Portal/Workbench 两处 `npm run typecheck`、Swift helper 测试及 standalone Morde 与原 Minis target 通用 iOS 构建通过。模拟器运行和真实服务 SSE 联验仍留给相应验收项。
  - **（基线已完成）** **F03-T04.07（完成：本地实现与合同验证，依赖 F03-T04.06）补齐消息恢复边界**：云端 dsh 的实际输出采用流式 delta；服务端在唯一 Task 消息路径保存有界 assistant 快照，delta 与事件序号同事务提交，最终消息复用稳定 ID 并只落 canonical 会话消息一次。Task 详情从同一 repeatable-read 快照返回消息正文、关联 ID 与 `streaming`/`completed`/`interrupted` 状态；失败、预算等待、取消和租约恢复不会把未完成正文标成完成。Portal 与 iOS 按稳定消息 ID 合并快照和会话历史，canonical 最终消息优先，避免历史过期或刷新重复。PostgreSQL 16 本地并发测试验证读取快照与新 delta/最终提交同时发生时，事件序号、正文及状态保持一致；另覆盖中断恢复、非流式完成及 32 KiB 上限。API、Portal 和 iOS fixture/helper 测试，Portal 全量 Vitest（25 文件/192 项及 2 个 Node 脚本）、类型检查、相关 ESLint、Go API 测试、iOS helper 测试、Swift parser 与 Minis 通用 iOS 设备构建均通过。真实服务运行、SSE 断线及模拟器交互仍按 T09/T10 验收。

- **（基线未完成）** **F03-T05（进行中，汇总）完成 PC 接续、审批和人工接管**
  - 修改：原 PC 会话列表及会话内进度接账号 Runtime 状态、Task 快照/事件、产物、补充输入、审批、停止与 Host 控制；iOS 同步接入共用动作语义。区分“查看结果”“继续对话”“接管电脑”“停止当前处理”，展示接管转换及撤权结果。
  - 验收：手机离线后 PC 用自己的凭证看到同一 ID/版本；打开页面不创建或重跑任务，PC/iOS 均不启动本机 Agent。重复审批只有一次生效，版本冲突刷新；接管先停相关 AI 工具再开放人工控制，交还读取新文件版本；停止未知时禁止伪装已停止。

  - **（基线未完成）** **F03-T05.01（本地实现完成，待真实服务联验，依赖 F03-T02.01、F03-T04.06）PC 查看同一云端任务**：桌面 Morde WebView 认证后、登录恢复及管理员页回退均载入同源 Portal `/assistant`；主进程路径测试覆盖加载、恢复和跨源拦截，共 2 项。Assistant 按服务实例 ID 与账号 ID 在 localStorage 保存最近会话 ID，并仅在当前账号会话列表中命中后恢复；开始新对话会清除恢复指针。新增恢复、跨账号、跨服务实例隔离及新对话清除用例。本轮 Portal 全量验证 27 个文件/205 项与 2 个 Node 测试、`npm run typecheck`、Assistant ESLint 均通过。内置浏览器 mock 实操“打开最近会话→刷新恢复同一 Task”及“开始新对话→刷新仍为空白”通过。组件测试覆盖纯文本提交、读取同一 Task 和丢响应重试；本机 PostgreSQL 17 集成测试通过：先由 PC Web Session 启用账号执行，再由 iOS Client 提交纯文本 Task 并重试；桌面安装绑定 Web Session 与 iOS 均读取到同一 Task、事件序号、会话消息和列表记录。指定测试站管理端可登录，但 PC Assistant 无可用模型且 `GET /api/capabilities` 为 404 `not_found`，当前部署不能验证 Morde Client 纯文本 Task；未录入模型 key 或创建任务。嵌入窗口渲染、跨端 UI 与 SSE 断线联验仍待提供当前 API 合同的服务环境。
  - **（基线未完成）** **F03-T05.02（待验证：本地实现完成，真实服务联验依赖 F03-T05.01）PC 接入补充与审批**：Assistant 按预期 approval/clarification 版本提交；同一请求与内容遇响应丢失会重用幂等键，后端 `conflict` 会重新读取 Task 并提示先核对最新状态。Assistant 组件测试覆盖批准/拒绝、澄清、两类动作的丢响应重试和版本冲突刷新提示；API client 测试断言两个操作均携带调用方幂等键。Portal 全量测试 25 个文件/167 项与 2 个脚本测试、`npm run typecheck`、相关 ESLint、Task API handler 定向测试通过。PostgreSQL 16 临时本地容器验证版本化审批、重复审批重放、两客户端并发仅一个生效、澄清重复回答和 stale version 冲突通过；临时容器已停止。页面级本地 mock 可操作，显示当前服务未启用执行；真实 API、账号和 PC/iOS 跨端确认仍待 T05.01 的服务环境。
  - **（基线未完成）** **F03-T05.03（待验证：本地实现完成，依赖 F03-T04.02 与模拟器验收）iOS 接入补充与审批**：iOS 按 pending/clarification 版本提交审批、拒绝与补充；动作请求体哈希后复用幂等键，不保留澄清原文；响应丢失时同请求重试沿用 key，409 时重读 Task 并提示核对，提交失败保留输入。`run-credential-expiry-tests.sh`（含动作指纹及 key 复用断言）、`run-host-navigation-tests.sh` 通过；Morde 通用 iOS 设备构建退出 0。仍需模拟器重复审批/版本冲突操作验证；本机 CoreSimulator 1174.9.2 低于 Xcode 要求 1174.21.0，当前无法启动模拟器。真实服务与跨端联验仍依赖 T05.01。
  - **（基线未完成）** **F03-T05.04（待验证：本地实现完成；依赖 F03-T05.01）PC 接入人工接管和交还**：桌面 Host 控制区分查看、接管与交还；接管/交还均等待服务端工具停止确认，pending 状态继续使用同一 request ID；AI 控制和控制转换期间标记工作台受限，转换完成后才开放相应操作。交还成功后刷新 Host 与 Workspace 查询以核对新版本。Portal 全量测试 25 个文件/176 项与 2 个脚本、`npm run typecheck`、相关 ESLint、`git diff --check` 通过；本地 mock 浏览器完成 AI→接管 pending→人工→交还 pending→AI。真实工具停止回执、Gateway 拒绝写操作与真实 Workspace 新版本联验仍待 T05.01/T10 的运行环境。
  - **（基线未完成）** **F03-T05.05（待验证：PC 本地实现完成，依赖 F03-T05.01、F03-T04.02）两端接入停止与未知状态**：PC Assistant 增加带 expected_status 与幂等键的停止请求，iOS 已接入 cancel action；两端均区分 cancelling、已确认 cancelled 与缺少回执的未知状态。服务端受理但仍有 unsettled operation 时只提示等待，不显示停止成功；关闭页面或退出安装不发送停止请求。Portal 全量测试 25 个文件/172 项与 2 个脚本、类型检查、相关 ESLint、Go TaskAction API 测试通过；MSW 测试覆盖重复请求、持久 pending 回执和 stale status 冲突；本地浏览器确认 queued 任务收到 mock `cancelling/pending` 后显示等待状态。真实服务回执、跨端状态及关闭/退出生命周期仍待 T05.01、T04.02 环境联验。

- **（基线未完成）** **F03-T06（进行中，汇总）把 Workspace 同步纳入原桌面构建**
  - 修改：将 02 固定版本的 Syncthing 完整节点随 PC 包构建；桌面受信主进程复用 `checkedDirectory` 和其调用方的系统目录/应用数据目录限制，管理目录映射、设备登记、受控本机 REST、同步开关、状态、退出清理和恢复，并提供本机安全副本的查看、恢复和额外磁盘占用提示。每次恢复前复核全部已启用目录及安全副本位置的重叠，不能只检查所选目录自身身份。renderer 不持有设备私钥、本机 REST key 或任意文件访问能力；iOS 显式上传归 F03-T04，不接常驻进程。旧 `morde-sync` 不作为新 PC Workspace 的并行写入者。
  - 验收：只在用户选定且开启的目录扫描；同目录不重复绑定，已有云端内容先显示差异；创建、修改、删除、空目录、冲突双向到达，离线重连补齐，超 300 MiB 排除和 5 GiB 暂停状态可见。同步覆盖或删除本机文件前保留安全副本，用户可查看和恢复；副本不自动上传、不计 5 GiB，额外磁盘占用可见，删除副本需另行确认，保全失败时显示暂停原因。PC 断网或停止同步时区分已确认云端版本和本机最新状态未知；已受理任务继续，重连且服务端确认外部变化后显示暂停并要求按新版本重新提交。关闭某 Workspace 开关只停该 folder，其他已开启 folder 继续；应用真正退出后无 PC 同步进程，重启获准恢复。PC 下载云端产物核对检查点摘要，敏感凭证不进 renderer/命令行日志。

  - **（基线未完成）** **F03-T06.01（实现部分完成，待 F03-T06.04、F02-T10 验收；依赖 F03-T02.01）验证本机目录映射和重叠限制**：桌面可信主进程已在选目录和每次同步启动前校验 realpath、系统目录、应用数据目录及其他 Workspace 映射重叠；目录选择等待期间复核授权与同步状态，并拒绝并发选择。桌面 `typecheck` 与全量测试 212/212 通过，路径/同步定向测试 11/11 通过；server `go test ./internal/app -count=1` 通过，macOS 跳过仅支持 Linux openat2 的 Workspace current 文件读验证。安全副本位置的重叠检查与恢复路径验收待 F03-T06.04/F02-T10；真实 F02 节点联验仍未完成。验收：选择和恢复时均拒绝重叠与无法确认路径；只扫描已选目录。
  - **（基线未完成）** **F03-T06.02（未开始，依赖 F03-T06.01、F02-T10）接入 PC 节点身份和受控进程**：修改：桌面构建接入 02 固定版本节点、设备登记和本机受限 REST；验收：授权设备可启动；密钥不进入 renderer、命令行或日志。
  - **（基线未完成）** **F03-T06.03（未开始，依赖 F03-T06.02、F02-T10）接入每个 Workspace 的同步开关**：修改：桌面管理各 folder 状态、进程退出与重启恢复；验收：关闭一个 folder 不影响其他 folder；真正退出无残留同步进程。
  - **（基线未完成）** **F03-T06.04（底层实现部分完成，PC/04 集成未完成；依赖 F02-T01，先完成 F03-T06.01 的目录映射检查）落实本机安全副本及失败关闭**：旧 `project/morde-sync/client/preserved.go` 已在所选 Workspace 的父目录 `.morde-preserved/<workspace>/<id>` 保存替换/删除前副本，提供列表与仅在原路径为空时恢复；副本在 Workspace 扫描树外。定向 `go test ./client -run 'Preserved|LiveReplacement|LiveDelete' -count=1` 通过，覆盖正常恢复、并发本地更改及保全失败。该 `project/morde-sync` 源码由桌面 `build-morde-sync.mjs` 编入新客户端二进制，`client/client.go` 的 live replacement/deletion 路径已调用上述保全逻辑；本轮已在本地加入副本列表/恢复/删除 stdio RPC 和部分桌面 IPC/UI 代码；`project/morde-sync` 的 `go test ./... -count=1` 通过，桌面集成与交互尚未验收。仍需对照 F02 最终同步节点合同核对唯一写入者、所有 Workspace 映射及 `.morde-preserved` 保全根的重叠和实际覆盖/删除失败关闭，因此本项保持未完成。验收：普通替换、删除及失败场景可恢复或暂停；副本不上传、不计同步额度。
  - **（基线未完成）** **F03-T06.05（进行中，依赖 F03-T06.04）提供安全副本查看和恢复**：正在接入桌面本地 stdio RPC 与 IPC/设置入口；离线时基于当前账号已保存的本机 Workspace mapping 查看和恢复。恢复遇到已有目标拒绝覆盖且保留归档；删除单份副本前二次确认并显示额外磁盘占用。仍待核对 RPC/IPC、所有 Workspace 与 `.morde-preserved` 路径边界，完成桌面测试和本机交互验收后再标完成。
  - **（基线未完成）** **F03-T06.06（未开始，依赖 F03-T06.03、F03-T06.05、F02-T10）联验 PC 同步与检查点**：修改：桌面状态接真实 02 节点、Host 与不可变检查点；验收：双向创建/修改/删除、冲突、空目录、离线恢复及限额状态正确；下载核对摘要。

- **（基线未完成）** **F03-T07（进行中，汇总）迁入云端电脑操作与普通用户页面**
  - 修改：将 `web/workbench` 迁为 nanoMuse 下唯一共享渲染模块，接 PC/iOS 原生容器和受权会话；逐项迁入 `web/portal/src/routes/` 的 Host、Workspace、成员/权限、通知、模型连接、安装、API Key 和设置能力。PC 首页进入云端电脑；iOS 按设计稿从资源库或会话进度进入电脑详情/远程画面，不新增“工作台”顶级菜单。PC 沿用适用组件，iOS 导航由 T11 重整；更新资源和打包路径。
  - 验收：每个原用户路由有明确新入口与原权限；内置浏览器及两端嵌入容器实际验证远程桌面、终端、Workspace 文件、同步状态和 VS Code 等支持能力，超限/待同步不显示为已就绪，过期会话可恢复、跨账号拒绝。网页来源/导航/IPC 有边界，远程页面不能调用本机凭证或任意文件桥；管理员页不误入普通客户端。已迁功能不在两处正式维护。

  - **（基线已完成）** **F03-T07.01（完成）准备唯一共享 Workbench 模块**：PC/iOS 后续容器共用 Morde Gateway 下 `/__ch/wb/` 的 Workbench 静态入口；Workbench production base 固定为该路径，API 仍用同源 cookie 会话。Gateway 仅在有效 Host 会话下提供静态页面和资源；定向测试覆盖一次性入口票据、未授权拒绝、资源与 SPA fallback。`web/workbench` 的 `npm run typecheck`、`npm test`（9 个文件/75 项）、`npm run lint`、`npm run build` 与 `server` 的 `go test ./internal/gateway` 全部通过。实际 PC WebView 与 iOS 容器接入仍分别由 T07.02/T07.03 完成。
  - **（基线未完成）** **F03-T07.02（本地 MVP 实现完成，待真实过期/跨账号验收，依赖 F03-T07.01、F03-T05.04）PC 接入受权电脑和工作台**：桌面 Host 入口只接受 Portal 同源 `/host-entry/<id>` 与受限 target；主进程按当前账号重新读取自有/共享 Host 来源，并仅允许票据跳转到该 Host Gateway origin。Host 窗口使用独立隔离 WebView（无 preload、无 Node、sandbox 与 webSecurity 开启），拒绝弹窗及跨源导航；仅授权来源可下载并显示保存对话框，权限请求限制为剪贴板净化写入。`morde-main-recovery` 覆盖目标来源与 Gateway `/__ch/wb/` 跳转、恶意来源拦截和 Host 页面返回；当前桌面 `npm test` 212/212 通过。`harness/desktop/MORDE.md` 记录本地 Portal/PostgreSQL 与 Ubuntu/Lima 环境中自有/共享 Host 桌面、终端、文件和 VS Code 操作，以及共享账号撤权后的终端断开。仍待过期会话恢复和跨账号拒绝的真实客户端交互验收；本轮未访问用户提供的测试地址，也未提交其凭据。
  - **（基线未完成）** **F03-T07.03（本地 MVP 实现完成，待模拟器/真实会话验收，依赖 F03-T07.01、F03-T03.04）iOS 接入受权电脑和工作台**：四栏资源库的快捷菜单新增嵌套“Morde · Computers”入口，不增加底部顶级菜单；复用当前 Morde 账号的自有/共享 Host 列表，仅运行中的 Host 可打开。自有 Host 请求 `/enter`，共享 Host 请求 `observation-ticket`；客户端校验返回票据必须指向列表中的 Host HTTPS 来源，再由无本机桥接的 WKWebView 打开并拦截跨源导航。扩展 host-navigation 脚本覆盖有效票据及错误 Host/端口/路径、缺失/重复/空 token。`run-host-navigation-tests.sh`、Swift parser 与 Minis generic iOS device `build-for-testing` 通过。真实模拟器内入口、会话过期恢复和无权限提示仍待设备/服务验收；当前模拟器运行受本机 CoreSimulator 1174.9.2 低于 Xcode 要求 1174.21.0 限制。
  - **（基线未完成）** **F03-T07.04（本地 PC 用户入口部分完成，待真实服务与权限验收；依赖 F03-T02.01）迁入 PC 资源和普通用户设置**：Portal 用户端现有并可导航至 Host、Workspace、通知、客户端、模型连接与 API Key 页面；新增“账户设置”，显示当前账号/角色并可前往修改密码或客户端管理，普通用户与管理员改密完成后分别回对应登录页。自有主机卡片现提供共享成员管理：展开后加载成员，可授予 `desktop:view`、`desktop:control`、`terminal:control`、`mcp:control`，并在授权和撤权前二次确认；每项权限说明通过 `aria-describedby` 关联到勾选框。授权范围不含工作区访问、主机管理或再次分享权限。`web/portal` 类型检查、lint、全量测试（27 个文件/201 项及 2 个脚本）和生产构建均通过；服务端 Host 分享授权范围、动态权限及观察票据测试 3 项通过。本地 MSW 浏览器打开账户设置、改密表单及主机共享入口，并查看后取消邀请确认，未写入邀请。管理后台仍与普通用户路由隔离。真实服务中的账号存在性、权限执行、失败与空态，以及跨账号验收仍待完成；本项保持未完成。
  - **（基线未完成）** **F03-T07.05（部分完成：账户页与客户端安装管理已实现；成员、模型连接和 API Key 管理未迁入；待模拟器/真实服务验收；依赖 F03-T02.02、F03-T11.01）迁入 iOS 资源和普通用户设置**：现有四栏导航不变，nanoMuse“设置”新增“Morde 账户”入口，显示当前 Morde 用户名称、账号 ID 和规范化后的 HTTPS 服务地址；账户页退出前二次确认并复用现有 `signOut()`，不显示客户端凭据。“客户端安装”页通过现有客户端 Bearer 凭据读取 `/api/clients`，列出当前账号设备并二次确认撤销其他设备；当前设备不可从列表撤销，只能通过账户页退出。安装 ID 格式与撤销路径有本地 helper 检查；本地凭据策略测试、三项服务端路由合同测试、Swift parser、登录/Host 导航 helper、9 语言字符串校验、Minis 与独立 Morde generic iOS 构建均通过，两个 app 产物及 Bundle ID 已核对。成员管理尚未迁入；模型连接和 API Key 管理也尚未迁入，审计发现对应端点只接受 Portal 会话，客户端 Bearer 凭据不能安全复用；需后端提供受支持的原生认证合同后再实现。Workspace 文件读取/同步仍由 T07.06 覆盖。真实服务验收待做；本机模拟器不可用，CoreSimulator 1174.9.2 低于当前 Xcode 要求 1174.21.0，因此模拟器/真机交互验收仍待完成；本项保持未完成。
  - **（基线未完成）** **F03-T07.06（未开始，依赖 F03-T06.06、F03-T07.02、F03-T07.03、F02-T10）接入真实 Workspace 文件与同步状态**：修改：共享 Workbench 读取已确认文件、同步及 Host 就绪状态；验收：PC/iOS 容器与内置浏览器验证文件、限额、待同步和授权状态。
  - **（基线未完成）** **F03-T07.07（未开始，依赖 F03-T07.04、F03-T07.05、F03-T07.06）收敛已迁普通用户重复入口**：修改：逐项确认原 Portal 用户功能有对应客户端入口后调整正式构建/导航；验收：每个退出的旧入口均有可操作的新入口和权限证据，未迁入口不提前移除。

- **（基线未完成）** **F03-T08（进行中，汇总）停用 iCloud 写入并清理旧模型密钥**
  - 修改：`NanoMuseSync.swift`、`NanoMuseSyncSettings.swift`、`Agent/Sync/V2/SyncV2Bootstrap.swift`、旧 CloudKit/备份调度、extension 入口及 `Shared/EnvVarStore.swift` 等可同步 Keychain 路径。停止旧业务同步并保护本机快照、附件、AppGroup 和其他凭据；用户已明确同意的旧模型 API key 按下述范围直接清理。
  - 已确认的设备和数据范围：每台升级的 PC/iOS 上，只删除本应用存储且可识别为用户自带模型供应商的 API key。PC 盘点 `harness/dsh-nanomuse/src/cloud.ts`、`providers.ts` 中 own-provider 的 `keyRef`、模型适配配置与对应凭据；iOS 盘点 `Providers/ProviderConfigStore.swift` 的 `ProviderKeychainHelper`（`io.github.nanomuse.app.provider.<id>`、`api-key`）及相关引用，删除该项的非同步和可同步版本。同步项删除可能传播到同一 Apple ID 的其他设备，应明确告知；不批量清除 Keychain、登录/同步凭据、Morde 平台模型凭据、MCP OAuth 或其他应用数据。
  - 清理规则：先按 provider 实际归属识别并移除密钥及旧模型调用引用，不凭 `NANOMUSE_KEY_` 前缀批量删除；该前缀也用于非自带 API key。`$$ENV_VAR` 中明确由本应用独占且专用于模型的 key 一并清理；共享或用途不明的变量不能盲删，先停用引用并记录待核对项。检查本应用在设备本地管理的备份/缓存中可识别的密钥字段并防止旧恢复流程重新导入；用户独立导出的备份和其他应用文件不递归扫描或删除，记录其可能残留。删除结果须可核对、幂等重试，失败不吞掉或标记成功，日志不包含密钥。现有桌面 `dropOwnRow` 会吞掉凭据删除错误，iOS `deleteAPIKey` 不返回结果，实施时不能据此直接宣称清理完成。
  - 验收：用隔离的合成数据检查 PC 自带 provider 凭据、iOS 同步/非同步 API key 均被定向删除，其他凭据和业务数据保留；重复执行、删除失败/重试、共享变量与旧备份恢复均有明确结果。融合版本不再向 iCloud 写业务数据；其他私密项按原数据保护规则转换。退出保留隔离数据，普通同步关闭不删除数据、不取消已受理云端任务；其余数据删除仍须独立确认范围。未归属历史不因登录新账号自动上传。
  - 交接：记录待替代的数据类型和剩余路径；04 负责 Morde 个人同步及所有 iCloud 路径完整验收。此时不得宣传完整跨端资料同步已经完成，也不能为了演示重新启用旧 Cloud/iCloud。

  - **（基线已完成）** **F03-T08.01（完成，依赖 F03-T02.01）PC 定向清理旧自带模型 key**：启动时一次性迁移在 catalogue 加载后、旧 provider 路由重建前执行；由 own-provider state 或持久化 LLM_ROW 的精确 `apiKeyEnv`、catalogue key-auth 归属及 `keyRefFor(id)` 确认候选。先持久移除模型/Hands/媒体/聊天引用，再按精确 keyRef 删除；pending ledger 可在失败后重试。settings 删除失败不动凭据，凭据删除失败保持待删目标；Cloud、ChatGPT、Morde、runtime、平台 key、无 key 和归属不明项保留。桌面包内未发现单独 `$$ENV_VAR` 模型密钥恢复路径；通用环境变量和其他插件凭据不扫描，`OPENAI_API_KEY`、runtime 合成项保持不变。合成测试覆盖 adapter-only legacy row、重启幂等、失败/重试及无关 key；主任务复验 `npm run build`、`npm run typecheck`、定向测试 18/18 与 `git diff --check` 通过。
  - **（基线未完成）** **F03-T08.02（本地清理与影响提示完成，待隔离 Keychain/设备验收，依赖 F03-T02.02）iOS 定向清理可访问的模型 key**：升级时等 ProviderMigration 和首个安全的 provider-config/DB adoption，再按已知 ProviderType + `apiKey` 精确筛选实例；先把 instance ID 与三条固定旧 singleton API-key service 写入本地版本 ledger，再删除 `io.github.nanomuse.app.provider.<id>/api-key` 和旧服务的 legacy/synchronizable 两类 Keychain 条目。两个范围显式限定为同步和非同步条目，成功/不存在视为幂等成功；仅两类都成功才清 LWW 时间戳；失败目标保留到下次启动重试。OAuth、Morde 凭证及用途未知 provider 不删除；未知 provider ID 写入 review ledger。清理开始时持久化一次性说明，由应用根界面提示旧服务商 API key 若位于 iCloud Keychain，删除可能传播到同一 Apple Account 的其他设备；用户确认后记为已读，旧版本已完成但未提示的 ledger 也补发一次。标题和正文覆盖 9 种语言。helper 测试覆盖身份筛选、未知保留、ledger 重启/失败重试、成功/不存在/部分失败及提示显示/确认状态；credential-expiry 脚本新增合成 Keychain 注入，验证只删除目标服务的两类 API key、保留 OAuth/其他服务项目和部分失败状态。`run-credential-expiry-tests.sh`、Swift parser、Localizable.xcstrings JSON/9 语言校验、generic iOS device `build-for-testing` 均通过。仍缺签名隔离 Keychain 与设备交互；当前未访问/清理任何真实设备 Keychain。旧身份访问路径另由 T08.05 验证，故本项保持进行中。
  - **（基线未完成）** **F03-T08.03（静态 fail-closed 完成，待隔离设备验证；依赖 F03-T01.02）停用 iOS 旧 iCloud 自动业务写入**：CloudKit v1/v2 启动、前台恢复、定时器、migration、实际 transport fetch/send、delegate merge 与 zone delete 均受默认关闭的 fail-closed 策略保护；CloudSyncEngine 停止不清本机 dirty 队列/游标/快照/附件，也不删除远端 zone。iCloud 设置页在策略关闭时只显示暂停说明，隐藏旧启用开关、上传类别、设备名和设备列表，且不启动刷新或读取设备；暂停文案覆盖 9 种语言。nanoMuse relay 和用户手动选择的备份目的地保持独立；不改本机备份文件。Helper、Swift parser、diff check 和 Minis 通用 iOS device build 通过。仍未在隔离设备验证启动/恢复/定时及已运行 transport 均无 CloudKit 网络操作、本机数据可读性和远端独有数据缺口；CoreSimulator 版本仍低于 Xcode 要求。`DebugJSONRPC` 保留只读 CloudKit 查询 RPC（不是自动业务同步），记录为本项范围外余项；故本项保持待设备验证。
  - **（基线已完成）** **F03-T08.04（本地完成，依赖 F03-T08.01、F03-T08.02）防止旧备份恢复被删除的模型 key**：仅在本应用已识别的 `.minisbak` 凭据恢复入口跳过 Provider `apiKey`；OAuth、环境变量及 MCP 凭据继续按原合并规则恢复，用户独立 Provider JSON 导入与其他应用数据不扫描、不改动。合成备份验证确认旧 `apiKey` 未写入模拟 Keychain、OAuth 仍恢复；未知字段计数并显示忽略提示，损坏/不可读 `secrets.json` 会返回明确错误状态与恢复报告。`run-credential-expiry-tests.sh`、Swift parser、Minis 通用 iOS device build 通过；应用构建的既存 Swift/资源警告保留。未访问真实 Keychain；合成 importer harness 使用隔离内存 Keychain stub 验证写入边界。
  - **（基线未完成）** **F03-T08.05（待签名身份与隔离设备验证）验证旧 iOS 身份的模型 key 清理路径**：静态核对：原 Minis target 与本轮恢复 target 的 Bundle ID 均为 `io.github.nanomuse.app`，`Minis.entitlements` 未显式声明 Keychain access group，因此当前定向查询依赖签名 team 的默认 app access group；独立 Morde target 是 `io.morde.app`，不能据此假设它能访问旧 Minis Keychain。清理函数仅按已知 instance service 或三条固定旧 provider service 删除，并分别覆盖 synchronizable 与非 synchronizable 查询。尚未选定/签署 R3 安装身份，也未访问或注入真实/隔离 Keychain 项；必须用签名后的隔离安装验证旧身份两类条目删除成功、其他凭据保留后才可勾选，当前代码未扩展 Keychain group 权限。

- **（基线未完成）** **F03-T11（进行中，汇总）按设计稿重建 iOS 导航与主流程页面**
  - 修改：在原 iOS `NanoMuse/` 工程按设计稿落地页面布局、色彩、字体、图标及组件样式，重建四栏导航、会话侧栏、助手与个人数据、目标例程、设置、资源库、文件、电脑详情和远程画面；将 T03～T07 的真实会话、附件、进度、审批/澄清、产物和资源操作接入相应设计位置。设计稿中的动态/点子/个人数据等未由 03 提供的功能显示待接入状态，交给 04 连接真实数据和操作；不能用演示内容冒充已保存或已执行。
  - 验收：Xcode 模拟器对照设计稿逐页核对布局与组件样式，并操作四栏切换、返回、侧栏、附件选择、进度/审批/澄清/文件预览、资源选择、电脑详情与远程入口；会话内不显示独立任务编号或任务列表。检查窄屏、键盘、安全区域、读屏，以及加载/空态/离线/无权/失败/未确认状态；常用资源切换不改变进行中的本轮，上传内容先确认范围。列明 04 尚需接入的页面和具体入口，记录可验证结果。

  - **（基线未完成）** **F03-T11.01（本地实现完成，待模拟器交互验收；依赖 F03-T01.02）搭建 iOS 四栏与会话导航**：底部导航已改为聊天、动态、点子、资源库四栏；目标/例程入口改挂点子子页并提供返回，资源库提供电脑入口和默认工作区未接入提示；增加四栏与路由映射单测。Swift parser、通用 iOS device build 和 generic iOS device `build-for-testing` 通过，完整 `MinisTests` 可编译。模拟器 `build-for-testing` 在链接设备版 `libish_emu.a` 时失败；本机 CoreSimulator 1174.9.2 也低于当前 Xcode 要求的 1174.21.0，因此模拟器切换、侧栏及返回的实际交互尚未验证。验收：模拟器实际切换四栏、侧栏及返回，不把示例当真实数据。
  - **（基线未完成）** **F03-T11.02（本地实现完成，待真实服务与模拟器验收；依赖 F03-T11.01、F03-T04.03、F03-T05.03）接入聊天与会话状态页面**：原 Minis 四栏主聊天现在使用 Morde 云端对话，恢复会话与 assistant 消息快照，显示流式/中断和任务状态，并在会话内提供审批、拒绝、澄清、暂停、继续、取消及失败批次项重试；主聊天页不呈现 Task ID。设置页复用同一 Morde 客户端，但不重复启动 SSE/授权生命周期；Morde-only 登录现可穿过旧 NanoMuse 首次设置门禁，回到原四栏主聊天；首次设置门禁现调用纯 Swift `MordeFirstRunPolicy`，credential-expiry helper runner 新增 6 个策略断言，覆盖 Morde-only 会话跳过旧引导及原有 NanoMuse 本地模型/会话分支。新增路由 XCTest 编入完整 `MinisTests` 构建。Swift parser、Morde helper tests（含新增首次设置策略断言）、credential-expiry、host-navigation、本轮更改后 Minis 通用 iOS device build 再次通过；既有 generic iOS device `build-for-testing` 编译完整 `MinisTests`。Xcode 提示本机 CoreSimulator 1174.9.2 低于要求的 1174.21.0，模拟器仍不可用。模拟器版本不兼容，且测试服务 `/api/capabilities` 当前返回 404；真实登录、消息回执和状态操作尚未验收。验收：实际操作真实状态和按钮，不显示独立 Task 列表/编号。
  - **（基线未完成）** **F03-T11.03（本地页面实现完成，待 F03-T04.05/F02-T10 与模拟器验收；依赖 F03-T11.01、F03-T04.05、F03-T07.03）接入资源、文件和电脑页面**：资源库现含电脑、Workspace、文件三个子页；电脑列表复用账号获准的自有/共享 Host 及现有一次性 Gateway ticket 和受限 WKWebView，Workspace 只读显示当前账号资源；文件页明确提示下载/检查点/预览合同尚未接入，不显示示例文件或假成功，默认资源选择也明确待接入。Swift parser、`run-host-navigation-tests.sh`、`git diff --check` 与 Minis 通用 iOS device build 通过。`workspace-sync` capability 关闭、F03-T04.05/F02-T10 文件合同未就绪，实际文件和模拟器交互仍未验收；CoreSimulator 1174.9.2 低于当前 Xcode 要求 1174.21.0。验收：合同就绪后在模拟器操作真实文件与电脑入口，切换资源不改变在途请求。
  - **（基线未完成）** **F03-T11.04（静态审查与修正已完成，待模拟器/真机视觉和读屏验收；依赖 F03-T11.01）核对 iOS 视觉和可访问性**：静态核对确认四栏按钮具备本地化 VoiceOver 标签和选中状态，离开聊天栏时聊天页从触摸和辅助功能树隐藏，底部安全区随栏显示状态调整；修复“减少动态效果”开启时仍播放的栏页切换淡入淡出，并把英文/中文设计及 iOS 文档同步为四个主标签、目标从点子页打开。`website` 的 `npm run docs:build` 通过。仍需在窄屏与 iPad 实际检查布局、色彩、键盘、安全区域和 VoiceOver，并逐页操作加载/空态/离线/无权/失败状态，确认个人页明确显示待接入能力；当前 CoreSimulator 版本落后，模拟器验收未完成，本项保持未完成。

- **（基线未完成）** **F03-T09（进行中，汇总）补齐构建、调用方与交互检查**
  - 修改：原桌面/iOS 测试入口、共享接口 fixture、资源/原生二进制复制、extension 依赖和开发说明；修复迁移造成的重复导航、不可达页面及旧地址调用。客户端会话、任务和状态统一读取 Morde，不恢复本机 Agent 执行分支。
  - 验收：按仓库现有脚本完成桌面类型/构建及相关行为测试；Xcode 本地模拟器构建并启动原应用。测试断网、空态、权限/版本/额度错误、长任务状态、键盘/读屏可达性及实际使用的语言；涉及签名/扩展/真机限制如实记录，不能用静态页面截图替代交互验证。

  - **（基线已完成）** **F03-T09.01（完成，依赖 F03-T01.01）检查桌面构建和受影响测试**：桌面 `typecheck` 现覆盖全部 TypeScript 源码；`npm test` 编译完整测试用源码并运行 `tests/*.test.mjs`，不清理既有 `out`。类型检查、Morde-only 编译、完整测试均通过（212 项），`git diff --check` 通过；修正了一条过期的 sync 许可证文案断言。另在本机临时目录实际构建 Linux x64 unpacked app，检查 `.asar` 文件白名单和随包 `morde-sync` SHA-256；临时产物清理，既有 `dist` 未覆盖。签名、其他平台安装包与真机节点联验仍待相应环境。
  - **（基线未完成）** **F03-T09.02（设备与 XCTest 编译完成，模拟器/签名/真机待验证，依赖 F03-T01.02）检查 iOS 构建和扩展**：修改：更新原 iOS target、资源及 extension 依赖；验收：本地模拟器构建并启动；签名、扩展及真机未测项单列。Minis 通用 iOS device build 与 generic iOS device `build-for-testing` 均退出 0，scheme 包含 MinisShare、AgentWidgetExtension、MinisFileProvider；测试 target 文件同步配置排除 9 个独立命令行 `Standalone` harness。修复 `NanoMuseCopyTests.swift` 的 5 处 `load()` 歧义，并把主机标签提取收敛到 `LLMError.hostTag`，解除 XCTest target 对未编译 UI 层 `NanoMuseProviderReach` 的反向依赖。模拟器 `build-for-testing` 在链接设备版 `libish_emu.a` 时失败；即使修复该架构差异，本机 CoreSimulator 1174.9.2 仍低于 Xcode 要求的 1174.21.0，模拟器启动、签名及真机运行仍待验收。
  - **（基线未完成）** **F03-T09.03（待验证：本地合同与客户端实现完成，真实环境联验依赖 F03-T02.01、F03-T02.02）核对客户端合同和错误 fixture**：共享样例 `server/openapi/fixtures/morde-client-v1.json` 覆盖 queued Task API、可执行 Worker、旧 `cloud_tasks` 兼容输入、API v1/不兼容版本、401/403/404/409/422/503/504、network/non-JSON 失败、Task 快照序号、assistant 消息快照及 SSE delta/snapshot_required；服务端能力将 `task_api`（对话+Task API）与 `cloud_tasks`（再加执行控制）分开，OpenAPI、Portal schema 与 Swift DTO 已同步。服务端 API 测试验证能力状态、fixture 错误码、OpenAPI 状态/消息及 snapshot_required 帧；Task 快照并发测试覆盖稳定消息 ID、同序号正文和状态。PC Assistant 在能力请求 pending/error 或 Task API 缺失时禁用提交并提供重试/不兼容提示；Worker 缺失但 Task API 可用时明确显示排队。iOS 能力失败区分断网/HTTP/无效响应/版本不兼容；Task API 缺失时禁用提交并保留旧 cloud_tasks 兼容；切换会话清除旧 Task 并拒绝错误会话的 Task。iOS Host/Workspace/模型资源逐项显示 loading/loaded/failed；单项失败清空数据并提供重试，空态按状态区分，未加载时清除选择，发送前复核模型和能力。Portal 全量测试 25 个文件/192 项及 2 个 Node 脚本、`npm run typecheck`、相关 ESLint、Go API 定向测试、PostgreSQL 16 消息快照并发测试、iOS helper/fixture runner、Swift parser、Minis 通用 iOS device build 均通过（Swift/资源有既存警告）。尚待真实服务登录、设备操作、SSE 联验与模拟器交互；指定测试站最新复验为 `/admin/login` 可登录、`/api/capabilities` 返回 HTTP 404 `not_found`；未录入模型 key、创建任务或发送文件。
  - **（基线未完成）** **F03-T09.04（部分完成，依赖 F03-T07.02、F03-T11.04）检查已接入页面交互**：修改：在原客户端和内置浏览器操作已实现的用户入口；验收：记录真实交互、返回、键盘/读屏和失败状态，不用静态截图替代。本机 Electron 源码构建配合 loopback mock 已验证登录页 Tab 顺序（服务地址→账号→密码→登录）、必填字段提示、登录进入内置 Assistant、本机同步空态及同源 `/hosts` 页面跳转；请求只到 `127.0.0.1`，临时日志不记录请求体。重开恢复与退出未验证：第二次启动 Electron Helper 遇到 `sandbox_extension_issue_file ... Operation not permitted` 后未呈现 Morde 窗口；外链拦截、VoiceOver 和失败状态交互仍待完成，故本项保持未勾选。

- **（基线未完成）** **F03-T10（进行中，服务端主流程/隔离已有本地证据，客户端联验待环境；汇总）完成跨端主流程并交接 04**
  - 修改：仅补齐上述实现问题与本清单验证记录，不创建第二套演示后端。使用本地 Morde、真实持久库、服务端 dsh 与获准测试 Host，PC/iOS 运行本次原工程构建。
  - 验收：手机提交后结束 App，全部客户端离线时任务继续；PC 重开读到相同任务及已保存文件，并成功审批/补充/接管/交还/停止或撤安装。至少覆盖提交断线、保存失败、账号切换、同账号 Runtime 归属、不同会话并行及另一账号隔离，服务端及数据证据与界面一致；模拟器与真机结果分开记，环境缺失项保持待验证。

  - **（基线未完成）** **F03-T10.01（服务端持久化跨身份验证已通过，待客户端断开/恢复联验；依赖 F03-T04.02、F03-T05.01）验收无文件的跨端接续**：本地 PostgreSQL 17 集成测试 `TestClientTaskSubmissionIsVisibleToDesktopAndIOSInstallations` 通过：iOS Client 提交并幂等重试纯文本 Task 后，桌面 Web Session 读取到同一 Task、事件序号、会话消息和列表记录。测试使用仅绑定 `127.0.0.1`、无持久数据卷的临时内存 PostgreSQL 容器，退出后已停止并自动移除；现存其他 Docker 容器未受影响。**未完成**：这项服务端测试不模拟关闭 iOS/PC 客户端、重新启动后的 UI 恢复、SSE 进度或真实 DSH 执行；这些仍需本地客户端服务环境后联验，不能凭 DB 测试勾选本项。
  - **（基线未完成）** **F03-T10.02（未开始，依赖 F03-T05.02、F03-T05.03、F03-T05.04、F03-T05.05）验收跨端审批、接管和停止**：修改：在真实 PC/iOS 与获准本地 Host 上操作控制流程；验收：审批、接管/交还和停止状态与服务端证据一致。
  - **（基线未完成）** **F03-T10.03（未开始，依赖 F03-T06.06、F03-T07.06、F02-T10）验收跨端 Workspace 文件链**：修改：通过真实 02 文件合同在 PC/iOS 与 Host 之间传输；验收：提交、保存、下载、断线及失败场景核对持久文件摘要。
  - **（基线未完成）** **F03-T10.04（服务端隔离/Runtime 并行验证通过，待客户端 UI 联验；依赖 F03-T02.03、F03-T02.04）验收账号隔离和并行**：本地 PostgreSQL 17 临时内存库上的 `TestClientLoginMobileAndCrossAccountRenewal`、`TestPostgresAccountAgentRuntimeLeaseAndTaskSequence`、`TestPostgresTokenAndInstallationRevocationStopsOnlyMatchingTasks`、`TestPostgresClientTaskAuthorizationSnapshotSurvivesReloadAndRevocation`、`TestPostgresTaskWebSessionRetainsOwnerInstallation` 均通过：不同账号不能续期/撤销对方安装或提交对方安装授权的 Task；会话/安装撤销只停止匹配任务；授权快照跨读取、追加消息和领取保持，并在撤销后阻止操作；同一账号 Runtime 的不同 Task 可并行取得唯一序号，旧执行 epoch 不能续约。测试容器只绑定 `127.0.0.1`，使用临时内存数据目录，测试后已停止自动移除。**未完成**：当前只证明服务端存储、授权与 Runtime 并发行为，不包含真实 PC/iOS 两个账号的界面隔离、客户端并行执行和停止范围 UI 联验，故本项保持未勾选。
  - **（基线未完成）** **F03-T10.05（未开始，依赖 F03-T01、F03-T02、F03-T03、F03-T04、F03-T05、F03-T06、F03-T07、F03-T08、F03-T09、F03-T11）完成 03 阶段交接**：修改：汇总原功能、设计稿页面、真实构建及 F03-T10.01–.04 的证据，列出 04 待接入口；验收：父项全部验收满足且未测平台如实记录，才交接 04；不等同整体验收。

## 4. 交接门槛与问题记录

交接给 04 时必须已有可操作的原 PC/iOS 入口、统一身份、云端任务主流程、文件同步及云端电脑操作；iOS 完成设计稿所需导航、主流程页面和明确的待接入状态。适用的客户端页面、文件传输及既有本地数据受到保护，Agent 执行统一在云端。附上原功能清单、设计稿页面与状态逐项去向、实际构建/验证证据，以及 04 待接入的数据类型和页面。**04 可以在这些客户端上新增业务适配，但不负责补救 03 主流程空壳或缺失接口。**

| 问题/边界 | 处理 |
|---|---|
| R3 正式签名、安装身份及升级路线 | 用户于 2026-11-01 选择先本地核对可行性再决定；F03-T01 使用隔离测试安装，F03-T08.05 与 F05-T02.05a/.05b 分别核对旧密钥、导出和签名。正式身份未确定不更改旧数据 |
| 原本机 Agent 与云端统一执行 | 按运行时统一化需求停用客户端 Agent 执行；除 D2 已确认清理的模型 API key 外保留旧数据。其他新增核心冲突记录行为、影响与选择，提交用户审核 |
| 04 尚未提供个人同步/Hub 等能力 | 明确未接通并保留已有本地数据；不假装可用，不回退旧服务；不作为 F03-T10 的主流程前置 |
| 模拟器无法覆盖签名、扩展或真实设备能力 | 可验证项先完成，具体未验证项交 05 保留；不得据模拟器通过声称真机通过 |
| R1 Android 本期范围 | 用户已确认本期手机端只改 iOS；Android 源码、配置、构建和验收不动，PC 仍在范围内 |

后续进度、问题和证据记录在五会话任务文档及独立验收清单；本页保留拆分时基线。
