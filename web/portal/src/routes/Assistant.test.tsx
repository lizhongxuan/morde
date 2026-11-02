import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiError } from '../api/client';
import { portalApi } from '../app/api';
import { ToastProvider } from '../app/toast';
import { Assistant, assistantRecentConversationKey, mergeTaskAssistantMessage } from './Assistant';

const authUser = vi.hoisted(() => ({ id: 'u1234567' }));
vi.mock('../app/auth', () => ({ useAuth: () => ({ user: authUser }) }));

const timestamp = '2026-10-22T08:00:00.000Z';
type ConversationMessage = Awaited<ReturnType<typeof portalApi.conversationMessages>>['items'][number];
const clientContract = JSON.parse(readFileSync(
  resolve(process.cwd(), '../../server/openapi/fixtures/morde-client-v1.json'), 'utf8',
)) as {
  capabilities: Record<'unsupported' | 'queued_task_api' | 'cloud_execution' | 'unsupported_version',
    Awaited<ReturnType<typeof portalApi.capabilities>>>;
};
const model = {
  id: 'p1234567', name: '团队模型', base_url: 'https://models.example.test/v1', model_name: 'morde-test', shared: true, has_api_key: true,
  capabilities: { text: true, tools: false, vision: false }, checked_at: timestamp, created_at: timestamp, updated_at: timestamp,
} satisfies Awaited<ReturnType<typeof portalApi.modelConnections>>['items'][number];
const otherModel = { ...model, id: 'p7654321', name: '备用模型' };
const targetHost: Awaited<ReturnType<typeof portalApi.hosts>>['items'][number] = {
  id: 'h1234567', name: '测试电脑', owner_id: 'u1234567', template_id: 'std', isolation: 'container', state: 'running', state_reason: '',
  runtime_generation: 1, control_version: 2, control_state: 'ai', url: 'https://h1234567.hosts.test',
  workspaces: [{ workspace: { id: 'w1234567', name: '项目工作区', revision: 5, used_bytes: 20, state: 'ready' },
    directory: 'projects/w1234567', state: 'active', binding_version: 1, desired_revision: 5, applied_revision: 5, sync_state: 'healthy', sync_conflicts: [] }],
  resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 },
  expires_at: null, last_active_at: null, created_at: timestamp,
};
const conversation = { id: 'c1234567', title: '你好', created_at: timestamp, updated_at: timestamp };
const message = { id: 'm1234567', conversation_id: conversation.id, role: 'user' as const, content: '你好', task_id: 'q1234567', created_at: timestamp };
const reply = { id: 'm7654321', conversation_id: conversation.id, role: 'assistant' as const, content: '你好！', task_id: 'q1234567', created_at: timestamp };
const task = {
  id: 'q1234567', owner_id: 'u1234567', execution_location: 'server' as const, conversation_id: conversation.id,
  model_id: model.id, objective: message.content, status: 'completed' as const, event_sequence: 0, current_step: null,
  budget: { active_seconds: 600, tool_calls: 10, model_tokens: 1000 }, usage: { active_seconds: 2, tool_calls: 0, model_tokens: 12 },
  created_at: timestamp, updated_at: timestamp, result: reply.content, waiting_reason: null, steps: [], batch_items: [],
  artifacts: [], pending_request: null, clarification_request: null, stop_request: null, assistant_message: null, finished_at: timestamp,
} satisfies Awaited<ReturnType<typeof portalApi.task>>;

function renderPage(initialState: unknown = null) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const view = render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter initialEntries={[{ pathname: '/assistant', state: initialState }]}><Assistant /></MemoryRouter></ToastProvider></QueryClientProvider>);
  return { ...view, client };
}

