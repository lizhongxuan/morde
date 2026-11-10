# 会话 3：iOS 客户端实施 TODO

> 阅读：[并行执行入口](00-五会话并行执行入口.md)、[设计方案](../../Morde定位对齐与功能精简设计方案.md)。本清单覆盖 A07/A09–A11/A15、B01–B11、C01–C10 及 P1/P2 的 iOS 部分。
>
> 边界：只写 `project/nanoMuse/android/src/ios/**` 和本清单，包含原 `Minis.xcodeproj`、iOS 资源和测试；不改 Android 其他目录、PC、服务端、OpenAPI 或共享构建文件。保留现有数据和其他会话改动。

## 接口与交接

- 按设计 10.1 接主会话查询、初始化、切换；消息走现有会话追加接口并展示 R04 的投递状态。现有 Task 澄清、审批、停止和事件续读语义继续使用。
- Workspace diff/回滚、文件操作、只读和敏感路径由 W05–W07 提供服务端合同；一对一绑定、控制权、复制和移交由 W17–W22 提供。项目、成员、预算、目标和提醒由 P02–P10/W08 提供。
- 本机 `MordeLocalExecutionPolicy` 已关闭；在正式 target 真正移除对应执行链和完成覆盖前不能移除门禁。页面验证只用本地内存数据，不上传、外发或发送文件。

## 本机验证范围

按设计 11.1–11.4 执行。当前 Mac 已有可用的 iOS 模拟器；用本机编译和模拟器/内存替身覆盖界面状态，不要求真实 iPhone、第二个人或真实文件传输。分别记录构建、模拟器运行及未验证的真实设备/后端；缺离线依赖时标待验证，不下载或发送文件。

## P0-A：iOS 正式入口与会话

- [x] **I01（已完成，本机静态检查与 iOS target 构建通过）确认原 iOS 工程与本机执行边界**
  - 修改：`Minis.xcodeproj`、`MinisApp.swift`、`NanoMuseShell.swift` 及必要测试；保留原工程和 Morde 云端入口，核对 `MordeLocalExecutionPolicy` 覆盖聊天、模型、Hub 委派、例程和快捷指令。
  - 验收：静态 target/调用检查和可用本地构建确认本机循环被禁用；模拟器或设备不可用时记录待验证，不把构建成功写成设备通过。

- [ ] **I02（进行中：主会话接口和普通会话已接；双客户端及服务端冲突验收待验证，依赖 W02b）接唯一主会话和普通会话**
  - 修改：`Morde/MordeApp.swift`、`NanoMuseShell.swift`、会话列表和本机最近会话缓存；从服务端读取主会话并处理初始化、选择候选和版本冲突。
  - 验收：本机模拟两个客户端读取同一主会话 ID；切换/刷新不新建 Task；换账号/服务实例不显示旧缓存；历史不确定时可选而不自动合并。真实跨设备结果另记。

- [ ] **I03（进行中：server DTO、OpenAPI 与 iOS 已对齐 delivery_status 并展示；双客户端断线/重开端到端待验证，依赖 R03/R05/W02b）接多轮消息与投递状态**
  - 修改：会话输入、消息列表及 Task 快照/SSE 恢复；区分持久受理、已进入 Agent 和未知，发送失败保留草稿，重复请求复用幂等键。
  - 验收：本地测试与可用模拟器覆盖两轮同一会话、断线、应用重开和重复提交；仅已确认消息显示对应状态，打开历史不触发执行。

- [ ] **I04（进行中：批准/拒绝已改走 versioned approval responses，澄清走 inputs；双端重复审批、版本冲突和停止证明场景依赖 R04/R05/W02b 待验）会话内审批、澄清与改指令**
  - 修改：会话中的批准、拒绝、暂停、继续、取消和补充输入；区分普通补充与版本化澄清，等待停止证明时保持等待。
  - 验收：模拟双端重复批准、版本冲突、暂停后追加和取消未知；退出/切屏不停止云端任务。

## P0-B：个人托管与手机文件

