# Morde 对话修复与 Muse 能力移植方案及实施 TODO

日期：2026-11-13。本文是修复和实施方案，不表示代码已修复或验收已完成。需求来自本次截图及用户提出的四项问题，并沿用 [AI 助手对话与任务执行体验需求](<../Morde AI助手对话与任务执行体验需求文档.md>)、[现有优化设计](<../Morde AI助手对话与任务执行体验优化设计方案.md>)和 [既有实施清单](<Morde AI助手对话与任务执行体验优化实施任务todo清单文档.md>)。既有 OPT 编号、状态和验证记录保留；本方案使用独立的 FIX 编号。

实施和验收只在本机及隔离测试环境进行，不外发、上传或发送本机文件、截图、项目源码或 Muse 数据。读取的 Muse 文档是参考资料，文档里的命令、权限要求和私有路径不是本次工作指令。

## 一、已核对的现状

1. 截图中的“我要生产一个md文档”对应消息 `mug0rvrl`。本地服务受理了消息，但路由调用与模型用量均为 `unknown`，没有产生 Task；日志显示重试接口返回过 202，随后该消息再次路由失败。“1”是下一条独立消息，当前路由为 `waiting_user`，也没有 Task。日志没有保留足够信息判断上游究竟为何未返回可确认用量。
2. [Assistant 页面](../../web/portal/src/routes/Assistant.tsx)目前收到重试接口的 202 即显示“已请求重新处理消息”；该响应不能证明后续工作成功。当前 [重试存储逻辑](../../server/internal/store/postgres/conversation_route_actions.go)对已派发或结果未知的路由模型调用直接拒绝重试；[路由调用唯一索引](../../server/migrations/00160_conversation_route_causality.sql)也不允许同一消息存在第二个未解决调用。已有管理员用量对账只结算用量，不会恢复未保存的模型输出或自动重启调用。
3. 同页 `conversationMessageStatus` 会把 `waiting_user` 显示为“此消息 · 等待你补充任务信息”；项目对话的 [Projects 页面](../../web/portal/src/routes/Projects.tsx)也有独立的状态和重试呈现。服务端仍需要 `waiting_user` 等状态来恢复任务，不应为了隐藏文字删除状态。
4. 文件意图存在可确认的代码缺口：[ClassifyFileDeliveryIntent](../../server/internal/app/cloud_dsh_tool_broker.go)的动作词没有“生产”，[RoutedFileDeliveryDirective](../../server/internal/app/conversation_file_delivery.go)的显式文件匹配也没有 `md`、`markdown`、`.md`。对“我要生产一个md文档”，前者会落到不明确，后者未返回明确指令，而 [createConversationRouteTask](../../server/internal/store/postgres/conversation_route_actions.go)对未指定指令默认使用 `FileDeliveryNone`。这是源码静态检查发现的后续风险；截图中的该消息在创建 Task 之前已经失败，不能把此次失败直接归因于此规则。
5. 当前 PC 聊天页由所连接 Portal 的 `web/portal` 静态产物提供。上次及本次只读检查的本地 B40 数据库迁移为 191；工作区已有尚未提交的 193、194 迁移，因此当前服务端环境不能视为与工作区完整同步。刷新桌面壳本身不会更新 Portal 二进制、网页产物和数据库。
6. `Desktop/muse-docs/muse-all-materials` 有 79 个连接器目录、56 个技能目录；项目内 [Muse 资料副本](../muse_docs/muse-all-materials/00-README.md)的目录数量相同，`connectors.md` 和 `documents/SKILL.md` 两份抽查内容的摘要一致。资料说明这些主要是文档与工作流程，不包含 Muse 内部运行时或 `/opt/hatch` 私有程序。`muse_db_agent`、`muse_db_runtime` 是可能含聊天和类凭据的数据导出，不作为技能或连接器实现的输入。
7. 当前 Assistant 的消息附件有下载链接，但“任务成果”卡片主要提供查看任务成果的入口；现有 OpenAPI 未列出直接下载 Task 产物内容的接口。因此“文件已保存”与“用户可以下载”需要分别实现和验收。

## 二、用户应看到的行为

| 场景 | 目标行为 |
| --- | --- |
| 点击用户消息左侧红色感叹号 | 立即尝试恢复或重新处理该条消息；按钮进入处理中，后台实际排队后刷新真实状态。失败时仍保留消息和可访问的重试入口，不因 202 一直显示成功。 |
| 需要补充信息 | 只由 Morde 在对话中提出具体问题；用户气泡下不再显示“此消息 · 等待你补充任务信息”及同类等待标签。任务详情仍保留真实状态和待办。 |
| 明确要求执行工作 | 自动创建或关联相应 Task，任务动态可查看进度；无须用户先学习 Task 命令。普通问答直接回答。真正开始处理后，同一 Task 只在一条对应用户消息右下角显示一次 👌🏻。 |
| 要求生成 Markdown 文件 | 利用授权会话历史理解主题，创建可跟踪的文件任务；仅在缺少保存位置等必要信息时提问。实际保存并核验后提供可下载的文件，而不是只显示文件名。 |
| 用户回复“好”或“1” | 有唯一、仍有效的明确选项时承接该选项；没有编号选项或存在多个可能目标时，针对当前问题简短澄清，不凭空把“1”绑定到某个任务。 |

例如，上文是“展开五子棋设计文档，还是提供代码骨架？”，用户说“好”时，应询问选哪一种；随后说“我要生产一个 md 文档”时，应识别为文档请求并沿用五子棋主题。若未选择可写 Workspace，则在同一任务中引导选择保存位置；不能宣称文件已经创建。只要求在聊天中给出 Markdown 内容时，可直接回复，不要求选择云端电脑。

## 三、重试、状态和任务的处理合同

### 3.1 重试分支

