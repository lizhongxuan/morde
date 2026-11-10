# 2026-10-22 本地 Morde Host Workbench 验收（当前）

- 修复 noVNC 键盘输入失败：Gateway 现在识别固定 AIO 使用的 QEMU ExtendedKeyEvent（类型 255、子类型 0），仍经 Host fence 授权。修复交回 AI 时 noVNC 在控制转换期间过早重连的竞态；交接稳定后才切到只读连接。
- 修复终端 404：Workbench 原先在 AI 只读态请求 `/v1/shell/terminal-url`，被 Gateway 按控制权限正确拒绝后把 404 错误保留，接管后不再重试。现在终端只在用户接管稳定后取 URL；AI 态显示“请先接管电脑后使用终端”，接管完成自动加载。交回 AI 时清除终端 URL 并卸载旧 iframe，避免用过期 session 重连；下次接管会获取新 URL。
- Workbench TODO：T05.17 四个工作区入口、切换保留和权限状态通过；T05.27 退役入口清理通过；T05.28 同源 code-server 在用户接管态可用、AI 状态禁用通过。真实 Host 上浏览器显示 Chromium、键盘打开 `chrome://version` 后恢复 `chrome://newtab`；终端 `pwd` 返回 `/home/gem/workspace`；文件管理浏览到 `Downloads`（空目录）；VSCode Web 加载 `/home/gem/workspace`。交回 AI 后 noVNC 只读重连成功。没有写入、上传或删除 Host 文件。
- 验证：Workbench lint、typecheck、66 项单测、生产 build、Playwright E2E 6/6 通过（端口 5197）。E2E 覆盖 AI 态不请求终端 URL、接管后自动连接并运行终端命令，以及交回 AI 后卸载终端 iframe；内置浏览器真实 Host 再次执行 `pwd` 返回 `/home/gem/workspace`，随后成功交回 AI 并确认只读态不再挂载终端 iframe。此前 Server Gateway/API 定向 Go 测试与 race 测试、`make lint`、Linux arm64 Portal 构建通过；Portal `/healthz`、`/readyz` 为 200。
- 本地 Lima 已更新 Workbench bundle `index-DnjIlycv.js`，SHA-256 `943a6b5644a950398620dfaa3b4a75330e52710dc5d3ece3d7fad1fb65f1b4a1`；上一个 dist 备份到 `.rollback/20261022-2113-terminal-access-fix-v2/workbench-dist`。Portal 二进制保持 SHA-256 `28d9f28b3afa94fa79a90344d92ec7c14fcbc22384e56422ef46ff825996a6cf`。Host `hk3rikyw` 保持运行，控制权已交回 AI。
- 限制：本轮只验证本地 Docker Host；隔离 Linux/runsc、PVC 恢复、全量 Portal 创建回归和完整 Host generation 连续性未覆盖。真实文件上传未执行。

---

# 规划容量展示复核（2026-10-20）

只改Workspaces页面与既有测试，列名改为“规划容量”，说明当前没有单目录硬配额。Portal Node24 lint/typecheck/test（26项）/build通过，内置浏览器重载实际服务后已看到新列名与完整说明；保留的合成Workspace we4htluw为detached。此处只修正展示，未声称实现磁盘限额。

# 最终构建复验（2026-10-20）

根任务将Linux Portal切换到冻结源码构建，SHA256 fbf66ef872d3fe3f776731287cba94ddf5064aee869da895be03f681d1794d01；真实Playwright单机流程再次通过，测试36.7秒、整体38.1秒。首次重跑在“正在连接终端”时撞上测试默认5秒断言；仅将该断言等待对齐已有API 30秒上限后通过，生产超时和文件写入/读回成功断言未改。临时Host按精确ID清理。

# 当前结论：内置浏览器核心流程验收完成（2026-10-20）

实际主流程：Portal登录→唯一模板创建→running→一次性ticket Workbench→VNC显示MCP导航页面→终端键盘命令写文件→文件编辑保存中文/MCP读回→删除保留Workspace→新Host同卷重挂→私有preview页面/资源/WS HMR。HMR模块计数1→2且页面标题变化，没有刷新浏览器；后台Vite在shell_exec结束后持续服务。

