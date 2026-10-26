import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError } from '../api/client';
import { portalApi } from '../app/api';
import { ToastProvider } from '../app/toast';
import { Hosts } from './Hosts';

vi.mock('../app/auth', () => ({ useAuth: () => ({ user: { id: 'u1234567' } }) }));

type Host = Awaited<ReturnType<typeof portalApi.hosts>>['items'][number];
const pollingHostCases: [Host['state'], string][] = [
  ['creating', '创建中'],
  ['stopping', '停止中'],
  ['deleting', '删除中'],
];
const ownedHost: Host = {
  id: 'h1234567', name: '测试主机', owner_id: 'u1234567', template_id: 'std', isolation: 'container', state: 'running', state_reason: '', runtime_generation: 1, control_version: 1, control_state: 'ai', url: 'https://h1234567.hosts.test',
  workspaces: [], resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 }, expires_at: null, last_active_at: null, created_at: '',
};

describe('Hosts', () => {
  beforeEach(() => { vi.spyOn(portalApi, 'sharedHosts').mockResolvedValue({ items: [] }); });
  afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.useRealTimers(); });

  it('deletes a host without consulting templates and retains its independent workspaces', async () => {
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [{
      id: 'h1234567', name: '测试主机', owner_id: 'u1234567', template_id: 'std', isolation: 'container', state: 'running', state_reason: '', runtime_generation: 1, control_version: 1, control_state: 'ai', url: '',
      workspaces: [],
      resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 },
      expires_at: null, last_active_at: null, created_at: '',
    }] });
    vi.spyOn(portalApi, 'templates');
    const deleteHost = vi.spyOn(portalApi, 'deleteHost').mockResolvedValue({
      id: 'h1234567', name: '测试主机', owner_id: 'u1234567', template_id: 'std', isolation: 'container', state: 'deleting', state_reason: '', runtime_generation: 1, control_version: 1, control_state: 'ai', url: '', workspaces: [],
      resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 }, expires_at: null, last_active_at: null, created_at: '',
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const user = userEvent.setup();

    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    expect(await screen.findByRole('button', { name: '进入主机 测试主机' })).toBeEnabled();
    expect(screen.queryByRole('button', { name: /暂停主机|恢复主机|管理主机令牌/ })).not.toBeInTheDocument();
    await user.click(await screen.findByRole('button', { name: '删除主机 测试主机' }));
    expect(screen.getByText(/云端电脑的运行环境和专属资料卷会被删除，正在运行的终端命令及服务会中止/)).toBeInTheDocument();
    expect(screen.getByText(/绑定的独立工作区会自动解绑并保留在服务端，不随主机删除/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: /^删除主机$/ }));
    await waitFor(() => expect(deleteHost).toHaveBeenCalledWith('h1234567'));
    expect(portalApi.templates).not.toHaveBeenCalled();
  });

  it('keeps management actions available but prevents entering another owner\'s host', async () => {
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [{
      id: 'h8765432', name: '他人主机', owner_id: 'u8765432', template_id: 'std', isolation: 'container', state: 'running', state_reason: '', runtime_generation: 1, control_version: 1, control_state: 'ai', url: '',
      workspaces: [],
      resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 },
      expires_at: null, last_active_at: null, created_at: '',
    }] });
    vi.spyOn(portalApi, 'templates').mockResolvedValue({ items: [{
      id: 'std', name: '标准规格', description: '', isolation: 'container',
      resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 },
        workspace_size_mib: 1024, ttl_seconds: 3600, idle_timeout_seconds: 0, on_idle: 'disabled', keep_workspace_default: true,
    }] });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });

    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    expect(await screen.findByRole('button', { name: '进入主机 他人主机' })).toBeDisabled();
    expect(screen.getByText('仅主机所有者可以进入工作环境。')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '续期主机 他人主机' })).toBeEnabled();
    expect(screen.getByRole('button', { name: '删除主机 他人主机' })).toBeEnabled();
  });

  it('confirms compute release and calls the release endpoint', async () => {
    const host: Host = {
      id: 'h1234567', name: '我的云端电脑', owner_id: 'u1234567', template_id: 'std', isolation: 'container', state: 'running', state_reason: '', runtime_generation: 1, control_version: 0, control_state: 'ai', url: '',
      workspaces: [],
      resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 },
      expires_at: '', last_active_at: '', created_at: '',
    };
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [host] });
    vi.spyOn(portalApi, 'templates').mockResolvedValue({ items: [] });
    const release = vi.spyOn(portalApi, 'releaseHost').mockResolvedValue(host);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const user = userEvent.setup();
    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    await user.click(await screen.findByRole('button', { name: '释放运行资源 我的云端电脑' }));
    expect(screen.getByText(/主机身份、工作目录和文件会保留/)).toBeInTheDocument();
    expect(screen.getByText(/终端窗口断开后，里面的命令仍可能继续运行/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '取消' }));
    expect(release).not.toHaveBeenCalled();
    await user.click(screen.getByRole('button', { name: '释放运行资源 我的云端电脑' }));
    await user.click(screen.getByRole('button', { name: '释放运行资源' }));
    await waitFor(() => expect(release).toHaveBeenCalledWith(host.id, true));
  });

  it('offers restore for a stopped host', async () => {
    const host: Host = {
      id: 'h1234567', name: '我的云端电脑', owner_id: 'u1234567', template_id: 'std', isolation: 'container', state: 'stopped', state_reason: 'user_release', runtime_generation: 1, control_version: 0, control_state: 'ai', url: '',
      workspaces: [],
      resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 },
      expires_at: '', last_active_at: '', created_at: '',
    };
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [host] });
    vi.spyOn(portalApi, 'templates').mockResolvedValue({ items: [] });
    const restore = vi.spyOn(portalApi, 'restoreHost').mockResolvedValue({ ...host, state: 'creating', runtime_generation: 2 });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const user = userEvent.setup();
    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    await user.click(await screen.findByRole('button', { name: '恢复主机 我的云端电脑' }));
    expect(screen.getByText(/保留原有文件/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '恢复主机' }));
    await waitFor(() => expect(restore).toHaveBeenCalledWith(host.id));
  });

  it.each(pollingHostCases)('re-polls a %s host after three seconds', async (state, label) => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
    const host: Host = {
      id: 'h1234567', name: '处理中主机', owner_id: 'u1234567', template_id: 'std', isolation: 'container', state,
      state_reason: '', runtime_generation: 1, control_version: 1, control_state: 'ai', url: '', workspaces: [],
      resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 },
      expires_at: null, last_active_at: null, created_at: '',
    };
    const hosts = vi.spyOn(portalApi, 'hosts')
      .mockResolvedValueOnce({ items: [host] })
      .mockResolvedValue({ items: [{ ...host, state: 'running' }] });
    vi.spyOn(portalApi, 'templates').mockResolvedValue({ items: [] });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });

    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    expect(await screen.findByText(label)).toBeInTheDocument();
    expect(hosts).toHaveBeenCalledTimes(1);
    await act(async () => { await vi.advanceTimersByTimeAsync(3000); });
    expect(hosts).toHaveBeenCalledTimes(2);
    await expect(hosts.mock.results[1]!.value).resolves.toMatchObject({ items: [expect.objectContaining({ state: 'running' })] });
  });

  it('does not expose shared member management in the MVP', async () => {
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [ownedHost] });
    const listMembers = vi.spyOn(portalApi, 'hostMembers');
    const invite = vi.spyOn(portalApi, 'inviteHostMember');
    const revoke = vi.spyOn(portalApi, 'revokeHostMember');
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    expect(await screen.findByRole('button', { name: '进入主机 测试主机' })).toBeEnabled();
    expect(screen.queryByRole('button', { name: /管理共享|邀请并授权|撤销权限/ })).not.toBeInTheDocument();
    expect(screen.queryByLabelText(/对方账号 ID|授予权限/)).not.toBeInTheDocument();
    expect(listMembers).not.toHaveBeenCalled();
    expect(invite).not.toHaveBeenCalled();
    expect(revoke).not.toHaveBeenCalled();
  });

  it('opens a shared desktop using a same-origin one-time observation ticket', async () => {
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'sharedHosts').mockResolvedValue({ items: [{ id: ownedHost.id, name: '共享电脑', state: 'running', url: ownedHost.url, permissions: ['desktop:view'] }] });
    const ticket = vi.spyOn(portalApi, 'issueObservationTicket').mockResolvedValue({ url: `${ownedHost.url}/__ch/auth?t=one-time`, expires_at: '2026-10-25T00:01:00Z' });
    const replace = vi.fn();
    const close = vi.fn();
    const popup = { opener: window, location: { replace }, close } as unknown as Window;
    const open = vi.spyOn(window, 'open').mockReturnValue(popup);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    const user = userEvent.setup();
    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    await user.click(await screen.findByRole('button', { name: '查看桌面' }));
    await waitFor(() => expect(ticket).toHaveBeenCalledWith(ownedHost.id, 'desktop'));
    await waitFor(() => expect(replace).toHaveBeenCalledWith(`${ownedHost.url}/__ch/auth?t=one-time`));
    expect(open).toHaveBeenCalledWith('about:blank', '_blank');
    expect(popup.opener).toBeNull();
    expect(close).not.toHaveBeenCalled();
  });

  it.each([
    { code: 'not_found', status: 404 },
    { code: 'forbidden', status: 403 },
  ])('refreshes shared Hosts after a revoked member is denied an entry ticket ($code)', async ({ code, status }) => {
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [] });
    const sharedHosts = vi.spyOn(portalApi, 'sharedHosts')
      .mockResolvedValueOnce({ items: [{ id: ownedHost.id, name: '共享电脑', state: 'running', url: ownedHost.url, permissions: ['desktop:view'] }] })
      .mockResolvedValueOnce({ items: [] });
    vi.spyOn(portalApi, 'issueObservationTicket').mockRejectedValue(new ApiError({ message: '无法访问这台主机', code, status }));
    const close = vi.fn();
    vi.spyOn(window, 'open').mockReturnValue({ opener: window, location: { replace: vi.fn() }, close } as unknown as Window);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    const user = userEvent.setup();
    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    await user.click(await screen.findByRole('button', { name: '查看桌面' }));

    await waitFor(() => expect(sharedHosts).toHaveBeenCalledTimes(2));
    expect(await screen.findByText('还没有人与你共享云端电脑。')).toBeInTheDocument();
    expect(screen.queryByText('共享电脑')).not.toBeInTheDocument();
    expect(close).toHaveBeenCalledOnce();
  });

  it('gives a terminal-only member a terminal entry without showing desktop access', async () => {
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'sharedHosts').mockResolvedValue({ items: [{ id: ownedHost.id, name: '终端共享电脑', state: 'running', url: ownedHost.url, permissions: ['terminal:control'] }] });
    const ticket = vi.spyOn(portalApi, 'issueObservationTicket').mockResolvedValue({ url: `${ownedHost.url}/__ch/auth?t=one-time&next=%2F__ch%2Fterminal%2F`, expires_at: '2026-10-25T00:01:00Z' });
    const replace = vi.fn();
    const open = vi.spyOn(window, 'open').mockReturnValue({ opener: window, location: { replace }, close: vi.fn() } as unknown as Window);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    const user = userEvent.setup();
    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    expect(await screen.findByRole('button', { name: '打开终端' })).toBeEnabled();
    expect(screen.queryByRole('button', { name: '查看桌面' })).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '打开终端' }));
    await waitFor(() => expect(ticket).toHaveBeenCalledWith(ownedHost.id, 'terminal'));
    await waitFor(() => expect(replace).toHaveBeenCalledWith(`${ownedHost.url}/__ch/auth?t=one-time&next=%2F__ch%2Fterminal%2F`));
    expect(open).toHaveBeenCalledWith('about:blank', '_blank');
  });

  it('rejects and closes a shared desktop ticket from a different origin', async () => {
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'sharedHosts').mockResolvedValue({ items: [{ id: ownedHost.id, name: '共享电脑', state: 'running', url: ownedHost.url, permissions: ['desktop:view'] }] });
    vi.spyOn(portalApi, 'issueObservationTicket').mockResolvedValue({ url: 'https://attacker.example/__ch/auth?t=one-time', expires_at: '2026-10-25T00:01:00Z' });
    const replace = vi.fn();
    const close = vi.fn();
    vi.spyOn(window, 'open').mockReturnValue({ opener: window, location: { replace }, close } as unknown as Window);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    const user = userEvent.setup();
    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    await user.click(await screen.findByRole('button', { name: '查看桌面' }));
    expect(await screen.findByText('共享桌面入口地址无效，已阻止打开。')).toBeInTheDocument();
    expect(replace).not.toHaveBeenCalled();
    expect(close).toHaveBeenCalledOnce();
  });
});
