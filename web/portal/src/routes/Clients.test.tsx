import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, expect, it, vi } from 'vitest';
import { portalApi } from '../app/api';
import { ToastProvider } from '../app/toast';
import { Clients } from './Clients';

afterEach(() => { cleanup(); vi.restoreAllMocks(); });

it('lists an installation and revokes its linked access', async () => {
  const item = { id: 'cli_12345678', platform: 'desktop', display_name: 'Morde 笔记本', expires_at: '2027-10-22T08:00:00Z', revoked: false, created_at: '2026-10-22T08:00:00Z', last_used_at: null } as const;
  vi.spyOn(portalApi, 'clients').mockResolvedValue({ items: [item as Awaited<ReturnType<typeof portalApi.clients>>['items'][number]] });
  const revoke = vi.spyOn(portalApi, 'revokeClient').mockResolvedValue(undefined);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(<QueryClientProvider client={queryClient}><ToastProvider><MemoryRouter><Clients /></MemoryRouter></ToastProvider></QueryClientProvider>);
  expect(await screen.findByText('Morde 笔记本')).toBeInTheDocument();
  expect(screen.queryByRole('link')).not.toBeInTheDocument();
  await userEvent.setup().click(screen.getByRole('button', { name: '撤销客户端' }));
  expect(within(screen.getByRole('dialog')).getByText(/本机文件不会删除/)).toBeInTheDocument();
  await userEvent.setup().click(within(screen.getByRole('dialog')).getByRole('button', { name: '撤销客户端' }));
  await waitFor(() => expect(revoke.mock.calls[0]?.[0]).toBe(item.id));
});

it('warns when a client authorization expires within seven days', async () => {
  const item = { id: 'cli_87654321', platform: 'ios' as const, display_name: 'iPhone', expires_at: new Date(Date.now() + 6 * 24 * 60 * 60 * 1000).toISOString(), revoked: false, created_at: new Date().toISOString(), last_used_at: null };
  vi.spyOn(portalApi, 'clients').mockResolvedValue({ items: [item as Awaited<ReturnType<typeof portalApi.clients>>['items'][number]] });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={queryClient}><ToastProvider><MemoryRouter><Clients /></MemoryRouter></ToastProvider></QueryClientProvider>);
  expect(await screen.findByText('即将到期')).toBeInTheDocument();
  expect(screen.getByText('登录状态将在 7 天内到期，届时请在此客户端重新登录。')).toBeInTheDocument();
});
