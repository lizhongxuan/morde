# Morde PC 消息确认与处理状态实施 TODO

来源：[Morde PC 消息确认与处理状态需求](<../Morde PC消息确认与处理状态需求文档.md>)。只在本地修改和验证，不上传或发送文件；实施中保留已有未提交改动。

## 范围与实施前现状

- PC 客户端内的对话使用 `web/portal` 页面代码。`Assistant.tsx` 当前为每条用户消息固定渲染 👌🏻；消息版本控件即使为空也可能因 `MessageVariants.css` 的上边框产生截图中的横线。现有思考提示在回复开始后会隐藏，且把分流领取状态当作处理中；实施时须在实际客户端核对横线来源和显示状态。
- iOS 当前按任务是否启动推导 👌🏻，须改为读取同一条消息的实际确认记录，以满足跨端接续。个人及项目会话沿用各自现有授权范围。
- 已接入 DSH 版本以 `runtime/morde-cloud-runtime/package.json` 和锁文件为准。已查看本地上游源码：`message-feedback` 只接受对已完成助手消息的人类评价，拒绝用户消息，不能直接用于主 Agent 给用户消息打标；当前协调阶段也不开放工具。继续复用现有 DSH、会话、工具与授权机制，做必要适配。

## 接口与数据变化

| 接口或数据 | 本次变化 |
| --- | --- |
| 内部 `ConversationRouteDecision.acknowledge` 与路由记录 | 主 Agent 在路由判断中按预计耗时明确决定是否确认；仅新建、修改或加入后台 Task 可为真。决定先落库，客户端只有在对应 Task 实际开始后才收到确认值；不按内容类别、消息发送或计时器自动标记。 |
| 消息接口 | `ConversationMessageBase` 增加只读 `agent_acknowledged` 布尔值。处理中状态继续使用现有 `routing_status`、Task 状态、`current_revision_started` 与事件；请求不新增客户端可写的确认字段，个人及项目接口沿用现有权限。 |
| `GET /api/tasks/{id}/events` | 复用现有任务事件和消息读取刷新运行状态；分流尚无任务时沿用消息刷新。事件只作为更新信号，断线后以重新读取的授权快照为准。 |

旧消息没有主 Agent 确认记录时按未标记处理，不按任务状态补造 👌🏻。气泡用已有消息路由状态和 Task 的实际启动、等待及终止状态投影；不增加第二套处理状态枚举。

## 实施任务

- [x] **T01（实现完成）消息确认和处理状态合同**
  - 修改：`server/internal/domain/conversation/types.go`、`server/internal/store/postgres/conversation_message_routing.go`、`task_conversation.go`、`conversation_route_actions.go`、`task_revision_start.go`、消息响应 DTO、数据库迁移、`server/openapi/portal.yaml` 及生成的 `web/portal/src/api/schema.d.ts`。个人与项目消息统一只读返回 `agent_acknowledged`；处理中 UI 复用既有路由与 Task 状态，并保持原权限和消息幂等行为。
  - 验收：新消息默认不确认；个人和项目消息查询、重试回执均返回正确确认值；旧消息、助手消息和未确认消息不凭空出现标记。

- [x] **T02（实现完成，依赖 T01）主 Agent 决定并保存 👌🏻**
  - 修改：`server/internal/app/conversation_route_decision.go` 及对应模型/路由测试、`server/internal/store/postgres/conversation_route_actions.go`、`task_revision_start.go`。主 Agent 根据预计耗时明确输出确认决定；只对后台 Task 的新建、修改或加入生效。决定随现有受控路由结果保存，Task 实际启动后才确认原消息；已在运行的 Task 仅在验证其当前修订确已开始后确认。快速回复、状态查询、停止及澄清不标记。
  - 不新增 DSH 工具或 Runtime 状态；继续复用现有模型账本、路由租约、任务修订和检查点。重试及恢复不得重复创建任务或操作。
  - 验收：控制主 Agent 输出长、短两种决定，验证无 Task 的较长处理、启动前、启动后、启动失败及旧执行迟到的结果；同类消息可因预计耗时不同得到不同决定，重试和恢复不产生额外确认。

