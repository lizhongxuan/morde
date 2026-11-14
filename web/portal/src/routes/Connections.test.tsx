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
  capabilities: { text: true, tools: true, vision: false, text_stream: false }, checked_at: timestamp, created_at: timestamp, updated_at: timestamp,
};
const mcpConnector: MCPConnector = {
  id: 'k1234567', name: '日历工具', endpoint_url: 'https://mcp.example.test/mcp', enabled: false,
  has_credential: false, credential_version: 0, revision: 1, created_at: timestamp, updated_at: timestamp,
};

function renderPage() {
  if (!vi.isMockFunction(portalApi.mcpConnectors)) vi.spyOn(portalApi, 'mcpConnectors').mockResolvedValue({ items: [] });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}><ToastProvider><ToastNoticeCenter /><MemoryRouter><Connections /></MemoryRouter></ToastProvider></QueryClientProvider>);
}

async function selectArea(user: ReturnType<typeof userEvent.setup>, name: '个人模型' | '团队模型' | 'MCP 工具') {
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
    expect(open).toHaveBeenCalledWith('about:blank', '_blank');
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

  it('lists shared and personal models returned by the owner-scoped API', async () => {
    const user = userEvent.setup();
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [model, { ...model, id: 'p7654321', name: '我的私有模型', shared: false }], configuration_status: 'configured' });
    renderPage();

    await selectArea(user, '团队模型');
    expect(await screen.findByRole('heading', { name: '团队模型', level: 2 })).toBeInTheDocument();
    expect(await screen.findByText('团队共享')).toBeInTheDocument();
    expect(within((await screen.findByText('团队模型', { selector: 'h3' })).closest('article')!).queryByRole('button', { name: '编辑' })).not.toBeInTheDocument();
    await selectArea(user, '个人模型');
    const personal = (await screen.findByText('我的私有模型')).closest('article')!;
    expect(within(personal).getByText('个人')).toBeInTheDocument();
    expect(within(personal).getByRole('button', { name: '检查连接' })).toBeInTheDocument();
  });

  it('paginates team models without exposing rows from another page', async () => {
    const user = userEvent.setup();
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [
      model,
      { ...model, id: 'p2234567', name: '团队模型二' },
      { ...model, id: 'p3234567', name: '团队模型三' },
    ], configuration_status: 'configured' });
    renderPage();
    await selectArea(user, '团队模型');

    expect(await screen.findByRole('heading', { name: '团队模型', level: 3 })).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: '团队模型二', level: 3 })).not.toBeInTheDocument();
    const pagination = screen.getByRole('navigation', { name: '团队模型分页' });
    await user.click(within(pagination).getByRole('button', { name: '下一页' }));

    expect(await screen.findByRole('heading', { name: '团队模型二', level: 3 })).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: '团队模型', level: 3 })).not.toBeInTheDocument();
    await user.click(within(pagination).getByRole('button', { name: '下一页' }));
    expect(await screen.findByRole('heading', { name: '团队模型三', level: 3 })).toBeInTheDocument();
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
    await user.type(screen.getByLabelText('显示名称'), '待保存模型');
    await user.type(screen.getByLabelText('服务地址'), 'https://draft.example.test/v1');
    await selectArea(user, '团队模型');
    await selectArea(user, '个人模型');
    expect(screen.getByLabelText('显示名称')).toHaveValue('待保存模型');
    expect(screen.getByLabelText('服务地址')).toHaveValue('https://draft.example.test/v1');

    await selectArea(user, 'MCP 工具');
    await user.click(await screen.findByRole('button', { name: '＋ 添加工具连接' }));
    await user.type(screen.getByLabelText('连接名称'), '待保存工具');
    await user.type(screen.getByLabelText('MCP 服务地址'), 'https://draft.example.test/mcp');
    await selectArea(user, '个人模型');
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
    expect(await screen.findByText('尚未添加个人模型')).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
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

    expect(await screen.findByText(/服务地址待核对/)).toBeInTheDocument();
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
    expect(await screen.findByText(/检查流式回复会发起一次模型调用/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '检查流式回复' }));
    expect(check).toHaveBeenCalledWith(model.id);
    expect(await screen.findByRole('status')).toHaveTextContent('模型流式能力已验证');
  });

  it('creates a private connection, clears the secret field and checks the saved model', async () => {
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

    expect(await screen.findByText('尚未添加个人模型')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: /添加个人模型/ }));
    await user.type(screen.getByLabelText('显示名称'), '我的模型');
    await user.type(screen.getByLabelText('模型名'), 'private-model');
    await user.type(screen.getByLabelText('服务地址'), 'https://api.example.test/v1');
    await user.type(screen.getByLabelText('API 密钥'), 'private-test-secret');
    await user.click(screen.getByRole('button', { name: '保存个人模型' }));

    expect(create).toHaveBeenCalledWith({ name: '我的模型', base_url: 'https://api.example.test/v1', model_name: 'private-model', api_key: 'private-test-secret', shared: false });
    const card = (await screen.findByRole('heading', { name: '我的模型', level: 3 })).closest('article')!;
    expect(within(card).getByText('尚未检查；检查通过后可在 Assistant 的模型选项中选择。')).toBeInTheDocument();
    expect(screen.queryByDisplayValue('private-test-secret')).not.toBeInTheDocument();
    expect(screen.queryByText('private-test-secret')).not.toBeInTheDocument();

    await user.click(within(card).getByRole('button', { name: '检查连接' }));
    expect(check).toHaveBeenCalledWith(personal.id);
    expect(await screen.findByText('本次检查：文本可用 · 工具调用可用 · 视觉未验证')).toBeInTheDocument();
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
    expect(screen.getByLabelText('API 密钥')).toHaveValue('');
    expect(screen.getByLabelText('API 密钥')).not.toBeRequired();
    await user.clear(screen.getByLabelText('显示名称'));
    await user.type(screen.getByLabelText('显示名称'), '已修改模型');
    await user.click(screen.getByRole('button', { name: '保存修改' }));
    expect(update).toHaveBeenCalledWith(personal.id, { name: '已修改模型', base_url: personal.base_url, model_name: personal.model_name });

    const updatedCard = (await screen.findByRole('heading', { name: '已修改模型', level: 3 })).closest('article')!;
    await user.click(within(updatedCard).getByRole('button', { name: '编辑' }));
    await user.type(screen.getByLabelText('API 密钥'), 'new-test-secret');
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
    await user.type(screen.getByLabelText('显示名称'), '坏地址模型');
    await user.type(screen.getByLabelText('模型名'), 'private-model');
    await user.type(screen.getByLabelText('服务地址'), 'http://not-allowed.example.test/v1');
    await user.type(screen.getByLabelText('API 密钥'), 'must-not-send-secret');
    await user.click(screen.getByRole('button', { name: '保存个人模型' }));

    expect(create).not.toHaveBeenCalled();
    expect(screen.getByLabelText('服务地址')).toBeInvalid();
  });

  it('surfaces server allowlist failures without displaying the submitted secret', async () => {
    const user = userEvent.setup();
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [], configuration_status: 'not_configured' });
    const create = vi.spyOn(portalApi, 'createModelConnection').mockRejectedValue(new Error('服务地址不在当前域名 allowlist 中'));
    renderPage();

    await user.click(await screen.findByRole('button', { name: /添加个人模型/ }));
    await user.type(screen.getByLabelText('显示名称'), '未允许模型');
    await user.type(screen.getByLabelText('模型名'), 'private-model');
    await user.type(screen.getByLabelText('服务地址'), 'https://not-allowed.example.test/v1');
    await user.type(screen.getByLabelText('API 密钥'), 'private-test-secret');
    await user.click(screen.getByRole('button', { name: '保存个人模型' }));

    expect(create).toHaveBeenCalledOnce();
    expect(await screen.findByRole('alert')).toHaveTextContent('服务地址不在当前域名 allowlist 中');
    expect(screen.getByLabelText('API 密钥')).toHaveValue('');
    expect(screen.queryByText('private-test-secret')).not.toBeInTheDocument();
  });
});
