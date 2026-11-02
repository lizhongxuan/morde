import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { gatewayApi, type GatewayHost } from '../api/gateway';
import { downloadFile, listFiles } from '../app/fileApi';
import WorkspaceBrowser from './WorkspaceBrowser';

vi.mock('../app/fileApi', () => ({ downloadFile: vi.fn(), listFiles: vi.fn() }));

const workspaceID = 'w1234567';
const rootPath = `/home/gem/workspace/${workspaceID}`;
const binding: GatewayHost['workspaces'][number] = {
  workspace: { id: workspaceID, name: '项目', revision: 4, used_bytes: 1536, quota_bytes: 5 * 1024 ** 3, state: 'ready' },
  directory: workspaceID, state: 'active', binding_version: 2, desired_revision: 4, applied_revision: 4,
  sync_state: 'healthy', sync_conflicts: [],
};
const host: GatewayHost = {
  id: 'h1234567', name: '测试主机', portal_origin: window.location.origin, state: 'running', expires_at: null,
  workspaces: [binding],
};

beforeEach(() => {
  vi.spyOn(gatewayApi, 'host').mockResolvedValue(host);
  vi.mocked(listFiles).mockResolvedValue([]);
  vi.mocked(downloadFile).mockResolvedValue(undefined);
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.useRealTimers();
});

describe('WorkspaceBrowser', () => {
  it('browses only the active Host binding and downloads a listed file', async () => {
    vi.mocked(listFiles).mockImplementation(async (path) => path === rootPath
      ? [{ name: 'src', path: `${rootPath}/src`, is_directory: true }]
      : [{ name: 'main.go', path: `${rootPath}/src/main.go`, is_directory: false, size: 1536 }]);
    render(<WorkspaceBrowser host={host} active canRead />);

    expect(await screen.findByText(/服务端 revision 4/)).toBeVisible();
    expect(screen.getByText(/1\.5 KB/)).toBeVisible();
    await waitFor(() => expect(listFiles).toHaveBeenCalledWith(rootPath, false));
    fireEvent.click(await screen.findByRole('button', { name: 'src/' }));
    await waitFor(() => expect(listFiles).toHaveBeenCalledWith(`${rootPath}/src`, false));
    fireEvent.click(await screen.findByRole('button', { name: '下载' }));

    await waitFor(() => expect(downloadFile).toHaveBeenCalledWith(`${rootPath}/src/main.go`, 'main.go'));
  });

  it('fails closed when the binding is stale or has a sync conflict', async () => {
    const conflicted: GatewayHost = { ...host, workspaces: [{ ...binding, sync_state: 'conflict', sync_conflicts: ['src/main.go'] }] };
    vi.spyOn(gatewayApi, 'host').mockResolvedValue(conflicted);
    render(<WorkspaceBrowser host={conflicted} active canRead />);

    expect(await screen.findByText(/同步冲突/)).toBeVisible();
    expect(screen.getByText('src/main.go')).toBeVisible();
    expect(screen.getByText(/文件暂不显示/)).toBeVisible();
    expect(listFiles).not.toHaveBeenCalled();
  });

  it('hides files when Workspace usage exceeds its quota', async () => {
    const overQuota: GatewayHost = { ...host, workspaces: [{ ...binding, workspace: { ...binding.workspace, used_bytes: binding.workspace.quota_bytes + 1 } }] };
    vi.spyOn(gatewayApi, 'host').mockResolvedValue(overQuota);
    render(<WorkspaceBrowser host={overQuota} active canRead />);

    expect(await screen.findByText('工作区用量超出或配额状态无效；文件暂不显示。')).toBeVisible();
    expect(listFiles).not.toHaveBeenCalled();
  });

  it('rejects a Host response whose workspace directory is not its workspace ID', async () => {
    const invalid: GatewayHost = { ...host, workspaces: [{ ...binding, directory: '/tmp/other' }] };
    vi.spyOn(gatewayApi, 'host').mockResolvedValue(invalid);
    render(<WorkspaceBrowser host={invalid} active canRead />);

    expect(await screen.findByRole('alert')).toHaveTextContent('工作区目录信息无效');
    expect(listFiles).not.toHaveBeenCalled();
  });

  it('rejects a directory listing that escapes the selected workspace', async () => {
    vi.mocked(listFiles).mockResolvedValue([{ name: 'secret.txt', path: '/etc/secret.txt', is_directory: false }]);
    render(<WorkspaceBrowser host={host} active canRead />);

    expect(await screen.findByRole('alert')).toHaveTextContent('越界路径');
    expect(screen.queryByRole('button', { name: '下载' })).not.toBeInTheDocument();
  });

  it('hides the file list if the Host status refresh becomes unavailable', async () => {
    vi.useFakeTimers();
    vi.mocked(listFiles).mockResolvedValue([{ name: 'README.md', path: `${rootPath}/README.md`, is_directory: false }]);
    const hostRequest = vi.spyOn(gatewayApi, 'host').mockResolvedValueOnce(host).mockRejectedValue(new Error('connection lost'));
    render(<WorkspaceBrowser host={host} active canRead />);
    await act(async () => { await Promise.resolve(); await Promise.resolve(); });
    expect(screen.getByText('README.md')).toBeVisible();

    await act(async () => { await vi.advanceTimersByTimeAsync(5000); });
    expect(screen.getByRole('alert')).toHaveTextContent('connection lost');
    expect(screen.queryByText('README.md')).not.toBeInTheDocument();
    expect(hostRequest).toHaveBeenCalledTimes(2);
  });
});
