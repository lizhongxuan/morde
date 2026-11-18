# Morde PC 端云端电脑界面优化实施任务 TODO

来源：[PC 端设计稿对齐技术设计方案](<../Morde PC端设计稿对齐技术设计方案.md>)的“与 Codex 云端电脑界面的差距”“需要调整的区域”和“状态和权限规则”，以及 [PC 端布局与云端电脑体验需求](<../Morde PC端布局与云端电脑体验需求文档.md>)的“云端电脑”与验收条件。本文是单独的后续实施清单，不表示功能已经完成；原设计稿对齐清单的 P09、P10、P11 和 G2 状态与历史记录继续保留。

> 侧栏布局和按钮文案的最新要求见[云端电脑侧边栏增量实施清单](<Morde PC端云端电脑侧边栏实施任务todo清单文档.md>)。本文 T01～T06 的状态和既有执行记录保留；相同场景可复用符合要求的验收证据。

## 修改边界与现状

- 修改 `morde-pc` 内的云端电脑控制区：真实画面居中、底部四项功能入口、唯一接管／交还按钮，以及连接状态和错误表现。Portal 和 Workbench 是 PC 客户端内部实现；PC 外壳、对话和电脑管理页的管理操作继续可用。
- 当前 `Assistant.tsx` 的四个入口负责创建嵌入会话，且已经要求当前用户先接管；嵌入 Workbench 后导航和控制按钮被隐藏。Electron 使用独立 `WebContentsView` 承载 Workbench，不能假设 Portal 的 HTML 控件能覆盖它。为让失败时控制栏仍可见，本清单由 Portal 在原生视图外统一承载四项入口和控制按钮，Workbench 负责切换和校验实际功能；这细化了技术方案中嵌入模式 Dock 的实现位置。
- 进入已授权电脑面板后，可自动建立只读桌面观察连接，让 AI 控制时也能看到真实画面；这不等于点击四项入口或取得控制权。四项入口和桌面输入仍必须等当前账号的有效接管租约经服务端确认，且各目标本身已授权后才能使用。无桌面查看权限或无当前会话时显示原因，不自动选择其他电脑或创建会话、任务。
- 原有嵌入票据、控制权和租约接口继续使用。只改必要的客户端消息；若本地真实链路证实服务端合同缺失，再定位并记录缺口，不预先重建服务端。Workspace 和主机文件仍受各自授权约束，不删除数据或服务能力。
- 仅在本地修改；验收不得外发或发送项目文件、附件、截图、日志或合成测试文件，也不修改远端环境。使用本机隔离账号、服务和测试电脑；无法获得实际连接条件时，相关任务保持待验证。

## 接口变化

| 接口及位置 | 变化和调用约束 | 任务 |
| --- | --- | --- |
| `morde.workbench.embed` 版本化消息；`Assistant.tsx`、`Workbench.tsx`、`morde-preload.ts`、`morde-main.ts` | 拟新增父端到 Workbench 的 `select_target`（`target` 为 `browser`、`terminal`、`files`、`vscode`）和 `control_lock`（`locked` 为布尔值）命令，各带 `seq`：两类命令在每次嵌入中分别从 1 递增。前者只请求切换，后者只限制输入；解除限制必须重新读取网关控制状态，消息本身不能授予权限。 | T01、T03、T04 |
| 同一协议的 `state` 消息；上述客户端文件 | 增加 `active_target`（四项之一或 `null`）、`control_version`（已确认版本或 `null`）、`control_locked`（输入限制是否生效）及已处理的 `selection_seq`、`lock_seq`；继续传递 `workbench_ready`、`remote_desktop`、控制状态和租约字段。`workspace` 在主入口状态中映射为 `files`。原生桥接受 `ai`、`taking_over`、`user`、`returning`、`unknown`，并转发 `workbench_ready=false`，不能保留旧的成功状态。 | T01、T03、T04、T05 |
| `POST /api/hosts/{id}/embed-tickets`，`GET /api/hosts/{id}/control`，`POST /api/hosts/{id}/control/actions`，`POST /api/hosts/{id}/control/lease/renew` | 现有 HTTP 合同不变。进入面板时以已授权的 `browser` 为初始目标申请观察；接管／交还复用 `request_id` 和 `expected_control_version`。不能将票据或 Cookie 放进消息、URL 历史或日志。 | T02、T03 |

