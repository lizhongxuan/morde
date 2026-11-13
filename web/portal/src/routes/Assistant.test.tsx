import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { StrictMode } from 'react';
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
import { Assistant, assistantMainConversationCacheKey, assistantRecentConversationKey, mergeTaskAssistantMessage, resolveTaskObservationURL } from './Assistant';
import { assistantComposerDraftScope, assistantDraftStorage } from './assistant-drafts';
import type { AssistantComposerDraft } from './assistant-drafts';

const authUser = vi.hoisted(() => ({ id: 'u1234567' }));
vi.mock('../app/auth', () => ({ useAuth: () => ({ user: authUser }) }));
const pdfMock = vi.hoisted(() => ({ getDocument: vi.fn() }));
vi.mock('pdfjs-dist', () => ({ GlobalWorkerOptions: { workerSrc: '' }, getDocument: pdfMock.getDocument }));
const tesseractMock = vi.hoisted(() => ({ createWorker: vi.fn() }));
vi.mock('tesseract.js', () => ({ createWorker: tesseractMock.createWorker, OEM: { LSTM_ONLY: 1 } }));

const timestamp = '2026-10-22T08:00:00.000Z';
type ConversationMessage = Awaited<ReturnType<typeof portalApi.conversationMessages>>['items'][number];
type Conversation = Awaited<ReturnType<typeof portalApi.conversations>>['items'][number];
type Task = Awaited<ReturnType<typeof portalApi.task>>;
type TaskFileFetch = Awaited<ReturnType<typeof portalApi.taskFileFetches>>['items'][number];
type TaskSummary = components['schemas']['TaskSummary'];
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
  step_count: 0, completed_step_count: 0,
  budget: { active_seconds: 600, tool_calls: 10, model_tokens: 1000 }, usage: { active_seconds: 2, tool_calls: 0, model_tokens: 12 },
  dependencies: [], child_jobs: [], result_review: null,
  created_at: timestamp, updated_at: timestamp, result: reply.content, waiting_reason: null, steps: [], batch_items: [],
  artifacts: [], pending_request: null, clarification_request: null, stop_request: null, assistant_message: null, finished_at: timestamp,
} satisfies Awaited<ReturnType<typeof portalApi.task>>;
const taskFileFetch = {
  fetch_id: 'o1234567', task_id: task.id, source_installation_id: 'i1234567', source_device_name: '本机 Mac',
  source_directory_name: null, failure_reason: null,
  relative_path: 'reports/brief.pdf', task_revision: 1, source_version: 'a'.repeat(64), source_sha256: 'b'.repeat(64),
  media_type: 'application/pdf', size_bytes: 1024, confirmed_bytes: 512, status: 'transferring' as const, waiting_reason: null,
  content_id: null, expires_at: timestamp, content_state: 'none' as const, parse_status: 'pending' as const, parse_error: null, deleted_at: null,
} satisfies TaskFileFetch;

// Older server snapshots predate the required K2 projection fields.
function asLegacyTask(value: Task): Task {
  const legacy: Record<string, unknown> = { ...value };
  delete legacy.dependencies;
  delete legacy.child_jobs;
  delete legacy.result_review;
  return legacy as unknown as Task;
}
let acceptedMessageSequence = 0;
let assistantDraftRecords = new Map<string, AssistantComposerDraft>();

function acceptedReceipt(conversationID: string, body: AcceptConversationMessageRequest): Awaited<ReturnType<typeof portalApi.acceptConversationMessage>> {
  const sequence = ++acceptedMessageSequence;
  const submitted: ConversationMessage = {
    id: `m${String(sequence).padStart(7, '0')}`, conversation_id: conversationID,
    sequence: message.sequence + sequence, role: 'user', content: body.content,
    client_message_id: body.client_message_id, attachment_ids: [], routing_status: 'pending',
    delivery_status: 'pending', created_at: timestamp,
  };
  return { message: submitted, replayed: false };
}

function renderPage(initialState: unknown = null) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const view = render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter initialEntries={[{ pathname: '/assistant', state: initialState }]}><Assistant /></MemoryRouter></ToastProvider></QueryClientProvider>);
  return { ...view, client };
}

async function findReadyComposer() {
  const composer = await screen.findByRole('textbox', { name: '消息内容' });
  await waitFor(() => expect(composer).toBeEnabled());
  return composer;
}

async function openTaskDynamics() {
  if (!document.querySelector('#assistant-task-panel')) {
    await userEvent.setup().click(await screen.findByRole('button', { name: '打开侧边栏' }));
  }
}

async function findTaskActivity(taskID: string) {
  await openTaskDynamics();
  const user = userEvent.setup();
  const backButton = document.querySelector<HTMLButtonElement>('.task-panel-detail-nav button');
  if (backButton) await user.click(backButton);
  await waitFor(() => expect(document.querySelector(`#assistant-task-panel .task-panel-list:not([hidden]) [data-task-id="${taskID}"]`))
    .not.toBeNull());
  const card = document.querySelector<HTMLElement>(`#assistant-task-panel .task-panel-list:not([hidden]) [data-task-id="${taskID}"]`)!;
  const pagination = card.closest<HTMLElement>('.screen-pagination');
  const pageButtons = pagination?.querySelectorAll<HTMLButtonElement>('.screen-pagination__controls button[aria-label^="第 "]') ?? [];
  for (const pageButton of pageButtons) {
    if (!card.closest('.screen-pagination__item')?.hasAttribute('hidden')) break;
    await user.click(pageButton);
  }
  await user.click(within(card).getByRole('button', { name: /查看/ }));
  await waitFor(() => expect(document.querySelector(`#assistant-task-panel .task-panel-detail .task-activity[data-task-id="${taskID}"]`))
    .not.toBeNull());
  return document.querySelector<HTMLElement>(`#assistant-task-panel .task-panel-detail .task-activity[data-task-id="${taskID}"]`)!;
}

function mockCommon(options: { conversations?: Conversation[]; messages?: ConversationMessage[]; mainConversation?: MainConversation; cloudTasks?: boolean; capabilities?: Awaited<ReturnType<typeof portalApi.capabilities>>; hosts?: typeof targetHost[]; tasks?: TaskSummary[]; defaultHostID?: string | null } = {}) {
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
  vi.spyOn(portalApi, 'conversationPreview').mockResolvedValue({ items: [], next_cursor: null });
  vi.spyOn(portalApi, 'acceptConversationMessage').mockImplementation(async (conversationID, body) => acceptedReceipt(conversationID, body));
  vi.spyOn(portalApi, 'conversationTasks').mockResolvedValue({ items: [], next_cursor: null, pending_count: 0 });
  vi.spyOn(portalApi, 'tasks').mockResolvedValue({ items: options.tasks ?? [], next_cursor: null });
  vi.spyOn(portalApi, 'task').mockImplementation(async (id) => ({ ...task, id }));
  vi.spyOn(portalApi, 'taskFileFetches').mockResolvedValue({ items: [] });
  vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [model, otherModel], configuration_status: 'configured' });
  vi.spyOn(portalApi, 'mcpConnectors').mockResolvedValue({ items: [] });
  vi.spyOn(portalApi, 'preferences').mockResolvedValue({ assistant_name: 'Morde', selected_model_id: model.id, default_host_id: options.defaultHostID ?? null, updated_at: timestamp });
  vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: options.hosts ?? [] });
  vi.spyOn(portalApi, 'sharedHosts').mockResolvedValue({ items: [] });
  vi.spyOn(portalApi, 'execution').mockResolvedValue({ owner_id: 'u1234567', state: 'enabled', epoch: 1, pending_task_count: 0,
    runtime: { state: 'running', runtime_id: 'rt1234567', runtime_generation: 1, max_concurrent_tasks: 4, active_task_count: 0 }, updated_at: timestamp });
}

