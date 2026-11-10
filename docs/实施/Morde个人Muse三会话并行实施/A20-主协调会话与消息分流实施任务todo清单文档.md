# A20 主协调会话与消息分流实施任务 TODO

**会话：A｜阶段：P2 前半｜状态：进行中（A20-T1 定向实现和检查通过，等待 G1/A10-T3 iOS 集成验收；T2–T4 已有本地实现及验证，受前置关口约束未勾选）。**

来源：[需求与架构](../../Morde个人Muse需求与架构设计方案.md)；共同规则：[执行总览](00-三会话执行总览.md)；接口：[共享合同](01-共享接口与联调合同.md)。

## 开始条件与修改边界

A20-T1 的协调器代码已完成审查和本地定向检查：文本直答、无工具轮次、Task 运行时处理下一条消息、跨会话 Runtime fence 均有实现或测试证据。修复前发现的 `StartCoordinator` 会话 fence 缺口已补齐；当前 G1/A10-T3 iOS 登录与 App-close 集成验收尚未通过，因此 T1 记为待验证，不发布 G1。T2–T4 的本地实现/验证不越过 T1 的正式前置关口；A21 仍未开始。

runtime/morde-cloud-runtime/；server/internal/app/、worker/、store/postgres/ 的会话协调与任务提交；必要的新协调模块标记为新增；公共启动/类型/迁移仍只有 A 写。

## 接口变化

落实 K3 主协调事件与现有 K1 分流；K2 中相关字段在实现时同步正式 OpenAPI。保留 update/new/status/stop/clarify 的业务语义，补直接回复和委派意图，不绕过应用层。

## 任务

- [ ] A20-T1（待验证：本地定向检查通过，等待 G1/A10-T3 iOS 集成验收）增加独立的 DSH 主协调上下文
  - 修改：在现有 DSH adapter/session/provider 机制内增加会话协调角色和检查点接线。主上下文与 Task 执行上下文分离，仍经 Morde 模型网关、授权和计量，不新写 Agent 循环。
  - 验收：简单问答可直接回复；执行任务 A 期间，主协调轮次可以结束并处理下一事件；跨会话/跨账号上下文不串用。模型不支持工具时普通问答仍可用。
  - 本地检查：`TestConversationCoordinator*` 与 `TestCloudDSHCoordinator*` 定向 Go 测试通过；Postgres `TestDSHCoordinatorDirectReplyUsesNoToolsModelAndAppliesOnce` 在临时数据库通过，模型能力为 `capability_tools=false`；DSH runtime `npm test` 94/94 通过。新增 Task 持续运行时处理第二条消息的回归测试，以及拒绝其他 owner、conversation 或 service instance coordinator 写入当前 Runtime 的防护测试。Xcode `build-for-testing` 使用 ad-hoc Simulator 签名通过，UI 测试目标可编译；G1 UI/App-close 端到端验收仍待 iOS 27.2 Simulator runtime 下载完成后运行。

- [ ] A20-T2（待前置验收；路由实现与本地验证完成）将持久消息分流接到主协调决策
  - 前置：A20-T1。
  - 修改：复用 conversation_route_worker、route actions 和消息持久记录，按 K3 认领事件、提供获准上下文并校验动作。提交动作与保存消费结果有明确原子边界，模型调用期间不持有会话数据库锁。
  - 验收：新任务、修改、状态和停止走各自受控动作；直接回复只落一条会话消息；未知目标或歧义走澄清 fallback。同话题独立工作能并行；重复消费不创建第二个 Task，未知目标不误停现有任务。模型 provider 阻塞时，另一条同会话消息的接收/数据库操作仍能成功，证明网络调用不持有会话事务锁。

- [ ] A20-T3（待前置验收；Task 子会话、后台 Job 与恢复实现及本地验证完成）委派有界工作并保留独立生命周期
  - 前置：A20-T2。
  - 修改：DSH 委派/后台 Job 与现有 Task/子会话接线；上下文、工具、预算和来源限定。主协调回复完成不取消独立执行任务，不再被父执行的 waitForBackgroundJobs 路径锁住收消息。
  - 验收：长任务派出后可接收并开始独立 B；单任务内的子 Agent 有真实归属，越权工具被拒绝；达到预算/并发限制显示等待或失败而非无限递归。

- [ ] A20-T4（待前置验收；生产入口已接到唯一持久路由 worker，本地验证完成）收敛唯一决策入口并交付前半阶段
  - 前置：A20-T3。
  - 修改：公共入口由 A 切换到唯一协调决策路径；按原标识接续未消费消息，停用旧重复决策路径。保留现有受控动作提交；最终结果发布的统一由 A21 完成。
  - 验收：旧/新处理器不能同时为同一消息创建任务；未消费消息不丢，状态查询不触发重执行。完成定向 Go/Node 检查，公布 A21 依赖的本地版本；完整主协调能力尚不宣告 G2。

## 交付