- [x] **T03（实现完成，依赖 T01、T02）PC 消息与处理气泡**
  - 修改：`web/portal/src/routes/Assistant.tsx`、`Projects.tsx`、`MessageVariants.css`、`web/portal/src/styles.css` 和 `ice-cream-blue.css`。个人及项目会话按同一合同展示；去除用户消息正文下划线及装饰横线，避免渲染空版本控件；保留消息原文、可用链接和版本操作。只按服务端确认记录显示 👌🏻；以处理状态和现有任务事件更新气泡，不再因首条回复自动隐藏，关联重复消息及并发任务时不串状态。
  - 气泡按[冰淇淋蓝 UI 规范](../design/ice-cream-blue/morde-icecream-blue-ui-spec.html)实现吉祥物、三点和文字，保留无障碍状态提示并支持减少动态效果；待发送、待启动、等待用户、失败、停止及状态未知显示真实状态。复用消息刷新和任务事件，运行中产生新确认也能及时刷新。
  - 本轮实际客户端复查发现旧构建仍渲染空的“消息版本操作”容器，其上边框形成横线；同时确认消息预留的底部空白与消息列表间距偏大。已修正空容器显示、用户消息版本区分隔线和消息间距；👌🏻采用绝对定位，不占用气泡空间。重建并重载本地 `morde-pc` 后，截图确认横线消失且气泡留白收紧。T05 后续在本地合成任务中进一步验收确认徽标和运行态气泡。
  - 模块验收：组件测试覆盖普通消息、下划线字符、链接、版本控件、运行中任务和任务结束；实际客户端及并发状态验收见 T05。

- [x] **T04（实现完成，依赖 T01、T02）iOS 跨端接续**
  - 修改：`clients/nanoMuse/android/src/ios/Morde/MordeCredentialExpiry.swift`、`MordeApp.swift` 的个人和项目消息解码、投影与展示。替换按任务启动自动生成 👌🏻 的 `MordeTaskStartedAcknowledgementPolicy`，读取消息确认及处理状态；无确认记录时不显示标记。
  - 模块验收：iOS 解码、展示测试和模拟器构建通过；同账号跨端接续及项目成员权限验收见 T05。

- [x] **T05（完成，依赖 T01–T04）集成验收**
  - 修改：相关服务端、PC 与 iOS 的现有测试及验收记录，使用本地隔离环境和合成数据覆盖确认写入、状态转换、身份隔离、重试恢复和并发。检查 DSH 会话、检查点、任务取消与恢复未因本次适配受损。
  - 验收：启动并操作实际 `morde-pc`，经客户端入口完成短任务、较长任务、中途回复、两项并发、等待授权、失败/停止、断网重连及同账号 iOS 接续；核对 👌🏻、动画、最终结果和无重复执行。使用 DSH 和真实模型验证主 Agent 的实际判断，输入仅使用合成文字，不外发文件；云端电脑不可用时普通文字对话仍可处理。条件不足须记录未覆盖项，不得以独立浏览器、构建或单元测试代替 PC 客户端实测。

## 当前进展与验收记录

