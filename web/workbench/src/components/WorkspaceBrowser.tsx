import { useEffect, useRef, useState } from 'react';
import { gatewayApi, type GatewayHost } from '../api/gateway';
import { downloadFile, listFiles, type AioFile } from '../app/fileApi';

// shortcut: bundled Host templates share this mount root; expose it in Gateway metadata if templates diverge.
const WORKSPACE_MOUNT_ROOT = '/home/gem/workspace';
const REFRESH_INTERVAL_MS = 5000;

type Binding = GatewayHost['workspaces'][number];

function validWorkspaceID(value: string): boolean {
  return /^w[a-z0-9]{7}$/.test(value);
}

function bindingPath(binding: Binding): string | null {
  if (!validWorkspaceID(binding.workspace.id) || binding.directory !== binding.workspace.id) return null;
  return `${WORKSPACE_MOUNT_ROOT}/${binding.directory}`;
}

function formatBytes(value: number): string {
  if (!Number.isFinite(value) || value < 0) return '未知';
  if (value < 1024) return `${value} B`;
  if (value < 1024 ** 2) return `${(value / 1024).toFixed(1)} KB`;
  if (value < 1024 ** 3) return `${(value / 1024 ** 2).toFixed(1)} MB`;
  return `${(value / 1024 ** 3).toFixed(1)} GB`;
}

function withinQuota(workspace: Binding['workspace']): boolean {
  return Number.isFinite(workspace.used_bytes) && workspace.used_bytes >= 0
    && Number.isFinite(workspace.quota_bytes) && workspace.quota_bytes > 0
    && workspace.used_bytes <= workspace.quota_bytes;
}

function readyBinding(host: GatewayHost | null, binding: Binding | undefined): boolean {
  return Boolean(host?.state === 'running' && binding?.workspace.state === 'ready' && binding.state === 'active'
    && withinQuota(binding.workspace)
    && binding.sync_state === 'healthy' && Array.isArray(binding.sync_conflicts) && binding.sync_conflicts.length === 0
    && binding.workspace.revision === binding.desired_revision && binding.applied_revision === binding.desired_revision);
}

function entryPath(directory: string, entry: AioFile): string | null {
  if (!entry.name || entry.name === '.' || entry.name === '..' || entry.name.includes('/')) return null;
  const expectedPath = `${directory.replace(/\/+$/, '')}/${entry.name}`;
  return entry.path === expectedPath ? expectedPath : null;
}

