import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
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
  workspace_id: workspaceID, revision: 4, files, empty_directories: emptyDirectories, directories: [], next_cursor: nextCursor,
});
const file = (path: string, sizeBytes: number): GatewayWorkspaceManifest['files'][number] => ({
  path, kind: 'file', size_bytes: sizeBytes, sha256: 'a'.repeat(64), mode: 420, chunks: [{ sha256: 'a'.repeat(64), size_bytes: sizeBytes }],
});
const symlink = (path: string, target: string): GatewayWorkspaceManifest['files'][number] => ({
  path, kind: 'symlink', symlink_target: target, size_bytes: new TextEncoder().encode(target).length,
  sha256: 'b'.repeat(64), mode: 0, chunks: [],
});

beforeEach(() => {
  vi.spyOn(gatewayApi, 'workspaces').mockResolvedValue({ items: [binding] });
  vi.spyOn(gatewayApi, 'workspaceManifest').mockResolvedValue(page([file('README.md', 1536), file('src/main.go', 45)], ['empty']));
  vi.spyOn(gatewayApi, 'workspaceDiff').mockResolvedValue({ workspace_id: workspaceID, from_revision: 3, to_revision: 4, changes: [] });
  vi.spyOn(gatewayApi, 'downloadWorkspaceFile').mockResolvedValue(undefined);
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllEnvs();
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
    expect(screen.getByText('当前文件浏览只读；历史差异可查看，Workspace 恢复请在 Portal 中确认。')).toBeVisible();
    expect(screen.queryByRole('button', { name: /回滚/ })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /本机 Mock/ })).not.toBeInTheDocument();
    expect(await screen.findByRole('cell', { name: '1.5 KB' })).toBeVisible();
    fireEvent.click(await screen.findByRole('button', { name: '加载更多文件' }));
    await waitFor(() => expect(manifest).toHaveBeenNthCalledWith(2, workspaceID, 4, undefined, 'cursor-1', 500));
    fireEvent.click(screen.getByRole('button', { name: 'src/' }));
    expect(await screen.findByText('main.go')).toBeVisible();
    fireEvent.click(screen.getByRole('button', { name: '下载' }));

    await waitFor(() => expect(gatewayApi.downloadWorkspaceFile).toHaveBeenCalledWith(workspaceID, 4, 'src/main.go'));
  });

  it('loads a real read-only revision diff and safely renders bounded text', async () => {
    vi.mocked(gatewayApi.workspaceDiff).mockResolvedValue({
      workspace_id: workspaceID,
      from_revision: 3,
      to_revision: 4,
      changes: [
        { path: 'README.md', action: 'changed', before: { kind: 'file', path: 'README.md', size_bytes: 6 },
          after: { kind: 'file', path: 'README.md', size_bytes: 27 }, display_status: 'text',
          before_text: 'before', after_text: '<script>alert("x")</script>' },
        { path: 'archive.bin', action: 'added', before: null, after: { kind: 'file', path: 'archive.bin', size_bytes: 2048 },
          display_status: 'binary', before_text: null, after_text: null },
      ],
    });
    render(<WorkspaceBrowser active />);

    expect(await screen.findByText('README.md')).toBeVisible();
    expect(screen.getByRole('spinbutton', { name: '起始 revision' })).toHaveValue(3);
    expect(screen.getByRole('spinbutton', { name: '目标 revision' })).toHaveValue(4);
    fireEvent.click(screen.getByRole('button', { name: '查看历史差异' }));

    expect(await screen.findByText('revision 3 → 4，共 2 条路径差异。')).toBeVisible();
    expect(gatewayApi.workspaceDiff).toHaveBeenCalledWith(workspaceID, 3, 4);
    fireEvent.click(screen.getByText('查看小文件文本'));
    expect(screen.getByText('<script>alert("x")</script>')).toBeVisible();
    expect(document.querySelector('.workspace-diff-text-grid script')).toBeNull();
    expect(screen.getByText('二进制，仅显示元数据')).toBeVisible();
    expect(within(screen.getByRole('region', { name: 'Workspace 历史差异' })).queryByRole('button', { name: /恢复|回滚|下载/ })).not.toBeInTheDocument();
  });

  it('clears diff access errors and rejects malformed paths from the Gateway response', async () => {
    vi.mocked(gatewayApi.workspaceDiff).mockRejectedValueOnce(new ApiError({
      message: 'forbidden', code: 'forbidden', status: 403,
    })).mockResolvedValueOnce({ workspace_id: workspaceID, from_revision: 3, to_revision: 4, changes: [
      { path: '../private.txt', action: 'added', before: null, after: null, display_status: 'metadata_only', before_text: null, after_text: null },
    ] });
    render(<WorkspaceBrowser active />);

    expect(await screen.findByText('README.md')).toBeVisible();
    fireEvent.click(screen.getByRole('button', { name: '查看历史差异' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('当前账号无权读取');
    fireEvent.click(screen.getByRole('button', { name: '查看历史差异' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('无效路径或字段');
    expect(screen.queryByText('../private.txt')).not.toBeInTheDocument();
  });

  it('lists a symlink as metadata and never offers to download its referent', async () => {
    vi.spyOn(gatewayApi, 'workspaceManifest').mockResolvedValue(page([symlink('current', '.env'), file('README.md', 10)]));
    render(<WorkspaceBrowser active />);

    expect(await screen.findByText('current')).toBeVisible();
    expect(screen.getByText('current').closest('tr')).toHaveTextContent('符号链接');
    expect(screen.getByText('current').closest('tr')).toHaveTextContent('不跟随链接');
    expect(screen.getByRole('button', { name: '下载' })).toBeInTheDocument();
    expect(screen.getAllByRole('button', { name: '下载' })).toHaveLength(1);
    expect(gatewayApi.downloadWorkspaceFile).not.toHaveBeenCalled();
  });

  it('rejects an unsafe symlink target before showing the manifest', async () => {
    vi.spyOn(gatewayApi, 'workspaceManifest').mockResolvedValue(page([symlink('link', '../../outside')]));
    render(<WorkspaceBrowser active />);

    expect(await screen.findByRole('alert')).toHaveTextContent('符号链接元数据无效');
    expect(screen.queryByText('link')).not.toBeInTheDocument();
    expect(gatewayApi.downloadWorkspaceFile).not.toHaveBeenCalled();
  });

  it('rejects volume-like symlink targets before showing the manifest', async () => {
    vi.spyOn(gatewayApi, 'workspaceManifest').mockResolvedValue(page([symlink('link', 'name:stream')]));
    render(<WorkspaceBrowser active />);

    expect(await screen.findByRole('alert')).toHaveTextContent('符号链接元数据无效');
    expect(screen.queryByText('link')).not.toBeInTheDocument();
  });

  it('shows a clearly synthetic metadata diff and rollback preview in local mock mode', async () => {
    vi.stubEnv('VITE_MOCK_API', '1');
    const manifest = vi.mocked(gatewayApi.workspaceManifest);
    render(<WorkspaceBrowser active />);

    expect(await screen.findByText('README.md')).toBeVisible();
    expect(screen.getByText(/本机 VITE_MOCK_API 测试替身/)).toBeVisible();
    fireEvent.click(screen.getByRole('button', { name: '生成本机 Mock 差异与回滚预演' }));

    expect(await screen.findByRole('heading', { name: '本机 Mock 回滚预演 · v4 → 模拟 v3' })).toBeVisible();
    expect(screen.getByText('回滚后修改')).toBeVisible();
    expect(screen.getByText('回滚后新增')).toBeVisible();
    expect(screen.getByText('回滚后移除')).toBeVisible();
    expect(screen.getByText(/不是实际历史快照|不是真实历史快照/)).toBeVisible();
    expect(screen.getByText(/没有修改服务端状态或文件正文/)).toBeVisible();
    expect(manifest).toHaveBeenCalledTimes(2);
    expect(manifest).toHaveBeenLastCalledWith(workspaceID, 4, undefined, undefined, 500);
    expect(gatewayApi.downloadWorkspaceFile).not.toHaveBeenCalled();
    expect(screen.getByRole('button', { name: '关闭预演' })).toBeVisible();
  });

  it('stops the mock preview when the applied revision conflicts', async () => {
    vi.stubEnv('VITE_MOCK_API', '1');
    const conflict = new ApiError({ message: 'Workspace revision 已变化', code: 'workspace_conflict', status: 409 });
    vi.mocked(gatewayApi.workspaceManifest).mockResolvedValueOnce(page([file('README.md', 10)])).mockRejectedValueOnce(conflict);
    render(<WorkspaceBrowser active />);

    expect(await screen.findByText('README.md')).toBeVisible();
    fireEvent.click(screen.getByRole('button', { name: '生成本机 Mock 差异与回滚预演' }));

    expect(await screen.findByRole('alert')).toHaveTextContent('版本冲突：Workspace revision 已变化');
    expect(screen.queryByRole('heading', { name: /本机 Mock 回滚预演/ })).not.toBeInTheDocument();
    expect(gatewayApi.downloadWorkspaceFile).not.toHaveBeenCalled();
  });

  it('requires confirmation and labels a successful mock restore without changing Workspace data', async () => {
    vi.stubEnv('VITE_MOCK_API', '1');
    const fetch = vi.fn();
    vi.stubGlobal('fetch', fetch);
    const manifest = vi.mocked(gatewayApi.workspaceManifest);
    render(<WorkspaceBrowser active />);

    expect(await screen.findByText('README.md')).toBeVisible();
    fireEvent.click(screen.getByRole('button', { name: '生成本机 Mock 差异与回滚预演' }));
    expect(await screen.findByRole('heading', { name: /本机 Mock 回滚预演/ })).toBeVisible();
    fireEvent.click(screen.getByRole('button', { name: '本机 Mock 模拟回滚…' }));
    expect(screen.getByRole('region', { name: '确认本机 Mock 回滚' })).toBeVisible();
    expect(screen.getByText(/不会调用 API、修改 Workspace 或本机文件/)).toBeVisible();
    fireEvent.click(screen.getByRole('button', { name: '模拟恢复成功' }));

    expect(await screen.findByText('本机预演模拟恢复成功；没有发送写请求，Workspace 与本机文件均未改变。')).toBeVisible();
    expect(manifest).toHaveBeenCalledTimes(2);
    expect(fetch).not.toHaveBeenCalled();
    expect(gatewayApi.downloadWorkspaceFile).not.toHaveBeenCalled();
  });

  it('simulates a stale-revision conflict only after explicit mock confirmation', async () => {
    vi.stubEnv('VITE_MOCK_API', '1');
    const fetch = vi.fn();
    vi.stubGlobal('fetch', fetch);
    const manifest = vi.mocked(gatewayApi.workspaceManifest);
    render(<WorkspaceBrowser active />);

    expect(await screen.findByText('README.md')).toBeVisible();
    fireEvent.click(screen.getByRole('button', { name: '生成本机 Mock 差异与回滚预演' }));
    expect(await screen.findByRole('heading', { name: /本机 Mock 回滚预演/ })).toBeVisible();
    fireEvent.click(screen.getByRole('button', { name: '本机 Mock 模拟回滚…' }));
    fireEvent.click(screen.getByRole('button', { name: '模拟版本冲突' }));

    expect(await screen.findByRole('alert')).toHaveTextContent('本机预演模拟服务端返回版本冲突');
    expect(manifest).toHaveBeenCalledTimes(2);
    expect(fetch).not.toHaveBeenCalled();
    expect(gatewayApi.downloadWorkspaceFile).not.toHaveBeenCalled();
  });

  it('keeps mock metadata preview unavailable until workspace and Host revisions are ready', async () => {
    vi.stubEnv('VITE_MOCK_API', '1');
    vi.spyOn(gatewayApi, 'workspaces').mockResolvedValue({ items: [{ ...binding, applied_revision: 3 }] });
    render(<WorkspaceBrowser active />);

    expect(await screen.findByText('本机 Mock 差异与回滚预演要求 Workspace 与 Host revision 已同步就绪；当前未就绪，预演已停用。')).toBeVisible();
    expect(screen.queryByRole('button', { name: '生成本机 Mock 差异与回滚预演' })).not.toBeInTheDocument();
    expect(gatewayApi.workspaceManifest).not.toHaveBeenCalled();
  });

  it('reports a manifest permission failure without attempting rollback', async () => {
    vi.stubEnv('VITE_MOCK_API', '1');
    const forbidden = new ApiError({ message: '没有 revision 清单权限', code: 'forbidden', status: 403 });
    vi.mocked(gatewayApi.workspaceManifest).mockResolvedValueOnce(page([file('README.md', 10)])).mockRejectedValueOnce(forbidden);
    render(<WorkspaceBrowser active />);

    expect(await screen.findByText('README.md')).toBeVisible();
    fireEvent.click(screen.getByRole('button', { name: '生成本机 Mock 差异与回滚预演' }));

    expect(await screen.findByRole('alert')).toHaveTextContent('无权读取 revision manifest 元数据：没有 revision 清单权限');
    expect(screen.queryByRole('heading', { name: /本机 Mock 回滚预演/ })).not.toBeInTheDocument();
    expect(gatewayApi.downloadWorkspaceFile).not.toHaveBeenCalled();
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
