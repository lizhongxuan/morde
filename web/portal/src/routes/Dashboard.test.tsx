import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { portalApi } from '../app/api';
import { ToastProvider } from '../app/toast';
import { MemoryRouter } from 'react-router';
import { Dashboard } from './Dashboard';

vi.mock('../app/auth', () => ({ useAuth: () => ({ user: { id: 'u1234567' } }) }));

function renderDashboard() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Dashboard /></MemoryRouter></ToastProvider></QueryClientProvider>);
}

describe('Dashboard', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  it('offers instant chat without any task links', async () => {
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'workspaces').mockResolvedValue({ items: [] });
    renderDashboard();

    expect(await screen.findByRole('heading', { name: 'AI 助手' })).toBeInTheDocument();
    expect(screen.getAllByRole('link', { name: '开始对话' })[0]).toHaveAttribute('href', '/assistant');
    expect(screen.queryByRole('link', { name: /任务/ })).not.toBeInTheDocument();
  });

  it('distinguishes browser access from PC client folder sync', async () => {
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'workspaces').mockResolvedValue({ items: [] });
    renderDashboard();

    expect(await screen.findByText(/浏览器不绑定本机目录；Morde PC 客户端可在本机同步页配置/)).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: /获取 Morde Desktop/ })).not.toBeInTheDocument();
  });
});
