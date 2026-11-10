# 并行 03：项目工作项实施 TODO

先读[并行执行入口](<00-并行执行入口及任务总览.md>)、[融合设计总清单](<../Morde iOS与nanoMuse融合设计实施任务todo清单文档.md>)及[设计稿](<../../Morde iOS设计稿.html>)。本文是 MIF-06、MIF-07 的唯一任务状态来源；下文完整保留总清单原任务和状态，实施结果记录在本文。总清单不另行维护这些任务的第二份状态。

## 并行边界

- 原会话 3 的 `project/nanoMuse/android/src/ios/**` 写入权已释放给用户指定的唯一集成者；当前会话已接管共享 iOS 接线及本清单的实现、验收与状态更新。
- `.staging/03/` 的工作项视图和独立测试由 01 集成者纳入原 `Minis.xcodeproj` / `morde` target；纳入后仍需统一完成本机替身与 Simulator 验收才能勾选。
- 用户明确指定当前会话为唯一集成者，并要求自行解决阻塞项。因此本轮在原会话 4/5、会话 2 的交付均缺失时，由当前集成者统一完成 MIF-07 服务端、OpenAPI、Portal、生成类型和 iOS 接入；保持文件边界与代理拆分，原用户改动不 reset、restore、clean 或覆盖。
- 页面继续复用唯一 `MordeClient`，不新增客户端、预算展示或绕过服务端权限。只在本机工作，不外发、上传、下载、分享或发送文件。
- 先对照设计稿的 `data-view="work"`、`wiSheet` 和相关交互脚本，采用 nanoMuse iOS 风格。预算和费用不展示；好友与 AI 工具会话记录维持占位。只在本地工作，不外发、上传、下载、分享或发送文件。

## 原任务与状态

- [x] **MIF-06（完成：2026-11-05 原工程 `morde` Debug build 与干净 iPhone 17 Simulator 工作项 UI 验收通过；覆盖项目切换、分页、只读权限、过滤、认领/恢复、Task 关联、交付、创建及版本冲突重试）接入项目工作项基础功能**
  - 修改：复用 `MordeProjectsRoom`、`MordeProjectWorkItem` 和现有项目工作项请求，在任务页的“工作项”分段选择项目并展示真实列表、创建、已有阻塞/恢复、关联 Task 和交付操作；按角色限制编辑。抽取或组合页面时不得把现有项目预算展示带入新界面。
  - 验收：本机替身覆盖项目切换、分页、创建、权限不足、版本冲突、关联任务和返回；普通任务与工作项的状态不混用。分类、待认领与认领按 MIF-07 完成后才能视为原稿工作项功能全部完成。

- [x] **MIF-07（完成：2026-11-05 服务端/API、Portal、iOS 和本机浏览器替身验收通过；旧客户端默认响应兼容）补齐原稿筛选和认领**
  - 交接：由会话 4/5 为现有工作项列表/创建响应明确缺陷、需求和历史未分类的表示，以及未认领、已认领、待验收与现有 `open`/`blocked`/`delivered` 的关系。拟扩展现有 `GET/POST /api/projects/{id}/work-items` 的字段；认领优先复用现有 `PATCH /api/projects/{id}/work-items/{work_item_id}` 的 `assignee_id`、`expected_version` 和幂等键，由服务端校验当前项目成员及竞争冲突。当前 `assignee_id` 必填：允许未分配前须明确版本/capability 与 PC、Portal、旧 iOS 客户端兼容方案；历史记录不得猜测分类或改变负责人。
  - 修改：合同、迁移和生成物分别由现有所有者交付后，会话 3 本人或交接后的唯一集成者与本组接入“全部/缺陷/需求/待认领”筛选、新建类型及认领动作；合同未就绪时不显示可误操作的伪筛选或假成功。
  - 验收：本机替身覆盖历史未分类、两种类型、未认领、并发认领的版本冲突、权限不足及旧客户端响应兼容；不把 `open` 等同未认领。

原任务中涉及 `MordeProjectsRoom`、`MordeProjectWorkItem` 和共享请求的修改由 01 集成者实施；03 页面工作者负责上述暂存视图和独立测试。原任务中的服务端职责仍归原五会话的 4、5，不能由本子清单越权执行。

## 交接记录

在本节记录原会话 3 的文件交接、01 冻结的共享接口、会话 4/5 合同与会话 2 调用方状态、已参考设计稿场景、暂存文件、整合验收结果和剩余限制。不要将接口待定或尚未整合的内容记为完成。

### MIF-B01 工作项页面接口需求（供 01 冻结）

