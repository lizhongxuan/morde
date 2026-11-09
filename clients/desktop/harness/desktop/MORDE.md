# Morde 桌面客户端

Morde PC 基于 nanoMuse Electron 外壳。一次账号密码登录后，窗口内直接使用与普通用户 Web 相同的 Portal 页面；本机目录同步是 PC 专属功能。桌面主进程把 Client/Sync 凭证存入操作系统安全存储，把短期用户 Web 会话放入独立内存分区的 HttpOnly Cookie。远端页面不能调用本机目录或同步 IPC。

## 本地构建

在 `clients/desktop/harness/desktop` 运行 `npm ci`、`npm test`、`npm run dist:dir`。打包前从仓库根目录的 `runtime/morde-sync` 源码构建 Go 同步程序。开发运行可把 `MORDE_SYNC_BINARY` 指向本机同步程序。`npm run dist:dir` 生成当前系统目录包，`npm run dist` 生成当前系统安装包；跨平台构建命令见 `package.json`。跨平台打包不能代替目标系统安装验收。

macOS 当前为未签名开发包。正式签名、公证以及 Windows/Linux 原生安装须分别验收后才能发布。

## 登录与退出

1. 输入 Morde 服务的 HTTPS 根地址；本机开发可使用 `http://localhost` 或 `http://portal.localhost`。
2. 输入已有账号和密码并登录。服务端自动登记这个安装，不需要设备码、核对或批准。桌面重启时，只要 Client 凭证仍有效，短期用户 Web 会话会自动续取。
3. 登录成功后“AI 助手”进入 Portal `/assistant`，可选择已检查且支持文本的模型，并使用云端 Task 与云端电脑 Workbench；“账户设置”打开 Portal `/settings`，可查看账号、改密和管理客户端；“本机同步”仅在 PC 窗口提供。Admin 使用独立浏览器登录。
4. “退出登录”会请求服务端撤销此安装，再清除本机凭证、关闭云端电脑窗口并停止同步。本机文件、按服务地址和账号隔离的目录映射及同步恢复点保留；切换账号不会读取另一账号的映射。断网退出后，应在 Portal 的“客户端”列表撤销安装。

安全存储不可用时，先解锁 Keychain、GNOME Keyring 或 KDE Wallet。客户端不把密码保存到磁盘；Client 到期后需再次输入账号密码。

本地界面验收可设置绝对路径环境变量 `MORDE_TEST_USER_DATA`，把凭证、账号隔离状态和同步恢复点写入独立测试目录。未设置时仍使用默认 `Morde Client` 数据目录；正式 appId 不变。

## 本机 Workspace 同步

先选自己当前可用的 Workspace，再选择本机目录，才会开始扫描。每个 Workspace 对应一个独立目录；路径或文件系统身份改变时须重新选择。上传、只补缺失、双向补缺失、实时同步和交接检查点都由包内 `morde-sync` 执行，权限由服务端逐请求按当前账号、安装和 Workspace 状态核验，不预先勾选授权范围。

中断时保留会话 ID、幂等键和本机恢复点；用同一操作恢复，或在确认旧进程不再占用目录后明确放弃。目录操作锁和 PID 核验防止两个同步进程同时写同一目录。退出时若无法确认同步进程已释放目录，应用会保持打开并保留恢复点；确认后可再次退出。退出和撤权都不删除本机文件。

若本机目录映射或恢复点文件损坏、无法读取，客户端会暂停该账号的本机同步并显示文件位置。先备份并修复该文件，再重启客户端；用户 Web 仍可使用，不会自动覆盖损坏的记录。

## 当前本地验证

2026-10-27：`npm test` 40/40 通过。macOS arm64 原生包连接本机当前 Portal/PostgreSQL 与 Ubuntu/Lima OpenSandbox；账号密码登录即授权，重启后无感恢复，在应用内打开当前 Portal 用户页面以及自有 Host 的桌面、终端、文件和 VS Code。共享账号在应用内打开获授权的只读桌面和终端，撤权后终端连接断开。真实 Host 开窗时发现 Electron 未传入平台提供的 `createWindow` 参数会触发主进程错误，已修复并原生复验。四平台目录包和安装包的应用归档 SHA-256 一致（`8f6826704e05035e657952ee93421f4ef9fb5bd6834ef0235e44d0e41e3f94ed`），两份 macOS DMG 校验通过。此前本机同步夹具已覆盖补缺、LiveSync、冲突、强退和数据库短暂不可用恢复；Host 文件上传/下载、Windows/Linux 原生安装、正式签名与公证仍待验收。

随后修复 Host 完成同步却未成功上报“已应用”的回执丢失：完成会话保留到回执成功，失败和重启重试同一 session。修复后的四平台同步程序及安装包已重建，包内 SHA 清单核对通过；桌面 40/40、morde-sync/morde-runtime 全量 Go 测试及服务端本机 PostgreSQL 全套通过。本机 Ubuntu 26.04 x64 容器运行 Linux 目录包可显示 Morde 窗口；远端 Ubuntu、Windows 原生及签名/公证仍待验收。

桌面 Web Session 定时续取遇到断网时现会安全等待重试，不再留下未处理异常。桌面 41/41 与服务端真实 PostgreSQL 的过期续取专项/race 测试通过。四平台本地包再次重新封装，应用归档 SHA-256 为 `c9474727bf977fe286638503e5812d1c99216af76774491b0c9db0acf1ca9320`；DMG、NSIS、AppImage 内层均核对一致，macOS arm64 新包已原生启动。正式签名与目标机原生验收仍待完成。
