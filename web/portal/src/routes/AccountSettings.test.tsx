import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { MemoryRouter } from 'react-router';
import { portalApi } from '../app/api';
import { ToastProvider } from '../app/toast';
import { AccountSettings } from './AccountSettings';

vi.mock('../app/auth', () => ({ useAuth: () => ({ user: { name: 'Alice', role: 'user' } }) }));

describe('AccountSettings', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  function renderSettings() {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    return render(<QueryClientProvider client={queryClient}><ToastProvider><MemoryRouter><AccountSettings /></MemoryRouter></ToastProvider></QueryClientProvider>);
  }

  it('shows the signed-in account, links, and default-off record consent controls', async () => {
    vi.spyOn(portalApi, 'recordConsents').mockResolvedValue({ items: [
      { owner_id: 'u1', purpose: 'full_record', enabled: false, version: 0 },
      { owner_id: 'u1', purpose: 'training_contribution', enabled: false, version: 0 },
    ] });
    renderSettings();

    expect(screen.getByRole('heading', { level: 1, name: '账户设置' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { level: 3, name: 'Alice' })).toBeInTheDocument();
    expect(screen.getByText('普通用户')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: '修改密码' })).toHaveAttribute('href', '/settings/password');
    expect(screen.getByRole('link', { name: '管理客户端' })).toHaveAttribute('href', '/clients');
    expect(await screen.findByRole('checkbox', { name: '允许保存完整会话记录' })).not.toBeChecked();
    expect(screen.getByRole('checkbox', { name: '允许保存训练样本贡献' })).not.toBeChecked();
  });

  it('updates one independent consent and reloads its server state', async () => {
    const user = userEvent.setup();
    vi.spyOn(portalApi, 'recordConsents')
      .mockResolvedValueOnce({ items: [
        { owner_id: 'u1', purpose: 'full_record', enabled: false, version: 0 },
        { owner_id: 'u1', purpose: 'training_contribution', enabled: false, version: 0 },
      ] })
      .mockResolvedValueOnce({ items: [
        { owner_id: 'u1', purpose: 'full_record', enabled: true, version: 1 },
        { owner_id: 'u1', purpose: 'training_contribution', enabled: false, version: 0 },
      ] });
    const setConsent = vi.spyOn(portalApi, 'setRecordConsent').mockResolvedValue({ owner_id: 'u1', purpose: 'full_record', enabled: true, version: 1 });
    renderSettings();

    await user.click(await screen.findByRole('checkbox', { name: '允许保存完整会话记录' }));
    await waitFor(() => expect(setConsent.mock.calls[0]?.[0]).toEqual({ purpose: 'full_record', enabled: true }));
    expect(await screen.findByRole('checkbox', { name: '允许保存完整会话记录' })).toBeChecked();
    expect(screen.getByRole('checkbox', { name: '允许保存训练样本贡献' })).not.toBeChecked();
  });
});
