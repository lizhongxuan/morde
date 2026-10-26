import { api, requestFetch, unwrapApiResponse } from '../api/client';
import { errorMessage } from '../api/errors';
import type { components, operations } from '../api/schema';

type AuditQuery = NonNullable<operations['listAuditLogs']['parameters']['query']>;

export type HostMemberPermission = components['schemas']['HostMemberPermission'];
export type HostMember = components['schemas']['HostMember'];
export type SharedHost = components['schemas']['SharedHost'];

export const portalApi = {
  me: async () => unwrapApiResponse(await api.GET('/api/me')),
  preferences: async () => unwrapApiResponse(await api.GET('/api/me/preferences')),
  savePreferences: async (body: components['schemas']['SaveUserPreferencesRequest']) => unwrapApiResponse(await api.PUT('/api/me/preferences', { body })),
  onboardingStatus: async () => unwrapApiResponse(await api.GET('/api/onboarding/status')),
  ensureDefaultHost: async (body: components['schemas']['EnsureDefaultHostRequest'] = {}) => unwrapApiResponse(await api.POST('/api/me/default-host', { body })),
  login: async (name: string, password: string) => unwrapApiResponse(await api.POST('/api/auth/login', { body: { name, password } })),
  logout: async () => unwrapApiResponse(await api.POST('/api/auth/logout')),
  hosts: async () => unwrapApiResponse(await api.GET('/api/hosts')),
  sharedHosts: async () => unwrapApiResponse(await api.GET('/api/shared-hosts')),
  hostMembers: async (id: string) => unwrapApiResponse(await api.GET('/api/hosts/{id}/members', { params: { path: { id } } })),
  inviteHostMember: async (id: string, body: components['schemas']['HostMemberInviteRequest']) => unwrapApiResponse(await api.POST('/api/hosts/{id}/members', { params: { path: { id } }, body })),
  revokeHostMember: async (id: string, userID: string) => {
    unwrapApiResponse(await api.DELETE('/api/hosts/{id}/members/{user_id}', { params: { path: { id, user_id: userID } } }));
  },
  issueObservationTicket: async (id: string, target: 'desktop' | 'terminal' = 'desktop') => unwrapApiResponse(await api.POST('/api/hosts/{id}/observation-ticket', {
    params: { path: { id }, ...(target === 'terminal' ? { query: { target } } : {}) },
  })),
  host: async (id: string) => unwrapApiResponse(await api.GET('/api/hosts/{id}', { params: { path: { id } } })),
  templates: async () => unwrapApiResponse(await api.GET('/api/templates')),
  createHost: async (body: components['schemas']['CreateHostRequest'], idempotencyKey = newIdempotencyKey()) => unwrapApiResponse(await api.POST('/api/hosts', {
    params: { header: { 'Idempotency-Key': idempotencyKey } }, body,
  })),
  enterHost: async (id: string) => unwrapApiResponse(await api.POST('/api/hosts/{id}/enter', { params: { path: { id } } })),
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
  bindWorkspaceToHost: async (id: string, body: components['schemas']['BindWorkspaceRequest']) => unwrapApiResponse(await api.POST('/api/hosts/{id}/workspace-bindings', {
    params: { path: { id }, header: { 'Idempotency-Key': newIdempotencyKey() } }, body,
  })),
  retryHostWorkspaceSync: async (hostID: string, workspaceID: string, expectedBindingVersion: number) => {
    const response = await requestFetch(`/api/hosts/${encodeURIComponent(hostID)}/workspace-bindings/${encodeURIComponent(workspaceID)}/retry-sync`, {
      method: 'POST', body: JSON.stringify({ expected_binding_version: expectedBindingVersion }),
    });
    if (!response.ok) await throwResponseError(response);
  },
  unbindWorkspaceFromHost: async (id: string, workspaceID: string, body: operations['unbindWorkspaceFromHost']['requestBody']['content']['application/json']) => unwrapApiResponse(await api.DELETE('/api/hosts/{id}/workspace-bindings/{workspace_id}', {
    params: { path: { id, workspace_id: workspaceID }, header: { 'Idempotency-Key': newIdempotencyKey() } }, body,
  })),
  deleteWorkspace: async (id: string) => unwrapApiResponse(await api.DELETE('/api/workspaces/{id}', { params: { path: { id } } })),
  workspaceManifest: async (id: string, query: NonNullable<operations['getWorkspaceManifest']['parameters']['query']> = {}) =>
    unwrapApiResponse(await api.GET('/api/workspaces/{id}/manifest', { params: { path: { id }, query } })),
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
  inspectClientAuthorization: async (userCode: string) => unwrapApiResponse(await api.POST('/api/client-authorizations/inspect', { body: { user_code: userCode } })),
  approveClientAuthorization: async (body: { user_code: string; workspace_ids: string[] }) => unwrapApiResponse(await api.POST('/api/client-authorizations/approve', { body })),
  denyClientAuthorization: async (userCode: string) => unwrapApiResponse(await api.POST('/api/client-authorizations/deny', { body: { user_code: userCode } })),
  clients: async () => unwrapApiResponse(await api.GET('/api/clients')),
  revokeClient: async (id: string) => unwrapApiResponse(await api.DELETE('/api/clients/{id}', { params: { path: { id } } })),
  tasks: async (cursor?: string) => unwrapApiResponse(await api.GET('/api/tasks', { params: { query: { limit: 100, ...(cursor ? { cursor } : {}) } } })),
  conversations: async (cursor?: string) => unwrapApiResponse(await api.GET('/api/conversations', { params: { query: { limit: 100, ...(cursor ? { cursor } : {}) } } })),
  conversationMessages: async (id: string, cursor?: string) => unwrapApiResponse(await api.GET('/api/conversations/{id}/messages', { params: { path: { id }, query: { limit: 100, ...(cursor ? { cursor } : {}) } } })),
  submitTask: async (body: components['schemas']['SubmitTaskRequest'], idempotencyKey?: string) => unwrapApiResponse(await api.POST('/api/tasks', {
    params: { header: { 'Idempotency-Key': idempotencyKey ?? newIdempotencyKey() } }, body,
  })),
  appendConversationMessage: async (id: string, body: components['schemas']['AppendMessageRequest'], idempotencyKey?: string) => unwrapApiResponse(await api.POST('/api/conversations/{id}/messages', {
    params: { path: { id }, header: { 'Idempotency-Key': idempotencyKey ?? newIdempotencyKey() } }, body,
  })),
  task: async (id: string) => unwrapApiResponse(await api.GET('/api/tasks/{id}', { params: { path: { id } } })),
  taskAction: async (id: string, body: components['schemas']['TaskActionRequest'], idempotencyKey = newIdempotencyKey()) => unwrapApiResponse(await api.POST('/api/tasks/{id}/actions', {
    params: { path: { id }, header: { 'Idempotency-Key': idempotencyKey } }, body,
  })),
  schedules: async (cursor?: string) => unwrapApiResponse(await api.GET('/api/schedules', { params: { query: { limit: 100, ...(cursor ? { cursor } : {}) } } })),
  schedule: async (id: string) => unwrapApiResponse(await api.GET('/api/schedules/{id}', { params: { path: { id } } })),
  createSchedule: async (body: components['schemas']['CreateScheduleRequest']) => unwrapApiResponse(await api.POST('/api/schedules', {
    params: { header: { 'Idempotency-Key': newIdempotencyKey() } }, body,
  })),
  updateSchedule: async (id: string, body: components['schemas']['UpdateScheduleRequest']) => unwrapApiResponse(await api.PATCH('/api/schedules/{id}', { params: { path: { id } }, body })),
  scheduleAction: async (id: string, body: components['schemas']['ScheduleActionRequest']) => unwrapApiResponse(await api.POST('/api/schedules/{id}/actions', {
    params: { path: { id }, header: { 'Idempotency-Key': newIdempotencyKey() } }, body,
  })),
  scheduleRuns: async (id: string, cursor?: string) => unwrapApiResponse(await api.GET('/api/schedules/{id}/runs', { params: { path: { id }, query: { limit: 100, ...(cursor ? { cursor } : {}) } } })),
  notifications: async (cursor?: string, unreadOnly = false) => unwrapApiResponse(await api.GET('/api/notifications', { params: { query: { limit: 100, unread_only: unreadOnly, ...(cursor ? { cursor } : {}) } } })),
  markNotificationRead: async (id: string) => unwrapApiResponse(await api.POST('/api/notifications/{id}/read', { params: { path: { id } } })),
  executionControl: async () => unwrapApiResponse(await api.GET('/api/execution')),
  stopAllExecution: async (expectedEpoch: number) => unwrapApiResponse(await api.POST('/api/execution/stop-all', {
    params: { header: { 'Idempotency-Key': newIdempotencyKey() } }, body: { expected_epoch: expectedEpoch },
  })),
  enableExecution: async (expectedEpoch: number) => unwrapApiResponse(await api.POST('/api/execution/enable', {
    params: { header: { 'Idempotency-Key': newIdempotencyKey() } }, body: { expected_epoch: expectedEpoch },
  })),
  apiKeys: async () => unwrapApiResponse(await api.GET('/api/apikeys')),
  createApiKey: async (body: components['schemas']['CreateAPIKeyRequest']) => unwrapApiResponse(await api.POST('/api/apikeys', { body })),
  deleteApiKey: async (id: string) => unwrapApiResponse(await api.DELETE('/api/apikeys/{id}', { params: { path: { id } } })),
  modelConnections: async () => unwrapApiResponse(await api.GET('/api/model-connections')),
  createModelConnection: async (body: components['schemas']['CreateModelConnectionRequest']) => unwrapApiResponse(await api.POST('/api/model-connections', { body })),
  checkModelConnection: async (id: string) => unwrapApiResponse(await api.POST('/api/model-connections/{id}/check', { params: { path: { id } } })),
  adminHosts: async () => unwrapApiResponse(await api.GET('/api/admin/hosts')),
  adminWorkspaces: async (cursor?: string) => unwrapApiResponse(await api.GET('/api/admin/workspaces', {
    params: { query: { limit: 50, ...(cursor ? { cursor } : {}) } },
  })),
  adminWorkspace: async (id: string) => unwrapApiResponse(await api.GET('/api/admin/workspaces/{id}', { params: { path: { id } } })),
  adminUsers: async () => unwrapApiResponse(await api.GET('/api/admin/users')),
  createUser: async (body: components['schemas']['CreateUserRequest']) => unwrapApiResponse(await api.POST('/api/admin/users', { body })),
  updateUser: async (id: string, body: components['schemas']['UpdateUserRequest']) => unwrapApiResponse(await api.PATCH('/api/admin/users/{id}', { params: { path: { id } }, body })),
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

export function enterHostInNewTab(id: string): Promise<void> {
  const tab = window.open('about:blank', '_blank');
  if (!tab) return Promise.reject(new Error('浏览器阻止了新窗口，请允许弹窗后重试。'));
  tab.opener = null;
  return portalApi.enterHost(id).then(({ url }) => {
    const ticket = new URL(url);
    if (ticket.pathname !== '/__ch/auth' || !ticket.searchParams.has('t')) throw new Error('服务器返回了无效的云端电脑入口');
    tab.location.href = ticket.href;
  }).catch((error: unknown) => {
    tab.close();
    throw error;
  });
}