消息仍绑定当前账号、Host、会话和嵌入 nonce；跨源消息检查精确 `origin`、`source`、channel 和版本，Electron 同时校验受信 sender、主 frame、当前凭据及目标集合。文件入口允许映射到获授权的 `files` 或 `workspace`，其余目标直接对应票据中的目标。Workbench 对每次切换重新检查当前目标授权及网关租约；错误、过期或旧会话消息一律不改变当前面板。新增字段和命令仅供客户端协调界面，网关的授权与输入控制仍是最终依据。

## 实施任务

各项改动同步更新对应的 Portal、Workbench 或 Desktop 定向测试；T06 汇总并完成客户端实测。

- [ ] **T01（待验证）接通原生与 Workbench 状态桥**
  - 修改：`web/portal/src/routes/Assistant.tsx`、`web/workbench/src/app/Workbench.tsx`、`clients/desktop/harness/desktop/src/morde-preload.ts`、`src/morde-main.ts`，按上表增加命令和状态字段。每次处理 `select_target` 后回报相应序号，只有切换成功才改变 `active_target`；被拒绝时保留原目标并提示未能切换。`control_lock` 请求锁定时，在禁止桌面输入和功能切换后回报 `control_locked=true` 和相应序号；解除时重新查询网关，查询失败继续锁定。双方过滤无效 target、旧 nonce、旧序号和无权消息。
  - 验收：本地定向覆盖四项目标、状态未就绪／恢复、`taking_over`／`returning`、锁定及解除、快速切换的迟到确认、错误 sender、不同账号／Host／会话、旧 nonce 和未授权 target；确认既有 `remote_desktop` 四态可经 Electron 到达 Portal，且桥消息不能单独解锁。对应 PC 场景由 T06 实测。

- [ ] **T02（待验证，依赖 T01）建立居中画面和稳定控制区**
  - 修改：`Assistant.tsx`、`web/portal/src/styles.css`、`Workbench.tsx`、`web/workbench/src/styles.css`，让电脑面板打开时对当前会话和 Host 自动发起一次已授权的只读桌面连接；移除 MORDE CLOUD 预览卡、独立主机状态栏和工作画面的固定最大尺寸。远程画面在可用空间居中并按比例适配，Portal 在画面下方预留固定控制区；Electron 原生视图边界只覆盖画面区域，随面板、窗口和侧栏变化更新，不能遮住控制区。
  - 修改：保留现有单次票据、目标权限、请求代次和过期清理；加载失败或会话／Host 切换时取消或隔离旧结果，不重复兑换票据。没有会话、默认电脑、查看权限或可用主机时在画面区域说明实际原因，控制入口保持不可操作。
  - 验收：在实际 `morde-pc` 中打开面板、切换会话、收起／展开及调整窗口，确认画面居中、对话可用、控制区始终露出；未接管时可观察但无法向桌面输入。覆盖 1440×900、1024×768、800×600，且旧请求不会显示到新会话；连接失败不显示为已连接。

- [ ] **T03（待验证，依赖 T01、T02）统一接管按钮与租约生命周期**
  - 修改：`web/portal/src/components/HostControlPanel.tsx`、`web/portal/src/components/PortalLayout.tsx`、`Assistant.tsx` 和 `Workbench.tsx`。由认证后的持久 PC 壳层统一管理当前账号／Host 的控制状态、操作请求和续租，控制区仅显示一个“接管电脑／交还 AI”按钮；管理页的完整控制信息与权限规则保留。接管或交还直接使用该按钮发起，进行中显示真实进度并禁用；他人持有、只读、状态未知或租约失效时不显示为当前用户持有，也不替他人交还。
  - 修改：接管／交还前先锁定工作台输入并等待锁定确认；工作台不可用或消息发送失败时隐藏其原生画面并保持禁用。结果未确认时先查状态，必要时通过同一控制按钮沿用原 `request_id` 继续请求，禁止重复切换；状态读取失败自动重新核对，版本冲突后重新查询，不增加独立重试按钮。只有确认当前用户有效持有租约，且 Workbench 重新核对同一控制版本后，才解除限制。避免 Portal 与嵌入 Workbench 同时续租；折叠、切会话、切页或关闭标签不主动交还，隐藏画面不能接收输入；退出／换账号或续租失败后重新确认，不续租其他用户或 Host。
  - 验收：在本地服务与实际 `morde-pc` 检查接管、交还、处理中、失败／结果未知、版本冲突、他人持有、租约失效及后台超过一个续租周期；请求重复或迟到响应不造成重复切换，按钮和输入状态均以已确认结果为准。校验管理页未丢失原有操作。