- [ ] **I05（进行中：版本清单浏览、目录前缀筛选、预览、下载和显式上传已接入；W06 已交付远端重命名/移动/删除路由与 OpenAPI，iOS 操作入口/API 已接但仍受 capability 门禁；服务端未声明 workspace_sync，F02-T10 的 Ubuntu peer 连接验收未通过，因此不能开放写操作，依赖 W05–W07/F02-T10）实现手机 Workspace 文件管理**
  - 修改：`MordeApp.swift`、`MordeWorkspaceTransfer.swift` 与资源库入口；接已确认版本的浏览、预览、搜索、下载、重命名、移动、删除，默认只读并在获得服务端授权后显式切换写入；区分本机副本和云端文件。
  - 验收：使用本地策略测试与进程内假文件结果验证权限、敏感路径、版本冲突和操作反馈；不实际传送业务文件；真实传输/同步记录待验证，不能开启未通过的 capability。

- [ ] **I06（进行中：Goals 云端快照已改为完整分页，Schedule 按账号隔离且已有运行历史分页；完整记忆/例程同步与冲突验收依赖 P02/P04/P05/R07）接个人资料、记忆、目标、例程和提醒**
  - 修改：`NanoMuse` 对应页面、Morde 数据调用和个人同步；原有本机历史可读，云端对象走统一账号和服务端版本，未同步草稿与冲突保留。
  - 验收：本地测试覆盖授权、冲突、目标和计划状态、通知定位；不同账号资料不串用。

- [ ] **I07（进行中：本机 API 已挂载 Workspace diff/restore 且 iOS 有差异选择与显式回滚；Workspace 实时 changes/Host 状态流未接入，Task SSE 仅用于当前 Task 状态恢复，依赖 W05/R04）接文件 diff、回滚和实时过程**
  - 修改：资源库版本页面和会话过程展示；回滚选择已确认版本，提交前显示范围和期望版本；展示事件、当前步骤和 Host 独立状态。
  - 验收：模拟差异、回滚冲突、保存未确认、事件断线/快照恢复；不把静态摘要伪装成完整操作回放。

- [ ] **I14（进行中：iOS 恢复入口只允许负责人恢复已停止且恰有一个就绪 Workspace 的电脑；默认路径与迁移状态依赖 W18/W22）对齐电脑与唯一 Workspace 状态**
  - 修改：`MordeApp.swift` 的电脑、Workspace 选择和恢复入口；已有创建/恢复操作必须选择恰好一个可用 Workspace，无绑定旧电脑不可直接恢复/打开；展示工作与下载的默认根、Workspace 外临时数据边界、移交冻结和迁移待处理状态；不能承诺应用内手选的其他路径会持久化。不为本项新增独立的电脑创建产品流程。
  - 验收：本地测试/模拟器覆盖零个/多个、占用、无权、冻结、旧电脑待绑定及路径未就绪；只显示服务端确认结果。

- [ ] **I15（进行中：Portal 已暴露操作者/30 秒租约和续租合同；iOS 已接 owner Host 状态、按版本接管/交还/续租；共享 Host 仍只读，双账号和租约冲突验收待完成）接多人观察与单人操作权**
  - 修改：手机电脑观察和控制入口；显示实际控制人、只读、申请/释放及交接状态，按服务端控制版本和授权启用操作，画面共享提示可能显示 Workspace 内容。
  - 验收：本地测试/模拟器用两个逻辑账号覆盖抢权、服务端报告旧连接阻断、撤权、断线、AI 交接和状态未知；观察者不能伪装为操作者。旧连接实际拦截由 W19 单独验证。

- [ ] **I16（进行中：本机已有复制和按已知 ID 查询/接受/取消移交路由，但未声明 workspace_sync capability，且缺待接收移交列表路由；iOS 操作 UI 仍未接入，依赖 W20/W21）接 Workspace 复制与移交**
  - 修改：手机 Workspace 资源入口；提供完整复制、发起移交、申请列表、接收和取消；展示版本、接收人、冻结、配额、失败和恢复状态，并提示文件树含隐藏文件及用户保存的凭据；如源未停写、文件类型不支持或空间能力未就绪，显示服务端返回的原因。
  - 验收：本地测试/模拟器用内存数据模拟运行中无法快照、申请后双方不能使用、接受/取消并发、失败保留冻结和成功切换所有者；不实际传送文件。

## P0-C：团队项目与交付

