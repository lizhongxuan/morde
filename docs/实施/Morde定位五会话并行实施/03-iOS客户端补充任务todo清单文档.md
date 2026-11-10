# 会话 3：iOS 客户端补充实施任务 TODO

> 来源：[03-iOS 客户端实施清单](03-iOS客户端实施任务todo清单文档.md)、[Morde 五会话未闭环功能补充设计方案](../../Morde五会话未闭环功能补充设计方案.md)、[五会话并行执行入口](00-五会话并行执行入口.md)。状态基线：2026-11-03。
>
> 本文仅补充原 I01–I16 清单中的未闭环项，不覆盖原任务和历史验证记录。已接入的代码不重复实现；只有本项实现和验收都完成才勾选。写入范围仍限 `project/nanoMuse/android/src/ios/**` 与本文件，不修改 Android、PC、服务端、OpenAPI 或其他会话清单。

## 1. 当前基线

- I01 已完成；I02–I04 的主会话、消息投递、审批和澄清代码已接，缺双客户端冲突、断线、重开和未知结果验收。
- I05 已有 manifest、前缀筛选、预览、下载、上传、diff/restore 和重命名/移动/删除客户端代码；服务端尚未声明 `workspace_sync`，F02-T10 的 peer 联通未验证，写操作必须关闭。I07 缺 iOS 可用的 Workspace 变更时间线和 Host 独立状态展示。
- I06 的 Goal 分页和 Schedule 运行历史已接；完整个人记忆/例程同步及冲突对账未完成。
- I08/I09 已有项目、成员和项目 Task/工作项/预算/交付 UI；负责人待处理邀请列表缺 API，`project_budget_execution` 关闭时不能执行或报告完整结算。
- I14/I15 已有部分 Host 恢复与租约 UI；默认路径/迁移状态仍缺合同，双账号租约竞态未验收。I16 已有按 ID 操作代码，但移交收件箱缺 API 且 Workspace 能力未开放。
- I11 已接通知分级和设备链接接力；项目连接器 UI 未接。内置技能为只读指引，不是项目授权。I12 的批注、搜索和媒体入口未接。

## 2. 跨会话接口依赖

下表只描述客户端继续实现所需的合同，不授权本会话改服务端文件。缺接口由对应所有者在自己的清单和 OpenAPI 中交付后，iOS 再消费。

| 合同 | 当前状态 | iOS 处理与交付归属 |
| --- | --- | --- |
| `/api/capabilities.supported_features` 的 Workspace 能力 | 当前不声明 `workspace_sync`；设计建议拆为 `workspace_read`、`workspace_write`、`workspace_restore`、`workspace_copy`、`workspace_transfer`。 | 会话 5 冻结并交付最小能力合同；iOS 按操作分别检查 capability 和对象授权。旧 `workspace_sync` 只用于服务端确认完整旧合同后兼容旧客户端。 |
| `GET /api/workspaces/{id}/changes` | OpenAPI 明确将其定义为配对 PC/Host 副本的 durable cursor，不能当作手机历史时间线；409 只适合副本重新比较 manifest。 | 会话 5 提供只读、分页的手机 revision feed。iOS 不消费或持久化 PC/Host 副本 cursor 作为手机历史。 |
| 项目负责人待处理邀请 | `/api/me/project-invitations` 是发给当前用户的邀请箱，不是 owner 创建的待处理邀请列表。 | 会话 4/5 提供 owner-only、分页、逐请求复核权限的项目邀请列表（建议 `GET /api/projects/{project_id}/invitations?state=pending`）；iOS 再接负责人查看、撤销及刷新。 |
| 接收方 Workspace 移交收件箱 | 目前只能按已知 Workspace/transfer ID 查询、接受或取消，没有收件人过滤列表。 | 会话 5 提供仅当前账号可读的分页合同（建议 `GET /api/me/workspace-transfers?direction=incoming&state=pending`）；不得通过枚举 ID 查找移交。 |
| 撤销连接器状态/版本 | 项目连接器列表只返回 active grant；`GET /api/projects/{id}/mcp-connectors/{connector_id}` 尚无 GET 操作。 | 会话 4/5 提供仅 connector owner 或 project owner 可读的非秘密 `active/grant_version` 查询；不得返回 credential 或 endpoint。 |
| Host 默认根目录与迁移状态 | W18/W22 尚无 iOS 可消费的完整状态合同。 | 会话 5 提供服务端确认的 Workspace 根目录、路径就绪和迁移状态；iOS 不硬编码 Host 路径或从本机目录推断云端状态。 |
| 项目预算执行 | 只有完整装配费率、双侧预算、幂等结算和交付证明验证后才可声明 `project_budget_execution`。 | 会话 4/5 交付 capability 与 Portal 实际装配证据；iOS 继续 fail-closed，unknown 用量不显示为已结算金额。 |

