# Morde 云端电脑与项目文件同步实施任务 TODO

来源：[需求文档](../Morde云端电脑与项目文件同步需求文档.md)、[设计方案](../Morde云端电脑与项目文件同步设计方案.md)。若早期文档与根目录 `AGENTS.md` 第一里程碑不一致，以第一里程碑为准。本清单只记录实施安排，不表示未完成项已经可用。

## 修改边界

- 用户只管理“云端电脑”；内部继续使用 Host、Workspace、文件版本和现有授权。一台电脑绑定一个 Workspace，多个项目放在该电脑的 `~/workspace/project/<项目名>/`。
- PC 端由用户点击后，将选定目录单向上传；不监听文件变化、不定时上传、不把云端变化下载到 PC。复用现有 `morde-sync` 分块会话、版本提交和安全副本；新项目上传不接入 Syncthing。
- 首次上传遇到云端已有内容时先冲突处理；删除只依据该项目上次成功上传的基线，且不得删除云端独立修改。复制电脑复用 Workspace 文件复制并创建新电脑，不复制旧 Host 的运行时、profile、应用状态、平台凭据、任务或同步关联。
- 只在本地修改和验证，不向服务或设备发送文件、不连接远端。项目上传能力在完整验收前保持关闭；测试用本地替身和元数据，不以编译通过代替传输验收。
- 删除电脑时云端文件及历史版本的处理方式、隐藏/敏感项目文件的默认范围仍待确认；对应操作保持关闭或明确提示。

## 接口与调用方

| 接口 | 变更和归属 |
|---|---|
| `POST /api/hosts`（已有） | Portal 会话可省略 `workspace_ids`，由服务端绑定可用 Workspace 或创建新的；旧版显式单 Workspace 合同继续有效。调用方移除 Workspace 选择。 |
| 会话消息接口（已有） | 电脑选择只影响新请求；服务端按需解析 Workspace。输入框默认值不得改写已受理任务的电脑、授权或输入版本。 |
| `POST/GET /api/hosts/{id}/sync-folders`（已新增） | 注册及列出电脑下的项目 folder；服务端生成 `folder_id` 和 `project/<项目名>` 路径。项目 folder 的 `sync_available=false`，表示不进入 Syncthing。 |
| `POST /api/workspaces/{id}/sync-sessions` 等会话接口（已有，扩展 `folder_id`） | 携带 `folder_id` 时作为显式 PC→云端项目上传：客户端只发项目相对清单，不传绝对路径、删除列表或任意 scope；服务端确定目标路径并从成功基线推导删除。进度查同步会话，不查 Syncthing peer 进度。 |
| `POST /api/hosts/{id}/copies`（已实现） | 用 `Idempotency-Key`、新名称和 `expected_current_revision` 创建或恢复复制；要求源电脑停机且其 Workspace 版本已确认。相同请求可幂等重放；没有单独的查询接口。成功返回独立的新电脑和 Workspace。 |

修改 API、数据结构或状态时，同步更新 `server/openapi/portal.yaml`、生成类型、`docs/实施/00-总览与公共约定.md`、调用方和对应测试。新增接口在 OpenAPI 落地前仍属拟议。

## 可执行任务

- [x] **CFS-01（完成）自动为新电脑准备文件空间**
  - 修改：Host 创建 API、app/store、迁移及 Portal/PC 创建调用；Portal 不再要求用户选择 Workspace，旧显式单 ID 合同保留。
  - 验收：定向测试覆盖省略/显式/空值、权限、配额、重试及创建中断；不会重复建电脑或丢失已有关联。

- [x] **CFS-02（完成）固定新任务的电脑选择**
  - 修改：个人/项目会话消息路由及 Portal 默认选择；沿用 DSH 和现有任务目标围栏。
  - 验收：定向用例覆盖切换电脑、修订已有任务、无电脑纯文字对话和越权；切换不改变已受理任务。

- [x] **CFS-03（完成）保护旧电脑与文件归属**
  - 修改：复用迁移预检和 Workspace/Host 卷登记；归属不明的记录保留，不自动合并或删除。
  - 验收：本地元数据预检覆盖一对一、无绑定、多绑定和旧根目录映射。

- [x] **CFS-04（完成）登记项目 folder 并保留旧同步隔离**
  - 修改：folder API、迁移、授权和设备证明；兼容根目录流程，禁止根目录与项目目录同时写入同一 Workspace。
  - 验收：定向 App/Postgres 用例覆盖幂等、名称冲突、越权、路径篡改及旧接口拒绝。

- [x] **CFS-05（完成）扩展多项目 folder 数据模型**
  - 修改：按 folder 关联设备、挑战、预留和状态，按 Expand/Contract 保留旧合同。
  - 验收：隔离数据库用例覆盖迁移阶段、并发登记、root/project 互斥和撤权后的迟到请求。

