# Morde PC 端 Codex 会话接续实施任务 TODO

需求依据：[Morde PC 端 Codex 会话接续需求文档](../Morde%20PC端%20Codex%20会话接续需求文档.md)。实现边界依据：[设计方案](../Morde%20PC端%20Codex%20会话接续设计方案.md)、根目录 AGENTS.md、server/AGENTS.md、clients/nanoMuse/AGENTS.md 与 runtime/morde-cloud-runtime/AGENTS.md。

当前状态：CX-00 至 CX-06 的实现已完成，PC 新增本地 Codex 会话管理窗口；CX-07 统一验证待后续执行。运行中外部 GUI turn 会通过有界快照回传可见项，终态仍需连续稳定快照；外部 GUI 的审批/问题若无原 RPC handle 则明确不可代答。云端保留合法问题文本并按 snapshot/live/approval scope 校验；iOS 在仅撤销 live 时保留 read 历史、撤销 read 或确认会话不存在时清理历史，并按请求 ID/version 阻止重复回答。尚未运行测试、构建或迁移，也未执行真实设备验收；Codex Desktop 外部 GUI 增量、同线程发送和审批尚未实测，完成 CX-07 前不得宣称全功能可用。

## 使用方法

1. 在 Morde PC 登录账号，打开应用菜单的“设置 → Codex 会话与授权”。
2. 点击“添加 Codex 配置”，先选择已有 Codex 登录状态的 `CODEX_HOME` 文件夹，再选择本机 `codex` 可执行文件；常见配置目录为 `~/.codex`。该操作不上传 Codex 凭证。
3. 选择一个 profile 并刷新本机 Codex 会话。每个要接续的会话单独授权；`读取历史`、`实时更新`、`发送消息`是默认勾选范围，`中断任务`和`处理审批`需要主动勾选。撤销会话授权可随时停止后续同步与操作。
4. 在 Morde iOS 打开“Codex 会话”，选择已授权的 PC 和会话，即可查看历史与状态；按授权范围发送消息或处理审批。PC 端 Morde 需保持运行并联网以领取命令和同步授权范围内的 Codex 可见事件。

本地管理窗口与 Codex App Server 的实际运行交互仍待 CX-07 在真实 Morde PC/Codex 环境验收；未通过该验收前，不承诺每种 Codex Desktop GUI 更新和审批都可接续。

## 修改边界与现状

- 只在 Morde 本地仓库实施；不得将项目文件、测试文件、日志、截图、附件或 Codex 凭证发送到外部服务。产品正常运行时 iOS 文字命令及已授权的 Codex 可见事件可经本产品 Morde 云端转发，这是需求链路；不包括项目文件正文或认证材料。
- 云端只处理身份、授权、持久命令、会话映射和可见事件；Codex Provider 在用户 PC 上调用官方 codex app-server。不得为此功能创建 DSH Task、云端 Agent 循环或第二套 Morde Agent Runtime。
- 现有 /api/hub 是已认证的 Client 安装 WebSocket，但本功能不依赖 Hub 唤醒：PC 使用已认证 HTTPS API 每 2 秒领取持久命令，避免内存态 presence/call 语义成为交付依赖。不得把 Hub 当持久命令队列或 Codex 事件日志。
- 现有 /api/conversations/:id/messages 进入云端 Task 路由，基础 Conversation 与云端 Host 关联，消息存储存在 Task 关联。Codex 走独立 Codex session/command/event API，不改普通 Morde Agent 对话语义。
- 桌面发行入口为 clients/desktop/harness/desktop；Electron main 是 src/morde-main.ts，Morde 助手通过 Portal WebContentsView 加载。本次 Codex Provider、会话管理窗口和受限 IPC 入口位于 Electron 已打包的 main/preload/resources 路径；客户端通过 HTTPS 持久命令接口工作，不依赖 Hub。clients/desktop/harness/dsh-nanomuse/src/hub.ts 不在该 Electron 编译/打包路径内。旧 clients/desktop/nanomuse/coding/agents.py 和 runner.py 扫描私有记录并调用 codex exec resume，不作为本功能 Provider。
- iOS 工程为 clients/nanoMuse/android/src/ios/Morde.xcodeproj、scheme Morde，target 使用显式 source membership；新增 Swift 源文件要登记。现有 Morde 任务 SSE 游标允许跳号，不可直接复用为 Codex 严格 gap cursor。只改 Morde iOS 所需源码、测试和 Localizable.xcstrings；Android 不在本范围。
- 服务端遵循 server/AGENTS.md：Gin handler → app → domain/store，PostgreSQL/pgxpool、goose、sqlc，OpenAPI 唯一合同。新增或变更接口同步 server/openapi/portal.yaml、docs/实施/00-总览与公共约定.md 和相关客户端类型。
- 仓库当前存在其他未提交工作；实现时先复核 git diff 与文件所有权，只提交本功能触及的改动，不覆盖其他工作。

