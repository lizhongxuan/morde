import { useEffect, useRef, useState } from 'react';
import { gatewayApi, type GatewayWorkspaceBinding, type GatewayWorkspaceDiff, type GatewayWorkspaceManifest } from '../api/gateway';
import FloatingError from './FloatingError';

const REFRESH_INTERVAL_MS = 5000;
const MANIFEST_PAGE_SIZE = 500;
const PREVIEW_MANIFEST_ENTRY_LIMIT = 100_000;
const DIFF_CHANGE_LIMIT = 500;
const DIFF_TEXT_LIMIT_BYTES = 256 * 1024;

type Entry = { path: string; name: string; isDirectory: boolean; isSymlink?: boolean; size?: number };
type ManifestMetadata =
  | { path: string; kind: 'file'; sizeBytes: number; sha256: string; mode: number }
  | { path: string; kind: 'symlink'; sizeBytes: number; sha256: string; target: string }
  | { path: string; kind: 'directory' };
type MetadataChange = {
  path: string;
  change: 'added' | 'removed' | 'modified';
  current?: ManifestMetadata;
  target?: ManifestMetadata;
};
type MockRevisionPreview = { currentRevision: number; targetRevision: number; changes: MetadataChange[] };
type MockPreviewState = {
  key: string;
  loading?: boolean;
  error?: string;
  result?: MockRevisionPreview;
  confirmingRestore?: boolean;
  restoreResult?: 'success' | 'conflict';
};
type WorkspaceDiffState = { key: string; loading?: boolean; error?: string; result?: GatewayWorkspaceDiff };

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

function validRelativePath(value: unknown): value is string {
  return typeof value === 'string' && value.length > 0 && value.length <= 1024 && !value.startsWith('/') && !value.includes('\\')
    && value.split('/').every((segment) => segment.length > 0 && segment !== '.' && segment !== '..');
}

function validDiffVersion(value: unknown): boolean {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) return false;
  const version = value as Record<string, unknown>;
  if (!validRelativePath(version.path) || !['file', 'symlink', 'directory'].includes(String(version.kind))) return false;
  if (version.size_bytes !== undefined && (!Number.isSafeInteger(version.size_bytes) || Number(version.size_bytes) < 0)) return false;
  if (version.sha256 !== undefined && (typeof version.sha256 !== 'string' || !/^[a-f0-9]{64}$/.test(version.sha256))) return false;
  if (version.mode !== undefined && (!Number.isSafeInteger(version.mode) || Number(version.mode) < 0 || Number(version.mode) > 511)) return false;
  if (version.kind === 'symlink' && (typeof version.symlink_target !== 'string'
    || !validSymlinkTarget(version.path, version.symlink_target))) return false;
  return true;
}

function validateWorkspaceDiff(diff: GatewayWorkspaceDiff, workspaceID: string, fromRevision: number, toRevision: number) {
  if (diff === null || typeof diff !== 'object' || Array.isArray(diff) || diff.workspace_id !== workspaceID
    || diff.from_revision !== fromRevision || diff.to_revision !== toRevision || !Array.isArray(diff.changes)
    || diff.changes.length > DIFF_CHANGE_LIMIT) throw new Error('历史差异响应无效或超出显示上限，已停止显示');
  let textBytes = 0;
  for (const item of diff.changes) {
    if (item === null || typeof item !== 'object' || !validRelativePath(item.path)
      || !['added', 'removed', 'changed', 'renamed'].includes(item.action)
      || !['text', 'binary', 'too_large', 'metadata_only', 'not_applicable'].includes(item.display_status)
      || (item.before !== null && !validDiffVersion(item.before)) || (item.after !== null && !validDiffVersion(item.after))
      || (item.before_text !== null && typeof item.before_text !== 'string')
      || (item.after_text !== null && typeof item.after_text !== 'string')) {
      throw new Error('历史差异包含无效路径或字段，已停止显示');
    }
    if (item.display_status === 'text') {
      if (item.before_text === null && item.after_text === null) throw new Error('历史差异文本缺失，已停止显示');
      textBytes += new TextEncoder().encode(item.before_text ?? '').length + new TextEncoder().encode(item.after_text ?? '').length;
    } else if (item.before_text !== null || item.after_text !== null) {
      throw new Error('非文本差异包含正文，已停止显示');
    }
    if (textBytes > DIFF_TEXT_LIMIT_BYTES) throw new Error('历史差异文本超出显示上限，已停止显示');
  }
}

