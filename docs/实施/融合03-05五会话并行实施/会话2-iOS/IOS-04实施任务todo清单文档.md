# IOS-04：iOS 个人数据与目标例程实施 TODO

> 开发量：按一个 AI 会话、前置就绪后的连续开发估算，最多一天；只计实现与交接记录。测试编写、运行和验收单独进行，不计入这一天。
> 范围估算不是完成承诺；发现超出一天时把剩余实现按文件/接口继续合并拆开，保持原验收条件。
> 当前状态：部分验收。iPhone Simulator 已通过个人资料/记忆、Feed 点赞、Idea 实时版本冲突应用与 tombstone 冲突丢弃、Library 元数据、Goal、额度及同步暂停/恢复主流程；附件传输、云端 Schedule 创建/执行和已受理 Schedule 在暂停同步后的存续仍未验收。
> 直接前置：可先改页面；个人数据依赖 DATA-01–DATA-05，Goal/Schedule 依赖 DATA-06–DATA-07。
> 修改边界：原 iOS 个人资料、动态、点子、资源库、目标例程与个人同步。
> 写入规则：[五会话执行入口](../并行执行入口.md)；只修改本会话的指定文件与本会话文档。公共合同/路由/迁移变更在本文件交接给会话 5。
> 本地约束：只在本地修改和验证，不发送或上传文件，不向远端部署。

## 本次开发

- [ ] **IOS-04.01（部分验收；资料、动态、Idea 冲突恢复与 Library 元数据已验收，附件传输未验收）接入个人数据**：资料、记忆、动态、点子和资源库使用真实账号数据、附件和本地 outbox。
- [ ] **IOS-04.02（部分验收；云端 Goal、Schedule 授权与空目标保护已验收，Schedule 创建/执行未验收）接入目标与例程**：使用云端 Goal/Schedule 和独立授权，旧计划由用户显式选择。
- [ ] **IOS-04.03（部分验收；开关、额度和冲突处理已验收，已受理 Schedule 的存续未验收）接入同步及额度状态**：支持开关、冲突、待上传与额度提示；关闭同步不删除数据或停止已受理任务。

### 2026-11-02 本机 Xcode Simulator 验收

