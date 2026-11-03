import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError } from '../api/client';
import type { components } from '../api/schema';
import { portalApi } from '../app/api';
import { projectApi } from '../app/project-api';
import type { AppendProjectConversationMessageResponse, Project, ProjectBudget, ProjectConversation, ProjectConversationMessage, ProjectInvitation, ProjectMember, ProjectResource, ProjectWorkItem } from '../app/project-api';
import { Projects } from './Projects';

vi.mock('../app/auth', () => ({ useAuth: () => ({ user: { id: 'uowner01' } }) }));

const project: Project = { id: 'jproject1', owner_id: 'uowner01', name: '交付协作', description: '', version: 3 };
const owner: ProjectMember = { project_id: project.id, user_id: project.owner_id, role: 'owner', version: 1 };
const teammate: ProjectMember = { project_id: project.id, user_id: 'umember1', role: 'member', version: 2 };
const projectConversation: ProjectConversation = {
  id: 'cproj001', project_id: project.id, owner_id: teammate.user_id, title: '共享执行计划',
  created_at: '2026-11-02T00:00:00Z', updated_at: '2026-11-02T00:00:00Z',
};
const projectTaskReceipt: AppendProjectConversationMessageResponse = {
  message: {
    id: 'mproj001', conversation_id: projectConversation.id, owner_id: projectConversation.owner_id, actor_id: owner.user_id,
    role: 'user', content: '检查当前发布状态', task_id: 'qproj001', delivery_status: 'applied', created_at: '2026-11-02T00:01:00Z',
  },
  task: {
    id: 'qproj001', owner_id: projectConversation.owner_id, initiator_id: owner.user_id, project_id: project.id,
    execution_location: 'server', conversation_id: projectConversation.id, model_id: 'model-test', objective: '检查当前发布状态',
    status: 'queued', current_step: null, budget: { active_seconds: 1800, tool_calls: 100, model_tokens: 50000 },
    usage: { active_seconds: 0, tool_calls: 0, model_tokens: null }, created_at: '2026-11-02T00:01:00Z', updated_at: '2026-11-02T00:01:00Z',
    event_sequence: 0, result: null, waiting_reason: null, steps: [], batch_items: [], artifacts: [], pending_request: null,
    clarification_request: null, stop_request: null, assistant_message: null, finished_at: null,
  },
};
const projectHistoryMessage: ProjectConversationMessage = {
  ...projectTaskReceipt.message, id: 'mhistory1', task_id: 'qhistory1', actor_id: owner.user_id,
  content: '上一位成员提交的提示', delivery_status: 'applied',
};
const modelConnection: components['schemas']['ModelConnection'] = {
  id: 'model-test', name: '团队文本模型', base_url: 'https://models.example.invalid/v1', model_name: 'test-model',
  shared: true, has_api_key: true, capabilities: { text: true, tools: true, vision: false },
  checked_at: '2026-11-02T00:00:00Z', created_at: '2026-11-02T00:00:00Z', updated_at: '2026-11-02T00:00:00Z',
};
const alternateModelConnection = { ...modelConnection, id: 'model-alt01', name: '备用文本模型', model_name: 'alternate-model' };
const readyCapabilities: Awaited<ReturnType<typeof portalApi.capabilities>> = {
  service_instance_id: '00000000-0000-4000-8000-000000000001', api_version: 'v1', hub_version: null,
  event_version: null, minimum_client_version: null, supported_features: ['project_budget_execution'], default_max_request_bytes: 1_048_576,
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
  assignee_id: owner.user_id, state: 'open', blocked_reason: '', task_ids: [], version: 1,
};
const task = {
  id: 't0000001', owner_id: owner.user_id, initiator_id: owner.user_id, execution_location: 'server' as const, conversation_id: 'c0000001', model_id: 'model-test',
  objective: '验证交付', status: 'completed' as const, current_step: null,
  budget: { active_seconds: 0, tool_calls: 0, model_tokens: 0 }, usage: { active_seconds: 0, tool_calls: 0, model_tokens: 0 },
  created_at: '2026-11-02T00:00:00Z', updated_at: '2026-11-02T00:00:00Z',
};

