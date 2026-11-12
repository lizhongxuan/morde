# A40 服务端整体验收与集成实施任务 TODO

**会话：A｜阶段：P4｜状态：进行中（2026-11-12；A40-T1 完成；A40-T2 既有检查通过，但服务端工作树后续变化，需重验并重新冻结候选版；A40-T3 等 B40/C40-T3 完成后做最终跨端集成验收）。**

来源：[需求与架构](../../Morde个人Muse需求与架构设计方案.md)；共同规则：[执行总览](00-三会话执行总览.md)；接口：[共享合同](01-共享接口与联调合同.md)。

## 开始条件与修改边界

G3；与 B40、C40 并发。只修第一版闭环缺口。

A 的服务端/Runtime/Host/部署和隔离验证范围；公共验收记录及 00 状态。合入 B/C 提交可以，代改其源码不可以；不扩展 APNs 或商业平台。

## 接口变化

不预设新增接口；检查 K1–K4 的实际实现及能力声明一致。有真实合同缺口由 A 修订，B/C 更新消费者后再重验。

## 任务

- [x] A40-T1（完成，2026-11-11）验证云电脑、资源协调与接管
  - 修改：使用隔离 Host/AIO/Workspace 接通主协调、任务工具与产物；核对 RuntimeControl 和在途操作记录，只补本轮需要的缺口。
  - 验收：两任务不能同时抢同一桌面输入；人工接管期间 Agent 停止输入，交回后重读状态。无 Host 的普通问答仍可用，缺失电脑不回退到用户本机；覆盖 A7。
  - 本地进展（2026-11-11）：Hostless Task 保留普通对话；只允许受 Task 授权与 PC 目录 grant 约束的 `file.fetch`，PC 截图、目录浏览、有限文件读取和命令仍要求绑定云端 Host。发现对 Hub 动作的一刀切 Host 检查会误挡 A30 正式聊天取回链路，已收窄该检查；`TestCloudDSHHubHostlessTaskOnlyAllowsFileFetch` 验证 Hostless Task 仅暴露 `file.fetch` 且旧命令调用在创建操作前被拒绝。A30 聊天→DSH→Hub→PC Bridge 全链路重跑中。
  - 接管证据：`runtime_desktop.TestTakeoverCancelsQueuedDesktopInputAcrossTasks` 以两个不同 Task、共用控制器和阻塞输入替身验证输入串行、接管取消后排队任务不产生输入、Stop 均确认结束；`server/internal/worker.TestCloudTaskActiveHostOperationWaitsForTakeoverStopProof` 在临时本机 PostgreSQL 上验证真实 Morde 控制 API/Runtime fence、接管等待停止证明、旧 fence 拒绝、释放后重连同步状态并用新 fence 成功写入 Workspace。上述定向用例及相关 race 用例通过。
  - AIO 桌面烟测：在网络隔离的临时 Linux 容器中运行 `runtime_desktop.TestLiveX11DesktopSmoke`，使用固定 Host/AIO 镜像里的真实 X11、窗口管理器、鼠标与键盘输入，点击并向合成 Tk 窗口输入文本，通过；临时容器和测试二进制已清理。
  - 验收边界：并发/接管取消用例采用可控阻塞输入替身；Host API/Workspace E2E 用合成操作 handler 验证停止证明和释放后新 fence；真实 AIO 烟测单独确认 Linux 桌面输入链。未连接用户线上环境或真实模型，不把这些本地证据扩大为生产/PC 实机验收。

