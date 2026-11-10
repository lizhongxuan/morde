# Morde 基于 nanoMuse 多端客户端：精简设计与重复能力决策

> 客户端授权与 Web/PC 功能边界以[登录即授权精简方案](Morde客户端无感设备授权精简设计方案.md)为准：用户只登录，不输入设备码或二次批准；本机目录同步只在 PC 客户端提供。

> 2026-10-26，实施中的产品与技术决策版。详细接口、安全和首次安装流程见[完整版设计](Morde基于nanoMuse的PC与手机端二次开发设计方案.md)，逐项开发与验收见[实施 TODO](实施/22-Morde基于nanoMuse多端客户端实施任务todo清单文档.md)，业务范围见[多端需求](Morde多端托管与共享工作区需求文档.md)。本文的“关闭/删除”指 **Morde 发行版**，不表示现在删改了源码。用户已明确这是新项目，不做旧桌面客户端迁移或并行运行。
>
> 基线：此前核对的 nanoMuse v1.0.7 具有桌面 Electron/dsh 外壳、Web、Android、iOS、本地 MuseAgent、Sentinel、Cloud/Hub；[上游架构](https://github.com/nano-muse/nanoMuse/blob/v1.0.7/docs/architecture.md)、[桌面说明](https://github.com/nano-muse/nanoMuse/blob/v1.0.7/docs/desktop.md)、[Hub 说明](https://github.com/nano-muse/nanoMuse/blob/v1.0.7/docs/hub.md)可复核。本地 `project/nanoMuse` 已克隆，当前 `main` 为 `22eba3f`，仍须核对它与此前参考版本的差异、构建入口和许可；`project/openmuse` 并非替代源码。Morde 当前能力以本地 [OpenAPI](../server/openapi/portal.yaml) 和代码为准。

## 一、结论

Morde 最终用 nanoMuse 提供**桌面外壳和手机交互基础**，用 Morde 现有后端提供**身份、Host、Workspace、任务、审批、通知、画面与控制权**。用户只有一个 Morde 账号、一套任务历史、一套工作区和一套授权。nanoMuse 的 Cloud/Hub、本地 Agent 等重叠后端能力不接入首版 Morde 业务链路。

判断一项重叠功能归属的规则：

1. 需要跨端持续、审计、权限或资源版本的状态，以 Morde 服务端为准。
2. 只影响本机呈现、窗口、文件选择、安全存储的能力，优先复用 nanoMuse 客户端。
3. 相同名称但对象不同的功能必须分名展示，例如 nanoMuse Hub“设备”不是 Morde Host；本机同步不是云端 Task。
4. Morde 目前仅有需求、尚无服务端实现的能力，不能拿 nanoMuse 本地实现冒充“已跨端完成”。

## 二、nanoMuse 与 Morde 重复能力如何决策

| 重叠项 | 两边现状 | Morde 版决策 | 代码/界面处理 |
|---|---|---|---|
| **账号、Cloud、设备登录** | nanoMuse 用 Cloud 账号让各 Muse 设备互通；Morde 有 Portal 用户会话与 Workspace Sync 设备凭证 | **Morde 身份唯一**。桌面/原生端在应用内输入账号密码，成功后自动登记安装并签发独立客户端凭证；桌面同步仍用受限设备凭证 | 删除手输设备码和浏览器二次批准页；替换 Cloud API，关闭上游 Cloud 自动连接、账号切换与回退 |
| **Agent、聊天和任务** | nanoMuse 的 MuseAgent 在设备/本地运行自己的会话与工具循环；Morde 已有 Conversation、Task、Agent、SSE 和 Host 绑定 | **Morde Task 唯一**。PC 直接复用 Portal Assistant/Task 页面；原生手机可复用 nanoMuse 聊天外观，但只展示 Morde Task | 移除 Morde 版的本地 Agent 自动执行入口，不复制 nanoMuse 会话数据库或把 Hub task 转成 Morde Task |
| **审批与安全拦截** | nanoMuse Sentinel/RiskGate 决定其本地工具动作；Morde TaskAction 和 Gateway 控制权决定云端动作 | **按执行位置决策**。云端 Task 只认 Morde 审批版本和权限；首版不运行本地 Agent，因此不显示第二套审批卡 | 复用卡片外观可行；事件、审批 ID、风险描述和批准结果均接 Morde，不调用 nanoMuse Sentinel 代批准 |
| **“设备”和远程能力** | nanoMuse Hub 把手机、电脑当 Muse 设备，通过 relay 执行其自身任务；Morde Host 是云端运行环境，客户端安装与同步设备另有身份 | **三类对象分开**：云端电脑 Host、本机客户端、本机 Workspace 同步设备 | Host 页只查 Morde Host；原 Hub/Reach 列表和跨设备指令从 Morde 导航/后台连接移除 |
| **桌面画面、终端、浏览器接管** | nanoMuse Hands 操作本机屏幕；Morde Gateway/Workbench 操作云端 Host 并控制观看/接管权 | **云端操作走 Gateway/Workbench**；不把本机 Hands 当作 Host 控制通道 | nanoMuse 外壳打开受控 Host 页面；移动端补触控和软键盘。首版不请求无用的本机录屏/辅助功能权限 |
| **文件操作与同步** | nanoMuse 本地工具可读写其设备文件，Hub 可传文件；Morde 有 Workspace 版本/分块协议和 `morde-sync` | **项目同步只走 Morde Workspace**。普通本机目录由 `morde-sync` 管理；Host 副本由 Morde Runtime 管理 | 保留 nanoMuse 桌面文件选择/托盘能力；移植旧 Morde Desktop 的映射和状态，不重写 Go 同步算法、不启用 Hub file.get/put |
| **模型、Provider、密钥库** | nanoMuse 可在本机配置 LLM、Provider 与 Vault；Morde 已有用户模型连接与服务端任务模型选择 | **服务端模型唯一**。PC 内嵌用户 Web 可完整配置模型连接；本地同步进程不持有模型密钥，原生手机只读可用模型名称 | 禁用 Morde 版的 nanoMuse 模型采购/Provider 登录/Vault 设置；PC 在内嵌 Portal 页面编辑模型，密钥只经 Web 表单传服务端，不进本机同步配置 |
| **定时任务和通知** | nanoMuse 本地服务有计划工作/设备通知；Morde 已有 `/api/schedules`、`/api/notifications` 与 Task 执行 | **Morde 记录唯一**。手机/桌面读同一通知和日程 | 使用 nanoMuse 通知呈现可行；关闭本地计划任务队列和 Hub notify。首版只保证应用内通知，系统推送单独设计 |
| **记忆、长期目标、主动工作** | nanoMuse 本地有 SQLite memory/goals；Morde 的 Muse 风格需求也提出这些体验，但当前服务端没有等价的已验收统一实现 | **暂不合并，也不假装同步**。首版不露出 nanoMuse 本地记忆/目标；未来在 Morde 服务端统一设计后再接客户端 | 关闭相关本地入口与自动推进；保留需求文档中的目标，避免不同端各自记住不同内容 |
| **MCP、浏览器/文件/Shell 工具** | nanoMuse Agent 可直接用本机工具和 MCP；Morde 通过平台 MCP、Agent、Runtime 在受控 Host 操作 | **工具执行位置统一为 Morde Host**。本地工具仅供客户端自身的文件同步等明确功能 | 首版不让 nanoMuse 本地工具绕过 Morde Host ACL、审计和审批；以后如开放本机 Agent，另设计独立会话和权限 |
| **Web 控制台和管理页面** | nanoMuse 有 `/app` Web 控制台；Morde 有 Portal 用户 Web 和独立 Admin Web | **PC 全量复用用户 Web**。PC 内嵌同一份 Portal 用户页面并增加本机同步；手机 Web 建同源 `/app/`；Admin Web 仍独立 | PC 内完成所有用户 Web 页面与操作，不能以外部浏览器深链替代；安装绑定 Web 会话不能访问 Admin |
| **打包、更新和品牌** | nanoMuse 桌面壳已有 Windows/macOS/Linux 安装与上游更新路径；旧 Morde Desktop 仅作代码参考 | **用 nanoMuse 打包能力，发布 Morde 自己的产品** | 独立 app/bundle ID、图标、名称、更新源与签名；停用上游更新源，不接旧包运行时逻辑 |

上述“关闭”先指 Morde 构建中无入口、无后台连接、无权限申请、无数据写入。核对已克隆源码的依赖关系后再删除死代码；不能先整仓删除 `nanomuse/` 或 dsh Host，否则可能破坏桌面壳启动。上游许可证和 NOTICE 在发行检查中保留。

## 三、客户端与现有 Web 的分工

| 端 | 首版负责什么 | 交给现有 Web/服务端什么 |
|---|---|---|
| **nanoMuse 改造的 PC 客户端** | 应用内复用全部 Portal 用户 Web 页面和 Workbench：任务/计划、Host/Workspace 全生命周期、模型连接、API Key、通知、客户端管理等；额外提供本机目录同步、冲突/交接与托盘 | 业务状态仍由同一 Morde 服务端管理；Admin Web 独立，不在桌面安装绑定用户会话中开放 |
| **nanoMuse 改造的手机 Web** | 同一会话/Host/Workspace/通知的轻量入口，移动画面与任务恢复；不绑定本机目录或运行本机同步 | Portal Cookie 登录；创建/配置/撤销安装仍可打开 Portal 页面 |
| **nanoMuse 改造的 Android/iOS** | 原生导航、登录绑定、消息 SSE、任务动作、Host 画面、前后台恢复 | 任务执行、审批判定、Host 权限都由 Morde；不承诺手机本机 AI 或系统级远控 |
| **Portal 用户 Web** | 工作台、Assistant、任务/计划、Host/Workspace、模型连接、API Key、通知、客户端管理、下载及 Workbench | 同一用户页面直接供 PC 内嵌使用；纯浏览器不运行本机目录 LiveSync |
| **Admin Web** | 用户、Host、Workspace、容量和审计等现有平台管理 | 保持角色校验，不移植到原生端；可补客户端安装/撤销审计视图 |

桌面登录一次后，Electron 主进程保存 Client/Sync 凭证并建立安装绑定、仅限用户 Web 的 HttpOnly 会话；PC 内嵌同一 Portal 页面，另有隔离的本机同步界面与 `morde-sync` 子进程。手机原生凭证放系统安全存储；手机 Web 使用同源 Cookie。桌面必须分开**客户端凭证**和**Workspace 同步设备凭证**，因为当前 Morde `KindDevice` 只能走同步白名单；不能为省事把它扩权。客户端授权、Host ticket 与撤销后 Gateway 会话失效的具体新接口见完整版第 5、7 节。

## 四、实际要改、加、关

**修改 nanoMuse：** `web/`、Android、iOS 的 API 与页面数据源；`harness/desktop` 的品牌、更新、托盘、安全存储、隔离用户 Web 视图及本机同步。PC 不维护另一套会话/电脑/项目界面，而是在窗口内复用 Portal 当前和以后全部用户路由；只另加「本机同步」。所有资源 ID、状态和操作结果来自 Morde。

**增加 Morde 端：** 复用账号校验的客户端登录、安装绑定用户 Web 会话续取/整组撤销及严格路由白名单；Portal 保留 Web 登录和 `/app/` 静态入口，删除设备配对页；安全签发可由原生端打开的 Host ticket，并在 Gateway Host Cookie 中关联客户端撤销；无密钥的可用模型列表；Workbench 触控布局。Task/Host 服务层当前对 Session 类型有显式检查，须只在对应日常用例增加客户端 Kind，不得全局等同管理员或 Portal Session。OpenAPI、迁移、审计和负向权限测试同时更新。

**复用既有同步能力：** `project/morde-sync` 继续执行目录扫描、哈希分块、断点续传、`fill_missing`、LiveSync、冲突和交接。`project/morde-desktop` 仅供只读参考安全存储、目录重叠判定、设置与 Go 进程管理；最终安装包是 nanoMuse 改造版。首次安装在客户端内只需账号密码登录；首次使用本机同步再手动选择目录，不读取旧 app 状态或凭证；同一目录只能由一个新客户端实例写入。

**Morde 发行版关闭：** nanoMuse Cloud/Hub/relay 自动连接、Hub 远程任务、本地 MuseAgent 自动执行、Sentinel 作为云端审批、单独的会话/记忆/目标库、Provider/Vault 管理、上游更新与不再需要的本机录屏权限。关闭后检查无后台请求和残留 UI；源码是否物理删除以恢复后的依赖/构建检查为准。

## 五、交付顺序与通过标准

1. **核对已克隆的 nanoMuse 基线并编译原版。** 核对当前 `main` 与此前 v1.0.7 参考版本的差异、许可证和各平台入口，锁定实施 commit；构建通过后才完成此实施前提。
2. **先落 Morde 登录和 Portal。** PC 一次登录取得本机与安装绑定用户 Web 会话；其用户 Web 页面/API 全可用，Admin 被拒绝；同步设备凭证仍不能访问 Task。
3. **完成 PC 闭环。** 先逐项核对 PC 内当前用户 Web 的全部路由与操作，再验证本机目录选择、上传、补缺、LiveSync、冲突、交接、撤权及重启恢复；分别在 Windows、macOS、目标 Linux 环境原生验证。
4. **完成手机 Web，再验 Android/iOS 真机。** 同一账号跨端看到同一 Task/消息/审批；任务断线恢复；Host 可看、接管、输入、重连；网页管理入口可达。
5. **发行检查。** 安装包身份和更新源均属 Morde；无自动 nanoMuse Cloud/Hub 请求；凭证和 ticket 不进日志；许可证、签名、公证及各平台实际支持范围确认。

**验收总判据：** 一个用户动作只产生一条 Morde 业务记录；一个审批只由 Morde 权限和版本决定；一个项目只由 Morde Workspace 协议同步；一个 Host 只由 Morde Gateway/Runtime 控制。界面不能把 nanoMuse 本地状态与 Morde 云端状态合并显示为“已完成”。
