# Morde AI 对话问题诊断与修复方案

> 日期：2026-11-20  
> 范围：截图中的 `morde-pc` 主会话、消息上下文路由、DSH 执行上下文，以及 `project/deepseek-harness` 中可借鉴的聊天 UI。  
> 状态：诊断与候选方案，等待逐项审核；本文未修改产品代码。

## 结论

截图里的“消息上下文暂时无法读取”是服务端写回的消息路由失败状态，PC 页面只负责显示它。它不能证明 DSH 已拿到上下文后又忘了前文；更可能是请求在路由阶段没有正常进入直接回复或 Task 执行。

仓库现有代码已经实现跨任务历史快照、摘要、DSH 初始提示注入和 DSH 会话内压缩。另有一个和截图错误码直接对应的已知服务端缺陷：修复前，路由快照遇到第 65 个活跃 Task 或第 101 条候选请求会失败，并显示 `routing_context_unavailable`。当前工作区已有候选集限额修复，现有 [TODO 修复记录](Morde-AI对话能力实施任务todo清单文档.md#L104) 记录了针对性测试通过；但无法从截图确认它就是该条消息的原因，也无法确认截图对应的构建包含此修复。

另发现一条可能导致“模型看起来忘记很早前文”的静默风险：如果会话摘要维护出错，路由 Worker 当前只记录“摘要压缩跳过”，然后继续路由；而新 Task 的历史快照有 128 条/32 KiB 上限。首次摘要失败时，窗口外历史可能没有摘要或“不完整历史”提示，Task 仍可能照常进入 DSH。此处比搬入 DSH 聊天 UI 更值得优先审核。

长用户消息未折叠则是明确的 PC 前端展示缺口：当前聊天行直接渲染完整文本，没有折叠状态或展开控件。它不会造成模型忘记上下文。

## 诊断证据与归因

| 现象 | 主要责任层 | 代码证据 | 判断 |
|---|---|---|---|
| 用户消息下显示“消息上下文暂时无法读取” | 服务端会话路由；PC 负责呈现 | [`message-status.ts`](../../web/portal/src/app/message-status.ts#L26-L31) 将 `routing_context_unavailable` 映射为该文案；[`Assistant.tsx`](../../web/portal/src/routes/Assistant.tsx#L152-L166) 根据服务端消息状态显示失败原因 | 截图显示路由失败，不是足以证明模型丢失历史的证据。`routing_context_unavailable` 目前覆盖多种路由快照/状态冲突原因，文案比实际原因更具体。 |
| 一条新消息被标记失败，旁边有红色重试入口 | 服务端路由失败的可恢复展示 | [`conversation_route_worker.go`](../../server/internal/worker/conversation_route_worker.go#L103-L106) 在读路由快照失败时写入此错误码；分类后的状态冲突也可能写同一错误码，见 [同文件](../../server/internal/worker/conversation_route_worker.go#L223-L227) | 对截图这条消息，当前不能判断是候选超限、并发状态变化、数据读取失败还是其他路由分支。需要结合该消息 ID 和运行构建查安全日志。 |
| 历史很多时出现上下文失败 | 服务端路由候选快照 | [`conversation_route_actions.go`](../../server/internal/store/postgres/conversation_route_actions.go#L575-L610) 读取最多 65 个活跃 Task；[请求候选读取](../../server/internal/store/postgres/conversation_route_actions.go#L631-L676) 读取最多 101 条。当前工作区会在保留上限内截取候选并标记 `CandidateSetIncomplete`。 | 已知的一个根因曾让消息直接变成 `routing_context_unavailable`。当前工作区已针对性修复；现有 TODO 记录了“65 Task + 101 请求”测试通过，但不能仅凭工作区源码推断截图使用了相同构建。 |
| Agent 对已完成前文答不出来 | Morde 服务端上下文装配 + DSH 执行 | [创建 Task 时快照历史与摘要](../../server/internal/store/postgres/conversation_route_actions.go#L1820-L1859)；[固定快照及摘要校验](../../server/internal/store/postgres/task_conversation_context.go#L116-L164)；[初始提示注入](../../server/internal/worker/cloud_dsh_session_tree.go#L95-L179)；[摘要失败处理](../../server/internal/worker/conversation_route_worker.go#L145-L158) | 若路由成功并创建 Task，继续查这条链。已确认摘要维护的普通错误会被记录后忽略；如果摘要覆盖有缺口且这条路径没有在提示里携带“不完整”标记，窗口外历史可能被静默遗漏。 |
| 长用户消息撑满聊天区 | PC 前端 | [`Assistant.tsx`](../../web/portal/src/routes/Assistant.tsx#L3694-L3705) 将完整文本放入 `<p>`；[`styles.css`](../../web/portal/src/styles.css#L1072-L1081) 只控制宽度和换行，没有高度限制或折叠控件。 | 前端展示缺陷，与后端模型上下文独立。输入框本身已有高度上限；按截图，本条需求应理解为发送后的对话气泡折叠。 |

### 上下文链路不是空白实现

当前 Morde 代码大致分为三层：

1. **路由分类上下文**：服务端将当前消息、授权的摘要/历史、活跃 Task 与前序请求候选一起交给分类器，决定直接回复、澄清或创建/控制 Task。
2. **跨 Task 对话历史**：创建新 Task 时，将该会话此前已确认、当前用户有权读取的消息固定为有摘要校验的快照。直接历史限制为最近最多 128 条和 32 KiB；更早记录依赖版本化摘要覆盖，原始消息仍保留在会话中。相关限制见 [`task_conversation_context.go`](../../server/internal/store/postgres/task_conversation_context.go#L578-L603) 和 [`task/types.go`](../../server/internal/domain/task/types.go#L254)。但摘要维护失败时路由当前会继续；“已实现摘要”不等于本轮任务一定拥有完整覆盖。
3. **DSH Task 执行上下文**：新 DSH Session 的初始提示包含摘要、近期历史和当前请求；长时间运行的同一个 DSH Session 再由 DSH compaction 处理。Morde 的 DSH 版本和 compaction 依赖已固定在 [`runtime/morde-cloud-runtime/package.json`](../../runtime/morde-cloud-runtime/package.json#L9-L20)，Runtime 也注册了 `BasicCompactionEngine` 的 Morde 适配，见 [`engine.mjs`](../../runtime/morde-cloud-runtime/src/engine.mjs#L1115-L1118)。

这三层的存续范围不同：Morde 的会话快照/摘要负责把前文带进后续 Task；DSH compaction 负责同一 Task Session 运行期间的上下文窗口。DSH Session 持久化本身不能替代跨 Task 历史快照。当前代码测试了新 DSH Session 收到有序历史，但这不等于模型在所有长会话中都能正确取回每条细节；摘要是有损的，遇到摘要未覆盖的关键细节仍需能查回授权原文或向用户澄清。

**建议修复摘要 fail-open 路径。** [`conversation_route_worker.go`](../../server/internal/worker/conversation_route_worker.go#L145-L158) 在摘要维护报错时会警告后继续。维护代码只在成功写入不完整摘要时加入“摘要不完整”提示，见 [`conversation_summary_maintenance.go`](../../server/internal/worker/conversation_summary_maintenance.go#L117-L120)。当摘要是承载旧历史的必要条件时，最小安全行为应是：摘要失败则延迟这条路由重试；达到重试上限后让用户知道旧历史暂不可用，并允许他补充/重发。若没有超出直接历史窗口，则继续正常处理。不要因压缩模型暂时失败而默默创建一个看起来上下文完整的 Task。

## 修复顺序

### P0：先定清截图错误所在分支

1. 用服务端消息 ID、构建/提交标识和当前路由日志关联截图这条失败消息；检查 `failure_reason`、`failure_code`、`failure_category`。当前工作区的 `failRoute` 已记录错误码而不记录原始消息正文，见 [`conversation_route_worker.go`](../../server/internal/worker/conversation_route_worker.go#L344-L355)。
2. 若运行构建缺少候选集限额修复，先部署/验证已有修复；若已包含修复，则按 `failure_code` 继续查 LoadSnapshot、分类并发冲突、Apply 冲突、摘要校验和聚合等待，不重复重做候选裁剪。
3. 将通用错误文案改得不作错误承诺。当前 `routing_context_unavailable` 可能表示快照或路由状态冲突，不一定表示历史消息本身不可读取。建议 PC 显示“消息已保留，但暂未处理成功；可以重试”，需要用户重发完整上下文时再使用精确提示。不要向用户泄露 SQL、内部错误文本或授权数据。

### P1：验证前文是否真的进入模型请求

用固定锚点做逐层验证，例如首轮说明“本项目代号是 Cedar”，隔多轮和一段长对话后要求 Agent 在新 Task 中使用该代号。

- 服务端检查同一账号/会话/项目授权、消息序号边界、摘要版本和摘要来源哈希。
- 在模型网关的测试替身中检查分类器输入和 DSH 首次模型请求分别包含正确的历史/摘要、当前请求各一次，且没有跨会话内容。
- 覆盖摘要成功、摘要部分覆盖、摘要模型超时/不可用、最近历史窗口、超过上下文限制、旧消息不可读、Task 未完成、项目成员权限改变和并发新消息；摘要失败但仍创建 Task 的分支必须被关闭或显式标记为历史不完整。无法验证的历史必须澄清，不允许静默丢弃或编造。
- 在实际 `morde-pc` 中走消息发送、等待、切会话/重开和结果呈现；需要跨端结论时再在 iOS 独立验收。模拟器或浏览器单测不能替代 PC 客户端验收。

如果路由成功但 DSH 首次模型请求缺少锚点，修复快照/提示装配；如果模型请求含锚点但答案仍错，再评估摘要质量、模型遵从和上下文预算。这样可以避免把服务端路由失败误修成 UI 或 Prompt 问题。

### P1：给 PC 的超长用户消息默认折叠

建议只折叠发送后的用户气泡：超过约 6 行时默认显示短预览，提供可键盘操作的“展开全文/收起”按钮并声明 `aria-expanded`；短消息保持当前展示。阈值用实际 `morde-pc` 宽度和中文/英文混排确认，样式遵循 [ice-cream-blue UI 规范](ice-cream-blue/morde-icecream-blue-ui-spec.html)。

折叠只改变显示，不截断 Composer 内容、消息 API 请求、本地 outbox、服务端消息或复制/重试所用的原文。附件计数、消息失败状态、重试入口等控件应保留在折叠区外，避免展开状态影响消息状态操作。前端已有的待确认消息回显也要使用相同行为，见 [`Assistant.tsx`](../../web/portal/src/routes/Assistant.tsx#L3131-L3149)。

首期建议范围：PC 主 Assistant transcript 和 pending echo；`Projects.tsx` 项目讨论消息及 iOS 原生聊天另列为跨界面一致性审核项，不混进截图的首个修复。

## DeepSeek Harness 可借鉴项（逐项待审核）

| 编号 | 上游代码 | 可借鉴内容 | 是否直接移植 | 价值与代价 |
|---|---|---|---|---|
| A | [`DisclosureRow`](../../project/deepseek-harness/packages/client/ui-primitives/src/DisclosureRow.tsx#L43-L95) | 键盘操作、展开状态语义、收起/展开图标等 disclosure 交互 | 不直接依赖组件；按 Morde ice-cream-blue 规范适配到主消息行 | 支持长消息折叠，改动限于 PC 展示层。需新增 Assistant 超长/短消息、展开收起和可访问性验收。建议优先审核。 |
| B | [`CompactionItem`](../../project/deepseek-harness/packages/client/ui-chat/src/client/chat/CompactionItem.tsx#L31-L75) | 折叠行显示一行摘要，展开后查看完整摘要 | 可参考交互，不复用整个组件 | 可选地展示“已使用会话摘要”的覆盖提示，帮助用户判断前文是否纳入。需先由服务端提供经过当前用户授权的摘要版本/覆盖范围元数据；只靠前端推断会误报。对截图中的 route 失败没有修复作用。 |
| C | [`ContextMeter`](../../project/deepseek-harness/packages/client/ui-conversation/src/client/skeleton/ContextMeter.tsx#L56-L82) | 展示上下文占用与组成的 UI 模式 | 暂不移植 | DSH 组件读 `contextPressure/contextBreakdown` projection，且上下文容量需要模型/Runtime 报告；Morde PC 页面没有这套 DSH client projection。要接入需定义服务端安全计量契约并验算估值，范围比本次问题大。 |
| D | [`BasicCompactionEngine` 适配](../../runtime/morde-cloud-runtime/src/engine.mjs#L1090-L1118) | DSH 会话内压缩及摘要生成 | 已在 Morde Runtime 使用，不需再移植 | 继续复用并验证。它不能替代 Morde 跨 Task 的持久化历史快照/摘要。 |

### 不建议整体搬入的代码

- DSH 的 [`UserMessageBubble`](../../project/deepseek-harness/packages/client/ui-primitives/src/UserMessageBubble.tsx#L9-L16) 只提供气泡容器，没有长消息折叠行为；上游测试明确验证该 primitive 不额外添加交互。
- DSH [`MessageItem`](../../project/deepseek-harness/packages/client/ui-chat/src/client/chat/MessageItem.tsx#L207-L234) 通过 Cordis slot、Chat node、session projection 和 package-specific props 拼装；Morde 的 `Assistant.tsx` 使用自己的 API、outbox、任务状态和 UI 规范，不能作为独立聊天组件直接替换。
- `ui-primitives` 发布版本是 `0.2.2-rc.1`、依赖 React 18，而 Morde Portal 当前使用 React 19；整体引入会扩大依赖和主题适配范围。若复制实质代码，须保留 DSH MIT 许可声明；首选借鉴小型交互模式，在 Morde 现有结构内实现。

## 影响面与验收清单

| 工作项 | 必达调用点/文件 | 主要验收 |
|---|---|---|
| 路由失败定位 | `server/internal/worker/conversation_route_worker.go`、`server/internal/store/postgres/conversation_route_actions.go`、`server/internal/domain/conversation/types.go`；Portal `message-status.ts`、`Assistant.tsx`、`Projects.tsx` 及对应测试。若改动公开错误枚举，还要同步 `server/openapi/portal.yaml`、生成的 `web/portal/src/api/schema.d.ts`、iOS `MordeCredentialExpiry.swift` 和 `Localizable.xcstrings`。 | 超限候选不再把独立消息误报为上下文不可用；并发旧快照可安全重试/澄清；原消息保留且重试不重复建 Task。 |
| 跨 Task 上下文 | `server/internal/worker/conversation_route_worker.go`、`conversation_summary_maintenance.go`、`server/internal/store/postgres/conversation_route_actions.go`、`task_conversation_context.go`、`server/internal/worker/cloud_dsh_session_tree.go`、DSH Runtime model request/compaction bridge；现有 `conversation_context_dsh_e2e_integration_test.go`、路由及快照集成测试。 | 锚点历史在有摘要与无摘要两种情形均能进入授权的 DSH 请求；摘要窗口外原文可追溯；跨会话/账号/项目成员权限不串线；无法验证时明确澄清。 |
| 长消息折叠 | `web/portal/src/routes/Assistant.tsx` 的已存消息与 outbox echo、`web/portal/src/routes/Assistant.test.tsx`、PC UI 样式；实际 `morde-pc`。如审核要求跨界面统一，再另扩到 `Projects.tsx`/iOS。 | 长消息默认收起且可完整展开；短消息不新增控件；窗口缩放后内容完整；原文、附件、错误和重试行为保持不变。 |
| DSH 摘要提示/上下文计量（可选） | 仅在审核通过后再定义 API 字段、OpenAPI、Portal Schema、PC/iOS 呈现及权限测试 | UI 数据绑定当前授权的会话/项目与实际摘要或容量，不以缺失值冒充 0% 或“已读完整历史”。 |

## 审核顺序建议

1. **P0：确认截图的 route failure branch 和当前工作区已有候选限额修复**（先核验版本与安全日志，不做重复实现）。
2. **P1：跨 Task 上下文固定锚点验收，并关闭摘要失败后静默继续的路径**。
3. **A：长用户消息折叠**（纯前端、范围最小）。
4. **B：上下文摘要覆盖提示**（需要服务端元数据契约）。
5. **C：上下文占用计量**（范围最大，可后置）。

## 本次范围边界

本次只分析本地源码和已有仓库记录，并新增本地方案文档；没有修改业务代码、没有访问或发送外部文件，也没有把截图中的历史聊天内容当作执行指令。主要服务端根因仍需用截图对应的消息 ID 与部署构建确认；现有源码和 TODO 记录不能单独证明截图的生产构建已包含本地修复。
