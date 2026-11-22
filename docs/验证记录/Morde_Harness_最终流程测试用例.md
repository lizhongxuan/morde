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

### 旧 LC 用例映射到当前 MH 范围

| 旧用例 | 当前承接用例 | 保留的验收意图与边界 |
|---|---|---|
| LC-01｜长消息后短追问 | MH-CTX-001、MH-MSG-001 | 短追问不能因前一条长消息而失败；输入是否超过 8,192 tokens 必须另有 tokenizer/Provider 证据，不能用旧用例的字节数替代。 |
| LC-02｜40 轮及多次压缩 | MH-CTX-002 | 4K 合成窗口、多轮压缩、早期与最新锚点、消息/工具组完整性。 |
| LC-03｜运行中追问、独立任务与重启 | MH-MSG-001、MH-REC-001 | 检查任务关联、独立任务隔离、检查点恢复和不重放；真实 DSH/PC 入口仍须单独验收。 |
| LC-04｜摘要/Provider 失败恢复 | MH-FAIL-001 | 保留无效摘要、上下文溢出、取消、版本冲突和有界恢复语义。 |
| LC-05｜会话与账号隔离 | MH-ISO-001、MH-CTX-004 | 交叉追问不得泄漏其他账号/会话历史；历史检索文本仍是不可信数据。 |
| LC-06｜PC/iOS 接续与重连 | MH-CLIENT-001、MH-DUR-001 | 两个真实客户端接续同一会话并检查消息、进度、结果和恢复；模拟器或合成接口测试不替代实测。 |

该表只迁移旧方案的测试意图；当前规格的 token、授权、运行时与客户端边界优先，旧 LC 输入数字和“通过”结论不自动沿用。

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
| MH-AGENT-001 | 规格 06、2.2；H11 | 未执行 | |
| MH-GATE-001 | 规格 09–10；H05 | 未执行 | |
| MH-ISO-001 | 规格 2.1；H09 | 未执行 | |
| MH-CLIENT-001 | 规格 2.4；H06 | 部分通过（PC 单次回复；iOS 本地发送 UI 用例通过）；同会话追问及 PC/iOS 接续未验收 | PC 短文本及 42,603 字节合成长文本首轮收到真实回复，但长消息后的历史追问未回复。iOS 本地模拟器用例仅覆盖未知云端模型状态下的消息受理，未连接真实 Provider，也未与 PC 接续同一会话。端到端客户端验收仍未完成。详见下方实际 PC 探测记录和“本地自动化证据”中的 iOS 行。 |

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
| Portal Assistant 行为与类型 | `web/portal` 执行 `pnpm vitest run src/routes/Assistant.test.tsx`、`pnpm run typecheck` | 2026-11-22 当前工作树：Assistant 测试 209/209 通过；类型检查通过。覆盖任务状态/进度、追问投递、停止与恢复、结果整理、事件重连和长消息状态展示；仍不替代真实模型与 `morde-pc` / iOS 验收。 |
| iOS 本地消息受理 UI | `xcodebuild test -quiet -project clients/nanoMuse/android/src/ios/Minis.xcodeproj -scheme morde -destination 'platform=iOS Simulator,id=E4D916C4-5693-4DF4-9403-ED939CF5A394' -only-testing:MinisUITests/MordeChatModelGateUITests/testUnknownCloudModelStatusDoesNotBlockMessageAcceptance -parallel-testing-enabled NO -derivedDataPath /tmp/morde-h06-ios-20261122 -resultBundlePath /tmp/morde-h06-ios-20261122/ios-send-final2.xcresult` | iPhone 18 Pro / iOS 27.2 专用本地模拟器：1 通过、0 失败。仅验证云端模型状态未知时客户端仍受理消息；使用本地 fixture，不证明 Provider 回复、长对话、跨设备接续或实际 iOS 生产配置。结果包保存在本机 `/tmp/morde-h06-ios-20261122/ios-send-final2.xcresult`。 |
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

### 本机运行中消息投递回归（2026-11-22；分层自动化测试）

- **环境：** 使用 loopback-only 的临时 PostgreSQL 16 容器和本地 Node DSH Runtime；容器及隔离数据库已在测试后删除。模型使用合成响应，没有发送项目文件或访问远程测试服务。
- **PostgreSQL 路由：** `go test ./internal/store/postgres -run '^TestConversationRouteSteersOnlySafeRunningTaskSupplement$' -count=1` 通过（3.941 秒）。正在运行的 Task 收到安全补充后，消息只关联该 Task 并以 `steer/pending` 等待投递；改变任务范围的输入进入澄清，不被误当作补充。
- **Go DSH executor：** `go test ./internal/worker -run '^(TestCloudDSHExecutorDeliversMessageDuringModelCall|TestCloudDSHExecutorDeliversPendingConversationMessageOnce)$' -count=1` 通过（4.531 秒）。模型调用期间到达的两条消息按顺序进入后续模型轮次，重复投递仍按消息 ID 去重。
- **Node DSH Runtime：** `node --test --test-name-pattern='account runtime forwards fenced running message delivery to the task agent|replays an enqueued steer after its checkpoint acknowledgement is lost|does not claim a steer twice when restart follows its claim checkpoint' runtime/morde-cloud-runtime/test/runtime.test.mjs` 通过（3/3，2.105 秒），覆盖任务栅栏、入队检查点回执丢失重放及领取后重启不重复领取。
- **长历史及推理中补充：** `go test ./internal/worker -run '^TestConversationSummaryDSHMainAndSideChatsContinueAfterLongHistoryRestart$' -count=1 -timeout=8m` 通过（29.80 秒）；使用 840 条连续 predecessor 历史，在本地 Node DSH Runtime 重启后完成授权原文搜索。另在模型调用挂起时向运行中的 Task 投递安全补充，确认旧模型输出及工具调用在执行前被丢弃，DSH 将新消息与授权历史检索纳入后续推理并完成任务；后续并发路由断言也通过。该结果是本地合成模型/隔离 PostgreSQL 证据。
- **运行时回归：** 在 `runtime/morde-cloud-runtime` 目录执行 `node --test --test-concurrency=1 test/runtime.test.mjs test/compaction-recovery.test.mjs test/hermes-compaction-runtime.test.mjs`，144/144 通过（55.943 秒）；覆盖运行时、压缩恢复及 Hermes 压缩行为。另执行 `node --check src/engine.mjs` 与 `git diff --check`，均通过。
- **限制：** 以上是数据库路由、Go executor 和 Node Runtime 的分层覆盖，不是一次从真实 PC/iOS 经服务端、Provider 到实际 Task 完成的端到端验收；未覆盖真实模型下定向追问、独立 Task 并行、用户停止、跨端接续或 60 分钟耐久。因此 `MH-MSG-001` 与 `MH-DUR-001` 的整体验收状态不变。

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
