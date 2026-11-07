import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { setupServer } from 'msw/node';
import userEvent from '@testing-library/user-event';
import { readFileSync } from 'node:fs';
import { webcrypto } from 'node:crypto';
import { resolve } from 'node:path';
import { MemoryRouter } from 'react-router';
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError } from '../api/client';
import { portalApi } from '../app/api';
import type { components } from '../api/schema';
import { messageOutbox } from '../app/message-outbox';
import type { PendingConversationMessage } from '../app/message-outbox';
import type { MainConversation } from '../app/api';
import { ToastProvider } from '../app/toast';
import { Assistant, assistantRecentConversationKey, mergeTaskAssistantMessage, resolveTaskObservationURL } from './Assistant';

const authUser = vi.hoisted(() => ({ id: 'u1234567' }));
vi.mock('../app/auth', () => ({ useAuth: () => ({ user: authUser }) }));

const timestamp = '2026-10-22T08:00:00.000Z';
type ConversationMessage = Awaited<ReturnType<typeof portalApi.conversationMessages>>['items'][number];
type AcceptConversationMessageRequest = components['schemas']['AcceptConversationMessageRequest'];
const clientContract = JSON.parse(readFileSync(
  resolve(process.cwd(), '../../server/openapi/fixtures/morde-client-v1.json'), 'utf8',
)) as {
  capabilities: Record<'unsupported' | 'queued_task_api' | 'cloud_execution' | 'unsupported_version',
    Awaited<ReturnType<typeof portalApi.capabilities>>>;
};
const model = {
  id: 'p1234567', name: '团队模型', base_url: 'https://models.example.test/v1', model_name: 'morde-test', shared: true, has_api_key: true,
  capabilities: { text: true, tools: true, vision: false, text_stream: false }, checked_at: timestamp, created_at: timestamp, updated_at: timestamp,
} satisfies Awaited<ReturnType<typeof portalApi.modelConnections>>['items'][number];
const mcpConnector = {
  id: 'k1234567', name: '日历工具', endpoint_url: 'https://mcp.example.test/mcp', enabled: true,
  has_credential: true, credential_version: 1, revision: 2, created_at: timestamp, updated_at: timestamp,
} satisfies Awaited<ReturnType<typeof portalApi.mcpConnectors>>['items'][number];
type AvailableModels = Awaited<ReturnType<typeof portalApi.availableModels>>;
const defaultAvailableModels: AvailableModels = {
  items: [{ id: model.id, name: model.name, model_name: model.model_name, capabilities: model.capabilities }],
  configuration_status: 'configured',
  default_model_configured: true,
  default_model_id: model.id,
};
let availableModelsResponse = defaultAvailableModels;
const server = setupServer(http.get('*/api/available-models', () => HttpResponse.json(availableModelsResponse)));
const otherModel = { ...model, id: 'p7654321', name: '备用模型' };
const targetHost: Awaited<ReturnType<typeof portalApi.hosts>>['items'][number] = {
  id: 'h1234567', name: '测试电脑', owner_id: 'u1234567', template_id: 'std', isolation: 'container', state: 'running', state_reason: '',
  runtime_generation: 1, control_version: 2, control_state: 'ai', url: 'https://h1234567.hosts.test',
  workspaces: [{ workspace: { id: 'w1234567', name: '项目工作区', revision: 5, used_bytes: 20, state: 'ready' },
    directory: 'projects/w1234567', state: 'active', binding_version: 1, desired_revision: 5, applied_revision: 5, sync_state: 'healthy', sync_conflicts: [] }],
  resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 },
  expires_at: null, last_active_at: null, created_at: timestamp,
};
const conversation = { id: 'c1234567', scope: 'personal' as const, is_main: true, title: '你好', created_at: timestamp, updated_at: timestamp };
const message = { id: 'm1234567', conversation_id: conversation.id, sequence: 1, role: 'user' as const, content: '你好', task_id: 'q1234567', delivery_status: 'applied' as const, created_at: timestamp };
const reply = { id: 'm7654321', conversation_id: conversation.id, sequence: 2, role: 'assistant' as const, content: '你好！', task_id: 'q1234567', delivery_status: 'applied' as const, created_at: timestamp };
const task = {
  id: 'q1234567', owner_id: 'u1234567', initiator_id: 'u1234567', execution_location: 'server' as const, conversation_id: conversation.id,
  model_id: model.id, objective: message.content, status: 'completed' as const, event_sequence: 0, current_step: null,
  budget: { active_seconds: 600, tool_calls: 10, model_tokens: 1000 }, usage: { active_seconds: 2, tool_calls: 0, model_tokens: 12 },
  created_at: timestamp, updated_at: timestamp, result: reply.content, waiting_reason: null, steps: [], batch_items: [],
  artifacts: [], pending_request: null, clarification_request: null, stop_request: null, assistant_message: null, finished_at: timestamp,
} satisfies Awaited<ReturnType<typeof portalApi.task>>;
let acceptedMessageSequence = 0;

function acceptedReceipt(conversationID: string, body: AcceptConversationMessageRequest) {
  const sequence = ++acceptedMessageSequence;
  const submitted: ConversationMessage = {
    id: `m${String(sequence).padStart(7, '0')}`, conversation_id: conversationID,
    sequence: message.sequence + sequence, role: 'user', content: body.content, client_message_id: body.client_message_id,
    routing_status: 'pending', delivery_status: 'pending', created_at: timestamp,
  };
  return { message: submitted, replayed: false };
}

function renderPage(initialState: unknown = null) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const view = render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter initialEntries={[{ pathname: '/assistant', state: initialState }]}><Assistant /></MemoryRouter></ToastProvider></QueryClientProvider>);
  return { ...view, client };
}

function mockCommon(options: { conversations?: typeof conversation[]; messages?: ConversationMessage[]; mainConversation?: MainConversation; cloudTasks?: boolean; capabilities?: Awaited<ReturnType<typeof portalApi.capabilities>>; hosts?: typeof targetHost[] } = {}) {
  const standard = options.cloudTasks === false ? clientContract.capabilities.queued_task_api : clientContract.capabilities.cloud_execution;
  vi.spyOn(portalApi, 'capabilities').mockResolvedValue(options.capabilities ??
    { ...standard, supported_features: [...standard.supported_features, 'conversation_auto_routing_v1'] });
  vi.spyOn(portalApi, 'conversations').mockResolvedValue({ items: options.conversations ?? [], next_cursor: null });
  vi.spyOn(portalApi, 'conversation').mockResolvedValue(conversation);
  vi.spyOn(portalApi, 'mainConversation').mockResolvedValue(options.mainConversation ?? (options.conversations?.[0]
    ? { status: 'ready', conversation_id: options.conversations[0].id, version: 1 }
    : { status: 'selection_required', conversation_id: null, version: 0 }));
  vi.spyOn(portalApi, 'ensureMainConversation').mockResolvedValue({ status: 'ready', conversation_id: conversation.id, version: 1 });
  vi.spyOn(portalApi, 'setMainConversation').mockImplementation(async (conversationID) => ({ status: 'ready', conversation_id: conversationID, version: 2 }));
  vi.spyOn(portalApi, 'conversationMessages').mockResolvedValue({ items: options.messages ?? [], next_cursor: null });
  vi.spyOn(portalApi, 'acceptConversationMessage').mockImplementation(async (conversationID, body) => acceptedReceipt(conversationID, body));
  vi.spyOn(portalApi, 'conversationTasks').mockResolvedValue({ items: [], next_cursor: null });
  vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [model, otherModel], configuration_status: 'configured' });
  vi.spyOn(portalApi, 'mcpConnectors').mockResolvedValue({ items: [] });
  vi.spyOn(portalApi, 'preferences').mockResolvedValue({ assistant_name: 'Morde', selected_model_id: model.id, default_host_id: null, updated_at: timestamp });
  vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: options.hosts ?? [] });
  vi.spyOn(portalApi, 'execution').mockResolvedValue({ owner_id: 'u1234567', state: 'enabled', epoch: 1, pending_task_count: 0,
    runtime: { state: 'running', runtime_id: 'rt1234567', runtime_generation: 1, max_concurrent_tasks: 4, active_task_count: 0 }, updated_at: timestamp });
}

