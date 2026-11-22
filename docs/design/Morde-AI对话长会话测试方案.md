# Morde AI 对话长会话测试方案

> 范围：验证复杂任务、长历史、连续追问和 DSH 压缩/恢复。原始消息仍以服务端会话记录为准；模型只接收受上下文预算约束的摘要、近期历史和当前输入。本文是可执行验收方案，不表示所有客户端或真实模型场景已经通过。

## 验收规则

- 已受理的消息必须留在对应会话，压缩只改变模型上下文投影，不删除原始消息、任务记录或检查点。
- 上下文装配按所选模型容量和输出预留计量；旧历史可裁剪或摘要，必须显式标记覆盖不完整，并保留可追溯的原文引用。
- DSH 压缩须保留完整工具调用/结果组、最新用户输入和可继续对话的助手状态。摘要失败、溢出或取消时不得提交半成品。
- 长任务期间可以接收追问。相同消息重试不得重复执行，停止一个任务不得连带停止独立任务。
- 未知模型容量按最小支持窗口做预检。单条输入本身超过所选模型预算时应明确失败并保留原文，不能静默截断后假装完整处理。
- 单条超过路由预算的消息使用有界原文引用和授权分段读取；分段偏移连续且 SHA-256 一致后才可报告完整处理。8 KiB、16 KiB、200 KiB 用例表示 UTF-8 字节数，不代表 Token 数。

## 测试用例

| ID | 场景与步骤 | 通过条件 | 自动化位置 |
|---|---|---|---|
| LC-01 | 在同一会话发送约 18 KiB 的长要求，随后发送“请用英语回答”；前序消息 ID 指向长要求。另分别构造 8 KiB+1、16 KiB+1、200 KiB+1 字节的单条当前消息，以 4K 未知容量测试有界路由预览和 Task 来源指针；大于 32 KiB UTF-8 的客户端输入走可恢复分片上传。用 200 KiB+1 字节混合 ASCII/UTF-8 内容逐段读取，覆盖开头、中间、结尾锚点。再构造短追问引用一个目标很长的运行中 Task、以及引用同作用域前序长请求的路径。 | 路由请求在 4K 预算内，只含有界首尾预览、消息 ID、原文长度和 SHA-256；Task 目标不携带完整超长原文。分片上传后，原文只返回给当前 Task，每段最多 8 KiB，字符偏移连续、不漏不重、UTF-8 合法，所有片段 SHA-256 相同，拼接后与持久原文逐字节一致。未读完不得声称已处理；模型容量未知时不因源消息超过 128 KiB 协议载荷上限而提前拒绝。另保留 18 KiB 历史追问与长 Task/前序裁剪断言。 | `web/portal/src/app/message-upload.test.ts`（200 KiB+1 UTF-8 分片、断线续传）；`server/internal/app/conversation_route_context_budget_test.go`（`TestConversationRouteModelRequestProjectsLargeCurrentMessages`、`TestConversationRouteTaskObjectiveReferencesLargeSource`）；`server/internal/worker/conversation_route_context_budget_test.go`（`TestConversationRouteWorkerPreviewsCurrentMessageAtMinimumWindowWhenCapacityIsUnknown`）；`server/internal/app/cloud_dsh_history_tools_test.go`（`TestCloudDSHCurrentMessageReaderReconstructsMoreThan200KiBExactly`）；PostgreSQL upload/source-reader integration；以及既有 route H01 测试。 |
| LC-02 | 以 4K token 合成窗口连续发送 40 条较长追问，使 DSH 至少压缩两次；最早要求设置唯一锚点，末条追问设置另一锚点。 | 所有 40 条用户消息各持久化一次；压缩后模型仍看到早期要求和最新追问；任务完成，无 Runtime 错误，检查点中仍可查原始消息。 | `runtime/morde-cloud-runtime/test/hermes-compaction-runtime.test.mjs`。 |
| LC-03 | 发起多步骤工具任务；工具执行期间补充同任务要求，同时创建独立任务；在检查点确认后重启 Runtime。 | 追问进入正确会话/任务；已确认的工具调用不重放；两个任务各自恢复并完成，进度和结果不串线；停止一个任务不停止另一个。 | Runtime 投递/恢复用例；`server/internal/worker/conversation_context_dsh_e2e_integration_test.go`。 |
| LC-04 | 分别注入空摘要、截断摘要、摘要模型失败、Provider 上下文溢出及用户取消。对失败案例恢复 Runtime 并重试一次。 | 无效摘要不替换活动历史；原始事件和失败记录保留；溢出仅按 DSH 有界恢复策略重试；取消不提交摘要；重启不会重复已确认调用或重复结算。 | `runtime/morde-cloud-runtime/test/compaction-recovery.test.mjs`。 |
| LC-05 | 建立两个会话及两个账号，分别写入相同格式但不同内容的早期约束；多轮压缩后交叉追问。 | 回答、摘要、历史搜索和检查点只引用当前用户获授权的会话；不同会话/账号内容不串用。 | PostgreSQL 会话权限、摘要来源和历史过滤集成测试。 |
| LC-06 | 在实际 `morde-pc` 与 iOS 客户端分别继续同一长会话；执行中关闭/重连客户端，再查看消息、进度和最终结果。 | 两端看到同一会话的持久状态；重连不新建重复任务；早期记录可查看；界面如实显示处理中、完成、失败或等待。 | 真实 PC/iOS 客户端验收；不得以独立 Web 页、构建或单元测试替代 PC 验收。 |

## 执行顺序

1. 先跑确定性单元与 Runtime 用例，覆盖预算、选窗、工具结果配对、重复压缩、检查点和失败恢复。
2. 用隔离 PostgreSQL 跑服务端路由与长历史重启 E2E，核对完整正文、摘要来源范围、幂等和权限。
3. 最后在实际 PC/iOS 客户端及明确配置的测试模型上验收端到端对话。真实模型用例只使用合成内容，不读取或上传项目文件。

常用本地命令：

```sh
cd runtime/morde-cloud-runtime && npm test
cd server && go test ./internal/app ./internal/worker -count=1
```

PostgreSQL E2E 需设置本地隔离测试 DSN `CH_TEST_DATABASE_URL`，并使用 `go test ./internal/worker -run '^TestConversationSummaryDSHMainAndSideChatsContinueAfterLongHistoryRestart$' -count=1`；不要连接生产数据库。

## 已知边界

长会话的目标是持续推进、保留原文并按需摘要/检索，不承诺把无限历史一次性塞入模型。当前会话原文最大 8 MiB；路由只发送不超过 2 KiB 的首尾预览和来源指针，DSH 按授权逐段读取，每段原文不超过 8 KiB 并核对 SHA-256 与字符偏移。模型单次上下文、Task Token 预算或工具调用预算不足时仍须失败并保留原文，不能伪称已完整处理；8 KiB/16 KiB/200 KiB 单元测试按字节计量，不替代真实 `>8,192` Token 验收。DSH 压缩无法缩短系统提示、工具定义或会话固定前缀；见 [DSH compaction-basic 限制](https://github.com/deepseek-ai/deepseek-harness/blob/master/packages/compaction/compaction-basic/README.md)。真实 Provider 行为和 PC/iOS 客户端闭环必须单独验收，合成模型通过不能替代这些证据。