内置浏览器还完成 mkdir→rename、实际文件下载，下载字节与中文编辑内容一致。截图仅在本机 `.local/builtin-file-final.png`、`.local/builtin-cjk.png`、`.local/builtin-hmr.png`。未选择或上传用户文件；真实上传遵守禁止外发文件要求未执行，小文件上传和覆盖/大小/失败边界由mock与单测覆盖。

Node24两端lint/typecheck/test/build通过；Portal26项、Workbench63项。Portal mock12项、Workbench mock4项与真实单机Playwright1项通过。管理员跨owner的“进入”按钮已禁用并解释；后端签票同样仅限owner，不增加代入权限。后文旧Node26、旧测试计数、mock-only及待真实链路描述为历史阶段，以本节为准。

---

# 本轮新增：真实 Playwright 全链路通过（2026-10-20）

Node v24.19.0，`CH_REAL_E2E=1 npm run e2e -- --grep 'real single-host flow'`：1 passed（12.3秒）。实际连接 Portal/Gateway/PostgreSQL/固定 OSB/runsc/AIO，UI 创建、等待运行、ticket进入、终端写文件、文件列表与内容读取成功，finally删除自己的Host/Workspace。真实测试改为检查终端写出的文件及内容，避免把Canvas终端的DOM文本当输出；清理请求明确携带 Origin/JSON。

# 本轮新增：真实 Workbench 文件与终端（2026-10-20）

内置浏览器经 portal.localhost 创建 builtin-basic-20261020（hva6h4rh），一次性 ticket 进入 Workbench。真实终端执行 printf，写入 terminal-builtin.txt；MCP 读取文件成功。MCP 打开合成页面，VNC 显示同一页面并保持连接。

文件页首次报“目录列表响应无效”：固定 AIO 的目录项 size:null，现有校验错误拒绝。新增真实形状回归先失败后通过，统一类型允许 nullable size/extension；保留非法类型、负值、非有限数和必要字段校验。修复后内置浏览器列出目录/文件，size:null 显示 —；编辑 MCP 写入文件并保存中文内容，MCP 再读回一致。Workbench Node24 lint/typecheck/test（63项）/build 和 mock E2E 4/4 通过。证据仅存本机 .local/builtin-*.png。

# 本轮新增：Node 24 门禁（2026-10-20）

使用 Node v24.19.0 重跑两个项目 lint/typecheck/test/build 全部通过；Portal 单测 25、Workbench 单测 59。隔离本地端口运行 mock Playwright：Portal 12 passed、1 real-host 用例因未启用而 skip；Workbench 4 passed。mock 的 VNC proxy ECONNREFUSED 不作为真实 VNC 成功证据。

# 05 Web 前端验证记录

> 日期：2026-10-20
> 范围：基础版门户与工作台的自动化门禁、mock 流程及有限真实 Portal 登录验收。下方“历史”小节保留此前版本记录，不代表本轮功能或验收状态。

## 本轮首版最终验收

| 验收 | 结果 | 证据 / 限制 |
|---|---|---|
| Portal lint / typecheck / unit / build | 通过 | 本机 Node `v26.8.2`；Vitest 9 个文件、25 项通过。 |
| Workbench lint / typecheck / unit / build | 通过 | 本机 Node `v26.8.2`；Vitest 8 个文件、59 项通过。 |
| Portal Playwright | mock 通过；真实主机用例跳过 | mock E2E 12 项通过。另 1 项真实单机主机流程跳过，未创建或操作真实 Host。 |
| Workbench Playwright | mock 通过 | E2E 4 项通过；终端通过 synthetic iframe 键盘输入验证。Gateway/AIO VNC 代理目标 `127.0.0.1:7081` 未运行，测试覆盖明确的服务不可用状态，不代表真实主机工作台通过。 |
| 内置浏览器真实 Portal 登录 | 部分通过 | 登录成功并打开主机页；主机尚未就绪，未进入真实 Workbench，未验证真实 Browser/VNC 链路。 |
| Portal→Host→Gateway→AIO Browser 真实全链路 | 待验证 | 需要真实且运行中的 Host 与 Gateway；本轮 mock E2E、独立固定 AIO 工具验证都不能替代该链路。 |

所有测试只使用本地 mock 和合成数据；没有选择、读取或上传用户文件，没有修改远端主机。完整真实 Browser 链路仍未完成。

