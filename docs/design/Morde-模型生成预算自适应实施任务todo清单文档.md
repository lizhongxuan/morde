# Morde 模型生成预算自适应实施任务 TODO

**状态（2026-11-28）：预算候选 `663d9250` 已由 `0c906232` 纳入 `release`；B08 长回复请求类型缺陷已修复，并通过定向回归及一次真实 DeepSeek Gateway 长回复调用（4,096 token cap，provider 报告输出 4,096 token，返回 `length` 部分结果）。此前 P0 smoke 8 pass/0 fail/2 预期例外、DSH runtime 302/302、Worker→PostgreSQL→fake HTTP 整链、PostgreSQL 容量恢复及 DSH 来源/校验和检查通过。预算审计记录提交 `84e055f1` 已包含于 `release`。截图所示 worktree 当前均不在 `git worktree list`；分支引用保留，目标提交已在 `release` 或有等价补丁，详见第35节。其他任务的 detached worktree 与工作区改动仍保留，未纳入预算提交。测试 Portal 当前可达，但远端尚不满足完整 v4 离线备份及安全升级前置条件。尚未部署，PC 真实长回复、刷新/重连、取消恢复与生产账单门禁未验收；B01–B09 生产/PC 门禁仍未完成，B10 尚未开始。**
评审依据：《Morde_个人Agent_模型预算与可靠交付实施方案.md》、固定提交的 Morde 源码，以及锁定版本 DSH 的本地包契约和离线实跑。保留原 B01–B10 编号；已实现 DSH opt-in IPC/Worker 协议探针、Task compaction prepare、假 HTTP 集成测试、模型输出能力元数据、阶段策略快照及 PostgreSQL 预算准备账本。Portal 可单独加载显式路由阶段策略，但不会因此启用 Task admission 或 shadow sampling；默认配置没有任何阶段预算值。第25–27节记录旧候选 smoke、00270 迁移失败与本地 schema 修复；第28节记录当前候选本地验证、冲突核查及测试机不可达状态。

**方案复核决策（2026-11-27）：** 采纳方案的 P0–P4 门禁、等待/暂停语义、周期止损与试点质量验收；保留“未知能力不猜、短答不多一次模型往返、历史 Task/Schedule 合同不迁移、未知派发结果继续占用”的边界。B03 影子原型不再作为开始 B04 账本工作的硬依赖；预算策略与 `O_send` 计算仍须在 B04/P1 试点启用前通过影子核验。方案中的 `4,000` 仅是反例数值，拟议 API 字段不代表现有接口；均不写成默认额度或既有合同。


**附件方案复核决策（2026-11-28）：** 采纳“去掉普通 Task 固定 2,048 输出上限，但不等于每次请求模型最大值”的方向：进入签约自适应路径后按 `O_send=min(O_goal,O_safe)` 派发，且以模型能力、完整上下文、账户/Task/项目额度和协议资源边界取最小值；可靠数据不足时不设通用新默认。发布上保留旧路径和硬止损，只有 B01–B05 门禁通过的试点才放开高 cap。采纳长回复留在原会话、短答不增加分类模型往返、持久化分块及有边界的截断处理；过滤、取消、超时、未知用量和不完整工具参数不得当作文本截断。B07 的自适应 Task 可按原计划最多续接两次；B08 无 Task 的显式长回复先限定为一次、最长 4 分钟的模型调用，`length` 时持久化并展示部分结果，不自动再开调用。这样可用单次账本预留及总时长约束回答，同时避免在没有持久化“每条回复累计预算/调用数”合同前自动叠加新预留；如要给无 Task 回复增加自动续写，先补齐该合同、幂等身份和恢复验证。按次/滚动周期止损和产品默认值继续放到 B10，根据试点数据另行决策，不改变旧 Task/Schedule 合同。附件的 `4,000` 是用于说明并发 trade-off 的反例，不能据此设默认预算；离线演算证明特定假设下的逻辑，不证明生产性能或质量。

**本次复核《Morde 个人 Agent：模型预算与可靠交付实施方案》（2026-11-28）：** 附件作为建议与证据，不视为自动执行指令。采纳三条交付路径：短答/状态控制走快路径、显式长回复回原会话、长任务进入持久化 Task；不增加每条消息的分类模型往返。`O_safe` 表示模型/上下文/协议/适用预算共同允许的上限，可信阶段策略给出 `O_goal/O_min`，本次请求按需预留并令 `O_send=min(O_goal,O_safe)`；低于最低值或无法安全计量时不派发。`stage_unknown` 不从 prompt 猜测或据此扩大预算。B08 无 Task 长回复维持单次、最长 4 分钟，不自动续接；后续如增加自动续接，须先建立回复级累计预算/调用数、稳定身份、结算与恢复合同。Task 50,000 默认及历史 Task/Schedule 合同不变；按次/滚动周期预算和新默认留到 B10，待 B09 真实试点数据及产品评审后再决定。B09 将每类至少 30 个固定样本、至少一个 PC 可见输出超过 2,048 模型 token、p95 首字时间相对基线不劣化超过 10%（噪声过大则扩样）、双子 Agent 并发及暂停恢复且无重复副作用作为待验证门槛，不记作结果；Fake provider/离线演算不替代真实模型和 PC 证据。

## 1. 决策摘要

1. 目标是移除自适应 Task 路径上普通生成固定为 2,048 token 的限制，并核对网关/provider 的 32,768 上限；不在生产门禁前全局改旧路径。只有能力、预算账本与长回复传输验证通过的签约路径才可提高 cap。单纯把上限改大仍会因预算过度预留而拒绝或阻塞并发。
2. 每次调用分开计算：
   - `O_safe`：模型能力、上下文余量、适用 token/金额额度和协议边界允许的最高值。
   - `O_goal` / `O_min`：服务端按模型及执行阶段设置的目标和最低可派发值；短答、工具步骤、子任务、汇总、显式长文及续接可使用不同策略。
   - `O_send = min(O_goal, O_safe)`：本次唯一实际发送和预留的输出 token 数。低于 `O_min` 时不派发。
3. 不把“允许的最大值”当成每个请求都应预留的值。按阶段分配，减少一个大请求独占额度而压住其它并发步骤的情况；可靠数据不足时先做影子计算和试点测量，不猜固定的普遍新上限。
4. 近期保留当前 Task/Schedule 的预算合同、普通 Task 的 50,000 服务端默认、已有 Schedule 传入的明确额度和历史记录，不把 `model_tokens` 改成 `null`，也不迁移旧任务。Task 累计限额继续作为每个调用上限以外的止损。是否更改新任务默认或增加按次/按周期预算，待试点数据支持后单独决策。
5. 保留简单问答、状态查询与控制的快路径。显式长文走原会话长回复路径；正常短答不新增“先分类、再生成”的模型往返。首期只做显式入口，不能把这个保证误写成自动识别全部长问句；后台多步骤工作仍持久化在原 Task 中。短回复、长回复、后台 Task 的预算策略分别验证。
6. BYOK 不默认无限。未知供应商费率时不承诺金额上限或“免费”，保持既有 Task 的 token、执行时长和工具限额；显式对话长回复没有 Task 时必须单独实现有限执行上限（B08），独立调用次数与周期规则的全入口统一留到 B10。也不自动更换模型、扩展权限或新增 Agent Runtime。

## 2. 计算与账本合同

每个适用预算维度独立换算和预留，不混用 token 与金额单位。对窗口型端点，建议按适配器合同计算：

`O_safe = max(0, min(
  M_model,
  W - I_ctx - reserve_margin,
  每个适用 token 额度的剩余值 - I_bill,
  每个适用金额额度可承受的输出值,
  协议与资源边界
))`

`O_send = min(O_goal, O_safe)`；在现有输入加输出 token 口径下，`reserved_tokens = I_bill + O_send`。金额账本则单独保存 `reserved_micros`，按锁定价格、输入和 `O_send` 计算。不同计量口径的额度要单独换算。

- `I_ctx` 代表完整请求的上下文占用上界；`I_bill` 是现有 token 计费口径下可证明的输入预留。两者按 provider/endpoint 分别计算，覆盖 system、工具 schema、压缩后的历史、图片和协议开销。无法取得可信上界时，拒绝进入新增的自适应路径。
- 项目费用按固定的价格版本、整数最小货币单位及保守适用费率计算；不能用浮点舍入，也不能把金额直接换成 token。价格或计费边界未知时，禁用声称有金额硬上限的路径。
- 短事务内检查并预留账户、Task、项目、单次和适用周期额度。已有调用账本负责幂等和结算；提交事务后才请求模型。输出预留必须等于实际发送的 `O_send`；总 token 预留另含输入。请求摘要是规范化输入的散列，不把 prompt 正文、API key 或推理内容写入观测日志。
- 已结算值和所有可能已派发但未结算的占用继续计入预算。平台账户未知用量仍按现有规则阻断；Task 未知时现有 `usage_model_tokens` 可为 SQL NULL，严禁 `COALESCE(...,0)` 后继续发送。不要把确定性算术通过误写成可拦住供应商自身超额账单。只有可证明从未派发才能释放；网络中断或超时无法证明模型没收到请求，必须保留未知占用并对账。超出预留的 provider 账单需记录差额、冻结受影响路径并调查，不能截断数字掩盖。
- 若并发预留暂时占满额度，进入可恢复的 `waiting_capacity`；确定额度不足则进入 `paused_budget`。能力/计量未知则进入 `blocked_model_config`。三者需有明确的服务端原因与用户提示，不无限等待、不新建 Task 绕过额度。等待队列须有截止时间；等待者不占预算或 worker 槽，额度释放后重新计算并唤醒；不要求为这些语义机械新增数据库状态，须说明与现有状态的映射，并验证跨任务下小请求没有明显饿死。
- GPT-6 Luna 的 max 推理目前可把值抬到 25,000；代码在最终校验/预留前调整，不能把它描述为现有“预留后越权”。自适应路径须在签约前统一这个策略，避免网关的实际值和 DSH header 分叉。用户选择的推理档位无法在当前 `O_safe` 内满足时，说明并等待/暂停；不得静默降低推理设置。

## 3. 路由、恢复与交付

- **短答和状态控制：**保留当前快速路由；短答不得因为长文支持而多走一次模型往返。查询或取消使用现有确定性接口。
- **显式长回复：**正文经现有网关与账本生成，回到原会话；分类器的结构化小输出不承载长文正文。优先复用现有可辨识的用户交付形式；若缺少入口，再在 PC 增加明确的长回复入口。不得把后台电脑、工具 Task 或第二套循环引入纯文本长回复。
- **后台 Task：**延续原任务 ID、checkpoint、会话及原授权边界；恢复若按现有规则推进 generation，旧 generation 仍要被隔离，不能覆盖新结果。等待预算释放或获准加额后不重跑已完成步骤。预算获批不等于工具权限或外部操作获批。
- **截断恢复：**只有 provider 明确报告输出 token 上限截断且调用结果可核对时，才考虑纯文本续接，最多两次，每次使用独立调用身份和账本结算。保存已确认内容；内容过滤、取消、超时和未知用量不算截断。工具参数不完整时绝不执行；外部操作回执未知时先查证，不重放有副作用操作。
- **传输：**延长生成超时前先区分连接、首包、空闲、总时长和取消。增加正文长度时采用可恢复的有界分块、持久化序号和慢消费者反压；先证明断线续读、部分交付和取消，不将内存、单事件或总消息边界设为无限。
- **定时任务预算：**按次与滚动周期预算属于后续阶段。若进入该阶段，单次触发的重试和子 Agent 共用额度；滚动周期内在途预留不能因跨窗而消失；超限暂停，不积压并在恢复后补跑。老 Schedule 保留原数值。

## 4. 接口和改动边界

现有接口以 `server/openapi/portal.yaml` 为准。本期保留 `TaskBudget`、`ScheduleBudget`、任务创建请求和现有数据库数值合同；**不实现**先前草案中的 Task token 上限 nullable/`null` 改造。

- 模型能力：后续拟在现有 `GET/POST /api/model-connections`、`GET/PUT /api/model-connections/{id}` 增加 provider/endpoint/model 对应的有效最大输出、覆盖值及来源。能力变更时失效旧快照；权限、credential 加密和脱敏沿用现有实现。未知能力只阻止新增的自适应试点，不能悄悄令未入试点的既有对话失效。
- 调用账本：扩展现有 model-call 与项目预算账本，保存 capability/价格版本、阶段策略版本、`O_goal/O_min/O_send`、请求摘要、输入预留、调用身份及 provider 用量；同一身份参数变化时冲突，续接作为新调用记录。
- 用户可见状态：复用现有 Task、会话消息、进度、失败和审批机制；如新增原因码，更新 OpenAPI、PC/iOS 类型与展示。预算等待和暂停不赋予任何新授权。
- 发布方式：DSH 接口门禁通过前，不改实际调用的发送上限。功能落地后分开控制预算策略和长回复传输；先影子计算，再限路由/用户试点，逐步放量。回滚停止创建新策略，但在途请求、已保存正文和未知账单仍按原账本对账。

## 5. 实施任务