- [ ] **T04（待验证，依赖 T01、T02、T03）接通四项底部功能入口**
  - 修改：`Assistant.tsx`、`web/portal/src/styles.css`、`Workbench.tsx`。在画面下方固定显示“远程桌面、终端、文件、VS Code”四项及 T03 的唯一控制按钮；去掉控制区的续期、适应窗口、键盘、Alt+Tab、找回窗口、全屏和独立重试等按钮。正常、加载和错误状态均不重复绘制另一组入口；PC 外壳自身的标签及窗口操作保留。
  - 修改：入口点击经 `select_target` 在同一 Workbench 切换；处理函数和 Workbench 接收端均核对当前用户租约、控制版本及目标授权，结果确认后才更新选中态。入口提供可访问名称、可见键盘焦点和不可用原因。文件入口使用既有主机文件和 Workspace 授权能力，Workspace 不作为第五个底部入口；功能内部正常操作不因清理控制栏而删除。终端复用现有会话和结果未知处理，切换或重复点击不能重复创建终端。
  - 验收：在实际 `morde-pc` 中确认未接管四项均不可点击；取得有效租约且对应目标获授权后，逐一切换并实际使用桌面、终端、文件与 VS Code；交还、撤权、他人持有或租约过期后立即禁用。共享账号仅能使用已授权目标，未授权项仍可见并说明原因；绕过按钮发送消息也不能操作。

- [ ] **T05（待验证，依赖 T01、T02、T04）按真实连接状态显示加载与错误**
  - 修改：`Workbench.tsx`、`web/workbench/src/styles.css`、`Assistant.tsx`。区分 Host 运行、工作台就绪和 noVNC 实际连接：初次 `connecting` 在画面中央显示加载，`connected` 显示真实画面，实际失败、超时未确认或连接中断时仅将画面区域变黑并居中说明。断线后自动重连期间保留错误状态，直到重新确认连接；不得因 iframe／原生视图加载或 Host 为 `running` 显示桌面已连接。
  - 修改：保留四项入口和唯一控制按钮；沿用 noVNC 自动重连，已接管时重新选择远程桌面可触发重连，不增加独立重试按钮。票据、权限或整个 Workbench 加载失败显示对应局部原因，不伪装成 noVNC 断线；重新打开电脑面板时可再次尝试，使用新票据，不重放已消费票据。终端、文件和 VS Code 的失败只影响相应功能区域。避免失败状态下无限签发票据或重复创建终端。
  - 验收：本地故障注入后在实际 `morde-pc` 区分初次连接、成功、超时、已连接后断线及恢复；确认仅桌面失败时画面变黑、中央有错误且底部控件仍可见。分别检查 Host 运行但 noVNC 不通、票据拒绝和其他功能局部失败，不误报成功或误分类。

- [ ] **T06（待验证，依赖 T01～T05）完成 PC 端集成验收并记录证据**
  - 修改：汇总 `Assistant.test.tsx`、`Workbench.test.tsx`、`clients/desktop/harness/desktop/tests/morde-main-portal-bridge.test.mjs` 和必要的 preload 定向测试，补充集成缺口；在本文记录本机启动方式、环境、各场景结果及未覆盖项。先审查测试副作用，只运行不发送文件正文、不修改远端的用例。
  - 验收：执行 Portal、Workbench、Desktop 的类型检查和相关定向测试；启动实际 `morde-pc`，从客户端入口完成 T02～T05 的交互与状态检查。确认本机 Host 网关的 `gateway.workbench_dir` 使用本轮构建的 Workbench 资源，避免客户端更新而网关仍返回旧页面。使用本机隔离服务和电脑验证真实 noVNC 画面、一次可观察的桌面操作、只读终端命令及各功能切换，文件相关只查看空目录或元数据；核对没有新增任务、没有切换目标电脑，原对话和已受理任务保持正确。
  - 验收：区分客户端真实画面、真实本机服务与模拟数据的证据；本机模拟只能证明对应模拟场景。缺少可连接电脑、某种故障或平台时，写明原因并保持相关任务待验证，不能以构建、单元测试或浏览器预览代替 PC 验收。