function renderProjects() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  const result = render(<QueryClientProvider client={client}><MemoryRouter><Projects /></MemoryRouter></QueryClientProvider>);
  return { ...result, client };
}

describe('Projects API integration', () => {
  beforeEach(() => {
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
    vi.spyOn(projectApi, 'appendProjectConversationMessage').mockResolvedValue(projectTaskReceipt);
    vi.spyOn(projectApi, 'budget').mockResolvedValue(budget);
    vi.spyOn(projectApi, 'ledger').mockResolvedValue([{
      call_id: 'call-unknown', actor_id: owner.user_id, source: 'task', task_id: task.id, model_id: task.model_id,
      rate_version: 1, currency: 'USD', status: 'unknown', reserved_usage: { input_tokens: 20, output_tokens: 10 }, reserved_micros: 250_000,
      created_at: '2026-11-02T00:00:00Z',
    }]);
    vi.spyOn(projectApi, 'workItems').mockResolvedValue([workItem]);
    vi.spyOn(projectApi, 'tasks').mockResolvedValue([task]);
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [modelConnection] });
    vi.spyOn(portalApi, 'capabilities').mockResolvedValue(readyCapabilities);
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'workspaces').mockResolvedValue({ items: [] });
  });

  afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); vi.unstubAllEnvs(); });

  it('uses the typed project conversation routes and idempotency headers', async () => {
    vi.mocked(projectApi.createProjectConversation).mockRestore();
    vi.mocked(projectApi.projectConversations).mockRestore();
    vi.mocked(projectApi.projectConversationMessages).mockRestore();
    vi.mocked(projectApi.appendProjectConversationMessage).mockRestore();
    const requests: Request[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = input instanceof Request ? new Request(input, init) : new Request(input, init);
      requests.push(request);
      if (request.method === 'GET' && new URL(request.url).pathname.endsWith('/messages')) {
        return Response.json({ items: [projectHistoryMessage], next_cursor: null });
      }
      if (request.method === 'GET') return Response.json({ items: [projectConversation], next_cursor: null });
      if (new URL(request.url).pathname.endsWith('/messages')) return Response.json(projectTaskReceipt, { status: 202 });
      return Response.json(projectConversation, { status: 201 });
    }));

    await projectApi.createProjectConversation(project.id, { title: projectConversation.title }, 'project-create-key-01');
    await projectApi.projectConversations(project.id);
    await projectApi.projectConversationMessages(project.id, projectConversation.id);
    await projectApi.appendProjectConversationMessage(project.id, projectConversation.id, {
      model_id: modelConnection.id, content: projectTaskReceipt.message.content,
    }, 'project-append-key-01');

    expect(requests.map((request) => [request.method, new URL(request.url).pathname])).toEqual([
      ['POST', `/api/projects/${project.id}/conversations`],
      ['GET', `/api/projects/${project.id}/conversations`],
      ['GET', `/api/projects/${project.id}/conversations/${projectConversation.id}/messages`],
      ['POST', `/api/projects/${project.id}/conversations/${projectConversation.id}/messages`],
    ]);
    expect(requests[0]?.headers.get('Idempotency-Key')).toBe('project-create-key-01');
    expect(requests[3]?.headers.get('Idempotency-Key')).toBe('project-append-key-01');
    expect(await requests[3]?.clone().json()).toEqual({ model_id: modelConnection.id, content: projectTaskReceipt.message.content });
  });

  it('loads server state, keeps unknown usage visible, and creates a project through the API', async () => {
    const create = vi.spyOn(projectApi, 'createProject').mockResolvedValue({ ...project, id: 'jcreated1', name: '新客户项目' });
    const user = userEvent.setup();
    renderProjects();

    expect(await screen.findByRole('heading', { name: '项目' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Muse 项目能力演示' })).toBeInTheDocument();
    expect(screen.getAllByRole('note').some((note) => /本地演示.*刷新即丢.*不连接服务/.test(note.textContent ?? ''))).toBe(true);
    expect(await screen.findByRole('heading', { name: '验证发布' })).toBeInTheDocument();
    expect(screen.getByText(/存在未知费用/)).toBeInTheDocument();
    expect(screen.getByText('unknown')).toBeInTheDocument();
    expect(projectApi.projects).toHaveBeenCalled();
    expect(projectApi.getProject).toHaveBeenCalledWith(project.id);

    await user.type(screen.getByLabelText('新项目名称'), '新客户项目');
    await user.type(screen.getByLabelText('项目说明'), '试点协作');
    await user.click(screen.getByRole('button', { name: '创建项目' }));
    await screen.findByText('服务端已确认创建项目。');
    expect(create).toHaveBeenCalledWith({ name: '新客户项目', description: '试点协作' }, expect.any(String));
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

    await screen.findByRole('heading', { name: '项目共享 AI 对话' });
    await user.type(screen.getByLabelText('新共享对话标题（可选）'), '上线排查');
    await user.click(screen.getByRole('button', { name: '创建共享对话' }));
    await screen.findByText('服务端已创建项目共享对话；尚未启动 Task。');
    expect(create).toHaveBeenCalledWith(project.id, { title: '上线排查' }, expect.any(String));
    expect(await screen.findByRole('option', { name: '上线排查' })).toBeInTheDocument();
  });

  it('shows shared history actor, delivery state, and any known Task status', async () => {
    renderProjects();
    const message = await screen.findByRole('article', { name: '项目对话消息' });
    expect(within(message).getByText(`执行者 ${owner.user_id} · 对话创建者 ${projectConversation.owner_id}`)).toBeInTheDocument();
    expect(within(message).getByText('applied')).toBeInTheDocument();
    expect(within(message).getByText('Task qhistory1 · 状态 未知')).toBeInTheDocument();
    expect(within(message).getByText('上一位成员提交的提示')).toBeInTheDocument();
  });

  it('keeps shared-history loading, error, and empty states distinct', async () => {
    let resolveMessages!: (messages: ProjectConversationMessage[]) => void;
    vi.spyOn(projectApi, 'projectConversationMessages').mockImplementation(() => new Promise((resolve) => { resolveMessages = resolve; }));
    renderProjects();
    expect(await screen.findByText('正在读取共享历史…')).toBeInTheDocument();
    await act(async () => { resolveMessages([]); });
    expect(await screen.findByText('该共享对话暂无消息。')).toBeInTheDocument();

    cleanup();
    vi.spyOn(projectApi, 'projectConversationMessages').mockRejectedValue(new ApiError({ message: '共享历史无权访问', code: 'forbidden', status: 403 }));
    renderProjects();
    expect(await screen.findByRole('alert')).toHaveTextContent('读取共享历史失败：没有执行此操作的权限');
  });

  it('submits a shared project prompt with a verified model and displays the Task project, initiator, and status', async () => {
    const append = vi.spyOn(projectApi, 'appendProjectConversationMessage');
    const user = userEvent.setup();
    renderProjects();

    await screen.findByRole('heading', { name: '项目共享 AI 对话' });
    await user.type(screen.getByLabelText('提交到共享对话的提示'), '检查当前发布状态');
    await user.click(screen.getByRole('button', { name: '提交并排队项目 Task' }));

    expect(await screen.findByRole('article', { name: '项目 Task 回执' })).toHaveTextContent('qproj001');
    expect(screen.getByRole('article', { name: '项目 Task 回执' })).toHaveTextContent(`项目 ${project.id}`);
    expect(screen.getByRole('article', { name: '项目 Task 回执' })).toHaveTextContent(`发起人 ${owner.user_id}`);
    expect(screen.getByRole('article', { name: '项目 Task 回执' })).toHaveTextContent('queued');
    expect(append).toHaveBeenCalledWith(project.id, projectConversation.id, {
      model_id: modelConnection.id, content: '检查当前发布状态',
    }, expect.any(String));
    expect(screen.getByText(/真实项目 Task 由服务端检查并预留项目预算/)).toBeInTheDocument();
    expect(screen.getByText(/服务端会在模型调用前预留项目预算/)).toBeInTheDocument();
  });

  it('labels the local mock budget ledger as attribution only', async () => {
    vi.stubEnv('VITE_MOCK_API', '1');
    renderProjects();

    await screen.findByRole('heading', { name: '项目共享 AI 对话' });
    expect(screen.getByText(/本机 MSW 只展示预算归因并返回合成的 queued Task 回执/)).toBeInTheDocument();
    expect(screen.getByText(/本机 MSW 只展示合成预算归因/)).toBeInTheDocument();
    expect(screen.queryByText(/真实项目 Task 由服务端检查并预留项目预算/)).not.toBeInTheDocument();
  });

  it('fails closed for live Task submission when the backend has not advertised project execution', async () => {
    vi.stubEnv('VITE_MOCK_API', '0');
    vi.spyOn(portalApi, 'capabilities').mockResolvedValue({ ...readyCapabilities, supported_features: [] });
    const user = userEvent.setup();
    renderProjects();

    await screen.findByRole('heading', { name: '项目共享 AI 对话' });
    expect(screen.getByText(/真实项目 Task 由服务端检查并预留项目预算/)).toBeInTheDocument();
    expect(await screen.findByText(/真实项目 Task 提交已停用/)).toBeInTheDocument();
    await user.type(screen.getByLabelText('提交到共享对话的提示'), '检查当前发布状态');
    expect(screen.getByRole('button', { name: '项目 Task 执行未就绪' })).toBeDisabled();
    expect(projectApi.appendProjectConversationMessage).not.toHaveBeenCalled();
  });

  it('only offers project-linked, directly accessible resources and pins the selected Workspace revision', async () => {
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
      id: workspaceID, name: '项目空间', revision, used_bytes: 32, quota_bytes: 1000, state: 'ready',
      created_at: '2026-11-02T00:00:00Z', updated_at: '2026-11-02T00:00:00Z',
    } satisfies components['schemas']['Workspace'];
    const resources: ProjectResource[] = [
      { project_id: project.id, kind: 'host', resource_id: hostID, version: 1 },
      { project_id: project.id, kind: 'workspace', resource_id: workspaceID, version: 1 },
    ];
    vi.spyOn(projectApi, 'resources').mockResolvedValue(resources);
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [modelConnection, alternateModelConnection] });
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [host] });
    vi.spyOn(portalApi, 'workspaces').mockResolvedValue({ items: [workspace] });
    const append = vi.spyOn(projectApi, 'appendProjectConversationMessage');
    const user = userEvent.setup();
    renderProjects();

    await screen.findByRole('heading', { name: '项目共享 AI 对话' });
    await user.selectOptions(screen.getByLabelText('文本模型'), alternateModelConnection.id);
    await user.selectOptions(screen.getByLabelText('目标电脑（可选）'), hostID);
    await user.selectOptions(screen.getByLabelText('输入 Workspace 版本（可选）'), workspaceID);
    await user.type(screen.getByLabelText('提交到共享对话的提示'), '检查当前发布状态');
    await user.click(screen.getByRole('button', { name: '提交并排队项目 Task' }));

    await screen.findByRole('article', { name: '项目 Task 回执' });
    expect(append).toHaveBeenCalledWith(project.id, projectConversation.id, {
      model_id: alternateModelConnection.id, content: '检查当前发布状态', host_id: hostID, workspace_id: workspaceID, input_revision: revision,
    }, expect.any(String));
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

  it('retries an uncertain project append with the same exact body and idempotency key', async () => {
    const append = vi.spyOn(projectApi, 'appendProjectConversationMessage')
      .mockRejectedValueOnce(new ApiError({ message: '连接中断', code: 'network_error', status: 503 }))
      .mockResolvedValueOnce(projectTaskReceipt);
    const user = userEvent.setup();
    renderProjects();

    await screen.findByRole('heading', { name: '项目共享 AI 对话' });
    const prompt = screen.getByLabelText('提交到共享对话的提示');
    await user.type(prompt, '检查当前发布状态');
    await user.click(screen.getByRole('button', { name: '提交并排队项目 Task' }));

    expect(await screen.findByRole('button', { name: '重试同一项目 Task' })).toBeInTheDocument();
    expect(prompt).toHaveValue('检查当前发布状态');
    expect(prompt).toBeDisabled();
    await user.click(screen.getByRole('button', { name: '重试同一项目 Task' }));
    await screen.findByRole('article', { name: '项目 Task 回执' });
    expect(append).toHaveBeenCalledTimes(2);
    expect(append.mock.calls[1]?.[0]).toBe(append.mock.calls[0]?.[0]);
    expect(append.mock.calls[1]?.[1]).toBe(append.mock.calls[0]?.[1]);
    expect(append.mock.calls[1]?.[2]).toEqual(append.mock.calls[0]?.[2]);
    expect(append.mock.calls[1]?.[3]).toBe(append.mock.calls[0]?.[3]);
  });

  it('keeps exact retry available if the execution capability disappears after an uncertain append', async () => {
    vi.stubEnv('VITE_MOCK_API', '0');
    const capabilities = vi.spyOn(portalApi, 'capabilities').mockResolvedValue(readyCapabilities);
    const append = vi.spyOn(projectApi, 'appendProjectConversationMessage')
      .mockRejectedValueOnce(new ApiError({ message: '连接中断', code: 'network_error', status: 503 }))
      .mockResolvedValueOnce(projectTaskReceipt);
    const user = userEvent.setup();
    const { client } = renderProjects();

    await screen.findByRole('heading', { name: '项目共享 AI 对话' });
    const prompt = screen.getByLabelText('提交到共享对话的提示');
    await user.type(prompt, '检查当前发布状态');
    await user.click(screen.getByRole('button', { name: '提交并排队项目 Task' }));
    const retry = await screen.findByRole('button', { name: '重试同一项目 Task' });
    expect(retry).toBeEnabled();

    capabilities.mockResolvedValue({ ...readyCapabilities, supported_features: [] });
    await client.invalidateQueries({ queryKey: ['service-capabilities'] });
    expect(await screen.findByText(/真实项目 Task 提交已停用/)).toBeInTheDocument();
    expect(retry).toBeEnabled();

    await user.click(retry);
    await screen.findByRole('article', { name: '项目 Task 回执' });
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

    await screen.findByRole('heading', { name: '验证发布' });
    await user.clear(screen.getByLabelText('预算限额（主币种单位，最多 6 位小数）'));
    await user.type(screen.getByLabelText('预算限额（主币种单位，最多 6 位小数）'), '12.5');
    await user.click(screen.getByRole('button', { name: '保存预算' }));
    await screen.findByText('服务端已确认预算更新。');
    expect(setBudget).toHaveBeenCalledWith(project.id, { currency: 'USD', limit_micros: 12_500_000, expected_version: budget.version }, expect.any(String));

    await user.type(screen.getByLabelText('工作项标题'), '部署验收');
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

  it('keeps mutations pending and unknown until the API settles, then retries with the same idempotency key', async () => {
    let rejectFirst!: (error: Error) => void;
    const create = vi.spyOn(projectApi, 'createWorkItem')
      .mockImplementationOnce(() => new Promise((_resolve, reject) => { rejectFirst = reject; }))
      .mockResolvedValueOnce(workItem);
    const user = userEvent.setup();
    renderProjects();

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
