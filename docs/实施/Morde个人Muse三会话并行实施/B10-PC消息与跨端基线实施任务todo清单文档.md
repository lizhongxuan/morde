# B10 PC 消息与跨端基线实施任务 TODO

**会话：B｜阶段：P1｜状态：完成（2026-11-10；G0 已发布，合同 `muse-a00-v1`）。**

来源：[需求与架构](../../Morde个人Muse需求与架构设计方案.md)；共同规则：[执行总览](00-三会话执行总览.md)；接口：[共享合同](01-共享接口与联调合同.md)。

## 开始条件与修改边界

G0；可与 A10/C10 并发。真实服务联调只在 A10-T2 交付后进行。

clients/desktop/harness/desktop/ 的 morde-main/core/preload 和测试；当前正式入口实际加载的 Web/客户端页面及 API 层；仅 B 负责生成 web/portal/src/api/schema.d.ts。沿用唯一 PC 入口，不同时改造两套聊天产品。

## 接口变化

消费 K1 和已有任务/事件接口。只按 A 公布的正式 OpenAPI 生成类型，拟新增字段先在版本明确的 fixture 中使用；缺接口交 A，不能增加本机 Agent 或第二个后端。

## 任务

- [x] B10-T1（完成）对齐正式 PC 入口和消息 API
  - 修改：确认桌面入口加载的页面，整理实际使用的 API/DTO 映射；复用当前登录与安装凭据。按 K1 保留 client_message_id、前序引用、账号/服务实例归属及稳定重试身份。
  - 验收：类型检查与合成接口测试通过；错误账号、过期身份和重试不会写进另一会话。只改真实生产调用路径，不只修改演示 mock。

- [x] B10-T2（完成）修正发送、待发记录与跨端显示
  - 前置：B10-T1。
  - 修改：当前 PC 聊天页面及 outbox/状态处理；区分本机待发、服务端已受理和任务执行，断线保留消息，任务忙碌时仍可发新消息。查看/刷新只查询，不重新触发任务。
  - 验收：本地 fixture 覆盖连续消息、响应丢失、乱序事件、附件待就绪和重新登录；界面没有虚假的已发送或重复 Task。PC 关闭只断客户端连接。

- [x] B10-T3（完成）使用真实隔离服务完成 PC 基线验证
  - 前置：B10-T2、A10-T2。
  - 修改：运行 A10-T2 提供版本的服务和 B 独立资料目录；通过内置浏览器及 Electron 实际入口验证。缺失服务能力反馈 A，不自行改服务端。
  - 验收：A1/A2/A12 在 PC 链路成立：消息一次受理、关闭后云端继续、重连补取、其他账号不可读；提交可合入的本地版本供 A10-T3。

## 当前执行记录

