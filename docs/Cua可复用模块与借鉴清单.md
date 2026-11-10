# Cua 可复用模块与借鉴清单

> 2026-10-21。配套 [基于 Cua 优化 Morde 的设计方案][design] 与 [源码对比及超越路线][comparison]。目标是让 Agent 在用户的长期云端电脑上可靠工作，并让普通用户容易开始、接管和继续使用。
>
> 本文记录源码证据、复用决定和验收条件；不表示这些功能已经接入或通过实测。沿用原研究的 B01–B15，新增 B16–B20。当前开发进度以各 T 清单为准，验证状态以 [V01–V32 清单][validation] 及带日期的验证记录为准。

> 后续执行见 [20 Cua 借鉴优化实施 TODO](/Users/zhongxuan_li/Desktop/morde/docs/实施/20-Cua借鉴优化实施任务todo清单文档.md)：先完成现有实施清单，再以完成后的实际基线补差异；已有能力复用其有效验收证据。

## 1. 结论：优先复用什么

| 类别 | 当前选择 | 原因 |
|---|---|---|
| 独立组件接入 | 优先验证 **MIT Cua Driver**，由 `morde-runtime` 内部调用 | 可利用已有桌面观察、元素定位和动作核验；Rust 不必搬到 Go 内，也不用替换 Morde 的任务、权限和电脑管理。 |
| 可以复制的源码候选 | Driver JSON 合同样例；纯 TypeScript `cursor-motion` 的最小依赖集合；隔离验证用 Python 表单 fixture | 分别适合协议测试、可选的画面反馈和独立验证。必须有实际调用方，复制时连同适用许可和相关测试一起保留。 |
| 在 Morde 实现的行为 | 动作证据、过期引用拒绝、可靠停止、恢复、首次体验、历史最小化、独立验收 | 使用已有 Go/React/PostgreSQL 和冻结运行协议；避免再建一套 Agent、状态库或权限系统。 |
| 暂不引入的整体模块 | Spaces UI/服务端、Volume、Keyvault、Teleport、Fleet、ACP runner、多平台运行体系 | 分别存在许可、职责重复、架构或当前部署范围不匹配。按下面的模块级记录取舍。 |

**本次落地为设计文档。** Morde 正在另一会话开发，本次不改应用源码、不向生产目录加入没有调用方的 vendor 文件，不操作其进程、数据库、容器或运行镜像。后续实施可以按本文复用候选直接推进，不必为了使用 Cua 而硬搬语言或框架。

## 2. 可借鉴点完整记录

“优先”表示纳入当前 M1/P0 的设计和验收；“按需”表示有具体需求及收益后再接入。各项均是本地源码可支持的借鉴点，不据此宣称 Morde 已优于 Cua。

### 2.1 电脑操作与结果可信度

| 编号 | Cua 依据 | Morde 采用内容 | 落点与验收 |
|---|---|---|---|
| **B01 优先** | [ActionResult][C03]、[verify_state][C03b] | 分开记录调用是否结束、动作是否产生效果、任务目标是否达成；保留 `confirmed/partial/unverifiable/suspected_noop/refused` 的原始含义。`confirmed` 不等于任务完成。 | T11/T12；V04/V13/V16。窗口变化、HTTP 成功、模型自述均不能单独完成业务任务。 |
| **B02 优先** | [元素 token][C04]、[平台支持][C20] | 观察绑定电脑、运行实例、控制版本、窗口/页面和画面坐标；拒绝过期 token，重新观察后再行动。读取 DOM、可访问性树和截图按实际支持选择。 | T12/T13；V03/V04/V10。覆盖页面刷新、窗口重建、缩放、接管与运行端重启。 |
| **B03 优先** | [生成合同][C25]、[平台支持][C20] | 能力由固定运行产物、实际桌面环境、已接入的工具和权限共同决定；不支持时给稳定原因。协议版本、驱动版本和 capability 版本分别记录。 | T09/T12；V01/V03。能力列表不能仅来自镜像名或编译开关。 |
| **B04 优先** | [动作结果中的 hints/next_call][C03] | 失败时建议重新列窗口、重新观察等可执行下一步；下一步仍走 Morde 的授权、预算和控制校验。 | T11；V04/V18。上游提示及网页内容都不能直接触发工具调用或扩大权限。 |
| **B05 优先** | [进程登记与查询][C13]、[进程启动][C13b] | 长命令与 HTTP 会话分离；输出按游标读取，保留退出码、截断与停止状态。Morde 持久 operation 仍是重试与对账依据。 | T11/T12；V08/V12/V14。断线不丢进度；只有受管执行域确实停止才确认取消。 |

