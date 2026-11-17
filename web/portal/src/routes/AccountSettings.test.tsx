import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { MemoryRouter } from 'react-router';
import { portalApi } from '../app/api';
import { ToastNoticeCenter, ToastProvider } from '../app/toast';
import { AccountSettings } from './AccountSettings';

vi.mock('../app/auth', () => ({ useAuth: () => ({ user: { id: 'u1', name: 'Alice', role: 'user' } }) }));
const personalSyncStore = vi.hoisted(() => ({
  subscribeStatus: vi.fn((_userID: string, listener: (status: { pendingCount: number; conflictCount: number }) => void) => {
    listener({ pendingCount: 2, conflictCount: 1 });
    return vi.fn();
  }),
}));
vi.mock('./personal-data-store', () => ({
  subscribePersonalDataSyncStatus: personalSyncStore.subscribeStatus,
}));

describe('AccountSettings', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  function renderSettings() {
    const capabilities = vi.spyOn(portalApi, 'capabilities').mockResolvedValue({ service_instance_id: 'service-account-test' } as Awaited<ReturnType<typeof portalApi.capabilities>>);
    const personalDataQuota = vi.spyOn(portalApi, 'personalDataQuota').mockResolvedValue({ owner_id: 'u1', used_bytes: 10, reserved_bytes: 2,
      quota_bytes: 1000, max_attachment_bytes: 500, remaining_bytes: 988, version: 1 });
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    render(<QueryClientProvider client={queryClient}><ToastProvider><ToastNoticeCenter autoOpenErrors overlay /><MemoryRouter><AccountSettings /></MemoryRouter></ToastProvider></QueryClientProvider>);
    return { capabilities, personalDataQuota };
  }

  it('shows the account and personal data sections without record controls', () => {
    renderSettings();

    expect(screen.getByRole('heading', { level: 1, name: '账户设置' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { level: 2, name: '账号与安全' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { level: 3, name: 'Alice' })).toBeInTheDocument();
    expect(screen.getByText('普通用户')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: '修改密码' })).toHaveAttribute('href', '/settings/password');
    expect(screen.getByRole('link', { name: '管理客户端' })).toHaveAttribute('href', '/clients');
    expect(screen.getAllByRole('tab')).toHaveLength(2);
    expect(screen.getByRole('tab', { name: '账号' })).toBeInTheDocument();
    expect(screen.getByRole('tab', { name: '个人数据' })).toBeInTheDocument();
    expect(screen.queryByRole('tab', { name: '隐私与同意' })).not.toBeInTheDocument();
    expect(screen.queryByRole('tab', { name: '记录状态' })).not.toBeInTheDocument();
    expect(screen.queryByRole('tab', { name: '已保存记录' })).not.toBeInTheDocument();
  });

  it('loads and displays personal data status only when its section is opened', async () => {
    const { capabilities, personalDataQuota } = renderSettings();
    expect(capabilities).not.toHaveBeenCalled();
    expect(personalDataQuota).not.toHaveBeenCalled();

    await userEvent.setup().click(screen.getByRole('tab', { name: '个人数据' }));
    await waitFor(() => {
      expect(capabilities).toHaveBeenCalledOnce();
      expect(personalDataQuota).toHaveBeenCalledOnce();
    });
    expect(await screen.findByText('待提交 2 项 · 冲突 1 项')).toBeInTheDocument();
    expect(await screen.findByText(/已用 10 B/)).toBeInTheDocument();
    expect(personalSyncStore.subscribeStatus).toHaveBeenCalledWith('u1', expect.any(Function), window.location.origin, 'service-account-test');
  });
});
