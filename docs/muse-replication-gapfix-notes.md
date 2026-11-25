# Morde 复刻 Muse：P0 缺口修复说明（release 基底 80ada17）

> **这份文档是给并行合代码的 Codex 看的。**
>
> - 本文档记录的每一处改动都在 `~/workspace/morde-release-fix` 工作区里、基于 release 基底
>   `80ada17`（服务器现跑的版本），**不是** sit 分支；不要把 sit 分支的代码合进来，也不要
>   把这里的改动往 sit 分支上搬。
> - 每处改动归属写明在各小节的「改了哪些文件」里：**只改列出的文件**。小节里标明的「边界
>   （Codex 不该碰的部分）」不要覆盖、不要顺手重构、不要改名、不要移动。
> - 行为有变但对应的 flow 文档（`docs/muse_docs/muse_steps/` 下的流程与 TODO 文档）还没同步
>   时，各小节会记一笔「flow-doc 待同步」——只记一笔，**不要直接改 flow 文档**。
> - 有待拍板的决定时，小节会写明「被拦住」的原因；没拍板前不要猜着实现。
> - 每项的验收标准以《增强方案》§二 P0 验收行与《核查报告》对应条目为准，设计方案 §16
>   的六条验收仍需真实任务端到端证据，本轮只做机制级修复，不宣称 §16 已通过。

## E01 Planner 职责（brief 四件套 + 修复方案结构化）

- 缺口：核查报告 §一 E01 行（部分通过：无 brief 不派发的写门缺失；“只在影响执行/安全/
  成本/不可逆时追问”是模型行为，无代码门）与 §三第 1–2 项的精神；增强方案 E01；设计方案
  补充文档 §2（Planner 仍产 brief 四件套，但合并判定不预设窗口；TODO 02 明确不预设独立
  Planner 文件、固定 60s 窗口、无验收标准时不一律追问）。本实现按补充文档口径：Planner
  是 Harness 内的一份职责，不建独立 Planner 文件、不预设合并窗口。
- 改了哪些文件：
  - `server/internal/domain/task/brief.go`（新建）：`Brief`（scope/acceptance/
    verification_method；目标在 `Plan.Goal`）、`FixProposal`（minimal_change/risks/
    alternatives）、`FailingTestRecord`、`ImpactMap`，以及 `Plan.IsComplex`（多于 1 步
    即复杂任务）、`Plan.MissingBriefFields`、`Plan.BlockingBriefGaps`。
  - `server/internal/domain/task/plan.go`：`Plan` 增加 `Brief/Fix/Impact/FailingTest`
    四个可选字段（`omitempty`，旧 plan 照常校验通过）；`Plan.Validate` 增加四者的结构
    校验（只校验格式，不校验完整度）。
  - `server/internal/domain/task/brief_test.go`（新建）：表驱动单测。
  - `server/internal/app/gapfix_gates.go`（新建）：`CheckBriefGate`。
  - `server/internal/app/gapfix_gates_test.go`（新建）：`CheckBriefGate` 单测（含反例）。
  - `server/internal/app/cloud_dsh_task_plan.go`：plan 工具入参增加 brief/fix/impact/
    failing_test 四个可选字段并记入 plan；提交时先过 `Plan.Validate` 再过
    `CheckBriefGate`；checkpoint/存量回执比对增加四字段的 `reflect.DeepEqual`；工具
    定义的 Parameters 与 Description 同步声明四字段与“复杂 plan 缺 brief 即拒绝”。
- 逻辑是什么：
  - 复杂任务（plan 步数 > 1）提交时必须带完整 brief（goal/scope/acceptance/
    verification_method 四项齐全），缺任一项即拒绝提交，错误里点名缺失字段；简单
    任务（1 步）不要求 brief。
  - “只在影响执行/安全/成本/不可逆时追问”的规则落成代码：`BlockingBriefGaps` 只在
    复杂任务上返回缺失字段——四个字段分别对应执行范围（scope）、验收/不可逆后果
    （acceptance）、执行与成本（verification_method）与目标（goal）；简单任务永不
    阻塞。拒绝只作为工具调用错误返回给 agent 的结构化信息，不产生用户可见提问，
    不调用 clarify。
  - brief/fix/impact/failing_test 随 plan 事件持久化（plan 事件 JSON），后续 E02/E03/
    E06/E07 的门直接读 `TaskDetails.Plan`，不另建表、不另建 store。
- 边界（Codex 不该碰的部分）：`task.Plan.Validate` 的旧字段规则一个字没动；plan 事件
  的 fingerprint/回执比对逻辑只追加了四字段的相等比较，没改原有比较；`executeTaskPlan`
  的授权、预算、revision 推进逻辑没动。不要把 brief 改成必填的独立 Planner 文件，不
  要给 plan 提交加固定时间窗口。
- flow-doc 待同步：无（行为变只在 plan 工具入参层面；流程文档的 plan 步骤语义未变）。
- 验证结果：`go build ./...` 通过；`go vet ./internal/domain/task/ ./internal/app/` 通过；
  `gofmt -l` 在改动文件上无输出；`go test -race` 指定测试（brief/plan/fix/failing/
  impact/CheckBriefGate/plan 工具定义）全部通过；`go test ./internal/domain/task/` 全过。
  `go test ./internal/app/` 全量有 4 个失败，全部是基线既有失败（与核查报告 §二所列
  af5bd5b 基线同一组：`TestAgentWorkspaceFilesAndHostUseTheSameDirectory`、
  `TestRuntimeToolExecutorVerifyAgainstWorkspaceFileHandler`、
  `TestApplyLiveProjectUploadMovesUnchangedRenamedFileInSharedDirectory`、
  `TestQuotaSettlePersistsOwnerStoragePauseBeforeAborting`，均为工作区路径/环境问题），
  与本次改动无关（无一处触及 plan/brief 代码路径）。

## E02 影响面分析（依赖图 + 调用者 + 覆盖测试自动映射）

- 缺口：核查报告 §一 E02 行（部分通过：影响面分析与回归对照的机制缺失；无真实任务
  证据）与 §三第 13 项（回归对照未接线——本轮 E07 接线，见 E07 小节）；增强方案 E02；
  设计方案 §16.7（失败工作项留痕、回归可追溯的精神）。
- 改了哪些文件：
  - `server/internal/domain/task/impact.go`（新建）：`DependencyGraph`（`AddFile`/
    `AddCoverage`/`AnalyzeImpact`/`ImpactOf`/`CoveringTestsOf`/`CheckImpactCoverage`）、
    `FilesOutsideImpact`、`ImpactMap.Normalized`。
  - `server/internal/domain/task/impact_test.go`（新建）：表驱动单测（含传递调用者、
    未知文件、覆盖对照、无覆盖文件必报未覆盖的反例）。
  - `server/internal/app/gapfix_gates.go`：增加 `UnrunCoveringTests`（回归对照：已记录
    的 covering tests 逐项核对跑没跑）与 `QueryTaskImpact`（只读影响面查询）。
  - `server/internal/app/gapfix_gates_test.go`：增加两者单测。
  - `server/internal/app/cloud_dsh_task_plan.go`：plan 提交时把已记录的 impact 归一化
    （排序 + 去重），使回归对照的比较确定。
- 逻辑是什么：
  - 依赖图是显式输入：文件 → 它 import 的文件，以及文件 → 覆盖它的测试。`AnalyzeImpact`
    对改动文件做传递闭包：调用者是所有（直接或间接）依赖改动文件的上游文件，覆盖测试
    是改动文件与调用者的覆盖测试并集；图里没有的文件仍列为改动文件，但调用者与覆盖
    测试为空——分析不假装知道图里没有的东西。
  - 回归对照分两层：图层面的 `CheckImpactCoverage`（逐改动文件：至少一个覆盖测试跑过
    才算覆盖；图里无覆盖记录的文件一律报未覆盖）；plan 层面的 `UnrunCoveringTests`
    （已记录的 covering tests 逐项核对，缺跑的按名报出）。plan 层面的对照由 E07 的
    测试门在完成入口调用（见 E07 小节）。
  - 只读查询 `QueryTaskImpact` 只读 plan 里已记录的 impact（归一化副本），不写、不重算；
    图分析本身不修改图，可反复只读查询。
- 边界（Codex 不该碰的部分）：依赖图的边是 agent 依据读代码的结果在 plan 里声明的，
  服务端不做源码扫描——不要为“自动”去加一个运行时扫仓的步骤（那是后续工作：从 `go list`
  输出建图，接口就是 `DependencyGraph`）；`ImpactMap` 的 JSON 形状不要改（已随 plan
  事件持久化）；`CheckImpactCoverage` 对无覆盖记录的文件报未覆盖是故意 fail-closed，
  不要改成“未知即通过”。
- flow-doc 待同步：无。
- 验证结果：`go build ./...` 通过；`go vet ./internal/domain/task/ ./internal/app/` 通过；
  `gofmt -l` 在改动文件上无输出；`go test -race ./internal/domain/task/` 指定测试
  （impact/brief/plan/fix/failing）全部通过；`go test ./internal/app/` 指定测试
  （CheckBriefGate/UnrunCoveringTests/QueryTaskImpact/plan 工具定义）全部通过。

## E03 失败测试先行门

- 缺口：核查报告 §一 E03 行（部分通过：写门拦得住，但 hostless 写豁免是设计内缺口，
  验收“无失败测试的修改被拦下”对 hostless 任务不成立）与 §三第 8 项；增强方案 E03。
- 改了哪些文件：
  - `server/internal/app/gapfix_gates.go`：增加 `CheckFailingTestGate`。
  - `server/internal/app/gapfix_gates_test.go`：增加 `TestCheckFailingTestGate`（含
    无失败测试真被拦、hostless 豁免、非 fix 任务放行的反例与正例）。
  - `server/internal/app/cloud_dsh_tool_broker.go`：host 工具派发前，`file_write` 调用
    先过 `CheckFailingTestGate(details.Plan, false)`。
  - `server/internal/app/cloud_dsh_workspace_tools.go`：`executeWorkspaceTool` 里
    workspace 写授权前先过 `CheckFailingTestGate(details.Plan, true)`。
- 逻辑是什么：
  - 门只约束已声明的 fix 任务：plan 里带了 fix proposal（结构化修复方案）才算 fix
    任务；没有 plan、或 plan 没有 fix proposal 的任务不约束（门无法判定一次没声明
    是修复的写入是在修哪个 bug）。
  - fix 任务在写代码前必须在 plan 里记下复现 bug 的失败测试（`FailingTestRecord`：
    测试名 + 失败输出摘要）；没记就拦下，错误直接返回给 agent 并点名缺失的记录，
    agent 补记后重试，不产生用户可见提问。
  - hostless 写豁免按设计保留（见下），不要“修掉”它。
- 边界（Codex 不该碰的部分）：
  - **已知缺口（等用户定）**：hostless workspace 写（`morde_workspace_write`）一律豁免，
    因为 hostless 任务没有 host 运行时可供跑复现。核查报告 §三第 8 项要求用户决定是否
    接受该豁免或给 hostless 任务补一道等价门；在用户定之前保持豁免，不要擅自收紧。
  - 门只认 `file_write`（host）与 `morde_workspace_write`（hostless）这两个写入口；
    `shell_exec` 也能改文件，但命令文本不作写分类——不要把 shell 命令解析成写操作
    （那是后续工作）。
  - 门的拒绝以工具调用错误直接返回给 agent，不经过 `failBeforeDispatch`（后者会把
    具体原因吞掉换成通用文案，agent 看不到缺什么）。
- flow-doc 待同步：无（门只加在写入口之前；流程文档的“复现 bug”步骤语义未变，
  只是现在有代码门）。
- 验证结果：`go build ./...` 通过；`go vet ./internal/app/` 通过；`gofmt -l` 在改动
  文件上无输出；`go test -race ./internal/app/` 指定测试（CheckFailingTestGate/
  CheckBriefGate/UnrunCoveringTests/QueryTaskImpact）全部通过。

## E06 最小 diff 约束

- 缺口：核查报告 §一 E06 行（通过（机制级）：host/workspace 两道写门 + Complete
  三重复核）；增强方案 E06。注意：核查报告的结论是机制级通过、无真实任务端到端证据；
  本轮在 release 基底上重建同等机制，§16 仍需真实任务证据（不在本轮范围）。
- 改了哪些文件：
  - `server/internal/app/gapfix_gates.go`：增加 `CheckMinimalDiffGate` 与
    `agentFileEvidencePaths`（完成时复核用的证据路径提取）。
  - `server/internal/app/gapfix_gates_test.go`：增加 `TestCheckMinimalDiffGate`（含
    超范围真被拦并点名文件的反例）与 `TestAgentFileEvidencePaths`。
  - `server/internal/app/cloud_dsh_tool_broker.go`：host 工具派发前，`file_write` 调用
    先过 `CheckMinimalDiffGate`（按 arguments 里的 path）；`Complete` 的 host 分支在
    验证步骤落盘后、真正核验前再复核一次证据文件清单，超范围即
    `pauseCompletion(..., "minimal_diff_violation", ...)`，不交付。
  - `server/internal/app/cloud_dsh_workspace_tools.go`：`executeWorkspaceTool` 里
    workspace 写授权前先过 `CheckMinimalDiffGate`（按 arguments 里的 path）。
- 逻辑是什么：
  - 门只约束记录了 impact map 的任务：写的文件必须在 impact 的 changed_files 或
    callers 里；超出的文件按名报出并拦下。没记 impact 的任务不约束（门无法判定一次
    没声明范围的写入是否越界）——范围声明来自 E01/E02 的 plan。
  - 两道写门（host `file_write`、hostless `morde_workspace_write`）在派发/授权前拦；
    完成时 `Complete` 按 `agentFileWriteEvidence` 的文件清单再复核一次，拦下漏网
    的写入（例如 plan 之前已记录的写入），以 `minimal_diff_violation` 暂停完成。
  - 门的拒绝以工具调用错误直接返回给 agent（点名越界文件），完成时复核以暂停完成
    处理；都不产生用户可见提问。禁止顺手重构就是这道门：不在影响面里的文件写不进去。
- 边界（Codex 不该碰的部分）：
  - impact 路径必须与写工具用的 workspace 相对路径是同一形式才比得上；不要擅自加
    前缀剥离或模糊匹配（见 E02 的说明）。
  - `shell_exec` 能改文件但不走这道门（见 E03 的边界）；不要把 shell 命令解析成写操作。
  - `Complete` 的复核只在 host 分支（有 `agentFileWriteEvidence` 的那条路）；hostless
    分支的写入已在写门拦过，不再复核。`pauseCompletion` 的 reason
    `minimal_diff_violation` 不要改名（回归对照靠它认）。
- flow-doc 待同步：无。
- 验证结果：`go build ./...` 通过；`go vet ./internal/app/` 通过；`gofmt -l` 在改动
  文件上无输出；`go test -race ./internal/app/` 指定测试（CheckMinimalDiffGate/
  AgentFileEvidencePaths/CheckFailingTestGate/CheckBriefGate/UnrunCoveringTests/
  QueryTaskImpact）全部通过。`Complete` 复核分支无 broker 级单测（需 store + runtime
  全套 fake，超出本轮范围），门函数本身与写门接线已覆盖。

## E07 测试门

- 缺口：核查报告 §一 E07 行（通过（机制级）：`Complete()` 调测试门，失败即停；无真实
  任务端到端证据）；增强方案 E07。注意：核查报告的结论同样是机制级通过、无真实任务
  端到端证据；本轮在 release 基底上重建同等机制，§16 仍需真实任务证据。
