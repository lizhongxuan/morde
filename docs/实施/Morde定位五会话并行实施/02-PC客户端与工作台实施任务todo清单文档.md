# 会话 2：PC 客户端与工作台实施 TODO

> 阅读：[并行执行入口](00-五会话并行执行入口.md)、[设计方案](../../Morde定位对齐与功能精简设计方案.md)。本清单覆盖 A06/A08/A10/A11/A15、B01–B07/B09–B11、C01–C10 及 P1/P2 的 PC、Portal 和 Workbench 部分。
>
> 边界：`project/nanoMuse/harness/desktop/**`、`project/nanoMuse/harness/dsh-nanomuse/**`、`project/morde-desktop/**`、`web/portal/**` 和 `web/workbench/**` 均由本会话独占；只更新本清单。不改 iOS、服务端、迁移或 OpenAPI；公共 API 类型由本会话根据会话 5 已定合同生成。保留现有改动。

## 接口与交接

- 使用设计 10.1 的主会话接口与现有会话列表/消息接口。普通消息的 `delivery_status` 由 R04/W02b 提供；澄清和审批继续用现有独立接口，切会话和刷新不创建 Task。
- 工作区 diff/回滚、文件写入和电脑复制调用 W05–W10 的服务端合同；一对一绑定、统一路径、单人操作、Workspace 复制和移交调用 W17–W22 的合同。项目、目标、预算与多 AI 工作项调用 P02–P10 和 W08 的合同。
- 页面必须能展示未开放、无权限、保存中和未知状态；服务端能力未就绪时可用内存替身开发，但不能把 mock 显示当作真实验收。禁止发送业务文件。

## 本机验证范围

按设计 11.1–11.4 执行：两人或两端用本机浏览器的两个内存身份模拟，不要求第二台电脑；文件和两种 AI 结果用内存替身，不发送文件或接外部服务。Portal/Workbench 要求 Node 24；本机已有 Node 24 的具体路径见设计 11.2，缺包时不联网安装。记录 UI 替身结果与真实后端/Ubuntu 未验证范围。

## P0-A：PC 正式入口与会话

- [x] **PC01（正式入口、白名单测试和构建验收完成；未生成发行包产物）核对并保留正式 PC 入口**
  - 修改：`harness/desktop/package.json`、`electron-builder.yml`、`src/morde-main.ts` 和必要测试。当前入口已是 `morde-main.js`，在此基础上接入原 Muse 界面/能力，检查安装包不含本机 Agent；不要把未使用的旧 `src/main.ts` 当成当前启动事实。
  - 验收：本地构建和检查启动路径/打包白名单；原应用可访问的保留功能有去向，未接入能力显示真实状态，既有账户和目录不被覆盖。

- [x] **PC02（主会话合同、API、冲突保护和本地集成验收完成；真实服务与双设备验收未跑，依赖 R01、W02b）接主会话和多会话**
  - 修改：PC Muse 会话列表、Portal Assistant/API 和前端 schema；主会话从服务端读取、初始化或处理 `selection_required`，普通会话独立；本地最近记录只作显示缓存。
  - 验收：本地浏览器模拟两端一致 ID、并发切换冲突、无候选和待选择；切换或刷新不新建 Task，不展示其他账号缓存。

- [x] **PC03（delivery_status 合同、同一会话多轮、未知投递保护及事件断线续读本地验收完成；真实后端/跨端未跑）接入多轮对话与投递状态**
  - 修改：会话消息、输入框和事件续读；新消息使用统一会话 API 与幂等键，展示 `pending/applied/unknown`。网络失败保留输入，不把 `202` 或模型流结束当作任务完成。
  - 验收：本地浏览器验证两轮同一会话、断线恢复、重复发送和未知状态；对话历史与当前 Task 对应正确。

