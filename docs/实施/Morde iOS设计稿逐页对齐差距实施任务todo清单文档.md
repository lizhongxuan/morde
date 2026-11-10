# Morde iOS 设计稿逐页对齐差距实施任务 TODO

## 目标与状态来源

以 [Morde iOS 设计稿](<../Morde iOS设计稿.html>) 和[逐页对齐需求](<../Morde iOS设计稿逐页对齐需求文档.md>)为页面、组件、按钮数量及位置的依据，使用 nanoMuse iOS 的原生视觉风格。逐页核对现有 iOS 页面，完成已审核的改动，并验证真实交互、加载、失败和返回路径。

本清单补充“严格按设计稿逐页对齐”工作，不替代原融合实施清单。原 MIF 编号及既有完成记录继续以[并行执行入口](<Morde iOS与nanoMuse融合设计并行实施/00-并行执行入口及任务总览.md>)和 01～04 子清单为准；本清单使用 MDA 编号记录页面对齐。按用户最新“这是个新项目、不保留新旧两套逻辑”的要求，本轮最终验收目标为独立 `Morde.xcodeproj` / `Morde` scheme；旧 `Minis.xcodeproj` 及其 MIF 页面只作只读功能参考，旧 target 测试不作为新 App 验收证据。原融合 00 的 target 与文件所有权安排不覆盖本清单的新 Morde target 执行边界。原任务的功能验收通过，也不自动等于 MDA 页面视觉和按钮验收通过。已实现但尚未完成本轮验收的任务仍为 `[ ]`。

本次仅在本地修改和验收。禁止外发或发送文件、访问真实 Portal/Host 及使用真实用户文件；涉及附件或文件操作的验收使用进程内替身、合成账号和内存中的合成数据，不进行实际文件传输。原 `Minis.xcodeproj` 的 `morde` scheme 是 iOS 目标，保留现有用户数据及正在进行的其他工作。

## AI 执行入口

- **执行顺序：** MDA-00 基线已完成；按 MDA-01 → MDA-15 逐页实现和验收，最后执行 MDA-16。每次只推进当前页面及其直接依赖的共享组件，不因组件已经接线或 build 成功而跳过页面验收。
- **单页工作循环：** 对照该页 HTML `data-view` 和相关弹层，清点可见按钮、输入、筛选、行点击及返回位置；再检查 Morde 页面并只修改当前页；使用合成数据覆盖成功、空、加载、失败和权限状态；记录 Morde scheme 的验收结果和页面截图差异后再继续。
- **完成门槛：** 页面交互、真实数据/权限状态、按钮数量与位置、返回路径及设计稿视觉均已核验才勾选。功能用例通过、源码已接线或 build 成功只能记录为阶段证据；拿不到 HTML 可视预览时，视觉项继续待验证。
- **待审核处理：** 按“已确认决定”直接落实用户已审核内容。遇到未审核的设计稿外按钮或可能失去入口的功能，记录名称、页面位置、现有作用、移除影响和建议，等待用户决定后再改该项；不影响的页面继续。缺少真实服务端/Host 合同时保持真实的不可用状态，不伪造成功。
- **工程边界：** 使用独立 `Morde.xcodeproj` / `Morde` scheme。`NanoMuse/` 目录中的共享视图或纯数据代码只有在明确纳入 Morde target 且不带入旧根导航/旧 App Group 时才可复用；不以旧 `Minis.xcodeproj` 的测试结果代替 Morde 验收。

## 当前结论

**iOS 页面尚未全部与设计稿对齐。** 独立 `Morde.xcodeproj` / `Morde` scheme 最近一次全量 UI 套件在本次队列改动前共 25 项：24 项通过、1 项因未提供 IOS-03 凭据而按预期跳过、0 项失败，结果 `/tmp/MDA-turn-integration-20261106.xcresult`。新增文字队列功能随后通过 Morde Simulator 用例，结果 `/tmp/MDA-queue-followup-20261106.xcresult`；服务端已有的活动任务追加/待投递处理相关单测也通过。之后 7 项页面定向回归全部通过，结果 `/tmp/MDA-page-acceptance-20261106.xcresult`，覆盖审批刷新状态、文字队列、云电脑票据、事件链恢复/空态、资源 Workspace、设置几何。真实 IOS-03 登录因没有提供凭据未执行。设计稿已在内置浏览器通过 `127.0.0.1` 回环地址打开并走查主导航及聊天、任务、审批/事件链、资源/Workspace、云电脑演示、工作项、好友、文件库/文件详情、AI 工具会话和设置入口；这些 HTML 样例数据与成功反馈是原型模拟，不能作为 Morde 功能验收，也未替代 Simulator 截图对照。逐页截图与 HTML 设计稿视觉对照仍未完成。

**2026-11-08 最新增量：** MDA-04 Task 详情标题/次级文字/步骤虚线、MDA-07 深色 Host/Workspace 图标对比、MDA-08 认领按钮位置/工作项文字尺寸、MDA-11 文件库菜单/标题几何、MDA-12 个人附件预览页头及 MDA-13 设置行箭头均按 HTML 做局部调整。MDA-07/08/13 的 Morde 综合 UI 用例 5/5 通过（`/tmp/MDA-three-pages-light-20261108.xcresult`）；MDA-07 深色资源页另通过 1/1（`/tmp/MDA-resource-dark-preboot-20261108.xcresult`），深色截图已检查。MDA-13 设置箭头最终按 HTML `.srow` 16px/600 正文字体改为 16pt semibold 深色字形 `›`，值与箭头分别靠右排列；旧 20pt 浅灰版本已被此轮更精确的 CSS 核对取代。上述均为局部字体/颜色微调，页面状态和按钮数量未因此判定完成。Host 扩展状态、工作项额外字段/操作、设置详情与状态、三页全页并排视觉仍未闭环，MDA-07/08/13 保持未勾选。

**2026-11-09 最新增量：** MDA-03 五行任务样例文字间距复验 3/3 通过；MDA-07/08 的真实空态排版、资源多行标题行距及工作项输入字体局部调整已实施，资源空态/Workspace 与工作项失败保留输入用例通过 3/3，移除不代表外层胶囊尺寸的 `TextField` 无障碍框高度断言后，工作项列表/创建表单复验 1/1。MDA-04 三处页面级 Retry 按用户最新决定继续移除，返回列表后重进恢复读取。MDA-12 个人附件与 Task Quick Look 全屏预览的 TabBar 容器 ID 覆盖子项标识均已修复；聊天加载预览取消/切资源、文件库图片/PDF预览/切聊天、Quick Look 四 Tab 可访问性各通过 Morde Simulator 1/1，并与本地 HTML 文件详情页做了截图对照，确认返回/标题、内容卡与四 Tab 的层级、位置和数量一致。MDA-12 页面验收完成；MDA-03/04/07/08 的完整视觉/状态门槛仍按各页任务保持未勾选。

**2026-11-09 MDA-01 增量：** 从 Morde 侧栏移除设计稿外的旧“协作→项目”入口，通知收件箱继续无可见导航入口；项目数据/成员/API 不删，工作项项目选择器仍使用真实项目与权限。侧栏聊天、资源、文件库图形按 HTML SVG 校准；二级页面与 Task 详情不再沿用主 Tab 选中态，任务行在 Morde 根导航推入明确 `.task` 路由，详情底栏恢复为无选中。抽屉动画按 HTML 0.34s cubic-bezier 校准。Morde iPhone 18 Pro / iOS 27.0 定向 UI 回归 4/4 通过（`/Users/zhongxuan_li/Library/Developer/Xcode/DerivedData/Morde-hawfozzytljmicgvrjitsykselwa/Logs/Test/Test-Morde-2026.11.09_07-31-41-+0800.xcresult`），覆盖主导航/好友/工具会话/设置无误高亮、抽屉/搜索分页、隐藏项目入口但保留工作项项目范围、Task 详情高亮。已检查截图 `/tmp/MDA01-nav-final-attachments-20261109/52075715-40CF-4338-9AE0-02076379CCEB.png`（抽屉）及 `3452DB44-D0FF-43DB-882E-89BEF567AA19.png`（好友无选中）、`6A58FC77-B839-4F15-9ADE-32D6EAB6ACB5.png`（Task 详情无选中），并在内置浏览器走查设计稿侧栏与文件库→筛选→聊天 PDF 详情。页面仍缺可用真实登录态以核对真实旁聊/审批角标数据，因此 MDA-01 保持 `[ ]`。

**2026-11-09 MDA-01 图标复核：** 侧栏“工作项”图标由 SF Symbols `checklist` 改为自绘线框剪贴板，外形与 HTML 对应 SVG 一致。`testMordeDesignNavigationRoutes` 与 `testMordeDrawerGeometrySearchAndConversationPagination` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 2/2，结果 `/Users/zhongxuan_li/Library/Developer/Xcode/DerivedData/Morde-hawfozzytljmicgvrjitsykselwa/Logs/Test/Test-Morde-2026.11.09_07-37-59-+0800.xcresult`；侧栏截图 `/tmp/MDA01-workitems-glyph-attachments-20261109/1F56FD23-F3F7-4F0B-9C12-97FDC1630EB1.png` 已目视检查，未改入口数量与行为。MDA-04 三处 Task 详情 Retry 的移除/返回重进恢复，继续沿用既有 3/3 验收证据。

**2026-11-09 MDA-04/MDA-11 增量：** 用户再次确认移除 Task 详情页详情读取失败、事件流失败、未确认历史三处“重试加载”，返回列表后重进触发重读；现有实现与 Morde Simulator 3/3 恢复用例已符合，无需重复改产品代码。MDA-11 文件类型徽标字距由 `.tracking(0.02)` 校准为 HTML `.fic` 的 `.02em`（8.5pt × .02 = 0.17pt）；Morde iPhone 18 Pro / iOS 27.0 上聊天图片/PDF发送、文件库筛选、预览、返回保留筛选及预览中切换 Tab 的用例通过 1/1（`/tmp/MDA11-filetype-tracking-final-20261109.xcresult`），截图已检查。验收测试改用每次唯一的本地合成账号，并修正无 Task 的个人对话不应期待 Task 摘要卡、预览页 Tab 重复无障碍标识时选择首个匹配控件；未改变产品按钮或权限。MDA-11 仍待本机文件行等审核差异与完整页面状态/视觉闭环。

**2026-11-09 MDA-13/MDA-14 增量：** 设置页 51 个新补译/缺译 key 的动态调用已接到 `AppLocalized`，确保遵循应用内语言选择；独立 Morde `testMordeDesignNavigationRoutes` 通过 1/1，截图已检查，但设备/凭证状态、待审核入口去留和整页视觉未完成。Workspace 分享合同复核确认：同一领取人只可在 `claiming` 阶段恢复未完成领取；进入 `claimed` 后 token 对所有人失效，包括接收者本人，接收者从自己的 Workspace 列表使用已创建的永久独立副本。应用层 `TestWorkspaceShare` 测试通过；没有改变 API 或状态机。

**2026-11-09 MDA-03/MDA-15 增量：** 任务列表旧系统回弹分支的强制回退 UI 用例在当前 iOS 27.0 Morde Simulator 通过 1/1，但 iOS 16.0～16.3 runtime 仍未安装。云电脑截图预览原先显示黑图，现改为从当前 WKWebView 层级生成图像；Morde Simulator 截图已目视确认浏览器内容可见，预览图断言通过 1/1（`/tmp/MDA15-capture-drawhierarchy-final-20261109.xcresult`）。MDA-15 仍有真实 Host、设计稿快捷入口位置/交还动作待用户确认、截图写入 Workspace 的授权边界及逐页视觉门槛，不勾选。

**2026-11-09 MDA-07 增量：** 新增仅用于 Morde UI 测试的普通资源页单 Host/单 Workspace 夹具，并取得与 HTML `res` 对照的 Simulator 首屏截图。`testMordeResourceOrdinaryStateMatchesDesignDensity` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA07-ordinary-resource-layout-20261109.xcresult`），截图 `/tmp/MDA07-ordinary-resource-layout-attachments-20261109/40E04AB1-11F5-4B97-B8EE-AE468E88122D.png` 已检查。页头、分组次序、平铺 Host/Workspace 行基本对应；差异是 Host 行多出真实的“AI 正在控制此电脑”状态，云手机和 AI 工具按 API 无数据展示不可用/空态，而设计稿展示演示实例。前者的显示去留仍待审核，后者不伪造状态。未改生产数据或服务端 API，MDA-07 仍保持 `[ ]`。

**2026-11-09 MDA-06/07/15 增量：** MDA-06 按用户答复落实“审批提交后继续显示看事件链，Task 详情确认该审批不再待处理后隐藏；状态未确认或读取失败时继续显示”；确认态隐藏与待确认态保留两项用例和 MDA-15 两项云电脑文案/票据用例在独立 Morde iPhone 17 Pro / iOS 27.0 Simulator 通过 4/4（`/tmp/MDA-status-copy-approval-confirmation-20261109.xcresult`）。MDA-07 “＋”反馈按 HTML `.toast` 置于顶部、使用“暂未实现”短提示并保持 2.2 秒，无 Alert；`testMordeResourceWorkspaceRouteBrowsesSyntheticManifest` 在同版本 Simulator 通过 1/1，截图 `/tmp/MDA-compact-copy-alignment-r4-attachments-20261109/1D3CA0B0-761F-4CE5-B408-C82C2136DD83.png` 已检查。该两项组合结果包另有旧 MDA-15 合成 Host 夹具失败；夹具修复后 MDA-15 和 MDA-06 的新组合结果通过 4/4。本轮 MDA-07 证据路径为 `/tmp/MDA-compact-copy-alignment-r4-20261109.xcresult`。MDA-15 当前用户控制态标题/徽标改为 HTML 对应的“全由你掌控”/“由你控制”，Host 票据错误使用简短“连接失败”，详细错误仍留在资源行。本轮只收敛显示文案/时机，不改审批与控制权限；MDA-06/07/15 因各自其他状态、待审核差异及整页视觉仍未闭环，保持 `[ ]`。

**2026-11-08 MDA-02 图片/PDF 附件流程复验：** 在干净的 iPhone 18 Pro / iOS 27.0 Morde Simulator 上，`testMordeFullLayoutUploadsAndSendsImageAndPDFFixture` 通过 1/1（`/tmp/MDA02-attachments-final3-20261108.xcresult`）。截图 `/tmp/MDA02-attachments-final3-attachments-20261108/DA4E042D-4318-4029-B8E8-1916B3CF4D0C.png` 已检查，确认浅色用户气泡为设计稿灰色、图片与 PDF 均随消息显示且发送状态可见；此结果覆盖合成附件成功路径，不代表真实账号上传、键盘/滚动和权限错误状态验收完成。

**MDA-04 摘要间距复验：** 按本地 HTML 的摘要/步骤节点间距调整后，`testMordeTaskDetailShowsRealStepsWithoutBudget` 在 iPhone 18 Pro / iOS 27.0 的 Morde Simulator 通过 1/1（`/tmp/MDA04-summary-spacing-align-final-20261108.xcresult`）；截图 `/tmp/MDA04-summary-spacing-align-final-attachments-20261108/5D23002A-BB26-407D-B6FE-24F1617E0FAA.png` 已检查。页面级三处“重试加载”仍按用户审核移除；MDA-04 因全页视觉与活动历史/交付物等待审核差异尚未闭环而保持未勾选。

**2026-11-08 审批/资源/工作项增量与集成回归：** 按 HTML 将审批页菜单图形绘为双横线并保留 38pt 命中区；资源 Host/Workspace 行标题到摘要、摘要到状态间距分别校准为 2pt/3pt；工作项创建弹层底色、底部留白、标题输入框边框/占位色、分类间距与主按钮深色按设计稿微调。没有改按钮数量、行为、权限、API 或仍待审核的工作项字段/操作。iPhone 18 Pro / iOS 27.0 Morde Simulator 串行运行任务列表、Task 详情及三种移除 Retry 状态、审批、资源/Workspace、工作项与图片/PDF 文件流程 10 项，9 项通过；唯一失败是 `testMordeFullLayoutUploadsAndSendsImageAndPDFFixture` 在合批时未注入初始附件，失败快照显示空聊天。该用例隔离重跑通过 1/1（`/tmp/MDA-attachment-fixture-isolated-20261108.xcresult`），批次结果 `/tmp/MDA-turn-integrated-20261108.xcresult`；因此将该失败记为合成夹具批次隔离问题，不判为产品附件发送回归。审批、资源和工作项列表/新建弹层截图已检查，截图包分别位于 `/tmp/MDA-turn-integrated-20261108-approval-attachments/`、`/tmp/MDA-turn-integrated-20261108-resource-attachments/`、`/tmp/MDA-turn-integrated-20261108-workitems-attachments/`。Task 详情读取失败返回后重进、事件流自动重连、未确认历史重进恢复均通过；对应三个页面级 Retry 保持移除。MDA-06/07/08 整页视觉、其他状态及各自待审核差异尚未闭环，继续未勾选。

**2026-11-08 MDA-02 聊天页头像与气泡对齐：** 本地 HTML `chat` 的暖色头像由 SwiftUI 局部绘制复现；语音功能仍在长按菜单，空闲态显示设计稿头像，启动/录音时保留原麦克风/波形状态图标和无障碍值。浅色模式用户消息气泡改为设计稿 `#F0F0F2`，深色模式继续沿用 nanoMuse 色板。干净 iPhone 18 Pro / iOS 27.0 Morde Simulator 上 `testMordeChatMultiTurnVisualFixtureMatchesDesignDensity` 在最终状态通过 1/1（`/tmp/MDA-chat-mascot-final3-20261108.xcresult`），七条消息截图 `/tmp/MDA-chat-mascot-final3-attachments-20261108/207B4484-A448-440F-A799-4A65C7E6A796.png` 已检查。`testMordeChatLongPressActionsOpenTaskOptionsAndImporter` 在同轮组合运行通过（头像/菜单改动后、浅色气泡改色前）。复用设备的首次运行因残留待发附件内容显示非视觉夹具，消息数断言失败；干净设备隔离复跑通过，不据此改动产品逻辑。头像/气泡的本地首屏差异已修正，但聊天 Task 摘要卡跳转仍待审核，图片/PDF失败或撤销状态、键盘/滚动状态及全页逐控件视觉尚未闭环，MDA-02 仍未勾选。

**2026-11-08 任务列表/资源/工作项/云电脑综合回归：** MDA-03 活动/完成图标路径、MDA-07 Host 与 Workspace 文本列 1pt 顶距、MDA-08 滚动区顶部 2pt 留白已按 HTML 校准。独立 `Morde.xcodeproj` / `Morde` scheme 在 iPhone 18 Pro / iOS 27.0 的 7 个定向 Simulator 用例通过 7/7（`/tmp/MDA-parallel-pages-regression-20261108.xcresult`），覆盖任务样例/未知状态、资源与 Workspace、工作项列表/创建提交、云电脑 WebView 三入口及返回资源；共导出 11 张测试截图，重点查看 6 张。MDA-03/07/08/15 仍分别有系统运行时、待审核控件、真实 Host/Gateway 或完整逐页视觉等门槛，未勾选整页状态。结果包记录的 `Invalid frame dimension` 可复现在工作项标题输入框获得焦点时；当前日志栈仅定位到 SwiftUICore 与 App `main` 入口、无有效产品源码行，尚不能安全归因或修复；UIKit 子视图警告为另一条 runtime 记录。

**2026-11-08 MDA-04 / MDA-14 增量：** 用户确认移除 Task 详情页详情加载失败、事件流失败、未确认历史的三处页面级“重试加载”，返回列表后重进恢复；前一轮 Morde Simulator 三种状态回归 3/3 通过，业务失败批次“重试”和事件流自动重连保留。Workspace 面包屑现按 HTML 收敛为只显示直系上级与当前目录（“‹ 上一级 / 当前目录”）；根页/`pay` 样例和 `docs/empty` 逐级返回两项 Simulator 回归 2/2 通过，结果 `/tmp/MDA14-parent-nav-20261108.xcresult`，截图 `/tmp/MDA14-parent-nav-attachments-20261108/` 已检查。Workspace 文件排序、变更摘要、目录描述仍受当前 manifest 字段限制；MDA-14 其他状态/完整逐控件视觉未完成。

**2026-11-08 工作项/审批局部视觉复核：** 按 HTML 尺寸调整工作项创建标题为 17.5pt、字距 -0.175pt；审批事件链入口调整为 14pt，同时保留 32pt 命中高度。内置浏览器复核 HTML `work` 的缺陷筛选及创建流程，并检查 `appr` 原型控件；Morde Simulator 审批/工作项页面及创建提交用例共通过 3/3（`/tmp/MDA06-08-visual-type-alignment-20261108.xcresult`、`/tmp/MDA08-workitem-create-after-type-align-20261108-rerun.xcresult`）。对应截图已查看。审批第三动作/历史/摘要与工作项上下文/描述/行操作仍待审核；这些局部调整不能代表页面整体视觉验收，MDA-06/08 保持未勾选。

**2026-11-08 MDA-03/07/13、MDA-11、MDA-15 增量验收：** MDA-03/07/13 的独立 Morde 集成定向回归 6/6 通过（`/tmp/MDA03-07-13-integrated-20261108.xcresult`）；任务/资源色值和设置交互均已并入证据，但各页仍有未完成状态/视觉项。MDA-11 将浅色模式文件库筛选、摘要和默认类型块颜色对齐 HTML，`testMordeFullLayoutUploadsAndSendsImageAndPDFFixture` 通过 1/1（`/tmp/MDA11-library-colors-20261108-r2.xcresult`），截图 `/tmp/MDA11-library-colors-attachments-20261108/8FCBFB61-FBCD-4AB8-ACB6-8909C51E3BE6.png` 已检查。MDA-15 本地 Workbench 视觉 fixture 补齐浏览器标签/地址栏和显示器图标，并等待 WebView 完成加载后采集；带合成 Host 匹配 Task 的页面视觉用例通过 1/1（`/tmp/MDA15-browser-chrome-20261108-r4.xcresult`），截图 `/tmp/MDA15-browser-chrome-attachments-20261108/34EA436A-9019-4367-8F90-BF059B58D45D.png` 已检查。均未访问真实 Host、Portal 或用户文件。

**2026-11-07 页面复核：** 5 项 Morde 定向用例 5/5 通过（`/tmp/MDA-browser-logic-validation-20261107.xcresult`），另有工作项筛选/创建弹层用例复跑 1/1 通过（`/tmp/MDA-workitems-visual-capture-20261107.xcresult`）。MDA-07 已将中文标题改为“云端主机”、移除设计稿没有的“云手机”独立标题，并用 Morde Simulator 验证不可用状态行仍位于 Host 与 Workspace 之间；本轮修复并验证 Host 控制状态错误只显示一次。截图仍使用包含多种 Host 生命周期状态的验收夹具，完整列表视觉和 Host 行额外控制状态仍待审核。MDA-08 当前真实账号没有项目，Device Hub 可见窗口仍只能显示真实空态；合成列表、创建弹层及列表失败/恢复截图现已补齐。实际列表与设计稿差异、额外字段/操作审核、行操作和键盘专项验收仍未完成，因此页面任务保持未勾选。

**2026-11-07 云电脑移动端首轮对齐：** 按用户要求隐藏移动端 Workbench 的主机续租按钮、Host 到期倒计时、控制租约倒计时、版本号及长说明，重复的主机名/运行态改由原生页头呈现，Workbench 保留简短控制状态、接管/交还和错误恢复入口；控制权安全租约及自动续期逻辑未改。Morde 原生云电脑页头改为居中控制状态、Host 与已绑定 Workspace 副标题，右侧关闭；加载提示缩短并提高对比度。内置浏览器用本机 MSW 数据在 390×844 确认续期/倒计时/冗余信息不可见，桌面默认视口复核原有续期与倒计时保留；Workbench 顶栏用例 7/7、typecheck 通过；Morde Simulator 的进入/返回、WebView 加载失败、票据拒绝 3/3 通过，结果 `/tmp/MDA-cloud-computer-final-20261107.xcresult`，页头截图 `/tmp/MDA-cloud-computer-final-attachments-20261107/268A424B-89CB-4582-8A31-C5ED75A84E7F.png`。只使用合成数据，未连接真实 Host。后续 MDA-15 移动端入口与窗口工具收敛见页面执行记录。停止 Task 已复用当前会话匹配 Host 的现有 Task 取消接口并通过合成 UI 验收；没有独立“急停”API。桌面截图仅在设备内预览，不保存到 Workspace。真实 Host 仍未连接验收，MDA-15 不勾选。

**2026-11-07 审批与 Workspace 局部视觉复核：** HTML 审批事实区行高为 1.65，本轮将 Morde 对应事实文字行距调整到相同节奏；Workspace 容量摘要字号对齐原型 `.row .tt span` 的 14px。独立 Morde iPhone 18 Pro / iOS 27.0 Simulator 定向回归 4/4 通过，覆盖已确认/未确认审批态、Workspace manifest 和共享 Host 控制，结果 `/tmp/MDA-approval-resource-alignment-derived/Logs/Test/Test-Morde-2026.11.07_22-54-37-+0800.xcresult`。已检查审批确认态截图 `/tmp/MDA-approval-resource-alignment-attachments/FF4F8B52-2C45-4C96-9248-9DF8EBE3FCD6.png` 与 Workspace 根目录截图 `/tmp/MDA-approval-resource-alignment-attachments/922A0084-BB46-4289-AF31-CF3105434986.png`。此为局部视觉调整和状态回归；审批媒体直达入口/额外动作与历史仍待审核，Host 行额外文字及两页的完整逐控件视觉核对仍未完成，MDA-06/07 不勾选。

最近一次 MDA-01/02/06 定向串行回归为 4 项全部通过，结果包 `/tmp/MDA-nav-approval-derived/Logs/Test/Test-Morde-2026.11.06_09-12-42-+0800.xcresult`；覆盖导航与占位入口、抽屉几何/分页/搜索、聊天长按菜单/文件选择器、审批列表与事件链入口。该定向回归不替代全量回归或逐页截图验收。

本轮合并后又在独立 Morde target、ad-hoc 签名、iPhone 18 Pro / iOS 27.0 Simulator 上完成两组串行定向回归：导航/设置 2/2 通过，任务详情、资源 Workspace、工作项、聊天菜单、Host WebView、图片/PDF 聊天及文件库预览 6/6 通过；结果分别为 `/tmp/MDA-current-signed.xcresult` 和 `/tmp/MDA-current-pages-signed.xcresult`。尚未重跑完整 UI suite；本轮源码和交互变化已 build 并进入上述 8 个 Morde UI 用例。无签名的首次尝试因本地认证替身未生效而出现 DNS 错误，未作为产品失败结论，随后已用 ad-hoc 签名复验通过。除已完成的占位页及文件库/附件详情截图对照外，其他页面的逐页设计稿截图对比仍未完成。

其后又完成两项独立 Morde UI 复验：Workspace 目录浏览及标题长按上传/版本菜单 1/1 通过，结果 `/tmp/MDA-turn-derived/Logs/Test/Test-Morde-2026.11.06_10-36-10-+0800.xcresult`；任务列表跨账号迟到分页隔离 1/1 通过，结果 `/tmp/MDA-account-switch-safe-area.xcresult`。后者同时修正账号设置列表底部操作被自定义 Tab 栏遮挡的问题，为列表增加 86pt 底部安全区，并让用例滚动到退出入口完全露出后再操作。两项均为 iPhone 18 Pro / iOS 27.0 Simulator 合成账号验证；没有重跑全量套件，也没有完成 HTML 与 Simulator 的逐页视觉对照。

MDA-00 基线已完成；MDA-01 路由、MDA-02 聊天附件与同账号跨会话及跨账号草稿隔离、MDA-03 刷新恢复、跨账号迟到分页与权限撤销边界、MDA-04 任务详情、MDA-05 事件链账号隔离、节点状态、七行密度、空任务、无历史、加载及网络错误恢复、MDA-06 审批（含列表失败后下拉恢复）、MDA-08 工作项、MDA-11/12 个人文件证明与权限拒绝、MDA-14 Workspace 浏览、标题/文件行菜单操作及非法路径拒绝、MDA-15 云电脑票据进入/关闭、票据拒绝及 Host 匹配 Task 停止状态均有独立 Morde Simulator 证据；MDA-13 部分设置行也有交互证据。MDA-03 错误态刷新修复已在 iPhone 18 Pro / iOS 27.0 的定向及全量 UI 回归中通过，跨账号迟到分页隔离也已通过独立复验。账号设置列表现增加底部安全区，防止退出入口落在自定义 Tab 栏下面。MDA-11/12 个人附件预览已有加载/取消处理和临时文件删除失败重试；2026-11-07 在独立 Morde Simulator 验证有效文件库证明下的 403/404 fail closed、无证明附件不可预览、合法 metadata-only 行可见但不可预览。完整逐页视觉和 Quick Look 与设计稿文件详情页的结构差异仍待审核，MDA-11/12 不勾选。Workspace 分享 API 已实现；单链接领取/并发占用的服务端测试通过，创建与领取 UI 用例在同一轮 Simulator 2/2 通过（`/tmp/MDA-workspace-share-deeplink-regression4-20261106.xcresult`）；2026-11-07 系统级链接确认/领取 1/1、manifest 边界 3/3 通过。接收方领取流程用测试夹具调用生产 URL handler，展示权限与独立副本说明，领取后新副本进入接收方列表。系统级外部唤起已通过 `simctl openurl` + SpringBoard UI 测试确认：点按“打开”后进入 Workspace 分享领取页并领取到独立副本；结果 `/tmp/MDA14-external-share-deeplink-confirm-20261107.xcresult`。Workspace 文件行菜单 1/1、非法路径拒绝 1/1、只读操作权限 1/1 均通过 Morde Simulator（结果分别为 `/tmp/MDA-workspace-row-menu-final-20261106.xcresult`、`/tmp/MDA-workspace-invalid-manifest-20261106.xcresult`、`/tmp/MDA-workspace-readonly-guard-20261106.xcresult`）；文件/目录路径冲突、重复分页游标和不安全路径在 2026-11-07 再通过 3/3（`/tmp/MDA14-manifest-edges-20261107.xcresult`）；manifest 首次 503 后通过下拉刷新恢复文件行 1/1（`/tmp/MDA14-manifest-refresh-safearea-fix-20261107.xcresult`）。分享创建/领取在 Simulator 重启后同轮通过 2/2（`/tmp/MDA-workspace-share-after-fixtures-20261106.xcresult`）。任务详情其他失败反馈/返回草稿、资源与文件库其余错误/权限状态、设置待决行及各页视觉状态仍需验收；附件因账号 scope 失效、同步关闭及 A→B 切换而取消的 UI 已于 2026-11-07 通过。Workspace 主页面已按用户决定收敛，上传和版本操作移到标题长按菜单，合成 UI 用例通过。之后继续逐页处理 MDA-01～15 的剩余状态、交互与视觉验收，最后完成 MDA-16。不能将旧 `MinisTests` 测试视为本轮验收，也不能用 build、功能用例或源码检查替代逐页视觉验收。用户已决定移除项目管理与通知收件箱的可见导航入口，保留其底层数据和服务/API；其他待审核能力见“接口与待决能力”。

**最新集成回归（2026-11-06）：** 独立 `Morde.xcodeproj` / `Morde` scheme 在 iPhone 18 Pro / iOS 27.0 Simulator 全量运行 26 项：25 项通过、1 项因未设置 IOS-03 凭据按预期跳过、0 项失败；结果 `/tmp/MDA-integration-after-retry-removal-20261106.xcresult`。该批包含活动 Task 文字队列、审批/事件链、Host、Workspace、附件、设置、任务列表及工作项流程。编译仍有 `ObservableObject` 主线程隔离警告；运行中见 `Invalid frame dimension` 与 UIKit/SwiftUI 子视图层级警告。工作项加载错误的三个独立 Retry 已移除，页面/创建失败两项 Morde 用例通过；三种加载失败本身尚未单独做 UI 状态注入验收。

**MDA-09/10 占位页视觉验收（2026-11-06）：** `testMordeDesignNavigationRoutes` 在 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（结果 `/Users/zhongxuan_li/Library/Developer/Xcode/DerivedData/Morde-hawfozzytljmicgvrjitsykselwa/Logs/Test/Test-Morde-2026.11.06_22-10-47-+0800.xcresult`），并导出好友、AI 工具会话占位页截图到 `/tmp/MDA-placeholder-screenshots-20261106/`。逐页对照本地 HTML：Morde 保留设计稿风格的页头/返回与底部原生导航；按用户决定隐藏好友申请、列表、添加按钮和 AI 会话样例/输入，仅显示占位说明，没有额外操作入口。两页当前占位状态与已审核范围一致，MDA-09、MDA-10 已完成。

**MDA-11/12 文件库与附件详情截图及状态复验（2026-11-06）：** `testMordeFullLayoutUploadsAndSendsImageAndPDFFixture`、`testMordeAttachmentDownloadFailureDoesNotOpenBlankPreview`、`testMordePersonalAttachmentPreviewCancelsWhenClosedWhileLoading` 在 iPhone 18 Pro / iOS 27.0 Simulator 同轮通过 3/3，结果 `/Users/zhongxuan_li/Library/Developer/Xcode/DerivedData/Morde-hawfozzytljmicgvrjitsykselwa/Logs/Test/Test-Morde-2026.11.06_22-34-50-+0800.xcresult`；截图导出到 `/tmp/MDA-library-preview-screenshots-20261106-final/`。按本地 HTML 核对：文件库标题已从“资源库”改为“文件库”；图片/PDF 详情改为全屏视图并保留四个底部 Tab，聊天来源返回聊天、文件库来源返回并保留筛选；无分享/保存按钮。下载失败不打开空白页；加载中返回会取消请求，延迟响应不会迟到重开预览或弹错误。测试只用进程内 URLProtocol 与合成附件。账号 A→B、同步关闭取消及缺少个人库证明的聊天附件拒绝预览已通过后续 Morde UI 用例；临时文件清理失败重试和页面视觉仍未闭环，MDA-11/12 不勾选。

**MDA-11/12 错误态与文件证明复验（2026-11-06）：** `testMordeAttachmentPreviewRejectsMismatchedContentLength` 和 `testMordeLibraryShowsUnavailableInsteadOfEmptyWhenPersonalSyncFails` 在同一 iOS 27.0 Simulator 回归通过 2/2，结果 `/Users/zhongxuan_li/Library/Developer/Xcode/DerivedData/Morde-hawfozzytljmicgvrjitsykselwa/Logs/Test/Test-Morde-2026.11.06_22-43-00-+0800.xcresult`。附件 `Content-Length` 与实际下载字节不一致时显示失败且不打开图片预览；个人同步 API 返回 503 时文件库显示“不可用”，不误显示“云端还没有文件”。均为进程内 URLProtocol 合成响应；附件账号切换、同步关闭、权限/证明缺失以及临时清理失败重试仍待验收。

**MDA-11/12 文件库筛选与详情切换主 Tab 复验（2026-11-06）：** `testMordeFullLayoutUploadsAndSendsImageAndPDFFixture` 在 iPhone 18 Pro / iOS 27.0 Simulator 以 ad-hoc 签名通过 1/1，结果 `/tmp/MDA-library-filter-nav-signed-20261106.xcresult`。用例覆盖“AI 产物”不包含个人附件、“全部”包含合成图片/PDF、“我上传的”筛选可打开图片；从文件库图片全屏详情点击“闲聊”Tab 后详情关闭、聊天输入框出现且聊天 Tab 选中。聊天和文件库图片/PDF预览及返回也通过。此前仅修正测试定位器，不改产品导航；关闭代码签名的尝试造成 URLProtocol 未生效/DNS 错误，不作为产品失败结论。MDA-11/12 仍需完成权限/证明缺失、切号/同步关闭取消、临时清理失败重试及其余预览状态验收。

**MDA-11/12 账号 scope 失效、A→B 切换与同步关闭时取消附件预览（2026-11-07）：** `testMordePersonalAttachmentPreviewCancelsWhenAccountChangesWhileLoading` 与 `testMordePersonalAttachmentPreviewCancelsWhenSyncIsDisabledWhileLoading` 在 iPhone 18 Pro Morde Simulator 分别通过 1/1。前者在 Account A 的个人附件下载中登出并登录 Account B，确认预览关闭、B 的文件库为空，延迟下载不重开页面、不在 B scope 弹出失败提示；后者在加载中关闭个人同步，确认预览立即关闭、文件库显示“不可用”，延迟响应不产生旧预览或错误弹窗。均使用本机 URLProtocol 与合成图片/PDF。首次 A→B 运行因 UI 测试沿用已登录的 B 账号、未进入 A 夹具而失败；测试现会先重置旧账号，再显式登录 A，修复后通过。最终 A→B 结果包 `/Users/zhongxuan_li/Library/Developer/Xcode/DerivedData/Morde-hawfozzytljmicgvrjitsykselwa/Logs/Test/Test-Morde-2026.11.07_05-17-59-+0800.xcresult`；此前同步关闭结果 `/Users/zhongxuan_li/Library/Developer/Xcode/DerivedData/Morde-hawfozzytljmicgvrjitsykselwa/Logs/Test/Test-Morde-2026.11.07_05-01-11-+0800.xcresult`。临时文件删除失败后的 Morde target 重试仍待验收。

**MDA-12 缺少个人附件证明时 fail closed（2026-11-06）：** 新增 `-morde.localChatAttachmentMissingProofFixture`，让本地个人同步快照不返回附件记录；`testMordeChatAttachmentWithoutLibraryProofCannotOpenPreview` 在 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1，结果 `/tmp/MDA-attachment-missing-proof-fixed-20261106.xcresult`。已接受消息仍保留 4 个附件引用，但消息中没有可点击预览按钮、没有打开图片预览且未显示成功提示。只使用合成图片/PDF和进程内 URLProtocol。该项覆盖缺少账号库关联证明；权限变化、账号切换/同步关闭期间取消和临时文件清理失败重试仍待验收。

**MDA-06 审批未知响应重试复验（2026-11-06）：** 新增 `testMordeApprovalRetriesUnknownResponseWithSameDecision`，在 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1，结果 `/tmp/MDA-approval-retry-unknown-fixed-20261106.xcresult`。本地服务夹具首次记录请求但丢失响应且不提交，界面显示“未确认”并保留同一审批按钮；再次提交被夹具强制要求复用原幂等键，最终仅确认一次并移除待办按钮。拒绝动作由 `testMordeApprovalRejectSubmitsAndRemovesPendingActions` 单独覆盖；这些均为合成账号验收，重复点击竞态、媒体权限及加载错误状态仍待验收。

**MDA-07 资源页空状态来源复验（2026-11-06）：** 扩展 `testMordeResourceWorkspaceRouteBrowsesSyntheticManifest`，在 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1，结果 `/tmp/MDA-resources-honest-empty-20261106.xcresult`。本地资源 fixture 同时提供真实 Host/Workspace 行和 `/api/devices` 客户端安装记录；页面仍显示云手机“执行状态不可用”及 AI 工具运行态空状态，没有把客户端安装或连接器 metadata 显示成云手机/活跃工具。此项确认分组与数据源边界；云手机实时状态没有真实接口，Host 其他错误/权限状态和页面视觉仍待验收。

## 剩余实施任务总览

本节是执行入口；每项的具体修改位置、状态规则和验收步骤见下方同编号 MDA 任务。已通过的功能用例只证明对应交互路径，不代表页面已完成设计稿视觉对齐。禁止仅凭 build、语法解析或旧 `Minis.xcodeproj` 测试勾选 MDA 项。

| 顺序 | 任务包 | 已有证据 | 还需完成 | 依赖/待审核边界 |
| --- | --- | --- | --- | --- |
| 1 | MDA-01～02 导航、聊天与附件 | 独立 Morde 路由、抽屉入口/分页/搜索、长按菜单、图片/PDF 合成发送通过；同一会话文字及图片/PDF 可追加到活动 Task，Simulator 覆盖首次失败后保留草稿并以相同幂等键重试；同账号跨会话文字/图片/PDF 草稿隔离、跨账号文字草稿隔离，以及切换到任务列表打开详情再返回后恢复原会话文字/图片/PDF 草稿均通过。7 条多轮聊天截图用例通过；暖色头像/状态、浅色用户灰气泡、输入框文案已对齐原型。空输入发送位置现为黑色并点按聚焦；用户图片/PDF 消息已改为设计稿文件卡片样式；queued/running 时显示真实本地化状态，停止 Task 的入口已移入长按菜单。2026-11-08 复核 `.chat`/`.msg` 排版，消息间距/边距、助手纯正文与逐条 Morde 标识已对齐原型；Simulator 截图确认首屏可见。用户已授权服务端/API 队列能力。 | 对齐真实会话/审批数；补其他 Task 状态、状态文案和完整多轮内容的逐页视觉核对。 | 头像只作为显示/状态元素，不提供资料页跳转；聊天 Task 摘要卡详情跳转仍待审核；活动 Task 图片/PDF 队列使用已批准的 Go/Runtime 合同；本地临时 PostgreSQL 定向集成五项通过，启动重试 `-count=3` 与 `-count=10` 共 26 个子用例通过，历史间歇失败未复现但根因未知。 |
| 2 | MDA-03～05 任务、详情、事件链 | 任务空态、续翻、错误态下拉刷新、跨账号迟到分页、页头几何、详情基础状态和运行中 Task 暂停时动作互斥/状态刷新、完成/失败/取消终态、审批/澄清详情控件及审批提交互斥已通过定向验收；现有详情操作已固定在 HTML `task-foot` 对应的底部区域，置于 Tab 栏上方；未知操作结果和临时 503 均复用原操作及同一幂等键恢复；按用户审核移除详情读取失败、事件流失败、未确认历史三处条件式“重试加载”。事件流保留自动重连，返回后重进详情会触发重读，失败批次业务重试不受影响。事件链已覆盖空任务、关联任务无历史、恢复/去重、账号切换/迟到投递隔离、完成/失败/等待/排队节点及七行密度；2026-11-07 空任务和无历史截图已完成对照，简体中文标题与空态文案已补齐；事件加载、网络错误展示、不增加 Retry 按钮及自动恢复已通过定向 UI 验收。MDA-03 首屏截图已与 HTML 对照：页头/按钮位置、平铺任务行与图标基本对应；TaskSummary 已按用户授权扩展真实总步数/完成数，预算步骤不计入。旧系统回弹分支在当前 iOS 27 Simulator 通过强制夹具覆盖，但实际 iOS 16.0～16.3 runtime 未安装，仍待真机/对应 runtime 验证。 | 第二页 403 后清空失效任务并下拉恢复已通过；继续核对详情其他异常/返回草稿状态，并完成逐屏视觉与操作位置对照。 | 设计稿“急停这个任务”无独立 API；不得以暂停、取消或全局急停替代。 |
| 3 | MDA-06 审批 | 待办未知时隐藏计数、批准/拒绝、未知响应幂等重试、指定任务事件链跳转、列表失败后下拉刷新恢复、版本冲突及重复提交已通过 Morde 用例。已取得单审批截图并与 HTML 首屏结构对照；审批标题/正文/动作字号分别符合 `.appr` CSS 的 17.5/14.5/14.5px；2026-11-08 将待办数和事实文字颜色对齐至 `#A3A3A8` / `#737378`，并将“看事件链”链接字号对齐为 14pt、保留 32pt 命中高度；审批 UI 回归 1/1 通过。用户确认：决策提交后保留事件链入口，直到 Task 详情确认审批不再待处理时隐藏；实现及确认/待确认两种状态的 Morde Simulator 用例已通过。 | 补其他审批状态视觉；审批媒体直达 Task 预览入口等待用户审核。 | HTML 有灰度/改天数第三动作、自动处理历史及变更摘要信息；当前 API 没有对应动作/历史/变更合同，等待审核，不能伪造。 |
| 4 | MDA-07～08 资源与工作项 | 资源 Workspace 路由/浏览、资源“＋”诚实提示（未接入提示使用已有本地化字符串）、资源确认空态/Host 权限和生命周期状态、错误态下拉恢复及控制状态错误单次显示已验收；资源“＋”提示位置/字号/阴影/时长及“添加资源”无障碍标签本轮按 HTML 修正。工作项表单通过合成 API 创建/失败保留输入，筛选结果及只读/可编辑项目创建权限已验收；新建标题按 HTML `.sheet h3` 对齐为 17.5pt、`-0.175pt` 字距。用户已审核删除工作项页 3 个独立 Retry，保留下拉刷新。2026-11-07 源码核对确认三个错误卡片只保留错误说明，`.refreshable` 触发 `store.load()`；设计稿中文创建/标题提示/认领文案已有正确本地化；工作项列表和创建弹层的合成截图已保存至 `/tmp/MDA-08-work-items-visual-evidence-attachments-20261107/`。 | 完成资源/工作项视觉核对；工作项额外字段、选择器和行操作等待用户审核，实际 Portal 创建仍未连接验收。 | 资源“＋”无真实添加流；MDA-07 Host 行额外控制状态待审核；MDA-08 真实账号无项目，设计稿外字段/操作待审核。 |
| 5 | MDA-09～10 好友与 AI 工具会话占位 | 路由、标题、占位说明与返回通过；Morde Simulator 两页截图已与本地 HTML 原型对照。 | 无；按已审核范围保留占位，不接真实数据。 | 好友申请/列表/添加及 AI 会话样例/输入均按用户决定隐藏；不造数据或成功反馈。 |
| 6 | MDA-11～12 文件库与附件详情 | 三段筛选、文件库标题、图片/PDF 全屏预览、聊天/文件库返回及筛选保留已有 Morde 用例；HTML 与 Simulator 截图已对照；“全部”筛选在有内容的来源时不再显示其他空来源的重复空行，所有来源均可用且为空时只显示一个统一空态；独立类别筛选仍显示自身空态，权限拒绝/不可用状态不变。附件预览标题与返回按钮间距由 14pt 调为设计稿的 10pt，个人附件页标题再对齐为 -0.17pt 字距；本轮图片/PDF 聊天预览、文件库筛选与返回保留用例通过 1/1（`/tmp/MDA12-letterspacing-regression-20261108.xcresult`），Task Quick Look 与无 Library proof 不可预览边界通过 2/2（`/tmp/MDA12-preview-boundaries-20261108.xcresult`）。新图片预览截图 `/tmp/MDA12-letterspacing-attachments-20261108/527340AC-C42C-4B9C-8318-D3307D258886.png` 已检查。下载失败和关闭加载态取消 3/3 通过；账号 scope 失效/同步关闭期间取消 2/2 通过；Task 构件缺少 Workspace proof 时只显示 metadata、没有预览按钮，完整 proof 构件仍可打开 Quick Look；个人附件缺少账号库证明时不可预览，合法 metadata-only 行在“全部”可见、在“我上传的”隐藏且无预览入口；有效文件库证明下服务端返回 403/404 时均失败关闭，以上 Morde UI 用例通过。 | 补其他错误/权限状态及完整逐页视觉核对。Task 构件 Quick Look 已使用设计稿文件详情的返回栏/标题/正文卡/四项底部 Tab 外框并保留原生只读正文；Morde Simulator 截图验收通过。其余权限状态与整页视觉仍待完成。 | MDA-12 依赖 MDA-01 的返回栈；只使用合成附件，不传真实文件。 |
| 7 | MDA-13 设置 | 根页 10 行顺序/分组/返回/底部导航已与 HTML 截图对照；账号详情截图显示真实身份、服务地址、Host members、Model connections、API Keys、安装管理和退出入口；设备详情在本地 Hub fixture 下打开远端授权弹窗后取消，未发送请求。五个未实现详情及关于页可达；设置文案目录补译 51 个 key 并已通过 AppLocalized 接入应用内语言选择；独立 Morde 设置导航测试通过 1/1。 | 补设备稳定态、凭证详情、完整详情视觉、设备刷新/错误/撤销等状态；逐项核对账号页额外入口实际可达性；落实仍待审核的设置入口处理决定。 | 连接器、权限、审批规则、通知偏好、外观、Hub relay、Retry、Portal 入口、旧本机功能的去留待审核；急停恢复和真实摘要差异待审核；不伪造能力。 |
| 8 | MDA-14～15 Workspace 与云电脑 | Workspace 目录浏览、根页/子目录设计样例、直系上级面包屑逐级返回和空目录提示有 Morde Simulator 合成验收；标题长按菜单上传/版本入口、自有与共享 Host 的相应票据路由已有合成验收；本机副本删除已限制为普通文件；分享创建与接收领取 UI、服务端单领测试通过；分享预览/领取阶段 404 与网络错误 UI 本机验收 4/4；2026-11-08 reservation 已改为创建时归接收者，按权限生成只读/可编辑副本、向接收者计配额，并覆盖安全失败重试与未知状态恢复；Morde Simulator 创建/深链领取 2/2 通过。云电脑移动端主栏已收敛为设计稿的浏览器/终端/文件，浏览器地球、终端窗口、文件夹三个 SVG 图标也已在合成 WebView fixture 对齐；有绑定 Workspace 的自有 Host 点击“文件”进入该 Workspace；“主机文件”、Workspace、VS Code 与窗口工具从常驻栏移入长按/上下文菜单。WKWebView 在 documentEnd 注入 Morde iOS 壳样式，旧 Host CSS 也会隐藏续租、倒计时、版本、重复状态与长说明。停止任务已复用当前会话中 Host 匹配 Task 的既有取消动作；共享成员桌面控制/Portal Session、页面前台静默续租已在 Morde Simulator 通过 1/1；绑定 Workspace 文件入口已通过 Simulator 1/1；真实 Gateway 内容仍未验收。 | 补路径校验、文件行菜单、错误/权限/返回；验证系统级 URL 分发；核对 Host 加载失败、关闭清理、Gateway/WebView 可用交互和逐页视觉，并记录 HTML 与实际 manifest 在文件排序/元数据和目录说明上的差异。 | 用户已授权 Workspace 分享 API 与云电脑共享控制权；Task Runtime 的 AI 桌面工具不等于 Morde iOS 用户的实时远控验收，参见 MDA-15。 |
| 9 | MDA-16 跨页回归 | 等 MDA-01～15 逐页完成后开始。 | 串行跑主流程，并覆盖主题、窄屏、键盘、安全区、Dynamic Type、VoiceOver、减少动态效果。 | 真实账号路径缺凭据时单独标记未验收，不用合成结果替代。 |

**逐页完成门槛：** 每页对照 HTML 对应 `data-view` 记录可见按钮/输入/筛选数量与顺序、相对位置、nanoMuse 视觉、真实状态、点击目标和返回路径；保留 Simulator 截图及差异结论。内置浏览器拒绝直接打开本地 `file://`，当前改由只绑定 `127.0.0.1` 的临时静态 HTTP 服务预览原文件；没有 HTTPS 发布、代理或外网可访问地址，也没有上传/发送 HTML。该回环预览用于查看原型页面与走查原型交互，HTML 的静态数据和本地模拟结果不能代表 Morde 真功能。各页仍须与独立 Morde Simulator 截图逐项比对并记录，完成之前不能勾选页面任务。

## 执行前置条件与当前快照

- iOS 目录中同时存在 `Morde.xcodeproj`（本轮唯一验收 target，`Morde` scheme，bundle ID `io.morde.app`）和旧 `Minis.xcodeproj`（`morde` scheme，bundle ID `io.github.nanomuse.app`）。两者共享部分源码，但 source/test 集合不同。独立 Morde scheme build 成功；设计稿导航已接入 Morde 活动根路由。历史 MDA/MIF 页面测试来自旧 `Minis.xcodeproj`，不作为新 App 页面验收证据。
- 父仓库 `.gitignore` 忽略整个 `project/` 和 `docs/`，因此嵌套工程与本 TODO 的改动不会出现在父仓库普通 `git status/diff` 中。当前目录含大量未提交和未跟踪文件，后续只改本 TODO 指定的源码/测试，不 reset、stash、清理、覆盖或从 `/Users/zhongxuan_li/Desktop/project/nanoMuse` 旧副本同步文件。
- 既有验证证据以各 MDA 页面执行记录和 MIF 子清单为准。下表只概括最近一次进度；`进行中/待验证` 项不能因历史记录或源码曾经改动就标为完成。

| 子项状态 | 已有结果 |
| --- | --- |
| 已完成的基线 | MDA-00 页面清点、独立 target build/launch 基线与工程 lint。旧 `Minis.xcodeproj` 的抽屉、附件流程和任务分页用例及 MIF 单测只作功能参考，不作为新 Morde 页面验收。 |
| 已接线/已改源码、仍有页面状态或视觉验收未完成 | MDA-01 导航/根路由；MDA-02 聊天与附件；MDA-03 任务列表标识与手势；MDA-05 事件链；MDA-06 待办未知态及审批到指定事件链的路由；MDA-07 资源分组/空态与平铺行；MDA-08 工作项弹层/筛选/平铺行；MDA-11 文件库筛选；MDA-12 附件预览；MDA-13 设置页（根页截图对照已通过；账号/设备仅部分详情有证据，凭证详情和完整详情视觉仍未完成）；MDA-14 Workspace 浏览；MDA-15 现有 Host WebView 路由。源码已包含在成功 build 中，但各项剩余状态仍需逐项完成。 |
| 本轮阶段完成（局部视觉微调已验收，页面未关闭） | MDA-07 Host/Workspace 深色图标对比、MDA-08 工作项输入/多行标题排版、MDA-13 设置行箭头按 HTML 校准；5 项浅色 Morde UI 回归和 1 项深色资源页用例均通过，细节见本清单最新执行记录。没有新增按钮、改路由或触碰待审核数据/操作。 |
| 前序验证结论 | 独立 `Morde.xcodeproj` / `Morde` scheme / iPhone 18 Pro / iOS 27.0 Simulator 定向用例共 5/5 通过（`/tmp/MDA-three-pages-light-20261108.xcresult`），MDA-07 深色模式另 1/1 通过（`/tmp/MDA-resource-dark-preboot-20261108.xcresult`）；截图分别位于 `/tmp/MDA-three-pages-light-attachments-20261108/` 与 `/tmp/MDA-resource-dark-preboot-attachments-20261108/`。此前最新全量 UI 套件 `/tmp/MDA-integration-after-retry-removal-20261106.xcresult`：26 项中 25 项通过、IOS-03 凭据用例 1 项跳过、0 项失败。编译仍有现存 `ObservableObject` 主线程隔离警告；UI 运行记录无效 frame 与 UIKit/SwiftUI 子视图层级警告。内置浏览器使用仅绑定 `127.0.0.1` 的本地 HTML 原型。Simulator 截图对照已完成 MDA-09/10 占位页、MDA-11/12 图片/PDF 详情页和 MDA-13 设置根页；MDA-13 账号及设备详情有局部截图，但未完成详情页并排视觉核对，安全凭证/API Keys 详情未打开验收。旧 `Minis.xcodeproj` 结果不作为新 Morde 页面验收证据。 |
| 2026-11-08 后续定向回归 | MDA-02 聊天样例/空输入、MDA-07 共享 Host 控制成员进入、MDA-15 云电脑票据返回资源共 4/4 通过（`/tmp/MDA02-MDA15-design-permission-final-20261108.xcresult`）；MDA-03 任务页“＋”创建会话、返回聊天并聚焦键盘另 1/1 通过（`/tmp/MDA03-task-plus-focused-composer-20261108.xcresult`）。MDA-04 三种 Retry 移除及重进/自动恢复再次通过 3/3（`/tmp/MDA04-retry-confirm-current-20261108.xcresult`）。这些是本机合成 fixture 的局部交互证据；相关整页状态/视觉门槛未完成，MDA-02/03/04/07/15 均保持未勾选。 |
| 待继续 | 对 MDA-02～08、MDA-11～15 完成逐页位置、尺寸、按钮数量及返回/错误状态核对；MDA-13 已接通设置动态文案的应用内本地化；仍需完成设备稳定态及刷新/错误/撤销状态、凭证详情和完整详情视觉对照，并等待设置外额外交互审核；确认其余已审核的设计稿外能力处理方式，最后进行 MDA-16 跨页面回归。MDA-15 已覆盖 Host 票据、停止匹配 Task 和本机截图预览；预览黑屏已修复并通过截图检查。文件入口到绑定 Workspace、README 显示与返回资源 Simulator 用例 1/1 通过；Host 实时桌面、键盘/触控、终端、截图写入 Workspace 的实现授权及完整真实 Host 视觉仍待处理。 |

## 已确认决定

| 范围 | 按已审核决定执行 |
| --- | --- |
| 好友、AI 工具会话记录 | 保留入口和“暂未实现”占位，不伪造好友、会话、运行结果或成功提示。AI 工具资源运行状态是另一项，按 MDA-07 核对。 |
| 预算和旧导航 | 不在应用生成的任务、聊天摘要、审批、事件链及工作项界面显示预算/费用；动态、点子、目标、例程的旧侧边栏/聊天菜单入口移除。用户打开的原始文件内容不改写。 |
| 聊天输入 | 附件、语音、Task options 和选择停止 Task 的入口保留在长按/菜单；首期附件支持图片和 PDF。聊天输入区不增加常驻附件、麦克风或更多按钮。停止生成/加入队列按已确认方式复用发送按钮位置。 |
| 活动 Task 消息队列 | 用户本轮再次明确授权扩展服务端/API 与 Go worker、JS runtime 投递合同，将图片/PDF 随运行中 Task 排队。现有 iOS、Go、Runtime 合同及响应丢失后的同幂等键重试实现覆盖该范围，并已通过本地合成/定向测试和本机临时 Postgres 定向集成；启动重试用例的 `-count=3` / `-count=10` 复跑通过，此前间歇性 `task_runtime_unavailable` 根因仍未知。未验证真实部署数据库或线上服务。 |
| 任务详情 | 保留暂停、继续、取消，以及失败批次对应的业务“重试”等现有真实操作；按用户已审核决定移除详情读取失败、事件流失败、未确认事件历史三处页面级“重试加载”按钮。失败后返回上一页再进入详情触发重新读取；事件流仍自动重连，不影响业务重试。不把不同操作简单等同于“急停这个任务”。 |
| 审批提交后的事件链入口 | 用户确认：批准/驳回提交后，保留“看事件链”；直到 Task 详情确认该审批不再待处理时才隐藏。详情状态仍待确认或刷新失败时继续显示入口。 |
| 工作项错误态 | 已审核：删除项目列表、共享会话列表、工作项列表失败态中的 3 个独立 Retry 按钮；保留错误说明和页面下拉刷新，以一次刷新重新加载页面数据。 |
| Workspace 文件 | 已有文件操作保留，适合文件行的操作放入文件行菜单；无法放入文件行且需要额外可见入口的能力按下方审核规则单独处理。 |
| 资源与列表 | 任务页使用设计稿的“＋”，刷新改下拉；资源页按设计稿保留云主机、云手机、Workspace、AI 工具分组，移除额外文件库/刷新按钮及默认目标卡片的展示，文件库从抽屉进入；主机和 Workspace 以整行点击进入。云手机/AI 工具只显示真实数据或真实空态，不扩服务端合同、不造运行状态。主机交还控制按钮作为已审核例外保留；续租、刷新控制状态的额外可见按钮按 UI-13 决定移除，真实控制能力和状态校验保留。 |
| 已审核的旧入口 | 抽屉不显示“全部会话”归档及“系统文件”；旁聊错误重试改为重新打开抽屉自动重试，分页改为滚动加载。此轮对齐不需要清除历史数据。 |
| 项目与通知入口 | 移除旧项目管理和通知收件箱的侧边栏/聊天菜单入口；保留已有项目、通知数据、推送和 API，不新加偏离设计稿的入口。工作项仍按真实项目 ID 调用现有工作项合同。 |
| 云电脑与主机分享 | 移动端常驻浏览器/终端/文件；Workspace、主机文件、VS Code 和窗口工具移到长按/菜单。云电脑页不显示续租、租期倒计时、版本与冗长说明，真实控制租约及自动续期保留。云端电脑分享从主机行长按菜单进入，复用长期有效的 `desktop:control` 成员权限，直至所有者撤销；单次控制租约仍按现有期限续期。 |

原需求中“附件需先询问”和“额外任务操作需先询问”已由上述用户决定覆盖，不再重复询问。其他设计稿之外的可见按钮、菜单项或功能，如果确实需要保留或新增，先记录名称、所在页面与位置、现有作用、移除后的影响和建议，交用户审核；若已在原审核表中明确批准则直接执行。设计稿已画出的按钮按设计稿实施，缺少真实接口的动作按下文处理。对已批准移除的可见入口直接实施；发现未审核过的功能会因此失去入口时，先列出影响，等待用户决定该项，其余工作继续。

## 本轮已核实的起点

| 范围 | 源码与清单现状 | 本轮需要完成 |
| --- | --- | --- |
| 抽屉导航 | `MordeDesignNavigation.swift` 与 MordeRoot 已接入四个设计稿底部入口、抽屉、真实旁聊列表/审批数和滚动分页。Morde 菜单按钮已补稳定无障碍标识；Morde Simulator 用例覆盖主 Tab、抽屉事件链、好友/AI 工具占位、搜索、分页、返回和设置入口。 | MDA-01 继续核对真实会话/徽标状态和逐屏视觉位置/尺寸；合成夹具分页已通过。项目/通知入口隐藏，底层数据/API 保留。 |
| 任务/审批 | 任务页已有“＋”；02 子清单的 MIF-03～05 仍未勾选。新 Morde 定向用例已验证任务详情的目标文本、真实步骤、暂停操作及预算字段不显示；审批页已验证真实待办数、批准/驳回入口及无部分状态误标。 | 任务列表、事件链、任务详情其他失败反馈/返回草稿及审批异常/失败状态继续逐项验收；不因单一页面用例通过就声称整组完成。 |
| 资源页 | 资源视图已改为 Host/Workspace 平铺行、设计稿分组和真实空态；MordeRoot 已接入资源 route。资源“＋”沿用原按钮，在无真实流程时短暂提示“添加资源暂未接入”，不增加需点按关闭的 Alert 按钮。 | 完成页面验收；只显示真实数据或真实空态，不扩接口；资源“＋”保持诚实的未接入提示。 |
| 工作项 | 工作项视图已改为四段筛选、平铺行和新建 sheet，并已接入 Morde route。Morde target 合成用例已验证四种筛选结果、viewer/owner 创建权限和创建提交成功/失败。 | 实际 Portal 创建、行操作、项目切换和页面视觉仍待验收；设计稿外能力按待审核范围处理。 |
| 文件库 | `MIFLibraryViews.swift` 已改为“全部 / AI 产物 / 我上传的”三段筛选；个人云端文件按有效 `attachment_id` 判断是否属于上传，metadata 行不开放伪预览。 | 完成测试和逐页核对；个人附件详情由 MDA-12 接入，修正仍期待旧 Cloud 筛选/按钮的历史验收用例。 |
| 审批与设置 | 审批页已移除额外刷新按钮并改平铺，保留真实批准/驳回及首条事件链入口；已补入真实待办数标题附件和异常状态处理，当前 API 的决策枚举仅为 `approve`、`reject`。设置页仍有设计稿以外的旧功能行。 | 在 Morde scheme 下验收审批状态和视觉；第三种审批动作、历史记录及高影响设置入口仍待审核。 |
| 聊天附件 | 图片/PDF 本机处理、消息 `attachment_ids` 和个人 Task 读取工具已有实现；解析、上传发送、丢失响应幂等重试、取消、失败反馈、16 项上限及 A→B 附件隔离均有通过记录；同账号跨会话文字/图片/PDF 草稿隔离已通过。 | 跨账号文字草稿切换及 PDF 展开后超过 16 项的 Morde Simulator UI 边界已通过；受保护预览缺证明、同步关闭取消及长度校验已有通过用例；继续验收未登录/权限变化及实际页面对照。PDF 文本最多 100 页/48 KiB，页图前 10 页；一条消息最多 16 个附件 ID，Task 工具读取最近 1,000 条消息中的最多 64 个附件。 |

逐页视觉对照、按钮清点和完整页面流程尚未完成，不能仅依据源码映射或既有功能测试声称所有页面对齐。基线记录见 [01 组 MIF-12 和附件记录](<Morde iOS与nanoMuse融合设计并行实施/01-共享接口与外壳实施任务todo清单文档.md>)、[02 组任务/审批记录](<Morde iOS与nanoMuse融合设计并行实施/02-任务与审批实施任务todo清单文档.md>)、[03 组工作项记录](<Morde iOS与nanoMuse融合设计并行实施/03-工作项实施任务todo清单文档.md>)和[04 组资源/文件记录](<Morde iOS与nanoMuse融合设计并行实施/04-资源文件与占位实施任务todo清单文档.md>)。

## 设计稿页面清点基线

下面按 HTML 的 14 个 `data-view` 清点可见控件、现状和审核边界。MordeRoot 已接入设计稿导航与页面 route；最新 Morde UI 套件已验证导航、占位页、聊天附件、急停及合成 Workspace 浏览等路径。其余页面可达性、逐屏控件、位置/尺寸和状态仍需在每项 MDA 页面任务里核验；这里作为逐页实施的控件基线，不把 HTML 的演示数据当真实数据。

| 页面（HTML `data-view`） | 设计稿可见控件与顺序 | iOS 当前实现 / 已知差异 | 处理依据 |
| --- | --- | --- | --- |
| 聊天 `chat` | 菜单；头像/状态；消息流与文件行；单行输入；发送/停止单位置 | nanoMuse 聊天头、Morde Task 聊天和输入区已接入；附件/语音/Task options 走长按菜单；图片/PDF 选择→上传→发送和账号隔离有本机替身通过记录，同账号跨会话及跨账号文字草稿隔离已通过。旧截图 `/tmp/MDA-02-cross-account-draft-attachments-20261107/8E491F27-0A28-4D68-ABCF-DD2B124847AB.png` 曾显示输入框左侧常驻“＋”；已按用户审核决定移入聊天长按菜单的 Task options 并移除常驻按钮，目标选择、文字队列、图片/PDF 队列和同键重试回归均通过。聊天输入区上方滚动留白按 HTML 16pt 列表底距 + 6pt 输入区顶距调整为 22pt；空闲输入胶囊按原型收敛到约 46pt 高，保留 44pt 点击目标和 nanoMuse 灰色胶囊风格。更新本地登录测试 helper 后，多轮消息视觉和空输入聚焦弹键盘在当前尺寸下各通过 1/1，截图已检查。原型 390×812、Simulator 402×874，且按已审核决定隐藏预算后少一行内容，短消息首屏剩余空白属于较大视口/较短内容下的自然留白。Task 卡片和其余未决控件、长短内容及附件状态全页核验待完成 | UI-07；输入栏不加未经审核的常驻按钮；图片/PDF API 已授权 |
| 任务列表 `tasks` | 菜单、标题、右侧“＋”、任务整行；页头水平边距 22pt、圆形按钮 38pt、标题与菜单间距 10pt | MIF 任务页已移除行尾箭头、“＋”跳新会话并聚焦输入、列表滚动触底分页；独立 Morde target 已验证页头控件坐标、空态、空中间页自动续翻、下拉恢复和跨账号迟到分页隔离。逐页视觉截图仍待验收。 | UI-08；刷新改下拉，工作项从抽屉进入，不恢复未经审核的额外按钮 |
| 任务详情 `task` | 返回、任务名、目标摘要、步骤时间线、底部单任务急停 | MIF 详情视图保留现有暂停/继续/取消/失败批次重试等真实操作；操作区已固定在 HTML `task-foot` 对应底部、Tab 栏上方。按用户审核移除了详情读取失败、事件流失败和未确认历史的三个“重试加载”入口；说明文案及事件自动重连保留。Morde 定向 UI 已验证标题、真实步骤、操作结果未知后复用同一动作/幂等键恢复、完成/失败/取消、审批和补充信息控件，且预算步骤不显示；其他异常状态及整页视觉核对仍待完成 | UI-09 已明确保留现有操作；不把暂停、取消或全局急停改称单任务急停 |
| 审批 `appr` | 菜单、标题、待审批卡；驳回/灰度或改天数/批准；自动处理历史 | 2026-11-07 单项 Morde 截图已与原型首屏对照。Morde 显示当前合成账号的 1 项待审批、拒绝/批准和事件链链接；HTML 有 2 项样例、灰度/改天数、已自动处理历史及变更摘要。Morde 真实 API 只支持 approve/reject，且无自动处理历史/摘要合同，未复制假动作/历史/统计；异常、权限和完整页面视觉仍待核验。 | UI-10；第三决策及历史无合同，不伪造，按待审核项处理 |
| 事件链 `chain` | 菜单、标题、当前任务、按序事件 | MordeRoot 已接入独立 `eventChain` 抽屉 route，MIF 事件视图已编入 Morde target；登录后可达性、空态、页面标题与刷新路径待 Simulator 核验 | UI-11；无当前任务展示真实空态，不增加任务选择入口，不显示预算 |
| 资源 `res` | 菜单、标题、“＋”；云主机、云手机、Workspace、AI 工具区 | MordeRoot 已接入资源 route 并通过 target build；资源视图已移除额外文件库/刷新按钮及默认目标卡，Host/Workspace 平铺整行，云手机/AI 工具使用真实空态。页面尚未登录后验收；“＋”无真实添加流程。 | UI-12～14；用户已确认按设计稿保留分组；只有真实数据或真实空态，不使用 HTML 样例、不扩服务端 |
| 工作项 `work` | 菜单、标题、“＋”；全部/缺陷/需求/待认领四段筛选；创建弹层仅类型、标题、创建 | 定向 Morde UI 已验证合成项目选择、四段筛选、页头“＋”及创建表单的类型/标题字段；源码核对确认三个错误态无独立 Retry 且下拉刷新调用全量 `store.load()`。中文创建、标题提示、认领文案已有正确本地化。当前仍多出项目/Task 上下文区、描述框及关联 Task/记录 Workspace 版本/阻塞/解除阻塞行操作；详细作用和建议见待决表，待用户审核后收敛。 | UI-15；对齐筛选及新建弹层；真实项目作用域/认领不可虚构 |
| 好友 `friends` | 菜单、标题、“＋”、好友申请/列表及通过/忽略 | Morde drawer route 与“暂未实现”占位视图已接入并通过 target build；打开/返回和页面无多余控件尚未完成 Simulator 验收 | UI-02；只保留占位，不接好友数据或操作，不显示演示好友/申请控件 |
| 文件库 `lib` | 菜单、标题；全部/AI 产物/我上传的三段筛选；文件行 | MordeRoot 已接入抽屉 route；三段筛选与有效附件 ID 分类已实现；本机文件和 Task artifact 保留 proof/权限校验。合成 Task 构件已从 AI 产物行打开 Quick Look 并完成稳定态 Simulator 截图，设计稿的同类文件行只显示“打开…”提示，行为差异待用户审核。 | UI-16；只认真实 attachment ID，保留 proof/权限门控；差异见待决表 |
| 文件详情 `file` | 返回、文件名、正文/预览 | 聊天附件图片/PDF 的全屏预览、来源返回及 Tab 导航有合成 Morde 证据；Task 构件 Quick Look 也通过本地合成文件验收，但系统 sheet 没有设计稿文件详情的返回栏和四项底部 Tab，仅能下滑关闭，且没有可见关闭/分享/保存操作。设计稿文件库行点击只显示提示，Task 构件的最终点击行为等待用户审核。 | UI-16/17；个人图片/PDF 保留安全门控；不增加设计稿以外的可见按钮 |
| AI 工具会话 `toolchat` | 返回聊天、工具名/标题、只读消息区、只读说明 | Morde drawer route 与“暂未实现”占位视图已接入并通过 target build；打开/返回和无多余输入/工具按钮尚未完成 Simulator 验收 | UI-02；不增加输入、发送或工具操作按钮 |
| 设置 `settings` | 返回、标题；账号与连接、权限与审批、通用三组，共 10 行 | 2026-11-07 在独立 Morde Simulator 通过 `testMordeDesignNavigationRoutes` 1/1 并导出设置页截图：10 行顺序、三组分区、唯一急停控制行及四个底部 Tab 与 HTML 一致。差异：账号值显示真实用户名而非 HTML 示例套餐/用量；审批规则行未显示“高风险才找你”；账号/设备详情的部分路由和授权弹窗已核验，完整详情视觉、设备稳定态与凭证页仍待核验。OpenAPI 的 `/api/me` 返回 id、name、role、status、must_change_password、quota、usage；客户端 `MordeMe` 当前只解码 id/name，页面只显示真实用户名。按用户已删除预算显示的决定，不新增账号 quota/usage 展示，也不照搬 HTML 的套餐/使用率样例；审批规则页显示“暂未实现”。 | UI-19；保留真实账号数据，不伪造套餐、用量或审批策略；全局急停服务端状态差异见待决表 |
| Workspace `ws` | 返回、Workspace 名称、分享；描述、路径、文件/目录行；查看/编辑分享弹层 | 主页面已移除常驻选择器/版本号、能力说明、上传及版本区域；上传和版本比较/恢复放入标题长按菜单，传输中/失败才显示对应状态；账户自己的 Workspace 由资源页行传入目标。未找到查看/编辑分享链接 API | UI-17；文件级操作收入行菜单；标题长按处理已确认，分享合同仍待核实 |
| 云电脑 `pc` | 顶部截图/关闭；浏览器、终端、Workspace 文件；停止任务/接管；接管后的键盘、触控、截图和文件入口 | Morde 票据 WebView 加载 Host Gateway Workbench。已按用户审核在移动端收起续租、Host/租约倒计时、控制版本和长说明；常驻导航只显示浏览器/终端/文件，Workspace/VS Code 及窗口操作分别移入长按/菜单。原生页头显示控制状态与 Host/Workspace，提供截图本机预览及关闭；底栏复用当前会话中匹配 Host 的 Task 停止入口和接管/交还。真实 Host WebView 页面、桌面截图及终端/文件交互尚未通过授权 Host 验收；用户提供的界面截图仍显示旧 Workbench 资源，不能以本地源码/合成 Host 证明线上资源已更新。 | UI-18；入口与无倒计时规则已审核并实施；只在本地修改，不部署/外发；真实 Host 视觉和交互继续待验收 |

**同时清点的弹层/交互：**工作项新建 `wiSheet`（UI-15）；Workspace 分享 `shareSheet`（UI-17）；好友示例详情和审批样例历史不接真实功能；键盘、长按菜单、系统文件选择器与空/加载/错误态在对应页面 MDA 任务中继续核验。额外功能的来源、删除影响与审核状态以原总览 UI-01～20 表为准，不覆盖用户已审核决定。

## 接口与待决能力

本轮布局和已有能力接线优先复用当前合同；用户已授权聊天图片/PDF API 和活动 Task 消息队列相关服务端/API 工作，不能把这两项授权扩大为下表其他服务端或 Host 扩展。

| 接口/能力 | 当前合同及实施约束 | 归属 |
| --- | --- | --- |
| 聊天消息和个人附件 | `POST /api/conversations/{id}/messages` 已包含 `attachment_ids`，每条最多 16 个；复用个人附件上传合同及 Task 读取工具，保持个人账号、消息关联、幂等和项目 Task 禁用边界。 | MDA-02；已通过上传/发送、幂等重试、失败/取消、附件账号隔离、同账号跨会话文字/附件草稿隔离、跨账号文字草稿清理、PDF 展开后超限拒绝及 fail-closed 预览边界；补页面核对。 |
| 任务列表步骤计数 | 用户已授权扩展 `TaskSummary`。服务端通过单次批量聚合为 `GET /api/tasks` 与会话活动任务摘要提供 `step_count`、`completed_step_count`；排除 `kind == budget`，完成数只计 `status == succeeded`。完整 Task 响应按同一规则计算；iOS DTO 兼容旧响应缺字段的情形，优先显示服务端计数，不请求逐行详情、不填演示数据。 | MDA-03；OpenAPI、Go 聚合、iOS 展示及本地 UI fixture 已实现。iOS Simulator 展示用例、Workbench 类型检查/构建通过；MDA-03 计数 API 与领域单测通过；Postgres 集成用例因未配置 CH_TEST_DATABASE_URL 而跳过。全量 API suite 另有既存 OpenAPI 合同断言失败，计数定向用例通过。 |
| 活动 Task 消息队列 | 已扩展 Go pending message 和 v2 私有 worker frame；JS Runtime 同时兼容 v1 文字和 v2 附件消息，按正文 + 附件 ID 去重。附件 ID 限每条最多 16 个，必须唯一并按字典序排列，且为 `pa_` + 32 位小写十六进制。数据库只交付相同 owner/Task/service instance 下已提交上传的附件；checkpoint ID 不匹配时标记 `unknown` 并 fail closed。为运行中的个人 Server Task 注入通用 ID 读取参数，broker 每次按当前 owner/Task/会话重新校验附件关联；读取支持 JPEG、PDF 提取文本和 PDF 页 JPEG。未新增公开 API 路由。2026-11-07 本地临时 PostgreSQL 定向集成五项复跑通过；启动重试用例 `-count=3` 与 `-count=10` 的 26 个子用例均通过，历史 `task_runtime_unavailable` 本轮未复现，根因仍未知。 | MDA-02；Go app、worker、Runtime 合成测试通过；选定 Postgres 集成复跑通过，历史间歇失败根因及其余数据库覆盖仍待跟进。 |
| Morde 个人数据本地存储 | `NanoMuseMordePersonalContentStore`、个人 mutation/profile outbox 和附件 staging 的存储方法改为显式接收 `root`；旧 NanoMuse caller 显式传 `NanoMuseDirs.root`，Morde caller 传自己的 Application Support 根目录。保留现有校验、错误和原子写入语义；不读取旧 App Group、不迁移数据。 | MDA-00；需更新 Feed、Rooms、Library、Profile、Goals 与 MordeRoot 调用点。 |
| Task、事件及审批 | 复用 `GET /api/tasks`、`GET /api/tasks/{id}`、`GET /api/tasks/{id}/events`、`POST /api/tasks/{id}/actions`、`POST /api/tasks/{id}/approvals/{approval_id}/responses`；保留权限、版本、幂等、事件游标和 202 后刷新语义。 | MDA-03～06；现有操作无接口变更。 |
| 项目工作项 | 复用 `/api/projects/{id}/work-items` 及 `/{work_item_id}/claim` 等现有合同；项目 ID 和账号权限不可猜测，认领使用版本及幂等约束。现有状态为 `open`、`blocked`、`delivered`。 | MDA-08；页面布局不自行扩大合同。 |
| 资源、Workspace 和 Host | 复用 `/api/hosts`、`/api/workspaces`、Workspace manifest 与既有 Host 进入/控制接口；保持短期票据、HTTPS 同源、权限和租约规则。 | MDA-07、MDA-14、MDA-15。 |

下面的能力先检查当前源码、OpenAPI 和实际可用权限，记录证据及对页面的影响；能用既有合同实现的直接接入。若确需新增服务端/Host 合同或选择不同业务语义，向用户给出具体选项并等待该项决定，不把未实现动作伪装为成功，也不阻塞其他可独立完成的页面。

| 待核实/待决项 | 具体问题与可供审核的选择 | 关联任务 |
| --- | --- | --- |
| 抽屉长按排序 | 设计稿和当前抽屉均显示“长按排序”提示；HTML 原型和 iOS 均没有重排手势逻辑。本轮按一致的视觉提示保留，不新加排序设置或按钮。 | MDA-01 |
| 聊天输入框常驻 Task 选择器 | 输入框左侧“＋”打开“新任务 / 现有 Task”菜单，含活动 Task 队列目标；HTML 原型的常驻输入栏没有该按钮。用户已选择“移到长按/菜单入口”，本轮按此决定移除输入框左侧常驻“＋”，把同一 Task options 功能迁入聊天现有长按/菜单入口；默认保持新建，只有用户选中具体活动 Task 后才追加队列。保留现有权限、活动 Task 校验、幂等键和草稿行为，不增加菜单项以外的常驻按钮。服务端/附件队列授权不扩大到其他业务合同。 | MDA-02 |
| 聊天页 Task 状态卡 | 真实 Task 关联后会显示状态/步骤卡，卡内蓝色“Open task details and activity”跳转到 Task 详情；HTML `chat` 没有这张卡或此入口。建议保留真实状态摘要、删除卡内跳转，任务详情仍可从任务页进入。移除整张卡会失去聊天中的任务状态摘要，但不会删除 Task 或其数据。当前聊天页头像只是语音状态指示，没有资料页跳转，因此不列为额外交互。请选择：①保留卡片、删除跳转；②卡片和跳转都保留；③卡片和跳转都移除。 | MDA-02 |
| 聊天读取失败 Retry | 聊天主会话加载失败、消息列表请求失败、较早消息历史读取失败时分别显示“重试读取”；HTML 正常态没有这些按钮。它们会在同页重新请求，不会改消息数据。源码核对发现当前切出再回到聊天路由不保证重新读取，因此若移除按钮，需要提供明确恢复方式。建议保留错误说明，移除三个 Retry 按钮并用下拉刷新重读当前会话/失败历史。请选择：①保留三个错误态按钮；②移除按钮并改用下拉刷新（推荐）。 | MDA-02 |
| 聊天历史滚动按钮 | “Load earlier messages”用于按游标读取更早历史；离开最新消息时出现的“回到最新消息”用于滚回末尾。设计稿没有独立按钮。建议滚到列表顶部时自动加载更早历史，用户可通过普通滚动回到末尾，移除这两个按钮。请选择：①保留条件式按钮；②改自动加载/普通滚动并移除按钮（推荐）。 | MDA-02 |
| 聊天消息与本机待确认恢复控件 | 条件式入口包括：发送失败的“重试”、顺序阻塞时“独立发送”、处理失败的“Retry processing”，以及本机待确认消息“查看/恢复/删除”；它们用于恢复发送、查看其他实例消息或清理仅存本机的未确认消息。删除会使失败/未确认消息难以在应用内恢复或清理；删除本机记录不会删除已确认的服务端消息。建议保留数据与恢复能力，将按钮收进对应消息的长按菜单。请选择：①保留当前行内控件；②移入对应消息长按菜单（推荐）；③删除这些恢复/清理入口。 | MDA-02 |
| 聊天附件错误提示与待发送移除 | 文件选择/解析、附件上限、上传或完整性检查失败会显示系统“请求失败”提示及一个“确定”按钮；该按钮仅关闭提示。待发送附件 chip 另有长按菜单“移除附件”，可从草稿中撤销误选文件；移除后不再发送该附件。推荐保留这两个仅在出错或长按时出现的控制，避免错误提示无法关闭或草稿附件无法单独撤销。请选择：①保留；②把错误提示改为无按钮的行内状态，保留附件移除；③移除附件移除入口。 | MDA-02 |
| 审批第三动作与自动处理历史 | “灰度”“改 3 天”及“已自动处理”没有当前响应/历史合同；OpenAPI 和 handler 只支持 approve/reject。是否保留不可用入口或授权扩展 API 与业务语义，由用户选择。 | MDA-06 |
| 审批媒体预览入口 | 媒体审批在收件箱中依安全规则不显示 approve/reject，只提示“必须在带预览的任务视图中处理”；目前卡片只有“看事件链 ›”，没有直达对应 Task 详情预览的入口，用户需切到任务页自行查找。`testMordeApprovalInboxDoesNotDecideMediaWithoutPreview` 已验证：图片审批不暴露决策按钮、保留事件链入口，Simulator 截图 `/tmp/MDA-approval-media-route3-attachments-20261107/710960C0-8B2C-4C1C-85FA-6F1471A90B88.png`。建议让审批标题/卡片区域直接打开同一 Task 详情预览，不加常驻按钮；这会增加设计稿没有的行点击行为。也可以继续保持当前显式安全提示，让用户通过任务 Tab 手动查找。请审核，答复前不改。 | MDA-06 |
| Task 详情条件式 Retry 按钮 | 用户已审核并再次确认：移除详情请求失败、事件流失败、事件历史未确认三种状态下的“重试加载”按钮，通过返回后重新进入详情重新读取；事件流原有自动重连保留。普通失败/未确认说明继续显示。失败批次项的业务操作“重试”不属于这三处，仍按既有审核决定保留。Morde Simulator 已验证三种状态均无按钮，详情返回重进后重新请求并恢复。 | MDA-04；决定与验收已完成，完整布局/视觉仍待核验 |
| Task 详情内联活动记录和交付物区 | 2026-11-07 逐页对照确认：HTML `task` 页只展示任务摘要、步骤时间线和底部单个动作；Morde 详情还展示事件历史及“交付物”区。活动记录用于查看服务端事件时间线，已有独立 MDA-05 事件链页面可继续查看；移除详情内区块只减少重复入口，不删除事件或改变事件链页面。交付物区只列 API 返回的文件名和类型，没有直接打开操作；源码复核确认 Task artifacts 会被文件库聚合展示，已验证且通过权限/内容证明校验的项目可从文件库预览。移除详情区块不会删产物或文件库记录，但会失去 Task 详情里的产物概览；没有有效证明的旧/未验证产物仍不可预览。建议两块均从 Task 详情移除：活动记录留在事件链页，产物列表留在文件库。等待用户审核，答复前不改。 | MDA-04、MDA-05、MDA-11/12 |
| 云手机和 AI 工具数据源限制 | 用户已确认按设计稿保留分组。现有 `/api/devices` 是客户端安装与 Hub presence，MCP connector 是配置/metadata，均不能冒充云手机列表或 AI 实时运行态；无真实数据时在设计稿分组内呈现真实空态，不扩服务端合同。 | MDA-07（规则已确认，数据源核验仍待完成） |
| 资源页“＋” | HTML 原型当前只弹短提示，iOS 没有真实添加流。当前按现有原型保留原位“＋”并显示自动消失的“添加资源暂未接入”提示，不增加 Alert 关闭按钮，也不提示添加成功。真正的 Host/Workspace 添加流程不在本轮启用。 | MDA-07（当前处理已实施，页面视觉仍待验收） |
| 工作项范围、创建字段与行操作 | 源码盘点（2026-11-07）：项目选择器用于为 `/api/projects/{id}/work-items` 确定真实 project ID；其下“Task in this project”区包含共享会话选择、Task 选择和当前 Task 摘要，选中的 Task 会被“关联当前 Task”操作使用。新建 sheet 还显示设计稿没有的可选描述框。每行除设计稿中的“认领”外，还有“关联当前 Task”（把所选 Task 关联到工作项）、“记录 Workspace 版本”（将所选 Task 与 Workspace revision 记作交付）、“标记阻塞”（要求填写原因）或“解除阻塞”。建议保留必要的项目上下文；若保留这些真实操作，将 Task 选择收进关联操作流程，并把关联/交付/阻塞操作收进工作项行菜单，避免常驻行按钮；可选描述建议移除以贴合设计稿，或由用户批准继续保留。是否这样调整需用户审核，答复前不隐藏或迁移。当前“待认领”筛选按 `assignee_id == nil`，HTML 样例按 `status == open`；推荐按是否未分配筛选，避免把已分配但仍 open 的项算作待认领。iOS 对 `blocked` 且未分配的项仍显示认领入口，而 HTML 仅对 `open` 项显示；改为只对 open 显示会收紧现有能力，也需用户审核。当前 API 没有可靠的创建者/来源字段，不伪造 HTML 示例中的“王岚创建·来自事件链”。现有 `delivered` 不等于 review/验收状态，不新增假状态。 | MDA-08 |
| 工作项额外列表文本与键盘 Done | 行内会按真实数据额外显示描述、验收标准、阻塞原因、关联 Task ID 和交付版本；HTML `renderWi()` 只显示标题、创建者/来源、状态和认领。隐藏这些行文字不会删除数据，但会减少列表内的规格、阻塞原因和交付关联信息。创建表单或描述/阻塞原因输入框聚焦时，iOS 还显示自定义键盘工具栏“Done”；HTML 只有 `enterkeyhint="done"`，没有工具栏按钮。建议列表收敛为设计稿已有字段、保留服务端数据；“Done”可移除并用键盘完成键关闭，需先确认提交标签/焦点行为。请选择：①保留扩展文本和 Done；②列表隐藏扩展文本，保留 Done；③列表隐藏扩展文本并移除 Done（按设计稿收敛，推荐）。 | MDA-08 |
| 文件库 Task 构件行预览 | 2026-11-07 的 Morde Simulator 合成文件截图显示旧版 Quick Look 没有可见关闭、分享、保存或其他操作按钮，仅能下滑关闭；按钮清单 `/tmp/MDA-12-quicklook-screenshots/7A6B2605-5B62-4A34-8D4C-D8A62F8D682C.txt`，截图 `/tmp/MDA-12-quicklook-screenshots/564E9BCA-F5C6-42BC-B8D0-BC70AAC0C82A.png`。设计稿文件详情有返回栏、标题、正文和四项底部 Tab，文件库行点击本身只显示“打开 <文件名>”提示。处理结果：保留 proof gate 与 Quick Look 原生只读正文，外层使用设计稿的返回、标题、正文卡和四项底部 Tab；不添加分享/保存按钮。`testMordeTaskArtifactQuickLookShowsSyntheticPreview` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA12-quicklook-shell-final-20261108.xcresult`），并验证返回后保留“AI 产物”筛选。截图 `/tmp/MDA12-quicklook-shell-final-attachments/C273A19C-9526-4100-BF0F-9ADE417FA9D4.png` 与可见按钮清单 `/tmp/MDA12-quicklook-shell-final-attachments/7B200136-D3BF-46E4-837E-300685AE93A4.txt` 已检查。 | MDA-11/12；本机文本构件合成验收，无真实用户文件。 |
| 文件库“全部”中的本机文件 | 当前 Morde 的“全部”会扫描 app Documents 下 `Morde Workspace Files` 并允许有 proof 的本机文件整行打开 Quick Look；HTML 原型“全部”只列 AI 产物与我上传的云端文件，没有该本机来源。此入口用于查看保存在本机容器中的文件；删除 UI 入口不会删除文件，但会使其无法从 Morde 文件库浏览。请审核：保留本机文件行并接受该原型差异，或移除本机文件行但保留容器数据；如需搬到其他位置，需先定义访问位置。答复前不改动或迁移文件。 | MDA-11/12 |
| 活动 Task 的图片/PDF 队列 | 用户已授权并完成本地实现：公开消息 API 复用 `attachment_ids`；Go pending 查询和 v2 delivery frame 传递附件，Runtime 支持 v1/v2 并以完整附件签名幂等去重；checkpoint 将消息附件 ID 与 DB 关系复核，不匹配或未提交时标记 `unknown` 并拒绝安全确认。Runtime 不再冻结枚举 ID，而仅在个人 Server Task 加通用 ID 读取 schema；tool broker 执行时动态列出当前 Task 最近 1,000 条消息中的最多 64 个附件并复验 owner/Task/会话权限。数量上限 16、唯一/排序/格式、上传 committed、JPEG/PDF 派生文件范围及读出大小/尺寸限制均 fail closed。用户 2026-11-07 再次明确授权图片/PDF 随活动 Task 入队；既有实现及同幂等键重试合成验收覆盖范围。未新增公开路由；本机 Postgres 集成证据因未配置 `CH_TEST_DATABASE_URL` 暂缺。 | MDA-02（实现已完成；合成/定向验收通过，DB 集成待配置） |
| Workspace 级操作入口 | 设计稿 `ws` 主页面只有返回、Workspace 名称、分享、描述/路径/文件行；此前 iOS 还显示工作区选择器/版本、能力说明、上传、版本差异/恢复和传输状态。上传会向本人 Workspace 提交新检查点；版本比较/恢复能查看变化并按路径恢复；状态区反馈传输结果。用户已确认主页面收敛到设计稿，上传及版本比较/恢复保留在标题长按菜单；传输中/失败才显示对应状态，不新增常驻按钮。移除常驻入口不删云端文件或 API。 | MDA-14：决定已确认并实施；分享服务端/API 与创建 UI 已接入并验证；接收方深链领取页仍待验收。 |
| Workspace 分享 | 用户已授权真实服务端/API，并确定登录领取、单个链接只允许一位接收者领取、未领取链接 7 天失效且领取成功后立即失效；领取后创建归接收者所有的独立副本，按查看/编辑权限设为只读或可编辑，永久保留且与原 Workspace 无关。不能以撤销链接撤销已领取副本，也不得复用 ownership transfer、Host membership 或显示未经服务端确认的复制成功。2026-11-08 服务端已改为 reservation 直接归属接收者并向接收者计入配额；安全失败可由同一领取人重试，存在或不确定副作用时保留恢复 fence。Postgres 定向集成、`go test ./internal/app ./internal/storage`、`make generate` 通过。升级前已处于 claiming 且已有 target Workspace 的旧链接目前 fail closed；如何兼容待用户选择。 | MDA-14；服务端实现/定向验收已完成，旧链接兼容与逐页 UI/视觉验收待处理 |
| 云电脑分享控制权 | 用户已授权接收者控制共享云电脑；复用 `desktop:control` 成员权限，长期有效直到所有者撤销；单次控制仍按现有有限租约及续期流程。入口放 Host 行长按菜单，不加常驻图标。 | MDA-15 |
| 云电脑页面对齐与缺口 | 用户明确要求移动端不显示续租按钮、Host 到期/控制租约倒计时和冗余说明。WorkBench 保留安全租约及自动续期逻辑。常驻入口只保留浏览器、终端、文件；Workspace、VS Code 及窗口操作移入长按/上下文菜单。云电脑页按设计稿显示截图、标题/Host 信息、关闭、Stop Task、接管/交还；本机 WKWebView 截图预览已修复并通过 Simulator 截图检查。HTML 的截图操作还会把图写入绑定 Workspace，Morde 暂无此写入流程，需确认是否授权新增真实 Workspace 文件写入/API；未确认前只保留本机预览，不伪造云端保存成功。Stop Task 复用 Morde 已有 Task 停止接口，仅对当前会话中匹配 Host 的活动 Task 启用；无匹配任务禁用，多项时由菜单选择，不扩 API。 | MDA-15（常驻入口决定已确认并实施；截图 Workspace 写入/底栏快捷操作等候审核，真实 Host 与 WebView 内容仍待验收） |
| 云电脑控制状态失败按钮 | 已按设计稿移除错误状态下额外的“重试状态”按钮；错误状态仍显示在页面标题。关闭云电脑回到资源页后重进，会通过资源页既有刷新重新请求控制状态。失败状态及返回重进恢复均用本地合成 Simulator 验收。 | MDA-15（已确认并实施） |
| 现有项目管理入口 | 已审核：移除旧项目管理页面入口以贴合设计稿；保留项目数据、项目成员与资源管理服务/API，不删除数据。工作项页面仍使用真实项目 ID 和现有权限。 | MDA-01、MDA-08（决定已确认） |
| 现有通知收件箱 | 已审核：移除旧通知收件箱页面入口；保留通知数据、推送和关联任务服务/API。设计稿“通知”设置项仍不冒充收件箱。 | MDA-01、MDA-13（决定已确认） |
| PDF 大文档范围 | 源码复核：PDF 文本最多读取前 100 页/48 KiB，页图最多前 10 页；送入 Task 的解析文本会附“其余文字/页面未读取”说明，模型可识别范围不完整。聊天附件区和用户气泡只显示原 PDF 名称，用户看不到该限制。现有“扫描 PDF 超出 10 页”断言位于旧 `MinisTests`，不作为独立 Morde 验收证据。建议仅在触及限制时，在现有 PDF 附件行增加一条简短、不可交互的范围说明，不增按钮；PDF 上限是否扩展暂不纳入。 | MDA-02；用户端说明是否接受待审核 |
| 设置页真实摘要与急停恢复状态 | HTML 账号行显示“免费版 · 已用 3%”、审批规则行显示“高风险才找你”，Morde 当前只能显示真实用户名；`/api/me` 只有 id/name，审批规则页尚未实现，不能照抄 HTML 示例值。HTML 显示固定“急停全部任务”，Morde 在服务端确认全局停用后会把同一位置的唯一按钮改为“重新启用全部任务”，以便恢复任务执行，不增加第二个按钮。建议保留真实用户名与不带虚假副标题的行，并保留状态联动的单行恢复动作；该状态动作待用户审核。 | MDA-13 |
| 设置页“设备”详情额外交互 | HTML 点击“设备”只弹提示；Morde 打开详情后除客户端安装列表外，还显示 Hub relay 说明、网址输入、对在线且非本机/未撤销设备的“Open link on this device”按钮，以及二次授权弹窗；只有确认后才发送开链接请求。来源：Simulator 本轮启用本地 Hub fixture 输入 `https://example.com`，打开确认后取消，确认未发送请求。详情有两个条件式错误重试按钮（`Retry devices`、`Retry`），分别重载 Hub devices 与 client installations；同页下拉刷新已同时重载两类数据。安装列表还支持撤销其他安装，会使其会话/凭证失效。已通过 UI 用例并截图 `/tmp/MDA-settings-detail-audit3-attachments-20261107/0FD1711A-1F66-4510-8250-401CB77A68F8.png`；合成 Hub 能力，不代表真实 Hub 联调。建议保留安装清单/撤销作为账号安全管理；Hub relay 按钮/网址输入和两个重复 Retry 均是设计稿外能力，等待用户审核。答复前不调整。 | MDA-13 |
| 设置页入口与隐藏影响 | HTML 设置行点击只弹“打开「…」”提示；Morde 的“账号”进入完整账户页：显示真实名称、账号 ID、服务地址，提供 Host members、Model connections、API Keys、管理客户端安装及退出当前账户入口；退出会清除此设备登录，安装管理可撤销其他设备会话/凭证。API Keys 在“安全凭证”根行也可到达，存在重复路径。Simulator 用本地账号夹具核对以上页面和操作入口并截图 `/tmp/MDA-settings-detail-audit3-attachments-20261107/DA74B647-E98E-4ABA-852B-7A06E54966C2.png`。建议保留账户身份、退出与凭证/安装安全管理；Host members、Model connections、API Keys 详情及重复 API Keys 入口应按用户审定的设计稿差异表移除、迁移或保留。另有 Memory、Storage、Shared Folders、Mount External Folders、Backup & Restore、Data controls、Logs 等 NanoMuse 本机功能可能在收敛根页后失去入口；Storage 含清理会话产物，Data controls 含同步/隐私/云端删除，不能默默隐藏或删数据。设计稿的 MCP connector、通用权限、审批规则、通知和外观目前无真实 Morde 页面/合同；不造数据。以上均待审核。 | MDA-13 |

## 执行及审核规则

1. 先完成 MDA-00，之后按 MDA-01～15 的页面顺序一次实施和验收一页；共享导航只由集成者修改。某项接口待决时记录阻塞原因，继续其他无依赖页面。
2. 每页读取设计稿对应 `data-view` 和弹层、相关 Swift 页面及已审核表。清点普通、加载、空、失败、无权限、选中和弹层状态中实际可见的按钮、行点击、输入框、筛选、手势和返回入口；记录名称、位置、数量、目标、对应设计稿、审核依据和删除影响。系统键盘/系统文件选择器也记录，避免漏掉额外关闭或操作按钮。
3. 使用设计稿中的位置与顺序和 nanoMuse 的色彩、字体、圆角及 iOS 适配；不复制 HTML 的模拟状态栏、静态样例和演示成功反馈。任务可见控件必须有真实状态与权限来源。
4. 每页在对应 MDA 任务完成前，以 `Morde iOS设计稿.html` 的 `data-view`、控件顺序和样式为依据，核对该页的交互及 Simulator 截图；记录入口、状态、截图路径、控件差异、动作结果和未决项。内置浏览器拒绝直接打开本地 `file://`，可使用仅绑定回环地址的临时静态预览访问本地原文件；不得把设计稿发布/代理到外网。原型交互仅用于确认设计稿页面/入口，Morde 的可用性须由 Morde Simulator/本地替身验收。若不能取得 Morde 截图或真实状态，视觉或逻辑验收保持待完成，但继续其它可验收工作。MDA-16 再做跨页面回归及辅助功能检查。
5. 一项只有实现、关联入口、审核结果和本项验收均完成才勾选 `[x]`。原 MIF 任务状态、交接及历史记录由其原子清单维护，不修改旧日志来制造一致。

## 并行执行与文件边界

页面组件可以按文件所有权并行准备；共享路由、Morde 根页面、Xcode target membership 与最终页面验收由集成者串行完成。不得让两个执行者同时修改同一 Swift 文件或 `project.pbxproj`。

| 工作包 | 独占写入范围 | 依赖与交付 |
| --- | --- | --- |
| 集成 / MDA-00、01、13、16 | `Morde/MordeApp.swift`、`Morde/MordeStandaloneApp.swift`、`Morde/MordeDesignNavigation.swift`、`Morde.xcodeproj/project.pbxproj`、Morde 专属 UI 测试 | 先冻结 route/页头回调；负责 target membership、MordeRoot 导航接线、设置页和最终构建/Simulator 回归。`NanoMuseShell.swift`、`MinisApp.swift` 不进入 Morde target。 |
| 任务与审批 / MDA-03～06 | `NanoMuse/MIFTaskViews.swift`、`NanoMuse/MIFApprovalViews.swift` 及各自独立测试 | 组件通过 MordeRoot 注入 `onMenu`、任务 ID 和操作回调；不改共享导航、`MordeApp.swift` 或工程文件。 |
| 工作项 / MDA-08 | `NanoMuse/MIFWorkItemViews.swift` 及独立测试 | 使用冻结的 Morde 页头及真实项目/工作项状态；不改共享导航、根页面或工程文件。 |
| 资源、文件、占位 / MDA-07、09～12 | `NanoMuse/MIFResourceViews.swift`、`MIFLibraryViews.swift`、`MIFPlaceholderViews.swift` 及各自独立测试 | 组件以闭包接收菜单、导航及真实数据；个人数据纯合同另由共享数据工作包处理。不得修改 MordeRoot 或工程文件。 |
| 共享个人数据合同 | `NanoMuse/NanoMuseMordePersonalData.swift`、`NanoMuse/NanoMuseLegacySessionArchive.swift`、`NanoMuse/NanoMuseRooms.swift`、`NanoMuse/NanoMuseProfile.swift`、`NanoMuse/NanoMuseGoals.swift` 及经集成者确认的最小纯数据源文件 | 只提取纯 DTO、UserDefaults preference、存储/冲突合同；Morde 使用自己的 Application Support 路径，旧 NanoMuse 调用显式沿用 `NanoMuseDirs.root`；不将旧根页面、Profile/Goals UI 或旧 App Group 路径纳入 Morde target。共享数据工作包可在页面组件交付后，只改 `MIFLibraryViews.swift` 的 store 调用参数；该文件 UI 仍归资源/文件工作包，禁止并行编辑。 |

所有页面代码接入 Morde target 后，逐页截图对照和逻辑验收仍按 MDA 编号串行进行；旧 `Minis.xcodeproj` 的测试通过不算 Morde scheme 验收。

## 实施任务

状态说明：MDA-00 只验收独立 target、当前根路由基线和设计稿页面清点。MDA-01 活动导航已接通；MDA-02～15 中“已实施/待验证”仍可能只代表组件或源代码层进度。除已明确列出的 Morde scheme 可达性/交互证据外，每个页面还需完成本页状态、截图及设计稿按钮/位置核对，才能称该页面已对齐。

- [x] **MDA-00（完成：独立 Morde target build/launch 基线已核实；设计稿路由差异已登记）建立设计稿、活动 target 与页面状态基线**
  - target 边界：`Morde.xcodeproj` 的 `Morde` scheme 现已登记 Morde 独立导航/Chrome、选定 MIF 页面、明确的纯数据合同、本地化、字体和语音依赖。使用 `Morde/MordeDesignChrome.swift`，不纳入 `NanoMuse/NanoMuseShell.swift`、`MinisApp.swift` 或 `NanoMuse/NanoMuseShellChrome.swift`，避免旧根导航和旧 App Group 进入 Morde target。Morde 个人数据与附件 staging 使用 Morde app sandbox 的 Application Support；旧 NanoMuse caller 显式沿用 `NanoMuseDirs.root`，不迁移旧数据。语音 ObjC 异常捕获使用最小 `Shared/ObjCExceptionCatcher` bridging header，不带入完整 NativeOffloads bridge；standalone target 使用 `MORDE_STANDALONE` 编译条件。
  - 删除了 `MordeApp.swift` 中无调用方的 `MordeLegacyHistoryBrowser`、`MordeLibraryBrowser` 包装器；底层数据未删除或迁移。standalone DEBUG target 的旧本地聊天夹具也已隔离，不再要求纳入 legacy `ChatStore` 类型。
  - 已在本清单记录设计稿 14 个 `data-view` 页面、Workspace 分享及工作项新建弹层的控件/现状/审核边界，并确认独立 Morde target 的文件和数据边界。此项只建立基线，不代表 MDA-01～15 页面已完成。
  - 验收：独立 `Morde` scheme build 成功、Xcode 工程 lint 通过，专用 iOS 27.0 Simulator 可启动并显示登录页。登录后页面和视觉对齐不属于 MDA-00 验收范围。

- [ ] **MDA-01（实现与本机合成 UI/设计稿几何核验已完成；真实登录态下的旁聊和审批角标数据待核验；对应 MIF-01）主导航与侧边抽屉**
  - 修改：`Morde/MordeDesignNavigation.swift`、`Morde/MordeDesignChrome.swift`、`Morde/MordeApp.swift`、`Morde/MordeStandaloneApp.swift`。四个聊天/任务/审批/资源底部入口和独立抽屉已接入 MordeRoot；抽屉按主要聊天、任务、审批、事件链、资源、文件库、工作项、好友排列，其后是来自真实 Morde conversation drawer store 的旁聊和 AI 工具会话占位入口，底部是设置、搜索、新聊天。使用真实选中态、审批数量、滚动分页；搜索仅筛选旁聊。已按用户决定移除旧项目管理与通知收件箱入口，保留底层数据、推送和服务/API。“长按排序”仅保留原型提示，本轮不扩排序行为。不得把 `NanoMuseShell.swift` 接入 Morde target。最新 Morde build 已成功。
  - 本轮落实：删除侧栏“协作→项目”行及仅支撑其 UI 用例的本地 composer 夹具；`/api/projects`、项目模型/成员与工作项项目选择/API 保留。侧栏聊天/资源/文件库图形改为与 HTML SVG 形状对应；所有非 `chat/tasks/appr/res` 次级页面显示无选中 Tab。任务列表在 Morde 根导航使用 `.task(id)` 路由进入详情，其他 target 的原导航分支不改；抽屉动效改为 HTML 0.34s cubic-bezier。没有增加或调整按钮、权限或服务端合同。
  - 本轮验收：`testMordeDesignNavigationRoutes`、`testMordeDrawerGeometrySearchAndConversationPagination`、`testMordeTaskDetailShowsRealStepsWithoutBudget`、`testMordeDrawerHidesProjectManagementWhileWorkItemsKeepProjectScope` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 4/4，结果 `/Users/zhongxuan_li/Library/Developer/Xcode/DerivedData/Morde-hawfozzytljmicgvrjitsykselwa/Logs/Test/Test-Morde-2026.11.09_07-31-41-+0800.xcresult`。抽屉图标/排序/无项目入口截图为 `/tmp/MDA01-nav-final-attachments-20261109/52075715-40CF-4338-9AE0-02076379CCEB.png`；Task 详情底部无选中 Tab 截图 `/tmp/MDA01-nav-final-attachments-20261109/6A58FC77-B839-4F15-9ADE-32D6EAB6ACB5.png`。真实登录账号数据未提供，旁聊内容和审批计数无法与真实服务比对；MDA-01 继续 `[ ]`。
  - 验收：专用 iOS 27.0 iPhone 18 Pro Simulator 已通过 `testMordeDesignNavigationRoutes`：四个 Tab、抽屉事件链、好友与 AI 工具占位、工具页返回、设置十行与急停入口均可达。此用例于 `/Users/zhongxuan_li/Library/Developer/Xcode/DerivedData/Morde-hawfozzytljmicgvrjitsykselwa/Logs/Test/Test-Morde-2026.11.07_05-30-57-+0800.xcresult` 通过 1/1；聊天与任务 Tab 选中态均断言通过，截图 `/tmp/MDA-01-nav-visual-attachments-20261107/5F53E5A3-A2EC-47E2-AEC4-060453326A12.png`、`E2021BD2-54D1-4162-BB31-234C4DE58614.png`。与本地 HTML `chat`/`tasks` 对照，四项 Tab 的顺序、胶囊位置与选中态一致。`testMordeDrawerGeometrySearchAndConversationPagination` 在 `/Users/zhongxuan_li/Library/Developer/Xcode/DerivedData/Morde-hawfozzytljmicgvrjitsykselwa/Logs/Test/Test-Morde-2026.11.07_05-26-36-+0800.xcresult` 通过 1/1；断言四个 Tab 等宽/顺序、设置与新聊天按钮 38pt、搜索胶囊 38pt、旁聊行无额外图标、滚动续页、实时搜索和回车不关闭抽屉。Morde 抽屉截图 `/tmp/MDA-01-drawer-visual-final-20261107/64197CDE-D353-4578-9250-9AEBD1B6EE5B.png` 已与本地 HTML `drawer` 状态对照：86%/322pt 抽屉宽度上限、主导航顺序、22pt 图标、旁聊标题和底部设置/搜索/新聊天 38pt 控件位置/尺寸一致；旁聊已从带气泡图标的行改为设计稿的纯文本 16pt 中等字重、12pt 内边距和 12pt 圆角，入口与交互不变。Morde 背景继续采用 nanoMuse 的 grouped system 色，不复制 HTML 的 `#FAFAFA`；HTML 静态 AI 工具会话样例不显示在 Morde，按已确认占位方案保留单个“AI 工具会话”占位入口。MDA-02 的常驻 Task 目标“＋”已按审核移入长按菜单，目标选择及文字/图片/PDF 队列回归已通过；不在 MDA-01 处理。2026-11-08 源码复核确认旁聊项取自真实 conversation drawer store（排除主会话）、审批角标取自已确认待办数并在未知时留空；无真实账号数据时不能核验实际数量。2026-11-08 在 iPhone 18 Pro Simulator 启动 `io.morde.app` 后，当前页面显示离线/登录待确认状态，没有已登录账号数据；因此不能核对真实旁聊、审批数量或角标状态，合成夹具和 HTML 样例不用于代替该项。页面实现与本机几何对照已完成，但真实登录数据核验仍待可用会话，本项保持未勾选。

- [ ] **MDA-02（进行中：聊天菜单、暖色 Morde 头像/状态、浅色灰色用户气泡、图片/PDF 合成发送、活动 Task 文本/图片/PDF 队列、同账号跨会话草稿隔离和跨账号文字草稿清理均通过合成验收；聊天视觉夹具与 HTML 同为 7 条去预算文案并有精确条数断言；附件 chip 的 HTML 8pt 间距已在 Simulator 截图复核；活动 Task 附件关联后现以客户端消息 ID 复读并确认服务器附件集合，避免清理 outbox 后消息气泡丢失 chip；合成图片/PDF 入队 UI 用例通过 1/1；选定 PostgreSQL 集成已在本地临时库验证；启动重试用例 `-count=3` 与 `-count=10` 共 26 个子用例通过，历史间歇失败未复现、根因未定位；附件其他状态和逐页视觉验收未完成；对应 MIF-02）聊天页及附件**
- 本次新增附件恢复验收：`testMordeChatAttachmentUploadResumesMissingChunksAfterAppRestart` 在独立 `Morde` scheme、iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA02-attachment-resume-20261108.xcresult`）；验证文本先被接受、附件中断后重启 App，仅续传缺失分块并完成附件关联。该用例只使用合成 JPEG 和本地请求夹具，不代表真实账号/服务验收。
- 活动 Task 消息响应丢失后的同幂等键恢复也已复验；`testMordeChatComposerReconcilesQueuedAttachmentsAfterLostResponse` 在 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA02-queue-attachment-reconcile-retry1-20261108.xcresult`）。最初失败是因为测试在同步元数据可使附件进入预览按钮之前立即断言；截图已见原消息只出现一次、图片/PDF 显示“已保存”及助手成功回执。测试已改为限时等待预览按钮并通过，不涉及产品入口或按钮数量。
- 修改：`Morde/MordeApp.swift` 与 MordeRoot 的导航/页头接线。聊天附件 chip 接入个人图片/PDF 预览代码，待 MDA-12 完整验收；Morde standalone 使用 86pt 底栏占位把 composer 抬到设计稿导航上方。活动 Task 消息沿用 POST `expected_task_id`、幂等键和现有公开消息 API；Go pending message、worker v2 投递帧、checkpoint DB 关系校验、JS Runtime v1/v2 schema/去重及个人 Server Task 动态附件读取已接入。输入区只保留文本和原发送位；附件、语音及 Task options 按用户审核移到聊天长按菜单，空输入时原发送位显示黑色箭头并聚焦输入框。聊天消息按设计稿显示灰色用户气泡、带 “Morde” 标签的无底色助手正文。Task 摘要卡可进入任务详情，属于稿件之外的现有交互，作用和移除影响已列入待审核表，等用户选择后再改；聊天头像当前仅为语音状态指示图标，没有资料页跳转。
- 验收：长按聊天菜单→Task options→附件选择器已通过；独立专用 Simulator 用合成 JPEG/PDF 覆盖导入、进程内上传替身、附件 ID 消息发送及成功回执，结果 `/tmp/MDA-02-chat-attachment-final2-20261106.xcresult`，1/1 通过。根据用户明确授权，活动 Task 追加消息沿用现有 `attachment_ids`，Go worker 与 JS Runtime 已扩展投递合同。`testMordeChatComposerQueuesImageAndPDFAttachmentsOnActiveTask` 与 `testMordeChatComposerReconcilesQueuedAttachmentsAfterLostResponse` 在 iPhone 18 Pro / iOS 27.0 Simulator 同轮通过 2/2（`/tmp/MDA-02-attachments-approved-regression-20261107.xcresult`）：图片/PDF 随运行中 Task 入队并呈现；首个已接收请求响应丢失后，草稿仍保留，重试使用同一幂等键并复用已接受消息。新增 `testMordeChatDraftAndAttachmentsStayWithTheirConversation` 通过 1/1（`/tmp/MDA-chat-draft-scope2-20261107.xcresult`），同账号切换会话后验证文字草稿及图片/PDF chip 不会出现在另一会话。`testMordeChatAttachmentLimitRejectsExpandedPDFSet` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA-02-attachment-limit-ui-20261107-r1.xcresult`）：2 张图片加 5 个单页 PDF 展开为 17 个文件对象，超过 16 项后显示限额提示，未把部分附件或入队操作留在草稿。此前文本队列测试通过 1/1（`/tmp/MDA-queue-text-regression-20261106.xcresult`）。Go app checkpoint/附件权限与消息幂等定向测试通过；worker 投递、恢复、动态读取定向测试通过；Postgres 包 compile-only 通过。Runtime `node --test test/runtime.test.mjs` 本轮 68/68 通过。2026-11-07 在本机临时 PostgreSQL 路径执行 5 项定向集成复验并串行通过；启动重试用例此前出现过一次间歇失败，本轮额外 13 次复跑未复现、根因仍未知，详见后续执行记录。该证据只代表本机临时数据库，不代表部署环境或线上数据库。活动 Task 附件队列实现与合成/定向验收已完成；跨账号文字草稿隔离已由后续 Morde Simulator 用例通过。2026-11-07 按设计稿收敛输入区及消息样式，最新 `testMordeChatMultiTurnVisualFixtureMatchesDesignDensity` 与 `testMordeChatLongPressActionsOpenTaskOptionsAndImporter` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 2/2，结果 `/tmp/MDA02-chat-design-final-20261107.xcresult`；截图 `/tmp/MDA02-chat-design-final-attachments-20261107/9A942CEF-B47F-4512-9640-9B7EAB29022D.png` 已检查。确认助手正文无气泡并带 Morde 标签，用户消息为灰气泡，空输入只有黑色发送箭头，常驻“＋”和麦克风不显示；附件、语音、Task options 仍在长按菜单。MDA-02 仍因头像/Task 卡额外交互待审核、历史间歇失败根因未知、其余数据库覆盖及页面状态视觉未闭环而保持未勾选。

- [ ] **MDA-03（进行中：空态、续翻、页头几何、错误态刷新及跨账号迟到分页隔离已通过；步骤摘要与八种状态矩阵已通过；有真实摘要的五行样例按 HTML DOM 实测保持 91.6pt，无摘要两行按内容自然收紧；任务标题字距/多行行距及摘要与状态间距已局部调整；Morde 长列表末行到浮动 TabBar 胶囊的 30pt 尾距已用 Simulator 直接测量验证，Morde 专属尾部 padding 为 108pt，NanoMuse 路径保持原值；长列表/状态断言通过 1/1，任务状态/计数用例通过 2/2；最新五行样例、紧凑计数、审批等待文案及等待态控件复验通过 4/4（`/tmp/MDA03-compact-task-rows-20261109.xcresult`）；次级信息/状态文字色值已对齐，标题/详情/状态间距按 HTML 校准为 2/3pt 并由 Morde UI 用例复验 2/2；强制 UIKit 回弹回退夹具下的下拉恢复新增用例通过 1/1；页头菜单圆钮保留 NanoMuse 轻阴影，作为与 HTML `.circbtn` 无阴影的风格差异，不影响按钮位置/数量；iOS 16.0～16.3 运行时缺失，其他状态与全页视觉继续待核；对应 MIF-03）任务列表**
  - 修改：`NanoMuse/MIFTaskViews.swift`、`Morde/MordeDesignChrome.swift`、`Morde/MordeApp.swift` 和 Morde route 接线。对照 HTML `tasks`：菜单与标题在左、现有“＋”在右，页头水平边距 22pt、菜单与标题间距 10pt、菜单和“＋”均为 38pt；列表为轻量平铺行，44pt 圆形状态图标、标题、真实步骤总数/完成数以及当前步骤或结果次级信息，整行进入详情。移除当前包裹所有行的白色圆角卡片和行分隔线；活动任务用三节点图标，已完成任务用勾选图标，不显示行尾箭头。步骤总数和完成数直接使用必填 `step_count` / `completed_step_count`；服务端统一排除 budget 步骤、仅把 succeeded 计为完成。iOS 不从步骤数组推算或回退，不伪造执行者、百分比或完成数。新建任务进入聊天并聚焦输入；刷新使用下拉，分页滚动触底自动加载，不显示“加载更多”或列表内错误重试按钮；不恢复工作项分段和未经审核的额外按钮。
  - 验收：页头几何、确认空态、空中间页续翻及错误态恢复已通过独立 Morde UI 用例。错误态短列表新增仅在顶部下拉时可触发的无可见控件手势补偿，并通过 `testMordeTaskListRecoversFromLoadFailureWithPullToRefresh`；刷新函数保持旧列表/错误信息直到新结果提交，并用 `isRefreshing` 防止重复请求。最新完整 Morde UI 套件 15 项中 14 项通过、1 项真实凭据用例跳过、0 项失败，结果 `/tmp/MDA-refresh-fix-derived/Logs/Test/Test-Morde-2026.11.06_08-22-41-+0800.xcresult`。跨账号迟到分页隔离用例 `testMordeTaskListDropsLatePageAfterAccountSwitch` 已在独立 Morde target 通过 1/1，结果 `/tmp/MDA-account-switch-safe-area.xcresult`；验证 Account A 的第二页请求在途时退出并登录 Account B，迟到响应不会混入 B 的任务列表。该次复验还修正账号设置列表的底部安全区，并断言退出入口位于自定义 Tab 栏上方。短错误空态的补偿手势不增加可见控件，正常列表仍用系统下拉刷新。iOS 16.4 起使用 `scrollBounceBehavior(.always)`；16.0～16.3 已加入 UIKit 父级 ScrollView 回弹探针。本次 `testMordeTaskListRecoversFromLoadFailureWithPullToRefresh` 在 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（`/Users/zhongxuan_li/Library/Developer/Xcode/DerivedData/Morde-hawfozzytljmicgvrjitsykselwa/Logs/Test/Test-Morde-2026.11.06_22-03-57-+0800.xcresult`）。当前环境没有 iOS 16.0～16.3 Simulator，故旧系统探针运行时行为仍待复验。2026-11-07 首屏截图 `/tmp/MDA-03-visual-attachments-20261107/713EC29F-375D-48F0-A77E-C366F5A12BB9.png` 已与本机 HTML `tasks` 原型做首轮对照：菜单/标题/右侧“＋”在相同位置，38pt 页头按钮、44pt 图标、无边框平铺行与设计稿接近；HTML 为 6 条演示任务，Morde fixture 为 100 条真实形态分页数据，数量差异不作为 UI 失败。原型行有步骤数/完成数、执行者和进度示例；用户授权扩展 TaskSummary 后，服务端为摘要和详情响应统一返回真实总步数/完成数，过滤 budget 步骤且只计 succeeded；OpenAPI 现将两项计数设为必填，iOS DTO 非可选，列表不再以 steps 回退推算。八种任务状态及未知状态的行文案/图标均通过合成 Simulator 用例；第二页加载收到 403 后清空旧任务并可下拉恢复已通过；iOS 16.0～16.3 刷新运行时和完整页面视觉仍待核验，本项保持进行中。

  - 2026-11-08 行高回归：`testMordeTaskListRendersFiveDesignSampleRows`、`testMordeTaskListRendersKnownAndUnknownStatusesWithoutActions` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 2/2（`/tmp/MDA03-adaptive-task-rows-r3-20261108.xcresult`）；五行有摘要样例仍为 91.6pt，无摘要/零步骤行取消统一固定最小高度、保持紧凑两行版式且仍可点击/滚动。截图 `/tmp/MDA03-adaptive-task-rows-r3-attachments-20261108/06B9663D-5C79-452D-85A3-04483F36FFAF.png` 和原型 `tasks` 页对照：页头、新建按钮、44pt 状态图标、标题/摘要/状态次序、平铺行与底部 Tab 位置符合 CSS；测试继续断言真实计数/状态、无预算和无“加载更多”。iOS 16.0～16.3 和其他状态/全页逐控件视觉仍未验收，MDA-03 保持未勾选。

  - 2026-11-09 MDA-03 重验：`testMordeTaskListShowsStepCountsFromSummary`、`testMordeTaskListRendersKnownAndUnknownStatusesWithoutActions` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 2/2（`/Users/zhongxuan_li/Library/Developer/Xcode/DerivedData/Morde-hawfozzytljmicgvrjitsykselwa/Logs/Test/Test-Morde-2026.11.09_07-45-45-+0800.xcresult`）。复核真实 `5 执行步骤 · 2 已完成`、八种状态无操作按钮，以及长列表末行与浮动 TabBar 30pt 间距；截图 `/tmp/MDA03-task-list-rerun-attachments-20261109/0D002E6B-ACD1-4AE7-83EE-A8B77623A2D3.png`、`BF5014A1-E465-4F67-97AC-9ED01E5CCC81.png`、`9D9C7178-C3E2-493F-98C2-5658D435BC1D.png` 已检查。内置浏览器打开 HTML `tasks` 原型，复核页头、加号、任务行信息顺序、平铺样式和底部 Tab；本轮无生产页面布局改动。iOS 16.0～16.3 运行时和逐状态整页视觉仍未完成，MDA-03 保持 `[ ]`。

- [ ] **MDA-04（基础页面交互、运行中暂停/并发动作互斥、完成/失败/取消终态、审批/澄清及“操作结果待核对”已通过 Simulator 验收；详情 403 错误态与返回重进恢复已验证；Task/账号 scope 变化保护及账号切换迟到详情响应隔离已通过；标题到摘要间距、运行中节点及时间线标题/说明多行行距已按 HTML 局部收敛；已知 queued 步骤映射为待办圆环；三处条件式“重试加载”已按审核移除，事件流仍自动重连；未知响应及临时 503 均可经原操作和同一幂等请求恢复已验证；MDA-02 已验证任务详情往返后恢复聊天草稿；已确认的暂停/继续/取消操作固定在底部 Tab 上方；已完成与 HTML 的首轮截图结构对照，其他状态及完整逐控件位置/尺寸核对仍待完成；对应 MIF-03）任务详情**
  - 修改：`NanoMuse/MIFTaskViews.swift` 与 `Morde/MordeApp.swift` 中已接入的任务详情 route。对照 `task` 的返回/任务名、目标摘要、真实任务步骤时间线和底部“急停这个任务”；步骤链现排在摘要后。保留用户已审核的审批、澄清、暂停/继续/取消及失败批次项重试等真实操作，不因视觉调整丢失真实动作；内联活动历史和交付物区不在设计稿中，已列入待审核表，在决定前不移除或迁移。已移除详情请求失败、事件流失败和事件历史未确认状态下的“重试加载”，通过返回后重进详情重读；事件流仍按现有逻辑自动重连。当前 API 无独立“立即急停”动作，不能把暂停或全局急停改名冒充。若剩余控件的展示位置仍无法同时满足设计稿和已审核决定，列出具体入口影响并请用户确认布局/语义映射。
- 原型差异（2026-11-07）：HTML 显示经理状态摘要、四个时间线节点和底部固定红色“急停这个任务”；Morde 使用真实 Task 状态、步骤、活动与产物，Task API 没有经理元数据或独立“急停”动作，不能复制样例信息或把暂停/取消改名为急停。用户已审核保留现有操作入口。最新 Morde 详情已把真实暂停/继续/取消等既有操作固定在底部 Tab 上方；可滚动正文展示真实步骤、事件和产物。动作位置已对齐设计稿底部区域，但 Morde 保留的两个真实操作按钮与 HTML 单个“急停”按钮在数量、颜色和语义上不同，这是已审核保留现有操作后的差异；不新增按钮、不伪造 API。详情内联事件历史和交付物区不在 HTML `task` 页面中，其用途及移除影响已列入“接口与待决能力”，不擅自删除。
  - 验收：定向 Morde UI 用例 `testMordeTaskDetailShowsRealStepsWithoutBudget` 已通过，验证目标文本、真实步骤、暂停入口及预算字段不显示；结果包 `/tmp/MDA-page-smoke-fixed-derived/Logs/Test/Test-Morde-2026.11.06_05-47-26-+0800.xcresult`。本轮详情加载及任务操作按捕获的 task ID 执行，并检查取消状态、账号 scope 与当前 task ID；scope/task 切换时清除未发送澄清草稿、关闭取消确认并使旧操作 spinner 失效；任务动作、审批和澄清复用在途锁。当前合并后 Morde UI 详情基础用例通过，完整定向结果 `/tmp/MDA-current-pages-signed.xcresult`。2026-11-07 新增 `testMordeTaskDetailPauseLocksActionsAndRefreshesStatus`，iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA-04-task-actions-20261107.xcresult`）：运行中详情有暂停/取消；暂停请求待响应时两个动作同时禁用；本地 API 返回 202 后详情刷新为“已暂停”，暂停入口消失、继续入口出现。前后截图：`/tmp/MDA-04-task-actions-attachments-20261107/21A28201-D56E-4301-9081-26EFA55D0353.png`、`/tmp/MDA-04-task-actions-attachments-20261107/B32B664B-D1B1-4BFB-8D1A-3204DC1129CD.png`。这些仍是本地合成 API，不代表真实 Portal 联调。暂停操作响应丢失后通过原暂停入口、复用同一幂等请求恢复已通过合成验收；聊天文字/图片/PDF 草稿在任务列表与详情往返后保留已通过；其他失败反馈及与 HTML 的逐页截图对照仍待验收。
  - 2026-11-09 Task 详情原型复核：内置浏览器打开 HTML `task` 状态并与 Morde 详情截图 `/tmp/MDA04-summary-spacing-align-final-attachments-20261108/5D23002A-BB26-407D-B6FE-24F1617E0FAA.png` 对照。返回/标题、状态副标题、时间线、底部操作区及四个 Tab 的层级顺序一致；Morde 按真实数据展示三条步骤及交付物区，HTML 展示经理摘要与演示时间线，未复制样例文本/预算。底部保留用户审核过的暂停/取消两个真实操作，所以与 HTML 一个红色“急停”在按钮数量、颜色和动作语义上仍有已审核差异；事件/交付物额外区仍在待审表，本轮不移动或删除。此轮只读对照，没有改产品代码；MDA-04 仍因完整状态与逐控件视觉、待审区块未闭环而保持 `[ ]`。

- [x] **MDA-05（完成：无任务空态、关联任务无历史、加载/网络错误/恢复、事件状态与七行密度、失败节点及账号切换/迟到响应隔离均已通过 Morde Simulator；状态视觉与 HTML 对照完成；本轮按 `.lead-sub`/`.ev` 补齐标题字号、事件标题/摘要字号及 2pt 间距；对应 MIF-04）独立事件链页**
  - 修改：`NanoMuse/MIFTaskViews.swift` 与 `Morde/MordeApp.swift`。复用 MordeDesignRoute 的 `eventChain` 路由和现有真实事件展示；对照 `chain` 的菜单、标题、关联任务、时间线顺序及状态。没有当前关联任务时显示真实空态，不加任务选择按钮；事件仅使用已有允许展示的字段，不显示预算。事件节点优先按事件 payload 的状态绘制完成/失败/进行中/待确认；无法识别时回退到 Task 状态和序号。步骤摘要使用 Task 步骤状态本地化，避免将 `succeeded` 显示为“状态未知”。事件流请求必须在取消、账号 scope 变化或当前任务 ID 变化时丢弃过期响应；页面事件流失败不得覆盖已加载的任务详情。旧 `NanoMuseShell.swift` 中的路由不能作为 Morde target 接线。
  - 验收：无任务空态/返回聊天用例通过 1/1，结果包 `/tmp/MDA-05-chain-derived/Logs/Test/Test-Morde-2026.11.06_07-10-00-+0800.xcresult`。新增关联任务时间线用例通过 1/1：合成事件流先断线、再要求从快照游标恢复，收到重复序号后界面只显示一条“Recovered event”，且不显示预算步骤；结果包 `/tmp/MDA-05-chain-derived/Logs/Test/Test-Morde-2026.11.06_07-44-42-+0800.xcresult`。新增 `testMordeEventChainShowsConfirmedNoHistoryForLinkedTask` 在 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA-05-event-chain-no-history-derived/Logs/Test/Test-Morde-2026.11.07_02-39-26-+0800.xcresult`）：本地服务返回 HTTP 204 后显示已确认的无事件历史状态，不渲染事件行或预算步骤；截图 `/tmp/MDA-05-event-chain-no-history-screenshots/F27AF2F4-8828-4D05-B3D6-030C10AC5B19.png`。页面为无历史提示增加稳定 accessibility ID。新增账号隔离用例 `testMordeEventChainSwitchingAccountsDoesNotShowPreviousAccountData` 在 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA-05-account-isolation-final.xcresult`）：账号 A 事件请求在途时登出并切换至账号 B，B 侧显示自己的同 ID 任务详情/事件，不显示 A 私有数据。事件状态节点与原型节点表现已开始逐态核对；取消后迟到投递与设计稿截图对照见本节后续执行记录。

- [ ] **MDA-06（普通审批、版本冲突刷新、重复决策禁用、未知状态隐藏徽标、媒体审批禁决策及加载失败已验收；首屏标题/待办数和事实行距/颜色/“看事件链”字号已局部对齐，批准提交后的事件链入口显示时机待用户决定；待额外动作/历史和媒体直达预览决定、完整状态视觉核对；审批待办数标题已按 HTML 收紧为“%d 件”并完成 9 种语言本地化，`testMordeApprovalInboxShowsOnlyConfirmedDecisions` 在专用 iPhone 18 Pro Simulator 通过 1/1（`/tmp/MDA06-short-approval-count-20261109.xcresult`），截图 `/tmp/MDA06-short-approval-count-attachments-20261109/92FDF068-234D-402A-A66B-A05C41B5DAF5.png` 已检查；对应 MIF-05）审批页**
  - 修改：`NanoMuse/MIFApprovalViews.swift`；MordeRoot 在 `Morde/MordeApp.swift` 注入真实审批数据、待处理数和路由。对照 `appr` 的菜单、真实待处理数、平铺审批信息、驳回/第三动作/批准顺序及首条审批的事件链入口；待办数显示在标题旁；若 API 返回项状态不全或并非 `waiting_user`，不得显示“0 件待办”误导用户，应标识部分/未知状态。保留下拉刷新，移除额外刷新按钮和静态技术状态说明。已有批准/驳回复用真实请求和版本/幂等处理；API 仅支持 approve/reject，且没有已处理审批历史合同，第三动作和自动历史须等用户选择是否扩展。
- 验收：定向 Morde UI 用例 `testMordeApprovalInboxShowsOnlyConfirmedDecisions` 已通过，合成数据确认标题待办数为 1、拒绝/批准入口对应同一审批项且没有部分状态误标；扩展测试确认“看事件链 ›”进入标题为 `Local approval fixture` 的对应任务事件链页，结果包 `/tmp/MDA-approval-event-chain-derived/Logs/Test/Test-Morde-2026.11.06_06-33-54-+0800.xcresult`。`testMordeApprovalRetriesUnknownResponseWithSameDecision` 在 `/tmp/MDA-approval-retry-unknown-fixed-20261106.xcresult` 通过：首次响应丢失后保留待办，重试必须复用原幂等键，最终待办只清除一次。`testMordeApprovalRejectSubmitsAndRemovesPendingActions` 在 `/tmp/MDA-approval-reject-results-20261106.xcresult` 通过，确认拒绝提交后移除待决按钮。待办数未知时已由可空状态隐藏徽标；`testMordeApprovalInboxDoesNotDecideMediaWithoutPreview` 在 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA-approval-media-route3-20261107.xcresult`），确认带预览操作 ID 的媒体审批在收件箱不显示批准/拒绝，提示前往带预览任务视图并保留事件链入口。直达 Task 预览的入口行为待用户审核。`testMordeApprovalConflictRefreshesAndRemovesStaleDecision` 通过 1/1（`/tmp/MDA-approval-conflict-refresh-20261107.xcresult`）：合成服务返回版本冲突后详情刷新为已完成，旧批准按钮被移除，标题待办数从 1 更新为 0。`testMordeApprovalDisablesBothDecisionsWhileSubmitting` 通过 1/1（`/tmp/MDA-approval-disable-race-20261107.xcresult`），延迟响应期间批准和拒绝同时不可用。加载失败已于 2026-11-07 通过 Morde Simulator 验收；无服务端能力时不编造历史或决策成功。第三动作/自动历史待用户审核，整页视觉未核对。

  - 2026-11-07 补充验收：`testMordeApprovalInboxShowsUnconfirmedStateWhenTaskListFails` 在 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA-05-event-chain-no-history-derived/Logs/Test/Test-Morde-2026.11.07_02-42-59-+0800.xcresult`）。合成审批列表返回 503 后显示“无法确认待办”，不显示“0 件”徽标、空收件箱或批准/拒绝按钮；截图 `/tmp/MDA-06-approval-failure-screenshots/936AEEDE-B311-448C-A384-C27F35905095.png`。本地化错误状态增加稳定 accessibility ID。媒体直达 Task 预览行为仍待审核，页面视觉尚未与 HTML 并排核对。
  - 2026-11-07 下拉恢复验收：`testMordeApprovalInboxShowsUnconfirmedStateWhenTaskListFails` 在 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA-06-approval-refresh-fixed.xcresult`）。合成列表首次返回 503，页面显示未确认态；用户下拉后下一次列表请求成功并确认空收件箱，计数恢复为 0，错误态消失。诊断发现 SwiftUI 会在成功响应返回时标记刷新任务已取消；现在以账号 scope 与 reload generation 拒绝旧响应，保留当前账号、当前刷新已成功返回的数据。无新增可见按钮；该用例只覆盖失败到确认空列表的恢复，已有审批项、计数及动作由独立成功用例覆盖。

- [ ] **MDA-07（进行中：资源分组、确认空态、Host 权限与生命周期、错误态下拉恢复已有合成验收；已移除云手机独立标题并对齐“云端主机”文案；浅色画布已对齐设计稿 `#FAFAFA`；Workspace 按真实 API 显示容量/使用率、只读属性和挂载 Host，容量摘要字号对齐原型 14px，行图标改为 HTML 细线托盘形；Host/Workspace 多行标题行距及真实空态圆形图标已按 HTML 做局部调整，错误态布局不变；资源长列表末行到浮动 TabBar 胶囊的 30pt 尾距已用 Simulator 直接测量验证，Morde 尾部 padding 为 95pt（空态行自带 13pt 尾距），NanoMuse 路径保持 22pt；共享主机区分只读/可控制权限；新增普通单 Host/Workspace 首屏 Simulator 截图并与 HTML 对照，确认 Host 控制状态一行和无云手机/AI 工具运行数据的空态仍有页面差异，Host 状态文字去留待审核；整页视觉仍未闭环；对应 MIF-08）资源页**
  - 修改：`NanoMuse/MIFResourceViews.swift`、其独立测试和 Morde 本地化；MordeRoot 在 `Morde/MordeApp.swift` 接入资源数据与 route。对照 `res` 的菜单、标题、右侧“＋添加资源”、云端主机/云手机、Workspace 和 AI 工具区；已核实 `/api/devices` 是客户端安装与 Hub presence，MCP connector 是配置/metadata，均不能证明云手机或 AI 工具的实时运行态。Host/Workspace 使用平铺整行；云手机和 AI 工具分组显示本地化真实空态。保留已批准的交还按钮。ready 且有真实 Host 绑定的 Workspace 显示“已挂载 · Host 名称”，同步中、不可用、未挂载等状态继续按真实字段显示；没有 Host 绑定时不拼接名称。未确认运行状态服务合同前不填演示数据，也不扩服务端。
  - 验收：`testMordeResourceWorkspaceRouteBrowsesSyntheticManifest` 在 `/tmp/MDA-resources-heading-final-20261107.xcresult` 通过 1/1，并保存截图 `/tmp/MDA-resources-heading-attachments-20261107/49E9B331-CFCE-4A73-9E4C-D53E5C2C83D0.png`。断言中文标题为“云端主机”、无“云端电脑”或独立“云手机”标题，真实 Host、云手机不可用状态和 Workspace 按屏幕顺序排列；保留状态行，没有把 `/api/devices` 客户端安装记录冒充云手机实例。截图使用多状态生命周期夹具，只能对照页头和首屏 Host，不代表整页视觉已通过。原 `testMordeResourceWorkspaceRouteBrowsesSyntheticManifest` 于 `/tmp/MDA-resources-honest-empty-20261106.xcresult` 通过，覆盖 Workspace 打开、合成目录/子目录浏览、返回、添加资源提示和无额外 Alert 关闭按钮；AI 工具仍显示无实时运行状态。`testMordeResourcePreferenceFailureKeepsConfirmedEmptySections` 在 `/tmp/MDA-resource-refresh-empty-20261106.xcresult` 通过，Host/Workspace 列表确认为空时呈现真实空态，默认 Host 偏好失败不冒充列表错误；`testMordeResourcePermissionMessageRequiresRunningSharedHost` 与 `testMordeResourceHostLifecycleStatesMatchServer` 在 `/tmp/MDA-resource-host-states-20261106.xcresult` 通过 2/2，验证共享 Host 仅在运行态显示权限提示，已停止/启动中/停止中/失败/删除中状态与服务端数据一致。此前多轮刷新失败结果包为 `/tmp/MDA-resource-refresh-empty-20261106.xcresult`、`/tmp/MDA-resource-refresh-locator-20261106.xcresult`、`/tmp/MDA-resource-refresh-gesture-fix-20261106.xcresult`、`/tmp/MDA-resource-refresh-recovery-final-20261106.xcresult`、`/tmp/MDA-resource-refresh-coordinate-20261106.xcresult`、`/tmp/MDA-resource-refresh-any-pull-20261106.xcresult`；定位后确认 SwiftUI 下拉刷新任务曾在列表状态更新期间取消，第三次请求虽成功却未应用。现通过 `.refreshable` 中的独立 Task 保持刷新工作，并让 `refreshResourceLists()` 等待现有全局加载结束，而不是丢弃并发刷新。`testMordeResourcePullToRefreshRecoversAfterListFailure` 在 iPhone 18 Pro / iOS 27.0 Simulator 干净复跑通过 1/1（`/tmp/MDA-resource-refresh-clean-final-20261107.xcresult`）：前两次列表请求失败后，下拉触发后续请求，Workspace 行出现且错误状态清除。新增 Host/Workspace 状态断言覆盖已挂载、同步中、不可用和未挂载/无 Host 名称；本轮 `xcodebuild build-for-testing` 在 iPhone 18 Pro / iOS 27.2 ARM64 Simulator 构建 Morde 与 MordeUITests 成功（`/tmp/MDA-07-workspace-mounted-test-build`）。此前 UI 用例曾在登录页前触发 `NSURLErrorDomain -3003` DNS 失败；为使本地合成验收 fail closed，测试 Origin 与资源/工作项/附件 fixture 均改用 `127.0.0.1`。首次重跑因资源 fixture 尚匹配旧主机名而没有行（`/tmp/MDA-07-loopback-workspace-test/Logs/Test/Test-Morde-2026.11.07_12-40-20-+0800.xcresult`）；修正后 `testMordeResourceWorkspaceRouteBrowsesSyntheticManifest` 通过 1/1（`/tmp/MDA-07-loopback-workspace-test/Logs/Test/Test-Morde-2026.11.07_12-44-37-+0800.xcresult`），截图 `/tmp/MDA-07-loopback-workspace-test-attachments/BC0342C8-DAF9-4C7A-9B29-04E46C56449D.png`。Host 行额外展示操作者、租约期限和交接摘要等信息，设计稿未显示，保留待审核；其他错误/权限状态和完整页面视觉仍待核对，任务保持未勾选。
  - 2026-11-07 资源 API 字段补齐：`MordeWorkspace` 按当前 OpenAPI 必填合同解码 `used_bytes`、`quota_bytes` 和 `is_read_only`，行内按设计顺序显示容量/使用率，再显示真实挂载与只读状态；缺字段时按无效 Workspace 响应处理，不兼容旧响应。共享 Host 按 API 权限区分 `desktop:view`、`desktop:control` 和无查看权限；控制成员不再误标只读，进入共享 observation-ticket 路由。无服务端/API 扩展。`testMordeResourceWorkspaceRouteBrowsesSyntheticManifest` 与 `testMordeSharedHostDesktopControlMemberCanTakeOverAndRelease` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 2/2（`/tmp/MDA07-resource-permissions-derived/Logs/Test/Test-Morde-2026.11.07_22-16-19-+0800.xcresult`）；截图 `/tmp/MDA07-resource-permissions-final-attachments/F93783A7-E4E6-4052-988A-9FF041AB3C53.png` 已检查，显示 API 返回的 `128 GB · 已用 46%` 和只读副本标记。Host 操作者/租约等额外文字仍待用户审核，云手机/AI 工具无实时状态 API，且资源页全屏视觉尚未完成对照，MDA-07 保持未勾选。

- [ ] **MDA-08（列表、筛选和创建表单基础交互已通过；创建错误保留输入；新建标题 17.5pt/`-0.175pt` 字距、输入字号 15pt/外层最小高度 44pt、筛选/元数据/状态/类别色值已局部对齐；认领按钮相对行垂直居中；空态已收敛为设计稿同类的 14.5pt 次级文字行；3 个错误态 Retry 已按用户审核移除；顶部 2pt 留白已按 HTML 对齐；Morde 长列表尾部 padding 已从 14pt 调整为 95pt，末行到浮动 TabBar 胶囊的目标 30pt 尾距通过最终合法夹具直接测量验证（`/tmp/MDA08-tail-gap-final-state-20261109.xcresult` 通过 1/1）；空标题主按钮保持黑色、点击后聚焦标题且不发起创建，正常创建文案标点空格已对齐 HTML 并通过 1/1；待审核控件收敛决定、其他状态和整页视觉核对仍待完成；对应 MIF-06、MIF-07）工作项页和新建弹层**
  - 修改：`NanoMuse/MIFWorkItemViews.swift`，MordeRoot 页面路由由集成者在 `Morde/MordeApp.swift` 接入。从 Morde 抽屉直接打开独立 `work` 内容；页头为菜单、标题、“＋”，筛选按全部/缺陷/需求/待认领横向排列；“＋”打开原生创建 sheet，列表改平铺行。保留真实项目作用域、权限、幂等创建与 API 合同。用户已确认删除项目列表、共享会话列表、工作项列表加载失败时各自的 Retry 按钮；源码确认错误说明保留，`.refreshable` 调用 `store.load()`。描述、项目/会话/Task 选择和额外行操作的去留与位置按“工作项范围、创建字段与行操作”待决表执行，获审核前不隐藏或迁移。
  - 验收：`testMordeWorkItemsRouteShowsDesignFiltersAndCreateForm` 在 iPhone 18 Pro / iOS 27.0 Simulator 通过，结果 `/tmp/MDA-workitem-permission-filter-final-20261106.xcresult`：viewer 的创建按钮禁用、owner 的创建按钮启用；缺陷/需求/待认领筛选显示匹配行并隐藏其他类别。`testMordeWorkItemCreateSubmitsToSelectedProject` 在 `/tmp/MDA-workitem-create-success-20261106.xcresult` 通过，合成 API 创建后标题/描述出现在选定项目列表且表单关闭；`testMordeWorkItemCreateFailureKeepsFormAndInput` 在 `/tmp/MDA-workitem-retry-removal-20261106.xcresult` 验证冲突后表单和标题保留。均为本机 fixture，不代表真实 Portal 联调。当前 API 没有可靠创建者/来源字段；“待认领”按 `assignee_id == nil` 筛选，而 HTML 样例按 `state=open` 展示；`blocked` 且未分配的项仍显示认领入口。设计稿中文创建按钮、标题提示和认领文案已在本地化表核对正确。2026-11-07 更新的用例在列表和创建弹层分别保存 Simulator 截图：列表 `/tmp/MDA-08-work-items-visual-evidence-attachments-20261107/5AD5E76C-344C-4DEC-8818-397CC14F7A94.png`，弹层 `/tmp/MDA-08-work-items-visual-evidence-attachments-20261107/54B88779-0C79-4743-9A27-A3FFA9E03EDA.png`；本地 HTML `work` 页面及 `wiSheet` 已在内置浏览器逐项核对。差异：Morde 在筛选器前多项目选择器和“此项目中的任务”会话/Task 卡片；行上出现真实阻塞管理动作，而 HTML 展示创建者/来源字段（Morde API 无可靠字段，不能造样例值）；新建弹层多可选描述且比 HTML 矮底部紧凑式弹层占屏更高；未分配但阻塞的项在 Morde 待认领筛选中并可能显示认领，HTML 仅显示 `open`。项目/Task 上下文、描述、额外行操作及状态筛选差异已提交待审核，答复前保留。截图用于后续版式比对，不代表真实 Portal 联调。项目列表、共享会话列表、工作项列表的失败状态现已由下拉刷新恢复，无 Retry 按钮用例见 2026-11-07 复验记录；成功创建后会重置为“全部”并显示“已创建 · 团队可见”短暂提示，定向 UI 用例及浮动 Tab 上方可见性断言已通过；项目切换、键盘 Done、行操作专项状态与整页视觉验收仍待完成。

- [x] **MDA-09（好友占位路由、标题/说明、无额外好友操作及视觉对照已通过；对应 MIF-10）好友占位页**
  - 修改：`NanoMuse/MIFPlaceholderViews.swift`、`Morde/MordeApp.swift` 与 `MordeDesignRoute.friends` 路由。按已审核需求第 3.9 节，只显示好友页标题和“暂未接入”说明；隐藏 HTML 样例里的好友申请、好友列表、“＋”添加和行操作，不接好友服务、不造数据、不显示成功反馈。
  - 验收：`testMordeDesignNavigationRoutes` 在 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1，确认好友入口、标题、占位说明与无添加按钮；结果包 `/Users/zhongxuan_li/Library/Developer/Xcode/DerivedData/Morde-hawfozzytljmicgvrjitsykselwa/Logs/Test/Test-Morde-2026.11.06_22-10-47-+0800.xcresult`。导出截图 `/tmp/MDA-placeholder-screenshots-20261106/C38DB79A-1DFA-4D50-8ECA-E524FA211652.png` 并对照本地 HTML `friends` 页：页头、标题和原生 Tab 导航位置一致；好友申请、列表、添加和行操作依用户决定隐藏，不造数据或成功反馈。页面返回抽屉后可继续导航。

- [x] **MDA-10（AI 工具会话占位路由、返回、标题/说明、无样例操作及视觉对照已通过；对应 MIF-10）AI 工具会话占位页**
  - 修改：`NanoMuse/MIFPlaceholderViews.swift`、`Morde/MordeApp.swift` 与 `MordeDesignRoute.toolChats` 路由。按 `toolchat` 调整抽屉入口、标题和返回位置，仅展示“暂未实现”；不依赖旧 Shell。
  - 验收：`testMordeDesignNavigationRoutes` 在 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1，确认抽屉入口、占位页返回聊天及无会话操作按钮；结果包 `/Users/zhongxuan_li/Library/Developer/Xcode/DerivedData/Morde-hawfozzytljmicgvrjitsykselwa/Logs/Test/Test-Morde-2026.11.06_22-10-47-+0800.xcresult`。导出截图 `/tmp/MDA-placeholder-screenshots-20261106/85974018-717D-4779-B289-6E001AE6C374.png` 与本地 HTML `toolchat` 页对照：返回/标题和底部导航位置一致；原型里的 Claude 对话样例及输入/发送按用户决定不展示。Morde 仅显示“暂未实现”，不读取或伪造会话数据。

- [ ] **MDA-11（进行中：筛选、来源归类、文件库标题及列表视觉已有 HTML 对照；浅色模式筛选/摘要/默认类型块色值已对齐；Morde 共用页头和文件库专用页头的汉堡图形按 HTML 路径收敛为 13×1.7pt、3.1pt 线距并保留 38pt 点击区；PDF/DOC/图片徽标色按 CSS 精确 RGB 对齐；浅色背景继续使用 nanoMuse system background，和 HTML `#FAFAFA` 有轻微差异，遵循既有风格规则；菜单按钮及标题行几何已按 HTML 单页覆写；本机文件、Task 构件与云端附件标题按 HTML 增加 1pt 顶距和 1.5pt 多行行距；All 筛选空行合并与单一全空提示、来源独立权限/空态已有专项用例；图片/PDF 发送、预览和“我上传的”筛选、仅 metadata 行权限及同步失败状态在 Morde 合成账号回归通过；待权限/证明及其他状态；对应 MIF-09）文件库**
  - 修改：`NanoMuse/MIFLibraryViews.swift`、`Morde/MordeApp.swift` 的 Morde library route。沿用新 Morde 抽屉路由，对照 `lib` 的菜单/标题和全部、AI 产物、我上传的三个横向筛选；筛选随单一纵向滚动区滚动，列表使用平铺内容与设计稿边距/底部留白。AI 产物按真实 Task artifact 归类；个人云端文件只有在 `hasAttachment` 指向实际附件时才能归到“我上传的”，仅 metadata 的行只在“全部”展示；本机索引不擅自归入上传/AI 产物。保留现有权限、证明及已验证预览门控；文件上传行无障碍标签包含真实文件名。
- 验收：`testMordeFullLayoutUploadsAndSendsImageAndPDFFixture` 已在 Morde target 验证“全部 / AI 产物 / 我上传的”三种筛选：个人附件只出现在“全部”和“我上传的”，合成图片/PDF 均可见；同用例也核对图片详情返回后筛选保留。该用例于 2026-11-08 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA11-12-library-empty-and-header-20261108.xcresult`），新增断言确认“全部”有上传内容时不会额外显示本机空行；同时确认预览标题与返回按钮间距为 10pt。截图导出至 `/tmp/MDA11-12-library-empty-and-header-attachments-20261108/` 并检查。旧截图 `/tmp/MDA-library-preview-screenshots-20261106-final/7E987F7F-3731-439C-9788-D2CCF9BB9AD3.png` 与本地 HTML `lib` 页对照，菜单、标题、三段筛选、平铺行及底部 Tab 的顺序/位置对应，类型/大小来自合成附件元数据。`testMordeLibraryShowsUnavailableInsteadOfEmptyWhenPersonalSyncFails` 通过：`/api/sync/snapshot` 返回 503 时显示“不可用”，不误报“云端还没有文件”；结果 `/Users/zhongxuan_li/Library/Developer/Xcode/DerivedData/Morde-hawfozzytljmicgvrjitsykselwa/Logs/Test/Test-Morde-2026.11.06_22-43-00-+0800.xcresult`。未登录提示与权限/证明缺失的文件库状态仍待验收；账号 scope 失效与同步关闭期间的预览取消已独立通过，见 2026-11-07 执行记录。无额外上传、下载、分享、删除按钮。旧 `MinisTests` 不计为 Morde 页面验收。

- [x] **MDA-12（完成：聊天/文件库图片/PDF 全屏详情和 Task Quick Look 已完成返回、筛选保留、成功/加载/失败/取消、权限证明与文件完整性边界验收；TabBar 子按钮标识修复并验证聊天/文件库切 Tab 与 Quick Look 四 Tab；本地 HTML 对照确认详情页头、内容卡、底部四 Tab 的层级/位置/数量一致；对应 MIF-09）文件详情/预览**
  - 修改：`Morde/MordeApp.swift`、`NanoMuse/MIFLibraryViews.swift` 和必要的预览组件。对照 `file` 的返回按钮、文件标题和正文；聊天进入后返回聊天，文件库进入后返回文件库并保留筛选和滚动位置。已有 `downloadPersonalAttachment` 需在当前账号可读、同步开启且附件 ID 有效时复用；个人附件改在原有文件行/消息附件上点击打开，不加按钮。使用 opaque handle 传递目标，在下载前后检查账号 scope、同步状态和附件 ID；校验后的个人图片/PDF 使用无额外操作入口的原生只读预览，临时文件不写入持久下载目录。本机与 Task 文件沿用当前 proof gate，需检查其 QuickLook 实际可见控制是否超出设计稿。
  - 验收：独立 Morde UI 用例使用本机 `URLProtocol` 合成响应验证聊天和文件库图片/PDF 预览、返回路径与筛选保留；`testMordeFullLayoutUploadsAndSendsImageAndPDFFixture` 在 `/tmp/MDA-library-filter-nav-signed-20261106.xcresult` 通过，覆盖聊天图片/PDF 只读预览、文件库预览返回筛选，以及从预览切到聊天 Tab 后回到聊天输入区。`testMordeChatAttachmentWithoutLibraryProofCannotOpenPreview` 在 `/tmp/MDA-attachment-missing-proof-fixed-20261106.xcresult` 通过：缺少当前个人库附件关联证明时，已接受的消息保留 4 个附件引用但没有可点击预览和空白/成功预览。文件库个人附件在预览全屏页内先显示加载态，同一页面随下载完成更新内容；聊天附件下载完成后打开同一全屏详情。两条路径均显示原生四 Tab 底栏，不显示分享/保存按钮。HTML `file` 页面与 Morde 聊天图片/PDF及文件库图片详情截图已对照：页头返回+标题、正文预览卡、底部 Tab 顺序/位置一致，实际附件内容按文件呈现，不伪造 HTML 样例文本。截图 `/tmp/MDA-library-preview-screenshots-20261106-final/5BF775FD-C912-4879-981A-AB1C78F4C2EA.png`、`CDB7B16B-68C1-4ECC-9098-89B64BE0F879.png`、`2377DE81-8A75-4F4F-96BE-A8D8A699E3AC.png`。下载 503 不打开空白预览，用户关闭加载页会取消请求且不出现迟到错误/预览；`testMordePersonalAttachmentPreviewCancelsWhenClosedWhileLoading` 通过。`testMordeAttachmentPreviewRejectsMismatchedContentLength` 通过：下载头与正文长度不匹配时 fail closed，不打开预览；这两项及成功预览/下载失败同轮 3/3 通过（`/Users/zhongxuan_li/Library/Developer/Xcode/DerivedData/Morde-hawfozzytljmicgvrjitsykselwa/Logs/Test/Test-Morde-2026.11.06_22-34-50-+0800.xcresult`，header proof 追加复验 2/2 见 `/Users/zhongxuan_li/Library/Developer/Xcode/DerivedData/Morde-hawfozzytljmicgvrjitsykselwa/Logs/Test/Test-Morde-2026.11.06_22-43-00-+0800.xcresult`）。临时清理仍只接受临时目录内普通文件，清理失败保留 pending URL；Morde target 删除失败/权限恢复重试用例现通过（`/tmp/MDA-12-temp-cleanup-retry-20261107-r2.xcresult`），首次失败保留 URL，重试清理后从 pending 移除。退出导致账号 scope 失效、A→B 切换以及同步关闭期间的预览取消/迟到响应丢弃均由 Morde UI 用例覆盖，A→B 结果见 2026-11-07 的独立执行记录；权限变化仍待验收；Task 构件 Quick Look 已包上设计稿返回栏、标题、正文卡及四项 Tab，按钮清单未见分享/保存操作，返回后保留“AI 产物”筛选。无权限、无效附件和当前 scope 变化必须 fail closed；没有重复关闭/返回、编辑、分享或下载按钮；无法预览不显示空白成功页，不连接真实服务、不传真实文件。

- [ ] **MDA-13（进行中：根页 10 行/分组/返回/底部导航与新 chevron 样式已有 Simulator 截图和 HTML 对照；账号/设备详情路由与五个占位入口在 Morde Simulator ad-hoc 签名复验 1/1（`/tmp/MDA13-settings-routes-adhoc-20261108.xcresult`）；全局急停确认与恢复路径已验收；设备页额外 Hub 操作、凭证详情、完整详情视觉和若干状态仍待核；额外功能待用户审核；对应 UI-19）设置页**
  - 已实现：standalone Morde 的设置根页与 `settingsDetail` 路由已接入；按设计稿顺序显示三组共 9 个设置行，加唯一的全局执行控制行（共 10 行）。账号使用真实用户名，不仿造 HTML 示例套餐/使用率；账号、设备、安全凭证分别映射现有账号页、客户端安装页和同源 API Keys Portal；关于页读取应用名称/版本。五个没有真实 Morde 页面/合同的详情仍诚实显示“暂未实现”。不把旧 NanoMuse 设置页作为替代，不新增后端合同；预算信息不显示。
  - 待审核的占位交互：连接器没有 Morde MCP 管理页/合同（`/connections` 是 Model connections）；权限与审批规则没有全局合同（Host members、任务审批 inbox 均不等同）；通知偏好没有合同（现有通知收件箱不等于通知偏好）；外观没有 Morde 专属页面。HTML 点击这些行只显示提示，Morde 当前导航到占位详情。是否保留该页面跳转或改为提示需审核；答复前不新增能力、不伪造数据。
  - 急停恢复已接入但仍待审核：停止全部任务需经确认；服务端确认全局停用后，同一行改为“重新启用全部任务”，点击会直接调用 enable、没有第二次确认。现有 UI 用例覆盖停止确认及重新启用状态。保留为单行现状，不增加/删除入口，等待用户确认恢复动作。
  - 设备额外交互仍待审核：2026-11-08 `testMordeSettingsRoutesAndHonestUnavailableRows` 使用 `CODE_SIGNING_ALLOWED=YES CODE_SIGN_IDENTITY=-` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA13-settings-routes-adhoc-20261108.xcresult`）；签名关闭的首次执行停在本地登录 fixture，未到达页面断言，不计为产品失败。截图 `/tmp/MDA13-settings-routes-adhoc-attachments-20261108/04106E2F-0CBD-4A5F-BD98-8D9200A28ECA.png` 已检查：设备详情包含 Device relay URL 输入、Hub 在线状态、长段英文说明、远程打开按钮/确认框、客户端安装列表与撤销能力；HTML 点“设备”只弹“打开「设备」”。用例只在本地 Hub fixture 输入 `https://example.com`、打开二次确认后取消，未提交远程请求或撤销安装。确认是否保留/迁移 Hub relay 和设备详情；设备/安装加载失败的两个 Retry 与下拉刷新重复，待逐状态验收和用户决定；不因 Task 详情 Retry 的已审核删除决定而改动设置页 Retry。
  - 账号详情额外交互仍待审核：页面显示账号名称、ID、服务地址以及 Host members、Model connections、API Keys、客户端安装管理和退出入口；API Keys 也可从“安全凭证”行进入。截图能看到这些入口，但 UI 用例没有逐项打开 Portal 页面；确认这些入口保留、移入其他菜单或收敛前，不移除或迁移。
  - 旧 NanoMuse Memory、Storage 清理、Shared/Mount folders、Backup & Restore、Data controls、Logs 等不自动迁移到 Morde 设置。当前条目只限定 Morde 设置实现范围，不表示已决定删除旧入口或数据；若需改动旧入口，先单独审核其位置与影响，任何情况下不得删除或迁移其数据。
  - 验收：`testMordeDesignNavigationRoutes` 与 `testMordeSettingsRoutesAndHonestUnavailableRows` 分别验证根页 9 个设置入口 + 急停行、设置行位置、返回页头几何、账号详情部分内容、设备远程授权弹窗取消、五个占位详情和关于页可达（本地合成账号/Hub；结果 `/tmp/MDA13-chevron-alignment-20261108.xcresult`、`/tmp/MDA13-settings-route-recheck-20261108.xcresult`）。改后根页截图 `/tmp/MDA-three-pages-light-attachments-20261108/AC9DB136-2D34-4105-B1D0-5631564BA47D.png` 已与 HTML `set` 页面并排核对：三组/十行控件顺序、页头和四个底部 Tab 一致；最终箭头为深色 16pt semibold `›`，匹配 `.srow` 继承的深色文字。原记录 `/tmp/MDA-settings-root-visual-20261107/` 与账号/设备详情截图 `/tmp/MDA-settings-detail-audit3-attachments-20261107/` 仍作为详情证据。设备稳定态截图、凭证/API Keys 实际详情及设备撤销/刷新/错误状态尚缺，额外交互仍待审核，MDA-13 保持未完成。

- [ ] **MDA-14（进行中：目录浏览/返回、标题长按菜单上传/版本入口、本机副本安全删除及文件行菜单操作已有 Morde Simulator/单测；空目录/面包屑回退、未挂载只读副本、服务端能力关闭、403 权限拒绝及切换目录迟到分页隔离已复验；Workspace 分享已改为 reservation 直接归接收者、按 view/edit 建只读/可编辑副本并向接收者计配额；安全失败重试、未知状态恢复和旧已完成 transfer 数据迁移均通过本地 Postgres 验收，iOS 创建/领取 Simulator 2/2；升级前 claiming+target 链接兼容待用户决定；接收方深链系统确认/领取与短错误态下拉恢复已通过；2026-11-08 根页三行样例与 HTML 同序并有 Simulator 截图，文件修改差异行数因 manifest 合同缺字段仍显示真实大小；分享弹层按钮/选中态颜色、CSS 边距与行距已对齐，创建链接回归 1/1；其余状态和完整视觉核验未完成；对应 MIF-08）Workspace 浏览与分享弹层**
  - 修改：`MordeApp.swift` 的 `MordeWorkspaceFilesView`、manifest DTO/校验和本机 fixture。已改为以真实 `directories` 与 `files` 生成目录行，支持空目录和面包屑；文件操作收进文件行菜单；路径前缀输入改为目录导航，手动“读取更多”改为滚动触底加载。Workspace 名称取真实数据。文件行不再显示设计稿外的 SHA-256 字符串；上传/下载校验仍保留。不得使用示例数据或增加旧 manifest 兼容分支。已确认的文件级操作保留在文件行菜单。
  - 已确认并实施：用户决定主页面按设计稿收敛，移除常驻 Workspace 选择器/版本号、“文件能力”说明、上传及版本比较/恢复区域；上传与版本比较/恢复入口移至 Workspace 标题长按菜单，文件级操作留在文件行菜单。仅在传输进行中或失败时显示状态，不显示常驻状态区。移除常驻入口不删除云端文件或 API。Workspace 选择器此前用于切换本人 Workspace，版本操作用于提交检查点及按路径比较/恢复；这些操作仍可从标题长按菜单到达。用户已授权真实副本分享服务端/API，并确定：接收者必须登录；一个链接只允许一位接收者领取，未领取链接 7 天失效，领取成功后链接立即失效；系统新建归接收者所有的独立 Workspace 副本，权限设为只读或可编辑；领取后的副本永久有效，不随原 Workspace 后续变化、删除或分享方撤销而失效。禁止把 token 当作访问原 Workspace 的凭据；未接通真实合同前不得伪造链接或复制成功。
  - 验收：`testMordeResourceWorkspaceRouteBrowsesSyntheticManifest` 在独立 Morde target 使用进程内合成 manifest 浏览根目录 README 与 `docs/guide.md` 子目录并返回资源页；标题长按菜单入口也已通过，未传输真实文件。服务端 `WorkspaceService.ListWorkspaces` 使用 owner-scoped 列表，`isReadOnly: false` 有所有权依据。Morde 文件行菜单的“删除本机副本”现在只允许经 `unlinkat` 删除普通文件，遇到目录、符号链接或特殊文件拒绝删除；`MordeWorkspaceTransferTests` 新增普通文件删除、目录内容保留、符号链接目标保留断言并通过。Workspace 分享已接入真实服务端 API 与 iOS 创建、预览和领取独立副本流程；单链接单人领取，领取成功后立即失效。创建权限 UI 与接收方领取 UI 在 iPhone 18 Pro / iOS 27.0 同轮通过 2/2（`/tmp/MDA-workspace-share-deeplink-regression4-20261106.xcresult`）；`go test -count=1 ./internal/app ./internal/store/postgres -run 'TestWorkspaceShare|TestPostgresWorkspaceShare'` 通过，`go test ./internal/api -run '^$'` 编译通过，OpenAPI YAML 解析通过。接收方测试使用本地 DEBUG 深链夹具调用生产 URL handler，展示只读权限和独立副本说明，领取后新副本出现在接收方列表。系统识别 scheme 后的确认、唤起和领取已由 SpringBoard UI 测试闭环通过，详见 2026-11-07 记录。2026-11-07 主机票据/分享集成回归首轮 4/5，其中接收方深链页未出现；该用例单独重跑 1/1 通过，随后同一组 5 个用例串行复跑 5/5 通过（`/tmp/MDA-host-workspace-integrated-repeat/Logs/Test/Test-Morde-2026.11.07_02-27-35-+0800.xcresult`），首轮异常暂未复现。根目录、空目录和面包屑回退已通过合成数据核对；非法 `../escape.md` manifest 会被拒绝且不渲染文件行；文件行菜单下载/重命名/移动/删除云端确认也已有独立用例；独立只读 Workspace 中下载保持可用而云端重命名/移动/删除被禁用。Workspace 页面已按 HTML 调整为 38pt 返回/分享按钮、22pt 内容边距、真实挂载状态摘要、圆形文件/目录图标和平铺滚动行；移除“文件”章节标题及清单页脚，防覆盖说明改在下载确认中呈现。容量和用量来自 Workspace API 的 `quota_bytes`、`used_bytes` 字段；此前详情页遗漏这两项，已在 2026-11-08 补入挂载摘要并通过 Morde Simulator。布局源码已修改，最终 Simulator 根页截图已查看，HTML 与 Simulator 的并排视觉核验仍未完成；重复路径、文件/目录父子冲突和重复分页游标已补拒绝逻辑并通过 Morde 单测/UI 用例。普通分页迟到响应在切换目录后的隔离已于 2026-11-07 通过独立 Morde UI 用例 1/1（`/tmp/MDA14-late-manifest-page-retry2-20261107.xcresult`）；未挂载/其他权限/返回状态及 HTML 与最终 Simulator 截图并排视觉核验仍待验收。系统级深链和首次 503 后下拉恢复已通过，MDA-14 仍保持进行中。

- [ ] **MDA-15（进行中：按设计稿精简移动端云电脑控制页，无续租按钮/持有倒计时/长说明；控制状态错误时无额外“重试状态”按钮，关闭返回资源后重进会重新读取控制状态，合成 Simulator 通过 1/1；常驻浏览器、终端、文件，Workspace/主机文件/VS Code/窗口操作移入长按菜单；自有 Host 的文件入口打开绑定 Workspace 且可返回资源；页头截图入口图形与布局已按 HTML 对齐；截图预览黑屏已改为渲染当前 WebView 层级，Simulator 用例通过 1/1 且截图内容已检查；自有/共享资源进入、票据拒绝、权限不足、关闭返回与迟到票据不重开均有合成验收；接管态快捷入口和交还操作位置等待用户确认；原型截图写入 Workspace 的实现授权未确认；真实 Gateway/WebView 及逐控件视觉仍待验；对应 MIF-08）云电脑页**
-  - 修改：Morde 资源页 Host 行进入独立 `.computer(id, shared:)` 路由，自有/共享 Host 继续使用现有进入票据、observation ticket、HTTPS 同源导航及资源权限；关闭后返回资源页。原生页面对齐设计稿：左侧截图、居中控制状态和 Host/Workspace 副标题、右侧关闭，底部 Stop Task / 接管或交还控制；iOS Workbench 壳使用 62pt 导航栏、42pt 文件/浏览器图标、38pt 页头圆按钮、白色面板上/下留白 36/64pt 和 46pt 底部操作按钮。移动端隐藏续租、持有时间倒计时、版本信息、重复主机状态和长说明；保留控制安全租约和自动续期。常驻页签只留浏览器/终端/文件，主机文件映射为“文件”；Workspace、VS Code 移入“文件”长按菜单，键盘/窗口操作移入“浏览器”长按/菜单。截图由现有 WebView 在本机生成并预览；HTML 原型还会把截图写入绑定 Workspace，需先确认是否授权真实文件上传/API，答复前不伪造云端保存成功。Stop Task 只对当前会话里匹配 Host 的活动 Task 启用；多个任务由菜单选择，无匹配时禁用，复用既有 Morde Task 停止接口。云电脑分享仍使用 Host 行长按菜单及现有成员 API；不把 Task Runtime 工具当作 Morde 原生用户控制接口。
  - 验收：`testMordeComputerRouteAcceptsSyntheticTicketAndReturnsToResources` 使用本机 URLProtocol 覆盖自有 Host 进入票据和共享 Host 桌面 observation ticket，均进入 WebView 并关闭返回资源页。`testMordeHostLongPressSharesAndRevokesDesktopControl` 在独立 Morde Simulator 通过 1/1，证明 Host 行没有常驻分享按钮、长按进入分享、成员获得控制权限且能撤销；结果 `/tmp/MDA-host-share-verified2-20261106.xcresult`。新增 `testMordeComputerRouteKeepsHostTicketDenialOnResourceRow` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA-host-ticket-denied-readable-20261107.xcresult`）：合成 403 票据拒绝后不创建 WebView，错误显示在云电脑失败页，关闭后返回资源行。截图 `/tmp/MDA-host-ticket-denied-readable-attachments-20261107/5AB62337-5D7E-4D03-8645-355141E5DDCC.png`。2026-11-07 5 项主机/Workspace 分享串行回归 5/5（`/tmp/MDA-host-workspace-integrated-repeat/Logs/Test/Test-Morde-2026.11.07_02-27-35-+0800.xcresult`），包括票据进入/返回、WebView 失败、票据拒绝、分享创建和深链领取。新增 `testMordeComputerStopsTheActiveTaskBoundToItsHost` 使用合成 Host/会话 Task 通过 1/1（`/tmp/MDA15-host-task-stop-recheck-20261107.xcresult`）：云电脑按钮取消 Host 匹配的运行中 Task，打开同一 Task 详情确认显示“已取消”；截图 `/tmp/MDA15-host-task-stop-recheck-attachments-20261107/202E252F-7807-4734-B65E-6D90A8DFE3C8.png`、`/tmp/MDA15-host-task-stop-recheck-attachments-20261107/242F6B00-A951-4DF1-BA08-3C966E5327B1.png` 已检查。Workbench 手机菜单用例在 `web/workbench/src/app/Workbench.test.tsx` 中通过 2/2；内置浏览器以本机 Vite/MSW、390×844 打开 Workbench，确认常驻入口为浏览器/终端/文件，浏览器上下文菜单可见窗口操作，文件菜单可打开 Workspace，触摸长按组件用例确认不误切页。只使用本地模拟数据。截图对比暴露并修复了黑底错误文字颜色过暗的问题；当前主机名、页面副标题、连接中和错误状态均使用可读的浅色文本/指示器，错误状态在 Simulator 截图中可读。用例与截图仅用本机合成 API，没有连接真实 Host。共享 Host 权限不足与关闭时资源清理已通过本地合成验收；仍需确认当前 WebView 能否消费 Gateway 桌面/终端能力及完成逐页 HTML 对照。Agent Runtime 截图/输入工具不是此项真实用户操作的验收证据。
  - 验收：以本机替身覆盖只读、AI 控制、接管中、用户控制、租约过期、权限不足、操作失败和返回；并在有授权的本地 Host 环境中确认 ticket WebView 真正提供哪些页面操作，不触达真实用户 Host。逐控件记录是由 Gateway 现有能力提供，还是需新建 Morde 原生/Host 合同。不能将 HTML 终端输出、截图和提示当成真实执行。
  - 2026-11-07 源码审计补充：`/api/hosts/{id}/enter` 经 Gateway 根路径进入 `/__ch/wb/`；`web/workbench/src/app/Workbench.tsx` 与 `TopBar.tsx` 实现 noVNC、终端、Workspace/Host 文件、租约/接管、移动键盘及窗口快捷键。HTML 原型的 Browser/Terminal/Files 与这些功能并非完全同名同层级；Workbench 另有 Workspace、Host 文件、VS Code、续期、适应窗口、找回窗口、全屏、重连等设计稿外入口。现有 Morde 合成票据 UI 用例没有运行真实 Gateway Workbench。Workbench 没有截图保存到 Workspace 的流程，也没有直接停止当前 Task 的动作；Task agent `desktop.observe`/`desktop.input` 不是面向 iOS 用户的替代合同。未运行测试或连接 Host。

- [ ] **MDA-16（未开始；依赖 MDA-01～15 的逐页记录及对应待决项）跨页面最终回归**
  - 修改：更新本清单逐页记录；必要时调整 Morde 专属本地化及 UI/单元测试。确认各 MDA 页面都已完成本页视觉和交互验收后，在专用 Simulator 的 `Morde.xcodeproj` / `Morde` scheme 跑聊天→任务→事件链→审批→资源、工作项、文件库/详情、Workspace、云电脑、设置和两个占位页的跨页面流程。旧 `Minis.xcodeproj` / `morde` scheme 的通过记录不能代替本任务证据。
  - 验收：覆盖聊天→任务→事件链→审批→资源、工作项、文件库/详情、Workspace、云电脑、设置和两个占位页；检查浅色/深色、窄屏、键盘、安全区、Dynamic Type、VoiceOver、减少动态效果。复核已记录的对比度/Dynamic Type finding 和 `Invalid frame dimension`；记录证据及仍未解决的原因，不把部分测试或源码对照写成全页面通过。

## 页面执行记录

实施时按 MDA 编号追加：页面和设计稿位置、控件清点、当前差异、额外/移除功能及审核结果、实际修改文件、使用的本地替身、验收结果和仍待处理的能力。保留每次记录，状态变化以最新记录为准。

本页历史记录按当时状态保存；与当前进度冲突时，以本节最新记录、顶部“当前结论”和 MDA 任务状态为准。旧 target 的测试结果仅作为功能参考，不替代独立 Morde target 验收。

### 2026-11-07 · MDA-15 Host Gateway 与 Runtime 合同边界复核

- 检查 `server/openapi/portal.yaml`、`server/internal/app/host_sharing.go`、`server/internal/app/runtime_tool_executor.go` 与 `project/morde-runtime/contract/openapi.yaml`。Host 票据可进入 Gateway；Portal 票据可选桌面、终端、Workspace，Native observation ticket 只接受 desktop target。Host member 使用动态 `desktop:view` / `desktop:control` 等权限。
- Runtime operation schema 与 server Task tool executor 确实支持受 Task fence 约束的桌面截图、窗口列表、桌面点击/输入/按键及浏览器、shell/file 工具；这些是 AI Task agent 发起的操作，不是 iOS 用户会话的实时画面流、原生触控 API 或终端 UI，因此不能直接拿来认定设计稿按钮已具备对应能力。
- MDA-15 现存实现仍是 Host ticket → HTTPS 同源 `MordeHostView` 路由与合成票据验收。下一步需要确认已有 Gateway 页面嵌入 WebView 后实际提供哪些桌面/终端/文件操作，再按设计稿逐项标注由现存 Gateway 支持、确需新增 Host/API 合同，或当前不应显示。未连接真实 Host，也未运行测试；页面视觉和实际 Host 交互保持未验收。

### 2026-11-05 · MDA-00 页面基线

- 检查了 HTML 的 14 个 `data-view`（`chat/tasks/task/appr/chain/res/work/friends/lib/file/toolchat/settings/ws/pc`）及 Workspace 分享、工作项创建交互，并将设计稿控件顺序、当前 iOS 主要差异及审核边界写入上方表格。
- 核对 01～04 子清单唯一状态来源：MIF-03～05 与 MIF-12 仍进行中；MIF-06～10 已完成。没有把已有功能接入或旧模拟器记录当成本轮逐页验收。
- 更新总览 UI-07：合成 JPEG/PDF 准备测试 2/2 和服务端附件测试已通过，完整选择→上传→发送替身往返仍未验收。附件/API 详细状态仍以 01 子清单 MIF-02 为准。
- 影响：审批第三动作/历史、云手机与 AI 工具运行态、Workspace 分享链接和云电脑远程输入缺当前合同；按已审核规则记为待决，不用演示数据替代。

### 2026-11-05 · MDA-01 主导航与侧边抽屉

- 检查了 `NanoMuseShell.swift` / `NanoMuseShellChrome.swift` 的四个底部目的地、抽屉目的地次序、好友与 AI 工具占位、旧归档和系统文件入口；导航目的地已存在，无需改动应用视图代码。
- 修正 `MIFWorkItemAcceptanceUITests.swift` 中把聊天入口误认成 `drawer.chat`，以及未登录时要求审批数量、资源标题和任务样例数据的断言；改为验证实际稳定的路由控件、目的地和未登录状态下存在的页面标识。
- 在专用 `Codex MDA 01 Fresh 20261105`（iPhone 17 Pro / iOS 27.0）运行 `morde` UI 测试：四个底部入口、已审核抽屉入口及已移除入口、事件链/文件库/工作项直达、好友和 AI 工具占位，共 3/3 项通过。结果包：`/tmp/MDA-01-drawer-verified.xcresult`。
- 本页入口和路由功能用例已通过；控件位置与尺寸的本页截图对照尚未完成。内置浏览器拒绝本地 `file://` 设计稿地址，本轮没有尝试绕过；因此 MDA-01 保持待验收。

### 2026-11-05 · MDA-02 聊天页与附件（进行中）

- 将聊天输入框改为单行；常驻输入区仍只有发送按钮，附件、语音和 Task options 继续放在长按/菜单入口。
- 新增仅用于 DEBUG UI 验收的进程内附件 API 替身，覆盖个人同步快照、配额、文件库写入、分片上传及 SHA-256 校验、提交回执和消息附件 ID 校验。测试图片/PDF 在本机临时目录生成；请求由 `URLProtocol` 进程内截获，不连接真实服务、不发送文件。
- iPhone 17 Pro / iOS 27.0 上通过 3 项专用 UI 用例：图片 + PDF 的完整上传和四个附件对象发送；首次消息响应丢失后以同一幂等键重试；从长按菜单进入 iOS 文件选择器并取消；上传失败提示且不把未提交附件加入草稿。结果包：`/tmp/MDA-02-chat-retry.xcresult`、`/tmp/MDA-02-chat-picker-cancel.xcresult`、`/tmp/MDA-02-chat-upload-failure-2.xcresult`。
- 仍未验收：16 项上限与 PDF 展开项超限；跨账号完整文字草稿隔离（同账号会话切换下的文字、图片和 PDF 隔离已通过，账号切换附件隔离已有旧用例）；附件 chip 后续接入 MDA-12 的受权限保护预览。头像为显示/语音状态元素且没有资料页跳转；聊天 Task 摘要卡的详情跳转仍待用户审核，答复前不调整该入口。
- 聊天页本页位置与尺寸截图对照尚未完成；本轮没有完成浏览器截图对照，因此不宣称聊天页视觉已与 HTML 完全一致。

### 2026-11-05 · MDA-02 进度补充

- 新增 16 项上限 UI 用例并通过；与此前记录合计，以下 4 项专用 UI 用例通过：JPEG/PDF 上传及携带附件 ID 发送（含首次响应丢失后的同幂等键重试）、文件选择器取消、上传失败反馈、PDF 展开对象导致的 16 项上限拦截。新增结果包：`/tmp/MDA-02-chat-overflow.xcresult`；此前结果包见上条记录。
- 账号切换隔离用例尚未通过：A 账号登录前出现“当前安装所属账号已变化，请重新登录”错误弹窗，遮挡登录表单，导致测试停在夹具登录阶段。该结果不能证明附件串号或隔离正确，需先修复本地测试夹具的初始账号状态，再重跑 A→B 切换用例。
- MDA-02 继续保持进行中。未完成项：附件 chip 接入 MDA-12 权限保护预览；聊天 Task 摘要卡详情入口仍待用户审核；附件/键盘/滚动等其他状态及完整逐控件视觉核验未完成。头像为显示/状态元素，没有资料页入口。

### 2026-11-05 · MDA-02 账号隔离复验

- 在全新专用 iPhone 17 Pro / iOS 27.0 Simulator `Codex MDA 02 Account Clean 20261106` 重跑账号切换 UI 用例：A 账号读取其图片/PDF 待发送附件，登出并登录 B 后确认 B 的对话与草稿不含 A 的附件，1/1 通过；结果包 `/tmp/MDA-02-chat-account-switch-clean.xcresult`。该结果解决先前旧模拟器残留撤销提示导致的夹具登录失败。
- 整理聊天和文件库需使用的内置浏览器时，尝试在 Codex 内置浏览器打开本地设计稿被 URL 安全策略拒绝（仅允许 HTTP(S)）。未使用替代浏览器或其他方式绕过；浏览器对照仍未完成，原生交互继续用 Simulator 验收。
- MDA-02 仍进行中：会话切换草稿隔离、附件 chip 的 MDA-12 权限保护预览，以及两处待用户审核的额外交互尚未闭环。另有既有 `Invalid frame dimension` runtime warning，来源未定位。

### 2026-11-05 · MDA-03 任务列表进度

- `MIFTaskViews.swift` 移除任务行尾设计稿外的 chevron，整行详情导航保留；工作项分段在当前任务列表中已不存在，无额外删除。
- 修复设计稿“＋”当前发送 `newTask` 但外壳未处理的问题：现在创建新云端对话、切回聊天并聚焦输入框；通过 iPhone 17 Pro / iOS 27.0 专用 Simulator UI 用例 `testTaskPlusCreatesConversationAndFocusesComposer`，结果包 `/tmp/MDA-03-task-create-focus-retry.xcresult`。
- MDA-03 仍待专用 Simulator 回归：任务列表空态、失败重试、空中间页后的分页及下拉刷新需要完成检查；任务列表本页视觉截图核对也未完成。

### 2026-11-05 · MDA-03 测试夹具修正

- `MordeRegistrationUITests.swift` 的登录 helper、任务操作夹具和重启 helper 已改为可抛错并 fail-fast：登录表单缺失/不可操作、凭据未填入、登录未完成或错误弹窗出现时立即终止测试，不再继续点击登录后的页面。
- 任务列表回归用例已改为滚动触底触发分页，覆盖空中间页和迟到的跨账号分页响应；列表失败用例改为验证真实错误标识、下拉刷新恢复及不出现行内重试按钮。
- 本次只完成测试代码修正，尚未运行 Simulator；需使用全新专用 Simulator 复验。此前旧 Simulator 的“服务器安装撤销尚未确认”提示不应通过清除旧设备数据处理，也不能当作产品页面验收证据。

### 2026-11-05 · MDA-07 资源页开始实施

- 按已审核清单移除资源页的文件库/刷新常驻控件和默认目标展示卡，Host/Workspace 改整行点击，保留已批准的交还 AI 动作并隐藏未批准的接管/续租/单独刷新按钮。改动见 `MIFResourceViews.swift` 与 `MIFResourceViewsTests.swift`；子任务 generic iOS Debug build 和 `git diff --check` 通过，完整 UI 验收尚未运行。
- 设计稿“＋”目前只有提示效果，iOS 没有实际添加流程；服务端已有创建 Host/Workspace API，但实现向导会新增真实资源创建行为。本按钮按设计稿位置保留，行为等待用户选择，不伪造创建成功；旧 UI 测试仍需改为从抽屉进入文件库。

### 2026-11-05 · MDA-03 列表回归与 MDA-07 集成进度

- MDA-03 的 `testTaskPlusCreatesConversationAndFocusesComposer` 在 iPhone 17 Pro / iOS 27.0 Simulator 通过；验证“＋”创建新云端对话、切回聊天并聚焦输入框。任务页/资源页单元测试通过，四个根导航入口回归通过。
- 任务列表空态、失败重试、分页等 UI 用例没有完成有效验收：`signInToFixtureAccount` 未成功登录，页面实际停在“登录以查看 Morde 任务”，而 `launchTaskActionFixture` 在登录失败后仍继续测试。该失败属于测试夹具启动问题，既不能证明列表功能失败，也不能证明列表功能通过。下一步修复公共登录夹具并 fail-fast，再用干净 Simulator 重跑相关用例。
- MDA-07 的资源视图单元测试通过；相关旧 UI 断言已改为从抽屉进入文件库，并核对资源页存在设计稿“＋”、不再存在常驻文件库/刷新控件。Simulator 页面验收尚未完成。
- MDA-07 “＋”动作仍等待用户在已提交的选项中决定；收到决定前按钮保持不可执行，不显示虚构成功提示。

### 2026-11-05 · MDA-09 / MDA-10 占位页进度

- `MIFPlaceholderViews.swift` 已移除占位页重复标题和装饰图标，内容区只显示本地化“暂未实现”；标题、返回/菜单由外壳既有目的地导航提供。
- 好友及 AI 工具会话入口保持可达，尚未运行对应 Simulator UI 验收；MDA-09、MDA-10 继续为待验证，不因源码修改勾选完成。

### 2026-11-05 · MDA-13 设置页只读盘点

- 对照设计稿确认根页目标是 3 组共 10 行。Morde 目标现有账号、API Keys、客户端安装列表、通知、外观、关于等可映射页面；Morde MCP connectors、通用权限和审批规则没有可直接映射的原生设置页。`Model connections` 不能替代 MCP connectors，Host members 或审批收件箱也不能冒充通用权限/审批规则。
- Morde scheme 使用 `MINIS_CLOUD_ONLY`；旧 NanoMuse 本机 Connectors、Computers 和 Cloud task execution 根入口被条件隐藏。不能解除条件以补页面。全局急停已有真实 `MIFExecutionControlStore`，但需按设计行重新接入并统一互相冲突的可达性测试，同时保留确认、服务端结果和恢复操作。
- 已盘点 Soul/Avatar、Memory、Storage、Shared Folders、Mount External Folders、Backup & Restore、Data controls、Logs、Privacy、Feedback、Version/Update 的实际作用和候选替代入口。高影响本机能力的隐藏/迁移需用户审核；本次仅盘点，没有修改源码，MDA-13 未开始实施。

### 2026-11-06 · MDA-03 列表回归结果

- 在专用 iPhone 17 Pro / iOS 27.0 Simulator `Codex MDA 03 Fresh Run 20261107` 使用已签名的 `morde` scheme 运行 5 项 UI 用例。`testTaskPlusCreatesConversationAndFocusesComposer` 通过；4 项任务列表用例都在登录表单可操作性检查处失败（`The fixture sign-in form should be hittable before entering credentials.`），未进入列表页面断言。
- 结果包：`/tmp/MDA-03-regression-signed-20261107.xcresult`；日志：`/tmp/MDA-03-regression-signed-20261107.log`。该结果只确认“＋”流程通过，不能证明任务列表分页、空态或错误恢复通过。下一步先从测试活动/界面树查清登录夹具停留在哪个页面，再修正导航和夹具状态并重跑列表用例。
- 测试运行另有 `Invalid frame dimension (negative or non-finite)` runtime warning，尚未定位；作为独立问题保留，不据此判断列表功能结果。

### 2026-11-06 · MDA-03 干净设备回归诊断

- 在新建专用 iPhone Simulator 上，`testTaskListShowsConfirmedEmptyState` 通过；第二轮 3 项回归中，`testTaskListCanLoadMoreAfterAnEmptyIntermediatePage` 通过，失败恢复和跨账号迟到分页两项未通过。结果包：`/tmp/MDA-03-clean-empty-state-20261106.xcresult`、`/tmp/MDA-03-list-regression-clean-20261106.xcresult`；活动导出：`/tmp/MDA03-retry-activities.json`、`/tmp/MDA03-late-activities.json`。
- 失败恢复界面树确认 Task 页面和错误态已加载；同时 Chat 和 Task 两棵滚动视图都在可访问树中，测试使用 `app.scrollViews.firstMatch` 实际拖动的是 Chat transcript，故没有触发 Task `.refreshable`。下一步只给 Task 列表滚动容器加稳定标识并让测试对其下拉。
- 跨账号用例的活动日志显示公共退出辅助函数在已选 Task 页面仍额外切 Chat 并等待 `isSelected`；这不是被测页面要求。将从当前 Shell 抽屉进入设置/账号后退出，再复验登录夹具和迟到响应隔离。
- 本轮只通过任务空态、空中间页分页两个断言；刷新恢复、跨账号迟到分页和位置/按钮截图核对仍未验收，MDA-03 保持进行中。

### 2026-11-06 · MDA-06 审批页安全视觉调整

- 已移除标题栏额外刷新按钮，保留下拉刷新；审批行改为平铺分隔样式；事件链入口只显示在第一条待审批项；移除静态 HTTP 202 说明。批准/驳回、幂等键、版本冲突、未知结果及媒体权限逻辑未改，未添加无 API 支持的第三动作或处理历史。
- 新增首条审批/空列表事件链入口断言；改动在 `MIFApprovalViews.swift`、`MIFApprovalViewsTests.swift`。后续统一运行中的 `xcodebuild` 已报告 `MIFApprovalViewsTests` 全通过；UI 页面回归仍运行中。第三动作和历史是否扩 API 尚待用户回答，设计稿图文/状态字段差异及截图对照待验收，MDA-06 仍未完成。

### 2026-11-06 · MDA-11 文件库安全视觉调整

- 文件库筛选改为“全部 / AI 产物 / 我上传的”横向三段；“我上传的”只匹配具有有效附件 ID 的账号文件，避免把 metadata 误当可预览附件。通过原有权限/证明校验的本机文件和 Task artifact 改为整行进入预览；个人附件暂保留为不可点击元数据，待 MDA-12 接入安全详情页。
- 新增筛选与附件归类单测，调整两项文件库 UI 用例，并在 `Localizable.xcstrings` 为新增筛选词补齐 9 个 locale。静态 JSON/locale 集合检查通过；统一运行中的 `xcodebuild` 已报告 `MIFLibraryViewsTests` 全通过，UI 页面回归仍运行中。
- `IOS04CloudAcceptanceUITests.swift` 的旧 Cloud 控件/按钮断言已更新为新“全部”筛选与不可点击 metadata 行断言；该测试需要本机凭据，目前未能运行。MDA-11 保持页面验收待完成。

### 2026-11-06 · 统一 iOS 测试进度补充

- 运行中的 serial `xcodebuild` `/tmp/morde-ios-non-ai-full.xcresult` 已完成 `MIFTaskViewsTests`、`MIFWorkItemViewsTests`、`MIFApprovalViewsTests`、`MIFLibraryViewsTests`、`MIFResourceViewsTests` 与 `NanoMuseMordePersonalDataTests` 单测；截至本条记录，这些 suite 均通过。
- 这轮同一进程的 `MIFWorkItemAcceptanceUITests` 已通过：抽屉路由/已移除旧入口/好友与 AI 工具占位、四个底部 Tab、工作项筛选/创建/认领冲突/关联/交付等。`IOS04CloudAcceptanceUITests` 因缺少本机测试凭据被跳过。后续 `MordeRegistrationUITests` 仍在运行；其中有资源、文件库、审批和任务用例。
- 记录该条时 `MordeRegistrationUITests` 仍在运行；该运行后续以 `TEST INTERRUPTED` 结束（见下方最新进度），不能作为整轮回归通过证据。MDA-03 的 Task 列表滚动标识/退出 helper 和 MDA-08 工作项视觉编辑发生在该次 build 启动之后，不能用旧 build 代表这些新改动已验证。

### 2026-11-06 · MDA-07 资源视图补充

- `MIFResourceViews.swift` 已补齐设计稿的云手机、AI 工具分组和本地化真实空态；Host/Workspace 行调整为平铺呈现。`MIFResourceViewsTests.swift` 覆盖分组标题、空态及不伪造运行状态的断言；对应文案已加入 `Localizable.xcstrings` 各语言目录。
- 上述集成后的资源单元测试、Simulator 页面验收及位置/按钮截图对照尚未完成，MDA-07 继续为进行中；资源“＋”的动作仍待用户决定。没有运行状态合同前继续显示真实空态，不伪造云手机或 AI 工具数据。

### 2026-11-06 · 本轮实施清单状态整理

- 上一轮统一 serial `xcodebuild` 已结束为 `TEST INTERRUPTED`，结果包 `/tmp/morde-ios-non-ai-full.xcresult` 不完整，不能作为全量通过证据。中断前 `MIFTaskViewsTests`、`MIFWorkItemViewsTests`、`MIFApprovalViewsTests`、`MIFLibraryViewsTests`、`MIFResourceViewsTests`、`NanoMuseMordePersonalDataTests` 通过。`MIFWorkItemAcceptanceUITests` 为混合结果：抽屉路由测试未等到 Library 抽屉项出现；工作项交互测试仍断言已移除的刷新控件、旧筛选选择器和弹层外只读标识。相应旧断言已按新 UI 修改，但需重跑确认。`IOS04CloudAcceptanceUITests` 因缺本机凭据跳过；多项 `MordeRegistrationUITests` 在登录夹具表单可操作性检查失败，未进入页面行为断言。
- MDA-03 已在上述 build 之后为任务列表增加 `morde.tasks.list` accessibility id，并让下拉手势精确作用于 Task 列表；账号切换测试 helper 改为从当前 Shell 进入设置。尚未用这版源码重跑，刷新恢复和跨账号迟到分页仍待验证。
- MDA-08 已在 `MIFWorkItemViews.swift` 将页头刷新换为权限门控“＋”、保留下拉刷新；增加系统 sheet，保留描述/项目/会话字段和全部现有行操作；筛选改为分段控件、行改平铺；UI 用例增加权限门控及从“＋”创建。上述改动发生在旧 build 后，尚无 compile/Simulator 验收。待认领当前仍按未分配人筛选，与 HTML 示例按 `open` 状态筛选的差异需记录和审核。
- MDA-11 文件库单测在中断前通过；`IOS04CloudAcceptanceUITests.swift` 已更新为新“全部”筛选和非按钮 metadata 行断言，但需要本机凭据才能运行。文件库页面截图与视觉对照未完成。
- MDA-12 的 UI 和本地替身代码已写入 `MIFLibraryViews.swift`、`MordeApp.swift`、`MIFLibraryViewsTests.swift` 与 `MordeRegistrationUITests.swift`：个人图片/PDF 只读预览复核账号 scope、同步状态和附件元数据；聊天沿用原附件行打开同一预览 sheet；本机 URLProtocol fixture 提供成功、503、错误长度和错误 ETag 响应。预览页仅有返回、标题和内容，本机/Task 产物保留原 proof gate。尚未 compile 或运行测试；切号/退出/取消后的文件清理和预览返回路径仍待串行验证。
- 当前明确等待用户审核：资源页“＋”的动作；聊天 Task 摘要详情的额外交互；审批第三种动作/自动处理历史；工作项描述字段是否保留在弹层、移到其他入口或移除，及行操作是否迁移/移除；设置页旧功能的隐藏/迁移范围；云电脑缺失的远程能力。聊天头像按设计稿仅作显示/状态元素，没有资料页跳转；Workspace 分享 API 已获授权并接入，当前进入实现回归；各项以“接口与待决能力”表为决策列表，未获答复前只暂停对应入口变更，不阻塞无关页面。
- 失败的 Registration 测试活动/界面树显示登录页上方出现本机“请求失败 / 有服务器安装撤销尚未确认；本机已退出并保留撤销标识”提示，遮挡登录表单；这只说明合成登录夹具不能继续，不代表被测页面断言失败。后续将 Registration UI 用例与其他 UI suite 分开，使用新建专用 Simulator，并检查本地登出撤销响应是否被替身正确接管；不得清理或重置用户原有设备数据。
- 内置浏览器仍不能访问本地 `file://` 设计稿。本轮不通过启动本地服务器、切换浏览器或其他方式绕过；未能目视对照 HTML 的页面一律保留视觉验收待完成状态。后续按 MDA 编号逐页用专用 Simulator 对照并留存截图路径。

### 2026-11-06 · MDA-14 Workspace 浏览代码进度

- `MordeApp.swift` 中的 Workspace 浏览页、manifest DTO/校验和本机 fixture 已改：目录来自 manifest 的 `directories` 与 `files`，支持空目录；增加面包屑和目录切换；翻页改为滚动触底加载，并校验 Workspace、revision、path，防止旧请求覆盖当前目录；文件级操作收入行菜单。保留原权限、版本/并发及 SHA-256 校验。
- 本机合成 manifest fixture 已补齐根目录与子目录。静态检查确认旧 prefix 状态引用已移除；该修改尚未在独立 `Morde` scheme 构建或运行 Simulator 测试，因此 MDA-14 仍待验证。
- 未实现 Workspace 描述和分享链接操作：现有接口未提供相应数据/权限合同。Workspace 选择、上传、版本比较/恢复、传输状态仍保留；需要结合截图逐项核对额外可见入口和移除影响后再决定是否调整。

### 2026-11-06 · MDA-13 独立 Morde 设置入口审计

- `MordeStandaloneApp` 进入 `MordeEmbeddedView(layout: .full)` 后只显示对话、电脑、项目、通知四个 Tab；Morde target 未包含 `NanoMuseSettings.swift`。`MordeAccountSettingsView` 虽在 `MordeApp.swift` 定义，但其唯一调用处位于未编入 Morde target 的 NanoMuse 设置文件，因此当前独立 Morde 没有活动设置入口。
- 确认可映射能力：Morde 账号页、同源 API Keys Portal、Morde 客户端安装/撤销列表、stop-all/enable 全局云端执行控制；About 可使用应用元数据。不可混同的能力：Model connections ≠ MCP connectors；Host members ≠ 全局权限；任务审批 inbox ≠ 审批规则；通知收件箱 ≠ 通知偏好。设备页不默认带入 Hub relay。
- 连接器、全局权限、审批规则、通知偏好和 Morde 外观页缺可直接复用的独立页面或合同，已将 MDA-13 改为新建 Morde 原生入口并把缺失能力单独列为待决。旧 NanoMuse Memory/Storage/共享文件夹/备份/Data controls/Logs 等不是当前 Morde target 的活动功能，不自动迁移。
- 本次为只读审计；没有改源码或调用服务。MDA-13 的实现及 Simulator 验收仍未开始。

### 2026-11-06 · MDA-00 独立 Morde target 构建复核

- 再次运行 `xcodebuild -project android/src/ios/Morde.xcodeproj -scheme Morde -destination 'generic/platform=iOS Simulator' ... build`，仍以 `BUILD FAILED` 结束；完整日志在本机 `/tmp/morde-standalone-build-current.log`。
- 错误确认 `MordeApp.swift` 直接引用了不在 target 的 MIF/NanoMuse/Shared 类型。定义分别位于 `NanoMuseLegacySessionArchive.swift`、`MIFResourceViews.swift`、`MIFLibraryViews.swift`、`NanoMuseFeed.swift`、`NanoMuseShell.swift`、`Agent/Speech/SpeechRecognitionManager.swift` 与 `Shared/AppLocalization.swift`。本条只更新了实施清单，没有改 Xcode target 或源码。
- MDA-00 仍为进行中；接下来逐一决定哪些纯共享合同/组件可以安全纳入独立 Morde，哪些应改为 Morde 原生实现。不得将旧 NanoMuse 根导航接入为第二套活动逻辑；完成后重建并启动 Morde scheme。

### 2026-11-06 · 独立 Morde 导航与页面组件接线准备

- 新增 `Morde/MordeDesignNavigation.swift`：提供 Morde 四个主 Tab、drawer route/model、兼容 `MIFDrawerDestination` 与可由调用方注入真实会话/徽标的导航壳。新增的 `MordeDesignPageHeader` 复用 `NanoMuse/NanoMuseShellChrome.swift` 菜单页头。
- 任务、审批、资源、工作项、文件库页已增加可选 `onMenu` 页头入口；任务页“＋”可由 MordeRoot 注入新会话回调。原 MIF chrome 路径仍供旧 target 使用，但 Morde target 不纳入 `NanoMuse/NanoMuseShell.swift`，避免旧导航与兼容类型重复。
- 上述仅为组件准备；MordeRoot 的 route 接线、最小共享类型抽取、`Morde.xcodeproj` target membership、build 和 Simulator 验收仍未完成。MDA-00、MDA-01 继续未完成；不以代码已写或旧 target 测试结果替代新 target 验收。

### 2026-11-06 · MDA-00 独立 Morde build/launch 基线完成；MDA-01 路由差异确认

- 在独立 `Morde.xcodeproj` / `Morde` scheme 上重新执行 `xcodebuild -project android/src/ios/Morde.xcodeproj -scheme Morde -destination 'generic/platform=iOS Simulator' -derivedDataPath /tmp/morde-standalone-integration-derived CODE_SIGNING_ALLOWED=NO build`，结果 `BUILD SUCCEEDED`；`plutil -lint android/src/ios/Morde.xcodeproj/project.pbxproj` 结果 `OK`。
- 在专用 iOS 27.0 Simulator `Codex MDA 00 Morde Standalone 20261106`（UDID `29AFCF93-1AB6-4512-B178-BFC864217E5E`）安装并启动 `io.morde.app`。启动进程返回 PID `28754`；延迟 8 秒截图 `/tmp/morde-standalone-after-launch.png` 显示登录页。仅验证未登录首屏，不代表登录后路由或任一设计稿页面已通过。
- 源码核对确认 `MordeStandaloneApp` 入口为 `MordeEmbeddedView()`；`MordeDesignNavigationShell` 目前只有定义，没有被活动根页面调用；`MordeRoot.primaryTabs` 仍是对话、电脑、项目、通知。设计稿导航尚未进入可见主流程，故 MDA-01 和逐页任务仍未完成。
- build 输出仍有 `MordeApp.swift` 中多个 `ObservableObject` conformance 的 Swift actor-isolation warnings；当前 Swift 语言模式下不阻断 build，但进入 Swift 6 language mode 会成为错误，后续接线时应一并核对。
- 历史 2026-11-06 “构建复核失败”和“接线准备未完成”两条作为当时记录保留；本条是较新结果并更新其状态。此次只运行 build/lint 和启动检查，没有运行新的 UI 测试；Simulator 页面与 HTML 设计稿逐页目视对照仍待完成。

### 2026-11-06 · MDA-01 导航集成 build 成功；登录后验收被本地夹具阻断

- `Morde/MordeApp.swift` 已将 MordeRoot 接到设计稿 Shell；当前源码包含聊天、任务、审批、资源底部入口，以及抽屉中的事件链、工作项、好友占位、文件库、AI 工具会话占位和设置路由。任务、审批、资源、工作项、文件库及 Workspace 等页面组件已纳入独立 Morde target。用户已决定隐藏旧项目管理与通知收件箱入口，并保留其底层数据/API。
- 执行独立 `Morde.xcodeproj` / `Morde` scheme Simulator build，结果 `BUILD SUCCEEDED`；`plutil -lint android/src/ios/Morde.xcodeproj/project.pbxproj` 结果 `OK`。日志：`/tmp/morde-mda-active-integration-build.log`。
- 在干净 iOS 27.0 iPhone 18 Pro Simulator `Codex MDA Active Route Clean 20261106` 运行 `IOS03SimulatorAcceptanceUITests/testMordeDesignNavigationRoutes()`。测试停在登录页，界面显示“请求失败：未能完成操作。（NSURLErrorDomain 错误 -3003。）”；可访问性树只见登录页与“确定”按钮，未出现主导航标识。因此本次结果不能判定登录后路由成功或失败。结果包 `/tmp/MDA-01-route-clean-20261106.xcresult`，日志 `/tmp/MDA-01-route-clean-20261106.log`。
- 下一步先检查本地登录 URLProtocol/fixture 的请求拦截与网络会话配置，不连接真实 Portal；修好后只重跑路由用例，再继续逐页 Simulator 验收。不得清除用户数据来绕过登录问题。
- 尝试用内置浏览器打开本地 `Morde iOS设计稿.html` 被 `file://` URL 安全策略拒绝。本轮没有通过其他浏览器、本地服务器或代理绕过；所有页面位置、尺寸和按钮清点仍需后续逐页留存 Simulator 对照证据。

### 2026-11-06 · MDA-01 签名路由重跑及页面 TODO 汇总

- 使用启用 Simulator ad-hoc 签名的 `Morde.xcodeproj` / `Morde` scheme 重跑 `IOS03SimulatorAcceptanceUITests/testMordeDesignNavigationRoutes()`。本地合成登录成功，测试进入设计稿主页面并看到四个主 Tab；结果为 1 个 UI 测试、2 条断言失败，均指向菜单按钮预期标识 `morde.nav.menu` 不存在。当前菜单按钮可访问标签为“对话与设置”，系统图标标识为 `line.3.horizontal`。新结果包：`/tmp/MDA-01-route-signed-retry-20261106.xcresult`。因此旧 `-3003` 登录错误被此次结果取代；MDA-01 的下一项是给 Morde 活动菜单按钮补稳定无障碍标识并重跑，不增加可见控件。
- 任务/事件链组件新增了基于真实任务状态的节点标记规则及单元测试；Morde scheme 通用 build 成功。旧 `Minis.xcodeproj` 测试编译因新 Morde route 类型不在旧 target 而失败，不能当作本轮验收结果。MDA-03～06 的 Morde target 定向测试与 Simulator 页面回归仍待执行。
- 工作项组件已调整类型徽标、筛选/行间距并移除技术 revision 文本及重复提示，增加徽标断言；设计稿指定的“创建·团队可见”“一句话说清问题或需求”“认领”文案仍需本地化核对。描述/项目/会话字段及行操作仍按待审核表处理，答复前不隐藏或迁移。
- 资源与占位页本轮完成源码/路由状态审阅，未据此把未运行的 Simulator 页面验收标为完成；Workspace 文件操作继续放在行菜单，分享合同及 Workspace 级额外控件仍按待决表等待审核。
- 内置浏览器仍拒绝本地 `file://` 设计稿。未使用本地服务器或其他浏览器绕过；因此逐页视觉位置核对仍未完成，相关 MDA 任务保持未勾选。

### 2026-11-06 · MDA-01～02 Simulator 复验与聊天附件入口修复

- Morde 路由用例 `testMordeDesignNavigationRoutes` 已通过：四个 Tab、抽屉路由、好友/AI 工具占位、工具页返回以及设置入口均可达。长按菜单、Task options 和附件选择器用例 `testMordeChatLongPressActionsOpenTaskOptionsAndImporter` 也通过；证据位于 `/tmp/MDA-01-02-chat-fresh-retry-20261106.xcresult`。该结果包包含一个当时仍失败的附件用例，因此不代表整包通过。
- 修复附件草稿显示发送操作的问题：发送按钮现在将待发送附件计入内容状态，并在上传期间保持禁用、上传完成后启用；为 Morde standalone composer 预留 86pt 底部导航空间，避免被自定义 Tab 栏遮挡。图片/PDF 附件合成发送用例在干净 Simulator 独立通过 1/1，结果包 `/tmp/MDA-02-chat-attachment-final2-20261106.xcresult`。
- MDA-09/10 的打开、返回交互有上述路由用例证据；页面截图仍未与 HTML 对照，故只更新交互状态，不勾选任务完成。
- MDA-15 Host WebView 路由虽已接入并包含在 build 中，尚无安全的本地 Host 渲染夹具，未做 UI 验收；真实 Host 能力和设计稿中的截图、键盘/触控、终端、停止任务控件仍待确认合同。
- 内置浏览器拒绝本地 `file://` 设计稿。没有尝试绕过限制；逐页视觉对照和 MDA-16 跨页回归仍未完成。build 仍有非阻断的 Swift `ObservableObject` actor-isolation 警告，切换 Swift 6 语言模式前需处理或复核。

### 2026-11-06 · Morde UI 全套串行回归与设置详情路由

- 为设置详情使用 Morde 已有的 `selectedDesignRoute` 根路由：每个设置行切到对应详情 route，详情返回回到设置列表。清理聊天草稿时不再误重置整个导航；明确退出登录时仍按原逻辑返回聊天。没有增加或调整任何可见设置按钮。
- 修正设置 UI 用例等待抽屉切换完成后再点击设置行，避免过渡动画期间首击落在抽屉遮罩。独立设置用例曾 1/1 通过；等待逻辑加入后的完整 Morde UI 套件也通过。
- 使用专用 `Codex MDA Final 20261106`（iPhone 18 Pro / iOS 27.0）运行 `Morde.xcodeproj` / `Morde` scheme 全套 `MordeUITests`：6 项通过、0 项失败；真实凭据用例因本机未提供 IOS-03 凭据而跳过 1 项。通过项包括聊天长按/附件选择、Morde 导航/占位页、图片与 PDF 合成上传发送、急停/重新启用、合成 Workspace manifest 浏览以及设置五个占位详情/关于页返回。结果包：`/tmp/MDA-full-final-retry-20261106.xcresult`；测试日志：`/tmp/MDA-full-final-retry-20261106.log`。
- 另一次未加入动画等待的全套运行中，设置项首击未执行；失败快照显示页面仍停在设置列表。加入“行可点后再点击”后，全套串行重跑通过。记录此时序修正，不把第一次失败结果覆盖掉。
- MDA-01、MDA-02、MDA-09、MDA-10 的路径与交互证据更新；MDA-13 的五个占位详情和关于页、全局急停确认/恢复通过；MDA-14 的合成 Workspace 根目录/子目录浏览通过。其它页面状态、真实凭据登录流程和全页面截图仍未覆盖，因此相关 MDA 项保持未勾选。
- 编译仍有 `ObservableObject` actor-isolation 警告，Simulator 有 UIKit/SwiftUI 子视图层级警告；它们没有阻断本次 UI 套件。内置浏览器仍拒绝 `file://` HTML 设计稿；没有使用本地服务器、其他浏览器或代理绕过，视觉位置/尺寸/按钮对照仍待完成。

### 2026-11-06 · MDA-04/06/08 定向页面验收与无障碍标识修正

- 在独立 `Morde.xcodeproj` / `Morde` scheme、合成账号和专用 iPhone 18 Pro Simulator 上运行 Morde 定向页面 UI 用例。任务详情 `testMordeTaskDetailShowsRealStepsWithoutBudget` 与审批页 `testMordeApprovalInboxShowsOnlyConfirmedDecisions` 均通过：前者核对目标文本、真实步骤、暂停入口和预算字段不显示；后者核对标题待办数、正确审批项的批准/拒绝入口和无部分状态误标。结果包 `/tmp/MDA-page-smoke-fixed-derived/Logs/Test/Test-Morde-2026.11.06_05-47-26-+0800.xcresult`。
- 工作项 `testMordeWorkItemsRouteShowsDesignFiltersAndCreateForm` 先后发现了测试前置步骤和组件可访问性问题：需要先从项目选择器选中可编辑项目；`filterPicker`、创建表单和任务标题父级的无障碍标识会覆盖内部筛选按钮、文本字段或目标文本标识。删除未被调用的容器标识、清除任务标题父级标识并补上选项目步骤，没有改变可见布局。第一次定向运行的工作项用例因此失败；修正后独立复跑通过，验证四段筛选、页头“＋”、类型选择和标题字段。结果包 `/tmp/MDA-work-item-smoke-derived/Logs/Test/Test-Morde-2026.11.06_05-51-06-+0800.xcresult`。
- 变更位置：`NanoMuse/MIFTaskViews.swift`、`NanoMuse/MIFWorkItemViews.swift`、`MordeUITests/IOS03SimulatorAcceptanceUITests.swift`。审批 assertion 采用现有 task/request/version 复合 ID；没有改审批动作或业务合同。MDA-04/06/08 仅更新为“基础页面交互通过”，其他状态、项目权限/操作和逐页截图视觉核对仍未完成，三项保持未勾选。
- 定向运行产生的临时失败及其诊断记录保留在 `/tmp/MDA-page-smoke-fixed-derived/Logs/Test/Test-Morde-2026.11.06_05-47-26-+0800.xcresult`；其中任务详情与审批用例通过，只有当时尚未修正无障碍标识的工作项用例失败。旧 `Minis.xcodeproj` 测试仍不作为本轮验收依据。

### 2026-11-06 · MDA-03/05/06/07 本轮清单复核

- MDA-03/05：任务页真实步骤计数与预算步骤过滤、详情步骤顺序、事件流取消/scope/当前任务保护已在现有源码中；相关 Swift 文件通过 `swiftc -frontend -parse` 和静态断言。本轮未运行 Morde `xcodebuild`、Morde Simulator 或旧 `MinisTests`，因此这些仅是源码级复核，不更新完成状态。
- MDA-06：审批操作间距和驳回颜色按设计稿微调；Swift 语法解析通过，未运行 build/Simulator。复核发现 MordeRoot 把未知审批数映射成 `0`，以及首条事件链入口仍打开任务活动时间线；后续修正并验收这两个路由/状态差异。第三种决策及自动处理历史仍没有合同。
- MDA-07：资源分组、整行入口、加载/失败不冒充空态和无回调时禁用“＋”通过静态检查；没有 Morde build/Simulator 证据。资源适配器固定传 `isReadOnly: false`，需先确认当前 Workspace 权限合同能否提供真实只读状态；另核对未使用的 `openLibrary` 回调和聚合的加载/错误状态是否影响页面行为。
- 本轮没有取得 HTML 的内置浏览器可视预览；所有上述页面仍须按设计稿截图和 NanoMuse 视觉基线逐屏核对。任务详情的设计稿“急停这个任务”仍缺专用后端动作，不能改名复用暂停、取消或全局急停。

### 2026-11-06 · MDA-12 图片/PDF 预览定向验收

- 初次运行 `/tmp/MDA-library-preview-derived/Logs/Test/Test-Morde-2026.11.06_06-17-09-+0800.xcresult` 失败，快照显示 `NSURLErrorDomain错误-1011`。追到原因是发送成功后客户端同时读取合成任务 `q9999999` 的事件流，而聊天附件夹具未处理该 GET 路由，通用夹具回了 400，触发全局错误弹窗并遮住预览。
- 在进程内聊天附件夹具为 `GET /api/tasks/q9999999/events` 返回结束状态 `204`，随后重跑独立 `Morde.xcodeproj` / `Morde` scheme 的同一 UI 用例，通过 1/1。图片与 PDF 都从聊天附件行打开只读预览、隐藏分享/保存按钮并可返回；接着进入资源→Library→“我上传的”，图片打开预览后返回并保留筛选。结果包 `/tmp/MDA-library-preview-derived/Logs/Test/Test-Morde-2026.11.06_06-24-44-+0800.xcresult`。
- 验收使用合成图片/PDF、进程内 URLProtocol 替身及专用 iPhone 17 Pro Simulator；没有调用真实 Portal/Host 或传送用户文件。MDA-12 的账号切换/取消时临时文件清理、失败响应、缺权限状态及视觉对照仍待验收，保持进行中。

### 2026-11-06 · MDA-06 审批待办与事件链跳转

- Morde Root 的审批计数从 `Int` 改为 `Int?`，不再将 API 未确认状态折成“0 件”；导航徽标仅在拿到确认的正数时显示。审批卡“看事件链”按设计稿进入事件链页并携带当前审批任务 ID，不再进入任务详情活动时间线。
- 扩展 `testMordeApprovalInboxShowsOnlyConfirmedDecisions`，在合成审批 `q0000101` 上验证“看事件链 ›”文案，点击后进入显示同一任务标题的事件链页。独立 `Morde.xcodeproj` / `Morde` scheme 定向用例通过 1/1；结果包 `/tmp/MDA-approval-event-chain-derived/Logs/Test/Test-Morde-2026.11.06_06-33-54-+0800.xcresult`。未访问真实服务或用户数据。
- 审批部分数据/失败恢复、重复点击/未知响应、媒体审批权限、第三动作/自动处理历史和视觉截图仍未完成；MDA-06 保持进行中。

### 2026-11-06 · Morde 全套 UI 串行回归

- 运行独立 `Morde.xcodeproj` / `Morde` scheme 的全套 `MordeUITests`，将 IOS-03 真实登录的账号、密码和服务器地址环境变量显式置空，避免访问真实服务。10 项中 9 项通过、1 项凭据测试按预期跳过、0 项失败；合成账号和进程内 URLProtocol 夹具覆盖当前已纳入的 UI 页面/路径。结果包 `/tmp/MDA-full-alignment-derived/Logs/Test/Test-Morde-2026.11.06_06-35-52-+0800.xcresult`。
- 本套测试覆盖已实现交互，不覆盖所有错误/权限/账号切换状态，也不替代逐屏设计稿截图核对；现存 Swift `ObservableObject` actor 隔离编译警告及 Simulator 系统 accessibility 重复类警告未阻断测试。MDA 未完成项继续按各页面小节跟踪。
- 全套串行运行中的资源 Workspace 用例曾在登录字段交互前触发一个 `请求失败` 弹窗，自动化中断处理器将其关闭后继续通过；同一资源用例随后单项隔离复跑通过且未复现弹窗。请求内容未从通过用例结果中获取，暂按潜在 UI 测试顺序/凭据隔离问题跟踪，不计入功能通过证据。

### 2026-11-06 · MDA-03 Morde 任务列表回归与页头几何校正

- 对照 `docs/Morde iOS设计稿.html` 的 `tasks` 结构与 CSS，确认页头水平边距为 22pt、菜单和“＋”为 38pt，菜单至标题间距为 10pt。Simulator 的无障碍界面树显示旧 Morde 页头实际为左/右 12pt、菜单 40pt；已改 `MordeDesignChrome.swift`，并在空列表 UI 用例中加入坐标断言。
- 新增 Morde target 专用用例覆盖确认空态、空中间页自动续翻、短错误态下拉刷新。首轮普通 `swipeDown()` 未触发刷新；为支持 iOS 16.4+ 的短列表回弹增加可用性受限的原生 `.scrollBounceBehavior(.always)`，用长下拉操作复验刷新成功。最低部署版本仍为 iOS 16.0，没有引入更高版本 API 到 16.0～16.3。
- 定向 3 项 UI 用例通过；随后完整 Morde UI 套件 13 项中 12 项通过、IOS-03 凭据登录 1 项按预期跳过、0 项失败。完整结果 `/tmp/MDA-03-list-derived/Logs/Test/Test-Morde-2026.11.06_07-00-28-+0800.xcresult`，日志 `/tmp/MDA-03-full-suite.log`；定向结果 `/tmp/MDA-03-list-derived/Logs/Test/Test-Morde-2026.11.06_06-57-51-+0800.xcresult`。
- 验收仅用专用 Simulator、合成账号及进程内 URLProtocol；未连接真实 Portal/Host。MDA-03 仍待当前 Morde target 的跨账号迟到分页用例和逐页截图对照。内置浏览器拒绝 HTML 的本地 `file://` 地址；未换浏览器、起本地服务或代理绕过。16.0～16.3 的实际滚动刷新行为也未在本机 Simulator runtime 覆盖，需后续设备/运行时验收。

### 2026-11-06 · MDA-05 事件链空态与全量回归修正

- 新增 `testMordeEventChainShowsRealEmptyStateAndReturnsToChat` 并通过 1/1：从抽屉进入事件链，验证当前无任务时只显示真实空态、不显示任务标题/事件行，并能返回聊天。结果包 `/tmp/MDA-05-chain-derived/Logs/Test/Test-Morde-2026.11.06_07-10-00-+0800.xcresult`。
- 新增 `testMordeEventChainRecoversExpiredCursorAndDeduplicatesEvents`，使用独立合成审批任务夹具验证首次事件请求断线、快照游标过期、恢复读取序号 9 事件及重复序号去重；标题/恢复事件展示且不泄露预算字段，定向 Morde UI 用例通过 1/1。结果包 `/tmp/MDA-05-chain-derived/Logs/Test/Test-Morde-2026.11.06_07-44-42-+0800.xcresult`。这条用例不访问真实服务或用户数据。
- 加入事件链用例后的全套 Morde UI 运行共 14 项：12 项通过、IOS-03 凭据用例 1 项跳过，任务列表下拉刷新恢复 1 项失败。失败结果 `/tmp/MDA-05-chain-derived/Logs/Test/Test-Morde-2026.11.06_07-13-09-+0800.xcresult`；随后隔离复测同一刷新用例仍未恢复，结果 `/tmp/MDA-05-chain-derived/Logs/Test/Test-Morde-2026.11.06_07-24-33-+0800.xcresult`。曾尝试让短内容填满 ScrollView 视口，但未修复刷新触发，已撤回该布局实验。
- 因此此前 MDA-03 定向刷新用例的单次通过不视为稳定验收。保留 iOS 16.4+ `.scrollBounceBehavior(.always)`；最小 iOS 16.0～16.3 的设备行为仍未核验。事件链无任务空态/返回、关联任务断线恢复与事件去重已有验证；无历史、账号切换及视觉对照仍待完成。

### 2026-11-06 · Morde 最新全量 UI 回归

- 加入关联任务事件链恢复/去重用例后重跑独立 Morde `MordeUITests`，并显式取消 IOS-03 真实账号环境变量：共 15 项，13 项通过、1 项凭据测试按预期跳过、1 项任务列表错误态下拉刷新恢复失败。结果包 `/tmp/MDA-05-chain-derived/Logs/Test/Test-Morde-2026.11.06_07-46-45-+0800.xcresult`，日志 `/tmp/MDA-05-latest-full-suite.log`。两条事件链用例、列表空态/空页续翻以及其他原有页面用例通过。
- 失败仍集中在 `testMordeTaskListRecoversFromLoadFailureWithPullToRefresh` 的真实空态恢复断言；不把此前单独运行的通过记录为稳定验收。MDA-03 未完成；视觉对照、跨账号分页、真实凭据路径和 iOS 16.0～16.3 实机行为均未验收。

### 2026-11-06 · MDA-03 / MDA-08 / MDA-11 / MDA-12 更新

- MDA-03 的短错误空态刷新增加顶部下拉手势补偿，正常列表继续使用系统 `.refreshable`；统一刷新函数避免先清掉旧列表/错误状态，并阻止重复刷新。`testMordeTaskListRecoversFromLoadFailureWithPullToRefresh` 已通过，随后 Morde 全量 UI 套件 15 项中 14 项通过、1 项 IOS-03 真实凭据用例按预期跳过、0 项失败。结果包 `/tmp/MDA-refresh-fix-derived/Logs/Test/Test-Morde-2026.11.06_08-22-41-+0800.xcresult`；结果摘要为 `Passed`。该次验证使用 iPhone 18 Pro / iOS 27.0 Simulator，没有真实登录凭据。
- MDA-08 完成只读合同核对，没有修改工作项代码：当前工作项 DTO 不提供可靠的创建者/来源字段；“待认领”目前以 `assignee_id == nil` 定义，不能和 HTML 示例中的 `state=open` 混为一个状态。描述、项目/会话字段和现有行操作继续保留，相关展示差异仍待审核。
- MDA-11/12 为个人附件预览增加先呈现加载页、下载后更新同一 sheet 的流程；返回/关闭会取消加载，失败提示延至预览页关闭后显示。临时文件删除失败会登记路径并在后续清理中重试。全量 Morde UI 套件通过现有图片/PDF 预览路径；清理失败重试单测 `testTemporaryAttachmentFileIsRetainedAndRemovedOnRetry` 因旧测试 target 缺少独立 Morde 类型而未能启动，不作为 Morde UI 验收。
- 为核对清理失败重试而尝试运行上述旧 `MinisTests` 单测；构建在测试启动前因旧工程缺少 `MordeDesignTab` / `MordeDesignRoute` 等独立 Morde 类型而失败，xcresult 总用例数为 0。它没有验证或否定清理逻辑；不通过把新路由塞回旧工程来修复该验证目标。
- MDA-03 跨账号迟到分页隔离已在 Morde target 通过；仍需核验 iOS 16.0～16.3 刷新行为和逐页截图。MDA-08 创建提交、权限、行操作及视觉核对仍待完成；MDA-11/12 错误/权限、切号/取消清理及视觉核对仍待完成。内置浏览器仍拒绝本地 HTML `file://` 设计稿，未使用其他浏览器或本地服务绕过。

### 2026-11-06 · MDA-14 Workspace 权限来源与 MDA-15 Host 页面路由

- 只读核对 `server/internal/app/workspace.go`、`server/internal/api/resources.go` 和 Morde 适配器：`WorkspaceService.ListWorkspaces` 以 `principal.UserID` 调用 `ListWorkspacesByOwner`，`GET /api/workspaces` 不返回 `read_only` 字段；当前资源页只从该 owner-scoped 列表生成 Workspace 行，因此 `isReadOnly: false` 有账号所有权范围依据。共享主机是独立 `/api/shared-hosts` 数据，按 `desktop:view` 权限校验，不将 Host 观察权限推导成 Workspace 写权限。
- 为资源验收夹具增加单次短期票据响应，为 Host WebView 增加仅用于测试定位的 accessibility identifier；新 UI 用例验证点击资源主机行→接受同源票据并呈现 WebView→关闭返回资源页。独立 `Morde.xcodeproj` / `Morde` scheme 用例通过 1/1，结果 `/tmp/MDA-host-route-derived/Logs/Test/Test-Morde-2026.11.06_08-41-56-+0800.xcresult`。另运行 `Tests/run-host-navigation-tests.sh`，同源、HTTPS、票据目标、下载来源及安全落盘检查通过。
- 该 Host 验收使用 `resource-fixture.invalid` 合成地址，没有连接真实 Portal/Host；只证明当前安全 WebView 路由与返回，不证明截图、终端、触控或键盘等设计稿控件可用。票据失败/权限不足、WebView 加载失败/关闭生命周期和逐页视觉核对仍待完成。

### 2026-11-06 · MDA-01/02 抽屉分页、搜索及聊天页头几何复验

- 对照 `Morde iOS设计稿.html` 中 `.head.tall` 的 22pt 水平边距，修正聊天菜单从 16pt 到 22pt；抽屉搜索外层胶囊保留 38pt 高度并为外层尺寸核验增加无障碍标识，不新增可见控件。
- 修复旁聊末行先出现、会话分页游标后返回时漏加载下一页的状态衔接：末行出现时若还没有游标则暂存该会话 ID，游标到达后继续加载。同步修正本机 URLProtocol 夹具优先级，确保显式分页夹具不被基本主聊天夹具覆盖。
- 独立 `Morde.xcodeproj` / `Morde` scheme、合成账号、iPhone 18 Pro / iOS 27.0 Simulator 定向串行回归 4/4 通过，覆盖 `testMordeDesignNavigationRoutes`、`testMordeDrawerGeometrySearchAndConversationPagination`、`testMordeChatLongPressActionsOpenTaskOptionsAndImporter`、`testMordeApprovalInboxShowsOnlyConfirmedDecisions`。结果包 `/tmp/MDA-nav-approval-derived/Logs/Test/Test-Morde-2026.11.06_09-12-42-+0800.xcresult`；没有设置真实 IOS-03 凭据，也未访问真实 Portal/Host 或用户文件。
- MDA-01/02 继续保持进行中：真实会话/审批数据和各页完整按钮位置、尺寸、顺序的截图对照仍待完成。内置浏览器拒绝本地 `file://` HTML，尚未完成 HTML 与 Simulator 截图的逐页视觉比对。

### 2026-11-06 · MDA-04/07/08/11/12/13 集成修正与定向复验

- MDA-04：任务详情加载/重试固定使用捕获的 task ID，并在请求响应前后检查取消状态、账户 scope、task ID；任务或账户切换时清除澄清草稿、关闭取消确认并使旧操作 spinner 失效；任务操作与审批/澄清共用在途锁。Swift 语法解析通过，9 项定向静态断言通过。`testMordeTaskDetailShowsRealStepsWithoutBudget` 在本轮 Morde target 定向回归通过；任务切换/账号迟到响应等竞态尚无 UI 用例。
- MDA-07：云手机区把“无数据”文案改为已有本地化“执行状态不可用”，不暗示不存在的实时运行数据；资源“＋”无真实回调时仍禁用。同步更新旧 `MIFResourceViewsTests` 的文案断言，旧 `MinisTests` 本轮未运行且不作为 Morde 验收。
- MDA-08：工作项创建失败改在表单内展示真实错误，失败后保留 sheet 与输入。Morde target 的列表/筛选/创建表单基础用例通过；本轮未提交失败请求，错误回显尚未被 UI 用例覆盖。
- MDA-11/12：未登录状态显示登录提示；请求或同步不可用保持 unavailable 状态；Task 数据刷新保留个人附件预览。临时清理只允许临时目录内的普通文件，拒绝非 file URL、临时目录外路径、目录和符号链接；删除失败保留待重试 URL。Swift parse、7 项静态断言与格式检查通过。Morde 图片/PDF 用例覆盖聊天和文件库预览、返回并保留筛选；切号/取消清理、失败重试和错误/权限状态仍待 UI 验收。
- MDA-13：Morde target 共用返回页头由居中改为左对齐，返回按钮 38pt、左右页边距 22pt、标题间距 10pt；设置页已有 Morde UI 几何断言通过。没有修改可见控件数量。设备、账号和凭证的真实映射以及待审核设置行仍未验收。
- 集成回归使用独立 `Morde.xcodeproj` / `Morde` scheme、ad-hoc 签名、合成账号和 iPhone 18 Pro / iOS 27.0 Simulator。导航/设置 2 项全通过，结果 `/tmp/MDA-current-signed.xcresult`；任务详情、资源 Workspace、工作项、聊天菜单、Host WebView、图片/PDF 聊天与文件库预览 6 项全通过，结果 `/tmp/MDA-current-pages-signed.xcresult`。该批没有访问真实 Portal/Host 或真实文件。
- 一次未签名 Simulator 尝试出现 `NSURLErrorDomain -3003` 并被中断，不能视为 UI 产品失败；之后使用 ad-hoc 签名并完成上述 8 项通过。为避免依赖 Simulator 持久化的 `morde.server`，Morde UI 测试 helper 显式传入本地服务和注册替身参数。
- 最新通过结果仍不表示逐页视觉对齐：内置浏览器拒绝本地 `file://` 设计稿，尚无 HTML 与 Simulator 并排截图核对；本轮也未重跑全量 Morde UI suite。MDA-04、07、08、11～13 和其它页面维持未勾选。

### 2026-11-06 · MDA-03 跨账号分页与 MDA-14 Workspace 入口决策落实

- MDA-03：修正账号设置列表底部操作被自定义 Tab 栏遮挡的问题，为列表增加 86pt 底部安全区；测试等待抽屉关闭、进入账号页并滚动列表，使退出入口完整露出后再执行退出。`testMordeTaskListDropsLatePageAfterAccountSwitch` 在 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1，结果 `/tmp/MDA-account-switch-safe-area.xcresult`。用例验证 Account A 的任务第二页仍在途时切换到 Account B，A 的迟到私有任务不会出现在 B 的列表中。MDA-03 仍因逐页视觉及 iOS 16.0～16.3 刷新运行时未核验而保持进行中。
- MDA-14：按用户确认收敛 Workspace 主页面，上传和版本比较/恢复移到标题长按菜单，文件级操作保留在文件行菜单；常驻状态区改为只呈现传输中/失败。目录浏览、返回及新菜单路径用例通过，结果 `/tmp/MDA-turn-derived/Logs/Test/Test-Morde-2026.11.06_10-36-10-+0800.xcresult`，没有传输真实文件。分享合同、空/错误/权限状态和整页视觉仍待核验，MDA-14 保持进行中。
- 本轮未重跑完整 Morde UI suite。内置浏览器拒绝本地 HTML `file://` 设计稿，没有进行 HTML 与 Simulator 截图并排视觉核对；因此页面任务不因功能用例通过而勾选完成。

### 2026-11-06 · MDA-02 文字队列与 MDA-01/09 路由复验

- MDA-02：在活动 Task、会话、模型、Host、Workspace 与目标状态匹配时，沿用原发送按钮把纯文字追加到当前 Task。复用已有 POST `expected_task_id`、幂等与服务端 pending-message 投递合同，没有新增 API 路由或后端代码。`testMordeChatComposerQueuesFollowUpOnActiveTask` 在 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1，结果包 `/tmp/MDA-queue-followup-20261106.xcresult`；客户端凭据/幂等持久化、服务端 `AppendMessage` 和 worker 待投递单测通过。
- 运行中任务的附件工具集在任务启动时建立，追加消息投递合同只携带文本，因此图片/PDF 不会伪装为已排队，继续留在草稿中等待任务结束后发送。没有完整重跑 Morde UI 套件。
- MDA-01/09：好友页按已审核需求隐藏设计稿样例申请、好友列表、“＋”添加及行操作。testMordeDesignNavigationRoutes 在独立 Morde target、iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1，结果包 /tmp/MDA-friends-placeholder-20261106.xcresult；覆盖占位内容、好友操作控件不存在、AI 工具会话返回和设置入口。
- HTML 逐页截图对照仍未完成。内置浏览器拒绝本地 `file://`；不通过本地服务、代理或切换浏览器绕开该限制，因此所有 MDA 页面的视觉验收仍待完成。

### 2026-11-06 · MDA-08 工作项加载错误控件收敛及全量复验

- 按用户审核删除项目列表、共享会话列表、工作项列表失败态的 3 个 Retry 按钮，保留错误说明和 `.refreshable` 下拉刷新；刷新会重新读取项目，并在选中项目后更新详情、工作项和共享会话。清除仅供按钮重试使用的闭包、未调用参数及 `refreshWorkItems` 包装方法；未改变服务端请求和行操作。
- `testMordeWorkItemsRouteShowsDesignFiltersAndCreateForm` 与 `testMordeWorkItemCreateFailureKeepsFormAndInput` 在 iPhone 18 Pro / iOS 27.0 Simulator 通过 2/2，结果 `/tmp/MDA-workitem-retry-removal-20261106.xcresult`。全量 `MordeUITests` 通过 25 项，真实 IOS-03 凭据测试 1 项按预期跳过，0 项失败，结果 `/tmp/MDA-integration-after-retry-removal-20261106.xcresult`。没有专门模拟三种列表加载失败状态来断言 Retry 控件不存在；源码现无这些按钮，刷新仍接在主滚动区。
- 全量回归仍记录 `Invalid frame dimension (negative or non-finite)` 和 `_UIGravityWellEffectAnchorView` 子视图层级警告，来源为 `MordeStandaloneApp.swift`；编译存在 `ObservableObject` actor 隔离警告。逐页设计稿截图对照仍阻塞：内置浏览器拒绝 `file://`，本地 `AGENTS.md` 禁止把 HTML 发布/发送到外部；MDA-08 及其它 MDA 页均保持未勾选。

### 2026-11-06 · MDA-14 Workspace 文件行点击映射复核

- 对照 HTML `renderFiles()`：目录行点击进入子目录；文件行点击只显示临时“打开 <文件名>”提示，没有预览/下载实现。原型没有定义文件行菜单，但用户已审核保留 Workspace 文件操作菜单；Morde 继续把真实的本机核验预览、下载和云端文件操作放在该菜单中，不新增无实际结果的占位点击或假预览动作。MDA-14 的分享合同、状态边界和视觉验收仍按原 TODO 待完成。

### 2026-11-06 · Workspace 单人领取分享实现与验收

- 按用户确认落实：分享链接登录后领取；每条链接只允许一位接收者，未领取 7 天失效，成功领取后立即失效；副本归接收者所有，查看权限只读、编辑权限可写，后续与原 Workspace 独立。分享记录随原 Workspace 或接收者副本删除，不会阻止任一 Workspace 删除。
- 服务端新增创建、预览、领取 API；链接为 256 位随机 token，数据库只保存 SHA-256 摘要。领取通过事务锁定并把链接保留给首个领取账号；同一领取账号可重试未完成领取，其他账号无法接管。成功后状态进入 `claimed`。
- iOS 已接入权限选择、真实创建请求、复制 `morde://workspace-share` 深链、接收方预览和领取页面；URL scheme 已登记。`testMordeWorkspaceShareCreatesLinkWithSelectedPermission` 在 iPhone 18 Pro / iOS 27.0 通过 1/1，证明编辑权限请求和复制完成提示；结果 `/tmp/MDA-workspace-share-20261106/Logs/Test/Test-Morde-2026.11.06_15-24-30-+0800.xcresult`。`go test -count=1 ./internal/app ./internal/store/postgres -run 'TestWorkspaceShare|TestPostgresWorkspaceShare'` 通过，覆盖单领及并发占用；`go test ./internal/api -run '^$'` 编译通过，OpenAPI YAML 解析通过。
- 接收方领取 UI 尚无稳定 Simulator 证据：`XCUIApplication.open(URL)` 的冷启动无法在保留本地认证夹具的同时稳定呈现领取页，因此失败尝试未记为通过，也移除了该不可靠 UI 用例。需要再用外部 URL 打开并验收登录、权限摘要、领取、独立副本列表和重复打开失效；在此之前 MDA-14 保持进行中。未向外发送或上传任何文件。

### 2026-11-06 · Workspace 分享单人领取规则复核

- 用户再次明确：一个副本分享链接仅允许单人领取，领取成功后链接失效；沿用“登录领取、未领取 7 天过期、成功后获得永久独立副本”的已确认规则。
- 服务端测试补上创建时 7 天有效期断言，并验证成功领取后原接收者再次预览/领取也返回 not found。`go test -count=1 ./internal/app -run '^TestWorkspaceShare'` 与 `go test -count=1 ./internal/store/postgres -run '^TestPostgresWorkspaceShare'` 均通过；migration cascade 调整已随 Postgres 集成用例验证。MDA-14 仍因接收方深链 UI 与逐页视觉验收未完成而保持进行中。

### 2026-11-06 · MDA-02 活动 Task 附件队列合同审查

- 只读检查确认：现有 REST `attachment_ids` 可复用，上传和附件关联不缺公开接口；缺口在 Go pending message 查询/worker delivery frame 与 JS runtime frame schema、验证、去重及 followup，当前只传文字。运行时附件工具的 ID enum 在 Task 启动时固定，新消息附件无法被调用；执行器本身会再次校验账号和当前 Task 的附件授权。
- 解除客户端 guard 不能单独实现可用队列。完整图片/PDF 需要扩展 Go + JS 内部合同、将附件 ID 纳入投递和幂等比较、保持数量/类型/大小限制，并对未提交/过期/不可读附件显式失败；PDF 还需关联文本/页图拆分出的整组附件。已向用户询问是否授权扩展，答复前保持图片/PDF 草稿行为，继续无依赖页面。

### 2026-11-06 · MDA-02 活动 Task 图片/PDF 队列授权

- 用户授权扩展 Go worker 与 JS runtime 的投递合同，使图片/PDF 可在活动 Task 期间排队。独立工作包负责 Go pending message/worker frame 和 JS runtime 的字段校验、幂等去重、恢复及消息构造；主任务负责 iOS guard/发送入口与合成 UI 验收。
- 在端到端完成前，iOS 不得把响应丢失、未接受或附件校验失败显示为“已入队”；需验证失败附件拒绝、附件 ID 合并/去重、单条消息上限及运行中 Task 读取新附件仍限定到当前 Task/账号。

### 2026-11-06 · MDA-02 活动 Task 图片/PDF 队列接入中

- iOS 移除“活动 Task 只能排入文字”的客户端拒绝条件，并允许带附件草稿复用原发送位置；已有目标菜单选择当前运行 Task，正文和 attachment IDs 仍复用既有公开消息 API。初次 Simulator 运行发现 composer 没接入队列状态；修正后 `testMordeChatComposerQueuesFollowUpOnActiveTask` 与 `testMordeChatComposerQueuesImageAndPDFAttachmentsOnActiveTask` 分别 1/1 通过，结果包为 `/tmp/MDA-queue-text-regression-20261106.xcresult` 与 `/tmp/MDA-02-queue-attachments2-20261106.xcresult`。
- Go worker 已用 v2 私有帧传递已提交附件 IDs，消息签名同时包含正文和 IDs；Runtime 同时验证 v1 纯文字和 v2 附件 frame，checkpoint 存储标记 ID，合并 pending 消息前与 DB 上传状态复核；运行中个人 Server Task 获得通用 ID 阅读参数，broker 重新加载当前 Task 附件并校验授权。附件至多 16 项且唯一、排序和格式受限；JPEG、PDF 文本与页 JPEG 受读出限制。`go test -count=1 ./internal/app -run 'TestAgentCheckpoint|TestCloudDSHTaskAttachment'`、worker 定向测试、Postgres compile-only 通过；Runtime 完整用例 `node --test test/runtime.test.mjs` 67/67 通过。
- Postgres 实际集成因本机未配置 `CH_TEST_DATABASE_URL` 暂未运行。页面仍因账号/会话草稿隔离、容量边界、错误和幂等重试、权限预览、已审核额外交互及设计稿截图对照而保持进行中，不把合成测试标记为真实服务通过。

### 2026-11-06 · MDA-02 图片/PDF 队列合同复验

- 按用户授权复跑 Go app checkpoint/附件读取定向测试、Go worker 队列投递/恢复测试、Postgres 包编译检查和 JS runtime 全套测试：app、worker 均通过；Postgres compile-only 通过；`node --test test/runtime.test.mjs` 67/67 通过。
- `CH_TEST_DATABASE_URL` 未配置，因此本次未运行数据库集成；附件提交、pending 消息关系与 checkpoint 幂等仍缺本地 PostgreSQL 实测证据。

### 2026-11-06 · MDA-02 活动 Task 附件队列重试复验

- 补跑 `testMordeChatComposerKeepsQueuedAttachmentsAfterLostResponseForRetry`，iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1，结果 `/tmp/MDA-02-queue-retry2-20261106.xcresult`。本地夹具在首次请求已记录接受后返回 503；UI 验证失败告警不会把消息呈现为已确认、图片/PDF chip 保留，重试携带同一幂等键后服务端夹具返回同一条已接受消息。
- 活动 Task 图片/PDF 入队的实现和本地合成重试验收已完成；真实 Postgres 集成测试未运行（`CH_TEST_DATABASE_URL` 未设置），故不声称数据库/部署环境集成通过。本记录之后，草稿隔离及 fail-closed 预览边界已有 2026-11-07 UI 证据；MDA-02 仍因 16 项上限 UI 边界、已审核额外交互和逐页视觉核验未完成而保持进行中。临时附件清理失败重试属于 MDA-11/12，已有后续用例覆盖，见该页执行记录。此记录更新并取代本节先前“附件仍留在草稿”的阶段结论。

### 2026-11-06 · MDA-14 接收方领取深链 UI 复验

- 新增 `testMordeWorkspaceShareDeepLinkClaimsPermanentIndependentCopy`，独立 Morde target / iPhone 18 Pro / iOS 27.0 Simulator 与分享创建用例同轮通过 2/2，结果 `/tmp/MDA-workspace-share-deeplink-regression4-20261106.xcresult`。合成夹具从 Morde URL handler 打开领取页，验证只读权限、领取后的独立副本说明、领取动作和接收者 Workspace 列表出现新副本。
- `simctl openurl` 在 iOS 显示“在 Morde 中打开”系统确认，证明已登记的 URL scheme 被识别；直接用 XCTest `XCUIApplication.open(URL)` 会重启 App 并丢失 URLProtocol 进程内夹具，不能用于这组本地 API 验收。系统确认后的 OS 唤起暂未完成 UI 自动化验收；MDA-14 仍因剩余目录/权限/错误状态、文件行操作和视觉对照未完成而保持进行中。

### 2026-11-06 · 内置浏览器本机原型走查

- 内置浏览器直接打开 `file://` 受限后，按用户此前“可以自己搞一个预览”的授权，在只绑定 `127.0.0.1:8765` 的临时静态服务中打开原始 HTML；没有使用公共 HTTPS、代理或向外发送设计稿。浏览器进程被中断时本地服务也随进程结束，重启后预览可继续访问。
- 在浏览器走查抽屉、搜索、底部 Tab、聊天输入→任务列表→任务详情、审批→事件链、资源→Workspace→分享权限选择/目录/文件行、云电脑接管/终端输入、工作项筛选/空标题校验、好友、AI 工具会话、文件库筛选/聊天附件详情、设置返回。HTML 中的消息发送、任务创建、终端命令和电脑控制均由前端 JS 模拟；不访问 Morde API/Host，也不代表产品功能通过。已完成设计稿原型行为检查，HTML 与 Morde Simulator 的截图/位置比对仍待执行，所有对应 MDA 任务继续保持未勾选。
### 2026-11-06 · MDA-14 Workspace 文件行菜单回归

- 新增 `testMordeWorkspaceFileActionsStayInRowMenu`，在独立 Morde target 的 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1，结果 `/tmp/MDA-workspace-row-menu-final-20261106.xcresult`。
- 用例确认“下载并校验 / 重命名 / 移动 / 删除云端文件”没有常驻行按钮，只有展开 `README.md` 操作菜单后才可见；重命名和移动进入输入确认，删除出现新检查点确认。用例未执行文件传输或删除。
- iOS 27 的确认弹层取消控件在 UI 自动化树中没有可匹配文本；本用例停在安全的确认步骤后结束，不将结果扩大为取消交互或真实云端文件操作验收。非法/重复 manifest、权限及错误状态、分页竞态和逐页视觉核对仍待检查，MDA-14 保持进行中。
### 2026-11-06 · MDA-14 非法 Workspace manifest 拒绝

- 新增本地 fixture 返回 `../escape.md` 路径，并新增 `testMordeWorkspaceRejectsUnsafeManifestPaths`；独立 Morde target 的 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1，结果 `/tmp/MDA-workspace-invalid-manifest-20261106.xcresult`。
- 验证客户端显示文件清单错误、不呈现不安全路径或文件菜单、保持返回资源入口且不显示 Retry 控件；未连接 Portal 或传输文件。重复/冲突路径的服务端页面状态、权限/失败细节和视觉对照仍待核验，MDA-14 保持进行中。
### 2026-11-06 · MDA-14 Workspace manifest 和只读权限验收

- `testMordeWorkspaceRejectsUnsafeManifestPaths` 通过 1/1（`/tmp/MDA-workspace-invalid-manifest-20261106.xcresult`）：进程内替身返回 `../escape.md` 时页面展示清单错误，不渲染危险文件或其菜单，且没有独立 Retry 控件。
- `testMordeReadOnlyWorkspaceDisablesCloudMutations` 通过 1/1（`/tmp/MDA-workspace-readonly-guard-20261106.xcresult`）：合成只读 Workspace 中“下载并校验”可用，云端重命名、移动、删除均禁用。两项使用合成账号/manifest，没有连接真实 Portal 或传输文件。

### 2026-11-06 · MDA-14 Workspace 分享流程复验

- 单独复跑领取用例时，App 停留在聊天页，未展示领取界面；该次不作为通过。Simulator 重启后，把分享创建和领取用例放在同一轮串行执行，`testMordeWorkspaceShareCreatesLinkWithSelectedPermission` 与 `testMordeWorkspaceShareDeepLinkClaimsPermanentIndependentCopy` 通过 2/2，结果 `/tmp/MDA-workspace-share-after-fixtures-20261106.xcresult`。领取后 Workspace 列表出现新副本。
- 该复验只使用本地 API fixture；系统级 `morde://` 外部唤起和 manifest 不安全路径、文件/目录冲突、重复分页游标已在 2026-11-07 后续 Simulator 复验通过（见文末）；Workspace 其余权限/错误状态及逐页视觉对照仍待验收，MDA-14 保持进行中。

### 2026-11-06 · MDA-14 Workspace 主页面视觉收敛

- 对照 `docs/Morde iOS设计稿.html` 的 Workspace 页面，把 iOS 主列表由分组 `List` 改为平铺 `ScrollView`，使用 22pt 内容边距、38pt 返回/分享按钮、圆形文件/文件夹图标及 13pt 行距；移除“文件”章节标题和清单页脚。保留用户审核过的文件行操作菜单、标题长按上传/历史菜单及传输期间状态。
- 页面挂载摘要读取真实 Host binding 与同步状态；当前 API 不提供容量/使用率，不展示 HTML 样例中的虚构容量。下载的本机写入范围和同名文件不覆盖说明移入确认弹窗。
- 保留下拉刷新并将 `.refreshable` 直接挂到新的 Workspace `ScrollView`。
- 更新既有 Workspace UI 验收定位器以匹配 `ScrollView`。独立 Morde target 的目录浏览/路由用例在 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA-workspace-layout-route-20261106.xcresult`）；最终布局的文件行菜单用例通过 1/1（`/tmp/MDA-workspace-layout-final-20261106.xcresult`）。已查看最终 Simulator 截图 `/tmp/MDA-workspace-layout-final-attachments-20261106/4A27EAE4-3988-46E6-9852-2DCCE05FA3DC.png`：挂载摘要、路径、平铺行及底部导航可见，卡片分组、章节标题与页脚已消失。`swiftc -frontend -parse`、独立 Morde `xcodebuild build` 和 `git diff --check` 通过；build 仍有既存 `ObservableObject` actor-isolation 警告。仍未完成全部 Workspace 状态与剩余页面的逐项视觉验收，MDA-14 继续保持进行中。


### 2026-11-06 · MDA-14 Workspace manifest 冲突与游标保护

- 新增 manifest 路径结构校验：大小写折叠后的重复路径会被拒绝，文件路径不能作为另一条文件或目录路径的父节点；根目录 `.` 仅作为目录标记处理。安全 `../escape.md` 原有拒绝行为保持不变。
- manifest 列表采用 `LazyVStack`，避免未显示的底部哨兵提前请求后续页。记录已请求的 cursor；空 cursor、重复 cursor 和循环 cursor 会停止追加，显示现有清单错误状态并移除卡住的分页 spinner，下拉刷新可重新开始。
- `run-morde-workspace-transfer-tests.sh` 通过；Morde iPhone 18 Pro / iOS 27.0 Simulator 的 Workspace 目录浏览、文件/目录路径冲突拒绝、重复游标停止三项通过 3/3（`/tmp/MDA-workspace-manifest-final.xcresult`）。用例使用进程内 API fixture，未连接真实 Portal/Host 或传输文件。Swift parse 与父/嵌套仓库 `git diff --check` 通过。
- MDA-14 仍未完成：路径冲突与 cursor loop 已覆盖；普通分页竞态/迟到响应、未挂载/权限/错误/返回状态、系统级 `morde://` 确认和 HTML 与最终 Simulator 截图并排核验仍需完成。

### 2026-11-06 · MDA-06 审批拒绝动作验收

- 新增 `testMordeApprovalRejectSubmitsAndRemovesPendingActions`，在 iPhone 18 Pro / iOS 27.0 Simulator 使用合成 task-pages fixture 通过 1/1，结果 `/tmp/MDA-approval-reject-results-20261106.xcresult`。点击“拒绝”后页面展示已提交状态，并移除批准/拒绝两个待决动作。
- 此项覆盖拒绝按钮的 UI 提交与确认态，不代表真实 Portal 决策已验收；MDA-06 仍需加载失败、版本冲突、重复点击竞态及逐页视觉核对。

### 2026-11-06 · MDA-08 工作项筛选、权限与创建路径复验

- `testMordeWorkItemsRouteShowsDesignFiltersAndCreateForm` 在只读项目和可编辑项目之间切换：viewer 项目的创建“＋”禁用，owner 项目启用；缺陷、需求和未认领筛选分别显示匹配行并隐藏不匹配行。iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1，结果 `/tmp/MDA-workitem-permission-filter-final-20261106.xcresult`。
- 新增 `testMordeWorkItemCreateSubmitsToSelectedProject`，合成 POST 创建需求项，确认标题/描述保留到选定项目列表且成功后表单关闭，通过 1/1，结果 `/tmp/MDA-workitem-create-success-20261106.xcresult`。此前 `testMordeWorkItemCreateFailureKeepsFormAndInput` 已覆盖失败后保留表单和标题；这些都是进程内 API fixture，不代表真实 Portal 创建或权限服务已联调。
- MDA-08 的筛选、项目门控和创建表单成功/失败已有 Morde target 合成验收；真实 Portal、剩余行操作、错误/冲突状态和逐页截图仍待完成，页面任务保持未勾选。

### 2026-11-06 · MDA-07 资源 Host 状态与刷新恢复验收

- `testMordeResourceHostLifecycleStatesMatchServer` 与 `testMordeResourcePermissionMessageRequiresRunningSharedHost` 在 iPhone 18 Pro / iOS 27.0 Simulator 同轮通过 2/2，结果 `/tmp/MDA-resource-host-states-20261106.xcresult`；`testMordeResourcePreferenceFailureKeepsConfirmedEmptySections` 通过 1/1，结果 `/tmp/MDA-resource-refresh-empty-20261106.xcresult`。均使用本地合成 API：Host 生命周期状态与权限提示对应服务端字段；资源列表确认为空时显示空态，偏好接口失败不被混作列表加载错误。
- `testMordeResourcePullToRefreshRecoversAfterListFailure` 修复前多次未通过：刷新请求已返回成功，但 SwiftUI refresh task 在资源状态更新期间取消，导致数据未应用。此前失败包仅作历史记录；根因修复后于 2026-11-07 在 Morde Simulator 复验通过，见下节。

### 2026-11-06 · MDA-08 工作项筛选结果验收

- 扩展 `testMordeWorkItemsRouteShowsDesignFiltersAndCreateForm`，在 Morde target 中使用合成项目数据检查“全部 / 缺陷 / 需求 / 未认领”筛选后的实际行集合：缺陷筛选只显示缺陷项，需求筛选只显示需求项，未认领筛选显示 assignee 为空的未分类项；用例在 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1，结果 `/tmp/MDA-workitem-filter-results-20261106.xcresult`。
- 此项只验证现有客户端筛选与合成数据，不改变“未认领”定义（仍为 `assignee_id == nil`），不覆盖真实创建提交、项目权限、行操作或视觉对照；MDA-08 继续保持进行中。

### 2026-11-07 · MDA-07 资源刷新与活动 Task 附件队列复验

- 资源页修复下拉刷新取消问题：列表状态更新会使 SwiftUI 的 refresh task 被取消，结果是接口成功但数据没有应用。独立 Task 保持刷新工作，`refreshResourceLists()` 等待现有全局加载释放后再发起刷新。`testMordeResourcePullToRefreshRecoversAfterListFailure` 在 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA-resource-refresh-clean-final-20261107.xcresult`）。
- 根据用户对图片/PDF 入队的明确授权复验活动 Task 消息：Morde UI 入队及丢失响应同键重试 2/2（`/tmp/MDA-02-attachments-approved-regression-20261107.xcresult`）；Go app/worker 定向测试和 Postgres 包 compile-only 通过；JS Runtime 68/68 通过。真实 PostgreSQL 集成因本机无测试数据库配置未运行。

### 2026-11-07 · MDA-08 工作项界面源码盘点

- 源码确认项目列表、共享会话列表、工作项列表的错误卡片均无独立 Retry 按钮；滚动区 `.refreshable` 调用 `store.load()`，沿用用户已审核的下拉刷新方案。未新增或运行测试。
- 核对本地化后，创建按钮显示“创建·团队可见”、标题提示为“一句话说清问题或需求”、认领按钮为“认领”。
- 按设计稿盘点待审核项：项目选择器用于选择真实 project ID；顶部 Task 区含会话/Task 选择与 Task 摘要；创建弹层还多可选描述；每行除设计稿中的认领外，还有关联 Task、记录 Workspace 版本、标记阻塞/解除阻塞。它们的作用、风险及收敛建议已加入“工作项范围、创建字段与行操作”待决表。当前 API 没有可靠的创建者/来源字段；`blocked` 且未分配时 iOS 仍显示认领，HTML 样例只对 `open` 显示；上述内容交用户审核前不改动。MDA-08 仍未完成，等待审核、错误态专项 UI 证据及逐页视觉核对。

### 2026-11-07 · MDA-07 Host 错误态源码盘点

- 资源页下拉刷新会重载 Host/共享 Host/Workspace、默认 Host 偏好及自有 Host 控制状态。列表/控制错误以文本呈现且没有独立 Retry；Host 打开失败也在对应行显示，用户再次点该行可重试，成功后错误清除。无需为这些恢复路径新增按钮。
- Host 行还会显示操作者、控制租约及到期时间、交接摘要和状态错误；HTML 样例只显示名称、Workspace/容量摘要与运行状态。控制权状态对“交还给 AI”安全门控有作用，具体保留哪些文本、是否收进 Host 详情待逐屏视觉审核；本轮未改动源码或运行测试。

### 2026-11-07 · MDA-07 本地回环 Workspace 状态验收

- 为确保合成验收即使 URLProtocol 拦截失效也不会访问真实 Portal，`launchLocalChat` 将测试服务地址设为 `https://127.0.0.1:49713`；资源、工作项及附件 fixture 辅助响应也改为只匹配该回环主机，正式默认服务器地址未改。
- 首次运行因资源 fixture 辅助响应仍匹配旧 Portal 主机名，资源列表没有合成行，用例在资源 Workspace 按钮断言失败（`/tmp/MDA-07-loopback-workspace-test/Logs/Test/Test-Morde-2026.11.07_12-40-20-+0800.xcresult`）。修正 fixture 主机匹配后，同一 Morde scheme 用例在 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA-07-loopback-workspace-test/Logs/Test/Test-Morde-2026.11.07_12-44-37-+0800.xcresult`）。
- 截图 `/tmp/MDA-07-loopback-workspace-test-attachments/BC0342C8-DAF9-4C7A-9B29-04E46C56449D.png` 证明已挂载 Workspace 显示 Host 名称、同步中显示绑定状态、未挂载 Workspace 不伪造 Host 名称；画面为资源页滚动中的合成多状态列表。MDA-07 仍未完成：Host 行额外控制信息的审核、资源页与 HTML 的整页视觉核对及其余状态仍待处理。

### 2026-11-07 · MDA-07/08 首轮页面对照与工作项回归

- Morde iPhone 18 Pro / iOS 27.0 Simulator 定向用例 5/5 通过，结果 `/tmp/MDA-browser-logic-validation-20261107.xcresult`：导航占位、工作项筛选、创建成功、创建失败保留输入、资源下拉刷新恢复。工作项列表/筛选/创建弹层用例另复跑 1/1 通过，结果 `/tmp/MDA-workitems-visual-capture-20261107.xcresult`。Device Hub 可见窗口显示普通账号的无项目空态，结果包也未包含合成项目列表/弹层截图；不将合成用例记为视觉验收。
- 对照本地 HTML 与可见 Simulator：MDA-07 的菜单、标题、“＋”及云端主机/云手机/Workspace/AI 工具四组次序相同；真实 Host/Workspace、云手机与 AI 工具只呈现实际数据或空/不可用状态。Host 操作者、租约/到期、交接说明和错误信息比设计稿多，功能作用、收敛建议及待审核状态已加入待决表，未隐藏安全状态。
- MDA-08 HTML 原型展示样例工作项、筛选和认领按钮；当前可见 Morde 账号未加入项目，所以只显示真实空态且“＋”禁用。合成筛选/权限/创建路径有功能证据，但当前缺少可复用的合成列表截图；项目选择器、Task/会话上下文、可选描述及额外行操作的审核结论未收到，保持现状并继续待决。
- 用户本轮明确授权运行中 Task 的图片/PDF 入队；既有 iOS、Go worker 与 JS Runtime 投递合同已覆盖图片 + PDF，同幂等键重试 UI 2/2，Go app/worker 定向测试通过，Postgres 包 compile-only 通过，Runtime 68/68。因本机没有 `CH_TEST_DATABASE_URL`，Postgres 集成仍未运行。本条授权不意味着数据库或线上集成通过。
- MDA-07、MDA-08 继续保持未勾选：前者等待 Host 行信息呈现审核及完整视觉核对；后者等待用户决定待决表并完成合成列表截图、额外状态验收和逐项视觉核对。

### 2026-11-07 · MDA-11/12 Task 构件 Quick Look 复验

- `testMordeTaskArtifactQuickLookShowsSyntheticPreview` 按真实导航加载合成 Task 详情，再从文件库“AI 产物”点开已校验的本机文本构件；首次验收和 2026-11-07 稳定态复验均通过 1/1，最新结果 `/tmp/MDA-12-quicklook-derived/Logs/Test/Test-Morde-2026.11.07_08-16-43-+0800.xcresult`，截图 `/tmp/MDA-12-quicklook-screenshots/564E9BCA-F5C6-42BC-B8D0-BC70AAC0C82A.png`。按钮清单 `/tmp/MDA-12-quicklook-screenshots/7A6B2605-5B62-4A34-8D4C-D8A62F8D682C.txt`；不涉及真实用户文件或服务端。
- 稳定态只有系统预览正文，没有可见关闭、分享、保存或其他预览操作按钮，只能下滑关闭；因此当前没有额外按钮需要删除。与设计稿的实质差异是缺少文件详情返回栏/标题/四项底部 Tab，且 HTML 同类行点击只提示“打开 <文件名>”。实现符合设计稿的详情页、保留 Quick Look 或恢复提示三种选择已列入待审核表；答复前不改 Task 构件行行为。
- 此项只证明合成 Task artifact 从文件库打开及显示本机预览，不覆盖真实账户、附件权限切换、清理失败重试，也不替代逐页截图差异核对。MDA-11/12 继续未勾选。

### 2026-11-07 · MDA-13 设置根页截图对照

- `testMordeDesignNavigationRoutes` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1，结果 `/tmp/MDA-settings-root-visual-20261107.xcresult`；截图 `/tmp/MDA-settings-root-visual-attachments-20261107/6AC7500E-A704-4C9C-A373-21A5C95A4BDD.png`。本地 HTML 通过仅绑定回环的预览打开，逐项核对设置标题/返回、10 行名称顺序、三个分组及底部四个 Tab；Morde 和设计稿根页控件数量与顺序一致。
- 真实值差异：HTML 的套餐使用率和审批规则描述是演示字段；Morde 的 `/api/me` 只有账号 ID/名称，审批规则详情尚未实现，所以当前显示用户名且不伪造示例套餐/策略摘要。Morde 唯一急停行会在服务端确认已停用时改为“重新启用全部任务”，仍是原位置的单一控件；该恢复状态已写入待审核表。MDA-13 根页截图已取得，详情页及设置旧功能入口处理仍待验收。

### 2026-11-07 · MDA-13 账号与设备详情复验

- 扩展 `testMordeSettingsRoutesAndHonestUnavailableRows` 并在 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA-settings-detail-audit3-20261107.xcresult`）：从设置根页进入“账号”，核对服务地址、管理客户端安装和退出入口；进入“设备”，用本地 Hub fixture 填入 HTTPS 地址并打开远端授权弹窗后取消，未提交开链接请求；随后原样返回设置页，其他占位路由与关于页仍可达。
- 详情稳定态截图：账号 `/tmp/MDA-settings-detail-audit3-attachments-20261107/DA74B647-E98E-4ABA-852B-7A06E54966C2.png`，设备授权弹窗 `/tmp/MDA-settings-detail-audit3-attachments-20261107/0FD1711A-1F66-4510-8250-401CB77A68F8.png`。Hub/账号均为本地合成夹具，不代表真实服务联调。
- 与 HTML 设置行只显示 toast 的原型行为有差异；Morde 详情页多出真实账户/安全操作及 Hub 远端开链接流程。具体作用、移除影响和建议已记入待审核表。本轮只增加 UI 验收与文档记录，没有改动产品行为；MDA-13 仍待用户审核额外交互并完成详情页视觉核对。

### 2026-11-07 · MDA-06 媒体审批与版本冲突

- 媒体审批合成用例 `/tmp/MDA-approval-media-route3-20261107.xcresult` 通过 1/1。截图 `/tmp/MDA-approval-media-route3-attachments-20261107/710960C0-8B2C-4C1C-85FA-6F1471A90B88.png` 显示当前账号下 1 项图片审批：收件箱隐藏批准/拒绝，说明必须在带预览的 Task 视图处理，并保留“看事件链 ›”。
- 审批版本冲突合成用例 `/tmp/MDA-approval-conflict-refresh-20261107.xcresult` 通过 1/1：服务端对旧版本返回冲突，详情刷新为已完成后旧批准按钮消失、标题待办数从 1 更新到 0。此项不代表真实 Portal 联调。
- `testMordeApprovalDisablesBothDecisionsWhileSubmitting` `/tmp/MDA-approval-disable-race-20261107.xcresult` 通过 1/1；合成响应延迟期间同时断言批准和拒绝入口禁用，避免另一决策抢先提交。
- 当前媒体审批卡没有直达 Task 预览入口；差异和“不加按钮、由标题/卡片点击进入任务预览”的候选方案已列入待审核表。未获审核前不改现有卡片交互。

### 2026-11-07 · MDA-06 审批列表下拉恢复

- `testMordeApprovalInboxShowsUnconfirmedStateWhenTaskListFails` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA-06-approval-refresh-fixed.xcresult`）。本地列表第一次返回 503，界面不伪报 0 或空列表；下拉后第二次返回已确认的空列表，待办计数显示 0 且错误提示消失。
- 此次定位到刷新成功响应返回时 SwiftUI 的 refresh task 已被取消，旧逻辑把有效结果丢弃并永久显示加载态。审批刷新现在通过账号 scope 和请求代次保护并发/切号，接受仍属于当前账号和最新刷新的成功结果。该恢复用例不覆盖有审批项结果；批准、拒绝、计数与动作已有独立 Morde 用例。整页视觉仍待与 HTML 对照。

### 2026-11-07 · MDA-05 事件链账号隔离

- 新增 `testMordeEventChainSwitchingAccountsDoesNotShowPreviousAccountData`，在 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA-05-account-isolation-final.xcresult`）。本地 A/B 账号使用相同的合成 Task ID，但各自返回不同详情和事件；A 的事件加载态出现后通过账号设置登出并登录 B，再打开同 ID 事件链，验证 B 的详情与事件可见、A 私有标记不可见。
- 用例补充了本地已登录账号清理步骤，避免 Simulator 的持久登录态让“切换账号”断言从 B 开始。两次早期失败均保留：一次是 `ProgressView` 被查成 `StaticText`，一次是 Simulator 留有 B 登录；修复后原账号隔离断言通过。URLProtocol 在登出时取消 A 的 30 秒延迟请求，因此未证明取消后仍被 URLSession 交付的迟到响应；保留这个验收限制。改动只增加 DEBUG 合成数据/稳定无障碍标识和 UI 验收，不改生产数据合同。

### 2026-11-07 · MDA-02 会话草稿与附件隔离

- `testMordeChatDraftAndAttachmentsStayWithTheirConversation` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA-chat-draft-scope2-20261107.xcresult`）。本地合成主会话挂有图片和 PDF，输入文字草稿后切换到另一会话；验证另一会话自己的消息正常显示，原会话文字草稿和图片/PDF chip 均未串入。用例不连接真实服务。
- 此结果关闭同账号跨会话的草稿/附件隔离检查；同账号跨会话及跨账号文字草稿隔离均已通过；附件权限预览、剩余附件边界和聊天页逐屏视觉对照仍未验收，因此 MDA-02 保持进行中。

### 2026-11-07 · 内置浏览器设计稿原型逻辑走查

- 在 Codex 内置浏览器通过仅绑定 `127.0.0.1:8765` 的本机预览交互核对聊天文本发送、任务列表/详情、审批到事件链、资源到 Workspace 分享权限弹层、文件库三类筛选和行提示、工作项新建弹层、好友入口、AI 工具会话只读返回、抽屉搜索与设置入口。聊天使用一条合成测试文本验证原型后续显示和任务列表状态；无真实服务请求，无文件上传或外部分享。
- 本轮再次在内置浏览器按实际界面顺序回归任务列表→任务详情（点“急停”后原型状态变为“已急停”）、审批→对应事件链、资源→Workspace→文件行提示、抽屉→文件库；“AI 产物”筛选只显示 AI 文件，“我上传的”筛选只显示个人上传文件。浏览器截图显示文件库筛选页。操作发生在本地 HTML 的演示状态，无真实 API、副作用或文件传输。
- 此走查只证明本地 HTML 原型的演示交互可触发，不代表 Morde 页面功能验收。Morde 行为仍以独立 `Morde.xcodeproj` / `Morde` scheme 的 Simulator 用例为准；不因此勾选任何 MDA 页面。

### 2026-11-07 · MDA-05 隔离回归复验与资源提示本地化

- 用户再次明确授权活动 Task 的图片/PDF 附件入队。现有 iOS、Go worker 与 JS Runtime 合同已实现并有 Morde UI 同幂等键重试 2/2、Go 定向测试及 Runtime 68/68 通过证据；本机未配置 `CH_TEST_DATABASE_URL`，真实 PostgreSQL 集成仍未运行。该授权不代表线上部署验收。
- 复核 MDA-05 后移除了对已取消 URLProtocol 请求的 31 秒空等；用例仍确认 Account A 事件请求在加载中时切换至 B、同一 Task ID 的 B 详情与事件可见、A 私有数据不可见。更新后的 `testMordeEventChainSwitchingAccountsDoesNotShowPreviousAccountData` 在 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA-05-account-isolation-fast-final2-20261107.xcresult`）。取消后仍实际送达的迟到响应依然未覆盖。
- 资源页无真实添加流程时的提示改为使用已有 `Not implemented yet` 本地化字符串，不增加控件或改变按钮行为；Morde build 随上述 UI 用例通过。MDA-07 继续因 Host 额外状态审核与逐页视觉对照未完成而保持进行中。

### 2026-11-07 · MDA-11/12 个人附件预览取消源码复核

- 复核 `MIFLibraryViews.swift` 与 Morde 聊天预览状态：预览目标携带账号 scope；解析和下载完成后均重新检查登录、账号 scope、同步状态、加载/错误状态及附件元数据；账号 scope 变化、同步关闭、同步错误及页面关闭都会清除预览并取消下载，已下载的临时文件会进入清理流程。
- 现有 `testMordePersonalAttachmentPreviewCancelsWhenClosedWhileLoading` 覆盖用户手动关闭加载页后不显示迟到错误/预览；尚无 Simulator 用例直接注入下载中切换账号或关闭同步。源码路径有保护，但该两种 UI 时序仍保持未验收，MDA-11/12 不因此勾选。

### 2026-11-07 · MDA-06/08 页面差异审计与 MDA-08 截图

- MDA-06 只读审计确认：第三种审批动作、摘要 diff/成本/测试/回滚信息和自动处理历史缺当前 API 合同；媒体审批缺 Task 预览直达入口。第三动作与入口行为已在待决表等待用户审核；保持批准/拒绝及当前权限门控，不新增按钮或伪造服务数据。审批卡与设计稿仍需并排截图验收。
- 新增 `MDA-08-work-items-list` 和 `MDA-08-work-item-create-form` 截图附件；`testMordeWorkItemsRouteShowsDesignFiltersAndCreateForm` 在 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA-08-work-items-visual-evidence-20261107.xcresult`）。本地 HTML 原型经内置浏览器实测：工作项列表显示 6 个示例项、4 个筛选和认领动作；“＋”打开只含类型、标题与创建按钮的底部弹层。与 Morde 截图对照后，Morde 仍比原型多项目/Task 上下文卡片、可选描述及阻塞管理操作；原型 creator/source 字段在 API 中缺少可信值；“待认领”语义不一致。具体用途与建议留在待决表，未收到用户审核前不更改产品行为；MDA-08 仍保持未勾选。

### 2026-11-07 · MDA-04 Task 暂停状态与并发动作验收

- `testMordeTaskDetailPauseLocksActionsAndRefreshesStatus` 在 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA-04-task-actions-20261107.xcresult`）。合成 Task 从 running 开始，延迟的暂停动作提交期间 Pause 与 Cancel 同时禁用；收到 202 后重新加载 Task，显示 paused 状态和 Resume。用例截图已导出至 `/tmp/MDA-04-task-actions-attachments-20261107/`。该测试补上暂停到暂停态的真实客户端路径，不声称覆盖其他任务状态或真实服务。

### 2026-11-07 · MDA-05 迟到投递回归与事件链原型对照

- 扩展 `testMordeEventChainSwitchingAccountsDoesNotShowPreviousAccountData` 的 DEBUG URLProtocol 夹具：账号 A 的事件流工作项在 `stopLoading` 后不取消，并在延迟窗口后执行 `deliver`；随后切换至 B 并等待该窗口结束，B 的同 ID 任务仍只显示 B 详情/事件。测试中修正本地重登 helper，先清除用户名输入框的残值，避免合成账号误判。最终 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1，结果 `/tmp/MDA-05-late-response-derived/Logs/Test/Test-Morde-2026.11.07_04-02-18-+0800.xcresult`。Simulator 截图 `/tmp/MDA-05-late-response-visual-attachments/66844C54-C3E7-481D-8D6A-4DC5D55C8BDB.png`。首次测试失败来自账号输入框残值，修复 helper 后通过。
- 内置浏览器按 `chain` 页面核对原型：顶部菜单/“事件链”标题、任务名、7 条带已完成/当前/未开始节点的样例事件和四项底部 Tab。Morde 同样有菜单、标题和四项底部 Tab，但在标题后展示 API 任务目标和真实事件；截图只含一个合成 B 事件，节点状态显示“状态未知”。样例中的经理安排、未来步骤和预算没有对应真实 API 依据，按用户已确认的真实数据边界不伪造；“数量/状态”差异已记录。MDA-05 仍未完成，因为整体页面位置和视觉收敛尚未验收。

### 2026-11-07 · MDA-02 跨账号文字草稿隔离复验

- 截图已从 xcresult 导出到 `/tmp/MDA-02-cross-account-draft-attachments-20261107/8E491F27-0A28-4D68-ABCF-DD2B124847AB.png`。
- 修正 DEBUG URLProtocol 测试夹具的主会话响应优先级：账号隔离夹具同时启用通用聊天夹具时，按当前登录账号返回各自会话 ID，避免账号 A/B 数据被通用会话覆盖；不改生产会话逻辑。
- `testMordeChatDraftIsClearedWhenAccountChanges` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1，结果 `/tmp/MDA-02-cross-account-draft-derived/Logs/Test/Test-Morde-2026.11.07_04-13-05-+0800.xcresult`。用例在 Account A 的会话输入未发送文字，登出并登录 Account B 后验证 Account A 的草稿不显示在 B 的输入区或消息流中。
- 本用例仅覆盖文字草稿跨账号清理；附件权限变化、切号/同步关闭时取消与临时文件清理仍需另行验收。MDA-02 继续因附件状态与页面视觉对照未完成而保持进行中。

### 2026-11-07 · MDA-02 聊天页首轮截图差异

- 已对照 HTML `chat` 页面与 Morde 的跨账号草稿验收截图；Morde 保留 nanoMuse 消息气泡/字体和设计稿四项底部 Tab。截图 `/tmp/MDA-02-cross-account-draft-attachments-20261107/8E491F27-0A28-4D68-ABCF-DD2B124847AB.png` 是功能状态截图（Account B 的合成会话），不是最终布局截图，不能据此勾选页面。
- 常驻“＋”差异已按用户审核决定修复：移除输入栏按钮，并把新建/选择活动 Task 放到聊天长按菜单中的 Task options。相关入口、活动 Task 文字队列、图片/PDF 队列及响应丢失后的同键重试用例已通过；聊天主页面完整截图对照、消息密度、头部状态、输入框尺寸/间距和所有状态的控件数量仍需核验。

### 2026-11-07 · MDA-03 任务列表首屏截图对照

- 为 `testMordeTaskDetailShowsRealStepsWithoutBudget` 增加一次任务列表首屏截图附件；用例在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1，结果 `/tmp/MDA-03-visual-derived/Logs/Test/Test-Morde-2026.11.07_04-23-44-+0800.xcresult`。截图 `/tmp/MDA-03-visual-attachments-20261107/713EC29F-375D-48F0-A77E-C366F5A12BB9.png`，该测试改动仅保存页面证据。
- 内置浏览器的 HTML `tasks` 页显示菜单、标题、“＋”及 6 行样例；Morde 首屏的页头位置/38pt 按钮、44pt 状态图标、平铺行和无行尾箭头基本对齐。Morde fixture 显示 11 个首屏/后续任务行，是合成分页数据量，与 HTML 静态样例数量不同；不复制 HTML 的执行者名或百分比。
- 行摘要差异：原型显示步骤总数/完成数、执行者/等待角色和演示进度；当时服务端 `TaskSummary` 尚无步数。用户随后已授权扩展真实计数；现由批量聚合提供 `step_count` / `completed_step_count`，预算步骤不计入，避免逐行读取详情。执行者和百分比仍没有可信字段，不复制演示值。实现及验收记录见“2026-11-07 · MDA-03 TaskSummary 步数摘要”。
- 首屏之外的分页/刷新/失败/无权限状态布局、长列表滚动到导航底部的截屏及 iOS 16.0～16.3 探针运行仍未核验，MDA-03 保持进行中。

### 2026-11-07 · MDA-02 Task 目标迁入长按菜单与附件队列复验

- 按用户审核结论移除聊天输入框常驻 Task 目标“＋”，将“新建 / 追加活动 Task”选择放入现有聊天长按菜单的 Task options。菜单标签扩大到整行可点区域，并为新任务、各活动 Task 选项增加稳定的无障碍标识；保留现有目标/模型/Host/Workspace 校验与原发送位置。
- 首次回归暴露两个 UI 自动化问题：菜单标签没有覆盖整行中心，且半屏 Task options 内的模型选择器需要滚动后才创建。修正后，长按 Task options 与附件选择器、运行中 Task 文字入队通过 2/2（`/tmp/MDA-02-task-target-menu-final-20261107.xcresult`）。
- 随后聊天导航、运行中 Task 图片/PDF 入队、响应丢失后的相同幂等键附件重试通过 3/3（`/tmp/MDA-02-attachment-queue-final-20261107.xcresult`）。新的聊天页截图 `/tmp/MDA-02-attachment-queue-attachments-20261107/5FD5024F-7767-4BE9-89DA-A085D665454A.png` 确认输入栏无多余“＋”，原有消息输入/发送位置及四个底部 Tab 保留。内置浏览器同时核对 HTML `chat`：四个底部 Tab 和输入/发送区位置对应；本次 Simulator 只有一条合成短消息，而 HTML 停在多轮对话末尾，顶部文案也随任务状态不同，不能据此声称消息密度和完整内容区已视觉验收。
- `testMordeChatAttachmentWithoutLibraryProofCannotOpenPreview`、`testMordePersonalAttachmentPreviewCancelsWhenSyncIsDisabledWhileLoading` 和 `testMordeAttachmentPreviewRejectsMismatchedContentLength` 在 iPhone 18 Pro / iOS 27.0 Simulator 通过 3/3（`/tmp/MDA-02-preview-boundaries-final-20261107.xcresult`）：无个人库证明不提供可点击预览，同步关闭会取消加载，下载头与实际长度不一致时 fail closed。以上用例均为本地合成账号、文件与 URLProtocol，不连接真实服务。
- MDA-02 仍因 Task 摘要卡额外交互审核，以及聊天页完整视觉对照未完成而保持进行中；MDA-01 真实数据计数与全页视觉项目仍未完成。

### 2026-11-07 · MDA-02 附件上限与内置浏览器原型走查

- `testMordeChatAttachmentLimitRejectsExpandedPDFSet` 与 `testMordeTemporaryAttachmentCleanupRetriesAfterFailure` 在 iPhone 18 Pro / iOS 27.0 独立 Morde scheme Simulator 同轮通过 2/2，结果 `/tmp/MDA-02-12-attachment-boundaries-20261107-r1.xcresult`。本机合成的 2 张图片与 5 个单页 PDF 展开为 17 个上传对象；应用显示 16 项限额提示，不显示部分成功的附件 chip，也没有活动 Task 入队按钮。清理用例验证首次失败登记路径、恢复权限后重试删除；没有连接真实服务或访问用户文件。
- 内置浏览器使用 `http://127.0.0.1:8765/Morde%20iOS%E8%AE%BE%E8%AE%A1%E7%A8%BF.html` 实际走查：侧栏搜索“广东”筛选出对应旁聊；进入任务列表并打开 Task 详情；顶部“新建任务”回到聊天 composer；审批页可见设计稿里的拒绝、灰度、批准及自动处理历史。此处是静态 HTML 原型的本地交互，不作为 Morde App 功能验收或页面截图对照。
- 用户再次授权运行中 Task 支持图片 + PDF 入队；该合同与实现已由前序 Go worker、JS Runtime 和 Morde UI 同幂等键重试证据覆盖，本轮不重复扩展。MDA-02 仍待 Task 摘要卡入口审核及完整聊天页面视觉对照；MDA-01 真实数据计数与完整视觉仍未完成。
- 并行只读复核没有改动 MDA-04、MDA-07、MDA-11/12 页面源码：任务详情继续保留用户审核过的暂停/取消操作；资源页不伪造当前模型没有的云手机/AI 状态或 Workspace 容量，Host 额外状态维持待审核；附件预览继续 fail closed，临时文件删除失败路径保留待重试记录，但 Morde target 重试验收现已通过。

### 2026-11-07 · MDA-12 临时附件清理失败重试

- 新增 `-morde.localAttachmentCleanupFailureFixture` 和 `testMordeTemporaryAttachmentCleanupRetriesAfterFailure`。该 Morde DEBUG 夹具在 app 临时目录创建合成附件，临时移除目录写权限以触发 `unlink` 失败，确认清理器保留 pending URL；恢复权限后调用实际 `retryPending()`，确认文件删除且 pending 项清除。
- 首次 UI 运行没有看到稳定结果提示；将夹具延至登录后 1 秒执行后，单项用例通过；随后与附件 16 项超限边界在 iPhone 18 Pro / iOS 27.0 独立 `Morde` scheme Simulator 串行复验 2/2，结果 `/tmp/MDA-02-12-attachment-boundaries-20261107-r1.xcresult`。测试使用合成临时文件，不接真实 Portal/Host，不访问用户文件。
- MDA-12 的临时清理失败重试验收已完成；权限变化的其他状态、本机/Task Quick Look 控件和剩余逐页视觉对照仍待验收，故 MDA-11/12 不勾选。

### 2026-11-07 · MDA-02 多轮聊天首屏对照

- `testMordeChatMultiTurnVisualFixtureMatchesDesignDensity` 在 iPhone 18 Pro / iOS 27.0 独立 Morde scheme Simulator 通过 1/1，结果 `/tmp/MDA-02-chat-multiturn-visual-20261107-r11.xcresult`；截图 `/tmp/MDA-02-chat-multiturn-visual-20261107-r11/62AC656C-FBDC-4613-9CBA-45BDBC616DE2.png`。合成夹具与 HTML `chat` 同为 7 条文字消息，不含真实账户、预算或附件；7 条消息都在输入栏上方可见，四项底部 Tab 位置保留。
- 对照本地 HTML 首屏后，将简体中文占位从“消息”改为“发消息…”，iOS UI 用例断言该文案。当前剩余可见差异：HTML 头部显示“努力工作中”，Morde 显示品牌名 `Morde`；HTML 的空输入发送按钮为黑色，Morde 的空输入按钮呈禁用灰色；HTML 样例含 PDF/产物 chip，本截图夹具不含附件。状态文案不能以演示值伪装真实 Task 状态；后续需用真实状态和合成附件另验对应 UI，核对字体、气泡间距及按钮视觉，不新增入口。
- 该截图只核对空输入、7 条文本消息的首屏几何与数量；MDA-02 仍因附件/Task 状态视觉、真实数据计数及 Task 摘要卡额外交互审核而保持进行中。

### 2026-11-07 · MDA-04 Task 详情首轮原型对照

- 在内置浏览器打开 HTML `data-view="task"`：返回、任务标题、经理摘要、纵向事件/步骤时间线，以及固定在底部 Tab 上方的红色“急停这个任务”。Morde Simulator 运行中截图 `/tmp/MDA-04-task-actions-attachments-20261107/21A28201-D56E-4301-9081-26EFA55D0353.png`：返回与任务标题位置接近，真实状态/步骤/事件/产物分区清楚；Budget 字段未显示。
- 差异：Morde 的“任务操作”区在可滚动正文中，显示实际可用的“暂停”和“取消任务”；HTML 只有一个固定“急停这个任务”。当前 Task API 没有独立急停动作，不能将暂停/取消改名或当作等价操作；用户已审核保留现有真实操作，故继续保留其入口。Morde 的真实 Activity 和 artifacts 信息也比 HTML 样例多；不伪造经理元数据、执行者、未来步骤或预算。
- 本轮是结构对照，不代表固定底部按钮的位置/动作数已对齐；已批准动作在保留前提下的最终布局与状态页面视觉仍待核验。MDA-04 继续进行中。

### 2026-11-07 · MDA-06 审批页首屏截图对照

- 在 Morde Simulator 运行 `testMordeApprovalInboxShowsOnlyConfirmedDecisions`，通过 1/1（`/tmp/MDA-06-visual-derived/Logs/Test/Test-Morde-2026.11.07_04-33-05-+0800.xcresult`）。在点击批准前保留截图 `/tmp/MDA-06-visual-attachments-20261107/EBD22ABB-343A-46AD-B0F4-882897623C74.png`；用例随后仍验证批准成功、保留事件链链接并能打开对应 Task 事件链。
- HTML `appr` 首屏有菜单/标题/待办数、多个审批卡、差异/测试摘要、驳回/灰度/批准三按钮和“已自动处理”历史。Morde 截图显示一条合成审批卡、真实摘要字段、拒绝/批准两按钮、事件链链接和四 Tab；页头、卡片平铺与底部 Tab 结构相近。Morde 的“1 项待审批”是本地 fixture 当前确认数，HTML 的“2 件”是静态示例，数量差异不表示服务功能缺失。
- 需要用户审核的差异仍是灰度/改天数第三动作、自动处理历史与详细 diff/成本/测试信息；当前 API 无相关合同，不新增/伪造。媒体审批安全提示和缺 Task 直达预览入口已在待决表。状态、加载/错误、媒体审批与其他按钮组合仍待逐状态视觉核对，MDA-06 保持进行中。

### 2026-11-07 · MDA-15 Host Gateway Workbench 源码对照

- 对照 `MordeComputerDesignRoom` / `MordeHostView`、Host Gateway 入口和 `web/workbench/src/app/Workbench.tsx`。Morde 路由持有票据并把整个 Gateway 页面装入 WKWebView；Host 根路径会跳转到 `/__ch/wb/`。Workbench 页面含 noVNC 桌面、终端、Workspace、Host 文件、可选 VS Code、控制租约/接管/交还、移动键盘、窗口快捷键、全屏、重连和主机续期。Morde 外层另有关闭/主机名标题，所以 Workbench TopBar 会与外层构成两级标题；这是源码推断，尚未用真实 Gateway 页面截图确认。
- 与 HTML `pc` 对照：设计稿有顶部/接管后“截图”、桌面里的浏览器/终端/文件，以及停止任务、接管、键盘、触控；Workbench 没有“截图到 Workspace”或停止当前 Task 的流程。Workbench 另有 Workspace、主机文件和 VS Code 分页以及续期/状态/窗口工具。WorkBench 控制 API 的动作是 `take_over` / `release`；Task agent 的 desktop screenshot/input API 不是 iOS 用户控制流。已将两侧差异与建议写入接口待决表，等待用户决定是否收敛额外 WebView 入口、是否扩展截图/Task 停止合同。
- 本轮为源码和 OpenAPI 对照，没有运行测试或接入任何 Host。已有 Simulator 票据用例只验证合成 URL/失败状态，不能证明真实 WKWebView 呈现、noVNC 同源画面、移动键盘或交接租约可用；MDA-15 保持未勾选。

### 2026-11-07 · MDA-02 空输入发送按钮对齐

- HTML `chat` 的发送圆按钮在空输入时仍为黑色；Morde 原来显示禁用灰色且不可点击。仅对 Morde composer 传入空输入回调：保留设计稿现有发送位置与黑色样式，点按只聚焦输入框，不会提交空消息；未传该回调的 nanoMuse composer 保持原状态。
- 图片/PDF 附件行改为白底描边文件卡片，显示真实保存类型标签；点击预览的账号/同步/元数据安全门控未改。`testMordeEmptyComposerSendFocusesInput` 与 `testMordeFullLayoutUploadsAndSendsImageAndPDFFixture` 在 iPhone 18 Pro / iOS 27.0 的独立 Morde scheme 同轮通过 2/2，结果 `/tmp/MDA-02-chat-visual-final-20261107/Logs/Test/Test-Morde-2026.11.07_07-23-37-+0800.xcresult`。空输入截图 `/tmp/MDA-02-chat-visual-final-attachments/AC4AF7E6-F002-4C86-9CB8-73A1D6031FE2.png`，图片/PDF 消息截图 `/tmp/MDA-02-chat-visual-final-attachments/709C2DE7-81EE-4C79-BDF6-EB0EC75F31AC.png`。
- 这两项差异已修复；MDA-02 仍需完成活动 Task 状态、消息与头部视觉及待审核交互的逐项核对，不因此勾选整页。

### 2026-11-07 · MDA-02 活动 Task 页头与停止入口收敛

- `chat` 页头仅在当前会话包含 queued/running Task 时显示真实运行状态；没有运行中 Task 时继续显示 Morde。运行中用例确认简体中文标签为“进行中…”，对齐原型状态语义且不伪造状态。
- 输入栏的独立“选择要停止的任务”图标与设计稿不符。依据用户已审核的聊天输入功能移入长按/菜单，将停止指定活动 Task 的操作移入现有 chat actions 二级菜单；原有停止 API 和 busy 门控保留，输入栏继续只用原发送位置。`testMordeChatComposerQueuesFollowUpOnActiveTask` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1，并验证没有常驻停止按钮且菜单仍提供对应 Task 的停止动作。结果 `/tmp/MDA-02-chat-visual-final-20261107/Logs/Test/Test-Morde-2026.11.07_07-40-04-+0800.xcresult`；截图 `/tmp/MDA-02-active-header-final/836988A2-98AA-4C86-8119-D19CB97B1AD2.png`。
- 移动入口后，`testMordeChatComposerQueuesImageAndPDFAttachmentsOnActiveTask` 与 `testMordeChatComposerKeepsQueuedAttachmentsAfterLostResponseForRetry` 同轮通过 2/2；确认附件队列、幂等重试及长按菜单停止入口未回归。结果 `/tmp/MDA-02-chat-visual-final-20261107/Logs/Test/Test-Morde-2026.11.07_07-42-46-+0800.xcresult`。
- 当前截图的头部、附件卡片和输入位置已核对；其他活动 Task 状态、完整聊天密度及 Task 摘要卡详情跳转仍待审核，MDA-02 保持进行中。

### 2026-11-07 · MDA-05 事件状态节点与摘要对齐

- 事件链行现在保留事件 payload 的 `status`；节点优先按事件自己的 `succeeded/completed/failed/running/waiting_user/queued/pending/unknown` 显示完成、失败、进行中或待确认状态，未识别状态回退原 Task 状态推断。Task 详情页面原有节点判定不变。步骤摘要改用步骤状态本地化，修复 `succeeded` 被显示为“状态未知”。
- `testMordeEventChainSwitchingAccountsDoesNotShowPreviousAccountData` 加入状态断言后在 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1，结果 `/tmp/MDA-05-event-status-derived/Logs/Test/Test-Morde-2026.11.07_07-57-25-+0800.xcresult`。它同时验证 Account A 被取消且晚到投递后不会覆盖 B 数据，B 的 `succeeded` 事件显示绿色完成节点及“已完成”摘要。截图 `/tmp/MDA-05-event-status-final-screenshots/41D779D3-DF45-4A2C-BECC-3822D4A55915.png`。
- 与 `chain` 原型并排复核后，菜单、标题、任务摘要、节点到正文的缩进、节点直径/连接线和底部四 Tab 顺序相符；HTML 的 7 条经理编排样例没有真实事件 API 依据，不能复制。七行视觉夹具已在后续记录中完成，但失败节点及加载/错误状态的视觉组合仍未验收，MDA-05 保持进行中。

### 2026-11-07 · MDA-05 七行事件链布局验收

- 新增仅供 Morde Simulator 使用的七行事件夹具，覆盖 5 条完成、1 条等待确认、1 条排队事件；补齐 queued 节点的本地化辅助标签“已排队”，圆圈与原型未开始节点保持同款空心视觉。夹具不改变服务/API，也不写入产品默认数据。
- `testMordeEventChainTimelineDensityAndStatusesMatchPrototype` 在 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA-05-event-status-derived/Logs/Test/Test-Morde-2026.11.07_08-09-06-+0800.xcresult`），验证七条标题、节点状态数量、无预算字段，并保存截图 `/tmp/MDA-05-event-chain-seven-row-screenshots/96564A08-4721-4EC4-93CE-6D6B39C96B4E.png`。
- 与本地 HTML `chain` 页截图逐行对照：两边均为 7 行、五个绿色完成节点、一个当前黑色节点、一个待开始空心节点；Morde 的标题、摘要层级、虚线连线、左右边距和四项底部 Tab 对齐。原型中的经理安排、部署/预算结果仍缺少真实事件合同；失败节点验收及加载/错误状态另记于后续记录，MDA-05 保持进行中。

### 2026-11-07 · MDA-05 失败事件节点验收

- `testMordeEventChainMarksFailedEvents` 在 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA-05-event-status-derived/Logs/Test/Test-Morde-2026.11.07_08-13-26-+0800.xcresult`）。合成的 `failed` 步骤显示红色叉号节点及“失败”摘要；其他行仍保留各自的成功/等待/排队状态。截图 `/tmp/MDA-05-event-chain-failed-screenshots/BDC2823B-E480-4EEB-B0CB-94131C41390E.png`。
- 完成、失败、等待确认、排队/未开始节点的状态映射及代表性七行密度已覆盖。事件流网络失败、加载中状态尚未完成最终视觉逐态核验；空任务与关联任务无历史已有独立截图验收；MDA-05 继续保持进行中。

### 2026-11-07 · MDA-05 空任务/关联任务无历史状态截图与本地化校正

- 在隔离的 iPhone 18 Pro / iOS 27.0 Simulator，以签名构建运行空任务和关联任务无历史两项用例，2/2 通过；结果包 /tmp/MDA-05-empty-states-localized.xcresult。签名构建保留 Simulator Keychain 能力，供合成登录凭证写入。
- HTML 的事件链标题为“事件链”；首轮截图发现 Morde 因 Event chain 缺少 zh-Hans 而显示英文，空任务说明也回退英文。为 Event chain 与 No current task is available 补齐简体/繁体中文；空任务测试现断言“事件链”和“当前没有可用的任务”，并在返回聊天前截图。
- 最新截图：关联 Task 无历史 /tmp/MDA-05-empty-states-localized-attachments/4EDC78F3-023A-4ABB-AC24-E6FEF275E8F1.png；无当前 Task /tmp/MDA-05-empty-states-localized-attachments/19F003B9-6E92-47A7-8298-0BF9F7292949.png。标题、真实空态文案及四项 Tab 已与设计稿语言和结构核对；两类空态均无预算或伪造事件。
- 后续事件链 loading 状态已由账号 A 延迟事件流用例覆盖；持续 503 网络错误及自动恢复见 2026-11-07 MDA-05 错误态验收记录。MDA-05 整页仍不勾选。

### 2026-11-07 · MDA-05 事件链网络错误与恢复验收

- 新增合成事件流持续返回 503 的专用夹具和稳定 accessibility ID。事件流错误留在事件链页面内显示“任务事件连接中断，正在重试…”，不再触发遮住页面的全局错误弹窗，也不增加独立 Retry 按钮；任务详情与已展示事件继续保持页面状态。
- `testMordeEventChainShowsNetworkErrorWithoutRetryButton` 与 `testMordeEventChainRecoversExpiredCursorAndDeduplicatesEvents` 在 iPhone 18 Pro / iOS 27.0 Simulator 同轮通过 2/2，结果 `/tmp/MDA-05-network-error-final-20261107.xcresult`。前者使用本地合成 503，确认事件链错误状态及无 Retry 按钮；后者确认临时断线后按快照游标恢复且重复事件只显示一次。
- 错误态截图 `/tmp/MDA-05-network-error-final-attachments/1E89BDB7-4D81-454B-BF1F-5FFE82C37BED.png` 已检查：保留设计稿页头、关联 Task 标题、四项底部 Tab，事件失败仅显示行内本地化提示。页面加载、网络错误及恢复路径已有合成证据；真实服务故障和其余页面视觉仍未验收，MDA-05 继续进行中。

### 2026-11-07 · MDA-05 加载态视觉及整页验收完成

- 在既有 `testMordeEventChainSwitchingAccountsDoesNotShowPreviousAccountData` 中记录 A 账号事件流加载态截图；同一流程随后切换至 B 账号，验证 B 的同 ID 任务/事件显示、A 的私有事件及延迟响应不泄漏。iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1，结果 `/tmp/MDA-05-loading-visual-20261107.xcresult`。
- 加载截图 `/tmp/MDA-05-loading-visual-attachments-20261107/38DE90E1-AD35-4E7A-A484-5608099DF22B.png` 已检查：保留“事件链”标题、关联任务标题及四项底部 Tab，内容区仅显示加载指示和“正在连接任务事件…”，没有增加 Retry 按钮。通过内置浏览器按原型的任务→抽屉→事件链顺序走查，原型主态为七条时间线；Simulator 七行截图 `/tmp/MDA-04-current-marker-attachments-20261107/772862D3-D269-4547-8B79-A014A4B48D35.png` 在标题、左右缩进、节点/连接线、状态与底部导航上与原型一致，预算字段未显示。
- 已结合空态、无历史、加载、网络错误/恢复、七行正常态、失败节点、事件状态及账号隔离证据完成 MDA-05；标记完成。

### 2026-11-07 · MDA-02 活动 Task 图片/PDF 队列合同再次确认

- 用户授权扩展运行中 Task 的图片/PDF 入队。检查确认这项实现已在 Go worker `message_delivery` v2、JS runtime 字段校验/幂等去重及 Task 内动态附件读取中接通：队列帧只携带附件 ID；broker 每次读取时重新验证当前个人 Task、账号授权及附件归属，图片和 PDF 分别按既有限额及解析流程处理，因此无需再建第二套合同或重复改接口。
- Go app checkpoint/附件读取定向测试与 Go worker 投递测试分别通过；JS runtime 的图片/PDF 投递与去重、冲突附件 ID 拒绝、account runtime 消息转发定向用例通过 3/3。此前 Morde Simulator 图片/PDF 入队及同幂等键重试用例已通过 3/3（`/tmp/MDA-02-attachment-queue-final-20261107.xcresult`）。
- 本轮完整 `node --test test/runtime.test.mjs` 运行超过 7 分钟没有新输出，手动中断时为 57 通过、1 项取消；被卡的 account runtime 测试单独复跑通过。`CH_TEST_DATABASE_URL` 未配置，PostgreSQL 集成没有执行；不能以本轮定向测试替代数据库集成结论。

### 2026-11-07 · MDA-07 资源页标题与云手机状态行对齐

- 对照 HTML `res`：设计稿显示“云端主机”，云手机是无独立小标题的状态行，之后才进入 Workspace。Morde 简体/繁体中文标题改为“云端主机/雲端主機”；移除“云手机”状态行上方的小标题，继续保留“执行状态不可用”且不伪造云手机实例。
- `testMordeResourceWorkspaceRouteBrowsesSyntheticManifest` 在独立 Morde target、iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA-resources-heading-final-20261107.xcresult`），断言标题、无额外云手机标题及 Host → 状态行 → Workspace 顺序。截图 `/tmp/MDA-resources-heading-attachments-20261107/49E9B331-CFCE-4A73-9E4C-D53E5C2C83D0.png` 已查看；夹具含多种 Host 生命周期/权限状态，因此只能说明页头和首屏 Host，完整视觉仍待核对。Host 行额外控制/租约/权限信息继续等待用户审核，没有擅自删除。

### 2026-11-07 · MDA-12 Task 构件缺少 Workspace proof 时不开放预览

- 扩展 `testMordeTaskArtifactQuickLookShowsSyntheticPreview`：缺少 `workspace_id/revision/path/size/SHA-256` 的 `Legacy metadata only` 构件行仍显示 metadata，但不存在 Quick Look 预览按钮；带完整 proof 的 `Verified local output` 仍可打开原生 Quick Look。
- 新增断言与原有成功预览流程在独立 Morde Simulator 同轮通过 2/2（`/tmp/MDA-resource-proof-final-20261107.xcresult`）。这里只验证合成 fixture 下的客户端 proof gate；有效 proof 后服务端返回 403/404、Bad ETag 和完整预览状态仍待验收，Quick Look 与设计稿详情页结构差异等待用户审核。

### 2026-11-07 · MDA-04 Task 标题与摘要间距收敛

- 将详情滚动内容的顶部留白从 14pt 收到 2pt，底部留白保持 14pt，缩小设计稿标题与状态摘要之间的间隔；不改 Task 操作或状态逻辑。
- `testMordeTaskDetailShowsRealStepsWithoutBudget` 在独立 Morde target、iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA-task-detail-spacing-final-20261107.xcresult`）；任务详情截图 `/tmp/MDA-task-detail-spacing-attachments-20261107/8312DAD8-FB9E-4528-A765-8B0F2B1099DD.png` 已导出检查，预算字段仍不显示。其他状态与页面全量位置/动作视觉仍待核对，MDA-04 保持未勾选。

### 2026-11-07 · MDA-12 错误 ETag 预览失败关闭

- 新增 `testMordeAttachmentPreviewRejectsMismatchedETag`，使用本机合成图片响应返回与校验内容不匹配的 ETag；客户端显示“请求失败”并且不呈现 `morde.attachment.preview.image`，没有进入预览页。
- 在 iPhone 18 Pro / iOS 27.0 独立 Morde Simulator 通过 1/1（`/tmp/MDA-attachment-bad-etag-final-20261107.xcresult`）。权限撤销/删除时 403/404 和完整预览状态核对仍待完成；Quick Look 与设计稿文件详情页结构差异仍等待用户审核，MDA-11～12 保持进行中。

### 2026-11-07 · MDA-15 云电脑移动端顶栏精简与原生页头对齐

- 按用户要求在 `max-width: 760px` 隐藏 Workbench 的 Host 续期按钮、Host 到期倒计时、控制租约倒计时、版本号和冗余说明；主机名/运行状态从移动端 Workbench 顶栏收起，改由原生页头显示。控制状态缩短为紧凑文案；接管/交还、失败重试及现有自动租约续期继续有效。
- 原生 `.computer(id, shared:)` 页头按设计稿改为居中控制状态，第二行显示云电脑 Host 名称及已绑定 Workspace，关闭按钮置右并使用深色圆底；连接提示缩短为“连接中…”且保证文字对比度。没有新增设计稿外按钮或未经授权的截图保存/停止 Task API。
- 本机 MSW Workbench 在内置浏览器按 390×844 实际媒体断点检查，AX 树和截图均确认无“续期”、到期时间、租约时间、控制版本和长说明，接管按钮仍在；恢复默认桌面视口后确认原续期按钮/时间信息仍在。`npm run test -- src/components/TopBar.test.tsx` 7/7 通过，`npm run typecheck` 通过。iPhone 18 Pro / iOS 27.0 Morde Simulator 的云电脑路由进入/返回、WebView 加载失败和票据拒绝 3/3 通过，结果 `/tmp/MDA-cloud-computer-final-20261107.xcresult`；首屏截图 `/tmp/MDA-cloud-computer-final-attachments-20261107/268A424B-89CB-4582-8A31-C5ED75A84E7F.png` 已检查。
- 这轮未连接真实 Host，WebView 内容仍需有真实但获准的 Host 才能验收。HTML 设计稿只有浏览器/终端/文件入口；额外页面与窗口工具的移动端入口已按用户后续决定移到长按/上下文菜单，见下方记录。MDA-15 继续未勾选。

### 2026-11-07 · MDA-15 移动端主入口与窗口工具收敛

- 按用户确认，移动端常驻导航只显示“浏览器 / 终端 / 文件”；主机文件页作为设计稿“文件”入口显示，Workspace 与 VS Code 从常驻栏移入“文件”长按菜单。浏览器的键盘、适应窗口、Alt+Tab、找回窗口和全屏操作移入“浏览器”长按菜单；同时支持触摸长按、上下文菜单与键盘菜单键，不增加常驻按钮。菜单仍调用原页面、权限门控和控制 API；未接管时 VS Code 保持禁用。
- 移动端隐藏已连接状态的重复文字及窗口控制常驻按钮；连接错误文案缩为“连接失败/连接已断开”，保留重新连接动作。续租、持有时间倒计时、版本号、主机重复信息和长说明继续隐藏；租约与自动续期逻辑不变。桌面视口仍显示原有分页、续期和窗口工具。
- 本机 MSW 内置浏览器 390×844 确认导航只显示三项、窗口操作及更多功能菜单可打开、Workspace 能从菜单切换；默认桌面视口确认原有五项分页及续期/窗口操作仍可见。`npm run test -- src/app/Workbench.test.tsx src/components/TopBar.test.tsx` 21/21、`npm run typecheck` 和 `npm run lint` 通过。只使用本机合成数据；Host WebView 仍显示断连替身，不能证明真实桌面画面、终端或 Host 文件能力，MDA-15 保持未勾选。

### 2026-11-07 · MDA-15 原生云电脑页头与控制区对齐

- 原生页头按设计稿调整为左侧截图、居中控制状态与 Host/Workspace 副标题、右侧关闭；修正首轮截图发现的左右按钮压住标题问题。底部保留设计稿的“停止任务”和接管/交还控制入口；“停止任务”复用现有 Morde Task 接口，只操作当前会话里 Host 匹配的活动 Task，无匹配时禁用，多个 Task 时通过菜单选择。截图调用 WKWebView 本机快照并在设备内预览，不写入 Workspace、不上传。
- iPhone 18 Pro / iOS 27.0 Simulator 的 `testMordeComputerRouteAcceptsSyntheticTicketAndReturnsToResources` 通过 1/1（`/tmp/MDA-cloud-computer-redesign4-20261107.xcresult`）；覆盖页头元素、Stop Task 在无匹配任务时禁用、接管入口、Host 关闭返回及共享 Host 路由。截图 `/tmp/MDA-cloud-computer-redesign4-review-attachments-20261107/5F762848-2903-417C-8494-FADA088905F6.png` 已复核，左右按钮和标题不再重叠。
- 本轮没有连接真实 Host；合成票据下 WebView 仍处于“连接中…”，所以该截图只能验原生页头与控制区，不能验收实际蓝色桌面、浏览器、终端和文件内容。Workbench 的本机 390×844 预览及菜单/桌面断点测试作为独立证据保留。MDA-15 继续进行中，需在授权 Host 上完成嵌入 WebView 的实际页面核验后再勾选。

### 2026-11-07 · MDA-15 按 HTML 尺寸收敛云电脑移动端布局

- 直接按 `docs/Morde iOS设计稿.html` 的云电脑 CSS 收敛原生与 Workbench 嵌入壳：页头相机/关闭圆按钮 38pt、标题 19pt、副标题 13pt；导航栏 62pt、三枚图标 42pt；主窗口上留 36pt、下留 64pt；底部 Stop Task / 控制权操作 46pt，文案和内外边距同步收紧。加载/连接失败时 WKWebView 透明显示同色竖向蓝色渐变。独立 Morde 桌面视口及 Workbench 非 iOS 移动预览布局保持原样。
- Workbench 的 `Workbench.test.tsx` 与 `TopBar.test.tsx` 共 21 项通过，`npm run typecheck`、`npm run lint` 通过。iPhone 18 Pro / iOS 27.0 ARM64 Simulator 的 `testMordeComputerRouteAcceptsSyntheticTicketAndReturnsToResources` 通过 1/1（`/tmp/MDA-cloud-computer-redesign9-20261107.xcresult`），覆盖原生控制页进入/返回、页头元素、无活动 Task 时 Stop Task 禁用和接管入口；截图 `/tmp/MDA-cloud-computer-redesign9-review-attachments-20261107/B33B45DF-A36A-4DC8-9D56-07FB3BCB53B3.png` 已检查，按钮未遮挡、连接态背景为蓝色渐变。
- 该 Simulator 票据指向合成不可达 Host，最终画面显示“连接失败”提示，不含真实 Gateway 内容；本轮因此没有宣称浏览器、终端、文件的真实 Host 视觉/操作已验收。全架构 Simulator 构建另有 x86_64 `MordeMessageOutbox` 符号缺失；本次 iPhone ARM64 用例使用 `ONLY_ACTIVE_ARCH=YES` 通过。MDA-15 继续进行中，需在真实授权 Host 连接后核对嵌入页面。

### 2026-11-07 · MDA-04 运行中时间线节点视觉对齐

- 将共享时间线 `.current` 节点调整为 24pt 黑色圆点内的 4pt 背景色中心点，与 HTML `.ev.now .ck` 的黑底白色“·”一致；节点状态判定和 Task 操作逻辑不变。共享渲染也让事件链当前节点同步对齐。
- 仅为无动作延迟替身的运行中详情 fixture 补一条 `running` 当前步骤；暂停动作 fixture 保持原步骤集，不伪造非法的“paused”步骤状态。`testMordeTaskDetailShowsRealStepsWithoutBudget` 与 `testMordeEventChainTimelineDensityAndStatusesMatchPrototype` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 同轮通过 2/2（`/tmp/MDA-04-current-marker-20261107.xcresult`）；截图 `/tmp/MDA-04-current-marker-attachments-20261107/CDA78509-887C-4550-8171-94D52CC03071.png`、`772862D3-D269-4547-8B79-A014A4B48D35.png` 已查看。暂停/取消互斥和恢复用例另行通过 1/1（`/tmp/MDA-04-current-marker-pause-20261107.xcresult`）。MDA-04 仍因其他状态、动作布局和页面整体对照未完成而保持未勾选。

### 2026-11-07 · MDA-04 活动事件节点跟随事件状态

- Task 仍在运行时，任务活动区原来会把最后一条事件按整个 Task 状态画成“当前”；当事件自身已 `succeeded` 时，圆点与“已完成”文案冲突。现在 Task 详情与独立事件链共用事件状态优先的节点判定，并向辅助朗读提供相同状态。
- 合成夹具确认 running Task 的活动事件为 succeeded。`testMordeTaskDetailShowsRealStepsWithoutBudget` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA-04-event-marker-20261107.xcresult`）；断言活动事件读作“已完成”，截图 `/tmp/MDA-04-event-marker-attachments-20261107/80539581-712B-4E84-8CA2-31F5069C6D21.png` 已检查：已完成事件为绿色勾选，当前 Task 步骤仍为黑色中心点。MDA-04 仍待其他状态、动作布局和页面整体对照，未勾选。

### 2026-11-07 · MDA-08 工作项页截图差异复核

- 检查现有合成列表截图 `/tmp/MDA-08-work-items-visual-evidence-attachments-20261107/5AD5E76C-344C-4DEC-8818-397CC14F7A94.png` 与创建弹层截图 `/tmp/MDA-08-work-items-visual-evidence-attachments-20261107/54B88779-0C79-4743-9A27-A3FFA9E03EDA.png`，对应 `testMordeWorkItemsRouteShowsDesignFiltersAndCreateForm` 的既有 Morde Simulator 证据（`/tmp/MDA-08-work-items-visual-evidence-20261107.xcresult`）。HTML `work` 主页面只有标题/“＋”、四个筛选和工作项行；当前 Morde 截图还显示项目选择器、“此项目中的任务”选择卡、项目 Task 摘要及额外行操作。创建弹层的类型、标题和创建按钮对得上，但 Morde 还多一个可选描述框。
- 上述项目上下文、Task 关联、行操作和描述字段的作用/影响及建议已记录在“工作项范围、创建字段与行操作”待决表。没有未经审核地移除或迁移这些能力；MDA-08 继续待用户审核和剩余行操作、错误/空态及完整视觉验收，未勾选。
### 2026-11-07 · MDA-15 用户确认入口迁移与内置浏览器复验

- 用户确认将设计稿外的 Workspace、VS Code 与窗口操作移入长按/菜单入口；已有实现符合决定，本轮没有增加常驻按钮。移动端常驻栏仍只显示浏览器、终端、文件，其中“文件”映射主机文件；文件菜单提供 Workspace/VS Code，浏览器菜单提供键盘、适应窗口、Alt+Tab、找回窗口和全屏。
- 内置浏览器以 390×844 窄屏打开本机 MSW 合成 Host，页面只呈现浏览器/终端/文件三项。实际打开文件上下文菜单后显示 Workspace 与受控制权门控的 VS Code；选择 Workspace 成功切换内容。浏览器上下文菜单显示窗口操作项；键盘在未接管的合成状态下正确禁用。页面可访问树没有续期按钮、持有/租约倒计时、版本号或长篇 Host 说明。已有 `Workbench.test.tsx` 的触摸长按用例覆盖菜单出现并抑制常规页签点击，上一轮相关 Workbench/TopBar 测试 21/21 通过。
- 内置浏览器运行的是普通移动 Web 断点，没有 Morde WKWebView 注入的 `data-morde-ios-shell` 标记，因此通用 Web 移动页仍显示自己的控制状态和底部操作区；iOS 壳通过专用 CSS 隐藏重复内容并使用原生页头/底栏，相关 Simulator 票据路由已通过。合成 Host 的 noVNC 显示连接失败，本轮只验证了导航、菜单和移动布局，没有验证真实 Gateway 桌面、终端或文件交互；MDA-15 继续进行中。

### 2026-11-07 · MDA-08 三类列表错误与下拉恢复验收

- 新增本地 `-morde.localWorkItemListFailureFixture`，让项目列表、共享会话列表和工作项列表各在首次请求时返回合成 503。`testMordeWorkItemListFailuresRecoverOnPullToRefreshWithoutRetryButtons` 在 iPhone 18 Pro / iOS 27.0 独立 Morde Simulator 通过 1/1（`/tmp/MDA-08-workitem-refresh-test/Logs/Test/Test-Morde-2026.11.07_12-59-13-+0800.xcresult`）：项目列表错误卡片没有 Retry；下拉后项目列表恢复；选中项目后会话/工作项错误状态也都没有 Retry；再次下拉后二者恢复并显示工作项。错误态截图 `/tmp/MDA-08-workitem-refresh-attachments/CAC7410B-2873-4E0A-AC83-B22D58DEB623.png`，会话与工作项错误截图 `/tmp/MDA-08-workitem-refresh-attachments/FC9153DE-50D0-48EB-BE8B-0C4702291A08.png`。
- 首次复验暴露 SwiftUI 刷新闭包在项目状态变更期间被取消，造成恢复后内容空白；将刷新工作放入独立 `Task` 并等待完成后，同一用例通过。整个过程只请求 `127.0.0.1` URLProtocol fixture，不访问真实 Portal。MDA-08 仍因额外项目/Task 上下文、描述字段和行操作待审核，及整页视觉、其他行状态/键盘交互未验收而保持未勾选。

### 2026-11-07 · MDA-11/12 个人附件证明、metadata-only 与权限拒绝验收

- 首次附件权限复验停在聊天登录页并报告 `NSURLErrorDomain -3003`，测试没有到达附件请求，结果包不完整。后续只在独立合成 Simulator 加诊断，确认 `/api/capabilities`、`/api/auth/client-login` 均由 `127.0.0.1` URLProtocol 返回 200，Keychain 添加状态为 0；未访问真实 Portal。继续检查发现夹具生成 `fixture-image.png` 而测试一直寻找 `fixture-image.jpg`，且消息 fixture 的固定附件数量与两份实际合成源文件不符。已将图像夹具改为 JPEG、校正占位数量和测试定位器；移除本轮临时诊断日志与启动参数。附件引用数量会随原始/派生文件变化，缺少 proof 的用例现在只验证引用仍显示、没有可点预览、没有预览页和误报成功。
- `testMordeAttachmentPreviewFailsClosedWhenPermissionIsDeniedAfterLibraryProof` 与 `testMordeAttachmentPreviewFailsClosedWhenServerReturnsNotFoundAfterLibraryProof` 在 iPhone 18 Pro / iOS 27.0 独立 Morde Simulator 分别通过 1/1，结果 `/tmp/MDA-12-permission-403-fixed-20261107.xcresult`、`/tmp/MDA-12-permission-404-fixed-20261107.xcresult`。本地服务 fixture 确认附件请求分别收到合成 403/404；UI 显示请求失败且未呈现图片或 PDF 预览。
- 新增 metadata-only fixture 与 `testMordeLibraryMetadataOnlyRowsAreVisibleButNotPreviewable`：合法 cloud metadata 在“全部”显示“仅元数据”，无按钮/预览入口，在“我上传的”筛选中隐藏。该测试在 `/tmp/MDA-11-metadata-only-final-20261107.xcresult` 中单项通过，Simulator 截图导出到 `/tmp/MDA-11-metadata-only-all-attachments-20261107/`。`testMordeChatAttachmentWithoutLibraryProofCannotOpenPreview` 在最终独立复验通过 1/1（`/tmp/MDA-11-no-proof-final2-20261107.xcresult`）。均使用合成账号/文件及 loopback URLProtocol。
- 以上补齐 403/404、聊天附件缺 proof 和 metadata-only 可见性/不可预览用例；MDA-11/12 仍因其他页面状态、完整视觉比对及 Quick Look 与设计稿详情页的结构差异待审核而保持未勾选。

### 2026-11-07 · MDA-07 Workspace/Host 绑定状态断言复验

- `testMordeResourceWorkspaceRouteBrowsesSyntheticManifest` 在独立 Morde target、iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA-07-status-final-20261107.xcresult`）。新增断言覆盖：ready 且有真实 Host 绑定时显示“已挂载”和 Host 名称；同步中保留同步状态；不可用或未绑定时不显示“已挂载”，也不伪造 Host 名称。
- 截图 `/tmp/MDA-07-status-final-attachments/79087AEE-7EB1-4ED0-958C-6351778EF6CC.png` 已检查，展示合成 Host 生命周期与 Workspace 状态行。MDA-07 仍待 Host 行额外控制信息审核及资源页完整视觉对照，保持未勾选。

### 2026-11-07 · MDA-15 云电脑截图复核与本地 Workbench 构建

- 用户再次确认云电脑主界面不显示续租按钮、持有/租约倒计时和冗余说明；常驻导航保留“浏览器 / 终端 / 文件”，Workspace、主机文件、VS Code 与窗口操作从主界面移到长按/菜单入口。现有本地实现已按此规则收敛：移动端隐藏续租、倒计时、版本号、长说明及常驻窗口操作；保留接管/交还、失败恢复与自动租约逻辑。Workspace 与 VS Code 经“文件”菜单进入，主机文件对应“文件”常驻项。
- 对当前源码执行 `npm run build` 成功；`npm run test -- src/app/Workbench.test.tsx src/components/TopBar.test.tsx` 21/21 通过，`npm run typecheck` 与 `npm run lint` 通过。实现位置：`web/workbench/src/components/TopBar.tsx`、`web/workbench/src/styles.css`、`web/workbench/src/app/Workbench.tsx`；原生壳和 WebView 入口位于 `project/nanoMuse/android/src/ios/Morde/MordeApp.swift`。
- 为避免 WebView 文档开始时 `<html>` 尚未建立而漏掉移动壳样式标记，Morde WebView 现于 document start 与 document end 都注入 `data-morde-ios-shell`。`testMordeComputerRouteAcceptsSyntheticTicketAndReturnsToResources` 在专用 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA15-shell-injection-final-20261107.xcresult`）；截图 `/tmp/MDA15-shell-injection-attachments/0E59EAAD-0701-4AD0-B704-D5D3E9849002.png` 已检查，原生标题/关闭/控制区布局正常。合成 Host 不可达，截图中的 WebView 显示连接失败，不能据此认定真实桌面内容已验收。
- 用户截图仍呈现续租/倒计时/长说明及五个常驻分页；这与已检查的本地源码和构建结果不同，推测实际 Host 当前提供的 Workbench 资源尚未更新，或 iOS 壳样式未作用于该页面。Workbench 由 Host/Portal 提供，单独本地构建不会替换正在运行的 Host 资源。遵循仅本地修改的要求，本轮没有向外发送文件、部署或连接真实 Host；MDA-15 继续未勾选，待获准环境载入更新后的本地构建后再验实际 WebView 视觉与远程桌面交互。

### 2026-11-07 · MDA-03 TaskSummary 步数摘要

- 按用户授权，在 Go Task 摘要/API 与 OpenAPI TaskSummary 增加 step_count、completed_step_count。Postgres 对每页任务做一次批量计数查询，不为每行额外请求详情；过滤 kind == budget，只把 status == succeeded 计入完成数。完整 Task 响应从实际 steps 计算相同字段。
- iOS MordeTask 解码两个可选字段，任务列表优先展示 API 计数；旧服务响应缺字段时用已有真实 steps 回退。无步骤的任务不显示“0 执行步骤 · 0 已完成”。
- testMordeTaskListShowsStepCountsFromSummary 在 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1，断言行显示“5 执行步骤 · 2 已完成”；结果 /tmp/MDA-03-task-step-counts-20261107.xcresult，截图 /tmp/MDA-03-task-step-counts-attachments/C1EAF426-3C5A-456A-805C-28C608A879AE.png。Go 领域计数单测通过；Workbench API 类型生成、npm run typecheck 与 npm run build 通过。
- Go 领域计数单测及定向 API 计数用例通过；Postgres 集成用例因未配置 CH_TEST_DATABASE_URL 而跳过。全量 API suite 另有既存 OpenAPI 合同断言失败，不影响定向计数用例。Portal 类型检查也有当前 OpenAPI 与页面/mock 合同不一致错误。计数页面验收已通过，但 MDA-03 仍待其他状态、旧系统运行时与完整页面视觉核对。

### 2026-11-07 · MDA-03 任务列表状态矩阵

- 新增本地合成 TaskSummary fixture，覆盖 queued、running、waiting_user、paused、completed、failed、cancelled 与未知状态；所有计数为 0，确认不显示假的步骤进度。行状态增加仅供 UI 验收定位的无障碍 ID，不增加任何可见控件。
- testMordeTaskListRendersKnownAndUnknownStatusesWithoutActions 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1；逐行断言八种状态文案、完成/失败图标和列表中没有暂停/继续/取消操作按钮。结果 /tmp/MDA-03-task-status-20261107.xcresult，截图 /tmp/MDA-03-task-status-attachments/9EAEE1D5-DFC0-4832-95DF-85372F8C6D79.png 已检查。状态展示已覆盖；第二页 403 后旧任务清理与下拉恢复也已通过；MDA-03 仍待旧版系统刷新行为和完整设计稿截图对照。


### 2026-11-07 · MDA-04 完成/失败详情状态验收

- 在本地状态矩阵 fixture 中增加 completed 与 failed 详情，以及空事件历史的 204 响应；仅增加 UI 测试定位用 accessibility ID，不增添可见按钮或改变任务操作。
- `testMordeTaskTerminalDetailsKeepStatusStepsAndApprovedActions` 在专用 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA-04-terminal-details-final-20261107.xcresult`）。完成态验证结果与已完成步骤可见、预算步骤和暂停/取消入口隐藏；失败态验证失败步骤、红色失败状态和现有批次“重试”入口保留，且不出现暂停/取消或预算字段。两个状态均确认无事件历史，不展示无关错误提示。
- 截图已检查：完成态 `/tmp/MDA-04-terminal-details-final-attachments-20261107/79F23C6F-EB51-4CFF-9790-A8213986F3C7.png`；失败态 `/tmp/MDA-04-terminal-details-final-attachments-20261107/7972B2B4-E32D-4FE0-BA44-91A1827B535C.png`。MDA-04 继续未勾选，等待/权限状态、剩余竞态和与 HTML 的动作位置/整页视觉对照仍待核验。


### 2026-11-07 · MDA-04 等待审批/补充信息详情验收

- `testMordeTaskWaitingDetailsShowApprovalAndClarificationControls` 在专用 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA-04-waiting-details-20261107.xcresult`）。审批详情显示正确的等待审批状态、拒绝/批准和已有取消操作；澄清详情显示问题、空输入时禁用发送、填写后启用，并保留已有取消操作；两者不会显示对方类型的控件。用例未提交审批或澄清响应。
- 截图已检查：审批 `/tmp/MDA-04-waiting-details-attachments-20261107/E74EF2A9-DFD2-4D58-B5E2-D8642982B569.png`；补充信息 `/tmp/MDA-04-waiting-details-attachments-20261107/5A34C9CA-E88A-47F3-80CC-B20C20F4EEA6.png`。MDA-04 仍未勾选，其他等待原因/权限状态、剩余竞态及与 HTML 的逐页动作位置和整体视觉比对待验收。


### 2026-11-07 · MDA-04 取消终态与步骤状态文案验收

- 截图复核发现取消完成的 Task 步骤被显示为“状态未知”。已将真实 `cancelled` 步骤状态映射为本地化“已取消”；状态标记继续使用中性未确认样式，不把取消误标成成功或失败。未增加可见按钮。
- `testMordeTaskTerminalDetailsKeepStatusStepsAndApprovedActions` 在专用 iPhone 18 Pro / iOS 27.0 Morde Simulator 复跑通过 1/1（`/tmp/MDA-04-terminal-details-cancelled-label-20261107.xcresult`），并断言取消步骤可访问状态为“已取消”；完成、失败、取消详情仍均不显示预算字段或不适用的 Task 操作。
- 复查截图 `/tmp/MDA-04-terminal-details-cancelled-label-attachments-20261107/B47E7D17-9161-47C2-8F47-3F5C3124CA29.png`。MDA-04 仍未勾选；其他等待/权限状态、竞态以及与 HTML 逐屏对照和按钮位置核验待完成。

### 2026-11-07 · MDA-04 详情审批提交互斥与状态刷新

- 新增 `testMordeTaskApprovalLocksTaskActionsWhileSubmitting`，用本地延迟响应验证详情页批准提交期间，批准、拒绝和取消任务三个现有入口同时禁用；服务端确认后详情刷新为已完成并移除待决操作。没有增加可见控件或发送真实审批请求。
- 用例在专用 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA-04-approval-action-lock-visual-20261107.xcresult`）。审批提交中截图 `/tmp/MDA-04-approval-action-lock-visual-attachments-20261107/A5506FA5-22C5-4644-8B9F-77AFFD4F70E5.png` 与已完成刷新截图 `/tmp/MDA-04-approval-action-lock-visual-attachments-20261107/A8AB7160-C3EB-4AD6-8BF1-956388E22DC8.png` 已检查。MDA-04 仍因其他竞态、完整布局与 HTML 逐屏视觉核对未完成而保持未勾选。


### 2026-11-07 · MDA-04 澄清提交互斥与状态刷新

- 为本地 UI 验收添加可控的延迟澄清响应 fixture。`testMordeTaskClarificationLocksTaskActionsWhileSubmitting` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA-04-clarification-action-lock-20261107.xcresult`）：提交期间输入框、发送和取消任务入口均禁用；响应确认后详情刷新为已完成并清除澄清问题。只使用合成文本，没有提交真实澄清。
- 提交中截图 `/tmp/MDA-04-clarification-action-lock-attachments-20261107/C623D1AB-31CB-4929-816B-27E3C848BF5F.png` 已检查。MDA-04 的审批/澄清提交与取消互斥现均有证据；Task A→B 迟到详情隔离已在下方独立复验，其他失败反馈/返回草稿和完整视觉差异仍待核验。

### 2026-11-07 · MDA-04 Task 详情账号切换迟到响应隔离

- 新增仅供本地 UI 验收的延迟详情响应 fixture：Account A 的同 ID Task 详情请求在页面返回/请求取消后仍于 35 秒后投递，用于覆盖账号切换竞态。`launchLocalChat(resetExistingAccount: true)` 现在会从已恢复页面回到聊天入口后再重置账号，避免 Simulator 保留上次登录路由影响测试准备。
- `testMordeTaskDetailDropsLateResponseAfterAccountSwitch` 在专用 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA-04-detail-account-late-response-final-20261107.xcresult`）：A 的 q0000001 详情请求仍在途时退出并登录 B，B 打开相同 Task ID；A 响应延迟到达后，B 详情仍保持 `account-b-private-task-detail`，没有出现 A 的目标内容。仅使用本地合成账号与 API fixture。
- 截图已检查：A 请求在途 `/tmp/MDA-04-detail-account-late-response-final-attachments-20261107/3CE91CDE-687D-48C5-814A-9717A20E02C5.png`；迟到响应后 B 详情 `/tmp/MDA-04-detail-account-late-response-final-attachments-20261107/83A888A9-ECF2-4E17-9DD7-B8BBE8CFF698.png`。MDA-04 仍未勾选；其他等待原因/权限状态、失败反馈、返回草稿及页面完整动作布局和与 HTML 逐页视觉核对待完成。

### 2026-11-07 · MDA-04 其他等待原因与详情权限拒绝态

- 新增本地 `mcp_result_unknown` Task fixture。`testMordeTaskWaitingOtherReasonDoesNotShowApprovalControls` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA-04-waiting-other-reason-20261107.xcresult`）：详情显示“操作结果待核对”及当前步骤，只保留已存在的取消操作，不误显审批或澄清表单。截图 `/tmp/MDA-04-waiting-other-reason-attachments-20261107/6D8C2C9B-AC00-4A79-812B-C0D3E47BAB40.png` 已检查。
- `testMordeTaskDetailShowsPermissionErrorWithoutTaskActions` 在同一专用 Simulator 通过 1/1，结果 `/tmp/MDA-04-waiting-permission-regression-20261107.xcresult`。该轮的合成 403 页面当时仍显示“重试加载”；该入口已按后续用户审核移除，最新重进恢复用例及证据见下方记录。
- MDA-04 当时仍等待用户决定三种条件式 Retry；当前决定已在“接口与待决能力”表关闭。其他异常反馈/返回草稿及完整页面动作布局和 HTML 逐页视觉核对仍未完成。

### 2026-11-07 · MDA-04 移除三种条件式“重试加载”按钮

- 按用户审核，详情读取失败、事件流失败、事件历史未确认三种状态不再显示“重试加载”；错误/未确认文案保留。移除对应手动回调及用于触发重试的状态字段；Task 详情重新进入时仍按 scope/task ID 执行加载，事件流维持原有自动重连。失败批次项的业务“重试”操作保留。
- `testMordeTaskDetailShowsPermissionErrorWithoutTaskActions`、`testMordeTaskDetailTimelineFailureHasNoRetryButtonAndRecoversAutomatically`、`testMordeTaskDetailUnconfirmedHistoryHasNoRetryButton` 在专用 iPhone 18 Pro / iOS 27.0 Morde Simulator 同轮通过 3/3，结果 `/Users/zhongxuan_li/Library/Developer/Xcode/DerivedData/Morde-hawfozzytljmicgvrjitsykselwa/Logs/Test/Test-Morde-2026.11.07_16-34-09-+0800.xcresult`。覆盖首个详情请求 403 后返回重进、第二次读取成功；事件流错误后自动重连并恢复；未确认历史只显示状态说明。没有访问真实 Portal。
- 已检查截图：详情错误 `/tmp/MDA-04-retry-removal-attachments-20261107/05F40870-B0A5-4DC5-8DFB-B9DC437BB4A2.png`，返回重进恢复 `/tmp/MDA-04-retry-removal-attachments-20261107/F1F42CD3-E10E-4B6A-AA61-2745FF8B1561.png`，未确认历史 `/tmp/MDA-04-retry-removal-attachments-20261107/14BF09C9-9FA5-479D-9793-7EECFD9C6BCC.png`，事件自动恢复 `/tmp/MDA-04-retry-removal-attachments-20261107/DD97ECB5-80FF-4191-925B-BE2297740498.png`。MDA-04 继续未勾选，仍需完成其他失败/返回草稿状态和任务详情整页与 HTML 的动作位置、视觉核对。

- 2026-11-09 按用户再次确认后重跑 `testMordeTaskDetailShowsPermissionErrorWithoutTaskActions`、`testMordeTaskDetailTimelineFailureHasNoRetryButtonAndRecoversAutomatically`、`testMordeTaskDetailUnconfirmedHistoryHasNoRetryButton`，iPhone 18 Pro / iOS 27.0 Morde Simulator 3/3 通过，结果 `/Users/zhongxuan_li/Library/Developer/Xcode/DerivedData/Morde-hawfozzytljmicgvrjitsykselwa/Logs/Test/Test-Morde-2026.11.09_07-48-38-+0800.xcresult`。复核无三处页面级 Retry、详情失败返回重进后成功、事件流自动恢复、未确认历史只显示说明；已检查截图 `/tmp/MDA04-retry-approval-final-attachments-20261109/F0B7F083-4515-449F-9D38-18DD33A05C41.png`、`6A1FB83A-7EDA-4787-B54F-083B47FB2D52.png`、`574F7860-BA5E-4B72-BFE9-B75373E9C8E9.png`、`A3F588FD-6690-4C5D-8E19-AF5340E7F511.png`。暂停/取消、失败批次“重试”等已审核业务操作未改；MDA-04 仍待其余状态及整页 HTML 视觉核验。


### 2026-11-07 · MDA-03 任务权限撤销与 MDA-04 Retry 审核落实

- MDA-03：任务列表分页请求遇到 Morde 403 时清空已缓存任务和游标，避免权限撤销后继续展示旧数据；错误说明保留，用户可用下拉刷新重新读取。只处理权限拒绝，不清除普通网络错误下仍可用的旧列表。
- `testMordeTaskListClearsStaleRowsAfterPermissionRevocationAndRefreshRecovers` 在专用 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1，结果 `/tmp/MDA-03-permission-recovery-pagination-final-20261107.xcresult`：首屏 24 条任务、加载第二页时模拟 403、断言旧行清空且没有 Retry 按钮，再下拉刷新确认首屏恢复。截图已检查：权限错误 `/tmp/MDA-03-permission-recovery-pagination-attachments-20261107/C7DB5CC4-7349-4FFD-8977-A2953010E314.png`，恢复后 `/tmp/MDA-03-permission-recovery-pagination-attachments-20261107/416B8501-87C1-40D0-B2E7-0D4EE9158AF6.png`。未访问真实 Portal。
- MDA-04：按用户审核，详情读取失败、事件流失败、事件历史未确认三处移除条件式“重试加载”按钮；保留解释文字、事件自动重连和失败批次项的业务“重试”。Task 详情返回后重进会重新读取。三项定向 Simulator 用例 3/3 通过，记录见“2026-11-07 · MDA-04 移除三种条件式‘重试加载’按钮”；用户于 2026-11-08 再次确认此方案。
- MDA-03 / MDA-04 仍未勾选：完整页面与 HTML 视觉/动作位置核对、iOS 16.0～16.3 刷新运行时及 Task 详情其他错误和返回草稿状态仍待完成。


### 2026-11-07 · MDA-04 Task 操作结果未知后的恢复

- 新增合成用例 `testMordeTaskDetailRetriesUnknownPauseThroughTheExistingAction`，在首个暂停请求已被服务端接受但响应丢失时验证：详情继续显示最后确认的“运行中”状态；现有“暂停”操作恢复可用；错误说明显示“任务操作结果未知”；没有添加 Retry 按钮。再次点击“暂停”时夹具要求复用原幂等键，随后详情刷新为“已暂停”，暂停入口切换成已有“继续”。
- 用例在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1，结果 `/tmp/MDA-04-task-action-unknown-retry-20261107.xcresult`。错误状态截图 `/tmp/MDA-04-task-action-unknown-retry-attachments-20261107/5B0D8A4C-2E6B-451E-8567-722DC4BA84FB.png`，恢复后 `/tmp/MDA-04-task-action-unknown-retry-attachments-20261107/74C7045F-B230-427A-B580-C1987AE63D9A.png`；均使用本地合成 API。
- 此项复用已批准的任务操作入口，不增加按钮；MDA-04 仍需完成其它竞态、返回草稿和动作位置/页面视觉对照。


### 2026-11-07 · MDA-02 / MDA-04 聊天草稿经 Task 详情往返

- 扩展 `testMordeChatDraftAndAttachmentsStayWithTheirConversation`：在主会话保留未发送文字、图片和 PDF 后，切换到设计稿已有的任务 Tab，打开任务详情并返回，再切回聊天；断言三种草稿仍属于原会话。随后切换到侧边会话，继续确认它们不泄漏到其他会话。
- 用例在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1，结果 `/tmp/MDA-02-draft-after-task-detail-20261107.xcresult`；返回聊天的截图 `/tmp/MDA-02-draft-after-task-detail-attachments-20261107/6708E619-3B56-4A92-9833-1A9701F66ADD.png` 已检查。只使用本机合成附件和账号，没有发送真实消息或文件。
- MDA-02/MDA-04 不因这条流程完成而勾选；聊天页和任务详情仍需完整 HTML 视觉、按钮位置与其余状态验收。


### 2026-11-07 · MDA-04 Task 操作固定底栏与审核项回归

- 现有 Task 操作区按 HTML `task-foot` 固定在内容区底部、Tab 栏上方；可滚动内容预留空间，避免按钮遮挡步骤。保留运行中暂停/取消、暂停后继续/取消、审批/澄清和失败批次项“重试”等现有操作，没有新增常驻操作。
- 当前布局下 `testMordeTaskApprovalLocksTaskActionsWhileSubmitting`、`testMordeTaskClarificationLocksTaskActionsWhileSubmitting`、`testMordeTaskTerminalDetailsKeepStatusStepsAndApprovedActions` 在专用 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 3/3（`/tmp/MDA-04-task-footer-regression-20261107.xcresult`）。已检查审批/澄清提交中、失败批次项及取消终态截图；既有操作可见，终态无不适用按钮。
- 同布局 `testMordeTaskDetailPauseLocksActionsAndRefreshesStatus` 通过 1/1（`/tmp/MDA-04-task-footer-overlay-20261107.xcresult`）；`testMordeTaskDetailRetriesUnknownPauseThroughTheExistingAction` 通过 1/1（`/tmp/MDA-04-task-footer-unknown-result-20261107.xcresult`）。未知结果说明和原“暂停”入口位于底栏，再次点按后刷新为已暂停；截图 `/tmp/MDA-04-task-footer-unknown-result-attachments-20261107/FFBCE2E2-D1B2-4C34-9C05-303289019065.png`、`/tmp/MDA-04-task-footer-unknown-result-attachments-20261107/BD7E8363-8524-451C-9A69-A6BDDF21DEB2.png` 已检查。全部仅使用本地合成 API。
- 3 项回归结果另记录一条非阻断运行时警告：`MordeStandaloneApp.swift` 出现 `Invalid frame dimension (negative or non-finite)`；测试无失败。需在后续页面/布局核验中确认来源。Task 详情整页与 HTML 的完整视觉对照仍未完成，MDA-04 不勾选。


### 2026-11-07 · MDA-06 审批首屏与原型再次对照

- 在内置浏览器按原型审批 Tab 查看首屏：页头待办数、平铺审批内容、动作行、事件链链接、自动处理历史和底部四 Tab。与 Morde Simulator 首屏截图 `/tmp/MDA-06-visual-attachments-20261107/EBD22ABB-343A-46AD-B0F4-882897623C74.png` 并排核对；Morde 的页头、真实审批文本、拒绝/批准按钮、事件链入口和四 Tab 顺序已覆盖原型主结构。
- 标题/正文/按钮字号沿用 HTML `.appr` CSS（17.5/14.5/14.5px）；事实文字现按 HTML `line-height:1.65` 增加行距，并将卡内事实项间距调整为 6pt。Simulator 使用一条合成审批，原型中的第二张卡、灰度/改天数、自动处理历史和额外摘要没有对应真实合同，继续按待审核表保留为差异，不补造内容或按钮。
- MDA-06 仍待其他错误/权限状态的视觉核对、审批媒体入口决定和上述设计稿外能力审核；本次重核不勾选整页。


### 2026-11-07 · MDA-04 三处加载重试移除决定确认

- 用户再次确认移除 Task 详情读取失败、事件流失败、事件历史未确认三处“重试加载”入口；错误说明与自动事件重连保留，失败批次项的业务“重试”不受影响。三个回归用例在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 3/3（`/tmp/MDA04-load-recovery-buttons-20261107.xcresult`），覆盖详情读取失败后返回重进恢复、事件流无按钮并自动重连、未确认历史无按钮。另将未确认历史用例扩展为返回后重进并确认空历史，1/1 通过（`/tmp/MDA04-unconfirmed-history-reentry-20261107.xcresult`）。

### 2026-11-07 · MDA-15 iOS 壳独立对齐旧版 Workbench

- 用户截图与本地 Workbench 源码不一致，说明旧 Host 页面 CSS 可能没有 iOS 壳专用规则。现在由 `MordeHostView` 在 WebView documentEnd 注入 Morde iOS 样式，使旧 Host 页面也按设计稿收敛；iOS 专用规则集中在原生壳，不再依赖远端 Host 资源更新。续租、Host/控制租约倒计时、版本号、重复状态、冗余说明及 Workspace/VS Code 主导航隐藏；设计稿“文件”仍映射主机文件浏览，Workspace、VS Code 与窗口操作从菜单进入。
- 新增仅 Debug 可用的合成旧 Workbench HTML 和 `testMordeComputerWebViewAlignsLegacyWorkbenchToDesignOnDevice`，断言主界面只显示浏览器/终端/文件，且续租、到期/租约倒计时、版本及长说明不可见。该用例与原有 `testMordeComputerRouteAcceptsSyntheticTicketAndReturnsToResources` 在专用 Morde iPhone 18 Pro / iOS 27.0 Simulator 复验中通过；结果见 `/tmp/MDA15-old-host-shell-final-20261107.xcresult`。合成页面截图 `/tmp/MDA15-old-host-shell-final-attachments-20261107/FCB3BDA4-5D60-42A3-AE51-A7C62DC37E92.png` 已检查。
- 本次仅使用本地合成 Host 页面，没有部署、外发文件或连接真实 Host。截图中 WebView 内容是本地的“新标签页”替身，不代表真实远端桌面已加载；本地 CSS 注入只调整页面结构与隐藏内容；长按菜单等操作仍依赖 Host 当前下发的 Workbench JavaScript。实际 Gateway 桌面、终端和文件交互及完整逐页视觉核验仍待授权环境，因此 MDA-15 不勾选。

### 2026-11-07 · MDA-15 Host 匹配 Task 停止流程

- 本地合成 fixture 将运行中的 `q0000001` 绑定到当前主会话与 Host `h0000001`。`testMordeComputerStopsTheActiveTaskBoundToItsHost` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA15-host-task-stop-recheck-20261107.xcresult`）：等待主会话加载后打开 Host、点击“停止任务”，再打开同一 Task 详情确认状态为“已取消”。截图 `/tmp/MDA15-host-task-stop-recheck-attachments-20261107/202E252F-7807-4734-B65E-6D90A8DFE3C8.png`、`/tmp/MDA15-host-task-stop-recheck-attachments-20261107/242F6B00-A951-4DF1-BA08-3C966E5327B1.png` 已检查。
- 入口复用当前会话中 Host 匹配 Task 的既有 `cancel` 动作，只取消目标 Task；没有扩充 API，也不等同于全局或独立急停能力。测试中的合成 WebView 显示“连接失败”，因此该结果只验证 Host/Task 匹配、取消请求及详情状态同步，不代表真实 Gateway/桌面验收。内置浏览器回环打开 `Morde iOS设计稿.html`，从资源页进入 PC-02，核对页头、浏览器/终端/文件及底栏位置；再以本机 Vite/MSW、390×844 实际打开 Workbench：续租/倒计时/版本/长说明均被移动样式收起，浏览器菜单可见键盘/窗口操作，文件菜单可打开 Workspace；相关触摸长按用例通过 2/2。以上只用本地合成数据，原型/Workbench 模拟页不代替真实 Host 验收。MDA-15 仍不勾选。


### 2026-11-07 · MDA-07 Host 控制错误去重

- 资源适配器之前把同一 `hostControlErrors[id]` 同时放进 `host.error` 和 `control.error`，错误时会在 Host 行重复出现。现改为：Host 打开错误只走 Host 错误位，控制状态错误只在控制状态位显示；未改变控制状态门控、刷新方式或 Host 行操作。
- 新增 `testMordeResourceShowsHostControlErrorOnce`，在本地合成 Host 控制状态返回 503 时断言控制错误只出现一次且未进入 Host 打开错误位。独立 Morde iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA07-control-error-20261107.xcresult`），截图 `/tmp/MDA07-control-error-attachments-20261107/FC0B7056-9705-4740-8D05-38A8BBA3D034.png` 已检查。MDA-07 仍待 Host 行额外信息审核及资源页完整视觉对照，不勾选。

### 2026-11-07 · MDA-13 设置页进度校准

- 根页 10 行顺序/分组/返回截图已通过；账号页和设备详情部分入口/授权弹窗有 Morde Simulator 证据。审计确认设置页不是“详情完全未核验”，但完整详情视觉、设备稳定态、凭证页和部分操作状态尚未覆盖；TODO 当前状态已同步修正。
- 急停恢复、设备远端开链接、账号页额外入口、旧本机设置功能去留、未实现的设计稿设置能力仍在待审核表中。未删除、迁移或新增这些功能。

### 2026-11-07 · MDA-04 当前详情布局复验

- 在内置浏览器打开本地 HTML `task` 详情，核对返回/标题、经理摘要、四个时间线节点、底部红色动作区及四项 Tab。调整 Morde 详情底部操作按钮为 46pt、移除装饰图标和设计稿没有的分隔线；不改变暂停/继续/取消、审批、澄清、失败项重试等动作、状态门控或请求语义。`testMordeTaskDetailShowsRealStepsWithoutBudget` 增加运行中两个操作按钮的 46pt 几何断言。
- `testMordeTaskDetailShowsRealStepsWithoutBudget`、`testMordeTaskDetailPauseLocksActionsAndRefreshesStatus`、`testMordeTaskDetailRetriesUnknownPauseThroughTheExistingAction`、`testMordeTaskDetailShowsPermissionErrorWithoutTaskActions` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 4/4（`/tmp/MDA04-footer-46pt-recheck4-20261107.xcresult`）。覆盖按钮尺寸、暂停期间动作互斥及状态刷新、未知结果按原操作恢复、403 后返回重进恢复；最终截图 `/tmp/MDA04-footer-46pt-attachments-final2-20261107/B2CC2A4E-FD29-4048-B230-07A22B5FD461.png` 已查看。
- 当前截图证实两个真实操作已固定在底部 Tab 上方，正文可滚动。按钮位置/高度与设计稿操作区接近；因用户已审核保留现有任务操作，按钮数量、颜色和语义仍与 HTML 单个红色“急停”不同。Task API 无独立急停动作，不将暂停/取消冒充急停；合成数据不补造经理信息或预算。其余状态和全页视觉核对仍待完成，MDA-04 保持未勾选。

### 2026-11-07 · MDA-04 临时服务错误后的同操作恢复

- 扩展本地 Task action fixture：首次暂停请求返回 503 并保留请求幂等键，Task 仍是已确认的“运行中”；第二次点击现有“暂停”操作时，fixture 强制要求复用同一幂等键，再返回已接受的“已暂停”状态。没有新增 Retry 按钮，也未更改生产 API 或任务操作逻辑。
- 新增 `testMordeTaskDetailRecoversFromTemporaryActionFailureWithSameRequest`，在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1，结果 `/tmp/MDA04-action-503-retry-20261107.xcresult`。验证 503 后错误说明、运行中状态和原操作仍可见；复用请求后状态变为已暂停。截图已检查：失败 `/tmp/MDA04-action-503-retry-attachments-20261107/EC200ECA-05C1-4C8E-A4A8-11204092CA9F.png`，恢复 `/tmp/MDA04-action-503-retry-attachments-20261107/8EF7055A-B6BA-4924-8BAD-F7130EC60780.png`。仅使用本地合成 API。
- MDA-04 仍未勾选：任务详情其余失败/返回状态及整页 HTML 与 Simulator 视觉、按钮位置核对继续待完成。

### 2026-11-07 · MDA-03 旧版刷新运行时可用性检查

- 检查本机 CoreSimulator runtimes：目前只安装 iOS 27.0 和 27.1，没有 iOS 16.0～16.3 runtime；因此不能在本机模拟器运行 MDA-03 的旧版 UIKit 回弹探针。现有 iOS 27.0 下拉刷新通过记录保留，但不作为旧版系统兼容证据；MDA-03 继续未勾选，待有相应运行时/设备时补验。

### 2026-11-07 · MDA-04 Task 详情原型逐页对照

- 用内置浏览器仅通过 `127.0.0.1` 打开本地 `docs/Morde iOS设计稿.html`，从设计稿任务列表进入 `task` 详情。确认原型首屏包括返回/标题、经理状态摘要、四个进度节点、底部单个红色操作及四项 Tab；本机没有向外网发布或上传 HTML。
- 与既有 Morde Simulator 截图 `/tmp/MDA-04-task-footer-overlay-attachments-20261107/2E4A911D-4A86-4888-82E7-0B159ADBA2CE.png` 对照：返回/标题和底部固定操作区域位置基本对应；Morde 显示真实 Task 状态和两条步骤，不补造经理信息；保留的暂停/取消两项真实动作遵照用户审核，因此数量、颜色及文案仍与原型单一红色“急停”不同。该截图使用合成 Task/API，只证明页面构成，不代表真实 Portal 数据视觉。
- Morde 详情比 HTML 多内联事件历史与交付物区；前者已有独立事件链页，后者当前仅显示文件名/类型。已在待审核表记录用途和移除影响；答复前不改。MDA-04 整页仍未完成，也不勾选。

### 2026-11-07 · MDA-02 聊天输入和消息样式对齐

- 依据 HTML `chat` 首屏与已审核的聊天长按菜单决定，移除输入区常驻附件“＋”和麦克风；附件、语音、Task options 继续由现有长按菜单提供。空输入仍在发送位显示黑色箭头，点按聚焦输入框。此轮将助手页头保留为 Morde 品牌、用户消息保留 nanoMuse 用户色气泡；助手逐条发言标识及去气泡样式于 2026-11-08 复核后按原型补齐，见下文。消息内容、附件 chip、队列合同和失败恢复逻辑未改。
- `testMordeChatMultiTurnVisualFixtureMatchesDesignDensity`、`testMordeChatLongPressActionsOpenTaskOptionsAndImporter` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 2/2（`/tmp/MDA02-chat-design-final-20261107.xcresult`）。前者断言发送位存在且常驻附件/语音控件不存在，并导出截图 `/tmp/MDA02-chat-design-final-attachments-20261107/9A942CEF-B47F-4512-9640-9B7EAB29022D.png`；后者确认 Task options、附件导入仍能从长按菜单打开。截图仅使用本地合成消息/文件。
- MDA-02 页面仍未完成：Task 卡详情跳转待审核，附件/键盘/滚动及其他消息状态仍需逐页视觉核验；本次 2 项定向通过不作为整页完成依据。

### 2026-11-08 · MDA-02 聊天消息密度与排版复核

- 按本地 HTML `.chat` / `.msg` 样式对齐：消息列间距 18，左右内边距 22、顶部 10、底部 16；正文 15.5 pt、字距 -0.15、行距 6；紧凑宽度用户气泡以 52 pt 右侧留白匹配 85% 最大宽度，常规宽度使用 102 pt。助手消息改为无气泡正文，并在每条回复前显示 13 pt “Morde” 标识；用户气泡继续采用 nanoMuse 蓝色以保持已确定的产品视觉风格。移除此前试验的内容宽度测量状态，避免文本较短时滚动视图宽度塌缩。
- `testMordeChatMultiTurnVisualFixtureMatchesDesignDensity` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1，结果 `/tmp/MDA-mda15-workspace-route-derived-20261107/Logs/Test/Test-Morde-2026.11.08_00-22-07-+0800.xcresult`；截图 `/tmp/MDA02-chat-aligned-final-attachments-20261108/69E21A7F-8DA8-456F-A2BE-2FBE57CECEFC.png` 已检查，七条往返消息、逐条发言标签、输入框和底部 Tab 均可见。
- SwiftUI 行距与 CSS `line-height: 1.6` 并非完全相同的像素实现；本次检查覆盖消息密度和首屏可见性，不代表逐像素对照或其他键盘、附件、滚动状态完成。MDA-02 仍不勾选。

### 2026-11-08 · MDA-02 输入焦点与软键盘截图边界

- 复查既有 `testMordeEmptyComposerSendFocusesInput` 截图 `/tmp/MDA-02-chat-visual-final-attachments/AC4AF7E6-F002-4C86-9CB8-73A1D6031FE2.png`：点按原发送位置后输入框获得焦点，截图只看到“完成”附件栏，没有软键盘按键区。测试断言 `app.keyboards` 存在，证明有键盘焦点/键盘元素，但截图不足以确认软件键盘弹出时的输入栏与底部 Tab 避让。
- 尝试通过本机 Simulator 图形应用复核软键盘，但当前 Xcode 安装没有可打开的 `Simulator.app`；因此键盘可视布局保留未验收，不据此改动布局。该限制不影响聊天静态首屏及长按菜单截图证据。

### 2026-11-08 · MDA-02 聊天原型复核与输入/消息样式修正

- 在内置浏览器通过 `127.0.0.1` 打开本地 `docs/Morde iOS设计稿.html` 并进入聊天页，确认每条助手回复前有“Morde”、助手正文无底色气泡、用户消息独立右对齐气泡、空输入栏显示发送箭头。按此修正 Morde：移除常驻麦克风，用可点按聚焦输入框的发送位；语音与附件仍从已审核的长按菜单进入；助手回复显示“Morde”署名且取消灰色气泡，用户消息继续保留 nanoMuse 蓝色。
- `testMordeEmptyComposerSendFocusesInput` 与 `testMordeChatMultiTurnVisualFixtureMatchesDesignDensity` 在 iPhone 18 Pro / iOS 27.0 Simulator 使用全新 DerivedData 通过 2/2，最终结果 `/tmp/MDA02-chat-final-confirm-20261108.xcresult`。两个用例使用隔离的本地测试账号，避免已有 Simulator 登录状态污染；空输入截图 `/tmp/MDA02-chat-final-confirm-attachments-20261108/AEA0CF5A-10FB-4B6A-AE76-57BC2248F7B5.png` 可见软键盘及底部 Tab 避让，多轮截图 `/tmp/MDA02-chat-final-confirm-attachments-20261108/CA80B45D-1C8E-42BF-9FB1-82451BEF1BD2.png` 已检查，助手逐条署名且无气泡。此结果补充并更新上一节的键盘截图边界。
- MDA-02 仍不勾选：Task 卡额外详情跳转仍待审核；附件其它状态、滚动/多尺寸键盘、真实数据库集成和整页逐控件视觉验收仍未闭环。

### 2026-11-08 · MDA-03 多行列表与步骤计数截图复核

- 重新检查多行任务列表截图 `/tmp/MDA-03-visual-attachments-20261107/713EC29F-375D-48F0-A77E-C366F5A12BB9.png` 和步骤计数截图 `/tmp/MDA-03-task-step-counts-attachments/C1EAF426-3C5A-456A-805C-28C608A879AE.png`。多行列表保持无卡片/分隔线、状态图标和整行布局；独立计数用例显示真实合同字段“5 执行步骤 · 2 已完成”，且未显示预算字段。
- 两张图使用不同合成夹具：多行图没有伪造步骤数，计数图只有一行；真实列表当前依 Task API 有无步骤摘要显示对应次级内容。此轮只核对现有证据，没有改变 Task 数据或界面；不同状态的完整页面截图仍待补齐，MDA-03 不勾选。

### 2026-11-08 · MDA-04 三处加载 Retry 决定与底部操作复核

- 用户再次确认：删除详情读取失败、事件流失败、未确认事件历史三处条件式“重试加载”，返回后重新进入详情触发读取。该决定已有 Morde Simulator 3/3 及未确认历史返回重进 1/1 证据；现有实现不增加 Retry，事件流继续自动重连。
- 复查已验收截图 `/tmp/MDA-04-task-footer-overlay-attachments-20261107/2E4A911D-4A86-4888-82E7-0B159ADBA2CE.png`：暂停/取消仍固定在 Tab 栏上方的底部操作区；与 HTML 单个红色“急停”不同，是用户已审核保留真实操作后的差异。该截图不代表所有 Task 状态已完成视觉核对；MDA-04 仍不勾选。

### 2026-11-07 · MDA-14 系统级 Workspace 分享链接唤起复核

- 在 iPhone 18 Pro / iOS 27.0 Simulator 通过 `simctl openurl` 打开合成 `morde://workspace-share?token=...` 链接，iOS 弹出系统确认框“在 ‘morde’ 中打开？”，证明已安装 Morde 的 `morde` scheme 注册有效。
- 这次初验只确认系统提示，未点按“打开”；后续已通过 SpringBoard UI 自动化点击确认并完成领取，详见文末复验记录。手动 `simctl launch` 不能建立 UI 测试的进程内 URLProtocol 登录夹具，停在登录页；未连接 Portal、使用真实账号或执行领取。

### 2026-11-07 · MDA-15 旧版 Workbench 移动壳 Simulator 复验

- `testMordeComputerWebViewAlignsLegacyWorkbenchToDesignOnDevice` 在独立 Morde target、`MDA-05 Fresh` / iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA15-legacy-workbench-review2-20261107.xcresult`）。用内置 legacy HTML fixture 验证原生页头、蓝色移动壳、浏览器/终端/文件三入口，以及续租、Host 到期倒计时、控制租约倒计时、版本和长说明在 iOS 页面均不可见；最终截图 `/tmp/MDA15-legacy-workbench-review2-attachments-20261107/1D7C8EF5-799B-43D0-9D79-B1F896D35B77.png` 已检查。
- 首次复跑因测试账号偏好里存有旧服务地址，覆盖了启动传入的 loopback fixture，DNS `-3003` 导致未进入页面。现 DEBUG 启动按显式 `-morde.server` 参数选择服务地址，空值也保留；不清理/改写持久化的用户偏好。修复后同设备测试通过。此测试只验移动端布局和旧 DOM 收敛，不模拟真实桌面内容或真实 Host 交互；MDA-15 继续未勾选。


### 2026-11-07 · MDA-14 系统级 Workspace 分享链接唤起通过

- 在 `MDA-05 Fresh` / iPhone 18 Pro / iOS 27.0 Simulator 运行一次性 UI 验收用例，同时从宿主机调用 `simctl openurl` 打开合成 `morde://workspace-share?token=...`。SpringBoard 呈现系统“在 morde 中打开？”确认框；测试点按“打开”后，Morde 的 `.onOpenURL` 收到链接并显示分享预览、只读权限和独立副本说明。
- 继续完成本地合成 API 领取，Workspace 列表出现新副本 `w0000004`；该项 1/1 通过，结果 `/tmp/MDA14-external-share-deeplink-confirm-20261107.xcresult`。没有连接 Portal、真实账号或传输文件。专用一次性协调测试未留在默认全量 UI 套件中，以免无外部链接时挂起；既有直接领取流程测试仍保留。
- 这一结果关闭系统级外部唤起的验收缺口。manifest 不安全路径、文件/目录冲突、重复游标随后 3/3 通过（`/tmp/MDA14-manifest-edges-20261107.xcresult`）；Workspace 其他权限与错误状态和页面视觉仍待完成，因此 MDA-14 继续进行中。


### 2026-11-07 · MDA-14 Workspace 清单短错误态下拉恢复

- 首次 manifest 请求通过本地 fixture 返回 503，Workspace 页面不显示独立 Retry；首次下拉没有触发刷新，定位到清单错误态内容短于视口，垂直滚动范围不足。滚动内容现在在短空态/错误态至少填满可用高度，使 `.refreshable` 仍可用，不增加页面按钮。
- `testMordeWorkspaceManifestFailureRecoversByPullToRefresh` 在 `MDA-05 Fresh` / iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA14-manifest-refresh-safearea-fix-20261107.xcresult`）：下拉后 README 文件行恢复、错误状态消失、返回入口保留。只使用本地 API fixture，没有连接真实 Portal 或传输文件。
- `testMordeWorkspaceIgnoresLateManifestPageAfterDirectoryChange` 在 `MDA-05 Fresh` / iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA14-late-manifest-page-retry2-20261107.xcresult`）：根目录游标页延迟返回期间切换到 `docs`，迟到的根目录文件没有混入新目录，也未覆盖当前目录状态。仅使用本地 API fixture。
- `testMordeReadOnlyWorkspaceDisablesCloudMutations` 扩展后在 `MDA-05 Fresh` / iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA14-unmounted-readonly-20261107.xcresult`）：资源行显示“未挂载”；打开后仍能查看 README 清单，挂载摘要不虚构 Host；只读副本允许下载，重命名/移动/删除云端文件保持禁用。截图 `/tmp/MDA14-unmounted-readonly-attachments-20261107/E115AD2A-E6FD-4A0E-AAC8-E52D35466F1C.png` 已检查；使用合成资源和文件。
- 新增本地 403 manifest fixture；`testMordeWorkspacePermissionDeniedKeepsRowsHiddenAndReturnsToResources` 在同一设备通过 1/1（`/tmp/MDA14-workspace-forbidden-20261107.xcresult`）：拒绝信息显示为清单错误，不误显示空态/文件行/Retry，返回资源页可恢复；截图 `/tmp/MDA14-workspace-forbidden-attachments-20261107/B3207529-EBE7-4D34-8589-2CE4916129FC.png` 已检查。只使用合成 API 响应。
- 内置浏览器打开本地 HTML `ws` 页，实测进入 `pay` 目录、通过面包屑返回根目录、打开分享面板并在“可查看/可编辑”间切换。未点击“复制分享链接”，没有改动剪贴板；没有向外分享任何内容。
- 未挂载只读场景、服务端能力关闭和 Workspace manifest 403 均有证据；根页/分享弹层已作局部视觉对照，行菜单、空目录与长列表滚动截图已补；其余状态完整视觉仍待完成；MDA-14 保持进行中。

### 2026-11-07 · MDA-14 根页和分享弹层截图对照

- `testMordeResourceWorkspaceRouteBrowsesSyntheticManifest` 在 `MDA-05 Fresh` / iPhone 18 Pro / iOS 27.0 Morde Simulator 最终复验通过 1/1（`/tmp/MDA14-workspace-root-visual-20261107.xcresult`），新增根页截图 `/tmp/MDA14-workspace-root-visual-attachments-20261107/1D66580D-F344-48A9-B452-9B6A6F45C3EF.png`。`testMordeWorkspaceShareCreatesLinkWithSelectedPermission` 在新增弹层截图后通过 1/1（`/tmp/MDA14-workspace-share-visual-final-20261107.xcresult`），截图 `/tmp/MDA14-workspace-share-visual-final-attachments-20261107/63342372-414F-40BF-9213-F929013DCADF.png`；创建链接仍使用本地合成 API。
- 对照 HTML `ws` 根页和分享面板：返回/标题/分享按钮、挂载摘要、次级 Workspace 标题、目录/文件行与四个底部 Tab 的结构对应；设计稿示例容量因 API 无真实数据未仿造，文件操作菜单依用户审核决定保留。分享面板的查看/编辑选项与复制按钮位置已核对；原生操作按钮偏离设计稿过高，现将分享 sheet 从 260pt 改为 228pt，底部 padding 从 24pt 调为 8pt。NanoMuse 蓝色原生按钮风格保留。
- 2026-11-07 本轮在内置浏览器再次实际操作本地 HTML 原型：从 `ws` 根页打开 `pay` 后显示子目录文件，通过面包屑返回后恢复根目录列表；打开分享面板，在“可编辑/可查看”间切换并确认选中态随之变化。没有点击复制链接、改动剪贴板或外传内容。此项只验证设计原型交互，不替代 Morde 产品的 Simulator 验收。空目录、行菜单和长列表滚动截图已有本地证据；Workspace 其他权限/错误态的完整视觉仍待验收。

### 2026-11-07 · MDA-14 空目录与文件操作菜单截图复核

- `testMordeResourceWorkspaceRouteBrowsesSyntheticManifest` 与 `testMordeWorkspaceFileActionsStayInRowMenu` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 串行通过 2/2（`/tmp/MDA14-workspace-empty-rowmenu-visual-20261107.xcresult`）。根目录、挂载/同步资源行、空目录和文件行菜单截图均已导出；本轮新增空目录 `/tmp/MDA14-workspace-empty-rowmenu-attachments-final-20261107/0AE64FEA-AC30-46FD-AEB8-D3B72747FF85.png`、文件菜单 `/tmp/MDA14-workspace-empty-rowmenu-attachments-final-20261107/C13A3F34-4585-4FF9-9CDB-1C04E74B8F2D.png`。
- 对照设计稿 `ws` 页源码：固定返回/标题/分享区域、挂载摘要、路径、平铺文件/目录行和底部导航结构一致；空目录保留路径与空态提示。设计稿未画出文件操作菜单，已按用户审核决定保留在文件行的菜单中，不新增常驻按钮。截图确认菜单未改变主页面布局；长列表另由本地合成 manifest 覆盖。

### 2026-11-07 · MDA-14 长文件列表滚动与底部导航

- 新增 24 个文件的本地 manifest fixture；`testMordeWorkspaceLongListScrollKeepsLastRowAboveBottomNavigation` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA14-workspace-long-list-20261107.xcresult`）：第 24 行可滚动到达，且末行完整位于浮动底部导航上方。截图 `/tmp/MDA14-workspace-long-list-attachments-20261107/344974A5-E41D-4D0F-B7BB-9D2CB0E1D29B.png` 已查看；只使用合成文件名与内容。

### 2026-11-07 · MDA-14 Workspace 能力关闭态可达性

- 发现服务端未声明 `workspace_sync` 时，资源页会禁用已就绪 Workspace 行，导致 Workspace 页自身已有的能力说明不可达。现改为已就绪 Workspace 可打开；manifest 请求、上传和下载仍由原能力门禁阻止，不显示真实文件行或操作入口。
- `testMordeWorkspaceExplainsWhenServerCapabilityIsUnavailable` 在 Morde Simulator / iPhone 18 Pro / iOS 27.0 最终复验通过 1/1（`/tmp/MDA14-capability-disabled-final-20261107.xcresult`）。截图 `/tmp/MDA14-capability-disabled-final-attachments-20261107/CA597A49-964A-401B-8AFD-20397D580F5F.png` 已查看：能力说明、返回按钮与底部导航均可见，没有 README 或文件操作菜单。仅用本地合成 API。
- Workspace 空目录、文件操作和长列表滚动已有行为验收及截图对照；其余权限/错误态的完整视觉核验仍未完成；MDA-14 继续进行中。

### 2026-11-07 · MDA-15 关闭中断 Host 票据并验证权限入口

- 新增本地延迟 `/api/hosts/h0000001/enter` 的 UI fixture。`testMordeComputerCloseCancelsPendingHostTicketWithoutReopeningWebView` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 与权限入口用例串行通过 2/2（`/tmp/MDA15-close-permissions-20261107.xcresult`）：票据未返回时关闭云电脑，回到资源页；等待超过延迟窗口后没有迟到 WebView/Host sheet 重开。运行中共享 Host 缺 `desktop:view` 时入口禁用；已停止且无权限 Host 显示真实停止状态，不显示权限拒绝文案。截图 `/tmp/MDA15-close-permissions-attachments-20261107/4871B7CA-0F80-4F73-9231-46707D38F19C.png` 已查看。只使用本机 API fixture，不连接 Gateway 或真实 Host。
- 这项补齐关闭/权限入口边界，不证明票据 WebView 中真实 Gateway 桌面、终端和文件内容已对齐。MDA-15 继续进行中，仍待授权 Host 的端到端能力核验与完整设计稿逐控件视觉比对。

### 2026-11-07 · MDA-15 共享成员云电脑控制与隐式续租

- 按用户已确认的规则，分享给接收者的 `desktop:control` 权限保持长期有效，直至所有者撤销。Morde iOS 现在按共享 Host 的真实权限显示接管/交还入口；控制状态、接管、交还和续租均通过既有 Portal Session cookie 请求，不新增控制 API。服务端控制授权只接受 Session principal；查看要求 `desktop:view` 或 `desktop:control`，控制/续租严格要求 `desktop:control`，终端/MCP 权限不再被误当作桌面控制权限。
- 云电脑页面内不显示续租按钮或租约倒计时。桌面已加载且当前用户持有活动控制租约时，页面每 10 秒静默续租；离开页面后停止续租，原 30 秒服务端租约栅栏继续负责实际输入的安全性。持续共享权限与某一次活动控制租约是两层状态：关闭页面可让活动租约到期，但不会删除接收者的长期共享权限。
- 增加本地 `desktop:control` 共享 Host fixture，要求控制状态/动作/续租带 Portal Session cookie，并要求接管后先自动续租再交还。`testMordeSharedHostDesktopControlMemberCanTakeOverAndRelease` 在 `MDA-05 Fresh` / iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA15-shared-control-20261107-final.xcresult`）：加载本地 Workbench fixture 后共享成员可接管；后台续租不替换可见操作按钮；交还成功；界面没有续租按钮或剩余时间文案。截图 `/tmp/MDA15-shared-control-20261107-final-attachments/8AA93D43-572B-406C-961E-449DB0D10417.png` 已检查。仅用本机 API/Workbench fixture。
- Morde iOS Simulator `xcodebuild ... build` 以 generic iOS Simulator 目标编译成功（退出码 0）；输出含项目已有的 ObservableObject 主 actor 隔离及未使用结果警告。`server/internal/app` 的 `go build` 成功。没有运行测试、连接真实 Portal、Gateway 或云端电脑。
- 共享控制权限和静默续租路径已通过本地 Simulator 验收；真实 Gateway 内容和逐控件视觉验收仍未完成，MDA-15 保持进行中。

### 2026-11-07 · MDA-08 创建成功筛选恢复与提示位置

- 按本地 HTML `work` 原型成功反馈落实：创建工作项成功后关闭表单、切回“全部”筛选，使新建项立刻可见，并显示“已创建 · 团队可见”短暂提示。提示浮在底部 Tab 上方，避免被导航遮挡；列表/创建权限、API 调用和用户审核中的描述、项目/Task 选择及行操作保持原样。
- `testMordeWorkItemCreateSubmitsToSelectedProject` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA08-create-success-derived2/Logs/Test/Test-Morde-2026.11.07_22-43-24-+0800.xcresult`）：先选“缺陷”筛选，再创建需求项；用例确认筛选回到“全部”、项目内出现新建项、描述保留、sheet 关闭、提示可访问且显示在 Tab 栏上方。截图 `/tmp/MDA08-create-success-final-attachments2/D5B92D14-1C60-4481-AC93-F0312CE58223.png` 已查看。仅使用本地 API fixture。
- MDA-08 继续未勾选：项目/会话/Task 选择、描述、额外行操作及待认领条件仍在审核表；键盘/行操作专项状态与工作项页、创建 sheet 的完整逐页视觉核对尚未完成。

### 2026-11-07 · MDA-04 Task 详情 Retry 决定复核与截图对照

- 用户确认移除详情请求失败、事件流失败、事件历史未确认三处“重试加载”，通过返回后重进详情重新读取；这一决定已落实到页面和验收用例，事件流自动重连、错误说明及失败批次项的业务“重试”保留。三种恢复路径 3/3 及未确认历史返回重进 1/1 的 Simulator 证据见上方 MDA-04 执行记录，本次答复关闭该待决项。
- 将本机 HTML `task` 页面与 Morde Simulator 详情截图 `/tmp/MDA04-footer-46pt-attachments-final2-20261107/B2CC2A4E-FD29-4048-B230-07A22B5FD461.png` 并排复核。返回/标题、步骤主列和底部固定操作区结构对应；Morde 使用真实三步数据（原型示例为四步），显示真实状态、内联活动/交付物区，底部保留两个真实暂停/取消入口。暂停/取消保留已审核；经理元数据与独立急停动作无真实 API；内联活动/交付物的去留仍按审核表等待决定，不伪造或擅删。此截图仅覆盖一个运行中 Task fixture，不能代替其他终态、空/错误/审批状态以及完整组件尺寸核对。
- MDA-04 保持未勾选：其余异常/返回草稿状态和逐控件完整视觉验收仍待完成。

### 2026-11-07 · MDA-06/07 审批与 Workspace 视觉细节复核

- 对照本地 HTML CSS 后，将 Morde 审批事实文字设置为 `line-height:1.65` 对应的行距和 6pt 项间距；Workspace 容量摘要设为 14pt，对应 HTML `.row .tt span` 的 14px。没有增加或移除按钮。
- `testMordeApprovalInboxShowsOnlyConfirmedDecisions`、`testMordeApprovalInboxShowsUnconfirmedStateWhenTaskListFails`、`testMordeResourceWorkspaceRouteBrowsesSyntheticManifest`、`testMordeSharedHostDesktopControlMemberCanTakeOverAndRelease` 在 iPhone 18 Pro / iOS 27.0 独立 Morde Simulator 串行通过 4/4，结果 `/tmp/MDA-approval-resource-alignment-derived/Logs/Test/Test-Morde-2026.11.07_22-54-37-+0800.xcresult`。已查看审批确认态 `/tmp/MDA-approval-resource-alignment-attachments/FF4F8B52-2C45-4C96-9248-9DF8EBE3FCD6.png` 和 Workspace 根目录 `/tmp/MDA-approval-resource-alignment-attachments/922A0084-BB46-4289-AF31-CF3105434986.png`；截图均为合成 API 数据。
- 审批第三动作、自动处理历史和媒体直达预览行为仍待用户审核；MDA-07 Host 操作者/租约等额外文字仍待审核。此次只是局部字体和状态回归，两页完整逐控件视觉验收仍未完成，MDA-06/07 保持未勾选。

### 2026-11-07 · MDA-02 本机临时 PostgreSQL 队列集成复验

- 本机 Docker 已有 `postgres:16.15-bookworm` 镜像；用专用容器 `mda02-it-pg`、回环端口 `127.0.0.1:55438` 和临时内存卷运行集成验收。每个 Go 测试由 `newTemporaryStore` 创建并删除独立数据库；结束时确认遗留测试库数量为 0，停止并移除了专用容器，没有使用既有服务数据库、真实账号或线上服务。
- 定向测试 `TestConversationAttachmentAssociationGatesOnlyItsMessage`、`TestConversationRouteInitialAttachmentIsDeliveredOnceAfterStartupRetry`、`TestPostgresAgentCheckpointAppliesOnlyMatchingRootMessages`、`TestPostgresConversationReceiptsAcrossTaskStates`、`TestPostgresPendingConversationMessagesBlockTaskCompletion` 首轮 4/5 通过，初始附件启动重试出现 checkpoint 未持久确认错误；单项复跑 1/1 通过，随后五项串行复跑 5/5 通过。
- 为检查间歇性，再以 `-count=3` 重跑启动重试用例：3 次中 2 次通过、1 次失败，错误为 `task_runtime_unavailable`。失败原因尚未定位，因此这些测试证明选定数据库路径曾完整通过，但不构成可重复稳定的恢复证据；MDA-02 继续未勾选，需先查明恢复用例间歇失败，再补其余实际 DB 覆盖。附件队列 API/worker/Runtime 的代码和 UI 合成验收结论不变。

### 2026-11-07 · MDA-02 启动重试用例重复复验

- 使用新建的本机临时 PostgreSQL 容器和临时内存卷复跑 `TestConversationRouteInitialAttachmentIsDeliveredOnceAfterStartupRetry`：`-count=3` 的 6 个子用例全部通过，随后 `-count=10` 的 20 个子用例全部通过。此前记录的 `task_runtime_unavailable` 本轮未能复现；没有修改运行时代码，因此不能把该历史间歇失败标记为已修复，根因仍未知。
- 测试结束确认 `morde_test_*` 临时数据库数量为 0，并停止、移除专用容器。仅验证本地数据库路径；MDA-02 继续未勾选，其他附件错误/权限状态和聊天页逐控件视觉验收仍待完成。


### 2026-11-07 · MDA-15 云电脑文件入口按 HTML 原型映射

- 重新在本地内置浏览器实际操作 `Morde iOS设计稿.html`：资源页进入 PC-02、接管后点云电脑里的“文件”，原型打开当前 Host 绑定的“归档盘” Workspace。此前清单把该入口记成主机文件，现已更正；这是基于原型实测的修订。
- Morde iOS 的设计稿主栏“文件”现在通过同源、主 frame 的 WKWebView 消息桥打开该自有 Host 绑定的原生 Workspace，并从 Workspace 的“返回资源”返回资源页；Host 文件浏览保留在 Workbench 长按/菜单的“主机文件”项。没有增加常驻按钮。
- `testMordeComputerWebViewAlignsLegacyWorkbenchToDesignOnDevice` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1；覆盖点“文件”进入绑定 Workspace、README 行可见、返回资源，结果 `/tmp/MDA-mda15-workspace-route-derived-20261107/Logs/Test/Test-Morde-2026.11.07_23-40-19-+0800.xcresult`。Workspace 根目录截图 `/tmp/MDA15-workspace-route-attachments-20261107/D3256B2F-503D-4493-8230-08607AB01423.png` 已查看。Workbench 移动菜单测试 14/14、typecheck 与 lint 通过；“主机文件”仍只在长按/上下文菜单中。全程使用本地原型和合成 Host/Workspace 数据。
- 该 UI 测试用本地 WKWebView fixture，不能证明真实 Gateway/WebView 页面加载和远程文件交互；共享 Host 没有可用的 Workspace binding 时仍走 Host 文件页。没有连接真实 Portal/Gateway、上传或发送文件。MDA-15 继续未勾选，待真实 Host 与完整逐控件视觉验收。

### 2026-11-08 · MDA-04 Retry 回归与 MDA-11/12 文件行排版

- 用户确认移除 Task 详情请求失败、事件流失败、未确认事件历史三处“重试加载”；本轮独立 Morde Simulator 三项通过 3/3，结果 `/tmp/MDA-followup-review-20261108.xcresult`。返回后重新进入会重新读取；事件流自动重连、错误说明和失败批次的业务“重试”继续保留。
- 文件库行标题加入 `-0.16pt` 字距，标题/元数据间距由 3pt 调整为 2pt，14pt 元数据增加 3.5pt 行距，分别对齐 HTML `.row .tt b` 的 `letter-spacing:-.01em`、`.row .tt span` 的 `line-height:1.45` 和 2px 上边距。该共享视图文件已确认属于 Morde target。`testMordeFullLayoutUploadsAndSendsImageAndPDFFixture` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1，结果 `/tmp/MDA-library-style-and-attachment-repeat-20261108.xcresult`；已查看截图 `/tmp/MDA-library-style-attachments-20261108/4EE97027-0F60-4D2B-A7BC-89121003254F.png`。本次复核发现附件-only fixture 没有关联 Task，旧测试却等待 Task 摘要卡；截图显示图片/PDF 文件卡和助手已接收回复，故将断言改为等待该真实回复，没有改动卡片行为。Task 摘要卡是否保留/跳转仍等待用户选择。
- 这是局部排版与恢复路径回归，MDA-04 页面仍需完整状态/尺寸视觉验收；MDA-11/12 的其他错误/权限状态、Quick Look 结构差异及逐页视觉也未闭环，以上页面仍保持未勾选。

### 2026-11-08 · MDA-06/07/08 细节对照

- 审批标题加入 `-0.175pt` 字距，对应 HTML `17.5px` 标题的 `letter-spacing:-.01em`。工作项筛选器上下间距改为顶部 8pt、底部 2pt；工作项标题加入 `-0.16pt` 字距、14pt 元数据增加 3.5pt 行距，对应 HTML `.seg` 和 `.row .tt` 样式。两处共享视图都已确认纳入独立 Morde target。
- `testMordeApprovalInboxShowsOnlyConfirmedDecisions` 与 `testMordeWorkItemsRouteShowsDesignFiltersAndCreateForm` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 同轮通过 2/2，结果 `/tmp/MDA-approval-workitem-style-20261108.xcresult`。截图 `/tmp/MDA-approval-workitem-style-attachments-20261108/93BD18C7-62E2-4B43-BA2C-CB45AA8294E3.png`、`/tmp/MDA-approval-workitem-style-attachments-20261108/887C2932-2688-4485-AEB2-9432FF41D2A3.png` 已查看。工作项截图仍显示项目/会话/Task 选择、额外描述和阻塞操作；这些真实入口均在审核表中，答复前不删除或迁移。
- MDA-07 只读复核确认资源页基本字号/尺寸已接近 HTML，Workspace 容量/权限字段有真实 API 来源；Host 行目前没有具名操作者，只显示控制状态。已把待审核表中的错误“操作者”更正为“控制状态”。现有资源截图 `/tmp/MDA07-control-error-attachments-20261107/FC0B7056-9705-4740-8D05-38A8BBA3D034.png` 已复查；Host 额外状态及行尾交还按钮的完整显示条件仍需审核/直接断言，云手机与 AI 工具没有可用的实时状态合同，未造数据。
- 以上只是字体、间距和需求描述校正。MDA-06/07/08 的额外行为决定、剩余状态覆盖及完整逐控件截图核验仍未完成，页面任务继续未勾选。

### 2026-11-08 · MDA-03 任务行排版与资源/任务状态复验

- MDA-07 资源 Host 图标由 20pt 调整为 21pt，Host/Workspace 标题加 `-0.16pt` 字距，摘要增加 3.5pt 行距，主状态文字改为动态 tertiary 灰度；没有改动租约、控制、错误、交还或点击行为。`testMordeResourceWorkspaceRouteBrowsesSyntheticManifest` 与 `testMordeResourceShowsHostControlErrorOnce` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 2/2，结果 `/tmp/MDA-resource-style-20261108.xcresult`；Host 错误截图 `/tmp/MDA-resource-style-attachments-20261108/6692551A-8936-447D-BDD3-C373713E262E.png` 已检查。Host 行额外状态是否收敛仍按待审核项处理。
- MDA-03 任务标题加 `-0.16pt` 字距，次级内容固定为 14pt 并加 3.5pt 行距，状态固定为 13pt 并补 1pt 顶部间距，符合原型 `.row .tt` 字号与节奏；不改变红叉失败态或任务数据。`testMordeTaskListShowsStepCountsFromSummary` 和 `testMordeTaskListRendersKnownAndUnknownStatusesWithoutActions` 串行通过 2/2，结果 `/tmp/MDA-task-style-after-isolation-20261108.xcresult`；步骤截图 `/tmp/MDA-task-style-attachments-20261108/E6985823-9838-4B89-947F-67959BC05352.png`、八状态截图 `/tmp/MDA-task-style-attachments-20261108/A2EDB06F-1288-4AD0-A37F-E5A0E43ED8CC.png` 已检查。首次在复用的 `MDA-05 Fresh` 状态下运行时，状态用例未进入 Tasks 路由；新 iPhone 18 Pro 上单项通过，随后让该状态矩阵用例先退出/重登本机合成账号并断言 Tasks 选中后，复跑组合 2/2。失败属于测试账号/路由状态隔离，未发现任务页排版导致的功能回归。
- MDA-03 的 iOS 16.0–16.3 回弹仍缺对应运行时，列表其他状态与完整设计稿尺寸对照未闭环；MDA-07 Host 额外控制/租约文字及交还按钮显示条件仍待审核。两页均保持未勾选。

### 2026-11-08 · MDA-04 三处“重试加载”最终确认

- 用户确认移除详情读取失败、事件流失败、事件历史未确认时的三处“重试加载”按钮；返回后重新进入详情触发重读，事件流保留自动重连，失败批次的业务“重试”继续保留。
- 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 重新运行 `testMordeTaskDetailShowsPermissionErrorWithoutTaskActions`、`testMordeTaskDetailTimelineFailureHasNoRetryButtonAndRecoversAutomatically`、`testMordeTaskDetailUnconfirmedHistoryHasNoRetryButton`，3/3 通过，结果 `/tmp/MDA-task-retry-confirm-20261108.xcresult`。用户于 2026-11-08 再次确认后复跑同三项，仍为 3/3 通过，结果 `/tmp/MDA04-reentry-user-approval-20261108.xcresult`。覆盖详情读失败重进恢复、事件自动重连和未确认历史重进后重新读取；三种状态均没有 Retry 按钮。
- 用内置浏览器重新打开本机 HTML 的 `task` 页面，与 Simulator 恢复态截图对照。原型为单个红色“急停这个任务”；Morde 将真实取消操作固定在 Tab 栏上方，符合已审核的保留现有操作决定；截图 `/tmp/MDA-task-retry-confirm-attachments-20261108/ED7E6F73-8F0D-46F4-862D-7E940894FD63.png` 已查看。详情的真实状态、步骤、澄清、事件历史和交付物区仍比原型多，按待审核表保留，未擅自删除。
- 用户本轮再次确认移除这三处按钮。当前页面和回归结果已符合决定；此项保持关闭，不增加或恢复独立重试入口。
- 此决定项已关闭；MDA-04 仍未勾选，因其他状态及整页视觉核验尚未完成。

### 2026-11-08 · MDA-15 云电脑壳与 Host Task 停止复验

- 内置浏览器打开本地 HTML 原型，按“资源 → 云电脑 PC-01 → 接管控制权 → 打开 Workspace 文件”操作；验证控制状态切换、文件入口进入绑定 Workspace 和文件列表显示。该原型只验证设计流程，没有访问真实 Host。
- 独立 Morde Simulator 的 `testMordeComputerWebViewAlignsLegacyWorkbenchToDesignOnDevice` 与 `testMordeSharedHostDesktopControlMemberCanTakeOverAndRelease` 首次回归通过 2/2；首轮截图 `/tmp/MDA15-current-recheck-attachments-20261108/9298CD1C-F55A-4BCC-9F9A-D924B9B0878E.png`、`D39D3577-83B8-4F32-8652-571A70B63DEC.png`、`EE77F2C1-338E-4EC3-A277-428536027A9F.png` 已检查。移动端没有续租按钮、Host/租约倒计时或版本/长说明；入口收敛为浏览器、终端、文件；共享成员静默续租后仍只显示交还控制。首轮图标截图由后续修订替代，见下方 `MDA-15 云电脑图标与页面状态复核`。
- 同一批 `testMordeComputerStopsTheActiveTaskBoundToItsHost` 因复用 Simulator 登录账号而没有初始化主会话 Task fixture，按钮按真实当前状态正确禁用，不能据此证明产品失败。已让该用例退出并重登合成账号，单独复验 1/1 通过（`/tmp/MDA15-stop-task-fixture-isolated-20261108.xcresult`）：Stop Task 取消 Host 匹配的活动 Task，之后在同一 Task 详情看到已取消。前后截图 `/tmp/MDA15-stop-task-attachments-20261108/949E33EE-0069-4106-BC7A-91C898BE576B.png`、`867DE15A-86D0-4570-BBE4-82BC6DFC96F7.png` 已检查。
- 三条 Morde 回归现分别通过（前两项在 `/tmp/MDA15-current-recheck-20261108.xcresult`，Stop Task 隔离复验见上）；结果仍仅来自本地合成 API/Workbench fixture。真实 Gateway 页面及逐控件全状态视觉尚未验收，MDA-15 保持未勾选。
- 按 HTML `.pc-title` 将原生云电脑标题改为 19pt Bold、-0.19pt 字距，并把标题/副标题间距从 2pt 调到 3pt；副标题不透明度从 65% 调至约 58%，匹配原型 `#9A9AA0`。未改按钮、导航、控制租约或 Host 文案。`testMordeComputerWebViewAlignsLegacyWorkbenchToDesignOnDevice` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1，结果 `/tmp/MDA15-title-weight-20261108.xcresult`；截图 `/tmp/MDA15-title-weight-attachments-20261108/E910170B-3DE4-42A5-B334-ED14D09F60C9.png` 已查看，页头与原型位置一致、单行副标题未挤压，文件入口仍进入绑定 Workspace 并可返回资源。测试使用本机合成 Host 与 Workbench fixture；实际 Gateway 页面仍需真实授权环境验收。

### 2026-11-08 · MDA-14 Workspace 容量摘要补齐

- Workspace API 已提供真实 `quota_bytes` 与 `used_bytes`，详情页此前仅显示挂载状态和 Host，漏了设计稿同一行中的容量及已用比例。Morde 现在复用资源列表的 `.memory` 字节格式、整数四舍五入和 0–100% 夹紧逻辑：已挂载摘要为“状态 · 云电脑 Host · 容量 · 已用比例”，未挂载时保留状态并补容量；缺少有效配额时不显示伪造数字。没有增加按钮或改动传输行为。
- `testMordeResourceWorkspaceRouteBrowsesSyntheticManifest` 与 `testMordeReadOnlyWorkspaceDisablesCloudMutations` 在 iPhone 18 Pro / iOS 27.0 独立 Morde Simulator 通过 2/2，结果 `/tmp/MDA14-capacity-summary-20261108.xcresult`。断言覆盖已挂载 `已挂载 · 云电脑 Running host · 128 GB · 已用 46%` 与未挂载只读 Workspace `未挂载 · 128 GB · 已用 46%`。已查看截图 `/tmp/MDA14-capacity-summary-attachments-20261108/4BE31FD1-F561-4481-BC5C-87C54057E7D7.png`、`88AACD74-4209-4440-BBBC-43B50E6E4B21.png`；仅用进程内合成 API 数据。本轮内置浏览器按“资源 → morde 仓库”进入 HTML 原型，看到元信息 `已挂载 · 云电脑 PC-01 · 128 GB · 已用 46%`；和 Morde 的实际值结构及顺序一致，Host 名保留本机合成服务返回值。整页并排截图对照仍未完成。
- HTML `.lead-sub` 的容量摘要和 `.crumb .cur` 使用 `14.5px / #737378`；详情页摘要及当前目录文字已改为相同色值。`testMordeResourceWorkspaceRouteBrowsesSyntheticManifest` 与 `testMordeReadOnlyWorkspaceDisablesCloudMutations` 在独立 Morde Simulator 再通过 2/2（`/tmp/MDA14-capacity-summary-color-20261108.xcresult`），最新截图位于 `/tmp/MDA14-capacity-summary-color-attachments-20261108/`。
- MDA-14 仍未勾选：完整逐页视觉对照、其余状态和 Workspace 分享页面验收未完成。

### 2026-11-08 · MDA-03 任务状态图标色值对齐

- 对照 `docs/Morde iOS设计稿.html` 的 `.ric` CSS，将活动态图标渐变设为 `#B7BAC9 → #8E93A8`、完成态图标设为 `#34C759 / #ECECF0`、失败态设为 `#C81E1E / #FDECEC`。仅调整视觉色值，不改变任务状态映射、点击或操作入口。
- `testMordeTaskListRendersKnownAndUnknownStatusesWithoutActions` 在 iPhone 18 Pro / iOS 27.0 独立 Morde Simulator 通过 1/1，结果 `/tmp/MDA03-task-icon-palette-20261108.xcresult`；覆盖排队、运行、待确认、暂停、完成、失败、取消和未知八种状态。已查看截图 `/tmp/MDA03-task-icon-palette-attachments-20261108/C4A3B700-0AF8-4D59-BC9D-20FCA0E7BF0F.png`：活动蓝灰渐变、绿色勾和红色叉与 HTML 指定色值一致，行间距及无多余操作保持不变。仅用本机合成账号。
- MDA-03 仍未勾选：当前环境没有 iOS 16.0–16.3 Simulator，旧系统回弹行为和全页逐控件视觉/布局复核未完成。

### 2026-11-08 · MDA-14 Workspace 文件行样式

- 按 HTML `.row .tt` 样式调整 Workspace 文件/目录行：使用与原型 SVG 同尺寸/描边/颜色的 21pt 本地线框图标（`#3A3A3C`）；标题加 `-0.16pt` 字距；14pt 元信息统一使用 `#737378`，标题与元信息间距改为 2pt、元信息行距增加 3.5pt。保留已审核的文件行菜单和所有文件操作，不改变数据、点击目标或入口。
- `testMordeResourceWorkspaceRouteBrowsesSyntheticManifest` 与 `testMordeWorkspaceFileActionsStayInRowMenu` 在 iPhone 18 Pro / iOS 27.0 独立 Morde Simulator 通过 2/2，结果 `/tmp/MDA14-workspace-fileglyph-20261108.xcresult`。目录/空目录浏览、返回和已审核文件菜单操作保持正常；已查看根目录与文件菜单截图 `/tmp/MDA14-workspace-fileglyph-attachments-20261108/8EB38159-EDDD-4F85-B08D-C9F6B2F5BF3B.png`、`941EFB6E-924B-464D-93DF-5AD1CF35A7B1.png`，新图标尺寸、线宽/颜色、标题字距及元信息色值/行距在 iPhone 18 Pro 上可读，没有挤压或改变行操作。本轮内置浏览器读到了 HTML `ws` 页的容量摘要、文件/目录顺序和分享按钮；截图视口被裁切，仍不作为整页并排视觉通过证据。
- MDA-14 保持未勾选：完整逐页截图对照、其余状态和 Workspace 分享页面验收未完成。HTML `renderFiles()` 在子目录只提供直系上级入口；此行为已在后续 `/tmp/MDA14-parent-nav-20261108.xcresult` 中按“‹ 上一级 / 当前目录”复验通过。

### 2026-11-08 · MDA-14 内置浏览器原型逻辑走查

- 在内置浏览器的本机 HTML 预览中从 Workspace 根目录进入 `pay` 子目录，通过原型提供的直接父级面包屑返回根目录；打开分享面板并切换到“可编辑”，确认选中态移动到该权限。没有点击“复制分享链接”、改动剪贴板或发送内容。
- 这次只验证 HTML 原型交互，不代表 Morde 原生页面或分享 API 的产品验收；Morde 对应导航、分享创建/领取仍按独立 Simulator 和服务端证据记录，整页视觉并排核对未完成。

### 2026-11-08 · MDA-15 内置浏览器 Workbench 合成逻辑走查

- 打开本机 `127.0.0.1:5175` 的 Vite/MSW Workbench（`VITE_MOCK_API=1`）。合成 Host 首次接管返回“运行端暂不可用”，点“重试接管”后进入用户控制状态；本机终端执行固定模拟命令 `echo local-terminal-ok` 并显示预期结果；Workspace 可打开 `notes & plans/` 并返回根目录，Host 文件页可进入 `w1234567` 并列出 `README.md`。浏览器与 Simulator 走查均未下载、上传或改写文件；临时开发服务已停止。
- 本地无 VNC 服务，Workbench 报 `127.0.0.1:18084` 连接拒绝；不以此代替 Gateway/Host 验收。桌面视口保留续期和租约倒计时；本次未模拟 Morde WKWebView 注入的 iOS 壳样式，移动端隐藏这些信息的结论仍只以 Morde Simulator 证据为准。MDA-15 继续未勾选。

### 2026-11-08 · MDA-11 本机设计稿筛选逻辑复核

- 在内置浏览器本机 HTML `lib` 页面确认“全部 / AI 产物 / 我上传的”筛选能切换列表，原型分别展示 7 / 4 / 3 条静态样例；本次只切换筛选，没有打开文件、下载或改动内容。
- 这是 HTML 原型自身的逻辑走查，不是 Morde 产品验收；样例数量与来源不能代替真实账号数据。Morde 三种来源的合成 UI 验收及文件库截图证据仍见 MDA-11 前文；权限/证明缺失状态和完整逐控件视觉核验未闭环，MDA-11 保持未勾选。

### 2026-11-08 · MDA-02 Markdown 依赖独立化与 MDA-15 云电脑图标

- 首次重新构建独立 `Morde` target 暴露出 `MordeApp.swift` 对 `NanoMuseInlineMarkdown` 的两处引用；该视图只在未加入 Morde target 的 `NanoMuseFeed.swift` 中定义。为消除 Morde 对旧模块的编译依赖，在 Morde 源码内新增等价的 `MordeInlineMarkdown` 行内 Markdown 显示，并将两处调用切换到 Morde 类型；同时把超长无障碍标签分解为任务、事件和合并文本，消除编译器在该表达式上的类型推断超时。渲染内容/样式不变，不接入旧 Feed 页面。
- `MordeApp.swift` 中的本机旧 Workbench HTML fixture 原用 `◎`、`>_`、`□` 文本代替图标，导致 Simulator 截图与设计稿不符。已替换为与 `web/workbench/src/app/Workbench.tsx` 相同的 SVG 地球、终端窗口和文件夹图形；不改产品入口、菜单或点击行为，生产 Workbench 已有对应 SVG。
- 独立 `Morde.xcodeproj` / `Morde` scheme 在 `MDA-05 Fresh` iPhone 18 Pro / iOS 27.0 Simulator 运行 `testMordeComputerWebViewAlignsLegacyWorkbenchToDesignOnDevice`，1/1 通过；结果 `/tmp/MDA15-icons-20261108-r2.xcresult`。截图 `/tmp/MDA15-icons-20261108-attachments/26048B7A-4952-4B78-8DCC-0B6B7A898F1B.png` 已查看，三种图标形状现与设计稿相符；云电脑整页状态与真实 Gateway 仍未完成验收，MDA-15 保持未勾选，MDA-02 也仍需其他状态和逐页视觉核对。

### 2026-11-08 · MDA-15 云电脑图标与页面状态复核

- 复查 Simulator 最终图 `/tmp/MDA15-icons-20261108-attachments/26048B7A-4952-4B78-8DCC-0B6B7A898F1B.png`：浏览器、终端、文件三个入口图标与 HTML 形状一致；移动主界面不显示续租按钮、持有时间/租约倒计时、版本号或长说明，页头和控制状态文字保持精简，主操作为停止任务与接管/交还控制权。
- 截图里的远程浏览器是本机 WKWebView 新标签占位，页面空白；本机没有 VNC/Gateway 远程桌面服务，不能把该图当成实际浏览器工具栏、远程内容、全屏/窗口操作或真实 Host 状态的验收证据。此前记录的 VNC `127.0.0.1:18084` 拒绝连接仍是此限制。没有连接真实 Host、Portal 或 Gateway。
- 因此本次只确认已完成的移动端减项、入口图标和简化布局；MDA-15 仍未勾选，待获得可用的本地远程桌面 fixture 后再核对设计稿中的远程屏幕区域和交互状态。

### 2026-11-08 · MDA-06 审批原型控件再核对

- 在内置浏览器打开本地 HTML 的 `appr` 页面，AX 树确认有两张审批卡：第一张为“驳回 / 灰度 / 批准”，第二张为“驳回 / 改 3 天 / 批准”；第一张另有“看事件链”，卡片后有“已自动处理”区和两条历史行。底部四 Tab 顺序仍为聊天、任务、审批、资源。
- 与 Morde Simulator 合成截图 `/tmp/MDA-approval-workitem-style-attachments-20261108/93BD18C7-62E2-4B43-BA2C-CB45AA8294E3.png` 对照，Morde 当前一张真实待办卡显示批准/拒绝两动作、事件链入口和四 Tab。第三动作、自动处理历史及设计稿的变更/测试/成本摘要在当前 API 中没有对应合同；不新增按钮或伪造历史/数字，继续等待待决表中的用户选择。媒体审批仍只在带预览的 Task 详情处理。
- 本次为原型控件清点，不替代其他审批状态的 Simulator 验收或整页尺寸核对；MDA-06 保持未勾选。

### 2026-11-08 · MDA-07 主机图标与添加符号对齐

- 资源页主机图标改为与 HTML SVG 同构的线框显示器（21pt 画布、1.8pt 圆角描边）；资源“＋”改用原型全角 `＋` 字符。按钮尺寸、位置、可达性、长按分享和添加提示行为未改变，也没有新增控件。
- 独立 Morde iPhone 18 Pro / iOS 27.0 Simulator 运行 `testMordeResourceWorkspaceRouteBrowsesSyntheticManifest`，1/1 通过，结果 `/tmp/MDA07-glyph-20261108.xcresult`。测试继续验证资源“＋”可用、触发原有短提示、无额外关闭按钮；截图 `/tmp/MDA07-glyph-attachments-20261108/B781A135-4BBA-4812-B361-22987E7EFC78.png` 已查看，Host 线框图标可见，资源行/底部 Tab 未变。
- MDA-07 仍未勾选：Host 控制/租约状态文字、条件式“交还给 AI”入口的去留/显示条件与整页视觉尚未审核或完整核验；此项只完成无业务影响的符号对齐。

### 2026-11-08 · MDA-11/12 本机文件来源差异登记

- 对照 HTML `lib`（“全部 / AI 产物 / 我上传的”三类云端样例）和 Morde 当前代码，确认 Morde 的“全部”还会列出 app Documents 中 `Morde Workspace Files` 的本机文件，并在有 proof 时整行打开 Quick Look；AI 产物/用户上传分类不含该本机来源。这是额外数据来源与预览路径，不是设计稿里的云端文件行。
- 已在待审核表新增此差异，说明本机文件入口用途和移除 UI 对文件数据的影响；用户决定前不隐藏、删除或迁移文件。Task 构件 Quick Look 的三种行为选择仍单独待审。
- 本轮只做只读来源核对和差异登记，没有改动本机文件、同步、上传或预览行为；MDA-11/12 保持未勾选。

### 2026-11-08 · MDA-07 内置浏览器资源页逻辑复核

- 在内置浏览器本地 HTML 预览切换到资源页，确认原型显示云端主机、云手机、Workspace、AI 工具分组及静态容量/状态样例；点击“＋”显示“可添加云主机、Workspace 或 AI 工具”提示，没有进入真实创建流程。
- 这是 HTML 原型逻辑确认，不代表 Morde 有相同云手机/AI 工具数据源或添加 API。Morde 继续按真实可用数据呈现云主机/Workspace，缺少运行时合同的云手机和 AI 工具不伪造列表；其“＋”保留同位置短提示。MDA-07 状态不变，Host 额外状态及整页对照仍未审核/完成。

### 2026-11-08 · MDA-14 分享副本直接归属接收者

- Workspace 分享领取现由复制 reservation 同时记录源所有者、接收者、链接 ID 与只读权限；目标 Workspace 在建立时就归接收者所有，view 副本为只读、edit 副本可编辑，物理配额计入接收者。创建/恢复路径不调用 ownership-transfer service，也不改变原 Workspace 或 Host binding。
- 可证明尚未发生卷或物料化副作用的 `reserved` 失败，在单一事务中释放接收者配额、移除 reservation/idempotency、清空当前领取链接的 target，并保留已删除目标 tombstone，使同一领取人可重试。`copying`、`unknown` 或目标卷/物料化证据不干净时拒绝释放并保留 fence，以原目标恢复。
- Postgres 定向集成覆盖 view/edit、接收者配额、源冻结、owner history 且无 transfer、目标卷 reservation、unknown 恢复、安全失败后同人重试和有副作用时拒绝释放；通过。`go test ./internal/app ./internal/storage`、`make generate`、相关文件 whitespace 检查通过。结果由本机临时 Postgres fixture 验证，测试容器已清理。
- 变更后在 iPhone 18 Pro / iOS 27.0 Morde Simulator 重跑 `testMordeWorkspaceShareCreatesLinkWithSelectedPermission` 和 `testMordeWorkspaceShareDeepLinkClaimsPermanentIndependentCopy`，2/2 通过（`/tmp/MDA14-owner-split-ios-20261108.xcresult`）。覆盖创建链接选择编辑权限、深链领取页展示只读/独立副本规则及领取后副本出现在接收者列表；使用本地合成 API fixture，不是实际 Portal 联调。
- 迁移 00162 对旧版已完成的所有权转移副本按目标 Workspace 实际 owner/只读状态回填；domain 只接受无 share ID 的已完成旧记录。新增 `TestWorkspaceCopyOwnerSplitMigrationPreservesLegacyTransferredCopy` 验证升级、回滚和再升级不改旧 source/target owner、`is_read_only` 或 transfer history，并验证新 direct-share reservation 阻止回滚；本机 Postgres `-race` 用例通过。
- 上述仅验收服务端复制与持久化合同。升级前若存在 `claiming + target_workspace_id`，当前实现 fail closed；等待用户选择是否只为这种旧链接保留一次兼容领取路径。接收方领取 UI 和 MDA-14 整页视觉仍需验收，页面任务继续未勾选。

### 2026-11-08 · MDA-01 本机登录态检查

- 内置浏览器可以从本机 `127.0.0.1` 预览 `Morde iOS设计稿.html`；iPhone 18 Pro Simulator 中的 `io.morde.app` 当前显示离线/登录待确认，没有真实账号会话。
- 本轮没有输入或读取账号凭据。该设备状态不足以核验真实旁聊列表、审批数量和角标；既有合成 UI 测试及设计稿样例只证明布局/逻辑，不能替代真实数据验证。
- MDA-01 页面实现与抽屉/Tab 几何核验维持已完成状态；真实账号数据项留待恢复可用登录会话后复验，故页面任务仍未勾选。

### 2026-11-08 · MDA-02 聊天页首屏截图对照

- 在内置浏览器打开本地 HTML `chat` 页面，并对照 Morde Simulator 多轮聊天截图 `/tmp/MDA02-chat-final-confirm-attachments-20261108/CA80B45D-1C8E-42BF-9FB1-82451BEF1BD2.png`。两边都有居中头像/状态区、逐条 “Morde” 助手标识、无底色助手正文、单行输入与黑色发送箭头、四项底部 Tab。
- 该轮截图时用户消息气泡仍使用 nanoMuse 蓝色；之后按本地 HTML 改为浅灰，见“2026-11-08 MDA-02 聊天页头像与气泡对齐”。当前两张图的消息内容、附件和滚动位置不同，Morde 截图首条消息局部滚出视口，不能据此认定布局错误；后续用同文案、同附件的 fixture 复核可见密度及附件卡片位置。没有复制 HTML 中的预算样例数据。
- Task 状态摘要卡内的详情跳转仍属待审核交互；本轮未增删卡片或入口。MDA-02 保持未勾选，继续核对附件/键盘/滚动状态与逐控件视觉。

### 2026-11-08 · MDA-02 聊天视觉夹具与设计稿消息数统一

- 源码复核发现原 `chatVisualFixtureMessages()` 有 8 条文字消息，和 HTML `chat` 的 7 条样例不一致。已改为 7 条与原型相同的对话文案，省略原型首条消息中的预算示例；没有改产品端消息展示逻辑。
- Simulator 用例现在精确断言 7 个聊天消息行，以及页头和 4 条助手回复的 Morde 标识。图片/PDF 文件卡由独立附件用例验收；这个视觉夹具仍是纯文字，不能据此声称附件布局也已对齐。
- `testMordeChatMultiTurnVisualFixtureMatchesDesignDensity` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1，0 失败（`/tmp/MDA02-chat-seven-messages-20261108.xcresult`）。测试精确断言 7 个消息行及 4 条助手回复的 Morde 标识；截图 `/tmp/MDA02-chat-seven-messages-20261108-attachments/3032109F-E7E5-4381-9FA8-98C8CF5B0613.png` 已检查，全部 7 条消息均在输入栏上方可见，没有预算样例或被截断的首条消息。
- 对照 HTML `chat` 首屏，头像/状态区、助手逐条署名、消息顺序、发送位和四项 Tab 位置可直接比较；该轮截图用户气泡为 nanoMuse 蓝色，之后已改为 HTML 浅灰色（见最新 MDA-02 记录）。此截图夹具不含原型的 PDF/构件 chip，附件文件卡继续用独立 Morde 合成附件用例核对，因此 MDA-02 页面不勾选。
- `testMordeFullLayoutUploadsAndSendsImageAndPDFFixture` 于同一 Simulator 以本轮最新构建通过 1/1（`/tmp/MDA02-attachments-current-20261108.xcresult`）。图片和 PDF 成功发送并在用户消息气泡内显示文件名/“已保存”状态，助手回执可见；后续文件库筛选、图片预览与返回也通过。截图 `/tmp/MDA02-attachments-current-20261108-export/DBCD27B5-C057-4CD7-9812-46C9BEE17233.png` 已检查，文件 chip 结构与原型对应，用户气泡颜色继续采用 nanoMuse 蓝。该 fixture 没有活动 Task，因此不显示 Task 摘要卡；有关联活动 Task 时的卡片及详情跳转仍待用户审核。

### 2026-11-08 · MDA-03/MDA-07 视觉微调与集成回归

- MDA-03 的任务行文字组增加 1pt 顶部内距，对齐 HTML `.row .tt { padding-top:1px }`；原有状态文案间距不变，真实步骤计数、预算过滤、刷新与分页逻辑不变。MDA-07 的 Workspace 行存储图标改为自绘细线托盘，尺寸、圆角、线宽和指示点按 HTML 资源页图标对齐；容量字号与数据保持不变。
- 集成运行 `testMordeTaskListShowsStepCountsFromSummary`、`testMordeTaskListRendersKnownAndUnknownStatusesWithoutActions`、`testMordeResourceWorkspaceRouteBrowsesSyntheticManifest`，iPhone 18 Pro / iOS 27.0 Morde Simulator 3/3 通过，结果 `/tmp/MDA03-07-visual-integration-20261108.xcresult`。任务行/步骤计数/资源与 Workspace 路由截图已导出；资源截图 `/tmp/MDA03-07-visual-integration-20261108-attachments/FB5ABC8E-5FD8-49BA-AC40-E159B686D916.png`、状态矩阵 `/tmp/MDA03-07-visual-integration-20261108-attachments/52A24F95-70FF-4A67-BA5F-8103AA857F21.png`、真实步骤计数 `/tmp/MDA03-07-visual-integration-20261108-attachments/72E18EF4-A647-426A-B0F3-FD0849606076.png`。
- 重新对照 HTML `tasks` 页后，将 Morde 任务页“新建任务”图形从 16pt SF Symbol 改成设计稿使用的 19pt 全角 `＋`，仍保持 38pt 圆形点击区；只改 Morde 的 `onMenu` 分支，nanoMuse 旧壳分支不变。两边行布局的 44pt 图标、13pt 间距、13pt 上下留白及 22pt 水平边距一致。HTML 使用 5 条静态任务，Morde 的状态覆盖夹具为 8 条合成任务；此次核对行几何与操作尺寸，不把不同样例数据当作内容对齐证据。
- `testMordeTaskListShowsConfirmedEmptyState`（菜单和新建按钮宽度均为 38pt）与 `testMordeTaskListRendersKnownAndUnknownStatusesWithoutActions`（八种状态且无任务行额外操作）在干净 MDA-05 Fresh iPhone 18 Pro / iOS 27.0 Morde Simulator 2/2 通过，结果 `/tmp/MDA03-task-plus-glyph-clean-20261108.xcresult`；状态截图 `/tmp/MDA03-task-plus-glyph-attachments-20261108/094644EB-0430-4A66-9658-96268C1F585C.png` 已查看。首次复用中的 Simulator 在测试 Runner 建立连接前被系统终止；换干净 Simulator 后通过，未观察到产品断言失败。
- MDA-03/07 仅完成可核验的视觉微调与定向回归；完整逐屏位置、设备尺寸和 Host/资源未决状态仍未验收，两项页面保持未勾选。MDA-06 审批视图本轮只读复核未发现未审核边界内可安全修改的视觉差异，未改文件。

### 2026-11-08 · MDA-04 底部任务操作尺寸复核

- 内置浏览器从 `tasks` 点“修复支付回调超时”进入原型详情，确认目标摘要、四个执行节点与固定在底部的红色“急停这个任务”；原型 HTML 只展示交互，不作为 Morde 数据/动作验收。
- 对照 HTML `.stopbtn` 的 15.5px 字号与 13px 上下内边距，将 Morde 详情中已审核保留的暂停/取消操作按钮由 46pt 高度调至 44pt；两项仍位于 Tab 栏上方，动作数、颜色、权限、确认及互斥行为不变。
- `testMordeTaskDetailPauseLocksActionsAndRefreshesStatus` 在干净 MDA-05 Fresh iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1，结果 `/tmp/MDA04-action-footer-44pt-20261108.xcresult`。用例断言暂停/取消按钮均为 44pt；暂停请求在途时两项互斥，返回后详情刷新为暂停态并显示继续操作。截图 `/tmp/MDA04-action-footer-44pt-attachments-20261108/5D69FB3D-6BA6-458B-932D-7ADCE18AE0F6.png` 已查看。
- MDA-04 仍未勾选：活动历史/交付物区的待审核决定、其他详情状态和全页逐控件视觉尚未闭环。

### 2026-11-08 · MDA-03 回弹兼容分支覆盖与 MDA-11/12 空态/标题修正

- MDA-03 为 iOS 16.0～16.3 使用的 UIKit `alwaysBounceVertical` 兼容路径增加仅 DEBUG 进程参数夹具；生产构建不读取此参数，实际 OS 版本分支不变。`testMordeTaskListLegacyBounceProbeRecoversFromLoadFailureWithPullToRefresh` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 强制经过兼容路径，通过 1/1（`/tmp/MDA03-legacy-bounce-probe-20261108.xcresult`），确认任务列表请求失败时没有独立 Retry，仍可下拉恢复。此结果只证明兼容代码路径在 iOS 27 上工作；机器没有 iOS 16.0～16.3 runtime，不宣称已完成这些系统版本的真实运行时验收，MDA-03 保持未勾选。
- MDA-11 的“全部”筛选在有附件/产物内容时不再为其他空来源追加重复空行；仅当本机、Task 产物及个人来源均确认可用且内容为空时显示一次“这一类暂时没有”。单独类别继续显示自身空态，权限拒绝与来源不可用状态不改变。MDA-12 附件预览标题与返回按钮从 14pt 间距调整为与 HTML 对应的 10pt。`testMordeFullLayoutUploadsAndSendsImageAndPDFFixture` 在同一 Morde Simulator 回归中确认该列表状态、标题间距及不存在多余本机空态行，1/1 通过（`/tmp/MDA11-12-library-empty-and-header-20261108.xcresult`）；筛选图和图片预览图已导出并人工检查。MDA-11/12 仍不勾选：proof/权限边界、Quick Look 与原型结构差异及其余状态的整页视觉对照尚未闭环。
- 用户本轮再次确认移除 Task 详情的三处条件式“重试加载”按钮。已落实：详情读取错误及未确认历史不显示页面级重试；事件流自动重连；返回再进入详情触发重读；失败批次业务重试保持可用。此前 Morde Simulator 专项与“未确认历史返回重进”回归均通过，最新结果分别见 `/tmp/MDA04-reentry-user-approval-20261108.xcresult`；此项不引入新入口，MDA-04 因其他页面核验继续保持未勾选。

### 2026-11-08 · MDA-08 工作项滚动底部留白

- 按 HTML `.scroll` 的 `padding-bottom:108px` 对齐 Morde 工作项页滚动终点。SwiftUI 内容容器原有底部 14pt 内距，加 86pt Tab 安全区共 100pt；现将安全区补偿增至 94pt，合计 108pt。仅影响滚动到末尾后的留白，不改变筛选、项目/Task 上下文或行操作。
- 内置浏览器复核 `work` 页面：标题/“＋”、四个筛选、平铺工作项行及底部 Tab 已逐项可见。`xcodebuild build` 成功；`testMordeWorkItemsRouteShowsDesignFiltersAndCreateForm` 在干净 MDA-05 Fresh、iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA-08-scroll-padding-clean-20261108.xcresult`），覆盖工作项路由、权限、筛选和创建弹层；截图 `/tmp/MDA-08-scroll-padding-clean-attachments-20261108/57ACA8F9-A013-45A4-9076-19DCCEA222F9.png`、`644CA8D6-EF4E-455F-975A-59A39AD9A65A.png` 已保存并检查。未单独滚动到列表末尾作截图，因此滚动终点的视觉位置仍待验收；前两次在复用 Simulator 启动测试 Runner 时被系统终止，换清洁 Simulator 后成功。
- MDA-08 仍未勾选：项目/会话/Task 上下文、额外行操作、描述字段与待认领语义等待用户审核；键盘、行操作及完整逐项视觉核验仍未完成。

### 2026-11-08 · MDA-06 审批待办与事实文字颜色对齐

- 对照 HTML `.head .sub` 与 `.appr .facts`，Morde 审批标题旁待办数改为 `#A3A3A8`，事实文字改为 `#737378`；标题/事实/动作字号、行距、卡片顺序与决策逻辑不变。额外灰度/改天数动作、自动处理历史和变更摘要不在本次改动内，继续等审核与 API 合同。
- 内置浏览器走查 HTML `appr`：确认两条审批示例、驳回/灰度/批准三项、首条“看事件链”、第二条自动处理历史的原型布局。Morde Simulator `testMordeApprovalInboxShowsOnlyConfirmedDecisions` 在 iPhone 18 Pro / iOS 27.0 通过 1/1（`/tmp/MDA06-appr-color-20261108.xcresult`）：待办数为 1、批准后对应决策按钮移除、事件链入口打开对应任务。截图 `/tmp/MDA06-appr-color-attachments-20261108/065D123A-7E2D-41FF-907E-D0F8DFD7FD8D.png` 已查看，待办数和事实文本明暗现与 HTML 接近。
- MDA-06 仍未勾选：审批额外动作/历史与媒体直达预览仍待用户决定，其他状态和整页逐控件视觉核验尚未完成。

### 2026-11-08 · MDA-03 TaskSummary 步数改为必需字段

- 按用户已审核的“扩展 API 返回真实计数”决定收紧合同：`TaskSummary.step_count` 与 `completed_step_count` 在 OpenAPI 中列为必需整数，描述明确排除预算步骤；Portal/Workbench API schema 已重新生成。iOS `MordeTask` 两个字段改为非可选，任务行只显示服务端计数，删除从详情步骤数组推算的兼容回退；本地 Task 合成响应补齐字段。
- `go test ./internal/api` 通过；Portal 与 Workbench 的 `pnpm run generate:api` 成功；Morde Debug Simulator build 成功。iPhone 18 Pro / iOS 27.0 ad-hoc 签名 Simulator 用例 `testMordeTaskListShowsStepCountsFromSummary` 与 `testMordeTaskDetailShowsRealStepsWithoutBudget` 2/2 通过，结果 `/tmp/MDA03-required-counts-adhoc-20261108.xcresult`。列表截图 `/tmp/MDA03-required-counts-adhoc-attachments-20261108/492AF9B9-A2FC-4BDB-898F-3F98E3F23D43.png` 已检查，显示“5 执行步骤 · 2 已完成”。
- 无签名启动测试时，本地 UI 测试 URLProtocol fixture 在登录请求处返回 DNS `-3003`；改用 ad-hoc 签名后，工作项路由夹具和本次两个任务用例均通过，未改应用网络行为。MDA-03 仍需其他状态、整页视觉以及 iOS 16.0～16.3 运行时核验，页面任务不勾选。

### 2026-11-08 · MDA-08 工作项路由夹具签名复验

- 在干净 iPhone 18 Pro / iOS 27.0 Simulator 以 ad-hoc 签名运行 `testMordeWorkItemsRouteShowsDesignFiltersAndCreateForm`，1/1 通过，结果 `/tmp/MDA08-login-probe-adhoc-20261108.xcresult`。合成夹具验证工作项路由、项目切换、四类筛选及创建弹层；没有访问真实 Portal。
- 首次无签名测试运行在登录夹具阶段遇到 DNS `-3003`，不是工作项断言失败；ad-hoc 签名后复验通过。MDA-08 仍因待审核的项目/Task 上下文、描述与行操作，以及剩余错误/空态和整页视觉核验而未勾选。

### 2026-11-08 · MDA-03 五行任务页与内置浏览器交互核验

- 在 ChatGPT 内置浏览器打开本地 HTML `tasks` 页，实际点击“任务”→“新建任务”→输入并发送本地验收文案→返回任务页→打开“修复支付回调超时”→点击“急停这个任务”→返回列表；设计稿的 Tab、任务详情跳转、聊天输入/发送和详情状态反馈均可触发。原型急停后详情变为“已急停”，但返回列表仍保持静态的“进行中 · 72%”；这是 HTML 样例未联动，不作为 Morde 产品合同，也未照搬到服务端驱动的 iOS 实现。交互完成后重新加载，恢复原型默认状态；没有连接真实服务或传输用户文件。
- 为同内容视觉核对新增仅 DEBUG 生效的五行 TaskSummary 合成响应，不改生产路径。Simulator 先通过五行内容断言，但追加与 DOM 几何匹配的行高断言时发现 Morde 行高 84pt，HTML `.row` 实测为 91.59375px。已在共享行视图为标题、次级信息、状态补上对应的行高内距，使 5 个可见行均为 92pt。`testMordeTaskListRendersFiveDesignSampleRows` 在 iPhone 18 Pro / iOS 27.0 独立 Morde Simulator 最终通过 1/1，结果 `/tmp/MDA03-five-design-rows-aligned-20261108.xcresult`；精确断言五个与 HTML 相同标题/顺序、真实计数/当前步骤/状态、行高 91.6±2pt、五行首屏可点、无预算文案和无“加载更多”按钮。最终截图 `/tmp/MDA03-five-design-rows-aligned-attachments-20261108/D27A8E83-C81A-4C3E-A64F-AE1B6DC77356.png` 已检查：页头、新建入口、五个平铺行与底部 Tab 同屏，文字未折行/裁切；Morde 只显示 API 有依据的计数/当前步骤/状态，不复制原型执行者、设备数或进度百分比样例。
- MDA-03 仍不勾选：机器只安装 iOS 27.0/27.1 Simulator runtime，无法运行 iOS 16.0～16.3 回弹探针；五行首屏已完成的对照不覆盖列表错误/刷新、状态矩阵、分页末端以及不同设备/动态字号的完整视觉验收。

### 2026-11-08 · MDA-04 当前底部操作位置复验

- 重新在本机浏览器查看 HTML `task` 页面，确认设计稿单个“急停这个任务”按钮固定在底部 Tab 上方；当前 Morde 继续保留用户已审核的暂停/继续与取消双操作，且其 44pt 操作栏同样固定在 Tab 上方，不改变动作和语义。此前 44pt 尺寸已断言，本次重新导出当前实现截图排除旧截图误判。
- `testMordeTaskDetailPauseLocksActionsAndRefreshesStatus` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA04-task-actions-current-20261108.xcresult`）；最新运行态截图 `/tmp/MDA04-task-actions-current-attachments-20261108/B8552341-0877-472C-A571-54C7AE26D4CA.png` 显示两个操作保持在底部 Tab 正上方，请求中的互斥和暂停态刷新均通过。
- 本轮只验证已审核保留的操作位置，没有移除暂停/取消功能。Activity/交付物区域的去留等待此前待决表审核；该详情页其余状态和全页控件视觉仍需完成。

### 2026-11-08 · MDA-06/08 创建标题与事件链入口细节对齐

- 按 HTML `.sheet h3` 的 17.5px 字号及 `letter-spacing:-.01em`，Morde 工作项创建标题改为 17.5pt、-0.175pt 字距；按 HTML “看事件链”字号将审批事件链入口调整为 14pt，同时保留至少 32pt 的点击高度。没有新增/删除按钮或改变动作；审批第三动作/自动历史/变更摘要、工作项项目与 Task 上下文/可选描述/额外行操作继续按待审核项保留。
- 在内置浏览器打开本地 HTML 原型，走查 `work` 页缺陷筛选、创建弹层、填写本地合成文案并创建，以及 `appr` 页两条审批卡和原型动作/历史布局。创建后页面显示成功提示且新行可见；原型成功反馈仅用于交互核验，没有访问 Portal 或发送文件。
- 独立 Morde target、iPhone 18 Pro / iOS 27.0 Simulator：`testMordeApprovalInboxShowsOnlyConfirmedDecisions` 与 `testMordeWorkItemsRouteShowsDesignFiltersAndCreateForm` 通过 2/2（`/tmp/MDA06-08-visual-type-alignment-20261108.xcresult`）；查看审批截图 `/tmp/MDA06-08-visual-type-alignment-attachments-20261108/060F2F05-67AE-45A5-9FD5-D350FD7B21F5.png`、工作项创建弹层截图 `/tmp/MDA06-08-visual-type-alignment-attachments-20261108/2EE52C25-98A3-4273-B846-B52EFA44AEA2.png` 和列表截图 `/tmp/MDA06-08-visual-type-alignment-attachments-20261108/2649AC3E-9545-4154-BADE-FD4F174C2A9E.png`。随后 `testMordeWorkItemCreateSubmitsToSelectedProject` 通过 1/1（`/tmp/MDA08-workitem-create-after-type-align-20261108-rerun.xcresult`），断言创建成功提示、新工作项行出现及表单关闭。测试使用本机合成 API fixture。
- 首次单独启动创建提交用例时遇到共享 DerivedData 构建锁；待已有回归进程结束后串行重跑通过，锁冲突不计为产品测试失败。测试日志仍有既存 `Invalid frame dimension` 警告；本轮未改其成因。
- 这里只完成局部字号/入口和交互复核。工作项额外上下文与字段、审批设计稿外动作/历史、其他页面状态、完整并排视觉对照均未闭环；MDA-06/08 继续保持未勾选。

### 2026-11-08 · MDA-03/07 视觉色值与 MDA-13 设置原型走查

- MDA-03 任务行状态文字色值改为 HTML `.row .tt time` 的 `#ACACB1`；MDA-07 Host/Workspace 图标圆底、摘要文字、状态文字在浅色模式分别对齐 `#ECECF0`、`#737378`、`#ACACB1`，深色模式继续使用 nanoMuse 原生语义色。没有改变状态、权限、API、按钮或导航。主 agent 随后在独立 Morde Simulator 集成回归中验证任务列表与资源页相关用例 4/4 通过，页面继续保持未勾选。
- 在内置浏览器本地 HTML 原型依次点击菜单→设置→账号，再通过返回回到聊天并重新打开设置；账号入口触发原型“打开账号”提示，路由可用。原型账号值包含“已用 3%”预算样例；按用户决定，Morde 设置不展示预算/使用率，原型样例不作为应恢复该字段的依据。集成回归中的设置路由/真实不可用行与全局执行控制确认、恢复用例通过 2/2；根页仍有完整十项顺序/分组/几何的既有 Simulator 截图证据。凭证详情、设备错误/撤销状态和全页完整视觉仍待补齐，MDA-13 保持未勾选。

### 2026-11-08 · MDA-03/07/13 集成回归及 MDA-03/07 原型走查

- 独立 `Morde.xcodeproj` / `Morde` scheme、iPhone 18 Pro / iOS 27.0 Simulator 定向集成回归共 6/6 通过，结果 `/tmp/MDA03-07-13-integrated-20261108.xcresult`：MDA-03 五行任务页与任务状态矩阵 2/2；MDA-07 Workspace 浏览与 Host 控制错误展示 2/2；MDA-13 设置路由/真实不可用行与全局执行控制确认及恢复 2/2。已检查任务行、八状态矩阵、Host/Workspace、设置页及错误态截图；Host/Workspace 仍显示现存控制/租约类补充信息，按待审核决定保留，不在本轮擅自删除。
- 内置浏览器本机 HTML 原型逐项走查：任务列表→“发布 v2.4.1”详情→返回列表；资源→Workspace 根目录→`pay` 子目录→面包屑返回根目录。原型样例仅用于验证路由与交互，不作为 Morde 数据合同；未连接真实服务、未传输文件。
- MDA-03 与 MDA-07 的浅色模式颜色微调已纳入上述集成回归。MDA-03 尚缺 iOS 16.0–16.3 实际 runtime、列表其他状态/末端及全页逐控件视觉验收；MDA-07 尚缺 Host 额外状态审核和完整页面视觉验收；MDA-13 尚缺凭证详情、设备稳定/错误/撤销状态与完整详情页视觉验收。因此三页均保持未勾选。
- MDA-04 的三处页面级“重试加载”已按用户确认移除，返回后重进触发详情/历史重读，事件流自动重连和失败批次业务重试继续保留；此前和本次确认后的三项专项复验均为 3/3 通过，详见 `/tmp/MDA04-reentry-user-approval-20261108.xcresult`。MDA-04 其他详情状态和整页视觉仍待验收，页面保持未勾选。

### 2026-11-08 · MDA-11 文件库色值与 MDA-15 云电脑屏幕截图补正

- MDA-11 只调整 Morde standalone 浅色模式：筛选次级文字、行摘要设为 HTML `#737378`，选中筛选底色设为 `#EFEFEF`，默认类型块设为 `#ECECF0` / `#55555C`；暗色模式和旧 NanoMuse palette 保持原样。文件/附件内容、分类、筛选、操作入口和权限逻辑不变。`testMordeFullLayoutUploadsAndSendsImageAndPDFFixture` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1，结果 `/tmp/MDA11-library-colors-20261108-r2.xcresult`；“我上传的”截图 `/tmp/MDA11-library-colors-attachments-20261108/8FCBFB61-FBCD-4AB8-ACB6-8909C51E3BE6.png` 已检查。首次运行沿用了旧空会话、未出现附件夹具；测试现明确重置本机测试账号后再登录，修复测试隔离，不改产品登录行为。
- MDA-15 仅修正 DEBUG 本地 Workbench 视觉 fixture：补齐设计稿的标签栏、地址栏、屏幕图标 SVG，保留复现续租/倒计时/长说明的隐藏样例；视觉截图等待 WebView `.loaded` 后采集，并重置本机合成账号、加入一个 Host 匹配活动 Task，使底部停止按钮呈现设计稿的可用态。`testMordeComputerWebViewAlignsLegacyWorkbenchToDesignOnDevice` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA15-browser-chrome-20261108-r4.xcresult`），截图 `/tmp/MDA15-browser-chrome-attachments-20261108/34EA436A-9019-4367-8F90-BF059B58D45D.png` 已对照：三入口/图标、浏览器标签/地址栏、布局和底部操作符合设计稿。本次没有接触真实 Host；生产 WebView 内容仍需授权 Gateway 环境验收，MDA-15 保持未勾选。
- MDA-11、MDA-15 均只完成局部视觉证据，页面剩余权限/错误状态、真实 Gateway 内容及整页逐控件对照仍未闭环，不勾选。

### 2026-11-08 · MDA-03/06/08 视觉微调与集成验收

- MDA-03 将任务行次级信息/状态文字对齐 HTML 的 `#737378` / `#ACACB1`；活动、完成、失败图标沿用此前已对齐的设计稿色值。Morde Simulator 的五行设计样例与八状态矩阵通过 2/2；截图位于 `/tmp/Morde-design-alignment-attachments-20261108/`，包含 `31157271-089E-45FE-AA81-06D53E70FDDD.png`、`8CED40DA-C78A-4990-9E65-E885D3E04B69.png`。未改预算隐藏、行高、操作或状态映射。
- MDA-06 对齐审批标题字距、事实行间距/灰色、页头待办数字与事件链入口字号；批准/驳回数量与权限未变。`testMordeApprovalInboxShowsOnlyConfirmedDecisions` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1，批准后按钮退出并可进入事件链；结果 `/tmp/Morde-design-alignment-20261108/Logs/Test/Test-Morde-2026.11.08_07-45-37-+0800.xcresult`，截图 `/tmp/Morde-design-alignment-attachments-20261108/0838971F-6158-4F4D-994C-19FCBC54E8AB.png`。
- MDA-08 仅在浅色模式对齐筛选未选文字/底色、行元数据/状态、缺陷/需求类别圆标和创建类型按钮色值；深色继续使用原语义色。`testMordeWorkItemsRouteShowsDesignFiltersAndCreateForm` 通过 1/1，覆盖项目权限选择、缺陷/需求/待认领筛选和创建弹层；相同结果包，列表及弹层截图分别为 `/tmp/Morde-design-alignment-attachments-20261108/8787BE6A-22A1-450A-A6A5-B83B79E5E228.png`、`9D48BF19-1201-4962-A2E3-3DA364403110.png`。截图仍显示设计稿外项目/Task 上下文、描述及工作项操作，均等待用户决定；未改按钮或操作。
- 本机内置浏览器打开本地 `Morde iOS设计稿.html`：侧栏→任务列表→任务详情→返回；侧栏→审批→事件链；侧栏→工作项，切换“缺陷”和“待认领”并打开新建弹层。列表/筛选/路由反馈符合原型交互；未提交创建、批准或驳回操作。原型只用于交互走查，不作为原生页面/API 证据，也未连接外部服务。
- 三页全量状态和逐控件并排视觉尚未完成；MDA-03、MDA-06、MDA-08 均保持未勾选。

### 2026-11-08 · MDA-04 三处页面级重试移除回归与 MDA-11/12 清理重试

- 按用户最新确认，Task 详情读取失败、事件流失败、未确认事件历史三处页面级“重试加载”均已移除；返回任务列表后重新进入会重读详情/历史，事件流保留自动重连，失败批次业务重试保留。`testMordeTaskDetailTimelineFailureHasNoRetryButtonAndRecoversAutomatically`、`testMordeTaskDetailUnconfirmedHistoryHasNoRetryButton`、`testMordeTaskTerminalDetailsKeepStatusStepsAndApprovedActions` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 3/3（`/tmp/MDA04-alignment-regression-20261108.xcresult`）。终态和错误/恢复截图已检查；Task 详情额外活动/交付物区域仍等待既有待决项，不因本次决定一并删除。
- MDA-11/12 临时附件删除失败后保留清理队列并在后续重试的合成 UI 用例，首次发现 DEBUG 夹具误读独立授权镜像态，未展示结果提示；触发条件改为读取 Morde 客户端自身已发布的登录态，不改生产清理逻辑或页面入口。复跑 `testMordeTemporaryAttachmentCleanupRetriesAfterFailure` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1，结果 `/tmp/MDA11-cleanup-retry-20261108-r2.xcresult`。本用例覆盖失败保留和修复目录权限后再次清理，无截图附件。
- MDA-04 仍需完成其余状态和整页逐控件视觉；MDA-11/12 仍需完成剩余权限/证明状态及全页视觉核对，均保持未勾选。

### 2026-11-08 · MDA-14 Workspace 根页设计样例对照

- 为让 Simulator 截图能按设计稿根页样例逐项比较，新增仅本地 DEBUG URLProtocol 使用的 Workspace 视觉夹具：名称为“morde 仓库”，挂载摘要与设计稿一致，文件行包含 `pay` 目录、`callback.ts` 和 4 KB 的 `README.md`。新增 `testMordeWorkspaceRootMatchesDesignSampleItemCount`，检查三行顺序、标题/挂载状态，并保存截图。独立 Morde iPhone 18 Pro / iOS 27.0 Simulator 用例通过 1/1（`/tmp/MDA14-design-sample-20261108-r2.xcresult`）；截图 `/tmp/MDA14-design-sample-attachments-20261108/D99DF44B-04AB-4E38-8DB7-5C90825194F7.png` 已查看。
- 对照内置浏览器中的 HTML `ws`：标题、返回/分享、挂载摘要、根路径和三行顺序对齐；文件行“…”菜单是用户已审核保留的 Workspace 文件操作入口。Morde 的文件摘要显示真实清单大小；设计稿 `callback.ts` 还显示 `TS · 本次改动 · +86 / −41`，当前 Workspace manifest 合同只有路径、字节数、哈希、权限和分块，没有文件类型/差异行统计。不得把设计样例统计写入真实文件行；完整 diff 元数据需要另行扩 API 合同，当前保持真实文件大小。
- MDA-14 仍未勾选：其他错误/权限/分享状态、剩余目录页面视觉及分享领取页面核对未闭环；路径栏按 HTML 只保留直系上级入口，待本轮 Simulator 回归确认；MDA-16 不提前开始。

### 2026-11-08 · MDA-14 Workspace 路由回归与 MDA-04 重试入口审核确认

- 按用户确认，Task 详情页详情加载失败、事件流失败、事件历史未确认的三处页面级“重试加载”均删除；用户明确选择返回后重新进入恢复。前一轮 Morde Simulator 专项回归覆盖三种状态并通过 3/3（`/tmp/MDA04-alignment-regression-20261108.xcresult`），事件自动重连与失败批次业务重试保留。
- Workspace 视觉 fixture 加入后，重新运行 `testMordeResourceWorkspaceRouteBrowsesSyntheticManifest`，iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA14-workspace-route-after-designfixture-20261108.xcresult`）。截图 `/tmp/MDA14-workspace-route-after-designfixture-attachments-20261108/32E8AF31-9023-4255-B2D9-E0335B2746F3.png` 显示根目录挂载摘要及目录/文件行；`11E06EC4-F4FA-453C-8382-18AC375FEA45.png` 显示空目录提示。两张图已查看。该路由夹具仍使用 `Ready Workspace`，设计稿专用根页的 `morde 仓库`、`pay`、`callback.ts`、`README.md` 样例另由 `/tmp/MDA14-design-sample-20261108-r2.xcresult` 覆盖。
- 结果只确认合成 Workspace 路由与空目录恢复，不代表真实 Host 或分享 API 联调，也不完成 MDA-14 整页视觉门槛；MDA-04、MDA-14 仍保持未勾选。

### 2026-11-08 · MDA-14 Workspace `pay` 子目录逐页视觉核对

- 扩展本机 DEBUG Workspace fixture，为设计稿 `pay` 目录提供三份合成文件；`testMordeWorkspaceRootMatchesDesignSampleItemCount` 与 `testMordeResourceWorkspaceRouteBrowsesSyntheticManifest` 现覆盖根页进入 `pay`、子目录文件行及逐级父目录返回，通过 iPhone 18 Pro / iOS 27.0 Morde Simulator 2/2（`/tmp/MDA14-parent-nav-20261108.xcresult`）。截图 `/tmp/MDA14-parent-nav-attachments-20261108/C8B41BE0-F6DC-4747-860A-F27F5E7207C0.png` 显示“‹ morde 仓库 / pay”；`B9B5DF31-5C3A-4125-AFEF-C8347459EDE3.png` 显示“‹ docs / empty”。较早截图 `/tmp/MDA14-design-nested-20261108.xcresult` 的祖先面包屑样式已被本轮修正后的结果取代。
- 子目录结构和面包屑样式/逐级返回与 HTML 对应；仍有可见内容差异：HTML 将 `callback.ts`、`retry.ts`、`callback.test.ts` 按设计顺序排列并显示 TS/变更说明，Morde 当前按路径名排序（`callback.test.ts`、`callback.ts`、`retry.ts`）并显示 API 返回的实际字节数；HTML 根页文件夹摘要为“支付模块”，当前 manifest 只有路径，无法从 API 得到该摘要。不能伪造 diff、文件类型变更或目录说明；相关 manifest 合同补充仍未授权。
- 本次生产 UI 只把子目录面包屑改为 HTML 的直系上级入口和斜杠分隔，保留已验证的逐级返回；另有 DEBUG 夹具和 Simulator 视觉检查，没有更改生产文件排序/API，也未访问真实 Host。MDA-14 仍未勾选，其他页面状态、错误/权限/分享对照和完整逐控件视觉核验未闭环。

### 2026-11-08 · MDA-04/07/08/11/12/13 逐页差异复核与设置根页微调

- 内置浏览器在本地 HTML 预览中走查资源→聊天→菜单→设置，确认设置设计根页为三组、九个设置入口加单一急停行，底部四 Tab 不变；账号套餐使用率和审批规则摘要是 HTML 演示值，Morde 不补预算/伪造策略数据。设置根页 Morde 截图 `/tmp/MDA-settings-root-visual-attachments-20261107/6AC7500E-A704-4C9C-A373-21A5C95A4BDD.png` 与原型根页顺序、分组、留白及页头位置对应。
- 对照 HTML `.srow`，设置行的值颜色改为 `#A3A3A8`，chevron 改为同字号/字重的文本 `›`，图标间距由 10pt 调整至 13pt，唯一红色全局执行控制在浅色模式使用 `#C81E1E`。深色使用动态语义色；未改变控件数量、位置层级、路由、急停确认和恢复行为。代码编译成功；但本轮两次 Simulator UI 验收都在进入设置页之前失败：本地登录请求报 `NSURLErrorDomain -3003`，没有导航控件出现，因此不能把本轮截图算作改后视觉验收，也不勾选 MDA-13。结果见 `/tmp/MDA13-settings-ui-align-20261108/Logs/Test/Test-Morde-2026.11.08_08-42-45-+0800.xcresult`（原 iPhone 18 Pro）和该目录后续 `08-48-57` 结果（全新本地 iPhone 18 Pro Simulator）。原页面路由/交互已有 2026-11-07 成功证据；需先排除本地验收登录夹具 DNS 错误后，再拍摄新样式截图。
- 只读复核 MDA-04：页头标题可补约 `-0.17pt` 字距，Task 摘要/步骤/事件次级文字可局部对齐 `#737378`；Task 步骤专属虚线已调为 2pt 与浅色 `#D9D9DE`，深色沿用 nanoMuse hairline，不动事件链共用节点。该次复核时摘要至首节点距离尚未测量，后续已按 HTML margin collapse 结果去掉重复的摘要底部间距，并用 Morde Simulator 截图复验。预算过滤、已审核的暂停/继续/取消、失败批次业务重试保留；三处页面级加载 Retry 已按用户确认移除。标题字距、次级文字颜色、步骤虚线及摘要间距均已实施；不改共享节点和未审核内容。
- 只读复核 MDA-07/08：资源页标题、按钮尺寸、图标、行距和已调整的浅色文字基本对应设计稿；Host 控制者/租约/交还等额外信息按待审核项保留。工作项筛选器/行排版基本一致；认领按钮垂直对齐已改为相对整行居中，图标/文字仍顶部对齐，未改按钮大小、权限或认领语义，并通过 Morde target 编译。创建文案间距和空标题按钮状态仍是差异；涉及本地化文件与交互，暂不扩范围。
- 只读复核 MDA-11/12：筛选、文件行及个人图片/PDF 预览没有常驻多余按钮；本机文件来源、Task 构件 Quick Look 和不可用提示仍在审核项。个人附件预览页头由上下各 8pt 改成设计稿上 10pt、下 4pt；文件库独立页头由通用 40pt/12pt 几何改成 38pt 按钮、22pt 水平边距、10/4pt 上下内距、10pt 标题间距和 -0.17pt 标题字距。菜单、返回、筛选保留、导航和控件数不变。MDA-11/12 的错误/权限状态及详情整页视觉验收仍未闭环。
- 本段新增视觉微调均不删除用户数据或对外发送文件；MDA-04、07、08、11、12、13 仍因各自页面状态、待审核行为或截图/整页对照未完成而保持未勾选。

### 2026-11-08 · MDA-13 设置箭头最终样式与登录夹具复验

- 对照 HTML `.srow .chev` 后发现此前只换了箭头字符，字重、字号和颜色仍偏黑偏粗；已将 Morde 设置行尾 `›` 调整为 20pt light，浅色模式使用原型 `#C6C6CB`，深色模式使用动态浅灰。行内控件数量、位置、路由、真实账号值和急停行为不变。
- 之前两次新 Simulator UI 用例在本地登录步骤报 `NSURLErrorDomain -3003`。随后 `testMordeSettingsRoutesAndHonestUnavailableRows` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA13-settings-route-recheck-20261108.xcresult`），验证账号/设备/占位详情路由、设置行尺寸及操作；`testMordeDesignNavigationRoutes` 也通过 1/1（`/tmp/MDA13-chevron-alignment-20261108.xcresult`），新截图 `/tmp/MDA13-chevron-attachments/F82347FC-027B-437A-9BCF-81EE9DEA98F7.png` 已检查。
- 内置浏览器本地 HTML `set` 页与新 Simulator 根页截图对照：三组、九个设置入口、单个急停行、四个底部 Tab 顺序一致；箭头轻细度已对齐。账号与审批规则的值仍使用 Morde 可获得的真实数据，不照抄 HTML 示例值。未连接真实 Portal/Hub，没有外发文件。
- MDA-13 保持未勾选：设备稳定态、撤销/错误/刷新状态、凭证/API Keys 详情、完整详情视觉及设置行额外交互审核仍待完成。

### 2026-11-08 · MDA-04 摘要至首个步骤的间距复验

- 对照本地 HTML 任务详情后，按 `.lead-sub` 的 14px 下边距与 `.chain` 间距在浏览器中的 margin collapse 计算，摘要文字块到首个步骤节点的有效间隔为单个约 14px 间距。Morde 原先同时使用摘要 `.padding(.bottom, 14)` 和详情 `VStack(spacing: 16)`，重复增加了留白；已移除摘要的额外底部 padding，保留主内容栈间距、节点、事件及操作区。
- `testMordeTaskDetailShowsRealStepsWithoutBudget` 在 iPhone 18 Pro / iOS 27.0 独立 Morde Simulator 通过 1/1，结果 `/tmp/MDA04-summary-spacing-align-final-20261108.xcresult`。页面截图 `/tmp/MDA04-summary-spacing-align-final-attachments-20261108/5D23002A-BB26-407D-B6FE-24F1617E0FAA.png` 已检查，确认首节点与摘要的间距收敛；验收使用本地合成 Task 数据，不连接真实 Portal/Host。
- 页面级详情读取、事件流和未确认事件历史的三处“重试加载”已按用户确认移除，返回后重进重新读取；事件流自动重连和失败批次业务“重试”保留。详情中仍有超出原型的活动历史/交付物区，已审核保留的暂停/继续/取消操作也与原型单一“急停”按钮不同；这些待审核差异未在本次擅自删除。MDA-04 继续保持未勾选，待完成整页逐控件视觉核验和剩余页面状态验收。

### 2026-11-08 · MDA-03/07/08/15 局部对齐与综合回归

- 按页面文件边界并行复核：`MIFTaskViews.swift` 重绘任务列表活动状态与完成状态图标以对应设计稿 SVG；`MIFResourceViews.swift` 为 Host 和 Workspace 文本列各加 1pt 顶距；`MIFWorkItemViews.swift` 将滚动区顶部留白调整为 2pt。没有新增/删除控件，没有触碰待审核数据或操作，也没有改 API。
- 内置浏览器从设计稿资源页进入 PC-01，接管后切换终端，再从“文件”打开绑定 Workspace、进入 `pay` 目录，返回资源后重新打开云电脑并关闭回资源。浏览器原型中终端在接管前锁定、接管后可切换；此行为已按可见 UI 状态验证。该流程是原型逻辑检查，不当成 Morde 功能证明。
- 独立 `Morde.xcodeproj` / `Morde` scheme 在 iPhone 18 Pro / iOS 27.0 同轮通过 7/7：`testMordeTaskListRendersFiveDesignSampleRows`、`testMordeTaskListRendersKnownAndUnknownStatusesWithoutActions`、`testMordeResourceWorkspaceRouteBrowsesSyntheticManifest`、`testMordeWorkItemsRouteShowsDesignFiltersAndCreateForm`、`testMordeWorkItemCreateSubmitsToSelectedProject`、`testMordeComputerWebViewAlignsLegacyWorkbenchToDesignOnDevice`、`testMordeComputerRouteAcceptsSyntheticTicketAndReturnsToResources`。结果 `/tmp/MDA-parallel-pages-regression-20261108.xcresult`。测试确认任务行/未知状态、资源/Workspace、工作项筛选与提交、云电脑仅显示浏览器/终端/文件、绑定 Workspace 和关闭返回资源。数据全部来自本地合成 fixture。
- 共导出 11 张测试截图，重点检查任务五行、八种状态、资源 Host/Workspace、工作项列表/新建弹层及云电脑 WebView 六张。代表截图：`/tmp/MDA-parallel-pages-regression-attachments-20261108/97D070D0-8F33-4DAD-AFA8-4083B6C6BE96.png`、`/tmp/MDA-parallel-pages-regression-attachments-20261108/48C96D46-AFB6-45E0-BCFA-8E122DA38038.png`、`/tmp/MDA-parallel-pages-regression-attachments-20261108/34258407-D03F-4370-AB55-9F71D9D19F1C.png`、`/tmp/MDA-parallel-pages-regression-attachments-20261108/F5CDE8F6-77FE-4637-8B76-31E27F77B46A.png`、`/tmp/MDA-parallel-pages-regression-attachments-20261108/80F39D3A-3B90-4762-8B12-0B005A100A30.png`、`/tmp/MDA-parallel-pages-regression-attachments-20261108/E4EC3621-1650-4EB7-8B27-E71BE686D355.png`。云电脑截图中没有续租按钮、持有时间倒计时或长说明。
- 结果包记录 `Invalid frame dimension (negative or non-finite)` 与 UIKit `_UIGravityWellEffectAnchorView` 层级警告。读取本机 iOS 27 Simulator unified log 和该用例 activity 后，确认无效 frame 出现在 `morde.workItems.create.title` 获得焦点、键盘弹出前；runtime 回溯只有 SwiftUICore 栈与 App `main`，source location 行号为 0。单文件只读追查也没有发现可确定的负尺寸表达式，因此本轮没有猜测性修改；需在后续授权调试运行中采集 SwiftUI 调用栈/键盘弹出前后的视图几何再判定。UIKit `_UIGravityWellEffectAnchorView` 警告是另一条系统 runtime 记录。MDA-03 因 iOS 16.0～16.3 runtime 与整页视觉未闭环、MDA-07 因 Host 扩展状态审核及整页视觉未闭环、MDA-08 因附加上下文/描述/行操作待审核、MDA-15 因真实 Gateway/WebView 未验与整页视觉未闭环，均保持未勾选。

### 2026-11-08 · MDA-12 附件标题复验与 MDA-04 Retry 决定复核

- 对照 HTML 文件详情 `.head h2` 的 `letter-spacing:-.01em`，将个人图片/PDF 附件详情标题补齐为 `-0.17pt` 字距；保持动态字体、10pt 返回按钮间距、设计稿页头内边距、预览卡和 Tab 栏不变。独立 Morde iPhone 18 Pro / iOS 27.0 Simulator 的 `testMordeFullLayoutUploadsAndSendsImageAndPDFFixture` 通过 1/1，覆盖聊天图片/PDF 预览、文件库来源筛选、返回保留筛选及从预览切换 Tab；截图 `/tmp/MDA12-letterspacing-attachments-20261108/527340AC-C42C-4B9C-8318-D3307D258886.png` 已查看。
- 为保护未改动边界，`testMordeTaskArtifactQuickLookShowsSyntheticPreview` 与 `testMordeChatAttachmentWithoutLibraryProofCannotOpenPreview` 在同一 Morde scheme 复验通过 2/2（`/tmp/MDA12-preview-boundaries-20261108.xcresult`）。内置浏览器本地 HTML 走查确认聊天 PDF 可进文件详情并返回原会话；“我上传的”筛选能缩小文件列表。样例库行点击只保留列表并显示原型状态提示；这是设计原型的模拟交互，不作为 Morde 文件详情功能证据。
- 用户再次确认移除 Task 详情页详情加载失败、事件流失败、未确认事件历史的三处“重试加载”按钮，返回列表后重进恢复。该决定与已实现代码一致；本轮在 iPhone 18 Pro / iOS 27.0 的 Morde Simulator 复跑 `testMordeTaskDetailTimelineFailureHasNoRetryButtonAndRecoversAutomatically`、`testMordeTaskDetailUnconfirmedHistoryHasNoRetryButton` 和 `testMordeTaskTerminalDetailsKeepStatusStepsAndApprovedActions`，通过 3/3（`/tmp/MDA04-retry-reaffirmation-20261108.xcresult`）。详情/未确认历史无页面 Retry、事件自动恢复、重进重新读取均通过；失败批次业务“重试”仍保留。检查的错误/恢复与业务重试截图位于 `/tmp/MDA04-retry-reaffirmation-attachments-20261108/`。本轮无需改动代码。
- MDA-12 仍因其他错误/权限状态及整页视觉对照未闭环而不勾选；Task Quick Look 的详情页结构差异继续按待审核边界保留。本轮仅使用合成图片/PDF，未访问真实 Host、Portal 或用户文件，也未外发文件。

### 2026-11-08 · MDA-07/08/13 按 HTML 补充微调与浅/深色验收

- 逐项核对本地 `Morde iOS设计稿.html` 的 CSS：工作项输入框 `.wi-in input` 为 15px，Morde 新建标题输入框补为 15pt；工作项标题 `.row .tt b` 为 16px、`letter-spacing:-.01em`、`line-height:1.3`，Morde 多行标题采用 `-0.16pt` 字距及 1.5pt 行距。只改变文字视觉，没有新增/删除控件或改变工作项筛选、创建、认领和权限逻辑。
- 设置行 `.srow` 的文字继承 16px/600 与 `#111`，chevron 靠右。本轮将之前 20pt 浅灰箭头修正为 16pt semibold 深色字形 `›`，并让存在 value 的行通过独立 spacer 将 value 与 arrow 排在行末；未改设置入口数量、路由、确认或恢复行为。MDA-13 根页截图 `/tmp/MDA-three-pages-light-attachments-20261108/AC9DB136-2D34-4105-B1D0-5631564BA47D.png` 已检查。
- MDA-07 在深色模式使用 nanoMuse 动态墨色提升 Host/Workspace 图标描边对比，浅色描边仍为 HTML 的 `#3A3A3C`。本轮使用预先设置深色外观的 iPhone 18 Pro / iOS 27.0 Morde Simulator 完成 `testMordeResourceWorkspaceRouteBrowsesSyntheticManifest` 1/1；深色截图 `/tmp/MDA-resource-dark-preboot-attachments-20261108/A4930BC8-E166-4CB8-BC10-EF3C2FB7026A.png` 已检查，确认图标清晰且未变更按钮位置/数量。较早在运行中切换外观得到的浅色截图未作为深色证据使用；测试后 Simulator 已恢复浅色外观并关机。
- 浅色 Morde 综合定向回归 5/5 通过：`testMordeDesignNavigationRoutes`、`testMordeResourceWorkspaceRouteBrowsesSyntheticManifest`、`testMordeSettingsRoutesAndHonestUnavailableRows`、`testMordeWorkItemCreateSubmitsToSelectedProject`、`testMordeWorkItemsRouteShowsDesignFiltersAndCreateForm`，结果 `/tmp/MDA-three-pages-light-20261108.xcresult`。资源/Workspace 截图 `/tmp/MDA-three-pages-light-attachments-20261108/064638BE-E4F8-4D59-A0B9-248B37FC6BFA.png`、工作项列表 `/tmp/MDA-three-pages-light-attachments-20261108/25383A41-CCC2-4A68-B87E-BFBE27EA9D6E.png`、新建弹层 `/tmp/MDA-three-pages-light-attachments-20261108/17646219-AE58-4B21-8D49-1BCAF379316B.png` 已查看；所有数据为本地合成 fixture。`git diff --check` 通过。
- 内置浏览器从本地 HTML 走查资源页→PC-01→Workspace 根目录→`pay` 子目录及返回，之后检查工作项缺陷筛选与新建原型并恢复初始页面。这仅验证原型路径；没有外发 HTML、向服务端提交真实数据或访问真实 Host/Portal。
- 本轮 MDA-07/08/13 页面依旧不勾选：Host 主列表中的额外控制状态/租约及完整页面视觉仍待审核；工作项 Task 上下文、可选描述和行操作仍待用户审核；设置页设备稳定/错误/撤销态、凭证详情和完整详情视觉仍未验收。UI 运行还记录 `_UIGravityWellEffectAnchorView` 层级警告及工作项标题获焦时的无效 frame 警告，当前结果未提供可定位的产品源码行；不把它们归入视觉微调结果。

### 2026-11-08 · MDA-03/06 行高与审批事实排版集成验收

- MDA-03 对照 HTML `.row` 及既有五行样例验收，将列表行最小高度校准为 91.6pt；首轮集成检查发现实际 89pt、未达到设计稿测量范围，修正后重新运行 `testMordeTaskListRendersFiveDesignSampleRows` 与 `testMordeTaskListRendersKnownAndUnknownStatusesWithoutActions`，iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 2/2（`/tmp/MDA03-task-row-height-restore-20261108.xcresult`）。任务行与八状态截图已查看，详情数据、行操作和状态映射未改变。
- MDA-06 对照 HTML `.appr` 微调审批标题行距、事实行字号/行距/颜色、事件链入口字色及按钮胶囊视觉内缩；44pt 点击区域和批准/拒绝动作保持不变。`testMordeApprovalInboxShowsOnlyConfirmedDecisions` 在同一 Morde Simulator 通过，结果包 `/tmp/MDA03-06-retry-style-acceptance-20261108.xcresult`；截图 `/tmp/MDA03-06-retry-style-attachments/426C40FE-DCB4-45B3-96CE-D0AC4A426DD7.png` 已查看。没有加入设计稿外动作、历史或统计数字。
- 同次定向回归中，Task 详情事件流失败自动恢复和未确认历史返回重进两项均通过；页面级“重试加载”继续移除，事件自动重连及失败批次业务重试保持原状。重试决定已由用户再次确认，与现有实现一致，本轮不重复改代码。
- MDA-03 仍待 iOS 16.0～16.3 runtime 及全页其他状态/视觉验收；MDA-06 仍待第三动作/历史/变更摘要与媒体直达入口的待审核结论及整页视觉验收，两页保持未勾选。

### 2026-11-08 · MDA-11/12 文件库与附件页头改后截图验收

- 在新的干净 iPhone 18 Pro / iOS 27.0 Morde Simulator 运行 `testMordeFullLayoutUploadsAndSendsImageAndPDFFixture`，通过 1/1（`/tmp/MDA11-12-postgeometry-fresh-20261108.xcresult`）。覆盖聊天图片/PDF 预览、资源 Tab → 抽屉 → 文件库、全部/AI 产物/我上传的筛选、上传图片打开、返回后筛选保留，以及从预览切回聊天。原共享 Simulator 的首次尝试在测试准备阶段清理旧登录态时失败，未进入文件页；改用干净测试 Simulator 后主流程通过。
- 已检查改后截图：文件库“我上传的” `/tmp/MDA11-12-postgeometry-fresh-attachments/27E6A904-0D02-4716-824B-7475E75190C3.png`，文件库图片详情 `/tmp/MDA11-12-postgeometry-fresh-attachments/DB76B679-0459-450E-AFD3-65936344D3B3.png`。页头、筛选、平铺文件行、标题与返回间距及底部 Tab 位置符合 HTML 的结构；真实文件名/大小和 PDF/图片内容按合成附件展示。详情页无分享/保存按钮。
- 内置浏览器从原型资源页进入侧边栏文件库，依次查看全部、AI 产物、我上传的，再从聊天附件打开 PDF 详情；原型页面显示标题、内容和底部 Tab。未触发外部传输或提交操作。
- MDA-11/12 仍保持未勾选：未登录与完整权限/证明状态及其余预览状态尚未闭环；Task Quick Look 文件详情外框已在后续 2026-11-08 记录中按设计稿对齐。这次通过只证明所列合成主流程和已截图状态。


### 2026-11-08 · MDA-12 Task 构件 Quick Look 详情外框对齐

- 按逐页对齐要求，Task 构件继续通过现有 Workspace proof gate 后打开原生只读 Quick Look 正文；现在外层补上设计稿文件详情页的返回按钮、文件标题、正文卡和四项底部 Tab。返回后关闭详情并回到原“AI 产物”筛选。没有增加分享、保存、编辑或下载按钮。
- `testMordeTaskArtifactQuickLookShowsSyntheticPreview` 在 iPhone 18 Pro / iOS 27.0 独立 Morde Simulator 通过 1/1（`/tmp/MDA12-quicklook-shell-final-20261108.xcresult`）。测试检查正文加载、预览卡高度、返回/标题/Tab、返回列表并保留筛选；改后截图 `/tmp/MDA12-quicklook-shell-final-attachments/C273A19C-9526-4100-BF0F-9ADE417FA9D4.png` 与按钮清单 `/tmp/MDA12-quicklook-shell-final-attachments/7B200136-D3BF-46E4-837E-300685AE93A4.txt` 已检查，未见分享或保存按钮。
- 用户再次确认移除 Task 详情页的三处页面级“重试加载”，通过返回后重新进入恢复；该决定与既有实现一致，事件流自动重连和失败批次业务“重试”保留。对应 Morde Simulator 专项验收为 3/3，见 `/tmp/MDA04-retry-reaffirmation-20261108.xcresult`。
- 本轮只用本机合成文本构件，没有读取真实用户文件或对外发送文件。MDA-11/12 因未登录/权限/证明边界及整页其他状态视觉未闭环，继续保持未勾选。


### 2026-11-08 · MDA-04 Task 详情内容区与原型逐屏复核

- 在内置浏览器从 HTML 任务列表进入“发布 v2.4.1”详情：原型包含返回/标题、单条状态摘要、步骤时间线、固定在底部 Tab 上方的红色“急停这个任务”按钮及四项 Tab。查看当前 Morde Simulator 截图 `/tmp/MDA04-task-detail-spacing-attachments-20261109/221E2A16-2660-460F-9F0D-5D96062BD6AF.png`：返回/标题、真实摘要/步骤及底部 Tab 上方操作区结构相同；Morde 使用用户已审核保留的真实暂停/取消操作，因此是两个并排按钮，不把它们伪装成原型的单独“急停”。
- Morde 截图另有“交付物 / 暂无交付物”区，HTML 详情没有该区；该区域只有在 Task 产出交付物时可提供打开文件的入口，空态本身无动作。该项用途与移除/迁移后影响已列在待审核表，仍等待用户决定，不擅自删。用户已审核移除的三处页面级“重试加载”不再显示；重新进入任务详情恢复读取。
- 此次是原型路由检查及现有本地合成截图核对，不是新的 Simulator 测试，也未检查所有状态和不同长度文本。MDA-04 继续未勾选。

### 2026-11-08 · MDA-14 分享领取页视觉核对

- 内置浏览器中的 HTML `ws` 页只有分享者弹层：权限“可查看/可编辑”两项及“复制分享链接”，没有接收者领取页面。接收者页是用户已授权 Workspace 分享链接和独立副本后必须经过的功能流程，不存在可逐项仿照的设计稿页面。
- 为检查该必要流程，在既有深链领取 UI 用例中增加截图附件；`testMordeWorkspaceShareDeepLinkClaimsPermanentIndependentCopy` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA14-share-claim-visual-20261109-r1.xcresult`），领取前截图 `/tmp/MDA14-share-claim-visual-attachments-20261109/9DB22B75-F049-441A-85E6-B82D3E08E1FD.png` 已检查。页面显示 Workspace 名称、访问权限、用户要求的“领取后为独立副本”说明和唯一主操作“领取独立副本”；本轮用测试数据 `Ready Workspace`，领取后副本进入接收者列表。所有流量都由本机合成 API 提供。
- 读取预览失败时还有一个条件式“重新加载”按钮，用于重试分享预览请求；成功领取页面没有该按钮。它不在设计稿中，已按“额外按钮需审核”的要求询问是否保留或移除，答复前不改。接收者页面无设计稿对应态，且该失败态按钮仍待决定，因此 MDA-14 继续未勾选。

### 2026-11-09 · MDA-03 任务行文字间距回归与原型逻辑走查

- 按 HTML `.row .tt b`、`.row .tt span`、`.row .tt time` 调整任务标题字距/多行行距、摘要行距及文字块间距；保留 91.6pt 首屏行高、五条真实样例形态、状态/操作语义和控件数量，不显示预算。
- `testMordeTaskListRendersFiveDesignSampleRows`、`testMordeTaskListRendersKnownAndUnknownStatusesWithoutActions`、`testMordeTaskListShowsStepCountsFromSummary` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 3/3（`/tmp/MDA03-row-spacing-20261109.xcresult`）。五行截图 `/tmp/MDA03-row-spacing-attachments-20261109/74D85B20-8836-4FFC-BD6D-66C9181BF8F9.png` 已检查，步骤计数和未知状态断言通过。
- 内置浏览器打开本地 HTML 任务页，确认五行样例可见；点击“发布 v2.4.1”进入详情，再按返回恢复任务列表。HTML 示例详情仍含预算文案；Morde 按用户已审核决定继续隐藏预算，不照搬该示例字段。
- MDA-03 仍保持未勾选：本机没有 iOS 16.0～16.3 runtime，完整多状态整页视觉验收也未闭环。

### 2026-11-09 · MDA-07/08 空态与工作项表单局部视觉复核

- MDA-07 资源页对照本地 HTML `.row .tt b` 的 1.3 行高为 Host/Workspace 多行标题补 1.5pt 行距；实际确认为空的云电脑、云手机、Workspace、AI 工具区改用 44pt 圆形图标底、13pt 图标/说明间距及 14pt 说明文字，继续保留真实模型符号、原有说明/标识和空态语义。网络错误状态沿用原样，不把失败误报为空；没有新增/删除按钮或改变资源加载及权限行为。
- MDA-08 新建标题输入字号对齐 `.wi-in input` 的 15px，外层保留 44pt 最小高度；不改类型、描述、创建、权限、筛选和行操作。最初尝试用 XCTest `TextField.frame.height == 44pt` 验胶囊尺寸，但该无障碍框实际返回内部文本输入区域 42.2488pt，固定外框后读数也不变，因此移除这条无法测量背景胶囊的断言；保留页面可见性与类型按钮断言，并检查最终表单截图。
- iPhone 18 Pro / iOS 27.0 独立 Morde Simulator 对 `testMordeResourcePreferenceFailureKeepsConfirmedEmptySections`、`testMordeResourceWorkspaceRouteBrowsesSyntheticManifest`、`testMordeWorkItemsRouteShowsDesignFiltersAndCreateForm`、`testMordeWorkItemCreateFailureKeepsFormAndInput` 定向回归最终合计 4/4 通过：前三项资源页/创建失败保留输入通过 `/tmp/MDA07-08-visual-retest-20261109.xcresult` 中 3/4；移除无效尺寸断言后，工作项列表与创建表单用例单独通过 1/1（`/tmp/MDA08-workitem-form-final-20261109.xcresult`）。工作项最终截图 `/tmp/MDA08-workitem-form-final-attachments-20261109/4585BAFC-A731-4ABB-8CAC-7F850872E04C.png` 已检查，Resource/Workspace 代表截图 `/tmp/MDA07-08-visual-retest-attachments-20261109/B328069E-75F3-4256-9380-B56D78F7BC92.png` 保留在同一资源包中。没有连接真实 Portal/Host，也未外发文件。
- MDA-07 仍因 Host 控制/租约等设计稿外信息待审核及完整逐页视觉验收未闭环而未勾选；MDA-08 仍因项目/Task 上下文、可选描述、额外行操作待审核，以及错误/空态、键盘/行操作和整页视觉验收未完成而未勾选。按用户已审核结论，MDA-04 详情读取、事件流、未确认事件历史的三处页面级 Retry 均移除，返回任务列表后重进重新读取；事件自动重连和失败批次业务重试保留。

### 2026-11-09 · MDA-04 时间线多行文字行距

- 对照 HTML `.ev b` 的 15.5px / 1.35 行高与 `.ev span` 的 14px / 1.5 行高，在 Morde 真实步骤及 Task 活动时间线的标题/说明补入对应的 2.5pt 与 4pt 多行间距；将摘要/步骤内容栈间距校准至 CSS 相邻 margin 折叠后的 14pt。字号、颜色、节点 24pt 尺寸、文本缩进、20pt 行间留白、事件排序和业务状态均不变；该调整只改变文字换行行距和摘要与首节点间距，不改变控件数量、操作或数据。
- `testMordeTaskDetailShowsRealStepsWithoutBudget` 在 iPhone 18 Pro / iOS 27.0 独立 Morde Simulator 通过 1/1（`/tmp/MDA04-task-detail-spacing-final-20261109.xcresult`）。改后截图 `/tmp/MDA04-task-detail-spacing-attachments-20261109/221E2A16-2660-460F-9F0D-5D96062BD6AF.png` 已检查，确认页头、真实步骤/活动节点、预算隐藏和已审核的底部暂停/取消入口未回退；本轮合成标题/说明均为单行，换行行距以源码对照为依据，仍需在较长内容状态中直接核对。
- MDA-04 继续未勾选：完整逐控件截图对照、换行文本状态，以及 HTML 单一“急停”与用户已审核保留的真实暂停/继续/取消入口之间的已知差异仍需完成记录/验收；活动历史和交付物区仍按待审核表保留。

### 2026-11-09 · MDA-12 全屏附件预览 TabBar 无障碍导航修复

- 同样移除了 Task Quick Look 预览 TabBar 容器上的重复 ID。`testMordeTaskArtifactQuickLookShowsSyntheticPreview` 通过 1/1（`/tmp/MDA12-quicklook-tabbar-id-20261109.xcresult`），断言聊天/任务/审批/资源四个 Tab 均以独立 ID 暴露且可点击，并保留返回文件库、恢复“AI 产物”筛选的检查。截图 `/tmp/MDA12-quicklook-tabbar-id-attachments-20261109/8B3A5079-1585-414D-B338-0C34D151922A.png` 和可见按钮清单已检查，没有分享/保存按钮。
- 内置浏览器打开本地 HTML `file` 附件详情样例，与 Morde 合成 PDF 截图 `/tmp/MDA12-shared-preview-tab-attachments-20261109/116E5AB7-CB8C-4602-9270-1D9A25694ABA.png` 以及 Task Quick Look 截图 `/tmp/MDA12-quicklook-tabbar-id-attachments-20261109/8B3A5079-1585-414D-B338-0C34D151922A.png` 对照，确认返回/标题、预览内容卡、底部四 Tab 的层级与位置一致；Morde 按文件展示真实合成 PDF/构件内容，而不把 HTML 样例正文伪装成文件数据。原型走查未上传或发送文件。
- MDA-12 页面验收完成：权限/证明/完整性边界见本清单已有定向 UI 记录；成功、加载、失败、取消及返回路径均有对应证据。本轮只使用本机合成图片/PDF，没有访问真实 Portal/Host 或发送真实文件。

### 2026-11-09 · MDA-13 设置根页 CSS 复核与详情审核门槛

- 重新核对本地 HTML `.sect`、`.srow` 样式：Morde 组标题的 16pt/700、首组 8pt 顶距、后续 24pt 组间距和 2pt 标题底距，与 CSS 对应；行文字为 16pt/600、上下各 13pt、值 14pt/500，chevron 继承深色字。最新设置根页截图 `/tmp/MDA-three-pages-light-attachments-20261108/AC9DB136-2D34-4105-B1D0-5631564BA47D.png` 与 HTML 的分组、十行顺序、返回页头及底部 Tab 对照通过。无需改动代码。
- 设置详情的额外页面/操作仍需用户审核，依据本清单“接口与待决能力”表：设计稿点击只提示而当前进入空详情；全局急停后的同一行当前直接重新启用；设备页显示 Hub relay URL/远程打开、安装撤销及状态刷新；账号页显示 Host members、Model connections、API Keys、安装管理和退出，API Keys 路径重复。答复前不删除、迁移或增加这些能力；MDA-13 继续未勾选。
- MDA-13 继续未勾选：设置详情去留与急停恢复仍待用户审核，设备/账号详情状态和整页逐控件视觉对照未闭环。MDA-12 已完成；本轮只使用本机合成图片/PDF，没有访问真实 Portal/Host 或发送真实文件。

### 2026-11-08 · MDA-04 决定复核、MDA-14 根页与 MDA-15 云电脑视觉复验

- MDA-04：用户最新确认移除详情读取失败、事件流失败、未确认历史的三处页面级“重试加载”，返回列表后重进恢复。该行为和此前专项回归一致，本轮不改代码；详情/历史重新读取、事件流自动重连、失败批次业务重试的现有证据见 `/tmp/MDA04-retry-reaffirmation-20261108.xcresult`（3/3）。
- MDA-14：用本地 HTML `ws` 页面复核后，将根页视觉 fixture 的 Host 名改为 `PC-01`；目录/文件行按 CSS 顶部对齐并校准目录箭头；文件行显示由真实扩展名推导的 `TS`/`MD` 类型和 API 返回的实际大小；分享弹层标题、字号和字距对齐 HTML“分享 Workspace”。不新增按钮或修改服务端合同。根页/`pay` 返回用例通过 1/1（`/tmp/MDA14-root-metadata-signed-20261108.xcresult`），截图 `/tmp/MDA14-root-metadata-attachments-20261108/4433DCF6-5978-47E9-A882-41794E8B7AA8.png` 已检查；分享权限/创建流程通过 1/1（`/tmp/MDA14-share-title-signed-20261108.xcresult`），弹层截图 `/tmp/MDA14-share-title-attachments-20261108/EAB06A91-29AA-42F1-BBE0-6907B3F3625D.png` 已检查。
- MDA-14 尚存数据差异：`pay` 目录说明“支付模块”、`callback.ts` 的变更摘要及目录内自定义排序，当前 Workspace manifest 没有对应字段，故继续使用通用“目录”、真实文件大小和 API 路径顺序，不伪造设计样例内容。已询问是否授权扩展可选 manifest 字段；收到答复前不改合同。MDA-14 保持未勾选。
- MDA-15：内置浏览器实际打开 HTML 云电脑样例；Morde Simulator 截图与页头、蓝色屏幕区、浏览器/终端/文件三入口、状态标签和双底部操作对照。最新合成 fixture 使用 `PC-01 · morde 仓库` 并带一个 Host 匹配活动 Task；续租、Host/租约倒计时、版本、长说明均不可见，“停止任务”为可用态。`testMordeComputerWebViewAlignsLegacyWorkbenchToDesignOnDevice` 通过 1/1（`/tmp/MDA15-cloud-design-sample-final-r2-20261108.xcresult`），截图 `/tmp/MDA15-cloud-design-sample-final-attachments-20261108/A4163310-3BA5-4C01-B76C-9411AF25E431.png` 已检查。真实 Gateway/远端桌面内容仍未验收，MDA-15 不勾选。
- 本轮均只读本地 HTML、使用本机合成 API/Workbench fixture 和 Simulator；没有访问真实 Portal/Host、没有发送或外发文件。

### 2026-11-08 · MDA-06/08 审批与工作项视觉复核

- MDA-06 浅色审批分隔线与拒绝按钮底色已按 HTML 分别对齐 `#EFEFEF`、`#F0F0F2`；深色模式沿用原语义色。MDA-08 新建表单主按钮最小高度从 48pt 收敛至 44pt。两项都只调整视觉。
- iPhone 18 Pro / iOS 27.0 独立 Morde Simulator 的 `testMordeApprovalInboxShowsOnlyConfirmedDecisions` 和 `testMordeWorkItemsRouteShowsDesignFiltersAndCreateForm` 通过 2/2，结果 `/tmp/MDA11-14-integrated-visual-20261108.xcresult`。审批、工作项列表和新建弹层截图 `/tmp/MDA11-14-integrated-visual-attachments-20261108/` 已检查；`git diff --check` 通过。该审批夹具只有一张卡，分隔线颜色以源码对照 HTML 核验，未声称截图已验证分隔线。
- 内置浏览器沿本机静态 HTML 原型打开审批和工作项页并核对控件/布局。Morde 当前工作项页比 HTML 多出项目选择、Task 上下文卡、可选描述及真实阻塞/认领操作；这些控件的用途和迁移建议仍按“接口与待决能力”表等待用户审核，未擅自删除。审批原型的第三动作、自动处理历史和变更摘要也仍待审核或缺少 API 字段，MDA-06/08 均保持未勾选。
- 用户再次确认移除 Task 详情读取失败、事件流失败和未确认历史的三处页面级“重试加载”；返回任务列表后重进恢复读取。本轮在当前代码上用 iPhone 18 Pro / iOS 27.0 Morde Simulator 重验详情权限错误后重进、事件流失败后自动恢复、未确认历史无 Retry，以及终态任务仍保留已审核操作，4/4 通过（`/tmp/MDA04-user-reconfirmation-current-20261108.xcresult`）。不改已确认的失败批次业务“重试”或事件流自动重连。另将 HTML `task` 原型和 Simulator 详情截图 `/tmp/MDA04-summary-spacing-align-final-attachments-20261108/5D23002A-BB26-407D-B6FE-24F1617E0FAA.png` 并排核对：返回/标题、步骤时间线、底部操作区和四 Tab 顺序/位置相符；HTML 的经理/预算演示摘要与七条示例节点没有复制到 Morde，Morde 显示真实状态和三条合成步骤；底部两个真实暂停/取消操作按用户已审核决定保留。Morde 仍多“交付物 / 暂无交付物”区，去留待审核；其余状态及全页逐控件尺寸复核未完成，MDA-04 保持未勾选。
- 本轮只使用本机合成 fixture 与本地 HTML；未访问真实账号、Host 或 Portal，没有发送/外发文件。

### 2026-11-08 · MDA-11 文件库筛选与来源差异复核

- 内置浏览器查看 HTML `lib` 页“全部”状态：页头后只有“全部 / AI 产物 / 我上传的”三项筛选和 7 条文件行；文件行无分享、保存等按钮。Morde 的同名页头、三项筛选、32×38pt 类型块、13pt 行边距和两行标题/来源布局与原型结构一致；浅色筛选/摘要/默认类型颜色已有上一轮校准。
- Morde “我上传的”合成截图 `/tmp/MDA11-library-colors-attachments-20261108/8FCBFB61-FBCD-4AB8-ACB6-8909C51E3BE6.png` 已检查，包含图片、PDF 与不可预览的 metadata-only 行；不显示预览/分享/保存额外按钮。当前“全部”还会展示 app 容器中的本机文件，HTML 原型没有该来源；隐藏行不会删文件，但会使它们不能从 Morde 文件库打开。该去留已单独询问用户，答复前保持现状，MDA-11 不勾选。
- 本轮只查看本地 HTML 与本机合成截图，没有打开或读取真实用户文件，也没有外发文件。

### 2026-11-08 · MDA-15 云端电脑当前实现复验

- 对当前 Morde 源码重跑 `testMordeComputerWebViewAlignsLegacyWorkbenchToDesignOnDevice`，iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA15-current-design-recheck-20261108.xcresult`）。新截图 `/tmp/MDA15-current-design-recheck-attachments-20261108/9839E39A-6E97-4FF1-87D6-A63906819A63.png` 已检查：常驻入口只有浏览器、终端、文件；页面保留标题、Host 名、屏幕区、状态提示和底部“停止任务 / 接管控制权”，没有续租、租约倒计时或冗长说明；点文件进入绑定 Workspace 后可返回资源页。
- 结果只覆盖本机合成 Workbench/manifest fixture，未连接真实 Gateway 或远程桌面。MDA-15 继续未勾选，真实远端内容与交互仍需验收。

### 2026-11-08 · MDA-06/08/11 审批、工作项与文件库逐页复核

- 内置浏览器在本地 `Morde iOS设计稿.html` 原型中检查 `appr`、`work`、`lib`：审批页核对两张审批卡的动作/事件信息，并点按“看事件链”确认路由；工作项页核对四项筛选、样例列表和新建表单字段，未提交创建；文件库核对“全部 / AI 产物 / 我上传的”筛选，切换到“我上传的”后打开 PDF，只观察到原型提示。以上只验证本机原型交互，不作为 Morde 原生功能验收。
- Morde iPhone 18 Pro / iOS 27.0 的页面回归首轮为 4/5：审批收件箱、metadata-only 行、工作项创建失败保留输入、工作项列表/筛选/新建表单通过；`testMordeFullLayoutUploadsAndSendsImageAndPDFFixture` 因重复使用的合成账号残留旧待投递队列而失败，截图显示旧失败项拦住本轮附件消息。这是测试夹具隔离问题，不据此判断产品附件发送失败。
- 将该全流程测试改为每次使用唯一合成账号、重置测试账号并启用按账号隔离的本机附件 fixture 后，重跑 `testMordeFullLayoutUploadsAndSendsImageAndPDFFixture` 通过 1/1（`/tmp/MDA11-chat-fixture-isolation-20261108.xcresult`）。新导出“我上传的”截图 `/tmp/MDA11-chat-fixture-isolation-attachments-20261108/AB5839C9-942B-4715-87E2-F41005AADC58.png` 已检查：图片、PDF、不可预览的 metadata-only 文件行正常显示，筛选选中态与底部导航可见；流程覆盖合成图片/PDF 发送、预览、返回及文件库筛选，不使用真实文件或账号。
- MDA-11 的本地文件、metadata-only 行等差异仍按待审核表保留；MDA-06 的第三动作/历史/摘要和媒体审批直达入口、MDA-08 的项目/Task 上下文/描述/行操作与筛选语义仍待用户审核。没有隐藏、删除或迁移这些额外功能，三页整页逐控件视觉与未覆盖状态仍未闭环，MDA-06/08/11 保持未勾选。
- 文件库三类行的标题文本已按 HTML `.row .tt { padding-top: 1px }` 增加 1pt 顶距，作用于本机文件、Task 构件和云端附件行；这只校准标题起点，不改数据、操作或权限。当前截图支持结构/筛选检查，1pt 差异不单凭肉眼截图声称完成精确视觉验收。
- 本轮只操作本地 HTML、Morde Simulator 和进程内合成 fixture；没有连接真实 Portal/Host、读取真实用户文件或发送/外发文件。当前可用 Simulator 仅含 iOS 27.0/27.1，iOS 16.0–16.3 runtime 验收仍不可执行。

### 2026-11-08 · MDA-03 有摘要与无摘要任务行高度复核

- 对照 HTML `.row` 的浏览器 DOM 测量（有摘要样例 91.59375px）及五行样例/状态矩阵截图，发现此前统一的 `minHeight: 91.6` 会把无步骤摘要的双行任务也撑到三行样例高度。现仅在任务确有列表详情（步骤计数/当前步骤）时保留 91.6pt 下限；无摘要项采用内容自然高度，状态/失败图标、行点击和滚动不变。
- 首次取消统一高度后，五行样例实际量为 89pt，因此没有沿用该试验；按已测 DOM 数值恢复为“有摘要 91.6pt / 无摘要自然高度”。新增稀疏行约束后，`testMordeTaskListRendersFiveDesignSampleRows` 与 `testMordeTaskListRendersKnownAndUnknownStatusesWithoutActions` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 2/2（`/tmp/MDA03-adaptive-task-rows-r3-20261108.xcresult`）。五行截图 `/tmp/MDA03-adaptive-task-rows-r3-attachments-20261108/06B9663D-5C79-452D-85A3-04483F36FFAF.png` 已检查，状态矩阵截图 `/tmp/MDA03-adaptive-task-rows-r3-attachments-20261108/A3EF6879-E29F-4049-B862-104C33E6AE83.png` 导出留档。
- 这只修正行高策略，没有改预算过滤、步骤统计、任务状态或任何按钮。完整任务页视觉及 iOS 16.0–16.3 runtime 仍待验收，MDA-03 保持未勾选。

### 2026-11-08 · MDA-04 Retry 审核落实与 MDA-08 标题焦点复验

- 按用户确认，Task 详情请求失败、事件流失败、事件历史未确认三处均不显示页面级“重试加载”；详情和历史在返回任务列表后再次进入时重新读取，事件流保留自动重连。iPhone 18 Pro / iOS 27.0 的 Morde Simulator 中，详情 403 返回重进恢复、事件流失败自动恢复、未确认历史返回重进恢复三项通过 3/3（结果包 `/tmp/MDA04-reentry-workitems-focus-final-20261108.xcresult`；同批次工作项聚焦断言首次失败，后续单独修复并复验）。失败批次项的业务重试仍保留。
- 工作项空标题按钮维持设计稿黑色外观。首次模拟器复验发现页面级焦点状态没有唤起键盘；现由标题输入组件自己的焦点状态接收空标题按钮的聚焦请求。`testMordeWorkItemsRouteShowsDesignFiltersAndCreateForm` 最终通过 1/1（`/tmp/MDA08-workitem-title-focus-r5-20261108.xcresult`）：空标题点击后键盘出现，未显示创建成功；表单截图 `/tmp/MDA08-workitem-title-focus-r5-attachments-20261108/267C4DFF-E1CE-4568-9302-0D37B87B9911.png` 已检查。`testMordeWorkItemCreateSubmitsToSelectedProject` 随后通过 1/1（`/tmp/MDA08-workitem-create-submit-final-20261108.xcresult`）：输入标题/描述、选择需求并成功创建，列表读回新项目。此前页面级焦点实现的失败结果 `/tmp/MDA08-workitem-focus-r4-20261108.xcresult` 已由上述最终复验取代。
- MDA-04 和 MDA-08 继续未勾选：剩余页面状态、待审核差异和整页视觉验收尚未闭环。本轮使用本机 Morde Simulator 及进程内合成 fixture，没有连接真实 Portal/Host，也没有外发文件。

### 2026-11-08 · MDA-01 导航与抽屉当前代码复验

- 使用独立 `Morde.xcodeproj` / `Morde` scheme，在 iPhone 18 Pro / iOS 27.0 Simulator 重跑 `testMordeDesignNavigationRoutes` 与 `testMordeDrawerGeometrySearchAndConversationPagination`，2/2 通过（`/tmp/MDA01-current-nav-recheck-20261108.xcresult`）。本轮确认四个 Tab、已审核侧边栏入口、项目/通知入口移除、设置/新聊天控件、旁聊搜索与分页断言均通过；UI 测试使用本机合成 fixture。
- 内置浏览器打开本机 HTML 原型，走查聊天、任务、审批、资源主导航以及抽屉搜索/旁聊入口；这只证明设计稿原型导航，不替代真实账号数据。当前 Simulator 仍没有可用真实登录会话，旁聊列表和审批角标的真实数量无法核验。
- MDA-01 源码实现、原型导航与合成 Simulator 几何/交互复验通过；真实登录态下的数据核验仍缺少可用会话，因此该页继续保持未勾选。本轮只访问本机静态 HTML 和本机合成 API，没有外发文件。

### 2026-11-08 · MDA-02 输入区与助手消息标识对齐

- 对照本地 HTML `chat`，Morde 输入区原先空态显示常驻麦克风、文本框可扩展至五行，助手消息也缺少逐条“Morde”署名。现将空态发送位置改为黑色上箭头并点按聚焦输入框；语音仍从已审核的长按菜单进入；输入改为单行；每条助手消息增加设计稿中的灰色 13pt semibold“Morde”标识。没有改动 Task 摘要卡及其待审核详情跳转，也未删除任何条件式恢复、历史分页或失败操作入口。
- 独立 `Morde` scheme 在 iPhone 18 Pro / iOS 27.0 Simulator 运行 `testMordeEmptyComposerSendFocusesInput`、`testMordeChatMultiTurnVisualFixtureMatchesDesignDensity`、`testMordeChatLongPressActionsOpenTaskOptionsAndImporter`，3/3 通过（`/tmp/MDA02-chat-composer-author-approved-20261108.xcresult`）。改后七条消息截图 `/tmp/MDA02-chat-composer-author-approved-attachments-20261108/2CCED47B-182C-4C79-B1BE-FD2FD005B049.png` 和聚焦输入/键盘截图 `/tmp/MDA02-chat-composer-author-approved-attachments-20261108/18F8A54D-BA89-49E9-9452-36358A7276D9.png` 已检查：每条助手消息显示署名、用户消息使用浅灰气泡、空输入发送箭头可见且不显示常驻麦克风；长按菜单仍能打开 Task options 和文件选择器。
- 本轮编译同时显示 `MordeApp.swift` 中多个 store 的既有 `ObservableObject` actor 隔离/unused-result 警告；不在本次聊天 UI 改动行内，记录为后续工程清理项。MDA-02 仍保持未勾选：真实登录数据、附件权限/失败/撤销状态、滚动和键盘其他状态、附件 chip 结构及整页逐控件对照尚未全部完成；Task 摘要卡详情跳转继续待用户审核。所有操作局限于本机源码、HTML 和 Simulator 合成 fixture，没有外发文件。
- 同一 Morde Simulator 单独复验 `testMordeFullLayoutUploadsAndSendsImageAndPDFFixture`，图片/PDF 合成上传发送、分别预览、返回后资源页/文件库筛选及返回聊天通过 1/1（`/tmp/MDA02-chat-attachment-visual-recheck-20261108.xcresult`）。已检查发送后图片/PDF消息截图 `/tmp/MDA02-chat-attachment-visual-recheck-attachments-20261108/42CD835D-33E9-4CE1-9C23-3684F3586EF3.png`：两个附件都在同一用户消息下呈卡片显示，无分享/保存入口；图片与 PDF 预览截图分别为 `A0ABF3C3-2363-44F1-A7FC-68CA3E783F80.png`、`1B9B0495-245B-4256-9464-D17697968C8F.png`。仅使用测试生成的附件和进程内本地 API 替身。

### 2026-11-08 · MDA-06 审批页原型结构复核

- 在本机 HTML `appr` 页复核到页头“审批 / 待审批数”、两张待办卡片；卡片含审批事实、三项操作与“看事件链”，列表后还有“已自动处理”历史。设计样例的预算/金额依已审核决定不复制到 Morde。
- 对照当前 Morde 合成审批截图 `/tmp/MDA-turn-integrated-20261108-approval-attachments/B6970328-5936-475D-B867-AFD86F8E76FC.png`：可见页头、单张合成待办、驳回/批准及事件链入口，没有预算；合成夹具只有一张待办，不能与原型的样例数量直接比较。第三动作、自动处理历史/摘要仍受当前 API 合同及“接口与待决能力”表审核限制，答复前不补造数据、操作或按钮。
- 本轮只复核本地 HTML DOM/截图与已有本机 Simulator 截图，没有改代码或运行测试。审批页其余状态和完整逐控件尺寸/位置并排核对仍未完成，MDA-06 保持未勾选。

### 2026-11-08 · MDA-04 Retry 决定复核、MDA-07 旧界面清理与 MDA-08 V2 门控

- 用户再次确认 Task 详情读取失败、事件流失败、未确认历史的三处页面级“重试加载”均移除；返回列表后重进重新读取，事件流自动重连和失败批次业务重试保留。该决定及既有 3/3 恢复证据已在上方待审核表和 MDA-04 执行记录中登记，本次不重复改产品逻辑。
- 清除了已无导航入口的旧 `.computers` 根页面、旧资源标签与含“续租 30 秒”/租约期限说明的 Host 控制面板，避免它与当前 Morde 资源页/云电脑路由并存；MordeComputerDesignRoom 的静默租约维护、接管/交还权限和当前设计稿路由保留。资源 Host 行也隐藏租约状态/到期时间和交接阶段文字，仅保留一行控制状态与必要错误；客户端仍用真实租约/转场值做安全门控。资源组件不再提供手动 `renewLease` 操作。
- 工作项页面固定走 `project_work_items_v2`：服务不支持时清空并停止调用旧列表接口，显示本地化不支持状态且禁用创建；创建和认领均要求 V2 capability。同步将本地工作项 Simulator fixture 标记为支持 V2，避免把旧 fixture 能力误当成生产服务合同。
- `Morde` / `MordeUITests` 编译完成；本轮 Simulator 页面验收未完成：iPhone 18 Pro UI 用例在登录前置出现 `NSURLErrorDomain -3003`，没有到达页面断言。用新建干净 Simulator 重试同样停在该前置，结果 `/tmp/MDA-workitem-final-clean-20261108.xcresult`。这不能证明本轮页面交互通过，也不能据此判为产品回归；需先恢复本机登录 fixture 路由后再重跑。
- `git diff --check` 通过；MDA-04、MDA-07、MDA-08、MDA-15 均继续未勾选，需完成受阻回归与各页剩余审核/视觉门槛后再关闭。本轮只改本机源码与文档，没有发送或外发文件。

### 2026-11-08 · Task Retry 决定复核与云电脑界面验收

- 用户再次确认移除 Task 详情请求失败、事件流失败、事件历史未确认时的 3 个页面级“重试加载”按钮；从任务列表返回并重新进入时重新读取。该规则已经落实，事件流自动重连与失败批次的业务重试继续保留，本轮不重复改产品代码。
- 内置浏览器在本机 HTML 云电脑页核对常驻浏览器/终端/文件入口、简短控制状态及底部“停止任务 / 接管控制权”操作；Morde 原生页在隔离的 iPhone 18 Pro / iOS 27.0 Simulator 重跑 `testMordeComputerWebViewAlignsLegacyWorkbenchToDesignOnDevice`，1/1 通过（`/tmp/MDA15-current-user-brief-20261108.xcresult`）。断言确认没有 Workspace、VS Code、续租、持有倒计时、控制版本和长说明；文件入口进入绑定 Workspace 后可返回资源页。截图 `/tmp/MDA15-current-user-brief-attachments/5A5F7CD1-47CA-43E3-BA2B-3A0E822E44E7.png` 已检查，当前原生页与设计稿的主要层级及按钮位置一致。
- 本次只用本机 HTML、本机合成 API/Workbench fixture 和临时 Simulator；未连接真实 Gateway/Host，也没有发送或外发文件。MDA-04 因全页状态及活动/交付物区待审核而保持未完成；MDA-15 因真实 Gateway/WebView 仍未验收而保持未完成。

### 2026-11-08 · Task Retry 决定复验、聊天/Host 权限回归与任务“＋”路径

- 用户确认删除 Task 详情页的三处页面级“重试加载”按钮；详情失败和未确认历史返回列表后重进重新读取，事件流失败自动重连。独立 Morde Simulator 上 `testMordeTaskDetailShowsPermissionErrorWithoutTaskActions`、`testMordeTaskDetailTimelineFailureHasNoRetryButtonAndRecoversAutomatically`、`testMordeTaskDetailUnconfirmedHistoryHasNoRetryButton` 通过 3/3（`/tmp/MDA04-retry-confirm-current-20261108.xcresult`）。本次没有增加页面按钮；失败批次的业务“重试”仍保留。
- 聊天空输入仍从发送箭头聚焦文本框；视觉 fixture 已同步到本地 HTML 的七条对话、移除预算示例，并补上每条助手“Morde”署名和设计稿空闲状态“努力工作中”。Morde UI 回归确认聊天 fixture 与空输入发送行为通过。共享 Host 权限也统一按 `desktop:view` 或 `desktop:control` 判断可查看；仅授予 `desktop:control` 的合成成员可打开云电脑、接管并交还。`testMordeChatMultiTurnVisualFixtureMatchesDesignDensity`、`testMordeEmptyComposerSendFocusesInput`、`testMordeComputerRouteAcceptsSyntheticTicketAndReturnsToResources`、`testMordeSharedHostDesktopControlMemberCanTakeOverAndRelease` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 4/4（`/tmp/MDA02-MDA15-design-permission-final-20261108.xcresult`）。云电脑用例继续断言无续租按钮、租约倒计时或冗长说明。
- 对 MDA-03 增补了 Morde 专属的“任务页＋ → 新聊天 → 输入框聚焦”UI 用例，不把旧 `Minis.xcodeproj` 的同类用例当成新 App 证据。`testMordeTaskPlusCreatesConversationAndFocusesComposer` 在干净 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA03-task-plus-focused-composer-20261108.xcresult`）；截图 `/tmp/MDA03-task-plus-focused-composer-attachments-20261108/369F3BCD-D68A-4E68-BD7D-B0276520D2F6.png` 已检查，可见聊天页输入光标、键盘与四个 Tab。
- 所有验证均使用本地合成账号/fixture；未连接真实 Portal 或 Host，未传输或外发文件。MDA-02 因附件权限/失败态及整页视觉未闭环，MDA-03 因多状态整页对照和 iOS 16.0–16.3 runtime 未验，MDA-04 因活动历史/交付物待审核与整页对照未完成，MDA-07 因 Host 列表扩展状态待审核，MDA-15 因真实 Gateway/WebView 未验及整页视觉未闭环，均保持未勾选。

### 2026-11-08 · MDA-03 状态覆盖与设计稿“新建任务”路径复核

- 用户再次确认移除 Task 详情请求失败、事件流失败、事件历史未确认时的 3 个页面级“重试加载”；现有实现和三状态恢复验收已符合决定。本次不改产品逻辑，失败批次业务“重试”与事件流自动重连继续保留。
- 新增的 Morde 专属任务状态合成用例 `testMordeTaskListShowsTransitionAndWaitingReasonStatuses` 在 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA03-transition-waiting-statuses-20261108.xcresult`）。截图 `/tmp/MDA03-transition-waiting-statuses-attachments-20261108/64540486-EBE1-4E0E-ACD3-4BDFB9CA8ADE.png` 已检查，覆盖暂停/停止转场、审批等待、澄清等待及操作结果待核对状态；列表未出现额外页面操作按钮。
- 内置浏览器在本地 HTML 设计稿中进入“任务”页并点击“新建任务（＋）”，原型返回聊天页且输入框获得焦点；与 Morde `testMordeTaskPlusCreatesConversationAndFocusesComposer` 的独立 Simulator 证据一致（1/1，`/tmp/MDA03-task-plus-focused-composer-20261108.xcresult`）。MDA-03 仍因全页其他状态/完整视觉对照及缺少 iOS 16.0–16.3 runtime 未完成而保持未勾选。
- 本轮只改本机实施文档，未发送或外发文件。设置页接口事实已按当前 OpenAPI 与客户端 `MordeMe` 解码范围更正；预算/用量显示仍遵守用户已删除预算显示的决定。

### 2026-11-08 · MDA-13 设置根页与设备详情当前代码复验

- 内置浏览器复核本地 HTML：设置页三组 9 个详情行加急停控件；点击“设备”只显示“打开「设备」”提示。Morde 页面使用真实设备管理详情，属于已记录的设计差异。
- 独立 `Morde.xcodeproj` / `Morde` scheme 在 iPhone 18 Pro / iOS 27.0 Simulator 上运行 `testMordeSettingsRoutesAndHonestUnavailableRows`，ad-hoc 签名后通过 1/1（`/tmp/MDA13-settings-routes-adhoc-20261108.xcresult`）。覆盖设置根页、账号详情、设备详情、五个不可用详情和关于页；设备的远程打开授权弹窗已取消。截图 `/tmp/MDA13-settings-routes-adhoc-attachments-20261108/04106E2F-0CBD-4A5F-BD98-8D9200A28ECA.png` 已检查。`CODE_SIGNING_ALLOWED=NO` 的首次尝试在本地登录夹具阶段失败，没有到页面断言；ad-hoc 签名复验通过。
- MDA-13 保持未勾选：设备详情的 Hub 远程打开/安装撤销与两个条件式 Retry、账号页管理入口、五个占位详情去留等待审核；安全凭证页和剩余详情的完整逐控件视觉/状态尚未完成。测试只走本机 URLProtocol，不提交远程打开、不撤销安装、不访问真实 Portal/Hub。

### 2026-11-08 · MDA-14 Workspace 分享弹层视觉收敛

- 按 HTML `.sheet` 与 `.perm` / `.copy` CSS 将 Morde 分享弹层调整为水平 22pt、上下 22/30pt 留白、13pt 权限行留白、16pt 主按钮间距和 221pt detent；选中圆点与“复制分享链接”按钮改为黑色，按钮文字/字号和权限说明按原型收敛。只改视觉布局，保留 view/edit 权限及分享 API 行为。
- `testMordeWorkspaceShareCreatesLinkWithSelectedPermission` 在独立 `Morde` scheme、iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA14-share-sheet-align-20261108.xcresult`）；截图 `/tmp/MDA14-share-sheet-align-attachments-20261108/8B2333B1-4B3A-4A22-BC3A-A3B0C84F8A36.png` 已检查，权限可切换且创建链接成功，圆点、按钮颜色、弹层边距与 CSS 几何对应。
- MDA-14 仍未完成：领取预览失败 Retry 去留、升级前 `claiming + target` 链接兼容待用户决定；manifest 元数据/排序差异、分享领取失败与其它状态及整页截图核对仍未闭环。

### 2026-11-08 · MDA-03/MDA-04 任务页截图复核与 Retry 决定确认

- 将本机 HTML 任务列表与 Morde `testMordeTaskListRendersFiveDesignSampleRows` 截图并排复核：页头、44pt 状态图标、五行信息层级、行间距及“＋”和底部四 Tab 的顺序/位置相符；状态次级文案来自真实 Task 字段。没有发现可安全单独调整的几何差异，因此本次不改产品代码。
- 将 HTML 任务详情与 Morde `testMordeTaskDetailShowsRealStepsWithoutBudget` 截图复核：38pt 返回键、24pt 时间线标记、步骤文字列起点、节点间隔、底部操作区和四 Tab 位置相符；Morde 不显示预算。底部暂停/取消按用户已审核决定保留；交付物区仍是待审核的额外区域，不在本次擅自移除。
- 用户再次确认移除详情读取失败、事件流失败、未确认历史三处页面级“重试加载”，返回任务列表后重进恢复读取。现有实现和三状态 Morde Simulator 回归 3/3 已满足该决定；事件流自动重连及失败批次业务“重试”保留，无需重复改代码。
- MDA-03 因完整状态视觉和 iOS 16.0–16.3 runtime 未验仍保持未完成；MDA-04 因活动/交付物待审、其他状态和完整逐控件核对未闭环仍保持未完成。本次仅核对本机 HTML 与本机 Simulator 截图。

### 2026-11-08 · MDA-08 工作项弹层复核与 MDA-11 文件库空态文字微调

- MDA-08 对照本地 HTML 的 `wiSheet`，隐藏原生新建工作项 sheet 的系统拖动指示条，匹配设计稿没有抓手条的弹层；下滑关闭仍保留。干净 iPhone 18 Pro / iOS 27.0 Morde Simulator 上 `testMordeWorkItemsRouteShowsDesignFiltersAndCreateForm` 单项复跑通过 1/1（`/tmp/MDA-workitems-route-retry-20261108.xcresult`），覆盖登录后打开侧栏、进入工作项、切换项目、筛选类别/待认领、显示创建弹层，以及空标题提交时聚焦标题且不发请求。已检查列表截图 `/tmp/MDA-workitems-route-retry-attachments-20261108/0A5BE6F3-011C-43F3-9391-9CB71E09E065.png` 和弹层截图 `/tmp/MDA-workitems-route-retry-attachments-20261108/0FD4D148-F122-48D3-9087-1C320CE19DD4.png`，确认弹层无拖动指示条且表单可见。此前复用 Simulator 的合批运行中侧栏条目未出现；干净 Simulator 单项复跑未复现，故不判为产品回归。
- 同一轮将 `MIFLibraryViews.emptyRow` 的说明字号校准为 14.5pt、顶部间距 7pt，对齐 HTML 空态文字。`testMordeLibraryShowsUnavailableInsteadOfEmptyWhenPersonalSyncFails` 在该轮通过；集成结果 `/tmp/MDA-implement-current-20261108.xcresult` 共 4/5 通过，唯一失败为工作项侧栏入口缺失，随后已由上述干净 Simulator 单项复跑通过。工作项列表/创建错误保留输入与成功创建用例也在该轮通过。
- 内置浏览器本机原型实际操作：从任务页打开侧栏进入工作项，点“缺陷”后只显示缺陷行；打开“新建工作项”，填写本机临时标题并创建，看到“已创建 · 团队可见”和新增行；随后重载静态 HTML，确认临时演示数据已重置。此流程只验证设计稿原型，不代表 Morde 服务端联调。
- MDA-08 仍未勾选：项目/Task 上下文、描述、额外行操作与待认领语义仍待用户审核，行操作/键盘及整页逐控件视觉验收未闭环。MDA-11 仍未勾选：本机文件及 metadata-only 行处理、权限/错误状态和全页视觉仍待验收。以上只改本机源码/清单，未连接真实 Portal、未传输或外发文件。

### 2026-11-08 · MDA-04 三处页面级 Retry 最终审核确认

- 用户确认移除 Task 详情请求失败、事件流失败、事件历史未确认状态中的三处页面级“重试加载”，回到任务列表后重新进入恢复读取。现有实现符合此决定，源码未发现 Task 详情的页面级 Retry；唯一“重新加载”按钮属于附件预览，未作改动。事件流自动重连与失败批次的业务重试保留。
- `testMordeTaskDetailShowsPermissionErrorWithoutTaskActions`、`testMordeTaskDetailTimelineFailureHasNoRetryButtonAndRecoversAutomatically`、`testMordeTaskDetailUnconfirmedHistoryHasNoRetryButton` 此前已在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 3/3（`/tmp/MDA04-retry-confirm-current-20261108.xcresult`）。本轮只复核源码和登记审核答复，不重复运行测试。
- MDA-04 仍因活动/交付物区待审核、其余状态及整页逐控件核对未闭环而保持未完成。

### 2026-11-08 · MDA-07 资源样式审计与 MDA-11 文件标题行距集成

- MDA-07 复核本地 HTML `res` 页与 `MIFResourceViews.swift`：菜单/加号尺寸、页头和标题、分组层级、首段/后续间距、Host/Workspace 平铺行、图标几何、文字字号/颜色及状态位置均有对应实现；没有发现需要新增或改动的纯样式差异，因此未修改资源页代码。剩余差异是无实时数据合同的云手机/AI 工具样例，以及 Host 权限/控制/错误状态和交还动作；这些涉及真实数据或待审核范围，不隐藏、不伪造。现有资源截图使用多状态夹具，不能证明常规资源首屏的完整视觉验收，MDA-07 继续未勾选。
- 内置浏览器在本机 HTML 实际操作资源页：打开 PC-02 后显示浏览器/终端/文件三个入口和任务控制，关闭后返回资源；打开“morde 仓库”进入 `pay` 子目录，面包屑可回根目录、页面返回可回资源；点击“添加资源”显示原型提示“可添加云主机、Workspace 或 AI 工具”。这只验证本地设计稿路由与交互，不代表 Morde/Gateway 服务联调。
- MDA-11 将本机文件、Task 构件和云端附件三类行标题的多行间距统一为 1.5pt，对齐设计稿 `.row .tt b { line-height: 1.3 }`；筛选、数据范围、预览权限及按钮行为未改。iPhone 18 Pro / iOS 27.0 Morde Simulator 的 `testMordeFullLayoutUploadsAndSendsImageAndPDFFixture`、`testMordeLibraryMetadataOnlyRowsAreVisibleButNotPreviewable`、`testMordeLibraryShowsUnavailableInsteadOfEmptyWhenPersonalSyncFails` 通过 3/3（`/tmp/MDA11-library-row-title-integrated-20261108.xcresult`）。测试覆盖图片/PDF筛选与预览、metadata-only行不提供预览入口，以及同步失败显示“不可用”而非空库。
- 已检查文件库“我上传的”截图 `/tmp/MDA11-library-row-title-attachments-20261108/B6B35F47-9D9E-4187-9A66-2ECF9E20458B.png` 与“全部”metadata-only截图 `/tmp/MDA11-library-row-title-attachments-20261108/C289DF67-FD27-4B0C-A131-CAF1E5CCE697.png`；行标题/次级信息、筛选与底部 Tab 可见。现有 fixture 标题均为单行，1.5pt 多行间距尚无长标题截图单独验证；CSS 到 SwiftUI 的静态映射和编译已确认。MDA-11 仍因权限证明/其他状态及整页逐控件视觉验收未完而保持未勾选。

### 2026-11-08 · MDA-02 聊天首屏空白间距对照

- 用内置浏览器重新查看本机 HTML `chat` 原型，确认消息列 `gap:18px`、底部留白 `16px`，输入区顶部留白 `6px`；预算样例仍按已审核决定不放入 Morde。
- 最新 Morde Simulator 截图确认消息列到输入框之间有额外空白。将 ScrollView 末尾为 composer/底栏预留的额外间距从 96pt 调整为 22pt，对应原型的 16pt + 6pt；滚动避让、输入框和导航栏位置未改。`testMordeChatMultiTurnVisualFixtureMatchesDesignDensity` 在当前源码的 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA02-chat-spacing-adjusted-20261108.xcresult`），截图 `/tmp/MDA02-chat-spacing-adjusted-attachments-20261108/1A714ADF-80B3-4ED5-B52A-DFB193044D40.png` 已检查。
- `testMordeEmptyComposerSendFocusesInput` 在当前源码的 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA02-keyboard-after-spacing-20261108.xcresult`）；键盘截图 `/tmp/MDA02-keyboard-after-spacing-attachments-20261108/0857BBF5-CE9C-4023-8199-0C96A34B9920.png` 确认输入区位于键盘上方、四个 Tab 仍可见且没有提交空消息。
- 短消息截图底部可见空白比 HTML 预览更大；源码核对解释了差异：HTML 预览设备为 390×812，Simulator 为 402×874（高 62pt），且 Morde 按用户决定删除了 HTML 第一条消息中的预算，少一行内容。消息短于可视区时列表自然留白，不应通过伪造内容或把消息整体推到底部来消除。22pt 末尾预留已按 `.chat` 16pt + `.composer` 顶部 6pt 对齐；Task 卡片待审、长短内容与附件状态全页核验仍未闭环，MDA-02 继续未勾选。没有改动已审核的附件/语音菜单入口。本轮只访问本机 HTML 与本机 Simulator，没有发送或外发文件。

### 2026-11-08 · MDA-02 输入胶囊尺寸对齐

- 对照本机 HTML `.composer-in`：原型外胶囊高度约 46pt、发送圆盘 34pt、输入与发送间距 8pt；Morde 原先沿用 nanoMuse 44pt 行高并额外加 8pt 内边距，胶囊约 60pt。新增仅供 Morde 使用的紧凑几何选项，将胶囊压至约 46pt、发送圆盘设为 34pt、字段间距设为 8pt，同时保留 44pt 点击区域、nanoMuse 灰色胶囊色板和发送/停止/队列逻辑；旧 nanoMuse 调用沿用原尺寸。未增加、删除或移动可见控件。
- 第一次视觉复验在本地登录夹具密码框没有取得键盘焦点后失败；测试登录 helper 已改为字段中心点按、等待键盘并确认登录按钮启用。独立干净 iPhone 18 Pro / iOS 27.0 Simulator 上，`testMordeChatMultiTurnVisualFixtureMatchesDesignDensity` 通过 1/1（`/tmp/MDA02-chat-composer-clean-sim-20261108.xcresult`），截图 `/tmp/MDA02-chat-composer-clean-sim-attachments-20261108/154A149B-4241-4CD5-85A0-2391F51F9A3E.png` 已检查，确认输入胶囊与消息流、四个 Tab 同屏可见。改尺寸后的 `testMordeEmptyComposerSendFocusesInput` 通过 1/1（`/tmp/MDA02-chat-composer-compact-keyboard-retry-20261108.xcresult`），键盘截图 `/tmp/MDA02-chat-composer-compact-keyboard-attachments-20261108/00556605-874B-4240-B4EF-04251D71DC23.png` 已检查：输入焦点、发送位与四个 Tab 仍可见；一次前置 runner SIGTERM 后，重启临时 Simulator 复跑通过。内置浏览器在本机 HTML 聊天页发送“本地原型走查”，确认原型消息回显、输入清空；该回复为原型演示，不代表 API 联调。Task 卡片和其他待审核控件、附件错误/撤销、长短内容完整逐屏核验仍未闭环，MDA-02 保持进行中。

### 2026-11-08 · MDA-02 附件 App 重启后续传复验

- 独立 `Morde.xcodeproj` / `Morde` scheme 在专用 iPhone 18 Pro / iOS 27.0 Simulator 运行 `testMordeChatAttachmentUploadResumesMissingChunksAfterAppRestart`，通过 1/1（`/tmp/MDA02-attachment-resume-20261108.xcresult`）。该用例用合成 JPEG 触发分块上传中断，确认文本消息先被接受、App 重启后保留消息并续传缺失分块，最终完成上传和附件关联；未访问用户文件或真实服务。
- 启动阶段出现一次本地合成请求失败提示后，测试 helper 按既有流程重新登录并继续；最终测试结果为通过，不能据此推断部署环境网络状态。未改产品代码或待审核控件。聊天附件失败/撤销呈现、待决的 Task 摘要卡入口及逐页全屏视觉仍未闭环，MDA-02 保持进行中。

### 2026-11-08 · MDA-02 活动 Task 附件响应丢失恢复

- `testMordeChatComposerReconcilesQueuedAttachmentsAfterLostResponse` 首次复验在两条附件可点击状态断言失败；失败前的本地录屏显示稳定消息只有一条，图片/PDF 已关联并显示“已保存”，助手也已收到附件成功回执。检查发现该测试使用即时 `.exists`，而同工程文件库/聊天附件验收使用 `waitForExistence` 等待异步个人库元数据。仅将这两处测试断言改为最多等待 10 秒；未改生产代码或用户可见控件。
- 更新后的用例在独立 `Morde` scheme、iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA02-queue-attachment-reconcile-retry1-20261108.xcresult`）：原始 client message ID 恢复为单条消息、图片/PDF 预览入口出现、附件成功回执可见且没有失败提示。该证明来自合成 fixture；聊天状态视觉和其余未审核入口仍需完成，MDA-02 保持进行中。

### 2026-11-08 · MDA-03 页头细节与本机 HTML 逐页走查

- 内置浏览器在本机 `Morde iOS设计稿.html` 操作任务页：查看任务列表，打开“修复支付回调超时”进入详情，再返回列表并点“新建任务”回到聊天输入框。该走查只验证 HTML 原型路由，不代表 Morde 服务/API 联调。
- 对照设计稿 `.head` / `.circbtn`：Morde 页头已有 22pt 水平边距、38pt 圆形按钮、10pt 标题间距与 52pt 总高度；进一步把 Morde 菜单图标由三横线改为设计稿的两条 19×2.1pt 圆角线，标题固定 17pt 并采用 -0.17pt 字距。保留 nanoMuse 表面色、描边和轻阴影；没有新增或删除可见控件。只修改 Morde 编译目标的 `MordeDesignChrome.swift`，旧 nanoMuse 源码未改。
- iPhone 18 Pro / iOS 27.0 Morde Simulator 的 `testMordeTaskListRendersFiveDesignSampleRows` 通过 1/1（`/tmp/MDA03-task-header-final-20261108.xcresult`）；截图 `/tmp/MDA03-task-header-final-attachments-20261108/88B0E9DF-4687-4CF3-B847-C9F9AB7517A7.png` 已检查，确认双线菜单、标题、右侧“＋”、五行任务和四个 Tab 同屏正常。
- `testMordeEventChainTimelineDensityAndStatusesMatchPrototype` 在合并运行及独立复跑中，UI Runner 均在建立测试连接前被系统以 signal kill 终止（`/tmp/MDA03-morde-page-header-20261108.xcresult`、`/tmp/MDA03-event-chain-header-retry-20261108.xcresult`），未进入页面断言；因此不记为事件链通过，也不能据此归因于页头改动。MDA-03 仍因完整状态/全页视觉和 iOS 16.0～16.3 运行时缺失等门槛保持进行中。

### 2026-11-08 · MDA-08 共享页头复核

- 页头改动后，`testMordeWorkItemsRouteShowsDesignFiltersAndCreateForm` 在 iPhone 18 Pro / iOS 27.0 Simulator 实际进入工作项列表与创建表单，并采集列表截图 `/tmp/MDA08-workitems-header-final-attachments-20261108/EF9E3189-013A-4609-9F10-7C8609ADB510.png`；截图确认工作项页也呈现双线菜单和正确标题位置。结果包 `/tmp/MDA08-workitems-header-final-20261108.xcresult` 最终仍因 UI Runner 在清理阶段 signal kill 被 XCTest 标记失败，不记为通过。空标题提交时还记录到已有 `Invalid frame dimension` 警告，源码栈无法定位到具体产品行；这两个问题需与页头视觉验收分开复核。MDA-08 继续未完成。

### 2026-11-08 · MDA-04 当前版本截图与原型逻辑复核

- 当前 Morde target 的 `testMordeTaskDetailShowsRealStepsWithoutBudget` 在 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA04-task-detail-current-20261108.xcresult`）。截图 `/tmp/MDA04-task-detail-current-attachments-20261108/21C05A27-DEC0-486D-AD08-19013DC14794.png` 已检查：标题/返回、任务状态摘要、真实步骤和时间线可见；暂停/取消操作固定在自定义 Tab 栏上方；预算字段不显示。当前屏幕仍显示详情内活动记录和交付物区，其保留/移除已有待审核记录，不擅自改动。
- 对照 HTML `.head`、`.lead-sub`、`.ev`、`.task-foot`：标题和返回键 38pt、左右边距 22pt；摘要后时间线首节点、节点文字列和 20pt 节点间距对应原型；底部操作区与 Tab 栏留白位置相符。暂停/继续/取消作为真实操作按用户已审核决定保留；不把它们改名为设计稿的“急停这个任务”，也不新增按钮。除两处待审核区域造成的整页差异外，本次未发现可在保留已审核操作前提下安全收敛的确定性几何偏差，因此未改 Task 详情产品代码。
- 内置浏览器对本机 `Morde iOS设计稿.html` 完成逻辑走查：任务列表 → 打开“修复支付回调超时” → 返回列表 → “新建任务”回到聊天并聚焦输入框。只验证本地 HTML 原型交互，不代表 Morde/Gateway 服务联调。
- Task 详情的三处页面级“重试加载”已按用户确认移除；返回列表后重进重新读取，事件流自动重连和失败批次业务重试保留。MDA-04 仍因活动/交付物区待审核、其他状态和整页逐控件视觉验收未闭环而保持进行中。

### 2026-11-08 · MDA-06 当前审批卡视觉复核

- 对照 HTML `.appr` / `.acts` 和 `MIFApprovalViews.swift`：卡片上下留白 16/18pt、标题 17.5pt 与 -0.175pt 字距、事实 14.5pt / #737378 / 约 23.5pt 行高、按钮间距 8pt 与顶部间距 14pt均有对应；按钮可点区域仍为 44pt，胶囊视觉高度约 40pt。最新合成截图 `/tmp/MDA03-06-retry-style-attachments/426C40FE-DCB4-45B3-96CE-D0AC4A426DD7.png` 已检查，显示页头待办数、平铺审批文本、拒绝/批准、事件链入口和四个 Tab。
- HTML 的第三种“灰度”动作、变更 diff/测试/成本事实和自动处理历史没有当前 API 合同；媒体审批直达带预览 Task 的入口也仍待用户审核。因此不添加/伪造这些内容，不改批准/拒绝权限或按钮顺序。MDA-06 因待审核行为、其他状态和全页逐控件视觉仍未闭环而保持未完成。

### 2026-11-08 · MDA-01 导航复验与 MDA-06 原型跳转走查

- 独立 `Morde.xcodeproj` / `Morde` scheme、iPhone 18 Pro Max / iOS 27.0 Simulator 上，正确过滤的 `IOS03SimulatorAcceptanceUITests.testMordeDesignNavigationRoutes` 通过 1/1（`/tmp/MDA01-navigation-current-20261108-pro-max.xcresult`）。同一用例先在 iPhone 18 Pro 上被 UI Runner 以 signal kill 终止；Pro Max 结果包明确记录 1/1 通过。首次错误过滤实际执行 0 项，不计作测试证据。真实登录账号的旁聊和审批计数仍不可由合成账号替代，MDA-01 保持未完成。
- 内置浏览器以仅绑定本机回环地址的静态 HTML 预览打开原型，进入“审批”页后点击“看事件链”，成功显示“发布 v2.4.1”事件链；这只验证 HTML 原型路由，审批卡的预算和演示历史不作为 Morde 产品数据合同。没有提交真实数据或外发设计稿。
- 本轮未改生产代码。MDA-01 仍待可用真实登录会话下的数据核验及全页视觉闭环；MDA-06 仍待额外动作/历史/媒体入口审核及完整状态视觉核验，均不勾选。

### 2026-11-08 · MDA-08 工作项列表与新建弹层差异复核

- 内置浏览器沿本机原型打开“工作项”并点击“新建工作项”：列表显示四个筛选和行内“认领”；新建弹层只显示“缺陷/需求”、标题输入和“创建·团队可见”，没有描述字段或项目/Task 上下文。原型的列表/弹层只验证控件和跳转，不提交 Morde 数据。
- 与当前 Morde Simulator 截图 `/tmp/MDA08-workitem-form-final-attachments-20261109/4585BAFC-A731-4ABB-8CAC-7F850872E04C.png` 对照，Morde 多出项目选择、Task 关联卡和描述输入，sheet 也明显高于原型。各项真实用途和精简建议已向用户征询；答复前不删除、隐藏或迁移相关操作，也不改变表单高度。既有筛选、创建/失败保留输入及权限用例证据不代表差异已审核。
- MDA-08 因这些设计稿外字段/操作待审核，以及键盘、行操作和整页视觉核验未闭环，继续未勾选；本轮未改生产代码。

### 2026-11-08 · MDA-11 文件库页头图标与筛选复核

- Morde 文件库独立页头原先用 SF 三横线菜单，和 HTML `.head` 的 19×19 SVG 两横线不同；改成两条 19×2.1pt 圆角线、4pt 行距，保留 38pt 圆形点击区与现有菜单动作，并补齐 `morde.nav.menu` 无障碍标识。文件行图块已是 HTML `.fic` 对应的 32×38pt，本轮未改行尺寸、文件权限或点击行为。
- 内置浏览器沿本机原型验证文件库“全部 → AI 产物 → 我上传的”筛选；每次都只显示对应原型条目，行点击不被误认作真实预览合同。独立 Morde `Morde` scheme、iPhone 18 Pro Max / iOS 27.0 Simulator 上 `testMordeFullLayoutUploadsAndSendsImageAndPDFFixture` 通过 1/1（`/tmp/MDA11-library-two-line-menu-20261108.xcresult`）；改后“我上传的”截图 `/tmp/MDA11-library-two-line-menu-attachments/0DC990BB-38C0-44E4-9114-809DC713443B.png` 已检查，确认双线图标、三段筛选、图片/PDF/metadata 行和四 Tab 正常。
- “全部”里的本机文件来源仍按待审核表保留；权限/证明错误状态和整页逐控件视觉尚未闭环，MDA-11 继续未勾选。本轮只使用本机合成附件，没有读取用户文件、访问真实 Portal 或传输文件。

### 2026-11-08 · MDA-05 事件链当前页头与七行密度复验

- 独立 Morde target 的 `IOS03SimulatorAcceptanceUITests.testMordeEventChainTimelineDensityAndStatusesMatchPrototype` 在 iPhone 18 Pro Max / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA05-event-chain-pro-max-20261108.xcresult`）。这是此前两次 Pro Simulator 在断言前被 signal kill 后的成功复验。
- 截图 `/tmp/MDA05-event-chain-pro-max-attachments/690DCD62-624A-458F-A321-A9F2101987D0.png` 已检查：双线页头、七个时间线节点、状态图标及底部四 Tab 同屏；预算字段未显示。该截图使用本机合成且含英文标题/说明的事件夹具，能确认布局密度和状态，不代表真实服务数据，也不能替代其他页面逐屏对照。

### 2026-11-08 · MDA-03/MDA-06/MDA-08 并行复核与 MDA-15 浏览器逻辑回走

- MDA-03 复核 `MIFTaskViews.swift` 与 HTML `tasks` 的 `.row` 规则：44pt 状态图标、13pt 列间距/上下内边距、文字列 1pt 顶距、标题/摘要字号、字距、行距及颜色均已有对应实现；检查现有五行 Simulator 截图，未发现可安全单独修正的确定视觉偏差。首行摘要和状态行都表达“等待确认”，作为内容重复差异登记；保留真实当前步骤/状态，不以删减真实信息伪造对齐。`xcrun simctl list runtimes` 当前只列 iOS 27.0 与 27.1，没有 iOS 16.0–16.3 runtime；因此旧系统回弹行为仍无法在本机 Simulator 直接验收。MDA-03 仍因其他状态及旧系统 runtime 未验而未完成。
- MDA-06 复核 `MIFApprovalViews.swift` 与 HTML `.head`、`.appr`、`.facts`、`.acts`、`.linkrow`，按钮比例、字色和间距已有对应实现；未发现不影响权限/动作的确定视觉改动。灰度第三动作、自动历史、差异/测试/成本摘要及媒体审批直达预览仍待用户决定，未擅自改动。
- MDA-08 复核 `MIFWorkItemViews.swift` 的弹层：页边距、标题字号/字距、类型按钮、输入框和底部主按钮与 HTML `wiSheet` 对应，系统拖动指示条已隐藏。项目/Task 上下文、描述、阻塞/Workspace/行操作仍待审核，未改功能或入口。
- 内置浏览器再次实际走查本机原型：资源 → PC-01 → 接管控制权（标题、状态及底部操作随状态改变）→ 文件 → 已绑定 Workspace → 返回资源；页面显示设计稿示例的 Workspace、容量与文件条目。此路径只验证 HTML 原型交互，不代表 Morde/Gateway 联调，也未传输或编辑任何真实文件。
- Morde 云电脑截图 `/tmp/MDA15-current-user-brief-attachments/5A5F7CD1-47CA-43E3-BA2B-3A0E822E44E7.png` 已检查：页头、屏幕区域、浏览器/终端/文件入口、操作状态和底部按钮位置与原型主要层级对应，未显示续租、持有倒计时或长说明；现有定向 Simulator 测试记录见 `/tmp/MDA15-current-user-brief-20261108.xcresult`（1/1）。真实 Gateway/远端桌面仍未验收，MDA-15 继续未完成。
- MDA-05 保持已完成状态；没有改动事件链产品逻辑。

### 2026-11-08 · MDA-15 云电脑长按菜单验收

- 按已审核决定核对常驻入口与次级操作：`web/workbench/src/app/Workbench.test.tsx` 中“浏览器/终端/文件为主入口并从长按菜单打开次级工具”和“触摸长按打开文件菜单且不误触 Tab”两项通过 2/2（`pnpm exec vitest run src/app/Workbench.test.tsx -t 'keeps browser, terminal, and files as main entries and opens secondary tools from long-press menus|opens the file menu after a touch long press and suppresses the tab click'`）。测试覆盖 Workspace、主机文件、VS Code 与浏览器/终端操作菜单入口，确认长按不会同时触发常驻 Tab 点击。
- 页面截图与 Morde Simulator 证据仍见上方 MDA-15 记录；本轮只验证菜单逻辑，没有改动产品代码。控制状态读取失败时的“重试状态”按钮仍待用户审核；真实 Gateway/远端桌面及整页状态/视觉验收未完成，MDA-15 不勾选。

### 2026-11-08 · MDA-13 设置详情当前路径复验

- 独立 Morde `Morde` scheme、iPhone 18 Pro Max / iOS 27.0 Simulator 上，`testMordeSettingsRoutesAndHonestUnavailableRows` 通过 1/1（`/tmp/MDA13-settings-current-20261108.xcresult`）。测试核对设置根页行数/页头位置，进入账号详情和设备详情，打开 Hub 远程打开确认后取消，连接器/权限/审批规则/通知/外观占位均可返回，“关于”页可达；没有发送远程打开请求。
- 已查看账号与设备截图：账号页显示真实身份/服务地址、Host members、Model connections、API Keys、安装管理和退出；设备页还含 Hub Relay URL、设备列表与远程打开确认。它们均超出 HTML 设置行点击提示的行为，入口/详情去留仍待审核，保留当前实现；具体功能损失与精简建议见待审核表。MDA-13 的设备刷新/错误/撤销、安全凭证详情和逐页视觉仍未闭环，不勾选。

### 2026-11-08 · MDA-02 PDF 解析范围复核

- 源码确认 PDF 文字提取上限为前 100 页/48 KiB，预览图上限为前 10 页；随附件发送的解析文本会附带“其余文字/页面未读取”说明，模型可获知不完整范围。聊天草稿附件行和用户消息卡只呈现原 PDF 名称，用户看不到该说明；派生文本附件在界面中隐藏。
- 仅作源码检查，未改附件上限或界面。建议在达到上限时于现有 PDF 附件行显示一条简短、不可交互的范围说明，不增加按钮；旧 `MinisTests` 中的扫描 PDF 测试不作为 Morde 独立 target 的验收证据。是否增加这行说明待用户决定，MDA-02 继续未完成。

### 2026-11-08 · MDA-04 Retry 决定复核与 Morde UI 测试目标恢复

- 用户确认移除 Task 详情请求失败、事件流失败、事件历史未确认时的三个页面级“重试加载”按钮；返回任务列表后重进重新读取，事件流保留自动重连。当前实现已符合决定，失败批次业务“重试”继续保留。
- 集成检查发现独立 `Morde.xcodeproj` 仍引用 `MordeUITests/IOS03SimulatorAcceptanceUITests.swift`，但该跟踪文件在本地工作区被删除，导致测试目标无法构建。已从本地 Git 版本恢复测试源文件；没有改项目合同或产品逻辑。`build-for-testing` 在 iPhone 18 Pro Max / iOS 27.0 Simulator 成功。
- 当前代码下 `testMordeTaskDetailShowsPermissionErrorWithoutTaskActions`、`testMordeTaskDetailTimelineFailureHasNoRetryButtonAndRecoversAutomatically`、`testMordeTaskDetailUnconfirmedHistoryHasNoRetryButton` 通过 3/3（`/tmp/MDA04-task-retry-reconfirm-20261108.xcresult`）：无页面级 Retry、详情返回重进恢复、事件流自动恢复均通过。MDA-04 仍因活动记录/交付物区待审核及完整逐页视觉尚未完成而保持未勾选。

### 2026-11-08 · MDA-01 导航、MDA-14 分享副本与 MDA-15 云电脑复验

- 删除 `MIFDrawerDestination` 到 `MordeDesignRoute.projects` 的过期映射，保留内部项目路由和数据。独立 `Morde` / `MordeUITests` 使用全新 DerivedData、ARM64 Simulator 构建后，在最终源码下 `testMordeDesignNavigationRoutes`、`testMordeWorkspaceEditShareDeepLinkClaimsEditableIndependentCopy`、`testMordeComputerIOSWorkbenchKeepsOnlyDesignTabsAndConciseStatus` 各通过 1/1（`/tmp/MDA01-navigation-final-20261108.xcresult`、`/tmp/MDA14-edit-share-final-20261108.xcresult`、`/tmp/MDA15-workbench-final-20261108.xcresult`）。导航测试核对四个底部 Tab、隐藏项目/通知入口和抽屉路由；截图 `/tmp/MDA01-navigation-final-attachments-20261108/B4BB9AC7-AFE4-42FF-B804-677D97E77FA7.png` 已检查。可编辑分享用例确认领取后生成独立 Workspace 副本，副本文件可编辑；云电脑用例确认仅保留浏览器/终端/文件入口，没有 Workspace、VS Code、续租、倒计时或冗长说明。对应 MDA-15 视觉截图参照上方已检查的 `/tmp/MDA15-current-user-brief-attachments/5A5F7CD1-47CA-43E3-BA2B-3A0E822E44E7.png`。
- Task 详情三个页面级“重试加载”按用户审核移除，Morde Simulator 恢复用例已通过 3/3（`/tmp/MDA04-task-retry-reconfirm-20261108.xcresult`）：返回列表后重进恢复详情/历史，事件流自动重连，失败批次业务重试保留。
- 此前本机登录 fixture 的 `NSURLErrorDomain -3003` 在清空专用 Simulator 后未复现；没有修改登录或网络代码。构建仍输出 Swift 并发隔离和测试部署版本警告。MDA-14 尚需完成分享失败/过期状态、manifest 差异与整页视觉核对；MDA-15 尚需真实 Gateway/远程桌面和整页视觉验收。本轮仅使用本机代码与合成数据，没有发送或外发文件。

### 2026-11-08 · MDA-06/07/08 局部对齐与 Simulator 复验

- 审批卡几何复核未发现已审核范围内可安全调整的差异。修复了批准请求提交后在 Morde 抽屉路由中隐藏“看事件链”的条件；该入口已存在于设计稿，恢复可见性没有增加新控件。`testMordeApprovalInboxShowsOnlyConfirmedDecisions` 在 `/tmp/MDA06-07-08-geometry-integrated-20261108.xcresult` 通过。
- 资源页浅色画布从 NanoMuse 的 `#F3F3F5` 调整为 HTML `.screen` 的 `#FAFAFA`，深色模式与 Host/Workspace 权限、状态及数据保持不变。工作项列表两种空态移除设计稿没有的图标卡，改为 14.5pt 次级文字行；不改变筛选、行操作或新建字段。当前定向 UI 用例覆盖正常列表和创建弹层，空态专用 Simulator 断言尚未增加。
- 同一集成结果中的 `testMordeWorkItemsRouteShowsDesignFiltersAndCreateForm` 通过；资源用例最初只因测试仍使用旧的“docs”/“Ready Workspace”无障碍名称失败，未发现产品导航故障。测试改用现有 `morde.workspace.breadcrumb.parent` 标识后，`testMordeResourceWorkspaceRouteBrowsesSyntheticManifest` 在 `/tmp/MDA07-resource-workspace-recheck-20261108.xcresult` 通过 1/1，覆盖合成 Workspace 清单、目录/子目录、面包屑返回及返回资源页。复验截图 `/tmp/MDA07-resource-workspace-recheck-attachments/9018A7FE-68EC-473E-9579-1065F3C24205.png` 已查看。
- 内置浏览器使用本机 HTML 预览完成主导航、任务列表/详情、审批事件链、聊天 PDF 预览、资源/Workspace、云电脑与工作项筛选走查。工作项“待认领”筛选正常更新样例行；云电脑原型显示浏览器/终端/文件三个入口，且不含续租、持有倒计时或长说明，但点击终端/文件后展示仍停留在浏览器；这记为 HTML 原型交互限制，不扩写成应用功能合同。没有提交审批、认领工作项、分享 Workspace 或传输文件。
- 上述修改和测试只证明局部控件及合成路径；MDA-06 的额外动作/历史和媒体直达预览、MDA-07 Host 额外文字/资源整页视觉、MDA-08 待审核字段/行操作与空态专用验证仍未完成，三页均继续未勾选。

### 2026-11-08 · MDA-15 云电脑控制错误状态收敛

- 按用户已确认的“页面不增加设计稿外按钮”规则，移除控制状态接口失败时放在底栏主操作位的“重试状态”按钮；保留标题错误状态和 Host 控制安全校验，不影响健康状态下的“接管控制权”/“交还给 AI”。回到资源页重进会复用既有 Host 控制状态刷新。
- 新增的本地合成用例 `testMordeComputerControlStatusErrorHasNoRetryButtonAndRecoversOnReentry` 在独立 Morde target、iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA15-control-reentry-evidence-20261108.xcresult`）。首个控制状态请求返回合成 503 时不显示额外按钮；返回资源页再进入后重新读取，恢复“Codex 正在控制”及“接管控制权”。错误截图 `/tmp/MDA15-control-reentry-attachments-20261108/D73C5B7E-7C7A-4BFF-AB38-3C76DC7B39FC.png`、重进恢复截图 `/tmp/MDA15-control-reentry-attachments-20261108/62FCF68B-E1BE-4B5E-86F4-FA182026B441.png` 已查看。仅使用本地 HTTP 替身，未连接真实 Gateway/Host。
- MDA-15 仍因真实 Gateway/WebView 操作能力和完整 HTML/Simulator 逐控件视觉对照未完成而保持未勾选。构建仅见原有 iOS 16 测试部署版本链接警告。

### 2026-11-09 · MDA-04 Retry 决定再次确认与 MDA-11 徽标字距复验

- 用户再次确认移除 Task 详情页详情加载失败、事件流失败及未确认历史的 3 个“重试加载”按钮；从任务列表返回并重新进入后恢复读取，事件流继续自动重连。当前 `MIFTaskInlineError` 仅显示状态说明，三条 Morde UI 用例已在 `/tmp/MDA04-task-retry-reconfirm-20261108.xcresult` 通过 3/3；失败批次的业务“重试”仍保留。
- MDA-11 对照 HTML `.fic` 的 `font-size:8.5px; letter-spacing:.02em`，将 `MIFLibraryViews.fileTypeTile` 的徽标字距从 `.tracking(0.02)` 改为 `.tracking(0.17)`；未改变徽标尺寸、颜色、行内容、筛选或点击行为。独立 `Morde.xcodeproj` / `Morde` scheme、iPhone 18 Pro / iOS 27.0 Simulator 的 `testMordeFullLayoutUploadsAndSendsImageAndPDFFixture` 最终通过 1/1（`/tmp/MDA11-filetype-tracking-final-20261109.xcresult`）。结果覆盖合成图片/PDF发送、AI 产物/我上传的筛选、个人附件预览、返回保留筛选以及预览中切回聊天；截图 `/tmp/MDA11-filetype-tracking-final-attachments-20261109/DB770425-2592-4D39-B444-ECCBA09E646A.png` 已检查。内置浏览器对照 HTML `lib` 与该 Simulator 截图，三段筛选、行结构和主导航位置对应；原型有 3 个上传样例，Morde 合成数据另有本机文件及仅元数据行，其可见性仍待审核。首轮测试曾复用本机旧聊天数据，后改为按唯一合成账号隔离；另修正无关联 Task 的个人聊天预期和预览页重复 Tab 无障碍标识定位，产品路径和控件未变。
- 文件库测试截图仍显示多个本机/同步文件样例行，其中“全部”筛选中的本机 App 文件入口已列待审核表，本轮未擅自移除或隐藏。MDA-11 仍待该决定及权限/错误状态、完整逐控件截图对照；MDA-12 的预览页已经单独完成。

### 2026-11-09 · MDA-07/08 资源页与工作项页几何复核

- MDA-07 对照本地 HTML `res` 首轮按源码推算为 22pt 尾部 padding；后续 Simulator 直接测量发现滚动内容绘制在浮动 TabBar 后方，该推算不成立，末行实际被胶囊遮挡 43pt。最终改为 Morde 路径 95pt（空态行自带 13pt 底部留白），末行到胶囊顶部实测 30pt；NanoMuse 路径仍为 22pt。首屏/Workspace 截图、字段、权限和操作未变。
- 最新源码下 `testMordeResourceWorkspaceRouteBrowsesSyntheticManifest` 在 iPhone 18 Pro Max / iOS 27.0 Morde Simulator 独立通过 1/1（`/tmp/MDA07-resource-final-20261109.xcresult`）。首屏与 Workspace 状态行截图为 `/tmp/MDA07-resource-final-attachments-20261109/0899558B-4D32-4EDD-9C8A-EEE40EBFDF6B.png`、`/tmp/MDA07-resource-final-attachments-20261109/71FB4E27-78D9-49F3-81A5-C5FF17E60BEB.png`。同一用例的合批运行曾在后续 Workspace 系统文件选择器“完成”按钮处失败；资源页断言已通过，独立复跑后全项通过，记为未复现的系统面板 XCTest 波动。
- MDA-08 上述首轮复核只覆盖首屏和创建表单；后续长列表直接测量发现旧 14pt 尾部 padding 会让末行被浮动胶囊遮挡 51pt。最终改为 Morde 路径 95pt、NanoMuse 路径仍为 14pt，并增加可滚动长列表夹具验证目标 30pt 间距。最终夹具恢复为已分配的 `open` 行，`testMordeWorkItemsLongListEndsAboveFloatingTabBar` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA08-tail-gap-final-state-20261109.xcresult`），末行可见并直接量得 30pt；截图 `/tmp/MDA08-tail-gap-final-state-attachments-20261109/F6A67184-3664-4D86-BD58-BA9116CB2446.png` 已检查。调试期间的临时 `delivered` 夹具缺少交付记录，服务端形状校验正确拒绝，已恢复合法数据，不作为当前结果。项目/Task 上下文、描述和额外行操作仍待用户审核，MDA-08 保持未勾选。

### 2026-11-09 · MDA-02 活动 Task 附件回执与 MDA-04 Retry 决定确认

- MDA-02：排查发现消息 POST 回执不带附件 ID，而个人附件在消息受理后才逐项上传并关联；原流程在关联完成时清理 outbox，却继续显示最初的无附件回执，造成已发送的活动 Task 消息气泡丢失图片/PDF chip。现在在移除 outbox 前调用现有 `GET /api/conversations/{id}/messages?client_message_id=...`，校验消息身份及服务端已关联附件 ID 集合，再用此回执更新当前会话；不增加公开路由或改变账号/Task 权限。若回执/附件集合无法确认，保留 outbox 供恢复。
- `testMordeChatComposerQueuesImageAndPDFAttachmentsOnActiveTask` 在独立 Morde scheme、iPhone 18 Pro Max / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA02-attachment-receipt-refresh-rerun-20261109.xcresult`）。验收按任意可访问元素查找两个附件名，确认回复消息送达后图片与 PDF chip 仍显示；截图 `/tmp/MDA02-attachment-receipt-refresh-rerun-attachments-20261109/23FEBA90-04FB-4CDB-AAA4-D095C96347B8.png` 已检查。菜单点击 helper 现在等待长按菜单项可点击并确认 Task options sheet 已打开。首轮测试停留在菜单点击时序问题，未到附件提交断言，最终重跑通过。构建仍有既有 Swift 并发隔离、iOS 16/XCTest 链接版本和 `_UIGravityWellEffectAnchorView` runtime 警告。
- MDA-03：此前按 safe-area 推算的 22pt 尾部 padding 已由直接测量取代：长列表停在末行时发现旧值被浮动 TabBar 遮住；最终 Morde 路径改为 108pt，NanoMuse chrome 路径保持原值。加入 20 行合成列表后，`testMordeTaskListRendersKnownAndUnknownStatusesWithoutActions` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA03-tail-gap-final-20261109-r3.xcresult`），末行到 TabBar 胶囊顶部直接测得 30pt，截图 `/tmp/MDA03-tail-gap-final-attachments-r3-20261109/EA252BE3-5D00-43A7-9818-1E35DCFB16FB.png` 已检查。此前状态矩阵和步骤计数用例 2/2、五行视觉对照证据仍有效；MDA-03 页面级验收继续未完成。
- MDA-07：采用直接 Simulator 几何测量后，旧 22pt padding 的末行遮挡胶囊 43pt；改为 Morde 路径 95pt（含空态行自带 13pt 尾距后，末行到胶囊顶部为 30pt），NanoMuse 路径保持 22pt。`testMordeResourceWorkspaceRouteBrowsesSyntheticManifest` 通过 1/1（`/tmp/MDA07-tail-gap-final-20261109-r2.xcresult`），已检查底部截图 `/tmp/MDA07-tail-gap-final-attachments-20261109/7A411B8B-A8F2-4532-BB83-1DB2735BB684.png`；资源/Workspace 其余状态及全页视觉仍未闭环。
- MDA-04：用户再次确认移除详情请求失败、事件流失败、未确认事件历史三处页面级“重试加载”；返回任务列表后重进重新读取，事件流自动重连，失败批次业务重试保留。现有 Morde UI 恢复用例此前已通过 3/3（`/tmp/MDA04-task-retry-reconfirm-20261108.xcresult`），本轮未重复改页面行为，MDA-04 仍因活动/交付物待审核差异及完整逐页视觉未闭环而未勾选。
- MDA-11：只读逐项复核文件库 HTML 与源码，页头、筛选、滚动尾距、行图块/字距及多行文字已对应，未发现可安全直接调整的纯视觉差异。Morde 显示本机 App 文件与仅元数据行，HTML 示例没有对应来源/状态；隐藏或统一点击行为会影响打开本机文件或真实权限/证明校验，继续留在待用户审核表，未更改。
- 本轮只改本机 Morde/iOS 源码、既有 UI 测试和本地 TODO；使用合成附件与 Simulator 替身，不访问真实 Portal/Host 或发送文件。

### 2026-11-09 · MDA-13 设置画布对齐与 MDA-04 Retry 答复登记

- MDA-13：复核本机 HTML `.screen` 的浅色背景为 `#FAFAFA`；设置根页和详情页此前沿用 nanoMuse `#F3F3F5` 画布。新增 Morde 设置页专属动态画布，浅色改为 `#FAFAFA`、深色仍为黑色，并应用于根页和详情页。共用 TopBar、按钮阴影/描边、导航材质、行内容、入口、路由和行为未变。
- `testMordeDesignNavigationRoutes` 在 iPhone 18 Pro / iOS 27.0 的 Morde Simulator 通过 1/1（`/tmp/MDA13-settings-canvas-align-20261109.xcresult`）；最新根页截图 `/tmp/MDA13-settings-canvas-align-attachments-20261109/2DD5E329-F413-4132-8674-2D3803C401CA.png` 已检查，十行顺序、分组、页头和四 Tab 均显示，画布与 HTML 色值一致。构建仍只有既有 Swift 并发隔离、未使用值及 XCTest 链接版本警告。
- MDA-04：用户确认移除详情请求失败、事件流失败及未确认历史三处页面级“重试加载”，通过返回任务列表后重新进入触发重读；事件流自动重连和失败批次业务重试保留。现有代码符合决定，不需新增修改；三状态 UI 证据见 `/tmp/MDA04-retry-confirm-current-20261108.xcresult`（3/3）。
- MDA-13 继续未勾选：设置详情去留/急停恢复等仍待审核，设备与凭证状态、详情页视觉及其他页面全量逐控件验收尚未闭环。仅修改本机源码和实施文档，没有发送或外发文件。

### 2026-11-09 · MDA-06 审批画布、MDA-03/07/08/14/15 逐页复核

- MDA-06：对照 HTML `.screen` 浅色背景，将 Morde 审批页背景限定调整为 `#FAFAFA`；仅影响 Morde 审批页浅色模式，深色模式及 NanoMuse chrome 保持既有表现。`testMordeApprovalInboxShowsOnlyConfirmedDecisions` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA06-approval-canvas-align-20261109.xcresult`），截图 `/tmp/MDA06-approval-canvas-align-attachments-20261109/D2FCFD2F-6CFD-434E-9472-77863E7E381C.png` 已检查。当前只验证浅色待办页，不代表审批其他状态或全页视觉关闭。
- MDA-03/07/08 长列表尾距：本轮已用 Simulator 直接测量替代 `.safeAreaInset` 源码推算。最终 Morde 任务/资源/工作项列表尾部 padding 分别为 108pt / 95pt / 95pt，NanoMuse 分支未改；任务、资源、工作项长列表用例均通过 1/1，末行到浮动 TabBar 胶囊目标为 30pt。工作项最新结果 `/tmp/MDA08-tail-gap-final-state-20261109.xcresult`（截图已检查）；调试中的两次临时夹具探索失败不属于最终源码/夹具结果。页面级按钮、状态、权限和全页视觉仍未闭环。
- MDA-07/08：独立复核 Host/Workspace 与工作项的页面几何，已审范围内未发现新的安全、纯视觉偏差；Host 额外状态信息和工作项上下文/描述/行操作仍按待审核表保留，未擅自删除或新增控件。
- MDA-14：当前分享链接/领取异常、旧链接状态兼容和 Workspace manifest 元数据差异仍在待审核/待验收表中；本轮不改 API、数据或入口。
- MDA-15：用户要求的移动端精简样式已保持：不显示续租、持有/到期倒计时和冗长说明；常驻只显示浏览器、终端、文件，Workspace/主机文件/VS Code 与窗口操作继续由已审核的长按菜单承载。新增 `attachScreen` 仅用于保留加载完成的本地 Workbench 截图；`testMordeComputerIOSWorkbenchKeepsOnlyDesignTabsAndConciseStatus` 在 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA15-loaded-shell-final-20261109.xcresult`），截图 `/tmp/MDA15-loaded-shell-final-attachments-20261109/97A7E565-9373-4919-AC98-009E7EB09DAE.png` 已检查，主框架、三入口、浏览器区域及底栏位置与 HTML 对应，且没有续租、倒计时和长说明。该截图使用本地合成 Workbench；没有代表真实 Gateway 内容。
- `testMordeComputerRouteAcceptsSyntheticTicketAndReturnsToResources` 在同一 Simulator 通过 1/1（`/tmp/MDA15-accept-ticket-return-20261109.xcresult`），验证票据页进入、关闭返回资源页及共享 Host 返回路径。该用例截图停留在远端页面“连接中”，只作为导航/票据路径证据，不用于视觉对齐结论。
- 内置浏览器再次实际操作本地设计稿原型的聊天发送、任务详情急停、审批事件链、工作项筛选/创建、资源与 Workspace、云电脑接管/终端/文件、文件库筛选及设置入口；原型以本地 JS 状态响应，不触发真实 API、Host 控制、分享或文件传输。Morde 本地合成 Host/WebView 的 Simulator 证据与原型走查分别记录，不能互相替代真实远端验收。
- Task 详情三个“重试加载”按钮按用户最新答复移除；返回任务列表再进入恢复读取，事件流自动重连，业务批次“重试”保留，现有三状态用例通过 3/3。活动记录/交付物两块是否保留仍待用户答复。MDA-03/04/06/07/08/14/15 均继续保持页面级未完成；本轮修改和验收均在本地完成，没有外发文件。

### 2026-11-09 · MDA-04 重试项确认与 MDA-15 截图预览

- MDA-04：用户此轮再次确认移除 Task 详情的详情读取、事件流和未确认历史三处页面级“重试加载”；返回任务列表后重进恢复读取，事件流自动重连，业务批次重试保留。现有实现与 3/3 恢复用例符合，无需改动页面逻辑。
- MDA-15：截图按钮在加载完本地合成 Workbench 后能够生成快照并打开预览弹层。`testMordeComputerCapturePreviewOpensAfterWorkbenchLoads` 在隔离的 iPhone 18 Pro / iOS 27.0 Simulator 通过 1/1（`/tmp/MDA15-screenshot-preview-sheet-final-20261109.xcresult`）。仅为弹层增加无障碍容器标识，未增加可见控件。该轮 Simulator 导出的整屏附件呈黑屏，未用作截图画质或视觉对齐证据；真实 Gateway/Host 与远端画面仍未验收。
- 以上均为本地源码、测试及文档更新；没有上传或发送文件。

### 2026-11-09 · MDA-02 输入栏审核边界与 MDA-15 设计样例复核

- MDA-02：按已审核的“语音移到长按/菜单入口”，Morde 聊天输入栏不再显示常驻麦克风；空输入态保留原发送位黑色箭头并聚焦文本框，语音功能仍可从长按菜单打开。干净 DerivedData 首轮复验发现空态聚焦回调也必须保留，随后恢复 `onIdleSend` 并复验。`testMordeEmptyComposerSendFocusesInput` 与 `testMordeChatLongPressActionsOpenTaskOptionsAndImporter` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 最终通过 2/2（`/tmp/MDA02-composer-menu-final-r2-20261109.xcresult`）；截图 `/tmp/MDA02-composer-menu-final-r2-attachments-20261109/42E9676D-5AFA-4B3A-95A4-D24EBC4DAC44.png` 已检查。未新增按钮；MDA-02 仍因其他待审核差异、附件状态与全页视觉未闭环而未勾选。
- MDA-15：为避免合成截图使用非设计稿名称，将 Workbench 视觉 fixture 固定为本机 HTML `pc` 样例的 `PC-01 · morde 仓库`；`testMordeComputerIOSWorkbenchKeepsOnlyDesignTabsAndConciseStatus` 通过 1/1（`/tmp/MDA15-workbench-design-names-20261109.xcresult`），截图 `/tmp/MDA15-workbench-design-names-attachments-20261109/6A9FD883-9FB2-48F4-AD5B-40A1F80DDE3F.png` 已检查。截图显示紧凑页头、浏览器/终端/文件三入口、设计稿样例标题和屏幕区域；无续租按钮、持有时间倒计时或冗长说明。仅为本机合成 Host/Workbench 的壳层视觉证据，真实 Gateway/远端桌面内容仍未验收，MDA-15 保持未勾选。
- MDA-04：登记用户本轮再次确认移除详情读取失败、事件流失败、历史未确认三处页面级“重试加载”；通过返回任务列表后重进恢复读取，事件流自动重连，失败批次的业务重试保留。现有 3/3 状态恢复证据仍有效，MDA-04 因活动/交付物待审核及整页验收未闭环保持未勾选。
- 本轮只修改本机源码、测试与本地实施清单；未发送或外发文件。

### 2026-11-09 · MDA-03/06/07 逐页复核与内置浏览器逻辑走查

- MDA-03：当前 `MIFTaskViews.swift` 的任务列表结构、水平/顶部/底部边距、44pt 状态图标、标题/摘要/状态色及 30pt 浮动 TabBar 尾距均有 HTML/Screenshot 对照证据。没有发现不影响真实 Task 信息的确定性视觉修复；部分状态中摘要和状态文案意思接近，但摘要来自真实 `current_step/result`，不能为压缩字数而隐藏。iOS 16.0–16.3 Simulator 不可用，完整多状态/逐控件视觉仍未验；不勾选。
- MDA-06：审批页首屏几何、卡片/事实行、批准/拒绝按钮比例与颜色、事件链入口、浅色画布已有 HTML/Simulator 对照；无确定的纯视觉修正。灰度动作、差异/测试/成本信息、自动处理历史及媒体审批直达预览仍缺用户决定或真实 API 合同；不新增/删减或伪造，整页保持未完成。
- MDA-07：资源页主要标题、Host/Workspace 行结构、图标、容量/挂载摘要和行距已有 HTML 对照；无不改变行为的确定性修正。Host 控制/权限状态及错误说明仍是待审核差异，不能单方面隐藏；Workspace 真实容量/只读/挂载字段继续保留。整页仍待审核和全状态视觉核验。
- 内置浏览器实际打开本地原型（`127.0.0.1:8765`），依次操作聊天发送“本地原型走查”并看到回显及新建 Task、任务列表/详情、审批列表、云电脑接管、终端命令模拟、文件入口、Workspace 子目录/面包屑返回和分享权限/提示。原型反馈证明设计稿交互可导航；它不调用 Morde API，也不证明 Morde App 的服务端/Host 功能。本机 Morde App 证据仍以各项 Simulator 测试为准。
- 本轮三个页面审计均未改代码；浏览器走查只改动本地原型内存态，没有上传、发送或外发文件。MDA-03/06/07 保持 `[ ]`，等待余下审核项和页面级验收。

### 2026-11-09 · MDA-11 预览权限失败提示与 MDA-02 输入栏回归

- MDA-11：第一轮文件库状态矩阵 3/4 通过（文件筛选/图片 PDF、metadata-only 不可预览、个人同步不可用均通过）；403 权限拒绝路径失败。原因是错误提示与全屏预览关闭在同一状态更新中提交，提示没有出现。现将真实附件拒绝/不匹配错误延后到预览 `onDismiss` 再呈现，仍保持 fail-closed，不加载或留下空白预览。403、404 与 503 下载失败三项最终通过。结合前一批用例覆盖文件筛选/预览、metadata-only 行、个人同步失败及三种下载拒绝，MDA-11 功能矩阵已验证；结果 `/tmp/MDA11-preview-alert-fix-20261109.xcresult` 通过 5/5（含 MDA-02 两项输入栏回归），首轮未修复结果 `/tmp/MDA11-library-state-matrix-20261109.xcresult` 为 3/4。
- MDA-02：当前源码明确关闭聊天输入栏常驻麦克风，并恢复 `onIdleSend` 聚焦回调；空输入显示黑色发送箭头，语音操作保留在长按菜单。`testMordeEmptyComposerSendFocusesInput` 与 `testMordeChatLongPressActionsOpenTaskOptionsAndImporter` 随上方最终矩阵通过 2/2；没有改变共享 NanoMuse composer 默认或新增可见按钮。MDA-02 仍有 Task 摘要卡等审核差异与整页视觉/其他附件状态未闭环，不勾选。
- MDA-11 页面仍因“全部”中的本机文件入口待审核及整页多状态/逐控件视觉验收未完成而保持 `[ ]`；局部权限错误测试通过不等于全页验收完成。
- 仅修改本机 Morde 源码与实施清单；未上传、发送或外发任何文件。

### 2026-11-09 · MDA-04 重试入口最终确认与 MDA-14 分享对齐复验

- MDA-04：用户再次确认移除 Task 详情请求失败、事件流失败、历史未确认时出现的 3 个页面级“重试加载”按钮。当前实现符合：返回任务列表后重进触发重新加载，事件流自动重连，失败步骤的业务“重试”继续保留。独立 `Morde` scheme / iPhone 18 Pro / iOS 27.0 三个状态用例最终通过 3/3（`/tmp/MDA04-no-retry-final-20261109.xcresult`）；不因这项决定把 MDA-04 整页标为完成。
- MDA-14：合批首次 4/5 通过，唯一失败暴露出分享弹层标题与 HTML 不一致：设计稿会随选中的 Workspace 显示 `分享「名称」`，iOS 原先固定显示 `分享 Workspace`。已改为使用当前 Workspace 名称生成标题。创建分享链接及权限选择单测通过 1/1（`/tmp/MDA14-share-title-fix-r2-20261109.xcresult`）；五项分享、副本领取、文件行菜单和 manifest 路径安全用例最终合并通过 5/5（`/tmp/MDA14-share-workspace-flow-final-20261109.xcresult`）。
- MDA-14 的这些测试使用本机合成 API 与合成账号，证明客户端流程和当前模拟合同，不代表真实服务端部署及真实用户链接生命周期已端到端验收。分享撤回/过期及兼容边界、Workspace manifest 与设计稿样例数据差异、整页视觉核对仍未闭环，MDA-14 保持 `[ ]`。
- 本轮 `git diff --check` 通过；仅本机修改 Morde 源码和实施清单，没有发送或外发文件。

### 2026-11-09 · MDA-08 工作项空态与设计稿文案

- 按本地 HTML `work` 页的渲染脚本，项目无工作项和筛选无匹配项均显示“这一类暂时没有”。Morde 原先的两种简体中文空态分别是“此项目目前没有工作项。”与“没有符合此筛选条件的工作项。”；已将这两个现有语义键的简/繁体中文文案统一成设计稿对应表达，没有增加入口、按钮或改变筛选行为。
- 新增本机 `-morde.localWorkItemEmptyStateFixture`，只让 Morde UI 测试的合成项目返回空列表。项目空态 `testMordeWorkItemsProjectEmptyStateShowsDesignCopy` 通过 1/1（`/tmp/MDA08-project-empty-final-20261109.xcresult`），筛选空态 `testMordeWorkItemsFilterEmptyStateShowsDesignCopy` 独立通过 1/1（`/tmp/MDA08-filter-empty-final-20261109.xcresult`）。截图分别位于 `/tmp/MDA08-project-empty-final-attachments-20261109/13727EA7-8339-451E-830E-C5860E4CA14F.png`、`/tmp/MDA08-filter-empty-final-attachments-20261109/4ED5D611-1A46-4E31-B878-E0FD9BFAF405.png`，已检查；空态文案、筛选选中态和底部导航可见。
- 首轮合批 0/2：项目空态测试错误地期待英文文本，实际 Simulator 按 `zh-Hans` 显示中文；筛选用例在同批运行时 test runner 被终止。修正本地化断言后分别隔离运行，两项最终均通过。合批首轮不作为产品功能通过证据。
- 项目选择器、Task 上下文、描述和关联/交付/阻塞操作仍在待审核表中；两种空态通过不代表 MDA-08 整页关闭，MDA-08 继续 `[ ]`。
- 修改仅限本机 Morde 源码、中文字符串目录、UI 测试和 TODO；没有访问真实 Portal/用户数据或外发文件。

### 2026-11-09 · 工作项新增原型走查与 Task 重试决定复核

- MDA-08：在本机 HTML 设计稿的工作项页验证“缺陷”“待认领”筛选、新建弹层和提交路径；提交后原型列表出现“浏览器原型检查工作项”，并显示“已创建 · 团队可见”。这是设计稿 JavaScript 的本地模拟，不代表 iOS API 或服务端创建验收。空态两项 Morde Simulator 独立用例仍分别通过 1/1，MDA-08 因上下文/描述/额外操作待审核及整页视觉未闭环保持 `[ ]`。
- MDA-04：用户再次确认移除详情加载失败、事件流失败和事件历史未确认时的 3 个页面级“重试加载”按钮；返回列表后重进重新加载，事件流自动重连，失败步骤的业务“重试”保留。独立 Morde Simulator 三项状态用例通过 3/3（`/tmp/MDA04-no-retry-final-20261109.xcresult`）。
- 本轮源码仓库 `git diff --check` 通过，`Localizable.xcstrings` JSON 解析通过。所有修改与原型操作均在本机完成，没有上传、发送或外发文件。

### 2026-11-09 · MDA-14 分享错误状态与 MDA-13 原型入口复核

- MDA-14：新增本机 URLProtocol 合成错误态，覆盖 Workspace 分享预览与领取阶段的 404 和网络失败。`testMordeWorkspaceSharePreviewNotFoundShowsInvalidLinkError`、`testMordeWorkspaceShareAcceptNotFoundDoesNotReportSuccess`、`testMordeWorkspaceSharePreviewNetworkFailureShowsError`、`testMordeWorkspaceShareAcceptNetworkFailureShowsErrorWithoutSuccess` 在独立 Morde scheme / iPhone 18 Pro / iOS 27.0 通过 4/4（`/tmp/MDA14-share-error-states-20261109.xcresult`）；截图 `/tmp/MDA14-share-error-states-attachments-20261109/` 已检查。错误态不显示领取成功或继续领取动作；未改变 API 合同、产品 UI 或待审核的“重新加载”按钮。分享的成功、错误、过期/撤销生命周期及整页视觉仍有未闭环项，MDA-14 继续 `[ ]`。
- MDA-14 分享领取合同复核：服务端允许处于 `claiming` 的同一账号恢复未完成领取；一旦转为 `claimed`，token 对所有人（包括接收者本人）均失效。领取成功但响应丢失时，接收者应从自己的 Workspace 列表继续使用已创建的独立副本；不把已完成链接改成可重复领取，以遵守用户审核的“单人领取、领取后链接失效”。`go test ./internal/app -run 'TestWorkspaceShare' -count=1` 通过；没有改动 API、状态机或客户端行为。
- MDA-04 与 MDA-08 既有结果包已重新读取摘要核实：Task 详情三种无页面级 Retry 状态通过 3/3；工作项项目空态和筛选空态分别通过 1/1。没有因单项通过勾选整页任务。
- MDA-13：设置文案目录补译的 51 个 key 覆盖 9 种语言；根页/详情页标题、分组、目的地标题、条件急停行、Portal 兜底信息及设备 Hub 状态/错误均接入 `AppLocalized`，遵循应用内语言选择的 bundle；Portal 静态标题和设备行的嵌入标签也能随所选语言显示。`Localizable.xcstrings` JSON 通过解析。独立 `Morde.xcodeproj` / `Morde` scheme 的 `testMordeDesignNavigationRoutes` 在 iPhone 18 Pro / iOS 27.0 通过 1/1（`/tmp/MDA13-settings-localization-wiring-20261109.xcresult`）；根页截图 `/tmp/MDA13-settings-localization-wiring-attachments-20261109/7E8D721F-891A-4D2D-B14B-3B68B92C932E.png` 已检查。账号/设备入口去留及占位行点击行为仍待用户审核，设备/凭证真实状态和整页视觉未闭环，MDA-13 不勾选。
- 另行结束的旧 `Minis.xcodeproj` `morde` scheme 诊断回归为 22 通过、5 跳过、2 失败（`/tmp/MordeNonAIRegression-20261109.xcresult`）：一项被 XCTest 取消，另一项旧断言要求空输入时不存在发送动作；该结果属于旧 target，不作为独立 `Morde.xcodeproj` 的页面验收证据，也未据此改写已审核的聊天输入行为。
- MDA-03：重新导出并检查当前 5 行样例的 Morde Simulator 截图 `/tmp/MDA03-row-spacing-attachments-20261109/74D85B20-8836-4FFC-BD6D-66C9181BF8F9.png`，与重置为原始 mock 数据后的 HTML `tasks` 首屏对照。标题/加号、五行顺序与密度、状态图标和四项 Tab 位置相符；这轮没有发现可安全直接调整的新增视觉差异。设计稿的 HTML 行为也仍只在本机原型内，MDA-03 继续 `[ ]`，等待 iOS 16.0～16.3 runtime 与完整多状态视觉验收。
- 内置浏览器在仅绑定 `127.0.0.1` 的本地 HTML 原型上复核设置页：侧边栏进入设置，点击“设备”和“安全凭证”分别留在设置页并显示原型 toast；点返回回到聊天。原型账号的套餐/使用率及审批规则内容属于演示值，未作为 Morde 真值。浏览器操作没有访问 Portal 或发送数据。
- 只读检查独立 Morde target：入口为 `MordeStandaloneApp` → `MordeEmbeddedView` → `MordeRoot`；Morde 本地化目录和用户可见文案中未发现“NanoMuse”品牌字符串，保留的 `NanoMuse*` 名称位于共享视觉/数据类型，不能据名称判定存在第二套活动导航逻辑。
- 本轮新增 MDA-14 仅为 DEBUG URLProtocol fixture 和 Morde UI 验收；`git diff --check` 通过。所有操作留在本机，未发送或外发文件。

### 2026-11-09 · MDA-03/06/08 只读复核与工作项创建文案对齐

- MDA-03：逐项复核任务列表 HTML 与 Morde 路径。行距、页头位置、按钮尺寸、图标和列表尾距已有对应规则及证据；页头菜单按钮轻阴影与 HTML `.circbtn` 无阴影不同，按“视觉沿用 nanoMuse、组件位置按设计稿”的既有原则保留，不影响控件数量/位置。浅色 grouped 背景按 MDA-01 既有决定保留。其余缺口为 iOS 16.0～16.3 实际 runtime 及完整多状态/多尺寸截图对照，MDA-03 不勾选。
- MDA-06：只读复核审批页没有发现新的安全纯视觉修正；卡片标题/事实区、批准/拒绝按钮与事件链入口已有对应 CSS 规则。额外审批动作/历史/摘要字段及媒体审批直达 Task 预览仍待用户决定或真实 API 合同，页面状态和完整视觉验收未闭环，MDA-06 不勾选。
- MDA-07：再次对照资源页 HTML、当前源码和已有本地合成截图，没有发现新的纯视觉安全修正。38pt 菜单/添加按钮、分组顺序、行图标和文字间距、浅色画布与 HTML 对应，列表尾距有 30pt 实测；继续使用真实 Host/Workspace 状态，不展示已审核移除的租约倒计时/冗余说明，也不把示例云手机/AI 状态伪装成真实数据。普通 Host/Workspace 单一状态的整页截图仍缺，MDA-07 不勾选。
- MDA-08：将简体中文创建按钮从“创建·团队可见”调整为“创建 · 团队可见”，繁体同步补齐分隔符两侧空格，对应 HTML `wiCreate` 文案。`Localizable.xcstrings` JSON 解析及 `git diff --check` 通过；`testMordeWorkItemsRouteShowsDesignFiltersAndCreateForm` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA08-workitem-copy-space-20261109-r1.xcresult`），截图 `/tmp/MDA08-workitem-copy-space-attachments-20261109/59062844-DD3B-46AF-8BED-E8466C97A17B.png` 已检查，创建弹层文案显示正确。新发现行内描述/验收标准/阻塞原因/关联 Task 与交付版本等额外文本，以及键盘工具栏“Done”按钮已登记到待审核表；未擅自隐藏或移除。
- MDA-11：按 HTML 文件库 `.circbtn` 中 19×19 viewBox 的两条路径（16/24 宽、2.1/24 粗、6/24 中心距），将 Morde 两种文件库/全局页头菜单图形调为 13×1.7pt、3.1pt 线间距，38pt 点击区和 NanoMuse 阴影风格保持；PDF/DOC/图片徽标填充与文字色精确对应 `.fic.pdf/.doc/.img` CSS RGB，旧 NanoMuse target 色值不变。`testMordeFullLayoutUploadsAndSendsImageAndPDFFixture` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA11-library-colors-menu-final-20261109.xcresult`），导出并检查文件库截图 `/tmp/MDA11-library-colors-menu-final-attachments-20261109/86C5C42A-17DC-401C-A3F3-C5AF07121EDD.png`；筛选、图片/PDF 发送/预览和回切路径未回归。MDA-11 整页视觉、来源/metadata 状态与本机文件入口去留仍未闭环，保持 `[ ]`。
- MDA-04：用户本轮再次确认移除详情请求失败、事件流失败及未确认历史的三个页面级“重试加载”；现有实现符合，详情错误说明保留、事件流自动重连、返回重进重新读取、失败批次业务“重试”保留。既有 Morde 三状态恢复测试 3/3（`/tmp/MDA04-no-retry-final-20261109.xcresult`）；无需再次改产品代码。

### 2026-11-09 · MDA-04 排队步骤待办标记

- 对照 HTML `.ev.todo .ck` 与 TaskStep OpenAPI 状态集合，`queued` 是已知尚未开始步骤，设计稿使用浅灰空心圆；iOS 原先将它落到通用 `unconfirmed` 样式。现将 `queued` 单独映射为已有 `.upcoming` 待办标记，未知/取消状态保持原显示。
- 本次仅改 `NanoMuse/MIFTaskViews.swift` 中步骤标记分支；未增加控件或改变步骤文案/顺序。独立 `Morde` generic iOS Simulator target 编译通过（`/tmp/MDA-design-alignment-build-20261109`），没有运行测试或 Simulator 视觉复核，MDA-04 仍 `[ ]`。

### 2026-11-09 · MDA-14 分享生命周期与 MDA-15 接管态对照

- MDA-14 只读复核确认现行合同符合已批准的单人领取、登录、7 天未领取失效、领取后 token 失效、view/edit 权限及接收方独立副本规则；`claiming + target` 旧链接兼容仍待用户决定。源码和迁移结构显示 target 是接收方自己的 Workspace/独立卷；删除源 Workspace 会清理 share link 与 reservation，删除 share link 不会级联删除 target，但尚无覆盖这些删除路径的端到端验收。另缺源 Workspace 变更导致链接失效的服务端验收，以及接受成功但客户端丢响应后从接收方 Workspace 列表恢复的 UI 验收。未发现显式撤回未领取 Workspace 分享链接的 API；用户尚未授权该功能，未添加接口或按钮。没有连接 PostgreSQL 测试环境，也未运行相关集成测试。
- MDA-15 静态合成截图的页头、62pt 侧栏、卡片留白、状态徽标和底部 Stop/接管操作与 HTML 几何基本吻合。设计稿的用户接管态底栏另有键盘/触控/截图/Workspace 快捷区；现有 iOS 截图/关闭/Workspace 分别在页头或菜单，尚未确认是否补键盘与触控按钮。已单独询问用户，回答前不新增控件。`/tmp/MDA15-screenshot-preview-fresh-current.png` 为黑屏，不计入视觉证据；没有连接真实 Gateway/Host。
- 以上均为源码/原型只读对照；除 MDA-04 queued 步骤标记的 1 行映射外，没有其他源码或 API 变更。页面仍需按各自门槛验收，未勾选 MDA-04/14/15。

### 2026-11-09 · MDA-15 页头相机图标矢量对齐

- 用户提供的云电脑页设计稿以 24×24 viewBox、1.8px 圆角线稿绘制截图按钮；Morde 页头原来用 SF Symbols `camera`。现按相同 path 坐标以原生 Canvas 重绘，并保留 19pt 图形、38pt 点击区、原位置和原截图行为。
- 独立 `Morde` generic iOS Simulator target 编译通过（`/tmp/MDA-design-alignment-build-20261109`）；该次只完成编译，不把源码形状对照记为视觉验收。后续 Simulator 截图复核见本清单的“MDA-15 精简云电脑页和截图入口复核”。编译输出只有现存 ObservableObject 主线程隔离警告，没有本次修改的编译错误。此修正没有增加按钮或网络/Host 行为。

### 2026-11-09 · MDA-02/MDA-13 消息与设置按压态对齐、MDA-04 答复登记

- MDA-02：按 HTML `.msg.me` 将浅色用户消息气泡调整为 `#F0F0F2`、20pt 圆角及 15/10pt 内边距；助手消息显示设计稿的 “Morde” 13pt semibold 标识，并移除原有灰色气泡底和内边距。深色模式继续使用 Morde/nanoMuse 色板。没有增加消息按钮或改变消息行为；多轮消息截图的后续 Simulator 复核见本清单“MDA-02 多轮聊天气泡 Simulator 视觉复核”，MDA-02 因其他验收未闭环仍保持未勾选。
- MDA-13：设置根页行与急停操作行按压时按 HTML `.srow:active` 降至 0.55 opacity，松开恢复；未改变行数、点击行为及详情路由。当前仅完成 build，需后续视觉验收，MDA-13 保持未勾选。
- MDA-04：用户再次确认移除详情请求失败、事件流失败、事件历史未确认时出现的 3 个页面级“重试加载”按钮；返回任务列表后重新进入读取详情/历史，事件流自动重连，失败步骤的业务“重试”保留。实现与已有三状态 Morde Simulator 3/3 恢复证据一致（`/tmp/MDA04-no-retry-final-20261109.xcresult`），无需重复改代码，也不据此勾选整页。
- 本机 HTML 原型在内置浏览器走查了抽屉、工作项筛选/创建、聊天发送、云电脑接管与快捷入口、Workspace 分享权限选择、任务详情、好友占位、文件库筛选/详情及设置入口。交互响应来自本地 HTML/JS 演示；走查后重载页面恢复初始样例，未复制分享链接、未触发真实 API/Host 操作、未外发文件。
- 独立 `Morde.xcodeproj` / `Morde` scheme generic iOS Simulator build 成功（`/tmp/MDA-design-alignment-build-20261109`）；截至此条记录只做编译。后续聊天与云电脑截图验收见本清单后续记录。编译存在原有 `ObservableObject` 主线程隔离警告，本次无编译错误；`git diff --check` 通过。MDA-02/04/13/15 的其他页面状态和逐控件视觉门槛仍未闭环，相关整页任务保持未勾选。

### 2026-11-09 · MDA-02 多轮聊天气泡 Simulator 视觉复核

- 独立 `Morde.xcodeproj` / `Morde` scheme 在 iPhone 18 Pro / iOS 27.0 Simulator 运行既有 `testMordeChatMultiTurnVisualFixtureMatchesDesignDensity`，通过 1/1（`/tmp/MDA02-chat-visual-signed-20261109.xcresult`）。导出并检查截图 `/tmp/MDA02-chat-visual-signed-attachments-20261109/2BE29404-3E92-40BB-B0DD-DDFB1CF91B93.png`：合成 7 条多轮消息中，浅色用户气泡呈浅灰填充和圆角，助手消息显示 “Morde” 标识且没有灰色气泡底；与 HTML 聊天消息样式规则一致。
- 这项截图只覆盖合成聊天流，不代表真实会话数据、附件路径或整页所有控件已验收。由于 MDA-02 仍有真实数据路径及逐项页面验收未闭环，继续保持 `[ ]`。
- 结果包和截图保存在本机 `/tmp`，没有发送或外发文件；源码 `git diff --check` 通过。

### 2026-11-09 · MDA-15 精简云电脑页和截图入口复核

- `testMordeComputerIOSWorkbenchKeepsOnlyDesignTabsAndConciseStatus` 与 `testMordeComputerCapturePreviewOpensAfterWorkbenchLoads` 在独立 Morde scheme / iPhone 18 Pro / iOS 27.0 Simulator 通过 2/2（`/tmp/MDA15-visual-camera-final-20261109.xcresult`）。前者确认常驻浏览器、终端、文件三个入口，无 Workspace/VS Code 常驻 Tab、续租、持有倒计时及长说明；导出截图 `/tmp/MDA15-visual-camera-final-attachments-20261109/BEFF0C31-1F55-48FD-A290-15F677EF1EC3.png` 已检查。相机按钮位置、颜色与图形符合 HTML `.pc-circ` 和截图 SVG。
- 截图预览测试确认点击后打开预览 sheet，但其 Simulator 附件 `/tmp/MDA15-visual-camera-final-attachments-20261109/396F36E7-292B-42BF-8D13-7B192827B88F.png` 显示全黑，不能证明捕获图像本身正确；保留为待排查视觉验收，不将黑屏截图记作通过证据。MDA-15 仍有真实 Gateway/WebView 及键盘/触控快捷区审核未闭环，继续保持 `[ ]`。
- 本轮源码 `git diff --check` 通过；测试及截图只保存在本机，没有上传或外发文件。

### 2026-11-09 · MDA-03 UIKit 回弹回退专项与设计稿浏览器逻辑走查

- 新增 Morde UI 用例 `testMordeTaskListRecoversFromLoadFailureWithLegacyUIKitBounceFallback`，仅修改 `MordeUITests/IOS03SimulatorAcceptanceUITests.swift`。通过合成首次加载失败和 `-morde.forceLegacyScrollBounceFixture` 强制使用 UIKit 回弹探针；断言错误态不出现 Retry，随后下拉恢复空列表。用例在 iPhone 18 Pro 形态、iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA03-legacy-bounce-fallback-20261109.xcresult`）。这只验证当前 Simulator 上被强制选择的回退实现，不替代 iOS 16.0～16.3 runtime/真机验收；MDA-03 保持 `[ ]`。
- 在内置浏览器打开本地 `127.0.0.1:8765` 的 `Morde iOS设计稿.html`，逐项操作任务列表/详情返回、审批列表、资源与云电脑接管、浏览器/终端/文件切换、Workspace 子目录/面包屑返回、好友占位、AI 工具只读会话、文件库筛选和打开提示、设置详情提示、工作项筛选/本地创建及聊天发送回显。上述是 HTML/JavaScript 原型内的交互，不代表 Morde API/Host 已联通。创建的测试工作项和发送的测试消息均在原型 reload 后恢复为初始样例；未触发审批决策、急停、真实分享、文件上传或外部发送。
- 浏览器结果确认设计稿内的用户接管态包含键盘、触控、截图、Workspace 文件四个快捷按钮；该需求稿内已有键盘/触控待审核记录，需结合用户“按设计稿组件/位置实施”的指示更新处理结论。源码 UI 测试 `git diff --check` 通过，改动与原型操作都在本机完成。

### 2026-11-09 · MDA-15 修复云电脑截图预览黑屏

- 对照之前的 Simulator 附件发现：云电脑页面正常，但 `WKWebView.takeSnapshot` 在测试 WebView 上生成黑图，原测试只确认预览 sheet 打开，没有检查图像内容。改用 `UIGraphicsImageRenderer` 对已显示的 WKWebView 层级取图，按 WebView 当前 bounds 生成本机预览；不保存到文件或 Workspace、不增加按钮。
- `testMordeComputerCapturePreviewOpensAfterWorkbenchLoads` 增加预览图无障碍元素断言，在 iPhone 18 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA15-capture-drawhierarchy-final-20261109.xcresult`）。导出的预览截图 `/tmp/MDA15-capture-drawhierarchy-final-attachments-20261109/1972A025-2B49-4FFC-8343-F2DC2EAE0416.png` 已检查，能看到完整浏览器、侧栏和新标签页内容；之前黑图记录被此结果替代。
- 此修正仅验证本机合成 WebView；真实 Gateway/Host 内容、键盘/触控快捷区及云电脑整页视觉仍未闭环。等待键盘/触控快捷操作是否按设计稿放回底部、交还操作是否移入菜单的待审核答复；MDA-15 保持 `[ ]`。
- 源码与清单改动仅在本机；没有上传、发送或外发文件。

### 2026-11-09 · MDA-04 重试决定复核与 MDA-06 审批截图对照

- MDA-04：用户确认移除 Task 详情读取失败、事件流失败、历史未确认时的三处页面级“重试加载”，返回列表后重新进入恢复读取。当前实现和三状态 Simulator 用例已满足决定；事件流自动重连、失败步骤自己的业务“重试”保留，不重复改动产品代码，也不据此勾选 MDA-04 整页。
- MDA-06：将本地 HTML `appr` 页与 Morde Simulator 截图 `/tmp/MDA06-approval-canvas-align-attachments-20261109/D2FCFD2F-6CFD-434E-9472-77863E7E381C.png` 对照。菜单/标题/待办数位置、卡片横向边距、标题与事实文字层级、批准/拒绝胶囊比例、事件链入口及浅色画布对应 HTML 主结构；没有发现不涉及动作、权限或真实数据的确定性视觉修正。截图为 1 条真实合成待办，原型为 2 条演示待办并包含自动处理历史，因此可见内容高度不同；不能用虚构待办/历史填充空白。原型差异摘要、第三决策动作、自动处理历史及媒体审批 Task 预览入口仍待审核或真实 API 合同，当前不增删/伪造。
- 本次审批对照仍只覆盖一条已确认待办的首屏；错误、媒体、空态和其他待办状态/整页仍未全部完成，MDA-06 保持 `[ ]`。仅更新本地实施记录；没有外发文件。

### 2026-11-09 · MDA-08/11/14 页面复核与原型流程验收

- MDA-08：独立复核 `MIFWorkItemViews.swift` 与 HTML `.seg`、`.row`、`.claimbtn`，筛选字形/间距、44pt 图标、行文字层级/留白和认领按钮尺寸/位置均有对应规则；项目/Task 上下文、描述及额外行操作属待审核差异，未改动。MDA-08 仍未完成。
- MDA-11：独立复核 `MIFLibraryViews.swift` 与 HTML `.seg`、`.fic`、`.row`，筛选按钮、文件类型徽标和列表行的尺寸/字形/留白已有对应实现；空态和待审核入口保持现状，未改权限或预览行为。MDA-11 仍未完成。
- MDA-14：按 HTML `.head h2` 和 `.row .tt b` 将 Workspace 标题字距对齐为 `-0.17pt`，文件/目录名增加 `1.5pt` 行距；不改控件、操作或数据。`testMordeResourceWorkspaceRouteBrowsesSyntheticManifest` 在 iPhone 18 Pro / iOS 27.0 独立 Morde Simulator 通过 1/1（`/tmp/MDA14-workspace-visual-20261109-r2.xcresult`），覆盖资源→Workspace、标题长按上传/版本菜单、子目录/空目录、逐级返回和资源返回。导出并检查截图 `/tmp/MDA14-workspace-visual-20261109-r2-attachments/1EBAD822-3436-4CAC-A0F7-68053E9D63CE.png`：页头、挂载摘要、根路径、平铺行和四项底部 Tab 可见；fixture 使用合成数据，未传输真实文件。Workspace 真实数据的完整状态及 HTML 与最终 Simulator 全页对照仍未完成，MDA-14 保持 `[ ]`。
- MDA-04：在 iPhone 18 Pro / iOS 27.0 独立 Morde Simulator 复跑详情读取失败返回重进恢复、事件流失败自动重连、历史未确认态三个已审核场景，0 失败/3 通过（`/tmp/MDA04-no-retry-current-20261109.xcresult`）。结果包确认无页面级“重试加载”；业务失败步骤的重试仍保留。MDA-04 其他页面状态与完整视觉门槛仍未完成，保持 `[ ]`。
- 内置浏览器在本机 `127.0.0.1:8765` 重置 HTML 原型后，走通聊天发送回显→任务列表/详情→审批→事件链→资源→Workspace 根目录/子目录/空目录/返回。交互仅改变本地 HTML/JavaScript 演示状态，不调用 Morde API 或 Host；没有触发审批决策、分享创建或文件传输。
- MDA-08/11 审计未找到可安全实施的纯视觉差异；本轮没有改动这两页，也没有删除/新增待审核控件。所有变更、测试和原型操作均留在本机，未外发文件。

### 2026-11-09 · MDA-02 聊天输入栏字号与入口复核

- Morde 聊天输入框按 HTML `.composer input` 使用 15pt 字号；此字号只作用于 `designCompactLayout`，不改变旧 NanoMuse 布局。空闲发送箭头点击聚焦输入框；麦克风常驻按钮隐藏，语音、附件和 Task options 仍从已审核的长按菜单打开。没有增加常驻按钮或改变附件/API 行为。
- `testMordeEmptyComposerSendFocusesInput` 和 `testMordeChatLongPressActionsOpenTaskOptionsAndImporter` 在 iPhone 18 Pro / iOS 27.0 Morde Simulator 各通过 1/1（`/tmp/MDA02-composer-align-20261109.xcresult`）。多轮视觉用例在该共享 Simulator 首次读到了既有待发送附件草稿，未通过样例消息断言；未清除已有数据，改用新建隔离 Simulator 单独运行 `testMordeChatMultiTurnVisualFixtureMatchesDesignDensity`，通过 1/1（`/tmp/MDA02-chat-visual-isolated-20261109.xcresult`）。已检查截图 `/tmp/MDA02-chat-visual-isolated-attachments-20261109/BF8D9780-19E6-4515-94C6-ABC7C58EB2FB.png`，七条样例消息、用户浅灰气泡、助手 Morde 标识、单行输入和底部 Tab 可见。
- 内置浏览器重新打开本机 `127.0.0.1:8765` 的设计稿并检查聊天输入栏；与 Simulator 截图核对了 15px 输入文本及 34px 发送圆钮尺寸。该验证只覆盖合成消息与输入区域；真实会话附件错误/取消状态和其余聊天页面视觉仍未完成，MDA-02 保持 `[ ]`。源码 `git diff --check` 通过，测试与截图仅保存在本机。

### 2026-11-09 · MDA-15 云电脑菜单入口答复登记

- 用户确认将 Workspace、主机文件、VS Code 和窗口操作入口移入长按/菜单；现有移动端常驻页签保留浏览器、终端、文件，已通过 `testMordeComputerIOSWorkbenchKeepsOnlyDesignTabsAndConciseStatus` 验证。续租按钮、租约倒计时及长说明已按用户要求从主页面隐藏，后台租约与自动续期逻辑不改。
- 复核 `Morde iOS设计稿.html` 的用户接管态 `.pc-bar#barUser`：原型底栏显示“停止任务”以及键盘、触控、截图、Workspace 四项快捷操作；当前原生底栏显示“停止任务”和“交还给 AI”。两项入口集合不一致，先不自行增删/迁移控制操作，等待用户决定按设计稿保留四项快捷操作并把“交还 AI”移入菜单，还是保留当前两项并将四项快捷操作移入菜单。现有控制权限和租约 API 不变。
- 当前结果仅登记用户已确认的菜单收敛与一个待决布局，不改产品代码；MDA-15 仍因用户接管态布局、截图写入 Workspace 授权和真实 Gateway/WebView 验收未闭环而保持 `[ ]`。本地测试/记录没有外发文件。

### 2026-11-09 · MDA-03 任务列表源码与设计稿样式复核

- 对照 HTML `.row` / `.ric` / `.row .tt` 及 Morde `MIFTaskListRow`：横向图标与文字间距 13pt、行上下内边距 13pt、状态图标 44pt、标题/摘要/状态 16/14/13pt 和字距、颜色、行内留白已有对应实现；包含摘要的行最小高度 91.6pt，符合当前页面密度实现。本轮未发现能安全单独修正且不会改变已对齐页面布局的视觉差异，没有改产品源码。
- HTML 示例中的执行者、审查者和百分比是演示内容，现有真实 Task API 不提供对应字段；列表继续显示授权扩展后的真实步骤计数、当前步骤/结果与状态，不用演示文案替代真实数据，也不新增按钮或行操作。
- 这次是源码规则复核，既有五行样例截图和 Simulator 验收证据仍有效；iOS 16.0–16.3 runtime 未安装，其他任务状态/尺寸组合与整页验收未闭环。MDA-03 保持 `[ ]`，改动只记录在本机实施清单，未外发文件。

### 2026-11-09 · MDA-06 审批计数文案与 MDA-08/MDA-13 只读审查

- MDA-06：审批数量从冗长的本地化句子收紧为 HTML 设计稿同类的“%d 件”格式，保持原有登录态、完整加载和同一范围的数据门槛，不改变计数来源或审批行为。9 种支持语言的现有 key 已更新；专用 iPhone 18 Pro Simulator 定向用例通过 1/1，截图已检查。一次复跑在已有共享 Simulator 上误触资源入口、未到达审批页；换用干净专用 Simulator 后通过，不据此改产品逻辑。额外动作、历史及媒体直达预览仍待决定/真实 API 合同，MDA-06 保持未勾选。
- MDA-08：复核列表/筛选/创建与三个错误态后，三个页面级 Retry 已按用户决定移除，下拉刷新保留并覆盖相应恢复；现有列表/创建视觉未发现确定且安全的 CSS-only 修正。项目选择、Task 上下文、描述、关联/交付/阻塞操作及额外行文案仍待审核，未修改这些产品入口。MDA-08 保持未勾选。
- MDA-13：复核设置根页 10 行、3 个分组和 `.srow` 排版；当前标题行、分组间距、按压态和 chevron 已有对应 HTML 实现，没有安全的纯视觉差异可改。账号/设备入口、Portal 凭证、Hub 控制/撤销与重试、占位详情和全局急停恢复等行为已有边界，仍需按待审核表决定去留；本次只读审查未修改设置功能。MDA-13 保持未勾选。
- 本条仅将审查和验收结果写入本机实施文档；未向外发送文件或连接真实 Host/账号。

### 2026-11-09 · MDA-03 任务行间距按 HTML 校准

- 复核设计稿 `.row .tt b/span/time`：标题到次级信息 `2px`、次级信息到状态 `3px`。`MIFTaskListRow` 原来的 `VStack(spacing: 2)` 与标题/详情/状态额外 padding 累加成约 `4pt/6pt`；现将间距逐项对应为有详情 `2pt/3pt`、无详情标题到状态 `3pt`，不改文字、真实计数、点击目标或按钮数。
- 独立 `Morde` scheme / iPhone 17 Pro / iOS 27.0 Simulator 重跑 `testMordeTaskListShowsStepCountsFromSummary` 与 `testMordeTaskListRendersKnownAndUnknownStatusesWithoutActions`，通过 2/2（`/tmp/MDA03-spacing-align-20261109.xcresult`）。截图 `/tmp/MDA03-spacing-align-attachments-20261109/EBEAF4F6-863F-437E-A813-02026BC696DF.png`（有真实步骤计数）和 `/tmp/MDA03-spacing-align-attachments-20261109/87B2B334-7454-4BF5-9FD2-600746C1B6CC.png`（八种状态及长列表）已检查，行尾到 TabBar 胶囊仍为 30pt。
- 内置浏览器走查本地原型：任务列表打开详情、返回列表、点“新建任务”进入聊天并聚焦输入框，路径可用。iOS 16.0～16.3 Simulator runtime 当前未安装；全状态并排视觉仍需继续验收，MDA-03 保持 `[ ]`。

### 2026-11-09 · MDA-03 紧凑步骤摘要与审批状态复验；MDA-04 重试入口确认

- MDA-03：任务摘要按设计稿压缩为真实计数（如“5 步 · 4 步完成”），审批等待原因显示“等你审批”。Morde `testMordeTaskListRendersFiveDesignRowsWithoutExtraActions`、`testMordeTaskListRendersKnownAndUnknownStatusesWithoutActions`、`testMordeTaskListShowsStepCountsFromSummary`、`testMordeTaskWaitingDetailsShowApprovalAndClarificationControls` 在 iPhone 17 Pro / iOS 27.0 Simulator 通过 4/4（`/tmp/MDA03-compact-task-rows-20261109.xcresult`）；五行截图 `/tmp/MDA03-compact-task-rows-attachments-20261109/F4C6057E-D630-4DDF-8A02-2006F97877C4.png` 已目视检查，五条样例、步骤计数、审批状态与底部导航可见，没有增加行操作。`Localizable.xcstrings` JSON 解析通过。
- MDA-04：按用户确认，详情读取失败、事件流失败、事件历史未确认时均不显示页面级“重试加载”；返回任务列表后重新进入读取，事件流自动重连。源码核对确认保留的 Retry 仅用于失败批次业务重试；既有三种恢复路径 Morde Simulator 用例通过 3/3（`/tmp/MDA04-no-retry-final-20261109.xcresult`）。
- MDA-03 仍保持 `[ ]`：完整多状态逐页视觉验收未闭环，且当前没有 iOS 16.0～16.3 Simulator runtime。上述四项测试中仍记录到 `Invalid frame dimension (negative or non-finite)` 运行时警告，来源标记为 `MordeStandaloneApp.swift`，尚未定位到可安全归因的产品源码行。

### 2026-11-09 · MDA-05 事件链标题与摘要字距按 HTML 校准

- 对照本地 HTML `.lead-sub`、`.ev b`、`.ev span`：任务说明固定为 14.5pt；事件标题保持 15.5pt 并补齐对应行距；摘要调整为 14pt/对应行距，标题与摘要间距从 4pt 收至设计稿的 2pt。仅改事件链文字排版，不改节点顺序、事件状态、刷新/返回路径或按钮数。
- `testMordeEventChainTimelineDensityAndStatusesMatchPrototype` 在 iPhone 17 Pro / iOS 27.0 Morde Simulator 通过 1/1（`/tmp/MDA05-event-chain-typography-final-20261109.xcresult`）。截图 `/tmp/MDA05-event-chain-typography-final-attachments-20261109/EDBB1186-5606-4E4F-BADD-DAD1D1374617.png` 已检查：七个节点、完成/当前/排队标记及底部导航均可见，预算字段未出现。该截图使用本机英文合成事件数据，不代表服务端真实文案。
- MDA-05 已满足整页门槛并维持 `[x]`；本轮内置浏览器另走查了 HTML 任务列表→详情→返回→“＋”新建任务跳转聊天并聚焦输入框，反馈来自本地原型，不代表 Morde API 联调。MDA-03 仍为 `[ ]`，其旧系统运行时和完整状态视觉门槛未完成。

### 2026-11-09 · MDA-06 审批提交后的事件链入口差异

- 源码核对发现：Morde 接受批准/驳回后，等待 Task 状态刷新时会替换响应按钮为状态说明，但仍显示“看事件链”；HTML `.is-done .acts,.linkrow{display:none}` 会同时隐藏动作和事件链链接。随后用户确认：保留入口直到 Task 详情确认该审批不再待处理；待确认或详情刷新失败时继续显示。
- 内置浏览器验证 HTML 行为：批准首条合成审批后，原型显示“已批准 · 经理执行中”、待办计数从 2 件变为 1 件，卡片中的“看事件链”入口消失；随后 reload 恢复原始样例。此流程仅操作本机 HTML/JavaScript，不调用 Morde API。
- Morde 现按已确认的状态门槛控制入口。`testMordeApprovalEventChainLinkHidesAfterTaskStatusIsConfirmed` 与 `testMordeApprovalInboxShowsOnlyConfirmedDecisions` 在 iPhone 17 Pro / iOS 27.0 Simulator 通过；前者验证已确认后隐藏，后者验证状态仍待确认时保留。MDA-06 仍因灰度/改天数动作、自动处理历史、变更摘要、媒体预览入口及其他状态的完整视觉验收未闭环而保持 `[ ]`。

### 2026-11-09 · MDA-07 顶部提示与 MDA-15 控制状态文案

- MDA-07：资源页“＋”继续保持单一按钮和原有无效操作反馈；Toast 按 HTML `.toast` 规则放在页面顶部、使用 13pt semibold / 15pt 水平内边距 / 0.95 黑底 / 同类阴影，并显示 2.2 秒。`testMordeResourceWorkspaceRouteBrowsesSyntheticManifest` 在两项用例的组合结果包中单独通过 1/1，验证“添加资源”无障碍标签、“暂未实现”提示位于屏幕上半区且不弹 Alert；同包旧 MDA-15 合成 Host 夹具失败，修复后的 MDA-15 回归见上一条 4/4 结果。本地截图 `/tmp/MDA-compact-copy-alignment-r4-attachments-20261109/1D3CA0B0-761F-4CE5-B408-C82C2136DD83.png` 已检查，Toast 相对页头的位置、文案和时长与原型规则一致。
- MDA-15：当前用户拥有控制权时，标题/徽标改用 HTML 对应短文案“全由你掌控”/“由你控制”；Host 票据请求失败只在云电脑页显示“连接失败”，详细服务端错误仍可在资源 Host 行查看。`testMordeComputerControlStatusErrorHasNoRetryButtonAndRecoversOnReentry` 与 `testMordeComputerRouteAcceptsSyntheticTicketAndReturnsToResources` 均通过；前者覆盖失败返回重进，后者覆盖控制态标题/徽标和本地合成 Workbench。
- 本轮独立 Morde iPhone 17 Pro / iOS 27.0 Simulator 共 4/4 通过，结果 `/tmp/MDA-status-copy-approval-confirmation-20261109.xcresult`。MDA-07、MDA-15 的真实资源/Host 状态、剩余审核差异和全页视觉仍未闭环，保持 `[ ]`。仅在本地修改源码、测试及清单，没有外发文件。

