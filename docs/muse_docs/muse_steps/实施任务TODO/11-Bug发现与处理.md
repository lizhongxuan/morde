# 11 Bug 发现与处理：实施任务 TODO

[返回总大纲](00-总大纲.md) · [用例 TC-11](../muse-harness-replication-test-cases.md)

## 定位

资料中的“发现—复现—验证根因—补回归—记录结果”是工程排障方法，没有足够证据证明它是 Muse 的内置产品机制。目标是让 01–10 的每个真实缺陷都能重现、修复并验证，不新增自动修 Bug 的产品能力。

## 接口与参考

- 本步骤不引入产品接口。相关改动应落在缺陷对应的现有模块及其调用方，影响消息、授权、任务状态或检查点时记录具体版本和操作顺序；日志需脱敏。
- **可适配测试方法：**[Hermes `test_corrections_are_generation_fenced_and_consumed_by_same_provider_loop`](../../../../project/hermes-agent/tests/gateway/test_session_busy_controls.py) 可参考用并发顺序和栅栏证明纠正被正确消费；按 Morde 测试工具重写，不复制单机 Runtime。
- **复用现有测试：**[TestConversationMessageReceiptIsAtomicAndScoped](../../../../server/internal/store/postgres/conversation_message_routing_integration_test.go) 可作为消息幂等与隔离测试入口，按实际缺陷补充相关用例。

## 任务

- [ ] **M11-01（部分完成：记录三条本地合成缺陷，实际 PC/DSH 现象待核对）发现并记录现象**：在对应步骤的任务文档记录版本、账号/会话/任务身份、环境、输入顺序、预期与实际结果；证据脱敏。验收：另一位开发者能确定受影响步骤与可复现入口，记录不包含凭据或外发内容。
- [ ] **M11-02（部分完成：三条本地合成复现已跑红，依赖 M11-01）建立最小复现**：使用本地合成数据、受控模型和工具重现问题，涉及并发时记录交错顺序及状态变化。验收：修复前最小用例能稳定展示故障；环境受限时写明未验证而非猜测。
- [ ] **M11-03（部分完成：本地 Broker 缺陷已修并新增回归，依赖 M11-02）确认根因并修复**：沿受影响的服务端、Runtime 与客户端调用链定位首次错误边界，在对应步骤任务内做最小完整修改。验收：原复现不再失败，同类调用者、隔离与幂等未被破坏。
- [ ] **M11-04（部分完成：定向回归通过，PC/DSH 待验，依赖 M11-03）回归与记录**：为有风险的逻辑补一条可维护的回归测试，执行相关自动测试及实际客户端验收；把结果、限制和任务状态写回对应步骤。验收：相关测试通过，未能实测的 PC/iOS 或外部环境明确标待验证。

## 验收标准

选取一个实际发现的缺陷，按四项完成可复现记录、根因、修复和回归；不得只贴模型推测或把未运行的测试写成通过。PC 功能须启动实际 `morde-pc` 验证，iOS 功能须在原生客户端验证。

## 本地缺陷记录（2026-11-24）

- **长消息转义膨胀：**源码审查发现，规范页按原文字节限制到 32 KiB，但工具结果再经 JSON 转义后可能超过 64 KiB。修复前，本地合成的 `<`、`&`、控制字符、反斜杠及 U+2028 五类页均在第一次读取时报结果超限；这不是已观察到的 PC 现场，也不能断言是历史 PC 中断的根因。现先校验完整规范页，再按可编码上限返回连续片段；记录每段范围并保留原页哈希。应用层回归覆盖原文重建、检查点重放、跳段/重复、损坏页及撤权。片段还要与真实模型预算和 DSH 持久恢复联调，见[步骤 10](10-上下文管理.md)。
- **同页检索遗漏未标记：**修复前，同一规范页内相距较远的多个匹配只返回一段，却返回 `partial=false`；英文多关键词、重复关键词和中文关键词的本地合成复现均失败。根因是旧逻辑只按匹配页数限制结果，没有检查摘录范围外的匹配。现对省略的匹配标 `excerpt_limit`，并在恢复时拒绝旧的错误完整性 checkpoint；页扫描完成不等于全文内容已交给模型。原 1 MiB 搜索与跨页引用回归通过。
- **执行范围与状态：**定向 `go test ./internal/app -run '^TestCloudDSHCurrentMessage(Reader|ReadDefinition|Search)' -count=1`、该组 `go test -race`、`go vet ./internal/app`、两处代码 `gofmt -d` / `goimports -d` 通过；`golangci-lint run ./internal/app --new-from-rev=HEAD --timeout=2m` 报 0 项，只覆盖应用包相对 HEAD 的新增问题，不等于全仓 lint。只使用本地合成数据。没有安全隔离的实际 PC + 本地 DSH/真实模型环境，因此没有实际用户界面或语义验收；M11-01–04 与 R14 不据此整体关闭。