- [x] **PC04（审批/澄清/暂停/继续/取消及运行中追加已接入；追加消息固定到活动 Task，任务状态竞态由 expected_task_id 冲突保护）会话内处理审批和干预**
  - 修改：Assistant/Muse 聊天中的批准、拒绝、澄清、暂停、继续、取消及运行中改指令；既有版本和幂等键重试机制保留。
  - 验收：本地浏览器模拟过期版本、重复确认、停止待证明和追加未确认；关闭页面不发送取消。

## P0-B：个人托管与工作过程

- [x] **PC05（Portal 正式 diff/restore、Host Gateway owner-only 历史 diff、安全解绑/恢复/重绑流程已接入；定向测试与本地浏览器验收通过；真实目标 Host 未连接验证）接 Workspace 版本、diff 和回滚**
  - 修改：Workbench `WorkspaceBrowser`、Portal Workspace 页面与 PC 同步状态；从已确认版本读取差异，显式确认回滚，按预期版本提交；本机安全副本和同步开关保留。
  - 验收：本地页面模拟版本差异、回滚成功/冲突、同步未就绪及无权；无业务文件发送，真实传输保留待验证。

- [x] **PC06（现有页面与本地测试核验完成，依赖 P02–P05、R07）接个人资料、记忆、目标与提醒**
  - 修改：Muse 原对应页面、Portal `PersonalData`、`GoalsAndSchedules` 和通知入口；使用服务端个人业务权威，保留旧本机内容只读/导入选择，不另写旧 Cloud。
  - 验收：本地页面完成个人对象编辑、目标进度、计划及提醒的成功/权限错误/冲突；未同步草稿不丢失。

- [x] **PC07（Portal 一次性 Task ticket、独立 Task-read Gateway 会话和 Workbench 只读观察页已接通；API/DB/组件测试及本地浏览器验收通过；真实目标 Host 未连接验证）展示会话内过程和电脑状态**
  - 修改：Assistant、Workbench 和桌面窗口；显示 Task 进度、事件、已确认结果与 Host 独立状态；实时桌面只在有权时进入，事件断线续读不重跑。
  - 验收：本地浏览器模拟事件断线、快照重取、审批等待、文件尚未保存和 Host 不可用；不伪造未记录的回放。

- [x] **PC14（单 Workspace 门禁、默认路径说明、旧数据迁移未知态及绑定/恢复已实现；本地测试与浏览器验收通过；真实目标机迁移未验证，依赖 W17、W18）调整电脑创建和 Workspace 绑定**
  - 修改：Portal `NewHost`、电脑列表和 Workspace 绑定/恢复页面；新建或选择恰好一个 Workspace，替换原多选；无绑定旧电脑先选择可用 Workspace 才可恢复/打开。展示默认工作/下载路径、Workspace 外数据临时、同步/移交锁定和旧数据迁移状态；不能承诺任意手选路径都会持久化。
  - 验收：本地浏览器模拟新建、已有 Workspace、零个/多个和跨电脑冲突、已释放仍绑定、旧电脑待绑定以及未就绪/移交中；失败保留用户输入，不把未保存数据说成已持久化。

- [x] **PC15（Portal/Workbench 控制权 UI、续租/只读门禁、Gateway 断租关闭和 PostgreSQL 撤权/过期/AI 交接旧输入拒绝已验收；本地客户端任务完成；W19 其他入口与跨端长连接矩阵仍待独立服务端验收）接多人观察与单人操作权**
  - 修改：`Hosts`、`HostMemberManager`、Workbench/桌面控制条；获准人员可以同时查看，显示当前操作者和控制版本，申请、释放和交接按服务端结果启用；无操作权者保持观察，不能由页面状态伪造控制成功。
  - 验收：本地浏览器以内存数据模拟两人抢权、交接中、撤权、服务端报告旧连接已阻断、控制人断线和 AI 模式；展示画面可能包含 Workspace 内容的共享提示。Gateway 与 PostgreSQL 的已覆盖栅栏用例通过；W19 其他写入口及跨端旧连接矩阵仍待单独验收。