## 历史自动化验证（前序版本）

| 验收 | 结果 | 证据 / 限制 |
|---|---|---|
| A05.01 | 测试通过 | 最终 Portal 测试 28/28、Workbench 测试 59/59；两端 lint/typecheck/build 有此前通过记录。新增 Gateway 类型客户端的路径、请求 ID、Cookie 凭据、错误映射与 nullable 到期时间回归也有测试覆盖。 |
| T05.02 | 部分完成并验证 | 契约 `server/openapi/portal.yaml` 与两端生成的 schema 文件已存在；Workbench 使用 `openapi-fetch` 类型化 `GET /__ch/api/host`、`POST /__ch/api/renew`、`POST /__ch/api/agent-info`，相关请求 ID、Cookie 凭据、错误码、`Retry-After` 与 nullable 到期时间回归通过。Portal 业务调用仍使用泛型 `apiRequest<T>(path)`，AIO 维持运行时校验且未从 mock 推断缺失 schema。 |
| A05.02 | 通过 | shell 转义用例 5 项通过，覆盖恶意文件名。 |
| A05.03 | 通过（mock） | Portal Playwright 14 项全部通过：主机生命周期/≤3 次点击、登录错误、Key/令牌一次性显示、管理员权限与管理页、工作目录保留/重挂载；另覆盖管理员创建/禁用用户、禁用后登录拒绝、审计筛选、owner 隔离、quota/capacity/workspace_busy、handoff 通知及本地 URL 守卫。Workbench 3 项通过，覆盖顶栏续期、文件 API 字段、目录操作、文件读写/上传/下载和 Agent 信息关闭后重新请求。 |
| A05.04 | 通过（mock） | 创建到打开 Workbench 统计为 2 次点击（上限 3 次）。 |
| A05.07 | 部分通过（mock） | Playwright 收到下载 HTTP 200，断言同源路径、文件名和下载字节；内置浏览器显示“下载已开始”。真实 AIO 目录与终端的一致性仍待验。 |
| A05.08 | 部分通过（不传输大文件） | mock E2E 用合成小文件验证上传；fileApi 单测验证超 100 MB 在创建 XHR 前拒绝、模拟 75% 进度回调；组件测试验证 64% 进度可见、超限提示在目录刷新后仍可见且不调用上传函数。未做真实 100 MB 拖拽上传。文件选择器曾出现非预期 mock 条目，之后未再使用选择器。 |
| A05.05–A05.06 | 未运行 | 真实流程需 Portal API / gateway / AIO；本地工作区没有这些服务，本轮未连接测试主机、未改远端状态或传输文件。另：A05.06 当前脚本只在终端标签显示后用 `page.request` 调 `/v1/shell/exec`，没有通过终端 iframe 键盘输入命令，因此不能证明终端 UI 可执行命令。 |
| A05.09 | 部分通过（Safari 本地 mock） | Safari 下完成 demo 登录、创建主机、暂停/恢复/续期、令牌签发/列表/吊销，并确认删除弹窗默认勾选保留工作目录。未进入真实主机或验证真实同源 iframe Cookie；Chrome/Firefox 的真实主机流程未验。 |
| A05.10 | 本机浏览器通过，其他入口待联调 | 本机 AIO 用项目既有 `seccomp=unconfined` 参数和 API Key 启动；Workbench 本地代理转发 `/vnc` 与 `/websockify`。内置浏览器显示 noVNC 桌面，并通过键盘输入打开本地测试页；未访问外网。Gateway 实现已交付，但本轮未运行 Portal API + Gateway 真实集成，终端/VSCode 与正式网关路径仍待联调。 |
| T05.27 | 通过（本地 mock/UI） | Workbench 主界面视觉收敛；3 个主入口、VSCode 次级入口、文件 README 读取、浏览器 502 重连提示和终端 mock 提示均在内置浏览器实测；未保存或上传文件。 |
| A05.11 | 本地键盘流程通过 | 内置浏览器键盘遍历 Portal 主导航与创建主机表单（返回、名称、规格、工作目录、取消、创建），以及 Workbench 主导航与文件页（路径、打开、隐藏文件、刷新、上传、建目录、README、下载、保存、文件内容）；Enter 可打开 README。焦点轮廓可见，表单字段有可访问名称；既有对比度抽查为 4.71:1–8.58:1。未做完整 WCAG/读屏审计。 |
| A05.12 | 通过（mock 结构） | Playwright 断言两种配置的 JSON 字段、授权头及剪贴板内容；未连接真实 MCP endpoint 或用配置启动 Agent。 |

