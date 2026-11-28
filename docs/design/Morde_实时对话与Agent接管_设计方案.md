# Morde 实时对话与 Agent 接管设计方案

**状态：** 设计与实施中。适用 `release`；Agent Runtime 继续唯一使用 DSH。

## 问题证据

- 旧 PC 曾按 `dsh_recovery_unresolved` 本地生成固定助手气泡，刷新时会重复显示，并让用户误以为模型已检查任务。当前本地版本已移除该伪造气泡，改为显示服务端状态卡。实机旧对话中相邻的同文状态卡对应多条不同的用户请求，不是单条消息被重复回答；它们仍显示结果待核对，真实 AI 回告缺失。
- 线上运行 `release@ee618bed`，Portal 服务 active。终态投递 Worker 每 30 秒反复在 `read_source_facts` 失败，同一 delivery 被重新领取多代；旧日志只有 `*domain.Error`，没有稳定错误码，解释模型没有收到事实。只读核对确认 Task/outbox 的 owner、conversation、initiator、修订、授权版本、事件类型与事件摘要相符，任务步骤计数也自洽；具体 facts 校验失败点仍待部署安全诊断后确认。自动恢复开关按默认值关闭。
- 消息接收 API 会先持久化并返回用户消息 receipt；之后由 ConversationRouteWorker 异步分类。PC 在 route 为 pending 时没有可见助手状态，进入 processing 后才每 2 秒轮询消息列表。用户会看到空窗。
- 另一个前端时序缺陷：收到 receipt 时，如果消息列表第一页还没载入，缓存追加函数返回 `undefined`，刚受理的用户消息本身也不会立即显示。
- 普通消息由 DSH coordinator 先作结构化路由；新任务随后由 DSH Task Worker 运行。两个执行上下文都使用 DSH，但目前短答和后台任务都要等 route 模型完整返回，短答没有首 token 流式展示。

## 设计原则

1. **持久 receipt 后立即可见。** PC 显示明确的“已收到，正在理解”状态；这个状态不是模型答案，也不承诺已开始执行。后续依据持久化 route/Task 状态更新。断网、刷新和切换页面后可从服务器状态恢复。
2. **DSH 主 Agent 负责理解和交付。** 保留 DSH coordinator 对直接回复、澄清、创建 Task、追问、修改和停止的结构化决策；需要操作时交给 DSH Task 上下文执行。复用 route 决策和已授权上下文，不为同一消息重复做第二次分类。普通问答不得依赖云端电脑。
3. **真实内容先流出。** 对直接回复，把 DSH 已验证的真实 assistant 文本增量写入同一条持久响应；客户端显示增量并在刷新后续接。只有安全、已提交的内容可对用户可见；工具参数、原始输出和内部 checkpoint 不流出。
4. **失败由模型看安全事实。** 从持久化事件、调用账本及已验证 Task 状态提取白名单字段，交给 DSH 一次性整理原因与已完成/未完成状态。结果未知时不再派发可能产生副作用的操作。按 Task 修订和来源事件去重，唯一发布到原会话。
5. **兜底是最后保障。** 只有 DSH 无法给出可确认说明时，才发服务器生成的一次性、明确说明原因未明的安全兜底；PC 不根据刷新到的 waiting 状态伪造助手消息。内部日志只记稳定错误码、阶段和关联 ID，不记供应商原文或凭据。
6. **Agent 能力不等于放宽授权。** 继续用现有 DSH 工具、MCP/技能、子 Agent 和 provider 接入。沿用用户/会话/Task 隔离、授权审批、账本、checkpoint 和外部副作用确认，不接入 OpenClaw Runtime。

## OpenClaw 可复用模式

- `project/openclaw/src/gateway/server-methods/chat-send-input-dispatch.ts` 在用户消息持久受理后才返回带 `runId`、`status=started` 和 transcript sequence 的 receipt；`chat-send-admission.ts` 管理幂等与并发受理。Morde 采用相同区分：受理回执立即可见，但不把它冒充模型回答。
- `project/openclaw/packages/gateway-protocol/src/schema/logs-chat.ts` 定义带 `runId/sessionKey/seq` 的状态、真实文本 delta、final、aborted 和 error 事件；`src/gateway/server-chat.ts` 记录 receipt 到首个助手事件的耗时，并在工具开始前冲刷模型已生成的实际文本增量。Morde 的直接回复应增加带 conversation/message/revision fence 的持久增量和续读协议，不能从 Task 状态文案推断模型内容。
- `project/openclaw/src/agents/embedded-agent-runner/run.ts`、`run-orchestrator.ts` 和 `run-loop.ts` 将模型驱动执行与产品消息生命周期分层。Morde 只借鉴分层，继续让 DSH 作为唯一 Runtime；现有 Broker、审批、账本和 checkpoint 保留授权边界。
- `project/openclaw/src/sessions/session-run-error.ts` 将安全失败与成功答复分开，按 run identity 去重；`chat-inflight-snapshot.ts` 和 `packages/gateway-client/src/chat-stream-projection.ts` 让客户端按 run 重建进行中的真实内容，并在事件基线缺失时请求续接。Morde 的终态失败事实应一次性、安全地交给 DSH 解释，再以来源和消息身份去重写入原会话；重连恢复应读取同一 run/message 的持久快照与事件游标，不能重派未知结果的操作。

