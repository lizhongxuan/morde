import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { portalApi } from '../app/api';
import { ToastProvider } from '../app/toast';
import { Hosts } from './Hosts';

vi.mock('../app/auth', () => ({ useAuth: () => ({ user: { id: 'u1234567', quota: { hosts: 20 } } }) }));

type Host = Awaited<ReturnType<typeof portalApi.hosts>>['items'][number];
const pollingHostCases: [Host['state'], string][] = [
  ['creating', '创建中'],
  ['stopping', '停止中'],
  ['deleting', '删除中'],
];
const hostWorkspaceBinding: Host['workspaces'][number] = {
  workspace: { id: 'w1234567', name: '项目工作区', revision: 2, used_bytes: 0, state: 'ready' },
  directory: 'projects/w1234567', state: 'active', binding_version: 1, desired_revision: 2, applied_revision: 2,
  sync_state: 'healthy', sync_conflicts: [],
};
const ownedHost: Host = {
  id: 'h1234567', name: '测试主机', owner_id: 'u1234567', template_id: 'std', isolation: 'container', state: 'running', state_reason: '', runtime_generation: 1, control_version: 1, control_state: 'ai', url: 'https://h1234567.hosts.test',
  workspaces: [hostWorkspaceBinding], resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 }, expires_at: null, last_active_at: null, created_at: '',
};
const aiControl = (control_version: number) => ({
  host_id: ownedHost.id, control_state: 'ai' as const, control_version, control_actor_user_id: '',
  control_lease_expires_at: null, control_lease_active: false, control_lease_held_by_me: false, active_transition: null,
});
const userControl = (control_version: number) => ({
  host_id: ownedHost.id, control_state: 'user' as const, control_version, control_actor_user_id: ownedHost.owner_id,
  control_lease_expires_at: '2026-11-02T00:00:30.000Z', control_lease_active: true, control_lease_held_by_me: true, active_transition: null,
});
async function openMoreActions(user: ReturnType<typeof userEvent.setup>, host: Pick<Host, 'id' | 'name'>) {
  const card = await waitFor(() => {
    const item = document.getElementById(`host-${host.id}`);
    if (!item) throw new Error(`Host card ${host.id} is not rendered`);
    return item;
  });
  const summary = within(card).queryByText('更多操作', { selector: 'summary' })
    ?? within(card).getByText('查看控制权状态', { selector: 'summary' });
  if (!summary.parentElement?.hasAttribute('open')) await user.click(summary);
}

async function openHostDetails(user: ReturnType<typeof userEvent.setup>, hostID: string) {
  await openMoreActions(user, { id: hostID, name: '' });
  const card = document.getElementById(`host-${hostID}`);
  if (!card) throw new Error(`Host card ${hostID} is not rendered`);
  const toggle = within(card).queryByRole('button', { name: /详情与管理/ });
  if (toggle?.getAttribute('aria-expanded') !== 'true') {
    if (toggle) await user.click(toggle);
  }
}