- 改了哪些文件：
  - `server/internal/app/gapfix_gates.go`：增加 `TestGateReport`/
    `TestGateDecision`/`EvaluateTestGate`（纯判定）、`RunShellTestCommand`（真实 shell
    退出码判定）、`TestReportFromInputs`（从完成证据里提取测试运行记录）、
    `testGateNewTestName`/`testGateCoveringTests`。
  - `server/internal/app/gapfix_gates_test.go`：增加 `TestEvaluateTestGate`（表驱动，
    含新测试未跑/失败、套件未跑/失败、covering test 未跑的反例）、
    `TestTestReportFromInputs`（含失败的新测试真判失败、单测运行不是套件运行、
    读不到退出码不算通过的反例）、`TestRunShellTestCommand`（`true`/`false` 真实
    退出码）。
  - `server/internal/app/cloud_dsh_tool_broker.go`：`Complete` 的 host 分支在 E06 复核
    之后调测试门；任一测试失败（或没跑）即 `pauseCompletion(..., "test_gate_pending", ...)`
    停在第 7 步，不进回归、不交付。
- 逻辑是什么：
  - 门只约束已声明的 fix 任务（plan 带 fix proposal）。判定是三项全过才放行：新测试
    （plan 里记的 failing test）跑过且通过；全部现有测试套件跑过且通过；impact 里记
    的 covering tests 全部跑过。任一项没跑或失败即拦下，reason 点名是哪一项。
  - 证据来自完成时已记录的工具调用：`shell_exec` 的命令文本判断跑的是新测试还是
    套件（带 `-run`/`-k` 的是单测，否则是套件），输出 JSON 里的 `exit_code` 是 host
    上真实跑出来的 shell 退出码；读不到退出码不算通过（fail-closed）。套件跑通过
    时，covering tests 全算跑过（套件跑了范围内的全部测试）。
  - `RunShellTestCommand` 是服务端自己跑测试命令时的退出码判定（`exec` 真实退出码，
    不解析输出）；目前供单测与后续的服务端测试运行使用，完成入口用的是已记录的
    host 退出码（服务端无法替 host 重跑测试）。
- 边界（Codex 不该碰的部分）：
  - 门只在 `Complete` 的 host 分支生效；hostless 分支不调门——hostless 任务没有运行时
    可供跑测试，调门会把 hostless fix 任务永久拦死。这是与 E03 hostless 豁免同类的
    已知缺口：hostless 任务的测试证据链路（谁跑测试、结果记在哪）还没设计，等用户定，
    不要擅自给 hostless 加一道永远过不了的门。
  - `test_gate_pending` 这个 waiting reason 不要改名（回归对照与客户端靠它认）。
  - 提取只认 `shell_exec` 的记录；测试输出的 `exit_code` 字段名不要改。
- flow-doc 待同步：流程文档第 7 步“跑测试”现在有代码门——只记一笔，**不要直接改
  flow 文档**（Codex 并行改代码中）。
- 验证结果：`go build ./...` 通过；`go vet ./internal/app/` 通过；`gofmt -l` 在改动
  文件上无输出；`go test -race ./internal/app/` 指定测试（EvaluateTestGate/
  TestReportFromInputs/RunShellTestCommand/CheckMinimalDiffGate/
  CheckFailingTestGate/CheckBriefGate）全部通过。`Complete` 门分支无 broker 级单测
  （需 store + runtime 全套 fake，超出本轮范围），门函数本身与提取逻辑已覆盖。

## E12 错误分类 + 重试梯子 + 终态

- 缺口：核查报告 §一 E12 行（部分通过：31 值分类表与梯子常量、路由 worker 已接
  `ModelRetryDelay`、任务级调用方已接；换方法→换 agent→终态全链路仍无真实任务实跑）
  与 §三第 6–7 项（任务级重试调用方、全链路实跑）；增强方案 E12；设计方案 §16.5
  （用户可见报错率 ≤1%：终态文案不带堆栈、不让用户自己修）。
- 改了哪些文件：
  - `server/internal/app/failover.go`（新建）：31 值 `FailoverReason` 表、
    `FailoverStrategyFor`（每原因的重试次数与耗尽后动作）、`ClassifyModelError`、
    `JitteredBackoff`（base 5s / max 120s，至多 25% jitter）、`DecideModelRetry`
    （Retry-After 优先，向上取整到秒）、`ApplyTaskModelFailureLadder`（换 agent 接手，
    深度默认 1、可配置）、`LadderExhaustedMessage`（终态文案，不带堆栈）。
  - `server/internal/app/failover_test.go`（新建）：31 值全覆盖、梯子边界、Retry-After
    优先、终态立即终态、换 agent 深度开关、终态文案无堆栈的表驱动单测。
  - `server/internal/app/failover_broker_test.go`（新建）：`PauseModelRecoveryAfterLadderExhaustion`
    的 broker 级单测（fake store：waiting reason、终态文案、耗尽类别落事件）。
  - `server/internal/app/agent_tools.go`：`AgentModelFailureDiagnostic` 增加
    `LadderReason`/`LadderAttempts`（耗尽记录的载体）。
  - `server/internal/app/cloud_dsh_tool_broker.go`：`safeAgentModelFailureDiagnostic`
    增加 `route_retry_ladder_exhausted` 类别与 ladder 字段校验；`pauseRecovery` 增加
    耗尽分支（waiting reason `model_retry_ladder_exhausted`，终态文案）；新增
    `PauseModelRecoveryAfterLadderExhaustion`（终态记录入口，复用现有 pause 落库，
    不建新表）。
  - `server/internal/worker/conversation_route_worker.go`：`conversationRouteRateLimitDelay`
    改走 `DecideModelRetry`（429 走梯子：3 次重试、base 5s/max 120s、Retry-After 优先、
    24h durable horizon 不变）；新增 `conversationRouteRetryLadderExhausted`，耗尽时记
    日志并落到启发式兜底（换方法），不让用户动手。
  - `server/internal/worker/cloud_dsh_executor.go`：`pauseCloudDSHModelRecovery` 增加
    终态分流——分类为不可重试原因时（`context_length_exceeded` 除外，它在 pauseRecovery
    里已有专用的“缩短后重发”文案）走 `PauseModelRecoveryAfterLadderExhaustion`；不实现
    该方法的工具保持原有 pause 路径。
  - `server/internal/worker/conversation_route_ladder_test.go`（新建）：路由 429 梯子
    单测（3 次重试的延迟档位、Retry-After 优先、耗尽不确认）。
- 逻辑是什么：
  - 分类：31 个原因覆盖限流/配额/超时/连接/网关/模型返回异常/上下文超限/内容过滤/
    认证/权限/请求无效/模型不存在/工具失败/预算/取消/流中断/用量未知/上游错误/未知。
    `ClassifyModelError` 按模型客户端错误 kind、domain 错误码与 provider reason、
    网络错误文本的顺序判定；认不出的归 unknown（重试 1 次后换方法，永不静默通过）。
  - 梯子：同调用最多重试 3 次（网络/服务端类；模型返回异常类 2 次；工具失败 1 次），
    延迟 base 5s 指数翻倍、上限 120s、至多 25% jitter；provider 的 Retry-After 优先
    （24h durable horizon 以上的仍按原有口径确认、无延迟重排）。
  - 换方法→换 agent→终态：重试耗尽后按策略走 switch_method（路由层是启发式兜底分类）
    或 switch_agent（任务层，深度默认 1、可配置；深度 0 时直接终态）；转完仍做不到
    进终态：任务暂停、进度保存、终态文案只说原因与重试次数、不带堆栈、不让用户动手。
  - 计数器复用已有持久化字段：路由层用 `RouteAttempt`（durable route claim 自带）；
    任务层耗尽记录复用现有 pause 落库（task 事件 + step，metadata 里带
    `failure_category: route_retry_ladder_exhausted`，可计数），不建新表。
- 边界（Codex 不该碰的部分）：
  - 31 个原因的表与策略不要改名、不要删项（终态计数与回归对照靠这些值认）。
  - `model_retry_ladder_exhausted` 这个 waiting reason 与 `route_retry_ladder_exhausted`
    类别不要改名。
  - `context_length_exceeded` 不走终态分流是故意的（它有专用的“缩短后重发”文案）。
  - 子 agent 深度默认 1 是待拍板项（补充设计 §4 #12）：`ModelRetryMaxAgentDepth` 与
    `ApplyTaskModelFailureLadder` 的 `maxAgentDepth` 参数就是配置点，用户定 2 层时只
    改这里，不要另起一套深度控制。
  - 全链路实跑（一次 429/网络错误注入走完重试→换方法→换 agent→终态）仍缺真实任务
    证据——本轮只做到机制级接线，见核查报告 §三第 7 项。
- flow-doc 待同步：hermes 19 步流程里“重试梯子/终态”两步现在有代码实现——只记一笔，
  **不要直接改 flow 文档**。
- 验证结果：`go build ./...` 通过；`go vet ./internal/app/ ./internal/worker/` 通过；
  `gofmt -l` 在改动文件上无输出；`go test -race ./internal/app/` 指定测试（31 值全覆盖/
  梯子边界/Retry-After 优先/终态/换 agent 深度/分类/终态文案/broker 终态记录）全部通过；
  `go test -race ./internal/worker/` 指定测试（路由 429 梯子档位/Retry-After 优先/耗尽）
  全部通过。执行器终态分流分支无 worker 级单测（需 executor 全套 fake），分类与终态
  记录本身已覆盖；全链路实跑仍需真实任务注入演练（核查报告 §三第 7 项）。

## E04 根因验证（复现输出 + 代码行 + 摘要校验按条件命名失败）

- 缺口：核查报告 §一 E04 行（部分通过：机制与 UT 过，无真实任务产出过根因，摘要
  仍 0 版本）与 §三第 9 项（E04 摘要 0 版本：诊断拆分只是代码，未部署）；增强方案
  E04；设计方案 §16.7（失败工作项留痕、回归可追溯的精神）与补充文档 §3（摘要验收
  口径：跨过触发线的会话实际产出版本，不是版本总数 ≠ 0）。本轮只做机制级修复，
  §16 仍需真实任务证据。
- 接手说明：本项由前一 agent 做到一半未提交后接手；接手时工作区有 8 个未提交文件。
  逐一 `git diff` 核对后确认：根因结构、根因门、摘要 6 条件拆分、worker 接线、单测
  的逻辑全部正确且与验收行一致，缺的只是 `conversation_summaries.go` 常量块一处
  gofmt 对齐（已手工修对齐，未跑 `gofmt -w`）。其余一行未改，补写本节说明后提交。
- 改了哪些文件：
  - `server/internal/domain/task/brief.go`：新增 `RootCause`（statement /
    reproduction_output / code_lines）与 `RootCauseCodeLine`（file / start_line /
    end_line / excerpt）；`RootCause.Validate` 与 `MissingEvidence`（缺失项按固定
    顺序点名，无效代码行按 `code_lines[i].excerpt` 点名）；`FixProposal` 增加
    `RootCause *RootCause`（`omitempty`，旧 plan 照常校验通过），`Validate` 追加
    根因格式校验。
  - `server/internal/domain/task/brief_test.go`：`TestRootCauseValidate`（含绝对
    路径、行号倒置、行距 >64、空 excerpt 的反例）、`TestRootCauseMissingEvidence`、
    `FixProposal` 带根因通过 / 带无效代码行失败的用例。
  - `server/internal/app/gapfix_gates.go`：新增 `CheckRootCauseGate`。
  - `server/internal/app/gapfix_gates_test.go`：新增 `TestCheckRootCauseGate`（无
    fix 任务放行、有证据通过、缺根因点名三项证据、缺复现输出只点名缺项、代码行无
    excerpt 点名行号的正反例）。
  - `server/internal/app/cloud_dsh_task_plan.go`：plan 工具定义增加 `fix.root_cause`
    结构（statement / reproduction_output 必填，code_lines 至少 1 条、至多 16 条，
    每条 file/start_line/end_line/excerpt 必填）与 Description 同步；提交时
    `Plan.Validate` 之后、`CheckBriefGate` 之后过 `CheckRootCauseGate`；checkpoint
    回放的 `reflect.DeepEqual(replay.Fix, plan.Fix)` 已覆盖根因，根因随 plan 事件
    持久化（plan 事件 JSON），点开 plan 就能看到证据。
  - `server/internal/app/conversation_summaries.go`：新增
    `CheckConversationSummaryRestore` 与 `ConversationSummaryRestoreFailure`：
    原先一个笼统的“cannot be restored”错误拆成 6 个按条件命名的失败
    （`open_failed` / `head_without_stored_version` / `head_version_invalid` /
    `identity_mismatch` / `version_mismatch` / `digest_mismatch`），每个失败带
    reason、stored/restored 字节数与至多 1024 runes 的摘要文本预览（不带密文、不
    带全文）；`ToError` 落成 `invalid_state` 的 domain 错误，details 里带
    condition/reason/字节数/预览。
  - `server/internal/app/conversation_summaries_test.go`：新增
    `TestCheckConversationSummaryRestoreNamesEachCondition`（6 条件逐一命名、字
    节数与预览上限断言）与 `TestCheckConversationSummaryRestorePreviewIsBounded`
    （2000 字文本预览正好截到 1024 runes）。
  - `server/internal/worker/conversation_summary_maintenance.go`：摘要恢复入口改
    走 `CheckConversationSummaryRestore`（原先的复合条件与“head has no stored
    version”两个笼统错误都换成按名失败）；分页时的输入变化检查也由一个笼统
    “input changed while paging”拆成 identity/head version/head digest/boundary
    四个按名错误。
- 逻辑是什么：
  - 根因结论必须附证据才算结论：fix 任务（plan 带 fix proposal）提交 plan 时，
    根因缺 statement、复现输出或代码行中任一项即被 `CheckRootCauseGate` 拒绝，
    错误里点名缺失的证据，agent 补齐后重提，不产生用户可见提问。非 fix 任务
    （plan 无 fix proposal）不受约束。格式上根因还受 `Plan.Validate` 约束：
    statement 与复现输出 1–2048 字、代码行 1–16 条、每条行距 ≤64 行、excerpt
    1–1024 字、文件路径为 workspace 相对路径（沿用 `validImpactPath`）。
  - 历史追溯靠摘要：摘要恢复失败不再只有一个笼统错误——6 个条件各有名字、原因、
    字节数与有界预览，任务因摘要失败停下时能从记录直接定位是哪一个条件。
- 边界（Codex 不该碰的部分）：
  - `CheckRootCauseGate` 只约束带 fix proposal 的 plan；不要把它扩大成所有 plan
    必带根因（简单任务与非修复任务不需要根因）。
  - 6 个摘要恢复条件名（`open_failed` 等）与 `ConversationSummaryRestorePreviewMaxRunes`
    = 1024 不要改名、不要改值（失败定位与回归对照靠它们认）。
  - 摘要 0 版本的部署后观察不在本项做：见下“部署后待做”。
- flow-doc 待同步：无（行为变只在 plan 工具入参与摘要恢复错误文案层面；流程文档
  的“定位根因”步骤语义未变，只是现在有代码门与可定位的失败）。
- 部署后待做（需部署 + 真实触发，本轮未做）：找一个跨过触发线（未覆盖源字节
  ≥72 KiB 或 token 压力 ≥ 预算 3/4）的会话，确认实际产出 ≥1 个摘要版本，并读日志
  定位曾失败的恢复条件；另需抽查真实任务产出的根因结论是否点开可见证据。
