import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { ReactNode } from 'react';
import { MemoryRouter, Route, Routes } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { portalApi } from '../app/api';
import { ToastProvider } from '../app/toast';
import { TaskDetail } from './TaskDetail';
import { Tasks } from './Tasks';

const timestamp = '2026-10-22T08:00:00.000Z';
const task = {
  id: 'q1234567', owner_id: 'u1234567', host_id: 'h1234567', conversation_id: 'c1234567', model_id: 'test-model',
  objective: '整理项目改动', status: 'running', current_step: '读取文件', result: null, waiting_reason: null,
  budget: { active_seconds: 1800, tool_calls: 100, model_tokens: 50000 },
  usage: { active_seconds: 15, tool_calls: 2, model_tokens: null },
  steps: [{ id: 's1234567', sequence: 1, kind: 'tool', status: 'running', title: '读取文件', summary: '正在读取项目内容。', created_at: timestamp }],
  batch_items: [], artifacts: [], pending_request: null, clarification_request: null, stop_request: null,
  created_at: timestamp, updated_at: timestamp, finished_at: null,
} satisfies Awaited<ReturnType<typeof portalApi.task>>;

const approvalTask: Awaited<ReturnType<typeof portalApi.task>> = {
  ...task,
  id: 'qreview1',
  objective: '发送外部消息？',
  status: 'waiting_user',
  current_step: '等待审批',
  waiting_reason: 'approval_required',
  pending_request: { id: 'rapprove', version: 2, kind: 'approval', title: '发送外部消息？', object: '项目群', content: '部署已完成。', impact: '消息会发送到外部群聊。' },
};

const clarificationTask: Awaited<ReturnType<typeof portalApi.task>> = {
  ...task,
  id: 'qclarif1',
  objective: '生成本周项目进度报告',
  status: 'waiting_user',
  current_step: '等待报告范围',
  waiting_reason: 'clarification_required',
  steps: [],
  clarification_request: { id: 'rclarify', version: 3, question: '报告需要包含哪几天？是否要加入风险和下周计划？' },
};

function renderWithProviders(child: ReactNode, initialEntry = '/tasks') {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter initialEntries={[initialEntry]}>{child}</MemoryRouter></ToastProvider></QueryClientProvider>);
}