## 本轮执行记录（2026-11-17）

- T01～T05 的客户端实现已落地，仍未勾选：状态桥和四目标命令已贯通；画面与控制区分离；接管先锁定并等待确认；四个入口受有效租约和目标授权约束；Workbench 按连接状态呈现加载、失败画面。实际 Host 工作台尚未进入，故这些任务统一保留“待验证”。
- 检查通过：Workbench 类型检查、lint、定向测试 24/24；Desktop 类型检查、`build:test`、主进程与 preload 桥接测试 15/15；Portal 云端电脑定向测试 5/5（含接管确认前重复点击只发出一次控制请求）；Portal Vite、Workbench、Desktop 本地构建成功，`git diff --check` 通过。常规 Desktop `npm start` 会先跑 Portal 类型检查并被下述 Admin 类型错误阻断，因此本轮用 Portal Vite 直接构建静态资源后再构建并启动 Desktop。
- Portal 全文件类型检查仍被管理页 `AdminHostDetail.tsx`、`AdminRecords.tsx`、`AdminUsage.tsx`、`AdminUserDetail.tsx` 的既有类型问题阻断；Portal 全文件 lint 仍报告 `Assistant.tsx` 既有 ref 读取错误及依赖警告。本轮新增的云端电脑 lint 报错已清除。完整 `Assistant.test.tsx` 有 175 项通过、9 项与云端电脑无关的失败；云端电脑定向用例通过。
- 实际客户端：使用隔离用户目录启动新构建的 `morde-pc`，确认应用进入登录页；未填写账号或连接外部服务。检查本机 Gateway/Host 端口 `7080`、`60908`、`57120` 无监听，因此无法从客户端进入云端电脑面板，也无法实测真实 noVNC、输入、租约或断线恢复。此前本机模拟浏览器所见仅作为模拟状态证据，不替代 PC 验收。
- 后续本机模拟尝试：临时构建启用 MSW 的 Portal 资源，并用本地假服务和 Host 网关启动 `morde-pc`。隔离模拟登录成功，但登录后窗口没有呈现可操作的 Portal 内容，未能进入助手或嵌入工作台；该模拟也没有真实 Host/noVNC，不能代替客户端功能验收。临时资源、服务、Caddy 网关容器和隔离用户目录已清理；未触碰既有 `morde-local-postgres` 容器。
- 未修改服务端：当前实现未变更服务端 API 合同，本机没有可连接的 Gateway/Host，无法据此判断服务端故障；没有重建服务端，也没有连接或修改远端环境。待本机服务和测试电脑可用后，继续完成 T02～T06 的真实 PC 场景，并确认 Gateway 返回本轮 Workbench 资源。

从仓库根目录执行的基础检查和客户端启动命令如下；先准备仅使用本机的服务、测试电脑及未使用的隔离用户目录：

```sh
npm --prefix web/portal run typecheck
npm --prefix web/workbench run typecheck
npm --prefix clients/desktop/harness/desktop run typecheck
(cd web/portal && ./node_modules/.bin/vitest run src/routes/Assistant.test.tsx)
(cd web/workbench && ./node_modules/.bin/vitest run src/app/Workbench.test.tsx)
npm --prefix clients/desktop/harness/desktop run build:test
node --test clients/desktop/harness/desktop/tests/morde-main-portal-bridge.test.mjs clients/desktop/harness/desktop/tests/morde-preload-bridge.test.mjs
MORDE_TEST_USER_DATA=/tmp/morde-pc-cloud-ui-acceptance npm --prefix clients/desktop/harness/desktop start
```

定向测试含不符合本地限制的用例时，筛选允许的用例并记录；上述命令在实施后执行，本文不将其作为已通过的证据。

## 执行顺序与完成规则

按 T01 → T02 → T03 → T04 → T05 → T06 集成；T01 的协议要与其调用方一起落地。依赖项完成实现并进入“待验证”后，即可开展后续任务及 T06，不必等其先勾选。每项实现和对应验收均完成后才勾选，未能完成实际 PC 验收时标记为“待验证”并说明缺口。上述任务是对原清单的补充，不修改既有完成证据。
