# 会话 4：项目、个人能力与账本实施 TODO

> 阅读：[并行执行入口](00-五会话并行执行入口.md)、[设计方案](../../Morde定位对齐与功能精简设计方案.md)。本清单覆盖 B01–B04、C01/C02/C05–C08/C10、A10 及 P1/P2 的个人和项目业务。
>
> 边界：只写入口第 3 节会话 4 的 `personal/goal/schedule/notification/record/hub/device/project` 专属服务、adapter、Store、domain 和测试，以及本清单。Task/Agent/模型网关由会话 1 修改；Host/Workspace/迁移/公共装配由会话 5 修改；客户端由会话 2/3 修改。

## 接口与交接

- 个人 profile、memory、goal、schedule 继续使用现有个人对象和 Goal/Schedule 业务接口；向 R07 提供带账号、授权版本和用途的读取/受控写入，不复制正文权威。
- 项目和成员、资源、预算的拟新增路由与请求原则见设计 10.3；本会话实现专属业务及 adapter，W08 先准备迁移及合同，P06/R08 就绪后 W08b 装配公开接口；涉及项目会话/Task 的所有权调整由 R08 完成。
- 调度支持无 Host 的纯文本任务；需要电脑时仍校验 Host。项目账本同时遵守现有账号预算；未知费用显示未知并阻止无法控制上限的执行。

## 本机验证范围

按设计 11.1–11.4 执行。项目成员和移交用两个逻辑账号与本地隔离数据库/内存替身验证，不要求两个人或另一台设备；只测试元数据和授权，不传送真实或合成文件，也不调用外部通知服务。真实跨模块联验交 W14，无法安全运行的部分记待验证。

## P0-B：个人资料、目标和例程

- [x] **P01（已完成）核对个人对象和既有同步基线**
  - 修改：只在有缺口时调整 `domain/personal`、`personal_service.go`、个人 Store/API；复用已有 profile、memory、soul、goal、feed 等对象、版本和变更流，读取原实施记录后只补定位增量。
  - 验收：定向单测及本机隔离 PostgreSQL 测试通过 owner 隔离、版本冲突、幂等和保留原对象；复用既有实现，没有重复建表或状态机。

- [x] **P02（完成，依赖 P01）向云端 Agent 提供个人资料/记忆用例**
  - 修改：个人资料、记忆、SOUL/日记业务与专属 Store；定义 R07 可调用的授权读取及受控写入，默认个人私有，版本冲突不覆盖。
  - 验收：同账号可读取获准对象，跨账号/撤权拒绝；重复写入去重、冲突保留原值；交接 R07、PC06、I06。
  - 完成证据：个人对象 API/Store 按 owner 隔离并支持 revision、幂等 mutation；Cloud DSH 仅在当前 personal Task 的 owner、session/client token、scope 和有效期均通过复核后读取有界 profile/memory/SOUL/Goal，并通过 Task 事务写回。日记仍是 owner 私有对象，不自动注入 Agent 上下文。

- [x] **P03（已完成，依赖 P01）完善个人 Goal 和进度**
  - 修改：`goal_service.go`、个人对象与 Goal API/Store；复用 Goal 与账号变更流，让页面和云端 Agent 使用同一目标及版本。
  - 验收：定向单测及本机隔离 PostgreSQL 测试覆盖创建/更新/读取、owner 隔离、版本冲突、幂等与 Goal 配额/版本；不会因记录状态更新清空用户目标。Agent 使用同一 owner-scoped Goal service，通知交接见 P05。

- [x] **P04（完成，依赖 P01）统一云端 Schedule 和无 Host 例程**
  - 修改：`domain/schedule`、`schedule_service.go`、schedule Store/worker/API。纯模型例程允许无 Host；有 Host/Workspace 时继续校验资源授权；复用 occurrence 去重、IANA 时区、失误和撤权规则。
  - 交接：W02 migration/OpenAPI 与 R07 无 Host Task 授权合同已接入；R07 继续负责 Agent 上下文消费。
  - 完成证据：migration `00121_hostless_schedules.sql` 支持无 Host 例程；Hostless Schedule Task 只取得 Task read/submit/control scopes，不授予电脑权限，并按 Task/Run、owner、initiator、位置及有效期复核。PostgreSQL hostless、解绑和授权范围定向用例通过。