## 接口、状态与验收约定

以下接口与授权合同已由 CX-01 定稿；服务端 OpenAPI 为实现时的唯一 REST 定义：

| 方向 | 接口/协议 | 核心约定 |
| --- | --- | --- |
| iOS → Morde | GET /api/codex/sessions | 当前用户按授权安装分页列出 Codex 镜像会话 |
| iOS → Morde | GET /api/codex/sessions/{id}/events?after_sequence=&limit= | 按严格递增 sequence 读取事件；游标过期可重新取快照 |
| Morde → iOS | GET /api/codex/sessions/{id}/events/stream?after_sequence= | 同时需 read+live；SSE 仅用于实时增量，断线后按持久游标续读 |
| iOS → Morde | POST /api/codex/sessions/{id}/commands | Idempotency-Key 必填；接受 send、interrupt、approval、answer_question 四种显式类型；answer_question 使用 send scope |
| iOS → Morde | DELETE /api/codex/sessions/{id} | 删除当前用户有权访问的镜像、事件与未执行命令；不声称停止本地运行中的 turn |
| PC → Morde | POST /api/codex/sessions/discover | 由已认证安装身份登记/更新授权范围内的 native thread，返回 Morde session ID；不接受请求体自报 owner |
| PC → Morde | POST /api/codex/commands/claim | 只领取绑定到该 owner 和 installation 的持久待处理命令 |
| PC → Morde | POST /api/codex/commands/{id}/result | 回报 Provider 实际接受、完成、失败或 unknown，重放幂等 |
| PC → Morde | POST /api/codex/sessions/{id}/events | 批量追加规范化事件；服务端持久化后确认 |

持久对象为 Codex session mapping、command/outbox、event log。读历史需 read；SSE 同时需 read+live，由 read 授权事件内容、live 授权流式传输；发送、中断、审批还须独立授权，answer_question 属于 send；每个命令还要求 read scope，以便用户能查看其执行进度。结构化问题使用白名单 question live event 携带请求 ID/version、问题与选项，answer_question 携带 ID/version 和 question ID 对应的答案数组；PC 只回复同一授权版本下仍待处理的原 App Server 请求，一次性响应。Cloud API 按当前 owner 和目标 PC 安装过滤，iOS 自己的 Client installation 可作为同一用户的读取/命令 actor。Hub 在线状态不是命令状态，SSE 连接状态不是任务状态。命令至少区分 waiting_pc、claimed、forwarded、running、waiting_approval、completed、failed、unknown；command_status 事件持久记录命令受理、PC 领取和结果；审批区分 pending、approved、rejected、expired、stale；问题区分 pending、answered、stale。对发送结果不明不得重新创建 turn。

实现测试要覆盖 Morde account/install 隔离、授权撤销、幂等键、事件乱序/重复/缺口、PC 轮询断线与重连、App Server 方法/事件兼容、审批过期、结构化问题回答的一次性 ID/version 绑定和真实 Codex GUI 接续。用户安排 CX-07 时再统一运行验证；不得运行会发送项目文件、修改远端机器或上传附件的验收用例。

## 推荐执行顺序

~~~mermaid
flowchart LR
    A["CX-00 可行性与产品门"] --> B["CX-01 定稿身份、权限、留存与 API"]
    B --> C["CX-02 云端持久模型和 API"]
    B --> D["CX-03 PC 官方 App Server Provider"]
    C --> E["CX-04 HTTPS 领取、命令执行与事件回传"]
    D --> E
    C --> F["CX-05 iOS Codex 会话页"]
    E --> F
    F --> G["CX-06 跨端恢复、撤权与审批验收"]
