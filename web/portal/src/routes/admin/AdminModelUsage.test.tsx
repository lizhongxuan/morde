import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { MemoryRouter, Route, Routes } from 'react-router';
import type { components } from '../../api/schema';
import { portalApi } from '../../app/api';
import { ToastProvider } from '../../app/toast';
import { AdminUserDetail } from './AdminUserDetail';

const user: components['schemas']['AdminUser'] = {
  id: 'u1234567', name: 'alice', role: 'user', status: 'approved',
  quota: { hosts: 2, cpu_milli: 4000, mem_mib: 8192 }, usage: { hosts: 0, cpu_milli: 0, mem_mib: 0 },
  default_host_resources: { cpu_milli: 1000, mem_mib: 2048 }, created_at: '2026-10-22T00:00:00Z',
};

type UnknownModelCall = Awaited<ReturnType<typeof portalApi.adminUnknownModelUsage>>['items'][number];

const call: UnknownModelCall = {
  owner_id: user.id, source: 'task', call_id: 'model-call-123', model_id: 'm1234567', provider_model: 'provider-model',
  task_id: 't1234567', reserved_tokens: 1000, created_at: '2026-10-27T00:00:00Z', unknown_at: '2026-10-27T00:01:00Z',
};

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter initialEntries={['/admin/users/u1234567']}><Routes>
    <Route path="/admin/users/:id" element={<AdminUserDetail />} />
  </Routes></MemoryRouter></ToastProvider></QueryClientProvider>);
}

describe('admin model usage reconciliation', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  it('lists only after opening and records provider-verified counts with evidence', async () => {
    const input = userEvent.setup();
    vi.spyOn(portalApi, 'adminUsers').mockResolvedValue({ items: [user] });
    vi.spyOn(portalApi, 'adminHosts').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'adminWorkspaces').mockResolvedValue({ items: [], next_cursor: null });
    vi.spyOn(portalApi, 'adminModelAccountBudget').mockResolvedValue({ owner_id: user.id, configured: true, token_limit: 5000, max_concurrent: 2, used_tokens: 0, in_flight: 0, has_unknown: false });
    const list = vi.spyOn(portalApi, 'adminUnknownModelUsage').mockResolvedValue({ items: [call] });
    const reconcile = vi.spyOn(portalApi, 'reconcileModelUsage').mockResolvedValue({ replayed: false });
    renderPage();

    expect(await screen.findByRole('heading', { name: 'alice' })).toBeInTheDocument();
    expect(list).not.toHaveBeenCalled();
    await input.click(screen.getByRole('button', { name: '查看待核对记录' }));
    expect(await screen.findByText('provider-model')).toBeInTheDocument();
    expect(list).toHaveBeenCalledWith(user.id);

    await input.type(screen.getByLabelText('核实的输入 tokens'), '620');
    await input.type(screen.getByLabelText('核实的输出 tokens'), '180');
    await input.type(screen.getByLabelText('供应商记录与核对依据'), '供应商账单记录 #abc-123');
    await input.click(screen.getByRole('button', { name: '保存核对结果' }));

    expect(reconcile).toHaveBeenCalledWith({
      owner_id: user.id, source: 'task', call_id: call.call_id, input_tokens: 620, output_tokens: 180,
      evidence: '供应商账单记录 #abc-123',
    });
    expect(await screen.findByText('模型用量已核对并记账。')).toBeInTheDocument();
    expect(screen.getByText(/不会恢复丢失的模型响应/)).toBeInTheDocument();
  });

  it('lets an administrator update the token and concurrency limits', async () => {
    const input = userEvent.setup();
    vi.spyOn(portalApi, 'adminUsers').mockResolvedValue({ items: [user] });
    vi.spyOn(portalApi, 'adminHosts').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'adminWorkspaces').mockResolvedValue({ items: [], next_cursor: null });
    vi.spyOn(portalApi, 'adminModelAccountBudget').mockResolvedValue({ owner_id: user.id, configured: true, token_limit: 5000, max_concurrent: 2, used_tokens: 120, in_flight: 1, has_unknown: true });
    const update = vi.spyOn(portalApi, 'setAdminModelAccountBudget').mockResolvedValue({
      owner_id: user.id, configured: true, token_limit: 7000, max_concurrent: 4, used_tokens: 120, in_flight: 1, has_unknown: true,
    });
    renderPage();

    expect(await screen.findByText('120 / 5,000 tokens')).toBeInTheDocument();
    await input.clear(screen.getByLabelText('平台模型 token 上限'));
    await input.type(screen.getByLabelText('平台模型 token 上限'), '7000');
    await input.clear(screen.getByLabelText('同时进行的模型调用数'));
    await input.type(screen.getByLabelText('同时进行的模型调用数'), '4');
    await input.click(screen.getByRole('button', { name: '保存预算' }));

    expect(update).toHaveBeenCalledWith(user.id, { token_limit: 7000, max_concurrent: 4 });
    expect(await screen.findByText('平台模型预算已保存。')).toBeInTheDocument();
    expect(screen.getByText('需要核对')).toBeInTheDocument();
  });
});
