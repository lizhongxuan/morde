# C：PC 页面与 iOS 实施任务 TODO

依据：[第一里程碑收口技术设计方案](../../Morde第一里程碑收口技术设计方案.md)、仓库根目录 `AGENTS.md` 和 `clients/nanoMuse/AGENTS.md`。本清单供第三个会话执行；与此前三会话清单的 A/B/C 划分不同，不修改旧清单及其状态。

## 修改边界与并行约定

| 唯一写入方 | 文件范围 |
|---|---|
| A | `runtime/morde-cloud-runtime/**`；除下列 B 范围外的 `server/**`；公共 `server/openapi/portal.yaml`、`server/migrations/**`、`server/internal/app/store.go`、`server/internal/api/api.go`、`server/cmd/portal/**`、`server/internal/store/postgres/sqlcgen/**`；`deploy/single/portal.yaml`；`docs/实施/00-总览与公共约定.md`。 |
| B | `runtime/morde-runtime/**`、`runtime/morde-sync/**`、`images/host/**`、`clients/desktop/**`；`server/internal/domain/workspace/**`、`server/internal/domain/host/**`，以及 `server/internal/app/workspace_sync*.go`、`server/internal/app/host*.go`、`server/internal/api/workspace_sync*.go`、`server/internal/api/host_replicas*.go`、`server/internal/api/hosts*.go`、`server/internal/store/postgres/workspace_sync*.go`、`server/internal/store/postgres/host_*.go`、`server/internal/store/postgres/tx_workspace*.go`、`server/internal/store/postgres/queries/workspace_sync*.sql`、`server/internal/worker/workspace_sync*.go`、`server/internal/worker/docker_workspace_sync*.go`。 |
| C（本会话） | `web/portal/**`、`web/workbench/**`、`clients/nanoMuse/android/src/ios/**`，包括 Portal 生成类型及 iOS 本地化。 |

- 各会话只更新自己的 TODO；需要修改别人的文件时，在本清单写明路径、接口、预期行为和验证方式，由对应负责人修改。未列出的文件先在 A 的清单确定唯一负责人，不能同时编辑。公共 OpenAPI、迁移、服务端生成物由 A 写；Desktop 主进程、preload 和原生资源界面由 B 写；本会话负责 Portal/iOS 页面、类型和调用。三份清单在本地互相读取，不发送文件。
- 开始前查看并保留全部已有未提交改动；不得重置、清理、覆盖、批量暂存或擅自回滚。测试使用隔离的临时目录、端口、数据库和构建输出；生成器运行前确认不会覆盖其他会话的修改。对外合同、实现及客户端可分阶段修改，但交付前必须组成同一完整变更。
- A、B、C 可同时开始独立工作：C-01～C-04 可先按已有合同执行；同步入口按 B 的 Desktop bridge 与 A 的公共合同确定后接入。优先使用现有 PC 入口和组件，不重复创建同功能页面。iOS 修改遵循其目录约定，新增可见文字同步 `Localizable.xcstrings` 各现有语言。
- 用户要求任何情况都不外发或发送文件：不向服务、电脑、设备或远端发送文件，不部署或上传。界面验收使用本地替身和内置浏览器；模拟器仅在无需安装或发送文件时使用，否则记待验证。不得为了验收向真机或模拟设备发送应用包或测试文件；真实同步数据面保持待验证且上传开关关闭。

## 接口变化

1. **已有** 消息、Task 快照、事件流及审批接口：按 A 确定的投影消费，不新增全局事件 ID。聊天按消息 ID 和会话 `sequence` 去重，任务详情按 `(task_id, 事件的 sequence)` 去重；快照的游标字段为 `event_sequence`，收到 `task.snapshot_required` 以新快照及其游标恢复，重新检查当前账号和权限。
2. **已有** Host 列表字段 `workspace_runtime_online`、`workspace_runtime_last_seen_at`：分别展示生命周期和 Runtime 连接状态，桌面连接另行判断。字段缺失或请求失败显示未知；不因为心跳不可用断言电脑已停止。前台每 10–15 秒刷新，页面不可见时停止周期刷新。
3. **已有** Desktop preload 的 `projectUploadTargets`、`selectProjectDirectory`、`uploadProject`、`state`、`onState`、`onSync`：只在实际桌面桥接可用的入口调用；B 确定输入、来源绑定、状态和错误，本会话消费并展示。确认项目名、目录名称、指定电脑、目标路径和实际范围；浏览器独立运行时不伪装成本机同步可用。
4. **拟扩展** 同步 session GET 的 Host 应用状态：A/B 确定字段后更新 `web/portal/src/api/schema.d.ts` 及实际调用方；仅证明本次 session 和指定 Host 应用完成时显示完成。等待、冲突、缺失回执和未知状态显示待处理；旧字段不能作为完成证明。

