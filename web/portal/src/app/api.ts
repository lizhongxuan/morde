import { api, requestFetch, unwrapApiResponse } from '../api/client';
import { errorMessage } from '../api/errors';
import type { components, operations } from '../api/schema';

type AuditQuery = NonNullable<operations['listAuditLogs']['parameters']['query']>;

export type HostMemberPermission = components['schemas']['HostMemberPermission'];
export type HostMember = components['schemas']['HostMember'];
export type SharedHost = components['schemas']['SharedHost'];
export type MainConversation = {
  status: 'ready' | 'uninitialized' | 'selection_required';
  conversation_id: string | null;
  version: number;
};
export type ConversationDeliveryStatus = components['schemas']['ConversationMessage']['delivery_status'];
export type TaskWorkbenchObservationTicket = components['schemas']['TaskWorkbenchObservationTicket'];

async function readMainConversation(response: Response): Promise<MainConversation> {
  if (!response.ok) await throwResponseError(response);
  const value: unknown = await response.json();
  if (!value || typeof value !== 'object' || !('status' in value) || !('conversation_id' in value) || !('version' in value)
    || !['ready', 'uninitialized', 'selection_required'].includes(String(value.status))
    || !(value.conversation_id === null || typeof value.conversation_id === 'string')
    || typeof value.version !== 'number' || !Number.isSafeInteger(value.version) || value.version < 0
    || (value.status === 'ready' && (typeof value.conversation_id !== 'string' || !value.conversation_id))
    || (value.status !== 'ready' && value.conversation_id !== null && value.conversation_id !== '')) {
    throw new Error('主会话响应格式无效');
  }
  return { ...value, conversation_id: value.conversation_id || null } as MainConversation;
}

async function readTaskWorkbenchObservationTicket(response: Response, taskID: string): Promise<TaskWorkbenchObservationTicket> {
  if (!response.ok) await throwResponseError(response);
  const value: unknown = await response.json();
  if (!value || typeof value !== 'object' || !('ticket' in value) || !('task_id' in value) || !('host_id' in value) || !('expires_at' in value)
    || typeof value.ticket !== 'string' || value.ticket.length < 16 || value.ticket.length > 4096
    || typeof value.task_id !== 'string' || value.task_id !== taskID
    || typeof value.host_id !== 'string' || !/^h[a-z0-9]{7}$/.test(value.host_id)
    || typeof value.expires_at !== 'string' || !Number.isFinite(Date.parse(value.expires_at)) || Date.parse(value.expires_at) <= Date.now()) {
    throw new Error('Task 观察票据响应无效');
  }
  return { ticket: value.ticket, task_id: value.task_id, host_id: value.host_id, expires_at: value.expires_at };
}

