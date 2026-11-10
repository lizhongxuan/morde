# Morde Linux 测试主机联验

日期：2026-10-26（Asia/Shanghai）  
执行环境：Ubuntu 26.04 x86_64，4 vCPU、8 GiB RAM；通过 Xvfb 启动桌面应用。  
范围：只验证 Linux 主机、Portal、Gateway 和合成 Host；按用户要求不测试手机端。

## 构建产物

- 最终 AppImage：`morde-desktop-0.1.0-linux-x64.AppImage`
- SHA-256：`95739b4607bc81056bfd027c8a798119b31e7dc6ce2a96f684a160742cfc7767`
- 构建：`npm run dist:linux:x64`
- 包含的同步程序：Linux x86_64 ELF；包内 `morde-sync.sha256` 与二进制匹配。
- 只将构建后的 AppImage 临时复制到测试主机；没有传源码、项目文件或同步测试数据。验收后删除测试主机上的 AppImage、解包目录和临时授权材料。

## 验收结果

| 检查 | 结果 | 证据 |
|---|---|---|
| Portal 与 OpenSandbox 健康 | 通过 | Portal `/readyz` HTTP 200；OpenSandbox `/health` HTTP 200；OpenSandbox 网络策略检查 5/5 通过。 |
| Linux 桌面启动 | 通过 | 以非 root `morde` 用户通过 AppImage 自身的 `APPIMAGE_EXTRACT_AND_RUN=1` 路径启动；DevTools 页面标题为 `Morde`。 |
| 启动闲置网络 | 通过（限定本次观测） | 冷配置启动 45 秒，每 5 秒采样一次进程 TCP 连接，共 9 次，均未发现 Morde 进程的外部 TCP 连接。 |
| Client 配对与 Host ticket | 通过 | 合成 Portal 账号批准 Linux Client；Client 的 Host 列表包含测试 Host；Client `POST /api/hosts/{id}/enter` 返回 200；Gateway ticket 兑换返回 302，`/__ch/api/host` 返回 200 且 Host ID 一致。 |
| 撤销活动 Gateway 连接 | 通过 | `/ws` WebSocket 握手返回 101；Portal 撤销 Client 返回 204；连接在 1.7 秒内关闭；撤销后的 Client Host API 返回 401。 |
| 合成 Host 清理 | 通过 | 测试 Host `hm8bey60` 删除请求返回 202；后台回收后对应 OpenSandbox 容器已消失。 |
| 桌面自动化测试 | 通过 | `npm test`：26/26 通过；`git diff --check` 通过。 |
| 测试后清理与主机状态 | 通过 | Portal 仍 active 且 `/readyz` 为 200；PostgreSQL、OpenSandbox 和本地验收镜像仓库保持运行。Morde 客户端进程、合成 Host 容器、临时授权文件和本轮日志均已清理。 |

## 网络审计与限制

首次冷启动抓包发现 Chromium 网络服务请求 `redirector.gvt1.com` 和 `rr1---sn-j5o7dn7e.gvt1-cn.com`。Linux 打包启动器现传入 `--disable-background-networking`、`--disable-component-update`，并将这两个 GVT 后缀映射为 `~NOTFOUND`；冷启动复验 45 秒未观察到 Morde 进程的外部 TCP。规则依据本次抓到的端点，Electron 升级后应重新检查空闲流量。Electron 文档说明 `--host-resolver-rules` 控制 Chromium 主机解析；Chromium 源码定义 `~NOTFOUND` 为解析失败：[Electron Command Line Switches](https://www.electronjs.org/docs/latest/api/command-line-switches)、[Chromium MappedHostResolver](https://chromium.googlesource.com/chromium/src/net/%2B/fc2f8467dc5858b5a19e0ca568b25ae9f7f78d79/dns/mapped_host_resolver.h)。

本次验收启动命令显式带 `--no-sandbox`，只证明应用可启动；打包启动器本身没有注入该参数，因此这次结果**不代表 Chromium 进程沙箱已验收，也不足以作为正式 Linux 发行许可**。补充检查发现主机 `kernel.unprivileged_userns_clone=1`，但 `kernel.apparmor_restrict_unprivileged_userns=1`；以非 root `morde` 用户执行 `unshare -Ur true` 被阻止。后续需用目标 AppImage 和针对应用的沙箱策略实测，不应全局关闭 AppArmor 限制。当前本地指引要求文件只在本地修改、不得外发，因此不能把本机新构建的 AppImage 发送到测试主机进行该项复测。另对活动 Portal 使用的 `morde_acceptance` 数据库做只读计数，`model_connections` 为 0；已部署配置的模型出站白名单仅包含 `api.openai.com`，未读取任何密钥。没有执行 Linux 同步、更新、卸载或安装器流程。Windows/macOS 原生安装更新与签名/公证未验收。TC-15、TC-25、TC-26 按用户指示跳过，不计为通过。
