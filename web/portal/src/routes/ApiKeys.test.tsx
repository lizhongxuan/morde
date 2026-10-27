import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { portalApi } from '../app/api';
import { ToastProvider } from '../app/toast';
import { ApiKeys } from './ApiKeys';

const renderPage = () => {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><ApiKeys /></MemoryRouter></ToastProvider></QueryClientProvider>);
};

describe('ApiKeys', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  it('creates a full-access key without host or permission selection', async () => {
    vi.spyOn(portalApi, 'apiKeys').mockResolvedValue({ items: [] });
    const create = vi.spyOn(portalApi, 'createApiKey').mockResolvedValue({ id: 't1234567', key: 'chk_one-time-secret', expires_at: new Date(Date.now() + 86400000).toISOString() });
    const user = userEvent.setup();
    renderPage();

    const submit = await screen.findByRole('button', { name: '创建 API Key' });
    expect(submit).toBeDisabled();
    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument();
    await user.type(screen.getByLabelText('名称'), '我的 MCP');
    expect(submit).toBeEnabled();
    await user.click(submit);

    await waitFor(() => expect(create.mock.calls[0]?.[0]).toEqual({ name: '我的 MCP' }));
    expect(await screen.findByText('chk_one-time-secret')).toBeInTheDocument();
  });

  it('shows revoked state and explains that already-issued external actions may finish', async () => {
    const key = {
      id: 't1234567', name: '已吊销密钥',
      expires_at: new Date(Date.now() + 86400000).toISOString(), last_used_at: null, revoked: false, created_at: '',
    };
    vi.spyOn(portalApi, 'apiKeys')
      .mockResolvedValueOnce({ items: [key] } as Awaited<ReturnType<typeof portalApi.apiKeys>>)
      .mockResolvedValue({ items: [{ ...key, revoked: true }] } as Awaited<ReturnType<typeof portalApi.apiKeys>>);
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
