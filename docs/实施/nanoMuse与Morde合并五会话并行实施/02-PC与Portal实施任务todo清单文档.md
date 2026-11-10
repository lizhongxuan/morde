# 会话 2：PC 与 Portal 实施 TODO

先读[并行入口](00-并行执行入口及任务总览.md)第 1、3、4、5 节。本会话独占原 nanoMuse PC、Portal/Workbench 和本文；服务端合同及 `project/morde-sync/**` 交会话 5。PC01–PC13 的本机完成记录继续保留，以下按最终合并要求补齐差异与验证。

- [x] **MC-01（本机正式入口、打包白名单、构建和 274 项测试通过；发行包产物与真实 Morde 服务未验）核对原 nanoMuse PC 正式入口与品牌**
  - 修改：`project/nanoMuse/harness/desktop/**`、`harness/dsh-nanomuse/**` 的正式启动/打包和界面；最终应用保留原客户端交互并使用 Morde 品牌、账号和云端服务，不启动本机 Agent 或直接模型循环。旧独立 Desktop 只作参考。
  - 验收：检查构建入口、打包白名单及启动链；本机启动后能够进入原客户端主界面和 Morde 登录，无旧 Cloud 服务回退，本地数据保留。
- [x] **MC-02（本机 Portal/MSW/页面验收完成；跨入口批准用例由组件与 API/MSW 覆盖，真实后端未验）接账号注册、审核与默认额度管理**
  - 修改：`web/portal/src/routes/Register.tsx`、登录/管理员用户和设置页面及对应 `web/portal/src/app` API；注册界面仅用户名、密码和确认密码，移除选填邀请码输入，不引入短信。管理员可设置默认模型 token 额度/并发上限并批准或拒绝申请。
  - 验收：本机内置浏览器验证仅用户名/密码/确认密码的注册表单、待审核状态和管理员默认额度配置；跨 Portal/Admin 入口的首次批准、拒绝与批准后登录由组件/API/MSW 测试覆盖（两个独立入口不会共享 MSW 进程内状态）；请求只包含用户名/密码，重复批准不显示额外额度。
- [x] **MC-03（本机 API/MSW/Workbench 测试与浏览器门禁通过；真实后端、Host 与传输未验）接 PC 会话、Host 与 Workspace**
  - 修改：原 PC 聊天、资源库、Workspace 和受控工作台的现有入口；沿用已有安装凭证、Task 状态、Host 授权和 `morde-sync` IPC。共享同步实现的改动向会话 5 交接。
  - 验收：本机替身/内置浏览器覆盖同账号会话、断线、无权限及 capability 未开放；`workspace_sync` 未声明时不启动实际传输或误报成功。
- [x] **MC-04（会话 2 本地实现及 MP-01/MP-02 所有者验收完成；旧 DSH Schedule 仍需用户逐项核对/停用；真实服务未验）迁移个人数据与旧服务入口**
  - 修改：PC 设置、资料、记忆、目标/例程和资源库页面；Morde 为云端数据权威，保留本地未同步草稿和用户选择导入的历史；旧 NanoMuse Cloud 登录、Relay 和个人模型 Key 使用入口按已确认范围收敛。
  - 验收：两个本机逻辑账号验证 scope、离线草稿、冲突和登出；Portal 历史导入先预览、再由用户明确提交；旧 rooms 数据不自动上传，不连接旧服务或发送文件。
  - [x] **Portal `rooms.json` Memory/Goal 导入（本机会话完成）**：文件在本机解析、逐条选择，只写所选记录；稳定 ID/幂等键使重试可安全识别已有记录，冲突不会覆盖远端内容。全量本地测试和 MSW 页面验收通过，详情见 2026-11-04 记录。
  - [x] **Portal `rooms.json` Feed/Ideas/Library/Soul 与独立 `profile.json` 导入（本机会话完成）**：Feed 只映射文本、类型、时间和喜欢状态；Feed routine、Identity、Diary、聊天及目标扩展字段明确留在源文件；Ideas 只接受当前静态目录中的标记；Library 只迁名称/类型，不读文件字节或携带路径；`docs.soul` 可单独勾选。Profile 只迁 Morde 支持的名称、基础头像、简介和风格，未选字段保留当前值；不兼容头像和旧版额外字段留在原文件。每项先预览、明确勾选再提交。旧 ZIP 不会展开，`feed_posts.json`、`ideas.json`、`memory.db`、`goals.db`、`reminders.db` 留在原 ZIP。本会话按会话 5 集成反馈将旧 ID 校验收紧为 ASCII 1–128 字符，并排除 `.`/`..`。
  - [x] **DSH Schedule JSON 导出与 Portal 导入（本机会话完成）**：旧 DSH 本机导出和 Portal 本机逐条预览共用 10 MiB/2000 条上限；标题派生合同校验、稳定幂等重试和逐条停用确认门槛均已覆盖测试及 MSW 浏览器验收。真实旧计划停用状态不能由现有 DSH 服务读取。
- [x] **MC-05（本机会话验证与交接记录已更新；MC-04 遗留运行限制和未测项已列明）验证并交接**
  - 修改：本文及本人负责的旧 PC 清单状态；Portal 前端类型在会话 5 发布 OpenAPI 合同后生成。
  - 验收：运行受影响模块的本机类型/构建检查和必要页面验证；记录命令、结果、未覆盖项并交会话 5 汇总。