## 评测汇总基础（2026-11-25）

- **实现：**新增 `server/internal/evaluation/summary.go` 的 `SummarizeRun`，只对完整且通过 `ValidateRun` 的 suite/run 生成描述性汇总：P0/P1 样本量、每项准则及标签分布、P0 hard-gate 失败、run provenance；N/A 与适用分母分开，未采集的耗时和实际成本明确为 `not_recorded`。不推导总分或通过阈值，不调用模型/Judge，也不接触真实用户对话。
- **验证：**`cd server && GOPROXY=off GOSUMDB=off go test ./internal/evaluation -count=1` 通过。测试覆盖 N/A 分母、标签计数、硬门禁、来源字段、输入不变，以及不完整/无效 run 返回零值报告。
- **边界：**这是 R14 的评测基础切片，不是一次缺陷复现/修复闭环，也不证明真实 DSH、实际 PC、10 条冒烟或 54 条体验场景通过；M11-01–04 与 R14 保持未完成。评分数据来源和人工/Judge 复核流程仍需后续按授权环境完成。

## Workspace 已写入但结果回执未知（2026-11-25）

- **可复现缺陷：**本地受控 Store 在 commit 已应用后丢弃响应，并令回执查询失败；live Workspace 另在原子文件替换后注入 adapter error，或在写后注入快照读取失败。旧路径会把部分情形报告为确定失败，Agent 可能按新调用重写。
- **修复与回归：**Hostless/live 写入现以持久回执和当前 live snapshot 对账；证据缺失或 pending 时停止并返回 unknown，同一 pinned call 恢复后已写内容不重复写入。确定的业务拒绝交回同一 Agent；模型意图仍由 Agent 判断，代码只执行身份、授权、revision、receipt 和副作用安全边界。定向 app 测试 7 个顶层/8 个子用例通过，详见步骤 09 的 S09-45。
- **范围：**这是本地合成故障复现，不是实际 PC/DSH 现场。M11-01–04 仍未完成，真实用户界面与 durable Agent-first 终态解释待验。

