# Morde PC 端统一设计方案

2026-11-15。设计方案（草稿，待用户确认）。依据：origin/main 264b5f04 实测核对 + 《PC 端统一需求》D1–D6 已决定。证据等级：✅ 表示已从仓库实测核实，❓ 表示待实施时核实，不作为已证实的事实。

## 1. 目标与已定决策

一句话：**PC 客户端是唯一的用户端，界面资源全部打包进客户端，服务端只提供 API；Web 端只保留管理后台。**

已定决策（2026-11-15，见需求文档）：

- **D1 内嵌打包：** PC 客户端默认打包好所有界面资源，不从服务端获取资源；与服务端交互只走必要的 API 接口。
- **D2 工作台全部保留：** 五个标签（桌面/终端/Workspace/主机文件/VS Code）都不删减，打包进 PC 客户端。
- **D3 删除手机 Web /app/：** 手机上只留 iOS 原生 App；对话在 PC 端照常可用。
- **D4 下载与登录：** PC 客户端下载临时放在 Web 管理后台；用户登录在 PC 端和 iOS 端做，管理员登录在 Web 端。
- **D5 下线后响应：** 浏览器访问已下线的用户地址直接报 404。
- **D6 先对齐再删除：** PC 对齐验收通过后，再删除用户 Web 端。

设计原则：打包优先；API-only（除 API 与网关票据外，客户端不直接访问服务端页面）；单用户面（浏览器里没有用户界面）；数据不动、会话不断（不迁移数据库、不强制重新登录）；先对齐再删除。

## 2. 现状（✅ 已核实）

