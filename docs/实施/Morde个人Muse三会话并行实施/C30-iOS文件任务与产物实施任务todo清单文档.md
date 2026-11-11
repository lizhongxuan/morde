# C30 iOS 文件任务与产物实施任务 TODO

**会话：C｜阶段：P3｜状态：进行中（C30-T1 已完成；C30-T2 owner session 复验中；C30-T3 联调中）。**

来源：[需求与架构](../../Morde个人Muse需求与架构设计方案.md)；共同规则：[执行总览](00-三会话执行总览.md)；接口：[共享合同](01-共享接口与联调合同.md)。

## 开始条件与修改边界

G2；与 A30/B30 并发。iOS 不获得 PC 根目录或传输凭证。

iOS 的消息、任务文件来源/进度/产物显示和现有打开/预览能力；Swift 类型、测试与本地化。PC 授权只能在 PC 侧建立，C 不写桥接或文件服务。

## 接口变化

消费 K4 v2 的 Task owner Portal Session 文件取回列表、取消、内容读取与删除接口；Task JSON 不内嵌文件取回列表，不能用 client credential 或 PC ticket 代替 Portal Session。`source_directory_name` 仅显示服务端返回的安全 basename；为空时明确显示未提供，不得从相对路径推断目录名。普通请求仍由聊天发送，读取设备/目录沿用既有 PC 授权，不在 iOS 新增绕过授权的入口。

## 任务

- [x] C30-T1（已完成）接入文件任务来源与等待状态
  - 修改：显示已授权来源设备、目录显示名、相对路径、版本、进度、在线/等待/失败原因及取消结果；设备离线不禁止发送其他独立消息。
  - 验收：fixture 中文件等待只影响对应步骤，其他任务继续；无绝对路径/令牌泄露，目录未授权不能显示为可读，取消受理与已取消区分。
  - 证据（2026-11-11）：`C30-OfflineWait-Verified3.xcresult` safe-progress 1/1；`C30-HelperUsers-Verified1.xcresult` offline-wait/cancel 1/1。独立聊天消息、来源目录/相对路径及离线取消状态均通过 UI 验收。

- [ ] C30-T2（进行中；待真实 owner session 复验）展示完整文件结果和副本生命周期
  - 前置：C30-T1、A30-T1。
  - 修改：使用服务端校验后的文件引用接现有预览/产物界面；显示解析失败、来源版本、临时/已保留副本和可用删除入口，按 API 权限操作。
  - 验收：文本、PDF 和选定办公文件的显示路径可验证；完整传输与解析失败不混淆，不能将旧副本标为最新；撤销目录不虚报已删除所有副本。
  - 证据（2026-11-11）：`C30-HelperUsers-Verified1.xcresult` 三条相关 UI 用例 3/3（offline、篡改拒绝并删除副本、raw-only PDF Quick Look）；PDF 正文可见截图：`/tmp/morde-c30-signed-20261111/helper-users-attachments/18225D54-2F0A-49FF-B67B-482F9B618C66.png`。
  - 复核（2026-11-11）：真实 G3 API 要求文件 owner 接口使用无安装 ID 的普通 Portal Session；当前 app 只用安装绑定的 client Web Session，不能通过 owner 文件接口鉴权。保持服务端授权不变，修复双 session 登录与调用后再完成本项。

### 验证限制

- `MinisTests/MIFTaskViewsTests.swift` 的两个定向单测未运行：Minis 宿主 app 构建需要本地 `Configs/ProviderCustomization.xcconfig` 以及 FFmpeg/Rclone Simulator XCFramework；当前工作区缺少这些配置/产物，FFmpeg 本地源码亦缺失。Minis 工程将这些 Framework 无条件加入宿主 app，未发现可仅排除 Multimedia 链接的现有构建设置；首轮 xcodebuild 在测试启动前失败，未计为测试失败或通过。

- [ ] C30-T3（进行中）从 iOS 发起 PC 文件任务联调
  - 前置：C30-T2、A30-T3、B30-T4。
  - 修改：使用 A30-T3 服务与 B30-T4 桥接版本，均在 C 的独立本地 fixture，读取合成目录；不得控制 B 正在开发的桌面资料目录。
  - 验收：覆盖 A8–A10：完整读取、PC 断连、文件变化、越界拒绝和撤权；界面与实际收到的文件版本一致。交付 A30-T4；接口或桥接缺口交 A/B。

## 交付

C 不参与传输凭证签发或 PC 文件实际读取；G3 后领取 C40。 只汇报本地位置、版本、验证及未完成项；不得外发文件。已有实现通过本任务验证可直接复用，不为勾选任务而重写。

