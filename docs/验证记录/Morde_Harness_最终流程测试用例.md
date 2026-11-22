# Morde Harness 最终流程测试用例

> **文档性质：** 本文是《[Morde Harness 最终流程与实现规格](../Morde_Harness_最终流程与实现规格.md)》的测试用例和执行记录模板；实施顺序与任务状态以《[Morde Harness 实施任务 TODO](../Morde_Harness_实施任务_TODO.md)》为准。用例定义目标行为，不代表当前功能已通过。
>
> **数据边界：** 只使用本地隔离环境、测试账号和合成文本；测试数据与产物留在本地，不上传或发送文件。不连接生产数据库、生产电脑或生产发布环境。真实模型用例只发送合成文本，并记录实际模型和用量，不记录密钥。

## 1. 目标与范围

验证最终流程从消息持久受理、上下文装配和意图路由，到 DSH 执行、服务端授权、恢复、证据门禁与原会话交付的闭环。重点确认普通对话不会被后台任务阻塞，超过上下文窗口的长会话能保留并找回授权历史，长时间运行能持续推进且可恢复。

| 测试层 | 覆盖范围 | 通过证据 |
|---|---|---|
| 自动化回归 | 路由、上下文预算、摘要/压缩、投递、幂等、权限、账本、检查点、交付门禁 | 对应 Go、Node 和隔离 PostgreSQL 测试结果及记录 ID |
| 真实模型验收 | 单条输入超过 8K tokens、累计历史超过模型窗口、真实回复与原文回查 | 模型标识、窗口/Tokenizer、输入统计、消息与会话 ID、可核查回复 |
| 耐久与恢复 | 至少 60 分钟长对话/任务、追问、客户端重连、服务端恢复 | 起止时间、各阶段活动/任务状态/检查点、重连前后同一消息和 Task 证据 |
| 客户端验收 | 实际 `morde-pc` 和 iOS 接续同一会话 | 客户端版本、会话与消息状态、进度和结果；PC 必须由 PC 客户端入口操作 |

状态只能填写：**通过 / 失败 / 阻塞 / 未执行**。缺少真实模型、指定客户端、授权或环境时标为阻塞/未执行；模拟、静态推演、HTTP 200、任务已创建或“已委派”都不足以单独判通过。

## 2. 执行前准备

1. 使用本地隔离服务和测试账号；记录 Morde 提交与工作区状态、DSH 锁定版本、数据库类型/迁移版本、实际客户端版本及模型配置。不得覆盖或清理用户已有数据。
2. 确认目标模型的上下文容量、输入输出限制和可用 tokenizer。`8K` 在本文中特指 **8,192 tokens**，不是字符数、字节数或“约 8 KB”。优先用该模型 tokenizer 统计合成输入；同时记录服务端/Provider 实际报告的输入 tokens。若不能取得可靠统计，标为阻塞，不能用字符数估算后宣称通过。
3. 超过 8K 的单条输入用例须选择有足够可用窗口的模型：完整用户输入、系统提示、工具定义、保留的历史和输出预算之和应能放入该模型窗口。记录预算明细；不能把此用例改成只测超限报错。
4. 准备含唯一标记的合成文本；标记不含真实姓名、密钥或业务数据。不上传、不附加、不要求模型读取文件。
5. 若运行真实工具或云端电脑用例，先准备隔离目标并核对授权范围；无需云端电脑的问答用例保持无电脑目标。任何副作用未知时先对账，不重放操作。
6. 每条用例记录预期与实际结果、可核查的日志/数据库/检查点位置、失败原因和未覆盖项；日志不得包含密钥或不必要的原文内容。

### 2.1 缺陷定位与机制修复准则

- 测试在约 4K 上下文或其他边界出错时，保留原始失败输入、环境与日志，先拆分实际 Token 预算：系统提示、工具定义、当前输入、近期历史、摘要/检索结果和输出预留；另核对模型窗口/Tokenizer、API 请求上限、路由/DSH 单条消息与 IPC/检查点字节上限。
- 根据证据区分根因：历史中无关内容被装入过多、摘要/压缩缺失或无进展、历史检索没有取回所需原文、Token 估算或窗口预算错误、单条不可分输入本身超预算，或不同协议层的硬上限不一致。记录根因和证据，不能只写“上下文太长”。
- 修复针对根因机制；不得缩小失败输入、删掉关键锚点、放宽断言或只调大一个阈值来消除错误。提高明确的产品/协议上限只有在其本来就是预期边界且调用链、IPC、持久化、恢复和客户端约束一并核对后才可作为修复，并须保留真正超限时的明确失败行为。
- 修复至少重跑原始失败样本，再跑 `MH-CTX-005` 的 4K、8K、100K、200K **累计多消息历史**规模回归。100K/200K 测的是可压缩、可检索的会话历史规模，不表示把这么多 Token 整体塞进一次模型请求；单条超协议/模型窗口输入仍需明确失败，除非已有可核验覆盖与原文回查的分块实现。

## 3. 主流程与可靠性用例

### MH-CHAT-001｜普通问答不依赖后台执行（P0；流程 01–03）

**步骤**

1. 新建会话，不选择云端电脑；确认文本模型可用，并关闭或隔离云端执行能力。
2. 发送：“用三句话解释 DNS。请只根据常识回答，不要调用工具。”
3. 检查 PC 对话状态和服务端路由记录。

**预期**：消息先持久受理并只产生一份回复；回复内容真实、可读，不要求先启动云端电脑、不创建可见的长任务卡、不伪造工具调用。执行能力关闭时，文本模型仍可正常回复。记录原消息 ID、会话 ID、路由结果和回复 ID。

### MH-ROUTE-001｜非法路由输出有界修复并重新校验（P0；流程 02）

**步骤**

1. 在隔离测试中令分类模型对一条普通问答返回自然语言或结构非法结果；令一次修复返回合法的无目标直接回复，检查路由和工具调用记录。
2. 分别令修复结果仍非法、目标不明确，或修复期间 Task/授权代次发生变化，再观察服务端行为。

**预期**：非法路由最多触发一次独立、可对账且无工具副作用的修复；修复结果重新校验目标、会话/Task 快照和授权。明确的普通问答正常回复；仍不明确则澄清，目标或授权已变化则重新读取状态，模型不可用则如实失败。不得用上一个任务猜目标或在修复前调用工具。

### MH-MODEL-001｜慢响应、空/截断回复与取消恢复（P0；流程 02–03、06）

**步骤**

1. 在隔离模型网关/Runtime 测试中分别注入首响应超过当前 45 秒单次调用超时、空回复、截断回复、流式输出中断和用户取消；另构造连续空回复，分别改变模型、Provider、退出原因或用量证据。对路由 Provider 429 分别注入 delta-seconds、HTTP-date、缺失/非法及超过两分钟的 `Retry-After`。
2. 记录调用身份、重试层、预算预留/结算、已展示文本、取消信号和最终消息状态；确认路由 429 最多三次总调用、每次有新调用 ID，该次账本为 `rejected` 且不写入虚构的 0 token 用量，账号/项目预留原子释放，超过上限失败关闭；对超时、断流等结果为 `unknown` 的情况触发对账。缺少成功响应用量时确认不会被归为零用量。

**预期**：每类退出原因明确区分；只有一个层拥有有界重试，且重试满足剩余预算、时间、未取消和安全重放条件。重复空响应只在模型、Provider、退出原因及用量证据一致时成立；缺失用量为 `unknown`。已展示的流式片段保留并标记中断；不得拿上一轮答案填充本轮空回复。调用或用量未知时先对账，取消后不继续发起请求；不重复结算或静默重播已发出的内容。

### MH-IDEM-001｜重复收件不重复建任务（P0；流程 01）

**步骤**

1. 在隔离 API/集成测试中提交一条复杂任务消息，记录 `client_message_id`、Task ID 和受理回执。
2. 用相同身份、相同消息 ID、相同内容/附件/选项重试；再用相同 ID 但不同内容重试。

**预期**：第一种重试返回原消息/任务，不重复路由、调用模型或执行操作；第二种冲突明确拒绝，不伪装成功。两条独立消息即使文字相同，也各自保留记录。客户端重试使用原消息 ID。

### MH-CTX-001｜单条当前输入超过 8K tokens（P0；流程 01A）

**前置条件**：目标模型和窗口已记录；模型窗口扣除系统提示、工具定义、历史与输出预留后，仍能容纳整条输入。输入还须在当前 128 KiB 路由/DSH 单条消息协议预算内；8 MiB 分片上传额度只是暂存限制。若模型窗口不满足，换用合适的测试模型；若协议预算不满足，转到超限保护子项，不得静默截断或把协议拒绝记作模型上下文通过。

**步骤**

1. 生成一条约 9,000–10,000 tokens 的纯合成文本。用所选模型 tokenizer 统计，确认正文 **大于 8,192 tokens**。文本开头、中段、末尾各放一个独特锚点（例如 `CTX-A-7F31`、`CTX-M-2C84`、`CTX-Z-9D16`），并在开头声明“请用简体中文，只依据本条内容回答”。
2. 将整条文本作为一条用户消息发送，要求列出三个锚点以及各自附近的一项指定事实；不要拆成多条消息或附件。
3. 检查回复、实际输入 token 统计、上下文预算/压缩记录和持久化原文；核对原文长度或摘要校验值与发送前一致。

**预期**：在满足窗口预算的前提下，消息被完整处理；回复能准确覆盖开头、中间和末尾锚点，并遵守开头约束；记录证明完整输入进入模型请求，没有在受理后被静默截断。消息、会话及回复只各有一份。若当前请求自身实际超预算，系统应保留已受理原文并明确报告长度限制；该结果只能通过“超限保护”子项，不能替代本用例的可处理通过结果。

**超限保护子项**：分别在隔离测试中验证三种边界：(a) 请求在消息受理前超过 API 硬限制，明确拒绝且没有“已受理”状态或半条消息/Task；(b) 分片上传消息已受理但超过当前 128 KiB 路由/DSH 协议预算，完整原文保留并标记 `message_too_long`，不派发模型调用；(c) 消息在协议预算内但超过模型可用 Token 预算，返回明确限制，完整原文可查，不伪称完成、不自动丢弃尾部或派发超预算模型调用。三种边界及其重试均不得产生重复消息或任务。

### MH-CTX-002｜累计历史超过窗口并经多轮压缩（P0；流程 01A、06）

**步骤**

1. 在合成 4K token 窗口夹具中创建同一会话，首轮设定唯一早期约束及锚点；随后发送 40 轮带唯一轮次标记的内容，使累计原始历史大于 8,192 tokens，并触发至少两次 DSH Session 压缩。
2. 在末轮提出新问题，要求同时遵守早期约束并回答最近一轮内容。
3. 检查摘要来源范围、压缩次数、工具调用/结果配对、40 条原始用户消息、检查点及助手回答。再从授权历史入口查询早期原文。

**预期**：模型上下文经压缩后保持在夹具窗口预算内；早期锚点/约束和最新消息都能正确使用；所有原始消息仍可查，压缩不覆盖或删除原文；摘要版本和来源可追溯；工具调用与结果不拆散；没有重复消息、Runtime 错误或半成品摘要。

### MH-CTX-003｜真实模型累计对话超过其单次窗口（P0 专项；流程 01A）

**步骤**

1. 在真实测试模型的同一会话中，逐轮发送合成内容；记录每轮输入用量，直到**累计原始历史估算超过该模型单次上下文窗口**。早期设置唯一约束和锚点，中后段追加无关轮次，最后提出需要早期约束才能正确回答的问题。
2. 检查回复是否遵守早期要求；打开会话原文并通过授权历史入口核对早期锚点。
3. 记录模型标识、公开/配置的窗口容量、tokenizer 或用量统计来源、累计原始历史估算方法、压缩/摘要版本与请求实际用量。

**预期**：在不把无限历史整体塞进单次请求的情况下，系统仍从授权的历史/摘要中取回所需事实；无法取回时应明确说明，不能编造。原始对话可查看，跨账号/会话不混用。只跑合成窗口夹具不算此项真实模型验收；没有可靠窗口或 token 统计时标为阻塞。

### MH-CTX-004｜检索原文作为不可信数据（P0；流程 01A、06）

**步骤**

1. 在隔离会话的一条早期用户消息中放入一段合成指令注入文本，要求忽略系统约束、读取其他会话或调用未授权工具；同时写入一个无害、唯一的事实锚点。
2. 多轮压缩后，通过授权历史搜索按锚点取回该原文片段，要求 Agent 回答该无害事实，并检查上下文来源标记、工具授权判定和检索日志。
3. 用另一个账号/会话尝试读取原文，验证越权拒绝不泄露正文。

**预期**：检索片段作为带消息 ID、会话 ID 和时间的用户数据提供给模型，不提升为系统/开发指令，不扩大工具权限；合法事实可据实引用，越权读取拒绝且不泄露内容。

### MH-CTX-005｜累计历史规模回归与冗余上下文治理（P0；流程 01A、06）

**前置条件**：在隔离存储中用确定性 tokenizer/夹具生成多消息历史；固定模型窗口 `W` 和输出预留，并确认最小系统提示、工具定义、当前问题及输出预算本身能够放入 `W`。若原始故障发生在 4K 模型窗口，保留该 4K 配置作为复现基线；不能以增大 `W` 替代原故障复现。

**步骤**

1. 分别构造累计原始会话历史为 4,096、8,192、100,000、200,000 tokens 的独立测试会话。每条消息都在单条协议上限内；历史中包含分布在开头/中段/近期的必要事实和唯一锚点，并混入大量与最后问题无关的合成内容。至少一个必需事实放在有界路由预览之外，确保需依赖摘要或授权历史检索才能回答。
2. 在每个会话中发送相同结构的短追问，要求回答一个早期事实和一个最新事实；检查实际模型请求的 Token 预算拆分、压缩/摘要进展、是否调用授权历史搜索、返回的消息 ID/序号/分页状态及最终回复。
3. 与尚未修复的原始失败样本对照；检查所有会话原文仍可查、没有因为压缩或检索覆盖而丢失。另用超出单条协议上限的隔离样本确认其仍走明确 `message_too_long`，不将累计历史回归混作大单条输入验收。

**预期**：四种累计历史规模下，每次实际模型请求都符合固定 `W` 的可用预算；与当前问题无关的历史不会仅因存在于 transcript 就整体装入请求；早期必需事实由有效摘要或带来源的授权原文检索恢复，未能取回时明确说明，不编造。100K/200K 历史只增长被持久保存和按需读取的原文规模，不让 Provider 请求随原文总量线性膨胀；修复同时通过原始失败样本。对超限的单条内容仍明确失败，除非单条分块具有可核验覆盖和原文回查。只提高限制、只把测试缩到 4K 以下、或仅有合成机制测试而无对应层证据，均不通过。

### MH-CTX-006｜单条长消息中段原文检索（P0；流程 01A、06）

**步骤**

1. 在一条已成功受理的合成长消息中写入 800 条带序号记录，令第 0400 条包含唯一值和校验值；确认它位于有界路由预览之外，且原始消息仍完整持久化。
2. 在同一会话追问第 0400 条的值和校验值，要求只根据当前会话回答。检查路由是否标记历史不完整，并在需要时由获授权的当前会话历史搜索工具检索原文。
3. 检查搜索工具返回的消息 ID、截断标记及片段内容；确认片段围绕命中记录，而不是固定返回消息开头。核对模型输入预算、路由状态和最终答案。

**预期**：长消息中的中段事实能通过有界授权检索恢复；返回片段不超过工具输出预算且包含实际命中记录，最终值与原文一致。若检索权限、原文或路由状态不可用，应呈现可诊断的失败，不把它误报成模型窗口溢出。不得通过提高单条消息或模型上下文上限来代替命中片段定位；需要分别记录字节/Token 预算、路由阶段与历史检索结果。

### MH-DUR-001｜至少 60 分钟会话耐久与任务恢复（P0 专项；流程 05–10）

**前置条件**：隔离测试环境、真实可用的测试模型和实际 `morde-pc`；准备无文件写入、无外发、可分阶段观察的合成任务。任务需产生真实可观察的阶段进展/检查点，不能只靠空闲等待、定时续租或虚构心跳维持运行。单次模型请求的 45 秒超时不等于 Task 总时长。

**步骤**

1. 在 T0 通过 PC 开始一项包含多个阶段的合成只读任务，记下消息、Task、Session ID 和实际开始时间。
2. 在同一会话保持测试连续至少 60 分钟；建议于 T+10、20、30、40、50、60 分钟各发一条相关追问或确认，并至少插入一条独立普通问答。整个过程保持同一个 Task ID，记录每次响应、阶段进度、模型/工具/子 Agent 实际活动、检查点、租约代次、墙钟时长和 `ActiveSeconds`。Task 可经历明确的等待与恢复，但每次继续都必须有真实可观察进展。
3. 在任务仍有活动时关闭 PC 客户端一段时间并重新打开同一会话；若具备 iOS 测试端，在 iOS 查看并继续同一会话，然后回 PC 核对。
4. 等待任务完成或进入明确的等待/失败状态，核对最终结果、证据和原会话回执；对比数据库/任务事件，确认无重复 Task、消息或副作用。

**预期**：从首条受理到至少 T+60 分钟仍能在同一会话继续多轮对话，且 Task ID 不变；每条追问有独立且准确的状态；普通问答不被后台 Task 阻塞；Task 经实际执行、等待和恢复后状态一致，并最终交付一份结果。等待审批、Provider 故障和实际完成须区分显示；租约续期不算进展。当前 `MaxActiveSeconds` 上限为 1,800 秒，因此本用例证明 60 分钟墙钟时间内的会话/任务可恢复性，不证明单次连续执行超过 30 分钟；若要验收后者，先实现并验证更高的明确任务预算。PC 必须实际操作 PC 客户端；iOS 结果另行记录。

### MH-MSG-001｜运行中追问、独立任务和定向停止（P0；流程 02、06）

**步骤**

