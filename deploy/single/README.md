# 基础版单机启动

> 唯一网络方案：runsc + 专用 Docker 网桥 + 宿主 nftables。固定 OSB 不发送 networkPolicy、不部署 egress sidecar。规则检查失败时 Portal 启动、readiness 和创建均失败。

目标环境为 Linux：Docker 已注册 runsc，安装 nftables、Python 3.11+（tomllib）、coreutils 和 util-linux（flock）。当前测试机只有一块系统盘，因此 Docker、PostgreSQL 和系统日志仍共享物理盘；Portal 在 Docker 数据根目录剩余空间低于 10 GiB 时拒绝新建 Host，但不提供每个 Workspace 硬配额或数据库独立保护。默认只对外开放 Caddy，Portal/数据库/OpenSandbox/Host 端口保持回环监听。F02 私有 Syncthing relay 使用独立 Compose profile，默认不启动；公网入口仅开放 TCP 22067，不开放 relay status 端口。账号节点使用普通 Docker bridge，但启动前装载的 nftables 默认拒绝其主机入站和转发，只允许节点访问 relay 主机绑定 IPv4 的 TCP 22067，并允许已建立连接的响应。Relay 不接入账号 bridge。执行前先跑 `m0/02-gvisor.sh`，证明固定 AIO 的核心接口和浏览器可用。

F02 中继由 Morde 自行托管，转发 Syncthing 加密节点流量，不加入公共 relay pool。Compose 使用独立的 `CH_WORKSPACE_SYNC_RELAY_TOKEN` 限制只有持有共享 token 的节点能加入；生成 32 字节随机 token（例如 `openssl rand -hex 32`），只保存在 root 持有、权限 `0600` 的 `/etc/cloudhost/cloudhost.env`，不要提交到仓库。桌面端通过设备登记响应取得 relay 的公网静态 URI；服务端账号节点使用 `CH_WORKSPACE_SYNC_RELAY_BIND_ADDRESS` 对应的主机 IPv4 连接同一个 relay，避免依赖云公网 NAT 回环。`CH_WORKSPACE_SYNC_RELAY_PUBLIC_ADDRESS` 必须是静态公网 IPv4 字面量和 TCP 22067，不能用 DNS 主机名；`CH_WORKSPACE_SYNC_RELAY_BIND_ADDRESS` 必须是可从账号 Docker bridge 路由到的具体主机 IPv4，不能用 `0.0.0.0` 或回环地址。nftables 按该主机 bind IPv4 的原始目的地址和原始 TCP 端口精确允许账号节点访问 relay，其他新建连接默认拒绝。主机防火墙和云安全组仅开放 relay 入站 TCP 22067。Relay 使用关闭 IP masquerading 的专用 bridge，不接入账号 bridge，也不提供节点 API 通道。桌面端公网 relay 入口、设备/Workspace folder 配置及撤权屏障通过验收前，保持 workspace-sync profile 和同步 capability 关闭。Portal 使用的 bind address 与 Compose 实际发布地址必须一致；Supervisor 会逐项比对 Docker inspect 结果。

Syncthing 节点与 relay 镜像只在本机生成，不需要推送外部 registry。创建 relay 状态目录时设为 `65532:65532` 所有、`0700` 权限；例如 `sudo install -d -o 65532 -g 65532 -m 0700 /srv/morde-workspace-sync-relay`。使用受控的 F02 Syncthing 源码 worktree 构建：

```sh
SYNCTHING_SOURCE_DIR="/path/to/local/syncthing-f02-worktree" IMAGE_PLATFORM=linux/amd64 images/workspace-sync/build.sh
```

构建脚本拒绝非本机 Docker endpoint、使用 `GOPROXY=off`，并输出两个镜像 tag 与内容 ID。将节点和 relay 内容 ID 分别填入 `CH_WORKSPACE_SYNC_NODE_IMAGE_ID`、`CH_WORKSPACE_SYNC_RELAY_IMAGE_ID`；将输出的 relay tag 填入 `CH_WORKSPACE_SYNC_RELAY_IMAGE`。再设置节点数据根目录和 relay 对外地址。Compose profile 仍须在 T04 的真实设备共享、relay 互通和撤权屏障验收后才可启动；当前没有进行这些部署操作。