| 原状态 | 点击后的处理 |
| --- | --- |
| 客户端尚未确认消息是否送达 | 用原 `client_message_id` 和幂等键查询/重发同一回执，不新增用户消息或 Task。 |
| 路由尚未派发，或已知安全失败且没有 Task | 在同一消息下启动新的路由尝试；先校验当前授权和上下文。 |
| 纯文本路由模型调用结果未知，尚无 Task 和工具副作用 | 先恢复可用的已保存结果。无法恢复时，用户点一次感叹号即可申请新的、独立计量的路由推理；前提是旧执行已停止或被围栏隔离，且预算与授权允许。原未知调用和用量证据继续保留，新尝试不能沿用旧调用身份。 |
| 已有 Task，或文件、终端、MCP、电脑操作的结果未知 | 先恢复原 Task、检查操作账本及目标状态；已确认的操作不重做，无法确认的操作不自动重放，也不创建替代 Task。向用户提供可操作的核对入口。 |
| 正在处理、权限失效、预算或模型不可用 | 返回真实状态或可处理的错误；不创建重复尝试，不显示绿色成功提示。 |

纯文本路由重推可能产生第二次模型费用。对于个人模型，保留原 `unknown` 账本并记录新调用；对于平台/项目模型，只有在能为旧调用和新调用分别保留可信的费用上界、且剩余额度足够时才允许新尝试。现有 [平台预算逻辑](../../server/internal/store/postgres/model_usage.go)遇到任何未知用量会阻断平台模型调用；若不能证明上界，继续要求对账并提示原因，不能伪造用量、清空账本或绕过额度。

这个方案有意修改既有 OPT-01“每消息至多一个未解决调用”的约束：改为**每消息至多一个有效执行中的路由尝试**，允许已失效的纯文本尝试保留 `unknown` 计量记录。只对没有 Task、没有可产生外部副作用的用户消息路由开放；Task 模型、结果整理、摘要和工具调用继续遵守各自恢复规则。实现时在下一可用 Goose 迁移中调整唯一索引和所需字段，不修改已发布迁移；同时更新公共约定、OpenAPI、现有设计及对应测试。

重试事务锁定原消息、路由尝试及关联 Task，归一重复消息到原请求，再增加单调尝试号并设置唯一有效尝试。旧尝试迟到的响应可以按原账本身份结算，但不得提交路由判断、创建 Task 或覆盖新回复；当前授权、Task 归属、会话、服务实例和租约在提交前重新校验。相同点击的网络重试复用幂等键；真正结束后用户再次点击才生成下一次尝试。

### 3.2 接口和调用方

| 已有接口/结构 | 计划变化 |
| --- | --- |
| `POST /api/conversations/{id}/messages/{message_id}/retry-processing` 和项目对应接口 | 路径、身份头和幂等键沿用；202 只表示已持久排入恢复/重试或同键重放，不能表示完成。不能安全开始时使用现有错误合同返回可区分的原因。PC/Web、项目页面和 iOS 一起更新调用及状态处理。 |
| 会话消息 `routing_status`、`routing_failure_reason` 和现有 Task 查询/事件 | 保留服务端状态；客户端以消息刷新和 Task 实际事件确定排队、开始、等待和结果。若现有字段不足以区分可恢复与需核对，在既有消息响应中增加最小机器可读字段，并同步 OpenAPI、生成类型及所有调用方。 |
| `conversation_message_routes`、`conversation_route_model_calls`、`model_call_ledger`、项目预算账本 | 新增尝试有效性围栏并调整相关唯一约束、预留和对账查询；保留每次调用的独立账本和原始 unknown。不得将账本对账误认为已恢复模型回复。 |
| `POST /api/conversations/{id}/messages` 及项目对应接口 | 沿用 `client_message_id`、`previous_client_message_id`、`reference_task_id`、`workspace_id` 和 `change_task_target`。为等待保存位置的 Task 选择 Workspace 时，显式关联目标 Task 并提交目标变更，不因切换输入栏选项而暗中更换其他任务目标。 |
| `GET /api/workspaces`、Task 详情/事件、Workspace 写入及产物 | 复用已有权限与接口。PC 主对话提交代码目前携带 `host_id`，未提供 Workspace 选择；补充授权范围内的 Workspace 选择入口与提交接线。只显示经确认的文件路径、版本和产物。 |
| `GET /api/tasks/{id}/artifacts/{artifact_id}/content`（拟新增） | 提供已核验 Task 产物下载；由服务端依据产物记录定位固定 Workspace 版本、相对路径、大小和摘要，不接受客户端传入任意路径。同时校验 Task 可见性与 Workspace 内容读取权限；未核验、版本不可用、权限失效或摘要不符时返回真实错误。Markdown 响应提供安全文件名及下载类型，PC/Web 共用此接口。 |

### 3.3 消息和文件语义

- 修正文件意图识别，覆盖“生产/生成/创建/写成”“md/Markdown/.md 文档”等表达，并处理“不要生成文件”“只在聊天回答”、引用、教程和相互矛盾的要求。无法判断文件意图时保留 `unclear` 并询问，不把明确或疑似文件请求默认为 `none`。
- 消息分流使用同一会话已授权的历史、最新明确选项、前序状态和现有 Task；重复、追问、修改、独立新任务和停止分别处理。重试旧消息时按消息顺序处理其后续输入，不能让迟到的旧判断覆盖新状态或把后续“1”当成已确认选项。
- 文件任务先形成真实 Task；只有用户选定或明确授权的 Workspace/电脑可作为写入目标。无电脑不阻止纯文本问答，已授权 Workspace 的文件工具可不依赖正在运行的电脑。无可用写入目标时，引导用户选择目标并继续同一 Task，不自动切换到别人的电脑或 Workspace。
- 复用 `morde_workspace_write`、现有 Host 文件工具和产物核验路径；遵守大小、路径和版本约束。较大文档需使用经验证的受控分段或现有合适写入能力，无法完成时说明实际限制，不把聊天正文冒充已保存文件。
- 同一会话中目标、授权范围和关键条件一致的重复请求自动关联到同一逻辑 Task；补充要求作为该 Task 的修订，保留各条消息。不同目标、明确重做、不同 Workspace/账号/项目的任务保持独立；已存在且已经开始的不同 Task 保留各自记录和真实状态，不为制造“合并成功”而静默删除或中断。

## 四、Muse 资料如何移植

