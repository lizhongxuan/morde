# A：云端 Agent 与公共合同实施任务 TODO

依据：[第一里程碑收口技术设计方案](../../Morde第一里程碑收口技术设计方案.md)、仓库根目录 `AGENTS.md` 和 `server/AGENTS.md`。本清单供第一个会话执行；与此前三会话清单的 A/B/C 划分不同，不修改旧清单及其状态。

## 修改边界与并行约定

| 唯一写入方 | 文件范围 |
|---|---|
| A（本会话） | `runtime/morde-cloud-runtime/**`；除下列 B 范围外的 `server/**`；公共 `server/openapi/portal.yaml`、`server/migrations/**`、`server/internal/app/store.go`、`server/internal/api/api.go`、`server/cmd/portal/**`、`server/internal/store/postgres/sqlcgen/**`；`deploy/single/portal.yaml`；`docs/实施/00-总览与公共约定.md`。 |
| B | `runtime/morde-runtime/**`、`runtime/morde-sync/**`、`images/host/**`、`clients/desktop/**`；`server/internal/domain/workspace/**`、`server/internal/domain/host/**`，以及 `server/internal/app/workspace_sync*.go`、`server/internal/app/host*.go`、`server/internal/api/workspace_sync*.go`、`server/internal/api/host_replicas*.go`、`server/internal/api/hosts*.go`、`server/internal/store/postgres/workspace_sync*.go`、`server/internal/store/postgres/host_*.go`、`server/internal/store/postgres/tx_workspace*.go`、`server/internal/store/postgres/queries/workspace_sync*.sql`、`server/internal/worker/workspace_sync*.go`、`server/internal/worker/docker_workspace_sync*.go`。 |
| C | `web/portal/**`、`web/workbench/**`、`clients/nanoMuse/android/src/ios/**`，包括 Portal 生成类型及 iOS 本地化。 |

- 各会话只更新自己的 TODO；需要修改别人的文件时，在自己的清单写明路径、接口、预期行为和验证方式，由对应负责人修改。未列出的文件先登记到本清单并确定唯一负责人，不能同时编辑。公共合同、迁移、生成物及总览由 A 唯一写入；B 提交同步合同和迁移需求，C 负责其客户端类型和调用方。三份清单在本地互相读取，不发送文件。
- 三个会话开始时先查看并保留全部已存在的未提交修改；不得重置、清理、覆盖、批量暂存或擅自回滚。测试使用隔离的临时目录、数据库、端口和构建输出；生成器运行前确认不会覆盖他人改动，必要时在本地隔离副本生成并由所属负责人核对。接口和实现可暂时分阶段在工作树中完成，但交付前必须与 OpenAPI、调用方、迁移及测试组成同一完整变更。
- A、B、C 可同时开始各自独立任务；B 先提出同步合同，A 汇总并落地公共合同后，B/C 按确定的合同联调；最终只由 A 进行集成验收，不越界修改别人的文件。
- 用户要求任何情况都不外发或发送文件：所有修改、证据和测试留在本地；不得向服务、电脑、设备或远端发送文件，也不部署或上传。测试前核对既有测试行为，只运行不传送文件的本地测试、合成元数据和替身。真实文件数据面及依赖发送文件的验收保持待验证，项目上传开关保持关闭。

## 接口变化

1. **已有** `POST /api/workspaces/{id}/sync-sessions`：B 拟为项目上传增加 `source_binding_id`；安装身份由已验证凭据或会话取得，不能信任客户端自报。与 folder、owner、安装和目标绑定，缺少可验证来源或旧基线时不授予删除权。同一幂等键复用原 session；待应用未对账时拒绝新的破坏性上传。非项目同步保持原有语义。A 与 B 确定字段类型、错误和兼容处理后写入 OpenAPI；B 实现，C 只在有对应调用时调整类型与页面。
2. **已有** `GET /api/workspaces/{id}/sync-sessions/{session_id}`：拟增加本次 `target_revision` 对应的 Host 应用状态及可安全展示的影响项；区分待应用、已确认、冲突和未知，具体字段与枚举由 A 按 B 的实现提案固定。只有同一 owner、Host、Workspace、绑定版本及实际应用 generation 的可核验回执才能确认；旧提交字段仍只表示服务端提交。A 写合同，B 实现，C 按合同展示。
3. **已有** `morde_current_conversation_history_search` 内部工具：拟增加可空续查游标，返回 `next_cursor` 和截断原因，保留现有 `partial` 及旧检查点行为。游标按固定历史边界及授权范围校验，错误或授权变更时说明不完整。本变化由 A 实现；仅工具内部变化时不新增公共 HTTP API。
4. **已有** Task 事件以 `(task_id, sequence)` 标识；快照游标和 `task.snapshot_required` 沿用现有合同，按事件类型过滤公开字段。若 A 确认必须增加公开字段，先更新 OpenAPI 并通知 C；没有新字段需求则不另造事件 ID。