验证命令：`env CH_REAL_E2E=0 CH_PORTAL_URL=http://poison.example CH_USERNAME=ignored-user CH_PASSWORD=ignored-password PATH=/Users/zhongxuan_li/.cache/codex-runtimes/codex-primary-runtime/dependencies/node/bin:$PATH bash deploy/single/verify/05-automated.sh`（Node `v24.19.0`），退出码 0；Portal E2E 14 项、Workbench E2E 3 项通过；真实单机测试显示为 SKIP。最终 Portal 28/28、Workbench 59/59 单测通过；两端 lint/typecheck/build 有此前通过记录，shellQuote 5 项通过。Vite 配置单测确认 mock 模式不代理 API、`VITE_MOCK_API=0` 时恢复代理。Playwright 使用隔离端口 5183/5184，禁止复用用户开发服务；mock 子进程清除真实流程标记、URL 和凭据变量，即使调用环境含无效外网 URL mock 仍使用隔离目标。门禁过程出现指向未运行 gateway/API 的预期代理连接拒绝，但 mock 测试全过。未连接或修改测试主机。所有 `deploy/**/*.sh` 通过 `bash -n`；ShellCheck 当前不可用，因此未执行本轮 ShellCheck。

本次 Workbench 验证：`npm run lint && npm run typecheck && npm test && npm run build && npm run e2e` 全部通过；Workbench 单测 8 个文件/59 项，E2E 3 项。E2E 的本地 VNC/VSCode 代理仍记录指向未运行 Gateway 的连接拒绝，页面现在会明确展示浏览器不可用状态；真实模式未运行。

全量 Portal E2E 日志曾出现一次 TanStack Query 的 `templates` 查询返回 `undefined` 警告；单独重跑相关错误流程用例未复现，尚未确认触发条件，未影响门禁结果。

## 历史内置浏览器验收（前序版本）

本节中关于暂停/恢复、Host Token、handoff 通知、VSCode 与 Agent Info 的部分记录了旧版行为。2026-10-22 按当前需求恢复了受 Gateway 控制的 VSCode 工作区；以下历史记录不代表当前权限合同。

使用 Codex 内置浏览器访问 `http://127.0.0.1:5173/` 和本机 Workbench `http://127.0.0.1:5174/`。本轮以 `demo/demo` 登录 Portal，创建本地 mock 主机并等到 Running，按默认勾选“保留工作目录”删除；Hosts 列表清空后转到工作目录页，目录自动从 attached 变为 detached，再删除目录并确认列表为空。另在内置浏览器验证登录后整页刷新仍保持登录，登出后回到登录页。Workbench 打开 Agent Info 查看两种配置，关闭再打开后刷新信息；文件页列出并读出 mock README。既有浏览器验收还覆盖 skip link 键盘导航、保存 README、创建目录、触发下载及配置复制；Safari 本地 mock 验收覆盖主机生命周期、令牌及默认保留目录。内置浏览器本轮没有选择或发送真实用户文件；Playwright 只用合成内容请求本机 mock。

本轮再次在内置浏览器确认 Portal 使用 `demo/demo` 登录后打开主机页；Workbench 本机页显示主机状态、续期入口、文件列表、README 内容与编辑控件。工作台打开文件面板并成功读取 mock README；未上传文件。另用 Portal mock 账号 `handoff/handoff` 验证未读移交通知横幅、示例主机与工作目录；点击“进入主机”打开本地 Workbench 新标签，点击“标为已读”后通知横幅消失。本次在新建的本机 Workbench 标签读取 mock README 成功，未选文件或上传。

本次 Gateway 客户端更新后，在独立内置浏览器标签确认主机信息加载、续期成功提示及 Agent 信息弹窗中的平台 MCP/主机直连配置；到期字段允许为空时显示“到期时间未知”。当前没有连接 Gateway，浏览器使用 Workbench 本地 mock；未复制令牌、选择文件或上传文件。

