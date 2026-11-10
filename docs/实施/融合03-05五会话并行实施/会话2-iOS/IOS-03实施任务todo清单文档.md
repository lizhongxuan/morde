# IOS-03：iOS 文件、电脑与资源入口实施 TODO

> 开发量：按一个 AI 会话、前置就绪后的连续开发估算，最多一天；只计实现与交接记录。测试编写、运行和验收单独进行，不计入这一天。
> 范围估算不是完成承诺；发现超出一天时把剩余实现按文件/接口继续合并拆开，保持原验收条件。
> 当前状态：进行中（IOS-03.01 文件往返、IOS-03.02 的登录、Workspace 列表和已停止 Host 状态，以及 IOS-03.03 三个 Portal 页面加载已有专用 iPhone Simulator 本地证据；本轮继续验收剩余流程。运行中 Host 工作台 WebView 受本地 Host 未就绪阻塞）。
> 直接前置：页面与本地选择可先实现；真实文件传输与检查点依赖 F02-T10，Host 合同依赖现有服务。
> 修改边界：原 Minis 文件选择/上传/下载、资源库、Host 工作台和普通用户资源设置。
> 写入规则：[五会话执行入口](../并行执行入口.md)；只修改本会话的指定文件与本会话文档。公共合同/路由/迁移变更在本文件交接给会话 5。
> 本地约束：只在本地修改和验证，不发送或上传文件，不向远端部署。

## 本次开发

- [x] **IOS-03.01（已验收）接入显式文件传输**：选择和确认文件范围/摘要，接入上传、已确认检查点、下载和摘要核验；专用 iPhone 18 Pro / iOS 27.0 Simulator 通过 XCUITest 完成 4,194,313 字节合成文件上传及下载，2 个分块，服务端 revision 1，下载文件 SHA-256 与源文件一致。
- [ ] **IOS-03.02（进行中；部分验收）接入资源及电脑**：资源库读取获准 Workspace/Host，使用受限 WebView；未就绪或无权限展示真实状态。运行中 Host 工作台 WebView 与无桌面权限共享 Host 仍待验收。
- [ ] **IOS-03.03（进行中；部分验收）补齐普通用户设置**：Host、模型连接、API Key 三个 Portal 页面已有 Simulator 短期 session WebView 加载证据；成员邀请/撤销、模型连接保存/删除、API Key 创建/撤销和客户端安装撤销仍待本轮验收。已迁入口确认后再收敛重复导航。

## 原任务与已有进度

### 2026-11-02 开发进展

- 已核对本地 Workspace sync API：支持 manifest、幂等 sync session、4 MiB SHA-256 分块、上传块确认、下载动作确认和提交；客户端凭据路由允许显式文件同步。
- F02-T10 的运行验收已按原清单豁免关闭，不能据此宣称真实跨端传输通过；本次只做本地实现和离线验证，不连接测试主机、不传输用户文件。
- IOS-03.02 的 Host/Workspace 页面已有基础实现；IOS-03.03 的模型连接/API Key 服务端接口需要 Portal session，客户端安装凭据不具备访问权限，需按现有安全合同处理并记录边界。
- IOS-03.01：接入明确选择的文件路径与 SHA-256 摘要；上传前复扫，按 4 MiB 分块传输并仅在服务端提交完成后报告检查点成功。下载先写入应用 Documents 临时文件，整文件摘要核验后原子落盘、不覆盖本机已有文件，再确认下载块和动作。
- IOS-03.02：资源页读取 Workspace 清单；Host 入口展示共享权限并在缺少 `desktop:view` 或 Host 未运行时禁用。Portal 页面使用安装级短期会话、无桥接的非持久化 WKWebView 和受限 HTTPS 路由。
- IOS-03.03：设置页接入 Portal `/hosts`、`/connections`、`/apikeys` 页面；沿用现有客户端安装管理。Portal 只拿到短期用户会话，不接触客户端 Bearer 凭据。
- 将传输及 Portal 路由辅助文件注册到 Morde 和 Minis 两个 Xcode target。
- 本地验证通过：`run-morde-workspace-transfer-tests.sh`、`run-morde-portal-navigation-tests.sh`、`run-host-navigation-tests.sh`；Swift 前端解析通过；Morde 与 Minis iOS 27.0 Simulator Debug 构建通过。Minis 的 Simulator 构建使用 `/tmp` 中临时编译的 iSH/FFmpeg 模拟器依赖，没有覆盖仓库设备版依赖；以模拟器专用 ad-hoc 签名补齐 app-group entitlement 后，已安装并启动到 nanoMuse 欢迎页。
- 本地服务准备：在 `/tmp/morde-ios03-acceptance-20261102` 创建隔离 PostgreSQL、HTTPS 代理和合成账号/Workspace；只使用本地服务和数据，未连接测试主机、未发送文件。
- 2026-11-02 本地 UI 验收：用 iPhone 18 Pro / iOS 27.0 专用 Simulator，通过 ad-hoc 签名运行 Morde XCUITest，1 项通过。实际验证客户端登录、Keychain 授权后读取 `/api/me`、Workspace 和 Host；“项目”页显示授权 Workspace，“电脑”页显示真实的“已停止 · AI 控制”状态。Minis UI 测试未运行；此次结果不代表文件传输、Portal WebView 或 Mini 主程序交互已验收。
- 2026-11-02 本地 XCUITest 首轮已通过登录/Workspace/已停止 Host；Minis UI 测试未运行。文件往返验收仍在继续，后续结果见下方 IOS-03.01 通过记录；运行中 Host/WebView 和 Portal 设置尚未验收。
- 2026-11-02 IOS-03.01 UI 验收通过：模拟器实际进入“浏览 → 我的 iPhone → Morde”选择合成文件，明确确认上传范围，服务端提交 revision 1 检查点，再下载并确认；服务端 manifest 与本机落盘文件大小、SHA-256 均匹配。结果包保存在本机 `/tmp/morde-ios03-acceptance-20261102/morde-ios03-file-transfer-final4.xcresult`。
- 修复并验证 Workspace 初始 revision 0 的 Manifest 查询：服务端之前将 0 视作非法值，导致新建空 Workspace 的文件页返回 400；现在允许 0 并回归测试通过。验收代理保留原始 Host，确保本地 TLS bridge 上的同源 POST 满足 CSRF 校验。
- 当前仍未验证：IOS-03.02 的运行中 Host 工作台 WebView 与无权限共享 Host；IOS-03.03 的成员邀请/撤销、模型连接变更、API Key 创建/撤销和客户端安装撤销。IOS-03.01 已完成专用 Simulator 验收，不再列为待验证。
- 2026-11-02 IOS-03.03 页面加载部分验收通过：通过 iPhone Simulator 登录后进入 Morde 账户设置，依次打开 Host、模型连接和 API Key Portal 页面；实际 WKWebView 标题/内容显示正常。隔离服务确认 `/api/me`、`/api/hosts`、`/api/shared-hosts`、`/api/model-connections`、`/api/apikeys` 均以 `auth_kind=session` 返回 200；测试结果包在本机 `/tmp/morde-ios03-acceptance-20261102/morde-ios03-portal-settings-final2.xcresult`。未创建密钥或修改连接/成员。
- 修复 Portal 设置 WebView 的路径校验：原代码用相对 URL 进入绝对 URL 白名单，导致三个页面都显示 `badURL`；现在先解析绝对 URL，再按原同源及路径白名单放行。Simulator 重验通过。