- 设备：iPhone 18 Pro / iOS 27.0 的独立 Simulator；仅使用本地合成账号和本地服务，未连接远端服务或真实模型。
- 冲突恢复后完整回归：`IOS04CloudAcceptanceUITests.testPersonalDataSyncAndCloudGoals` 通过（1/1，122.784 秒），7 个定向 XCTest 一并通过，总计 8/8，结果：`/tmp/morde-ios04-conflict-acceptance-round10-20261102.xcresult`。设备为新建的 `morde-ios04-conflict-final-20261102` iPhone 18 Pro / iOS 27.0 Simulator；仅在该模拟器安装本地测试 CA，应用仍执行正常 TLS 校验。
- 实时版本冲突验收：`IOS04CloudAcceptanceUITests.testLiveRevisionConflictApplyUsesCurrentRevision` 通过（30.655 秒），加 7 个定向 XCTest，总计 8/8，结果：`/tmp/morde-ios04-live-conflict-round7-20261102.xcresult`。UI 测试在登录后加载远端保存标记，制造并发 revision 更新，触发 409，点击“应用此 iPhone 更改”，再从本地服务读取并确认重基后的删除已生效（404）。
- UI 端到端：`IOS04CloudAcceptanceUITests.testPersonalDataSyncAndCloudGoals` 通过（1/1，123.937 秒），结果：`/tmp/morde-ios04-cloud-ui-20261102-final.xcresult`。验证登录、Feed fixture 点赞/取消并读回、Library metadata-only 项展示且不可打开、Idea 首次保存/移除标记、Ideas→Goals 路由、云端 Goal 创建并读回、Schedule 独立授权启用/撤销、空 Host/Model 时禁止导入且保留本地计划、额度页、同步暂停时 outbox 保留/恢复后清空、资料和 GLOBAL.md 记忆写入读回。
- 定向 XCTest：新增 Library metadata `attachment_id` 空/非空编解码回归通过（4/4），结果：`/tmp/morde-ios04-content-tests-20261102.xcresult`；此前已有定向套件 23/23 通过，结果：`/tmp/morde-ios04-minis-targeted-final2-20261102.xcresult`，覆盖账号隔离、outbox 暂停与不覆盖、Feed/idea/library 约束、附件分块摘要及校验、额度解码、Provider 边界和中文术语。
- 修复：点子页 Goals 按钮改用现有 `.nanoMuseOpenRoom` 目标路由；metadata-only Library 编码显式发送空 `attachment_id`，解码还原为 nil；Schedule 启停请求失败现在显示错误并提供重试，不再被静默吞掉。完整 UI 流程在这些改动后再次通过。
- 冲突恢复：将个人数据 409 元数据解码为专用错误；为通用及旧版资料 outbox 提供原子重基（新 mutation ID、当前 revision）与显式丢弃；Feed/Profile/Goal 冲突会继续刷新云端快照，并在 root 显示确认。实时 revision 冲突的 Simulator UI 已验证：选择“应用”后用服务端当前 revision 重发本地删除并从服务端读回确认对象已删除；tombstone 隐藏“应用”操作，只能确认丢弃，UI 也已验证这一保护。账号 scope 切换会清除内存中的旧冲突，过期 mutation 的迟到 409 不会覆盖当前冲突。精确三字段的旧版 409（无 `reason`）也会识别，其他冲突不误判。
- 服务层回归：`go test ./internal/app -run '^TestSchedule' -count=1` 通过，覆盖 Schedule 创建校验、授权、启停和 due 处理；这是服务层测试，不代替 iOS 到实际 Host/Model 的执行验收。
- Schedule 启停失败反馈尚未做故障注入；本次只验证授权启停正常路径和整体构建/页面流程。
- 尚未验收：云端 Library 附件上传/下载；旧本地计划在具备 Host/Model 时实际导入、云端 Schedule 创建/执行及撤销授权后已受理任务的存续。附件传输未执行，遵循本地禁止发送文件的约束；当前本机服务没有 ready Host、关闭 cloud DSH 且没有 text/tools Model，所以不能从 Simulator 安全验证云端 Schedule 执行。
- 冲突测试兼容：本机旧版合成服务的 tombstone 409 缺少当前服务端新增的 `reason` 字段；客户端仅在 `base_revision/current_revision/deleted` 三字段完整且没有额外字段时兼容识别。服务端源码当前版本仍返回明确 reason。
- 风险：最终 UI XCTest 仍记录 `Publishing changes from within view updates is not allowed` 运行时警告；无定位栈，未单独排查。

## 原任务与已有进度

下列编号用于追溯完整业务规则和拆分前证据；同一原编号涉及多个会话时，各会话只完成自身范围。原项全部实现及验收通过后才可视为完成。

| 原编号 | 原任务 | 拆分前状态 |
|---|---|---|
| [F04-T03.05](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 接入旧计划的用户选择 | 未开始，依赖 F04-T03.03、F04-T03.04、F04-T03.06 |
| [F04-T05.02](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | iOS 接入资料和记忆 | 未开始，依赖 F04-T02.01、F04-T02.03、F04-T02.06、F04-T02.09、F04-T02.10、F03-T11.01 |
| [F04-T05.04](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | iOS 接入动态、点子和资源库 | 未开始，依赖 F04-T02.04、F04-T02.07、F03-T11.01 |
| [F04-T05.06](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | iOS 接入目标和例程 | 未开始，依赖 F04-T02.11b、F04-T03.01、F04-T03.03、F04-T03.04、F04-T03.06、F03-T11.01 |
| [F04-T05.08b](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | iOS 接入个人同步开关 | 未开始，依赖 F04-T05.02、F04-T02.06 |
| [F04-T05.09b](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | iOS 显示个人数据额度状态 | 未开始，依赖 F04-T05.02、F04-T02.14b、F04-T02.14c |

## 交接

- 在本文件记录完成的接口、文件和待会话 5 处理的公共改动；不要直接改其他会话负责的文件。
- 对应验证在 [独立验收清单](../验收资料/验收入口.md) 执行并记录，不计入本开发文档的一天预算。
- `server/AGENTS.md` 要求接口、实现和测试在同一变更交付；可先完成代码并标“待验证”，合入/提交前再按独立验收清单补齐测试与所需检查。
