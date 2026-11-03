import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { portalApi } from '../app/api';
import { ToastProvider } from '../app/toast';
import { Connections } from './Connections';

const timestamp = '2026-10-22T08:00:00.000Z';
const model = {
  id: 'p1234567', name: '团队模型', base_url: 'https://models.example.test/v1', model_name: 'morde-test', shared: true, has_api_key: true,
  capabilities: { text: true, tools: true, vision: false }, checked_at: timestamp, created_at: timestamp, updated_at: timestamp,
} satisfies Awaited<ReturnType<typeof portalApi.modelConnections>>['items'][number];

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Connections /></MemoryRouter></ToastProvider></QueryClientProvider>);
}

describe('Connections', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  it('does not offer personal model credentials and shows the administrator-managed empty state', async () => {
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [] });
    const create = vi.spyOn(portalApi, 'createModelConnection');
    renderPage();
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(await screen.findByText('暂无可用模型')).toBeInTheDocument();
    expect(screen.getByText('请联系管理员配置团队模型。')).toBeInTheDocument();
    expect(screen.queryByLabelText('API Key')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /添加模型|添加个人模型/ })).not.toBeInTheDocument();
    expect(create).not.toHaveBeenCalled();
  });

  it('does not expose legacy personal connections returned by a stale API response', async () => {
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [{ ...model, shared: false, name: '旧个人模型' }] });
    renderPage();
    expect(await screen.findByText('暂无可用模型')).toBeInTheDocument();
    expect(screen.queryByText('旧个人模型')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '检查连接' })).not.toBeInTheDocument();
  });

  it('shows administrator-managed model capabilities without user controls', async () => {
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [model] });
    renderPage();
    expect(await screen.findByRole('article')).toBeInTheDocument();
    expect(screen.getByText('团队共享')).toBeInTheDocument();
    expect(screen.getByText('文本')).toBeInTheDocument();
    expect(screen.getByText('工具调用')).toBeInTheDocument();
    expect(screen.getByText('视觉')).toBeInTheDocument();
    expect(screen.queryByRole('button')).not.toBeInTheDocument();
  });
});