- [ ] **I08（进行中：项目/成员/邀请/负责人转移/资源关联 UI 已接本机 Portal 路由；负责人待处理邀请无列表路由，Workspace 复制/移交接口未由服务端 capability 开放，依赖 W20/W21）接项目和获准电脑**
  - 修改：四栏资源入口、项目列表、成员邀请/接受/撤销与负责人交接、权限和 Host 状态、共享与复制界面；电脑副本使用独立 Workspace，给其他用户须等待移交接受；只展示当前账号获准内容。
  - 验收：本地测试/模拟器模拟邀请接受前后、负责人交接、成员、只读、撤权、复制失败与移交待接收；个人主会话与记忆不自动共享。

- [ ] **I09（进行中：本机 API 已挂载会话/work-item/budget 路由；iOS 支持文本共享会话、创建者 versioned 审批/澄清、工作项、Task 关联、阻塞恢复、预算/费用及经确认的交付登记；重开后仅恢复会话创建者本人发起且归属匹配的 Task。完整双端验收仍待补；所有执行受 project_budget_execution capability 门禁约束，依赖 P08–P10/R09–R10/W11–W12）接项目工作、交付与预算**
  - 修改：项目共享会话、Todo、AI/电脑状态、Task 关联、阻塞处理、审批和交付版本；显示费用依据、预算不足与未知费用。
  - 验收：模拟至少两种执行目标与项目授权；无权限、停止未确认、未保存产物和未知费用均不报成功。

## 清理和后续

- [ ] **I10（待依赖：本机执行门禁仍关闭；正式替代能力和数据迁移未完成，按要求保留旧 target 与代码）移除旧本机 Agent 和重复发行入口**
  - 修改：`Agent/Chat`、`Providers`、旧 Hub/例程和自带模型 key 专属路径；关闭旧 nanoMuse Cloud 独立业务入口；逐项从正式 target 移除无调用的本机执行代码。退役独立 `Morde.xcodeproj`/`MordeStandaloneApp.swift` 时保留 `Minis` target 共用的 `MordeApp.swift`。
  - 依赖：云端替代及本机数据保留验证；向 R11/W13 提供调用方退役证据。
  - 验收：构建、target 和调用检查无本机 Agent/直连模型执行；按既有确认只清理可识别的旧设备模型 key，保留 Morde 登录、同步、MCP OAuth 和用户数据。未完成的保留功能不删。

- [ ] **I11（进行中：通知摘要遵守 redacted/actionable；设备列表使用 /api/devices，Hub v1 下提供用户点按的 HTTP(S) 打开链接接力；P12/R12/W15 服务端合同已完成，iOS 项目连接器 UI 未接；当前项目连接器列表只返回 active grant，撤销后缺少查询 revoked grant version 的路由，阻塞可靠重授权）接入 P1 手机能力**
  - 修改：设备接力、通知分级、项目技能/连接器和产物模板；依赖 P12、R12、W15 的对应合同。
  - 验收：本地测试或模拟器用内存数据验证设备/项目权限和空态；不访问外部服务、不发送文件，目标设备未运行时标待验证。

- [ ] **I12（待依赖 P13/R13/W16：本轮未接入媒体、跨项目搜索或行级批注接口）接入 P2 和待排的 Muse 手机能力**
  - 修改：行级批注、跨项目搜索、语音输入和媒体入口；Feed/Ideas/Library/简报保留并逐步接 P13、R13、W16 的统一服务。录音/选择文件不启动本机 Agent。
  - 验收：本地测试或模拟器以内存数据验证项目隔离、无权和未就绪状态；不调用真实媒体服务或发送文件。

- [ ] **I13（进行中：Morde simulator 构建通过；Minis generic iOS 与 build-for-testing 通过，Minis simulator 仍受 device-only 预编译库阻塞；Morde UI test 在未注入凭据时安全跳过。真实服务/双端验收未做）完成 iOS 回归验收**
  - 修改：本会话范围内 iOS helper、target 和必要测试；记录实际构建及模拟器/设备可用性。
  - 验收：核对主会话、消息、审批、项目/个人隔离、只读文件、一对一绑定、控制权、复制和移交冻结；真实服务、文件传输或设备未验证的项目单列待验证，不伪报完成。