## 实施任务

- [ ] A-01（未开始，依赖 B-01 的同步合同提案）固定公共合同与迁移
  - 修改：`server/openapi/portal.yaml`、按需新增 `server/migrations/NNNNN_*.sql`、`server/internal/app/store.go` 和 `docs/实施/00-总览与公共约定.md`。读取 B 在其清单中的来源绑定、已应用快照、待应用关联、回执及迁移提案，确定请求、响应、状态、鉴权、幂等与旧数据处理；复核当前最新迁移后分配编号，只增不改，按 Expand/Contract 实施。
  - 验收：A 在本清单记录已确定的合同字段、迁移编号和版本，明确既有客户端不得把服务端提交当作 Host 完成，B/C 据此继续。此项以合同和迁移固定为结束条件，实际实现、调用方及生成物的一致性在 A-07 联合验收，不等待 B/C 全部任务结束才发布合同。B 的 SQL 查询稳定后由 A 安全生成 `server/internal/store/postgres/sqlcgen/**`。
- [ ] A-02（未开始）收口消息路由与任务恢复
  - 修改：`server/internal/app/conversation_route_*.go`、`server/internal/app/conversation_message_routing.go`、`server/internal/worker/conversation_route_worker.go` 及相关测试。沿用消息持久受理和 `reply/new/update/status/stop/clarify`；稳定 `client_message_id`，重复同内容复用受理结果、同 ID 不同内容冲突；消息关闭或断线不取消任务。
  - 验收：同会话多任务可独立推进；明确追问、修改和停止关联正确任务，歧义先澄清；目标、授权和修订围栏有效，停止一项不停止其他任务，重试与恢复不重复创建或执行。
- [ ] A-03（未开始）完成 DSH 子 Agent 并行和结果回传
  - 修改：`runtime/morde-cloud-runtime/src/engine.mjs`、`runtime/morde-cloud-runtime/test/**`、`server/internal/worker/cloud_dsh_executor.go`、`server/internal/app/agent_sessions.go`、`server/internal/store/postgres/agent_sessions.go` 和既有结果审阅代码及测试。沿用 DSH 与现有 Task/检查点：同一 Task 内两个不同子会话后台运行；按 `(task_id, parent_session_id, spawn_key)` 幂等派发，同标识不同参数冲突，子会话遵守父任务权限和预算。子结果持久化后再进入父执行者的 DSH 后续轮次，确认其实际消费结果再交付；按来源和修订去重，不重复副作用。
  - 验收：屏障证明两个子会话执行区间重叠，父执行者收到并汇总双方结果；覆盖子任务失败或未知、父任务修订/取消、结果交付前后重启及事件重放。提前流式文本不得标为最终成功，重复恢复只发布一次；发起两个顶层 Task 不能替代此验收。先复核 `runtime/morde-cloud-runtime/package.json`、锁文件与 `project/deepseek-harness` 本地上游的工具、MCP、技能、子 Agent、会话及沙箱插件；如需新增插件，核对版本、许可证与隔离/恢复，禁止另建 Agent 循环。
