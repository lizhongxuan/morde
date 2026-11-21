# H02 DSH 上下文压缩

> 状态：已完成（2026-11-21；Provider 默认输出上限的数据流已交接 H07）。依赖：无。先阅读[总览与协作契约](00-总览与协作契约.md)和[精简设计](../Morde-Hermes-AI对话机制精简设计方案.md)。

## 修改边界与接口

独占 `runtime/morde-cloud-runtime/src/engine.mjs` 中压缩接入、必要的新增压缩模块、`test/compaction.test.mjs`、本任务新增的压缩测试，以及 `vendor/deepseek-harness/packages/compaction/compaction-basic/src/index.ts` 的最小选窗扩展点。`engine.mjs` 整文件在 H02 完成前不由 H04 修改。服务端跨 Task 摘要由 H01 负责。

保留现有 DSH `CompactionEngine`、检查点和服务端压缩请求/响应语义。当前 `BasicCompactionEngine` 已提供计量、裁剪和压缩事务，但子类钩子主要是 `summarize()`；参数无法表达 Hermes 窗口规划时，在 DSH 接口内实现兼容插件，同一 Session 只能启用一个压缩决策实现。

## 任务

- [x] **H02-1（完成）锁定上游行为**：`context_compressor.py`、`context_compressor_window.py` 和 `LICENSE` 的 SHA-256 与总览一致；下文记录实际复用、差异和限制。保留原 MIT 声明。
- [x] **H02-2（完成）移植压缩路径**：接入实际模型窗口与已传入的输出预留、Hermes 小/大窗口阈值和 Lean 尾部预算；复用 DSH TokenMeter、ToolResultPruner、检查点事务及 Morde 模型网关。以 DSH surface 映射 Hermes 头部、用户/助手锚点和工具组边界；固定合成模型的 Runtime 用例核对选窗和保留消息。
  - 验收：相同输入与固定摘要模型下，对照上游窗口/保留结果；旧工具结果可按现有授权记录恢复，调用和结果配对完整。
- [x] **H02-3（完成）补齐失败与恢复**：保留原始事件；覆盖摘要截断、空输出、失败、Provider 溢出回退、持久重试/重启恢复和取消。复用 DSH 事务对并发 surface 变更的稳定性检查；摘要超时作为模型失败由同一事务关闭失败标记，不替换历史。
  - 验收：失败、重试、重复触发、上下文溢出、模型切换及重启后检查点恢复；没有安全范围时不误报压缩成功。
- [x] **H02-4（完成）回归 DSH 能力**：完整 `test/runtime.test.mjs` 通过，覆盖 Session、检查点、工具、子 Agent、取消、恢复和并发隔离。
- [x] **H02-5（完成）交接**：记录源码版本/哈希、适配差异、验证命令及 H07 输出预留数据交接；无需新增 IPC 字段，未修改 H01/H03 文件。

## 尽量移植hermes-agent/项目的代码
Hermes-agent有很多写好的函数,特别是在harness这一块都是比较强的,能移植过来的话,能减少工作量.


## 完成证据

### 上游和实现记录