## 交接记录

按入口第 4 节追加服务端合同、文件权限、项目数据及验收缺口；不改其他会话的清单。

- **I11 → 会话 4/5（P12）：**项目 MCP Connector API 的列表仅返回 active grants。撤销响应会返回递增后的 revoked `grant_version`，但 App 重启后无法查询该版本，使用 `expected_version: 0` 重新授权已撤销 grant 会冲突。请会话 4 提供带当前 grant 状态/版本的只读 Store/App 合同，由会话 5 接入 `GET /api/projects/{project_id}/mcp-connectors/{connector_id}` 与 OpenAPI；仅当前连接器 owner 或项目 owner 可读取，无 credential/endpoint 字段，未知 ID 对无权调用方返回不泄露存在性的错误。新建仍用 version 0，重授权必须使用读回版本。未交付前 iOS 不显示撤销后的重授权按钮。
- **I11 设计边界：**Runtime 固定内置技能是只读指引，不增加 Task 工具权限；`skill_setting` personal object 目前不控制 Runtime catalog。iOS 可说明技能行为但不提供会被误认为授权的项目级 toggle；如需逐技能授权，须先由服务端设计版本化 skill grant。

### 2026-11-02 本机实施进展

- I01 核对通过：`MordeLocalExecutionPolicy.isEnabled` 固定为 `false`；Minis generic iOS target 与 Morde simulator target 均可编译。未移除本机 Agent 代码或独立发行入口。
- I02 已接 `/api/me/main-conversation`、`ensure`、带 `expected_version` 的切换，以及普通对话创建/分页。首次仅跟随服务端主会话；用户明确打开普通对话后保留该选择；历史候选必须显式设为主会话。版本冲突会刷新状态，不自动覆盖。双客户端真实并发仍待验证。
- 会话和 Workspace 安全收口：最近会话缓存不再自动选中；无已选会话时不创建 Task；消息投递状态缺失时明确显示未知；Workspace 文件清单/上传/下载在服务端未声明 `workspace_sync` 时不调用。
- Host 入口要求运行中的电脑恰好绑定一个就绪 Workspace；共享 Host 入口标记为只读观察；项目页不再把 Workspace 冒充项目。
- I03/I04 更新：服务端消息 DTO 与 OpenAPI 均要求 `delivery_status`，iOS 展示该字段；批准/拒绝使用 `/api/tasks/{id}/approvals/{approval_id}/responses` 的版本化请求，澄清使用 `/api/tasks/{id}/inputs`，两者复用幂等键并刷新 Task 快照。API 路由测试与本机策略测试通过；双端版本冲突、未知停止结果仍待端到端验收。
- I08 更新：Portal 已挂载项目 CRUD、成员/邀请、负责人转移和 resources。iOS 通过短期 Portal session cookie 调用，覆盖列表、邀请接受/撤销、成员权限、负责人交接和当前账号可见的 Host/Workspace 关联；关联不授予项目成员资源权限。服务端未提供负责人待处理邀请列表，因此撤销按钮仅作用于本机会话刚创建的邀请。复制/移交接口尚未提供，不能作为已完成 UI。
- I09 真实缺口：OpenAPI 的预算和 work-item 路径尚未在 `api.New`/Portal 启动流程装配；生产费率 provider、Task 关联授权器和 delivery 授权器也缺失。不得将 helper-router 单测当作 Portal 可用合同。
- 待上游交付/集成：R03/R05/W02b 的跨客户端运行消息验收；W05–W07 的 Workspace diff/restore handler 尚未接入 `api.New` 或 OpenAPI，Workspace changes iOS 消费及远端重命名、移动、删除（Task SSE 已接入）；W17 的创建/恢复约束已交付，剩余 W18/W22 的路径与迁移状态；W19 控制租约双账号抢权、续租到期和撤权验收；W20/W21 的复制与移交；P08–P10/R09–R10/W11–W12 的项目 Todo、预算和审计；P12–P13/R12–R13/W15–W16 的项目技能及其他 P1/P2 能力。项目端到端状态以 Portal 实际路由装配和持久化为准，本客户端不猜测缺失合同。
- I06 本轮修复 Goals 云端快照分页及 Schedule 的账号切换响应隔离；计划授权撤销后清除缓存目标。I11 通知摘要在 `redacted` 为真或字段缺失时隐藏；等待用户通知只有 `actionable == true` 且未脱敏时才导航到任务；设备 presence 仅作为提示，接力只在 Hub 返回 `opened:true` 后显示成功。
- 本机验证：`run-credential-expiry-tests.sh`、`run-morde-hub-relay-tests.sh`、`run-morde-workspace-transfer-tests.sh`、`run-host-navigation-tests.sh`、`run-morde-portal-navigation-tests.sh` 均通过；`Morde.xcodeproj` simulator build 通过。`Minis.xcodeproj` generic iOS device build 返回成功（保留既有 Swift/Objective-C 警告）；Portal `go test ./internal/api -run 'Test(TaskAction|Project)' -count=1` 通过。
- Minis simulator build 未通过：预编译 `android/deps/libs/libish_emu.a` 是 iOS device 架构，不能链接进 iOS simulator；该依赖位于本会话修改边界外，未替换或改动。Morde app 在模拟器启动后被系统定位权限弹窗遮挡，未作权限选择，因此没有记为界面运行通过。
- 未连接测试服务器、未使用用户提供的服务器/API 凭据、未上传或外发文件。真实设备、双账号/双客户端、服务端文件传输及远端运行状态均未验证。

