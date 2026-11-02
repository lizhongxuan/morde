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
  execution: async () => unwrapApiResponse(await api.GET('/api/execution')),
  onboardingStatus: async () => unwrapApiResponse(await api.GET('/api/onboarding/status')),
  ensureDefaultHost: async (body: components['schemas']['EnsureDefaultHostRequest'] = {}) => unwrapApiResponse(await api.POST('/api/me/default-host', { body })),
  login: async (name: string, password: string) => unwrapApiResponse(await api.POST('/api/auth/login', { body: { name, password } })),
  register: async (name: string, password: string, inviteCode = '') => unwrapApiResponse(await api.POST('/api/auth/register', { body: { name, password, ...(inviteCode.trim() ? { invite_code: inviteCode.trim() } : {}) } })),
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
  capabilities: async () => unwrapApiResponse(await api.GET('/api/capabilities')),
  conversations: async (cursor?: string) => unwrapApiResponse(await api.GET('/api/conversations', { params: { query: { limit: 100, ...(cursor ? { cursor } : {}) } } })),
  conversationMessages: async (id: string, cursor?: string) => unwrapApiResponse(await api.GET('/api/conversations/{id}/messages', {
    params: { path: { id }, query: { limit: 100, ...(cursor ? { cursor } : {}) } },
  })),
  task: async (id: string) => unwrapApiResponse(await api.GET('/api/tasks/{id}', { params: { path: { id } } })),
  taskAction: async (id: string, body: components['schemas']['TaskActionRequest'], idempotencyKey = newIdempotencyKey()) => unwrapApiResponse(await api.POST('/api/tasks/{id}/actions', {
    params: { path: { id }, header: { 'Idempotency-Key': idempotencyKey } }, body,
  })),
  submitTask: async (body: components['schemas']['SubmitTaskRequest'], idempotencyKey = newIdempotencyKey()) => unwrapApiResponse(await api.POST('/api/tasks', {
    params: { header: { 'Idempotency-Key': idempotencyKey } }, body,
  })),
  appendConversationMessage: async (id: string, body: components['schemas']['AppendConversationMessageRequest'], idempotencyKey = newIdempotencyKey()) => unwrapApiResponse(await api.POST('/api/conversations/{id}/messages', {
    params: { path: { id }, header: { 'Idempotency-Key': idempotencyKey } }, body,
  })),
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
    body: components['schemas']['PersonalObjectPutRequest']['body'], idempotencyKey = newIdempotencyKey(),
  ) => unwrapApiResponse(await api.PUT('/api/personal-data/{type}/{id}', {
    params: { path: { type, id }, header: { 'Idempotency-Key': idempotencyKey } }, body: { base_revision: baseRevision, body },
  })),
  deletePersonalObject: async (
    type: components['schemas']['PersonalMutationObjectType'], id: string, baseRevision: number,
    idempotencyKey = newIdempotencyKey(),
  ) => unwrapApiResponse(await api.DELETE('/api/personal-data/{type}/{id}', {
    params: { path: { type, id }, header: { 'Idempotency-Key': idempotencyKey } }, body: { base_revision: baseRevision },
  })),
  personalDataQuota: async () => unwrapApiResponse(await api.GET('/api/personal-data/quota')),
  createPersonalAttachmentUpload: async (
    body: components['schemas']['PersonalAttachmentUploadRequest'], idempotencyKey = newIdempotencyKey(),
  ) => unwrapApiResponse(await api.POST('/api/personal-data/uploads', {
    params: { header: { 'Idempotency-Key': idempotencyKey } }, body,
  })),
  putPersonalAttachmentChunk: async (uploadID: string, sequence: number, body: BodyInit, sha256: string) => {
    const response = await requestFetch(`/api/personal-data/uploads/${encodeURIComponent(uploadID)}/chunks/${sequence}`, {
      method: 'PUT', headers: { 'Content-Type': 'application/octet-stream', 'X-Chunk-SHA256': sha256 }, body,
    });
    if (!response.ok) await throwResponseError(response);
    return unwrapApiResponse(await response.json().then((data) => ({ data, response })));
  },
  commitPersonalAttachment: async (uploadID: string, idempotencyKey = newIdempotencyKey()) => unwrapApiResponse(await api.POST('/api/personal-data/uploads/{upload_id}/commit', {
    params: { path: { upload_id: uploadID }, header: { 'Idempotency-Key': idempotencyKey } },
  })),
  cancelPersonalAttachmentUpload: async (uploadID: string) => unwrapApiResponse(await api.DELETE('/api/personal-data/uploads/{upload_id}', { params: { path: { upload_id: uploadID } } })),
  personalAttachment: async (attachmentID: string): Promise<Blob> => {
    const response = await requestFetch(`/api/personal-data/attachments/${encodeURIComponent(attachmentID)}`);
    if (!response.ok) await throwResponseError(response);
    return response.blob();
  },
  importPersonalHistory: async (body: components['schemas']['PersonalHistoryImportRequest']) => unwrapApiResponse(await api.POST('/api/personal-data/history/import', { body })),
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
  checkModelConnection: async (id: string, idempotencyKey = newIdempotencyKey()) => unwrapApiResponse(await api.POST('/api/model-connections/{id}/check', {
    params: { path: { id }, header: { 'Idempotency-Key': idempotencyKey } },
  })),
  adminOverview: async () => unwrapApiResponse(await api.GET('/api/admin/overview')),
  adminModelConnections: async () => unwrapApiResponse(await api.GET('/api/admin/model-connections')),
  adminUsage: async (days: 7 | 30 | 90) => unwrapApiResponse(await api.GET('/api/admin/usage', { params: { query: { days } } })),
  adminQuality: async (days: 7 | 30 | 90 = 7) => unwrapApiResponse(await api.GET('/api/admin/quality', { params: { query: { days } } })),
  adminActivity: async (days: 7 | 30 | 90 = 7) => unwrapApiResponse(await api.GET('/api/admin/activity', { params: { query: { days } } })),
  adminPlaces: async (days: 7 | 30 | 90 = 7) => unwrapApiResponse(await api.GET('/api/admin/places', { params: { query: { days } } })),
  adminControls: async () => unwrapApiResponse(await api.GET('/api/admin/controls')),
  adminSettings: async () => unwrapApiResponse(await api.GET('/api/admin/settings')),
  adminInvites: async (search = '', limit = 25, offset = 0) => unwrapApiResponse(await api.GET('/api/admin/invites', { params: { query: { search, limit, offset } } })),
  adminSites: async (limit = 25, offset = 0) => unwrapApiResponse(await api.GET('/api/admin/sites', { params: { query: { limit, offset } } })),
  adminNudges: async () => unwrapApiResponse(await api.GET('/api/admin/nudges')),
  updateAdminNudges: async (body: components['schemas']['UpdateAdminNudgePolicyRequest']) => unwrapApiResponse(await api.PUT('/api/admin/nudges', { body })),
  myInviteCode: async () => unwrapApiResponse(await api.GET('/api/me/invite-code')),
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
