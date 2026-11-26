# 会话 E-01：PC 对话、审批与任务动态

**目标：**完成 R04/R06/R22/R23/R24 的 PC 呈现与交互；与其他 `01` 同时启动。先读[并发总览](<00-并发实施总览.md>)、[体验目标](<../Morde AI对话目标体验.md>)及[原方案](<../Morde 复刻 Muse 一体化实施与验收 TODO.md>)的 PC 补充和 T13–T15、T19–T21、T51–T54、T57。

**写入范围：**`web/portal/src/` 中 PC 内嵌界面 / 测试及 `clients/desktop/harness/desktop/` 由 E 负责；实际 `morde-pc` 默认由 E 统筹。用户明确要求其他会话操作时，按总览协调不重叠时段和独立 profile。后端数据、OpenAPI / MCP 资源所需改动交 A，终态 / 验证事实交 D；不创建独立普通用户 Web 页面。

**设计与交接：**按[审批卡与工作项](<01-复杂链路设计与交接.md#approval-activity>)处理双入口的一致身份、断线待确认及通用资源沙箱；终态事实见[交付设计](<01-复杂链路设计与交接.md#terminal-validation>)。

- [ ] **E01-1 对话与任务控制（PC 呈现完成；语义路由集成待验，失败消息状态/安全重试本地实现及定向测试完成）。**复用现有 `Assistant.tsx`、会话和澄清接口：普通问题自然回复，明确任务继续 / 停止经 B 的语义路由；失败 / 等待 / 合并依据实际事实呈现，不恢复机械中断状态行。本轮在实际 PC 验证了搜索请求、任务结果、等待授权和批准后完成；Assistant 定向测试覆盖失败、未知、等待、任务隔离、合并结果和停止意图呈现。T45–T48/T51 的真实语义路由仍须结合 B 的后端和真实模型做集成验收。
  - [x] C-01 本地实现与定向测试：在原对话 Morde 行说明已保存但路由失败的原因及下一步；仅对发送账号匹配、未关联 Task / 重复消息且非超长的原消息提供 `retry-processing`，复用原服务端消息 ID 和持久幂等键。`Assistant.test.tsx` 256 项通过，`npm run typecheck` 通过。
  - [ ] C-01 实际 PC + 可用服务回归：当前本机没有可用 Portal / Guardian，按本地修改约束未连接测试机或发送重试请求；源码 `make pc` 构建并启动过隔离 profile `/tmp/morde-e01-routing-retry-20261125`，但本轮无法读取该 PC 窗口，也没有 durable route-failure 样本。真实 PC 状态行与 retry POST 仍待验；不得重放结果未知的旧请求。
  - 验收：T45–T48、T51 中目标明确 / 歧义 / 合并 / 停止不串任务；思考状态、消息气泡与键盘交互符合体验目标。后端或真实模型未就绪时按层记录待验。
- [ ] **E01-2 双入口 MCP Apps 审批（本地呈现已验证；完整场景待验）。**复用 `TaskApprovalMcpApp.tsx`、`TaskApprovalCard.tsx` 和 `Reviews.tsx`；原对话与审核区共享后端审批 ID / revision，任务动态无审批 / 停止 / 任务验证按钮；后端通用资源能力需要时交 A。本轮通过本地合成审批检查对话和 Reviews 待处理项、在对话允许后同一任务从等待点继续并完成；真实 DSH、撤权 / 过期 / 断线和跨端恢复另行记录。
  - 验收：T19–T21、T52–T53 的允许 / 拒绝、过期、撤权、重复提交、刷新 / 重连及沙箱隔离；断线时先核对服务端决定，通用资源加载失败不自动批准。批准不等于执行成功。实际 PC 与真实 DSH 分开记录。
- [ ] **E01-3 精简、真实的工作项（本地呈现已验证；真实详情待验）。**显示搜索词、来源、浏览对象、文件 / 命令、目录、退出码、耗时、输出与状态的适用项；低价值调度噪音不占主要视图。只展示后端已授权事实，未知和截断明确标记。本轮检查了工作项摘要和详情呈现，UI 测试覆盖已有字段、失败状态、未知和截断；本地搜索 fixture 没有提供搜索词或来源，已在交接记录列给 A，未在界面推断。
  - 验收：T13–T15/T54 展开内容与实际账本一致，失败项保留，未知不展示未确认输出；缺字段时列给 A，不自行编造。
- [x] **E01-4 实际客户端回归（PC + 本地 fixture 层）。**复用组件测试并运行 typecheck；启动源码 `make pc`，在隔离 profile 操作受影响的界面。本轮已在实际 PC 操作对话、任务动态、MCP App 审批及 Reviews；范围与限制见下方交接记录。
  - 验收：实际 PC 对话、审批和任务动态有操作证据；不能用独立 Portal 浏览器 / iOS 代测。Playwright Electron 和 Trace 仅在能控制同一真实客户端时作为辅助，失败记录可行性，不代替真实 DSH 或账本证据。
- [ ] **E01-5 可读流程图交付（本地图形呈现已验证；R23-A / T57 待验）。**核对 `ChatMarkdownContent.tsx` 和既有产物查看能力，为 AI 输出的流程图接入可读图形：在对话直接显示，或在 Morde PC 客户端内打开。图形渲染失败时说明并保留可复制源码；纯代码块不算图形验收通过。E 独占客户端实现，若需要后端产物接口由 A 协调。本轮接入 Mermaid 流程图渲染，实际 PC 显示了本地合成消息中的图形和展开源码；真实模型生成与 T57 源码分析仍由 E02/A02 验收。
  - 验收：实际 `morde-pc` 可读图、结论及来源；安全处理不可信图表内容和非法语法，失败不影响其他消息，不放宽现有 CSP。组件测试仅作辅助，真实代码分析由 E02/A02 按 T57 验收。

**依赖 / 状态：**现有界面与 fixture 可立即核对；通用 MCP 资源、R20 终态和新增详情字段分别依赖 A/D 的合同。视觉权威规范缺失时沿用现有样式并单列待验。

### 2026-11-26 本轮收口进度

- 使用源码 `make pc` 和独立 profile `/tmp/morde-e01-closeout-20261126`，连接只监听回环地址的合成服务 `127.0.0.1:61017` 与独立 PostgreSQL `127.0.0.1:55470`。实际 PC 发送“跨端活动验收：等待用户授权”后显示 `Morde 的审批请求` / `MCP App 操作审批`；审核页显示同一条待处理卡及本地合成资源的影响说明。没有重放旧 Task，也没有连接真实 DSH、云端 Host 或真实模型。截图中的长源码分析提示本轮未送入真实模型；合成服务不能验证该分析结果或 Mermaid 输出。
- PC 与 A 联合只读核账确认 Task `qxmrx760`（conversation `cuiol6d4`、user message `mw7s0vnb`）为 `waiting_user / approval_required`，approval `ru0d71lk` v1 为 `pending`；用户消息 delivery=`applied`。账本有 2 条 status、2 条 step、1 条 waiting_user 事件，`task_operations` 和 `model_call_ledger` 均为 0。没有批准/拒绝决定或工具副作用。
- 未能从本轮可用 PC 无障碍树访问嵌入 MCP App 的允许 / 拒绝控件；CUA 也不能按 profile 精确区分多个 Electron 实例。为避免误操作其他会话，没有通过直接 API 代替 UI 决定。`approval-resume` 仍在 `awaiting_message`，没有结果；Go fixture 测试在 642 秒后由我主动 Ctrl-C（退出码 1），所以本轮审批完成与完整链路未通过。
- 本地回归：`npx vitest run --maxWorkers=1 src/routes/Assistant.test.tsx src/routes/Reviews.test.tsx src/components/TaskApprovalMcpApp.integration.test.ts src/components/TaskApprovalCard.test.tsx src/components/ChatMermaid.test.tsx src/components/ChatMermaid.integration.test.tsx`（6 个文件、294 项通过）；`npm run typecheck` 通过。独立 PC、Go fixture 和 PG 容器已停止；确认本轮 PC PID、61017 listener 和容器均已释放。
- **Step 6 补充复验：**`web/portal/` 中 Assistant、Reviews、Workspaces、TaskApprovalMcpApp、TaskApprovalCard、ChatMermaid 共 7 个定向测试文件 358/358 通过，`npm run typecheck` 通过。Assistant JSDOM 测试补齐浏览器 `scrollIntoView` mock；生产 UI 未改。以 `MORDE_TEST_USER_DATA=/tmp/morde-e01-fullchain-20261126 make pc` 完成本地源码 Portal、Workbench、同步 helper、桌面 TypeScript 构建并启动 Electron。随后本地 CUA 的 `windows/observe/apps/status` 调用均超过 60 秒无返回；新进程没有可安全绑定的窗口快照，未执行任何 UI 输入。仅手动 Ctrl-C 停止本轮自己启动的 profile（命令退出码 130）；没有连接 fixture 服务或测试站、发送消息、批准/拒绝、写入数据库或触碰其他 profile。此次只证明源码构建/启动，不构成新的 PC 交互或完整链路验收。
- E01-1/2/3/5 保持未勾选。E01-4 的 `[x]` 仍只代表此前已完成的本地 fixture PC 交互；本轮新增证据仅到真实 PC 可见的待审批状态和 A 的只读账本核对，不代表允许 / 拒绝、真实 DSH、撤权 / 过期 / 重连或跨端恢复通过。

**定向回归：**在 `web/portal/` 运行 Assistant、Reviews、TaskApprovalMcpApp、TaskApprovalCard 和 ChatMermaid 定向测试及 `npm run typecheck`；另在仓库根目录启动 `make pc` 实测，组件回归不能替代实际 PC。

## 交接记录

### 2026-11-25 本地 PC 与组件验证

- 使用源码 `make pc` 启动，独立 profile 为 `/tmp/morde-e01-pc-mermaid-20261125`。在实际 `morde-pc` 内登录仅监听回环地址的合成验收服务；未操作普通用户 Portal 浏览器或 iOS 客户端。
- 在 PC 主会话发送固定索引搜索请求。服务端 fixture 的 `/__morde_fixture/verify/search` 返回 `200 verified`；PC 显示合成搜索答复和“已完成”任务动态。打开“正在检索固定的本地索引”工作项，只看到“固定索引查询完成，”这一条已记录摘要。
- 发送授权请求。fixture 的 `/__morde_fixture/verify/approval` 返回 `200 verified`，任务保持 `waiting_user`，待处理请求保持 `pending`。实际 PC 对话卡和 Reviews 页均显示同一合成审批标题、影响和决定按钮。随后在另一项合成任务的对话卡点击“允许本次操作”；PC 显示从等待点继续并完成。隔离数据库记录该任务为 `completed`，同一审批记录为 `approved / approve / version 1`；结果明确显示没有执行回执，批准不代表操作成功。
- 为验收 PC 渲染路径，在隔离的本地验收数据库中把一条合成 assistant 消息设为 Mermaid 流程图内容，再由 PC 刷新读取。实际客户端显示了四节点 SVG 流程图，展开后可读原始 Mermaid 源码。此操作只验证呈现，不代表真实模型生成结果。
- 本地验证通过：`npm test -- --run src/routes/Assistant.test.tsx src/routes/Reviews.test.tsx src/components/TaskApprovalMcpApp.integration.test.ts src/components/ChatMermaid.test.tsx`（4 个文件、254 项）；Mermaid 定向测试（组件与真实渲染器集成，2 个文件、4 项）；`npm run typecheck`；`git diff --check`。`make pc` 的 Portal、Workbench、桌面 TypeScript 构建完成并打开了客户端；验收结束后手动停止客户端。
- 活动 fixture 的 search 和待审批核验均返回通过。批准后继续场景的 PC 界面和隔离数据库显示为已批准并完成，但 fixture 的完整跨端验证返回 `409`，原因是没有 iOS Task snapshot；因此不记录为跨端验收通过。未使用真实 DSH、真实 MCP connector 或用户提供的外部 API 密钥。
- 交 A 补充 / 评估任务详情账本字段：搜索词、来源 URL / 标题 / 摘录；浏览器目标；文件路径；命令、工作目录、退出码、耗时和输出。当前详情组件会按已授权字段显示这些内容，但本轮 fixture 没提供搜索词和来源，界面只显示已记录摘要。

### 2026-11-25 E01 复核与成本显示修正

- 只读复核确认 `AuthProvider` 在登录 / 退出时会取消并清除账号级查询缓存；对话 Task 查询键本身未包含账号 ID，但账号切换会先清掉非认证缓存，未发现当前实现存在跨账号缓存残留。
- 修复审批费用披露的边界：服务端若返回未知 `basis`，客户端不再把它显示为“已结算”，改为“费用数据不完整”；新增 TaskApprovalCard 回归测试。
- 最新定向回归：`npx vitest run --maxWorkers=1 src/routes/Assistant.test.tsx src/routes/Reviews.test.tsx src/components/TaskApprovalMcpApp.integration.test.ts src/components/TaskApprovalCard.test.tsx src/components/ChatMermaid.test.tsx src/components/ChatMermaid.integration.test.tsx`，6 个文件、263 项通过；`npm run typecheck`、`make pc` 的 Portal / Workbench / Desktop 构建与 `git diff --check` 通过。Vite 构建有既有大 chunk 提示，不影响构建完成。
- 更新后源码 `make pc` 在隔离 profile `/tmp/morde-e01-recheck-20261125` 启动到登录页，随后退出；未连接远端账号或新增云端电脑，以免影响并发中的 E02 默认电脑验收。对话、任务动态、审批卡、Reviews 和流程图的 PC 交互证据仍以本节前一条本地 fixture 实测为准，本次没有重做这些业务操作。
- E01-1 至 E01-5 的本地客户端实现和上述 fixture 证据保留；E01-1/2/3/5 因整项验收仍有缺口保持未勾选，E01-4 的勾选仅表示本地 fixture 的实际 PC 交互层完成。真实 DSH / 语义路由、撤权与跨端刷新后的审批恢复、线上 MCP App 页面实际脚本、后端账本字段完整性及真实模型代码分析仍未验收，继续交 A/B/D 与 E02/A02 按对应 T 项记录。批准结果仍不能当作操作执行成功。

### 2026-11-25 C-01 路由失败说明与安全重试

- `Assistant.tsx` 对无 Task、无重复关联的失败用户消息增加独立 Morde 状态行，说明消息已保存但尚未处理、展示后端失败原因及下一步；模型不可用按系统事实说明，`message_too_long` 指引缩短后重发且不显示原消息重试入口。用户气泡没有增加状态脚注。
- 重试仅在原消息发送账号匹配、消息仍为 `failed`、无 `task_id` / `related_task_id` / `duplicate_of_message_id`、非超长且服务声明并复核 `conversation_auto_routing_v1` 时开放；复用原 conversation/message ID 和持久幂等键。运行中重复点击被拦截，身份 / 服务能力变化时 fail-closed，结果不确定时保留同一幂等键，路由离开 `failed` 后清除。
- 本地验证：`npm test -- --run src/routes/Assistant.test.tsx` 为 1 个文件、256 项通过；`npm run typecheck` 通过；`npm run build:pc` 通过。源码 `make pc` 在隔离 profile `/tmp/morde-e01-routing-retry-20261125` 完成构建并启动；验收结束后通过 Ctrl-C 结束该 profile。GUI 截图读取报 Screen Recording permission denied，且本地无可用 Portal / Guardian，因此没有观察到失败状态行，没有发送或重试业务请求。实际 PC 与真实服务交互仍待验；未连接远端、部署、上传文件或修改远端数据库。
- 用户给出的截图对应的 T57 续接场景已经在 E-02 记录：首次“1”触发澄清；再次“1”按已列选项回复原任务仍在运行、Task 总数不变，但 route 的 `reference_task_id` 为空，不能证明权威绑定。该原任务结果仍需只读对账，不重新提交。

E 统一协调实际 PC 操作，供 A-02 收口。下一批：[E-02](<会话E-02-PC同步与体验验收.md>)。

### 2026-11-26 步骤 9：实际 PC 验收边界

- 本轮源码 PC 已构建并到达登录 UI，但登录显示用户名/密码错误；没有新消息、Task、审批决定或文件同步操作。
- E01-4 的 `[x]` 仍只对应此前隔离 fixture 的本地 PC 交互。本轮没有新的业务交互证据；E01-1/2/3/5、T51 全流程及真实 DSH/审批副作用仍未验。