## 3. P0 补充任务

- [ ] **IOS-S01（待验证，承接 I02–I04）完成会话与 Task 生命周期双客户端验收**
  - 修改：`Morde/MordeApp.swift`、`NanoMuse/NanoMuseShell.swift` 及现有 iOS 会话测试；沿用主会话、消息、Task 快照/SSE、versioned approval 和 inputs 接口，不另造状态源。
  - 验收：使用两个本地逻辑客户端覆盖同账号主会话一致、账号/服务实例隔离、消息重复提交、断线/重开、版本冲突、重复审批及停止结果 unknown；未确认状态不显示成功，不因读取历史创建 Task。
  - 依赖：R03–R05、W02b 的接口合同已装配；真实跨设备仍另记待验证。

- [ ] **IOS-S02（进行中，承接 I06）补齐个人数据同步与冲突恢复**
  - 修改：`NanoMuse` 个人数据/记忆/例程页面及现有 personal sync 调用与测试；复用 personal snapshot、change feed、对象 revision 和既有 outbox。
  - 验收：覆盖 profile、memory/soul/diary、goal、schedule、提醒的账号隔离、分页/cursor、撤权、应用重开和 revision 冲突；保留未同步草稿及服务端版本，不静默覆盖或跨账号复用缓存。
  - 依赖：P02/P04/P05/R07 对应对象和权限合同。合同未提供的对象显示未就绪，不以本地 Agent 替代。

- [ ] **IOS-S03（阻塞，承接 I05）按最小 capability 完成手机 Workspace 文件操作闭环**
  - 修改：`Morde/MordeApp.swift`、`Morde/MordeWorkspaceTransfer.swift` 与现有 transfer tests；在服务端合同交付后，将浏览/预览/下载、上传、重命名/移动/删除分别门控到 `workspace_read` 或 `workspace_write`，同时检查账号对 Workspace 的实际授权。现有 `workspace_sync` 关闭时不得降级放行。
  - 验收：本地替身覆盖 capability 缺失、无权、敏感路径拒绝、非法相对路径、并发 revision、重复提交及未知回执对账；写操作必须由用户明确触发并带 revision/幂等键。删除云端文件与删除本机副本保持不同操作。真实文件不传输。
  - 依赖：W05–W07/F02-T10；当前服务端未声明 Workspace capability，因此生产入口保持关闭。

- [ ] **IOS-S04（阻塞，承接 I07）接 Workspace revision 时间线和 Host 独立状态**
  - 修改：Workspace 版本页面和会话过程展示；复用已确认 revision、Task SSE 与 Host 状态接口，不新增第二套客户端事实源。
  - 验收：changes cursor 过期后重新拉 manifest/diff；回滚始终创建新 revision；Task 事件断线后快照恢复；Host 状态单独显示更新时间，页面可见时每 5 秒刷新，超过 15 秒标旧，进入后台停止轮询。未确认变化不得伪装为完整回放。
  - 依赖：R04–R05/W05；会话 5 先确认现有 `/api/workspaces/{id}/changes` 是否面向手机，必要时交付只读 revision feed。Host 状态仅在当前账号获准后读取。

- [ ] **IOS-S05（阻塞，承接 I14）接服务端确认的 Host/Workspace 路径与迁移状态**
  - 修改：电脑、Workspace 选择和恢复入口；显示唯一绑定、默认工作/下载根、迁移待处理和路径未就绪状态。
  - 验收：模拟零个/多个/被占用/未就绪 Workspace、旧 Host 待绑定、迁移中及状态 unknown；只有服务端确认恰有一个 ready Workspace 且操作者有权时才允许恢复。不得以本机文件夹选择结果声称 Host 根目录已切换。
  - 依赖：W17/W18/W22 提供实际状态和路径合同；合同未交付前保留停止/不可运行状态。

- [ ] **IOS-S06（待验证，承接 I15）完成多人观察与单人操作权验收**
  - 修改：现有 owner Host 观察/控制入口及控制策略测试；共享 Host 保持只读，租约调用继续携带当前版本。
  - 验收：两个逻辑账号覆盖同时申请、租约到期、续租冲突、主动交还、撤权、旧连接被服务端阻断及状态 unknown；只有服务端确认的 lease owner 可操作，失败/超时不显示控制权已释放。
  - 依赖：W19 服务端 lease/fence 及旧连接阻断测试。