- 验证结果：`go build ./...` 通过；`go vet ./internal/domain/task/ ./internal/app/
  ./internal/worker/` 通过；`gofmt -l` 在改动文件上无输出；`go test -race
  ./internal/domain/task/` 全过；`go test -race ./internal/app/` 指定测试
  （CheckRootCauseGate / CheckConversationSummaryRestore / ConversationSummary /
  CheckBriefGate / UnrunCoveringTests / CheckFailingTestGate / CheckMinimalDiffGate /
  EvaluateTestGate / CloudDSHTaskPlan）全部通过；`go test -race ./internal/worker/`
  指定测试（ConversationSummary）全部通过。

## E05 审批卡本体字段（证据指针 / 本次成本 / 累计成本 / 触发规则 / 拒绝后果）

- 缺口：核查报告 §一 E05 行（部分通过：机制与 UT 过，无真实出卡抽查，成本常为
  unknown）与 §三（E05 无独立条目；披露层已搭一半，缺的是本体字段）；增强方案
  E05；设计方案 §2.7 第 4 条（PendingRequest 本体缺五字段；R06/T19 要求六字段证据
  卡：动作/对象/依据/单次与累计成本/规则/拒绝后果；可信成本数后端常为 unknown）
  与补充文档 §3（R22 已定走法 (a)：审批卡可出现在对话流中）。本轮只做本体字段，
  不动审批 UI 的位置/呈现；§16 仍需真实出卡抽查。
- 改了哪些文件：
  - `server/internal/domain/task/types.go`：`PendingRequest` 增加五字段：
    `EvidencePointer`（string）、`CurrentCost` / `TotalCost`
    （`*ApprovalCost`，nil 即 unknown）、`TriggerRule`、`RejectionConsequence`；
    新增 `ApprovalCost`（amount_micros / currency / basis，basis 为 estimate 或
    settled）与 `Validate`（nil 合法；金额 ≥0、货币为 3 位大写、basis 二选一），
    以及 `PendingRequest.MissingEvidenceFields`（固定顺序点名缺失项，无效成本按
    字段名上报）与 `EvidenceComplete`（nil 成本算齐全：unknown 是已携带的值，不
    是缺失项）。
  - `server/internal/domain/task/approval_test.go`（新建）：`TestApprovalCostValidate`
    与 `TestPendingRequestMissingEvidenceFields`（表驱动，含未知成本仍齐全、旧行
    缺三项文本字段的反例）。
  - `server/internal/app/agent_tools.go`：`AgentApprovalRequest` 增加同样五字段
    （JSON 键与本体一致；成本 nil 时序列化为 null，即明确的 unknown）；新增触发
    规则常量（`sensitive_host_tool` / `workspace_write` / `mcp_tool`）与三条拒绝
    后果文案常量（拒绝后本次操作不执行、任务暂停等下一步指示）。
  - `server/internal/app/task_execution_helpers.go`：`boundedApproval` 增加三项
    文本字段的界限校验（≤512 runes；成本有效性不在此校验，见下）；新增
    `approvalRequestFromPending`（从已存 pending 重建展示请求，五字段全带）与
    `withStepEvidencePointer`（无指针时以 `task_step:<step id>` 补齐）；
    `newPendingApproval` 改为建卡后过 `MissingEvidenceFields`，缺任一文本字段或
    成本无效即 fail-closed 拒绝并点名缺失字段。
  - `server/internal/app/cloud_dsh_workspace_tools.go`：`newHostlessWorkspaceApproval`
    同上（建卡后过 `MissingEvidenceFields`）；`cloudDSHWorkspaceWriteApproval`
    带上 `workspace_write` 规则与拒绝后果；新建卡前以当前 step 补证据指针；
    `pauseForExistingWorkspaceApproval` 的重暂停改走 `approvalRequestFromPending`。
  - `server/internal/app/cloud_dsh_tool_broker.go`：`pauseForApproval` 在建卡前进
    行 `withStepEvidencePointer`（证据指针统一为 `task_step:<step id>`）；重暂停
    的既有卡改走 `approvalRequestFromPending`。
  - `server/internal/app/runtime_tool_executor.go`：`runtimeToolApprovalRequest`
    拆为文本构造（`runtimeToolApprovalRequestText`，原函数体不动）加一层包装，
    统一带上 `sensitive_host_tool` 规则与拒绝后果。
  - `server/internal/app/cloud_dsh_mcp.go`：`cloudDSHMCPApprovalRequest` 带上
    `mcp_tool` 规则与拒绝后果；`pauseForExistingMCPApproval` 的重暂停改走
    `approvalRequestFromPending`。
  - `server/migrations/00238_task_pending_request_evidence_fields.sql`（新建）：
    9 列全可空（3 文本 + 每成本 3 列），CHECK 只约束已填值的界限（文本 1–512
    字、金额 ≥0、货币 `^[A-Z]{3}$`、basis 二选一）；Down 逐列删回。旧行五字段
    为 NULL，成本 NULL 即 unknown。
  - `server/internal/store/postgres/task_execution.go`：`CommitTaskProgress` 的
    INSERT 增加 9 列（空文本存 NULL、nil 成本三列全 NULL）；最新 pending 的
    SELECT 增加 9 列并回填；commit 校验只对新字段做“有值则合界限”的校验（见
    边界）；新增 `pendingApprovalCostColumns` / `scanPendingApprovalCost` /
    `nullablePendingText` / `validPendingOptionalText` /
    `validPendingApprovalCost`。
  - `server/internal/store/postgres/queries/tasks.sql` 与
    `server/internal/store/postgres/sqlcgen/tasks.sql.go` /
    `server/internal/store/postgres/sqlcgen/models.go`（`make generate` 重生成）：
    `ListTaskApprovals` 增加 9 列（文本 `COALESCE(...,'')`，成本保持可空类型）。
  - `server/internal/store/postgres/task_approvals.go`：`ListTaskApprovals` 映射
    增加五字段（成本经 `scanPendingApprovalCost` 还原，NULL 即 nil）。
  - `server/internal/api/tasks.go`：`taskApprovalSnapshotResponse` 与
    `taskPendingRequestResponse` 增加五字段（成本为 `*taskApprovalCostResponse`，
    nil 序列化为 null）；`toTaskApprovalResponse` 与 `toTaskResponse` 的 pending
    映射做同样校验与映射（新文本字段只校验界限，不强制必填，见边界）；新增
    `toTaskApprovalCostResponse` / `validTaskApprovalCost` /
    `validTaskOptionalText`。
  - `server/openapi/portal.yaml`：新增 `TaskApprovalCost` 组件（basis /
    amount_micros / currency）；`TaskPendingRequest` 与 `TaskApprovalSnapshot`
    增加五属性（成本为 `TaskApprovalCost` 或 null；三文本字段注明 00238 前的旧
    行为空）。required 列表未变（旧行无此五字段）。
  - 测试：`server/internal/app/task_execution_helpers_test.go`（新增
    `TestNewPendingApprovalRequiresEvidenceFields`，含缺每项的反例与已知成本通
    过的正例；`TestWithStepEvidencePointer`；`TestApprovalRequestFromPending`）；
    `server/internal/app/runtime_tool_executor_test.go`（新增
    `TestRuntimeToolApprovalRequestCarriesEvidenceFields`；既有 fixture 与过期
    审批测试按新门补证据字段）；`server/internal/app/cloud_dsh_mcp_approval_test.go`
    （新增 `TestCloudDSHMCPApprovalCarriesEvidenceFields`：broker 级真走一次外部
    调用审批，pending 五字段齐全、成本 nil）；`server/internal/app/cloud_dsh_workspace_tools_test.go`
    （新增 `TestCloudDSHWorkspaceWriteApprovalCarriesEvidenceFields`，同上）；
    `server/internal/api/tasks_test.go`（新增
    `TestTaskApprovalResponseCarriesEvidenceFields`：已知本次成本 + 未知累计成本
    的 JSON 形（`"total_cost":null`、无 `"amount_micros":0`）、负成本进 internal
    错误；新增 `TestTaskPendingRequestOpenAPIIncludesEvidenceFields`）。
- 逻辑是什么：
  - 敏感修改出卡时五字段齐全：host 敏感工具、hostless Workspace 写、外部 MCP
    工具三条建卡路径都在建卡时带上触发规则与拒绝后果，证据指针统一为
    `task_step:<step id>`（建卡的 pause 函数以当前 step 补齐；step 就是能点开
    看到工具调用与证据的工作项）。`newPendingApproval` /
    `newHostlessWorkspaceApproval` 在建卡后按 `MissingEvidenceFields` 逐项点名，
    缺任一文本字段或成本无效即拒绝建卡，不产生用户可见提问。
  - 成本未知显示 unknown，不伪造 0：三条路径在建卡时都没有可信成本数（成本账
    本 `model_call_ledger` 在调用结算后才有 token 数，建卡时不存在），所以成本
    一律留 nil；DB 里三列全 NULL，API 里序列化为 null，卡面按 unknown 显示。
    只有调用方真正拿到金额、货币与 basis 时才填 `ApprovalCost`（金额 ≥0）。
  - 重暂停不丢字段：三处从已存 pending 重建展示请求的点（MCP、Workspace、broker
    主路）改走 `approvalRequestFromPending`，五字段与首次出卡一致。
- 边界（Codex 不该碰的部分）：
  - 门只在两处建卡构造函数收紧（`newPendingApproval` /
    `newHostlessWorkspaceApproval`）。store 的 commit 校验与 API 的响应校验对
    新字段只做“有值则合界限”，不强制必填——00238 之前的旧行与媒体保存审批
    （其建卡方不在这两处构造函数里，见下）仍能正常提交与展示，不要擅自收紧。
  - 媒体保存审批卡（`MediaPreviewOperationID` 那路）不在本轮门的覆盖内：它的
    pending 不由上述两处构造函数创建，五字段目前为空；要覆盖需先定位其建卡方，
    不要猜着改。
  - 成本字段的形状（micros + 货币 + basis）沿用披露层 `TaskApprovalCostDisclosure`
    的既有词汇，不要另起一套单位（例如分、元字符串）；nil 即 unknown，不要把
    nil 改成 0 或把 unknown 写成金额。
  - 触发规则取值目前只有三项（`sensitive_host_tool` / `workspace_write` /
    `mcp_tool`）；新增规则时只在 `agent_tools.go` 的常量处加，不要在各处散写
    字符串。
  - R22 已定走法 (a)（审批卡可出现在对话流中），但本项只做本体字段：不要动审批
    UI 的位置/呈现，不要动 `Disclosures` 的生成逻辑（仍为 unknown）。
- flow-doc 待同步：无（行为变只在建卡字段与 API 响应层面；流程文档的审批步骤
  语义未变，只是卡面现在有本体字段）。
- 部署后待做（需部署 + 真实出卡，本轮未做）：在真实任务里抽查敏感修改出卡，
  确认五字段齐全、成本为 unknown 而非 0；另需确认媒体保存审批卡的建卡方与五
  字段覆盖。
- 验证结果：`go build ./...` 通过；`go vet ./internal/domain/task/ ./internal/app/
  ./internal/api/ ./internal/store/postgres/` 通过；`gofmt -l` 在改动文件上无输
  出（`task_model_responses.go` 的基线格式问题未动）；`make generate` 二次运行
  无差异；`go test -race ./internal/domain/task/` 全过；`go test -race
  ./internal/app/` 指定测试（NewPendingApproval / WithStepEvidencePointer /
  ApprovalRequestFromPending / RuntimeToolApprovalRequest / MCP 与 Workspace
  写审批带证据字段 / 既有 MCP 审批与 Workspace 写审批 / 过期审批）全部通过，
  全量与基线一致（仅 E01 记录的 4 个既有失败：`TestAgentWorkspaceFilesAndHostUseTheSameDirectory`、
  `TestRuntimeToolExecutorVerifyAgainstWorkspaceFileHandler`、
  `TestApplyLiveProjectUploadMovesUnchangedRenamedFileInSharedDirectory`、
  `TestQuotaSettlePersistsOwnerStoragePauseBeforeAborting`，均为工作区路径/环境
  问题，与本次改动无关）；`go test ./internal/api/` 全过；`go test
  ./internal/store/postgres/` 全过（PG 集成测试无 `CH_TEST_DATABASE_URL` 跳过，
  含 00238 的迁移未在真库上跑过）。

## E09 独立验证者最小可用（服务端确定性重推导 + 读回比较）

- 缺口：核查报告 §一 E09 行（部分通过：机制与 UT 过，无真实任务 verdict 分布）
  与 §三第 10 项（E09/E11 的集成测试无库跳过；“不再常 pending”需真实任务数
  据）；增强方案 E09；设计方案 §16.8（独立验证者：换一个 agent 重跑测试 + 读
  回比较，给 PASS/FAIL/pending；换模型家族只是建议，不强制）。基底根因：
  `loadTaskValidationVerdictFacts` 对有执行记录的任务把
  `IrreversibleAction` / `RepeatedError` 恒置 unknown，
  `ApprovalHistoryVerified` / `CorrectionRecordVerified` /
  `ReplayVerified` 恒为 false，于是 H05/H06/H07 永远 pending，verdict 随之常
  pending。
- 改了哪些文件：
  - `server/internal/app/task_validation_facts.go`（新建）：三个纯函数 + 一个读
    回比较函数，输入只来自已存记录，不读模型报告。
    - `DeriveTaskValidationApprovalFacts`：H05 输入。不可逆动作 = 有已批准的
      审批行（审批门就是不可逆/敏感动作的服务端记录）；审批历史可信 = 全部审
      批行已终态（approved/rejected/expired/cancelled）且每个已批准的动作在
      operation ledger 里有终态（succeeded/failed）操作（operation_key =
      `AgentOperationKey(call_id)`）。
    - `DeriveTaskValidationCorrectionFacts`：H06 输入。重复错误 = 同一 (tool,
      failure_reason) 失败两次（reason 为空时按 tool 计）；纠正可信 = 每个重
      复签名都有一次更晚的同 tool 成功步骤（重试链记录了纠正）。
    - `DeriveTaskValidationReplayVerified`：H07 输入。ledger 覆盖每个有 call
      key 的工具步骤（终态操作，或 rejected/expired/cancelled 的终态审批决
      定）且全部操作行已终态，执行才可从已存记录重放。
    - `VerifyTaskValidationVerdictReplay`：对同一 report + facts 重跑
      Evaluate + Summarize，与已存 verdict 比较。
  - `server/internal/store/postgres/task_validation_review.go`：
    `loadTaskValidationVerdictFacts` 在有执行记录时读
    `task_pending_requests` / `task_operations` / `task_steps`（新增
    `loadTaskValidationReplayRecords`，同一事务内），用上述纯函数推导
    H05/H06/H07 事实，取代硬编码 unknown/false；无执行记录时保持原样
    （Absent）。`CompleteTaskValidationReview` 在 UPDATE 之后读回 `state` /
    `validation_state` 并与重推导的 verdict 比较，不一致返回 conflict 错误
    （事务回滚，review 保持 processing 待下次重试），而不是上报一个库里没
    有的 verdict；`RowsAffected != 1` 时仍按原样返回未完成，不报错。
  - `server/internal/app/task_validation_facts_test.go`（新建）：三项推导函数
    的表驱动测试（含拦得住/判得出的反例：已批准无操作、待批未决、重复失败无
    纠正、仅早于失败的成功、操作未终态、被拒调用无操作）与
    `TestIndependentVerifierResolvesFixTaskVerdict`（修任务记录集 → PASS；缺
    终态操作或缺重放覆盖 → pending）。
  - `server/internal/store/postgres/task_validation_evidence_integration_test.go`
    （新增）：`TestPostgresTaskValidationVerdictFactsResolveFromStoredRecords`，
    在真库上插已批准审批 + 终态操作 + 重复失败与后续成功步骤，调
    `loadTaskValidationVerdictFacts` 断言六项事实全部可信/已出现，并断言由此
    推导的 verdict 为 PASS。无 `CH_TEST_DATABASE_URL` 时跳过（本轮未跑，见部
    署后待做）。