function mockCommon(options: { conversations?: typeof conversation[]; messages?: ConversationMessage[]; cloudTasks?: boolean; capabilities?: Awaited<ReturnType<typeof portalApi.capabilities>>; hosts?: typeof targetHost[] } = {}) {
  vi.spyOn(portalApi, 'capabilities').mockResolvedValue(options.capabilities ??
    (options.cloudTasks === false ? clientContract.capabilities.queued_task_api : clientContract.capabilities.cloud_execution));
  vi.spyOn(portalApi, 'conversations').mockResolvedValue({ items: options.conversations ?? [], next_cursor: null });
  vi.spyOn(portalApi, 'conversationMessages').mockResolvedValue({ items: options.messages ?? [], next_cursor: null });
  vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [model, otherModel] });
  vi.spyOn(portalApi, 'preferences').mockResolvedValue({ assistant_name: 'Morde', selected_model_id: model.id, default_host_id: null, updated_at: timestamp });
  vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: options.hosts ?? [] });
  vi.spyOn(portalApi, 'execution').mockResolvedValue({ owner_id: 'u1234567', state: 'enabled', epoch: 1, pending_task_count: 0,
    runtime: { state: 'running', runtime_id: 'rt1234567', runtime_generation: 1, max_concurrent_tasks: 4, active_task_count: 0 }, updated_at: timestamp });
}

