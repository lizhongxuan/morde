import { http, HttpResponse } from "msw";
import type { components } from "../api/schema";
import type { HostMemberPermission } from "../app/api";

type User = { id: string; name: string; role: "admin" | "user"; disabled?: boolean; quota: { hosts: number; cpu_milli: number; mem_mib: number }; usage: { hosts: number; cpu_milli: number; mem_mib: number } };
type AuditLog = { id: number; at: string; actor_id: string; actor_kind: string; action: string; target: string; detail: Record<string, unknown>; ip: string };
type Workspace = components["schemas"]["Workspace"];
type WorkspaceRef = components["schemas"]["WorkspaceRef"];
type WorkspaceBinding = components["schemas"]["WorkspaceBinding"];
type Host = Omit<components["schemas"]["Host"], "workspaces"> & { sandbox_id?: string | null; workspaces: WorkspaceBinding[] };
type HostMember = { user_id: string; permissions: HostMemberPermission[]; created_at: string };
type Task = components["schemas"]["Task"];
type TaskSummary = components["schemas"]["TaskSummary"];
type TaskActionRequest = components["schemas"]["TaskActionRequest"];
type TaskActionResponse = components["schemas"]["TaskActionResponse"];
type ExecutionControl = components["schemas"]["ExecutionControl"];
type Conversation = components["schemas"]["Conversation"];
type Message = components["schemas"]["Message"];
type ModelConnection = components["schemas"]["ModelConnection"];
type UserPreferences = components["schemas"]["UserPreferences"];
type Schedule = components["schemas"]["Schedule"];
type ScheduleRun = components["schemas"]["ScheduleRun"];
type CreateScheduleRequest = components["schemas"]["CreateScheduleRequest"];
type UpdateScheduleRequest = components["schemas"]["UpdateScheduleRequest"];
type ScheduleActionRequest = components["schemas"]["ScheduleActionRequest"];
type Notification = components["schemas"]["Notification"];
type ClientInstallation = components["schemas"]["ClientInstallation"];
type AdminWorkspace = components["schemas"]["AdminWorkspace"];
type VisibleAdminWorkspace = Omit<Workspace, "state"> & { state: "ready" | "deleting" };
type MockExecutionControl = ExecutionControl & { drainChecks?: number };

const demoUser: User = { id: "u1a2b3c4", name: "demo", role: "admin", disabled: false, quota: { hosts: 5, cpu_milli: 8000, mem_mib: 16384 }, usage: { hosts: 0, cpu_milli: 0, mem_mib: 0 } };
const memberUser: User = { ...demoUser, id: "umember1", name: "member", role: "user" };
const templates = [{ id: "std-2c4g", name: "标准 2 核 4G", description: "适合日常浏览与开发", isolation: "container", resources: { cpu_milli: 500, mem_mib: 1536, cpu_limit_milli: 2000, mem_limit_mib: 4096 }, workspace_size_mib: 20480, ttl_seconds: 28800, idle_timeout_seconds: 7200, on_idle: "disabled", keep_workspace_default: true }];
let hosts: Host[] = [];
let workspaces: Workspace[] = [];
const workspaceOwners = new Map<string, string>();
let hostMembers = new Map<string, HostMember[]>();
let apiKeys: Array<Record<string, unknown>> = [];
let tasks: Task[] = [];
let conversations: Conversation[] = [];
let conversationMessages: Message[] = [];
let conversationOwners = new Map<string, string>();
let modelConnections: ModelConnection[] = [];
let userPreferences = new Map<string, UserPreferences>();
let schedules: Schedule[] = [];
let scheduleOwners = new Map<string, string>();
let scheduleRuns = new Map<string, ScheduleRun[]>();
let notifications: Notification[] = [];
let clients: ClientInstallation[] = [];
const clientOwners = new Map<string, string>();
const pendingClients = new Map<string, { ownerId: string; platform: "desktop" | "android" | "ios"; displayName: string; wantsWorkspaceSync: boolean; expiresAt: string }>();
let notificationOwners = new Map<string, string>();
const scheduleCreateReplays = new Map<string, { request: string; response: Schedule }>();
const workspaceCreateReplays = new Map<string, { request: string; response: Workspace }>();
const hostCreateReplays = new Map<string, { request: string; response: Host }>();
const workspaceBindReplays = new Map<string, { request: string; response: WorkspaceBinding }>();
const workspaceUnbindReplays = new Map<string, { request: string; response: WorkspaceBinding }>();
const workspaceUnbindingStartedAt = new Map<string, number>();
const scheduleActionReplays = new Map<string, { request: string; response: components["schemas"]["ScheduleActionResponse"] }>();
const taskActionReplays = new Map<string, { request: string; response: TaskActionResponse }>();
const taskSubmitReplays = new Map<string, { request: string; response: Task }>();
const messageSubmitReplays = new Map<string, { request: string; response: { message: Message; task: Task } }>();
const executionControls = new Map<string, MockExecutionControl>();
const executionActionReplays = new Map<string, { request: string; response: ExecutionControl }>();
const users: User[] = [demoUser];
const passwords = new Map<string, string>();
const loginAccounts = new Map<string, { user: User; password: string }>();
const sessions = new Map<string, { userId: string; name: string }>();
const sessionStoragePrefix = "cloudhost:portal-mock-session:";
const assistantMockStorageKey = "cloudhost:portal-mock-assistant-state-v2";
let auditLogs: AuditLog[] = [];
const hostDeletionStartedAt = new Map<string, number>();
const hostReleaseStartedAt = new Map<string, number>();
const workspaceDeletionStartedAt = new Map<string, number>();
const deletionTimers = new Set<ReturnType<typeof setTimeout>>();
const executionTimers = new Set<ReturnType<typeof setTimeout>>();
const taskActionTimers = new Set<ReturnType<typeof setTimeout>>();
const capacityLimit = { cpu_milli: 48000, mem_mib: 98304 };
const allocatedStates = new Set(["creating", "running", "stopping", "deleting"]);
const deletionDelayMs = 900;

const json = (body: Record<string, unknown>, status = 200, headers?: HeadersInit) => HttpResponse.json(body, { status, headers });
const empty = (headers?: HeadersInit) => new HttpResponse(null, { status: 204, headers });
const failure = (code: string, message: string, status: number, details: Record<string, unknown> = {}) => json({ error: { code, message, details } }, status);
const id = (prefix: string) => `${prefix}${Math.random().toString(36).slice(2, 9)}`;
const now = () => new Date().toISOString();
const isVisibleAdminWorkspace = (workspace: Workspace): workspace is VisibleAdminWorkspace => workspace.state !== "deleted";
const adminWorkspaceView = (workspace: VisibleAdminWorkspace): AdminWorkspace => {
  const ownerId = workspaceOwners.get(workspace.id) ?? "";
  return {
    ...workspace,
    owner_id: ownerId,
    owner_name: users.find((user) => user.id === ownerId)?.name ?? ownerId,
    bindings: hosts.flatMap((host) => {
      const binding = host.workspaces.find((item) => item.workspace.id === workspace.id);
      return binding ? [{
        host_id: host.id,
        host_name: host.name,
        state: binding.state,
        sync_state: binding.sync_state,
        desired_revision: binding.desired_revision,
        applied_revision: binding.applied_revision,
      }] : [];
    }),
  };
};
const encodeMessageCursor = (message: Message) => btoa(`${message.created_at}\n${message.id}`).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/g, "");
const decodeMessageCursor = (cursor: string) => {
  try {
    const base64 = cursor.replace(/-/g, "+").replace(/_/g, "/");
    const decoded = atob(base64 + "=".repeat((4 - base64.length % 4) % 4));
    const separator = decoded.indexOf("\n");
    const createdAt = decoded.slice(0, separator);
    const messageID = decoded.slice(separator + 1);
    return separator > 0 && Number.isFinite(Date.parse(createdAt)) && messageID ? { createdAt, messageID } : null;
  } catch {
    return null;
  }
};
const hideOwner = (resource: Record<string, unknown>) => { const visible = { ...resource }; delete visible.owner_id; return visible; };
const toWorkspaceRef = (workspace: Workspace): WorkspaceRef => ({
  id: workspace.id, name: workspace.name, revision: workspace.revision, used_bytes: workspace.used_bytes, state: workspace.state,
});

function createDemoTasks(): Task[] {
  const timestamp = now();
  const base = {
    owner_id: demoUser.id,
    host_id: "hdemo001",
    conversation_id: "cdemo001",
    model_id: "demo-model",
    budget: { active_seconds: 1800, tool_calls: 100, model_tokens: 50000 },
    usage: { active_seconds: 72, tool_calls: 4, model_tokens: null },
    batch_items: [],
    artifacts: [],
    stop_request: null,
    clarification_request: null,
    created_at: timestamp,
    updated_at: timestamp,
    finished_at: null,
  } satisfies Omit<Task, "id" | "objective" | "status" | "current_step" | "result" | "waiting_reason" | "steps" | "pending_request">;
  return [
    {
      ...base,
      id: "qdemo001",
      objective: "整理本周项目变更并生成摘要",
      status: "running",
      current_step: "读取项目文件",
      result: null,
      waiting_reason: null,
      steps: [{ id: "splan001", sequence: 1, kind: "plan", status: "succeeded", title: "分析任务", summary: "确认了需要检查的项目目录和变更范围。", created_at: timestamp }, { id: "sread001", sequence: 2, kind: "tool", status: "running", title: "读取项目文件", summary: "正在汇总最近的文件变更。", created_at: timestamp }],
      pending_request: null,
    },
    {
      ...base,
      id: "qreview1",
      objective: "向客户发送部署完成通知",
      status: "waiting_user",
      current_step: "等待审批",
      result: null,
      waiting_reason: "approval_required",
      steps: [{ id: "splan002", sequence: 1, kind: "plan", status: "succeeded", title: "准备通知内容", summary: "已整理收件对象和待发送内容。", created_at: timestamp }, { id: "swait002", sequence: 2, kind: "wait", status: "waiting_user", title: "等待你的决定", summary: "确认后才会继续发送。", created_at: timestamp }],
      pending_request: { id: "rapprove", version: 1, kind: "approval", title: "发送部署完成通知？", object: "客户项目群", content: "部署已完成，现可开始验收。", impact: "消息会发送到外部群聊。" },
    },
    {
      ...base,
      id: "qbatch01",
      objective: "处理 3 份周报并归档",
      status: "failed",
      current_step: "归档周报",
      result: "有一份文件未能写入目标目录。",
      waiting_reason: null,
      steps: [{ id: "splan003", sequence: 1, kind: "plan", status: "succeeded", title: "读取周报", summary: "找到 3 份待归档文件。", created_at: timestamp }, { id: "swrite03", sequence: 2, kind: "tool", status: "failed", title: "写入归档目录", summary: "目标目录暂时不可用。", created_at: timestamp }],
      batch_items: [{ id: "bfailed1", key: "周报-03", status: "failed", attempt: 1, result: null, error: "目标目录暂时不可用。" }],
      pending_request: null,
      finished_at: timestamp,
    },
    {
      ...base,
      id: "qclarif1",
      objective: "生成本周项目进度报告",
      status: "waiting_user",
      current_step: "等待报告范围",
      result: null,
      waiting_reason: "clarification_required",
      steps: [{ id: "splan004", sequence: 1, kind: "plan", status: "succeeded", title: "准备进度报告", summary: "准备汇总本周项目变更和风险。", created_at: timestamp }, { id: "swait004", sequence: 2, kind: "wait", status: "waiting_user", title: "等待你补充信息", summary: "报告时间范围需要确认。", created_at: timestamp }],
      pending_request: null,
      clarification_request: { id: "rclarify", version: 1, question: "报告需要包含哪几天？是否要加入风险和下周计划？" },
    },
  ];
}