describe('Hosts', () => {
  beforeEach(() => {
    vi.spyOn(portalApi, 'sharedHosts').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'preferences').mockResolvedValue({ assistant_name: 'Morde', selected_model_id: null, default_host_id: null, updated_at: null });
  });
  afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.useRealTimers(); });

  it('sets an owned default computer and links to that computer Workspace settings', async () => {
    const nextHost: Host = { ...ownedHost, id: 'h7654321', name: '第二台电脑', workspaces: [{
      ...hostWorkspaceBinding, workspace: { ...hostWorkspaceBinding.workspace, id: 'w7654321', name: '第二默认工作区' },
    }] };
    let defaultHostID: string | null = ownedHost.id;
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [ownedHost, nextHost] });
    vi.mocked(portalApi.preferences).mockImplementation(async () => ({
      assistant_name: 'Morde', selected_model_id: null, default_host_id: defaultHostID, updated_at: null,
    }));
    const setDefault = vi.spyOn(portalApi, 'ensureDefaultHost').mockImplementation(async (body) => {
      defaultHostID = body?.host_id ?? null;
      return {
        created: false,
        host: { id: nextHost.id, name: nextHost.name, state: 'running' },
        status: { model_status: 'ready', models: [], selected_model_id: null, computer_status: 'ready',
          computers: [], default_host_id: nextHost.id, can_submit: true, next_action: 'start_chat' },
      };
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const user = userEvent.setup();
    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    const selector = await screen.findByRole('combobox', { name: '默认云端电脑' });
    expect(selector).toHaveValue(ownedHost.id);
    await user.selectOptions(selector, nextHost.id);

    await waitFor(() => expect(setDefault).toHaveBeenCalledWith({ host_id: nextHost.id }));
    await waitFor(() => expect(selector).toHaveValue(nextHost.id));
    expect(document.querySelector('.host-default-workspace strong')).toHaveTextContent('第二默认工作区');
    expect(screen.getByRole('link', { name: '管理 Workspace' })).toHaveAttribute('href', `/workspaces?host=${nextHost.id}`);
  });

  it('keeps the previous default computer and the page usable when saving a new default fails', async () => {
    const nextHost = { ...ownedHost, id: 'h7654321', name: '第二台电脑' };
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [ownedHost, nextHost] });
    vi.spyOn(portalApi, 'preferences').mockResolvedValue({ assistant_name: 'Morde', selected_model_id: null, default_host_id: ownedHost.id, updated_at: null });
    vi.spyOn(portalApi, 'ensureDefaultHost').mockRejectedValue(new Error('保存默认失败'));
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const user = userEvent.setup();
    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    const selector = await screen.findByRole('combobox', { name: '默认云端电脑' });
    await screen.findByRole('button', { name: `查看云端电脑 ${ownedHost.name}` });
    await user.selectOptions(selector, nextHost.id);

    expect(await screen.findByText('保存默认失败')).toBeInTheDocument();
    await waitFor(() => expect(selector).toHaveValue(ownedHost.id));
    expect(screen.getByRole('button', { name: `查看云端电脑 ${ownedHost.name}` })).toBeInTheDocument();
  });

  it('shows the requested owner host on the first page after loading a browser handoff link', async () => {
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [ownedHost] });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter initialEntries={[`/hosts#host-${ownedHost.id}`]}><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    expect(await screen.findByRole('button', { name: `查看云端电脑 ${ownedHost.name}` })).toBeEnabled();
    expect(document.getElementById(`host-${ownedHost.id}`)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '我的电脑' })).toHaveAttribute('aria-pressed', 'true');
  });

  it('opens a shared host deep link in the shared list without page scrolling', async () => {
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'sharedHosts').mockResolvedValue({ items: [{ id: ownedHost.id, name: '共享电脑', state: 'running', url: ownedHost.url, permissions: ['desktop:view'] }] });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter initialEntries={[`/hosts#host-${ownedHost.id}`]}><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    expect(await screen.findByRole('button', { name: '查看桌面' })).toBeEnabled();
    expect(screen.getByRole('button', { name: '共享给我的' })).toHaveAttribute('aria-pressed', 'true');
  });

  it('paginates long owner and shared host lists within fixed screen areas', async () => {
    const ownerHosts: Host[] = Array.from({ length: 7 }, (_, index) => ({
      ...ownedHost,
      id: `h${String(index + 1).padStart(7, '0')}`,
      name: `我的电脑 ${index + 1}`,
      workspaces: [],
    }));
    const sharedItems = Array.from({ length: 7 }, (_, index) => ({
      id: `h${String(index + 11).padStart(7, '0')}`,
      name: `共享电脑 ${index + 1}`,
      state: 'running' as const,
      url: '',
      permissions: ['terminal:control' as const],
    }));
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: ownerHosts });
    vi.spyOn(portalApi, 'sharedHosts').mockResolvedValue({ items: sharedItems });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const user = userEvent.setup();

    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    const ownerPagination = await screen.findByRole('navigation', { name: '我的电脑分页' });
    expect(screen.getByRole('button', { name: '查看云端电脑 我的电脑 1' })).toBeDisabled();
    expect(screen.queryByRole('button', { name: '查看云端电脑 我的电脑 7' })).not.toBeInTheDocument();
    await user.click(within(ownerPagination).getByRole('button', { name: '下一页' }));
    expect(screen.getByRole('button', { name: '查看云端电脑 我的电脑 7' })).toBeDisabled();

    await user.click(screen.getByRole('button', { name: '共享给我的' }));
    const sharedPagination = await screen.findByRole('navigation', { name: '共享给我的电脑分页' });
    expect(screen.getAllByRole('button', { name: '打开终端' }).every((button) => !button.hasAttribute('disabled'))).toBe(true);
    for (let page = 1; page < sharedItems.length; page += 1) {
      await user.click(within(sharedPagination).getByRole('button', { name: '下一页' }));
    }
    expect(screen.getByRole('heading', { name: '共享电脑 7' })).toBeInTheDocument();
  });

  it('keeps a compact host summary visible and exposes full details through an accessible disclosure', async () => {
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [ownedHost] });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const user = userEvent.setup();
    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    const viewButton = await screen.findByRole('button', { name: `查看云端电脑 ${ownedHost.name}` });
    const card = document.getElementById(`host-${ownedHost.id}`)!;
    expect(card).toHaveClass('host-card-compact');
    expect(viewButton).toBeVisible();
    expect(within(card).getByText('更多操作', { selector: 'summary' })).toBeVisible();
    expect(within(card).getByRole('button', { name: '详情与管理' })).toHaveAttribute('aria-expanded', 'false');
    expect(document.querySelector('.hosts-pagination')).toBeInTheDocument();

    await openHostDetails(user, ownedHost.id);
    expect(document.getElementById(`host-details-${ownedHost.id}`)).toBeVisible();
    expect(await within(card).findByText('运行状态')).toBeVisible();
    expect(within(card).getByText('旧数据迁移状态未知')).toBeVisible();
    expect(within(card).getByRole('button', { name: /管理共享成员/ })).toBeVisible();
  });

  it('deletes a host without consulting templates and retains its independent workspaces', async () => {
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [{
      id: 'h1234567', name: '测试主机', owner_id: 'u1234567', template_id: 'std', isolation: 'container', state: 'running', state_reason: '', runtime_generation: 1, control_version: 1, control_state: 'ai', url: '',
      workspaces: [hostWorkspaceBinding],
      resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 },
      expires_at: null, last_active_at: null, created_at: '',
    }] });
    vi.spyOn(portalApi, 'templates');
    const deleteHost = vi.spyOn(portalApi, 'deleteHost').mockResolvedValue({
      id: 'h1234567', name: '测试主机', owner_id: 'u1234567', template_id: 'std', isolation: 'container', state: 'deleting', state_reason: '', runtime_generation: 1, control_version: 1, control_state: 'ai', url: '', workspaces: [hostWorkspaceBinding],
      resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 }, expires_at: null, last_active_at: null, created_at: '',
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const user = userEvent.setup();

    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    expect(await screen.findByRole('button', { name: '查看云端电脑 测试主机' })).toBeEnabled();
    expect(screen.queryByRole('button', { name: /暂停主机|恢复云端电脑|管理主机令牌/ })).not.toBeInTheDocument();
    await openMoreActions(user, ownedHost);
    await user.click(await screen.findByRole('button', { name: '删除云端电脑 测试主机' }));
    expect(screen.getByText(/云端电脑的运行环境和专属资料卷会被删除，正在运行的终端命令及服务会中止/)).toBeInTheDocument();
    expect(screen.getByText(/关联的云端文件会自动解除关联并保留在服务端，不随电脑删除/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: /^删除电脑$/ }));
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
    const user = userEvent.setup();

    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    expect(await screen.findByRole('button', { name: '查看云端电脑 他人主机' })).toBeDisabled();
    expect(screen.getByText('仅主机所有者可以进入工作环境。')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /管理共享成员/ })).not.toBeInTheDocument();
    await openMoreActions(user, { id: 'h8765432', name: '他人主机' });
    expect(screen.getByRole('button', { name: '续期主机 他人主机' })).toBeEnabled();
    expect(screen.getByRole('button', { name: '删除云端电脑 他人主机' })).toBeEnabled();
  });

  it('requires one fully applied Workspace before opening or restoring an owned host', async () => {
    const runningHost = { ...ownedHost, id: 'h0000001', name: '未绑定运行中主机', workspaces: [] };
    const multipleBindingHost = { ...ownedHost, id: 'h0000003', name: '多绑定运行中主机', workspaces: [hostWorkspaceBinding,
      { ...hostWorkspaceBinding, workspace: { ...hostWorkspaceBinding.workspace, id: 'w7654321', name: '另一个工作区' } }] };
    const stoppedHost = { ...ownedHost, id: 'h0000002', name: '未绑定已停止主机', state: 'stopped' as const, workspaces: [] };
    const staleRevisionHost = { ...ownedHost, id: 'h0000004', name: '工作区版本未同步主机', workspaces: [{
      ...hostWorkspaceBinding, workspace: { ...hostWorkspaceBinding.workspace, revision: 3 },
    }] };
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [runningHost, multipleBindingHost, stoppedHost, staleRevisionHost] });
    vi.spyOn(portalApi, 'workspaces').mockResolvedValue({ items: [] });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const user = userEvent.setup();

    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    expect(await screen.findByRole('button', { name: '查看云端电脑 未绑定运行中主机' })).toBeDisabled();
    expect(screen.getByRole('button', { name: '查看云端电脑 多绑定运行中主机' })).toBeDisabled();
    expect(screen.getByRole('button', { name: '查看云端电脑 工作区版本未同步主机' })).toBeDisabled();
    expect(screen.getByText('云端电脑的文件正在准备、同步或存在冲突；打开和移交操作暂不可用。')).toBeInTheDocument();
    expect(screen.getAllByText(/请先关联并准备好这台电脑的文件/)).toHaveLength(3);
    await openHostDetails(user, stoppedHost.id);
    await openMoreActions(user, stoppedHost);
    expect(screen.getByRole('button', { name: '恢复云端电脑 未绑定已停止主机' })).toBeDisabled();
    expect(screen.getByLabelText('恢复前关联已有云端文件')).toBeInTheDocument();
  });

  it('shows Workspace Runtime heartbeat separately from Host and noVNC desktop status', async () => {
    const host: Host = {
      ...ownedHost,
      workspace_runtime_online: false,
      workspace_runtime_last_seen_at: '2026-11-12T08:30:00Z',
    };
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [host] });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const user = userEvent.setup();
    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    const card = await waitFor(() => {
      const item = document.getElementById(`host-${host.id}`);
      if (!item) throw new Error('Host card not rendered');
      return item;
    });
    await openHostDetails(user, host.id);
    const runtime = within(card).getByText('Workspace Runtime').parentElement;
    expect(runtime).toHaveTextContent('离线');
    expect(runtime).toHaveTextContent('最近观测：');
    expect(within(card).getByText('运行中')).toBeInTheDocument();
    expect(within(card).getByText('桌面连接').parentElement).toHaveTextContent('由 noVNC 工作台单独检测');
  });

  it('keeps missing Runtime fields unknown', async () => {
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [ownedHost] });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const user = userEvent.setup();
    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    const card = await waitFor(() => {
      const item = document.getElementById(`host-${ownedHost.id}`);
      if (!item) throw new Error('Host card not rendered');
      return item;
    });
    await openHostDetails(user, ownedHost.id);
    expect(within(card).getByText('Workspace Runtime').parentElement).toHaveTextContent('未知');
    expect(within(card).getByText('桌面连接').parentElement).toHaveTextContent('由 noVNC 工作台单独检测');
  });

  it('binds one ready Workspace to a stopped legacy host before enabling restore', async () => {
    const stoppedHost = { ...ownedHost, state: 'stopped' as const, workspaces: [] };
    const workspace = {
      id: 'w7654321', name: '旧电脑工作区', revision: 4, used_bytes: 0, quota_bytes: 1024, is_read_only: false,
      state: 'ready' as const, created_at: '', updated_at: '',
    };
    const preparingBinding: Host['workspaces'][number] = {
      workspace: { id: workspace.id, name: workspace.name, revision: workspace.revision, used_bytes: workspace.used_bytes, state: workspace.state },
      directory: workspace.id, state: 'preparing', binding_version: 1, desired_revision: workspace.revision,
      applied_revision: 0, sync_state: 'healthy', sync_conflicts: [],
    };
    let currentHost: Host = stoppedHost;
    vi.spyOn(portalApi, 'hosts').mockImplementation(async () => ({ items: [currentHost] }));
    vi.spyOn(portalApi, 'workspaces').mockResolvedValue({ items: [workspace] });
    const bind = vi.spyOn(portalApi, 'bindWorkspaceToHost').mockImplementation(async (hostID, request) => {
      expect(hostID).toBe(stoppedHost.id);
      expect(request).toEqual({ workspace_id: workspace.id, expected_revision: workspace.revision, expected_binding_version: 0 });
      currentHost = { ...stoppedHost, workspaces: [preparingBinding] };
      return preparingBinding;
    });
    const restore = vi.spyOn(portalApi, 'restoreHost').mockResolvedValue({ ...stoppedHost, state: 'creating', workspaces: [preparingBinding] });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    const user = userEvent.setup();
    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    await screen.findByRole('button', { name: `查看云端电脑 ${stoppedHost.name}` });
    await openHostDetails(user, stoppedHost.id);
    const workspaceSelect = await screen.findByLabelText('恢复前关联已有云端文件');
    await screen.findByRole('option', { name: /旧电脑工作区/ });
    await user.selectOptions(workspaceSelect, workspace.id);
    await user.click(screen.getByRole('button', { name: '关联所选文件' }));
    expect(await screen.findByText(/已绑定 旧电脑工作区；恢复电脑后会继续应用/)).toBeInTheDocument();
    await openMoreActions(user, stoppedHost);
    const restoreButton = await screen.findByRole('button', { name: `恢复云端电脑 ${stoppedHost.name}` });
    expect(restoreButton).toBeEnabled();

    await user.click(restoreButton);
    await user.click(screen.getByRole('button', { name: '恢复主机' }));
    await waitFor(() => expect(restore).toHaveBeenCalledWith(stoppedHost.id));
    expect(bind).toHaveBeenCalledOnce();
  });

  it('confirms compute release and calls the release endpoint', async () => {
    const host: Host = {
      id: 'h1234567', name: '我的云端电脑', owner_id: 'u1234567', template_id: 'std', isolation: 'container', state: 'running', state_reason: '', runtime_generation: 1, control_version: 0, control_state: 'ai', url: '',
      workspaces: [hostWorkspaceBinding],
      resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 },
      expires_at: '', last_active_at: '', created_at: '',
    };
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [host] });
    vi.spyOn(portalApi, 'templates').mockResolvedValue({ items: [] });
    const release = vi.spyOn(portalApi, 'releaseHost').mockResolvedValue(host);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const user = userEvent.setup();
    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    await screen.findByRole('button', { name: `查看云端电脑 ${host.name}` });
    await openMoreActions(user, { id: host.id, name: host.name });
    await user.click(await screen.findByRole('button', { name: '释放运行资源 我的云端电脑' }));
    expect(screen.getByText(/主机身份、工作目录和文件会保留/)).toBeInTheDocument();
    expect(screen.getByText(/终端窗口断开后，里面的命令仍可能继续运行/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '取消' }));
    expect(release).not.toHaveBeenCalled();
    await openMoreActions(user, { id: host.id, name: host.name });
    await user.click(screen.getByRole('button', { name: '释放运行资源 我的云端电脑' }));
    await user.click(screen.getByRole('button', { name: '释放运行资源' }));
    await waitFor(() => expect(release).toHaveBeenCalledWith(host.id, true));
  });

  it('offers restore for a stopped host', async () => {
    const host: Host = {
      id: 'h1234567', name: '我的云端电脑', owner_id: 'u1234567', template_id: 'std', isolation: 'container', state: 'stopped', state_reason: 'user_release', runtime_generation: 1, control_version: 0, control_state: 'ai', url: '',
      workspaces: [hostWorkspaceBinding],
      resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 },
      expires_at: '', last_active_at: '', created_at: '',
    };
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [host] });
    vi.spyOn(portalApi, 'templates').mockResolvedValue({ items: [] });
    const restore = vi.spyOn(portalApi, 'restoreHost').mockResolvedValue({ ...host, state: 'creating', runtime_generation: 2 });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const user = userEvent.setup();
    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    await screen.findByRole('button', { name: `查看云端电脑 ${host.name}` });
    await openMoreActions(user, { id: host.id, name: host.name });
    await user.click(await screen.findByRole('button', { name: '恢复云端电脑 我的云端电脑' }));
    expect(screen.getByText(/保留原有文件/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '恢复主机' }));
    await waitFor(() => expect(restore).toHaveBeenCalledWith(host.id));
  });

  it('copies a stopped host from its confirmed Workspace revision and reuses the idempotency key on retry', async () => {
    const host: Host = { ...ownedHost, state: 'stopped' };
    const copiedHost: Host = {
      ...host, id: 'h7654321', name: '复制的测试主机',
      workspaces: [{ ...hostWorkspaceBinding, workspace: { ...hostWorkspaceBinding.workspace, id: 'w7654321', name: '复制的测试主机文件' } }],
    };
    const copyResult: Awaited<ReturnType<typeof portalApi.copyHost>> = {
      source_host_id: host.id, source_workspace_id: hostWorkspaceBinding.workspace.id, host: copiedHost,
      workspace: { id: 'w7654321', name: '复制的测试主机文件', revision: 1, state: 'ready' },
      source_revision: 2, state: 'completed', replayed: false,
    };
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [host] });
    vi.spyOn(portalApi, 'workspaces').mockResolvedValue({ items: [] });
    const copy = vi.spyOn(portalApi, 'copyHost')
      .mockRejectedValueOnce(new Error('网络超时'))
      .mockResolvedValue(copyResult);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    const user = userEvent.setup();
    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    await screen.findByRole('button', { name: `查看云端电脑 ${host.name}` });
    await openMoreActions(user, host);
    await user.click(await screen.findByRole('button', { name: `复制云端电脑 ${host.name}` }));
    expect(screen.getByText(/隐藏文件、配置和密钥/)).toBeInTheDocument();
    expect(screen.getByText(/运行环境、应用状态、平台凭据、任务和本机同步关联不会复制/)).toBeInTheDocument();
    const name = screen.getByLabelText('新电脑名称');
    await user.clear(name);
    await user.type(name, '复制的测试主机');
    await user.click(screen.getByRole('button', { name: '创建副本' }));
    expect(await screen.findByText(/网络超时/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '创建副本' }));

    await waitFor(() => expect(copy).toHaveBeenCalledTimes(2));
    expect(copy.mock.calls[0]![0]).toBe(host.id);
    expect(copy.mock.calls[0]![1]).toEqual({ name: '复制的测试主机', expected_current_revision: 2 });
    expect(copy.mock.calls[0]![2]).toBeTruthy();
    expect(copy.mock.calls[1]).toEqual(copy.mock.calls[0]);
    expect(await screen.findByText(/已创建云端电脑“复制的测试主机”/)).toBeInTheDocument();
  });

  it('disables host copy unless the owner has stopped the computer at a confirmed revision with AI control', async () => {
    const stopped: Host = { ...ownedHost, id: 'h7654321', name: '已停止电脑', state: 'stopped' };
    const stale: Host = { ...stopped, id: 'h7654322', name: '文件未确认电脑', workspaces: [{
      ...hostWorkspaceBinding, workspace: { ...hostWorkspaceBinding.workspace, revision: 3 },
    }] };
    const userControlled: Host = { ...stopped, id: 'h7654323', name: '人工控制电脑', control_state: 'user' };
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [ownedHost, stale, userControlled] });
    vi.spyOn(portalApi, 'workspaces').mockResolvedValue({ items: [] });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    const user = userEvent.setup();
    await screen.findByRole('button', { name: `查看云端电脑 ${ownedHost.name}` });
    await openMoreActions(user, ownedHost);
    await openMoreActions(user, { id: stale.id, name: stale.name });
    await openMoreActions(user, { id: userControlled.id, name: userControlled.name });
    expect(screen.getByRole('button', { name: `复制云端电脑 ${ownedHost.name}` })).toBeDisabled();
    expect(screen.getByRole('button', { name: '复制云端电脑 文件未确认电脑' })).toBeDisabled();
    expect(screen.getByRole('button', { name: '复制云端电脑 人工控制电脑' })).toBeDisabled();
    expect(screen.queryByRole('button', { name: '复制云端电脑 已停止电脑' })).not.toBeInTheDocument();
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

  it('refreshes the Runtime heartbeat while visible and pauses in the background', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
    const visibility = Object.getOwnPropertyDescriptor(document, 'visibilityState');
    let visibilityState: DocumentVisibilityState = 'visible';
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => visibilityState });
    const host: Host = { ...ownedHost, workspace_runtime_online: true };
    const hosts = vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [host] });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    try {
      expect(await screen.findByRole('button', { name: `查看云端电脑 ${host.name}` })).toBeEnabled();
      await act(async () => { await vi.advanceTimersByTimeAsync(12000); });
      expect(hosts).toHaveBeenCalledTimes(2);

      visibilityState = 'hidden';
      document.dispatchEvent(new Event('visibilitychange'));
      await act(async () => { await vi.advanceTimersByTimeAsync(12000); });
      expect(hosts).toHaveBeenCalledTimes(2);

      visibilityState = 'visible';
      document.dispatchEvent(new Event('visibilitychange'));
      await act(async () => { await vi.advanceTimersByTimeAsync(12000); });
      expect(hosts).toHaveBeenCalledTimes(3);
    } finally {
      if (visibility) Object.defineProperty(document, 'visibilityState', visibility);
      else Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' });
    }
  });

  it('loads owner-managed sharing grants only after opening the manager', async () => {
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [ownedHost] });
    const listMembers = vi.spyOn(portalApi, 'hostMembers').mockResolvedValue({ items: [] });
    const invite = vi.spyOn(portalApi, 'inviteHostMember');
    const revoke = vi.spyOn(portalApi, 'revokeHostMember');
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    const user = userEvent.setup();
    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    expect(await screen.findByRole('button', { name: '查看云端电脑 测试主机' })).toBeEnabled();
    await openHostDetails(user, ownedHost.id);
    const openManager = screen.getByRole('button', { name: /管理共享成员/ });
    expect(openManager).toBeInTheDocument();
    expect(listMembers).not.toHaveBeenCalled();
    await user.click(openManager);
    await waitFor(() => expect(listMembers).toHaveBeenCalledWith(ownedHost.id));
    expect(invite).not.toHaveBeenCalled();
    expect(revoke).not.toHaveBeenCalled();
  });

  it('opens a shared desktop through the same-origin handoff page', async () => {
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'sharedHosts').mockResolvedValue({ items: [{ id: ownedHost.id, name: '共享电脑', state: 'running', url: ownedHost.url, permissions: ['desktop:view'] }] });
    const ticket = vi.spyOn(portalApi, 'issueObservationTicket');
    const popup = { opener: window } as Window;
    const open = vi.spyOn(window, 'open').mockReturnValue(popup);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    const user = userEvent.setup();
    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    await user.click(await screen.findByRole('button', { name: '查看桌面' }));
    expect(open).toHaveBeenCalledWith(`${window.location.origin}/host-entry/${ownedHost.id}?target=desktop`, '_blank');
    expect(popup.opener).toBeNull();
    expect(ticket).not.toHaveBeenCalled();
  });

  it('gives a terminal-only member a terminal entry without showing desktop access', async () => {
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'sharedHosts').mockResolvedValue({ items: [{ id: ownedHost.id, name: '终端共享电脑', state: 'running', url: ownedHost.url, permissions: ['terminal:control'] }] });
    const ticket = vi.spyOn(portalApi, 'issueObservationTicket');
    const open = vi.spyOn(window, 'open').mockReturnValue({ opener: window } as Window);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    const user = userEvent.setup();
    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    expect(await screen.findByRole('button', { name: '打开终端' })).toBeEnabled();
    expect(screen.queryByRole('button', { name: '查看桌面' })).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '打开终端' }));
    expect(open).toHaveBeenCalledWith(`${window.location.origin}/host-entry/${ownedHost.id}?target=terminal`, '_blank');
    expect(ticket).not.toHaveBeenCalled();
  });

  it('gives a Workspace-only member a read-only Workspace entry without desktop or terminal access', async () => {
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'sharedHosts').mockResolvedValue({ items: [{ id: ownedHost.id, name: '工作区共享电脑', state: 'running', url: ownedHost.url, permissions: ['workspace:file:read'] }] });
    const ticket = vi.spyOn(portalApi, 'issueObservationTicket');
    const open = vi.spyOn(window, 'open').mockReturnValue({ opener: window } as Window);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    const user = userEvent.setup();
    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    expect(await screen.findByText('权限：读取云端电脑文件')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '查看电脑文件' })).toBeEnabled();
    expect(screen.queryByRole('button', { name: '查看桌面' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '打开终端' })).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '查看电脑文件' }));
    expect(open).toHaveBeenCalledWith(`${window.location.origin}/host-entry/${ownedHost.id}?target=workspace`, '_blank');
    expect(ticket).not.toHaveBeenCalled();
  });

  it('waits for AI tool stop confirmation before completing a Host takeover and resumes with the same request ID', async () => {
    let firstRequest: Parameters<typeof portalApi.hostControlAction>[1] | undefined;
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [ownedHost] });
    vi.spyOn(portalApi, 'hostControl')
      .mockResolvedValueOnce(aiControl(4))
      .mockImplementationOnce(async () => ({ ...aiControl(5), control_state: 'taking_over', active_transition: {
        request_id: firstRequest!.request_id, action: 'take_over', phase: 'draining', failure_reason: 'operations_pending',
        expected_control_version: firstRequest!.expected_control_version, blocked_control_version: 5, target_control_version: 6, created_at: '', updated_at: '',
      } }))
      .mockResolvedValue(userControl(6));
    const controlAction = vi.spyOn(portalApi, 'hostControlAction')
      .mockImplementationOnce(async (hostID, body) => {
        firstRequest = body;
        return {
          ...aiControl(5), host_id: hostID, control_state: 'taking_over',
          active_transition: {
            request_id: body.request_id, action: 'take_over', phase: 'draining', failure_reason: 'operations_pending',
            expected_control_version: body.expected_control_version, blocked_control_version: 5, target_control_version: 6, created_at: '', updated_at: '',
          }, pending_operation_ids: ['op1234567'],
        };
      })
      .mockResolvedValue({ ...userControl(6), pending_operation_ids: [] });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    const user = userEvent.setup();
    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    await openHostDetails(user, ownedHost.id);
    expect(await screen.findByText('电脑控制：云端 AI 控制')).toBeInTheDocument();
    await user.click(await screen.findByRole('button', { name: '接管操作' }));
    expect(screen.getByText(/工作台保持只读/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '开始接管' }));
    await waitFor(() => expect(controlAction).toHaveBeenCalledTimes(1));
    expect(await screen.findByText(/尚有 1 项操作等待停止确认/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '继续接管确认' })).toBeEnabled();

    await user.click(screen.getByRole('button', { name: '继续接管确认' }));
    await waitFor(() => expect(controlAction).toHaveBeenCalledTimes(2));
    expect(controlAction.mock.calls[1]![1]).toEqual(controlAction.mock.calls[0]![1]);
    expect(await screen.findByText('接管已完成，现在可以操作电脑。')).toBeInTheDocument();
    await openHostDetails(user, ownedHost.id);
    expect(await screen.findByText('电脑控制：人工控制')).toBeInTheDocument();
    expect(screen.getByText(`当前由你人工控制。交还完成后，云端 AI 才能恢复操作。`)).toBeInTheDocument();
  });

  it('confirms handing control back to AI and refreshes Host and Workspace versions', async () => {
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [{ ...ownedHost, control_state: 'user', control_version: 7 }] });
    vi.spyOn(portalApi, 'hostControl').mockResolvedValue(userControl(7));
    const controlAction = vi.spyOn(portalApi, 'hostControlAction').mockResolvedValue({
      ...aiControl(9), pending_operation_ids: [],
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    client.setQueryData(['workspaces'], { items: [] });
    const user = userEvent.setup();
    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    await openHostDetails(user, ownedHost.id);
    expect(await screen.findByText('电脑控制：人工控制')).toBeInTheDocument();
    await user.click(await screen.findByRole('button', { name: '交还 AI' }));
    expect(screen.getByText(/重新读取主机和工作区版本/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '交还给 AI' }));
    await waitFor(() => expect(controlAction).toHaveBeenCalledWith(ownedHost.id, {
      action: 'release', request_id: expect.any(String), expected_control_version: 7,
    }));
    await waitFor(() => expect(client.getQueryState(['workspaces'])?.isInvalidated).toBe(true));
    expect(await screen.findByText('已交还 AI，主机和工作区版本已刷新。')).toBeInTheDocument();
  });

  it('shows shared desktop control state to viewers without exposing control actions', async () => {
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'sharedHosts').mockResolvedValue({ items: [{
      id: ownedHost.id, name: '共享电脑', state: 'running', url: ownedHost.url, permissions: ['desktop:view'],
    }] });
    vi.spyOn(portalApi, 'hostControl').mockResolvedValue({
      ...userControl(22), control_actor_user_id: 'u8765432', control_lease_held_by_me: false,
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    const user = userEvent.setup();
    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    await screen.findByRole('button', { name: '查看桌面' });
    await openHostDetails(user, ownedHost.id);
    expect(await screen.findByText('控制版本：v22')).toBeInTheDocument();
    expect(screen.getByText(/当前由账号 u8765432人工控制/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '查看桌面' })).toBeEnabled();
    expect(screen.queryByRole('button', { name: '接管操作' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '交还 AI' })).not.toBeInTheDocument();
  });

  it('offers takeover to a shared desktop controller only after reading the current control state', async () => {
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'sharedHosts').mockResolvedValue({ items: [{
      id: ownedHost.id, name: '可操作共享电脑', state: 'running', url: ownedHost.url, permissions: ['desktop:control'],
    }] });
    vi.spyOn(portalApi, 'hostControl').mockResolvedValue(aiControl(8));
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    const user = userEvent.setup();
    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    await openHostDetails(user, ownedHost.id);
    expect(await screen.findByText('控制版本：v8')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '接管操作' }));
    expect(screen.getByRole('button', { name: '开始接管' })).toBeInTheDocument();
  });

  it('does not let a shared controller release another account’s active lease', async () => {
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'sharedHosts').mockResolvedValue({ items: [{
      id: ownedHost.id, name: '他人正在操作的电脑', state: 'running', url: ownedHost.url, permissions: ['desktop:control'],
    }] });
    vi.spyOn(portalApi, 'hostControl').mockResolvedValue({
      ...userControl(23), control_actor_user_id: 'u8765432', control_lease_held_by_me: false,
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    const user = userEvent.setup();
    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    await screen.findByRole('button', { name: '查看桌面' });
    await openHostDetails(user, ownedHost.id);
    expect(await screen.findByText('控制版本：v23')).toBeInTheDocument();
    expect(screen.getByText(/当前由账号 u8765432人工控制/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '交还 AI' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '接管操作' })).not.toBeInTheDocument();
  });

  it('refreshes control state after a stale control version conflict', async () => {
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [ownedHost] });
    vi.spyOn(portalApi, 'hostControl').mockResolvedValue(aiControl(9));
    const staleVersion = Object.assign(new Error('stale control version'), { code: 'conflict', status: 409 });
    const controlAction = vi.spyOn(portalApi, 'hostControlAction').mockRejectedValue(staleVersion);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    const user = userEvent.setup();
    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    await openHostDetails(user, ownedHost.id);
    expect(await screen.findByText('控制版本：v9')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '接管操作' }));
    await user.click(screen.getByRole('button', { name: '开始接管' }));
    await waitFor(() => expect(controlAction).toHaveBeenCalledWith(ownedHost.id, {
      action: 'take_over', request_id: expect.any(String), expected_control_version: 9,
    }));
    expect(await screen.findByText('控制状态已变化，已刷新最新状态，请重新操作。')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '开始接管' })).not.toBeInTheDocument();
  });

  it('renews the owner’s active control lease while the host list stays open', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [ownedHost] });
    vi.spyOn(portalApi, 'hostControl').mockResolvedValue(userControl(7));
    const renew = vi.spyOn(portalApi, 'renewHostControlLease').mockResolvedValue(userControl(7));
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const user = userEvent.setup();
    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Hosts /></MemoryRouter></ToastProvider></QueryClientProvider>);

    await screen.findByRole('button', { name: `查看云端电脑 ${ownedHost.name}` });
    await openHostDetails(user, ownedHost.id);
    expect(await screen.findByText(/操作租约：由你持有/)).toBeInTheDocument();
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });

    expect(renew).toHaveBeenCalledWith(ownedHost.id, 7);
  });
});
