# DeepSeek 模型接入与对话验证记录

日期：2026-11-17

## 模型与网关

- 仓库已有 `deepseek-flash` 个人模型预设（`https://api.deepseek.com`），服务端出站域名允许列表也已包含 `api.deepseek.com`，因此无需重复添加配置。
- 通过 Morde 的 `ModelGatewayService` 使用真实 DeepSeek API 完成五种对话检查：中文事实问答、编号格式约束、多轮上下文记忆、流式回复、工具调用后继续回复。五项均通过。
- API 密钥只在测试进程环境和 PC 密码输入框中临时使用，没有写入仓库文件。

## 缺陷与处理

- PC 端保存个人模型时，首次收到 HTTP 503（通用 `internal`）；打包更新后的 PC 客户端复测收到 `invalid_argument`，字段为 `base_url`，提示服务域名未获允许。重试后刷新列表仍无模型记录，因此没有完成 PC 端保存及实际对话验收。
- 失败响应可能发生在提交前，也可能发生在服务端已处理请求后；原客户端未刷新列表，容易让用户误判结果并重复提交。`Connections.tsx` 现会在网络错误和 5xx 后刷新模型列表，并提示先核对保存结果；`Connections.test.tsx` 增加“服务端已写入但返回 503”的回归用例，验证模型会从刷新后的列表显示且密钥不会重现。
- 根因已定位到当前连接的 Portal 拒绝 `api.deepseek.com`：PC 的实际保存响应指出 `base_url` 不获允许。仓库的模型预设和 `deploy/single/portal.yaml` 均已配置该域名，说明运行中服务的允许列表与当前仓库配置不一致。当前本机没有运行中的 Portal 服务（仅发现本地 PostgreSQL）；`limactl` 也报告本地 Morde VM 不存在，项目启动脚本明确拒绝创建空白安装。无法在本机修复或重启该运行环境；初次 503 的具体服务端日志也不可读。

## 验证结果

- 设置 `MORDE_TEST_DEEPSEEK_KEY` 后运行 `go test ./internal/app -run TestDeepSeekFlashDialogueThroughMordeModelGateway -count=1 -v`：真实模型网关五项通过。
- `npm exec -- vitest run src/routes/Connections.test.tsx`（`web/portal`）：30 项通过。
- PC 端实测：更新后的真实客户端显示允许列表错误、清空密钥并刷新列表；模型没有保存，故没有宣称 PC 对话验收通过。Portal 管理员需将 `api.deepseek.com` 加入运行环境的模型允许列表并重启服务，然后在 Morde PC 中保存模型并发送一条实际对话复验。

## 2026-11-18 测试主机复核

- 通过隔离的 `morde-pc` 配置连接测试主机 HTTPS 服务；`/api/capabilities` 返回 HTTP 200，客户端已登录并打开 DeepSeek Flash 添加表单。
- PC 保存时服务端返回 `invalid_argument`（字段 `base_url`）。PC 提示服务地址无效或未获服务端允许，并清空了输入的密钥；个人模型没有保存，因此没有完成 PC 实际对话验收。
- 根因是测试主机运行配置与仓库配置不一致。按用户请求，已备份远端配置到 `/opt/morde/deploy/single/portal.yaml.bak-20261118-model-hosts`，并把 `models.allowed_hosts` 更新为 `api.openai.com`、`api.deepseek.com`、`api.z.ai`，覆盖当前 PC 内置的 OpenAI、DeepSeek、智谱模型预设。
- `morde-portal.service` 重启成功，状态为 active；主机回环地址和外部 HTTPS 的 `/api/capabilities` 均返回 HTTP 200，确认 Portal 已恢复。配置只允许列出的精确域名，未开放任意主机名；服务器仍保留 HTTPS 和公网地址校验。
- 当前只验证了配置加载及服务恢复，尚未在 PC 重新输入已清空的密钥、保存模型并发送实际对话。

## 2026-11-18 PC 无回复排查

- PC 中的“你好”已显示为服务端收到，但助手未回复；“任务动态”显示此会话没有任务。Composer 已选择 DeepSeek Flash。
- 测试主机 `/api/capabilities` 未返回 `cloud_tasks` 或 `conversation_auto_routing_v1`。服务端仅在自动路由 Worker、Cloud DSH Task Worker 和执行控制服务都已装配时才声明自动路由能力；代码只在 `cloud_dsh.enabled` 时创建这些 Worker。
- 测试主机部署配置没有 `cloud_dsh` 配置段，且运行服务列表中没有 DSH Guardian 服务。当前 Portal 可以接收并保存用户消息、配置模型，但没有 Agent Runtime 消费消息并生成回复，因此 DeepSeek 没有收到这次对话请求。
- 右侧 `morde:portal-workbench-open` 错误属于云端电脑面板连接失败，与本次纯文本消息没有回复是两条独立问题。要启用 AI 自动回复，需先按部署要求安装独立 DSH Guardian 和隔离 Docker Daemon，再配置并启用 `cloud_dsh`，重启后确认 `conversation_auto_routing_v1` 出现在能力列表中。

## 2026-11-18 根因修复与 PC 实聊复验

- 后续在测试主机安装并启用了 DSH/Guardian 后，`/api/capabilities` 已返回 `conversation_auto_routing_v1` 和 `cloud_tasks`。PC 仍有新消息在模型调用前失败，因此前一节“尚未部署 Agent Runtime”的判断只对应当时状态，不是最终根因。
- 服务端诊断日志和数据库状态确认：测试账号的 `owner_execution_controls.state` 为 `disabled`。DSH 对话路由无条件启动账号 Runtime，而 Runtime 启动要求该执行控制启用，导致纯文字消息在模型调用前以 `account Agent Runtime start was not confirmed` 失败。
- 修复只对明确的“账号执行控制已关闭”冲突使用现有无工具模型网关完成普通回复/澄清；若路由判断需要创建任务或调用工具，则返回执行权限未开启的提示。其他 Runtime 错误仍原样失败，未更改账号执行权限，也未绕过工具授权。
- 修复已部署到测试主机，Portal 重启后状态为 `active`；临时诊断日志已从服务端源码撤回。`go test ./internal/worker` 通过，Portal 二进制构建成功。
- 在真实 `morde-pc` 的 DeepSeek Flash 会话中，实测简单回声（`received C`）、算术题（`42`）、中文指定回复（`中文连通正常`）以及两轮记忆（保存并准确回忆 `blue-kite-47`），均收到真实模型回复。
- 右侧云端电脑面板不可用仍是独立问题；本次验证的是不依赖云端电脑的纯文字对话，未验收云端电脑连接或任务/工具执行。
