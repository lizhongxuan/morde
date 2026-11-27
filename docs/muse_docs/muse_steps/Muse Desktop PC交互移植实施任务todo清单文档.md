# Muse Desktop PC 交互移植实施任务 TODO

来源：[需求文档](<Muse Desktop PC交互移植需求文档.md>)、[设计方案](<Muse Desktop PC交互移植设计方案.md>)。范围以用户确认的 **6 项功能**为准：工具活动组、长输出、结构化追问、附件、语音和完成通知。需求文档验收段中遗留的审批卡片、排队/引导不作为新增功能；现有审批和消息路由仍须回归。

除用户已同意的完整输出受保护保存、必答澄清可跳过和本机音频电平采集外，继续保留原有入口、权限、任务行为和数据处理方式。若实现必须改变其他既有行为，先说明具体影响并取得用户确认。兼容旧客户端时复用同一套 Task 记录和状态机，不另建业务流程。开发及验收使用本机合成数据，不上传或外发本机文件。

状态统一为：未开始、进行中、待验证、完成、阻塞；完成实现及本项验收后才能勾选。当前各项均未开始，上游插件已做的独立兼容性验证不等于接入完成。

## 接口变化

| 接口 | 本次合同及调用方 |
| --- | --- |
| Host 输出分块传递（拟新增，`runtime/morde-runtime` → 服务端 Broker） | 绑定账号、Task、修订、步骤、原调用及输出流；分块携带顺序、完整性信息和结束/截断状态，服务端重复接收可核对。失败或容量不足时停用采集支路并继续排空原输出，不影响工具返回和模型输入。M02、M03 实现。 |
| `GET /api/tasks/{id}/steps/{step_id}`（现有）及 `GET /api/tasks/{id}/steps/{step_id}/output`（拟新增） | 原详情保留；新接口按输出流和游标分页返回脱敏内容、下一游标及保存/截断状态，每页复核 Task、Workspace 授权。未保存或撤权不返回内容。PC 按需读取；M03、M04 实现。 |
| `GET /api/tasks/{id}`、`POST /api/tasks/{id}/inputs`（现有）及 `POST /api/tasks/{id}/clarifications/{clarification_id}/answers`（拟新增） | Task 详情保留旧单题字段，并可附带结构化问题；新答复携带问题版本、问题 ID、选项 ID、自由文本或明确跳过及幂等键。旧接口仍接受原文本；新批次向旧客户端逐题投影，逐题提交后更新待答身份/版本，全部处理后才恢复原调用。错误、旧版本、重复请求和撤权须有确定结果。M05、M07 实现。 |
| DSH `ask_user_question` ↔ Morde provider/Broker（拟新增适配） | 输入为受限 schema 的问题数组，输出为按问题 ID 对应的选项标签、`custom` 或显式跳过；绑定真实 Agent、Task、授权版本及原 `call_id`。待答持久化后按原调用恢复；不可用时保持原 `ask_user`。M06 实现。 |
| `GET /api/notifications`（现有）及 `GET /api/notifications/incremental`（拟新增） | 原列表、已读行为不变；新接口按当前账号的递增游标分页读取授权内通知，并可建立首次基线。PC 按服务实例和账号持久化游标与去重身份。M10、M11 实现。 |
| `morde:desktop-notifications:get` / `morde:desktop-notifications:set`（拟新增，PC 主进程 IPC） | 仅可信设置界面可读取开关或提交布尔值，返回当前设置；通知监听、游标及去重由主进程管理，不向界面暴露凭据。M11 实现。 |

接口路径为实施目标，新增项实现时同步 `server/openapi/portal.yaml`、`docs/实施/00-总览与公共约定.md`、调用方和测试。PC 本机通知开关与游标通过受信任的桌面主进程接口读取和保存，不暴露凭据。

## 实施任务

### 工具活动与输出

- [ ] **M01（未开始）折叠工具活动组**
  - 修改：`web/portal/src/routes/Assistant.tsx` 的 `taskAgentActivityGroups` 和步骤展示；按 Task、Agent、修订及步骤顺序合并连续工具，保留现有步骤详情。运行、等待、失败可辨；只显示可靠的文件增删行数，耗时或行数缺失时显示未记录。
  - 验收：覆盖跨任务/Agent、不连续步骤、事件重复和重连；在实际 `morde-pc` 展开、收起并核对步骤详情和状态。