- [x] **MC-06（2026-11-05 品牌收敛完成；旧 Cloud/源项目与技术标识保留）核对并更新 PC/DSH/Portal 的当前 Morde 显示名**
  - 修改：Desktop 菜单、托盘、窗口、About、启动/权限提示、helper 显示名及页面标题；DSH 侧栏/关于/版本/欢迎标题、权限说明、助手默认名；Portal Profile/Soul 默认名和灵感目录说明。未改独立旧 Desktop、Workbench（均未发现当前用户可见 nanoMuse 产品名）。
  - 边界：保留旧 nanoMuse Cloud、旧资料导入来源称谓、社区/许可证/Meta 法律来源、GitHub/官网地址、helper 文件名/可执行名/bundle ID、目录/包名、`NANOMUSE_*`、路径、协议/API/IPC 与旧导入幂等 ID。
  - 验收：Desktop 274 项测试；DSH 439 项测试与 typecheck；Portal PersonalData/rooms importer 定向 20 项与 typecheck；受影响差异空白检查通过。未访问测试机或使用账号/API key。
- [x] **MC-07（Portal 已接入会话 5 的全局默认模型合同并本机验收；服务端回归由会话 1/5 负责）管理员默认模型、个人连接与 Assistant 门禁**
  - 合同：`GET /api/available-models` 提供 typed 全局默认模型状态；`GET /api/admin/settings.default_model` 读取选择与版本，`PUT /api/admin/settings/model-default` 使用 expected_version CAS，空 `model_id` 清除选择。默认候选仅限共享、已检查且支持文本与工具调用的连接。
  - Portal：`/admin/settings` 可设置/清除默认模型，冲突后要求刷新；`/connections` 可新增个人 HTTPS 模型、提交 API Key、读回密钥状态并检查能力。密钥仅短暂存在组件输入状态并发往当前 Portal 域名，提交后立即清空；不写浏览器存储，具体出站域名由服务端 allowlist 校验。个人连接按 owner 可见。
  - Assistant：用户可选自己已检查且支持文本和工具调用的个人模型；新任务仍要求服务端平台默认模型有效。默认项缺失或不可用时提示并禁用提交，不因用户连接绕过。
  - 验收：`Connections`、`AdminSettings`、`Assistant`、typed API 与 MSW 定向 5 个测试文件共 140 项通过；Portal typecheck、lint 与 `git diff --check` 通过。本机内置浏览器 + MSW 实际添加/探测合成个人模型，Assistant 下拉出现该模型；管理员默认模型第一次清除操作显示未配置。
  - 未覆盖/交接：浏览器刷新后再次清除时有一次本地 MSW 通用“服务暂时不可用”；组件与 handler 的 CAS/清除/无默认提交拒绝测试均通过，真实服务未访问。会话 1/5 继续验收真实 API、数据库 CAS 与任务侧 fail-closed；本会话没有改 API/OpenAPI/server，也没有部署。

## 交接记录

只追加本会话任务 ID、目标会话、接口/文件、输入输出、权限与错误、验证结果；不编辑其他会话清单。

### 2026-11-03 本轮进度

- MC-02 注册与默认额度页面均已接入：Portal 不再显示邀请码，API 只发送用户名和密码；生成会话 5 OpenAPI 类型并使用 CAS 保存默认预算；未配置首次审批返回 `invalid_state` 并保持 pending。页面/API/MSW 用例和全量 Portal 回归通过。本机浏览器确认注册页字段及提交提示、待审核账号登录被拒、管理员设置保存和读回；批准链由组件/API/MSW 覆盖。
- MC-03 本地页面复核：Assistant 展示云端任务执行未启用并禁用发送；Workspace 设备进度只显示无活动同步会话，未启动传输；Workbench 的 Task 观察页只显示服务端定义的合成状态、事件和已核验元数据。Portal/Workbench 全量 API/MSW/组件测试及类型检查通过；真实后端和 Host 仍未连接验收。
- MC-04 DSH Morde 个人同步：`harness/dsh-nanomuse/src/rooms.ts` 有分页快照、账号 scope 隔离、Memory/Goal revision 写入、删除 tombstone 与冲突保留；Profile 抽屉在 Morde 登录时显示 Morde `profile/default`，名称和基础形象通过 `profile/update` 重新读取 revision 后 CAS 写回，冲突不覆盖远端；Morde 模式不打开旧 Cloud 头像工坊，避免旧服务收到资料或头像内容。首次同步不导入仅本地数据。该 11-03 记录中的 DSH 本地 Feed 例程/Schedule 和旧 `rooms.json` 用户选择导入缺口，已由后续 11-04 Schedule JSON 与 rooms/Profile 导入记录关闭实现。
- MC-01 Desktop 正式入口核对：`morde-client` 使用 `out/morde-main.js`，打包白名单没有旧 DSH 主入口、本机 Agent 或 nanoMuse Cloud 插件；Desktop build/typecheck 与 274 项测试通过。旧 DSH 插件不在正式包中；本地资料/Schedule 迁移缺口已在后续 11-04 记录完成，不是正式客户端构建错误。
- MC-04 Portal 个人数据内置浏览器复核：关闭 demo 合成账号同步后保存 1 项离线资料草稿，退出并登录 member 合成账号时草稿不显示、待同步数为 0；再登录 demo 后同步仍关闭且草稿保留。另有同源/user ID 分区、离线队列与冲突重基测试通过。新端口冷启动页经本地 MSW 读取空快照与额度后无解析错误；之前端口的一次 mock HTML 解析提示刷新后消失，未能在新端口复现。

