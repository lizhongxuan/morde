import { http, HttpResponse } from "msw";
import type { components } from "../api/schema";
import type { ConversationDeliveryStatus, HostMemberPermission, MainConversation } from "../app/api";
import { createK2TaskFixture } from "./k2-task-fixture";

type User = { id: string; name: string; role: components["schemas"]["UserRole"]; status: components["schemas"]["AccountStatus"]; must_change_password: boolean; quota: { hosts: number; cpu_milli: number; mem_mib: number }; usage: { hosts: number; cpu_milli: number; mem_mib: number }; default_host_resources: { cpu_milli: number; mem_mib: number }; created_at?: string };
type AuditLog = { id: number; at: string; actor_id: string; actor_kind: string; action: string; target: string; detail: Record<string, unknown>; ip: string };
type Workspace = components["schemas"]["Workspace"];
type WorkspaceRef = components["schemas"]["WorkspaceRef"];
type WorkspaceBinding = components["schemas"]["WorkspaceBinding"];
type Host = Omit<components["schemas"]["Host"], "workspaces"> & { sandbox_id?: string | null; workspaces: WorkspaceBinding[] };
type HostCopyResult = components["schemas"]["HostCopyResult"];
type ControlTransition = components["schemas"]["ControlTransition"];
type ControlActionRequest = components["schemas"]["ControlActionRequest"];
type ControlActionResponse = components["schemas"]["ControlActionResponse"];
type HostMember = { user_id: string; permissions: HostMemberPermission[]; created_at: string };
type ModelConnection = components["schemas"]["ModelConnection"];
type MCPConnector = components["schemas"]["MCPConnector"];
type AdminModelDefault = components["schemas"]["AdminModelDefault"];
type Conversation = components["schemas"]["Conversation"];
type ConversationMessage = components["schemas"]["ConversationMessage"];
type MockConversationMessage = ConversationMessage & { delivery_status?: ConversationDeliveryStatus };
type MockVariantAction = 'edit_resend' | 'regenerate_text' | 'continue_generation';
type MockVariantMessage = {
  id: string; conversation_id: string; sequence: number; role: 'user' | 'assistant'; content: string;
  task_id?: string; related_task_id?: string; routing_status?: string; output_status?: string;
  actor_id?: string; owner_id?: string; delivery_status?: string; created_at: string;
  client_message_id?: string;
  variant_group_id?: string; variant_version?: number; variant_action?: 'original' | MockVariantAction;
  sources?: { user_message_id: string }[];
};
type MockVariantResponse = {
  operation_id: string; variant_group_id: string; variant_version: number; action: MockVariantAction; state: 'accepted';
  user_message_id?: string; assistant_message_id?: string; task_id?: string;
};
type Task = components["schemas"]["Task"];
type TaskStep = components["schemas"]["TaskStep"];
type TaskStepRetryRequest = components["schemas"]["TaskStepRetryRequest"];
type TaskStepRetryResult = components["schemas"]["TaskStepRetryResult"];
type Project = components["schemas"]["Project"];
type ProjectMember = components["schemas"]["ProjectMember"];
type ProjectInvitation = components["schemas"]["ProjectInvitation"];
type ProjectResource = components["schemas"]["ProjectResource"];
type ProjectWorkspaceManifest = components["schemas"]["ProjectWorkspaceManifest"];
type ProjectBudget = components["schemas"]["ProjectBudget"];
type ProjectBudgetLedgerEntry = components["schemas"]["ProjectBudgetLedgerEntry"];
type ProjectWorkItemCategory = "bug" | "request" | "unclassified";
type ProjectWorkItem = Omit<components["schemas"]["ProjectWorkItem"], "assignee_id" | "category"> & {
  assignee_id: string | null;
  category: ProjectWorkItemCategory;
};
type CreateProjectWorkItemRequest = Omit<components["schemas"]["CreateProjectWorkItemRequest"], "category" | "unassigned"> & {
  category?: ProjectWorkItemCategory;
  unassigned?: boolean;
};
type ProjectConversation = components["schemas"]["ProjectConversation"];
type ProjectConversationMessage = components["schemas"]["ProjectConversationMessage"];
type AcceptedProjectConversationMessageResponse = components["schemas"]["AcceptedProjectConversationMessageResponse"];
type Schedule = components["schemas"]["Schedule"];
type ScheduleCreateRequest = components["schemas"]["ScheduleCreateRequest"];
type PersonalObject = components["schemas"]["PersonalObject"];
type GoalBody = components["schemas"]["GoalBody"];
type PersonalMutationReceipt = components["schemas"]["PersonalMutationReceipt"];
type UserPreferences = components["schemas"]["UserPreferences"];
type Notification = components["schemas"]["Notification"];
type ClientInstallation = components["schemas"]["ClientInstallation"];
type AdminWorkspace = components["schemas"]["AdminWorkspace"];
type VisibleAdminWorkspace = Omit<Workspace, "state"> & { state: "ready" | "deleting" };
type ModelAccountBudget = { owner_id: string; configured: boolean; token_limit: number; max_concurrent: number; used_tokens: number; in_flight: number; has_unknown: boolean };
type AdminModelBudgetDefault = components["schemas"]["AdminModelBudgetDefault"];
type PersonalDataQuota = components["schemas"]["AdminPersonalDataQuota"];
type AdminControlRule = components["schemas"]["AdminControlRule"];
type AdminControlNotification = components["schemas"]["AdminControlNotification"];
type RecordCapability = components["schemas"]["RecordCapability"];
type RecordConsent = components["schemas"]["RecordConsent"];
type RecordQuota = components["schemas"]["RecordQuota"];
type RecordAdminGrant = components["schemas"]["RecordAdminGrant"];
type AdminInvite = components["schemas"]["AdminInvite"];
type AdminNudgePolicy = components["schemas"]["AdminNudgePolicy"];

const mockServiceInstanceID = "00000000-0000-4000-8000-000000000001";

const defaultPersonalDataQuota = {
  used_bytes: 0,
  reserved_bytes: 0,
  quota_bytes: 5_368_709_120,
  max_attachment_bytes: 314_572_800,
  version: 1,
  default_quota_bytes: 5_368_709_120,
  default_max_attachment_bytes: 314_572_800,
} satisfies PersonalDataQuota;

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
const modelConnectionOwners = new Map<string, string>();
let mcpConnectors: MCPConnector[] = [];
const mcpConnectorOwners = new Map<string, string>();
let defaultModelID: string | null = null;
let defaultModelVersion = 0;
let schedules: Array<{ ownerID: string; value: Schedule }> = [];
let conversations: Conversation[] = [];
let conversationMessages: MockConversationMessage[] = [];
type MockMessageUpload = {
  ownerID: string;
  projectID: string | null;
  conversationID: string;
  clientMessageID: string;
  metadata: Record<string, unknown>;
  totalBytes: number;
  totalChunks: number;
  sha256: string;
  fingerprint: string;
  chunks: Map<number, Uint8Array>;
  message?: ConversationMessage | ProjectConversationMessage;
};
const conversationMessageUploads = new Map<string, MockMessageUpload>();
let conversationOwners = new Map<string, string>();
let mainConversations = new Map<string, MainConversation>();
let tasks = new Map<string, Task>();
let projects: Project[] = [];
let projectMembers: ProjectMember[] = [];
let projectInvitations: ProjectInvitation[] = [];
let projectResources: ProjectResource[] = [];
let projectWorkspaceManifests = new Map<string, ProjectWorkspaceManifest>();
let projectBudgets = new Map<string, ProjectBudget>();
let projectLedgers = new Map<string, ProjectBudgetLedgerEntry[]>();
let projectWorkItems: ProjectWorkItem[] = [];
let projectConversations: ProjectConversation[] = [];
let projectConversationMessages: ProjectConversationMessage[] = [];

function nextConversationMessageSequence(conversationID: string): number {
  const personal = conversationMessages.filter((message) => message.conversation_id === conversationID);
  const project = projectConversationMessages.filter((message) => message.conversation_id === conversationID);
  return Math.max(0, ...personal.map((message) => message.sequence), ...project.map((message) => message.sequence)) + 1;
}

function mockVariantRoot(target: MockVariantMessage, messages: readonly MockVariantMessage[]): string | null {
  if (target.variant_group_id) return target.variant_group_id;
  if (target.role === 'user') return target.id;
  const sources = [...new Set((target.sources ?? []).map((source) => source.user_message_id))];
  if (sources.length === 1) return sources[0] ?? null;
  if (target.task_id) {
    const users = [...new Set(messages.filter((message) => message.role === 'user' &&
      (message.task_id === target.task_id || message.related_task_id === target.task_id)).map((message) => message.variant_group_id ?? message.id))];
    if (users.length === 1) return users[0] ?? null;
  }
  return null;
}

function createMockMessageVariant(input: {
  user: User; request: Request; projectID?: string; conversationID: string; targetID: string;
  messages: Array<MockConversationMessage | ProjectConversationMessage>;
  projectConversation?: ProjectConversation;
}) {
  const { user, request, projectID, conversationID, targetID, messages, projectConversation } = input;
  const identityError = request.headers.get('X-Morde-Service-Instance-ID') !== mockServiceInstanceID
    ? failure('conflict', '服务身份不匹配', 409)
    : request.headers.get('X-Morde-Actor-ID') !== user.id ? failure('conflict', '账号身份不匹配', 409) : null;
  if (identityError) return identityError;
  const idempotencyKey = request.headers.get('Idempotency-Key')?.trim() ?? '';
  if (!/^[A-Za-z0-9._~-]{8,128}$/.test(idempotencyKey)) return failure('invalid_argument', 'Idempotency-Key 无效', 400);
  return request.json().catch(() => null).then((raw: unknown) => {
    const body = raw as { action?: unknown; expected_version?: unknown; content?: unknown } | null;
    const action = body?.action;
    if (!body || Object.keys(body).some((key) => !['action', 'expected_version', 'content'].includes(key)) ||
      !['edit_resend', 'regenerate_text', 'continue_generation'].includes(String(action)) ||
      !Number.isSafeInteger(body.expected_version) || Number(body.expected_version) < 1 ||
      (action === 'edit_resend' ? typeof body.content !== 'string' || !body.content.trim() || Array.from(body.content).length > 32768 : body.content !== undefined)) {
      return failure('invalid_argument', '消息版本操作请求无效', 400);
    }
    const target = messages.find((candidate) => candidate.id === targetID && candidate.conversation_id === conversationID) as MockVariantMessage | undefined;
    if (!target) return failure('not_found', '消息不存在', 404);
    if (projectID && (!projectConversation || target.owner_id !== projectConversation.owner_id)) return failure('not_found', '项目共享对话消息不存在', 404);
    if (projectID && target.actor_id !== user.id) return failure('forbidden', '只有消息发起人可以操作该版本', 403);
    const rootID = mockVariantRoot(target, messages as MockVariantMessage[]);
    if (!rootID) return failure('invalid_state', '无法确认消息的原始版本组', 409);
    if ((action === 'edit_resend' && target.role !== 'user') || (action !== 'edit_resend' && target.role !== 'assistant')) {
      return failure('invalid_state', '消息角色不支持此版本操作', 409);
    }
    if (action === 'continue_generation' && target.output_status !== 'interrupted') return failure('invalid_state', '当前回复没有可继续的检查点', 409);
    const scopedKey = `${user.id}:${projectID ?? 'personal'}:${conversationID}:${idempotencyKey}`;
    const requestText = canonicalJSON({ conversationID, targetID, ...(projectID ? { projectID } : {}), ...body });
    const replay = messageVariantReplays.get(scopedKey);
    if (replay) return replay.request === requestText ? json(replay.response, 202) : failure('idempotency_conflict', '幂等键已用于其他消息版本操作', 409);
    const groupRows = (messages as MockVariantMessage[]).filter((candidate) => candidate.variant_group_id === rootID || candidate.id === rootID);
    const latestVersion = Math.max(1, ...groupRows.map((candidate) => candidate.variant_version ?? 1));
    if (body.expected_version !== latestVersion) return failure('conflict', '消息版本已变化，请刷新后重试', 409);
    const version = latestVersion + 1;
    const timestamp = now();
    const role = action === 'edit_resend' ? 'user' : 'assistant';
    const nextID = id('m');
    const nextContent = action === 'edit_resend' ? String(body.content) : action === 'continue_generation'
      ? `${target.content}\n\n（本机演示：续写结果）` : `${target.content}\n\n（本机演示：重新生成的文字版本）`;
    const next: MockVariantMessage = {
      id: nextID, conversation_id: conversationID, sequence: nextConversationMessageSequence(conversationID), role, content: nextContent,
      ...(target.task_id ? { task_id: target.task_id } : {}), ...(target.related_task_id ? { related_task_id: target.related_task_id } : {}),
      ...(role === 'user' ? { routing_status: 'pending', delivery_status: 'pending', client_message_id: nextID } : { output_status: 'completed', delivery_status: 'applied' }),
      created_at: timestamp, variant_group_id: rootID, variant_version: version, variant_action: action as MockVariantAction,
      ...(projectID ? { actor_id: user.id, owner_id: projectConversation!.owner_id } : {}),
    };
    messages.push(next as MockConversationMessage & ProjectConversationMessage);
    if (projectConversation) projectConversation.updated_at = timestamp;
    const response: MockVariantResponse = {
      operation_id: id('op'), variant_group_id: rootID, variant_version: version, action: action as MockVariantAction, state: 'accepted',
      ...(role === 'user' ? { user_message_id: nextID } : { assistant_message_id: nextID }),
      ...(target.task_id ? { task_id: target.task_id } : {}),
    };
    messageVariantReplays.set(scopedKey, { request: requestText, response });
    if (!projectID) persistAssistantState();
    return json(response, 202);
  });
}

const projectConversationCreates = new Map<string, { request: string; response: ProjectConversation }>();
const projectConversationAcceptances = new Map<string, { request: string; response: AcceptedProjectConversationMessageResponse }>();
const messageVariantReplays = new Map<string, { request: string; response: MockVariantResponse }>();
const conversationCreates = new Map<string, { request: string; response: Conversation }>();
const projectWorkItemClaimReplays = new Map<string, { request: string; response: ProjectWorkItem }>();
const taskSubmitReplays = new Map<string, { request: string; response: Task }>();
const conversationAcceptances = new Map<string, { request: string; response: components["schemas"]["AcceptedConversationMessageResponse"] }>();
const taskActionReplays = new Map<string, { request: string; response: components["schemas"]["TaskActionResponse"] }>();
const taskStepRetryReplays = new Map<string, { request: string; response: TaskStepRetryResult }>();
const mainConversationReplays = new Map<string, { request: string; response: MainConversation }>();
let userPreferences = new Map<string, UserPreferences>();
let notifications: Notification[] = [];
let clients: ClientInstallation[] = [];
const clientOwners = new Map<string, string>();
const modelBudgets = new Map<string, ModelAccountBudget>();
let defaultModelBudget: AdminModelBudgetDefault = { configured: false, token_limit: 0, max_concurrent: 0, version: 0 };
const personalDataQuotas = new Map<string, PersonalDataQuota>();
let personalObjects = new Map<string, { ownerID: string; value: PersonalObject }>();
let personalChanges: Array<{ ownerID: string; value: components["schemas"]["PersonalChange"] }> = [];
const personalSequences = new Map<string, number>();
const personalMutationReplays = new Map<string, { request: string; response: PersonalMutationReceipt }>();
const personalMemoryHistory = new Map<string, components["schemas"]["PersonalMemoryRevision"][]>();
const personalMemoryHistoryCursors = new Map<string, { ownerID: string; objectID: string; offset: number }>();
const recordConsents = new Map<string, RecordConsent[]>();
const recordQuotas = new Map<string, RecordQuota>();
const recordGrants = new Map<string, RecordAdminGrant[]>();
let adminControlRules: AdminControlRule[] = [];
let adminControlNotifications: AdminControlNotification[] = [];
let nextAdminControlRuleId = 1;
let notificationOwners = new Map<string, string>();
const workspaceCreateReplays = new Map<string, { request: string; response: Workspace }>();
const workspaceRestoreReplays = new Map<string, { request: string; response: components["schemas"]["WorkspaceRestoreResult"] }>();
const hostCreateReplays = new Map<string, { request: string; response: Host }>();
const hostCopyReplays = new Map<string, { request: string; response: HostCopyResult }>();
const hostControlTransitions = new Map<string, ControlTransition>();
const hostControlActionReplays = new Map<string, { request: string; response: ControlActionResponse }>();
const hostControlLeases = new Map<string, string>();
const workspaceBindReplays = new Map<string, { request: string; response: WorkspaceBinding }>();
const workspaceUnbindReplays = new Map<string, { request: string; response: WorkspaceBinding }>();
const workspaceUnbindingStartedAt = new Map<string, number>();
const users: User[] = [demoUser, memberUser, initialAdmin];
let inviteCodes = new Map<string, string>();
let inviteRelations: Array<{ inviter_id: string; invitee_id: string; created_at: string }> = [];
let adminNudgePolicy: AdminNudgePolicy = { enabled: false, url: "", text_zh: "", text_en: "", version: 1, updated_at: null };
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
const personalObjectKey = (ownerID: string, type: string, objectID: string) => `${ownerID}\u0000${type}\u0000${objectID}`;
function savePersonalObject(ownerID: string, value: PersonalObject, mutationID: string): number {
  const sequence = (personalSequences.get(ownerID) ?? 0) + 1;
  personalSequences.set(ownerID, sequence);
  personalObjects.set(personalObjectKey(ownerID, value.object_type, value.object_id), { ownerID, value });
  personalChanges.push({ ownerID, value: {
    change_sequence: sequence, object_type: value.object_type, object_id: value.object_id,
    revision: value.revision, operation: "upsert", mutation_id: mutationID,
  } });
  return sequence;
}

function validGoalBody(value: unknown): value is GoalBody {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const body = value as Record<string, unknown>;
  return typeof body.title === "string" && body.title.length >= 1 && body.title.length <= 200 &&
    typeof body.description === "string" && body.description.length <= 4096 &&
    ["active", "paused", "done", "cancelled"].includes(String(body.status)) &&
    Array.isArray(body.steps) && body.steps.length <= 100 && body.steps.every((raw) => {
      if (!raw || typeof raw !== "object" || Array.isArray(raw)) return false;
      const step = raw as Record<string, unknown>;
      return typeof step.id === "string" && typeof step.title === "string" &&
        ["pending", "in_progress", "done", "blocked", "skipped"].includes(String(step.status)) &&
        (step.note === undefined || typeof step.note === "string");
    }) && Array.isArray(body.related_material_ids) && body.related_material_ids.length <= 100 &&
    body.related_material_ids.every((id) => typeof id === "string");
}

