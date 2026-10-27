import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
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

  it('stores a personal model and never renders the submitted secret again', async () => {
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValueOnce({ items: [] }).mockResolvedValue({ items: [model] });
    const create = vi.spyOn(portalApi, 'createModelConnection').mockResolvedValue(model);
    const user = userEvent.setup();
    renderPage();
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(screen.queryByText('连接 AI 对话工具')).not.toBeInTheDocument();
    await user.click(await screen.findByRole('button', { name: '添加个人模型' }));
    expect(screen.getByRole('dialog', { name: '添加个人模型' })).toBeInTheDocument();
    await user.type(screen.getByLabelText('显示名称'), '我的模型');
    await user.type(screen.getByLabelText('模型名称'), 'morde-test');
    await user.type(screen.getByLabelText('服务地址'), 'https://models.example.test/v1');
    await user.type(screen.getByLabelText('API Key'), 'test-only-secret');
    await user.click(screen.getByRole('button', { name: '保存模型' }));

    await waitFor(() => expect(create).toHaveBeenCalledWith({ name: '我的模型', model_name: 'morde-test', base_url: 'https://models.example.test/v1', api_key: 'test-only-secret', shared: false }));
    expect(await screen.findByText('团队模型')).toBeInTheDocument();
    expect(screen.queryByText('test-only-secret')).not.toBeInTheDocument();
  });

  it('shows server-reported model capabilities after an explicit check', async () => {
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [model] });
    const check = vi.spyOn(portalApi, 'checkModelConnection').mockResolvedValue({ text: true, tools: true, vision: false });
    const user = userEvent.setup();
    renderPage();
    await user.click(await screen.findByRole('button', { name: '检查连接' }));

    await waitFor(() => expect(check).toHaveBeenCalledWith(model.id));
    expect(await screen.findByText(/检查结果：文本可用.*工具调用可用.*视觉不可用/)).toBeInTheDocument();
  });

  it('opens the model form from the Add Model button and closes it without saving', async () => {
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [model] });
    const create = vi.spyOn(portalApi, 'createModelConnection');
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByRole('button', { name: '＋ 添加模型' }));
    expect(screen.getByRole('dialog', { name: '添加个人模型' })).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '取消' }));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(create).not.toHaveBeenCalled();
  });
});