- [x] **PC16（纯内存复制/移交演示已集成；定向测试和本地浏览器验收通过，正式复制/移交 API 仍待 W20、W21）接 Workspace 完整复制与移交**
  - 修改：Portal/Workbench 的 Workspace 列表及详情；提供复制、发起移交、查看申请、接收及取消入口；显示待处理/失败/冻结、目标账号、版本和配额信息，提示副本包含隐藏文件及用户保存的凭据；显示不支持文件类型、停写/检查点未完成及空间能力未就绪的原因。
  - 验收：本地浏览器以内存数据模拟运行中无法快照、完整副本结果、受理后双方不可使用、接受/取消竞争及接受失败仍冻结；取消或接受成功后按服务端确认的结果解锁，不发送文件。

## P0-C：团队项目与交付

- [x] **PC08（正式 Project API、Portal 页面与幂等操作已接入；本地测试/浏览器验收完成；部署后端和跨账号未验证，依赖 P06、P07、W08–W10）接项目、成员与多电脑**
  - 修改：Portal/Muse 项目入口、成员邀请/接受/撤销和负责人交接、Host 总览及复制表单；共享、完整 Workspace 复制与移交区分，给其他用户的副本须经 W21 接受，资源仍按 Host/Workspace 授权。
  - 验收：本地浏览器模拟邀请接受前后、负责人交接、成员、只读和撤权；电脑副本绑定独立 Workspace，接收前不显示电脑已交付成功；个人会话不随项目公开。

- [x] **PC09（Portal 项目会话/工作项/账本、预算预留/结算及 Postgres 交付证据校验器已接入；能力仅在项目模型费率和完整执行服务装配后发布；本地 UI/API 与 PostgreSQL 验收通过，未调用外部 AI；目标环境未验证）接项目工作项、交付和账本**
  - 修改：项目 Todo、AI 执行目标、阻塞与交付页面，以及管理端必要预算界面；显示任务发起人、项目、费用依据和未知状态。
  - 验收：本地浏览器模拟两种 AI/电脑目标、阻塞及补授权、交付版本、预算不足和用量未知；无真实外部 AI 调用。

## 清理和后续

- [x] **PC10（本地实施、独立安全审查与端到端验收完成；旧独立 Desktop 发行入口和个人模型 Key 用户入口已退役；真实部署、跨设备/文件传输、Ubuntu 与生产签名仍待发布验收）退役旧本机执行和重复入口**
  - 修改：`desktop/src/main.ts` 旧 dsh 启动、`dsh-nanomuse/src/task.ts` 与旧 Cloud/Hub 中专属本机 Agent 委派、用户自带模型 key UI；退役已迁入能力的 `project/morde-desktop` 独立打包/下载及重复 Portal 用户页面。保留当前正式 Morde 入口、可复用 Muse UI、Workspace 同步、安全副本、必要 Workbench 和管理后台。
  - 依赖：R06；每项旧用户功能已迁入并核验后才删除；向 R11/W13 提供调用方退役证据。
  - [x] 核验当前正式 Morde-client 编译和 Electron 白名单不含旧 dsh `main.ts`、`dsh-nanomuse` 或旧本机模型 key 页面；本机目录 IPC 只向 renderer 返回 Workspace ID 与目录名。
  - [x] 核验 Portal 只有一个 `/download` 用户页；移除旧独立 Desktop 的 Portal staging npm 命令，并让旧 schema v1 staging CLI 失败关闭，防止覆盖 Morde-client schema v3 清单。
  - [x] 将旧独立 Desktop 的长驻实时同步、逐 Workspace 状态、冲突与中断恢复迁入 Morde-client；确认保留本机历史/文件，且不在 `workspace-sync` 仍关闭时开放真实传输。本地验收：`npm run build`、`npm test` 274/274、`go test -count=1 ./...` 通过；IPC/renderer 替身覆盖 capability 拒绝、目录身份变化、多 Workspace、冲突、重启不自动恢复。未执行真实 peer 或传输。
  - [x] 前项通过独立安全与端到端验收后，移除旧独立 Desktop 的发行打包 npm 命令及 schema v1 Portal 暂存实现；Portal `/connections` 只显示管理员配置的共享模型，不再提供个人模型/API Key 表单或用户侧探测操作。保留旧源码、同步历史和本机 `release` 安装包，不触碰已有文件。
    - 验收证据（2026-11-03）：独立只读审查核验了 Morde-client 打包白名单、Portal 单一 `/download` 路由与旧发布调用方；`project/morde-sync` 全包测试通过，包含真实同步引擎对本机 `httptest.Server` 的初次同步、冲突保全、断网重连与重启恢复；Morde-client 构建和 274 项测试通过，覆盖 capability 失败关闭、逐 Workspace 隔离及 IPC/Renderer 安全边界；旧发布入口测试 2/2、签名暂存测试 7/7 通过。Portal 模型连接与 MSW 测试 52/52、typecheck、修改文件 ESLint 通过。内置浏览器确认普通用户没有 API Key、添加或探测按钮，仍能查看共享模型；管理员共享模型配置页及密钥字段保留，未提交任何密钥。
  - 验收：构建/打包和引用检查无本机 Agent 执行或自带模型 key 入口；旧本机历史及文件不删，未完成迁移的部分保留并标待验证。

