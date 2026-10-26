import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { portalApi } from '../app/api';
import { ToastProvider } from '../app/toast';
import { ApiKeys } from './ApiKeys';

const host = {
  id: 'h1234567', name: '测试主机', owner_id: 'u1234567', template_id: 'std', isolation: 'container', state: 'running', state_reason: '', url: '',
  workspaces: [],
  resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 },
  runtime_generation: 1, control_version: 1, control_state: 'ai', expires_at: null, last_active_at: null, created_at: '',
} satisfies Awaited<ReturnType<typeof portalApi.hosts>>['items'][number];

const renderPage = () => {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><ApiKeys /></MemoryRouter></ToastProvider></QueryClientProvider>);
};

describe('ApiKeys', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  it('creates a key only after choosing explicit scopes and carries the selected host', async () => {
    vi.spyOn(portalApi, 'apiKeys').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [host] });
    vi.spyOn(portalApi, 'sharedHosts').mockResolvedValue({ items: [] });
    const create = vi.spyOn(portalApi, 'createApiKey').mockResolvedValue({ id: 't1234567', key: 'chk_one-time-secret', host_ids: ['h1234567'], scopes: ['computer:operate'], expires_at: new Date(Date.now() + 86400000).toISOString() });
    const user = userEvent.setup();
    renderPage();

    const submit = await screen.findByRole('button', { name: '创建 API Key' });
    expect(screen.getByText('为自动化和 MCP 创建权限受限的密钥。密钥明文只显示一次。')).toBeInTheDocument();
    expect(screen.queryByText(/Codex/)).not.toBeInTheDocument();
    expect(submit).toBeDisabled();
    await user.type(screen.getByLabelText('名称'), '我的 MCP');
    await user.click(screen.getByLabelText(/测试主机/));
    await user.click(screen.getByLabelText(/操作浏览器、终端和文件/));
    expect(submit).toBeEnabled();
    await user.click(submit);

    await waitFor(() => expect(create).toHaveBeenCalledWith({ name: '我的 MCP', host_ids: ['h1234567'], scopes: ['computer:operate'] }));
    expect(await screen.findByText('chk_one-time-secret')).toBeInTheDocument();
  });

  it('allows a shared Host only when its current permissions include mcp:control', async () => {
    vi.spyOn(portalApi, 'apiKeys').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'sharedHosts').mockResolvedValue({ items: [
      { id: 'h7654321', name: '共享主机', state: 'running', url: '', permissions: ['mcp:control'] },
      { id: 'h8765432', name: '仅桌面主机', state: 'running', url: '', permissions: ['desktop:view'] },
    ] });
    const create = vi.spyOn(portalApi, 'createApiKey').mockResolvedValue({ id: 't7654321', key: 'chk_shared-secret', host_ids: ['h7654321'], scopes: ['computer:operate'], expires_at: new Date(Date.now() + 86400000).toISOString() });
    const user = userEvent.setup();
    renderPage();

    await user.type(await screen.findByLabelText('名称'), '共享电脑 MCP');
    await user.click(screen.getByLabelText(/共享主机/));
    await user.click(screen.getByLabelText(/操作浏览器、终端和文件/));
    await user.click(screen.getByRole('button', { name: '创建 API Key' }));

    await waitFor(() => expect(create).toHaveBeenCalledWith({ name: '共享电脑 MCP', host_ids: ['h7654321'], scopes: ['computer:operate'] }));
    expect(screen.queryByLabelText(/仅桌面主机/)).not.toBeInTheDocument();
  });

  it('shows revoked state and explains that already-issued external actions may finish', async () => {
    const key = {
      id: 't1234567', name: '已吊销密钥', host_ids: ['h1234567'], scopes: ['host:read'],
      expires_at: new Date(Date.now() + 86400000).toISOString(), last_used_at: null, revoked: false, created_at: '',
    };
    vi.spyOn(portalApi, 'apiKeys')
      .mockResolvedValueOnce({ items: [key] } as unknown as Awaited<ReturnType<typeof portalApi.apiKeys>>)
      .mockResolvedValue({ items: [{ ...key, revoked: true }] } as unknown as Awaited<ReturnType<typeof portalApi.apiKeys>>);
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [host] });
    vi.spyOn(portalApi, 'sharedHosts').mockResolvedValue({ items: [] });
    const revoke = vi.spyOn(portalApi, 'deleteApiKey').mockResolvedValue(undefined);
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByRole('button', { name: '吊销 API Key 已吊销密钥' }));
    expect(screen.getByText(/已经发出的外部操作仍可能完成/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '吊销 Key' }));
    await waitFor(() => expect(revoke.mock.calls[0]?.[0]).toBe('t1234567'));
    expect(await screen.findByText('已吊销')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '吊销 API Key 已吊销密钥' })).not.toBeInTheDocument();
  });
});