- 逻辑说明：verdict 事实的来源只有已存记录（审批行、步骤、操作账本），推导是
  纯函数，同一批记录永远得出同一 verdict；verdict 本身仍由既有
  `EvaluateTaskValidationReview` + `SummarizeTaskValidationEvaluations` 计
  算，七条硬检查的顺序与语义未变。没有新增迁移：基底没有卡住 verdict 的
  test-stamp CHECK 约束——唯一与 verdict 相关的约束是 00220 的
  `task_validation_reviews_verdict_state_check`（state='reviewed' 或
  validation_state='pending'），完成时两者同一 UPDATE 写入，并不卡住
  verdict；00214 的结果完整性约束要求 reviewed 时全部来源列非空，那是故意
  的完整性规则，不应放宽。verdict 事实不另建列持久化：事实的来源行（审批/
  步骤/操作）本身已持久化，推导是纯函数，verdict 随时可从库里重推导、与已存
  `validation_state` 对照；如需审计时免重推导，可后续另加 `verdict_facts
  jsonb` 列（需一次迁移决定，本轮不做）。
- 边界（Codex 不该碰的部分）：
  - H02 证据内容校验（`verifiedTaskEvidenceContentCoverage`）未动：verdict
    仍需 web/artifact 的可验证据内容才会 PASS，事实齐全但证据未验时 verdict
    仍为 pending，这是故意的。
  - `CorrectionRecordVerified` 的含义是“重试链记录了一次成功的纠正”，不是
    “纠正已沉淀为可复用的 lint/检查/skill”：基底没有纠正记录表，无法验证后
    者，不要把本项的 true 读成已沉淀。
  - `ReplayVerified` 只证明“已存记录足以重放”（ledger 覆盖 + 已终态），不
    是真的重跑了测试；取消（cancelled）的操作行会使重放保持未验证
    （fail-closed）。
  - 换模型家族仍只是建议：`SelectIndependentTaskValidationReviewModel` 未
    动，不强制。
  - `CompleteTaskValidationReview` 的读回比较只在 `RowsAffected == 1` 时跑；
    行未更新（租约过期/版本已变）时仍返回未完成，不报错。
- flow-doc 待同步：无（verdict 七检查的语义未变；只是原来恒 pending 的三项
  现在能从已存记录解析。流程文档若写“验证结论常 pending”，待部署后抽查确认
  再同步）。
- 部署后待做（需部署 + 真实任务数据，本轮未做）：配 `CH_TEST_DATABASE_URL`
  跑一次 `TestPostgresTaskValidationVerdictFactsResolveFromStoredRecords`；
  部署后抽查真实任务的验证 verdict 分布，确认结论不再常 pending（核查报告
  §三第 10 项）；另需一次带可验 web/artifact 证据的真实修任务，确认端到端
  PASS 路径。
- 验证结果：`go build ./...` 通过；`go vet ./internal/app/
  ./internal/store/postgres/` 通过；`gofmt -l` 在改动文件上无输出；`go test
  -race ./internal/app/` 指定测试（DeriveTaskValidation* /
  VerifyTaskValidationVerdictReplay / IndependentVerifierResolvesFixTaskVerdict
  / 既有 EvaluateTaskValidationReview 与 TaskValidationVerdict 测试）全部通
  过，全量 race 与基线一致（仅 E01 记录的 4 个既有失败：
  `TestAgentWorkspaceFilesAndHostUseTheSameDirectory`、
  `TestRuntimeToolExecutorVerifyAgainstWorkspaceFileHandler`、
  `TestApplyLiveProjectUploadMovesUnchangedRenamedFileInSharedDirectory`、
  `TestQuotaSettlePersistsOwnerStoragePauseBeforeAborting`，均为工作区路径/环
  境问题，与本次改动无关）；`go test ./internal/store/postgres/` 全过（新 PG
  集成测试无 `CH_TEST_DATABASE_URL` 跳过，loader 的三条新 SQL 未在真库上跑
  过）。

## E10 部署进链路 + 合并门（P0 smoke 10 条，任一失败拦下）

- 缺口：核查报告 §一 E10 行（门不存在）与 §三第 3 项（smoke 门 blocked 不
  拦——该项记的是 sit 上 fail-closed 改造后的状态；本轮按任务要求仍用原始
  设计的 pass/fail/blocked 三态，fail-closed 改造留给后面的 P3 批次，不提前
  做）；增强方案 E10；设计方案 §16.3（P0 smoke 10 条，每次部署后跑一遍；
  10 条固定为 T01/T04/T06/T10/T13/T19/T22/T26/T36/T43；标 P+D 的必须实际
  PC 与真实 DSH 两者贯通才算通过；标 S+P 的两层分别记，不用单一通过覆盖未
  执行的层级）。
- 改了哪些文件：
  - `deploy/single/smoke-cases.tsv`（新建）：10 行，列为 case / level / 测
    点 / check / needs；每条按 §16.3 的测点映射到一个真实 Go 测试——T01
    `TestConversationMessageReceiptIsAtomicAndScoped`、T26
    `TestConversationRouteSteersOnlySafeRunningTaskSupplement`、T36
    `TestPostgresTaskAgentRequestReconciliationPersistsUntilExactResolution`
    （三者均为 PG 集成测试，需 `CH_TEST_DATABASE_URL`）；T04
    `TestExplicitSearchFallbackAvoidsQuestionsNegationAndActiveTasks`、T06
    `TestConversationRouteWorkerClassifiesFollowUpWhilePredecessorRouting`、
    T13 三个步骤详情测试、T19
    `TestTaskApprovalResponseCarriesEvidenceFields`、T22
    `TestRuntimeToolExecutorVerifyRequiresCommittedWorkspaceBlockReadback`
    （内存测试，无库可跑）；T10/T43 为 `manual:needs real PC + real
    model/DSH`，没有自动化检查。
  - `deploy/single/smoke.sh`（新建）：一批受控跑，读 TSV，逐条记
    pass/fail/blocked/missing，写 JSON 记录（head / run_at / counts /
    cases；每条含 §16.3 level 与实际执行的层级 `executed`）。任一 fail →
    exit 1；blocked 不拦门（原始设计）。Go 发现顺序：`GO_BIN`、PATH 上的
    `go`、持久 toolchain（本机无 `/usr/bin/go`）。映射的测试不存在时
    （`go test` 报 "no tests to run"）记 missing，不谎报通过。
    `SMOKE_CASES` / `SMOKE_RECORD` / `GO_BIN` 可覆盖表、记录与 Go。
  - `deploy/single/update.sh`（只动门相关位置）：预检加环境挂了检测——
    新增 `config_cloud_dsh_value`（读 live portal.yaml 的 `cloud_dsh`
    块）；`enabled` 不是 false 时探 `docker -H <docker_host> info`，不可
    达即 die 并说明是哪个 host；`/srv/morde-acceptance` 须为挂载点，否则
    die 并说明原因；`mountpoint` 加入必需命令。checkout 后、build 前跑
    smoke 门（`$SRC_DIR/deploy/single/smoke.sh`），失败即 die 中止部署，此
    时未换版、运行中的二进制 untouched；dry-run 也跑这道门。
  - `server/Makefile`：`.PHONY` 加 `smoke`，`make smoke` 调
    `../deploy/single/smoke.sh`。
  - `.gitignore`：加 `deploy/single/smoke-record.json`。
  - `server/internal/evaluation/smoke_cases_test.go`（新建）：
    `TestSmokeCasesTableMatchesRealTests`（表恰为 10 条固定顺序，每个
    go-test 映射在对应包里确有此测试）与
    `TestSmokeCasesTableCheckerRejectsBadRows`（7 个反例：改名后无匹配、
    未知 check、重复用例、未知用例、未知 needs、manual 无理由、缺一条）。
- 逻辑说明：门的位置是 checkout 后、build 前——跑的是新源码的 smoke，
  失败时旧版毫发无损。状态口径是原始设计：只有 fail 拦门；blocked（无测
  试库、需真实 PC）与 missing（无映射）只记录，不拦门。记录里的
  `executed` 字段限定了 pass 的含义：T13/T19/T22 的 pass 是 service 层
  （映射的 Go 测试只覆盖服务侧规则），PC 层未验；T22 的 P+D 要求 PC+DSH
  贯通，自动化跑目前只覆盖“验证须读回”这条规则，完整通过仍需真实 PC 验
  收——按 §16.3，不用单一通过覆盖未执行的层级。
- 边界（Codex 不该碰的部分）：
  - blocked 口径不要提前改 fail-closed：那是 P3 批次；本轮 blocked 放行
    是故意的，记录在案。
  - 不要把 T10/T43 写成能自动通过：它们没有自动化检查，manual 行永远记
    blocked，直到有人在真实 PC 上跑并登记。
  - smoke 门不跑需要真模型/真服务器的慢测试：10 条映射的都是内存测试与
    PG 集成测试；T01/T26/T36 无库时记 blocked，不伪造。
  - `update.sh` 只动了门相关位置：预检段加环境检测、build 前加 smoke
    门；其余流程（回滚、迁移、换版、健康检查）未动。
- flow-doc 待同步：无（部署流程语义未变，只是 build 前多了一道门；流程
  文档若登记部署步骤，门的登记位置待同步，本轮不改 flow 文档）。
- 部署后待做（需测试库 / 真实 PC，本轮未做）：配
  `CH_TEST_DATABASE_URL` 跑一次全量 smoke，把 T01/T26/T36 的 blocked 消
  掉；在真实 PC 上跑 T10/T43（T43 即 CLS-01 全链路）并登记；P3 再做
  fail-closed（blocked 计失败 + 例外门通知）。
- 验证结果：`bash -n deploy/single/smoke.sh deploy/single/update.sh`
  通过；`go build ./...` 通过；`go vet ./internal/evaluation/` 通过；
  `gofmt -l` 在改动文件上无输出；`go test -race ./internal/evaluation/`
  全过（含新表测试与 7 个反例）；`make smoke` 本地实跑：pass=5 fail=0
  blocked=5 missing=0，exit 0（PASS：T04、T06、T13、T19、T22；BLOCKED：
  T01/T26/T36 无 `CH_TEST_DATABASE_URL`、T10/T43 需真实 PC + 真实模型
  /DSH；记录在 `deploy/single/smoke-record.json`，已在 .gitignore 内）；
  反例实跑：临时表放一个本地失败的测试 → fail=1、exit 1，门拦得住（临
  时文件已删）；不存在的测试映射 → missing、不谎报通过。`update.sh` 未在
  服务器上实跑（本机不是部署服务器；预检的 awk 解析已对仓库 portal.yaml
  验过，取出 `docker_host` 与 `enabled`；`/srv/morde-acceptance` 在本机
  未挂载，符合预期）。

## E08 事件本体 9 种（读侧投影 + 可回放的任务事件链）

- 缺口：核查报告 §一 E08 行（部分通过：本体 + 回放 UT 均过（含完整修 bug 链
  与交付无验证反例）；无 HTTP/SSE 端口，PC 看不到链，无真实任务证据）与
  §三第 11 项（E08 无端口：回放逻辑只在 UT 里，OpenAPI `TaskEvent` 仍是
  11 种枚举，未接 SSE）；增强方案 E08；设计方案 §16.1（CLS-01 证据链按任
  务 ID 串起来）与 §16.7（失败工作项留痕的精神）；补充文档 §3（总验收标
  准）。本轮只做读侧投影与 facts reader，不接 HTTP/SSE，§16 仍需真实任
  务证据。
- 改了哪些文件：
  - `server/internal/app/task_event_ontology.go`（新建）：9 种本体常量
    （created/split/dispatched/approval/execution/verification/delivery/
    failure/recovery）、`TaskEventChainFacts`（验证 verdict + 结果事件序
    列 + 收据已发布 + 收据事件序列）、`TaskEventChainNode` /
    `TaskEventChain`、`ProjectTaskEventChain`（纯投影函数）。
  - `server/internal/app/task_event_ontology_test.go`（新建）：完整修
    bug 链回放、交付三条件反例（无验证/FAIL/pending 均无 delivery）、
    逐项映射规则、坏输入拒绝、链头由首个事件派生。
  - `server/internal/app/task_store.go`：`TaskStore` 增加
    `GetTaskEventChain`（读侧契约；未来的 HTTP/SSE 接线点）。
  - `server/internal/app/task_service_test.go`：`taskStoreStub` 增加
    `GetTaskEventChain`（按 stub 已存事件投影）。
  - `server/internal/store/postgres/task_event_chain.go`（新建）：
    `Store.GetTaskEventChain` 与 `loadTaskEventChainFacts`（facts
    reader：全量事件 + 当前修订的 reviewed 验证 verdict + 已发布收据，
    一次 repeatable-read 快照内读完）。
  - `server/internal/store/postgres/task_event_chain_integration_test.go`
    （新建）：`TestPostgresTaskEventChainReplaysCompleteFixTask`（完整
    修 bug 链 + PASS 验证 + 已发布收据 → 链尾 verification + delivery）、
    `TestPostgresTaskEventChainOmitsDeliveryWhenVerificationFails`（FAIL
    验证 → 有 verification、无 delivery）、
    `TestPostgresTaskEventChainRequiresExistingTask`（缺任务 → not_found）。
- 逻辑是什么：
  - 只做读侧投影：写侧的 11 种 `task.*` 事件类型一个字没动，不新增写
    侧枚举，不加迁移；OpenAPI `TaskEvent` 仍是 11 种枚举，不动。
  - 映射规则写死在 `projectTaskEventOntologyNode` 里：最早的事件（通
    常是 `task.status`/`queued`，文件交付意图不明时是首个
    `task.waiting_user`）一律投影为 created，链头必为 created；
    `task.plan` → split；`task.step` / `task.batch_item` → execution，
    payload status 为 failed 时 → failure；`task.operation` →
    dispatched（status dispatched）、failure（failed）、recovery
    （status unknown 且 `requires_reconciliation`，即 lease 认领时的
    对账记录）、其余 execution；`task.waiting_user` → approval；
    `task.response` 只有带 `pending_id`（答复审批）时 → approval，澄
    清答复不算审批；`task.status`（非首个）→ dispatched（running）、
    failure（failed），其余不映射——`completed` 明确不映射为
    delivery；`task.message` / `task.artifact` / `task.priority` 及
    其他类型不映射。
  - verification 与 delivery 是事实投影，不是事件映射：verification
    在当前修订存在 reviewed 验证 verdict 时出现，锚在 verdict 的结
    果事件序列上；delivery 只在 verdict 为 PASS 或 PASS+NOTES 且收
    据已发布时出现，锚在已发布来源的事件序列上，排在 verification 之
    后。FAIL/pending verdict 或未发布收据时一律无 delivery——即使收
    据行存在，投影也不假设发布门曾拦住它。
  - 回放按来源序列排序，同序列按本体顺序排（verification 在 delivery
    之前）；recovery 插在失败后恢复处（对账记录的序列位置）；未映射
    的事件全部进 `UnmappedSequences`（升序），绝不悄悄丢掉——`task.result`
    事件本身也不映射（它的内容由锚在它上面的 verification/delivery
    节点代表），`completed` 状态事件同样进未映射。
  - facts reader 只读已存记录：事件全量按序列读；verdict 只认
    `state='reviewed'` 的行（pending/processing 的 review 不产生
    verification 节点）；收据只认来源与批次双双 `published` 的行。
    输入校验是 fail-closed：空事件表、跨任务事件、重复序列、非法
    verdict、verdict 无结果事件、已发布收据无收据事件都直接拒绝投
    影，不出半条链。
