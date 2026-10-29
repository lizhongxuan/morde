# 本地云端 DSH 运行包

这里提供服务端无界面 DSH 镜像的可重复构建入口，以及隔离 Docker Engine 和 Guardian 的 systemd 模板。文件只在仓库本地；不含模型密钥、Portal 凭证或已发布镜像。

## 构建与本地检查

在 `project/nanoMuse/harness/morde-cloud-runtime/` 执行：

```sh
./build.sh
./smoke.sh
```

在可访问宿主 `/proc`、cgroup v2 和 Docker Unix socket 的隔离 Linux 环境，可额外运行真实容器/Guardian 用例。若测试 runner 本身位于容器中，须使用 host PID 与 cgroup namespace（`--pid=host --cgroupns=host`），并有权读取宿主 `/proc`、访问专用 Docker socket、写入目标 `cgroup.kill`。私有 cgroup namespace 可能把目标显示为 `/../<container-id>`；Guardian 会 fail-closed 拒绝该路径，不要对其做路径归一化。执行步骤：先将 `MORDE_REAL_DOCKER_RUNTIME_IMAGE` 设为本地固定摘要镜像，再从 `server/` 执行 `go test -race ./internal/worker -run '^TestRealDockerAccountRuntimeTaskIsolationAndGuardianCrashRestart$' -count=1`。如同时设置本地隔离 PostgreSQL 的 `CH_TEST_DATABASE_URL`，用例会验证 Go Manager→真实 Docker/Guardian→两个并行 Node Task：取消并停止 Task A 后，Task B 仍可推进到 `completed_candidate`，随后停止账号 Runtime；另用 `httptest` 调用 Portal `POST /api/tasks` 并运行真实 TaskWorker，确认 HTTP 请求上下文关闭后 hostless Task 仍完成、Guardian Task/Runtime 停止证明均入库并 ACK。此 Portal 子测试使用受控模型 Stub，不调用模型供应商。用例创建并清理随机身份的账号 Runtime 容器；只使用本机 Docker Engine，不传文件到远端。未设置镜像变量时跳过整个用例，未设置数据库变量时只跳过 PostgreSQL 子测试。

`MORDE_REAL_DOCKER_RUNTIME_IMAGE` 必须填写 `docker image inspect --format '{{index .RepoDigests 0}}' <本地镜像标签>` 返回的完整 RepoDigest，例如 `morde-cloud-runtime@sha256:…`；不要把标签拼在摘要引用中。Supervisor 会将配置值与 Docker 返回的 RepoDigests 精确比对。

构建脚本先按目标平台校验 Node 24 与 Debian Bookworm 固定 digest；本地缺少某个基础镜像时只按其不可变 digest 拉取，再核对架构和 digest，随后用 `package-lock.json` 与 `npm ci` 构建，并在构建期间运行 Node runtime 测试。它不会回退到或拉取可变版本标签。最终镜像不继承 Node 基础镜像额外的 `NODE_VERSION`、`YARN_VERSION` 环境变量；这是 Portal supervisor 的镜像合同要求。最终镜像只带 Node、运行所需依赖与入口，默认用户为 `65532:65532`，不声明卷。

构建输出中的 BuildKit manifest digest 同时会在当前 Docker Engine 上登记为本地 RepoDigest，Portal supervisor 可用 `morde-cloud-runtime@sha256:…` 形式按摘要校验镜像。这个 RepoDigest 只证明当前 Docker Engine 中的镜像内容；本地构建默认使用当前 Docker context。若 Portal 指向另一台或另一个专用 Docker Engine，必须在该 Engine 上构建镜像，或通过获准的本地交付流程导入后确认 `docker image inspect` 返回的 RepoDigest 与配置值完全相同。此目录不 push 镜像，也不假设导入会保留 RepoDigest。

## Linux 部署前置条件

