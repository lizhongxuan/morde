# Morde 模型生成预算自适应实施任务 TODO

**状态：基于 `release@936389c` 推进。B01 的 P0 协议、fake HTTP 和 Worker planner → PostgreSQL 账本 → ModelGateway 合成客户端子门禁均通过；DSH → 生产 planner → provider 的统一生产发送门禁未验收。B02 能力元数据的本地实现与自动测试通过；PC 已显示能力/unknown 字段，真实测试服务端读写仍待验。B03 已有纯预算计算、精确路由六阶段版本策略、prepare 计算和 5,000 组固定种子的独立公式差分验证；线上影子采集与实流离线对照尚未完成。B04 已有本地 PostgreSQL 原子 prepare/dispatch/settle/过期释放、超额对账、恢复 attempt 独立 DSH/账本身份，以及仅针对精确模型并发限额的持久容量等待/截止/唤醒/暂停；新增容量等待定向 PG 门禁通过。仍待提交前后崩溃验证、动态费率/策略变化重算、持续并发下槽位公平、人工 overage 对账和 DSH→生产 planner→provider 整链验收。生产 planner 未接入，实际发送上限未变；B01/B03/B04 不勾选，B05–B10 未开始。**
评审依据：《Morde_个人Agent_模型预算与可靠交付实施方案.md》、固定提交的 Morde 源码，以及锁定版本 DSH 的本地包契约和离线实跑。保留原 B01–B10 编号；已实现 DSH opt-in IPC/Worker 协议探针、Task compaction prepare、假 HTTP 集成测试、模型输出能力元数据、阶段策略快照及 PostgreSQL 预算准备账本；未运行付费模型或部署，没有修改测试服务器。

**方案复核决策（2026-11-27）：** 采纳方案的 P0–P4 门禁、等待/暂停语义、周期止损与试点质量验收；保留“未知能力不猜、短答不多一次模型往返、历史 Task/Schedule 合同不迁移、未知派发结果继续占用”的边界。B03 影子原型不再作为开始 B04 账本工作的硬依赖；预算策略与 `O_send` 计算仍须在 B04/P1 试点启用前通过影子核验。方案中的 `4,000` 仅是反例数值，拟议 API 字段不代表现有接口；均不写成默认额度或既有合同。

## 1. 决策摘要

1. 移除普通 Task 单次输出统一压到 2,048 token 的设计目标；也要检查网关与 provider 上 32,768 的固定上限。单纯把它们改大仍会因预算过度预留而拒绝或阻塞并发。
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

