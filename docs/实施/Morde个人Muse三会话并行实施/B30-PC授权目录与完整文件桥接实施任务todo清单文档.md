# B30 PC 授权目录与完整文件桥接实施任务 TODO

**会话：B｜阶段：P3｜状态：已完成（T1–T4 全部完成；G2 已通过，B30 的真实 PC→本地服务数据链路已验收；G3 由 A30-T4 汇总）。**

来源：[需求与架构](../../Morde个人Muse需求与架构设计方案.md)；共同规则：[执行总览](00-三会话执行总览.md)；接口：[共享合同](01-共享接口与联调合同.md)。

## 开始条件与修改边界

G2；与 A30/C30 并发；K4 控制/数据合同必须先冻结。

clients/desktop/ 中目录授权、Hub、morde-hub-actions、Electron 主进程/preload 及 UI；runtime/morde-sync/client/hub_inspect.go、cmd/morde-sync 及相关本机 helper 测试。B 不改 server/internal/domain/hub 或任何服务端文件。

## 接口变化

消费/实现 K4 的 PC 对等端。拟扩展现有本机 helper 的完整文件快照/有界分块读取接口，由 B 独占；其输出受目录身份与授权约束，不能直接复用任意绝对路径读取。对服务器只使用冻结的 file.fetch 和 manifest/chunk/commit/cancel。

## 任务

- [x] B30-T1（完成）补完整文件快照与受限读取 helper
  - 修改：在已有受控 helper 中支持文本/二进制的稳定文件版本、摘要和有界分块读取，保留根目录身份校验。依现有模块组织扩展，不把整个文件常驻内存或开放公网文件服务。
  - 验收：使用合成目录验证超过 32 KiB 的 PDF/办公文档、父路径/符号链接/根目录替换；源变化拒绝继续旧快照，读取只发生在选中目录，输出与原文件摘要一致。
  - 完成与验收证据（2026-11-10）：helper 新增 `hub_file_snapshot`/`hub_file_chunk`，全程流式计算 SHA-256，读取上限 4 MiB，保留 32 KiB 文本预览；合成 PDF/DOCX 字节重组摘要一致，覆盖旧快照、文件/嵌套符号链接、父目录替换为外部符号链接及根目录替换。`go test ./client ./cmd/morde-sync -count=1` 与 `git diff --check` 通过。每个分块会重新流式校验完整源文件，内存有界但大文件多分块时磁盘读取量较高。

- [x] B30-T2（完成）实现授权传输与断线恢复
  - 前置：B30-T1、A30-T1。
  - 修改：PC Hub 收到 file.fetch 后复核设备、目录授权版本、Task 身份，调用 helper 并向 K4 数据端点传输；凭证留在受限主进程。处理分块确认、取消和同版本续传。
  - 验收：用 B 的本地服务替身验证重放、断线、源变化和撤权；令牌不进入页面/模型/日志，取消不是仅关闭 UI 等待，后续读取和传输确实停止。
  - 完成与验收证据（2026-11-11）：PC `file.fetch` handler 严格核验 server-generated Task 修订、目标授权版本、fetch ID、ticket；helper 只读授权根目录下相对路径、单文件上限 16 MiB，按 512 KiB 原始块与 `confirmed_sequences` 续传。ticket 不进入页面、事件、结果或本机记录；每次读取/传输前复核授权，取消会终止 helper 与 HTTP 请求。原替身测试覆盖重放、取消、撤权、源冲突；新增本机 Node live test `tests/cloud-morde-hub.live.mjs` 使用真实 `morde-sync` 和真实 Go API/PG fixture，通过 442/442 包测试、build、typecheck。live test 验证已确认分块续传、源变化后停止旧快照、16 MiB+1 拒绝、撤权后无新读取、未完成副本 404、提交后下载字节与合成 PDF 完全一致、ticket 不泄漏；并验证普通文本 `file.get` 保持可用。临时 PostgreSQL、服务、合成文件均在本机清理。

- [x] B30-T3（完成）接入目录授权和副本状态界面
  - 前置：B30-T2。
  - 修改：PC 目录选择/授权/撤销、在线状态与读取记录；显示来源版本、取回进度和服务端保留/删除结果。默认按需只读，不隐含同步整目录、写回或本机命令权限。
  - 验收：实际 Electron 选择合成目录可建立授权，撤销后立即阻止新请求；页面和 helper 状态一致。发布可供 C30-T3 使用的本地桥接版本，但不宣称 G3 已通过。
  - 完成与验收证据（2026-11-11）：Morde Hub 卡片继续复用授权/撤销与在线状态，最近 20 条本机取回记录展示相对路径、来源版本、确认字节、状态和保留期限且不持久化 ticket；已有卡片测试覆盖 Electron directory-picker 回调、授权撤销保留所选目录和访问记录展示。Portal Assistant 任务动态展示设备/版本、进度、期限、副本状态与删除时间，删除需确认并刷新 owner 状态。真实 Node live test 通过 production `mordeHubUpdateGrants` + `morde-sync` 对本机合成目录建立目录身份，实际 `file.fetch` 联到本机服务；本地撤权后 handler 在发出新 HTTP 请求前拒绝。owner Session 列表、完整下载、删除 tombstone 由同一 live test 验证。Portal 页面定向浏览器验收确认主会话固定在顶部、旁路组初始折叠及 +5 分页；Morde Hub/Portal UI 定向测试通过，桌面 Morde 主进程/渲染器测试 86/86、Portal 测试 553/553，Electron 隔离用户目录 build 通过。未使用远端测试站点。

- [x] B30-T4（完成）完成真实 helper 到本地服务的联调
  - 前置：B30-T3、A30-T3。
  - 修改：连接 A30-T3 的实现，使用独立 B 数据库/桥接进程与合成资料；修复仅归 B 的差异，服务器问题交 A。
  - 验收：A8–A10 的真实字节链路通过：完整文件、断线、越界、源变化、撤权和未完成副本不交付；普通文本 file.get 行为保留。向 A30-T4 交付证据。
  - 完成与验收证据（2026-11-11）：新增 live test 以真实 PC handler、生产 helper、真实本地 Go routes 和独立临时 PostgreSQL 验证完整文件字节、断线后按服务端确认序号续传、源变化和 16 MiB+1 越界拒绝、撤权后不再读取/传输、未完成副本不可下载、owner 下载/删除，以及普通文本 `file.get` 保持可用；测试 1/1 通过，下载 PDF 字节与合成源文件逐字节一致，ticket 未进入返回结果、事件或状态。B30 专项 `go test ./client ./cmd/morde-sync`、DSH 包 442/442、Portal 553/553、TypeScript typecheck/build、浏览器 UI 验收均通过，临时 PostgreSQL 与测试进程已清理。服务端 PostgreSQL/API 前序包通过；全量 `make it` 在 `TestCloudDSHCoordinatorRunsBoundedSummarySessionThroughDSH` 失败（worker 并发改动，超出 B30 文件边界），故不记作全仓集成通过。

## 交付

B30-T4 完成且 G3 通过后领取 B40。不能把调整 32 KiB 限制当作完整文件实现。 只汇报本地位置、版本、验证及未完成项；不得外发文件。已有实现通过本任务验证可直接复用，不为勾选任务而重写。