### 2.2 长期运行、接管和用户数据

| 编号 | Cua 依据 | Morde 采用内容 | 落点与验收 |
|---|---|---|---|
| **B06 优先** | [persistent Agent][C06]、[Volume 租约][C11]、[Volume 范围][C11b] | 单写、版本检查、明确保存范围；沿用 PostgreSQL 的任务/租约和独立持久卷，恢复前确认旧写入者失效。 | T10/T12/T15；V07/V12/V15/V22。重建后数据连续，副本不共享可写状态或默认继承凭证。 |
| **B07 按需** | [runs][C05]、[ACP runner][C05b]、[harness 目录][C05c] | 借鉴统一事件、后续输入、中断、会话恢复与模型能力检查。保留已有 Responses Agent；出现真实第二种 harness 需求时再扩展现有模型端口。 | T11；V13/V14/V17。会话续接不能替代任务检查点，且不能继承上游 `full_access/bypassPermissions/yolo` 作为 Morde 授权。 |
| **B08 优先** | [输入租约][C08]、[服务端鉴权][C09]、[连接撤销][C09b] | 撤权在服务端生效，处理旧连接、排队输入、按下未释放的键/鼠标、在途读取及后台任务。整机人工接管需完整排空确认。 | T12/T13；V08–V11。Morde 人工控制不会因用户几秒不动而自动交回；不照搬 Cua 窗口级闲置租约。 |
| **B09 优先边界、按需扩展** | [授权宿主][C22]、[session 授权][C04b]、[site login][C12]、[Keyvault][C12b] | 首版用私密人工登录；批准绑定可信用户、对象、内容、版本和有效期。将来确需凭据代理时仅返回掩码结果和受限凭据引用。 | T12/T13；V10/V18。公开 session 名、模型传入的用户 ID、旧批准均不构成授权；不先建设完整密码管理器。 |
| **B10 优先少量应用** | [Teleport][C12c]、[Volume][C11b] | 声明支持的软件、CPU 架构、安装来源/checksum、数据目录与恢复限制。镜像、文件、登录态、运行中进程分别说明。 | T12/T17；V06/V07/V22/V23。先保证工作目录和一个浏览器 profile，逐个应用扩大。 |
| **B14 优先原则** | [Computer History preview][C23]、[history 实现][C23b] | 默认收集最少任务证据；正文/截图按任务需要与授权短期保存，读取、删除、导出各有权限。不得默认全盘录屏或把秘密放进索引。 | T11/T15；V18/V20。历史与记忆复用已有存储和权限；不直接增加另一套 History 数据库。 |
| **B16 优先** | [routines][C07] | 可注入时钟、计算触发时间与实际执行分离、明确错过和忙碌时的行为；落到 Morde 已规划的计划、任务和通知表。 | T14；V19。使用明确 IANA 时区和触发点唯一标识；不照搬本机时区、JSON 存储或忙碌即消耗时段的默认语义。 |

### 2.3 上手、交付与验证

