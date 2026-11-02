import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError } from '../api/client';
import { gatewayApi, type GatewayWorkspaceBinding, type GatewayWorkspaceManifest } from '../api/gateway';
import WorkspaceBrowser from './WorkspaceBrowser';

const workspaceID = 'w1234567';
const binding: GatewayWorkspaceBinding = {
  workspace: { id: workspaceID, name: '项目', revision: 4, used_bytes: 1536, quota_bytes: 5 * 1024 ** 3, state: 'ready' },
  directory: workspaceID, state: 'active', binding_version: 2, desired_revision: 4, applied_revision: 4,
  sync_state: 'healthy', sync_conflicts: [],
};
const page = (files: GatewayWorkspaceManifest['files'], emptyDirectories: string[] = [], nextCursor: string | null = null): GatewayWorkspaceManifest => ({
  workspace_id: workspaceID, revision: 4, files, empty_directories: emptyDirectories, next_cursor: nextCursor,
});
const file = (path: string, sizeBytes: number): GatewayWorkspaceManifest['files'][number] => ({
  path, size_bytes: sizeBytes, sha256: 'a'.repeat(64), mode: 420, chunks: [{ sha256: 'a'.repeat(64), size_bytes: sizeBytes }],
});

beforeEach(() => {
  vi.spyOn(gatewayApi, 'workspaces').mockResolvedValue({ items: [binding] });
  vi.spyOn(gatewayApi, 'workspaceManifest').mockResolvedValue(page([file('README.md', 1536), file('src/main.go', 45)], ['empty']));
  vi.spyOn(gatewayApi, 'downloadWorkspaceFile').mockResolvedValue(undefined);
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe('WorkspaceBrowser', () => {
  it('browses the pinned manifest, follows cursors, and downloads a file at applied revision', async () => {
    const manifest = vi.mocked(gatewayApi.workspaceManifest);
    manifest
      .mockResolvedValueOnce(page([file('README.md', 1536)], ['empty'], 'cursor-1'))
      .mockResolvedValueOnce(page([file('src/main.go', 45)], ['src', 'src/empty']))
      .mockResolvedValueOnce(page([file('src/main.go', 45)]));
    render(<WorkspaceBrowser active />);

    expect(await screen.findByText(/服务端 revision 4 · Host 已应用 4 \/ 目标 4/)).toBeVisible();
    expect(await screen.findByRole('cell', { name: '1.5 KB' })).toBeVisible();
    fireEvent.click(await screen.findByRole('button', { name: '加载更多文件' }));
    await waitFor(() => expect(manifest).toHaveBeenNthCalledWith(2, workspaceID, 4, undefined, 'cursor-1', 500));
    fireEvent.click(screen.getByRole('button', { name: 'src/' }));
    expect(await screen.findByText('main.go')).toBeVisible();
    fireEvent.click(screen.getByRole('button', { name: '下载' }));

    await waitFor(() => expect(gatewayApi.downloadWorkspaceFile).toHaveBeenCalledWith(workspaceID, 4, 'src/main.go'));
  });

  it('hides files when the binding is stale or has a sync conflict', async () => {
    const conflicted = { ...binding, sync_state: 'conflict' as const, sync_conflicts: ['src/main.go'] };
    vi.spyOn(gatewayApi, 'workspaces').mockResolvedValue({ items: [conflicted] });
    render(<WorkspaceBrowser active />);

    expect(await screen.findByText('工作区尚未就绪、存在同步冲突或 Host revision 未追上；文件暂不显示。')).toBeVisible();
    expect(screen.getByText('src/main.go')).toBeVisible();
    expect(gatewayApi.workspaceManifest).not.toHaveBeenCalled();
  });

  it('hides files when Workspace usage exceeds its quota', async () => {
    const overQuota = { ...binding, workspace: { ...binding.workspace, used_bytes: binding.workspace.quota_bytes + 1 } };
    vi.spyOn(gatewayApi, 'workspaces').mockResolvedValue({ items: [overQuota] });
    render(<WorkspaceBrowser active />);

    expect(await screen.findByText('工作区用量超出或配额状态无效；文件暂不显示。')).toBeVisible();
    expect(gatewayApi.workspaceManifest).not.toHaveBeenCalled();
  });

  it('rejects a binding whose directory is not its workspace ID', async () => {
    vi.spyOn(gatewayApi, 'workspaces').mockResolvedValue({ items: [{ ...binding, directory: '/tmp/other' }] });
    render(<WorkspaceBrowser active />);

    expect(await screen.findByRole('alert')).toHaveTextContent('工作区绑定目录无效');
    expect(gatewayApi.workspaceManifest).not.toHaveBeenCalled();
  });

  it('fails closed when the Workspace binding response is malformed', async () => {
    vi.spyOn(gatewayApi, 'workspaces').mockResolvedValue({ items: [{} as GatewayWorkspaceBinding] });
    render(<WorkspaceBrowser active />);

    expect(await screen.findByRole('alert')).toHaveTextContent('Workspace binding 响应无效');
    expect(gatewayApi.workspaceManifest).not.toHaveBeenCalled();
  });

  it('rejects manifest entries outside the requested Workspace path', async () => {
    vi.spyOn(gatewayApi, 'workspaceManifest').mockResolvedValue(page([file('../secret.txt', 10)]));
    render(<WorkspaceBrowser active />);

    expect(await screen.findByRole('alert')).toHaveTextContent('无效或越界路径');
    expect(screen.queryByRole('button', { name: '下载' })).not.toBeInTheDocument();
  });

  it('shows a service permission error without reading a live Host projection', async () => {
    vi.spyOn(gatewayApi, 'workspaceManifest').mockRejectedValue(new ApiError({
      message: '没有 Workspace 文件读取权限', code: 'forbidden', status: 403,
    }));
    render(<WorkspaceBrowser active />);

    expect(await screen.findByRole('alert')).toHaveTextContent('没有 Workspace 文件读取权限');
    expect(gatewayApi.workspaces).toHaveBeenCalledOnce();
    expect(gatewayApi.downloadWorkspaceFile).not.toHaveBeenCalled();
  });

  it('removes already listed files if a later manifest page loses permission', async () => {
    vi.spyOn(gatewayApi, 'workspaceManifest')
      .mockResolvedValueOnce(page([file('README.md', 10)], [], 'cursor-1'))
      .mockRejectedValueOnce(new ApiError({ message: '访问权限已撤销', code: 'forbidden', status: 403 }));
    render(<WorkspaceBrowser active />);

    expect(await screen.findByText('README.md')).toBeVisible();
    fireEvent.click(screen.getByRole('button', { name: '加载更多文件' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('访问权限已撤销');
    expect(screen.queryByText('README.md')).not.toBeInTheDocument();
  });

  it('encodes nested file paths and sends only the same-origin Host cookie request', async () => {
    vi.mocked(gatewayApi.downloadWorkspaceFile).mockRestore();
    const requests: Array<{ url: URL; init?: RequestInit }> = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      requests.push({ url: new URL(String(input)), init });
      return new Response(new Blob(['content']), { status: 200, headers: { 'Content-Type': 'application/octet-stream' } });
    }));
    const createObjectURLDescriptor = Object.getOwnPropertyDescriptor(URL, 'createObjectURL');
    Object.defineProperty(URL, 'createObjectURL', { configurable: true, value: vi.fn(() => 'blob:workspace-test') });
    const revokeObjectURLDescriptor = Object.getOwnPropertyDescriptor(URL, 'revokeObjectURL');
    Object.defineProperty(URL, 'revokeObjectURL', { configurable: true, value: vi.fn() });
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined);
    try {
      await gatewayApi.downloadWorkspaceFile(workspaceID, 4, 'notes & plans/hello world.txt');
    } finally {
      if (createObjectURLDescriptor) Object.defineProperty(URL, 'createObjectURL', createObjectURLDescriptor);
      else Reflect.deleteProperty(URL, 'createObjectURL');
      if (revokeObjectURLDescriptor) Object.defineProperty(URL, 'revokeObjectURL', revokeObjectURLDescriptor);
      else Reflect.deleteProperty(URL, 'revokeObjectURL');
    }

    expect(requests).toHaveLength(1);
    expect(requests[0]?.url.origin).toBe(window.location.origin);
    expect(requests[0]?.url.pathname).toBe('/__ch/api/workspaces/w1234567/files/notes%20%26%20plans%2Fhello%20world.txt');
    expect(requests[0]?.url.searchParams.get('revision')).toBe('4');
    expect(requests[0]?.init?.credentials).toBe('include');
    expect(new Headers(requests[0]?.init?.headers).has('Authorization')).toBe(false);
  });

  it('hides a previously loaded manifest if binding refresh fails', async () => {
    vi.useFakeTimers();
    const request = vi.spyOn(gatewayApi, 'workspaces').mockResolvedValueOnce({ items: [binding] }).mockRejectedValue(new Error('connection lost'));
    vi.spyOn(gatewayApi, 'workspaceManifest').mockResolvedValue(page([file('README.md', 10)]));
    render(<WorkspaceBrowser active />);
    await act(async () => { await Promise.resolve(); await Promise.resolve(); await Promise.resolve(); });
    expect(screen.getByText('README.md')).toBeVisible();

    await act(async () => { await vi.advanceTimersByTimeAsync(5000); });
    expect(screen.getByRole('alert')).toHaveTextContent('connection lost');
    expect(screen.queryByText('README.md')).not.toBeInTheDocument();
    expect(request).toHaveBeenCalledTimes(2);
  });
});