- [ ] **B01（进行中：本地真实 DSH→PG ledger→Gateway→fake HTTP 重复压缩整链与默认关闭的 Portal bootstrap/路由门控已验证；生产/真实模型及恢复边界待验）验证 DSH 逐次输出控制**
  - 代码现状：Morde 锁定 `0.2.2-rc.1`；普通 Task `start.max_tokens` 带入 `AgentOptions.maxTokens`，Hermes 压缩会读 `requestHeader().config.maxTokens`；适配器目前只给窗口，不给动态 `defaultMaxTokens`。已弃用的 `agent/request` 不新增；`agent/pre-step` 能改消息/拒绝步骤，但不是直接覆写请求上限的入口。[锁定版本](https://github.com/lizhongxuan/morde/blob/936389c24ea61db45eded881423cef74d8815ca8/runtime/morde-cloud-runtime/package.json#L13-L29) · [普通 Task start](https://github.com/lizhongxuan/morde/blob/936389c24ea61db45eded881423cef74d8815ca8/server/internal/worker/cloud_dsh_session_tree.go#L2408-L2425) · [Agent 创建](https://github.com/lizhongxuan/morde/blob/936389c24ea61db45eded881423cef74d8815ca8/runtime/morde-cloud-runtime/src/engine.mjs#L1746-L1755) · [压缩预算](https://github.com/lizhongxuan/morde/blob/936389c24ea61db45eded881423cef74d8815ca8/runtime/morde-cloud-runtime/src/engine.mjs#L1598-L1619) · [适配器](https://github.com/lizhongxuan/morde/blob/936389c24ea61db45eded881423cef74d8815ca8/runtime/morde-cloud-runtime/src/engine.mjs#L2204-L2267)。
  - **已实测的候选方案，不是整链通过：** 锁定 DSH 可由 Agent 作用域 `agent/pre-step` 取得 `agent`、turn/step、输入消息和 `AbortSignal`；同一步的 `LlmAdapter.resolveModel(provider, model, signal)` 收到同一个 signal，可用 `WeakMap<AbortSignal, plan>` 将每步计划交给适配器 `defaultMaxTokens`。本地 fake adapter 复现了同一 Agent 的 **512→1,024**，并让两个同模型 Agent 并发获得 **768 / 3,072**，header 和 stream options 都与各自计划一致；未使用已弃用的 `agent/request`。显式 `AgentOptions.maxTokens=2,048` 仍固定两处为 **2,048**，所以生产试点必须只在服务端开关授权的路径省略显式 cap，并在计划缺失时拒绝入模。此接口组合提供了逐 step 传值的候选通道，但不会自动计算可信 `O_send`。
  - **DSH 准入钩子：** 上游锁定版本没有此 API；在 `@deepseek-ai/dsh-agent-loop@0.2.2-rc.1` 的 header 冻结前增加自有 `agent/request-budget` waterfall hook。钩子看到 Agent、turn/step、signal、最终投影的 config/messages/tools/toolHistory；返回 `{maxTokens}` 后重新 `prepareCall()`，并以同一预览提交 tool registry message，保证 DSH header、adapter stream options 和输入预览一致。无监听器或返回 `undefined` 保持原请求路径。补丁脚本校验 DSH 版本、源/目标 SHA-256、MIT LICENSE，以及其私有 hook registry 所依赖的 Cordis `4.1.0` 结构；在 `build:dsh-compaction` 和 Docker 测试构建中应用。此为 Morde 仓库针对锁定依赖的最小补丁，不是上游受支持插件。
  - **本轮新增的 P0 Morde 协议探针：** Worker 只在注入 `ModelBudgetPlanner` 时发送 opt-in admission；默认仍发送旧 `max_tokens`。runtime 在 AgentLoop hook 中基于 DSH/LLM 实际 provider 投影、已持久化的基线 checkpoint 和独立 call ID 发送 `model_budget_prepare`；Go 校验 Task/owner/Agent/generation/session/through/turn/step、工具授权和规范请求摘要，再返回短时、单次、会话绑定的 plan。最终 model frame 必须回显 plan/call/digest，输入投影与 cap 必须一致，否则拒绝入网关。实际 DSH hook 会去除 LLM 请求投影中的 developer/tool registry 内容；预算摘要投影已同步此行为，并覆盖恢复后工具增删。`AccountRuntime` 只允许该字段出现在 Task start。Portal 有显式默认关闭的 planner bootstrap，部署配置仍为 false；逐 Task 资格要求模型 endpoint 解析成功、输出/上下文能力来源可信且 exact route 存在于策略表，不匹配的 Task 保持 legacy cap。
  - **本地 fake HTTP 子门禁：通过。** Go Worker → Morde runtime hook/IPC → Go plan seam → `ModelGateway` → `httptest` 假 provider 验证真实序列化的 `max_output_tokens=3,072`，同时检查 prepare/final 请求摘要与身份一致、未绕过网关走 fallback。此测试里的 planner 只返回合成计划，不执行额度锁定或账本预留；不代表真实模型能力、用户余额或生产计费已验证。
  - **本地整链与失败门禁：** 单次本地 DSH runtime IPC、Worker planner adapter、PostgreSQL prepare/ledger、`ModelGateway` 和 localhost fake HTTP 链由 `TestCloudDSHModelBudgetPostgresHTTPUsesOneSignedCapAndSettlesOnce` 验证：prepare 绑定 cap-free 请求摘要，账本调用 ID（prepare through）与最终 DSH frame ID（final through）分离，HTTP 收到 `max_output_tokens=3,072`，usage `8/6` 入账，settled replay 后 HTTP 仍仅一次。`TestCloudDSHModelBudgetPostgresHTTPUsesTaskHermesCompactionSummaryStage` 本轮通过：同一真实 Node DSH Task 在 100 条运行中 follow-up 下发生至少两次 Hermes 压缩；每次 `purpose=compaction` 都由 PostgreSQL `summary` stage 计划、预留并结算，DSH/final ID、budget call ID、cap 和 fake HTTP provider 输出 cap 相符，用量 `8/6` 落账。`TestCloudDSHModelBudgetPostgresHTTPFailureEdges` 覆盖预派发取消安全重试、timeout/503、派发后 Task cancel 和 no-replay；其中新增 dispatch transaction 故障注入通过 `Gateway.Complete` 验证失败后 HTTP=0、prepared hold 与 plan 未消费，移除故障后同身份重试并只派发一次。PG crash 测试覆盖 Worker/Node 子进程崩溃；另有 §40 记录的 Linux 隔离 PostgreSQL + 实际 Portal 二进制进程崩溃/重启/unknown no-replay 子门禁通过。另新增 GPT-6 Luna adaptive 用例，证明 25,000 reasoning minimum 与计划、Provider cap、账本 reservation 一致，目标或已验证模型 cap 不足时 prepare 前拒绝。同一 Task 的两个真实 DSH child Agent 已通过并行 runtime 测试，分别取得 cap/plan/call identity 并各自完成 `child_settle`；这尚不验证两个 child 调用同时写入真实 PG ledger。生产 Portal 未启用 planner，真实 provider 能力/质量和计费也未验；B01 不勾选。`model_call_ledger` 保存总 token reservation，项目账本保存 input/output 与金额分项；预算请求摘要绑定最终 `max_tokens`。
  - **上游对照：** 检查了官方 `dsh-v0.2.2-rc.5` 和 `dsh-v0.2.3-alpha.2` 的 AgentLoop 源码，仍从 `AgentOptions.maxTokens`/已记录 header 取得 cap，并在写 header 前调用 `prepareCall()`；未发现替代的 per-request budget hook。[rc.5 的请求准备与 header 顺序](https://github.com/DeepSeek-ai/deepseek-harness/blob/dsh-v0.2.2-rc.5/packages/core/agent-loop/src/agent.ts#L590-L657) · [alpha.2 对应实现](https://github.com/DeepSeek-ai/deepseek-harness/blob/dsh-v0.2.3-alpha.2/packages/core/agent-loop/src/agent.ts#L575-L650)。alpha.2 还是预发布版本，并要求 Node.js 26.13.1 及以上；当前本地是 26.13.0，单纯升级并不适合且没有显示能解决该接口缺口。[官方发布记录](https://github.com/DeepSeek-ai/deepseek-harness/releases/tag/dsh-v0.2.3-alpha.2)。
  - **压缩路径：** 已为 opt-in Task 的 Hermes/Morde compaction `ctx.llm.stream()` 单独接入 prepare；计划绑定最终文件/tool 投影、当前 durable `through_seq`、turn/step、随机单次 call ID 和取消信号。失败的上下文溢出 attempt 先持久化，再为降级重试取得新计划。Coordinator compaction 和预算关闭路径保持旧行为。本地真实 DSH、PostgreSQL ledger 和 fake HTTP provider 的重复压缩整链已通过；生产 Portal planner 仍关闭，不能据此开放生产 admission。
  - **生产链与否决条件：** 本地 DSH→账本→Gateway→fake HTTP 整链已证明 `header == ipc.max_tokens == ledger.output_reserved == HTTP max_output_tokens`，包括账本/最终 DSH 身份分离和 settled replay 单次派发。`runAgent()` 普通模型步仍仅在显式注入 planner 时启用；Portal bootstrap 受 `output_budget_admission_enabled` 控制，部署值为 false，实际请求 cap 未改变。普通/压缩的取消、重启、崩溃和未知派发一致性、生产策略加载、真实 provider 用量及 PC 预算状态仍待验；因此 B01 不勾选。没有调用付费模型或测试服务器。
  - 当次验证：核对锁定 DSH 组件版本/integrity；默认值探针（3/3）与 request-budget hook（5/5）通过；runtime/Go 双向协议、plan 绑定和假 HTTP 网关测试见第 8 节。

- [ ] **B02（实现与本地自动验证完成；PC 真实服务端数据端到端读写待验）建立可信模型能力与试点目录**
  - 位置：模型目录、`ModelConnection` 存储/API、OpenAPI、管理后台和 PC 的模型连接入口。
  - 实施：已为最大输出补齐有效值、个人自定义路由覆盖、来源、解析时间、目录版本和 HTTPS 来源 URL；未知时返回 unknown/null，不依据模型名猜测。精确官方目录目前只收录 GPT-6 Luna、GPT-6.1 Sol、GPT-6 Astra 与 DeepSeek Flash；DeepSeek 官方 [Chat Completions API](https://api-docs.deepseek.com/api/create-chat-completion/) 规定 `max_tokens` 范围为 `1..393216`，数据库与代码目录使用上限 `393,216` 并保存该参数文档来源。自定义覆盖仅允许个人自有路由，后台共享路由保持官方目录或 unknown；系统预设和服务端认证模型不接受手工覆盖。模型连接 view、Gateway、Probe 和 Media endpoint 共用 freshness 检查，model-catalog 值若不匹配精确路由、目录值或版本会被清为 unknown；该能力元数据本身不改变 32,768 网关发送上限。
  - 验收：服务端精确目录、迁移、列表/详情、过期目录失效与个人覆盖范围由 Go/API/PG 用例覆盖；个人模型与后台模型页均显示有效值/来源/版本/来源链接或 unknown。个人自定义入口支持省略字段、显式覆盖和 `null` 清除；共享后台表单不再发送服务端拒绝的覆盖值。OpenAPI 将新增响应字段标记为可选，以兼容滚动升级中仍未返回该字段的旧服务器。
  - 来源依据：[OpenAI GPT-6 Luna](https://developers.openai.com/api/docs/models/gpt-6-luna)、[GPT-6.1 Sol](https://developers.openai.com/api/docs/models/gpt-6.1-sol)、[GPT-6 Astra](https://developers.openai.com/api/docs/models/gpt-6-astra)、[DeepSeek Models & Pricing](https://api-docs.deepseek.com/quick_start/pricing/)。未知的 GLM/custom 路由不设置输出上限。以上值只是能力元数据，不证明网关可发送到该值，也不代表价格/计费安全门槛已通过。
  - PC 实测：此前用 `make pc` 操作当前源码 `morde-pc`，打开“设置 → 模型与工具”，确认详情新增“单次生成上限”和来源；此前 PC 显示 unknown 展示有效，但未得到服务端新目录值。2026-11-28 再次运行 `make pc` 构建成功，实际客户端提示“已登录，但云端会话暂时无法续期”；HTTP(S) 探测经本机代理建立 CONNECT 后目标超时，未向服务端提交登录或业务请求。本轮未修改远端数据、凭据或数据库。个人自定义覆盖保存流程由 Portal 交互测试验证；真实 PC+新后端数据端到端读写仍未覆盖，因此本项暂不勾选。

- [ ] **B03（进行中：纯计算、只读影子快照与默认关闭的 trace/metrics 已实现；生产样本和离线对照未完成）实现分阶段额度策略与影子计算**
  - 位置：模型调用计划生成器、服务端配置、路由阶段识别及现有预算读取接口。
  - 实施：为已验证模型按 `root_text_only`（根 Agent 无工具的结构阶段，不代表用户要求短答）、工具步骤、子任务、汇总、显式长文与纯文本续接设置版本化 `O_goal` / `O_min`；实现第 2 节 `O_safe` 计算。首轮仅记录预算计划及其原因，不改真实发送值；不设一个对所有模型/阶段通用的替代常量。短答继续使用快路径，但当前 Task 元数据不能据此识别语义上的短答。
  - 验收：用同一批固定请求验证上下文、模型上限、各预算维度及最小额度的取最小结果；能力未知、输入上界缺失、余额不足、整数边界时给出稳定原因。影子输出能解释各项如何限制 `O_send`，并与离线参考结果对照。该影子阶段不得改真实发送值；它不是开始账本实现的阻塞依赖，但其结果必须在 P1 试点启用前核验通过。
  - 本轮实现：[纯函数计算器](../../server/internal/app/model_output_budget_plan.go)计算上限和限制来源；新增精确 HTTPS 路由绑定、明确版本号且必须配置六个阶段的不可变策略快照；`PrepareTaskModelBudget` 把精确模型能力、上下文余量、输入预留、阶段 `O_goal/O_min` 与协议边界带入计划，并由存储事务计算额度后的 `O_send`。新增固定种子 5,000 组输入的独立公式差分测试通过。另新增 `models.output_budget_shadow` 配置（默认关闭，启用时要求精确 HTTPS route、完整阶段策略和值域），Portal 只在启用时安装采样 observer；Task Gateway 通过 PostgreSQL read-only repeatable-read 快照读取账户/Task/项目已用额与在途预留，不创建/释放 hold、不改 Task、不阻断调用。trace 保存 route fingerprint/策略版本/预算值/原因及实际 output usage，metrics 只使用有限 stage/outcome/reason/failure 标签，route/version 不作 Prometheus 标签。shadow 协议/resource ceiling 与 adaptive 协议上限一致，并新增 >32,768 的候选计划用例；Portal 指标保留 `content_filtered` 失败类别。Gateway 回归证明开启影子计算不改变现行 provider cap、reservation、dispatch 或结果；现已可按根 Agent 无工具的可信结构标记 `root_text_only`，不声称语义上的短答或长答，也不从 prompt 猜阶段。summary、long_form、text_continuation 等阶段仍缺 Task 侧可信元数据。代码及本地 app 测试通过，但生产配置保持关闭，尚无线上样本、完整六阶段可信信号、固定请求流量对照和发布前核验，故 B03 不勾选。

- [ ] **B04（进行中：本地 PostgreSQL、Worker/DSH 子进程 crash、动态重算、FIFO 槽 claim、跨 owner 共享项目并发与有限轮次压力公平性、后台 overage 核对子门禁通过；生产门禁仍待验）把 `O_send` 原子预留到现有账本**
  - 位置：`server/internal/app/model_gateway*.go`、`server/internal/app/model_budget_preparation.go`、`server/internal/app/model_budget_capacity_wait.go`、`server/internal/store/postgres/model_usage.go`、`server/internal/store/postgres/model_budget_preparation.go`、`server/internal/store/postgres/model_budget_overage.go`、`server/internal/store/postgres/model_budget_capacity_wait.go` 与 migrations/集成测试。
  - 实施：把 B03 的纯计算器/策略快照纳入预算计划链，完成阶段化 `O_goal/O_min` 与 `O_safe/O_send`，先仅影子核验；同一短事务按固定锁序读取账户、Task、项目及后续引入的单次/周期额度，计算唯一值并保存输入/输出/费用预留和请求身份；事务提交后才派发，不能持数据库锁等模型响应。沿用普通 Task 当前默认和 Task/Schedule 整数合同，不迁移历史数据、不引入 nullable `budget.model_tokens`。验证共享账户和跨项目调用的锁顺序不产生旁路。账本语义须可区分已预留、已派发、已结算、结果未知和确定未发送；可映射现有状态，不必为名称机械增加迁移。保留 Task identity、请求摘要和授权代际；崩溃无法证明未送出时不得重放或释放未知占用。等待队列须有期限、额度释放后唤醒，等待者不占额度或 worker 槽；检查小请求饥饿。Scope 只扩试点路径，现有 `route/compaction/review/terminal_delivery` 专项合同不顺便改。
  - 验收：加入真实 PostgreSQL 并发竞争、同键重入、提交前后中断、等待期限/释放唤醒、结算差额、超预留、费率舍入及个人模型用例；由假端点计数验证：同一调用不重复派发、明确未发送可安全恢复、未知结果不自动再次派发。现有 `model_usage_recovery_integration_test.go` 作为基线；核对已用＋待结算不会放行超硬上限的本地预留。供应商返回超过占用的实际账单另验冻结、对账和如实记账。
  - **已实现和已验证的子集：** migration 00265 保存 Task fence、阶段策略/能力快照、输入与输出预留、单次调用身份和短期 prepare；单个 PostgreSQL 事务按账户、项目/成员、Task、项目预算及账本顺序计算并写入预留。prepare 可幂等重入；派发时重新校验 Task fence、请求摘要、能力/策略版本与过期时间，并原子消费单次计划；已知用量通过现有账本结算。周期清理通过每分钟 Portal 清理循环释放确定未派发且已过期的 hold，过期比较使用数据库时钟。项目费率按整数微单位向上计费，账户/Task/项目竞争不会超过本地 token 与费用额度。migration 00266 的超额对账表保存精确 token 差额及项目金额差额；已知用量超出签约输入/输出时冻结账户与项目账本为 `unknown`，并令 Task 用量变为未知。
  - **当前 PostgreSQL 行为测试：** 覆盖同键 prepare 回放、不同摘要冲突、能力/策略快照改变后释放未派发旧 hold 并重算、过期计划不能派发且有效计划不会被提前清理、账户与同一项目的并发竞争仅允许一笔成功、费用舍入边界、已知结算、个人路径超额、项目路径 token/金额差额冻结、确定未派发时原子释放账户/项目 hold、stale dispatch 变成 `unknown` 后不再派发/清理且新 prepare fail closed，以及 00265/00266 schema 约束和有记录时拒绝回滚。
  - **本轮新增并通过的容量等待子门禁：** PostgreSQL migration 00269 保存等待队列，00270 增加按 owner + `BudgetCallID` 持久化的首次等待时刻和 deadline。仅对 `reason=model_concurrency_limit, stage=reservation` 持久排队；原子设置 `waiting_user + model_budget_capacity`、清除 lease 并保存 Task/Agent/调用/请求摘要/checkpoint fence。deadline 从首次等待起由数据库时钟创建，单次 BudgetCall 最多 5 分钟；等待行被 requeue 删除后，deadline marker 保留，恢复后的同一调用再次等待不会续期。成功 prepare（新建或 replay-existing）会清除 marker；inactive Task 的 marker 由 reconciler 清理。容量事件使用 `task.status`，不触发无用户操作的 `attention_required` 通知。周期 reconciler 在确认同一旧 Agent 的 stop receipt 与 checkpoint 后，重新检查账户/Task/项目最小预算；额度释放时重排队到既有 claim loop，硬额度不足、期限到期或权限/模型/恢复身份失效时安全暂停。尚无 stop receipt 时只延后处理，不唤醒或暂停仍可能运行的旧 DSH。恢复 Task 的 DSH frame 和账本身份保持分离，并原子更新 recovery schedule/event sequence。
  - **容量等待证据：** 定向 PostgreSQL 用例覆盖 lease 清理与幂等入队、无 stop receipt 时不唤醒、active reservation 阻塞后释放并重排队、现有 claim loop 领取、单次 5 分钟期限、同一 `BudgetCallID` 跨 requeue/re-wait 保留首次 deadline、不同调用各自计时、过期调用必须取得当前 Agent stop receipt 后才暂停、00269 waiter 升级到 00270 时保留原 expiry、prepare 新建与 replay-existing 后清 marker、账户/Task/项目硬预算暂停、授权/普通及 recovery checkpoint 失效、审批/澄清等待隔离、双 Store 并发只唤醒一次，以及同一 owner 暂时放不下的大 waiter defer 后下一轮小 waiter 可先继续。新增 Task status/attention 文案与自动重算当前预算的状态由 Go/API 和 Portal 用例覆盖。migration 00272 增加 `slot_reserved_at`：有效 waiter 入队时持久声明下一空闲账户并发槽；新的普通/Task prepare 都将最老有效 claim 计入并发数，reconciler 重排队后 claim 保留，只有该 waiter 成功 prepare 才删除 claim。等待已过期的调用不再占槽，但仍等当前 Agent stop receipt 后走既有安全暂停。公平性 PG 用例在 `max_concurrent=1` 的真实 Store prepare 路径验证结算后新 Task 无法插队、waiter 重排队并消费保留槽、prepare 后 claim 清理且后续 Task 可获槽。另新增 `TestPostgresSharedProjectCapacityWaitMakesProgressUnderOtherOwnerTraffic`：同一硬限额项目由两个 owner 共享，A 连续完成 5 轮项目调用并逐轮 settle/reconcile 时，B 的容量 waiter 保留原 first_wait/deadline，随后在 deadline 前按当前预算重算并成功 prepare/结算；断言项目 actual+reserved 不超限、无 unknown、owner/Task 归属正确。隔离 PostgreSQL 定向用例 `go test ./internal/store/postgres -run '^TestPostgresSharedProjectCapacityWaitMakesProgressUnderOtherOwnerTraffic$' -count=3 -v -timeout=5m` 3/3 通过。动态 PG 用例验证同一调用的策略版本/项目费率变更会丢弃旧计划并按当前费率重算，也验证容量唤醒后按当前项目费率重新定价。deadline 仍只限制单个 `BudgetCallID`，不限制整个 Task 的总等待时长；这些定向场景没有替代多 owner 长期压力测试。
  - **进程崩溃/未知派发证据：** Worker helper 运行真实锁定 DSH runtime、Worker planner、PostgreSQL、ModelGateway，并在 `prepared→dispatched` 提交后进入阻塞型 fake ModelClient。父测试对 Worker/Node 进程组发 `SIGKILL`，确认 fake HTTP 收到 0 次；把账本时间推进到 stale 阈值后调用真实 `MarkStaleModelCallsUnknown`，ledger 变为 `unknown`、Task usage 变为 NULL；重新构造 Gateway 用相同身份回放被拒绝，HTTP 仍为 0。它验证了本地 Worker+DSH 子进程崩溃恢复，不等于对运行中的 Portal 二进制做 kill，也不说明真实 provider 是否已收到请求或计费。
  - **仍未完成验收：** 已有 PG trigger 用例证明 `prepared→dispatched` 失败会回滚 store hold；新增跨层 `Gateway.Complete` 故障注入进一步断言 provider HTTP 为 0、prepared hold/plan 未消费，移除故障后同身份重试只 dispatch 一次。settled replay 不重复派发、transport timeout/503 变为 unknown 并拒绝 replay、预派发取消安全重试、派发后 Task cancel 不重发，均已有 Worker/DSH→PG→Gateway→fake HTTP 用例。新增跨 owner 共享项目额度用例覆盖同项目并发预留、结算与超额的 account/project/Task 归属；尚未覆盖所有个人模型和复杂跨 owner 多项目组合；新增有限轮次跨 owner 压力用例不等于长期或高并发公平压力测试。人工/后台 overage 核对闭环已在本地实现并有定向 PG、API、UI 验证；尚缺部署后对实际供应商账单流程及生产 admission 的真实 provider 用量/计费核对。Portal planner bootstrap 已接入但部署关闭，B04 不勾选；不得把这些本地证明扩写成生产计费或生产 Portal crash 验收。

- [ ] **B05（进行中：本地签约 cap 高于 32,768 的 DSH→PG→Gateway→fake HTTP 已通过；默认关闭的 Portal planner bootstrap、exact-route 资格门控与目录 freshness 检查已验证；真实模型、PC 长回复及发布门禁待验，依赖 B01、B02、B04）将签约的上限接入 DSH 并清除固定天花板**
  - 位置：`server/cmd/portal/main.go`、`server/internal/config/config.go`、`server/internal/worker/cloud_dsh_session_tree.go`、`server/internal/app/model_connections.go`、`server/internal/app/model_budget_preparation.go`、`server/internal/app/model_gateway_dsh.go`、`server/internal/app/model_gateway.go`、`server/internal/model/responses.go`、`runtime/morde-cloud-runtime/src/engine.mjs`。
  - 实施：仅在 B01 接入合同成立后，让 DSH request header、压缩、provider 请求、账本都使用同一个签约值。仅为能力明确、通过试点的 Task 路径移除固定 2,048；对 32,768 网关/Responses 公共校验做来源和路由分界，只有已认证端点、预算与传输同时通过时才允许更高值。非试点、控制和摘要路径保持原限额。不要把 2,048 当上下文大小，也不要把运行时 start frame 的协议上限当模型规格。Luna reasoning、复用/恢复请求及子 Agent 不得越过签约值。
  - **本轮 Portal 接线：** 新增 `models.output_budget_admission_enabled`，默认 false，单机部署文件显式为 false。启用时 Portal 安装版本化策略并注入 Worker planner；Worker 启动 DSH 前检查当前 Task 的 exact model route、已验证的输出/上下文能力来源，策略未列出的模型继续 legacy `max_tokens` 路径。共享 endpoint resolver 现在按精确路由、目录值与目录版本校验 model-catalog 输出和上下文能力，并在过期或不匹配时清为空值；ModelGateway 资格与 prepare 因而都 fail closed。能力解析错误会在 DSH 启动前 fail closed。另为 B08 增加了独立策略 bootstrap：即使 Task admission/shadow 关闭，只要提供显式版本与完整 exact routes/stages，Portal 也加载同一策略快照，但不会注入 Task planner、开启 shadow 或设置默认预算。当前部署 YAML 没有策略 routes，因此长回复策略仍默认不可用；不代表生产已开放或 B01–B04 已验收。
  - 验收：不同输入长度和余额下实际单次额度随策略改变；压缩规划、账本与 fake HTTP endpoint 字段逐次一致；覆盖主/子并发、恢复、取消及路由改变。

- [ ] **B06（进行中：本地有界长回复传输、持久化与定向验证通过；实际 PC 断线/刷新验收待测试服务会话恢复，依赖 B05）可靠传输长回复和部分结果**
  - 位置：`server/internal/app/model_client.go`、`model_gateway.go`、`task_store.go`、`cloud_dsh_text_stream.go`、`server/internal/model/responses.go`、`server/internal/worker/cloud_dsh_executor.go`、Task stream/API、migration 00274，以及 `morde-pc` 会话读取与重连。
  - 已实现：Task 模型调用总时长 10 分钟，普通调用仍为 45 秒；Responses 分设连接/TLS 10 秒、响应头 20 秒、正文空闲 60 秒、总时长 10 分钟并支持取消。助手完整回复/Task snapshot/会话助手消息最多 2 MiB UTF-8 字节，数据库迁移同步该边界；用户消息 32 KiB 限制不变。每条 durable text delta 不超过 32 KiB，provider 大 delta 在 UTF-8 边界切片后同步持久化以形成反压；超总量在提交正文前拒绝。DSH 输出采用有界分块，每个文本/推理 delta 不超过 32 KiB，单 IPC frame 最多 4 MiB，非法 UTF-8、超总量或超 frame 在写出前拒绝。
  - 验收：fake provider 覆盖大于旧 256 KiB/32 KiB 边界的长文本、流式持久化、最大值拒绝、正文空闲/总超时、取消与慢消费者；PC 必须从实际客户端入口验证原会话的刷新/重连和无重复展示。有限 2 MiB 是存储/传输护栏，不等于无限输出，也不改变模型能力上限。
  - 本地测试和 PC 实测记录见第 18 节。`make pc` 构建通过；当前测试服务会话无法续期，不能宣称 PC 长回复交付验收通过，故保留未勾选状态。
- [ ] **B07（本地实现与回归验证完成；整体验收依赖 B05、B06）区分截断并安全续接文本**
  - 位置：provider 响应状态解析、模型调用结算、DSH checkpoint、对话片段持久化。
  - 实施：仅对带有效签约 budget plan 的 adaptive Task，把已知用量、明确 `length`、无工具/JSON/图片的纯文本部分结果加密保存；只在这一签约路径将其映射为 DSH `max-tokens` 并带受校验的 replay marker。旧路径继续按截断失败处理。DSH 使用已锁定 Runtime 的 `agent/turn-stopping` 与逐次 `agent/request-budget`：最多续接两次，每轮生成新的 call/checkpoint 身份并重新准备预算；已确认前文保留在同一会话。工具请求、过滤、取消、超时、未知用量和无效响应不进入续写。模型派发前检测到新用户消息时丢弃排队续写；若新消息在派发后到达，已有 delivery-version fence 丢弃旧结果并随后处理用户消息，但 provider 可能已接收这次请求。恢复时不再追加通用 recovery prompt。两次后仍被截断则以 `MODEL_OUTPUT_LIMIT` 返回部分失败。生产 planner admission 仍关闭，普通/旧 Task 路径和生产 cap 不变。
  - 验收：分别模拟 `max_output_tokens`、`content_filter`、网络中断/未知计费、部分正文、重复段落、不完整工具参数、重启恢复与额度不足；账本和原会话内容准确，副作用执行次数不重复。本地实现和已覆盖边界见第 19 节；实际 PC 刷新/重连交付随 B06 验收，因此 B07 整体仍未勾选。

- [ ] **B08（本地实现、exact `long_form` 的 `O_safe/O_send` 原子预留/账本落账、定向 PostgreSQL/全 Go 回归及文档记录的 DeepSeek Gateway 4,096-token 输出验证通过；当前源码 `make pc` 已实测长回复切换并恢复短答，PC 消息交付/重连与生产端到端仍待验，P3 放行依赖 B05–B07 门禁）提供显式原会话长回复路径**
  - 位置：消息接收/路由、PC 撰写入口、原会话正文持久化与展示。
  - 实施：短答/状态/取消继续走快路径；请求使用明确的 `response_mode=short|long_form`，在调用分类模型前分流；不把隐式识别放进首发标准。`ApplyConversationRoute` 的普通 reply 会把源消息标为 applied 并原子交付，长路径具有同等源消息/会话绑定、幂等和取消，不把现有 reply 结构字段抬成任意长正文。无 Task 长文必须匹配已配置的 exact model route 与 `long_form` 阶段策略，明确取得 `O_goal/O_min`；`O_safe` 由已验证模型输出/上下文、当前账户 token 余额及项目费率下剩余费用共同收紧，`O_send=min(O_goal,O_safe)`，低于 `O_min` 时不派发。新请求先尝试按候选 cap 原子预留；若额度不足，则按当前账户/项目余额重算并以缩小后的 cap 重试一次，最终事务再复核并发变化。没有 exact 策略、能力或可计量额度时 fail closed。route ledger 与账户/项目预留保存一致的分项计划和版本；不把 Task 的 50,000 描述成纯对话额度。每次长回复只调用一次 provider，总时限为 4 分钟；`length` 时保留并展示部分答复，不自动续写。取消、超时、未知用量不得按文本截断处理，也不得自动重放；长回复不创建后台工具 Task，除非用户实际委托后台工作。Portal 已加入显式入口、纯文本限制、取消及状态展示；它不替代 `morde-pc` 验收。
  - 验收：PC 上短答仍不新增模型往返；显式长文完整返回原会话，部分完成/暂停状态可读；验证 4 分钟总时限、取消及刷新重连，超时或未知用量时不自动重放、不误报成功且账本仍可核对；无 Task 模型能处理长文，重试不丢文或重复。普通 Web/iOS 不代替 PC 验收。

- [ ] **B09（进行中：当前 `release@d6c4c794` 的干净快照 P0 smoke 为 8 pass/0 fail/2 预期例外；远端 `/healthz` 可达，但运行提交 `b47f009c` 无法在本地浅克隆核验，尚未部署当前 release、完成远端 smoke/dry-run 或真实 PC+模型基线；B02–B08 的真实验收门禁仍未满足，v4 备份/恢复与独立存储门槛也未满足）按基线验证并灰度（P3）**
  - 位置：定向 Go/Node/PostgreSQL 测试、`make pc` 实测、验证记录，以及当时有效的 Harness 流程/TODO 文档。
  - 实施：锁定代码 SHA、模型与版本、端点/能力/计量来源、用例和判分方式。同一模型及合成输入对照原行为与候选策略；影子阶段先记录，不改变发送额度；通过门禁后只对明确试点用户和一两个可信模型启用，保留关闭新策略的回滚能力。
  - 验收：每类建议至少 30 个固定样本，报告有效样本量、p50/p95、绝对差、质量与失败类别；短答不得增加模型往返；暂以 p95 首字时间相对基线不劣化超过 10% 为门槛，噪声过大则扩样。长文按预先固定的必需条目/事实清单验收完整性，不以字数代替质量；至少一个预期完整的可见文本超过 2,048 个模型 tokenizer token。证明两个子 Agent 实际并发、预算暂停后恢复且已完成步骤不重跑；安全用例超额派发和重复可见副作用均为零，并覆盖长历史、未知能力、预算不足、取消和截断。假端点只证明协议，不冒充真实模型。结果分“通过/失败/未覆盖”；保留 provider 用量、发送上限和可见交付证据，不记录秘密或正文。

- [ ] **B10（未开始，P4 后续阶段；依赖 B09 试点数据）设计并评估独立按次与周期止损**
  - 位置：Task/Schedule 预算产品设计、OpenAPI、PC/iOS 设置与账单展示。
  - 范围：先实测现有 Task 50,000 是否需要调整；设计新交互 Task 的有限 token、单次触发总模型调用数和总时长止损。Schedule 采用单次触发额度与滚动 24 小时额度；预算占用按预留创建时间归窗，跨窗口的在途/未知预留继续计入，不因时间窗口翻转释放。同一触发中的重试和子 Agent 共用额度；超过后暂停并说明，不积压后补跑。未知 BYOK 费率只显示 token/调用次数/时长止损，不承诺金额上限。显式加额须有适用范围、失效时间和幂等键，沿用现有审批/审计；默认仅对获批的本次 Task 生效。用户可查看已用、剩余及暂停原因。
  - 验收：先提交模型/费率/计量分布、真实供应商回执数据与新产品合同，再决定是否实现及额度值；已知价格使用保守费率和整数最小货币单位。验证触发内重试/子 Agent 共用额度、跨窗未结占用、暂停后不补跑、加额重复提交不重复生效、BYOK 未知费率展示和回滚。历史 Task/Schedule 与旧客户端合同不静默迁移；产品结论未批准前不增加周期预算字段或变更新建默认。

## 6. 执行门禁与顺序

**B01 的 P0 分为本地整链门禁和生产发送门禁。** DSH runtime → Worker → PostgreSQL prepare/ledger → ModelGateway → fake HTTP 的单次签约、结算和 settled replay 门禁已通过；预算响应存储也已覆盖 ledger ID 与 DSH frame ID 相同/分离。新增边界用例证明预派发取消后同键安全重试，provider timeout/503 和派发后 Task cancel 不会再次 HTTP 派发；PG 故障注入覆盖 dispatch 事务回滚及 unknown/replay fail-closed。Portal bootstrap 已接入但部署 admission 关闭，实际请求 cap 没有改变；未杀真实进程，也未做真实 provider 计费。完成 B01 仍需验证普通调用与压缩调用的完整取消、重启/崩溃、未知派发及 provider 失败一致性。生产门禁通过前，不能为模型目录、影子策略或客户端流程修改在用请求上限、默认费用或权限。

P0 协议子门禁通过后可开始 P1 的 B02、B04；B03 影子计算可与 B02/B04 并行，B04 实现纳入 `O_goal/O_min/O_safe/O_send`，影子结果须在生产试点开放前核验。B05 接入实现依赖 B01 的生产合同、B02 和 B04；B01 生产门禁、B04 账本事务及 B05 一致性验证全部通过后，才可为试点路由开放发送策略。P2 为 B06+B07 的可靠长回复和安全续接；P3 为 B08+B09 的原会话长回复、PC/模型基线和灰度；P4 为 B10 的按次/周期止损评估与独立决策。B08/B09 用当前源码 `make pc` 验收，iOS 负责兼容和原会话对账。记录必须区分“接口探针 / fake HTTP / PG / 真实 PC 与模型”四层，各自标明通过或未测。

阶段依赖：B01 协议子门禁已通过，B02 的元数据/API/UI 实现已完成本地代码验证；B04 依赖协议子门禁与 B02，纳入 B03 计算和影子核验；B01 生产门禁与 B04 账本门槛通过后，再完成 B05 的发送合同和一致性验证，然后才开放发送试点 →（B06 与 B07）→（B08 与 B09）。B08 的本地开发可提前进行，但不能据此部署或开放试点；其 PC/生产验收仍等待 B05–B07 门禁。B10 等 B09 试点数据并独立产品评审。B05、B06 已有本地实现，但生产/客户端门禁未通过；B07 的本地实现及自动化回归已完成，整体仍依赖 B05/B06。所有生产门禁仍按已验收依赖逐项放行，不因 P1–P4 标签自动视为完成。

## 7. 复查代码与本地基线

- [Go 网关的 Luna 策略与重新校验](https://github.com/lizhongxuan/morde/blob/936389c24ea61db45eded881423cef74d8815ca8/server/internal/app/model_gateway.go#L492-L514)；[响应与 256 KiB 校验](https://github.com/lizhongxuan/morde/blob/936389c24ea61db45eded881423cef74d8815ca8/server/internal/app/model_gateway.go#L347-L374)；[Responses 已识别 length 和 content filter](https://github.com/lizhongxuan/morde/blob/936389c24ea61db45eded881423cef74d8815ca8/server/internal/model/responses.go#L478-L495)。B07 从终止状态走查，不要只改 timeout。
- [Task 与平台账户对预算的不同检查](https://github.com/lizhongxuan/morde/blob/936389c24ea61db45eded881423cef74d8815ca8/server/internal/store/postgres/model_usage.go#L175-L220)；[未知 Task 用量处理](https://github.com/lizhongxuan/morde/blob/936389c24ea61db45eded881423cef74d8815ca8/server/internal/store/postgres/model_usage.go#L1180-L1213)；[请求里的 TaskBudgetRequest / TaskBudget / TaskUsage](https://github.com/lizhongxuan/morde/blob/936389c24ea61db45eded881423cef74d8815ca8/server/openapi/portal.yaml#L12877-L12908)。不要把现有 TaskUsage 的可空用量误读成可空的 TaskBudget。
- [回复字段 2,000 rune](https://github.com/lizhongxuan/morde/blob/936389c24ea61db45eded881423cef74d8815ca8/server/internal/app/conversation_route_decision.go#L27-L41)；[直接回复的原会话原子交付](https://github.com/lizhongxuan/morde/blob/936389c24ea61db45eded881423cef74d8815ca8/server/internal/store/postgres/conversation_route_actions.go#L1032-L1045)；[32 KiB 助手消息边界](https://github.com/lizhongxuan/morde/blob/936389c24ea61db45eded881423cef74d8815ca8/server/internal/app/task_store.go#L10-L16)。32 KiB 是需核对和保留/替代的保存边界，不等同模型输出上限。
- 离线回归：在匹配 lockfile 的 runtime 副本内先运行 `npm run build:dsh-compaction`（现会先校验并应用 DSH request-budget patch，再构建 compaction），再运行第 8 节 Node 测试命令。默认值探针 `test/model-budget-dsh-default.test.mjs` 注册本地 `LlmAdapter`，验证 signal 默认值按 step 更新、显式 2,048 上限和并发 Agent 隔离；request hook 探针 `test/dsh-request-budget-hook.test.mjs` 验证 header 冻结前的动态 cap 与最终输入/工具历史一致。均不访问远程端点。

## 8. 本轮实施与验证记录（2026-11-27）

- **方案复核：** 已逐项核对 Downloads 中的《Morde_个人Agent_模型预算与可靠交付实施方案.md》。采纳其 P0–P4 分阶段门禁、预算等待截止/唤醒/公平性、跨滚动窗口继续计入在途占用、同次 Schedule 触发共享额度、显式加额幂等/范围/时效，以及固定样本和质量验收要求。对应修改已写入第 5、6 节；不把示例 `4,000` 作为默认，不照搬提议字段作为 API，不变更旧 Task/Schedule 合同。
- **B01 分层结果：** DSH hook 与 Worker/Runtime 双向 IPC 协议探针通过；Go `ModelBudgetPlanner` 已有 Worker adapter、ModelGateway preparation 与 PostgreSQL ledger 实现。Portal 现按 `output_budget_admission_enabled` 条件注入 planner，部署默认 false，且 Worker 仅为 exact-route 与能力来源合格的 Task 开启 admission。真实 Morde Worker → runtime → ModelGateway → 本地 `httptest` 假 provider 链通过，假计划 cap 为 **3,072**，provider 收到 `max_output_tokens=3,072`；验证了 prepare/final 摘要和身份，且没有 fallback 绕过网关。该本地假 provider 不代表真实模型计费。B01 生产门禁仍未通过。
- **预算安全限制：** 模型预算 admission 默认关闭，部署配置关闭 planner admission，legacy `max_tokens` 路径仍生效。DSH opt-in 协议探针使用 fake planner，不创建 PostgreSQL hold；独立的本地集成测试则直接覆盖 store-backed prepare/settle。账本与 DSH 生产链路、失败 attempt 和重启恢复门禁未完成前不得启用生产 admission。协议仍遵守 ModelGateway 已有 32,768 最大值，本轮没有移除或证明该固定网关边界是模型能力上限。
- **B02 能力目录实现：本地实现与自动验证通过；PC 端真实后端读写未验。** 新增输出最大值元数据迁移、模型目录、连接 API/OpenAPI、个人模型设置和管理后台展示。系统精确路由展示目录来源，unknown 显示“暂未识别”；个人自定义路由可设置覆盖，管理员共享路由表单不显示后端拒绝的覆盖字段。用 `make pc` 重建后在当前源码客户端打开“设置 → 模型与工具”，实测了上限字段/unknown 展示；当前测试服务端仍显示 unknown，未验证它能返回目录值或保存新覆盖。未在远程测试服务器写入配置。
- **B03 实施状态：** 在精确六阶段策略/计算器外，已实现默认关闭的 shadow 配置、Task 只读 repeatable-read 预算快照、采样 plan/result trace 和低基数 Prometheus 指标；PG 用例验证 account/Task/project output limit 与未知值，且前后 Task/ledger/preparation snapshot 完全相同。Gateway 用例验证开启 shadow 后现行 provider cap、reservation、dispatch 和结果不变。当前可用可信的根 Agent 无工具结构识别 `root_text_only`；summary/long_form/text_continuation 等阶段 metadata、在线 sample、真实流量离线比较及质量验收仍未完成，故 B03 不勾选。
- **B01/B04 未完成项：** 本地 PostgreSQL 已覆盖账户/Task/项目预留、settle、过期释放、竞争、已知超额差额、dispatch transaction rollback、settled no-duplicate 和 unknown/no-replay；Worker 整链覆盖预派发取消重试、provider timeout/503 和派发后 Task cancel。仍缺生产 planner 接入、压缩真实 ledger 结算、真实 Portal 进程 crash、动态策略/费率并发重算、持续负载槽位公平和 unknown overage 人工对账闭环。B01、B04 不勾选，不能开放生产 cap。PC 预算功能尚不存在；此前 PC 验收仅覆盖模型连接页展示，不覆盖预算发送策略或真实模型场景。
- **验证命令与结果：**
  - `runtime/morde-cloud-runtime`: `node --test --test-concurrency=1 test/dsh-request-budget-hook.test.mjs` —— **5/5 通过**；覆盖变 cap、最终 messages/tools/toolHistory 一致、series 重置、并发隔离、header/stream 一致与无 hook/undefined 回退。
  - `runtime/morde-cloud-runtime`: `npm run build:dsh-compaction` —— 通过；patch 脚本识别目标 DSH SHA 并确认已幂等应用，随后构建 compaction。
  - `runtime/morde-cloud-runtime`: `node --test --test-concurrency=1 test/model-budget-dsh-default.test.mjs test/hermes-compaction-window.test.mjs test/hermes-compaction-runtime.test.mjs test/compaction-recovery.test.mjs` —— **24/24 通过**。
  - `runtime/morde-cloud-runtime`: `node --test --test-concurrency=1 test/model-budget-admission-runtime.test.mjs` —— **26/26 通过**；覆盖普通 Task plan、取消、恢复工具投影、压缩生成独立计划以及 overflow attempt checkpoint 后重新申请计划。
  - `runtime/morde-cloud-runtime`: `npm test` —— **全量 290/290 通过**，包括 request-budget hook、prepare/plan IPC、Task compaction、会话/恢复/工具投影用例和构建 `dsh-compaction`。
  - `server`: `GOPROXY=off GOSUMDB=off go test ./internal/worker -run '^TestCloudDSHModelBudget' -count=1 -v` —— **7 个顶层测试及 7 个拒绝/变更子用例通过**；包括本地 HTTP `max_output_tokens=3,072` 检查。
  - `server`: `GOPROXY=off GOSUMDB=off go test ./internal/worker -run '^TestConversationRouteWorkerTriggersSummaryByTokenPressureBeforeTransportWatermark$' -count=1 -v` —— **失败**：`committed=false, sources=0`。这是非预算路径用例；仅凭本轮无法判断它是否为基线已有问题，不把失败归因于本次预算改动，worker 全包需修复或重新核对基线后再标绿。
  - `server`: `GOPROXY=off GOSUMDB=off go test ./internal/app -run '^TestBuildModelOutputBudgetPlan' -count=1` —— 通过。
  - `server`: `GOPROXY=off GOSUMDB=off go test ./internal/app -count=1` —— 通过。
  - `server`: `GOPROXY=off GOSUMDB=off go test ./internal/api -count=1` —— 通过；覆盖输出能力字段的响应/输入校验与密钥不回传。
  - `server`: `CH_TEST_DATABASE_URL='postgres://postgres:budgettest@127.0.0.1:63216/postgres?sslmode=disable' GOPROXY=off GOSUMDB=off go test ./internal/store/postgres -run '^TestModelMaxOutputCapacityMigrationUsesExactCatalogAndLeavesUnknownRoutesUnknown$' -count=1 -v` —— **通过**；使用本地临时 PostgreSQL 容器和随机临时数据库，不连接测试服务器。
  - `web/portal`: `npm exec -- vitest run src/routes/Connections.test.tsx src/routes/admin/AdminModels.test.tsx` —— **42/42 通过**；验证来源/unknown 展示、个人模型覆盖语义及共享后台表单不会发送被 API 拒绝的字段。
  - `web/portal`: `npm run typecheck` —— 通过；新增响应元数据在 OpenAPI 中按可选字段生成，允许旧版服务器滚动升级时省略。
  - `web/portal`: 两个模型连接页面及测试的 ESLint —— 通过。
  - `make pc` —— 当前源码构建与启动完成；在 `morde-pc` 设置页查看连接详情，确认新增单次生成上限及 unknown 文案。当前连接的测试服务器没有返回新目录值，未执行远程写入。
  - `git diff --check` —— 通过。
- **外部测试边界：** 没有调用 DeepSeek API、远程模型或测试服务器 API，没有部署、上传文件或用 SSH 修改远程主机。模型连接能力元数据的 PG 测试使用本地隔离数据库；fake provider 使用本地 `httptest`。完整门禁仍需预算生产接入后的 PC 写入路径、真实模型和 provider 用量核对。

## 9. 本轮预算实现增量与验证结果（2026-11-27）

- **策略与能力快照：** 阶段定义改为精确 `HTTPS BaseURL + model` 路由和显式策略版本；六个阶段必须完整配置，重复/缺失 stage、无效 route 或未配置 route 都拒绝。能力版本 hash 纳入输出上限及来源/目录版本、上下文窗口及来源/目录版本、精确路由和连接更新时间。存储发现同一 DSH 调用仍处于未派发状态但能力/策略/Task fence 已变化时，先释放旧 hold 再重新计算；已派发调用不重放。应用单测覆盖能力字段变化，PG 测试覆盖过期旧快照被替换且旧 plan 不能派发。
- **预算 ledger：** migration 00265 保存 prepare 身份、Task fence、能力/策略版本和分项预留；同一短事务计算并预留账户、Task 和可用项目 token/费用额度。最终请求在派发前重验调用摘要、签约 token 数、Task fence 和能力/策略快照，再将 `prepared` 原子转成 `dispatched`。已知结果结算，已过期且可证未派发的 hold 由 Portal 每分钟清理；清理和 dispatch 有效期检查均由 PostgreSQL `clock_timestamp()` 决定，避免节点时钟导致提前释放。
- **超额处理：** migration 00266 保存 provider 回报相对输入/输出预留的精确差额。已知用量超出签约值时，账户账本及项目账本（如适用）冻结为 `unknown`，Task 总用量置未知；项目路径同时保存锁定费率下的 reserved/actual/money-overage 微单位。migration 对有 overage 证据的回滚会拒绝，避免删掉尚待对账的数据。
- **本轮通过的验证：**
  - `server`: `GOPROXY=off GOSUMDB=off go test ./internal/app -count=1` —— 通过。
  - `server`: `CH_TEST_DATABASE_URL=<本地隔离 PostgreSQL DSN> GOPROXY=off GOSUMDB=off go test ./internal/store/postgres -run '^(TestPostgresAdaptiveProjectModelConfirmedNoDispatchReleasesAllHolds|TestPostgresStaleAdaptiveModelDispatchBecomesUnknownAndCannotReplay|TestPostgresTaskModelBudgetPreparationReservesOnceAndSettles|TestPostgresExpiredPreparedModelBudgetIsNotDispatchedAndIsReleased|TestPostgresAdaptiveModelOverageFreezesAndRecordsKnownDifference|TestPostgresAdaptiveProjectModelOverageRecordsTokenAndMoneyDifference|TestPostgresConcurrentTaskBudgetPreparationsShareAccountAndProjectLimits|TestPostgresProjectFeeIntegerRoundingCapsAdaptiveOutput|TestModelBudgetPreparationMigrationEnforcesPlanContractAndRollbackGuard)$' -count=1 -v` —— **9 个顶层 PG 用例通过**，migration 合同用例中的 14 个约束拒绝子用例也通过；共享 300-token/300-micro 额度并发场景恰有一个预留成功、另一个被拒绝；输入 50、输出 33/34 的费用边界分别为 51/52 微单位。
  - `server`: `CH_TEST_DATABASE_URL=<本地隔离 PostgreSQL DSN> GOPROXY=off GOSUMDB=off go test ./internal/store/postgres -run '^TestModelBudgetPreparationMigrationEnforcesPlanContractAndRollbackGuard$' -count=1 -v` —— 通过；覆盖 00265/00266 结构、约束及有准备/对账数据时拒绝回滚。
  - `server`: `GOPROXY=off GOSUMDB=off go test ./cmd/portal -count=1` —— 通过；覆盖过期 prepare 清理挂钩的调用、nil cleaner 和失败路径。
  - `git diff --check` —— 通过。
- **尚未验证与安全界限：** 本轮没有完成 `internal/store/postgres` 全包回归；仅完成上述定向 PostgreSQL 集成与 migration 测试。store 级用例已证明：调用方明确保证 provider HTTP 尚未开始时可以释放已消费但未发送的预留；stale `dispatched` 会保留为 `unknown`、阻止清理/新 prepare/重复派发。仍未覆盖崩溃注入和 DSH → store → fake endpoint 计数的端到端 no-dispatch/unknown recovery；第 9 节写入时 recovery attempt 与 adaptive plan 尚无兼容合同；第 10 节补充了独立 recovery DSH/ledger identity 及 PG 定向验证。等待队列唤醒/公平性、人工 overage 对账 UI、生产配置启用后的 planner + PostgreSQL + provider 端到端链路、真实模型账单和 `make pc` 预算流程也未验。Portal planner bootstrap 已接入但 admission 配置默认关闭，普通 Task 与现有 2,048/32,768 行为未改变；没有请求 DeepSeek API、连接测试服务器、部署或通过 SSH 修改远程主机。
- **等待/恢复边界审计：** 现有 Task 状态/API 可用 `waiting_user` 与原因码表达预算容量等待，离开 `running` 会清除 lease；`ClaimNextTask` 目前不会自动重领通用 `waiting_user`，因此后续唤醒必须使用专用 reason 和独立 reconciler，不能影响审批、澄清或 workspace-sync 等既有等待。第 9 节记录时预算化恢复身份合同尚缺；第 10 节已实现并验证独立 DSH frame / ledger ID，容量自动唤醒仍保持禁用。

## 10. 自适应预算集成与恢复身份验证记录（2026-11-27）

- **DSH / Worker / recovery 身份：** 普通自适应 prepare 将规范 DSH model-frame ID 与账本调用 ID 分开传递；Task recovery attempt 在 migration 00267/00268 持久化 `RecoveryDSHCallID` 和 `RecoveryCallID`，分别用于 DSH checkpoint/frame 与模型账本。Worker、runtime 与 app 在准备、执行、恢复时验证两种身份和 Task fence，未知派发仍 fail closed，已结算响应仅按原 ledger identity 回放。模型预算输入估算同时纳入网关附加的 SOUL/policy instructions。
- **集成中发现并修复的问题：** 首次预算 prepare 已创建 `prepared` ledger 行，网关随后的响应重放检查曾把它错误当成“不可恢复的已存在调用”并阻断首次派发。`RecoverTaskModelResponse` 现在仅对尚未派发的 `prepared` 返回“无响应可恢复”并继续到已签约派发路径；`dispatched`、`unknown` 等状态仍拒绝重试，settled 状态仍要求保存可验证响应。新增 PG 链路用例证明该分支及重放约束。
- **计算证据：** `TestBuildModelOutputBudgetPlanMatchesOfflineReference` 使用固定随机种子对 5,000 组输入逐一比较生产 `O_safe/O_send` 计算和独立参考公式；差分用例通过。它验证纯计算，不等于线上影子数据、真实流量对照或 provider 计费证明。
- **通过的验证：**
  - `server`: `GOPROXY=off GOSUMDB=off go test ./internal/app -count=1` —— 通过，包括 5,000 组固定种子差分计算。
  - `server`: `GOPROXY=off GOSUMDB=off go test ./internal/worker -run 'TestCloudDSHModelBudget|TestCloudDSHModelRecovery' -count=1 -v` —— 通过，覆盖 opt-in admission、plan 绑定/一次性消费及 recovery frame 与 ledger 身份校验。
  - `runtime/morde-cloud-runtime`: `node --test --test-concurrency=1 test/dsh-request-budget-hook.test.mjs test/model-budget-admission-runtime.test.mjs test/model-budget-recovery-runtime.test.mjs` —— **33/33 通过**，包括拒绝不匹配身份、取消、恢复后重新签约及默认关闭路径。
  - `server`: `CH_TEST_DATABASE_URL=<本地隔离 PostgreSQL DSN> GOPROXY=off GOSUMDB=off go test ./internal/store/postgres -run '^(TestPostgresTaskModelRecoveryPreparesDistinctBudgetAndDSHIdentities|TestPostgresAdaptiveBudgetPlannerGatewayAndLedgerUseOneOutputCap|TestPostgresTaskModelBudgetPreparationReservesOnceAndSettles|TestPostgresExpiredPreparedModelBudgetIsNotDispatchedAndIsReleased|TestPostgresAdaptiveProjectModelConfirmedNoDispatchReleasesAllHolds|TestPostgresStaleAdaptiveModelDispatchBecomesUnknownAndCannotReplay|TestPostgresAdaptiveModelOverageFreezesAndRecordsKnownDifference|TestPostgresAdaptiveProjectModelOverageRecordsTokenAndMoneyDifference|TestPostgresConcurrentTaskBudgetPreparationsShareAccountAndProjectLimits|TestPostgresProjectFeeIntegerRoundingCapsAdaptiveOutput|TestModelBudgetPreparationMigrationEnforcesPlanContractAndRollbackGuard|TestModelMaxOutputCapacityMigrationUsesExactCatalogAndLeavesUnknownRoutesUnknown|TestPostgresTaskModelRecoveryUsesDistinctLedgerIDAndReplaysSettledResponse|TestPostgresTaskModelRecoveryPersistsAndConsumesOneStableAttempt)$' -count=1` —— **14 个定向 PostgreSQL 用例通过**。包括 Worker planner adapter → PG prepare → `ModelGateway` → 合成模型客户端一次 3,072 输出 cap 派发与结算、settled response 重放不重复调用、recovery 独立身份/稳定 attempt、unknown/no-dispatch、并发额度和 migration 合同。只连接本机隔离 PostgreSQL，不连接用户测试服务器。
  - `server`: `CH_TEST_DATABASE_URL=<本地隔离 PostgreSQL DSN> GOPROXY=off GOSUMDB=off go test ./internal/store/postgres -run '^TestPostgresAdaptiveBudgetPlannerGatewayAndOpenAICompatibleHTTPUseOneOutputCap$' -count=1 -v` —— 通过；本地 wire-format HTTP 假 provider 收到 `max_output_tokens=3072`，输入/输出用量 `8/6` 入账，settled replay 后 HTTP 请求总数仍为 1。该测试 client 仅为本地测试实现，未使用生产 HTTPS allowlist client。
  - `git diff --check` —— 通过。
- **完整包回归结果：** `GOPROXY=off GOSUMDB=off go test ./internal/worker -count=1` 有 1 个包级失败：`TestConversationRouteWorkerTriggersSummaryByTokenPressureBeforeTransportWatermark`（`committed=false, sources=0`），与本轮前单独运行同一用例的失败一致；它不在预算路径，未据此宣称 Worker 全包通过。`CH_TEST_DATABASE_URL=<本地隔离 PostgreSQL DSN> GOPROXY=off GOSUMDB=off go test ./internal/store/postgres -count=1` 在 10 分钟全局测试超时后中止于 `TestPostgresMCPPlanDispatchRollsBackBindingWhenDispatchUpdateFails` 的测试库迁移初始化；该包包含约 1,148 个串行测试，整包结果未通过也未完成，预算相关定向 PG 组仍通过。
- **仍未完成：** runtime admission 测试与 PG → gateway → HTTP fake provider 测试仍彼此独立，尚未组成一个完整 DSH runtime → PostgreSQL → fake provider 进程级链路；全 `internal/store/postgres` 包 10 分钟超时，尚无整包通过结果；崩溃注入、容量等待/公平唤醒、unknown overage 人工结算 UI、线上影子采集或真实 DeepSeek/provider 计费核对。Portal bootstrap 已接入但 admission 仍默认关闭，普通 Task 实际发送 cap 未改变，B01/B03/B04 不勾选，B05–B10 未开始。没有使用提供的 API key、远程服务器凭据或修改远端机器。

## 11. 容量等待与用户状态验证记录（2026-11-28）

- **入队判定与状态：** 只有账本 prepare 明确返回模型并发限额（`model_concurrency_limit` 且阶段为 `reservation`）时才创建等待；provider 自身限流、账户/Task/项目硬预算不足和未知结果不会误入自动重试队列。PG 事务校验 Task/owner/initiator/lease/Agent/recovery/request/checkpoint fence，写 wait row、转换 Task 为 `waiting_user` 并清空 worker lease。migration 00270 按 owner + `BudgetCallID` 保存首次等待时刻和 deadline；期限从首次等待开始，单次调用最多 5 分钟。持久等待失败时拒绝继续执行。DSH 运行器收到已持久化等待标记后退出，不占 Worker 槽；否决消息写回失败也保留安全等待信号。
- **唤醒、暂停和通知：** Portal reconciler 每轮最多处理每 owner 的最老 waiter，按账户 → 项目/成员 → Task → wait/ledger 锁序检查。已 deferred waiter 更新 `last_reconciled_at`，使同一 owner 后续更小请求能在后续轮次被检查。只有旧 Agent stop receipt 摘要与 checkpoint/recovery identity 全部匹配，才允许 requeue、timeout pause 或 invalidation pause；没有可信 stop receipt 时保留 wait row 并 defer。等待和暂停写 `task.status` 事件，避免将自动重试误报为需用户处理的通知。达到容量时交给现有 `ClaimNextTask`，硬额度、单次调用 deadline 或运行条件失效则安全暂停并删除等待行。requeue 删除 wait row，但保留按 `BudgetCallID` 绑定的 deadline；相同调用再次等待不延长期限，不同调用各自计时。新建或 replay-existing prepare 成功会清 deadline marker；inactive Task marker 由 reconciler 清理。整个 Task 的总等待时长仍未统一设限。
- **用户展示：** Task summary 增加安全 `waiting_reason`；Task attention 将等待显示为“等待模型容量”且无操作按钮；超时、最低预算不足及等待条件失效显示各自的暂停说明。任务等待、超时、失效状态由 Portal 单测覆盖。`make pc` 当前源码构建完成；没有在远程测试服务创建 Task 或写入数据，因此实际 PC 中带真实容量等待记录的手工状态验收仍待安排。
- **通过的验证：**
  - `server`: `CH_TEST_DATABASE_URL='postgres://morde_test@localhost:55439/morde_budget_test?host=/tmp/morde-adaptive-budget-pg-sock-20261127' GOPROXY=off GOSUMDB=off go test ./internal/store/postgres -run '^TestPostgresModelBudgetCapacityWait.*$' -count=1 -v` —— **通过**；19 个顶层等待用例及其 approval/clarification 子用例通过，包含同一调用跨 requeue 保留 deadline、不同 `BudgetCallID` 独立 deadline、00269→00270 expiry 兼容、prepare 清理 marker、recovery requeue 与 stop receipt 检查。
  - `server`: 同一 PostgreSQL DSN 下 `go test ./internal/store/postgres -run '^TestPostgresModelBudgetCapacityRecoveryInvalidationSpendsAttempt$' -count=1 -v` —— **通过**；验证 recovery 条件失效后消耗尝试并暂停。
  - `server`: `CH_TEST_DATABASE_URL=<本机隔离 PostgreSQL DSN> GOPROXY=off GOSUMDB=off go test ./internal/store/postgres -run '^(TestPostgresModelBudgetCapacityWait.*|TestPostgresTaskModelRecoveryPreparesDistinctBudgetAndDSHIdentities|TestPostgresAdaptiveBudgetPlannerGatewayAndLedgerUseOneOutputCap|TestPostgresTaskModelBudgetPreparationReservesOnceAndSettles|TestPostgresExpiredPreparedModelBudgetIsNotDispatchedAndIsReleased|TestPostgresAdaptiveProjectModelConfirmedNoDispatchReleasesAllHolds|TestPostgresStaleAdaptiveModelDispatchBecomesUnknownAndCannotReplay|TestPostgresAdaptiveModelOverageFreezesAndRecordsKnownDifference|TestPostgresAdaptiveProjectModelOverageRecordsTokenAndMoneyDifference|TestPostgresConcurrentTaskBudgetPreparationsShareAccountAndProjectLimits|TestPostgresProjectFeeIntegerRoundingCapsAdaptiveOutput|TestModelBudgetPreparationMigrationEnforcesPlanContractAndRollbackGuard|TestModelMaxOutputCapacityMigrationUsesExactCatalogAndLeavesUnknownRoutesUnknown|TestPostgresTaskModelRecoveryUsesDistinctLedgerIDAndReplaysSettledResponse|TestPostgresTaskModelRecoveryPersistsAndConsumesOneStableAttempt)$' -count=1` —— **定向 PG 账本、恢复与容量等待组通过**；连接本机隔离 PostgreSQL。
  - `server`: `GOPROXY=off GOSUMDB=off go test ./internal/app -count=1` —— **通过**；覆盖预算等待精确分类、Task attention 和既有 app tests。
  - `server`: `GOPROXY=off GOSUMDB=off go test ./internal/worker -run 'TestCloudDSHModelBudget|TestCloudDSHCapacityWaitSurvivesBudgetDenialPipeFailure|TestCloudDSH.*Recovery' -count=1` —— **通过**。
  - `server`: `GOPROXY=off GOSUMDB=off go test ./internal/api -run 'TestTaskSummaryCarriesSafeWaitReason|TestTaskValidationEvidenceIsDetailOnlyAndBoundToResult' -count=1`、`go test ./internal/domain/task -count=1`、`go test ./cmd/portal -count=1` —— **通过**。
  - `web/portal`: `npm run typecheck` —— **通过**；`npm exec -- vitest run src/routes/Assistant.test.tsx -t 'shows a model capacity wait|explains when a model capacity wait is invalidated'` —— **2/2 通过**。
  - 根目录 `make pc` —— Portal、Workbench 与当前 Morde PC 源码构建完成；Vite 输出已有的大 bundle 警告仍存在。真实 PC 没有装载一个实际 PG 容量 waiter 记录，因此只记构建通过，不记该状态的手工验收通过。
  - `git diff --check` —— **通过**。
- **安全与未覆盖：** 未使用提供的 DeepSeek API key，也未连接测试服务器、写远程数据、部署或 SSH。`internal/store/postgres` 全包回归此前曾在 10 分钟超时后中止，本轮只记录定向组通过；没有真实模型计费、生产 planner 接线、线上影子数据、manual overage UI 或持续流量公平压测。

## 12. DSH 到 PostgreSQL 与 fake HTTP 整链验证记录（2026-11-28）

- **跨层调用身份：** Worker prepare 返回的 ledger `CallID` 绑定 prepare 时的 DSH through；最终 DSH frame `CallID` 可绑定后续最终 through。Worker plan binding、Gateway reservation、响应 AAD 和 PostgreSQL response recovery 均分别核验账本身份、DSH 帧身份及 budget plan/call/request digest。prepare 与 final through 相同或不同时都允许；legacy 与 recovery 调用保持各自原有身份合同。错计划、错摘要或替换 ledger/frame ID 均 fail closed。
- **完整本地链路：** `TestCloudDSHModelBudgetPostgresHTTPUsesOneSignedCapAndSettlesOnce` 使用锁定 DSH runtime 与真实 Worker、PostgreSQL planner/ledger、`ModelGateway` 及 localhost wire-format fake HTTP provider。请求 cap-free prepare 与最终 model frame 绑定同一计划；provider 收到 `max_output_tokens=3,072`；输入/输出 `8/6` 写入原账本；settled replay 返回已保存结果且 HTTP provider 仍只被调用一次。它是本地假服务证明，不代表真实模型或生产 Portal 已启用。
- **通过的验证：**
  - `server`: `CH_TEST_DATABASE_URL='postgres://morde_test@localhost:55439/morde_budget_test?host=/tmp/morde-adaptive-budget-pg-sock-20261127' GOPROXY=off GOSUMDB=off go test ./internal/worker -run '^TestCloudDSHModelBudgetPostgresHTTPUsesOneSignedCapAndSettlesOnce$' -count=1 -v` —— **通过**，默认 Go vet 开启；1 个 DSH→PG→Gateway→fake HTTP 整链用例通过。
  - `server`: `CH_TEST_DATABASE_URL='postgres://morde_test@localhost:55439/morde_budget_test?host=/tmp/morde-adaptive-budget-pg-sock-20261127' GOPROXY=off GOSUMDB=off go test ./internal/store/postgres -run '^TestPostgresTaskModel(RecoveryPreparesDistinctBudgetAndDSHIdentities|ResponseAdaptiveBudgetIdentity)$' -count=1 -v` —— **通过**；2 个顶层 PG 用例通过，其中新用例覆盖 ledger/DSH ID 相同与不同、以及计划、budget call、摘要、DSH ID 和 ledger ID 篡改拒绝。
  - `server`: `GOPROXY=off GOSUMDB=off go test ./internal/app -count=1` —— **通过**，包括新的 response AAD 相同/不同 ID round trip 与篡改拒绝。
  - `git diff --check` —— **通过**。
- **门禁状态与边界：** B01–B04 继续不勾选。B01 的本地整链、取消/timeout/503/no-replay 子门禁已有证据，但 Portal bootstrap 已接入且部署 admission 关闭，真实 provider 与 Portal 进程崩溃未验；B02 的真实测试服务端读写仍待验；B03 影子路径代码与只读 PG 快照已实现且默认关闭，仍缺线上样本、完整阶段信号与离线流量对照；B04 的 PG transaction rollback/unknown 子用例已过，但仍缺真实进程 crash、动态费率/策略并发重算、持续负载槽位公平和 overage 人工对账。B05–B10 尚未开始。生产发送 cap 未改变。没有调用 DeepSeek API 或连接测试服务器。

## 13. 只读预算影子、派发失败与事务回滚验证记录（2026-11-28）

- **B03 shadow 路径：** 增加 `models.output_budget_shadow` 配置，默认 `enabled: false`。启用时须配置固定采样率、策略版本、精确 HTTPS route 和六个阶段值；Portal 将其映射为独立 shadow snapshot，不调用生产 adaptive planner。`ModelGateway` 仅对已配置 route 的 Task model call 计算候选计划；stage 可从明确的子 Agent session、工具集合信号及根 Agent 无工具的结构识别，其中根阶段命名为 `root_text_only`，不表示语义上的短答。它不猜 prompt、不卡当前请求、不写或释放 ledger、不暂停 Task，也不更改 `ModelRequest.MaxOutputTokens`。读取账户/Task/项目额度时走只读 repeatable-read PostgreSQL snapshot；校验 persisted Task/checkpoint fence 及模型连接 endpoint/model/version，分别扣除已结算使用、在途 reservation 和当前输入。未知或缺失维度以 nil limit 和固定原因记录，候选计算 fail-closed。provider 实际 output tokens、stop reason 和有限失败类别作为结果事件记录。trace 不保存 prompt、响应正文、API key 或原始 endpoint；Prometheus 不把 route/version 放进 labels，事件队列/属性仍有界。
- **B03 证据范围：** PG tests 证明 account / Task / project 候选输出 limit 分别为 `700 / 49,710 / 370`，未知/缺失预算得到 nil 与有限原因，并且调用前后 Task、account ledger、project ledger 和 preparation 快照字节一致；也拒绝连接版本过期的预览请求。App tests 证明有无 shadow 的 provider cap、reserved tokens、reserve/dispatch 次数与回复相同，observer panic、preview error 与零采样率不影响正常模型调用；当前 `go test ./internal/app -count=1` 通过，涵盖新增 `root_text_only` 阶段路径。config、Portal metrics 与 span exporter tests 验证默认关闭、配置校验、低基数 labels、shadow event allowlist 及敏感 route/prompt 字段过滤。这里没有启用生产 route、收集真实线上样本、完成固定请求流量离线对照、验证 summary/long_form/text_continuation 阶段或答案质量，B03 保持未完成。
- **B01/B04 失败边界：** Worker + local PG + fake HTTP 用例覆盖 provider timeout、HTTP 503、派发后 Task cancel 均变成 unknown 且同键 replay 不增加 provider 请求数；provider 派发前取消则可安全同键重试。PG 故障注入在 `prepared→dispatched` update 事务中触发错误，确认事务回滚、hold 未消费和 fake client 未调用；settled replay 不重复请求；合成 transport error 进入 unknown 且拒绝 replay。该证据不覆盖真实 Portal 进程在提交后/HTTP 首字节前被 kill，也不代表供应商真实计费回执。
- **本轮验证命令：**
  - `server`: `GOPROXY=off GOSUMDB=off go test ./internal/app -count=1` —— **通过**；完整 app package，包括 5 个新 shadow Gateway 用例。
  - `server`: `GOPROXY=off GOSUMDB=off go test ./internal/config -count=1`、`go test ./cmd/portal -count=1`、`go test ./internal/tracing -count=1` —— **全部通过**；分别覆盖 shadow 配置、Portal 有界 metrics、PostgreSQL span allowlist。
  - `server`: `CH_TEST_DATABASE_URL='postgres://morde_test@localhost:55439/morde_budget_test?host=/tmp/morde-adaptive-budget-pg-sock-20261127' GOPROXY=off GOSUMDB=off go test ./internal/store/postgres -run '^(TestPostgresModelTaskBudgetShadowPreviewReturnsDimensionLimitsWithoutWrites|TestPostgresModelTaskBudgetShadowPreviewReturnsNilForUnknownAndMissingBudgets|TestPostgresAdaptiveBudgetPreparedCommitHasNoDispatchBeforeGatewayCall|TestPostgresAdaptiveBudgetSettledReplayDoesNotDispatchTwice|TestPostgresAdaptiveBudgetAmbiguousTransportFailureIsUnknownAndCannotReplay)$' -count=1 -v` —— **5/5 通过**，使用本机隔离 PostgreSQL。
  - `server`: 同一隔离 PostgreSQL DSN 下 `GOPROXY=off GOSUMDB=off go test ./internal/worker -run '^TestCloudDSHModelBudgetPostgresHTTP(FailureEdges|UsesOneSignedCapAndSettlesOnce)$' -count=1 -v` —— **整链及失败边界通过**；provider timeout、503 与派发后取消均保持 unknown/no-replay。
  - `git diff --check` —— **通过**。
- **外部测试边界：** 没有调用 DeepSeek API 或远程模型，没有部署、上传文件或通过 SSH 修改服务器；本机 fake HTTP、隔离 PostgreSQL 和 PC 重建通过。另对测试门户做了不带认证/业务数据的 HTTP(S) 可达性探测：代理 CONNECT 成功但目标超时；没有登录、写远端数据或修改数据库。生产 cap 仍未变化，B01–B04 保持未勾选，B05–B10 未开始；生产真实计费、在线影子样本、质量/首字延迟比较、PC 预算入口和后续产品止损决策仍待完成。

## 14. 测试门户可达性与当前源码 PC 重建（2026-11-28）

- **本机客户端复验（2026-11-28）：** 再次运行根目录 `make pc`，Portal、Workbench 与 Electron renderer 构建完成，当前仓库 `morde-pc` 启动。客户端保留的测试服务登录状态显示“已登录，但云端会话暂时无法续期”；点击“重试连接”并等待 15 秒后仍回到同一错误，没有进入普通用户页面。没有退出当前会话、清除本机凭据或修改用户数据。
- **远端探测：** 当前无认证 `/healthz` HTTPS 请求在 8 秒后超时；TCP 22 可建立连接，但 SSH 在 banner exchange 阶段超时，未进入身份认证。无法从 PC 验收服务端模型目录读写。没有向 `/assistant` 或其他普通用户 Web 页面发送请求，没有尝试使用管理员密码登录或重置密码，也没有直连/修改远端数据库。
- **结论：** 这次构建只证明当前源码 PC 可以编译；UI 续期与上游超时使真实 PC 数据闭环保持未验证。门户恢复可达后，继续验证模型能力字段读取和个人覆盖值保存/刷新读取，再同步更新 B02 与验收记录。

## 15. B04 进程崩溃、动态重算与 FIFO 槽保护（2026-11-28）

- **公平性与迁移：** 新增 migration `00272_model_budget_capacity_slot_claim.sql`，为有效 capacity waiter 保存 `slot_reserved_at`。入队事务声明下一可用账户并发槽，普通 model reservation 与 adaptive Task prepare 在并发判断时将最老有效 claim 计入；waiter 被 reconciler 重排队后 claim 仍保留，成功 prepare 才删除。调用 deadline 已过时不会占用 slot，但等待队列仍保留原 Agent stop receipt 安全暂停路径。正向 FIFO 用例使用 `max_concurrent=1` 和真实 PostgreSQL Store prepare/dispatch/settle：释放当前槽后、reconciler 运行前，fresh Task prepare 得到 `model_concurrency_limit`；reconcile 后 waiter 使用原 BudgetCallID/ledger/DSH ID 成功 prepare 并清 claim；随后 fresh Task 成功 prepare。容量等待全组通过，包含 deadline、stop receipt、权限/checkpoint、硬额度以及旧 waiter 升级用例。
- **动态重算：** PG 用例 `TestPostgresAdaptiveBudgetReplanUsesCurrentPolicyAndProjectRate` 证明相同预算调用的策略版本和项目 rate 改变后，旧准备被丢弃、按最新 rate 重算，ledger 只保留新 hold 且不超项目费用额度。`TestPostgresAdaptiveBudgetCapacityWakeUsesCurrentProjectRate` 证明等待恢复后使用当前项目费率重新计划。两个用例都通过本机隔离 PostgreSQL。
- **进程崩溃：** `TestCloudDSHModelBudgetPostgresHTTPWorkerProcessKilledBeforeProviderHTTP` 启动独立 Go Worker helper 与锁定 DSH Node runtime 子进程；真实 Gateway 将 ledger/prepare 原子提交为 `dispatched` 后，helper 在进入 fake HTTP 之前阻塞，父测试对 Worker/Node 进程组发 `SIGKILL`。provider HTTP 请求数为 0；测试把 ledger `created_at` 调整到 stale 阈值后调用实际 `MarkStaleModelCallsUnknown`，断言恰好一条转为 `unknown`、Task usage 保持 NULL；相同 identity 的新 Gateway replay 被拒，HTTP 仍为 0。这是本地 Worker/DSH 进程边界，不是 Portal 二进制 crash，也不能判定真实 provider 对外请求是否已经发出或计费。
- **通过的验证命令：**
  - `server`: `CH_TEST_DATABASE_URL='postgres://morde_test@localhost:55439/morde_budget_test?host=/tmp/morde-adaptive-budget-pg-sock-20261127' GOPROXY=off GOSUMDB=off go test ./internal/store/postgres -run '^(TestPostgresModelBudgetCapacity.*|TestPostgresAdaptiveBudgetCapacityWakeUsesCurrentProjectRate|TestPostgresAdaptiveBudgetReplanUsesCurrentPolicyAndProjectRate)$' -count=1 -v` —— **通过**；capacity wait/deadline/stop receipt/FIFO slot、动态政策与费率组通过。
  - `server`: `CH_TEST_DATABASE_URL='postgres://morde_test@localhost:55439/morde_budget_test?host=/tmp/morde-adaptive-budget-pg-sock-20261127' GOPROXY=off GOSUMDB=off go test ./internal/store/postgres -run '^(TestPostgresConcurrentTaskBudgetPreparationsShareAccountAndProjectLimits|TestPostgresAdaptiveProjectModelConfirmedNoDispatchReleasesAllHolds|TestPostgresStaleAdaptiveModelDispatchBecomesUnknownAndCannotReplay|TestPostgresModelBudgetCapacitySlotClaimProtectsFIFOWaiter)$' -count=1 -v` —— **4/4 通过**；包含共享账户/项目竞争、确定未发送 hold 释放、stale unknown 和 FIFO claim。
  - `server`: `CH_TEST_DATABASE_URL='postgres://morde_test@localhost:55439/morde_budget_test?host=/tmp/morde-adaptive-budget-pg-sock-20261127' GOPROXY=off GOSUMDB=off go test ./internal/worker -run '^TestCloudDSHModelBudgetPostgresHTTPWorkerProcessKilledBeforeProviderHTTP$' -count=1 -v` —— **通过**。
  - `server`: `CH_TEST_DATABASE_URL='postgres://morde_test@localhost:55439/morde_budget_test?host=/tmp/morde-adaptive-budget-pg-sock-20261127' GOPROXY=off GOSUMDB=off go test -race ./internal/worker -run '^TestCloudDSHModelBudgetPostgresHTTPWorkerProcessKilledBeforeProviderHTTP$' -count=1` —— **通过**。
  - `git diff --check` —— **通过**。
- **门禁状态与外部边界：** B04 的这三类本地门禁通过，但 B01–B04 仍不勾选：Portal planner bootstrap 已接线但配置关闭；B02 真实 PC→测试服务端目录读取/个人覆盖写入被门户不可达阻断；B03 无生产 shadow 样本、六阶段实流信号与离线质量/延迟对照；B04 的 overage 核对闭环已本地实现并通过定向验证，仍缺部署后的真实供应商账单流程、完整个人模型/跨 owner 项目并发组合和长期压力公平测试。B05–B10 尚未开始；生产 cap 未变化。本轮未调用 DeepSeek API、未登录/写测试服务器、未部署/上传文件、未 SSH 或修改远端数据库。

## 16. 自适应预算 overage 管理员核对闭环（2026-11-28）

- **实现：** migration `00273_model_budget_overage_admin_reconciliation.sql` 为自适应 Task overage 增加管理员身份、供应商核对依据和实际输入/输出/费用字段；解析记录必须保持 `open` 或拥有完整 `resolved` 审计字段，Down migration 遇到已解决记录会拒绝丢弃审计。管理员未知用量列表仅为存在的自适应 Task overage 附带签约预留、模型上报量、token 差额和项目微单位费用。现有 reconciliation API 接受核实数量与证据；仅当证据记录与未知 Task 的计划、调用身份、预留及项目费率一致时，才允许结算超出原 hold 的 provider 确认用量。账户与项目账本、Task 用量、overage 状态及审计在同一事务内更新；超预算实际用量仍计入已用额度，不增加预算、不恢复丢失响应。其他没有该自适应 overage 记录的未知调用仍保留原有超预留拒绝规则。
- **管理界面：** 用户详情的待核对记录显示签约值和未验证的模型上报值，提供可编辑的核实 token 数和必填供应商证据；提交按钮及说明明确该操作只对账、不重跑模型。测试覆盖普通未知调用原流程和 adaptive project overage 的表单提交。
- **通过的验证：**
  - `server`: `GOPROXY=off GOSUMDB=off go test ./internal/app ./internal/api` —— **通过**；包括 API overage JSON 映射和 app 校验。
  - `server`: `CH_TEST_DATABASE_URL='postgres://morde_test@localhost:55439/morde_budget_test?host=/tmp/morde-adaptive-budget-pg-sock-20261127' GOPROXY=off GOSUMDB=off go test ./internal/store/postgres -run '^(TestPostgresAdaptive(Model|ProjectModel)Overage.*|TestPostgresModelUsageAdmin(ReconcilesProjectTaskAsInitiatorAcrossBothLedgers|ConcurrentReconcileAndAudit|RebuildsTaskOnlyAfterAllUnknownCalls))$' -count=1 -v` —— **通过**；包含项目账本按核实超额结算、未知记录清除、审计保留、相同请求幂等及改变证据冲突，并复核普通 unknown reconciliation 回归。
  - `web/portal`: `npm run typecheck` —— **通过**；`npm exec -- vitest run src/routes/admin/AdminModelUsage.test.tsx` —— **3/3 通过**。
  - `web/portal`: `npm run generate:api` —— **通过**，更新 OpenAPI 类型；本轮改动的 Go 文件 `gofmt -d` 与 `git diff --check` —— **通过**。
- **门禁状态与外部边界：** B04 overage 核对子门禁现为本地实现并通过定向验证，但 B04 仍不勾选：需完成个人模型/跨 owner 并发组合、长时公平性压力、默认关闭的生产 admission/影子核验，以及部署后真实供应商账单证据流程的验收。B01–B04 仍未放行，B05–B10 依执行顺序未开始；普通模型实际 cap 未改变。未调用 DeepSeek API、未登录或写测试门户、未部署/上传、未 SSH 或修改远端数据库。

## 17. B05 已签约模型上限与 32,768 边界（2026-11-28）

- **逐层实现：** 旧 DSH `max_tokens` 路径继续按原来的 2,048 上限工作；仅 opt-in 的 adaptive Task path 省略这个 legacy cap，并由每步单次 plan 提供 DSH request header、最终 frame 与 provider request 的同一值。Worker protocol、budget plan 与 Responses provider encoder 支持到 `app.MaxModelOutputTokens` 的安全整数范围；Gateway 只在存在完整签约 plan/call/digest 身份时放宽 32,768 检查，并以当前模型目录/个人覆盖的已验证 `MaxOutputTokens` 再约束 plan。只有 adaptive Task 请求会把当前模型能力上限写入 provider request 私有校验字段；无该能力证明时仍沿用 32,768 旧边界。超过已验证能力或缺少签约身份时，在 dispatch 前拒绝。Portal bootstrap 已接入但部署 admission 仍关闭，因此实际生产请求、旧任务和控制路径均未变化。
- **端到端证据：** `TestCloudDSHModelBudgetPostgresHTTPUsesVerifiedModelCapAboveLegacyLimit` 使用锁定 DSH、Worker、PostgreSQL prepare/ledger、ModelGateway 和 fake Responses HTTP：模型能力 64,000，策略签约 `O_send=40,000`，断言 DSH final frame、`model_budget_preparations.send_tokens`、账本总 reservation 与 HTTP `max_output_tokens` 一致；usage `8/12` 结算且 settled replay 不重复派发。假 provider 返回短文本，避免将此用例误读成大正文传输验收。
- **通过的验证：**
  - `server`: `GOPROXY=off GOSUMDB=off go test ./internal/app ./internal/model -count=1` —— **通过**。
  - `server`: 定向 `TestValidateModelCallRejectsLegacyOutputLimitAbove32768`、`TestValidateModelCallAllowsLargerOutputOnlyWithAdaptiveBudgetIdentity`、`TestTaskModelGatewayRejectsAdaptiveOutputAboveVerifiedModelLimit` 及 `TestEncodeRequestAllowsOnlyVerifiedModelOutputLimit` —— **通过**；旧路径 32,769 拒绝、无能力证明不能放宽、超 verified cap 派发前拒绝。
  - `server`: 隔离 PostgreSQL 下 `TestCloudDSHModelBudgetPostgresHTTPUsesOneSignedCapAndSettlesOnce`、`TestCloudDSHModelBudgetPostgresHTTPUsesVerifiedModelCapAboveLegacyLimit`、`TestCloudDSHModelBudgetPostgresHTTPFailureEdges`、`TestCloudDSHModelBudgetPostgresHTTPWorkerProcessKilledBeforeProviderHTTP` —— **全部通过**。
  - `runtime/morde-cloud-runtime`: `node --test test/dsh-request-budget-hook.test.mjs test/model-budget-dsh-default.test.mjs test/model-budget-admission-runtime.test.mjs` —— **34/34 通过**，包括 header/stream cap 匹配、同模型并发隔离、missing plan fail-closed 与 legacy 默认行为。
  - `gofmt -d` 及 `git diff --check` —— **通过**。
- **仍待验：** 本地 B06 已把响应持久化总上限提升到有限 2 MiB，并加入 32 KiB durable delta、4 MiB DSH frame 和响应正文超时/取消边界；上述本地验证不证明 PC 可恢复完整长文。实际 PC 测试会话目前无法续期，且 B01–B04 生产门禁未通过，因此 B05 不勾选，高 cap 生产路由继续关闭。生产请求 cap 未改变；没有真实 provider 或生产计费证据。

## 18. B06 有界长回复传输与持久化（2026-11-28）

- **传输与存储合同：** Provider 请求/响应体与 SSE 行分别保留有限上界；连接/TLS、响应头、正文空闲、总时长和取消分开处理。模型输出最大值与内容字节边界分离：provider SSE 可逐块交付；DB 以不超过 32 KiB 的 UTF-8 安全 delta 写入，完整 assistant reply/snapshot/conversation message 上限为 2 MiB，DSH frame 上限为 4 MiB。超 2 MiB 的持久化消息会被拒绝且不增加 task event；migration 00274 限制 assistant 持久化文本，user message 限制仍是 32 KiB。
- **通过的验证：**
  - `server`: `GOPROXY=off GOSUMDB=off go test ./internal/app -count=1` —— 通过。
  - `server`: `GOPROXY=off GOSUMDB=off go test ./internal/model -count=1` —— 通过；包括 512 KiB fake SSE（8 个 64 KiB delta）、正文空闲/总超时和取消。
  - `server`: `GOPROXY=off GOSUMDB=off go test ./internal/api -run '^TestTaskResponseIncludesValidatedAssistantMessageSnapshot$' -count=1` —— 通过，覆盖接近 2 MiB 与超限拒绝。
  - `server`: 定向 Worker 测试 `TestWriteCloudDSHModelChunksLongUTF8ContentIntoBoundedFrames`、`TestWriteCloudDSHModelRejectsInvalidOrOversizedContentBeforeWriting`、`TestCloudDSHTextStreamSplitsLargeProviderDeltaIntoDurableChunks`、`TestCloudDSHTextStreamDisablesDisplayAboveFiniteReplyLimit` 和部分写入一致性用例 —— 通过；DSH framing cancellation 用例单独通过。
  - `server`: 隔离 PostgreSQL 下 assistant snapshot、累计正文超限、`CloudDSHStream`、stale model recovery 与 Task stall 定向组通过；`TestCloudDSHStreamPersistsLargeReplyAsBoundedDeltas` 单独复跑通过。验证覆盖超过 32 KiB 的 durable 分块、快照读取与超限时不变更正文/event。
  - `web/portal`: `Assistant.test.tsx` 中 Task snapshot、过期事件游标恢复及事件序列缺口恢复 4 个相关测试通过；`make pc` 当前源码构建通过。
- **实际 PC 验收边界：** 已启动当前源码构建的 `morde-pc`。现有测试会话提示云端会话暂时无法续期，点击重试仍未进入可操作会话；因此未能在 PC 入口执行长回复、刷新/重连及无重复展示验收。没有用普通 Web 代测，没有发送新测试消息、修改测试服务器或数据库。
- **状态：** 本地实现与定向验证完成，B06 仍进行中且不勾选；恢复 PC 测试会话后补做客户端验收。B05 高 cap 生产放行仍受 B01–B04 生产门禁及 B06 PC 验收约束；本轮没有调用真实模型/provider。

## 19. B07 明确截断与有限续写（2026-11-28）

- **部分结果边界：** Gateway 只把 adaptive Task 上的已知用量纯文本 `length` 响应识别为可保存部分结果；请求必须是纯文本且不含工具、JSON 模式或图片，响应不得有工具调用。截断文本以 Task response sealer 加密保存；已结算重放从密封记录读取，不再次调用 provider。旧非自适应路径仍返回截断失败。内容过滤、取消、超时、传输错误、未知用量、无效 UTF-8 或不完整工具参数均不伪装成可续写文本。
- **DSH 续写边界：** 仅在 `model_budget_admission` 已授权的 Task 会话监听 DSH `agent/turn-stopping`；来自 Worker/Gateway 的精确 replay marker 与 DSH `max-tokens` 结束原因同时匹配后，才将部分文本留在会话并排队继续。最多两次，每次新 turn 经过现有 `agent/request-budget`、生成新 budget call ID 并绑定新的 DSH checkpoint/call ID。派发前已排队的新用户输入会让 Runtime 跳过续写并优先消费输入；派发后到达的新消息由 delivery-version fence 丢弃旧结果、随后处理输入，但 provider 可能已经收到在途请求。最后一次仍截断时以 `MODEL_OUTPUT_LIMIT` 明确失败。带工具的 DSH 请求不续写，未改变工具授权或 Agent Runtime。
- **通过的验证：**
  - `server`: `go test ./internal/app -count=1` —— 全量应用层通过（31.664s）。覆盖 adaptive 纯文本截断分类、旧路径拒绝、已知用量、工具/过滤失败分类、加密响应 round trip 与 settled replay 不重复派发。
  - `server`: `GOPROXY=off GOSUMDB=off go test ./internal/worker -count=1` —— 全量 Worker 通过（76.592s）；覆盖只接受经过 Gateway 标记的截断响应、映射为 DSH `max-tokens` 和 replay marker，未经标记不得写入。
  - `server`: `CH_TEST_DATABASE_URL='postgres://morde_test@127.0.0.1:55442/postgres?sslmode=disable' GOPROXY=off GOSUMDB=off go test ./internal/store/postgres -run '^TestPostgresTaskModelTruncationPersistsEncryptedPartialAndReplaysWithoutProvider$' -count=1 -v -timeout=10m` —— 本地隔离 PG 通过：部分正文已加密持久化，settled replay 不触发第二次 provider 请求。
  - `runtime/morde-cloud-runtime`: `node --test test/model-budget-admission-runtime.test.mjs` —— 29/29 通过。验证新续写 turn 重新签发预算、续写上下文包含已确认的前文、最多两次、工具请求不续写以及新用户消息抢先处理。
  - `runtime/morde-cloud-runtime`: `npm test` —— 全套 298/298 通过；包含锁定 DSH runtime 与 IPC、checkpoint、恢复和并发回归，并覆盖“续写已入队后进程崩溃、恢复后只处理该续写且不追加通用 recovery prompt”。为修正 8 个旧 IPC 测试夹具传入非法占位 model call ID，本轮将它们改为按 Task/session/sequence 生成合法 ID；运行时实现本身未放宽身份校验。
  - `gofmt -d`（本轮 Go 改动文件）与 `git diff --check` —— 通过。
- **状态与限制：** B07 的本地实现及自动化验证完成，但整体仍未勾选：B05/B06 的依赖门禁与实际 PC 长回复刷新/重连尚未通过。Portal planner bootstrap 已接入但部署关闭，普通及旧 Task cap 没有改变。本轮未调用真实 provider、未写测试门户、未部署或 SSH 修改远端机器。

## 20. P3/P4 阶段门禁复核（2026-11-28）

- **B08 设计决策：** 按附件方案用 `response_mode` 在分类模型前分流，短答不增加模型往返；长答无工具、不创建 Task，复用 route 账本并绑定当前消息/会话。Task-less 长回复先采用单次调用与 4 分钟总时长上限；输出 `length` 时保存部分结果。暂不自动开第二次调用，因为尚无每条回复的累计预算/调用数持久合同；人工追问会形成新消息和新账本调用。此为防止无界叠加预留的范围决策，不宣称已实现附件建议的 Task-less 两次续接。
- **测试环境可达性：** 从开发机对 Portal 根路径及 `/healthz` 直连 HTTPS 443 均在 8 秒超时；测试主机 TCP 22 可建立连接，但 SSH banner exchange 超时，未能登录检查服务。没有远端数据库修改、配置变更、文件传输或部署。因此 B05/B06 真实 PC 刷新/重连门禁仍未通过。
- **B09/B10 状态：** B09 真实试点尚未开始：没有冻结的每类 30 个固定样本、真实 provider 基线/回执、质量与 p50/p95 首字延迟对照、实际 PC 与并发子 Agent 暂停恢复报告；本地 fake provider/runtime 测试不替代这些证据，灰度未开放。B10 未开始：依赖 B09 试点数据、真实供应商计量/费率分布和新的产品合同；不新增周期预算字段，不改变旧合同或新建默认。
- **本轮 provider 与数据边界：** `runtime/morde-cloud-runtime` 全套 298/298 回归通过；没有调用真实模型/provider，也没有在测试门户创建消息或写入数据。模型凭据未输出或写入仓库。

## 21. B08 原会话长回复本地实现与验证（2026-11-28）

- **实现范围：** personal/project 收件 API 接受严格枚举 `short|long_form`，并在消息列表与 reconciliation 投影保留模式；长回复在 Worker 中绕过分类模型，复用当前选定模型的 route 调用账本，要求模型上下文窗与输出上限有已验证值，按完整上下文、模型输出上限和现有账户/项目账本预留计算输出额度。请求只含纯文本和有界会话历史，不提供工具/附件/Task；单次 provider 请求最长 4 分钟。部分 `length` 文本原子保存并以 `partial` 状态展示；未知结果/用量保持待核对，不误报完成。取消标记持久化，取消后的迟到结果不会发布。Portal 提供显式短答/长回复切换、纯文本限制、状态及取消入口。
- **通过的本地验证：**
  - `server`: `go test ./internal/app -count=1` —— 通过（35.620s），覆盖输出 cap 高于 2,048、按上下文收紧、安全文本校验、单次 route 账本调用、部分结果及 settled replay。
  - `server`: `go test ./internal/api -count=1` —— 通过（2.973s），覆盖 personal/project response mode 校验和取消 API。
  - `server`: `go test ./internal/worker -run 'TestConversationRouteWorker' -count=1` —— 通过，覆盖绕过分类模型和取消 provider 调用。
  - `server`: `CH_TEST_DATABASE_URL='postgres://127.0.0.1:55439/morde_test?sslmode=disable' go test ./internal/store/postgres -run 'TestLongForm|TestValidStoredRouteDecisionKeepsLongReplyFields|TestProjectConversationMessageProjectsExplicitResponseMode' -count=1 -v -timeout=10m` —— 临时隔离 PostgreSQL 通过，覆盖 pending/in-flight 取消、迟到结果 fence、部分正文只提交一次、短答拒绝取消以及 personal/project response mode 投影。临时数据库仅在本机。
  - `web/portal`: `npm test -- --run src/app/api.test.ts src/routes/Assistant.test.tsx` —— 364/364 通过；`npm run typecheck` —— 通过。
  - 本轮 B08 涉及 Go 文件 `gofmt` —— 通过。
- **PC 实测与未覆盖：** 当前源码 `make pc` 构建并启动成功；实际 `morde-pc` 已尝试重连，但 15 秒后仍显示“已登录，但云端会话暂时无法续期”，未能进入助手页面。因此尚未从 PC 发送长回复、取消、刷新/重连或核对重复展示。`/healthz` HTTPS 8 秒超时，SSH TCP 可达但 banner exchange 超时。未调用 DeepSeek 或其它真实 provider，未验证真实供应商计量/账单，没有部署或更改测试服务器。Portal 自动测试不代替 PC 实测，也没有用普通 Web 代测 PC；生产 cap 和生产路由未改变。测试环境恢复后须继续实际 PC 验收；生产放行还需 B05–B07 门禁。

## 22. 默认关闭的 Portal admission 接线与最新客户端验证（2026-11-28）

- **B05 本轮实现：** Portal 增加 `models.output_budget_admission_enabled` 配置，默认 false，`deploy/single/portal.yaml` 显式设为 false。启用后才安装版本化预算策略并注入 DSH Worker planner。Worker 在启动 DSH 前按当前 Task 的 Initiator 与模型解析路由；只有输出和上下文能力均有允许的数据来源、数值落在协议支持范围且 exact endpoint/model route 存在策略表中，才发送 `model_budget_admission=true`。未列入策略或能力未验证的路由仍携带原 legacy `max_tokens`；路由解析报错则不启动 DSH，fail closed。策略快照缺项/非法值由配置校验拒绝。
- **能力目录 freshness 修复：** 在模型连接视图、Gateway/Probe/Media endpoint 共用 capacity sanitizer；当前 exact route 对应的 model-catalog 输出与上下文值必须匹配目录版本和值，否则返回 unknown，不向预算资格或 prepare 暴露数据库中的旧值；连接更新时会刷新过期上下文目录。新增 stale output/context 回归证明 resolver 清除旧值、adaptive eligibility 返回 false、prepare 在账本 reservation 前以 unsupported 失败。此处不清除个人显式 override，也不把 unknown 转成默认值。
- **本轮通过的定向验证：** `server`: `go test ./internal/app -count=1` —— 全量 app 测试通过（31.623s）；`go test ./internal/app -run '^TestAdaptiveBudgetRejectsStaleCatalogCapacityFromConnectionResolver$' -count=1` —— 通过，覆盖 stale output/context、Get/endpoint projections、资格拒绝、prepare fail closed 及 stale context 更新刷新；`go test ./cmd/portal -count=1` —— 通过，覆盖 bootstrap opt-in、准备服务缺失拒绝及 adapter 对资格 true/false/error 的透传；`go test ./internal/config -count=1` —— 通过，覆盖默认关闭和启用配置校验；`go test ./internal/worker -run 'TestCloudDSHModelBudget|TestCloudDSH.*BudgetAdmission' -count=1` —— 通过，覆盖 admission 协议与未入选 Task 保持 legacy cap。部署配置未启用，未连接真实 provider。
- **本轮客户端验证：** 根目录 `make pc` 完成当前 Portal、Workbench、Electron renderer 构建并启动实际 `morde-pc`。桌面端 `npm test` 为 297/297 通过。实际 PC 登录态提示“已登录，但云端会话暂时无法续期”；点击重试并等待 15 秒仍显示同一错误，没有进入助手页面，未发送测试消息。门户 `/healthz` HTTPS 经代理 8 秒超时；直连 HTTPS 握手超时；SSH TCP 22 可达但 banner exchange 超时，未登录或修改远端。该连接失败阻断 PC/服务端目录与回复验收，不能由自动化用例替代。
- **状态更新：** B05 的本地 bootstrap 与路由资格门控已实现并通过定向测试，但 B05 checkbox 保持未勾选，因为 B01–B04 发布门禁、真实 provider 计费/质量、长回复 PC 刷新重连和生产端到端仍未通过。B06–B08 的 PC/发布门禁也继续未完成；部署 admission 保持 false，legacy 请求发送上限未改变。未使用提供的 DeepSeek key，没有调用真实 provider、写测试门户、部署、上传文件或修改远端数据库。

## 23. Shadow 上限、指标映射与 B01 失败边界补验（2026-11-28）

- **B03 shadow 修正：** 影子预算计算之前仍使用 legacy `32,768` 协议上限，导致候选计划即使模型能力、上下文和预算都允许更高输出，也会被错误压低。现在使用 `MaxModelOutputTokens` 作为 adaptive shadow ceiling；新增 40,000 token stage goal 用例，断言 `SafeTokens=60,000`、`SendTokens=40,000`、限制维度为 `stage_goal`，同时 live legacy provider cap 仍为 321。Portal metrics allowlist 加入 `content_filtered`，避免该失败类别折叠为 `unknown`。这只修正 shadow 预测和归类，不更改 production provider cap。
- **B01/B04 故障边界补验：** 在 `TestCloudDSHModelBudgetPostgresHTTPFailureEdges` 增加 PG trigger 故障注入：通过真实 `Gateway.Complete` 让 `prepared→dispatched` update 失败，并暂时阻止该次 abandon 删除；断言 ledger 保持 `prepared`、budget plan 未消费、fake provider HTTP 为 0。移除 trigger 后用同一身份重试，ledger settled 且只新增一次 provider 请求。新增 `TestGPT6LunaAdaptiveBudget*` app 用例证明 GPT-6 Luna 最低 reasoning 25,000 与 adaptive plan、Provider cap、ledger reservation 一致；stage goal 或已验证模型上限不足时，在 prepare/ledger 写入和派发前拒绝。
- **并行 Agent 与共享项目补验：** `test/model-budget-admission-runtime.test.mjs` 新增同一 parent Task 通过 `dsh_delegate` 启动两个真实并行 child Agent 的用例：两者有独立 session、cap、plan ID、BudgetCallID/DSH call ID，model request 与各自 prepare digest 匹配，并按对应结果完成 DSH `child_settle`。这是锁定 DSH runtime 的调用隔离/结算协议证据，尚不证明并行 child 调用已同时写入或结算 PG ledger。`TestPostgresConcurrentSharedProjectBudgetAcrossOwnersPreservesLedgerAttribution` 用真实隔离 PostgreSQL 验证不同 owner 对同一项目并发预留一胜一拒，结算后 loser 仅获得剩余额度，overage 的账户/项目/Task/对账归属正确；定向测试连续 `-count=3` 通过。
- **纠正过期缺项：** B01/B04 清单已把 settled replay、transport unknown/no-replay、provider timeout/503、预派发取消与派发后 cancel 标为已有本地 E2E 证据；dispatch trigger 现在注明 Store 层和经 Gateway 的跨层覆盖。真实 adaptive Task compaction PG ledger/结算仍缺；稳定触发它需要在同一 Task 执行期间通过真实消息接收/路由送入 follow-up 并越过 pressure threshold，现有大 E2E harness 没有这段驱动，不能用伪造 prepare frame 替代。Portal 进程 crash、生产 provider/账单及跨 owner 长期公平性仍未通过，故不勾选 B01/B03/B04。
- **通过的验证：**
  - `server`: `GOPROXY=off GOSUMDB=off go test ./... -count=1` —— **全量 Go 包通过**（本轮修改后重跑；需外部隔离数据库的集成测试仍以单独 `CH_TEST_DATABASE_URL` 命令记录为准）。
  - `server`: `go test ./internal/app -run 'TestTaskModelBudgetShadow|TestGPT6LunaAdaptiveBudget' -count=1` —— **通过**；覆盖 >32,768 shadow 计划、live legacy cap 不变及 Luna 25,000 reasoning 边界。
  - `server`: `go test ./cmd/portal -run '^TestModelBudgetShadowMetricsUseBoundedLabelsAndKeepRouteOutOfLabels$' -count=1` —— **通过**，含 `content_filtered` 指标标签断言。
  - 本地隔离 PostgreSQL：`TestCloudDSHModelBudgetPostgresHTTPFailureEdges` 定向组 **通过**（timeout、HTTP 失败、Task cancel 与新增 dispatch transaction failure/retry）；provider 使用 localhost fake HTTP，无真实模型调用。
  - 本地隔离 PostgreSQL：`TestPostgresConcurrentSharedProjectBudgetAcrossOwnersPreservesLedgerAttribution -count=3` —— **3/3 通过**；验证跨 owner 同项目预留竞争、额度重算、结算和超额归属。
  - `runtime/morde-cloud-runtime`: `node --test test/model-budget-admission-runtime.test.mjs` —— **30/30 通过**；全套 `npm test` —— **299/299 通过**；含真实 DSH 同 Task 并行 child Agent 的独立预算身份与 `child_settle` 回归。
  - 改动 Go 文件 `gofmt -d` 与 `git diff --check` —— **通过**。
- **仍未满足的发布/客户端门禁：** B01/B03/B04 维持未勾选；B05–B08 维持未勾选，B09/B10 尚未开始。测试 Portal HTTPS 探测超时；SSH TCP 可达但 banner exchange 超时，未验证账号密码，也没有尝试改远端数据库。当前 PC 仍无法续期测试服务会话，未进入助手功能验收。部署 admission 仍为 false，普通请求 cap 未改变。本轮没有使用提供的 DeepSeek key、调用真实 provider、部署或远端写入。

## 24. 附件方案复核与测试机只读核查（2026-11-28）

- **附件复核结论：** 采纳其短答/原会话长回复/持久化 Task 三条路径、按需而非最大值预留、运行止损、BYOK 未知费率不做金额承诺、无 Task 长回复单次调用和先证据后定默认值。没有把附件中的示例 4,000 token、建议验收阈值或拟议字段当成已验证的产品默认/API。决策依据与对应 B03、B08、B09、B10 边界见文首复核记录。
- **测试服务器状态更新：** 之前第 23 节记载的 HTTPS 探测与 SSH banner 超时，是当时的连接结果；本次复测已恢复访问。测试 Portal `/healthz` 返回 HTTP 200，提供的管理员登录可用；SSH root 认证成功。只读检查显示部署提交为 `fdb5567a`，运行中的 Portal 二进制及 `/opt/morde/deploy/single/portal.yaml` 尚不包含 `output_budget_admission` / `output_budget_shadow` 配置，和当前本地开发分支不同。
- **PC 客户端实际观察：** 运行 `make pc` 构建并启动当前源码 `morde-pc` 后，助手页面可见；“回复长度”开关实际从短答切为长回复，再恢复短答。没有发送消息或创建 Task，因此只证明 PC 入口控件能交互，不证明请求、生成、部分结果、刷新/重连或服务端处理正确。当前客户端连到的测试服务器仍是上面的旧提交。
- **截至第24节的远端写入边界：** 当时没有改测试机数据库、账号密码、服务配置或文件，没有上传代码、部署、重启服务或调用真实模型/provider。管理员设置页仅作只读查看。此前因服务不可达而未完成的 PC 会话/功能验收不能仅凭 `/healthz`、登录成功或控件可切换标为通过；需先让测试服务与当前源码版本匹配，再从当前源码 PC 验证完整流程，并确认请求到达的服务版本。第25节记录了随后发生的候选机 staging 与 dry-run 尝试。

## 25. 测试机候选版本 dry-run 与连接中断（2026-11-28）

- **任务拆分和代理边界：** 主 Agent 负责候选集成、测试机部署门禁、PC 实际验收和本 TODO。`runtime/morde-cloud-runtime/test/model-budget-admission-runtime.test.mjs` 由子 Agent 单独补充同 Task 并行 child Agent 预算身份/settle 测试；定向文件 30/30、runtime 全套 299/299 通过。`server/internal/store/postgres/model_budget_multi_owner_integration_test.go` 由另一子 Agent 单独新增跨 owner 共享项目并发/超额归属测试；本机隔离 PG 连续 3 次通过。compaction PG 端到端子 Agent 只读分析确认现有 harness 缺少运行中 Task follow-up 驱动，因此没有伪造用例或改文件，相关验收仍未完成。
- **远端只读基线：** 开始 staging 前测试机 `/healthz` 返回 200，Portal 服务 active，部署提交为 `fdb5567a809195e7b40aadc89db2685a4be4f20f`，数据库迁移 current/latest 均为 269；线上配置仍是旧版本，未包含本次预算开关。管理员密码正确，无需改数据库或重置密码。
- **候选文件和工具链：** 为避免向 GitHub push，使用提交 `4ed9f1c9f17121e0527ccef0e55afd316e1830c5` 的本地 bundle 在测试机建立隔离 checkout，最终目录为 `/opt/morde-adaptive-budget-4ed9f1c9/`；Go 1.27.1 tarball 只放该目录并通过官方 `.sha256` 校验。没有覆盖 `/opt/morde/src`、`/opt/morde/bin/portal`、线上 YAML 或 systemd 服务文件。最初临时目录在 `/tmp`；发现 `/tmp` 为 3.7 GiB tmpfs 且 100% 后，只移走本任务候选目录到 `/opt` 并释放约 716 MiB，没有清理其它临时文件。后续 dry-run 的 `TMPDIR/GOTMPDIR/GOCACHE/GOMODCACHE`、源码、锁、日志和 smoke record 都指向候选目录。
- **dry-run 尝试结果：** 第一次因隔离 `MORDE_DIR` root-only，`morde` 用户迁移状态进程无法 `chdir`，在 P0 smoke 前失败。修正权限后，第二次只读检查正式数据库迁移状态成功（269/269），但候选 checkout 移动后 origin 仍指向旧 `/tmp`，在取代码阶段失败。修正候选 origin 后，第三次再通过 baseline 迁移只读检查（269/269），进入 `smoke.sh`；确认没有设置 `CH_TEST_DATABASE_URL`，PG 用例按 smoke 设计应为环境例外。远端 Go 首次下载/编译依赖较慢；PG 定向包测试曾运行到约 10 分钟仍无最终用例结果。只读代码复核说明：smoke 将 `go test` 的输出重定向到独立日志，所以控制台安静不等同于挂死；`-timeout 600s` 只限制测试二进制执行，不限制依赖下载/编译；未设置 `CH_TEST_DATABASE_URL` 的 PG 用例需编译后才会 skip。早先 `/tmp` 100% 的尝试报 `T01/T04` 写日志 “No space left on device”；该失败原因是环境存储，不是用例断言。
- **当前未知状态与限制：** P0 smoke 没有返回完整 smoke record；在查看输出时候选 dry-run 进程被暂停，关闭 SSH 会话后，之后 `ssh-keyscan` 无法取到 banner，Portal HTTPS 443 TCP 能建立但 TLS 与 `/healthz` 请求超时。继续任务时再次复测：TCP 22/443 可建立连接，但 `ssh-keyscan -T 10` 没取到 SSH host key/banner；Portal `/healthz` 在 5–12 秒超时，TLS 也在握手阶段超时。当前仍无法确认候选 smoke 子进程是否已退出或服务即时健康状态。依据已记录脚本位置，尚未越过 smoke 阶段，所以没有执行 `migrate up`、二进制/配置切换或 service restart；部署结果标为“未完成/状态待复核”，不能标为通过。未调用 DeepSeek key 或任何真实 provider，也未从 `morde-pc` 发送消息。
- **继续门槛：** 测试机恢复可访问后先只读核验候选进程、Portal 服务和迁移版本；清理时仅处理本任务的 `/opt/morde-adaptive-budget-4ed9f1c9/` 与已上传 bundle。之后须用完整 P0 smoke 记录和成功 dry-run 再决定是否更新测试 Portal；完成更新后仍须从实际 `morde-pc` 验证真实长回复、>2,048 模型输出 token、刷新/重连和账本结算。当前 B01–B08 发布/PC 门禁继续未勾选，B09 仅在推进测试机前置门禁，B10 仍未开始。

## 26. B08 阶段策略预算落账与本轮验证（2026-11-28）

- **预算策略加载边界：** Portal 现在可在 Task admission 和 shadow 均关闭时，单独加载显式配置的 exact route 阶段策略，用于 B08 原会话长回复。该路径只使用确切 provider/endpoint/model 与 `long_form` 策略；配置默认不含策略数值，不会开启 Task planner、shadow sampling 或普通 Task 的 adaptive cap。现有部署 YAML 未配置 `long_form`，因此部署默认仍关闭。
- **`O_safe/O_send` 与签约：** 长回复根据模型最大输出、完整上下文可用量、账户剩余 token 与项目剩余费用计算 `O_safe`，以 `O_send=min(O_goal,O_safe)` 决定本次 provider cap 和输出预留。先尝试按完整候选值原子 prepare；若账户或项目额度竞争导致候选值不可预留，再读取锁定的项目费率和剩余额度、缩小一次并重新签约。低于 `O_min`、能力/预算未知、策略缺失或最终事务复核失败时均不派发。持久化 route ledger 同事务记录策略/能力版本、目标值、各限制值、最终 cap 及精确 reservation；settled replay 沿用原调用身份。项目金额使用整数最小货币单位计算，不借用 token 单位或默认费率。
- **并行拆分与文件边界：** 配置/bootstrap 由子 Agent 限定在 `server/internal/config/config.go`、`config_test.go`、`server/cmd/portal/main.go`、`main_test.go`；长回复策略与能力检查由子 Agent 限定在 `server/internal/app` 长回复文件；主 Agent 负责 route ledger、PostgreSQL limiter、迁移、集成测试及本文档。集成时补充 route 输出计划接口和项目 rate snapshot 复用，未覆盖彼此负责的文件边界。
- **实际 PostgreSQL 证据：** 在仅供本任务使用的本机隔离 PostgreSQL 实例中运行 `CH_TEST_DATABASE_URL=<isolated local PostgreSQL DSN> GOPROXY=off GOSUMDB=off go test ./internal/store/postgres -run 'TestLongFormRoute(OutputBudget|WithoutValidOutputBudget)' -count=1 -v -timeout=5m` —— 通过。用例实际检查账户额度将目标 6,000 缩减为 3,000、项目费用上限缩减输出 cap、无效/缺失策略/低于最小值/预留不匹配在写 ledger 或派发前拒绝，以及数据库拒绝不一致或部分写入的计划字段。另运行 `CH_TEST_DATABASE_URL=<isolated local PostgreSQL DSN> GOPROXY=off GOSUMDB=off go test ./internal/store/postgres -run '^TestRouteModelRejectsGenericLedgerLifecycle$' -count=1 -v -timeout=5m` —— 通过，覆盖既有 short route 对全空长回复计划列的写入兼容性。测试只访问本机隔离库。
- **其余本地验证：** `GOPROXY=off GOSUMDB=off go test ./internal/app -run 'TestGenerateConversationLongReply' -count=1` —— 通过；`GOPROXY=off GOSUMDB=off go test ./internal/config ./cmd/portal -count=1` —— 通过；`GOPROXY=off GOSUMDB=off go test ./... -count=1`（`server` 目录）—— 所有 Go 包通过。全包命令没有设置 `CH_TEST_DATABASE_URL`，因此要求外部数据库的 PG 集成用例未由这条命令覆盖；上列两个目标组是在隔离 PG 上实际执行的。`git diff --check` —— 通过。
- **外部验收与状态：** 本轮测试机 Portal TLS/health 请求超时，SSH 未取得 banner/host key，不能核实候选 dry-run 子进程最终状态。按已知流程尚未进入数据库迁移、二进制/配置替换或服务重启；没有改远端数据库/账号、部署或调用真实 DeepSeek/provider，也没有从 `morde-pc` 发送消息。之前 PC 仅观察到回复长度开关可切换，真实生成、超过 2,048 token、取消、刷新/重连及 ledger 结算仍未验收。B08 本地实现及定向/全包自动化证据已补充，但 B08 及 B05–B08 发布和 PC 门禁不勾选；B01–B04 生产门禁也未通过，B09 dry-run 无完整结果，B10 未开始。恢复测试机后仍须先只读确认进程/服务/迁移状态，再完成 smoke、部署决策和实际 PC 端验收。

## 27. 旧版 00269 迁移兼容修复、冲突核对与验证（2026-11-28）

- **本地冲突状态：** 当时分支 `release` 的 HEAD 为 `9c168e66`，包含已合并的 `194ba92e`；`git ls-files -u` 无未合并索引项，预算迁移与回归测试文件中也没有冲突标记。保留其余正在进行的工作区改动，没有 stash、reset 或覆盖其它文件。
- **P0 smoke 结果：** 候选 `194ba92e` 的 `/tmp/morde-smoke-194ba92e/deploy/single/smoke-record.json` 记录完整结束，8 项通过、0 项失败、2 项预期例外（真实 PC/DSH 门禁）；smoke 进程退出码为 0。该结果只证明候选 smoke，不代表部署或 B09 验收完成。
- **测试机更新中止记录：** 随后的更新路径实际启动了非 dry-run 更新脚本，且此前没有生成 PostgreSQL 备份。`migrate up` 在版本 270 因已发布 00269 schema 缺少 `model_budget_capacity_waits.checkpoint_digest` 而失败；更新脚本记录在切换前中止，未替换 Portal 二进制、同步生产配置或重启服务。失败后只读检查显示迁移版本仍为 269，`checkpoint_digest`、`last_reconciled_at`、owner fairness index 及 00270 deadline table 均未留下，等待记录为 0。没有再对测试机执行写操作。重新尝试更新前，须先备份数据库并完成候选 smoke 与 dry-run。
- **修复决定：** 不改已发布的 00269 migration；在 00270 增加 `ADD COLUMN IF NOT EXISTS` 和缺失 fairness index。旧 waiter 的 digest 只从 task/owner/session/sequence 完全匹配的 `task_agent_sessions`、`task_agent_checkpoints` 以及具有有效 stop receipt digest 的 stopped Agent instance 取值。任何 waiter 缺少精确且 32 字节 checkpoint 证据时，迁移明确失败并由 PostgreSQL 事务回滚；不制造 digest。00270 回滚时保留这项向前兼容的加列/索引修复，以便已升级 schema 回退到 runtime 仍可用的 269 结构。
- **通过的本地隔离 PostgreSQL 验证：** `CH_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:55439/postgres?sslmode=disable' GOPROXY=off GOSUMDB=off go test ./internal/store/postgres -run 'TestPostgresModelBudgetCapacity(WaitReplayPreservesMigratedEpisodeExpiry|DeadlineMigrationRejectsUnmatchedLegacyCheckpoint)$' -count=1 -v -timeout=5m` —— 2/2 通过；正向用例模拟真实已发布 00269 的旧表结构，验证 00270 恢复原 digest、兼容列/索引与 episode deadline；负向用例证明证据不匹配时版本、表、列和 waiter 数据均回滚保留。
- **完整预算等待回归：** 同一隔离实例下 `go test ./internal/store/postgres -run '^TestPostgresModelBudgetCapacity' -count=1 -timeout=10m` —— 通过（17.743s）。`gofmt` 与 `git diff --check` —— 通过。数据库实例仅在本机临时目录运行。
- **状态更新：** 本地迁移兼容修复及预算等待 PG 回归完成，尚未把修复放到测试机或重新部署。B09 保持未完成；实际 PC 的长回复、>2,048 输出 token、取消、刷新/重连、恢复与 provider 账单仍未验收。B01–B08 既有发布/PC 门禁状态不变，B10 未开始；普通生产路由仍默认关闭，未调用真实模型 provider。

## 28. 当前本地冲突核查与 8c 基线候选验证（2026-11-28）

- **本地冲突核查：** 当前仓库 HEAD 为 `8c87122f`，没有进行中的 Git merge，`git ls-files -u` 为空；在源代码和文档（排除 `node_modules`、vendor 与第三方依赖镜像）的精确搜索没有 `<<<<<<<`/`>>>>>>>` 标记。`git diff --check` 与 `git diff --cached --check` 通过。首次检查时 Assistant 页面、对应测试和任务中断体验 TODO 为 `MM`；复核时三者都仅为工作区修改（`M`），这些路径的暂存区与 HEAD 一致（`git diff --cached --quiet` 退出码 0）。当前没有未解决的 merge conflict。
- **新候选：** 隔离 worktree 基于本地 `8c87122f` 创建候选 `663d9250f409b6ed00fae37c63de46a496522a72`，包含 migration 277、测试 setup timeout 修正及 DSH compaction 来源/补丁校验。候选 bundle SHA-256 为 `53331998f133ac9dccb379d1ce9a7e9094b2ad92359fcd23d5bcc4da69c7db16`。未包含当前未提交的 migration 278；此前远端只读基线为 current/latest `277/277`。
- **候选本地验证：** `npm run build:dsh-compaction` 通过；`npm test` 通过（299/299，约 95.8 秒）；`go test ./internal/worker -run '^TestTaskValidationReviewWorkerPersistsGatewayResultWithPostgres$' -count=1 -timeout=10m` 在隔离 worktree 通过（2.127 秒）；`go test ./internal/store/postgres -run '^TestPostgresModelBudgetCapacity' -count=1 -timeout=10m` 在隔离 PostgreSQL 通过（约 18.185 秒）。
- **P0 smoke：** 在候选 worktree 执行 `CH_TEST_DATABASE_URL=<isolated local PostgreSQL DSN> GOPROXY=off GOSUMDB=off bash ./deploy/single/smoke.sh --src "$PWD" --record /tmp/morde-budget-local-smoke-8c87122f/smoke-record.json --timeout 600 --commit 663d9250f409b6ed00fae37c63de46a496522a72` —— 通过（8 pass/0 fail/2 expected exceptions，退出码 0；exception 为必须使用真实 PC/DSH 的 T10/T43）。JSON 记录仅保存在本机 `/tmp/morde-budget-local-smoke-8c87122f/smoke-record.json`；该本机结果不替代远端 smoke。
- **远端阻断：** bundle 传输在 SSH banner 阶段超时，未上传；本轮 `/healthz` 6 秒超时。此前基于旧候选启动的只读 dry-run 会话仍无最终输出，因此其进程/完整结果待复核；没有新执行数据库迁移、替换二进制/配置或重启服务。当前不能宣称新候选通过远端 smoke/dry-run、部署或 `morde-pc` 实测。
- **状态：** B09 保持进行中且未完成；当前候选本地测试证据已补齐，远端恢复后先只读核验旧 dry-run/服务/迁移状态，再传输新候选、运行 smoke 与成功 dry-run，准备数据库备份后才评估测试机更新。真实 PC >2,048 token 输出、取消、刷新/重连、结算及 provider 账单仍未验收；未调用真实 provider。

## 29. 候选代码合入本地 release（2026-11-28）

- **合并结果：** 将 `codex/budget-deploy-candidate-8c87122f`（`663d9250`）以 merge commit `0c906232` 合入本地 `release`。合并无冲突；候选提交现为 `release` 祖先。
- **代码树核对：** 合并前 `git merge-tree --write-tree` 结果与 `release` 第一父提交 tree 一致；实际合并后 `HEAD^{tree}` 与 `HEAD^1^{tree}` 仍相同。候选五个文件在合并前已与 `7acb032c` 内容完全一致，所以本次合并记录了候选提交历史，没有重复改写代码。
- **范围与发布：** 预算基础实现提交 `4ed9f1c9` 已是 `release` 祖先。本次只在本地合入候选分支，没有 push。`release` 相对 `origin/release` ahead 2（候选提交与 merge commit）；`origin/release` 原先已有相同代码树内容。
- **验证与状态：** 合并前后候选文件树一致，`git diff --check HEAD^1 HEAD` 通过；既有 P0 smoke 8/0/2、DSH/runtime 299/299、T22 Worker 与 PostgreSQL 定向验证仍对应候选代码。本次没有因 tree 未变而重跑测试。B09 仍未完成：测试机 health/SSH 超时，未部署或执行真实 PC/provider 验收。


## 30. 当前 release 集成与本地复验（2026-11-28）

- **release 与 worktree：** 本次 TODO 更新提交前最终核对时，HEAD 为 `release@ee618bed`，`origin/release` 指向相同提交；该基线之后的提交 `ee618bed` 只增加终态诊断投递回归等非预算变更；对比 `0f83fab0..ee618bed`，预算 runtime/服务端实现文件无差异。候选 `663d9250` 经 `0c906232` 合入且是 `release` 祖先；部署测试分支 `4ed9f1c9` 也已是 `release` 祖先。两条 migration-compat 分支是独立快照历史，预算相关迁移（00270、00276）、容量等待实现和集成测试与 release 树一致，其小验收机器 2 分钟迁移测试超时改动也已存在于 release。`m04-06-source-locations`、`m06-03-result-event-regression`、`test-handshake-diagnostic` 三个分支的补丁均有 release 等价提交（`git cherry` 标记为已含）。截图所示 7 个 Codex worktree 均已核对后删除，没有遗漏待合入的预算改动；分支引用保留。隐藏 detached worktree 未在本次清理范围内；release 工作区的任务中断/输出捕获等其他未提交改动均保留，未纳入预算改动。
- **当前源码验证：** `runtime/morde-cloud-runtime` 执行 `npm test`，自动重建 DSH compaction 后 **302/302 通过**（102.6 秒）；`sh scripts/verify-dsh-upstream.sh` 确认锁定 DSH 来源 `fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159` 及 release 输出预算补丁，compaction 的 6 个文件 SHA-256 全部通过；`server` 定向 Worker 整链 `TestCloudDSHModelBudgetPostgresHTTPUsesOneSignedCapAndSettlesOnce` 通过；`TestPostgresModelBudgetCapacity` 组在本机隔离 PostgreSQL 通过（18.478 秒）；`git diff --check` 与 staged diff check 通过；这些自动化结果覆盖当前 release 的预算实现文件，执行工作区另含未提交的任务中断/输出捕获改动。
- **测试环境校正：** 首次重跑时旧 Unix socket 路径已不存在，PG 用例无法连接；Worker 命令未设置 `CH_TEST_DATABASE_URL` 时相应用例被跳过。改用当前本机 PostgreSQL TCP 连接后重跑，两项均通过。首次失败/跳过不计入成功证据。
- **远端边界：** 测试 Portal `/healthz` 请求仍超时；之前启动的只读 dry-run 进程句柄仍无输出，不能确认其最终状态。没有登录或改远端数据库、上传文件、部署、迁移或重启服务，也没有调用真实 DeepSeek/provider。因服务端不可达，B09 的远端 smoke/dry-run、真实 PC 长回复超过 2,048 模型输出 token、刷新/重连、取消/结算及质量/延迟基线仍待验收；B01–B09 生产/客户端门禁不得勾选，B10 仍未开始。

## 31. B08 长回复补验与远端连通性复查（2026-11-28）

- **本地 B08 定向复验：** 在 `release` 工作区（预算相关实现文件与 `origin/release@ee618bed` 一致）执行 `go test ./internal/app -run '^TestGenerateConversationLongReply' -count=1 -v` —— 6 项通过，覆盖版本化阶段目标、按账户余额缩 cap、低于最低额度拒绝、缺策略拒绝、上下文不足拒绝和未验证容量拒绝；`go test ./internal/worker -run '^TestConversationRouteWorker' -count=1 -v` —— 通过，覆盖显式长回复不经分类模型及取消传播；`CH_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:55439/postgres?sslmode=disable' go test ./internal/store/postgres -run 'TestLongForm|TestValidStoredRouteDecisionKeepsLongReplyFields|TestProjectConversationMessageProjectsExplicitResponseMode' -count=1 -timeout=10m -v` —— 通过，覆盖 durable cancel、部分结果仅提交一次、账户/项目余额约束、缺失/不足/不一致计划 fail closed 及消息模式投影；`go test ./internal/api -count=1` —— 通过。数据库测试使用本机临时 PostgreSQL database。
- **当前 release P0 smoke：** 从提交 `d323f5b0` 的干净 `git archive` 在 `/private/tmp` 临时目录运行 `deploy/single/smoke.sh`，为 runtime 依赖链接本机已安装的同锁文件 `node_modules` 后，T01/T04/T06/T13/T19/T22/T26/T36 **8 项通过、0 失败**；T10（真实 PC+模型）和 T43（真实 PC+DSH）为脚本声明的 **2 项预期例外**。原始 JSON 记录 `/tmp/morde-budget-release-smoke-final-with-deps.json`。首次无依赖归档运行时 T22 因缺少 `@deepseek-ai/cordis` 失败；依赖安装/版本校验后单测与整组均通过，该失败不计作代码缺陷。
- **远端再次探测：** 代理与 `--noproxy '*'` 直连 `/healthz` 都在 TLS 阶段超时（HTTP 000）；直连 HTTP 80 等待 6 秒无响应；TCP 22 可连接但 5 秒内没有 SSH banner。此前只读 dry-run 会话句柄仍处于运行态，重复轮询仍无输出。没有尝试密码认证、数据库修改、上传、部署、迁移或重启。
- **发布安全与未完成项：** `deploy/single/README.md` 要求先按完整流程停止所有 Portal/Host/Sandbox 写入者与 Compose 服务、创建并校验包含 PostgreSQL/WAL/Workspace/Site/named volumes 的受保护离线备份，再运行候选迁移与安装；失败恢复须保留并验证 rollback bundle。当前主机不可访问，无法证明这些先决条件，故不能执行部署。实际 `morde-pc` 仍受服务续期失败阻断，未发送长回复或验证 >2,048 模型输出 token、取消、刷新/重连及供应商用量。B08 本地代码门禁保持已验证，B01–B09 的生产/PC 验收继续未完成，B10 依赖真实试点数据且未开始。

## 32. B08 请求类型缺陷修复及真实模型核验（2026-11-28）

- **缺陷与修复：** 在隔离干净的 release 快照上新增真实 Gateway 长回复探针时，发现 `ConversationLongReplyModelRequest` 创建的历史和当前 `ModelInput` 都没有设置 `Type="message"`。Responses 编码器因此在本地 `client_validation` 阶段拒绝，provider 尚未收到请求，而 route ledger 将结果安全地保留为未知。已为两类输入补上类型，并扩展单测覆盖当前消息及历史消息类型；这说明先前长回复失败不是 DeepSeek 的 2,048-token 上限拒绝。
- **本地回归：** 在 `server` 目录运行 `go test ./internal/app -run '^(TestConversationLongReplyOutputTokensUsesVerifiedModelLimit|TestConversationLongReplyOutputTokensRespectsInputContext|TestGenerateConversationLongReply.*)$' -count=1` —— 通过；覆盖消息类型、上下文/预算构造及已有 6 个长回复路由用例。
- **真实 provider：** 在 `/private/tmp` 干净 release 源码副本（应用上述两处修复）运行 opt-in `TestDeepSeekFlashLongFormUsesAdaptiveStagePlanAndProviderUsage`，由 Morde `ModelGateway` 请求 DeepSeek `deepseek-flash`。用 4,096-token `long_form` 目标发出单次请求，provider 报告 input **216**、output **4,096** tokens，输出达到 cap 并以 `length` 标记部分结果，正文 **6,047 bytes**；用时 **21.48 秒**。这验证了单次 Gateway/provider 请求可使用并实际消耗高于 2,048 的输出额度。探针使用内存 route-store stub，不证明 PostgreSQL 持久化/账单结算、PC 展示、刷新/重连或生产部署；这次 provider 调用不构成全面质量或延迟基线。
- **PC 当前源码尝试：** 执行 `make pc` 后，确认本地 Electron 主进程 PID `54123` 和 `morde-pc` 窗口已启动，辅助功能树显示助手输入区与“短答”模式。没有从该客户端发送新消息：测试 Portal/SSH 不可达，当前服务无法完成会话续期；因此没有声称 PC 实际生成、展示长回复或完成刷新/重连验收。桌面 CUA 状态读取两次超时，使用 macOS System Events 只读确认当前窗口和控件。
- **密钥与远端边界：** DeepSeek key 仅作为该次 opt-in 测试的进程环境变量使用，未保存至文件或仓库。测试 Portal HTTPS/TLS 与 SSH banner 探测仍超时；本次没有登录、上传、迁移、数据库修改、部署或重启服务。B08 PC/生产验收和 B09 试点门禁保持未完成，B10 未开始。

## 33. 测试机连通恢复、PC 状态及安全发布门复核（2026-11-28）

- **本地 release 与 worktree：** 开始本轮复核时分支为 `release@bdb34b0d`，预算候选 `663d9250` 是其祖先；`origin/release` 在 `ee618bed`，本地 ahead 7，本轮没有 push。截图中的 7 个 worktree 已不存在，相关分支引用保留。另有 11 个与截图无关的 detached worktree，未触碰。工作区现有 Server Worker、Assistant 页面与设计文档的未提交改动均属于其他任务，本轮未暂存或覆盖。
- **测试机连通性：** 重新探测 `https://portal.120.25.178.93.sslip.io/healthz` 返回 HTTP 200（TLS 0.132 秒、总计 0.151 秒），HTTP 80 返回 308。之前的只读 SSH 清点确认 Portal、Docker、PostgreSQL 与 Sandbox 服务在运行，候选 staging 仍是旧提交；线上 Portal YAML 没有 `long_form` 阶段策略配置，先前 remote dry-run 停在 `CH_TEST_DATABASE_URL` 未设置。旧备份目录里没有满足当前要求的 v4 bundle。上述只读结果不证明预算候选已部署。
- **v4 备份前置条件：** `/etc/cloudhost/cloudhost.env` 缺少 `CH_POSTGRES_WAL_ARCHIVE_DIR` 与 `CH_WORKSPACE_SYNC_CURRENT_FILES_DIR`；当前 Workspace 存储位于 loop 挂载 `/srv/morde-acceptance`，无法证明它是部署 README 要求的独立、固定容量且非 thin-provisioned 数据文件系统。现有约 74 MB 旧备份不包含完整 WAL/Workspace/Site/named-volume v4 归档，不能作为本次升级回滚包。故本轮只读复核，没有停服务、修改 env/数据库、上传 bundle、迁移、替换二进制或重启 Portal。
- **实际 PC 检查：** `make pc` 退出码 0；Electron 窗口在内置 CUA 中显示当前源码 `/assistant` 页面、会话与长回复开关。页面里可见的模型失败状态是已有会话记录；本轮没有发送新消息或改写对话。当前没有把候选部署至测试机，因此真实长回复、超过 2,048 输出 token、刷新/重连、取消与账本结算仍未从 PC 验收。
- **状态：** B08 本地代码及 Gateway/provider 单次 4,096 输出证据保持通过；生产与 PC 验收不勾选。B09 继续未完成；先补齐并验证符合 README 的存储/配置与完整 v4 离线备份，再运行与当前 release 对应的远端 smoke/dry-run 和升级流程。B01–B09 生产/客户端门禁尚未全部通过，B10 未开始。

## 34. 当前 release 复验与数据盘门槛（2026-11-28）

- **当前源码回归及目标构建：** 在 `release` 文档提交基线 `947d6318` 对应的源码执行 `GOPROXY=off GOSUMDB=off go test ./internal/app -count=1`（33.972 秒）、`go test ./internal/worker -run '^TestConversationRouteWorker' -count=1`（通过）及 `go test ./internal/api -count=1`（通过）；本机隔离 PostgreSQL 上 `go test ./internal/store/postgres -run 'TestLongForm|TestValidStoredRouteDecisionKeepsLongReplyFields|TestProjectConversationMessageProjectsExplicitResponseMode' -count=1 -timeout=10m` 通过。干净归档中 `npm test` 重建 DSH compaction 后 **302/302 通过**（105.7 秒）。使用 Go 1.27.1 在同一干净归档执行 `GOOS=linux GOARCH=amd64 GOPROXY=off GOSUMDB=off go build -o /private/tmp/morde-portal-budget-947d6318 ./cmd/portal`，`file` 确认产物为静态链接 Linux x86-64 ELF。对比 `947d6318..HEAD` 仅有文档路径差异，预算源码与该构建基线一致。未提交的其他 Worker/UI 改动仍保留在工作区，Smoke 则使用干净归档。
- **当前 release P0 smoke：** 从 `947d6318` 建立本机干净归档，先运行 `npm run build:dsh-compaction` 生成 DSH compaction 产物，再运行 `deploy/single/smoke.sh`；T01/T04/T06/T13/T19/T22/T26/T36 **8 项通过、0 失败**，T10（真实 PC+模型）和 T43（真实 PC+DSH）为 **2 项预期例外**，退出码 0。记录保存在 `/tmp/morde-budget-smoke-release-947d6318-after-build.json`。未构建 compaction 的首次归档运行因缺生成文件使 T22 无法启动；补齐官方构建步骤后重跑通过，该结果不计代码缺陷。
- **远端存储核实：** SSH 只读 `lsblk` 显示只有 100 GiB 系统盘 `/dev/vda3`；Workspace 的 `/dev/loop0` 是 20 GiB ext4 loop 挂载，backing file `/var/lib/morde/workspace-sync.img` 位于 `/dev/vda3`，镜像 apparent size 为 20 GiB、实际分配约 135 MiB。它是系统盘上的稀疏文件，不能满足独立固定容量数据盘要求。远端没有创建或挂载云盘，也没有改动系统。
- **需要的下一项基础设施：** 只增加 Workspace 数据盘不足以完成 v4 备份门禁。当前系统盘同时承载 PGDATA/系统；按部署规则，Workspace 需要独立固定容量文件系统，WAL 归档需位于 Workspace 文件系统之外，v4 备份目标不能用同一系统分区上的另一个目录冒充独立备份。建议在 `cn-shenzhen` 同可用区挂载两块 20 GiB ESSD PL0：一块专用于 Workspace，一块专用于 PostgreSQL WAL 归档与 v4 备份目录；两块盘分别形成独立文件系统。公开参考费率按 40 GiB 合计约 ¥20/月，按量约 ¥1.008/天，最终以 ECS 控制台报价为准。[阿里云 ECS 块存储计费说明](https://help.aliyun.com/zh/ecs/block-storage-devices)说明 PL0 容量计费与参考价，[阿里云价格表](https://help.aliyun.com/zh/emr/emr-serverless-starrocks/product-overview/billable-items/)列出深圳地域 PL0 费率。WAL 归档没有自动保留/清理策略，归档盘需监控容量并制定清理流程。本任务尚未获新增付费资源授权，因此不创建磁盘；v4 备份/部署门槛仍未完成。

## 35. 截图 worktree 与 release 覆盖核对（2026-11-28）

- **worktree 清理状态：** 当前 `git worktree list` 中不存在截图列出的 Codex worktree；只保留主 `/Users/zhongxuan_li/Desktop/morde` 工作区及 11 个截图之外的 `/private/tmp/morde-*` detached worktree。未删除这些其他任务的 worktree。对应本地分支引用保留。
- **release 覆盖证据：** `codex/budget-deploy-candidate-8c87122f`、`codex/budget-test-deploy` 和 `main` 均没有比当前 `release` 多出的提交。`m04-06-source-locations`、`m06-03-result-event-regression`、`test-handshake-diagnostic-20261126` 的提交经 `git cherry release <branch>` 判定为等价补丁已包含。
- **迁移兼容分支：** 两个 `budget-migration-compat-*` 分支与 `release` 没有共同历史，整分支合并会带入整棵不相关快照，故没有合并快照。逐文件核对的 00269/00270/00272 迁移、预算容量等待 Store 与对应 PostgreSQL 回归测试已与 `release` 相同；小型验收机测试差异仅为注释。没有发现需要额外移植的预算迁移代码。
- **工作区保护：** 核对时 `release` 与 `origin/release` 同为 `b363bd38`。没有改动其它任务尚未提交的 `task_step_output_capture.go` 和对应 PostgreSQL 集成测试。本次分支审计没有触发代码合并或 worktree 删除操作。

## 36. 测试机存储和 v4 备份前置条件复核（2026-11-28）

- **只读主机核查：** Portal `/healthz` 返回 HTTP 200。SSH 只读 `lsblk` 显示只有 100 GiB `/dev/vda` 系统盘；`/` 在 `/dev/vda3`，约 33 GiB 可用。Workspace 现挂载为 20 GiB `/dev/loop0`，backing file `/var/lib/morde/workspace-sync.img` 位于 `/dev/vda3`，文件 apparent size 20 GiB、实际分配约 135 MiB。它仍是系统盘上的稀疏 loop 文件，不满足 Workspace 独立固定容量文件系统要求。
- **数据与环境现状：** 最新只读复核显示 PGDATA 约 593 MiB、Docker named-volume 目录约 3.9 GiB、Site 约 12 KiB，均在系统盘；Workspace loop 内容约 2.3 MiB。`/etc/cloudhost/cloudhost.env` 缺少 `CH_POSTGRES_WAL_ARCHIVE_DIR`、`CH_WORKSPACE_SYNC_CURRENT_FILES_DIR` 和新 Workspace data-root 配置。`/var/backups/morde` 约 75 MiB，现有条目是旧 dump/Portal 归档，没有 v4 bundle 目录；不能作为 v4 备份证据。
- **存储布局决策：** 部署安装脚本会校验 Workspace mount 的文件系统设备与 `/`、Docker、Site、PGDATA 均不同；WAL 归档还必须与 Workspace mount 不同。根分区仅有 33 GiB 可用且承载 PGDATA，按 README 不能把其中另一个目录视作磁盘隔离或离线备份。当前最低建议是 Workspace 20 GiB 独立盘 + WAL/v4 备份 20 GiB 独立盘；两盘参考总价约 ¥20/月或按量 ¥1.008/天，实际价格需控制台确认。系统盘现有空闲量可承载当前 `verify` 临时解包，但不替代独立 v4 bundle 目标。
- **未执行项：** 本轮仅作 HTTP、SSH、磁盘、环境变量名及目录大小的只读核查；没有创建/挂载云盘、改环境变量或数据库、停止服务、上传候选、执行迁移、替换二进制或重启服务。早前仅询问 20 GiB Workspace 盘；由于发现 v4 备份/WAL 还需独立存储，该单盘方案尚未获批且不足以继续部署，需先确认更新后的资源方案。

## 37. 当前 release 工作树、预算复验与整链边界（2026-11-28）

- **release 与本地冲突：** 本轮核对时分支为 `release@38f610ce`，`origin/release` 同为 `38f610ce`；预算审计提交 `84e055f1` 已是远端 release 的祖先。`git ls-files -u` 为空，没有未解决的索引冲突。`git diff --check` 通过。工作区另有 `cloud_dsh_session_tree.go` 的其他任务改动，未触碰或纳入本次 TODO/预算测试改动。
- **截图分支与 worktree：** 截图中 `codex/budget-deploy-candidate-8c87122f`、`codex/budget-test-deploy` 与 `main` 均为 `release` 祖先；`m04-06-source-locations`、`m06-03-result-event-regression`、`test-handshake-diagnostic-20261126` 的补丁均由 `git cherry release <branch>` 标记为已等价包含。两条 `budget-migration-compat-*` 是无共同历史的快照分支，但 00269/00270/00272、容量等待实现及对应测试文件与 `release` 相同；整分支合并会引入大量无关删除。截图里的 worktree 已不在 `git worktree list`；仅保留 11 个截图以外的 `/private/tmp/morde-*` detached worktree，未删除其他任务工作树或分支引用。
- **当前预算验证：** `runtime/morde-cloud-runtime` 的 `npm test` —— **302/302 通过**（93.8 秒）；`npm run build:dsh-compaction` 与 `sh scripts/verify-dsh-upstream.sh` 通过，后者确认锁定 DSH 来源 `fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159` 及 Morde request-budget patch。Node 定向 `opt-in Task compaction` 用例 **1/1 通过**。`server`: `go test ./internal/app -run '^TestTaskModelOutputBudgetStageUsesSummaryForCompaction$' -count=1` 通过；`go test ./internal/worker -run '^TestCloudDSHModelBudget' -count=1 -timeout=10m` 通过；隔离本机 PostgreSQL 上 `go test ./internal/store/postgres -run '^TestPostgresAdaptiveCompactionBudgetPlannerUsesSummaryStageAndSettlesOneOutputCap$' -count=1 -timeout=10m` 通过。
- **真实 DSH stale-checkpoint 门禁：** `TestCloudDSHModelBudgetPostgresHTTPRejectsStaleTaskCheckpointBeforeProviderDispatch` 通过。它使用真实 Node DSH 与 PostgreSQL：当 routed followup 令 Task/session checkpoint 前进到预算 prepare/final call 所依据的序号之后，Gateway 按 `task model root checkpoint is not committed` 拒绝该派发，localhost fake provider 收到 **0** 个 HTTP 请求。它证明 stale checkpoint fail closed；不证明 DSH compaction `summary` stage 已由同一次整链成功结算。
- **B01 状态与边界：** 当前证据分别覆盖 DSH 真实 compaction prepare（runtime）、PostgreSQL summary stage 账本结算（planner/PG）和真实 DSH stale-checkpoint no-dispatch；尚无“真实 DSH compaction summary → PostgreSQL prepare/settlement → Gateway → fake HTTP provider”同一次成功整链证据，因此 B01 仍进行中，不勾选。B01–B09 生产/PC 验收仍未完成，B10 未开始。没有使用 DeepSeek API key、连接测试服务器、SSH、部署或修改远端；预算 admission 仍默认关闭。

## 38. 重复压缩整链回归与 worktree 复核（2026-11-28）

- **checkpoint 顺序修复：** DSH `shadowedRange` 的 `start/end` 是当前对话 surface 的端点，不保证按事件序号递增；被压缩序号列表按 surface 顺序给出。此前 checkpoint 校验按数值区间判断，会拒绝前一次压缩产生的新替换节点与后续旧事件共同构成的范围。本轮改为跟踪活动 surface 顺序，校验 `shadowedSeqs` 是连续活动节点，并在 system、compaction、tool-result 替换时同步更新 projection。`TestCloudDSHCheckpointHistoryAcceptsRepeatedCompactionWithNonMonotonicSurfaceRange` 固定覆盖第二次压缩 `[7,4]` 的倒置数值端点与活动 surface 结果。
- **真实 DSH + PostgreSQL + fake HTTP 整链：** `TestCloudDSHModelBudgetPostgresHTTPUsesTaskHermesCompactionSummaryStage` 在本机隔离 PostgreSQL 与真实 Node DSH Runtime 中运行 100 条运行中 follow-up，确认发生至少两次 compaction；每个 compaction prepare 使用 `summary` stage，计划 cap 与已消费预留一致，ledger 状态 settled、provider usage 为 `8/6`，localhost fake HTTP 收到对应 summary 输出 cap。测试同时确认 follow-up 已应用，普通消息与 compaction 计划各自按其已结算/取消状态核对。此结果证明本地 fake-provider 整链，不代表生产 Portal、真实模型计费或 PC 验收。
- **本轮验证命令：** `runtime/morde-cloud-runtime` 的 `npm test`（先执行 `build:dsh-compaction`）—— **308/308 通过**；Node 定向 `test/hermes-compaction-window.test.mjs test/model-budget-admission-runtime.test.mjs` —— **42/42 通过**。`server` 中 adaptive 上下文拒绝/summary stage app 单测、Worker typed-denial/admission/compaction-purpose 协议测试、checkpoint history 测试及 `TestPostgresAdaptiveCompactionBudgetPlannerUsesSummaryStageAndSettlesOneOutputCap` 均通过。真实 DSH 整链命令 `CH_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:55439/postgres?sslmode=disable' GOPROXY=off GOSUMDB=off go test ./internal/worker -run '^TestCloudDSHModelBudgetPostgresHTTPUsesTaskHermesCompactionSummaryStage$' -count=1 -v -timeout=5m` —— **通过，102 秒**。`git diff --check` 通过。
- **B01 状态：** 本地重复压缩的 DSH→PostgreSQL prepare/ledger settlement→fake HTTP dispatch 子门禁现已通过；B01 仍不勾选，生产 planner 维持默认关闭。真实 provider 与计费、Portal 进程 crash、线上策略加载以及 PC 取消/刷新/重连等门禁仍未验。
- **截图分支与 worktree：** 截图显示的是本地分支列表，不是当前登记的 worktree 路径。已核对可见分支：预算候选及 migration 相关变更已有 release 等价内容，`budget-test-deploy` 是旧快照，不应整支合并；其他可见小分支的补丁已在 release。当前 `git worktree list` 仅有主工作树及 9 个截图之外的 detached `/private/tmp/morde-*`；这 9 个都含未提交修改或本地 smoke 记录，属于其他任务，未合并、未删除，以免丢失数据。截图所示 worktree 已不存在，因此没有额外的 worktree 可供删除；本轮没有改写这些分支引用。
- **release 状态：** 本轮预算与 checkpoint 安全修正已提交至 `release`，代码提交为 `3ecaf6b0`；本 TODO 验证记录也已提交。当前本地 `release` 尚未 push。iOS 验收改动和临时 scheme 文件未纳入本轮提交。

## 39. P0 发布门禁修复、当前源码 PC 入口与远端状态（2026-11-28）

- **Smoke 正则缺陷修复：** `deploy/single/smoke.sh` 原先通过在命令替换内临时赋值 `IFS` 拼接 `-run` 正则；Bash 在数组展开时仍使用外层分隔符，多个测试名被拼成带空格的单个模式，真实 `go test` 没有执行这些测试，烟测因此报告 T04/T06/T22 缺失。现改为显式逐项用 `|` 拼接；`TestSmokeGateAccounting` 的假 `go` 若收到含空格模式即失败，防止回归。此前该假执行器会把空格也拆成测试名，未能发现缺陷。
- **P0 smoke 验证：** 使用 `release@d6c4c794` 干净源码快照、锁定 Node 依赖和 `build:dsh-compaction` 生成物，在本机隔离 PostgreSQL `127.0.0.1:55439` 上运行完整 `deploy/single/smoke.sh`：T01、T04、T06、T13、T19、T22、T26、T36 **8 pass / 0 fail**；T10（真实 PC+模型）和 T43（真实 PC+DSH）是 **2 项预期例外**，退出码 0。记录位于 `/tmp/morde-budget-smoke-d6c4c794-postgres-runtime.json`。`go test ./internal/app -run '^TestSmokeGateAccounting$' -count=1` 通过。没有测试远端生产服务。
- **当前源码 PC 实测：** `make pc` 的 Portal、Workbench 与 Electron 启动构建通过；在实际 `morde-pc` 内嵌 Assistant 页面将“回复长度”从短答切至长回复，再恢复短答，AX 状态依次为 `Value: 0 → 1 → 0`，页脚随之显示“长回复/短答”。未发送消息、未创建 Task；因此只证明当前源码 PC 入口可用，不证明服务端请求、模型输出、取消或断线重连交付。
- **测试机只读复核：** `/healthz` 返回 HTTP 200，`morde-portal.service` active，Docker PostgreSQL healthy。`.deployed-head` 为 `b47f009c`，该对象不在本地浅克隆且未找到部署源码 checkout，故无法证明远端运行的是当前 release。物理盘只有 100 GiB `/dev/vda`；`/srv/morde-acceptance` 的 20 GiB `/dev/loop0` 仍位于系统盘。`CH_POSTGRES_WAL_ARCHIVE_DIR`、`CH_WORKSPACE_SYNC_CURRENT_FILES_DIR`、`CH_WORKSPACE_SYNC_DATA_ROOT_DIR` 缺失，`/var/backups/morde` 无 v4 bundle。没有上传或部署文件，没有改配置、数据库、密码，没有停止或重启服务。
- **发布阻塞：** 当前仍缺与系统/Docker/PGDATA 分离的固定容量 Workspace 文件系统、独立 WAL/v4 备份目标及真实 v4 备份/恢复演练；测试机现有 loop mount 和旧备份不满足要求。满足此前建议至少需要 Workspace 与 WAL/备份各一块 20 GiB 独立盘；新增付费资源未获授权，因此不改远端部署或启用 admission。
- **worktree 与用户工作区保护：** 截图列出的分支没有对应已登记 worktree，相关预算提交在 release 已有等价内容；其余 9 个 `/private/tmp/morde-*` detached worktree 均含未提交或未跟踪内容，全部保留。主工作区同时存在其他任务的未提交 iOS、Runtime、Task、Worker/Web Research 变更；未纳入本轮提交。为避免这些改动影响 HEAD 验证，P0 smoke 在干净 `release@d6c4c794` 快照运行。
- **状态结论：** B01–B09 仍未完成各自的真实 provider、PC 交付/恢复、生产部署或灰度门禁；B10 仍依赖 B09 试点数据与产品/费率决策。B01 planner 继续默认关闭。预算及 P0 smoke 修正保留在本地 `release`，未 push。

## 40. 阶段标记更新、跨 owner 公平性与 worktree 复核（2026-11-28）

- **B03 阶段语义收敛：** 普通 Task 根文本调用没有可信的“短答/长答”意图字段，因此新增的 `root_text_only` 只表示受信任的根 Agent 无工具结构，不代表回答长度或语义类型。策略快照与 admission/shadow 识别均使用该中性阶段；不解析 prompt 猜测，不扩大未知阶段额度。`summary`、`long_form`、`text_continuation` 尚无完整 Task 侧可信阶段信息，B03 仍未完成。
- **本地验证：** `server` 目录运行 `go test ./internal/app -count=1`，通过（约 35.4 秒），覆盖 app 包及新增的 `root_text_only` 计划/影子观察断言。另外在本机隔离 PostgreSQL 上运行 `go test ./internal/store/postgres -run '^(TestPostgresAdaptiveBudgetPlannerGatewayAndOpenAICompatibleHTTPUseOneOutputCap|TestPostgresAdaptiveBudgetPreparedCommitHasNoDispatchBeforeGatewayCall|TestPostgresAdaptiveBudgetSettledReplayDoesNotDispatchTwice|TestPostgresAdaptiveBudgetAmbiguousTransportFailureIsUnknownAndCannotReplay|TestPostgresAdaptiveCompactionBudgetPlannerUsesSummaryStageAndSettlesOneOutputCap)$' -count=1 -timeout=5m` —— 5/5 通过（4.312 秒）。阶段更名后复跑 `go test ./internal/worker -run '^TestCloudDSHModelBudgetPostgresHTTPUsesTaskHermesCompactionSummaryStage$' -count=1 -v -timeout=5m` —— 通过（101.58 秒），真实锁定 DSH Task 发生重复 compaction 并以 `summary` stage 经 PG、Gateway、fake HTTP 完成落账。B03 的生产 shadow 仍关闭；无线上样本、请求流量离线对照或答案质量验收，不改变生产发送上限。
- **B01 Portal 进程崩溃恢复：** `TestPortalProcessCrashMarksDispatchedModelCallUnknownAndDoesNotReplay` 在 Linux 隔离环境启动由当前源码构建的 Portal 二进制和本机 PostgreSQL。假 CONNECT proxy 在 ledger 已提交 `dispatched` 后阻塞且不返回隧道成功，断言 Portal 收到 SIGKILL 前 provider HTTP=0；重启 Portal 后 reconciler 将旧 ledger 收敛为 `unknown`，相同调用键重放被拒，仍只有一次 CONNECT、provider HTTP=0。运行 `/tmp/worker.test -test.run '^TestPortalProcessCrashMarksDispatchedModelCallUnknownAndDoesNotReplay$' -test.count=1 -test.v`，定向用例通过（1/1，0.74 秒）；隔离 PostgreSQL 使用 Docker `--network none` 与 tmpfs 数据目录，无宿主端口发布，测试后已删除容器和临时目录。该测试走实际 Portal API/模型 ledger，但没有启动 CloudDSH Task plan；不代表测试服务器/生产 Portal、真实 provider 计费或部署验收，B01 仍不勾选。
- **B05 DeepSeek Gateway 实测：** 新增仅在 `MORDE_TEST_DEEPSEEK_KEY` 显式设置时运行的 `TestDeepSeekFlashDialogueThroughMordeModelGateway/long_output_above_legacy_cap`。真实 `deepseek-flash` 经 Morde ModelGateway 收到 `MaxOutputTokens=4,096`，返回 `output_tokens=3,359`、正文 `14,783` 字符、`stop_reason=stop`；定向用例通过。该用例不建立 adaptive Task plan/PG reservation，也未经过 DSH 或 PC，只证明当前 ModelGateway/provider 通路可返回超过 2,048 token 的完整正文；测试 key 只通过进程环境传入，未写入仓库。
- **B09 当前源码 PC 实测：** 在实际 `morde-pc` 的新旁聊中开启“长回复”，提交“至少 3,500 个汉字”的中文技术长文请求。界面先显示“长回复正在生成”，随后变为“刚才没能处理你的消息；暂时无法确定具体原因，原消息已保留”。测试 Portal 同期日志显示消息 POST 返回 202，随后 `conversation_route_worker` 记录 `failure_reason=model_unavailable`、`failure_category=unknown`；没有可验证的模型正文或 provider 用量证据，因此不能算 >2,048 token 成功。未点击“重新处理”，避免对结果未知的调用重放。当前服务未部署本次代码；此结果记录为 PC/Portal 基线失败，不代表新实现失败或通过，B09 仍未完成。
- **B04 跨 owner 公平性：** `TestPostgresSharedProjectCapacityWaitMakesProgressUnderOtherOwnerTraffic` 在隔离 PostgreSQL 中以两个 owner 共用的项目硬限额执行：一个 owner 连续完成 5 轮 settle/reconcile，另一 owner 的容量 waiter 在等待期间保留首次时间和 deadline，之后在期限内按当前额度恢复并结算；断言项目已用额与预留额不超限、没有 unknown，owner/Task 账本归属正确。命令 `CH_TEST_DATABASE_URL=<isolated local PostgreSQL DSN> GOPROXY=off GOSUMDB=off go test ./internal/store/postgres -run '^TestPostgresSharedProjectCapacityWaitMakesProgressUnderOtherOwnerTraffic$' -count=3 -v -timeout=5m` —— **3/3 通过**；`git diff --check` 通过。有限轮次用例不替代长期压力测试或生产账单验收，B04 不勾选。
- **worktree 与分支核查：** 本次核对基线为本地 `release@f666108c`。截图中的 7 个 `codex/*` 项目均不在 `git worktree list --porcelain` 的登记路径中，是本地分支引用。预算 compaction 候选涉及的 DSH 补丁文件与 release 树相同；小验收 migration 分支只有过期说明注释差异，release 注释已明确首次 uncached 运行。`budget-test-deploy` 与 release 在 TODO、预算准备及 Runtime 关键文件仍有 60 行新增/432 行删除差异，migration 快照还覆盖到不同的旧代码树；不整支合并以免回退当前实现。M04 页面分支相对当前页面有大量删除，M06 只剩逻辑等价的布尔表达式，handshake 分支的诊断测试在 release 已有且 release 测试文件更新更多。没有发现需要补合的功能改动。
- **清理决定：** `git worktree list` 现有的 9 个 `/private/tmp/morde-*` checkout 都含未提交文件或本地 smoke 记录；主 release 工作树也有本轮预算实现与其它任务改动。截图项目没有对应的 checkout 可删，隐藏 checkout 的改动也不属于截图清理范围。因此本次没有执行 merge、`worktree remove`、分支删除、reset 或 stash；所有工作区内容均保留。此前记录中出现过的截图清理结论以本次实际登记列表为准。
