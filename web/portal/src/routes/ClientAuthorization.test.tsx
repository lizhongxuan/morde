import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { MemoryRouter, Route, Routes } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { portalApi } from '../app/api';
import { ClientAuthorization } from './ClientAuthorization';

afterEach(() => { cleanup(); vi.restoreAllMocks(); });

function renderPage() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={queryClient}><MemoryRouter initialEntries={['/client-authorize?user_code=ABCD2345']}><Routes><Route path="/client-authorize" element={<ClientAuthorization />} /></Routes></MemoryRouter></QueryClientProvider>);
}

describe('ClientAuthorization', () => {
  it('shows installation identity and requires explicit workspace selection', async () => {
    vi.spyOn(portalApi, 'inspectClientAuthorization').mockResolvedValue({ platform: 'desktop', display_name: 'Morde 笔记本', wants_workspace_sync: true, expires_at: '2027-10-25T00:00:00Z' });
    const clients = vi.spyOn(portalApi, 'clients');
    vi.spyOn(portalApi, 'workspaces').mockResolvedValue({ items: [{ id: 'wsp_12345678', name: '项目', state: 'ready', used_bytes: 0, quota_bytes: 1000, revision: 1, created_at: '2026-10-25T00:00:00Z', updated_at: '2026-10-25T00:00:00Z' }] });
    const approve = vi.spyOn(portalApi, 'approveClientAuthorization').mockResolvedValue({ id: 'cli_12345678', platform: 'desktop', display_name: 'Morde 笔记本', workspace_ids: [], expires_at: '2027-10-25T00:00:00Z', revoked: false, created_at: '2026-10-25T00:00:00Z', last_used_at: null });
    const user = userEvent.setup();
    renderPage();
    expect(screen.getByLabelText('客户端验证码')).toHaveValue('ABCD2345');
    await waitFor(() => expect(portalApi.inspectClientAuthorization).toHaveBeenCalledWith('ABCD2345'));
    expect(await screen.findByText('请求来自：Morde 笔记本')).toBeInTheDocument();
    expect(screen.getByText('平台：电脑')).toBeInTheDocument();
    expect(screen.queryByText('这是已配对客户端的授权续期。')).not.toBeInTheDocument();
    expect(clients).not.toHaveBeenCalled();
    await user.click(screen.getByRole('button', { name: '确认并连接' }));
    await waitFor(() => expect(approve.mock.calls[0]?.[0]).toEqual({ user_code: 'ABCD2345', workspace_ids: [] }));
  });

  it('identifies a renewal by its registered installation ID and name before approval', async () => {
    vi.spyOn(portalApi, 'inspectClientAuthorization').mockResolvedValue({
      platform: 'desktop', display_name: 'Morde 笔记本', wants_workspace_sync: false,
      renewal_installation_id: 'cli_12345678', expires_at: '2027-10-25T00:00:00Z',
    });
    const clients = vi.spyOn(portalApi, 'clients').mockResolvedValue({ items: [{
      id: 'cli_12345678', platform: 'desktop', display_name: '办公室 Mac', workspace_ids: [],
      expires_at: '2026-10-25T00:00:00Z', revoked: false, created_at: '2025-10-25T00:00:00Z', last_used_at: null,
    }] });
    const approve = vi.spyOn(portalApi, 'approveClientAuthorization').mockResolvedValue({
      id: 'cli_12345678', platform: 'desktop', display_name: 'Morde 笔记本', workspace_ids: [],
      expires_at: '2027-10-25T00:00:00Z', revoked: false, created_at: '2025-10-25T00:00:00Z', last_used_at: null,
    });
    renderPage();

    expect(await screen.findByText('这是已配对客户端的授权续期。')).toBeInTheDocument();
    expect(await screen.findByText('安装 ID：cli_12345678')).toBeInTheDocument();
    expect(screen.getByText('已登记名称：办公室 Mac')).toBeInTheDocument();
    expect(screen.getByText('本次请求名称：Morde 笔记本')).toBeInTheDocument();
    expect(clients).toHaveBeenCalledOnce();
    await userEvent.setup().click(screen.getByRole('button', { name: '确认并连接' }));
    await waitFor(() => expect(approve).toHaveBeenCalledWith({ user_code: 'ABCD2345', workspace_ids: [] }, expect.anything()));
  });

  it.each([
    { items: [], alert: '找不到这台已配对安装，无法核对。请拒绝请求并重新发起配对。' },
    { items: [{ id: 'cli_12345678', platform: 'desktop' as const, display_name: '办公室 Mac', workspace_ids: [], expires_at: '2026-10-25T00:00:00Z', revoked: true, created_at: '2025-10-25T00:00:00Z', last_used_at: null }], alert: '此安装已撤销，不能通过续期重新启用。请拒绝请求并重新配对。' },
  ])('blocks renewal approval when the installation cannot be trusted', async ({ items, alert }) => {
    vi.spyOn(portalApi, 'inspectClientAuthorization').mockResolvedValue({
      platform: 'desktop', display_name: 'Morde 笔记本', wants_workspace_sync: false,
      renewal_installation_id: 'cli_12345678', expires_at: '2027-10-25T00:00:00Z',
    });
    vi.spyOn(portalApi, 'clients').mockResolvedValue({ items });
    const approve = vi.spyOn(portalApi, 'approveClientAuthorization');
    renderPage();

    expect(await screen.findByText(alert)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '确认并连接' })).toBeDisabled();
    expect(approve).not.toHaveBeenCalled();
  });

  it('does not offer workspace grants to a phone and can deny a request', async () => {
    vi.spyOn(portalApi, 'inspectClientAuthorization').mockResolvedValue({ platform: 'ios', display_name: 'iPhone', wants_workspace_sync: false, expires_at: '2027-10-25T00:00:00Z' });
    const workspaces = vi.spyOn(portalApi, 'workspaces');
    const deny = vi.spyOn(portalApi, 'denyClientAuthorization').mockResolvedValue(undefined);
    renderPage();
    expect(await screen.findByText('平台：iPhone')).toBeInTheDocument();
    expect(screen.queryByRole('group', { name: '允许此电脑同步哪些工作区' })).not.toBeInTheDocument();
    expect(workspaces).not.toHaveBeenCalled();
    await userEvent.setup().click(screen.getByRole('button', { name: '拒绝请求' }));
    await waitFor(() => expect(deny.mock.calls[0]?.[0]).toBe('ABCD2345'));
    expect(within(screen.getByRole('status')).getByText('已拒绝此请求')).toBeInTheDocument();
  });
});