下列编号用于追溯完整业务规则和拆分前证据；同一原编号涉及多个会话时，各会话只完成自身范围。原项全部实现及验收通过后才可视为完成。

| 原编号 | 原任务 | 拆分前状态 |
|---|---|---|
| [F03-T03.05](../../融合03-PC与iOS客户端融合实施任务todo清单文档.md) | 确认本机上下文和文件范围 | 未开始，依赖 F02-T10 |
| [F03-T04.04](../../融合03-PC与iOS客户端融合实施任务todo清单文档.md) | 接入 iOS 显式文件上传 | 未开始，依赖 F03-T04.01、F02-T10 |
| [F03-T04.05](../../融合03-PC与iOS客户端融合实施任务todo清单文档.md) | 接入 iOS 文件下载与预览 | 未开始，依赖 F03-T04.04、F02-T10 |
| [F03-T07.03](../../融合03-PC与iOS客户端融合实施任务todo清单文档.md) | iOS 接入受权电脑和工作台 | 本地 MVP 实现完成，待模拟器/真实会话验收，依赖 F03-T07.01、F03-T03.04 |
| [F03-T07.05](../../融合03-PC与iOS客户端融合实施任务todo清单文档.md) | 迁入 iOS 资源和普通用户设置 | 部分完成：账户页与客户端安装管理已实现；成员、模型连接和 API Key 管理未迁入；待模拟器/真实服务验收；依赖 F03-T02.02、F03-T11.01 |
| [F03-T07.06](../../融合03-PC与iOS客户端融合实施任务todo清单文档.md) | 接入真实 Workspace 文件与同步状态 | 未开始，依赖 F03-T06.06、F03-T07.02、F03-T07.03、F02-T10 |
| [F03-T07.07](../../融合03-PC与iOS客户端融合实施任务todo清单文档.md) | 收敛已迁普通用户重复入口 | 未开始，依赖 F03-T07.04、F03-T07.05、F03-T07.06 |
| [F03-T11.03](../../融合03-PC与iOS客户端融合实施任务todo清单文档.md) | 接入资源、文件和电脑页面 | 本地页面实现完成，待 F03-T04.05/F02-T10 与模拟器验收；依赖 F03-T11.01、F03-T04.05、F03-T07.03 |

## 交接

- 在本文件记录完成的接口、文件和待会话 5 处理的公共改动；不要直接改其他会话负责的文件。
- 对应验证在 [独立验收清单](../验收资料/验收入口.md) 执行并记录，不计入本开发文档的一天预算。
- `server/AGENTS.md` 要求接口、实现和测试在同一变更交付；可先完成代码并标“待验证”，合入/提交前再按独立验收清单补齐测试与所需检查。