### 2026-11-03 本机实施进展

- I05 已把 manifest `prefix` 查询接到手机文件列表，提供显式路径筛选/清除；分页沿用已应用的前缀，空前缀不发查询参数，并拒绝绝对路径、上级目录、空路径段、反斜杠、NUL 和超长输入。服务端原有校验与过滤逻辑未改。云端重命名、移动和删除仍缺合同，因此 I05 保持进行中。
- I06 的 Schedule 运行历史分页已接入现有账号授权和 cursor；例程/记忆完整同步与双账号冲突处理仍依赖服务端合同。
- I07 本机 API 已挂载 diff/restore，iOS 支持选择差异、提交前预览路径/版本及显式确认回滚；目前没有适合手机消费的 Workspace changes 时间线合同，Host 实时状态也未接入。
- I08 项目 CRUD、成员、邀请、负责人转移和资源关联继续按短期 Portal session 调用。负责人的待处理邀请列表仍没有服务端路由；关联仍不授予 Host/Workspace 权限。
- I09 本机 API 已挂载项目共享会话、work-item、budget 和 ledger 路由。iOS 已支持文本共享会话、共享会话创建者按版本审批/拒绝与澄清回复、Task 状态刷新、工作项创建/阻塞/恢复、把当前项目 Task 关联到工作项、预算/费用显示，以及确认后提交 Workspace 当前版本供服务端验证交付；消息和交付操作都不附加或传送文件。重开后 Task 恢复与完整双端验收仍待补；所有执行按 `project_budget_execution` capability 关闭时 fail-closed。
- I14 Host 恢复入口已加负责人、已停止、唯一就绪 Workspace 校验，并调用现有 restore 路由；默认根路径与迁移状态没有可用合同。I15 owner Host 的接管/交还/续租 UI 已按版本和租约状态执行，共享 Host 保持只读；双账号竞态和真实撤权仍待验证。
- I16 本机 API 已挂载 Workspace copy 和通过已知 ID 查询/接受/取消 ownership transfer 的路由，但 capabilities 未声明 `workspace_sync`，且没有 recipient transfer inbox/list 路由；iOS 不开放这些操作，也不能从 Workspace 关联推断访问权。
- 本轮验证：8 个 iOS 本地测试 runner 均通过；`Morde.xcodeproj` generic iOS Simulator build 成功；`Minis.xcodeproj` generic iOS build 和 `build-for-testing` 成功。Morde 模拟器 UI test 在未提供本地验收凭据时跳过（0 passed、1 skipped），未访问后端。Minis simulator 仍因预编译 `libish_emu.a` 只有 device 架构而无法链接；Minis 编译保留既有 SDK/Swift/Objective-C 警告。
- 本轮未访问测试服务器、未使用用户提供的外部凭据、未上传/发送/外发文件。双客户端/双账号、真实设备、真实服务端状态及文件传输仍待验证。

