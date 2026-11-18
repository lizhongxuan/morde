# Morde 模型上下文容量自动识别与压缩优化设计方案

日期：2026-11-18。本文记录基于 Morde 当前工作区源码及 Hermes-agent、OpenClaw 上游代码调研形成的设计决策。实现和验收状态见[实施 TODO](./实施/Morde模型上下文自动识别与压缩增强实施任务todo清单文档.md)。

本文补充 [Morde AI 对话上下文压缩与长期记忆设计方案](./Morde%20AI对话上下文压缩与长期记忆设计方案.md)：本文聚焦模型上下文容量如何自动解析，以及怎样利用 DSH 做更稳健的压缩；原方案仍作为会话隔离、摘要来源、记忆授权、检查点和恢复的数据安全依据。DSH 仍是 Morde 唯一 Agent Runtime。

## 1. 结论

Morde 系统默认提供的模型由 Morde 团队查阅官方资料并维护上下文容量目录，用户添加这些模型时无需也不能填写容量。目录按精确 `base_url + model_id` 匹配，连接详情显示容量和来源；同一模型 ID 经不同 endpoint、代理或部署时不能跨路由套用容量。

OpenAI-compatible 自定义端点没有统一的上下文容量发现协议。本轮不探测 endpoint，也不从模型名、最大输出 token 或请求用量推断硬上限；不在目录中的自定义路由标为“容量未知”，仅在高级设置允许用户提供可选覆盖值。不能默认为 200K 或 256K 并把它显示成已验证容量。

Morde 已接入 DSH 压缩服务和会话摘要持久化，应继续复用 DSH `TokenMeter`、`BasicCompactionEngine` 和官方工具结果裁剪插件。只有容量已知时才启用长任务自动压缩；明确的上下文溢出由 DSH 现有恢复路径最多重试一次。未知容量的连接仍可用于普通短对话，但不自动压缩。

## 2. 开始改造前的 Morde 基线

| 位置 | 当前行为 | 影响 |
| --- | --- | --- |
| [个人模型设置](../web/portal/src/routes/Connections.tsx) 和管理员模型设置 | 提供可选的 `上下文窗口（tokens）` 输入框；留空时界面显示“未配置，不启用自动压缩”。 | 用户需要自行查文档并填写；漏填时无法自动摘要。截图展示的就是该状态。 |
| [模型连接 API 与存储](../server/internal/app/model_connections.go)、[迁移 00177](../server/migrations/00177_model_context_window.sql) | 连接记录只有一个可空的 `context_window_tokens`。更换服务地址、模型名或密钥时会清除已有值。 | 有基础的失效保护，但没有容量来源、验证时间或自动发现结果。 |
| [摘要模型路由](../server/internal/app/model_gateway_route.go) | `ResolveConversationSummaryModel` 在容量缺失或越界时拒绝摘要调用。 | 未配置的模型无法走 DSH 摘要流程。 |
| [会话摘要维护](../server/internal/worker/conversation_summary_maintenance.go) | 按窗口 token 预算选择完整来源组，分页和摘要输入仍受字节上限保护；摘要提交带租约、摘要版本和 CAS 校验。 | token 压力来自 DSH 公开启发式估算接口，传输字节和 token 预算分开判断。 |
| [DSH Runtime](../runtime/morde-cloud-runtime/src/engine.mjs) | 已挂载 `BasicCompactionEngine`；Morde 摘要任务显式调用 `compactNow()`，压缩配置按窗口设置 `thresholdRatio: 0.8`、近期保留、摘要输出上限和余量。常规 Agent Session 的 `auto` 仍为 `false`。 | 对话摘要已有 DSH 入口，但尚未让长任务 Session 自动使用它。 |
| [DSH 压缩编排](../server/internal/worker/cloud_dsh_compaction.go) | 摘要调用无工具，模型、窗口、来源及摘要版本均进入操作输入摘要；checkpoint、取消和模型网关走现有隔离与账本边界。 | 这条安全边界应保留，不另造摘要 Runtime。 |

