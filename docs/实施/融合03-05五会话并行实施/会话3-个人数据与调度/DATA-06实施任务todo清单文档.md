# DATA-06：Goal 与计划定义实施 TODO

> 开发量：按一个 AI 会话、前置就绪后的连续开发估算，最多一天；只计实现与交接记录。测试编写、运行和验收单独进行，不计入这一天。
> 范围估算不是完成承诺；发现超出一天时把剩余实现按文件/接口继续合并拆开，保持原验收条件。
> 当前状态：DATA-06.01/.02 本地实现和定向验收已完成。Goal/Schedule PostgreSQL 定向集成通过临时本地 PostgreSQL 16 验证；最新 make lint 通过，make test 仍在 Workspace 当前目录、Store Hub file.put 和 worker 配额 socket 权限用例失败，详见 DATA-01 至 DATA-07 验收记录。
> 直接前置：可先按现有计划模型开发；变更流依赖 DATA-01，公共合同由会话 5 整合。
> 修改边界：server Goal 唯一用例、既有 Schedule 存储和计划同步版本。
> 写入规则：[五会话执行入口](../并行执行入口.md)；只修改本会话的指定文件与本会话文档。公共合同/路由/迁移变更在本文件交接给会话 5。
> 本地约束：只在本地修改和验证，不发送或上传文件，不向远端部署。

## 本次开发

- [x] **DATA-06.01 实现 Goal 唯一用例**：Goal/步骤进展/资料引用采用 `personal_objects` 唯一正文，经通用个人数据 CRUD、所有权、revision、账号变更流与同事务额度核算；正文形状为 `title`（1–200 字符）、`description`（最多 4096 字符）、Goal `status`（active/paused/done/cancelled）、`steps`（最多 100 项，每项含唯一 ID、title、status、note；进展由步骤状态表达，不重复保存数字进度）及 `related_material_ids`（最多 100 个当前账号有效的 library_item ID）。提供 `GET/POST /api/goals`、`GET/PATCH/DELETE /api/goals/{id}`；通用个人对象同步仍支持 Goal snapshot/changes。
- [x] **DATA-06.02 恢复 Schedule 定义与同步**：复用计划存储、业务版本和既有 `once/daily/weekly` 规则；提供列表/创建、读取/更新、启停、运行记录 API。Schedule 正文快照直接投影 `schedules`，不另存副本；定义变更、账号 change log、quota 与幂等更新在同一事务，revision 复用 `Schedule.Version`。授权和运行时字段不进入同步正文。

## 实现交付与验证

- Goal 专用 App/API、账号分页列表和所有权/资料引用事务复用 DATA-05 个人对象账本；Schedule 的 create/update/enable/disable、Stop All 和单次完成写入账号变更流。单次完成保留 run 的触发版本并递增计划定义版本；周期推进不递增定义版本。
- Schedule 更新使用 `Idempotency-Key`，覆盖同请求重放、同键异请求冲突和事务回滚；API/应用/存储层测试与 Schedule 快照合同已同步。
- `go test ./internal/domain/personal ./internal/domain/schedule ./internal/app ./internal/api -count=1` 通过；`make lint`（0 issues）通过；`make generate` 通过且 `sqlcgen` 无差异。
- 临时本地 PostgreSQL 16 上，Goal quota/version/owner/pagination、Schedule occurrence/version/sync projection/启停/回滚/去重定向集成测试通过。临时容器和测试数据库已删除；未访问远端。
- 全量 `go test ./...`、`go test -race ./...`、`make cover` 和完整 `go test ./internal/store/postgres` 均因其他 Workspace/管理员工作区测试失败；DATA-06 定向包与 PostgreSQL 集成通过。完整 Postgres 包失败项包括管理员统计断言、Hub metadata 约束及多项 Workspace 同步/绑定/配额旧断言，未在本任务中改动。
- OpenAPI YAML 可解析，所有本地 `$ref` 均可解析且无重复键。Goal 请求正文不含旧客户端两套消息协议拼接出的 `category/progress/check_*` 字段；跨端源映射仍由 INT-01/F04-T01.03 总体合同继续核对。

## 原任务与已有进度

下列编号用于追溯完整业务规则和拆分前证据；同一原编号涉及多个会话时，各会话只完成自身范围。原项全部实现及验收通过后才可视为完成。

| 原编号 | 原任务 | 拆分前状态 |
|---|---|---|
| [F04-T01.03](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 确定目标与例程合同 | 本会话已冻结 server Goal/Schedule 输入输出及 OpenAPI；完整旧端来源映射仍由 INT-01 汇总 |
| [F04-T02.11b](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | Goal 和 Schedule 接入变更流 | DATA-06 定向 PostgreSQL 集成通过；其余 Agent 写入口依赖仍按原任务跟踪 |
| [F04-T02.12b](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 核对目标和计划同步版本 | Goal revision 与 Schedule.Version 已复用并由定向集成验证 |
| [F04-T03.01](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 实现 Goal 唯一用例 | 本地实现及 Goal 定向 PostgreSQL 验证完成 |
| [F04-T03.02](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 恢复计划定义与启停接口 | 本地实现及 Schedule 定向 PostgreSQL 验证完成 |

## 交接

- 在本文件记录完成的接口、文件和待会话 5 处理的公共改动；不要直接改其他会话负责的文件。
- 对应验证在 [独立验收清单](../验收资料/验收入口.md) 执行并记录，不计入本开发文档的一天预算。
- `server/AGENTS.md` 要求接口、实现和测试在同一变更交付；可先完成代码并标“待验证”，合入/提交前再按独立验收清单补齐测试与所需检查。
- DATA-06 当时将独立授权生命周期留给 DATA-07；DATA-07 已实现安装级 Schedule grant、撤销/续发与触发时授权复核，见 DATA-07。原 F04-T03.04 的完整客户端/阶段验收仍按 V03 单独跟踪。