function completeDeletions() {
  const time = Date.now();
  let changed = false;
  for (const host of hosts) for (const binding of host.workspaces) {
    const key = `${host.id}:${binding.workspace.id}`;
    if (binding.state === "unbinding" && time - (workspaceUnbindingStartedAt.get(key) ?? time) >= deletionDelayMs) {
      binding.state = "detached";
      binding.binding_version += 1;
      workspaceUnbindingStartedAt.delete(key);
      changed = true;
    }
  }
  const completedHosts = hosts.filter((host) => host.state === "deleting" && time - (hostDeletionStartedAt.get(host.id) ?? time) >= deletionDelayMs);
  for (const host of completedHosts) {
    hosts = hosts.filter((item) => item.id !== host.id);
    hostDeletionStartedAt.delete(host.id);
    changed = true;
  }
  const stoppedHosts = hosts.filter((host) => host.state === "stopping" && time - (hostReleaseStartedAt.get(host.id) ?? time) >= deletionDelayMs);
  for (const host of stoppedHosts) {
    host.state = "stopped";
    host.sandbox_id = null;
    hostReleaseStartedAt.delete(host.id);
    changed = true;
  }
  const completedWorkspaces = workspaces.filter((workspace) => workspace.state === "deleting" && time - (workspaceDeletionStartedAt.get(workspace.id) ?? time) >= deletionDelayMs);
  for (const workspace of completedWorkspaces) {
    workspace.state = "deleted";
    workspace.updated_at = now();
    workspaceDeletionStartedAt.delete(workspace.id);
    changed = true;
  }
  if (changed) persistAssistantState();
}

function scheduleDeletionCompletion() {
  const timer = setTimeout(() => {
    deletionTimers.delete(timer);
    completeDeletions();
  }, deletionDelayMs);
  deletionTimers.add(timer);
}

function executionControlFor(ownerId: string): MockExecutionControl {
  let control = executionControls.get(ownerId);
  if (!control) {
    control = { state: "enabled", epoch: 1, pending_task_count: 0, drainChecks: 0 };
    executionControls.set(ownerId, control);
  }
  return control;
}

function scheduleExecutionDrain(ownerId: string) {
  const timer = setTimeout(() => {
    executionTimers.delete(timer);
    const control = executionControls.get(ownerId);
    if (!control || control.state !== "stopping") return;
    for (const task of tasks) {
      if (task.owner_id === ownerId && task.status === "cancelling") {
        task.status = "cancelled";
        task.current_step = "已停止";
        task.updated_at = now();
        task.finished_at = now();
        task.stop_request = null;
        syncScheduleRunForTask(task);
      }
    }
    control.state = "disabled";
    control.epoch += 1;
    control.pending_task_count = 0;
    control.drainChecks = 0;
    persistAssistantState();
  }, 3000);
  executionTimers.add(timer);
}

function scheduleTaskStop(task: Task) {
  const request = task.stop_request;
  if (!request || request.status !== "pending") return;
  const timer = setTimeout(() => {
    taskActionTimers.delete(timer);
    if (task.stop_request?.id !== request.id || task.status !== (request.action === "pause" ? "pausing" : "cancelling")) return;
    task.status = request.action === "pause" ? "paused" : "cancelled";
    task.current_step = request.action === "pause" ? "已暂停" : "已停止";
    task.stop_request = null;
    task.updated_at = now();
    if (task.status === "cancelled") task.finished_at = task.updated_at;
    syncScheduleRunForTask(task);
    persistAssistantState();
  }, 1000);
  taskActionTimers.add(timer);
}

type PersistedAssistantState = {
  hosts: Host[];
  workspaces: Workspace[];
  tasks: Task[];
  conversations: Conversation[];
  conversationMessages: Message[];
  conversationOwners: Array<[string, string]>;
  workspaceOwners: Array<[string, string]>;
  userPreferences: Array<[string, UserPreferences]>;
  schedules?: Schedule[];
  scheduleOwners?: Array<[string, string]>;
  scheduleRuns?: Array<[string, ScheduleRun[]]>;
  notifications?: Notification[];
  notificationOwners?: Array<[string, string]>;
  hostMembers?: Array<[string, HostMember[]]>;
  clients?: ClientInstallation[];
  clientOwners?: Array<[string, string]>;
  auditLogs?: AuditLog[];
};

function persistAssistantState() {
  if (typeof localStorage === "undefined") return;
  const state: PersistedAssistantState = {
    hosts,
    workspaces,
    tasks,
    conversations,
    conversationMessages,
    conversationOwners: Array.from(conversationOwners.entries()),
    workspaceOwners: Array.from(workspaceOwners.entries()),
    userPreferences: Array.from(userPreferences.entries()),
    schedules,
    scheduleOwners: Array.from(scheduleOwners.entries()),
    scheduleRuns: Array.from(scheduleRuns.entries()),
    notifications,
    notificationOwners: Array.from(notificationOwners.entries()),
    hostMembers: Array.from(hostMembers.entries()),
    clients,
    clientOwners: Array.from(clientOwners.entries()),
    auditLogs,
  };
  localStorage.setItem(assistantMockStorageKey, JSON.stringify(state));
}

export function resetMockState(options: { restoreAssistantState?: boolean } = {}) {
  let persisted: PersistedAssistantState | undefined;
  if (options.restoreAssistantState && typeof localStorage !== "undefined") {
    try {
      const stored = localStorage.getItem(assistantMockStorageKey);
      if (stored) persisted = JSON.parse(stored) as PersistedAssistantState;
    } catch {
      localStorage.removeItem(assistantMockStorageKey);
    }
  } else if (typeof localStorage !== "undefined") {
    localStorage.removeItem(assistantMockStorageKey);
  }
  for (const timer of deletionTimers) clearTimeout(timer);
  deletionTimers.clear();
  for (const timer of executionTimers) clearTimeout(timer);
  executionTimers.clear();
  for (const timer of taskActionTimers) clearTimeout(timer);
  taskActionTimers.clear();
  hostReleaseStartedAt.clear();
  Object.assign(demoUser, { id: "u1a2b3c4", name: "demo", role: "admin", disabled: false, quota: { hosts: 5, cpu_milli: 8000, mem_mib: 16384 }, usage: { hosts: 0, cpu_milli: 0, mem_mib: 0 } });
  Object.assign(memberUser, { ...demoUser, id: "umember1", name: "member", role: "user" });
  sessions.clear();
  hosts = [];
  hostMembers = new Map();
  workspaces = [];
  workspaceOwners.clear();
  apiKeys = [];
  tasks = createDemoTasks();
  conversations = [{ id: "cdemo001", host_id: "hdemo001", title: tasks[0]?.objective ?? "示例任务", created_at: tasks[0]?.created_at ?? now(), updated_at: tasks[0]?.updated_at ?? now() }];
  conversationMessages = tasks.map((task, index) => ({ id: `mdemo00${index + 1}`, conversation_id: task.conversation_id, role: "user", content: task.objective, task_id: task.id, created_at: task.created_at }));
  conversationOwners = new Map([["cdemo001", demoUser.id]]);
  modelConnections = [{ id: "pmodel01", name: "团队示例模型", base_url: "https://models.example.invalid/v1", model_name: "morde-demo", shared: true, has_api_key: true, capabilities: { text: true, tools: true, vision: false }, checked_at: now(), created_at: now(), updated_at: now() }];
  userPreferences = new Map();
  schedules = [];
  scheduleOwners = new Map();
  scheduleRuns = new Map();
  notifications = [];
  clients = [];
  clientOwners.clear();
  pendingClients.clear();
  pendingClients.set("ABCD2345", { ownerId: demoUser.id, platform: "desktop", displayName: "Morde 演示电脑", wantsWorkspaceSync: true, expiresAt: new Date(Date.now() + 10 * 60_000).toISOString() });
  notificationOwners = new Map();
  scheduleCreateReplays.clear();
  workspaceCreateReplays.clear();
  hostCreateReplays.clear();
  workspaceBindReplays.clear();
  workspaceUnbindReplays.clear();
  workspaceUnbindingStartedAt.clear();
  scheduleActionReplays.clear();
  taskActionReplays.clear();
  taskSubmitReplays.clear();
  messageSubmitReplays.clear();
  executionControls.clear();
  executionActionReplays.clear();
  users.splice(0, users.length, demoUser);
  passwords.clear();
  loginAccounts.clear();
  loginAccounts.set("demo", { user: demoUser, password: "demo" });
  loginAccounts.set("admin", { user: demoUser, password: "admin" });
  loginAccounts.set("member", { user: memberUser, password: "member" });
  auditLogs = [
    { id: 3, at: now(), actor_id: demoUser.id, actor_kind: "user", action: "host.create", target: "h-audit-demo", detail: { name: "示例主机" }, ip: "127.0.0.1" },
    { id: 2, at: new Date(Date.now() - 60 * 60 * 1000).toISOString(), actor_id: demoUser.id, actor_kind: "user", action: "apikey.create", target: "t-audit-demo", detail: { name: "示例 Key" }, ip: "127.0.0.1" },
    { id: 1, at: new Date(Date.now() - 48 * 60 * 60 * 1000).toISOString(), actor_id: memberUser.id, actor_kind: "user", action: "auth.login", target: memberUser.id, detail: {}, ip: "127.0.0.1" },
  ];
  hostDeletionStartedAt.clear();
  workspaceDeletionStartedAt.clear();
  if (persisted && Array.isArray(persisted.hosts) && Array.isArray(persisted.workspaces)
    && Array.isArray(persisted.tasks) && Array.isArray(persisted.conversations)
    && Array.isArray(persisted.conversationMessages) && Array.isArray(persisted.conversationOwners)
    && Array.isArray(persisted.workspaceOwners) && Array.isArray(persisted.userPreferences)) {
    hosts = persisted.hosts;
    workspaces = persisted.workspaces;
    tasks = persisted.tasks;
    conversations = persisted.conversations;
    conversationMessages = persisted.conversationMessages;
    conversationOwners = new Map(persisted.conversationOwners);
    for (const [workspaceId, ownerId] of persisted.workspaceOwners) workspaceOwners.set(workspaceId, ownerId);
    userPreferences = new Map(persisted.userPreferences);
    if (Array.isArray(persisted.hostMembers)) hostMembers = new Map(persisted.hostMembers);
    if (Array.isArray(persisted.clients) && Array.isArray(persisted.clientOwners)) {
      clients = persisted.clients;
      clientOwners.clear();
      for (const [clientId, ownerId] of persisted.clientOwners) clientOwners.set(clientId, ownerId);
    }
    if (Array.isArray(persisted.auditLogs)) auditLogs = persisted.auditLogs;
    if (Array.isArray(persisted.schedules) && Array.isArray(persisted.scheduleOwners) && Array.isArray(persisted.scheduleRuns)) {
      schedules = persisted.schedules;
      scheduleOwners = new Map(persisted.scheduleOwners);
      scheduleRuns = new Map(persisted.scheduleRuns);
    }
    if (Array.isArray(persisted.notifications) && Array.isArray(persisted.notificationOwners)) {
      notifications = persisted.notifications;
      notificationOwners = new Map(persisted.notificationOwners);
    }
    for (const workspace of workspaces) {
      const host = hosts.find((item) => item.workspaces.some((binding) => binding.workspace.id === workspace.id));
      if (host && !workspaceOwners.has(workspace.id)) workspaceOwners.set(workspace.id, host.owner_id);
    }
    let hasPendingDeletion = false;
    const resumedAt = Date.now();
    for (const host of hosts) {
      if (host.state === "deleting") {
        hostDeletionStartedAt.set(host.id, resumedAt);
        hasPendingDeletion = true;
      } else if (host.state === "stopping") {
        hostReleaseStartedAt.set(host.id, resumedAt);
        hasPendingDeletion = true;
      }
      for (const binding of host.workspaces) {
        if (binding.state === "unbinding") {
          workspaceUnbindingStartedAt.set(`${host.id}:${binding.workspace.id}`, resumedAt);
          hasPendingDeletion = true;
        }
      }
    }
    for (const workspace of workspaces) {
      if (workspace.state === "deleting") {
        workspaceDeletionStartedAt.set(workspace.id, resumedAt);
        hasPendingDeletion = true;
      }
    }
    if (hasPendingDeletion) scheduleDeletionCompletion();
  }
  if (!schedules.length) {
    const timestamp = now();
    const demoRun: ScheduleRun = { id: "ydemo001", schedule_id: "jdemo001", schedule_version: 1, occurrence_key: "demo-ongoing-run", scheduled_at: timestamp, coalesced_count: 0, status: "running", skip_reason: null, task_id: "qdemo001", observation_outcome: "unknown", created_at: timestamp, updated_at: timestamp, finished_at: null };
    schedules = [{ id: "jdemo001", version: 1, status: "enabled", host_id: "hdemo001", model_id: "demo-model", objective: "整理本周项目变更并生成摘要", timing: { kind: "daily", starts_on: timestamp.slice(0, 10), local_time: "09:00", time_zone: "Asia/Shanghai" }, budget: { active_seconds: 1800, tool_calls: 100, model_tokens: 50000 }, misfire_policy: "skip", notification_events: ["completed", "failed", "waiting_user"], observation: null, next_run_at: new Date(Date.now() + 24 * 60 * 60 * 1000).toISOString(), last_run: demoRun, created_at: timestamp, updated_at: timestamp }];
    scheduleOwners = new Map([["jdemo001", demoUser.id]]);
    scheduleRuns = new Map([["jdemo001", [demoRun]]]);
  }
  if (!notifications.length) {
    const timestamp = now();
    notifications = [
      { id: "ndemo001", kind: "waiting_user", task_id: "qreview1", event_sequence: 3, summary: "任务等待确认：发送部署完成通知？", redacted: false, actionable: true, created_at: timestamp, read_at: null },
      { id: "ndemo002", kind: "failed", task_id: "qbatch01", event_sequence: 5, summary: "任务失败：有一份文件未能写入目标目录。", redacted: false, actionable: false, created_at: new Date(Date.now() - 60 * 60 * 1000).toISOString(), read_at: null },
    ];
    notificationOwners = new Map(notifications.map((item) => [item.id, demoUser.id]));
  }
}