1. 在目标机本地准备 `/etc/cloudhost/cloudhost.env`（root 持有、0600），按 `.env.example` 填写真实值。PostgreSQL 密码与 DSN 一致；Portal 和 OpenSandbox 使用同一 API Key。`CH_DOCKER_DATA_DIR` 必须与 Docker Engine `/info` 返回的 `DockerRootDir` 相同，`CH_DOCKER_MIN_FREE_BYTES` 默认 10 GiB。低于此余量会阻止新建 Host；已运行 Host 和 PostgreSQL 仍可能继续占用空间。
2. 若没有自有域名，可用目标机公网 IPv4 构造测试地址：`portal.<IPv4>.sslip.io` 和 `<IPv4>.nip.io`。分别设置为 `CH_PORTAL_DOMAIN` 与 `CH_HOST_SUFFIX`，`CH_CADDY_PORTAL_ADDRESS` 设为门户域名，`CH_CADDY_HOST_ADDRESS` 设为 `*.<IPv4>.nip.io`；设 `CH_SCHEME=https`、`CH_COOKIE_SECURE=true`。将两个 `CH_CADDY_*_TLS` 值分别设为多行 Caddy 配置 `tls {`、下一行缩进写 `on_demand`、最后一行 `}`。Caddy 按需申请每个具体主机名的证书，Portal 只授权门户自身和运行中的 Host/允许端口预览域名。公网入口须允许 TCP 80/443。动态 DNS 服务会公开目标 IP，适合临时验收；正式公网使用建议改成自有域名。临时域名不支持通配符证书。
3. 生成两份独立的 32 字节随机密钥到 `CH_DATA_KEY_FILE`、`CH_COOKIE_KEY_FILE`，权限 0600、属主为 Portal 专用用户。已有密钥保留，替换会使现有密文/Cookie 失效。
4. 在现有 `/etc/docker/daemon.json` 的 runsc runtime 中固定 `runtimeArgs` 为 `["--net-raw=false", "--allow-packet-socket-write=false"]`，保留其他 Docker 配置并 reload Docker。安装受保护的配置与网络检查脚本：

   ```sh
   sudo install -d -m 0755 /etc/cloudhost /etc/opensandbox /opt/cloudhost
   sudo install -o root -g root -m 0644 deploy/single/resolv.conf /etc/cloudhost/resolv.conf
   sudo install -o root -g root -m 0644 deploy/single/opensandbox.toml /etc/opensandbox/opensandbox.toml
   sudo install -o root -g root -m 0755 deploy/single/network-policy.sh /opt/cloudhost/network-policy.sh
   sudo /opt/cloudhost/network-policy.sh apply
   ```

   `apply` 只修改 `cloudhost` 专用表和网桥，不清空其他防火墙；固定子网 `172.30.0.0/24` 与现有网络冲突时失败。Sandbox 使用只读的公共 DNS 配置，避免 runsc 下 Docker 内嵌 DNS 的 NAT 路径；不开放宿主 DNS 或私网访问。安装后，systemd 会在 Docker 启动前 `preload` 规则，Docker 启动后再 `apply` 并核对真实网桥，最后启动 Portal。Portal 用户需要运行只读 check；sudoers 仅放行 `/usr/sbin/nft -s -nn list table inet cloudhost` 和 `/usr/sbin/nft -s -nn list table bridge cloudhost`，不得放行写入或 apply。用 `visudo -f /etc/sudoers.d/morde-network-check` 为现有服务账户写入并校验这一行（替换账户名）：

   ```text
   <已有的-Portal-服务账户> ALL=(root) NOPASSWD: /usr/sbin/nft -s -nn list table inet cloudhost, /usr/sbin/nft -s -nn list table bridge cloudhost
   ```
