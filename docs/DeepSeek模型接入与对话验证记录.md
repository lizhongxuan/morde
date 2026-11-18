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
- 只读检查运行主机的 `/opt/morde/deploy/single/portal.yaml`，`models.allowed_hosts` 仍只有 `api.openai.com`；本地仓库部署配置已包含 `api.deepseek.com`。运行中的 `morde-portal.service` 未加载本地仓库配置。
- 根因是测试主机运行配置与仓库配置不一致。未修改远端配置；待管理员将 `api.deepseek.com` 加入运行时允许列表并重启 Portal 后，在 PC 重新输入密钥、保存模型并发送实际对话复验。