export function setMockWorkspaceSyncConflict(hostID: string, workspaceID: string, conflicts: string[]) {
  const host = hosts.find((item) => item.id === hostID);
  const binding = host?.workspaces.find((item) => item.workspace.id === workspaceID);
  if (!binding) throw new Error(`No mock binding for ${hostID}/${workspaceID}`);
  binding.sync_state = "conflict";
  binding.sync_conflicts = conflicts;
}

resetMockState({ restoreAssistantState: true });

const sessionCookieName = "ch_mock_session";

function preferencesFor(user: User): UserPreferences {
  return userPreferences.get(user.id) ?? { assistant_name: "Morde", selected_model_id: null, default_host_id: null, updated_at: null };
}

function onboardingStatusFor(user: User): components["schemas"]["OnboardingStatus"] {
  const preferences = preferencesFor(user);
  const availableModels = modelConnections.filter((model) => model.checked_at && model.capabilities.text && model.capabilities.tools);
  const selectedModelAvailable = availableModels.some((model) => model.id === preferences.selected_model_id);
  const modelStatus = selectedModelAvailable ? "ready" : preferences.selected_model_id ? "selected_unavailable" : availableModels.length ? "selection_required" : "not_configured";
  const ownedHosts = hosts.filter((host) => host.owner_id === user.id);
  const defaultHost = ownedHosts.find((host) => host.id === preferences.default_host_id);
  let computerStatus: components["schemas"]["OnboardingStatus"]["computer_status"];
  if (!defaultHost) computerStatus = ownedHosts.length === 0 ? "not_created" : ownedHosts.length === 1 ? "available" : "selection_required";
  else if (defaultHost.state === "creating") computerStatus = "creating";
  else if (defaultHost.state === "stopping") computerStatus = "stopping";
  else if (defaultHost.state === "running") computerStatus = "ready";
  else if (defaultHost.state === "stopped") computerStatus = "stopped";
  else if (defaultHost.state === "failed") computerStatus = defaultHost.runtime_generation > 1 ? "restore_failed" : "create_failed";
  else computerStatus = "unavailable";
  let nextAction: components["schemas"]["OnboardingStatus"]["next_action"] = "review_setup";
  if (modelStatus !== "ready") nextAction = "configure_model";
  else if (computerStatus === "not_created") nextAction = "create_or_retry_computer";
  else if (computerStatus === "selection_required" || computerStatus === "available") nextAction = "choose_computer";
  else if (computerStatus === "creating" || computerStatus === "stopping") nextAction = "wait_for_computer";
  else if (computerStatus === "stopped") nextAction = "resume_computer";
  else if (computerStatus === "create_failed" || computerStatus === "restore_failed" || computerStatus === "unavailable") nextAction = "choose_another_computer";
  else if (computerStatus === "ready") nextAction = "submit_task";
  return {
    model_status: modelStatus,
    models: availableModels.map((model) => ({ id: model.id, name: model.name, capabilities: model.capabilities })),
    selected_model_id: preferences.selected_model_id,
    computer_status: computerStatus,
    computers: ownedHosts.map((host) => ({ id: host.id, name: host.name, state: host.state as components["schemas"]["OnboardingComputer"]["state"] })),
    default_host_id: defaultHost?.id ?? null,
    can_submit: modelStatus === "ready" && computerStatus === "ready",
    next_action: nextAction,
  };
}

function sessionUser(cookies: Record<string, string>): User | null {
  const sessionId = cookies[sessionCookieName];
  let session = sessionId ? sessions.get(sessionId) : undefined;
  if (!session && sessionId && typeof localStorage !== "undefined") {
    try {
      const stored = localStorage.getItem(`${sessionStoragePrefix}${sessionId}`);
      if (stored) session = JSON.parse(stored) as { userId: string; name: string };
    } catch {
      session = undefined;
    }
  }
  if (!session) return null;
  const user = users.find((item) => item.id === session.userId) ?? (memberUser.id === session.userId ? memberUser : undefined);
  if (!user || user.disabled) return null;
  return session.name === user.name ? user : { ...user, name: session.name };
}

const unauthenticated = () => failure("unauthenticated", "请先登录", 401);
// Keep this first so protected handlers cannot run before the session check.
const sessionGuard = http.all("/api/*", ({ request, cookies }) => {
  const path = new URL(request.url).pathname;
  if (path === "/api/auth/login" || path === "/api/auth/logout" || sessionUser(cookies)) return;
  return unauthenticated();
});

const findHost = (hostId: string, user: User) => {
  completeDeletions();
  const host = hosts.find((item) => item.id === hostId);
  return host && (user.role === "admin" || host.owner_id === user.id) ? host : undefined;
};
const findTask = (taskId: string, user: User) => {
  const task = tasks.find((item) => item.id === taskId);
  return task && (user.role === "admin" || task.owner_id === user.id) ? task : undefined;
};
function createMockTask(user: User, hostID: string, modelID: string, conversationID: string, objective: string, budget?: components["schemas"]["TaskBudget"]): Task {
  const timestamp = now();
  return {
    id: id("q"), owner_id: user.id, host_id: hostID, conversation_id: conversationID, model_id: modelID,
    objective, status: "queued", current_step: null, result: null, waiting_reason: null,
    budget: budget ?? { active_seconds: 1800, tool_calls: 100, model_tokens: 50000 },
    usage: { active_seconds: 0, tool_calls: 0, model_tokens: null }, steps: [], batch_items: [], artifacts: [],
    pending_request: null, clarification_request: null, stop_request: null,
    created_at: timestamp, updated_at: timestamp, finished_at: null,
  };
}

const activeScheduleRunStatuses = new Set<ScheduleRun["status"]>(["queued", "running", "waiting_user", "pausing", "paused", "stopping"]);

function findSchedule(scheduleID: string, user: User) {
  return scheduleOwners.get(scheduleID) === user.id ? schedules.find((item) => item.id === scheduleID) : undefined;
}

function nextMockRunAt(timing: Schedule["timing"]) {
  if (timing.kind === "once") return timing.run_at;
  // shortcut: mock recurring previews use the browser TZ; the server owns IANA and DST resolution.
  const date = new Date(`${timing.starts_on}T${timing.local_time}:00`);
  for (let day = 0; day < 370; day += 1) {
    const isoDay = date.getDay() || 7;
    const eligible = date.getTime() > Date.now() && (timing.kind === "daily" || timing.weekdays.includes(isoDay));
    if (eligible) return date.toISOString();
    date.setDate(date.getDate() + 1);
  }
  return null;
}

function scheduleTaskCancellation(schedule: Schedule, task: Task) {
  if (schedule.last_run?.task_id !== task.id) return;
  scheduleTaskStop(task);
}

function syncScheduleRunForTask(task: Task) {
  const status: ScheduleRun["status"] = task.status === "cancelling" ? "stopping" : task.status;
  for (const schedule of schedules) {
    if (schedule.last_run?.task_id !== task.id) continue;
    const runs = scheduleRuns.get(schedule.id) ?? [];
    for (const run of runs) {
      if (run.task_id === task.id) {
        run.status = status;
        run.updated_at = task.updated_at;
        run.finished_at = task.finished_at ?? null;
      }
    }
    const lastRun = schedule.last_run;
    lastRun.status = status;
    lastRun.updated_at = task.updated_at;
    lastRun.finished_at = task.finished_at ?? null;
    schedule.updated_at = task.updated_at;
  }
}

function materializeHost(host: Host) {
  if (host.state === "creating" && Date.now() - new Date(host.created_at).getTime() > 900) {
    host.state = "running";
    host.state_reason = "";
  }
  return host;
}

function occupiesResources(host: Host) {
  return allocatedStates.has(host.state) || (host.state === "failed" && (Boolean(host.sandbox_id) || host.workspaces.some((binding) => binding.state !== "detached")));
}

function usageFor(ownerId: string) {
  completeDeletions();
  return hosts.filter((host) => host.owner_id === ownerId && occupiesResources(host)).reduce((usage, host) => ({
    hosts: usage.hosts + 1,
    cpu_milli: usage.cpu_milli + host.resources.cpu_milli,
    mem_mib: usage.mem_mib + host.resources.mem_mib,
  }), { hosts: 0, cpu_milli: 0, mem_mib: 0 });
}

function userWithUsage(user: User) {
  return { ...user, usage: usageFor(user.id) };
}

function capacityUsage() {
  completeDeletions();
  return hosts.filter(occupiesResources).reduce((usage, host) => ({
    cpu_milli: usage.cpu_milli + host.resources.cpu_milli,
    mem_mib: usage.mem_mib + host.resources.mem_mib,
  }), { cpu_milli: 0, mem_mib: 0 });
}

function hostsByState() {
  completeDeletions();
  return hosts.reduce<Record<string, number>>((counts, host) => {
    counts[host.state] = (counts[host.state] ?? 0) + 1;
    return counts;
  }, {});
}

