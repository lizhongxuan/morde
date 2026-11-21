# Morde PC 云端电脑统一工作台实施任务 TODO

来源需求：[Morde PC 云端电脑统一工作台需求](Morde-PC云端电脑统一工作台需求文档.md)

技术依据：[统一技术设计 §1 云端电脑统一工作台](Morde-PC与AI对话统一技术设计.md#1-云端电脑统一工作台设计)

## 修改边界

在 `morde-pc` 对话页使用当前选定且已授权的同一台 Host，提供桌面、终端、主机文件、VS Code 四个视图。切换入口只切换视图，不另建 Host；终端首次打开时按需建立会话，返回时复用。主机文件、终端和 VS Code 共用 Host 的 `/home/gem/workspace`。已有 `Workspace` 仍访问 Morde Workspace 存储，不与主机文件合并。

复用现有 Host 嵌入工作台、PC 原生嵌入桥接、AIO 文件接口及 code-server。保留 Host 所有者/共享成员权限、嵌入目标授权、请求来源校验和控制租约；Host 或权限不可用时显示实际原因，不自动切换 Host。按当前共享权限，文件和 VS Code 目标只提供给 Host 所有者；共享成员缺少目标授权时应显示受限状态，不能通过放宽前端或网关校验绕过。

## 接口变化

四个入口复用当前选定的同一 Host。现有 AIO 1.11.0 已能提供主机文件列表、读取、写入、上传和下载；目录创建、移动/重命名、删除尚无对应接口，处理方案见[统一技术设计 §1.5](Morde-PC与AI对话统一技术设计.md#15-aio-文件接口能力差异与处理方案)。

| 接口/模块 | 实施要求 |
| --- | --- |
| `POST /api/hosts/{id}/embed-tickets`、`openHostWorkbenchEmbed`、`select_target` | 由 `web/portal/src/routes/Assistant.tsx` 请求当前 Host 的授权目标；PC 原生桥接沿用 `morde-main.ts`、`morde-preload.ts`。切换只传目标，不更改 Host；校验返回的 Host、会话和目标范围。 |
| `PUT/POST /__ch/api/terminal-session`、`/terminal?session_id=…`、AIO `/v1/shell/ws` | 首次按需建会话并保留 session ID；已有会话复用。只有用户确认后才可在创建结果未知时新建，避免重复会话或重复命令。网关只开放终端目标，不开放通用 `/v1/shell/exec`。 |
| AIO `/v1/file/list|read|write|upload|download` | `web/workbench/src/app/fileApi.ts` 使用 AIO 1.11.0 实际提供的文件接口；网关按 `files` 目标和精确方法授权。 |
| 文件夹创建、重命名和删除 | 当前 AIO 1.11.0 未提供对应 API。若保留文件面板内的 CRUD，须实现受 `files` 目标授权保护的 Host 内部文件操作接口，并限制在 `/home/gem/workspace`、拒绝路径越界及符号链接逃逸；不拼接 Shell 命令。短期可在同一 Host 的 VS Code 资源管理器完成这些操作。 |
| `/code-server/?folder=/home/gem/workspace` | `web/workbench/src/app/Workbench.tsx` 在同一 Host 打开 code-server；网关只对 `vscode` 目标、合法浏览器请求和有效控制租约放行。 |

## 实施任务

执行顺序：T1 → T2、T3 → T4 → T5。

- [x] T1（完成；自动化验收已有记录）收敛嵌入网关目标授权
  - 修改：`server/internal/gateway/embed_auth.go`、`embed_auth_test.go`。为 `files` 目标放行所需 AIO 文件操作；为 `vscode` 目标放行 code-server 浏览器方法和合法 GET WebSocket；桌面 noVNC 仅接受必要参数。
  - 验收记录：覆盖目标授权、HTTP 方法及非法 WebSocket；未授权目标、TRACE/CONNECT、通用 Shell API 均拒绝；控制租约、来源和路由限制保留。

- [ ] T2（部分完成；AIO 1.11.0 缺少目录创建、移动/重命名、删除接口，依赖 T1）接通主机文件操作
  - 修改：`web/workbench/src/app/fileApi.ts`、`FileManager.tsx` 及对应测试/mock。先保留 AIO 已提供的浏览、编辑保存、上传下载；若保留文件面板内的目录创建、移动/重命名、删除，则实现并接通设计 §1.5 所述的受限 Host 文件操作接口。重命名遇到同名目标不得覆盖；删除须确认；不以 Shell 拼命令。
  - 验收记录：现有组件/mock 测试只验证调用路径和交互，没有核验 AIO 真实接口。2026-11-21 本地 AIO 1.11.0 `/v1/openapi.json` 未列出 `/v1/file/mkdir`、`/v1/file/move`、`/v1/file/delete`；实际 `POST /v1/file/delete` 返回 404 `Not Found`。浏览、读写和列表的实际 AIO 调用已通过；Host 内目录变更接口仍未完成。

- [x] T3（完成；组件/API 验收已有记录，Host 联验见 T5，依赖 T1）接通终端与 VS Code
  - 修改：`web/workbench/src/app/Workbench.tsx`、`web/workbench/src/api/gateway.ts` 及对应测试。终端按需建会话并复用；创建状态未知时先要求用户确认；VS Code 指向同一 Host 的工作目录，失败可重试并显示原因。
  - 验收记录：用例覆盖会话复用、未知结果保护、失败重试及 VS Code 路径/加载状态；Host 交互由 T5 验收。

- [ ] T4（进行中，部分完成，依赖 T2、T3）在实际 `morde-pc` 接通并验收四入口
  - 修改：核对 `web/portal/src/routes/Assistant.tsx` 的 Host/目标传递和状态提示，以及 `clients/desktop/harness/desktop/src/morde-main.ts`、`morde-preload.ts` 的嵌入与票据交换；修复后更新对应 Portal 和桌面桥接测试。遵循 Morde UI 规范。
  - 验收：启动并操作实际 `morde-pc`，从对话页依次切换四入口；确认 Host ID 不变、入口权限正确、加载/失败状态可见、切换不重复启动 Host 或建立终端会话。不得以独立网页或构建结果代替客户端验收。
  - 实际客户端记录（2026-11-20，源码版复测）：在实际 `morde-pc` 的同一既有对话、同一 Host 工作台内切换四个入口。桌面显示 Chromium；主机文件面板列出 `/home/gem/workspace` 内容；终端页面可打开但视口持续黑屏；VS Code 页面可打开但视口持续白屏，后两项仍不可验收。接管先显示切换中；切到任务动态再返回后接管完成，VNC 从只读变为可控；归还后四入口禁用并显示接管提示。没有运行远端命令、上传或修改远端文件。
  - 本地实现进展（2026-11-20）：`confirmFrameResponse` 检查同源页面响应和终端 `[aria-label="Terminal input"]` / VS Code `.monaco-workbench` 就绪标记；错误状态区分网络失败、HTTP 状态码、应用未就绪和页面加载超时，并保留重试。网关访问日志只记录固定类别 `/terminal`、`/v1/shell/ws`、`/code-server`，不记录查询参数或动态路径。Workbench 定向测试 29/29、TypeScript 检查、生产构建及 `go test ./internal/gateway` 通过。此改动尚未部署或进入测试 Host 提供的页面，不能据此判定终端黑屏或 VS Code 白屏已修复。
  - iframe 迟到回调修复（2026-11-20）：终端/VS Code 仅接受当前 iframe 实例的加载结果，终端会话切换会更换 iframe 实例。Workbench 定向测试 31/31、TypeScript 检查通过；详细实现记录见[统一技术设计 §13](Morde-PC与AI对话统一技术设计.md#云端工作台-iframe-旧加载结果隔离2026-11-20)。这只修复 UI 状态竞态，不能证明实际 Host 的黑屏/白屏已解决。
  - 本轮原生视图故障处理（2026-11-20）：Portal 更新 Electron 原生视图 bounds 时若桥接拒绝、抛错或未返回 `updated: true`，现在关闭该原生视图并显示明确错误，不再静默保留空白面板。`Assistant.test.tsx` 两条相关用例通过，`npm run typecheck` 通过。该处理只覆盖窗口布局失败，不证明终端黑屏或 VS Code 白屏由此导致或已修复。
  - 本次终端页面显示已有会话参数，但未核对 PTY 是否复用或实际会话数；未验证目录读写、VS Code 编辑、权限不足和 Host 离线错误态。沿用客户端现有登录会话，没有对云端目录或 Host 权限做操作。
  - 账号处理：按用户授权，仅重置测试站 `admin` 的密码哈希；数据库回读确认目标记录匹配，角色/状态未变，强制改密标志保持关闭。未记录明文密码，未修改其他账号、远端服务配置/二进制、云端目录或权限。
  - 本地 AIO 对照（2026-11-20）：用项目固定的 AIO Sandbox 1.11.0 镜像和回环端口启动一次性容器。直接打开 `/terminal` 能显示 shell 提示符；`/code-server/?folder=/home/gem/workspace` 在空容器里先提示目录不存在，仅在本地容器创建空目录后编辑器正常渲染。此对照证明 AIO 独立页面可工作，但没有经过 Morde 网关，也没有验证文件读写或跨入口一致性；实际 Host 的黑屏/白屏更需检查代理后的 WebSocket、Host 运行镜像和嵌入页面状态，尚不能归因。
  - 测试站只读诊断（2026-11-20）：Portal 服务为 active，Host 容器为 healthy；Gateway 日志中终端会话创建接口返回 200。24 小时日志另有 7 次 WebSocket 503，均未带 `upstream_prefix`；该字段只在 Host/控制状态准入后写入，说明这些请求未到 AIO 代理阶段，但日志仍不能识别入口。Caddy 到 Gateway 的 5 次 502 集中在控制状态查询和根页面，也不足以归因到终端或 VS Code。重新打开的实际 `morde-pc` 当前未接管 Host，云端电脑入口被禁用；未再次获取控制权、未运行云端命令、未改远端配置或文件。黑屏/白屏根因仍待从本地隔离 Host 的实际客户端流程复现。
  - 账号恢复复核（2026-11-20）：按用户本轮授权恢复测试站 `admin` 登录；临时凭据只在内存中使用，最终改密经 Portal `/api/me/password` 完成，接口返回 204；随后用新凭据登录返回 200、`must_change_password=false`，验证会话注销返回 204。密码未写入文档、日志摘录或文件，其他账号和业务数据未改。
  - 账号状态复查（2026-11-20）：用户再次报告测试站 `admin` 停在强制改密页且忘记当前密码，并授权重置该账号。随后通过 SSH 在 PostgreSQL 事务中只更新 `name=admin` 的 bcrypt 密码哈希；回读并用应用 bcrypt 实现核对新口令匹配。角色仍为 `super_admin`、状态仍为 `approved`、`must_change_password` 仍为 `false`；未修改其他账号或字段，未把明文口令写入文件或日志。此前打开的浏览器会话可能仍显示旧状态，需重新登录刷新。
  - 下一步：把刚构建的本地 Host 镜像接入隔离 Gateway 与实际 `morde-pc`，复现终端/VS Code 黑白屏及 503 准入问题并定位根因；再补终端会话复用、目标权限、Host 离线状态和控制权旧连接排空验收。未完成客户端联验前，T4 保持进行中；不向测试站上传构建产物。
  - 已有本地回归记录：`cd clients/desktop/harness/desktop && npm run build:test`、`node --test tests/morde-core.test.mjs`（19/19）。该结果不代替本项客户端验收。

  - 实际客户端补验（2026-11-21）：在独立本地 profile 登录获准测试站后，云端电脑页可连接已有 Host。未接管时四个功能入口禁用；接管后“终端”切换到可访问树中的 Web Terminal，“主机文件”默认路径为 /home/gem/workspace，Workspace 仍是另一存储入口。VS Code 按钮可切换为选中态，但辅助功能树没有出现 code-server/Monaco 页面；窗口截图接口也未能捕获画面，因此 VS Code 是否实际渲染仍未验证。切换期间仍在同一工作台 WebArea；Host ID 和服务端创建计数未单独核对。归还控制权、退出登录后删除独立 profile。没有发送新对话、执行终端命令、上传或修改 Host 文件。T4 仍未通过，后续须验证 VS Code 页面和终端交互/会话复用。
  - 实际客户端复测（2026-11-21，源码版与临时 profile）：在当前源码构建的 `morde-pc` 登录测试站并打开已有 Host。桌面可显示 Chromium；主机文件面板加载 `/home/gem/workspace` 目录列表；终端切到带会话 ID 的页面但画面持续黑屏；VS Code 面板持续白屏，辅助功能树只出现 VSCode 容器，没有编辑器节点。接管成功、四个入口可用；归还后入口禁用，随后退出登录并关闭隔离客户端。现有凭据可登录，因此未执行密码重置或数据库写入。没有运行远端命令、打开文件内容、上传或修改 Host 文件。运行期间本地 Electron 主进程多次报告 `Invalid Host Workbench state`；测试 Host 部署版本与当前本地桥接版本不一致是可能原因，但尚未证实它与黑屏/白屏的因果关系。T4 仍未通过；需在本地隔离环境使用同版本 Host、Gateway 和 `morde-pc` 复现并分别检查终端与 code-server 页面/WebSocket，再决定修复点。
  - 实际客户端诊断复测（2026-11-21，隔离源码客户端）：接管后桌面可显示 Chromium；主机文件面板列出 `/home/gem/workspace` 下目录；终端 iframe 指向 `/terminal?session_id=…`，画面持续黑屏，尝试输入只读 `echo MordeTerminalOK` 后没有可见回显，无法确认命令是否执行；VS Code 面板持续白屏，辅助功能树未出现 code-server URL 或 Monaco 节点。控制权已归还、账号已退出，隔离客户端进程已关闭；隔离临时目录移入本机废纸篓待清理。未读取文件内容、修改 Host 文件或上传文件。此结果仍不能区分 Gateway 准入/ WebSocket 问题与测试 Host 部署版本差异，T4 未通过。
  - 桥接启动竞态修复（2026-11-21）：`Workbench.tsx` 仅在收到 embed 初始化且当前会话 ID、Host ID 均解析后发送状态，避免原生桥接把启动期的空 `conversation_id`/`host_id` 状态按身份不匹配拒绝。`Workbench.test.tsx` 覆盖初始化前不发送状态；定向测试 31/31、TypeScript 检查通过。Electron 桥接诊断错误改为只列出无效字段名，不输出字段值；定向桥接测试 11/11。该修复消除初始化期误报，但不证明或修复远端终端黑屏、VS Code 白屏。
  - 终端会话复用回归（2026-11-21）：四入口切换用例现从 VS Code 返回终端后断言 `ensureTerminalSession` 仍只调用一次，验证入口切换复用现有 PTY 会话。Workbench 定向测试 31/31、TypeScript 检查通过；真实 Host 的会话计数仍待 T4/T5 联验。
  - 控制交接稳定态门禁（2026-11-21）：桌面、终端和 VS Code 现须等接管状态稳定、操作租约由当前账号持有且待处理操作清空后才可用；交接期间每秒刷新状态，顶部状态和操作按钮显示“接管中/交还中”，不会提前声称用户已取得控制权。Workbench/TopBar 定向测试 44/44、TypeScript 检查通过。该门禁避免 UI 在网关仍拒绝交接中请求时提前连接，但未复现或证明修复既有 Host 的终端黑屏和 VS Code 白屏；T4 仍未通过。
  - 隔离联验前置检查（2026-11-21）：本机已有的两个 `morde-pc` 窗口分别连接本地占位服务和另一个用户会话，未借用它们；macOS 屏幕录制权限也拒绝截图。随后改用独立源码客户端 profile 联验，详见下一条。
  - 源码版 PC 测试站联验（2026-11-21）：由当前工作树构建并启动独立 `morde-pc`，登录获准测试站账号后打开一个已有 Host。初始 noVNC 桌面可访问；接管成功后桌面、终端、文件、VS Code 四个按钮均可切换，随后成功归还控制权。终端切换到 `主机终端` 容器，辅助功能树未出现 Terminal input；文件管理器加载目录列表；VS Code 切换到 `VSCode` 容器，但辅助功能树未出现 Monaco/code-server 节点或页面错误，等待 10 秒仍无变化。由于截图被 macOS 权限拒绝，本次无法直接观察终端黑屏或 VS Code 白屏画面；与先前的黑屏/白屏复测相符，但不能据此定位根因。切换仍发生在同一个对话工作台内，noVNC 会话显示 `gem@a398fcc189b5`；未单独读取 Host ID 或服务端创建计数。Electron 主进程退出时另记录 `Invalid Host Workbench state`（`conversation_id`/`host_id`），与测试站当前嵌入页版本不一致是可能原因，因果未证实。此次 Host 工作台由测试站返回的 embed URL 加载，未部署本地改动，因此没有验证本轮交接门禁是否能修复测试站的嵌入页。已归还控制权、退出登录、关闭客户端并删除临时 profile；未发送对话、运行终端命令、打开文件内容、上传或修改 Host 文件。T4 仍未通过；T5 文件跨入口一致性及失败边界未验收。
  - 测试账号恢复（2026-11-21）：按用户明确授权，仅重置测试站 `admin` 账号密码哈希。事务校验目标记录恰好一条；回读确认角色 `super_admin`、状态 `approved`、强制改密标志关闭。登录接口返回 200、登出返回 204；验证期间产生的临时会话均已注销/撤销。未改其他账号、权限、服务器文件或其他会话；密码明文未写入文档或日志。
  - 测试站 PC 联验（2026-11-21）：使用独立临时 profile 登录成功（客户端登录 200），该账号仅有一台运行中的 Host。接管后四个入口均可切换：桌面显示 Chromium，主机文件加载 `/home/gem/workspace` 目录列表；终端创建会话接口返回 200，但没有后续 `/terminal` 或 Shell WebSocket 网关请求，界面无 Terminal input；VS Code 容器出现，但没有 `/code-server` 请求或 Monaco 节点。归还控制权、登出（204）、关闭隔离客户端并删除临时 profile；未发消息、运行命令、读取或修改文件。日志证据指向终端/VS Code 嵌入内容未发起加载请求，但尚不能区分客户端嵌入桥接、远端页面版本或其他加载问题。本地 AIO 页面确认 Terminal/Monaco 就绪选择器在 1.11.0 中存在（见 T5），降低了源端选择器不匹配的可能性，但仍未定位测试站无请求问题。T4 仍未通过，T5 的文件跨入口一致性和失败边界未验收。
  - 当前工作树定向复核（2026-11-21）：`web/workbench` `Workbench.test.tsx` 32/32、`web/portal` `Assistant.test.tsx` 207/207；两包 TypeScript 检查通过；Electron `morde-main-portal-bridge.test.mjs` 与 `morde-main-path.test.mjs` 14/14，`npm run build:test` 通过。覆盖本地目标切换、终端会话复用、iframe 错误状态和原生桥接校验；不覆盖测试站实际嵌入页面的资源请求，因此 T4 仍未通过。

- [ ] T5（部分验证，依赖 T4）验证同 Host 文件一致性和失败边界
  - 修改：优先准备本地隔离、可丢弃的 Host/AIO；若发现问题，在对应 Workbench、PC 桥接或网关模块修复并补相应用例。
  - 验收：仅在本地隔离 Host 创建临时文件，经终端、主机文件面板和 VS Code 交叉读取及修改；再验证 Host 不可用、控制权失效和目标授权不足时不切换 Host、不执行未授权操作，并显示失败原因。
  - 早期进度（截至 2026-11-20）：项目锁定的 AIO 1.11.0 镜像在本地隔离容器中能显示终端提示符与 VS Code 编辑器；本地 AIO 经 Morde 网关的手工检查中，终端和 code-server 页面 HTTP 为 200、对应 WebSocket 为 101。当时尚未创建测试文件或做入口交叉读写；本轮后续检查见下文。网络层结果本身不证明嵌入页面已渲染，也未验证 Morde `morde-pc`、网关授权或失败边界。
  - 本地 AIO 实际页面与共享目录检查（2026-11-21）：在一次性隔离容器中，Terminal 页面显示 shell 提示符并出现 `[aria-label="Terminal input"]`；文件 API 写入的合成文本文件出现在 VS Code Explorer，打开后 Monaco 显示相同内容；终端执行只读 `cat` 也读到该内容。AIO 文件读、写、列表实际请求成功，确认这三个入口在 AIO 内指向同一工作目录；VS Code 编辑保存、Morde Gateway/PC 嵌入、权限及离线失败态未验证。AIO 没有删除接口，合成文件由本地隔离容器清理后再销毁；容器、网络及临时截图已清理。T5 仍未完成。
  - 本地 Host 共享目录探针（2026-11-20）：当前工作树构建的 arm64 Host 镜像在临时隔离容器中启动；鉴权后的终端页、VS Code 页请求成功，终端 WebSocket 握手返回 101，AIO 文件 API 写入 Workspace 后可由 `gem` shell 读回，测试文件在退出前删除。仅覆盖 Host/AIO 文件系统层，不覆盖 Gateway、实际 PC/VS Code 编辑器、code-server WebSocket 与权限/离线失败边界；T5 继续部分验证。
  - 环境阻塞记录（2026-11-20）：当时本机 Portal/Vite 未运行，项目指定 Lima 隔离 VM 不存在；标准启动脚本禁止自动新建空 VM。本轮后续已本地构建并启动 Morde Host 镜像，但 Portal/Vite、隔离 Gateway 与实际 `morde-pc` 联验环境仍未就绪；此处不再把“缺少本地 Host 镜像”作为当前阻塞。
  - 早期环境路径复核（2026-11-20）：只读检查 `scripts/start-macos.sh`、`.local/morde-local/start.sh`、Compose 配置及本机容器。标准本地启动要求既有 `morde-f01-u2604` VM；该实例不存在，7080/5173/5174 均未监听。当时 Docker 只有 PostgreSQL 容器，没有运行中的 AIO、OpenSandbox 或 Morde Host，也没有可直接复用的本地 Host 镜像。后续已按下文构建并验证本地 Host 镜像，但没有补齐 Portal/Vite、隔离 Gateway 与实际 `morde-pc` 联验环境；未创建替代 VM，也未操作外部 Host。
  - 本轮 PC 窗口复核（2026-11-20）：macOS 无截图权限，本次只检查辅助功能树。打包版 `morde-pc` 只暴露空白 WebArea；源码运行版能渲染本地验收对话，但云端电脑面板显示“尚未配置默认云端电脑”，桌面、终端、文件、VS Code 和接管按钮均禁用。没有操作测试站 Host、运行云端命令或改动云端文件。此结果解释了本轮无法从本地客户端完成 T4/T5，不定位打包版白屏原因，也不证明目标 Host 四入口可用。
  - 本地 Host 镜像补验（2026-11-20）：使用 `images/host/build.sh` 从当前工作树构建仅留在本机 Docker 的 `local/morde-cloudhost:verify-20261120`（linux/arm64），`images/host/test-runtime-bootstrap.sh` 网络隔离烟测通过。扩展的一次性本地探针确认 AIO 鉴权后的 `/terminal?session_id=…` 和 `/code-server/?folder=/home/gem/workspace` 页面均 HTTP 200，终端 WebSocket 握手返回 101；AIO 文件 API 写入隔离 Workspace 的合成文件后，UID `gem` 的 shell 能读回相同内容，随后删除文件并由脚本清理容器和卷。该镜像没有上传；探针没有经过 Morde Gateway/PC，关闭了 Chromium，没有验 code-server WebSocket、iframe 实际渲染或控制权限失败态，因此 T4/T5 仍未通过。已有证据把下一步收敛为用本地 Host 镜像接入隔离 Gateway 和实际 `morde-pc`，定位网关准入/嵌入问题；无需为四个入口另建 Host。
  - 限制：不得向外部 Host 上传或创建测试文件。若没有本地隔离 Host/AIO，本项保持未验证，不以生产 Host 代替。