- [ ] A40-T2（进行中，2026-11-12；此前本地必检与专用 OpenSandbox F01 已通过；服务端工作树后续变化，需重验并重新冻结候选版）执行持久化与权限故障验证
  - 前置：A40-T1。
  - 修改：实际 Go/DSH/数据库组件下验证重启、撤权、事件重投、旧任务修订和未知操作；配合限定范围自动化，执行 server/AGENTS.md 与 Runtime 的必须检查。
  - 验收：A1–A12 的服务端证据可追溯；旧凭据、错误账号和过期授权不获文件/任务访问，不重复外部副作用。发布最终两端验收使用的服务版本。
  - 持久化与权限证据：`TestCloudTaskMCPConnectorSurvivesNodeRestartWithoutRedispatch`、`TestCloudTaskArtifactCommitFailureRecoversWithoutRedispatch` 覆盖重启恢复且不重派副作用；`TestCloudTaskAuthorizationRevocationFencesHostDispatch`、`TestCloudTaskOwnerAccountDisableFencesHostDispatch`、`TestCloudTaskMCPGrantRevocationBeforeAndAfterDispatch` 覆盖撤权边界；`TestCloudDSHExecutorDeliversPendingConversationMessageOnce`、`TestCloudDSHExecutorRecoversCompletedCheckpointWithPendingMessage` 覆盖检查点恢复后的待发消息投递；`TestCloudTaskHostGenerationFencePreservesAgentChildCheckpoint`、`TestTaskStopWorkerNotFoundWaitsForSafeAfter`、`TestTaskStopWorkerSettlesNotFoundAfterSafeAfter` 覆盖未知/停止操作安全收敛。
  - 文件与账号边界：`TestPostgresTaskFileFetchPersistsTaskFenceTicketHashAndOfflineWait` 验证旧 Task 修订票据拒绝；`TestPostgresTaskFileFetchTransferResumeContentLifecycleAndCancel` 验证过期票据不能 manifest/chunk/commit、派发后 Task 修订改变时拒绝整条数据面、非 owner 不能读取状态/内容或取消/删除。该用例最新普通与 `-race` 定向重跑均通过。`TestGetTaskFileFetchRequiresPortalSessionAndCanonicalQuery` 与 `TestA30TaskFileFetchRunsFromChatThroughPostgresHubAndPCBridge` 覆盖凭据类型、账号隔离及正式聊天到 PC Bridge 取回；A30 全链路耗时 5.41 秒并通过。`TestCloudDSHHubHostlessTaskOnlyAllowsFileFetch` 确认无 Host 普通聊天可用，同时只开放获授权的 PC 文件取回。
  - 必须检查（2026-11-12 当前工作树）：`make -C server generate` 通过，sqlc 生成目录重跑前后哈希一致；`make -C server lint` 通过（0 issues）；`make -C server test race cover` 普通、竞态及覆盖率三阶段通过，总覆盖率 45.1%（无 `CH_TEST_DATABASE_URL`，此三阶段中的数据库集成用例跳过）；在本机临时 PostgreSQL 上以 20 分钟 Go 包超时运行完整 `make -C server it` 通过；该次命令因未配置 `CH_OSB_URL`/`CH_OSB_API_KEY` 跳过通用 OpenSandbox 探测。迁移 185/186、187/188/189 升级/降级及保护用例和历史迁移 162 测试通过；修复了迁移 162 测试误用后续 schema 的问题。A30 聊天→Postgres Hub→PC Bridge 文件取回集成测试通过；本轮新增桌面 session 仅允许活动 desktop installation 的 owner 隔离用例普通与 race 均通过。本段所列 `make it` 本身不是 OpenSandbox 实例验收，专项 F01 结果见“当前树复核”。
  - 交付边界：验收基线为本地 `HEAD 6317715` 加当前工作区变更；没有创建发布版本、连接用户线上环境或上传安装包，最终集成版本仍由 A40-T3 汇总。完整 `make it` 当时未配置 `CH_OSB_URL`/`CH_OSB_API_KEY`，因此该次命令跳过通用 OpenSandbox 探测；随后另行执行的本机专用 F01 OpenSandbox 集成见下方当前树记录。未声称 PC/iOS 实机验收。事件恢复由检查点与待发消息测试覆盖。
  - 当前树复核（2026-11-12）：当时候选树已扩展至迁移 189。`make generate`、`make lint`（0 issues）、`make test race cover`（三阶段通过，总覆盖率 45.1%）及本机临时 PostgreSQL 上完整 `make it` 均通过；sqlc 生成物重跑无差异。修正迁移 185/186/187/188/189 覆盖及历史迁移 162 测试夹具；在 `cloud_dsh_executor_test.go` 增加独立确认前崩溃注入，恢复后验证复用同一检查点、工具副作用不重放、用户/工具事件各仅一次并提交 Task 结果，定向测试通过。此前专用 Lima VM `morde-a40-osb-20261112` 的 OpenSandbox 1.1.1 + runsc `release-20261102.0` F01 测试已通过（9.16 秒）；本轮另在 Docker Desktop 上构建仓库固定 digest 的 OpenSandbox 补丁镜像，使用临时 PostgreSQL、临时 API key、loopback 端口和独立 sandbox 网络运行 `TestOpenSandboxIntegrationList` 与 `TestF01HostReleaseKeepsCommittedWorkspaceReadback`，均通过（worker 42.026 秒）；F01 清理 Host 沙箱及卷，本轮临时 DB/服务容器和网络已清理。Docker Desktop 使用 runc；该结果不替代 Ubuntu/runsc 验收。上述结果只覆盖当时树。随后核对候选源码指纹时，发现 `server/internal/api/{conversations.go,tasks.go,tasks_test.go}`、`server/internal/store/postgres/sqlcgen/{conversations.sql.go,tasks.sql.go}` 和 `task_conversation.go` 在指纹生成后又变化；临时指纹因此作废，不作为验收版本。需在这些共享源码变更稳定后重跑受影响必检、完整集成检查并重新生成候选清单。A40-T3 仍负责合并 B40/C40 结果、核对跨端链路并冻结最终集成结论；不得据此前测试宣称所有 A1–A12 或跨端实机验收完成。

