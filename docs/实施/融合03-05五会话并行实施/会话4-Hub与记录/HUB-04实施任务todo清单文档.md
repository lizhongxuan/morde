# HUB-04：独立同意与记录采集实施 TODO

> 开发量：按一个 AI 会话、前置就绪后的连续开发估算，最多一天；只计实现与交接记录。测试编写、运行和验收单独进行，不计入这一天。
> 范围估算不是完成承诺；发现超出一天时把剩余实现按文件/接口继续合并拆开，保持原验收条件。
> 当前状态：实现、本机 Cloud DSH Runtime→PostgreSQL 端到端验证完成；生产 Docker supervisor、Portal 与跨端故障验收待完成。同意、独立配额、版本化记录队列、撤回后的读取门禁、Task 完成后的 Cloud DSH 采集、崩溃补偿和 Portal 状态展示均已接入。Task 完成和采集入队仍为两个事务；RecordCaptureWorker 会依据保留的 Cloud DSH `task.result` 事件及完成时同意记录幂等补入队，并用同意版本阻止撤回/重新同意后的旧内容回补。
> 直接前置：可先实现专属模块；共享 schema/路由由 INT-05 整合。
> 修改边界：完整记录专属同意、受控写入、异步队列、期限和额度。
> 写入规则：[五会话执行入口](../并行执行入口.md)；只修改本会话的指定文件与本会话文档。公共合同/路由/迁移变更在本文件交接给会话 5。
> 本地约束：只在本地修改和验证，不发送或上传文件，不向远端部署。

## 本次开发

- [x] **HUB-04.01（实现与本机定向验证完成，跨端验收待做）实现独立用途同意**：区分完整记录、训练贡献等用途，默认关闭，撤回立即影响后续采集和访问。
- [x] **HUB-04.02（本机真实 DSH Runtime→PostgreSQL 验证完成，生产 Docker/Portal 验收待做）实现受控记录和队列**：采集及异步写入均核对同意版本，队列和专用附件计入独立额度与期限。
- [x] **HUB-04.03（本机故障注入验证完成，生产跨端验收待做）隔离记录故障**：满额、队列或额外记录失败不影响普通会话、业务文件与已受理 Task。

## 当前实现与交接

- `domain/record`、`app/record_consent.go`、`app/record_capture.go`、`store/postgres/record_consent.go`、`store/postgres/record_capture.go`、记录专用迁移及 `RecordCaptureWorker` 已提供默认关闭的双用途同意、版本递增、配额预留、异步落库复核、记录专用附件和到期清理。
- 本轮修改 `server/internal/store/postgres/record_access.go`：所有者撤回后，指定用途的记录列表拒绝访问，未筛选列表隐藏已撤回用途，正文、导出和记录专用附件均拒绝读取；所有者仍可请求删除历史记录。对应数据库用例加入 `record_integration_test.go`。
- **生产接线已完成：**`cmd/portal/main.go` 将同一 `RecordCaptureService` 注入 Cloud DSH broker 和状态 API；broker 在用户回合完成后以 Task 持久化授权快照和 owner 绑定采集，不伪造 session principal，只记录目标与最终用户可见结果。队列状态只报告元数据，`queued/leased` 仍为待持久化，只有 `stored` 表示已保存；Task 来源键和同意用途用于幂等归并。`app/agent.go` 未作为接线点。
- **剩余验收：**本机真实 `bin.mjs`、本地模型/supervisor 测试替身、PostgreSQL 实例/checkpoint、完成 Broker、记录队列和状态查询全链路通过；本机配额失败与进程崩溃补偿也有 PostgreSQL 故障注入。生产 Docker supervisor、Portal 跨端操作和部署环境故障注入未执行。崩溃窗口由 `RecordCaptureWorker` 扫描保留的 Cloud DSH 完成事件补偿，不依赖 Task 事务 outbox；若 Task 事件保留期短于 Worker 停机期，仍需改用事务 outbox。`file.put` 仍因缺少 PC-05/F02-T10 目标端内容传输和回执证据保持关闭。
- 验证：`make lint`（0 issues）、`make generate`、Portal 类型检查与 ESLint、mock handler + AccountSettings 定向测试（48 项）、本机 mock 浏览器空态、Go record domain/app/API/worker 定向测试、`go build ./cmd/portal` 通过。Linux Go 1.27.1 容器中 `go test ./...`、`go test -race ./...`、`go test -cover ./internal/...` 全部通过；本机 PostgreSQL 上 `go test ./internal/store/postgres -run '^TestRecord' -count=1` 通过（7.748 秒），撤回/重新同意 race、提交版本复核 race，以及队列并发/幂等/失败状态 race 用例通过；临时容器和网络已清理。
- 2026-11-02 本轮新增验证：真实本机 `bin.mjs` 到 PostgreSQL 的 Cloud DSH 端到端用例通过；`TestCloudDSHCompletedTaskCapturePersistsAndReportsStatus` 覆盖跨事务崩溃恢复、撤回后重新同意版本栅栏和 quota failure，race 模式通过；Worker 单测确认先恢复补偿再领取队列；`make generate` 与 `make lint`（0 issues）通过。
- 全量 `make test race cover` 在 `make test` 阶段失败，未运行全量 race/cover；带本机 PostgreSQL 的 `make it` 也失败于相同环境用例。`workspacecurrent` 两个删除用例在默认 macOS `/var/...` 临时路径失败，改用 `TMPDIR=/private/tmp` 后该包通过；Workspace Sync Docker 测试仍因 macOS 无法保留所需 setgid 权限位失败。记录相关定向测试通过，真实 Cloud DSH 运行及跨端故障注入验收仍待完成。

## 原任务与已有进度

下列编号用于追溯完整业务规则和拆分前证据；同一原编号涉及多个会话时，各会话只完成自身范围。原项全部实现及验收通过后才可视为完成。

| 原编号 | 原任务 | 拆分前状态 |
|---|---|---|
| [F04-T01.05](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 确定记录与同意合同 | 未开始 |
| [F04-T07.01](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 实现独立用途同意 | 未开始，依赖 F04-T01.05 |
| [F04-T07.02](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 实现受控记录写入 | 未开始，依赖 F04-T07.01、F04-T07.06 |
| [F04-T07.06](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 建立记录期限与额度配置 | 未开始，依赖 F04-T01.05 |
| [F04-T07.09](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 处理排队内容与同意版本 | 未开始，依赖 F04-T07.01、F04-T07.02 |
| [F04-T07.10](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 隔离记录故障与正常业务 | 未开始，依赖 F04-T07.02、F04-T07.06 |

## 交接

- 在本文件记录完成的接口、文件和待会话 5 处理的公共改动；不要直接改其他会话负责的文件。
- 对应验证在 [独立验收清单](../验收资料/验收入口.md) 执行并记录，不计入本开发文档的一天预算。
- `server/AGENTS.md` 要求接口、实现和测试在同一变更交付；可先完成代码并标“待验证”，合入/提交前再按独立验收清单补齐测试与所需检查。