~~~

## 可执行任务

### CX-00（已完成：列表/历史读取；GUI 实时交互待 CX-07）验证 Codex 接续能力及实施落点

- **修改边界：**只新增/更新本清单或设计文档中的验证记录；不改实现代码，不导出或上传 Codex profile、认证材料、项目文件、对话日志或截图。
- **核对范围：**Morde 支持的 PC 发行平台、Codex 版本及 App Server 协议；发行包实际启动的 PC Hub/Provider 进程和 renderer 接口；clients/nanoMuse/android/src/ios/Morde.xcodeproj 的 Morde target/source membership；Morde 现有 Codex 历史留存/删除规则。
- **能力验证：**在本机通过受支持的 Codex 官方接口，记录是否能列出、读取已打开 Codex GUI 的原生 thread，观察该 GUI 继续操作产生的更新，从 Morde-owned App Server thread/resume 到 GUI-owned thread 的行为差异，并验证一轮本地发送/事件及审批。只记版本、接口能力、状态与脱敏元信息；不得把新建的 Provider 线程伪记为现有 GUI 会话。
- **产品门：**列表/历史读取必须来自用户所选 profile 的原生 thread ID；已验证这一阶段，允许进入实现。桌面并行更新、同线程继续发送与审批列为 CX-07 的发布门；失败时标记对应操作 unavailable，不把新线程伪装成原线程。
- **验收：**留下可重复的本地步骤、版本、每项读/写/观察/审批结论、PC 平台差异、云端留存决策状态和继续/阻塞结论；明确区分真实 GUI 线程与 App Server 自建线程。
- **依赖：**无。
- **本机证据（2026-11-14）：**codex-cli 版本为 0.167.0-alpha.3.2；显式 `CODEX_HOME=~/.codex` 的只读 App Server `thread/list` 返回 326 个线程，277 个 thread ID 与 Codex Desktop 的本地全局状态线程引用匹配；`thread/read(includeTurns:true)` 成功读取一条列表线程的历史结构。之前未固定 profile 的探测返回 0，具体差异未定位，发行实现必须显式传入用户选择的 CODEX_HOME。Codex Desktop 仍使用独立 stdio App Server 子进程，daemon control socket 不存在；未发真实 turn、interrupt 或审批，也未验证外部 GUI 增量。上述证据只证明本机列表/历史读取和 ID 重合，其他平台及运行中 GUI 的双向操作需 CX-07 实机确认。
- **T3 参考核验：**`project/t3code/apps/server/src/provider/Drivers/CodexDriver.ts` / `CodexProvider.ts` 在 T3 Provider 中启动自己的 App Server；`CodexAdapterV2.ts` 仅对已经映射的 native thread ID 调用 thread/read 与 thread/resume。T3 文档说明复用 Codex 安装、登录和 CODEX_HOME，没有发现 Codex Desktop GUI 的列表/附着实现。Codex `thread/loaded/list` 返回的是当前 App Server 进程内存中已加载的 thread ID，不是跨进程桌面会话目录。
- **Remote Control 核验：**本机 CLI 提供 daemon start/enable-remote-control 与 remote-control pair 命令；官方 CLI 文档将其定义为由 Codex 官方桌面/移动客户端配对使用的 App Server daemon。它不提供 Morde 可调用的任意 GUI 会话订阅协议。未运行 bootstrap/start/pair，避免在用户 Codex profile 写入 daemon 配置或建立外部配对。
- **发行落点：**Electron main 已确认在 clients/desktop/harness/desktop/src/morde-main.ts；当前包不含 /api/hub client，dsh-nanomuse Hub 也不在编译/打包范围。Morde iOS target 在 Morde.xcodeproj，显式登记源码。CX-00 验证时服务端尚无 /api/codex 路由；CX-02 已新增并注册这些路由。
- **留存决策：**Codex 镜像保留至用户显式删除 session 或账号删除，不设自动 TTL；删除清除云端镜像、事件与未执行命令，撤权保留历史但拒绝后续访问/操作。此为独立 Codex 数据合同，不套用普通 Conversation 或额外完整记录留存规则。
- **剩余验收：**Codex Desktop 运行中的外部 GUI 增量、同一原生 thread 的发送与审批未实测，列入 CX-07；列表/历史读取证据足以开展实现，但不代表完整 GUI 双向接续已通过。按根 AGENTS 与本任务本地限制，本轮不连接远程测试机、不使用外部密钥，也不读取对话正文、私有数据库或日志，不注入 GUI。

