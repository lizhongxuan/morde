import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { portalApi } from '../app/api';
import { ToastProvider } from '../app/toast';
import { Assistant } from './Assistant';

const timestamp = '2026-10-22T08:00:00.000Z';
const host = {
  id: 'h1234567', name: '开发电脑', owner_id: 'u1234567', template_id: 'std', isolation: 'container', state: 'running', state_reason: '', url: '',
  workspaces: [],
  resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 }, runtime_generation: 1, control_version: 1,
  control_state: 'ai', expires_at: timestamp, last_active_at: timestamp, created_at: timestamp,
} satisfies Awaited<ReturnType<typeof portalApi.hosts>>['items'][number];
const model = {
  id: 'p1234567', name: '团队模型', base_url: 'https://models.example.test/v1', model_name: 'morde-test', shared: true, has_api_key: true,
  capabilities: { text: true, tools: true, vision: false }, checked_at: timestamp, created_at: timestamp, updated_at: timestamp,
} satisfies Awaited<ReturnType<typeof portalApi.modelConnections>>['items'][number];
const preferences = { assistant_name: 'Morde', selected_model_id: model.id, default_host_id: host.id, updated_at: timestamp } satisfies Awaited<ReturnType<typeof portalApi.preferences>>;
const onboarding = {
  model_status: 'ready', models: [{ id: model.id, name: model.name, capabilities: model.capabilities }], selected_model_id: model.id,
  computer_status: 'ready', computers: [{ id: host.id, name: host.name, state: 'running' }], default_host_id: host.id, can_submit: true, next_action: 'submit_task',
} satisfies Awaited<ReturnType<typeof portalApi.onboardingStatus>>;
const task = {
  id: 'q1234567', owner_id: 'u1234567', host_id: host.id, conversation_id: 'c1234567', model_id: model.id, objective: '整理项目文件', status: 'queued',
  current_step: null, result: null, waiting_reason: null, budget: { active_seconds: 1800, tool_calls: 100, model_tokens: 50000 },
  usage: { active_seconds: 0, tool_calls: 0, model_tokens: null }, steps: [], batch_items: [], artifacts: [], pending_request: null, clarification_request: null,
  stop_request: null, created_at: timestamp, updated_at: timestamp, finished_at: null,
} satisfies Awaited<ReturnType<typeof portalApi.task>>;
const conversation = { id: task.conversation_id, host_id: host.id, title: '已有会话', created_at: timestamp, updated_at: timestamp } satisfies Awaited<ReturnType<typeof portalApi.conversations>>['items'][number];
const otherConversation = { ...conversation, id: 'c7654321', title: '另一个会话' };

class TestEventSource {
  static instances: TestEventSource[] = [];
  private listeners = new Map<string, Set<EventListener>>();
  onerror: ((event: Event) => void) | null = null;

  constructor() { TestEventSource.instances.push(this); }
  addEventListener(type: string, listener: EventListenerOrEventListenerObject) {
    const callback = typeof listener === 'function' ? listener : (event: Event) => listener.handleEvent(event);
    this.listeners.set(type, new Set(this.listeners.get(type)).add(callback));
  }
  removeEventListener(type: string, listener: EventListenerOrEventListenerObject) {
    const callback = typeof listener === 'function' ? listener : (event: Event) => listener.handleEvent(event);
    this.listeners.get(type)?.delete(callback);
  }
  close() {}
  emit(type: string, data: string) {
    const event = new MessageEvent(type, { data });
    for (const listener of this.listeners.get(type) ?? []) listener(event);
  }
}

function renderPage(path = '/assistant') {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter initialEntries={[path]}><Assistant /></MemoryRouter></ToastProvider></QueryClientProvider>);
}

