# B：同步与云电脑运行时实施任务 TODO

依据：[第一里程碑收口技术设计方案](../../Morde第一里程碑收口技术设计方案.md)、仓库根目录 `AGENTS.md` 和 `server/AGENTS.md`。本清单供第二个会话执行；与此前三会话清单的 A/B/C 划分不同，不修改旧清单及其状态。

## 修改边界与并行约定

| 唯一写入方 | 文件范围 |
|---|---|
| A | `runtime/morde-cloud-runtime/**`；除下列 B 范围外的 `server/**`；公共 `server/openapi/portal.yaml`、`server/migrations/**`、`server/internal/app/store.go`、`server/internal/api/api.go`、`server/cmd/portal/**`、`server/internal/store/postgres/sqlcgen/**`；`deploy/single/portal.yaml`；`docs/实施/00-总览与公共约定.md`。 |
| B（本会话） | `runtime/morde-runtime/**`、`runtime/morde-sync/**`、`images/host/**`、`clients/desktop/**`；`server/internal/domain/workspace/**`、`server/internal/domain/host/**`，以及 `server/internal/app/workspace_sync*.go`、`server/internal/app/host*.go`、`server/internal/api/workspace_sync*.go`、`server/internal/api/host_replicas*.go`、`server/internal/api/hosts*.go`、`server/internal/store/postgres/workspace_sync*.go`、`server/internal/store/postgres/host_*.go`、`server/internal/store/postgres/tx_workspace*.go`、`server/internal/store/postgres/queries/workspace_sync*.sql`、`server/internal/worker/workspace_sync*.go`、`server/internal/worker/docker_workspace_sync*.go`。 |
| C | `web/portal/**`、`web/workbench/**`、`clients/nanoMuse/android/src/ios/**`，包括 Portal 生成类型及 iOS 本地化。 |

- 各会话只更新自己的 TODO；需要修改别人的文件时，在本清单写明路径、接口、预期行为和验证方式，由对应负责人修改。未列出的文件先在 A 的清单确定唯一负责人，不能同时编辑。公共合同、迁移、生成物及总览由 A 唯一写入；本会话提供明确提案和 SQL 查询，C 负责其客户端类型和 Portal 页面。三份清单在本地互相读取，不发送文件。
- 开始前查看并保留全部已有未提交改动；不得重置、清理、覆盖、批量暂存或擅自回滚。测试使用隔离的临时目录、数据库、端口和构建输出；生成器运行前确认不会覆盖其他会话的修改。接口、实现、迁移和调用方可分阶段修改，但交付前必须组成同一完整变更。
- A、B、C 可同时开始独立工作。B-01 先给 A 提供合同和迁移提案，也给 C 提供 Desktop bridge 的输入、状态与错误；A 落定公共合同后继续需要共享字段的联调。Desktop 主进程、preload、原生资源界面归 B；Portal 消费该 bridge 的页面归 C，两个会话不交叉写文件。
- 用户要求任何情况都不外发或发送文件：不向服务、电脑、设备或远端发送文件，不部署或上传；仅运行不会传送文件的本地测试、内存合成清单/哈希和替身。测试前检查现有用例是否触发文件传输。真实同步、设备安装等依赖发送文件的验收保持待验证，`project_upload_enabled` 保持 `false`，不得以替身通过代替真实验收。

## 接口变化

1. **已有** `POST /api/workspaces/{id}/sync-sessions`：拟为项目上传增加 `source_binding_id`；绑定到服务端已验证的安装、owner、folder 和目标，不接受客户端自报的安装身份或删除列表。换本机目录、安装或目标需新的来源绑定与确认；旧数据无归属证明时禁止推导删除。待应用未对账时拒绝新的破坏性同步，允许原幂等键查询/恢复原 session。B 实现，A 负责公共合同及迁移。
2. **已有** `GET /api/workspaces/{id}/sync-sessions/{session_id}`：拟增加与本次目标版本绑定的 Host 应用投影，区分待应用、已确认、冲突和未知，并提供可安全展示的影响项。确认必须匹配 owner、Host、Workspace、绑定版本、应用时 generation 和可核验回执；旧字段继续表示服务端提交。B 提案并实现，A 固定合同，C 消费展示。
3. **已有** Desktop preload 的 `projectUploadTargets`、`selectProjectDirectory`、`uploadProject`、`state`、`onState`、`onSync`：B 在现有 bridge 上按需调整来源绑定和状态投影，C 仅消费确定后的合同。确认框须给出目录名称、项目名、指定电脑、目标路径和实际范围；不暴露本机绝对路径或任意云端删除列表给不可信页面。
4. **已有** Host 副本回执：B 复用版本、绑定、generation 和 `sync_state`；现有整版回执不足以证明部分结果时，仅补必要逐项确认。A 按需补共享 API/迁移，不能用当前 desired/applied 相等代替本次 `target_revision` 的应用证明。

## 实施任务

- [ ] B-01（进行中）明确同步归属、回执和跨会话合同
  - 修改：本清单交接记录；依据 `server/internal/app/workspace_sync_project_upload.go`、`server/internal/store/postgres/workspace_sync*.go`、`runtime/morde-runtime/workspacereplica/controller.go`、Desktop preload 的实际代码列出 A 需要改的公共字段/迁移及 C 需要的 bridge 状态。项目范围为用户选定的完整目录；隐藏及敏感文件的默认包含/排除规则尚未确定，未确定前不得开放上传。
  - 验收：明确不可变的上次 Host 已确认 PC 清单 `B`、本次完整扫描 `L`、Workspace/Host 当前内容 `R`，以及来源绑定、目标版本、待应用记录和幂等边界。使用合成元数据复现“云端独立新增文件被当成删除候选”的现有风险；明确该复现不代表真实文件被删除。A/C 能据提案实施各自合同。