### CX-01（已完成）定稿 Morde session、授权和 API 合同

- **文档边界：**docs/Morde PC端 Codex 会话接续设计方案.md、本清单、docs/实施/00-总览与公共约定.md、server/openapi/portal.yaml；并核对 Server Client 权限白名单与安装身份合同所在代码。
- **合同内容：**Morde session 绑定 owner、PC installation、profile key 和原生 thread；grant 分 `read/live/send/interrupt/approval`。云端镜像保留至用户删除 session 或账号，不自动过期；撤权保留历史但拒绝后续访问与命令。事件单条上限 64 KiB、批次上限 512 KiB，command 使用 Idempotency-Key；已派发但结果未知时进入 `unknown`，不自动重发。
- **API：**REST API 逐路由定义于设计方案 §5 和服务端 OpenAPI；PC 每 2 秒以 HTTPS 领取持久命令，不依赖 Hub wake；SSE 用持久 sequence 游标恢复，事件批次提交后才 ACK。包含 discover、list、event page/stream、command create/claim/result、event append 与 session DELETE。
- **验收：**设计方案 §5.4 固定初版 JSON 合同；本清单固定路由、授权、状态与界限。服务端 OpenAPI 与公共合同同步属于 CX-02 的落地验收，不重复冻结。协议不创建 DSH Task、不走普通 Task 消息路由、不上传文件正文或凭证。
- **依赖：**CX-00 已确认显式 profile 下的线程列表/历史读取；GUI 运行时实时增量、同线程发送和审批留待 CX-07，不阻止开始实现。

### CX-02（实现完成，验证待 CX-07）实现云端 Codex 会话、命令和事件持久层

- **状态记录（2026-11-14）：**已新增迁移、owner/installation 隔离的 session/grant/command/event 存储、事件分页与 SSE、幂等命令、claim/result、删除及 OpenAPI/Portal schema。撤权或权限版本更新会将未领取命令标记 failed、已领取/执行中的命令标记 unknown；领取 60 秒无结果的命令在 PC 下次轮询时转为 unknown 且不重排；command_status 使用递增 status_version 保留重复进入同状态的迁移。SSE 需要 read+live，由 read 授权事件内容；question 事件严格校验 pending/answered/stale payload、live source、唯一且原样保留的 question ID；answer_question 命令使用 read+send scope 并按原 ID/version 持久转发。历史快照中的外部问题以 unsupported 诊断事件按 read scope 可见；live 中的同类内容不得伪装成可回答 question RPC。CWD 标签允许冒号但拒绝路径分隔符。列表读取、非空历史页及非空 SSE 事件批次增加只含操作者/installation、目标、游标或序号范围和条数的审计，不记录事件正文，也不审计空轮询。已添加 Go domain/app/API 与 PostgreSQL 集成测试源码；按用户安排未运行测试、构建或数据库迁移。

- **修改边界：**server/migrations/、server/internal/domain/、server/internal/app/、server/internal/store/postgres/ 及 sqlc 查询/生成物、server/internal/api/、server/cmd/portal/ 依赖装配、server/openapi/portal.yaml、对应 Go 测试、客户端 API 类型生成。
- **实现：**以 owner 和 installation 隔离 session mapping、持久 command/outbox、按 session 单调递增 event log。PC discover API 只能从经授权的客户端安装身份登记 native thread；所有 API 先授权再读写；命令幂等键、Provider event ID 和序号有数据库唯一约束；重复事件返回原确认；event sequence gap 拒绝或进入可恢复待补状态；超时结果可进入 unknown。
- **接口：**按 CX-01 实现 session list、iOS event page/SSE、命令创建、PC claim/result、PC event batch；PC 事件写入必须验证客户端安装、owner、session mapping、event schema 与批次大小。OpenAPI 和注册路由同步。
- **不做：**不调用模型、不创建 Task、不加入 DSH runtime、不改变普通 conversation message 路由、不保存 Codex auth 文件或项目正文。
- **验收：**Go domain/app/API/PostgreSQL 测试覆盖跨账号/安装拒绝、session 映射错误、并发幂等 claim、重复命令、重复/乱序事件、事务提交后 ACK、SSE 断线游标恢复、撤权和 retention 删除；迁移/回滚在本地隔离 PostgreSQL 验证。
- **依赖：**CX-01。