export const portalApi = {
  me: async () => unwrapApiResponse(await api.GET('/api/me')),
  preferences: async () => unwrapApiResponse(await api.GET('/api/me/preferences')),
  savePreferences: async (body: components['schemas']['SaveUserPreferencesRequest']) => unwrapApiResponse(await api.PUT('/api/me/preferences', { body })),
  execution: async () => unwrapApiResponse(await api.GET('/api/execution')),
  enableExecution: async (epoch: number, idempotencyKey: string) => unwrapApiResponse(await api.POST('/api/execution/enable', {
    params: { header: { 'Idempotency-Key': idempotencyKey } }, body: { expected_epoch: epoch },
  })),
  onboardingStatus: async () => unwrapApiResponse(await api.GET('/api/onboarding/status')),
  availableModels: async () => unwrapApiResponse(await api.GET('/api/available-models')),
  ensureDefaultHost: async (body: components['schemas']['EnsureDefaultHostRequest'] = {}) => unwrapApiResponse(await api.POST('/api/me/default-host', { body })),
  login: async (name: string, password: string) => unwrapApiResponse(await api.POST('/api/auth/login', { body: { name, password } })),
  register: async (name: string, password: string) => unwrapApiResponse(await api.POST('/api/auth/register', { body: { name, password } })),
  logout: async () => unwrapApiResponse(await api.POST('/api/auth/logout')),
  changePassword: async (body: components['schemas']['ChangePasswordRequest']) => unwrapApiResponse(await api.POST('/api/me/password', { body })),
  hosts: async () => unwrapApiResponse(await api.GET('/api/hosts')),
  sharedHosts: async () => unwrapApiResponse(await api.GET('/api/shared-hosts')),
  hostMembers: async (id: string) => unwrapApiResponse(await api.GET('/api/hosts/{id}/members', { params: { path: { id } } })),
  inviteHostMember: async (id: string, body: components['schemas']['HostMemberInviteRequest']) => unwrapApiResponse(await api.POST('/api/hosts/{id}/members', { params: { path: { id } }, body })),
  revokeHostMember: async (id: string, userID: string) => {
    unwrapApiResponse(await api.DELETE('/api/hosts/{id}/members/{user_id}', { params: { path: { id, user_id: userID } } }));
  },
  issueObservationTicket: async (id: string, target: 'desktop' | 'terminal' | 'workspace' = 'desktop') => unwrapApiResponse(await api.POST('/api/hosts/{id}/observation-ticket', {
    params: { path: { id }, ...(target !== 'desktop' ? { query: { target } } : {}) },
  })),
  host: async (id: string) => unwrapApiResponse(await api.GET('/api/hosts/{id}', { params: { path: { id } } })),
  templates: async () => unwrapApiResponse(await api.GET('/api/templates')),
  createHost: async (body: components['schemas']['CreateHostRequest'], idempotencyKey = newIdempotencyKey()) => unwrapApiResponse(await api.POST('/api/hosts', {
    params: { header: { 'Idempotency-Key': idempotencyKey } }, body,
  })),
  enterHost: async (id: string) => unwrapApiResponse(await api.POST('/api/hosts/{id}/enter', { params: { path: { id } } })),
  hostControl: async (id: string) => unwrapApiResponse(await api.GET('/api/hosts/{id}/control', { params: { path: { id } } })),
  hostControlAction: async (id: string, body: components['schemas']['ControlActionRequest']) => unwrapApiResponse(await api.POST('/api/hosts/{id}/control/actions', { params: { path: { id } }, body })),
  renewHostControlLease: async (id: string, expectedControlVersion: number) => unwrapApiResponse(await api.POST('/api/hosts/{id}/control/lease/renew', {
    params: { path: { id } }, body: { expected_control_version: expectedControlVersion },
  })),
  renewHost: async (id: string) => unwrapApiResponse(await api.POST('/api/hosts/{id}/renew', { params: { path: { id } } })),
  releaseHost: async (id: string, confirmProcessTermination: boolean) => unwrapApiResponse(await api.POST('/api/hosts/{id}/release', { params: { path: { id } }, body: { confirm_process_termination: confirmProcessTermination } })),
  restoreHost: async (id: string) => unwrapApiResponse(await api.POST('/api/hosts/{id}/restore', { params: { path: { id } } })),
  deleteHost: async (id: string) => unwrapApiResponse(await api.DELETE('/api/hosts/{id}', { params: { path: { id } } })),
  workspaces: async () => unwrapApiResponse(await api.GET('/api/workspaces')),
  sites: async (cursor?: string) => unwrapApiResponse(await api.GET('/api/sites', {
    params: { query: { limit: 100, ...(cursor ? { cursor } : {}) } },
  })),
  siteReleases: async (id: string, cursor?: string) => unwrapApiResponse(await api.GET('/api/sites/{id}/releases', {
    params: { path: { id }, query: { limit: 100, ...(cursor ? { cursor } : {}) } },
  })),
  createSite: async (body: components['schemas']['CreateSiteRequest'], idempotencyKey: string) => unwrapApiResponse(await api.POST('/api/sites', {
    params: { header: { 'Idempotency-Key': idempotencyKey } }, body,
  })),
  createSiteCandidate: async (id: string, body: components['schemas']['CreateSiteCandidateRequest'], idempotencyKey: string) => unwrapApiResponse(await api.POST('/api/sites/{id}/releases', {
    params: { path: { id }, header: { 'Idempotency-Key': idempotencyKey } }, body,
  })),
  materializeSiteRelease: async (id: string, releaseId: string) => unwrapApiResponse(await api.POST('/api/sites/{id}/releases/{release_id}/materialize', {
    params: { path: { id, release_id: releaseId } },
  })),
  createWorkspace: async (body: components['schemas']['CreateWorkspaceRequest'], idempotencyKey = newIdempotencyKey()) => unwrapApiResponse(await api.POST('/api/workspaces', {
    params: { header: { 'Idempotency-Key': idempotencyKey } }, body,
  })),
  bindWorkspaceToHost: async (id: string, body: components['schemas']['BindWorkspaceRequest'], idempotencyKey = newIdempotencyKey()) => unwrapApiResponse(await api.POST('/api/hosts/{id}/workspace-bindings', {
    params: { path: { id }, header: { 'Idempotency-Key': idempotencyKey } }, body,
  })),
  retryHostWorkspaceSync: async (hostID: string, workspaceID: string, expectedBindingVersion: number) => {
    const response = await requestFetch(`/api/hosts/${encodeURIComponent(hostID)}/workspace-bindings/${encodeURIComponent(workspaceID)}/retry-sync`, {
      method: 'POST', body: JSON.stringify({ expected_binding_version: expectedBindingVersion }),
    });
    if (!response.ok) await throwResponseError(response);
  },
  unbindWorkspaceFromHost: async (id: string, workspaceID: string, body: operations['unbindWorkspaceFromHost']['requestBody']['content']['application/json'], idempotencyKey = newIdempotencyKey()) => unwrapApiResponse(await api.DELETE('/api/hosts/{id}/workspace-bindings/{workspace_id}', {
    params: { path: { id, workspace_id: workspaceID }, header: { 'Idempotency-Key': idempotencyKey } }, body,
  })),
  deleteWorkspace: async (id: string) => unwrapApiResponse(await api.DELETE('/api/workspaces/{id}', { params: { path: { id } } })),
  workspaceManifest: async (id: string, query: NonNullable<operations['getWorkspaceManifest']['parameters']['query']> = {}) =>
    unwrapApiResponse(await api.GET('/api/workspaces/{id}/manifest', { params: { path: { id }, query } })),
  workspaceDiff: async (id: string, fromRevision: number, toRevision: number) => unwrapApiResponse(await api.GET('/api/workspaces/{id}/diff', {
    params: { path: { id }, query: { from_revision: fromRevision, to_revision: toRevision } },
  })),
  restoreWorkspacePaths: async (id: string, body: components['schemas']['WorkspaceRestoreRequest'], idempotencyKey: string) => unwrapApiResponse(await api.POST('/api/workspaces/{id}/restore', {
    params: { path: { id }, header: { 'Idempotency-Key': idempotencyKey } }, body,
  })),
  workspaceChanges: async (id: string, after: string, options: Partial<Omit<NonNullable<operations['getWorkspaceChanges']['parameters']['query']>, 'after'>> = {}) =>
    unwrapApiResponse(await api.GET('/api/workspaces/{id}/changes', { params: { path: { id }, query: { after, ...options } } })),
  createWorkspaceSyncSession: async (id: string, body: components['schemas']['CreateSyncSessionRequest']) => unwrapApiResponse(await api.POST('/api/workspaces/{id}/sync-sessions', {
    params: { path: { id }, header: { 'Idempotency-Key': newIdempotencyKey() } }, body,
  })),
  workspaceSyncSession: async (id: string, sessionId: string) => unwrapApiResponse(await api.GET('/api/workspaces/{id}/sync-sessions/{session_id}', {
    params: { path: { id, session_id: sessionId } },
  })),
  workspaceDeviceSyncProgress: async (id: string) => unwrapApiResponse(await api.GET('/api/workspaces/{id}/sync-progress', {
    params: { path: { id } },
  })),
  commitWorkspaceSyncSession: async (id: string, sessionId: string) => unwrapApiResponse(await api.POST('/api/workspaces/{id}/sync-sessions/{session_id}/commit', {
    params: { path: { id, session_id: sessionId }, header: { 'Idempotency-Key': newIdempotencyKey() } },
  })),
  cancelWorkspaceSyncSession: async (id: string, sessionId: string) => unwrapApiResponse(await api.POST('/api/workspaces/{id}/sync-sessions/{session_id}/cancel', {
    params: { path: { id, session_id: sessionId }, header: { 'Idempotency-Key': newIdempotencyKey() } },
  })),
  putWorkspaceSyncBlock: async (id: string, sessionId: string, digest: string, block: BodyInit) => {
    const path = syncBlockPath(id, sessionId, digest);
    const response = await requestFetch(path, {
      method: 'PUT', headers: { 'Content-Type': 'application/octet-stream' }, body: block,
    });
    if (!response.ok) await throwResponseError(response);
  },
  getWorkspaceSyncBlock: async (id: string, sessionId: string, digest: string): Promise<Blob> => {
    const response = await requestFetch(syncBlockPath(id, sessionId, digest));
    if (!response.ok) await throwResponseError(response);
    return response.blob();
  },
  ackWorkspaceSyncBlock: async (id: string, sessionId: string, digest: string) => {
    unwrapApiResponse(await api.POST('/api/workspaces/{id}/sync-sessions/{session_id}/blocks/{digest}/ack', {
      params: { path: { id, session_id: sessionId, digest } },
    }));
  },
  clients: async () => unwrapApiResponse(await api.GET('/api/clients')),
  revokeClient: async (id: string) => unwrapApiResponse(await api.DELETE('/api/clients/{id}', { params: { path: { id } } })),
  capabilities: async () => unwrapApiResponse(await api.GET('/api/capabilities')),
  conversations: async (cursor?: string, limit = 100) => unwrapApiResponse(await api.GET('/api/conversations', { params: { query: { limit, ...(cursor ? { cursor } : {}) } } })),
  createConversation: async (body: components['schemas']['CreateConversationRequest'], idempotencyKey: string, serviceInstanceID: string, actorID: string) => {
    if (!serviceInstanceID || !actorID) throw new Error('服务或账号身份尚未确认。');
    return unwrapApiResponse(await api.POST('/api/conversations', {
      params: { header: { 'Idempotency-Key': idempotencyKey, 'X-Morde-Service-Instance-ID': serviceInstanceID, 'X-Morde-Actor-ID': actorID } }, body,
    }));
  },
  conversation: async (id: string) => unwrapApiResponse(await api.GET('/api/conversations/{id}', { params: { path: { id } } })),
  mainConversation: async (): Promise<MainConversation> => {
    const response = await requestFetch('/api/me/main-conversation');
    return readMainConversation(response);
  },
  ensureMainConversation: async (idempotencyKey: string): Promise<MainConversation> => {
    const response = await requestFetch('/api/me/main-conversation/ensure', {
      method: 'POST', headers: { 'Idempotency-Key': idempotencyKey },
    });
    return readMainConversation(response);
  },
  setMainConversation: async (conversationID: string, expectedVersion: number, idempotencyKey: string): Promise<MainConversation> => {
    const response = await requestFetch('/api/me/main-conversation', {
      method: 'PUT', headers: { 'Idempotency-Key': idempotencyKey },
      body: JSON.stringify({ conversation_id: conversationID, expected_version: expectedVersion }),
    });
    return readMainConversation(response);
  },
  conversationMessages: async (id: string, cursor?: string, clientMessageID?: string, serviceInstanceID?: string, actorID?: string) => {
    if (clientMessageID && (!serviceInstanceID || !actorID)) throw new Error('服务或账号身份尚未确认。');
    return unwrapApiResponse(await api.GET('/api/conversations/{id}/messages', {
      params: { path: { id }, query: { limit: 100, ...(cursor ? { cursor } : {}), ...(clientMessageID ? { client_message_id: clientMessageID } : {}) },
        ...(serviceInstanceID || actorID ? { header: { ...(serviceInstanceID ? { 'X-Morde-Service-Instance-ID': serviceInstanceID } : {}),
          ...(actorID ? { 'X-Morde-Actor-ID': actorID } : {}) } } : {}) },
    }));
  },
  conversationTasks: async (id: string): Promise<components['schemas']['ConversationActiveTaskPage']> => {
    const items: components['schemas']['ConversationActiveTask'][] = [];
    const seen = new Set<string>();
    let cursor: string | undefined;
    for (let page = 0; page < 1000; page += 1) {
      const result = unwrapApiResponse(await api.GET('/api/conversations/{id}/tasks', {
        params: { path: { id }, query: { limit: 100, ...(cursor ? { cursor } : {}) } },
      }));
      items.push(...result.items);
      if (!result.next_cursor) return { items, next_cursor: null };
      if (seen.has(result.next_cursor)) throw new Error('任务分页游标重复，无法继续读取');
      seen.add(result.next_cursor);
      cursor = result.next_cursor;
    }
    throw new Error('任务页数超过读取上限');
  },
  tasks: async (): Promise<components['schemas']['TaskPage']> => {
    const items: components['schemas']['TaskSummary'][] = [];
    const seen = new Set<string>();
    let cursor: string | undefined;
    for (let page = 0; page < 1000; page += 1) {
      const result = unwrapApiResponse(await api.GET('/api/tasks', { params: { query: { limit: 100, ...(cursor ? { cursor } : {}) } } }));
      items.push(...result.items);
      if (!result.next_cursor) return { items, next_cursor: null };
      if (seen.has(result.next_cursor)) throw new Error('任务分页游标重复，无法继续读取');
      seen.add(result.next_cursor);
      cursor = result.next_cursor;
    }
    throw new Error('任务页数超过读取上限');
  },
  task: async (id: string) => unwrapApiResponse(await api.GET('/api/tasks/{id}', { params: { path: { id } } })),
  taskFileFetches: async (id: string) => unwrapApiResponse(await api.GET('/api/tasks/{id}/file-fetches', { params: { path: { id } } })),
  deleteTaskFileFetchContent: async (id: string, fetchID: string): Promise<void> => {
    unwrapApiResponse(await api.DELETE('/api/tasks/{id}/file-fetches/{fetch_id}/content', { params: { path: { id, fetch_id: fetchID } } }));
  },
  taskWorkbenchObservationTicket: async (id: string): Promise<TaskWorkbenchObservationTicket> => readTaskWorkbenchObservationTicket(
    await requestFetch(`/api/tasks/${encodeURIComponent(id)}/workbench-observation-ticket`, { method: 'POST' }), id,
  ),
  taskAction: async (id: string, body: components['schemas']['TaskActionRequest'], idempotencyKey = newIdempotencyKey()) => unwrapApiResponse(await api.POST('/api/tasks/{id}/actions', {
    params: { path: { id }, header: { 'Idempotency-Key': idempotencyKey } }, body,
  })),
  submitTask: async (body: components['schemas']['SubmitTaskRequest'], idempotencyKey = newIdempotencyKey()) => unwrapApiResponse(await api.POST('/api/tasks', {
    params: { header: { 'Idempotency-Key': idempotencyKey } }, body,
  })),
  acceptConversationMessage: async (id: string, body: components['schemas']['AcceptConversationMessageRequest'], idempotencyKey: string, serviceInstanceID: string, actorID: string) => {
    if (idempotencyKey !== body.client_message_id) throw new Error('消息编号与请求编号不一致。');
    if (!serviceInstanceID || !actorID) throw new Error('服务或账号身份尚未确认。');
    const result = await api.POST('/api/conversations/{id}/messages', {
      params: { path: { id }, header: { 'Idempotency-Key': idempotencyKey, 'X-Morde-Service-Instance-ID': serviceInstanceID, 'X-Morde-Actor-ID': actorID } }, body,
    });
    const submission = unwrapApiResponse(result);
    const { message } = submission;
    if (result.response.status !== 202 || message.role !== 'user' || message.conversation_id !== id ||
      message.client_message_id !== body.client_message_id || message.content !== body.content) {
      throw new Error('服务端未确认消息已接收，请核对后重试。');
    }
    return submission;
  },
  associateConversationAttachment: async (conversationID: string, messageID: string, clientAttachmentID: string, attachmentID: string, serviceInstanceID: string, actorID: string) => {
    if (!serviceInstanceID || !actorID) throw new Error('服务或账号身份尚未确认。');
    unwrapApiResponse(await api.PUT('/api/conversations/{id}/messages/{message_id}/attachments/{client_attachment_id}', {
      params: { path: { id: conversationID, message_id: messageID, client_attachment_id: clientAttachmentID },
        header: { 'X-Morde-Service-Instance-ID': serviceInstanceID, 'X-Morde-Actor-ID': actorID } }, body: { attachment_id: attachmentID },
    }));
  },
  retryConversationMessageProcessing: async (conversationID: string, messageID: string, idempotencyKey: string, serviceInstanceID: string, actorID: string) => {
    const result = await api.POST('/api/conversations/{id}/messages/{message_id}/retry-processing', {
      parseAs: 'text',
      params: { path: { id: conversationID, message_id: messageID },
        header: { 'Idempotency-Key': idempotencyKey, 'X-Morde-Service-Instance-ID': serviceInstanceID, 'X-Morde-Actor-ID': actorID } },
    });
    if (result.error !== undefined) unwrapApiResponse(result);
    if (result.response.status !== 202) throw new Error('服务端未确认消息重试。');
  },
  answerTaskInput: async (id: string, body: components['schemas']['TaskInputRequest'], idempotencyKey = newIdempotencyKey()) => unwrapApiResponse(await api.POST('/api/tasks/{id}/inputs', {
    params: { path: { id }, header: { 'Idempotency-Key': idempotencyKey } }, body,
  })),
  respondToTaskApproval: async (id: string, approvalID: string, body: components['schemas']['TaskApprovalResponseRequest'], idempotencyKey = newIdempotencyKey()) => unwrapApiResponse(await api.POST('/api/tasks/{id}/approvals/{approval_id}/responses', {
    params: { path: { id, approval_id: approvalID }, header: { 'Idempotency-Key': idempotencyKey } }, body,
  })),
  notifications: async (cursor?: string, unreadOnly = false) => unwrapApiResponse(await api.GET('/api/notifications', { params: { query: { limit: 100, unread_only: unreadOnly, ...(cursor ? { cursor } : {}) } } })),
  markNotificationRead: async (id: string) => unwrapApiResponse(await api.POST('/api/notifications/{id}/read', { params: { path: { id } } })),
  apiKeys: async () => unwrapApiResponse(await api.GET('/api/apikeys')),
  createApiKey: async (body: components['schemas']['CreateAPIKeyRequest']) => unwrapApiResponse(await api.POST('/api/apikeys', { body })),
  deleteApiKey: async (id: string) => unwrapApiResponse(await api.DELETE('/api/apikeys/{id}', { params: { path: { id } } })),
  personalDataSnapshot: async (cursor?: string, limit = 100) => unwrapApiResponse(await api.GET('/api/sync/snapshot', {
    params: { query: { limit, ...(cursor ? { cursor } : {}) } },
  })),
  personalDataChanges: async (afterSequence: number, throughSequence?: number, limit = 100) => unwrapApiResponse(await api.GET('/api/sync/changes', {
    params: { query: { after_sequence: afterSequence, limit, ...(throughSequence === undefined ? {} : { through_sequence: throughSequence }) } },
  })),
  getPersonalObject: async (type: components['schemas']['PersonalMutationObjectType'], id: string) => unwrapApiResponse(await api.GET('/api/personal-data/{type}/{id}', { params: { path: { type, id } } })),
  replacePersonalObject: async (
    type: components['schemas']['PersonalMutationObjectType'], id: string, baseRevision: number,
    body: components['schemas']['PersonalObjectPutRequest']['body'], idempotencyKey = newIdempotencyKey(), actorID?: string, serviceInstanceID?: string,
  ) => unwrapApiResponse(await api.PUT('/api/personal-data/{type}/{id}', {
    params: { path: { type, id }, header: { 'Idempotency-Key': idempotencyKey } }, body: { base_revision: baseRevision, body },
    headers: { ...(actorID ? { 'X-Morde-Actor-ID': actorID } : {}), ...(serviceInstanceID ? { 'X-Morde-Service-Instance-ID': serviceInstanceID } : {}) },
  })),
  deletePersonalObject: async (
    type: components['schemas']['PersonalMutationObjectType'], id: string, baseRevision: number,
    idempotencyKey = newIdempotencyKey(),
  ) => unwrapApiResponse(await api.DELETE('/api/personal-data/{type}/{id}', {
    params: { path: { type, id }, header: { 'Idempotency-Key': idempotencyKey } }, body: { base_revision: baseRevision },
  })),
  personalDataQuota: async () => unwrapApiResponse(await api.GET('/api/personal-data/quota')),
  createPersonalAttachmentUpload: async (
    body: components['schemas']['PersonalAttachmentUploadRequest'], idempotencyKey = newIdempotencyKey(), serviceInstanceID?: string, actorID?: string,
  ) => unwrapApiResponse(await api.POST('/api/personal-data/uploads', {
    params: { header: { 'Idempotency-Key': idempotencyKey } }, body,
    headers: { ...(serviceInstanceID ? { 'X-Morde-Service-Instance-ID': serviceInstanceID } : {}), ...(actorID ? { 'X-Morde-Actor-ID': actorID } : {}) },
  })),
  putPersonalAttachmentChunk: async (uploadID: string, sequence: number, body: BodyInit, sha256: string, serviceInstanceID?: string, actorID?: string) => {
    const response = await requestFetch(`/api/personal-data/uploads/${encodeURIComponent(uploadID)}/chunks/${sequence}`, {
      method: 'PUT', headers: { 'Content-Type': 'application/octet-stream', 'X-Chunk-SHA256': sha256,
        ...(serviceInstanceID ? { 'X-Morde-Service-Instance-ID': serviceInstanceID } : {}), ...(actorID ? { 'X-Morde-Actor-ID': actorID } : {}) }, body,
    });
    if (!response.ok) await throwResponseError(response);
    return unwrapApiResponse(await response.json().then((data) => ({ data, response })));
  },
  commitPersonalAttachment: async (uploadID: string, idempotencyKey = newIdempotencyKey(), serviceInstanceID?: string, actorID?: string) => unwrapApiResponse(await api.POST('/api/personal-data/uploads/{upload_id}/commit', {
    params: { path: { upload_id: uploadID }, header: { 'Idempotency-Key': idempotencyKey } },
    headers: { ...(serviceInstanceID ? { 'X-Morde-Service-Instance-ID': serviceInstanceID } : {}), ...(actorID ? { 'X-Morde-Actor-ID': actorID } : {}) },
  })),
  cancelPersonalAttachmentUpload: async (uploadID: string) => unwrapApiResponse(await api.DELETE('/api/personal-data/uploads/{upload_id}', { params: { path: { upload_id: uploadID } } })),
  personalAttachment: async (attachmentID: string): Promise<Blob> => {
    const response = await requestFetch(`/api/personal-data/attachments/${encodeURIComponent(attachmentID)}`);
    if (!response.ok) await throwResponseError(response);
    return response.blob();
  },
  previewPersonalOfficeText: async (file: Blob, mediaType: string): Promise<string> => {
    const supported = new Set([
      'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
      'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
      'application/vnd.openxmlformats-officedocument.presentationml.presentation',
    ]);
    if (!supported.has(mediaType)) throw new Error('仅支持预览 DOCX、XLSX 或 PPTX 文档。');
    const response = await requestFetch('/api/personal-data/office-preview', {
      method: 'POST', headers: { 'Content-Type': mediaType }, body: file,
    });
    if (!response.ok) await throwResponseError(response);
    const value: unknown = await response.json();
    if (!value || typeof value !== 'object' || !('text' in value) || typeof value.text !== 'string') {
      throw new Error('Office 文本预览响应格式无效。');
    }
    return value.text;
  },
  goals: async (cursor?: string) => unwrapApiResponse(await api.GET('/api/goals', {
    params: { query: { limit: 100, ...(cursor ? { cursor } : {}) } },
  })),
  createGoal: async (body: components['schemas']['GoalCreateRequest'], idempotencyKey = newIdempotencyKey()) => unwrapApiResponse(await api.POST('/api/goals', {
    params: { header: { 'Idempotency-Key': idempotencyKey } }, body,
  })),
  getGoal: async (id: string) => unwrapApiResponse(await api.GET('/api/goals/{id}', { params: { path: { id } } })),
  updateGoal: async (id: string, body: components['schemas']['GoalMutationRequest'], idempotencyKey = newIdempotencyKey()) => unwrapApiResponse(await api.PATCH('/api/goals/{id}', {
    params: { path: { id }, header: { 'Idempotency-Key': idempotencyKey } }, body,
  })),
  deleteGoal: async (id: string, body: components['schemas']['GoalDeleteRequest'], idempotencyKey = newIdempotencyKey()) => unwrapApiResponse(await api.DELETE('/api/goals/{id}', {
    params: { path: { id }, header: { 'Idempotency-Key': idempotencyKey } }, body,
  })),
  schedules: async (query: NonNullable<operations['listSchedules']['parameters']['query']> = {}) => unwrapApiResponse(await api.GET('/api/schedules', {
    params: { query: { limit: 100, ...query } },
  })),
  createSchedule: async (body: components['schemas']['ScheduleCreateRequest'], idempotencyKey = newIdempotencyKey()) => unwrapApiResponse(await api.POST('/api/schedules', {
    params: { header: { 'Idempotency-Key': idempotencyKey } }, body,
  })),
  getSchedule: async (id: string) => unwrapApiResponse(await api.GET('/api/schedules/{id}', { params: { path: { id } } })),
  updateSchedule: async (id: string, body: components['schemas']['ScheduleUpdateRequest'], idempotencyKey = newIdempotencyKey()) => unwrapApiResponse(await api.PUT('/api/schedules/{id}', {
    params: { path: { id }, header: { 'Idempotency-Key': idempotencyKey } }, body,
  })),
  scheduleAction: async (id: string, body: components['schemas']['ScheduleActionRequest'], idempotencyKey = newIdempotencyKey()) => unwrapApiResponse(await api.POST('/api/schedules/{id}/actions', {
    params: { path: { id }, header: { 'Idempotency-Key': idempotencyKey } }, body,
  })),
  scheduleRuns: async (id: string, query: NonNullable<operations['listScheduleRuns']['parameters']['query']> = {}) => unwrapApiResponse(await api.GET('/api/schedules/{id}/runs', {
    params: { path: { id }, query: { limit: 100, ...query } },
  })),
  modelConnections: async () => unwrapApiResponse(await api.GET('/api/model-connections')),
  createModelConnection: async (body: components['schemas']['CreateModelConnectionRequest']) => unwrapApiResponse(await api.POST('/api/model-connections', { body })),
  updateModelConnection: async (id: string, body: operations['updateModelConnection']['requestBody']['content']['application/json']) => unwrapApiResponse(await api.PUT('/api/model-connections/{id}', { params: { path: { id } }, body })),
  deleteModelConnection: async (id: string) => unwrapApiResponse(await api.DELETE('/api/model-connections/{id}', { params: { path: { id } } })),
  mcpConnectors: async () => unwrapApiResponse(await api.GET('/api/mcp/connectors')),
  createMCPConnector: async (body: components['schemas']['CreateMCPConnectorRequest']) => unwrapApiResponse(await api.POST('/api/mcp/connectors', { body })),
  beginMCPConnectorOAuth: async (id: string, body: components['schemas']['BeginMCPConnectorOAuthRequest']) => unwrapApiResponse(await api.POST('/api/mcp/connectors/{id}/oauth/authorize', { params: { path: { id } }, body })),
  updateMCPConnector: async (id: string, body: components['schemas']['UpdateMCPConnectorRequest']) => unwrapApiResponse(await api.PUT('/api/mcp/connectors/{id}', { params: { path: { id } }, body })),
  rotateMCPConnectorCredential: async (id: string, body: components['schemas']['RotateMCPConnectorCredentialRequest']) => unwrapApiResponse(await api.PUT('/api/mcp/connectors/{id}/credential', { params: { path: { id } }, body })),
  setMCPConnectorEnabled: async (id: string, body: operations['setMCPConnectorEnabled']['requestBody']['content']['application/json']) => unwrapApiResponse(await api.PUT('/api/mcp/connectors/{id}/enabled', { params: { path: { id } }, body })),
  deleteMCPConnector: async (id: string, expectedRevision: number) => unwrapApiResponse(await api.DELETE('/api/mcp/connectors/{id}', { params: { path: { id }, query: { expected_revision: expectedRevision } } })),
  checkModelConnection: async (id: string, idempotencyKey = newIdempotencyKey()) => {
    const text = unwrapApiResponse(await api.POST('/api/model-connections/{id}/check', {
      params: { path: { id }, header: { 'Idempotency-Key': idempotencyKey } }, body: { capability: 'text' },
    }));
    try {
      return unwrapApiResponse(await api.POST('/api/model-connections/{id}/check', {
        params: { path: { id }, header: { 'Idempotency-Key': newIdempotencyKey() } }, body: { capability: 'tools' },
      }));
    } catch {
      return text;
    }
  },
  checkModelTextStreaming: async (id: string, idempotencyKey = newIdempotencyKey()) => unwrapApiResponse(await api.POST('/api/model-connections/{id}/check', {
    params: { path: { id }, header: { 'Idempotency-Key': idempotencyKey } }, body: { capability: 'text_stream' },
  })),
  adminOverview: async () => unwrapApiResponse(await api.GET('/api/admin/overview')),
  adminModelConnections: async () => unwrapApiResponse(await api.GET('/api/admin/model-connections')),
  adminUsage: async (days: 7 | 30 | 90) => unwrapApiResponse(await api.GET('/api/admin/usage', { params: { query: { days } } })),
  adminQuality: async (days: 7 | 30 | 90 = 7) => unwrapApiResponse(await api.GET('/api/admin/quality', { params: { query: { days } } })),
  adminControls: async () => unwrapApiResponse(await api.GET('/api/admin/controls')),
  adminSettings: async () => unwrapApiResponse(await api.GET('/api/admin/settings')),
  updateAdminModelBudgetDefault: async (body: components['schemas']['UpdateAdminModelBudgetDefaultRequest']) => unwrapApiResponse(await api.PUT('/api/admin/settings/model-budget-default', { body })),
  updateAdminModelDefault: async (body: components['schemas']['UpdateAdminModelDefaultRequest']) => unwrapApiResponse(await api.PUT('/api/admin/settings/model-default', { body })),
  publicNudges: async () => unwrapApiResponse(await api.GET('/api/nudges')),
  createAdminControlRule: async (body: components['schemas']['CreateAdminControlRuleRequest']) => unwrapApiResponse(await api.POST('/api/admin/controls/rules', { body })),
  updateAdminControlRule: async (id: number, body: operations['updateAdminControlRule']['requestBody']['content']['application/json']) => unwrapApiResponse(await api.PUT('/api/admin/controls/rules/{id}', { params: { path: { id } }, body })),
  deleteAdminControlRule: async (id: number, expectedVersion: number) => unwrapApiResponse(await api.DELETE('/api/admin/controls/rules/{id}', { params: { path: { id }, query: { expected_version: expectedVersion } } })),
  evaluateAdminControls: async () => unwrapApiResponse(await api.POST('/api/admin/controls/evaluate')),
  adminControlNotifyTest: async () => unwrapApiResponse(await api.POST('/api/admin/controls/notify-test')),
  recordConsents: async () => unwrapApiResponse(await api.GET('/api/me/record-consents')),
  setRecordConsent: async (body: components['schemas']['SetRecordConsentRequest']) => unwrapApiResponse(await api.PUT('/api/me/record-consents', { body })),
  myRecordCaptureStatuses: async (query: NonNullable<operations['listMyRecordCaptureStatuses']['parameters']['query']> = {}) => unwrapApiResponse(await api.GET('/api/me/record-captures', { params: { query: { limit: 50, ...query } } })),
  myRecords: async (query: NonNullable<operations['listMyRecords']['parameters']['query']> = {}) => unwrapApiResponse(await api.GET('/api/me/records', { params: { query: { limit: 50, ...query } } })),
  deleteMyRecord: async (recordID: string) => unwrapApiResponse(await api.DELETE('/api/me/records/{record_id}', { params: { path: { record_id: recordID } } })),
  adminRecordSettings: async (id: string) => unwrapApiResponse(await api.GET('/api/admin/users/{id}/record-settings', { params: { path: { id } } })),
  adminRecordAccess: async (id: string) => unwrapApiResponse(await api.GET('/api/admin/users/{id}/record-access', { params: { path: { id } } })),
  updateRecordQuota: async (id: string, body: components['schemas']['SetAdminRecordQuotaRequest']) => unwrapApiResponse(await api.PUT('/api/admin/users/{id}/record-quota', { params: { path: { id } }, body })),
  updateRecordGrant: async (id: string, body: components['schemas']['SetAdminRecordGrantRequest']) => unwrapApiResponse(await api.PUT('/api/admin/users/{id}/record-grants', { params: { path: { id } }, body })),
  adminRecords: async (id: string, query: NonNullable<operations['listAdminUserRecords']['parameters']['query']> = {}) => unwrapApiResponse(await api.GET('/api/admin/users/{id}/records', { params: { path: { id }, query } })),
  adminRecord: async (id: string, recordID: string) => unwrapApiResponse(await api.GET('/api/admin/users/{id}/records/{record_id}', { params: { path: { id, record_id: recordID } } })),
  adminRecordAttachments: async (id: string, recordID: string) => unwrapApiResponse(await api.GET('/api/admin/users/{id}/records/{record_id}/attachments', { params: { path: { id, record_id: recordID } } })),
  adminRecordAttachment: async (id: string, recordID: string, attachmentID: string): Promise<Blob> => {
    const path = `/api/admin/users/${encodeURIComponent(id)}/records/${encodeURIComponent(recordID)}/attachments/${encodeURIComponent(attachmentID)}`;
    const response = await requestFetch(path);
    if (!response.ok) await throwResponseError(response);
    return response.blob();
  },
  adminRecordExport: async (id: string, recordID: string): Promise<Blob> => {
    const path = `/api/admin/users/${encodeURIComponent(id)}/records/${encodeURIComponent(recordID)}/export`;
    const response = await requestFetch(path);
    if (!response.ok) await throwResponseError(response);
    return response.blob();
  },
  deleteAdminRecord: async (id: string, recordID: string) => unwrapApiResponse(await api.DELETE('/api/admin/users/{id}/records/{record_id}', { params: { path: { id, record_id: recordID } } })),
  adminStats: async () => unwrapApiResponse(await api.GET('/api/admin/stats')),
  adminHosts: async () => unwrapApiResponse(await api.GET('/api/admin/hosts')),
  adminHostResources: async (id: string) => unwrapApiResponse(await api.GET('/api/admin/hosts/{id}/resources', { params: { path: { id } } })),
  updateAdminHostResources: async (id: string, body: components['schemas']['UpdateAdminHostResourcesRequest']) => unwrapApiResponse(await api.PATCH('/api/admin/hosts/{id}/resources', { params: { path: { id } }, body })),
  adminRecoverHost: async (id: string, body: components['schemas']['AdminHostRecoveryRequest']) => unwrapApiResponse(await api.POST('/api/admin/hosts/{id}/recovery', { params: { path: { id } }, body })),
  adminWorkspaces: async (cursor?: string) => unwrapApiResponse(await api.GET('/api/admin/workspaces', {
    params: { query: { limit: 50, ...(cursor ? { cursor } : {}) } },
  })),
  adminWorkspace: async (id: string) => unwrapApiResponse(await api.GET('/api/admin/workspaces/{id}', { params: { path: { id } } })),
  adminUsers: async (status?: components['schemas']['AccountStatus'], offset = 0) => unwrapApiResponse(await api.GET('/api/admin/users', {
    params: { query: { limit: 50, offset, ...(status ? { status } : {}) } },
  })),
  adminUser: async (id: string) => unwrapApiResponse(await api.GET('/api/admin/users/{id}', { params: { path: { id } } })),
  adminUserInstallations: async (id: string, offset = 0) => unwrapApiResponse(await api.GET('/api/admin/users/{id}/installations', { params: { path: { id }, query: { limit: 25, offset } } })),
  adminUserAuthorizations: async (id: string, offset = 0) => unwrapApiResponse(await api.GET('/api/admin/users/{id}/authorizations', { params: { path: { id }, query: { limit: 25, offset } } })),
  adminUserModelCalls: async (id: string, offset = 0) => unwrapApiResponse(await api.GET('/api/admin/users/{id}/model-usage/ledger', { params: { path: { id }, query: { limit: 25, offset } } })),
  updateUser: async (id: string, body: components['schemas']['UpdateUserRequest']) => unwrapApiResponse(await api.PATCH('/api/admin/users/{id}', { params: { path: { id } }, body })),
  adminUnknownModelUsage: async (ownerID: string) => unwrapApiResponse(await api.GET('/api/admin/model-usage/unknown', {
    params: { query: { owner_id: ownerID, limit: 100 } },
  })),
  reconcileModelUsage: async (body: operations['reconcileUnknownModelUsage']['requestBody']['content']['application/json']) => unwrapApiResponse(await api.POST('/api/admin/model-usage/reconcile', { body })),
  adminModelAccountBudget: async (id: string) => unwrapApiResponse(await api.GET('/api/admin/users/{id}/model-usage-budget', { params: { path: { id } } })),
  setAdminModelAccountBudget: async (id: string, body: operations['setModelAccountBudget']['requestBody']['content']['application/json']) => unwrapApiResponse(await api.PUT('/api/admin/users/{id}/model-usage-budget', { params: { path: { id } }, body })),
  adminPersonalDataQuota: async (id: string) => unwrapApiResponse(await api.GET('/api/admin/users/{id}/personal-data-quota', { params: { path: { id } } })),
  setAdminPersonalDataQuota: async (id: string, body: operations['setAdminPersonalDataQuota']['requestBody']['content']['application/json']) => unwrapApiResponse(await api.PATCH('/api/admin/users/{id}/personal-data-quota', { params: { path: { id } }, body })),
  capacity: async () => unwrapApiResponse(await api.GET('/api/admin/capacity')),
  audit: async (params: URLSearchParams) => {
    const query: AuditQuery = {};
    for (const name of ['actor', 'action', 'target', 'since'] as const) {
      const value = params.get(name);
      if (value !== null) query[name] = value;
    }
    const limit = params.get('limit');
    if (limit !== null) query.limit = Number(limit);
    return unwrapApiResponse(await api.GET('/api/admin/audit', { params: { query } }));
  },
};

