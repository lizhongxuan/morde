import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { components } from '../api/schema';
import { portalApi } from '../app/api';
import { ToastProvider } from '../app/toast';
import { Workspaces } from './Workspaces';

vi.mock('../app/auth', () => ({ useAuth: () => ({ user: { id: 'u1234567' } }) }));

const workspace: components['schemas']['Workspace'] = { id: 'w1234567', name: '项目工作区', revision: 2, used_bytes: 4096, quota_bytes: 1024 * 1024, state: 'ready', created_at: '', updated_at: '' };
const host: components['schemas']['Host'] = {
  id: 'h1234567', name: '开发电脑', owner_id: 'u1234567', template_id: 'std', isolation: 'container', state: 'running', state_reason: '', runtime_generation: 1, control_version: 1, control_state: 'ai', url: '',
  workspaces: [], resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 }, expires_at: null, last_active_at: null, created_at: '',
};
const binding: components['schemas']['WorkspaceBinding'] = {
  workspace: { id: workspace.id, name: workspace.name, revision: workspace.revision, used_bytes: workspace.used_bytes, state: workspace.state },
  directory: 'projects/w1234567', state: 'active', binding_version: 1, desired_revision: 2, applied_revision: 2,
  sync_state: 'healthy', sync_conflicts: [],
};
const deviceProgress: components['schemas']['DeviceWorkspaceSyncProgress'] = {
  workspace_id: workspace.id, device_id: 't7654321', session_id: 'p1234567', direction: 'download',
  state: 'applying', stage: 'apply', planned_bytes: 2048, confirmed_bytes: 1024,
  download_actions_total: 3, download_actions_done: 2, updated_at: '2026-10-24T00:00:00Z',
};
const deviceProgressSummary = { revision: workspace.revision, used_bytes: workspace.used_bytes, quota_bytes: workspace.quota_bytes,
  remaining_bytes: workspace.quota_bytes - workspace.used_bytes, quota_status: 'available' as const };

function renderWorkspaces() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Workspaces /></MemoryRouter></ToastProvider></QueryClientProvider>);
}