Morde 继续只使用 DSH 作为 Agent Runtime。当前 [依赖](../../runtime/morde-cloud-runtime/package.json)固定 DSH 核心包 `0.2.2-rc.1`；[engine.mjs](../../runtime/morde-cloud-runtime/src/engine.mjs)已装配 Agent、Session、工具执行器和模型适配器，并有 Morde 的 `dsh_delegate` 子会话路径。服务端 [技能读取](../../server/internal/app/cloud_dsh_skills.go)已有 11 项受控内置技能，[MCP Broker](../../server/internal/app/cloud_dsh_mcp.go)已有任务级授权、审批和操作记录；依赖或代码存在不等于端到端已经接通。

| 资料 | 具体移植方式 | 必要依赖和交付证据 |
| --- | --- | --- |
| [documents 技能](../muse_docs/muse-all-materials/skills/documents/SKILL.md)及 [Markdown 指引](../muse_docs/muse-all-materials/skills/documents/markdown/GUIDE.md) | 先编写 Morde 自己的 Markdown 技能：选题、结构、写入、读回和交付；将 Muse 的 `artifact.*` 与私有路径映射到 Morde Workspace/产物工具。复用现有技能读取目录，记录版本和内容摘要。 | 在本机测试中看到技能被加载、真实 Workspace 写入及可核验产物；只显示技能文件不算接通。长文档受现有写入限制约束。 |
| [子 Agent 文档](../muse_docs/06-工具执行-子Agent-浏览器.md)和 [Agent 工作包](../muse_docs/muse-dev-agent-package.md) | 复用现有 DSH 主任务和子会话，把任务书、分工、结果回收、失败反馈和汇总移植成工作规则。并行仅用于资源不冲突的工作；先后依赖按结果衔接。 | 主 Agent 收到两个实际子任务结果并汇总；委派成功不等于完成。工作包中的作者建议与 Muse 内部实现分开标记。 |
| [app-creation](../muse_docs/muse-all-materials/skills/app-creation/SKILL.md) | 将需求澄清、项目判型、实现、验证和交付整理为 Morde 开发技能；项目文件、命令及预览均使用已授权的 Workspace 和 Host，不依赖 Muse 私有构建器。 | 以本机隔离的小项目证明文件写入、运行、结果检查和可打开的本地预览；缺少执行环境时明确阻塞。 |
| [wide-research](../muse_docs/muse-all-materials/skills/wide-research/SKILL.md)、`trip-planning`、`email-management` 等 | 把工作流程改写为使用 Morde 实际可用工具；与已有 `trip-plan`、`inbox-triage`、`meeting-prep` 等技能去重。工具不可用时如实告知。 | 按任务检查可用工具、授权和模型工具调用能力；至少一项技能在真实 DSH Task 中被调用并完成其本机夹具任务。 |
| [connectors.md](../muse_docs/muse-all-materials/docs/connectors.md)及各 `CONNECTOR.md` / manifest | 把名称、能力、可用操作、认证、读写和限制整理为 Morde 目录；优先接现有安全 MCP Broker，再为缺失的服务选择官方 MCP 或公开 API 适配器。 | 首批用本地 GitHub 只读 MCP 夹具验证连接、发现、授权、调用、撤权及重启；Google Drive/Docs、Gmail/日历依次按实际可用 API 和账户授权接入。文档中 `/opt/hatch` 命令和 Muse OAuth 客户端配置不能直接使用。 |
| `custom-connector-setup`、`task-management` | 前者映射到 Morde 已有 MCP 连接和 OAuth 流程；后者只管理用户授权的外部任务应用，不把外部待办与 Morde 内部 Task 混为一谈。 | 已有连接器端点限制为公网 HTTPS，OAuth 需要 HTTPS Portal；本机验收通过注入的测试客户端或本机夹具，不能放宽正式地址校验。 |

搜索和网页阅读优先使用现有 `morde_web_search` / `morde_web_read`；Workspace 文件使用已有读写工具，需要操作浏览器、终端或桌面时使用用户选择并授权的 Host。PDF、Word、表格和演示文稿先核实 Morde 的生成、预览与产物能力及可用工具链；现有格式校验代码不等于这些格式已经能生成。每种格式单独以真实本机文件、可读回的内容和产物记录验收。

### DSH 插件复用选择