### 本轮验证记录

- Portal（Node v24.19.0）：`npm run generate:api`、`npm run typecheck`、`npm run lint`、`npm test` 均通过；45 个 Vitest 文件 375 项通过，copy-mobile Node 测试 2 项通过。`vite build --outDir /tmp/morde-portal-check-20261103` 通过，最大 790.19 kB chunk 超过 500 kB 的拆分提示。
- Workbench（Node v24.19.0）：`npm run typecheck`、`npm run lint`、`npm test`（11 个文件/113 项）及 `npm run build` 均通过。
- Desktop：`npm run build`、`npm run typecheck`、`npm test`（274 项）通过；本机运行的 Morde 登录页已核对，未输入凭证或发起服务请求。未构建发行包。
- 内置浏览器均使用 `127.0.0.1` Vite、MSW 和合成账号；检查注册请求只带用户名/密码、待审登录被拒、云端任务/设备传输未就绪时入口关闭、Workbench 观察只读，以及 PersonalData 跨账号登出隔离与离线草稿保留。未访问远端测试机、未连真实 Morde/Host、未发送文件或调用外部模型。

### 跨会话合同交接

- **目标会话 5 / MX-02，合同已冻结且后端/OpenAPI/数据库本机验收完成（据并行入口及会话 5 清单）。** `GET /api/admin/settings` 返回 `default_model_budget: {configured, token_limit, max_concurrent, version}`，未配置为 `false/0/0/0`，`automatic_new_user_model_budget` 固定为 `true`；`PUT /api/admin/settings/model-budget-default` 以 `{expected_version, token_limit, max_concurrent}` 做版本 CAS，初配 expected_version 为 0，成功后递增，token_limit 为正数、并发 1..100，旧版本为 409 `conflict`。首次批准无默认额度时返回 409 `invalid_state` 并保持 pending。会话 2 已生成 OpenAPI 类型并接入 API、管理员表单、冲突刷新、MSW 额度写入和审批门禁；不改会话 5 后端文件。本机页面/MSW 已验；未连接真实服务或测试机。

### 2026-11-03 MC-02 当前进度

- **状态：本机验收完成。** 注册表单与 API 只使用用户名和密码（页面另有确认密码），没有邀请码、短信或手机号字段。管理员默认模型额度使用会话 5 已冻结的 OpenAPI 类型；支持查看未配置 `false/0/0/0`、设置正数 Token 额度与 1–100 并发、expected_version CAS、409 后停用保存并要求刷新确认。用户审核页对 `invalid_state` 显示先配置默认额度的提示。真实后端链路未验。
- **MSW 行为：** 未配置时首次批准 pending/rejected 账号返回 409 `invalid_state` 且保留待审；有效 PUT 版本递增；陈旧版本返回 409 `conflict`；首次通过时复制默认额度，后续变更平台默认值或禁用/恢复账号均不覆盖该账号额度。
- **本机浏览器：** `127.0.0.1:5176` + MSW 验证注册页只有用户名/密码/确认密码；本机创建合成账号后登录被拒并显示待管理员审核。本机 demo 管理员设置 500,000 tokens / 并发 8 成功，读回版本 v1。Portal/Admin 分入口加载，MSW 新账号只保存在进程内，切换入口登录会重置 mock 用户；批准/拒绝与批准后登录转移由 API/MSW 及组件用例覆盖。未向远端测试机发请求。
- **定向验证：** Node 24 的 Portal `vitest run` 对 `api.test.ts`、`handlers.test.ts`、`AdminSettings.test.tsx`、`AdminUsers.test.tsx` 运行与本项相关用例，92 项通过；全量 45 个 Vitest 文件/375 项及 copy-mobile 两项通过，未复现之前隔离运行时的 Workspace/Host 时序失败。

### 2026-11-03 会话 2 续验与交接

