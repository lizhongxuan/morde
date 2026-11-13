# Morde 参考 DS-Bot 高优先级功能实施任务 TODO

> 来源：[需求文档](../Morde参考DS-Bot高优先级功能需求文档.md)、[设计方案](../Morde参考DS-Bot高优先级功能设计方案.md)。本清单只规划实施；实现及本项验收都完成后才勾选。任务编号只用于本清单。

## 修改边界

- 基于现有 Portal、PC Electron、iOS、Go 服务、DSH Runtime 和 Host/Workspace 链路补缺；主会话面向用户，子 Agent 只作为任务执行过程。保留多会话和现有授权、模型网关、审计、检查点及恢复机制。
- [第一里程碑 A/B/C 清单](Morde第一里程碑收口三会话并行实施/)已有实现及本地测试记录，本清单复用其完成部分；发现缺口时在对应模块修复，不重新实现一套任务或同步系统。保留工作区已有修改和各清单的状态。
- 按用户要求只在本地修改，不向服务、电脑、设备或远端发送文件，不部署或上传。测试仅使用不传文件的本地替身、合成元数据和隔离环境；不能安装或发送应用包到设备。真实文件数据面保持待验证，`project_upload_enabled` 保持关闭。
- 涉及 PC 页面时同时使用内置浏览器和**实际 PC 客户端**验收；网页替身通过不能代替 PC 客户端验收。iOS 只使用现有本地可运行条件；需要安装或发送文件时记待验证。

## 接口变化

| 接口 / 调用方 | 变化与约束 |
| --- | --- |
| `GET /api/conversations`、Portal `conversations(cursor, limit, q)` | 服务端 `q` 已有，按当前账号授权匹配标题；Portal 调用新增可选 `q`，查询改变重置游标并忽略旧响应。 |
| `GET /api/personal-data/{type}/{id}/history` | **拟新增**，仅 `type=memory`；沿用当前个人对象读取的 Portal Session / Client 授权，接收 `cursor`、`limit`，按版本倒序返回 `items`（`revision`、`changed_at`、`body` 或 null、`restorable`、来源状态、已授权来源）及 `next_cursor`，游标绑定账号和记忆。参数无效 400、凭证无效 401，不存在、已删除或无权访问 404；失权来源不返回内容。未保存正文的旧版本不可恢复。 |
| `PUT /api/personal-data/{type}/{id}` | **已有**接口，`type=memory` 时复用为恢复：提交所选历史正文、当前 `base_revision` 和幂等键；成功生成新版本，版本冲突不覆盖。删除保留墓碑和历史抑制，同时清理该记忆历史正文。 |
| `GET /api/tasks/{id}/artifacts/{artifact_id}/content` | **已有** Portal Session 下载接口；PC 复用 Task 和固定 Workspace 版本、路径、大小、摘要的校验。iOS 使用 Task 产物元数据及已有受限预览；预览条件不满足时只显示元数据，不扩展 Client 凭证访问。 |
| 消息、Task 事件/动作、Host 控制、项目上传和 Desktop IPC | **已有**；沿用当前合同。若实施确认现有字段无法表达必要事实，先同步 OpenAPI/公共约定和所有调用方，再修改实现。 |

新增历史接口须同步 `server/openapi/portal.yaml`、`docs/实施/00-总览与公共约定.md`、`server/internal/api/api.go` 的 Client 凭证路由白名单、服务端及 Portal 生成类型。历史表使用新迁移；实施时核对最新迁移编号，避免与现有工作冲突。

## P0：第一里程碑

- [ ] **T01（未开始）调整 PC 对话界面**〔需求 1〕
  - 修改：`web/portal/src/routes/Assistant.tsx`、`web/portal/src/routes/AssistantSidebar.css`、`web/portal/src/components/PortalLayout.tsx` 和相关样式；必要时调整 `clients/desktop/harness/desktop/` 的窗口或桥。参考 `project/DS-Bot/ds-bot/src/client/` 的布局、消息和卡片，接 Morde 真实数据，保留主会话/旁聊、任务、电脑、文件及草稿。复制源码前核查依赖及许可并保留必要声明。
  - 验收：用内置浏览器和实际 PC 客户端检查常规/窄窗口、键盘、侧栏切换及两个会话的草稿和任务归属；网页与 PC 结果分别记录。

