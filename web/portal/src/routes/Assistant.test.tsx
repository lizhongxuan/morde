import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { portalApi } from '../app/api';
import { ToastProvider } from '../app/toast';
import { Assistant } from './Assistant';

const timestamp = '2026-10-22T08:00:00.000Z';
type ConversationMessage = Awaited<ReturnType<typeof portalApi.conversationMessages>>['items'][number];
const model = {
  id: 'p1234567', name: '团队模型', base_url: 'https://models.example.test/v1', model_name: 'morde-test', shared: true, has_api_key: true,
  capabilities: { text: true, tools: false, vision: false }, checked_at: timestamp, created_at: timestamp, updated_at: timestamp,
} satisfies Awaited<ReturnType<typeof portalApi.modelConnections>>['items'][number];
const otherModel = { ...model, id: 'p7654321', name: '备用模型' };
const conversation = { id: 'c1234567', title: '你好', created_at: timestamp, updated_at: timestamp };
const message = { id: 'm1234567', conversation_id: conversation.id, role: 'user' as const, content: '你好', task_id: 'q1234567', created_at: timestamp };
const reply = { id: 'm7654321', conversation_id: conversation.id, role: 'assistant' as const, content: '你好！', task_id: 'q1234567', created_at: timestamp };
const task = {
  id: 'q1234567', owner_id: 'u1234567', execution_location: 'server' as const, conversation_id: conversation.id,
  model_id: model.id, objective: message.content, status: 'completed' as const, current_step: null,
  budget: { active_seconds: 600, tool_calls: 10, model_tokens: 1000 }, usage: { active_seconds: 2, tool_calls: 0, model_tokens: 12 },
  created_at: timestamp, updated_at: timestamp, result: reply.content, waiting_reason: null, steps: [], batch_items: [],
  artifacts: [], pending_request: null, clarification_request: null, stop_request: null, finished_at: timestamp,
} satisfies Awaited<ReturnType<typeof portalApi.task>>;

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Assistant /></MemoryRouter></ToastProvider></QueryClientProvider>);
}

function mockCommon(options: { conversations?: typeof conversation[]; messages?: ConversationMessage[]; cloudTasks?: boolean } = {}) {
  vi.spyOn(portalApi, 'capabilities').mockResolvedValue({
    service_instance_id: '00000000-0000-4000-8000-000000000001', api_version: '1', hub_version: null,
    event_version: null, minimum_client_version: null, supported_features: options.cloudTasks === false ? [] : ['cloud_tasks'],
    default_max_request_bytes: 1048576,
  });
  vi.spyOn(portalApi, 'conversations').mockResolvedValue({ items: options.conversations ?? [], next_cursor: null });
  vi.spyOn(portalApi, 'conversationMessages').mockResolvedValue({ items: options.messages ?? [], next_cursor: null });
  vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [model, otherModel] });
  vi.spyOn(portalApi, 'preferences').mockResolvedValue({ assistant_name: 'Morde', selected_model_id: model.id, default_host_id: null, updated_at: timestamp });
}

describe('Assistant durable tasks', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

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
    await waitFor(() => expect(respond).toHaveBeenCalledWith(task.id, approval.id, { version: approval.version, decision }));
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
    }));
  });
});