- 本地 Hermes 快照与总览哈希一致：`agent/context_compressor.py` 为 `498871a5b8c0f3a8b53c2547fb25867f32357c62d8dbf81fdd3c0f17275f065f`；`agent/context_compressor_window.py` 为 `8497d0b5b802e5a9a2ff5d74a0d4e52b3ba40e0ed5aa335b6263315a182af0fa`；`gateway/run_turn_hygiene.py` 为 `b5e59f36c1cefa1aacf19ff272e0603aadac200417e4cf84365559145a1204c4`；`LICENSE` 为 `821556e6336796450ab852d375117b48a4887e71d255794fd6318d99982a5ab6`（MIT）。
- DSH 上游快照为 `project/deepseek-harness` commit `fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159`（2026-11-04）；Runtime 包和锁文件固定 DSH `0.2.2-rc.1`。本任务开始时 Morde `HEAD` 为 `5f97110cf74ffe57aefc9be4664d713de3fbd62d`。工作区原本已有其他未提交改动，未使用清理或重置命令。
- DSH `BasicCompactionEngine` 只开放 `summarize()` 子类钩子，原选窗函数不可覆盖。vendored `src/index.ts` 增加默认仍调用 DSH 选择器的 `selectRange()` 扩展点；Morde 自动压力和溢出处理在此 DSH 引擎内接入 Hermes 窗口，不改依赖版本、锁文件或公开 IPC。
- `hermes-compaction-window.mjs` 按 Hermes 的 512K 窗口分界、64K 触发下限、85% 小窗口上限和 Lean 尾部预算计量。使用 DSH TokenMeter 的 surface 顺序，并通过 `toolPairingBalancedBefore()` 保持工具调用/结果边界；保留首轮头部、最近用户输入和助手回复，摘要之后允许头部保护衰减，子 Agent 固定保留第一条任务简报。
- 压缩前的 DSH `ToolResultPruner` 只缩短模型投影，append-only 原始事件保留。对摘要范围内的裁剪工具结果，通过 DSH `sourceEventSeqs` 还原原始消息；完整结果导致 Provider 窗口超限时最多回退一次到裁剪投影，并明确写入覆盖不足。摘要必须正常结束，且只含非空文本并符合字节上限；DSH 事务负责来源稳定性、取消和提交。
- 自动压力在有限次 DSH 压缩后仍高于阈值时失败返回；最近失败后冷却 60 秒，上下文溢出恢复可以绕过冷却。独立 coordinator `purpose=compaction` 在新建且关闭自动压缩的 Session 中汇总授权来源，因此该 `compactNow` 使用 DSH 默认强制窗口，不经过实时对话窗口规划。
- 适配差异：没有移植 Hermes 依赖 Transcript/本地归档的多用户锚点配置、超长活动轮次拆分、重启交接探测、工具结果外置归档/恢复指针或完整反抖状态机。DSH 保留最近一条真实用户输入和助手回复；摘要指令要求硬约束使用最短必要用户原话，但没有 Hermes 独立的逐用户原文区段。Morde 摘要输出上限仍为 `min(2048, max(256, contextWindow * 0.0625))`，与 Hermes 摘要预算策略不同。
- Worker 优先使用会话 `maxTokens`、Adapter `defaultMaxTokens`，再用已有 `start.max_tokens` 计算输出预留。当前 `WorkerModel.resolveModel()` 不暴露 Provider 默认输出上限；三者都缺失时按零预留，窗口阈值可能偏高。现有 IPC 已能表达 `max_tokens`，无需新增字段；H07 应在上游已知 Provider 上限时填入实际值。Worker 每次 start 固定授权一个模型；模型切换到其他 Provider/model 会被拒绝，因此没有可执行的跨模型压缩路径可做端到端验证。

### 本地验证

在 `runtime/morde-cloud-runtime/` 执行：

- `npm run build:dsh-compaction`：通过，esbuild 生成 vendored DSH compaction bundle。
- `node --test test/compaction.test.mjs test/hermes-compaction-window.test.mjs test/hermes-compaction-runtime.test.mjs test/compaction-recovery.test.mjs`：17/17 通过；覆盖真实 Runtime pressure 触发、选窗和保留、裁剪工具结果原文来源链、失败/截断/Provider 溢出回退、重试、重启恢复与取消。
- `node --test test/runtime.test.mjs`：129/129 通过；覆盖既有 Session、检查点、工具、子 Agent、取消、恢复及并发隔离。

验证使用本地固定合成模型和 IPC，没有调用外部 Provider 或测试机。未单独注入实时模型切换或并发 surface 写入；模型切换受 WorkerModel 单模型授权限制，surface 稳定性/取消检查复用 DSH compaction transaction。Hermes 窗口模块可本地导入，但 ContextCompressor 直接导入因缺少 `ruamel.yaml` 依赖失败；窗口核对依据已哈希源码和固定 fixture，没有执行上游 Python compressor。
