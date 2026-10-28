# 本地云端 DSH 运行包

这里提供服务端无界面 DSH 镜像的可重复构建入口，以及隔离 Docker Engine 和 Guardian 的 systemd 模板。文件只在仓库本地；不含模型密钥、Portal 凭证或已发布镜像。

## 构建与本地检查

在 `project/nanoMuse/harness/morde-cloud-runtime/` 执行：

```sh
./build.sh
./smoke.sh
```

构建脚本先按目标平台校验本地缓存中的 Node 24 与 Debian Bookworm 固定 digest，再用 `package-lock.json` 和 `npm ci` 构建，构建期间运行 Node runtime 测试。Dockerfile 和构建脚本都引用不可变摘要；缺失、架构不匹配或 digest 不匹配就停止，不自动拉取可变版本。最终镜像不继承 Node 基础镜像额外的 `NODE_VERSION`、`YARN_VERSION` 环境变量；这是 Portal supervisor 的镜像合同要求。最终镜像只带 Node、运行所需依赖与入口，默认用户为 `65532:65532`，不声明卷。

构建输出中的 BuildKit manifest digest 同时会在当前 Docker Engine 上登记为本地 RepoDigest，Portal supervisor 可用 `morde-cloud-runtime@sha256:…` 形式按摘要校验镜像。这个 RepoDigest 只证明当前 Docker Engine 中的镜像内容；本地构建默认使用当前 Docker context。若 Portal 指向另一台或另一个专用 Docker Engine，必须在该 Engine 上构建镜像，或通过获准的本地交付流程导入后确认 `docker image inspect` 返回的 RepoDigest 与配置值完全相同。此目录不 push 镜像，也不假设导入会保留 RepoDigest。

## Linux 部署前置条件

以下 systemd 文件为 `.service.in` 模板，含 `@PORTAL_GROUP@` 占位符，**不可直接安装或启用**。先在目标 Linux 本地确认专用 Portal 用户和只包含该用户的专用组，再生成实际 unit 和 daemon 配置；不要把用户添加到系统通用 `docker` 组。Guardian 以 root 身份运行但使用该专用组创建 0660 Unix socket；签名私钥和租约状态始终由 root 独占读取。Guardian 与 Portal 不共享签名私钥。

安装该包还要求 `dsh-guardian` 接受 `--socket-mode=0660`。该 flag 由 F01 的服务端改动提供；若二进制不支持该 flag，不要启动 Guardian。Guardian 单元在 socket ACL、Portal 组和专用 Docker socket 三者一致核对前不得安装。

`morde-dsh-docker.service.in` 与 `morde-dsh-containerd.service.in` 使用独立 `data-root`、`exec-root`、containerd root/state 和 Unix socket，且 Docker daemon 禁用默认 bridge/IP 转发；不得复用 Portal 管理 Host 的默认 Docker/containerd 服务。应将 Docker daemon 配置模板中的 `@PORTAL_GROUP@` 替换为 Portal 专用组，并将 Docker socket 路径作为 `cloud_dsh.docker_host`。该 socket 等同于对专用 daemon 的完整管理权，只能授予 Portal 服务账户和 root Guardian。

`morde-dsh-guardian.service.in` 要求 cgroup v2、rootful Docker Engine API 与可读/可写的 host cgroup；Guardian 只保留读取其他进程身份与写目标 cgroup kill 文件所需能力。它把持久租约放在 `/var/lib/morde/dsh-guardian`，socket 放在 `/run/morde/dsh-guardian.sock`。构建 Guardian 二进制后，私钥/公钥需放在 `/etc/morde/dsh-guardian/`；Guardian 私钥 root:root、0600，Portal 只读取公钥。可在目标机受保护目录中运行 `cd server && go run ../deploy/cloud-dsh/generate-attestation-key.go /etc/morde/dsh-guardian/attestation.key /etc/morde/dsh-guardian/attestation.pub` 一次性生成 raw Ed25519 文件；工具拒绝覆盖既有密钥，密钥轮换需要明确的运维计划。

本包不创建用户/组、不修改 Portal unit、不自动启用服务、不上传文件、不 push 镜像，也不部署到远程主机。安装前必须在目标 Linux 上确认运行架构、Docker/cgroup 行为、Guardian socket 权限、到期销毁和重启恢复；这些条件未验证前，`cloud_dsh.enabled` 必须保持 `false`。