本次 API 代理配置变更后，在新建的内置浏览器标签打开本机 Portal 主机页与 Workbench 文件页，读取 mock `README.md` 成功；未上传文件、未注销共享 Service Worker。旧 Workbench 标签的 502 原状态已不存在，因此只复验新标签，不声称原故障已彻底复现或修复。

内置浏览器的一个旧 Workbench 标签在文件列表请求时显示 `AIO 返回了无效响应（HTTP 502）`，点页面内“刷新”仍失败；整页重载后再次打开文件页恢复，新建本机标签也能正常列出并读取 README。Playwright 新浏览器上下文的自动化测试通过。Portal 旧 API Key 标签也曾在重载后显示服务暂不可用，而新标签登录后正常。只读审查发现两端 API 路径原先在 mock 模式仍代理到真实服务，现改为仅 `VITE_MOCK_API=0` 时代理；配置变更后本地 `GET /api/me` 返回 Vite SPA HTML，`POST /v1/file/list` 返回本地 404，未转发到 7080/7081。这样可阻止 Service Worker 漏拦时请求落到真实后端。旧标签故障现场响应和 SW 状态已丢失，故障根因与旧标签恢复仍未确认。未注销共享浏览器的 Service Worker。

本轮内置浏览器新标签通过 `demo/demo` 登录；创建 `browser-smoke-local` mock 主机并等到运行中，点击“进入主机”后进入本地 Workbench，文件页读出 README 并创建临时目录。随后删除该 mock 主机和保留的空工作目录，门户工作目录列表回到空态；重载 Workbench 后确认 mock 文件树复位、README 仍在且临时目录不再显示。未选择或上传用户文件。工作目录请求失败/重试由 Portal 单测覆盖，并未在浏览器中人为断网模拟。

曾点击文件选择控件检查焦点，随后 mock 列表出现一条 2.6 MB 文件的上传成功记录。未打开或读取其内容，也未传到测试主机；重载后 mock 列表复位，源文件未修改。为遵守本地文件边界，没有继续选择文件或做 100 MB 上传。

Playwright 的下载验证确认同源 `/v1/file/download` 返回 HTTP 200，路径、文件名和文件字节正确；内置浏览器亦显示“下载已开始”。真实 AIO 的下载目录、工作目录和终端三方一致性仍待联调。切换浏览器、终端、文件和 VSCode 标签后，浏览器 iframe 元素仍保持挂载；因缺少 gateway/AIO，本轮内置浏览器中的远程 iframe 显示连接错误，真实 VNC、终端、VSCode 内容与会话稳定性仍待联调。

2026-10-20 本次内置浏览器走查：打开 `http://127.0.0.1:5174/`，确认浏览器、终端、文件为三个主入口，VSCode 在右侧作为次级入口；浏览器代理返回 HTTP 502 后显示“浏览器服务暂不可用”及“重新连接”，重试后仍保留明确错误；终端 mock 显示需连接真实 Gateway，不再递归载入工作台；文件页列出并打开 mock `README.md`。未保存或上传文件。另查看本机 AIO `http://127.0.0.1:18084/vnc/index.html?autoconnect=true`，noVNC 画面为黑屏；容器日志显示 Chromium namespace 操作被拒并以 `rc=-6` 退出，seccomp filter 已启用但具体拒绝源未隔离。尝试 AIO 自带浏览器 hard-restart 接口未在 30 秒内返回；未改容器安全选项或连接测试主机。

2026-10-20 本机浏览器修复：先以原容器文件系统创建本地快照，再用项目基线中的 `seccomp=unconfined` 与随机 AIO API Key 重建 `morde-aio-demo`，保留原本机工作区；验证无密钥 VNC 返回 401、经 Workbench 服务端代理返回 200，AIO 截图为有效 PNG。内置浏览器中的 noVNC 显示 Chromium 新标签页，通过 noVNC 键盘输入打开本地 `data:` 测试页；一次快捷键测试把合成测试字符串误识别成 Google 搜索词，没有用户文件或凭证，随后已返回本地页面。未传输文件。Vite 代理只在 `.env.local` 配置回环地址时注入密钥，非回环目标会被拒绝；密钥不在浏览器响应中，`.env.local` 已忽略。Mock Playwright 独立清空这两个本机配置变量，仍按无 Gateway 场景测试。