| 编号 | Cua 依据 | Morde 采用内容 | 落点与验收 |
|---|---|---|---|
| **B11 优先方法** | [cua-bench][C17]、[独立表单 fixture][C28] | 固定输入、环境与 evaluator；让测试程序读取真实输出，而非让执行模型自己判分。记录所有失败与重试成本。 | T13/T16；V15/V16/V28/V32。Python Bench 与其所需 spacesd/镜像分别评估，不将 MIT runner 视为整套环境均可直接使用。 |
| **B13 优先** | [Quickstart][C19]、[首次运行 smoke][C24b] | 安装/能力检查、现成真实示例、后台准备、可解释进度、失败后保留输入并续接。普通用户默认从网页进入。 | T13/T16；V26–V31。管理员配置一次模型；有授权默认连接的用户无需重复填 Key。MCP 作为外部 Agent 入口。 |
| **B15 优先** | [测试 harness 指南][C24]、[Linux 验收范围][C20b] | 固定版本、独立应用结果、明确拒绝码、正常与故障路径并验；用户指南中的步骤也作为回归样例。 | T09/T12/T13/T16；V01/V04/V32。模拟通过、编译通过、文档描述和实际运行分别记账；不复制会上传产物的 CI 工作流。 |
| **B17 优先** | [contract README/manifest][C25]、[兼容 fixture 方法][C27] | 一处定义合同，生成客户端，检测 schema 漂移；挑选稳定的语义字段做回归。沿用 Morde 的 OpenAPI 与冻结运行合同。 | T10/T12/T13；V01/V12/V30。不能手改生成文件，也不能拿 Cua 的完整 manifest 直接覆盖 Morde API。 |
| **B19 优先** | [Driver 发布链][C21]、[Python 打包][C21c]、[许可地图][C16] | 固定组件版本、架构和摘要，列传递依赖，升级后只复验受影响能力；保存可恢复的数据版本与回退条件。 | T12/T16；V01/V02/V03/V22。构建包含 ARM64 不代表目标 runsc 环境已通过；本次未获取或安装新产物。 |

### 2.4 有明确需求时再采用

| 编号 | Cua 依据 | Morde 采用内容 | 落点与验收 |
|---|---|---|---|
| **B12 按需** | [mesh 策略][C14]、[relay 边界][C14b] | 显示实际连接状态和回退原因；跨节点再研究直连/中继。继续使用现有 Gateway，按实际部署说明数据经过哪里。 | T16/T18；V24。接入 mesh 需要匹配的对端与身份管理；不能只复制 MIT 客户端就假设整条链成立。 |
| **B18 按需、实验** | [visual regions][C32]、[模型第三方声明][C33] | DOM/可访问性不足时评估视觉区域观察；截图 ID、动作坐标映射与窗口身份一起使用，一次动作后重新观察。 | T12 后的能力扩展；V03/V04/V18/V32。模型缺失明确报错，不自动安装；不能绕过 stale 检查降为裸坐标。 |
| **B20 按需、低优先级** | [cursor-motion][C30]、[canvas renderer][C30b] | 可用纯 TS 动画显示 Agent 光标轨迹或回放；使用 Morde 自己的视觉样式。 | T13 桌面反馈完善时。校准截图/画布缩放，支持减少动画；叠层不拦截操作，不把动画当作真实指针位置或动作回执。 |

## 3. 模块级复用决定

### R01｜Cua Driver：优先作为独立进程接入，暂不 fork

- **来源：** `libs/cua-driver/rust`，本地 `VERSION` 为 `0.38.0`；MIT，实际发行包与可选资产另核对。
- **复用方式：** 固定版本的独立进程，通过公开 MCP 边界接到运行端。优先验证 stdio；Driver 生命周期、会话隔离、显示会话和权限必须能被运行端管理。具体决策见 [设计 §4][design]。
- **适合原因：** 桌面适配不依赖把 Rust 翻译成 Go。平台仍只认识 Morde 的 RuntimeControl，不把 Driver 暴露为第二个公网或模型直连入口。
- **引入前：** V01–V04、V08/V10；确认 Linux/runsc/普通用户、会话 token、停止与晚到结果边界；没有重复且不可控的共享 daemon。
- **不采用：** 整仓 vendor、多平台工具链搬入 Go 服务、未经验证的全部工具透传。仅在已有证据指出缺陷且独立仓库就绪后考虑上游补丁。

### R02｜Driver 合同及 JSON fixture：可以复制选定测试数据