- [x] **P05（完成，依赖 P03、P04）接目标阻塞、任务待批和完成提醒**
  - 修改：Notification 业务/worker 与 Goal/Schedule 事件消费；复用现有通知表和去重，不把通知作为执行权威。
  - 验收：目标阻塞、待审批、完成的通知只归获准账号或项目；重复事件不重复提醒；离线后可读取并标已处理。
  - 完成证据：新增 migration `00124_notification_source_outbox.sql` 与 source 通知合同，Goal 阻塞和项目工作项 blocked/delivered 通过固定摘要和收件人快照写入 durable outbox；投递/读取会重验 Goal owner 或当前项目成员资格，通知不含正文。Task 通知维持原有 task 归属。PostgreSQL source 通知、撤权重验、去重及 API 脱敏测试通过。

## P0-C：项目、成员、工作项与预算

- [x] **P06（已完成）建立项目和成员的最小业务模型**
  - 修改：拟新增 `domain/project`、`app/project*`、`api/project*`、`store/postgres/project*`；实现 `owner/member/viewer`、创建/读取、产品内邀请/接受/撤销、显式负责人交接、成员和资源关联的校验、版本及幂等；不发送外部消息。
  - 交接：W08 migration/OpenAPI 与 W08b 项目路由装配已交付；PC08/I08 可先用内存替身完成客户端演示。跨 Runtime 的项目 Task 范围仍按 P07/P08 与 R08 对齐。
  - 验收：受邀人接受前不能访问，错误账号和已撤销邀请不能接受，重试不重复授权；并发成员变更和负责人交接遵守版本冲突及唯一 owner 约束；关联资源不授予超出 Host/Workspace 原有的权限，移除成员后新请求拒绝。
  - 完成证据：`domain/project`、app、API 与 PostgreSQL ProjectStore 已实现；包括 owner 撤销待接受邀请、成员角色/移除、owner 交接、资源独立权限过滤、版本 CAS 和幂等。`00106_projects.sql` 及 OpenAPI 合同由 W08 交付；项目路由已由 `api/api.go`/`cmd/portal` 装配。PostgreSQL 16 本地集成通过邀请接受前隔离、错误账号/已撤销邀请拒绝、并发 owner 交接单一胜者、资源授权、撤权和内容版本用例。完整迁移 runner 可应用全部迁移。

- [x] **P07（完成，依赖 P06、W08）增加项目记忆、资料和目标范围**
  - 修改：个人数据/Goal 用例和 `project*` 业务；项目记录按项目和版本授权，个人记录仍按账号保存。向 R08/R07 提供已授权范围，项目任务不自动读取个人记忆。
  - 验收：两个成员读到同一获准项目背景；项目外账号和撤权成员拒绝；个人记忆、附件及变更日志不混入项目数据。
  - 完成证据：Cloud DSH 初始项目上下文绑定当前服务端 Task 与有效 session/client 授权；PostgreSQL Store 在事务中复核 Task snapshot、项目成员资格和撤权，并只提供有界 project memory/goal/document，不含个人记忆、附件正文或非上下文 Muse 对象。项目成员共享、非成员拒绝、撤权拒绝和大小上限集成用例通过。

- [x] **P08（完成，依赖 P06、R08）实现项目 Todo 与阻塞/交付业务**
  - 修改：`project*` 业务/Store/API，关联现有 Task、Step、验收标准、负责人和已保存 Workspace 版本；任务状态以现有 Task 为权威，不再创建一套执行调度。
  - 交接：本项先用执行替身验证业务记录；随后 R09/W11 接受控多 AI 与 Host，PC09/I09 负责界面，完整链路在 W14 联验。
  - 验收：模拟同一工作项分派、阻塞、恢复及关联已确认产物；成员失权后不能继续以他人身份执行，交付记录与实际版本一致。
  - 完成证据：Task link/assignment/block/resume/delivery API、Store 和授权器已在 `api/api.go`、`cmd/portal` 装配。Task link 事务锁定并核对 project/initiator，幂等重放重验授权；交付核对 Workspace checkpoint 与已保存 revision。Task、Workspace 读取授权器已注入；PostgreSQL 工作项、撤权和交付版本用例通过。

