import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { MemoryRouter } from 'react-router';
import { ApiError } from '../api/client';
import { portalApi } from '../app/api';
import { ToastNoticeCenter, ToastProvider } from '../app/toast';
import { Connections } from './Connections';

const timestamp = '2026-10-22T08:00:00.000Z';
type ModelConnection = Awaited<ReturnType<typeof portalApi.modelConnections>>['items'][number];
type MCPConnector = Awaited<ReturnType<typeof portalApi.mcpConnectors>>['items'][number];
const model: ModelConnection = {
  id: 'p1234567', name: '团队模型', base_url: 'https://models.example.test/v1', model_name: 'morde-test', shared: true, has_api_key: true,
  context_window_tokens: null, context_window_override_tokens: null, context_window_source: 'unknown', context_window_resolved_at: null, context_window_catalog_version: null,
  capabilities: { text: true, tools: true, vision: false, text_stream: false }, checked_at: timestamp, created_at: timestamp, updated_at: timestamp,
};
const mcpConnector: MCPConnector = {
  id: 'k1234567', name: '日历工具', endpoint_url: 'https://mcp.example.test/mcp', enabled: false,
  has_credential: false, credential_version: 0, revision: 1, created_at: timestamp, updated_at: timestamp,
};

function renderPage() {
  if (!vi.isMockFunction(portalApi.mcpConnectors)) vi.spyOn(portalApi, 'mcpConnectors').mockResolvedValue({ items: [] });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return { client, ...render(<QueryClientProvider client={client}><ToastProvider><ToastNoticeCenter autoOpenErrors overlay /><MemoryRouter><Connections /></MemoryRouter></ToastProvider></QueryClientProvider>) };
}

async function selectArea(user: ReturnType<typeof userEvent.setup>, name: '个人模型' | 'MCP 工具') {
  await user.click(screen.getByRole('button', { name: new RegExp(`^${name}`) }));
}

function createOAuthPopup() {
  const popup: { opener: Window | null; closed: boolean; location: { href: string }; close: ReturnType<typeof vi.fn> } = {
    opener: window, closed: false, location: { href: 'about:blank' }, close: vi.fn(),
  };
  popup.close.mockImplementation(() => { popup.closed = true; });
  return popup;
}