- [x] **PC11（P1 本地内存验收面板、定向测试和本地浏览器验收完成；正式连接器/通知/模板合同仍待对齐）实现 P1 客户端能力**
  - 修改：设备接力/既有获准 PC 单次动作、通知分级、项目技能/连接器及文档模板；依赖 P12、R12、W15 的对应合同。F02-T10 的关闭不代表真实文件传输已验收。
  - 验收：本地浏览器只用内存数据验证设备授权、项目连接器/通知和模板空态；不访问外部服务或发送文件。

- [x] **PC12（P2 本地内存验收面板、定向测试和本地浏览器验收完成；正式批注/搜索/媒体合同仍待对齐）实现 P2 和待排的 Muse 客户端能力**
  - 修改：行级批注、跨项目搜索、语音输入及媒体入口；Feed/Ideas/Library/简报按建议优先级复用原 UI 并接 P13、R13、W16 的统一业务。
  - 验收：本地浏览器以内存数据验证项目隔离、无权与未就绪状态；不调用真实媒体服务或发送文件。

- [x] **PC13（受影响 fixture 已更新；全量前端验证与代表性本地浏览器验收完成；真实跨端/文件传输/目标环境未跑）完成 PC 界面和回归验收**
  - 修改：本会话范围内必要测试及验收记录；与会话 5 对齐 OpenAPI 后生成 `web/portal` 和 `web/workbench` 对应 schema。
  - 验收：定向前端测试、类型检查和内置浏览器覆盖主会话、消息、审批、个人/项目、文件、单 Workspace、控制权、复制/移交及错误状态；真实跨端、文件传输和目标环境未跑时明确待验证。

## 交接记录

按入口第 4 节追加服务端合同、项目权限、文件操作及验收缺口；不改其他会话的清单。

### 2026-11-02 本轮进度

- Portal Assistant 以服务端主会话为准；补充主会话初始化、版本冲突处理、同一会话追加、未知投递保护和任务事件续读。运行中追加现在携带 expected_task_id，只追加到仍活动的目标 Task，状态变化返回冲突并保留输入。
- Portal 电脑创建改为新建或选择恰好一个就绪 Workspace；旧的已停止电脑可先绑定一个就绪 Workspace，再恢复并等待应用完成。Workbench 展示控制版本、操作者账号 ID、桌面共享提示和已确认 Workspace 只读状态。
- PC06 已完成；PC07 事件流、状态和只读观察通过本地模拟验收。PC08、PC09、PC11、PC12、PC16 当时的客户端内存流程和定向验收记录不代表所有服务端正式链路；PC09 预算执行服务与能力声明的后续完成情况见 2026-11-03。
- PC14 的停机前绑定与恢复合同现已具备；本地 UI、单测和浏览器验收通过。默认路径说明和旧数据迁移状态尚未覆盖。PC15 现在可展示服务返回的操作者账号 ID 与控制版本，多人争抢/断线和旧连接拦截仍需 W19 验收。