- 边界（Codex 不该碰的部分）：
  - 映射表（`projectTaskEventOntologyNode`）与 delivery 的双条件不要
    改：`task.status` completed 映射为 delivery 是明确禁止的；不要
    把 `task.artifact` 当成 delivery（产物不是交付收据）。
  - 不要把 recovery 的判定放宽成“失败后任意成功步骤”（步骤重试的
    重试链信息不在事件 payload 里，投影认的只有对账记录这一条硬证
    据）；不要把 `CorrectionRecordVerified` 之类的 E09 语义读进本
    项。
  - `UnmappedSequences` 不要删、不要改成只记类型：它是“绝不悄悄丢
    掉”的审计口径，回归对照靠它认。
  - 不要加 HTTP 端口、不要接 SSE：那是核查报告 §三第 11 项的后续独
    立一项，本轮的调用面只有 `TaskStore.GetTaskEventChain` 与纯投
    影函数。
  - 历史被裁剪过的任务（事件表不再从序列 1 开始）链头仍是现存最早
    事件派生的 created——投影不虚构真正的创建记录；若需区分，去
    查任务创建时间，不在本项做。
- flow-doc 待同步：事件本体 9 种是读侧新增概念——只记一笔，**不要直接
  改 flow 文档**（Codex 并行改代码中）。
- 部署后待做（需测试库 / 真实任务，本轮未做）：配
  `CH_TEST_DATABASE_URL` 跑一次三个 PG 集成测试（本轮无库跳过，
  loader 的三条新 SQL 未在真库上跑过）；部署后抽查真实修 bug 任务的
  事件链，确认能从头查到尾；接 HTTP/SSE 后再按核查报告 §三第 11 项
  验收“PC 上能从头查到尾”。
- 验证结果：`go build ./...` 通过；`go vet ./internal/app/
  ./internal/store/postgres/` 通过；`gofmt -l` 在改动文件上无输出；
  `go test -race ./internal/app/` 指定测试（ProjectTaskEventChain 全
  部）全部通过，全量与基线一致（仅 E01 记录的 4 个既有失败）；
  `go test ./internal/store/postgres/` 全过（三个新 PG 集成测试无
  `CH_TEST_DATABASE_URL` 跳过）。

## E11 交付真实性校验（FAIL 整批拒绝 + 收据措辞）

- 缺口：核查报告 §一 E11 行（部分通过：门禁 UT 过（FAIL 整批拒绝）；
  集成测试本地无库跳过，pending 不拦是待拍板口径）与 §三第 10 项（E09 /
  E11 的集成测试无库跳过）与第 12 项（E11 pending 口径待拍板：pending 不
  拦交付是实现方有据决定，未获用户确认）；增强方案 E11；设计方案 §16.1
  （CLS-01：交付消息与任务账本对不上即不通过）与 §16.7（失败工作项留
  痕）。本轮只做门禁机制，§16 仍需真实任务证据。
- 改了哪些文件：
  - `server/internal/app/conversation_result_delivery.go`（新建）：
    `ValidateConversationResultDelivery`（纯判定：任一来源当前 verdict
    为 FAIL 即整批拒绝，`CodeInvalidState`，stage detail 为
    `result_validation_gate`，并点名失败任务）。
  - `server/internal/app/conversation_result_delivery_test.go`（新建）：
    表驱动单测（含 FAIL 整批拒绝、pending 不拦、坏输入拒绝）。
  - `server/internal/store/postgres/conversation_result_review.go`：
    `publishConversationResultReviewInTx` 在算出各来源当前 verdict 后、
    任何发布写操作之前调门；拒绝即返回错误，发布事务回滚。
  - `server/internal/store/postgres/conversation_result_delivery_integration_test.go`
    （新建）：`TestPostgresTaskResultDeliveryRefusesFailedValidation`
    （FAIL → 整批 failed、路由 `routing_action_unavailable`、来源仍
    pending、零收据消息）、
    `TestPostgresTaskResultDeliveryRetryPublishesAfterReReviewPasses`
    （复核通过后授权重试重发 → published）、
    `TestPostgresTaskResultDeliveryPublishesPassingValidation`（PASS 发
    布、pending 不拦的正对照）。
  - `server/internal/store/postgres/conversation_result_aggregation_integration_test.go`：
    `TestConversationResultReceiptSeparatesCompletionFromCurrentValidation`
    去掉 FAIL 来源（见边界）：含 FAIL 的一批现在发不出去，该用例的
    FAIL 情形改由 E11 的集成测试覆盖。
- 逻辑是什么：
  - 纯门禁，接在发布写操作之前：`publishConversationResultReviewInTx`
    先按既有口径读出各来源当前 verdict（reviewed 行的
    `validation_state`，无行即 pending），再调
    `ValidateConversationResultDelivery`；任一 FAIL 即返回
    `CodeInvalidState`，stage detail 写清是结果验证门
    （`result_validation_gate`），details 里点名失败任务与个数。拒
    绝是原子的：发布在 `ApplyConversationRoute` 的事务里，门返回错误
    即回滚——不插收据消息、不把来源置 published、不把批次置
    published；路由 worker 随后以 `routing_action_unavailable` 失败
    该路由，`FailConversationRoute` 把整批置 failed（该路径是既有行
    为，本轮未改）。
  - 授权重试可在复核通过后重发：失败批次走既有的
    `RetryConversationRoute` 恢复为 processing，重新跑路由时门按当
    前 verdict 重判——复核仍 FAIL 就再拦，复核通过才放行。重发不需
    要新机制，门每次发布都重读当前 verdict。
  - pending 不拦交付（有据的既定决定，保持）：收据如实标注 pending，
    且没有“验证通过后重发”的机制，拦 pending 会把正常交付拖进手动重
    试。这是待用户拍板项（核查报告 §三第 12 项），本轮不擅自改成拦。
  - checkpoint-digest fence 保持已有层面的保证，未重做：发布路径既
    有的三处摘要核对（入队时结果内容摘要、加载时任务结果与来源内容
    一致、验证状态读取时 `task.result` 的 sha256 与结果事件的
    `result_content_sha256` 一致）都在门之前，对不上即按 stale supersede
    或 pending 处理，不进发布。
  - 收据措辞只写修了什么、哪些文件、怎么验证的：这是结构保证，不是对
    模型文本做关键词匹配（模型文本不可靠匹配，未做）。收据固定结构
    是模型摘要（修了什么）+ 交付事实行（哪些审批/操作/文件）+ 每来源
    验证状态（怎么验证的），头上注明“主助手整理，不代表独立验收”；
    门保证 FAIL 的 verdict 永远到不了收据，所以收据里不会出现“验证
    失败的结果被说成已验证”。
- 边界（Codex 不该碰的部分）：
  - 门只认当前 verdict：不要把它扩大成拦 pending（待拍板，见上），不
    要把它改成拦“无验证行”（无行即 pending，按 pending 口径放行）。
  - `result_validation_gate` 这个 stage 值与 `failed_task_ids` /
    `failed_count` 的 details 键不要改名（路由失败日志的
    `failure_stage` 与回归对照靠它们认）。
  - 不要动 `FailConversationRoute` 与 `RetryConversationRoute`：整批
    置 failed 与授权重试都是既有行为，门只负责拒绝发布。
  - `TestConversationResultReceiptSeparatesCompletionFromCurrentValidation`
    里 FAIL 来源的删除是故意的：含 FAIL 的一批按新门发不出去，该用
    例再断言“收据里列出 FAIL”是自相矛盾的；FAIL 的发布行为改由 E11
    的三个集成测试覆盖，不要把 FAIL 来源加回该用例。
  - 收据 formatter 本身（`conversationTaskResultReceipt*`）未动：它
    仍可格式化出 FAIL 状态行（纯函数测试仍覆盖），门是在发布入口拦，
    不是在 formatter 里藏 FAIL。
- flow-doc 待同步：结果发布现在多了一道验证门——只记一笔，**不要直接
  改 flow 文档**（Codex 并行改代码中）。
- 部署后待做（需测试库 / 真实任务，本轮未做）：配
  `CH_TEST_DATABASE_URL` 跑一次三个新 PG 集成测试（本轮无库跳过，
  门的接线未在真库上跑过）；部署后抽查真实任务的交付：FAIL 的结果确
  实发不出去、复核通过后重试确实能发；pending 是否也拦，等用户拍板
  （核查报告 §三第 12 项）。
- 验证结果：`go build ./...` 通过；`go vet ./internal/app/
  ./internal/store/postgres/` 通过；`gofmt -l` 在改动文件上无输出；
  `go test -race ./internal/app/` 指定测试
  （ValidateConversationResultDelivery 全部，含 FAIL 整批拒绝与
  pending 不拦）全部通过，全量与基线一致（仅 E01 记录的 4 个既有失
  败）；`go test ./internal/store/postgres/` 全过（三个新 PG 集成测试
  无 `CH_TEST_DATABASE_URL` 跳过；更新过的 SeparatesCompletion 用例
  同样跳过，未在真库上验证）；`go test ./internal/worker/ -run
  TestConversationRoute` 有 1 个失败
  （`TestConversationRouteWorkerHandlesOnlyConfirmedBoundedRateLimits/bounded_fallback`，
  限流梯子延迟断言 5.86s vs 期望 1s），已核实是基线既有失败——在
  未含 E11 改动的 E08 树上同样失败、连跑 3 次同样失败，与发布门无关
  （本轮未动 worker 与限流代码），记在此处备查。

## P3-1 任务级模型重试调用方（网关重试环 + 执行器梯子决策）

- 缺口：核查报告 §一 E12 行（部分通过：31 值分类表与梯子常量、路由 worker
  已接 `ModelRetryDelay`、任务级调用方已接；换方法→换 agent→终态全链路仍无
  真实任务实跑）与 §三第 6 项（E12 任务级重试缺调用方）与第 7 项（全链路实
  跑）；增强方案 E12；设计方案 §16.5（用户可见报错率 ≤1%：终态文案不带堆
  栈、不让用户自己修）。本轮只做任务级调用方的机制级接线，§16 仍需真实任
  务证据。
- 审计结果（动手前核实，P0 的 E12 并未包含本项）：
  - E12 只交付了梯子决策函数（`DecideModelRetry` /
    `ApplyTaskModelFailureLadder` / `ClassifyModelError`）与路由 worker 的
    接线（`conversation_route_worker.go` 两处 `DecideModelRetry` 调用）；
    `ApplyTaskModelFailureLadder` 在 P0 代码里除单测外无调用方。
  - 网关无重试环：`ModelGatewayService.Complete` 对 provider 只调一次；
    compaction（`CompleteConversationSummaryModel`）与 review
    （`CompleteTaskValidationReviewDSHModel`）两处任务级调用都是单次
    `s.Complete`，无循环。
  - 执行器无梯子决策：`pauseCloudDSHModelRecovery` 只把不可重试原因分流
    到终态记录；可重试原因从不查梯子（无尝试计数、无
    `ApplyTaskModelFailureLadder` 调用），直接走通用暂停。
  - 无 600s 累计封顶：全仓只有每步 120s 上限；Retry-After 优先且无上限
    （路由层另有 24h durable horizon，不在梯子内）。
  - 不能在调用方重复调 `Complete` 来重试：失败尝试已 settle（unknown 用
    量落账），同一 CallID 再次进 `Complete` 会在 reservation 被拒
    （`reserveModelCallInTx`：已有 ledger 行即 `CodeConflict`）。所以重
    试环只能放在 `Complete` 内部、provider 调用处、settle 之前。
- 改了哪些文件：
  - `server/internal/app/model_retry.go`（新建）：`ModelRetryTotalDelayCap`
    （600s 累计封顶）、`ModelRetryExhaustedError`（耗尽信号，含原因/已花
    重试次数/最后一次失败，可 `Unwrap`）、`IsModelRetryExhausted`、
    `modelRetryAfter`（从 `*ModelClientError` 或 `*domain.Error` 取
    Retry-After）、`sleepModelRetry`（带 ctx 的生产 sleep）、
    `retryTaskModelCall`（纯重试循环：分类→`DecideModelRetry`→累计封顶
    检查→sleep→重试）、`modelRetryExhaustedDomainError`（耗尽转 domain
    错误，code 沿用 `modelConnectionCheckError` 的映射，details 带
    reason/attempts/`retry_ladder=exhausted`，耗尽标记经 `Err` 保留）。
  - `server/internal/app/model_gateway.go`：`ModelGatewayService` 增加
    `modelRetrySleep` 字段（默认 `sleepModelRetry`，同包单测可替换以记录
    延迟而不等待）；`Complete` 的 provider 调用改走新增的
    `completeTaskModelCall`；settle 后的 `callErr` 返回分支先认耗尽错误
    （转 domain 错误），再走原有的 `modelConnectionCheckError`。
  - `server/internal/app/model_retry_test.go`（新建）：纯循环表驱动单测
    （瞬时失败后成功、3 次重试档位、Retry-After 优先与向上取整、600s 封
    顶两种耗尽、unknown 只重试 1 次、invalid_response 重试 2 次、ctx 中
    断不是耗尽、坏输入拒绝、耗尽 domain 错误的 code 与 details）与网关
    级接线单测（compaction：限流两次后成功，provider 调 3 次、sleep
    [5s..6.25s, 10s..12.5s]、reserve/dispatch/settle 各 1 次；compaction
    耗尽：provider 调 4 次、错误带耗尽标记且 code 为 rate_limited、仍只
    settle 1 次；review：超时一次后成功，provider 调 2 次、reserve/
    dispatch/settle 各 1 次）。
  - `server/internal/worker/cloud_dsh_executor.go`：
    `pauseCloudDSHModelRecovery` 改走梯子决策；新增
    `cloudDSHModelFailureLadderReason`（diagnostic 类别优先映射，见下）、
    `cloudDSHModelFailureCategoryReason`（类别→原因映射表，未知类别不猜，
    回落到 `ClassifyModelError`）、`cloudDSHModelFailureLadderSpent`
    （diagnostic 的 `LadderAttempts` + cause 自带的 Retry-After）。
  - `server/internal/worker/cloud_dsh_executor_retry_test.go`（新建）：
    `TestPauseCloudDSHModelRecoveryLadderDecision`（fake tools 记录走哪
    条暂停路：终态原因一次即耗尽、新限流只暂停、限流花满 3 次耗尽、花
    2 次仍暂停、invalid_response 花满 2 次耗尽、context_length 永不耗
    尽、未映射类别回落分类后暂停、纯 domain 限流走通用暂停、纯 domain
    认证错误耗尽）与 `TestCloudDSHModelFailureCategoryReason`（映射表正
    反例）。