5. 在仓库根目录安装 Go 1.27.1 和 Docker Buildx。先构建本地 OpenSandbox 补丁镜像：`IMAGE_PLATFORM=linux/arm64 images/opensandbox-server/build.sh`（AMD64 使用 `linux/amd64`）；它基于固定的 release 1.1.1 digest，发布 Host Runtime 的 TLS 端口 8090，并让 endpoint API 返回该端口的直连地址（44772 代理只支持 HTTP）。再运行 `images/host/build.sh`，默认构建 linux/amd64；ARM 主机使用 `IMAGE_PLATFORM=linux/arm64 images/host/build.sh`。Go 版本由 `project/morde-runtime/go.mod` 的 `toolchain` 固定，脚本会本机交叉编译 runtime，再由 Docker 组装镜像。构建会输出 digest；将 `deploy/single/templates/std-2c4g.yaml` 的 `image` 更新为 `<tag>@<digest>` 后再创建 Host，模板入口必须保持 `/usr/local/bin/morde-host-entrypoint`。仓库模板固定到当前本机 linux/arm64 镜像 digest；该镜像尚未推送到任何 registry。可先运行 `images/host/test-runtime-bootstrap.sh` 做本地引导烟雾测试（该测试关闭浏览器，只验运行凭证文件和 AIO 服务启动）；它不能替代目标 Linux 的 runsc/GUI 验收。镜像供同机 OpenSandbox 使用；对 `project/nanoMuse/web`、`project/nanoMuse/harness/desktop`、`web/portal` 与 `web/workbench` 执行各自的依赖安装和构建。`web/portal` 的生产构建先生成用户 PC `/` 与后台 `/admin/`，再调用 Morde 手机 Web 构建并打包到同源 `/app/`（包括 GPL/NOTICE）。缺少 `project/nanoMuse/web` 源码或依赖时构建会失败，部署前须确保它是当前 Morde 修改版 nanoMuse 工作树。先在仓库根目录 `mkdir -p .local/bin`，再在 `server/` 显式按目标机架构构建 Linux Portal：`GOOS=linux GOARCH=amd64 go build -o ../.local/bin/portal ./cmd/portal`（x86_64），或将 `GOARCH=arm64` 用于 aarch64。安装脚本会在写入 systemd 单元前验证 ELF 格式与目标 CPU，拒绝 macOS Mach-O 或架构不匹配的二进制。

   安装前要求 `web/portal/dist/index.html`、`web/portal/dist/admin/index.html`、`web/portal/dist/app/index.html` 和 `web/workbench/dist/index.html` 均已生成；任一入口缺失时安装会失败。正式 PC 客户端基于 `project/nanoMuse/harness/desktop` 二开并复用唯一同步引擎 `project/morde-sync`；其签名、公证和下载发布不在本部署步骤内。

每个 Host 还会创建一个独立的 `runtime-<host_id>` Docker named volume，挂载到 `/var/lib/morde-runtime`，用于运行端状态；入口将其初始化为 `root:root`、`0700`。OpenSandbox 新建 Workspace 卷默认是 root-owned，Morde entrypoint 启动时将 Workspace 根初始化为 `root:1000`、`2770`（setgid），确保固定 AIO UID/GID 1000 可读写；运行端原子创建的文件以 `0660` 提交，用户组可继续编辑。删除 Host 或清理失败 Sandbox 时，先确认 Sandbox 已清理，再回收 runtime 卷；Workspace 是否保留不影响 runtime 卷回收。当前 OpenSandbox Docker 后端会忽略 PVC `storage` 请求，因此模板发出的 `1Gi` 只是请求值，不是硬配额；Docker named volume 的用量仍受宿主 Docker 数据盘容量影响。运行端 operation store 已在本地实现，但尚未随精确 Morde 镜像在目标机验收。
Host 入口会在 Morde Runtime 进程意外退出后采用 1–30 秒有界退避重启；AIO 和桌面进程继续运行，Host 健康检查在 Runtime readiness 恢复前保持失败。该机制只恢复 Runtime 控制面进程，不会恢复尚未登记的 PTY / GUI 会话，也不会把未知操作结果标成成功。

