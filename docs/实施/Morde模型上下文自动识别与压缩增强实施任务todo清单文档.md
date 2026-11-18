# Morde 模型上下文自动识别与压缩增强实施任务 TODO

> 来源：[模型上下文容量自动识别与压缩优化设计方案](../Morde模型上下文容量自动识别与压缩优化设计方案.md)、[原上下文压缩与长期记忆设计方案](../Morde%20AI对话上下文压缩与长期记忆设计方案.md)。本清单记录当前工作区实施进度，尚未验收的任务不勾选。

## 修改边界

- Morde 系统默认提供的每个模型必须先由 Morde 查阅官方资料，确认精确 API 路由的上下文窗口后，才能进入默认模型目录；没有可核实官方容量的模型不得作为系统默认模型发布，也不能让用户手填来绕过目录缺项。用户新增或编辑系统预设无需也不能手动填写 token 窗口。自定义 endpoint 没有可信目录值时容量保持未知，用户可在高级设置填写可选覆盖。
- 只修改 Morde 的模型连接数据/API/UI、Morde 到 DSH 的启动配置与事件适配。保持 `runtime/morde-cloud-runtime/package.json` 和锁文件中的 DSH Runtime 固定版本；使用 DSH 公开插件、`TokenMeter`、`BasicCompactionEngine` 和其压缩恢复机制。
- 不修改 `project/deepseek-harness/**`、`runtime/morde-cloud-runtime/vendor/deepseek-harness/**` 或任何 DSH 库源码；不复制 DSH 插件、不另写 Agent 循环。Morde checkpoint 校验只负责安全适配 DSH 公开事件。
- 容量必须绑定精确 `base_url + model_id` 路由。未知路由不套用通用窗口值、不按最大输出或模型名猜测、不发起计费试探请求。保留用户/会话隔离、授权、幂等、任务租约、原始记录与摘要来源证明。
- 影响普通用户模型设置时，最终必须从实际 `morde-pc` 入口验收；后台模型管理按管理员 Web 职责验收。测试数据使用合成内容，不向外发送文件或应用包。

## 系统默认模型容量目录

下表按 2026-11-18 官方资料核对，使用 API 上下文窗口，不使用产品订阅额度或最大输出量。新增默认预设必须同步增加精确路由记录、来源及目录版本，并通过覆盖率检查。

