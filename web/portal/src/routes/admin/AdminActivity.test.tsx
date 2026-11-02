import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { portalApi } from '../../app/api';
import { AdminActivity } from './AdminActivity';

type ActivityData = Awaited<ReturnType<typeof portalApi.adminActivity>>;

const activityData: ActivityData = {
  days: 7,
  from: '2026-10-27',
  to: '2026-11-02',
  by_day: [
    { date: '2026-10-27', hub_operations: 4, task_events: 3, audit_events: 2 },
    { date: '2026-10-28', hub_operations: 2, task_events: 1, audit_events: 1 },
  ],
  hub_operations: [{ action: 'files', status: 'succeeded', count: 6 }],
  task_events: [{ type: 'created', count: 4 }],
  audit_events: [{ action: 'admin.user_update', count: 3 }],
};

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><AdminActivity /></QueryClientProvider>);
}

describe('admin activity', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  it('shows aggregate totals, daily counts and event categories without raw content', async () => {
    const request = vi.spyOn(portalApi, 'adminActivity').mockResolvedValue(activityData);
    renderPage();

    expect(await screen.findByRole('heading', { name: '活动' })).toBeInTheDocument();
    expect(screen.getByText('统计区间：2026-10-27 至 2026-11-02')).toBeInTheDocument();
    const facts = screen.getByRole('heading', { name: '活动概览' }).closest('section')!.querySelector<HTMLElement>('.admin-detail-facts')!;
    expect(within(facts).getByText('Hub 操作').closest('article')).toHaveTextContent('6');
    expect(within(facts).getByText('Task 事件').closest('article')).toHaveTextContent('4');
    expect(within(facts).getByText('审计事件').closest('article')).toHaveTextContent('3');
    expect(screen.getByRole('cell', { name: 'files' }).closest('tr')).toHaveTextContent('succeeded');
    expect(screen.getByRole('cell', { name: 'created' }).closest('tr')).toHaveTextContent('4');
    expect(screen.getByRole('cell', { name: 'admin.user_update' }).closest('tr')).toHaveTextContent('3');
    expect(screen.queryByText(/user_id|installation_id|address|hash|secret/i)).not.toBeInTheDocument();
    expect(request).toHaveBeenCalledWith(7);
  });

  it('uses the selected period and shows empty states for absent aggregates', async () => {
    const user = userEvent.setup();
    const request = vi.spyOn(portalApi, 'adminActivity').mockImplementation(async (days) => ({
      ...activityData, days, from: '2026-10-04', to: '2026-11-02', by_day: [], hub_operations: [], task_events: [], audit_events: [],
    }));
    renderPage();

    expect(await screen.findByText('所选区间没有每日活动统计。')).toBeInTheDocument();
    expect(screen.getByText('暂无 Hub 操作统计。')).toBeInTheDocument();
    expect(screen.getByText('暂无 Task 事件统计。')).toBeInTheDocument();
    expect(screen.getByText('暂无审计事件统计。')).toBeInTheDocument();
    await user.selectOptions(screen.getByLabelText('统计区间'), '30');
    expect(await screen.findByText('统计区间：2026-10-04 至 2026-11-02')).toBeInTheDocument();
    expect(request).toHaveBeenCalledWith(30);
  });

  it('shows loading and retries a failed activity request', async () => {
    const user = userEvent.setup();
    let resolveRequest!: (value: ActivityData) => void;
    const request = vi.spyOn(portalApi, 'adminActivity')
      .mockRejectedValueOnce(new Error('活动统计暂不可用'))
      .mockImplementationOnce(() => new Promise((resolve) => { resolveRequest = resolve; }));
    renderPage();

    expect(await screen.findByRole('alert')).toHaveTextContent('活动统计暂不可用');
    await user.click(screen.getByRole('button', { name: '重试' }));
    resolveRequest(activityData);
    expect(await screen.findByRole('cell', { name: 'files' })).toBeInTheDocument();
    expect(request).toHaveBeenCalledTimes(2);
  });
});