这些只作为回执、流事件、恢复和错误隔离的设计参考；不复用 OpenClaw Agent loop、provider 或 tool runtime，也不接入第二套运行时。

### 2026-11-28 实施决策：搜索交给 DSH Task Agent

- 路由分类器不再用单次 Broker 调用直接把网页搜索/读取原始输出发成助手消息。此捷径没有 DSH 总结步骤，Broker 失败时也只能落到同一条固定兜底。
- 搜索和网页读取统一创建普通 DSH Task。Task Agent 通过现有授权的只读工具检查来源并组织答案；未知操作结果仍遵守不重放规则，失败事实走现有 DSH 终态解释链路并回到原对话。
- 首次 receipt 与处理中状态继续由持久消息/Task 状态展示。纯文字直接回复保留单次 DSH 路由；搜索要经过 Agent 工具和结果总结，响应可能更久，后续以端到端样本拆解 receipt、route claim、分类、工具派发与首条持久助手回复耗时。此调整没有实现 token 增量流。

## 实施顺序与验收

### P0：错误解释和即时可见状态

- [x] 移除 PC 基于 `dsh_recovery_unresolved` 本地产生的固定助手气泡；真实助手消息未发布前只显示服务端任务状态，并保留详情入口。
- [x] receipt 到达后即使消息列表尚未载入，也先初始化本地缓存显示用户消息；route pending/processing 显示可访问状态。后台稳定状态和重连后的完整核对仍需实机验收。
- [x] Worker 失败日志增加稳定 `failure_code` 和现有 stage，不记录错误原文；定向日志测试通过。部署后仍需读取线上错误码，修正 source facts 根因并确认 DSH 成功回告。

### P1：缩短首次实际回复时间

- [x] 增加 receipt age-at-route-claim 和完整 route classification 耗时直方图，不带用户或 Task ID 标签；目前没有 DSH first-delta/首条持久回复埋点。
- 将 DSH coordinator 的真实直接答复改为有界增量流式持久化，复用已有会话消息版本/事件游标；客户端接入同一条流并支持重连补读。
- 保留当前 coordinator 对多任务追问和控制意图的决策，但检查每条消息是否存在重复分类或可并行准备的非模型工作；只在有证据的串行等待上做局部优化。

### P1：提高 Agent 接管能力

- 盘点当前已接入 DSH 工具、技能、MCP、子 Agent 和沙箱；主 Agent 按目标选择工具或委派独立子任务，合并子 Agent 的真实结果后再答复。
- 将公开进度和内部日志分层。进度使用类型化、可脱敏字段；工具调用与模型账本仍按 Task 记录，不把“已委派”显示为“已完成”。
- 自动恢复继续默认关闭；先有隔离 cohort、恢复专用成功率/额外调用/重复副作用指标和可回退门槛，再做灰度。

### 验收

- 实际 `morde-pc` 从发出消息开始立即出现持久状态；普通问答能看到模型真实回复，较长任务能看到状态推进和最终结果。
- 注入模型拒绝、超时及结果未知故障：Agent 只读安全事实并在原会话发布一次有差异的、准确说明；无法确认的操作不会重派。
- 检查一次新闻检索从 receipt 到回复的阶段与实际工具派发数；自动恢复成功不重复搜索，失败时说明具体已知原因。
- 用实际端到端延迟样本记录首个可见反馈和首段真实答案的时间；不以健康探针、单元测试或固定状态文案替代。

### 2026-11-28 设计核对补充