- [ ] A-04（未开始）补齐长会话原文续查
  - 修改：`server/internal/app/cloud_dsh_history_tools.go` 及相应 Broker、授权和检查点测试。新增防篡改游标，绑定服务实例、owner、发起者、会话、Task、固定历史边界、查询、授权/来源版本与有效期，按倒序继续未处理消息；每次续查重验授权与删除状态，区分扫描上限和命中上限。
  - 验收：800 条之外及单页结果上限之外的匹配均可续查、不漏项；跨账号/会话、篡改或过期游标、撤权和旧检查点均正确处理；取不到必要历史时说明不完整，原始消息仍可查看。
- [ ] A-05（未开始）收口事件、审批与结果审阅
  - 修改：`server/internal/api/task_events.go`、相关 Task 查询、结果审阅、审批和测试。按事件类型白名单清理可公开字段；复用每 Task 的事件序号和快照游标；审批绑定操作、版本与授权，结果整理完成后才发布面向用户的结果。
  - 验收：重复事件、审批和结果回放不重复创建卡片、消息或副作用；拒绝、过期、撤权和待审批均不执行；工具原始内容、凭据及其他账号数据不泄露。向 C 交接实际投影及恢复条件。
- [ ] A-06（未开始）验证工具代理、搜索和指定 Host 围栏
  - 修改：`server/internal/app/runtime_tool_executor.go`、搜索和项目文件工具、Host 操作账本和对应 Worker 测试；Host Runtime、镜像与服务端 Host 文件由 B 负责。沿用指定 Host、授权、generation、Workspace 路径/版本及操作身份；普通文字问答不依赖 Host。终端结果保留 Host、操作 ID、退出码、输出的安全摘要及超时状态。
  - 验收：本地替身覆盖搜索来源、授权范围内项目读写、图形操作/终端调用、目标失联、能力缺失、授权撤销与未知副作用；失败如实返回，不切换其他电脑、不重派已确认操作。真实模型与真实 Host 的观察单独留作 A-07 验收，不以替身冒充。
- [ ] A-07（待验证：依赖 A-01～A-06、B 和 C 交接及允许的真实环境）集成与第一里程碑验收
  - 修改：只在本清单记录各会话交接和候选指纹：源码 HEAD、已跟踪与未跟踪改动、Portal/PC/iOS 构建、DSH 锁文件、Host 镜像、数据库迁移、模型及开关版本，以及检查结果和未完成项；代码修复由对应文件负责人执行。核对各会话测试所用基线一致，按 `server/AGENTS.md` 完成适用的格式、lint、测试、生成和临时 PostgreSQL 检查；先排除会发送文件的测试，不能运行的逐项记待验证。
  - 验收：按下表记录每项步骤、预期、实际、身份及任务/操作标识；明确区分本地替身、真实组件、模拟器和真机。A1–A8 的必需真实结果均有证据后才可宣布里程碑完成；当前文件发送禁令下 A5–A7 的真实文件数据面保持待验证且上传开关保持关闭。

| 最终验收 | 责任与判定 |
|---|---|
| A1 两端对话与恢复 | A 联合 C：同账号 PC/iOS 接续真实回复、任务进度与结果；断连、锁屏、重连不重复。 |
| A2 历史与隔离 | A 联合 C：两个会话和两个账号隔离；真实模型超过实际窗口后仍能查询早期要求及原文。 |
| A3 工具与电脑 | A 联合 B：指定授权 Host 的真实界面操作和命令、项目读写与可核查搜索来源；不可用时如实报告。 |
| A4 并行与干预 | A：一个 Task 内两个子 Agent 的实际执行重叠，主 Agent 审阅结果；追问、修改、指定停止正确。 |
| A5 同步基本路径 | B 联合 C：手动同步的结构与哈希、下次点击前不更新、云端不反写；真实文件传输当前待验证。 |
| A6 同步安全 | B：首次非空、云端新增、双侧修改/删除、路径逃逸及归属保护；真实数据面当前待验证。 |
| A7 同步故障 | B 联合 C：根目录不可访问、断连、撤权、回执丢失与重启不误删/误报；真实数据面当前待验证。 |
| A8 事件与审批 | A 联合 C：聊天、详情、审批与结果分流；跨端重放不重复卡片或执行。 |

## 本会话交接记录

- 合同、迁移编号与生成版本：未确定；由 A-01 根据 B 的本地提案填写。
- 最终候选、各项验收证据及未完成原因：未开始。