- [x] **P09（完成，依赖 P06、W08）建立项目预算和用量账本用例**
  - 修改：项目预算、费率快照与账本的专属 domain/app/api/Store；以明确币种和版本控制项目额度，保留现有账号预算与账本。
  - 交接：本项先用调用替身验证预算用例；随后 R10 接入模型预留/结算，W12 接入 Host 用量与公共装配。不自动发放奖励或免费额度。
  - 验收：模拟并发预留不超项目及账号双重限制；费用不可确认则显示未知并限制需事前控额的执行；项目汇总可追溯到人、Task 与来源。
  - 完成证据：预算与账本路由已装配；Model Gateway 在发出项目 Task 模型请求前，由同一 PostgreSQL 事务预留项目金额与账号 token 预算，按持久费率快照原子结算；未知费用保留 hold 并拒绝超额执行。Host usage intervals 按唯一 Workspace/Project 资源记录元数据归属，归因不明确时标未知；账本只返回必要元数据。预算、双账本预留/结算、未知 hold、幂等和 Host 项目归属集成用例通过。

- [x] **P10（完成，依赖 P06–P09）落实项目撤权与记录隐私**
  - 修改：项目成员授权、`record*`/`hub*` 相关专属用例；用户、项目、管理员权限分离，完整记录仍遵守独立同意、授权、撤回和审计。
  - 验收：跨项目读取/导出拒绝；项目成员或资源撤销后新请求拒绝，在途执行交由 R08/W08 按停止证据处理；个人正文不进入普通审计。
  - 完成证据：项目 content/search/comment/work-item/connector/Workspace 请求均在 Store 与服务层复核当前成员、资源和调用 Task；个人 record/attachment 继续按账号 owner 与独立 consent/admin grant 隔离。撤权后的在途 Task 进入 cancelling，完成必须有 Agent stop evidence；没有停止证明不提交结果。跨项目搜索、评论 revision、connector 撤权、Task 撤权与脱敏测试通过。

- [x] **P14（完成，依赖 P06、P07）对齐 Workspace 移交与项目授权**
  - 修改：`project*`、`device*` 和必要的通知业务；为 W21 提供项目资源关联、旧设备/项目授权撤销和历史记录保留规则，移交申请仅发产品内状态，不发送外部消息或文件。先用替身完成本业务边界；具体所有权与关联撤销的同一事务由 W21/W14 装配。
  - 验收：模拟申请冻结、取消、接受后旧项目及成员不能凭旧授权继续访问；新项目仅经新所有者明确授权可关联，原项目历史和实际操作者归因保留，不自动复制个人数据。
  - 完成证据：Workspace 接收在同一所有权事务内撤销旧设备 binding/challenge/folder 与旧 owner 的节点期望状态，并保留 installation identity 供其它 Workspace 使用；项目资源按既有授权路径同步撤销。取消保持状态，注入失败回滚 ownership/device/node 变更。孤立 folder 可安全清理，因为设备授权与节点期望状态均要求有效 binding。接受、取消、rollback、孤立 folder 与全部 Workspace transfer PostgreSQL 用例通过。

## 清理和后续

- [x] **P11（完成：Morde 正式 PC/iOS target 不再依赖旧 nanoMuse Cloud；保留 legacy nanoMuse 路由和历史）收敛旧 Cloud 业务和设备授权**
  - 修改：服务端 Hub、设备及个人同步专属用例；与 PC10/I10 协调旧 nanoMuse Cloud 登录/同步的调用方退出，不创建另一套 Cloud 权威。保留用户已确认的目标 PC 单次截图、文件查看、命令及已选 Workspace 写入能力和各自门禁。
  - 验收：旧本机历史可读、旧 Cloud 不再成为正式客户端依赖；设备动作不能委派本机 Agent，撤权和无权限拒绝。代码和本地 helper 已验证；外部设备间真实文件传输保留为目标环境验收项。
  - 完成证据：服务端 Hub/设备授权、撤权、设备移交清理与 Workspace 写入门禁均由 Morde API/Store 提供；敏感设备动作绑定当前授权 Task 和 grant version，测试覆盖撤权与无权限拒绝。正式 PC 使用 `harness/desktop/src/morde-main.ts` 与 `/api/*`，不打包 dsh-nanomuse；正式 iOS Morde target 只编译 Morde 登录、Portal 与 Hub 文件。旧 nanoMuse Web 的 `/api/cloud/*`、跨设备聊天 `/api/sync/*` 仍服务 legacy nanoMuse 构建，不是 Morde 正式客户端调用；Morde `/api/sync/snapshot|changes` 是个人对象同步，明确不代替旧聊天同步。旧本机历史和旧服务实现未删除；旧 Cloud 线上账号/历史不迁移。`dsh-nanomuse` 中的 Morde Hub 适配器仍沿用该 legacy Harness 插件服务，但凭据与 Cloud 登出状态隔离，不构成 Morde 正式客户端或服务端的第二套 Cloud 权威。真实外部设备间文件传输未执行；本机真实 helper round-trip 已核对选定目录、文件列表、限长读取及路径穿越拒绝。

