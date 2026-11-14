import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { MemoryRouter, Route, Routes } from 'react-router';
import type { components } from '../../api/schema';
import { portalApi } from '../../app/api';
import { ToastNoticeCenter, ToastProvider } from '../../app/toast';
import { AdminUserDetail } from './AdminUserDetail';

const user: components['schemas']['AdminUser'] = {
  id: 'u1234567', name: 'alice', role: 'user', status: 'approved',
  quota: { hosts: 2, cpu_milli: 4000, mem_mib: 8192 }, usage: { hosts: 0, cpu_milli: 0, mem_mib: 0 },
  default_host_resources: { cpu_milli: 1000, mem_mib: 2048 }, created_at: '2026-10-22T00:00:00Z',
};

const initialQuota: components['schemas']['AdminPersonalDataQuota'] = {
  used_bytes: 1024, reserved_bytes: 2048, quota_bytes: 5_368_709_120, max_attachment_bytes: 314_572_800,
  version: 3, default_quota_bytes: 5_368_709_120, default_max_attachment_bytes: 314_572_800,
};

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><ToastProvider><ToastNoticeCenter /><MemoryRouter initialEntries={['/admin/users/u1234567']}><Routes>
    <Route path="/admin/users/:id" element={<AdminUserDetail />} />
  </Routes></MemoryRouter></ToastProvider></QueryClientProvider>);
}

describe('admin user personal-data quota', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  it('shows usage and defaults, then saves limits with the loaded version', async () => {
    const input = userEvent.setup();
    vi.spyOn(portalApi, 'adminUser').mockResolvedValue(user);
    vi.spyOn(portalApi, 'adminHosts').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'adminUserInstallations').mockResolvedValue({ items: [], total: 0, limit: 25, offset: 0 });
    vi.spyOn(portalApi, 'adminUserAuthorizations').mockResolvedValue({ items: [], total: 0, limit: 25, offset: 0 });
    vi.spyOn(portalApi, 'adminUserModelCalls').mockResolvedValue({ items: [], total: 0, limit: 25, offset: 0 });
    vi.spyOn(portalApi, 'adminModelAccountBudget').mockResolvedValue({ owner_id: user.id, configured: false, token_limit: 0, max_concurrent: 0, used_tokens: 0, in_flight: 0, has_unknown: false });
    vi.spyOn(portalApi, 'adminPersonalDataQuota').mockResolvedValue(initialQuota);
    vi.spyOn(portalApi, 'audit').mockResolvedValue({ items: [] });
    const updated = { ...initialQuota, quota_bytes: 6 * 1024 ** 3, max_attachment_bytes: 400 * 1024 ** 2, version: 4 };
    const save = vi.spyOn(portalApi, 'setAdminPersonalDataQuota').mockResolvedValue(updated);
    renderPage();

    expect(await screen.findByRole('heading', { name: 'alice' })).toBeInTheDocument();
    expect(await screen.findByText('个人数据额度')).toBeInTheDocument();
    expect(screen.getByText('已用').closest('article')).toHaveTextContent('1,024 B');
    expect(screen.getByText('预留').closest('article')).toHaveTextContent('2,048 B');
    expect(screen.getByText('默认额度').closest('article')).toHaveTextContent('5,368,709,120 B');

    await input.clear(screen.getByLabelText('个人数据总额度（字节）'));
    await input.type(screen.getByLabelText('个人数据总额度（字节）'), String(updated.quota_bytes));
    await input.clear(screen.getByLabelText('单个附件上限（字节）'));
    await input.type(screen.getByLabelText('单个附件上限（字节）'), String(updated.max_attachment_bytes));
    await input.click(screen.getByRole('button', { name: '保存个人数据额度' }));

    expect(save).toHaveBeenCalledWith(user.id, {
      expected_version: initialQuota.version,
      quota_bytes: updated.quota_bytes,
      max_attachment_bytes: updated.max_attachment_bytes,
    });
    expect(await screen.findByText('个人数据额度已保存。')).toBeInTheDocument();
    expect(screen.getByText('版本 v4。额度只用于个人附件，与聊天、电脑文件和记录额度分开计算。')).toBeInTheDocument();
  });
});