- [ ] B-02（未开始，依赖 B-01 和 A-01 的必要迁移合同）实现来源绑定与安全删除规划
  - 修改：`server/internal/domain/workspace/**`、`server/internal/app/workspace_sync_project_upload.go`、`server/internal/store/postgres/workspace_sync_project_upload.go`、`server/internal/store/postgres/queries/workspace_sync*.sql`、`runtime/morde-sync/**` 及相关测试。复用并保护现有 session 文件/空目录快照；记录上次**Host 确认应用**的 session、来源及摘要，不用整个 Workspace 版本代替 PC 归属清单。一个项目仅一个经确认来源可自动删除，其他来源先冲突或明确迁移。
  - 验收：删除候选只能来自 B 中有、完整 L 中无、且服务端和 Host 均未独立改变的已拥有路径；云端新增及目录内未知内容保留，不递归删除未知文件。初次目标非空即冲突，即使内容相同也不自动接管；源身份变更、旧清单缺失、根目录不可访问、扫描不完整或撤权均不推导删除。阻止目录穿越及符号链接/硬链接逃逸。
- [ ] B-03（未开始，依赖 B-02）使项目提交冲突与待应用记录安全
  - 修改：`server/internal/store/postgres/workspace_sync.go`、`server/internal/store/postgres/tx_workspace*.go`、项目上传 app/API 及测试。提交前在事务内重验绑定、基线和版本并先判断整批冲突；有冲突时保存结果但不提交该批新的文件树变化。服务端提交、`last_upload_revision` 更新与本次 session/Host/绑定/`target_revision` 的待应用关联同事务持久化。
  - 验收：混合冲突和非冲突项时提交前整批无新树变更、确认归属基线不前移；待应用时后续破坏性同步被挡住，原幂等键重查仍可用；旧 Workspace 根同步不受项目合同变化影响。PC 未改但云端独立修改时保持待处理，不将其推进为可覆盖的新基线。
- [ ] B-04（未开始，依赖 B-03）按本次目标版本对账 Host 应用
  - 修改：`runtime/morde-runtime/workspacereplica/controller.go`、`server/internal/api/host_replicas*.go`、Workspace 同步持久化及相关测试。复用现有应用围栏，核对 Host 实际状态；对账时重验 owner、Host、Workspace、绑定版本、应用 generation、授权和本次目标版本，只有可证明完整应用才将 PC 快照推进为新归属基线并清除待应用关联。
  - 验收：`target_revision=9` 而旧响应 `desired=8/applied=8`、`sync_state=conflict`、旧绑定或 generation、失联/撤权、丢回执均不误报完成；Host 高于目标版本时还需核验版本谱系和受影响路径/哈希。部分落盘只报告已确认、冲突和未知项，不声称整体回滚；重复回执或重启对账不重放删除。
- [ ] B-05（未开始，依赖 B-01；最终对账依赖 B-04 和 A-01）接通 PC 手动同步及状态恢复
  - 修改：`clients/desktop/harness/desktop/src/morde-main.ts`、`morde-preload.ts`、`morde-sync.ts`、必要的 `clients/desktop/harness/desktop/resources/**`、`runtime/morde-sync/cmd/morde-sync/**` 和相关测试。沿用用户点击入口，确认目录、电脑、项目名、目标和范围；只单向上传到 `~/workspace/project/<项目名>/`，默认项目名为本机目录名。持久保存来源绑定和待应用 session，按同一幂等键恢复，不启用监听或定时自动同步。
  - 验收：本地替身及内置浏览器或 Electron 本机界面验证选择、确认、手动触发和进行中/待处理/完成/失败；根目录失联、内容冲突、授权失效和无法确认的回执不得显示完成。变更需另一次点击才开始新同步，云端不反向改 PC；B-01 即先给 C 可实施的桥接状态及错误合同，B-05 完成后再联合验证。真实文件传输不执行。
- [ ] B-06（未开始）排查 Host 终端能力与运行状态
  - 修改：`runtime/morde-runtime/runtimeactions/shell_linux.go`、相关 Host Runtime/镜像、`server/internal/app/host*.go`、`server/internal/api/hosts*.go` 及测试；工具 Broker 归 A。核对 cgroup v2、资源控制、`bubblewrap`、`setpriv`、Workspace 及绑定探针，按实际失败类别输出安全诊断；沿用当前 generation 的 Runtime 心跳，不把生命周期 running 等同于在线。
  - 验收：本地替身覆盖能力缺失、授权失效、心跳过期/恢复、generation 变化及 Host 独立故障；不降低隔离、不切换到其他电脑。将 Host API 现有在线状态和诊断结果交给 A/C；真实指定电脑的观察由 A-07 单独记录，不能安装或发送文件。
- [ ] B-07（待验证：依赖 B-02～B-06，真实文件数据面受当前禁令限制）同步发布门禁与交接
  - 修改：本清单交接记录及 B 范围内相关测试；如果开关或公共注册文件不归 B，则向 A 提出具体修改。确认默认关闭 `project_upload_enabled`，在规则、合同和真实验收都未满足时不开放、不宣称完成。
  - 验收：A5–A7 的本地元数据/状态机检查记录具体结果；至少覆盖两次同步后云端独立新增文件保留、首次非空、根目录消失、绑定变化、混合冲突、回执丢失和版本错配。真实目录传输及真实设备验收在当前禁令下标记待验证，不得执行。

## 本会话交接记录

- 给 A 的公共合同、迁移与 SQL 生成需求：未确定；B-01 填写字段、权限、错误、幂等及验证方式。
- 给 C 的 Desktop bridge 输入、输出、状态、错误与本地页面替身：未确定；B-01/B-05 填写。
- 检查结果、候选版本及未完成原因：未开始。
