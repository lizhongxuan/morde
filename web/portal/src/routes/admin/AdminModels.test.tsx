import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { MemoryRouter } from 'react-router';
import { portalApi } from '../../app/api';
import { ToastNoticeCenter, ToastProvider } from '../../app/toast';
import { AdminModels } from './AdminModels';

type ModelConnection = Awaited<ReturnType<typeof portalApi.adminModelConnections>>['items'][number];

const model: ModelConnection = {
  id: 'p1234567', name: '平台主模型', base_url: 'https://models.example.test/v1', model_name: 'gpt-test',
  shared: true, has_api_key: true, capabilities: { text: true, tools: false, vision: false, text_stream: false },
  checked_at: '2026-10-22T08:00:00Z', created_at: '2026-10-20T08:00:00Z', updated_at: '2026-10-22T08:00:00Z',
};

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}><ToastProvider><ToastNoticeCenter /><MemoryRouter><AdminModels /></MemoryRouter></ToastProvider></QueryClientProvider>);
}

describe('admin platform model connections', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  it('fills the GLM platform preset and leaves the API key empty', async () => {
    vi.spyOn(portalApi, 'adminModelConnections').mockResolvedValue({ items: [], configuration_status: 'not_configured' });
    const user = userEvent.setup();
    renderPage();
    await user.click(await screen.findByRole('button', { name: '添加平台模型' }));
    await user.selectOptions(screen.getByLabelText('选择服务和模型'), 'glm-5.1');
    expect(screen.getByLabelText('模型名')).toHaveValue('glm-5.1');
    expect(screen.getByLabelText('服务地址')).toHaveValue('https://api.z.ai/api/paas/v4');
    expect(screen.getByLabelText('API 密钥')).toHaveValue('');
  });

  it('shows a loading state, then lists the shared model and only its key status', async () => {
    let resolveList!: (value: Awaited<ReturnType<typeof portalApi.adminModelConnections>>) => void;
    const pendingList = new Promise<Awaited<ReturnType<typeof portalApi.adminModelConnections>>>((resolve) => { resolveList = resolve; });
    vi.spyOn(portalApi, 'adminModelConnections').mockReturnValue(pendingList);
    renderPage();

    expect(screen.getByRole('status')).toHaveTextContent('正在加载平台模型连接');
    resolveList({ items: [model, { ...model, id: 'p7654321', name: '个人连接', shared: false }], configuration_status: 'configured' });

    const row = await screen.findByRole('row', { name: /平台主模型/ });
    expect(within(row).getByText('已配置')).toBeInTheDocument();
    expect(screen.queryByText('个人连接')).not.toBeInTheDocument();
    expect(screen.queryByText(/sk-|secret/i)).not.toBeInTheDocument();
  });

  it('shows a list error and retries successfully', async () => {
    const user = userEvent.setup();
    const list = vi.spyOn(portalApi, 'adminModelConnections')
      .mockRejectedValueOnce(new Error('读取连接失败'))
      .mockResolvedValue({ items: [model], configuration_status: 'configured' });
    renderPage();

    expect(await screen.findByRole('alert')).toHaveTextContent('读取连接失败');
    await user.click(screen.getByRole('button', { name: '重试' }));
    expect(await screen.findByRole('row', { name: /平台主模型/ })).toBeInTheDocument();
    expect(list).toHaveBeenCalledTimes(2);
  });

  it('shows an empty state and creates a shared connection without exposing the submitted key', async () => {
    const user = userEvent.setup();
    let rows: ModelConnection[] = [];
    vi.spyOn(portalApi, 'adminModelConnections').mockImplementation(async () => ({ items: [...rows], configuration_status: rows.length ? 'configured' : 'not_configured' }));
    const create = vi.spyOn(portalApi, 'createModelConnection').mockImplementation(async (body) => {
      expect(body.shared).toBe(true);
      rows = [{ ...model, name: body.name, base_url: body.base_url, model_name: body.model_name, has_api_key: true }];
      return rows[0]!;
    });
    renderPage();

    expect(await screen.findByRole('heading', { name: '尚未配置平台模型' })).toBeInTheDocument();
    expect(screen.getByText(/选择 GPT 或 DeepSeek 预设/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '添加平台模型' }));
    await user.type(screen.getByLabelText('显示名称'), '新共享模型');
    await user.type(screen.getByLabelText('模型名'), 'vendor-model-1');
    await user.type(screen.getByLabelText('服务地址'), 'https://api.example.test/v1');
    await user.type(screen.getByLabelText('API 密钥'), 'test-only-secret');
    await user.click(screen.getByRole('button', { name: '添加连接' }));

    expect(create).toHaveBeenCalledWith({ name: '新共享模型', base_url: 'https://api.example.test/v1', model_name: 'vendor-model-1', api_key: 'test-only-secret', shared: true });
    expect(await screen.findByRole('row', { name: /新共享模型/ })).toHaveTextContent('已配置');
    expect(screen.queryByText('test-only-secret')).not.toBeInTheDocument();
    expect(screen.queryByDisplayValue('test-only-secret')).not.toBeInTheDocument();
    expect(await screen.findByRole('status')).toHaveTextContent('平台模型连接已添加');
  });

  it('edits a connection with an empty key field and omits the key from the update', async () => {
    const user = userEvent.setup();
    let rows = [model];
    vi.spyOn(portalApi, 'adminModelConnections').mockImplementation(async () => ({ items: [...rows], configuration_status: rows.length ? 'configured' : 'not_configured' }));
    const update = vi.spyOn(portalApi, 'updateModelConnection').mockImplementation(async (id, body) => {
      rows = [{ ...model, ...body }];
      const saved = rows.find((item) => item.id === id);
      if (!saved) throw new Error('model connection not found');
      return saved;
    });
    renderPage();

    const row = await screen.findByRole('row', { name: /平台主模型/ });
    await user.click(within(row).getByRole('button', { name: '编辑' }));
    expect(screen.getByLabelText('API 密钥')).toHaveValue('');
    expect(screen.getByText(/留空则保留已保存的密钥/)).toBeInTheDocument();
    await user.clear(screen.getByLabelText('显示名称'));
    await user.type(screen.getByLabelText('显示名称'), '更新后的主模型');
    await user.click(screen.getByRole('button', { name: '保存连接' }));

    expect(update).toHaveBeenCalledWith(model.id, { name: '更新后的主模型', base_url: model.base_url, model_name: model.model_name });
    expect(await screen.findByRole('row', { name: /更新后的主模型/ })).toHaveTextContent('已配置');
    expect(screen.queryByText('test-only-secret')).not.toBeInTheDocument();
    expect(await screen.findByRole('status')).toHaveTextContent('平台模型连接已保存');
  });

  it('checks connectivity and presents the returned capability results', async () => {
    const user = userEvent.setup();
    vi.spyOn(portalApi, 'adminModelConnections').mockResolvedValue({ items: [model], configuration_status: 'configured' });
    const check = vi.spyOn(portalApi, 'checkModelConnection').mockResolvedValue({ text: true, tools: true, vision: false, text_stream: false });
    renderPage();

    const row = await screen.findByRole('row', { name: /平台主模型/ });
    await user.click(within(row).getByRole('button', { name: '检查连接' }));

    expect(check).toHaveBeenCalledWith(model.id);
    expect(await screen.findByText('本次检查：文本可用 · 工具调用可用 · 视觉未验证')).toBeInTheDocument();
    expect(await screen.findByRole('status')).toHaveTextContent('平台模型连接检查完成');
  });

  it('allows an administrator to check streaming independently', async () => {
    const user = userEvent.setup();
    vi.spyOn(portalApi, 'adminModelConnections').mockResolvedValue({ items: [model], configuration_status: 'configured' });
    const check = vi.spyOn(portalApi, 'checkModelTextStreaming').mockResolvedValue({ ...model.capabilities, text_stream: true });
    renderPage();
    const row = await screen.findByRole('row', { name: /平台主模型/ });
    expect(within(row).getByText(/检查流式回复会发起一次模型调用/)).toBeInTheDocument();
    await user.click(within(row).getByRole('button', { name: '检查流式回复' }));
    expect(check).toHaveBeenCalledWith(model.id);
    expect(await screen.findByRole('status')).toHaveTextContent('模型流式能力已验证');
  });

  it('requires confirmation before deleting a shared connection', async () => {
    const user = userEvent.setup();
    let rows = [model];
    vi.spyOn(portalApi, 'adminModelConnections').mockImplementation(async () => ({ items: [...rows], configuration_status: rows.length ? 'configured' : 'not_configured' }));
    const remove = vi.spyOn(portalApi, 'deleteModelConnection').mockImplementation(async (id) => {
      rows = rows.filter((item) => item.id !== id);
    });
    renderPage();

    const row = await screen.findByRole('row', { name: /平台主模型/ });
    await user.click(within(row).getByRole('button', { name: '删除' }));
    expect(within(row).getByRole('group', { name: '确认删除 平台主模型' })).toBeInTheDocument();
    await user.click(within(row).getByRole('button', { name: '确认删除' }));

    expect(remove).toHaveBeenCalledWith(model.id);
    expect(await screen.findByRole('heading', { name: '尚未配置平台模型' })).toBeInTheDocument();
    expect(await screen.findByRole('status')).toHaveTextContent('平台模型连接已删除');
  });
});