更完整的摘要权限与恢复基线见[原上下文压缩设计方案](./Morde%20AI对话上下文压缩与长期记忆设计方案.md)。

## 3. 上游实现对比

本次源码核对以 2026-11-18 的上游提交为基线：Hermes-agent `f7e2898aaefc8f967dbf7345e3999a65672fe60e`，OpenClaw `34d6c288fe1c46a369ff4171a103226b8c6059e3`。上游会持续变化，实施时应再核对固定提交。

### 3.1 模型容量解析

| 项目 | 做法 | 对 Morde 的启示 |
| --- | --- | --- |
| Hermes-agent | `get_model_context_length()` 汇总显式覆盖、按模型和 endpoint 隔离的持久缓存、服务商或端点元数据、provider-aware 模型目录、Ollama/本地探测、静态模型目录及最后兜底。缓存记录会区分来源；服务商错误若明确报告更小上限，可用来修正当前路由。完全无法识别时 Hermes 有 256K 兜底并发出警告。 | 学习“多来源解析、按端点缓存、保留来源、服务商报错校正”。不照搬 256K 兜底，因为代理模型和小窗口模型可能远小于此值。 |
| OpenClaw | 运行时使用 provider/model catalog 和逐模型元数据，支持显式上下文上限覆盖；把模型原生 `contextWindow` 与路由实际可用 `contextTokens` 分开，路由可施加更小上限。自定义 provider 文档指出，缺少发现和声明时预算调用会使用通用 200K fallback。缓存和报表尽量关联实际选择的路由。 | 学习区分模型规格与当前路由上限，以及让元数据与 provider 绑定。不照搬对未知路由的 200K fallback；相同 slug 不能跨 endpoint 借值。 |

