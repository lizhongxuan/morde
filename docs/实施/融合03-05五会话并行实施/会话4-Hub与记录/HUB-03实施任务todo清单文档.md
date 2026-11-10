# HUB-03：设备写入与 Workspace/Task 围栏实施 TODO

> 开发量：按一个 AI 会话、前置就绪后的连续开发估算，最多一天；只计实现与交接记录。测试编写、运行和验收单独进行，不计入这一天。
> 范围估算不是完成承诺；发现超出一天时把剩余实现按文件/接口继续合并拆开，保持原验收条件。
> 当前状态：部分实现，待 PC-05/F02-T10 目标端写入回执与真实同步联验。`file.put` 保持关闭。
> 直接前置：合同和状态可先做并与 PC-05 并行对齐；真实同步联验需要 F02-T10、PC-02、PC-05 和 HUB-02。
> 修改边界：服务端 PC 写操作专属状态、Hub 合同和云端 dsh 适配；已有 task*.go 修改由会话 3 承接。
> 写入规则：[五会话执行入口](../并行执行入口.md)；只修改本会话的指定文件与本会话文档。公共合同/路由/迁移变更在本文件交接给会话 5。
> 本地约束：只在本地修改和验证，不发送或上传文件，不向远端部署。

## 本次开发

- [x] **HUB-03.01（合同与域验证完成，端到端待依赖）定义受限写入和回执**：Hub domain 定义并测试请求元数据、本机回执和摘要绑定（`go test -race ./internal/domain/hub -count=1` 通过）；真实目标端回执由 PC-05 提供，传输与联验依赖 F02-T10。
- [ ] **HUB-03.02（进行中：持久围栏部分实现，待 PC-05/F02-T10 联验）衔接持久围栏与同步**：迁移 `00102` 为 Task Hub 写入状态预留 Workspace/来源检查点、本机回执摘要、同步 session 和云端 revision，并把 sync session 与操作关联；应用与数据库都将关联同步限制为精确单文件上传。已补充同步提交时的 Task checkpoint CAS 推进，以及 `waiting_user/task_hub_workspace_sync` 在同步结算后的安全自动 claim；锁序、claim 和 Task publish 屏障有定向本地 PG 测试。服务端已接入本机回执校验、持久化及同步等待适配，但真实目标端回执和端到端状态流仍待 PC-05/F02-T10；Hub 广告、dsh 工具和生产数据库 action allowlist 继续关闭。
- [x] **HUB-03.03（已完成：本地交接记录）协调客户端写入**：已把动作元数据、目标端回执边界和写入/同步分界记录在本文件，供 PC-05 对齐；目标端实现仍属会话 1。

### 2026-11-02 本地实现与验证

