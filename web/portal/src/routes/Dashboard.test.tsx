import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { components } from '../api/schema';
import { portalApi } from '../app/api';
import { ToastProvider } from '../app/toast';
import { MemoryRouter } from 'react-router';
import { Dashboard } from './Dashboard';

vi.mock('../app/auth', () => ({ useAuth: () => ({ user: { id: 'u1234567' } }) }));

const waitingTask: components['schemas']['TaskSummary'] = {
  id: 'q1234567', owner_id: 'u1234567', host_id: 'h1234567', conversation_id: 'c1234567', model_id: 'model-1',
  objective: '检查部署结果', status: 'waiting_user', current_step: '等待确认',
  budget: { active_seconds: 600, tool_calls: 20, model_tokens: 10000 },
  usage: { active_seconds: 30, tool_calls: 2, model_tokens: null },
  created_at: '2026-10-25T00:00:00Z', updated_at: '2026-10-25T00:01:00Z',
};

function renderDashboard() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Dashboard /></MemoryRouter></ToastProvider></QueryClientProvider>);
}

describe('Dashboard', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  it('highlights a task waiting for the user and links to its details', async () => {
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'workspaces').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'tasks').mockResolvedValue({ items: [waitingTask], next_cursor: null });
    renderDashboard();

    expect(await screen.findByRole('region', { name: '需要你处理的任务' })).toHaveTextContent('检查部署结果');
    expect(screen.getByRole('link', { name: '查看任务' })).toHaveAttribute('href', '/tasks/q1234567');
  });

  it('does not advertise local desktop installation or folder sync as available', async () => {
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'workspaces').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'tasks').mockResolvedValue({ items: [], next_cursor: null });
    renderDashboard();

    expect(await screen.findByText(/本机文件夹同步尚未开放/)).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: /获取 Morde Desktop/ })).not.toBeInTheDocument();
  });
});