- [ ] A40-T3（未开始）汇总三会话技术交付
  - 前置：A40-T2、B40-T3、C40-T3。
  - C40 前置复核更新（2026-11-12，取代此前“账号隔离断言未执行”的诊断）：loopback 合成 A/B 账号的 G3 离线消息隔离 UI 用例通过（1/1，110.882 秒），验证 B 看不到 A 的离线消息且恢复后服务端只接收一次。此前 `NSURLErrorDomain -3003` 已定位为未签名 UI 测试产物缺少 Keychain entitlement，Keychain `SecItemAdd` 返回 `-34018`；Morde 已配置默认 Keychain access group，并在 ad-hoc signed Simulator 中通过登录。真实设备、锁屏、云电脑接管和跨 PC/iOS 验收仍待完成。摘要：`.local/c40-sim/C40-G3-account-isolation-2026-11-12.summary.txt`。B40-T3/C40-T3 前置仍未满足，因此 A40-T3 保持未开始。
  - 前置复核（2026-11-12）：B40-T2 局部通过：本地 Portal installation revoke API 有撤销安装 204、被撤销安装 401、另一安装 200 的证据；`morde-main-recovery.test.mjs` 53/53 通过，新用例用 `/api/me` 401 替身验证主进程自动失权并保留 Workspace mapping/checkpoint。尚未用真实 Portal 撤权和 Electron UI 验证登录链路/恢复提示，也未实测 Mac 整机断网与睡眠唤醒。C40 的账号隔离最终复测见前一条“C40 前置复核更新”：ad-hoc signed Simulator UI 用例 1/1 通过，早期 client-login/`-3003` 失败诊断已被该成功复测取代；真机、锁屏、云电脑接管与跨端对账仍待验。B40-T3/C40-T3 前置仍未满足，因此 A40-T3 保持未开始。
  - 修改：顺序合入 B40/C40，复核所有变更位于各自边界；记录代码/合同/环境版本、真实组件与替身范围、未通过项。更新 00 技术验收状态，不把模块测试等同于全链路。
  - 验收：A1–A12 全部有结果与证据；PC/iOS 实机或其未具备条件被准确区分。任何必要项未通过时技术交付保留待验证；不部署到用户线上，不上传安装包。

- [ ] A40-T4（未开始）记录本人日常试用反馈
  - 前置：A40-T3。
  - 修改：技术验收后建立一周使用记录，收集用户本人反馈与本地问题证据；问题按 A/B/C 原写入归属处理。本任务不授权代理读取真实私人文件、外发文件或访问在线应用。
  - 验收：有至少一周实际使用反馈，关键重复工作可完成，未解决问题和后续优先级清楚。没有实际反馈保持待验证，不用一次演示代替；本轮写清单不启动观察任务。

## 交付

技术验收与一周试用分别标记，均不得由文档完成状态推断为已通过。 只汇报本地位置、版本、验证及未完成项；不得外发文件。已有实现通过本任务验证可直接复用，不为勾选任务而重写。