describe('Assistant', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); TestEventSource.instances = []; });

  it('submits a new persistent task to the selected running host and model', async () => {
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [host] });
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [model] });
    vi.spyOn(portalApi, 'preferences').mockResolvedValue(preferences);
    vi.spyOn(portalApi, 'onboardingStatus').mockResolvedValue(onboarding);
    vi.spyOn(portalApi, 'conversations').mockResolvedValue({ items: [], next_cursor: null });
    const submit = vi.spyOn(portalApi, 'submitTask').mockResolvedValue(task);
    const user = userEvent.setup();
    renderPage();

    const prompt = await screen.findByRole('textbox', { name: '要完成的任务' });
    await user.type(prompt, '整理项目文件');
    await user.click(screen.getByRole('button', { name: /交办任务/ }));

    await waitFor(() => expect(submit).toHaveBeenCalledWith(
      { host_id: host.id, model_id: model.id, objective: '整理项目文件' }, expect.stringMatching(/^[A-Za-z0-9._~-]{8,128}$/),
    ));
    expect(await screen.findByText('任务已接收，可在此查看进度。')).toBeInTheDocument();
  });

  it('ignores legacy third-party app URL params and submits a generic Morde task', async () => {
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [host] });
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [model] });
    vi.spyOn(portalApi, 'preferences').mockResolvedValue(preferences);
    vi.spyOn(portalApi, 'onboardingStatus').mockResolvedValue(onboarding);
    vi.spyOn(portalApi, 'conversations').mockResolvedValue({ items: [], next_cursor: null });
    const submit = vi.spyOn(portalApi, 'submitTask').mockResolvedValue(task);
    const user = userEvent.setup();
    renderPage(`/assistant?host_id=${host.id}&application_id=wechat`);
    expect(await screen.findByRole('textbox', { name: '要完成的任务' })).toBeInTheDocument();
    expect(screen.queryByText(/目标应用|微信/)).not.toBeInTheDocument();
    await user.type(screen.getByRole('textbox', { name: '要完成的任务' }), '查找联系人');
    await user.click(screen.getByRole('button', { name: /交办任务/ }));
    await waitFor(() => expect(submit).toHaveBeenCalledWith(
      { host_id: host.id, model_id: model.id, objective: '查找联系人' }, expect.any(String),
    ));
  });

  it('does not carry legacy third-party app targets into follow-up messages', async () => {
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [host] });
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [model] });
    vi.spyOn(portalApi, 'preferences').mockResolvedValue(preferences);
    vi.spyOn(portalApi, 'onboardingStatus').mockResolvedValue(onboarding);
    vi.spyOn(portalApi, 'conversations').mockResolvedValue({ items: [conversation], next_cursor: null });
    vi.spyOn(portalApi, 'conversationMessages').mockResolvedValue({ items: [
      { id: 'm1234567', conversation_id: conversation.id, role: 'user', content: '查找联系人', task_id: task.id, created_at: timestamp },
    ], next_cursor: null });
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, application_id: 'wechat', status: 'running' });
    const append = vi.spyOn(portalApi, 'appendConversationMessage').mockResolvedValue({
      message: { id: 'm2345678', conversation_id: conversation.id, role: 'user', content: '继续查找', task_id: task.id, created_at: timestamp },
      task: { ...task, application_id: 'wechat', status: 'running' },
    });
    const user = userEvent.setup();
    renderPage();
    await user.click(await screen.findByRole('button', { name: /已有会话/ }));
    expect(await screen.findByRole('heading', { name: '已有会话' })).toBeInTheDocument();
    expect(screen.queryByText(/目标应用|微信/)).not.toBeInTheDocument();
    await user.type(screen.getByRole('textbox', { name: '要完成的任务' }), '继续查找');
    await user.click(screen.getByRole('button', { name: /交办任务/ }));
    await waitFor(() => expect(append).toHaveBeenCalledWith(conversation.id,
      { host_id: host.id, model_id: model.id, content: '继续查找' }, expect.any(String)));
  });

  it('does not restore a third-party target when continuing a completed conversation', async () => {
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [host] });
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [model] });
    vi.spyOn(portalApi, 'preferences').mockResolvedValue(preferences);
    vi.spyOn(portalApi, 'onboardingStatus').mockResolvedValue(onboarding);
    vi.spyOn(portalApi, 'conversations').mockResolvedValue({ items: [conversation], next_cursor: null });
    vi.spyOn(portalApi, 'conversationMessages').mockResolvedValue({ items: [
      { id: 'm1234567', conversation_id: conversation.id, role: 'user', content: '查找联系人', task_id: task.id, created_at: timestamp },
    ], next_cursor: null });
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, application_id: 'wechat', status: 'completed' });
    const append = vi.spyOn(portalApi, 'appendConversationMessage').mockResolvedValue({
      message: { id: 'm2345678', conversation_id: conversation.id, role: 'user', content: '继续查找', task_id: 'q2345678', created_at: timestamp },
      task: { ...task, id: 'q2345678', application_id: 'wechat', status: 'queued' },
    });
    const user = userEvent.setup();
    renderPage();
    await user.click(await screen.findByRole('button', { name: /已有会话/ }));
    expect(await screen.findByRole('heading', { name: '已有会话' })).toBeInTheDocument();
    expect(screen.queryByText(/目标应用|微信/)).not.toBeInTheDocument();
    await user.type(screen.getByRole('textbox', { name: '要完成的任务' }), '继续查找');
    await user.click(screen.getByRole('button', { name: /交办任务/ }));
    await waitFor(() => expect(append).toHaveBeenCalledWith(conversation.id,
      { host_id: host.id, model_id: model.id, content: '继续查找' }, expect.any(String)));
  });

  it('prevents changing conversations while a task submission is pending', async () => {
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [host] });
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [model] });
    vi.spyOn(portalApi, 'preferences').mockResolvedValue(preferences);
    vi.spyOn(portalApi, 'onboardingStatus').mockResolvedValue(onboarding);
    vi.spyOn(portalApi, 'conversations').mockResolvedValue({ items: [conversation, otherConversation], next_cursor: null });
    vi.spyOn(portalApi, 'conversationMessages').mockResolvedValue({ items: [], next_cursor: null });
    let resolveAppend!: (result: Awaited<ReturnType<typeof portalApi.appendConversationMessage>>) => void;
    const append = vi.spyOn(portalApi, 'appendConversationMessage').mockReturnValue(new Promise((resolve) => { resolveAppend = resolve; }));
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByRole('button', { name: /已有会话/ }));
    await user.type(screen.getByRole('textbox', { name: '要完成的任务' }), '继续检查');
    await user.click(screen.getByRole('button', { name: /交办任务/ }));

    const newTask = screen.getByRole('button', { name: /新任务/ });
    const otherSession = screen.getByRole('button', { name: /另一个会话/ });
    expect(newTask).toBeDisabled();
    expect(otherSession).toBeDisabled();
    await user.click(newTask);
    await user.click(otherSession);
    expect(screen.getByRole('heading', { name: '已有会话' })).toBeInTheDocument();

    await act(async () => resolveAppend({
      message: { id: 'm1234567', conversation_id: conversation.id, role: 'user', content: '继续检查', task_id: task.id, created_at: timestamp }, task,
    }));
    await screen.findByText('任务已接收，可在此查看进度。');
    expect(append).toHaveBeenCalledTimes(1);
    expect(newTask).toBeEnabled();
  });

  it('appends a follow-up to the selected conversation and keeps its host fixed', async () => {
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [host] });
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [model] });
    vi.spyOn(portalApi, 'preferences').mockResolvedValue(preferences);
    vi.spyOn(portalApi, 'onboardingStatus').mockResolvedValue(onboarding);
    vi.spyOn(portalApi, 'conversations').mockResolvedValue({ items: [conversation], next_cursor: null });
    vi.spyOn(portalApi, 'conversationMessages').mockResolvedValue({ items: [], next_cursor: null });
    vi.spyOn(portalApi, 'task').mockResolvedValue(task);
    const append = vi.spyOn(portalApi, 'appendConversationMessage').mockResolvedValue({
      message: { id: 'm1234567', conversation_id: conversation.id, role: 'user', content: '再检查一次', task_id: task.id, created_at: timestamp }, task,
    });
    const user = userEvent.setup();
    renderPage();
    await user.click(await screen.findByRole('button', { name: /已有会话/ }));
    await user.type(screen.getByRole('textbox', { name: '要完成的任务' }), '再检查一次');
    await user.click(screen.getByRole('button', { name: /交办任务/ }));

    await waitFor(() => expect(append).toHaveBeenCalledWith(
      conversation.id, { host_id: host.id, model_id: model.id, content: '再检查一次' }, expect.stringMatching(/^[A-Za-z0-9._~-]{8,128}$/),
    ));
    expect(screen.getByRole('combobox', { name: '选择云端电脑' })).toBeDisabled();
  });

  it('reuses the same idempotency key when an accepted message loses its response', async () => {
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [host] });
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [model] });
    vi.spyOn(portalApi, 'preferences').mockResolvedValue(preferences);
    vi.spyOn(portalApi, 'onboardingStatus').mockResolvedValue(onboarding);
    vi.spyOn(portalApi, 'conversations').mockResolvedValue({ items: [conversation], next_cursor: null });
    vi.spyOn(portalApi, 'conversationMessages').mockResolvedValue({ items: [], next_cursor: null });
    const response = {
      message: { id: 'm1234567', conversation_id: conversation.id, role: 'user', content: '继续检查', task_id: task.id, created_at: timestamp },
      task,
    } satisfies Awaited<ReturnType<typeof portalApi.appendConversationMessage>>;
    const acceptedByKey = new Map<string, typeof response>();
    let attempts = 0;
    const append = vi.spyOn(portalApi, 'appendConversationMessage').mockImplementation(async (_id, _body, key) => {
      const idempotencyKey = key!;
      if (!acceptedByKey.has(idempotencyKey)) acceptedByKey.set(idempotencyKey, response);
      if (attempts++ === 0) throw new TypeError('服务器已接收请求，但网络响应丢失');
      return acceptedByKey.get(idempotencyKey)!;
    });
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByRole('button', { name: /已有会话/ }));
    const prompt = screen.getByRole('textbox', { name: '要完成的任务' });
    await user.type(prompt, '继续检查');
    await user.click(screen.getByRole('button', { name: /交办任务/ }));
    await screen.findByText('服务器已接收请求，但网络响应丢失');
    await user.click(screen.getByRole('button', { name: /交办任务/ }));

    await screen.findByText('任务已接收，可在此查看进度。');
    expect(append).toHaveBeenCalledTimes(2);
    expect(append.mock.calls[1]?.[2]).toBe(append.mock.calls[0]?.[2]);
    expect(acceptedByKey).toHaveProperty('size', 1);
  });

  it('uses a new idempotency key when the message text changes after a failed send', async () => {
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [host] });
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [model] });
    vi.spyOn(portalApi, 'preferences').mockResolvedValue(preferences);
    vi.spyOn(portalApi, 'onboardingStatus').mockResolvedValue(onboarding);
    vi.spyOn(portalApi, 'conversations').mockResolvedValue({ items: [conversation], next_cursor: null });
    vi.spyOn(portalApi, 'conversationMessages').mockResolvedValue({ items: [], next_cursor: null });
    const response = {
      message: { id: 'm1234567', conversation_id: conversation.id, role: 'user', content: '检查测试', task_id: task.id, created_at: timestamp },
      task,
    } satisfies Awaited<ReturnType<typeof portalApi.appendConversationMessage>>;
    const acceptedByKey = new Map<string, typeof response>();
    let attempts = 0;
    const append = vi.spyOn(portalApi, 'appendConversationMessage').mockImplementation(async (_id, body, key) => {
      const idempotencyKey = key!;
      const accepted = acceptedByKey.get(idempotencyKey) ?? { ...response, message: { ...response.message, content: body.content } };
      acceptedByKey.set(idempotencyKey, accepted);
      if (attempts++ === 0) throw new TypeError('网络响应丢失');
      return accepted;
    });
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByRole('button', { name: /已有会话/ }));
    const prompt = screen.getByRole('textbox', { name: '要完成的任务' });
    await user.type(prompt, '检查旧内容');
    await user.click(screen.getByRole('button', { name: /交办任务/ }));
    await screen.findByText('网络响应丢失');
    await user.clear(prompt);
    await user.type(prompt, '检查新内容');
    await user.click(screen.getByRole('button', { name: /交办任务/ }));

    await screen.findByText('任务已接收，可在此查看进度。');
    expect(append).toHaveBeenCalledTimes(2);
    expect(append.mock.calls[1]?.[2]).not.toBe(append.mock.calls[0]?.[2]);
    expect(acceptedByKey).toHaveProperty('size', 2);
  });

  it('renders one task activity when multiple user messages share the active task', async () => {
    vi.stubGlobal('EventSource', TestEventSource);
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [host] });
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [model] });
    vi.spyOn(portalApi, 'preferences').mockResolvedValue(preferences);
    vi.spyOn(portalApi, 'onboardingStatus').mockResolvedValue(onboarding);
    vi.spyOn(portalApi, 'conversations').mockResolvedValue({ items: [conversation], next_cursor: null });
    vi.spyOn(portalApi, 'conversationMessages').mockResolvedValue({
      items: [
        { id: 'm1234567', conversation_id: conversation.id, role: 'user', content: '先检查项目', task_id: task.id, created_at: timestamp },
        { id: 'm7654321', conversation_id: conversation.id, role: 'user', content: '再补充检查依赖', task_id: task.id, created_at: timestamp },
      ],
      next_cursor: null,
    });
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, status: 'running' });
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByRole('button', { name: /已有会话/ }));
    await screen.findByText('再补充检查依赖');
    await waitFor(() => expect(TestEventSource.instances).toHaveLength(1));
    expect(screen.getAllByText('云端任务')).toHaveLength(1);

    await act(async () => TestEventSource.instances[0]!.emit('task.message', JSON.stringify({
      payload: { role: 'assistant', message_id: 'm8877665', turn_id: 's7654321', delta: '共享回复片段' },
    })));
    expect(screen.getAllByText('共享回复片段')).toHaveLength(1);
  });

  it('streams a new assistant turn when the active task already has an older persisted reply', async () => {
    vi.stubGlobal('EventSource', TestEventSource);
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [host] });
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [model] });
    vi.spyOn(portalApi, 'preferences').mockResolvedValue(preferences);
    vi.spyOn(portalApi, 'onboardingStatus').mockResolvedValue(onboarding);
    vi.spyOn(portalApi, 'conversations').mockResolvedValue({ items: [conversation], next_cursor: null });
    vi.spyOn(portalApi, 'conversationMessages').mockResolvedValue({ items: [
      { id: 'm1234567', conversation_id: conversation.id, role: 'user', content: '先检查项目', task_id: task.id, created_at: timestamp },
      { id: 'm2345678', conversation_id: conversation.id, role: 'assistant', content: '第一轮回复', task_id: task.id, created_at: timestamp },
      { id: 'm7654321', conversation_id: conversation.id, role: 'user', content: '再补充检查依赖', task_id: task.id, created_at: timestamp },
    ], next_cursor: null });
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, status: 'running' });
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByRole('button', { name: /已有会话/ }));
    await screen.findByText('再补充检查依赖');
    await waitFor(() => expect(TestEventSource.instances).toHaveLength(1));

    await act(async () => TestEventSource.instances[0]!.emit('task.message', JSON.stringify({
      payload: { role: 'assistant', message_id: 'm8877665', turn_id: 's7654321', delta: '第二轮实时回复' },
    })));
    expect(screen.getByText('第二轮实时回复')).toBeInTheDocument();
  });

  it('loads older messages above the current page without duplicating the active task stream', async () => {
    vi.stubGlobal('EventSource', TestEventSource);
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [host] });
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [model] });
    vi.spyOn(portalApi, 'preferences').mockResolvedValue(preferences);
    vi.spyOn(portalApi, 'onboardingStatus').mockResolvedValue(onboarding);
    vi.spyOn(portalApi, 'conversations').mockResolvedValue({ items: [conversation], next_cursor: null });
    const conversationMessages = vi.spyOn(portalApi, 'conversationMessages')
      .mockResolvedValueOnce({
        items: [{ id: 'm7654321', conversation_id: conversation.id, role: 'user', content: '最近的消息', task_id: task.id, created_at: timestamp }],
        next_cursor: 'older-messages-cursor',
      })
      .mockResolvedValueOnce({
        items: [{ id: 'm1234567', conversation_id: conversation.id, role: 'user', content: '更早的消息', task_id: task.id, created_at: '2026-10-22T07:00:00.000Z' }],
        next_cursor: null,
      });
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, status: 'running' });
    const user = userEvent.setup();
    const { container } = renderPage();

    await user.click(await screen.findByRole('button', { name: /已有会话/ }));
    await screen.findByText('最近的消息');
    await waitFor(() => expect(TestEventSource.instances).toHaveLength(1));
    await user.click(await screen.findByRole('button', { name: '加载更早消息' }));
    await screen.findByText('更早的消息');

    const messageBlocks = Array.from(container.querySelectorAll('.message-block'));
    expect(messageBlocks).toHaveLength(2);
    expect(messageBlocks[0]).toHaveTextContent('更早的消息');
    expect(messageBlocks[1]).toHaveTextContent('最近的消息');
    expect(conversationMessages).toHaveBeenNthCalledWith(2, conversation.id, 'older-messages-cursor');
    expect(TestEventSource.instances).toHaveLength(1);
    expect(screen.getAllByText('云端任务')).toHaveLength(1);
  });

  it('keeps the visible history and retries after loading an older page fails', async () => {
    vi.stubGlobal('EventSource', TestEventSource);
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [host] });
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [model] });
    vi.spyOn(portalApi, 'preferences').mockResolvedValue(preferences);
    vi.spyOn(portalApi, 'onboardingStatus').mockResolvedValue(onboarding);
    vi.spyOn(portalApi, 'conversations').mockResolvedValue({ items: [conversation], next_cursor: null });
    vi.spyOn(portalApi, 'conversationMessages')
      .mockResolvedValueOnce({
        items: [{ id: 'm7654321', conversation_id: conversation.id, role: 'user', content: '最近的消息', task_id: task.id, created_at: timestamp }],
        next_cursor: 'older-messages-cursor',
      })
      .mockRejectedValueOnce(new Error('读取更早消息失败'))
      .mockResolvedValueOnce({
        items: [{ id: 'm1234567', conversation_id: conversation.id, role: 'user', content: '更早的消息', task_id: task.id, created_at: '2026-10-22T07:00:00.000Z' }],
        next_cursor: null,
      });
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, status: 'running' });
    const user = userEvent.setup();
    const { container } = renderPage();

    await user.click(await screen.findByRole('button', { name: /已有会话/ }));
    await screen.findByText('最近的消息');
    await waitFor(() => expect(TestEventSource.instances).toHaveLength(1));
    await user.click(screen.getByRole('button', { name: '加载更早消息' }));

    expect(await screen.findByRole('alert')).toHaveTextContent('读取更早消息失败');
    expect(screen.getByText('最近的消息')).toBeInTheDocument();
    expect(screen.queryByText('更早的消息')).not.toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: '加载更早消息' }));
    await screen.findByText('更早的消息');
    const messageBlocks = Array.from(container.querySelectorAll('.message-block'));
    expect(messageBlocks[0]).toHaveTextContent('更早的消息');
    expect(messageBlocks[1]).toHaveTextContent('最近的消息');
    expect(TestEventSource.instances).toHaveLength(1);
  });

  it('streams assistant deltas and refreshes the conversation only after completion', async () => {
    vi.stubGlobal('EventSource', TestEventSource);
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [host] });
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [model] });
    vi.spyOn(portalApi, 'preferences').mockResolvedValue(preferences);
    vi.spyOn(portalApi, 'onboardingStatus').mockResolvedValue(onboarding);
    vi.spyOn(portalApi, 'conversations').mockResolvedValue({ items: [conversation], next_cursor: null });
    const conversationMessages = vi.spyOn(portalApi, 'conversationMessages').mockResolvedValue({
      items: [{ id: 'm1234567', conversation_id: conversation.id, role: 'user', content: '开始任务', task_id: task.id, created_at: timestamp }],
      next_cursor: null,
    });
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, status: 'running' });
    const user = userEvent.setup();
    renderPage();
    await user.click(await screen.findByRole('button', { name: /已有会话/ }));
    await screen.findByText('开始任务');
    await waitFor(() => expect(TestEventSource.instances).toHaveLength(1));
    const [stream] = TestEventSource.instances;
    const initialCallCount = conversationMessages.mock.calls.length;

    await act(async () => stream!.emit('task.message', JSON.stringify({ payload: { role: 'assistant', message_id: 'm7654321', turn_id: 's7654321', delta: '部分回复' } })));
    expect(conversationMessages).toHaveBeenCalledTimes(initialCallCount);
    expect(screen.getByText('部分回复')).toBeInTheDocument();
    await act(async () => stream!.emit('task.message', JSON.stringify({ payload: { role: 'assistant', message_id: 'm7654321', turn_id: 's7654321', delta: '继续' } })));
    expect(screen.getByText('部分回复继续')).toBeInTheDocument();

    await act(async () => stream!.emit('task.message', JSON.stringify({ payload: { role: 'assistant', message_id: 'm7654321', turn_id: 's7654321', content: '完整回复', done: true } })));
    await waitFor(() => expect(conversationMessages).toHaveBeenCalledTimes(initialCallCount + 1));
    expect(screen.getByText('完整回复')).toBeInTheDocument();
    expect(TestEventSource.instances).toHaveLength(1);
  });

  it('reloads persisted history after an expired event cursor and hides incomplete deltas', async () => {
    vi.stubGlobal('EventSource', TestEventSource);
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [host] });
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [model] });
    vi.spyOn(portalApi, 'preferences').mockResolvedValue(preferences);
    vi.spyOn(portalApi, 'onboardingStatus').mockResolvedValue(onboarding);
    vi.spyOn(portalApi, 'conversations').mockResolvedValue({ items: [conversation], next_cursor: null });
    const userMessage = { id: 'm1234567', conversation_id: conversation.id, role: 'user', content: '开始任务', task_id: task.id, created_at: timestamp } as const;
    const assistantMessage = { id: 'm7654321', conversation_id: conversation.id, role: 'assistant', content: '完整回复', task_id: task.id, created_at: timestamp } as const;
    const conversationMessages = vi.spyOn(portalApi, 'conversationMessages')
      .mockResolvedValueOnce({ items: [userMessage], next_cursor: null })
      .mockResolvedValueOnce({ items: [userMessage], next_cursor: null })
      .mockResolvedValue({ items: [userMessage, assistantMessage], next_cursor: null });
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, status: 'running' });
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByRole('button', { name: /已有会话/ }));
    await screen.findByText('开始任务');
    await waitFor(() => expect(TestEventSource.instances).toHaveLength(1));
    const [stream] = TestEventSource.instances;

    await act(async () => stream!.emit('task.message', JSON.stringify({ payload: { role: 'assistant', message_id: assistantMessage.id, turn_id: 's7654321', delta: '游标之前的片段' } })));
    expect(screen.getByText('游标之前的片段')).toBeInTheDocument();

    await act(async () => stream!.emit('task.snapshot_required', JSON.stringify({ sequence: 9 })));
    await screen.findByText('任务事件不完整，已重新读取保存的状态；完整回复保存后会显示。');
    await waitFor(() => expect(conversationMessages).toHaveBeenCalledTimes(2));
    expect(screen.queryByText('游标之前的片段')).not.toBeInTheDocument();

    await act(async () => stream!.emit('task.message', JSON.stringify({ payload: { role: 'assistant', message_id: assistantMessage.id, turn_id: 's7654321', delta: '游标之后的片段' } })));
    expect(screen.queryByText('游标之后的片段')).not.toBeInTheDocument();
    await act(async () => stream!.emit('task.message', JSON.stringify({ payload: { role: 'assistant', message_id: assistantMessage.id, turn_id: 's7654321', content: assistantMessage.content, done: true } })));

    expect(await screen.findByText('完整回复')).toBeInTheDocument();
    expect(screen.queryByText('游标之前的片段')).not.toBeInTheDocument();
    expect(screen.queryByText('游标之后的片段')).not.toBeInTheDocument();
    expect(conversationMessages).toHaveBeenCalledTimes(3);
  });

  it('replaces the streamed response when the persisted assistant message arrives', async () => {
    vi.stubGlobal('EventSource', TestEventSource);
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [host] });
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [model] });
    vi.spyOn(portalApi, 'preferences').mockResolvedValue(preferences);
    vi.spyOn(portalApi, 'onboardingStatus').mockResolvedValue(onboarding);
    vi.spyOn(portalApi, 'conversations').mockResolvedValue({ items: [conversation], next_cursor: null });
    const userMessage = { id: 'm1234567', conversation_id: conversation.id, role: 'user', content: '开始任务', task_id: task.id, created_at: timestamp } as const;
    const assistantMessage = { id: 'm7654321', conversation_id: conversation.id, role: 'assistant', content: '最终正文', task_id: task.id, created_at: timestamp } as const;
    const conversationMessages = vi.spyOn(portalApi, 'conversationMessages')
      .mockResolvedValueOnce({ items: [userMessage], next_cursor: null })
      .mockResolvedValue({ items: [userMessage, assistantMessage], next_cursor: null });
    vi.spyOn(portalApi, 'task').mockResolvedValue({ ...task, status: 'running' });
    const user = userEvent.setup();
    renderPage();
    await user.click(await screen.findByRole('button', { name: /已有会话/ }));
    await screen.findByText('开始任务');
    await waitFor(() => expect(TestEventSource.instances).toHaveLength(1));
    const [stream] = TestEventSource.instances;

    await act(async () => stream!.emit('task.message', JSON.stringify({ payload: { role: 'assistant', message_id: assistantMessage.id, turn_id: 's7654321', delta: '临时回复' } })));
    expect(screen.getByText('临时回复')).toBeInTheDocument();
    await act(async () => stream!.emit('task.message', JSON.stringify({ payload: { role: 'assistant', message_id: assistantMessage.id, turn_id: 's7654321', content: assistantMessage.content, done: true } })));

    await waitFor(() => expect(conversationMessages).toHaveBeenCalledTimes(2));
    expect(await screen.findByText('最终正文')).toBeInTheDocument();
    expect(screen.queryByText('临时回复')).not.toBeInTheDocument();
    expect(screen.getAllByText('最终正文')).toHaveLength(1);
    expect(TestEventSource.instances).toHaveLength(1);
  });
});