- **来源：** [contract][C25] 下 `fixtures/image-result.json`、`session-success.json`、`tool-refusal.json`；根目录 MIT 适用，复制时保留声明。
- **建议去处：** 独立 `morde-runtime` 的 Driver 适配器测试数据目录，具体路径由该项目实施时确定；不放入生产业务配置。
- **使用方式：** 原始样例用于解码、拒绝、图像限制与协议漂移测试；额外添加 Morde 的 stale、失租、重复 operation 和结果不明样例。保留来源样例，不静默改成只适合本方测试的数据。
- **边界：** 本地 portable SDK 合同标为实验切片，`contract_version=0.8.0`，不等于 Driver `0.38.0`；运行时完整 schema 才能说明该版本实际工具。`tool-refusal.json` 不是所有工具通用的 ActionResult，不能据此造统一上游返回类型。[合同说明][C25]、[manifest][C26]

### R03｜cursor-motion：可以复制纯 TS 最小集合，但不进入首个闭环

- **来源：** [package][C30]、[render][C30b]；MIT，`private: true`，不能假定 npm 上已有可安装的稳定包。源码无外部运行依赖，使用浏览器 canvas。
- **建议去处：** 确有 Agent 轨迹展示需求后，放入 `web/workbench` 现有局部组件范围；按 renderer 的实际 import 闭包复制几何、路径、计划、样式等文件及被使用 API 的测试。
- **不要复制：** CLI 配置生成用的 `src/driver.ts`、无实际调用的入口导出和 playground；构建配置沿用 Morde，不再引入另一套构建体系。原测试含包结构/完整导出假设，裁剪后需保留行为断言并调整这些结构断言。
- **验收：** 不移动真实鼠标，不遮挡 noVNC 的输入与现有光标；缩放/DPI/画面边界一致；键盘用户和减少动画设置可用。Cua 的图形与名称不作为 Morde 品牌资产。

### R04｜Python 合成表单：可用于独立试验，不进入 Go 服务

- **来源：** [fixture_server.py][C28] 与 [test_fixture_server.py][C29]；Python 标准库，适用仓库 MIT。
- **价值：** 浏览器提交表单后由 `/state` 独立读出结果，避免根据截图或模型回答猜测成功；默认回环监听，测试支持临时端口。
- **采用条件：** 隔离验证环境本来就使用 Python，可连同测试复用。若当前验证使用 Go/现有网页 harness，直接采用相同测试方法，不为小型 fixture 新增生产语言依赖。
- **限制：** 样例仅保留最近提交值，不证明“提交不重复”。V16 还需合成服务持久记录提交次数、幂等键和回执，注入响应丢失/重启位置；不能用这两份文件宣称恢复能力通过。

### R05｜cua-bench：借鉴 evaluator 与轨迹方法，暂不整体接入

- **来源：** [libs/cua-bench][C17]，Python，MIT。
- **架构成本：** 其 runner 使用 Cua SDK 管理环境，并调用 spacesd 的 Shell/文件/截图；不是现有 RuntimeControl 的可直接替换客户端。
- **决定：** 优先给 Morde 现有验证增加独立 evaluator；确需外部任务集时再做最小适配，同时核对数据集、应用、镜像、模型及服务端许可。
- **区别：** `libs/cua-driver-bench` 是 FSL，官方允许内部商业产品评估，提供商业评测产品/服务有额外条件；不能混为一套 MIT 代码。[C15]

### R06｜Driver TypeScript/Python SDK：不复制进 Portal 或 Go 后端

- **来源：** [TypeScript package][C34]、[入口][C35]；MIT，但通过 UniFFI/原生模块调用 Rust，Node/Electron 入口并非纯浏览器 SDK。
- **决定：** 当前 Go 运行端优先协议接入；React 页面只调用 Morde 已授权 API。不得因为文件后缀是 `.ts` 就把原生 SDK 导入 Vite 页面，也不为此引入常驻 Node 桥。
- **后续例外：** 若单独提供给 Node 开发者的客户端示例，可按该 SDK 的部署要求使用；不影响内部主链路。

### R07｜cua-agents / ACP：借鉴模型适配和事件，不替换已实现 Agent