- [ ] **IOS-S07（进行中，承接 I08）补齐项目邀请箱和资源权限展示**
  - 修改：项目成员/邀请/资源入口；现有 `/api/me/project-invitations` 只用于当前账号收到的邀请，新增 owner 待处理邀请 UI 后再接项目列表合同。
  - 验收：覆盖邀请创建、接受、撤销、过期、角色变化、重复请求和权限撤销；负责人只看到自己有权管理的待处理邀请。加入项目或看到 Host/Workspace 关联不能自动获得该资源权限。
  - 依赖：P06/W08 与上表 owner invitation list 合同；未交付前保留“仅管理本次会话创建的邀请”行为，不推测其他邀请。

- [ ] **IOS-S08（阻塞，承接 I09）完成项目 Task、工作项、交付和账本闭环**
  - 修改：现有项目会话、work-item、Task 关联、预算/ledger 和 delivery UI；确保每个 Task 展示实际发起人、项目和目标，交付只提交服务端确认过的 revision/proof。
  - 验收：owner/member/viewer 两逻辑账号覆盖越权、Task 重开恢复、工作项阻塞/恢复、重复提交、预算不足、unknown 费率/结算和交付未确认；无 capability、未知费用或未确认产物时不允许执行或报成功。
  - 依赖：P08–P10/R09–R10/W11–W12 及 Portal 装配的 `project_budget_execution`。费率、付款主体或留存未定时只显示用量/unknown，不扣费。

- [ ] **IOS-S09（阻塞，承接 I16）接 Workspace 完整复制与接受式移交**
  - 修改：Workspace 资源页、复制/移交确认流程及现有 transfer tests；新增入口只消费已发布的独立 capability 和收件箱 API。
  - 验收：源 Workspace 必须停写且 checkpoint 已确认；接收人只查看发给自己的移交。覆盖申请冻结、拒绝/取消、接受与取消竞态、失败对账、成功后旧 owner 撤权及隐藏文件/凭据提示；状态 unknown 时保持冻结，不声称复制/移交成功。
  - 依赖：W20/W21/W22、W05 capability 合同及上表 transfer inbox API；开放前不从 Workspace ID 推导接收权限。

## 4. P1/P2 与退役任务

- [ ] **IOS-S10（进行中，承接 I11）完成 P1 手机能力**
  - 修改：项目连接器、设备接力、通知和模板入口；保留已完成的 `redacted/actionable` 通知规则与用户点按链接接力。
  - 验收：连接器 active/revoked/unknown 状态按版本读写，授权撤销后重开应用仍能安全读回 `grant_version` 再重授权；credential/endpoint 不显示或复制到项目。技能只展示只读说明，不做项目权限开关；设备不在线、通知不可行动或模板未就绪时明确显示未完成。
  - 依赖：P12/R12/W15；连接器状态查询 GET 合同由会话 4/5 交付。

- [ ] **IOS-S11（未开始，承接 I12）接 P2 批注、搜索与媒体入口**
  - 修改：Muse 保留的 Feed/Ideas/Library/简报入口及其 iOS 页面；只接已发布的服务端能力，不启动本机 Agent。
  - 验收：批注绑定 Workspace revision 和准确行/范围；搜索先按项目/Workspace 授权过滤；语音/图像/视频处理走获准模型连接，生成物写入授权 Workspace 并等待 artifact proof。替身覆盖无权、未就绪和失败，不发送文件或调用外部模型。
  - 依赖：P13/R13/W16 对应 API、授权和产物合同。

- [ ] **IOS-S12（阻塞，承接 I10）安全退役旧本机 Agent 与重复入口**
  - 修改：仅在正式 target 逐项移除无调用的本机执行/旧 Cloud 入口；保留 Morde 登录、个人数据、MCP OAuth 和用户资料。
  - 验收：target/call-site 检查确认旧本机执行不可达；迁移前后可读回的个人历史、记忆、例程和凭据范围一致且有回退证据。不可识别或没有新副本的本机数据不删除。
  - 依赖：云端替代能力、数据迁移/回退、R11/W13/W22 退役证据。前置未满足时继续保留旧代码和 target。