6. 在仓库根目录切换到 root shell，指定现有专用服务账户并启动基础服务。`cloudhost.env` 遵循 systemd 的 EnvironmentFile 语法；不要用 Bash `source` 执行它，尤其是多行 Caddy 配置。下面的读取器只提取单行路径和本地服务地址，不读取或打印密码、API Key：

   ```sh
   set -e
   PORTAL_USER='填写已有的 Portal 服务账户名'
   REPO_ROOT=$(pwd -P)
   CH_WORKSPACE_STORAGE_ROOT_DIR=$(python3 deploy/single/read-env-values.py /etc/cloudhost/cloudhost.env CH_WORKSPACE_STORAGE_ROOT_DIR)
   CH_SITE_STORAGE_ROOT_DIR=$(python3 deploy/single/read-env-values.py /etc/cloudhost/cloudhost.env CH_SITE_STORAGE_ROOT_DIR)
   CH_DOCKER_HOST=$(python3 deploy/single/read-env-values.py /etc/cloudhost/cloudhost.env CH_DOCKER_HOST)
   CH_OSB_URL=$(python3 deploy/single/read-env-values.py /etc/cloudhost/cloudhost.env CH_OSB_URL)
   CH_OSB_CONFIG_FILE=$(python3 deploy/single/read-env-values.py /etc/cloudhost/cloudhost.env CH_OSB_CONFIG_FILE)
   export CH_DOCKER_HOST CH_OSB_URL CH_OSB_CONFIG_FILE
   for dir in "$CH_WORKSPACE_STORAGE_ROOT_DIR" "$CH_SITE_STORAGE_ROOT_DIR"; do
     if [ ! -e "$dir" ] && [ ! -L "$dir" ]; then install -d -o "$PORTAL_USER" -g "$(id -gn "$PORTAL_USER")" -m 0700 "$dir"; fi
   done
   docker compose --env-file /etc/cloudhost/cloudhost.env -f deploy/single/docker-compose.yml up -d
   /opt/cloudhost/network-policy.sh check
   portal_once() {
     systemd-run --wait --collect --pipe -p "User=$PORTAL_USER" \
       -p "WorkingDirectory=$REPO_ROOT" \
       -p EnvironmentFile=/etc/cloudhost/cloudhost.env \
       "$REPO_ROOT/.local/bin/portal" "$@" --config "$REPO_ROOT/deploy/single/portal.yaml"
   }
   portal_once migrate up
   portal_once workspace verify
   deploy/single/install-portal-service.sh "$PORTAL_USER"
   ```

   空库首次启动会初始化超级管理员 `admin`，默认密码为 `admin`。首次修改密码前只通过本机回环地址或受信任的 SSH 本地隧道访问 Portal；不要在仍使用默认密码时开放公网入口。改密完成后再开放正常访问，后续重启不会恢复默认密码。普通用户统一从用户 Web 注册，初始状态为待审核；管理员通过审核后才能登录。管理员凭据文件同步和 `portal user add` 入口均已移除。新目录只在不存在时创建并明确归属于 Portal 账户；现有目录不自动改属主或递归改数据。迁移由同一个账户执行。已有服务账户应是非 root、禁止 shell 登录的专用账户；不要另建第二套数据目录。安装脚本要求 Workspace / Site 根目录为该账户所有且权限 0700，两份 32 字节密钥为该账户所有且权限 0600，并检查该账户可访问 Docker 和只读 nftables 检查。Docker socket/组权限等同 root，须限制该账户的其他用途。仓库放在 `/opt` 或 `/srv` 等非 home 目录，确保服务账户可读取仓库、前端构建物和 Portal 二进制；服务启用后不要移动仓库。安装脚本不复制代码或创建账户。真实密码不要放命令行或日志。

   安装脚本注册三个 systemd 单元和一个 Docker 服务依赖：Docker 启动前 root 预装载 `cloudhost` nftables 规则，Docker 启动后 root 重新核对网桥并应用相同规则，Portal 以指定账户启动并在异常退出后重启。人工重启 Docker 会按“预装载 → Docker → 后置应用 → Portal”顺序重启；安装脚本在 Docker 已运行时只重新装载规则，不为启用依赖而主动重启 Docker。`systemctl daemon-reload` 只更新单元定义，不清空已装载的 nftables 表。若预装载失败，Docker 不启动，也不会自动恢复旧 Host。数据库或 OpenSandbox 尚未就绪时 Portal 会重试启动；网络策略检查失败则拒绝服务。安装前先停止旧的手工 `portal serve` 进程，避免端口冲突；如果已有非本脚本管理的同名 systemd 单元或 Docker drop-in，脚本会拒绝覆盖，需先核对旧配置再迁移。使用 `systemctl status morde-portal.service` 和 `curl -fsS http://127.0.0.1:7080/readyz` 检查状态；配置或二进制更新后先运行迁移，再执行 `sudo systemctl restart morde-portal.service`。备份 / 恢复前使用 `sudo systemctl stop morde-portal.service` 并保持停止，结束后再启动。

   预装载无需 Docker 运行，按固定 `chbr0` 网桥名提前安装规则；目标 Linux 仍须做实际重启与阻断私网出站的验收，并确认其他防火墙管理器没有在 Docker 启动后清空 `cloudhost` 专用表。预装载失败会阻止整个 Docker 服务启动，连同该机的 PostgreSQL/OpenSandbox 容器也会受影响；先检查 `systemctl status morde-network-preload.service` 和日志，再修复根因。若既有 Docker `cloudhost` 网络被管理员改为其他网桥，预装载无法保护它；真实网桥核验失败会阻止 Portal，但必须先修正网络漂移再恢复 Host。

   `site_storage.root_dir` 与 Workspace、Docker 数据根及 Portal 静态目录必须分离，并由 Portal 服务账户私有持有。`CH_SITE_STORAGE_MAX_RELEASE_BYTES` 限制单个版本，`CH_SITE_STORAGE_MAX_OWNER_BYTES` 限制每位用户的累计发布物；未物化候选按固定 Workspace revision 字节数预留，物化产物持续计入。当前启动只初始化并验证不可变发布物存储；发布 API、执行沙箱与站点路由尚未开放。

   升级旧版本时，先停止旧 Portal 和旧 Host 的写入，再执行 `migrate up`。导入器只读取未被任何 Docker 容器引用的旧卷；需移除引用该卷的旧 Host 容器，同时保留 named volume 本身，并确认容器内没有需要保留的非卷数据。逐个运行 `.local/bin/portal workspace import-legacy --workspace <workspace-id> --config deploy/single/portal.yaml`。导入成功前不要删除旧卷；导入后再执行 `workspace verify`。Portal 会在仍有待导入旧卷时拒绝启动，并提示待处理 Workspace。全新安装没有待迁移的旧卷，可直接运行下面的 verify。

   `portal workspace verify` 会只读核对数据库所有历史 Workspace revision 引用的正文块，以及块目录里的哈希、大小和路径；发现缺块、损坏或未知残留时失败并保留原文件。它是完整性检查，不会创建备份，也不能替代数据库与正文块的一致备份 / 恢复演练。
