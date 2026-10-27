import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { MemoryRouter, Route, Routes } from 'react-router';
import type { components } from '../../api/schema';
import { portalApi } from '../../app/api';
import { ToastProvider } from '../../app/toast';
import { AdminUsers } from './AdminUsers';

const currentUser = vi.hoisted(() => ({ id: 'uadmin01', name: 'admin', role: 'super_admin' }));
vi.mock('../../app/auth', () => ({ useAuth: () => ({ user: currentUser }) }));

type AccountStatus = components['schemas']['AccountStatus'];
type AdminUser = components['schemas']['AdminUser'];

function userRecord(id: string, name: string, role: AdminUser['role'], status: AccountStatus): AdminUser {
  return {
    id, name, role, status,
    quota: { hosts: 2, cpu_milli: 2000, mem_mib: 4096 },
    usage: { hosts: 0, cpu_milli: 0, mem_mib: 0 },
    default_host_resources: { cpu_milli: 1000, mem_mib: 2048 },
    created_at: '2026-10-25T00:00:00Z',
  };
}

function setup(records: AdminUser[]) {
  const rows = [...records];
  const list = vi.spyOn(portalApi, 'adminUsers').mockImplementation(async (status) => ({
    items: rows.filter((item) => status === undefined || item.status === status),
  }));
  const update = vi.spyOn(portalApi, 'updateUser').mockImplementation(async (id, body) => {
    const index = rows.findIndex((item) => item.id === id);
    const current = rows[index];
    if (!current || current.status !== body.expected_status) throw new Error('账号状态已变化，请刷新后重试。');
    const next = { ...current, ...(body.status ? { status: body.status } : {}), ...(body.role ? { role: body.role } : {}) };
    rows[index] = next;
    return next;
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter initialEntries={['/admin/users']}><Routes>
    <Route path="/admin/users" element={<AdminUsers />} />
  </Routes></MemoryRouter></ToastProvider></QueryClientProvider>);
  return { list, update };
}

describe('admin account management', () => {
  afterEach(() => { currentUser.id = 'uadmin01'; currentUser.role = 'super_admin'; cleanup(); vi.restoreAllMocks(); });

  it('filters by status and protects the super administrator and current account', async () => {
    const input = userEvent.setup();
    const { list } = setup([
      userRecord('uadmin01', 'admin', 'super_admin', 'approved'),
      userRecord('u0000001', 'alice', 'user', 'pending'),
    ]);

    expect(await screen.findByText('受保护的超级管理员')).toBeInTheDocument();
    const protectedRow = screen.getByRole('row', { name: /admin.*超级管理员/ });
    expect(protectedRow).toHaveTextContent('受保护的超级管理员');
    expect(within(protectedRow).queryByRole('button')).not.toBeInTheDocument();
    await input.selectOptions(screen.getByLabelText('账号状态'), 'pending');
    expect(await screen.findByRole('row', { name: /alice.*待审核/ })).toBeInTheDocument();
    expect(list).toHaveBeenLastCalledWith('pending');
    const currentRow = screen.getByRole('row', { name: /alice.*待审核/ });
    expect(within(currentRow).getByRole('button', { name: '通过审核' })).toBeInTheDocument();
  });

  it('approves a pending registration using its expected status', async () => {
    const input = userEvent.setup();
    const { update } = setup([userRecord('u0000001', 'alice', 'user', 'pending')]);

    const row = await screen.findByRole('row', { name: /alice.*待审核/ });
    await input.click(within(row).getByRole('button', { name: '通过审核' }));
    expect(update).toHaveBeenCalledWith('u0000001', { expected_status: 'pending', status: 'approved' });
    expect(await screen.findByRole('row', { name: /alice.*已通过/ })).toBeInTheDocument();
  });

  it('does not offer status or role changes for the current administrator', async () => {
    currentUser.id = 'u0000001';
    currentUser.role = 'admin';
    setup([userRecord('u0000001', 'alice', 'admin', 'approved')]);
    const row = await screen.findByRole('row', { name: /alice.*管理员.*已通过/ });
    expect(within(row).getByText('当前账号')).toBeInTheDocument();
    expect(within(row).queryByRole('button')).not.toBeInTheDocument();
  });

  it('reapproves rejected accounts and restores disabled accounts', async () => {
    const input = userEvent.setup();
    const { update } = setup([
      userRecord('u0000001', 'alice', 'user', 'rejected'),
      userRecord('u0000002', 'bob', 'user', 'disabled'),
    ]);

    const alice = await screen.findByRole('row', { name: /alice.*已拒绝/ });
    await input.click(within(alice).getByRole('button', { name: '重新通过' }));
    const bob = await screen.findByRole('row', { name: /bob.*已禁用/ });
    await input.click(within(bob).getByRole('button', { name: '恢复账号' }));
    expect(update).toHaveBeenNthCalledWith(1, 'u0000001', { expected_status: 'rejected', status: 'approved' });
    expect(update).toHaveBeenNthCalledWith(2, 'u0000002', { expected_status: 'disabled', status: 'approved' });
    expect(await screen.findByRole('row', { name: /bob.*已通过/ })).toBeInTheDocument();
  });

  it('disables approved accounts and grants or revokes administrator role only while approved', async () => {
    const input = userEvent.setup();
    const { update } = setup([userRecord('u0000001', 'alice', 'user', 'approved')]);

    const row = await screen.findByRole('row', { name: /alice.*普通用户.*已通过/ });
    await input.click(within(row).getByRole('button', { name: '设为管理员' }));
    expect(update).toHaveBeenCalledWith('u0000001', { expected_status: 'approved', role: 'admin' });
    expect(await screen.findByRole('row', { name: /alice.*管理员.*已通过/ })).toBeInTheDocument();
    await input.click(within(screen.getByRole('row', { name: /alice.*管理员.*已通过/ })).getByRole('button', { name: '撤销管理员' }));
    expect(update).toHaveBeenNthCalledWith(2, 'u0000001', { expected_status: 'approved', role: 'user' });
    await input.click(within(screen.getByRole('row', { name: /alice.*普通用户.*已通过/ })).getByRole('button', { name: '禁用账号' }));
    expect(update).toHaveBeenNthCalledWith(3, 'u0000001', { expected_status: 'approved', status: 'disabled' });
  });
});