| 模型 ID | 默认 endpoint | 上下文窗口 | 官方来源 |
| --- | --- | ---: | --- |
| `gpt-6-luna` | `https://api.openai.com/v1` | 1,050,000 | [OpenAI API](https://developers.openai.com/api/docs/models/gpt-6-luna) |
| `gpt-6.1-sol` | `https://api.openai.com/v1` | 1,050,000 | [OpenAI API](https://developers.openai.com/api/docs/models/gpt-6.1-sol) |
| `gpt-6-astra` | `https://api.openai.com/v1` | 1,050,000 | [OpenAI API](https://developers.openai.com/api/docs/models/gpt-6-astra) |
| `deepseek-flash` | `https://api.deepseek.com` | 1,048,576 | [DeepSeek Models API](https://api-docs.deepseek.com/api/list-models/) |
| `glm-5.1` | `https://api.z.ai/api/paas/v4` | 200,000 | [Z.AI GLM-5.1](https://docs.z.ai/guides/llm/glm-5.1) |

## 接口变化

| 接口 | 约定 |
| --- | --- |
| 模型连接 POST/PUT | 新增可选 `context_window_override_tokens`，只供不在系统目录的自定义 endpoint 使用；`null` 清除覆盖。字段省略时沿用同一连接原覆盖，endpoint/model 路由变化时清除旧覆盖。系统预设拒绝覆盖。兼容期仍接受旧写入字段 `context_window_tokens` 作为弃用覆盖字段；新旧字段同时提供时拒绝。 |
| 模型连接 GET | `context_window_tokens` 表示有效容量；返回 `context_window_override_tokens`、`context_window_source`、`context_window_resolved_at` 和 `context_window_catalog_version`。本实现实际产生 `model_catalog`、`override`、`legacy_manual`、`unknown`；`provider_metadata` 与 `provider_error` 仅为保留的来源枚举，不由当前解析器产生。容量未知时有效值为 `null`。 |
| Morde 内部 DSH 启动帧 | `task_start` / `start` 可包含服务端按已授权模型路由确认的可选 `context_window_tokens`。缺失或无效按未知处理；只扩展 Morde 校验和配置，不放宽 owner、generation、authorization version、session 或工具边界。 |
| 压缩和错误恢复 | 沿用现有摘要 API、DSH checkpoint 和模型网关。仅将明确上下文超限映射为当前固定 DSH 版本支持的 `CONTEXT_WINDOW_EXCEEDED`；普通 413、限流、流中断不更改容量。 |

新增或变更 API 字段须同步 `server/openapi/portal.yaml`、服务端和 Portal API 类型。持久化改动使用新迁移号；执行前核对当前迁移序列，避免覆盖并行变更。

## 实施任务

- [x] **T01（完成）分离有效容量、覆盖值与来源**
  - 修改：`server/migrations/00208_model_context_capacity_metadata.sql`、模型连接 App/Store/SQLC/API。保留有效字段并新增覆盖值、来源、解析时间及目录版本；自定义连接的旧手填值保留为 `legacy_manual`。命中系统目录的旧记录改为官方目录值并清除旧覆盖。
  - 验收：迁移集成测试覆盖旧自定义值保留、系统预设旧值替换、超过旧 schema 上限时阻止降级、可表示值可回滚；所有权校验及覆盖值范围由模型连接服务校验。

- [x] **T02（完成）维护默认模型目录并自动解析系统预设**
  - 修改：`server/internal/app/model_context_capacity.go` 与 `model_context_capacity_test.go`。只按精确 endpoint 和 model ID 命中目录；目录值优先于旧输入，服务端拒绝系统预设的新增覆盖。未知自定义路由不从名称推测容量。
  - 验收：目录完整覆盖上表五个预设且有官方链接和版本；目录覆盖测试从系统预设列表读取模型并验证均有容量记录，新增预设若缺目录项则测试失败；同时验证精确 model ID、endpoint 隔离与尾部斜杠规范化。此路径不调用供应商，也不要求用户输入窗口值。新增默认模型必须先核实官方容量并更新目录、来源和版本。

- [x] **T03（完成）连接 API 与模型设置界面**
  - 修改：`server/internal/api/model_connections.go`、OpenAPI、`web/portal/src/routes/Connections.tsx`、`web/portal/src/routes/admin/AdminModels.tsx` 和生成类型。默认预设只展示目录值/来源，不显示可编辑窗口输入；未知自定义 endpoint 的可选覆盖放在高级设置。
  - 验收：Portal 定向 Vitest 已通过 39/39、对应 ESLint 已通过。实际隔离 `morde-pc` 中选择 `deepseek-flash` 后显示“上下文窗口由 Morde 自动识别；无需手动填写”，高级设置无窗口输入；保存个人连接后重新读取，详情显示 1,048,576 tokens、来源 `Morde 模型目录`、目录版本 `2026-11-18`。管理员 Web 中选择同一系统预设时显示目录自动识别且不可手工修改，保存后列表读回相同容量、来源和版本。使用无效占位 API key；PC 连接检查失败，管理员检查因未配置调用额度未执行，故只验收模型配置保存/读取和 UI，不代表供应商连接成功。

- [x] **T04（完成，已按 DeepSeek usage 样本校准）把有效容量和 token 压力接入会话摘要**
  - 修改：`server/internal/app/conversation_summaries.go`、`server/internal/worker/conversation_summary_maintenance.go`、`cloud_dsh_compaction.go` 与 Morde DSH coordinator 事件适配。存储页固定 64 KiB、完整序列化摘要输入限制 96 KiB，二者都不再按 token 窗口缩放。新增测量专用 coordinator 分支，调用 DSH 公开 `dsh-token-meter/estimate` 逐来源计量真实摘要提示/头/文本；无模型调用、无检查点。worker 用窗口 token 预算选完整来源组。为抵消 DSH 固定 4 字符启发式对中日韩/结构化内容的低估，估算至少乘二并设置中日韩每字符一个 token 的下限。摘要仍无工具，保留来源序列/CAS、租约和版本规则；不修改 DSH 源码。
  - 验收：Node 合成测试覆盖中日韩、JSON/schema、工具结果和普通文本，并验证测量路径不调用模型/不写 checkpoint；worker 定向测试覆盖 token 触发、独立字节水位和超预算时按完整来源组裁剪，均已通过。独立临时 PostgreSQL 上的长历史 DSH 集成测试也已通过。2026-11-18 用 DeepSeek `deepseek-flash` 实际 usage 校准四类合成消息和完整摘要提示：CJK 的 provider/raw 比值最高 2.276；当前 2 倍与中日韩字符下限的保守值在五组样本中都覆盖 `usage.prompt_tokens`。依据：[DeepSeek Chat Completions 用量字段](https://api-docs.deepseek.com/api/create-chat-completion/)。校准范围不代表其他 provider、代理或长会话，留待 T08。

- [x] **T05（完成）按 DSH 合同启用已知窗口的任务自动压缩**
  - 修改：服务端只向已授权任务传有效容量；`runtime/morde-cloud-runtime/src/engine.mjs` 将窗口交给 `WorkerModel.resolveModel()`，由当前 DSH `TokenMeter` 与 `BasicCompactionEngine` 工作。已知容量时启用自动压缩：阈值 0.8、headroom/近期保留量最多 4096 且不超过窗口 12.5%，摘要输出限制在 256–2048 token；未知容量时 `auto: false`。
  - 验收：runtime 定向测试验证窗口有界传输、未知容量不启用自动压缩及自动压缩恢复；`node --test test/compaction.test.mjs` 5/5 通过。不得修改 DSH 源码。

- [x] **T06（完成）安全处理明确的上下文溢出**
  - 修改：Morde 模型网关将准确识别的上下文超限映射为安全错误码，DSH 执行一次受限恢复；不传递 provider 自由文本，不根据普通 413 推断上限，也不重放结果未结算的工具副作用。
  - 验收：runtime 错误分类与启动帧定向测试通过，检查普通错误文本不能越过 allowlist。真实 provider 超限和运行中任务恢复仍列入 T08 端到端验收。

- [x] **T07（完成）接入 DSH 官方工具结果裁剪插件**
  - 修改：固定依赖 `@deepseek-ai/dsh-compaction-tool-result-pruner@0.2.2-rc.1`（MIT），按 DSH 插件顺序挂载；Morde checkpoint 只接受与原始工具结果相邻、字段和替换内容均能证明的裁剪投影事件。原始结果和操作账本不删除。
  - 验收：`node --test test/compaction.test.mjs` 5/5 通过（包含官方 pruner 保留原始日志、只替换模型投影）；worker checkpoint 裁剪证明、异常事件拒绝和原始账本回执定向测试通过。CJK/结构化结果、恢复后投影及完整原生任务仍需从 T08 完成端到端检查。

- [ ] **T08（待验收）完成端到端压缩能力验收**
  - 修改：补充本节验收记录，不新增第二套 runtime 或通用压缩系统。
  - 验收：本地 DSH Runtime 全量 Node 测试 118/118 通过，覆盖合成模型下的自动压缩、工具结果裁剪、摘要失败/取消回滚、checkpoint 重启恢复、owner/session fence 及上下文溢出 allowlist。当前工作树构建的 Runtime 镜像内测试 117 通过、1 项因 gateway 源码不在镜像构建上下文而跳过；Linux Docker/Guardian 集成 `TestRealDockerAccountRuntimeTaskIsolationAndGuardianCrashRestart` 通过，实际启动当前摘要固定的 Runtime 容器，验证并行 Task 隔离、单 Task 停止、Guardian 崩溃后到期清理与停止证明恢复；其 PostgreSQL manager 和 Portal `POST /api/tasks`/TaskWorker 两个子用例也通过，模型响应为受控 stub。本轮又在一次性 rootful DIND（cgroup v2、host PID/cgroup namespace、独立 Docker socket）中以当前 Runtime 源码构建 arm64 镜像，重新通过该集成用例及临时 PostgreSQL manager、Portal TaskWorker 子用例；依赖模型仍为受控 stub。临时本地 PostgreSQL 上 `TestConversationSummaryDSHMainAndSideChatsContinueAfterLongHistoryRestart` 通过，覆盖主会话、旁聊、长历史摘要续聊与 Runtime 重启。模型设置的 PC 与管理员 Web 保存/读取及系统默认容量无手工输入已在 T03 验收；真实 DeepSeek 网关的短问答、多轮文本、流式回复和工具结果续接也已单独通过，但未经过 Cloud DSH 压缩链路。仍需从实际 `morde-pc` 对话和 Portal 生产启动路径验证真实 provider 长对话压缩、长任务与恢复；摘要隔离和模型连接所有权/容量解析分别有 store/app 测试，checkpoint 新增跨 session/会话读隔离和 owner 身份拒绝检查，但完整两账号/会话矩阵仍需从实际服务流验收。未执行项继续标待验收。

## 当前验收记录

- 官方来源已核对表中五个系统模型；目录覆盖测试通过。服务端测试和迁移集成测试包含旧默认值切换为目录容量、未知自定义路由保留手填值等场景。
- Portal 定向测试通过 39/39、对应 ESLint 通过；本轮 `go test ./internal/worker -count=1`（明确 unset DB 集成测试变量）和 `go test ./internal/app -count=1` 均通过；`node --test --test-concurrency=1 test/*.test.mjs` 全量通过 118/118，Account Runtime smoke 通过。当前源码运行时镜像构建测试 117 通过、1 项跳过；Linux 真实 Docker/Guardian 的 `TestRealDockerAccountRuntimeTaskIsolationAndGuardianCrashRestart` 及其 PostgreSQL manager、Portal TaskWorker 子用例均通过。DSH Runtime 模型响应为合成值，不代表真实 provider 压缩行为。
- 实际隔离 `morde-pc` 与管理员 Web 均已验证系统预设不显示可编辑窗口输入，模型连接保存后读回 Morde 目录容量、来源和版本；PC 无效占位 key 的连接检查失败，管理员因调用额度未配置未发起 provider 检查。未验证真实 provider 连接成功。
- 使用授权的 DeepSeek 测试 key，以合成短文本通过 `TestDeepSeekFlashDialogueThroughMordeModelGateway`；覆盖问答、格式约束、路由决策、多轮上下文、流式回复和工具结果续接。未发送文件；该测试验证网关和 provider 连通，不验证 Cloud DSH 压缩。
- 长任务 DSH `TokenMeter` 已接管实时压力判定；摘要 worker 已用 DSH 公开启发式做保守 token 压力估算，Node/Go 定向测试通过。DeepSeek `deepseek-flash` 实际 `usage.prompt_tokens` 样本已校准；字节水位只与字节上限比较。
- 新建临时、本机回环绑定且使用内存盘的 PostgreSQL 后重新通过 `TestConversationSummaryDSHMainAndSideChatsContinueAfterLongHistoryRestart`（27.46s），覆盖主会话、两个旁聊、每轮摘要提交、8 MiB 原始历史检索及 DSH Runtime 重启恢复；模型响应为合成值。新增的 PostgreSQL checkpoint 测试验证另一 session 读不到已保存 head/page，并拒绝与 route claim owner 不一致的身份。`TestConversationSummaryIsolatedAcrossMainAndSideChats`、`TestConversationSummaryPersistenceLeasesReplayAndInvalidation`、`TestProjectConversationSummaryRequiresCurrentMembership`、`TestPostgresConversationRuntimeIdentityAndIsolation` 和 checkpoint 页序/上下文测试均在同一隔离 DB 上通过。此前 `TestModelContextCapacityMigrationPreservesLegacyValuesAndGuardsDowngrade` 与容量/摘要迁移集成测试通过。摘要回滚测试清理后续路由聚合迁移证据，确保实际命中摘要迁移的防降级保护。临时容器、网络及工作目录已清理。
- 本轮新增 `TestConversationCoordinatorCheckpointsAreIsolatedAcrossConversations`：以两个会话分别保存不同 checkpoint sentinel，验证各自 head/page 只还原本会话内容，并拒绝将另一会话的 route claim 与 checkpoint 身份交叉读取。在新建、内存盘、回环绑定的临时 PostgreSQL 上与原 checkpoint 页序/上下文测试一同通过；测试容器已停止并自动移除。
- 系统默认模型目录要求官方容量来源；目录覆盖测试确保系统预设列表中的每个模型都有记录，因此缺失容量的新增默认模型不能通过覆盖检查。PC 与管理员 Web 保存/读回流程已完成；真实 Docker/Guardian 任务执行及 Portal TaskWorker 路径已通过受控模型 stub 验证，但 Portal 生产进程未以 Cloud DSH 开启状态启动，PC 长对话没有走该 Cloud DSH 路径。真实 provider 压缩、实际 provider 溢出恢复与实际客户端两账号长会话仍未验证，留在 T08。
- 本轮创建了一次性 rootful DIND 和内存盘 PostgreSQL，验证 Linux cgroup v2、独立 Docker socket、当前 Runtime 镜像、Guardian 进程及 Portal TaskWorker 子场景；测试通过后已移除临时容器、volume、镜像和测试 binary。Docker Registry 初始 HTTPS 探测被重置，但镜像拉取最终完成。生产 Portal 路径仍未启动：`deploy/single/portal.yaml` 关闭 Cloud DSH，Portal serve 还要求 Linux `/proc`、独立 Workspace 挂载，以及实际 OpenSandbox/runsc/cloudhost 网络策略检查；本机没有常驻 Linux Portal VM。没有用通过式 guard 绕过策略，也未触碰现有数据库或 PC 测试实例。Cloud DSH 生产 Portal 与真实 `morde-pc` 长会话链路仍是 T08 未完成项。
- 当前 DSH 依赖固定在 `0.2.2-rc.1`；`npm audit --omit=dev` 报告依赖树含 20 项 high、2 项 moderate。工具结果 pruner 的 `0.2.2-rc.4` 要求 DSH 相关 peer 依赖也为 `rc.4`，故本轮没有升级 DSH 版本或改 DSH 源码；依赖安全更新需单独评估。

## 不纳入本轮

- Hermes 的逐轮 micro-compaction、Morde 自研 tokenizer、未知路由通用窗口兜底、对未知容量发起计费探测。
- DSH Runtime 升级、修改或复制 DSH 库代码、另行实现 Agent 循环。
- 在工具结果未知或操作未结算时自动重放副作用请求。