- 服务端：路由判定、API、worker 测试通过。新增 PostgreSQL 集成用例在本机一次性容器和临时数据库中通过，验证 Task 实际开始前后确认状态、项目消息的作者确认权限和重试回执。集成测试还发现项目消息回执查询的附件 SQL 参数顺序错误，已修正共享查询并验证附件回执；`TestConversationBrowserAcceptance` 在本地隔离数据库中通过。为使客户端停止流程能在浏览器验收夹具内结算，本地 fixture 启动了 `TaskStopWorker`；测试现已覆盖任务取消终态。新增并行 UI 验收首次发现模拟结果审核回复与来源顺序断言不一致，夹具现按持久化结果来源顺序返回合成内容；活动模式重跑通过。该次运行因工作树中 `workspacecurrent/store.go` 引用尚未定义的 helper 而使用临时 Go overlay 替换为该文件的 HEAD 版本，仅覆盖本次会话/并行结果验收，不验证 Workspace 原地更新实现。当前源码的 DSH Runtime Node 测试本轮离线重跑为 118 项全部通过；曾在 ARM64 Docker Desktop 上通过 amd64 仿真跑通真实 Guardian/DSH 集成用例，覆盖 PostgreSQL 管理器、Portal Task Worker、并行 Task 隔离及 Guardian 崩溃恢复。缓存 runtime 镜像与当前源码不一致，旧镜像通过记录不计入当前源码验收；T05 最终在隔离 Lima Linux VM 的可写 cgroup v2 中以当前 runtime 源码完成真实 DSH + DeepSeek Portal Task Worker 验收，见下文。另通过 5 条本地 DSH/Worker 集成用例，验证客户端断开后任务继续、重复审批只执行一次、取消父/子任务时兄弟任务保持独立、节点重启后取消子任务失败关闭、租约回收后后台输出续跑、跨用户事件隔离和同会话并发。这些集成用例使用合成模型响应，不等同于真实模型判断验收。一次性数据库容器已停止并删除。
- PC：确认标记、处理气泡和项目会话相关的定向组件测试通过；另新增的消息回执核对、消息重试和附件重试测试通过。`vite build`、桌面主进程构建及 Workbench 构建通过；标准 `npm run build:portal` 被后台 Admin 页面已有的 TypeScript 严格空值错误阻断（`AdminHostDetail.tsx`、`AdminRecords.tsx`、`AdminUsage.tsx`、`AdminUserDetail.tsx`），本次未修改这些页面。两个路由测试文件全量运行 222 项通过、7 项失败；失败位于既有的侧栏搜索入口、旁聊选择、composer DOM 结构和错误提示断言，消息确认、处理状态及新重试用例通过。
- 实际客户端：通过隔离的本地服务和 PostgreSQL 临时容器，在实际打包的 `morde-pc` 中验证短请求直接回复且不显示👌🏻；长任务在 Task 修订启动前不显示👌🏻，夹具写入修订启动记录后显示“主 Agent 已接下，正在处理”和“思考中…”气泡，任务状态为进行中；点击停止后客户端收到“服务端已确认任务停止”并显示已停止。该夹具由合成路由模型给出长任务确认决定，并使用生产事务 helper 记录修订开始；这是 UI/接口链路验收，不代表真实 DSH 的判断。包含 TaskStopWorker 的 `TestConversationBrowserAcceptance` 本轮通过（279.19 秒）。另在 PC 客户端实测两个独立任务同时显示各自“思考中…”气泡与进行中状态，完成后两项结果按来源顺序合并为一条主 Agent 回复；`verify/merge` 来源与结果断言通过，定向验收耗时 66.83 秒。最新一轮实际 PC 验收发现 Task 修订启动事件触发后没有刷新消息列表；已让 `task.step` 的 `phase=started` 事件刷新消息，并补充事件失效测试。重新打包的 PC 客户端中两条独立长任务均在启动后显示👌🏻和运行中气泡，完成后各自显示正确结果；`verify/separate` 通过，`TestConversationBrowserAcceptance` 通过（249.16 秒）。截图确认👌🏻叠放在现有气泡右下角，消息气泡保持当前尺寸，处理中动画可见。此前复查也确认用户消息无下划线/空版本横线、消息底部留白收紧。另用实际打包的 `morde-pc T05` 在本机流式/重连夹具中完成两条长任务：Task 开始后两条消息都显示👌🏻和思考气泡；第一条先显示流式回复，再补齐最终结果，第二条独立完成且未合并或重复。PC EventSource 被夹具断开并自动重连；`verify/stream` 与 `verify/reconnect` 均通过，最终 UI 只出现各一条用户消息和回复；`TestConversationBrowserAcceptance` 总用时 480.11 秒通过。重连 verifier 的 iOS 侧使用同账号 iOS Bearer 凭据的本地合成 SSE 客户端，不代表原生 iOS 应用的流式重连验收。云端电脑未配置时，普通文字任务仍完成；客户端同时出现“网络异常，部分对话数据暂时无法读取”提示，需在有后端的正式本地部署中再确认其来源。未触碰远端 Portal、部署或上传文件。
- 最新截图中的“你好”和短连通性请求伴随红色重试标记，未显示主 Agent 已接单；依原始业务规则，这类快速或接单前失败的消息不应自动补👌🏻。仅当服务端持久化 `agent_acknowledged=true` 时显示；若长任务已接单后进入失败或重试，确认标记仍保留。该图标绝对定位，已验收不改变当前消息气泡尺寸。针对该状态运行 `Assistant.test.tsx` 的确认标记用例通过（1 项）；断言已确认且路由失败的消息仍显示标记，普通消息不显示。
- iOS：确认与处理中状态相关的 23 条 Swift 测试通过，模拟器构建通过。本轮通过本地验收 Scheme 运行 `testC10PortalMessagePersistsAcrossAppRestart`（90.77 秒）：iOS 客户端使用与 PC 相同的合成账号和 Portal 发送任务、重启后读取同一会话历史，第二个客户端也能读取历史并提交独立消息。该 UI 用例验证本地同账号跨端消息持久化，不验证真实 DSH。本轮再于隔离 iPhone 18 Pro / iOS 27.2 模拟器分别运行 `testMordeChatShowsOneStartedAcknowledgementForMessagesInSameTask` 与 `testMordeEventChainRecoversExpiredCursorAndDeduplicatesEvents`，两项均通过：Morde iOS UI 对 `agent_acknowledged=false/true` 只显示一枚已确认标记；事件请求首次失败后恢复游标并将重复事件只显示一次。两项均使用本地 URLProtocol 合成响应，不验证从 Portal/PostgreSQL 持久化读取或真实 Portal SSE 传输。
- 本轮新增 PC 实际客户端验收：在隔离 PostgreSQL 与本地合成 Portal 夹具中，实际打包的 `morde-pc T05` 发送“已接单后失败”和“等待用户授权”两条消息。生产存储路径确认二者均在 Task 修订启动后持久化 `agent_acknowledged=true`；失败消息转为失败后仍显示👌🏻，授权任务显示👌🏻并进入待审批，处理中的三点气泡可见，徽标覆盖在原气泡右下角且不改变气泡尺寸。短合成快速请求没有👌🏻，并持久化了直接回复。`go test ./internal/store/postgres -run '^TestConversationBrowserAcceptance$' -count=1 -v -timeout=20m` 通过（512.98 秒）；`Assistant.test.tsx` 的等待授权、完成、失败状态用例 3 项通过。客户端出现“网络异常，部分对话数据暂时无法读取”提示。后续在本地 Portal/EventSource 夹具中，实际打包的 PC 收到 `task.waiting_user` 后实时显示“待审批”、停止思考动画，并保留已接单👌🏻；API verifier 确认 `agent_acknowledged=true`、待审批请求存在且 `waiting_reason=approval_required`。修正夹具通用断言，使仅含失败/待审批场景时允许没有助手回复后，approval 专项 `TestConversationBrowserAcceptance` 通过（266.12 秒；Go overlay 只屏蔽了工作树中无关 Workspace 签名不一致导致的编译阻塞）。实际客户端画面确认待审批时👌🏻仍贴在原尺寸消息气泡右下角，思考动画已隐藏。
- 主 Agent 实际判断：通过 Morde `ModelGateway.ClassifyConversationRoute` 调用真实 DeepSeek Flash，对 4 类合成文字场景各重复 3 次，共 12 次路由判定全部符合预期：短文件查询、快速 `git status` 不确认；长审查/更新、部署脚本任务确认。针对真实响应曾出现的截断 JSON、Task 决定混入回复文案及误判“重做”意图，已收紧路由提示，并令短 JSON 判定关闭 thinking、温度设为 0；定向单测及上述实时路由验收通过。此项验证的是 Morde 网关中的路由判断，不等同于完整 DSH Agent Runtime 端到端验收。
- T05 已完成：PC 实际客户端的短/长请求、接单后确认、处理中动画、并发、失败/停止、等待授权、流式与重连、结果唯一性及 iOS 同账号接续均已有前述本地验收记录；最终规则保持为只有主 Agent 明确接单并由服务端持久化 `agent_acknowledged=true` 后显示👌🏻。原生 iOS `testTaskActivityShowsLiveProgressFromLocalPortalSSE` 在 iPhone 18 Pro / iOS 27.2 模拟器通过（1/1）：ad-hoc signed app 登录本地 Portal、读取同一 Task，并经本地 Portal SSE 收到 `task.step` 后刷新事件与步骤；合成搜索夹具 `TestConversationBrowserAcceptance` 通过（669.70s）。当前 runtime 源码的 Linux ARM64 集成测试二进制交叉编译通过；在一次性 Lima Linux VM（cgroup v2 可写、Docker systemd、Rosetta）运行 `TestRealDockerAccountRuntimeTaskIsolationAndGuardianCrashRestart/portal-task-worker` 通过（18.59s）。该流程使用当前 runtime 源码副本、固定 Runtime 镜像和真实 DeepSeek ModelGateway；固定合成请求 `1+1` 返回含 `2` 的结果，且 `model_call_ledger` 有 settled 调用，覆盖 Portal Task Worker、DSH 执行、Guardian cgroup 隔离与恢复。全程未访问远端 Portal/Host、未上传文件；本轮一次性 VM、PostgreSQL、网络桥、iOS 构建/结果包及测试二进制均已清理。