2026-10-20 最终 mock 流程复验：在内置浏览器创建主机并等待状态变为 Running，点击进入 Workbench，读取 mock `README.md`，新建再删除临时工作目录；随后重置 mock 测试数据，主机与临时目录均不残留。该流程验证的是本地 mock UI，不代表 Portal API、Gateway、数据库或 OpenSandbox 的真实集成。

## 历史本地环境记录（本轮不作为当前功能状态）

- 门户开发服务：`http://127.0.0.1:5173/`，mock 账号 `demo/demo`。本轮未启动，当前连接失败；上一轮记录的 200 不代表当前服务在线。
- 工作台开发服务：`http://127.0.0.1:5174/`；本机 AIO `morde-aio-demo` 已健康运行在 `127.0.0.1:18084`，当前工作台浏览器页经本机代理连接 noVNC。`web/workbench/.env.local`（已忽略）保存回环 AIO 地址和 API Key；服务重启后继续使用该本机配置。
- mock 门户支持主机生命周期、工作目录、API Key、主机令牌、管理员页面，以及动态配额/容量统计和超限反馈；普通 `demo/demo` 账号不带 handoff 通知，使用 `handoff/handoff` 可在本地查看关联示例主机、工作目录和未读通知并完成进入/标读流程。主机生命周期及一次性密钥/令牌显示已走通。
- 可用 mock 账号：管理员 `demo/demo` 或 `admin/admin`；普通用户 `member/member`；带 handoff 演示数据的用户 `handoff/handoff`。管理员还可在本地创建/停用用户并筛选合成审计记录。
- mock 工作台支持文件浏览与编辑、目录创建/重命名/删除、小文件上传与下载、Agent 配置展示与复制。上传批次串行，上传前检查隐藏项并在覆盖时确认；mock 按 `show_hidden` 切换隐藏项并保留二进制下载字节。保存或上传期间会禁用会改变文件目标、目录或列表的操作；旧目录异步操作不会刷新当前列表。本机浏览器已接入 AIO noVNC，且可键盘交互；文件页仍为 mock，终端与 VSCode 尚未接入真实 Gateway 服务。

## 前序未验收边界（由本轮最终状态更新）

- `server/openapi/portal.yaml` 已进入当前工作区且 T03.01 lint 通过；两端生成 schema 文件在位。Portal 业务调用仍走泛型 `apiRequest<T>(path)`，路径/成功响应没有受 `openapi-fetch` 约束；Workbench 仅三个 Gateway 控制面路径已类型化。后续迁移 Portal adapter，并在固定 AIO 合同明确后再评估 AIO 类型生成。
- mock 模式漏过 MSW 的 API 请求现在由 Vite 本地处理，不会代理到真实后端；原旧标签 502 现场已丢失，仍需复验标签级 Service Worker 恢复行为。
- MSW 浏览器 mock 通过 `document.cookie` 保存模拟会话，不能设置 HttpOnly；真实 Portal API 必须提供并验证正式 Cookie 安全属性，本地 mock 验收不代表真实登录安全属性已通过。
- T05.13 的通知横幅、进入和标读已由本地演示账号、Playwright 与内置浏览器验证；真实 handoff 事件生产/投递仍待 03/06 联调。
- 本机 AIO 的 noVNC 会话已通过；AIO 下载、终端地址、VSCode 会话、文件/终端/工作区一致性及 Portal API 到真实 Gateway 的 Cookie 流程仍未验收。Gateway 代码与 httptest 已实现，但没有运行真实 Portal+Gateway 主机链路。
- 本机 AIO 使用 `seccomp=unconfined`，即容器未启用默认 seccomp 系统调用过滤；容器仍非 privileged、无主机目录挂载、仅回环端口发布并启用 API Key。正式主机隔离与 Gateway 路由尚未验收。
- 本轮没有连接测试机；真实单机流程未运行，没有传输文件或修改远端状态。
- 真实 100 MB 拖拽上传、真实主机下 Safari/Firefox 的 Cookie/会话验收和完整 WCAG 审计未执行；Safari 本地 mock 主流程已部分验收。
- 内置浏览器 Workbench 旧标签曾偶发 HTTP 502，页面内列表刷新不能恢复，整页重载或新开标签可恢复；mock API 代理现已 fail-closed 并在新标签复验。原 Service Worker 故障根因与旧标签恢复仍未定位。