describe('Assistant durable tasks', () => {
  afterEach(() => { cleanup(); localStorage.clear(); authUser.id = 'u1234567'; vi.restoreAllMocks(); });

  it('restores the last conversation and its Task for the current service account', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    const serviceInstanceID = clientContract.capabilities.cloud_execution.service_instance_id;
    localStorage.setItem(assistantRecentConversationKey(serviceInstanceID, authUser.id), conversation.id);
    const getTask = vi.spyOn(portalApi, 'task').mockResolvedValue(task);

    renderPage();

    expect(await screen.findByRole('heading', { name: '你好' })).toBeInTheDocument();
    await waitFor(() => expect(getTask).toHaveBeenCalledWith(task.id));
    expect(await screen.findByText('你好！')).toBeInTheDocument();
  });

  it('does not restore another account’s recent conversation', async () => {
    const serviceInstanceID = clientContract.capabilities.cloud_execution.service_instance_id;
    localStorage.setItem(assistantRecentConversationKey(serviceInstanceID, 'u1234567'), conversation.id);
    authUser.id = 'u7654321';
    mockCommon({ conversations: [conversation], messages: [message] });

    renderPage();

    expect(await screen.findByRole('heading', { name: '今天想聊些什么？' })).toBeInTheDocument();
    expect(portalApi.conversationMessages).not.toHaveBeenCalled();
  });

  it('prefills an explicitly selected Idea without putting its text in the URL or sending it', async () => {
    mockCommon();
    const submit = vi.spyOn(portalApi, 'submitTask');
    renderPage({ ideaID: 'idea-1', ideaPrompt: 'Draft a project outline' });

    expect(await screen.findByRole('textbox', { name: '消息内容' })).toHaveValue('Draft a project outline');
    expect(submit).not.toHaveBeenCalled();
  });

  it('does not restore a conversation from another service instance', async () => {
    const previousServiceID = clientContract.capabilities.cloud_execution.service_instance_id;
    const currentServiceID = '00000000-0000-4000-8000-000000000002';
    localStorage.setItem(assistantRecentConversationKey(previousServiceID, authUser.id), conversation.id);
    mockCommon({ conversations: [conversation], messages: [message], capabilities: {
      ...clientContract.capabilities.cloud_execution, service_instance_id: currentServiceID,
    } });

    renderPage();

    expect(await screen.findByRole('heading', { name: '今天想聊些什么？' })).toBeInTheDocument();
    expect(portalApi.conversationMessages).not.toHaveBeenCalled();
  });

  it('clears the recent conversation when the user starts a new one', async () => {
    const serviceInstanceID = clientContract.capabilities.cloud_execution.service_instance_id;
    const key = assistantRecentConversationKey(serviceInstanceID, authUser.id);
    localStorage.setItem(key, conversation.id);
    mockCommon({ conversations: [conversation], messages: [message] });
    vi.spyOn(portalApi, 'task').mockResolvedValue(task);
    const user = userEvent.setup();
    const view = renderPage();

    await screen.findByRole('heading', { name: '你好' });
    await user.click(screen.getByRole('button', { name: '＋ 新对话' }));
    expect(screen.getByRole('heading', { name: '今天想聊些什么？' })).toBeInTheDocument();
    expect(localStorage.getItem(key)).toBeNull();

    view.unmount();
    renderPage();
    expect(await screen.findByRole('heading', { name: '今天想聊些什么？' })).toBeInTheDocument();
  });

  it('submits a Task with the selected Host and applied Workspace revision', async () => {
    mockCommon({ hosts: [targetHost] });
    const conversations = vi.mocked(portalApi.conversations);
    const messages = vi.mocked(portalApi.conversationMessages);
    const acceptedTask = { ...task, host_id: targetHost.id, workspace_id: 'w1234567', input_revision: 5 };
    vi.spyOn(portalApi, 'submitTask').mockImplementation(async (body) => {
      conversations.mockResolvedValue({ items: [conversation], next_cursor: null });
      messages.mockResolvedValue({ items: [{ ...message, content: body.objective, task_id: acceptedTask.id }], next_cursor: null });
      return acceptedTask;
    });
    vi.spyOn(portalApi, 'task').mockResolvedValue(acceptedTask);
    const user = userEvent.setup();
    renderPage();

    await screen.findByRole('option', { name: /测试电脑/ });
    await user.selectOptions(screen.getByRole('combobox', { name: '选择目标电脑' }), targetHost.id);
    await user.selectOptions(screen.getByRole('combobox', { name: '选择目标 Workspace' }), 'w1234567');
    expect(screen.getByText(/云端 Agent Runtime：运行中/)).toBeInTheDocument();
    await user.type(screen.getByRole('textbox', { name: '消息内容' }), '检查工作区报告');
    await user.click(screen.getByRole('button', { name: /发送/ }));

    await waitFor(() => expect(portalApi.submitTask).toHaveBeenCalledWith({
      model_id: model.id, objective: '检查工作区报告', host_id: targetHost.id, workspace_id: 'w1234567', input_revision: 5,
    }, expect.any(String)));
    expect(await screen.findByText(/当前 Task 的目标已固定：云端 Agent Runtime · 测试电脑（运行中） · 项目工作区 v5/)).toBeInTheDocument();
  });

  it('keeps an accepted Task target fixed when the next-message Host selection changes', async () => {
    mockCommon({ conversations: [conversation], messages: [message], hosts: [targetHost] });
    const acceptedTask = { ...task, host_id: targetHost.id, workspace_id: 'w1234567', input_revision: 5 };
    vi.spyOn(portalApi, 'task').mockResolvedValue(acceptedTask);
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByRole('button', { name: /你好/ }));
    expect(await screen.findByText(/当前 Task 的目标已固定：云端 Agent Runtime · 测试电脑（运行中） · 项目工作区 v5/)).toBeInTheDocument();
    await user.selectOptions(screen.getByRole('combobox', { name: '选择目标电脑' }), '');
    expect(screen.getByText(/当前 Task 的目标已固定：云端 Agent Runtime · 测试电脑（运行中） · 项目工作区 v5/)).toBeInTheDocument();
    expect(screen.getByText(/目标只对接下来发送的消息生效/)).toBeInTheDocument();
  });

  it('does not silently drop a selected Workspace that disappears from the accessible bindings', async () => {
    mockCommon({ hosts: [targetHost] });
    vi.mocked(portalApi.hosts)
      .mockResolvedValueOnce({ items: [targetHost] })
      .mockResolvedValueOnce({ items: [{ ...targetHost, workspaces: [] }] });
    const user = userEvent.setup();
    const { client } = renderPage();

    await screen.findByRole('option', { name: /测试电脑/ });
    await user.selectOptions(screen.getByRole('combobox', { name: '选择目标电脑' }), targetHost.id);
    await user.selectOptions(await screen.findByRole('combobox', { name: '选择目标 Workspace' }), 'w1234567');
    await user.selectOptions(screen.getByRole('combobox', { name: '选择 AI 模型' }), model.id);
    await user.type(screen.getByRole('textbox', { name: '消息内容' }), '检查失效 Workspace');
    await client.invalidateQueries({ queryKey: ['assistant-hosts'] });

    expect(await screen.findByText(/所选 Workspace 已不在当前电脑的可访问列表中/)).toBeInTheDocument();
    expect(screen.getByRole('combobox', { name: '选择目标 Workspace' })).toHaveValue('w1234567');
    expect(screen.getByRole('button', { name: /发送/ })).toBeDisabled();
  });

  it('submits a durable task and displays its saved conversation reply', async () => {
    mockCommon();
    const conversations = vi.mocked(portalApi.conversations);
    const messages = vi.mocked(portalApi.conversationMessages);
    const submit = vi.spyOn(portalApi, 'submitTask').mockImplementation(async () => {
      conversations.mockResolvedValue({ items: [conversation], next_cursor: null });
      messages.mockResolvedValue({ items: [message], next_cursor: null });
      return { ...task, status: 'queued', result: null, finished_at: null };
    });
    const taskQuery = vi.spyOn(portalApi, 'task').mockImplementation(async () => {
      messages.mockResolvedValue({ items: [message, reply], next_cursor: null });
      return task;
    });
    const user = userEvent.setup();
    renderPage();
    await user.type(await screen.findByRole('textbox', { name: '消息内容' }), '你好');
    await user.click(screen.getByRole('button', { name: /发送/ }));
    await waitFor(() => expect(submit).toHaveBeenCalledWith({ model_id: model.id, objective: '你好' }, expect.any(String)));
    await waitFor(() => expect(screen.getByText('你好！')).toBeInTheDocument());
    expect(taskQuery).toHaveBeenCalledWith('q1234567');
  });

  it('only offers models that were checked and support text', async () => {
    mockCommon();
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [
      model,
      { ...model, id: 'p1111111', name: '未检查模型', checked_at: null },
      { ...model, id: 'p2222222', name: '仅视觉模型', capabilities: { ...model.capabilities, text: false } },
    ] });
    renderPage();

    const selector = await screen.findByRole('combobox', { name: '选择 AI 模型' });
    await waitFor(() => expect(selector).toHaveValue(model.id));
    expect(within(selector).getAllByRole('option').map((option) => (option as HTMLOptionElement).value)).toEqual(['', model.id]);
  });

  it('recovers a streaming assistant message from the Task snapshot after conversation history expires', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    vi.spyOn(portalApi, 'task').mockResolvedValue({
      ...task, status: 'running', result: null, finished_at: null,
      assistant_message: {
        id: 'm8765432', conversation_id: conversation.id, task_id: task.id, role: 'assistant',
        content: 'A persisted partial answer', status: 'streaming', created_at: timestamp,
      },
    });
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByRole('button', { name: /你好/ }));
    expect(await screen.findByText('A persisted partial answer')).toBeInTheDocument();
    expect(screen.getByText('正在生成…')).toHaveAttribute('role', 'status');
  });

  it('deduplicates a snapshot against the same committed conversation message', () => {
    const merged = mergeTaskAssistantMessage([reply], {
      ...task, status: 'running', result: null, finished_at: null,
      assistant_message: {
        ...reply, status: 'streaming',
      },
    });
    expect(merged).toHaveLength(1);
    expect(merged[0]?.content).toBe(reply.content);
    expect(merged[0]?.snapshotStatus).toBe('completed');
  });

  it('appends a task to an existing conversation and explains the disabled cloud worker queue', async () => {
    mockCommon({ conversations: [conversation], messages: [message, reply], cloudTasks: false });
    const newMessage = { ...message, id: 'm2345678', content: '继续', task_id: 'q2345678' };
    const messages = vi.mocked(portalApi.conversationMessages);
    const append = vi.spyOn(portalApi, 'appendConversationMessage').mockImplementation(async (_id, body) => {
      messages.mockResolvedValue({ items: [message, reply, newMessage], next_cursor: null });
      return { message: newMessage, task: { ...task, id: 'q2345678', status: 'queued', objective: body.content, result: null, finished_at: null } };
    });
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, id: 'q2345678', status: 'queued', objective: '继续', result: null, finished_at: null });
    const user = userEvent.setup();
    renderPage();
    await user.click(await screen.findByRole('button', { name: /你好/ }));
    expect(await screen.findByText('你好！')).toBeInTheDocument();
    expect(screen.getByRole('combobox', { name: '选择 AI 模型' })).toBeEnabled();
    await user.selectOptions(screen.getByRole('combobox', { name: '选择 AI 模型' }), otherModel.id);
    await user.type(screen.getByRole('textbox', { name: '消息内容' }), '继续');
    await user.click(screen.getByRole('button', { name: /发送/ }));
    await waitFor(() => expect(append).toHaveBeenCalledWith(conversation.id, { model_id: otherModel.id, content: '继续' }, expect.any(String)));
    expect(await screen.findByText(/当前服务尚未启用云端任务执行/)).toBeInTheDocument();
    expect(await screen.findByText(/任务已保存并排队/)).toBeInTheDocument();
  });

  it('fails closed and offers a capability retry when capabilities cannot be read', async () => {
    mockCommon();
    vi.mocked(portalApi.capabilities)
      .mockRejectedValueOnce(new Error('连接中断'))
      .mockResolvedValueOnce(clientContract.capabilities.cloud_execution);
    const submit = vi.spyOn(portalApi, 'submitTask').mockResolvedValue(task);
    const user = userEvent.setup();
    renderPage();

    expect(await screen.findByRole('alert')).toHaveTextContent('无法读取服务能力：连接中断');
    await user.type(screen.getByRole('textbox', { name: '消息内容' }), '等待能力校验');
    expect(screen.getByRole('button', { name: /发送/ })).toBeDisabled();
    await user.click(screen.getByRole('button', { name: '重试服务能力' }));

    await waitFor(() => expect(screen.getByRole('button', { name: /发送/ })).toBeEnabled());
    expect(portalApi.capabilities).toHaveBeenCalledTimes(2);
    expect(submit).not.toHaveBeenCalled();
  });

  it('disables submission when the service does not advertise the Task API', async () => {
    mockCommon({ capabilities: clientContract.capabilities.unsupported });
    const submit = vi.spyOn(portalApi, 'submitTask').mockResolvedValue(task);
    const user = userEvent.setup();
    renderPage();

    expect(await screen.findByText(/当前服务不支持云端任务 API/)).toBeInTheDocument();
    await user.type(screen.getByRole('textbox', { name: '消息内容' }), '不应发送');
    expect(screen.getByRole('button', { name: /发送/ })).toBeDisabled();
    expect(submit).not.toHaveBeenCalled();
  });

  it('accepts the legacy cloud_tasks feature as evidence that the Task API exists', async () => {
    mockCommon({ capabilities: {
      ...clientContract.capabilities.cloud_execution,
      supported_features: ['cloud_tasks'],
    } });
    const submit = vi.spyOn(portalApi, 'submitTask').mockResolvedValue(task);
    const user = userEvent.setup();
    renderPage();

    await user.type(await screen.findByRole('textbox', { name: '消息内容' }), '兼容旧服务');
    await user.click(screen.getByRole('button', { name: /发送/ }));
    await waitFor(() => expect(submit).toHaveBeenCalledWith({ model_id: model.id, objective: '兼容旧服务' }, expect.any(String)));
  });

  it('reuses the request key after a lost submission response', async () => {
    mockCommon();
    const submit = vi.spyOn(portalApi, 'submitTask').mockRejectedValueOnce(new Error('连接中断')).mockResolvedValueOnce(task);
    const user = userEvent.setup();
    renderPage();
    await user.type(await screen.findByRole('textbox', { name: '消息内容' }), '你好');
    await user.click(screen.getByRole('button', { name: /发送/ }));
    await waitFor(() => expect(submit).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(screen.getByRole('button', { name: /发送/ })).toBeEnabled());
    await user.click(screen.getByRole('button', { name: /发送/ }));
    await waitFor(() => expect(submit).toHaveBeenCalledTimes(2));
    expect(submit.mock.calls[1]?.[1]).toBe(submit.mock.calls[0]?.[1]);
  });

  it.each(['approve', 'reject'] as const)('sends the versioned %s decision for the exact pending approval', async (decision) => {
    mockCommon({ conversations: [conversation], messages: [message] });
    const approval = { id: 'k1234567', version: 4, kind: 'approval' as const, title: '写入文件', object: '工作区文件', content: '创建报告.md', impact: '会新增一个文件' };
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, status: 'waiting_user', result: null, waiting_reason: 'approval', pending_request: approval, finished_at: null });
    const respond = vi.spyOn(portalApi, 'respondToTaskApproval').mockResolvedValue({ task_id: task.id, action: 'respond', status: 'queued', unsettled_operation_ids: [] });
    const user = userEvent.setup();
    renderPage();
    await user.click(await screen.findByRole('button', { name: /你好/ }));
    expect(await screen.findByRole('region', { name: '需要批准的操作' })).toHaveTextContent('创建报告.md');
    await user.click(screen.getByRole('button', { name: decision === 'approve' ? '批准并继续' : '拒绝' }));
    await waitFor(() => expect(respond).toHaveBeenCalledWith(task.id, approval.id,
      { version: approval.version, decision }, expect.any(String)));
  });

  it('reuses the approval idempotency key after a lost response', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    const approval = { id: 'k1234567', version: 4, kind: 'approval' as const, title: '写入文件', object: '工作区文件', content: '创建报告.md', impact: '会新增一个文件' };
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, status: 'waiting_user', result: null, waiting_reason: 'approval', pending_request: approval, finished_at: null });
    const respond = vi.spyOn(portalApi, 'respondToTaskApproval')
      .mockRejectedValueOnce(new Error('连接中断'))
      .mockResolvedValueOnce({ task_id: task.id, action: 'respond', status: 'queued', unsettled_operation_ids: [] });
    const user = userEvent.setup();
    renderPage();
    await user.click(await screen.findByRole('button', { name: /你好/ }));
    await user.click(await screen.findByRole('button', { name: '批准并继续' }));
    await waitFor(() => expect(respond).toHaveBeenCalledTimes(1));
    await user.click(await screen.findByRole('button', { name: '批准并继续' }));
    await waitFor(() => expect(respond).toHaveBeenCalledTimes(2));
    expect(respond.mock.calls[0]?.[3]).toBeTruthy();
    expect(respond.mock.calls[1]?.[3]).toBe(respond.mock.calls[0]?.[3]);
  });

  it('refreshes the task and prompts before retrying a stale approval', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    const approval = { id: 'k1234567', version: 4, kind: 'approval' as const, title: '写入文件', object: '工作区文件', content: '创建报告.md', impact: '会新增一个文件' };
    const freshTask = { ...task, status: 'queued' as const, result: null, waiting_reason: null, pending_request: null, finished_at: null };
    const getTask = vi.spyOn(portalApi, 'task').mockResolvedValueOnce({ ...task, status: 'waiting_user', result: null, waiting_reason: 'approval', pending_request: approval, finished_at: null }).mockResolvedValueOnce(freshTask);
    vi.spyOn(portalApi, 'respondToTaskApproval').mockRejectedValue(new ApiError({
      message: 'pending request version is stale', code: 'conflict', status: 409,
    }));
    const user = userEvent.setup();
    renderPage();
    await user.click(await screen.findByRole('button', { name: /你好/ }));
    await user.click(await screen.findByRole('button', { name: '批准并继续' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('任务状态已变化，已刷新最新状态');
    await waitFor(() => expect(getTask).toHaveBeenCalledTimes(2));
    expect(screen.queryByRole('region', { name: '需要批准的操作' })).not.toBeInTheDocument();
  });

  it('sends a trimmed answer for the exact versioned clarification request', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    const clarification = { id: 'k7654321', version: 2, question: '文件应该保存在哪个目录？' };
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, status: 'waiting_user', result: null, waiting_reason: 'clarification', clarification_request: clarification, finished_at: null });
    const answer = vi.spyOn(portalApi, 'answerTaskInput').mockResolvedValue({ task_id: task.id, action: 'answer_clarification', status: 'queued', unsettled_operation_ids: [] });
    const user = userEvent.setup();
    renderPage();
    await user.click(await screen.findByRole('button', { name: /你好/ }));
    await user.type(await screen.findByRole('textbox', { name: '你的回答' }), '  /workspace/reports  ');
    await user.click(screen.getByRole('button', { name: '提交并继续' }));
    await waitFor(() => expect(answer).toHaveBeenCalledWith(task.id, {
      clarification_id: clarification.id, clarification_version: clarification.version, answer: '/workspace/reports',
    }, expect.any(String)));
  });

  it('reuses the clarification idempotency key after a lost response', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    const clarification = { id: 'k7654321', version: 2, question: '文件应该保存在哪个目录？' };
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, status: 'waiting_user', result: null, waiting_reason: 'clarification', clarification_request: clarification, finished_at: null });
    const answer = vi.spyOn(portalApi, 'answerTaskInput')
      .mockRejectedValueOnce(new Error('连接中断'))
      .mockResolvedValueOnce({ task_id: task.id, action: 'answer_clarification', status: 'queued', unsettled_operation_ids: [] });
    const user = userEvent.setup();
    renderPage();
    await user.click(await screen.findByRole('button', { name: /你好/ }));
    await user.type(await screen.findByRole('textbox', { name: '你的回答' }), '  /workspace/reports  ');
    await user.click(await screen.findByRole('button', { name: '提交并继续' }));
    await waitFor(() => expect(answer).toHaveBeenCalledTimes(1));
    await user.click(await screen.findByRole('button', { name: '提交并继续' }));
    await waitFor(() => expect(answer).toHaveBeenCalledTimes(2));
    expect(answer.mock.calls[0]?.[2]).toBeTruthy();
    expect(answer.mock.calls[1]?.[2]).toBe(answer.mock.calls[0]?.[2]);
  });

  it('refreshes the task and prompts before retrying stale clarification input', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    const clarification = { id: 'k7654321', version: 2, question: '文件应该保存在哪个目录？' };
    const freshTask = { ...task, status: 'queued' as const, result: null, waiting_reason: null, clarification_request: null, finished_at: null };
    const getTask = vi.spyOn(portalApi, 'task').mockResolvedValueOnce({ ...task, status: 'waiting_user', result: null, waiting_reason: 'clarification', clarification_request: clarification, finished_at: null }).mockResolvedValueOnce(freshTask);
    vi.spyOn(portalApi, 'answerTaskInput').mockRejectedValue(new ApiError({
      message: 'clarification version is stale', code: 'conflict', status: 409,
    }));
    const user = userEvent.setup();
    renderPage();
    await user.click(await screen.findByRole('button', { name: /你好/ }));
    await user.type(await screen.findByRole('textbox', { name: '你的回答' }), '放到报告目录');
    await user.click(await screen.findByRole('button', { name: '提交并继续' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('任务状态已变化，已刷新最新状态');
    await waitFor(() => expect(getTask).toHaveBeenCalledTimes(2));
    expect(screen.queryByRole('form', { name: '补充任务信息' })).not.toBeInTheDocument();
  });

  it('submits an idempotent stop request and waits for server confirmation', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    const runningTask = { ...task, status: 'running' as const, result: null, finished_at: null };
    const stoppingTask = {
      ...task, status: 'cancelling' as const, result: null, finished_at: null,
      stop_request: { id: 'stop12345', action: 'cancel' as const, status: 'pending' as const,
        safe_after: timestamp, unsettled_operation_ids: ['op1234567'] },
    };
    vi.spyOn(portalApi, 'task').mockResolvedValueOnce(runningTask).mockResolvedValue(stoppingTask);
    const stop = vi.spyOn(portalApi, 'taskAction').mockResolvedValue({
      task_id: task.id, action: 'cancel', status: 'cancelling', unsettled_operation_ids: ['op1234567'],
    });
    const user = userEvent.setup();
    renderPage();
    await user.click(await screen.findByRole('button', { name: /你好/ }));
    expect(await screen.findByRole('button', { name: '停止任务' })).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '停止任务' }));
    await waitFor(() => expect(stop).toHaveBeenCalledWith(task.id,
      { action: 'cancel', expected_status: 'running' }, expect.any(String)));
    expect(await screen.findByText('停止请求已受理，等待任务状态更新。')).toBeInTheDocument();
    expect(await screen.findByText('停止请求已受理，仍有 1 个操作结果待确认。')).toBeInTheDocument();
  });

  it('reuses the same idempotency key when a stop response is lost', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, status: 'running', result: null, finished_at: null });
    const stop = vi.spyOn(portalApi, 'taskAction')
      .mockRejectedValueOnce(new Error('连接中断'))
      .mockResolvedValueOnce({ task_id: task.id, action: 'cancel', status: 'cancelling', unsettled_operation_ids: [] });
    const user = userEvent.setup();
    renderPage();
    await user.click(await screen.findByRole('button', { name: /你好/ }));
    await user.click(await screen.findByRole('button', { name: '停止任务' }));
    await waitFor(() => expect(stop).toHaveBeenCalledTimes(1));
    await user.click(await screen.findByRole('button', { name: '停止任务' }));
    await waitFor(() => expect(stop).toHaveBeenCalledTimes(2));
    expect(stop.mock.calls[1]?.[2]).toBe(stop.mock.calls[0]?.[2]);
  });

  it('does not label a stop without a server receipt as completed', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, status: 'cancelling', result: null, stop_request: null, finished_at: null });
    renderPage();
    await userEvent.setup().click(await screen.findByRole('button', { name: /你好/ }));
    expect(await screen.findByText('停止状态待确认，正在重新读取服务端任务。')).toBeInTheDocument();
    expect(screen.queryByText('服务端已确认任务停止。')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '停止任务' })).not.toBeInTheDocument();
  });
});