export default function WorkspaceBrowser({ host, active, canRead }: {
  host: GatewayHost | null;
  active: boolean;
  canRead: boolean;
}) {
  const [hostSnapshot, setHostSnapshot] = useState<{ id: string; value: GatewayHost } | null>(null);
  const [hostError, setHostError] = useState('');
  const [selectedID, setSelectedID] = useState('');
  const [navigation, setNavigation] = useState<{ workspaceID: string; segments: string[] }>({ workspaceID: '', segments: [] });
  const [fileState, setFileState] = useState<{ path: string; entries?: AioFile[]; error?: string } | null>(null);
  const [refreshNumber, setRefreshNumber] = useState(0);
  const [downloading, setDownloading] = useState('');
  const [downloadError, setDownloadError] = useState('');
  const [notice, setNotice] = useState('');
  const listRequestID = useRef(0);
  const currentHost = hostSnapshot && (!host || hostSnapshot.id === host.id) ? hostSnapshot.value : host;

  useEffect(() => {
    if (!active) return;
    let current = true;
    const refresh = async () => {
      try {
        const nextHost = await gatewayApi.host();
        if (!current) return;
        setHostSnapshot({ id: nextHost.id, value: nextHost });
        setHostError('');
      } catch (error) {
        if (!current) return;
        setHostError(error instanceof Error ? error.message : '无法读取主机工作区状态');
        setFileState(null);
      }
    };
    void refresh();
    const timer = window.setInterval(() => void refresh(), REFRESH_INTERVAL_MS);
    return () => {
      current = false;
      window.clearInterval(timer);
    };
  }, [active]);

  const bindings = currentHost?.workspaces ?? [];
  const binding = bindings.find((item) => item.workspace.id === selectedID) ?? bindings[0];
  const rootPath = binding ? bindingPath(binding) : null;
  const segments = binding && navigation.workspaceID === binding.workspace.id ? navigation.segments : [];
  const currentPath = rootPath ? [rootPath, ...segments].join('/') : '';
  const quotaReady = binding ? withinQuota(binding.workspace) : false;
  const bindingReady = readyBinding(currentHost, binding);
  const mayListFiles = Boolean(active && canRead && !hostError && bindingReady && rootPath);
  const fileList = fileState?.path === currentPath ? fileState : null;
  const loading = mayListFiles && !fileList;
  const fileError = fileList?.error ?? '';
  const files = fileList?.entries ?? [];

  useEffect(() => {
    const requestID = ++listRequestID.current;
    if (!mayListFiles || !currentPath) {
      return;
    }
    // shortcut: AIO is a live Host projection, not revision-pinned; replace with a Host-scoped manifest API when available.
    void listFiles(currentPath, false)
      .then((entries) => {
        if (requestID !== listRequestID.current) return;
        if (entries.some((entry) => entryPath(currentPath, entry) === null)) {
          throw new Error('工作区文件列表包含越界路径，已拒绝显示');
        }
        setFileState({ path: currentPath, entries: [...entries].sort((a, b) => Number(b.is_directory) - Number(a.is_directory) || a.name.localeCompare(b.name)) });
      })
      .catch((error: unknown) => {
        if (requestID !== listRequestID.current) return;
        setFileState({ path: currentPath, error: error instanceof Error ? error.message : '读取工作区目录失败' });
      });
  }, [currentPath, mayListFiles, refreshNumber]);

  async function openEntry(entry: AioFile) {
    const path = entryPath(currentPath, entry);
    if (!path || !mayListFiles) return;
    setNotice('');
    setDownloadError('');
    if (entry.is_directory) {
      setNavigation((current) => ({
        workspaceID: binding?.workspace.id ?? '',
        segments: [...(current.workspaceID === binding?.workspace.id ? current.segments : []), entry.name],
      }));
      return;
    }
    setDownloading(path);
    try {
      await downloadFile(path, entry.name);
      setNotice(`${entry.name} 下载已开始`);
    } catch (error) {
      setDownloadError(error instanceof Error ? error.message : '下载工作区文件失败');
    } finally {
      setDownloading('');
    }
  }

  return (
    <section className="workspace-browser" aria-label="Workspace 文件">
      <div className="workspace-browser-toolbar">
        <label className="field-label" htmlFor="workspace-browser-select">工作区</label>
        <select id="workspace-browser-select" value={binding?.workspace.id ?? ''} onChange={(event) => {
          setSelectedID(event.target.value);
          setNavigation({ workspaceID: event.target.value, segments: [] });
          setNotice('');
        }} disabled={bindings.length < 2}>
          {bindings.map((item) => <option key={item.workspace.id} value={item.workspace.id}>{item.workspace.name}</option>)}
        </select>
        <button className="button button-secondary" type="button" onClick={() => setNavigation({ workspaceID: binding?.workspace.id ?? '', segments: [] })} disabled={!segments.length || !mayListFiles}>
          工作区根目录
        </button>
        <button className="button button-secondary" type="button" onClick={() => setNavigation({ workspaceID: binding?.workspace.id ?? '', segments: segments.slice(0, -1) })} disabled={!segments.length || !mayListFiles}>
          上级目录
        </button>
        <button className="button button-secondary" type="button" onClick={() => setRefreshNumber((value) => value + 1)} disabled={!mayListFiles || loading}>
          刷新
        </button>
      </div>

      {hostError ? <p className="panel-error" role="alert">工作区状态暂不可用：{hostError}</p>
        : !currentHost ? <p className="panel-loading" role="status">正在读取主机工作区…</p>
          : !bindings.length ? <p className="panel-loading" role="status">此主机没有绑定工作区。</p>
            : binding && (
              <>
                <section className="workspace-sync-status" aria-label="工作区同步状态" role="status">
                  <p>服务端 revision {binding.workspace.revision} · Host 已应用 {binding.applied_revision} / 目标 {binding.desired_revision}</p>
                  <p>容量 {formatBytes(binding.workspace.used_bytes)} / {formatBytes(binding.workspace.quota_bytes)} · 同步 {binding.sync_state === 'healthy' ? '正常' : binding.sync_state === 'conflict' ? '冲突' : '未知'}</p>
                  {Array.isArray(binding.sync_conflicts) && binding.sync_conflicts.length > 0 && (
                    <ul aria-label="同步冲突文件">{binding.sync_conflicts.map((path) => <li key={path}>{path}</li>)}</ul>
                  )}
                </section>
                {!rootPath ? <p className="panel-error" role="alert">工作区目录信息无效，已阻止文件访问。</p>
                  : !quotaReady ? <p className="panel-loading" role="status">工作区用量超出或配额状态无效；文件暂不显示。</p>
                    : !bindingReady ? <p className="panel-loading" role="status">工作区尚未就绪、存在同步冲突或 Host revision 未追上；文件暂不显示。</p>
                    : !canRead ? <p className="panel-loading" role="status">请先接管电脑后查看已应用的工作区文件。</p>
                      : (
                        <>
                          <p className="workspace-browser-path" aria-label="当前工作区路径">
                            {binding.workspace.name}{segments.map((segment) => ` / ${segment}`)}
                          </p>
                          {(fileError || downloadError) && <p className="panel-error" role="alert">{fileError || downloadError}</p>}
                          {notice && <p className="notice" role="status">{notice}</p>}
                          <div className="file-list-wrap">
                            <table className="file-table">
                              <thead><tr><th scope="col">名称</th><th scope="col">大小</th><th scope="col">操作</th></tr></thead>
                              <tbody>
                                {loading && <tr><td colSpan={3}>正在读取文件…</td></tr>}
                                {!loading && files.map((entry) => {
                                  const path = entryPath(currentPath, entry);
                                  return (
                                    <tr key={path}>
                                      <td>{entry.is_directory
                                        ? <button className="file-entry" type="button" onClick={() => void openEntry(entry)}>{entry.name}/</button>
                                        : entry.name}</td>
                                      <td>{entry.is_directory || entry.size == null ? '—' : formatBytes(entry.size)}</td>
                                      <td>{!entry.is_directory && <button className="button button-secondary" type="button" onClick={() => void openEntry(entry)} disabled={downloading === path}>{downloading === path ? '下载中…' : '下载'}</button>}</td>
                                    </tr>
                                  );
                                })}
                                {!loading && !fileError && fileList && !files.length && <tr><td colSpan={3}>此目录为空。</td></tr>}
                              </tbody>
                            </table>
                          </div>
                        </>
                      )}
              </>
            )}
    </section>
  );
}
