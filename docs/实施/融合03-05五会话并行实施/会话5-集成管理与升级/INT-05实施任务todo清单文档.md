# INT-05：后续合同装配与旧入口收敛实施 TODO

> 开发量：按一个 AI 会话、前置就绪后的连续开发估算，最多一天；只计实现与交接记录。测试编写、运行和验收单独进行，不计入这一天。
> 范围估算不是完成承诺；发现超出一天时把剩余实现按文件/接口继续合并拆开，保持原验收条件。
> 当前状态：INT-05.01/.03/.04 已实现并完成定向验收；INT-05.02 的写入围栏已集成，`file.put` 继续关闭，直到 PC-05/F02-T10 目标端回执与内容传输验收完成。遗留 HUB-05.03 已接入 v3 基线加后续 WAL 恢复，本地 PostgreSQL 合成状态演练通过；F05-T03.06d 的应用级附件/授权验收和目标机全流程演练仍待后续完成。
> 直接前置：按 DATA-04、DATA-05、DATA-06、HUB-03、HUB-04、HUB-05 的具体交接逐项装配；不等待无关模块。
> 修改边界：公共 API/schema/迁移/生成文件及统一 Portal 构建、导航和 capability 注册。
> 写入规则：[五会话执行入口](../并行执行入口.md)；只修改本会话的指定文件与本会话文档。公共合同/路由/迁移变更在本文件交接给会话 5。
> 本地约束：只在本地修改和验证，不发送或上传文件，不向远端部署。

## 本次开发

- [x] **INT-05.01（完成）整合附件、额度和记录合同**：统一已交付的 schema、接口、路由和生成物，保留账号隔离与现有业务唯一来源。
- [x] **INT-05.02（集成完成，写入仍关闭）整合设备写入围栏**：按 HUB-03 与 DATA 会话的具体接口完成公共装配和必要迁移；未取得 PC-05/F02-T10 目标端回执与内容传输证据前，`file.put` 必须保持关闭，不能据此勾选 F04-T04.06。
- [x] **INT-05.03（完成）收敛正式构建入口**：已迁管理功能保留一个正式入口；capabilities 只声明真正可用能力。
- [x] **INT-05.04（完成）装配 HUB-04 Task 记录状态**：新增 owner-scoped `GET /api/me/record-captures`，只返回队列元数据；用 Task 来源键实现重复提交归并，并明确区分 pending 与 stored/failed。写入范围：`server/migrations/00103_record_capture_task_status.sql`、`server/internal/api/{api.go,services.go,admin_records.go,admin_records_test.go}`、`server/cmd/portal/main.go`、`server/openapi/portal.yaml`、`web/portal/src/app/{api.ts,api.test.ts}`、`web/portal/src/api/schema.d.ts`；不修改会话 1 的普通用户页面。

## 原任务与已有进度

下列编号用于追溯完整业务规则和拆分前证据；同一原编号涉及多个会话时，各会话只完成自身范围。原项全部实现及验收通过后才可视为完成。

| 原编号 | 原任务 | 拆分前状态 |
|---|---|---|
| [F04-T01.07](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 确认 03 页面交接与最终映射 | 未开始，依赖 F03-T10 |
| [F04-T02.07](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 实现个人附件上传和读取 | 未开始，汇总，依赖 F04-T02.07a、F04-T02.07b、F04-T02.07c |
| [F04-T02.14](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 落实个人数据存储准入 | 未开始，汇总，依赖 F04-T02.14a、F04-T02.14b、F04-T02.14c、F04-T02.14d、F04-T02.14e、F04-T02.14f、F04-T02.14g、F04-T02.14h |
| [F04-T04.06](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 实现受控 PC Workspace 文件写入 | 未开始，汇总，依赖 F04-T04.06a、F04-T04.06b、F04-T04.06c、F04-T04.06d、F04-T04.06e、F04-T04.06f、F04-T04.06g |
| [F04-T07](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 实现完整记录、独立同意与删除 | 未开始，汇总 |
| [F04-T08.13](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 提供个人数据额度管理 | 未开始，依赖 F04-T02.14a |
| [F04-T10.04](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 收敛管理端和重复构建入口 | 未开始，依赖 F04-T09 |

## 交接

- **HUB-04 Task 记录状态装配（完成）：** Portal 以当前 Session/Client principal 查询 `/api/me/record-captures`，每项只含 `id/task_id/purpose/kind/state/failure_code/captured_at/expires_at`；没有 payload、附件、lease、reservation 或 owner 参数。`queued/leased` 是 pending，只有 `stored` 表示持久记录已写入。`00103` 为带 Task 来源的 queue item 加 `(owner_id,source_task_id,purpose)` 部分唯一索引，并只允许无正文终态 `failed/rejected` 使用零 reservation。`main.go` 将同一 `recordCapture` service 注入 Cloud DSH broker 和 API。
- **遗留 HUB-05.03 恢复路径（实现与定向演练完成）：** `deploy/single/backup/backup.py` 的 v3 bundle 记录 PostgreSQL system ID、时间线、redo/checkpoint LSN，并单独归档连续 WAL。恢复从基线 PGDATA 重放到同系统、同时间线的后续 bundle checkpoint；Workspace/Site 使用后续快照。缺段、来源不一致、时间线变化或后续 checkpoint 早于当前库时，在替换目录前失败关闭。完整事务重放保留新增、删除、撤权等数据库状态，不做表级合并。恢复容器关闭网络并只操作暂存目录。目标机全流程演练与真实附件引用校验仍属于 F05-T03.06d 后续验收范围。
- **本轮本地验证：** `python3 deploy/single/backup/test_backup.py` 26 项通过；PostgreSQL 16.15 临时容器上 `go test ./internal/store/postgres -run 'TestFusedConversationMigration|TestWorkspaceLegacyImport|TestPendingLegacyImportBlockGC|TestMarkLegacyWorkspaceNeedsCleanup' -count=1` 通过；新增 WAL 部署定向测试 4 项通过；完整 `python3 deploy/single/test-portal-deployment.py` 18 项通过（修正了一处与当前 `apply_policy_rules` 函数名不一致的测试断言）；Compose 配置解析、脚本语法检查、Python 编译与本次文件 `git diff --check` 通过。另有本地 PostgreSQL 16.15 PITR 合成状态演练验证后续新增、记录删除标记、同意撤回及 grant 撤销在目标 LSN 均保留；未连接目标机，也未做生产数据演练。
- 在本文件记录完成的接口、文件和待会话 5 处理的公共改动；不要直接改其他会话负责的文件。
- 对应验证在 [独立验收清单](../验收资料/验收入口.md) 执行并记录，不计入本开发文档的一天预算。
- `server/AGENTS.md` 要求接口、实现和测试在同一变更交付；可先完成代码并标“待验证”，合入/提交前再按独立验收清单补齐测试与所需检查。