- 实际 `morde-pc` 已确认旧固定气泡不再由 PC 本地生成；连续出现的相同任务卡各自绑定一条不同的用户消息。当前失败任务详情仍只有“模型服务未返回可确认的结果”，没有实际 AI 故障解释。
- DSH coordinator 目前等待完整结构化 route 结果后才落库；Runtime 内部虽然逐块读取模型输出，但 coordinator 适配器先收齐 chunks 再返回。直接回复没有 Taskless 的持久增量事件与续读 API，Task 流表受 `task_id` 和 Task event fence 约束，不能直接复用。
- 分类调用先于 Task Agent 调用，工具/搜索请求存在两次模型往返；摘要维护在特定长会话场景还会再增加一次串行模型请求。当前阶段耗时指标只有 receipt→claim 与完整分类时长，缺少 accept→first visible answer 和 first durable assistant message。
- 下一步先部署安全错误码并修复实际 facts 读取错误，再验收真实短答及只读工具；持久增量流需要独立协议与授权围栏，完成前不宣称有真实 token streaming。

### 2026-11-28 终态解释失败根因

- 安全日志部署到 `release@b363bd38` 后，线上错误码确定为 `invalid_argument`，失败阶段为 `read_source_facts`。只读数据库检查未输出 Task ID、用户文本或供应商内容；身份、修订/授权围栏、模型 ID、步骤计数和等待原因均满足约束。
- 触发任务的原始目标文本以换行结尾。`boundedTerminalDeliveryObjective` 只在目标超长时去除结尾空白；普通长度时原样返回，之后 `validBoundedTerminalDeliveryText` 因首尾空白拒绝事实摘要。这个校验错误让 Worker 永远到不了 DSH 解释调用。
- 修复为始终只规范化模型可见的目标摘要，原始目标 SHA-256 仍按数据库原文计算。加入普通/Unicode 空白、截断边界及空白输入单测；恢复诊断 PostgreSQL 用例追加结尾换行并校验摘要可读且原文哈希不变。隔离 PostgreSQL 下全部 `TaskTerminalDelivery` 用例通过。
- 修复尚未部署。真实 PC 的新普通问答、只读搜索、终态解释气泡和端到端延迟仍待部署后实测；直接回复的持久增量协议也尚未实现，不能宣称实时 token 流已完成。

### 2026-11-28 路由输出与慢消息刷新整改

- 根因证据：路由失败已能稳定归类为 `invalid_model_output`，此前没有输出结构的安全细分；解析器对 `tool_arguments` 严格要求 JSON 对象，但模型指令的字段合同和示例不足以区分对象与 JSON 字符串，模型输出因此可能被拒绝且没有进入工具派发。新诊断只记录枚举类别、UTF-8 合法性与字节数，不落原始模型文本。
- 处理方案：让路由提示中的字段合同、按 action 输出规则及 few-shot 示例一致；显式要求工具参数为对象值，并提供直接只读搜索示例。继续由 DSH 决定回复、工具或 Task，服务端解析与授权校验不放宽；失败时保留结构化安全原因，不能伪造工具结果。
- 延迟处理：PC 结果待消息期间此前每秒重复失效消息查询。若当前请求超过一秒，下一次失效可能取消或替代它，用户切换会话后才看到已持久回告。改为等待现有请求结束后再调度单次刷新，并以延迟响应回归用例锁住行为。
- 验收边界：对象参数解析、无效输出安全分类、慢消息回告轮询均有本地自动化测试。服务端实际模型输出与真实工具派发须在部署后的 `morde-pc` 中核验。当前架构仍没有 Taskless 直接回复的持久增量事件协议；在 owner/conversation/message/revision 围栏、重连游标和 PC 续读完成前，不承诺真实 token 流式回复。
- 实机搜索揭示第二个接线缺陷：路由已经选中 `morde_web_search` 并创建隐藏 Broker Task，但内置 MCP 执行要求读取 Task revision。直接路由 Task 没有用户可编辑的 revision，数据库 fence 合法值为 `current_revision=desired_revision=0`；应用读模型将它们投影为 nil，Broker 因此在操作账本/派发之前拒绝，用户看到兜底气泡且工具派发数为 0。仅对带可信 `InternalTool` 标记的这种隐藏单次 Task，将 nil/nil 解释为 revision 0；普通 Task 仍要求已有一致 revision。新增 Broker 路径回归用例已复现修复，但需要部署后用真实 PC 再确认 operation ledger 恰好一次、搜索结果进入原会话。
- DSH Runtime 已在 compaction 的模型预算请求中传 `purpose=compaction`，但 Portal adapter 之前丢弃该字段，导致 compaction 被按短答 stage 预算。现在只接受空值或明确的 `compaction`，将后者透传并映射到 summary stage；集成验证确认用 summary 配置创建并结算同一个输出上限。这是长会话摘要预算修正，不等同于首 token 流式回复，也不替代 PC 长任务验收。

