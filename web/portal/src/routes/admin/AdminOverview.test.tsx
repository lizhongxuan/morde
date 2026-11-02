import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { MemoryRouter, Route, Routes } from 'react-router';
import { portalApi } from '../../app/api';
import { AdminOverview } from './AdminOverview';

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><MemoryRouter initialEntries={['/admin']}><Routes>
    <Route path="/admin" element={<AdminOverview />} />
  </Routes></MemoryRouter></QueryClientProvider>);
}

describe('admin overview', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  it('shows distinct health, online installation, Host, and Agent values', async () => {
    vi.spyOn(portalApi, 'adminOverview').mockResolvedValue({
      checked_at: '2026-11-02T00:00:00Z',
      health: { ready: true },
      online_installations: 3,
      limit: { hosts: 10, cpu_milli: 8000, mem_mib: 16384 }, allocated: { hosts: 3, cpu_milli: 3000, mem_mib: 6144 },
      hosts_by_state: { running: 2, stopped: 1 },
      agents_by_state: { active: 4, idle: 1 },
    });
    renderPage();

    expect(await screen.findByRole('heading', { name: '管理总览' })).toBeInTheDocument();
    expect(screen.getByText('就绪')).toBeInTheDocument();
    expect(screen.getByText('3')).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Host 状态' })).toBeInTheDocument();
    expect(screen.getByText('运行中').closest('.state-count')).toHaveTextContent('2');
    expect(screen.getByText('已停止').closest('.state-count')).toHaveTextContent('1');
    expect(screen.getByRole('heading', { name: 'Agent 状态' })).toBeInTheDocument();
    expect(screen.getByText('active').closest('.state-count')).toHaveTextContent('4');
    expect(screen.getByText('idle').closest('.state-count')).toHaveTextContent('1');
    expect(screen.getByText(/不代表 Agent 或 Host 数量/)).toBeInTheDocument();
  });

  it('shows empty states without inventing Host or Agent counts', async () => {
    vi.spyOn(portalApi, 'adminOverview').mockResolvedValue({
      checked_at: '2026-11-02T00:00:00Z', health: { ready: false, problem: '数据库暂不可用' },
      online_installations: 0, limit: { hosts: 0, cpu_milli: 0, mem_mib: 0 }, allocated: { hosts: 0, cpu_milli: 0, mem_mib: 0 }, hosts_by_state: {}, agents_by_state: {},
    });
    renderPage();

    expect(await screen.findByText('未就绪')).toBeInTheDocument();
    expect(screen.getByText('数据库暂不可用')).toBeInTheDocument();
    expect(screen.getByText('0')).toBeInTheDocument();
    expect(screen.getByText('暂无 Host 状态数据。')).toBeInTheDocument();
    expect(screen.getByText('暂无 Agent 状态数据。')).toBeInTheDocument();
  });

  it('shows a loading state while the overview request is pending', async () => {
    let resolveRequest!: (value: Awaited<ReturnType<typeof portalApi.adminOverview>>) => void;
    vi.spyOn(portalApi, 'adminOverview').mockImplementation(() => new Promise((resolve) => { resolveRequest = resolve; }));
    renderPage();

    expect(screen.getByRole('status')).toHaveTextContent('正在加载管理总览');
    resolveRequest({
      checked_at: '2026-11-02T00:00:00Z', health: { ready: true }, online_installations: 1,
      limit: { hosts: 0, cpu_milli: 0, mem_mib: 0 }, allocated: { hosts: 0, cpu_milli: 0, mem_mib: 0 },
      hosts_by_state: {}, agents_by_state: {},
    });
    expect(await screen.findByText('1')).toBeInTheDocument();
  });

  it('shows request errors and retries the overview request', async () => {
    const user = userEvent.setup();
    let resolveRequest!: (value: Awaited<ReturnType<typeof portalApi.adminOverview>>) => void;
    const request = vi.spyOn(portalApi, 'adminOverview')
      .mockRejectedValueOnce(new Error('总览暂不可用'))
      .mockImplementationOnce(() => new Promise((resolve) => { resolveRequest = resolve; }));
    renderPage();

    expect(await screen.findByRole('alert')).toHaveTextContent('总览暂不可用');
    await user.click(screen.getByRole('button', { name: '重试' }));
    resolveRequest({
      checked_at: '2026-11-02T00:00:00Z', health: { ready: true }, online_installations: 1,
      limit: { hosts: 0, cpu_milli: 0, mem_mib: 0 }, allocated: { hosts: 0, cpu_milli: 0, mem_mib: 0 },
      hosts_by_state: {}, agents_by_state: {},
    });
    expect(await screen.findByText('1')).toBeInTheDocument();
    expect(request).toHaveBeenCalledTimes(2);
  });
});