- [x] **CFS-06（完成；项目上传能力仍关闭；依赖 CFS-04、CFS-05）接入服务端项目上传与版本基线**
  - 修改：`server/internal/domain/workspace`、`app/workspace_sync.go`、`store/postgres/workspace_sync.go`、迁移及 OpenAPI。服务端从获准电脑和 `folder_id` 解析 Workspace/项目路径；校验文件、目录、硬链接和符号链接目标均不越出项目根；在同一事务内确认 Host 绑定、Workspace revision 和项目基线。首次目标非空则冲突；成功提交后更新基线；并发基线变化、撤权或云端独立修改时拒绝危险删除。继续使用现有额度和安全副本，不建立第二套传输协议。`WorkspaceSyncOptions.ProjectUploadEnabled` 默认关闭；仅显式开放时才通告 `workspace_sync` 与 `project_upload`。
  - 当前证据：domain/app 定向用例通过，含项目越界符号链接拒绝、空项目根允许及默认关闭门禁；`make generate`、OpenAPI/00 更新和 Portal API 类型生成完成。隔离本地 PostgreSQL 用例通过，覆盖项目上传基线 CAS、竞争会话及首次目标非空时零传输冲突；只读元数据和空目录，未传输文件内容。
  - 验收：domain/app 与隔离本地 PostgreSQL 定向用例通过，覆盖 folder 授权、路径越界、首次冲突、基线 CAS、竞争会话及非空目标零传输冲突；只使用元数据和空目录。真实内容传输由 CFS-11 单独验收，当前不开放能力。

- [x] **CFS-07（本地验收完成；真实数据面仍受 CFS-11 门禁；依赖 CFS-06）接入 PC 手动上传**
  - 修改：桌面端复用目录选择器、本地路径校验和 `morde-sync` 分块会话；维护 folder ID、目标电脑及上传恢复状态。用户每次显式点击上传，并看到扫描、提交、冲突或失败状态。
  - 当前证据：`npm run build:test` 通过；项目上传定向桌面测试 7 项、路径边界测试 2 项通过；morde-sync 空 manifest/固定 folder 合同、云端下行动作拒绝、stdio 参数合同定向测试通过。测试覆盖显式点击、重启续传、冲突后的恢复点清理、目录移动/撤权、Host 关机状态和 Host revision 待应用；只使用空目录和元数据，未传输文件内容、未访问远端。
  - 验收：本地替身确认首次目标冲突不会显示完成、断点恢复复用原 session/幂等键/目录映射、目录移动和撤权会在启动前拒绝、请求不接受客户端删除列表、云端下行动作会在传输前拒绝；未点击时不触发上传。真实 Host revision 应用与含文件块端到端验证留在 CFS-11 门禁。

- [x] **CFS-08（完成；依赖 CFS-01、CFS-03）实现复制云端电脑**
  - 修改：复用 Workspace copy reservation 和 Host 创建；在停写围栏下复制已确认 Workspace revision，创建独立 Workspace/Host。新电脑不继承源电脑的任务、同步设备关系、运行时或平台凭据；幂等恢复不能留下伪成功目标。
  - 验收：服务端 app/API 定向用例通过；隔离本地 PostgreSQL 用例覆盖复制预留、源状态围栏、幂等恢复及竞争；失败保留源数据。只复制已确认 Workspace revision，新电脑不继承源 Host 的运行身份、凭据、任务或同步关联。

- [x] **CFS-09（本地验收完成；真实数据面仍受 CFS-11 门禁；依赖 CFS-01、CFS-02、CFS-06、CFS-07、CFS-08）统一客户端概念与入口**
  - 修改：Portal、桌面端及 iOS 将用户入口改为云端电脑；移除独立 Workspace 选择器。PC 显示手动项目上传和真实会话状态；电脑列表接入“复制电脑”，复制前明确提示需要源电脑停止，且可能复制隐藏文件和敏感内容；其他客户端只显示已支持的电脑/文件状态，不伪装成可上传。
  - 当前证据：Portal Hosts 与 mock handler 定向测试 82 项通过，`typecheck`、`lint` 通过；本地 Playwright 创建/删除、复制、API Key 与错误流程 7 项通过，复制流程覆盖停机源电脑、敏感内容提示、幂等键和新电脑显示。桌面端构建及上传定向测试通过；iOS Simulator Debug、UI test bundle 构建和 Swift 单测 12 项通过；隔离 iOS Simulator 资源页 UI 用例 1/1 通过，验证电脑文件就绪/同步中/未关联状态及电脑名称；代码检查确认项目资源新增关联只提供云端电脑选择。所有 E2E 使用本地 mock，不包含文件内容传输。
  - 验收：本地浏览器/替身检查创建、切换、复制确认和结果、入口归属；复制操作使用稳定幂等键，源电脑和目标电脑独立；检查旧数据及文件操作仍受原权限控制。不上载或选择用户文件。