- [x] **P12（服务端完成；客户端入口由 PC11/I11 跟进，依赖 P06–P10）实现 P1 项目连接器和技能授权**
  - 修改：项目技能/连接器的非秘密配置、授权和撤销用例及通知分级所需数据；复用现有个人/MCP 连接器，不复制个人凭据。
  - 验收：项目/个人范围独立，成员撤权后不可用；按条件生成通知且不泄露正文。R12 和 PC11/I11 接入。
  - 完成证据：Project MCP Connector grant/revoke API、数据库和 Task runtime 授权已接入；每次执行核验项目成员、Task initiator、连接器 owner 的显式授权和撤权状态，不复制 credential。成员移除与连接器删除在既有 Store 事务内撤销 grant 并围栏在途操作，迁移不使用触发器推进业务状态。技能只从固定 allowlist 提供只读内容。PostgreSQL connector owner/initiator/revocation、连接器删除和项目路由测试通过；客户端演示页/入口验收属于 PC11/I11。

- [x] **P13（服务端完成；客户端入口由 PC12/I12 跟进，依赖自身相关 P0）实现 P2 和待排的 Muse 数据能力**
  - 修改：Feed、Ideas、Library、简报的统一个人/项目业务范围，以及行级批注、跨项目搜索所需授权查询；不把建议 P2 当成已完成能力。
  - 验收：有权项目可读写、跨项目搜索与批注不得泄露无权内容，旧数据保留；PC12/I12 负责页面，W16 负责文件侧权限/版本支持。
  - 完成证据：项目 Feed/Idea/Library/Brief 独立版本 API、当前成员范围搜索与 revision-pinned 行级评论已实现；撤权后搜索/评论读取拒绝，跨项目 canary 内容不泄露。PostgreSQL revision、搜索、comment ACL 集成测试通过。`row_id` 是调用方提供的稳定锚点；现有内容 JSON 没有独立行目录，Store 无法证明该锚点对应正文中的实体行。UI 的本地内存验收由 PC12/I12 负责，真实媒体服务不在本项验证范围。

## 本轮统一验证记录

