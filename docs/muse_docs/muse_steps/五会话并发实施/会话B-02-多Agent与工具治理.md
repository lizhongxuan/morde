# 会话 B-02：多 Agent 与工具治理

**目标：**完成 R03/R12 及 R15；B-01 的身份 / 计划接口稳定后开始，不必等待其他 `01` 全部结束。先读[并发总览](<00-并发实施总览.md>)、[体验目标](<../Morde AI对话目标体验.md>)和[原方案](<../Morde 复刻 Muse 一体化实施与验收 TODO.md>)中 R03/R12/R15。

**写入范围：**B 专属的 `server/internal/app/agent_sessions.go`、`server/internal/store/postgres/agent_sessions.go`、`server/internal/app/cloud_dsh_mcp_builtin.go`、`cloud_dsh_skills.go`、`server/internal/store/postgres/cloud_dsh_skill_reads.go`、意图 / 工具与 Skill 选择专属文件及测试；`runtime/morde-cloud-runtime/` 和 `cloud_dsh_session_tree.go` 始终由 C 写，Broker / 预算 / schema 由 A 写。将 Runtime 调整写成交接需求，不自行改 C 的文件。

**设计与交接：**按[多 Agent 与共享资源](<01-复杂链路设计与交接.md#agents-tools>)绑定父子身份、工具摘要与资源互斥；按[受控 Skill 设计](<01-复杂链路设计与交接.md#memory-skill>)核对来源版本和动态提供方式，不把模型选择或 Skill 当作授权。

- [ ] **B02-1 真实协作与结果汇总。**复用 DSH 委派及 root → coordinator → worker 的既有能力；独立子任务可并行，有依赖的步骤依真实结果衔接，权限不超过父级。
  - 验收：T09–T12 覆盖两层深度、两 worker 实际时间重叠、同一 GUI / 冲突写入不无序并发、成功 / 失败 / unknown 逐级汇总、取消目标不影响独立任务和 checkpoint 恢复；真实 PC + 模型由 E/A 共同核验 T10/T43。
- [ ] **B02-2 工具选择、循环和取消。**复核 65 项 MCP 目录、模型每轮工具上限、必需工具 / 预算 / 权限及既有 `repeat-tool-reminder`。工具集恢复保持一致；无法证明底层停止时保持 unknown，不把 IPC 等待超时冒充硬取消。
  - 验收：T42 覆盖超限、必需工具不丢、授权变化、重启、取消及未知副作用不重放；缺失 Runtime 适配交 C、共享 Broker / 预算交 A。
- [ ] **B02-3 受控 Skill 候选。**先完成 B02-1/2；C-02 的 R09 记忆 / 来源 / 授权合同稳定后实施 R15。核对现有 Skill 读取与 DSH 上游 provider 的接入、版本及隔离能力，适用时优先复用；候选只在授权范围内形成、验证、启用和撤销。
  - 验收：A01 覆盖未批准不可用、明确启用后可用、撤权即时失效、不同账号 / Task 隔离；插件兼容及许可证核对记录在案。新 schema 交 A。
- [ ] **B02-4 交接给 C 和 A。**交出 Skill 候选 / 启用 / 撤销的可运行合同给 C-02 的 R16，并将协作图、资源归属和测试结果交 A-02。
  - 验收：C 可在不等本文件所有 PC 实测关闭的情况下使用已稳定合同；未运行真实模型或 PC 的部分明确待验。
- [ ] **B02-5 资料来源与工具边界（R26）。**复核 `cloud_dsh_mcp_builtin.go` 及文件/检索工具是否能传递真实来源、可用位置和不完整信息，按工具自身现有合同补缺；将 Runtime 调整交 C、权限和跨层字段交 A。
  - 验收：T57/T59 的本地受控来源与伪造授权样本中，正常分析仍可执行，资料内命令不能增加权限；无法读取和定位如实反馈。真实模型和实际 PC 分开记录。
- [ ] **B02-6 子任务监视与交接（M03-05/06）。**按[最终设计 L4](<../Morde 复刻 Muse 最终流程与开发设计.md>)核对已有子 Session、结算、后台 handoff 与恢复，补齐关键状态交回直接父 Session 的缺口；普通进度不反复唤醒，失败/取消/unknown 和必要阻塞不能被静默吞掉。B 负责子 Session 和事实组织；跨层持久身份/事务交 A，Runtime 与 checkpoint 接线交 C，用户可见终态交 D，PC 验收交 E。先交付独立可用的合同切片，不等待所有会话关闭。
  - 验收：按 M03-05 覆盖交错完成、失败与未知、取消/撤权、陈旧修订、重复事件及进程重启；父级拿到可回查事实且无重复副作用。T09–T12/T43 的实际 PC + 真实 DSH/模型另由 E/A 记录。统计比例不是验收门槛。
- [ ] **B02-7 已授权 Skill/connector 选择（L1）。**复核 `cloud_dsh_skills.go` 的 `nanomuse_skill_read` 与 `cloud_dsh_mcp_selected.go` 的 `mcp_discover` / `mcp_call`，让执行 Agent 按任务需要从当前用户/Task 可用目录选择并读取真实能力；B-01 负责语义路由，选择不授予权限、不自动创建 connector，也不要求普通问答进入 Task。自定义 Skill 的候选/确认仍归 B02-3，不以其完整验收阻塞已有受控目录使用。
  - 验收：已有授权工具可被正确选择并完成任务，用户无需记住 Skill/工具名；无匹配、未选择、撤权、目录/调用不一致时，说明缺失能力，存在可用配置入口或授权内替代时给出具体选择，不能编造工具、静默换账号/目标或越权派发。App/Broker 回归与实际 PC + DSH/模型语义分开记录。

**依赖：**B01 的相关合同及 A01 共享身份先稳定；Runtime 修改由 C 独占。R15 等 C-02 先交付 R09 合同，再把 R15 合同交 C-02 做 R16；不要求 C-02 整份文档先关闭。

**定向回归：**在 `server/` 运行 `go test ./internal/app ./internal/worker -run 'AgentSession|CloudDSHNested|DSHSkill' -count=1`；PostgreSQL 并发与真实模型 / PC 的执行单独登记。

## 交接记录

记录父子身份 / 授权与工具集合合同、Skill 输入输出、C/A 的实现请求、测试命令和真实模型 / PC 未验项。
