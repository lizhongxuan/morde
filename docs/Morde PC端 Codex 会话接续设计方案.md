# Morde PC 端 Codex 会话接续设计方案

> 状态：CX-00 至 CX-06 的合同与实现源码已完成；测试、构建、迁移及真实设备验收待 CX-07。代码状态不代表实机能力已验证。
> 需求依据：[Morde PC 端 Codex 会话接续需求文档](Morde%20PC端%20Codex%20会话接续需求文档.md)；实施任务见[TODO 清单](实施/Morde%20PC端%20Codex%20会话接续实施任务todo清单文档.md)。产品范围以需求文档为准；本方案补充实现边界，真实设备能力仍以 CX-07 验收结果为准。

## 1. 设计目标

让用户在 Morde iOS 查看、接续获授权的 PC Codex 线程。Codex 在 PC 本地运行并继续使用该机现有安装、登录和项目目录；云端 Morde 负责身份与授权、命令转发、用户可见事件持久化及断线恢复。

该链路是一种本地 Codex Provider 接入，不是云端 AI Agent：

- Morde 云端不调用模型，不创建 DSH Task，不执行 Codex 请求，也不把请求切给 DSH Agent。
- PC 上的 Morde 进程通过 Codex 官方 App Server 协议操作本机线程。
- Morde 的用户会话标识和 Codex 原生 thread ID 分开保存、展示和审计。
- 仅同步用户授权且官方接口公开给客户端的对话事件；不传 Codex 登录凭证或项目文件。

## 2. T3code 参考与能力边界

### 2.1 可复用的模式

T3code 把执行环境放在持有 Codex 安装、登录态、文件和持久状态的一端。其 Codex Provider 在该环境启动自己的 Codex App Server，通过官方协议处理 T3 已映射的 native thread、发送 turn、读取增量事件和响应审批；T3 的多账号说明复用 Codex 登录与 `CODEX_HOME`，并没有承诺发现或监听 Codex Desktop 正在打开的 GUI 会话。Morde 可以复用“本地 Provider + 云端授权与持久转发”的结构，但这不等于已有 GUI 会话可被附着。

参考文件：

- [T3code Codex Provider 说明](../project/T3code/docs/user/providers-codex.md)
- [T3code Codex Adapter](../project/T3code/apps/server/src/orchestration-v2/Adapters/CodexAdapterV2.ts)
- [T3code 远端架构](../project/T3code/docs/internals/remote.md)
- [T3 Connect 信任边界](../project/T3code/docs/internals/t3-connect.md)

T3 Connect 的 relay 用于环境关联和连接建立，应用流量仍到达环境。本功能按需求明确经由云端 Morde，因此不能照搬“T3 relay 不转发应用流量”；Morde 云端必须验证每条操作的用户、客户端安装、会话及授权范围，并持久保存待发送命令和接收事件。

### 2.2 不能据此推断的能力

