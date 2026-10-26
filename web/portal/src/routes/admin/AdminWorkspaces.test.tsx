import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { MemoryRouter, Route, Routes } from 'react-router';
import type { components } from '../../api/schema';
import { portalApi } from '../../app/api';
import { AdminWorkspaceDetail } from './AdminWorkspaceDetail';
import { AdminUserDetail } from './AdminUserDetail';
import { AdminWorkspaces } from './AdminWorkspaces';

const workspace: components['schemas']['AdminWorkspace'] = {
  id: 'w1234567', owner_id: 'u1234567', owner_name: 'alice', name: '示例工作区', state: 'ready',
  revision: 3, used_bytes: 1024, quota_bytes: 4096, created_at: '2026-10-25T00:00:00Z', updated_at: '2026-10-25T01:00:00Z',
  bindings: [{ host_id: 'h1234567', host_name: '开发电脑', state: 'active', sync_state: 'healthy', desired_revision: 3, applied_revision: 3 }],
};
const user: components['schemas']['AdminUser'] = {
  id: 'u1234567', name: 'alice', role: 'user', disabled: false,
  quota: { hosts: 2, cpu_milli: 4000, mem_mib: 8192 }, usage: { hosts: 0, cpu_milli: 0, mem_mib: 0 },
  created_at: '2026-10-22T00:00:00Z',
};

function renderPage(path: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><MemoryRouter initialEntries={[path]}><Routes>
    <Route path="/admin/workspaces" element={<AdminWorkspaces />} />
    <Route path="/admin/workspaces/:id" element={<AdminWorkspaceDetail />} />
    <Route path="/admin/users/:id" element={<AdminUserDetail />} />
  </Routes></MemoryRouter></QueryClientProvider>);
}

describe('admin workspaces', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  it('lists owner, storage metadata and binding links without file content', async () => {
    vi.spyOn(portalApi, 'adminWorkspaces').mockResolvedValue({ items: [workspace], next_cursor: null });
    renderPage('/admin/workspaces');

    expect(await screen.findByRole('link', { name: /示例工作区/ })).toHaveAttribute('href', '/admin/workspaces/w1234567');
    expect(screen.getByRole('link', { name: /alice/ })).toHaveAttribute('href', '/admin/users/u1234567');
    expect(screen.getByRole('link', { name: /开发电脑/ })).toHaveAttribute('href', '/admin/hosts/h1234567');
    expect(screen.queryByText(/文件正文|\.env|secret/)).not.toBeInTheDocument();
  });

  it('shows revision and applied state while keeping workspace contents private', async () => {
    vi.spyOn(portalApi, 'adminWorkspace').mockResolvedValue(workspace);
    renderPage('/admin/workspaces/w1234567');

    expect(await screen.findByRole('heading', { name: '示例工作区' })).toBeInTheDocument();
    expect(screen.getByText('当前版本').closest('article')).toHaveTextContent('v3');
    expect(screen.getByText('已绑定')).toBeInTheDocument();
    expect(screen.getByText(/不包含目录路径、文件正文或恢复操作/)).toBeInTheDocument();
  });

  it('shows the user workspace metadata and finds it across pages', async () => {
    vi.spyOn(portalApi, 'adminUsers').mockResolvedValue({ items: [user] });
    vi.spyOn(portalApi, 'adminHosts').mockResolvedValue({ items: [] });
    const list = vi.spyOn(portalApi, 'adminWorkspaces')
      .mockResolvedValueOnce({ items: [], next_cursor: 'next' })
      .mockResolvedValueOnce({ items: [workspace], next_cursor: null });
    const input = userEvent.setup();
    renderPage('/admin/users/u1234567');

    expect(await screen.findByText(/暂未找到该用户的工作区/)).toBeInTheDocument();
    await input.click(screen.getByRole('button', { name: '继续查找工作区' }));
    expect(await screen.findByRole('link', { name: /示例工作区/ })).toHaveAttribute('href', '/admin/workspaces/w1234567');
    expect(screen.getByRole('row', { name: /1,024 B.*4,096 B/ })).toBeInTheDocument();
    expect(list).toHaveBeenLastCalledWith('next');
  });
});