### CX-03（实现完成，验证待 CX-07）在唯一已打包 PC 进程接入官方 Codex App Server

- **状态记录（2026-11-14）：**Electron main/preload/renderer 已接入 profile 管理、官方 `codex app-server --stdio`、versionless JSON-RPC、会话分页/历史游标、可见事件白名单、审批及加密本地账本。发送前查询活动 turn，有活动 turn 时用 `turn/steer` 继续同一 turn；App Server 明确拒绝记为 failed，发送已派发后结果不明则记为 unknown。`thread/read` 前后元数据变化时保留当次 bounded turn snapshot；活动 turn 的可见 item 按内容指纹版本化投递，终态仍要求连续稳定快照，对账稳定后跳过已投递内容。外部问题历史只有诊断说明、无可回答 RPC handle；外部审批/问题均不伪造本地响应句柄。错误/未知通知只投影诊断字段；意外进程退出仅将关联 profile 的活动命令记为 unknown，并使该进程 epoch 的审批和用户输入请求失效。结构化问题以请求 ID/version 绑定 profile、thread、grant version 和原 App Server RPC；只在本地有 pending 记录时一次性响应，重复 ID、过期版本、权限变化或进程退出时 stale。没有 send 时若 live 可用则只上传 pending/stale 并拒绝 RPC，不保留待回答状态。答案保持原 question ID 和文本，secret 答案不进入额外 user-message 事件。审批事件同步白名单化的操作对象、权限范围和 Provider reason；工作目录与权限范围中的路径只保留末尾目录标签，审批命令按敏感操作正文处理。已按 T3 MIT 许可移植必要协议与映射逻辑并加入发行版 notice，不引入 Effect 依赖。新增 JSON-RPC fixture 测试源码但未运行；Codex GUI 外部增量与并发写入待 CX-07。

- **修改边界：**clients/desktop/harness/desktop/src/morde-*.ts 已打包 Electron 运行路径及资源 renderer；涉及 morde-main.ts、preload/IPC、现有子进程管理与凭证桥、新 Provider/API client 模块和构建/测试。当前包没有 Hub client，不新增 Hub 依赖。不得同时在旧 Python CodingService 和新 Electron Provider 重复实现。
- **实现：**从用户显式选择的 Codex 可执行程序/profile 启动本地 codex app-server，仅以参数数组调用，不拼接 Shell 命令；通过 JSON-RPC 做协议版本协商、线程列表/读取/恢复、turn、interrupt 和审批响应；接收通知及 delta，规范化成带稳定 provider event ID 的白名单事件。
- **范围：**Provider 仅访问用户授权的 profile/thread；记录实际能力，不可访问时返回不可接续；认证材料只供本机 App Server 使用。按目标操作系统检测可执行程序、权限和进程退出。
- **本地恢复：**复用已核实的 PC 私有持久存储；保存未 ACK 命令/事件最小账本。Provider 已接受 turn 但 Morde ACK 丢失时恢复线程状态并标 unknown/补事件，绝不盲目重复 turn。
- **测试：**JSON-RPC fixture 测试握手、通知/delta、工具输出、文件摘要、审批请求/版本、结构化问题回答及 stale 绑定、未知事件、断流重启、协议不兼容、参数边界、命令去重与事件 buffer；另做目标平台 Codex 实装检查。
- **依赖：**CX-00 验证显式 profile 下可发现/读取原生线程，并确认唯一发行进程；CX-01 定稿 Provider 事件/授权合同。GUI 运行时协作能力留待 CX-07。

### CX-04（实现完成，验证待 CX-07；Electron API 集成与 CX-03 共用边界）连接云端命令队列并可靠回传事件