- 逻辑是什么：
  - 梯子参数与 E12 一致：同调用最多重试 3 次（invalid_response 类 2 次、
    unknown/tool_failed 类 1 次），base 5s 指数翻倍、上限 120s、至多
    25% jitter；Retry-After 优先（向上取整到秒）；累计等待超 600s 即耗
    尽，不再 sleep。
  - 只重试 compaction 与 review 两处任务级调用：`completeTaskModelCall`
    只在 `ModelCallCompaction` / `ModelCallReview` 时走重试循环，其余
    source（Task/Route/Probe/Client）保持单次调用。同一 CallID 只做一
    次 reservation、一次 dispatch、一次 settle：重试的是裸 provider 调
    用，settle 只在循环结束后以最终结果跑一次。
  - 耗尽即终态：循环返回 `*ModelRetryExhaustedError`，`Complete` 在
    settle 后把它转成带 `retry_ladder=exhausted` 的 domain 错误返回；
    compaction 调用方收到错误即本次压缩失败、不产生新摘要版本（旧摘要
    仍有效，不伪造），review 调用方收到错误即本次验证未完成、verdict
    保持 pending（不判 PASS）。两种终态都不让失败冒充成功。
  - 执行器侧：暂停决策先按梯子算一遍——原因优先取 diagnostic 的类别
    （DSH 结果错误的文本是通用的，只凭文本分类会把限流误判成 unknown），
    再取 diagnostic 里已花的 `LadderAttempts` 与 cause 的 Retry-After，
    调 `ApplyTaskModelFailureLadder`；耗尽（不可重试，或可重试但次数已
    花满）才记终态记录，否则走原有的 diagnostic/通用暂停（暂停后恢复即
    任务级重试）。`context_length_exceeded` 仍排除在外（专用“缩短后重
    发”文案不变）。DSH 失败本身不带 `LadderAttempts`，所以首次暂停永
    远是重试、不是终态。
  - 行为变化（有意）：diagnostic 类别为终态原因（如
    authentication_rejected）的 DSH 失败，过去因文本分类为 unknown 而
    走通用暂停，现在按类别走终态记录。这是梯子决策的本意，不是回归。
- 边界（Codex 不该碰的部分）：
  - 不要把重试环搬到调用方重复调 `Complete`（同一 CallID 第二次进
    `Complete` 会被 reservation 以 `CodeConflict` 拒绝，见上审计）。
  - `ModelRetryTotalDelayCap` = 600s 与每步 120s 上限不要改值；`ModelCallTimeout`
    = 45s 仍是单次调用的实际 ceiling（sleep 遇 ctx 超时即返回 ctx 错误，
    不记耗尽）。
  - settle 后才知道的响应形状失败（摘要/验证体非法）不在重试范围：它在
    settle 之后才被发现，重试需要第二个 reservation，账本不允许。
  - `ModelRetryExhaustedError` 的 `Err` 链不要断：code 映射与耗尽标记都
    靠它（`errors.As` 找得到耗尽标记、provider 错误与 domain 错误）。
  - 执行器侧不要给每次暂停凭空编一个尝试计数：跨暂停的累计计数不存在，
    那是 P3-6 重复错误记录的事，本轮不猜。
- flow-doc 待同步：hermes 19 步流程里“重试梯子/终态”两步现在覆盖了任务
  级 compaction/review 调用与执行器暂停决策——只记一笔，**不要直接改
  flow 文档**（Codex 并行改代码中）。
- 部署后待做（需真实任务，本轮未做）：一次 429/网络错误注入，走完重
  试→换方法→换 agent→终态全链路（核查报告 §三第 7 项），并核对终态告
  知文案与 `route_retry_ladder_exhausted` 记录。
- 验证结果：`go build ./...` 通过；`go vet ./internal/app/
  ./internal/worker/` 通过；`gofmt -l` 在改动文件上无输出；`go test
  -race` 指定测试（重试循环、耗尽、Retry-After、600s 封顶、ctx 中断、
  compaction/review 接线、执行器梯子决策、类别映射）全部通过；
  `go test ./internal/app/` 全量仅 3 个失败，全部是 E01 记录的基线既
  有失败（`TestAgentWorkspaceFilesAndHostUseTheSameDirectory`、
  `TestApplyLiveProjectUploadMovesUnchangedRenamedFileInSharedDirectory`、
  `TestQuotaSettlePersistsOwnerStoragePauseBeforeAborting`，工作区路径/
  环境问题；第 4 个基线失败
  `TestRuntimeToolExecutorVerifyAgainstWorkspaceFileHandler` 本轮未复
  现）；`go test -race ./internal/worker/` 全量仅 1 个失败，即任务指定
  不动的基线既有失败
  （`TestConversationRouteWorkerHandlesOnlyConfirmedBoundedRateLimits/bounded_fallback`，
  限流梯子延迟断言 6.11s vs 期望 1s，路由 worker 未动）。

## P3-2 E02 回归对照接线（图层覆盖检查 + 套件范围核对）

- 缺口：核查报告 §一 E02 行（部分通过：影响面分析与回归对照的机制缺失）
  与 §三第 13 项（回归对照未接线）；增强方案 E02；设计方案 §16.7（失败
  工作项留痕、回归可追溯的精神）。本轮只做回归验证路径的机制级接线，
  §16 仍需真实任务证据。
- 审计结果（动手前核实，P0 的 E02/E07 只接了一半）：
  - 已接的是 plan 层：`EvaluateTestGate` 调 `UnrunCoveringTests`，把
    plan 的 impact 里记的 covering tests 与跑过的测试逐项核对；`Complete`
    的 host 分支在 E07 门里调它。
  - 未接的是图层：`task.DependencyGraph.CheckImpactCoverage` 在全仓除
    单测外无调用方；它要的 per-file 覆盖边（哪个测试覆盖哪个文件）在
    plan 里根本不存在——`ImpactMap` 只有三张扁平清单（changed_files /
    callers / covering_tests），plan 工具入参也不收 per-file 数据。
  - 更深的缺口在证据提取：`TestReportFromInputs` 里套件一通过就把全部
    covering tests 算成跑过（“套件跑了范围内的全部测试”是个假设，不是
    核对）。于是经真实路径，plan 层对照永远拦不住：套件通过 ⇒ 全算跑
    过 ⇒ 无未跑项；套件失败/未跑 ⇒ 更早的分支先拦。`go test
    ./internal/app/` 通过也能让 worker 包的 covering test“被跑过”。
- 改了哪些文件：
  - `server/internal/domain/task/brief.go`：`ImpactMap` 增加可选字段
    `Coverage map[string][]string`（`json:"coverage,omitempty"`，文件 →
    覆盖它的测试）；`Validate` 增加 coverage 校验（键须为合法 impact
    路径、每文件至多 128 个测试、测试名 ≤256 字且不重复；旧 plan 无此
    字段仍照常通过）。
  - `server/internal/domain/task/impact.go`：`Normalized` 同步归一化
    coverage（排序 + 去重；不在 changed/callers 里的文件条目丢掉；空
    清单丢掉），提交时已调 `Normalized`，新字段随 plan 事件持久化，
    checkpoint 回放的 `DeepEqual` 自动覆盖。
  - `server/internal/app/cloud_dsh_task_plan.go`：plan 工具的 impact
    入参增加 `coverage` 对象（文件 → 测试数组，各至多 128 项）与
    Description 同步（impact 应记 per-file coverage，否则门只能核对扁
    平清单）。
  - `server/internal/app/gapfix_gates.go`：新增 `CheckImpactCoverageGate`
    （plan 有 per-file coverage 时建图调 `CheckImpactCoverage`，未覆盖
    的改动文件按名报错；无 per-file coverage 时放行——扁平清单检查是
    唯一对照，旧 plan 行为不变）；`TestReportFromInputs` 改为收整个
    `*task.ImpactMap`，套件通过不再全算跑过，改为 scope-aware：整树运
    行（`go test ./...`、`make test`、`npm test` 等）才算全部跑过，包
    限定的套件只算它覆盖到的测试（测试覆盖的每个文件都须落在套件跑过
    的包里，按路径后缀匹配，兼容 `./internal/app/` 与
    `./server/internal/app/` 两种写法；无 per-file coverage 时包限定
    套件一个不算，fail-closed）；新增 `testSuiteScope`（套件命令的包
    范围解析）、`coveringTestCoveredBySuite`（测试是否被包限定套件覆
    盖）、`testSuiteCoversFile`（文件是否落在跑过的包里）；删除已无调
    用方的 `testGateCoveringTests`。
  - `server/internal/app/cloud_dsh_tool_broker.go`：`Complete` 的 host
    分支在 `EvaluateTestGate` 之后调 `CheckImpactCoverageGate`（用
    scope-aware 的 `report.CoveringTestsRan`），未覆盖即以
    `test_gate_pending` 暂停完成，不进回归、不交付。
  - 测试：`server/internal/domain/task/brief_test.go`
    （`TestImpactMapValidate` 增加 coverage 合法与三项非法用例）；
    `server/internal/domain/task/impact_test.go`
    （`TestImpactMapCoverageNormalized`：排序/去重/丢范围外文件/丢空
    清单）；`server/internal/app/gapfix_gates_test.go`
    （`TestCheckImpactCoverageGate`：未覆盖文件拦下并点名、全部覆盖通
    过、无 plan/无 impact/无 per-file coverage 放行、只记 caller 覆盖
    仍拦改动文件；`TestTestReportFromInputsSuiteScope`：包限定套件超
    出影响面时两道门同拦的反例、包限定套件覆盖全部影响包时通过、
    repo-root 包写法匹配、無 per-file coverage 时包限定套件不算数、整
    树套件仍全算；`TestTestSuiteScope`：范围解析表；既有
    `TestTestReportFromInputs` 四处调用按新签名改 impact 入参，整树
    套件用例行为不变）。
- 逻辑是什么：
  - 对照分两层，都在 `Complete` 的 E07 门里、同一 `test_gate_pending`
    暂停下：扁平层（`EvaluateTestGate`：新测试与套件都跑过且通过，
    impact 里记的 covering tests 全部跑过）与图层
    （`CheckImpactCoverageGate`：每个改动文件至少一个覆盖它的测试跑
    过，未覆盖的文件按名报出）。
  - 套件“跑过”的认定改按证据范围：整树运行算全部；包限定运行只算它
    覆盖到的测试；点名跑的测试（`-run`）按命令文本认定，与之前一致。
    包限定的认定只看命令文本里的包参数，不猜工作目录（路径后缀匹配
    兼容两种写法）。
  - per-file coverage 是可选的：agent 记了才有图层检查；没记时图层门
    放行，扁平层仍按 scope-aware 规则拦（包限定套件在无 per-file 数据
    时一个 covering test 都不算）。
- 边界（Codex 不该碰的部分）：
  - `ImpactMap` 旧字段的 JSON 形状没动；`Coverage` 是新增可选字段，
    不要把它改成必填（旧 plan 与未记 per-file coverage 的任务必须照
    常走扁平层检查）。
  - `CheckImpactCoverage` 对无覆盖记录的文件报未覆盖是故意
    fail-closed，不要改成“未知即通过”；图层门只查改动文件，不查
    callers（callers 的覆盖进扁平清单，不进图层）。
  - 不要把 `testSuiteScope` 的包匹配改成“前缀匹配”或“模糊匹配”：
    两种写法靠路径后缀匹配，工作目录未知时不要猜。
  - 非 Go 的包限定套件（如 `pytest tests/foo.py`）目前按整树计，是已
    知局限（见下），不要悄悄改成别的口径。
  - `test_gate_pending` 这个 waiting reason 不要改名（回归对照与客户
    端靠它认）。
- flow-doc 待同步：流程文档第 7 步“跑测试”与第 8 步“回归验证”现在有
  两层覆盖门与套件范围核对——只记一笔，**不要直接改 flow 文档**
  （Codex 并行改代码中）。
- 部署后待做（需真实任务，本轮未做）：一次真实修 bug 任务，impact
  记 per-file coverage，跑包限定套件，确认未覆盖文件真拦门、覆盖全
  后真放行；另需确认非 Go 套件的范围口径（目前整树计）。
- 验证结果：`go build ./...` 通过；`go vet ./internal/app/
  ./internal/domain/task/` 通过；`gofmt -l` 在改动文件上无输出；
  plan 工具 Parameters JSON 仍合法（含新增 `coverage` 属性）；
  `go test -race` 指定测试（覆盖门、套件范围、范围解析、impact
  归一化与校验、既有 E07 门与报告提取）全部通过；`go test
  ./internal/domain/task/` 全过；`go test ./internal/app/` 全量仅
  3 个失败，全部是 E01 记录的基线既有失败（与 P3-1 同一组 3 个，
  工作区路径/环境问题，与本次改动无关）。`Complete` 门分支无
  broker 级单测（需 store + runtime 全套 fake，超出本轮范围，与
  E07 同口径），门函数本身与接线已覆盖。

## P3-3 smoke 门 fail-closed（终态只有 pass/fail/exception，无证据即失败）

- 缺口：核查报告 §一 E10 行（部分通过：smoke 门三态）与 §三第 3 项
  （smoke 门不是 fail-closed）；增强方案 E10；设计方案 §16.3（P0
  smoke 10 条，任一失败拦下）与 §16.8（每周 44 条套件的门禁口径）。
  P1 的 E10 按原始设计做了三态（pass/fail/blocked），`blocked` 不拦门：
  无测试库时 3 个 PG 用例、无真实 PC 时 2 个用例都记 `blocked`，门照常
  通过——“没跑过”与“跑过且通过”在部署决策上没有区别。
- 审计结果（动手前核实，P0/P1 只做了三态，fail-closed 确实没做）：
  - `deploy/single/smoke.sh` 的终态是 pass/fail/blocked/missing 四
    种，`fail == 0` 即退出 0；`blocked` 与 `missing` 都不拦门。
  - `go test` 只看退出码：退出 0 即记 pass，不区分“跑过且通过”与
    “全跳过”（退出码同样是 0）；映射的测试改名后只记 `missing`，同
    样不拦门。
  - `deploy/single/update.sh` 的 preflight 已有环境挂了检测（DSH
    daemon 不可达、`/srv/morde-acceptance` 未挂载时以原因命名 abort），
    在 smoke 门之前跑；两层例外门的职责不同，见下。