- **MC-02：** Portal 全量测试、类型检查、Lint 和生产构建通过；管理员额度保存到 `500000` tokens / 并发 `8` 后页面显示版本 `v1`。注册页仅有用户名、密码和确认密码；本地合成账号注册后显示待审核，登录显示“账号正在等待管理员审核”。首次批准必须先配置默认额度、批准写入预算及幂等语义已由 MSW/组件测试覆盖。连续跨入口浏览器流程受 MSW 内存重置限制，未以真实账号或远端服务补测。
- **MC-04（该 11-03 记录时）：** DSH 测试覆盖账号 A→B→A 分区往返、旧 `rooms.json` 不自动导入、显式 Memory/Goal 写入、Profile revision 写回、并发 Profile 冲突不覆盖远端及登出中止分页请求；另外验证 Morde `dragon` Profile 不发送 `emoji/color`、`emoji` Profile 保留合规字段。`pnpm run typecheck`、`pnpm run build`、`pnpm test`（431 项）与受影响文件 `git diff --check` 通过。Portal 已有用户选择、预览后明确提交的会话历史导入 UI/API/MSW 测试；本机浏览器确认该入口，无文件被选择或发送。该日期尚未完成 DSH 旧 rooms/例程/Schedule 迁移；相关实现已在下述 11-04 记录完成，MC-04 当前状态见最新交接。
- **跨会话交接 → 会话 4/5：** `harness/dsh-nanomuse/src/rooms.ts` 读取 `/api/sync/snapshot` 与 `/api/goals`，以 `/api/personal-data/{type}/{id}` 和 Goal revision API 写入 Memory/Goal；`profile/update` 读取当前 `profile/default` revision 并 CAS 写回。Profile 仅使用文字和基础形象字段，没有头像附件上传；该 DSH 插件未在正式 Desktop 白名单中。Portal 会话历史导入已要求用户选择、预览并明确提交。若继续迁移 DSH 本地例程，明确旧 Schedule 的 session/model 映射、`model_id`、预算、Host/时区、执行 Gate 与用户明确启用语义，避免离线任务被隐式搬迁或重复触发。会话 2 未修改服务端合同、未上传文件。
- **本轮命令：** Portal（Node v24.19.0）`npm test`（45 文件/375 项 + copy-mobile 2 项）、`npm run typecheck`、`npm run lint`、`npm run build` 均通过；API/MSW/管理员组件定向回归 92 项通过。构建 chunk 790.19 kB，超过 500 kB 提示；nanoMuse Web PWA 构建及 mobile copy 通过。DSH `pnpm test`（431 项）、`pnpm run typecheck`、`pnpm run build` 通过。Workbench 11 文件/113 项、类型/Lint/构建结果沿用本清单既有验收。

### 2026-11-03 会话 2 Schedule 与旧 rooms 导入续验

- **MC-04 / DSH rooms.json：** 增加只读预览和逐项勾选导入。仅选中的 Memory/Goal 经当前 Morde 账号作用域同步；预览不写入，Feed 动态/例程、Library 索引和聊天明确留在本机，旧 `rooms.json` 不改写。账号登出时拒绝预览/提交，导入标记保存在当前账号本机分区，重复提交不重复创建。
- **MC-04 / 旧 DSH Schedule 映射：** 增加只读枚举、mapping preview 和逐条明确提交。旧 `title` 作为独立 title 原样发送，旧 `prompt` 原样作为 objective；title 缺省时不发送该字段，由服务端按冻结合同从 TrimSpace 后的 objective 派生，客户端要求创建响应始终包含 title。Host 为 null，Morde 文本模型与预算由用户选择；执行策略、通知和 observation 在提交预览中显示。稳定 `Idempotency-Key` 由当前账号 scope 与源 session/schedule ID 派生；远端创建结果不确定时重试仍用同一 key。旧计划不会被删除或自动重启。
- **映射边界：** 一次性/每日/每周时间可映射；`every` 仅接受 1–168 小时的整小时周期，以旧 `scheduledAt` 原样作为 `anchor_at`，需为显式时区偏移的 RFC3339 且小数秒不超过 6 位；不支持或无法无损表示的计划显示原因并禁止导入。带额外旧字段（包括旧模型参数、expression、未使用的定时字段）仍禁止导入。旧会话 ID 会在预览显示并明确提示不会迁入新建的 hostless Schedule。
- **旧计划停用门槛（运行限制）：** 创建新的 enabled Schedule 前，每条计划需用户勾选“已停用原 DSH 计划”并明确提交。DSH Schedule 服务目前只有 list/delete，没有读取 enabled 状态或 disable 操作；因此这项是用户确认，不是程序验证。原记录保留以防误删；这是 MC-04 关闭后仍须保留的用户操作限制。
- **Portal Schedule：** Portal 可创建没有 Host 的文本 Schedule；Observation 仍要求 Host。内置浏览器用本机 MSW 合成账号实际提交一条无 Host 文本计划，列表显示已启用且标记“无（文本计划）”。浏览器验收发现 MSW `POST /api/schedules` 原先仍拒绝 `host_id: null`；handler 现只在 `observation == null` 时接受无 Host 计划，并新增 handler 用例验证 hostless Observation 被拒绝。MSW 合成的 `next_run_at` 固定为当前时间后 60 秒，不代表定时器对 `starts_on` 的真实调度；真实服务未在本轮连接验收。管理员注册/默认模型额度仍按会话 5 合同接入：注册只发用户名和密码，无邀请码或短信。
- **本轮 DSH 验证：** `pnpm run build`、`pnpm run typecheck`、`pnpm test` 全通过；Node 测试 435 项通过。新增测试覆盖 title/objective 分离、缺省 title、缺失响应 title 拒绝、未知旧 model 字段阻断、整小时计划 phase 导入、无效时间与超过微秒精度阻断、用户确认门槛、幂等键透传及断线重试复用、原旧计划不删除。未访问测试机、未使用提供的远端凭据/API key、未发送或上传文件。
- **本轮 Portal/Workbench 续验：** 会话 5 更新 `anchor_at` 最多 6 位小数秒的合同描述后，Portal 与 Workbench 均重生成 `src/api/schema.d.ts`。Portal `npm run typecheck`、`npm run lint`、`npm test`（45 个 Vitest 文件/378 项 + copy-mobile Node 测试 2 项）及 Vite 生产构建通过；schedule 列表使用独立 `title`，并分开展示 objective；小时计划列表和编辑预览显示 anchor，编辑周期或其它字段时原样保留 anchor。构建 bundle 为 790.77 kB，仍触发 500 kB 拆分提示。Workbench `npm run typecheck`、`npm run lint`、`npm test`（11 文件/113 项）和 `npm run build` 通过。本机 `127.0.0.1:5183/goals` 浏览器通过 MSW 实际提交一条仅文本计划，卡片显示标题、独立的执行内容、已启用状态、无主机标记及下次运行信息；此次数据只存在本机 MSW 进程，不代表真实云端调度或 anchor 浏览器验收，anchor 渲染与编辑保留仍由组件/API 测试覆盖。
- **交接 → 会话 4/5：** 会话 4 已完成 `every_n_hours.anchor_at` 服务端实现：创建/更新、响应与同步投影、持久化及停用/重启用相位恢复已按其报告验收；调度器选择严格晚于当前时间的首个 slot，跳过过期 slot，未传 anchor 继续使用创建/重启用相对周期。会话 5 冻结 OpenAPI，并收紧 `anchor_at` 为最多 6 位小数秒；会话 2 已把合同生成到 Portal/Workbench 类型。DSH 以源 `scheduledAt` 锚定，仅整小时且符合服务端精度时可导入；创建响应需确认 anchor 回显和 `next_run_at`，UI 展示返回的下次运行时间。会话 2 未改 `server/**`、未编辑其他会话清单。
- **正式 Desktop 边界复核（2026-11-03 快照，已由 2026-11-04 本机 JSON 路线解决入口缺口）：** 正式 `morde-client` 工作页没有读取 DSH session/Schedule 服务的本机 IPC；旧 `exportData()` ZIP 也不含 Schedule。当前路线不依赖这两者：DSH Harness 通过受保护的本机路由生成 Schedule JSON，Portal 在本机读取并逐项预览。现有 `ScheduleLike` 仍不能读取 enabled 状态或停用旧计划，故导入前由用户逐条停用并确认；这项不是程序验证。独立的 `project/morde-desktop` 仍不承载此次迁移。
- **Schedule JSON 合同与重试：** DSH 导出包含目标 Schedule 请求体与 `legacy-<sha256(mordePersonalScope + NUL + source_id)>` 幂等键；Portal 仅为用户确认的单条请求调用既有 API，未知结果重试沿用原请求和键。DSH exporter 与 Portal importer 都限制 UTF-8 文件不超过 10 MiB、Schedule 条目不超过 2000 条；Portal 对 `title_is_derived` 条目要求预览标题等于 `objective.trim()`，避免显示标题与服务端最终派生值不同。已完成本机回归，详见 2026-11-04 记录。
- **Schedule 源列表核验：** 已安装的 SessionController 合同声明 `list()` 返回全部可见 Session rows，没有分页游标；DSH Schedule 预览读取完整列表，本轮未发现分页遗漏。`rooms-morde-personal.test.mjs` 全文件通过（9/9），覆盖旧 `rooms.json` 预览/逐项导入与登出拒绝、Schedule 映射、Profile revision 和冲突保护。
- **会话 5 汇总交接：** `00` 与会话 5 清单已记录本机 JSON Schedule 路线，但仍列导出总大小限制、派生标题校验和 Portal 最终回归待会话 2 完成。本轮已全部完成并记录，详见 2026-11-04 Schedule JSON 导出与导入回归；请会话 5 在其文件边界内刷新 `00`/MX-04 的待验快照。会话 2 未修改 `00`、`server/**` 或其他会话清单。