- [ ] **M02（未开始）采集源头输出**
  - 修改：`runtime/morde-runtime/runtimeactions/shell.go` 和 Host→服务端传输；在原 64 KiB 截断前为支持的输出增加独立采集支路，标记源头截断、结束及采集失败。保持原执行结果、流排空、取消和超时行为。
  - 验收：长输出、失败、取消、传输中断与容量不足时，原命令不阻塞、不重跑，原输出和执行状态与基线一致；未提供完整输出的工具上报未记录或源头截断。

- [ ] **M03（未开始，依赖 M02）保存与授权读取输出**
  - 修改：`server/internal/app/task_step_detail.go`、`task_step_execution.go`、Broker、API、PostgreSQL 及迁移；按调用与输出流绑定记录，沿用敏感命令隐藏和脱敏规则，安全处理跨块敏感内容后加密保存，设置容量和生命周期清理；实现分页读取接口。
  - 验收：本机 PostgreSQL 和 API 测试覆盖跨块脱敏、完整性、重复分块、保存失败、容量截断、跨账号/Workspace 与撤权；只有源头和保存均完整才标记完整，历史缺失内容不可恢复。

- [ ] **M04（未开始，依赖 M03）长输出查看器**
  - 修改：`Assistant.tsx`、`web/portal/src/app/api.ts`；步骤详情先显示现有摘要，点击后分页加载，区分保存中、可读取、源头截断、容量截断、未记录和保存失败。
  - 验收：在实际 `morde-pc` 验证按需加载、多页、失败与撤权状态；原步骤详情和摘要仍可使用。

### 结构化追问

- [ ] **M05（未开始）澄清记录与答复合同**
  - 修改：`server/internal/domain/task`、`server/internal/app`、`server/internal/store/postgres/task_pending.go`、`server/internal/api/tasks.go` 和迁移；记录问题、唯一选项 ID/标签、规则及显式跳过，按账号/Task/问题版本与幂等键原子提交。兼容现有 `/inputs`；仅新批次逐题处理后恢复。文件交付和路由问题跳过后保留原授权与交付门禁。
  - 验收：在本机 PostgreSQL/API 覆盖单选、多选、自由文本、必答跳过、旧客户端逐题答复、并发/重复提交、过期版本、撤权及文件交付；跳过不等于授权，无法安全继续时说明未完成内容。

- [ ] **M06（未开始，依赖 M05）DSH 插件接入与恢复**
  - 修改：`runtime/morde-cloud-runtime/package.json`、锁文件、`src/engine.mjs` 和 `server/internal/app/cloud_dsh_tool_broker.go`；优先接入已核验许可证、固定为 `0.2.2-rc.1` 的 `dsh-user-questions`/`dsh-tool-ask-user`，通过 provider 和受限 schema 适配，绑定执行上下文中的原 `call_id`，将新工具纳入子 Agent 禁用及同轮单独调用规则。数据库仍是待答依据，按原工具调用恢复。
  - 验收：真实 Morde 服务链路覆盖两账号/并发 Task、定向取消、等待期间进程重启与断线、原调用恢复、重复答复和旧 checkpoint；不能将插件 `timed` 的新消息当成原调用结果。若无法保持原有隔离、授权和恢复语义，记录原因并按设计回退至 `ask_user` 最小扩展。

- [ ] **M07（未开始，依赖 M05；联合验收依赖 M06）追问卡片**
  - 修改：`Assistant.tsx` 和 `src/app/api.ts`；根据服务端规则显示单选、多选、自由文本及跳过，保留提交中、失败、结果未知和重试状态，草稿绑定 Task、问题和版本。
  - 验收：在实际 `morde-pc` 提交各种答案和必答跳过，检查失败后状态、重复提交、切换会话及原文本澄清；不出现虚假答案或审批。

### 输入与桌面体验

