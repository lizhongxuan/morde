import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { portalApi } from '../app/api';
import type { components } from '../api/schema';
import { ToastNoticeCenter, ToastProvider } from '../app/toast';
import { Reviews } from './Reviews';

const timestamp = '2026-11-15T08:00:00.000Z';
const disclosures: components['schemas']['TaskApprovalDisclosures'] = {
  risk: { state: 'known', summary: '仅读取已授权目录' },
  this_cost: { state: 'not_applicable', basis: null, amount_micros: null, currency: null },
  cumulative_cost: { state: 'unknown', basis: null, amount_micros: null, currency: null },
  alternatives: { state: 'known', options: ['用户可稍后处理'] },
  revocation: { state: 'known', summary: '可在云端电脑设置中撤销授权' },
};

function item(id: string, status: components['schemas']['TaskApprovalSnapshot']['status']): components['schemas']['TaskApprovalItem'] {
  return {
    task_id: 'q1234567', conversation_id: 'c1234567', project_id: null, host_id: 'h1234567',
    task_status: status === 'pending' ? 'waiting_user' : 'queued', task_event_sequence: 8, task_updated_at: timestamp,
    approval: {
      id, version: 3, status, decision: status === 'approved' ? 'approve' : status === 'rejected' ? 'reject' : '', kind: 'approval',
      title: `审批 ${id}`, object: '工作资料', content: '读取授权目录中的内容。', impact: '会把文件内容用于当前任务。',
      disclosures, expires_at: null, responded_at: status === 'pending' ? null : timestamp, created_at: timestamp,
    },
  };
}

function renderReviews() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}><ToastProvider><ToastNoticeCenter />
    <MemoryRouter initialEntries={['/reviews']}><Reviews /></MemoryRouter>
  </ToastProvider></QueryClientProvider>);
}

describe('Reviews', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  it('groups pending and handled cards, shows the full pending count, and submits the snapshot version', async () => {
    vi.spyOn(portalApi, 'taskApprovals').mockResolvedValue({ items: [item('k1234567', 'pending'), item('k7654321', 'approved')],
      next_cursor: null, pending_count: 5 });
    const respond = vi.spyOn(portalApi, 'respondToTaskApproval').mockResolvedValue({
      task_id: 'q1234567', action: 'respond', status: 'queued', unsettled_operation_ids: [],
    });
    const user = userEvent.setup();
    renderReviews();

    expect(await screen.findByRole('heading', { name: '待处理 · 5' })).toBeInTheDocument();
    const pendingSection = screen.getByRole('region', { name: '待处理审核' });
    const card = within(pendingSection).getByRole('region', { name: '需要批准的操作' });
    expect(card).toHaveTextContent('仅读取已授权目录');
    expect(card).toHaveTextContent('用户可稍后处理');
    expect(screen.getByRole('region', { name: '已处理审核' })).toHaveTextContent('已允许');
    await user.click(within(card).getByRole('button', { name: '允许' }));
    await waitFor(() => expect(respond).toHaveBeenCalledWith('q1234567', 'k1234567',
      { version: 3, decision: 'approve' }, expect.any(String)));
    expect(await screen.findByText('审核已受理，任务后续状态等待更新。')).toBeInTheDocument();
  });

  it('does not show an empty or zero state when the approval list fails', async () => {
    vi.spyOn(portalApi, 'taskApprovals').mockRejectedValue(new Error('offline'));
    renderReviews();

    expect(await screen.findByRole('alert')).toHaveTextContent('审核事项暂不可用，待处理数量和结果均未确认。');
    expect(screen.queryByText('当前没有待处理审核。')).not.toBeInTheDocument();
    expect(screen.queryByText('0 项待处理')).not.toBeInTheDocument();
  });

  it('loads later pages and reports a repeated cursor instead of looping', async () => {
    const list = vi.spyOn(portalApi, 'taskApprovals')
      .mockResolvedValueOnce({ items: [item('k1234567', 'pending')], next_cursor: 'same-cursor', pending_count: 1 })
      .mockResolvedValueOnce({ items: [item('k7654321', 'rejected')], next_cursor: 'same-cursor', pending_count: 1 });
    const user = userEvent.setup();
    renderReviews();

    await user.click(await screen.findByRole('button', { name: '加载更多审核事项' }));
    expect(await screen.findByRole('region', { name: '已处理的审批' })).toHaveTextContent('审批 k7654321');
    expect(screen.getByRole('alert')).toHaveTextContent('审核列表分页游标重复，尚未加载的事项状态未确认。');
    expect(list).toHaveBeenCalledTimes(2);
  });
});
