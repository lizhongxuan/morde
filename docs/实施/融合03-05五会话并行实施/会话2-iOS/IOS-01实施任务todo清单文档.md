# IOS-01：iOS 原工程与安全边界实施 TODO

> 开发量：按一个 AI 会话、前置就绪后的连续开发估算，最多一天；只计实现与交接记录。测试编写、运行和验收单独进行，不计入这一天。
> 范围估算不是完成承诺；发现超出一天时把剩余实现按文件/接口继续合并拆开，保持原验收条件。
> 当前状态：待验证（代码与本地验证已完成；设备、签名、真实服务与 Keychain/iCloud 验收未完成，因此原任务保持未勾选）。
> 直接前置：可立即开始；正式签名身份未决定时使用隔离安装，设备和签名验收单列。
> 修改边界：只改原 Minis 的 iOS target、资源、账号/Keychain 和 iCloud 路径；Android target 不改。
> 写入规则：[五会话执行入口](../并行执行入口.md)；只修改本会话的指定文件与本会话文档。公共合同/路由/迁移变更在本文件交接给会话 5。
> 本地约束：只在本地修改和验证，不发送或上传文件，不向远端部署。

## 本次开发

- [ ] **IOS-01.01（代码完成，待设备/签名验收：`AgentWidgetExtension` target/嵌入项、本地 task AppIntents、VoicePrompt intent、Widget 专用 AudioToggle intent、旧 chat/message-list 与终端/rootfs 管理等 UI、iSH kernel、旧 shell/session CLI handlers 及无调用方的 native offload 均已从 Minis 编译输入移除；Feed/Goals/Scheduler 与 Hub 本机执行入口关闭；挂载目录不再桥接 iSH，安全书签和 FileProvider 保留；MCP shell CLI 刷新关闭；个人 provider/key/OAuth 设置入口移除，Cloud relay key 保留。只读会话 intents/shortcuts 保留，并补上显式 session ID 查询与打开时的账号可见性复核。2026-11-02 generic iOS `build`、`build-for-testing CODE_SIGNING_ALLOWED=NO` 成功；存储路径、rootfs 归档、备份凭据、登录隔离和 Host 导航五个本地 harness 及 `git diff --check` 通过；无 `NanoMuse/` 编译告警）收敛原工程和本机执行代码**：保留 Minis 入口、远端会话消息与只读旧会话归档；归档预览限定在归档目录内并拒绝符号链接/越界路径，旧 iSH 用户目录以冲突即失败的方式复制到 Documents，源数据不删除。新手与 Morde 设置页不再提供个人模型/API key/OAuth 配置，头像页不再选择个人图片模型；本机图片路由失败关闭。ChatStore 仍作为会话存储与同步数据源，旧 VM、终端、工具预检和聊天专用索引不在 Minis 编译清单。已移除 rootfs 资源打包，但设备已有 `alpine-rootfs` 保留，归档副本继续本地保存。**待验证**：CoreSimulator 已恢复，但 simulator XCTest 链接仍被仅含 iOS 真机 slice 的 `libish_emu.a` 与 FFmpeg framework 阻断；保留 iSH 链接项是因为移除后真机通用构建出现 `CurrentRoot`/`NativeOffloadUtils` 未定义符号。模拟器交互、真机、正式签名/entitlement 和真实服务登录尚未验收。
- [ ] **IOS-01.02（待验证：Morde 能力检查、安全存储及有序登录/退出按服务实例、账号和安装隔离；修复迟到 SSE 401 不能清理新登录，并以 `run-credential-expiry-tests.sh` 验证 token/scope、取消和待登录围栏，2026-11-02 通过）完成账号和有序登录边界**：真实服务与模拟器登录 UI 验收仍待完成。
- [ ] **IOS-01.03（待验证：定向 key 清理与 CloudKit fail-closed 路径已审查；关闭同步时初始化不再删除 etag cache；移除超出已确认范围的 fresh-install Keychain 前缀清扫；OAuth 迁移在目标 Keychain 写入失败时保留旧值并可重试；2026-11-02 credential-expiry harness 覆盖 Cloud relay key 清理保护并通过，rootfs helper 与备份 importer harness 通过）落实旧模型 key 与 iCloud 边界**：仅由定向迁移删除已确认的旧模型 API key，保留 OAuth、环境变量、MCP、Rclone、代理及其他旧凭据；旧备份不能恢复模型 API key，但仍合并其他允许的密钥且不覆盖本机值。停用旧自动 iCloud 业务写入，保留 AppGroup/附件与远端独有数据线索。**风险**：当前备份导出器仍可能把清理前存在的 provider API key 写进备份；导入器会跳过这些 key，但本项不阻止本地备份文件包含它们。真实 Keychain 和隔离设备 iCloud 验收仍待完成。

### IOS-01.01 后续拆分