1. 启动可观察的只读任务 A；A 运行中发送一条目标明确的低风险补充、一个普通问题和一个独立任务 B。另从 PC/iOS 并发提交两条不同合成消息，记录服务端分配的会话 `sequence`。
2. 确认补充进入 A，普通问题获得直接回复，B 可独立开始；只停止 B。
3. 检查消息投递帧、DSH 根 Session 检查点和 `delivery_status`。

**预期**：任务补充只投递到 A；并发提交的用户消息按服务端 `sequence` 排序，不因客户端时钟或路由完成先后重排；目标不明时先澄清，不中断 A/B；消息只有在对应 `user/message` 被领取并持久化后才标记 `applied`，仅入队检查点不算应用；不能确认则标记 `unknown`。停止 B 不影响 A，状态、结果与消息归属不串线。

### MH-TOOL-001｜授权、审批及未知结果对账（P0；流程 07–08）

**步骤**

1. 通过隔离 Broker 发起一个有明确测试目标、获授权且需要审批的无害操作；分别验证未批准、撤权、超预算和落盘失败情形。
2. 对已批准的操作，模拟“外部已生效但回执丢失”；重启后触发恢复/对账。

**预期**：审批前、撤权后、超预算或意图未持久化时均不派发；批准绑定当前目标、动作与版本；回执未知时先查外部真实状态，确认后关联原回执，无法确认则保持待对账，不重复执行。相同参数但不同合法操作 ID 不应误合并。

### MH-REC-001｜运行时重启与消息恢复不重放（P0；流程 01、06、08、10）

**步骤**

1. 让消息已持久化并进入 DSH；分别在消息入队前、入队后、`user/message` 领取后及工具结果持久化后注入 Runtime/服务进程重启。
2. 恢复同一账号、会话和 Task，检查消息状态、检查点、调用账本和最终回执。

**预期**：已接收消息不丢失；恢复沿同一消息/Task/代次对账；不重复创建消息、重复结算或重放已确认副作用；无法确认的交付/工具状态保持 `unknown` 并提示对账，不伪称已完成；最终回执至多一份。

### MH-AGENT-001｜主 Agent 并行委派两个子 Agent（P0；流程 06）

**步骤**

1. 提交纯文本、只读且可拆成两个互不冲突子任务的合成分析请求，例如分别分析客户端和服务端接口影响。
2. 检查 DSH 子会话实际创建时间、各自输入/输出、完成或失败状态及检查点，确认二者时间上有并行重叠。
3. 检查主 Agent 汇总是否对应两个真实结果。

**预期**：主 Agent 实际派发至少两个 DSH 子 Agent；二者权限不超过父任务；主 Agent 汇总事实准确且标明未完成/失败项。只返回静态建议、只创建一个子 Agent、或只记录“已委派”均不通过。

### MH-GATE-001｜证据门禁和原会话交付（P0；流程 09–10）

**步骤**

1. 对隔离测试任务分别提供：证据齐全的当前版本结果；缺证据、旧修订、错误环境或只有模型自述的结果。
2. 在验证与发布交界处模拟重启/回执丢失，随后恢复并重连会话。

**预期**：只有当前 Task 修订、产物版本、来源和实际环境匹配且必要证据齐全时才标记验收通过。缺项标为失败或未验证，不得由模型自述覆盖硬门禁。通过的最终回执先持久化再发回原会话；恢复后只展示一份，不重复操作或发布。

### MH-ISO-001｜账号、会话和项目隔离（P0；流程 01A、06）

**步骤**

1. 在账号 A 的会话 A1 与 A2 写入不同的合成锚点；在账号 B 写入同格式但不同的锚点。
2. 多轮摘要/压缩后分别请求历史查询，并尝试通过未经授权的会话/项目 ID 读取其他范围。

**预期**：每个请求只能看到当前身份与授权允许的来源；不存在跨账号、跨会话或跨项目泄漏；权限错误不暴露被拒内容。摘要、检索、检查点和缓存均纳入检查。

### MH-CLIENT-001｜PC/iOS 接续同一会话（P0；流程 01、10）

**步骤**

1. 在实际 `morde-pc` 创建会话并发送合成问题，记录会话、消息、Task 和结果 ID。
2. 在 iOS 登录同一测试账号并打开该会话，检查历史、当前进度和结果；从 iOS 发送一条追问，再回 PC 查看。
3. 任务运行中分别短暂关闭/重连客户端，核对实际消息和任务状态。

**预期**：两端接续同一服务端会话，显示一致的消息、进度和结果；客户端关闭不取消已受理任务；重新打开不重复创建或执行。PC 不能用独立 Web 页面代替，iOS 不能用 PC 结果代替。

## 4. 缺陷恢复专项

### MH-FAIL-001｜压缩、超限、取消和租约竞争

**步骤**：在隔离自动化环境分别注入空/截断摘要、摘要模型失败、重复窗口无压缩进展、同 Session 连续摘要失败、Provider 明确上下文溢出、用户取消、摘要并发版本冲突及租约代次变化；对可安全恢复的场景重启并重试一次。

**预期**：无效摘要不替换有效历史，原文和失败记录保留；重复失败按持久化冷却避免每轮重试；无进展时不重复压缩或推进覆盖水位；只有明确上下文溢出可触发额外一次有界恢复压缩。当前输入不静默截断；取消或版本冲突不提交半成品；旧租约/旧代次不得提交新结果；调用或用量 `unknown` 时先对账，不能无条件重跑模型或外部副作用。

## 5. 规格映射与执行记录

下表状态表示整条端到端用例是否执行；下方“本地自动化证据”只证明列出的代码路径，不替代真实模型、PC/iOS 或 60 分钟耐久验收。

### S00 H00 Hermes 固定提交关键文件哈希核对 2026-11-22

