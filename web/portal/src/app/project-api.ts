import { api, unwrapApiResponse } from '../api/client';
import type { components, operations } from '../api/schema';
import { newIdempotencyKey } from './api';

export type Project = components['schemas']['Project'];
export type ProjectRole = components['schemas']['ProjectRole'];
export type ProjectMember = components['schemas']['ProjectMember'];
export type ProjectInvitation = components['schemas']['ProjectInvitation'];
export type ProjectResource = components['schemas']['ProjectResource'];
export type ProjectWorkspaceManifest = components['schemas']['ProjectWorkspaceManifest'];
export type ProjectBudget = components['schemas']['ProjectBudget'];
export type ProjectBudgetLedgerEntry = components['schemas']['ProjectBudgetLedgerEntry'];
export type ProjectWorkItemCategory = components['schemas']['ProjectWorkItemCategory'];
export type ProjectWorkItem = components['schemas']['ProjectWorkItem'];
export type CreateProjectWorkItemRequest = components['schemas']['CreateProjectWorkItemRequest'];
export type ClaimProjectWorkItemRequest = components['schemas']['ClaimProjectWorkItemRequest'];
export type ProjectConversation = components['schemas']['ProjectConversation'];
export type ProjectConversationMessage = components['schemas']['ProjectConversationMessage'];
export type AcceptedProjectConversationMessageResponse = components['schemas']['AcceptedProjectConversationMessageResponse'];
export type Task = components['schemas']['Task'];

type Page<T> = { items: T[]; next_cursor: string | null };
type PageQuery = NonNullable<operations['listProjects']['parameters']['query']>;
type ProjectWorkItemsPageQuery = NonNullable<operations['listProjectWorkItems']['parameters']['query']>;
type ProjectConversationPageQuery = NonNullable<operations['listProjectConversations']['parameters']['query']>;
type ProjectConversationMessagesPageQuery = NonNullable<operations['listProjectConversationMessages']['parameters']['query']>;
type ProjectWorkspaceManifestQuery = NonNullable<operations['getProjectWorkspaceManifest']['parameters']['query']>;
type RequestKey = string;

async function allPages<T>(load: (query: PageQuery) => Promise<Page<T>>): Promise<T[]> {
  const items: T[] = [];
  let cursor: string | undefined;
  const seenCursors = new Set<string>();
  for (let page = 0; page < 1000; page += 1) {
    const result = await load({ limit: 100, ...(cursor ? { cursor } : {}) });
    items.push(...result.items);
    if (!result.next_cursor) return items;
    if (seenCursors.has(result.next_cursor)) throw new Error('分页游标重复，无法继续读取');
    seenCursors.add(result.next_cursor);
    cursor = result.next_cursor;
  }
  throw new Error('项目数据页数超过读取上限');
}

function requestKey(key?: RequestKey) {
  return key ?? newIdempotencyKey();
}