7. 登录门户，创建主机并进入工作台，验证浏览器、终端和文件；在 API Key 页创建平台 Key，按 `examples/agent/codex/config.toml` 接入 MCP。预览为私有，用户先进入主机再打开预览链接。删除主机默认保留工作目录；自动空闲释放当前禁用，用户显式释放计算资源时保留 Host、Workspace 和 runtime 数据。

本机没有额外数据盘时，不要把同一根分区上的不同目录当作磁盘隔离或备份。达到停创阈值后，先迁移数据或清理经确认可删除的数据；不要对 Docker 执行自动 prune。正式多用户开放前还需要独立数据库/容器存储、可用的备份目标及恢复演练。

验证脚本在 `verify/`。只配置指向本机的测试服务，使用合成数据；测试输出保存在本地。runsc 不可用或真实 OpenSandbox 缺失时，应记录未完成，禁止将 mock UI 结果当作真实主机验收。首版没有暂停/唤醒、直连令牌、公开预览、VSCode、通知及一键安装器。

## 离线备份与恢复

`backup/backup.py` 备份 PostgreSQL 完整数据目录、`workspace_storage.root_dir` 不可变块和 `site_storage.root_dir` 发布物。它不会自动停止或启动服务。备份与恢复前，先通过实际使用的进程管理器停止 `portal serve`，再停止 Compose 中的 PostgreSQL，并在整个操作期间保持二者停止；脚本会检查 Portal/PostgreSQL 未运行和 PostgreSQL 已干净关闭，但无法阻止外部管理器重新启动它们。也要停止所有可能直接改写这三个数据目录的进程。例如在仓库目录中停止 PostgreSQL：

