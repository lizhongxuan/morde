import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { components } from '../api/schema';
import { portalApi } from '../app/api';
import type { PendingWorkspaceRestore } from '../app/workspace-restore';
import { ToastProvider } from '../app/toast';
import { Workspaces } from './Workspaces';

vi.mock('../app/auth', () => ({ useAuth: () => ({ user: { id: 'u1234567' } }) }));

const workspace: components['schemas']['Workspace'] = { id: 'w1234567', name: '项目工作区', revision: 2, used_bytes: 4096, quota_bytes: 1024 * 1024, is_read_only: false, state: 'ready', created_at: '', updated_at: '' };
const host: components['schemas']['Host'] = {
  id: 'h1234567', name: '开发电脑', owner_id: 'u1234567', template_id: 'std', isolation: 'container', state: 'running', state_reason: '', runtime_generation: 1, control_version: 1, control_state: 'ai', workspace_runtime_online: true, url: '',
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
  remaining_bytes: workspace.quota_bytes - workspace.used_bytes, quota_status: 'available' as const, quota_pauses: [] };
const projectCapabilities = {
  service_instance_id: '11111111-1111-4111-8111-111111111111', api_version: '1', hub_version: null,
  event_version: null, minimum_client_version: null, supported_features: ['workspace_sync', 'project_upload'],
  default_max_request_bytes: 1024 * 1024,
};
const projectTarget = {
  hostId: host.id, hostName: host.name, hostState: 'running' as const,
  workspaceId: workspace.id, workspaceName: workspace.name, folders: [],
};

function renderWorkspaces(initialWorkspaces?: components['schemas']['Workspace'][], initialHosts: components['schemas']['Host'][] = [], initialEntry = '/workspaces') {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, ...(initialWorkspaces ? { staleTime: Infinity } : {}) }, mutations: { retry: false } } });
  if (initialWorkspaces) {
    client.setQueryData(['workspaces'], { items: initialWorkspaces });
    client.setQueryData(['hosts'], { items: initialHosts });
  }
  return render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter initialEntries={[initialEntry]}><Workspaces /></MemoryRouter></ToastProvider></QueryClientProvider>);
}

function setProjectBridge(value: unknown) {
  Object.defineProperty(window, 'morde', { configurable: true, value });
}