- 改了哪些文件：
  - `deploy/single/smoke.sh`：终态改成 pass/fail/exception 三种，
    取消 `blocked` 与 `missing`；记账键只剩
    `{pass, fail, exception}`，退出 0 当且仅当 `fail == 0`。
    `needs=CH_TEST_DATABASE_URL` 且变量未设时记 `exception`（不跑测
    试）；`needs=real-pc` 与 `manual:` 检查记 `exception`（自动化门
    里永远跑不了）；未知 needs / 未知 check 种记 `fail`（表错了也是
    失败，不是“没证据”）。
    go-test 分支改跑 `go test -v`，用新增的 `go_test_verdict`（内嵌
    python）按叶级解析 `--- PASS/FAIL/SKIP:` 行：有子测试的顶层测试
    只按子测试判（父测试通过而子测试全跳过 = 无证据 = 失败）；任一
    FAIL 叶、任一顶层测试零 PASS 叶、无任何 verdict 行都记 `fail`；
    `no tests to run` 记 `fail`（映射的测试已不存在）；退出码非 0 仍
    按尾部输出记 `fail`。例外在记录里逐条留 detail，并在 stderr 打
    一条例外通知（例外数 + 每条的原因），部署日志里看得见哪些证据缺
    着——例外不是通过，只是暂时不拦门。
  - `deploy/single/smoke-cases.tsv`：表头注释改 fail-closed 口径（无
    证据即失败；例外只认上面两条自动检测的环境限制；例外记进记录并
    打印通知，永不算通过）。10 行用例本身未动。
  - `deploy/single/update.sh`：只改 smoke 门段的注释（行为未动）：
    写明两层例外门的关系——preflight 拦的是“线上环境本身挂了”
    （DSH 不可达、同步盘未挂载），abort 整个部署；smoke 门的例外拦
    的是“环境是好的，但某条用例的检查在这里跑不了”（测试库未配、
    需真实 PC）。preflight 先跑，永远不让门替死环境开脱；门后跑，
    永远不让缺测试库替代码失败开脱。
  - `server/internal/evaluation/smoke_cases_test.go`：
    `checkSmokeCases` 增加例外规则的表级强制：manual 检查必须配
    `needs=real-pc`；`needs=CH_TEST_DATABASE_URL` 只允许
    `internal/store/postgres` 的 go-test 检查（PG 用例）；go-test 检
    查配 `real-pc` 直接拒绝（会悄悄跳过执行）。坏表用例新增三项
    （manual 无 real-pc needs、库 needs 落在非 PG 包、go-test 配
    real-pc needs），真实表仍通过。
  - 新增 `server/internal/evaluation/smoke_gate_fixture_test.go`：
    两个永远跳过的夹具测试（顶层直接 skip；两个子测试全 skip、父测
    试本身通过），只为让门有一个真实的“无证据”样本；包测试里只显
    示为 SKIP，不影响任何通过/失败。
  - 新增 `server/internal/evaluation/smoke_gate_test.go`：
    `TestSmokeGateAccounting` 跑真实 `smoke.sh`（夹具表 6 行：1 通过、
    2 例外、映射丢失、全跳过、子用例全跳过），断言退出码 1、记账
    pass=1/fail=3/exception=2、每行状态与 detail 措辞、且记录里不
    出现 blocked/missing 状态；
    `TestSmokeGateExceptionsDoNotFail` 跑只有例外的夹具表，断言退出
    0、exception=2、pass=0（例外不是通过）。
  - 新增 `server/internal/evaluation/testdata/smoke-gate-fixture.tsv`
    与 `testdata/smoke-gate-fixture-exceptions.tsv`：上述两张夹具表
    （用例 ID 用 `FIX-*`，与真实 T01–T43 区分，永不用于真实部署）。
- 逻辑是什么：
  - “无证据即失败”有三条触发线：映射丢失（`no tests to run`）、全
    跳过（顶层测试只 skip）、子用例全跳过（有子测试时只看子测试
    叶，父测试的 PASS 行不算证据）。三条都经过真实 `go test -v`
    输出判定，不靠退出码。
  - 例外只有两条自动检测：测试库未配（PG 用例）与需真实 PC
    （manual/real-pc）。例外不拦门，但记进记录、打进 stderr 通知、
    记账里单独计数，部署日志里一眼看得出哪些证据缺着。
  - 表级再加一道防线：例外理由写错位置（manual 不配 real-pc、库
    needs 配到非 PG 包、go-test 配 real-pc）由
    `TestSmokeCasesTableMatchesRealTests` 在门禁测试里拦下，不等到
    部署时才发现。
- 边界（Codex 不该碰的部分）：
  - 例外条件不要扩：只认上面两条自动检测。不要加“测试太慢”“环境
    不稳”“先放行”之类的人工例外；新的例外理由必须先改表检查器与
    记账测试，再改 `smoke.sh`。
  - 叶级解析不要退回“数 PASS 行”：父测试通过而子测试全跳过的夹具
    就是反例（`TestSmokeGateFixtureSubtestsSkipped` 的父测试印
    `--- PASS`，两个子测试印 `--- SKIP`），数 PASS 行会误判通过。
  - `smoke-record.json` 的记账键是 `{pass, fail, exception}`，不要
    加回 `blocked`/`missing`/`skip` 键（下游读记录只认这三个）。
  - `update.sh` 的 preflight 与 smoke 门的顺序不要换（preflight 先
    跑，门后跑），两层例外门的注释不要删。
  - 夹具测试（`TestSmokeGateFixture*`）与夹具表（`FIX-*`）只供
    `TestSmokeGateAccounting` 用，不要把它们写进真实
    `smoke-cases.tsv`。
- flow-doc 待同步：无。smoke 门是部署脚本层面的口径变化，未改
  Harness 路由/会话/任务流；设计方案 §16.3 的“三态”描述与本节不
  一致时以本节为准（原始三态已被本批 supersede），flow 文档本身不
  动。
- 部署后待做（需真实环境，本轮未做）：配好 `CH_TEST_DATABASE_URL`
  后重跑 `make smoke`，T01/T26/T36 应由 `exception` 变为真实
  pass/fail；在真实 PC 上跑 T10/T43，例外清零；`update.sh` 全流程
  部署时确认例外通知出现在部署日志里。
- 验证结果：`go build ./...` 通过；`go vet ./internal/evaluation/`
  通过；`gofmt -l` 在改动文件上无输出；`go test -race` 指定测试
  （表映射、表检查器坏行 10 项、记账、例外不拦门、两个跳过夹具）
  全部通过；`go test -race ./internal/evaluation/` 全过。真实
  `make smoke` 实跑（2026-11-25，HEAD `9e796ec`）：exit 0，
  `pass=5 fail=0 exception=5`，例外通知打印在 stderr（T01/T26/T36
  缺测试库，T10/T43 需真实 PC），通过的 5 条与 E10 基线一致
  （T04/T06/T13/T19/T22）；记录 `deploy/single/smoke-record.json`
  的 head 为 `9e796ec`、counts 为 `{pass:5, fail:0, exception:5}`。
  注意：`make smoke` 的目标在 `server/Makefile`，根目录无此目标。

## P3-4 44 条周套件（live 套件与 8 条合成合同层分开）

- 缺口：核查报告 §一 16.4 行（未验证：周套件只有 8 条）与 §三第 4
  项（每周 44 条未补齐）；增强方案无单项编号（属 §16.4 验收）；
  设计方案 §16.4（每周 44 条 100% PASS：44 = PC-CHAT 001–024 +
  20 条专测分类失败、报错恢复、审批、成本、多轮合并；证据是
  `server/internal/evaluation/testdata/suite-live-v1.json` + 每周周
  报；评测套件是 Morde 原生 + Inspect AI，不依赖 OTel）。
- 审计结果（动手前核实，live 套件确实不存在）：
  - `server/internal/evaluation/testdata/` 里只有 `suite-v1.json`
    （8 条合成用例），`suite-live-v1.json` 不存在。
  - 8 条合成层不能直接扩成 44 条：它的校验器只认 `AIC-{H,C,E}-NN`
    形 ID、P0/P1 两档优先级、`data_class` 强制 `synthetic`——而
    live 用例的 ID 是 PC-CHAT-001–024 与 SPEC-xxx、优先级含 P2
    （PC-CHAT-015 在源文档里就是 P2）、数据是真实执行不是合成。
    拓宽共享校验器会动摇冻结的合同层，所以 live 套件另建结构、
    另建校验，与合成层分开跑、分开算分。
  - 20 条专测无全文可抄：设计方案 §16.4 与补充设计 §3 只有分组
    （分类失败、报错恢复、审批、成本、多轮合并）与验收提示，无逐
    条 input/expected。20 条按已知故障模式新写，每条的 `source`
    字段注明来源。
- 改了哪些文件：
  - 新增 `server/internal/evaluation/testdata/suite-live-v1.json`
    （版本 `morde-pc-chat-live-v1.0.0`，44 条，无 rubric、无分数）：
    PC-CHAT 001–024 按 `pc-chat-merged-candidate.md` 逐条 condensed
    （步骤压成 input、预期压成 expected_behavior，优先级与分组沿
    用源文档：001–007 基础、008–013 复杂任务、014–016 会话与长历
    史、017–019 审批卡、020–022 事件链与 Workspace 交付、023–024
    成本与用量）；SPEC-CLF-01–05（分类失败）、SPEC-ERR-01–05（报
    错恢复）、SPEC-APR-01–04（审批）、SPEC-COST-01–03（成本）、
    SPEC-MRG-01–03（多轮合并）按已知故障模式新写——短消息 3 字节
    超限（PR #2）、长消息守卫提前拦截（PR #3）、DeepSeek 分类 8 次
    5 次 invalid_output（2026-11-19 实测）、P3-1 重试梯子、服务边
    界四种告知、P3-6 重复错误沉淀、E05 审批卡证据化、R22 审批卡双
    处出现、hermes 成本账与 fail-closed 预算门、merge_notice 同话
    题合并。每条都有 `source` 字段说明出处。
  - 新增 `server/internal/evaluation/live_suite.go`：
    `LiveSuite`/`LiveCase`（id/priority/group/input/
    expected_behavior/source）与 `LoadLiveSuite`/`ValidateLiveSuite`
    （DisallowUnknownFields + 拒绝尾随 JSON；版本与 44 条固定 ID
    顺序、ID 与 group 的对应关系、P0/P1/P2、文本有界，任一项不符
    即拒绝）。
  - 新增 `server/internal/evaluation/live_suite_test.go`：
    `TestLoadLiveSuite`（版本 + 44 个 ID 与固定顺序全等 + 11 个
    抽查点的 priority/group/source）；`TestLoadLiveSuiteRejectsInvalidSuites`
    （重复、错序、错 group、P3、空 input、缺 source、缺一条、未知
    字段、尾随 JSON、畸形 JSON 共 10 项全拒绝）；
    `TestLiveSuiteStatus`（状态文件 44 行、与套件同序、状态只认
    manual-runnable/blocked、每行有原因、计数 36/8、表头含周自动
    跑 0/44 与两条原因）；
    `TestLiveSuiteIsSeparateFromSyntheticSuite`（live 文件不能用
    `LoadSuite` 加载、两套版本与条数都不同，跑起来的分数不会串）。
  - 新增 `server/internal/evaluation/live-suite-status.md`：逐条标
    手动可跑/阻塞 + 原因。判定口径写在表头：手动可跑 = 今天在用户
    PC + 现线上服务器上能由人工执行（只依赖用例自身点名的前置条
    件）；阻塞 = 有已知缺口挡着、连执行都执行不了。结果：
    manual-runnable 36 / blocked 8 / passed 0——没有任何一条标通
    过，通过只能由真实执行判定、写进周报。8 条阻塞：005（内置搜
    索未配置）、015（需约 25 万 tokens 多轮对话、手工无法构造）、
    016（两个测试账号未确认备好）、CLF-03 与 ERR-01（需故障注入、
    无手动注入手段）、ERR-05（依赖 P3-6，本批后一项才做）、APR-03
    （审批超时机制未核实）、COST-03（预算上限配置入口未核实）。
    周自动跑 0/44 可跑写进表头：无 Inspect AI runner、Judge key
    未配。
- 逻辑是什么：
  - live 套件是“每周要跑的题单”，不是“已经跑过的成绩单”：文件
    里只有题目（input/expected/source），没有分数；状态文件里只有
    “今天能不能动手跑”，没有通过。两处都不设“通过”这个值，防
    止把“写进来了”当成“跑过了”。
  - 清单测试锁死 44 个 ID、顺序、分组与优先级落点；状态测试锁死
    44 行状态与 36/8 计数。任一增删改序、分组错位、状态文件漏行
    都会先在门禁测试里失败。
  - live 与合成层分开：不同的文件、不同的加载器、不同的版本、不同
    的条数。合成层（8 条）继续走盲测 + 独立评审的合同口径；live
    层（44 条）走每周真跑 + 周报口径。
- 边界（Codex 不该碰的部分）：
  - 不要把 live 用例塞进 `suite-v1.json`，也不要拓宽 `suite.go`
    的 `validCaseID`/优先级/`data_class` 校验去容纳 live ID——两
    层分开是故意的，串了就分不清“合成合同分”与“每周真跑分”。
  - 不要在套件文件或状态文件里写任何一条 `passed`：通过只能由
    真实执行判定。状态变化时改状态表 + 同步改测试里的 36/8 计数
    锁，不要只改一处。
  - SPEC 用例的 `source` 字段不要删：20 条是新写的，来源（故障
    模式出处）是它们唯一的依据；改 input/expected 时同步改 source。
  - PC-CHAT-015 保持 P2（源文档原样），不要为了“统一成 P0/P1”
    改优先级；live 校验器认 P0/P1/P2 三档是故意的。
- flow-doc 待同步：无。评测套件是执行证据层，未改 Harness 路由/
  会话/任务流；设计方案 §16.4 点名的证据文件路径与本节一致。
- 部署后待做（需外部条件，本轮未做）：建 Inspect AI runner + 配
  Judge key，跑第一次周自动跑（当前 0/44 可跑）；人工抽跑 36 条
  manual-runnable 并写第一份周报（通过/失败/跳过/阻塞 + 证据，
  失败进 R14 回归）；清掉 8 条阻塞的缺口（搜索配置、长对话构造
  脚本、测试账号、故障注入入口、P3-6、审批超时核实、预算配置核
  实）后再补跑。
- 验证结果：`go build ./...` 通过；`go vet ./internal/evaluation/`
  通过；`gofmt -l` 在改动文件上无输出；`go test -race` 指定测试
  （live 加载、10 项拒绝、状态文件、与合成层分离）全部通过；
  `go test -race ./internal/evaluation/` 全过。live JSON 经 python
  独立校验：44 条、无重复 ID、优先级 P0×13/P1×30/P2×1、分组计数
  与设计一致、文本字段无首尾空白、无超长。

## P3-5 E08 HTTP/SSE 端口（事件链回放能通过接口读到）

- 缺口：核查报告 §三第 11 项（E08 事件链无 HTTP/SSE 端口）；
  增强方案 E08；设计方案 §16.1（任务事件链可追溯）与 §16.5（事件
  链本体 9 种）。写侧 OpenAPI `TaskEvent` 仍是 11 种枚举不动，9 种
  本体只做读侧投影。
- 审计结果（动手前核实，P2 的 E08 只做了回放逻辑，端口确实没有）：
  - 回放逻辑已存在且有测试：`server/internal/app/
    task_event_ontology.go`（`ProjectTaskEventChain`：11 种写侧事
    件 → 9 种本体，delivery 只在 reviewed PASS/PASS+NOTES 且回执
    已发布时出现；FAIL  verdict 永不产 delivery）与
    `server/internal/store/postgres/task_event_chain.go`（`Store.
    GetTaskEventChain`：repeatable-read 快照内读事件 +  verdict +
    回执事实再投影），ontology 测试已覆盖完整链与 delivery 的四种
    反例。
  - 但链到不了 PC：`TaskStore` 接口有 `GetTaskEventChain`，除
    postgres 实现外零调用方——app 层 `TaskService` 没有对应方法，
    API 层 `TaskService` 接口没有，路由没有，OpenAPI 没有。已有的
    `GET /tasks/:id/events`（`streamTaskEvents`）只流原始 11 种事
    件；`task_store.go` 注释原话就是“nine-kind replayable chain
    (E08, read-side only; no HTTP/SSE yet)”。
