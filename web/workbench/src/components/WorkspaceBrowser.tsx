import { useEffect, useRef, useState } from 'react';
import { gatewayApi, type GatewayWorkspaceBinding, type GatewayWorkspaceManifest } from '../api/gateway';

const REFRESH_INTERVAL_MS = 5000;
const MANIFEST_PAGE_SIZE = 500;

type Entry = { path: string; name: string; isDirectory: boolean; size?: number };

function validWorkspaceID(value: string): boolean {
  return /^w[a-z0-9]{7}$/.test(value);
}

function isWorkspaceBinding(value: unknown): value is GatewayWorkspaceBinding {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) return false;
  const binding = value as Record<string, unknown>;
  const workspace = binding.workspace;
  if (workspace === null || typeof workspace !== 'object' || Array.isArray(workspace)) return false;
  const valueWorkspace = workspace as Record<string, unknown>;
  return typeof valueWorkspace.id === 'string' && typeof valueWorkspace.name === 'string'
    && typeof valueWorkspace.revision === 'number' && typeof valueWorkspace.used_bytes === 'number'
    && typeof valueWorkspace.quota_bytes === 'number' && ['ready', 'deleting', 'deleted'].includes(String(valueWorkspace.state))
    && typeof binding.directory === 'string' && ['preparing', 'active', 'unbinding', 'detached', 'failed'].includes(String(binding.state))
    && typeof binding.binding_version === 'number' && typeof binding.desired_revision === 'number'
    && typeof binding.applied_revision === 'number' && ['healthy', 'conflict'].includes(String(binding.sync_state))
    && Array.isArray(binding.sync_conflicts) && binding.sync_conflicts.every((path) => typeof path === 'string');
}

function validRelativePath(value: string): boolean {
  return value.length > 0 && value.length <= 1024 && !value.startsWith('/') && !value.includes('\\')
    && value.split('/').every((segment) => segment.length > 0 && segment !== '.' && segment !== '..');
}

function withinQuota(binding: GatewayWorkspaceBinding): boolean {
  const { used_bytes: usedBytes, quota_bytes: quotaBytes } = binding.workspace;
  return Number.isSafeInteger(usedBytes) && usedBytes >= 0 && Number.isSafeInteger(quotaBytes) && quotaBytes > 0
    && usedBytes <= quotaBytes;
}

function readyBinding(binding: GatewayWorkspaceBinding): boolean {
  return validWorkspaceID(binding.workspace.id) && binding.directory === binding.workspace.id
    && binding.workspace.state === 'ready' && binding.state === 'active' && withinQuota(binding)
    && Number.isSafeInteger(binding.workspace.revision) && binding.workspace.revision >= 0
    && Number.isSafeInteger(binding.desired_revision) && Number.isSafeInteger(binding.applied_revision)
    && binding.sync_state === 'healthy' && Array.isArray(binding.sync_conflicts) && binding.sync_conflicts.length === 0
    && binding.workspace.revision === binding.desired_revision && binding.applied_revision === binding.desired_revision;
}

function formatBytes(value: number): string {
  if (!Number.isFinite(value) || value < 0) return '未知';
  if (value < 1024) return `${value} B`;
  if (value < 1024 ** 2) return `${(value / 1024).toFixed(1)} KB`;
  if (value < 1024 ** 3) return `${(value / 1024 ** 2).toFixed(1)} MB`;
  return `${(value / 1024 ** 3).toFixed(1)} GB`;
}

function isInPrefix(path: string, prefix: string): boolean {
  return !prefix || path === prefix || path.startsWith(`${prefix}/`);
}

function addEntry(entries: Map<string, Entry>, entry: Entry) {
  const current = entries.get(entry.path);
  if (current && (current.isDirectory !== entry.isDirectory || current.size !== entry.size)) {
    throw new Error('Workspace manifest 包含冲突路径，已拒绝显示');
  }
  entries.set(entry.path, entry);
}

function entriesFromManifest(page: GatewayWorkspaceManifest, prefix: string): Entry[] {
  const entries = new Map<string, Entry>();
  for (const file of page.files) {
    if (!validRelativePath(file.path) || !isInPrefix(file.path, prefix) || !Number.isSafeInteger(file.size_bytes) || file.size_bytes < 0) {
      throw new Error('Workspace manifest 包含无效或越界路径，已拒绝显示');
    }
    if (file.path === prefix) throw new Error('当前路径不是目录，已拒绝显示');
    const remainder = prefix ? file.path.slice(prefix.length + 1) : file.path;
    const slash = remainder.indexOf('/');
    const name = slash < 0 ? remainder : remainder.slice(0, slash);
    const childPath = prefix ? `${prefix}/${name}` : name;
    addEntry(entries, slash < 0
      ? { path: childPath, name, isDirectory: false, size: file.size_bytes }
      : { path: childPath, name, isDirectory: true });
  }
  for (const directory of page.empty_directories) {
    if (!validRelativePath(directory) || !isInPrefix(directory, prefix) || directory === prefix) {
      throw new Error('Workspace manifest 包含无效或越界目录，已拒绝显示');
    }
    const remainder = prefix ? directory.slice(prefix.length + 1) : directory;
    const slash = remainder.indexOf('/');
    const name = slash < 0 ? remainder : remainder.slice(0, slash);
    const childPath = prefix ? `${prefix}/${name}` : name;
    addEntry(entries, { path: childPath, name, isDirectory: true });
  }
  return [...entries.values()].sort((a, b) => Number(b.isDirectory) - Number(a.isDirectory) || a.name.localeCompare(b.name));
}

