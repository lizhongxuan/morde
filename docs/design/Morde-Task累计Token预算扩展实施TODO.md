# Morde Task 累计 Token 预算扩展实施 TODO

**状态：完成（2026-11-28）。** 本清单落实普通 Task 保持 50,000 Token 默认值，同时允许用户为单个新 Task 显式设定更高的累计输入与输出 Token 上限。高额度仍受单次模型调用能力、Task 时长与工具次数、账户及项目额度约束；Schedule 继续使用 50,000 上限，历史 Task 不迁移。

## 修改边界与接口

- 修改 `task.Budget` 的 Task 校验、Task 存储约束、Task 创建消息接口和 `morde-pc` 对话输入；默认省略预算时仍归一为 50,000。
- Task `budget.model_tokens` 的存储上界采用 PostgreSQL `integer` 的可表示范围（2,147,483,647）；这只是单个 Task 的累计止损上限，不代表单次模型输出值或默认额度。
- `ScheduleBudget`、其 API 校验及数据库约束仍为 1–50,000；原有 Task 预算值保留。
- 对话消息接口沿用现有可选 `budget` 字段；PC 显式设值后通过收件箱、重试和分块上传保持同一预算快照。长回复不发送 Task 预算。

## 任务

- [x] **T1（完成）分开 Task 与 Schedule 的 Token 上限**
  - 修改：`server/internal/domain/task/types.go`、Schedule 的预算校验调用点与领域测试。Task 最大值改为存储上界，默认值独立保留为 50,000；Schedule 专用校验继续限制为 50,000。
  - 验收：领域单测覆盖默认、50,001、存储上界、越界拒绝，以及 Schedule 仍拒绝 50,001。
- [x] **T2（完成）放宽新 Task 存储并更新 API 契约**
  - 修改：新增只放宽 `tasks.budget_model_tokens` 的迁移；更新 OpenAPI Task 请求上界及生成类型，不改 Schedule schema/表，也不修改历史行。
  - 验收：迁移验证新 Task 可存储大于 50,000 的值；迁移前已有 Task 值保持不变；Schedule 超过 50,000 仍被拒绝。
- [x] **T3（完成）接通 PC 对话入口与幂等重试**
  - 修改：`web/portal/src/routes/Assistant.tsx`；为短答/Task 消息增加默认 50,000 的明确数值入口。用户自定义值随原消息写入 outbox，并在重试及长消息上传中保持一致；长回复不附带 Task 预算。
  - 验收：Portal 定向测试验证默认、显式加额、outbox 重试快照、分块上传和长回复边界；实际 `morde-pc` 验证入口可打开、值可设到 120,000 并还原 50,000，切到长回复后预算入口隐藏。实际客户端本轮未发消息，提交载荷由组件与 outbox 测试覆盖。
- [x] **T4（完成）完成集成验证并记录结果**
  - 验收：相关 Go、Portal 测试及隔离 PostgreSQL 迁移升降级检查通过。此改动未部署到远程测试机。

## 验证记录

- 通过：`go test ./internal/domain/task ./internal/domain/schedule`、`go test ./internal/app ./internal/api`。
- 通过：在临时本地 PostgreSQL 16.15 上运行 `go test ./internal/store/postgres -run TestTaskModelTokenBudgetMigrationPreservesRowsAndGuardsDowngrade -count=1 -v`。确认旧 Task 的 50,000 值不变、新 Task 可存 120,000 和 `integer` 最大值 2,147,483,647、Schedule 约束仍为 50,000；有高于 50,000 的 Task 时拒绝降级，清除扩额 Task 后可降级。首次实跑发现 Goose 需要对多行回滚保护使用 `StatementBegin/StatementEnd`，修正后整项通过。
- 通过：Portal 定向测试 `npm test -- --run src/routes/Assistant.test.tsx src/app/message-outbox.test.ts src/app/message-upload.test.ts`，3 个文件、346 项通过；`npm run typecheck` 通过。
- 通过：`npm run generate:api` 与 `make pc`。在实际 `morde-pc` 中打开预算控件、输入 120,000、恢复默认 50,000，并切到长回复确认预算控件隐藏。没有从该 PC 会话发送消息；提交载荷由 Portal 组件、outbox 与上传测试验证。
- 通过：`git diff --check`。未部署远程测试机；线上模型、费用和真实 DSH Task 执行不属于本清单验收。
