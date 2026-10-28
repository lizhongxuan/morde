import { http, HttpResponse } from "msw";
import type { components } from "../api/schema";
import type { HostMemberPermission } from "../app/api";

type User = { id: string; name: string; role: components["schemas"]["UserRole"]; status: components["schemas"]["AccountStatus"]; must_change_password: boolean; quota: { hosts: number; cpu_milli: number; mem_mib: number }; usage: { hosts: number; cpu_milli: number; mem_mib: number }; default_host_resources: { cpu_milli: number; mem_mib: number } };
type AuditLog = { id: number; at: string; actor_id: string; actor_kind: string; action: string; target: string; detail: Record<string, unknown>; ip: string };
type Workspace = components["schemas"]["Workspace"];
type WorkspaceRef = components["schemas"]["WorkspaceRef"];
type WorkspaceBinding = components["schemas"]["WorkspaceBinding"];
type Host = Omit<components["schemas"]["Host"], "workspaces"> & { sandbox_id?: string | null; workspaces: WorkspaceBinding[] };
type HostMember = { user_id: string; permissions: HostMemberPermission[]; created_at: string };
type ModelConnection = components["schemas"]["ModelConnection"];
type Conversation = components["schemas"]["Conversation"];
type ConversationMessage = components["schemas"]["ConversationMessage"];
type Task = components["schemas"]["Task"];
type UserPreferences = components["schemas"]["UserPreferences"];
type Notification = components["schemas"]["Notification"];
type ClientInstallation = components["schemas"]["ClientInstallation"];
type AdminWorkspace = components["schemas"]["AdminWorkspace"];
type VisibleAdminWorkspace = Omit<Workspace, "state"> & { state: "ready" | "deleting" };

const defaultHostResources = { cpu_milli: 1000, mem_mib: 2048 };
const demoUser: User = { id: "u1a2b3c4", name: "demo", role: "admin", status: "approved", must_change_password: false, quota: { hosts: 1, cpu_milli: 4000, mem_mib: 8192 }, usage: { hosts: 0, cpu_milli: 0, mem_mib: 0 }, default_host_resources: defaultHostResources };
const memberUser: User = { ...demoUser, id: "umember1", name: "member", role: "user" };
const initialAdmin: User = { ...demoUser, id: "uadmin01", name: "admin", role: "super_admin", must_change_password: true };
const templates = [
  { id: "std-2c4g", name: "标准 2 核 4G", description: "适合日常浏览与开发", isolation: "container", resources: { cpu_milli: 500, mem_mib: 1536, cpu_limit_milli: 2000, mem_limit_mib: 4096 }, workspace_size_mib: 20480, ttl_seconds: 28800, idle_timeout_seconds: 7200, on_idle: "disabled", keep_workspace_default: true },
  { id: "std-1c2g", name: "标准 1 核 2G", description: "适合轻量浏览与简单任务", isolation: "container", resources: { cpu_milli: 250, mem_mib: 768, cpu_limit_milli: 1000, mem_limit_mib: 2048 }, workspace_size_mib: 20480, ttl_seconds: 28800, idle_timeout_seconds: 7200, on_idle: "disabled", keep_workspace_default: true },
  { id: "std-2c2g", name: "标准 2 核 2G", description: "适合轻量开发与多任务", isolation: "container", resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 2000, mem_limit_mib: 2048 }, workspace_size_mib: 20480, ttl_seconds: 28800, idle_timeout_seconds: 7200, on_idle: "disabled", keep_workspace_default: true },
];
let hosts: Host[] = [];
let workspaces: Workspace[] = [];
const workspaceOwners = new Map<string, string>();
let hostMembers = new Map<string, HostMember[]>();
const hostResourceVersions = new Map<string, number>();
let apiKeys: Array<Record<string, unknown>> = [];
let modelConnections: ModelConnection[] = [];
let conversations: Conversation[] = [];
let conversationMessages: ConversationMessage[] = [];
let conversationOwners = new Map<string, string>();
let tasks = new Map<string, Task>();
const taskSubmitReplays = new Map<string, { request: string; response: Task }>();
const taskMessageReplays = new Map<string, { request: string; response: { message: ConversationMessage; task: Task } }>();
let userPreferences = new Map<string, UserPreferences>();
let notifications: Notification[] = [];
let clients: ClientInstallation[] = [];
const clientOwners = new Map<string, string>();
let notificationOwners = new Map<string, string>();
const workspaceCreateReplays = new Map<string, { request: string; response: Workspace }>();
const hostCreateReplays = new Map<string, { request: string; response: Host }>();
const workspaceBindReplays = new Map<string, { request: string; response: WorkspaceBinding }>();
const workspaceUnbindReplays = new Map<string, { request: string; response: WorkspaceBinding }>();
const workspaceUnbindingStartedAt = new Map<string, number>();
const users: User[] = [demoUser, memberUser, initialAdmin];
const passwords = new Map<string, string>();
const loginAccounts = new Map<string, { user: User; password: string }>();
const sessions = new Map<string, { userId: string; name: string }>();
const sessionStoragePrefix = "cloudhost:portal-mock-session:";
const assistantMockStorageKey = "cloudhost:portal-mock-assistant-state-v3";
let auditLogs: AuditLog[] = [];
const hostDeletionStartedAt = new Map<string, number>();
const hostReleaseStartedAt = new Map<string, number>();
const workspaceDeletionStartedAt = new Map<string, number>();
const deletionTimers = new Set<ReturnType<typeof setTimeout>>();
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
const hideOwner = (resource: Record<string, unknown>) => { const visible = { ...resource }; delete visible.owner_id; return visible; };
const toWorkspaceRef = (workspace: Workspace): WorkspaceRef => ({
  id: workspace.id, name: workspace.name, revision: workspace.revision, used_bytes: workspace.used_bytes, state: workspace.state,
});

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