function validPersonalMutation(type: string, objectID: string, value: unknown): value is Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const body = value as Record<string, unknown>;
  const only = (keys: string[]) => Object.keys(body).every((key) => keys.includes(key));
  if (type === "memory") return /^m_[a-f0-9]{8}$/.test(objectID) && only(["content", "category"]) &&
    typeof body.content === "string" && body.content.length > 0 && body.content.length <= 65536 &&
    (body.category === undefined || (typeof body.category === "string" && body.category.length <= 64));
  if (type === "profile") return objectID === "default" && only(["name", "avatar", "emoji", "color", "description", "style"]) &&
    typeof body.name === "string" && body.name.trim() === body.name && body.name.length > 0 && body.name.length <= 60 &&
    (body.avatar === "dragon" || body.avatar === "emoji") &&
    (body.emoji === undefined || (typeof body.emoji === "string" && [...body.emoji].length <= 16)) &&
    (body.color === undefined || (typeof body.color === "string" && (body.color === "" || /^#[0-9a-fA-F]{6}$/.test(body.color)))) &&
    (body.description === undefined || (typeof body.description === "string" && [...body.description].length <= 200)) &&
    (body.style === undefined || ["", "muse", "flat", "clay", "watercolor", "pixel", "line", "sticker"].includes(String(body.style))) &&
    (body.avatar === "dragon" ? (!body.emoji || body.emoji === "") && (!body.color || body.color === "") : Boolean(body.emoji) && typeof body.color === "string" && /^#[0-9a-fA-F]{6}$/.test(body.color));
  if (!/^[A-Za-z0-9._~-]{1,160}$/.test(objectID)) return false;
  if (type === "feed_post") return only(["title", "type", "emoji", "created_at", "liked", "body"]) &&
    typeof body.title === "string" && [...body.title].length > 0 && [...body.title].length <= 200 &&
    typeof body.type === "string" && [...body.type].length > 0 && [...body.type].length <= 32 &&
    typeof body.body === "string" && [...body.body].length > 0 && [...body.body].length <= 65536 &&
    (body.emoji === undefined || (typeof body.emoji === "string" && [...body.emoji].length <= 16)) &&
    (body.created_at === undefined || (typeof body.created_at === "string" && body.created_at.includes("T") && Number.isFinite(Date.parse(body.created_at)))) &&
    (body.liked === undefined || typeof body.liked === "boolean");
  if (type === "feed_preferences") return objectID === "default" && only(["content"]) &&
    typeof body.content === "string" && [...body.content].length > 0 && [...body.content].length <= 8192;
  if (type === "idea_mark") return only(["state"]) && (body.state === "saved" || body.state === "dismissed");
  if (type === "library_item") return only(["name", "kind", "mime", "size_bytes", "attachment_id"]) &&
    typeof body.name === "string" && body.name.trim() === body.name && body.name.length > 0 && body.name.length <= 255 &&
    body.name !== "." && body.name !== ".." && ![...body.name].some((character) => character === "/" || character === "\\" ||
      (character.codePointAt(0) ?? 0) < 0x20 || character === "\u007f") &&
    (body.kind === "artifact" || body.kind === "media") &&
    (body.mime === undefined || (typeof body.mime === "string" && [...body.mime].length <= 128)) &&
    (body.size_bytes === undefined || (Number.isSafeInteger(body.size_bytes) && Number(body.size_bytes) >= 0)) &&
    (body.attachment_id === undefined || (typeof body.attachment_id === "string" && body.attachment_id.length > 0));
  if (type === "soul") return only(["name", "emoji", "style", "lang", "content"]) &&
    typeof body.name === "string" && [...body.name].length > 0 && [...body.name].length <= 80 &&
    typeof body.content === "string" && [...body.content].length > 0 && [...body.content].length <= 65536 &&
    (body.emoji === undefined || (typeof body.emoji === "string" && [...body.emoji].length <= 16)) &&
    (body.style === undefined || (typeof body.style === "string" && [...body.style].length <= 64)) &&
    (body.lang === undefined || (typeof body.lang === "string" && /^[A-Za-z0-9_-]{1,16}$/.test(body.lang)));
  return false;
}
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
  conversationMessages?: MockConversationMessage[];
  conversationOwners?: Array<[string, string]>;
  mainConversations?: Array<[string, MainConversation]>;
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
    mainConversations: Array.from(mainConversations.entries()),
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
  schedules = [];
  modelConnections = [{ id: "pmodel01", name: "团队示例模型", base_url: "https://models.example.invalid/v1", model_name: "morde-demo", shared: true, has_api_key: true, capabilities: { text: true, tools: true, vision: false, text_stream: false }, checked_at: now(), created_at: now(), updated_at: now() }];
  modelConnectionOwners.clear();
  mcpConnectors = [];
  mcpConnectorOwners.clear();
  defaultModelID = "pmodel01";
  defaultModelVersion = 1;
  conversations = [];
  conversationMessages = [];
  conversationMessageUploads.clear();
  conversationOwners = new Map();
  mainConversations = new Map();
  tasks = new Map();
  projects = [{ id: "pdem0001", owner_id: demoUser.id, name: "本地项目演示", description: "仅用于本机 Portal/MSW 验收。", version: 1 }];
  projectMembers = [{ project_id: "pdem0001", user_id: demoUser.id, role: "owner", version: 1 }];
  projectInvitations = [{ id: "rinvite1", project_id: "pdem0001", inviter_id: demoUser.id, invitee_id: memberUser.id, role: "member", state: "pending", version: 1 }];
  projectResources = [];
  projectWorkspaceManifests = new Map();
  projectBudgets = new Map([["pdem0001", { configured: true, currency: "USD", limit_micros: 10_000_000, version: 1, spent_micros: 0, reserved_micros: 500_000, has_unknown: true }]]);
  projectLedgers = new Map([["pdem0001", [{ call_id: "c-demo01", actor_id: demoUser.id, source: "client", model_id: "pmodel01", rate_version: 1, currency: "USD", status: "unknown", reserved_usage: { input_tokens: 100, output_tokens: 25 }, reserved_micros: 500_000, created_at: now() }]]]);
  projectWorkItems = [{ id: "jdem0001", project_id: "pdem0001", title: "确认交付范围", description: "本机演示工作项；不会启动 Task。", acceptance_criteria: ["确认负责人", "确认验收条件"], assignee_id: demoUser.id, category: "unclassified", state: "open", blocked_reason: "", task_ids: [], version: 1 }];
  projectConversations = [];
  projectConversationMessages = [];
  projectConversationCreates.clear();
  projectConversationAcceptances.clear();
  messageVariantReplays.clear();
  conversationCreates.clear();
  projectWorkItemClaimReplays.clear();
  taskSubmitReplays.clear();
  conversationAcceptances.clear();
  taskActionReplays.clear();
  taskStepRetryReplays.clear();
  mainConversationReplays.clear();
  userPreferences = new Map();
  notifications = [];
  clients = [{ id: "cli_demo01", platform: "desktop", display_name: "Morde 演示电脑", expires_at: new Date(Date.now() + 30 * 86_400_000).toISOString(), revoked: false, created_at: now(), last_used_at: null } as ClientInstallation];
  clientOwners.clear();
  clientOwners.set(clients[0]!.id, demoUser.id);
  modelBudgets.clear();
  defaultModelBudget = { configured: false, token_limit: 0, max_concurrent: 0, version: 0 };
  personalDataQuotas.clear();
  personalObjects = new Map();
  personalChanges = [];
  personalSequences.clear();
  personalMutationReplays.clear();
  personalMemoryHistory.clear();
  personalMemoryHistoryCursors.clear();
  recordConsents.clear();
  recordQuotas.clear();
  recordGrants.clear();
  adminControlRules = [];
  adminControlNotifications = [];
  nextAdminControlRuleId = 1;
  notificationOwners = new Map();
  workspaceCreateReplays.clear();
  workspaceRestoreReplays.clear();
  hostCreateReplays.clear();
  hostCopyReplays.clear();
  hostControlTransitions.clear();
  hostControlActionReplays.clear();
  hostControlLeases.clear();
  workspaceBindReplays.clear();
  workspaceUnbindReplays.clear();
  workspaceUnbindingStartedAt.clear();
  users.splice(0, users.length, demoUser, memberUser, initialAdmin);
  inviteCodes = new Map([["0123456789abcdef0123456789abcdef", demoUser.id]]);
  inviteRelations = [];
  adminNudgePolicy = { enabled: false, url: "", text_zh: "", text_en: "", version: 1, updated_at: null };
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
      if (Array.isArray(persisted.mainConversations)) mainConversations = new Map(persisted.mainConversations);
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
  if (import.meta.env.DEV && import.meta.env.VITE_MOCK_API === "1" && !workspaceOwners.has("wlocal01")) {
    const timestamp = now();
    workspaces = [{ id: "wlocal01", name: "本地差异演示工作区", revision: 2, used_bytes: 1024, quota_bytes: 20 * 1024 * 1024 * 1024, is_read_only: false,
      state: "ready", created_at: timestamp, updated_at: timestamp }, ...workspaces.filter((workspace) => workspace.id !== "wlocal01")];
    workspaceOwners.set("wlocal01", demoUser.id);
    projectResources = [{ project_id: "pdem0001", kind: "workspace", resource_id: "wlocal01", version: 1 }];
    projectWorkspaceManifests.set("wlocal01", {
      project_id: "pdem0001", workspace_id: "wlocal01", revision: 2,
      files: [
        { path: "README.md", kind: "file", size_bytes: 84, mode: 420 },
        { path: "src/main.go", kind: "file", size_bytes: 940, mode: 420 },
      ],
      empty_directories: ["docs/empty"], next_cursor: null,
    });
  }
  if (!notifications.length) {
    const timestamp = now();
    notifications = [
      { id: "ndemo001", kind: "waiting_user", task_id: "qreview1", event_sequence: 3, summary: "历史任务曾等待确认：发送部署完成通知？", redacted: false, actionable: false, created_at: timestamp, read_at: null },
      { id: "ndemo002", kind: "failed", task_id: "qbatch01", event_sequence: 5, summary: "任务失败：有一份文件未能写入目标目录。", redacted: false, actionable: false, created_at: new Date(Date.now() - 60 * 60 * 1000).toISOString(), read_at: null },
    ];
    notificationOwners = new Map(notifications.map((item) => [item.id, demoUser.id]));
  }
  if (import.meta.env.DEV && import.meta.env.VITE_MOCK_API === "1" && import.meta.env.VITE_K2_TASK_FIXTURE === "1"
  ) {
    const fixture = createK2TaskFixture(demoUser.id, defaultModelID ?? "pmodel01", now());
    conversations = [fixture.conversation, ...conversations.filter((conversation) => conversation.id !== fixture.conversation.id)];
    conversationMessages = [...conversationMessages.filter((message) => message.conversation_id !== fixture.conversation.id), ...fixture.messages];
    conversationOwners.set(fixture.conversation.id, demoUser.id);
    mainConversations.set(demoUser.id, fixture.mainConversation);
    for (const task of fixture.tasks) tasks.set(task.id, task);
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

function canAccessModel(user: User, model: ModelConnection): boolean {
  return model.shared || modelConnectionOwners.get(model.id) === user.id;
}

function isIanaTimeZone(value: unknown): boolean {
  if (typeof value !== "string" || !value) return false;
  try { new Intl.DateTimeFormat("en", { timeZone: value }); return true; } catch { return false; }
}

function isAutoMessageBody(value: unknown): value is components["schemas"]["AcceptConversationMessageRequest"] {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const body = value as Record<string, unknown>;
  const allowed = ["client_message_id", "previous_client_message_id", "reference_task_id", "model_id", "host_id", "workspace_id",
    "application_id", "mcp_connector_ids", "client_attachment_ids", "content", "time_zone", "budget"];
  if (Object.keys(body).some((field) => !allowed.includes(field)) || typeof body.client_message_id !== "string"
    || !/^[A-Za-z0-9._~-]{8,128}$/.test(body.client_message_id) || typeof body.content !== "string") return false;
  if (body.time_zone !== undefined && !isIanaTimeZone(body.time_zone)) return false;
  const attachmentIDs = body.client_attachment_ids;
  if (attachmentIDs !== undefined && (!Array.isArray(attachmentIDs) || attachmentIDs.length > 16
    || attachmentIDs.some((item) => typeof item !== "string" || !/^[A-Za-z0-9._~-]{8,128}$/.test(item))
    || new Set(attachmentIDs).size !== attachmentIDs.length)) return false;
  return Boolean(body.content.trim() || (Array.isArray(attachmentIDs) && attachmentIDs.length));
}

const messageUploadChunkBytes = 16 * 1024;
const messageUploadMaxBytes = 8 * 1024 * 1024;
const messageMetadataFields = ["previous_client_message_id", "reference_task_id", "model_id", "host_id", "workspace_id",
  "application_id", "mcp_connector_ids", "client_attachment_ids", "time_zone", "budget"] as const;

function canonicalJSON(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(canonicalJSON).join(",")}]`;
  if (value && typeof value === "object") {
    const object = value as Record<string, unknown>;
    return `{${Object.keys(object).sort().map((key) => `${JSON.stringify(key)}:${canonicalJSON(object[key])}`).join(",")}}`;
  }
  return JSON.stringify(value);
}

function messageUploadKey(ownerID: string, projectID: string | null, conversationID: string, clientMessageID: string): string {
  return `${ownerID}:${projectID ?? "personal"}:${conversationID}:${clientMessageID}`;
}

function messageUploadIdentityFailure(request: Request, user: User, clientMessageID: string, requireIdempotency: boolean) {
  if (!/^[A-Za-z0-9._~-]{8,128}$/.test(clientMessageID)) return failure("invalid_argument", "消息编号无效", 400);
  if (request.headers.get("X-Morde-Service-Instance-ID") !== mockServiceInstanceID) return failure("conflict", "服务身份不匹配", 409);
  if (request.headers.get("X-Morde-Actor-ID") !== user.id) return failure("conflict", "账号身份不匹配", 409);
  if (requireIdempotency && request.headers.get("Idempotency-Key")?.trim() !== clientMessageID) return failure("invalid_argument", "消息编号与请求编号不一致", 400);
  return null;
}

function validMessageUploadInit(value: unknown): value is components["schemas"]["ConversationMessageUploadInit"] {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const body = value as Record<string, unknown>;
  const allowed = new Set(["total_bytes", "total_chunks", "sha256", ...messageMetadataFields]);
  if (Object.keys(body).some((field) => !allowed.has(field))
    || !Number.isSafeInteger(body.total_bytes) || Number(body.total_bytes) < 1 || Number(body.total_bytes) > messageUploadMaxBytes
    || !Number.isSafeInteger(body.total_chunks) || Number(body.total_chunks) !== Math.ceil(Number(body.total_bytes) / messageUploadChunkBytes)
    || typeof body.sha256 !== "string" || !/^[a-f0-9]{64}$/.test(body.sha256)) return false;
  for (const field of ["previous_client_message_id", "reference_task_id", "model_id", "host_id", "workspace_id", "application_id"] as const) {
    const item = body[field];
    if (item !== undefined && (typeof item !== "string" || !item.length || item.length > (field === "model_id" ? 256 : field === "application_id" ? 64 : 128))) return false;
  }
  if (body.time_zone !== undefined && !isIanaTimeZone(body.time_zone)) return false;
  for (const field of ["mcp_connector_ids", "client_attachment_ids"] as const) {
    const items = body[field];
    if (items !== undefined && (!Array.isArray(items) || items.length > 16 || items.some((item) => typeof item !== "string"
      || !/^[A-Za-z0-9._~-]{8,128}$/.test(item)) || new Set(items).size !== items.length)) return false;
  }
  if (body.budget !== undefined && body.budget !== null) {
    if (!body.budget || typeof body.budget !== "object" || Array.isArray(body.budget)) return false;
    const budget = body.budget as Record<string, unknown>;
    if (Object.keys(budget).some((field) => !["active_seconds", "tool_calls", "model_tokens"].includes(field))
      || [budget.active_seconds, budget.tool_calls, budget.model_tokens].some((item) => !Number.isSafeInteger(item) || Number(item) < 0)) return false;
  }
  return true;
}

function messageUploadMetadata(value: components["schemas"]["ConversationMessageUploadInit"]): Record<string, unknown> {
  return Object.fromEntries(messageMetadataFields.filter((field) => value[field] !== undefined).map((field) => [field, value[field]]));
}

function messageUploadView(upload: MockMessageUpload): components["schemas"]["ConversationMessageUpload"] {
  return {
    client_message_id: upload.clientMessageID,
    state: upload.message ? "committed" : "uploading",
    total_bytes: upload.totalBytes,
    total_chunks: upload.totalChunks,
    received_sequences: [...upload.chunks.keys()].sort((a, b) => a - b),
    ...(upload.message ? { message: upload.message } : {}),
  };
}

async function sha256Hex(bytes: Uint8Array): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", new Uint8Array(bytes).buffer);
  return Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, "0")).join("");
}

function bytesEqual(left: Uint8Array, right: Uint8Array): boolean {
  return left.length === right.length && left.every((byte, index) => byte === right[index]);
}

function decodeMessageChunk(value: unknown): Uint8Array | null {
  if (!value || typeof value !== "object" || Array.isArray(value)) return null;
  const body = value as Record<string, unknown>;
  if (Object.keys(body).some((field) => field !== "content_base64" && field !== "sha256")
    || typeof body.content_base64 !== "string" || body.content_base64.length > 21848
    || typeof body.sha256 !== "string" || !/^[a-f0-9]{64}$/.test(body.sha256)) return null;
  try {
    const binary = atob(body.content_base64);
    const bytes = Uint8Array.from(binary, (character) => character.charCodeAt(0));
    if (!bytes.length || btoa(binary) !== body.content_base64) return null;
    return bytes;
  } catch {
    return null;
  }
}

async function putMessageUploadChunk(upload: MockMessageUpload | undefined, sequence: number, body: unknown) {
  if (!upload) return failure("not_found", "消息上传不存在", 404);
  if (!Number.isSafeInteger(sequence) || sequence < 0 || sequence >= upload.totalChunks) return failure("invalid_argument", "分片序号无效", 400);
  const bytes = decodeMessageChunk(body);
  if (!bytes) return failure("invalid_argument", "消息分片无效", 400);
  const expectedBytes = sequence === upload.totalChunks - 1
    ? upload.totalBytes - messageUploadChunkBytes * (upload.totalChunks - 1) : messageUploadChunkBytes;
  if (bytes.length !== expectedBytes || await sha256Hex(bytes) !== (body as { sha256: string }).sha256) {
    return failure("invalid_argument", "消息分片长度或摘要无效", 400);
  }
  const previous = upload.chunks.get(sequence);
  if (previous) return bytesEqual(previous, bytes) ? empty() : failure("conflict", "该分片序号已保存不同内容", 409);
  if (upload.message) return failure("conflict", "消息已提交", 409);
  upload.chunks.set(sequence, bytes);
  return empty();
}

async function assembleMessageUpload(upload: MockMessageUpload): Promise<{ content?: string; response?: Response }> {
  if (upload.chunks.size !== upload.totalChunks) return { response: failure("conflict", "消息分片尚未全部上传", 409) };
  const bytes = new Uint8Array(upload.totalBytes);
  for (let sequence = 0; sequence < upload.totalChunks; sequence += 1) {
    const chunk = upload.chunks.get(sequence);
    if (!chunk) return { response: failure("conflict", "消息分片尚未全部上传", 409) };
    bytes.set(chunk, sequence * messageUploadChunkBytes);
  }
  if (await sha256Hex(bytes) !== upload.sha256) return { response: failure("invalid_argument", "完整消息摘要不匹配", 400) };
  try {
    return { content: new TextDecoder("utf-8", { fatal: true }).decode(bytes) };
  } catch {
    return { response: failure("invalid_argument", "消息内容不是有效 UTF-8", 400) };
  }
}

function modelConfigurationStatus(user: User): "configured" | "not_configured" | "unknown" {
  const visible = modelConnections.filter((model) => canAccessModel(user, model));
  return visible.some((model) => model.has_api_key) ? "configured" : visible.length ? "unknown" : "not_configured";
}

function adminModelDefault(): AdminModelDefault {
  const selected = defaultModelID ? modelConnections.find((model) => model.id === defaultModelID) : undefined;
  return {
    configured: Boolean(selected?.shared && selected.checked_at && selected.capabilities.text),
    model_id: defaultModelID,
    version: defaultModelVersion,
  };
}

function preferencesFor(user: User): UserPreferences {
  return userPreferences.get(user.id) ?? { assistant_name: "Morde", selected_model_id: null, default_host_id: null, updated_at: null };
}

function onboardingStatusFor(user: User): components["schemas"]["OnboardingStatus"] {
  const preferences = preferencesFor(user);
  const availableModels = modelConnections.filter((model) => canAccessModel(user, model) && model.checked_at && model.capabilities.text);
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

function projectMemberFor(projectID: string, userID: string) {
  return projectMembers.find((member) => member.project_id === projectID && member.user_id === userID);
}

function readableProject(projectID: string, user: User) {
  const project = projects.find((item) => item.id === projectID);
  return project && projectMemberFor(projectID, user.id) ? project : undefined;
}

function editableProject(projectID: string, user: User) {
  const project = readableProject(projectID, user);
  const member = projectMemberFor(projectID, user.id);
  return project && (member?.role === "owner" || member?.role === "member") ? project : undefined;
}

function ownedProject(projectID: string, user: User) {
  const project = readableProject(projectID, user);
  return project && projectMemberFor(projectID, user.id)?.role === "owner" ? project : undefined;
}

function canReadProjectWorkspace(workspaceID: string, user: User) {
  if (workspaceOwners.get(workspaceID) === user.id) return true;
  return hosts.some((host) => {
    if (host.state === "deleting" || host.state === "deleted"
      || !host.workspaces.some((binding) => binding.workspace.id === workspaceID && binding.state === "active")) return false;
    return hostMembers.get(host.id)?.some((member) => member.user_id === user.id
      && member.permissions.includes("workspace:file:read")) ?? false;
  });
}

function createTaskMessage(user: User, conversation: Conversation, modelID: string, content: string, request: Record<string, unknown>) {
  const timestamp = now();
  const budget = request.budget as { active_seconds: number; tool_calls: number; model_tokens: number } | undefined;
  const task: Task = {
    id: id("q"), owner_id: user.id, initiator_id: user.id, host_id: typeof request.host_id === "string" ? request.host_id : undefined,
    workspace_id: typeof request.workspace_id === "string" ? request.workspace_id : undefined,
    input_revision: typeof request.input_revision === "number" && Number.isSafeInteger(request.input_revision) ? request.input_revision : undefined,
    execution_location: "server", conversation_id: conversation.id, model_id: modelID, objective: content,
    status: "queued", event_sequence: 0, current_revision: 0, desired_revision: 0, current_step: null, step_count: 0, completed_step_count: 0,
    budget: budget ?? { active_seconds: 1800, tool_calls: 100, model_tokens: 50000 },
    usage: { active_seconds: 0, tool_calls: 0, model_tokens: null }, created_at: timestamp, updated_at: timestamp,
    dependencies: [], child_jobs: [], result_review: null,
    result: null, waiting_reason: null, steps: [], batch_items: [], artifacts: [], pending_request: null,
    clarification_request: null, stop_request: null, assistant_message: null, finished_at: null,
  };
  const message: MockConversationMessage = {
    id: id("m"), conversation_id: conversation.id, sequence: nextConversationMessageSequence(conversation.id),
    role: "user", content, task_id: task.id, created_at: timestamp,
    delivery_status: "applied",
    ...(typeof request.client_message_id === "string" ? { client_message_id: request.client_message_id } : {}),
    ...(Array.isArray(request.attachment_ids) ? { attachment_ids: request.attachment_ids as string[] } : {}),
  };
  conversation.updated_at = timestamp;
  tasks.set(task.id, task);
  conversationMessages.push(message);
  persistAssistantState();
  return { task, message };
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
  if (path === "/api/capabilities" || path === "/api/nudges" || path === "/api/auth/login" || path === "/api/auth/register" || path === "/api/auth/logout") return;
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
    host.workspaces = host.workspaces.map((binding) => binding.state === "preparing"
      ? { ...binding, state: "active", applied_revision: binding.desired_revision, sync_state: "healthy", sync_conflicts: [] }
      : binding);
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

function recordMockAudit(actor: User, action: string, target: string, detail: Record<string, unknown>) {
  auditLogs.unshift({ id: Date.now(), at: now(), actor_id: actor.id, actor_kind: "user", action, target, detail, ip: "127.0.0.1" });
}

function recordConsentState(ownerID: string): RecordConsent[] {
  const stored = recordConsents.get(ownerID);
  if (stored) return stored;
  const initial: RecordConsent[] = [
    { owner_id: ownerID, purpose: "full_record", enabled: false, version: 0 },
    { owner_id: ownerID, purpose: "training_contribution", enabled: false, version: 0 },
  ];
  recordConsents.set(ownerID, initial);
  return initial;
}

function recordQuotaState(ownerID: string): RecordQuota {
  return recordQuotas.get(ownerID) ?? {
    owner_id: ownerID, retention_days: 30, limit_bytes: 1_073_741_824, used_bytes: 0, reserved_bytes: 0, version: 1, updated_at: now(),
  };
}

function hasControlCharacters(value: string) {
  return Array.from(value).some((character) => character.charCodeAt(0) < 32 || character.charCodeAt(0) === 127);
}

function adminPage(request: Request, defaultLimit = 25) {
  const query = new URL(request.url).searchParams;
  const limits = query.getAll("limit");
  const offsets = query.getAll("offset");
  const limit = limits.length ? Number(limits[0]) : defaultLimit;
  const offset = offsets.length ? Number(offsets[0]) : 0;
  return limits.length > 1 || offsets.length > 1 || !Number.isInteger(limit) || limit < 1 || limit > 100 ||
    !Number.isInteger(offset) || offset < 0 || offset > 1_000_000 ? null : { limit, offset };
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
    const body = await request.json().catch(() => ({})) as { name?: string; password?: string; invite_code?: unknown };
    const name = body.name?.trim();
    if (!name || name.length < 3 || name.length > 64 || !body.password || body.password.length < 8 || body.password.length > 72) return failure("invalid_argument", "用户名或密码无效", 400);
    if (body.invite_code !== undefined && typeof body.invite_code !== "string") return failure("invalid_argument", "邀请码无效", 400);
    const inviteCode = typeof body.invite_code === "string" ? body.invite_code.trim().toLowerCase() : "";
    const inviterID = inviteCode ? inviteCodes.get(inviteCode) : undefined;
    if (inviteCode && (!/^[a-f0-9]{32}$/.test(inviteCode) || !inviterID || users.find((user) => user.id === inviterID)?.status !== "approved")) return failure("invalid_argument", "邀请码无效", 400);
    if (name.toLowerCase() === "admin" || loginAccounts.has(name) || users.some((user) => user.name === name)) return failure("conflict", "用户名已被使用", 409);
    const user: User = { id: id("u"), name, role: "user", status: "pending", must_change_password: false,
      quota: { ...demoUser.quota }, usage: { hosts: 0, cpu_milli: 0, mem_mib: 0 }, default_host_resources: { ...defaultHostResources } };
    users.push(user);
    if (inviterID) inviteRelations.push({ inviter_id: inviterID, invitee_id: user.id, created_at: now() });
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
  http.get("/api/projects", ({ cookies }) => {
    const user = sessionUser(cookies)!;
    return json({ items: projects.filter((project) => projectMemberFor(project.id, user.id)), next_cursor: null });
  }),
  http.post("/api/projects", async ({ request, cookies }) => {
    const user = sessionUser(cookies)!;
    const body = await request.json().catch(() => null) as components["schemas"]["CreateProjectRequest"] | null;
    if (!body || typeof body.name !== "string" || !body.name.trim() || body.name.length > 120 || (body.description?.length ?? 0) > 2000) {
      return failure("invalid_argument", "项目名称或说明无效", 400);
    }
    const project: Project = { id: id("p"), owner_id: user.id, name: body.name.trim(), description: body.description?.trim() ?? "", version: 1 };
    projects.push(project);
    projectMembers.push({ project_id: project.id, user_id: user.id, role: "owner", version: 1 });
    projectBudgets.set(project.id, { configured: false, currency: "USD", limit_micros: 0, version: 1, spent_micros: 0, reserved_micros: 0, has_unknown: false });
    projectLedgers.set(project.id, []);
    return json(project, 201);
  }),
  http.get("/api/projects/:projectID", ({ params, cookies }) => {
    const project = readableProject(String(params.projectID), sessionUser(cookies)!);
    return project ? json(project) : failure("not_found", "项目不存在或无权访问", 404);
  }),
  http.get("/api/projects/:projectID/conversations", ({ params, cookies }) => {
    const user = sessionUser(cookies)!;
    const projectID = String(params.projectID);
    if (!readableProject(projectID, user)) return failure("not_found", "项目不存在或无权访问", 404);
    return json({ items: projectConversations.filter((item) => item.project_id === projectID), next_cursor: null });
  }),
  http.post("/api/projects/:projectID/conversations", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const projectID = String(params.projectID);
    if (!readableProject(projectID, user)) return failure("not_found", "项目不存在或无权访问", 404);
    const body = await request.json().catch(() => null) as components["schemas"]["CreateProjectConversationRequest"] | null;
    if (!body || (body.title !== undefined && (typeof body.title !== "string" || body.title.length > 120))) return failure("invalid_argument", "共享对话标题无效", 400);
    const key = request.headers.get("Idempotency-Key")?.trim() ?? "";
    if (!key) return failure("invalid_argument", "缺少 Idempotency-Key", 400);
    const normalizedBody = { ...(body.title?.trim() ? { title: body.title.trim() } : {}) };
    const fingerprint = JSON.stringify(normalizedBody);
    const replayKey = `${user.id}:${projectID}:${key}`;
    const replay = projectConversationCreates.get(replayKey);
    if (replay) return replay.request === fingerprint
      ? json(replay.response, 200)
      : failure("conflict", "幂等键已用于不同的共享对话请求", 409);
    const timestamp = now();
    const value: ProjectConversation = {
      id: id("c"), project_id: projectID, owner_id: user.id,
      title: normalizedBody.title ?? "项目共享对话", created_at: timestamp, updated_at: timestamp,
    };
    projectConversations.push(value);
    projectConversationCreates.set(replayKey, { request: fingerprint, response: value });
    return json(value, 201);
  }),
  http.put("/api/projects/:projectID/conversations/:conversationID/message-uploads/:clientMessageID", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const projectID = String(params.projectID);
    const conversationID = String(params.conversationID);
    const clientMessageID = String(params.clientMessageID);
    if (!readableProject(projectID, user)) return failure("not_found", "项目不存在或无权访问", 404);
    if (!projectConversations.some((item) => item.project_id === projectID && item.id === conversationID)) return failure("not_found", "项目共享对话不存在", 404);
    const identityError = messageUploadIdentityFailure(request, user, clientMessageID, true);
    if (identityError) return identityError;
    const body: unknown = await request.json().catch(() => null);
    if (!validMessageUploadInit(body)) return failure("invalid_argument", "长消息上传初始化参数无效", 400);
    const metadata = messageUploadMetadata(body);
    const fingerprint = canonicalJSON({ metadata, total_bytes: body.total_bytes, total_chunks: body.total_chunks, sha256: body.sha256 });
    const key = messageUploadKey(user.id, projectID, conversationID, clientMessageID);
    const existing = conversationMessageUploads.get(key);
    if (existing) return existing.fingerprint === fingerprint
      ? json(messageUploadView(existing)) : failure("conflict", "消息编号已用于不同的上传内容", 409);
    const upload: MockMessageUpload = {
      ownerID: user.id, projectID, conversationID, clientMessageID, metadata,
      totalBytes: body.total_bytes, totalChunks: body.total_chunks, sha256: body.sha256,
      fingerprint, chunks: new Map(),
    };
    conversationMessageUploads.set(key, upload);
    return json(messageUploadView(upload));
  }),
  http.get("/api/projects/:projectID/conversations/:conversationID/message-uploads/:clientMessageID", ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const projectID = String(params.projectID);
    const conversationID = String(params.conversationID);
    const clientMessageID = String(params.clientMessageID);
    if (!readableProject(projectID, user)) return failure("not_found", "项目不存在或无权访问", 404);
    if (!projectConversations.some((item) => item.project_id === projectID && item.id === conversationID)) return failure("not_found", "项目共享对话不存在", 404);
    const identityError = messageUploadIdentityFailure(request, user, clientMessageID, false);
    if (identityError) return identityError;
    const upload = conversationMessageUploads.get(messageUploadKey(user.id, projectID, conversationID, clientMessageID));
    return upload ? json(messageUploadView(upload)) : failure("not_found", "消息上传不存在", 404);
  }),
  http.put("/api/projects/:projectID/conversations/:conversationID/message-uploads/:clientMessageID/chunks/:sequence", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const projectID = String(params.projectID);
    const conversationID = String(params.conversationID);
    const clientMessageID = String(params.clientMessageID);
    if (!readableProject(projectID, user)) return failure("not_found", "项目不存在或无权访问", 404);
    if (!projectConversations.some((item) => item.project_id === projectID && item.id === conversationID)) return failure("not_found", "项目共享对话不存在", 404);
    const identityError = messageUploadIdentityFailure(request, user, clientMessageID, false);
    if (identityError) return identityError;
    const upload = conversationMessageUploads.get(messageUploadKey(user.id, projectID, conversationID, clientMessageID));
    const body: unknown = await request.json().catch(() => null);
    return putMessageUploadChunk(upload, Number(params.sequence), body);
  }),
  http.post("/api/projects/:projectID/conversations/:conversationID/message-uploads/:clientMessageID/commit", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const projectID = String(params.projectID);
    const conversationID = String(params.conversationID);
    const clientMessageID = String(params.clientMessageID);
    if (!readableProject(projectID, user)) return failure("not_found", "项目不存在或无权访问", 404);
    const conversation = projectConversations.find((item) => item.project_id === projectID && item.id === conversationID);
    if (!conversation) return failure("not_found", "项目共享对话不存在", 404);
    const identityError = messageUploadIdentityFailure(request, user, clientMessageID, true);
    if (identityError) return identityError;
    const upload = conversationMessageUploads.get(messageUploadKey(user.id, projectID, conversationID, clientMessageID));
    if (!upload) return failure("not_found", "消息上传不存在", 404);
    if (upload.message) return json({ message: upload.message }, 202);
    const assembled = await assembleMessageUpload(upload);
    if (assembled.response) return assembled.response;
    const body = { client_message_id: clientMessageID, ...upload.metadata, content: assembled.content! };
    if (!isAutoMessageBody(body)) return failure("invalid_argument", "项目消息无效", 400);
    const replayKey = `${user.id}:${projectID}:${conversationID}:${clientMessageID}`;
    const replay = projectConversationAcceptances.get(replayKey);
    let message: ProjectConversationMessage;
    if (replay) {
      if (replay.request !== canonicalJSON(body)) return failure("conflict", "幂等键已用于不同的项目消息", 409);
      message = replay.response.message;
    } else {
      const timestamp = now();
      message = {
        id: id("m"), conversation_id: conversationID, owner_id: conversation.owner_id, actor_id: user.id,
        sequence: nextConversationMessageSequence(conversationID), role: "user", content: body.content,
        client_message_id: clientMessageID, routing_status: "pending", delivery_status: "pending", created_at: timestamp,
      };
      conversation.updated_at = timestamp;
      projectConversationMessages.push(message);
      projectConversationAcceptances.set(replayKey, { request: canonicalJSON(body), response: { message, replayed: false } });
    }
    upload.message = message;
    return json({ message }, 202);
  }),
  http.get("/api/projects/:projectID/conversations/:conversationID/messages", ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const projectID = String(params.projectID);
    const conversationID = String(params.conversationID);
    if (!readableProject(projectID, user)) return failure("not_found", "项目不存在或无权访问", 404);
    if (!projectConversations.some((item) => item.project_id === projectID && item.id === conversationID)) return failure("not_found", "项目共享对话不存在", 404);
    const clientMessageID = new URL(request.url).searchParams.get("client_message_id");
    if (clientMessageID && request.headers.get("X-Morde-Service-Instance-ID") !== mockServiceInstanceID) return failure("conflict", "服务身份不匹配", 409);
    if (clientMessageID && request.headers.get("X-Morde-Actor-ID") !== user.id) return failure("conflict", "账号身份不匹配", 409);
    return json({
      items: projectConversationMessages.filter((item) => item.conversation_id === conversationID
        && (!clientMessageID || (item.client_message_id === clientMessageID && item.actor_id === user.id))),
      next_cursor: null,
    });
  }),
  http.post("/api/projects/:projectID/conversations/:conversationID/messages/:messageID/variants", ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const projectID = String(params.projectID);
    const conversationID = String(params.conversationID);
    const projectConversation = projectConversations.find((item) => item.project_id === projectID && item.id === conversationID);
    if (!readableProject(projectID, user) || !projectConversation) return failure("not_found", "项目共享对话不存在", 404);
    return createMockMessageVariant({ user, request, projectID, conversationID, targetID: String(params.messageID),
      messages: projectConversationMessages as unknown as Array<MockConversationMessage | ProjectConversationMessage>, projectConversation });
  }),
  http.get("/api/projects/:projectID/conversations/:conversationID/tasks", ({ params, cookies }) => {
    const user = sessionUser(cookies)!;
    const projectID = String(params.projectID);
    const conversationID = String(params.conversationID);
    if (!readableProject(projectID, user) || !projectConversations.some((item) => item.project_id === projectID && item.id === conversationID)) {
      return failure("not_found", "项目共享对话不存在", 404);
    }
    return json({ items: Array.from(tasks.values()).filter((task) => task.conversation_id === conversationID
      && !["completed", "failed", "cancelled"].includes(task.status)), next_cursor: null });
  }),
  http.post("/api/projects/:projectID/conversations/:conversationID/messages", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const projectID = String(params.projectID);
    const conversationID = String(params.conversationID);
    if (!readableProject(projectID, user)) return failure("not_found", "项目不存在或无权访问", 404);
    const conversation = projectConversations.find((item) => item.project_id === projectID && item.id === conversationID);
    if (!conversation) return failure("not_found", "项目共享对话不存在", 404);
    const body: unknown = await request.json().catch(() => null);
    if (!isAutoMessageBody(body)) return failure("invalid_argument", "项目消息无效", 400);
    const key = request.headers.get("Idempotency-Key")?.trim() ?? "";
    if (request.headers.get("X-Morde-Service-Instance-ID") !== mockServiceInstanceID) return failure("conflict", "服务身份不匹配", 409);
    if (request.headers.get("X-Morde-Actor-ID") !== user.id) return failure("conflict", "账号身份不匹配", 409);
    if (key !== body.client_message_id) return failure("invalid_argument", "消息编号与请求编号不一致", 400);
    const fingerprint = canonicalJSON(body);
    const replayKey = `${user.id}:${projectID}:${conversationID}:${key}`;
    const replay = projectConversationAcceptances.get(replayKey);
    if (replay) return replay.request === fingerprint
      ? json({ ...replay.response, replayed: true }, 202)
      : failure("conflict", "幂等键已用于不同的项目消息", 409);
    const timestamp = now();
    const message: ProjectConversationMessage = {
      id: id("m"), conversation_id: conversationID, owner_id: conversation.owner_id, actor_id: user.id,
      sequence: nextConversationMessageSequence(conversationID), role: "user", content: body.content,
      client_message_id: key, routing_status: "pending", delivery_status: "pending", created_at: timestamp,
    };
    const response: AcceptedProjectConversationMessageResponse = { message, replayed: false };
    conversation.updated_at = timestamp;
    projectConversationMessages.push(message);
    projectConversationAcceptances.set(replayKey, { request: fingerprint, response });
    return json(response, 202);
  }),
  http.get("/api/projects/:projectID/members", ({ params, cookies }) => {
    const projectID = String(params.projectID);
    if (!readableProject(projectID, sessionUser(cookies)!)) return failure("not_found", "项目不存在或无权访问", 404);
    return json({ items: projectMembers.filter((member) => member.project_id === projectID), next_cursor: null });
  }),
  http.post("/api/projects/:projectID/members", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const projectID = String(params.projectID);
    const project = ownedProject(projectID, user);
    if (!project) return readableProject(projectID, user) ? failure("forbidden", "只有项目负责人可以邀请成员", 403) : failure("not_found", "项目不存在或无权访问", 404);
    const body = await request.json().catch(() => null) as components["schemas"]["InviteProjectMemberRequest"] | null;
    if (!body || !["member", "viewer"].includes(body.role) || typeof body.user_id !== "string" || !Number.isSafeInteger(body.expected_version)) return failure("invalid_argument", "邀请信息无效", 400);
    if (body.expected_version !== project.version) return failure("conflict", "项目版本已变化，请刷新后重试", 409);
    const invitee = users.find((item) => item.id === body.user_id);
    if (!invitee) return failure("not_found", "目标账号不存在", 404);
    if (projectMemberFor(projectID, invitee.id) || projectInvitations.some((item) => item.project_id === projectID && item.invitee_id === invitee.id && item.state === "pending")) return failure("conflict", "目标账号已有项目成员关系或待处理邀请", 409);
    const invitation: ProjectInvitation = { id: id("r"), project_id: projectID, inviter_id: user.id, invitee_id: invitee.id, role: body.role, state: "pending", version: 1 };
    projectInvitations.push(invitation);
    project.version += 1;
    return json(invitation, 201);
  }),
  http.patch("/api/projects/:projectID/members/:userID", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const projectID = String(params.projectID);
    if (!ownedProject(projectID, user)) return readableProject(projectID, user) ? failure("forbidden", "只有项目负责人可以修改成员角色", 403) : failure("not_found", "项目不存在或无权访问", 404);
    const member = projectMemberFor(projectID, String(params.userID));
    if (!member || member.role === "owner") return failure("not_found", "项目成员不存在", 404);
    const body = await request.json().catch(() => null) as components["schemas"]["UpdateProjectMemberRequest"] | null;
    if (!body || !["member", "viewer"].includes(body.role) || !Number.isSafeInteger(body.expected_version)) return failure("invalid_argument", "成员角色更新信息无效", 400);
    if (body.expected_version !== member.version) return failure("conflict", "成员版本已变化，请刷新后重试", 409);
    member.role = body.role;
    member.version += 1;
    return json(member);
  }),
  http.delete("/api/projects/:projectID/members/:userID", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const projectID = String(params.projectID);
    if (!ownedProject(projectID, user)) return readableProject(projectID, user) ? failure("forbidden", "只有项目负责人可以移除成员", 403) : failure("not_found", "项目不存在或无权访问", 404);
    const member = projectMemberFor(projectID, String(params.userID));
    if (!member || member.role === "owner") return failure("not_found", "项目成员不存在", 404);
    const body = await request.json().catch(() => null) as components["schemas"]["RemoveProjectMemberRequest"] | null;
    if (!body || !Number.isSafeInteger(body.expected_version)) return failure("invalid_argument", "成员版本无效", 400);
    if (body.expected_version !== member.version) return failure("conflict", "成员版本已变化，请刷新后重试", 409);
    projectMembers = projectMembers.filter((item) => item !== member);
    return empty();
  }),
  http.delete("/api/projects/:projectID/invitations/:invitationID", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const projectID = String(params.projectID);
    if (!ownedProject(projectID, user)) return readableProject(projectID, user) ? failure("forbidden", "只有项目负责人可以撤回邀请", 403) : failure("not_found", "项目不存在或无权访问", 404);
    const invitation = projectInvitations.find((item) => item.project_id === projectID && item.id === params.invitationID);
    if (!invitation || invitation.state !== "pending") return failure("not_found", "待处理邀请不存在", 404);
    const body = await request.json().catch(() => null) as components["schemas"]["RevokeProjectInvitationRequest"] | null;
    if (!body || !Number.isSafeInteger(body.expected_version)) return failure("invalid_argument", "邀请版本无效", 400);
    if (body.expected_version !== invitation.version) return failure("conflict", "邀请版本已变化，请刷新后重试", 409);
    invitation.state = "revoked";
    invitation.version += 1;
    return empty();
  }),
  http.get("/api/me/project-invitations", ({ cookies }) => {
    const user = sessionUser(cookies)!;
    return json({ items: projectInvitations.filter((item) => item.invitee_id === user.id), next_cursor: null });
  }),
  http.post("/api/project-invitations/:invitationID/accept", ({ params, cookies }) => {
    const user = sessionUser(cookies)!;
    const invitation = projectInvitations.find((item) => item.id === params.invitationID && item.invitee_id === user.id);
    if (!invitation) return failure("not_found", "项目邀请不存在", 404);
    if (invitation.state === "accepted") {
      const member = projectMemberFor(invitation.project_id, user.id);
      return member ? json(member) : failure("conflict", "邀请状态与成员关系不一致", 409);
    }
    if (invitation.state !== "pending") return failure("conflict", "邀请已撤回", 409);
    const member: ProjectMember = { project_id: invitation.project_id, user_id: user.id, role: invitation.role, version: 1 };
    projectMembers.push(member);
    invitation.state = "accepted";
    invitation.version += 1;
    return json(member, 201);
  }),
  http.post("/api/projects/:projectID/owner-transfer", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const projectID = String(params.projectID);
    const project = ownedProject(projectID, user);
    if (!project) return readableProject(projectID, user) ? failure("forbidden", "只有当前负责人可以移交项目", 403) : failure("not_found", "项目不存在或无权访问", 404);
    const body = await request.json().catch(() => null) as components["schemas"]["TransferProjectOwnerRequest"] | null;
    if (!body || typeof body.user_id !== "string" || !Number.isSafeInteger(body.expected_version)) return failure("invalid_argument", "负责人移交信息无效", 400);
    if (body.expected_version !== project.version) return failure("conflict", "项目版本已变化，请刷新后重试", 409);
    const newOwner = projectMemberFor(projectID, body.user_id);
    const oldOwner = projectMemberFor(projectID, user.id);
    if (!newOwner || !oldOwner || newOwner.role === "owner") return failure("conflict", "目标必须是已接受的项目成员", 409);
    oldOwner.role = "member";
    oldOwner.version += 1;
    newOwner.role = "owner";
    newOwner.version += 1;
    project.owner_id = newOwner.user_id;
    project.version += 1;
    return json(project);
  }),
  http.get("/api/projects/:projectID/resources", ({ params, cookies }) => {
    const user = sessionUser(cookies)!;
    const projectID = String(params.projectID);
    if (!readableProject(projectID, user)) return failure("not_found", "项目不存在或无权访问", 404);
    const items = projectResources.filter((resource) => resource.project_id === projectID && (resource.kind === "host"
      ? hosts.some((host) => host.id === resource.resource_id && (isAdmin(user) || host.owner_id === user.id))
      : canReadProjectWorkspace(resource.resource_id, user)));
    return json({ items, next_cursor: null });
  }),
  http.get("/api/projects/:projectID/workspaces/:workspaceID/manifest", ({ params, request, cookies }) => {
    const user = sessionUser(cookies);
    if (!user) return failure("unauthenticated", "请先登录", 401);
    const projectID = String(params.projectID);
    const workspaceID = String(params.workspaceID);
    if (!readableProject(projectID, user)) return failure("not_found", "项目不存在或无权访问", 404);
    if (!projectResources.some((resource) => resource.project_id === projectID
      && resource.kind === "workspace" && resource.resource_id === workspaceID)) {
      return failure("not_found", "项目工作区不存在", 404);
    }
    const workspace = workspaces.find((item) => item.id === workspaceID && item.state === "ready");
    if (!workspace || !canReadProjectWorkspace(workspaceID, user)) return failure("not_found", "项目工作区不存在", 404);

    const query = new URL(request.url).searchParams;
    const revisionText = query.get("revision");
    const revision = revisionText === null ? workspace.revision : Number(revisionText);
    if (!Number.isSafeInteger(revision) || revision < 0) return failure("invalid_argument", "revision 无效", 400);
    if (revision > workspace.revision || (revision !== 0 && revision !== workspace.revision)) {
      return failure("not_found", "工作区版本不存在", 404);
    }
    const limit = query.has("limit") ? Number(query.get("limit")) : 500;
    if (!Number.isSafeInteger(limit) || limit < 1 || limit > 1000) return failure("invalid_argument", "limit 无效", 400);
    const rawPrefix = query.get("prefix") ?? "";
    const prefix = rawPrefix.endsWith("/") ? rawPrefix.slice(0, -1) : rawPrefix;
    if (prefix.startsWith("/") || prefix.split("/").some((segment) => segment === "" || segment === "." || segment === "..")) {
      if (prefix !== "") return failure("invalid_argument", "prefix 无效", 400);
    }
    const source = projectWorkspaceManifests.get(workspaceID) ?? {
      project_id: projectID, workspace_id: workspaceID, revision: workspace.revision, files: [], empty_directories: [], next_cursor: null,
    };
    if (source.project_id !== projectID || source.revision !== workspace.revision) {
      return failure("conflict", "演示工作区目录快照已变化", 409);
    }
    const filteredFiles = source.files.filter((file) => !prefix || file.path === prefix || file.path.startsWith(`${prefix}/`));
    const cursor = query.get("cursor") ?? "";
    const offset = cursor === "" ? 0 : Number(cursor);
    if (!Number.isSafeInteger(offset) || offset < 0 || offset >= filteredFiles.length) {
      if (cursor !== "" || filteredFiles.length > 0) return failure("invalid_argument", "cursor 无效", 400);
    }
    const files = filteredFiles.slice(offset, offset + limit);
    const nextOffset = offset + files.length;
    const emptyDirectories = source.empty_directories.filter((path) => !prefix || path === prefix || path.startsWith(`${prefix}/`));
    return json({
      project_id: projectID, workspace_id: workspaceID, revision: workspace.revision, files,
      empty_directories: emptyDirectories, next_cursor: nextOffset < filteredFiles.length ? String(nextOffset) : null,
    });
  }),
  http.post("/api/projects/:projectID/resources", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const projectID = String(params.projectID);
    const project = ownedProject(projectID, user);
    if (!project) return readableProject(projectID, user) ? failure("forbidden", "只有项目负责人可以关联资源", 403) : failure("not_found", "项目不存在或无权访问", 404);
    const body = await request.json().catch(() => null) as components["schemas"]["ProjectResourceMutationRequest"] | null;
    if (!body || !["host", "workspace"].includes(body.kind) || typeof body.resource_id !== "string" || !Number.isSafeInteger(body.expected_version)) return failure("invalid_argument", "资源关联信息无效", 400);
    if (body.expected_version !== project.version) return failure("conflict", "项目版本已变化，请刷新后重试", 409);
    const accessible = body.kind === "host" ? Boolean(findHost(body.resource_id, user)) : workspaceOwners.get(body.resource_id) === user.id;
    if (!accessible) return failure("not_found", "资源不存在或当前账号无权访问", 404);
    if (projectResources.some((item) => item.project_id === projectID && item.kind === body.kind && item.resource_id === body.resource_id)) return failure("conflict", "资源已关联", 409);
    const resource: ProjectResource = { project_id: projectID, kind: body.kind, resource_id: body.resource_id, version: project.version + 1 };
    projectResources.push(resource);
    project.version += 1;
    return json(resource, 201);
  }),
  http.delete("/api/projects/:projectID/resources", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const projectID = String(params.projectID);
    const project = ownedProject(projectID, user);
    if (!project) return readableProject(projectID, user) ? failure("forbidden", "只有项目负责人可以移除资源关联", 403) : failure("not_found", "项目不存在或无权访问", 404);
    const body = await request.json().catch(() => null) as components["schemas"]["ProjectResourceMutationRequest"] | null;
    if (!body || !Number.isSafeInteger(body.expected_version)) return failure("invalid_argument", "资源关联信息无效", 400);
    if (body.expected_version !== project.version) return failure("conflict", "项目版本已变化，请刷新后重试", 409);
    const resource = projectResources.find((item) => item.project_id === projectID && item.kind === body.kind && item.resource_id === body.resource_id);
    if (!resource) return failure("not_found", "资源关联不存在", 404);
    projectResources = projectResources.filter((item) => item !== resource);
    project.version += 1;
    return empty();
  }),
  http.get("/api/projects/:projectID/budget", ({ params, cookies }) => {
    const projectID = String(params.projectID);
    if (!readableProject(projectID, sessionUser(cookies)!)) return failure("not_found", "项目不存在或无权访问", 404);
    return json(projectBudgets.get(projectID) ?? { configured: false, currency: "USD", limit_micros: 0, version: 1, spent_micros: 0, reserved_micros: 0, has_unknown: false });
  }),
  http.put("/api/projects/:projectID/budget", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const projectID = String(params.projectID);
    if (!ownedProject(projectID, user)) return readableProject(projectID, user) ? failure("forbidden", "只有项目负责人可以设置预算", 403) : failure("not_found", "项目不存在或无权访问", 404);
    const body = await request.json().catch(() => null) as components["schemas"]["SetProjectBudgetRequest"] | null;
    if (!body || !/^[A-Z]{3}$/.test(body.currency) || !Number.isSafeInteger(body.limit_micros) || body.limit_micros < 1 || !Number.isSafeInteger(body.expected_version)) return failure("invalid_argument", "预算信息无效", 400);
    const current = projectBudgets.get(projectID) ?? { configured: false, currency: "USD", limit_micros: 0, version: 1, spent_micros: 0, reserved_micros: 0, has_unknown: false };
    if (body.expected_version !== current.version) return failure("conflict", "预算版本已变化，请刷新后重试", 409);
    const updated: ProjectBudget = { ...current, configured: true, currency: body.currency, limit_micros: body.limit_micros, version: current.version + 1 };
    projectBudgets.set(projectID, updated);
    return json(updated);
  }),
  http.get("/api/projects/:projectID/budget/ledger", ({ params, cookies }) => {
    const projectID = String(params.projectID);
    if (!readableProject(projectID, sessionUser(cookies)!)) return failure("not_found", "项目不存在或无权访问", 404);
    return json({ items: projectLedgers.get(projectID) ?? [], next_cursor: null });
  }),
  http.get("/api/projects/:projectID/work-items", ({ params, cookies, request }) => {
    const projectID = String(params.projectID);
    if (!readableProject(projectID, sessionUser(cookies)!)) return failure("not_found", "项目不存在或无权访问", 404);
    const includeUnassigned = new URL(request.url).searchParams.get("include_unassigned") === "true";
    return json({ items: projectWorkItems.filter((item) => item.project_id === projectID && (includeUnassigned || item.assignee_id !== null)), next_cursor: null });
  }),
  http.post("/api/projects/:projectID/work-items", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const projectID = String(params.projectID);
    if (!editableProject(projectID, user)) return readableProject(projectID, user) ? failure("forbidden", "当前项目角色不能修改工作项", 403) : failure("not_found", "项目不存在或无权访问", 404);
    const body = await request.json().catch(() => null) as CreateProjectWorkItemRequest | null;
    if (!body || typeof body.title !== "string" || !body.title.trim() || body.title.length > 200 || (body.description?.length ?? 0) > 4096 || (body.acceptance_criteria && body.acceptance_criteria.some((criterion) => !criterion.trim() || criterion.length > 1000))) return failure("invalid_argument", "工作项信息无效", 400);
    if (body.category !== undefined && !["bug", "request", "unclassified"].includes(body.category)) return failure("invalid_argument", "工作项类型无效", 400);
    if (body.unassigned !== undefined && typeof body.unassigned !== "boolean") return failure("invalid_argument", "工作项认领状态无效", 400);
    const assigneeID = body.unassigned ? null : body.assignee_id ?? user.id;
    if (assigneeID && !projectMemberFor(projectID, assigneeID)) return failure("invalid_argument", "负责人必须是已接受的项目成员", 400);
    const item: ProjectWorkItem = { id: id("j"), project_id: projectID, title: body.title.trim(), description: body.description ?? "", acceptance_criteria: body.acceptance_criteria ?? [], assignee_id: assigneeID, category: body.category ?? "unclassified", state: "open", blocked_reason: "", task_ids: [], version: 1 };
    projectWorkItems.push(item);
    return json(item, 201);
  }),
  http.get("/api/projects/:projectID/work-items/:workItemID", ({ params, cookies }) => {
    const projectID = String(params.projectID);
    if (!readableProject(projectID, sessionUser(cookies)!)) return failure("not_found", "项目不存在或无权访问", 404);
    const item = projectWorkItems.find((workItem) => workItem.project_id === projectID && workItem.id === params.workItemID);
    return item ? json(item) : failure("not_found", "工作项不存在", 404);
  }),
  http.patch("/api/projects/:projectID/work-items/:workItemID", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const projectID = String(params.projectID);
    if (!editableProject(projectID, user)) return readableProject(projectID, user) ? failure("forbidden", "当前项目角色不能修改工作项", 403) : failure("not_found", "项目不存在或无权访问", 404);
    const item = projectWorkItems.find((workItem) => workItem.project_id === projectID && workItem.id === params.workItemID);
    if (!item) return failure("not_found", "工作项不存在", 404);
    const body = await request.json().catch(() => null) as components["schemas"]["AssignProjectWorkItemRequest"] | null;
    if (!body || typeof body.assignee_id !== "string" || !Number.isSafeInteger(body.expected_version)) return failure("invalid_argument", "工作项负责人信息无效", 400);
    if (body.expected_version !== item.version) return failure("conflict", "工作项版本已变化，请刷新后重试", 409);
    if (!projectMemberFor(projectID, body.assignee_id)) return failure("invalid_argument", "负责人必须是已接受的项目成员", 400);
    item.assignee_id = body.assignee_id;
    item.version += 1;
    return json(item);
  }),
  http.post("/api/projects/:projectID/work-items/:workItemID/claim", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const projectID = String(params.projectID);
    if (!editableProject(projectID, user)) return readableProject(projectID, user) ? failure("forbidden", "当前项目角色不能认领工作项", 403) : failure("not_found", "项目不存在或无权访问", 404);
    const item = projectWorkItems.find((workItem) => workItem.project_id === projectID && workItem.id === params.workItemID);
    if (!item) return failure("not_found", "工作项不存在", 404);
    const idempotencyKey = request.headers.get("Idempotency-Key");
    const body = await request.json().catch(() => null) as { expected_version?: unknown } | null;
    if (!idempotencyKey || !body || !Number.isSafeInteger(body.expected_version) || Number(body.expected_version) < 1) return failure("invalid_argument", "认领请求或工作项版本无效", 400);
    const replayKey = `${user.id}:${projectID}:${item.id}:${idempotencyKey}`;
    const requestBody = JSON.stringify(body);
    const replay = projectWorkItemClaimReplays.get(replayKey);
    if (replay) return replay.request === requestBody ? json(replay.response) : failure("conflict", "幂等键已用于不同的认领请求", 409);
    if (body.expected_version !== item.version || item.assignee_id !== null) return failure("conflict", "工作项已被其他成员认领或版本已变化", 409);
    item.assignee_id = user.id;
    item.version += 1;
    const response = { ...item };
    projectWorkItemClaimReplays.set(replayKey, { request: requestBody, response });
    return json(response);
  }),
  http.post("/api/projects/:projectID/work-items/:workItemID/block", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const projectID = String(params.projectID);
    if (!editableProject(projectID, user)) return readableProject(projectID, user) ? failure("forbidden", "当前项目角色不能修改工作项", 403) : failure("not_found", "项目不存在或无权访问", 404);
    const item = projectWorkItems.find((workItem) => workItem.project_id === projectID && workItem.id === params.workItemID);
    if (!item) return failure("not_found", "工作项不存在", 404);
    const body = await request.json().catch(() => null) as components["schemas"]["BlockProjectWorkItemRequest"] | null;
    if (!body || typeof body.blocked_reason !== "string" || !body.blocked_reason.trim() || body.blocked_reason.length > 2000 || !Number.isSafeInteger(body.expected_version)) return failure("invalid_argument", "阻塞原因或工作项版本无效", 400);
    if (body.expected_version !== item.version || item.state !== "open") return failure("conflict", "工作项状态或版本已变化", 409);
    item.state = "blocked";
    item.blocked_reason = body.blocked_reason.trim();
    item.version += 1;
    return json(item);
  }),
  http.post("/api/projects/:projectID/work-items/:workItemID/resume", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const projectID = String(params.projectID);
    if (!editableProject(projectID, user)) return readableProject(projectID, user) ? failure("forbidden", "当前项目角色不能修改工作项", 403) : failure("not_found", "项目不存在或无权访问", 404);
    const item = projectWorkItems.find((workItem) => workItem.project_id === projectID && workItem.id === params.workItemID);
    if (!item) return failure("not_found", "工作项不存在", 404);
    const body = await request.json().catch(() => null) as components["schemas"]["ProjectWorkItemVersionRequest"] | null;
    if (!body || !Number.isSafeInteger(body.expected_version)) return failure("invalid_argument", "工作项版本无效", 400);
    if (body.expected_version !== item.version || item.state !== "blocked") return failure("conflict", "工作项状态或版本已变化", 409);
    item.state = "open";
    item.blocked_reason = "";
    item.version += 1;
    return json(item);
  }),
  http.post("/api/projects/:projectID/work-items/:workItemID/tasks", ({ params, cookies }) => {
    const user = sessionUser(cookies)!;
    const projectID = String(params.projectID);
    if (!editableProject(projectID, user)) return readableProject(projectID, user) ? failure("forbidden", "当前项目角色不能关联 Task", 403) : failure("not_found", "项目不存在或无权访问", 404);
    return failure("unsupported", "本地替身未实现项目 Task 授权关系；不建立未经验证的关联", 501);
  }),
  http.post("/api/projects/:projectID/work-items/:workItemID/delivery", ({ params, cookies }) => {
    const user = sessionUser(cookies)!;
    const projectID = String(params.projectID);
    if (!editableProject(projectID, user)) return readableProject(projectID, user) ? failure("forbidden", "当前项目角色不能提交交付", 403) : failure("not_found", "项目不存在或无权访问", 404);
    return failure("unsupported", "本地替身无法验证 Workspace 修订是否已保存，不会确认交付", 501);
  }),
  http.get("/api/me/invite-code", ({ cookies }) => {
    const user = sessionUser(cookies)!;
    let code = Array.from(inviteCodes).find(([, ownerID]) => ownerID === user.id)?.[0];
    if (!code) {
      do { code = Array.from({ length: 32 }, () => Math.floor(Math.random() * 16).toString(16)).join(""); }
      while (inviteCodes.has(code));
      inviteCodes.set(code, user.id);
    }
    return json({ code });
  }),
  http.get("/api/nudges", () => json(adminNudgePolicy.enabled ? adminNudgePolicy : { ...adminNudgePolicy, url: "", text_zh: "", text_en: "" })),
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
    if (body.selected_model_id && !modelConnections.some((model) => canAccessModel(user, model) && model.id === body.selected_model_id && model.checked_at && model.capabilities.text)) {
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
  http.get("/api/available-models", ({ cookies }) => {
    const user = sessionUser(cookies)!;
    const items = modelConnections.filter((model) => canAccessModel(user, model) && model.has_api_key && model.checked_at && model.capabilities.text)
      .map(({ id: modelID, name, model_name, capabilities }) => ({ id: modelID, name, model_name, capabilities }));
    const configured = Boolean(defaultModelID && items.some((item) => item.id === defaultModelID)
      && modelConnections.some((model) => model.id === defaultModelID && model.shared));
    return json({ items, configuration_status: modelConfigurationStatus(user), default_model_configured: configured, default_model_id: configured ? defaultModelID : null });
  }),
  http.get("/api/personal-data/quota", ({ cookies }) => {
    const user = sessionUser(cookies)!;
    return json({ owner_id: user.id, used_bytes: 0, reserved_bytes: 0, quota_bytes: defaultPersonalDataQuota.quota_bytes,
      max_attachment_bytes: defaultPersonalDataQuota.max_attachment_bytes, remaining_bytes: defaultPersonalDataQuota.quota_bytes, version: 1 });
  }),
  http.get("/api/personal-data/:type/:objectID/history", ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const type = String(params.type);
    const objectID = String(params.objectID);
    const key = personalObjectKey(user.id, type, objectID);
    if (type !== "memory" || !personalObjects.has(key)) return failure("not_found", "个人记忆不存在或无权访问", 404);
    const query = new URL(request.url).searchParams;
    const limit = Number(query.get("limit") ?? 100);
    if (!Number.isSafeInteger(limit) || limit < 1 || limit > 100) return failure("invalid_argument", "历史分页参数无效", 400);
    const cursor = query.get("cursor");
    const page = cursor ? personalMemoryHistoryCursors.get(cursor) : undefined;
    if (cursor && (!page || page.ownerID !== user.id || page.objectID !== objectID)) return failure("invalid_argument", "历史游标无效", 400);
    const revisions = personalMemoryHistory.get(key) ?? [];
    const offset = page?.offset ?? 0;
    const items = revisions.slice(offset, offset + limit);
    const hasMore = offset + items.length < revisions.length;
    let nextCursor: string | null = null;
    if (hasMore) {
      nextCursor = id("history");
      personalMemoryHistoryCursors.set(nextCursor, { ownerID: user.id, objectID, offset: offset + items.length });
    }
    return json({ items, next_cursor: nextCursor });
  }),
  http.get("/api/personal-data/:type/:objectID", ({ params, cookies }) => {
    const user = sessionUser(cookies)!;
    const type = String(params.type);
    const objectID = String(params.objectID);
    const object = personalObjects.get(personalObjectKey(user.id, type, objectID));
    return object ? json(object.value) : failure("not_found", "个人资料不存在", 404);
  }),
  http.put("/api/personal-data/:type/:objectID", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const type = String(params.type);
    const objectID = String(params.objectID);
    if (!/^[A-Za-z0-9._~-]{1,160}$/.test(objectID) || (type === "memory" && !/^m_[a-f0-9]{8}$/.test(objectID)) ||
      ((type === "profile" || type === "feed_preferences") && objectID !== "default")) return failure("invalid_argument", "个人资料编号无效", 400);
    const mutationID = request.headers.get("Idempotency-Key") ?? "";
    if (!/^[A-Za-z0-9._~-]{8,128}$/.test(mutationID)) return failure("invalid_argument", "需要有效的 Idempotency-Key", 400);
    const requestBody = await request.json().catch(() => null) as { base_revision?: unknown; body?: unknown } | null;
    if (!requestBody || Object.keys(requestBody).some((key) => key !== "base_revision" && key !== "body") ||
      !Number.isSafeInteger(requestBody.base_revision) || Number(requestBody.base_revision) < 0 ||
      !requestBody.body || typeof requestBody.body !== "object" || Array.isArray(requestBody.body)) {
      return failure("invalid_argument", "个人资料变更无效", 400);
    }
    if (!validPersonalMutation(type, objectID, requestBody.body)) return failure("invalid_argument", "个人资料内容无效", 400);
    const body = requestBody.body;
    const normalizedRequest = JSON.stringify({ type, objectID, base_revision: requestBody.base_revision, body });
    const replayKey = `${user.id}\u0000${type}\u0000${objectID}\u0000${mutationID}`;
    const prior = personalMutationReplays.get(replayKey);
    if (prior) return prior.request === normalizedRequest
      ? json({ ...prior.response, replay: true }, 200)
      : failure("idempotency_conflict", "幂等键已用于不同的个人资料变更", 409);
    const key = personalObjectKey(user.id, type, objectID);
    const current = personalObjects.get(key)?.value;
    const baseRevision = Number(requestBody.base_revision);
    if ((current?.revision ?? 0) !== baseRevision) return failure("conflict", "个人资料版本已变化", 409);
    const value: PersonalObject = { object_type: type as PersonalObject["object_type"], object_id: objectID, revision: baseRevision + 1, body: body as PersonalObject["body"] };
    const sequence = savePersonalObject(user.id, value, mutationID);
    if (type === "memory") {
      const historyKey = personalObjectKey(user.id, type, objectID);
      const history = personalMemoryHistory.get(historyKey) ?? [];
      history.unshift({ revision: value.revision, changed_at: now(), body: value.body as components["schemas"]["PersonalMemory"],
        restorable: true, source_status: "available", sources: [{ kind: "mutation_receipt", id: mutationID }] });
      personalMemoryHistory.set(historyKey, history);
    }
    const receipt: PersonalMutationReceipt = { object_type: type as PersonalMutationReceipt["object_type"], object_id: objectID, result_revision: value.revision, account_change_sequence: sequence, replay: false };
    personalMutationReplays.set(replayKey, { request: normalizedRequest, response: receipt });
    return json(receipt, 200);
  }),
  http.delete("/api/personal-data/:type/:objectID", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const type = String(params.type);
    const objectID = String(params.objectID);
    const mutationID = request.headers.get("Idempotency-Key") ?? "";
    if (!/^[A-Za-z0-9._~-]{1,160}$/.test(objectID) || (type === "memory" && !/^m_[a-f0-9]{8}$/.test(objectID))) return failure("invalid_argument", "个人资料编号无效", 400);
    if (!/^[A-Za-z0-9._~-]{8,128}$/.test(mutationID)) return failure("invalid_argument", "需要有效的 Idempotency-Key", 400);
    const requestBody = await request.json().catch(() => null) as { base_revision?: unknown } | null;
    if (!requestBody || Object.keys(requestBody).some((field) => field !== "base_revision") ||
      !Number.isSafeInteger(requestBody.base_revision) || Number(requestBody.base_revision) < 0) return failure("invalid_argument", "个人资料删除版本无效", 400);
    const replayKey = `${user.id}\u0000${type}\u0000${objectID}\u0000${mutationID}`;
    const normalizedRequest = JSON.stringify({ operation: "delete", type, objectID, base_revision: requestBody.base_revision });
    const prior = personalMutationReplays.get(replayKey);
    if (prior) return prior.request === normalizedRequest
      ? json({ ...prior.response, replay: true }) : failure("idempotency_conflict", "幂等键已用于不同的个人资料变更", 409);
    const key = personalObjectKey(user.id, type, objectID);
    const current = personalObjects.get(key)?.value;
    if (!current) return failure("not_found", "个人资料不存在或无权访问", 404);
    if (current.object_type === "schedule") return failure("invalid_argument", "计划需要通过计划接口管理", 400);
    if (Number(requestBody.base_revision) !== current.revision) return failure("conflict", "个人资料版本已变化", 409);
    const sequence = (personalSequences.get(user.id) ?? 0) + 1;
    personalSequences.set(user.id, sequence);
    personalObjects.delete(key);
    personalMemoryHistory.delete(key);
    personalChanges.push({ ownerID: user.id, value: { change_sequence: sequence, object_type: current.object_type,
      object_id: objectID, revision: current.revision + 1, operation: "delete", mutation_id: mutationID } });
    const receipt: PersonalMutationReceipt = { object_type: current.object_type, object_id: objectID,
      result_revision: current.revision + 1, account_change_sequence: sequence, replay: false };
    personalMutationReplays.set(replayKey, { request: normalizedRequest, response: receipt });
    return json(receipt);
  }),
  http.get("/api/sync/snapshot", ({ cookies }) => {
    const user = sessionUser(cookies)!;
    const items = Array.from(personalObjects.values()).filter((item) => item.ownerID === user.id).map((item) => item.value);
    return json({ watermark: personalSequences.get(user.id) ?? 0, items, next_cursor: null });
  }),
  http.get("/api/sync/changes", ({ request, cookies }) => {
    const user = sessionUser(cookies)!;
    const query = new URL(request.url).searchParams;
    const after = Number(query.get("after_sequence"));
    const rawThrough = query.get("through_sequence");
    const limit = Number(query.get("limit") ?? 100);
    const through = rawThrough === null ? Math.max(after, personalSequences.get(user.id) ?? 0) : Number(rawThrough);
    if (!Number.isSafeInteger(after) || after < 0 || !Number.isSafeInteger(through) || through < after ||
      !Number.isSafeInteger(limit) || limit < 1 || limit > 100) return failure("invalid_argument", "变更游标无效", 400);
    const available = personalChanges.filter((item) => item.ownerID === user.id && item.value.change_sequence > after && item.value.change_sequence <= through);
    const items = available.slice(0, limit).map((item) => item.value);
    const hasMore = available.length > items.length;
    return json({ items, through_sequence: through, next_after_sequence: hasMore ? items.at(-1)!.change_sequence : through, has_more: hasMore });
  }),
  http.get("/api/me/records", ({ cookies }) => {
    sessionUser(cookies)!;
    return json({ items: [], next_before: "" });
  }),
  http.get("/api/me/record-captures", ({ cookies }) => {
    sessionUser(cookies)!;
    return json({ items: [], next_before: "" });
  }),
  http.post("/api/personal-data/history/import", async ({ request, cookies }) => {
    sessionUser(cookies)!;
    const body = await request.json().catch(() => null) as components["schemas"]["PersonalHistoryImportRequest"] | null;
    if (!body || !body.source_id || !body.device_account_id || !body.source_conversation_id || !Array.isArray(body.messages) ||
      body.messages.length < 1 || body.messages.length > 200 || body.messages.some((message) => !message.source_message_id ||
        (message.role !== "user" && message.role !== "assistant") || !message.content)) {
      return failure("invalid_argument", "历史导入内容无效", 400);
    }
    const conversationID = body.target_conversation_id ?? id("c");
    return json({ conversation_id: conversationID, title: body.title ?? body.source_conversation_id, replay: false,
      messages: body.messages.map((message, index) => ({ source_message_id: message.source_message_id, message_id: id("m"),
        conversation_id: conversationID, role: message.role, content: message.content, sequence: index + 1,
        attachment_ids: message.attachment_ids ?? [], replay: false })) });
  }),
  http.get("/api/goals", ({ cookies }) => {
    const user = sessionUser(cookies)!;
    const items = Array.from(personalObjects.values()).filter((item) => item.ownerID === user.id && item.value.object_type === "goal").map((item) => item.value);
    return json({ items, next_cursor: null });
  }),
  http.post("/api/goals", async ({ request, cookies }) => {
    const user = sessionUser(cookies)!;
    const mutationID = request.headers.get("Idempotency-Key") ?? "";
    if (!/^[A-Za-z0-9._~-]{8,128}$/.test(mutationID)) return failure("invalid_argument", "需要有效的 Idempotency-Key", 400);
    const body = await request.json().catch(() => null) as { id?: unknown; body?: unknown } | null;
    if (!body || Object.keys(body).some((key) => key !== "id" && key !== "body") || typeof body.id !== "string" ||
      !/^[A-Za-z0-9._~-]{1,128}$/.test(body.id) || !validGoalBody(body.body)) return failure("invalid_argument", "目标内容无效", 400);
    const normalizedRequest = JSON.stringify({ id: body.id, body: body.body });
    const replayKey = `${user.id}\u0000goal-create\u0000${mutationID}`;
    const prior = personalMutationReplays.get(replayKey);
    if (prior) return prior.request === normalizedRequest
      ? json({ ...prior.response, replay: true }, 201)
      : failure("idempotency_conflict", "幂等键已用于不同的目标创建请求", 409);
    if (personalObjects.has(personalObjectKey(user.id, "goal", body.id))) return failure("conflict", "目标已存在", 409);
    const value: PersonalObject = { object_type: "goal", object_id: body.id, revision: 1, body: body.body };
    const sequence = savePersonalObject(user.id, value, mutationID);
    const receipt: PersonalMutationReceipt = { object_type: "goal", object_id: body.id, result_revision: 1, account_change_sequence: sequence, replay: false };
    personalMutationReplays.set(replayKey, { request: normalizedRequest, response: receipt });
    return json(receipt, 201);
  }),
  http.get("/api/goals/:id", ({ params, cookies }) => {
    const user = sessionUser(cookies)!;
    const object = personalObjects.get(personalObjectKey(user.id, "goal", String(params.id)));
    return object ? json(object.value) : failure("not_found", "目标不存在", 404);
  }),
  http.get("/api/schedules", ({ cookies }) => json({
    items: schedules.filter((item) => item.ownerID === sessionUser(cookies)!.id).map((item) => item.value), next_cursor: "",
  })),
  http.post("/api/schedules", async ({ request, cookies }) => {
    const user = sessionUser(cookies)!;
    const body = await request.json().catch(() => null) as ScheduleCreateRequest | null;
    if (!body || (body.host_id !== null && !body.host_id) || (body.host_id === null && body.observation != null) ||
      !body.model_id || typeof body.objective !== "string" || !body.objective.trim() ||
      (body.title !== undefined && (typeof body.title !== "string" || !body.title.trim())) || !body.timing || !body.budget) {
      return failure("invalid_argument", "计划定义无效", 400);
    }
    const timestamp = now();
    const value: Schedule = {
      id: id("j"), version: 1, status: "enabled", host_id: body.host_id, model_id: body.model_id,
      title: body.title ?? body.objective.trim(), objective: body.objective, timing: body.timing, budget: body.budget, misfire_policy: body.misfire_policy ?? "skip",
      notification_events: body.notification_events ?? ["completed", "failed", "waiting_user"],
      observation: body.observation ?? null,
      next_run_at: body.timing.kind === "once" ? body.timing.run_at : new Date(Date.now() + 60_000).toISOString(),
      last_run: null, current_run: null, created_at: timestamp, updated_at: timestamp,
    };
    schedules.push({ ownerID: user.id, value });
    return json({ ...value }, 201);
  }),
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
    const allowedPermissions = new Set<HostMemberPermission>(["desktop:view", "desktop:control", "terminal:control", "mcp:control", "workspace:file:read"]);
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
    if (target !== "desktop" && target !== "terminal" && target !== "workspace") return failure("invalid_argument", "入口类型无效", 400);
    const member = hostMembers.get(host?.id ?? "")?.find((item) => item.user_id === user.id);
    const authorized = host && (host.owner_id === user.id || (target === "desktop"
      ? member?.permissions.some((permission) => permission === "desktop:view" || permission === "desktop:control")
      : target === "terminal" ? member?.permissions.includes("terminal:control") : member?.permissions.includes("workspace:file:read")));
    if (!host || !authorized || host.state === "deleting") return failure("not_found", "主机不存在", 404);
    if (materializeHost(host).state !== "running") return failure("invalid_state", "主机尚未运行", 409);
    const url = new URL("/__ch/auth", `https://${host.id}.hosts.mock.invalid`);
    url.searchParams.set("t", `mock-${host.id}-${Date.now()}`);
    if (target === "terminal") url.searchParams.set("next", "/__ch/terminal/");
    if (target === "workspace") url.searchParams.set("next", "/__ch/wb/?tab=workspace");
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
    if (body.workspace_ids !== undefined && (!Array.isArray(body.workspace_ids) || body.workspace_ids.length !== 1 || body.workspace_ids.some((workspaceId) => typeof workspaceId !== "string"))) {
      return failure("invalid_argument", "workspace_ids 必须包含一个工作区编号", 400);
    }
    const template = templates.find((item) => item.id === "std-1c2g")!;
    const resources = { ...user.default_host_resources, cpu_limit_milli: user.default_host_resources.cpu_milli, mem_limit_mib: user.default_host_resources.mem_mib };
    const normalizedRequest = JSON.stringify({ template_id: template.id, name: body.name?.trim() || template.name, workspace_ids: body.workspace_ids ?? null });
    const replayKey = `${user.id}\u0000${idempotencyKey}`;
    const prior = hostCreateReplays.get(replayKey);
    if (prior) {
      if (prior.request !== normalizedRequest) return failure("idempotency_conflict", "幂等键已用于不同的主机创建请求", 409);
      return json(materializeHost(prior.response), 201);
    }
    if (hosts.filter((host) => host.owner_id === user.id).length >= user.quota.hosts) {
      return failure("quota_exceeded", "达到云端电脑上限,无法再创建", 409, { dimension: "hosts", limit: user.quota.hosts });
    }
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
    let selectedWorkspaces: Array<Workspace | undefined>;
    if (body.workspace_ids !== undefined) {
      selectedWorkspaces = body.workspace_ids.map((workspaceId) => workspaces.find((item) => item.id === workspaceId && workspaceOwners.get(item.id) === user.id));
      if (selectedWorkspaces.some((workspace) => !workspace || workspace.state !== "ready")) return failure("workspace_busy", "工作区不存在、正在删除或无权访问", 409);
    } else {
      let selected = workspaces.find((item) => item.state === "ready" && workspaceOwners.get(item.id) === user.id &&
        !hosts.some((host) => host.owner_id === user.id && host.workspaces.some((binding) => binding.workspace.id === item.id && binding.state !== "detached")));
      if (!selected) {
        const timestamp = now();
        selected = { id: id("w"), name: "Default Workspace", revision: 0, used_bytes: 0,
          quota_bytes: template.workspace_size_mib * 1024 * 1024, is_read_only: false, state: "ready", created_at: timestamp, updated_at: timestamp };
        workspaces = [selected, ...workspaces];
        workspaceOwners.set(selected.id, user.id);
      }
      selectedWorkspaces = [selected];
    }
    const timestamp = now();
    const host: Host = { id: id("h"), name: body.name?.trim() || template.name, owner_id: user.id, template_id: template.id, isolation: template.isolation, state: "creating", state_reason: "", runtime_generation: 1, control_version: 0, control_state: "ai", url: import.meta.env.VITE_WORKBENCH_URL ?? "http://127.0.0.1:5174", workspaces: selectedWorkspaces.filter((workspace): workspace is Workspace => Boolean(workspace)).map((workspace): WorkspaceBinding => ({ workspace: toWorkspaceRef(workspace), directory: `projects/${workspace.id}`, state: "active", binding_version: 1, desired_revision: workspace.revision, applied_revision: workspace.revision, sync_state: "healthy", sync_conflicts: [] })), resources, expires_at: new Date(Date.now() + template.ttl_seconds * 1000).toISOString(), last_active_at: timestamp, created_at: timestamp };
    hosts = [host, ...hosts];
    hostCreateReplays.set(replayKey, { request: normalizedRequest, response: host });
    persistAssistantState();
    return json(host, 201);
  }),
  http.post("/api/hosts/:hostId/copies", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const source = hosts.find((item) => item.id === String(params.hostId) && item.owner_id === user.id);
    if (!source) return failure("not_found", "主机不存在", 404);
    const key = request.headers.get("Idempotency-Key") ?? "";
    if (!/^[A-Za-z0-9._~-]{8,128}$/.test(key)) return failure("invalid_argument", "需要有效的 Idempotency-Key", 400);
    const body = await request.json().catch(() => null) as { name?: unknown; expected_current_revision?: unknown } | null;
    if (!body || typeof body !== "object" || Array.isArray(body) || Object.keys(body).some((field) => field !== "name" && field !== "expected_current_revision")
      || typeof body.name !== "string" || !body.name.trim() || Array.from(body.name.trim()).length > 100
      || typeof body.expected_current_revision !== "number" || !Number.isSafeInteger(body.expected_current_revision) || body.expected_current_revision < 0) {
      return failure("invalid_argument", "电脑复制请求无效", 400);
    }
    const name = body.name.trim();
    const normalizedRequest = JSON.stringify({ name, expected_current_revision: body.expected_current_revision });
    const replayKey = `${user.id}\u0000${source.id}\u0000${key}`;
    const prior = hostCopyReplays.get(replayKey);
    if (prior) {
      if (prior.request !== normalizedRequest) return failure("idempotency_conflict", "幂等键已用于不同的电脑复制请求", 409);
      return json({ ...prior.response, replayed: true }, 200);
    }
    if (source.state !== "stopped" || source.sandbox_id || source.control_state !== "ai") return failure("workspace_busy", "请先停止源电脑并交还 AI 控制权", 409);
    const bindings = source.workspaces.filter((binding) => binding.state !== "detached");
    const binding = bindings[0];
    const sourceWorkspace = binding && workspaces.find((item) => item.id === binding.workspace.id && workspaceOwners.get(item.id) === user.id);
    if (bindings.length !== 1 || !binding || binding.state !== "active" || !sourceWorkspace || sourceWorkspace.state !== "ready"
      || binding.sync_state !== "healthy" || binding.sync_conflicts.length > 0 || binding.desired_revision !== body.expected_current_revision
      || binding.applied_revision !== body.expected_current_revision || sourceWorkspace.revision !== body.expected_current_revision) {
      return failure("workspace_conflict", "源电脑文件尚未完全确认", 409);
    }
    const usage = usageFor(user.id);
    if (usage.hosts >= user.quota.hosts || usage.cpu_milli + source.resources.cpu_milli > user.quota.cpu_milli
      || usage.mem_mib + source.resources.mem_mib > user.quota.mem_mib) return failure("quota_exceeded", "已达到用户主机配额上限", 409);
    const allocated = capacityUsage();
    if (allocated.cpu_milli + source.resources.cpu_milli > capacityLimit.cpu_milli
      || allocated.mem_mib + source.resources.mem_mib > capacityLimit.mem_mib) return failure("capacity_exceeded", "集群资源容量不足", 409);

    const timestamp = now();
    const copiedWorkspace: Workspace = { ...sourceWorkspace, id: id("w"), name: `${name} 文件`, revision: 1, state: "ready", created_at: timestamp, updated_at: timestamp };
    const copiedHost: Host = {
      id: id("h"), name, owner_id: user.id, template_id: source.template_id, isolation: source.isolation,
      state: "creating", state_reason: "", runtime_generation: 1, control_version: 0, control_state: "ai",
      url: import.meta.env.VITE_WORKBENCH_URL ?? "http://127.0.0.1:5174",
      workspaces: [{ workspace: toWorkspaceRef(copiedWorkspace), directory: `projects/${copiedWorkspace.id}`, state: "active", binding_version: 1,
        desired_revision: 1, applied_revision: 1, sync_state: "healthy", sync_conflicts: [] }],
      resources: { ...source.resources }, expires_at: new Date(Date.now() + 8 * 60 * 60 * 1000).toISOString(), last_active_at: timestamp, created_at: timestamp,
    };
    const result: HostCopyResult = {
      source_host_id: source.id, source_workspace_id: sourceWorkspace.id, host: copiedHost,
      workspace: { id: copiedWorkspace.id, name: copiedWorkspace.name, revision: 1, state: "ready" },
      source_revision: sourceWorkspace.revision, state: "completed", replayed: false,
    };
    workspaces.unshift(copiedWorkspace);
    workspaceOwners.set(copiedWorkspace.id, user.id);
    hosts.unshift(copiedHost);
    hostCopyReplays.set(replayKey, { request: normalizedRequest, response: result });
    persistAssistantState();
    return json(result, 201);
  }),
  http.get("/api/hosts/:hostId/control", ({ params, cookies }) => {
    const user = sessionUser(cookies)!;
    const host = findHost(String(params.hostId), user);
    if (!host || host.owner_id !== user.id || host.state !== "running") return failure("not_found", "主机不存在", 404);
    const expiresAt = hostControlLeases.get(host.id) ?? null;
    const leaseActive = host.control_state === "user" && !!expiresAt && Date.parse(expiresAt) > Date.now();
    if (!leaseActive) hostControlLeases.delete(host.id);
    return json({
      host_id: host.id,
      control_state: host.control_state,
      control_version: host.control_version,
      control_actor_user_id: leaseActive ? host.owner_id : "",
      control_lease_expires_at: leaseActive ? expiresAt : null,
      control_lease_active: leaseActive,
      control_lease_held_by_me: leaseActive && user.id === host.owner_id,
      active_transition: hostControlTransitions.get(host.id) ?? null,
    });
  }),
  http.post("/api/hosts/:hostId/control/lease/renew", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const host = findHost(String(params.hostId), user);
    if (!host || host.owner_id !== user.id || host.state !== "running") return failure("not_found", "主机不存在", 404);
    const body = await request.json().catch(() => null) as { expected_control_version?: unknown } | null;
    if (!body || Object.keys(body).length !== 1 || !Number.isSafeInteger(body.expected_control_version)
      || Number(body.expected_control_version) < 0) return failure("invalid_argument", "控制租约续期请求无效", 400);
    const expiresAt = hostControlLeases.get(host.id);
    if (body.expected_control_version !== host.control_version || host.control_state !== "user"
      || !expiresAt || Date.parse(expiresAt) <= Date.now()) return failure("conflict", "控制租约已变化或过期", 409);
    const renewedExpiry = new Date(Date.now() + 30_000).toISOString();
    hostControlLeases.set(host.id, renewedExpiry);
    return json({
      host_id: host.id, control_state: host.control_state, control_version: host.control_version,
      control_actor_user_id: host.owner_id, control_lease_expires_at: renewedExpiry,
      control_lease_active: true, control_lease_held_by_me: true,
      active_transition: hostControlTransitions.get(host.id) ?? null,
    });
  }),
  http.post("/api/hosts/:hostId/control/actions", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const host = findHost(String(params.hostId), user);
    if (!host || host.owner_id !== user.id || host.state !== "running") return failure("not_found", "主机不存在", 404);
    const body = await request.json().catch(() => null) as Partial<ControlActionRequest> | null;
    if (!body || Object.keys(body).sort().join(",") !== "action,expected_control_version,request_id" ||
      (body.action !== "take_over" && body.action !== "release") || typeof body.request_id !== "string" ||
      !/^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/.test(body.request_id) || !Number.isSafeInteger(body.expected_control_version) || (body.expected_control_version ?? -1) < 0) {
      return failure("invalid_argument", "主机控制请求无效", 400);
    }
    const command = body as ControlActionRequest;
    const requestBody = JSON.stringify(command);
    const replayKey = `${user.id}\u0000${host.id}\u0000${command.request_id}`;
    const prior = hostControlActionReplays.get(replayKey);
    if (prior) {
      if (prior.request !== requestBody) return failure("idempotency_conflict", "请求编号已用于不同的控制操作", 409);
      return json({ ...prior.response });
    }

    const active = hostControlTransitions.get(host.id);
    if (active) {
      if (active.request_id !== command.request_id || active.action !== command.action || active.expected_control_version !== command.expected_control_version) {
        return failure("conflict", "已有主机控制切换正在进行", 409);
      }
      host.control_state = command.action === "take_over" ? "user" : "ai";
      host.control_version = active.target_control_version;
      hostControlTransitions.delete(host.id);
      if (host.control_state === "user") hostControlLeases.set(host.id, new Date(Date.now() + 30_000).toISOString());
      else hostControlLeases.delete(host.id);
      const expiresAt = hostControlLeases.get(host.id) ?? null;
      const response: ControlActionResponse = {
        host_id: host.id, control_state: host.control_state, control_version: host.control_version,
        control_actor_user_id: expiresAt ? host.owner_id : "",
        control_lease_expires_at: expiresAt, control_lease_active: !!expiresAt,
        control_lease_held_by_me: !!expiresAt && user.id === host.owner_id,
        active_transition: null, pending_operation_ids: [],
      };
      hostControlActionReplays.set(replayKey, { request: requestBody, response });
      persistAssistantState();
      return json({ ...response });
    }

    if (host.control_version !== command.expected_control_version || (command.action === "take_over" ? host.control_state !== "ai" : host.control_state !== "user")) {
      return failure("conflict", "主机控制版本或状态已变化", 409, { control_version: host.control_version, control_state: host.control_state });
    }
    const blockedVersion = host.control_version + 1;
    const transition: ControlTransition = {
      request_id: command.request_id, action: command.action, phase: "draining", failure_reason: "operations_pending",
      expected_control_version: command.expected_control_version, blocked_control_version: blockedVersion,
      target_control_version: blockedVersion + 1, created_at: now(), updated_at: now(),
    };
    host.control_state = command.action === "take_over" ? "taking_over" : "returning";
    host.control_version = blockedVersion;
    hostControlTransitions.set(host.id, transition);
    const response: ControlActionResponse = {
      host_id: host.id, control_state: host.control_state, control_version: host.control_version,
      control_actor_user_id: "",
      control_lease_expires_at: null, control_lease_active: false, control_lease_held_by_me: false,
      active_transition: transition, pending_operation_ids: [`op-${host.id}`],
    };
    return json({ ...response }, 202);
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
    const bindings = host.workspaces.filter((binding) => binding.state !== "detached");
    if (bindings.length !== 1 || bindings[0]!.workspace.state !== "ready" || bindings[0]!.sync_state !== "healthy"
      || bindings[0]!.desired_revision !== bindings[0]!.workspace.revision
      || (bindings[0]!.state !== "active" && bindings[0]!.state !== "preparing")) {
      return failure("workspace_conflict", "恢复前需要一个就绪工作区绑定", 409);
    }
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
  http.post("/api/hosts/:hostId/embed-tickets", ({ params, cookies }) => {
    const host = findHost(String(params.hostId), sessionUser(cookies)!);
    if (!host) return failure("not_found", "主机不存在", 404);
    if (host.state !== "running") return failure("invalid_state", "主机尚未运行", 409);
    return failure("not_supported", "本地演示模式无法连接真实云端电脑。", 501);
  }),
  http.get("/api/workspaces", ({ cookies }) => { const user = sessionUser(cookies)!; completeDeletions(); return json({ items: workspaces.filter((workspace) => workspaceOwners.get(workspace.id) === user.id && workspace.state !== "deleted") }); }),
  http.get("/api/workspaces/:workspaceId/manifest", ({ params, request, cookies }) => {
    const user = sessionUser(cookies);
    if (!user) return failure("unauthenticated", "请先登录", 401);
    const workspace = workspaces.find((item) => item.id === String(params.workspaceId) && workspaceOwners.get(item.id) === user.id && item.state !== "deleted");
    if (!workspace) return failure("not_found", "工作区不存在", 404);
    const query = new URL(request.url).searchParams;
    const revisionText = query.get("revision");
    const revision = revisionText === null ? workspace.revision : Number(revisionText);
    if (!Number.isSafeInteger(revision) || revision < 0) return failure("invalid_argument", "revision 无效", 400);
    if (revision > workspace.revision) return failure("not_found", "revision 不存在", 404);
    const cursor = query.get("cursor");
    if (query.has("cursor") && (!cursor || cursor.length > 512)) return failure("invalid_argument", "manifest 游标无效", 400);
    return json({ workspace_id: workspace.id, revision, files: [], empty_directories: [], next_cursor: null });
  }),
  http.get("/api/workspaces/:workspaceId/diff", ({ params, request, cookies }) => {
    const user = sessionUser(cookies);
    if (!user) return failure("unauthenticated", "请先登录", 401);
    const workspace = workspaces.find((item) => item.id === String(params.workspaceId) && workspaceOwners.get(item.id) === user.id && item.state !== "deleted");
    if (!workspace) return failure("not_found", "工作区不存在", 404);
    const query = new URL(request.url).searchParams;
    const fromRevision = Number(query.get("from_revision"));
    const toRevision = Number(query.get("to_revision"));
    if (!Number.isSafeInteger(fromRevision) || !Number.isSafeInteger(toRevision) || fromRevision < 0 || toRevision < 0) {
      return failure("invalid_argument", "revision 无效", 400);
    }
    if (fromRevision > workspace.revision || toRevision > workspace.revision) return failure("not_found", "revision 不存在", 404);
    const changes: components["schemas"]["WorkspaceDiffChange"][] = fromRevision === toRevision ? [] : [{
      path: "README.md", action: "changed",
      before: { kind: "file", path: "README.md", size_bytes: 12, sha256: "a".repeat(64), mode: 420 },
      after: { kind: "file", path: "README.md", size_bytes: 13, sha256: "b".repeat(64), mode: 420 },
      display_status: "text", before_text: `本地合成 revision v${fromRevision}`, after_text: `本地合成 revision v${toRevision}`,
    }];
    return json({ workspace_id: workspace.id, from_revision: fromRevision, to_revision: toRevision, changes });
  }),
  http.post("/api/workspaces/:workspaceId/restore", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies);
    if (!user) return failure("unauthenticated", "请先登录", 401);
    const workspace = workspaces.find((item) => item.id === String(params.workspaceId) && workspaceOwners.get(item.id) === user.id && item.state !== "deleted");
    if (!workspace) return failure("not_found", "工作区不存在", 404);
    const key = request.headers.get("Idempotency-Key") ?? "";
    const body = await request.json().catch(() => null) as components["schemas"]["WorkspaceRestoreRequest"] | null;
    if (!/^[A-Za-z0-9._~-]{8,128}$/.test(key) || !body || Object.keys(body).some((field) =>
      !["target_revision", "expected_current_revision", "paths"].includes(field))
      || !Number.isSafeInteger(body.target_revision) || !Number.isSafeInteger(body.expected_current_revision)
      || !Array.isArray(body.paths) || body.paths.length < 1 || body.paths.length > 1000
      || body.paths.some((path) => typeof path !== "string" || !path || path.startsWith("/") || path.includes("\\")
        || path.split("/").some((part) => !part || part === "." || part === ".."))) {
      return failure("invalid_argument", "Workspace 恢复请求无效", 400);
    }
    const replayKey = `${user.id}:${workspace.id}:${key}`;
    const serialized = JSON.stringify(body);
    const replay = workspaceRestoreReplays.get(replayKey);
    if (replay) return replay.request === serialized
      ? json({ ...replay.response, replayed: true })
      : failure("idempotency_conflict", "幂等键已用于其他请求", 409);
    if (workspace.state !== "ready" || body.expected_current_revision !== workspace.revision
      || body.target_revision < 0 || body.target_revision > workspace.revision) {
      return failure("workspace_conflict", "Workspace 已变化或恢复版本不可用", 409);
    }
    if (hosts.some((host) => host.workspaces.some((binding) => binding.workspace.id === workspace.id && binding.state !== "detached"))) {
      return failure("workspace_busy", "请先解除 Workspace 主机绑定", 409);
    }
    if (body.paths.some((path) => path !== "README.md")) return failure("not_found", "恢复路径不在本地合成版本中", 404);
    const result: components["schemas"]["WorkspaceRestoreResult"] = {
      workspace_id: workspace.id, target_revision: body.target_revision, expected_current_revision: workspace.revision,
      revision: workspace.revision + 1, replayed: false,
    };
    workspace.revision = result.revision;
    workspace.updated_at = now();
    workspaceRestoreReplays.set(replayKey, { request: serialized, response: result });
    persistAssistantState();
    return json(result);
  }),
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
    const workspace: Workspace = { id: id("w"), name: body.name.trim(), revision: 0, used_bytes: 0, quota_bytes: templates[0]!.workspace_size_mib * 1024 * 1024, is_read_only: false, state: "ready", created_at: timestamp, updated_at: timestamp };
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
    if (host.state !== "running" && host.state !== "stopped") return failure("host_not_ready", "主机尚未就绪", 409);
    if (host.workspaces.some((binding) => binding.workspace.id !== body.workspace_id)) return failure("workspace_conflict", "电脑的 Workspace 归属不可更换", 409);

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
    const stopped = host.state === "stopped";
    const binding: WorkspaceBinding = {
      workspace: toWorkspaceRef(workspace), directory: `projects/${workspace.id}`, state: stopped ? "preparing" : "active",
      binding_version: bindingVersion, desired_revision: workspace.revision, applied_revision: stopped ? 0 : workspace.revision,
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
    return workspace ? json({ revision: workspace.revision, used_bytes: workspace.used_bytes, quota_bytes: workspace.quota_bytes,
      remaining_bytes: Math.max(0, workspace.quota_bytes - workspace.used_bytes),
      quota_status: workspace.used_bytes > workspace.quota_bytes ? "over_quota" : workspace.used_bytes === workspace.quota_bytes ? "full" : "available", items: [], quota_pauses: [] }) : failure("not_found", "工作区不存在", 404);
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
  http.get("/api/capabilities", () => json({ service_instance_id: mockServiceInstanceID, api_version: "1",
    hub_version: null, event_version: null, minimum_client_version: null, supported_features: ["password_login", "task_api", "cloud_tasks", "project_work_items_v2", "conversation_auto_routing_v1"], default_max_request_bytes: 1048576 })),
  http.get("/api/execution", ({ cookies }) => {
    const user = sessionUser(cookies)!;
    const ownerTasks = Array.from(tasks.values()).filter((task) => task.owner_id === user.id);
    return json({
      owner_id: user.id, state: "disabled", epoch: 1,
      pending_task_count: ownerTasks.filter((task) => task.status === "queued" || task.status === "running" || task.status === "waiting_user" || task.status === "paused").length,
      runtime: { state: "stopped", runtime_generation: 0, max_concurrent_tasks: 4, active_task_count: 0 }, updated_at: now(),
    });
  }),
  http.post("/api/execution/enable", ({ cookies }) => {
    const user = sessionUser(cookies)!;
    const ownerTasks = Array.from(tasks.values()).filter((task) => task.owner_id === user.id);
    return json({ owner_id: user.id, state: "enabled", epoch: 1,
      pending_task_count: ownerTasks.filter((task) => ["queued", "running", "waiting_user", "paused"].includes(task.status)).length,
      runtime: { state: "not_started", runtime_generation: 0, max_concurrent_tasks: 4, active_task_count: 0 }, updated_at: now(),
    });
  }),
  http.get("/api/me/main-conversation", ({ cookies }) => {
    const user = sessionUser(cookies);
    if (!user) return failure("unauthenticated", "请先登录", 401);
    const state = mainConversations.get(user.id) ?? { status: "uninitialized" as const, conversation_id: null, version: 0 };
    return json({ ...state, conversation_id: state.conversation_id ?? "" });
  }),
  http.post("/api/me/main-conversation/ensure", ({ request, cookies }) => {
    const user = sessionUser(cookies);
    if (!user) return failure("unauthenticated", "请先登录", 401);
    const key = request.headers.get("Idempotency-Key") ?? "";
    if (!/^[A-Za-z0-9._~-]{8,128}$/.test(key)) return failure("invalid_argument", "Idempotency-Key 无效", 400);
    const replayKey = `${user.id}:${key}`;
    const previous = mainConversationReplays.get(replayKey);
    if (previous) return json(previous.response);
    let state = mainConversations.get(user.id) ?? { status: "uninitialized" as const, conversation_id: null, version: 0 };
    if (state.status === "uninitialized") {
      const candidates = conversations.filter((item) => conversationOwners.get(item.id) === user.id);
      if (candidates.length > 1) state = { status: "selection_required", conversation_id: null, version: 0 };
      else {
        let conversation = candidates[0];
        if (conversation) conversation.is_main = true;
        else {
          const timestamp = now();
          conversation = { id: id("c"), scope: "personal", is_main: true, title: "主会话", created_at: timestamp, updated_at: timestamp };
          conversations.push(conversation);
          conversationOwners.set(conversation.id, user.id);
        }
        state = { status: "ready", conversation_id: conversation.id, version: 1 };
      }
      mainConversations.set(user.id, state);
      persistAssistantState();
    }
    mainConversationReplays.set(replayKey, { request: "ensure", response: state });
    return json({ ...state, conversation_id: state.conversation_id ?? "" });
  }),
  http.put("/api/me/main-conversation", async ({ request, cookies }) => {
    const user = sessionUser(cookies);
    if (!user) return failure("unauthenticated", "请先登录", 401);
    const key = request.headers.get("Idempotency-Key") ?? "";
    if (!/^[A-Za-z0-9._~-]{8,128}$/.test(key)) return failure("invalid_argument", "Idempotency-Key 无效", 400);
    const body = await request.json().catch(() => null) as { conversation_id?: unknown; expected_version?: unknown } | null;
    if (!body || Object.keys(body).some((field) => field !== "conversation_id" && field !== "expected_version")
      || typeof body.conversation_id !== "string" || typeof body.expected_version !== "number"
      || !Number.isSafeInteger(body.expected_version) || body.expected_version < 0) {
      return failure("invalid_argument", "主会话更新请求无效", 400);
    }
    const replayKey = `${user.id}:${key}`;
    const requestText = JSON.stringify(body);
    const previous = mainConversationReplays.get(replayKey);
    if (previous) return previous.request === requestText ? json(previous.response) : failure("idempotency_conflict", "幂等键已用于其他请求", 409);
    const current = mainConversations.get(user.id) ?? { status: "uninitialized" as const, conversation_id: null, version: 0 };
    if (body.expected_version !== current.version) return failure("conflict", "主会话版本已变化", 409);
    const conversation = conversations.find((item) => item.id === body.conversation_id && conversationOwners.get(item.id) === user.id);
    if (!conversation) return failure("not_found", "对话不存在", 404);
    for (const item of conversations) {
      if (conversationOwners.get(item.id) === user.id) item.is_main = item.id === conversation.id;
    }
    const state: MainConversation = { status: "ready", conversation_id: conversation.id, version: current.version + 1 };
    mainConversations.set(user.id, state);
    mainConversationReplays.set(replayKey, { request: requestText, response: state });
    persistAssistantState();
    return json(state);
  }),
  http.get("/api/conversations", ({ cookies }) => {
    const user = sessionUser(cookies)!;
    return json({ items: conversations.filter((item) => conversationOwners.get(item.id) === user.id)
      .sort((left, right) => right.updated_at.localeCompare(left.updated_at)), next_cursor: null });
  }),
  http.post("/api/conversations", async ({ request, cookies }) => {
    const user = sessionUser(cookies)!;
    const key = request.headers.get("Idempotency-Key") ?? "";
    if (request.headers.get("X-Morde-Service-Instance-ID") !== mockServiceInstanceID) return failure("conflict", "服务身份不匹配", 409);
    if (request.headers.get("X-Morde-Actor-ID") !== user.id) return failure("conflict", "账号身份不匹配", 409);
    if (!/^[A-Za-z0-9._~-]{8,128}$/.test(key)) return failure("invalid_argument", "Idempotency-Key 无效", 400);
    const body = await request.json().catch(() => null) as Record<string, unknown> | null;
    if (!body || Object.keys(body).some((field) => field !== "host_id" && field !== "title")
      || (body.title !== undefined && (typeof body.title !== "string" || [...body.title.trim()].length > 120))
      || (body.host_id !== undefined && typeof body.host_id !== "string")) return failure("invalid_argument", "对话请求无效", 400);
    const title = typeof body.title === "string" ? body.title.trim() : "";
    const hostID = typeof body.host_id === "string" ? body.host_id : "";
    const fingerprint = JSON.stringify({ host_id: hostID, title });
    const replayKey = `${user.id}:${key}`;
    const replay = conversationCreates.get(replayKey);
    if (replay) return replay.request === fingerprint ? json(replay.response) : failure("idempotency_conflict", "幂等键已用于其他请求", 409);
    if (hostID && !hosts.some((host) => host.id === hostID && host.owner_id === user.id)) return failure("not_found", "电脑不存在", 404);
    const timestamp = now();
    const conversation: Conversation = { id: id("c"), scope: "personal", is_main: false, title, created_at: timestamp, updated_at: timestamp,
      ...(hostID ? { host_id: hostID } : {}) };
    conversations.push(conversation);
    conversationOwners.set(conversation.id, user.id);
    conversationCreates.set(replayKey, { request: fingerprint, response: conversation });
    persistAssistantState();
    return json(conversation, 201);
  }),
  http.get("/api/conversations/:conversationID", ({ params, cookies }) => {
    const user = sessionUser(cookies)!;
    const conversation = conversations.find((item) => item.id === String(params.conversationID) && conversationOwners.get(item.id) === user.id);
    return conversation ? json(conversation) : failure("not_found", "对话不存在", 404);
  }),
  http.put("/api/conversations/:conversationID/message-uploads/:clientMessageID", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const conversationID = String(params.conversationID);
    const clientMessageID = String(params.clientMessageID);
    if (conversationOwners.get(conversationID) !== user.id) return failure("not_found", "对话不存在", 404);
    const identityError = messageUploadIdentityFailure(request, user, clientMessageID, true);
    if (identityError) return identityError;
    const body: unknown = await request.json().catch(() => null);
    if (!validMessageUploadInit(body)) return failure("invalid_argument", "长消息上传初始化参数无效", 400);
    const metadata = messageUploadMetadata(body);
    const fingerprint = canonicalJSON({ metadata, total_bytes: body.total_bytes, total_chunks: body.total_chunks, sha256: body.sha256 });
    const key = messageUploadKey(user.id, null, conversationID, clientMessageID);
    const existing = conversationMessageUploads.get(key);
    if (existing) return existing.fingerprint === fingerprint
      ? json(messageUploadView(existing)) : failure("conflict", "消息编号已用于不同的上传内容", 409);
    const upload: MockMessageUpload = {
      ownerID: user.id, projectID: null, conversationID, clientMessageID, metadata,
      totalBytes: body.total_bytes, totalChunks: body.total_chunks, sha256: body.sha256,
      fingerprint, chunks: new Map(),
    };
    conversationMessageUploads.set(key, upload);
    return json(messageUploadView(upload));
  }),
  http.get("/api/conversations/:conversationID/message-uploads/:clientMessageID", ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const conversationID = String(params.conversationID);
    const clientMessageID = String(params.clientMessageID);
    if (conversationOwners.get(conversationID) !== user.id) return failure("not_found", "对话不存在", 404);
    const identityError = messageUploadIdentityFailure(request, user, clientMessageID, false);
    if (identityError) return identityError;
    const upload = conversationMessageUploads.get(messageUploadKey(user.id, null, conversationID, clientMessageID));
    return upload ? json(messageUploadView(upload)) : failure("not_found", "消息上传不存在", 404);
  }),
  http.put("/api/conversations/:conversationID/message-uploads/:clientMessageID/chunks/:sequence", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const conversationID = String(params.conversationID);
    const clientMessageID = String(params.clientMessageID);
    if (conversationOwners.get(conversationID) !== user.id) return failure("not_found", "对话不存在", 404);
    const identityError = messageUploadIdentityFailure(request, user, clientMessageID, false);
    if (identityError) return identityError;
    const upload = conversationMessageUploads.get(messageUploadKey(user.id, null, conversationID, clientMessageID));
    const body: unknown = await request.json().catch(() => null);
    return putMessageUploadChunk(upload, Number(params.sequence), body);
  }),
  http.post("/api/conversations/:conversationID/message-uploads/:clientMessageID/commit", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const conversationID = String(params.conversationID);
    const clientMessageID = String(params.clientMessageID);
    const conversation = conversations.find((item) => item.id === conversationID && conversationOwners.get(item.id) === user.id);
    if (!conversation) return failure("not_found", "对话不存在", 404);
    const identityError = messageUploadIdentityFailure(request, user, clientMessageID, true);
    if (identityError) return identityError;
    const upload = conversationMessageUploads.get(messageUploadKey(user.id, null, conversationID, clientMessageID));
    if (!upload) return failure("not_found", "消息上传不存在", 404);
    if (upload.message) return json({ message: upload.message }, 202);
    const assembled = await assembleMessageUpload(upload);
    if (assembled.response) return assembled.response;
    const body = { client_message_id: clientMessageID, ...upload.metadata, content: assembled.content! };
    if (!isAutoMessageBody(body)) return failure("invalid_argument", "对话消息无效", 400);
    const replayKey = `${user.id}:${conversationID}:${clientMessageID}`;
    const replay = conversationAcceptances.get(replayKey);
    let message: MockConversationMessage;
    if (replay) {
      if (replay.request !== canonicalJSON(body)) return failure("conflict", "幂等键已用于不同的对话消息", 409);
      message = replay.response.message;
    } else {
      const timestamp = now();
      message = {
        id: id("m"), conversation_id: conversationID, sequence: nextConversationMessageSequence(conversationID),
        role: "user", content: body.content, client_message_id: clientMessageID,
        routing_status: "pending", delivery_status: "pending", created_at: timestamp,
      };
      conversation.updated_at = timestamp;
      conversationMessages.push(message);
      conversationAcceptances.set(replayKey, { request: canonicalJSON(body), response: { message, replayed: false } });
      persistAssistantState();
    }
    upload.message = message;
    return json({ message }, 202);
  }),
  http.get("/api/conversations/:conversationID/messages", ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const conversationID = String(params.conversationID);
    if (conversationOwners.get(conversationID) !== user.id) return failure("not_found", "对话不存在", 404);
    const clientMessageID = new URL(request.url).searchParams.get("client_message_id");
    if (clientMessageID && request.headers.get("X-Morde-Service-Instance-ID") !== mockServiceInstanceID) return failure("conflict", "服务身份不匹配", 409);
    if (clientMessageID && request.headers.get("X-Morde-Actor-ID") !== user.id) return failure("conflict", "账号身份不匹配", 409);
    return json({ items: conversationMessages.filter((item) => item.conversation_id === conversationID
      && (!clientMessageID || item.client_message_id === clientMessageID)), next_cursor: null });
  }),
  http.post("/api/conversations/:conversationID/messages/:messageID/variants", ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const conversationID = String(params.conversationID);
    if (conversationOwners.get(conversationID) !== user.id) return failure("not_found", "对话不存在", 404);
    return createMockMessageVariant({ user, request, conversationID, targetID: String(params.messageID),
      messages: conversationMessages as unknown as Array<MockConversationMessage | ProjectConversationMessage> });
  }),
  http.get("/api/conversations/:conversationID/tasks", ({ params, cookies }) => {
    const user = sessionUser(cookies)!;
    const conversationID = String(params.conversationID);
    if (conversationOwners.get(conversationID) !== user.id) return failure("not_found", "对话不存在", 404);
    return json({ items: Array.from(tasks.values()).filter((task) => task.conversation_id === conversationID
      && task.owner_id === user.id && !["completed", "failed", "cancelled"].includes(task.status)), next_cursor: null });
  }),
  http.post("/api/tasks", async ({ request, cookies }) => {
    const user = sessionUser(cookies)!;
    const key = request.headers.get("Idempotency-Key") ?? "";
    if (!/^[A-Za-z0-9._~-]{8,128}$/.test(key)) return failure("invalid_argument", "Idempotency-Key 无效", 400);
    const body = await request.json().catch(() => null) as {
      host_id?: string; workspace_id?: string; input_revision?: number; application_id?: string; model_id?: string;
      objective?: string; budget?: { active_seconds: number; tool_calls: number; model_tokens: number };
    } | null;
    const allowed = ["host_id", "workspace_id", "input_revision", "application_id", "model_id", "objective", "budget"];
    if (!body || Object.keys(body).some((field) => !allowed.includes(field)) || typeof body.model_id !== "string"
      || typeof body.objective !== "string" || !body.objective.trim() || Array.from(body.objective).length > 32768) {
      return failure("invalid_argument", "任务内容无效", 400);
    }
    const requestText = JSON.stringify({ ...body, objective: body.objective.trim() });
    const replayKey = `${user.id}:${key}`;
    const replay = taskSubmitReplays.get(replayKey);
    if (replay) return replay.request === requestText ? json({ ...replay.response, replayed: true }, 202) : failure("idempotency_conflict", "幂等键已用于其他请求", 409);
    if (!modelConnections.some((item) => item.id === body.model_id && canAccessModel(user, item) && item.has_api_key)) return failure("not_found", "模型不存在", 404);
    const timestamp = now();
    const conversation: Conversation = { id: id("c"), scope: "personal", is_main: false,
      title: Array.from(body.objective.trim()).slice(0, 120).join(""), created_at: timestamp, updated_at: timestamp };
    conversations.push(conversation);
    conversationOwners.set(conversation.id, user.id);
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
    const body: unknown = await request.json().catch(() => null);
    if (!isAutoMessageBody(body)) return failure("invalid_argument", "对话消息无效", 400);
    const key = request.headers.get("Idempotency-Key")?.trim() ?? "";
    if (request.headers.get("X-Morde-Service-Instance-ID") !== mockServiceInstanceID) return failure("conflict", "服务身份不匹配", 409);
    if (request.headers.get("X-Morde-Actor-ID") !== user.id) return failure("conflict", "账号身份不匹配", 409);
    if (key !== body.client_message_id) return failure("invalid_argument", "消息编号与请求编号不一致", 400);
    const requestText = canonicalJSON(body);
    const replayKey = `${user.id}:${conversationID}:${key}`;
    const replay = conversationAcceptances.get(replayKey);
    if (replay) return replay.request === requestText
      ? json({ ...replay.response, replayed: true }, 202)
      : failure("conflict", "幂等键已用于不同的对话消息", 409);
    const timestamp = now();
    const message: MockConversationMessage = {
      id: id("m"), conversation_id: conversationID, sequence: nextConversationMessageSequence(conversationID),
      role: "user", content: body.content, client_message_id: body.client_message_id,
      routing_status: "pending", delivery_status: "pending", created_at: timestamp,
    };
    conversation.updated_at = timestamp;
    conversationMessages.push(message);
    const response: components["schemas"]["AcceptedConversationMessageResponse"] = { message, replayed: false };
    conversationAcceptances.set(replayKey, { request: requestText, response });
    persistAssistantState();
    return json(response, 202);
  }),
  http.post("/api/tasks/:taskID/actions", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const taskID = String(params.taskID);
    const task = tasks.get(taskID);
    if (!task || task.owner_id !== user.id) return failure("not_found", "任务不存在", 404);
    const key = request.headers.get("Idempotency-Key") ?? "";
    if (!/^[A-Za-z0-9._~-]{8,128}$/.test(key)) return failure("invalid_argument", "Idempotency-Key 无效", 400);
    const body = await request.json().catch(() => null) as {
      action?: unknown;
      expected_status?: unknown;
      expected_current_revision?: unknown;
      expected_desired_revision?: unknown;
    } | null;
    if (!body || Object.keys(body).some((field) => !["action", "expected_status", "expected_current_revision", "expected_desired_revision"].includes(field))
      || !["pause", "resume", "cancel"].includes(String(body.action)) || typeof body.expected_status !== "string") {
      return failure("invalid_argument", "任务操作请求无效", 400);
    }
    const hasCurrentRevision = Object.hasOwn(body, "expected_current_revision");
    const hasDesiredRevision = Object.hasOwn(body, "expected_desired_revision");
    if (hasCurrentRevision !== hasDesiredRevision || (hasCurrentRevision && (
      typeof body.expected_current_revision !== "number" || !Number.isSafeInteger(body.expected_current_revision) || body.expected_current_revision < 0
      || typeof body.expected_desired_revision !== "number" || !Number.isSafeInteger(body.expected_desired_revision) || body.expected_desired_revision < 0
    ))) {
      return failure("invalid_argument", "任务修订条件必须同时提供非负整数", 400);
    }
    const action = body.action as "pause" | "resume" | "cancel";
    const replayKey = `${user.id}:${key}`;
    const requestText = JSON.stringify({ taskID, ...body });
    const replay = taskActionReplays.get(replayKey);
    if (replay) return replay.request === requestText ? json(replay.response, 202) : failure("idempotency_conflict", "幂等键已用于其他请求", 409);
    const allowedExpectedStatuses = action === "resume" ? ["paused"]
      : action === "pause" ? ["queued", "running", "waiting_user"]
        : ["queued", "running", "waiting_user", "paused"];
    if (!allowedExpectedStatuses.includes(body.expected_status)) return failure("invalid_argument", "当前任务状态不允许此操作", 400);
    if (task.status !== body.expected_status) return failure("conflict", "任务状态已变化", 409);
    if (hasCurrentRevision && (task.current_revision !== body.expected_current_revision
      || task.desired_revision !== body.expected_desired_revision)) return failure("conflict", "任务修订已变化", 409);
    const timestamp = now();
    const nextStatus = action === "pause" ? "pausing" : action === "resume" ? "queued" : "cancelling";
    task.status = nextStatus;
    task.stop_request = action === "resume" ? null : {
      id: id("s"), action, status: "pending", safe_after: timestamp, unsettled_operation_ids: [],
    };
    task.updated_at = timestamp;
    task.event_sequence += 1;
    const response: components["schemas"]["TaskActionResponse"] = {
      task_id: taskID, action, status: nextStatus, unsettled_operation_ids: [],
    };
    taskActionReplays.set(replayKey, { request: requestText, response });
    persistAssistantState();
    return json(response, 202);
  }),
  http.get("/api/tasks", ({ cookies }) => {
    const user = sessionUser(cookies)!;
    return json({ items: Array.from(tasks.values()).filter((task) => task.owner_id === user.id), next_cursor: null });
  }),
  http.post("/api/tasks/:taskID/workbench-observation-ticket", ({ params, cookies }) => {
    const user = sessionUser(cookies)!;
    const task = tasks.get(String(params.taskID));
    if (!task || task.owner_id !== user.id || !task.host_id) return failure("not_found", "任务不存在", 404);
    const host = hosts.find((item) => item.id === task.host_id);
    if (!host || host.state !== "running") return failure("invalid_state", "任务目标电脑不可用", 409);
    return json({ ticket: `mock-one-time-task-ticket-${task.id}`, task_id: task.id, host_id: host.id,
      expires_at: new Date(Date.now() + 60_000).toISOString() });
  }),
  http.get("/api/tasks/:taskID", ({ params, cookies }) => {
    const user = sessionUser(cookies)!;
    const task = tasks.get(String(params.taskID));
    if (!task || task.owner_id !== user.id) return failure("not_found", "任务不存在", 404);
    return json(task);
  }),
  http.post("/api/tasks/:taskID/steps/:stepID/retries", async ({ params, request, cookies }) => {
    const user = sessionUser(cookies)!;
    const taskID = String(params.taskID);
    const stepID = String(params.stepID);
    const task = tasks.get(taskID);
    if (!task || task.owner_id !== user.id) return failure("not_found", "任务不存在", 404);
    const key = request.headers.get("Idempotency-Key") ?? "";
    if (!/^[A-Za-z0-9._~-]{8,128}$/.test(key)) return failure("invalid_argument", "Idempotency-Key 无效", 400);
    const body = await request.json().catch(() => null) as TaskStepRetryRequest | null;
    if (!body || typeof body !== "object" || Array.isArray(body) ||
      Object.keys(body).some((field) => !["expected_task_revision", "expected_attempt"].includes(field)) ||
      !Number.isSafeInteger(body.expected_task_revision) || body.expected_task_revision < 0 ||
      !Number.isSafeInteger(body.expected_attempt) || body.expected_attempt < 1) {
      return failure("invalid_argument", "步骤重试条件无效", 400);
    }
    const replayKey = `${user.id}:${key}`;
    const requestText = JSON.stringify({ taskID, stepID, ...body });
    const replay = taskStepRetryReplays.get(replayKey);
    if (replay) return replay.request === requestText
      ? json({ ...replay.response, replayed: true }, 202)
      : failure("idempotency_conflict", "幂等键已用于其他请求", 409);
    const source = task.steps.find((step) => step.id === stepID);
    if (!source || task.status !== "failed" || source.status !== "failed" || source.retryable !== true) {
      return failure("invalid_state", "步骤当前不可安全重试", 409);
    }
    if (task.current_revision !== body.expected_task_revision || task.desired_revision !== body.expected_task_revision ||
      source.task_revision !== body.expected_task_revision || source.attempt !== body.expected_attempt) {
      return failure("conflict", "任务修订或步骤尝试已变化", 409);
    }
    const timestamp = now();
    const attemptStep: TaskStep = {
      ...source,
      id: id("s"),
      sequence: Math.max(0, ...task.steps.map((step) => step.sequence)) + 1,
      attempt: source.attempt + 1,
      retry_of_step_id: source.id,
      status: "queued",
      summary: "重试已受理，等待执行。",
      failure_reason: null,
      retryable: false,
      task_revision: body.expected_task_revision,
      created_at: timestamp,
    };
    source.retryable = false;
    task.steps.push(attemptStep);
    task.status = "queued";
    task.current_step = attemptStep.id;
    task.step_count += 1;
    task.event_sequence += 1;
    task.updated_at = timestamp;
    task.result = null;
    task.finished_at = null;
    task.waiting_reason = null;
    const response: TaskStepRetryResult = { task_id: taskID, source_step_id: stepID, attempt_step_id: attemptStep.id,
      attempt: attemptStep.attempt, status: "queued", replayed: false };
    taskStepRetryReplays.set(replayKey, { request: requestText, response });
    persistAssistantState();
    return json(response, 202);
  }),
  http.get("/api/tasks/:taskID/events", ({ params, cookies }) => {
    const user = sessionUser(cookies)!;
    const task = tasks.get(String(params.taskID));
    if (!task || task.owner_id !== user.id) return failure("not_found", "任务不存在", 404);
    if (["completed", "failed", "cancelled"].includes(task.status)) return new HttpResponse(null, { status: 204 });
    return new HttpResponse(new ReadableStream<Uint8Array>({ start() {} }), {
      headers: { "Cache-Control": "no-cache", "Content-Type": "text/event-stream; charset=utf-8" },
    });
  }),
  http.get("/api/model-connections", ({ cookies }) => {
    const user = sessionUser(cookies)!;
    return json({ items: modelConnections.filter((item) => canAccessModel(user, item)), configuration_status: modelConfigurationStatus(user) });
  }),
  http.post("/api/model-connections", async ({ request, cookies }) => {
    const body = await request.json().catch(() => null) as components["schemas"]["CreateModelConnectionRequest"] | null;
    if (!body || typeof body.name !== "string" || !body.name.trim() || typeof body.model_name !== "string" || !body.model_name.trim()
      || typeof body.api_key !== "string" || !body.api_key.trim() || typeof body.base_url !== "string" || typeof body.shared !== "boolean") {
      return failure("invalid_argument", "模型连接信息不完整", 400);
    }
    const actor = sessionUser(cookies)!;
    if (body.shared && !isAdmin(actor)) return failure("forbidden", "只有管理员可配置团队模型", 403);
    let parsedURL: URL;
    try { parsedURL = new URL(body.base_url); } catch { return failure("invalid_argument", "服务地址无效", 400); }
    if (parsedURL.protocol !== "https:" || parsedURL.username || parsedURL.password || parsedURL.hash) return failure("invalid_argument", "服务地址必须是有效的 HTTPS 地址", 400);
    const timestamp = now();
    const connection: ModelConnection = { id: id("p"), name: body.name.trim(), base_url: body.base_url, model_name: body.model_name.trim(), shared: body.shared, has_api_key: true, capabilities: { text: false, tools: false, vision: false, text_stream: false }, checked_at: null, created_at: timestamp, updated_at: timestamp };
    modelConnections.unshift(connection);
    if (!connection.shared) modelConnectionOwners.set(connection.id, actor.id);
    if (connection.shared) recordMockAudit(actor, "model.connection.create", connection.id, { shared: true, model_name: connection.model_name, base_host: parsedURL.hostname, has_api_key: true });
    return json(connection, 201);
  }),
  http.get("/api/model-connections/:connectionID", ({ params, cookies }) => {
    const actor = sessionUser(cookies)!;
    const connection = modelConnections.find((item) => item.id === String(params.connectionID) && canAccessModel(actor, item));
    return connection ? json(connection) : failure("not_found", "模型连接不存在", 404);
  }),
  http.put("/api/model-connections/:connectionID", async ({ params, request, cookies }) => {
    const connection = modelConnections.find((item) => item.id === String(params.connectionID));
    const actor = sessionUser(cookies)!;
    if (!connection || (connection.shared ? !isAdmin(actor) : modelConnectionOwners.get(connection.id) !== actor.id)) return failure("not_found", "模型连接不存在", 404);
    const body = await request.json().catch(() => null) as components["schemas"]["UpdateModelConnectionRequest"] | null;
    if (!body || !body.name.trim() || !body.model_name.trim()) return failure("invalid_argument", "模型连接信息不完整", 400);
    let parsedURL: URL;
    try { parsedURL = new URL(body.base_url); } catch { return failure("invalid_argument", "服务地址无效", 400); }
    if (parsedURL.protocol !== "https:") return failure("invalid_argument", "服务地址必须使用 HTTPS", 400);
    const changedEndpoint = connection.base_url !== body.base_url || connection.model_name !== body.model_name.trim() || Boolean(body.api_key?.trim());
    connection.name = body.name.trim();
    connection.base_url = body.base_url;
    connection.model_name = body.model_name.trim();
    connection.has_api_key ||= Boolean(body.api_key?.trim());
    if (changedEndpoint) {
      connection.checked_at = null;
      connection.capabilities = { text: false, tools: false, vision: false, text_stream: false };
    }
    connection.capabilities.text_stream = false;
    connection.updated_at = now();
    if (connection.shared) recordMockAudit(actor, "model.connection.update", connection.id, { shared: true, model_name: connection.model_name, base_host: parsedURL.hostname, has_api_key: connection.has_api_key, api_key_replaced: Boolean(body.api_key?.trim()) });
    return json(connection);
  }),
  http.delete("/api/model-connections/:connectionID", ({ params, cookies }) => {
    const index = modelConnections.findIndex((item) => item.id === String(params.connectionID));
    const actor = sessionUser(cookies)!;
    const connection = modelConnections[index];
    if (!connection || (connection.shared ? !isAdmin(actor) : modelConnectionOwners.get(connection.id) !== actor.id)) return failure("not_found", "模型连接不存在", 404);
    modelConnections.splice(index, 1);
    modelConnectionOwners.delete(connection.id);
    if (connection.shared) recordMockAudit(actor, "model.connection.delete", connection.id, { shared: true, model_name: connection.model_name, base_host: new URL(connection.base_url).hostname });
    return empty();
  }),
  http.post("/api/model-connections/:connectionID/check", async ({ params, cookies, request }) => {
    const connection = modelConnections.find((item) => item.id === String(params.connectionID));
    const actor = sessionUser(cookies)!;
    if (!connection || (connection.shared ? !isAdmin(actor) : modelConnectionOwners.get(connection.id) !== actor.id)) return failure("not_found", "模型连接不存在", 404);
    const body = await request.json().catch(() => null) as { capability?: string } | null;
    const capability = body?.capability ?? 'all';
    connection.capabilities = {
      ...connection.capabilities,
      text: connection.capabilities.text || capability === 'text' || capability === 'vision' || capability === 'all',
      tools: connection.capabilities.tools || capability === 'tools' || capability === 'all',
      vision: connection.capabilities.vision || capability === 'vision',
      text_stream: capability === 'text_stream',
    };
    connection.checked_at = now();
    connection.updated_at = now();
    if (connection.shared) recordMockAudit(actor, "model.connection.check", connection.id, { shared: true, model_name: connection.model_name, base_host: new URL(connection.base_url).hostname, capabilities: connection.capabilities });
    return json(connection.capabilities);
  }),
  http.get("/api/mcp/connectors", ({ cookies }) => {
    const actor = sessionUser(cookies)!;
    return json({ items: mcpConnectors.filter((connector) => mcpConnectorOwners.get(connector.id) === actor.id) });
  }),
  http.post("/api/mcp/connectors", async ({ request, cookies }) => {
    const body = await request.json().catch(() => null) as components["schemas"]["CreateMCPConnectorRequest"] | null;
    if (!body || typeof body.name !== "string" || !body.name.trim() || body.name.length > 80 || typeof body.endpoint_url !== "string") {
      return failure("invalid_argument", "MCP 工具连接信息不完整", 400);
    }
    let endpoint: URL;
    try { endpoint = new URL(body.endpoint_url); } catch { return failure("invalid_argument", "MCP 服务地址无效", 400); }
    if (endpoint.protocol !== "https:" || endpoint.username || endpoint.password || endpoint.search || endpoint.hash) return failure("invalid_argument", "MCP 服务地址必须是无凭据的 HTTPS 地址", 400);
    if (body.credential !== undefined && (!body.credential || typeof body.credential !== "object" || Array.isArray(body.credential))) return failure("invalid_argument", "MCP 凭据必须是 JSON 对象", 400);
    const actor = sessionUser(cookies)!;
    const timestamp = now();
    const connector: MCPConnector = {
      id: id("k"), name: body.name.trim(), endpoint_url: endpoint.toString(), enabled: false,
      has_credential: Boolean(body.credential), credential_version: body.credential ? 1 : 0,
      revision: 1, created_at: timestamp, updated_at: timestamp,
    };
    mcpConnectors.unshift(connector);
    mcpConnectorOwners.set(connector.id, actor.id);
    return json(connector, 201);
  }),
  http.put("/api/mcp/connectors/:connectorID", async ({ params, request, cookies }) => {
    const actor = sessionUser(cookies)!;
    const connector = mcpConnectors.find((item) => item.id === String(params.connectorID) && mcpConnectorOwners.get(item.id) === actor.id);
    if (!connector) return failure("not_found", "MCP 工具连接不存在", 404);
    const body = await request.json().catch(() => null) as components["schemas"]["UpdateMCPConnectorRequest"] | null;
    if (!body || !body.name.trim() || body.name.length > 80 || typeof body.endpoint_url !== "string") return failure("invalid_argument", "MCP 工具连接信息无效", 400);
    if (body.expected_revision !== connector.revision) return failure("conflict", "MCP 工具连接已变化，请刷新后重试", 409);
    let endpoint: URL;
    try { endpoint = new URL(body.endpoint_url); } catch { return failure("invalid_argument", "MCP 服务地址无效", 400); }
    if (endpoint.protocol !== "https:" || endpoint.username || endpoint.password || endpoint.search || endpoint.hash) return failure("invalid_argument", "MCP 服务地址必须是无凭据的 HTTPS 地址", 400);
    if (connector.endpoint_url !== endpoint.toString()) {
      connector.has_credential = false;
      connector.credential_version += 1;
    }
    connector.name = body.name.trim();
    connector.endpoint_url = endpoint.toString();
    connector.revision += 1;
    connector.updated_at = now();
    return json(connector);
  }),
  http.put("/api/mcp/connectors/:connectorID/credential", async ({ params, request, cookies }) => {
    const actor = sessionUser(cookies)!;
    const connector = mcpConnectors.find((item) => item.id === String(params.connectorID) && mcpConnectorOwners.get(item.id) === actor.id);
    if (!connector) return failure("not_found", "MCP 工具连接不存在", 404);
    const body = await request.json().catch(() => null) as components["schemas"]["RotateMCPConnectorCredentialRequest"] | null;
    if (!body || !body.credential || typeof body.credential !== "object" || Array.isArray(body.credential)) return failure("invalid_argument", "MCP 凭据必须是 JSON 对象", 400);
    if (body.expected_revision !== connector.revision) return failure("conflict", "MCP 工具连接已变化，请刷新后重试", 409);
    connector.has_credential = true;
    connector.credential_version += 1;
    connector.revision += 1;
    connector.updated_at = now();
    return json(connector);
  }),
  http.put("/api/mcp/connectors/:connectorID/enabled", async ({ params, request, cookies }) => {
    const actor = sessionUser(cookies)!;
    const connector = mcpConnectors.find((item) => item.id === String(params.connectorID) && mcpConnectorOwners.get(item.id) === actor.id);
    if (!connector) return failure("not_found", "MCP 工具连接不存在", 404);
    const body = await request.json().catch(() => null) as { enabled?: unknown; expected_revision?: number } | null;
    if (!body || typeof body.enabled !== "boolean" || typeof body.expected_revision !== "number") return failure("invalid_argument", "连接状态或版本无效", 400);
    if (body.expected_revision !== connector.revision) return failure("conflict", "MCP 工具连接已变化，请刷新后重试", 409);
    connector.enabled = body.enabled;
    connector.revision += 1;
    connector.updated_at = now();
    return json(connector);
  }),
  http.delete("/api/mcp/connectors/:connectorID", ({ params, request, cookies }) => {
    const actor = sessionUser(cookies)!;
    const index = mcpConnectors.findIndex((item) => item.id === String(params.connectorID) && mcpConnectorOwners.get(item.id) === actor.id);
    const connector = mcpConnectors[index];
    if (!connector) return failure("not_found", "MCP 工具连接不存在", 404);
    const expectedRevision = Number(new URL(request.url).searchParams.get("expected_revision"));
    if (!Number.isSafeInteger(expectedRevision) || expectedRevision !== connector.revision) return failure("conflict", "MCP 工具连接已变化，请刷新后重试", 409);
    mcpConnectors.splice(index, 1);
    mcpConnectorOwners.delete(connector.id);
    return empty();
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
  http.get("/api/admin/settings", ({ cookies }) => isAdmin(sessionUser(cookies)!) ? json({
    registration_review_required: true,
    automatic_new_user_model_budget: true,
    automatic_invitation_reward: false,
    automatic_contribution_reward: false,
    default_personal_data_quota_bytes: 5 * 1024 ** 3,
    default_personal_attachment_bytes: 300 * 1024 ** 2,
    default_model_budget: defaultModelBudget,
    default_model: adminModelDefault(),
  }) : failure("forbidden", "没有执行此操作的权限", 403)),
  http.put("/api/admin/settings/model-budget-default", async ({ request, cookies }) => {
    const actor = sessionUser(cookies)!;
    if (!isAdmin(actor)) return failure("forbidden", "没有执行此操作的权限", 403);
    const body = await request.json().catch(() => null) as { expected_version?: unknown; token_limit?: unknown; max_concurrent?: unknown } | null;
    if (!body || !Number.isSafeInteger(body.expected_version) || Number(body.expected_version) < 0
      || !Number.isSafeInteger(body.token_limit) || Number(body.token_limit) < 1
      || !Number.isInteger(body.max_concurrent) || Number(body.max_concurrent) < 1 || Number(body.max_concurrent) > 100) {
      return failure("invalid_argument", "新用户默认模型额度无效", 400);
    }
    if (Number(body.expected_version) !== defaultModelBudget.version) return failure("conflict", "默认模型额度版本已变化，请刷新后重试", 409);
    defaultModelBudget = {
      configured: true,
      token_limit: Number(body.token_limit),
      max_concurrent: Number(body.max_concurrent),
      version: defaultModelBudget.version + 1,
    };
    recordMockAudit(actor, "model.budget.default.update", "platform", { token_limit: defaultModelBudget.token_limit, max_concurrent: defaultModelBudget.max_concurrent, version: defaultModelBudget.version });
    return json(defaultModelBudget);
  }),
  http.put("/api/admin/settings/model-default", async ({ request, cookies }) => {
    const actor = sessionUser(cookies)!;
    if (!isAdmin(actor)) return failure("forbidden", "没有执行此操作的权限", 403);
    const body = await request.json().catch(() => null) as { expected_version?: unknown; model_id?: unknown } | null;
    if (!body || !Number.isSafeInteger(body.expected_version) || Number(body.expected_version) < 0
      || typeof body.model_id !== "string") return failure("invalid_argument", "默认模型设置无效", 400);
    if (Number(body.expected_version) !== defaultModelVersion) return failure("conflict", "默认模型版本已变化，请刷新后重试", 409);
    if (body.model_id) {
      const selected = modelConnections.find((model) => model.id === body.model_id);
      if (!selected?.shared || !selected.checked_at || !selected.capabilities.text) {
        return failure("invalid_argument", "默认模型必须是已检查且支持文本的共享连接", 400);
      }
      defaultModelID = selected.id;
    } else {
      defaultModelID = null;
    }
    defaultModelVersion += 1;
    recordMockAudit(actor, "model.default.update", "platform", { configured: Boolean(defaultModelID), model_id: defaultModelID, version: defaultModelVersion });
    return json(adminModelDefault());
  }),
  http.get("/api/admin/invites", ({ request, cookies }) => {
    if (!isAdmin(sessionUser(cookies)!)) return failure("forbidden", "没有执行此操作的权限", 403);
    const query = new URL(request.url).searchParams;
    const searches = query.getAll("search");
    const page = adminPage(request);
    const search = searches[0]?.trim() ?? "";
    if (!page || searches.length > 1 || search.length > 128 || /[\r\n\0]/.test(search)) return failure("invalid_argument", "邀请查询参数无效", 400);
    const items: AdminInvite[] = inviteRelations.flatMap((relation) => {
      const inviter = users.find((user) => user.id === relation.inviter_id);
      const invitee = users.find((user) => user.id === relation.invitee_id);
      if (!inviter || !invitee) return [];
      const match = [inviter.id, inviter.name, invitee.id, invitee.name].some((value) => value.toLowerCase().includes(search.toLowerCase()));
      if (!match) return [];
      return [{ inviter_id: inviter.id, inviter_name: inviter.name, invitee_id: invitee.id, invitee_name: invitee.name,
        invitee_status: invitee.status, created_at: relation.created_at,
        inviter_invitees: inviteRelations.filter((item) => item.inviter_id === inviter.id).length }];
    }).sort((left, right) => right.created_at.localeCompare(left.created_at) || left.invitee_id.localeCompare(right.invitee_id));
    return json({ items: items.slice(page.offset, page.offset + page.limit), total: items.length,
      total_inviters: new Set(items.map((item) => item.inviter_id)).size, ...page });
  }),
  http.get("/api/admin/sites", ({ request, cookies }) => {
    if (!isAdmin(sessionUser(cookies)!)) return failure("forbidden", "没有执行此操作的权限", 403);
    const page = adminPage(request);
    if (!page) return failure("invalid_argument", "分页参数无效", 400);
    return json({ items: [], summary: { sites: 0, active_sites: 0, releases: 0, healthy_releases: 0 },
      integrations: { demo: "not_configured", github_stats: "not_configured", traffic: "not_collected" }, total: 0, ...page });
  }),
  http.get("/api/admin/nudges", ({ cookies }) => isAdmin(sessionUser(cookies)!) ? json(adminNudgePolicy) : failure("forbidden", "没有执行此操作的权限", 403)),
  http.put("/api/admin/nudges", async ({ request, cookies }) => {
    const actor = sessionUser(cookies)!;
    if (!isAdmin(actor)) return failure("forbidden", "没有执行此操作的权限", 403);
    const body = await request.json().catch(() => null) as { expected_version?: unknown; enabled?: unknown; url?: unknown; text_zh?: unknown; text_en?: unknown } | null;
    if (!body || !Number.isSafeInteger(body.expected_version) || Number(body.expected_version) < 1 || typeof body.enabled !== "boolean" ||
      typeof body.url !== "string" || typeof body.text_zh !== "string" || typeof body.text_en !== "string") return failure("invalid_argument", "运营提示无效", 400);
    if (Number(body.expected_version) !== adminNudgePolicy.version) return failure("conflict", "运营提示版本已变化，请刷新后重试", 409);
    const url = body.url.trim();
    const textZH = body.text_zh.trim();
    const textEN = body.text_en.trim();
    const empty = !body.enabled && !url && !textZH && !textEN;
    let validGitHubURL = false;
    try {
      const parsed = new URL(url);
      validGitHubURL = parsed.protocol === "https:" && ["github.com", "www.github.com"].includes(parsed.hostname) &&
        !parsed.username && !parsed.password && !parsed.search && !parsed.hash && parsed.pathname.replaceAll("/", "") !== "";
    } catch { /* Invalid URLs are rejected below. */ }
    const validText = [textZH, textEN].every((value) => Array.from(value).length <= 240 && !/[\r\n\0]/.test(value));
    if (!validText || !(empty || (validGitHubURL && (!body.enabled || textZH || textEN)))) return failure("invalid_argument", "运营提示需要 GitHub 链接和不超过 240 字的中英文说明", 400);
    adminNudgePolicy = { enabled: body.enabled, url, text_zh: textZH, text_en: textEN, version: adminNudgePolicy.version + 1, updated_at: now() };
    recordMockAudit(actor, "admin.nudge.policy.update", "admin:nudge-policy", { version: adminNudgePolicy.version, enabled: adminNudgePolicy.enabled });
    return json(adminNudgePolicy);
  }),
  http.get("/api/admin/overview", ({ cookies }) => {
    const actor = sessionUser(cookies)!;
    if (!isAdmin(actor)) return failure("forbidden", "没有执行此操作的权限", 403);
    const usage = capacityUsage();
    return json({ checked_at: now(), health: { ready: true }, online_installations: 0,
      limit: { hosts: 100, ...capacityLimit }, allocated: { hosts: hosts.filter(occupiesResources).length, ...usage },
      hosts_by_state: hostsByState(), agents_by_state: {} });
  }),
  http.get("/api/admin/model-connections", ({ cookies }) => {
    if (!isAdmin(sessionUser(cookies)!)) return failure("forbidden", "没有执行此操作的权限", 403);
    return json({ items: modelConnections.filter((item) => item.shared) });
  }),
  http.get("/api/admin/usage", ({ request, cookies }) => {
    if (!isAdmin(sessionUser(cookies)!)) return failure("forbidden", "没有执行此操作的权限", 403);
    const query = new URL(request.url).searchParams;
    const values = query.getAll("days");
    const days = Number(values[0] ?? 7);
    if (values.length > 1 || ![7, 30, 90].includes(days)) return failure("invalid_argument", "统计区间必须为 7、30 或 90 天", 400);
    const to = now();
    const from = new Date(Date.now() - days * 86_400_000).toISOString();
    return json({ days, from, to, summary: { calls: 0, settled_calls: 0, unknown_calls: 0, input_tokens: 0, output_tokens: 0, active_accounts: 0 }, by_day: [], by_model: [] });
  }),
  http.get("/api/admin/quality", ({ request, cookies }) => {
    if (!isAdmin(sessionUser(cookies)!)) return failure("forbidden", "没有执行此操作的权限", 403);
    const values = new URL(request.url).searchParams.getAll("days");
    const days = Number(values[0] ?? 7);
    if (values.length > 1 || ![7, 30, 90].includes(days)) return failure("invalid_argument", "统计区间必须为 7、30 或 90 天", 400);
    return json({ days, available: false, unavailable_reason: "没有启用已单独同意的质量样本数据源。" });
  }),
  http.get("/api/admin/activity", ({ request, cookies }) => {
    if (!isAdmin(sessionUser(cookies)!)) return failure("forbidden", "没有执行此操作的权限", 403);
    const values = new URL(request.url).searchParams.getAll("days");
    const days = Number(values[0] ?? 7);
    if (values.length > 1 || ![7, 30, 90].includes(days)) return failure("invalid_argument", "统计区间必须为 7、30 或 90 天", 400);
    const to = new Date();
    const from = new Date(Date.UTC(to.getUTCFullYear(), to.getUTCMonth(), to.getUTCDate() + 1 - days));
    const by_day = Array.from({ length: days }, (_, index) => {
      const date = new Date(from.getTime() + index * 86_400_000).toISOString().slice(0, 10);
      return { date, hub_operations: 0, task_events: 0, audit_events: 0 };
    });
    return json({ days, from: from.toISOString(), to: to.toISOString(), by_day, hub_operations: [], task_events: [], audit_events: [] });
  }),
  http.get("/api/admin/places", ({ request, cookies }) => {
    if (!isAdmin(sessionUser(cookies)!)) return failure("forbidden", "没有执行此操作的权限", 403);
    const values = new URL(request.url).searchParams.getAll("days");
    const days = Number(values[0] ?? 7);
    if (values.length > 1 || ![7, 30, 90].includes(days)) return failure("invalid_argument", "统计区间必须为 7、30 或 90 天", 400);
    return json({ days, available: false, unavailable_reason: "没有合规的聚合位置数据源；不会根据 IP 推断地点。" });
  }),
  http.get("/api/admin/controls", ({ cookies }) => {
    const actor = sessionUser(cookies)!;
    if (!isAdmin(actor)) return failure("forbidden", "没有执行此操作的权限", 403);
    const rules = [...adminControlRules].sort((left, right) => left.threshold - right.threshold || left.id - right.id);
    return json({
      rules,
      accounts_total: users.length,
      next_threshold: rules.find((rule) => rule.enabled && rule.last_fired_at === null)?.threshold ?? null,
      notify_configured: true,
      notifications: adminControlNotifications.slice(0, 20),
    });
  }),
  http.post("/api/admin/controls/rules", async ({ request, cookies }) => {
    const actor = sessionUser(cookies)!;
    if (!isAdmin(actor)) return failure("forbidden", "没有执行此操作的权限", 403);
    const body = await request.json().catch(() => null) as { threshold?: unknown; note?: unknown } | null;
    const note = typeof body?.note === "string" ? body.note.trim() : "";
    if (!body || !Number.isSafeInteger(body.threshold) || Number(body.threshold) < 1 ||
      (body.note !== undefined && typeof body.note !== "string") || Array.from(note).length > 256 || hasControlCharacters(note)) {
      return failure("invalid_argument", "通知规则无效", 400);
    }
    const rule: AdminControlRule = {
      id: nextAdminControlRuleId++, threshold: Number(body.threshold), enabled: true, note, created_at: now(),
      last_fired_at: null, fired_accounts: 0, version: 1,
    };
    adminControlRules.push(rule);
    recordMockAudit(actor, "admin.control.rule.create", `rule:${rule.id}`, { threshold: rule.threshold, version: rule.version });
    return json(rule as unknown as Record<string, unknown>, 201);
  }),
  http.put("/api/admin/controls/rules/:ruleId", async ({ params, request, cookies }) => {
    const actor = sessionUser(cookies)!;
    if (!isAdmin(actor)) return failure("forbidden", "没有执行此操作的权限", 403);
    const rule = adminControlRules.find((item) => item.id === Number(params.ruleId));
    if (!rule) return failure("not_found", "通知规则不存在", 404);
    const body = await request.json().catch(() => null) as { expected_version?: unknown; threshold?: unknown; enabled?: unknown; note?: unknown; rearm?: unknown } | null;
    if (!body || !Number.isSafeInteger(body.expected_version) || Number(body.expected_version) < 1 ||
      (body.threshold !== undefined && (!Number.isSafeInteger(body.threshold) || Number(body.threshold) < 1)) ||
      (body.enabled !== undefined && typeof body.enabled !== "boolean") ||
      (body.note !== undefined && typeof body.note !== "string") ||
      (body.rearm !== undefined && typeof body.rearm !== "boolean") ||
      (body.threshold === undefined && body.enabled === undefined && body.note === undefined && body.rearm !== true)) {
      return failure("invalid_argument", "通知规则更新无效", 400);
    }
    if (Number(body.expected_version) !== rule.version) return failure("conflict", "通知规则版本已变化，请刷新后重试", 409);
    const note = typeof body.note === "string" ? body.note.trim() : rule.note;
    if (Array.from(note).length > 256 || hasControlCharacters(note)) return failure("invalid_argument", "通知规则备注无效", 400);
    const before = rule.version;
    if (body.threshold !== undefined) rule.threshold = Number(body.threshold);
    if (body.enabled !== undefined) rule.enabled = body.enabled;
    if (body.note !== undefined) rule.note = note;
    if (body.rearm) { rule.last_fired_at = null; rule.fired_accounts = 0; }
    rule.version++;
    recordMockAudit(actor, "admin.control.rule.update", `rule:${rule.id}`, { previous_version: before, version: rule.version, threshold: rule.threshold, enabled: rule.enabled });
    return json(rule as unknown as Record<string, unknown>);
  }),
  http.delete("/api/admin/controls/rules/:ruleId", ({ params, request, cookies }) => {
    const actor = sessionUser(cookies)!;
    if (!isAdmin(actor)) return failure("forbidden", "没有执行此操作的权限", 403);
    const rule = adminControlRules.find((item) => item.id === Number(params.ruleId));
    if (!rule) return failure("not_found", "通知规则不存在", 404);
    const versions = new URL(request.url).searchParams.getAll("expected_version");
    if (versions.length !== 1 || !/^[1-9]\d*$/u.test(versions[0]!)) return failure("invalid_argument", "版本号无效", 400);
    if (Number(versions[0]) !== rule.version) return failure("conflict", "通知规则版本已变化，请刷新后重试", 409);
    adminControlRules = adminControlRules.filter((item) => item.id !== rule.id);
    recordMockAudit(actor, "admin.control.rule.delete", `rule:${rule.id}`, { threshold: rule.threshold, version: rule.version });
    return empty();
  }),
  http.post("/api/admin/controls/evaluate", ({ cookies }) => {
    const actor = sessionUser(cookies)!;
    if (!isAdmin(actor)) return failure("forbidden", "没有执行此操作的权限", 403);
    const accountsTotal = users.length;
    const fired = adminControlRules.filter((rule) => rule.enabled && rule.last_fired_at === null && rule.threshold <= accountsTotal);
    for (const rule of fired) {
      rule.last_fired_at = now();
      rule.fired_accounts = accountsTotal;
      rule.version++;
      adminControlNotifications.unshift({ kind: "rule_fired", rule_id: rule.id, accounts_total: accountsTotal, created_at: rule.last_fired_at });
      recordMockAudit(actor, "admin.control.rule.fire", `rule:${rule.id}`, { threshold: rule.threshold, accounts_total: accountsTotal, version: rule.version });
    }
    recordMockAudit(actor, "admin.control.evaluate", "rules", { accounts_total: accountsTotal, fired: fired.length });
    return json({ accounts_total: accountsTotal, fired });
  }),
  http.post("/api/admin/controls/notify-test", ({ cookies }) => {
    const actor = sessionUser(cookies)!;
    if (!isAdmin(actor)) return failure("forbidden", "没有执行此操作的权限", 403);
    const createdAt = now();
    adminControlNotifications.unshift({ kind: "test", rule_id: null, accounts_total: users.length, created_at: createdAt });
    recordMockAudit(actor, "admin.control.notify_test", "local-receiver", { channel: "local_database", accounts_total: users.length });
    return json({ sent: true, configured: true, created_at: createdAt });
  }),
  http.get("/api/admin/stats", ({ cookies }) => {
    if (!isAdmin(sessionUser(cookies)!)) return failure("forbidden", "没有执行此操作的权限", 403);
    const counts = { total: users.length, approved: 0, pending: 0, rejected: 0, disabled: 0 };
    for (const user of users) counts[user.status]++;
    const platforms = new Map<string, { installations: number; active_installations: number; users: Set<string> }>();
    const currentTime = Date.now();
    for (const client of clients) {
      const values = platforms.get(client.platform) ?? { installations: 0, active_installations: 0, users: new Set<string>() };
      values.installations++;
      if (!client.revoked && Date.parse(client.expires_at) > currentTime && client.last_used_at && Date.parse(client.last_used_at) >= currentTime - 30 * 86_400_000) values.active_installations++;
      const ownerID = clientOwners.get(client.id);
      if (ownerID) values.users.add(ownerID);
      platforms.set(client.platform, values);
    }
    return json({ users: counts, platforms: Array.from(platforms, ([platform, values]) => ({ platform, installations: values.installations, active_installations: values.active_installations, users: values.users.size })), cohorts: [], version_tracking_available: false });
  }),
  http.get("/api/admin/users", ({ request, cookies }) => {
    const user = sessionUser(cookies)!;
    if (!isAdmin(user)) return failure("forbidden", "没有执行此操作的权限", 403);
    const page = adminPage(request, 50);
    if (!page) return failure("invalid_argument", "分页参数无效", 400);
    const requested = new URL(request.url).searchParams.getAll("status");
    if (requested.length > 1 || (requested.length === 1 && !validAccountStatus(requested[0]!))) return failure("invalid_argument", "账号状态无效", 400);
    const filtered = users.filter((item) => !requested.length || item.status === requested[0]);
    return json({ items: filtered.slice(page.offset, page.offset + page.limit).map((item) => ({ ...userWithUsage(item), created_at: item.created_at ?? now() })), total: filtered.length, ...page });
  }),
  http.get("/api/admin/users/:userId", ({ params, cookies }) => {
    if (!isAdmin(sessionUser(cookies)!)) return failure("forbidden", "没有执行此操作的权限", 403);
    const user = users.find((item) => item.id === String(params.userId));
    return user ? json({ ...userWithUsage(user), created_at: user.created_at ?? now() }) : failure("not_found", "用户不存在", 404);
  }),
  http.get("/api/admin/users/:userId/installations", ({ params, request, cookies }) => {
    if (!isAdmin(sessionUser(cookies)!)) return failure("forbidden", "没有执行此操作的权限", 403);
    const page = adminPage(request);
    if (!page) return failure("invalid_argument", "分页参数无效", 400);
    const owned = clients.filter((client) => clientOwners.get(client.id) === String(params.userId));
    const items = owned.slice(page.offset, page.offset + page.limit).map((client) => ({ id: client.id, display_name: client.display_name, platform: client.platform,
      created_at: client.created_at, last_used_at: client.last_used_at, expires_at: client.expires_at, revoked_at: null, online: false }));
    return json({ items, total: owned.length, ...page });
  }),
  http.get("/api/admin/users/:userId/authorizations", ({ request, cookies }) => {
    if (!isAdmin(sessionUser(cookies)!)) return failure("forbidden", "没有执行此操作的权限", 403);
    const page = adminPage(request);
    if (!page) return failure("invalid_argument", "分页参数无效", 400);
    return json({ items: [], total: 0, ...page });
  }),
  http.get("/api/admin/users/:userId/model-usage/ledger", ({ request, cookies }) => {
    if (!isAdmin(sessionUser(cookies)!)) return failure("forbidden", "没有执行此操作的权限", 403);
    const page = adminPage(request);
    if (!page) return failure("invalid_argument", "分页参数无效", 400);
    return json({ items: [], total: 0, ...page });
  }),
  http.get("/api/admin/users/:userId/model-usage-budget", ({ params, cookies }) => {
    if (!isAdmin(sessionUser(cookies)!)) return failure("forbidden", "没有执行此操作的权限", 403);
    if (!users.some((user) => user.id === String(params.userId))) return failure("not_found", "用户不存在", 404);
    return json(modelBudgets.get(String(params.userId)) ?? { owner_id: String(params.userId), configured: false, token_limit: 0, max_concurrent: 0, used_tokens: 0, in_flight: 0, has_unknown: false });
  }),
  http.put("/api/admin/users/:userId/model-usage-budget", async ({ params, request, cookies }) => {
    const actor = sessionUser(cookies)!;
    if (!isAdmin(actor)) return failure("forbidden", "没有执行此操作的权限", 403);
    const ownerID = String(params.userId);
    if (!users.some((user) => user.id === ownerID)) return failure("not_found", "用户不存在", 404);
    const body = await request.json().catch(() => null) as { token_limit?: unknown; max_concurrent?: unknown } | null;
    if (!body || !Number.isSafeInteger(body.token_limit) || Number(body.token_limit) < 1 || !Number.isInteger(body.max_concurrent) || Number(body.max_concurrent) < 1 || Number(body.max_concurrent) > 100) return failure("invalid_argument", "平台模型预算无效", 400);
    const budget: ModelAccountBudget = { owner_id: ownerID, configured: true, token_limit: Number(body.token_limit), max_concurrent: Number(body.max_concurrent), used_tokens: 0, in_flight: 0, has_unknown: false };
    modelBudgets.set(ownerID, budget);
    recordMockAudit(actor, "model.usage.budget.update", ownerID, { token_limit: budget.token_limit, max_concurrent: budget.max_concurrent });
    return json(budget);
  }),
  http.get("/api/admin/users/:userId/personal-data-quota", ({ params, cookies }) => {
    if (!isAdmin(sessionUser(cookies)!)) return failure("forbidden", "没有执行此操作的权限", 403);
    const ownerID = String(params.userId);
    if (!users.some((user) => user.id === ownerID)) return failure("not_found", "用户不存在", 404);
    return json(personalDataQuotas.get(ownerID) ?? defaultPersonalDataQuota);
  }),
  http.patch("/api/admin/users/:userId/personal-data-quota", async ({ params, request, cookies }) => {
    const actor = sessionUser(cookies)!;
    if (!isAdmin(actor)) return failure("forbidden", "没有执行此操作的权限", 403);
    const ownerID = String(params.userId);
    if (!users.some((user) => user.id === ownerID)) return failure("not_found", "用户不存在", 404);
    const current = personalDataQuotas.get(ownerID) ?? defaultPersonalDataQuota;
    const body = await request.json().catch(() => null) as { expected_version?: unknown; quota_bytes?: unknown; max_attachment_bytes?: unknown } | null;
    if (!body || !Number.isSafeInteger(body.expected_version) || !Number.isSafeInteger(body.quota_bytes) || Number(body.quota_bytes) < 1 ||
      !Number.isSafeInteger(body.max_attachment_bytes) || Number(body.max_attachment_bytes) < 1 || Number(body.max_attachment_bytes) > 9007199250546688) {
      return failure("invalid_argument", "个人数据额度无效", 400);
    }
    if (Number(body.expected_version) !== current.version) return failure("conflict", "个人数据额度版本已变化，请刷新后重试", 409);
    const updated: PersonalDataQuota = {
      ...current,
      quota_bytes: Number(body.quota_bytes),
      max_attachment_bytes: Number(body.max_attachment_bytes),
      version: current.version + 1,
    };
    personalDataQuotas.set(ownerID, updated);
    recordMockAudit(actor, "personal_data.quota.update", ownerID, {
      quota_bytes: updated.quota_bytes,
      max_attachment_bytes: updated.max_attachment_bytes,
      version: updated.version,
    });
    return json(updated);
  }),
  http.get("/api/me/record-consents", ({ cookies }) => {
    const actor = sessionUser(cookies)!;
    return json({ items: recordConsentState(actor.id) });
  }),
  http.put("/api/me/record-consents", async ({ request, cookies }) => {
    const actor = sessionUser(cookies)!;
    const body = await request.json().catch(() => null) as { purpose?: unknown; enabled?: unknown } | null;
    if (!body || Object.keys(body).some((key) => key !== "purpose" && key !== "enabled") ||
      (body.purpose !== "full_record" && body.purpose !== "training_contribution") || typeof body.enabled !== "boolean") {
      return failure("invalid_argument", "记录同意请求无效", 400);
    }
    const consent = recordConsentState(actor.id).find((item) => item.purpose === body.purpose)!;
    consent.enabled = body.enabled;
    consent.version++;
    consent.updated_at = now();
    recordMockAudit(actor, "record.consent.update", actor.id, { purpose: consent.purpose, enabled: consent.enabled, version: consent.version });
    return json(consent as unknown as Record<string, unknown>);
  }),
  http.get("/api/admin/users/:userId/record-settings", ({ params, cookies }) => {
    const actor = sessionUser(cookies)!;
    if (actor.role !== "super_admin") return failure("forbidden", "没有执行此操作的权限", 403);
    const ownerID = String(params.userId);
    if (!users.some((user) => user.id === ownerID)) return failure("not_found", "用户不存在", 404);
    recordMockAudit(actor, "record.settings.read", ownerID, { capability: "settings" });
    return json({ owner_id: ownerID, consents: recordConsentState(ownerID), quota: recordQuotaState(ownerID), grants: recordGrants.get(ownerID) ?? [] });
  }),
  http.get("/api/admin/users/:userId/record-access", ({ params, cookies }) => {
    const actor = sessionUser(cookies)!;
    if (!isAdmin(actor)) return failure("forbidden", "没有执行此操作的权限", 403);
    const ownerID = String(params.userId);
    if (!users.some((user) => user.id === ownerID)) return failure("not_found", "用户不存在", 404);
    const grants = (recordGrants.get(ownerID) ?? []).filter((item) => item.admin_id === actor.id && item.granted);
    recordMockAudit(actor, "record.access.read", ownerID, { capability: "self_grants" });
    return json({ capabilities: grants.map((item) => item.capability), consents: recordConsentState(ownerID) });
  }),
  http.put("/api/admin/users/:userId/record-quota", async ({ params, request, cookies }) => {
    const actor = sessionUser(cookies)!;
    if (actor.role !== "super_admin") return failure("forbidden", "没有执行此操作的权限", 403);
    const ownerID = String(params.userId);
    if (!users.some((user) => user.id === ownerID)) return failure("not_found", "用户不存在", 404);
    const body = await request.json().catch(() => null) as { expected_version?: unknown; retention_days?: unknown; limit_bytes?: unknown } | null;
    if (!body || Object.keys(body).some((key) => !["expected_version", "retention_days", "limit_bytes"].includes(key)) ||
      !Number.isSafeInteger(body.expected_version) || Number(body.expected_version) < 1 ||
      !Number.isSafeInteger(body.retention_days) || Number(body.retention_days) < 1 || Number(body.retention_days) > 3650 ||
      !Number.isSafeInteger(body.limit_bytes) || Number(body.limit_bytes) < 1) {
      return failure("invalid_argument", "记录额度无效", 400);
    }
    const current = recordQuotaState(ownerID);
    if (Number(body.expected_version) !== current.version) return failure("conflict", "记录额度版本已变化，请刷新后重试", 409);
    if (Number(body.limit_bytes) < current.used_bytes + current.reserved_bytes) return failure("conflict", "记录额度不能低于已使用和已预留容量", 409);
    const updated: RecordQuota = { ...current, retention_days: Number(body.retention_days), limit_bytes: Number(body.limit_bytes), version: current.version + 1, updated_at: now() };
    recordQuotas.set(ownerID, updated);
    recordMockAudit(actor, "record.quota.update", ownerID, { retention_days: updated.retention_days, limit_bytes: updated.limit_bytes, version: updated.version });
    return json(updated as unknown as Record<string, unknown>);
  }),
  http.put("/api/admin/users/:userId/record-grants", async ({ params, request, cookies }) => {
    const actor = sessionUser(cookies)!;
    if (actor.role !== "super_admin") return failure("forbidden", "没有执行此操作的权限", 403);
    const ownerID = String(params.userId);
    if (!users.some((user) => user.id === ownerID)) return failure("not_found", "用户不存在", 404);
    const body = await request.json().catch(() => null) as { admin_id?: unknown; capability?: unknown; granted?: unknown } | null;
    if (!body || Object.keys(body).some((key) => !["admin_id", "capability", "granted"].includes(key)) || typeof body.admin_id !== "string" ||
      !["view", "attachment", "export", "delete"].includes(String(body.capability)) || typeof body.granted !== "boolean") {
      return failure("invalid_argument", "记录访问授权无效", 400);
    }
    const admin = users.find((user) => user.id === body.admin_id);
    if (!admin || !isAdmin(admin) || admin.status !== "approved" || admin.id === ownerID) return failure("invalid_argument", "目标账号必须是已通过的其他管理员", 400);
    const grants = recordGrants.get(ownerID) ?? [];
    const capability = body.capability as RecordCapability;
    let grant = grants.find((item) => item.admin_id === admin.id && item.capability === capability);
    if (grant) {
      grant.granted = body.granted;
      grant.granted_by = actor.id;
      grant.version++;
      grant.updated_at = now();
    } else {
      grant = { owner_id: ownerID, admin_id: admin.id, capability, granted: body.granted, granted_by: actor.id, version: 1, updated_at: now() };
      grants.push(grant);
      recordGrants.set(ownerID, grants);
    }
    recordMockAudit(actor, "record.grant.update", ownerID, { admin_id: admin.id, capability, granted: grant.granted, version: grant.version });
    return json(grant as unknown as Record<string, unknown>);
  }),
  http.get("/api/admin/users/:userId/records", ({ params, request, cookies }) => {
    const actor = sessionUser(cookies)!;
    if (!isAdmin(actor)) return failure("forbidden", "没有执行此操作的权限", 403);
    const ownerID = String(params.userId);
    if (!users.some((user) => user.id === ownerID)) return failure("not_found", "用户不存在", 404);
    if (!(recordGrants.get(ownerID) ?? []).some((item) => item.admin_id === actor.id && item.capability === "view" && item.granted)) return failure("forbidden", "没有查看记录的授权", 403);
    const query = new URL(request.url).searchParams;
    const purposes = query.getAll("purpose");
    const limits = query.getAll("limit");
    const beforeIDs = query.getAll("before_id");
    const limit = Number(limits[0] ?? 50);
    if (purposes.length > 1 || (purposes.length === 1 && !["full_record", "training_contribution"].includes(purposes[0]!)) ||
      limits.length > 1 || !Number.isInteger(limit) || limit < 1 || limit > 100 || beforeIDs.length > 1 ||
      (beforeIDs[0] && !/^r[a-zA-Z0-9]{6,63}$/u.test(beforeIDs[0]))) return failure("invalid_argument", "记录分页参数无效", 400);
    recordMockAudit(actor, "record.list", ownerID, { purpose: purposes[0] ?? "all", limit });
    return json({ items: [], next_before: "" });
  }),
  http.get("/api/admin/users/:userId/records/:recordId/attachments", ({ params, cookies }) => {
    const actor = sessionUser(cookies)!;
    if (!isAdmin(actor)) return failure("forbidden", "没有执行此操作的权限", 403);
    const ownerID = String(params.userId);
    if (!users.some((user) => user.id === ownerID)) return failure("not_found", "用户不存在", 404);
    if (!(recordGrants.get(ownerID) ?? []).some((item) => item.admin_id === actor.id && item.capability === "attachment" && item.granted)) return failure("forbidden", "没有查看附件的授权", 403);
    return json({ items: [] });
  }),
  http.get("/api/admin/users/:userId/records/:recordId/attachments/:attachmentId", ({ params, cookies }) => {
    const actor = sessionUser(cookies)!;
    if (!isAdmin(actor)) return failure("forbidden", "没有执行此操作的权限", 403);
    const ownerID = String(params.userId);
    if (!(recordGrants.get(ownerID) ?? []).some((item) => item.admin_id === actor.id && item.capability === "attachment" && item.granted)) return failure("forbidden", "没有查看附件的授权", 403);
    return failure("not_found", "附件不存在", 404);
  }),
  http.get("/api/admin/users/:userId/records/:recordId/export", ({ params, cookies }) => {
    const actor = sessionUser(cookies)!;
    if (!isAdmin(actor)) return failure("forbidden", "没有执行此操作的权限", 403);
    const ownerID = String(params.userId);
    if (!(recordGrants.get(ownerID) ?? []).some((item) => item.admin_id === actor.id && item.capability === "export" && item.granted)) return failure("forbidden", "没有导出记录的授权", 403);
    return failure("not_found", "记录不存在", 404);
  }),
  http.get("/api/admin/users/:userId/records/:recordId", ({ params, cookies }) => {
    const actor = sessionUser(cookies)!;
    if (!isAdmin(actor)) return failure("forbidden", "没有执行此操作的权限", 403);
    const ownerID = String(params.userId);
    if (!(recordGrants.get(ownerID) ?? []).some((item) => item.admin_id === actor.id && item.capability === "view" && item.granted)) return failure("forbidden", "没有查看记录的授权", 403);
    return failure("not_found", "记录不存在", 404);
  }),
  http.delete("/api/admin/users/:userId/records/:recordId", ({ params, cookies }) => {
    const actor = sessionUser(cookies)!;
    if (!isAdmin(actor)) return failure("forbidden", "没有执行此操作的权限", 403);
    const ownerID = String(params.userId);
    if (!(recordGrants.get(ownerID) ?? []).some((item) => item.admin_id === actor.id && item.capability === "delete" && item.granted)) return failure("forbidden", "没有删除记录的授权", 403);
    return failure("not_found", "记录不存在", 404);
  }),
  http.get("/api/admin/model-usage/unknown", ({ cookies }) => {
    if (!isAdmin(sessionUser(cookies)!)) return failure("forbidden", "没有执行此操作的权限", 403);
    return json({ items: [] });
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
    const firstApproval = nextStatus === "approved" && (user.status === "pending" || user.status === "rejected");
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
    if (firstApproval && defaultModelBudget.configured && !modelBudgets.has(user.id)) modelBudgets.set(user.id, {
      owner_id: user.id,
      configured: true,
      token_limit: defaultModelBudget.token_limit,
      max_concurrent: defaultModelBudget.max_concurrent,
      used_tokens: 0,
      in_flight: 0,
      has_unknown: false,
    });
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