| 部分 | 现状 | 位置 |
|---|---|---|
| PC 客户端 | Electron 壳（morde-client 0.1.0，Electron 44.7.0，electron-builder 26）；WebView 加载 `${auth.origin}/assistant`，即服务端下发的 `web/portal`；点 /admin 时在外部浏览器打开 | `clients/desktop/harness/desktop/src/morde-main.ts:2238,2303` |
| 用户 Web | `web/portal` 是 SPA，同时装用户路由（/assistant、Projects、Hosts、Sites、Connections、PersonalData、GoalsAndSchedules、Codex 等）和管理后台路由（/admin/*）；构建产出 dist/index.html、dist/admin/index.html，手机 Web 再打包进 /app/ | `web/portal/src/App.tsx`、`web/portal/package.json`（build 脚本含 mobile 构建与 `copy-morde-mobile.mjs`） |
| 工作台 | 独立应用，五个标签；服务端由 `gateway.workbench_dir` 下发；进工作台靠一次性票据（60 秒过期），网关重定向后设 HttpOnly、Secure、SameSite=Strict 的 Cookie（作用域 /__ch/）；任务观察另有票据接口 | `web/workbench/src/app/Workbench.tsx`、`server/openapi/portal.yaml`（/api/tasks/{id}/workbench-observation-ticket） |
| 手机 Web | `web/mobile`（nanomuse-web：聊天、目标、想法、记忆、审批），构建后拷进 portal dist 的 /app/ | `web/mobile/package.json` |
| 管理后台 | 同一个 portal 包里的 /admin/* 路由 + 服务端 /admin/* API（hosts、workspaces、users、capacity、audit、overview、usage 等） | `web/portal/src/App.tsx:99-104`、`server/internal/api/admin.go`、`admin_console.go` |
| 服务端静态服务 | `PortalFiles: os.DirFS(cfg.PortalWeb.Dir)`；`portal_web.dir` 与 `gateway.workbench_dir` 都是必填配置 | `server/cmd/portal/main.go:790`、`server/internal/config/config.go` |
| 登录态 | PC 客户端凭证存 Electron safeStorage（含 origin、serviceInstanceID、accountName、installationId、过期时间）；有 `client-login` / `client-web-session` / `client-logout` 三个客户端专用接口 | `clients/desktop/harness/desktop/src/morde-main.ts`、`server/openapi/portal.yaml`（/api/auth/*） |
| 客户端发版 | `dist` 脚本已存在（mac dmg、win nsis、linux AppImage）；`electron-updater` 6.8.12 已在依赖中 | `clients/desktop/harness/desktop/package.json` |
| 客户端本地页面先例 | PC 客户端已有本地页面加载（`loadFile(resources/morde.html)`、`loadFile(resources/codex.html)`） | `clients/desktop/harness/desktop/src/morde-main.ts:3128,3183` |

根因（同需求文档）：PC 聊天界面就是服务端下发的 `web/portal`，PC 客户端只是壳；过去“改 PC”全是在改 `web/portal`，验收又只在浏览器里看，所以改动只落到了 Web 端。

## 3. 目标架构

```text
PC 客户端（Electron）
  ├─ 打包的用户界面（portal 用户入口：聊天/任务/审批/文件/主机/设置…）
  ├─ 打包的工作台界面（桌面/终端/Workspace/主机文件/VS Code）
  ├─ 原生壳（同步引擎 morde-sync、Codex 会话、登录态 safeStorage、自动更新）
  └─ 只调 API ──→ 服务端
iOS 原生 App ──只调 API──→ 服务端
浏览器 ──仅──→ 管理后台 Web（/admin/*）＋ 临时下载页
服务端
  ├─ API：/api/*（auth/me/hosts/codex/tasks/model…）、/__ch/*（网关票据与主机 API）、/mcp
  ├─ 管理后台 Web：仅 dist/admin 入口
  └─ 不再服务：用户 Web 界面、/app/ 手机 Web、工作台页面
```

变化点清单：

1. 服务端不再向 PC 下发用户界面；`portal_web.dir` 改为仅管理后台产物。
2. `web/portal` 构建拆分：用户入口改为客户端打包输入，管理后台入口继续服务端下发。
3. `/app/` 手机 Web 下线；用户路由对浏览器返回 404。
4. 工作台 dist 改作客户端打包输入，不再向浏览器提供。
5. PC 客户端与服务端之间只剩 API 与网关票据两种交互。

## 4. 详细设计

### 4.1 PC 客户端打包（✅ 机制已存在，待接线）

- **构建输入：** `web/portal` 的用户入口产物 + `web/workbench` 的 dist，作为桌面客户端构建的输入。electron-builder 已有 `dist` 脚本（mac dmg、win nsis、linux AppImage），在其上加一步“打包 portal 用户产物 + workbench 产物”（放进 `extraResources` 或 asar 内资源，实施时定）。
- **加载方式：** `morde-main.ts` 的 `openAssistant` 从 `loadURL(${auth.origin}/assistant)` 改为加载本地打包文件。已有先例：客户端已用 `loadFile` 加载本地页面（`resources/morde.html`、`resources/codex.html`）。登录态不动（credentials 已在 safeStorage）。
- **API 地址注入：** 打包界面不再用相对路径 `/api`；启动时由客户端注入 API origin（即 `auth.origin`，`portalOrigin()` 已有格式校验：HTTPS，或本地开发时 HTTP localhost）。开发期 vite proxy（/api、/mcp → 127.0.0.1:7080）保持不变。
- **版本与更新：** `electron-updater` 6.8.12 已在依赖中，❓ 是否已接通更新源、更新检查与安装流程是否跑通过，待实施时核实。界面随客户端发版；客户端旧版调新 API 需保持兼容，或提示升级（兼容策略实施时定）。

### 4.2 API 交互（API-only 契约）

打包界面允许调用的服务端接口（✅ 均已存在于 `server/openapi/portal.yaml`）：

- 认证：`/api/auth/client-login`、`/api/auth/client-web-session`、`/api/auth/client-logout`（另有浏览器用的 `/api/auth/login`、`/register`、`/logout`，用户 Web 下线后浏览器侧不再用）。
- 账户与偏好：`/api/me`、`/api/me/preferences`、`/api/me/password`、`/api/me/invite-code`、`/api/me/records`、`/api/nudges`、`/api/onboarding/status`、`/api/me/default-host`。
- 主机与云端电脑：`/api/hosts`、`/api/hosts/{id}`、`/control`、`/release`、`/restore`、`/usage`、`/copies`、`/control/lease/renew`。
- 对话与任务：`/api/model/v1/chat/completions`、`/api/capabilities`、`/api/hub`、`/api/templates`、任务流与事件（SSE）。
- Codex：`/api/codex/sessions`、`/api/codex/connections`、`/api/codex/sessions/{id}/events/stream`、`/api/codex/commands/*`。
- 工作台票据：`/api/tasks/{id}/workbench-observation-ticket`；网关：`/​__ch/api/*`、`/​__ch/task-auth`。

禁止项：界面资源（HTML/JS/CSS/图片/字体）不再从服务端拿；除上述 API 与网关票据外，不直接访问服务端页面。流式接口（SSE）保持；服务端不可达时界面显示明确的断网提示，不静默失败。

### 4.3 登录与会话

- **用户登录收进客户端：** 打包的登录页 → `POST /api/auth/client-login` → 凭证存 safeStorage（现有机制，含过期判断 `credentialExpired` / `credentialExpiresSoon`）。注册同样收进客户端（调 `/api/auth/register` 或客户端注册接口，实施时定）。
- **工作台会话：** `/api/auth/client-web-session` 换取 `web_session_credential` → 网关一次票据（60 秒过期）→ HttpOnly Cookie（作用域 /__ch/）→ 打开工作台。打包后流程不变，只是工作台界面来自本地。
- **管理员登录：** Web 端 `/admin/login` 不变；PC 客户端点 /admin 仍走外部浏览器打开（现有行为不变）。
- **登出：** `/api/auth/client-logout`，清本地凭证；管理后台登出走现有 Web 流程。

### 4.4 工作台打包

- **打包范围：** 只打包工作台的界面资源（`web/workbench/dist`）；运行时数据仍走网关 API（`/​__ch/api/*`）与票据流；桌面/终端画面来自主机网关的流，不是静态资源，打包解决不了流本身，只解决界面从哪来。
- **网关地址：** 按主机票据取（`gatewayURL` 校验已存在：`/​__ch/auth?t=…&next=…`，仅限 https 或本地 hosts 域名）。
- **❓ 待实施核实：** 打包界面以 `file://` 起源加载时，票据 Cookie（SameSite=Strict，作用域 /__ch/）是否正常工作。如有问题，备选方案是客户端内置本地静态服务（127.0.0.1 随机端口）加载打包界面，保持 http 起源。
- **验收：** 五个标签逐个在真 PC 上跑通（桌面画面、终端输入、Workspace 浏览、主机文件、VS Code 打开）。

### 4.5 管理后台 Web

- **构建拆分：** portal 构建拆出管理后台入口（`dist/admin/index.html` 已存在，另有 `/admin/` 历史路由回退插件）；服务端只服务该入口，用户入口不再构建、不再服务。
- **下载页（D4，临时）：** PC 客户端安装包由管理后台提供下载。桌面端已有 `stage:portal` 脚本（`scripts/portal-downloads.mjs`），❓ 它现在产出什么、与下载页如何衔接，待实施时核对。
- **PC 客户端点 /admin：** 保持现有行为（外部浏览器打开，PC 回到助手页）。

### 4.6 服务端改动

- **停止提供用户界面：** 用户路由对浏览器返回 404（D5）；`/app/*` 返回 404；工作台页面不再向浏览器提供。
- **配置：** `portal_web.dir` 改为仅管理后台产物；`gateway.workbench_dir` 不再向浏览器提供工作台页面，构建产物改作客户端打包输入。两个配置项是否保留、校验逻辑如何改，实施时定。
- **API 不变，数据不迁移。** `update.sh` 的 Web 产物警告改为只检查管理后台产物（用户 Web 已不存在，不再警告）。

### 4.7 移动 Web 下线

- portal 构建不再执行 mobile 构建与 `copy-morde-mobile.mjs`；`/app/*` 返回 404；iOS 原生 App 覆盖聊天、目标、想法、记忆、审批。

### 4.8 客户端更新机制

- 发版：electron-builder（dmg / nsis / AppImage）+ `electron-updater`（❓ 接通情况待核实）。
- 更新检查、下载、安装流程与提示文案，实施时按现有更新机制补齐；如现有机制未接通，先以管理后台下载页人工更新为临时方案（与 D4 一致）。
- 版本号与 API 兼容：客户端发版号与服务端 API 兼容范围要写进发布检查（新客户端可调旧 API，旧客户端调新 API 至少不崩溃或明确提示升级）。

### 4.9 兼容与数据

- 不动数据库，不动会话；已登录的 PC 客户端升级后凭证保留（safeStorage）。
- 旧书签（/assistant 等）→ 404；已收藏的下载页地址如变化，在管理后台下载页保留说明。

## 5. 实施计划

### 第 0 步：冻结与基线（不动代码）

- 定“PC 与 Web 一样”的基线：页面清单（需求文档第 3 节表格）+ 聊天页按《PC 聊天页实现规范》的 20 项截图验收。
- 冻结期间不再往用户 Web 端加新功能，只修阻塞项。

### 第 1 步：PC 端对齐（先对齐，再删除）

1. portal 构建拆分用户入口与管理后台入口；用户入口产物改作客户端打包输入。
2. 桌面客户端打包用户界面 + 工作台界面；`openAssistant` 改加载本地文件；API origin 运行时注入。
3. 登录/注册收进客户端；工作台票据流按 4.4 核实（先测 `file://` + Cookie，不行改本地静态服务）。
4. 客户端更新机制核实并接通（electron-updater）；如未接通，先以管理后台下载页人工更新为临时方案。
5. 真 PC 上验收（见第 6 节）。

准出：PC 客户端内对话、任务、审批、文件、主机、工作台全流程跑通，与当前用户 Web 端逐页一致。

### 第 2 步：删除用户 Web 端

1. 服务端停止提供用户界面与 /app/（返回 404）；工作台页面不再向浏览器提供。
2. portal 构建不再产用户入口与 mobile 产物；`update.sh` 的 Web 产物警告只检查管理后台产物。
3. 更新部署文档（`deploy/single/README.md`）与安装前置检查。

准出：浏览器访问任何用户端地址均 404；管理后台完整可用。

### 第 3 步：收尾验证

1. PC 客户端从登录到对话、任务、审批、交付全流程跑通；关闭重开后会话、任务状态不断。
2. iOS 端回归一遍，不受影响。
3. 管理后台回归：登录、各管理页面、下载页可用。

## 6. 验收标准（全部满足才算做完）

1. 真实 PC 客户端内，用户功能与删除前的用户 Web 端逐页一致；聊天页按实现规范 20 项截图验收全过。
2. 浏览器访问任何用户端地址返回 404；`/app/*` 404；/admin/ 完整可用。
3. PC 客户端登录、对话、任务、审批、文件、主机、工作台五标签全流程跑通；重启、断网重连后状态不断、不重复执行。
4. iOS 端回归通过；管理后台下载页能下到最新 PC 客户端安装包。
5. 部署文档与更新脚本已同步改掉，不再要求用户 Web 产物。
6. 客户端从旧版（服务端下发界面）升级到新版（打包界面）后，已登录凭证保留，无需重新登录。

## 7. 风险与待核实项

- ❓ `electron-updater` 是否已接通更新源、更新流程是否跑通过（依赖已在，接线未核实）。如未接通，第 1 步要补接通，或先以管理后台下载页人工更新过渡。
- ❓ 打包界面以 `file://` 加载时，工作台票据 Cookie（SameSite=Strict，作用域 /__ch/）是否正常工作。备选：客户端内置本地静态服务。
- ❓ `stage:portal` 脚本（`scripts/portal-downloads.mjs`）现在产出什么，与 D4 的管理后台下载页如何衔接。
- portal 构建拆分用户/管理入口的工作量：vite 多入口已存在，但拆分后用户入口不再进服务端产物，构建脚本与部署检查要同步改。
- 客户端发版节奏变慢：界面改动不再随服务端部署走，每次改界面都要发客户端版本。这是 D1 已接受的代价。
- 管理后台下载安装包的签名与校验方式，实施时定（与现有发版流程一致）。

## 8. 与在途工作的关系

- 《PC 端统一需求》是本文档的决策来源（D1–D6 已全部决定）；本文档是它的实施设计。
- 《PC 聊天页实现规范》不变，它就是第 1 步的验收标准；品牌色“先不动”的决定（2026-11-15）继续有效。
- 《优化需求文档》P0–P2 不受影响，仍按原来的排期；本方案只改“界面归谁承载”，不改功能清单。
- 服务端重部署被 00082 迁移挡住一事与本方案无关，各走各的；但第 2 步的服务端改动（停止提供用户界面）要在重部署恢复之后再上，避免两个服务端改动叠在一起。