- [x] **CFS-10（本地合同与回归完成；真实数据面验收另属 CFS-11；依赖 CFS-01～CFS-09）完成本地合同与回归**
  - 修改：核对 OpenAPI、迁移、生成类型、Portal/桌面/iOS 调用方和端到端状态。
  - 当前证据：Portal `typecheck`、`lint`、Hosts/mock handler 82 项测试通过；本地 Playwright 创建/删除电脑、复制、API Key 和错误流程 7 项通过（真实 Host 用例安全跳过）；桌面端项目上传 7 项、路径边界 2 项和 morde-sync/stdio 合同测试通过；Go domain/app/API 定向测试通过，Host copy API Key principal 负向用例确认拒绝发生在 Workspace reservation 和 Host 操作之前。OpenAPI 已明确 `folder_id` 项目上传只接受拥有者 Portal Session 或安装绑定 Sync credential，并已重新生成 Portal 类型、通过 typecheck。iOS Simulator Debug、UI test bundle 构建、Swift 单测 12 项和隔离模拟器资源页 UI 1 项通过。浏览器替身确认云端文件未确认时复制入口保持禁用；CFS-06/08 隔离本地 PostgreSQL 定向用例通过，只使用元数据和空目录。未执行真实文件传输，未访问远端。
  - 验收：执行本地定向测试、构建、元数据集成测试及浏览器替身验收；记录未运行项。任何冲突、撤权、超额或状态未知均不得显示成功；待确认的删除/敏感文件规则不得默认开放。

- [ ] **CFS-11（进行中：本地门禁与配置验证完成；远端预检完成，真实传输待环境准备；依赖 CFS-10）保留真实传输发布门禁**
  - 当前证据：生产入口现通过 `workspace_sync_node.project_upload_enabled` 显式配置门禁，`deploy/single/portal.yaml` 默认 `false`。`go test ./internal/config -run '^(TestLoadProjectUploadGateDefaultsClosedAndOpensExplicitly|TestLoadWorkspaceSyncRelayBindAddress)$' -count=1`、`go test ./internal/app -run '^TestProjectUpload' -count=1`、`go test ./internal/api -run '^(TestWorkspaceSyncSessionForwardsProjectFolderID|TestCapabilitiesAdvertiseWorkspaceSyncOnlyWhenProjectUploadGateIsOpen)$' -count=1` 均通过；`go test ./cmd/portal -run '^$' -count=1` 编译通过。门禁打开时还会通告 `workspace_sync`，因此旧桌面根同步和 iOS Workspace 文件工具也会重新可用。2026-11-12 经授权只读预检指定测试服务器：服务未通告 `workspace_sync` / `project_upload`，现有云端电脑未触碰；服务器仅有系统盘，Workspace 同步专用数据挂载及对应配置缺失。部署规范要求同步数据使用独立、固定容量、非 thin-provisioned 文件系统，不能用系统盘上的普通目录代替，因此未部署、未重启服务、未上传测试文件。用户选择为当前 ECS 增加独立盘后再次检查，系统仍只识别 `/dev/vda`；已确认是 Alibaba Cloud ECS，但本地无 Alibaba Cloud CLI、云资源连接器或阿里云凭证配置，ECS 元数据凭证端点也不可用。阿里云控制台仍需用户登录，已打开登录页并等待用户接管。已在 `.local/cfs11-project-fixture-20261112/` 准备不含敏感信息的验收项目目录，待云盘挂载后使用。
  - 兼容边界：门禁关闭时不通告 `workspace_sync` / `project_upload`；开启项目上传门禁时两者同时通告，旧桌面根同步和 iOS Workspace 文件工具也会恢复。无 `folder_id` 的旧 Workspace/Host 同步仍按原合同授权；门禁不代表所有 Workspace 写接口关闭。
  - 验收：待测试服务器提供符合部署规范的独立同步数据挂载，并部署含门禁配置的服务版本后，仅在该测试环境显式设置 `project_upload_enabled: true`，使用合成测试文件经 PC 同步到指定云端电脑，检查新增、修改、删除、冲突、断点恢复、撤权和目标目录；全部真实链路通过前保持门禁关闭，不宣称端到端验收完成。

## 待确认

1. 删除云端电脑时，是否一并删除 Workspace 文件和历史版本，保留多久？
2. 隐藏文件、配置文件及可能含密钥的项目文件，默认上传/复制范围是什么？