### 2026-11-03 本轮进度

- PC05 增量已闭环：Host Gateway 增加 owner-only、binding/revision 校验的只读历史 diff；Portal 安全恢复按解绑、release、W05 restore、bind、Host restore 顺序推进，未知状态暂停。Workbench 使用 Gateway diff 并以文本节点安全呈现。Portal/Workbench 定向测试、本地页面和 Gateway 授权测试通过；真实目标 Host 未连接验证。
- 主会话、`delivery_status` 与追加消息 `expected_task_id` 类型已从当前 OpenAPI 生成。PC03 同一会话第二轮 Task、SSE 断线重连和重复序号测试通过；本轮已将运行中追加接入 Assistant，并要求服务端只接受仍活动的目标 Task，状态竞态返回冲突而不创建新 Task。Portal/MSW 定向测试、内置浏览器本地追加流程、app/API 测试及 PostgreSQL 投递集成测试通过；真实部署后端/跨设备未跑。
- 项目、工作项、预算与共享项目会话已接入 Portal API；本机 MSW 覆盖合成 queued 回执、共享历史与幂等重试。服务端 ModelGateway 已接入项目/账号双重预算预留与结算，工作项交付由 Postgres 校验项目 Task、Workspace checkpoint、登记资源及唯一成功发布回执。新增 project_budget_execution 能力门禁：完整服务栈和至少一项项目费率同时就绪才发布，空配置继续 fail closed；客户端/API 测试及定向 Postgres 用例通过，未调用外部 AI。Workspace 完整复制/移交和多入口人工控制旧连接验收仍依赖 W20/W21、W19。
- PC14 已补默认工作/下载路径说明及旧数据迁移未知状态；Ubuntu 目标机和真实旧数据迁移未验证。PC15 客户端控制与续租已完成；Gateway 与临时本机 PostgreSQL 已验证断租关闭、撤权/过期及 AI 交接后旧输入拒绝。W19 其他连接入口及跨端联验仍待服务端会话。
- PC07 增量已闭环：Portal Session 签发短时单次票据，Host Gateway 兑换成独立 Task-read cookie；snapshot/SSE 逐次复核 Task、Host、runtime generation 和 Portal Session 撤权。Workbench 观察页只显示状态、事件和已验证产物元数据，不提供文件、终端或控制操作。API、PostgreSQL、组件测试及本地浏览器页通过；没有把 Host cookie 转成 Task 权限。
- PC10 调用方审计确认旧 `main.ts → dsh Host → dsh-nanomuse` 和自带模型 key UI 仍留在源码，但不进入当前 Morde-client 构建/安装包。本轮把单次同步、恢复点续跑、进度/冲突/不确定状态及人工停写后的云端检查点 UI 迁入 Morde-client；未知结果不自动重放，handoff 目标仅作本机记录，固定实时同步开关仍关闭。下载暂存新增 Ed25519 验签与包 SHA-256 重算，临时密钥测试 5/5 通过；没有正式签名身份、真实构建/验收证据或生产暂存。只读调用方审计发现旧 `project/morde-desktop` 暂存器会以 schema v1 覆盖同目录的 schema v3 清单，已禁用其 Portal 暂存 npm 入口和 CLI，并以定向测试验证失败关闭；保留旧本地打包和同步能力。当时 Task 级实时同步/冲突/恢复迁移仍待补齐；后续 PCX-07 收尾见下一条。
- PCX-07/08 补充收尾：Morde-client 现在持久化逐 Workspace 实时同步状态和恢复点，UI 支持显式启停/恢复，冲突及未知结果不会自动重放；`workspace-sync` capability 关闭时在主进程失败关闭。目录身份变化、多 Workspace 隔离、冲突、重启不自动恢复由 IPC/renderer 替身覆盖。`npm run build`、全量 `npm test` 274/274、`go test -count=1 ./...` 通过。`npm run stage:portal -- <Portal URL>` 已验证参数传递和临时目录签名包暂存，缺参/不可信 sidecar 无输出。真实部署、跨设备/文件传输、Ubuntu 和生产签名身份仍未验。
- PC10 本地退役收尾：独立审计和本机同步端到端用例通过后，移除了旧独立 Desktop 的发行打包命令与 schema v1 Portal 生成/暂存代码，保留旧本机源码、同步历史和已存在安装包。普通用户的 Portal `/connections` 已取消个人模型/API Key 收集和探测入口；页面仅展示管理员共享模型，管理员配置入口保持可用。真实部署服务、跨设备运行、文件传输、Ubuntu 和生产签名仍为后续发布验收。
- 本机实时同步状态与恢复迁移已完成本地验收，但不证明服务端确认，也没有执行真实文件传输。Ubuntu 目标机及生产签名身份与证据链仍未完成。

