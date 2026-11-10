# HUB-06：dsh 个人能力与旧 Cloud 依赖实施 TODO

> 开发量：按一个 AI 会话、前置就绪后的连续开发估算，最多一天；只计实现与交接记录。测试编写、运行和验收单独进行，不计入这一天。
> 范围估算不是完成承诺；发现超出一天时把剩余实现按文件/接口继续合并拆开，保持原验收条件。
> 当前状态：源码映射已完成一轮；个人数据接入与旧依赖清理未开始。代码完成后标“待验证”；对应验证完成后再勾选并记证据。
> 直接前置：个人数据依赖 DATA-02、DATA-03，目标计划依赖 DATA-06、DATA-07；可先完成独立源码映射。
> 修改边界：服务端 dsh 插件的 profile、rooms、目标、例程和连接器适配；不改 PC client/**。
> 写入规则：[五会话执行入口](../并行执行入口.md)；只修改本会话的指定文件与本会话文档。公共合同/路由/迁移变更在本文件交接给会话 5。
> 本地约束：只在本地修改和验证，不发送或上传文件，不向远端部署。

## 本次开发

- [ ] **HUB-06.01（未开始）接入个人数据与计划**：服务端 dsh 通过唯一业务用例读取/更新资料、记忆、目标和计划，保留对象及版本授权。
- [ ] **HUB-06.02（源码映射完成，待验证）映射连接器和原服务端能力**：复用获准云端连接器，核对 shell、浏览器、文件和持久环境去向；不带出设备私有凭据。
- [ ] **HUB-06.03（未开始）清理旧 Cloud/重复调度依赖**：只替换已迁服务端调用，保留获准模型连接和原数据。

## 原任务与已有进度

下列编号用于追溯完整业务规则和拆分前证据；同一原编号涉及多个会话时，各会话只完成自身范围。原项全部实现及验收通过后才可视为完成。

| 原编号 | 原任务 | 拆分前状态 |
|---|---|---|
| [F04-T06.01](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | Agent 接入账号资料和记忆 | 未开始，依赖 F04-T02.03 |
| [F04-T06.02](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | Agent 接入目标和例程 | 未开始，依赖 F04-T03.01、F04-T03.02 |
| [F04-T06.03](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 映射连接器和设备私有能力 | 未开始，依赖 F04-T02.05 |
| [F04-T06.04](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 核对原 App Server 能力映射 | 未开始，依赖 F04-T06.01 |
| [F04-T10.03](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 清理服务端旧依赖 | 未开始，依赖 F04-T06、F04-T07 |

## 交接

- 在本文件记录完成的接口、文件和待会话 5 处理的公共改动；不要直接改其他会话负责的文件。
- 对应验证在 [独立验收清单](../验收资料/验收入口.md) 执行并记录，不计入本开发文档的一天预算。
- `server/AGENTS.md` 要求接口、实现和测试在同一变更交付；可先完成代码并标“待验证”，合入/提交前再按独立验收清单补齐测试与所需检查。

## 2026-11-02 源码映射与接口缺口

### HUB-06.01 接入前核对

- DSH `src/rooms.ts` 当前把 goals、memory、Feed 和 room 文档保存在本机 `$DSH_HOME/nanomuse/rooms.json`；Room 路由和 `goals_room_update` 最终调用本地 `NanomuseRooms.updateGoal()`。目前没有 owner、对象 revision 或 mutation id，不能直接把这份本地状态当作账号数据。
- Morde 已有 Personal API：`GET/PUT/DELETE /api/personal-data/:type/:id`。写入使用 `base_revision` 与 `Idempotency-Key`；Goal API `GET/POST/PATCH/DELETE /api/goals[/:id]` 复用 `PersonalMutationService`，所有者由认证 principal 确定。适配只能经这些 API/业务用例，不得绕过 app 层直接读写 Store。
- 独立 Morde Client 登录已在 `src/cloud.ts` 实现，凭据以 `MORDE_CLIENT_CREDENTIAL` 单独安全保存；当前只用于 Client Installation、设备列表和 Hub。尚无 Personal/Goal API 调用。不得把该凭据交给 nanoMuse Relay，也不得与 `NANOMUSE_CLOUD_TOKEN` 混用。
- Goal 正文目前不完全相同：DSH 另有 `category`、`summary`、`activity`、`sessionId`、数字 `progress` 与 `attention`；Morde Goal 正文采用 `active/paused/done/cancelled`、带状态的 `steps` 和 `related_material_ids`。`sessionId` 是本地会话引用，不能跨账号同步；其余字段也不能在适配中静默丢弃或复用到语义不同的字段。
- Morde `personal.profile/default` 与 NanoMuse Agent Profile 是同一业务对象：iOS `NanoMuseProfile` 已通过该对象读写 Agent 名称、描述和风格。DSH `profile.ts` 也保存 Agent 名称/外观，但 Relay 还支持本机脸图；Morde profile 目前只接受 dragon/emoji 外观。适配可复用 Morde profile 对象，但本机脸图须继续留在本机，不能用 dragon/emoji 默认值覆盖未迁移的 face 数据。
- Morde Schedule API 是 Host/Model 任务调度，当前 DSH Goal/Feed schedule 是本地 session prompt 调度；常规 desktop Client credential 没有 `schedule:manage`，现有授权路径针对 iOS，Schedule API 也没有 DELETE 路由。现阶段不得将两者直连或新增第二个 scheduler。

### HUB-06.02 能力映射（源码审查，仍待独立验收）

| 能力 | 当前可复用入口 | 明确限制/边界 |
|---|---|---|
| 已获准 MCP 云连接器 | dsh `src/connectors.ts` 与 `src/connectors-tools.ts`；Morde Cloud DSH task broker 也通过已授权的 MCP connector provider/client 路由任务工具。 | 本机 dsh Connector OAuth/API key 保存在 `$DSH_HOME/nanomuse/connectors.json`，仅 loopback 凭据代理可读；Relay 只同步经白名单处理的非秘密元数据。不要迁移或复制本机凭据。 |
| 本机 shell 与 Workspace 文件 | dsh Host 工具、Workspace 和现有策略；Coding CLI 只接收工作区路径并由 CLI 自身权限模式控制。 | 本机 Host 与服务器任务不是同一执行环境；Morde Hub `command` 仅在能力可用、选定根目录并明确授权后开放，不能视为 unrestricted App Server shell。 |
| 远端 shell / 文件 | 旧 nanoMuse Hub Reach；Morde Hub 提供受授权目录下的 `files`/`file.get` 和受限 `command` helper。 | Morde Hub 当前没有 `file.put`；PC/iOS 本机同步和 Workspace checkpoint 也不等于 Agent 文件 API。 |
| 浏览器 / 持久 Web 环境 | 已连接的搜索、抓取或 Browser Rendering MCP 工具；按各连接器实际公开工具调用。 | 当前未找到可供 Agent 操作的浏览器、持久 cookie/profile、页面接管或浏览器会话 API；旧 Python browser demo 仍在旧 runtime。不能把 Portal 登录 Cookie 或网页抓取工具当作持久浏览器。 |

审查范围：`project/nanoMuse/harness/dsh-nanomuse/src/{cloud,connectors,connectors-tools,reach,rooms,rooms-tools,profile}.ts`、`project/nanoMuse/android/src/ios/NanoMuse/NanoMuseProfile.swift`、`server/internal/api/{api,personal_data,personal_sync,goals,schedules}.go`、`server/internal/app/{personal_service,goal_service,schedule_service,cloud_dsh_tool_broker}.go` 及 `server/internal/domain/personal/content.go`。本次为只读源码审查，没有调用网络/测试服务器，也没有读取凭据或运行测试。

### 后续文件所有者交接请求

1. **会话 3（个人数据与调度）**：确认 DSH Goal 的 `category/summary/activity/progress/attention` 哪些是跨端规范字段、哪些只保留在 DSH owner-scoped cache；给出 `tracking ↔ active`、文本 steps ↔ 状态 steps 的映射规则。DSH `sessionId` 留在本机，不进入 Personal API。
2. **会话 3 + 会话 5**：如要把 DSH 计划迁到 Morde Schedule，请定义 desktop Client 的最小 `schedule:manage` 授权合同、Schedule 删除/归档语义，以及 DSH session prompt 如何映射成 Morde Host/Model Task。完成此合同前继续复用现有 dsh scheduler，不增加第二调度器。
3. **会话 5**：若 Personal/Goal/Schedule 接入需要公共路由、权限范围、OpenAPI 或凭据能力变化，由会话 5 认领相应公共文件；会话 4 只在插件侧消费获准接口。

### HUB-06.03 旧依赖保留边界

- `src/cloud.ts` 仍承担 nanoMuse Relay 模型 adapter、账号/会话同步和现有 Hub；这次没有发现可整块删除的独立服务依赖。未迁移前保留其可用路径，不能误删获准模型连接。
- dsh schedule bundle 是 Goals/Feed 当前唯一调度所有者；`cloud.ts` 监听 schedule 状态并用于任务授权范围，没有另建 scheduler。
- `src/connectors.ts` 的本机授权与 `src/cloud.ts` 的非秘密连接器元数据同步是不同用途。待个人/连接器迁移合同确定后再删旧同步调用，不能连同本机连接器授权一并清理。