describe('Workspaces', () => {
  beforeEach(() => {
    vi.spyOn(portalApi, 'workspaces').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [] });
  });
  afterEach(() => {
    cleanup();
    delete (window as Window & { morde?: unknown }).morde;
    localStorage.clear();
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  it('explains project directory selection is unavailable in the browser without calling service capabilities', async () => {
    const capabilities = vi.spyOn(portalApi, 'capabilities').mockResolvedValue(projectCapabilities);
    renderWorkspaces();

    expect(await screen.findByRole('status')).toHaveTextContent('本机目录选择需要 Morde 桌面客户端');
    expect(capabilities).not.toHaveBeenCalled();
    expect(screen.queryByRole('button', { name: /上传/ })).not.toBeInTheDocument();
  });

  it('fails closed when the project upload gate is not advertised and does not read target metadata', async () => {
    const projectUploadTargets = vi.fn().mockResolvedValue([projectTarget]);
    const selectProjectDirectory = vi.fn();
    setProjectBridge({ projectUploadTargets, selectProjectDirectory });
    const capabilities = vi.spyOn(portalApi, 'capabilities').mockResolvedValue({ ...projectCapabilities, supported_features: [] });
    renderWorkspaces();

    expect(await screen.findByText(/服务端尚未开放项目目录同步/)).toBeInTheDocument();
    expect(capabilities).toHaveBeenCalledTimes(1);
    expect(projectUploadTargets).not.toHaveBeenCalled();
    expect(selectProjectDirectory).not.toHaveBeenCalled();
    expect(screen.queryByRole('button', { name: /上传/ })).not.toBeInTheDocument();
  });

  it('does not read target metadata when service capabilities cannot be confirmed', async () => {
    const projectUploadTargets = vi.fn().mockResolvedValue([projectTarget]);
    setProjectBridge({ projectUploadTargets, selectProjectDirectory: vi.fn() });
    vi.spyOn(portalApi, 'capabilities').mockRejectedValue(new Error('capabilities unavailable'));
    renderWorkspaces();

    expect(await screen.findByText(/无法确认服务端项目同步能力/)).toBeInTheDocument();
    expect(projectUploadTargets).not.toHaveBeenCalled();
    expect(screen.queryByRole('button', { name: /上传/ })).not.toBeInTheDocument();
  });

  it('lets the user choose a target and local directory and confirm a project name without starting an upload', async () => {
    const projectUploadTargets = vi.fn().mockResolvedValue([projectTarget]);
    const selectProjectDirectory = vi.fn().mockResolvedValue({
      selectionID: '11111111-1111-4111-8111-111111111112', hostId: host.id, workspaceId: workspace.id,
      projectName: 'demo', directoryName: 'demo',
    });
    const uploadProject = vi.fn();
    setProjectBridge({ projectUploadTargets, selectProjectDirectory, uploadProject });
    vi.spyOn(portalApi, 'capabilities').mockResolvedValue(projectCapabilities);
    vi.spyOn(portalApi, 'preferences').mockResolvedValue({ assistant_name: 'Morde', selected_model_id: null, default_host_id: null, updated_at: null });
    const user = userEvent.setup();
    renderWorkspaces();

    expect(await screen.findByText(host.name)).toBeInTheDocument();
    expect(projectUploadTargets).toHaveBeenCalledTimes(1);
    await user.click(screen.getByRole('button', { name: '选择本机目录' }));

    expect(await screen.findByText('已选：demo')).toBeInTheDocument();
    expect(selectProjectDirectory).toHaveBeenCalledWith(host.id, workspace.id);
    const nameInput = screen.getByRole('textbox', { name: '项目名' });
    await user.clear(nameInput);
    await user.type(nameInput, 'release-notes');
    await user.click(screen.getByRole('button', { name: '确认项目名' }));

    expect(screen.getByText('项目名已确认：release-notes')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '上传功能等待 CFS-11 验收' })).toBeDisabled();
    expect(uploadProject).not.toHaveBeenCalled();
  });

  it('preselects the configured default computer and its bound Workspace', async () => {
    const alternateTarget = { ...projectTarget, hostId: 'h7654321', hostName: '备用电脑', workspaceId: 'w7654321', workspaceName: '备用工作区' };
    const projectUploadTargets = vi.fn().mockResolvedValue([alternateTarget, projectTarget]);
    setProjectBridge({ projectUploadTargets, selectProjectDirectory: vi.fn() });
    vi.spyOn(portalApi, 'capabilities').mockResolvedValue(projectCapabilities);
    const configuredHost = { ...host, workspaces: [binding] };
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [configuredHost] });
    vi.spyOn(portalApi, 'preferences').mockResolvedValue({ assistant_name: 'Morde', selected_model_id: null,
      default_host_id: configuredHost.id, updated_at: null });
    const user = userEvent.setup();
    renderWorkspaces([], [configuredHost]);

    const targetButtons = await screen.findAllByRole('button', { name: '使用此目标' });
    expect(targetButtons).toHaveLength(1);
    expect(targetButtons[0]).toHaveAttribute('aria-pressed', 'false');
    await user.click(screen.getByRole('button', { name: '下一页' }));
    const defaultTargetButton = await screen.findByRole('button', { name: '使用此目标' });
    expect(defaultTargetButton).toHaveAttribute('aria-pressed', 'true');
    expect(defaultTargetButton.closest('article')).toHaveTextContent('开发电脑');
    expect(defaultTargetButton.closest('article')).toHaveTextContent('默认');
  });

  it('explains when the configured default computer is unavailable and does not silently select another target', async () => {
    const alternateTarget = { ...projectTarget, hostId: 'h7654321', hostName: '备用电脑', workspaceId: 'w7654321', workspaceName: '备用工作区' };
    setProjectBridge({ projectUploadTargets: vi.fn().mockResolvedValue([alternateTarget]), selectProjectDirectory: vi.fn() });
    vi.spyOn(portalApi, 'capabilities').mockResolvedValue(projectCapabilities);
    const configuredHost = { ...host, workspaces: [binding] };
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [configuredHost] });
    vi.spyOn(portalApi, 'preferences').mockResolvedValue({ assistant_name: 'Morde', selected_model_id: null,
      default_host_id: configuredHost.id, updated_at: null });
    const user = userEvent.setup();
    renderWorkspaces([], [configuredHost]);

    expect(await screen.findByText(/设置中的默认电脑或其 Workspace 当前不可用于目录选择/)).toBeInTheDocument();
    const alternateTargetButton = screen.getByRole('button', { name: '使用此目标' });
    expect(alternateTargetButton).toHaveAttribute('aria-pressed', 'false');
    expect(screen.queryByRole('button', { name: '选择本机目录' })).not.toBeInTheDocument();

    await user.click(alternateTargetButton);
    expect(screen.getByRole('button', { name: '选择本机目录' })).toBeInTheDocument();
  });

  it('routes new cloud-file creation through cloud-computer creation', async () => {
    const create = vi.spyOn(portalApi, 'createWorkspace');
    renderWorkspaces();

    expect(screen.getByRole('link', { name: '＋ 创建云端电脑' })).toHaveAttribute('href', '/hosts/new');
    expect(screen.queryByLabelText('工作区名称')).not.toBeInTheDocument();
    expect(create).not.toHaveBeenCalled();
  });

  it('does not offer a second Workspace binding for a host that already has one', async () => {
    const otherWorkspaceBinding = { ...binding, workspace: { ...binding.workspace, id: 'w7654321', name: '另一个工作区' } };
    vi.mocked(portalApi.workspaces).mockResolvedValue({ items: [workspace] });
    vi.mocked(portalApi.hosts).mockResolvedValue({ items: [{ ...host, workspaces: [otherWorkspaceBinding] }] });
    renderWorkspaces();

    expect(await screen.findByText('暂无未绑定工作区的运行中主机')).toBeInTheDocument();
    expect(screen.queryByRole('combobox', { name: `工作区 ${workspace.name} 的绑定主机` })).not.toBeInTheDocument();
  });

  it('shows only the files bound to the computer opened from its card', async () => {
    const otherWorkspace = { ...workspace, id: 'w7654321', name: '其他电脑文件' };
    renderWorkspaces([workspace, otherWorkspace], [{ ...host, workspaces: [binding] }], `/workspaces?host=${host.id}`);

    expect(await screen.findByRole('heading', { name: '开发电脑 · 文件与版本' })).toBeInTheDocument();
    expect(screen.getByText(workspace.name)).toBeInTheDocument();
    expect(screen.queryByText(otherWorkspace.name)).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /新建云端文件空间/ })).not.toBeInTheDocument();
  });

  it('paginates workspaces and keeps each item action with its workspace', async () => {
    const second = { ...workspace, id: 'w7654321', name: '其他工作区' };
    const user = userEvent.setup();
    renderWorkspaces([workspace, second]);

    expect(screen.getByRole('heading', { name: workspace.name })).toBeVisible();
    expect(screen.queryByRole('heading', { name: second.name })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: `删除工作区 ${workspace.name}` })).toBeEnabled();
    await user.click(screen.getByRole('button', { name: '下一页' }));

    expect(screen.getByRole('heading', { name: second.name })).toBeVisible();
    expect(screen.queryByRole('heading', { name: workspace.name })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: `删除工作区 ${second.name}` })).toBeEnabled();
  });

  it('keeps the workspace list within the available 480×720 page viewport', () => {
    const longBinding = { ...binding, sync_state: 'conflict' as const,
      sync_conflicts: Array.from({ length: 5 }, (_, index) => `docs/long-conflict-name-${index}.md`) };
    const firstHost = { ...host, workspaces: [longBinding] };
    renderWorkspaces([workspace], [firstHost]);

    const pagination = screen.getByRole('region', { name: '云端工作区' });
    expect(pagination).toHaveStyle({ height: 'min(max(calc(100dvh - clamp(320px, 50dvh, 450px)), 36px), 620px)' });
    expect(Math.min(Math.max(720 - Math.min(Math.max(320, 720 * 0.5), 450), 36), 620)).toBe(360);
    const measuredContentHeight = 703;
    const previousPaginationHeight = 450;
    const availablePageHeight = 662;
    const pageChromeHeight = measuredContentHeight - previousPaginationHeight;
    expect(pageChromeHeight + 360).toBeLessThanOrEqual(availablePageHeight);
    expect(360 - 36).toBeGreaterThanOrEqual(270);
    const shortViewportListHeight = Math.min(Math.max(480 - Math.min(Math.max(320, 480 * 0.5), 450), 36), 620);
    expect(pageChromeHeight + shortViewportListHeight).toBeLessThanOrEqual(480 - 58);
    expect(screen.getByRole('article', { name: `工作区 ${workspace.name}` })).toBeVisible();
    expect(screen.getByText(/开发电脑 · 运行中/)).toBeVisible();
    expect(screen.getByText('docs/long-conflict-name-4.md')).toBeVisible();
  });

  it('shows host-read failures outside the page and retries without leaving it', async () => {
    vi.mocked(portalApi.hosts).mockRejectedValueOnce(new Error('电脑列表暂不可用')).mockResolvedValue({ items: [] });
    const user = userEvent.setup();
    renderWorkspaces();

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('无法读取主机绑定状态');
    expect(alert).toHaveTextContent('电脑列表暂不可用');
    expect(alert.closest('.toast-region')?.parentElement).toBe(document.body);
    await user.click(within(alert).getByRole('button', { name: '重试' }));
    await waitFor(() => expect(screen.queryByText(/无法读取主机绑定状态/)).not.toBeInTheDocument());
  });

  it('explains an initial workspace is waiting for its creating host, not a file transfer', async () => {
    const initial = { ...workspace, revision: 0, used_bytes: 0 };
    const initialBinding = { ...binding, workspace: { ...binding.workspace, revision: 0, used_bytes: 0 },
      state: 'preparing' as const, desired_revision: 0, applied_revision: 0 };
    const creatingHost = { ...host, state: 'creating' as const, workspace_runtime_online: false, workspaces: [initialBinding] };
    renderWorkspaces([initial], [creatingHost]);

    expect(screen.getByText('初始版本，暂无历史版本')).toBeInTheDocument();
    expect(screen.getByText('电脑创建中，工作区等待挂载。')).toBeInTheDocument();
    expect(screen.queryByText(/电脑运行环境未在线确认/)).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: '比较云端文件版本 项目工作区' })).toBeDisabled();
    expect(screen.getByRole('button', { name: '打开云端电脑 开发电脑' })).toBeDisabled();
    expect(screen.getByRole('button', { name: '查看设备同步进度 项目工作区' })).toBeEnabled();
  });

  it('does not treat a later zero-byte revision as an initial workspace', () => {
    renderWorkspaces([{ ...workspace, used_bytes: 0 }]);

    expect(screen.queryByText('初始版本，暂无历史版本')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: '比较云端文件版本 项目工作区' })).toBeEnabled();
  });

  it('does not claim a host is still being created when its recovery is uncertain', () => {
    const initial = { ...workspace, revision: 0, used_bytes: 0 };
    const initialBinding = { ...binding, workspace: { ...binding.workspace, revision: 0, used_bytes: 0 },
      state: 'preparing' as const, desired_revision: 0, applied_revision: 0 };
    renderWorkspaces([initial], [{ ...host, state: 'creating', state_reason: 'restore_unknown',
      workspace_runtime_online: false, workspaces: [initialBinding] }]);

    expect(screen.getByText('电脑恢复状态待核实，工作区尚未挂载。')).toBeInTheDocument();
  });

  it('still shows a sync conflict while the host is creating', () => {
    const initial = { ...workspace, revision: 0, used_bytes: 0 };
    const conflictBinding = { ...binding, workspace: { ...binding.workspace, revision: 0, used_bytes: 0 },
      state: 'preparing' as const, desired_revision: 0, applied_revision: 0,
      sync_state: 'conflict' as const, sync_conflicts: ['notes.txt'] };
    renderWorkspaces([initial], [{ ...host, state: 'creating', workspace_runtime_online: false, workspaces: [conflictBinding] }]);

    expect(screen.getByText('云端电脑文件冲突')).toBeInTheDocument();
    expect(screen.getByText('notes.txt')).toBeInTheDocument();
  });

  it('renders the server diff, metadata, and expandable text safely', async () => {
    const fileVersion = (path: string, hash: string) => ({ kind: 'file' as const, path, size_bytes: 4, sha256: hash, mode: 0o644 });
    const changes: components['schemas']['WorkspaceDiffChange'][] = [
      { path: 'legacy.txt', action: 'removed', before: fileVersion('legacy.txt', 'b'.repeat(64)), after: null, display_status: 'not_applicable', before_text: null, after_text: null },
      { path: 'new.txt', action: 'added', before: null, after: fileVersion('new.txt', 'c'.repeat(64)), display_status: 'text', before_text: null, after_text: '<script>demo</script>' },
      { path: 'config.json', action: 'changed', before: fileVersion('config.json', 'd'.repeat(64)), after: fileVersion('config.json', 'e'.repeat(64)), display_status: 'binary', before_text: null, after_text: null },
    ];
    const diff = vi.spyOn(portalApi, 'workspaceDiff').mockResolvedValue({ workspace_id: workspace.id, from_revision: 1, to_revision: 2, changes });
    const user = userEvent.setup();
    renderWorkspaces([workspace]);

    await user.click(await screen.findByRole('button', { name: `比较云端文件版本 ${workspace.name}` }));
    expect(screen.getByText(/小型文本可展开查看/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '比较版本' }));

    expect(await screen.findByText('new.txt')).toBeInTheDocument();
    expect(screen.getByText('legacy.txt')).toBeInTheDocument();
    expect(screen.getByText('config.json')).toBeInTheDocument();
    expect(screen.getAllByText('新增')).toHaveLength(1);
    expect(screen.getAllByText('移除')).toHaveLength(1);
    expect(screen.getAllByText('修改')).toHaveLength(1);
    expect(screen.getByText('二进制，不展示内容')).toBeInTheDocument();
    await user.click(screen.getByText('展开文本'));
    expect(screen.getByText('<script>demo</script>')).toBeInTheDocument();
    expect(diff).toHaveBeenCalledWith(workspace.id, 1, 2);
  });

  it('rejects an invalid revision before requesting the diff', async () => {
    const diff = vi.spyOn(portalApi, 'workspaceDiff');
    const user = userEvent.setup();
    renderWorkspaces([workspace]);

    await user.click(await screen.findByRole('button', { name: `比较云端文件版本 ${workspace.name}` }));
    const targetRevision = screen.getByLabelText('目标 revision');
    await user.clear(targetRevision);
    await user.type(targetRevision, '3');
    await user.click(screen.getByRole('button', { name: '比较版本' }));

    expect(await screen.findByRole('alert')).toHaveTextContent('revision 必须是 0 到当前版本 v2 之间的整数');
    expect(diff).not.toHaveBeenCalled();
  });

  it('paginates a long server diff list', async () => {
    const changes: components['schemas']['WorkspaceDiffChange'][] = Array.from({ length: 51 }, (_, index) => {
      const path = `added-${String(index).padStart(2, '0')}.txt`;
      return { path, action: 'added', before: null, after: { kind: 'file', path, size_bytes: 1, sha256: 'a'.repeat(64), mode: 0o644 }, display_status: 'metadata_only', before_text: null, after_text: null };
    });
    const diff = vi.spyOn(portalApi, 'workspaceDiff').mockResolvedValue({ workspace_id: workspace.id, from_revision: 1, to_revision: 2, changes });
    const user = userEvent.setup();
    renderWorkspaces([workspace]);

    await user.click(await screen.findByRole('button', { name: `比较云端文件版本 ${workspace.name}` }));
    await user.click(screen.getByRole('button', { name: '比较版本' }));

    expect(await screen.findByText('共 51 个路径差异；按路径分页显示。')).toBeInTheDocument();
    expect(screen.getByText('added-49.txt')).toBeInTheDocument();
    expect(screen.queryByText('added-50.txt')).not.toBeInTheDocument();
    expect(screen.getByText('第 1 / 2 页')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '下一页差异' }));
    expect(screen.getByText('added-50.txt')).toBeInTheDocument();
    expect(screen.getByText('第 2 / 2 页')).toBeInTheDocument();
    expect(diff).toHaveBeenCalledTimes(1);
  });

  it('rejects a diff response for the wrong Workspace or revision', async () => {
    const diff = vi.spyOn(portalApi, 'workspaceDiff').mockResolvedValue({ workspace_id: 'w7654321', from_revision: 1, to_revision: 2, changes: [] });
    const user = userEvent.setup();
    renderWorkspaces([workspace]);

    await user.click(await screen.findByRole('button', { name: `比较云端文件版本 ${workspace.name}` }));
    await user.click(screen.getByRole('button', { name: '比较版本' }));

    expect(await screen.findByRole('alert')).toHaveTextContent('历史差异响应的工作区、revision 或路径格式无效');
    expect(diff).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole('heading', { name: 'v1 → v2' })).not.toBeInTheDocument();
  });

  it('shows a conflict without displaying a partial server diff', async () => {
    const conflict = Object.assign(new Error('工作区历史版本已变化'), { code: 'workspace_conflict', status: 409 });
    const diff = vi.spyOn(portalApi, 'workspaceDiff').mockRejectedValue(conflict);
    const user = userEvent.setup();
    renderWorkspaces([workspace]);

    await user.click(await screen.findByRole('button', { name: `比较云端文件版本 ${workspace.name}` }));
    await user.click(screen.getByRole('button', { name: '比较版本' }));

    expect(await screen.findByRole('alert')).toHaveTextContent('历史 revision 冲突：工作区历史版本已变化');
    expect(diff).toHaveBeenCalledTimes(1);
  });

  it('confirms restore paths and reuses the same idempotency key after an uncertain response', async () => {
    const fileVersion = (path: string) => ({ kind: 'file' as const, path, size_bytes: 4, sha256: 'a'.repeat(64), mode: 0o644 });
    const changes: components['schemas']['WorkspaceDiffChange'][] = [
      { path: 'new.md', action: 'renamed', before: fileVersion('old.md'), after: fileVersion('new.md'), display_status: 'metadata_only', before_text: null, after_text: null },
      { path: 'docs', action: 'changed', before: { kind: 'directory', path: 'docs' }, after: { kind: 'directory', path: 'docs' }, display_status: 'not_applicable', before_text: null, after_text: null },
      { path: 'docs/guide.md', action: 'changed', before: fileVersion('docs/guide.md'), after: fileVersion('docs/guide.md'), display_status: 'metadata_only', before_text: null, after_text: null },
    ];
    vi.spyOn(portalApi, 'workspaceDiff').mockResolvedValue({ workspace_id: workspace.id, from_revision: 1, to_revision: 2, changes });
    const restoreResult: components['schemas']['WorkspaceRestoreResult'] = {
      workspace_id: workspace.id, target_revision: 1, expected_current_revision: 2, revision: 3, replayed: false,
    };
    let currentWorkspace = workspace;
    vi.mocked(portalApi.workspaces).mockImplementation(async () => ({ items: [currentWorkspace] }));
    const restore = vi.spyOn(portalApi, 'restoreWorkspacePaths')
      .mockRejectedValueOnce(new TypeError('服务器已接受请求，但响应丢失'))
      .mockImplementation(async () => {
        currentWorkspace = { ...workspace, revision: 3 };
        return restoreResult;
      });
    const user = userEvent.setup();
    renderWorkspaces([workspace]);

    await user.click(await screen.findByRole('button', { name: `比较云端文件版本 ${workspace.name}` }));
    await user.click(screen.getByRole('button', { name: '比较版本' }));
    await user.click(await screen.findByRole('button', { name: '从 v1 恢复 3 个路径' }));
    expect(screen.getByText(/以当前 v2 为条件创建新 revision/)).toBeInTheDocument();
    await user.click(screen.getByRole('checkbox', { name: /我已关闭可能修改这些云端文件/ }));
    await user.click(screen.getByRole('button', { name: '恢复文件并创建新版本' }));
    expect(await screen.findByText('服务器已接受请求，但响应丢失')).toBeInTheDocument();
    const pending = JSON.parse(localStorage.getItem('morde.pending-workspace-restore.u1234567') ?? 'null') as PendingWorkspaceRestore;
    expect(pending.phase).toBe('restore_workspace');
    expect(pending.restoreRequested).toBe(true);
    await user.click(screen.getByRole('button', { name: '核对状态并继续恢复' }));
    await user.click(screen.getByRole('checkbox', { name: /我已关闭可能修改这些云端文件/ }));
    await user.click(screen.getByRole('button', { name: '核对状态并继续' }));

    await waitFor(() => expect(restore).toHaveBeenCalledTimes(2));
    const firstRequest = restore.mock.calls[0]!;
    expect(firstRequest[0]).toBe(workspace.id);
    expect(firstRequest[1]).toEqual({ target_revision: 1, expected_current_revision: 2, paths: ['docs', 'new.md', 'old.md'] });
    expect(firstRequest[2]).toMatch(/^[A-Za-z0-9._~-]{8,128}$/);
    expect(restore.mock.calls[1]).toEqual(firstRequest);
    expect(await screen.findByText('云端文件已恢复为版本 v3，新版本文件已确认。')).toBeInTheDocument();
    expect(localStorage.getItem('morde.pending-workspace-restore.u1234567')).toBeNull();
  });

  it('unbinds, releases, restores, rebinds and restarts a running Host in order', async () => {
    const changes: components['schemas']['WorkspaceDiffChange'][] = [
      { path: 'notes.md', action: 'changed', before: { kind: 'file', path: 'notes.md', size_bytes: 1, sha256: 'a'.repeat(64), mode: 0o644 }, after: { kind: 'file', path: 'notes.md', size_bytes: 1, sha256: 'b'.repeat(64), mode: 0o644 }, display_status: 'metadata_only', before_text: null, after_text: null },
    ];
    let serverWorkspace = workspace;
    let serverHost = { ...host, workspaces: [binding] };
    vi.mocked(portalApi.workspaces).mockImplementation(async () => ({ items: [serverWorkspace] }));
    vi.mocked(portalApi.hosts).mockImplementation(async () => ({ items: [serverHost] }));
    vi.spyOn(portalApi, 'workspaceDiff').mockResolvedValue({ workspace_id: workspace.id, from_revision: 1, to_revision: 2, changes });
    const unbind = vi.spyOn(portalApi, 'unbindWorkspaceFromHost').mockImplementation(async () => {
      const detached = { ...binding, state: 'detached' as const, binding_version: 2 };
      serverHost = { ...serverHost, workspaces: [detached] };
      return detached;
    });
    const release = vi.spyOn(portalApi, 'releaseHost').mockImplementation(async () => {
      serverHost = { ...serverHost, state: 'stopped' };
      return serverHost;
    });
    const restore = vi.spyOn(portalApi, 'restoreWorkspacePaths').mockImplementation(async () => {
      serverWorkspace = { ...workspace, revision: 3 };
      return { workspace_id: workspace.id, target_revision: 1, expected_current_revision: 2, revision: 3, replayed: false };
    });
    const bindWorkspace = vi.spyOn(portalApi, 'bindWorkspaceToHost').mockImplementation(async () => {
      const preparing = { ...binding, state: 'preparing' as const, binding_version: 3, desired_revision: 3, workspace: { ...binding.workspace, revision: 3 } };
      serverHost = { ...serverHost, workspaces: [preparing] };
      return preparing;
    });
    const restoreHost = vi.spyOn(portalApi, 'restoreHost').mockImplementation(async () => {
      const active = { ...binding, state: 'active' as const, binding_version: 3, desired_revision: 3, applied_revision: 3, workspace: { ...binding.workspace, revision: 3 } };
      serverHost = { ...serverHost, state: 'running', workspace_runtime_online: true, workspaces: [active] };
      return serverHost;
    });
    const user = userEvent.setup();
    renderWorkspaces([workspace], [{ ...host, workspaces: [binding] }]);

    await user.click(await screen.findByRole('button', { name: '比较云端文件版本 ' + workspace.name }));
    await user.click(screen.getByRole('button', { name: '比较版本' }));
    await user.click(await screen.findByRole('button', { name: '安全恢复并重启 Host' }));
    const confirm = screen.getByRole('button', { name: '解除关联、恢复文件并重启电脑' });
    expect(confirm).toBeDisabled();
    await user.click(screen.getByRole('checkbox', { name: /我已关闭可能修改这些云端文件/ }));
    await user.click(confirm);

    expect(await screen.findByText(/云端电脑与新版本应用状态均已确认/)).toBeInTheDocument();
    expect(unbind).toHaveBeenCalledWith(host.id, workspace.id, { expected_binding_version: 1, writers_stopped: true }, expect.stringMatching(/^[A-Za-z0-9._~-]{8,128}$/));
    expect(release).toHaveBeenCalledWith(host.id, true);
    expect(restore).toHaveBeenCalledWith(workspace.id, { target_revision: 1, expected_current_revision: 2, paths: ['notes.md'] }, expect.stringMatching(/^[A-Za-z0-9._~-]{8,128}$/));
    expect(bindWorkspace).toHaveBeenCalledWith(host.id, { workspace_id: workspace.id, expected_revision: 3, expected_binding_version: 2 }, expect.stringMatching(/^[A-Za-z0-9._~-]{8,128}$/));
    expect(restoreHost).toHaveBeenCalledWith(host.id);
    expect(localStorage.getItem('morde.pending-workspace-restore.u1234567')).toBeNull();
  });

  it('pauses after an unknown unbind result and does not release the Host', async () => {
    const changes: components['schemas']['WorkspaceDiffChange'][] = [
      { path: 'notes.md', action: 'changed', before: { kind: 'file', path: 'notes.md', size_bytes: 1, sha256: 'a'.repeat(64), mode: 0o644 }, after: { kind: 'file', path: 'notes.md', size_bytes: 1, sha256: 'b'.repeat(64), mode: 0o644 }, display_status: 'metadata_only', before_text: null, after_text: null },
    ];
    vi.mocked(portalApi.workspaces).mockResolvedValue({ items: [workspace] });
    vi.mocked(portalApi.hosts).mockResolvedValue({ items: [{ ...host, workspaces: [binding] }] });
    vi.spyOn(portalApi, 'workspaceDiff').mockResolvedValue({ workspace_id: workspace.id, from_revision: 1, to_revision: 2, changes });
    const unbind = vi.spyOn(portalApi, 'unbindWorkspaceFromHost').mockRejectedValue(new TypeError('解绑结果未知'));
    const release = vi.spyOn(portalApi, 'releaseHost').mockResolvedValue({ ...host, state: 'stopped' });
    const restore = vi.spyOn(portalApi, 'restoreWorkspacePaths').mockResolvedValue({
      workspace_id: workspace.id, target_revision: 1, expected_current_revision: 2, revision: 3, replayed: false,
    });
    const user = userEvent.setup();
    renderWorkspaces([workspace], [{ ...host, workspaces: [binding] }]);

    await user.click(await screen.findByRole('button', { name: '比较云端文件版本 ' + workspace.name }));
    await user.click(screen.getByRole('button', { name: '比较版本' }));
    await user.click(await screen.findByRole('button', { name: '安全恢复并重启 Host' }));
    await user.click(screen.getByRole('checkbox', { name: /我已关闭可能修改这些云端文件/ }));
    await user.click(screen.getByRole('button', { name: '解除关联、恢复文件并重启电脑' }));

    expect(await screen.findByText('解绑结果未知')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '核对状态并继续恢复' })).toBeInTheDocument();
    const saved = JSON.parse(localStorage.getItem('morde.pending-workspace-restore.u1234567') ?? 'null') as PendingWorkspaceRestore;
    expect(saved.phase).toBe('unbind');
    expect(saved.unbindKey).toMatch(/^[A-Za-z0-9._~-]{8,128}$/);
    expect(release).not.toHaveBeenCalled();
    expect(restore).not.toHaveBeenCalled();
    expect(unbind).toHaveBeenCalledTimes(1);
  });

  it('keeps restore disabled when the compared target is not the current revision', async () => {
    vi.spyOn(portalApi, 'workspaceDiff').mockResolvedValue({ workspace_id: workspace.id, from_revision: 0, to_revision: 1, changes: [
      { path: 'old.txt', action: 'removed', before: { kind: 'file', path: 'old.txt', size_bytes: 1, sha256: 'a'.repeat(64), mode: 0o644 }, after: null, display_status: 'not_applicable', before_text: null, after_text: null },
    ] });
    const user = userEvent.setup();
    renderWorkspaces([workspace]);

    await user.click(await screen.findByRole('button', { name: `比较云端文件版本 ${workspace.name}` }));
    await user.clear(screen.getByLabelText('起始 revision'));
    await user.type(screen.getByLabelText('起始 revision'), '0');
    await user.clear(screen.getByLabelText('目标 revision'));
    await user.type(screen.getByLabelText('目标 revision'), '1');
    await user.click(screen.getByRole('button', { name: '比较版本' }));

    expect(await screen.findByRole('button', { name: '从 v0 恢复 1 个路径' })).toBeDisabled();
    expect(screen.getByText('恢复前请将目标 revision 设为当前版本。')).toBeInTheDocument();
  });

  it('opens a clearly local copy preview without creating a real Workspace or reading file content', async () => {
    const createWorkspace = vi.spyOn(portalApi, 'createWorkspace');
    const readManifest = vi.spyOn(portalApi, 'workspaceManifest');
    const user = userEvent.setup();
    renderWorkspaces([workspace]);

    await user.click(await screen.findByRole('button', { name: `预演复制与移交 ${workspace.name}` }));
    expect(screen.getByText(/不连接真实服务、不创建服务端工作区、不上传文件/)).toBeInTheDocument();
    await user.click(screen.getByRole('checkbox', { name: /我已检查快照可能包含/ }));
    await user.click(screen.getByRole('button', { name: '模拟快照并创建副本' }));

    expect(screen.getByText(/这不是服务端副本/)).toBeInTheDocument();
    expect(createWorkspace).not.toHaveBeenCalled();
    expect(readManifest).not.toHaveBeenCalled();
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
    localStorage.setItem('morde.pending-workspace-create.u1234567', JSON.stringify({ version: 1, name: '项目工作区', idempotencyKey: 'legacy-create-key' }));
    const user = userEvent.setup();
    const firstRender = renderWorkspaces();

    await user.click(screen.getByRole('button', { name: '继续上次创建' }));
    expect(await screen.findByText('创建失败：服务器已创建工作区，但响应丢失')).toBeInTheDocument();
    const storageKey = 'morde.pending-workspace-create.u1234567';
    const pending = JSON.parse(localStorage.getItem(storageKey) ?? 'null') as { name: string; idempotencyKey: string };
    expect(pending.name).toBe('项目工作区');
    expect(pending.idempotencyKey).toBe(create.mock.calls[0]?.[1]);

    firstRender.unmount();
    renderWorkspaces();
    expect(await screen.findByLabelText('历史文件名称')).toHaveValue('项目工作区');
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
    expect(screen.getByRole('button', { name: '创建工作区' })).toBeDisabled();
    expect(create).not.toHaveBeenCalled();

    await user.click(screen.getByRole('button', { name: '我已核实，清除记录' }));
    expect(localStorage.getItem('morde.pending-workspace-create.u1234567')).toBeNull();
    expect(screen.getByRole('link', { name: '＋ 创建云端电脑' })).toHaveAttribute('href', '/hosts/new');
    expect(create).not.toHaveBeenCalled();
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
    expect(await screen.findByText('服务端已接受关联，正在等待电脑应用云端文件。')).toBeInTheDocument();
    expect(await screen.findByText('服务端已保存')).toBeInTheDocument();
    expect(await screen.findByText('应用：已应用')).toBeInTheDocument();
    expect(screen.getByText('同步：已同步 v2')).toBeInTheDocument();
    expect(screen.getByText('项目：文件已就绪')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: '打开云端电脑 开发电脑' }));
    expect(open).toHaveBeenCalledWith(`${window.location.origin}/host-entry/${host.id}`, '_blank');
    expect(tab.opener).toBeNull();
    expect(enter).not.toHaveBeenCalled();
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

    expect(await screen.findByText('服务端已确认云端文件应用到电脑。')).toBeInTheDocument();
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
    await user.click(await screen.findByRole('button', { name: '解除云端文件与电脑 开发电脑 的关联' }));
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
    expect(await screen.findByText('已提交解除关联，服务端正在等待操作完成并校验文件。')).toBeInTheDocument();
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

    await user.click(await screen.findByRole('button', { name: '解除云端文件与电脑 开发电脑 的关联' }));
    await user.click(screen.getByRole('button', { name: '已停止写入者，确认解绑' }));

    expect(await screen.findByText('应用：正在解绑')).toBeInTheDocument();
    expect(await screen.findByText('工作区绑定版本已变化，请重试。')).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: '解绑工作区与主机 开发电脑' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '解除云端文件与电脑 开发电脑 的关联' })).not.toBeInTheDocument();
  });

  it('polls while deletion is pending and keeps the deleted state in the inventory', async () => {
    const workspaces = vi.spyOn(portalApi, 'workspaces')
      .mockResolvedValueOnce({ items: [{ ...workspace, state: 'deleting' }] })
      .mockResolvedValue({ items: [{ ...workspace, state: 'deleted' }] });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });

    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Workspaces /></MemoryRouter></ToastProvider></QueryClientProvider>);

    expect(await screen.findByText('删除中')).toBeInTheDocument();
    expect(screen.getByText('已用 / 配额')).toBeInTheDocument();
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
    expect(screen.getByText('项目：等待电脑应用文件')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '打开云端电脑 开发电脑' })).toBeDisabled();
  });

  it('does not mark a host with an older workspace revision as project-ready', async () => {
    const newerWorkspace = { ...workspace, revision: 3 };
    vi.mocked(portalApi.workspaces).mockResolvedValue({ items: [newerWorkspace] });
    vi.mocked(portalApi.hosts).mockResolvedValue({ items: [{ ...host, workspaces: [binding] }] });
    renderWorkspaces();

    expect(await screen.findByText('同步：待应用：电脑 v2 / 文件 v3')).toBeInTheDocument();
    expect(screen.getByText('项目：等待电脑应用文件')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '打开云端电脑 开发电脑' })).toBeDisabled();
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

    expect(await screen.findByText('云端电脑文件冲突')).toBeInTheDocument();
    expect(screen.getByText('src/file-1.go')).toBeInTheDocument();
    expect(screen.getByText('src/file-5.go')).toBeInTheDocument();
    expect(screen.queryByText('src/file-6.go')).not.toBeInTheDocument();
    expect(screen.getByText('另有 2 个冲突路径未展开')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: '重试电脑文件同步 项目工作区 到 开发电脑' }));

    await waitFor(() => expect(retry).toHaveBeenCalledWith(host.id, workspace.id, binding.binding_version));
    await waitFor(() => expect(hosts).toHaveBeenCalledTimes(2));
    expect(await screen.findByText('同步：已同步 v2')).toBeInTheDocument();
    expect(screen.queryByText('云端电脑文件冲突')).not.toBeInTheDocument();
  });

  it('polls active Host bindings so a new runtime conflict appears without a manual refresh', async () => {
    const hosts = vi.spyOn(portalApi, 'hosts')
      .mockResolvedValueOnce({ items: [{ ...host, workspaces: [binding] }] })
      .mockResolvedValue({ items: [{ ...host, workspaces: [{ ...binding, sync_state: 'conflict', sync_conflicts: ['src/main.go'] }] }] });
    vi.mocked(portalApi.workspaces).mockResolvedValue({ items: [workspace] });
    renderWorkspaces();

    expect(await screen.findByText('同步：已同步 v2')).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText('云端电脑文件冲突')).toBeInTheDocument(), { timeout: 5000 });
    expect(await screen.findByText('src/main.go')).toBeInTheDocument();
    expect(hosts).toHaveBeenCalledTimes(2);
  });

  it('shows conflict details without exposing retry controls to a non-owner', async () => {
    const conflictBinding = { ...binding, sync_state: 'conflict' as const, sync_conflicts: ['README.md'] };
    vi.mocked(portalApi.workspaces).mockResolvedValue({ items: [workspace] });
    vi.mocked(portalApi.hosts).mockResolvedValue({ items: [{ ...host, owner_id: 'u9999999', workspaces: [conflictBinding] }] });
    renderWorkspaces();

    expect(await screen.findByText('云端电脑文件冲突')).toBeInTheDocument();
    expect(screen.getByText('README.md')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /重试电脑文件同步/ })).not.toBeInTheDocument();
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

    expect(await screen.findByText('当前没有设备正在同步。设备连接后，仍可能有待同步的文件。')).toBeInTheDocument();
    await waitFor(() => expect(progress).toHaveBeenCalledTimes(2), { timeout: 4000 });
    expect(await screen.findByText('设备 t7654321 · 下载 · 应用中')).toBeInTheDocument();
  }, 10000);

  it('closes the device progress panel, stops polling, and allows reopening it', async () => {
    vi.mocked(portalApi.workspaces).mockResolvedValue({ items: [workspace] });
    const progress = vi.spyOn(portalApi, 'workspaceDeviceSyncProgress').mockResolvedValue({ ...deviceProgressSummary, items: [] });
    const user = userEvent.setup();
    renderWorkspaces();

    await user.click(await screen.findByRole('button', { name: '查看设备同步进度 项目工作区' }));
    expect(await screen.findByText('当前没有设备正在同步。设备连接后，仍可能有待同步的文件。')).toBeInTheDocument();
    await waitFor(() => expect(progress).toHaveBeenCalledTimes(1));

    await user.click(screen.getByRole('button', { name: '关闭设备同步进度' }));
    expect(screen.queryByText('当前没有设备正在同步。设备连接后，仍可能有待同步的文件。')).not.toBeInTheDocument();
    await new Promise((resolve) => setTimeout(resolve, 3200));
    expect(progress).toHaveBeenCalledTimes(1);

    await user.click(screen.getByRole('button', { name: '查看设备同步进度 项目工作区' }));
    await waitFor(() => expect(progress).toHaveBeenCalledTimes(2));
  }, 10000);
});