以下 systemd 文件为 `.service.in` 模板，含 `@PORTAL_GROUP@` 占位符，**不可直接安装或启用**。先在目标 Linux 创建专用 Portal 用户和只包含该用户的专用组；不要把用户添加到系统通用 `docker` 组。可在本地已存在的输出目录运行 `deploy/cloud-dsh/render-units.sh "$PORTAL_USER" "$PORTAL_GROUP" /path/to/existing-output-directory`，一次渲染所有 `.service.in` 和 `.json.in` 文件。渲染前，脚本通过 `getent -s files` 只读检查本地用户和组数据库，并用 `id -u` 确认用户 UID 非 root；要求用户属于该组、组成员列表没有其他用户、没有其他本地用户使用该组作为主 GID，也没有其他本地组复用其 GID。它校验组名格式、拒绝 `root`/`docker` 和未替换占位符，拒绝覆盖目标文件；不创建用户或组、不安装或启用服务。可用 `python3 deploy/cloud-dsh/render-units_test.py` 在临时目录做本地检查，测试用临时 PATH 中的 `getent`/`id` 替身，不读取或修改系统账号。Guardian 以 root 身份运行但使用该专用组创建 0660 Unix socket；签名私钥和租约状态始终由 root 独占读取。Guardian 与 Portal 不共享签名私钥。

安装该包还要求 `dsh-guardian` 接受 `--socket-mode=0660`。该 flag 由 F01 的服务端改动提供；若二进制不支持该 flag，不要启动 Guardian。Guardian 单元在 socket ACL、Portal 组和专用 Docker socket 三者一致核对前不得安装。

渲染脚本不检查运行时 socket ACL、systemd 实际加载结果或 cgroup 行为。部署验收时在目标机用实际值只读核对：`getent -s files group "$PORTAL_GROUP"`、`getent -s files passwd "$PORTAL_USER"`，以及 `stat -c '%U:%G %a %n' /run/morde/dsh-guardian.sock /run/morde-dsh-docker/docker.sock` 和 `getfacl -cp` 查看这两个 socket。Guardian 与专用 Docker socket 应为 `root:<Portal专用组>`、0660；ACL 不得额外授予其他账户或组。不满足时停止服务并检查配置。

`morde-dsh-docker.service.in` 与 `morde-dsh-containerd.service.in` 使用独立 `data-root`、`exec-root`、containerd root/state 和 Unix socket，且 Docker daemon 禁用默认 bridge/IP 转发；不得复用 Portal 管理 Host 的默认 Docker/containerd 服务。两项 daemon unit 使用 `UMask=0022`，确保镜像层的目录权限（特别是容器根目录 0755）不被缩窄，否则默认 UID 65532 的 Runtime 无法读取其根文件系统。Guardian 保持独立的 `UMask=0077`。Portal、Guardian 与 Workspace Docker 客户端先读取 Engine `/version`，在 API v1.43–v1.44 中选择最高兼容版本；Ubuntu 26.04 Docker Engine 29.1.3 实测要求 v1.44，而 Docker 24 使用 v1.43。渲染出的 `dsh-docker-daemon.json` 应人工核对后安装为 `/etc/morde/dsh-docker/daemon.json`，并将 Docker socket 路径作为 `cloud_dsh.docker_host`。该 socket 等同于对专用 daemon 的完整管理权，只能授予 Portal 服务账户和 root Guardian。

`morde-dsh-guardian.service.in` 要求 cgroup v2、rootful Docker Engine API 与可读/可写的 host cgroup；Guardian 只保留读取其他进程身份与写目标 cgroup kill 文件所需能力。它把持久租约放在 `/var/lib/morde/dsh-guardian`，socket 放在 `/run/morde/dsh-guardian.sock`。构建 Guardian 二进制后，私钥/公钥需放在 `/etc/morde/dsh-guardian/`；Guardian 私钥 root:root、0600，Portal 只读取公钥。可在目标机受保护目录中运行 `cd server && go run ../deploy/cloud-dsh/generate-attestation-key.go /etc/morde/dsh-guardian/attestation.key /etc/morde/dsh-guardian/attestation.pub` 一次性生成 raw Ed25519 文件；工具拒绝覆盖既有密钥，密钥轮换需要明确的运维计划。

Guardian 异常退出后可能遗留 Unix socket 路径。重启只会在该路径仍为原 socket、连接明确被拒绝且文件身份未变化时清理它；有活动监听或路径变成普通文件时启动失败，交给管理员检查。

本包不创建用户/组、不修改 Portal unit、不自动启用服务、不上传文件、不 push 镜像，也不部署到远程主机。安装前必须在目标 Linux 上确认运行架构、Docker/cgroup 行为、Guardian socket 权限、到期销毁和重启恢复；这些条件未验证前，`cloud_dsh.enabled` 必须保持 `false`。