export const handlers = [
  sessionGuard,
  http.post("/api/auth/login", async ({ request }) => {
    const body = await request.json().catch(() => ({})) as { name?: string; password?: string };
    const account = loginAccounts.get(body.name ?? "");
    const user = account ? users.find((item) => item.id === account.user.id) ?? account.user : users.find((item) => item.name === body.name);
    const password = account?.password ?? (user ? passwords.get(user.id) : undefined);
    if (!user || user.disabled || body.password !== password) return failure("unauthenticated", "用户名或密码错误", 401);
    const displayUser = body.name === "admin" ? { ...user, name: "admin" } : user;
    const sessionId = globalThis.crypto?.randomUUID?.() ?? id("s");
    const session = { userId: user.id, name: displayUser.name };
    sessions.set(sessionId, session);
    if (typeof localStorage !== "undefined") localStorage.setItem(`${sessionStoragePrefix}${sessionId}`, JSON.stringify(session));
    // MSW stores mocked response cookies through document.cookie; the real API sets HttpOnly.
    return json({ user: displayUser }, 200, { "Set-Cookie": `${sessionCookieName}=${sessionId}; Path=/; SameSite=Lax` });
  }),
  http.post("/api/auth/logout", ({ cookies }) => {
    const sessionId = cookies[sessionCookieName];
    if (sessionId) {
      sessions.delete(sessionId);
      if (typeof localStorage !== "undefined") localStorage.removeItem(`${sessionStoragePrefix}${sessionId}`);
    }
    return empty({ "Set-Cookie": `${sessionCookieName}=; Path=/; Max-Age=0; SameSite=Lax` });
  }),
  http.get("/api/me", ({ cookies }) => json(userWithUsage(sessionUser(cookies)!))),
  http.post("/api/client-authorizations/inspect", async ({ request, cookies }) => {
    const user = sessionUser(cookies)!;
    const body = await request.json().catch(() => ({})) as { user_code?: string };
    const pending = pendingClients.get(body.user_code ?? "");
    if (!pending || pending.ownerId !== user.id || Date.parse(pending.expiresAt) <= Date.now()) return failure("not_found", "验证码无效或已过期", 404);
    return json({ platform: pending.platform, display_name: pending.displayName, wants_workspace_sync: pending.wantsWorkspaceSync, expires_at: pending.expiresAt });
  }),
  http.post("/api/client-authorizations/approve", async ({ request, cookies }) => {
    const user = sessionUser(cookies)!;
    const body = await request.json().catch(() => ({})) as { user_code?: string; workspace_ids?: string[] };
    const pending = pendingClients.get(body.user_code ?? "");
    if (!pending || pending.ownerId !== user.id || Date.parse(pending.expiresAt) <= Date.now()) return failure("not_found", "验证码无效或已过期", 404);
    const workspaceIds = body.workspace_ids ?? [];
    if (!Array.isArray(workspaceIds) || (pending.platform !== "desktop" && workspaceIds.length) || workspaceIds.some((workspaceId) => !workspaces.some((workspace) => workspace.id === workspaceId && workspaceOwners.get(workspaceId) === user.id && workspace.state === "ready"))) return failure("invalid_argument", "工作区授权无效", 400);
    const client: ClientInstallation = { id: id("cli_"), platform: pending.platform, display_name: pending.displayName, workspace_ids: workspaceIds, expires_at: new Date(Date.now() + 30 * 86_400_000).toISOString(), revoked: false, created_at: now(), last_used_at: null };
    clients.unshift(client);
    clientOwners.set(client.id, user.id);
    pendingClients.delete(body.user_code!);
    auditLogs.unshift({ id: auditLogs.length + 1, at: now(), actor_id: user.id, actor_kind: "user", action: "client.pair", target: client.id, detail: { platform: client.platform, workspace_count: client.workspace_ids.length }, ip: "127.0.0.1" });
    persistAssistantState();
    return json(client, 201);
  }),
  http.post("/api/client-authorizations/deny", async ({ request, cookies }) => {
    const user = sessionUser(cookies)!;
    const body = await request.json().catch(() => ({})) as { user_code?: string };
    const pending = pendingClients.get(body.user_code ?? "");
    if (!pending || pending.ownerId !== user.id) return failure("not_found", "验证码无效或已过期", 404);
    pendingClients.delete(body.user_code!);
    auditLogs.unshift({ id: auditLogs.length + 1, at: now(), actor_id: user.id, actor_kind: "user", action: "client.deny", target: "", detail: { platform: pending.platform }, ip: "127.0.0.1" });
    persistAssistantState();
    return empty();
  }),
  http.get("/api/clients", ({ cookies }) => json({ items: clients.filter((client) => clientOwners.get(client.id) === sessionUser(cookies)!.id) })),
  http.delete("/api/clients/:clientId", ({ params, cookies }) => {
    const user = sessionUser(cookies)!;
    const client = clients.find((item) => item.id === params.clientId && clientOwners.get(item.id) === user.id);
    if (!client) return failure("not_found", "客户端不存在", 404);
    client.revoked = true;
    auditLogs.unshift({ id: auditLogs.length + 1, at: now(), actor_id: user.id, actor_kind: "user", action: "client.revoke", target: String(params.clientId), detail: { platform: client.platform }, ip: "127.0.0.1" });
    persistAssistantState();
    return empty();
  }),
  http.get("/api/me/preferences", ({ cookies }) => json(preferencesFor(sessionUser(cookies)!))),
  http.put("/api/me/preferences", async ({ request, cookies }) => {
    const user = sessionUser(cookies)!;
    const body = await request.json().catch(() => null) as components["schemas"]["SaveUserPreferencesRequest"] | null;
    if (!body || typeof body.assistant_name !== "string" || body.assistant_name.length > 80 || typeof body.selected_model_id !== "string") {
      return failure("invalid_argument", "用户偏好信息无效", 400);
    }
    if (body.selected_model_id && !modelConnections.some((model) => model.id === body.selected_model_id && model.checked_at && model.capabilities.text && model.capabilities.tools)) {
      return failure("invalid_argument", "所选模型尚不可用", 400);
    }
    const prior = preferencesFor(user);
    const saved: UserPreferences = { ...prior, assistant_name: body.assistant_name.trim(), selected_model_id: body.selected_model_id || null, updated_at: now() };
    userPreferences.set(user.id, saved);
    persistAssistantState();
    return json(saved);
  }),
  http.get("/api/onboarding/status", ({ cookies }) => json(onboardingStatusFor(sessionUser(cookies)!))),
  http.post("/api/me/default-host", async ({ request, cookies }) => {
    const user = sessionUser(cookies)!;
    const body = await request.json().catch(() => ({})) as { host_id?: string };
    if (body.host_id !== undefined && typeof body.host_id !== "string") return failure("invalid_argument", "云端电脑编号无效", 400);
    const ownedHosts = hosts.filter((host) => host.owner_id === user.id);
    let selected = body.host_id ? ownedHosts.find((host) => host.id === body.host_id) : ownedHosts.find((host) => host.id === preferencesFor(user).default_host_id);
    let created = false;
    if (body.host_id && !selected) return failure("not_found", "云端电脑不存在或无权访问", 404);
    if (!selected && ownedHosts.length === 1) selected = ownedHosts[0];
    if (!selected && ownedHosts.length > 1) return json({ created: false, host: null, status: onboardingStatusFor(user) });
    if (!selected && ownedHosts.length === 0) {
      const timestamp = now();
      const selectedWorkspace: Workspace = { id: id("w"), name: "我的工作区", revision: 0, used_bytes: 0, quota_bytes: templates[0]!.workspace_size_mib * 1024 * 1024, state: "ready", created_at: timestamp, updated_at: timestamp };
      selected = { id: id("h"), name: "我的云端电脑", owner_id: user.id, template_id: templates[0]!.id, isolation: templates[0]!.isolation, state: "creating", state_reason: "", runtime_generation: 1, control_version: 0, control_state: "ai", url: import.meta.env.VITE_WORKBENCH_URL ?? "http://127.0.0.1:5174", workspaces: [{ workspace: toWorkspaceRef(selectedWorkspace), directory: `projects/${selectedWorkspace.id}`, state: "active", binding_version: 1, desired_revision: selectedWorkspace.revision, applied_revision: selectedWorkspace.revision, sync_state: "healthy", sync_conflicts: [] }], resources: templates[0]!.resources, expires_at: new Date(Date.now() + templates[0]!.ttl_seconds * 1000).toISOString(), last_active_at: timestamp, created_at: timestamp };
      workspaceOwners.set(selectedWorkspace.id, user.id);
      workspaces = [selectedWorkspace, ...workspaces];
      hosts = [selected, ...hosts];
      created = true;
    } else if (selected?.state === "stopped") {
      selected.state = "creating";
      selected.state_reason = "restore_pending";
      selected.runtime_generation += 1;
      selected.created_at = now();
      delete selected.sandbox_id;
    }
    if (selected) {
      const prior = preferencesFor(user);
      userPreferences.set(user.id, { ...prior, default_host_id: selected.id, updated_at: now() });
    }
    persistAssistantState();
    return json({ created, host: selected ? { id: selected.id, name: selected.name, state: selected.state } : null, status: onboardingStatusFor(user) });
  }),
  http.get("/api/templates", () => json({ items: templates })),
  http.get("/api/hosts", ({ cookies }) => { const user = sessionUser(cookies)!; completeDeletions(); return json({ items: hosts.filter((host) => host.owner_id === user.id).map(materializeHost) }); }),
  http.get("/api/shared-hosts", ({ cookies }) => {
    const user = sessionUser(cookies)!;
    const items = Array.from(hostMembers.entries()).flatMap(([hostID, members]) => {
      const member = members.find((item) => item.user_id === user.id);
      const host = hosts.find((item) => item.id === hostID);
      return member && host && host.owner_id !== user.id && host.state !== "deleting"
        ? [{ id: host.id, name: host.name, state: materializeHost(host).state, url: `https://${host.id}.hosts.mock.invalid`, permissions: member.permissions }]
        : [];
    });
    return json({ items });
  }),
  http.post("/api/hosts/:hostId/members", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const host = hosts.find((item) => item.id === String(params.hostId) && item.owner_id === user.id && item.state !== "deleting");
    if (!host) return failure("not_found", "主机不存在", 404);
    const body = await request.json().catch(() => null) as { user_id?: unknown; permissions?: unknown } | null;
    if (!body || typeof body.user_id !== "string" || !/^u[a-z0-9]{7}$/.test(body.user_id) || body.user_id === user.id) return failure("invalid_argument", "用户编号无效", 400);
    const allowedPermissions = new Set<HostMemberPermission>(["desktop:view", "desktop:control", "terminal:control", "mcp:control"]);
    if (!Array.isArray(body.permissions) || !body.permissions.length || !body.permissions.every((permission): permission is HostMemberPermission => typeof permission === "string" && allowedPermissions.has(permission as HostMemberPermission))) return failure("invalid_argument", "共享权限无效", 400);
    const target = users.find((item) => item.id === body.user_id) ?? (memberUser.id === body.user_id ? memberUser : undefined);
    if (!target || target.disabled) return failure("not_found", "用户不存在", 404);
    const members = hostMembers.get(host.id) ?? [];
    const existing = members.find((item) => item.user_id === body.user_id);
    const permissions = Array.from(new Set(body.permissions));
    if (permissions.includes("desktop:control") && !permissions.includes("desktop:view")) permissions.unshift("desktop:view");
    const member: HostMember = { user_id: body.user_id, permissions, created_at: existing?.created_at ?? now() };
    hostMembers.set(host.id, existing ? members.map((item) => item.user_id === body.user_id ? member : item) : [...members, member]);
    persistAssistantState();
    return json(member, 201);
  }),
  http.get("/api/hosts/:hostId/members", ({ params, cookies }) => {
    const user = sessionUser(cookies)!;
    const host = hosts.find((item) => item.id === String(params.hostId) && item.owner_id === user.id && item.state !== "deleting");
    return host ? json({ items: hostMembers.get(host.id) ?? [] }) : failure("not_found", "主机不存在", 404);
  }),
  http.delete("/api/hosts/:hostId/members/:userID", ({ params, cookies }) => {
    const user = sessionUser(cookies)!;
    const hostID = String(params.hostId);
    const host = hosts.find((item) => item.id === hostID && item.owner_id === user.id && item.state !== "deleting");
    if (!host) return failure("not_found", "主机不存在", 404);
    const members = hostMembers.get(hostID) ?? [];
    const next = members.filter((item) => item.user_id !== String(params.userID));
    if (next.length === members.length) return failure("not_found", "共享成员不存在", 404);
    hostMembers.set(hostID, next);
    persistAssistantState();
    return empty();
  }),
  http.post("/api/hosts/:hostId/observation-ticket", ({ params, cookies, request }) => {
    const user = sessionUser(cookies)!;
    const host = hosts.find((item) => item.id === String(params.hostId));
    const search = new URL(request.url).searchParams;
    const targets = search.getAll("target");
    if (search.size > 1 || targets.length > 1) return failure("invalid_argument", "入口类型无效", 400);
    const target = targets[0] ?? "desktop";
    if (target !== "desktop" && target !== "terminal") return failure("invalid_argument", "入口类型无效", 400);
    const member = hostMembers.get(host?.id ?? "")?.find((item) => item.user_id === user.id);
    const authorized = host && (host.owner_id === user.id || (target === "desktop"
      ? member?.permissions.some((permission) => permission === "desktop:view" || permission === "desktop:control")
      : member?.permissions.includes("terminal:control")));
    if (!host || !authorized || host.state === "deleting") return failure("not_found", "主机不存在", 404);
    if (materializeHost(host).state !== "running") return failure("invalid_state", "主机尚未运行", 409);
    const url = new URL("/__ch/auth", `https://${host.id}.hosts.mock.invalid`);
    url.searchParams.set("t", `mock-${host.id}-${Date.now()}`);
    if (target === "terminal") url.searchParams.set("next", "/__ch/terminal/");
    return json({ url: url.href, expires_at: new Date(Date.now() + 60_000).toISOString() });
  }),
  http.post("/api/hosts", async ({ request, cookies }) => {
    const user = sessionUser(cookies)!;
    completeDeletions();
    const body = await request.json().catch(() => ({})) as { name?: string; template_id?: string; workspace_ids?: string[] };
    const idempotencyKey = request.headers.get("Idempotency-Key") ?? "";
    if (!/^[A-Za-z0-9._~-]{8,128}$/.test(idempotencyKey)) return failure("invalid_argument", "需要有效的 Idempotency-Key", 400);
    if (body.workspace_ids !== undefined && (!Array.isArray(body.workspace_ids) || body.workspace_ids.some((workspaceId) => typeof workspaceId !== "string") || new Set(body.workspace_ids).size !== body.workspace_ids.length)) {
      return failure("invalid_argument", "workspace_ids 必须是无重复的工作区编号列表", 400);
    }
    const template = templates.find((item) => item.id === body.template_id) ?? templates[0]!;
    const workspaceIDs = [...(body.workspace_ids ?? [])].sort();
    const normalizedRequest = JSON.stringify({ template_id: template.id, name: body.name?.trim() || template.name, workspace_ids: workspaceIDs });
    const replayKey = `${user.id}\u0000${idempotencyKey}`;
    const prior = hostCreateReplays.get(replayKey);
    if (prior) {
      if (prior.request !== normalizedRequest) return failure("idempotency_conflict", "幂等键已用于不同的主机创建请求", 409);
      return json(materializeHost(prior.response), 201);
    }
    const selectedWorkspaces = (body.workspace_ids ?? []).map((workspaceId) => workspaces.find((item) => item.id === workspaceId && workspaceOwners.get(item.id) === user.id));
    if (selectedWorkspaces.some((workspace) => !workspace || workspace.state !== "ready")) return failure("workspace_busy", "工作区不存在、正在删除或无权访问", 409);
    const usage = usageFor(user.id);
    const quotaCheck = [
      ["hosts", usage.hosts, 1, user.quota.hosts],
      ["cpu_milli", usage.cpu_milli, template.resources.cpu_milli, user.quota.cpu_milli],
      ["mem_mib", usage.mem_mib, template.resources.mem_mib, user.quota.mem_mib],
    ] as const;
    const quotaExceeded = quotaCheck.find(([, used, requested, limit]) => used + requested > limit);
    if (quotaExceeded) return failure("quota_exceeded", "已达到用户主机配额上限", 409, { dimension: quotaExceeded[0], limit: quotaExceeded[3], used: quotaExceeded[1], requested: quotaExceeded[2] });
    const allocated = capacityUsage();
    const capacityExceeded = (["cpu_milli", "mem_mib"] as const).find((dimension) => allocated[dimension] + template.resources[dimension] > capacityLimit[dimension]);
    if (capacityExceeded) return failure("capacity_exceeded", "集群资源容量不足", 409, { dimension: capacityExceeded, limit: capacityLimit[capacityExceeded], allocated: allocated[capacityExceeded], requested: template.resources[capacityExceeded] });
    const timestamp = now();
    const host: Host = { id: id("h"), name: body.name?.trim() || template.name, owner_id: user.id, template_id: template.id, isolation: template.isolation, state: "creating", state_reason: "", runtime_generation: 1, control_version: 0, control_state: "ai", url: import.meta.env.VITE_WORKBENCH_URL ?? "http://127.0.0.1:5174", workspaces: selectedWorkspaces.filter((workspace): workspace is Workspace => Boolean(workspace)).map((workspace): WorkspaceBinding => ({ workspace: toWorkspaceRef(workspace), directory: `projects/${workspace.id}`, state: "active", binding_version: 1, desired_revision: workspace.revision, applied_revision: workspace.revision, sync_state: "healthy", sync_conflicts: [] })), resources: template.resources, expires_at: new Date(Date.now() + template.ttl_seconds * 1000).toISOString(), last_active_at: timestamp, created_at: timestamp };
    hosts = [host, ...hosts];
    hostCreateReplays.set(replayKey, { request: normalizedRequest, response: host });
    persistAssistantState();
    return json(host, 201);
  }),
  http.get("/api/hosts/:hostId", ({ params, cookies }) => {
    const host = findHost(String(params.hostId), sessionUser(cookies)!);
    return host ? json(materializeHost(host)) : failure("not_found", "主机不存在", 404);
  }),
  http.delete("/api/hosts/:hostId", ({ params, cookies }) => {
    const host = findHost(String(params.hostId), sessionUser(cookies)!);
    if (!host) return failure("not_found", "主机不存在", 404);
    if (host.state === "deleting") return failure("invalid_state", "主机正在删除", 409, { state: host.state });
    host.state = "deleting";
    hostDeletionStartedAt.set(host.id, Date.now());
    scheduleDeletionCompletion();
    return json(host, 202);
  }),
  http.post("/api/hosts/:hostId/release", ({ params, cookies }) => {
    const host = findHost(String(params.hostId), sessionUser(cookies)!);
    if (!host) return failure("not_found", "主机不存在", 404);
    materializeHost(host);
    if (host.state === "stopping" || host.state === "stopped") return json(host, 202);
    if (host.state !== "running") return failure("invalid_state", "主机当前不能释放运行资源", 409, { state: host.state });
    host.state = "stopping";
    host.state_reason = "user_release";
    hostReleaseStartedAt.set(host.id, Date.now());
    scheduleDeletionCompletion();
    return json(host, 202);
  }),
  http.post("/api/hosts/:hostId/restore", ({ params, cookies }) => {
    const host = findHost(String(params.hostId), sessionUser(cookies)!);
    if (!host) return failure("not_found", "主机不存在", 404);
    if (host.state === "creating" && host.runtime_generation > 1) return json(host, 202);
    if (host.state === "running") return json(host, 202);
    if (host.state !== "stopped") return failure("invalid_state", "主机当前不能恢复", 409, { state: host.state });
    host.state = "creating";
    host.state_reason = "restore_pending";
    host.runtime_generation += 1;
    host.created_at = now();
    host.expires_at = new Date(Date.now() + 8 * 60 * 60 * 1000).toISOString();
    delete host.sandbox_id;
    return json(host, 202);
  }),
  http.post("/api/hosts/:hostId/renew", ({ params, cookies }) => { const host = findHost(String(params.hostId), sessionUser(cookies)!); if (!host) return failure("not_found", "主机不存在", 404); if (host.state === "deleting") return failure("invalid_state", "主机正在删除", 409, { state: host.state }); host.expires_at = new Date(Date.now() + 8 * 60 * 60 * 1000).toISOString(); return json(host); }),
  http.post("/api/hosts/:hostId/enter", ({ params, cookies }) => { const host = findHost(String(params.hostId), sessionUser(cookies)!); if (!host) return failure("not_found", "主机不存在", 404); if (host.state === "deleting") return failure("invalid_state", "主机正在删除", 409, { state: host.state }); const url = new URL("/__ch/auth", host.url); url.searchParams.set("t", `mock-${host.id}-${Date.now()}`); return json({ url: url.href, expires_at: new Date(Date.now() + 60_000).toISOString() }); }),
  http.get("/api/workspaces", ({ cookies }) => { const user = sessionUser(cookies)!; completeDeletions(); return json({ items: workspaces.filter((workspace) => workspaceOwners.get(workspace.id) === user.id && workspace.state !== "deleted") }); }),
  http.get("/api/sites", ({ cookies }) => { sessionUser(cookies)!; return json({ items: [], next_cursor: null }); }),
  http.get("/api/sites/:siteId/releases", () => failure("not_found", "网站不存在", 404)),
  http.post("/api/workspaces", async ({ request, cookies }) => {
    const user = sessionUser(cookies)!;
    const idempotencyKey = request.headers.get("Idempotency-Key") ?? "";
    if (!/^[A-Za-z0-9._~-]{8,128}$/.test(idempotencyKey)) return failure("invalid_argument", "Idempotency-Key 无效", 400);
    const body = await request.json().catch(() => null) as { name?: string } | null;
    if (!body || typeof body.name !== "string" || !body.name.trim() || body.name.length > 100) return failure("invalid_argument", "工作区名称必须为 1 到 100 个字符", 400);
    const serializedRequest = JSON.stringify({ name: body.name.trim() });
    const replayKey = `${user.id}:${idempotencyKey}`;
    const replay = workspaceCreateReplays.get(replayKey);
    if (replay) return replay.request === serializedRequest ? json({ ...replay.response }, 201) : failure("idempotency_conflict", "幂等键已用于其他请求", 409);
    const timestamp = now();
    const workspace: Workspace = { id: id("w"), name: body.name.trim(), revision: 0, used_bytes: 0, quota_bytes: templates[0]!.workspace_size_mib * 1024 * 1024, state: "ready", created_at: timestamp, updated_at: timestamp };
    workspaces = [workspace, ...workspaces];
    workspaceOwners.set(workspace.id, user.id);
    workspaceCreateReplays.set(replayKey, { request: serializedRequest, response: workspace });
    persistAssistantState();
    return json(workspace, 201);
  }),
  http.post("/api/hosts/:hostId/workspace-bindings", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const host = hosts.find((item) => item.id === String(params.hostId) && item.owner_id === user.id);
    if (!host) return failure("not_found", "主机不存在", 404);

    const key = request.headers.get("Idempotency-Key") ?? "";
    if (!key || key.length > 128) return failure("invalid_argument", "Idempotency-Key 无效", 400);
    const body = await request.json().catch(() => null) as { workspace_id?: unknown; expected_revision?: unknown; expected_binding_version?: unknown } | null;
    if (!body || typeof body.workspace_id !== "string" || Object.keys(body).some((field) => field !== "workspace_id" && field !== "expected_revision" && field !== "expected_binding_version")
      || !Number.isSafeInteger(body.expected_binding_version) || Number(body.expected_binding_version) < 0
      || (body.expected_revision !== undefined && (!Number.isSafeInteger(body.expected_revision) || Number(body.expected_revision) < 0))) {
      return failure("invalid_argument", "工作区绑定请求无效", 400);
    }
    const serializedRequest = JSON.stringify({ workspace_id: body.workspace_id, expected_revision: body.expected_revision, expected_binding_version: body.expected_binding_version });
    const replayKey = `${user.id}:${host.id}:${key}`;
    const replay = workspaceBindReplays.get(replayKey);
    if (replay) return replay.request === serializedRequest ? json({ ...replay.response }, 201) : failure("idempotency_conflict", "幂等键已用于其他请求", 409);

    const workspace = workspaces.find((item) => item.id === body.workspace_id && workspaceOwners.get(item.id) === user.id);
    if (!workspace) return failure("not_found", "工作区不存在", 404);
    if (workspace.state !== "ready") return failure("workspace_busy", "工作区正在删除", 409);
    if (body.expected_revision !== undefined && body.expected_revision !== workspace.revision) return failure("workspace_conflict", "工作区版本已变化", 409);
    materializeHost(host);
    if (host.state !== "running") return failure("host_not_ready", "主机尚未就绪", 409);

    const history = host.workspaces.filter((item) => item.workspace.id === workspace.id);
    const currentVersion = Math.max(0, ...history.map((item) => item.binding_version));
    if (body.expected_binding_version !== currentVersion) return failure("workspace_conflict", "工作区绑定版本已变化", 409);

    const existing = history.find((item) => item.state === "active" || item.state === "preparing");
    if (existing) {
      workspaceBindReplays.set(replayKey, { request: serializedRequest, response: existing });
      return json({ ...existing }, 201);
    }
    if (hosts.some((item) => item.workspaces.some((binding) => binding.workspace.id === workspace.id && binding.state !== "detached"))) {
      return failure("workspace_busy", "工作区已绑定到另一台主机", 409);
    }

    const bindingVersion = currentVersion + 1;
    const binding: WorkspaceBinding = {
      workspace: toWorkspaceRef(workspace), directory: `projects/${workspace.id}`, state: "active",
      binding_version: bindingVersion, desired_revision: workspace.revision, applied_revision: workspace.revision,
      sync_state: "healthy", sync_conflicts: [],
    };
    host.workspaces = [...host.workspaces, binding];
    workspaceBindReplays.set(replayKey, { request: serializedRequest, response: binding });
    persistAssistantState();
    return json(binding, 201);
  }),
  http.post("/api/hosts/:hostId/workspace-bindings/:workspaceId/retry-sync", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const host = findHost(String(params.hostId), user);
    if (!host) return failure("not_found", "主机不存在", 404);
    const workspaceID = String(params.workspaceId);
    const body = await request.json().catch(() => null) as { expected_binding_version?: unknown } | null;
    if (!body || Object.keys(body).some((field) => field !== "expected_binding_version")
      || !Number.isSafeInteger(body.expected_binding_version) || Number(body.expected_binding_version) < 1) {
      return failure("invalid_argument", "工作区同步重试请求无效", 400);
    }
    const binding = host.workspaces.filter((item) => item.workspace.id === workspaceID).sort((a, b) => b.binding_version - a.binding_version)[0];
    if (!binding || binding.state === "detached") return failure("not_found", "工作区绑定不存在", 404);
    if (binding.binding_version !== body.expected_binding_version) return failure("workspace_conflict", "工作区绑定版本已变化", 409);
    if (binding.state !== "active") return failure("workspace_busy", "工作区当前状态不能重试同步", 409);
    if (binding.sync_state === "healthy") return json(binding);
    binding.sync_state = "healthy";
    binding.sync_conflicts = [];
    persistAssistantState();
    return json(binding);
  }),
  http.delete("/api/hosts/:hostId/workspace-bindings/:workspaceId", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const host = findHost(String(params.hostId), user);
    if (!host) return failure("not_found", "主机不存在", 404);
    const workspaceID = String(params.workspaceId);
    const key = request.headers.get("Idempotency-Key") ?? "";
    if (!/^[A-Za-z0-9._~-]{8,128}$/.test(key)) return failure("invalid_argument", "Idempotency-Key 无效", 400);
    const body = await request.json().catch(() => null) as { expected_binding_version?: unknown; writers_stopped?: unknown } | null;
    if (!body || Object.keys(body).some((field) => field !== "expected_binding_version" && field !== "writers_stopped")
      || !Number.isSafeInteger(body.expected_binding_version) || Number(body.expected_binding_version) < 1 || body.writers_stopped !== true) {
      return failure("invalid_argument", "请确认写入工作区的 AI、IDE 和其他应用均已停止", 400);
    }
    const serializedRequest = JSON.stringify({ expected_binding_version: body.expected_binding_version, writers_stopped: true });
    const replayKey = `${user.id}:${host.id}:${workspaceID}:${key}`;
    const replay = workspaceUnbindReplays.get(replayKey);
    if (replay) return replay.request === serializedRequest ? json({ ...replay.response }, 202) : failure("idempotency_conflict", "幂等键已用于其他请求", 409);
    if (host.state !== "running") return failure("workspace_busy", "主机必须处于运行中才能安全解绑工作区", 409);
    const binding = host.workspaces.filter((item) => item.workspace.id === workspaceID).sort((a, b) => b.binding_version - a.binding_version)[0];
    if (!binding) return failure("not_found", "工作区绑定不存在", 404);
    if (binding.binding_version !== body.expected_binding_version) return failure("workspace_conflict", "工作区绑定版本已变化", 409);
    if (!["preparing", "active", "failed"].includes(binding.state)) return failure("workspace_busy", "工作区当前状态不能解绑", 409);
    binding.state = "unbinding";
    binding.binding_version += 1;
    workspaceUnbindingStartedAt.set(`${host.id}:${workspaceID}`, Date.now());
    workspaceUnbindReplays.set(replayKey, { request: serializedRequest, response: { ...binding } });
    persistAssistantState();
    return json(binding, 202);
  }),
  http.get("/api/workspaces/:workspaceId/sync-progress", ({ params, cookies }) => {
    const user = sessionUser(cookies)!;
    const workspace = workspaces.find((item) => item.id === String(params.workspaceId) && workspaceOwners.get(item.id) === user.id);
    return workspace ? json({ items: [] }) : failure("not_found", "工作区不存在", 404);
  }),
  http.delete("/api/workspaces/:workspaceId", ({ params, cookies }) => {
    const user = sessionUser(cookies)!;
    completeDeletions();
    const item = workspaces.find((workspace) => workspace.id === String(params.workspaceId) && workspaceOwners.get(workspace.id) === user.id);
    if (!item) return failure("not_found", "工作目录不存在", 404);
    if (item.state !== "ready" || hosts.some((host) => host.workspaces.some((binding) => binding.workspace.id === item.id && binding.state !== "detached"))) return failure("workspace_busy", "工作区仍有主机绑定或正在删除", 409);
    item.state = "deleting";
    item.updated_at = now();
    workspaceDeletionStartedAt.set(item.id, Date.now());
    persistAssistantState();
    scheduleDeletionCompletion();
    return json(item, 202);
  }),
  http.get("/api/execution", ({ cookies }) => {
    const user = sessionUser(cookies)!;
    const control = executionControlFor(user.id);
    return json({ state: control.state, epoch: control.epoch, pending_task_count: control.pending_task_count });
  }),
  http.post("/api/execution/stop-all", async ({ request, cookies }) => {
    const user = sessionUser(cookies)!;
    const idempotencyKey = request.headers.get("Idempotency-Key") ?? "";
    if (!/^[A-Za-z0-9._~-]{8,128}$/.test(idempotencyKey)) return failure("invalid_argument", "Idempotency-Key 无效", 400);
    const body = await request.json().catch(() => null) as Record<string, unknown> | null;
    if (!body || Object.keys(body).some((key) => key !== "expected_epoch") || !Number.isSafeInteger(body.expected_epoch) || Number(body.expected_epoch) < 1) {
      return failure("invalid_argument", "expected_epoch 必须是正整数", 400);
    }
    const serializedRequest = JSON.stringify(body);
    const replayKey = `${user.id}:stop:${idempotencyKey}`;
    const replay = executionActionReplays.get(replayKey);
    if (replay) return replay.request === serializedRequest ? json({ ...replay.response }) : failure("idempotency_conflict", "幂等键已用于其他请求", 409);

    const control = executionControlFor(user.id);
    if (control.epoch !== Number(body.expected_epoch)) return failure("conflict", "执行门禁版本已变化，请刷新后重试", 409, { epoch: control.epoch });
    if (control.state === "enabled") {
      control.state = "stopping";
      control.epoch += 1;
      const activeTasks = tasks.filter((item) => item.owner_id === user.id && ["queued", "running", "waiting_user", "pausing", "paused", "cancelling"].includes(item.status));
      for (const item of activeTasks) {
        item.status = "cancelling";
        item.pending_request = null;
        item.clarification_request = null;
        item.waiting_reason = null;
        item.stop_request = { id: "xstopall", action: "cancel", status: "pending", safe_after: now(), unsettled_operation_ids: [] };
        item.updated_at = now();
      }
      control.pending_task_count = activeTasks.length;
      if (activeTasks.length === 0) {
        control.state = "disabled";
        control.epoch += 1;
      } else {
        scheduleExecutionDrain(user.id);
      }
    }
    const response: ExecutionControl = { state: control.state, epoch: control.epoch, pending_task_count: control.pending_task_count };
    executionActionReplays.set(replayKey, { request: serializedRequest, response });
    return json({ ...response });
  }),
  http.post("/api/execution/enable", async ({ request, cookies }) => {
    const user = sessionUser(cookies)!;
    const idempotencyKey = request.headers.get("Idempotency-Key") ?? "";
    if (!/^[A-Za-z0-9._~-]{8,128}$/.test(idempotencyKey)) return failure("invalid_argument", "Idempotency-Key 无效", 400);
    const body = await request.json().catch(() => null) as Record<string, unknown> | null;
    if (!body || Object.keys(body).some((key) => key !== "expected_epoch") || !Number.isSafeInteger(body.expected_epoch) || Number(body.expected_epoch) < 1) {
      return failure("invalid_argument", "expected_epoch 必须是正整数", 400);
    }
    const serializedRequest = JSON.stringify(body);
    const replayKey = `${user.id}:enable:${idempotencyKey}`;
    const replay = executionActionReplays.get(replayKey);
    if (replay) return replay.request === serializedRequest ? json({ ...replay.response }) : failure("idempotency_conflict", "幂等键已用于其他请求", 409);

    const control = executionControlFor(user.id);
    if (control.epoch !== Number(body.expected_epoch)) return failure("conflict", "执行门禁版本已变化，请刷新后重试", 409, { epoch: control.epoch });
    if (control.state === "stopping") return failure("conflict", "执行门禁仍在停止中", 409);
    if (control.state === "disabled") {
      control.state = "enabled";
      control.epoch += 1;
    }
    const response: ExecutionControl = { state: control.state, epoch: control.epoch, pending_task_count: control.pending_task_count };
    executionActionReplays.set(replayKey, { request: serializedRequest, response });
    return json({ ...response });
  }),
  http.get("/api/conversations", ({ cookies }) => {
    const user = sessionUser(cookies)!;
    const visible = conversations.filter((conversation) => conversationOwners.get(conversation.id) === user.id)
      .sort((left, right) => right.updated_at.localeCompare(left.updated_at));
    return json({ items: visible, next_cursor: null });
  }),
  http.get("/api/conversations/:conversationID/messages", ({ params, cookies, request }) => {
    const user = sessionUser(cookies)!;
    const conversationID = String(params.conversationID);
    if (conversationOwners.get(conversationID) !== user.id) return failure("not_found", "会话不存在", 404);
    const query = new URL(request.url).searchParams;
    const limits = query.getAll("limit");
    const limit = Number(limits[0] ?? 50);
    if (limits.length > 1 || !Number.isInteger(limit) || limit < 1 || limit > 100) return failure("invalid_argument", "limit 无效", 400);
    const cursors = query.getAll("cursor");
    if (cursors.length > 1) return failure("invalid_argument", "cursor 无效", 400);
    const cursor = cursors[0] ? decodeMessageCursor(cursors[0]) : null;
    if (cursors[0] && !cursor) return failure("invalid_argument", "cursor 无效", 400);
    const visible = conversationMessages.filter((message) => message.conversation_id === conversationID)
      .filter((message) => !cursor || Date.parse(message.created_at) < Date.parse(cursor.createdAt) || (message.created_at === cursor.createdAt && message.id < cursor.messageID))
      .sort((left, right) => Date.parse(right.created_at) - Date.parse(left.created_at) || right.id.localeCompare(left.id));
    const descendingPage = visible.slice(0, limit + 1);
    const hasMore = descendingPage.length > limit;
    const items = descendingPage.slice(0, limit);
    return json({ items: items.slice().reverse(), next_cursor: hasMore ? encodeMessageCursor(items[items.length - 1]!) : null });
  }),
  http.post("/api/tasks", async ({ request, cookies }) => {
    const user = sessionUser(cookies)!;
    const idempotencyKey = request.headers.get("Idempotency-Key") ?? "";
    if (!/^[A-Za-z0-9._~-]{8,128}$/.test(idempotencyKey)) return failure("invalid_argument", "Idempotency-Key 无效", 400);
    const body = await request.json().catch(() => null) as components["schemas"]["SubmitTaskRequest"] | null;
    if (!body || typeof body.objective !== "string" || !body.objective.trim() || !body.host_id || !body.model_id) return failure("invalid_argument", "任务信息不完整", 400);
    const serializedRequest = JSON.stringify(body);
    const replayKey = `${user.id}:${idempotencyKey}`;
    const replay = taskSubmitReplays.get(replayKey);
    if (replay) return replay.request === serializedRequest ? json(replay.response) : failure("idempotency_conflict", "幂等键已用于其他请求", 409);
    const host = findHost(body.host_id, user);
    if (!host) return failure("not_found", "云端电脑不存在或无权访问", 404);
    materializeHost(host);
    if (host.state !== "running") return failure("host_not_ready", "云端电脑尚未就绪", 409, { state: host.state });
    if (!modelConnections.some((item) => item.id === body.model_id)) return failure("not_found", "模型连接不存在或不可用", 404);
    let conversationID = body.conversation_id;
    if (conversationID) {
      const existing = conversations.find((item) => item.id === conversationID && conversationOwners.get(item.id) === user.id);
      if (!existing || existing.host_id !== body.host_id) return failure("conflict", "对话电脑与本次所选电脑不一致", 409);
    } else {
      conversationID = id("c");
      const timestamp = now();
      conversations.unshift({ id: conversationID, host_id: body.host_id, title: Array.from(body.objective.trim()).slice(0, 120).join(""), created_at: timestamp, updated_at: timestamp });
      conversationOwners.set(conversationID, user.id);
    }
    const task = createMockTask(user, body.host_id, body.model_id, conversationID, body.objective.trim(), body.budget);
    const message: Message = { id: id("m"), conversation_id: conversationID, role: "user", content: body.objective.trim(), task_id: task.id, created_at: task.created_at };
    tasks.unshift(task);
    conversationMessages.push(message);
    const conversation = conversations.find((item) => item.id === conversationID);
    if (conversation) conversation.updated_at = task.updated_at;
    taskSubmitReplays.set(replayKey, { request: serializedRequest, response: task });
    persistAssistantState();
    return json(task, 202);
  }),
  http.post("/api/conversations/:conversationID/messages", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const conversationID = String(params.conversationID);
    const conversation = conversations.find((item) => item.id === conversationID && conversationOwners.get(item.id) === user.id);
    if (!conversation) return failure("not_found", "会话不存在", 404);
    const idempotencyKey = request.headers.get("Idempotency-Key") ?? "";
    if (!/^[A-Za-z0-9._~-]{8,128}$/.test(idempotencyKey)) return failure("invalid_argument", "Idempotency-Key 无效", 400);
    const body = await request.json().catch(() => null) as components["schemas"]["AppendMessageRequest"] | null;
    if (!body || typeof body.content !== "string" || !body.content.trim() || !body.host_id || !body.model_id) return failure("invalid_argument", "消息内容不完整", 400);
    const serializedRequest = JSON.stringify(body);
    const replayKey = `${user.id}:${idempotencyKey}`;
    const replay = messageSubmitReplays.get(replayKey);
    if (replay) return replay.request === serializedRequest ? json(replay.response, 202) : failure("idempotency_conflict", "幂等键已用于其他请求", 409);
    if (conversation.host_id !== body.host_id) return failure("conflict", "对话电脑与本次所选电脑不一致", 409);
    const host = findHost(body.host_id, user);
    if (!host) return failure("not_found", "云端电脑不存在或无权访问", 404);
    materializeHost(host);
    if (host.state !== "running") return failure("host_not_ready", "云端电脑尚未就绪", 409, { state: host.state });
    if (!modelConnections.some((item) => item.id === body.model_id)) return failure("not_found", "模型连接不存在或不可用", 404);
    const task = createMockTask(user, body.host_id, body.model_id, conversationID, body.content.trim(), body.budget);
    const message: Message = { id: id("m"), conversation_id: conversationID, role: "user", content: body.content.trim(), task_id: task.id, created_at: task.created_at };
    tasks.unshift(task);
    conversationMessages.push(message);
    conversation.updated_at = task.updated_at;
    const response = { message, task };
    messageSubmitReplays.set(replayKey, { request: serializedRequest, response });
    persistAssistantState();
    return json(response, 202);
  }),
  http.get("/api/model-connections", () => json({ items: modelConnections })),
  http.post("/api/model-connections", async ({ request }) => {
    const body = await request.json().catch(() => null) as components["schemas"]["CreateModelConnectionRequest"] | null;
    if (!body || !body.name?.trim() || !body.model_name?.trim() || !body.api_key?.trim()) return failure("invalid_argument", "模型连接信息不完整", 400);
    let parsedURL: URL;
    try { parsedURL = new URL(body.base_url); } catch { return failure("invalid_argument", "服务地址无效", 400); }
    if (parsedURL.protocol !== "https:") return failure("invalid_argument", "服务地址必须使用 HTTPS", 400);
    const timestamp = now();
    const connection: ModelConnection = { id: id("p"), name: body.name.trim(), base_url: body.base_url, model_name: body.model_name.trim(), shared: false, has_api_key: true, capabilities: { text: true, tools: false, vision: false }, checked_at: null, created_at: timestamp, updated_at: timestamp };
    modelConnections.unshift(connection);
    return json(connection, 201);
  }),
  http.post("/api/model-connections/:connectionID/check", ({ params }) => {
    const connection = modelConnections.find((item) => item.id === String(params.connectionID));
    if (!connection) return failure("not_found", "模型连接不存在", 404);
    connection.capabilities = { text: true, tools: true, vision: false };
    connection.checked_at = now();
    connection.updated_at = now();
    return json(connection.capabilities);
  }),
  http.get("/api/schedules", ({ request, cookies }) => {
    const user = sessionUser(cookies)!;
    const query = new URL(request.url).searchParams;
    const status = query.get("status");
    const limit = Math.max(1, Math.min(100, Number(query.get("limit") ?? 100) || 100));
    const offset = Math.max(0, Number(query.get("cursor") ?? 0) || 0);
    const visible = schedules.filter((item) => scheduleOwners.get(item.id) === user.id && (!status || item.status === status)).sort((a, b) => b.updated_at.localeCompare(a.updated_at));
    const items = visible.slice(offset, offset + limit);
    return json({ items, next_cursor: offset + items.length < visible.length ? String(offset + items.length) : null });
  }),
  http.post("/api/schedules", async ({ request, cookies }) => {
    const user = sessionUser(cookies)!;
    const idempotencyKey = request.headers.get("Idempotency-Key") ?? "";
    if (!/^[A-Za-z0-9._~-]{8,128}$/.test(idempotencyKey)) return failure("invalid_argument", "Idempotency-Key 无效", 400);
    const body = await request.json().catch(() => null) as CreateScheduleRequest | null;
    if (!body || typeof body.objective !== "string" || !body.objective.trim() || !body.host_id || !body.model_id || !body.timing?.time_zone) return failure("invalid_argument", "计划信息不完整", 400);
    const serialized = JSON.stringify(body);
    const replayKey = `${user.id}:${idempotencyKey}`;
    const replay = scheduleCreateReplays.get(replayKey);
    if (replay) return replay.request === serialized ? json(replay.response, 201) : failure("idempotency_conflict", "幂等键已用于其他请求", 409);
    if (executionControlFor(user.id).state !== "enabled") return failure("execution_disabled", "AI 执行当前已停止，不能创建计划", 409);
    const host = findHost(body.host_id, user);
    if (!host || materializeHost(host).state !== "running") return failure("host_not_ready", "所选云端电脑不存在或尚未运行", 409);
    const model = modelConnections.find((item) => item.id === body.model_id && item.checked_at && item.capabilities.text && item.capabilities.tools);
    if (!model) return failure("model_unavailable", "所选模型尚不可用", 409);
    try { Intl.DateTimeFormat("zh-CN", { timeZone: body.timing.time_zone }); } catch { return failure("invalid_argument", "时区必须是有效的 IANA 名称", 400); }
    if (body.timing.kind === "once" && (!Number.isFinite(Date.parse(body.timing.run_at)) || Date.parse(body.timing.run_at) <= Date.now())) return failure("invalid_argument", "一次性执行时间必须是将来的有效时刻", 400);
    if (body.timing.kind !== "once" && (!/^\d{4}-\d{2}-\d{2}$/.test(body.timing.starts_on) || !/^([01]\d|2[0-3]):[0-5]\d$/.test(body.timing.local_time))) return failure("invalid_argument", "重复计划日期或时间无效", 400);
    const timestamp = now();
    const schedule: Schedule = {
      id: id("j"), version: 1, status: "enabled", host_id: body.host_id, model_id: body.model_id, objective: body.objective.trim(), timing: body.timing,
      budget: body.budget ?? { active_seconds: 1800, tool_calls: 100, model_tokens: 50000 }, misfire_policy: body.misfire_policy ?? "skip",
      notification_events: body.notification_events ?? ["completed", "failed", "waiting_user"], observation: body.observation ?? null, next_run_at: nextMockRunAt(body.timing), last_run: null, created_at: timestamp, updated_at: timestamp,
    };
    schedules.unshift(schedule);
    scheduleOwners.set(schedule.id, user.id);
    scheduleRuns.set(schedule.id, []);
    scheduleCreateReplays.set(replayKey, { request: serialized, response: schedule });
    persistAssistantState();
    return json(schedule, 201);
  }),
  http.get("/api/schedules/:scheduleID", ({ params, cookies }) => {
    const schedule = findSchedule(String(params.scheduleID), sessionUser(cookies)!);
    return schedule ? json(schedule) : failure("not_found", "计划不存在", 404);
  }),
  http.patch("/api/schedules/:scheduleID", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const schedule = findSchedule(String(params.scheduleID), user);
    if (!schedule) return failure("not_found", "计划不存在", 404);
    const body = await request.json().catch(() => null) as UpdateScheduleRequest | null;
    if (!body || !Number.isSafeInteger(body.expected_version) || body.expected_version < 1 || Object.keys(body).length < 2) return failure("invalid_argument", "计划更新内容无效", 400);
    const { expected_version, ...changes } = body;
    if (expected_version !== schedule.version) return failure("conflict", "计划版本已变化，请刷新后重试", 409, { version: schedule.version });
    const host = body.host_id ? findHost(body.host_id, user) : undefined;
    if (body.host_id && (!host || materializeHost(host).state !== "running")) return failure("host_not_ready", "所选云端电脑不存在或尚未运行", 409);
    if (body.model_id && !modelConnections.some((item) => item.id === body.model_id && item.checked_at && item.capabilities.text && item.capabilities.tools)) return failure("model_unavailable", "所选模型尚不可用", 409);
    Object.assign(schedule, changes);
    schedule.version += 1;
    schedule.next_run_at = schedule.status === "enabled" ? nextMockRunAt(schedule.timing) : null;
    schedule.updated_at = now();
    persistAssistantState();
    return json(schedule);
  }),
  http.post("/api/schedules/:scheduleID/actions", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const schedule = findSchedule(String(params.scheduleID), user);
    if (!schedule) return failure("not_found", "计划不存在", 404);
    const idempotencyKey = request.headers.get("Idempotency-Key") ?? "";
    if (!/^[A-Za-z0-9._~-]{8,128}$/.test(idempotencyKey)) return failure("invalid_argument", "Idempotency-Key 无效", 400);
    const body = await request.json().catch(() => null) as ScheduleActionRequest | null;
    if (!body || !Number.isSafeInteger(body.expected_version) || body.expected_version < 1) return failure("invalid_argument", "计划操作版本无效", 400);
    const serialized = JSON.stringify(body);
    const replayKey = `${user.id}:${schedule.id}:${idempotencyKey}`;
    const replay = scheduleActionReplays.get(replayKey);
    if (replay) return replay.request === serialized ? json(replay.response, 202) : failure("idempotency_conflict", "幂等键已用于其他请求", 409);
    if (body.expected_version !== schedule.version) return failure("conflict", "计划版本已变化，请刷新后重试", 409, { version: schedule.version });
    if (body.action === "enable") {
      if (schedule.status === "completed") return failure("invalid_state", "已完成的一次性计划不能重新启用", 409);
      if (executionControlFor(user.id).state !== "enabled") return failure("execution_disabled", "AI 执行当前已停止，不能启用计划", 409);
      if (schedule.status !== "enabled") { schedule.status = "enabled"; schedule.version += 1; schedule.next_run_at = nextMockRunAt(schedule.timing); schedule.updated_at = now(); }
    } else {
      if (body.current_run_action === "cancel" && schedule.last_run && activeScheduleRunStatuses.has(schedule.last_run.status)) {
        const run = schedule.last_run;
        run.status = "stopping";
        run.updated_at = now();
        const task = run.task_id ? tasks.find((item) => item.id === run.task_id && item.owner_id === user.id) : undefined;
        if (task && activeScheduleRunStatuses.has(run.status)) {
          task.status = "cancelling";
          task.stop_request = { id: "xscheduled", action: "cancel", status: "pending", safe_after: now(), unsettled_operation_ids: [] };
          task.updated_at = now();
          scheduleTaskCancellation(schedule, task);
        }
      }
      if (schedule.status !== "disabled") { schedule.status = "disabled"; schedule.version += 1; schedule.next_run_at = null; schedule.updated_at = now(); }
    }
    const run = schedule.last_run && activeScheduleRunStatuses.has(schedule.last_run.status) ? schedule.last_run : null;
    const response: components["schemas"]["ScheduleActionResponse"] = { schedule, current_run: run };
    scheduleActionReplays.set(replayKey, { request: serialized, response });
    persistAssistantState();
    return json(response, 202);
  }),
  http.get("/api/schedules/:scheduleID/runs", ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const schedule = findSchedule(String(params.scheduleID), user);
    if (!schedule) return failure("not_found", "计划不存在", 404);
    const query = new URL(request.url).searchParams;
    const limit = Math.max(1, Math.min(100, Number(query.get("limit") ?? 100) || 100));
    const offset = Math.max(0, Number(query.get("cursor") ?? 0) || 0);
    const runs = [...(scheduleRuns.get(schedule.id) ?? [])].sort((a, b) => b.created_at.localeCompare(a.created_at));
    const items = runs.slice(offset, offset + limit);
    return json({ items, next_cursor: offset + items.length < runs.length ? String(offset + items.length) : null });
  }),
  http.get("/api/notifications", ({ request, cookies }) => {
    const user = sessionUser(cookies)!;
    const query = new URL(request.url).searchParams;
    const unreadOnly = query.get("unread_only") === "true";
    const limit = Math.max(1, Math.min(100, Number(query.get("limit") ?? 100) || 100));
    const offset = Math.max(0, Number(query.get("cursor") ?? 0) || 0);
    const visible = notifications.filter((item) => notificationOwners.get(item.id) === user.id && (!unreadOnly || item.read_at === null)).sort((a, b) => b.created_at.localeCompare(a.created_at));
    const items = visible.slice(offset, offset + limit);
    return json({ items, next_cursor: offset + items.length < visible.length ? String(offset + items.length) : null });
  }),
  http.post("/api/notifications/:notificationID/read", ({ params, cookies }) => {
    const user = sessionUser(cookies)!;
    const notification = notifications.find((item) => item.id === String(params.notificationID) && notificationOwners.get(item.id) === user.id);
    if (!notification) return failure("not_found", "通知不存在", 404);
    notification.read_at ??= now();
    persistAssistantState();
    return empty();
  }),
  http.get("/api/tasks", ({ cookies }) => {
    const user = sessionUser(cookies)!;
    const visible = tasks.filter((task) => user.role === "admin" || task.owner_id === user.id)
      .sort((left, right) => right.updated_at.localeCompare(left.updated_at));
    const items: TaskSummary[] = visible.map((task) => ({
      id: task.id, owner_id: task.owner_id, host_id: task.host_id, conversation_id: task.conversation_id,
      model_id: task.model_id, objective: task.objective, status: task.status, current_step: task.current_step,
      budget: task.budget, usage: task.usage, created_at: task.created_at, updated_at: task.updated_at,
    }));
    return json({ items, next_cursor: null });
  }),
  http.get("/api/tasks/:taskId", ({ params, cookies }) => {
    const task = findTask(String(params.taskId), sessionUser(cookies)!);
    return task ? json(task) : failure("not_found", "任务不存在", 404);
  }),
  http.get("/api/tasks/:taskId/events", ({ params, cookies }) => {
    if (!findTask(String(params.taskId), sessionUser(cookies)!)) return failure("not_found", "任务不存在", 404);
    const encoder = new TextEncoder();
    const body = new ReadableStream<Uint8Array>({ start: (controller) => controller.enqueue(encoder.encode(": connected\n\n")) });
    return new HttpResponse(body, { headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" } });
  }),
  http.post("/api/tasks/:taskId/actions", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const task = findTask(String(params.taskId), user);
    if (!task) return failure("not_found", "任务不存在", 404);
    const idempotencyKey = request.headers.get("Idempotency-Key") ?? "";
    if (!/^[A-Za-z0-9._~-]{8,128}$/.test(idempotencyKey)) return failure("invalid_argument", "Idempotency-Key 无效", 400);
    const body = await request.json().catch(() => null) as TaskActionRequest | null;
    if (!body || typeof body !== "object" || !("action" in body)) return failure("invalid_argument", "任务操作无效", 400);
    const serializedRequest = JSON.stringify(body);
    const replayKey = `${user.id}:${idempotencyKey}`;
    const replay = taskActionReplays.get(replayKey);
    if (replay) return replay.request === serializedRequest ? json(replay.response, 202) : failure("idempotency_conflict", "幂等键已用于其他请求", 409);

    if (body.action === "respond") {
      const pending = task.pending_request;
      if (task.status !== "waiting_user" || task.waiting_reason !== "approval_required" || !pending || pending.id !== body.pending_id || pending.version !== body.pending_version) return failure("conflict", "审批请求已变化或不再等待处理", 409);
      task.pending_request = null;
      task.waiting_reason = null;
      task.status = "queued";
    } else if (body.action === "answer_clarification") {
      const pending = task.clarification_request;
      const answerLength = Array.from(body.answer).length;
      if (answerLength === 0 || answerLength > 16384 || !body.answer.trim() || body.answer.includes("\0")) return failure("invalid_argument", "回答必须包含有效文字且不能超过 16,384 个字符", 400);
      if (task.status !== "waiting_user" || task.waiting_reason !== "clarification_required" || !pending || pending.id !== body.clarification_id || pending.version !== body.clarification_version) return failure("conflict", "补充请求已变化或不再等待处理", 409);
      task.clarification_request = null;
      task.waiting_reason = null;
      task.status = "queued";
    } else if (body.action === "pause") {
      if (body.expected_status !== task.status || (task.status !== "queued" && task.status !== "running")) return failure("conflict", "任务状态已变化，请刷新后重试", 409);
      task.status = "pausing";
      task.stop_request = { id: "xmock001", action: "pause", status: "pending", safe_after: new Date(Date.now() + 1000).toISOString(), unsettled_operation_ids: [] };
    } else if (body.action === "resume") {
      if (body.expected_status !== "paused" || task.status !== "paused") return failure("conflict", "任务状态已变化，请刷新后重试", 409);
      task.status = "queued";
      task.stop_request = null;
    } else if (body.action === "cancel") {
      if (body.expected_status !== task.status || !["queued", "running", "waiting_user", "paused"].includes(task.status)) return failure("conflict", "任务状态已变化，请刷新后重试", 409);
      task.status = "cancelling";
      task.pending_request = null;
      task.clarification_request = null;
      task.waiting_reason = null;
      task.stop_request = { id: "xmock001", action: "cancel", status: "pending", safe_after: new Date(Date.now() + 1000).toISOString(), unsettled_operation_ids: [] };
    } else if (body.action === "retry_batch_item") {
      const item = task.batch_items.find((candidate) => candidate.id === body.batch_item_id);
      if (!item || item.status !== "failed" || task.status !== "failed") return failure("conflict", "失败项已变化，无法重试", 409);
      item.status = "queued";
      item.attempt += 1;
      item.error = null;
      task.status = "queued";
      task.result = null;
      task.finished_at = null;
    } else {
      return failure("invalid_argument", "不支持的任务操作", 400);
    }
    task.updated_at = now();
    if (body.action === "pause" || body.action === "cancel") scheduleTaskStop(task);
    persistAssistantState();
    const result: TaskActionResponse = { task_id: task.id, action: body.action, status: task.status, unsettled_operation_ids: task.stop_request?.unsettled_operation_ids ?? [] };
    taskActionReplays.set(replayKey, { request: serializedRequest, response: result });
    return json(result, 202);
  }),
  http.get("/api/apikeys", ({ cookies }) => { const user = sessionUser(cookies)!; return json({ items: apiKeys.filter((key) => key.owner_id === user.id).map(hideOwner) }); }),
  http.post("/api/apikeys", async ({ request, cookies }) => {
    const user = sessionUser(cookies)!;
    const body = await request.json().catch(() => ({})) as { name?: string; ttl_seconds?: number; host_ids?: string[]; scopes?: string[] };
    if (!Array.isArray(body.host_ids) || !Array.isArray(body.scopes)) return failure("invalid_argument", "主机和权限范围必须明确选择", 400);
    if (body.host_ids.some((hostID) => {
      const host = typeof hostID === "string" ? hosts.find((item) => item.id === hostID) : undefined;
      const member = hostMembers.get(String(hostID))?.find((item) => item.user_id === user.id);
      return !host || (host.owner_id !== user.id && !member?.permissions.includes("mcp:control"));
    })) return failure("not_found", "主机不存在", 404);
    const key = { id: id("t"), kind: "api_key", name: body.name ?? "", host_id: "", host_ids: body.host_ids, scopes: body.scopes, owner_id: user.id, expires_at: new Date(Date.now() + (body.ttl_seconds ?? 86400) * 1000).toISOString(), last_used_at: null, revoked: false, created_at: now() };
    apiKeys.push(key);
    return json({ ...hideOwner(key), key: "chk_mock-one-time-key" }, 201);
  }),
  http.delete("/api/apikeys/:keyId", ({ params, cookies }) => {
    const user = sessionUser(cookies)!;
    const key = apiKeys.find((item) => item.id === String(params.keyId) && item.owner_id === user.id);
    if (!key) return failure("not_found", "API Key 不存在", 404);
    key.revoked = true;
    return empty();
  }),
  http.get("/api/admin/hosts", ({ cookies }) => { const user = sessionUser(cookies)!; completeDeletions(); return user.role === "admin" ? json({ items: hosts.map(materializeHost) }) : failure("forbidden", "没有执行此操作的权限", 403); }),
  http.get("/api/admin/workspaces", ({ cookies }) => {
    const user = sessionUser(cookies)!;
    if (user.role !== "admin") return failure("forbidden", "没有执行此操作的权限", 403);
    completeDeletions();
    return json({ items: workspaces.filter(isVisibleAdminWorkspace).map(adminWorkspaceView), next_cursor: null });
  }),
  http.get("/api/admin/workspaces/:workspaceId", ({ params, cookies }) => {
    const user = sessionUser(cookies)!;
    if (user.role !== "admin") return failure("forbidden", "没有执行此操作的权限", 403);
    completeDeletions();
    const workspace = workspaces.filter(isVisibleAdminWorkspace).find((item) => item.id === String(params.workspaceId));
    return workspace ? json(adminWorkspaceView(workspace)) : failure("not_found", "工作区不存在", 404);
  }),
  http.get("/api/admin/users", ({ cookies }) => { const user = sessionUser(cookies)!; return user.role === "admin" ? json({ items: users.map(userWithUsage) }) : failure("forbidden", "没有执行此操作的权限", 403); }),
  http.post("/api/admin/users", async ({ request, cookies }) => {
    if (sessionUser(cookies)!.role !== "admin") return failure("forbidden", "没有执行此操作的权限", 403);
    const body = await request.json().catch(() => ({})) as { name?: string; password?: string; role?: "admin" | "user"; quota?: User["quota"] };
    const name = body.name?.trim();
    if (!name || !body.password) return failure("invalid_argument", "用户名和初始密码不能为空", 400);
    if (loginAccounts.has(name) || users.some((user) => user.name === name)) return failure("invalid_state", "用户名已存在", 409);
    const user: User = { id: id("u"), name, role: body.role ?? "user", disabled: false, quota: body.quota ?? demoUser.quota, usage: { hosts: 0, cpu_milli: 0, mem_mib: 0 } };
    users.push(user);
    passwords.set(user.id, body.password);
    return json(user, 201);
  }),
  http.patch("/api/admin/users/:userId", async ({ params, request, cookies }) => { if (sessionUser(cookies)!.role !== "admin") return failure("forbidden", "没有执行此操作的权限", 403); const user = users.find((item) => item.id === String(params.userId)); if (!user) return failure("not_found", "用户不存在", 404); Object.assign(user, await request.json().catch(() => ({}))); return json(userWithUsage(user)); }),
  http.get("/api/admin/capacity", ({ cookies }) => sessionUser(cookies)!.role === "admin" ? json({ limit: capacityLimit, allocated: capacityUsage(), hosts_by_state: hostsByState() }) : failure("forbidden", "没有执行此操作的权限", 403)),
  http.get("/api/admin/audit", ({ request, cookies }) => {
    if (sessionUser(cookies)!.role !== "admin") return failure("forbidden", "没有执行此操作的权限", 403);
    const params = new URL(request.url).searchParams;
    const actor = params.get("actor")?.toLowerCase();
    const action = params.get("action")?.toLowerCase();
    const target = params.get("target")?.toLowerCase();
    const since = Date.parse(params.get("since") ?? "");
    const requestedLimit = Number(params.get("limit") ?? 100);
    const limit = Number.isInteger(requestedLimit) && requestedLimit > 0 ? Math.min(requestedLimit, 500) : 100;
    const items = auditLogs.filter((item) =>
      (!actor || item.actor_id.toLowerCase().includes(actor)) &&
      (!action || item.action.toLowerCase().includes(action)) &&
      (!target || item.target.toLowerCase().includes(target)) &&
      (!Number.isFinite(since) || Date.parse(item.at) >= since),
    ).sort((left, right) => right.at.localeCompare(left.at)).slice(0, limit);
    return json({ items });
  }),
];