### 2026-11-03 后续实施进展

- I09 项目共享会话重开后，按最新一条由会话创建者本人发送且关联有效 Task ID 的消息恢复 Task；服务端返回后再次校验 Task ID 和 conversation ID。非创建者消息不触发恢复，恢复读取失败保留已加载消息并提示重试。策略测试覆盖创建者范围、其他成员隔离和无效 ID 回退。
- 本次验证：`Tests/run-credential-expiry-tests.sh` 通过；`Morde.xcodeproj` iOS Simulator build 与 `Minis.xcodeproj` iOS device `build-for-testing` 均成功；`git diff --check` 通过。未执行真实服务端或双账号 UI 验收。

### 2026-11-03 I05 Workspace 文件操作接入

- 核对 W06 当前代码：`POST /api/workspaces/{id}/file-operations` 已在 Portal API 装配并写入 OpenAPI；服务端负责会话身份、敏感凭据路径、期望 revision、活动 Host/写入者、移交冻结和幂等检查。本轮仅修改 iOS 客户端边界。
- iOS Workspace 文件行新增重命名、移动、删除云端操作；重命名/移动输入按相对 POSIX 路径校验，提交携带用户确认的 revision、显式 `write_enabled: true` 和确定性幂等键。删除单独确认并说明创建新检查点、旧版本仍可恢复；成功或未知回执后刷新 Workspace 与 manifest。既有本机副本删除按钮仍只操作本机目录。
- 所有写操作继续受 `workspace_sync` capability 门禁；服务端 `capabilities.go` 未声明该能力。05 入口记录 F02-T10 曾豁免运行验收且 Ubuntu peer 建连超时，因此本轮不开放 capability，也没有实际操作服务端文件。
- 验证：`bash Tests/run-morde-workspace-transfer-tests.sh` 通过（覆盖重命名目标、移动目标与非法/穿越路径拒绝）；`Morde.xcodeproj` iOS Simulator build、`Minis.xcodeproj` iOS device `build-for-testing` 及 `git diff --check` 通过。未做真实服务端、模拟器 UI 操作或文件传输；I05 保持进行中，等待 F02-T10 capability 门禁解除后验收。

### 2026-11-03 Task 操作回执补充

- I04：Task 操作已成功但随后读取状态失败时，客户端保留同一幂等键；未知结果未对账前拒绝改发不同操作，避免覆盖待核对请求。确定的非重试 4xx 会清除该次尝试。停止请求的非确定结果仍立即刷新 Task，并按刷新结果显示未知状态。
- 本机验证：`sh Tests/run-credential-expiry-tests.sh` 通过，覆盖操作指纹、幂等尝试复用、不同操作拒绝和相关 Task 策略。双客户端审批/停止交互仍待真实或本地 HTTP 替身验收，I04 保持进行中。
- I07 合同复核：`GET /api/workspaces/{id}/changes` 的 OpenAPI 说明限定于配对 PC/Host 副本 cursor；客户端没有把它作为手机历史时间线消费。I07 时间线继续等待专用手机 revision feed，Host 独立状态也未完成。

### 2026-11-03 iOS 补充实施进展

- IOS-S02 已接入按账号 scope 分页读取云端日记、按日期/body/revision 严格校验、显式选择本机或云端副本作为草稿，并通过已有 personal-data outbox 排队；账号切换会隐藏旧 scope 云端内容。导入的云端例程保留为关闭的只读本机归档，不重复发提醒或本机运行。跨设备、完整例程同步与冲突矩阵仍待验收。
- 本轮回归：8 个 iOS 本地 runner 通过；`Morde.xcodeproj` Simulator Debug build 和 `Minis.xcodeproj` generic iOS `build-for-testing` 通过。Morde UI acceptance 无凭据时按预期跳过（0 passed、1 skipped），未启动 app 或访问服务；Minis XCTest 未运行，模拟器链接受已有 device-only `libish_emu.a` 阻断；未访问测试服务器或使用外部凭据。
- IOS-S03/S04/S05/S07/S08/S09/S10/S11/S12 的接口依赖、未开放操作及 fail-closed 状态仍按补充清单执行；本轮未修改服务端、Portal、Host 或其它客户端。