- **来源：** [harness][C05c]、[runs][C05]、[runner][C05b]；主体 Rust + Node，cua-agents 标 MIT，现有执行链还依赖 spacesd。
- **决定：** 复用概念、字段语义及能力探测方法。Morde 的 `Agent/ToolExecutor/TaskWorker`、预算和任务持久化继续作为唯一任务执行链。
- **触发条件：** 真实需求要求接入一个无法经现有模型接口实现的外部 coding harness，再核对其权限模式、恢复、进程控制和依赖。不得为了多模型列表重建完整 Agent 框架。

### R08｜Spaces、Volume、Keyvault、Teleport、媒体：按行为设计，源码不默认复制

- **来源：** [许可地图][C16] 覆盖 `apps/cua-spaces-web`、spacesd、persistent、volume、keyvault、teleport 和 media 系列；FSL-1.1-MIT。
- **架构：** Web UI 虽为 React/TS，仍依赖 Rust/WASM app core、Zustand、TanStack Router 等；Morde 使用自己的 React Router、Query 和组件。数据、电脑、任务、身份职责也与 Morde 重叠。[C38]
- **决定：** 学习清晰的体验和业务约束，在 Morde 既有结构中实现必要行为。不复制整页、状态容器或服务；把代码机械翻译成 Go/TS 仍不能消除原许可证要求。
- **若选择完整采用：** 单独评估商业授权、成本和运行体系替换。官方说明：以 Spaces 或基于它的产品向他人提供 hosted/managed 环境需要商业许可；每个版本两年后转 MIT，不等于当前版本已是 MIT。[官方说明](https://github.com/trycua/cua/blob/main/legal/COMMERCIAL.md)

### R09｜Fleet Go 后端：语言相同，职责与依赖不适合

- **来源：** [Fleet main.go][C36]、[go.mod][C36b]。包含 Go 服务，但围绕 Cyclops/Kubernetes、Keycloak、OPA、Stripe 等构建，与 Morde 的服务及身份边界不同；`libs/fleet` 是上游规定的只读镜像。
- **决定：** 不复制业务服务、路由、鉴权或存储框架。语言相同不足以抵消系统迁移成本；本次没有逐文件完成其第三方许可审计，也不武断把整个 Fleet 标为 FSL。
- **背景：** Cua 官方已宣布 Cloud/Fleets 于 2026-10-18 结束；本地 SDK/BYOC 路线继续。此处排除的是这套托管栈的复用，不能据此说 Cua 不支持云端电脑。[官方迁移说明](https://cua.ai/docs/cua-sdk/guides/cua-cloud-ended)

### R10｜Modal Go helper、Lume、多云 provider 与 mesh：按部署需求选择

- [Modal helper][C37] 同为 Go，却绑定 Modal SDK/gRPC；当前 Morde 的 Linux/OpenSandbox 主线没有该依赖，不复制。
- Lume/macOS 虚拟化和其他 provider 属于扩展平台覆盖，首个 Linux 长期电脑闭环无须整套引入；若后续明确支持 macOS 再单独评价硬件、部署与维护成本。
- [cua-mesh][C14] 是 MIT 客户端组件，但运行端、协调、身份及部分服务属于另一层；现有单节点 Gateway 已承担访问路径，先按 B12 记录设计取舍。

### R11｜视觉 perception：实验组件与模型资产分开选择

- [visual regions][C32] 标为 developer preview；长连接中保留截图，引用 `capture_id` 和返回的动作坐标映射，一次输入后重新观察。一次性 CLI 调用不能假定能解析另一个进程的截图 ID。
- Driver/worker 的 MIT 不覆盖所有模型：本地声明包含 OmniParser 图标模型 AGPL-3.0-only、PP-OCR 模型 Apache-2.0 和 ONNX Runtime MIT。[模型声明][C33]
- 当前决定是借鉴有效期、能力缺失和受限 worker 的设计；仅在真实任务证明视觉解析有必要时，核对许可、资源占用、中文识别及效果后采用。截图解析 worker 不应获得输入、凭据或任意联网权限。

### R12｜发布/文档 smoke/兼容 fixture：复制方法，按现有工具改写最少测试

- 参考 [Driver 发布][C21]、[首次使用 smoke][C24b]、[兼容样例][C27]；不复制整套多平台 CI、上传步骤或本地用不到的环境编排。
- 验收绑定精确版本、应用与镜像，用稳定字段做差异检查；拒绝码和不支持的平台必须成为可见结果，不能删掉失败样例让统计好看。
- 用户要求仅本地修改和保存，相关证据、产物及文档留在本地；不配置产物上传或自动发布。

## 4. 实际复制/接入时的本地记录

每次只记录这次真正采用的模块，不预建一整套 vendor 管理平台：

| 必填项 | 内容 |
|---|---|
| 来源 | 上游仓库、目录、release/commit；无法确认提交时记录文件 SHA-256，不臆造版本身份。 |
| 许可与署名 | 保留原 LICENSE、版权及适用第三方声明；核对素材和传递依赖；记录原作者来源，改名不删除署名。 |
| 复制范围 | 精确文件列表、必要依赖与原测试；注明删减和本地修改原因。 |
| 实际调用方 | 对应页面、适配器或验证 harness；没有调用方的代码暂不进入生产目录。 |
| 行为边界 | 入口、授权、数据保存、取消、错误和不支持范围；不得绕过 RuntimeControl。 |
| 验证与更新 | 对应 V 项、本地结果、采用/拒绝理由、升级负责人和回退条件。 |

如果只是参考行为，记录 B 编号与独立实现位置；如果包含上游实现、结构化协议资产或翻译代码，按实际复用保留许可来源，不伪装成完全自有代码。Cua 的名称与标识另有商标规则。

## 5. 本地证据基线

源码位于 [本地 Cua 目录][cua-root]。上次研究的指纹见 [主报告附录 B][comparison]；本轮补充读取时间为 2026-10-21 19:27（UTC+8）。没有在本轮确认整仓对应 Git commit；下面指纹仅标识所读文件，不代表一套已构建、已部署版本。

| 相对 Cua 根目录的文件 | SHA-256 |
|---|---|
| `libs/cua-driver/contract/README.md` | `b72fa2f088b6001b9b1ee6f32b2be9ddedde65c886cd196740af9d8026e2013d` |
| `libs/cua-driver/contract/manifest.json` | `6a174343bf2c8215c65f31e787d44881bb7a9e3aeb7061928b5014e119cf6b4c` |
| `libs/cua-driver/typescript/src/index.ts` | `7cf62ab0bc66a3d6d6479a8fbb514e7ebd721856b66b59de114c8075f0e79a7b` |
| `libs/cua-driver/typescript/package.json` | `56497d2eae816ce26e0b3c399eaf2a40502e080de85e1cda406a2e6cb5cf271f` |
| `libs/cua-driver/examples/agent-sdks/fixture_server.py` | `f7dee09c293e040ae8f8fd137134df19602981497ea7976a69787192976a4189` |
| `libs/cua-driver/examples/agent-sdks/tests/test_fixture_server.py` | `f173df7946678ec035bccd52fd28e73922557a83771b18601297ca9202175d0f` |
| `libs/typescript/cursor-motion/src/driver.ts` | `016fe9815c8f64d1ace40bf93944b7dc7f07329735d3ed718db67c093fed9759` |
| `libs/typescript/cursor-motion/package.json` | `f3f9e8e4e403497a0982d89ec2c9e5183e3e7722d6a5504917bb74f3d834e9bb` |
| `libs/fleet/backend/main.go` | `452173490a148d8bfff36a2962569afd486fcc9f9061e5dd4fe4757daec07afb` |
| `libs/cua/crates/cua-contrib/modal-helper/go.mod` | `5b9b30024761568de94676703ed3e6e288b139182dcf73b560140c3c23b41cb7` |


[C03]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/cua-driver/docs/action-result-contract.md:1
[C03b]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/cua-driver/rust/crates/cua-driver-contract/src/verification.rs:4
[C04]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/cua-driver/rust/crates/cua-driver-core/src/element_token.rs:4
[C04b]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/cua-driver/rust/crates/cua-driver-core/src/session_authorization.rs:1
[C05]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/cua/crates/cua-agents/src/runs.rs:1
[C05b]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/cua/crates/cua-agents/runner/acp-runner.mjs:331
[C05c]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/cua/crates/cua-agents/src/harness.rs:38
[C06]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/cua/crates/cua-spaces-ext/src/persistent/mod.rs:7
[C07]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/cua/crates/cua-spaces/src/routines.rs:33
[C08]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/cua-spacesd/crates/cua-spacesd-session/src/media/leases.rs:4
[C09]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/cua-spacesd/crates/cua-spacesd-server/src/auth.rs:102
[C09b]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/cua-spacesd/crates/cua-spacesd-server/src/server.rs:493
[C11]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/cua/crates/cua-volume/src/lease.rs:4
[C11b]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/docs/content/docs/spaces/guides/cua-volume.mdx:9
[C12]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/cua/crates/cua-spaces-ext/src/daemon/site_login.rs:274
[C12b]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/docs/content/docs/spaces/guides/keyvault.mdx:8
[C12c]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/docs/content/docs/spaces/guides/teleport-an-app.mdx:9
[C13]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/cua-spacesd/crates/cua-spacesd-server/src/process/mod.rs:654
[C13b]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/cua-spacesd/crates/cua-spacesd-server/src/process/spawn.rs:193
[C14]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/cua/crates/cua-mesh/src/policy.rs:4
[C14b]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/docs/content/docs/spaces/guides/relay-security.mdx:32
[C15]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/legal/COMMERCIAL.md:3
[C16]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/legal/LICENSING.md:5
[C17]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/cua-bench/README.md:20
[C19]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/docs/content/docs/spaces/quickstart.mdx:1
[C20]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/docs/content/docs/cua-driver/concepts/platform-support.mdx:1
[C20b]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/cua-driver/docs/linux-desktop-validation.md:1
[C21]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/.github/workflows/cd-rust-cua-driver.yml:188
[C21c]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/cua-driver/python/build_wheel.py:79
[C22]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/cua-driver/rust/crates/cua-driver-sdk/src/authorization_host.rs:1
[C23]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/cua-driver/docs/computer-history-preview.md:1
[C23b]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/cua-driver/rust/crates/cua-driver-core/src/history.rs:426
[C24]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/cua-driver/docs/test-harnesses-guide.md:1
[C24b]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/.github/workflows/docs-driver-first-run-smoke.yml:1
[C25]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/cua-driver/contract/README.md
[C26]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/cua-driver/contract/manifest.json
[C27]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/cua-driver/compat-fixtures/README.md
[C28]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/cua-driver/examples/agent-sdks/fixture_server.py
[C29]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/cua-driver/examples/agent-sdks/tests/test_fixture_server.py
[C30]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/typescript/cursor-motion/package.json
[C30b]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/typescript/cursor-motion/src/render.ts
[C32]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/docs/content/docs/cua-driver/guides/visual-regions.mdx
[C33]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/cua-driver/rust/crates/cua-perception/models/THIRD_PARTY_NOTICES.md
[C34]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/cua-driver/typescript/package.json
[C35]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/cua-driver/typescript/src/index.ts
[C36]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/fleet/backend/main.go
[C36b]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/fleet/backend/go.mod
[C37]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/libs/cua/crates/cua-contrib/modal-helper/go.mod
[C38]: /Users/zhongxuan_li/Desktop/morde/server/project/cua/apps/cua-spaces-web/package.json
[comparison]: /Users/zhongxuan_li/Desktop/morde/docs/Morde与Cua源码对比及超越路线.md
[cua-root]: /Users/zhongxuan_li/Desktop/morde/server/project/cua
[design]: /Users/zhongxuan_li/Desktop/morde/docs/基于Cua优化Morde的设计方案.md
[validation]: /Users/zhongxuan_li/Desktop/morde/docs/实施/Morde与Cua对照验证任务清单.md