function diffVersionSummary(version: GatewayWorkspaceDiff['changes'][number]['before']): string {
  if (!version) return '不存在';
  const kind = version.kind === 'file' ? '文件' : version.kind === 'symlink' ? '符号链接' : '目录';
  const details = [version.size_bytes === undefined ? undefined : formatBytes(version.size_bytes),
    version.sha256 ? `SHA-256 ${version.sha256}` : undefined,
    version.mode === undefined ? undefined : `权限 ${version.mode.toString(8).padStart(3, '0')}`,
    version.symlink_target ? `→ ${version.symlink_target}` : undefined].filter(Boolean);
  return [kind, ...details].join(' · ');
}

function diffActionLabel(action: GatewayWorkspaceDiff['changes'][number]['action']): string {
  return action === 'added' ? '新增' : action === 'removed' ? '移除' : action === 'renamed' ? '重命名' : '修改';
}

function diffDisplayStatusLabel(status: GatewayWorkspaceDiff['changes'][number]['display_status']): string {
  return status === 'text' ? '可显示文本' : status === 'binary' ? '二进制，仅显示元数据'
    : status === 'too_large' ? '文件较大，仅显示元数据' : status === 'metadata_only' ? '仅显示元数据' : '不适用';
}

function workspaceDiffError(error: unknown): string {
  const apiError = error !== null && typeof error === 'object' ? error as { code?: unknown; status?: unknown; message?: unknown } : {};
  if (apiError.status === 403 || apiError.code === 'forbidden') return '当前账号无权读取此 Workspace 的历史差异。';
  if (apiError.status === 409 || apiError.code === 'conflict' || apiError.code === 'workspace_conflict') return 'Workspace 状态或 revision 已变化；请刷新后重新选择版本。';
  if (apiError.status === 404) return 'Workspace 或指定 revision 不可用。';
  if (apiError.status === 413) return '差异内容超出服务端展示限制。';
  return error instanceof Error ? error.message : '读取历史差异失败。';
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
  if (current && (current.isDirectory !== entry.isDirectory || current.isSymlink !== entry.isSymlink || current.size !== entry.size)) {
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
    if ((file.kind !== undefined && file.kind !== 'file' && file.kind !== 'symlink')
      || (file.kind !== 'symlink' && file.symlink_target !== undefined)) {
      throw new Error('Workspace manifest 文件类型无效，已拒绝显示');
    }
    if (file.kind === 'symlink' && (typeof file.symlink_target !== 'string'
      || !validSymlinkTarget(file.path, file.symlink_target) || file.mode !== 0
      || !Array.isArray(file.chunks) || file.chunks.length !== 0 || new TextEncoder().encode(file.symlink_target).length !== file.size_bytes
      || !/^[a-f0-9]{64}$/.test(file.sha256))) {
      throw new Error('Workspace manifest 符号链接元数据无效，已拒绝显示');
    }
    if (file.path === prefix) throw new Error('当前路径不是目录，已拒绝显示');
    const remainder = prefix ? file.path.slice(prefix.length + 1) : file.path;
    const slash = remainder.indexOf('/');
    const name = slash < 0 ? remainder : remainder.slice(0, slash);
    const childPath = prefix ? `${prefix}/${name}` : name;
    addEntry(entries, slash < 0
      ? { path: childPath, name, isDirectory: false, isSymlink: file.kind === 'symlink', size: file.size_bytes }
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
  if (page === null || typeof page !== 'object' || Array.isArray(page)
    || page.workspace_id !== workspaceID || page.revision !== revision || !Array.isArray(page.files)
    || !Array.isArray(page.empty_directories) || (page.next_cursor !== null
      && (typeof page.next_cursor !== 'string' || page.next_cursor.length === 0 || page.next_cursor.length > 512))) {
    throw new Error('Workspace manifest 响应无效，已拒绝显示');
  }
}

function metadataEntriesFromManifest(page: GatewayWorkspaceManifest): ManifestMetadata[] {
  const entries: ManifestMetadata[] = [];
  for (const file of page.files) {
    if (!validRelativePath(file.path) || !Number.isSafeInteger(file.size_bytes) || file.size_bytes < 0
      || typeof file.sha256 !== 'string' || !/^[a-f0-9]{64}$/.test(file.sha256)
      || !Number.isSafeInteger(file.mode) || file.mode < 0 || file.mode > 511
      || !Array.isArray(file.chunks)) {
      throw new Error('manifest 文件元数据无效，已停止本机预演');
    }
    if (file.kind === 'symlink') {
      const target = file.symlink_target;
      if (typeof target !== 'string' || !validSymlinkTarget(file.path, target)
        || file.mode !== 0 || file.chunks.length !== 0 || new TextEncoder().encode(target).length !== file.size_bytes) {
        throw new Error('manifest 符号链接元数据无效，已停止本机预演');
      }
      entries.push({ path: file.path, kind: 'symlink', sizeBytes: file.size_bytes, sha256: file.sha256, target });
      continue;
    }
    if ((file.kind !== undefined && file.kind !== 'file') || file.symlink_target !== undefined) {
      throw new Error('manifest 文件类型无效，已停止本机预演');
    }
    let chunkBytes = 0;
    for (const chunk of file.chunks) {
      if (chunk === null || typeof chunk !== 'object' || typeof chunk.sha256 !== 'string'
        || !/^[a-f0-9]{64}$/.test(chunk.sha256) || !Number.isSafeInteger(chunk.size_bytes)
        || chunk.size_bytes < 1 || chunk.size_bytes > 4 * 1024 * 1024 || chunkBytes > file.size_bytes - chunk.size_bytes) {
        throw new Error('manifest 分块元数据无效，已停止本机预演');
      }
      chunkBytes += chunk.size_bytes;
    }
    if (chunkBytes !== file.size_bytes || (file.size_bytes === 0) !== (file.chunks.length === 0)) {
      throw new Error('manifest 文件大小与分块元数据不一致，已停止本机预演');
    }
    entries.push({ path: file.path, kind: 'file', sizeBytes: file.size_bytes, sha256: file.sha256, mode: file.mode });
  }
  for (const path of page.empty_directories) {
    if (!validRelativePath(path)) throw new Error('manifest 目录元数据无效，已停止本机预演');
    entries.push({ path, kind: 'directory' });
  }
  return entries;
}

function validSymlinkTarget(linkPath: string, target: string): boolean {
  if (!target || new TextEncoder().encode(target).length > 4096 || target.includes('\0') || target.includes('\\') || target.includes(':') || target.startsWith('/')) return false;
  let depth = linkPath.split('/').length - 1;
  for (const part of target.split('/')) {
    if (!part || part === '.') continue;
    if (part === '..') {
      if (--depth < 0) return false;
    } else depth++;
  }
  return true;
}

async function readWorkspaceManifestMetadata(workspaceID: string, revision: number): Promise<Map<string, ManifestMetadata>> {
  const entries = new Map<string, ManifestMetadata>();
  const foldedPaths = new Set<string>();
  const seenCursors = new Set<string>();
  let cursor: string | undefined;

  while (true) {
    const page = await gatewayApi.workspaceManifest(workspaceID, revision, undefined, cursor, MANIFEST_PAGE_SIZE);
    validateManifest(page, workspaceID, revision);
    if (page.files.length + page.empty_directories.length > MANIFEST_PAGE_SIZE) {
      throw new Error('manifest 分页超过条目上限，已停止本机预演');
    }
    const pageEntries = metadataEntriesFromManifest(page);
    if (!pageEntries.length && page.next_cursor !== null) throw new Error('manifest 分页没有前进，已停止本机预演');
    if (entries.size + pageEntries.length > PREVIEW_MANIFEST_ENTRY_LIMIT) throw new Error('manifest 条目超过上限，已停止本机预演');
    for (const entry of pageEntries) {
      const foldedPath = entry.path.toLowerCase();
      if (entries.has(entry.path) || foldedPaths.has(foldedPath)) throw new Error('manifest 存在重复路径，已停止本机预演');
      entries.set(entry.path, entry);
      foldedPaths.add(foldedPath);
    }

    if (page.next_cursor === null) return entries;
    if (seenCursors.has(page.next_cursor)) throw new Error('manifest 分页游标重复，已停止本机预演');
    seenCursors.add(page.next_cursor);
    cursor = page.next_cursor;
  }
}

function simulatePreviousManifest(current: Map<string, ManifestMetadata>): Map<string, ManifestMetadata> {
  const target = new Map(current);
  const files = [...target.values()].filter((entry): entry is Extract<ManifestMetadata, { kind: 'file' }> => entry.kind === 'file');
  const firstFile = files[0];
  if (firstFile) {
    target.set(firstFile.path, {
      ...firstFile,
      sizeBytes: firstFile.sizeBytes === Number.MAX_SAFE_INTEGER ? firstFile.sizeBytes - 1 : firstFile.sizeBytes + 1,
      sha256: `${firstFile.sha256[0] === 'b' ? 'c' : 'b'}${firstFile.sha256.slice(1)}`,
      mode: firstFile.mode === 384 ? 420 : 384,
    });
  }
  if (files.length > 1) {
    const lastFile = files.at(-1);
    if (lastFile) target.delete(lastFile.path);
  }
  let addedPath = '__morde_mock_preview_added.txt';
  while (target.has(addedPath)) addedPath = `_${addedPath}`;
  target.set(addedPath, { path: addedPath, kind: 'file', sizeBytes: 17, sha256: 'c'.repeat(64), mode: 420 });
  return target;
}

function metadataEqual(left: ManifestMetadata, right: ManifestMetadata): boolean {
  if (left.kind !== right.kind) return false;
  if (left.kind === 'directory') return true;
  if (left.kind === 'file') return right.kind === 'file' && left.sizeBytes === right.sizeBytes
    && left.sha256 === right.sha256 && left.mode === right.mode;
  return right.kind === 'symlink' && left.sizeBytes === right.sizeBytes
    && left.sha256 === right.sha256 && left.target === right.target;
}

function createMockRevisionPreview(currentRevision: number, current: Map<string, ManifestMetadata>): MockRevisionPreview {
  const target = simulatePreviousManifest(current);
  const paths = new Set([...current.keys(), ...target.keys()]);
  const changes: MetadataChange[] = [];
  for (const path of paths) {
    const currentEntry = current.get(path);
    const targetEntry = target.get(path);
    if (!currentEntry && targetEntry) changes.push({ path, change: 'added', target: targetEntry });
    else if (currentEntry && !targetEntry) changes.push({ path, change: 'removed', current: currentEntry });
    else if (currentEntry && targetEntry && !metadataEqual(currentEntry, targetEntry)) {
      changes.push({ path, change: 'modified', current: currentEntry, target: targetEntry });
    }
  }
  changes.sort((left, right) => left.path.localeCompare(right.path));
  return { currentRevision, targetRevision: Math.max(0, currentRevision - 1), changes };
}

function manifestMetadataSummary(entry: ManifestMetadata): string {
  if (entry.kind === 'directory') return '空目录';
  if (entry.kind === 'symlink') return `符号链接 → ${entry.target} · ${formatBytes(entry.sizeBytes)}`;
  return `文件 · ${formatBytes(entry.sizeBytes)} · SHA-256 ${entry.sha256} · 权限 ${entry.mode.toString(8).padStart(3, '0')}`;
}

function mockPreviewError(error: unknown): string {
  const apiError = error !== null && typeof error === 'object' ? error as { code?: unknown; status?: unknown; message?: unknown } : {};
  if (apiError.code === 'workspace_conflict' || apiError.code === 'conflict' || apiError.status === 409) {
    return `版本冲突：${typeof apiError.message === 'string' ? apiError.message : '已应用 revision 发生变化'}；预演已停止。`;
  }
  if (apiError.code === 'forbidden' || apiError.status === 403) {
    return `无权读取 revision manifest 元数据：${typeof apiError.message === 'string' ? apiError.message : '服务端拒绝访问'}。`;
  }
  return error instanceof Error ? error.message : '读取 revision manifest 元数据失败，预演已停止。';
}

function mergeEntries(existing: Entry[], incoming: Entry[]): Entry[] {
  const entries = new Map(existing.map((entry) => [entry.path, entry]));
  for (const entry of incoming) addEntry(entries, entry);
  return [...entries.values()].sort((a, b) => Number(b.isDirectory) - Number(a.isDirectory) || a.name.localeCompare(b.name));
}

export default function WorkspaceBrowser({ active }: { active: boolean }) {
  const [bindingState, setBindingState] = useState<{ items: GatewayWorkspaceBinding[]; error?: string } | null>(null);
  const [selectedID, setSelectedID] = useState('');
  const [diffFromInput, setDiffFromInput] = useState<string | null>(null);
  const [diffToInput, setDiffToInput] = useState<string | null>(null);
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
  const [diffState, setDiffState] = useState<WorkspaceDiffState | null>(null);
  const [mockPreviewState, setMockPreviewState] = useState<MockPreviewState | null>(null);
  const listRequestID = useRef(0);
  const diffRequestID = useRef(0);
  const mockPreviewRequestID = useRef(0);

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
  const mockPreviewEnabled = import.meta.env.DEV && import.meta.env.VITE_MOCK_API === '1';
  const mockPreviewFence = binding ? JSON.stringify([binding.workspace.id, binding.binding_version, binding.state, binding.workspace.state,
    binding.workspace.revision, binding.desired_revision, binding.applied_revision, binding.sync_state,
    binding.sync_conflicts, binding.workspace.used_bytes, binding.workspace.quota_bytes]) : '';
  const mockPreviewFenceRef = useRef('');
  useEffect(() => { mockPreviewFenceRef.current = mockPreviewFence; }, [mockPreviewFence]);
  const mockPreview = mockPreviewState?.key === mockPreviewFence ? mockPreviewState : null;
  const fileKey = binding ? `${binding.workspace.id}:${binding.applied_revision}:${currentPath}` : '';
  const fromRevisionText = diffFromInput ?? (appliedRevision === undefined ? '' : String(Math.max(0, appliedRevision - 1)));
  const toRevisionText = diffToInput ?? (appliedRevision === undefined ? '' : String(appliedRevision));
  const fromRevision = Number(fromRevisionText);
  const toRevision = Number(toRevisionText);
  const diffSelectionValid = Boolean(binding && Number.isSafeInteger(fromRevision) && fromRevision >= 0
    && fromRevision <= binding.applied_revision && Number.isSafeInteger(toRevision) && toRevision >= 0
    && toRevision <= binding.applied_revision && fromRevision !== toRevision);
  const diffKey = binding ? JSON.stringify([binding.workspace.id, binding.binding_version, binding.state,
    binding.workspace.state, binding.workspace.revision, binding.desired_revision, binding.applied_revision,
    binding.sync_state, binding.sync_conflicts, fromRevisionText, toRevisionText]) : '';
  const workspaceDiff = diffState?.key === diffKey ? diffState : null;
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

  async function runMockRevisionPreview() {
    if (!mockPreviewEnabled || !binding || !bindingReady || !active) return;
    const requestID = ++mockPreviewRequestID.current;
    const fence = mockPreviewFence;
    const previewBinding = binding;
    setMockPreviewState({ key: fence, loading: true });
    try {
      const current = await readWorkspaceManifestMetadata(previewBinding.workspace.id, previewBinding.applied_revision);
      if (requestID !== mockPreviewRequestID.current) return;
      if (mockPreviewFenceRef.current !== fence) {
        throw Object.assign(new Error('Host 应用的 Workspace revision 已变化'), { code: 'workspace_conflict', status: 409 });
      }
      setMockPreviewState({
        key: fence,
        result: createMockRevisionPreview(previewBinding.applied_revision, current),
      });
    } catch (error) {
      if (requestID === mockPreviewRequestID.current) setMockPreviewState({ key: fence, error: mockPreviewError(error) });
    }
  }

  async function runWorkspaceDiff() {
    if (!active || !binding || !bindingReady || !diffSelectionValid) return;
    const requestID = ++diffRequestID.current;
    const key = diffKey;
    setDiffState({ key, loading: true });
    try {
      const result = await gatewayApi.workspaceDiff(binding.workspace.id, fromRevision, toRevision);
      validateWorkspaceDiff(result, binding.workspace.id, fromRevision, toRevision);
      if (requestID === diffRequestID.current) setDiffState({ key, result });
    } catch (error) {
      if (requestID === diffRequestID.current) setDiffState({ key, error: workspaceDiffError(error) });
    }
  }

  function closeMockRevisionPreview() {
    mockPreviewRequestID.current++;
    setMockPreviewState(null);
  }

  function confirmMockRestore(result: 'success' | 'conflict') {
    if (!mockPreviewEnabled || !active || !bindingReady || !mockPreview?.result) return;
    if (mockPreviewFence !== mockPreview.key || mockPreviewFenceRef.current !== mockPreview.key) {
      setMockPreviewState({ key: mockPreviewFence, error: '版本冲突：Workspace revision 已变化；本机预演已停止。' });
      return;
    }
    setMockPreviewState({ key: mockPreview.key, result: mockPreview.result, restoreResult: result });
  }

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
    if (entry.isSymlink) {
      setDownloadError('符号链接只显示链接本身，不会跟随链接下载内容。');
      return;
    }
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
          setDiffFromInput(null);
          setDiffToInput(null);
          setDiffState(null);
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

      {bindingState?.error ? <FloatingError>工作区状态暂不可用：{bindingState.error}</FloatingError>
        : !bindingState ? <p className="panel-loading" role="status">正在读取 Workspace 状态…</p>
          : !bindings.length ? <p className="panel-loading" role="status">此电脑没有可访问的工作区。</p>
            : binding && (
              <>
                <section className="workspace-sync-status" aria-label="工作区同步状态" role="status">
                  <p>服务端 revision {binding.workspace.revision} · Host 已应用 {binding.applied_revision} / 目标 {binding.desired_revision}</p>
                  <p>容量 {formatBytes(binding.workspace.used_bytes)} / {formatBytes(binding.workspace.quota_bytes)} · 同步 {binding.sync_state === 'healthy' ? '正常' : '冲突'}</p>
                  {mockPreviewEnabled
                    ? <p>本机 VITE_MOCK_API 测试替身：差异与回滚预演使用 manifest 元数据合成，不代表真实历史版本，也不会读取文件正文或写回服务端。</p>
                    : <p>当前文件浏览只读；历史差异可查看，Workspace 恢复请在 Portal 中确认。</p>}
                  {binding.sync_conflicts.length > 0 && (
                    <ul aria-label="同步冲突文件">{binding.sync_conflicts.map((path) => <li key={path}>{path}</li>)}</ul>
                  )}
                </section>
                <section className="workspace-sync-status workspace-history-diff" aria-label="Workspace 历史差异">
                  <h3>历史版本差异（只读）</h3>
                  <p>选择两个已确认 revision。首期仅 Workspace owner 可读；当前 Host/Workspace 未同步就绪时会拒绝读取。</p>
                  <div className="workspace-browser-toolbar">
                    <label className="field-label" htmlFor="workspace-diff-from">起始 revision</label>
                    <input id="workspace-diff-from" aria-label="起始 revision" type="number" min={0} max={binding.applied_revision}
                      step={1} value={fromRevisionText} onChange={(event) => setDiffFromInput(event.target.value)} />
                    <label className="field-label" htmlFor="workspace-diff-to">目标 revision</label>
                    <input id="workspace-diff-to" aria-label="目标 revision" type="number" min={0} max={binding.applied_revision}
                      step={1} value={toRevisionText} onChange={(event) => setDiffToInput(event.target.value)} />
                    <button className="button button-secondary" type="button" onClick={() => void runWorkspaceDiff()}
                      disabled={!active || !bindingReady || !diffSelectionValid || Boolean(workspaceDiff?.loading)}>
                      {workspaceDiff?.loading ? '正在读取差异…' : '查看历史差异'}
                    </button>
                  </div>
                  {!bindingReady && <p className="panel-loading" role="status">Host binding、同步或 revision 未就绪；历史差异暂不可读。</p>}
                  {bindingReady && !diffSelectionValid && <p className="panel-loading" role="status">请输入两个不同且不大于 Host 已应用 revision {binding.applied_revision} 的非负整数。</p>}
                  {workspaceDiff?.error && <FloatingError>{workspaceDiff.error}</FloatingError>}
                  {workspaceDiff?.loading && <p role="status">正在读取 revision {fromRevision} → {toRevision}…</p>}
                  {workspaceDiff?.result && <>
                    <p role="status">revision {workspaceDiff.result.from_revision} → {workspaceDiff.result.to_revision}，共 {workspaceDiff.result.changes.length} 条路径差异。</p>
                    {!workspaceDiff.result.changes.length ? <p>这两个 revision 没有差异。</p> : (
                      <div className="file-list-wrap"><table className="file-table workspace-diff-table">
                        <caption className="sr-only">Workspace 历史版本只读差异</caption>
                        <thead><tr><th scope="col">变化</th><th scope="col">路径</th><th scope="col">起始版本</th><th scope="col">目标版本</th><th scope="col">内容</th></tr></thead>
                        <tbody>{workspaceDiff.result.changes.map((change, index) => <tr key={`${change.path}-${change.action}-${index}`}>
                          <td>{diffActionLabel(change.action)}</td>
                          <td className="mono">{change.path}</td>
                          <td>{diffVersionSummary(change.before)}</td>
                          <td>{diffVersionSummary(change.after)}</td>
                          <td>{change.display_status === 'text'
                            ? <details><summary>查看小文件文本</summary>
                              <div className="workspace-diff-text-grid">
                                <div><strong>起始版本</strong><pre>{change.before_text ?? '（文件不存在）'}</pre></div>
                                <div><strong>目标版本</strong><pre>{change.after_text ?? '（文件不存在）'}</pre></div>
                              </div>
                            </details>
                            : diffDisplayStatusLabel(change.display_status)}</td>
                        </tr>)}</tbody>
                      </table></div>
                    )}
                  </>}
                </section>
                {mockPreviewEnabled && active && bindingReady && <section className="workspace-sync-status" aria-label="本机 Mock revision 预演">
                  <button className="button button-secondary" type="button" onClick={() => void runMockRevisionPreview()} disabled={mockPreview?.loading}>
                    {mockPreview?.loading ? '正在读取 manifest 元数据…' : '生成本机 Mock 差异与回滚预演'}
                  </button>
                  {mockPreview?.error && <FloatingError>{mockPreview.error}</FloatingError>}
                </section>}
                {mockPreviewEnabled && !bindingReady && <p className="panel-loading" role="status">本机 Mock 差异与回滚预演要求 Workspace 与 Host revision 已同步就绪；当前未就绪，预演已停用。</p>}
                {mockPreview?.result && <section className="workspace-sync-status" aria-label="本机 Mock 回滚预演" role="status">
                  <div className="workspace-browser-toolbar">
                    <h3>本机 Mock 回滚预演 · v{mockPreview.result.currentRevision} → 模拟 v{mockPreview.result.targetRevision}</h3>
                    <button className="button button-secondary" type="button" onClick={closeMockRevisionPreview}>关闭预演</button>
                  </div>
                  <p>目标是由当前已应用清单合成的示例元数据，不是真实历史快照。以下只预演路径、大小、摘要和权限变化；没有修改服务端状态或文件正文。</p>
                  <p>共 {mockPreview.result.changes.length} 条路径差异。</p>
                  <div className="file-list-wrap"><table className="file-table">
                    <caption className="sr-only">本机 Mock 回滚预演差异</caption>
                    <thead><tr><th scope="col">预演变化</th><th scope="col">路径</th><th scope="col">当前元数据</th><th scope="col">模拟目标元数据</th></tr></thead>
                    <tbody>{mockPreview.result.changes.slice(0, 100).map((change) => <tr key={`${change.path}-${change.change}`}>
                      <td>{change.change === 'added' ? '回滚后新增' : change.change === 'removed' ? '回滚后移除' : '回滚后修改'}</td>
                      <td className="mono">{change.path}</td>
                      <td>{change.current ? manifestMetadataSummary(change.current) : '—'}</td>
                      <td>{change.target ? manifestMetadataSummary(change.target) : '—'}</td>
                    </tr>)}</tbody>
                  </table></div>
                  {mockPreview.result.changes.length > 100 && <p className="panel-loading">仅显示前 100 条；共 {mockPreview.result.changes.length} 条。</p>}
                  <button className="button button-secondary" type="button" onClick={() => setMockPreviewState({ ...mockPreview, confirmingRestore: true, restoreResult: undefined })}>
                    本机 Mock 模拟回滚…
                  </button>
                  {mockPreview.restoreResult === 'success' && <p role="status">本机预演模拟恢复成功；没有发送写请求，Workspace 与本机文件均未改变。</p>}
                  {mockPreview.restoreResult === 'conflict' && <FloatingError>本机预演模拟服务端返回版本冲突；没有发送写请求，Workspace 与本机文件均未改变。</FloatingError>}
                  {mockPreview.confirmingRestore && <section className="workspace-sync-status" aria-labelledby="mock-restore-title">
                    <h3 id="mock-restore-title">确认本机 Mock 回滚</h3>
                    <p>模拟以当前 revision {mockPreview.result.currentRevision} 为预期版本，从 v{mockPreview.result.targetRevision} 恢复。此处不会调用 API、修改 Workspace 或本机文件。</p>
                    <button className="button button-secondary" type="button" onClick={() => confirmMockRestore('success')}>模拟恢复成功</button>
                    <button className="button button-secondary" type="button" onClick={() => confirmMockRestore('conflict')}>模拟版本冲突</button>
                    <button className="button button-secondary" type="button" onClick={() => setMockPreviewState({ ...mockPreview, confirmingRestore: false })}>取消</button>
                  </section>}
                </section>}
                {!validWorkspaceID(binding.workspace.id) || binding.directory !== binding.workspace.id
                  ? <FloatingError>工作区绑定目录无效，已阻止文件访问。</FloatingError>
                  : !withinQuota(binding) ? <p className="panel-loading" role="status">工作区用量超出或配额状态无效；文件暂不显示。</p>
                    : !bindingReady ? <p className="panel-loading" role="status">工作区尚未就绪、存在同步冲突或 Host revision 未追上；文件暂不显示。</p>
                      : (
                        <>
                          <p className="workspace-browser-path" aria-label="当前工作区路径">
                            {binding.workspace.name}{segments.map((segment) => ` / ${segment}`)}
                          </p>
                          {(fileError || downloadError) && <FloatingError>{fileError || downloadError}</FloatingError>}
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
                                      : <>{entry.name}{entry.isSymlink && <small className="muted"> 符号链接</small>}</>}</td>
                                    <td>{entry.isDirectory || entry.size === undefined ? '—' : formatBytes(entry.size)}</td>
                                    <td>{entry.isSymlink ? '不跟随链接' : !entry.isDirectory && <button className="button button-secondary" type="button" onClick={() => void openEntry(entry)} disabled={downloading === entry.path}>{downloading === entry.path ? '下载中…' : '下载'}</button>}</td>
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