### 2026-11-04 Portal rooms.json 导入续验

- **MC-04 / Portal 正式资料页入口：** 在“个人资料”页新增旧 `rooms.json` 本机预览和 Memory/Goal 逐项勾选导入。文件上限 10 MiB，每类最多 1000 项；无效、重复 ID 和不支持的 Goal 状态被跳过。未勾选条目不会进入 API 请求；不会上传整个文件、创建 Task、改写原文件或访问 nanoMuse Cloud。导入结果按选择文件时的 Morde 用户 ID 隔离；切换账号会停止后续提交并隐藏原账号的预览、状态和错误。
- **字段与冲突：** 旧记忆内容和旧 Goal 的标题、说明、状态、步骤映射到当前 personal object 合同。旧时间戳、Goal 分类/进度/摘要/活动、聊天、Feed、例程和 Library 不迁移，界面说明保留边界。Memory/Goal ID 与幂等键稳定派生自旧 ID；相同内容视为已存在，不同内容发生 ID 冲突时停止且不覆盖远端。
- **本地 MSW：** 补齐账号隔离的 Memory GET/PUT、Goal GET/POST/list、个人快照与变更游标，支持精确幂等重放和同键异请求冲突。此前内置浏览器提交遇到 Vite HTML 回退（MSW 缺少 importer 写路由）；现在用例覆盖写入、重放、冲突及账号隔离。
- **组件/API 验证：** Portal `npm run typecheck`、`npm run lint`、`npm test` 通过（47 个 Vitest 文件/398 项，copy-mobile Node 测试 2 项）；定向导入/MSW 测试 62 项通过；`git diff --check` 对本会话 Portal 文件通过。`vite build --outDir /tmp/morde-portal-rooms-build --emptyOutDir` 成功，811.40 kB chunk 仍触发 500 kB 拆分提示。Portal 原组合 `npm run build` 会继续构建边界外的 nanoMuse Web，因此本轮未运行该脚本。
- **内置浏览器：** `127.0.0.1:5191` + 本机 MSW 合成 demo 账号选择合成 `rooms.json`，预览后只勾选一条 Memory 和一条 Goal 并提交；Memory 列表及 `/goals` 仅出现已勾选记录。再选择同一 Memory 源 ID、不同内容的合成文件，页面提示 ID 冲突，原 Memory 内容仍在。没有连接真实 Morde 服务/测试机，也没有使用远端凭据、API key 或发送文件。
- **待办与交接 → 会话 4/5：** 本记录之后，会话 4 已在其清单将 MP-01/MP-02 标记完成，并交接会话 5 更新总览；会话 2 的 MC-04 现已关闭。旧 DSH 服务没有 enabled 读取或 disable 接口，因此旧计划停用仍由用户逐条确认，不能标成程序验证。会话 2 已实现 DSH 本机 JSON 导出、Portal 本机逐项导入及 rooms/Profile 资料导入；会话 2 未改 `00`、`server/**` 或其他会话清单。