export const projectApi = {
  projects: () => allPages(async (query) => unwrapApiResponse(await api.GET('/api/projects', { params: { query } }))),
  getProject: async (id: string) => unwrapApiResponse(await api.GET('/api/projects/{id}', { params: { path: { id } } })),
  createProject: async (body: components['schemas']['CreateProjectRequest'], idempotencyKey?: RequestKey) => unwrapApiResponse(await api.POST('/api/projects', {
    params: { header: { 'Idempotency-Key': requestKey(idempotencyKey) } }, body,
  })),
  members: (id: string) => allPages(async (query) => unwrapApiResponse(await api.GET('/api/projects/{id}/members', { params: { path: { id }, query } }))),
  inviteMember: async (id: string, body: components['schemas']['InviteProjectMemberRequest'], idempotencyKey?: RequestKey) => unwrapApiResponse(await api.POST('/api/projects/{id}/members', {
    params: { path: { id }, header: { 'Idempotency-Key': requestKey(idempotencyKey) } }, body,
  })),
  updateMemberRole: async (id: string, userID: string, body: components['schemas']['UpdateProjectMemberRequest'], idempotencyKey?: RequestKey) => unwrapApiResponse(await api.PATCH('/api/projects/{id}/members/{user_id}', {
    params: { path: { id, user_id: userID }, header: { 'Idempotency-Key': requestKey(idempotencyKey) } }, body,
  })),
  removeMember: async (id: string, userID: string, body: components['schemas']['RemoveProjectMemberRequest'], idempotencyKey?: RequestKey) => {
    unwrapApiResponse(await api.DELETE('/api/projects/{id}/members/{user_id}', {
      params: { path: { id, user_id: userID }, header: { 'Idempotency-Key': requestKey(idempotencyKey) } }, body,
    }));
  },
  revokeInvitation: async (id: string, invitationID: string, body: components['schemas']['RevokeProjectInvitationRequest'], idempotencyKey?: RequestKey) => {
    unwrapApiResponse(await api.DELETE('/api/projects/{id}/invitations/{invitation_id}', {
      params: { path: { id, invitation_id: invitationID }, header: { 'Idempotency-Key': requestKey(idempotencyKey) } }, body,
    }));
  },
  myInvitations: () => allPages(async (query) => unwrapApiResponse(await api.GET('/api/me/project-invitations', { params: { query } }))),
  acceptInvitation: async (id: string, idempotencyKey?: RequestKey) => unwrapApiResponse(await api.POST('/api/project-invitations/{id}/accept', {
    params: { path: { id }, header: { 'Idempotency-Key': requestKey(idempotencyKey) } },
  })),
  transferOwner: async (id: string, body: components['schemas']['TransferProjectOwnerRequest'], idempotencyKey?: RequestKey) => unwrapApiResponse(await api.POST('/api/projects/{id}/owner-transfer', {
    params: { path: { id }, header: { 'Idempotency-Key': requestKey(idempotencyKey) } }, body,
  })),
  resources: (id: string) => allPages(async (query) => unwrapApiResponse(await api.GET('/api/projects/{id}/resources', { params: { path: { id }, query } }))),
  workspaceManifest: async (id: string, workspaceID: string, query: ProjectWorkspaceManifestQuery) => unwrapApiResponse(await api.GET('/api/projects/{id}/workspaces/{workspace_id}/manifest', {
    params: { path: { id, workspace_id: workspaceID }, query },
  })),
  addResource: async (id: string, body: components['schemas']['ProjectResourceMutationRequest'], idempotencyKey?: RequestKey) => unwrapApiResponse(await api.POST('/api/projects/{id}/resources', {
    params: { path: { id }, header: { 'Idempotency-Key': requestKey(idempotencyKey) } }, body,
  })),
  removeResource: async (id: string, body: components['schemas']['ProjectResourceMutationRequest'], idempotencyKey?: RequestKey) => {
    unwrapApiResponse(await api.DELETE('/api/projects/{id}/resources', {
      params: { path: { id }, header: { 'Idempotency-Key': requestKey(idempotencyKey) } }, body,
    }));
  },
  budget: async (id: string) => unwrapApiResponse(await api.GET('/api/projects/{id}/budget', { params: { path: { id } } })),
  setBudget: async (id: string, body: components['schemas']['SetProjectBudgetRequest'], idempotencyKey?: RequestKey) => unwrapApiResponse(await api.PUT('/api/projects/{id}/budget', {
    params: { path: { id }, header: { 'Idempotency-Key': requestKey(idempotencyKey) } }, body,
  })),
  ledger: (id: string) => allPages(async (query) => unwrapApiResponse(await api.GET('/api/projects/{id}/budget/ledger', { params: { path: { id }, query } }))),
  workItems: (id: string, includeUnassigned = false): Promise<ProjectWorkItem[]> => allPages(async (query) => {
    const pageQuery: ProjectWorkItemsPageQuery = {
      ...query,
      ...(includeUnassigned ? { include_unassigned: true } : {}),
    };
    const page = unwrapApiResponse(await api.GET('/api/projects/{id}/work-items', {
      params: { path: { id }, query: pageQuery },
    }));
    return page;
  }),
  createWorkItem: async (id: string, body: CreateProjectWorkItemRequest, idempotencyKey?: RequestKey): Promise<ProjectWorkItem> => unwrapApiResponse(await api.POST('/api/projects/{id}/work-items', {
    params: { path: { id }, header: { 'Idempotency-Key': requestKey(idempotencyKey) } },
    body,
  })) as ProjectWorkItem,
  assignWorkItem: async (id: string, workItemID: string, body: components['schemas']['AssignProjectWorkItemRequest'], idempotencyKey?: RequestKey): Promise<ProjectWorkItem> => unwrapApiResponse(await api.PATCH('/api/projects/{id}/work-items/{work_item_id}', {
    params: { path: { id, work_item_id: workItemID }, header: { 'Idempotency-Key': requestKey(idempotencyKey) } }, body,
  })) as ProjectWorkItem,
  claimWorkItem: async (id: string, workItemID: string, body: ClaimProjectWorkItemRequest, idempotencyKey?: RequestKey): Promise<ProjectWorkItem> => unwrapApiResponse(await api.POST('/api/projects/{id}/work-items/{work_item_id}/claim', {
    params: { path: { id, work_item_id: workItemID }, header: { 'Idempotency-Key': requestKey(idempotencyKey) } }, body,
  })) as ProjectWorkItem,
  blockWorkItem: async (id: string, workItemID: string, body: components['schemas']['BlockProjectWorkItemRequest'], idempotencyKey?: RequestKey): Promise<ProjectWorkItem> => unwrapApiResponse(await api.POST('/api/projects/{id}/work-items/{work_item_id}/block', {
    params: { path: { id, work_item_id: workItemID }, header: { 'Idempotency-Key': requestKey(idempotencyKey) } }, body,
  })) as ProjectWorkItem,
  resumeWorkItem: async (id: string, workItemID: string, body: components['schemas']['ProjectWorkItemVersionRequest'], idempotencyKey?: RequestKey): Promise<ProjectWorkItem> => unwrapApiResponse(await api.POST('/api/projects/{id}/work-items/{work_item_id}/resume', {
    params: { path: { id, work_item_id: workItemID }, header: { 'Idempotency-Key': requestKey(idempotencyKey) } }, body,
  })) as ProjectWorkItem,
  linkWorkItemTask: async (id: string, workItemID: string, body: components['schemas']['LinkProjectWorkItemTaskRequest'], idempotencyKey?: RequestKey): Promise<ProjectWorkItem> => unwrapApiResponse(await api.POST('/api/projects/{id}/work-items/{work_item_id}/tasks', {
    params: { path: { id, work_item_id: workItemID }, header: { 'Idempotency-Key': requestKey(idempotencyKey) } }, body,
  })) as ProjectWorkItem,
  deliverWorkItem: async (id: string, workItemID: string, body: components['schemas']['DeliverProjectWorkItemRequest'], idempotencyKey?: RequestKey): Promise<ProjectWorkItem> => unwrapApiResponse(await api.POST('/api/projects/{id}/work-items/{work_item_id}/delivery', {
    params: { path: { id, work_item_id: workItemID }, header: { 'Idempotency-Key': requestKey(idempotencyKey) } }, body,
  })) as ProjectWorkItem,
  tasks: () => allPages(async (query) => unwrapApiResponse(await api.GET('/api/tasks', { params: { query } }))),
  projectConversations: (id: string) => allPages(async (query: ProjectConversationPageQuery) => unwrapApiResponse(await api.GET('/api/projects/{id}/conversations', { params: { path: { id }, query } }))),
  projectConversationMessages: (id: string, conversationID: string, clientMessageID?: string, serviceInstanceID?: string, actorID?: string) => {
    if (clientMessageID && (!serviceInstanceID || !actorID)) throw new Error('服务或账号身份尚未确认。');
    return allPages(async (query: ProjectConversationMessagesPageQuery) => unwrapApiResponse(await api.GET('/api/projects/{id}/conversations/{conversation_id}/messages', { params: { path: { id, conversation_id: conversationID }, query: { ...query, ...(clientMessageID ? { client_message_id: clientMessageID } : {}) }, ...(serviceInstanceID || actorID ? { header: { ...(serviceInstanceID ? { 'X-Morde-Service-Instance-ID': serviceInstanceID } : {}), ...(actorID ? { 'X-Morde-Actor-ID': actorID } : {}) } } : {}) } })));
  },
  createProjectConversation: async (id: string, body: components['schemas']['CreateProjectConversationRequest'], idempotencyKey?: RequestKey) => unwrapApiResponse(await api.POST('/api/projects/{id}/conversations', {
    params: { path: { id }, header: { 'Idempotency-Key': requestKey(idempotencyKey) } }, body,
  })),
  acceptProjectConversationMessage: async (id: string, conversationID: string, body: components['schemas']['AcceptConversationMessageRequest'], idempotencyKey: string, serviceInstanceID: string, actorID: string) => {
    if (idempotencyKey !== body.client_message_id) throw new Error('消息编号与请求编号不一致。');
    if (!serviceInstanceID || !actorID) throw new Error('服务或账号身份尚未确认。');
    const result = await api.POST('/api/projects/{id}/conversations/{conversation_id}/messages', {
      params: { path: { id, conversation_id: conversationID }, header: { 'Idempotency-Key': idempotencyKey, 'X-Morde-Service-Instance-ID': serviceInstanceID, 'X-Morde-Actor-ID': actorID } }, body,
    });
    const receipt = unwrapApiResponse(result);
    if (result.response.status !== 202) throw new Error('服务端未确认消息受理，请核对后重试。');
    return receipt;
  },
  retryProjectConversationMessageProcessing: async (id: string, conversationID: string, messageID: string, idempotencyKey: string, serviceInstanceID: string, actorID: string) => {
    const result = await api.POST('/api/projects/{id}/conversations/{conversation_id}/messages/{message_id}/retry-processing', {
      parseAs: 'text',
      params: { path: { id, conversation_id: conversationID, message_id: messageID },
        header: { 'Idempotency-Key': idempotencyKey, 'X-Morde-Service-Instance-ID': serviceInstanceID, 'X-Morde-Actor-ID': actorID } },
    });
    if (result.error !== undefined) unwrapApiResponse(result);
    if (result.response.status !== 202) throw new Error('服务端未确认消息重试。');
  },
};