- [ ] **IOS-S13（进行中，承接 I13）完成分层 iOS 回归与结果记录**
  - 修改：本会话 iOS tests/targets 和本清单验证记录；不改共享构建或外部服务配置。
  - 验收：本机运行相关策略/transfer/导航测试；构建 Morde Simulator 与 Minis generic iOS/build-for-testing；对可用模拟器用内存响应验收会话、文件只读门禁、项目权限、Host lease 和移交状态。记录每项是“实现通过、本机验证、上游阻塞或环境待验证”；所有可在本机验证的 P0 回归通过后关闭客户端回归项，真实服务/双端限制另列待验证，不据此宣称跨端通过。
  - 依赖：按 S01–S09 的已交付合同分批执行；device-only `libish_emu.a` 阻塞 Minis Simulator 时不更换外部依赖，记录该 target 限制。

## 5. 建议执行顺序与协作边界

1. 并行核对 S01/S02，并向会话 5 提交 S03/S04/S05/S09 的 capability、Workspace feed、路径/迁移和 transfer inbox 合同请求。
2. 收到个人和 Workspace 合同后完成 S02–S06；项目邀请和项目执行合同分别到位后完成 S07/S08。未受阻的 UI 空态、权限策略和本地替身可先做，不开放真实操作。
3. P0 稳定后接 S10/S11；S12 等迁移与回退证据齐备后再执行；最后按 S13 分层回归。
4. `Morde/MordeApp.swift` 被多个 iOS 任务共用，任何并行 agent 必须指定唯一写入负责人；该文件相关任务默认顺序实现。个人数据、Workspace transfer 和各自测试只有在文件边界完全独立时才并行。

全程使用本机内存替身、逻辑账号、模拟器和临时数据；不连接测试服务器、不上传或发送文件。capability 缺失、权限未知、revision 冲突及超时均保持 fail-closed，并在原 03 清单记录实际状态。本文与原 03 清单均位于被 `.gitignore` 忽略的 `docs/`；文件保存在本地工作区。

## 6. 2026-11-03 本轮进展

- IOS-S01：Task 操作成功但状态刷新失败时保留原幂等键；未确认操作不能被不同操作替换，明确的客户端拒绝会清除尝试。本机策略测试通过；双客户端 HTTP 流程仍待验证。
- IOS-S02：已接入按账号 scope 分页读取日记对象、严格校验日期/body/revision、比较本机与云端副本，并仅在用户选择草稿后写本机及排入现有 revision outbox；账号切换会隐藏旧 scope 的云端正文。云端例程导入后保留为关闭的只读本地归档，不会生成重复提醒或本地执行。完整个人数据/例程双向同步、冲突和撤权场景矩阵仍未验收，保持进行中。
- IOS-S04：合同审计确认 `/api/workspaces/{id}/changes` 是配对 PC/Host 副本游标，不适合作为手机历史时间线；独立手机 revision feed 仍是上游阻塞。
- IOS-S13：8 个 iOS 本地 runner 全通过；Morde Simulator Debug build 通过；Minis generic iOS `build-for-testing` 通过并编译 app 与测试 target。Morde UI acceptance 在未配置凭据时按预期跳过（0 passed、1 skipped），未启动 app 或访问服务；Minis Simulator link 被仓库已有的 device-only `libish_emu.a` 阻断，因此 XCTest 未执行。完整设备/双账号回归仍待验证。
- 本轮触及的 iOS 源文件、测试文件、英文/中文 iOS 文档与 changelog 已完成 `git diff --check`；根 `docs/` 任务文档被 `.gitignore` 忽略，仍只保存在本地工作区。

## 7. 2026-11-04 Morde app identity and legacy retirement

- [x] **IOS-S14 设定独立 Morde iOS app 名称与默认服务端**：`Morde.xcodeproj` / `Morde` scheme 使用 `morde` 展示名；空或未设置的 `morde.server` 使用部署 HTTPS origin，已保存的自定义 origin 保持不变。Morde 账号密码登录继续调用 `/api/auth/client-login`。验证：`Morde` iOS Simulator Debug build 成功；安装到 iPhone 18 Pro / iOS 27.0 后，Device Hub 显示 Morde 账号密码登录与默认服务地址。未登录、未发送凭据、未访问服务端。
- [ ] **IOS-S15（方向已纠正；删除部分阻塞，待用户确认）在 nanoMuse 原工程迁移并按确认范围清理旧逻辑**：此前建议退役 `Minis` target 的删除清单已被用户的新要求覆盖；原 `Minis.xcodeproj`、原生界面和用户数据是迁移基础，不能整体删除。独立 `Morde.xcodeproj` 的 IOS-S14 只保留为历史验证记录。依照[原工程迁移任务](../nanoMuse与Morde合并五会话并行实施/03-iOS原工程实施任务todo清单文档.md)的 MI-05/MI-06 重新列出具体候选、影响及替代方案，取得用户确认后再逐项删除；未确认时继续不依赖删除的工作。