### 2026-11-04 DSH Schedule JSON 导入续验

- **导出/导入门槛：** DSH 现在在返回 JSON 前限制最多 2000 条，并按 `JSON.stringify(bundle, null, 2)` 的 UTF-8 字节数执行 10 MiB 总大小限制，超限返回 413；与 Portal parser 两项相同。Portal parser 对 `title_is_derived` 项强制 `title === objective.trim()`，避免文件预览标题和 Schedule 服务派生标题不一致。均有回归用例。
- **DSH 验证：** `pnpm test` 全量 438 项通过；`pnpm run typecheck`、`pnpm run build` 通过。新增用例用 170 条各自有效记录构造超 10 MiB 导出，并用 2001 条记录验证数量门槛。
- **Portal 验证：** `npm test` 通过（47 个 Vitest 文件/399 项，copy-mobile Node 测试 2 项）；`npm run typecheck`、`npm run lint` 及定向 Schedule importer 12 项通过；`npx vite build --outDir /tmp/morde-portal-rooms-build-final --emptyOutDir` 成功，811.45 kB 主 chunk 仍超过 500 kB 拆分提示。`git diff --check` 通过。
- **本机浏览器：** 在 `127.0.0.1:5192/goals` 使用 MSW 与合成 `demo` 账号选择本机合成 Schedule JSON。页面显示完整源 ID、执行内容、频率、模型和预算；未勾选“旧端已停用并核对”时提交按钮保持禁用。勾选后只向本机 MSW 提交单条 Schedule 请求，返回“已提交”，列表出现已启用、无 Host 的合成计划。此流程没有禁用真实 DSH 计划，没有连接远端服务或测试机，也未使用给定凭据/API key、发送或上传用户文件。
- **交接 → 会话 4/5：** 会话 2 已完成本机 Schedule JSON 路线的限额与派生标题修复、Portal 最终回归和 MSW 浏览器验收。随后会话 4 已完成 MP-01/MP-02 所有者验收，并交接会话 5 更新 `00`/MX-04；会话 2 不再等待这两项验收。旧计划停用仍由用户逐条确认；本轮没有修改 `00`、`05`、`server/**` 或其他会话清单。

### 2026-11-04 DSH 不可迁移 Schedule 预览续验

- **缺陷与修复：** DSH 导出遇到空执行内容、过长标题/内容或无效时间/时区时，不再让 Portal 因整份文件解析失败而丢失其它记录。此类旧计划作为带原因的只读预览行导出，不附带迁移请求；有效计划仍按原合同校验，未截断后静默迁移。数量、单项预览文本和最终 JSON 大小均受限。
- **Portal 边界：** parser 仅对明确标记不可用且没有 `migration` 的行容忍旧字段无效；可导入行仍拒绝空内容和无效映射。页面显示不可导入原因，不提供该行的确认/提交操作。
- **验证：** DSH `pnpm run build`、`pnpm run typecheck`、`pnpm test` 通过（439 项）；Portal `npm test` 通过（47 个 Vitest 文件/402 项及 copy-mobile Node 测试 2 项），`npm run typecheck`、`npm run lint`、`npx vite build --outDir /tmp/morde-portal-session2-final --emptyOutDir` 通过；Schedule importer/Goals 定向测试 26 项、DSH rooms migration 定向测试 13 项通过。`git diff --check` 与本会话新增未跟踪源文件及本文尾随空白检查通过。生产构建仍报告最大 811.68 kB chunk 超过 500 kB 拆分建议。
- **本机浏览器：** `127.0.0.1:5193/goals` + 本地 MSW 读取合成的空内容/无效时间计划 JSON。页面保留该条记录并显示“无 prompt/objective”原因，检查确认没有导入或提交按钮；无请求写入 MSW。只用 `/tmp` 合成文件，随后已关闭本地 Vite 并清理文件及构建目录。
- **交接 → 会话 4/5：** 会话 4 提出的“坏行保留预览、只阻断单条迁移”要求已在本机会话处理和回归。随后会话 4 已在其清单完成 MP-01/MP-02 验收，并交接会话 5 刷新 `00`/MX-04；会话 2 已关闭 MC-04。旧 DSH 没有程序化读取 enabled 状态或停用计划的 API，旧计划仍需用户逐条核对/停用。Workbench 无本轮代码改动，沿用此前 11 个文件/113 项及类型、Lint、构建验收，未在本续验重跑。
- **边界与数据：** 仅修改会话 2 的 DSH、Portal 和本文；未访问远端测试机、未使用提供的凭据/API key、未连接真实服务、未上传/发送文件。本轮未修改 `00`、`04`、`05`、`server/**` 或其他会话文件。

