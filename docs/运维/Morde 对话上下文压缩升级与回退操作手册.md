# Morde 对话上下文压缩升级与回退操作手册

## 升级

1. 备份 PostgreSQL，并确认新版本的 DSH 源码、Runtime 构建、摘要恢复和账本测试已经通过。
2. 停止旧版消息路由 Worker 和 DSH Task Worker，保留 API 服务运行。新消息可以继续持久受理，但不要让新旧 Worker 同时消费同一队列。
3. 检查 `conversation_compaction_operations` 与模型用量账本。对 `unknown` 或已派发但未结算的模型调用先按现有管理员对账流程处理；不要因升级把它们直接改为失败或重放。

   ```sql
   SELECT state, count(*) FROM conversation_compaction_operations GROUP BY state ORDER BY state;
   SELECT state, count(*) FROM model_call_ledger WHERE source='compaction' GROUP BY state ORDER BY state;
   SELECT state, count(*) FROM project_budget_ledger WHERE source='compaction' GROUP BY state ORDER BY state;
   ```
4. 部署新 Server/Runtime 并应用数据库迁移，再启动新 Worker。迁移只增加摘要、来源和账本能力，原始会话消息不改写。旧会话在第一次新消息触发的压缩中从最早未覆盖来源开始分页；每个已提交摘要版本保存来源证明，后续运行从该版本继续。出现 `【历史摘要不完整】` 时说明仍有授权历史待处理，消息照常受理。
5. 检查路由队列、摘要操作状态、模型账本及 Task 恢复结果。长历史分多次压缩推进，不把部分摘要报告成完整历史。

## 回退

1. 停止新旧消息路由及 DSH Task Worker，避免同时消费或恢复同一会话。
2. 对已派发/未知的 compaction 模型调用完成管理员对账，再停止相关调用。不得删除或清空模型用量账本。
3. 回退 Server/Runtime 应用版本，但保留数据库迁移、摘要、来源证明、检查点和原始消息。不要执行摘要相关迁移的 Down；有持久数据时迁移会拒绝破坏性回退。
4. 旧版路由可按自身原有的有界历史方式继续新消息处理，完整压缩能力暂不可用。恢复旧 Task 检查点前必须先由旧版验证它的 `ENGINE_VERSION` 与事件身份；无法验证时将 Task 保持在既有安全等待/停止路径，核对外部操作后再让用户重发，不尝试重放未知工具或模型调用。
5. 回退后核对主会话指针、旁聊消息序号、待处理路由、Task 状态及模型账本。摘要和原文保留，待恢复新版本时继续按来源证明分页推进。

数据库只支持保留数据的应用版本回退，不支持把含摘要、来源或 compaction 账本的数据库降回旧 schema。回退演练应在隔离数据库和合成模型上完成，不能在生产数据上试探 `Down`。