OpenAI 对 App Server 的官方说明将其描述为供客户端使用的线程/turn/事件/审批协议；本地应用启动 App Server 后以 JSON-RPC 驱动其线程。参见 [Codex as a platform](https://developers.openai.com/blog/codex-as-a-platform) 与 [Unlocking the Codex harness](https://openai.com/index/unlocking-the-codex-harness/)。检查 T3 源码后确认，T3 在自己的 Provider 进程里启动 App Server，并在已知 native thread ID 后调用 `thread/read`、`thread/resume`；它没有实现 Codex Desktop GUI 会话发现或另一进程事件订阅。Codex CLI 的 Remote Control daemon/配对也是面向 Codex 官方客户端的管理流程，不是公开给 Morde 的任意 GUI attach 协议，参见 [Codex App Server Daemon](https://github.com/openai/codex/blob/main/codex-rs/app-server-daemon/README.md)。

本机只读探测（2026-11-14，codex-cli 0.167.0-alpha.3.2）中，用显式 `CODEX_HOME` 启动官方 App Server 后，`thread/list` 返回 326 个可列出线程；其中 277 个 thread ID 与 Codex Desktop 本地全局状态中的线程引用匹配。`thread/read(includeTurns: true)` 成功读取一条列表线程的历史结构。此前未显式固定 `CODEX_HOME` 的探测曾返回 0；确切差异尚未定位，发行实现必须显式绑定经用户选择的 profile。`thread/loaded/list` 只返回当前 App Server 进程内存中已加载的线程。Codex 桌面进程仍有独立 stdio App Server，managed daemon control socket 不存在；因此列表/历史可读证据不证明跨进程 GUI 实时增量、同时发送或审批可用。当前 CLI help 仍把 app-server 标为 experimental，与 OpenAI 官方文章把它定位为客户端集成面之间存在版本/产品面差异，实施必须锁定并实测发行版本。

CX-00 已确认官方 App Server 能发现并读取与 Codex Desktop 本地引用相同的 native thread，因此继续按“接续现有对话”开发，不改成另起线程。另一 App Server 是否能观察桌面仍在运行时的 delta、向同一 thread 发送以及处理审批，列入 CX-07 统一验收；这些能力通过实机前不得宣称已完成。

### 2.3 Morde 当前实现事实

- Go Hub 入口为 **/api/hub**，只接受已认证的 Morde Client 安装身份。当前 Go Hub 路由处理 ping、devices、call、result、cancel；协议帧上限为 64 KiB。普通 call 有 30 秒结果等待上限，连接、待处理调用和 presence 保存在进程内存中。它适合安装在线状态和短时设备调用，不提供可恢复的 Codex 命令队列或持久事件日志。
- 现有 Morde 对话消息接口会进入云端 Task 路由。当前 Conversation 又关联云端 Host；基础消息表存在 Task 关联。这条路径不能直接用于本地 Codex turn，否则会错误创建云端 Agent 任务并混淆执行者。
- 当前桌面 Electron 安装包入口为 **clients/desktop/harness/desktop**，main 为 morde-main.ts；助手 Portal 内容在独立 WebContentsView 加载。该发行包没有 Morde /api/hub WebSocket client 或 Codex Provider。**clients/desktop/harness/dsh-nanomuse/src/hub.ts** 不在该 Electron 包的 TypeScript 编译/打包路径中，不能当作现有 PC Hub。**clients/desktop/nanomuse/coding/agents.py** 解析 Codex 私有会话 JSONL，**runner.py** 使用 **codex exec resume**；新功能不得把私有文件扫描或该 CLI 执行方式作为 Codex Provider。PC 新增的 Hub client/Provider 必须进入 Electron 已打包的 main/IPC/preload 路径。
- iOS 工程依据 **clients/nanoMuse/AGENTS.md** 位于 **clients/nanoMuse/android/src/ios/Morde.xcodeproj**，scheme 为 Morde，采用显式 source membership；新增 Swift 文件要登记到该 target。现有任务 SSE 游标允许跳号，不能直接复用作 Codex 严格 gap detection。接入应落在此 Morde 工程与其共享 API / 会话 UI 中，Android 不在范围内。

## 3. 总体架构

~~~mermaid
flowchart LR
    IOS["Morde iOS<br/>线程列表 / 事件时间线 / 输入与审批"]
    API["Morde Go API<br/>Codex 会话 / 命令 / 事件"]
    DB[("PostgreSQL<br/>命令队列 / 事件游标 / 访问授权")]
    PC["Morde PC<br/>安装授权 / 事件缓冲 / 状态对账"]
    APP["本机 codex app-server<br/>JSON-RPC"]
    CODEX["Codex 本机线程<br/>本机登录与项目文件"]

    IOS -->|"提交消息 / 控制 / 读取事件"| API
    API <--> DB
    PC <-->|"Client 安装身份 / 持久命令轮询"| API
    PC -->|"追加事件批次并等持久化确认"| API
    API -->|"SSE 或游标分页"| IOS
    PC <-->|"官方 App Server 接口"| APP
    APP --> CODEX
~~~

### 3.1 职责边界

| 部件 | 职责 | 明确不承担 |
| --- | --- | --- |
| Morde iOS | 选择已授权 PC 与 Codex 线程，显示完整可见历史/事件，提交发送、中断和审批操作；按游标续读 | 直接连接 PC、运行 Agent、读本机 Codex 凭证 |
| Morde 云端 | 安装及会话授权、用户隔离、幂等命令持久化与领取、事件追加与游标读取 | Codex 模型请求、Shell/文件执行、DSH Task 或 Agent 循环 |
| Morde PC | 通过 Morde Client 安装凭证轮询领取持久命令；启动/管理本机 App Server；转换事件；本地短期缓存未确认事件；对账命令结果 | 对外开放监听端口、上传 Codex 登录文件、自动读取未授权账号或项目 |
| Codex App Server | 官方协议线程操作、事件流和审批 | 由云端 Morde 远程启动、由 iOS 直接访问 |

Codex App Server 是 PC 端 Provider 的本地子进程，不是第二套 Morde Agent Runtime。云端 Morde Agent 仍按项目约定使用 DSH；本功能不启动云端 DSH Agent。

### 3.2 已有对话系统为何不直接复用

当前 Morde **POST /api/conversations/:id/messages** 绑定云端 Task 路由，Conversation 又要求云端 Host 语义。Codex 线程属于 PC 上外部 Provider，有自己的 thread ID、turn 状态和 App Server 事件，二者无法用现有 Task 或普通消息字段准确表示。

采用最小的新业务边界：

- 新增 Codex 会话映射、命令和事件持久化模型，沿用现有 Morde 用户、安装凭证、审计、PostgreSQL、Gin/API 分层和 OpenAPI 管理方式。
- Codex Morde 会话 ID 标识云端镜像及其授权关系；Codex 原生 thread ID 只标识 Provider 线程。
- iOS 的 Codex 页面读取专用 Codex API，不调用普通 Task 消息发送接口。
- 现有 Morde 对话、Task、云端 Host 和 DSH 状态机保持原用途，不复制一份 Agent Runtime。

## 4. 数据模型与状态

最终字段和保留规则须在实施前对齐迁移及当前数据策略；以下为最小持久化实体。

### 4.1 云端数据

| 实体 | 关键字段 | 规则 |
| --- | --- | --- |
| Codex 会话映射 | Morde session ID、owner ID、client installation ID、PC instance key、Codex thread ID、标题/工作目录展示名、授权范围、连接/同步状态、最后事件序号、创建及更新时间 | owner 与安装绑定；不保存 CODEX_HOME 绝对路径、认证文件或 access token；唯一约束避免同一安装/实例/thread 重复映射 |
| Codex 命令 | command ID、Morde session ID、client request ID、命令类型、必要载荷、状态、创建/领取/结束时间、结果码 | 请求通过幂等键去重；命令正文只保存用户提交内容或审批决定；领取与执行状态可重入，发送结果不明时转 unknown，不盲目重发 |
| Codex 事件 | Morde session ID、严格递增 sequence、Provider event ID、类型、已规范化的用户可见 payload、Provider 时间、入库时间 | 先持久化再确认；session + sequence 与 Provider event ID 去重；按用户隔离读取，遵守经确认的数据保留和删除策略 |

不把完整 App Server JSON-RPC 原始流存入云端。PC 端先按白名单转换为客户端可见事件，再持久化事件类型和必要字段，过滤认证材料、非公开内部字段与不在授权范围内的信息。用户可见命令输出和路径仍属于敏感会话数据。Codex 镜像事件和命令正文保留至用户显式删除该镜像会话或账号删除；不设置自动过期。删除操作清除镜像会话、事件和未执行命令；已开始的 PC turn 仍按 Codex 实际状态运行，不因删云端记录而伪报停止。只上传文件变更摘要，不上传项目文件正文、附件或凭证。

### 4.2 PC 本地状态

复用当前 Electron / Morde 桌面安装已有的持久存储和安全存储惯例。若没有合适的持久事件缓冲，在应用私有数据目录保存最小的未确认命令/事件账本：

- command ID、原生线程 ID、Provider 请求 ID 和 dispatched / confirmed / unknown 状态；
- 等待云端确认的有序事件批次及最后确认游标；
- 审批请求及结构化用户输入请求 ID、版本摘要、原始 RPC ID 和未处理请求；
- 不保存可通过重新拉取获得的完整历史副本，不把 CODEX_HOME、认证材料或项目文件路径清单同步上云。

重启后先从云端领取未完成命令，再用 App Server 读取线程状态进行对账。若 Provider 已接受请求但确认丢失，标记结果未知并恢复事件；不得通过重发消息来“修复”状态。

### 4.3 状态机

| 对象 | 状态 |
| --- | --- |
| PC 连接 | offline、connecting、online、revoked、unavailable |
| 命令/Turn | waiting_pc、claimed、forwarded、running、waiting_approval、completed、failed、unknown |
| 审批 | pending、approved、rejected、expired、stale |
| 结构化用户输入请求 | pending、answered、stale |
| 事件同步 | 按 session 的单调 sequence；客户端保存 last_seen_sequence，发现游标缺口时先补历史，不跳过缺口 |

状态以持久记录和 Provider 确认作为依据。PC 离线、休眠或结果未知时不得把命令显示成已执行；关闭 iOS 不改变 PC 上已开始的执行。

## 5. 接口与传输合同

以下均为拟议接口。正式实施时由 **server/openapi/portal.yaml** 定义唯一 REST 合同，并同步 **docs/实施/00-总览与公共约定.md**、服务端 Client 权限白名单和客户端生成类型。新增路径必须按项目现行版本的 OpenAPI/鉴权惯例核对，不能将本表视作当前已存在 API。

### 5.1 iOS → Morde

| 接口 | 请求与返回 | 权限及错误 |
| --- | --- | --- |
| **GET /api/codex/sessions?installation_id=&cursor=&limit=** | 返回当前用户对指定安装获授权的 Morde session、线程标题/状态与 next cursor | 需要当前用户登录及 session read；无权安装返回 forbidden，不枚举他人线程 |
| **GET /api/codex/sessions/{id}/events?after_sequence=&limit=** | 返回严格递增、可分页事件及 current sequence；用于首屏、缺口修复和离线恢复 | 校验 owner、安装授权及 read scope；无效旧游标要求重新取快照 |
| **GET /api/codex/sessions/{id}/events/stream?after_sequence=** | SSE 按序推送新入库事件；断线后由 after_sequence 续接 | 同时需要 `read` 和 `live`；read 授权事件内容，live 授权流式传输。SSE 不是持久化来源；连接结束不取消 turn |
| **POST /api/codex/sessions/{id}/commands** | Header：Idempotency-Key；body 为 send、interrupt 或 approval 中一种及相应目标。返回 command ID、接受状态和当前序号 | 所有命令先检查 read，再检查对应 send、interrupt 或 approval scope；重复键返回首次结果；PC 离线时可返回 waiting_pc，不伪报转发成功 |
| **DELETE /api/codex/sessions/{id}** | 删除该镜像会话、云端事件及未执行命令 | 当前 owner 登录；撤销读取和待处理命令；不代表 PC 上正在执行的 turn 已停止 |

### 5.2 PC ↔ Morde

| 接口 | 请求与返回 | 权限及错误 |
| --- | --- | --- |
| **POST /api/codex/sessions/discover** | PC 在用户授权后登记/更新一个官方接口发现的 native thread，携带 profile key、线程 ID、标题/工作目录展示名、能力和授权 scope；返回 Morde session ID | owner 由 Client installation 身份导出；不信任请求体 owner；thread mapping 按 owner/install/profile/native thread 唯一；不接受绝对 profile 路径 |
| **POST /api/codex/commands/claim** | PC 安装每 2 秒轮询领取属于自己的等待命令，返回 command ID、session/thread 绑定、类型和最少载荷；服务端以条件更新保证并发领取安全；领取 60 秒仍无结果时在下一次 PC 轮询中记为 unknown | 安装必须有效且属于同一 owner；撤权、旧授权和错误 thread 绑定拒绝；已领取命令超时后不重新入队 |
| **POST /api/codex/commands/{id}/result** | PC 回报 accepted/running/waiting_approval/completed/failed/unknown 与 Provider request ID | 重复确认幂等；状态只能单向推进或进入显式 unknown |
| **POST /api/codex/sessions/{id}/events** | PC 批量追加已规范化 Provider 事件；请求携带 Provider event ID，不信任客户端自报 owner；返回已提交 sequence | 仅绑定的 PC 安装可写；服务端验证序号、大小、事件类型及重复 ID，事务提交后返回确认 |

PC 使用短周期 HTTPS claim 轮询而不是现有 `/api/hub` 的内存态 generic call。命令以数据库队列为准，云端重启、PC 重连或请求丢失后继续领取未执行命令；已领取 60 秒仍无结果时，在下一次 PC claim 中转为 `unknown`，不会重新入队或自动重发。Codex 事件经受控 HTTPS API 成批回传并在数据库提交后确认。iOS 使用现有 Morde API 认证读取，不能获取 PC 凭证。

### 5.3 事件字段

最小规范化事件字段包括 **session_id**、**sequence**、**provider_event_id**、**kind**、**occurred_at**、**turn_id**、**item_id**、**parent_item_id**、**status** 及对应类型的用户可见内容。审批请求 payload 只允许 `approval_id`、`approval_version`、`kind`、`summary`、`object`、`detail`、`impact`；绝对路径缩减为末尾目录标签，命令及 Provider 原因按敏感对话处理并限制长度。结构化问题事件 `kind=question` 使用 `source=live`，只允许绑定 `question_request_id`、64 位小写 SHA-256 `question_version`、`status` 和白名单 `questions`；问题 ID 保持 App Server 原样，且同一请求内必须唯一。问题标题、正文、选项标题和说明按 App Server 原样保存，单字段最多 4096 UTF-8 字节，允许 LF、CR、TAB。其他 App Server 进程的 thread history 若包含问题文本，只以 `kind=unsupported` 的白名单诊断和 `source=snapshot` 按 `read` scope 展示，不带可回答 RPC handle；不允许用 `live` source 绕开 question request/version 合同。PC 写入事件另带 `source=snapshot|live|approval` 供服务端逐事件核验 scope；此授权标记不作为云端事件内容返回。payload 按类型校验，不接受任意工具参数或任意路径读取指令。

至少覆盖官方接口实际提供的，以及云端命令本身的持久进度：

- user / assistant 消息、delta 与最终文本；
- 可见 reasoning summary、计划和 turn 状态；
- 命令/工具调用摘要与公开输出、文件变更摘要；
- 子 Agent 运行摘要；
- 问题、审批对象与审批状态；
- 运行失败、取消和结束结果。
- Morde 命令受理、PC 领取和 Provider 结果状态（`command_status`）；仅包含 command ID/type/status/result code，不复制命令正文。

事件解析器采用 Provider 版本和事件类型白名单。未知事件以可诊断的 unsupported 状态记录，不把原始内部数据透传给 iOS。相同 turn/item 的 App Server 通知与后续历史快照使用相同 provider event ID，云端据此去重。事件和输出设置单条及批次上限；过长输出须按稳定分片 ID/序号分片并合并显示，不能静默截断后标为完整。

每条命令的持久状态迁移递增 `status_version`；事件 ID 使用 `morde:command:<command-id>:<status-version>:<status>`，保留同一命令重复进入相同状态的完整历史。授权版本变化时，尚未领取的命令终止为 failed，已领取或执行中的命令变为 unknown，不伪称撤权已停止本机 turn。

### 5.4 初版 JSON 合同

以下字段名和枚举供 CX-02、CX-03、CX-04、CX-05 共用，最终以 OpenAPI 为准：

列表返回 `{"items":[session...],"next_cursor":null}`；session 包含 `id`、`installation_id`、`profile_key`、`thread_id`、`title`、`cwd_label`、`updated_at`、`status`、`scopes`、`grant_version`、`last_sequence`。`discover` 返回 `{"session":session}`；仅在 PC 用户明确选择 scope 后调用，重复调用只更新元数据与该次明确选择的 scope，不自动扩权。iOS 列表与历史需 `read`；SSE 同时需 `read+live`，由 `read` 授权事件内容、`live` 授权流式传输；命令先需 `read` 再需该类型的操作 scope。`answer_question` 属于用户发送内容，使用 `send` scope。

~~~json
POST /api/codex/sessions/discover
{
  "profile_key": "opaque-local-id",
  "thread_id": "native-codex-thread-id",
  "title": "visible thread title",
  "cwd_label": "project display name",
  "updated_at": "2026-11-14T00:00:00Z",
  "status": "idle",
  "scopes": ["read", "live", "send", "interrupt", "approval"]
}

POST /api/codex/sessions/{id}/events
{
  "events": [{
    "provider_event_id": "stable-provider-event-id",
    "source": "live",
    "kind": "assistant_delta",
    "occurred_at": "2026-11-14T00:00:00Z",
    "turn_id": "native-turn-id",
    "item_id": "native-item-id",
    "payload": {"text": "visible text delta"}
  }]
}

POST /api/codex/sessions/{id}/commands
{"type": "send", "text": "user message"}
{"type": "interrupt", "turn_id": "native-turn-id"}
{"type": "approval", "approval_id": "id", "approval_version": "version", "decision": "approve"}
{"type": "answer_question", "question_request_id": "100", "question_version": "sha256", "answers": {"choice-id": ["A"]}}
~~~

结构化问题 pending 事件携带 `{question_request_id, question_version, status:"pending", questions}`；回答或 PC 进程失效分别追加 `status:"answered"` 或 `status:"stale"`。`questions` 保留 App Server 的 question ID、标题、问题、选项和 secret/other 标记；ID 在一份请求内唯一并按原值保存。`answer_question` command 要求 `read+send` scope，必须带 request ID/version，并为每个原始 question ID 提供答案数组。PC 只有在本地仍存在匹配 profile、thread、grant version 和请求版本的待处理 RPC 时才响应；RPC 写入成功后，在同一次本地持久化中移除 request、结算命令并保存已回答事件。缺少 send 时，若 live 可用则上报 pending/stale 并拒绝 RPC；权限变化、重启、进程退出或错版回答会拒绝并使对应请求 stale。App Server 答案采用 `{answers:{[questionId]:{answers:[...]}}}`。重复、过期或撤权后的答案不得再次提交；标记为 secret 的答案不回显到额外的 user-message 事件。

`GET /sessions/{id}/events` 返回 `{"items":[event...],"current_sequence":42,"next_cursor":null}`；每个事件由服务端附加 `session_id` 和 `sequence`。SSE 的 `id` 是 sequence，`data` 是同一事件对象。命令创建返回 `{"id":"command-id","session_id":"session-id","type":"send","status":"waiting_pc","created_at":"..."}`。PC `claim` 请求体为空对象，返回零或一个 `items`；领取后的命令包含 `id`、session/thread/profile 标识、grant_version、type 与类型载荷。PC `result` 请求只允许 `forwarded|running|waiting_approval|completed|failed|unknown`，结果更新幂等。PC event batch 的确认返回 `{"items":[{"provider_event_id":"...","sequence":43}],"last_sequence":43}`；已确认的重复 ID 返回原 sequence，不产生新事件。Session 删除成功返回 204。

`profile_key` 是 PC 生成的稳定不透明标识，绝不携带 `CODEX_HOME` 路径。事件上限为单条 64 KiB、单批 512 KiB；JSON payload 只允许白名单事件结构。`scopes` 仅接受 `read`、`live`、`send`、`interrupt`、`approval`。命令需 `Idempotency-Key`；claim 返回的命令先在 PC 本地账本写为 `claimed`，Codex 已接受而云端未确认时改为 `unknown`，不得因租约到期重发。用户删除镜像会话使用 DELETE；重新授权需 PC 再次明确提交 discover，不自动复活已删云端记录。

## 6. 关键流程

### 6.1 首次绑定与授权

1. 用户在 PC Morde 选择 Codex 可执行程序与本地 profile（可用 CODEX_HOME 或受支持的安装选择器），明确授权账户/安装及允许接续的线程范围。
2. PC 检查本地版本和 App Server 能力，启动 JSON-RPC 子进程并读取可用线程。
3. PC 使用现有 Morde Client 安装身份登记 session/thread 元数据；云端不收绝对 profile 路径或认证文件。
4. 用户按读取、实时接收、发送、中断、审批分别授予权限。PC 上可查看安装、实例、session、授权范围和连接状态，并可撤销。

如果官方 API 无法发现既有 GUI 线程，PC 必须显式呈现不可接续能力；不能自动建立一个新线程充当原线程。功能状态和对外文案由第 9 节的 go/no-go 结果决定。

### 6.2 iOS 发送、PC 执行、事件回传

1. iOS 为某个 Morde session 生成稳定 client request ID，调用命令 API。云端在同一事务中验证授权、写命令与幂等键并追加 `command_status=waiting_pc` 事件；返回“云端已受理”，不显示成 Codex 已接收。PC 领取和结果状态变化也写入相同持久事件流，使 iOS 重连后能恢复进度。
2. PC main 每 2 秒按安装身份轮询持久命令 API，先在本地账本标记领取，再通过 App Server 发起 turn/interrupt/approval。轮询与 Hub 状态无关。
3. PC 将官方事件转换为用户可见事件，批量 POST；云端事务追加新序号后确认。只有已确认事件才从本地缓冲清除。
4. iOS 从云端事件日志用 SSE 接收增量，并在重连、游标缺口或快照过期时按 after_sequence 分页补齐。
5. 若命令在 Provider 接受后失去确认，PC / 云端使用 thread/read 和 App Server 状态进行对账；无法确认则标记 unknown，由用户决定是否重新发送。

### 6.3 Codex 结构化用户输入

1. PC 收到 `item/tool/requestUserInput` 后，按白名单将问题、选项和 secret/other 标记作为 `question` live 事件持久化；用户输入请求使用 `live` scope 展示，回答使用 `send` scope。
2. iOS 将各问题呈现为选项/文本输入卡片，secret 字段使用安全输入；提交时按 question ID 原样构造 `answers`。answers 和幂等键随 Morde command API 传递，不发送到普通 Conversation/Task。
3. PC 校验 request ID/version、thread/profile/grant 与每题答案后，一次性回复原 RPC。App Server 进程退出、重启或授权失效后请求变为 stale；Morde 不伪造空答案。
4. 其他进程的 Codex GUI 问题若只在 thread history snapshot 中可见，则以无 response handle 的 unsupported 诊断显示；GUI 外部审批同样不伪造成 Morde 可回复的 approval。只有真实到达本机 Morde App Server 的原始请求才能走对应的一次性响应合同。

### 6.4 审批与撤权

- 每个审批使用 PC 生成的稳定 approval ID 和 request version/hash。iOS 回复携带这两个值；PC 在写 App Server response 前比对待处理请求及未过期状态。
- 一个审批最多执行一次；已变化、已过期、已撤权、已响应的旧审批返回 stale，不得转成自由文本或发给另一操作。
- 撤权事务先使云端拒绝新读取/命令，再通知 PC 关闭该 session 的实时转发并使本地授权失效。若 Codex turn 已运行，撤权不代表已中断；只有单独的 interrupt 获 Codex 确认后才显示已停止。
- 安装被注销、owner 不匹配、会话映射变化或授权版本落后均 fail closed。

## 7. 安全、可靠性与隐私

- **认证：** PC 使用注册的 Morde Client installation credential 连接 /api/hub 和 PC API；iOS 用 Morde 原生 Client 身份。所有会话、命令、事件请求按已认证 owner 与安装复核，不接收请求体里的 owner 作为授权依据。
- **授权：** read history、live events、send（含结构化问题回答）、interrupt、approval 独立检查。更改 grant version 后，旧命令/审批/问题请求失效；PC 只处理 owner 和 thread mapping 完全匹配的命令。
- **账号与安装隔离：** 所有持久行包含 owner 归属；API 查询必须按当前 owner 过滤并结合安装授权。事件序号和 Provider ID 只在对应 Morde session 内唯一。
- **传输及限制：** PC 主动以 HTTPS 请求云端，云端不开放对 PC 的公网入站端口。Client 凭证只在 Electron main 使用；新增 HTTP event batch 要有最大正文、请求频率、超时和每 session 缓冲上限。
- **内容边界：** 不上传 Codex auth 文件、token、项目文件、附件或任意本地目录。只回传用户已授权的线程可见内容、工具输出和文件变更摘要；可见的项目路径和命令输出按敏感对话信息处理。
- **数据生命周期：** 镜像事件和命令正文无限期保留至用户删除该 Codex 镜像会话或账号删除，不设自动 TTL；`DELETE /api/codex/sessions/{id}` 撤销访问并删除云端会话映射、事件和未执行命令。撤权阻止后续读取与命令，但不等于删除历史，也不声称已停止本地 turn。PC 本地未确认事件按 ACK 清理，已授权 profile/thread 范围保存在本机加密配置中。
- **断线与重启：** 云端事件是可续读持久来源，PC 轮询领取数据库持久命令；PC 本地账本保护未确认批次。客户端重试不重建已受理命令；无法判定执行结果时明确显示 unknown。
- **审计：** 记录 grant 变更、PC 绑定/解绑、消息受理、命令执行结果、审批决定、撤权和跨端读取的必要元信息。列表读取、非空历史页读取及 SSE 检索到的非空事件批次只记录操作者、读取端 installation ID、目标 installation/session ID、游标或序号范围与条数；不记录正文，也不为心跳或空轮询逐条写审计。不写访问凭证、完整项目文件或任意未授权输出。

## 8. UI 及平台边界

### PC

- 在现有 Morde 桌面客户端展示 Codex 连接入口、可用 profile、Morde 账号、授权范围、接入能力、PC/App Server 在线状态和撤权操作。
- 会话列表标出原生线程 ID 或可复制的短标识、线程标题/工作目录展示名、最近活动、同步状态与是否可接续。
- 只在官方 Provider 给出对应信息时展示运行状态、工具调用、文件变更、子 Agent 或审批；unsupported/unavailable 清楚可见。
- 错误按 Codex 安装/登录缺失、版本不支持、权限未授予、App Server 退出、网络中断、撤权和未知结果分类，不能用模拟活动伪装在线。

### iOS

- Morde 会话区域新增 Codex PC 来源入口/筛选，按 PC 安装与获授权 session 展示列表；打开后显示完整可见事件时间线。
- 发送按钮状态区别本地草稿、云端受理、等待 PC、Codex 执行和完成；审批卡片显示原始请求对象、影响和有效期。
- 消息时间线按事件 sequence 稳定归并 delta；重连时去重并补齐。原 Morde 云端 Agent 会话、Task 页面和普通消息路由保持原样。
- 新用户可见文本同步更新 **clients/nanoMuse/android/src/ios/Localizable.xcstrings** 中现有语言。

### PC 系统平台

Codex CLI/App Server 可用性须逐一验证 Morde 桌面发行支持的平台。只在受支持平台启用入口；平台/版本不支持时返回具体原因。不以 macOS 上通过推断 Windows/Linux 已通过。

## 9. 实施门与验收顺序

1. **门 A：既有 GUI 线程可接续性。** 列表/历史读取已在本机通过官方 `thread/list` 与 `thread/read` 验证，且线程 ID 与 Codex Desktop 本地引用匹配；桌面继续操作时的增量、发送同一线程和审批仍须在 CX-07 统一验收验证。失败时如实报告兼容性，不把另起线程称为原对话。
2. **门 B：数据留存和同意。** 默认授权按用户选择的 Codex profile/thread 与独立 scope 生效；云端镜像数据保留至显式删除或账号删除，不自动过期；明确撤权与删除的差异。
3. **门 C：本地 PC 实现路径。** 发行包实际落点已确认为 Electron main/IPC/preload；实现显式 `CODEX_HOME` 的官方 App Server Provider，不将旧 NanoMuse Hub 或私有会话 parser 当成新 Morde 实现。
4. **服务端合同。** 迁移、app/domain/store/API、权限、OpenAPI、公共实施约定和生成类型一致；云端断开后命令仍持久，事件按游标恢复。
5. **PC 和 iOS 各自验收。** 先用 RPC/API 测试替身覆盖协议解析、重复、断线与撤权，再分别在真实 Morde PC 和真实 iOS 设备验证从 iOS 发消息到本机 Codex、事件回流、审批和恢复。替身不替代实机结果。

## 10. 未决事项与风险

| 事项 | 当前结论 | 对实施的影响 |
| --- | --- | --- |
| 接入现有已打开的 Codex GUI 线程 | 本机 `thread/list` / `thread/read` 可读取与 Codex Desktop 本地状态相同的线程 ID；跨进程实时增量、同线程发送和审批尚未实测 | 先按现有 native thread ID 实施；CX-07 必须验证桌面并行操作与审批，失败按能力显式显示 |
| Codex 线程的云端历史保留、删除与同意 | 已定：显式授权 profile/thread 后镜像；不自动过期，用户删除或账号删除时清除；撤权保留历史但禁止访问 | 迁移/API/UI 必须实现 DELETE 会话并清除事件和未执行命令；已开始 turn 不伪报停止 |
| 不同发行平台的 App Server 支持与安装发现 | 未验证 | 首期 capability 必须按平台检测；不能承诺未测平台 |
| 事件输出可能很大或含敏感路径 | 已知风险 | 做类型白名单、大小上限、分片和可见范围说明；不上传文件正文或凭证 |
| 现有 Hub 单实例内存 presence | 现状限制 | 不把 Hub 当持久消息总线；数据库队列为准，PC 重连后可通过持久命令 API 自恢复 |
| PC 到云端的线程发现同步 | 现有 API 尚无线程映射登记接口 | CX-01 必须把 POST /api/codex/sessions/discover 纳入 OpenAPI 与安装权限合同 |
| 当前 GUI 与独立 App Server 的连接 | 本机显式 profile 的 `thread/list` 能列出与 Desktop 状态引用匹配的 ID，`thread/read` 能读取历史；GUI 实时增量、同线程并发操作和审批尚未实测 | CX-00 的发现/历史门已通过；CX-07 必须验证 GUI 运行时交互，失败时按能力显式显示 |

