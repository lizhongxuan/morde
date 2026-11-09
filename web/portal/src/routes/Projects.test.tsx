import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError } from '../api/client';
import type { components } from '../api/schema';
import { portalApi } from '../app/api';
import { messageOutbox } from '../app/message-outbox';
import type { PendingConversationMessage } from '../app/message-outbox';
import { projectApi } from '../app/project-api';
import type { AcceptedProjectConversationMessageResponse, Project, ProjectBudget, ProjectConversation, ProjectConversationMessage, ProjectInvitation, ProjectMember, ProjectResource, ProjectWorkItem } from '../app/project-api';
import { Projects } from './Projects';

const projectAuthState = vi.hoisted(() => ({ role: 'user' as string }));
vi.mock('../app/auth', () => ({ useAuth: () => ({ user: { id: 'uowner01', role: projectAuthState.role } }) }));
vi.mock('../components/AssistantReplySpeech', () => ({
  AssistantReplySpeech: ({ text, sessionKey }: { text: string; sessionKey: string }) =>
    <button type="button" aria-label="朗读回复" data-session-key={sessionKey} data-reply-text={text} />,
}));

const project: Project = { id: 'jproject1', owner_id: 'uowner01', name: '交付协作', description: '', version: 3 };
const owner: ProjectMember = { project_id: project.id, user_id: project.owner_id, role: 'owner', version: 1 };
const teammate: ProjectMember = { project_id: project.id, user_id: 'umember1', role: 'member', version: 2 };
const projectConversation: ProjectConversation = {
  id: 'cproj001', project_id: project.id, owner_id: teammate.user_id, title: '共享执行计划',
  created_at: '2026-11-02T00:00:00Z', updated_at: '2026-11-02T00:00:00Z',
};
const projectMessageReceipt: AcceptedProjectConversationMessageResponse = {
    message: {
    id: 'mproj001', conversation_id: projectConversation.id, owner_id: projectConversation.owner_id, actor_id: owner.user_id,
    sequence: 1, role: 'user', content: '检查当前发布状态', client_message_id: 'project-message-001', routing_status: 'pending', delivery_status: 'pending', created_at: '2026-11-02T00:01:00Z',
  },
  replayed: false,
};
const projectHistoryMessage: ProjectConversationMessage = {
  ...projectMessageReceipt.message, id: 'mhistory1', task_id: 'qhistory1', actor_id: owner.user_id,
  content: '上一位成员提交的提示', routing_status: 'routed', delivery_status: 'applied',
};
const newerProjectHistoryMessage: ProjectConversationMessage = {
  ...projectHistoryMessage, id: 'mhistory2', sequence: 2, content: '较新的项目提示',
};
const modelConnection: components['schemas']['ModelConnection'] = {
  id: 'model-test', name: '团队文本模型', base_url: 'https://models.example.invalid/v1', model_name: 'test-model',
  shared: true, has_api_key: true, capabilities: { text: true, tools: true, vision: false, text_stream: false },
  checked_at: '2026-11-02T00:00:00Z', created_at: '2026-11-02T00:00:00Z', updated_at: '2026-11-02T00:00:00Z',
};
const alternateModelConnection = { ...modelConnection, id: 'model-alt01', name: '备用文本模型', model_name: 'alternate-model' };
const readyCapabilities: Awaited<ReturnType<typeof portalApi.capabilities>> = {
  service_instance_id: '00000000-0000-4000-8000-000000000001', api_version: 'v1', hub_version: null,
  event_version: null, minimum_client_version: null, supported_features: ['project_budget_execution', 'conversation_auto_routing_v1'], default_max_request_bytes: 1_048_576,
};
const invitation: ProjectInvitation = {
  id: 'i0000001', project_id: project.id, inviter_id: 'umember1', invitee_id: 'uowner01', role: 'viewer', state: 'pending', version: 4,
};
const budget: ProjectBudget = {
  configured: true, currency: 'USD', limit_micros: 10_000_000, version: 2,
  spent_micros: 2_500_000, reserved_micros: 1_000_000, has_unknown: true,
};
const workItem: ProjectWorkItem = {
  id: 'w0000001', project_id: project.id, title: '验证发布', description: '检查服务端流程', acceptance_criteria: ['有持久化证据'],
  assignee_id: owner.user_id, category: 'unclassified', state: 'open', blocked_reason: '', task_ids: [], version: 1,
};
const task = {
  id: 't0000001', owner_id: owner.user_id, initiator_id: owner.user_id, execution_location: 'server' as const, conversation_id: 'c0000001', model_id: 'model-test',
  objective: '验证交付', status: 'completed' as const, current_step: null,
  step_count: 0, completed_step_count: 0,
  budget: { active_seconds: 0, tool_calls: 0, model_tokens: 0 }, usage: { active_seconds: 0, tool_calls: 0, model_tokens: 0 },
  created_at: '2026-11-02T00:00:00Z', updated_at: '2026-11-02T00:00:00Z',
};

function renderProjects() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  const result = render(<QueryClientProvider client={client}><MemoryRouter><Projects /></MemoryRouter></QueryClientProvider>);
  return { ...result, client };
}

async function openProject() {
  await userEvent.setup().click(await screen.findByRole('button', { name: '查看项目 交付协作' }));
}