Hermes 的上下文解析入口及压缩实现见 [`model_metadata.py`](https://github.com/NousResearch/hermes-agent/blob/f7e2898aaefc8f967dbf7345e3999a65672fe60e/agent/model_metadata.py)、[`context_compressor.py`](https://github.com/NousResearch/hermes-agent/blob/f7e2898aaefc8f967dbf7345e3999a65672fe60e/agent/context_compressor.py) 和[上下文压缩说明](https://github.com/NousResearch/hermes-agent/blob/f7e2898aaefc8f967dbf7345e3999a65672fe60e/website/docs/developer-guide/context-compression-and-caching.md)。OpenClaw 的对应实现见 [`context.ts`](https://github.com/openclaw/openclaw/blob/34d6c288fe1c46a369ff4171a103226b8c6059e3/src/agents/context.ts)、[自定义 Provider 文档](https://github.com/openclaw/openclaw/blob/34d6c288fe1c46a369ff4171a103226b8c6059e3/docs/concepts/model-providers/custom-providers.md) 和[上下文说明](https://github.com/openclaw/openclaw/blob/34d6c288fe1c46a369ff4171a103226b8c6059e3/docs/concepts/context.md)。

### 3.2 上下文压缩

| 项目 | 做法 | 可借鉴点 |
| --- | --- | --- |
| Hermes-agent | 主要压缩器按窗口阈值批量压缩：先清理受保护尾部之外的旧工具输出，再保护系统开头、近期原文和完整工具轮次，用结构化摘要替换中段。摘要继续合并前一版，保留任务、约束、进展、标识符和待办。另有 micro-compaction：每个正常完成的 Agent turn 摘要一个最早未压缩轮次，用户原文始终保留；数据库归档与新压缩记录原子提交。 | 工具输出占用常常大于对话文字；保护最近内容和工具轮次；更新单一摘要而非叠放摘要；摘要失败时原输入仍有效。micro-compaction 可降低峰值，但会更早把历史变成二手信息，不应作为 Morde 首期默认策略。 |
| OpenClaw | 默认在推算压力接近阈值时主动压缩，收到可识别的 provider context-overflow 错误时也可压缩后恢复。压缩记录持久化，后续请求看摘要加边界后的近期消息；工具调用与结果不能被切开。摘要输入有界，超长历史用采样和缺口标记；safeguard 路径还能核查摘要结构、待处理请求和精确标识符。 | 采用“主动维护 + 明确溢出恢复”两条入口、保留原始 transcript、限制摘要输入，且只在工具已完成/可安全接续时恢复。 |
| Morde / DSH | Morde 已复用 `@deepseek-ai/dsh-compaction-basic`。会话摘要由独立、无工具 DSH 上下文生成，保存摘要和来源证明。worker 通过 Morde coordinator 启动只测量的 DSH 子进程，调用公开 `dsh-token-meter/estimate`，不发模型请求也不创建检查点。 | 保留 DSH 的压缩、检查点和事务语义；Morde 只适配摘要来源、权限与压力门槛。 |

Hermes 的 micro-compaction 设计见[上游说明](https://github.com/NousResearch/hermes-agent/blob/f7e2898aaefc8f967dbf7345e3999a65672fe60e/website/docs/developer-guide/micro-compaction.md)。OpenClaw 的[压缩源码](https://github.com/openclaw/openclaw/blob/34d6c288fe1c46a369ff4171a103226b8c6059e3/src/agents/compaction.ts)及[压缩行为说明](https://github.com/openclaw/openclaw/blob/34d6c288fe1c46a369ff4171a103226b8c6059e3/docs/reference/session-management-compaction/compaction.md)描述了摘要输入上限、工具配对、主动阈值和溢出恢复。

## 4. Morde 目标设计

### 4.1 容量字段语义

`context_window_tokens` 表示**该模型连接当前实际可用的输入上下文上限**，而不是最大输出 token 数，也不是 Morde 内部按字符数测出的历史大小。如果服务商同时给出模型原生窗口和该 deployment/proxy 的更小上限，写入两者中的较小值。相同模型 ID 的不同服务商、endpoint、部署或路由模式要分别解析。

DSH Task Session 的压力判断使用 DSH `TokenMeter`；provider usage 可用时应优先用于校准。会话摘要通过 DSH 公开的纯 `estimateMessage` 启发式估算与实际来源消息一致的固定提示、来源头、旧摘要和正文，不创建模型请求。由于启发式不是 provider tokenizer，Morde 当前将估算至少乘二，并对中日韩字符按每字符至少一个 token 计入。2026-11-18 使用 DeepSeek `deepseek-flash` 的 `usage.prompt_tokens` 做了四类合成消息及完整摘要提示形状校准，当前保护值均覆盖实际输入；这只验证 DeepSeek 当前路由和这些样本，其他 provider、代理及长会话仍须端到端观察。存储分页固定按 64 KiB 字节上限，序列化后的完整摘要输入固定不超过 96 KiB；字节上限只用于传输安全和独立的传输压力触发，不与 token 窗口比较。

| 校准输入 | 字符数 | DSH 原始估算 | DeepSeek `prompt_tokens` | 当前保守值 |
| --- | ---: | ---: | ---: | ---: |
| English | 11,999 | 3,008 | 2,004 | 6,016 |
| CJK | 5,879 | 1,478 | 3,364 | 5,258 |
| JSON/schema | 10,542 | 2,644 | 2,669 | 5,288 |
| 工具结果 JSON | 12,709 | 3,186 | 2,901 | 6,372 |
| 完整 DSH 摘要提示（含 system、来源头、混合中英/JSON 工具结果、end marker、摘要 prompt） | 来源 3,175 | 1,692 | 1,951 | 3,440 |

DeepSeek 的 `prompt_tokens` 是输入计数，[官方 Chat Completions 用量字段](https://api-docs.deepseek.com/api/create-chat-completion/)为校准依据。四类单消息中 CJK 的 provider/raw 比值最高为 2.276；当前中日韩字符下限与 2 倍保护均覆盖本次样本，暂不下调系数。

### 4.2 系统模型目录与自定义路由

系统默认模型容量使用 Morde 随版本维护的官方资料目录。当前目录在 2026-11-18 核对，值对应 API 路由上下文窗口，不使用 Codex/ChatGPT 产品额度或最大输出 token：

| 默认模型 ID | Morde 默认 endpoint | 上下文窗口 | 官方来源 |
| --- | --- | ---: | --- |
| `gpt-6-luna` | `https://api.openai.com/v1` | 1,050,000 | [OpenAI API model card](https://developers.openai.com/api/docs/models/gpt-6-luna) |
| `gpt-6.1-sol` | `https://api.openai.com/v1` | 1,050,000 | [OpenAI API model card](https://developers.openai.com/api/docs/models/gpt-6.1-sol) |
| `gpt-6-astra` | `https://api.openai.com/v1` | 1,050,000 | [OpenAI API model card](https://developers.openai.com/api/docs/models/gpt-6-astra) |
| `deepseek-flash` | `https://api.deepseek.com` | 1,048,576 | [DeepSeek Models API](https://api-docs.deepseek.com/api/list-models/) |
| `glm-5.1` | `https://api.z.ai/api/paas/v4` | 200,000 | [Z.AI GLM-5.1 model card](https://docs.z.ai/guides/llm/glm-5.1) |

系统预设只按精确 endpoint 和 model ID 命中目录。新建或编辑预设时忽略并拒绝手工覆盖；已有记录中的旧手填值应迁成目录值。新增系统预设必须一并提交容量和官方来源，目录覆盖率检查必须为 100%。

不在目录内的自定义 endpoint 保持 `null` / `unknown`，不探测模型、不用最大输出 token 推窗口，也不把模型名相同的官方值跨 endpoint 复用。用户如确知自建代理或部署的容量，可在高级设置填写可选覆盖；该值标记为 `override` 或保留的 `legacy_manual`，不是自动验证结果。当前 `provider_metadata`、`provider_error` 来源值仅为存储/API 可识别状态，不由本轮容量解析器生成。

容量和来源在同一连接记录中保存。目录来源同时记录 `context_window_resolved_at` 和目录版本；route 或凭据变化时重新执行目录精确匹配，未知自定义 route 继续为 `unknown`，不发起探测请求。自定义覆盖只绑定该连接，不能传播到其他同名模型。

### 4.3 配置与用户界面

- 系统预设从 Morde 目录立即返回容量和来源，不额外调用供应商，不产生探测用量；常规新增/编辑页面不出现上下文窗口输入框。
- 普通用户连接卡片显示目录容量和来源。未知自定义 endpoint 显示“容量未知；自动压缩未启用”，高级设置可选填写覆盖值并显示其来源。
- 后台管理员使用相同目录和规则。用户模型凭据及连接记录继续按现有边界隔离。

### 4.4 自动压缩和恢复

**对话维护。** 用实际窗口 `W`、摘要输出预留 `O` 和适配器余量 `H` 计算预算 `W − O − H`；当累计估算达到预算的 75% 时触发。Morde 使用 DSH 公开 `estimateMessage` 对实际摘要消息逐项计量，以 2 倍估算和中日韩字符下限留出安全余量。传输字节另按 96 KiB 上限及其 75% 水位触发增量摘要；不把 UTF-8 字节换算或比较成 token。未知窗口不生成 token 预算。

当估算压力接近触发点时，在提交当前消息或后续请求前，压缩旧的、已授权历史；本次用户请求和近期尾部原文保留。摘要采用单一滚动版本，明确区分目标、用户约束、已确认决策、已验证结果、未完成事项、待用户确认内容和可核查的来源 ID。操作事实仍须从任务状态、模型账本、工具回执或原始消息读取，不能以摘要证明某操作已完成。

**工具结果裁剪。** 使用与锁定 DSH 版本兼容的 `dsh-compaction-tool-result-pruner` 处理历史大工具输出，并核对其 `compaction/prune` 与紧随其后的投影替换事件。Morde 只有在原始结果和对应调用仍能通过授权检查找回时，才接受替换投影；服务端工具回执、任务产物和审计记录不删除。裁剪由 DSH 在下一次模型请求前按压力触发，不手工重写 DSH 的裁剪算法。

**溢出恢复。** 只把 provider 明确表示上下文超限的响应归一成 DSH 支持的 `CONTEXT_WINDOW_EXCEEDED` 错误类别；通用 413、限流、网络失败不推断容量。当前不从错误正文中更新容量数字。由 DSH 现有恢复路径执行一次有界压缩和至多一次重试；task 恢复还必须通过 Morde checkpoint、授权与副作用状态检查。重试仍失败时保留原文和账本并返回实际失败原因。

**失败保护。** 摘要为空、截断、输出超预算、未缩小输入、摘要模型失败、容量未知、授权变化或 CAS 冲突时，旧摘要与原始来源继续有效。摘要模型无工具，走现有 Morde 网关、预算、账本、取消和操作 ID。一次溢出恢复后仍超限时，返回清楚的状态，提示缩短当前输入或改用已验证的大窗口模型。

### 4.5 暂不采用的上游功能

首期不引入每轮 micro-compaction、不同的上下文引擎插件框架、外部记忆刷新 Agent、OpenClaw 的 200K fallback 或 Hermes 的 256K fallback。它们可能有用，但不是解决截图中手工配置容量的最低闭环；压缩频率或摘要质量有真实数据支持后再评估。

## 5. 落地顺序

1. **容量目录与连接契约**：为系统预设维护官方容量、精确 endpoint、model ID、来源和目录版本；迁移已知预设旧手填值。更新 OpenAPI 和 PC/管理界面，系统预设不显示输入项，自定义未知路由才可选覆盖。
2. **摘要与任务压缩**：将有效窗口传入现有摘要 worker 和 DSH task runtime。Task Session 由 DSH `TokenMeter` 判断压力；有窗口时以阈值 `0.8`、headroom/近期保留量 `min(4096, 12.5% × window)` 和有界摘要输出启用自动压缩，未知窗口关闭自动压缩。会话摘要复用 DSH 公开消息估算接口，并对估算加安全余量；摘要传输继续有独立 byte/page cap。DeepSeek 合成输入 usage 样本校准已完成；其他 provider 路由及长会话实际偏差列入端到端验收。
3. **溢出恢复**：把明确的上下文超限映射到 DSH 支持的类别，由 DSH 现有路径最多压缩重试一次；不从普通 413 猜窗口，不对副作用状态未知的工具调用重放。
4. **工具结果裁剪**：使用与当前锁定 DSH 版本兼容的官方 `dsh-compaction-tool-result-pruner`，按 DSH 插件顺序挂载；Morde checkpoint 只接受有相邻原始结果证明的投影替换，服务端原始结果和审计记录不删除。

以上为实施顺序，不是时间承诺。数据库迁移保留未知自定义连接原有手填值并标注来源；命中系统预设目录的旧手填值升级为官方目录值。

## 6. 验收条件

1. 五个系统默认模型按官方来源自动显示上下文窗口，添加和编辑时不要求也不接受用户输入；精确 endpoint 变化后不得复用另一条路由的值。
2. 自定义未知路由可保存并用于普通对话；高级覆盖可选，未填写时显示容量未知且关闭自动压缩。
3. 已知容量的长任务由 DSH 在阈值处压缩；工具调用/结果配对、近期上下文、原始日志和 Morde 会话摘要来源完整。
4. 只有明确上下文超限进入一次 DSH 恢复；普通 413、限流、流中断或工具结果未结算时不推断窗口、不重复副作用。
5. 通过真实 `morde-pc` 入口验收连接设置，并核实管理员 Web 流程、模型用量和跨用户/会话隔离。未执行的真实 provider、PC 客户端或恢复场景继续标为待验收；单元测试、构建或插件挂载不代替端到端验收。

## 7. 范围外

本文不设计长期记忆、历史搜索、第二 Agent Runtime 或普通用户 Web 产品端；这些继续遵循现有 Morde 需求、DSH 接入规则和上下文压缩设计。不会将对话摘要取代原始聊天或任务回执。