function validateManifest(page: GatewayWorkspaceManifest, workspaceID: string, revision: number) {
  if (page.workspace_id !== workspaceID || page.revision !== revision || !Array.isArray(page.files)
    || !Array.isArray(page.empty_directories) || (page.next_cursor !== null
      && (typeof page.next_cursor !== 'string' || page.next_cursor.length === 0 || page.next_cursor.length > 4096))) {
    throw new Error('Workspace manifest 响应无效，已拒绝显示');
  }
}

function mergeEntries(existing: Entry[], incoming: Entry[]): Entry[] {
  const entries = new Map(existing.map((entry) => [entry.path, entry]));
  for (const entry of incoming) addEntry(entries, entry);
  return [...entries.values()].sort((a, b) => Number(b.isDirectory) - Number(a.isDirectory) || a.name.localeCompare(b.name));
}

export default function WorkspaceBrowser({ active }: { active: boolean }) {
  const [bindingState, setBindingState] = useState<{ items: GatewayWorkspaceBinding[]; error?: string } | null>(null);
  const [selectedID, setSelectedID] = useState('');
  const [navigation, setNavigation] = useState<{ workspaceID: string; segments: string[] }>({ workspaceID: '', segments: [] });
  const [fileState, setFileState] = useState<{
    key: string;
    entries?: Entry[];
    error?: string;
    nextCursor: string | null;
    seenCursors: string[];
    loadingNext?: boolean;
  } | null>(null);
  const [refreshNumber, setRefreshNumber] = useState(0);
  const [downloading, setDownloading] = useState('');
  const [downloadError, setDownloadError] = useState('');
  const [notice, setNotice] = useState('');
  const listRequestID = useRef(0);

  useEffect(() => {
    if (!active) return;
    let current = true;
    const refresh = async () => {
      try {
        const response = await gatewayApi.workspaces();
        if (!Array.isArray(response.items) || !response.items.every(isWorkspaceBinding)) throw new Error('Workspace binding 响应无效');
        if (current) setBindingState({ items: response.items });
      } catch (error) {
        if (!current) return;
        setBindingState({ items: [], error: error instanceof Error ? error.message : '无法读取 Workspace 状态' });
        setFileState(null);
      }
    };
    void refresh();
    const timer = window.setInterval(() => void refresh(), REFRESH_INTERVAL_MS);
    return () => {
      current = false;
      window.clearInterval(timer);
    };
  }, [active, refreshNumber]);

  const bindings = bindingState?.items ?? [];
  const binding = bindings.find((item) => item.workspace.id === selectedID) ?? bindings[0];
  const workspaceID = binding?.workspace.id;
  const appliedRevision = binding?.applied_revision;
  const segments = binding && navigation.workspaceID === binding.workspace.id ? navigation.segments : [];
  const currentPath = segments.join('/');
  const bindingReady = binding ? readyBinding(binding) : false;
  const fileKey = binding ? `${binding.workspace.id}:${binding.applied_revision}:${currentPath}` : '';
  const mayListFiles = Boolean(active && !bindingState?.error && binding && bindingReady);
  const fileList = fileState?.key === fileKey ? fileState : null;
  const loading = mayListFiles && !fileList;
  const fileError = fileList?.error ?? '';
  const files = fileList?.entries ?? [];

  useEffect(() => {
    const requestID = ++listRequestID.current;
    if (!mayListFiles || !workspaceID || appliedRevision === undefined) return;
    void gatewayApi.workspaceManifest(workspaceID, appliedRevision, currentPath || undefined, undefined, MANIFEST_PAGE_SIZE)
      .then((page) => {
        validateManifest(page, workspaceID, appliedRevision);
        const entries = entriesFromManifest(page, currentPath);
        if (requestID === listRequestID.current) setFileState({ key: fileKey, entries, nextCursor: page.next_cursor, seenCursors: [] });
      })
      .catch((error: unknown) => {
        if (requestID === listRequestID.current) setFileState({ key: fileKey, error: error instanceof Error ? error.message : '读取 Workspace manifest 失败', nextCursor: null, seenCursors: [] });
      });
  }, [appliedRevision, currentPath, fileKey, mayListFiles, refreshNumber, workspaceID]);

  async function loadMore() {
    if (!binding || !fileList?.nextCursor || fileList.loadingNext || !mayListFiles) return;
    const requestID = listRequestID.current;
    const cursor = fileList.nextCursor;
    if (fileList.seenCursors.includes(cursor)) {
      setFileState({ ...fileList, error: 'Workspace manifest 分页游标无效', nextCursor: null });
      return;
    }
    setFileState({ ...fileList, loadingNext: true, error: '' });
    try {
      const page = await gatewayApi.workspaceManifest(binding.workspace.id, binding.applied_revision, currentPath || undefined, cursor, MANIFEST_PAGE_SIZE);
      validateManifest(page, binding.workspace.id, binding.applied_revision);
      const nextCursor = page.next_cursor;
      if (nextCursor === cursor || (nextCursor !== null && fileList.seenCursors.includes(nextCursor))) {
        throw new Error('Workspace manifest 分页游标无效');
      }
      const entries = entriesFromManifest(page, currentPath);
      if (requestID === listRequestID.current) {
        setFileState({
          ...fileList,
          entries: mergeEntries(fileList.entries ?? [], entries),
          nextCursor,
          seenCursors: [...fileList.seenCursors, cursor],
          loadingNext: false,
          error: undefined,
        });
      }
    } catch (error) {
      if (requestID === listRequestID.current) {
        setFileState({ ...fileList, entries: [], nextCursor: null, loadingNext: false, error: error instanceof Error ? error.message : '读取 Workspace manifest 失败' });
      }
    }
  }

  async function openEntry(entry: Entry) {
    if (!validRelativePath(entry.path) || !mayListFiles || !binding) return;
    setNotice('');
    setDownloadError('');
    if (entry.isDirectory) {
      setNavigation((current) => ({
        workspaceID: binding.workspace.id,
        segments: [...(current.workspaceID === binding.workspace.id ? current.segments : []), entry.name],
      }));
      return;
    }
    setDownloading(entry.path);
    try {
      await gatewayApi.downloadWorkspaceFile(binding.workspace.id, binding.applied_revision, entry.path);
      setNotice(`${entry.name} 下载已开始`);
    } catch (error) {
      setDownloadError(error instanceof Error ? error.message : '下载 Workspace 文件失败');
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
        <button className="button button-secondary" type="button" onClick={() => setRefreshNumber((value) => value + 1)} disabled={!active || loading}>
          刷新
        </button>
      </div>

      {bindingState?.error ? <p className="panel-error" role="alert">工作区状态暂不可用：{bindingState.error}</p>
        : !bindingState ? <p className="panel-loading" role="status">正在读取 Workspace 状态…</p>
          : !bindings.length ? <p className="panel-loading" role="status">此电脑没有可访问的工作区。</p>
            : binding && (
              <>
                <section className="workspace-sync-status" aria-label="工作区同步状态" role="status">
                  <p>服务端 revision {binding.workspace.revision} · Host 已应用 {binding.applied_revision} / 目标 {binding.desired_revision}</p>
                  <p>容量 {formatBytes(binding.workspace.used_bytes)} / {formatBytes(binding.workspace.quota_bytes)} · 同步 {binding.sync_state === 'healthy' ? '正常' : '冲突'}</p>
                  {binding.sync_conflicts.length > 0 && (
                    <ul aria-label="同步冲突文件">{binding.sync_conflicts.map((path) => <li key={path}>{path}</li>)}</ul>
                  )}
                </section>
                {!validWorkspaceID(binding.workspace.id) || binding.directory !== binding.workspace.id
                  ? <p className="panel-error" role="alert">工作区绑定目录无效，已阻止文件访问。</p>
                  : !withinQuota(binding) ? <p className="panel-loading" role="status">工作区用量超出或配额状态无效；文件暂不显示。</p>
                    : !bindingReady ? <p className="panel-loading" role="status">工作区尚未就绪、存在同步冲突或 Host revision 未追上；文件暂不显示。</p>
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
                                {loading && <tr><td colSpan={3}>正在读取已应用 revision…</td></tr>}
                                {!loading && files.map((entry) => (
                                  <tr key={entry.path}>
                                    <td>{entry.isDirectory
                                      ? <button className="file-entry" type="button" onClick={() => void openEntry(entry)}>{entry.name}/</button>
                                      : entry.name}</td>
                                    <td>{entry.isDirectory || entry.size === undefined ? '—' : formatBytes(entry.size)}</td>
                                    <td>{!entry.isDirectory && <button className="button button-secondary" type="button" onClick={() => void openEntry(entry)} disabled={downloading === entry.path}>{downloading === entry.path ? '下载中…' : '下载'}</button>}</td>
                                  </tr>
                                ))}
                                {!loading && !fileError && fileList && !files.length && !fileList.nextCursor && <tr><td colSpan={3}>此目录为空。</td></tr>}
                                {!loading && fileList?.nextCursor && <tr><td colSpan={3}>
                                  <button className="button button-secondary" type="button" onClick={() => void loadMore()} disabled={fileList.loadingNext}>
                                    {fileList.loadingNext ? '加载中…' : '加载更多文件'}
                                  </button>
                                </td></tr>}
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