- 改了哪些文件：
  - `server/internal/app/task_service.go`：新增 `TaskService.
    GetTaskEventChain`，与 `ListEvents` 同一套守卫（principal、
    任务 ID 合法、GetTask 确权、内部工具任务按 not-found 隐藏、
    `authorizeTaskRead`）。revoked 时返回 not-found 而不是退化链，
    注释写明原因：空链不是合法投影（链必须以 created 开头），没有
    安全的退化形状可回，not-found 不泄漏任何信息。
  - `server/internal/api/tasks.go`：`TaskService` 接口增加
    `GetTaskEventChain`；注册 `GET /tasks/:id/event-chain` 与
    `GET /tasks/:id/event-chain/stream`。
  - `server/internal/api/task_events.go`：新增 `getTaskEventChain`
    （JSON 回放：`{task_id, nodes[], unmapped_sequences[]}`，集合
    恒为数组、永不为 null；拒绝 query/body；node 保留 source
    sequence/source type/detail/created_at，客户端可逐节点追回存
    储事件或事实）与 `streamTaskEventChain`（SSE：首帧恒为完整链
    `event: task.event_chain`；任务未终态时每秒重投影一次，链变了
    才发新完整快照——verification/delivery 落在旧 sequence 上也
    能送达；重连即重发完整链，无 cursor；任务终态、重投影失败或客
    户端断开时结束；15 秒 keep-alive）。写侧 11 种枚举与原有
    `streamTaskEvents` 均未动。
  - `server/openapi/portal.yaml`：新增两条路径（`getTaskEventChain`、
    `streamTaskEventChain`）与两个 schema（`TaskEventChain`、
    `TaskEventChainNode`，kind 枚举 9 种）；`TaskEvent` 的 11 种枚
    举一字未动。
  - `server/internal/app/task_service_test.go`：新增
    `TestTaskServiceGetTaskEventChainReplaysThroughService`（stub
    事件经真实投影回放：9 个事件派生节点、kinds 与 unmapped
    `[3 11 12 13 14]`、无 verdict 事实时无 verification/delivery；
    空 principal、非法 ID、缺失任务、内部工具任务、revoked 访问
    五项守卫）。
  - `server/internal/api/tasks_test.go`：`taskServiceStub` 增加
    chain/chains/chainCalls/chainErr/chainErrAfterCall 与
    `GetTaskEventChain`（复用 ListEvents 的 after-call 语义）。
  - 新增 `server/internal/api/task_events_test.go`：
    `TestGetTaskEventChainReturnsCompleteChain`（11 个节点全序、
    delivery 在末尾、unmapped 原样返回数组）；
    `TestGetTaskEventChainOmitsDeliveryOnFailedVerdict`（FAIL
    verdict + 已发布回执：接口返回 verification、不编造 delivery
    的反例——夹具链用真实投影算出，不是手写 JSON）；
    `TestGetTaskEventChainRejectsBadRequests`（缺失任务 404、非法
    ID 与 query 400、被拒请求不到达 service）；
    `TestStreamTaskEventChainSendsSnapshotAndStopsOnTerminal`
    （终态任务只发一帧快照即结束）；
    `TestStreamTaskEventChainSendsChangedSnapshotOnPoll`（首帧无
    delivery、第二帧 delivery 落在旧 sequence 14 上仍送达，重投
    影失败时流结束）；
    `TestTaskEventChainOpenAPIContract`（两 operationId、两路径、
    两 schema、9 种 kind 枚举、unmapped/nodes 字段形状；并断言
    TaskEvent schema 仍在）。
- 逻辑是什么：
  - 分层与 P2 的 E08 一致：store 读事实 → app 投影 → API 只做
    鉴权、整形与传输；API 层不判断“该不该有 delivery”，只原样返
    回投影结果。delivery 的有无由投影规则决定，接口层无法编造
    也无法吞掉。
  - GET 是回放（PC 从头查到尾的一次性读），SSE 是跟踪（链变了才
    推新快照）。SSE 不设 cursor：快照本身就是重同步点，重连即重
    发完整链；变化检测按整链 JSON 比较，verification/delivery 这
    类落在旧 sequence 上的节点也不会漏。
  - 读权限与 `ListEvents` 同口径；内部工具任务与 revoked 访问一
    律 not-found；query/body 一律拒绝（与 `getTaskStepDetail` 同
    口径的只读投影端点）。
- 边界（Codex 不该碰的部分）：
  - 不要给写侧加第 12 种事件、不要给 `TaskEvent` 加 kind：9 种本
    体只在读侧投影里存在，投影规则改了会影响每个任务的回放（规
    则见 `task_event_ontology.go` 顶部的固定映射表）。
  - 不要把 `getTaskEventChain` 的空节点/unmapped 改成 null：集合
    字段恒为数组是 server/AGENTS.md §4.2 的契约，测试用原始 body
    字符串锁死。
  - SSE 的变化检测不要改成“只比节点数”：delivery 落在旧
    sequence 上时节点数 +1 但“最后一个节点”没变，按整链 JSON
    比较才不会漏；不要给 SSE 加 cursor——快照即重同步点。
  - revoked 访问返回 not-found 是故意的（没有合法的空链形状可
    回），不要改成返回空链或 403 明文。
  - `streamTaskEvents`（原始 11 种事件流）不要动：PC 上已有客户
    端在用它，两条流并存，各司其职。
- flow-doc 待同步：无。新增的是只读查询端点，未改 Harness 路由/
  会话/任务流；设计方案 §16.1/§16.5 的“事件链可追溯”现在有端口
  可验，flow 文档本身不动。
- 部署后待做（需真实环境，本轮未做）：在真实 PC 上对一个已完成
  的真实任务调 `GET /tasks/:id/event-chain`，核对链与任务详情页
  一致；开着 SSE 跑一次真实任务，确认 approval→execution→
  verification→delivery 各帧按序到达；PG 集成测试
  `TestPostgresTaskEventChainReplaysCompleteFixTask` 需
  `CH_TEST_DATABASE_URL` 才能跑（本地无库，未跑）。
- 验证结果：`go build ./...` 通过；`go vet ./internal/app/
  ./internal/api/` 通过；`gofmt -l` 在改动文件上无输出；
  `portal.yaml` 经 python yaml 解析通过（278 条路径，新旧 schema
  齐全，`TaskEvent` 11 种枚举未动）；`go test -race` 指定测试
  （service 层回放 + API 层 6 项）全部通过；
  `go test -race ./internal/api/` 全过；`go test ./internal/app/`
  全量仅 4 个失败，全部是基线既有失败（与 P3-1/P3-2 同一组：
  `TestAgentWorkspaceFilesAndHostUseTheSameDirectory`、
  `TestApplyLiveProjectUploadMovesUnchangedRenamedFileInSharedDirectory`、
  `TestQuotaSettlePersistsOwnerStoragePauseBeforeAborting`、
  `TestRuntimeToolExecutorVerifyAgainstWorkspaceFileHandler`，工作
  区路径/环境问题，与本次改动无关）。

## P3-6 重复错误沉淀（记下、可查、可转检查项的最小可用机制）

- 缺口：核查报告 §三第 14 项（重复错误无沉淀机制）；设计方案
  §16.7（同一错误第二次出现时，已有对应的 lint / 检查 / skill；
  证据：第二次出现时的 lint/检查/skill 记录 + 任务账本；没有即不
  通过。Morde 自定验收，非 Muse 证据）。P3-1 遗留：执行器梯子跨
  pause 的次数统计缺口，明确留给本项。
- 审计结果（动手前核实，跨任务的记录与转化路径确实没有）：
  - 已有的是任务内推导：E09 的
    `DeriveTaskValidationCorrectionFacts`（`task_validation_facts.go`）
    从一个任务的 stored steps 里认“同一 (tool, failure reason)
    失败两次”，并用后来的成功 step 验 correction；它的注释原话
    是“no corrections table in this base”，即沉淀成可复用的
    lint/check/skill 无法在此验证。调用方只有 review 落库时
    （`store/postgres/task_validation_review.go`）与单测，无跨任
    务记录、无查询入口、无检查项类型。
  - E04 的根因记录是 plan 层的（`RootCause` 在任务简报里，修 bug
    任务的证据结构），不是重复错误登记处；全仓无 `CheckItem` /
    `LintRecord` / `CorrectionRecord` 类型。
  - 所以本项做的是“记录 + 暴露”，不是“接进已有系统”：机制本
    身新建，持久化、喂数点、检查项去向三处待定，见下。
- 改了哪些文件：
  - 新增 `server/internal/app/repeated_error_ledger.go`：
    `RepeatedErrorLedger`（进程内内存账本，`sync.Mutex` 保护，并
    发安全）+ `RepeatedErrorRecord` / `RepeatedErrorCheckItem`。
    签名是归一化的 `(Kind, Tool, Reason)`：Kind 分 `tool`（工具
    step 失败：Tool 是工具名）与 `model`（模型失败：Tool 是模型
    调用来源 executor/compaction/review，Reason 是梯子原因）；
    Tool/Reason 存 trimmed 值，空值无法建 key、直接拒绝（不把不
    同未知原因塌进同一个桶）。
    - 记：`Record`（一次出现；同一 (task, sequence) 重记是幂等
      回放，只留一次）；`RecordTaskSteps`（工具 feed：扫一个任务
      的 failed steps 记账，返回本次之后已重复的记录；无工具名、
      无原因、无可用 sequence 的失败记入 skipped 计数，不静默丢
      也不用空 key 记）；`RecordModelFailure`（模型 feed：P3-1
      梯子用，一次模型失败即一次出现）。
    - 查：`Find`（按签名取记录副本）、`IsRepeated`（≥2 次）、
      `List`（按首次出现顺序）、`TaskAttempts`（某任务内的出现
      次数——P3-1 梯子要的跨 pause 次数）。
    - 转：`ConvertToCheckItem`（fail-closed：未记录 → not-found；
      只有 1 次 → invalid-state；已转过 → 返回同一 item，不重复；
      新转时生成确定性 ID `rec-` + 签名 SHA-256 前 16 位 hex，同
      一签名永远转成同一 item，标题按 kind 生成）。
    - 记录上的 `FirstSeenAt` / `LastSeenAt` / `TaskAttempts` 由
      occurrence 列表算出；返回的记录都是深拷贝，调用方改不动账
      本内态。
  - 新增 `server/internal/app/repeated_error_ledger_test.go`：
    记下与查询（两次出现变 repeated、跨任务累计、幂等重记、不同
    reason 分桶、List 首次出现顺序、First/LastSeen）；
    拒绝不可 key 的错误（空 kind/tool/reason、空任务、0/负
    sequence、零时间、首尾空白归一化）；
    `RecordTaskSteps`（重复对返回、单次失败不返回、成功 step 不
    计数、3 类不可 key 记 skipped=3、空任务/零时间拒绝）；
    与 E09 口径对照（单次/重复/不同 reason/无名工具四组一致，另
    锁死一处有意的分歧：E09 把空 reason 也算“按工具重复”，ledger
    把空 reason 的 step 记 skipped、不建 key）；
    转化（未记录 → not-found、单次 → invalid-state、重复转出
    确定性 ID 与标题、再转返回同一 item、记录状态变 converted）；
    模型 feed（同一任务两次 rate_limited → `TaskAttempts` 为 2，
    即跨 pause 次数；换任务为 0；sequence 0 拒绝；标题为模型口
    径）；并发安全（10 个 goroutine 各记一次，-race 下仍为 10
    次）。
- 逻辑是什么：
  - “重复”的定义与 E09 一致：同一签名出现 ≥2 次。第一次出现只
    记账不声张；第二次出现时记录本身（签名 + 每次出现的任务、
    sequence、时间）就是 §16.7 要的“任务账本”证据，转化出的
    check item 就是“检查”记录。
  - 转化是 fail-closed 的：单次失败转不成检查项（一次失败不是重
    复错误），没记过的签名转不成（无中生有），转过的不重复生成
    （确定性 ID 保证同一签名同一 item）。
  - 两个 feed 共用同一账本、同一签名空间：工具失败与模型失败不
    会串（Kind 不同），但查询与转化是同一套。P3-1 梯子要的跨
    pause 次数就是 `TaskAttempts`：pause 时记一次，resume 后再
    pause 时读到的就是之前累计的次数。
- 边界（Codex 不该碰的部分）：
  - 不要把空 Tool/Reason 的失败记进账本：测试锁死了“空 reason
    分歧”——E09 任务内推导可以按工具算重复，账本不行；放宽会让
    不同未知原因塌进同一个桶、误报重复。
  - 不要把 `ConvertToCheckItem` 的确定性 ID 改成随机 ID：同一签
    名多次转化必须返回同一 item，否则“已转过”去重失效；也不要
    删“单次不可转”的 fail-closed 检查。
  - 不要悄悄给 ledger 加持久化或喂数点：持久化（corrections 表）
    与喂数点（review 落库时还是 validation 服务里）是待定项，
    见下；加之前先定 owner 与生命周期。
- 待定（本批不猜，留给用户/后续决定）：
  - 持久化：ledger 现在是进程内内存账本，重启即失。§16.7 要的
    “第二次出现时的 lint/检查/skill 记录”是持久证据，需要一张
    corrections 表（E09 注释里点的缺口）或等价落盘处；表结构、
    归属、迁移都未定。
  - 喂数点：`RecordTaskSteps` 与 `RecordModelFailure` 已暴露，
    但没有 live 调用方。候选点是 review 落库时（store 层调
    `DeriveTaskValidationCorrectionFacts` 处）与 validation 服务
    里；ledger 归谁持有（TaskService？worker？进程全局？）与生
    命周期未定，定了才能接线——接线前不要在 worker 里 new 一个
    各自为政的账本。
  - 执行器跨 pause 接线（P3-1 遗留）：机制已就绪（模型 feed +
    `TaskAttempts`），但 `pauseCloudDSHModelRecovery` 尚未改读
    它：梯子现在用的 attempts 仍只来自 diagnostic 的
    `LadderAttempts`（DSH 失败无此字段时首次 pause 恒为一次重
    试）。接线时 pause 处记一次模型失败、决策前读
    `TaskAttempts`，与现有 diagnostic 口径的优先级需一并定。
  - 检查项去向：`ConvertToCheckItem` 产出的 item 目前没有消费
    方——没有 checks 系统、skill 注册表或 lint 落点接收它。去向
    定了之后，转化才算端到端闭环；现在转化只产出值并标记记录已
    转，不伪装成“已落地为检查”。
- flow-doc 待同步：无。新增的是纯内存账本与纯函数，未改 Harness
  路由/会话/任务流；设计方案 §16.7 的验收仍需持久化 + 真实任务
  证据才算通过，本文只做到机制级。
- 部署后待做（需真实任务与待定决定，本轮未做）：定 corrections
  表与 ledger 归属并接线喂数点；跑一次真实重复错误任务（同一工
  具同一原因失败两次），确认第二次出现时账本里有记录、可查到、
  可转出检查项；执行器接线后跑一次跨 pause 的模型失败，确认第
  二次 pause 时梯子读到累计次数。
- 验证结果：`go build ./...` 通过；`go vet ./internal/app/`
  通过；`gofmt -l` 在改动文件上无输出；`go test -race` 指定测试
  （账本记/查/转、拒绝不可 key、RecordTaskSteps、E09 口径对照、
  模型 feed 跨 pause 计数、并发安全）全部通过；
  `go test ./internal/app/` 全量仅 4 个失败，全部是基线既有失败
  （与 P3-1/P3-2/P3-5 同一组，工作区路径/环境问题，与本次改动无
  关）。