- **状态记录（2026-11-14）：**PC 已通过 HTTPS 每 2 秒领取命令，按事件来源 scope 回传白名单事件，并只在逐条事件 ID 获得 ACK 后清理本地缓冲；结果未知时本地账本不重发。send 命令关联原生活动 turn 并持续上报运行/审批/完成状态；外部 GUI 活动 turn 的用户可见快照按内容版本排重并进入同一本地 ACK 缓冲；answer_question 严格匹配本地待处理 RPC 并一次性回复，结果和问题终态通过本地持久事件缓冲回传；对应 profile 的 App Server 退出时活动命令上报 unknown、pending 问题标记 stale。未运行客户端或服务端测试。

- **修改边界：**CX-03 的 Electron main/Provider/API client 与 CX-02 的 Codex command/event API；复用 Morde Client 安装身份。
- **实现：**Electron main 每 2 秒通过已认证 HTTPS claim API 领取命令，通过独立 event batch API 回传事件。云端事务提交事件后才返回 ACK，PC 只有获得 ACK 才清理本地 pending buffer。PC 重连或云端重启后从数据库命令队列和事件游标恢复。
- **限制：**不依赖 Hub 或内存 presence；不把完整 Codex JSON-RPC 流、认证材料或项目文件正文同步到云端。对已领取但结果未知的命令不自动重发。
- **状态：**claim 使用条件更新，同一安装同一命令只有一个有效执行者；领取 60 秒仍无结果时标记 unknown，不回到可重复发送状态。
- **验收：**服务端与 PC API 测试覆盖重连、轮询重复、重复 claim、租约过期、安装撤销、事件 batch 丢 ACK、重复 batch、序号缺口、PC 本地缓存重放及单安装/跨安装隔离。
- **依赖：**CX-02、CX-03。

### CX-05（实现完成，验证待 CX-07）在 Morde iOS 接入 Codex 会话列表与事件时间线

- **状态记录（2026-11-14）：**新增独立 Codex 会话页、安装/会话选择、严格 sequence 去重补缺、SSE、发送/中断/审批/删除；SSE 仅在 read+live 均授权时连接。仅撤销 live 或短暂无法刷新 scopes 时保留 read 历史；明确缺少 read 或完整分页确认 session 消失时清除历史，刷新失败时重试流。列表展示 native thread ID 短码及最近活动时间，可复制完整 ID，状态优先采用本地命令/会话事件。审批卡片读取对象、详情、Provider reason 与服务端当前状态，审批命令仍绑定 ID/version；Codex App Server 未提供可信 expiry 时不推算到期时间。结构化问题按 request ID/version 合并状态，支持选项、多选、自定义文本、is_other 及 SecretField；答案按原 ID 回传，旧 session 卡片不能提交到已切换会话。approval/question 按 request identity 防重复；waiting_pc 不解除回答锁，结果未知时重试沿用原 idempotency key，同时保留运行中 turn 的后续发送能力。时间线合并长 user/assistant 消息的版本分段，显示白名单 unsupported diagnostic。会话内容只驻留运行时内存并在账号 scope 改变/登出时清理，新增 Swift 文件已登记工程，字符串覆盖 9 种语言。新增审批及问题合同 fixture 源码但未运行 iOS 测试或构建。

- **修改边界：**clients/nanoMuse/android/src/ios/Morde.xcodeproj 的显式 Morde target membership、MordeApp.swift 中私有 MordeClient API 调用边界、MordeDesignNavigation.swift、Morde/ 相关纯 Swift policy/tests、MordeUITests 和 clients/nanoMuse/android/src/ios/Localizable.xcstrings。新增 Swift 源文件需更新 project.pbxproj。不要改 Android 工程或让 iOS 直连 PC。
- **实现：**列出已授权 PC installation 和 Codex session，打开后按 sequence 加载/续读事件；delta 归并、重复去重和游标缺口补齐；发送、waiting_pc/forwarded/running/approval/completed/failed/unknown 显示真实状态；中断只作用于指定 session；审批卡片携带 approval ID/version 并显示对象及有效性。
- **数据隔离：**账号切换、安装切换、会话切换或撤权后清理/隐藏不再授权的缓存；页面错误保留其他会话可用性；不把 Codex session 塞进普通云端 Agent Task 页面。
- **测试：**iOS model/client/UI 测试覆盖分页、SSE 重连、乱序和重复 delta、游标过期、排队、unknown、审批过期/撤权、结构化问题的选项/文本/secret 回答及 stale 状态、账号与安装切换；新增用户文本覆盖所有已有本地化语言。
- **验收：**在真实 Morde iOS 工程/设备上能列出已授权本机线程、看到官方可见历史及增量，并能显示服务端真实失败状态。
- **依赖：**CX-02、CX-04。

