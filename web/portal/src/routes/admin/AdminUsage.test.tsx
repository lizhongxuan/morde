import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { portalApi } from '../../app/api';
import { AdminUsage } from './AdminUsage';

type UsageData = Awaited<ReturnType<typeof portalApi.adminUsage>>;
type StatsData = Awaited<ReturnType<typeof portalApi.adminStats>>;

const usageData: UsageData = {
  days: 7,
  from: '2026-10-27',
  to: '2026-11-02',
  summary: { calls: 12, settled_calls: 9, unknown_calls: 3, input_tokens: 1500, output_tokens: 500, active_accounts: 4 },
  by_day: [{ date: '2026-11-02', calls: 5, input_tokens: 600, output_tokens: 200, active_accounts: 3 }],
  by_model: [{ model_id: 'model-a', calls: 12, input_tokens: 1500, output_tokens: 500 }],
};

const statsData: StatsData = {
  users: { total: 20, approved: 15, pending: 3, rejected: 0, disabled: 2 },
  platforms: [{ platform: 'ios', installations: 10, active_installations: 6, users: 8 }],
  cohorts: [{ week: '2026-W44', signups: 8, with_installation: 3 }],
  version_tracking_available: false,
};

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><AdminUsage /></QueryClientProvider>);
}

describe('admin usage and platform statistics', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  it('shows ledger, platform and registration-cohort values with their definitions', async () => {
    const user = userEvent.setup();
    const usage = vi.spyOn(portalApi, 'adminUsage').mockResolvedValue(usageData);
    vi.spyOn(portalApi, 'adminStats').mockResolvedValue(statsData);
    renderPage();

    expect(await screen.findByRole('heading', { name: '用量与统计' })).toBeInTheDocument();
    expect(screen.getByText('model_call_ledger', { exact: false })).toBeInTheDocument();
    expect(await screen.findByText('统计区间：2026-10-27 至 2026-11-02')).toBeInTheDocument();
    expect(usage).toHaveBeenCalledWith(7);
    expect(screen.getByText('账本调用数').closest('article')).toHaveTextContent('12');
    expect(screen.getByText('已结算调用').closest('article')).toHaveTextContent('9');
    expect(screen.getByText('待核对调用').closest('article')).toHaveTextContent('3');
    const usageSection = screen.getByRole('heading', { name: '平台模型用量' }).closest('section')!;
    const usageMetrics = usageSection.querySelector<HTMLElement>('.admin-detail-facts')!;
    expect(within(usageMetrics).getByText('活跃账户').closest('article')).toHaveTextContent('4');
    expect(screen.getByRole('cell', { name: 'model-a' })).toBeInTheDocument();

    expect(screen.getByText('活跃安装').closest('thead')).toBeInTheDocument();
    expect(screen.getByText(/last_used_at/)).toBeInTheDocument();
    expect(screen.getByRole('cell', { name: 'ios' }).closest('tr')).toHaveTextContent('6');
    expect(screen.getByRole('cell', { name: '2026-W44' }).closest('tr')).toHaveTextContent('37.5%');
    expect(screen.getByText(/注册后曾有安装的账号比例/)).toBeInTheDocument();
    expect(screen.getByText('暂不可采集客户端版本。服务端目前没有可用的版本统计数据。')).toBeInTheDocument();

    const filter = screen.getByLabelText('用量区间');
    await user.selectOptions(filter, '30');
    expect(usage).toHaveBeenCalledWith(30);
    await user.selectOptions(filter, '90');
    expect(usage).toHaveBeenCalledWith(90);
  });

  it('shows empty states for empty series without adding rows', async () => {
    vi.spyOn(portalApi, 'adminUsage').mockResolvedValue({
      ...usageData,
      summary: { calls: 0, settled_calls: 0, unknown_calls: 0, input_tokens: 0, output_tokens: 0, active_accounts: 0 },
      by_day: [], by_model: [],
    });
    vi.spyOn(portalApi, 'adminStats').mockResolvedValue({
      users: { total: 0, approved: 0, pending: 0, rejected: 0, disabled: 0 }, platforms: [], cohorts: [], version_tracking_available: false,
    });
    renderPage();

    expect(await screen.findByText('所选区间没有每日用量记录。')).toBeInTheDocument();
    expect(screen.getByText('所选区间没有模型用量记录。')).toBeInTheDocument();
    expect(screen.getByText('暂无平台安装统计。')).toBeInTheDocument();
    expect(screen.getByText('暂无注册周统计。')).toBeInTheDocument();
    expect(screen.queryByRole('cell', { name: 'model-a' })).not.toBeInTheDocument();
    expect(screen.queryByRole('cell', { name: 'ios' })).not.toBeInTheDocument();
  });

  it('shows independent loading states for both requests', async () => {
    let resolveUsage!: (value: UsageData) => void;
    let resolveStats!: (value: StatsData) => void;
    vi.spyOn(portalApi, 'adminUsage').mockImplementation(() => new Promise((resolve) => { resolveUsage = resolve; }));
    vi.spyOn(portalApi, 'adminStats').mockImplementation(() => new Promise((resolve) => { resolveStats = resolve; }));
    renderPage();

    expect(screen.getByText('正在加载平台模型用量…')).toBeInTheDocument();
    expect(screen.getByText('正在加载平台统计…')).toBeInTheDocument();
    resolveUsage(usageData);
    resolveStats(statsData);
    expect(await screen.findByRole('cell', { name: 'model-a' })).toBeInTheDocument();
  });

  it('shows and retries each request error independently', async () => {
    const user = userEvent.setup();
    const usage = vi.spyOn(portalApi, 'adminUsage')
      .mockRejectedValueOnce(new Error('用量暂不可用')).mockResolvedValue(usageData);
    const stats = vi.spyOn(portalApi, 'adminStats')
      .mockRejectedValueOnce(new Error('统计暂不可用')).mockResolvedValue(statsData);
    renderPage();

    await screen.findByText('用量暂不可用');
    await screen.findByText('统计暂不可用');
    expect(screen.getByText('用量暂不可用')).toBeInTheDocument();
    expect(screen.getByText('统计暂不可用')).toBeInTheDocument();
    const alerts = screen.getAllByRole('alert');
    const usageAlert = alerts.find((alert) => within(alert).queryByText('用量暂不可用'));
    const statsAlert = alerts.find((alert) => within(alert).queryByText('统计暂不可用'));
    expect(usageAlert).toBeTruthy();
    expect(statsAlert).toBeTruthy();

    await user.click(within(usageAlert!).getByRole('button', { name: '重试' }));
    expect(await screen.findByText('model-a')).toBeInTheDocument();
    await user.click(within(statsAlert!).getByRole('button', { name: '重试' }));
    expect(await screen.findByText('20')).toBeInTheDocument();
    expect(usage).toHaveBeenCalledTimes(2);
    expect(stats).toHaveBeenCalledTimes(2);
  });
});