- **E03 失败测试先行门（2026-11-25）：**`Plan` 增加可选 `fix_plan` 字段（`server/internal/domain/task/plan.go`），有值即标记修 bug 任务；`morde_task_plan_update` 工具输入同步增加可选 `fix_plan`（changed_files/risks/alternatives，各 ≤8 条、各 ≤512 字符）。写操作门只落在 `CloudDSHToolBroker.execute()`（host `file_write`），与 E01 brief 门并排、先 brief 后失败测试；hostless 的 `morde_workspace_write` 豁免——该路径没有 shell 工具，失败测试永远跑不出来，设门会死锁全部 hostless 修 bug 任务，给 hostless 任务加跑测试的路是后续工作。失败测试记录是 shell 步骤的 `FailureReason="test_failed"`（`task_steps.failure_reason`，shell 跑测试且退出码非零或超时时由 `taskToolCallProgress` 打上），无记录的修 bug 任务写操作被拒并提示先跑红复现测试。只跑通过的测试、或只跑失败的非测试命令，均不放行；无 FixPlan 的任务不受影响。
- **E06 最小 diff 约束（2026-11-25）：**允许集 = 任务计划里 `FixPlan.ChangedFiles`（agent 在修复方案中声明的最小改动集；E02 影响面清单是分析参考，不由本门直接读取）。写时门两处：host `file_write` 在 `CloudDSHToolBroker.execute()`、hostless `morde_workspace_write` 在 `executeWorkspaceTool()`，均与 E01 brief 门并排，顺序为 brief → 失败测试（仅 host）→ 最小 diff；路径比对前规范化（Clean、去前导 `/`、统一分隔符），集外文件拒绝本次写调用并提示把文件加进 `fix_plan.changed_files` 再写、不问用户。`ChangedFiles` 为空时允许集为空、任何写都被拒；无 FixPlan 的任务不受影响。完成时复核在 `Complete()`：host 路用 `agentFileWriteEvidence()` 汇总的改动文件、hostless 路用已验证产物路径、checkpoint 不一致分支同样复核，任一越界文件即以 `minimal_diff_violation` 暂停完成、不进交付。门逻辑在 `server/internal/app/cloud_dsh_minimal_diff_gate.go`，测试为 `cloud_dsh_minimal_diff_test.go`（表驱动、内存 fake）。
- **E07 测试门（2026-11-25）：**只对带 FixPlan 的 host 修 bug 任务生效，无 FixPlan 的任务不受影响。两半缺一不可：新测试通过（E03 记录的失败测试命令在修复后重跑通过——同一工具调用参数、同一 request digest、更晚的 step 序号，另一条测试不能冒充）AND 全量套件通过（跑过一次无测试选择的测试命令且退出码为 0）。测试结果只来自真实 shell 运行的退出码/输出，不采信模型自报的 `TaskResultClaim`：成功测试在提交时打标（`test_passed` / 含全量命令的 `test_suite_passed`，与 E03 的 `test_failed` 共用 step 的 `FailureReason` 记录通道，store 的 `validTaskToolCallFailureEvidence` 白名单相应放行成功步骤的这两个值），门在 `Complete()` 里读 steps 上的记录。门顺序为 brief → 失败测试（host）→ 最小 diff → 测试门：`Complete()` 内最小 diff 复核之后、回归验证之前，两处落点（host 分支、checkpoint 不一致分支）共用 `pauseForTestGate`；任一半缺失即以 `test_gate_failed` 暂停完成（StatusWaitingUser，文案写明缺哪一半及下一步，不问用户），不进交付。hostless fix 任务豁免——没有 shell 工具、永远产生不了测试记录，设门会死锁（与 E03 同理）；**已知局限：hostless fix 任务无测试门，后续给 hostless 加测试运行器是 follow-up**，不要为此造假的测试通过记录。全量判定是启发式：无 `-run`/`-k`/`::`/单测文件等选择标记的测试命令才算全量（`go test ./...`、`pytest`、`npm test` 算；`go test -run TestX`、`pytest tests/test_x.py`、`go test ./pkg/foo` 不算）；`go test` 的包参数只认 `.` / `./...`，`cargo test foo`、`npm test -- auth` 等位置参数按选择处理。注意 `request digest` 是工具名 + 参数原文 JSON 的哈希：重跑命令改了写法（哪怕只是空格）即视为不同命令、门会拦下，需原样重跑。门逻辑在 `server/internal/app/cloud_dsh_test_gate.go`（全量判定复用 E03 的命令识别，`testCommandSegment` 重构成 `testCommandRunner` + `cleanTestCommandTokens` 供两处共用），打标在 `task_step_detail.go` 的 `taskToolCallProgress` 成功分支，测试为 `cloud_dsh_test_gate_test.go`（表驱动、内存 fake，含 Complete 级别的拦下/放行/hostless 豁免/checkpoint 不一致分支）与 store 白名单用例；E06 的两处 host Complete 测试补了绿色测试步骤，E03 的“通过测试不打标”旧用例按 E07 改为打标 `test_suite_passed`。
- **E04 根因验证（2026-11-25）：**`FixPlan` 增加可选 `root_cause`（`server/internal/domain/task/brief.go` 的 `RootCause`：结论 summary ≤1024 字符 + 复现输出 ≤4096 rune + 代码行 ≤8 条 `path:line`；三件缺一件即整条根因被拒，旧计划 JSON 仍可读、无迁移）。代码行只认工作区相对路径（绝对路径、`..` 逃逸、无行号、行号 0/非数字均拒）。`morde_task_plan_update` 工具 schema 与描述同步加 `root_cause`，`BriefFromPlan` 原样带进 brief。只做“结论带证据”的结构校验，不设“修 bug 必须先填根因”的门——E03 的失败测试门已卡住无复现的修改，根因字段是给结论配证据，不是第二道写操作门。摘要诊断见 10-上下文管理.md 的 E04。
