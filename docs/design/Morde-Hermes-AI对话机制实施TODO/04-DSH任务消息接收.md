# H04 DSH 任务消息接收

> 状态：完成。H03 的 v3 Worker 发送端已与 H04 Node Runtime 在本地工作区联调；H03 其他路由与 PostgreSQL 验收继续由 H03 TODO 跟踪。

## 修改边界与接口

独占 `runtime/morde-cloud-runtime/src/engine.mjs` 中任务消息接收、`ipc.mjs`、`account-runtime.mjs`、`test/runtime.test.mjs` 及本任务新增测试。H02 合入后以其最新版本为基础，保留压缩、检查点和子 Agent 改动。不要修改服务端 Go、PC/iOS 文件。

已有 `message_delivery` v1/v2 由接收端使用 `agent.followup()`。实现总览中拟定的 v3 `mode`，保持 v1/v2 行为；使用已安装 DSH 的 `agent.steer()`、`agent.followup()` 和持久收件箱，不新建 Agent Loop。

## 任务

- [x] **H04-1（完成）接入投递模式**：在 Runtime 帧校验、账号 Runtime 分发和 Agent 接收端贯通 v3；`steer` 进入 `next-step`，`followup` 进入 `next-turn`。保留附件、消息 ID 和完整身份栅栏；不信任调用方指定的其他 Session。
  - 验收：运行中的工具完成后，轻量补充在下一步边界被领取；v1/v2 仍按原逻辑运行；错误模式、错误身份和授权版本被拒绝。
- [x] **H04-2（完成）持久化与去重**：同 ID 同内容/附件/模式重放只保留一份；冲突内容或模式拒绝。以 DSH inbox 插入及 `user/message` 检查点为入队和领取证据；Runtime 刷盘/确认不能阻塞接收检查点 ACK 的事件处理。
  - 验收：覆盖投递前崩溃、插入后 ACK 丢失、领取前取消、领取后重启和重复帧；结果未知时不伪造完成。
- [x] **H04-3（完成）保持现有执行能力**：验证压缩、工具调用、子 Agent、任务取消和恢复的隔离；只向指定 Task 的根 Agent 投递，不改变其他并发 Task。
- [x] **H04-4（完成）交接并验证**：Node Runtime 定向回归、v3 帧及持久检查点样例已记录；Go `CloudDSHExecutor` 投递/恢复用例通过真实 Node Runtime 进程验证，v3 发送端及 PostgreSQL 持久化检查点用例通过。H03 更广的路由 + PostgreSQL 端到端验收继续由 H03 TODO 跟踪。

## 尽量移植hermes-agent/项目的代码
Hermes-agent有很多写好的函数,特别是在harness这一块都是比较强的,能移植过来的话,能减少工作量.


## 完成证据

`2026-11-21` 本地验证（Node `v26.13.0`、Go `go1.27.1 darwin/arm64`、Morde Engine `0.2.0-rc.2`、DSH `0.2.2-rc.1`）：

- `runtime/morde-cloud-runtime/`：`npm run build:dsh-compaction` 通过；`node --test test/runtime.test.mjs` 通过，135/135。`node --test --test-concurrency=1 test/compaction.test.mjs test/compaction-recovery.test.mjs test/hermes-compaction-window.test.mjs` 通过，16/16。Runtime 用例覆盖 v1/v2 兼容、v3 `steer`/`followup` 的 `next-step`/`next-turn`、完整栅栏、相同/冲突重放、投递前重启重投、inbox ACK 丢失恢复、领取后 ACK 丢失恢复、领取前取消、拒绝检查点时不报完成、取消和检查点 ACK 并行处理、并发 Task 投递隔离，以及既有压缩、工具、子 Agent 和恢复路径。
- `server/`：`CH_TEST_DATABASE_URL=<本地临时 PostgreSQL 17>` 下，Go Worker 定向组合测试 7/7 通过：包含 v3 帧发送、4 个真实 `bin-account.mjs` 子进程投递/恢复用例，以及 PostgreSQL + Node Runtime 账户运行时用例。PostgreSQL 消息回执、待处理消息和 Agent 检查点用例 3/3 通过。实际命令见下方。
- H07 复验：`runtime/morde-cloud-runtime/ npm test` 全量通过，152/152；本地 PostgreSQL 下完整 `go test -timeout=45m ./internal/worker -count=1` 通过（255.844s）。
- H07 修复：终态前台子 Agent 的已核对工具结果现在通过内部 `child_session.settled_output` 从 Go 服务端传回 Runtime，重放时保留 `action_refs` 等完整结果；Runtime 仅接受终态前台子 Agent 的有界字符串结果。`TestCloudDSHPersonalChildRecoveryAfterNodeRestart` 定向通过，完整 Worker 回归也通过。

H03 当前发送帧样例（身份值为测试值）：

```json
{
  "type": "message_delivery",
  "version": 3,
  "task_id": "task_123",
  "owner_id": "user_123",
  "agent_id": "agent_123",
  "generation": 2,
  "authorization_version": 3,
  "session_id": "session_123",
  "message_id": "m1234567",
  "content": "add a brief note",
  "attachment_ids": [],
  "mode": "steer"
}
```

ACK 丢失用例捕获到的持久检查点事件（省略正文和其他事件）：

```json
{
  "type": "agent/inbox/spliced",
  "data": {
    "target": "next-step",
    "inserted": [{
      "id": "m7654321",
      "role": "user",
      "source": { "kind": "user", "morde_delivery_mode": "steer" }
    }]
  }
}
```

恢复后领取会在同一持久事件流追加 `user/message`，保留同一消息 ID 与 `source.morde_delivery_mode`；测试断言 inbox 插入和领取各恰好一次。Node 编译和 Runtime 回归命令：

```bash
cd runtime/morde-cloud-runtime
npm run build:dsh-compaction
node --test test/runtime.test.mjs
```

Go Worker v3 发送端验收命令：

```bash
cd server
CH_TEST_DATABASE_URL=<本地临时 PostgreSQL 17 DSN> go test ./internal/worker -run '^(TestCloudDSHMessageDeliveryUsesV3ModeAndDeduplicatesByMode|TestCloudDSHExecutorDeliversPendingConversationMessageOnce|TestCloudDSHExecutorDeliversMessageDuringModelCall|TestCloudDSHExecutorRecoversCompletedCheckpointWithPendingMessage|TestCloudDSHExecutorRecoversCheckpointAfterRuntimeCrashBeforeAck|TestAccountRuntimePostgresAndRealNodeContinueAfterFirstTaskContextEnds|TestAccountRuntimePostgresAndRealNodeRecoverAfterManagerRestart)$' -count=1 -v
CH_TEST_DATABASE_URL=<本地临时 PostgreSQL 17 DSN> go test ./internal/store/postgres -run '^TestPostgres(ConversationReceiptsAcrossTaskStates|PendingConversationMessagesBlockTaskCompletion|AgentCheckpointAppliesOnlyMatchingRootMessages)$' -count=1 -v
```

H04-1 至 H04-4 均已完成。H07 使用 Go Worker + 本地 Node Runtime + 隔离 PostgreSQL 重跑 `TestConversationSummaryDSHMainAndSideChatsContinueAfterLongHistoryRestart`，通过；此前 `message_too_long` 失败记录已被本轮通过结果取代。子 Agent 恢复的结果不匹配也已按上文修复并通过完整 Worker 回归。以上均为本地合成模型/测试数据，不构成真实 Provider 或 PC/iOS 端到端验收。