| 子项 | 状态 | 文件/接口边界与验收条件 |
|---|---|---|
| IOS-01.01a 本机 Agent 与会话数据解耦 | 代码完成，待设备验收：Feed/Goals/Scheduler 不创建或运行本机 VM/headless 会话，Hub task 请求拒绝；最终 `Minis.SwiftFileList` 不含 `AIChatViewModel*.swift`、旧 VM cache 或聊天专用索引，ChatStore 保留为会话存储与同步源 | `NanoMuseFeed.swift`、`NanoMuseGoals.swift`、`NanoMuseScheduler.swift`、`NanoMuseHubTasks.swift`、`AIChatViewModel*.swift`、`ChatStore.swift`；验收 Morde 会话和只读归档可用且 Minis 不编译旧 VM，设备 UI 待验证。 |
| IOS-01.01b 工具、挂载与 iSH executor 解耦 | 代码完成，待设备验收：MCP shell CLI 刷新及外部目录 iSH bind/unbind 已移除；`ISHKernel.m`、`ISHExecutionCoordinator.swift`、`MinisFsRouter.swift`、`ISHShellExecutor.m`、旧 session CLI 和 shell native handlers 不在 Minis 编译清单；安全书签、FileProvider、旧 rootfs 只读归档保留 | `MCPStore.swift`、`ISHExecutionCoordinator.swift`、`MountedFoldersManager.swift`、`MinisFsRouter.swift`、`RootfsManager.swift`；Minis 保留 `RootfsManager.swift` 的旧数据路径辅助，但没有 kernel 启动、安装/重置 UI 或终端调用方。模拟器/真机验收待完成。 |
| IOS-01.01c Provider 与剩余 native offload 收敛 | 代码完成，待设备验收：个人 key/OAuth UI 已移除，普通 provider 发现 fail-closed，Cloud relay 凭据受保护；无调用方的 Weather/Alarm/Browser/Model bridge、权限管理 UI 与 shell CLI handlers 已从 Minis target 移除，Morde 调用的 ConfigOffloadBridge 保留 | `Providers/**`、`ProviderConfigStore.swift`、`NativeOffloads/**`、`OffloadPermissionManager.swift`、`ISHKernel.m`、语音设置路径；Cloud 模型、语音和视觉继续走共享 Provider 层。设备登录/语音配置待验收。 |

## 原任务与已有进度

下列编号用于追溯完整业务规则和拆分前证据；同一原编号涉及多个会话时，各会话只完成自身范围。原项全部实现及验收通过后才可视为完成。

| 原编号 | 原任务 | 拆分前状态 |
|---|---|---|
| [F03-T01.02](../../融合03-PC与iOS客户端融合实施任务todo清单文档.md) | 恢复 iOS 原工程入口 | 本地原工程/target 已恢复且通用设备构建通过，待模拟器/真机打开验收 |
| [F03-T01.04](../../融合03-PC与iOS客户端融合实施任务todo清单文档.md) | 核对 iOS 原功能与扩展去向 | 待验证：源码与 target 去向盘点完成，依赖 F03-T01.02 |
| [F03-T01.05b](../../融合03-PC与iOS客户端融合实施任务todo清单文档.md) | 核对 iOS 不运行本机 Agent | 本机执行代码围栏完成，待 target 清理及设备验收；依赖 F03-T01.02 |
| [F03-T02.02](../../融合03-PC与iOS客户端融合实施任务todo清单文档.md) | 接入 iOS 登录和能力检查 | 本地代码完成，待真实服务与模拟器验收；依赖 F03-T01.02 |
| [F03-T02.04](../../融合03-PC与iOS客户端融合实施任务todo清单文档.md) | 隔离 iOS 账号数据和退出撤权 | 本地完成，依赖 F03-T02.02 |
| [F03-T02.07](../../融合03-PC与iOS客户端融合实施任务todo清单文档.md) | iOS 接入可靠登录重试 | 待模拟器验证，依赖 F03-T02.02、F03-T02.05 |
| [F03-T08.02](../../融合03-PC与iOS客户端融合实施任务todo清单文档.md) | iOS 定向清理可访问的模型 key | 本地清理与影响提示完成，待隔离 Keychain/设备验收，依赖 F03-T02.02 |
| [F03-T08.03](../../融合03-PC与iOS客户端融合实施任务todo清单文档.md) | 停用 iOS 旧 iCloud 自动业务写入 | 静态 fail-closed 完成，待隔离设备验证；依赖 F03-T01.02 |
| [F03-T08.04](../../融合03-PC与iOS客户端融合实施任务todo清单文档.md) | 防止旧备份恢复被删除的模型 key | 本地完成，依赖 F03-T08.01、F03-T08.02 |
| [F03-T08.05](../../融合03-PC与iOS客户端融合实施任务todo清单文档.md) | 验证旧 iOS 身份的模型 key 清理路径 | 待签名身份与隔离设备验证 |
| [F03-T09.02](../../融合03-PC与iOS客户端融合实施任务todo清单文档.md) | 检查 iOS 构建和扩展 | 设备与 XCTest 编译完成，模拟器/签名/真机待验证，依赖 F03-T01.02 |
| [F05-T02.04](../../融合05-联调数据升级与整体验收实施任务todo清单文档.md) | 盘点 iOS 本地和 iCloud 独有数据 | 未开始，依赖 F03-T01.02 |
| [F05-T03.04](../../融合05-联调数据升级与整体验收实施任务todo清单文档.md) | 演练 iOS 本地数据和旧密钥处理 | 未开始，依赖 F05-T02.04、F03-T08.02、F03-T08.05 |

## 交接

- 在本文件记录完成的接口、文件和待会话 5 处理的公共改动；不要直接改其他会话负责的文件。
- 对应验证在 [独立验收清单](../验收资料/验收入口.md) 执行并记录，不计入本开发文档的一天预算。
- `server/AGENTS.md` 要求接口、实现和测试在同一变更交付；可先完成代码并标“待验证”，合入/提交前再按独立验收清单补齐测试与所需检查。