- 基础代码：`main@273d300216b08f8dcc0689e76058224f5616ce14`；共享合同：`muse-a00-v1`；A10-T2 已完成。
- 正式 PC 入口为 `clients/desktop/harness/desktop` 的 Electron 主进程，载入 Portal `/assistant`；消息发送、outbox、轮询/事件刷新均走 Portal 正式 K1 API，不使用演示 mock 代替生产路径。
- T1 已完成：按 `server/openapi/portal.yaml` 生成 Web API 类型（无 diff）；Portal `npm run typecheck` 通过；K1 API/Assistant/outbox/upload 5 个 Vitest 文件 139/139 通过；Electron 生产 `npm run build`、Morde 测试类型检查/构建及 core/main/recovery/node 测试 68/68 通过。Electron package main 为 `./out/morde-main.js`，实际页面为 Portal `/assistant`。
- Desktop `tsconfig.tests.json` 原先将旧 `src/main.ts`、`src/operator.ts`、`src/windows-updates.ts` 纳入 Morde 包类型检查，并因缺少 `electron-updater` 与 `@computer-use/nut-js` 及旧 updater 类型失败；已将检查范围对齐当前生产 Morde 入口 `src/morde-*.ts`，生产构建仍单独通过。整个 legacy `npm test` 尚未作为本任务验收，PC 的 68 个相关测试已运行通过。
- T2 已完成：Portal 测试覆盖连续消息、忙碌 Task 时继续发送、丢失受理响应复用原身份、前序回执排序、附件待就绪、会话/账号/服务实例隔离、读取/事件重同步；outbox 浏览器并发测试通过，IndexedDB 两标签对同一对话串行化。Electron 关闭路径不发送 Task cancel；A10-T2 K1 E2E 已验证断客户端后持久 route/task workers 继续执行。验证结果：Portal typecheck、5 个 Vitest 文件 139/139、outbox browser 1/1、Electron production build、Morde typecheck/build:test 与相关 Node tests 68/68 均通过。T3 使用 B 自己的 Linux 服务、PostgreSQL、模型替身和 Electron profile；不得连接或复用 A00 fixture。
- T3 已完成（2026-11-10）：只使用 B fixture `morde-b10-u2604`、B PostgreSQL、B Portal、B 模型替身和本地 loopback tunnel；未连接 A/C fixture、远端测试机或真实模型。Portal `/healthz` 与 `/readyz` 均为 200，本地入口 `127.0.0.1:17080` 只转发 B VM 的 `127.0.0.1:7080`；migration version 170。CUA 未提供内置 iab，使用独立 Chrome `B10 Local QA` profile 作浏览器端验收，并另行验证实际 Electron `/assistant` 入口。
  - 通过 Portal 注册、管理员 UI 审批的 synthetic owner-t3 与 peer-t3 账号完成验收；owner 云端执行门为 `enabled/epoch=2`。凭据只留在 B fixture 私有文件，未记录或外传。
  - Portal UI 保存的 owner 模型为 `B10 本地合成模型`（`gpt-4.1-mini`）。Portal 模型能力检查 HTTP 200，stub 精确 probe 返回 text/tools/vision 全部可用；stub 仅处理 B 本地 loopback 请求，不连上游。流式能力未验证。模型凭据只保存在本地 fixture 配置，文档不记录明文。
  - A1（浏览器）：owner `/assistant` 发送合成消息 `mggk1adx`，关联唯一 Task `qjh87x6t`，route=`routed`、Task=`completed`，UI 显示 fixture 回复。使用同一独立 Chrome profile 离开后重开，原消息、Task 和回复恢复；该 Task 在离开前已完成。
  - A2（Electron 断开与重连）：使用独立 `MORDE_TEST_USER_DATA=.local/morde-b10-pc-20261109/profiles/electron-owner-t3` 实际加载 Portal `/assistant`。B10 stub 以 8 秒延迟并在模型完成请求开始时只记录 stage/delay（不记录 body/key）；看到 started 后立即通过 Electron 原生菜单 Quit，Electron main PID 65248 退出。关闭期间 Task `q607y91f` 完成（message `m9orgb7q`，assistant reply `msh12p05`）；等待至少 15 秒后 Portal API 核对 user marker、Task、assistant reply 各一份，再用同一 userData 重开。UI 显示原消息、Task 和完成回复，无重复。
  - A12（账号隔离）：peer 独立登录后 `/api/conversations` 返回空列表；owner conversation `cb698uy1` 的 GET conversation/messages/tasks 对 peer 均为 404。
  - 过程记录：60 秒与 25 秒 delay 高于 Portal 模型请求约 20 秒超时，分别进入 `dsh_recovery_unresolved`；最终验收使用 8 秒 delay。已移除 QA systemd drop-in，stub service active、effective delay 恢复 15 秒。host `.local/morde-b10-pc-20261109/scripts/model-stub.py` 与 guest `/opt/morde-b10-portal/model-stub.py` hash 同为 `e7fda20ef03b6e7770f4730d6cd0e227433edb098fee9ea11000d22bfcef669e`。
  - 无 PC 源码、依赖清单或锁文件变更；未删除或修改额外 synthetic 账号。B 服务与本地 tunnel 保留，供后续本地联调。所有验收记录仅保存在本地，不含凭据。

## 交付

B10-T3 完成后等待 G1，再领取 B20；不在等待期间改 A/C 的文件。 只汇报本地位置、版本、验证及未完成项；不得外发文件。已有实现通过本任务验证可直接复用，不为勾选任务而重写。

