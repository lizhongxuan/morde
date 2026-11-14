import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, expect, it, vi } from 'vitest';
import { portalApi } from '../app/api';
import { ToastNoticeCenter, ToastProvider } from '../app/toast';
import { Clients } from './Clients';

afterEach(() => { cleanup(); vi.restoreAllMocks(); });

it('lists an installation and revokes its linked access', async () => {
  const item = { id: 'cli_12345678', platform: 'desktop', display_name: 'Morde 笔记本', expires_at: '2027-10-22T08:00:00Z', revoked: false, created_at: '2026-10-22T08:00:00Z', last_used_at: null } as const;
  vi.spyOn(portalApi, 'clients').mockResolvedValue({ items: [item as Awaited<ReturnType<typeof portalApi.clients>>['items'][number]] });
  const revoke = vi.spyOn(portalApi, 'revokeClient').mockResolvedValue(undefined);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(<QueryClientProvider client={queryClient}><ToastProvider><ToastNoticeCenter /><MemoryRouter><Clients /></MemoryRouter></ToastProvider></QueryClientProvider>);
  expect(await screen.findByText('Morde 笔记本')).toBeInTheDocument();
  expect(screen.getByRole('link', { name: '下载客户端' })).toHaveAttribute('href', '/download');
  await userEvent.setup().click(screen.getByRole('button', { name: '撤销客户端' }));
  expect(within(screen.getByRole('dialog')).getByText(/本机文件不会删除/)).toBeInTheDocument();
  await userEvent.setup().click(within(screen.getByRole('dialog')).getByRole('button', { name: '撤销客户端' }));
  await waitFor(() => expect(revoke.mock.calls[0]?.[0]).toBe(item.id));
});

it('warns when a client authorization expires within seven days', async () => {
  const item = { id: 'cli_87654321', platform: 'ios' as const, display_name: 'iPhone', expires_at: new Date(Date.now() + 6 * 24 * 60 * 60 * 1000).toISOString(), revoked: false, created_at: new Date().toISOString(), last_used_at: null };
  vi.spyOn(portalApi, 'clients').mockResolvedValue({ items: [item as Awaited<ReturnType<typeof portalApi.clients>>['items'][number]] });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={queryClient}><ToastProvider><ToastNoticeCenter /><MemoryRouter><Clients /></MemoryRouter></ToastProvider></QueryClientProvider>);
  expect(await screen.findByText('即将到期')).toBeInTheDocument();
  expect(screen.getByText('登录状态将在 7 天内到期，届时请在此客户端重新登录。')).toBeInTheDocument();
});

it('keeps client details hidden after an authorization failure and retries the list', async () => {
  const item = { id: 'cli_12345678', platform: 'desktop', display_name: '受限电脑', expires_at: '2027-10-22T08:00:00Z', revoked: false, created_at: '2026-10-22T08:00:00Z', last_used_at: null };
  const list = vi.spyOn(portalApi, 'clients')
    .mockRejectedValueOnce(Object.assign(new Error('当前账号无权读取客户端'), { status: 403 }))
    .mockResolvedValue({ items: [item as Awaited<ReturnType<typeof portalApi.clients>>['items'][number]] });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const user = userEvent.setup();
  render(<QueryClientProvider client={queryClient}><ToastProvider><ToastNoticeCenter /><MemoryRouter><Clients /></MemoryRouter></ToastProvider></QueryClientProvider>);

  expect(await screen.findByRole('alert')).toHaveTextContent('当前账号无权读取客户端');
  expect(screen.queryByText('受限电脑')).not.toBeInTheDocument();
  await user.click(screen.getByRole('button', { name: '重试' }));
  expect(await screen.findByText('受限电脑')).toBeInTheDocument();
  expect(list).toHaveBeenCalledTimes(2);
});

it('paginates logged-in clients and keeps revoke available on later pages', async () => {
  const clients = [
    { id: 'cli_11111111', platform: 'desktop', display_name: '工作电脑', expires_at: '2027-10-22T08:00:00Z', revoked: false, created_at: '2026-10-22T08:00:00Z', last_used_at: null },
    { id: 'cli_22222222', platform: 'ios', display_name: '个人手机', expires_at: '2027-10-22T08:00:00Z', revoked: false, created_at: '2026-10-22T08:00:00Z', last_used_at: null },
  ] as const;
  vi.spyOn(portalApi, 'clients').mockResolvedValue({ items: clients as unknown as Awaited<ReturnType<typeof portalApi.clients>>['items'] });
  const revoke = vi.spyOn(portalApi, 'revokeClient').mockResolvedValue(undefined);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(<QueryClientProvider client={queryClient}><ToastProvider><ToastNoticeCenter /><MemoryRouter><Clients /></MemoryRouter></ToastProvider></QueryClientProvider>);

  expect(await screen.findByText('工作电脑')).toBeVisible();
  expect(screen.getByText('个人手机')).not.toBeVisible();
  await userEvent.setup().click(screen.getByRole('button', { name: '下一页' }));
  expect(screen.getByText('个人手机')).toBeVisible();
  expect(screen.getByText('工作电脑')).not.toBeVisible();

  await userEvent.setup().click(screen.getByRole('button', { name: '撤销客户端' }));
  await userEvent.setup().click(within(screen.getByRole('dialog')).getByRole('button', { name: '撤销客户端' }));
  await waitFor(() => expect(revoke.mock.calls[0]?.[0]).toBe(clients[1].id));
});