### 2026-11-28 PC 复现：重复失败说明与任务回复延迟

- 实际 `morde-pc` 的无工具短问答由 DSH 返回了与问题相符的真实答案，观察到的可见延迟低于 1 秒。两条相同的失败气泡分别对应用户提交的两条重复请求；并非同一条助手消息被复制。短答样本通过不代表后台任务链路已通过。
- 新建的只读搜索 Task 在首个 DSH 模型调用前因任务预算预留被拒绝。线上 Task 显示 50,000 模型 token 预算、已用 0；没有该 Task 的模型调用账本或预算预案。唯一相关的模型账本行为终态说明调用（输入 386、输出 116），并已写入原会话。等待事件的失败类别却被安全过滤成 `unknown`，所以 DSH 终态说明只知道中断状态，无法给用户具体原因。源码中 `task_budget_exhausted` 唯一映射来自 Task reservation guard；它在 Provider dispatch 前拒绝 `request.ReservedTokens > Task 剩余额度`，预留由估算输入与输出上限相加组成。
- 处理：终态失败事实现在保留白名单类别 `task_budget_exhausted`、阶段 `model_call_reservation` 和原因码，并由 DSH 解释模型基于这些事实用自然语言说明“模型请求没有发出”；不把固定兜底文本伪装成模型答复。日志只新增整数预算 envelope（输入估算、输出上限、总预留、任务上限/已用/在途预留），不写提示词、工具定义或供应商文本。
- PC 端对尚未出现在当前会话的终态回告采用最多 8 次退避刷新；每次等当前消息读取完成再启动，收到与该 Task 关联的助手消息即停止。审批与澄清卡存在时不启动回告刷新。这样处理 terminal-delivery Worker 晚于 Task SSE 的窗口，同时保持刷新次数有界。
- 尚未降低保守预留系数、抬高任务预算或绕过 hard budget。需先部署整数诊断并在一条新的只读 PC 搜索请求上取得输入/输出预留拆分；若输入本身超过剩余预算，应修剪可安全去重的上下文/schema 或明确说明预算约束，不能把未派发的搜索报告成已执行。
- 本地验证：App/Worker 定向诊断测试、隔离 PostgreSQL Task reservation 用例、PC Assistant 测试（334 项）及 Portal typecheck 均通过。新诊断尚未部署；预算问题是否能在现有 50,000 硬额度内完成搜索与最终综合，必须由部署后真实 `morde-pc` 验收确认。Taskless 持久增量流仍未实现。

## 自动恢复的灰度与停止策略

- 自动恢复保持默认关闭。当前 `MORDE_TASK_MODEL_RECOVERY_AUTO` 在 Portal 启动时读取并作用于整组 Portal/Task Worker；线上是单一 `portal serve`，没有独立 Worker cohort、用户分组或恢复专属指标，因此不能安全地把它作为线上灰度开关。
- 关闭后，尚未启动的自动恢复计划必须原子退回 `waiting_user / task_model_recovery_retry_available`，将恢复记录降为 `retry_available`，保留一次明确的手动入口；不得消费自动尝试额度或派发模型请求。用户主动点击后的计划标记 `automatic=false`，仍走原权限、修订、账本和 checkpoint fence。
- 已开始的模型调用、结果或用量未知的调用不因开关变化而重新派发。必须先按已存在账本与 checkpoint 事实恢复；无法证明未产生副作用时只暂停并说明。
- 未来灰度启用前，必须有独立 cohort 与可单独关停的门控，并用低基数指标比较「无有效入口却提示等待用户」、原会话终态回告覆盖率、自动恢复成功率及额外模型调用。指标不得包含 owner、Task、conversation 或 call ID。发生重复副作用或跨权限展示时立即关闭 cohort 并保留任务记录。
- 这轮新增了关闭门时将排队自动计划原子退回手动入口的实现及测试。独立 cohort 和灰度指标仍未实现，所以线上继续关闭自动恢复，不能把默认关闭或隔离测试写成灰度通过。

### 2026-11-28 真实 PC 搜索后续复现与处理方案

