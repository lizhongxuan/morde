# 会话 E-02：PC 同步与体验验收

**目标：**完成 R05 的 PC 入口、真实用户体验及跨端交接；E-01 的相关界面稳定后开始。先读[并发总览](<00-并发实施总览.md>)、[体验目标](<../Morde AI对话目标体验.md>)和[原方案](<../Morde 复刻 Muse 一体化实施与验收 TODO.md>)中 R05、R09、R13、T16–T18、T43–T59。

**写入范围：**E 独占的 PC 界面 / `clients/desktop/harness/desktop/` 和对应测试；原生 iOS 专属源码如需调整亦由 E 负责。同步服务端 / 记忆由 C 写，事实 / 接口由 A、D 写。E 是实际 PC GUI 与测试 profile 的唯一操作方。

**设计与交接：**按[PC 同步](<01-复杂链路设计与交接.md#sync>)、[长消息](<01-复杂链路设计与交接.md#long-message>)和[双端验收](<01-复杂链路设计与交接.md#cross-client>)逐项取证。

- [ ] **E02-1 文件夹同步真实入口（本地实现完成，Origin 授权错误已修并通过客户端回归；真实云端首次同步仍受默认 Host `failed` 阻塞）。**复用 `morde-sync.ts`、桌面主进程及 C-02 的服务端合同：选择本地文件夹、确认项目名 / 范围、用户点击后单向同步至固定默认云电脑。
  - [x] Session-cookie 项目目录登记携带与服务同源的 `Origin`；客户端回归通过，源码 `make pc` 构建并启动通过。
  - [x] 实际 PC 确认默认 Host 为 `failed` 时显示暂停提示且不提供换机入口；未执行云端写入。
  - [x] C-02 Go 客户端绑定扫描根目录 device/inode 身份，并在会话查询、恢复和传输前复核；定向回归通过。
  - [ ] T16–T18 首次 / 增量 / 修改 / 删除 / 冲突 / 恢复的真实云端验收。
  - 验收：T16–T18 覆盖首次同步、未点击不变化、再次点击后的新增 / 修改 / 删除、无反向同步；同步中本机变更显示待处理，部分应用 / unknown 先对账。冲突、源不可访问、失权 / 默认电脑不可用时保留内容并提示，不换目标。使用实际 `morde-pc`，真实云端按任务授权安排。
- [ ] **E02-2 真实 PC + DSH 主链（基础回复已通过；T43 在 `waiting_user / dsh_recovery_unresolved` 阻塞）。**与 A 选择隔离账号和版本，实际执行 T43：会话 → 复杂任务 → 两个 worker → 搜索 / 文件 / 命令或指定电脑操作 → MCP Apps 审批 → 读回验证 → 原会话交付。
  - 验收：工作项的对象、归属、状态和输出与真实账本一致；两 worker 实际重叠且汇总真实结果；审批卡两处同步，批准不等于成功。缺任一环节记录未覆盖，fixture 不能顶替 P+D。
- [ ] **E02-3 用户体验异常与长文。**执行 T45–T56 的复杂续接、歧义、停止、失败 / 合并、断线、晚到 FAIL 及 8K / 128 KiB+1 / 1 MiB+1 输入；按 E-01 的界面设计核对任务动态，T57 由 E02-6 单独执行。
  - 验收：原对话能说明结果或真实原因、无机械中断行、合并不误报失败；长文分别验证首条、运行中补充 / 修改及断线恢复，在模型 / 预算足够时核对首中尾及全文覆盖，不以仅保存消息或 mock 答案通过。
- [ ] **E02-4 双端与真实目标分层（默认 Host 当前为 `failed`，真实同步受阻；iOS 未运行）。**按 T44 分别记录 PC → 真实云电脑同步及同账号 PC/iOS 接续；验证 Agent 在授权范围读取同步文件、不同账号隔离。
  - 验收：真实云端和跨端各自提供环境、操作及结果；当前 iOS 交互验收若受项目规则或环境限制则标阻塞，不能用 iOS 代替 PC，也不能以 PC 结果冒充跨端。
- [ ] **E02-5 客户端证据交 A。**优先 `make pc`；可行时在同一源码 Electron 客户端保存 Playwright trace，结合实际状态 / 账本验证。
  - 验收：每项区分实际 PC + fixture、实际 PC + 真实 DSH、真实云端与未运行；用户体验、证据及缺陷交 A-02，不能用浏览器网页或 trace 回放代替客户端验收。
- [ ] **E02-6 GitHub 源码到流程图体验（T57 已在源码 PC 发出请求，但结果未知）。**E01-5 的展示入口可用后，在实际 `morde-pc` 发起公开 GitHub 仓库核心业务流程图任务；与 A 核对版本、关键代码引用、真实 DSH 调用及对话交付，并覆盖无法访问仓库的说明。
  - [x] 实际源码 PC 新旁聊提交了对 `deepseek-ai/deepseek-harness` 的只读分析请求。
  - [ ] 核对真实源码 commit / 行号、真实 DSH 结果和可见 Mermaid 图；客户端显示“网络异常，部分对话数据暂时无法读取”，等待 30 秒仍无回复。关闭后重开同一源码 PC profile，原用户消息仍可见、仍无助手回复且网络错误持续；本轮未重发，原请求处理状态需先对账。
  - 验收：用户在对话或 PC 客户端内看到可读图和来源；源码关键路径与图一致，权限/网络不足时如实说明。真实仓库或 DSH 不可用则对应层标阻塞，固定模型和纯代码块不代替通过。
- [ ] **E02-7 代码修复与资料结论体验（未运行，对应 T58/T59）。**相关合同和实现分项就绪后，在实际 PC 交办隔离项目修复和合成文档分析；与 A/D 核对修订、真实检查及可定位的来源，在对话中检查结果与未完成项。
  - 验收：按需验证可完成明确任务，用户/项目要求的检查不可省略；失败和缺证据如实说明。文件交付有关键要点，来源缺失/撤权不误导或泄露。真实 DSH 或实际 PC 不可用则对应层保持阻塞。

**依赖：**同步服务端由 C-02 分项交付；R20 / 长输入分别依赖 A/D、C 的可运行合同；实际验收不要求其他会话所有文档关闭后才开始，但完整通过必须覆盖真实结果。

## 交接记录

### 2026-11-25 本地与实际 PC 验收

- **源码 PC / profile：**通过 `MORDE_TEST_USER_DATA=/tmp/morde-e02-user-data make pc` 启动本轮源码客户端；实际 PC 端口为 `60118`。当前选中模型显示为 `DeepSeek Flash · deepseek-flash`。未在记录中保存任何 API 密钥或口令。
- **本地回归：**`cd clients/desktop/harness/desktop && npm test` 为 292 项通过、0 失败；`cd web/portal && npx vitest run --maxWorkers=1 src/routes/Workspaces.test.tsx` 为 57 项通过、0 失败，`cd web/portal && npm run typecheck` 通过；`cd runtime/morde-sync && go test ./client` 通过（`ok morde-sync/client 14.289s`），`gofmt -d` 无输出，C-02 两文件 `git diff --check` 通过；一次性 PostgreSQL 16.15 上 `server/internal/store/postgres` 的项目同步目录聚焦集成回归为 5 项通过、0 失败；E02 文件范围的 `git diff --check -- <E02 文件>` 通过。当前全仓 `git diff --check` 因并发中的 `server/internal/store/postgres/conversation_route_revisions.go:326` 报 `space before tab in indent` 而失败，该文件不在 E02 修改范围内。新增覆盖确认目录确认回调后重新核对本机目录身份、扫描后根目录替换时不创建 session、已保存 session 遇根目录替换时不恢复、session 创建后根目录替换时不传输、上传重试复用 session / 幂等键、helper 查询失败后的恢复、项目根目录不可访问时不报成功，以及同步目标查询错误可见且保持 fail-closed。
- **服务端修复与测试机更新：**按测试机实际源码提交 `80ada1709823f55698e5384987e424d5098cfd20` 建隔离 worktree，只叠加 C-02 的两个 Postgres 授权文件并构建 Linux/amd64 Portal；`migrate status` 显示 `237/237`，无需迁移。已备份旧二进制至 `/opt/morde/backups/pre-e02-sync-20261125T1550/portal`，新二进制 SHA-256 为 `f4029bf497afc4d52096f9179d239634826b20881a09a0afc96b0e569321abf2`；重启后 `healthz=ok`、`readyz=200`。
- **T43 基础真实 PC + DSH：**从源码 `morde-pc` 的新旁聊发送只读 smoke 请求，DeepSeek Flash 返回 `E02_PC_CONNECTION_OK`，回复已显示在原旁聊。随后提交完整 T43 只读验收请求（两个并行 worker、官方来源、默认云端电脑 UI + `pwd`、单次 MCP Apps 只读审批、读回与原对话交付）。T43 任务在 UI 显示“等待你处理”；只读查询测试账本确认 task `q95bzz7n` 为 `waiting_user`、`waiting_reason=dsh_recovery_unresolved`、无活动 lease，当前步 `suh20930`。原会话仍没有主 Agent 回复，也没有待审批卡；因此没有重发、批准、恢复或操作云端 Host。两个 worker、官方来源、`pwd`、MCP Apps 审批、读回和账本交付均未完成，后台未知操作状态需先安全对账。
- **T16 / T44 同步预检：**实际 PC 进入“文件同步”后，默认云电脑运行中。初次 `GET /api/hosts/hdqokxzv/sync-folders` 返回 HTTP 500（请求 ID：`ff13ff6f-5146-437d-b58a-7056ae718366`、`d5160a73-dbfd-4a4a-bdba-e465cc2cf26c`）；测试机日志确认（`be1862d3-aed1-44fe-9985-120563194b54`）。只读 DB 核实 Host 为 running、Workspace binding 为 active、Workspace 为 ready、用户为 approved、同步表/列存在。部署源码 `requireActivePortalSession` 只接受 `client_installation_id IS NULL`，与绑定 desktop installation 的 PC WebSession 不兼容；C-02 已修正 guard。修复版测试机部署后，源码 `morde-pc` 文件同步页显示默认电脑运行中、已有项目数 0、选择目录按钮可用；Portal 日志中的 GET 返回 HTTP 200（请求 ID：`02fd50c8-e7f0-4240-bc89-05fb02cbf1ba`）。未打开目录选择器或上传文件，真实同步、冲突和删除验收仍待做。同步页持续 fail-closed。
- **2026-11-25 文件夹登记 403 复核与源码 PC 重测：**当前服务端 `checkOrigin` 中间件要求 session 的所有写请求携带与请求 Host 同源的 `Origin`；`MordeAPI.registerHostProjectFolder` 先前只发送 Portal cookie、幂等键和 JSON，没有 `Origin`，因此 GET 可成功而登记 POST 会被中间件拒绝。客户端现传入登录时已校验的 `auth.origin`；`morde-core.test.mjs` 新增回归断言检查该请求头。`cd clients/desktop/harness/desktop && npm test` 全量通过，292/292。之后通过 `MORDE_TEST_USER_DATA=/tmp/morde-e02-user-data make pc` 构建并启动源码 PC；实际 PC 的“云端电脑”和“文件同步”页面当前均显示默认 Host 为 `failed`，同步页提示默认电脑不可用于项目同步、已暂停且不会切换目标，目录选择入口未显示。因此本轮没有发起登记或文件上传，之前无 Origin 的 POST 403 已由只读 DB 确认没有目录登记或项目文件写入。隔离本机合成目录 `/tmp/morde-e02-sync-acceptance-20261125` 保留供 Host 恢复后重测；上轮目录选择误选的 `demo-test` 不是该夹具，也没有对应远端登记。
- **默认 Host / T43 最新只读对账：**Host `hdqokxzv` 当前为 `failed/lost`、generation 5，租约到期时间仍在未来（不是 TTL 过期）；最近 reconciler 将状态转为 `lost`，但本次证据无法确定云端 sandbox 消失的根因。Docker 中无对应目标 sandbox，活跃 Host activity lease 为 0。已知恢复路径不覆盖当前 `lost` 状态，因此没有改 DB、重启或改绑目标。T43 task `q95bzz7n` 仍为 `waiting_user`，无 lease owner / 活动 task lease，当前 step `suh20930`，event_seq=32、checkpoint 到 seq=17；一个 step succeeded、一个 verification unknown，6 个 checkpoint，审批数 0、task_operations 与 task_hub_operations 均为 0。活动标记虽显示 running/started，但 heartbeat 已陈旧；`host_id` 为空，不能把该任务归属到 Host ledger。未读取或解密模型响应，不恢复、不重试、不批准，也不操作云端电脑。
- **源码 PC 暂停状态复验：**在最新源码 `make pc` 构建启动后，从实际客户端依次打开账号菜单、设置、文件同步。页面显示默认电脑“失败”、同步已暂停、默认电脑不可用于项目同步且不会切换到其他电脑；可访问控件中没有“选择本机目录”或上传入口。客户端已退出，未提交登记或同步请求。
- **未运行项：**T16–T18 的真实云端首次/增量/删除/冲突/恢复验收未运行；T44 的 Agent 读取同步项目、PC/iOS 接续和账号隔离未运行；T45–T56 长消息、复杂续接、失败恢复未运行；T57 仅提交了真实 PC 请求，未取得回复、源码版本或流程图交付；T58/T59 项目修复 / 文档分析未运行。T43 任务仍处 `waiting_user`，没有主 Agent 交付，不以本地 fixture 代替。
- **跨范围交接：**C-02 已在 `runtime/morde-sync/client/client.go` 保存扫描根目录身份，并调用 `ScanMatchingIdentity` 将设备号和 inode 校验绑定到实际扫描对象；resume metadata 记录身份，会话查询 / 恢复及传输前复核根目录身份。三项定向回归分别覆盖 manifest 后替换根目录不创建 session、恢复前替换根目录不访问服务端、session 创建后替换根目录不提交传输。`go test ./client` 通过。现有 `ScanMatchingIdentity` 不接受扫描进度回调，所以扫描期间目前只有 inventory、hash 完成和 planning 阶段快照，没有原先细粒度逐文件进度；真实 PC 同步体验仍需在 Host 恢复后检查这一变化是否可接受。该客户端修复未部署到测试机。
- **下一步：**先对账 T57 旁聊中已发出的请求后再决定是否继续，不能重复派发未知操作；同时需由受支持的运维路径恢复同一默认 Host 并安全对账 T43 的 `dsh_recovery_unresolved`。T16–T18 的 POST Origin 客户端修复已通过本地回归，但默认 Host 当前为 `failed/lost`，只有该 Host / Workspace 恢复为可用状态后，才从实际 PC 选 `/tmp/morde-e02-sync-acceptance-20261125` 继续首次与增量同步、冲突及删除测试；不得切换电脑或在 Host 未就绪时直调登记接口。T44 的 Agent 读回、iOS 接续与账号隔离、T45–T56 与 T58/T59 体验场景继续保持未完成。