- 页面输入：当前账号作用域下可选项目及其加载/分页状态、所选项目 ID、该项目工作项列表及加载/失败状态、当前用户在项目中的角色、当前项目会话 Task ID 和可登记交付的 Host/Workspace ID。只读成员可浏览；编辑权限以现有服务端结果为准。不要向本页提供或展示项目预算、账本或费用。
- 页面动作：按项目 ID 加载/刷新工作项；创建标题和说明；按工作项 ID 提交阻塞原因或恢复；将当前项目 Task 关联到工作项；按现有交付合同登记 Task 与 Workspace 版本；返回任务页。操作结果需能区分成功、权限不足、版本冲突/刷新中和普通失败，避免本地假成功。
- 接线约束：上述状态与回调必须由唯一 `MordeClient` 提供，沿用既有账号 scope、分页、成员权限、`expected_version`、幂等重试及冲突刷新路径；新视图不自行创建客户端或发起平行请求。MIF-B01 冻结的签名和错误语义需覆盖项目切换、分页、创建、只读权限、版本冲突、关联 Task、返回及账号切换时迟到响应。
- MIF-07 的类别和认领字段不属于本接口的既定能力；合同交付前不为它们增加模型属性、筛选状态或动作。

### 2026-11-04 本地审计与写入权交接更新

- 用户已明确授权当前会话担任唯一集成者并先完成 MIF-B01/MIF-01；原会话 3 随后确认暂停 iOS 源码、测试和工程文件写入，在原 iOS TODO 记录释放 `project/nanoMuse/android/src/ios/**` 写入权。已核实该 Codex 任务转为 idle。MIF-B01 已转为进行中，MIF-01 和 MIF-06 尚未开始。
- 当前 iOS 子仓库在多个文件上有未提交修改及未跟踪工程文件；交接记录保留其原样，不 reset、restore、clean 或覆盖。源文件实现仍待本轮按原始未提交基线检查后进行。
- 现有 `MordeApp.swift` 已有项目工作项 DTO、分页读取、创建、阻塞/恢复、关联当前项目 Task、交付及角色校验；`MordeProjectsRoom` 将工作项嵌在项目详情中。相关 DTO 与页面为文件内私有声明，不能直接从独立暂存视图复用，需等待 MIF-B01 提供冻结接口。整个 room 还包含预算区，不能整体嵌入工作项页面；现有实现也没有覆盖本任务要求的独立工作项分段页面和整套本机替身验收。
- 当前服务端合同仍没有工作项类别字段；响应要求 `assignee_id`，状态仅为 `open`、`blocked`、`delivered`。现有 PATCH 是带 `expected_version` 的成员分配/更新，不定义未认领或认领语义。会话 4/5 尚无分类、历史未分类、认领和旧客户端兼容方案的交接，因此 MIF-07 继续阻塞；不得将 `open` 显示成待认领。
- **MIF-B01 工作项页面接口冻结：**在 `MordeApp.swift` 增加唯一共享的 `MIFWorkItemPageStore.shared`。只读输入为 `isSignedIn/scopeKey/projects/projectsState/selectedProjectID/workItems/workItemsState/conversations/conversationsState/selectedConversationID/currentTask/conversationError/actionInFlight/actionError/notice/deliveryTargets`；角色用 `role(in:)`、编辑能力用 `canManage(in:)` 读取。回调为 `load()`、`selectProject(_:)`、`refreshWorkItems(_:)`、`selectConversation(_:)`、`create(projectID:title:description:)`、`block(projectID:item:reason:)`、`resume(projectID:item:)`、`linkTask(projectID:item:taskID:)`、`recordDelivery(projectID:item:target:taskID:)`。使用单一 `MordeClient`、账号作用域、现有分页/权限/版本/幂等与冲突刷新；独立工作项加载和操作不触发预算/账本请求。`MordeClient.portalData` 改用已注入 `urlSession`；Debug 下任意 `-morde.local*` 启动参数启用本机 `URLProtocol`，未配路由返回本地错误。接口当时的 `morde` Simulator Debug build 通过；随后根导航开始引用暂存的 Task/Approval 页面，集成前构建暂时因这些源文件尚未纳入原 target 失败，最终验收待集成后重跑。
- 本次此前已只读核对设计稿的 `data-view="work"`、`wiSheet`、筛选项与创建交互；MIF-07 的类别/认领合同仍未解决，继续保持阻塞。该审计前尚未创建 `.staging/03/` 文件；当前阶段也尚未开始 MIF-06 实现或 Xcode/Simulator 验收，因此 MIF-06、MIF-07 均保持未勾选。

### 2026-11-04 当前会话唯一集成与 MIF-07 合同冻结