A20 完成后接 A21；B20/C20 可以继续按合同实现，但真正联调等 A21-T3。只汇报本地位置、版本、验证及未完成项；不得外发文件。已有实现通过本任务验证可直接复用，不为勾选任务而重写。

## A20 本地实现与 DSH 插件核对（2026-11-10）

- A20-T1：`TestConversationCoordinator*`、`TestCloudDSHCoordinator*` 与 DSH Runtime 定向用例通过；文本直答、Task 运行时接收下一消息及 coordinator 身份/上下文围栏有回归覆盖。G1 的本地 Portal 登录和 App-close UI 验收仍是未完成的正式前置条件。
- A20-T2：唯一生产路由 worker 已接入持久消息队列、主协调器、原子动作回执及 direct reply；route worker、Go/PostgreSQL 定向用例和 `make test race cover` 通过。`make it` 首次发现 `TestDSHSearchReadThroughBrokerPersistsSourceDateAndWorkChain` 传入半套 MCP connector 依赖；该测试不调用 connector，改为两项均不配置后定向 PostgreSQL 集成通过。全量 `make it` 复跑仍未通过：一次全跑中，Postgres worker 包出现重复 token fixture、personal proof fixture 与 host runtime fence fixture 失败，并在 `TestTaskWorkspacePublishSerializesSameContentAcrossTasks` 等待数据库回复时触发 10 分钟包级超时。随后单独重跑 `TestPostgresScheduledPersonalContextRequiresExactOwnerRunAndTask` 和 `TestCloudDSHStreamFencesPersonalProofRevisionAndModelVersion` 通过；MCP 两项用例最初因 fixture Host 未设为 AI control 被拒绝，在 `newF02MCPWorkspaceFixture` 补上本地 Host AI-control 状态后，`TestCloudTaskMCPWorkspaceFence` 与 `TestCloudTaskMCPGrantRevocationBeforeAndAfterDispatch` 定向 PostgreSQL 复验通过。Store 包单独重跑确认 `TestPostgresProjectTaskRevocationRequiresAgentStopEvidence` 返回 `project not found` 而断言期待 `CodeInvalidState`；`TestPostgresTaskClaimWithCheckpointDoesNotReloadConversationHistory` 在个人上下文缺失时返回 `found=false`，与当前“上下文无法恢复就取消任务”的恢复策略不一致，二者没有为通过测试而放宽生产校验。workspace publish 超时尚未单独复验。另有 `TestPersonalOfficeAttachmentUploadTaskReaderE2E` 在当次全跑失败，之后其合成 PPTX fixture 已补全 presentation relationships，定向复验通过。全量失败记录保留，不作为 A20 通过证据。
- A20-T3：Morde 持久子 Task 绑定父 Task、父 Session 和 spawn key；子工具由 Morde 授权子集校验，Job 状态/输出/取消留在既有 Task 账本；恢复、取消隔离及同 Runtime 并行用例通过。主协调器独立进程无 Task 工具，因此 Task 完成后的后台执行与消息分流互不锁住。
- A20-T4：Portal 生产接线只启动一个 `ConversationRouteWorker` 作为持久消息分流入口；旧路由模型服务仅由该 worker 调用。重复事件沿用已受理 route receipt，不再重复创建 Task。
- DSH 评估：锁定依赖和上游 `dsh-subagent`、`dsh-tool-subagent`、`dsh-jobs`、`dsh-jobs-local` 均为 `0.2.2-rc.1`，`dsh-subagent` 许可为 MIT；版本和许可没有阻碍。[上游 subagent provider 合同](https://github.com/deepseek-ai/deepseek-harness/blob/dsh-v0.2.2-rc.1/packages/subagent/subagent/src/types.ts)、[tool-subagent](https://github.com/deepseek-ai/deepseek-harness/blob/dsh-v0.2.2-rc.1/packages/subagent/tool-subagent/README.md) 可扩展外部 provider，但工具请求没有 Morde 的每次授权工具子集、Task/spawn 身份和预算字段；external provider 也没有跨进程重新附着接口，且 provider `dispose()` 的合同要求取消剩余工作并等待静止，和 Morde 子 Task 脱离父执行、由 Task 账本恢复的生命周期不一致。`dsh-jobs-local` 在 owner 或 harness 关闭时结束任务；`dsh-jobs` 只提供抽象服务合同，仍需自建持久 provider。[上游 jobs](https://github.com/deepseek-ai/deepseek-harness/blob/dsh-v0.2.2-rc.1/packages/jobs/jobs/README.md)、[jobs-local 生命周期](https://github.com/deepseek-ai/deepseek-harness/blob/dsh-v0.2.2-rc.1/packages/jobs/jobs-local/README.md)。因此本轮继续复用已锁定 DSH Agent Loop/Session，通过 Morde IPC/provider 接入持久 Task、授权和恢复；没有再添加一个无法承载这些生命周期的 DSH activation/job manager，也没有另写 Agent loop。若未来将生命周期改为 DSH activation 所有，需另做 Morde 持久 provider、恢复桥和完整合同测试。