describe('Assistant durable tasks', () => {
  beforeAll(() => server.listen());
  beforeEach(() => {
    availableModelsResponse = defaultAvailableModels;
    acceptedMessageSequence = 0;
    tesseractMock.createWorker.mockReset();
    const values = new Map<string, PendingConversationMessage[]>();
    assistantDraftRecords = new Map();
    vi.spyOn(assistantDraftStorage, 'get').mockImplementation(async (scope, id) => assistantDraftRecords.get(`${scope}\u0000${id}`) ?? null);
    vi.spyOn(assistantDraftStorage, 'list').mockImplementation(async (scope) => [...assistantDraftRecords.entries()]
      .filter(([key]) => key.startsWith(`${scope}\u0000`)).map(([, draft]) => draft));
    vi.spyOn(assistantDraftStorage, 'put').mockImplementation(async (scope, draft) => { assistantDraftRecords.set(`${scope}\u0000${draft.id}`, draft); });
    vi.spyOn(assistantDraftStorage, 'remove').mockImplementation(async (scope, id) => { assistantDraftRecords.delete(`${scope}\u0000${id}`); });
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

    expect(await screen.findByRole('button', { name: '打开侧边栏' })).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByText('任务动态')).not.toBeInTheDocument();
    expect(document.querySelector(`#assistant-task-panel .task-activity[data-task-id="${task.id}"]`)).not.toBeInTheDocument();
    const sidebar = await screen.findByRole('complementary', { name: '对话' });
    expect(within(sidebar).getByRole('button', { name: '新建旁聊' })).toBeEnabled();
    expect(within(sidebar).queryByRole('button', { name: '＋ 新对话' })).not.toBeInTheDocument();
    const composer = screen.getByRole('region', { name: '发送对话消息' });
    expect(within(composer).getByLabelText('选择目标电脑')).toBeInTheDocument();
    expect(within(composer).getByLabelText('选择 AI 模型')).toBeInTheDocument();
    expect(within(composer).getByLabelText('消息内容')).toBeInTheDocument();
    expect(Array.from(composer.children, (child) => child.tagName)).toEqual(['FORM']);
  });

  it('connects an authorized cloud computer to the selected conversation through a one-time form ticket', async () => {
    mockCommon({ conversations: [conversation], hosts: [targetHost] });
    vi.spyOn(portalApi, 'issueHostEmbedTicket').mockResolvedValue({
      exchange_url: `${targetHost.url}/__ch/embed-auth`, ticket: 'A'.repeat(43), embed_nonce: 'B'.repeat(32),
      targets: ['browser', 'terminal', 'workspace', 'files', 'vscode'], expires_at: '2026-11-12T03:00:00.000Z',
    });
    const submissions: Array<{ action: string; target: string; fields: Record<string, string> }> = [];
    const submit = vi.spyOn(HTMLFormElement.prototype, 'submit').mockImplementation(function(this: HTMLFormElement) {
      submissions.push({ action: this.action, target: this.target,
        fields: Object.fromEntries(Array.from(new FormData(this).entries()).map(([key, value]) => [key, String(value)])) });
    });
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByRole('button', { name: '打开侧边栏' }));
    const sidebar = await screen.findByRole('complementary', { name: '当前会话侧边栏' });
    await user.click(within(sidebar).getByRole('button', { name: '云端电脑' }));
    await user.click(within(sidebar).getByRole('button', { name: '连接到此会话' }));

    await waitFor(() => expect(submit).toHaveBeenCalledOnce());
    expect(portalApi.issueHostEmbedTicket).toHaveBeenCalledWith(targetHost.id, {
      conversation_id: conversation.id, target: 'browser', targets: ['browser', 'terminal', 'workspace', 'files', 'vscode'],
    });
    expect(submissions).toEqual([{
      action: `${targetHost.url}/__ch/embed-auth`, target: `morde-cloud-${'B'.repeat(32)}`,
      fields: { ticket: 'A'.repeat(43), embed_nonce: 'B'.repeat(32) },
    }]);
    expect(new URL(submissions[0]!.action).search).toBe('');
    expect(screen.getByLabelText('选择目标电脑')).toHaveValue('');

    const frame = within(sidebar).getByTitle('当前会话云端电脑工作台') as HTMLIFrameElement;
    const postMessage = vi.spyOn(frame.contentWindow!, 'postMessage');
    const handshake = { channel: 'morde.workbench.embed', version: 1, nonce: 'B'.repeat(32) };
    act(() => window.dispatchEvent(new MessageEvent('message', {
      origin: new URL(targetHost.url).origin, source: frame.contentWindow,
      data: { ...handshake, type: 'ready' },
    })));
    expect(postMessage).toHaveBeenCalledWith({ ...handshake, type: 'init', conversation_id: conversation.id, visible: true }, new URL(targetHost.url).origin);
    act(() => window.dispatchEvent(new MessageEvent('message', {
      origin: new URL(targetHost.url).origin, source: frame.contentWindow,
      data: { ...handshake, type: 'state', conversation_id: conversation.id, host_id: targetHost.id, workbench_ready: true,
        control_state: 'user', control_lease_active: true, control_lease_held_by_me: true },
    })));
    expect(await within(sidebar).findByText('工作台已就绪')).toBeInTheDocument();
    expect(document.querySelector('.assistant-layout')).toHaveClass('assistant-layout-computer-expanded');

    await user.click(within(sidebar).getByRole('button', { name: '文件' }));
    expect(postMessage).toHaveBeenLastCalledWith({ ...handshake, type: 'visibility', visible: false }, new URL(targetHost.url).origin);
    expect(document.querySelector('.assistant-layout')).not.toHaveClass('assistant-layout-computer-expanded');
    await user.click(within(sidebar).getByRole('button', { name: '云端电脑' }));
    expect(postMessage).toHaveBeenLastCalledWith({ ...handshake, type: 'visibility', visible: true }, new URL(targetHost.url).origin);
    expect(document.querySelector('.assistant-layout')).toHaveClass('assistant-layout-computer-expanded');
    act(() => window.dispatchEvent(new MessageEvent('message', {
      origin: new URL(targetHost.url).origin, source: frame.contentWindow,
      data: { ...handshake, type: 'state', conversation_id: conversation.id, host_id: targetHost.id, workbench_ready: true,
        control_state: 'ai', control_lease_active: false, control_lease_held_by_me: false },
    })));
    expect(document.querySelector('.assistant-layout')).not.toHaveClass('assistant-layout-computer-expanded');
  });

  it('uses the configured default in the computer sidebar without binding ordinary chat messages to it', async () => {
    mockCommon({ conversations: [conversation], hosts: [targetHost], defaultHostID: targetHost.id });
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByRole('button', { name: '打开侧边栏' }));
    const sidebar = await screen.findByRole('complementary', { name: '当前会话侧边栏' });
    await user.click(within(sidebar).getByRole('button', { name: '云端电脑' }));

    expect(within(sidebar).getByRole('combobox', { name: '选择侧栏云端电脑' })).toHaveValue(targetHost.id);
    expect(screen.getByLabelText('选择目标电脑')).toHaveValue('');
  });

  it('only requests shared computer tabs allowed by current permissions', async () => {
    const sharedHost = {
      id: 'h7654321', name: '共享电脑', state: 'running', url: 'https://h7654321.hosts.test',
      permissions: ['desktop:view', 'terminal:control', 'workspace:file:read'],
    } satisfies Awaited<ReturnType<typeof portalApi.sharedHosts>>['items'][number];
    mockCommon({ conversations: [conversation] });
    vi.mocked(portalApi.sharedHosts).mockResolvedValue({ items: [sharedHost] });
    const issueTicket = vi.spyOn(portalApi, 'issueHostEmbedTicket').mockResolvedValue({
      exchange_url: `${sharedHost.url}/__ch/embed-auth`, ticket: 'C'.repeat(43), embed_nonce: 'D'.repeat(32),
      targets: ['browser', 'workspace'], expires_at: '2026-11-12T03:00:00.000Z',
    });
    vi.spyOn(HTMLFormElement.prototype, 'submit').mockImplementation(() => undefined);
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByRole('button', { name: '打开侧边栏' }));
    const sidebar = await screen.findByRole('complementary', { name: '当前会话侧边栏' });
    await user.click(within(sidebar).getByRole('button', { name: '云端电脑' }));
    await user.selectOptions(within(sidebar).getByRole('combobox', { name: '选择侧栏云端电脑' }), sharedHost.id);
    await user.click(within(sidebar).getByRole('button', { name: '连接到此会话' }));

    await waitFor(() => expect(issueTicket).toHaveBeenCalledWith(sharedHost.id, {
      conversation_id: conversation.id, target: 'browser', targets: ['browser', 'workspace'],
    }));
    expect(screen.getByLabelText('选择目标电脑')).toHaveValue('');
  });

  it('renders newest-first API pages chronologically and keeps task activity out of the chat', async () => {
    mockCommon({ conversations: [conversation], messages: [reply, message] });
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, status: 'running', result: null, finished_at: null });
    renderPage();

    await screen.findByText('你好！', { exact: true });
    const messageList = (await screen.findByText('你好！', { exact: true })).closest<HTMLElement>('.message-list');
    expect(messageList).not.toBeNull();
    const articles = within(messageList!).getAllByRole('article');
    expect(articles.map((article) => article.textContent)).toEqual(expect.arrayContaining([
      expect.stringContaining('你好'), expect.stringContaining('你好！'),
    ]));
    expect(articles[0]?.querySelector('p')).toHaveTextContent(/^你好$/);
    expect(articles[1]?.querySelector('.chat-markdown')).toHaveTextContent(/^你好！$/);
    const activity = await findTaskActivity(task.id);
    expect(messageList!.querySelector('.task-activity')).not.toBeInTheDocument();
    expect(activity).toHaveTextContent('进行中');
  });

  it('shows PC file-fetch progress without tickets and refreshes the deletion receipt', async () => {
    const transferring = { ...taskFileFetch, ticket: 'ticket-secret', ticket_hash: 'ticket-hash-secret' } as TaskFileFetch;
    const retained: TaskFileFetch = {
      ...taskFileFetch, fetch_id: 'o1234568', relative_path: 'reports/retained.pdf', status: 'completed',
      confirmed_bytes: 1024, content_id: 'o7654321', content_state: 'retained', parse_status: 'raw_only',
    };
    const records = new Map<string, TaskFileFetch[]>([[task.id, [transferring, retained]]]);
    mockCommon({ conversations: [conversation], messages: [message] });
    vi.mocked(portalApi.taskFileFetches).mockImplementation(async (taskID) => ({ items: records.get(taskID) ?? [] }));
    const removeContent = vi.spyOn(portalApi, 'deleteTaskFileFetchContent').mockImplementation(async (taskID, fetchID) => {
      records.set(taskID, (records.get(taskID) ?? []).map((item) => item.fetch_id === fetchID
        ? { ...item, content_state: 'deleted' as const, deleted_at: timestamp } : item));
    });
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(true);
    const user = userEvent.setup();
    renderPage();

    const activity = await findTaskActivity(task.id);
    expect(await within(activity).findByRole('progressbar', { name: '读取本机文件进度：reports/brief.pdf' })).toHaveAttribute('value', '512');
    expect(activity).toHaveTextContent('本机 Mac');
    expect(activity).not.toHaveTextContent(taskFileFetch.source_version);
    expect(activity).toHaveTextContent('云端临时副本：保留中');
    expect(activity).not.toHaveTextContent('ticket-secret');
    expect(activity).not.toHaveTextContent('ticket-hash-secret');

    await user.click(within(activity).getByRole('button', { name: '删除云端副本' }));
    expect(confirm).toHaveBeenCalledWith(expect.stringContaining('删除后无法恢复'));
    await waitFor(() => expect(removeContent).toHaveBeenCalledWith(task.id, retained.fetch_id));
    expect(await within(activity).findByText(/云端临时副本：已删除/)).toBeInTheDocument();
    expect(activity).toHaveTextContent('删除于');
  });

  it('associates each Morde reply with its timestamp and makes it keyboard accessible', async () => {
    mockCommon({ conversations: [conversation], messages: [message, reply] });
    renderPage();

    const response = await screen.findByRole('article', { name: 'Morde 的回复' });
    expect(response).toHaveAttribute('tabindex', '0');
    const time = within(response.parentElement!).getByLabelText(/回复时间：/);
    expect(time).toHaveAttribute('datetime', reply.created_at);
    expect(time).toHaveClass('assistant-reply-time');
  });

  it('renders assistant Markdown while keeping user message text literal', async () => {
    mockCommon({ conversations: [conversation], messages: [
      { ...message, content: '**literal user text**' },
      { ...reply, content: '**已完成**\n\n- 搜索\n- 汇总' },
    ] });
    renderPage();

    await screen.findByText('已完成', {}, { timeout: 5000 });
    const articles = await screen.findAllByRole('article');
    expect(within(articles[0]!).getByText('**literal user text**')).toBeInTheDocument();
    expect(within(articles[0]!).queryByText('literal user text', { selector: 'strong' })).not.toBeInTheDocument();
    expect(within(articles[1]!).getByText('已完成').tagName).toBe('STRONG');
    expect(within(articles[1]!).getByRole('list').querySelectorAll('li')).toHaveLength(2);
  });

  it('shows a download link for a personal attachment restored from the conversation', async () => {
    const attachmentID = 'pa_0123456789abcdef0123456789abcdef';
    mockCommon({ conversations: [conversation], messages: [{ ...message, task_id: undefined, attachment_ids: [attachmentID] }] });
    renderPage();
    const link = await screen.findByRole('link', { name: '下载附件 1' });
    expect(link).toHaveAttribute('href', `/api/personal-data/attachments/${attachmentID}`);
    expect(link).toHaveAttribute('download');
  });

  it('loads conversation history on demand, deduplicates overlapping pages, and preserves the scroll anchor', async () => {
    mockCommon({ conversations: [conversation] });
    const older = { ...message, task_id: undefined };
    const newer = { ...reply, task_id: undefined };
    vi.mocked(portalApi.conversationMessages).mockImplementation(async (_id, cursor) => cursor
      ? { items: [older, newer], next_cursor: null }
      : { items: [newer], next_cursor: 'older-page' });
    const user = userEvent.setup();
    renderPage();
    expect(await screen.findByText('你好！')).toBeInTheDocument();
    expect(screen.getAllByText('你好！')).toHaveLength(1);
    expect(screen.queryByText('你好', { selector: 'p' })).not.toBeInTheDocument();
    expect(portalApi.conversationMessages).toHaveBeenCalledTimes(1);
    const view = document.querySelector<HTMLElement>('.conversation-view')!;
    Object.defineProperty(view, 'scrollHeight', { configurable: true, get: () => document.querySelectorAll('.chat-message-row').length * 100 });
    Object.defineProperty(view, 'clientHeight', { configurable: true, value: 50 });
    view.scrollTop = 20;
    await user.click(screen.getByRole('button', { name: '加载更早消息' }));
    expect(await screen.findByText('你好', { selector: 'p' })).toBeInTheDocument();
    expect(screen.getAllByText('你好！')).toHaveLength(1);
    expect(portalApi.conversationMessages).toHaveBeenCalledWith(conversation.id, 'older-page');
    await waitFor(() => expect(view.scrollTop).toBe(120));
  });

  it('isolates composer text, model, and next-message host across side conversations', async () => {
    mockCommon({ hosts: [targetHost] });
    const user = userEvent.setup();
    renderPage();

    const composer = await findReadyComposer();
    await user.click(screen.getByRole('button', { name: '新建旁聊' }));
    await user.type(composer, '甲会话草稿');
    await user.selectOptions(screen.getByRole('combobox', { name: '选择 AI 模型' }), otherModel.id);
    await user.selectOptions(screen.getByRole('combobox', { name: '选择目标电脑' }), targetHost.id);
    await waitFor(() => expect([...assistantDraftRecords.values()].some((draft) => draft.content === '甲会话草稿')).toBe(true));

    await user.click(screen.getByRole('button', { name: '新建旁聊' }));
    await waitFor(() => expect(composer).toHaveValue(''));
    await user.type(composer, '乙会话草稿');
    await user.selectOptions(screen.getByRole('combobox', { name: '选择 AI 模型' }), model.id);
    await user.selectOptions(screen.getByRole('combobox', { name: '选择目标电脑' }), '');
    await waitFor(() => expect([...assistantDraftRecords.values()].some((draft) => draft.content === '乙会话草稿')).toBe(true));

    await user.click(screen.getByText('旁聊'));
    await user.click(await screen.findByRole('button', { name: '恢复草稿：甲会话草稿' }));
    await waitFor(() => expect(composer).toHaveValue('甲会话草稿'));
    expect(screen.getByRole('combobox', { name: '选择 AI 模型' })).toHaveValue(otherModel.id);
    expect(screen.getByRole('combobox', { name: '选择目标电脑' })).toHaveValue(targetHost.id);

    await user.click(await screen.findByRole('button', { name: '恢复草稿：乙会话草稿' }));
    await waitFor(() => expect(composer).toHaveValue('乙会话草稿'));
    expect(screen.getByRole('combobox', { name: '选择 AI 模型' })).toHaveValue(model.id);
    expect(screen.getByRole('combobox', { name: '选择目标电脑' })).toHaveValue('');
  });

  it('restores a saved local side-conversation draft after remounting the assistant page', async () => {
    mockCommon();
    const user = userEvent.setup();
    const first = renderPage();
    await user.type(await findReadyComposer(), '刷新后还要恢复');
    await waitFor(() => expect([...assistantDraftRecords.values()].some((draft) => draft.content === '刷新后还要恢复')).toBe(true));
    first.unmount();

    renderPage();
    await user.click(await screen.findByText('旁聊'));
    await user.click(await screen.findByRole('button', { name: '恢复草稿：刷新后还要恢复' }));
    expect(await findReadyComposer()).toHaveValue('刷新后还要恢复');
  });

  it('hydrates an existing conversation draft when StrictMode replays effects', async () => {
    mockCommon({ conversations: [conversation] });
    const scope = assistantComposerDraftScope(clientContract.capabilities.cloud_execution.service_instance_id, authUser.id);
    assistantDraftRecords.set(`${scope}\u0000${conversation.id}`, {
      id: conversation.id, conversationID: conversation.id, content: '恢复之前的会话草稿', modelID: '', hostID: '',
      mcpConnectorIDs: [], attachments: [], updatedAt: Date.now(),
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });

    render(<StrictMode><QueryClientProvider client={client}><ToastProvider><MemoryRouter initialEntries={['/assistant']}><Assistant /></MemoryRouter></ToastProvider></QueryClientProvider></StrictMode>);

    expect(await findReadyComposer()).toHaveValue('恢复之前的会话草稿');
  });

  it('keeps a newer composer edit when a delayed draft read completes', async () => {
    mockCommon({ conversations: [conversation], mainConversation: { status: 'ready', conversation_id: conversation.id, version: 1 } });
    let finishDraft!: (draft: AssistantComposerDraft | null) => void;
    vi.spyOn(assistantDraftStorage, 'get').mockImplementation(() => new Promise((resolve) => { finishDraft = resolve; }));
    const user = userEvent.setup();
    renderPage();

    const composer = await screen.findByRole('textbox', { name: '消息内容' });
    await waitFor(() => expect(finishDraft).toBeTypeOf('function'));
    fireEvent.change(composer, { target: { value: '较新的输入' } });
    await act(async () => finishDraft({
      id: conversation.id, conversationID: conversation.id, content: '旧草稿', modelID: '', hostID: '',
      mcpConnectorIDs: [], attachments: [], updatedAt: Date.now(),
    }));

    await waitFor(() => expect(composer).toBeEnabled());
    expect(composer).toHaveValue('较新的输入');
    await user.type(composer, '仍可继续输入');
    expect(composer).toHaveValue('较新的输入仍可继续输入');
  });

  it('keeps a locally staged attachment in its draft across a page remount without uploading it', async () => {
    mockCommon();
    const forbiddenTransfer = () => Promise.reject(new Error('attachment upload must not run in this test'));
    const replaceObject = vi.spyOn(portalApi, 'replacePersonalObject').mockImplementation(forbiddenTransfer);
    const createUpload = vi.spyOn(portalApi, 'createPersonalAttachmentUpload').mockImplementation(forbiddenTransfer);
    const putChunk = vi.spyOn(portalApi, 'putPersonalAttachmentChunk').mockImplementation(forbiddenTransfer);
    const commitUpload = vi.spyOn(portalApi, 'commitPersonalAttachment').mockImplementation(forbiddenTransfer);
    const user = userEvent.setup();
    const first = renderPage();
    await findReadyComposer();
    await user.upload(screen.getByLabelText('添加附件'), new File(['local sample'], 'note.txt', { type: 'text/plain' }));
    await waitFor(() => expect([...assistantDraftRecords.values()].some((draft) => draft.attachments.some((attachment) => attachment.name === 'note.txt'))).toBe(true));
    first.unmount();

    renderPage();
    await user.click(await screen.findByText('旁聊'));
    await user.click(await screen.findByRole('button', { name: '恢复草稿：新旁聊草稿' }));
    expect(await screen.findByText('note.txt')).toBeInTheDocument();
    expect(replaceObject).not.toHaveBeenCalled();
    expect(createUpload).not.toHaveBeenCalled();
    expect(putChunk).not.toHaveBeenCalled();
    expect(commitUpload).not.toHaveBeenCalled();
  });

  it('keeps model errors and retry controls in the scrollable conversation rather than the input area', async () => {
    mockCommon();
    vi.mocked(portalApi.modelConnections).mockRejectedValueOnce(new Error('读取失败')).mockResolvedValue({ items: [model], configuration_status: 'configured' });
    const user = userEvent.setup();
    renderPage();

    const error = await screen.findByText(/无法读取模型：读取失败/);
    const composer = screen.getByRole('region', { name: '发送对话消息' });
    expect(composer).not.toContainElement(error);
    expect(Array.from(composer.children, (child) => child.tagName)).toEqual(['FORM']);
    await user.click(within(error).getByRole('button', { name: '重试' }));
    await waitFor(() => expect(screen.queryByText(/无法读取模型：读取失败/)).not.toBeInTheDocument());
  });

  it('shows one fixed retry notification when several assistant reads fail with network errors', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    const networkFailure = () => Promise.reject(new SyntaxError('服务器响应格式异常'));
    vi.spyOn(portalApi, 'capabilities').mockImplementation(networkFailure);
    vi.spyOn(portalApi, 'conversations').mockImplementation(networkFailure);
    vi.spyOn(portalApi, 'modelConnections').mockImplementation(networkFailure);
    vi.spyOn(portalApi, 'mcpConnectors').mockImplementation(networkFailure);
    vi.spyOn(portalApi, 'preferences').mockImplementation(networkFailure);
    vi.spyOn(portalApi, 'hosts').mockImplementation(networkFailure);
    renderPage();

    const error = await screen.findByRole('alert');
    expect(error).toHaveTextContent('网络异常，部分对话数据暂时无法读取。');
    expect(error.closest('.assistant-page')).toBeNull();
    expect(screen.getAllByRole('alert')).toHaveLength(1);
    expect(within(error).getByRole('button', { name: '重试' })).toBeInTheDocument();
    expect(screen.queryByText('工具连接暂不可用')).not.toBeInTheDocument();
    expect(screen.queryByText(/无法读取模型：/)).not.toBeInTheDocument();
  });

  it('keeps a main-conversation API error out of the page layout and allows retry', async () => {
    mockCommon();
    vi.spyOn(portalApi, 'mainConversation')
      .mockRejectedValueOnce(new Error('主会话接口暂不可用'))
      .mockResolvedValue({ status: 'selection_required', conversation_id: null, version: 0 });
    const user = userEvent.setup();
    renderPage();

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('无法刷新服务端主会话');
    expect(alert.closest('.toast-region')).toBeInTheDocument();
    expect(alert.closest('.assistant-page')).toBeNull();
    await user.click(within(alert).getByRole('button', { name: '重试' }));
    await waitFor(() => expect(screen.queryByText(/无法刷新服务端主会话/)).not.toBeInTheDocument());
  });

  it('builds a same-Host task-auth URL and rejects unsafe Workbench origins', () => {
    const url = new URL(resolveTaskObservationURL('opaque-ticket-123456', targetHost.url, targetHost.id, 'portal.example.test'));
    expect(url.origin).toBe(new URL(targetHost.url).origin);
    expect(url.pathname).toBe('/__ch/task-auth');
    expect(url.searchParams.get('ticket')).toBe('opaque-ticket-123456');
    expect(() => resolveTaskObservationURL('opaque-ticket-123456', 'http://evil.example.test', targetHost.id, 'portal.example.test'))
      .toThrow('Workbench 地址不符合安全规则');
    expect(() => resolveTaskObservationURL('opaque-ticket-123456', `${targetHost.url}/other`, targetHost.id, 'portal.example.test'))
      .toThrow('Workbench 地址不符合安全规则');
    expect(() => resolveTaskObservationURL('short', targetHost.url, targetHost.id, 'portal.example.test'))
      .toThrow('Task 观察票据格式无效');
    const local = new URL(resolveTaskObservationURL('opaque-ticket-123456', `http://${targetHost.id}.hosts.localhost`, targetHost.id, '127.0.0.1'));
    expect(local.origin).toBe(`http://${targetHost.id}.hosts.localhost`);
    expect(local.pathname).toBe('/__ch/task-auth');
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

    await findTaskActivity(task.id);
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
    mockCommon({ conversations: [conversation, recent], messages: [message, reply], mainConversation: { status: 'ready', conversation_id: conversation.id, version: 3 } });
    const serviceInstanceID = clientContract.capabilities.cloud_execution.service_instance_id;
    localStorage.setItem(assistantRecentConversationKey(serviceInstanceID, authUser.id), recent.id);
    const getTask = vi.spyOn(portalApi, 'task').mockResolvedValue(task);

    renderPage();

    expect(await screen.findByRole('heading', { name: '主会话' })).toBeInTheDocument();
    expect(await screen.findByText('本机最近查看')).toBeInTheDocument();
    await waitFor(() => expect(getTask).toHaveBeenCalledWith(task.id));
    expect(await screen.findByText('你好！')).toBeInTheDocument();
  });

  it('keeps the last known main conversation visible when conversation history cannot load', async () => {
    const serviceInstanceID = clientContract.capabilities.cloud_execution.service_instance_id;
    localStorage.setItem(assistantMainConversationCacheKey(authUser.id), JSON.stringify({ conversationID: conversation.id, serviceInstanceID }));
    mockCommon();
    vi.spyOn(portalApi, 'conversations').mockRejectedValue(new TypeError('Failed to fetch'));
    vi.spyOn(portalApi, 'mainConversation').mockRejectedValue(new TypeError('Failed to fetch'));
    vi.spyOn(portalApi, 'conversation').mockRejectedValue(new TypeError('Failed to fetch'));

    renderPage();

    const sidebar = screen.getByRole('complementary', { name: '对话' });
    const main = await within(sidebar).findByRole('button', { name: /^主会话/ });
    expect(main).toHaveClass('is-selected');
    expect(within(sidebar).getByText('旁聊历史暂不可用')).toBeInTheDocument();
    expect(within(sidebar).queryByText('对话列表暂不可用')).not.toBeInTheDocument();
    expect(within(sidebar).getByRole('button', { name: '新建旁聊' })).toBeEnabled();
  });

  it('does not use a cached main conversation from another service instance', async () => {
    localStorage.setItem(assistantMainConversationCacheKey(authUser.id), JSON.stringify({
      conversationID: conversation.id, serviceInstanceID: 'different-service',
    }));
    mockCommon();
    vi.spyOn(portalApi, 'conversations').mockRejectedValue(new TypeError('Failed to fetch'));
    vi.spyOn(portalApi, 'mainConversation').mockRejectedValue(new TypeError('Failed to fetch'));

    renderPage();

    const sidebar = screen.getByRole('complementary', { name: '对话' });
    expect(await within(sidebar).findByText('旁聊历史暂不可用')).toBeInTheDocument();
    expect(within(sidebar).queryByRole('button', { name: /^主会话/ })).not.toBeInTheDocument();
  });

  it('pins the main conversation and reveals side conversations five at a time in latest activity order', async () => {
    const main = { ...conversation, id: 'c0000001', title: '我的主会话', updated_at: '2026-10-22T07:00:00.000Z' };
    const side = Array.from({ length: 15 }, (_, index) => ({
      ...conversation,
      id: `c${String(index + 10).padStart(7, '0')}`,
      is_main: false,
      title: `旁路 ${index + 1}`,
      updated_at: new Date(Date.parse(timestamp) - index * 60_000).toISOString(),
    }));
    mockCommon({ mainConversation: { status: 'ready', conversation_id: main.id, version: 1 } });
    vi.spyOn(portalApi, 'conversation').mockResolvedValue(main);
    const list = vi.spyOn(portalApi, 'conversations').mockImplementation(async (cursor) => cursor === 'cursor-2'
      ? { items: side.slice(5, 10), next_cursor: 'cursor-3' }
      : cursor === 'cursor-3' ? { items: side.slice(10), next_cursor: null }
        : { items: [side[4]!, side[0]!, side[3]!, side[1]!, side[2]!], next_cursor: 'cursor-2' });
    const user = userEvent.setup();

    renderPage();

    const sidebar = screen.getByRole('complementary', { name: '对话' });
    expect(await within(sidebar).findByRole('button', { name: /^主会话/ })).toBeInTheDocument();
    expect(sidebar.querySelector('.conversation-row')?.textContent).toContain('主会话');
    const sideGroup = within(sidebar).getByText('旁聊').closest('details')!;
    expect(sideGroup.open).toBe(false);
    expect(sideGroup).not.toHaveAttribute('open');
    await user.click(within(sideGroup).getByText('旁聊'));
    expect(sideGroup.open).toBe(true);
    expect(within(sideGroup).getAllByRole('button').filter((button) => button.classList.contains('conversation-item')))
      .toHaveLength(5);
    expect(within(sideGroup).getByRole('button', { name: /旁路 1/ })).toBeInTheDocument();
    expect(within(sideGroup).queryByRole('button', { name: /旁路 6/ })).not.toBeInTheDocument();

    await user.click(within(sideGroup).getByRole('button', { name: '更多' }));

    expect(await within(sideGroup).findByRole('button', { name: /旁路 10/ })).toBeInTheDocument();
    expect(within(sideGroup).getAllByRole('button').filter((button) => button.classList.contains('conversation-item')))
      .toHaveLength(10);
    expect(within(sideGroup).queryByRole('button', { name: /旁路 11/ })).not.toBeInTheDocument();
    await user.click(within(sideGroup).getByRole('button', { name: '更多' }));
    expect(await within(sideGroup).findByRole('button', { name: /旁路 15/ })).toBeInTheDocument();
    expect(within(sideGroup).getAllByRole('button').filter((button) => button.classList.contains('conversation-item')))
      .toHaveLength(15);
    expect(within(sideGroup).queryByRole('button', { name: /旁路 16/ })).not.toBeInTheDocument();
    expect(list).toHaveBeenNthCalledWith(1, undefined, 5);
    expect(list).toHaveBeenNthCalledWith(2, 'cursor-2', 5);
    expect(list).toHaveBeenNthCalledWith(3, 'cursor-3', 5);

    const createConversation = vi.spyOn(portalApi, 'createConversation');
    await user.click(within(sidebar).getByRole('button', { name: '新建旁聊' }));
    expect(await screen.findByRole('heading', { name: '今天想聊些什么？' })).toBeInTheDocument();
    expect(createConversation).not.toHaveBeenCalled();
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

    expect(await findReadyComposer()).toHaveValue('Draft a project outline');
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

    await screen.findByRole('heading', { name: '主会话' });
    await user.click(screen.getByRole('button', { name: '新建旁聊' }));
    expect(screen.getByRole('heading', { name: '今天想聊些什么？' })).toBeInTheDocument();
    expect(localStorage.getItem(key)).toBeNull();

    view.unmount();
    renderPage();
    expect(await screen.findByRole('heading', { name: '主会话' })).toBeInTheDocument();
  });

  it('initializes an empty main conversation with a stable ensure request', async () => {
    mockCommon({ mainConversation: { status: 'uninitialized', conversation_id: null, version: 0 } });
    const ensure = vi.mocked(portalApi.ensureMainConversation);
    renderPage();

    expect(await screen.findByRole('heading', { name: '主会话' })).toBeInTheDocument();
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
    await user.type(await findReadyComposer(), '新会话');
    await user.click(screen.getByRole('button', { name: '发送' }));
    await waitFor(() => expect(accept).toHaveBeenCalledOnce());
    expect(create).toHaveBeenCalledWith({}, expect.any(String), clientContract.capabilities.cloud_execution.service_instance_id, authUser.id);
    expect(accept.mock.calls[0]?.[1].time_zone).toBe(Intl.DateTimeFormat().resolvedOptions().timeZone);
  });

  it('does not accept the same draft twice while it is being saved locally', async () => {
    mockCommon({ conversations: [conversation] });
    const accept = vi.mocked(portalApi.acceptConversationMessage);
    const user = userEvent.setup();
    renderPage();

    await user.type(await findReadyComposer(), '快速重复提交');
    const composer = screen.getByRole('region', { name: '发送对话消息' });
    const form = composer.querySelector('form');
    expect(form).not.toBeNull();
    fireEvent.submit(form!);
    fireEvent.submit(form!);

    await waitFor(() => expect(accept).toHaveBeenCalledOnce());
    expect(accept.mock.calls[0]?.[1].content).toBe('快速重复提交');
  });

  it('accepts messages when cloud Task execution is not enabled', async () => {
    const legacy = clientContract.capabilities.cloud_execution;
    mockCommon({ conversations: [conversation], cloudTasks: false, capabilities: legacy });
    const accept = vi.spyOn(portalApi, 'acceptConversationMessage').mockImplementation(async (conversationID, body) => acceptedReceipt(conversationID, body));
    const user = userEvent.setup();
    renderPage();
    await user.type(await findReadyComposer(), '旧服务中的待发送消息');
    await user.click(screen.getByRole('button', { name: '发送' }));
    await waitFor(() => expect(accept).toHaveBeenCalledOnce());
    expect(legacy.supported_features).not.toContain('conversation_auto_routing_v1');
    expect(await screen.findByText(/服务端尚未确认支持对话自动处理/)).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole('textbox', { name: '消息内容' })).toHaveValue(''));
    await user.type(await findReadyComposer(), '第二条待发送消息');
    expect(screen.getByRole('button', { name: '发送' })).toBeEnabled();
    expect(accept.mock.calls[0]![1].content).toBe('旧服务中的待发送消息');
  });

  it('rechecks service capability and retries the same locally saved message after an upgrade', async () => {
    const ready = clientContract.capabilities.cloud_execution;
    const legacy = { ...ready, supported_features: ready.supported_features.filter((feature) => feature !== 'conversation_auto_routing_v1') };
    mockCommon({ conversations: [conversation], capabilities: legacy });
    vi.mocked(portalApi.capabilities).mockReset()
      .mockResolvedValueOnce(legacy)
      .mockResolvedValueOnce(legacy)
      .mockResolvedValue({ ...ready, supported_features: [...ready.supported_features, 'conversation_auto_routing_v1'] });
    const accept = vi.spyOn(portalApi, 'acceptConversationMessage')
      .mockRejectedValueOnce(new ApiError({ message: '服务尚不支持自动分流，请升级后重试', code: 'conversation_auto_routing_unavailable', status: 404 }))
      .mockImplementation(async (conversationID, body) => acceptedReceipt(conversationID, body));
    const user = userEvent.setup();
    renderPage();
    await user.type(await findReadyComposer(), '旧服务升级后恢复');
    await user.click(screen.getByRole('button', { name: '发送' }));
    expect(await screen.findByText(/服务尚不支持自动分流，请升级后重试/)).toBeInTheDocument();
    const originalRequest = accept.mock.calls[0]!;
    await user.click(screen.getByRole('button', { name: '核对并重试' }));
    await waitFor(() => expect(screen.queryByRole('button', { name: '核对并重试' })).not.toBeInTheDocument());
    expect(accept).toHaveBeenCalledTimes(2);
    expect(accept.mock.calls[1]).toEqual(originalRequest);
  });

  it('accepts a message with a selected computer and stable identity', async () => {
    mockCommon({ conversations: [conversation], hosts: [targetHost] });
    vi.mocked(portalApi.mcpConnectors).mockResolvedValue({ items: [mcpConnector] });
    const accept = vi.spyOn(portalApi, 'acceptConversationMessage').mockImplementation(async (conversationID, body) => acceptedReceipt(conversationID, body));
    const user = userEvent.setup();
    renderPage();
    const composer = await findReadyComposer();
    await screen.findByRole('option', { name: /测试电脑/ });
    await user.selectOptions(screen.getByRole('combobox', { name: '选择目标电脑' }), targetHost.id);
    expect(screen.queryByRole('combobox', { name: '选择目标 Workspace' })).not.toBeInTheDocument();
    await user.click(screen.getByText('工具', { selector: 'summary' }));
    await user.click(await screen.findByRole('checkbox', { name: '日历工具' }));
    await user.type(composer, '检查云端电脑文件');
    await user.click(screen.getByRole('button', { name: '发送' }));
    await waitFor(() => expect(accept).toHaveBeenCalledOnce());
    const [conversationID, body, key, serviceID, actorID] = accept.mock.calls[0]!;
    expect(conversationID).toBe(conversation.id);
    expect(body).toMatchObject({ content: '检查云端电脑文件', model_id: model.id, host_id: targetHost.id,
      mcp_connector_ids: [mcpConnector.id], client_message_id: key });
    expect(body).not.toHaveProperty('workspace_id');
    expect(body).not.toHaveProperty('mode');
    expect(serviceID).toBe(clientContract.capabilities.cloud_execution.service_instance_id);
    expect(actorID).toBe(authUser.id);
    await waitFor(() => expect(screen.getByRole('textbox', { name: '消息内容' })).toHaveValue(''));
  });

  it('accepts a message when the explicitly selected host is stopped', async () => {
    const stoppedHost = { ...targetHost, state: 'stopped' as const };
    mockCommon({ conversations: [conversation], hosts: [stoppedHost] });
    const accept = vi.spyOn(portalApi, 'acceptConversationMessage')
      .mockImplementation(async (conversationID, body) => acceptedReceipt(conversationID, body));
    const user = userEvent.setup();
    renderPage();

    const hostSelector = await screen.findByRole('combobox', { name: '选择目标电脑' });
    await findReadyComposer();
    await user.selectOptions(hostSelector, stoppedHost.id);
    expect(await screen.findByText(/目标电脑暂不可用：已停止/)).toBeInTheDocument();
    await user.type(await findReadyComposer(), '先受理这条对话');
    expect(screen.getByRole('button', { name: '发送' })).toBeEnabled();
    await user.click(screen.getByRole('button', { name: '发送' }));

    await waitFor(() => expect(accept).toHaveBeenCalledOnce());
    expect(accept.mock.calls[0]![1]).toMatchObject({ content: '先受理这条对话', host_id: stoppedHost.id });
  });

  it('offers explicit voice input in the composer without starting recognition automatically', async () => {
    mockCommon({ conversations: [conversation] });
    renderPage();

    const voiceButton = await screen.findByRole('button', { name: '开始语音输入' });
    expect(voiceButton).toHaveAttribute('aria-pressed', 'false');
    expect(voiceButton).toHaveAttribute('title', '语音识别由浏览器提供，可能使用在线服务。');
    expect(voiceButton).toBeEnabled();
  });

  it('keeps Workspace selection out of the composer contract', async () => {
    mockCommon({ conversations: [conversation], hosts: [targetHost] });
    const accept = vi.spyOn(portalApi, 'acceptConversationMessage').mockImplementation(async (conversationID, body) => acceptedReceipt(conversationID, body));
    const user = userEvent.setup();
    renderPage();
    await screen.findByRole('option', { name: /测试电脑/ });
    await findReadyComposer();
    await user.selectOptions(screen.getByRole('combobox', { name: '选择目标电脑' }), targetHost.id);
    const composer = screen.getByRole('textbox', { name: '消息内容' });
    await user.type(composer, '查看项目文件');
    await user.click(screen.getByRole('button', { name: '发送' }));
    await waitFor(() => expect(accept).toHaveBeenCalledOnce());
    expect(accept.mock.calls[0]![1]).toMatchObject({ content: '查看项目文件', host_id: targetHost.id });
    expect(accept.mock.calls[0]![1]).not.toHaveProperty('workspace_id');
    await waitFor(() => expect(composer).toHaveValue(''));
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
    await user.type(await findReadyComposer(), '普通文字');
    await user.click(screen.getByRole('button', { name: '发送' }));
    await waitFor(() => expect(accept).toHaveBeenCalledOnce());

    availableModelsResponse = { ...defaultAvailableModels, items: [], configuration_status: 'not_configured', default_model_configured: false, default_model_id: null };
    await view.client.invalidateQueries({ queryKey: ['available-models'] });
    await waitFor(() => expect(view.client.getQueryData<AvailableModels>(['available-models'])?.configuration_status).toBe('not_configured'));
    vi.mocked(portalApi.modelConnections).mockResolvedValue({ items: [], configuration_status: 'not_configured' });
    await view.client.invalidateQueries({ queryKey: ['model-connections'] });
    await waitFor(() => expect(view.client.getQueryData<Awaited<ReturnType<typeof portalApi.modelConnections>>>(['model-connections'])?.configuration_status)
      .toBe('not_configured'));
    const composer = await findReadyComposer();
    await user.type(composer, '仍然保留');
    expect(screen.getByRole('button', { name: '发送' })).toBeEnabled();
    await user.click(screen.getByRole('button', { name: '发送' }));
    expect(screen.getByRole('status')).toHaveTextContent(/还没有配置云端模型/);
    expect(composer).toHaveValue('仍然保留');
    expect(accept).toHaveBeenCalledOnce();
  });

  it('accepts a message when configuration exists but no checked text model is available', async () => {
    mockCommon({ conversations: [conversation] });
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [], configuration_status: 'configured' });
    availableModelsResponse = { ...defaultAvailableModels, items: [], configuration_status: 'configured',
      default_model_configured: false, default_model_id: null };
    const accept = vi.spyOn(portalApi, 'acceptConversationMessage')
      .mockImplementation(async (conversationID, body) => acceptedReceipt(conversationID, body));
    const user = userEvent.setup();
    renderPage();
    await screen.findByText(/当前没有通过检查的文字模型；消息仍可先发送/);
    const composer = await findReadyComposer();
    await user.type(composer, '先保存这条消息');
    await user.click(screen.getByRole('button', { name: '发送' }));

    await waitFor(() => expect(accept).toHaveBeenCalledOnce());
    expect(accept.mock.calls[0]![1]).toMatchObject({ content: '先保存这条消息' });
    expect(accept.mock.calls[0]![1]).not.toHaveProperty('model_id');
    await waitFor(() => expect(composer).toHaveValue(''));
  });

  it('keeps a selected model visible while its health check is pending and omits it from submission', async () => {
    const unchecked = { ...model, id: 'p1122334', name: '待核对模型', checked_at: null };
    mockCommon({ conversations: [conversation] });
    vi.mocked(portalApi.modelConnections).mockResolvedValue({ items: [unchecked, model], configuration_status: 'configured' });
    vi.mocked(portalApi.preferences).mockResolvedValue({ assistant_name: 'Morde', selected_model_id: unchecked.id,
      default_host_id: null, updated_at: timestamp });
    availableModelsResponse = { ...defaultAvailableModels,
      items: [{ id: model.id, name: model.name, model_name: model.model_name, capabilities: model.capabilities }] };
    const accept = vi.spyOn(portalApi, 'acceptConversationMessage')
      .mockImplementation(async (conversationID, body) => acceptedReceipt(conversationID, body));
    const user = userEvent.setup();
    renderPage();

    const selector = await screen.findByRole('combobox', { name: '选择 AI 模型' });
    await waitFor(() => expect(selector).toHaveValue(unchecked.id));
    expect(selector).toHaveValue(unchecked.id);
    expect(within(selector).getByRole('option', { name: '已选择模型，状态待核对' })).toBeInTheDocument();
    expect(within(selector).getByRole('option', { name: /团队模型/ })).toBeInTheDocument();

    await user.type(await findReadyComposer(), '使用我选的模型');
    await user.click(screen.getByRole('button', { name: '发送' }));
    await waitFor(() => expect(accept).toHaveBeenCalledOnce());
    expect(accept.mock.calls[0]![1]).not.toHaveProperty('model_id');
  });

  it('does not report stale not-configured data after a model refresh fails', async () => {
    mockCommon({ conversations: [conversation] });
    vi.spyOn(portalApi, 'modelConnections')
      .mockResolvedValueOnce({ items: [], configuration_status: 'not_configured' })
      .mockRejectedValueOnce(new Error('模型状态刷新失败'));
    const view = renderPage();

    expect(await screen.findByText(/还没有配置云端模型/)).toBeInTheDocument();
    await act(async () => {
      await view.client.invalidateQueries({ queryKey: ['model-connections'] });
    });

    expect(await screen.findByText(/无法读取模型：模型状态刷新失败/)).toBeInTheDocument();
    expect(screen.queryByText(/还没有配置云端模型/)).not.toBeInTheDocument();
  });

  it('accepts a message while model configuration status is unknown', async () => {
    mockCommon({ conversations: [conversation] });
    vi.spyOn(portalApi, 'modelConnections').mockRejectedValue(new Error('模型检查暂不可用'));
    vi.spyOn(portalApi, 'preferences').mockResolvedValue({ assistant_name: 'Morde', selected_model_id: null, default_host_id: null, updated_at: timestamp });
    availableModelsResponse = { ...defaultAvailableModels, items: [], configuration_status: 'unknown', default_model_configured: false, default_model_id: null };
    const accept = vi.spyOn(portalApi, 'acceptConversationMessage');
    const user = userEvent.setup();
    renderPage();
    await screen.findByText(/无法读取模型：模型检查暂不可用/);
    const composer = await findReadyComposer();
    await user.type(composer, '先保存这条消息');
    await user.click(screen.getByRole('button', { name: '发送' }));
    await waitFor(() => expect(accept).toHaveBeenCalledOnce());
    expect(accept.mock.calls[0]![1]).toMatchObject({ content: '先保存这条消息' });
    expect(accept.mock.calls[0]![1]).not.toHaveProperty('model_id');
    await waitFor(() => expect(composer).toHaveValue(''));
  });

  it('accepts another message while the previous Task is running without attaching it to that Task', async () => {
    const priorMessage = { ...message, client_message_id: 'prior-message-001', routing_status: 'routed' as const };
    const running = { ...task, status: 'running' as const, result: null, finished_at: null };
    mockCommon({ conversations: [conversation], messages: [priorMessage] });
    vi.spyOn(portalApi, 'task').mockResolvedValue(running);
    const accept = vi.spyOn(portalApi, 'acceptConversationMessage')
      .mockImplementation(async (conversationID, body) => acceptedReceipt(conversationID, body));
    const user = userEvent.setup();
    renderPage();
    const activity = await findTaskActivity(running.id);
    expect(within(activity).getByRole('button', { name: '停止任务' })).toBeEnabled();
    await user.type(await findReadyComposer(), '另一项工作');
    await user.click(screen.getByRole('button', { name: '发送' }));
    await waitFor(() => expect(accept).toHaveBeenCalledOnce());
    expect(accept.mock.calls[0]![1]).toMatchObject({ content: '另一项工作', previous_client_message_id: 'prior-message-001' });
    expect(accept.mock.calls[0]![1]).not.toHaveProperty('reference_task_id');
  });

  it('sends an explicit supplement for Task A while Task B remains independent', async () => {
    const secondMessage = { ...message, id: 'm2345678', sequence: 2, task_id: 'q7654321', content: 'Task B' };
    const firstTask = { ...task, status: 'running' as const, result: null, finished_at: null,
      current_revision: 2, desired_revision: 2 };
    const secondTask = { ...firstTask, id: secondMessage.task_id, objective: secondMessage.content,
      current_revision: 1, desired_revision: 1 };
    mockCommon({ conversations: [conversation], messages: [message, secondMessage] });
    vi.mocked(portalApi.conversationTasks).mockResolvedValue({ items: [firstTask, secondTask], next_cursor: null, pending_count: 0 });
    vi.mocked(portalApi.task).mockImplementation(async (id) => id === firstTask.id ? firstTask : secondTask);
    const accept = vi.spyOn(portalApi, 'acceptConversationMessage')
      .mockImplementation(async (conversationID, body) => acceptedReceipt(conversationID, body));
    const user = userEvent.setup();
    renderPage();

    const firstPanel = await findTaskActivity(firstTask.id);
    await user.click(within(firstPanel).getByRole('button', { name: '补充任务' }));
    await user.type(within(firstPanel).getByRole('textbox', { name: '补充任务内容' }), '按最新要求更新 Task A');
    await user.click(within(firstPanel).getByRole('button', { name: '发送补充任务' }));

    await waitFor(() => expect(accept).toHaveBeenCalledOnce());
    expect(accept.mock.calls[0]![1]).toMatchObject({
      content: '按最新要求更新 Task A',
      reference_task_id: firstTask.id,
    });
    expect(accept.mock.calls[0]![1].reference_task_id).not.toBe(secondTask.id);
    expect(await findTaskActivity(secondTask.id)).toHaveTextContent('修订 1 / 1');
  });

  it('shows only the selected conversation tasks and sends a supplement to that conversation', async () => {
    const main = { ...conversation, title: '主会话' };
    const other = { ...conversation, id: 'c7654321', is_main: false, title: '另一旁聊' };
    const sideTask = { ...task, conversation_id: other.id, status: 'running' as const, result: null, finished_at: null };
    mockCommon({
      conversations: [main, other],
      mainConversation: { status: 'ready', conversation_id: main.id, version: 1 },
    });
    vi.mocked(portalApi.conversationTasks).mockImplementation(async (conversationID) => ({
      items: conversationID === other.id ? [sideTask] : [], next_cursor: null, pending_count: 0,
    }));
    vi.mocked(portalApi.task).mockResolvedValue(sideTask);
    const accountTasks = vi.mocked(portalApi.tasks);
    const accept = vi.spyOn(portalApi, 'acceptConversationMessage')
      .mockImplementation(async (conversationID, body) => acceptedReceipt(conversationID, body));
    const user = userEvent.setup();
    renderPage();

    const sidebar = await screen.findByRole('complementary', { name: '对话' });
    await user.click(within(sidebar).getByRole('button', { name: /另一旁聊/ }));
    const activity = await findTaskActivity(sideTask.id);
    expect(screen.queryByRole('button', { name: '打开侧边栏' })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: '关闭侧边栏' })).toBeInTheDocument();
    expect(screen.queryByText('全部任务')).not.toBeInTheDocument();
    expect(screen.queryByText('当前会话')).not.toBeInTheDocument();
    expect(within(activity).queryByRole('button', { name: '打开所属会话' })).not.toBeInTheDocument();
    expect(accountTasks).not.toHaveBeenCalled();
    await user.click(within(activity).getByRole('button', { name: '补充任务' }));
    await user.type(within(activity).getByRole('textbox', { name: '补充任务内容' }), '继续主会话中的任务');
    await user.click(within(activity).getByRole('button', { name: '发送补充任务' }));

    await waitFor(() => expect(accept).toHaveBeenCalledOnce());
    expect(accept.mock.calls[0]?.[0]).toBe(other.id);
    expect(accept.mock.calls[0]?.[1]).toMatchObject({
      content: '继续主会话中的任务', reference_task_id: sideTask.id,
    });
    expect(screen.getByRole('heading', { name: other.title })).toBeInTheDocument();
  });

  it('recovers the current conversation sidebar after recent task loading fails', async () => {
    mockCommon({ conversations: [conversation] });
    let recentReads = 0;
    const readTasks = vi.mocked(portalApi.conversationTasks).mockImplementation(async (_id, options) => {
      if (options?.view === 'recent' && recentReads++ === 0) throw new Error('recent task read failed');
      return { items: [], next_cursor: null, pending_count: 0 };
    });
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByRole('button', { name: '打开侧边栏' }));
    const recentError = await screen.findByText('已结束任务暂不可用。');
    await user.click(within(recentError.parentElement!).getByRole('button', { name: '重试' }));
    await waitFor(() => expect(screen.queryByText('已结束任务暂不可用。')).not.toBeInTheDocument());
    expect(readTasks).toHaveBeenCalledWith(conversation.id, { view: 'recent' });
    expect(readTasks.mock.calls.filter(([, options]) => options?.view === 'recent')).toHaveLength(2);
  });

  it('keeps task and file pages scoped to the selected conversation when an older task request finishes late', async () => {
    const main = { ...conversation, title: '主会话' };
    const side = { ...conversation, id: 'c7654321', is_main: false, title: '旁聊文件会话' };
    const mainMessage = { ...message, conversation_id: main.id, task_id: 'q1234567', attachment_ids: ['a1234567'], content: '主会话附件' };
    const sideMessage = { ...message, conversation_id: side.id, id: 'm7654321', task_id: 'q7654321', attachment_ids: ['a7654321', 'a7654322'], content: '旁聊附件' };
    const mainTask = { ...task, conversation_id: main.id, objective: '主会话任务', status: 'running' as const, result: null, finished_at: null };
    const sideTask = { ...task, id: 'q7654321', conversation_id: side.id, objective: '旁聊任务', status: 'completed' as const,
      artifacts: [{ id: 'f7654321', step_id: 's7654321', label: '旁聊成果', path: 'reports/side.txt', media_type: 'text/plain',
        workspace_id: null, workspace_revision: null, size_bytes: 12, sha256: null, verified: true,
        batch_item_id: null, batch_attempt: 1, created_at: timestamp }] };
    const sideTask2 = { ...sideTask, id: 'q8765432', objective: '旁聊第二任务', artifacts: [] };
    let resolveMainTasks!: (value: Awaited<ReturnType<typeof portalApi.conversationTasks>>) => void;
    const mainTaskResult = new Promise<Awaited<ReturnType<typeof portalApi.conversationTasks>>>((resolve) => { resolveMainTasks = resolve; });
    mockCommon({ conversations: [main, side], mainConversation: { status: 'ready', conversation_id: main.id, version: 1 } });
    vi.mocked(portalApi.conversationMessages).mockImplementation(async (id) => ({
      items: id === main.id ? [mainMessage] : [sideMessage], next_cursor: null,
    }));
    const conversationTaskRead = vi.mocked(portalApi.conversationTasks).mockImplementation(async (id, options) => {
      if (id === main.id && options?.view !== 'recent') return mainTaskResult;
      if (id === side.id && options?.view === 'recent') return { items: [], next_cursor: null, pending_count: 0 };
      if (id === side.id) return { items: [sideTask, sideTask2], next_cursor: null, pending_count: 0 };
      return { items: [], next_cursor: null, pending_count: 0 };
    });
    vi.mocked(portalApi.task).mockImplementation(async (id) => id === sideTask.id ? sideTask : id === sideTask2.id ? sideTask2 : mainTask);
    const user = userEvent.setup();
    renderPage();

    const sidebar = await screen.findByRole('complementary', { name: '对话' });
    await user.click(within(sidebar).getByRole('button', { name: /旁聊文件会话/ }));
    await findTaskActivity(sideTask.id);
    expect(conversationTaskRead).toHaveBeenCalledWith(side.id, { view: 'recent' });
    expect(screen.queryByText('主会话任务')).not.toBeInTheDocument();
    resolveMainTasks({ items: [mainTask], next_cursor: null, pending_count: 0 });
    await waitFor(() => expect(screen.queryByText('主会话任务')).not.toBeInTheDocument());

    const taskPanel = document.querySelector<HTMLElement>('#assistant-task-panel')!;
    await user.click(within(taskPanel).getByRole('button', { name: /返回任务列表/ }));
    await user.click(within(taskPanel).getByRole('button', { name: '上一页' }));
    expect(within(taskPanel).getByText('旁聊第二任务')).toBeInTheDocument();
    await user.click(within(taskPanel).getByRole('button', { name: '下一页' }));
    await waitFor(() => expect(within(taskPanel).getByText('旁聊任务')).toBeInTheDocument());
    await user.click(within(taskPanel).getByRole('button', { name: '文件' }));
    const filesPanel = document.querySelector<HTMLElement>('#assistant-task-panel .task-panel-files')!;
    expect(within(filesPanel).getByRole('link', { name: '下载消息附件 1' })).toHaveAttribute(
      'href', '/api/personal-data/attachments/a7654321');
    expect(within(filesPanel).queryByRole('link', { name: /a1234567/ })).not.toBeInTheDocument();
    expect(within(filesPanel).queryByText('主会话任务')).not.toBeInTheDocument();
    expect(within(filesPanel).getByRole('button', { name: '下一页' })).toBeEnabled();
    await user.click(within(filesPanel).getByRole('button', { name: '下一页' }));
    expect(within(filesPanel).getByRole('link', { name: '下载消息附件 2' })).toHaveAttribute(
      'href', '/api/personal-data/attachments/a7654322');
    await user.click(within(filesPanel).getByRole('button', { name: '第 3 页' }));
    expect(within(filesPanel).getByText('旁聊成果')).toBeInTheDocument();
    expect(within(filesPanel).getByRole('button', { name: '查看任务成果' })).toBeInTheDocument();

    await user.click(within(taskPanel).getByRole('button', { name: '任务' }));
    await findTaskActivity(sideTask.id);
    await user.click(within(sidebar).getByRole('button', { name: /主会话/ }));
    await waitFor(() => expect(within(taskPanel).getByText('主会话任务')).toBeInTheDocument());
    expect(within(taskPanel).queryByText('旁聊任务')).not.toBeInTheDocument();
    await user.click(within(taskPanel).getByRole('button', { name: '文件' }));
    const mainFilesPanel = document.querySelector<HTMLElement>('#assistant-task-panel .task-panel-files')!;
    expect(within(mainFilesPanel).getByRole('link', { name: '下载消息附件 1' })).toHaveAttribute(
      'href', '/api/personal-data/attachments/a1234567');
    expect(within(mainFilesPanel).queryByRole('link', { name: /a7654321/ })).not.toBeInTheDocument();
    expect(within(mainFilesPanel).queryByText('旁聊成果')).not.toBeInTheDocument();
  });

  it('uses the newest server predecessor when history is returned newest first', async () => {
    const newest = { ...message, id: 'm7654321', sequence: 8, content: '近期上下文', client_message_id: 'recent-client-message' };
    const oldest = { ...message, id: 'm1234568', sequence: 2, content: '旧上下文', client_message_id: 'old-client-message' };
    mockCommon({ conversations: [conversation], messages: [newest, oldest] });
    const accept = vi.spyOn(portalApi, 'acceptConversationMessage')
      .mockImplementation(async (conversationID, body) => acceptedReceipt(conversationID, body));
    const user = userEvent.setup();
    renderPage();
    await user.type(await findReadyComposer(), '继续这次对话');
    await user.click(screen.getByRole('button', { name: '发送' }));

    await waitFor(() => expect(accept).toHaveBeenCalledOnce());
    expect(accept.mock.calls[0]![1]).toMatchObject({
      content: '继续这次对话', previous_client_message_id: 'recent-client-message',
    });
  });

  it('accepts long UTF-8 text within the character limit', async () => {
    mockCommon({ conversations: [conversation] });
    const submit = vi.spyOn(portalApi, 'acceptConversationMessage').mockImplementation(async (conversationID, body) => acceptedReceipt(conversationID, body));
    const user = userEvent.setup();
    renderPage();
    await screen.findByRole('option', { name: /团队模型/ });
    const content = '中文a'.repeat(4000);
    fireEvent.change(await findReadyComposer(), { target: { value: content } });
    await user.click(screen.getByRole('button', { name: '发送' }));
    await waitFor(() => expect(submit).toHaveBeenCalledOnce());
    expect(submit.mock.calls[0]![1].content).toBe(content);
    expect(submit.mock.calls[0]![1]).not.toHaveProperty('previous_client_message_id');
  });

  it('retries a lost message acceptance with the exact idempotency key and request', async () => {
    mockCommon({ conversations: [conversation] });
    const submit = vi.spyOn(portalApi, 'acceptConversationMessage')
      .mockRejectedValueOnce(new ApiError({ message: '连接中断', code: 'network_error', status: 503 }))
      .mockImplementation(async (conversationID, body) => acceptedReceipt(conversationID, body));
    const user = userEvent.setup();
    renderPage();
    const composer = await findReadyComposer();
    await user.type(composer, '保存后断线');
    await user.click(screen.getByRole('button', { name: '发送' }));
    const retry = await screen.findByRole('button', { name: '核对并重试' });
    expect(composer).toBeEnabled();
    const firstCall = submit.mock.calls[0]!;
    await user.click(retry);
    await waitFor(() => expect(screen.queryByRole('button', { name: '核对并重试' })).not.toBeInTheDocument());
    expect(submit).toHaveBeenCalledTimes(2);
    expect(submit.mock.calls[1]).toEqual(firstCall);
  });

  it('waits to route a later message until the earlier message receipt is confirmed', async () => {
    mockCommon({ conversations: [conversation] });
    const submit = vi.spyOn(portalApi, 'acceptConversationMessage')
      .mockRejectedValueOnce(new ApiError({ message: '连接中断', code: 'network_error', status: 503 }))
      .mockImplementation(async (conversationID, body) => acceptedReceipt(conversationID, body));
    const user = userEvent.setup();
    renderPage();
    await user.type(await findReadyComposer(), '第一条待确认');
    await user.click(screen.getByRole('button', { name: '发送' }));
    await screen.findByRole('button', { name: '核对并重试' });
    const firstCall = submit.mock.calls[0]!;
    await user.type(await findReadyComposer(), '后续消息');
    await user.click(screen.getByRole('button', { name: '发送' }));
    await waitFor(() => expect(submit).toHaveBeenCalledOnce());
    expect(submit.mock.calls[0]).toEqual(firstCall);
    expect(await screen.findByText(/前一条消息尚未确认/)).toBeInTheDocument();
    expect(screen.getByText('后续消息')).toBeInTheDocument();
  });

  it('keeps a message locally when the server detects a different authenticated account', async () => {
    mockCommon({ conversations: [conversation] });
    const accept = vi.spyOn(portalApi, 'acceptConversationMessage').mockRejectedValue(
      new ApiError({ message: '账号身份不匹配', code: 'conflict', status: 409 }),
    );
    const user = userEvent.setup();
    renderPage();
    await user.type(await findReadyComposer(), '账号切换前的内容');
    await user.click(screen.getByRole('button', { name: '发送' }));
    expect(await screen.findByText(/账号、服务或消息状态已变化，消息保留在本机/)).toBeInTheDocument();
    expect(accept.mock.calls[0]?.[4]).toBe(authUser.id);
    expect(screen.getByRole('button', { name: '核对并重试' })).toBeEnabled();
    expect(screen.getByText('账号切换前的内容')).toBeInTheDocument();
  });

  it('accepts attachments before upload and keeps the same upload keys on retry', async () => {
    vi.stubGlobal('crypto', webcrypto);
    mockCommon({ conversations: [conversation] });
    const submit = vi.spyOn(portalApi, 'acceptConversationMessage').mockImplementation(async (conversationID, body) => acceptedReceipt(conversationID, body));
    const replace = vi.spyOn(portalApi, 'replacePersonalObject').mockResolvedValue({
      object_type: 'library_item', object_id: 'chat-upload', result_revision: 1, account_change_sequence: 1, replay: false,
    });
    const upload = vi.spyOn(portalApi, 'createPersonalAttachmentUpload').mockRejectedValueOnce(new Error('上传中断'))
      .mockResolvedValueOnce({ upload_id: 'pu_0123456789abcdef0123456789abcdef', state: 'active', chunk_count: 1,
        received_sequences: [], replay: false });
    const chunk = vi.spyOn(portalApi, 'putPersonalAttachmentChunk').mockResolvedValue({
      upload_id: 'pu_0123456789abcdef0123456789abcdef', sequence: 0, replay: false,
    });
    const commit = vi.spyOn(portalApi, 'commitPersonalAttachment').mockResolvedValue({
      attachment_id: 'pa_0123456789abcdef0123456789abcdef', object_type: 'library_item', object_id: 'chat-upload',
      result_revision: 2, account_change_sequence: 2, replay: false,
    });
    const associate = vi.spyOn(portalApi, 'associateConversationAttachment').mockResolvedValue(undefined);
    const user = userEvent.setup();
    renderPage();
    await user.upload(await screen.findByLabelText('添加附件'), new File(['hello'], 'brief.txt', { type: 'text/plain' }));
    await user.type(await findReadyComposer(), '请阅读附件');
    await user.click(screen.getByRole('button', { name: '发送' }));
    const retry = await screen.findByRole('button', { name: '重试附件' });
    expect(await screen.findByText(/消息已发送，附件等待处理：上传中断/)).toBeInTheDocument();
    expect(submit).toHaveBeenCalledOnce();
    expect(screen.getByRole('textbox', { name: '消息内容' })).toBeEnabled();
    await user.click(retry);
    await waitFor(() => expect(associate).toHaveBeenCalledOnce());
    const body = submit.mock.calls[0]![1];
    expect(body.client_attachment_ids).toHaveLength(1);
    expect(body.content).toBe('请阅读附件');
    expect(replace).toHaveBeenCalledTimes(2);
    expect(replace.mock.calls[1]?.[4]).toBe(replace.mock.calls[0]?.[4]);
    expect(upload).toHaveBeenCalledTimes(2);
    expect(upload.mock.calls[1]?.[1]).toBe(upload.mock.calls[0]?.[1]);
    const serviceID = clientContract.capabilities.cloud_execution.service_instance_id;
    expect(replace.mock.calls[1]?.slice(5)).toEqual([authUser.id, serviceID]);
    expect(upload.mock.calls[1]?.slice(2)).toEqual([serviceID, authUser.id]);
    expect(chunk.mock.calls[0]?.slice(4)).toEqual([serviceID, authUser.id]);
    expect(commit.mock.calls[0]?.slice(2)).toEqual([serviceID, authUser.id]);
    expect(associate).toHaveBeenCalledWith(conversation.id, expect.any(String), expect.any(String),
      'pa_0123456789abcdef0123456789abcdef', serviceID, authUser.id);
  });

  it('extracts PDF text locally and uploads only the bounded text derivative to the Task', async () => {
    vi.stubGlobal('crypto', webcrypto);
    mockCommon({ conversations: [conversation], messages: [message] });
    pdfMock.getDocument.mockReturnValue({
      promise: Promise.resolve({ numPages: 1, getPage: async () => ({
        getTextContent: async () => ({ items: [{ str: '仅派生文本进入任务附件' }] }),
        cleanup: vi.fn(),
      }) }),
      destroy: vi.fn().mockResolvedValue(undefined),
    });
    const submit = vi.spyOn(portalApi, 'acceptConversationMessage').mockImplementation(async (conversationID, body) => acceptedReceipt(conversationID, body));
    const replace = vi.spyOn(portalApi, 'replacePersonalObject').mockResolvedValue({
      object_type: 'library_item', object_id: 'chat-pdf-text', result_revision: 1, account_change_sequence: 1, replay: false,
    });
    const upload = vi.spyOn(portalApi, 'createPersonalAttachmentUpload').mockResolvedValue({
      upload_id: 'pu_0123456789abcdef0123456789abcdef', state: 'active', chunk_count: 1, received_sequences: [], replay: false,
    });
    const chunk = vi.spyOn(portalApi, 'putPersonalAttachmentChunk').mockResolvedValue({
      upload_id: 'pu_0123456789abcdef0123456789abcdef', sequence: 0, replay: false,
    });
    vi.spyOn(portalApi, 'commitPersonalAttachment').mockResolvedValue({
      attachment_id: 'pa_0123456789abcdef0123456789abcdef', object_type: 'library_item', object_id: 'chat-pdf-text',
      result_revision: 2, account_change_sequence: 2, replay: false,
    });
    const associate = vi.spyOn(portalApi, 'associateConversationAttachment').mockResolvedValue(undefined);
    const user = userEvent.setup();
    renderPage();

    await user.upload(await screen.findByLabelText('添加附件'), new File(['synthetic PDF original'], 'brief.pdf', { type: 'application/pdf' }));
    expect(await screen.findByText('brief.pdf · 已提取文字')).toBeInTheDocument();
    await user.type(await findReadyComposer(), '总结这份 PDF');
    await user.click(screen.getByRole('button', { name: '发送' }));

    await waitFor(() => expect(associate).toHaveBeenCalledOnce());
    const body = submit.mock.calls[0]![1];
    expect(body.content).toBe('总结这份 PDF');
    expect(body.client_attachment_ids).toHaveLength(1);
    expect(replace.mock.calls[0]?.[3]).toEqual(expect.objectContaining({
      name: 'brief.pdf.txt', mime: 'text/plain',
    }));
    expect(upload.mock.calls[0]?.[0]).toEqual(expect.objectContaining({ media_type: 'text/plain' }));
    expect(new TextDecoder().decode(chunk.mock.calls[0]?.[2] as ArrayBuffer)).toContain('仅派生文本进入任务附件');
    expect(new TextDecoder().decode(chunk.mock.calls[0]?.[2] as ArrayBuffer)).not.toContain('synthetic PDF original');
  });

  it('cancels local PDF OCR explicitly while keeping its preparation attached to the original conversation', async () => {
    vi.stubGlobal('crypto', webcrypto);
    const otherConversation = { ...conversation, id: 'c7654321', title: '另一个会话' };
    mockCommon({ conversations: [conversation, otherConversation] });
    const page = {
      getTextContent: vi.fn(async () => ({ items: [] })),
      getViewport: vi.fn(({ scale }: { scale: number }) => ({ width: 300 * scale, height: 150 * scale })),
      render: vi.fn(() => ({ promise: Promise.resolve(), cancel: vi.fn() })),
      cleanup: vi.fn(),
    };
    const destroy = vi.fn().mockResolvedValue(undefined);
    pdfMock.getDocument.mockReturnValue({
      promise: Promise.resolve({ numPages: 1, getPage: async () => page }),
      destroy,
    });
    vi.spyOn(HTMLCanvasElement.prototype, 'getContext').mockImplementation(() => ({}) as CanvasRenderingContext2D);
    const worker = {
      recognize: vi.fn(() => new Promise(() => undefined)),
      terminate: vi.fn().mockResolvedValue(undefined),
    };
    tesseractMock.createWorker.mockResolvedValue(worker);
    const user = userEvent.setup();
    renderPage();

    await user.upload(await screen.findByLabelText('添加附件'), new File(['synthetic PDF original'], 'scan.pdf', { type: 'application/pdf' }));
    await waitFor(() => expect(worker.recognize).toHaveBeenCalledOnce());
    await user.click(screen.getByRole('button', { name: '取消读取' }));
    await waitFor(() => expect(worker.terminate).toHaveBeenCalledOnce());
    expect(screen.getByText('scan.pdf · 已暂停读取')).toBeInTheDocument();

    await user.upload(screen.getByLabelText('添加附件'), new File(['synthetic PDF original'], 'scan-again.pdf', { type: 'application/pdf' }));
    await waitFor(() => expect(worker.recognize).toHaveBeenCalledTimes(2));
    expect(screen.getByText('scan.pdf · 已暂停读取')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: /另一个会话/ }));
    await waitFor(() => expect(screen.getByRole('heading', { name: '另一个会话' })).toBeInTheDocument());
    expect(worker.terminate).toHaveBeenCalledOnce();
    await user.click(screen.getByRole('button', { name: /主会话/ }));
    expect(await screen.findByText('scan-again.pdf · 待读取')).toBeInTheDocument();
    expect(screen.getByText('scan.pdf · 已暂停读取')).toBeInTheDocument();
    expect(worker.terminate).toHaveBeenCalledOnce();
    expect(worker.recognize).toHaveBeenCalledTimes(2);
    await user.click(screen.getByRole('button', { name: '取消读取' }));
    await waitFor(() => expect(worker.terminate).toHaveBeenCalledTimes(2));

    expect(destroy).toHaveBeenCalledTimes(2);
    expect(screen.queryByText('scan.pdf · 已提取文字')).not.toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('previews Office text only after an explicit click and renders the result as plain text', async () => {
    mockCommon({ conversations: [conversation] });
    const previewRequests: { mediaType: string | null; bodySize: number }[] = [];
    server.use(http.post('*/api/personal-data/office-preview', async ({ request }) => {
      previewRequests.push({ mediaType: request.headers.get('content-type'), bodySize: (await request.arrayBuffer()).byteLength });
      return HttpResponse.json({ text: '<script>text only</script>\nA1=42' });
    }));
    const user = userEvent.setup();
    renderPage();

    const file = new File(['synthetic DOCX bytes'], 'synthetic.docx', {
      type: 'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
    });
    await user.upload(await screen.findByLabelText('添加附件'), file);
    expect(screen.getByText('synthetic.docx')).toBeInTheDocument();
    expect(previewRequests).toHaveLength(0);

    await user.click(screen.getByRole('button', { name: '预览附件 synthetic.docx' }));
    const result = await screen.findByRole('region', { name: '文本预览 synthetic.docx' });
    expect(result.textContent).toContain('<script>text only</script>\nA1=42');
    expect(result.querySelector('script')).toBeNull();
    expect(previewRequests).toEqual([{
      mediaType: 'application/vnd.openxmlformats-officedocument.wordprocessingml.document', bodySize: file.size,
    }]);
    expect(screen.getByText('synthetic.docx')).toBeInTheDocument();
  });

  it('keeps the attachment and message draft when Office text preview fails', async () => {
    mockCommon({ conversations: [conversation] });
    vi.spyOn(portalApi, 'previewPersonalOfficeText').mockRejectedValue(new Error('synthetic parse failure'));
    const user = userEvent.setup();
    renderPage();

    await user.upload(await screen.findByLabelText('添加附件'), new File(['synthetic malformed DOCX'], 'draft.docx', {
      type: 'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
    }));
    const message = await findReadyComposer();
    await user.type(message, '保留此草稿');
    await user.click(screen.getByRole('button', { name: '预览附件 draft.docx' }));

    const error = await screen.findByRole('alert');
    expect(error).toHaveTextContent('draft.docx 预览失败：synthetic parse failure');
    expect(error.closest('.assistant-composer')).toBeNull();
    expect(screen.getByText('draft.docx')).toBeInTheDocument();
    expect(message).toHaveValue('保留此草稿');
    expect(screen.getByRole('button', { name: '发送' })).toBeEnabled();
  });

  it('accepts PPTX and uploads it with the presentation MIME type for Task Reader extraction', async () => {
    vi.stubGlobal('crypto', webcrypto);
    mockCommon({ conversations: [conversation], messages: [message] });
    const submit = vi.spyOn(portalApi, 'acceptConversationMessage').mockImplementation(async (conversationID, body) => acceptedReceipt(conversationID, body));
    const replace = vi.spyOn(portalApi, 'replacePersonalObject').mockResolvedValue({
      object_type: 'library_item', object_id: 'chat-pptx', result_revision: 1, account_change_sequence: 1, replay: false,
    });
    const upload = vi.spyOn(portalApi, 'createPersonalAttachmentUpload').mockResolvedValue({
      upload_id: 'pu_0123456789abcdef0123456789abcdef', state: 'active', chunk_count: 1, received_sequences: [], replay: false,
    });
    const chunk = vi.spyOn(portalApi, 'putPersonalAttachmentChunk').mockResolvedValue({
      upload_id: 'pu_0123456789abcdef0123456789abcdef', sequence: 0, replay: false,
    });
    vi.spyOn(portalApi, 'commitPersonalAttachment').mockResolvedValue({
      attachment_id: 'pa_0123456789abcdef0123456789abcdef', object_type: 'library_item', object_id: 'chat-pptx',
      result_revision: 2, account_change_sequence: 2, replay: false,
    });
    const associate = vi.spyOn(portalApi, 'associateConversationAttachment').mockResolvedValue(undefined);
    const user = userEvent.setup();
    renderPage();

    const picker = await screen.findByLabelText('添加附件');
    expect(picker).toHaveAttribute('accept', expect.stringContaining('.pptx'));
    await user.upload(picker, new File(['synthetic PPTX bytes'], 'brief.pptx', {
      type: 'application/vnd.openxmlformats-officedocument.presentationml.presentation',
    }));
    expect(await screen.findByText('brief.pptx')).toBeInTheDocument();
    await user.type(await findReadyComposer(), '总结演示文稿');
    await user.click(screen.getByRole('button', { name: '发送' }));

    await waitFor(() => expect(associate).toHaveBeenCalledOnce());
    expect(submit.mock.calls[0]?.[1].client_attachment_ids).toHaveLength(1);
    expect(replace.mock.calls[0]?.[3]).toEqual(expect.objectContaining({
      name: 'brief.pptx', mime: 'application/vnd.openxmlformats-officedocument.presentationml.presentation',
    }));
    expect(upload.mock.calls[0]?.[0].media_type).toBe('application/vnd.openxmlformats-officedocument.presentationml.presentation');
    expect(new TextDecoder().decode(chunk.mock.calls[0]?.[2] as ArrayBuffer)).toBe('synthetic PPTX bytes');
  });

  it('recovers a streaming assistant message from the Task snapshot after conversation history expires', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    vi.spyOn(portalApi, 'task').mockResolvedValue(asLegacyTask({
      ...task, status: 'running', result: null, finished_at: null,
      assistant_message: {
        id: 'm8765432', conversation_id: conversation.id, sequence: 2, task_id: task.id, role: 'assistant',
        content: 'A persisted partial answer', status: 'streaming', created_at: timestamp,
      },
    }));
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByRole('button', { name: /^主会话/ }));
    expect(await screen.findByText('A persisted partial answer')).toBeInTheDocument();
    expect(screen.getByText('正在生成…')).toHaveAttribute('role', 'status');
  });

  it('keeps a legacy raw task result in task dynamics when no assistant message was saved', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    vi.spyOn(portalApi, 'task').mockResolvedValue(asLegacyTask({ ...task, result: '已完成整理。' }));
    renderPage();

    const activity = await findTaskActivity(task.id);
    expect(within(activity).getByText('已完成整理。')).toBeInTheDocument();
    expect(document.querySelector('.chat-message-assistant')).toBeNull();
    expect(screen.queryByText('已接收')).not.toBeInTheDocument();
  });

  it('does not describe an undelivered message in a stopped task as still waiting', async () => {
    mockCommon({ conversations: [conversation], messages: [{ ...message, delivery_status: 'pending' }] });
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, status: 'cancelled', result: null });
    renderPage();

    expect(await screen.findByText('消息未处理，处理已结束')).toBeInTheDocument();
    expect(screen.queryByText('已保存，等待接收')).not.toBeInTheDocument();
  });

  it.each([
    ['pending', '此消息 · 已受理，等待处理'], ['processing', '此消息 · 正在处理'],
    ['waiting_user', '此消息 · 等待你补充任务信息'], ['failed', null],
  ] as const)('shows the actual routing state %s without requiring a Task', async (routingStatus, label) => {
    mockCommon({ conversations: [conversation], messages: [{ ...message, task_id: undefined, routing_status: routingStatus, delivery_status: 'pending' }] });
    renderPage();
    if (label) expect(await screen.findByText(label)).toBeInTheDocument();
    else {
      expect(screen.queryByText('消息处理失败')).not.toBeInTheDocument();
      expect(await screen.findByRole('button', { name: '重新处理这条消息' })).toBeInTheDocument();
    }
  });

  it('retains the processing retry key when the response is lost and reuses it on retry', async () => {
    mockCommon({ conversations: [conversation], messages: [{ ...message, task_id: undefined,
      routing_status: 'failed', routing_failure_reason: 'model_unavailable', delivery_status: 'pending' }] });
    const retry = vi.spyOn(portalApi, 'retryConversationMessageProcessing')
      .mockRejectedValueOnce(new Error('连接中断')).mockResolvedValueOnce(undefined);
    const user = userEvent.setup();
    renderPage();
    expect(screen.queryByText('模型暂时不可用或用量待核对')).not.toBeInTheDocument();
    await user.click(await screen.findByRole('button', { name: '重新处理这条消息' }));
    await screen.findByText('连接中断');
    await user.click(screen.getByRole('button', { name: '重新处理这条消息' }));
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

  it('keeps an oversized failed message visible without inline failure details', async () => {
    mockCommon({ conversations: [conversation], messages: [{ ...message, task_id: undefined,
      routing_status: 'failed', routing_failure_reason: 'message_too_long', delivery_status: 'pending' }] });
    renderPage();
    const retry = await screen.findByRole('button', { name: '重新处理这条消息' });
    const article = retry.closest<HTMLElement>('article')!;
    expect(within(article).getByText(message.content)).toBeInTheDocument();
    expect(within(article).queryByText('消息处理失败')).not.toBeInTheDocument();
    expect(within(article).queryByText(/超过当前模型输入上限/)).not.toBeInTheDocument();
  });

  it('deduplicates a snapshot against the same committed conversation message', () => {
    const merged = mergeTaskAssistantMessage([reply], asLegacyTask({
      ...task, status: 'running', result: null, finished_at: null,
      assistant_message: {
        ...reply, status: 'streaming',
      },
    }));
    expect(merged).toHaveLength(1);
    expect(merged[0]?.content).toBe(reply.content);
    expect(merged[0]?.snapshotStatus).toBe('completed');
  });

  it('places an unsequenced streaming snapshot after accepted conversation messages', () => {
    const snapshot = { ...reply, id: 'msnapshot1', sequence: undefined, status: 'streaming' as const };
    const merged = mergeTaskAssistantMessage([message], asLegacyTask({ ...task, assistant_message: snapshot }));

    expect(merged).toHaveLength(2);
    expect(merged[1]).toMatchObject({ id: snapshot.id, sequence: 2, snapshotStatus: 'completed' });
  });

  it('deduplicates combined replies only for the matching task, revision and result event', () => {
    const combined = { ...reply, id: 'mcombined', sources: [
      { user_message_id: message.id, task_id: task.id, task_revision: 2, event_sequence: 5, content: reply.content },
    ] };
    const snapshot = { ...reply, id: 'msnapshot', status: 'completed' as const, task_revision: 2, result_event_sequence: 5 };
    expect(mergeTaskAssistantMessage([combined], asLegacyTask({ ...task, assistant_message: snapshot }))).toHaveLength(1);
    expect(mergeTaskAssistantMessage([combined], asLegacyTask({ ...task, assistant_message: { ...snapshot, task_revision: 3 } }))).toHaveLength(2);
    expect(mergeTaskAssistantMessage([combined], asLegacyTask({ ...task, assistant_message: { ...snapshot, result_event_sequence: 6 } }))).toHaveLength(2);
    expect(mergeTaskAssistantMessage([combined], asLegacyTask({ ...task, assistant_message: { ...snapshot, result_event_sequence: undefined } }))).toHaveLength(2);
  });

  it('renders combined assistant replies as natural language without exposing source metadata in chat', async () => {
    const secondUserMessage: ConversationMessage = {
      ...message, id: 'm2234567', sequence: 2, content: '再核对一下总价', task_id: 'q7654321',
    };
    const combined: ConversationMessage = {
      ...reply, id: 'm8877665', sequence: 3, task_id: undefined,
      content: '第一项的结果是 12。\n\n第二项的结果是 34。',
      sources: [
        { user_message_id: message.id, task_id: task.id, task_revision: 2, event_sequence: 5, content: '第一项的结果是 12。' },
        { user_message_id: secondUserMessage.id, task_id: 'q7654321', task_revision: 1, event_sequence: 8, content: '第二项的结果是 34。' },
      ],
    };
    mockCommon({ conversations: [conversation], messages: [message, secondUserMessage, combined] });
    renderPage();

    const assistantMessage = await screen.findByText('第一项的结果是 12。');
    expect(assistantMessage).toBeInTheDocument();
    expect(screen.getByText('第二项的结果是 34。')).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: /查看关联的用户消息/ })).not.toBeInTheDocument();
    expect(document.querySelector('.chat-message-source-meta')).not.toBeInTheDocument();
    expect(document.querySelector('.chat-message-source-task')).not.toBeInTheDocument();
  });

  it('merges persisted streaming messages without losing sequence or reverting interrupted content', () => {
    const saved = { ...reply, id: 'mstream01', content: '前缀', sequence: 4,
      output_status: 'streaming' as const, output_task_revision: 2 };
    const snapshot = { ...reply, id: saved.id, content: '前缀和后续', status: 'streaming' as const, task_revision: 2 };
    const merged = mergeTaskAssistantMessage([saved], asLegacyTask({ ...task, assistant_message: snapshot }));
    expect(merged).toHaveLength(1);
    expect(merged[0]).toMatchObject({ sequence: 4, content: '前缀和后续', snapshotStatus: 'completed' });

    const interrupted = { ...saved, output_status: 'interrupted' as const };
    expect(mergeTaskAssistantMessage([interrupted], asLegacyTask({ ...task, assistant_message: snapshot }))[0])
      .toMatchObject({ sequence: 4, content: '前缀', snapshotStatus: 'interrupted' });
    expect(mergeTaskAssistantMessage([interrupted], undefined)[0]?.snapshotStatus).toBe('interrupted');
    expect(mergeTaskAssistantMessage([saved], asLegacyTask({ ...task, assistant_message: { ...snapshot, task_revision: 3 } }))[0]?.content).toBe('前缀');
  });

  it('does not render a final Task result while its conversation reply is pending or already published', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, result_message_pending: true });
    renderPage();
    const activity = await findTaskActivity(task.id);
    expect(activity).toHaveTextContent('任务已完成，回复整理中。');
    await userEvent.setup().click(within(activity).getByText('更多信息'));
    const rawResult = within(activity).getByRole('region', { name: '原始结果' });
    await userEvent.setup().click(within(rawResult).getByText('查看原始结果（不是最终回复）'));
    expect(within(rawResult).getByText(reply.content)).toBeInTheDocument();
    expect(Array.from(document.querySelectorAll('.chat-message-assistant')).map((element) => element.textContent))
      .not.toEqual(expect.arrayContaining([expect.stringContaining(reply.content)]));
    const snapshot = { ...reply, status: 'completed' as const, task_revision: 0, result_event_sequence: 5 };
    expect(mergeTaskAssistantMessage([], { ...task, current_revision: 0, assistant_message: snapshot, result_message_pending: true })).toHaveLength(0);
    expect(mergeTaskAssistantMessage([], { ...task, current_revision: 0, assistant_message: snapshot, result_message_published: true })).toHaveLength(0);
    expect(mergeTaskAssistantMessage([], asLegacyTask({ ...task, current_revision: 0,
      assistant_message: { ...snapshot, status: 'streaming' }, result_message_pending: true }))).toHaveLength(1);
    const oldInterrupted = { ...snapshot, id: 'moldreply', status: 'interrupted' as const, task_revision: 0, result_event_sequence: undefined };
    const newReply = { ...reply, id: 'mnewreply', sources: [{ user_message_id: message.id, task_id: task.id,
      task_revision: 1, event_sequence: 7, content: reply.content }] };
    expect(mergeTaskAssistantMessage([], asLegacyTask({ ...task, current_revision: 1, assistant_message: oldInterrupted, result_message_pending: true }))).toHaveLength(1);
    expect(mergeTaskAssistantMessage([newReply], asLegacyTask({ ...task, current_revision: 1, assistant_message: oldInterrupted, result_message_published: true }))).toHaveLength(2);
  });

  it('shows K2 task dependencies, child outcomes, and result review without promoting raw results', async () => {
    const secondMessage = { ...message, id: 'm2345678', sequence: 3, task_id: 'q7654321', content: '并发任务 B' };
    const thirdMessage = { ...message, id: 'm3456789', sequence: 5, task_id: 'q8901234', content: '依赖任务 C' };
    const firstTask = {
      ...task, status: 'completed' as const, current_revision: 2, desired_revision: 2, result: 'Task A 原始结果',
      child_jobs: [
        { job_id: 'child-a-done', task_step_id: 's1234567', title: '整理来源 A', task_revision: 2, status: 'succeeded' },
        { job_id: 'child-a-failed', task_step_id: 's2345678', title: '校验来源 B', task_revision: 2, status: 'failed' },
      ],
      dependencies: [],
      result_review: { state: 'failed', source_task_id: task.id, source_task_revision: 2, source_event_sequence: 7,
        reply_message_id: null, trigger_message_id: 'mreview01' },
    } as unknown as Task;
    const secondTask = {
      ...task, id: secondMessage.task_id, status: 'completed' as const, current_revision: 2, desired_revision: 3,
      result: 'Task B 原始结果', child_jobs: [
        { job_id: 'child-b-done', task_step_id: 's3456789', title: '搜索资料', task_revision: 2, status: 'succeeded' },
      ],
      dependencies: [],
      result_review: { state: 'processing', source_task_id: secondMessage.task_id, source_task_revision: 2, source_event_sequence: 9, reply_message_id: null },
    } as unknown as Task;
    const thirdTask = {
      ...task, id: thirdMessage.task_id, status: 'queued' as const, current_revision: 0, desired_revision: 1, result: null,
      dependencies: [{ task_id: secondTask.id, required_revision: 3, task_status: 'completed', satisfied: false }],
      child_jobs: [], result_review: null,
    } as unknown as Task;
    const taskSnapshots = new Map([[firstTask.id, firstTask], [secondTask.id, secondTask], [thirdTask.id, thirdTask]]);
    mockCommon({ conversations: [conversation], messages: [message, secondMessage, thirdMessage] });
    const retryResultReview = vi.spyOn(portalApi, 'retryConversationMessageProcessing').mockResolvedValue(undefined);
    vi.mocked(portalApi.task).mockImplementation(async (id) => taskSnapshots.get(id) ?? task);
    const user = userEvent.setup();
    renderPage();

    const firstPanel = await findTaskActivity(firstTask.id);
    expect(firstPanel).toHaveTextContent('已完成');
    expect(within(firstPanel).getByRole('region', { name: '子任务进度' })).toHaveTextContent('整理来源 A已完成');
    expect(within(firstPanel).getByRole('region', { name: '子任务进度' })).toHaveTextContent('校验来源 B失败');
    expect(within(firstPanel).getByRole('region', { name: '结果整理' }))
      .toHaveTextContent('结果整理失败；原始任务结果仍保留，尚未发布最终回复。');
    await user.click(within(firstPanel).getByRole('button', { name: '重试结果整理' }));
    await screen.findByText('已请求重新整理任务结果。');
    expect(retryResultReview).toHaveBeenCalledWith(conversation.id, 'mreview01', expect.any(String), expect.any(String), authUser.id);
    const secondPanel = await findTaskActivity(secondTask.id);
    expect(within(secondPanel).getByRole('region', { name: '结果整理' }))
      .toHaveTextContent('主助手正在整理原始结果。');
    expect(within(secondPanel).queryByRole('button', { name: '重试结果整理' })).not.toBeInTheDocument();
    const thirdPanel = await findTaskActivity(thirdTask.id);
    await user.click(within(thirdPanel).getByText('更多信息'));
    expect(within(thirdPanel).getByRole('region', { name: '依赖任务' }))
      .toHaveTextContent('等待任务 q7654321 修订 3（当前状态：已完成）');

    const firstPanelAgain = await findTaskActivity(firstTask.id);
    await user.click(within(firstPanelAgain).getByText('更多信息'));
    await user.click(within(firstPanelAgain).getByText('查看原始结果（不是最终回复）'));
    expect(within(firstPanelAgain).getByText('Task A 原始结果')).toBeInTheDocument();
    expect(Array.from(document.querySelectorAll('.chat-message-assistant')).map((element) => element.textContent))
      .not.toEqual(expect.arrayContaining([expect.stringContaining('Task A 原始结果'), expect.stringContaining('Task B 原始结果')]));

    const composer = await findReadyComposer();
    expect(composer).toBeEnabled();
    await user.type(composer, '继续处理另一项');
    expect(composer).toHaveValue('继续处理另一项');
  });

  it('restores one source-matched published reply after reload without promoting the raw Task result', async () => {
    const finalReply: ConversationMessage = {
      ...reply, id: 'mfinal001', sequence: 3, task_id: undefined, content: '主助手整理后的最终回复',
      sources: [{ user_message_id: message.id, task_id: task.id, task_revision: 2, event_sequence: 9, content: '主助手整理后的最终回复' }],
    };
    const publishedTask: Task = {
      ...task, status: 'completed', result: 'Task A 原始结果', current_revision: 2, desired_revision: 2,
      result_message_pending: false, result_message_published: true,
      result_review: { state: 'published', source_task_id: task.id, source_task_revision: 2,
        source_event_sequence: 9, reply_message_id: finalReply.id },
      assistant_message: { ...reply, id: 'msnapshot001', task_id: task.id, task_revision: 2,
        result_event_sequence: 9, status: 'completed' },
    };
    mockCommon({ conversations: [conversation], messages: [message, finalReply] });
    vi.spyOn(portalApi, 'task').mockResolvedValue(publishedTask);

    const firstLoad = renderPage();
    await screen.findByText('主助手整理后的最终回复');
    firstLoad.unmount();

    renderPage();
    await openTaskDynamics();
    const finalMessages = await screen.findAllByText('主助手整理后的最终回复');
    expect(finalMessages).toHaveLength(1);
    expect(document.querySelectorAll('.chat-message-assistant')).toHaveLength(1);
    expect(document.querySelector('.chat-message-assistant')).toHaveTextContent('主助手整理后的最终回复');
    expect(document.querySelector('.chat-message-assistant')).not.toHaveTextContent('Task A 原始结果');
    const activity = await findTaskActivity(task.id);
    expect(within(activity).getByRole('region', { name: '结果整理' })).toHaveTextContent('最终回复已同步到会话。');
  });

  it('shows the second parallel Task result while the first Task is still being reviewed', async () => {
    const secondMessage = { ...message, id: 'm2345678', sequence: 2, task_id: 'q7654321', content: '并发任务 B' };
    const finalReply = { ...reply, id: 'm3456789', sequence: 3, task_id: undefined, content: '任务 B 的汇总回复', sources: [{
      user_message_id: secondMessage.id, task_id: secondMessage.task_id!, task_revision: 1, event_sequence: 9, content: '任务 B 的汇总回复',
    }] };
    const firstTask = {
      ...task, status: 'running' as const, result: '任务 A 的原始结果', current_revision: 1, desired_revision: 1,
      dependencies: [], child_jobs: [{ job_id: 'child-a', task_step_id: 's1234567', title: '任务 A 子工作', task_revision: 1, status: 'running' }],
      result_review: { state: 'processing', source_task_id: task.id, source_task_revision: 1, source_event_sequence: 7, reply_message_id: null },
    } as unknown as Task;
    const secondTask = {
      ...task, id: secondMessage.task_id, status: 'completed' as const, result: '任务 B 的原始结果', current_revision: 1, desired_revision: 1,
      dependencies: [], child_jobs: [{ job_id: 'child-b', task_step_id: 's2345678', title: '任务 B 子工作', task_revision: 1, status: 'succeeded' }],
      result_review: { state: 'published', source_task_id: secondMessage.task_id, source_task_revision: 1, source_event_sequence: 9, reply_message_id: finalReply.id },
      assistant_message: { ...finalReply, task_id: secondMessage.task_id, task_revision: 1, result_event_sequence: 9, status: 'completed' as const },
    } as unknown as Task;
    mockCommon({ conversations: [conversation], messages: [message, secondMessage, finalReply] });
    vi.mocked(portalApi.task).mockImplementation(async (id) => id === secondTask.id ? secondTask : firstTask);
    renderPage();

    const firstPanel = await findTaskActivity(firstTask.id);
    expect(within(firstPanel).getByRole('region', { name: '结果整理' }))
      .toHaveTextContent('主助手正在整理原始结果。');
    const secondPanel = await findTaskActivity(secondTask.id);
    expect(within(secondPanel).getByRole('region', { name: '结果整理' }))
      .toHaveTextContent('最终回复已同步到会话。');
    expect(await screen.findByText('任务 B 的汇总回复')).toBeInTheDocument();
    const assistantReplies = Array.from(document.querySelectorAll('.chat-message-assistant')).map((element) => element.textContent ?? '');
    expect(assistantReplies.some((content) => content.includes('任务 A 的原始结果') || content.includes('任务 B 的原始结果'))).toBe(false);
  });

  it('does not report a published reply as synced when its source revision does not match', async () => {
    const publishedTask = {
      ...task, current_revision: 1, desired_revision: 1, result: 'Task 原始结果',
      dependencies: [], child_jobs: [],
      result_review: { state: 'published', source_task_id: task.id, source_task_revision: 1,
        source_event_sequence: 4, reply_message_id: 'mfinal001' },
    } as unknown as Task;
    const staleReply = { ...reply, id: 'mfinal001', sequence: 3, task_id: undefined, content: '旧修订的消息',
      sources: [{ user_message_id: message.id, task_id: task.id, task_revision: 0, event_sequence: 3, content: '旧修订的消息' }] };
    mockCommon({ conversations: [conversation], messages: [message, staleReply] });
    vi.spyOn(portalApi, 'task').mockResolvedValue(publishedTask);
    renderPage();

    const panel = await findTaskActivity(task.id);
    expect(within(panel).getByRole('region', { name: '结果整理' }))
      .toHaveTextContent('最终回复已发布，正在从会话历史补取。');
  });

  it.each([
    ['pending', '原始结果已保存，等待主助手整理。'],
    ['published', '最终回复已发布，正在从会话历史补取。'],
  ] as const)('does not present a %s K2 review result as a final assistant reply', async (state, reviewText) => {
    const reviewTask = {
      ...task, current_revision: 1, desired_revision: 1, result: '仅属于 Task 的原始结果',
      dependencies: [], child_jobs: [],
      result_review: {
        state, source_task_id: task.id, source_task_revision: 1, source_event_sequence: 4,
        reply_message_id: state === 'published' ? 'mfinal001' : null,
      },
      assistant_message: { ...reply, id: 'msnapshot1', task_revision: 1, result_event_sequence: 4, status: 'completed' as const },
    } as unknown as Task;
    mockCommon({ conversations: [conversation], messages: [message] });
    vi.mocked(portalApi.task).mockResolvedValue(reviewTask);
    renderPage();

    const panel = await findTaskActivity(task.id);
    expect(within(panel).getByRole('region', { name: '结果整理' })).toHaveTextContent(reviewText);
    expect(mergeTaskAssistantMessage([], reviewTask)).toHaveLength(0);
    if (state === 'published') {
      const publishedReply = { ...reply, id: 'mfinal001', sources: [{ user_message_id: message.id, task_id: task.id,
        task_revision: 1, event_sequence: 4, content: reply.content }] };
      expect(mergeTaskAssistantMessage([publishedReply], reviewTask)).toHaveLength(1);
    }
    const assistantReplies = Array.from(document.querySelectorAll('.chat-message-assistant')).map((element) => element.textContent ?? '');
    expect(assistantReplies.some((content) => content.includes('仅属于 Task 的原始结果'))).toBe(false);
  });

  it('does not merge a completed Task snapshot when its K2 projection is malformed', () => {
    const invalidTask = { ...task, dependencies: [], assistant_message: {
      ...reply, id: 'msnapshot-invalid', task_revision: 1, result_event_sequence: 4, status: 'completed' as const,
    } } as unknown as Task;
    expect(mergeTaskAssistantMessage([], invalidTask)).toHaveLength(0);
  });

  it('does not merge a streaming K2 Task snapshot from an older revision', () => {
    const staleTask = {
      ...task, status: 'running' as const, current_revision: 3, desired_revision: 3,
      dependencies: [], child_jobs: [], result_review: null,
      assistant_message: { ...reply, id: 'msnapshot-old-revision', task_revision: 2,
        result_event_sequence: 6, status: 'streaming' as const },
    } as unknown as Task;
    const currentSnapshotTask = {
      ...staleTask, current_revision: 2, desired_revision: 3,
      assistant_message: { ...staleTask.assistant_message!, id: 'msnapshot-current-revision', task_revision: 2 },
    } as unknown as Task;

    expect(mergeTaskAssistantMessage([], staleTask)).toHaveLength(0);
    expect(mergeTaskAssistantMessage([], currentSnapshotTask)).toHaveLength(1);
  });

  it('keeps a Task result inspectable but never promotes it when K2 review is not published', async () => {
    const unreviewedTask = {
      ...task, status: 'completed' as const, current_revision: 1, desired_revision: 1, result: '尚未整理的 Task 原始结果',
      dependencies: [], child_jobs: [], result_review: null,
      assistant_message: { ...reply, id: 'msnapshot-unreviewed', task_revision: 1, result_event_sequence: 4, status: 'completed' as const },
    } as unknown as Task;
    mockCommon({ conversations: [conversation], messages: [message] });
    vi.spyOn(portalApi, 'task').mockResolvedValue(unreviewedTask);
    renderPage();

    const panel = await findTaskActivity(task.id);
    await userEvent.setup().click(within(panel).getByText('更多信息'));
    const rawResult = within(panel).getByRole('region', { name: '原始结果' });
    expect(within(rawResult).getByText('查看原始结果（不是最终回复）')).toBeInTheDocument();
    expect(mergeTaskAssistantMessage([], unreviewedTask)).toHaveLength(0);
    expect(Array.from(document.querySelectorAll('.chat-message-assistant')).map((element) => element.textContent))
      .not.toEqual(expect.arrayContaining([expect.stringContaining('尚未整理的 Task 原始结果')]));
  });

  it('shows a failed task snapshot as interrupted instead of still generating', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    const failedTask = asLegacyTask({
      ...task,
      status: 'failed' as const,
      result: null,
      finished_at: timestamp,
      assistant_message: {
        id: 'm2345678', conversation_id: conversation.id, sequence: 2, task_id: task.id,
        role: 'assistant' as const, content: '部分回复', status: 'streaming' as const, created_at: timestamp,
      },
    });
    vi.spyOn(portalApi, 'task').mockResolvedValue(failedTask);
    renderPage();

    const activity = await findTaskActivity(failedTask.id);
    expect(await screen.findByText('回复已中断')).toBeInTheDocument();
    expect(within(activity).getByRole('alert')).toHaveTextContent('任务执行失败，查看工作过程或更多信息了解原因。');
    expect(screen.queryByText('正在生成…')).not.toBeInTheDocument();
    expect(screen.queryByText('AI 正在处理…')).not.toBeInTheDocument();
  });

  it('normalizes a persisted streaming reply when its Task is already terminal', () => {
    const streamedSnapshot = {
      id: 'm2345678', conversation_id: conversation.id, sequence: 2, task_id: task.id,
      task_revision: 0, role: 'assistant' as const, content: '部分回复补充', status: 'streaming' as const,
      created_at: timestamp,
    };
    const persistedPrefix = {
      ...message, id: streamedSnapshot.id, sequence: streamedSnapshot.sequence, role: 'assistant' as const,
      content: '部分回复', output_status: 'streaming' as const, output_task_revision: 0,
    };
    const failedTask = asLegacyTask({
      ...task, status: 'failed' as const, assistant_message: streamedSnapshot,
    });

    expect(mergeTaskAssistantMessage([persistedPrefix], failedTask)[0]?.snapshotStatus).toBe('interrupted');
  });

  it('reports unavailable task progress without exposing internal task ids', async () => {
    const secondMessage = { ...message, id: 'm2345678', task_id: 'q7654321', content: '另一项任务' };
    mockCommon({ conversations: [conversation], messages: [message, secondMessage] });
    vi.spyOn(portalApi, 'task').mockImplementation(async (id) => { throw new Error(`读取 ${id} 失败`); });
    renderPage();

    await openTaskDynamics();
    expect(await screen.findByText('部分任务进展暂不可用，显示最近一次已确认状态。')).toBeInTheDocument();
    expect(screen.queryByText(`读取 ${task.id} 失败`)).not.toBeInTheDocument();
  });

  it('hides cached task details and artifacts after task access is revoked', async () => {
    const restrictedTask = {
      ...task,
      result: '仅授权任务可见的原始结果',
      artifacts: [{ id: 'a1234567', step_id: 's1234567', label: '受限成果名称', path: 'private/report.md',
        media_type: 'text/markdown', workspace_id: 'w1234567', workspace_revision: 2, size_bytes: 12,
        sha256: 'a'.repeat(64), verified: true, batch_item_id: null, batch_attempt: 1, created_at: timestamp }],
    } satisfies Task;
    mockCommon({ conversations: [conversation], messages: [message] });
    const getTask = vi.spyOn(portalApi, 'task').mockResolvedValue(restrictedTask);
    const user = userEvent.setup();
    const { client } = renderPage();

    await findTaskActivity(task.id);
    await user.click(screen.getByRole('button', { name: '文件' }));
    expect(await screen.findByText('受限成果名称')).toBeInTheDocument();

    getTask.mockRejectedValueOnce(new ApiError({ message: 'Task access was revoked', code: 'not_found', status: 404 }));
    await act(async () => {
      await client.invalidateQueries({ queryKey: ['task', task.id], exact: true });
    });
    await waitFor(() => expect(client.getQueryState(['task', task.id])?.status).toBe('error'));
    expect(client.getQueryData<Task>(['task', task.id])?.result).toBe('仅授权任务可见的原始结果');
    expect(screen.queryByText('受限成果名称')).not.toBeInTheDocument();
    expect(screen.queryByText('private/report.md')).not.toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: '任务' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('任务详情暂不可用，当前状态未确认。');
    expect(screen.queryByText('仅授权任务可见的原始结果')).not.toBeInTheDocument();
  });

  it('hides cached messages, attachments, and task artifacts after conversation access is revoked, then recovers on retry', async () => {
    const attachmentID = 'pa_0123456789abcdef0123456789abcdef';
    const privateMessage = { ...message, content: '受限会话消息正文', attachment_ids: [attachmentID] };
    const restrictedTask = {
      ...task, result: '受限任务结果',
      artifacts: [{ id: 'a1234567', step_id: 's1234567', label: '受限任务成果', path: 'private/report.md',
        media_type: 'text/markdown', workspace_id: 'w1234567', workspace_revision: 2, size_bytes: 12,
        sha256: 'a'.repeat(64), verified: true, batch_item_id: null, batch_attempt: 1, created_at: timestamp }],
    } satisfies Task;
    mockCommon({ conversations: [conversation], messages: [privateMessage] });
    const readMessages = vi.mocked(portalApi.conversationMessages);
    vi.mocked(portalApi.task).mockResolvedValue(restrictedTask);
    const user = userEvent.setup();
    const { client } = renderPage();

    expect(await screen.findByText('受限会话消息正文')).toBeInTheDocument();
    await findTaskActivity(task.id);
    await user.click(screen.getByRole('button', { name: '文件' }));
    expect(await screen.findByRole('link', { name: '下载消息附件 1' })).toBeInTheDocument();
    expect(await screen.findByText('受限任务成果')).toBeInTheDocument();

    readMessages.mockRejectedValueOnce(new ApiError({ message: 'Conversation access was revoked', code: 'not_found', status: 404 }))
      .mockResolvedValueOnce({ items: [privateMessage], next_cursor: null });
    await act(async () => { await client.invalidateQueries({ queryKey: ['conversation-messages', conversation.id], exact: true }); });
    await waitFor(() => expect(client.getQueryState(['conversation-messages', conversation.id])?.status).toBe('error'));
    expect(within(document.querySelector('.conversation-view')!).getByRole('alert')).toHaveTextContent('当前对话内容暂不可访问。');
    expect(screen.queryByText('受限会话消息正文')).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: '下载消息附件 1' })).not.toBeInTheDocument();
    expect(screen.queryByText('受限任务成果')).not.toBeInTheDocument();
    expect(screen.queryByText('private/report.md')).not.toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: '重试读取对话' }));
    expect(await screen.findByText('受限会话消息正文')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '文件' }));
    expect(await screen.findByRole('link', { name: '下载消息附件 1' })).toBeInTheDocument();
  });

  it('keeps cached conversation content visible during a transient network error', async () => {
    const attachmentID = 'pa_0123456789abcdef0123456789abcdef';
    const cachedMessage = { ...message, content: '断网时仍可查看的会话', attachment_ids: [attachmentID] };
    mockCommon({ conversations: [conversation], messages: [cachedMessage] });
    const readMessages = vi.mocked(portalApi.conversationMessages);
    const user = userEvent.setup();
    const { client } = renderPage();

    expect(await screen.findByText('断网时仍可查看的会话')).toBeInTheDocument();
    await user.click(await screen.findByRole('button', { name: '打开侧边栏' }));
    await user.click(screen.getByRole('button', { name: '文件' }));
    expect(await screen.findByRole('link', { name: '下载消息附件 1' })).toBeInTheDocument();

    readMessages.mockRejectedValueOnce(new TypeError('Failed to fetch'));
    await act(async () => { await client.invalidateQueries({ queryKey: ['conversation-messages', conversation.id], exact: true }); });
    await waitFor(() => expect(client.getQueryState(['conversation-messages', conversation.id])?.status).toBe('error'));
    expect(screen.getByText('断网时仍可查看的会话')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: '下载消息附件 1' })).toBeInTheDocument();
    expect(screen.queryByText('当前对话内容暂不可访问。')).not.toBeInTheDocument();
  });

  it('accepts a message even when the runtime is stopping', async () => {
    mockCommon({ conversations: [conversation] });
    vi.mocked(portalApi.execution).mockResolvedValue({ owner_id: 'u1234567', state: 'stopping', epoch: 3, pending_task_count: 1,
      runtime: { state: 'stopping', runtime_id: 'rt1234567', runtime_generation: 1, max_concurrent_tasks: 4, active_task_count: 1 }, updated_at: timestamp });
    const accept = vi.spyOn(portalApi, 'acceptConversationMessage').mockImplementation(async (conversationID, body) => acceptedReceipt(conversationID, body));
    const user = userEvent.setup();
    renderPage();
    await user.type(await findReadyComposer(), '继续提问');
    expect(await screen.findByText(/AI 服务正在停止；消息仍可先发送/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '发送' }));
    await waitFor(() => expect(accept).toHaveBeenCalledOnce());
  });

  it('keeps normal conversation submission independent of the active task', async () => {
    const running = { ...task, status: 'running' as const, result: null, finished_at: null };
    mockCommon({ conversations: [conversation], messages: [message] });
    vi.spyOn(portalApi, 'task').mockResolvedValue(running);
    const accept = vi.spyOn(portalApi, 'acceptConversationMessage').mockImplementation(async (conversationID, body) => acceptedReceipt(conversationID, body));
    const user = userEvent.setup();
    renderPage();
    await findTaskActivity(running.id);
    await user.type(await findReadyComposer(), '追加说明');
    await user.click(screen.getByRole('button', { name: '发送' }));
    await waitFor(() => expect(accept).toHaveBeenCalledWith(conversation.id,
      expect.objectContaining({ content: '追加说明' }), expect.any(String), expect.any(String), authUser.id));
    expect(accept.mock.calls[0]![1]).not.toHaveProperty('reference_task_id');
  });

  it('shows meaningful labels for built-in search and page-reading steps', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    const runningTask = { ...task, status: 'running' as const, result: null, finished_at: null, current_step: 's1234567', steps: [
      { id: 's1234567', sequence: 1, kind: 'tool' as const, status: 'succeeded' as const,
        title: 'Run cloud dsh MCP tool: morde_web_search', summary: '正在从公开新闻来源检索候选文章。', created_at: timestamp },
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
    const panel = await findTaskActivity(task.id);
    await user.click(within(panel).getByText('工作过程'));
    expect(within(panel).getAllByText('搜索网页').length).toBeGreaterThan(0);
    expect(within(panel).getByText('读取网页')).toBeInTheDocument();
    expect(within(panel).getByText('读取电脑文件')).toBeInTheDocument();
    expect(within(panel).getByText('正在从公开新闻来源检索候选文章。')).toBeInTheDocument();
    expect(within(panel).getByText('使用工具')).toBeInTheDocument();
    expect(within(panel).getByText('开始处理')).toBeInTheDocument();
    expect(within(panel).queryByText(/morde_web_/)).not.toBeInTheDocument();
    expect(within(panel).queryByText(/Hostless|internal_tool_name/)).not.toBeInTheDocument();
  });

  it('keeps event cursors and invalidation isolated by task', async () => {
    const first = { ...task, status: 'running' as const, result: null, finished_at: null, event_sequence: 3 };
    const second = { ...first, id: 'q7654321', objective: '第二项任务', event_sequence: 7 };
    const remaining = Array.from({ length: 4 }, (_, index) => ({ ...first, id: `q${String(index + 3).padStart(7, '0')}` }));
    const allTasks = [first, second, ...remaining];
    mockCommon({ conversations: [conversation], messages: [message, { ...message, id: 'm2345678', task_id: second.id }] });
    vi.mocked(portalApi.conversationTasks).mockResolvedValue({ items: allTasks, next_cursor: null, pending_count: 0 });
    vi.spyOn(portalApi, 'task').mockImplementation(async (id) => allTasks.find((item) => item.id === id)!);
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
    await waitFor(() => expect(sources).toHaveLength(4));
    expect(sources.map((source) => source.url)).toEqual(expect.arrayContaining([
      `/api/tasks/${first.id}/events?after=3`, `/api/tasks/${second.id}/events?after=7`,
    ]));
    expect(sources.map((source) => source.url)).not.toEqual(expect.arrayContaining(
      allTasks.slice(4).map((item) => `/api/tasks/${item.id}/events?after=3`),
    ));
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

  it('keeps the current Task revision when a late older-revision event arrives', async () => {
    const currentTask = {
      ...task, status: 'running' as const, result: null, finished_at: null, event_sequence: 10,
      current_revision: 3, desired_revision: 3, current_revision_started: true,
      dependencies: [], child_jobs: [{ job_id: 'child-current', task_step_id: 's1234567', title: '当前修订工作', task_revision: 3, status: 'running' }],
      result_review: null,
    } as unknown as Task;
    mockCommon({ conversations: [conversation], messages: [message] });
    const getTask = vi.mocked(portalApi.task).mockResolvedValue(currentTask);
    const sources: Array<{ listeners: Map<string, EventListener> }> = [];
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
    await waitFor(() => expect(sources).toHaveLength(1));
    const invalidate = vi.spyOn(client, 'invalidateQueries');

    await act(async () => sources[0]?.listeners.get('task.result')?.(new MessageEvent('task.result', { data: JSON.stringify({
      task_id: task.id, sequence: 11, task_revision: 2, status: 'completed', result: '旧修订结果不能覆盖当前任务',
    }) })));

    await waitFor(() => expect(getTask.mock.calls.length).toBeGreaterThan(1));
    const panel = await findTaskActivity(task.id);
    expect(panel).toHaveTextContent('进行中');
    await userEvent.setup().click(within(panel).getByText('更多信息'));
    expect(panel).toHaveTextContent('修订 3 / 3');
    expect(panel).toHaveTextContent('当前修订工作');
    expect(panel).not.toHaveTextContent('旧修订结果不能覆盖当前任务');
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ['task', task.id] });
  });

  it('keeps two task workchains and each stop action isolated in its own panel', async () => {
    const firstOutput = { ...reply, id: 'm2345678', sequence: 2, content: '第一项任务已完成首轮建议。',
      output_status: 'completed' as const, output_task_revision: 1 };
    const secondMessage = { ...message, id: 'm3456789', sequence: 3, task_id: 'q7654321', content: '第二项任务：读取网页' };
    const secondOutput = { ...reply, id: 'm4567890', sequence: 4, task_id: secondMessage.task_id,
      content: '第二项任务正在读取网页。', output_status: 'streaming' as const, output_task_revision: 1 };
    const approval = { id: 'k2345678', version: 2, kind: 'approval' as const, title: '写入文件', object: '报告.md',
      content: '准备创建报告。', impact: '会新增一个文件。' };
    const firstTask: Task = {
      ...task, status: 'waiting_user', objective: '第一项任务：审批写入', current_step: 's1234567', event_sequence: 5,
      result: null, finished_at: null, waiting_reason: 'approval', current_revision: 1, desired_revision: 1,
      steps: [{ id: 's1234567', sequence: 1, kind: 'tool', status: 'waiting_user',
        title: 'Run cloud dsh MCP tool: morde_workspace_write', summary: '', created_at: timestamp }],
      pending_request: approval,
      assistant_message: { ...firstOutput, task_id: task.id, status: 'completed', task_revision: 1 },
    };
    const secondTask: Task = {
      ...task, id: secondMessage.task_id, status: 'running', objective: secondMessage.content,
      current_step: 's2345678', event_sequence: 7, result: null, finished_at: null, waiting_reason: null,
      current_revision: 1, desired_revision: 1,
      steps: [{ id: 's2345678', sequence: 1, kind: 'tool', status: 'running',
        title: 'Run cloud dsh MCP tool: morde_web_read', summary: '', created_at: timestamp }],
      assistant_message: { ...secondOutput, status: 'streaming', task_revision: 1 },
    };
    const firstMessage = { ...message, content: '第一项任务：审批写入' };
    mockCommon({ conversations: [conversation], messages: [firstMessage, firstOutput, secondMessage, secondOutput] });
    vi.mocked(portalApi.conversationTasks).mockResolvedValue({ items: [firstTask, secondTask], next_cursor: null, pending_count: 0 });
    const snapshots = new Map<string, Task>([[firstTask.id, firstTask], [secondTask.id, secondTask]]);
    vi.mocked(portalApi.task).mockImplementation(async (id) => {
      const snapshot = snapshots.get(id);
      if (!snapshot) throw new Error(`未知任务 ${id}`);
      return snapshot;
    });
    const stop = vi.spyOn(portalApi, 'taskAction').mockImplementation(async (id, request) => {
      const snapshot = snapshots.get(id)!;
      snapshots.set(id, { ...snapshot, status: 'cancelling', stop_request: {
        id: 'stop23456', action: 'cancel', status: 'pending', safe_after: timestamp, unsettled_operation_ids: [],
      } });
      return { task_id: id, action: request.action, status: 'cancelling', unsettled_operation_ids: [] };
    });
    const user = userEvent.setup();
    renderPage();

    const firstPanel = await findTaskActivity(firstTask.id);
    expect(within(firstPanel).getByText('等待审批：写入文件')).toBeInTheDocument();
    expect(firstPanel).toHaveTextContent('保存电脑文件');
    const secondPanel = await findTaskActivity(secondTask.id);
    expect(within(secondPanel).queryByText('AI 正在处理…')).not.toBeInTheDocument();
    expect(within(secondPanel).getByRole('status')).toHaveTextContent('读取网页');
    expect(screen.getByText(firstOutput.content)).toBeInTheDocument();
    expect(screen.getByText(secondOutput.content)).toBeInTheDocument();
    expect(screen.getByText(firstOutput.content).closest('.message-list')?.querySelector('.task-activity')).toBeNull();
    expect(screen.getByText(secondOutput.content).closest('.message-list')?.querySelector('.task-activity')).toBeNull();

    const firstPanelAgain = await findTaskActivity(firstTask.id);
    await user.click(within(firstPanelAgain).getByRole('button', { name: '停止任务' }));
    await waitFor(() => expect(stop).toHaveBeenCalledWith(firstTask.id,
      { action: 'cancel', expected_status: 'waiting_user', expected_current_revision: 1, expected_desired_revision: 1 }, expect.any(String)));
    expect(await within(firstPanelAgain).findByText('正在等待服务端确认停止结果。')).toBeInTheDocument();
    expect(within(await findTaskActivity(secondTask.id)).getByRole('status')).toHaveTextContent('读取网页');
    expect(snapshots.get(secondTask.id)?.status).toBe('running');
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
    await findTaskActivity(task.id);
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

  it('keeps task progress and stop controls in the task panel beside the conversation composer', async () => {
    mockCommon({ conversations: [conversation], messages: [message, reply] });
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, status: 'running', result: null, finished_at: null });
    renderPage();

    const activity = await findTaskActivity(task.id);
    expect(within(activity).getByText('AI 正在处理…')).toBeInTheDocument();
    const messageList = screen.getByText(reply.content).closest('.message-list');
    expect(messageList?.querySelector('.task-activity')).toBeNull();
    const composer = screen.getByRole('region', { name: '发送对话消息' });
    const stop = within(activity).getByRole('button', { name: '停止任务' });
    const send = within(composer).getByRole('button', { name: '发送' });
    expect(stop).toBeEnabled();
    expect(send).toBeInTheDocument();
    expect(within(composer).queryByRole('button', { name: '停止任务' })).not.toBeInTheDocument();
    expect(within(activity).getByRole('button', { name: '暂停任务' })).toBeEnabled();
    expect(within(composer).queryByRole('button', { name: '暂停任务' })).not.toBeInTheDocument();
    expect(screen.queryByText('消息将追加到当前任务，并沿用当前模型和目标。')).not.toBeInTheDocument();
  });

  it('keeps the conversation scroll position steady when only task progress changes', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    const running = { ...task, status: 'running' as const, result: null, finished_at: null };
    vi.spyOn(portalApi, 'task').mockResolvedValue(running);
    const { container, client } = renderPage();
    const activity = await findTaskActivity(task.id);
    expect(within(activity).getByText('AI 正在处理…')).toBeInTheDocument();

    const viewport = container.querySelector('.conversation-view');
    if (!(viewport instanceof HTMLElement)) throw new Error('conversation viewport not found');
    let scrollTop = 160;
    const writeScrollTop = vi.fn((value: number) => { scrollTop = value; });
    Object.defineProperties(viewport, {
      scrollHeight: { configurable: true, value: 900 },
      clientHeight: { configurable: true, value: 400 },
      scrollTop: { configurable: true, get: () => scrollTop, set: writeScrollTop },
    });

    await act(async () => {
      client.setQueryData(['task', task.id], {
        ...running, event_sequence: running.event_sequence + 1,
        current_revision: 1, desired_revision: 1, current_revision_started: true,
      });
    });

    expect(await screen.findByText('开始处理你的请求。')).toBeInTheDocument();
    expect(writeScrollTop).not.toHaveBeenCalled();
    expect(scrollTop).toBe(160);
  });

  it.each([
    { current_revision: 1, desired_revision: 1, current_revision_started: false, text: '任务已保存，正在等待开始。' },
    { current_revision: 1, desired_revision: 1, current_revision_started: true, text: '开始处理你的请求。' },
    { current_revision: 1, desired_revision: 2, current_revision_started: true, text: '正在调整任务，等待开始新要求。' },
  ])('shows the confirmed state of the current revision', async ({ text, ...evidence }) => {
    mockCommon({ conversations: [conversation], messages: [message] });
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, ...evidence, status: 'running', result: null, finished_at: null });
    renderPage();
    const activity = await findTaskActivity(task.id);
    expect(await within(activity).findByText(text)).toBeInTheDocument();
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
    vi.spyOn(portalApi, 'task').mockResolvedValueOnce(interrupted).mockResolvedValueOnce(interrupted).mockResolvedValue(cancelled);
    const stop = vi.spyOn(portalApi, 'taskAction').mockResolvedValue({
      task_id: task.id, action: 'cancel', status: 'cancelled', unsettled_operation_ids: [],
    });
    renderPage();

    const activity = await findTaskActivity(task.id);
    expect(await within(activity).findByText(/AI 运行中断，无法确认本次执行结果/)).toBeInTheDocument();
    expect(screen.getByText('模型服务正在限流，稍后再试。')).toBeInTheDocument();
    expect(screen.queryByText(/category:/)).not.toBeInTheDocument();
    expect(screen.queryByText('Review cloud task recovery')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '等待接收' })).not.toBeInTheDocument();
    await userEvent.setup().click(within(activity).getByRole('button', { name: '停止任务' }));
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
    const panel = await findTaskActivity(task.id);
    expect(within(panel).getByRole('status')).toHaveTextContent('原文已保留；请缩短后重新发送');
    expect(screen.queryByText(/AI 运行中断，无法确认本次执行结果/)).not.toBeInTheDocument();
  });

  it('shows a computer problem only on the Task that attempted to use it', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, status: 'waiting_user', result: null, finished_at: null,
      waiting_reason: 'computer_unavailable', current_step: null, steps: [] });
    renderPage();
    await openTaskDynamics();
    expect(await screen.findByText(/所选云端电脑当前不可用或无法确认访问权限/)).toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: '消息内容' })).toBeEnabled();
  });

  it('allows a new task while another task waits for approval', async () => {
    mockCommon({ conversations: [conversation], messages: [message, reply] });
    const approval = { id: 'k1234567', version: 4, kind: 'approval' as const, title: '写入文件', object: '工作区文件', content: '创建报告.md', impact: '会新增一个文件' };
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, status: 'waiting_user', result: null,
      waiting_reason: 'approval', pending_request: approval, finished_at: null });
    const accept = vi.spyOn(portalApi, 'acceptConversationMessage');
    renderPage();

    const activity = await findTaskActivity(task.id);
    expect(screen.getByRole('textbox', { name: '消息内容' })).toBeEnabled();
    expect(within(activity).getByRole('button', { name: '停止任务' })).toBeEnabled();
    expect(accept).not.toHaveBeenCalled();
  });

  it('resyncs task events after a sequence gap without displaying internal event history', async () => {
    mockCommon({ conversations: [conversation], messages: [message, reply] });
    const runningTask = { ...task, status: 'running' as const, result: null, finished_at: null, current_step: 's1234567', steps: [{
      id: 's1234567', sequence: 1, kind: 'tool' as const, status: 'running' as const, title: '正在整理资料', summary: '', created_at: timestamp,
    }] };
    const getTask = vi.spyOn(portalApi, 'task').mockResolvedValueOnce(runningTask).mockResolvedValue({ ...runningTask, event_sequence: 3 });
    let sourceURL = '';
    const sourceURLs: string[] = [];
    let sourceOpen: ((event: Event) => void) | null = null;
    let sourceError: ((event: Event) => void) | null = null;
    const eventHandlers = new Map<string, (event: MessageEvent<string>) => void>();
    class TestEventSource {
      get onopen() { return sourceOpen; }
      set onopen(handler: ((event: Event) => void) | null) { sourceOpen = handler; }
      get onerror() { return sourceError; }
      set onerror(handler: ((event: Event) => void) | null) { sourceError = handler; }
      constructor(url: string) { sourceURL = url; sourceURLs.push(url); }
      addEventListener(type: string, handler: EventListener) { eventHandlers.set(type, handler as (event: MessageEvent<string>) => void); }
      close() {}
    }
    vi.stubGlobal('EventSource', TestEventSource);
    renderPage();
    await openTaskDynamics();
    await findTaskActivity(task.id);

    await waitFor(() => expect(sourceURL).not.toBe(''));
    expect(sourceURL).toBe(`/api/tasks/${task.id}/events?after=${task.event_sequence}`);
    await act(async () => sourceOpen?.(new Event('open')));
    expect(screen.getAllByText('正在整理资料').length).toBeGreaterThan(0);
    await act(async () => sourceError?.(new Event('error')));
    expect(screen.getByText('连接中断，正在重连…')).toBeInTheDocument();
    await act(async () => sourceOpen?.(new Event('open')));
    expect(screen.queryByText('连接中断，正在重连…')).not.toBeInTheDocument();
    await act(async () => eventHandlers.get('task.step')?.(new MessageEvent('task.step', { data: JSON.stringify({
      task_id: task.id, sequence: 3, type: 'task.step', created_at: timestamp, payload: { secret: 'must stay private' },
    }) })));
    await waitFor(() => expect(getTask).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(sourceURLs).toHaveLength(2));
    expect(sourceURLs[1]).toBe(`/api/tasks/${task.id}/events?after=3`);
    expect(screen.queryByText('任务步骤已更新')).not.toBeInTheDocument();
    expect(screen.queryByText('must stay private')).not.toBeInTheDocument();
  });

  it('keeps a message locally when service identity is unavailable', async () => {
    mockCommon({ conversations: [conversation] });
    vi.mocked(portalApi.capabilities).mockRejectedValue(new Error('连接中断'));
    const accept = vi.spyOn(portalApi, 'acceptConversationMessage');
    const user = userEvent.setup();
    renderPage();
    const composer = await findReadyComposer();
    await user.type(composer, '等待服务身份');
    expect(screen.getByRole('button', { name: '发送' })).toBeEnabled();
    await user.click(screen.getByRole('button', { name: '发送' }));
    expect(await screen.findByText(/已保存在本机，等待发送/)).toBeInTheDocument();
    expect(within(screen.getByRole('article', { name: '你的消息' })).getByRole('status'))
      .toHaveTextContent(/等待确认原服务和账号/);
    expect(accept).not.toHaveBeenCalled();
  });

  it.each(['approve', 'reject'] as const)('sends the versioned %s decision for the exact pending approval', async (decision) => {
    mockCommon({ conversations: [conversation], messages: [message] });
    const approval = { id: 'k1234567', version: 4, kind: 'approval' as const, title: '写入文件', object: '工作区文件', content: '创建报告.md', impact: '会新增一个文件' };
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, status: 'waiting_user', result: null, waiting_reason: 'approval', pending_request: approval, finished_at: null });
    const respond = vi.spyOn(portalApi, 'respondToTaskApproval').mockResolvedValue({ task_id: task.id, action: 'respond', status: 'queued', unsettled_operation_ids: [] });
    const user = userEvent.setup();
    renderPage();
    await openTaskDynamics();
    await findTaskActivity(task.id);
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
    await openTaskDynamics();
    await findTaskActivity(task.id);
    await user.click(await screen.findByRole('button', { name: '批准并继续' }));
    await waitFor(() => expect(respond).toHaveBeenCalledTimes(1));
    await user.click(await screen.findByRole('button', { name: '批准并继续' }));
    await waitFor(() => expect(respond).toHaveBeenCalledTimes(2));
    expect(respond.mock.calls[0]?.[3]).toBeTruthy();
    expect(respond.mock.calls[1]?.[3]).toBe(respond.mock.calls[0]?.[3]);
  });

  it('prevents duplicate approval calls while the first response is pending', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    const approval = { id: 'k1234567', version: 4, kind: 'approval' as const, title: '写入文件', object: '工作区文件', content: '创建报告.md', impact: '会新增一个文件' };
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, status: 'waiting_user', result: null, waiting_reason: 'approval', pending_request: approval, finished_at: null });
    const response = { task_id: task.id, action: 'respond', status: 'queued' as const, unsettled_operation_ids: [] };
    let release!: (value: typeof response) => void;
    const pending = new Promise<typeof response>((resolve) => { release = resolve; });
    const respond = vi.spyOn(portalApi, 'respondToTaskApproval').mockImplementation(() => pending);
    const user = userEvent.setup();
    renderPage();
    await openTaskDynamics();
    await findTaskActivity(task.id);
    const approve = await screen.findByRole('button', { name: '批准并继续' });
    await user.click(approve);
    expect(approve).toBeDisabled();
    await user.click(approve);
    expect(respond).toHaveBeenCalledOnce();
    await act(async () => { release(response); await pending; });
  });

  it('refreshes after another device changes the pending approval version', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    const approval = { id: 'k1234567', version: 4, kind: 'approval' as const, title: '写入文件', object: '工作区文件', content: '创建报告.md', impact: '会新增一个文件' };
    const freshTask = { ...task, status: 'queued' as const, result: null, waiting_reason: null, pending_request: null, finished_at: null };
    const getTask = vi.spyOn(portalApi, 'task').mockResolvedValueOnce({ ...task, status: 'waiting_user', result: null, waiting_reason: 'approval', pending_request: approval, finished_at: null }).mockResolvedValueOnce(freshTask);
    vi.spyOn(portalApi, 'respondToTaskApproval').mockRejectedValue(new ApiError({
      message: 'pending request version is stale', code: 'conflict', status: 409,
    }));
    const user = userEvent.setup();
    renderPage();
    await openTaskDynamics();
    await findTaskActivity(task.id);
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
    await openTaskDynamics();
    await findTaskActivity(task.id);
    await user.type(await screen.findByRole('textbox', { name: '你的回答' }), '  /workspace/reports  ');
    await user.click(screen.getByRole('button', { name: '提交并继续' }));
    await waitFor(() => expect(answer).toHaveBeenCalledWith(task.id, {
      clarification_id: clarification.id, clarification_version: clarification.version, answer: '/workspace/reports',
    }, expect.any(String)));
  });

  it('links a browser authentication handoff to the exact task host', async () => {
    mockCommon({ conversations: [conversation], messages: [message], hosts: [targetHost] });
    const clarification = { id: 'k7654321', version: 2, question: '请先在云端浏览器登录，然后告诉我继续。' };
    vi.spyOn(portalApi, 'task').mockResolvedValue({
      ...task, status: 'waiting_user', result: null, waiting_reason: 'clarification', clarification_request: clarification,
      host_id: targetHost.id, steps: [{ id: 's1234567', sequence: 1, kind: 'tool', status: 'succeeded',
        title: 'Run cloud dsh tool: browser_prepare_user_handoff', summary: '', created_at: timestamp }], finished_at: null,
    });
    renderPage();

    const activity = await findTaskActivity(task.id);
    expect(within(activity).getByRole('link', { name: '打开目标电脑进行登录' })).toHaveAttribute('href', `/hosts#host-${targetHost.id}`);
    expect(within(activity).getByText('登录完成后交还 AI，再回到这里提交并继续。')).toBeInTheDocument();
  });

  it('does not offer a Host handoff link for an ordinary clarification', async () => {
    mockCommon({ conversations: [conversation], messages: [message], hosts: [targetHost] });
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, status: 'waiting_user', result: null, waiting_reason: 'clarification',
      clarification_request: { id: 'k7654321', version: 2, question: '需要补充一个细节。' }, host_id: targetHost.id, finished_at: null });
    renderPage();
    const activity = await findTaskActivity(task.id);
    expect(within(activity).queryByRole('link', { name: '打开目标电脑进行登录' })).not.toBeInTheDocument();
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
    await openTaskDynamics();
    await findTaskActivity(task.id);
    await user.type(await screen.findByRole('textbox', { name: '你的回答' }), '  /workspace/reports  ');
    await user.click(await screen.findByRole('button', { name: '提交并继续' }));
    await waitFor(() => expect(answer).toHaveBeenCalledTimes(1));
    await user.click(await screen.findByRole('button', { name: '提交并继续' }));
    await waitFor(() => expect(answer).toHaveBeenCalledTimes(2));
    expect(answer.mock.calls[0]?.[2]).toBeTruthy();
    expect(answer.mock.calls[1]?.[2]).toBe(answer.mock.calls[0]?.[2]);
  });

  it('refreshes after another device changes the clarification version', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    const clarification = { id: 'k7654321', version: 2, question: '文件应该保存在哪个目录？' };
    const freshTask = { ...task, status: 'queued' as const, result: null, waiting_reason: null, clarification_request: null, finished_at: null };
    const getTask = vi.spyOn(portalApi, 'task').mockResolvedValueOnce({ ...task, status: 'waiting_user', result: null, waiting_reason: 'clarification', clarification_request: clarification, finished_at: null }).mockResolvedValueOnce(freshTask);
    vi.spyOn(portalApi, 'answerTaskInput').mockRejectedValue(new ApiError({
      message: 'clarification version is stale', code: 'conflict', status: 409,
    }));
    const user = userEvent.setup();
    renderPage();
    await openTaskDynamics();
    await findTaskActivity(task.id);
    await user.type(await screen.findByRole('textbox', { name: '你的回答' }), '放到报告目录');
    await user.click(await screen.findByRole('button', { name: '提交并继续' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('任务状态已变化，已刷新最新状态');
    await waitFor(() => expect(getTask).toHaveBeenCalledTimes(2));
    expect(screen.queryByRole('form', { name: '补充任务信息' })).not.toBeInTheDocument();
  });

  it.each(['approval', 'clarification'] as const)('does not report a %s response as successful after access is revoked', async (kind) => {
    mockCommon({ conversations: [conversation], messages: [message] });
    const approval = { id: 'k1234567', version: 4, kind: 'approval' as const, title: '写入文件', object: '工作区文件', content: '创建报告.md', impact: '会新增一个文件' };
    const clarification = { id: 'k7654321', version: 2, question: '文件应该保存在哪个目录？' };
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, status: 'waiting_user', result: null, waiting_reason: kind,
      pending_request: kind === 'approval' ? approval : null,
      clarification_request: kind === 'clarification' ? clarification : null, finished_at: null });
    const revoked = new ApiError({ message: 'Task access was revoked', code: 'not_found', status: 404 });
    const respond = vi.spyOn(portalApi, 'respondToTaskApproval').mockRejectedValue(revoked);
    const answer = vi.spyOn(portalApi, 'answerTaskInput').mockRejectedValue(revoked);
    const user = userEvent.setup();
    renderPage();
    await openTaskDynamics();
    await findTaskActivity(task.id);
    if (kind === 'approval') {
      await user.click(await screen.findByRole('button', { name: '批准并继续' }));
      await waitFor(() => expect(respond).toHaveBeenCalledWith(task.id, approval.id,
        { version: approval.version, decision: 'approve' }, expect.any(String)));
      expect(screen.queryByText('已批准，任务正在恢复。')).not.toBeInTheDocument();
    } else {
      await user.type(await screen.findByRole('textbox', { name: '你的回答' }), '放到报告目录');
      await user.click(screen.getByRole('button', { name: '提交并继续' }));
      await waitFor(() => expect(answer).toHaveBeenCalledWith(task.id, {
        clarification_id: clarification.id, clarification_version: clarification.version, answer: '放到报告目录',
      }, expect.any(String)));
      expect(screen.queryByText('补充信息已提交，任务正在恢复。')).not.toBeInTheDocument();
    }
    expect(await screen.findByRole('alert')).toHaveTextContent('请求的内容不存在');
  });

  it('submits an idempotent stop request and waits for server confirmation', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    const runningTask = { ...task, status: 'running' as const, result: null, finished_at: null,
      current_revision: 2, desired_revision: 3 };
    const stoppingTask = {
      ...task, status: 'cancelling' as const, result: null, finished_at: null,
      current_revision: 2, desired_revision: 3,
      stop_request: { id: 'stop12345', action: 'cancel' as const, status: 'pending' as const,
        safe_after: timestamp, unsettled_operation_ids: ['op1234567'] },
    };
    vi.spyOn(portalApi, 'task').mockResolvedValueOnce(runningTask).mockResolvedValueOnce(runningTask).mockResolvedValue(stoppingTask);
    const stop = vi.spyOn(portalApi, 'taskAction').mockResolvedValue({
      task_id: task.id, action: 'cancel', status: 'cancelling', unsettled_operation_ids: ['op1234567'],
    });
    const user = userEvent.setup();
    renderPage();
    await openTaskDynamics();
    await findTaskActivity(task.id);
    expect(await screen.findByRole('button', { name: '停止任务' })).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '停止任务' }));
    await waitFor(() => expect(stop).toHaveBeenCalledWith(task.id,
      { action: 'cancel', expected_status: 'running', expected_current_revision: 2, expected_desired_revision: 3 }, expect.any(String)));
    expect(await screen.findByText('停止请求已受理，等待任务状态更新。')).toBeInTheDocument();
    expect(await screen.findByText('停止请求已受理，仍有 1 个操作结果待确认。')).toBeInTheDocument();
  });

  it('can resume a previously paused Task with its current status', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    const pausedTask = { ...task, status: 'paused' as const, result: null, finished_at: null,
      current_revision: 5, desired_revision: 5 };
    vi.spyOn(portalApi, 'task').mockResolvedValue(pausedTask);
    const action = vi.spyOn(portalApi, 'taskAction')
      .mockResolvedValue({ task_id: task.id, action: 'resume', status: 'queued', unsettled_operation_ids: [] });
    const user = userEvent.setup();
    renderPage();
    await openTaskDynamics();
    await findTaskActivity(task.id);
    expect(await screen.findByRole('button', { name: '继续任务' })).toBeEnabled();
    await user.click(screen.getByRole('button', { name: '继续任务' }));
    await waitFor(() => expect(action).toHaveBeenCalledWith(task.id,
      { action: 'resume', expected_status: 'paused', expected_current_revision: 5, expected_desired_revision: 5 }, expect.any(String)));
  });

  it('reuses the same idempotency key when a stop response is lost', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, status: 'running', result: null, finished_at: null });
    const stop = vi.spyOn(portalApi, 'taskAction')
      .mockRejectedValueOnce(new Error('连接中断'))
      .mockResolvedValueOnce({ task_id: task.id, action: 'cancel', status: 'cancelling', unsettled_operation_ids: [] });
    const user = userEvent.setup();
    renderPage();
    await openTaskDynamics();
    await findTaskActivity(task.id);
    await user.click(await screen.findByRole('button', { name: '停止任务' }));
    await waitFor(() => expect(stop).toHaveBeenCalledTimes(1));
    await user.click(await screen.findByRole('button', { name: '停止任务' }));
    await waitFor(() => expect(stop).toHaveBeenCalledTimes(2));
    expect(stop.mock.calls[1]?.[2]).toBe(stop.mock.calls[0]?.[2]);
  });

  it('prevents duplicate stop calls while the selected task is awaiting the first response', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, status: 'running', result: null, finished_at: null });
    const response = { task_id: task.id, action: 'cancel', status: 'cancelling' as const, unsettled_operation_ids: [] };
    let release!: (value: typeof response) => void;
    const pending = new Promise<typeof response>((resolve) => { release = resolve; });
    const stop = vi.spyOn(portalApi, 'taskAction').mockImplementation(() => pending);
    const user = userEvent.setup();
    renderPage();
    await openTaskDynamics();
    await findTaskActivity(task.id);
    const stopButton = await screen.findByRole('button', { name: '停止任务' });
    await user.click(stopButton);
    expect(stopButton).toBeDisabled();
    await user.click(stopButton);
    expect(stop).toHaveBeenCalledOnce();
    await act(async () => { release(response); await pending; });
  });

  it('refreshes instead of stopping a Task whose revision changed after it was selected', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    const selectedTask = { ...task, status: 'running' as const, result: null, finished_at: null,
      current_revision: 2, desired_revision: 3 };
    const latestTask = { ...selectedTask, current_revision: 3, desired_revision: 3 };
    const getTask = vi.spyOn(portalApi, 'task').mockResolvedValueOnce(selectedTask).mockResolvedValue(latestTask);
    const stop = vi.spyOn(portalApi, 'taskAction').mockResolvedValue({
      task_id: task.id, action: 'cancel', status: 'cancelling', unsettled_operation_ids: [],
    });
    const user = userEvent.setup();
    renderPage();
    await openTaskDynamics();
    await findTaskActivity(task.id);
    await user.click(await screen.findByRole('button', { name: '停止任务' }));

    expect(await screen.findByRole('alert')).toHaveTextContent('任务状态已变化，已刷新最新状态');
    expect(stop).not.toHaveBeenCalled();
    await waitFor(() => expect(getTask.mock.calls.length).toBeGreaterThanOrEqual(3));
    const activity = await findTaskActivity(task.id);
    await user.click(within(activity).getByText('更多信息'));
    expect(activity).toHaveTextContent('修订 3 / 3');
  });

  it('does not label a stop without a server receipt as completed', async () => {
    mockCommon({ conversations: [conversation], messages: [message] });
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, status: 'cancelling', result: null, stop_request: null, finished_at: null });
    renderPage();
    await openTaskDynamics();
    await findTaskActivity(task.id);
    expect(await screen.findByText('停止状态待确认，正在重新读取服务端任务。')).toBeInTheDocument();
    expect(screen.queryByText('服务端已确认任务停止。')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '停止任务' })).not.toBeInTheDocument();
  });
});
