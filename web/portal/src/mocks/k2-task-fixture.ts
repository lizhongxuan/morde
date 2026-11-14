import type { components } from "../api/schema";
import type { MainConversation } from "../app/api";
import type { K2TaskProjection } from "../app/k2-task-projection";

type Task = components["schemas"]["Task"];
type Conversation = components["schemas"]["Conversation"];
type ConversationMessage = components["schemas"]["ConversationMessage"];

export type K2FixtureTask = Task & K2TaskProjection;

export function createK2TaskFixture(ownerID: string, modelID: string, timestamp: string) {
  const conversation: Conversation = {
    id: "c0000001", scope: "personal", is_main: true, title: "K2 并发任务展示验收",
    created_at: timestamp, updated_at: timestamp,
  };
  const messages: ConversationMessage[] = [
    { id: "m0000001", conversation_id: conversation.id, sequence: 1, role: "user", content: "任务 A：整理资料并继续运行。",
      task_id: "q0000001", delivery_status: "applied", created_at: timestamp },
    { id: "m0000002", conversation_id: conversation.id, sequence: 2, role: "user", content: "任务 B：并行核对来源。",
      task_id: "q0000002", delivery_status: "applied", created_at: timestamp },
    { id: "m0000003", conversation_id: conversation.id, sequence: 3, role: "assistant", content: "任务 B 已整理完成；一个子工作失败，已保留其余有效结果。",
      sources: [{ user_message_id: "m0000002", task_id: "q0000002", task_revision: 2, event_sequence: 9,
        content: "任务 B 已整理完成；一个子工作失败，已保留其余有效结果。" }],
      delivery_status: "applied", created_at: timestamp },
    { id: "m0000004", conversation_id: conversation.id, sequence: 4, role: "user", content: "任务 C：等任务 B 的下一修订后再汇总。",
      task_id: "q0000003", delivery_status: "applied", created_at: timestamp },
    { id: "m0000005", conversation_id: conversation.id, sequence: 5, role: "user", content: "任务 D：检查失败分支。",
      task_id: "q0000004", delivery_status: "applied", created_at: timestamp },
  ];
  const budget = { active_seconds: 600, tool_calls: 20, model_tokens: 4000 };
  const usage = { active_seconds: 12, tool_calls: 4, model_tokens: 720 };
  const taskBase = (id: string, objective: string, status: Task["status"], eventSequence: number, currentRevision: number,
    desiredRevision: number, currentStep: string | null, stepCount: number, completedStepCount: number,
    result: string | null, finishedAt: string | null) => ({
    id, owner_id: ownerID, initiator_id: ownerID, execution_location: "server" as const,
    conversation_id: conversation.id, model_id: modelID, objective, status, event_sequence: eventSequence,
    current_revision: currentRevision, desired_revision: desiredRevision, current_step: currentStep,
    step_count: stepCount, completed_step_count: completedStepCount, budget, usage,
    created_at: timestamp, updated_at: timestamp, result, waiting_reason: null,
    steps: [], batch_items: [], artifacts: [], pending_request: null, clarification_request: null,
    stop_request: null, assistant_message: null, finished_at: finishedAt,
  });

  const tasks: K2FixtureTask[] = [
    {
      ...taskBase("q0000001", messages[0]!.content, "running", 7, 2, 2, "s0000001", 2, 1,
        "任务 A 原始结果：资料已整理一半。", null),
      steps: [
        { id: "s0000001", sequence: 1, attempt: 1, retryable: false, kind: "subagent", status: "running", title: "整理来源 A", summary: "继续处理中。", created_at: timestamp },
        { id: "s0000002", sequence: 2, attempt: 1, retryable: false, kind: "subagent", status: "succeeded", title: "抽取来源 B", summary: "已返回原始材料。", created_at: timestamp },
      ],
      dependencies: [],
      child_jobs: [
        { job_id: "child-a-running", task_step_id: "s0000001", title: "整理来源 A", task_revision: 2, status: "running" },
        { job_id: "child-a-done", task_step_id: "s0000002", title: "抽取来源 B", task_revision: 2, status: "succeeded" },
      ],
      result_review: { state: "processing", source_task_id: "q0000001", source_task_revision: 2,
        source_event_sequence: 7, reply_message_id: null },
    },
    {
      ...taskBase("q0000002", messages[1]!.content, "completed", 9, 2, 2, null, 2, 1,
        "任务 B 原始结果：有一项来源未通过校验。", timestamp),
      steps: [
        { id: "s0000003", sequence: 1, attempt: 1, retryable: false, kind: "subagent", status: "succeeded", title: "交叉核对来源", summary: "结果已返回。", created_at: timestamp },
        { id: "s0000004", sequence: 2, attempt: 1, retryable: false, kind: "subagent", status: "failed", title: "校验补充来源", summary: "远端样本缺少必要字段。", created_at: timestamp },
      ],
      dependencies: [],
      child_jobs: [
        { job_id: "child-b-done", task_step_id: "s0000003", title: "交叉核对来源", task_revision: 2, status: "succeeded" },
        { job_id: "child-b-failed", task_step_id: "s0000004", title: "校验补充来源", task_revision: 2, status: "failed" },
      ],
      result_review: { state: "published", source_task_id: "q0000002", source_task_revision: 2,
        source_event_sequence: 9, reply_message_id: "m0000003" },
      assistant_message: { id: "m0000006", conversation_id: conversation.id, task_id: "q0000002", role: "assistant",
        task_revision: 2, result_event_sequence: 9, content: "任务 B Task 快照结果（不应重复展示）", status: "completed", created_at: timestamp },
    },
    {
      ...taskBase("q0000003", messages[3]!.content, "queued", 1, 0, 1, null, 0, 0, null, null),
      dependencies: [{ task_id: "q0000002", required_revision: 3, task_status: "completed", satisfied: false }],
      child_jobs: [], result_review: null,
    },
    {
      ...taskBase("q0000004", messages[4]!.content, "failed", 4, 1, 1, null, 1, 0,
        "任务 D 原始结果：执行失败前的部分输出。", timestamp),
      steps: [{ id: "s0000005", sequence: 1, attempt: 1, retryable: false, kind: "subagent", status: "failed", title: "执行失败分支", summary: "子工作返回错误。", created_at: timestamp }],
      dependencies: [],
      child_jobs: [{ job_id: "child-d-failed", task_step_id: "s0000005", title: "执行失败分支", task_revision: 1, status: "failed" }],
      result_review: { state: "failed", source_task_id: "q0000004", source_task_revision: 1,
        source_event_sequence: 4, reply_message_id: null, trigger_message_id: "m0000005" },
    },
  ];

  const mainConversation: MainConversation = { status: "ready", conversation_id: conversation.id, version: 1 };
  return { conversation, mainConversation, messages, tasks };
}