### CX-06（实现完成，验证待 CX-07）收口授权撤销、数据删除与跨端恢复

- **状态记录（2026-11-14）：**PC grant、云端 command/event scope、审计和删除及 iOS 撤权/缓存清理已实现；权限版本变化后的未领取命令终止、在途命令 unknown、审批版本绑定/进程退出失效和本地结果对账已接通。问题请求绑定一次性回答；撤权、版本不匹配、重复回答和进程退出均拒绝或标记 stale，不自动重放。iOS live-only 撤权保留 read 缓存，read 撤权或 session 确认消失时清理；刷新不确定时不误删并重试 live stream。跨端断线、审批与撤权集成验收未执行，留待 CX-07。

- **修改边界：**PC grant UI/本地 Provider、云端授权/API/审计/保留删除、iOS Codex 页面与缓存；对应 Go、TypeScript、Swift 测试和文档合同。
- **实现：**单独管理 read/live/send/interrupt/approval 授权；撤权立即拒绝新命令/读取并停止事件转发；会话删除按 CX-00/CX-01 确认的 Morde retention 执行；PC 休眠/离线、进程崩溃、SSE 中断、云端提交后 ACK 丢失均如实呈现及恢复。撤权不伪报本机 turn 已停止。
- **审批：**一个版本化审批最多执行一次，过期、变化、撤权、重复响应均拒绝；Codex 确认后才更新审批和 turn 状态。
- **验收：**端到端集成测试验证撤权生效、旧票据/命令/审批失效、同账号多安装隔离、另一用户完全不可见、iOS 关闭不取消 turn、PC 断线补发事件无丢失/重复、结果未知时不重发。
- **依赖：**CX-03、CX-04、CX-05；retention 以 CX-00/CX-01 结论为准。

### CX-07（待后续统一验收）完成真实 PC + iOS 验收并登记发布能力

- **验收环境：**真实打包 Morde PC、真实 Codex 安装与已有登录、真实 Morde iOS；分别记录 macOS/Windows/Linux 中属于本次发行范围的平台结果。模拟 Provider/Hub 只能作为定向测试，不能替代真实 GUI thread。
- **必测场景：**既有 Codex GUI 线程发现、历史读取、桌面外部更新监听、iOS 发消息经云端与 PC 到达同一 thread、assistant delta 与可见工具/计划/文件摘要/审批/结构化问题返回、审批批准/拒绝/过期、结构化问题按选项和文本回答及重启/撤权后 stale、PC 与云端断线恢复、iOS 关闭后任务继续、重复请求单次 turn、撤权、账号/安装隔离。
- **证据：**只保留版本、事件类型、状态和必要脱敏元信息；不得把真实项目文件、认证材料、完整本机日志或截图发到外部服务。报告明确写出真实设备/模拟器、平台、功能门、限制和失败场景。
- **发布：**只有 GUI 既有线程能力和持久事件/命令恢复验收全部通过，才能把“接续 PC 上已有 Codex 对话”标为可用；受限平台显示具体 unavailable 原因。
- **依赖：**CX-06。

## 最终验收记录

CX-00 至 CX-06 的实现和静态复核完成；新增外部 GUI 活跃 turn 快照回传、T3 问题文本/授权来源兼容、iOS 权限撤销历史保留及命令重复边界修复。此前加入的 question 命令/事件、PC 一次性 RPC 回答、iOS 问题卡片和 00201 迁移源码仍在范围内。Go/TypeScript/Swift 测试、构建、数据库迁移验证及真实设备验收按用户安排由后续 CX-07 统一执行，本轮未运行。真实 GUI 外部更新、同线程写入与审批尚未实测；App Server 自建 thread、Mock、模拟器或普通 SSE 测试均不能替代此项。