- [ ] **M08（未开始）附件拖放和预览**
  - 修改：`Assistant.tsx` 与 `assistant-drafts.ts`；文件拖入显示反馈，复用 `addComposerFiles` 和现有文件类型、数量、大小及上传校验，提供预览和移除，异步结果绑定原会话草稿。
  - 验收：在实际 `morde-pc` 拖入允许/不允许文件，验证提示、预览、移除和切换会话；上传流程只用本机内存合成数据和替身验证，不实际发送文件。失败不清空草稿、不自动发送。

- [ ] **M09（未开始）语音波形与取消**
  - 修改：`web/portal/src/components/AssistantVoiceInput.tsx`、`clients/desktop/harness/desktop/src/morde-main.ts`；沿用 `SpeechRecognition`，仅在用户启动后对可信 Assistant 主框架开放麦克风并在本机计算电平，保留既有剪贴板等权限规则；展示聆听、转写、取消和不可用状态。按会话/录音代次隔离回调，结束、取消、切换或出错时释放音频资源。
  - 验收：在实际 `morde-pc` 验证真实电平、转写与取消，已写入草稿保留；拒绝麦克风、波形失败、过期回调不影响原识别或文字输入，其他页面和摄像头不获权限；采样音频不保存或上传。

- [ ] **M10（未开始）完成通知增量读取**
  - 修改：`server/internal/app/notification_service.go`、`server/internal/store/postgres/notification.go`、`notification_source_store.go`、通知 API 与迁移；复用已持久化的 `completed` 通知及权限/计划通知规则，覆盖通知表的全部入库路径，在事务中按账号串行分配递增游标，实现基线和增量分页。
  - 验收：本机 PostgreSQL/API 覆盖并发入库、分页、重连、跨账号与权限撤销；不漏读或重复分配，原通知列表与已读行为不变。

- [ ] **M11（未开始，依赖 M10）PC 完成通知及设置**
  - 修改：`clients/desktop/harness/desktop/src/morde-main.ts` 及 PC 设置入口/本机配置；开关默认开启，按服务实例、账号、通知 ID 保存去重信息与游标，登录期间持续监听，首次启用建立基线。仅窗口不在前台且开关开启时弹系统通知，通知摘要沿用现有脱敏规则；账号退出或切换停止旧监听。
  - 验收：在实际 `morde-pc` 验证后台完成仅通知一次、关闭开关、重连及账号切换；系统权限不可用时 Task 结果和站内通知仍可用。记录崩溃恰在投递前后的不重复优先限制。

### 联合验收

- [ ] **M12（未开始，依赖 M01–M11）回归与记录**
  - 修改：按实际代码变化同步[最终流程](<Morde 复刻 Muse 最终流程与开发设计.md>)、[实施任务总大纲](实施任务TODO/00-总大纲.md)、`docs/验证记录/Morde_Harness_最终流程测试用例.md`，记录命令、结果和未覆盖范围；涉及服务端接口、迁移及跨模块合同同步 OpenAPI 与公共约定。
  - 验收：执行受影响模块测试及 `server/AGENTS.md` 要求的检查和隔离 PostgreSQL 集成测试；使用 `make pc` 启动实际 `morde-pc` 验证六项交互与失败边界，回归普通对话、多会话、审批双入口、澄清、消息路由、语音、云端电脑和 PC/iOS 同会话。附件发送仅做内存替身验证，实际发送待用户另行授权；不得用普通用户 Web 端或 iOS 代测 PC。环境或授权不足须标记待验证/阻塞，不宣称通过。

## 执行顺序与边界

M02 → M03 → M04；M05 → M06，并由 M07 接入；M10 → M11。M01、M08、M09 可在相关合同稳定后独立开展，最后执行 M12。M01、M04、M07、M08 共改 `Assistant.tsx`，M09、M11 共改 `morde-main.ts`，修改这些共享文件时按文件串行集成，避免覆盖彼此的改动。

统一 UI 规范文件 `docs/design/ice-cream-blue/morde-icecream-blue-ui-spec.html` 当前缺失；实施时先复用现有样式，权威规范可用后再验收视觉一致性。无法启动实际 PC、真实 DSH 或跨端环境时，保留对应任务的待验证或阻塞状态。