### 2026-11-04 两项定向失败修复续验

- **DSH UTF-8 裁剪：** 定向输出发现超长旧标题/执行内容的预览字段已裁剪，但创建请求仍可能携带旧标题，且此前裁剪没有按字节边界处理。现在直接按 UTF-8 字节数组在完整码点处截断；只要预览文本被缩短，该记录就只保留在预览、不会生成 migration，创建请求标题也使用已校验的预览标题。测试改用中文与 emoji，检查两字段字节上限、无替换字符/残缺代理项并断言无创建载荷。
- **Portal stale migration payload：** 定向输出发现带有效 `unavailable_reason` 的行若仍含旧 fixture migration payload，会被整份 parser 拒绝。现在不可用行优先只读预览，忽略并从解析结果移除其 migration payload；可导入行的严格校验不变。parser 与组件用例覆盖带残留载荷的空内容/无效时间记录，验证不会显示提交入口或调用创建 API。
- **验证：** DSH `node --test tests/rooms-morde-personal.test.mjs`（13 项）、`pnpm test`（439 项）、`pnpm run typecheck`、`pnpm run build` 均通过。Portal 定向测试 26 项、全量 `npm test`（47 个 Vitest 文件/402 项及 copy-mobile Node 测试 2 项）、`npm run typecheck`、`npm run lint` 和 Vite 生产构建均通过；最大 811.58 kB chunk 仍有 500 kB 拆分提示。diff 与本会话未跟踪源文件空白检查通过。
- **本机浏览器：** `127.0.0.1:5193/goals` + MSW 载入带有效残留 migration 请求的合成 unavailable JSON。页面显示该行与原因，migration 内容未显示，且没有导入/提交按钮；未发生 API 写入。针对上轮失败状态重新独立运行断言（通过）与截图（单独输出）。上轮失败原因为测试断言误要求 payload 可见，属于验收断言写反，不是浏览器定位或产品行为失败。服务已关闭并清理合成文件及构建目录。
- **交接与边界：** 会话 4 提出的不可迁移行保留预览现已补齐失败分支；会话 4 后续已完成 MP-01/MP-02 验收并交接会话 5 更新总览。MC-04 已关闭；旧 DSH 无启用状态读取/停用 API，仍需用户逐条确认。仅改会话 2 的 DSH、Portal 与本文；未触碰 `00`、`04`、`05`、`server/**`、Workbench 或其他会话文件，未访问远端/发送文件。

### 2026-11-04 Portal MP-01 rooms/Profile 补充映射续验

- **完成项：** Portal rooms parser/UI 现在逐项预览并勾选 Feed 文本与偏好、当前静态 Ideas 标记、Library 名称/类型索引及 `docs.soul`；Profile 使用独立文件选择器，字段级勾选与当前 Profile 合并后按 revision CAS 写入。解析器从不打开 Library 文件路径或读取附件字节；请求体不含 Feed 图片/提示/会话 ID、Library 路径/描述/会话 ID或 Profile 未映射字段。页面明确标出旧 ZIP 未展开以及其中 `feed_posts.json`、`ideas.json`、`memory.db`、`goals.db`、`reminders.db` 继续留在原 ZIP。
- **映射与失败边界：** `docs.identity`、Feed routine（含开关/时间）、Diary、聊天和目标扩展字段保留在 `rooms.json`；未知/已移除 Ideas ID、无效条目和超出个人数据对象 ID 合同的旧 ID 不提交并计入预览跳过/不可映射说明。Profile 的 tagline、tone、communication、user_name、主动程度、间隔、quiet hours、自绘头像资源及不兼容风格保留在 `profile.json`。目标 API ID 只使用稳定摘要构造，源 ID 校验为 ASCII `[A-Za-z0-9._~-]`、最多 128 字符并拒绝 `.` 与 `..`。
- **本机浏览器验收：** `127.0.0.1:5194/personal` + 本机 MSW、合成 demo 账号与 `/tmp/nanomuse-rooms-mp01.json`；页面显示逐项勾选、`Identity 有文本，仅留在原文件`、`Feed routine 已开启，时间 08:00，仅留在原文件`、未知 Ideas 标记原因、Library 本机路径字段留源等预览信息。此续验仅选择文件并预览，未提交新写入；此前的本机 MSW 验收已实际导入合成 Feed/偏好/Ideas/Library/Soul 与 Profile 所选字段。未访问测试机或真实服务，未使用远端凭据/API key，没有向外发送或上传文件。
- **验证：** 定向 `npx vitest run src/routes/legacy-rooms-import.test.ts src/routes/PersonalData.test.tsx src/mocks/handlers.test.ts` 通过（3 文件/72 项）；Portal `npm test` 通过（47 个 Vitest 文件/412 项，copy-mobile Node 测试 2 项）；`npm run typecheck`、`npm run lint`、`npx vite build --outDir /tmp/morde-portal-mp01-final --emptyOutDir` 通过。构建最大 chunk 828.77 kB，仍有超过 500 kB 的拆分建议。未运行会额外构建边界外 nanoMuse Web 的 `npm run build`。`git diff --check` 及会话 2 文件无尾随空白检查通过；Vite、本机合成 fixtures 和构建临时目录已关闭/清理。
- **交接与状态：** 已按会话 5 集成审查对齐 personal-object ID 限制，新增非法空值、点段、斜杠、非 ASCII、尾随换行和超长 ID 测试；未改服务端/OpenAPI。会话 4 已将 MP-01/MP-02 标记完成并交接会话 5 更新总览，故 MC-04 现标记完成。遗留限制：旧 DSH Schedule enabled 状态无法经现有接口读取或停用，实际导入前仍由用户逐条核对/停用；真实服务未测；Portal 构建最大 chunk 为 828.77 kB，超过 500 kB 拆分建议。仅修改会话 2 的 Portal 与本文；未修改 `00`、`04`、`05`、`server/**` 或其他会话文件。

