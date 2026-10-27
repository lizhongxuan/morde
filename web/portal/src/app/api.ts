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
  clients: async () => unwrapApiResponse(await api.GET('/api/clients')),
  revokeClient: async (id: string) => unwrapApiResponse(await api.DELETE('/api/clients/{id}', { params: { path: { id } } })),
  instantChatConversations: async () => unwrapApiResponse(await api.GET('/api/instant-chat/conversations')),
  instantChatMessages: async (id: string) => unwrapApiResponse(await api.GET('/api/instant-chat/conversations/{id}/messages', { params: { path: { id } } })),
  sendInstantChatMessage: async (body: components['schemas']['InstantChatSendRequest'], idempotencyKey = newIdempotencyKey()) => unwrapApiResponse(await api.POST('/api/instant-chat/messages', {
    params: { header: { 'Idempotency-Key': idempotencyKey } }, body,
  })),
  notifications: async (cursor?: string, unreadOnly = false) => unwrapApiResponse(await api.GET('/api/notifications', { params: { query: { limit: 100, unread_only: unreadOnly, ...(cursor ? { cursor } : {}) } } })),
  markNotificationRead: async (id: string) => unwrapApiResponse(await api.POST('/api/notifications/{id}/read', { params: { path: { id } } })),
  apiKeys: async () => unwrapApiResponse(await api.GET('/api/apikeys')),
  createApiKey: async (body: components['schemas']['CreateAPIKeyRequest']) => unwrapApiResponse(await api.POST('/api/apikeys', { body })),
  deleteApiKey: async (id: string) => unwrapApiResponse(await api.DELETE('/api/apikeys/{id}', { params: { path: { id } } })),
  modelConnections: async () => unwrapApiResponse(await api.GET('/api/model-connections')),
  createModelConnection: async (body: components['schemas']['CreateModelConnectionRequest']) => unwrapApiResponse(await api.POST('/api/model-connections', { body })),
  checkModelConnection: async (id: string, idempotencyKey = newIdempotencyKey()) => unwrapApiResponse(await api.POST('/api/model-connections/{id}/check', {
    params: { path: { id }, header: { 'Idempotency-Key': idempotencyKey } },
  })),
  adminHosts: async () => unwrapApiResponse(await api.GET('/api/admin/hosts')),
  adminHostResources: async (id: string) => unwrapApiResponse(await api.GET('/api/admin/hosts/{id}/resources', { params: { path: { id } } })),
  updateAdminHostResources: async (id: string, body: components['schemas']['UpdateAdminHostResourcesRequest']) => unwrapApiResponse(await api.PATCH('/api/admin/hosts/{id}/resources', { params: { path: { id } }, body })),
  adminWorkspaces: async (cursor?: string) => unwrapApiResponse(await api.GET('/api/admin/workspaces', {
    params: { query: { limit: 50, ...(cursor ? { cursor } : {}) } },
  })),
  adminWorkspace: async (id: string) => unwrapApiResponse(await api.GET('/api/admin/workspaces/{id}', { params: { path: { id } } })),
  adminUsers: async (status?: components['schemas']['AccountStatus']) => unwrapApiResponse(await api.GET('/api/admin/users', {
    params: { query: status ? { status } : {} },
  })),
  updateUser: async (id: string, body: components['schemas']['UpdateUserRequest']) => unwrapApiResponse(await api.PATCH('/api/admin/users/{id}', { params: { path: { id } }, body })),
  adminUnknownModelUsage: async (ownerID: string) => unwrapApiResponse(await api.GET('/api/admin/model-usage/unknown', {
    params: { query: { owner_id: ownerID, limit: 100 } },
  })),
  reconcileModelUsage: async (body: operations['reconcileUnknownModelUsage']['requestBody']['content']['application/json']) => unwrapApiResponse(await api.POST('/api/admin/model-usage/reconcile', { body })),
  adminModelAccountBudget: async (id: string) => unwrapApiResponse(await api.GET('/api/admin/users/{id}/model-usage-budget', { params: { path: { id } } })),
  setAdminModelAccountBudget: async (id: string, body: operations['setModelAccountBudget']['requestBody']['content']['application/json']) => unwrapApiResponse(await api.PUT('/api/admin/users/{id}/model-usage-budget', { params: { path: { id } }, body })),
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

export function enterHostInNewTab(id: string, target?: 'desktop' | 'terminal'): void {
  const destination = new URL(`/host-entry/${encodeURIComponent(id)}`, window.location.origin);
  if (target) destination.searchParams.set('target', target);
  const tab = window.open(destination.href, '_blank');
  if (!tab) throw new Error('浏览器阻止了新窗口，请允许弹窗后重试。');
  tab.opener = null;
}