describe('Assistant durable tasks', () => {
  beforeAll(() => server.listen());
  beforeEach(() => {
    availableModelsResponse = defaultAvailableModels;
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
  });
  afterEach(() => { cleanup(); server.resetHandlers(); localStorage.clear(); authUser.id = 'u1234567'; vi.restoreAllMocks(); vi.unstubAllGlobals(); });
  afterAll(() => server.close());

  it('keeps conversation and model actions in their respective chat panels', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    renderPage();

    const sidebar = await screen.findByRole('complementary', { name: '最近对话' });
    expect(within(sidebar).getByRole('button', { name: '＋ 新对话' })).toBeEnabled();
    const composer = screen.getByRole('region', { name: '发送云端任务消息' });
    expect(within(composer).getByLabelText('选择目标电脑')).toBeInTheDocument();
    expect(within(composer).getByLabelText('选择 AI 模型')).toBeInTheDocument();
    expect(within(composer).getByLabelText('消息内容')).toBeInTheDocument();
    expect(Array.from(composer.children, (child) => child.tagName)).toEqual(['FORM']);
  });

  it('renders newest-first API pages as a chronological chat and keeps progress after its task messages', async () => {
    mockCommon({ conversations: [conversation], messages: [reply, message] });
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, status: 'running', result: null, finished_at: null });
    renderPage();

    const messageList = (await screen.findByText('你好！', { exact: true })).closest<HTMLElement>('.message-list');
    expect(messageList).not.toBeNull();
    const articles = within(messageList!).getAllByRole('article');
    expect(articles.map((article) => article.textContent)).toEqual(expect.arrayContaining([
      expect.stringContaining('你好'), expect.stringContaining('你好！'),
    ]));
    expect(articles[0]?.querySelector('p')).toHaveTextContent(/^你好$/);
    expect(articles[1]?.querySelector('p')).toHaveTextContent(/^你好！$/);
    const activity = await within(messageList!).findByRole('region', { name: `任务 ${task.id} 的工作过程` });
    const renderedText = messageList!.textContent ?? '';
    expect(renderedText.indexOf('你好！')).toBeLessThan(renderedText.indexOf(activity.textContent ?? ''));
  });

  it('shows a download link for a personal attachment restored from the conversation', async () => {
    const attachmentID = 'pa_0123456789abcdef0123456789abcdef';
    mockCommon({ conversations: [conversation], messages: [{ ...message, task_id: undefined, attachment_ids: [attachmentID] }] });
    renderPage();
    const link = await screen.findByRole('link', { name: '下载附件 1' });
    expect(link).toHaveAttribute('href', `/api/personal-data/attachments/${attachmentID}`);
    expect(link).toHaveAttribute('download');
  });

  it('loads every conversation page without duplicating overlapping messages', async () => {
    mockCommon({ conversations: [conversation] });
    const older = { ...message, task_id: undefined };
    const newer = { ...reply, task_id: undefined };
    vi.mocked(portalApi.conversationMessages).mockImplementation(async (_id, cursor) => cursor
      ? { items: [older, newer], next_cursor: null }
      : { items: [newer], next_cursor: 'older-page' });
    renderPage();
    expect(await screen.findByText('你好！')).toBeInTheDocument();
    expect(screen.getAllByText('你好！')).toHaveLength(1);
    expect(screen.getByText('你好', { selector: 'p' })).toBeInTheDocument();
    expect(portalApi.conversationMessages).toHaveBeenCalledWith(conversation.id, 'older-page');
  });

  it('keeps model errors and retry controls in the scrollable conversation rather than the input area', async () => {
    mockCommon();
    vi.mocked(portalApi.modelConnections).mockRejectedValueOnce(new Error('读取失败')).mockResolvedValue({ items: [model], configuration_status: 'configured' });
    const user = userEvent.setup();
    renderPage();

    const error = await screen.findByText(/无法读取模型：读取失败/);
    const composer = screen.getByRole('region', { name: '发送云端任务消息' });
    expect(composer).not.toContainElement(error);
    expect(Array.from(composer.children, (child) => child.tagName)).toEqual(['FORM']);
    await user.click(within(error).getByRole('button', { name: '重试' }));
    await waitFor(() => expect(screen.queryByText(/无法读取模型：读取失败/)).not.toBeInTheDocument());
  });

  it('builds a same-Host task-auth URL and rejects unsafe Workbench origins', () => {
    const url = new URL(resolveTaskObservationURL('opaque-ticket-123456', targetHost.url, targetHost.id, 'portal.example.test'));
    expect(url.origin).toBe(new URL(targetHost.url).origin);
    expect(url.pathname).toBe('/__ch/task-auth');
    expect(url.searchParams.get('ticket')).toBe('opaque-ticket-123456');
    expect(() => resolveTaskObservationURL('opaque-ticket-123456', 'http://evil.example.test', targetHost.id, 'portal.example.test'))
      .toThrow('无效的 Workbench 地址');
    expect(() => resolveTaskObservationURL('opaque-ticket-123456', `${targetHost.url}/other`, targetHost.id, 'portal.example.test'))
      .toThrow('无效的 Workbench 地址');
  });

  it('opens a host-bound Workbench task observation with a one-time Portal ticket', async () => {
    const hostTask = { ...task, host_id: targetHost.id };
    mockCommon({ conversations: [conversation], messages: [message], hosts: [targetHost] });
    vi.spyOn(portalApi, 'task').mockResolvedValue(hostTask);
    vi.spyOn(portalApi, 'taskWorkbenchObservationTicket').mockResolvedValue({ task_id: task.id, host_id: targetHost.id,
      ticket: 'opaque-one-time-ticket-123', expires_at: new Date(Date.now() + 30_000).toISOString() });
    const popup = { opener: window, location: { replace: vi.fn() }, close: vi.fn() };
    vi.spyOn(window, 'open').mockReturnValue(popup as unknown as Window);
    renderPage();

    fireEvent.click(await screen.findByRole('button', { name: '在 Workbench 只读观察' }));
    await waitFor(() => expect(popup.location.replace).toHaveBeenCalledOnce());
    expect(portalApi.taskWorkbenchObservationTicket).toHaveBeenCalledWith(task.id);
    const target = new URL(popup.location.replace.mock.calls[0]![0] as string);
    expect(target.origin).toBe(new URL(targetHost.url).origin);
    expect(target.pathname).toBe('/__ch/task-auth');
    expect(target.searchParams.get('ticket')).toBe('opaque-one-time-ticket-123');
    expect(popup.opener).toBeNull();
  });

  it('uses the server main conversation while keeping the local recent conversation as a display hint', async () => {
    const recent = { ...conversation, id: 'c7654321', is_main: false, title: '本机最近会话' };
    mockCommon({ conversations: [conversation, recent], messages: [message], mainConversation: { status: 'ready', conversation_id: conversation.id, version: 3 } });
    const serviceInstanceID = clientContract.capabilities.cloud_execution.service_instance_id;
    localStorage.setItem(assistantRecentConversationKey(serviceInstanceID, authUser.id), recent.id);
    const getTask = vi.spyOn(portalApi, 'task').mockResolvedValue(task);

    renderPage();

    expect(await screen.findByRole('heading', { name: '你好' })).toBeInTheDocument();
    expect(await screen.findByText('本机最近查看')).toBeInTheDocument();
    await waitFor(() => expect(getTask).toHaveBeenCalledWith(task.id));
    expect(await screen.findByText('你好！')).toBeInTheDocument();
  });

  it('does not restore another account’s recent conversation', async () => {
    const serviceInstanceID = clientContract.capabilities.cloud_execution.service_instance_id;
    localStorage.setItem(assistantRecentConversationKey(serviceInstanceID, 'u1234567'), conversation.id);
    authUser.id = 'u7654321';
    mockCommon({ conversations: [conversation], messages: [message], mainConversation: { status: 'selection_required', conversation_id: null, version: 0 } });

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
    mockCommon({ conversations: [conversation], messages: [message], mainConversation: { status: 'selection_required', conversation_id: null, version: 0 }, capabilities: {
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
    expect(await screen.findByRole('heading', { name: '你好' })).toBeInTheDocument();
  });

  it('initializes an empty main conversation with a stable ensure request', async () => {
    mockCommon({ mainConversation: { status: 'uninitialized', conversation_id: null, version: 0 } });
    const ensure = vi.mocked(portalApi.ensureMainConversation);
    renderPage();

    expect(await screen.findByRole('heading', { name: '你好' })).toBeInTheDocument();
    await waitFor(() => expect(ensure).toHaveBeenCalledTimes(1));
    expect(ensure.mock.calls[0]?.[0]).toMatch(/[A-Za-z0-9._~-]{8,128}/);
  });

  it('opens an existing conversation without changing the server main conversation or creating a Task', async () => {
    const second = { ...conversation, id: 'c7654321', is_main: false, title: '另一个会话' };
    mockCommon({ conversations: [conversation, second], messages: [message], mainConversation: { status: 'selection_required', conversation_id: null, version: 0 } });
    const setMain = vi.mocked(portalApi.setMainConversation);
    const submit = vi.spyOn(portalApi, 'submitTask');
    vi.spyOn(portalApi, 'task').mockResolvedValue(task);
    const user = userEvent.setup();
    renderPage();

    expect(await screen.findByRole('heading', { name: '今天想聊些什么？' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '设为主会话' })).not.toBeInTheDocument();
    await user.click(await screen.findByRole('button', { name: /你好/ }));
    expect(await screen.findByRole('heading', { name: '你好' })).toBeInTheDocument();
    expect(setMain).not.toHaveBeenCalled();
    expect(submit).not.toHaveBeenCalled();
  });

  it('creates a new conversation using the outbox account and service identities', async () => {
    mockCommon();
    const create = vi.spyOn(portalApi, 'createConversation').mockResolvedValue(conversation);
    const accept = vi.spyOn(portalApi, 'acceptConversationMessage').mockImplementation(async (conversationID, body) => acceptedReceipt(conversationID, body));
    const user = userEvent.setup();
    renderPage();
    await user.type(await screen.findByRole('textbox', { name: '消息内容' }), '新会话');
    await user.click(screen.getByRole('button', { name: '发送' }));
    await waitFor(() => expect(accept).toHaveBeenCalledOnce());
    expect(create).toHaveBeenCalledWith({}, expect.any(String), clientContract.capabilities.cloud_execution.service_instance_id, authUser.id);
  });

  it('keeps messages locally when the service has not enabled automatic routing', async () => {
    const legacy = clientContract.capabilities.cloud_execution;
    mockCommon({ conversations: [conversation], capabilities: legacy });
    const accept = vi.spyOn(portalApi, 'acceptConversationMessage').mockImplementation(async (conversationID, body) => acceptedReceipt(conversationID, body));
    const user = userEvent.setup();
    renderPage();
    await user.type(await screen.findByRole('textbox', { name: '消息内容' }), '旧服务中的待发送消息');
    await user.click(screen.getByRole('button', { name: '发送' }));
    expect(await screen.findByText(/服务尚不支持自动分流/)).toBeInTheDocument();
    expect(accept).not.toHaveBeenCalled();
    expect(screen.getByRole('textbox', { name: '消息内容' })).toHaveValue('');
    expect(screen.getByRole('button', { name: '核对并重试' })).toBeEnabled();
  });

  it('accepts a message with selected targets and stable identity', async () => {
    mockCommon({ conversations: [conversation], hosts: [targetHost] });
    vi.mocked(portalApi.mcpConnectors).mockResolvedValue({ items: [mcpConnector] });
    const accept = vi.spyOn(portalApi, 'acceptConversationMessage').mockImplementation(async (conversationID, body) => acceptedReceipt(conversationID, body));
    const user = userEvent.setup();
    renderPage();
    await screen.findByRole('option', { name: /测试电脑/ });
    await user.selectOptions(screen.getByRole('combobox', { name: '选择目标电脑' }), targetHost.id);
    await user.selectOptions(screen.getByRole('combobox', { name: '选择目标 Workspace' }), 'w1234567');
    await user.click(await screen.findByRole('checkbox', { name: '日历工具' }));
    await user.type(screen.getByRole('textbox', { name: '消息内容' }), '检查工作区报告');
    await user.click(screen.getByRole('button', { name: '发送' }));
    await waitFor(() => expect(accept).toHaveBeenCalledOnce());
    const [conversationID, body, key, serviceID, actorID] = accept.mock.calls[0]!;
    expect(conversationID).toBe(conversation.id);
    expect(body).toMatchObject({ content: '检查工作区报告', model_id: model.id, host_id: targetHost.id,
      workspace_id: 'w1234567', mcp_connector_ids: [mcpConnector.id], client_message_id: key });
    expect(body).not.toHaveProperty('mode');
    expect(body).not.toHaveProperty('input_revision');
    expect(serviceID).toBe(clientContract.capabilities.cloud_execution.service_instance_id);
    expect(actorID).toBe(authUser.id);
    await waitFor(() => expect(screen.getByRole('textbox', { name: '消息内容' })).toHaveValue(''));
  });

  it('accepts a message when its selected Workspace disappears and leaves tool failure to that task', async () => {
    mockCommon({ conversations: [conversation], hosts: [targetHost] });
    vi.mocked(portalApi.hosts).mockResolvedValueOnce({ items: [targetHost] })
      .mockResolvedValue({ items: [{ ...targetHost, workspaces: [] }] });
    const accept = vi.spyOn(portalApi, 'acceptConversationMessage').mockImplementation(async (conversationID, body) => acceptedReceipt(conversationID, body));
    const user = userEvent.setup();
    const { client } = renderPage();
    await screen.findByRole('option', { name: /测试电脑/ });
    await user.selectOptions(screen.getByRole('combobox', { name: '选择目标电脑' }), targetHost.id);
    await user.selectOptions(screen.getByRole('combobox', { name: '选择目标 Workspace' }), 'w1234567');
    await client.invalidateQueries({ queryKey: ['assistant-hosts'] });
    expect(await screen.findByText(/所选 Workspace 状态待核对/)).toBeInTheDocument();
    const composer = screen.getByRole('textbox', { name: '消息内容' });
    await user.type(composer, '检查失效 Workspace');
    await user.click(screen.getByRole('button', { name: '发送' }));
    await waitFor(() => expect(accept).toHaveBeenCalledOnce());
    expect(accept.mock.calls[0]?.[1]).toMatchObject({ content: '检查失效 Workspace', workspace_id: 'w1234567' });
    expect(composer).toHaveValue('');
  });

  it('allows a checked text-only model and preserves input when configuration is explicitly absent', async () => {
    const textOnly = { ...model, capabilities: { ...model.capabilities, tools: false }, name: '仅文本模型' };
    mockCommon({ conversations: [conversation] });
    vi.mocked(portalApi.modelConnections).mockResolvedValue({ items: [textOnly], configuration_status: 'configured' });
    availableModelsResponse = { ...defaultAvailableModels, items: [{ id: textOnly.id, name: textOnly.name,
      model_name: textOnly.model_name, capabilities: textOnly.capabilities }] };
    const accept = vi.spyOn(portalApi, 'acceptConversationMessage').mockImplementation(async (conversationID, body) => acceptedReceipt(conversationID, body));
    const user = userEvent.setup();
    const view = renderPage();
    const selector = await screen.findByRole('combobox', { name: '选择 AI 模型' });
    expect(await within(selector).findByRole('option', { name: /仅文本模型/ })).toBeInTheDocument();
    await user.type(screen.getByRole('textbox', { name: '消息内容' }), '普通文字');
    await user.click(screen.getByRole('button', { name: '发送' }));
    await waitFor(() => expect(accept).toHaveBeenCalledOnce());

    availableModelsResponse = { ...defaultAvailableModels, items: [], configuration_status: 'not_configured', default_model_configured: false, default_model_id: null };
    await view.client.invalidateQueries({ queryKey: ['available-models'] });
    await waitFor(() => expect(view.client.getQueryData<AvailableModels>(['available-models'])?.configuration_status).toBe('not_configured'));
    const composer = screen.getByRole('textbox', { name: '消息内容' });
    await user.type(composer, '仍然保留');
    expect(screen.getByRole('button', { name: '发送' })).toBeEnabled();
    await user.click(screen.getByRole('button', { name: '发送' }));
    expect(await screen.findByText(/请先配置云端模型，输入内容已保留/)).toBeInTheDocument();
    expect(composer).toHaveValue('仍然保留');
    expect(accept).toHaveBeenCalledOnce();
  });

  it('accepts messages when model health is unknown and no explicit absence is confirmed', async () => {
    mockCommon({ conversations: [conversation] });
    vi.spyOn(portalApi, 'modelConnections').mockRejectedValue(new Error('模型检查暂不可用'));
    vi.spyOn(portalApi, 'preferences').mockResolvedValue({ assistant_name: 'Morde', selected_model_id: null, default_host_id: null, updated_at: timestamp });
    availableModelsResponse = { ...defaultAvailableModels, items: [], configuration_status: 'unknown', default_model_configured: false, default_model_id: null };
    const accept = vi.spyOn(portalApi, 'acceptConversationMessage');
    const user = userEvent.setup();
    renderPage();
    await screen.findByText(/无法读取模型：模型检查暂不可用/);
    const composer = screen.getByRole('textbox', { name: '消息内容' });
    await user.type(composer, '先保存这条消息');
    await user.click(screen.getByRole('button', { name: '发送' }));
    await waitFor(() => expect(accept).toHaveBeenCalledOnce());
    expect(accept.mock.calls[0]?.[1]).toMatchObject({ content: '先保存这条消息' });
    expect(accept.mock.calls[0]?.[1]).not.toHaveProperty('model_id');
    expect(composer).toHaveValue('');
  });

  it('submits each message independently while another submission is still running', async () => {
    mockCommon({ conversations: [conversation] });
    let finishFirst: (() => void) | undefined;
    const accept = vi.spyOn(portalApi, 'acceptConversationMessage').mockImplementation(async (conversationID, body) => {
      if (body.content === '第一条') await new Promise<void>((resolve) => { finishFirst = resolve; });
      return acceptedReceipt(conversationID, body);
    });
    const user = userEvent.setup();
    renderPage();
    const composer = await screen.findByRole('textbox', { name: '消息内容' });
    await user.type(composer, '第一条');
    await user.click(screen.getByRole('button', { name: '发送' }));
    await waitFor(() => expect(accept).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(composer).toHaveValue(''));
    await user.type(composer, '第二条');
    await user.click(screen.getByRole('button', { name: '发送' }));
    expect(composer).toBeEnabled();
    expect(accept).toHaveBeenCalledTimes(1);
    finishFirst?.();
    await waitFor(() => expect(accept).toHaveBeenCalledTimes(2));
    const first = accept.mock.calls[0]![1];
    const second = accept.mock.calls[1]![1];
    expect(first).not.toHaveProperty('mode');
    expect(second).not.toHaveProperty('mode');
    expect(second.previous_client_message_id).toBe(first.client_message_id);
    expect(second.client_message_id).not.toBe(first.client_message_id);
  });

  it('accepts a long text message as one receipt without turning it into a Task immediately', async () => {
    vi.stubGlobal('crypto', webcrypto);
    mockCommon({ conversations: [conversation] });
    const accept = vi.spyOn(portalApi, 'acceptConversationMessage').mockImplementation(async (conversationID, body) => acceptedReceipt(conversationID, body));
    const user = userEvent.setup();
    renderPage();
    await screen.findByRole('option', { name: /团队模型/ });
    const content = '中文a'.repeat(10000);
    const requests: Request[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = input instanceof Request ? new Request(input, init) : new Request(new URL(String(input), window.location.origin), init);
      requests.push(request);
      return Response.json({});
    }));
    fireEvent.change(screen.getByRole('textbox', { name: '消息内容' }), { target: { value: content } });
    await user.click(screen.getByRole('button', { name: '发送' }));
    await waitFor(() => expect(accept).toHaveBeenCalledOnce());
    expect(accept.mock.calls[0]?.[1].content).toBe(content);
    expect(accept.mock.calls[0]?.[1]).not.toHaveProperty('mode');
    expect(requests.some((request) => new URL(request.url).pathname.includes('/message-uploads/'))).toBe(true);
    await waitFor(() => expect(screen.queryByRole('button', { name: '核对并重试' })).not.toBeInTheDocument());
  });

  it('reconciles a lost response by message identity without sending it twice', async () => {
    mockCommon({ conversations: [conversation] });
    vi.spyOn(portalApi, 'task').mockResolvedValue(task);
    const accept = vi.spyOn(portalApi, 'acceptConversationMessage').mockRejectedValue(new ApiError({ message: '连接中断', code: 'network_error', status: 503 }));
    const user = userEvent.setup();
    renderPage();
    const composer = await screen.findByRole('textbox', { name: '消息内容' });
    await user.type(composer, '保存后断线');
    await user.click(screen.getByRole('button', { name: '发送' }));
    const retry = await screen.findByRole('button', { name: '核对并重试' });
    expect(composer).toBeEnabled();
    const body = accept.mock.calls[0]![1];
    vi.mocked(portalApi.conversationMessages).mockImplementation(async (_id, _cursor, clientID) => ({
      items: clientID ? [{ ...message, task_id: task.id, content: body.content, client_message_id: body.client_message_id, delivery_status: 'applied' }] : [], next_cursor: null,
    }));
    await user.click(retry);
    await waitFor(() => expect(screen.queryByRole('button', { name: '核对并重试' })).not.toBeInTheDocument());
    expect(accept).toHaveBeenCalledOnce();
    expect(portalApi.conversationMessages).toHaveBeenCalledWith(conversation.id, undefined, body.client_message_id,
      clientContract.capabilities.cloud_execution.service_instance_id, authUser.id);
  });

  it('submits a new message while an earlier outbox message is unresolved', async () => {
    mockCommon({ conversations: [conversation] });
    vi.spyOn(portalApi, 'task').mockResolvedValue(task);
    const accept = vi.spyOn(portalApi, 'acceptConversationMessage')
      .mockRejectedValueOnce(new ApiError({ message: '连接中断', code: 'network_error', status: 503 }))
      .mockImplementation(async (conversationID, body) => acceptedReceipt(conversationID, body));
    const user = userEvent.setup();
    const firstView = renderPage();
    await user.type(await screen.findByRole('textbox', { name: '消息内容' }), '第一条待确认');
    await user.click(screen.getByRole('button', { name: '发送' }));
    await screen.findByRole('button', { name: '核对并重试' });
    const firstID = accept.mock.calls[0]![1].client_message_id;
    firstView.unmount();
    firstView.client.clear();
    renderPage();
    await screen.findByText('第一条待确认');
    await user.type(screen.getByRole('textbox', { name: '消息内容' }), '后续消息');
    await user.click(screen.getByRole('button', { name: '发送' }));
    await waitFor(() => expect(accept).toHaveBeenCalledTimes(2));
    expect(accept.mock.calls[1]![1]).toMatchObject({ mode: 'new', content: '后续消息' });
    expect(accept.mock.calls[1]![1]).not.toHaveProperty('previous_client_message_id');
    expect(accept.mock.calls[1]![1].client_message_id).not.toBe(firstID);
    expect(screen.queryByText(/前一条消息尚未确认/)).not.toBeInTheDocument();
  });

  it('keeps a message locally when the server detects a different authenticated account', async () => {
    mockCommon({ conversations: [conversation] });
    const accept = vi.spyOn(portalApi, 'acceptConversationMessage').mockRejectedValue(
      new ApiError({ message: '账号身份不匹配', code: 'conflict', status: 409 }),
    );
    const user = userEvent.setup();
    renderPage();
    await user.type(await screen.findByRole('textbox', { name: '消息内容' }), '账号切换前的内容');
    await user.click(screen.getByRole('button', { name: '发送' }));
    expect(await screen.findByText(/账号、服务或消息状态已变化，消息保留在本机/)).toBeInTheDocument();
    expect(accept.mock.calls[0]?.[4]).toBe(authUser.id);
    expect(screen.getByRole('button', { name: '核对并重试' })).toBeEnabled();
    expect(screen.getByText('账号切换前的内容')).toBeInTheDocument();
  });

  it('uploads attachments before creating a Task and reuses upload keys after a retry', async () => {
    vi.stubGlobal('crypto', webcrypto);
    mockCommon({ conversations: [conversation] });
    const accept = vi.spyOn(portalApi, 'acceptConversationMessage').mockImplementation(async (conversationID, body) => acceptedReceipt(conversationID, body));
    const replace = vi.spyOn(portalApi, 'replacePersonalObject').mockResolvedValue({
      object_type: 'library_item', object_id: 'chat-upload', result_revision: 1, account_change_sequence: 1, replay: false,
    });
    const upload = vi.spyOn(portalApi, 'createPersonalAttachmentUpload').mockRejectedValueOnce(new Error('上传中断'))
      .mockResolvedValueOnce({ upload_id: 'pu_0123456789abcdef0123456789abcdef', state: 'active', chunk_count: 1, replay: false });
    const chunk = vi.spyOn(portalApi, 'putPersonalAttachmentChunk').mockResolvedValue({
      upload_id: 'pu_0123456789abcdef0123456789abcdef', sequence: 0, replay: false,
    });
    const commit = vi.spyOn(portalApi, 'commitPersonalAttachment').mockResolvedValue({
      attachment_id: 'pa_0123456789abcdef0123456789abcdef', object_type: 'library_item', object_id: 'chat-upload',
      result_revision: 2, account_change_sequence: 2, replay: false,
    });
    const associate = vi.spyOn(portalApi, 'associateConversationAttachment');
    const user = userEvent.setup();
    renderPage();
    await user.upload(await screen.findByLabelText('添加附件'), new File(['hello'], 'brief.txt', { type: 'text/plain' }));
    await user.type(screen.getByRole('textbox', { name: '消息内容' }), '请阅读附件');
    await user.click(screen.getByRole('button', { name: '发送' }));
    const retry = await screen.findByRole('button', { name: '核对并重试' });
    expect(await screen.findByText(/发送结果待确认：上传中断/)).toBeInTheDocument();
    expect(accept).not.toHaveBeenCalled();
    expect(screen.getByRole('textbox', { name: '消息内容' })).toBeEnabled();
    await user.click(retry);
    await waitFor(() => expect(accept).toHaveBeenCalledOnce());
    await waitFor(() => expect(screen.queryByRole('button', { name: '核对并重试' })).not.toBeInTheDocument());
    const body = accept.mock.calls[0]![1];
    expect(body.attachment_ids).toEqual(['pa_0123456789abcdef0123456789abcdef']);
    expect(body.mode).toBe('new');
    expect(associate).not.toHaveBeenCalled();
    expect(replace).toHaveBeenCalledTimes(2);
    expect(replace.mock.calls[1]?.[4]).toBe(replace.mock.calls[0]?.[4]);
    expect(upload).toHaveBeenCalledTimes(2);
    expect(upload.mock.calls[1]?.[1]).toBe(upload.mock.calls[0]?.[1]);
    const serviceID = clientContract.capabilities.cloud_execution.service_instance_id;
    expect(replace.mock.calls[1]?.slice(5)).toEqual([authUser.id, serviceID]);
    expect(upload.mock.calls[1]?.slice(2)).toEqual([serviceID, authUser.id]);
    expect(chunk.mock.calls[0]?.slice(4)).toEqual([serviceID, authUser.id]);
    expect(commit.mock.calls[0]?.slice(2)).toEqual([serviceID, authUser.id]);
  });

  it('recovers a streaming assistant message from the Task snapshot after conversation history expires', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    vi.spyOn(portalApi, 'task').mockResolvedValue({
      ...task, status: 'running', result: null, finished_at: null,
      assistant_message: {
        id: 'm8765432', conversation_id: conversation.id, sequence: 2, task_id: task.id, role: 'assistant',
        content: 'A persisted partial answer', status: 'streaming', created_at: timestamp,
      },
    });
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByRole('button', { name: /你好/ }));
    expect(await screen.findByText('A persisted partial answer')).toBeInTheDocument();
    expect(screen.getByText('正在生成…')).toHaveAttribute('role', 'status');
  });

  it('shows a completed task result as the answer when no assistant message was saved', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, result: '已完成整理。' });
    renderPage();

    const answer = await screen.findByText('已完成整理。');
    expect(answer.closest('article')).toHaveClass('chat-message-assistant');
    expect(screen.queryByText('已接收')).not.toBeInTheDocument();
  });

  it('does not describe an undelivered message in a stopped task as still waiting', async () => {
    mockCommon({ conversations: [conversation], messages: [{ ...message, delivery_status: 'pending' }] });
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, status: 'cancelled', result: null });
    renderPage();

    expect(await screen.findByText('任务已结束，消息未处理')).toBeInTheDocument();
    expect(screen.queryByText('已保存，等待接收')).not.toBeInTheDocument();
  });

  it.each([
    ['pending', '已受理，等待处理'], ['processing', '正在判断任务'],
    ['waiting_user', '等待补充或确认'], ['failed', '消息处理失败'],
  ] as const)('shows the actual routing state %s without requiring a Task', async (routingStatus, label) => {
    mockCommon({ conversations: [conversation], messages: [{ ...message, task_id: undefined, routing_status: routingStatus, delivery_status: 'pending' }] });
    renderPage();
    expect(await screen.findByText(label)).toBeInTheDocument();
  });

  it('retains the processing retry key when the response is lost and reuses it on retry', async () => {
    mockCommon({ conversations: [conversation], messages: [{ ...message, task_id: undefined,
      routing_status: 'failed', routing_failure_reason: 'model_unavailable', delivery_status: 'pending' }] });
    const retry = vi.spyOn(portalApi, 'retryConversationMessageProcessing')
      .mockRejectedValueOnce(new Error('连接中断')).mockResolvedValueOnce(undefined);
    const user = userEvent.setup();
    renderPage();
    expect(await screen.findByText('模型暂时不可用或用量待核对')).toBeInTheDocument();
    await user.click(await screen.findByRole('button', { name: '重试处理' }));
    await screen.findByText('连接中断');
    await user.click(screen.getByRole('button', { name: '重试处理' }));
    await waitFor(() => expect(retry).toHaveBeenCalledTimes(2));
    const first = retry.mock.calls[0]!;
    const second = retry.mock.calls[1]!;
    expect(first).toEqual(second);
    expect(first[0]).toBe(conversation.id);
    expect(first[1]).toBe(message.id);
    expect(first[2]).toBeTruthy();
    expect(first[4]).toBe(authUser.id);
    await screen.findByText('已请求重新处理消息。');
  });

  it('keeps an oversized accepted message visible and explains that it must be shortened', async () => {
    mockCommon({ conversations: [conversation], messages: [{ ...message, task_id: undefined,
      routing_status: 'failed', routing_failure_reason: 'message_too_long', delivery_status: 'pending' }] });
    renderPage();
    const explanation = await screen.findByText(/消息及上下文超过当前模型输入上限/);
    const article = explanation.closest<HTMLElement>('article')!;
    expect(within(article).getByText(message.content)).toBeInTheDocument();
    expect(within(article).getByText(/请缩短后重新发送/)).toBeInTheDocument();
    expect(within(article).queryByRole('button', { name: '重试处理' })).not.toBeInTheDocument();
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

  it('deduplicates combined replies only for the matching task, revision and result event', () => {
    const combined = { ...reply, id: 'mcombined', sources: [
      { user_message_id: message.id, task_id: task.id, task_revision: 2, event_sequence: 5, content: reply.content },
    ] };
    const snapshot = { ...reply, id: 'msnapshot', status: 'completed' as const, task_revision: 2, result_event_sequence: 5 };
    expect(mergeTaskAssistantMessage([combined], { ...task, assistant_message: snapshot })).toHaveLength(1);
    expect(mergeTaskAssistantMessage([combined], { ...task, assistant_message: { ...snapshot, task_revision: 3 } })).toHaveLength(2);
    expect(mergeTaskAssistantMessage([combined], { ...task, assistant_message: { ...snapshot, result_event_sequence: 6 } })).toHaveLength(2);
    expect(mergeTaskAssistantMessage([combined], { ...task, assistant_message: { ...snapshot, result_event_sequence: undefined } })).toHaveLength(2);
  });

  it('merges persisted streaming messages without losing sequence or reverting interrupted content', () => {
    const saved = { ...reply, id: 'mstream01', content: '前缀', sequence: 4,
      output_status: 'streaming' as const, output_task_revision: 2 };
    const snapshot = { ...reply, id: saved.id, content: '前缀和后续', status: 'streaming' as const, task_revision: 2 };
    const merged = mergeTaskAssistantMessage([saved], { ...task, assistant_message: snapshot });
    expect(merged).toHaveLength(1);
    expect(merged[0]).toMatchObject({ sequence: 4, content: '前缀和后续', snapshotStatus: 'streaming' });

    const interrupted = { ...saved, output_status: 'interrupted' as const };
    expect(mergeTaskAssistantMessage([interrupted], { ...task, assistant_message: snapshot })[0])
      .toMatchObject({ sequence: 4, content: '前缀', snapshotStatus: 'interrupted' });
    expect(mergeTaskAssistantMessage([interrupted], undefined)[0]?.snapshotStatus).toBe('interrupted');
    expect(mergeTaskAssistantMessage([saved], { ...task, assistant_message: { ...snapshot, task_revision: 3 } })[0]?.content).toBe('前缀');
  });

  it('does not render a final Task result while its conversation reply is pending or already published', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, result_message_pending: true });
    renderPage();
    expect(await screen.findByText('任务已完成，正在整理回复。')).toBeInTheDocument();
    expect(screen.queryByText(reply.content)).not.toBeInTheDocument();
    const snapshot = { ...reply, status: 'completed' as const, task_revision: 0, result_event_sequence: 5 };
    expect(mergeTaskAssistantMessage([], { ...task, assistant_message: snapshot, result_message_pending: true })).toHaveLength(0);
    expect(mergeTaskAssistantMessage([], { ...task, assistant_message: snapshot, result_message_published: true })).toHaveLength(0);
    expect(mergeTaskAssistantMessage([], { ...task, assistant_message: { ...snapshot, status: 'streaming' }, result_message_pending: true })).toHaveLength(1);
    const oldInterrupted = { ...snapshot, id: 'moldreply', status: 'interrupted' as const, task_revision: 0, result_event_sequence: undefined };
    const newReply = { ...reply, id: 'mnewreply', sources: [{ user_message_id: message.id, task_id: task.id,
      task_revision: 1, event_sequence: 7, content: reply.content }] };
    expect(mergeTaskAssistantMessage([], { ...task, current_revision: 1, assistant_message: oldInterrupted, result_message_pending: true })).toHaveLength(1);
    expect(mergeTaskAssistantMessage([newReply], { ...task, current_revision: 1, assistant_message: oldInterrupted, result_message_published: true })).toHaveLength(2);
  });

  it('shows a failed task snapshot as interrupted instead of still generating', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    const failedTask = {
      ...task,
      status: 'failed' as const,
      result: null,
      finished_at: timestamp,
      assistant_message: {
        id: 'm2345678', conversation_id: conversation.id, sequence: 2, task_id: task.id,
        role: 'assistant' as const, content: '部分回复', status: 'streaming' as const, created_at: timestamp,
      },
    };
    vi.spyOn(portalApi, 'task').mockResolvedValue(failedTask);
    renderPage();

    expect(await screen.findByText('回复已中断')).toBeInTheDocument();
    expect(screen.getByRole('alert')).toHaveTextContent('任务执行失败。');
    expect(screen.queryByText('正在生成…')).not.toBeInTheDocument();
    expect(screen.queryByText('AI 正在处理…')).not.toBeInTheDocument();
  });

  it('keeps task read errors identifiable when several tasks share a conversation', async () => {
    const secondMessage = { ...message, id: 'm2345678', task_id: 'q7654321', content: '另一项任务' };
    mockCommon({ conversations: [conversation], messages: [message, secondMessage] });
    vi.spyOn(portalApi, 'task').mockImplementation(async (id) => { throw new Error(`读取 ${id} 失败`); });
    renderPage();

    expect(await screen.findByText(`无法读取任务 ${task.id} 的状态：读取 ${task.id} 失败`)).toBeInTheDocument();
    expect(screen.getByText('无法读取任务 q7654321 的状态：读取 q7654321 失败')).toBeInTheDocument();
  });

  it('accepts a message even when the runtime is stopping', async () => {
    mockCommon({ conversations: [conversation] });
    vi.mocked(portalApi.execution).mockResolvedValue({ owner_id: 'u1234567', state: 'stopping', epoch: 3, pending_task_count: 1,
      runtime: { state: 'stopping', runtime_id: 'rt1234567', runtime_generation: 1, max_concurrent_tasks: 4, active_task_count: 1 }, updated_at: timestamp });
    const accept = vi.spyOn(portalApi, 'acceptConversationMessage').mockImplementation(async (conversationID, body) => acceptedReceipt(conversationID, body));
    const user = userEvent.setup();
    renderPage();
    await user.type(await screen.findByRole('textbox', { name: '消息内容' }), '继续提问');
    expect(await screen.findByText(/AI 服务正在停止；消息仍可先发送/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '发送' }));
    await waitFor(() => expect(accept).toHaveBeenCalledOnce());
  });

  it('continues the conversation without an explicit task reference', async () => {
    const running = { ...task, status: 'running' as const, result: null, finished_at: null };
    mockCommon({ conversations: [conversation], messages: [message] });
    vi.spyOn(portalApi, 'task').mockResolvedValue(running);
    const accept = vi.spyOn(portalApi, 'acceptConversationMessage').mockImplementation(async (conversationID, body) => acceptedReceipt(conversationID, body));
    const user = userEvent.setup();
    renderPage();
    await user.click(await screen.findByRole('button', { name: '继续对话' }));
    await user.type(screen.getByRole('textbox', { name: '消息内容' }), '追加说明');
    await user.click(screen.getByRole('button', { name: '发送' }));
    await waitFor(() => expect(accept).toHaveBeenCalledWith(conversation.id,
      expect.objectContaining({ content: '追加说明' }), expect.any(String), expect.any(String), authUser.id));
    expect(accept.mock.calls[0]![1]).not.toHaveProperty('reference_task_id');
  });

  it('does not offer a task reference button and sends follow-up text as a new Task', async () => {
    const running = { ...task, status: 'running' as const, result: null, finished_at: null };
    mockCommon({ conversations: [conversation], messages: [message] });
    vi.spyOn(portalApi, 'task').mockResolvedValue(running);
    const accept = vi.spyOn(portalApi, 'acceptConversationMessage').mockImplementation(async (conversationID, body) => acceptedReceipt(conversationID, body));
    const user = userEvent.setup();
    renderPage();
    expect(await screen.findByRole('button', { name: '继续对话' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '补充任务' })).not.toBeInTheDocument();
    await user.type(screen.getByRole('textbox', { name: '消息内容' }), '追加说明');
    await user.click(screen.getByRole('button', { name: '发送' }));
    await waitFor(() => expect(accept).toHaveBeenCalledOnce());
    expect(accept.mock.calls[0]![1]).toMatchObject({ mode: 'new', content: '追加说明' });
    expect(accept.mock.calls[0]![1]).not.toHaveProperty('reference_task_id');
    expect(accept.mock.calls[0]![1]).not.toHaveProperty('host_id');
    expect(accept.mock.calls[0]![1]).not.toHaveProperty('workspace_id');
  });

  it('shows meaningful labels for built-in search and page-reading steps', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    const runningTask = { ...task, status: 'running' as const, result: null, finished_at: null, current_step: 's1234567', steps: [
      { id: 's1234567', sequence: 1, kind: 'tool' as const, status: 'succeeded' as const,
        title: 'Run cloud dsh MCP tool: morde_web_search', summary: '', created_at: timestamp },
      { id: 's2345678', sequence: 2, kind: 'tool' as const, status: 'running' as const,
        title: 'Run cloud dsh MCP tool: morde_web_read', summary: '', created_at: timestamp },
      { id: 's3456789', sequence: 3, kind: 'tool' as const, status: 'succeeded' as const,
        title: 'Run Hostless Workspace tool: morde_workspace_read', summary: '', created_at: timestamp },
      { id: 's4567890', sequence: 4, kind: 'tool' as const, status: 'running' as const,
        title: 'Run cloud dsh tool: internal_tool_name', summary: '', created_at: timestamp },
      { id: 's5678901', sequence: 5, kind: 'progress' as const, status: 'succeeded' as const,
        title: '开始处理', summary: '正在处理你的请求。', created_at: timestamp },
    ] };
    vi.spyOn(portalApi, 'task').mockResolvedValue(runningTask);
    const user = userEvent.setup();
    renderPage();
    const panel = await screen.findByRole('region', { name: `任务 ${task.id} 的工作过程` });
    await user.click(within(panel).getByText('查看工作过程'));
    expect(within(panel).getAllByText('搜索网页').length).toBeGreaterThan(0);
    expect(within(panel).getByText('读取网页')).toBeInTheDocument();
    expect(within(panel).getByText('读取工作区文件')).toBeInTheDocument();
    expect(within(panel).getByText('使用工具')).toBeInTheDocument();
    expect(within(panel).getByText('开始处理')).toBeInTheDocument();
    expect(within(panel).queryByText(/morde_web_/)).not.toBeInTheDocument();
    expect(within(panel).queryByText(/Hostless|internal_tool_name/)).not.toBeInTheDocument();
  });

  it('keeps event cursors and invalidation isolated by task', async () => {
    const first = { ...task, status: 'running' as const, result: null, finished_at: null, event_sequence: 3 };
    const second = { ...first, id: 'q7654321', objective: '第二项任务', event_sequence: 7 };
    mockCommon({ conversations: [conversation], messages: [message, { ...message, id: 'm2345678', task_id: second.id }] });
    vi.mocked(portalApi.conversationTasks).mockResolvedValue({ items: [first, second], next_cursor: null });
    vi.spyOn(portalApi, 'task').mockImplementation(async (id) => id === second.id ? second : first);
    const sources: Array<{ url: string; listeners: Map<string, EventListener> }> = [];
    class TestEventSource {
      onopen: ((event: Event) => void) | null = null;
      onerror: ((event: Event) => void) | null = null;
      listeners = new Map<string, EventListener>();
      constructor(public url: string) { sources.push(this); }
      addEventListener(type: string, listener: EventListener) { this.listeners.set(type, listener); }
      close() {}
    }
    vi.stubGlobal('EventSource', TestEventSource);
    const { client } = renderPage();
    await waitFor(() => expect(sources).toHaveLength(2));
    expect(sources.map((source) => source.url)).toEqual(expect.arrayContaining([
      `/api/tasks/${first.id}/events?after=3`, `/api/tasks/${second.id}/events?after=7`,
    ]));
    const invalidate = vi.spyOn(client, 'invalidateQueries');
    const source = sources.find((item) => item.url.includes(second.id))!;
    await act(async () => source.listeners.get('task.step')?.(new MessageEvent('task.step', { data: JSON.stringify({ task_id: first.id, sequence: 8 }) })));
    expect(invalidate).not.toHaveBeenCalled();
    await act(async () => source.listeners.get('task.step')?.(new MessageEvent('task.step', { data: JSON.stringify({ task_id: second.id, sequence: 8 }) })));
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ['task', second.id] });
    const calls = invalidate.mock.calls.length;
    await act(async () => source.listeners.get('task.step')?.(new MessageEvent('task.step', { data: JSON.stringify({ task_id: second.id, sequence: 8 }) })));
    expect(invalidate).toHaveBeenCalledTimes(calls);
  });

  it('reloads conversation messages after an expired event cursor and resumes from the Task snapshot', async () => {
    const running = { ...task, status: 'running' as const, result: null, finished_at: null,
      event_sequence: 3, current_revision: 1, desired_revision: 1, current_revision_started: false };
    const recovered = { ...running, event_sequence: 11, current_revision_started: true,
      assistant_message: { ...reply, id: 'mcurrent', content: '当前回复', status: 'streaming' as const, task_revision: 1 } };
    const interrupted = { ...reply, id: 'moldreply', content: '断线期间保留的上一轮回复',
      output_status: 'interrupted' as const, output_task_revision: 0 };
    mockCommon({ conversations: [conversation], messages: [message] });
    let history: ConversationMessage[] = [message];
    vi.mocked(portalApi.conversationMessages).mockImplementation(async () => ({ items: history, next_cursor: null }));
    const getTask = vi.spyOn(portalApi, 'task').mockResolvedValue(running);
    const sources: Array<{ url: string; listeners: Map<string, EventListener>; closed: boolean }> = [];
    class TestEventSource {
      onopen: ((event: Event) => void) | null = null;
      onerror: ((event: Event) => void) | null = null;
      listeners = new Map<string, EventListener>();
      closed = false;
      constructor(public url: string) { sources.push(this); }
      addEventListener(type: string, listener: EventListener) { this.listeners.set(type, listener); }
      close() { this.closed = true; }
    }
    vi.stubGlobal('EventSource', TestEventSource);
    renderPage();
    expect(await screen.findByText('任务已保存，正在等待开始。')).toBeInTheDocument();
    await waitFor(() => expect(sources).toHaveLength(1));

    history = [message, interrupted];
    getTask.mockResolvedValue(recovered);
    await act(async () => sources[0]!.listeners.get('task.snapshot_required')?.(
      new MessageEvent('task.snapshot_required', { data: JSON.stringify({ sequence: 11 }) }),
    ));

    expect(await screen.findByText('断线期间保留的上一轮回复')).toBeInTheDocument();
    expect(await screen.findByText('当前回复')).toBeInTheDocument();
    await waitFor(() => expect(sources).toHaveLength(2));
    expect(sources[0]!.closed).toBe(true);
    expect(sources[1]!.url).toBe(`/api/tasks/${task.id}/events?after=11`);
  });

  it('places task progress after the latest message and the stop button beside send', async () => {
    mockCommon({ conversations: [conversation], messages: [message, reply] });
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, status: 'running', result: null, finished_at: null });
    renderPage();

    const progress = await screen.findByText('AI 正在处理…');
    const lastMessage = screen.getByText(reply.content);
    expect(lastMessage.compareDocumentPosition(progress) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    const composer = screen.getByRole('region', { name: '发送云端任务消息' });
    expect(within(composer).getByRole('button', { name: '停止任务' })).toBeEnabled();
    expect(within(composer).getByRole('button', { name: '发送' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '暂停任务' })).not.toBeInTheDocument();
  });

  it.each([
    { current_revision: 1, desired_revision: 1, current_revision_started: false, text: '任务已保存，正在等待开始。' },
    { current_revision: 1, desired_revision: 1, current_revision_started: true, text: '开始处理你的请求。' },
    { current_revision: 1, desired_revision: 2, current_revision_started: true, text: '正在调整任务，等待开始新要求。' },
  ])('shows the confirmed state of the current revision', async ({ text, ...evidence }) => {
    mockCommon({ conversations: [conversation], messages: [message] });
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, ...evidence, status: 'running', result: null, finished_at: null });
    renderPage();
    expect(await screen.findByText(text)).toBeInTheDocument();
  });

  it('shows a recovery interruption and lets the user stop instead of waiting for delivery forever', async () => {
    const pending = { ...message, id: 'm2345678', content: '继续', delivery_status: 'pending' as const };
    mockCommon({ conversations: [conversation], messages: [message, pending] });
    const interrupted = { ...task, status: 'waiting_user' as const, result: null, finished_at: null,
      waiting_reason: 'dsh_recovery_unresolved', current_step: 's1234567', steps: [{
        id: 's1234567', sequence: 1, kind: 'verification' as const, status: 'unknown' as const,
        title: 'Review cloud task recovery',
        summary: 'The model request could not be verified (category: provider_rate_limited); retry after checking the model connection.',
        created_at: timestamp,
      }] };
    const cancelled = { ...interrupted, status: 'cancelled' as const, waiting_reason: null, finished_at: timestamp };
    vi.spyOn(portalApi, 'task').mockResolvedValueOnce(interrupted).mockResolvedValue(cancelled);
    const stop = vi.spyOn(portalApi, 'taskAction').mockResolvedValue({
      task_id: task.id, action: 'cancel', status: 'cancelled', unsettled_operation_ids: [],
    });
    renderPage();

    expect(await screen.findByText(/AI 运行中断，无法确认本次执行结果/)).toBeInTheDocument();
    expect(screen.getByText('模型服务正在限流，稍后再试。')).toBeInTheDocument();
    expect(screen.queryByText(/category:/)).not.toBeInTheDocument();
    expect(screen.queryByText('Review cloud task recovery')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '等待接收' })).not.toBeInTheDocument();
    await userEvent.setup().click(screen.getByRole('button', { name: '停止任务' }));
    await waitFor(() => expect(stop).toHaveBeenCalledWith(task.id,
      { action: 'cancel', expected_status: 'waiting_user' }, expect.any(String)));
    await waitFor(() => expect(screen.getByRole('textbox', { name: '消息内容' })).toBeEnabled());
  });

  it('explains a model input limit after a Task has already started', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, status: 'waiting_user', result: null, finished_at: null,
      waiting_reason: 'model_context_limit', current_step: 's1234567', steps: [{
        id: 's1234567', sequence: 1, kind: 'verification', status: 'unknown', title: '消息超过模型输入上限',
        summary: '消息及上下文超过当前模型输入上限，原文已保留；请缩短后重新发送。', created_at: timestamp,
      }] });
    renderPage();
    expect(await screen.findByText(/消息及上下文超过当前模型输入上限/)).toHaveTextContent('原文已保留；请缩短后重新发送');
    expect(screen.queryByText(/AI 运行中断，无法确认本次执行结果/)).not.toBeInTheDocument();
  });

  it('shows a computer problem only on the Task that attempted to use it', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, status: 'waiting_user', result: null, finished_at: null,
      waiting_reason: 'computer_unavailable', current_step: null, steps: [] });
    renderPage();
    expect(await screen.findByText(/所选云端电脑当前不可用或无法确认访问权限/)).toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: '消息内容' })).toBeEnabled();
  });

  it('allows a new task while another task waits for approval', async () => {
    mockCommon({ conversations: [conversation], messages: [message, reply] });
    const approval = { id: 'k1234567', version: 4, kind: 'approval' as const, title: '写入文件', object: '工作区文件', content: '创建报告.md', impact: '会新增一个文件' };
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, status: 'waiting_user', result: null,
      waiting_reason: 'approval', pending_request: approval, finished_at: null });
    const accept = vi.spyOn(portalApi, 'acceptConversationMessage');
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByRole('button', { name: /你好/ }));
    expect(screen.getByRole('textbox', { name: '消息内容' })).toBeEnabled();
    expect(screen.getByRole('button', { name: '停止任务' })).toBeEnabled();
    expect(accept).not.toHaveBeenCalled();
  });

  it('resumes task events without displaying internal event history', async () => {
    mockCommon({ conversations: [conversation], messages: [message, reply] });
    const runningTask = { ...task, status: 'running' as const, result: null, finished_at: null, current_step: 's1234567', steps: [{
      id: 's1234567', sequence: 1, kind: 'tool' as const, status: 'running' as const, title: '正在整理资料', summary: '', created_at: timestamp,
    }] };
    vi.spyOn(portalApi, 'task').mockResolvedValue(runningTask);
    let sourceURL = '';
    let sourceOpen: ((event: Event) => void) | null = null;
    let sourceError: ((event: Event) => void) | null = null;
    const eventHandlers = new Map<string, (event: MessageEvent<string>) => void>();
    class TestEventSource {
      get onopen() { return sourceOpen; }
      set onopen(handler: ((event: Event) => void) | null) { sourceOpen = handler; }
      get onerror() { return sourceError; }
      set onerror(handler: ((event: Event) => void) | null) { sourceError = handler; }
      constructor(url: string) { sourceURL = url; }
      addEventListener(type: string, handler: EventListener) { eventHandlers.set(type, handler as (event: MessageEvent<string>) => void); }
      close() {}
    }
    vi.stubGlobal('EventSource', TestEventSource);
    const user = userEvent.setup();
    renderPage();
    await user.click(await screen.findByRole('button', { name: /你好/ }));

    await waitFor(() => expect(sourceURL).not.toBe(''));
    expect(sourceURL).toBe(`/api/tasks/${task.id}/events?after=${task.event_sequence}`);
    await act(async () => sourceOpen?.(new Event('open')));
    expect(screen.getAllByText('正在整理资料').length).toBeGreaterThan(0);
    await act(async () => eventHandlers.get('task.step')?.(new MessageEvent('task.step', { data: JSON.stringify({
      task_id: task.id, sequence: 1, type: 'task.step', created_at: timestamp, payload: { secret: 'must stay private' },
    }) })));
    expect(screen.queryByText('任务步骤已更新')).not.toBeInTheDocument();
    expect(screen.queryByText('must stay private')).not.toBeInTheDocument();
    await act(async () => sourceError?.(new Event('error')));
    expect(screen.getByText('连接中断，正在重连…')).toBeInTheDocument();
    await act(async () => sourceOpen?.(new Event('open')));
    expect(screen.queryByText('连接中断，正在重连…')).not.toBeInTheDocument();
    await act(async () => eventHandlers.get('task.step')?.(new MessageEvent('task.step', { data: JSON.stringify({
      task_id: task.id, sequence: 1, type: 'task.step', created_at: timestamp, payload: {},
    }) })));
    expect(screen.queryByText('任务步骤已更新')).not.toBeInTheDocument();
  });

  it('keeps a message locally when service identity is unavailable', async () => {
    mockCommon({ conversations: [conversation] });
    vi.mocked(portalApi.capabilities).mockRejectedValue(new Error('连接中断'));
    const accept = vi.spyOn(portalApi, 'acceptConversationMessage');
    const user = userEvent.setup();
    renderPage();
    const composer = await screen.findByRole('textbox', { name: '消息内容' });
    await user.type(composer, '等待服务身份');
    expect(screen.getByRole('button', { name: '发送' })).toBeEnabled();
    await user.click(screen.getByRole('button', { name: '发送' }));
    expect(await screen.findByText(/等待确认原服务和账号/)).toBeInTheDocument();
    expect(accept).not.toHaveBeenCalled();
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

  it('can resume a previously paused Task with its current status', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    const pausedTask = { ...task, status: 'paused' as const, result: null, finished_at: null };
    vi.spyOn(portalApi, 'task').mockResolvedValue(pausedTask);
    const action = vi.spyOn(portalApi, 'taskAction')
      .mockResolvedValue({ task_id: task.id, action: 'resume', status: 'queued', unsettled_operation_ids: [] });
    const user = userEvent.setup();
    renderPage();
    await user.click(await screen.findByRole('button', { name: /你好/ }));
    expect(await screen.findByRole('button', { name: '继续任务' })).toBeEnabled();
    await user.click(screen.getByRole('button', { name: '继续任务' }));
    await waitFor(() => expect(action).toHaveBeenCalledWith(task.id,
      { action: 'resume', expected_status: 'paused' }, expect.any(String)));
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