describe('Projects API integration', () => {
  beforeEach(() => {
    const values = new Map<string, PendingConversationMessage[]>();
    const orderings = new Map<string, { heads: Record<string, string>; aliases: Record<string, string> }>();
    vi.spyOn(messageOutbox, 'read').mockImplementation(async (scope) => values.get(scope) ?? []);
    vi.spyOn(messageOutbox, 'update').mockImplementation(async (scope, change) => {
      const ordering = orderings.get(scope) ?? { heads: {}, aliases: {} };
      const result = change(values.get(scope) ?? [], ordering);
      orderings.set(scope, ordering);
      values.set(scope, result);
      return result;
    });
    vi.spyOn(projectApi, 'projects').mockResolvedValue([project]);
    vi.spyOn(projectApi, 'getProject').mockResolvedValue(project);
    vi.spyOn(projectApi, 'members').mockResolvedValue([owner, teammate]);
    vi.spyOn(projectApi, 'inviteMember').mockResolvedValue({ ...invitation, invitee_id: 'uuser001', inviter_id: owner.user_id, role: 'member' });
    vi.spyOn(projectApi, 'revokeInvitation').mockResolvedValue(undefined);
    vi.spyOn(projectApi, 'myInvitations').mockResolvedValue([invitation]);
    vi.spyOn(projectApi, 'acceptInvitation').mockResolvedValue({ project_id: project.id, user_id: owner.user_id, role: 'viewer', version: 5 });
    vi.spyOn(projectApi, 'transferOwner').mockResolvedValue({ ...project, owner_id: teammate.user_id, version: 4 });
    vi.spyOn(projectApi, 'resources').mockResolvedValue([]);
    vi.spyOn(projectApi, 'projectConversations').mockResolvedValue([projectConversation]);
    vi.spyOn(projectApi, 'projectConversationMessages').mockImplementation(async (_id, conversationID) => conversationID === projectConversation.id ? [projectHistoryMessage] : []);
    vi.spyOn(projectApi, 'createProjectConversation').mockResolvedValue(projectConversation);
    vi.spyOn(projectApi, 'acceptProjectConversationMessage').mockImplementation(async (_id, conversationID, body) => ({
      message: { ...projectMessageReceipt.message, conversation_id: conversationID, client_message_id: body.client_message_id, content: body.content },
      replayed: false,
    }));
    vi.spyOn(projectApi, 'budget').mockResolvedValue(budget);
    vi.spyOn(projectApi, 'ledger').mockResolvedValue([{
      call_id: 'call-unknown', actor_id: owner.user_id, source: 'task', task_id: task.id, model_id: task.model_id,
      rate_version: 1, currency: 'USD', status: 'unknown', reserved_usage: { input_tokens: 20, output_tokens: 10 }, reserved_micros: 250_000,
      created_at: '2026-11-02T00:00:00Z',
    }]);
    vi.spyOn(projectApi, 'workItems').mockResolvedValue([workItem]);
    vi.spyOn(projectApi, 'createWorkItem').mockResolvedValue(workItem);
    vi.spyOn(projectApi, 'claimWorkItem').mockResolvedValue(workItem);
    vi.spyOn(projectApi, 'tasks').mockResolvedValue([task]);
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [modelConnection], configuration_status: 'configured' });
    vi.spyOn(portalApi, 'capabilities').mockResolvedValue(readyCapabilities);
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'workspaces').mockResolvedValue({ items: [] });
  });

  afterEach(() => { cleanup(); localStorage.clear(); projectAuthState.role = 'user'; vi.restoreAllMocks(); vi.unstubAllGlobals(); vi.unstubAllEnvs(); });

  it('explains that administrators cannot use projects without calling project APIs', () => {
    projectAuthState.role = 'super_admin';
    renderProjects();
    expect(screen.getByRole('status')).toHaveTextContent('项目功能仅支持普通用户');
    expect(projectApi.projects).not.toHaveBeenCalled();
    expect(projectApi.myInvitations).not.toHaveBeenCalled();
  });

  it('shows project attachments from another client using their scoped read route', async () => {
    const attachmentID = 'pca_0123456789abcdef0123456789abcdef';
    vi.mocked(projectApi.projectConversationMessages).mockResolvedValue([{ ...projectHistoryMessage, attachment_ids: [attachmentID] }]);
    renderProjects();
    await openProject();
    const link = await screen.findByRole('link', { name: '下载附件 1' });
    expect(link).toHaveAttribute('href', `/api/projects/${project.id}/conversations/${projectConversation.id}/messages/${projectHistoryMessage.id}/attachments-by-id/${attachmentID}/content`);
    expect(link).toHaveAttribute('download');
  });

  it('deduplicates overlapping project history pages by message ID', async () => {
    vi.mocked(projectApi.projectConversationMessages).mockResolvedValue([projectHistoryMessage,
      { ...projectHistoryMessage, content: '较旧的重复版本' }]);
    renderProjects();
    await openProject();
    expect(await screen.findByText(projectHistoryMessage.content)).toBeInTheDocument();
    expect(screen.queryByText('较旧的重复版本')).not.toBeInTheDocument();
    expect(screen.getAllByRole('article', { name: '项目对话消息' })).toHaveLength(1);
  });

  it('renders project assistant replies as Markdown', async () => {
    vi.mocked(projectApi.projectConversationMessages).mockResolvedValue([{
      ...projectHistoryMessage, role: 'assistant', task_id: undefined, content: '**已完成**\n\n- 搜索\n- 汇总',
    }]);
    renderProjects();
    await openProject();

    await screen.findByText('已完成', {}, { timeout: 5000 });
    const reply = (await screen.findAllByRole('article', { name: '项目对话消息' }))[0]!;
    expect(within(reply).getByText('已完成').tagName).toBe('STRONG');
    expect(within(reply).getByRole('list').querySelectorAll('li')).toHaveLength(2);
  });

  it('stops only the current member’s selected project task', async () => {
    const mine = { ...task, id: 'qhistory1', conversation_id: projectConversation.id, status: 'running' as const };
    const other = { ...mine, id: 'qhistory2', initiator_id: teammate.user_id };
    vi.mocked(projectApi.tasks).mockResolvedValue([mine, other]);
    vi.mocked(projectApi.projectConversationMessages).mockResolvedValue([projectHistoryMessage,
      { ...newerProjectHistoryMessage, task_id: other.id, actor_id: teammate.user_id }]);
    const stop = vi.spyOn(portalApi, 'taskAction').mockResolvedValue({ task_id: mine.id, action: 'cancel', status: 'cancelling', unsettled_operation_ids: [] });
    const user = userEvent.setup();
    renderProjects();
    await openProject();
    const button = await screen.findByRole('button', { name: '停止此任务' });
    expect(screen.getAllByRole('button', { name: '停止此任务' })).toHaveLength(1);
    await user.click(button);
    await waitFor(() => expect(stop).toHaveBeenCalledWith(mine.id, { action: 'cancel', expected_status: 'running' }, expect.any(String)));
    expect(await screen.findByText('停止请求已受理，正在等待服务端确认任务状态。')).toBeInTheDocument();
  });

  it('uses the typed project conversation routes and idempotency headers', async () => {
    vi.mocked(projectApi.createProjectConversation).mockRestore();
    vi.mocked(projectApi.projectConversations).mockRestore();
    vi.mocked(projectApi.projectConversationMessages).mockRestore();
    vi.mocked(projectApi.acceptProjectConversationMessage).mockRestore();
    const requests: Request[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = input instanceof Request ? new Request(input, init) : new Request(input, init);
      requests.push(request);
      if (request.method === 'GET' && new URL(request.url).pathname.endsWith('/messages')) {
        return Response.json({ items: [projectHistoryMessage], next_cursor: null });
      }
      if (request.method === 'GET') return Response.json({ items: [projectConversation], next_cursor: null });
      if (new URL(request.url).pathname.endsWith('/messages')) return Response.json(projectMessageReceipt, { status: 202 });
      return Response.json(projectConversation, { status: 201 });
    }));

    await projectApi.createProjectConversation(project.id, { title: projectConversation.title }, 'project-create-key-01');
    await projectApi.projectConversations(project.id);
    await projectApi.projectConversationMessages(project.id, projectConversation.id);
    await projectApi.projectConversationMessages(project.id, projectConversation.id, 'project-message-key-01', readyCapabilities.service_instance_id, owner.user_id);
    await projectApi.acceptProjectConversationMessage(project.id, projectConversation.id, {
      client_message_id: 'project-message-key-01', model_id: modelConnection.id, content: projectMessageReceipt.message.content,
    }, 'project-message-key-01', readyCapabilities.service_instance_id, owner.user_id);

    expect(requests.map((request) => [request.method, new URL(request.url).pathname])).toEqual([
      ['POST', `/api/projects/${project.id}/conversations`],
      ['GET', `/api/projects/${project.id}/conversations`],
      ['GET', `/api/projects/${project.id}/conversations/${projectConversation.id}/messages`],
      ['GET', `/api/projects/${project.id}/conversations/${projectConversation.id}/messages`],
      ['POST', `/api/projects/${project.id}/conversations/${projectConversation.id}/messages`],
    ]);
    expect(requests[0]?.headers.get('Idempotency-Key')).toBe('project-create-key-01');
    expect(requests[3]?.headers.get('X-Morde-Actor-ID')).toBe(owner.user_id);
    expect(requests[4]?.headers.get('Idempotency-Key')).toBe('project-message-key-01');
    expect(requests[4]?.headers.get('X-Morde-Service-Instance-ID')).toBe(readyCapabilities.service_instance_id);
    expect(requests[4]?.headers.get('X-Morde-Actor-ID')).toBe(owner.user_id);
    expect(await requests[4]?.clone().json()).toEqual({ client_message_id: 'project-message-key-01', model_id: modelConnection.id, content: projectMessageReceipt.message.content });
  });

  it('offers processing retries only for the sending project member and reuses an uncertain key', async () => {
    const own = { ...projectHistoryMessage, task_id: undefined, routing_status: 'failed' as const,
      routing_failure_reason: 'model_unavailable' as const, delivery_status: 'pending' as const };
    const other = { ...own, id: 'mhistory2', sequence: 2, actor_id: teammate.user_id, content: '其他成员的消息' };
    vi.mocked(projectApi.projectConversationMessages).mockResolvedValue([own, other]);
    const retry = vi.spyOn(projectApi, 'retryProjectConversationMessageProcessing')
      .mockRejectedValueOnce(new Error('连接中断')).mockResolvedValueOnce(undefined);
    const user = userEvent.setup();
    renderProjects();
    await openProject();
    const ownMessage = (await screen.findByText(own.content)).closest<HTMLElement>('article')!;
    const otherMessage = (await screen.findByText(other.content)).closest<HTMLElement>('article')!;
    expect(within(otherMessage).queryByRole('button', { name: '重试处理' })).not.toBeInTheDocument();
    await user.click(within(ownMessage).getByRole('button', { name: '重试处理' }));
    await screen.findByText('连接中断');
    await user.click(within(ownMessage).getByRole('button', { name: '重试处理' }));
    await waitFor(() => expect(retry).toHaveBeenCalledTimes(2));
    expect(retry.mock.calls[0]).toEqual(retry.mock.calls[1]);
    expect(retry.mock.calls[0]).toEqual([project.id, projectConversation.id, own.id, expect.any(String), readyCapabilities.service_instance_id, owner.user_id]);
    await screen.findByText('已请求重新处理消息。');
  });

  it('explains an oversized project message without offering an ineffective retry', async () => {
    vi.mocked(projectApi.projectConversationMessages).mockResolvedValue([{ ...projectHistoryMessage, task_id: undefined,
      routing_status: 'failed', routing_failure_reason: 'message_too_long', delivery_status: 'pending' }]);
    renderProjects();
    await openProject();
    const explanation = await screen.findByText(/消息及上下文超过当前模型输入上限/);
    expect(explanation).toHaveTextContent('原文已保留；请缩短后重新发送');
    expect(within(explanation.closest<HTMLElement>('article')!).queryByRole('button', { name: '重试处理' })).not.toBeInTheDocument();
  });

  it('uses the capability-gated work-item query and idempotent claim endpoint', async () => {
    vi.mocked(projectApi.workItems).mockRestore();
    vi.mocked(projectApi.createWorkItem).mockRestore();
    vi.mocked(projectApi.claimWorkItem).mockRestore();
    const requests: Request[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = input instanceof Request ? new Request(input, init) : new Request(input, init);
      requests.push(request);
      if (request.method === 'GET') return Response.json({ items: [{ ...workItem, category: 'bug', assignee_id: null }], next_cursor: null });
      return Response.json({ ...workItem, category: 'bug', assignee_id: owner.user_id, version: 2 });
    }));

    await projectApi.workItems(project.id);
    const listed = await projectApi.workItems(project.id, true);
    await projectApi.createWorkItem(project.id, { title: '新增缺陷', category: 'bug', unassigned: true }, 'work-item-create-v2-01');
    await projectApi.claimWorkItem(project.id, workItem.id, { expected_version: 1 }, 'work-item-claim-v2-01');

    expect(listed[0]).toMatchObject({ category: 'bug', assignee_id: null });
    expect(new URL(requests[0]!.url).searchParams.has('include_unassigned')).toBe(false);
    expect(new URL(requests[1]!.url).searchParams.get('include_unassigned')).toBe('true');
    expect(requests.map((request) => [request.method, new URL(request.url).pathname])).toEqual([
      ['GET', `/api/projects/${project.id}/work-items`],
      ['GET', `/api/projects/${project.id}/work-items`],
      ['POST', `/api/projects/${project.id}/work-items`],
      ['POST', `/api/projects/${project.id}/work-items/${workItem.id}/claim`],
    ]);
    expect(requests[2]?.headers.get('Idempotency-Key')).toBe('work-item-create-v2-01');
    expect(await requests[2]?.clone().json()).toMatchObject({ category: 'bug', unassigned: true });
    expect(requests[3]?.headers.get('Idempotency-Key')).toBe('work-item-claim-v2-01');
    expect(await requests[3]?.clone().json()).toEqual({ expected_version: 1 });
  });

  it('loads server state, keeps unknown usage visible, and creates a project through the API', async () => {
    const create = vi.spyOn(projectApi, 'createProject').mockResolvedValue({ ...project, id: 'jcreated1', name: '新客户项目' });
    const user = userEvent.setup();
    renderProjects();

    expect(await screen.findByRole('heading', { name: '我的项目' })).toBeInTheDocument();
    await openProject();
    expect(screen.queryByRole('heading', { name: 'Muse 项目能力演示' })).not.toBeInTheDocument();
    expect(await screen.findByRole('heading', { name: '验证发布' })).toBeInTheDocument();
    expect(screen.getByText(/存在未知费用/)).toBeInTheDocument();
    expect(screen.getByText('unknown')).toBeInTheDocument();
    expect(projectApi.projects).toHaveBeenCalled();
    expect(projectApi.getProject).toHaveBeenCalledWith(project.id);

    await user.click(screen.getByRole('button', { name: '← 返回项目' }));
    await user.click(screen.getByRole('button', { name: '＋ 创建项目' }));
    await user.type(screen.getByLabelText('新项目名称'), '新客户项目');
    await user.type(screen.getByLabelText('项目说明'), '试点协作');
    await user.click(screen.getByRole('button', { name: '创建项目' }));
    await screen.findByText('服务端已确认创建项目。');
    expect(create).toHaveBeenCalledWith({ name: '新客户项目', description: '试点协作' }, expect.any(String));
  });

  it('opens a project from the list and returns without changing project data', async () => {
    const user = userEvent.setup();
    const create = vi.spyOn(projectApi, 'createProject');
    renderProjects();

    expect(await screen.findByRole('heading', { name: '我的项目' })).toBeInTheDocument();
    await user.click(await screen.findByRole('button', { name: '查看项目 交付协作' }));
    expect(await screen.findByRole('region', { name: '项目详情' })).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: '我的项目' })).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '← 返回项目' }));
    expect(screen.getByRole('heading', { name: '我的项目' })).toBeInTheDocument();
    expect(create).not.toHaveBeenCalled();
  });

  it('creates a shared project conversation and selects the returned conversation', async () => {
    let sharedConversations: ProjectConversation[] = [];
    vi.spyOn(projectApi, 'projectConversations').mockImplementation(async () => sharedConversations);
    const created = { ...projectConversation, id: 'cnew001', title: '上线排查' };
    const create = vi.spyOn(projectApi, 'createProjectConversation').mockImplementation(async () => {
      sharedConversations = [created];
      return created;
    });
    const user = userEvent.setup();
    renderProjects();
    await openProject();

    await screen.findByRole('heading', { name: '项目共享 AI 对话' });
    await user.type(screen.getByLabelText('新共享对话标题（可选）'), '上线排查');
    await user.click(screen.getByRole('button', { name: '创建共享对话' }));
    await screen.findByText('服务端已创建项目共享对话；尚未启动 Task。');
    expect(create).toHaveBeenCalledWith(project.id, { title: '上线排查' }, expect.any(String));
    expect(await screen.findByRole('option', { name: '上线排查' })).toBeInTheDocument();
  });

  it('shows shared history actor, delivery state, and any known Task status', async () => {
    renderProjects();
    await openProject();
    const message = await screen.findByRole('article', { name: '项目对话消息' });
    expect(within(message).getByText(`执行者 ${owner.user_id} · 对话创建者 ${projectConversation.owner_id}`)).toBeInTheDocument();
    expect(within(message).getByText('已发送')).toBeInTheDocument();
    expect(within(message).getByText('Task qhistory1 · 状态 未知')).toBeInTheDocument();
    expect(within(message).getByText('上一位成员提交的提示')).toBeInTheDocument();
  });

  it('shows completed Task step counts beside the related project message', async () => {
    vi.mocked(projectApi.tasks).mockResolvedValueOnce([{ ...task, step_count: 4, completed_step_count: 2 }]);
    vi.mocked(projectApi.projectConversationMessages).mockResolvedValueOnce([{ ...projectHistoryMessage, task_id: task.id }]);
    renderProjects();
    await openProject();
    expect(await screen.findByText(/Task t0000001 · 状态 completed · 已完成 2 \/ 4 步/)).toBeInTheDocument();
  });

  it('renders combined project replies by source and links each segment to its member message', async () => {
    const origin: ProjectConversationMessage = { ...projectHistoryMessage, id: 'morigin01', sequence: 4,
      role: 'user', task_id: undefined, content: '检查第一项交付' };
    const combined: ProjectConversationMessage = { ...projectHistoryMessage, id: 'mreply01', sequence: 6,
      role: 'assistant', task_id: undefined, content: '第一项已完成。\n\n第二项仍在进行。', sources: [
        { user_message_id: origin.id, task_id: 'qtask001', task_revision: 2, event_sequence: 7, content: '第一项已完成。' },
        { user_message_id: 'munknown1', task_id: 'qtask002', task_revision: 1, event_sequence: 3, content: '第二项仍在进行。' },
      ] };
    vi.mocked(projectApi.projectConversationMessages).mockResolvedValueOnce([origin, combined]);
    renderProjects();
    await openProject();
    const reply = (await screen.findAllByRole('article', { name: '项目对话消息' }))[1]!;
    expect(within(reply).getByRole('link', { name: '查看关联的成员消息：检查第一项交付' }))
      .toHaveAttribute('href', '#project-conversation-message-morigin01');
    expect(within(reply).getByText('任务 qtask001 · 修订 2 · 结果事件 7')).toBeInTheDocument();
    expect(within(reply).getByText('关联消息 munknown1')).toBeInTheDocument();
    expect(within(reply).getByText('第二项仍在进行。')).toBeInTheDocument();
  });

  it('keeps the full project reply when source segments do not match its content', async () => {
    vi.mocked(projectApi.projectConversationMessages).mockResolvedValueOnce([{
      ...projectHistoryMessage, id: 'mreply02', role: 'assistant', task_id: undefined,
      content: '完整的服务端回复。', sources: [
        { user_message_id: 'munknown1', task_id: 'qtask001', task_revision: 1, event_sequence: 2, content: '旧的分段内容。' },
      ],
    }]);
    renderProjects();
    await openProject();
    const reply = await screen.findByRole('article', { name: '项目对话消息' });
    expect(within(reply).getByText('完整的服务端回复。')).toBeInTheDocument();
    expect(within(reply).getByText('关联消息 munknown1 · 任务 qtask001 · 修订 1 · 结果事件 2')).toBeInTheDocument();
    expect(within(reply).queryByText('旧的分段内容。')).not.toBeInTheDocument();
  });

  it('shows a project routing failure instead of an agent delivery status', async () => {
    vi.mocked(projectApi.projectConversationMessages).mockResolvedValueOnce([{ ...projectHistoryMessage,
      task_id: undefined, routing_status: 'failed', delivery_status: 'pending' }]);
    renderProjects();
    await openProject();
    const message = await screen.findByRole('article', { name: '项目对话消息' });
    expect(within(message).getByText('消息处理失败')).toBeInTheDocument();
  });

  it('shows persisted streaming and interrupted project replies in their conversation order', async () => {
    vi.mocked(projectApi.projectConversationMessages).mockResolvedValue([
      { ...projectHistoryMessage, id: 'mstream1', sequence: 3, role: 'assistant', content: '正在回复',
        output_status: 'streaming', output_task_revision: 1 },
      { ...projectHistoryMessage, id: 'minterrupt1', sequence: 2, role: 'assistant', content: '先前片段',
        output_status: 'interrupted', output_task_revision: 0 },
    ]);
    renderProjects();
    await openProject();
    const messages = await screen.findAllByRole('article', { name: '项目对话消息' });
    expect(messages[0]).toHaveTextContent('先前片段');
    expect(messages[0]).toHaveTextContent('回复已中断');
    expect(messages[1]).toHaveTextContent('正在回复');
    expect(messages[1]).toHaveTextContent('正在生成');
  });

  it('shows speech only for final assistant replies and scopes it to the project conversation', async () => {
    vi.mocked(projectApi.projectConversationMessages).mockResolvedValueOnce([
      { ...projectHistoryMessage, id: 'mfinal01', sequence: 1, role: 'assistant', content: '最终项目回复', output_status: 'completed' },
      { ...projectHistoryMessage, id: 'mstream2', sequence: 2, role: 'assistant', content: '生成中的回复', output_status: 'streaming' },
      { ...projectHistoryMessage, id: 'minter2', sequence: 3, role: 'assistant', content: '中断的回复', output_status: 'interrupted' },
      { ...projectHistoryMessage, id: 'muser02', sequence: 4, role: 'user', content: '成员提示', output_status: 'completed' },
    ]);
    renderProjects();
    await openProject();

    const messages = await screen.findAllByRole('article', { name: '项目对话消息' });
    expect(within(messages[0]!).getByRole('button', { name: '朗读回复' }))
      .toHaveAttribute('data-session-key', `project:${project.id}:conversation:${projectConversation.id}`);
    expect(within(messages[1]!).queryByRole('button', { name: '朗读回复' })).not.toBeInTheDocument();
    expect(within(messages[2]!).queryByRole('button', { name: '朗读回复' })).not.toBeInTheDocument();
    expect(within(messages[3]!).queryByRole('button', { name: '朗读回复' })).not.toBeInTheDocument();
  });

  it('renders newest-first project history pages in sequence order', async () => {
    vi.mocked(projectApi.projectConversationMessages).mockResolvedValue([newerProjectHistoryMessage, projectHistoryMessage]);
    renderProjects();
    await openProject();

    const messages = await screen.findAllByRole('article', { name: '项目对话消息' });
    expect(messages).toHaveLength(2);
    expect(messages[0]).toHaveTextContent('上一位成员提交的提示');
    expect(messages[1]).toHaveTextContent('较新的项目提示');
  });

  it('keeps shared-history loading, error, and empty states distinct', async () => {
    let resolveMessages!: (messages: ProjectConversationMessage[]) => void;
    vi.spyOn(projectApi, 'projectConversationMessages').mockImplementation(() => new Promise((resolve) => { resolveMessages = resolve; }));
    renderProjects();
    await openProject();
    expect(await screen.findByText('正在读取共享历史…')).toBeInTheDocument();
    await act(async () => { resolveMessages([]); });
    expect(await screen.findByText('该共享对话暂无消息。')).toBeInTheDocument();

    cleanup();
    vi.spyOn(projectApi, 'projectConversationMessages').mockRejectedValue(new ApiError({ message: '共享历史无权访问', code: 'forbidden', status: 403 }));
    renderProjects();
    await openProject();
    expect(await screen.findByRole('alert')).toHaveTextContent('读取共享历史失败：没有执行此操作的权限');
  });

  it('accepts a shared project message without expecting an immediate Task', async () => {
    const append = vi.spyOn(projectApi, 'acceptProjectConversationMessage');
    const user = userEvent.setup();
    renderProjects();
    await openProject();

    await screen.findByRole('heading', { name: '项目共享 AI 对话' });
    await user.type(screen.getByLabelText('提交到共享对话的提示'), '检查当前发布状态');
    await user.click(screen.getByRole('button', { name: '发送消息' }));

    expect(await screen.findByText('服务端已受理消息，等待分流。')).toBeInTheDocument();
    expect(append).toHaveBeenCalledWith(project.id, projectConversation.id, expect.objectContaining({
      client_message_id: expect.any(String), model_id: modelConnection.id, content: '检查当前发布状态',
    }), expect.any(String), readyCapabilities.service_instance_id, owner.user_id);
    expect(screen.getByText(/消息受理不代表任务已开始或预算已预留/)).toBeInTheDocument();
    expect(screen.getByText(/此处展示服务端返回的预算与账本状态/)).toBeInTheDocument();
  });

  it('offers explicit voice input in the shared conversation composer without sending automatically', async () => {
    const append = vi.spyOn(projectApi, 'acceptProjectConversationMessage');
    renderProjects();
    await openProject();

    const prompt = await screen.findByLabelText('提交到共享对话的提示');
    expect(screen.getByRole('button', { name: '开始语音输入' })).toBeInTheDocument();
    expect(prompt).toHaveValue('');
    expect(append).not.toHaveBeenCalled();
  });

  it('stores successive project messages and accepts them in order', async () => {
    let finishFirst: (() => void) | undefined;
    const append = vi.mocked(projectApi.acceptProjectConversationMessage).mockImplementation(async (_id, conversationID, body) => {
      if (body.content === '第一条') await new Promise<void>((resolve) => { finishFirst = resolve; });
      return { message: { ...projectMessageReceipt.message, id: `m${body.content}`, conversation_id: conversationID,
        client_message_id: body.client_message_id, content: body.content }, replayed: false };
    });
    const user = userEvent.setup();
    renderProjects();
    await openProject();
    const prompt = await screen.findByLabelText('提交到共享对话的提示');
    await user.type(prompt, '第一条');
    await user.click(screen.getByRole('button', { name: '发送消息' }));
    await waitFor(() => expect(append).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(prompt).toHaveValue(''));
    await user.type(prompt, '第二条');
    await user.click(screen.getByRole('button', { name: '发送消息' }));
    await screen.findByText('第二条');
    expect(append).toHaveBeenCalledTimes(1);
    finishFirst?.();
    await waitFor(() => expect(append).toHaveBeenCalledTimes(2));
    expect(append.mock.calls[1]?.[2].previous_client_message_id).toBe(append.mock.calls[0]?.[2].client_message_id);
    expect(append.mock.calls[1]?.[2].client_message_id).not.toBe(append.mock.calls[0]?.[2].client_message_id);
    expect(prompt).toBeEnabled();
  });

  it('labels the local mock budget ledger as attribution only', async () => {
    vi.stubEnv('VITE_MOCK_API', '1');
    renderProjects();
    await openProject();

    await screen.findByRole('heading', { name: '项目共享 AI 对话' });
    expect(screen.getByText(/本机 MSW 只模拟消息接收/)).toBeInTheDocument();
    expect(screen.getByText(/本机 MSW 只展示合成预算归因/)).toBeInTheDocument();
    expect(screen.queryByText(/消息受理不代表任务已开始或预算已预留/)).not.toBeInTheDocument();
  });

  it('allows sending while project execution capability is unavailable', async () => {
    vi.stubEnv('VITE_MOCK_API', '0');
    vi.spyOn(portalApi, 'capabilities').mockResolvedValue({ ...readyCapabilities, supported_features: ['conversation_auto_routing_v1'] });
    const user = userEvent.setup();
    renderProjects();
    await openProject();

    await screen.findByRole('heading', { name: '项目共享 AI 对话' });
    expect(screen.getByText(/消息受理不代表任务已开始或预算已预留/)).toBeInTheDocument();
    expect(await screen.findByText(/消息仍可先发送或保存在本机/)).toBeInTheDocument();
    await user.type(screen.getByLabelText('提交到共享对话的提示'), '检查当前发布状态');
    await user.click(screen.getByRole('button', { name: '发送消息' }));
    await waitFor(() => expect(projectApi.acceptProjectConversationMessage).toHaveBeenCalledOnce());
  });

  it('still attempts to send a project message when automatic routing is not advertised', async () => {
    vi.mocked(portalApi.capabilities).mockResolvedValue({ ...readyCapabilities, supported_features: ['project_budget_execution'] });
    const accept = vi.spyOn(projectApi, 'acceptProjectConversationMessage');
    const user = userEvent.setup();
    renderProjects();
    await openProject();
    await user.type(await screen.findByLabelText('提交到共享对话的提示'), '项目待发送消息');
    await user.click(screen.getByRole('button', { name: '发送消息' }));
    await waitFor(() => expect(accept).toHaveBeenCalledOnce());
    expect(await screen.findByText(/服务端尚未确认支持对话自动处理；仍会尝试发送/)).toBeInTheDocument();
    expect(await screen.findByText('服务端已受理消息，等待分流。')).toBeInTheDocument();
    expect(accept.mock.calls[0]?.[2].content).toBe('项目待发送消息');
    await waitFor(() => expect(screen.getByLabelText('提交到共享对话的提示')).toHaveValue(''));
    await user.type(screen.getByLabelText('提交到共享对话的提示'), '下一条项目消息');
    expect(screen.getByRole('button', { name: '发送消息' })).toBeEnabled();
  });

  it('sends explicitly selected project resources without pinning a Workspace revision at receipt', async () => {
    const workspaceID = 'w1234567';
    const hostID = 'h1234567';
    const revision = 6;
    const host = {
      id: hostID, name: '可用开发机', owner_id: owner.user_id, template_id: 'std-2c4g', isolation: 'container',
      state: 'running', state_reason: '', runtime_generation: 1, control_version: 2, control_state: 'ai', url: 'https://host.example.invalid',
      workspaces: [{ workspace: { id: workspaceID, name: '项目空间', revision, used_bytes: 32, state: 'ready' },
        directory: workspaceID, state: 'active', binding_version: 1, desired_revision: revision, applied_revision: revision, sync_state: 'healthy', sync_conflicts: [] }],
      resources: { cpu_milli: 1000, mem_mib: 2048, cpu_limit_milli: 2000, mem_limit_mib: 4096 },
      expires_at: null, last_active_at: null, created_at: '2026-11-02T00:00:00Z',
    } satisfies components['schemas']['Host'];
    const workspace = {
      id: workspaceID, name: '项目空间', revision, used_bytes: 32, quota_bytes: 1000, is_read_only: false, state: 'ready',
      created_at: '2026-11-02T00:00:00Z', updated_at: '2026-11-02T00:00:00Z',
    } satisfies components['schemas']['Workspace'];
    const resources: ProjectResource[] = [
      { project_id: project.id, kind: 'host', resource_id: hostID, version: 1 },
      { project_id: project.id, kind: 'workspace', resource_id: workspaceID, version: 1 },
    ];
    vi.spyOn(projectApi, 'resources').mockResolvedValue(resources);
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [modelConnection, alternateModelConnection], configuration_status: 'configured' });
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [host] });
    vi.spyOn(portalApi, 'workspaces').mockResolvedValue({ items: [workspace] });
    const append = vi.spyOn(projectApi, 'acceptProjectConversationMessage');
    const user = userEvent.setup();
    renderProjects();
    await openProject();

    await screen.findByRole('heading', { name: '项目共享 AI 对话' });
    await user.selectOptions(screen.getByLabelText('文本模型'), alternateModelConnection.id);
    await user.selectOptions(screen.getByLabelText('目标电脑（可选）'), hostID);
    await user.selectOptions(screen.getByLabelText('输入 Workspace（可选）'), workspaceID);
    await user.type(screen.getByLabelText('提交到共享对话的提示'), '检查当前发布状态');
    await user.click(screen.getByRole('button', { name: '发送消息' }));

    await screen.findByText('服务端已受理消息，等待分流。');
    expect(append).toHaveBeenCalledWith(project.id, projectConversation.id, expect.objectContaining({
      client_message_id: expect.any(String), model_id: alternateModelConnection.id, content: '检查当前发布状态', host_id: hostID, workspace_id: workspaceID,
    }), expect.any(String), readyCapabilities.service_instance_id, owner.user_id);
    expect(append.mock.calls[0]?.[2]).not.toHaveProperty('input_revision');
  });

  it('shows only sanitized, permission-gated Workspace directory metadata', async () => {
    const workspaceID = 'w1234567';
    vi.spyOn(projectApi, 'resources').mockResolvedValue([
      { project_id: project.id, kind: 'workspace', resource_id: workspaceID, version: 1 },
    ]);
    const readManifest = vi.spyOn(projectApi, 'workspaceManifest')
      .mockResolvedValueOnce({
        project_id: project.id, workspace_id: workspaceID, revision: 6,
        files: [
          { path: 'src/main.go', kind: 'file', size_bytes: 2048, mode: 420 },
          { path: 'src/current', kind: 'symlink', size_bytes: 7, mode: 0 },
        ], empty_directories: [], next_cursor: 'next-page',
      })
      .mockResolvedValueOnce({
        project_id: project.id, workspace_id: workspaceID, revision: 6,
        files: [{ path: 'docs/README.md', kind: 'file', size_bytes: 12, mode: 420 }], empty_directories: ['docs/empty'], next_cursor: null,
      });
    const user = userEvent.setup();
    renderProjects();
    await openProject();

    await user.click(await screen.findByRole('button', { name: '查看文件目录' }));
    expect(await screen.findByText('src/main.go')).toBeInTheDocument();
    expect(screen.getByText('src/current').parentElement).toHaveTextContent('符号链接');
    expect(screen.queryByText('../.env')).not.toBeInTheDocument();
    expect(screen.getByText(/已确认版本 v6/)).toBeInTheDocument();
    expect(readManifest).toHaveBeenNthCalledWith(1, project.id, workspaceID, { limit: 100 });

    await user.click(screen.getByRole('button', { name: '下一页' }));
    expect(await screen.findByText('docs/README.md')).toBeInTheDocument();
    expect(screen.getByText('docs/empty/')).toBeInTheDocument();
    expect(readManifest).toHaveBeenNthCalledWith(2, project.id, workspaceID, { limit: 100, revision: 6, cursor: 'next-page' });
    expect(screen.queryByText(/sha256|chunks/i)).not.toBeInTheDocument();
  });

  it('retries an uncertain project message with the same exact body and idempotency key', async () => {
    const append = vi.spyOn(projectApi, 'acceptProjectConversationMessage')
      .mockRejectedValueOnce(new ApiError({ message: '连接中断', code: 'network_error', status: 503 }))
      .mockImplementationOnce(async (_id, conversationID, body) => ({ message: {
        ...projectMessageReceipt.message, conversation_id: conversationID, client_message_id: body.client_message_id, content: body.content,
      }, replayed: false }));
    const user = userEvent.setup();
    renderProjects();
    await openProject();

    await screen.findByRole('heading', { name: '项目共享 AI 对话' });
    const prompt = screen.getByLabelText('提交到共享对话的提示');
    await user.type(prompt, '检查当前发布状态');
    await user.click(screen.getByRole('button', { name: '发送消息' }));

    expect(await screen.findByRole('button', { name: '核对并重试' })).toBeInTheDocument();
    expect(prompt).toHaveValue('');
    expect(prompt).toBeEnabled();
    await user.click(screen.getByRole('button', { name: '核对并重试' }));
    await screen.findByText('服务端已受理消息，等待分流。');
    expect(append).toHaveBeenCalledTimes(2);
    expect(append.mock.calls[1]?.[0]).toBe(append.mock.calls[0]?.[0]);
    expect(append.mock.calls[1]?.[1]).toBe(append.mock.calls[0]?.[1]);
    expect(append.mock.calls[1]?.[2]).toEqual(append.mock.calls[0]?.[2]);
    expect(append.mock.calls[1]?.[3]).toBe(append.mock.calls[0]?.[3]);
  });

  it('keeps exact retry available if the execution capability disappears after an uncertain receipt', async () => {
    vi.stubEnv('VITE_MOCK_API', '0');
    const capabilities = vi.spyOn(portalApi, 'capabilities').mockResolvedValue(readyCapabilities);
    const append = vi.spyOn(projectApi, 'acceptProjectConversationMessage')
      .mockRejectedValueOnce(new ApiError({ message: '连接中断', code: 'network_error', status: 503 }))
      .mockImplementationOnce(async (_id, conversationID, body) => ({ message: {
        ...projectMessageReceipt.message, conversation_id: conversationID, client_message_id: body.client_message_id, content: body.content,
      }, replayed: false }));
    const user = userEvent.setup();
    const { client } = renderProjects();
    await openProject();

    await screen.findByRole('heading', { name: '项目共享 AI 对话' });
    const prompt = screen.getByLabelText('提交到共享对话的提示');
    await user.type(prompt, '检查当前发布状态');
    await user.click(screen.getByRole('button', { name: '发送消息' }));
    const retry = await screen.findByRole('button', { name: '核对并重试' });
    expect(retry).toBeEnabled();

    capabilities.mockResolvedValue({ ...readyCapabilities, supported_features: ['conversation_auto_routing_v1'] });
    await client.invalidateQueries({ queryKey: ['service-capabilities'] });
    expect(await screen.findByText(/消息仍可先发送或保存在本机/)).toBeInTheDocument();
    expect(retry).toBeEnabled();

    await user.click(retry);
    await screen.findByText('服务端已受理消息，等待分流。');
    expect(append).toHaveBeenCalledTimes(2);
    expect(append.mock.calls[1]?.[2]).toEqual(append.mock.calls[0]?.[2]);
    expect(append.mock.calls[1]?.[3]).toBe(append.mock.calls[0]?.[3]);
  });

  it('accepts incoming invitations, creates and revokes outgoing invitations, and transfers ownership', async () => {
    const accept = vi.spyOn(projectApi, 'acceptInvitation');
    const invite = vi.spyOn(projectApi, 'inviteMember');
    const revoke = vi.spyOn(projectApi, 'revokeInvitation');
    const transfer = vi.spyOn(projectApi, 'transferOwner');
    const user = userEvent.setup();
    renderProjects();

    const incoming = (await screen.findByRole('heading', { name: `项目 ${project.id}` })).closest('article');
    expect(incoming).not.toBeNull();
    await user.click(within(incoming!).getByRole('button', { name: '接受邀请' }));
    await screen.findByText('服务端已确认接受邀请并建立成员关系。');
    expect(accept).toHaveBeenCalledWith(invitation.id, expect.any(String));
    await openProject();

    await user.type(screen.getByLabelText('被邀请账号 ID'), 'uuser001');
    await user.click(screen.getByRole('button', { name: '创建邀请' }));
    await screen.findByText('服务端已创建待接受邀请；这不会发送邮件。');
    expect(invite).toHaveBeenCalledWith(project.id, { user_id: 'uuser001', role: 'member', expected_version: project.version }, expect.any(String));
    const outgoing = await screen.findByText('uuser001');
    const outgoingCard = outgoing.closest('article');
    expect(outgoingCard).not.toBeNull();
    await user.click(within(outgoingCard!).getByRole('button', { name: '撤回邀请' }));
    await screen.findByText('服务端已确认撤回邀请。');
    expect(revoke).toHaveBeenCalledWith(project.id, invitation.id, { expected_version: invitation.version }, expect.any(String));

    await user.selectOptions(screen.getByLabelText('移交负责人给已接受成员'), teammate.user_id);
    await user.click(screen.getByRole('button', { name: '移交负责人' }));
    await screen.findByText('服务端已确认负责人移交。');
    expect(transfer).toHaveBeenCalledWith(project.id, { user_id: teammate.user_id, expected_version: project.version }, expect.any(String));
  });

  it('sends exact budget and work item requests, and uses server state for block, resume, Task link, and delivery', async () => {
    let currentWorkItems = [workItem];
    vi.spyOn(projectApi, 'workItems').mockImplementation(async () => currentWorkItems);
    const setBudget = vi.spyOn(projectApi, 'setBudget').mockResolvedValue({ ...budget, limit_micros: 12_500_000, version: 3 });
    const createWorkItem = vi.spyOn(projectApi, 'createWorkItem').mockImplementation(async (_id, body) => {
      const created: ProjectWorkItem = { ...workItem, ...body, id: 'wcreated1', version: 1 };
      currentWorkItems = [...currentWorkItems, created];
      return created;
    });
    const block = vi.spyOn(projectApi, 'blockWorkItem').mockImplementation(async (_id, _workItemID, body) => {
      currentWorkItems = [{ ...currentWorkItems[0]!, state: 'blocked', blocked_reason: body.blocked_reason, version: 2 }];
      return currentWorkItems[0]!;
    });
    const resume = vi.spyOn(projectApi, 'resumeWorkItem').mockImplementation(async () => {
      currentWorkItems = [{ ...currentWorkItems[0]!, state: 'open', blocked_reason: '', version: 3 }];
      return currentWorkItems[0]!;
    });
    const link = vi.spyOn(projectApi, 'linkWorkItemTask').mockImplementation(async (_id, _workItemID, body) => {
      currentWorkItems = [{ ...currentWorkItems[0]!, task_ids: [body.task_id], version: 4 }];
      return currentWorkItems[0]!;
    });
    const delivery = vi.spyOn(projectApi, 'deliverWorkItem').mockRejectedValue(new ApiError({ message: '交付校验暂不可用', code: 'not_implemented', status: 501 }));
    const user = userEvent.setup();
    renderProjects();
    await openProject();

    await screen.findByRole('heading', { name: '验证发布' });
    await user.clear(screen.getByLabelText('预算限额（主币种单位，最多 6 位小数）'));
    await user.type(screen.getByLabelText('预算限额（主币种单位，最多 6 位小数）'), '12.5');
    await user.click(screen.getByRole('button', { name: '保存预算' }));
    await screen.findByText('服务端已确认预算更新。');
    expect(setBudget).toHaveBeenCalledWith(project.id, { currency: 'USD', limit_micros: 12_500_000, expected_version: budget.version }, expect.any(String));

    await user.type(screen.getByLabelText('工作项标题'), '部署验收');
    expect(screen.queryByLabelText('类型')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '待认领' })).not.toBeInTheDocument();
    await user.type(screen.getByLabelText('说明'), '确认已保存版本');
    await user.type(screen.getByLabelText('验收标准（每行一项）'), '版本可读取');
    await user.click(screen.getByRole('button', { name: '创建工作项' }));
    await screen.findByRole('heading', { name: '部署验收' });
    expect(createWorkItem).toHaveBeenCalledWith(project.id, { title: '部署验收', description: '确认已保存版本', acceptance_criteria: ['版本可读取'] }, expect.any(String));

    const itemCard = screen.getByRole('heading', { name: '验证发布' }).closest('article');
    expect(itemCard).not.toBeNull();
    await user.type(within(itemCard!).getByLabelText('阻塞原因'), '等待审批');
    await user.click(within(itemCard!).getByRole('button', { name: '阻塞工作项' }));
    await screen.findByText('服务端已确认工作项进入阻塞状态。');
    expect(block).toHaveBeenCalledWith(project.id, workItem.id, { blocked_reason: '等待审批', expected_version: workItem.version }, expect.any(String));
    await user.click(within(itemCard!).getByRole('button', { name: '恢复工作项' }));
    await screen.findByText('服务端已确认恢复工作项。');
    expect(resume).toHaveBeenCalledWith(project.id, workItem.id, { expected_version: 2 }, expect.any(String));

    await user.selectOptions(within(itemCard!).getByLabelText('关联已有 Task'), task.id);
    await user.click(within(itemCard!).getByRole('button', { name: '关联 Task' }));
    await screen.findByText('服务端已确认 Task 关联。');
    expect(link).toHaveBeenCalledWith(project.id, workItem.id, { task_id: task.id, expected_version: 3 }, expect.any(String));

    await user.type(within(itemCard!).getByLabelText('已保存 Workspace ID'), 'w0000001');
    await user.type(within(itemCard!).getByLabelText('已保存版本号'), '7');
    await user.click(within(itemCard!).getByRole('button', { name: '提交交付证据' }));
    await screen.findByText(/交付校验暂不可用/);
    expect(delivery).toHaveBeenCalledWith(project.id, workItem.id, { task_id: task.id, workspace_id: 'w0000001', revision: 7, expected_version: 4 }, expect.any(String));
    expect(within(itemCard!).getByText('开放')).toBeInTheDocument();
    expect(within(itemCard!).queryByText(/服务端交付证据/)).not.toBeInTheDocument();
  });

  it('enables category filters and claims only returned unassigned work items when v2 is advertised', async () => {
    vi.mocked(portalApi.capabilities).mockResolvedValue({
      ...readyCapabilities, supported_features: [...readyCapabilities.supported_features, 'project_work_items_v2'],
    });
    const unclaimedBug: ProjectWorkItem = { ...workItem, id: 'wbug001', title: '待认领缺陷', category: 'bug', assignee_id: null };
    const requestItem: ProjectWorkItem = { ...workItem, id: 'wreq001', title: '普通开放需求', category: 'request' };
    const deliveredItem: ProjectWorkItem = { ...workItem, id: 'wdel001', title: '已交付需求', category: 'request', state: 'delivered' };
    vi.mocked(projectApi.workItems).mockResolvedValue([workItem, unclaimedBug, requestItem, deliveredItem]);
    const create = vi.mocked(projectApi.createWorkItem).mockResolvedValue({
      ...workItem, id: 'wcreated1', title: '新需求', category: 'request', assignee_id: null,
    });
    const claim = vi.mocked(projectApi.claimWorkItem).mockResolvedValue({
      ...unclaimedBug, assignee_id: owner.user_id, version: 2,
    });
    const user = userEvent.setup();
    renderProjects();
    await openProject();

    expect(await screen.findByRole('button', { name: '待认领' })).toBeInTheDocument();
    await waitFor(() => expect(projectApi.workItems).toHaveBeenCalledWith(project.id, true));
    expect(screen.getByText('已交付')).toBeInTheDocument();
    expect(screen.queryByText('待验收')).not.toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: '需求' }));
    expect(await screen.findByRole('heading', { name: '普通开放需求' })).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: '待认领缺陷' })).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '待认领' }));
    const unclaimedCard = screen.getByRole('heading', { name: '待认领缺陷' }).closest('article');
    expect(unclaimedCard).not.toBeNull();
    expect(screen.queryByRole('heading', { name: '验证发布' })).not.toBeInTheDocument();
    await user.click(within(unclaimedCard!).getByRole('button', { name: '认领工作项' }));
    await screen.findByText('服务端已确认认领工作项。');
    expect(claim).toHaveBeenCalledWith(project.id, unclaimedBug.id, { expected_version: 1 }, expect.any(String));

    await user.click(screen.getByRole('button', { name: '全部' }));
    await user.selectOptions(screen.getByLabelText('类型'), 'request');
    await user.type(screen.getByLabelText('工作项标题'), '新需求');
    await user.click(screen.getByRole('button', { name: '创建工作项' }));
    await screen.findByText('服务端已创建工作项；没有因此启动 Task。');
    expect(create).toHaveBeenCalledWith(project.id, { title: '新需求', category: 'request', unassigned: true }, expect.any(String));
  });

  it('does not offer claim to a viewer even when work-item v2 is available', async () => {
    vi.mocked(portalApi.capabilities).mockResolvedValue({
      ...readyCapabilities, supported_features: [...readyCapabilities.supported_features, 'project_work_items_v2'],
    });
    vi.mocked(projectApi.members).mockResolvedValue([{ ...owner, role: 'viewer' }]);
    vi.mocked(projectApi.workItems).mockResolvedValue([{ ...workItem, id: 'wview001', title: '只读可见的未认领项', category: 'bug', assignee_id: null }]);
    const user = userEvent.setup();
    renderProjects();
    await openProject();

    await user.click(await screen.findByRole('button', { name: '待认领' }));
    const itemCard = screen.getByRole('heading', { name: '只读可见的未认领项' }).closest('article');
    expect(itemCard).not.toBeNull();
    expect(within(itemCard!).queryByRole('button', { name: '认领工作项' })).not.toBeInTheDocument();
  });

  it('keeps mutations pending and unknown until the API settles, then retries with the same idempotency key', async () => {
    let rejectFirst!: (error: Error) => void;
    const create = vi.spyOn(projectApi, 'createWorkItem')
      .mockImplementationOnce(() => new Promise((_resolve, reject) => { rejectFirst = reject; }))
      .mockResolvedValueOnce(workItem);
    const user = userEvent.setup();
    renderProjects();
    await openProject();

    await screen.findByRole('heading', { name: '验证发布' });
    await user.type(screen.getByLabelText('工作项标题'), '待确认任务');
    await user.click(screen.getByRole('button', { name: '创建工作项' }));
    expect(await screen.findByRole('button', { name: '正在创建…' })).toBeDisabled();
    expect(screen.queryByRole('heading', { name: '待确认任务' })).not.toBeInTheDocument();

    await act(async () => { rejectFirst(new ApiError({ message: '连接中断', code: 'network_error', status: 503 })); });
    expect(await screen.findByText(/结果尚未确认：连接中断/)).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: '待确认任务' })).not.toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: '创建工作项' }));
    await screen.findByText('服务端已创建工作项；没有因此启动 Task。');
    await waitFor(() => expect(create).toHaveBeenCalledTimes(2));
    expect(create.mock.calls[0]?.[2]).toBe(create.mock.calls[1]?.[2]);
  });
});