type PersistedAssistantState = {
  userResources?: Array<Pick<User, "id" | "quota" | "default_host_resources">>;
  apiKeys?: Array<Record<string, unknown>>;
  hosts: Host[];
  workspaces: Workspace[];
  workspaceOwners: Array<[string, string]>;
  userPreferences: Array<[string, UserPreferences]>;
  conversations?: Conversation[];
  conversationMessages?: ConversationMessage[];
  conversationOwners?: Array<[string, string]>;
  tasks?: Task[];
  notifications?: Notification[];
  notificationOwners?: Array<[string, string]>;
  hostMembers?: Array<[string, HostMember[]]>;
  hostResourceVersions?: Array<[string, number]>;
  clients?: ClientInstallation[];
  clientOwners?: Array<[string, string]>;
  auditLogs?: AuditLog[];
};

function persistAssistantState() {
  if (typeof localStorage === "undefined") return;
  const state: PersistedAssistantState = {
    userResources: users.map(({ id, quota, default_host_resources }) => ({ id, quota, default_host_resources })),
    apiKeys,
    hosts,
    workspaces,
    workspaceOwners: Array.from(workspaceOwners.entries()),
    userPreferences: Array.from(userPreferences.entries()),
    conversations,
    conversationMessages,
    conversationOwners: Array.from(conversationOwners.entries()),
    tasks: Array.from(tasks.values()),
    notifications,
    notificationOwners: Array.from(notificationOwners.entries()),
    hostMembers: Array.from(hostMembers.entries()),
    hostResourceVersions: Array.from(hostResourceVersions.entries()),
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
  hostReleaseStartedAt.clear();
  Object.assign(demoUser, { id: "u1a2b3c4", name: "demo", role: "admin", status: "approved", must_change_password: false,
    quota: { hosts: 1, cpu_milli: 4000, mem_mib: 8192 },
    usage: { hosts: 0, cpu_milli: 0, mem_mib: 0 }, default_host_resources: defaultHostResources });
  Object.assign(memberUser, { ...demoUser, id: "umember1", name: "member", role: "user", status: "approved", must_change_password: false });
  Object.assign(initialAdmin, { ...demoUser, id: "uadmin01", name: "admin", role: "super_admin", status: "approved", must_change_password: true });
  sessions.clear();
  hosts = [];
  hostMembers = new Map();
  hostResourceVersions.clear();
  workspaces = [];
  workspaceOwners.clear();
  apiKeys = [];
  modelConnections = [{ id: "pmodel01", name: "团队示例模型", base_url: "https://models.example.invalid/v1", model_name: "morde-demo", shared: true, has_api_key: true, capabilities: { text: true, tools: true, vision: false }, checked_at: now(), created_at: now(), updated_at: now() }];
  conversations = [];
  conversationMessages = [];
  conversationOwners = new Map();
  tasks = new Map();
  taskSubmitReplays.clear();
  taskMessageReplays.clear();
  userPreferences = new Map();
  notifications = [];
  clients = [{ id: "cli_demo01", platform: "desktop", display_name: "Morde 演示电脑", expires_at: new Date(Date.now() + 30 * 86_400_000).toISOString(), revoked: false, created_at: now(), last_used_at: null } as ClientInstallation];
  clientOwners.clear();
  clientOwners.set(clients[0]!.id, demoUser.id);
  notificationOwners = new Map();
  workspaceCreateReplays.clear();
  hostCreateReplays.clear();
  workspaceBindReplays.clear();
  workspaceUnbindReplays.clear();
  workspaceUnbindingStartedAt.clear();
  users.splice(0, users.length, demoUser, memberUser, initialAdmin);
  passwords.clear();
  loginAccounts.clear();
  loginAccounts.set("demo", { user: demoUser, password: "demo" });
  loginAccounts.set("admin", { user: initialAdmin, password: "admin" });
  loginAccounts.set("member", { user: memberUser, password: "member" });
  auditLogs = [
    { id: 3, at: now(), actor_id: demoUser.id, actor_kind: "user", action: "host.create", target: "h-audit-demo", detail: { name: "示例主机" }, ip: "127.0.0.1" },
    { id: 2, at: new Date(Date.now() - 60 * 60 * 1000).toISOString(), actor_id: demoUser.id, actor_kind: "user", action: "apikey.create", target: "t-audit-demo", detail: { name: "示例 Key" }, ip: "127.0.0.1" },
    { id: 1, at: new Date(Date.now() - 48 * 60 * 60 * 1000).toISOString(), actor_id: memberUser.id, actor_kind: "user", action: "auth.login", target: memberUser.id, detail: {}, ip: "127.0.0.1" },
  ];
  hostDeletionStartedAt.clear();
  workspaceDeletionStartedAt.clear();
  if (persisted && Array.isArray(persisted.hosts) && Array.isArray(persisted.workspaces)
    && Array.isArray(persisted.workspaceOwners) && Array.isArray(persisted.userPreferences)) {
    if (Array.isArray(persisted.userResources)) {
      for (const resources of persisted.userResources) {
        const user = users.find((item) => item.id === resources.id);
        if (user) Object.assign(user, { quota: resources.quota, default_host_resources: resources.default_host_resources });
      }
    }
    if (Array.isArray(persisted.apiKeys)) apiKeys = persisted.apiKeys;
    hosts = persisted.hosts;
    workspaces = persisted.workspaces;
    for (const [workspaceId, ownerId] of persisted.workspaceOwners) workspaceOwners.set(workspaceId, ownerId);
    userPreferences = new Map(persisted.userPreferences);
    if (Array.isArray(persisted.conversations) && Array.isArray(persisted.conversationMessages) && Array.isArray(persisted.conversationOwners)) {
      conversations = persisted.conversations;
      conversationMessages = persisted.conversationMessages;
      conversationOwners = new Map(persisted.conversationOwners);
      tasks = new Map((persisted.tasks ?? []).map((task) => [task.id, task]));
    }
    if (Array.isArray(persisted.hostMembers)) hostMembers = new Map(persisted.hostMembers);
    if (Array.isArray(persisted.hostResourceVersions)) for (const [hostID, version] of persisted.hostResourceVersions) hostResourceVersions.set(hostID, version);
    if (Array.isArray(persisted.clients) && Array.isArray(persisted.clientOwners)) {
      clients = persisted.clients;
      clientOwners.clear();
      for (const [clientId, ownerId] of persisted.clientOwners) clientOwners.set(clientId, ownerId);
    }
    if (Array.isArray(persisted.auditLogs)) auditLogs = persisted.auditLogs;
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
  if (!notifications.length) {
    const timestamp = now();
    notifications = [
      { id: "ndemo001", kind: "waiting_user", task_id: "qreview1", event_sequence: 3, summary: "历史任务曾等待确认：发送部署完成通知？", redacted: false, actionable: false, created_at: timestamp, read_at: null },
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
  const availableModels = modelConnections.filter((model) => model.checked_at && model.capabilities.text);
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
  const nextAction: components["schemas"]["OnboardingStatus"]["next_action"] = modelStatus === "ready" ? "start_chat" : "configure_model";
  return {
    model_status: modelStatus,
    models: availableModels.map((model) => ({ id: model.id, name: model.name, capabilities: model.capabilities })),
    selected_model_id: preferences.selected_model_id,
    computer_status: computerStatus,
    computers: ownedHosts.map((host) => ({ id: host.id, name: host.name, state: host.state as components["schemas"]["OnboardingComputer"]["state"] })),
    default_host_id: defaultHost?.id ?? null,
    can_submit: modelStatus === "ready",
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
  if (!user || user.status !== "approved") return null;
  return session.name === user.name ? user : { ...user, name: session.name };
}

function createTaskMessage(user: User, conversation: Conversation, modelID: string, content: string, request: Record<string, unknown>) {
  const timestamp = now();
  const budget = request.budget as { active_seconds: number; tool_calls: number; model_tokens: number } | undefined;
  const task: Task = {
    id: id("q"), owner_id: user.id, host_id: typeof request.host_id === "string" ? request.host_id : undefined,
    execution_location: "server", conversation_id: conversation.id, model_id: modelID, objective: content,
    status: "queued", current_step: null,
    budget: budget ?? { active_seconds: 1800, tool_calls: 100, model_tokens: 50000 },
    usage: { active_seconds: 0, tool_calls: 0, model_tokens: null }, created_at: timestamp, updated_at: timestamp,
    result: null, waiting_reason: null, steps: [], batch_items: [], artifacts: [], pending_request: null,
    clarification_request: null, stop_request: null, finished_at: null,
  };
  const message: ConversationMessage = {
    id: id("m"), conversation_id: conversation.id, role: "user", content, task_id: task.id, created_at: timestamp,
  };
  conversation.updated_at = timestamp;
  tasks.set(task.id, task);
  conversationMessages.push(message);
  persistAssistantState();
  return { task, message };
}

function activeConversationTask(conversationID: string) {
  return Array.from(tasks.values()).find((task) => task.conversation_id === conversationID
    && !["completed", "failed", "cancelled"].includes(task.status));
}

const isAdmin = (user: User) => user.role === "admin" || user.role === "super_admin";
const validAccountStatus = (status: string): status is User["status"] => ["pending", "rejected", "approved", "disabled"].includes(status);
const nextAccountStatuses: Record<User["status"], User["status"][]> = {
  pending: ["approved", "rejected"], rejected: ["approved"], approved: ["disabled"], disabled: ["approved"],
};
function revokeUserSessions(userId: string) {
  for (const [sessionId, session] of sessions) {
    if (session.userId !== userId) continue;
    sessions.delete(sessionId);
    if (typeof localStorage !== "undefined") localStorage.removeItem(`${sessionStoragePrefix}${sessionId}`);
  }
}

const unauthenticated = () => failure("unauthenticated", "请先登录", 401);
// Keep this first so protected handlers cannot run before the session check.
const sessionGuard = http.all("/api/*", ({ request, cookies }) => {
  const path = new URL(request.url).pathname;
  if (path === "/api/capabilities" || path === "/api/auth/login" || path === "/api/auth/register" || path === "/api/auth/logout") return;
  const user = sessionUser(cookies);
  if (!user) return unauthenticated();
  if (user.must_change_password && !(path === "/api/me" || path === "/api/me/password")) {
    return failure("forbidden", "请先修改超级管理员密码", 403);
  }
});

const findHost = (hostId: string, user: User) => {
  completeDeletions();
  const host = hosts.find((item) => item.id === hostId);
  return host && (isAdmin(user) || host.owner_id === user.id) ? host : undefined;
};
function materializeHost(host: Host) {
  if (host.state === "creating" && Date.now() - new Date(host.created_at).getTime() > 900) {
    host.state = "running";
    host.state_reason = "";
    persistAssistantState();
  }
  return host;
}

function occupiesResources(host: Host) {
  return allocatedStates.has(host.state) || (host.state === "failed" && (Boolean(host.sandbox_id) || host.workspaces.some((binding) => binding.state !== "detached")));
}

function usageFor(ownerId: string) {
  completeDeletions();
  return hosts.filter((host) => host.owner_id === ownerId).reduce((usage, host) => {
    const allocated = occupiesResources(host);
    return {
      hosts: usage.hosts + 1,
      cpu_milli: usage.cpu_milli + (allocated ? host.resources.cpu_milli : 0),
      mem_mib: usage.mem_mib + (allocated ? host.resources.mem_mib : 0),
    };
  }, { hosts: 0, cpu_milli: 0, mem_mib: 0 });
}

function ensureMockDefaultHost(user: User) {
  if (user.quota.hosts < 1 || hosts.some((host) => host.owner_id === user.id)) return;
  const template = templates.find((item) => item.id === "std-1c2g")!;
  const timestamp = now();
  const host: Host = {
    id: id("h"), name: "我的云端电脑", owner_id: user.id, template_id: template.id, isolation: template.isolation,
    state: "stopped", state_reason: "", runtime_generation: 0, control_version: 0, control_state: "ai", url: "", workspaces: [],
    resources: { ...user.default_host_resources, cpu_limit_milli: user.default_host_resources.cpu_milli, mem_limit_mib: user.default_host_resources.mem_mib },
    expires_at: new Date(Date.now() + template.ttl_seconds * 1000).toISOString(), last_active_at: timestamp, created_at: timestamp,
  };
  hosts.unshift(host);
  userPreferences.set(user.id, { ...preferencesFor(user), default_host_id: host.id, updated_at: timestamp });
  persistAssistantState();
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
  http.post("/api/auth/register", async ({ request }) => {
    const body = await request.json().catch(() => ({})) as { name?: string; password?: string };
    const name = body.name?.trim();
    if (!name || name.length < 3 || name.length > 64 || !body.password || body.password.length < 8 || body.password.length > 72) return failure("invalid_argument", "用户名或密码无效", 400);
    if (name.toLowerCase() === "admin" || loginAccounts.has(name) || users.some((user) => user.name === name)) return failure("conflict", "用户名已被使用", 409);
    const user: User = { id: id("u"), name, role: "user", status: "pending", must_change_password: false,
      quota: { ...demoUser.quota }, usage: { hosts: 0, cpu_milli: 0, mem_mib: 0 }, default_host_resources: { ...defaultHostResources } };
    users.push(user);
    passwords.set(user.id, body.password);
    return json({ user }, 201);
  }),
  http.post("/api/auth/login", async ({ request }) => {
    const body = await request.json().catch(() => ({})) as { name?: string; password?: string };
    const account = loginAccounts.get(body.name ?? "");
    const user = account ? users.find((item) => item.id === account.user.id) ?? account.user : users.find((item) => item.name === body.name);
    const password = account?.password ?? (user ? passwords.get(user.id) : undefined);
    if (!user || body.password !== password) return failure("unauthenticated", "用户名或密码错误", 401);
    if (user.status !== "approved") return failure(`account_${user.status === "pending" ? "pending" : user.status === "rejected" ? "rejected" : "disabled"}`, "账号当前状态不允许登录", 403);
    const hostname = new URL(request.url).hostname;
    if (user.must_change_password && body.password === "admin" && hostname !== "localhost" && hostname !== "127.0.0.1" && hostname !== "[::1]" && hostname !== "::1") {
      return failure("unauthenticated", "用户名或密码错误", 401);
    }
    ensureMockDefaultHost(user);
    const displayUser = user;
    const sessionId = globalThis.crypto?.randomUUID?.() ?? id("s");
    const session = { userId: user.id, name: displayUser.name };
    sessions.set(sessionId, session);
    if (typeof localStorage !== "undefined") localStorage.setItem(`${sessionStoragePrefix}${sessionId}`, JSON.stringify(session));
    // MSW stores mocked response cookies through document.cookie; the real API sets HttpOnly.
    return json({ user: userWithUsage(displayUser) }, 200, { "Set-Cookie": `${sessionCookieName}=${sessionId}; Path=/; SameSite=Lax` });
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
  http.post("/api/me/password", async ({ request, cookies }) => {
    const user = sessionUser(cookies)!;
    if (user.name !== "admin" || user.role !== "super_admin") return failure("forbidden", "只有超级管理员可以修改此密码", 403);
    const body = await request.json().catch(() => ({})) as { current_password?: string; new_password?: string };
    const account = loginAccounts.get("admin");
    if (!body.current_password || body.current_password !== account?.password) return failure("unauthenticated", "当前密码错误", 401);
    if (!body.new_password || body.new_password.length < 8 || body.new_password.length > 72) return failure("invalid_argument", "新密码长度必须为 8 至 72 个字符", 400);
    user.must_change_password = false;
    passwords.set(user.id, body.new_password);
    loginAccounts.set("admin", { user, password: body.new_password });
    revokeUserSessions(user.id);
    return empty({ "Set-Cookie": `${sessionCookieName}=; Path=/; Max-Age=0; SameSite=Lax` });
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
      ensureMockDefaultHost(user);
      selected = hosts.find((host) => host.owner_id === user.id);
      created = Boolean(selected);
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
    if (!target || target.status !== "approved") return failure("not_found", "用户不存在", 404);
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
    const body = await request.json().catch(() => null) as { name?: string; workspace_ids?: string[] } | null;
    const idempotencyKey = request.headers.get("Idempotency-Key") ?? "";
    if (!/^[A-Za-z0-9._~-]{8,128}$/.test(idempotencyKey)) return failure("invalid_argument", "需要有效的 Idempotency-Key", 400);
    if (!body || typeof body !== "object" || Array.isArray(body) || Object.keys(body).some((field) => field !== "name" && field !== "workspace_ids")) {
      return failure("invalid_argument", "用户不能选择云端电脑规格", 400);
    }
    if (body.workspace_ids !== undefined && (!Array.isArray(body.workspace_ids) || body.workspace_ids.some((workspaceId) => typeof workspaceId !== "string") || new Set(body.workspace_ids).size !== body.workspace_ids.length)) {
      return failure("invalid_argument", "workspace_ids 必须是无重复的工作区编号列表", 400);
    }
    const template = templates.find((item) => item.id === "std-1c2g")!;
    const resources = { ...user.default_host_resources, cpu_limit_milli: user.default_host_resources.cpu_milli, mem_limit_mib: user.default_host_resources.mem_mib };
    const workspaceIDs = [...(body.workspace_ids ?? [])].sort();
    const normalizedRequest = JSON.stringify({ template_id: template.id, name: body.name?.trim() || template.name, workspace_ids: workspaceIDs });
    const replayKey = `${user.id}\u0000${idempotencyKey}`;
    const prior = hostCreateReplays.get(replayKey);
    if (prior) {
      if (prior.request !== normalizedRequest) return failure("idempotency_conflict", "幂等键已用于不同的主机创建请求", 409);
      return json(materializeHost(prior.response), 201);
    }
    if (hosts.filter((host) => host.owner_id === user.id).length >= user.quota.hosts) {
      return failure("quota_exceeded", "达到云端电脑上限,无法再创建", 409, { dimension: "hosts", limit: user.quota.hosts });
    }
    const selectedWorkspaces = (body.workspace_ids ?? []).map((workspaceId) => workspaces.find((item) => item.id === workspaceId && workspaceOwners.get(item.id) === user.id));
    if (selectedWorkspaces.some((workspace) => !workspace || workspace.state !== "ready")) return failure("workspace_busy", "工作区不存在、正在删除或无权访问", 409);
    const usage = usageFor(user.id);
    const quotaCheck = [
      ["cpu_milli", usage.cpu_milli, resources.cpu_milli, user.quota.cpu_milli],
      ["mem_mib", usage.mem_mib, resources.mem_mib, user.quota.mem_mib],
    ] as const;
    const quotaExceeded = quotaCheck.find(([, used, requested, limit]) => used + requested > limit);
    if (quotaExceeded) return failure("quota_exceeded", "已达到用户主机配额上限", 409, { dimension: quotaExceeded[0], limit: quotaExceeded[3], used: quotaExceeded[1], requested: quotaExceeded[2] });
    const allocated = capacityUsage();
    const capacityExceeded = (["cpu_milli", "mem_mib"] as const).find((dimension) => allocated[dimension] + resources[dimension] > capacityLimit[dimension]);
    if (capacityExceeded) return failure("capacity_exceeded", "集群资源容量不足", 409, { dimension: capacityExceeded, limit: capacityLimit[capacityExceeded], allocated: allocated[capacityExceeded], requested: resources[capacityExceeded] });
    const timestamp = now();
    const host: Host = { id: id("h"), name: body.name?.trim() || template.name, owner_id: user.id, template_id: template.id, isolation: template.isolation, state: "creating", state_reason: "", runtime_generation: 1, control_version: 0, control_state: "ai", url: import.meta.env.VITE_WORKBENCH_URL ?? "http://127.0.0.1:5174", workspaces: selectedWorkspaces.filter((workspace): workspace is Workspace => Boolean(workspace)).map((workspace): WorkspaceBinding => ({ workspace: toWorkspaceRef(workspace), directory: `projects/${workspace.id}`, state: "active", binding_version: 1, desired_revision: workspace.revision, applied_revision: workspace.revision, sync_state: "healthy", sync_conflicts: [] })), resources, expires_at: new Date(Date.now() + template.ttl_seconds * 1000).toISOString(), last_active_at: timestamp, created_at: timestamp };
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
    persistAssistantState();
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
  http.get("/api/capabilities", () => json({ service_instance_id: "00000000-0000-4000-8000-000000000001", api_version: "1",
    hub_version: null, event_version: null, minimum_client_version: null, supported_features: ["password_login"], default_max_request_bytes: 1048576 })),
  http.get("/api/conversations", ({ cookies }) => {
    const user = sessionUser(cookies)!;
    return json({ items: conversations.filter((item) => conversationOwners.get(item.id) === user.id)
      .sort((left, right) => right.updated_at.localeCompare(left.updated_at)), next_cursor: null });
  }),
  http.get("/api/conversations/:conversationID/messages", ({ params, cookies }) => {
    const user = sessionUser(cookies)!;
    const conversationID = String(params.conversationID);
    if (conversationOwners.get(conversationID) !== user.id) return failure("not_found", "对话不存在", 404);
    return json({ items: conversationMessages.filter((item) => item.conversation_id === conversationID), next_cursor: null });
  }),
  http.post("/api/tasks", async ({ request, cookies }) => {
    const user = sessionUser(cookies)!;
    const key = request.headers.get("Idempotency-Key") ?? "";
    if (!/^[A-Za-z0-9._~-]{8,128}$/.test(key)) return failure("invalid_argument", "Idempotency-Key 无效", 400);
    const body = await request.json().catch(() => null) as {
      host_id?: string; workspace_id?: string; input_revision?: number; application_id?: string; model_id?: string;
      conversation_id?: string | null; objective?: string; budget?: { active_seconds: number; tool_calls: number; model_tokens: number };
    } | null;
    const allowed = ["host_id", "workspace_id", "input_revision", "application_id", "model_id", "conversation_id", "objective", "budget"];
    if (!body || Object.keys(body).some((field) => !allowed.includes(field)) || typeof body.model_id !== "string"
      || typeof body.objective !== "string" || !body.objective.trim() || Array.from(body.objective).length > 32768
      || (body.conversation_id !== undefined && body.conversation_id !== null && typeof body.conversation_id !== "string")) {
      return failure("invalid_argument", "任务内容无效", 400);
    }
    const requestText = JSON.stringify({ ...body, objective: body.objective.trim() });
    const replayKey = `${user.id}:${key}`;
    const replay = taskSubmitReplays.get(replayKey);
    if (replay) return replay.request === requestText ? json(replay.response, 202) : failure("idempotency_conflict", "幂等键已用于其他请求", 409);
    if (!modelConnections.some((item) => item.id === body.model_id && item.checked_at && item.capabilities.text)) return failure("not_found", "模型不存在", 404);
    let conversation = body.conversation_id ? conversations.find((item) => item.id === body.conversation_id && conversationOwners.get(item.id) === user.id) : undefined;
    if (body.conversation_id && !conversation) return failure("not_found", "对话不存在", 404);
    if (conversation && activeConversationTask(conversation.id)) return failure("task_busy", "对话中已有任务正在运行", 409);
    if (!conversation) {
      const timestamp = now();
      conversation = { id: id("c"), title: Array.from(body.objective.trim()).slice(0, 120).join(""), created_at: timestamp, updated_at: timestamp };
      conversations.push(conversation);
      conversationOwners.set(conversation.id, user.id);
    }
    const { task } = createTaskMessage(user, conversation, body.model_id, body.objective.trim(), body);
    taskSubmitReplays.set(replayKey, { request: requestText, response: task });
    persistAssistantState();
    return json(task, 202);
  }),
  http.post("/api/conversations/:conversationID/messages", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const conversationID = String(params.conversationID);
    const conversation = conversations.find((item) => item.id === conversationID && conversationOwners.get(item.id) === user.id);
    if (!conversation) return failure("not_found", "对话不存在", 404);
    const key = request.headers.get("Idempotency-Key") ?? "";
    if (!/^[A-Za-z0-9._~-]{8,128}$/.test(key)) return failure("invalid_argument", "Idempotency-Key 无效", 400);
    const body = await request.json().catch(() => null) as {
      host_id?: string; workspace_id?: string; input_revision?: number; application_id?: string; model_id?: string;
      content?: string; budget?: { active_seconds: number; tool_calls: number; model_tokens: number };
    } | null;
    const allowed = ["host_id", "workspace_id", "input_revision", "application_id", "model_id", "content", "budget"];
    if (!body || Object.keys(body).some((field) => !allowed.includes(field)) || typeof body.model_id !== "string"
      || typeof body.content !== "string" || !body.content.trim() || Array.from(body.content).length > 32768) {
      return failure("invalid_argument", "对话内容无效", 400);
    }
    const requestText = JSON.stringify({ conversation_id: conversationID, ...body, content: body.content.trim() });
    const replayKey = `${user.id}:${key}`;
    const replay = taskMessageReplays.get(replayKey);
    if (replay) return replay.request === requestText ? json(replay.response, 202) : failure("idempotency_conflict", "幂等键已用于其他请求", 409);
    if (!modelConnections.some((item) => item.id === body.model_id && item.checked_at && item.capabilities.text)) return failure("not_found", "模型不存在", 404);
    if (activeConversationTask(conversationID)) return failure("task_busy", "对话中已有任务正在运行", 409);
    const result = createTaskMessage(user, conversation, body.model_id, body.content.trim(), body);
    taskMessageReplays.set(replayKey, { request: requestText, response: result });
    persistAssistantState();
    return json(result, 202);
  }),
  http.get("/api/tasks/:taskID", ({ params, cookies }) => {
    const user = sessionUser(cookies)!;
    const task = tasks.get(String(params.taskID));
    if (!task || task.owner_id !== user.id) return failure("not_found", "任务不存在", 404);
    return json(task);
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
  http.get("/api/apikeys", ({ cookies }) => { const user = sessionUser(cookies)!; return json({ items: apiKeys.filter((key) => key.owner_id === user.id).map(hideOwner) }); }),
  http.post("/api/apikeys", async ({ request, cookies }) => {
    const user = sessionUser(cookies)!;
    const body = await request.json().catch(() => null) as { name?: unknown; ttl_seconds?: unknown } | null;
    if (!body || Object.keys(body).some((field) => field !== "name" && field !== "ttl_seconds") || typeof body.name !== "string" || !body.name.trim() || body.name.length > 100) return failure("invalid_argument", "API Key 名称无效", 400);
    if (body.ttl_seconds !== undefined && (!Number.isInteger(body.ttl_seconds) || Number(body.ttl_seconds) < 1 || Number(body.ttl_seconds) > 31536000)) return failure("invalid_argument", "有效期无效", 400);
    const key = { id: id("t"), name: body.name.trim(), owner_id: user.id, expires_at: new Date(Date.now() + (Number(body.ttl_seconds) || 86400) * 1000).toISOString(), last_used_at: null, revoked: false, created_at: now() };
    apiKeys.push(key);
    persistAssistantState();
    return json({ id: key.id, key: "chk_mock-one-time-key", expires_at: key.expires_at }, 201);
  }),
  http.delete("/api/apikeys/:keyId", ({ params, cookies }) => {
    const user = sessionUser(cookies)!;
    const key = apiKeys.find((item) => item.id === String(params.keyId) && item.owner_id === user.id);
    if (!key) return failure("not_found", "API Key 不存在", 404);
    key.revoked = true;
    persistAssistantState();
    return empty();
  }),
  http.get("/api/admin/hosts", ({ cookies }) => { const user = sessionUser(cookies)!; completeDeletions(); return isAdmin(user) ? json({ items: hosts.map(materializeHost) }) : failure("forbidden", "没有执行此操作的权限", 403); }),
  http.get("/api/admin/hosts/:hostId/resources", ({ params, cookies }) => {
    if (!isAdmin(sessionUser(cookies)!)) return failure("forbidden", "没有执行此操作的权限", 403);
    const host = hosts.find((item) => item.id === String(params.hostId));
    return host ? json({ host_id: host.id, state: host.state, resources: { cpu_milli: host.resources.cpu_milli, mem_mib: host.resources.mem_mib }, version: hostResourceVersions.get(host.id) ?? 1, update_state: "idle" }) : failure("not_found", "云端电脑不存在", 404);
  }),
  http.patch("/api/admin/hosts/:hostId/resources", async ({ params, request, cookies }) => {
    if (!isAdmin(sessionUser(cookies)!)) return failure("forbidden", "没有执行此操作的权限", 403);
    const host = hosts.find((item) => item.id === String(params.hostId));
    if (!host) return failure("not_found", "云端电脑不存在", 404);
    if (host.state !== "stopped" && host.state !== "running") return failure("invalid_state", "当前电脑状态不能调整规格", 409);
    const body = await request.json().catch(() => null) as { cpu_milli?: number; mem_mib?: number; expected_version?: number } | null;
    if (!body || typeof body !== "object" || Array.isArray(body) || Object.keys(body).some((field) => field !== "cpu_milli" && field !== "mem_mib" && field !== "expected_version") ||
      !Number.isInteger(body.cpu_milli) || !Number.isInteger(body.mem_mib) || !Number.isInteger(body.expected_version) ||
      Number(body.cpu_milli) < 1 || Number(body.mem_mib) < 1 || Number(body.expected_version) < 1) return failure("invalid_argument", "电脑规格无效", 400);
    const version = hostResourceVersions.get(host.id) ?? 1;
    if (body.expected_version !== version) return failure("conflict", "电脑规格已变化", 409);
    const owner = users.find((user) => user.id === host.owner_id);
    if (!owner) return failure("not_found", "用户不存在", 404);
    const ownerUsage = usageFor(owner.id);
    const clusterUsage = capacityUsage();
    const cpuIncrease = host.state === "running" ? Math.max(0, body.cpu_milli! - host.resources.cpu_milli) : body.cpu_milli!;
    const memIncrease = host.state === "running" ? Math.max(0, body.mem_mib! - host.resources.mem_mib) : body.mem_mib!;
    if (ownerUsage.cpu_milli + cpuIncrease > owner.quota.cpu_milli || ownerUsage.mem_mib + memIncrease > owner.quota.mem_mib) {
      return failure("quota_exceeded", "已达到用户主机配额上限", 409);
    }
    if (clusterUsage.cpu_milli + cpuIncrease > capacityLimit.cpu_milli || clusterUsage.mem_mib + memIncrease > capacityLimit.mem_mib) {
      return failure("capacity_exceeded", "集群资源容量不足", 409);
    }
    host.resources = { cpu_milli: body.cpu_milli!, mem_mib: body.mem_mib!, cpu_limit_milli: body.cpu_milli!, mem_limit_mib: body.mem_mib! };
    hostResourceVersions.set(host.id, version + 1);
    persistAssistantState();
    return json({ host_id: host.id, state: host.state, resources: { cpu_milli: body.cpu_milli, mem_mib: body.mem_mib }, version: version + 1, update_state: "idle" });
  }),
  http.get("/api/admin/workspaces", ({ cookies }) => {
    const user = sessionUser(cookies)!;
    if (!isAdmin(user)) return failure("forbidden", "没有执行此操作的权限", 403);
    completeDeletions();
    return json({ items: workspaces.filter(isVisibleAdminWorkspace).map(adminWorkspaceView), next_cursor: null });
  }),
  http.get("/api/admin/workspaces/:workspaceId", ({ params, cookies }) => {
    const user = sessionUser(cookies)!;
    if (!isAdmin(user)) return failure("forbidden", "没有执行此操作的权限", 403);
    completeDeletions();
    const workspace = workspaces.filter(isVisibleAdminWorkspace).find((item) => item.id === String(params.workspaceId));
    return workspace ? json(adminWorkspaceView(workspace)) : failure("not_found", "工作区不存在", 404);
  }),
  http.get("/api/admin/users", ({ request, cookies }) => {
    const user = sessionUser(cookies)!;
    if (!isAdmin(user)) return failure("forbidden", "没有执行此操作的权限", 403);
    const requested = new URL(request.url).searchParams.getAll("status");
    if (requested.length > 1 || (requested.length === 1 && !validAccountStatus(requested[0]!))) return failure("invalid_argument", "账号状态无效", 400);
    return json({ items: users.filter((item) => !requested.length || item.status === requested[0]).map(userWithUsage) });
  }),
  http.patch("/api/admin/users/:userId", async ({ params, request, cookies }) => {
    const actor = sessionUser(cookies)!;
    if (!isAdmin(actor)) return failure("forbidden", "没有执行此操作的权限", 403);
    const user = users.find((item) => item.id === String(params.userId));
    if (!user) return failure("not_found", "用户不存在", 404);
    if (user.name === "admin" || user.role === "super_admin") return failure("forbidden", "超级管理员账号受保护", 403);
    const body = await request.json().catch(() => ({})) as { expected_status?: string; status?: string; role?: string; quota?: User["quota"]; default_host_resources?: User["default_host_resources"] };
    if (body.expected_status === undefined || !validAccountStatus(body.expected_status)) return failure("invalid_argument", "expected_status 无效", 400);
    if (body.expected_status !== user.status) return failure("conflict", "账号状态已变化，请刷新列表", 409);
    if (body.status !== undefined && !validAccountStatus(body.status)) return failure("invalid_argument", "账号状态无效", 400);
    if (body.role !== undefined && body.role !== "user" && body.role !== "admin") return failure("invalid_argument", "角色无效", 400);
    if (body.status === undefined && body.role === undefined && body.quota === undefined && body.default_host_resources === undefined) return failure("invalid_argument", "至少需要一个更新字段", 400);
    const nextStatus = body.status ?? user.status;
    if (body.status !== undefined && !nextAccountStatuses[user.status].includes(body.status)) return failure("conflict", "不允许此账号状态转换", 409);
    if (body.status !== undefined && actor.id === user.id) return failure("forbidden", "不能修改自己的账号状态", 403);
    if (body.role !== undefined) {
      if (user.status !== "approved" || nextStatus !== "approved") return failure("conflict", "只有已通过账号可以调整角色", 409);
      if (actor.id === user.id) return failure("forbidden", "不能修改自己的角色", 403);
    }
    if (body.quota !== undefined) {
      if (!body.quota || Object.values(body.quota).some((value) => !Number.isInteger(value) || value < 0)) return failure("invalid_argument", "配额无效", 400);
    }
    if (body.default_host_resources !== undefined && (!body.default_host_resources || !Number.isInteger(body.default_host_resources.cpu_milli)
      || !Number.isInteger(body.default_host_resources.mem_mib) || body.default_host_resources.cpu_milli < 1 || body.default_host_resources.mem_mib < 1)) {
      return failure("invalid_argument", "默认电脑规格无效", 400);
    }
    const credentialsChanged = nextStatus !== user.status || body.role !== undefined;
    user.status = nextStatus;
    if (body.role !== undefined) user.role = body.role;
    if (body.quota !== undefined) user.quota = body.quota;
    if (body.default_host_resources !== undefined) user.default_host_resources = body.default_host_resources;
    if (credentialsChanged) revokeUserSessions(user.id);
    auditLogs.unshift({ id: auditLogs.length + 1, at: now(), actor_id: actor.id, actor_kind: "user", action: "user.update", target: user.id, detail: { ...(body.status ? { status: body.status } : {}), ...(body.role ? { role: body.role } : {}), ...(body.quota ? { quota: body.quota } : {}), ...(body.default_host_resources ? { default_host_resources: body.default_host_resources } : {}) }, ip: "127.0.0.1" });
    persistAssistantState();
    return json(userWithUsage(user));
  }),
  http.get("/api/admin/capacity", ({ cookies }) => isAdmin(sessionUser(cookies)!) ? json({ limit: capacityLimit, allocated: capacityUsage(), hosts_by_state: hostsByState() }) : failure("forbidden", "没有执行此操作的权限", 403)),
  http.get("/api/admin/audit", ({ request, cookies }) => {
    if (!isAdmin(sessionUser(cookies)!)) return failure("forbidden", "没有执行此操作的权限", 403);
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