describe('Workspaces', () => {
  beforeEach(() => {
    vi.spyOn(portalApi, 'workspaces').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [] });
  });
  afterEach(() => { cleanup(); localStorage.clear(); vi.restoreAllMocks(); vi.useRealTimers(); });

  it('creates a standalone workspace through the API', async () => {
    const create = vi.spyOn(portalApi, 'createWorkspace').mockResolvedValue(workspace);
    const user = userEvent.setup();
    renderWorkspaces();

    await user.type(await screen.findByLabelText('工作区名称'), '  客户项目  ');
    await user.click(screen.getByRole('button', { name: '创建工作区' }));

    await waitFor(() => expect(create).toHaveBeenCalledWith({ name: '客户项目' }, expect.stringMatching(/^[A-Za-z0-9._~-]{8,128}$/)));
    expect(await screen.findByText('工作区已在服务端创建并保存。')).toBeInTheDocument();
  });

  it('recovers the same create key after the server accepted a workspace but its response was lost', async () => {
    const accepted = new Map<string, typeof workspace>();
    let attempts = 0;
    const create = vi.spyOn(portalApi, 'createWorkspace').mockImplementation(async (_body, key) => {
      const idempotencyKey = key!;
      let result = accepted.get(idempotencyKey);
      if (!result) {
        result = workspace;
        accepted.set(idempotencyKey, result);
      }
      if (attempts++ === 0) throw new TypeError('服务器已创建工作区，但响应丢失');
      return result;
    });
    const user = userEvent.setup();
    const firstRender = renderWorkspaces();

    await user.type(await screen.findByLabelText('工作区名称'), '项目工作区');
    await user.click(screen.getByRole('button', { name: '创建工作区' }));
    expect(await screen.findByText('创建失败：服务器已创建工作区，但响应丢失')).toBeInTheDocument();
    const storageKey = 'morde.pending-workspace-create.u1234567';
    const pending = JSON.parse(localStorage.getItem(storageKey) ?? 'null') as { name: string; idempotencyKey: string };
    expect(pending.name).toBe('项目工作区');
    expect(pending.idempotencyKey).toBe(create.mock.calls[0]?.[1]);

    firstRender.unmount();
    renderWorkspaces();
    expect(await screen.findByLabelText('工作区名称')).toHaveValue('项目工作区');
    expect(screen.getByRole('button', { name: '继续上次创建' })).toBeEnabled();
    await user.click(screen.getByRole('button', { name: '继续上次创建' }));

    expect(await screen.findByText('工作区已在服务端创建并保存。')).toBeInTheDocument();
    expect(create).toHaveBeenCalledTimes(2);
    expect(create.mock.calls[1]?.[1]).toBe(create.mock.calls[0]?.[1]);
    expect(accepted.size).toBe(1);
    expect(localStorage.getItem(storageKey)).toBeNull();
  });

  it('fails closed for a corrupt recovery record until the user verifies and clears it', async () => {
    localStorage.setItem('morde.pending-workspace-create.u1234567', '{broken json');
    const create = vi.spyOn(portalApi, 'createWorkspace').mockResolvedValue(workspace);
    const user = userEvent.setup();
    renderWorkspaces();

    expect(await screen.findByRole('alert')).toHaveTextContent('无法读取本机上次创建记录');
    expect(screen.getByLabelText('工作区名称')).toBeDisabled();
    expect(screen.getByRole('button', { name: '创建工作区' })).toBeDisabled();
    expect(create).not.toHaveBeenCalled();

    await user.click(screen.getByRole('button', { name: '我已核实，清除记录' }));
    expect(localStorage.getItem('morde.pending-workspace-create.u1234567')).toBeNull();
    await user.type(screen.getByLabelText('工作区名称'), '新项目');
    await user.click(screen.getByRole('button', { name: '创建工作区' }));

    expect(await screen.findByText('工作区已在服务端创建并保存。')).toBeInTheDocument();
    expect(create).toHaveBeenCalledTimes(1);
  });

  it('binds an existing workspace to an owned running host and opens it after the API reports it applied', async () => {
    vi.mocked(portalApi.workspaces).mockResolvedValue({ items: [workspace] });
    vi.mocked(portalApi.hosts).mockResolvedValueOnce({ items: [host] }).mockResolvedValue({ items: [{ ...host, workspaces: [binding] }] });
    const bind = vi.spyOn(portalApi, 'bindWorkspaceToHost').mockResolvedValue({ ...binding, state: 'preparing', applied_revision: 1 });
    const enter = vi.spyOn(portalApi, 'enterHost');
    const tab = { opener: window };
    const open = vi.spyOn(window, 'open').mockReturnValue(tab as unknown as Window);
    const user = userEvent.setup();
    renderWorkspaces();

    await user.click(await screen.findByRole('button', { name: '绑定' }));
    await waitFor(() => expect(bind).toHaveBeenCalledWith(host.id, { workspace_id: workspace.id, expected_revision: workspace.revision, expected_binding_version: 0 }));
    expect(await screen.findByText('服务端已接受绑定，正在等待主机应用工作区。')).toBeInTheDocument();
    expect(await screen.findByText('服务端已保存')).toBeInTheDocument();
    expect(await screen.findByText('应用：已应用')).toBeInTheDocument();
    expect(screen.getByText('同步：已同步 v2')).toBeInTheDocument();
    expect(screen.getByText('项目：文件已就绪')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: '打开主机 开发电脑' }));
    expect(open).toHaveBeenCalledWith(`${window.location.origin}/host-entry/${host.id}`, '_blank');
    expect(tab.opener).toBeNull();
    expect(enter).not.toHaveBeenCalled();
    expect(screen.getByText(/Host 应用与设备传输进度分别显示/)).toBeInTheDocument();
  });

  it('preserves a newer host selection when an earlier bind request completes', async () => {
    const secondHost = { ...host, id: 'h2234567', name: '备用电脑' };
    vi.mocked(portalApi.workspaces).mockResolvedValue({ items: [workspace] });
    vi.mocked(portalApi.hosts).mockResolvedValue({ items: [host, secondHost] });
    let finishBind!: (value: typeof binding) => void;
    vi.spyOn(portalApi, 'bindWorkspaceToHost').mockImplementation(() => new Promise((resolve) => { finishBind = resolve; }));
    const user = userEvent.setup();
    renderWorkspaces();

    const hostSelect = await screen.findByRole('combobox', { name: '工作区 项目工作区 的绑定主机' });
    await user.selectOptions(hostSelect, host.id);
    await user.click(screen.getByRole('button', { name: '绑定' }));
    await user.selectOptions(hostSelect, secondHost.id);
    finishBind(binding);

    expect(await screen.findByText('服务端已确认工作区应用到主机。')).toBeInTheDocument();
    expect(screen.getByRole('combobox', { name: '工作区 项目工作区 的绑定主机' })).toHaveValue(secondHost.id);
  });

  it('requires explicit stopped-writers confirmation before unbinding and refreshes retained workspace state', async () => {
    vi.mocked(portalApi.workspaces).mockResolvedValue({ items: [workspace] });
    vi.mocked(portalApi.hosts).mockResolvedValueOnce({ items: [{ ...host, workspaces: [binding] }] })
      .mockResolvedValue({ items: [{ ...host, workspaces: [{ ...binding, state: 'unbinding', binding_version: 2 }] }] });
    const unbind = vi.spyOn(portalApi, 'unbindWorkspaceFromHost').mockResolvedValue({ ...binding, state: 'unbinding', binding_version: 2 });
    const user = userEvent.setup();
    renderWorkspaces();

    expect(await screen.findByRole('button', { name: '删除工作区 项目工作区' })).toBeDisabled();
    await user.click(await screen.findByRole('button', { name: '解绑工作区 项目工作区 与主机 开发电脑' }));
    expect(screen.getByRole('heading', { name: '解绑工作区与主机 开发电脑' })).toBeInTheDocument();
    expect(screen.getByText(/Morde 无法自动检测外部写入者.*关闭会写入该工作区的 AI、IDE、终端任务和其他应用/)).toBeInTheDocument();
    expect(screen.getByText(/解绑不会删除服务端工作区或主机磁盘副本/)).toBeInTheDocument();
    expect(unbind).not.toHaveBeenCalled();

    await user.click(screen.getByRole('button', { name: '已停止写入者，确认解绑' }));

    await waitFor(() => expect(unbind).toHaveBeenCalledWith(host.id, workspace.id, {
      expected_binding_version: binding.binding_version,
      writers_stopped: true,
    }));
    expect(await screen.findByText('应用：正在解绑')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '删除工作区 项目工作区' })).toBeDisabled();
    expect(await screen.findByText('已提交解绑，服务端正在等待操作完成并校验工作区。')).toBeInTheDocument();
    await waitFor(() => {
      expect(portalApi.hosts).toHaveBeenCalledTimes(2);
      expect(portalApi.workspaces).toHaveBeenCalledTimes(2);
    });
  });

  it('keeps deletion disabled while refreshing a stale cached host list', async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    vi.spyOn(portalApi, 'workspaces').mockResolvedValue({ items: [workspace] });
    client.setQueryData(['workspaces'], { items: [workspace] });
    client.setQueryData(['hosts'], { items: [] });
    let finishHosts!: (value: { items: typeof host[] }) => void;
    const hosts = vi.spyOn(portalApi, 'hosts').mockImplementation(() => new Promise((resolve) => { finishHosts = resolve; }));

    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Workspaces /></MemoryRouter></ToastProvider></QueryClientProvider>);

    await waitFor(() => expect(hosts).toHaveBeenCalled());
    const deleteButton = await screen.findByRole('button', { name: '删除工作区 项目工作区' });
    expect(deleteButton).toBeDisabled();

    finishHosts({ items: [] });
    await waitFor(() => expect(deleteButton).toBeEnabled());
  });

  it('refreshes bindings and closes a stale delete confirmation after the server reports a conflict', async () => {
    const boundHost = { ...host, workspaces: [binding] };
    const hosts = vi.spyOn(portalApi, 'hosts').mockResolvedValueOnce({ items: [] }).mockResolvedValue({ items: [boundHost] });
    vi.spyOn(portalApi, 'workspaces').mockResolvedValue({ items: [workspace] });
    const remove = vi.spyOn(portalApi, 'deleteWorkspace').mockRejectedValue(new Error('工作区仍有主机绑定'));
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    const user = userEvent.setup();
    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Workspaces /></MemoryRouter></ToastProvider></QueryClientProvider>);

    await user.click(await screen.findByRole('button', { name: '删除工作区 项目工作区' }));
    expect(screen.getByRole('heading', { name: '删除工作区' })).toBeInTheDocument();
    expect(screen.getByText('确定删除“项目工作区”吗？这会永久删除 Morde 云端工作区及其已同步数据。本地文件夹和未同步的文件会保留。')).toBeInTheDocument();

    client.setQueryData(['hosts'], { items: [boundHost] });
    expect(await screen.findByText('开发电脑 · 运行中')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: /^删除工作区$/ }));

    expect(await screen.findByText('工作区仍有主机绑定')).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: '删除工作区' })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: '删除工作区 项目工作区' })).toBeDisabled();
    expect(remove).toHaveBeenCalledTimes(1);
    expect(remove.mock.calls[0]?.[0]).toBe(workspace.id);
    await waitFor(() => {
      expect(hosts).toHaveBeenCalledTimes(2);
      expect(portalApi.workspaces).toHaveBeenCalledTimes(2);
    });
  });

  it('refreshes the binding and closes a stale unbind confirmation after a version conflict', async () => {
    vi.mocked(portalApi.workspaces).mockResolvedValue({ items: [workspace] });
    vi.mocked(portalApi.hosts).mockResolvedValueOnce({ items: [{ ...host, workspaces: [binding] }] })
      .mockResolvedValue({ items: [{ ...host, workspaces: [{ ...binding, state: 'unbinding', binding_version: 2 }] }] });
    vi.spyOn(portalApi, 'unbindWorkspaceFromHost').mockRejectedValue(new Error('工作区绑定版本已变化，请重试。'));
    const user = userEvent.setup();
    renderWorkspaces();

    await user.click(await screen.findByRole('button', { name: '解绑工作区 项目工作区 与主机 开发电脑' }));
    await user.click(screen.getByRole('button', { name: '已停止写入者，确认解绑' }));

    expect(await screen.findByText('应用：正在解绑')).toBeInTheDocument();
    expect(await screen.findByText('工作区绑定版本已变化，请重试。')).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: '解绑工作区与主机 开发电脑' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '解绑工作区 项目工作区 与主机 开发电脑' })).not.toBeInTheDocument();
  });

  it('polls while deletion is pending and keeps the deleted state in the inventory', async () => {
    const workspaces = vi.spyOn(portalApi, 'workspaces')
      .mockResolvedValueOnce({ items: [{ ...workspace, state: 'deleting' }] })
      .mockResolvedValue({ items: [{ ...workspace, state: 'deleted' }] });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });

    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Workspaces /></MemoryRouter></ToastProvider></QueryClientProvider>);

    expect(await screen.findByText('删除中')).toBeInTheDocument();
    expect(screen.getByRole('columnheader', { name: '已用 / 配额' })).toBeInTheDocument();
    expect(screen.getByText('4,096 B / 1,048,576 B')).toBeInTheDocument();
    expect(workspaces).toHaveBeenCalledTimes(1);
    await waitFor(() => expect(workspaces).toHaveBeenCalledTimes(2), { timeout: 4000 });
    expect(await screen.findByText('已删除')).toBeInTheDocument();
    expect(workspaces).toHaveBeenCalledTimes(2);
    await new Promise((resolve) => setTimeout(resolve, 3200));
    expect(workspaces).toHaveBeenCalledTimes(2);
  }, 10000);

  it('shows the service workspace after the deleting host is removed', async () => {
    vi.spyOn(portalApi, 'workspaces').mockResolvedValue({ items: [{ ...workspace, name: '保留工作区' }] });
    const hosts = vi.spyOn(portalApi, 'hosts')
      .mockResolvedValueOnce({ items: [{ ...host, name: '正在删除的主机', state: 'deleting', workspaces: [{ ...binding, workspace: { ...binding.workspace, name: '保留工作区' } }] }] })
      .mockResolvedValue({ items: [] });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });

    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Workspaces /></MemoryRouter></ToastProvider></QueryClientProvider>);

    expect(await screen.findByText(/同步：已同步 v2/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '删除工作区 保留工作区' })).toBeDisabled();
    await waitFor(() => expect(hosts).toHaveBeenCalledTimes(2), { timeout: 4000 });
    expect(await screen.findByText('未绑定')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '删除工作区 保留工作区' })).toBeEnabled();
    await new Promise((resolve) => setTimeout(resolve, 3200));
    expect(hosts).toHaveBeenCalledTimes(2);
  }, 10000);

  it('does not claim a workspace binding succeeded when the API rejects it', async () => {
    vi.mocked(portalApi.workspaces).mockResolvedValue({ items: [workspace] });
    vi.mocked(portalApi.hosts).mockResolvedValue({ items: [host] });
    vi.spyOn(portalApi, 'bindWorkspaceToHost').mockRejectedValue(new Error('工作区版本已变化'));
    const user = userEvent.setup();
    renderWorkspaces();

    await user.click(await screen.findByRole('button', { name: '绑定' }));

    expect(await screen.findByText('工作区版本已变化')).toBeInTheDocument();
    expect(screen.queryByText(/服务端已接受绑定/)).not.toBeInTheDocument();
  });

  it('distinguishes server persistence from host application and project readiness', async () => {
    const waitingBinding = { ...binding, state: 'preparing' as const, applied_revision: 1 };
    vi.mocked(portalApi.workspaces).mockResolvedValue({ items: [workspace] });
    vi.mocked(portalApi.hosts).mockResolvedValue({ items: [{ ...host, workspaces: [waitingBinding] }] });
    renderWorkspaces();

    expect(await screen.findByText('服务端已保存')).toBeInTheDocument();
    expect(screen.getByText('应用：正在应用')).toBeInTheDocument();
    expect(screen.getByText('项目：等待主机应用')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '打开主机 开发电脑' })).toBeDisabled();
  });

  it('does not mark a host with an older workspace revision as project-ready', async () => {
    const newerWorkspace = { ...workspace, revision: 3 };
    vi.mocked(portalApi.workspaces).mockResolvedValue({ items: [newerWorkspace] });
    vi.mocked(portalApi.hosts).mockResolvedValue({ items: [{ ...host, workspaces: [binding] }] });
    renderWorkspaces();

    expect(await screen.findByText('同步：待同步：主机 v2 / 工作区 v3')).toBeInTheDocument();
    expect(screen.getByText('项目：等待文件同步')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '打开主机 开发电脑' })).toBeDisabled();
  });

  it('shows a bounded Host conflict list, lets only the owner retry, and refreshes hosts', async () => {
    const conflicts = Array.from({ length: 7 }, (_, index) => `src/file-${index + 1}.go`);
    const conflictBinding = { ...binding, sync_state: 'conflict' as const, sync_conflicts: conflicts };
    vi.mocked(portalApi.workspaces).mockResolvedValue({ items: [workspace] });
    const hosts = vi.mocked(portalApi.hosts)
      .mockResolvedValueOnce({ items: [{ ...host, workspaces: [conflictBinding] }] })
      .mockResolvedValue({ items: [{ ...host, workspaces: [binding] }] });
    const retry = vi.spyOn(portalApi, 'retryHostWorkspaceSync').mockResolvedValue(undefined);
    const user = userEvent.setup();
    renderWorkspaces();

    expect(await screen.findByText('Host 工作区同步冲突')).toBeInTheDocument();
    expect(screen.getByText('src/file-1.go')).toBeInTheDocument();
    expect(screen.getByText('src/file-5.go')).toBeInTheDocument();
    expect(screen.queryByText('src/file-6.go')).not.toBeInTheDocument();
    expect(screen.getByText('另有 2 个冲突路径未展开')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: '重试 Host 同步 项目工作区 到 开发电脑' }));

    await waitFor(() => expect(retry).toHaveBeenCalledWith(host.id, workspace.id, binding.binding_version));
    await waitFor(() => expect(hosts).toHaveBeenCalledTimes(2));
    expect(await screen.findByText('同步：已同步 v2')).toBeInTheDocument();
    expect(screen.queryByText('Host 工作区同步冲突')).not.toBeInTheDocument();
  });

  it('polls active Host bindings so a new runtime conflict appears without a manual refresh', async () => {
    const hosts = vi.spyOn(portalApi, 'hosts')
      .mockResolvedValueOnce({ items: [{ ...host, workspaces: [binding] }] })
      .mockResolvedValue({ items: [{ ...host, workspaces: [{ ...binding, sync_state: 'conflict', sync_conflicts: ['src/main.go'] }] }] });
    vi.mocked(portalApi.workspaces).mockResolvedValue({ items: [workspace] });
    renderWorkspaces();

    expect(await screen.findByText('同步：已同步 v2')).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText('Host 工作区同步冲突')).toBeInTheDocument(), { timeout: 5000 });
    expect(await screen.findByText('src/main.go')).toBeInTheDocument();
    expect(hosts).toHaveBeenCalledTimes(2);
  });

  it('shows conflict details without exposing retry controls to a non-owner', async () => {
    const conflictBinding = { ...binding, sync_state: 'conflict' as const, sync_conflicts: ['README.md'] };
    vi.mocked(portalApi.workspaces).mockResolvedValue({ items: [workspace] });
    vi.mocked(portalApi.hosts).mockResolvedValue({ items: [{ ...host, owner_id: 'u9999999', workspaces: [conflictBinding] }] });
    renderWorkspaces();

    expect(await screen.findByText('Host 工作区同步冲突')).toBeInTheDocument();
    expect(screen.getByText('README.md')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /重试 Host 同步/ })).not.toBeInTheDocument();
  });

  it('loads and labels only the authorized device sync summary without file details', async () => {
    vi.mocked(portalApi.workspaces).mockResolvedValue({ items: [workspace] });
    vi.spyOn(portalApi, 'workspaceDeviceSyncProgress').mockResolvedValue({ ...deviceProgressSummary, items: [deviceProgress] });
    const user = userEvent.setup();
    renderWorkspaces();

    await user.click(await screen.findByRole('button', { name: '查看设备同步进度 项目工作区' }));

    expect(await screen.findByText('设备 t7654321 · 下载 · 应用中')).toBeInTheDocument();
    expect(screen.getByText('已确认传输：1,024 B / 2,048 B')).toBeInTheDocument();
    expect(screen.getByText('已应用文件动作：2 / 3')).toBeInTheDocument();
    expect(screen.queryByText(/private\/path/)).not.toBeInTheDocument();
  });

  it('keeps looking for a session while the user is viewing an empty device progress panel', async () => {
    vi.mocked(portalApi.workspaces).mockResolvedValue({ items: [workspace] });
    const progress = vi.spyOn(portalApi, 'workspaceDeviceSyncProgress')
      .mockResolvedValueOnce({ ...deviceProgressSummary, items: [] })
      .mockResolvedValue({ ...deviceProgressSummary, items: [deviceProgress] });
    const user = userEvent.setup();
    renderWorkspaces();

    await user.click(await screen.findByRole('button', { name: '查看设备同步进度 项目工作区' }));

    expect(await screen.findByText('当前没有活动的设备同步会话；这不代表两端没有待同步差异。')).toBeInTheDocument();
    await waitFor(() => expect(progress).toHaveBeenCalledTimes(2), { timeout: 4000 });
    expect(await screen.findByText('设备 t7654321 · 下载 · 应用中')).toBeInTheDocument();
  }, 10000);

  it('closes the device progress panel, stops polling, and allows reopening it', async () => {
    vi.mocked(portalApi.workspaces).mockResolvedValue({ items: [workspace] });
    const progress = vi.spyOn(portalApi, 'workspaceDeviceSyncProgress').mockResolvedValue({ ...deviceProgressSummary, items: [] });
    const user = userEvent.setup();
    renderWorkspaces();

    await user.click(await screen.findByRole('button', { name: '查看设备同步进度 项目工作区' }));
    expect(await screen.findByText('当前没有活动的设备同步会话；这不代表两端没有待同步差异。')).toBeInTheDocument();
    await waitFor(() => expect(progress).toHaveBeenCalledTimes(1));

    await user.click(screen.getByRole('button', { name: '关闭设备同步进度' }));
    expect(screen.queryByText('当前没有活动的设备同步会话；这不代表两端没有待同步差异。')).not.toBeInTheDocument();
    await new Promise((resolve) => setTimeout(resolve, 3200));
    expect(progress).toHaveBeenCalledTimes(1);

    await user.click(screen.getByRole('button', { name: '查看设备同步进度 项目工作区' }));
    await waitFor(() => expect(progress).toHaveBeenCalledTimes(2));
  }, 10000);
});