- **普通对话样本：** 实际 `morde-pc` 新旁聊发送短问答，用户消息和真实 DSH 回复均出现；从点击发送到首次观察约 0.7 秒。PC 能立即显示后台请求的受理/处理中状态，但这仍不是 token 增量流。
- **搜索失败事实：** 同一客户端发送只读新闻搜索后，约 0.7 秒出现“已收到，正在等待处理”，随后进入“结果待核对”。线上日志证明首轮模型请求在 Provider dispatch 前被 `task_budget_exhausted` 拒绝：保守输入预留 50,908，加输出上限 2,048，共 52,956，超过 Task 固定上限 50,000；数据库中该会话只有 537 字节、4 条消息，Task 模型调用账本只有终态诊断调用，搜索派发为零。终态 outbox 已发布一条 DSH 解释消息，明确说明搜索请求未处理且没有搜索结果。
- **诊断边界：** 50,908 是按 UTF-8 字节计的安全上界，不是模型精确 token 数。现有日志仅有总 envelope，尚不能把超额按固定系统指令、广泛工具 schema 或授权个人上下文精确拆分；不得为通过门槛而抬高 50,000 硬预算、降低字节预留或重放未知调用。
- **处理方案：** 复用 route model 已验证的“搜索/网页读取需要 Task Agent”决策，持久记录一个有限的公开网页研究工具 profile。首次 DSH Task 请求的 Broker 工具仅留网页搜索、网页读取和必要澄清；过滤在 Task 附件读取工具注入后再应用，避免该工具扩大 schema。DSH 内部的结果证据标注器仍可用，它只绑定模型已给出的文本与证据引用，不访问外部资源或执行操作。首轮不加载无关个人记忆、不启动子 Agent/后台任务；保留当前用户目标、必要会话历史、Morde 系统安全指令、Provider 账本、Task 授权和 revision/checkpoint fence。Profile 不是额外授权，只会缩小 Broker 已授权工具集；Task 后续发生用户修订时恢复常规工具选择并继续按现有授权校验。此裁剪能降低新短会话首轮的输入预留，但不保证超长历史或项目上下文一定低于 50,000；不得降低安全估算或移除必要上下文来强行放行。
- **OpenClaw 参考边界：** 借鉴 route/admission 明确交接、按运行身份隔离状态和按有效工具 allowlist 裁剪 schema；继续由 DSH 决策与综合，不接入 OpenClaw Runtime、模型循环或工具执行器。工具调用开始前仍显示受理/执行状态，失败原因基于白名单事实经 DSH 生成并只发布一次。
- **待验收：** 重新从实际 `morde-pc` 新旁聊跑短答和只读搜索，检查首次受理、任务最终状态、网页工具 operation 账本恰好一次、来源综合后的原会话助手气泡与失败说明；测量发送到首次可见状态、Provider dispatch 和首条持久助手消息的阶段耗时。另验证超预算拒绝不派发、未知结果不重试、普通修订任务仍可使用其授权工具。Taskless 持久 token 增量流与独立恢复灰度仍是单独未完成门槛。

### 2026-11-28 复核：公开网页研究 profile 未触发

- 在 `release@94312702` 上通过实际 `morde-pc` 复测：普通短答收到真实 DSH 回复「2+2等于4。」，从点击发送到客户端观察到助手气泡不超过约 0.8 秒。新闻搜索请求仍在 Provider 派发前被 Task 预算拒绝，预留为输入 50,830、输出 2,048、合计 52,878，高于 50,000 上限；没有搜索 operation。原会话的 DSH 终态气泡准确说明请求未执行、没有生成新闻和来源。
- 新根因：DB 中该搜索 Task 的会话上下文 profile 为空。路由提示同时要求 `action="new"`、工具字段留空；Worker 只会在服务端校验 `action="tool"` 的搜索/网页读取标记后附加受限 `web_research` profile。于是请求进入普通 Task 路径，没得到预期的工具集裁剪。
- 处理：统一主路由与兼容路由的提示合同和 schema 示例。Eligible 公共网页搜索/读取用 `action="tool"`、固定工具名和结构化参数作为内部路由标记；服务端验证后仍创建 DSH Task，首轮只暴露 Broker 已授权的搜索、读取和必要澄清工具。模型不会直接执行 Broker 操作或把原始搜索结果发给用户。其他新任务、直接回复、授权与幂等校验保持既有语义。
- 本地验证：路由提示/解析回归、PostgreSQL profile 持久化用例，以及 Worker 的受限 profile 与搜索→阅读→来源综合 E2E 通过。此修复尚未部署；须在下次发布后以新 PC 旁聊重新确认 profile 生效、真实搜索派发一次、最终来源答复回到原会话。
- 边界：Taskless 直接回复仍没有持久 delta/续读协议，故约 0.8 秒样本只证明本次完整短答及时出现，不能称为 token 实时流。恢复灰度 cohort 和 Worker 完整包也仍未验收，自动恢复保持关闭。