describe('Connections', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  it('keeps cached model connections visible and reports a transient refresh failure in a popup', async () => {
    const personalModel = { ...model, shared: false, has_api_key: true };
    const read = vi.spyOn(portalApi, 'modelConnections')
      .mockResolvedValueOnce({ items: [personalModel], configuration_status: 'configured' })
      .mockRejectedValueOnce(new TypeError('Failed to fetch'))
      .mockResolvedValue({ items: [personalModel], configuration_status: 'configured' });
    vi.spyOn(portalApi, 'mcpConnectors').mockResolvedValue({ items: [] });
    const { client } = renderPage();
    expect(await screen.findByRole('heading', { name: personalModel.name, level: 3 })).toBeInTheDocument();

    await client.invalidateQueries({ queryKey: ['model-connections'] });

    const notice = await screen.findByRole('alert');
    expect(notice).toHaveTextContent('模型连接状态读取失败');
    expect(notice.closest('.connections-page')).toBeNull();
    expect(screen.getByRole('heading', { name: personalModel.name, level: 3 })).toBeInTheDocument();
    await userEvent.setup().click(screen.getByRole('button', { name: '重试' }));
    await waitFor(() => expect(read).toHaveBeenCalledTimes(3));
    await waitFor(() => expect(screen.queryByRole('alert')).not.toBeInTheDocument());
  });

  it('keeps the MCP area placeholder and reports an initial network failure in a popup', async () => {
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [], configuration_status: 'not_configured' });
    const connectors = vi.spyOn(portalApi, 'mcpConnectors').mockRejectedValue(new TypeError('Failed to fetch'));
    const user = userEvent.setup();
    renderPage();
    await selectArea(user, 'MCP 工具');

    const notice = await screen.findByRole('alert');
    expect(notice).toHaveTextContent('MCP 工具连接状态读取失败');
    expect(notice.closest('.connections-page')).toBeNull();
    expect(screen.getByRole('status')).toHaveTextContent('正在读取工具连接…');
    expect(screen.queryByText('Failed to fetch')).not.toBeInTheDocument();
    expect(connectors).toHaveBeenCalled();
  });

  it('opens the MCP form in a dialog and clears a cancelled draft', async () => {
    const user = userEvent.setup();
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [], configuration_status: 'not_configured' });
    vi.spyOn(portalApi, 'mcpConnectors').mockResolvedValue({ items: [] });
    renderPage();
    await selectArea(user, 'MCP 工具');

    const open = await screen.findByRole('button', { name: '＋ 添加工具连接' });
    await user.click(open);
    const dialog = screen.getByRole('dialog', { name: '添加工具连接' });
    await user.type(within(dialog).getByLabelText('连接名称'), '待取消的工具');
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog', { name: '添加工具连接' })).not.toBeInTheDocument();
    expect(open).toHaveFocus();

    await user.click(open);
    expect(within(screen.getByRole('dialog', { name: '添加工具连接' })).getByLabelText('连接名称')).toHaveValue('');
  });

  it('keeps the MCP dialog open while a save is in progress', async () => {
    const user = userEvent.setup();
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [], configuration_status: 'not_configured' });
    vi.spyOn(portalApi, 'mcpConnectors').mockResolvedValue({ items: [] });
    let finishCreate!: (connector: MCPConnector) => void;
    vi.spyOn(portalApi, 'createMCPConnector').mockReturnValue(new Promise((resolve) => { finishCreate = resolve; }));
    renderPage();
    await selectArea(user, 'MCP 工具');
    await user.click(await screen.findByRole('button', { name: '＋ 添加工具连接' }));
    const dialog = screen.getByRole('dialog', { name: '添加工具连接' });
    await user.type(within(dialog).getByLabelText('连接名称'), '本地测试');
    await user.type(within(dialog).getByLabelText('MCP 服务地址'), 'https://mcp.example.test/mcp');
    await user.click(within(dialog).getByRole('button', { name: '保存连接' }));
    expect(within(dialog).getByRole('button', { name: '正在保存…' })).toBeDisabled();
    await user.keyboard('{Escape}');
    expect(screen.getByRole('dialog', { name: '添加工具连接' })).toBeInTheDocument();

    finishCreate(mcpConnector);
    await waitFor(() => expect(screen.queryByRole('dialog', { name: '添加工具连接' })).not.toBeInTheDocument());
  });

  it('creates and enables an owner-scoped MCP connector without keeping its credential in the form', async () => {
    const user = userEvent.setup();
    let rows: MCPConnector[] = [];
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [], configuration_status: 'not_configured' });
    vi.spyOn(portalApi, 'mcpConnectors').mockImplementation(async () => ({ items: [...rows] }));
    const create = vi.spyOn(portalApi, 'createMCPConnector').mockImplementation(async (body) => {
      const created = { ...mcpConnector, name: body.name, endpoint_url: body.endpoint_url, has_credential: Boolean(body.credential), credential_version: body.credential ? 1 : 0 };
      rows = [created];
      return created;
    });
    const enable = vi.spyOn(portalApi, 'setMCPConnectorEnabled').mockImplementation(async (_id, body) => {
      rows = rows.map((item) => ({ ...item, enabled: body.enabled ?? false, revision: item.revision + 1 }));
      return rows[0]!;
    });
    renderPage();
    await selectArea(user, 'MCP 工具');

    await user.click(await screen.findByRole('button', { name: '＋ 添加工具连接' }));
    await user.type(screen.getByLabelText('连接名称'), '日历工具');
    await user.type(screen.getByLabelText('MCP 服务地址'), 'https://mcp.example.test/mcp');
    const secret = '{"kind":"headers","headers":{"Authorization":"Bearer local-test-secret"}}';
    fireEvent.change(screen.getByLabelText('认证凭据 JSON（可选）'), { target: { value: secret } });
    await user.click(screen.getByRole('button', { name: '保存连接' }));

    await waitFor(() => expect(create).toHaveBeenCalledWith({ name: '日历工具', endpoint_url: 'https://mcp.example.test/mcp', credential: JSON.parse(secret) }));
    const card = (await screen.findByRole('heading', { name: '日历工具', level: 3 })).closest('article')!;
    expect(screen.queryByDisplayValue(secret)).not.toBeInTheDocument();
    expect(screen.queryByText('local-test-secret')).not.toBeInTheDocument();
    await user.click(within(card).getByRole('button', { name: '启用' }));
    await waitFor(() => expect(enable).toHaveBeenCalledWith(mcpConnector.id, { enabled: true, expected_revision: 1 }));
    expect(await screen.findByText('已启用')).toBeInTheDocument();
  });

  it('does not display an echoed secret when an MCP credential save fails', async () => {
    const user = userEvent.setup();
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [], configuration_status: 'not_configured' });
    vi.spyOn(portalApi, 'mcpConnectors').mockResolvedValue({ items: [] });
    const secret = 'local-test-secret';
    vi.spyOn(portalApi, 'createMCPConnector').mockRejectedValue(new ApiError({
      code: 'internal', status: 500, message: `could not save Bearer ${secret}`,
    }));
    renderPage();
    await selectArea(user, 'MCP 工具');

    await user.click(await screen.findByRole('button', { name: '＋ 添加工具连接' }));
    await user.type(screen.getByLabelText('连接名称'), '日历工具');
    await user.type(screen.getByLabelText('MCP 服务地址'), 'https://mcp.example.test/mcp');
    fireEvent.change(screen.getByLabelText('认证凭据 JSON（可选）'), { target: { value: `{"kind":"headers","headers":{"Authorization":"Bearer ${secret}"}}` } });
    await user.click(screen.getByRole('button', { name: '保存连接' }));

    expect(await screen.findByRole('alert')).toHaveTextContent('服务暂时不可用，请稍后重试');
    expect(screen.queryByText(secret)).not.toBeInTheDocument();
    expect(screen.queryByDisplayValue(new RegExp(secret))).not.toBeInTheDocument();
  });

  it('confirms connector deletion and refreshes the MCP list', async () => {
    const user = userEvent.setup();
    let rows = [mcpConnector];
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [], configuration_status: 'not_configured' });
    vi.spyOn(portalApi, 'mcpConnectors').mockImplementation(async () => ({ items: [...rows] }));
    const remove = vi.spyOn(portalApi, 'deleteMCPConnector').mockImplementation(async (id) => {
      rows = rows.filter((connector) => connector.id !== id);
    });
    renderPage();
    await selectArea(user, 'MCP 工具');

    const card = (await screen.findByRole('heading', { name: mcpConnector.name, level: 3 })).closest('article')!;
    await user.click(within(card).getByRole('button', { name: '删除' }));
    const dialog = screen.getByRole('dialog');
    expect(dialog).toHaveTextContent('正在运行的任务不受影响');
    await user.click(within(dialog).getByRole('button', { name: '删除连接' }));

    await waitFor(() => expect(remove).toHaveBeenCalledWith(mcpConnector.id, mcpConnector.revision));
    await waitFor(() => expect(screen.queryByRole('heading', { name: mcpConnector.name, level: 3 })).not.toBeInTheDocument());
  });

  it('adds the official GitHub MCP preset and clears the token after saving', async () => {
    const user = userEvent.setup();
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [], configuration_status: 'not_configured' });
    vi.spyOn(portalApi, 'mcpConnectors').mockResolvedValue({ items: [] });
    const create = vi.spyOn(portalApi, 'createMCPConnector').mockImplementation(async (body) => ({
      ...mcpConnector, name: body.name, endpoint_url: body.endpoint_url, has_credential: Boolean(body.credential), credential_version: body.credential ? 1 : 0,
    }));
    renderPage();
    await selectArea(user, 'MCP 工具');

    await user.click(await screen.findByRole('button', { name: '＋ 添加工具连接' }));
    await user.selectOptions(screen.getByLabelText('连接模板'), 'github');
    expect(screen.getByLabelText('连接名称')).toHaveValue('GitHub');
    expect(screen.getByLabelText('MCP 服务地址')).toHaveValue('https://api.githubcopilot.com/mcp/');
    expect(screen.getByLabelText('MCP 服务地址')).toHaveAttribute('readonly');
    const token = 'ghp-local-synthetic-token';
    fireEvent.change(screen.getByLabelText('GitHub Personal Access Token（可选）'), { target: { value: token } });
    await user.click(screen.getByRole('button', { name: '保存连接' }));

    await waitFor(() => expect(create).toHaveBeenCalledWith({
      name: 'GitHub', endpoint_url: 'https://api.githubcopilot.com/mcp/',
      credential: { kind: 'headers', headers: { Authorization: `Bearer ${token}` } },
    }));
    expect(screen.queryByDisplayValue(token)).not.toBeInTheDocument();
    expect(screen.queryByText(token)).not.toBeInTheDocument();
  });

  it('creates official Google Workspace MCP presets without asking the user to paste an access token', async () => {
    const user = userEvent.setup();
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [], configuration_status: 'not_configured' });
    vi.spyOn(portalApi, 'mcpConnectors').mockResolvedValue({ items: [] });
    const create = vi.spyOn(portalApi, 'createMCPConnector').mockResolvedValue({
      ...mcpConnector, name: 'Gmail', endpoint_url: 'https://gmailmcp.googleapis.com/mcp/v1',
    });
    renderPage();
    await selectArea(user, 'MCP 工具');

    await user.click(await screen.findByRole('button', { name: '＋ 添加工具连接' }));
    await user.selectOptions(screen.getByLabelText('连接模板'), 'google_gmail');
    expect(screen.getByLabelText('连接名称')).toHaveValue('Gmail');
    expect(screen.getByLabelText('MCP 服务地址')).toHaveValue('https://gmailmcp.googleapis.com/mcp/v1');
    expect(screen.getByLabelText('MCP 服务地址')).toHaveAttribute('readonly');
    expect(screen.getByRole('link', { name: '查看 Google 配置说明' })).toHaveAttribute(
      'href', 'https://developers.google.com/workspace/guides/configure-mcp-servers');
    expect(screen.queryByLabelText(/认证凭据 JSON/)).not.toBeInTheDocument();
    expect(screen.getByText(/Morde 不要求你粘贴 Google 访问令牌/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '保存连接' }));

    await waitFor(() => expect(create).toHaveBeenCalledWith({
      name: 'Gmail', endpoint_url: 'https://gmailmcp.googleapis.com/mcp/v1',
    }));
  });

  it('opens OAuth separately and refreshes the connector after the provider callback completes', async () => {
    const user = userEvent.setup();
    let rows = [mcpConnector];
    const list = vi.spyOn(portalApi, 'mcpConnectors').mockImplementation(async () => ({ items: [...rows] }));
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [], configuration_status: 'not_configured' });
    const begin = vi.spyOn(portalApi, 'beginMCPConnectorOAuth').mockResolvedValue({
      kind: 'authorize', authorization_url: 'https://auth.example.test/authorize?state=opaque&code_challenge=challenge',
      redirect_uri: 'https://morde.example.test/api/mcp/oauth/callback',
    });
    const popup = createOAuthPopup();
    const open = vi.spyOn(window, 'open').mockReturnValue(popup as unknown as Window);
    renderPage();
    await selectArea(user, 'MCP 工具');

    const card = (await screen.findByRole('heading', { name: '日历工具', level: 3 })).closest('article')!;
    await user.click(within(card).getByRole('button', { name: 'OAuth 授权' }));
    await waitFor(() => expect(begin).toHaveBeenCalledWith(mcpConnector.id, { expected_revision: 1 }));
    await waitFor(() => expect(popup.location.href).toContain('https://auth.example.test/authorize?state=opaque'));
    expect(open).toHaveBeenCalledWith('about:blank', 'morde-mcp-oauth');
    expect(popup.opener).toBeNull();
    expect(await screen.findByText(/正在等待 日历工具 的 OAuth 授权/)).toBeInTheDocument();

    popup.location.href = 'https://morde.example.test/api/mcp/oauth/callback?state=opaque';
    rows = [{ ...mcpConnector, has_credential: true, credential_version: 1, revision: 2 }];
    fireEvent.focus(window);
    expect(await screen.findByText('MCP OAuth 授权完成，凭据已由 Morde 安全保存。')).toBeInTheDocument();
    expect(popup.close).toHaveBeenCalled();
    expect(list).toHaveBeenCalled();
    expect(screen.queryByText(/access_token|refresh_token/)).not.toBeInTheDocument();
  });

  it('supports manually supplied OAuth client details and clears the client secret after submitting', async () => {
    const user = userEvent.setup();
    vi.spyOn(portalApi, 'mcpConnectors').mockResolvedValue({ items: [mcpConnector] });
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [], configuration_status: 'not_configured' });
    const begin = vi.spyOn(portalApi, 'beginMCPConnectorOAuth')
      .mockResolvedValueOnce({
        kind: 'client_required', developer_url: 'https://provider.example.test/developers',
        redirect_uri: 'https://morde.example.test/api/mcp/oauth/callback',
      })
      .mockResolvedValueOnce({
        kind: 'authorize', authorization_url: 'https://auth.example.test/authorize?state=opaque',
        redirect_uri: 'https://morde.example.test/api/mcp/oauth/callback',
      });
    const firstPopup = createOAuthPopup();
    const secondPopup = createOAuthPopup();
    vi.spyOn(window, 'open').mockReturnValueOnce(firstPopup as unknown as Window).mockReturnValueOnce(secondPopup as unknown as Window);
    renderPage();
    await selectArea(user, 'MCP 工具');

    const card = (await screen.findByRole('heading', { name: '日历工具', level: 3 })).closest('article')!;
    await user.click(within(card).getByRole('button', { name: 'OAuth 授权' }));
    const form = await screen.findByRole('form', { name: '配置 日历工具 OAuth 客户端' });
    expect(firstPopup.close).toHaveBeenCalled();
    expect(within(form).getByRole('link', { name: '服务商开发者页面' })).toHaveAttribute('href', 'https://provider.example.test/developers');
    expect(within(form).getByText('https://morde.example.test/api/mcp/oauth/callback')).toBeInTheDocument();

    await user.type(within(form).getByLabelText('OAuth 客户端 ID'), 'oauth-client-id');
    await user.type(within(form).getByLabelText('OAuth 客户端密钥（可选）'), 'oauth-client-secret');
    await user.click(within(form).getByRole('button', { name: '继续授权' }));
    await waitFor(() => expect(begin).toHaveBeenCalledTimes(2));
    expect(begin).toHaveBeenLastCalledWith(mcpConnector.id, {
      expected_revision: 1, client_id: 'oauth-client-id', client_secret: 'oauth-client-secret',
    });
    await waitFor(() => expect(secondPopup.location.href).toBe('https://auth.example.test/authorize?state=opaque'));
    expect(screen.queryByDisplayValue('oauth-client-secret')).not.toBeInTheDocument();
    expect(secondPopup.location.href).not.toContain('oauth-client-secret');
    expect(screen.queryByText('oauth-client-secret')).not.toBeInTheDocument();
  });

  it('cancels OAuth client setup without sending client credentials', async () => {
    const user = userEvent.setup();
    vi.spyOn(portalApi, 'mcpConnectors').mockResolvedValue({ items: [mcpConnector] });
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [], configuration_status: 'not_configured' });
    const begin = vi.spyOn(portalApi, 'beginMCPConnectorOAuth').mockResolvedValue({
      kind: 'client_required', developer_url: 'https://provider.example.test/developers',
      redirect_uri: 'https://morde.example.test/api/mcp/oauth/callback',
    });
    const popup = createOAuthPopup();
    vi.spyOn(window, 'open').mockReturnValue(popup as unknown as Window);
    renderPage();
    await selectArea(user, 'MCP 工具');

    const card = (await screen.findByRole('heading', { name: '日历工具', level: 3 })).closest('article')!;
    await user.click(within(card).getByRole('button', { name: 'OAuth 授权' }));
    await screen.findByRole('form', { name: '配置 日历工具 OAuth 客户端' });
    await user.click(screen.getByRole('button', { name: '取消' }));

    expect(screen.queryByRole('form', { name: '配置 日历工具 OAuth 客户端' })).not.toBeInTheDocument();
    expect(begin).toHaveBeenCalledTimes(1);
    expect(begin).toHaveBeenCalledWith(mcpConnector.id, { expected_revision: 1 });
    expect(popup.close).toHaveBeenCalled();
  });

  it('reports a closed OAuth window as cancelled when no new credentials were saved', async () => {
    const user = userEvent.setup();
    vi.spyOn(portalApi, 'mcpConnectors').mockResolvedValue({ items: [mcpConnector] });
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [], configuration_status: 'not_configured' });
    vi.spyOn(portalApi, 'beginMCPConnectorOAuth').mockResolvedValue({
      kind: 'authorize', authorization_url: 'https://auth.example.test/authorize?state=opaque',
      redirect_uri: 'https://morde.example.test/api/mcp/oauth/callback',
    });
    const popup = createOAuthPopup();
    vi.spyOn(window, 'open').mockReturnValue(popup as unknown as Window);
    renderPage();
    await selectArea(user, 'MCP 工具');

    const card = (await screen.findByRole('heading', { name: '日历工具', level: 3 })).closest('article')!;
    await user.click(within(card).getByRole('button', { name: 'OAuth 授权' }));
    await screen.findByText(/正在等待 日历工具 的 OAuth 授权/);
    popup.closed = true;
    fireEvent.focus(window);

    expect(await screen.findByText('OAuth 授权未完成或已取消；连接状态已刷新。')).toBeInTheDocument();
  });

  it('fills the GLM preset without storing or prefilling a key', async () => {
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [], configuration_status: 'not_configured' });
    const user = userEvent.setup();
    renderPage();
    await user.click(await screen.findByRole('button', { name: /添加个人模型/ }));
    await user.selectOptions(screen.getByLabelText('选择服务和模型'), 'glm-5.1');
    expect(screen.getByLabelText('模型名')).toHaveValue('glm-5.1');
    expect(screen.getByLabelText('服务地址')).toHaveValue('https://api.z.ai/api/paas/v4');
    expect(screen.getByLabelText('API 密钥')).toHaveValue('');
  });

  it('omits the page title and shared models while listing personal models', async () => {
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [model, { ...model, id: 'p7654321', name: '我的私有模型', shared: false }], configuration_status: 'configured' });
    renderPage();

    expect(screen.queryByRole('heading', { name: '模型与工具', level: 1 })).not.toBeInTheDocument();
    expect(screen.queryByText('管理你的模型连接与可用工具。')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '团队模型' })).not.toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: '团队模型', level: 3 })).not.toBeInTheDocument();
    const personal = (await screen.findByText('我的私有模型')).closest('article')!;
    expect(within(personal).getByText('检查通过')).toBeInTheDocument();
    expect(within(personal).getByRole('button', { name: '检查连接' })).toBeInTheDocument();
  });

  it('paginates MCP connectors while retaining their accessible actions', async () => {
    const user = userEvent.setup();
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [], configuration_status: 'not_configured' });
    vi.spyOn(portalApi, 'mcpConnectors').mockResolvedValue({ items: [mcpConnector, { ...mcpConnector, id: 'k7654321', name: '第二个工具' }] });
    renderPage();
    await selectArea(user, 'MCP 工具');

    expect(await screen.findByRole('heading', { name: '日历工具', level: 3 })).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: '第二个工具', level: 3 })).not.toBeInTheDocument();
    const pagination = screen.getByRole('navigation', { name: 'MCP 工具连接列表分页' });
    await user.click(within(pagination).getByRole('button', { name: '下一页' }));

    const nextConnector = await screen.findByRole('heading', { name: '第二个工具', level: 3 });
    expect(screen.queryByRole('heading', { name: '日历工具', level: 3 })).not.toBeInTheDocument();
    expect(within(nextConnector.closest<HTMLElement>('article')!).getByRole('button', { name: '启用' })).toBeInTheDocument();
  });

  it('paginates personal models and keeps row actions usable', async () => {
    const user = userEvent.setup();
    const personal = { ...model, shared: false, checked_at: null };
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [
      { ...personal, name: '私有模型一' },
      { ...personal, id: 'p2234567', name: '私有模型二' },
    ], configuration_status: 'configured' });
    const check = vi.spyOn(portalApi, 'checkModelConnection').mockResolvedValue({ text: true, tools: true, vision: false, text_stream: false });
    renderPage();

    expect(await screen.findByRole('heading', { name: '私有模型一', level: 3 })).toBeInTheDocument();
    const pagination = screen.getByRole('navigation', { name: '个人模型分页' });
    await user.click(within(pagination).getByRole('button', { name: '下一页' }));
    const nextModel = await screen.findByRole('heading', { name: '私有模型二', level: 3 });
    expect(screen.queryByRole('heading', { name: '私有模型一', level: 3 })).not.toBeInTheDocument();
    await user.click(within(nextModel.closest<HTMLElement>('article')!).getByRole('button', { name: '检查连接' }));
    expect(check).toHaveBeenCalledWith('p2234567');
  });

  it('keeps unfinished model and MCP form drafts when switching areas', async () => {
    const user = userEvent.setup();
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [], configuration_status: 'not_configured' });
    vi.spyOn(portalApi, 'mcpConnectors').mockResolvedValue({ items: [] });
    renderPage();

    await user.click(await screen.findByRole('button', { name: /添加个人模型/ }));
    await user.selectOptions(screen.getByLabelText('选择服务和模型'), 'custom');
    await user.type(screen.getByLabelText('显示名称'), '待保存模型');
    await user.type(screen.getByLabelText('服务地址'), 'https://draft.example.test/v1');
    await selectArea(user, 'MCP 工具');
    await user.click(await screen.findByRole('button', { name: '＋ 添加工具连接' }));
    await user.type(screen.getByLabelText('连接名称'), '待保存工具');
    await user.type(screen.getByLabelText('MCP 服务地址'), 'https://draft.example.test/mcp');
    await selectArea(user, '个人模型');
    expect(screen.getByLabelText('显示名称')).toHaveValue('待保存模型');
    expect(screen.getByLabelText('服务地址')).toHaveValue('https://draft.example.test/v1');
    await selectArea(user, 'MCP 工具');
    expect(screen.getByLabelText('连接名称')).toHaveValue('待保存工具');
    expect(screen.getByLabelText('MCP 服务地址')).toHaveValue('https://draft.example.test/mcp');
  });

  it('keeps connection query failures on the selected page and retries in place', async () => {
    const user = userEvent.setup();
    let attempts = 0;
    vi.spyOn(portalApi, 'modelConnections').mockImplementation(async () => {
      attempts += 1;
      if (attempts === 1) throw new Error('模型服务暂时不可用');
      return { items: [], configuration_status: 'not_configured' };
    });
    renderPage();

    expect(await screen.findByRole('alert')).toHaveTextContent('模型服务暂时不可用');
    await user.click(screen.getByRole('button', { name: '重试' }));
    await waitFor(() => expect(screen.queryByRole('alert')).not.toBeInTheDocument());
    expect(await screen.findByText('尚未添加个人模型')).toBeInTheDocument();
    expect(attempts).toBe(2);
  });

  it('shows MCP query failures inside the MCP area and restores the list on retry', async () => {
    const user = userEvent.setup();
    let attempts = 0;
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [], configuration_status: 'not_configured' });
    vi.spyOn(portalApi, 'mcpConnectors').mockImplementation(async () => {
      attempts += 1;
      if (attempts === 1) throw new Error('MCP 服务暂时不可用');
      return { items: [mcpConnector] };
    });
    renderPage();
    await selectArea(user, 'MCP 工具');

    expect(await screen.findByRole('alert')).toHaveTextContent('MCP 服务暂时不可用');
    await user.click(screen.getByRole('button', { name: '重试' }));
    expect(await screen.findByRole('heading', { name: '日历工具', level: 3 })).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(attempts).toBe(2);
  });

  it('renders unsafe and overlong connection addresses safely', async () => {
    const user = userEvent.setup();
    const longAddress = `https://mcp.example.test/${'segment'.repeat(30)}?token=secret-only-in-url`;
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({
      items: [{ ...model, shared: false, base_url: 'javascript:alert(1)' }], configuration_status: 'configured',
    });
    vi.spyOn(portalApi, 'mcpConnectors').mockResolvedValue({ items: [{ ...mcpConnector, endpoint_url: longAddress }] });
    renderPage();

    const unsafeModel = (await screen.findByRole('heading', { name: model.name, level: 3 })).closest('article')!;
    expect(within(unsafeModel).getByText(/服务地址待核对/, { selector: 'p' })).toBeInTheDocument();
    await selectArea(user, 'MCP 工具');
    const connector = await screen.findByRole('heading', { name: '日历工具', level: 3 });
    const address = within(connector.closest<HTMLElement>('article')!).getByText(/mcp\.example\.test/);
    expect(address.textContent?.length).toBeLessThan(110);
    expect(address).not.toHaveAttribute('title', expect.stringContaining('secret-only-in-url'));
    expect(screen.queryByText(/secret-only-in-url/)).not.toBeInTheDocument();
  });

  it('explains a model budget failure without referring to host quotas', async () => {
    const user = userEvent.setup();
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [{ ...model, shared: false }], configuration_status: 'configured' });
    vi.spyOn(portalApi, 'checkModelConnection').mockRejectedValue(new ApiError({ code: 'quota_exceeded', status: 409, message: 'quota exceeded' }));
    renderPage();

    await user.click(await screen.findByRole('button', { name: '检查连接' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('模型调用额度不足或尚未配置');
  });

  it('checks streaming separately and explains that the check consumes model usage', async () => {
    const user = userEvent.setup();
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [{ ...model, shared: false }], configuration_status: 'configured' });
    const check = vi.spyOn(portalApi, 'checkModelTextStreaming').mockResolvedValue({ ...model.capabilities, text_stream: true });
    renderPage();
    const modelCard = (await screen.findByRole('heading', { name: model.name, level: 3 })).closest('article')!;
    expect(within(modelCard).getByRole('button', { name: '检查流式回复' })).toHaveAttribute('title', '检查流式回复会产生一次模型调用');
    await user.click(within(modelCard).getByRole('button', { name: '检查流式回复' }));
    expect(check).toHaveBeenCalledWith(model.id);
    expect(await screen.findByRole('status')).toHaveTextContent('模型流式能力已验证');
  });

  it('creates a private connection, clears the secret field and automatically checks the saved model', async () => {
    const user = userEvent.setup();
    const personal: ModelConnection = { ...model, id: 'p7654321', name: '我的模型', model_name: 'private-model', shared: false, checked_at: null, capabilities: { text: false, tools: false, vision: false, text_stream: false } };
    let rows: ModelConnection[] = [];
    vi.spyOn(portalApi, 'modelConnections').mockImplementation(async () => ({ items: [...rows], configuration_status: rows.length ? 'configured' : 'not_configured' }));
    const create = vi.spyOn(portalApi, 'createModelConnection').mockImplementation(async (body) => {
      expect(body.shared).toBe(false);
      rows = [{ ...personal, name: body.name, base_url: body.base_url, model_name: body.model_name, has_api_key: true }];
      return rows[0]!;
    });
    const check = vi.spyOn(portalApi, 'checkModelConnection').mockImplementation(async () => {
      rows = rows.map((item) => ({ ...item, checked_at: timestamp, capabilities: { text: true, tools: true, vision: false, text_stream: false } }));
      return { text: true, tools: true, vision: false, text_stream: false };
    });
    renderPage();

    await waitFor(() => expect(screen.queryByText('正在读取模型连接…')).not.toBeInTheDocument());
    expect(screen.getByText('尚未添加个人模型')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: /添加个人模型/ }));
    const dialog = screen.getByRole('dialog', { name: '添加个人模型' });
    expect(dialog.querySelector('form.connection-form')).not.toBeNull();
    await user.selectOptions(screen.getByLabelText('选择服务和模型'), 'custom');
    await user.type(screen.getByLabelText('显示名称'), '我的模型');
    await user.type(screen.getByLabelText('模型名'), 'private-model');
    await user.type(screen.getByLabelText('服务地址'), 'https://api.example.test/v1');
    await user.type(screen.getByLabelText('上下文窗口高级覆盖（tokens，可选）'), '32768');
    await user.type(screen.getByLabelText('API 密钥'), 'private-test-secret');
    expect(screen.getByRole('button', { name: '保存并检查连接' }).closest('form')).toBeValid();
    await user.click(screen.getByRole('button', { name: '保存并检查连接' }));

    expect(create).toHaveBeenCalledWith({ name: '我的模型', base_url: 'https://api.example.test/v1', model_name: 'private-model', context_window_override_tokens: 32768, api_key: 'private-test-secret', shared: false });
    const card = (await screen.findByRole('heading', { name: '我的模型', level: 3 })).closest('article')!;
    expect(check).toHaveBeenCalledWith(personal.id);
    expect(await within(card).findByText('本次检查：文本可用 · 工具调用可用 · 视觉未验证')).toBeInTheDocument();
    expect(screen.queryByDisplayValue('private-test-secret')).not.toBeInTheDocument();
    expect(screen.queryByText('private-test-secret')).not.toBeInTheDocument();

    expect(screen.queryByText('private-test-secret')).not.toBeInTheDocument();
  });

  it('refreshes the model list after an uncertain save failure to avoid duplicate submissions', async () => {
    const user = userEvent.setup();
    const personal: ModelConnection = { ...model, id: 'p7654321', name: 'DeepSeek Flash', shared: false, checked_at: null };
    let rows: ModelConnection[] = [];
    const list = vi.spyOn(portalApi, 'modelConnections').mockImplementation(async () => ({
      items: [...rows], configuration_status: rows.length ? 'configured' : 'not_configured',
    }));
    vi.spyOn(portalApi, 'createModelConnection').mockImplementation(async () => {
      rows = [{ ...personal, has_api_key: true }];
      throw new ApiError({ code: 'internal', status: 503, message: 'service unavailable' });
    });
    renderPage();

    await user.click(await screen.findByRole('button', { name: /添加个人模型/ }));
    await user.selectOptions(screen.getByLabelText('选择服务和模型'), 'deepseek-flash');
    expect(screen.queryByLabelText('上下文窗口高级覆盖（tokens，可选）')).not.toBeInTheDocument();
    expect(screen.getByText('上下文窗口由 Morde 自动识别；无需手动填写。')).toBeInTheDocument();
    await user.type(screen.getByLabelText('API 密钥'), 'private-test-secret');
    await user.click(screen.getByRole('button', { name: '保存并检查连接' }));

    expect(await screen.findByRole('alert')).toHaveTextContent('保存结果尚未确认，请先核对模型列表');
    expect(screen.getByRole('alert')).not.toHaveTextContent('请重新输入后重试');
    expect(screen.getByLabelText('API 密钥')).toHaveValue('');
    expect(await screen.findByRole('heading', { name: 'DeepSeek Flash', level: 3 })).toBeInTheDocument();
    expect(list).toHaveBeenCalledTimes(2);
    expect(screen.queryByText('private-test-secret')).not.toBeInTheDocument();
  });

  it('edits an existing personal model without exposing or replacing its saved key unless requested', async () => {
    const user = userEvent.setup();
    const personal: ModelConnection = { ...model, id: 'p7654321', name: '我的模型', shared: false };
    let rows = [personal];
    vi.spyOn(portalApi, 'modelConnections').mockImplementation(async () => ({ items: [...rows], configuration_status: rows.length ? 'configured' : 'not_configured' }));
    const update = vi.spyOn(portalApi, 'updateModelConnection').mockImplementation(async (_id, body) => {
      rows = [{ ...personal, name: body.name, base_url: body.base_url, model_name: body.model_name }];
      return rows[0]!;
    });
    renderPage();

    const card = (await screen.findByRole('heading', { name: '我的模型', level: 3 })).closest('article')!;
    await user.click(within(card).getByRole('button', { name: '编辑' }));
    expect(screen.getByRole('dialog', { name: '编辑个人模型' })).toBeInTheDocument();
    expect(screen.getByLabelText('API 密钥')).toHaveValue('');
    expect(screen.getByLabelText('API 密钥')).not.toBeRequired();
    await user.clear(screen.getByLabelText('显示名称'));
    await user.type(screen.getByLabelText('显示名称'), '已修改模型');
    await user.click(screen.getByRole('button', { name: '保存修改' }));
    expect(update).toHaveBeenCalledWith(personal.id, { name: '已修改模型', base_url: personal.base_url, model_name: personal.model_name });

    const updatedCard = (await screen.findByRole('heading', { name: '已修改模型', level: 3 })).closest('article')!;
    await user.click(within(updatedCard).getByRole('button', { name: '编辑' }));
    fireEvent.change(screen.getByLabelText('API 密钥'), { target: { value: '  new-test-secret  ' } });
    await user.click(screen.getByRole('button', { name: '保存修改' }));
    expect(update).toHaveBeenLastCalledWith(personal.id, { name: '已修改模型', base_url: personal.base_url, model_name: personal.model_name, api_key: 'new-test-secret' });
    expect(screen.queryByDisplayValue('new-test-secret')).not.toBeInTheDocument();
  });

  it('rejects non-HTTPS URLs in the form before sending credentials', async () => {
    const user = userEvent.setup();
    const create = vi.spyOn(portalApi, 'createModelConnection');
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [], configuration_status: 'not_configured' });
    renderPage();

    await user.click(await screen.findByRole('button', { name: /添加个人模型/ }));
    await user.selectOptions(screen.getByLabelText('选择服务和模型'), 'custom');
    await user.type(screen.getByLabelText('显示名称'), '坏地址模型');
    await user.type(screen.getByLabelText('模型名'), 'private-model');
    await user.type(screen.getByLabelText('服务地址'), 'http://not-allowed.example.test/v1');
    await user.type(screen.getByLabelText('API 密钥'), 'must-not-send-secret');
    await user.click(screen.getByRole('button', { name: '保存并检查连接' }));

    expect(create).not.toHaveBeenCalled();
    expect(screen.getByLabelText('服务地址')).toBeInvalid();
  });

  it('surfaces server allowlist failures without displaying the submitted secret', async () => {
    const user = userEvent.setup();
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [], configuration_status: 'not_configured' });
    const create = vi.spyOn(portalApi, 'createModelConnection').mockRejectedValue(new Error('服务地址不在当前域名 allowlist 中'));
    renderPage();

    await user.click(await screen.findByRole('button', { name: /添加个人模型/ }));
    await user.selectOptions(screen.getByLabelText('选择服务和模型'), 'custom');
    await user.type(screen.getByLabelText('显示名称'), '未允许模型');
    await user.type(screen.getByLabelText('模型名'), 'private-model');
    await user.type(screen.getByLabelText('服务地址'), 'https://not-allowed.example.test/v1');
    await user.type(screen.getByLabelText('API 密钥'), 'private-test-secret');
    expect(screen.getByRole('button', { name: '保存并检查连接' }).closest('form')).toBeValid();
    await user.click(screen.getByRole('button', { name: '保存并检查连接' }));

    expect(create).toHaveBeenCalledOnce();
    expect(await screen.findByRole('alert')).toHaveTextContent('服务地址不在当前域名 allowlist 中');
    expect(screen.getByLabelText('API 密钥')).toHaveValue('');
    expect(screen.queryByText('private-test-secret')).not.toBeInTheDocument();
  });

  it('trims pasted key whitespace and explains a server validation failure before retry', async () => {
    const user = userEvent.setup();
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [], configuration_status: 'not_configured' });
    const create = vi.spyOn(portalApi, 'createModelConnection').mockRejectedValue(new ApiError({
      code: 'invalid_argument', status: 400, message: 'invalid request', details: { field: 'base_url' },
    }));
    renderPage();
    await user.click(await screen.findByRole('button', { name: /添加个人模型/ }));
    await user.selectOptions(screen.getByLabelText('选择服务和模型'), 'deepseek-flash');
    fireEvent.change(screen.getByLabelText('API 密钥'), { target: { value: '  local-test-secret  ' } });
    await user.click(screen.getByRole('button', { name: '保存并检查连接' }));

    expect(create).toHaveBeenCalledWith(expect.objectContaining({ api_key: 'local-test-secret' }));
    expect(create.mock.calls[0]?.[0]).not.toHaveProperty('context_window_override_tokens');
    expect(create.mock.calls[0]?.[0]).not.toHaveProperty('context_window_tokens');
    expect(await screen.findByRole('alert')).toHaveTextContent('服务地址无效或未获服务端允许');
    expect(screen.getByRole('alert')).toHaveTextContent('密钥已清除，请重新输入后重试');
    const dialog = screen.getByRole('dialog', { name: '添加个人模型' });
    expect(within(dialog).getByRole('alert')).toHaveTextContent('服务地址无效或未获服务端允许');
    expect(screen.getByText('高级设置').closest('details')).not.toHaveAttribute('open');
    expect(screen.getByLabelText('API 密钥')).toHaveValue('');
    expect(screen.getByLabelText('显示名称')).toHaveValue('DeepSeek Flash');
    await user.click(within(dialog).getByRole('button', { name: '取消' }));
    expect(screen.getByRole('alert')).toHaveTextContent('服务地址无效或未获服务端允许');
    await user.click(screen.getByRole('button', { name: '关闭通知' }));
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('does not submit a key containing only whitespace', async () => {
    const user = userEvent.setup();
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [], configuration_status: 'not_configured' });
    const create = vi.spyOn(portalApi, 'createModelConnection');
    renderPage();
    await user.click(await screen.findByRole('button', { name: /添加个人模型/ }));
    await user.selectOptions(screen.getByLabelText('选择服务和模型'), 'deepseek-flash');
    fireEvent.change(screen.getByLabelText('API 密钥'), { target: { value: '   ' } });
    await user.click(screen.getByRole('button', { name: '保存并检查连接' }));
    expect(create).not.toHaveBeenCalled();
    expect(await screen.findByRole('alert')).toHaveTextContent('API 密钥不能为空');
  });
});