- [ ] **T02（未开始）核对跨端会话与长上下文**〔需求 2〕
  - 修改：按缺口调整 `server/internal/app/conversation_*`、`server/internal/store/postgres/conversation_*`、`runtime/morde-cloud-runtime/src/engine.mjs`、Portal outbox 及 `clients/nanoMuse/android/src/ios/Morde/` / `clients/nanoMuse/android/src/ios/NanoMuse/`。复用已有主会话、持久消息、压缩、授权历史和检查点。
  - 验收：不发送文件地用合成消息和模型替身覆盖超窗口续聊、原文分页、两账号/会话隔离及关闭客户端后重连；纯问答不要求电脑。真实 PC/iOS 与真实模型结果单独记录，未运行不标完成。

- [ ] **T03（未开始，依赖 T02）核对执行中消息路由和去重**〔需求 3〕
  - 修改：按缺口调整 `server/internal/app/conversation_route_*`、`server/internal/store/postgres/conversation_route_*`、Portal 消息 outbox 和 iOS 消息投影。保留消息受理与 Task 创建的区别，按任务目标、附件和执行范围关联；只对明确重跑创建新工作。
  - 验收：本地替身覆盖追问、修改、状态、停止、独立任务、关联不明和重复请求；两个任务中停止一个不影响另一个，同键重试不重复创建或执行。

- [ ] **T04（未开始，依赖 T02）核对子 Agent 并行与结果恢复**〔需求 4〕
  - 修改：按缺口调整 `runtime/morde-cloud-runtime/src/engine.mjs`、`server/internal/app/agent_sessions.go`、`server/internal/worker/cloud_dsh_session_tree.go` 及相关结果汇总代码；参考 DS-Bot 的投递、完整结果和恢复行为，沿用 DSH、授权子集、租约和检查点。
  - 验收：本地 DSH 与模型替身实际派发两个互不冲突的子任务，检查并行、完整结果/失败回传、依赖衔接、重启与取消隔离；结果未知时不重复执行。真实模型协作未验收时单独列出。

- [ ] **T05（未开始，依赖 T01、T03、T04）接通进度、审批和控制展示**〔需求 5〕
  - 修改：`web/portal/src/routes/Assistant.tsx`、消息/Task 投影和 `clients/nanoMuse/android/src/ios/NanoMuse/MIFTaskViews.swift`；按现有快照与事件序号显示当前会话的任务、子任务、审批、澄清和控制结果。
  - 验收：内置浏览器及实际 PC 客户端使用本地替身检查两个任务的归属、审批/拒绝、停止、断线续读和未知状态；iOS 在现有可运行条件下检查同一合同。更新本地化文字。

- [ ] **T06（未开始，依赖 T05）核对指定电脑和常用工具**〔需求 6〕
  - 修改：按缺口调整 `server/internal/app/runtime_tool_executor.go`、`server/internal/app/cloud_dsh_*`、`runtime/morde-runtime/runtimebrowser/`、`runtime/morde-runtime/runtime_desktop/`、`runtime/morde-runtime/runtimeactions/` 和 PC 电脑面板；继续按 Host、授权和控制版本围栏执行。
  - 验收：本地替身检查指定目标、接管/交还、权限撤销、搜索来源、命令输出及不可用/未知状态；内置浏览器和实际 PC 客户端分别查看面板。真实指定 Host 操作和真实搜索未验证时标明。

- [ ] **T07（未开始，依赖 T06）核对 PC 手动项目同步**〔需求 7〕
  - 修改：按缺口调整 `clients/desktop/harness/desktop/src/morde-main.ts`、`clients/desktop/harness/desktop/src/morde-sync.ts`、`clients/desktop/harness/desktop/resources/morde.js`、`server/internal/app/workspace_sync_project_upload.go`、`server/internal/store/postgres/workspace_sync*.go`、`runtime/morde-sync/` 和 `runtime/morde-runtime/workspacereplica/`。沿用现有 source binding、Host 确认基线和 session 回执。
  - 验收：内置浏览器及实际 PC 客户端配合合成元数据/替身检查选择、确认、手动触发、冲突、不可访问、撤权、待应用及已应用；不监听/定时、不反向写 PC，未知回执不报完成。真实文件同步受当前限制保持待验证。

- [ ] **T08（待验证，前置任务未完成；依赖 T01–T07）汇总第一里程碑验收**
  - 修改：[现有 A-07 验收记录](Morde第一里程碑收口三会话并行实施/A-云端Agent与公共合同实施任务todo清单文档.md)或其后续记录；列出当前候选版本、实际 PC/iOS、真实模型和 Host 与本地替身的证据及未完成项。
  - 验收：按根目录 AGENTS.md 逐项核对；真实文件发送被禁止，相关同步数据面不能执行、不能宣布里程碑完成。P1 可继续进行不依赖真实文件发送的本地工作。