export function newIdempotencyKey(): string {
  return globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${Math.random().toString(36).slice(2)}`;
}

function syncBlockPath(id: string, sessionId: string, digest: string): string {
  return `/api/workspaces/${encodeURIComponent(id)}/sync-sessions/${encodeURIComponent(sessionId)}/blocks/${encodeURIComponent(digest)}`;
}

async function throwResponseError(response: Response): Promise<never> {
  const error = await response.json().catch(() => ({}));
  unwrapApiResponse({ response, error });
  throw new Error('unreachable API error handling');
}

export function errorDetails(error: unknown): { code?: string; message: string; retryAfter?: string } {
  const value = error instanceof Error ? error as Error & { code?: string; retryAfter?: string | number } : undefined;
  const retryAfter = value?.retryAfter === undefined ? undefined : String(value.retryAfter);
  return { code: value?.code, message: errorMessage(error), retryAfter };
}

export function enterHostInNewTab(id: string, target?: 'desktop' | 'terminal' | 'workspace'): void {
  const destination = new URL(`/host-entry/${encodeURIComponent(id)}`, window.location.origin);
  if (target) destination.searchParams.set('target', target);
  const tab = window.open(destination.href, '_blank');
  if (!tab) throw new Error('浏览器阻止了新窗口，请允许弹窗后重试。');
  tab.opener = null;
}
