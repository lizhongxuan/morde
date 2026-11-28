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

- `project/openclaw/src/agents/embedded-agent-subscribe.handlers.messages.update.ts` 先展示真实模型 text delta，再结束该条响应。
- `project/openclaw/src/sessions/session-run-error.ts` 把失败报告与正常模型回复分开，并按 run ID 幂等追加，避免模型无回复时静默。
- `project/openclaw/src/agents/embedded-agent-subscribe.handlers.tools.progress.ts` 只展示明确标记为公开的类型化进度；内部工具细节留在诊断面板。
- `project/openclaw/src/channels/message/ingress-drain.ts` 用持久 ingress 状态和可恢复 lease，保证接收、处理、重试和失败都有可观察生命周期。

这些只作为消息流、去重和公开状态的设计参考；不复用其 Agent loop、provider 或 tool runtime。

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