### 待服务端合同/集成验收

- Portal 主会话、消息投递状态、项目共享会话、Workspace diff/restore 与 Task observation 已纳入当前 OpenAPI 并生成 Portal/Workbench schema；本地 API/MSW/Gateway/PostgreSQL 测试通过。真实部署后端和跨设备服务行为尚未验证。
- project_budget_execution 现由服务端按就绪条件发布：需配置项目模型费率，并装配 Cloud Task、项目会话/工作项/账本、模型连接和 ModelGateway；不满足条件的空配置仍 fail closed。服务端已为模型调用原子预留项目与账号预算并结算，未知用量保持 hold；Postgres 工作项交付校验 Task 项目/Workspace checkpoint、项目资源登记及唯一成功发布回执。
- PC09 客户端验收：在两种可用模型中选择备用模型，并与已授权 Host、当前 Workspace revision 一起提交；客户端用例覆盖能力缺失时 fail closed、未知费用显示、交付 revision、工作项阻塞/恢复和交付校验失败；应用层和 Postgres 用例覆盖双重预算拒绝、预留/结算及 unknown hold。本轮不调用真实外部模型。
- Assistant 运行中追加已通过 `expected_task_id` 与服务端状态栅栏固定到现有 Task；若 Task 已结束或切换，服务端返回 conflict，客户端保留输入并要求核对。
- PC05 的 Workspace 历史 diff 和安全恢复合同已实现；Workbench diff 由 Host owner Gateway 读取，Portal 恢复复用正式 W05 与 Host 生命周期 API。恢复全流程由 Portal 组件测试覆盖；浏览器验收检查了只读 diff 和写入者确认门禁，没有对真实 Host 执行恢复。
- PC07 的 Task observation 权限交接已实现并通过本地服务端/客户端测试；远端 Host、撤权后活跃 SSE 连接及跨设备真实部署仍未运行。Workspace 完整复制/移交真实 API 与多入口旧控制连接栅栏仍待 W20/W21、W19。
- PC02、PC03、PC14 的本机页面与测试已完成；生产后端、Ubuntu 目标机、真实旧数据迁移和跨设备行为仍未验收。

### 本轮验证