- `workspace_sync_sessions` 可携带成对的 `task_id` / `task_hub_operation_key`；morde-sync 客户端及服务端应用/数据库都限制为设备 upload、`compare_merge`、精确单文件 scope/manifest，不接受递归 scope、多文件、删除或目录变更。服务端在 Task 与 Workspace 锁序下关联操作，并在同步提交时核对路径、大小、摘要和来源检查点。
- `task_hub_operations` 的新列和约束记录 Workspace 写入阶段及回执摘要；迁移保留现有 action allowlist，`file.put` 仍无法由 Hub/DSH 调用。这样在 PC-05 目标端写入与 F02-T10 传输合同就绪前，不会开放不可恢复的写入路径。
- Workspace sync commit 会阻止其他活动 Task 写入和 pending Task publish 交错；本地 Postgres 定向用例覆盖 publish 两种事务顺序。Task-linked file.put 提交链因动作保持关闭，仍未做真实设备验收。
- dsh 沿用现有 Morde Client Installation 登录和独立 `MORDE_CLIENT_CREDENTIAL`；不复用 nanoMuse Cloud token，也未增加第二套登录流程。
- 定向检查通过：`go test -race ./internal/domain/hub -count=1`、Workspace domain/app 限定测试、morde-sync `client`/`cmd/morde-sync` 测试，以及本地临时 Postgres 的 Hub 操作、单文件 manifest 与 Task publish 屏障测试；`gofmt -d` 和 `git diff --check` 无输出。随后在 `darwin/arm64` 上运行服务端 `make test race cover`：`make test` 首阶段失败并提前退出，race/cover 未运行。失败包括 `workspacecurrent` 的两个删除测试，其 `t.TempDir()` 路径经 macOS `/var -> /private/var` 符号链接，被有意拒绝 symlink ancestor 的安全检查挡住；另有 9 个 Docker Workspace Sync Node fixture 无法设置 quota socket 目录所需权限。当前宿主不能完成 Linux `openat2` 与 Docker 权限验收；未放宽安全检查，也未将全量验收记为通过。
- 2026-11-02 后续 Linux 容器定向 race 检查：在本机 Docker 的 Go 1.27.1 Linux 容器内运行 `go test -race ./internal/domain/hub ./internal/app ./internal/storage/workspacecurrent ./internal/worker -count=1`，四个包通过。这不是 PostgreSQL 集成或服务端全量验收。只读核对发现，现有 Task-linked morde-sync 仅能上传 PC 本地已有的精确单文件；尚无 Cloud Task 内容到 PC 的受授权下行合同、目标端写入回执查询入口或调用入口。F02 的 `workspace-sync` capability 仍关闭，因此 `file.put` 继续关闭。
- 2026-11-02 后续 Hub 持久围栏与凭据复核：Workspace sync commit 通过同一事务内的来源 revision CAS 推进 `tasks.workspace_checkpoint_revision`，再结算 file.put；新增 PG 回归用例覆盖推进和旧来源冲突。`ClaimNextTask` 仅在同 Task 有 `succeeded/cloud_synced` file.put 且没有活动待同步写入时恢复 `waiting_user/task_hub_workspace_sync`，并在锁住 Task 后复查；相关 Task Workspace publish/sync 屏障与 Hub 用例通过本地 PG `go test -race`。claim 用例只在临时隔离测试库移除 action allowlist 约束以构造 file.put 状态，生产迁移和 allowlist 未改。按用户选择沿用 dsh 现有 Morde Client Installation 登录；独立凭据与 nanoMuse Cloud token 隔离，`node --test tests/cloud-morde-hub.test.mjs` 的 5 项通过。目标端写入回执、内容下行合同及 F02/PC-05 真实设备联验仍未完成，HUB-03.02 保持进行中，`file.put` 保持关闭。
- 2026-11-02 服务端本机回执适配（未开放分支）：Cloud dsh broker 源码在 `file.put` 条件分支中先绑定当前 Task Workspace 与来源 checkpoint；成功响应须通过严格 `local_applied` 回执校验，随后持久化回执摘要并写入带 `operation_key` 的 `task_hub_workspace_sync` 等待事件。回执不匹配或数据库持久化失败时将操作标为 unknown 并暂停；持久化和暂停使用无取消上下文。该分支目前不能从生产 ToolBroker 调用：工具注册、名称解析和 action 广告都排除 `file.put`；app 单测直接覆盖 helper，不算端到端调用。数据库 action 约束也保持关闭，PostgreSQL 回执集成测试仅在隔离测试库临时移除该约束。定向 `go test -race` 通过 Hub domain 参数/结果合同、app 回执有效/错误/持久化失败 helper 路径及工具隐藏、本地 PostgreSQL 回执幂等/同步结算/checkpoint CAS/最新 file.put 自动 claim。此前 Darwin 全量验收失败未重跑。
- 2026-11-02 PC-05/F02 依赖复核：PC-05 TODO 的 `.03` 仍未开始；`stdio.go` 没有 Task 写入或按 operation ID 查询持久回执入口，Workspace Sync 的已选根目录身份未绑定到同步写入路径，Task-linked session 仍只支持精确 upload。普通 Workspace download 不携带 Task operation/grant 回执语义，不能替代内容下行合同。F02-T10 按用户要求豁免运行验收并关闭实施项，但既有 Ubuntu peer 连接超时不能视作传输成功。目标端写入/回执查询、Cloud→PC 下行合同与真实设备联验仍未完成；HUB-03.02 保持进行中，`file.put` 不可调用。

## Hub 侧写入合同与边界

- 拟议 action 为 `file.put`，当前保持关闭：不加入可广告动作、不加入 Cloud DSH 工具集或数据库 action allowlist。F02-T10 最终传输合同、会话 5 的持久状态迁移和 PC-05 目标端实现就绪后再开放。
- Hub `to` 字段指定目标安装；调用方身份来自已验证的 Morde 安装凭据，必须与 Task 所有者一致。服务端生成的 Task context 绑定 `task_id`、Task 授权版本、目标 grant 版本和稳定 `operation_id`，目标端必须在执行时重新校验 grant 版本。
- 请求元数据为 `workspace_id`、Workspace 相对 `path`、`size_bytes` 与小写 SHA-256 `sha256`。内容字节不进入 JSON；实际传输复用 F02-T10 最终合同。domain validator 限定路径不含绝对路径、父目录、反斜线或空路径；单文件最多 300 MiB。账号 Workspace 总量 5 GiB 及实际准入以 F02-T10 最终规则为准。
- 目标端只可写入该安装当前已选且身份校验通过的 Workspace 根目录；在动文件前重算收到内容的字节数和 SHA-256，并在覆盖已有文件前完成可恢复安全副本。它还需在本机持久化操作回执。成功回执回显 `operation_id`、Workspace、相对路径、字节数、摘要和目标 grant 版本，`stage` 固定为 `local_applied`；该回执只证明本机落盘，不证明云端同步或 Task 文件交付完成。
- 稳定 `operation_id` 重试前必须查询目标端持久回执；无回执、无法证明或并发变更时保持 `unknown` 并暂停相关 Task，不得盲目重放。只有能关联同一安装、Task 授权版本、Workspace 来源检查点、操作 ID 和内容摘要的同步回执，才可解除同步围栏。
- 已落地的 Hub domain 元数据/回执 validator 位于 `server/internal/domain/hub/protocol.go`，边界测试位于同目录 `protocol_test.go`。它们定义合同但不开放写入。

