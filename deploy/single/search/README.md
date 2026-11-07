# 本机内置搜索

`scripts/setup-builtin-search.sh` 在本机安装锁定的 `mcp-searxng` 2.10.0 npm 依赖，幂等应用本地网页日期补丁，并启动固定 digest 的 SearXNG 容器；需要 Node.js 22+、npm、Docker Compose、OpenSSL 与网络。补丁只在已限长读取的 HTML 页面中提取格式严格的 `Published` / `Modified` 日期候选，不修改搜索和 URL/重定向/SSRF 校验。日期来自网页自报的机器可读元数据，模型仍须打开来源并核实；缺少可靠日期时不得声称新闻“最近”。脚本在 `.local/builtin-search/searxng.secret` 生成本机密钥，不打印密钥。SearXNG 只监听 `127.0.0.1:8189`，JSON 搜索由 `settings.yml` 开启。

在运行 Portal 的同一台机器上配置 `cloud_dsh.builtin_search.enabled: true`、`node_path` 为该机器上的 Node 绝对路径、`script_path` 为 `deploy/single/search/node_modules/mcp-searxng/dist/cli.js` 的绝对路径、`searxng_url: http://127.0.0.1:8189`、`max_concurrent: 4`。SearXNG 与 Portal 必须能在各自的网络命名空间访问同一个回环地址；macOS 虚拟机中的 Portal 需要在虚拟机内启动搜索服务或提供本机回环转发。然后重启 Portal 并在对话中使用最新消息搜索，无需给用户增加 MCP 配置或搜索 API Key。

每次工具调用以独立 STDIO 进程启动 MCP，且只暴露搜索和公网网页读取；进程环境不继承服务端密钥，进程缓存不会跨任务复用。SearXNG 容器关闭日志并将运行缓存放在临时目录，某些引擎仍会共享匿名搜索令牌；不要将私密资料或密钥放进搜索词。外部网页和搜索引擎仍需要网络，服务不可用或结果为空不能视为验证通过。mcp-searxng 为 MIT 许可，SearXNG 为 AGPL-3.0；分发时保留相应许可。

本地验证可先运行 `curl -fsSG --data-urlencode 'q=latest AI news' --data-urlencode 'format=json' http://127.0.0.1:8189/search`，再在 `server/` 运行 `CH_TEST_BUILTIN_SEARCH_SCRIPT="$(cd .. && pwd)/deploy/single/search/node_modules/mcp-searxng/dist/cli.js" go test ./internal/mcpconnector -run TestBuiltinSearchWithLocalSearxng -count=1`。测试只发送合成搜索词，不发送本地文件。

## 当前本地 Lima 虚拟机

当前 `morde-f01-u2604` 是 arm64 Ubuntu 26.04，软件源提供 Node.js 22.22.1；实际版本需在安装时再次确认。Portal 使用 `mordeportal` 账号，不能遍历 macOS 用户的 Desktop，因此其脚本应安装在 `mordeportal` 可读、由管理员管理的目录，如 `/opt/morde-search/node_modules/mcp-searxng/dist/cli.js`。在虚拟机安装 Node.js 22+ 和 npm 后，应将本目录的 `package.json`、`package-lock.json`、`apply-mcp-searxng-patch.mjs` 与 `patches/page-date.mjs` 放在 `/opt/morde-search`，执行 `npm ci --omit=dev --ignore-scripts --no-audit --no-fund` 和 `node /opt/morde-search/apply-mcp-searxng-patch.mjs`，并检查 Portal 账号对安装目录有读取和遍历权限；不要以 Portal 服务账号写入依赖。Morde patcher 会核对上游包名、版本和源码锚点，版本不匹配时拒绝修改。

本地补丁回归：在本目录运行 `npm run test:morde-patches`，仅使用离线 HTML fixture；它不访问网页或发送文件。

macOS 搜索容器的 8189 端口只绑定 macOS 回环地址，虚拟机的回环地址是独立的。可保留一个独立的本机 SSH 反向转发，将虚拟机 `127.0.0.1:8189` 转到 macOS `127.0.0.1:8189`：

```bash
ssh -F "$HOME/.lima/morde-f01-u2604/ssh.config" -o ControlPath=none \
  -o ExitOnForwardFailure=yes -N \
  -R 127.0.0.1:8189:127.0.0.1:8189 lima-morde-f01-u2604
```

该进程必须保持运行；重连后检查端口，并在虚拟机内请求 `http://127.0.0.1:8189/`。若端口已被其他进程占用，应先核对用途，不终止未知进程。确认转发和依赖后，虚拟机 Portal 配置使用 `/usr/bin/node`、`/opt/morde-search/node_modules/mcp-searxng/dist/cli.js` 和 `http://127.0.0.1:8189`，再由服务管理器完成更新。上述步骤仅为本机集成说明，未修改虚拟机已有进程或数据。

配置就绪后，`scripts/start-macos.sh` 会启动或复用本机搜索容器，并在已有的本地 Lima 启动环境下检查 DSH、Guardian 和 Portal 搜索配置，自动建立或复用回环转发；缺少依赖或检查失败时会提示原因。它不会替虚拟机安装 Node 或修改 Portal 配置，首次集成需要先完成上述准备。`--demo` 使用模拟数据，不启动搜索。生产示例配置默认仍保持关闭。