- 用户明确指定本会话接管唯一集成工作，并要求补完此前未完成项、主动解决阻塞。因此原“会话 4/5 负责服务端、会话 2 负责 Portal”的边界由当前用户授权覆盖；服务端、Portal、iOS 页面分别委派到独立文件边界，本文仍由主集成者统一更新状态。
- **类别：**新增 `bug`、`request`、`unclassified`。新建请求可指定类别；旧请求未带类别时记为 `unclassified`。既有行也统一展示为 `unclassified`，不按标题或状态猜测。
- **认领：**空 `assignee_id` 表示未认领，非空表示已认领；它与工作状态独立，`open` 既可能已认领，也可能未认领。列表与单项读取都只在 `include_unassigned=true` 时包含未认领项目；默认读取继续排除这些行，使旧的非可选 `assignee_id` 解码器安全。新建用显式 `unassigned:true`；旧请求仍默认由创建者负责。
- **并发与权限：**新增 `POST /api/projects/{id}/work-items/{work_item_id}/claim`，要求 `expected_version` 与 `Idempotency-Key`；只有项目 owner/member 可把未认领项认领给自己，viewer 无权，已分配项不可被抢占。既有 PATCH 负责人变更语义不变。服务能力为 `project_work_items_v2`；能力缺失时新客户端隐藏分类/认领控件，并保持原请求形状。
- **待验收：**当前服务端没有 review/accept/reject 状态或动作。`delivered` 继续表示登记了有效的已保存 Workspace 版本证据，并保持终态；不把它映射成“待验收”。设计稿的完整验收闭环须另立接口与权限合同，MIF-07 仅补类型筛选与认领。
- **实现进度：**工作项视图及策略测试已复制到原工程 `NanoMuse/` 与 `MinisTests/`；Task、审批、工作项页面原 `morde` scheme Simulator Debug 构建通过。新建独立 iPhone 17 Simulator 上使用临时模拟器签名运行 `MIFWorkItemViewsTests`，4 项通过。`CODE_SIGNING_ALLOWED=NO` 会因缺少 App Group entitlement 导致应用启动断言；使用临时 ad-hoc Simulator 签名可通过，不涉及发布证书。
- `MIFTaskWorkItemHome.swift` 已新增任务/工作项分段容器，但还未接入 nanoMuse 根任务入口；MIF-06 仍进行中。MIF-07 后端/Portal/iOS 功能尚未集成和验证，均未勾选。

### 2026-11-05 MIF-06/MIF-07 集成与验收

- **MIF-07 合同与实现：**按上方冻结约定完成服务端领域/应用/API、PostgreSQL 存储和迁移、OpenAPI 与 capability、Portal 生成类型和页面、iOS 兼容 DTO/请求/工作项 store 与视图。能力缺失时 iOS 继续发送旧请求形状并隐藏 v2 控件；默认列表排除未分配记录，显式筛选或认领时才请求未认领项。未增加“待验收”状态；`delivered` 仍表示保存版本证据且为终态。
- **Portal 验收：**`Projects.test.tsx` 与 mocks 定向测试 71 项通过；Portal 与 Workbench 类型检查通过。内置浏览器打开本地 Vite/MSW 替身，验证类别创建、未认领筛选、类别筛选和认领后的版本/负责人更新；仅使用合成数据。
- **服务端验收：**工作项 domain/app 与 API/capability 定向 Go 测试通过；工作项 race 与 PostgreSQL 非数据库测试通过。迁移文件已纳入，但未连接或改动远端服务/数据库，也未对真实 PostgreSQL 实例应用迁移。
- **iOS 验收：**`MIFWorkItemViewsTests` 7 项通过；原 `morde` scheme Simulator Debug 构建通过。新建隔离 iPhone 17 / iOS 27 Simulator 的 `testWorkItemsFilterCreateClaimConflictLinkAndDeliveryLocally` 通过；完整覆盖只读项目、分页、类别与未认领过滤、认领、恢复、关联 Task、登记 Workspace 版本、创建未认领项和版本冲突刷新/重试。此前验收发现末尾工作项按钮被 nanoMuse 浮动底栏遮挡，已在工作项列表内容尾部加入 `NanoMuseBottomBar.height` 滚动余量；修正后的用例通过。根任务/工作项分段 UI 用例亦在同一原工程验收中通过。
- **本地安全边界：**浏览器、iOS 与服务端验证均使用本地替身/合成记录；未访问用户提供的 Portal、主机或 API key，未上传、发送或分享文件。
- **设计稿对照：**参考 `data-view="work"` 和 `wiSheet` 的工作项列表、类型筛选、待认领筛选和创建流程；沿用 nanoMuse 房间头部、卡片与浮动底栏。按已冻结范围不展示预算，也不实现服务端尚无合同的验收闭环。

### 2026-11-05 原工程最终回归

- 共享外壳及辅助功能调整后，在原 `Minis.xcodeproj` / `morde` scheme 的专用 iPhone 17e / iOS 27.0 Simulator 重跑 `testWorkItemsFilterCreateClaimConflictLinkAndDeliveryLocally`，1/1 通过；结果包 `/tmp/MIFWorkItemFinalRegression2.xcresult`。最新 generic iOS Debug 构建通过，结果目录 `/tmp/MIFGenericIOSBuildFinal3`。
- 回归仍记录一条 `Invalid frame dimension (negative or non-finite)`，XCTest 将其关联到工作项标题输入框的点击事件，未提供应用源码栈。该运行时提示不影响本用例通过；MIF-06、MIF-07 状态保持完成。
