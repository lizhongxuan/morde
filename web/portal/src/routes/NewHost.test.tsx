import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { portalApi } from '../app/api';
import { ToastNoticeCenter, ToastProvider } from '../app/toast';
import { NewHost } from './NewHost';

vi.mock('../app/auth', () => ({ useAuth: () => ({ user: { id: 'u1234567', quota: { hosts: 2 } } }) }));

const createdHost: Awaited<ReturnType<typeof portalApi.createHost>> = {
  id: 'h1234567', name: '测试电脑', owner_id: 'u1234567', template_id: 'std', isolation: 'container', state: 'creating',
  state_reason: '', runtime_generation: 1, control_version: 0, control_state: 'ai', url: '', workspaces: [],
  resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 },
  expires_at: null, last_active_at: null, created_at: '',
};

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><ToastProvider><ToastNoticeCenter /><MemoryRouter><NewHost /></MemoryRouter></ToastProvider></QueryClientProvider>);
}

describe('NewHost', () => {
  beforeEach(() => { vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [] }); });
  afterEach(() => { cleanup(); localStorage.clear(); vi.restoreAllMocks(); });

  it('creates a cloud computer without asking the user to choose a Workspace', async () => {
    vi.mocked(portalApi.hosts).mockResolvedValue({ items: [
      { ...createdHost, id: 'h8765432', owner_id: 'u8765432' },
      { ...createdHost, id: 'h7654321', owner_id: 'u7654321' },
    ] });
    const createHost = vi.spyOn(portalApi, 'createHost').mockResolvedValue(createdHost);
    const createWorkspace = vi.spyOn(portalApi, 'createWorkspace');
    const user = userEvent.setup();
    renderPage();

    expect(await screen.findByText('~/workspace/project/项目名/')).toBeInTheDocument();
    expect(screen.queryByRole('radio', { name: /工作区/ })).not.toBeInTheDocument();
    await user.type(screen.getByRole('textbox', { name: /云端电脑名称/ }), '研究电脑');
    await user.click(screen.getByRole('button', { name: '创建云端电脑' }));

    await waitFor(() => expect(createHost).toHaveBeenCalledWith({ name: '研究电脑' }, expect.stringMatching(/^[A-Za-z0-9._~-]{8,128}$/)));
    expect(createWorkspace).not.toHaveBeenCalled();
  });

  it('retries an uncertain automatic creation with the same request key', async () => {
    const createHost = vi.spyOn(portalApi, 'createHost').mockRejectedValueOnce(new TypeError('响应丢失'))
      .mockResolvedValue(createdHost);
    const user = userEvent.setup();
    renderPage();

    await screen.findByRole('button', { name: '创建云端电脑' });
    await user.type(screen.getByRole('textbox', { name: /云端电脑名称/ }), '研究电脑');
    await user.click(screen.getByRole('button', { name: '创建云端电脑' }));
    expect(await screen.findByText('响应丢失')).toBeInTheDocument();
    const stored = JSON.parse(localStorage.getItem('morde.pending-host-create.u1234567') ?? 'null');
    expect(stored).toEqual({
      name: '研究电脑',
      hostIdempotencyKey: createHost.mock.calls[0]?.[1],
    });
    await user.click(screen.getByRole('button', { name: '继续上次创建' }));

    await waitFor(() => expect(createHost).toHaveBeenCalledTimes(2));
    expect(createHost.mock.calls[1]?.[0]).toEqual(createHost.mock.calls[0]?.[0]);
    expect(createHost.mock.calls[1]?.[1]).toBe(createHost.mock.calls[0]?.[1]);
    expect(localStorage.getItem('morde.pending-host-create.u1234567')).toBeNull();
  });

  it('requires checking and clearing a legacy Workspace create instead of replaying it', async () => {
    const input = { name: '旧电脑', workspace_mode: 'new', workspace_ids: [] };
    const key = (value: string) => `request-${value}`;
    localStorage.setItem('morde.pending-host-create.u1234567', JSON.stringify({
      version: 1, fingerprint: JSON.stringify(input), input, hostIdempotencyKey: key('host'),
      workspaceIdempotencyKey: key('workspace'),
    }));
    const createWorkspace = vi.spyOn(portalApi, 'createWorkspace');
    const createHost = vi.spyOn(portalApi, 'createHost').mockResolvedValue(createdHost);
    const user = userEvent.setup();
    renderPage();

    const warning = await screen.findByRole('alert');
    expect(warning).toHaveTextContent('发现旧版电脑创建恢复记录，不能安全重放');
    expect(warning).toHaveTextContent('请先检查云端电脑列表，确认上次创建结果');
    expect(within(warning).getByRole('link', { name: '检查云端电脑列表' })).toHaveAttribute('href', '/hosts');
    expect(screen.queryByRole('button', { name: '继续上次创建' })).not.toBeInTheDocument();
    expect(createWorkspace).not.toHaveBeenCalled();
    expect(createHost).not.toHaveBeenCalled();

    await user.click(within(warning).getByRole('button', { name: '我已核实，清除记录' }));
    expect(localStorage.getItem('morde.pending-host-create.u1234567')).toBeNull();
    expect(screen.getByRole('button', { name: '创建云端电脑' })).toBeEnabled();
  });

  it('blocks creation when the saved recovery record is invalid until the user verifies the host list', async () => {
    localStorage.setItem('morde.pending-host-create.u1234567', '{"version":0}');
    renderPage();

    const warning = await screen.findByRole('alert');
    expect(warning).toHaveTextContent('无法读取本机上次创建记录');
    expect(within(warning).getByRole('link', { name: '检查云端电脑列表' })).toHaveAttribute('href', '/hosts');
    const submit = screen.getByRole('button', { name: '创建云端电脑' });
    expect(submit).toBeDisabled();
    await userEvent.setup().click(screen.getByRole('button', { name: '我已核实，清除记录' }));
    expect(localStorage.getItem('morde.pending-host-create.u1234567')).toBeNull();
    expect(submit).toBeEnabled();
  });
});