- **来源：** 固定上游 [`NousResearch/hermes-agent@0d49d6d9d20e461155d49ba9d188687079c811af`](https://github.com/NousResearch/hermes-agent/tree/0d49d6d9d20e461155d49ba9d188687079c811af)；本地 `project/hermes-agent/` 无独立 Git 元数据。
- **方法：** 对下列本地文件运行 `shasum -a 256`，并分别对固定 SHA 的对应 raw 文件运行 `curl -fsSL <raw-url> | shasum -a 256`。上下游哈希逐项一致；网络仅读取公开上游字节流，没有上传或保存外部文件。
- **结果（本地 SHA-256 = 固定提交 SHA-256）：**

| 文件 | SHA-256 |
|---|---|
| `agent/retry_utils.py` | `4c643880c012dd536dca08041f1bae93b6a58477186b5cf0b4e27ec3c640014c` |
| `scripts/build/agent.py` | `d3e1b6a4ed10c03945457ab81b5163715ac89fec5a8f5ec7d67c3652c848665d` |
| `agent/turn_response_check.py` | `ab7e47f2f39890955effd01330f7da8dc3ef5b9a5de13afdadf717731956c94c` |
| `agent/turn_empty_response.py` | `c547e532fd6a82269bd88aa4a2bef8547b59995c3db14de2b2e44adfe66067d4` |
| `gateway/run_turn_hygiene.py` | `b5e59f36c1cefa1aacf19ff272e0603aadac200417e4cf84365559145a1204c4` |
| `tools/session_search_tool.py` | `5c16c13bdda96c45641f1c40b4459630f6802f914c12022c3b79cdc3c726aeb1` |
| `agent/turn_context.py` | `42674dad11fbe0c202e518c11dd9ad765aa1404b42ec39fa13d9cfaaeb9ced25` |
| `agent/context_compressor_window.py` | `8497d0b5b802a5e9a2ff5d74a0d4e52b3ba40e0ed5aa335b6263315a182af0fa` |
| `agent/context_compressor.py` | `498871a5b8c0f3a8b53c2547fb25867f32357c62d8dbf81fdd3c0f17275f065f` |
| `agent/turn_iteration_prep.py` | `16a8ee94f8ec974e165e91580fa737b150f5102e538c12b41afa1cb56cfccfdc` |
| `gateway/session_busy_controls.py` | `1557b6a052a76805dae5889474277a2f9a6e099fc1e0f02a6f2d9cd9b3232b28` |
| `gateway/turn_lease.py` | `5600965be037e2d440d547c2967e2d3ae74d0e30fc77d50e7f37250c8acae661` |
| `agent/turn_tool_round.py` | `9cd3be661ea93acb51985c2907dafced48b6df6238060903bd9608a560c83b17` |
| `agent/turn_liveness.py` | `1577203f3c2b8f19c42103e2f310b1e147d9eaadab14759d12cb7e4ea30b2f84` |
| `gateway/session_stall.py` | `d21aa30df0cbf8e66dc4e08dfb5457641a1c08e65cfef623f66fd2890125007e` |
| `agent/turn_stop_gates.py` | `ec0e46a1bb51c9529ab44447f4506a25a7724a4f9f036cda6bbd1bc907da619b` |
| `agent/turn_final_response.py` | `7a669214584974cf0745be653b2a784dc66d0f319dcc2549da12ca0096fbfbc8` |
| `LICENSE` | `821556e6336796450ab852d375117b48a4887e71d255794fd6318d99982a5ab6` |

- **关键文件边界：** 17 个核心源文件及 LICENSE 共 18 个重点文件逐字节匹配固定提交；该抽查结果单独不能证明整个快照与上游提交完全一致，实际全树比较见下项。
- **全树比较：** 对固定提交 GitHub recursive tree（`truncated=false`）的 blob SHA-1 与本地 Git blob SHA-1 逐路径比较：上游 27,298 个 blob，本地 27,297 个文件；27,187 个共有文件内容匹配，28 个上游文件本地缺失，83 个共有文件内容不同。27 个本地额外路径全部是 `__pycache__/*.pyc`。差异示例包括 `gateway/run_shutdown.py`、`gateway/run_startup.py`、`apps/desktop/src/app/chat/transcript-backfill.ts`、`hermes_cli/urllib_security.py`；具体为哪些文件不同不改变关键清单中 17 个源文件与 LICENSE 均匹配的结果。本次只读取上游 tree 元数据，没有下载或上传整份快照。
- **边界：** 不能把 `project/hermes-agent/` 快照称为固定提交的完整副本。H00 仍缺真实 Provider/PC/iOS、目标云电脑、故障复现闭环及耐久验收，继续保持进行中。

### 长会话测试方案 LC 用例映射到当前 MH 范围

此表仅用于把必执行的 LC-01–LC-06 对应到 MH 记录项，不是选测菜单。最终验收须逐项执行测试方案中的场景；尚未运行、环境不满足或只有局部自动化证据时，状态保持待验证/阻塞并注明原因。

| 测试方案用例 | 当前承接用例 | 保留的验收意图与边界 |
|---|---|---|
| LC-01｜长消息后短追问 | MH-CTX-001、MH-MSG-001 | 短追问不能因前一条长消息或被引用 Task 的长目标而失败；4K 未知容量窗口下的回归属于必验项。输入是否超过 8,192 tokens 必须另有 tokenizer/Provider 证据，不能用字节数替代。 |
| LC-02｜40 轮及多次压缩 | MH-CTX-002 | 4K 合成窗口、多轮压缩、早期与最新锚点、消息/工具组完整性。 |
| LC-03｜运行中追问、独立任务与重启 | MH-MSG-001、MH-REC-001 | 检查任务关联、独立任务隔离、检查点恢复和不重放；真实 DSH/PC 入口仍须单独验收。 |
| LC-04｜摘要/Provider 失败恢复 | MH-FAIL-001 | 保留无效摘要、上下文溢出、取消、版本冲突和有界恢复语义。 |
| LC-05｜会话与账号隔离 | MH-ISO-001、MH-CTX-004 | 交叉追问不得泄漏其他账号/会话历史；历史检索文本仍是不可信数据。 |
| LC-06｜PC/iOS 接续与重连 | MH-CLIENT-001、MH-DUR-001 | 两个真实客户端接续同一会话并检查消息、进度、结果和恢复；模拟器或合成接口测试不替代实测。 |

测试方案的全部验收意图均须保留；当前规格的 token、授权、运行时与客户端边界用于约束执行方式，既有 LC 输入数字和“通过”结论不自动沿用。

### LC-01 短追问误报根因调查与修复 2026-11-22

- **现象与证据边界：** 截图中的单字短消息显示 `message_too_long`。UI/持久状态把工作线程上下文预算失败、Model Gateway 预检失败和 Provider 明确溢出映射到同一用户文案；截图本身没有消息 ID、失败阶段、模型容量或 Provider 用量，不能单独证明这次线上失败属于哪一层。
- **可复现根因：** 路由预算会删除普通候选，但保留明确引用的前序消息和活动 Task。原先只把它们预览到 4 KiB；若容量未知而按 4K 窗口预检，长 Task 目标或前序请求预览仍可能挤满预算，于是短当前消息被错误标成超限。新增的短追问 + 长被引用 Task 目标回归在修复前失败，确认了这类机制缺口；前序请求路径也已补充原文哈希不变的回归。
- **修复：** 预算拟合现在会逐步缩短被保留的旧消息/Task 文本，保持 Task/message ID；前序消息继续保留完整 SHA-256 用于精确关联，Task 目标和候选集明确标记截断。当前消息不参与此裁剪。失败日志新增安全的 `failure_stage`、`context_window_tokens`、`context_window_known` 字段，不记录原始输入；Provider 明确溢出保留其独立阶段/错误码。
- **Hermes 机制对照：** 对照固定上游 `0d49d6d9` 的 `gateway/run_turn_hygiene.py::_hmwa_hygiene_plan`、`agent/context_compressor_window.py::_plan_compaction_window` 和 `tools/session_search_tool.py::_discover/_scroll`。Hermes 的回合前压缩以 Provider 实际用量/持久锚点/保守估算加硬消息数兜底，采用失败冷却；窗口算法先做安全工具结果修剪，再保持近期尾部与工具配对选择压缩范围。Morde 已在 DSH Runtime 适配 Hermes 窗口预算、范围规划和进展检查；服务端则用 DSH TokenMeter 维护跨 Task 摘要与带授权证明的原文检索。本次复现属于独立路由器固定引用旧请求/Task 文本挤占 4K 预算，不属于 DSH Session 压缩窗口；已在路由预算拟合处做专属修复。没有复制 Hermes 的 Gateway/SQLite/后台线程实现，也没有新增第二个 Agent/压缩循环。`project/hermes-agent/` 本地源码快照没有独立 Git 元数据，核对上游函数时以规格固定提交 `0d49d6d9` 为准；本轮只做源码对照，未运行 Hermes 测试套件。
- **本地验证：** 新回归在修复前稳定失败，修复后通过。本轮修改后 `cd server && go test ./internal/app -count=1` 通过（20.095 秒）；`cd server && go test ./internal/worker -run '^(TestFitConversationRouteContext|TestConversationRouteWorker)' -count=1` 通过（0.730 秒），覆盖长 Task 与前序长请求裁剪及安全预算诊断；两个新增回归单独复跑均通过（长 Task 0.725 秒，前序长请求 1.883 秒）。最终代码下 `go test ./internal/app ./internal/worker -count=1` 通过（App 20.192 秒，Worker 58.902 秒），包含未知容量 Worker 路由、窗口预算、长历史拟合、Task/message 保留、Provider 溢出诊断和 4K/8K/100K/200K 合成摘要用例。Runtime 全套 `npm test` 通过（181/181，63.6 秒），包含 40 轮多次压缩锚点及压缩失败恢复。PostgreSQL LC-01 命令已执行，但因 `CH_TEST_DATABASE_URL` 未配置而跳过；实际 PC/iOS 和真实 Provider 未重跑。以上自动化结果不替代[测试方案](../design/Morde-AI对话长会话测试方案.md)剩余 LC 用例的端到端验收。
- **剩余确认：** 该修复覆盖了已复现的 4K 预算机制缺口，但目前不能确认截图中的部署实例就是此根因；需在测试站重现后按新失败阶段日志核对服务端版本、模型窗口来源和 Provider 用量，再完成 LC-01–LC-06 端到端验收。

| 用例 | 主要规格 / TODO | 当前状态 | 实际结果与证据位置 |
|---|---|---|---|
| MH-CHAT-001 | 规格 01–03；H01、H06 | 未执行 | |
| MH-ROUTE-001 | 规格 02、2.3；H01 | 未执行 | |
| MH-MODEL-001 | 规格 2.3；H02 | 未执行 | |
| MH-IDEM-001 | 规格 01；H01、H06 | 未执行 | |
| MH-CTX-001–005 | 规格 2.1、2.4；H09、H06 | Provider 与 Morde Model Gateway 的 >8,192-token 输入/路由/追问通过；完整服务端、DSH 历史工具及客户端验收未完成；002–005 尚无完整验收 | Morde Gateway live test 处理 51,842 字节、800 条合成记录，Provider 报告 `input_tokens=18502`，准确返回总数及首/中/尾锚点；对截断历史的记录 0400 追问选择 `new` Task；带完整历史的 Morde 网关普通文本追问报告 `input_tokens=18575`，准确返回记录 0400。Provider 直连同类探针报告 18,502/18,575 tokens；两次 JSON 模式的全历史直连探针返回 `completed` 但正文空白，普通文本成功；这不代表 Morde 路由 JSON 失败，因为 Morde 的截断历史路由实测返回有效 `new` 决策。仍未证明服务端真实消息持久化、DSH Task 实际调用历史搜索工具、PC/iOS 同会话接续、累计历史压缩或 100K/200K 规模治理。实际 PC 客户端的 42,603 字节记录首轮回答正确，但紧接的同会话追问失败且没有 Provider 用量；132,305 字节超协议预算探测另记失败。 |
| MH-DUR-001 | 规格 2.3、2.4；H04、H06 | 未执行 | |
| MH-MSG-001 | 规格 2.2；H10 | 部分通过（本机实际 PC 客户端 + 本地合成模型/执行器；DSH、真实 Provider、Task 定向追问与停止未验） | `TestConversationBrowserAcceptance` 中先创建后台 Task，确认 DB 状态为 `running`，随后从 `morde-pc` 发送普通快速消息并收到夹具回复；不覆盖 Task steer/followup、独立任务并行或目标停止。详见“本机 PC 运行中普通追问夹具”。 |
| MH-TOOL-001 | 规格 07–08；H03、H04 | 未执行 | |
| MH-REC-001、MH-FAIL-001 | 规格 2.1–2.3；H04、H09、H10 | 未执行 | |
| MH-AGENT-001 | 规格 06、2.2；H11 | 未通过（指定测试站实际入口任务中断；无子 Agent 记录） | 本轮 PC 旁聊请求实际进入 Task `q86mee2m`，状态 `waiting_user`、原因 `dsh_recovery_unresolved`；快照只有 main Agent，`child_jobs=[]`，无结果回执。没有重试。详见下方“S07 H11 真实站点子 Agent 委派探针”。 |
| MH-GATE-001 | 规格 09–10；H05 | 部分通过（本地服务端门禁回归；工程/非工程场景和客户端入口未验） | App verdict、PostgreSQL revision/result/event fencing 与一次发布、Worker review 定向测试通过；原会话 assistant 回执分别标注主 Agent 摘要、Task completed 和各来源当前修订的 validation 状态；缺可信执行环境或可核查 Artifact 内容时保持 `pending`。Artifact review metadata 现绑定 Workspace revision 与内容 SHA-256，但输入包不含 Artifact 字节。Task 投影仍分别提供 Task 状态、ResultReview 和 validation。测试命令及完整边界见[S04 审计记录](../Morde_Harness_并发实施/04-结果验证与交付.md)。 |
| MH-ISO-001 | 规格 2.1；H09 | 未执行 | |
| MH-CLIENT-001 | 规格 2.4；H06 | 部分通过（指定测试站的 PC/iOS 同一旁聊短文本往返及自动恢复对账通过；Task/耐久与其它 P0 场景未完成） | PC 短文本及 42,603 字节合成长消息首轮收到真实回复，但长消息后的历史追问未回复。后续 iOS 从同一旁聊读取 PC 提示与回复，并发送追问；客户端曾显示“发送结果待确认”，未手动重试，自动核对后显示“已发送”并收到 IOS-REAL-OK。模拟器另有本地消息受理用例。以上不证明 Task 进度、跨账号隔离、文件夹同步或耐久；详见“PC 短文本真实回复补充”和“S07 iOS 同会话真实服务补测”。 |
| S05-PC-001 | 规格 2.2–2.4；H06 | 部分通过（本机实际 `morde-pc` + 合成模型/执行器；失败提示的实际路由注入及超限大文本未验） | PC 发出普通消息并显示合成回复；Task 失败与等待用户授权由本地夹具核验。运行中状态追问和重启后状态恢复已操作，但可访问性读取未确认状态回复全文。`routingFailureText` 展示、原文保留及 `message_too_long` 不允许同内容重试由 Assistant 组件测试覆盖。见“本机 PC S05 客户端验收”。 |

### 本地自动化及真实 Provider 探针（2026-11-22；不计作完整端到端验收）

| 范围 | 执行 | 结果与边界 |
|---|---|---|
| DSH 长会话 | `runtime/morde-cloud-runtime` 下执行 `pnpm test` | 155 通过、0 失败；先构建锁定 DSH compaction 包，再覆盖 40 轮/多次 Session 压缩、早期与最新锚点、Token/源事件覆盖进展、无进展摘要不提交及 18 KiB 字节级当前输入合成用例。此用例不证明真实模型收到超过 8,192 tokens；真实模型累计历史窗口仍未测。 |
| Provider 限流恢复 | `go test ./internal/app ./internal/worker -count=1`；隔离 PostgreSQL 16 下执行 `go test -v ./internal/store/postgres -run '^TestConversationRouteRetriesConfirmedRateLimitWithNewMeteredIdentity$' -count=1` | 单测和 PostgreSQL 端到端通过；覆盖 429 拒绝、账号/项目预留释放、新调用 ID、Retry-After 延迟、最多三次及终止失败。`Retry-After` 秒数/HTTP 日期解析、缺失/非法/超长边界由模型响应单测覆盖。服务端 `go test ./...` 在账本修复后通过。完整 PostgreSQL 包测试触及 10 分钟仓库超时，在 `TestPostgresTaskFileFetchTransferResumeContentLifecycleAndCancel` 的迁移初始化期间退出；定向限流集成通过。无真实 Provider 验收。 |
| DeepSeek Provider 长输入直连 | 本地临时测试仅向官方 Responses API 提交合成对话文本，没有发送项目文件 | 51,842 字节、800 条记录首轮由 Provider 报告 18,502 输入 tokens，准确返回总数和首/中/尾锚点；完整前轮 user/assistant 历史的普通文本追问报告 18,575 输入 tokens，准确返回记录 0400。两次 JSON 模式全历史追问返回 `completed` 但正文空白，不能记作答对。直连证明 Provider 能处理超 8,192 tokens 的输入；不证明 Morde 网关、路由/Task 检索、原文持久化、PC/iOS 或累计历史压缩。 |
| Morde 网关 DeepSeek 长输入/路由实测 | `cd server && go test ./internal/app -run '^TestDeepSeekFlashDialogueThroughMordeModelGateway$' -count=1 -v`；本机显式设置 `MORDE_TEST_MODEL_PROXY` 并使用授权测试凭据 | 全套 live suite 通过：中文问答、格式输出、4 种工作量路由、短多轮、长输入、截断历史追问路由、完整历史续问、流式回复、工具调用与结果续接。长输入 51,842 字节由 Provider 报告 18,502 tokens，首/中/尾锚点和总数准确；不完整历史追问返回 `new` Task 决策；完整历史续问由 Provider 报告 18,575 tokens 并准确回答 0400。初次不配代理时失败原因为系统 DNS 将 `api.deepseek.com` 映射到保留测试地址 `198.18.0.62`；已为显式代理下固定白名单 DeepSeek 域名添加与 OpenAI 一致的代理 DNS 路径，并验证主机白名单和 TLS 校验保持开启。此测试只覆盖 Morde Model Gateway 与路由分类器，不是服务端持久化/DSH 工具执行/PC/iOS 的完整端到端验收。 |
| 截断历史路由提示 | `go test ./internal/app -run '^TestH09LongHistoryFollowupIsMarkedForAuthorizedSearch$' -count=1` | 通过；验证中间事实不进入 4 KiB 有界预览，缺失历史细节必须转成授权历史搜索 Task；结果整理流程仍保持“不创建 Task”的约束。真实 Provider 路由决策见上行。 |
| 固定白名单 Provider 代理路径 | `go test ./internal/model -run '^TestModelProxyConnectsToValidatedIPWithoutForwardingCredentials$' -count=1` | 通过；配置代理时 OpenAI 与 DeepSeek 固定域名经代理 CONNECT；其他域名仍被拒绝，TLS 配置仍启用证书校验。 |
| 路由、消息恢复、上下文预算 | `go test ./internal/app ./internal/worker -count=1`；隔离 PostgreSQL 下运行 `TestConversationRouteRepairsOneInvalidOutputWithSeparateMeteredCall`、`TestConversationRouteContextChangesReclassifyUpToWorkerConcurrencyLimit` 和 `TestConversationResultAggregation` | 通过；覆盖结构非法/截断/工具响应被拒后最多一次独立计量的无工具修复、第二次非法输出失败关闭、租约/上下文冲突、取消恢复和结果聚合。只证明 Go/本地 PostgreSQL 行为，不证明实际模型回复或 PC/iOS 入口。 |
| 长历史后的短追问路由与检索 | `go test ./internal/app -run '^TestH09LongHistoryFollowupIsMarkedForAuthorizedSearch$' -count=1`；`CH_TEST_DATABASE_URL=<本机临时 PostgreSQL> go test ./internal/store/postgres -run '^TestH01LongPriorMessageUsesBoundedHistoryAndRequestPreviews$' -count=1`；`CH_TEST_DATABASE_URL=<本机临时 PostgreSQL> go test ./internal/worker -run '^TestConversationSummaryDSHMainAndSideChatsContinueAfterLongHistoryRestart$' -count=1` | 单条长历史用例先保存首轮直接回复，再受理短追问；确认前序已完成，快照包含该助手答复并将用户原文标为截断，路由请求要求调用授权历史搜索，且提示摘要、历史和任务文本均是不可信数据。Worker E2E 将 840 条旧消息组成连续 `PreviousClientMessageID` 链，并断言从末条回溯恰好找到 840 条；随后在真实 Node DSH Runtime 重启后继续路由、调用授权历史搜索并完成回复。两项 PostgreSQL 用例均连接本机临时 PostgreSQL 16 并通过；Worker E2E 用时 33.58 秒。均为本地/合成模型证据，不替代真实 Provider 或 PC 追问验收。 |
| 聊天请求体超限 | `cd server && go test ./internal/api -run '^TestOversizedConversationMessageIsRejectedBeforeAcceptance$' -count=1` | 通过；超过 API 请求体上限时返回 413，证明聊天消息受理/建单服务没有被调用。受理后的模型 Token 预算限制由路由预算和长消息集成用例覆盖。 |
| 路由/DSH 单条输入上限 | `go test ./internal/worker -run '^TestConversationRouteWorkerPreservesLongMessagesAndExplainsOversizedInput$' -count=1`；隔离 PostgreSQL 执行 `go test ./internal/store/postgres -run '^TestConversationRouteOversizedMessageIsKeptWithVisibleReason$' -count=1`；Node 运行时边界测试 | 通过；40 KiB 纯文本仍进入路由，超过 128 KiB 的消息在模型派发前以 `message_too_long` 结束；PostgreSQL 保留完整原文和可见失败原因；DSH 接受恰好 128 KiB 的投递并拒绝超限内容。此协议边界不是模型窗口 Token 数。 |
| PC 对话路由失败提示 | `cd web/portal && pnpm vitest run src/routes/Assistant.test.tsx`、`pnpm run typecheck` | Assistant 测试 210/210 通过、类型检查通过。`message_too_long` 显示原文已保留和缩短建议，且不提供相同内容重试；其他已受理路由失败也显示安全中文原因，同时保留既有重处理入口。实际 `morde-pc` 重启并打开同一测试旁聊后，失败原因已在消息气泡内可见；该客户端仍收到失败状态，未通过追问成功验收。 |
| DSH 当前消息长度 | `node --test --test-name-pattern='passes an 18 KB current message intact to the model within its context budget' test/hermes-compaction-runtime.test.mjs`；`node --test --test-name-pattern='validates message delivery Unicode characters' test/runtime.test.mjs` | 两项通过；证明约 18 KiB 当前输入完整进入合成模型请求，且 DSH 投递严格守住 128 KiB 边界。不代表真实模型 Token 统计或回复。 |
| Broker 授权、审批、预算、操作对账 | `go test ./internal/app -run 'TestCloudDSHToolBroker(SensitiveApprovalResumesExactCallOnce|ReconcilesApprovedActionAfterLostRuntimeReceipt|ChangedApprovedPayloadDoesNotDispatch|RejectedApprovalReturnsWithoutDispatch|ExpiredApprovalReturnsWithoutDispatch|PausesClarificationAtToolCallBudget|UsesAuthorizedHostOperationLedger|ReconcilesCheckpointedResultWithoutMoreToolBudget|QueriesUnknownOperationAtExhaustedBudget|PausesUnresolvedRecoveryDurably)|TestBuiltinSearchRequiresCurrentTaskAuthorization|TestCloudDSHWorkspaceWrite(RequiresApprovalAndCommitsOnlyTheApprovedCall|ExpiredApprovalDoesNotCommit|ApprovalCannotAuthorizeChangedContent)' -count=1`；另跑 `go test ./internal/app -run 'TestCloudDSHHubTaskAuthorizationRevocationFailsClosed|TestProjectBudget' -count=1`、`go test ./internal/worker -run '^TestCloudTaskAuthorizationRevocationFencesHostDispatch$' -count=1` 和 PostgreSQL `TestProjectBudgetPersistenceAndLedgerPagination` / `TestProjectRouteBudgetFailureDoesNotReserveEitherLedger` | 全部通过；验证的是服务端拒绝、撤权、额度持久化和未知结果恢复。 |
| 租约与子 Agent 恢复 | `go test ./internal/worker -run 'TestCloudDSHExecutor(RunsAndSettlesPersistedChildSession|RecoversChildSessionAfterRuntimeRestartWithoutReplayingTool|CancellationStopsChild|LeaseRenewalFailureStopsChild|ReconcilesEveryPinnedToolCallBeforeSettling)|TestCloudDSHChildSuccessRequiresReconciledHistory|TestCloudDSHDelegateToolsAreRestrictedToParentDefinitions|TestTaskWorkerRenewsLeaseWhileExecuting' -count=1` | 通过；含租约两次续期、子 Session 检查点与重启恢复。 |
| 双子 Agent 并行 | `node --test --test-name-pattern='runs two background children concurrently and the parent consumes both settled results' test/runtime.test.mjs` | 1 通过；测试证明两个子任务重叠运行、分别结算，主 Agent 再读取两份真实运行结果。未使用真实 Provider。 |
| 证据验证与原会话发布 | 隔离 PostgreSQL 下执行 `go test ./internal/store/postgres -run 'Test(PostgresTaskValidation|PostgresReviewModelUsage|ConversationResultReview|ConversationResultAggregation|TaskResultReviewMessages)' -count=1` | 通过；覆盖当前修订/证据绑定、重放、过时来源 supersede、一次发布和大结果分批保留。此处的 conversation `ResultReview` 是结果摘要，不是 `TaskValidationSummary` 验收结论。 |
| H05 回执验收状态门禁（2026-11-22，本机工作区） | App verdict、PostgreSQL revision/result/event fencing、单次原会话回执与 Worker review 定向回归；命令详见 [S04 审计记录](../Morde_Harness_并发实施/04-结果验证与交付.md) | App、PostgreSQL 定向集成和 Worker review 测试通过。回执标注主 Agent 摘要、Task completed 与各来源当前修订的服务端 validation；review verdict 只有与当前 Task、revision、result event 和 SHA 匹配时才展示，否则为 `pending`。并发回归确认回执事务持有 validation 输入共享锁直到提交。Task 投影仍将状态字段分别提供。Artifact review metadata 现绑定 Workspace revision 与内容 SHA-256；未给 review model 传产物字节，所以这只绑定来源，不表示已检查产物内容。可信执行环境和工程/非工程完整场景、实际客户端、真实 Provider 未验收，缺证据保持 `pending`。 |
| S01–S06 服务端集成回归 | 在 `codex/harness-h06-integration` 集成工作树执行 `go test ./...`；本机隔离 PostgreSQL 定向组；`TestConversationSummaryDSHMainAndSideChatsContinueAfterLongHistoryRestart` | 全服务端 Go 包通过，worker 全量 53.765 秒通过；本机隔离 PostgreSQL 定向回归 10/10 通过；H09 长历史 DSH 重启 E2E 通过（39.506 秒）。全包 `go test ./...` 未设置 `CH_TEST_DATABASE_URL`，所以全包中的其他 PostgreSQL 钩子跳过。Runtime DSH 与 Portal 验证另见 TODO 中同一集成基线记录。 |
| Portal Assistant 行为与类型 | `web/portal` 执行 `pnpm vitest run src/routes/Assistant.test.tsx`、`pnpm run typecheck` | 2026-11-22 当前工作树：Assistant 测试 209/209 通过；类型检查通过。覆盖任务状态/进度、追问投递、停止与恢复、结果整理、事件重连和长消息状态展示；仍不替代真实模型与 `morde-pc` / iOS 验收。 |
| iOS 本地消息受理 UI | `xcodebuild test -quiet -project clients/nanoMuse/android/src/ios/Minis.xcodeproj -scheme morde -destination 'platform=iOS Simulator,id=E4D916C4-5693-4DF4-9403-ED939CF5A394' -only-testing:MinisUITests/MordeChatModelGateUITests/testUnknownCloudModelStatusDoesNotBlockMessageAcceptance -parallel-testing-enabled NO -derivedDataPath /tmp/morde-h06-ios-20261122 -resultBundlePath /tmp/morde-h06-ios-20261122/ios-send-final2.xcresult` | iPhone 18 Pro / iOS 27.2 专用本地模拟器：1 通过、0 失败。仅验证云端模型状态未知时客户端仍受理消息；使用本地 fixture，不证明 Provider 回复、长对话、跨设备接续或实际 iOS 生产配置。结果包保存在本机 `/tmp/morde-h06-ios-20261122/ios-send-final2.xcresult`。 |
| S06 iOS Task 时间线重连回归 | `Morde` scheme / `MordeUITests`，iPhone 18 Pro / iOS 27.2；结果包 `/tmp/MordeH06S06-regression-rerun2-20261122.xcresult` | 7/7 通过。覆盖顺序发送、失败路由原位重试、断网/重启受理状态恢复、运行任务期间追问与独立消息、服务端 204 后以 `after=8` 重新订阅并取得序号 9。首次回归中的 204 重连用例单独失败，根因为动态任务详情没有启动 `watchTimeline`；复用现有订阅后该用例及全套回归通过。仅为 loopback 合成服务 UI 验收，不证明真实 Provider、服务端 Task 或 PC 创建会话的跨端续接。 |
| S06 iOS 审批披露单测 | `xcodebuild test -quiet -project clients/nanoMuse/android/src/ios/Minis.xcodeproj -scheme morde -destination 'platform=iOS Simulator,id=F163DF6D-C418-435E-A0DA-DF2D213C59C8' -only-testing:MinisTests/MIFApprovalViewsTests -parallel-testing-enabled NO`；iPhone 18 Pro / iOS 27.2；主工作区 HEAD `81e7304d`；结果包 `/tmp/MordeH06S06-approval-disclosure-corrected-20261122.xcresult` | 19/19 通过。覆盖已知金额、未知/缺省披露、“不适用”状态及审批状态边界；只验证本地呈现/合同 fixture，不证明服务端实际费用或审批请求。构建期间出现既有 Swift/Objective-C 警告，无失败。 |
| S06 iOS Task UI 主工作区复验 | `xcodebuild test -quiet -project clients/nanoMuse/android/src/ios/Morde.xcodeproj -scheme Morde -destination 'platform=iOS Simulator,id=F163DF6D-C418-435E-A0DA-DF2D213C59C8' -only-testing:MordeUITests/IOS03SimulatorAcceptanceUITests/testMordeChatComposerSendsSequentialPlainTextMessages -only-testing:MordeUITests/IOS03SimulatorAcceptanceUITests/testMordeFailedRouteShowsLeftRetryButtonAndRetriesInPlace -only-testing:MordeUITests/IOS03SimulatorAcceptanceUITests/testMordeChatComposerRecoversPlainTextAfterOfflineRestart -only-testing:MordeUITests/IOS03SimulatorAcceptanceUITests/testMordeChatComposerReconcilesAcceptedMessageAfterAppRestart -only-testing:MordeUITests/IOS03SimulatorAcceptanceUITests/testMordeChatComposerQueuesFollowUpOnActiveTask -only-testing:MordeUITests/IOS03SimulatorAcceptanceUITests/testMordeChatShowsOneStartedAcknowledgementForMessagesInSameTask -only-testing:MordeUITests/IOS03SimulatorAcceptanceUITests/testMordeTaskDetailReconnectsAfter204WhenSnapshotIsStillActive -parallel-testing-enabled NO -derivedDataPath /tmp/MordeH06S06-main-derived-correct-20261122 -resultBundlePath /tmp/MordeH06S06-main-ui-correct-20261122.xcresult`；主工作区 HEAD `81e7304d` | iPhone 18 Pro / iOS 27.2；7/7 通过，0 失败。覆盖顺序发送、失败原位重试、断网与重启恢复、活跃 Task 追问/独立消息，以及 204 后时间线重连。仅为 loopback 合成服务 UI 验收；真实 Provider、服务端执行及 PC 创建会话的跨端接续未验证。存在 `MordeStandaloneApp.swift` 的 UIKit hierarchy runtime warning，未影响测试结果。 |
| S06 iOS 账号/授权边界补充 UI | `Morde.xcodeproj` / `MordeUITests/IOS03SimulatorAcceptanceUITests`，iPhone 18 Pro / iOS 27.2；初始结果 `/tmp/MordeS00H06-iOS03-selected-20261122.xcresult`；最终结果 `/tmp/MordeS06-IOS03-final3-20261122.xcresult` | 初始 3 项用例 0/3、5 个断言失败。修正审批详情动作 selector、撤权分页 fixture，并确认账号切换前第二页请求已发出后，最终选定 4 项（事件链账号隔离、撤权分页错误/刷新恢复、丢弃迟到分页、切换账号清空草稿）4/4 通过，249.666 秒；撤权恢复用例另单独 1/1 通过（`/tmp/MordeS06-IOS03-permission-simplified-20261122.xcresult`）。仅为本地 loopback fixture，不替代真实双账号隔离、服务端授权、Provider 或 PC→iOS 接续验收。 |
| 大结果测试夹具 | 先以未声明窗口的 4K 保守默认值运行 `TestConversationResultAggregationPreservesLargeResultsAcrossReviews`，用例被正确拒绝；随后让该测试模型明确声明 65,536 token 已知窗口并重跑同一用例 | 修正后的用例通过；未改变生产窗口策略，也没有把超预算结果改成可通过。 |
| `morde-pc` 桌面客户端 | `clients/desktop/harness/desktop` 下执行 `npm test` | 288 通过、0 失败；覆盖桌面主进程、消息/门户桥接、授权及同步等自动化路径，不替代真实服务端与模型验收。 |

### 实际 PC 探测记录（2026-11-22；不替代完整 H06）

- **环境：** 使用仓库中的实际 `morde-pc` 窗口，服务端地址属于已授权测试环境；界面模型为 DeepSeek Flash。工作树修改未部署到此服务。
- **输入：** 新建旁聊，提交纯合成长文本，长度 132,305 UTF-8 字节，含首/中/尾锚点并明确禁止调用工具；随后提交一条询问三个锚点的短追问。未附加或发送任何文件。原始用户消息显示在会话中，输入框清空。
- **结果：未通过真实长上下文验收。** 首条输入后超过 60 秒，首条及追问都只有用户消息，没有助手回复或可见状态；没有重发。展开长消息后，可访问性树仍未暴露中、尾锚点，因此只记录界面显示并受理，不宣称完整原文已被模型处理。当前工作树的路由/DSH 单条输入协议预算为 128 KiB；132,305 字节比该值大 1,233 字节。按当前本地代码，这应保留原文并以 `message_too_long` 明确失败，不能作为超 8K tokens 的成功用例；远端测试服务未运行当前工作树，故未显示失败状态仍待查。
- **短文本探针：** 在同一实际 `morde-pc` 中新建独立空旁聊，选择 DeepSeek Flash，发送纯文本“短对话连通性探针……只回复 SHORT-OK”，客户端收到 `SHORT-OK`。未附加文件；没有可靠 Provider 用量或请求 ID。这只证明该测试服务的短文本真实回复可用，不满足长上下文、云端执行关闭或 PC/iOS 接续验收。
- **长文本自动化尝试：** 前次局部合成草稿已在实际 `morde-pc` 输入框清空并确认发送按钮禁用。本轮生成 800 条合成记录（42,603 UTF-8 字节，首/中/尾锚点）；首次 `SET_VALUE` 和随后读取没有返回，故当时没有发送。后续客户端可访问性读数仍只展示约 5.3K 字符的前段，不能单靠该读数确认完整性；继续观察实际会话后，用户消息尾部显示第 800 条及 `OMEGA-Q9W3`，助手回复“首锚点：ALPHA-7KQ2；中锚点：MID-4P8N；尾锚点：OMEGA-Q9W3；记录数：800。”因此该 42,603 字节合成输入已在真实 PC/DeepSeek Flash 链路中得到正确处理。未附加或发送文件。没有取得 Provider 用量、Tokenizer 结果或请求 ID，故只算字节级长输入证据，不证明超过 8,192 tokens；也不证明本地未部署工作树的代码行为。
- **长文本后的同会话追问：** 在上述同一旁聊中追问“记录 0400 的数值和校验值，并复述首、尾锚点”，本地夹具核对的预期为 `842`、`167600`、`ALPHA-7KQ2`、`OMEGA-Q9W3`。约 50 秒后仍无助手回复，消息出现“重新处理这条消息”；通过该按钮重试一次后，约 30 秒仍无回复，按钮再次出现。再次检查时仍无回复，界面只暴露重处理按钮。未再次重试或创建重复消息。记录为真实 PC 同会话追问失败；由于该测试服务未运行当前工作树，不能归因于本地分支。
- **服务探测：** 对同一测试服务执行只读 `GET /api/capabilities` 成功，返回 `api_version=1`、`conversation_auto_routing_v1`、`cloud_tasks`，`default_max_request_bytes=1048576`。该结果仅证明公开能力接口可用，不能证明消息路由、模型网关或 Provider 正常。
- **本次修复与验收边界：** 本地 `web/portal/src/routes/Assistant.tsx` 曾隐藏除 `message_too_long` 外的所有 `routing_status=failed` 原因，导致失败消息只露出重处理按钮；现显示 `routingFailureText` 提供的安全中文说明，长度超限仍保留原文、给出缩短建议并隐藏同内容重试。也覆盖重复请求继承原消息失败状态的路径。Assistant 全量测试 210/210、类型检查、`build:pc` 及 `git diff --check` 通过。首次强制刷新仍只读到重处理按钮；之后用 `--force-renderer-accessibility` 重启实际 `morde-pc` 并重新打开同一合成旁聊，失败原因“消息上下文暂时无法读取”已在气泡内显示，PC 失败原因展示通过。长追问仍无回答；测试服务未运行本地服务端工作树，未获得服务端诊断或模型用量。
- **源码诊断与后续回归：** 截图文案对应前端 `routing_failure_reason=routing_context_unavailable`。它是宽泛路由失败类别：Worker 会在读取会话快照失败、消息聚合仍未解决、重读快照失败、修复状态查询失败或上下文冲突后使用该状态；当前服务端日志只记录消息 ID、失败类别、错误码和分类，没有记录具体阶段/原因。因此截图能排除“明确报成模型超限”，但没有对应消息 ID 和服务端日志，仍不能锁定这次远端失败的底层分支。另确认历史工具把历史消息错误地按助手回复的 32 KiB 限制校验；42,603 字节长消息虽低于会话输入的 128 KiB 协议预算，仍会在原文检索时被拒绝。历史工具原先也固定返回消息开头 1,200 个字符，可能再丢掉中段命中。现将历史校验改为会话输入预算，并让当前/个人历史检索返回围绕命中词的有界片段；`TestCloudDSHCurrentConversationHistorySearchReturnsMiddleOfLongMessage` 用单条 800 记录、超过 32 KiB 的合成消息验证可返回第 0400 条。`go test ./internal/app -count=1` 通过。此为本地机制修复证据，不等于已复现或修复截图对应的远端路由失败。
- **限制：** 未取得本次长消息和失败追问的 Provider 用量、Token 统计、Tokenizer 结果、消息/Task ID 或请求 ID；没有目标模型上下文窗口及可靠 Tokenizer，无法判断是否达到 8,192 tokens。远端测试服务未运行当前工作树，PC 结果不能归因于本地实现。未执行 iOS、累计历史超窗口、云端任务与子 Agent、重连恢复或 60 分钟耐久验收。

### 本机 PC 长历史分页夹具（2026-11-22；合成数据，不计作模型追问验收）

- **环境：** 当前工作树构建的 `morde-pc`，独立 `/tmp/morde-pc-harness.*` user-data profile；真实 HTTP/API handlers 连接本机隔离 PostgreSQL，服务只监听 loopback。未访问远程测试服务，未附加或发送文件。
- **输入：** 临时夹具在主会话预置 840 条合成用户消息，每条约 10 KiB，总量超过 8 MiB；首条含 `ORCHID-PC-HISTORY-SENTINEL-8472`。PC 登录后收到本地会话历史，使用“加载更早消息”翻页到 `row=0000` 并读到首条哨兵；此时更早消息按钮消失。该 UI 观察证明能到达历史起点，不证明所有消息同时可见或模型读到了这些上下文。
- **执行结果：** 含客户端的首轮夹具因未在 10 分钟内触发临时 `STOP_FILE` 而以 `browser fixture timed out` 退出；这是验收驱动器收尾超时，不是通过。随后本机重跑在读到新临时停止路径后立即触发，夹具数据库断言确认 840 条记录并在 13.779 秒通过。该重跑没有 PC 客户端连接，因此只确认种子和数据库断言。
- **限制：** PC UI 提示“工具连接暂不可用”；此夹具不挂真实 DSH/Provider，不执行历史搜索 Task。客户端没有在本用例发送追问，不能算长上下文推理、真实回复或 MH-CTX-001 通过。无截图（本机截图接口返回捕获失败）；可访问性树在大量消息控件下达到 1,200 个元素上限。长对话的真实 DSH 证据见上面的 Worker E2E；PC/iOS 追问接续和 60 分钟耐久仍待验收。测试服务和隔离客户端已停止。

### 本机 PC 运行中普通追问夹具（2026-11-22；合成模型/执行器，不计作真实模型验收）

- **环境与步骤：** 当前工作树的 `morde-pc` 使用单独 `/tmp/morde-pc-harness.*` profile，连接只监听 loopback 的临时 HTTP/API 服务与本机隔离 PostgreSQL 16。通过实际 PC 输入框发送 `Morde browser acceptance: long background task`；数据库确认对应 Task 仍为 `running` 时，再从同一会话发送 `Morde browser acceptance: quick reply`。
- **结果：** 页面显示普通回复“本地验收：快速请求直接回复。”；数据库记录两条用户消息和一条助手回复，长任务当时仍为运行态。`TestConversationBrowserAcceptance` 以 `go test -count=1 -timeout=6m -v ./internal/store/postgres -run '^TestConversationBrowserAcceptance$'` 通过（测试 337.82 秒，包 339.579 秒）。结束夹具时关闭隔离 PC 客户端以释放长连接 SSE；这只是本地测试服务收尾步骤。
- **测试夹具修正：** 首轮复现中合成 `ModelResponse` 漏设 `StopReason="stop"`。路由网关会把未明确正常结束的响应按协议标为无效，因此修正 `browserAcceptanceModel` 的成功响应，测试随后通过；这是测试夹具缺字段，不是产品路由解析器缺陷。
- **限制：** 模型和任务执行器均为本地合成替身；长任务只模拟运行状态，没有证明真实 DSH 工具调用、任务完成、真实 Provider 回复、Task 定向追问/停止、iOS 接续或 60 分钟耐久。该结果只证明本机 PC 消息入口在后台 Task 运行期间仍能完成一条普通快速对话。

### 本机 PC S05 客户端验收（2026-11-22；合成模型/执行器，不计作真实模型验收）

- **实现：** S05 分支 `codex/harness-s05` 基于共同本地基线 `e009815383daaa883ec7f2b40fe2ec7cdeb7397a`，只改 PC 内嵌 Portal 的 Assistant 路由、直接样式和测试。此前失败消息只露出无说明的 `!` 重处理按钮；现按 `routingFailureText` 显示安全中文原因及带标签的重处理按钮。重复消息不继承原消息的失败原因；超长消息保留原文、显示缩短建议并禁止同内容重试。未加依赖、改 DTO 或改独立 Web 用户端。
- **本机验证：** `pnpm exec vitest run src/routes/Assistant.test.tsx` 210/210 通过；`pnpm run typecheck`、`pnpm run build:pc`、桌面端 `npm run build` 通过。使用当前 S05 工作树启动真实 `morde-pc`（内嵌 Portal，而非独立浏览器），单独 `/tmp` profile 指向 loopback-only 测试服务。PC 实际发送普通快速消息，Accessibility 查询到合成回复“本地验收：快速请求直接回复。”；发送“已接单后任务失败”和“等待用户授权”两项后，夹具 API 分别确认 `agent_acknowledged=true/task_status=failed` 与 `agent_acknowledged=true/task_status=waiting_user/pending_request_status=pending`。隔离 PostgreSQL 的 `TestConversationBrowserAcceptance` 使用本机随机端口和随机 DSN，显式选取 `failed,approval`，写入停止标记后通过：测试 126.46 秒，包 127.303 秒；容器随测试结束清理。首次尝试未触发停止标记而超时，未计通过；随后完整重跑通过。
- **状态/恢复操作：** 另一次同一隔离 profile 的实际 PC 操作提交了运行中 Task、普通状态追问和普通快速问题；关闭并重新启动 PC 后，登录态恢复，客户端重新请求同一会话消息和 Task 状态。该探索使用的长运行 fixture 因遗漏停止标记而超时，因此只记录为 UI/API 操作证据，不替代成功的 PostgreSQL 验收，也未从无障碍树确认状态回复全文。屏幕捕获因 macOS Screen Recording 权限不可用；未生成截图。
- **未覆盖与边界：** 实际 PC 中未注入单独的 `routing_status=failed` 消息样本；错误文案、重处理入口、重复失败和超长禁重试由 210 项组件测试验证。尝试用 Accessibility 向真实客户端输入 132,305 字节合成文本时操作超过 60 秒无响应，已取消，未确认发送，不计通过。该输入边界另由本地 Worker/PostgreSQL/Runtime 既有回归覆盖。未改同步入口，故未重跑文件夹同步。审批费用合同审计显示既有卡片支持本次/累计费用的已知或未知状态，但服务端当前写入未知，且合同没有通用金额限额字段；PC 本轮没有伪造限额或添加授权逻辑。未访问共享站点、未使用用户提供的远程凭据/API key、未发送文件。H06 的真实 Provider、PC/iOS 接续、累计历史与耐久验收仍未完成。

### 本机运行中消息投递回归（2026-11-22；分层自动化测试）

- **环境：** 使用 loopback-only 的临时 PostgreSQL 16 容器和本地 Node DSH Runtime；容器及隔离数据库已在测试后删除。模型使用合成响应，没有发送项目文件或访问远程测试服务。
- **PostgreSQL 路由：** `go test ./internal/store/postgres -run '^TestConversationRouteSteersOnlySafeRunningTaskSupplement$' -count=1` 通过（3.941 秒）。正在运行的 Task 收到安全补充后，消息只关联该 Task 并以 `steer/pending` 等待投递；改变任务范围的输入进入澄清，不被误当作补充。
- **Go DSH executor：** `go test ./internal/worker -run '^(TestCloudDSHExecutorDeliversMessageDuringModelCall|TestCloudDSHExecutorDeliversPendingConversationMessageOnce)$' -count=1` 通过（4.531 秒）。模型调用期间到达的两条消息按顺序进入后续模型轮次，重复投递仍按消息 ID 去重。
- **Node DSH Runtime：** `node --test --test-name-pattern='account runtime forwards fenced running message delivery to the task agent|replays an enqueued steer after its checkpoint acknowledgement is lost|does not claim a steer twice when restart follows its claim checkpoint' runtime/morde-cloud-runtime/test/runtime.test.mjs` 通过（3/3，2.105 秒），覆盖任务栅栏、入队检查点回执丢失重放及领取后重启不重复领取。
- **长历史及推理中补充：** `go test ./internal/worker -run '^TestConversationSummaryDSHMainAndSideChatsContinueAfterLongHistoryRestart$' -count=1 -timeout=8m` 通过（29.80 秒）；使用 840 条连续 predecessor 历史，在本地 Node DSH Runtime 重启后完成授权原文搜索。另在模型调用挂起时向运行中的 Task 投递安全补充，确认旧模型输出及工具调用在执行前被丢弃，DSH 将新消息与授权历史检索纳入后续推理并完成任务；后续并发路由断言也通过。该结果是本地合成模型/隔离 PostgreSQL 证据。
- **运行时回归：** 在 `runtime/morde-cloud-runtime` 目录执行 `node --test --test-concurrency=1 test/runtime.test.mjs test/compaction-recovery.test.mjs test/hermes-compaction-runtime.test.mjs`，144/144 通过（55.943 秒）；覆盖运行时、压缩恢复及 Hermes 压缩行为。另执行 `node --check src/engine.mjs` 与 `git diff --check`，均通过。
- **限制：** 以上是数据库路由、Go executor 和 Node Runtime 的分层覆盖，不是一次从真实 PC/iOS 经服务端、Provider 到实际 Task 完成的端到端验收；未覆盖真实模型下定向追问、独立 Task 并行、用户停止、跨端接续或 60 分钟耐久。因此 `MH-MSG-001` 与 `MH-DUR-001` 的整体验收状态不变。

### S02 合并后回归（2026-11-22；本机当前工作树）

| 覆盖 | 命令 | 结果 |
|---|---|---|
| Runtime 投递、压缩、恢复与 Hermes 压缩 | `cd runtime/morde-cloud-runtime && node --test --test-concurrency=1 --test-reporter=dot test/runtime.test.mjs test/compaction.test.mjs test/compaction-recovery.test.mjs test/hermes-compaction-window.test.mjs test/hermes-compaction-runtime.test.mjs` | 178 项通过；合并后覆盖 steer 流中作废旧答案/工具调用，以及检查点后、IPC 派发前到达消息的版本栅栏。 |
| App checkpoint/inbox | `cd server && go test ./internal/app -run 'TestAgentCheckpoint|TestCheckpointInbox' -count=1` | 通过。 |
| PostgreSQL DSH 流式消息 | `cd server && go test ./internal/store/postgres -run '^TestCloudDSHStream' -count=1` | 全套通过；pending same-Task steer 后旧 delta 被拒，已展示快照保留为 interrupted。 |
| Worker DSH | `cd server && go test ./internal/worker -run '^TestCloudDSH' -count=1` | 全套通过（67.075 秒）；新协议回归确认 pending steer 先送达 Runtime，旧模型工具调用不执行。 |
| 840 条长历史重启 E2E | `cd server && go test ./internal/worker -run '^TestConversationSummaryDSHMainAndSideChatsContinueAfterLongHistoryRestart$' -count=1 -timeout=8m` | 通过（45.623 秒）；真实本地 Node DSH Runtime 重启后完成授权历史检索。 |

Worker 全套中的 `TestCloudDSHBackgroundOutputContinuationAfterLeaseReclaim` 曾在两次整组运行时于 claim 检查失败；单测复跑和随后整组运行通过。未确认其间歇失败根因，保留为时序风险。测试使用合成模型和本机隔离 PostgreSQL；未调用真实 Provider、测试站点或客户端入口。`node --check src/engine.mjs`、相关 Go 文件 `gofmt -d` 和 `git diff --check` 均无输出。

### S00 主工作区整合复验（2026-11-22；本机隔离环境）

测试产品代码基线为 `fd18564e`；之后仅把 S02 的回执断言修正为检查 S04 包裹格式，测试提交为 `af9c960b`。S01、S02、S03、S04、S06 的实现已在本地 `main`，没有推送或部署。

| 覆盖 | 命令 | 结果 |
|---|---|---|
| App 全量 | `cd server && go test ./internal/app -count=1` | PASS，22.636 秒。 |
| PC Assistant 状态 UI | `npm --prefix web/portal test -- src/routes/Assistant.test.tsx`；`npm --prefix web/portal run typecheck` | 210/210 与 typecheck 通过，Node `26.13.0`。本轮未运行生产构建；Portal `engines` 声明 `24.x`，此项仅作补充结果。当前 `web/portal` Assistant 源文件与 S05 实际 PC 验收工作树无差异；前述 PC failure-label 实际操作及 build 证据见“S05 PC 客户端验收”。 |
| S01 路由/摘要 | `cd server && CH_TEST_DATABASE_URL=postgres://postgres:local_test_only@127.0.0.1:55432/postgres?sslmode=disable go test ./internal/store/postgres -run '^(Test(H01SummaryConcurrentMessageAndStaleCommit|H01SummaryCommitRechecksProjectMembership|H01SummaryScopeVersionChangeInvalidatesOldSummaryAndReopensSources|CoordinatorRouteRetriesConfirmedRateLimitWithNewMeteredIdentity|ConversationRouteRetriesConfirmedRateLimitWithNewMeteredIdentity|ConversationSummaryPersistenceLeasesReplayAndInvalidation|ConversationSummaryInputPagesKeepLongSourcesAndExcludeSuppressedDescendants|ProjectConversationSummaryInputSharesOnlyProvenMemberHistory|TaskConversationSummaryFailsClosedWhenSourcesOrMemoryChange))$' -count=1 -timeout=8m` | 9 项通过，17.649 秒。 |
| S04 validation/回执 | `cd server && CH_TEST_DATABASE_URL=postgres://postgres:local_test_only@127.0.0.1:55432/postgres?sslmode=disable go test ./internal/store/postgres -run '^Test(PostgresTaskValidation|PostgresModelSettlementRejectsReview|PostgresReviewModel|ConversationResult|ConversationTaskResultReceipt|TaskResultReviewMessages|ConversationResultReceiptLocksValidationInputsUntilCommit)' -count=1 -timeout=8m` | 定向组通过，39.187 秒；包括回执字段、单次发布和并发事务持有 validation 输入锁。 |
| S04 review Worker | `cd server && CH_TEST_DATABASE_URL=postgres://postgres:local_test_only@127.0.0.1:55432/postgres?sslmode=disable go test ./internal/worker -run '^(TestTaskValidationReviewWorker|TestTaskValidationReviewInput|TestValidReviewContentEvidence)' -count=1 -timeout=8m` | 通过，2.110 秒。 |
| S02/S03 Worker 跨层 E2E | `cd server && CH_TEST_DATABASE_URL=postgres://postgres:local_test_only@127.0.0.1:55432/postgres?sslmode=disable go test ./internal/worker -run '^(TestCloudDSHCompletionRaceRecoversPendingSteerOnSameSession|TestConversationSummaryDSHMainAndSideChatsContinueAfterLongHistoryRestart|TestAccountRuntimePostgresAndRealNodeContinueAfterFirstTaskContextEnds)$' -count=1 -v -timeout=8m` | 3 项通过，34.084 秒。真实本地 Node Account Runtime/DSH Runtime + 隔离 PostgreSQL；覆盖 Account Runtime 两个 Task 上下文连续、840 条历史在 Runtime 重启后检索，以及模型推理中 steer 命中完成 fence 后同 Session 重推理、side effect 一次、旧 completion/回执零发布、修订后单一回执与 source 发布。 |

本地 PostgreSQL 16 使用临时容器、仅绑定 `127.0.0.1:55432`、无持久卷，测试结束后已停止清理。主分支首次 E2E 曾因断言仍要求 assistant 正文等于原始摘要而失败；S04 已给回执添加主助手说明、Task 完成和 validation 状态，故仅把 S02 测试改为检查正文子串及有界状态标签，未改产品代码。一次与 PostgreSQL 大组并行的重跑在完成 fence 等待超时；隔离后串行重跑通过。此前 H09 的 `model_unavailable` 也定位为隐藏 result-review route 未被测试 resolver 配置模型的 fixture 缺陷，未放宽生产路由资格。

这些均为合成模型/本机数据库证据；未访问共享测试站、未配置或调用真实 Provider、未运行当前代码的 PC→iOS 实际入口或 60 分钟耐久验收。H01/H05/H06/H09/H10 及 P0 仍按 TODO 保持未完成。

### S00 主工作区整包失败复核与定向补测（2026-11-22；本机隔离环境）

- **基线与范围：** 本地 `main` 已整合 S01–S04 的测试/实现修复及 S02/S03 child checkpoint、admission 和 SSE 测试适配，当前 HEAD `acbdfb67`。本轮 S00 另修复路由分类请求在最小 4K 模型窗口下的提示预算和 history slice 别名问题；只在本机工作区修改，未部署、未访问共享站点或外发文件。
- **整包初次结果：** `CH_TEST_DATABASE_URL=<本机隔离 PostgreSQL> make it` 失败退出，耗时 962.341 秒；日志保存在本机 `/tmp/morde-s00-make-it-20261122.log`。初始失败为 PostgreSQL 包 15 项、Worker 包 5 项。S03 修复可空 Provider model SQL NULL、Task activity/stall notice 持久化及相关测试 fixture/断言；S04 修正 validation receipt 和 project-scoped fixture；S02 修复 child replay 使用持久 root checkpoint baseline，并修正重启测试夹具；S03 修正生命周期测试 store admission 透传和 SSE keepalive reader。没有因失败放宽数据库约束、权限、回执门禁或审批断言。

| 覆盖 | 命令/用例 | 结果 |
|---|---|---|
| App 全量 | `cd server && go test ./internal/app -count=1` | PASS，20.739 秒。 |
| 路由最小窗口 | `cd server && go test ./internal/worker -run '^(TestFitConversationRouteContextFitsMinimumWindowWithIncompleteHistoryAndCandidates|TestFitConversationRouteContextDropsHistoryTextButKeepsReferences|TestFitConversationRouteContextDropsOversizedSummary)$' -count=1` | PASS，3 项。大候选/不完整历史在 4096 token 窗口 fixture 中被安全收缩，候选集合标记不完整，模型输入预算仍在界内。 |
| 长运行 Task 短追问最小窗口 | `cd server && go test ./internal/worker -run '^TestFitConversationRouteContextShrinksReferencedTaskForShortFollowup$' -count=1` | PASS，1 项（0.725 秒）。保留被引用的长运行 Task、截断其长目标并标记候选集合不完整，最终路由输入仍在 4096-token fixture 窗口内。 |
| 前序长请求摘要保留 | `cd server && go test ./internal/worker -run '^TestFitConversationRouteContextShrinksPreviousRequestWithoutLosingItsDigest$' -count=1` | PASS，1 项（1.883 秒）；保留原消息 ID 与完整正文 SHA-256，同时对路由预览做安全截断并保持模型请求在 4096-token fixture 窗口内。 |
| PostgreSQL 初始失败复验 | 隔离 PostgreSQL 下重跑初始失败中除修订重启 E2E 外的 14 项：回执、项目模型派发/预留、共享项目历史、streaming、task activity/stall 及 pending-input completion | PASS，14 项，15.263 秒。 |
| Worker / Node 恢复与长历史 | 隔离 PostgreSQL 下 `TestCloudTaskWithCancelledPersonalChildFailsClosedAfterNodeRestart`、`TestCloudDSHBackgroundOutputContinuationAfterLeaseReclaim`、`TestCloudTaskHostGenerationFencePreservesAgentChildCheckpoint`、`TestA30TaskFileFetchRunsFromChatThroughPostgresHubAndPCBridge`、`TestConversationSummaryDSHMainAndSideChatsContinueAfterLongHistoryRestart` | PASS，5 项，41.020 秒。 |
| 会话修订重启（修复前） | `TestConversationRevisionStopsRealAgentAndRestartsWithLatestMessages` | 修复前失败：新版 Task Runtime 退出但没有结果，报告 `child_exited_without_result`。后续诊断和修复结果见下方 S00 记录。 |
| 运行中任务生命周期 | `CH_TEST_DATABASE_URL='postgres://s07triage:s07-local-test-only@127.0.0.1:55435/s07test?sslmode=disable' go test ./internal/worker -run '^TestCloudTaskSurvivesClientDisconnectAndPublishesItsPrivateWorkspaceView$' -count=1 -v` | PASS，S02 在当前 HEAD `acbdfb67e38cdaecc20d87669c21b482cc84c76a` 的私有 PostgreSQL 上复验，用例 7.28 秒、Worker 包 8.083 秒。命令输出通过 `2>&1 | tail -n 12` 截尾，使用 `set -o pipefail` 保留退出码；容器已删除。先前 handshake/`deferred task agent changed` 失败未在本次复现，不作为共同根因证据。 |

- **S00 路由根因与机制：** 不完整 history 同时带有截断候选时，路由提示重复声明了 history-incomplete/no-inference 规则，和较大的候选列表一起可能使短消息在未知模型容量时无法装入保守的 4096-token 窗口。S00 压缩候选边界说明，并在历史检索任务提示中合并不完整历史规则；一般不完整历史仍保留 no-inference 提示。另在 context fitting 前复制 history slice，避免 fit 对共享底层数组的修剪污染用于 Apply 快照校验的原始 snapshot。定向单测证明代表性大候选输入可以在 4096 token 预算内完成；尚未以真实 Provider 复验。
- **修订重启 E2E 根因与修复：** 最初根因是 Go start frame 漏传 delivery mode，Runtime 将消息默认为 `followup`，导致同 ID 的 v4 `steer` 被拒绝。补齐 mode 后复验又暴露 personal mutation 已提交时，恢复准入检查先于收据对账，可能把已有副作用误判为未派发；Worker 现在先以持久收据验证并复用该结果，再处理后续 steer。另为无法确认的恢复工具结果增加固定 `TOOL_OUTCOME_UNKNOWN` checkpoint 标记；Store 严格验证该标记、保留结果并保持未对账，不允许它满足完成栅栏。
- **最终定向复验（2026-11-22）：** `cd runtime/morde-cloud-runtime && node --check src/engine.mjs && node --check src/ipc.mjs && node --check test/runtime.test.mjs` 通过；Node 定向投递/恢复回归 13/13 通过。`cd server && go test ./internal/worker -run '^TestCloudDSHCheckpointHistoryPersistsUnknownSupersededToolResultWithoutReconciling$' -count=1` 通过。使用本机隔离 PostgreSQL 临时数据库运行 `go test ./internal/store/postgres -run '^TestConversationRevisionStopsRealAgentAndRestartsWithLatestMessages$' -count=1 -v`，跨层 E2E 通过（2.79 秒；Worker 包 3.637 秒），确认两条修订顺序无丢失/重复，personal mutation revision 保持 1，ToolCalls 仍为 1。PostgreSQL 测试容器已停止删除，临时数据库由 fixture 自动删除；临时诊断输出已清理。`git diff --check` 通过。
- **Node 定向命令：** `node --test --test-concurrency=1 --test-name-pattern='reconciles an interrupted tool call before resuming|supersedes an interrupted tool recovery|v4 supersede for another recovering call|v4 supersede preserves a checkpointed tool result|v4 supersede releases a pending delegated child recovery|mismatched v4 target does not release a pending delegated child recovery|reconciles an interrupted MCP IPC call|revoked MCP grant blocks an interrupted call|replays an interrupted delegate using the reconciled child output|replayed background delegation returns a durable handle|resumes an interrupted child from its own checkpoint|Task start validates persisted message modes|start-seeded persisted message modes' test/runtime.test.mjs`，13 项通过。
- **未覆盖与结论：** 未重跑 962 秒整包；真实模型、PC→iOS 同会话、账号隔离、指定远程环境、完整文件夹同步与 60 分钟耐久验收仍未执行；H01 和 H06/P0 保持未完成。E2E 使用合成模型和本机隔离 PostgreSQL，只证明当前修订恢复跨层路径，不证明 Provider 或客户端验收。

### S00 用户指定 Harness 分支合并与验证（2026-11-22）

- **起点与范围：** 从本地 `main` / `origin/main` 相同提交 `ada69312946a235b9aa58577dcb754f7f7ae36ba` 建立隔离集成 worktree `codex/harness-merge-all`。按截图逐一合并以下 19 个分支：`codex/harness-h06-integration`、`codex/harness-s01`、`codex/harness-s01-coordinator-rate-limit`、`codex/harness-s01-history`、`codex/harness-s01-main`、`codex/harness-s02-go`、`codex/harness-s02-integration`、`codex/harness-s02-node`、`codex/harness-s02-receipt-fix`、`codex/harness-s02-review`、`codex/harness-s03-integration`、`codex/harness-s03-review`、`codex/harness-s04-integration`、`codex/harness-s04-main`、`codex/harness-s05`、`codex/harness-s05-review`、`codex/harness-s05-review-2`、`codex/harness-s06`、`codex/harness-s06-regression`。
- **冲突处理：** H06/S01/S02/S03/S04/S06 合并中出现的冲突，均通过对照两侧实现和提交历史解决，保留当前集成树中的较新安全约束与回归覆盖。冲突文件为 `docs/Morde_Harness_并发实施/04-结果验证与交付.md`、`docs/Morde_Harness_并发实施/06-iOS客户端.md`、`server/internal/app/conversation_route_h01_test.go`、`runtime/morde-cloud-runtime/src/engine.mjs`、`server/internal/store/postgres/conversation_result_aggregation_integration_test.go`、`server/internal/store/postgres/conversation_result_review.go`、`server/internal/store/postgres/task_agent_stall_integration_test.go`、`server/internal/worker/cloud_dsh_executor_test.go`、`server/internal/worker/cloud_dsh_session_tree.go`、`server/internal/worker/conversation_context_dsh_e2e_integration_test.go`。例如保留了收据发布事务对 review/task/event 的共享锁、已应用 steer 的 admission fence、最新的 child checkpoint/restart 覆盖和完整 validation/UI 回归记录；没有将旧分支中缺少这些更新的文件版本覆盖进来。
- **Git 验证：** 19 个分支 tip 对集成分支执行 `git merge-base --is-ancestor` 均成功；无冲突分支使用正常 merge，冲突按文件比较后解决，未使用 `git merge -s ours`。合并树相对起点只有 S01 交接文档一处内容差异（42 行新增、2 行删除）；随后增加管理文档，产品源码 diff 仍为空。正常 fast-forward 后执行 `git push origin main` 成功，合并提交为 `e631d4cdd9ec68e1143f93a46f5572a54702862f`；随后 `main` 与 `origin/main` 前进到 `a23d7e7572f349211992bdea69c541a31a29de5c`（`fix: 优化harness`）。当前本地和远端 `main` 均为 `a23d7e7572f349211992bdea69c541a31a29de5c`，19/19 指定分支 tip 仍是其祖先。
- **分支与 worktree 清理：** 截图中的 19 个分支 tip 均为 `main` 祖先，目标 worktree 均已清理，19 个本地分支引用已删除。`codex/harness-s05` 有三处未提交 Portal 改动，经逐文件比较与 `main` 完全相同后随其 worktree 一并清理。临时 `codex/harness-merge-all` 与 `codex/merge-to-main` 分支/worktree 也已移除；后者只剩未完成的冲突工作区，已确认其已提交 tip 在 main 中。`git worktree prune` 清掉已不存在路径的登记。未删除无关的 `workspace-1` 和 `codex/client-login-ordering-20261105`。
- **测试范围：** 本次分支整合没有改变产品源码，因此未机械重跑代码测试。复用上方同一产品源码快照已有的 App、路由预算、14 项 PostgreSQL、5 项 Worker/Node 及生命周期 E2E 定向结果；当时已知会话修订重启 E2E 尚未解决，后续修复与通过结果见“修订重启 E2E 根因与修复”。H06/H00 不因此标完成。对合并后的文档与 Git diff 执行 `git diff --check main` 通过，冲突标记扫描无匹配；新提交对象中的 API key/私钥常见格式扫描无匹配。

### S00 合并后剩余本地分支与 worktree 清理 2026-11-23

- **核验：** 当前 `main`/`origin/main` 为 `51cbe1375e02abfd780c9b9ee349d1fa85e79b99`。本地剩余 `codex/client-login-ordering-20261105`（`e5877690`）与 `workspace-1`（`fd8574dd`）均通过 `git merge-base --is-ancestor <branch> main`；`git log --oneline main..<branch>` 无输出。目标 `workspace-1` worktree `git status --short --branch` 仅显示干净分支状态。远端 `git ls-remote --heads origin` 只列 `refs/heads/main`。
- **清理：** 执行 `git worktree remove /Users/zhongxuan_li/.thinkrail/worktrees/morde/workspace-1`、`git branch -d workspace-1` 和 `git branch -d codex/client-login-ordering-20261105`。三项成功；清理后 `git worktree list` 只列 `/Users/zhongxuan_li/Desktop/morde`，`git branch -vv` 只列 `main`，远端仍只列 `main`。主工作区内 8 个本地未提交文件未触碰、未推送。
- **远端核对：** `git fetch origin main` 后本地 `main` 与远端提交均为 `a23d7e7572f349211992bdea69c541a31a29de5c`；`git ls-remote --heads origin` 只返回 `main`，因此截图分支没有待推送的远端引用，当前不需要再次 push。合并历史本身不代表真实 Provider、PC/iOS、云端电脑或耐久验收通过。

### S00 会话修订重启 E2E 退出诊断（2026-11-22；本机隔离 PostgreSQL）

- **退出状态诊断实现：** S02 在 `task_terminated.reason` 中补充有界 child `exit_code` 或信号信息，显式终止原因保持原值、Task 状态仍为 `unknown`。Node 定向测试 `node --test --test-concurrency=1 --test-name-pattern='reports unexpected child signals' test/runtime.test.mjs` 通过（1/1）；两个改动文件的 `node --check` 通过。
- **先前复现：** `TestConversationRevisionStopsRealAgentAndRestartsWithLatestMessages` 两次不带捕获运行均在修订 Task 阶段以 `child_exited_without_result; exit_code=1` 失败。此前两次 stderr 捕获尝试额外向 `bin-account.mjs` 环境注入诊断 flag，被其环境白名单拒绝，因此握手失败是诊断方法造成的伪象，不代表原始 E2E 握手不稳定。
- **根因诊断：** 使用不修改环境变量的本机 stderr 捕获重跑同一 E2E，输出 `content_same=true attachments_same=true mode=followup->steer version=4 supersede=true`，随后在 `runtime/morde-cloud-runtime/src/ipc.mjs` 的去重校验拒绝该帧。调用链确认：Go `cloud_dsh_session_tree.go` 构造 start 的 `initial_message`/`revision_messages` 时遗漏持久 delivery mode；Node `WorkerIPC` 把初始消息默认按 `followup` 记账，稍后服务端为同一 ID 发出的持久 `steer` supersede 帧因此被误判为模式冲突，触发 `exit_code=1`。内容和附件完全相同；同 ID 真实不同模式仍须拒绝。
- **诊断与修复状态：** 临时捕获只作用于合成本机 E2E，且所有 stderr/IPC 测试诊断都已恢复；产品改动只保留有界 child 退出码/信号回传、mode 持久传播、恢复调用安全处理及已验证的 receipt reconciliation。最终隔离 PostgreSQL E2E 通过，H01 仍因真实 Provider 与客户端验收缺口保持进行中。

### H05 Artifact 审核来源绑定补强（2026-11-22；本机合成输入）

- **实现：** S04 在现有 review packet 中加入 Artifact 的 Workspace revision、canonical SHA-256 和从持久 Task 修订证据引用解析出的 `source_call_id`，并校验 Artifact workspace 与当前 Task workspace 一致、路径和摘要有效、大小在既有限制内。缺失 CallID、Artifact 记录或来源/元数据无效、超限时，证据状态保持 `unavailable`；没有读取或注入产物字节。review system prompt 明确说明 CallID 与 revision/hash 绑定的只是来源和 metadata，不能证明执行环境或产物内容正确。
- **验证：** `cd server && go test -count=1 ./internal/worker -run '^TestTaskValidationReview'`、`cd server && go test -count=1 ./internal/app -run '^TestTaskValidation'` 均通过；回归检查合法 CallID/revision/hash 同条进入输入包、缺 CallID/缺 hash、错 workspace、负 revision、超限 size 和缺失 Artifact 时 fail closed，原始测试产物字节未进入请求。`git diff --check` 通过。
- **范围：** 未改数据库 schema、API、Artifact 存储或交付硬门禁。S04/H05 仍未完成：未有可信执行环境身份，review 模型也没有取得可核查的 Artifact 内容；工程/非工程真实场景、客户端和 H06 入口未验收。

### S00 H05 证据读取边界审计 2026-11-22

- **审计结论：** S04 的 `TaskValidationReviewSnapshotReader` 只提供 Task/revision/result 与 Artifact metadata，没有 claim-bound 内容读取方法。仓库已有 `TaskService.OpenTaskArtifactContent`，会复用 Task/Host 授权并从 `WorkspaceSyncService.OpenCommittedArtifact` 读取固定 revision、校验 manifest/block/整文件 SHA-256；但通用方法本身不绑定当前 review claim、Task evidence ref 与 step。metadata 和 hash 证明来源及字节完整性，不证明语义正确。
- **边界与决策：** 把 Artifact bytes 填入远程模型请求会把文件内容发送出本机，违反仓库 `AGENTS.md` 的本地文件约束；本轮未增加 claim-bound 读取、未将内容放进 reviewer 请求，也未调用外部环境。未来若需语义核验，应提供符合本地文件约束的本地复核路径。当前 metadata-only 输入继续要求 reviewer 对无法读取的 Artifact 内容标 `insufficient`，服务端 verdict 保持 `pending`。
- **仍待验收：** 持久 Host/操作记录提供的可信环境身份、工程与非工程真实场景、H06 客户端入口与环境验收。只读审计无代码改动、无测试执行；本地 H09 Token 规模测试另见同轮 TODO/验证记录。

### S00 H02 确认 429 后的 Worker 取消恢复 2026-11-22（本机隔离 PostgreSQL）

- **根因与改动：** `ConversationRouteWorker.RunOnce` 收到分类阶段已确认的 Provider 429 后，如果 worker context 同时取消，会切换到限时 `WithoutCancel` context 完成数据库恢复；原代码却把已计算的重试延迟清零，导致存储层将 429 按不可重试结果失败。现保留确认的有界 Retry-After，只将恢复事务从已取消的请求 context 解耦。
- **Worker 回归：** `cd server && go test ./internal/worker -run '^TestConversationRouteWorkerHandlesOnlyConfirmedBoundedRateLimits$' -count=1` 通过（2.167 秒）；覆盖分类返回已确认 429 后取消 worker context，仍将原延迟传给恢复处理器。
- **持久层回归：** 使用临时 `postgres:16.15-bookworm`（仅绑定 `127.0.0.1:55432`，无持久卷，运行后删除）执行 `cd server && CH_TEST_DATABASE_URL=<本机一次性 PostgreSQL 16 DSN> go test ./internal/store/postgres -run '^TestConversationRouteRetriesConfirmedRateLimitWithNewMeteredIdentity$' -count=1 -v -timeout=3m`，3 个子用例通过，4.451 秒。新增取消竞态用例检查路由仍为 `pending`、attempt 只递增一次、Retry-After 仍生效、拒绝的模型调用和用量账本均为 `rejected`，且 Provider 只调用一次。
- **边界：** Provider 为合成客户端；未验证真实 429/Retry-After 头、真实 worker 进程退出、真实模型首响与流中断。H02 仍进行中。

### S00 H02 模型客户端恢复边界定向验证（2026-11-22；合成 transport）

- **命令：** `cd server && go test -count=1 ./internal/model -run '^(TestCompleteStopsReadingProviderStreamOnCancellation|TestCompleteTimeoutAndResponseBounds|TestProviderRateLimitCarriesBoundedRetryAfter|TestParseProviderRetryAfter)$'`
- **结果：** 4 项通过，1.525 秒。测试覆盖 context deadline 错误分类/响应限制、流读取在取消后停止、明确 429 携带 bounded `Retry-After` 及秒数/HTTP 日期解析边界；超时 fixture 是 10 ms 的本地合成 deadline。
- **边界：** 此组不调用真实 Provider，不等待实际 45 秒网关期限，也不证明真实 Provider 首响应超时、真实断流或 H01 修订重启 E2E；相关状态保持未完成。

### S00 H02 Task 模型空/截断响应分类 2026-11-22

- **实现：** `validateTaskModelResponse` 为可确认的 Task 空输出或截断输出附加有界原因类别。`empty_response` 要求 Provider reported model 非空且符合格式、stop=`stop`、无工具调用、text/reasoning 为空、usage 已知且输入 token 为正、输出 token 为零；`truncated_response` 要求非空合法 reported model、stop=`length`、usage 已知且输入/输出 token 均在边界内并为正。缺失/非法身份、输出或输入用量不一致、零生成的截断均为 `invalid_response`；未知 usage 仍走原 `unknown` 结算。已结算的 invalid Task 响应由恢复门禁阻止再次 reserve/dispatch。
- **跨层边界：** Worker 只将 `CodeUpstreamError` 中精确的 `empty_response` / `truncated_response` 传入恢复摘要；未知或非字符串理由仍归为 `upstream_error`，Provider status 和 client validation 分类优先。Broker 只接受这两个新精确类别作为安全恢复摘要，未知秘密样式理由归一为 `unknown`。没有新增重试；纯路由结构修复仍最多一次、无工具、独立计量。
- **验证命令与结果：** `cd server && go test ./internal/app -run '^(TestModelGatewayRejectsTaskToolCallsOutsideWorkerContract|TestTaskModelGatewayClassifiesOnlyUnambiguousEmptyAndTruncatedResponses)$' -count=1` 通过（0.640 秒）；`cd server && go test ./internal/app -run '^TestCloudDSHToolBroker(ModelRecoveryStoresSafeFailureCategory|ExplainsModelContextLimit|ExplainsUnavailableAttachmentReader|ExplainsInterruptedStream)$' -count=1` 通过（0.950 秒）；`cd server && go test ./internal/worker -run '^TestCloudDSHModelFailureCategoryUsesOnlySafeAllowlistedReason$' -count=1` 通过（0.782 秒）。目标 Go 文件 `gofmt -d` 无输出，`git diff --check` 通过。Gateway 测试覆盖模糊/缺失身份和用量、正向类别、usage 与 provider identity 结算、以及已结算无效响应重放不二次派发。
- **整包 App 回归：** `cd server && go test ./internal/app -count=1` 通过（21.571 秒）。这只覆盖 App 包，不替代 Worker 全包或 PostgreSQL 持久恢复测试。
- **DSH 重试插件评估：** 锁定 `@deepseek-ai/dsh-llm-retry@0.2.2-rc.1` 为 MIT peer dependency，未在 Morde Runtime 注册。插件默认可对空响应、限流、服务端、超时和传输类错误最多重试 5 次，`always` 可无界；策略由 Provider 插件持有，不处理 Morde 的独立调用身份、账本、unknown 或已结算恢复。因此本轮不挂载，避免与 Morde 的有限重试/对账规则叠加。
- **限制：** 完整 App 与上述 Worker 定向合成用例已通过；没有运行 PostgreSQL 持久层或真实 Provider/45 秒超时；没有自动空响应重试的行为因此仍不需要连续空响应重复判断。H02 保持进行中，真实模型、流中断和 H06 入口验收未完成。

### S00 H06 PC 文件同步入口本机验收（2026-11-22；本地合成夹具）

- **基线与实际入口：** 工作树基于 `main`/`origin/main` 的 `e631d4cdd9ec68e1143f93a46f5572a54702862f`，另有本地文档、Runtime 和 Server 改动。启动真实 `morde-pc`（Electron 44.7.0），使用随机 `/tmp` 隔离 profile 和仅监听 `127.0.0.1:65432` 的 synthetic API；从登录页经“设置 → 文件同步”进入实际客户端页面。
- **UI 结果：** 页面显示默认电脑 `Synthetic Default Computer`、运行中、固定目标 `~/workspace/project/<项目名>/`，并提示仅在用户点击同步后更新云端、云端变更不会下载到本机。点击选目录后，macOS 原生 NSOpenPanel 停留在 `/tmp`，无法稳定定位唯一合成夹具目录；按本机验收边界取消选择，未进入项目名/同步范围确认，未调用同步 helper，未发送任何文件内容。
- **自动化与构建：** 在 `clients/desktop/harness/desktop` 执行 `npm run build:test && node --test tests/morde-main-recovery.test.mjs tests/morde-sync.test.mjs`，75/75 通过；执行 `npm run build:portal`（含 Portal `build:pc` 与 workbench build）和 `npm run build` 均通过。Portal 构建有既存 bundle size warning。源代码审查未发现需修的 PC 同步缺口，未修改 PC 文件。
- **清理与结论：** 取消目录选择后停止本轮 Electron 进程组、本地 synthetic API，并删除临时 profile、helper 与夹具；进程复查无本轮残留。UI 验收到同步页和原生选择器，不能记为文件上传/冲突/删除验收通过；这些场景及 PC↔iOS、真实云端目标仍待 H06。未访问共享测试站或上传项目/测试文件。

### H10 工具接纳与最终完成栅栏（2026-11-22；本机隔离 PostgreSQL）

- **实现：** S03 提交 `8d66e8326601ed36682412d0f6ce57406c095ccb`，已 cherry-pick 到主工作树为 `9d3ffac7`。迁移 `00227_task_tool_supersession_fences.sql` 持久化 tool-call admission 与 superseding message IDs；root checkpoint 在 steer 标记 `applied` 的同一事务写入 delivery；工具及 child/Workspace/Task 完成的最终副作用门按 Task 行锁核对 pending 与 applied steer。新 child 保存创建时 root checkpoint baseline；无法证明历史 child baseline 且已有后续 applied steer 时，admission 终态失败关闭，避免错误比较 child-local/root 序号或无界重试。Task 完成候选携带 root Session、Agent instance/代次、授权版本、checkpoint through 和 digest；命中较新 applied steer 时保留 Task running，不创建旧结果事件或 assistant 回执。
- **根因：** 接纳时读取 pending 与副作用最终派发分属不同事务，期间已受理 steer 可变为 applied；只查 pending 会放行旧工具调用。Task completion 同样可能在候选结果生成后看到根 checkpoint 已推进，而旧候选 fence 本身仍有效，因此需要在 Task 行锁下比较候选和较新 applied steer，并返回可恢复的 typed pending 消息快照。
- **PostgreSQL 定向回归：** `go test ./internal/store/postgres -run 'Test(ChildToolAdmissionUsesRootCheckpointBaseline|PostgresAgentLifecycleRequiresVerifiedStopBeforeReplacement|ClaimedMessageMigrationBackfillsConservativeRootCheckpointUpperBound|CommitTaskProgressCompletionFenceRejectsAppliedSteerAfterCandidate|AdmitAgentToolCallLinearizesWithConversationSteer|AppliedSteerSupersedesToolCallsAcrossAdmissionAndDispatch|AgentChildCreateAndCancelFinalGatesRespectSteerOrder|TaskOperationDispatchGateAndRecoveryRespectSteerOrder|MCPHubAndFileFetchFinalDispatchGatesReturnPendingInput|.*Workspace.*Steer|CommitTaskProgressCompletionReturnsLockedPendingInputSnapshot)' -count=1`；`CH_TEST_DATABASE_URL` 指向本机隔离 PostgreSQL 16。结果：限定用例通过，21.285 秒。覆盖 migration backfill、root/child baseline、steer 与 admission/dispatch 的先后顺序、MCP/Hub/file.fetch/Workspace/child create/cancel 终门，以及 applied steer 晚于完成候选时不发布旧结果。
- **App / 编译：** `go test ./internal/app -run 'TestCloudDSHToolBrokerCompletePreservesAppliedSteerOnCheckpointMismatch|AgentChildSession|AgentTool' -count=1` 通过（1.623 秒），确认 Broker checkpoint mismatch 保留 typed pending；`go test ./internal/worker -run '^$'` 通过（仅编译，0.749 秒）；S03 工作树 `git diff --check` 通过。
- **范围：** 这些结果验证服务端/PostgreSQL admission 与最终门禁；旧 Agent 取消/恢复的跨层路径另由上文修订重启 E2E 覆盖。真实 PC/iOS、Provider、站点登录与 60 分钟耐久验收仍未完成；H06 状态不变。

### S00 H04 模型请求停滞提示 2026-11-22

- **改动：** 复用 `task_agent_sessions.activity_progressed_at` 与现有严格 Task/Agent/lease/generation/revision 栅栏。当前 root 或 child Session 的 `model/started` 若超过 `app.ModelCallTimeout` 45 秒仍未进入终态，即使 Runtime heartbeat 新鲜也会触发既有的去重提示；模型终态会清除提示。所有阶段若 `started` 与 Session `running` 状态不一致，快照保持 unknown，既有提示不清除；工具、子 Agent 与 checkpoint 不按旧进展时间单独判停滞。本次没有新增 migration、自动取消或重试。
- **验证：** 使用一次性 `postgres:16.15-bookworm` 容器（只绑定本机回环端口，测试后删除），执行 `cd server && CH_TEST_DATABASE_URL=<本机一次性 PostgreSQL 16 DSN> go test ./internal/store/postgres -run '^(TestPostgresTaskAgentModelCallStallUsesProgressDespiteFreshHeartbeat|TestPostgresTaskAgentStallNoticeWaitsForKnownStaleActivityAndClearsOnce)$' -count=1 -v`，通过（10.944 秒）。新测试八个子用例覆盖：模型期限内不提示、heartbeat 新鲜但 root/child model activity 超期时提示且仅一条、模型已终态不再判超期、child `idle+started` 保持 unknown、新鲜工具 heartbeat 不因旧进展时间误报，以及 root/child `idle+started` 工具快照不会清除既有提示；原有 pending 输入去重、活动恢复清除和 Task 状态不变回归也通过。目标 Go 文件 `gofmt`、`git diff --check` 通过。
- **客户端静态链路核对：** `clients/desktop/harness/desktop/src/morde-main.ts` 加载本地 Portal bundle；`web/portal/src/routes/Assistant.tsx` 订阅 Task SSE，并在 Agent 工作过程中显示 Task snapshot steps。iOS `MordeApp.swift` 将 `task.step` 投影为可见 timeline row，`MIFTaskViews.swift` 渲染该时间线。独立 `clients/desktop/harness/dsh-nanomuse/src/client/TaskDetail.tsx` 使用本机 Rooms ActivityRecord，不消费云端 Task。未运行客户端测试或操作实际客户端；真实 PC/iOS 用户入口呈现该通知仍待验收。
- **限制：** 未调用真实 Provider；45 秒是当前服务端单次请求总期限。工具/子 Agent/checkpoint 的请求级期限、真实慢工具进程、PC/iOS 提示呈现、断线恢复和 60 分钟耐久仍未验收，故 H04 继续进行中。

### S00 H04 工具执行期间活动帧静态审计（修复前） 2026-11-22

- **发现：** `cloud_dsh_session_tree.go` 的帧消费循环在 `tool_call` 分支同步调用 `Tools.Execute`，恢复调用同步执行 `Reconcile`；`readCloudDSHFrames` 虽独立读取 stdout，但写入容量为 1 的帧通道。工具调用未返回前，主循环不能消费并持久化后续 activity/heartbeat，子 Agent 的普通帧也可能占住待处理槽位。当前请求级停滞提示只覆盖 `model/started` 的 45 秒期限；工具、子 Agent 与 checkpoint 仍只有整个 Task 的 ActiveSeconds 及 Task 生命周期取消。
- **处理：** 本轮只读审计，没有改代码或跑测试。现有副作用规则不允许把超时当作“操作未执行”，也不应在没有最新租约/活动证据时自动取消或重派。因此保留未知操作的对账路径，不加固定工具超时；后续若实现监测，应在工具等待期间继续处理经身份/代次/序号验证的 activity 帧，并让失效/超时代次进入 Reconcile，而非重复 Execute。
- **边界：** 以上记录的是补强前缺口；后续通过工具等待期间继续消费活动帧修复了 heartbeat 持久化阻塞。工具/子 Agent 请求级停滞语义、真实慢工具、子 Agent 与 PC/iOS 通知仍未验证，H04 继续进行中。

### S00 H04 工具等待期间活动帧持久化补强 2026-11-22

- **改动：** `runCloudDSH` 在 `Tools.Execute` 和恢复路径 `CloudDSHRecoveryTools.Reconcile` 等待期间，继续从 Runtime frame channel 接收帧，并复用同一 status/activity/heartbeat 校验及持久化逻辑。身份、Task revision、活动转移、当前活动与单调序号验证保持不变。其他业务帧按 FIFO 暂存；暂存达到 16 MiB 阈值后停止读取并等待工具结果，使用既有 channel/pipe 背压，结果返回后再按序消费；读取/解码/活动持久化错误也会保留给主循环处理。没有增加工具超时、自动取消或重试。
- **验证：** `cd server && go test -race ./internal/worker -run '^TestCloudDSHExecutorPersistsHeartbeatsWhileToolExecutionIsPending$' -count=1 -v -timeout=3m` 通过（0.40 秒）；恢复路径 `go test -race ./internal/worker -run '^TestCloudDSHExecutorReconcilesEveryPinnedToolCallBeforeSettling$' -count=1 -v -timeout=3m` 通过（0.36 秒）；`go test ./internal/worker -count=1 -timeout=5m` 全量通过（57.295 秒）。两条合成 worker 回归分别阻塞普通 `Tools.Execute` 与 pinned checkpoint 的 `Reconcile`，断言至少两次 heartbeat 在释放前落库；普通路径还排入 Runtime failure 帧并验证工具结果后再按序处理。Runtime `npm test`（包含 compaction build 与全部 `test/*.test.mjs`）通过，182/182，74.823 秒；其中 `emits heartbeats while an IPC tool call is pending` 通过（15.205 秒）。`node --check runtime/morde-cloud-runtime/test/runtime.test.mjs`、`gofmt` 与 `git diff --check` 通过。
- **范围与限制：** 服务端 worker 侧缓解了“工具调用同步等待导致 heartbeat 无法落库”的缺口；普通工具与 checkpoint 恢复对账都由合成 Runtime/假 Broker 覆盖，没有使用真实 Host 工具或故障中的远端副作用。未操作实际 `morde-pc`/iOS，未使用真实 Provider；工具/子 Agent/checkpoint 请求级期限、未知副作用安全停止、断线恢复和 60 分钟耐久仍待验收。H04 保持进行中。

### S00 H04 操作对账与跨包 race 复验 2026-11-23

- **恢复语义审计：** 追踪 `CloudDSHRecoveryTools`、`CloudDSHToolBroker.Reconcile`、`RuntimeToolExecutor.ReconcileOperation`、`observeScheduledField` 与 `RuntimeOperationRunner.Execute`。当前实现只在原 Runtime lease fence 的精确查询得到 `AgentToolNotFound` 时，才以相同 operation ID 重 arm；其它错误或 fence 不匹配均保留 unknown。Runtime Manager 在启动 handler 前持久记录 accepted/running；相同实例中的活跃操作查询得到既有记录，同 ID 重复提交返回原记录而不重复执行。Runtime 存储损坏/故障或服务未就绪会失败关闭，不作为 NotFound。未发现需改 app 生产逻辑的缺陷。
- **新增回归：** `TestRuntimeToolExecutorReconcilesActiveDispatchAfterTaskLeaseReclaim` 使用进程内 Runtime Manager 和阻塞 handler：旧 dispatch 处于 running 时回收 Task lease，恢复仍按原 Runtime lease 查询并取得旧结果；handler 调用次数为 1，恢复没有 POST 第二次 dispatch。现有相邻用例继续覆盖精确原 lease 查询和仅在显式 NotFound 后同 ID retry。
- **Account Runtime 握手复核：** 当前 Go 校验的 ready frame 需要与 Node `account-runtime.mjs` 的 producer 对照的字段一致；两端的 `runtime_start` / `runtime_ready` 字段和 `0.2.0-rc.2` engine version 匹配。旧快照 `fee63a35` 的 race 握手失败未在当前主线重现，但旧日志没有原始 stdout/stderr，不能据此证明旧失败的具体根因。
- **验证：** `cd server && go test ./internal/app -run '^(TestRuntimeToolExecutorReconcilesActiveDispatchAfterTaskLeaseReclaim|TestRuntimeToolExecutorReturnsNotFoundOnlyAfterExactOriginalLeaseQuery|TestRuntimeToolExecutorRetriesSameIDOnlyAfterExplicitNotFound)$' -count=1` 通过（1.972 秒）；`cd server && go test -race -count=1 ./...` 全部 Go 包通过，`internal/app` 42.532 秒、`internal/worker` 65.795 秒；本轮前述工具执行/恢复 heartbeat 定向 race 用例、worker 全量与 Runtime 182/182 也通过。源码格式检查和 `git diff --check` 通过。
- **仍待验范围：** 未操作远端测试站、真实 Host 工具进程或真实 Provider；真实 Provider 验证暂缓，因为项目会话约定要求聊天明文出现的凭据先按已暴露处理、撤销并轮换，新凭据只从本机环境变量/密钥存储读取。工具/子 Agent/checkpoint 的请求级停滞策略、PC/iOS 实际入口、60 分钟耐久及旧握手失败的原始现场仍未验证。H04、H00 保持进行中，当前工作区改动未提交/推送。

### S00 H05 来源身份 SQL 静态审计 2026-11-22

- **核对：** `server/migrations/00150_conversation_result_aggregation.sql` 给 `conversation_result_sources` 建立复合外键：来源行的 `(batch_message_id, owner_id, conversation_id, actor_id)` 必须对应其批次；`(task_id, owner_id, conversation_id)` 必须对应 Task；`(task_id, owner_id, actor_id)` 必须对应 Task 的 initiator。路由加载先验证 batch owner/actor/conversation 与 route claim 相同，再对比 batch authorization version、source authorization version 和当前 claim version。因此只靠错绑来源行跨入另一批次/Task 的静态候选已由现有数据库约束和版本检查阻止，本轮未改 SQL。
- **锁覆盖：** `conversationTaskResultValidationStateInTx` 使用 `FOR SHARE OF review, task, event`。现有 `TestConversationResultReceiptLocksValidationInputsUntilCommit` 直接验证 Task 与 event 更新被阻塞；尚未直接验证 `task_validation_reviews.validation_state` 更新被阻塞。这是测试覆盖缺口，不是已确认的生产锁缺陷。
- **补充回归与验证：** 扩展 `TestConversationResultReceiptLocksValidationInputsUntilCommit`，证明 `task_validation_reviews.validation_state` 的并发更新在回执事务持有共享锁期间被阻塞，并在回执提交后成功。使用一次性 `postgres:16.15-bookworm`，端口仅绑定 `127.0.0.1:55433`，无持久卷，测试结束后移除容器；`CH_TEST_DATABASE_URL=<本机一次性 PostgreSQL DSN> go test ./internal/store/postgres -run '^TestConversationResultReceiptLocksValidationInputsUntilCommit$' -count=1 -v -timeout=3m` 通过（测试 1.98 秒，包 3.947 秒）。
- **范围：** 静态检查上述迁移、入队/加载/发布 SQL 与既有集成测试，没有产品 SQL/schema 改动。错绑候选由现有复合外键/版本比对阻止。H05 仍因可信执行环境身份、可核查 Artifact 内容和 H06 入口验收未完成。

### S00 H01 长前文截断与授权检索提示断言对齐 2026-11-22（本机隔离 PostgreSQL）

- **复核：** `ConversationRouteModelRequest` 对长历史预览设置 `content_truncated=true`，并使用包含 `morde_current_conversation_history_search` 的授权历史检索指引及不推测省略内容规则。第一次集成运行发现测试仍要求已废弃的 `history_incomplete is true` 提示字样；只更新测试断言匹配当前提示契约，未改生产路由。
- **验证：** 使用临时 `postgres:16.15-bookworm`（仅绑定 `127.0.0.1:55432`，无持久卷，运行后删除）执行 `cd server && CH_TEST_DATABASE_URL=<本机一次性 PostgreSQL 16 DSN> go test ./internal/store/postgres -run '^TestH01Long' -count=1 -v -timeout=3m`，4 项通过，8.645 秒。首跑因旧提示文案断言失败；更新后同组复跑全部通过。
- **边界：** 该组验证 PostgreSQL 路由快照和分类请求，不调用真实 Provider，也不替代 PC/iOS 长历史入口或跨账号授权验收；H01 仍进行中。

### S00 H09 累计 Token 规模合成回归 2026-11-22

- **服务端摘要：** 新增 `server/internal/worker/conversation_route_h09_token_scale_test.go`，以显式的合成估算器循环调用现有增量摘要流程，覆盖累计 4K、8K、100K、200K tokens。每个 compact 请求不超过 3,328-token（4,096 窗口减输出预留）或现行 96 KiB 输入界限；检查摘要首/中/尾锚点、全量来源 proof、原文 fixture 回读及 SHA-256。命令 `cd server && go test ./internal/worker -run '^TestConversationRouteH09SyntheticTokenScaleCompaction$' -count=1 -v` 通过（0.928 秒）；`cd server && go test ./internal/worker -count=1` 全包通过（56.057 秒）。
- **DSH Runtime：** `runtime/morde-cloud-runtime/test/hermes-compaction-runtime.test.mjs` 使用锁定 DSH `estimateMessage`/`estimateToolsTokens` 构造合计恰为 4K/8K/100K/200K 的多条合成消息；Runtime 测试 context 分别为 4,096/8,192/65,536/131,072，逐项断言输入估值和 tools schema 不超过窗口扣除 `max_tokens` 输出预留，且每条消息低于 128 KiB。覆盖摘要来源序号、授权源读取的 tool-call/result 配对、checkpoint 的 1 MiB/page、8 MiB 总恢复字节和 100K event 上限，以及完成态恢复经一次 ledger reconciliation 后不重复写入 source/tool-result 事件。先前恢复驱动没有应答该 reconciliation tool call 而挂起；补入同一已保存结果后复验，不改生产恢复逻辑或放宽预算断言。`node --test --test-concurrency=1 test/hermes-compaction-runtime.test.mjs` 通过 6/6（2.313 秒）；`npm test`（Node 26.13.0，含 build）通过 181/181（59.549 秒）；`node --check test/hermes-compaction-runtime.test.mjs` 和 `git diff --check` 通过。
- **限制：** Runtime 使用 DSH 固定 TokenMeter 估算和本地 fake provider；服务端规模使用显式合成 token 计数与 fixture store。它们验证压缩选择、预算和来源链路，不代表真实 Provider tokenizer/usage、PostgreSQL 原文检索、PC/iOS 实际累计历史或跨账号隔离。H09 仍待真实客户端/模型及耐久验收。

### S07 本轮 P0 集成复验（2026-11-22；部分通过，H06 未完成）

- **基线：** 使用独立、干净的集成工作树 `codex/harness-h06-integration`，commit `fee63a35e2b944673ede57b5768a83e4853b05ac`；测试前后 `git status --short --untracked-files=all` 为空。DSH 锁定版本为 `0.2.2-rc.1`（`runtime/morde-cloud-runtime/package.json` 与 `package-lock.json`）。并核对 `engine.mjs` 实际注册 `SessionStore`、`SessionProjectionRegistry`、`AgentRegistry`、`SystemPrompt`、`ToolRuntime`、native tool executor、`LlmRuntime`、persistence、`AgentLoop`、checkpoint policy、`TokenMeter`、`ToolResultPruner` 和 Morde compaction engine；`dsh-mcp-client` 仅在依赖中，MCP tools 仍经 Morde Broker 注册；本版本未注册 DSH subagent/team/sandbox 插件。主记录工作树另有既存文档及 iOS 测试改动，未纳入该基线或由本轮覆盖；完整插件盘点见 TODO H00。
- **Go 回归：** `go test -count=1 ./...` 用时 330.159 秒；除 worker 外各包通过，worker 有两个 30 秒/3 秒时序用例失败。隔离重跑这两项后 `2/2` 通过（2.524 秒），原始结果仍保留于 `/tmp/s07-go-test-fee63a35.log`、`/tmp/s07-go-worker-repro-fee63a35.log`。`go test -race -count=1 ./...` 的 worker 用时 177.431 秒，记录 54 条失败、56 处 `account Agent Runtime handshake is invalid`，并有 DSH browser/search 链路超时；其余 Go 包通过。race 模式下隔离重跑 `TestCloudDSHExecutorRunsHostlessServerTaskWithoutCompletingIt`（2.265 秒）和 `TestDSHBrowserToolCompletesThroughWorkerAndRecordsOperationAndStep`（31.949 秒）仍失败，日志 `/tmp/s07-go-race-fee63a35.log`。只读调用链检查显示 `Acquire()` 收到账号 Runtime 首个 stdout frame 后，`cloud_dsh_account_runtime.go` 的严格 ready 校验失败；测试 supervisor 丢弃 stderr，日志也未保留原始 frame，因此具体不匹配字段/启动错误无法证实。Node producer `account-runtime.mjs` 与 Go 的 `runtime_start`/ready 结构从静态检查看相符。浏览器/搜索链路共享 `cloudDSHFixture`，搜索超时显示 0 rounds、0 broker calls，可能是同一启动失败的下游表现，尚未证实。S00 已在总控明确由 S02 拥有 Go/Docker Account Runtime supervisor（`cloud_dsh_account_runtime*.go`、`docker_cloud_dsh_account_runtime*.go`）及 browser/search 集成测试。S02 任务书将 `account-runtime.mjs` 限定为 status/activity/heartbeat 帧职责；若根因要求改 Node startup producer，应先修订 owner/接口范围再改，具体交接见总控。若定位到 S03 Broker/账本问题再做 owner 间接口交接。S07 未改产品代码。
- **隔离 PostgreSQL：** 在本机 `morde-local-postgres` 上运行 Task Agent activity ordering/fencing、heartbeat non-progress、stall notice stale/clear、progress clear 四项，用例内创建并清理临时数据库；`4/4` 通过，7.133 秒。没有 `CH_TEST_DATABASE_URL` 的全包运行跳过其他数据库钩子；此前 S01–S06 在同一 `fee63a35` 集成基线的 10 项 PostgreSQL 回归 `10/10` 通过，详见上方既有记录。
- **DSH Runtime / Portal / PC 自动化：** 本机 Node `v24.22.2`。首次 Node 26 Runtime 全量 `157/158`，唯一失败是 `resumes a completed coordinator checkpoint and returns its saved text without a model call` 在 5 秒内未收到 resume frame；改在 Node 24 全量复验后 `158/158` 通过（39.375 秒），该失败未复现。命令 `PATH=/opt/homebrew/opt/node@24/bin:$PATH npm test`，日志 `/tmp/s07-runtime-node24-fee63a35.log`。Runtime 离线安装缺少缓存 `undici@8.11.2`；为不访问包仓库，临时链接到主工作树 Runtime 的本机 `node_modules`，确认两工作树 `package.json`/`package-lock.json` 哈希相同且 `undici` 为锁定的 `8.11.2` 后运行，结束时移除链接；fee63 测试工作树保持干净。Portal Node 26 和声明支持的 Node 24 全量均为 `792/838`，Node 24 重跑耗时 27.59 秒：46 项断言失败分布在 19 个有失败用例的文件，另有 1 个 suite setup 失败（20/66 个测试文件标失败）。缺少忽略目录 `.local/muse-a00-contract-v1.json` 导致 setup 失败。Node 24 下 `npm run typecheck` 再次通过（exit 0）。测试日志 `/tmp/s07-portal-node24-fee63a35.log`。Desktop `npm test` `288/288`、`typecheck` 通过，日志 `/tmp/s07-desktop-tests-fee63a35.log`。
- **Portal 失败归因（只读）：** 46 个断言失败可归为：18 项找不到“重试”按钮；3 项 `.notice-center` class 期望与实际 error overlay 不符；10 项 PersonalData 测试期待的 `动态/偏好/灵感` 导航项不在 `PortalLayout.personalSections`，尽管 `PersonalData` 有对应 section；3 项需要的恢复链接/按钮被 `FloatingError` 消去或 Toast 模式隐藏；2 项 Connections alert 未找到；其余 10 项为文案、状态或 selector 期望不匹配。共享 `toast.tsx` 的 error 分支不保留 action，但 `use-network-read-notice.ts` 传入 retry action，相关 Toast 单测也固定了当前行为；`FloatingError.tsx` 同样会过滤子控件。以上暴露 route 与共享 UI 契约不一致，但不能仅据此判定产品缺陷；Connections 与其余文案/selector 项仍需各 route owner 单独核对。没有 `Assistant.test.tsx` 失败，因此本轮 Portal 结果未指向 S05 Assistant 改动。后台 Admin route 是否归 S05 不明确，修复前需按实际产品端文件 owner 交接。当前没有修改代码。
- **iOS 本地入口：** `Morde` scheme、iPhone 18 Pro / iOS 27.2、Xcode 27.2；显式选择的七个 loopback fixture UI 用例覆盖顺序发送、失败原位重试、断网重启、受理状态对账、运行中追问/独立消息及 204 后时间线重连，`7/7` 通过、0 跳过。结果包 `/tmp/morde-s07-ios-20261122-morde-fixtures.xcresult`；有既存 UIKit hierarchy runtime warning。`MinisTests/MIFApprovalViewsTests` 的 19 项在 `fee63a35` 未进入 XCTest：build 缺少生成的 `Configs/ProviderCustomization.xcconfig`、`Rclone.xcframework` 和七个 FFmpeg XCFramework；结果包 `/tmp/morde-s07-ios-20261122-approval-tests.xcresult`。只读本地检查发现这些依赖仅在另一份 dirty 主工作树中有现成产物；干净基线的 Provider config 可用空值 `.example` 模板生成，Rclone 源码/模块可关闭代理离线解析，但 FFmpeg `6.1.2` 源码与构建脚本要求的本地 tarball 不存在，脚本会从 ffmpeg.org 下载。为保持 S07 指定远程主机边界及基线纯度，未复制主工作树的产物、未生成/下载依赖。另一次未计划的宽范围命令部分执行 12 项，其中 9 项审批 UI 断言失败、3 项跳过，随后中止；结果包 `/tmp/morde-s07-ios-20261122-morde-ui.xcresult`，不代替七项 fixture 验收或 19 项审批测试。
- **远端及真实客户端（早期检查）：** 首次 S07 检查只对用户指定测试站执行匿名 `curl --head --max-time 15`，HTTP/2 200；未做登录或写入。未找到本机 S07 environment variables，也未找到以文档变量名作为 Keychain service 的条目；按仓库 [S07 秘密管理要求](../Morde_Harness_并发实施/10-各会话开发提示词.md#s07-测试环境变量)，聊天中提供的 DeepSeek key 按已暴露处理且未使用。当时用户指示先保持 H06 待验证，因此未继续配置密钥或凭据，也未连接 Provider、登录管理员或重置数据库密码；当时已运行的 `morde-pc` 服务目标未确认是指定站点，未向现有会话发送消息。真实 PC↔iOS 同会话、Task 期间追问/停止、双账号隔离、超过模型窗口的累计历史及 60 分钟耐久均未执行。
- **PC 短文本真实回复补充：** 使用本机当前工作区构建（本地 Portal bundle + Electron）的 `morde-pc`，临时隔离 profile `/tmp/morde-h04-morde-pc-profile`，启动参数 `--force-renderer-accessibility`，登录用户指定测试站 `https://portal.120.25.178.93.sslip.io`。在新建空旁聊中发送合成文本“这是本机 morde-pc 验收探针。请只回复 PC-REAL-OK。”；页面显示模型 `DeepSeek Flash · deepseek-flash`，并收到实际回复 `PC-REAL-OK`。没有上传或发送项目文件，也没有修改远端数据库。此项只证明当前本地 PC build 对测试服务的一次普通短文本真实回复；不证明 Task/停滞提示、iOS 接续、跨账号隔离、远端服务版本与当前工作树一致或耐久。
- **S07 iOS 同会话真实服务补测（2026-11-22）：** 为仅本轮真实服务探测临时加入 `MordeUITests` opt-in XCTest，并以仅允许指定 HTTPS 主机的断言保护目标；`xcodebuild build-for-testing -project clients/nanoMuse/android/src/ios/Morde.xcodeproj -scheme Morde -destination 'id=E1C86EF7-3EB6-4348-9233-7C197A7E3490' -parallel-testing-enabled NO -derivedDataPath /tmp/morde-h06-ios-derived` 通过。Shell 环境不会自动传入 XCTest：首次试跑跳过；第二次尝试因抽屉搜索字段未获得键盘焦点失败，之后改为通过旁聊行 label 精确选择。最终试跑在 iPhone 18 Pro / iOS 27.2 专用模拟器使用已有账号会话，成功读取 PC 新建的旁聊行 `c66sqvzg` 和回复 `PC-REAL-OK`；该次临时测试的用户提示断言误用了大写 `PC`。只读消息 API 返回该会话 11 条消息，确认其中包含 PC 用户提示、PC 回复、iOS 追问和 iOS 回复；随后运行精确匹配小写 `morde-pc 验收探针` 的只读 UI 回归，`build-for-testing` 通过且单项 `test-without-building` 1/1 通过。探针随后仅发送一次合成 iOS 消息“请只回复 IOS-REAL-OK。”；XCUITest 退出后重新启动实际 App，消息一度显示“发送结果待确认”并带有“重试”；未点击重试。等待约 15 秒后，客户端的恢复核对将同一消息更新为“已发送”，并显示真实回复 `IOS-REAL-OK`。因此指定旁聊的 PC→iOS 历史、双向短文本和一次不重复重试后的自动状态核对通过；不代表 Task/停滞提示、双账号隔离、文件夹同步或耐久验收通过。结果包 `/tmp/morde-h06-ios-remote-20261122-attempt3.xcresult` 和 `/tmp/morde-h06-ios-history-readonly-20261122.xcresult`，消息自动恢复截图 `/tmp/morde-h06-ios-after-reconnect-wait.png`，UI hierarchy `/tmp/morde-h06-attempt3-all/B7EF4FF5-A82D-4DEB-AE3D-DC3FFC56E2F7.txt`。临时 XCTest 与临时 scheme 环境变量已恢复；未更改远端数据库或重试不确定消息。
- **S07 结论：** 自动化证据有通过也有失败。早期真实站点/Provider/跨端/耐久探针因安全凭据不可用而阻塞；后续本机 PC 临时 profile 对用户指定站点登录成功，并收到一次 DeepSeek Flash 合成短文本回复（见“PC 短文本真实回复补充”）；iOS 已读取相同旁聊的 PC 用户提示与回复，发出的合成追问经自动对账后收到回复（见“S07 iOS 同会话真实服务补测”）。Task 与停滞提示、双账号隔离、文件夹同步和耐久仍未验收，H06 保持待验证。

### S00 H06 PC 文件夹同步 UI 安全检查 2026-11-22（未执行同步）

- **检查：** `arc-cua` 列出了一个标题为 `morde-pc` 的 Electron 窗口，但该进程没有暴露可核验的隔离 `user-data-dir`。为避免操作可能属于用户的默认 profile，本轮未向窗口输入、点击或切换账号。
- **边界：** 未打开目录选择器、未触发同步、未上传/外发任何文件。用户明确要求文件仅在本地修改；远端真实同步验收保持待验证。此前本地合成同步测试与页面说明检查见 H06 现有记录。

### S00 H06 PC 项目上传删除来源审计 2026-11-22

- **删除来源：** `runtime/morde-sync` 的 LiveSync 只对共同基线中存在、服务端生成删除动作且当前内容仍匹配基线的路径执行删除；删除前保留恢复副本，内容变动会转为冲突。目录读取/元数据错误会使扫描失败，客户端在建立同步 session 前退出，不能用部分清单代表完整目录。
- **项目上传：** 服务端从目标 `SourceBindingID` 对应且 Host 已确认 `applied` 的 session manifest 生成删除候选，不把合并后的 Workspace manifest 当成本机来源快照。项目上传客户端不能直接提交 `DeletedPaths`。远端独立新增不属于 PC 来源，因此不进入删除集；远端改动已同步路径时计划逻辑报冲突并过滤删除，Host 应用前还检查 pinned 当前状态、哈希及目录非空条件。现有 PostgreSQL `TestWorkspaceProjectUploadUsesInstallationCredentialWithoutSyncthingBinding` 覆盖“初次 PC 空项目、云端新增文件、再次手动空快照后云端文件保留”；`TestProjectUploadBaselineComesFromHostConfirmedSourceNotWorkspaceRevision` 验证基线来源；`TestProjectUploadConflictDoesNotApplyNonconflictingMutations` 验证冲突原子性。
- **补充三方冲突回归：** 新增 `TestWorkspaceProjectUploadPreservesRemotelyModifiedFileWhenLocalSnapshotDeletes`，建立 Host 已确认的 PC 来源基线，再模拟云端独立修改已同步文件并提交本机不含该文件的下一次完整快照；断言计划保留 `delete_changed` 冲突、过滤上传删除，远端 revision 中仍保留修改后的文件。使用一次性 `postgres:16.15-bookworm`，端口仅绑定 `127.0.0.1:55433`、无持久卷，测试后移除；`CH_TEST_DATABASE_URL=<本机一次性 PostgreSQL DSN> go test ./internal/store/postgres -run '^TestWorkspaceProjectUploadPreservesRemotelyModifiedFileWhenLocalSnapshotDeletes$' -count=1 -v -timeout=3m` 通过（测试 1.14 秒，包 3.069 秒）。
- **仍待验范围：** helper 侧已有单边删除、删除并发替换、双方修改保留、远端删除与本地编辑冲突回归；嵌套 `ReadDir` 失败尚未直接注入，且本轮未操作客户端/Host 或发送文件。实际 PC/Host 手动同步仍待 H06。

### S07 H11 真实站点子 Agent 委派探针（2026-11-22；未通过）

- **操作：** 在本机当前工作区启动的 `morde-pc`、隔离 profile `/tmp/morde-h04-morde-pc-profile` 和用户指定测试站的新旁聊 `cxqiwn07` 中，提交纯文本 A/B 分工任务，要求主 Agent 通过两个 `dsh_delegate` 并行执行，只做合成文本处理，不调用文件、终端、网络或云端电脑。
- **结果与证据：** PC 页面显示“AI 运行中断，无法确认本次执行结果。请停止任务后重试。”只读 `GET /api/conversations/cxqiwn07/messages?limit=100` 返回一条用户消息并关联 Task `q86mee2m`；只读 `GET /api/tasks/q86mee2m` 返回 `status=waiting_user`、`waiting_reason=dsh_recovery_unresolved`、`current_revision_started=false`，Agent 快照只有 `role=main,status=unknown`，`child_jobs=[]`，没有结果或 assistant 回执。因此未观察到真实子 Agent、并行重叠或主 Agent 汇总，`MH-AGENT-001` 未通过，H11 不勾选。
- **边界：** 未点击“重试”，避免在上次执行结果未知时重复派发；未改远端数据库，也未上传或发送项目文件。该次不能证明当前测试站服务与本地 `main` 的代码版本一致。

### S00 H11 并行子 Agent 取消隔离回归 2026-11-22

- **改动：** 将既有“两子 Agent 并行、父 Agent 消费两份结果”Runtime 用例改为更强的组合：两个 child model 请求确认同时在途后，由主会话取消子 Agent A；断言只有 A 收到 `model_cancel`，A 等待持久结算为 cancelled，B 保持运行并返回成功结果，父 Agent 读取两份状态与结果后汇总。
- **验证：** `cd runtime/morde-cloud-runtime && npm run build:dsh-compaction && node --test --test-concurrency=1 --test-name-pattern='cancelling one of two concurrent background children' test/runtime.test.mjs` 通过，1/1。随后 `node --test --test-concurrency=1 test/runtime.test.mjs` 全量通过，157/157，55.119 秒。
- **边界：** 这是本地 DSH Runtime 合成 IPC/模型回归；指定测试站的实际 PC 探针此前停在 `dsh_recovery_unresolved`，没有观察到子 Agent，且未重试未知任务。H11 仍未通过真实服务入口验收。

### S00 当前 main 工作区回归 2026-11-22

- **代码基线：** `main` / 刚 fetch 的 `origin/main` 均指向 `a23d7e7572f349211992bdea69c541a31a29de5c`；以下是该基线上的本地未提交改动回归。
- **Go 回归：** `cd server && go test ./internal/app -count=1` 通过（21.728 秒）；`cd server && go test ./internal/worker -count=1` 通过（58.961 秒）。H01 长历史 PostgreSQL 4 项及 H02 确认 429 重试 PostgreSQL 3 个子用例另见本记录对应小节。
- **Runtime 回归：** `cd runtime/morde-cloud-runtime && npm run build:dsh-compaction && node --test --test-concurrency=1 test/runtime.test.mjs` 通过，157/157，55.119 秒；新增/加强的 H11 并行取消隔离场景包含在内。
- **静态检查：** `git diff --check`、所有修改 Go 文件的 `gofmt -d`、`node --check runtime/morde-cloud-runtime/test/runtime.test.mjs` 均通过。
- **未覆盖：** 本轮未重跑整个 PostgreSQL 包（完整包此前达到 10 分钟仓库超时上限并停在迁移初始化）；未调用真实 Provider，也未重试远端 `dsh_recovery_unresolved` Task。未提交/推送这些本地开发改动。

### 单次执行信息模板

```text
执行日期/时区：
Morde 提交 / 工作区状态：
DSH 锁定版本：
隔离环境 / 数据库迁移：
客户端及版本：
模型 / 推理配置 / 窗口：
Tokenizer 与 token 统计来源：
原始失败样本 / 累计历史规模：
预算拆分（系统/工具/当前输入/近期历史/摘要检索/输出）：
根因分类与证据：
机制修复点及覆盖更大规模的理由：
原失败样本与 4K/8K/100K/200K 回归结果：
测试账号（脱敏）：
用例 ID 与状态：
消息 ID / 会话 ID / Task ID（按需）：
预期：
实际：
证据位置（仅本地）：
失败/阻塞原因与未覆盖范围：
```

## 6. 验收完成条件

- P0 自动化项通过，或每个未通过项有明确的失败/阻塞记录；不得通过放宽断言或把模拟结果记为真实通过。
- `MH-CTX-001` 证明实际处理了大于 8,192 tokens 的单条输入；`MH-CTX-002` 证明多轮压缩后历史、原文和最新消息都可用；`MH-CTX-003` 单独证明真实模型会话的累计原始历史超过其单次窗口。
- `MH-CTX-004` 证明历史检索不提升指令权限且继续满足账号/会话授权隔离；它不能代替真实模型窗口验收。
- 发现上下文缺陷时记录预算分解、根因分类、证据和机制修复，并通过原复现及 `MH-CTX-005` 的 4K/8K/100K/200K 累计历史回归；不得通过缩小输入或孤立提高限制判定修复完成。
- `MH-DUR-001` 同一会话与 Task 的墙钟耐久不少于 60 分钟，期间经过真实执行、等待或恢复并有可观察进展和分时对话；另记录 `ActiveSeconds`。续租本身不作为进展证据；该用例不声称单次连续活动超过当前 1,800 秒预算上限。
- 实际 PC 与 iOS 入口各自验收；PC 页面构建、浏览器演示和服务端/单元测试不能替代客户端证据。
- 记录所有未测项和环境限制；没有可核查证据时不宣称通过。

## 7. 参考

- [Morde Harness 最终流程与实现规格](../Morde_Harness_最终流程与实现规格.md) §2.1–2.4。
- [Morde Harness 实施任务 TODO](../Morde_Harness_实施任务_TODO.md) H00–H11，尤其 H06、H09–H11。
- [Morde AI 对话长会话测试方案](../design/Morde-AI对话长会话测试方案.md) LC-01–LC-06。
- [Morde PC 端 AI 对话测试用例](PC端AI对话测试用例-2026-11-12.md) 既有 PC 场景及历史执行记录。