## P1：优先补强

- [ ] **T09（未开始，依赖 T01）接入会话标题搜索**〔需求 8〕
  - 修改：`web/portal/src/app/api.ts`、`web/portal/src/routes/Assistant.tsx` 和相关样式/测试；复用已有 `q`，查询改变时重置分页，过期请求不能覆盖新结果，搜索不清除草稿。
  - 验收：内置浏览器和实际 PC 客户端检查标题搜索、多个结果页、切换会话、空结果与失败；不同账号或已撤权内容不可见。

- [ ] **T10（未开始）实现记忆历史与来源合同**〔需求 9〕
  - 修改：`server/migrations/`、`server/internal/domain/personal/`、`server/internal/app/personal_*`、`server/internal/store/postgres/personal_mutations.go` / `server/internal/store/postgres/personal_sync.go` / `server/internal/store/postgres/personal_quota.go`、API、Client 凭证路由白名单和 OpenAPI。复用 `personal_memory_sources` 与变更日志，新增版本正文存储及只读历史接口；首次修改旧记忆时保留修改前可用版本。恢复使用现有 PUT 的 CAS 和幂等键，历史写入/配额与对象修改原子提交；历史正文计入配额，删除时清理正文、释放配额并保留既有墓碑和抑制。
  - 验收：本地隔离数据库及合成文本覆盖来源缺失、历史分页、恢复、并发 409、配额不足、删除/关闭记忆与跨账号隔离；不将历史正文供未授权任务读取。

- [ ] **T11（未开始，依赖 T10）接入记忆历史界面**〔需求 9〕
  - 修改：`web/portal/src/routes/PersonalData.tsx`、`web/portal/src/routes/personal-data-store.ts`、`web/portal/src/app/api.ts` 和生成类型；展示来源、时间、可恢复版本及冲突，删除前提示历史正文一并清理且不能恢复。iOS 沿用个人数据同步读取恢复后的新版本。
  - 验收：内置浏览器和实际 PC 客户端检查修改、历史查看、恢复、无来源、并发冲突及删除；iOS 现有同步逻辑在可运行条件下看到已确认的新版本，不覆盖未提交草稿。

- [ ] **T12（未开始，依赖 T06）接入 Office 执行能力**〔需求 10〕
  - 修改：`runtime/morde-cloud-runtime/` 依赖与技能适配、`server/internal/app/cloud_dsh_skills.go` / 工具代理、授权执行环境和相关产物代码。先验证上游 `@deepseek-ai/dsh-skill-office@0.2.2-rc.1`、`skills` 依赖及许可；按需适配 DS-Bot Office。解释器、文档库和渲染组件须在授权环境可用，所有文件操作走 Morde 工具和 Workspace。
  - 验收：在本地执行环境内直接创建、检查、修改、打开 DOCX/XLSX/PPTX 合成样例并检查内容；验证缺依赖和无权限时真实失败。不得将样例文件发送到服务或设备；云端文件数据面待验证。

- [ ] **T13（未开始，依赖 T12）接入已核验 Office 产物查看和下载**〔需求 10〕
  - 修改：按缺口调整 `web/portal/src/routes/Assistant.tsx`、`clients/nanoMuse/android/src/ios/NanoMuse/MIFTaskViews.swift`、`clients/nanoMuse/android/src/ios/NanoMuse/MIFLibraryViews.swift` 和本地化。PC 复用现有产物下载入口，展示可用的检查或预览结果；iOS 展示同一已确认产物，满足现有授权和本地校验条件时复用预览，否则显示元数据和提示；未核验产物不能显示为已确认。
  - 验收：本地合成元数据和替身覆盖账号/权限撤销、路径与摘要不符；内置浏览器和实际 PC 客户端检查产物状态、查看/下载入口和失败提示，iOS 在现有可运行条件下检查相同元数据。实际文件查看、下载或跨端传输保持待验证。

- [ ] **T14（待验证，前置任务未完成；依赖 T09–T13）汇总 P1 验收**
  - 修改：本地验收记录，区分搜索、记忆及 Office 的本地检查与真实数据面。
  - 验收：P1 各项真实行为均有对应证据后才标完成；当前不能发送 Office 样例或产物到服务/设备，相关交付与跨端文件验收保留待验证。