```sh
sudo docker compose --env-file /etc/cloudhost/cloudhost.env -f deploy/single/docker-compose.yml stop postgres
```

在 Linux 部署仓库目录执行。需要 root、GNU tar、Docker/Compose、util-linux `findmnt`，且三个数据路径和备份目标必须在脚本支持的本地文件系统。备份目标目录先由 root 创建为私有目录；从受信任的环境文件读取三个路径，只把路径参数交给 sudo：

```sh
set -e
sudo install -d -o root -g root -m 0700 /var/backups/morde
sudo install -d -o root -g root -m 0700 /var/backups/morde/.tmp
CH_POSTGRES_DATA_DIR=$(python3 deploy/single/read-env-values.py /etc/cloudhost/cloudhost.env CH_POSTGRES_DATA_DIR)
CH_WORKSPACE_STORAGE_ROOT_DIR=$(python3 deploy/single/read-env-values.py /etc/cloudhost/cloudhost.env CH_WORKSPACE_STORAGE_ROOT_DIR)
CH_SITE_STORAGE_ROOT_DIR=$(python3 deploy/single/read-env-values.py /etc/cloudhost/cloudhost.env CH_SITE_STORAGE_ROOT_DIR)
sudo env TMPDIR=/var/backups/morde/.tmp python3 deploy/single/backup/backup.py create \
  --postgres-data-dir "$CH_POSTGRES_DATA_DIR" \
  --workspace-root "$CH_WORKSPACE_STORAGE_ROOT_DIR" \
  --site-storage-root "$CH_SITE_STORAGE_ROOT_DIR" \
  --destination "/var/backups/morde/morde-$(date -u +%Y%m%dT%H%M%SZ)"
```

备份格式当前为 v2，只写到本机指定目录，不覆盖已存在的 bundle；目录权限为 `0700`，bundle 文件为 `0600`。它包含校验和、三个源路径、PostgreSQL 镜像 digest、Workspace 块统计和 Site release 统计。此前生成的 v1 bundle 不会被当前脚本验证或恢复；升级前请确认旧 bundle 是否仍需恢复，并在迁移前保留。备份目标需能容纳三份数据目录的完整离线副本及归档开销。完整性验证会在 `TMPDIR`（默认通常是 `/tmp`）解包一份临时副本；需为它准备足够空间。下例中的 `BUNDLE` 应替换为实际生成的 bundle 目录。验证会检查校验和、安全 tar 路径、Workspace 块哈希、Site 发布物结构和 PostgreSQL 控制文件，不连接或更改在线服务；需要本机已缓存 metadata 记录的精确 PostgreSQL 镜像。

```sh
BUNDLE=/var/backups/morde/morde-实际时间戳
sudo env TMPDIR=/var/backups/morde/.tmp python3 deploy/single/backup/backup.py verify --bundle "$BUNDLE"
```

恢复会替换三个数据目录。只在确认已经停止 Portal 和 PostgreSQL、并已验证 bundle 后执行；bundle 必须 root 持有且仅 root 可访问，路径必须与备份记录完全相同，PostgreSQL 镜像必须使用同一 digest。确保目标盘和 `TMPDIR` 的余量足以同时容纳当前数据、rollback bundle、临时验证副本和解包后的目标数据。脚本会在修改前先生成并验证一份当前数据的 rollback bundle，在同级临时目录解包并校验，再分别替换三个目录，并保留回滚 bundle：

