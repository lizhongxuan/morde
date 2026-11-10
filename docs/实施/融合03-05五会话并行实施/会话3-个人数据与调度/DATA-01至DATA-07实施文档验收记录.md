# DATA-01 至 DATA-07 实施文档验收记录

> 验收日期：2026-11-02
> 范围：会话 3 目录内 DATA-01 至 DATA-07 七份实施 TODO、其交接内容及可在本地核对的代码/测试证据。仅本地检查，不访问或部署测试服务器。

## 验收结论

- **文档一致性：通过。** 清除了已经完成的会话交接、同步范围、附件合同和 Goal/Schedule 授权事项中的过期描述；七份任务文档与已记录的本地实现边界基本一致。
- **实施任务最终验收：待验证。** 本轮 make lint 通过（0 issues），但 make test 在 workspacecurrent、Store Hub file.put 和 worker 配额 socket 权限用例失败；race/cover、生成物核对及 PostgreSQL 集成未全部完成。V03 中对应的 F04-T02/F04-T03 原任务仍未勾选，本记录不替代它们。

## 分项结论

| 文档 | 文档审查结论 | 本轮修订/待办 |
|---|---|---|
| DATA-01 | 通过 | 更新同步范围，反映 DATA-02/03 与 DATA-06 已加入的对象；明确会话和消息正文仍不属于个人同步对象。 |
| DATA-02 | 通过 | OpenAPI 已定义个人对象枚举、正文 schema、同步 capability 和水位语义；附件仍由 DATA-04 合同处理。 |
| DATA-03 | 通过 | 更正同步范围，加入 Goal/Schedule，并核实 OpenAPI 与服务端分页实现的描述一致。 |
| DATA-04 | 通过 | 将已经落地的迁移 DDL 代码块标为早期草案，实际结构以迁移和 Store 实现为准。 |
| DATA-05 | 通过 | 将 Goal 专用 API 合同状态改为由 DATA-06 完成。 |
| DATA-06 | 通过 | 将独立 Schedule 授权从“仍属后续范围”更正为由 DATA-07 实现，保留 F04 客户端/阶段验收边界。 |
| DATA-07 | 通过 | 当前状态与共享合同、登录续期修复及会话 2 的导入映射交接一致。 |

## 本轮本地证据

- go test -count=1 ./internal/domain/personal ./internal/domain/schedule ./internal/app：通过。
- go test -count=1 ./internal/api -run 'Personal|Schedule|Goal'：通过。
- server/openapi/portal.yaml YAML 解析：通过。
- 会话 3 七份 TODO 的本地 Markdown 链接：51 个均可解析，无缺失目标。

此前各 DATA 文档记录的临时 PostgreSQL 16 定向集成通过作为历史验收证据保留；本轮 CH_TEST_DATABASE_URL 未设置，因此没有重跑数据库集成。全量 make test 中 internal/api 通过；workspacecurrent 两项路径安全测试、Store 的 TestTaskHubFilePutStateValidationAndReceiptBinding、worker 的 9 项配额 socket 权限测试失败。

## 尚未通过的整体门槛

1. server/AGENTS.md 要求 make lint、make test race cover、make generate 及涉及 Store/迁移时的 PostgreSQL 集成检查通过。本轮 lint 通过；make test 的失败位于 session 3 范围外，本轮没有修改这些模块。race/cover/generate 与 PostgreSQL 集成未运行。
2. 独立验收清单 V03 中相关 F04-T02/F04-T03 项仍未完成；客户端联调、阶段验收不能由会话 3 的定向服务端测试代替。
3. DATA-07 记录的 iOS 旧 Goal 导入 intervalHours 映射仍归会话 2；此处只验收服务端每小时调度合同。