describe('Tasks', () => {
  beforeEach(() => {
    vi.spyOn(portalApi, 'executionControl').mockResolvedValue({ state: 'enabled', epoch: 1, pending_task_count: 0 });
    vi.spyOn(portalApi, 'schedules').mockResolvedValue({ items: [], next_cursor: null });
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'onboardingStatus').mockResolvedValue({ model_status: 'not_configured', models: [], selected_model_id: null, computer_status: 'not_created', computers: [], default_host_id: null, can_submit: false, next_action: 'configure_model' });
  });
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  it('shows task progress, waiting approvals, and links to task detail', async () => {
    vi.spyOn(portalApi, 'tasks').mockResolvedValue({ items: [
      { id: task.id, owner_id: task.owner_id, host_id: task.host_id, conversation_id: task.conversation_id, model_id: task.model_id, objective: task.objective, status: task.status, current_step: task.current_step, budget: task.budget, usage: task.usage, created_at: task.created_at, updated_at: task.updated_at },
      { id: approvalTask.id, owner_id: approvalTask.owner_id, host_id: approvalTask.host_id, conversation_id: approvalTask.conversation_id, model_id: approvalTask.model_id, objective: approvalTask.objective, status: approvalTask.status, current_step: approvalTask.current_step, budget: approvalTask.budget, usage: approvalTask.usage, created_at: approvalTask.created_at, updated_at: approvalTask.updated_at },
    ], next_cursor: null });

    renderWithProviders(<Tasks />);

    expect(await screen.findByRole('link', { name: '查看任务 整理项目改动' })).toHaveAttribute('href', `/tasks/${task.id}`);
    expect(screen.getByText('执行中')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: '发送外部消息？' })).toHaveAttribute('href', `/tasks/${approvalTask.id}`);
    expect(screen.getByText('等待你处理')).toBeInTheDocument();
  });

  it('keeps loaded tasks visible and retries only the failed next page', async () => {
    const firstTask = {
      id: task.id, owner_id: task.owner_id, host_id: task.host_id, conversation_id: task.conversation_id,
      model_id: task.model_id, objective: task.objective, status: task.status, current_step: task.current_step,
      budget: task.budget, usage: task.usage, created_at: task.created_at, updated_at: task.updated_at,
    };
    const secondTask = { ...firstTask, id: 'q7654321', objective: '第二页任务' };
    const list = vi.spyOn(portalApi, 'tasks')
      .mockResolvedValueOnce({ items: [firstTask], next_cursor: 'next-page' })
      .mockRejectedValueOnce(new Error('网络暂时不可用'))
      .mockResolvedValueOnce({ items: [secondTask], next_cursor: null });
    const user = userEvent.setup();
    renderWithProviders(<Tasks />);

    expect(await screen.findByRole('link', { name: firstTask.objective })).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '加载更多任务' }));

    expect(await screen.findByRole('alert')).toHaveTextContent('网络暂时不可用');
    expect(screen.getByRole('link', { name: firstTask.objective })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '重试' })).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '重试' }));

    expect(await screen.findByRole('link', { name: secondTask.objective })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: firstTask.objective })).toBeInTheDocument();
    expect(list).toHaveBeenNthCalledWith(2, 'next-page');
    expect(list).toHaveBeenNthCalledWith(3, 'next-page');
  });

  it('requires confirmation and shows the durable stopping state without claiming MCP tools were stopped', async () => {
    const status = vi.mocked(portalApi.executionControl);
    status.mockReset()
      .mockResolvedValueOnce({ state: 'enabled', epoch: 1, pending_task_count: 0 })
      .mockResolvedValueOnce({ state: 'stopping', epoch: 2, pending_task_count: 1 })
      .mockResolvedValue({ state: 'stopping', epoch: 2, pending_task_count: 1 });
    const stop = vi.spyOn(portalApi, 'stopAllExecution').mockResolvedValue({ state: 'stopping', epoch: 2, pending_task_count: 1 });
    const user = userEvent.setup();
    renderWithProviders(<Tasks />);

    await user.click(await screen.findByRole('button', { name: '停止所有 AI 执行' }));
    const dialog = screen.getByRole('dialog');
    expect(within(dialog).getByText(/MCP 电脑操作会请求运行端停止/)).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: '停止 AI 执行' }));

    await waitFor(() => expect(stop).toHaveBeenCalledWith(1));
    expect(await screen.findByText('停止中')).toBeInTheDocument();
    expect(screen.getByText(/剩余 1 个执行项/)).toBeInTheDocument();
  });

  it('enables only new tasks from a disabled gate', async () => {
    vi.mocked(portalApi.executionControl).mockReset()
      .mockResolvedValueOnce({ state: 'disabled', epoch: 3, pending_task_count: 0 })
      .mockResolvedValueOnce({ state: 'enabled', epoch: 4, pending_task_count: 0 })
      .mockResolvedValue({ state: 'enabled', epoch: 4, pending_task_count: 0 });
    const enable = vi.spyOn(portalApi, 'enableExecution').mockResolvedValue({ state: 'enabled', epoch: 4, pending_task_count: 0 });
    const user = userEvent.setup();
    renderWithProviders(<Tasks />);

    expect(await screen.findByText('重新开启只允许新任务运行，不会恢复已取消的任务。')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '重新开启' }));
    await waitFor(() => expect(enable).toHaveBeenCalledWith(3));
  });

  it('submits an exact approval decision separately from task controls', async () => {
    vi.spyOn(portalApi, 'task').mockResolvedValue(approvalTask);
    const sendAction = vi.spyOn(portalApi, 'taskAction').mockResolvedValue({ task_id: approvalTask.id, action: 'respond', status: 'queued', unsettled_operation_ids: [] });
    const user = userEvent.setup();
    renderWithProviders(<Routes><Route path="/tasks/:id" element={<TaskDetail />} /></Routes>, `/tasks/${approvalTask.id}`);
    await user.click(await screen.findByRole('button', { name: '拒绝' }));

    await waitFor(() => expect(sendAction).toHaveBeenCalledWith(approvalTask.id, {
      action: 'respond', pending_id: 'rapprove', pending_version: 2, decision: 'reject',
    }, expect.any(String)));
    expect(screen.getByRole('status')).toHaveTextContent('已提交审批决定。');
  });

  it('reuses the same idempotency key when a batch retry response is lost', async () => {
    const batchTask = {
      ...task,
      batch_items: [{ id: 'b1234567', key: 'item-a', status: 'failed', attempt: 1, result: null, error: '暂时失败' }],
    } satisfies Awaited<ReturnType<typeof portalApi.task>>;
    vi.spyOn(portalApi, 'task').mockResolvedValue(batchTask);
    const sendAction = vi.spyOn(portalApi, 'taskAction')
      .mockRejectedValueOnce(new Error('连接中断，响应未知'))
      .mockResolvedValue({ task_id: task.id, action: 'retry_batch_item', status: 'queued', unsettled_operation_ids: [] });
    const user = userEvent.setup();
    renderWithProviders(<Routes><Route path="/tasks/:id" element={<TaskDetail />} /></Routes>, `/tasks/${task.id}`);

    await user.click(await screen.findByRole('button', { name: '重试' }));
    await waitFor(() => expect(sendAction).toHaveBeenCalledTimes(1));
    await user.click(await screen.findByRole('button', { name: '重试' }));

    await waitFor(() => expect(sendAction).toHaveBeenCalledTimes(2));
    expect(sendAction.mock.calls[1]?.[2]).toBe(sendAction.mock.calls[0]?.[2]);
  });

  it('shows clarification separately from approval and sends the exact request version and answer', async () => {
    vi.spyOn(portalApi, 'task').mockResolvedValue(clarificationTask);
    const sendAction = vi.spyOn(portalApi, 'taskAction').mockResolvedValue({ task_id: clarificationTask.id, action: 'answer_clarification', status: 'queued', unsettled_operation_ids: [] });
    const user = userEvent.setup();
    renderWithProviders(<Routes><Route path="/tasks/:id" element={<TaskDetail />} /></Routes>, `/tasks/${clarificationTask.id}`);

    expect(await screen.findByText(clarificationTask.clarification_request!.question)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '批准并继续' })).not.toBeInTheDocument();
    await user.type(screen.getByLabelText('你的回答'), '包含周一到周五，附上风险和下周计划。');
    await user.click(screen.getByRole('button', { name: '提交回答' }));

    await waitFor(() => expect(sendAction).toHaveBeenCalledWith(clarificationTask.id, {
      action: 'answer_clarification', clarification_id: 'rclarify', clarification_version: 3,
      answer: '包含周一到周五，附上风险和下周计划。',
    }, expect.any(String)));
  });

  it('counts clarification answers by Unicode characters and disables overlong replies', async () => {
    vi.spyOn(portalApi, 'task').mockResolvedValue(clarificationTask);
    renderWithProviders(<Routes><Route path="/tasks/:id" element={<TaskDetail />} /></Routes>, `/tasks/${clarificationTask.id}`);

    const answer = await screen.findByLabelText('你的回答');
    fireEvent.change(answer, { target: { value: '😊'.repeat(8192) } });
    expect(screen.getByText('8,192 / 16,384')).toBeInTheDocument();
    fireEvent.change(answer, { target: { value: '中'.repeat(16385) } });
    expect(screen.getByText('16,385 / 16,384')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '提交回答' })).toBeDisabled();
  });

  it('requires explicit confirmation before cancelling and includes the current status for compare-and-set', async () => {
    vi.spyOn(portalApi, 'task').mockResolvedValue(task);
    const sendAction = vi.spyOn(portalApi, 'taskAction').mockResolvedValue({ task_id: task.id, action: 'cancel', status: 'cancelling', unsettled_operation_ids: [] });
    const user = userEvent.setup();
    renderWithProviders(<Routes><Route path="/tasks/:id" element={<TaskDetail />} /></Routes>, `/tasks/${task.id}`);
    await user.click(await screen.findByRole('button', { name: '取消任务' }));
    expect(screen.getByText(/外部副作用不会自动撤销/)).toBeInTheDocument();
    await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: '取消任务' }));
    await waitFor(() => expect(sendAction).toHaveBeenCalledWith(task.id, { action: 'cancel', expected_status: 'running' }, expect.any(String)));
  });
});