## 实施任务

- [x] C-01（已完成：PC 对话与任务投影/审批验收通过）收口 PC 对话、任务详情和审批
  - 修改：`web/portal/src/routes/Assistant.tsx`、关联组件、`web/portal/src/app/api.ts`、消息 outbox 及测试。沿用现有消息/Task 流：主助手面向用户的内容进入聊天，工具/子 Agent 过程进入任务详情，审批单独展示；结果审阅未完成时展示整理中。
  - 验收：内置浏览器配合本地替身检查发送、进度、结果、审批、拒绝与待处理；重复事件或断线重连不增加重复气泡/卡片，旧快照不覆盖新状态；工具原始参数或内部内容不进入聊天。真实后端问题记录交 A，不越界修改。
- [x] C-02（本地实现/构建与可运行投影测试完成；Minis UI 测试因现有依赖缺失待验证）收口 iOS 对话和任务投影
  - 修改：`clients/nanoMuse/android/src/ios/Morde/MordeApp.swift`、实际调用的数据模型与相关测试、`clients/nanoMuse/android/src/ios/Localizable.xcstrings`。与 PC 采用同一消息、事件、审批和结果事实；处理前后台恢复、撤权和账号切换。
  - 验收：本地构建和可执行的投影测试覆盖跨会话隔离、重复事件、审批及结果整理；可在不安装或发送文件的条件下用已有本地模拟器检查时记录实际界面，否则记待验证。真实 iOS/锁屏和模型回复交 A-07 单独验收。
- [x] C-03（已完成：本地双会话切换、outbox/重复事件与任务隔离检查通过）校正多会话、消息幂等与断线恢复
  - 修改：`web/portal/src/app/message-outbox.ts`、`web/portal/src/routes/Assistant.tsx`、iOS Morde 消息模型/恢复逻辑和相应测试。客户端持久保存 `client_message_id`，网络失败先按原 ID 对账；以会话消息序号和每 Task 独立快照游标恢复，账号切换清除旧投影。
  - 验收：内置浏览器及本地 iOS 测试中创建、切换两个会话；任务运行时断线、重开和重试不创建重复任务或显示另一账号/会话的数据；一个任务取消不影响其他任务，任务关联不明确时展示澄清。
- [x] C-04（PC 与 iOS 本地逻辑/构建验证完成；Minis UI 测试与真实设备页面待验证）展示 Host 可达性与桌面连接
  - 修改：`web/portal/src/routes/Hosts.tsx`、Host 列表/电脑面板组件、iOS Host 展示及测试。保留 Host 生命周期，单独显示服务端 Runtime 心跳状态及最后观测时间，noVNC/桌面连接单独显示；前台刷新，恢复网络或重新进入时立即刷新。
  - 验收：内置浏览器、本地 iOS 测试覆盖心跳过期和恢复、字段缺失、请求失败、电脑仍运行但桌面断开；状态未知不显示成已停止或已连接，后台不持续轮询。若服务端字段不足，交 A/B 修复。
- [x] C-05（已完成：复用 Desktop 现有手动项目上传页；session receipt、待处理/冲突/未知状态和重启恢复通过本地测试；真实同步数据面留待 A-07）接入 PC 同步界面
  - 修改：与 B 核对现有桌面同步入口；若其已满足可达性及范围要求，就沿用该入口，不增加 Portal 重复页面。若 PC 主流程确需 Portal 入口，则修改 `web/portal/src/routes/Hosts.tsx` 或现有可达入口及 Desktop bridge 的 Portal 类型/调用与测试。用户可确认目录、项目名、指定 Host、目标路径、实际纳入/排除范围，并查看进行中、待处理、完成和失败。
  - 验收：新增 Portal 入口时，以内置浏览器及本地 bridge 替身检查按钮、确认、冲突/撤权/离线/未知回执和关闭的功能开关；没有真实 bridge 时显示不可用。如复用 B 的原生入口，由 B-05 提供对应界面证据并由 C 核对跨入口一致性。不在文件变更或定时器触发同步，不提供云端反向写 PC。真实文件发送和实际设备验收保持待验证。
- [x] C-06（Portal 类型已按最新 OpenAPI 重新生成并核对无漂移；iOS 仅在 `status=applied`、Host/绑定/目标/运行代次/健康状态和影响项均通过时接受 `applied_revision >= target_revision`，未知状态仍 fail-closed；迁移验证属于 A-01）同步客户端合同
  - 修改：`web/portal/src/api/schema.d.ts`、受影响的 Portal API 调用和 iOS 模型/测试。A 固定 OpenAPI 后由 C 安全生成 Portal 类型并核对本地变更；按实际公开合同处理新字段缺失、未知值和兼容响应，不预先假定字段名。
  - 验收：生成类型与最终 OpenAPI 一致，PC/iOS 调用均能编译；旧响应和未知 Host 应用状态不被显示为完成，账号/授权变化后不能读取旧状态。若生成会覆盖他人改动，先隔离并协调。