- [ ] **B01（进行中：P0 协议与本地 fake HTTP 子门禁通过；PG/生产整链待验）验证 DSH 逐次输出控制**
  - 代码现状：Morde 锁定 `0.2.2-rc.1`；普通 Task `start.max_tokens` 带入 `AgentOptions.maxTokens`，Hermes 压缩会读 `requestHeader().config.maxTokens`；适配器目前只给窗口，不给动态 `defaultMaxTokens`。已弃用的 `agent/request` 不新增；`agent/pre-step` 能改消息/拒绝步骤，但不是直接覆写请求上限的入口。[锁定版本](https://github.com/lizhongxuan/morde/blob/936389c24ea61db45eded881423cef74d8815ca8/runtime/morde-cloud-runtime/package.json#L13-L29) · [普通 Task start](https://github.com/lizhongxuan/morde/blob/936389c24ea61db45eded881423cef74d8815ca8/server/internal/worker/cloud_dsh_session_tree.go#L2408-L2425) · [Agent 创建](https://github.com/lizhongxuan/morde/blob/936389c24ea61db45eded881423cef74d8815ca8/runtime/morde-cloud-runtime/src/engine.mjs#L1746-L1755) · [压缩预算](https://github.com/lizhongxuan/morde/blob/936389c24ea61db45eded881423cef74d8815ca8/runtime/morde-cloud-runtime/src/engine.mjs#L1598-L1619) · [适配器](https://github.com/lizhongxuan/morde/blob/936389c24ea61db45eded881423cef74d8815ca8/runtime/morde-cloud-runtime/src/engine.mjs#L2204-L2267)。
  - **已实测的候选方案，不是整链通过：** 锁定 DSH 可由 Agent 作用域 `agent/pre-step` 取得 `agent`、turn/step、输入消息和 `AbortSignal`；同一步的 `LlmAdapter.resolveModel(provider, model, signal)` 收到同一个 signal，可用 `WeakMap<AbortSignal, plan>` 将每步计划交给适配器 `defaultMaxTokens`。本地 fake adapter 复现了同一 Agent 的 **512→1,024**，并让两个同模型 Agent 并发获得 **768 / 3,072**，header 和 stream options 都与各自计划一致；未使用已弃用的 `agent/request`。显式 `AgentOptions.maxTokens=2,048` 仍固定两处为 **2,048**，所以生产试点必须只在服务端开关授权的路径省略显式 cap，并在计划缺失时拒绝入模。此接口组合提供了逐 step 传值的候选通道，但不会自动计算可信 `O_send`。
  - **DSH 准入钩子：** 上游锁定版本没有此 API；在 `@deepseek-ai/dsh-agent-loop@0.2.2-rc.1` 的 header 冻结前增加自有 `agent/request-budget` waterfall hook。钩子看到 Agent、turn/step、signal、最终投影的 config/messages/tools/toolHistory；返回 `{maxTokens}` 后重新 `prepareCall()`，并以同一预览提交 tool registry message，保证 DSH header、adapter stream options 和输入预览一致。无监听器或返回 `undefined` 保持原请求路径。补丁脚本校验 DSH 版本、源/目标 SHA-256、MIT LICENSE，以及其私有 hook registry 所依赖的 Cordis `4.1.0` 结构；在 `build:dsh-compaction` 和 Docker 测试构建中应用。此为 Morde 仓库针对锁定依赖的最小补丁，不是上游受支持插件。
  - **本轮新增的 P0 Morde 协议探针：** Worker 只在注入 `ModelBudgetPlanner` 时发送 opt-in admission；默认仍发送旧 `max_tokens`。runtime 在 AgentLoop hook 中基于 DSH/LLM 实际 provider 投影、已持久化的基线 checkpoint 和独立 call ID 发送 `model_budget_prepare`；Go 校验 Task/owner/Agent/generation/session/through/turn/step、工具授权和规范请求摘要，再返回短时、单次、会话绑定的 plan。最终 model frame 必须回显 plan/call/digest，输入投影与 cap 必须一致，否则拒绝入网关。实际 DSH hook 会去除 LLM 请求投影中的 developer/tool registry 内容；预算摘要投影已同步此行为，并覆盖恢复后工具增删。`AccountRuntime` 只允许该字段出现在 Task start；生产 wiring 目前没有 planner，故默认发送行为没有切换。
  - **本地 fake HTTP 子门禁：通过。** Go Worker → Morde runtime hook/IPC → Go plan seam → `ModelGateway` → `httptest` 假 provider 验证真实序列化的 `max_output_tokens=3,072`，同时检查 prepare/final 请求摘要与身份一致、未绕过网关走 fallback。此测试里的 planner 只返回合成计划，不执行额度锁定或账本预留；不代表真实模型能力、用户余额或生产计费已验证。
  - **剩余生产门槛：** 新增 PostgreSQL 集成用例分别验证 Worker planner adapter → PG prepare → `ModelGateway` → 合成 `ModelClient`，以及同一路径通过本地 `httptest` 收到 Responses wire-format 请求；HTTP 用例检查 `max_output_tokens=3,072`、usage 入账和 settled replay 不二次请求。HTTP 测试客户端只模拟 wire format，不是生产 `ResponsesClient`；两类用例都未经过 DSH runtime IPC，不能替代完整生产门禁。普通调用与压缩在取消/重启/未知派发下的一致性仍未统一验证，也未把 planner 注入生产 wiring。`model_call_ledger` 保存总 token reservation，项目账本保存 input/output 与金额分项；预算请求摘要绑定最终 `max_tokens`。并发子 Agent、取消/超时、压缩真实账本结算、Luna 推理下限和 provider HTTP 失败注入仍需 fake HTTP + 隔离 PostgreSQL 联合验证。通用 `LlmRuntime.prepareCall(..., configure)` 虽有第三参数，AgentLoop 不传入，不能依赖。未通过前不启用生产 cap。
  - **上游对照：** 检查了官方 `dsh-v0.2.2-rc.5` 和 `dsh-v0.2.3-alpha.2` 的 AgentLoop 源码，仍从 `AgentOptions.maxTokens`/已记录 header 取得 cap，并在写 header 前调用 `prepareCall()`；未发现替代的 per-request budget hook。[rc.5 的请求准备与 header 顺序](https://github.com/DeepSeek-ai/deepseek-harness/blob/dsh-v0.2.2-rc.5/packages/core/agent-loop/src/agent.ts#L590-L657) · [alpha.2 对应实现](https://github.com/DeepSeek-ai/deepseek-harness/blob/dsh-v0.2.3-alpha.2/packages/core/agent-loop/src/agent.ts#L575-L650)。alpha.2 还是预发布版本，并要求 Node.js 26.13.1 及以上；当前本地是 26.13.0，单纯升级并不适合且没有显示能解决该接口缺口。[官方发布记录](https://github.com/DeepSeek-ai/deepseek-harness/releases/tag/dsh-v0.2.3-alpha.2)。
  - **压缩路径：** 已为 opt-in Task 的 Hermes/Morde compaction `ctx.llm.stream()` 单独接入 prepare；计划绑定最终文件/tool 投影、当前 durable `through_seq`、turn/step、随机单次 call ID 和取消信号。失败的上下文溢出 attempt 先持久化，再为降级重试取得新计划。Coordinator compaction 和预算关闭路径保持旧行为。runtime 用例通过；压缩仍未接生产账本，故不能开放 admission。
  - **生产链与否决条件：** `runAgent()` 的普通模型步现有 opt-in handler/Go plan prepare 往返可工作，但生产 wiring 仍不注入 planner，实际请求 cap 未改变。只有全局可变默认、只能在 Go 网关后改、多 Agent 共享值串号、或必须复活弃用监听器，都应保持旧发送路径；继续接入两阶段账本合同后逐项证明 `header == ipc.max_tokens == ledger.output_reserved == HTTP max_output_tokens`。`make pc` 此前启动并打开当前源码的客户端，只确认主会话和输入框可见，随后以 SIGINT/130 停止；当前改动是服务端协议探针，没有 PC 预算入口可验收。未调用付费模型。B01 不勾选。
  - 当次验证：核对锁定 DSH 组件版本/integrity；默认值探针（3/3）与 request-budget hook（5/5）通过；runtime/Go 双向协议、plan 绑定和假 HTTP 网关测试见第 8 节。

- [ ] **B02（实现与本地自动验证完成；PC 真实服务端数据端到端读写待验）建立可信模型能力与试点目录**
  - 位置：模型目录、`ModelConnection` 存储/API、OpenAPI、管理后台和 PC 的模型连接入口。
  - 实施：已为最大输出补齐有效值、个人自定义路由覆盖、来源、解析时间、目录版本和 HTTPS 来源 URL；未知时返回 unknown/null，不依据模型名猜测。精确官方目录目前只收录 GPT-6 Luna、GPT-6.1 Sol、GPT-6 Astra 与 DeepSeek Flash；DeepSeek 官方 [Chat Completions API](https://api-docs.deepseek.com/api/create-chat-completion/) 规定 `max_tokens` 范围为 `1..393216`，数据库与代码目录使用上限 `393,216` 并保存该参数文档来源。自定义覆盖仅允许个人自有路由，后台共享路由保持官方目录或 unknown；系统预设和服务端认证模型不接受手工覆盖。输出能力元数据不改变 32,768 网关发送上限。
  - 验收：服务端精确目录、迁移、列表/详情、过期目录失效与个人覆盖范围由 Go/API/PG 用例覆盖；个人模型与后台模型页均显示有效值/来源/版本/来源链接或 unknown。个人自定义入口支持省略字段、显式覆盖和 `null` 清除；共享后台表单不再发送服务端拒绝的覆盖值。OpenAPI 将新增响应字段标记为可选，以兼容滚动升级中仍未返回该字段的旧服务器。
  - 来源依据：[OpenAI GPT-6 Luna](https://developers.openai.com/api/docs/models/gpt-6-luna)、[GPT-6.1 Sol](https://developers.openai.com/api/docs/models/gpt-6.1-sol)、[GPT-6 Astra](https://developers.openai.com/api/docs/models/gpt-6-astra)、[DeepSeek Models & Pricing](https://api-docs.deepseek.com/quick_start/pricing/)。未知的 GLM/custom 路由不设置输出上限。以上值只是能力元数据，不证明网关可发送到该值，也不代表价格/计费安全门槛已通过。
  - PC 实测：用 `make pc` 重建并操作当前源码 `morde-pc`，打开“设置 → 模型与工具”，确认详情新增“单次生成上限”和来源；现连测试服务端显示“暂未识别”，说明 unknown 展示有效，但未得到服务端新目录值，未在远程设置中新增或修改连接。个人自定义覆盖的保存流程由 Portal 交互测试验证；真实 PC+新后端数据的端到端写入仍未覆盖，因此本项暂不勾选。

- [ ] **B03（进行中：纯计算、精确路由六阶段策略快照和 prepare 计算已实现；影子采集与离线对照未完成）实现分阶段额度策略与影子计算**
  - 位置：模型调用计划生成器、服务端配置、路由阶段识别及现有预算读取接口。
  - 实施：为已验证模型按短答、工具步骤、子任务、汇总、显式长文与纯文本续接设置版本化 `O_goal` / `O_min`；实现第 2 节 `O_safe` 计算。首轮仅记录预算计划及其原因，不改真实发送值；不设一个对所有模型/阶段通用的替代常量。短答继续使用快路径。
  - 验收：用同一批固定请求验证上下文、模型上限、各预算维度及最小额度的取最小结果；能力未知、输入上界缺失、余额不足、整数边界时给出稳定原因。影子输出能解释各项如何限制 `O_send`，并与离线参考结果对照。该影子阶段不得改真实发送值；它不是开始账本实现的阻塞依赖，但其结果必须在 P1 试点启用前核验通过。
  - 本轮实现：[纯函数计算器](../../server/internal/app/model_output_budget_plan.go)计算上限和限制来源；新增精确 HTTPS 路由绑定、明确版本号且必须配置六个阶段的不可变策略快照；`PrepareTaskModelBudget` 把精确模型能力、上下文余量、输入预留、阶段 `O_goal/O_min` 与协议边界带入计划，并由存储事务计算额度后的 `O_send`。新增固定种子 5,000 组输入的独立公式差分测试通过，路由没有策略或模型能力未知时拒绝进入自适应 prepare。线上影子日志/指标、真实流量离线对照与发布前影子核验仍未做，B03 不勾选。

- [ ] **B04（进行中：本地 PostgreSQL prepare/dispatch/settle/释放/超额账本及容量等待定向子门禁通过；崩溃恢复、动态费率与持续负载公平性、完整调用链未验）把 `O_send` 原子预留到现有账本**
  - 位置：`server/internal/app/model_gateway*.go`、`server/internal/app/model_budget_preparation.go`、`server/internal/app/model_budget_capacity_wait.go`、`server/internal/store/postgres/model_usage.go`、`server/internal/store/postgres/model_budget_preparation.go`、`server/internal/store/postgres/model_budget_overage.go`、`server/internal/store/postgres/model_budget_capacity_wait.go` 与 migrations/集成测试。
  - 实施：把 B03 的纯计算器/策略快照纳入预算计划链，完成阶段化 `O_goal/O_min` 与 `O_safe/O_send`，先仅影子核验；同一短事务按固定锁序读取账户、Task、项目及后续引入的单次/周期额度，计算唯一值并保存输入/输出/费用预留和请求身份；事务提交后才派发，不能持数据库锁等模型响应。沿用普通 Task 当前默认和 Task/Schedule 整数合同，不迁移历史数据、不引入 nullable `budget.model_tokens`。验证共享账户和跨项目调用的锁顺序不产生旁路。账本语义须可区分已预留、已派发、已结算、结果未知和确定未发送；可映射现有状态，不必为名称机械增加迁移。保留 Task identity、请求摘要和授权代际；崩溃无法证明未送出时不得重放或释放未知占用。等待队列须有期限、额度释放后唤醒，等待者不占额度或 worker 槽；检查小请求饥饿。Scope 只扩试点路径，现有 `route/compaction/review/terminal_delivery` 专项合同不顺便改。
  - 验收：加入真实 PostgreSQL 并发竞争、同键重入、提交前后中断、等待期限/释放唤醒、结算差额、超预留、费率舍入及个人模型用例；由假端点计数验证：同一调用不重复派发、明确未发送可安全恢复、未知结果不自动再次派发。现有 `model_usage_recovery_integration_test.go` 作为基线；核对已用＋待结算不会放行超硬上限的本地预留。供应商返回超过占用的实际账单另验冻结、对账和如实记账。
  - **已实现和已验证的子集：** migration 00265 保存 Task fence、阶段策略/能力快照、输入与输出预留、单次调用身份和短期 prepare；单个 PostgreSQL 事务按账户、项目/成员、Task、项目预算及账本顺序计算并写入预留。prepare 可幂等重入；派发时重新校验 Task fence、请求摘要、能力/策略版本与过期时间，并原子消费单次计划；已知用量通过现有账本结算。周期清理通过每分钟 Portal 清理循环释放确定未派发且已过期的 hold，过期比较使用数据库时钟。项目费率按整数微单位向上计费，账户/Task/项目竞争不会超过本地 token 与费用额度。migration 00266 的超额对账表保存精确 token 差额及项目金额差额；已知用量超出签约输入/输出时冻结账户与项目账本为 `unknown`，并令 Task 用量变为未知。
  - **当前 PostgreSQL 行为测试：** 覆盖同键 prepare 回放、不同摘要冲突、能力/策略快照改变后释放未派发旧 hold 并重算、过期计划不能派发且有效计划不会被提前清理、账户与同一项目的并发竞争仅允许一笔成功、费用舍入边界、已知结算、个人路径超额、项目路径 token/金额差额冻结、确定未派发时原子释放账户/项目 hold、stale dispatch 变成 `unknown` 后不再派发/清理且新 prepare fail closed，以及 00265/00266 schema 约束和有记录时拒绝回滚。
  - **本轮新增并通过的容量等待子门禁：** PostgreSQL migration 00269 和专用等待账本仅对 `reason=model_concurrency_limit, stage=reservation` 持久排队；原子设置 `waiting_user + model_budget_capacity`、清除 lease 并保存 Task/Agent/调用/请求摘要/checkpoint fence，截止期为数据库时钟的 5 分钟。容量事件使用 `task.status`，不触发无用户操作的 `attention_required` 通知。周期 reconciler 在确认同一旧 Agent 的 stop receipt 与 checkpoint 后，重新检查账户/Task/项目最小预算；额度释放时重排队到既有 claim loop，硬额度不足、期限到期或权限/模型/恢复身份失效时安全暂停。尚无 stop receipt 时只延后处理，不唤醒或暂停仍可能运行的旧 DSH。恢复 Task 的 DSH frame 和账本身份保持分离，并原子更新 recovery schedule/event sequence。
  - **容量等待证据：** 定向 PostgreSQL 用例覆盖 lease 清理与幂等入队、无 stop receipt 时不唤醒、active reservation 阻塞后释放并重排队、现有 claim loop 领取、5 分钟单次等待期限、账户/Task/项目硬预算暂停、授权/普通及 recovery checkpoint 失效、审批/澄清等待隔离、双 Store 并发只唤醒一次，以及同一 owner 暂时放不下的大 waiter defer 后下一轮小 waiter 可先继续。新增 Task status/attention 文案与自动重算当前预算的状态由 Go/API 和 Portal 用例覆盖。此公平证据只覆盖固定轮转场景：reconciler 重排队后没有原子保留 provider 并发槽，持续新请求竞争可能插队；仍未证明持续负载下小请求不饿死。wait row 被重排队时会删除；若同一模型调用随后再次被容量挡住，会新建 5 分钟期限，跨多次“唤醒→重试→再等待”的总时长尚无上限。活跃 reservation 判定仍使用入队时保存的 minimum，策略/费率提高可能早于可用额度结算而触发新的重排队尝试，降低则可能多等。不得把单次 wait row 截止时间描述成 Task 整体等待上限。
  - **未完成验收：** 没有提交前后崩溃注入，也没有把完整 DSH runtime → adaptive plan → PostgreSQL → provider HTTP endpoint 计数器连起来证明端到端“不重复发送”。没有人工/后台清算 overage `unknown` 的对账闭环，也没有全量个人模型/跨 owner 项目并发组合测试。此账本尚未连到生产 DSH planner，B04 不勾选；在处理动态费率/策略变化、持续并发槽公平和端到端门禁前不得开放生产 cap。

- [ ] **B05（未开始，依赖 B01、B02、B04）将签约的上限接入 DSH 并清除固定天花板**
  - 位置：`server/internal/worker/cloud_dsh_session_tree.go`、`server/internal/app/model_gateway_dsh.go`、`server/internal/app/model_gateway.go`、`server/internal/model/responses.go`、`runtime/morde-cloud-runtime/src/engine.mjs`。
  - 实施：仅在 B01 接入合同成立后，让 DSH request header、压缩、provider 请求、账本都使用同一个签约值。仅为能力明确、通过试点的 Task 路径移除固定 2,048；对 32,768 网关/Responses 公共校验做来源和路由分界，只有已认证端点、预算与传输同时通过时才允许更高值。非试点、控制和摘要路径保持原限额。不要把 2,048 当上下文大小，也不要把运行时 start frame 的协议上限当模型规格。Luna reasoning、复用/恢复请求及子 Agent 不得越过签约值。
  - 验收：不同输入长度和余额下实际单次额度随策略改变；压缩规划、账本与 fake HTTP endpoint 字段逐次一致；覆盖主/子并发、恢复、取消及路由改变。

- [ ] **B06（未开始，依赖 B05）可靠传输长回复和部分结果**
  - 位置：模型连接/响应超时、`model_gateway.go`、DSH 响应 framing、服务端持久化和 `morde-pc` 会话展示。
  - 实施：把 45 秒单一超时改成适配器支持的连接、首包/空闲、总时长、取消策略；文本采用有界可恢复分块，区分单事件与总消息大小，给消费者慢读设置背压。现有 DSH adapter `stream(options)` 先攒满 `chunks` 再交回 DSH；PC 现有增量展示通道不代表 DSH 已支持有界透传。沿用并验证现有流式展示，补齐缓冲↔取消↔durable prefix 三者的合同；评估 IPC、现有 256 KiB 响应校验、32 KiB 助手消息边界以及结果稿件/附件路径，不能只改一个 cap。超过会话正文边界则经已验证的分块或附件在原会话引用，不截断伪装完成。不在观测、客户端或非必要存储中扩散原始推理；不要为省空间盲删 provider 协议要求的可恢复状态。冻结所需最小协议字段、密封保存及访问边界后再改。
  - 验收：本地 fake endpoint 模拟慢首包、长间隔、超过 256 KiB、断线重连、取消和慢 PC 客户端；完整正文或已确认部分可恢复读取，不重复可见文字，不因放大字节上限造成无界内存。

- [ ] **B07（未开始，依赖 B05、B06）区分截断并安全续接文本**
  - 位置：provider 响应状态解析、模型调用结算、DSH checkpoint、对话片段持久化。
  - 实施：Responses 适配器已经把 `max_output_tokens` 译为 `length`，把过滤单独编码；但 `ValidDSHModelResponse` 对纯文本目前只接受 `stop`。改动点不是再解析一次 provider，而是增加有明确状态的可恢复“部分文本”路径，保持截断工具/不完整 JSON 不能进入执行侧；不得通用放松验证或把 `length` 改写为 `stop`。只有已知纯文本 token 截断允许最多两次有界续接；每次新调用有独立身份和额度，保全已确认内容。截断工具参数绝不执行，未知外部操作结果先对账。
  - 验收：分别模拟 `max_output_tokens`、`content_filter`、网络中断/未知计费、部分正文、重复段落、不完整工具参数、重启恢复与额度不足；账本和原会话内容准确，副作用执行次数不重复。

- [ ] **B08（未开始，依赖 B05、B06）提供显式原会话长回复路径**
  - 位置：消息接收/路由、PC 撰写入口、原会话正文持久化与展示。
  - 实施：短答/状态/取消继续走快路径；先以显式交付选择进入，服务器在调用分类模型前分流；不把隐式识别放进首发标准。`ApplyConversationRoute` 的普通 reply 会把源消息标为 applied 并原子交付，长路径需有同等源消息/会话绑定、幂等和取消，不把现有 reply 结构字段抬成任意长正文。B02/B04 的公共计量能力可复用；开发期可以验证 PC 入口，不把未关联 Task 的长文混进 Task 专属账本来源。为无 Task 长文设一次回复的总 token/最多调用次数/总时长并查上层额度；不能说 Task 的 50,000 已保护纯对话。长回复不创建后台工具 Task，除非用户实际委托后台工作。
  - 验收：PC 上短答仍不新增模型往返；显式长文完整返回原会话，部分完成/暂停状态可读；无 Task 模型能处理长文，刷新重连、取消和重试不丢文或重复。普通 Web/iOS 不代替 PC 验收。

- [ ] **B09（未开始，依赖 B02–B08）按基线验证并灰度（P3）**
  - 位置：定向 Go/Node/PostgreSQL 测试、`make pc` 实测、验证记录，以及当时有效的 Harness 流程/TODO 文档。
  - 实施：锁定代码 SHA、模型与版本、端点/能力/计量来源、用例和判分方式。同一模型及合成输入对照原行为与候选策略；影子阶段先记录，不改变发送额度；通过门禁后只对明确试点用户和一两个可信模型启用，保留关闭新策略的回滚能力。
  - 验收：每类建议至少 30 个固定样本，报告有效样本量、p50/p95、绝对差、质量与失败类别；短答不得增加模型往返；暂以 p95 首字时间相对基线不劣化超过 10% 为门槛，噪声过大则扩样。长文按预先固定的必需条目/事实清单验收完整性，不以字数代替质量；至少一个预期完整的可见文本超过 2,048 个模型 tokenizer token。证明两个子 Agent 实际并发、预算暂停后恢复且已完成步骤不重跑；安全用例超额派发和重复可见副作用均为零，并覆盖长历史、未知能力、预算不足、取消和截断。假端点只证明协议，不冒充真实模型。结果分“通过/失败/未覆盖”；保留 provider 用量、发送上限和可见交付证据，不记录秘密或正文。

- [ ] **B10（未开始，P4 后续阶段；依赖 B09 试点数据）设计并评估独立按次与周期止损**
  - 位置：Task/Schedule 预算产品设计、OpenAPI、PC/iOS 设置与账单展示。
  - 范围：先实测现有 Task 50,000 是否需要调整；设计新交互 Task 的有限 token、单次触发总模型调用数和总时长止损。Schedule 采用单次触发额度与滚动 24 小时额度；预算占用按预留创建时间归窗，跨窗口的在途/未知预留继续计入，不因时间窗口翻转释放。同一触发中的重试和子 Agent 共用额度；超过后暂停并说明，不积压后补跑。未知 BYOK 费率只显示 token/调用次数/时长止损，不承诺金额上限。显式加额须有适用范围、失效时间和幂等键，沿用现有审批/审计；默认仅对获批的本次 Task 生效。用户可查看已用、剩余及暂停原因。
  - 验收：先提交模型/费率/计量分布、真实供应商回执数据与新产品合同，再决定是否实现及额度值；已知价格使用保守费率和整数最小货币单位。验证触发内重试/子 Agent 共用额度、跨窗未结占用、暂停后不补跑、加额重复提交不重复生效、BYOK 未知费率展示和回滚。历史 Task/Schedule 与旧客户端合同不静默迁移；产品结论未批准前不增加周期预算字段或变更新建默认。

## 6. 执行门禁与顺序

**B01 的 P0 分为协议子门禁和生产发送门禁。** DSH/Worker fake HTTP 协议子门禁及 opt-in Task compaction prepare 路径通过；B04 已有独立 PostgreSQL 原子预留/结算实现和行为用例，但还没有接入该生产 DSH 链路。完成 B01 仍需验证普通调用与压缩调用的取消、恢复、崩溃和未知派发一致性。生产门禁通过前，B02 的能力目录、B03 影子验证和 B08 现有路由/PC 界面验证都不能修改在用请求上限、默认费用或权限。

P0 协议子门禁通过后可开始 P1 的 B02、B04；B03 影子计算可与 B02/B04 并行，B04 实现纳入 `O_goal/O_min/O_safe/O_send`，影子结果须在生产试点开放前核验。B05 接入实现依赖 B01 的生产合同、B02 和 B04；B01 生产门禁、B04 账本事务及 B05 一致性验证全部通过后，才可为试点路由开放发送策略。P2 为 B06+B07 的可靠长回复和安全续接；P3 为 B08+B09 的原会话长回复、PC/模型基线和灰度；P4 为 B10 的按次/周期止损评估与独立决策。B08/B09 用当前源码 `make pc` 验收，iOS 负责兼容和原会话对账。记录必须区分“接口探针 / fake HTTP / PG / 真实 PC 与模型”四层，各自标明通过或未测。

阶段依赖：B01 协议子门禁已通过，B02 的元数据/API/UI 实现已完成本地代码验证；B04 依赖协议子门禁与 B02，纳入 B03 计算和影子核验；B01 生产门禁与 B04 账本门槛通过后，再完成 B05 的发送合同和一致性验证，然后才进入发送试点 →（B06 与 B07）→（B08 与 B09）；B10 等 B09 试点数据并独立产品评审。所有生产门禁仍按已验收依赖逐项放行，不因 P1–P4 标签自动视为完成。

## 7. 复查代码与本地基线

- [Go 网关的 Luna 策略与重新校验](https://github.com/lizhongxuan/morde/blob/936389c24ea61db45eded881423cef74d8815ca8/server/internal/app/model_gateway.go#L492-L514)；[响应与 256 KiB 校验](https://github.com/lizhongxuan/morde/blob/936389c24ea61db45eded881423cef74d8815ca8/server/internal/app/model_gateway.go#L347-L374)；[Responses 已识别 length 和 content filter](https://github.com/lizhongxuan/morde/blob/936389c24ea61db45eded881423cef74d8815ca8/server/internal/model/responses.go#L478-L495)。B07 从终止状态走查，不要只改 timeout。
- [Task 与平台账户对预算的不同检查](https://github.com/lizhongxuan/morde/blob/936389c24ea61db45eded881423cef74d8815ca8/server/internal/store/postgres/model_usage.go#L175-L220)；[未知 Task 用量处理](https://github.com/lizhongxuan/morde/blob/936389c24ea61db45eded881423cef74d8815ca8/server/internal/store/postgres/model_usage.go#L1180-L1213)；[请求里的 TaskBudgetRequest / TaskBudget / TaskUsage](https://github.com/lizhongxuan/morde/blob/936389c24ea61db45eded881423cef74d8815ca8/server/openapi/portal.yaml#L12877-L12908)。不要把现有 TaskUsage 的可空用量误读成可空的 TaskBudget。
- [回复字段 2,000 rune](https://github.com/lizhongxuan/morde/blob/936389c24ea61db45eded881423cef74d8815ca8/server/internal/app/conversation_route_decision.go#L27-L41)；[直接回复的原会话原子交付](https://github.com/lizhongxuan/morde/blob/936389c24ea61db45eded881423cef74d8815ca8/server/internal/store/postgres/conversation_route_actions.go#L1032-L1045)；[32 KiB 助手消息边界](https://github.com/lizhongxuan/morde/blob/936389c24ea61db45eded881423cef74d8815ca8/server/internal/app/task_store.go#L10-L16)。32 KiB 是需核对和保留/替代的保存边界，不等同模型输出上限。
- 离线回归：在匹配 lockfile 的 runtime 副本内先运行 `npm run build:dsh-compaction`（现会先校验并应用 DSH request-budget patch，再构建 compaction），再运行第 8 节 Node 测试命令。默认值探针 `test/model-budget-dsh-default.test.mjs` 注册本地 `LlmAdapter`，验证 signal 默认值按 step 更新、显式 2,048 上限和并发 Agent 隔离；request hook 探针 `test/dsh-request-budget-hook.test.mjs` 验证 header 冻结前的动态 cap 与最终输入/工具历史一致。均不访问远程端点。

## 8. 本轮实施与验证记录（2026-11-27）

- **方案复核：** 已逐项核对 Downloads 中的《Morde_个人Agent_模型预算与可靠交付实施方案.md》。采纳其 P0–P4 分阶段门禁、预算等待截止/唤醒/公平性、跨滚动窗口继续计入在途占用、同次 Schedule 触发共享额度、显式加额幂等/范围/时效，以及固定样本和质量验收要求。对应修改已写入第 5、6 节；不把示例 `4,000` 作为默认，不照搬提议字段作为 API，不变更旧 Task/Schedule 合同。
- **B01 分层结果：** DSH hook 与 Worker/Runtime 双向 IPC 协议探针通过；Go `ModelBudgetPlanner` 是测试 seam，没有生产 planner 实现或 ledger。真实 Morde Worker → runtime → ModelGateway → 本地 `httptest` 假 provider 链通过，假计划 cap 为 **3,072**，provider 收到 `max_output_tokens=3,072`；验证了 prepare/final 摘要和身份，且没有 fallback 绕过网关。该本地假 provider 不连接 PostgreSQL，也不代表真实模型计费。B01 生产门禁仍未通过。
- **预算安全限制：** 模型预算 admission 默认关闭，生产 wiring 未注入 planner，legacy `max_tokens` 路径仍生效。DSH opt-in 协议探针使用 fake planner，不创建 PostgreSQL hold；独立的本地集成测试则直接覆盖 store-backed prepare/settle。账本与 DSH 生产链路、失败 attempt 和重启恢复门禁未完成前不得启用生产 admission。协议仍遵守 ModelGateway 已有 32,768 最大值，本轮没有移除或证明该固定网关边界是模型能力上限。
- **B02 能力目录实现：本地实现与自动验证通过；PC 端真实后端读写未验。** 新增输出最大值元数据迁移、模型目录、连接 API/OpenAPI、个人模型设置和管理后台展示。系统精确路由展示目录来源，unknown 显示“暂未识别”；个人自定义路由可设置覆盖，管理员共享路由表单不显示后端拒绝的覆盖字段。用 `make pc` 重建后在当前源码客户端打开“设置 → 模型与工具”，实测了上限字段/unknown 展示；当前测试服务端仍显示 unknown，未验证它能返回目录值或保存新覆盖。未在远程测试服务器写入配置。
- **B03 实施状态：** 初始计算器已扩展为按精确路由配置的六阶段版本策略快照，并接入 opt-in plan 计算；线上影子日志/指标、离线固定请求对照和发布前核验仍未实现，因此 B03 未勾选。
- **B01/B04 未完成项：** 本地 PostgreSQL 已覆盖账户/Task/项目预留、settle、过期释放、竞争和已知超额差额；仍缺 DSH 生产 planner 接入、压缩真实 ledger 结算、dispatch crash/unknown/no-dispatch 恢复、等待期限与公平唤醒及未知费用的人工对账闭环。B01、B04 不勾选，不能开放生产 cap。PC 预算功能尚不存在；此前 PC 验收仅覆盖模型连接页展示，不覆盖预算发送策略或真实模型场景。
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
- **尚未验证与安全界限：** 本轮没有完成 `internal/store/postgres` 全包回归；仅完成上述定向 PostgreSQL 集成与 migration 测试。store 级用例已证明：调用方明确保证 provider HTTP 尚未开始时可以释放已消费但未发送的预留；stale `dispatched` 会保留为 `unknown`、阻止清理/新 prepare/重复派发。仍未覆盖崩溃注入和 DSH → store → fake endpoint 计数的端到端 no-dispatch/unknown recovery；第 9 节写入时 recovery attempt 与 adaptive plan 尚无兼容合同；第 10 节补充了独立 recovery DSH/ledger identity 及 PG 定向验证。等待队列唤醒/公平性、人工 overage 对账 UI、生产 planner + PostgreSQL + provider 的端到端链路、真实模型账单和 `make pc` 预算流程也未验。预算 planner 仍未进入生产 wiring，普通 Task 与现有 2,048/32,768 行为未改变；没有请求 DeepSeek API、连接测试服务器、部署或通过 SSH 修改远程主机。
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
- **仍未完成：** runtime admission 测试与 PG → gateway → HTTP fake provider 测试仍彼此独立，尚未组成一个完整 DSH runtime → PostgreSQL → fake provider 进程级链路；全 `internal/store/postgres` 包 10 分钟超时，尚无整包通过结果；崩溃注入、容量等待/公平唤醒、unknown overage 人工结算 UI、线上影子采集或真实 DeepSeek/provider 计费核对。生产 planner 仍未 wiring，普通 Task 实际发送 cap 未改变，B01/B03/B04 不勾选，B05–B10 未开始。没有使用提供的 API key、远程服务器凭据或修改远端机器。

## 11. 容量等待与用户状态验证记录（2026-11-28）

- **入队判定与状态：** 只有账本 prepare 明确返回模型并发限额（`model_concurrency_limit` 且阶段为 `reservation`）时才创建等待；provider 自身限流、账户/Task/项目硬预算不足和未知结果不会误入自动重试队列。PG 事务校验 Task/owner/initiator/lease/Agent/recovery/request/checkpoint fence，写 5 分钟 wait row、转换 Task 为 `waiting_user` 并清空 worker lease。持久等待失败时拒绝继续执行。DSH 运行器收到已持久化等待标记后退出，不占 Worker 槽；否决消息写回失败也保留安全等待信号。
- **唤醒、暂停和通知：** Portal reconciler 每轮最多处理每 owner 的最老 waiter，按账户 → 项目/成员 → Task → wait/ledger 的锁序检查。已 deferred waiter 更新 `last_reconciled_at`，使同一 owner 后续更小请求能在后续轮次被检查。只有旧 Agent stop receipt 摘要与 checkpoint/recovery identity 全部匹配，才允许 requeue、timeout pause 或 invalidation pause；没有可信 stop receipt 时保留 wait row 并 defer。等待和暂停写 `task.status` 事件，避免将自动重试误报为需用户处理的通知。达到容量时交给现有 `ClaimNextTask`，硬额度、wait row 的 5 分钟 deadline 或运行条件失效则安全暂停并删除等待行；requeue 删除 wait row，若同一调用再次被挡住会创建新 deadline，故整体等待总时长尚未界定。
- **用户展示：** Task summary 增加安全 `waiting_reason`；Task attention 将等待显示为“等待模型容量”且无操作按钮；超时、最低预算不足及等待条件失效显示各自的暂停说明。任务等待、超时、失效状态由 Portal 单测覆盖。`make pc` 当前源码构建完成；没有在远程测试服务创建 Task 或写入数据，因此实际 PC 中带真实容量等待记录的手工状态验收仍待安排。
- **通过的验证：**
  - `server`: `CH_TEST_DATABASE_URL=<本机隔离 PostgreSQL DSN> GOPROXY=off GOSUMDB=off go test ./internal/store/postgres -run '^(TestPostgresModelBudgetCapacityWait.*|TestPostgresTaskModelRecoveryPreparesDistinctBudgetAndDSHIdentities|TestPostgresAdaptiveBudgetPlannerGatewayAndLedgerUseOneOutputCap|TestPostgresTaskModelBudgetPreparationReservesOnceAndSettles|TestPostgresExpiredPreparedModelBudgetIsNotDispatchedAndIsReleased|TestPostgresAdaptiveProjectModelConfirmedNoDispatchReleasesAllHolds|TestPostgresStaleAdaptiveModelDispatchBecomesUnknownAndCannotReplay|TestPostgresAdaptiveModelOverageFreezesAndRecordsKnownDifference|TestPostgresAdaptiveProjectModelOverageRecordsTokenAndMoneyDifference|TestPostgresConcurrentTaskBudgetPreparationsShareAccountAndProjectLimits|TestPostgresProjectFeeIntegerRoundingCapsAdaptiveOutput|TestModelBudgetPreparationMigrationEnforcesPlanContractAndRollbackGuard|TestModelMaxOutputCapacityMigrationUsesExactCatalogAndLeavesUnknownRoutesUnknown|TestPostgresTaskModelRecoveryUsesDistinctLedgerIDAndReplaysSettledResponse|TestPostgresTaskModelRecoveryPersistsAndConsumesOneStableAttempt)$' -count=1` —— **定向 PG 账本、恢复与容量等待组通过**；连接本机隔离 PostgreSQL。
  - `server`: `GOPROXY=off GOSUMDB=off go test ./internal/app -count=1` —— **通过**；覆盖预算等待精确分类、Task attention 和既有 app tests。
  - `server`: `GOPROXY=off GOSUMDB=off go test ./internal/worker -run 'TestCloudDSHModelBudget|TestCloudDSHCapacityWaitSurvivesBudgetDenialPipeFailure|TestCloudDSH.*Recovery' -count=1` —— **通过**。
  - `server`: `GOPROXY=off GOSUMDB=off go test ./internal/api -run 'TestTaskSummaryCarriesSafeWaitReason|TestTaskValidationEvidenceIsDetailOnlyAndBoundToResult' -count=1`、`go test ./internal/domain/task -count=1`、`go test ./cmd/portal -count=1` —— **通过**。
  - `web/portal`: `npm run typecheck` —— **通过**；`npm exec -- vitest run src/routes/Assistant.test.tsx -t 'shows a model capacity wait|explains when a model capacity wait is invalidated'` —— **2/2 通过**。
  - 根目录 `make pc` —— Portal、Workbench 与当前 Morde PC 源码构建完成；Vite 输出已有的大 bundle 警告仍存在。真实 PC 没有装载一个实际 PG 容量 waiter 记录，因此只记构建通过，不记该状态的手工验收通过。
  - `git diff --check` —— **通过**。
- **安全与未覆盖：** 未使用提供的 DeepSeek API key，也未连接测试服务器、写远程数据、部署或 SSH。`internal/store/postgres` 全包回归此前曾在 10 分钟超时后中止，本轮只记录定向组通过；没有真实模型计费、生产 planner 接线、线上影子数据、manual overage UI 或持续流量公平压测。
