import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { MemoryRouter, Route, Routes } from 'react-router';
import type { components } from '../../api/schema';
import { portalApi } from '../../app/api';
import { ToastNoticeCenter, ToastProvider } from '../../app/toast';
import { AdminHostDetail } from './AdminHostDetail';

const owner: components['schemas']['AdminUser'] = {
  id: 'u1234567', name: 'alice', role: 'user', status: 'approved',
  quota: { hosts: 2, cpu_milli: 4000, mem_mib: 8192 }, usage: { hosts: 1, cpu_milli: 2000, mem_mib: 4096 },
  default_host_resources: { cpu_milli: 2000, mem_mib: 4096 }, created_at: '2026-10-22T00:00:00Z',
};

const failedHost: components['schemas']['Host'] = {
  id: 'h1234567', owner_id: owner.id, name: '默认电脑', template_id: 'standard', isolation: 'container',
  state: 'failed', state_reason: 'lost', runtime_generation: 5,
  control_version: 0, control_state: 'ai', url: 'https://h1234567.example.test',
  resources: { cpu_milli: 2000, mem_mib: 4096, cpu_limit_milli: 2000, mem_limit_mib: 4096 }, workspaces: [],
  expires_at: '2026-12-22T00:00:00Z', last_active_at: null, created_at: '2026-10-22T00:00:00Z',
};

function renderPage(host: components['schemas']['Host'] = failedHost) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.spyOn(portalApi, 'adminHosts').mockResolvedValue({ items: [host] });
  vi.spyOn(portalApi, 'adminUsers').mockResolvedValue({ items: [owner], total: 1, limit: 50, offset: 0 });
  vi.spyOn(portalApi, 'adminHostResources').mockResolvedValue({
    host_id: host.id, state: 'failed', resources: { cpu_milli: 2000, mem_mib: 4096 }, version: 1, update_state: 'idle',
  });
  return render(<QueryClientProvider client={client}><ToastProvider><ToastNoticeCenter /><MemoryRouter initialEntries={[`/admin/hosts/${host.id}`]}><Routes>
    <Route path="/admin/hosts/:id" element={<AdminHostDetail />} />
  </Routes></MemoryRouter></ToastProvider></QueryClientProvider>);
}

describe('admin Host recovery', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  it('requires explicit confirmation before recovering a failed Host and sends its exact generation', async () => {
    const user = userEvent.setup();
    const recover = vi.spyOn(portalApi, 'adminRecoverHost').mockResolvedValue({
      host_id: failedHost.id, runtime_generation: 6, action: 'retry_create', sandbox_count: 0,
    });
    renderPage();

    expect(await screen.findByRole('heading', { name: '异常恢复' })).toBeInTheDocument();
    expect(screen.getByText(/失败的沙箱会在确认后移除并创建新代/)).toBeInTheDocument();
    const confirmation = screen.getByRole('checkbox', { name: /我已修复外部问题/ });
    const action = screen.getByRole('button', { name: '检查并恢复' });
    expect(confirmation).not.toBeChecked();
    expect(action).toBeDisabled();
    expect(recover).not.toHaveBeenCalled();

    await user.click(confirmation);
    expect(action).toBeEnabled();
    await user.click(action);

    expect(recover).toHaveBeenCalledTimes(1);
    expect(recover).toHaveBeenCalledWith(failedHost.id, { runtime_generation: 5, confirm_retry: true });
    expect(await screen.findByText('已确认并重新创建电脑，电脑数据目录已保留。')).toBeInTheDocument();
  });

  it('explains that an unknown restore is checked before retrying', async () => {
    const restoreUnknown: components['schemas']['Host'] = {
      ...failedHost, state: 'creating', state_reason: 'restore_unknown', runtime_generation: 4,
    };
    renderPage(restoreUnknown);

    expect(await screen.findByRole('heading', { name: '异常恢复' })).toBeInTheDocument();
    expect(screen.getByText(/上次恢复结果不确定，系统已暂停自动重试/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '检查并恢复' })).toBeDisabled();
  });
});