- 通过：本轮 `GOTOOLCHAIN=local GOPROXY=off go test ./internal/app ./internal/api -count=1`、Morde Hub API 专项测试、`morde-sync` Hub inspection helper 专项测试通过；PC Desktop `npm test` 256/256、nanoMuse Web `npm test` 157/157；P11 Morde Hub/Reach 专项 `node --test tests/cloud-morde-hub.test.mjs tests/reach-morde-hub.test.mjs` 22/22 通过。使用临时本机目录和本地 `morde-sync` 二进制完成 Hub helper 选目录、列出/读取文件及拒绝路径穿越的 round-trip。Morde Web `npm run build` 通过，产物仅含入口、CSS/JS，未包含旧 Cloud/sync 路由；此前记录的 PostgreSQL P02/P04/P05/P07–P10/P12–P14 定向集成及 `TestPostgresWorkspaceOwnershipTransfer` 组通过。
- 未通过：`project/nanoMuse/harness/dsh-nanomuse npm run build` 的 esbuild bundle 阶段完成，但 TypeScript declaration 阶段因现有 `src/rooms.ts` 缺少 `syncMordePersonal`、`selectMordeScope`、`integerMap`、`mordeGoalBodyMap` 而失败；本轮未改这些 Rooms 文件。全量 PostgreSQL suite、race/coverage、Linux 目标机、真实跨端/文件传输均未运行；不连接测试服务器、不调用外部 AI 或发送文件。Host usage 当前明确返回 `price_status=unknown`、`host_rate_unconfigured`，没有伪造费用。
- `make generate` 未运行，共享生成仍由会话 5 负责；本轮未运行 `make lint`。
- 2026-11-03（Runtime/会话任务收尾）：`project/morde-runtime` 与 `project/morde-sync` 全量 `go test ./... -count=1` 通过；本轮受影响的 `morde-sync/client` 复测通过。Server `make test race cover` 通过，覆盖率汇总 46.2%；使用仅绑定本机 `127.0.0.1:55432` 的 PostgreSQL 执行 `make it` 通过（PostgreSQL 包 388.083s、Worker 包 95.813s），OpenSandbox 因本机未配置 `CH_OSB_URL`/`CH_OSB_API_KEY` 而跳过。worker 断连后发布自身 Workspace 产物的回归用例通过；Runtime/Workspace mtime 元数据传播已覆盖。`sqlc v1.31.1` 的本地缓存源码生成成功，重复生成输出哈希一致。`make generate`/`make lint` 包装命令在离线 Go 工具链元数据查询阶段失败；改用本地缓存的 sqlc v1.31.1 完成生成，并以本地缓存的 golangci-lint v2.14.0 检查为 0 issues（项目声明版本 v2.15.0 未缓存，故未声称该精确版本通过）。最终 `git diff --check` 与 378 个已改/新增 Go 文件的 `gofmt -d` 检查通过。全程没有连接测试主机、调用外部 AI 或发送文件。

## 交接记录

- P02 → R07：Personal Task 读取和受控写入绑定 owner、当前 Task/session/client token、scope 和授权有效期；每次重新检查授权。Diary 保持 owner 私有，不默认注入上下文。
- P04 → W02、R07：`00121_hostless_schedules.sql` 与 Task/Run 范围授权已完成；Hostless 任务没有电脑权限。
- P05 → W02、R07、W08：`00124_notification_source_outbox.sql` 已明确 Goal/项目 source、收件快照、去重和撤权重验；仅保存脱敏摘要。
- P06/P07 → W08、W08b、R08/R07：Project/content 路由已装配；Cloud DSH 项目上下文用当前获权 Task snapshot 查询有界 memory/goal/document，不继承个人记忆。
- P08 → W08、W08b、R08、W16：Task initiator/project/Workspace checkpoint 的锁定校验、重放授权检查、delivery revision 验证和 runtime 路由装配已完成；跨真实客户端执行交 W14。
- P09 → W08、W08b、R10、W12：Model Gateway 已原子预留/结算项目金额与账号 token budget。Host usage 保存 CPU/memory 用量及项目归属，但 Host rate 未配置时明示 unknown；不生成虚构金额。
- P10 → W08、R08、W14：项目对象和记录路径按当前成员/资源授权校验；个人 record/attachment 保持 owner 与独立 consent/admin grant。被撤权的在途 Task 需 stop evidence 后才能结束。
- P11 → PC10/I10：Morde 正式 PC/iOS target 的旧 Cloud 调用边界已核验；旧 nanoMuse Cloud account/C7 UI 仍供 legacy nanoMuse 构建使用，不能将个人对象 sync 当成聊天同步替代，也不因 Morde 迁移删除 legacy 路由、账号或本机历史。Morde Hub/device scope、撤权、Workspace transfer cleanup 与授权写入门禁已完成；PC10/I10 各自未完成的签名、发行入口和真实目标机验收继续由各自任务跟进。
- P12 → W08、R12、W15、PC11/I11：项目连接器的非秘密授权、固定 skill allowlist 和执行时 owner/member/initiator 检查已完成；PC11/I11 负责正式 UI 接入。
- P13 → W08、W16、PC12/I12：Feed/Idea/Library/Brief revision、项目范围搜索和 revision-pinned comment 的服务端合同及 ACL 已完成；PC12/I12 负责 UI，真实媒体服务不在本轮范围。
- P14 → W08、W21、W14：Workspace 接收/取消与旧设备状态清理、节点 desired revision、项目资源授权处于同一所有权事务；取消保留状态，失败回滚，历史与操作者归因保留。