### 2026-11-05 PC/Portal Morde 显示名续验

- **用户可见改动：** Desktop 当前菜单、托盘提示、窗口/关于页、启动错误与 macOS 辅助权限说明使用 Morde；Splash/overlay 文档标题、helper 的 macOS 显示名及本机错误提示使用 Morde。DSH 会话显示、侧栏/关于/版本/欢迎标题、macOS 权限与桌面版诊断、默认助手名使用 Morde。Portal 无用户 Profile 时默认名为 Morde；导入 Soul 缺失名称时补为 Morde；灵感目录说明改为 Morde 内置目录。
- **兼容保留：** helper 的 `nanoMuse Computer Use.app` 输出目录/可执行文件名与 `io.github.nanomuse.desktop.computer-use` bundle ID 不变，以保留安装路径与 TCC 权限记录。旧 nanoMuse Cloud 服务名/入口、GitHub/官网地址、许可证与旧社区/法律来源、Portal 旧资料/文件格式导入标签，及包名、文件/目录、API/IPC、旧格式幂等前缀保持原样；Workbench 与独立 `project/morde-desktop` 无当前用户可见 nanoMuse 品牌，不作空改动。
- **验证：** Desktop `npm test`：274/274；DSH `pnpm test`：439/439、`pnpm run typecheck` 通过；Portal `npx vitest run src/routes/PersonalData.test.tsx src/routes/legacy-rooms-import.test.ts`：2 文件/20 项通过，`npm run typecheck` 通过。DSH 首次全量测试命中已重命名 splash 的旧品牌断言；将断言对齐新显示名后复跑通过。所有本轮修改代码文件 `git diff --check` 通过。未访问远端、未使用提供的凭据或 API key，未发送/上传文件。
- **交接：** 会话 2 的范围内已完成本轮 Morde 可见标签收敛；技术标识与旧数据来源仍保留，未需其他会话改合同。未修改 `00`、会话 1/3/4/5 文件、`server/**` 或共享公共合同。

### 2026-11-05 默认模型配置审计与 Assistant 门禁

- **当前合同：** 管理员 `/admin/settings` 的 `GET /api/admin/settings` / `PUT /api/admin/settings/model-budget-default` 配置首次审核账号额度，不是供应商/模型默认项。管理员 `/admin/models` 使用 `GET /api/admin/model-connections` 管理共享连接，能增删改和检查连接，但没有 `is_default`/默认模型路由。普通 Portal `GET /api/model-connections` 按 OpenAPI 返回共享连接及当前用户个人连接；`GET/PUT /api/me/preferences` 只管理该账号的 `selected_model_id`。`GET /api/onboarding/status` 返回当前用户 `model_status`、可用模型和 `can_submit`，其服务端语义也是有检查过的文本模型且用户已选模型，不是全局默认模型。
- **Portal 低风险调整：** `Assistant.tsx` 的 `readyModels` 现在同时要求 `shared`、`checked_at` 和文本能力；这与 `/connections` 已有只读团队模型列表保持一致，陈旧个人连接不会在 Assistant 绕过团队配置策略。没有可用共享文本模型时有效选择为空、显示配置提示且发送按钮禁用。没有改变每用户 selected model、AdminModels 共享连接配置或任何 API/schema。
- **验证：** `npx vitest run src/routes/Assistant.test.tsx` 通过（1 文件/38 项）；`npm run typecheck`、`npx eslint src/routes/Assistant.tsx src/routes/Assistant.test.tsx` 和代码 `git diff --check` 通过。新增回归覆盖个人/未检查/纯视觉模型不进入列表，且无已检查共享文本模型时发送不调用 `submitTask`。
- **交接 → 会话 5：** 目前没有可由 Portal 接入的管理员全局默认模型字段/接口。请确认需求是否真的要求平台指定单一默认连接；若是，请在会话 5 边界定义可持久化选择、并发更新、管理员读取/写入、用户侧就绪/不可用语义及 fail-closed 响应，再通知会话 2 接入。会话 2 未自造 endpoint、未改 `server/openapi/**`、API 或迁移；在合同到达前，MC-07 保持开放。
- **边界：** 仅改 `web/portal/src/routes/Assistant.tsx`、对应测试和本文。未访问远端/测试机、未使用凭据/API key、未部署或外发文件；未改 Workbench、服务端或其他会话清单。