- Portal：`typecheck`、`lint`、45 个测试文件/363 个测试、copy-mobile Node 测试 2 项通过，生产构建通过；生产包约 772.45 kB JavaScript 分块警告。 PC09 变更后定向 Projects.test.tsx 14 项和 typecheck 通过。
- Workbench：`typecheck`、`lint`、10 个测试文件/105 个测试、生产构建通过。
- Gateway：`go test ./internal/gateway -count=1` 通过（20.74 秒）；覆盖活动租约续租失败时 SSE/WebSocket fail closed。
- 本轮复核：Gateway 与 MCP 全量包测试通过；W19 PostgreSQL 测试因本轮未设置 CH_TEST_DATABASE_URL 而跳过，已覆盖的租约 PG 场景在前一轮临时本机数据库通过；未声称完整 W19 入口矩阵已完成。
- 本机 `VITE_MOCK_API=1` 浏览器验收：Workbench 打开 Workspace 合成差异，显式确认后分别展示本机模拟恢复成功和 409 冲突；选择超配额 Workspace 时禁用预演并隐藏文件。模拟只读 manifest 元数据，不发送写请求、不修改 Workspace 或本机文件。项目共享对话创建及合成 Task `queued` 回执通过，页面明确标注不调用模型、不读写文件。全流程仅为本地 MSW 数据。
- Desktop：正式入口构建通过，234 项测试通过；发行包产物未构建，项目没有 `lint`/`check:package-config` 脚本。
- 本轮补充：Assistant 活动 Task 追加及 MSW 幂等/旧 Task 冲突用例通过；内置浏览器只在本地 MSW 中创建 queued Task 并追加一条 pending 消息，页面显示确认投递前不再追加。临时本机 PostgreSQL 上 `TestPostgresPendingConversationMessagesBlockTaskCompletion`、`TestPostgresHostControlLeaseActorsRenewalRevocationAndExpiry`、项目预算账本及 WorkItem 交付证据用例通过。测试容器已清理；未验证目标机或真实跨端行为。
- Server：TaskService、API、Gateway 全量包测试通过；PostgreSQL 包可编译且上述筛选集成测试通过，未运行整个 PostgreSQL 集成套件。 本轮 GIN_MODE=release go test ./internal/api ./cmd/portal -count=1 通过。
- OpenAPI：更新 capabilities 对 project_budget_execution 的说明；Portal 和 Workbench schema 已重新生成。
- Morde-client 同步迁移：`npm test` 245 项通过，`npm run typecheck` 与 `npm run build` 通过；新增测试覆盖 checkpoint 旧格式、scope 校验/重启恢复、handoff 人工确认和固定同步互斥。UI 通过 Electron renderer DOM/IPC 替身测试；没有运行真实同步、文件传输、打包签名或 Portal staging。
- 本轮 Morde-client：`npm test` 252 项通过，`npm run typecheck` 与 `npm run build` 通过。新增 handoff 目标持久化、未知结果重启续跑、旧 checkpoint 目标默认值、四种发行目标 schema v3 staging fixture；没有运行真实同步、文件传输、签名、实际 Portal staging 或发行包构建。
- 内置 Electron UI：使用临时 localhost mock 登录并打开本机同步页，确认恢复点显示原 scope、锁定“此电脑”目标和“需要检查”记录，且未勾选人工停写前恢复按钮不可用；没有启动同步或访问真实服务。
- PC10 后续验收：Morde-client `npm test` 256 项、`npm run typecheck`、`npm run build` 通过；macOS arm64 本机目录包构建成功，ASAR 仅含 `morde-*.js` 和 Morde 资源，无旧 `main.js`、dsh-nanomuse 或旧模型 key 入口。打包配置 `identity: null` 导致跳过代码签名；临时包未发布且已清理，未执行 Portal staging。
- Morde-client 本轮状态快照：Syncthing 状态与无效计数单测、授权/绑定/路径 IPC 单测以及本地内置浏览器模拟通过；未启动真实同步节点或发送文件。状态快照为手动本机数据，不能代替旧 Desktop 的 Task 级状态、冲突恢复或云端确认。
- 2026-11-03 收尾复核：Morde-client `npm run build && npm test` 通过（265/265）；旧 Desktop Portal 暂存器 5/5 测试通过，已移除会覆盖当前 schema v3 清单的旧 staging npm 命令并令 CLI 失败关闭；迁移 `DownTo(124)`/`Up` 往返集成测试在临时 PostgreSQL 16 通过。全量测试发现的 renderer 目录字段断言已改为核验仅返回 Workspace ID 与目录名。
- 未登录测试服务器、未调用外部模型、未发送业务文件；真实后端、Ubuntu 目标机、真实文件传输及跨设备运行态验收仍待执行。