```sh
set -e
CH_POSTGRES_DATA_DIR=$(python3 deploy/single/read-env-values.py /etc/cloudhost/cloudhost.env CH_POSTGRES_DATA_DIR)
CH_WORKSPACE_STORAGE_ROOT_DIR=$(python3 deploy/single/read-env-values.py /etc/cloudhost/cloudhost.env CH_WORKSPACE_STORAGE_ROOT_DIR)
CH_SITE_STORAGE_ROOT_DIR=$(python3 deploy/single/read-env-values.py /etc/cloudhost/cloudhost.env CH_SITE_STORAGE_ROOT_DIR)
BUNDLE=/var/backups/morde/morde-实际时间戳
sudo env TMPDIR=/var/backups/morde/.tmp python3 deploy/single/backup/backup.py restore \
  --postgres-data-dir "$CH_POSTGRES_DATA_DIR" \
  --workspace-root "$CH_WORKSPACE_STORAGE_ROOT_DIR" \
  --site-storage-root "$CH_SITE_STORAGE_ROOT_DIR" \
  --bundle "$BUNDLE" \
  --confirm-restore
```

除 `--confirm-restore` 外，还必须在交互终端准确输入脚本显示的三个目标路径。恢复完成前不要启动服务。若恢复进程在目录替换期间意外中断，保持服务停止，保留所有同级 `.pre-restore-*`、`.morde-restore-*` 目录及 rollback bundle，不要手动删除或改名；使用脚本输出的 rollback bundle 执行：

```sh
set -e
CH_POSTGRES_DATA_DIR=$(python3 deploy/single/read-env-values.py /etc/cloudhost/cloudhost.env CH_POSTGRES_DATA_DIR)
CH_WORKSPACE_STORAGE_ROOT_DIR=$(python3 deploy/single/read-env-values.py /etc/cloudhost/cloudhost.env CH_WORKSPACE_STORAGE_ROOT_DIR)
CH_SITE_STORAGE_ROOT_DIR=$(python3 deploy/single/read-env-values.py /etc/cloudhost/cloudhost.env CH_SITE_STORAGE_ROOT_DIR)
ROLLBACK_BUNDLE=/var/backups/morde/本次生成的回滚目录
sudo env TMPDIR=/var/backups/morde/.tmp python3 deploy/single/backup/backup.py recover \
  --postgres-data-dir "$CH_POSTGRES_DATA_DIR" \
  --workspace-root "$CH_WORKSPACE_STORAGE_ROOT_DIR" \
  --site-storage-root "$CH_SITE_STORAGE_ROOT_DIR" \
  --rollback-bundle "$ROLLBACK_BUNDLE" \
  --confirm-recover
```

还必须在交互终端准确输入脚本显示的三个目标路径，才能恢复保留的原始目录。脚本会在回滚重试时把三个目标目录与已校验的回滚归档逐文件比较；若无法证明目录状态，journal 会标记 `manual_recovery_required` 并拒绝继续自动替换。遇到该状态、目标和原始目录都缺失或任何校验错误时，保持服务停止、保留所有路径和回滚 bundle，交由人工检查。

该工具不备份 Portal 二进制/迁移文件、配置、数据加密密钥、Cookie 密钥、Docker/Caddy named volumes、Host/Sandbox 卷或镜像。它不是跨版本或跨路径迁移工具；恢复后应用代码、数据库 schema、密钥和部署配置必须仍兼容。它只支持同一 Linux 主机的原路径恢复，拒绝数据根目录/内部挂载点、符号链接及特殊文件；备份内容可能含用户数据，需放在受保护的本地介质上并自行管理介质保管。工具不加密、不上传，也不提供时间点恢复。目标环境尚未完成真实备份/恢复演练；当前验证仅使用合成数据，不能替代演练。