## 会话 2 / 会话 5 / F02-T10 / PC-05 交接

- 会话 5：为 PC 写操作提供独立持久阶段，不能复用带 Host 外键的 `task_operations` 或伪造 Host，也不能提前开放 action allowlist。迁移 `00102_task_hub_workspace_write_state.sql` 为 `task_hub_operations` 增加 Workspace/path/size/digest、来源检查点、本机回执摘要和云端同步结算列，并在 `workspace_sync_sessions` 保存 Task 操作关联；历史迁移 `00092_task_hub_operations.sql` 不修改。设备安装/令牌绑定、sync session 和 commit receipt 可复用，但普通设备 sync receipt 本身不证明 Task 文件来源。Task Workspace publish receipt 要求 Host/runtime fence，不套用到 PC。
- 会话 2 / Workspace sync：设备 sync commit 同时检查活动 Task operations、活动 `file.put` 和 pending Task Workspace publish receipt。Task publish 与设备 sync 的两种事务先后顺序已有本地 Postgres 集成测试，锁序保持“先安装/设备身份，再 Task，再 Workspace，再 receipt”；目标端写入入口开放后仍需做完整 Task file.put 竞争验收。
- Task-device 写的恢复顺序：先持久登记操作并阻止受影响 Task 继续派发；目标端安全落盘后只进入 `local_applied/sync_pending`；通过现有设备 sync session 提交后，以可关联同一 operation ID、Workspace 与摘要的云端 commit receipt 结算；若来源、检查点或回执不明，保持 unknown 并限制在受影响 Workspace/Task。数据库阶段和 sync link 已预留；目标端回执与真正 file.put 入口仍待 PC-05/F02-T10 完成后接通。
- F02-T10：确认内容传输、大小与 quota、同步提交/检查点以及可验证同步回执合同；当前 `file.put` JSON 不承载内容。
- PC-05：在 `client/**` 和 `stdio.go` 实现目标端选定根目录身份复核、相对路径安全写入、覆盖前可恢复备份、操作 ID 去重/回执查询；本机落盘与云端同步分开报告。Hub 侧只消费该接口，不复制目标端写入逻辑。

## 原任务与已有进度

下列编号用于追溯完整业务规则和拆分前证据；同一原编号涉及多个会话时，各会话只完成自身范围。原项全部实现及验收通过后才可视为完成。

| 原编号 | 原任务 | 拆分前状态 |
|---|---|---|
| [F04-T04.06](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 实现受控 PC Workspace 文件写入 | 未开始，汇总，依赖 F04-T04.06a、F04-T04.06b、F04-T04.06c、F04-T04.06d、F04-T04.06e、F04-T04.06f、F04-T04.06g |
| [F04-T04.06a](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 确定受控文件写入合同 | 未开始，依赖 F04-T04.03d |
| [F04-T04.06b](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 验证本机路径与覆盖保全 | 未开始，依赖 F03-T06.01、F03-T06.04 |
| [F04-T04.06c](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 处理写入回执与未知结果 | 未开始，依赖 F04-T04.06a、F04-T04.06b |
| [F04-T04.06d](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 接入真实 Workspace 文件写入 | 未开始，依赖 F04-T04.06g、F03-T06.06、F02-T10 |
| [F04-T04.06e](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 核验设备写入与 Task 文件围栏 | 未开始，依赖 F04-T04.06d、F02-T10 |
| [F04-T04.06f](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 将 PC 操作接入 Workspace 围栏 | 未开始，依赖 F04-T04.06a |
| [F04-T04.06g](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 拆分本机结算与云端同步 | 未开始，依赖 F04-T04.06c、F04-T04.06f |

## 交接

- 在本文件记录完成的接口、文件和待会话 5 处理的公共改动；不要直接改其他会话负责的文件。
- 对应验证在 [独立验收清单](../验收资料/验收入口.md) 执行并记录，不计入本开发文档的一天预算。
- `server/AGENTS.md` 要求接口、实现和测试在同一变更交付；可先完成代码并标“待验证”，合入/提交前再按独立验收清单补齐测试与所需检查。