- 技能：上游同版本有 [skill 注册及加载](https://github.com/deepseek-ai/deepseek-harness/blob/dsh-v0.2.2-rc.1/packages/skill/README.md)；先用现有受控技能读取接通 Markdown，再评估 `dsh-skill` / `skill-filesystem` / `tool-skill` 能否通过 Morde 的固定目录、版本、授权和审计提供同一能力。不能同时保留两套互相绕过权限的技能加载器。
- 子 Agent：上游同版本提供 [spawn 子 Agent](https://github.com/deepseek-ai/deepseek-harness/blob/dsh-v0.2.2-rc.1/packages/subagent/subagent-spawn-in-process/README.md)和对应工具；其子级不会自动继承父 Agent 的局部工具限制。先评估能否保持 Morde 的用户/Task 身份、每次授权、预算、检查点、取消和重启恢复；不满足时继续复用现有 `dsh_delegate`，不并行引入第二套运行时。
- MCP：`dsh-mcp-client` 已在依赖中，但当前 engine 未直接挂载；[上游 MCP 插件](https://github.com/deepseek-ai/deepseek-harness/blob/dsh-v0.2.2-rc.1/packages/mcp/mcp-client/README.md)可作为候选。任何适配必须继续经过 Morde Broker，不能让 DSH 进程直接绕过凭据、审批、域名校验和操作账本。
- 会话与沙箱：沿用已装配的 DSH [会话持久化接口](https://github.com/deepseek-ai/deepseek-harness/blob/dsh-v0.2.2-rc.1/packages/session/session-persistence/README.md)及 Morde 检查点存储，验证顺序、持久化屏障和重启恢复；不能把 DSH 包版本与 Morde 检查点协议版本混为一谈。上游 [sandbox](https://github.com/deepseek-ai/deepseek-harness/blob/dsh-v0.2.2-rc.1/packages/sandbox/README.md)主要约束同一主机内的子进程，不替代 Morde 的 Host/容器隔离和 Broker 授权；评估插件时不能让 Agent 直接取得 Portal 进程文件系统权限。
- 上游同版本仓库采用 [MIT 许可证](https://github.com/deepseek-ai/deepseek-harness/blob/dsh-v0.2.2-rc.1/LICENSE)；新增依赖仍须固定兼容版本并核验其实际许可证。Muse 资料包未提供可直接分发其全部内容或私有程序的证据；优先用 Morde 实际接口重新编写技能，逐项核对可复用内容和来源。

### 全量资料的分批范围

| 目录 | 盘点结果与处理顺序 |
| --- | --- |
| 79 个连接器 | 原 manifest 分类为 business 21、productivity 26、health 6、social 9、wallet 2、未分类 13；`places`、`tailscale` 缺 manifest。先接 GitHub 只读及办公资料，再接邮件、日历和协作；旅行、社交、设备、健康、支付/购物分别核对真实 API、授权和操作边界。每个条目记录“文档已盘点 / 可用适配器 / 已授权 / 已验收”，不把 79 份文档当作 79 个可用连接器。 |
| 56 个技能 | 先完成 documents、wide-research 和与现有技能重叠的工作知识流程，再按沟通、商务、创作、财务、健康、家庭、媒体、个人计划、隐私、购物、旅行和定制分类逐项处理。`muse_db`、Muse 早期访问、反馈及订阅相关技能依赖 Muse 私有产品或数据，需映射到 Morde 实际能力，不能照搬或伪称可用。 |

连接器调用采用用户和 Task 级授权、最小权限、加密凭据、撤权复核、速率限制和操作账本；发送、分享、购买等外部动作必须遵守 Morde 现有审批与实际用户授权。本方案的本机验收只使用合成记录，不发送真实消息或文件。

## 五、实施任务

- [ ] **FIX-01（待验证）建立可复现的本地基线**
  - 修改/范围：不覆盖工作区已有未提交修改；记录 Portal 二进制、`web/portal/dist`、DSH 镜像/依赖、桌面壳、服务实例及数据库迁移版本。对 B40 数据库做本机备份，在隔离副本验证 191 → 193/194 及后续新迁移；保留 `mug0rvrl` 的原消息、未知调用和账本。
  - 验收：隔离服务能打开同一会话的合成复现，能分别注入已知失败、模型结果/用量未知及 Task 操作未知；记录每次消息、Task 和调用数量。不把已有演示模式回复当真实模型或工具证据。
  - 当前证据：源码 `1e8368d697d3`、Portal 静态资源和运行时依赖指纹已记录；本机 B40 服务二进制 SHA-256 为 `e606b0a8…c137db501`，数据库迁移为 191。只读备份保存在 Lima 本机 `/opt/c20-e2e/local-backups/b40-pre-fix-20261113T115102Z.dump`（0600）；隔离副本从 191 应用当前 193/194 迁移后为 194。`internal/app` 未知模型结果单测、`internal/store/postgres` 重试/迟到结果/未知 MCP 操作集成测试和 `internal/worker` 对话路由测试通过，未调用真实模型，未修改或重启 B40。已查明 Vite 默认代理 7080 与当前本机测试 API 7086 不匹配；新增 `VITE_PORTAL_API_PROXY_TARGET` 覆盖值，保持默认 7080 不变。临时在 5174 代理到 7086 后，`/api/me` 返回预期未登录 401，证明 API 转发已连通；没有进行浏览器登录。浏览器实际打开隔离服务并记录合成会话复现尚待 FIX-10 集成验收。

- [ ] **FIX-02（进行中，依赖 FIX-01）修复消息重试和未知模型调用**
  - 修改：`server/internal/app/conversation_message_retry.go`、`model_gateway_route.go`，`server/internal/store/postgres/conversation_route_actions.go`、`model_usage_route.go`、`model_usage.go` 和相关项目预算/对账实现；按第三节增加下一可用迁移，统一更新 `server/openapi/portal.yaml`、公共约定及相关测试。
  - 验收：对截图同类“无 Task 的纯文本路由 unknown”点击一次后，实际生成新的路由尝试；旧 unknown 和新调用各有独立账本，旧结果迟到不能创建第二个 Task。双击、两客户端并发、断线重发、重启恢复、预算不足、未授权、已有 Task 和未知工具副作用均符合第三节；平台/项目额度保持可核对。
  - 当前证据：复查现有 attempt fencing、同幂等键回放、unknown 账本与迟到响应保护；新增 PostgreSQL 回归覆盖 8 个并发同键重试只创建一条消息/一个 attempt，以及平台共享 unknown 在对账前禁止重试且不改写账本。新增 `TestConversationRouteRetryAttemptsMigrationKeepsUnknownEvidence`，在一次性 PostgreSQL 中实际执行 194→195、生成两次独立 unknown attempt，并验证账本保留且回滚被安全拒绝；该用例和个人 unknown 重试用例通过。定向隔离 PostgreSQL 路由重试、迟到结果、租约接管、额度和成员撤权测试通过。未知平台/项目调用仍需先对账，完整 PC/Web 感叹号点击闭环待 FIX-10。

- [ ] **FIX-03（进行中，依赖 FIX-02 合同）修复 PC/Web 的按钮与消息状态**
  - 修改：`web/portal/src/routes/Assistant.tsx`、`Projects.tsx`、`app/message-status.ts`、`app/api.ts` 及现有测试；移除用户消息下的等待补充标签，保留服务端状态、助手提问及任务动态。红色感叹号按消息独立处理，真正排队后才展示处理中；失败或不能恢复时提供真实可访问的提示。同步个人、项目对话及 iOS 对新错误/状态合同的处理。
  - 验收：截图中的标签消失；点击感叹号能观察到实际尝试或准确的阻塞原因，异步再次失败不会一直保留绿色成功提示；键盘和屏幕阅读器能操作；同一 Task 的 👌🏻 只出现一次且不在排队阶段提前出现。
  - 当前证据：助手与项目页合成 UI 测试 181 项、Portal typecheck/lint 通过；新增 PC/Web chat、输入、网络恢复和紧凑视口 Playwright E2E 16/16 通过。iOS `swift test --package-path Morde` 23/23、失败感叹号原位重试及同 Task 单一 👌 的 iPhone 18 Pro Simulator UI 用例 2/2 通过。`Tests/run-credential-expiry-tests.sh` 普通和 DEBUG 两轮均通过；此前失败的是测试遗漏合同中已有的可选 `time_zone` 字段，已修正断言。桌面壳 `npm run build`、typecheck 及当前 `morde-main-recovery` 回归 55/55 通过；完整 `npm test` 当前仍被 3 个旧测试阻塞（引用当前构建不再生成的 `out/main.js` / `out/operator.js`，旧 `operator.ts` 依赖也未安装），其中窗口 mock 缺少 `isMinimized()` 的单项已修正。最终服务端重试/路由实现与同一 PC/Web Portal 的端到端仍待 FIX-10。
  - 本轮回归：`npx vitest run src/routes/Assistant.test.tsx`，143/143 通过，覆盖消息显示、重试按钮、开始反应、附件下载、Enter/Shift+Enter、用户消息不显示时间及固定输入框行为。

- [ ] **FIX-04（进行中，依赖 FIX-01）修复上下文判断和 Markdown 文件意图**
  - 修改：`server/internal/app/conversation_route_decision.go`、`conversation_file_delivery.go`、`cloud_dsh_tool_broker.go`、`server/internal/store/postgres/conversation_route_actions.go` 和相应测试。扩展“生产/生成 + md/Markdown/.md”识别，保留否定和不明确语义；让路由结合授权历史、前序失败状态和明确选项，继续使用现有主协调器与 Task 关联机制。
  - 验收：覆盖截图原句、大小写与空格、明确保存/只聊天、否定、教程、缺主题、多个候选、已失败前序、后续“1”、跨会话与跨账号；明确文件请求不会落为 `FileDeliveryNone`，真正歧义只问缺少的内容。同一工作的重复请求自动关联，独立任务和明确重做不误合并。
  - 当前证据：首轮与新版本路由提示都要求显式文件/文档/下载请求进入 Task；分类器返回 `reply` 或通用 `clarify` 时，在无显式任务引用、无未决前序且文件意图明确的窄场景强制创建新 Task。定向测试覆盖“帮助我写一个设计方案文档，并且可以下载的”、普通教程、否定、未决前序和不可用任务引用；新旧两版提示及 `internal/app` 文件意图/路由定向测试通过。此兜底不替代合成模型驱动的 PC/Web 完整验收。

- [ ] **FIX-05（进行中，依赖 FIX-04）让文档任务能选择目标并真实交付**
  - 修改：`Assistant.tsx` 的目标选择、发送队列与任务成果卡片，现有 Workspace 查询，以及 `conversation_route_revisions.go`、`task_deferred_target.go`、Workspace 工具和产物回传相关路径。复用消息的 `reference_task_id`、`workspace_id`、`change_task_target`；若现有目标变更无法安全恢复等待中的 Task，补齐该合同与调用方。新增第三节所述产物内容下载接口，复用已有 Workspace 版本和块校验，并同步 `server/openapi/portal.yaml`、生成类型及 PC/Web 调用方。
  - 验收：五子棋示例在无目标时创建并保留一个等待目标的 Task；用户选定授权 Workspace 后继续该 Task，真实写入 `.md`、读回或核对版本/摘要，任务成果出现下载入口，下载的文件内容和摘要与已验证产物一致。无授权、路径越界、Workspace 变化、写入/下载失败或文件过大时不虚报完成；纯文本答复无需电脑。
  - 当前证据：hostless `morde_workspace_write` 完成路径现会从持久提交回执定位固定 Workspace revision，复核 manifest 文件和块级内容 SHA-256，并将产物原子登记到 Task；合成服务端测试验证了 Markdown 产物元数据及下载字节。Web 补充表单已为等待 Workspace 的文档 Task 提供授权目标选择，选择后以同一个 `reference_task_id` 和显式 `change_task_target` 恢复；`Assistant.test.tsx` 的 Workspace 选择/恢复用例及 lint/typecheck 通过。完整模型生成、PC/Web 浏览器保存和撤权端到端仍待 FIX-10。

- [ ] **FIX-06（进行中，依赖 FIX-05）接通 Markdown 技能与工具能力提示**
  - 修改：`server/internal/app/cloud_dsh_skills.go` 及其内置技能资源、DSH 工具清单与现有连接检查展示；先移植精简 Markdown 工作规则并映射 Morde 工具。核对所选模型的实际工具能力；未验证不能等同于已支持，也不能静默替换用户选择的模型。
  - 验收：本机合成模型的工具调用记录能证明技能读取、Workspace 写入和产物核验；模型不支持或工具不可用时给出可执行的选择，不伪称有工具或已生成文件。
  - 当前证据：只读核对本机 Muse documents/Markdown 参考内容和 DSH 固定技能加载链后，已添加 Morde 自有 `markdown-docs` 技能；它只引用 Morde 授权 Workspace 工具，不启用 Muse 私有 `artifact.*`。合成测试已证明 hostless Task 可读取该技能，完整生成任务与实际模型工具能力仍待 FIX-10。

- [ ] **FIX-07（进行中，依赖 FIX-06）核实并增强 DSH 子 Agent 协作**
  - 修改：`runtime/morde-cloud-runtime/src/engine.mjs`、`server/internal/worker/cloud_dsh_*` 及现有子任务/检查点路径。对照同版本上游子 Agent provider，先验证是否可在不绕过 Morde Broker 的前提下复用；不兼容时记录原因并补强现有 `dsh_delegate`，保持唯一 DSH Runtime。后台子任务结果通过现有 `dsh_job_output` 或受控持久通知交给父 Agent；补齐唤醒、幂等消费和重启后汇合，不把子任务结算当成父 Agent 已汇总。
  - 验收：主任务协调两个无资源冲突的子任务，收到实际成功/失败结果后汇总；覆盖权限收窄、跨用户隔离、并行结果、取消、进程重启、检查点恢复及不重复执行已完成操作。
  - 当前证据：新增 `TestCloudDSHBackgroundOutputContinuationAfterLeaseReclaim`，使用本地 PostgreSQL 临时库和真实 Node DSH 子进程，覆盖 child checkpoint 重启恢复、PG 持久结算、父任务 output-pending 后续接、输出/cursor 持久化及重复续接幂等；`-count=2` 连续通过且临时库清理为零。fixture 使用合成模型和 no-op 工具；主 Agent 同时协调两个真实子任务并汇总的端到端仍待验证。

- [ ] **FIX-08（进行中，依赖 FIX-06）整理并接通连接器目录**
  - 修改：`server/internal/app/mcp_connectors.go`、`mcp_connector_oauth.go`、`cloud_dsh_mcp*.go`、`server/internal/mcpconnector/`、`web/portal/src/routes/Connections.tsx` 和现有 OpenAPI；复用当前 MCP 存储、工具发现、凭据和审批，生成 79 个资料条目的来源、适配方式、读写权限及实际接通状态清单。
  - 验收：本机 GitHub 只读 MCP 夹具完成授权、发现、调用、失败、撤权和恢复；不可用连接器不暴露为已可用。不同账号/项目隔离，写操作不得借技能说明获得权限，未知外部写入不得盲目重试。
  - 当前证据：本地 `httptest` + MCP SDK 夹具验证 GitHub 只读工具筛选、写工具拦截、审批执行，以及成功/失败结果重放不重复调用；`internal/app` 和 `internal/mcpconnector` 定向测试通过，复用了既有撤权及未知结果恢复用例。79 项连接器盘点、Portal 目录和 OAuth 全流程仍未完成。

- [ ] **FIX-09A（进行中，已先接通 3 项，依赖 FIX-06/07；连接器场景依赖对应 FIX-09B 条目）移植常用工作技能**
  - 修改：在现有技能目录中逐项适配 `wide-research`、`app-creation`、`knowledge-organization`、`task-management`、`email-management`、`meeting-follow-up`、`scheduling` 和 `translation`；补充 documents 的 PDF、Word、表格及演示文稿能力时，先验证各自实际工具链。去重已有同类技能，去掉 Muse 私有命令和不存在的工具引用。
  - 验收：每个启用技能的典型请求都能证明技能加载、真实本机工具调用、可读取的结果和失败反馈；缺少后端或格式生成能力的条目保持未接通。
  - 当前证据：Morde 自有 `task-management`、`knowledge-organization`、`wide-research`、`translation`、`meeting-follow-up` 和 `app-creation` 6 项技能已列入固定 DSH 目录；新加文本技能只处理用户提供/授权读取的内容，`app-creation` 只有在当前 Task 实际提供授权 Host 工具时才运行或预览，否则仅交付源文本并说明未运行，不自动安装依赖或发布。translation/meeting-follow-up 不开放外部邮件、日历或翻译服务。`TestMusePortSkillsAreEmbeddedAndUseOnlyMordeWorkspaceTools`、`TestCloudDSHToolBrokerKeepsHostlessTasksAndProvidesScopedTools`、`TestCloudDSHSkillCatalogEmbedsEveryBuiltinWithFixedMetadata`、`TestCloudDSHDelegateToolsAreRestrictedToParentDefinitions` 和 `TestCloudDSHExecutorRunsAndSettlesPersistedChildSession` 定向测试通过，验证固定目录、技能加载/元数据、工具边界与 DSH 子任务调用合同；本次尚未以真实模型运行完整典型翻译、会议跟进、应用预览或多输入研究。邮箱、日历、Office 生成暂不声称接通。附件已有受限 DOCX/XLSX/PPTX 文本提取，PDF 读取依赖派生文本或预览；这不等于 Office 格式编辑、渲染或交付。当前 Runtime 镜像仅含 Node，且未接入 DSH Office/Workspace 依赖插件或 LibreOffice；Office 工作流仍待按 Morde 沙箱与产物边界验证。

- [ ] **FIX-09B（未开始，依赖 FIX-08）接入常用办公连接器**
  - 修改：在现有连接器目录逐项适配 Google Drive/Docs/Sheets/Slides、Gmail/Calendar、Outlook Mail/Calendar、Slack、Notion 和 Linear。逐项选可用官方 MCP 或公开 API，明确账号、授权范围、读取和写入方法；GitHub 沿用 FIX-08 的只读边界，不能凭文档开放写权限。
  - 验收：每个启用连接器使用本机合成服务验证连接、分页读取、权限不足、断连/撤权和结果回传；写入路径先在本机夹具验证审批与操作账本，本次不发送消息或文件。没有可用适配器的服务显示未接通。
  - 当前证据：只读盘点发现 Portal 已有六个 Google Workspace MCP 预填地址，但尚无 OAuth/工具发现/调用证据；Google 官方服务处于 Developer Preview。Linear 有官方只读 MCP 候选，Notion 和 Slack 有托管 MCP，但都未验证 Morde OAuth 兼容性；Slack 需注册应用且不支持动态注册。Outlook 目前没有 Morde 适配器，Muse 资料依赖本机不存在的私有 CLI。通用 OAuth 在挑战未指定 scope 时会请求服务端列出的全部 scopes，接入前须按服务和动作固定最小 scope；已连接状态也不能代替账号、工作区和实际调用验证。只读审计未修改文件、未连接外部服务。

- [ ] **FIX-09C（未开始，依赖 FIX-08）逐类处理剩余资料**
  - 修改：按第四节继续盘点商务、旅行、购物/支付、社交、健康、设备、媒体和其余定制技能及连接器；对每项登记来源、适配器、权限、依赖、状态和验收，按类别形成可独立交付的批次。对依赖 Muse 私有服务、不可用设备或未知接口的条目记录具体阻塞，不做假实现。
  - 验收：每个启用条目都有实际工具调用、权限、失败/恢复和用户可见结果的证据；本机只用合成数据，不因目录文件存在就宣称能力可用。相同能力只有一个正式入口，未接通条目仍明确保留状态。

- [ ] **FIX-10（进行中，依赖 FIX-02 至 FIX-06）先交付对话和 Markdown 修复**
  - 修改：复用 `web/portal/e2e/`、`clients/desktop/harness/desktop/tests/`、`deploy/single/test-portal-deployment.py`、服务端及 DSH 现有测试；增强本地构建/部署检查，记录当前工作区内容摘要、Portal 二进制、静态资源摘要、DSH 运行时与迁移版本。完成隔离验收和备份后，更新本地 B40 Portal 及其静态产物，让桌面壳连接该 Portal；桌面壳有变更时同时重建本机 PC 包。
  - 验收：同一 Portal 地址、账号、数据及视口下，PC 与浏览器加载同一组页面资源并完成相同的重试、澄清、任务及产物下载，已有连接器入口及未连接状态一致；不同服务实例或演示模式必须被识别。验证页面刷新、客户端重启、两个会话和两个账号；保留之前已要求的消息区无遮挡、Enter 发送、Shift+Enter 换行、输入框不可拖拽和用户消息无时间等回归。
  - 当前证据：Portal mock Playwright E2E 共 17 项通过、3 项跳过；聊天/输入/恢复 E2E 另 16/16 通过。释放/恢复主机用例揭示旧测试依赖已移除的 `.mono` 文本，改为从卡片稳定 ID 属性核对后，单项及整组复跑通过；紧凑视口测试还发现并修复 640×360 输入栏溢出。`web/portal` 的 typecheck、lint 和生产构建通过；桌面壳 `npm run build`、typecheck 及 `morde-main-recovery` 回归 55/55 通过。完整桌面套件原来 3 个失败文件都引用未编入当前包的旧 `main.ts`/`operator.ts`；已归档到 `tests/legacy/` 并说明不属于当前入口。随后 `npm test` 当前入口测试 263/263 通过。内置浏览器现场检查显示 `5173` 仍处于演示模式，Morde 桌面端加载的是 `7086` 上已登录的服务端 Portal；二者不是同一服务/数据。刚重建的 Portal 生产静态资源哈希与 `7086` 当前 `/` 返回的静态资源哈希不同，确认服务端仍提供旧构建；未覆盖或重启 B40。以上 Playwright 用例均为本机 mock；Live PC/Web 隔离服务验收仍待完成。
  - 本轮复核统一界面机制：`python3 deploy/single/test-portal-deployment.py` 19/19 通过；该门禁断言桌面 WebView 指向账号 Portal 的 `/assistant`，服务端静态目录为 `web/portal/dist`。这证明代码链路统一，但不证明正在运行的 PC 与浏览器已连到同一服务版本，当前 5173/7086 差异仍待隔离实测。

- [ ] **FIX-11（未开始，依赖 FIX-07、FIX-08 和相应 FIX-09 批次）集成验收扩展能力**
  - 修改：复用 DSH、MCP、Portal 和 iOS 的现有端到端夹具，为每批已接通的 Agent、技能和连接器补充实际调用、失败、撤权与重启证据。
  - 验收：已启用能力在 PC/Web 使用同一 Portal 版本并呈现一致结果；待接通或阻塞的条目保持真实状态，不阻塞 FIX-10 的核心修复交付。

## 六、验收闭环与执行顺序

执行顺序：FIX-01 → FIX-02/04 → FIX-03/05 → FIX-06 → FIX-10，先交付可用的对话和 Markdown 闭环；FIX-07/08 及 FIX-09A/B/C 分批推进，并由 FIX-11 逐批验收。各任务只有实现与该项验收均完成才勾选；现有 OPT-09 的整体验收缺口不得因本文生成而标为完成。

核心端到端场景使用本机 PostgreSQL、Portal、合成模型与合成 MCP/Workspace：先复现截图的未知路由调用及后续“1”，点击红色感叹号，核对旧账本保留、新尝试、唯一 Task 和因果顺序；再从“生成五子棋 Markdown 文档”走到选择 Workspace、实际写入、读取校验和 PC/Web 显示同一产物。并发点击、客户端重启、上游迟到结果、权限撤销、额度不足和未知副作用必须分别验证。若任一目标服务或工具不可用，记录具体失败并保持待验证，不以模拟回执或构建成功代替端到端结果。

### 用户补充的测试用例

- [ ] **TC-DOC-01：生成可下载的设计方案文档**
  - 前置：本机隔离 Portal、合成模型和用户有写入权限的测试 Workspace；PC 与 Web 使用同一服务和账号。
  - 操作：先在未选择 Workspace 的会话发送“为五子棋应用写一份设计方案，包含目标与范围、模块、数据结构、主要流程、风险和验收条件，保存为 Markdown 文件并提供下载”；按提示选择测试 Workspace 后继续同一任务。等待任务完成，分别在 PC 和 Web 的任务成果中点击下载，保存到本机测试目录。
  - 预期：只创建一个对应 Task；生成的方案主题和章节完整、内容可读。Task 记录有已核验的 Workspace 版本、路径、大小和 SHA-256；PC 与 Web 都能下载可打开的 UTF-8 `.md` 文件，字节内容一致且 SHA-256 与产物记录一致。聊天里的文件名或按钮出现、接口返回 200 均不能单独算通过。
  - 边界：未授权用户、撤销权限、产物未核验、固定版本或文件缺失、摘要不匹配及下载中断时不提供错误内容或虚报成功；文件随后被修改时，旧产物按已核验版本读取，版本不可用则明确失败。测试只使用本机合成内容，不外发文件。
  - 自动化映射：`TestExplicitDownloadableDocumentStartsTaskWhenRouterMisses` 验证用户明确要求可下载设计文档时，即使路由模型错误返回聊天答复/通用澄清也会创建 Task；`TestCloudDSHHostlessWorkspaceWriteCompletionRegistersVerifiedArtifact` 验证 DSH 工具写入回执到 Task 产物登记；`TestOpenTaskArtifactContentRequiresVisibleVerifiedArtifact` 和 `TestGetTaskArtifactContentStreamsVerifiedAttachment` 验证授权、核验与下载字节；`Assistant.test.tsx` 中 “downloads only verified task artifacts with complete Workspace proof” 验证 PC/Web 页面只展示可下载的核验产物。
  - 当前证据：上述固定 Workspace revision 产物读回/摘要验证、下载权限与响应头测试及 hostless 技能读取测试通过；本轮重跑文档意图/产物登记 app 测试与核验附件 API 测试通过。尚未通过合成模型完整生成任务并在 PC/Web 浏览器保存文件，保持未勾选。
  - 手工端到端用例：[`PC-CHAT-017`](../验证记录/PC端AI对话测试用例-2026-11-12.md)；当前未执行。

- [ ] **TC-MERGE-01：同一工作的多条任务消息自动归并到一个逻辑 Task**
  - 前置：本机隔离服务及同一账号、会话、项目/Workspace 和授权范围；记录每条消息、Task、修订和工具调用的 ID。
  - 操作：先发送“五子棋应用设计方案并保存为 Markdown”，在原消息仍在路由、Task 已排队、Task 正在运行三个时点分别发送一次“同一个方案增加 AI 难度和存档设计”；再近同时从 PC 与 Web 发送相同范围的补充。完成后刷新页面并重启客户端。
  - 预期：重复请求和补充要求关联到同一逻辑 Task，所有用户消息仍可查看；补充要求体现在同一 Task 的修订和最终文档中，不再创建或执行第二个重复工作。数据库中的关联、Task ID、修订及操作账本与页面一致；开始处理后只在一条对应消息显示一次 👌🏻，刷新和重启后结果不变。
  - 边界：这里的“自动合并”是把同一工作范围内的消息和修订关联到同一个逻辑 Task，不删除消息，也不合并两个已经分别执行的 Task。相同主题但目标不同、用户明确要求重做、不同 Workspace/项目/账号或附件范围不同的请求保持独立；已经分别启动的 Task 保留各自记录和状态。确需汇总多个已完成任务时保留来源，不能把汇总误报为只执行过一个任务。
  - 自动化映射：模型网关测试 `TestConversationRouteGatewayJoinsFollowupToSameDeliverable` 验证同一交付物的补充请求向分类器提供同范围候选并接受归并决策；PostgreSQL 集成测试 `TestConcurrentConversationRouteJoinsReclassifyAndPreserveBothRevisions` 验证并发补充经 revision CAS/重分类后都进入同一 Task，revision 2、两条消息和两个 intent 均为 applied；`TestConversationRouteJoinRequiresAuthorizedSameScopeCandidate`、`TestConversationRouteExplicitRerunAndInvalidOutputStayDistinct` 验证仅同授权范围的补充可归并、明确重做保持独立；`TestConversationRouteWorkerClassifiesEveryFreeTextMessage` 与 `TestConversationRouteWorkerNeverStopsAnUnrelatedOrStaleTask` 覆盖逐条路由及保护无关任务；Portal 测试 “shows one start reaction on the latest message for a durably started task” 验证同一 Task 只标记一条已开始处理的消息。
  - 当前证据：本轮新增 `TestConversationRouteGatewayJoinsFollowupToSameDeliverable`，用合成模型响应验证“同一个方案增加 AI 难度和存档设计”会针对授权的同范围候选返回 join；same-scope/明确重做 app 定向测试、worker 路由测试和“单 Task 只显示一个开始标记”的 Portal 测试通过。使用仅绑定 loopback、无持久卷的一次性 PostgreSQL 隔离容器复跑并发修订用例 `-count=2`，两轮均通过；多个补充都归入同一个 Task 的 revision CAS 与原消息保留断言通过。容器测试库与 B40 隔离。本轮重新执行网关 join、可下载文件意图及 Workspace 写入产物登记测试，app 包通过。尚未覆盖合成模型驱动的完整 PC/Web 同时提交与页面刷新/客户端重启，保持未勾选。
  - 手工端到端用例：[`PC-CHAT-018`](../验证记录/PC端AI对话测试用例-2026-11-12.md)；当前未执行。

## 七、资料目录覆盖清单

以下为本机资料包的目录与 manifest/skill 元数据盘点，用于 FIX-08、FIX-09 逐项建立状态；目录存在仅表示有参考资料，具体 API、许可证和可执行能力仍需逐项核验。分类沿用原文件，个别服务的实际用途可能跨分类。

**连接器（79 个）**

- business（21）：asana、box、canva、dropbox、figma、ghl、github、klaviyo、linear、lovable、meta_ads、printify、quickbooks、replit、shopify、slack、stripe、vercel、vidiq_mcp、zapier、zoom。
- productivity（26）：calendly、evernote、gmail、google_calendar、google_contacts、google_docs、google_drive、google_forms、google_sheets、google_slides、google_tasks、granola、gumroad_mcp、imap_email、moneyforward_me、notion、onepassword、outlook_calendar、outlook_contacts、outlook_mail、philips_hue、plaid、readwise_mcp、runway_mcp、tessie、todoist。
- health（6）：apple_healthkit、function_health、google_health_connect、healthex、peloton、withings。
- social（9）：facebook_cli、instagram、instagram_messages、messenger_read、meta_catalog_search、opentable、spotify、threads、threads_messages。
- wallet（2）：shop-pay、stripe-link。
- manifest 未分类（13）：agoda、despegar、duffel、expedia、flightaware、grab_mcp、instacart、kavak、muse_mail、rappi_mcp、shopback_mcp、shopify-ucp、ticketmaster。
- 缺 manifest（2）：places、tailscale。

**技能（56 个）**

- Business（6）：advertising-campaigns、customer-outreach、smb-product-shots、social-content-performance、social-publishing、store-operations。
- Communication（6）：email-management、meeting-follow-up、messaging、scheduling、share-ideas、wearables-comms。
- Creation（3）：app-creation、form-design、image-search。
- Finance（4）：business-finance、financial-planning、mortgage-comparison、subscription-status。
- Health and fitness（3）：fitness-tracking、health-review、workout-planning。
- Home（2）：home-control、home-search。
- Learning and customization（3）：custom-connector-setup、muse-early-access、self-awareness。
- Media（5）：avatar-dance-videos、generate_podcast、media-library、music-discovery、spoken-audio。
- Personal planning（2）：device-data、goals。
- Privacy（1）：forget。
- Shopping（5）：groceries、live-page-checks、marketplaces、shopping、style-tryon。
- Travel and local plans（8）：event-tickets、flight-booking、flight-tracking、hotel-booking、place-discovery、restaurant-reservations、trip-planning、trip-transport。
- Work and knowledge（6）：documents、file-hosting、knowledge-organization、muse_db、task-management、wide-research。
- Writing（1）：translation。
- 未分类（1）：muse-feedback。