- [x] C-07（本地交接记录完成：C-01～C-06、A-01～A-06 和 B-02～B-06 有实现/测试证据；真实模型、设备和文件数据面按约束留待 A-07）向 A 交付两端验收证据
  - 修改：本清单交接记录。记录本地测试、内置浏览器页面和能够在不发送文件条件下观察的 iOS 结果，说明版本、使用替身与限制；由 A 汇总 A1、A2、A5、A7、A8 的最终状态。
  - 验收：PC 与 iOS 的消息、任务、审批、Host 和同步状态有明确对应；真机、真实模型及真实同步中未执行的部分单独标记待验证，不以本地替身代替。

## 本会话交接记录

- 请求 A/B 修改的合同、路径与验证方式：A-01 的 OpenAPI、唯一 `00191_workspace_sync_project_source_receipts.sql`、隔离 PostgreSQL migration up/down、schema smoke 与 stale-baseline 并发 CAS 已完成；`TestWorkspaceProjectUploadReceiptRejectsConcurrentBaselineAdvance` 通过行锁等待屏障验证旧 receipt 冲突且 baseline 不回退，并以 `-count=5` 通过。A-02/A-03 本地消息幂等、并发路由/取消、DSH 子会话并行、父结果消费与重启恢复已验收；PostgreSQL receipt/checkpoint/child job 用例通过，DSH Runtime 106/106 通过。A-05 的 Task event envelope、审批/结果审阅与恢复投影由服务端 API/app、Portal 和 iOS 逻辑测试覆盖；未知安全 `task.*` 类型保留序号并投影空 payload。B-02～B-05 在 Host replica、Server Store/API、Runtime 与 Desktop bridge 中完成来源基线、session receipt 和恢复：同 session 回执 GET 校验 Host/Workspace/binding/target/manifest/generation；只有已应用 receipt 才清理检查点，旧聚合状态不再作完成依据。不得因 receipt 验证而开启真实文件传输。Portal 类型与 iOS decoder 已消费当前合同。
- PC/iOS 本地检查结果、候选版本与未完成原因：Portal `Assistant.test.tsx`、`Hosts.test.tsx`、`message-outbox.test.ts` 共 156 项通过，Portal ESLint 与 `tsc -b` 通过；本轮按最新 OpenAPI 重新生成 `schema.d.ts`，再次运行 `tsc -b` 并与独立生成结果核对无差异。内置浏览器使用 `VITE_MOCK_API=1` 检查了 Host Runtime 缺失时显示“未知”且与 Host 生命周期/noVNC 状态分开；新建并切换两个会话，第二会话重开后仍只显示自己的本地演示消息。演示模式明确提示消息不由 AI 执行；未进行真实模型或远端 API 验收。Workbench noVNC HTTP 200 但无连接标记 E2E 1 项通过。Desktop `npm run typecheck`、`npm run build:test` 与 `node --test tests/morde-main-recovery.test.mjs tests/morde-renderer.test.mjs` 共 83 项通过，覆盖根目录失联提前失败、Host 名称/路径/全量范围确认、completed session 只读原 receipt 不重扫重传、pending/conflict/unknown 保留检查点及 applied 后清理；测试使用临时空目录与 mock Sync，不传文件块。B Runtime 重启 receipt 重试、A-01/B Store 临时 PostgreSQL migration/receipt/high-revision/affected-path 用例通过。Morde Swift package `swift test` 本轮 15/15 通过，覆盖服务端验证谱系条件下较新 `applied_revision`。A-05 `go test ./internal/api -count=1` 与 `go test ./internal/app -count=1` 通过，覆盖安全未知事件 envelope/空 payload 与已知事件读取；事件写入白名单仍拒绝未知类型。Morde scheme clean build 与含 C-06 模型的增量 build 均退出 0；本轮 `git diff --check` 通过；此前 Localizable.xcstrings JSON 和两个 pbxproj `plutil -lint` 通过。Minis `build-for-testing` 因现有缺失的 `Configs/ProviderCustomization.xcconfig`、FFmpeg/libav* XCFramework 与 `Rclone.xcframework` 在依赖解析阶段失败，相关 iOS UI/审批测试未能执行；没有安装或发送 App，真实 iOS 前后台/桌面页面待验证。真实模型、真机、Host 操作与文件数据面继续留待 A-07，实际文件传输未执行。
