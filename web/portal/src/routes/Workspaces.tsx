import { useEffect, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { Link, useSearchParams } from 'react-router';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import type { components } from '../api/schema';
import { enterHostInNewTab, errorDetails, newIdempotencyKey, portalApi } from '../app/api';
import { useAuth } from '../app/auth';
import { formatBytes, formatDate, hostStateLabel } from '../app/format';
import { useToast } from '../app/toast';
import { continueWorkspaceRestore, isPendingWorkspaceRestore } from '../app/workspace-restore';
import type { PendingWorkspaceRestore } from '../app/workspace-restore';
import { ConfirmDialog } from '../components/ConfirmDialog';
import { ScreenPagination } from '../components/ScreenPagination';
import { WorkspaceTransferPreview } from '../components/WorkspaceTransferPreview';
import { FloatingError, FloatingNotice } from '../components/FloatingError';
import { LocalWorkspaceRecovery } from '../components/LocalWorkspaceRecovery';

type Workspace = Awaited<ReturnType<typeof portalApi.workspaces>>['items'][number];
type Host = Awaited<ReturnType<typeof portalApi.hosts>>['items'][number];
type Binding = Host['workspaces'][number];
type ProjectUploadTarget = {
  hostId: string;
  hostName: string;
  hostState: 'running' | 'stopped';
  workspaceId: string;
  workspaceName: string;
  folders: { folderID: string; projectName: string; relativePath: string; state: string }[];
  blockedMessage?: string;
};
type ProjectDirectorySelection = {
  selectionID: string;
  hostId: string;
  workspaceId: string;
  projectName: string;
  directoryName: string;
};
type PortalProjectBridge = {
  projectUploadTargets?: () => Promise<unknown>;
  selectProjectDirectory?: (hostId: string, workspaceId: string) => Promise<unknown>;
  uploadProject?: (selectionID: string, projectName: string) => Promise<unknown>;
  stopProjectSync?: (selectionID: string) => Promise<unknown>;
  state?: () => Promise<unknown>;
  onState?: (listener: (value: unknown) => void) => () => void;
  onSync?: (listener: (value: unknown) => void) => () => void;
};
type LocalProjectState = {
  projectMappings: ProjectDirectorySelection[];
  syncState: 'idle' | 'running' | 'shutdown_unknown';
  syncCheckpoint: { workspaceId: string; projectSelectionID?: string; projectName?: string; sessionState?: string; hostApplicationStatus?: string } | null;
  syncRecovery: { state: string; message?: string } | null;
};
type PendingWorkspaceCreate = { version: 1; name: string; idempotencyKey: string };
type WorkspaceRevisionDiff = components['schemas']['WorkspaceDiff'];
type WorkspaceRestoreRecovery = { pending: PendingWorkspaceRestore | null; invalid: boolean };
const revisionDiffPageSize = 50;
const restorePhaseLabel: Record<PendingWorkspaceRestore['phase'], string> = {
  unbind: '解除电脑文件关联',
  wait_detached: '等待确认电脑文件已解除关联',
  release_host: '释放电脑运行资源',
  wait_stopped: '等待电脑停止',
  restore_workspace: '恢复云端文件版本',
  wait_projection: '等待云端文件准备',
  bind_workspace: '重新关联云端电脑',
  restore_host: '恢复云端电脑',
  wait_running: '等待电脑应用新文件版本',
};
const workspaceStateLabel: Record<Workspace['state'], string> = { ready: '服务端已保存', deleting: '删除中', deleted: '已删除' };
const bindingStateLabel: Record<Binding['state'], string> = {
  preparing: '正在应用', active: '已应用', unbinding: '正在解绑', detached: '未绑定', failed: '应用失败',
};
const workspaceDiffActionLabel: Record<WorkspaceRevisionDiff['changes'][number]['action'], string> = {
  added: '新增', removed: '移除', changed: '修改', renamed: '重命名',
};
const workspaceDiffDisplayLabel: Record<WorkspaceRevisionDiff['changes'][number]['display_status'], string> = {
  text: '文本可预览', binary: '二进制，不展示内容', too_large: '文件过大，不展示内容',
  metadata_only: '仅元数据', not_applicable: '无正文',
};

function portalProjectBridge(): PortalProjectBridge | undefined {
  if (typeof window === 'undefined') return undefined;
  return (window as Window & { morde?: PortalProjectBridge }).morde;
}

function hasControlCharacters(value: string): boolean {
  return Array.from(value).some((character) => {
    const codePoint = character.codePointAt(0) ?? 0;
    return codePoint <= 0x1f || (codePoint >= 0x7f && codePoint <= 0x9f);
  });
}

function safeDisplayText(value: string, maxLength: number): string {
  return Array.from(value, (character) => hasControlCharacters(character) ? ' ' : character).join('').trim().slice(0, maxLength);
}

function parseProjectUploadTargets(value: unknown): ProjectUploadTarget[] {
  if (!Array.isArray(value) || value.length > 1000) throw new Error('云端电脑列表无效');
  return value.map((item) => {
    if (!item || typeof item !== 'object' || Array.isArray(item)) throw new Error('云端电脑列表无效');
    const target = item as Record<string, unknown>;
    if (typeof target.hostId !== 'string' || !/^h[a-z0-9]{7}$/.test(target.hostId)
      || typeof target.workspaceId !== 'string' || !/^w[a-z0-9]{7}$/.test(target.workspaceId)
      || typeof target.hostName !== 'string' || typeof target.workspaceName !== 'string'
      || (target.hostState !== 'running' && target.hostState !== 'stopped')
      || !Array.isArray(target.folders) || target.folders.length > 1000
      || (target.blockedMessage !== undefined && typeof target.blockedMessage !== 'string')) {
      throw new Error('云端电脑列表无效');
    }
    const folders = target.folders.map((folder) => {
      if (!folder || typeof folder !== 'object' || Array.isArray(folder)) throw new Error('云端项目目录列表无效');
      const value = folder as Record<string, unknown>;
      if (typeof value.folderID !== 'string' || typeof value.projectName !== 'string'
        || typeof value.relativePath !== 'string' || typeof value.state !== 'string') {
        throw new Error('云端项目目录列表无效');
      }
      return { folderID: value.folderID, projectName: value.projectName, relativePath: value.relativePath, state: value.state };
    });
    return {
      hostId: target.hostId,
      hostName: safeDisplayText(target.hostName, 120) || target.hostId,
      hostState: target.hostState,
      workspaceId: target.workspaceId,
      workspaceName: safeDisplayText(target.workspaceName, 120) || target.workspaceId,
      folders,
      ...(typeof target.blockedMessage === 'string' ? { blockedMessage: safeDisplayText(target.blockedMessage, 240) } : {}),
    };
  });
}

function parseProjectDirectorySelection(value: unknown, target: ProjectUploadTarget): ProjectDirectorySelection | null {
  if (value === null) return null;
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('本机目录选择结果无效');
  const selection = value as Record<string, unknown>;
  if (typeof selection.selectionID !== 'string' || !/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(selection.selectionID)
    || selection.hostId !== target.hostId || selection.workspaceId !== target.workspaceId
    || typeof selection.projectName !== 'string' || !selection.projectName || [...selection.projectName].length > 120
    || typeof selection.directoryName !== 'string' || !selection.directoryName || selection.directoryName.length > 255
    || /[\\/]/.test(selection.directoryName) || hasControlCharacters(selection.directoryName)) {
    throw new Error('本机目录选择结果无效');
  }
  return {
    selectionID: selection.selectionID,
    hostId: target.hostId,
    workspaceId: target.workspaceId,
    projectName: selection.projectName.normalize('NFC'),
    directoryName: selection.directoryName,
  };
}

function parseLocalProjectState(value: unknown): LocalProjectState {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('本机同步状态无效');
  const state = value as Record<string, unknown>;
  if (!Array.isArray(state.projectMappings) || state.projectMappings.length > 1000
    || !['idle', 'running', 'shutdown_unknown'].includes(String(state.syncState))) throw new Error('本机同步状态无效');
  const mappings = state.projectMappings.map((item) => {
    if (!item || typeof item !== 'object' || Array.isArray(item)) throw new Error('本机项目目录状态无效');
    const mapping = item as Record<string, unknown>;
    if (typeof mapping.hostId !== 'string' || !/^h[a-z0-9]{7}$/.test(mapping.hostId)
      || typeof mapping.workspaceId !== 'string' || !/^w[a-z0-9]{7}$/.test(mapping.workspaceId)) throw new Error('本机项目目录状态无效');
    return parseProjectDirectorySelection(item, { hostId: mapping.hostId, workspaceId: mapping.workspaceId,
      hostName: '', hostState: 'stopped', workspaceName: '', folders: [] })!;
  });
  const checkpoint = state.syncCheckpoint;
  if (checkpoint !== null && (!checkpoint || typeof checkpoint !== 'object' || Array.isArray(checkpoint))) throw new Error('本机同步恢复点无效');
  const saved = checkpoint as Record<string, unknown> | null;
  if (saved && (saved.mode !== 'project_upload' || typeof saved.workspaceId !== 'string' || !/^w[a-z0-9]{7}$/.test(saved.workspaceId)
    || ['projectSelectionID', 'projectName', 'sessionState', 'hostApplicationStatus'].some((key) => saved[key] !== undefined && typeof saved[key] !== 'string'))) {
    throw new Error('本机同步恢复点无效');
  }
  const recovery = state.syncRecovery;
  if (recovery !== null && (!recovery || typeof recovery !== 'object' || Array.isArray(recovery)
    || typeof (recovery as Record<string, unknown>).state !== 'string'
    || ((recovery as Record<string, unknown>).message !== undefined && typeof (recovery as Record<string, unknown>).message !== 'string'))) {
    throw new Error('本机同步恢复状态无效');
  }
  return { projectMappings: mappings, syncState: state.syncState as LocalProjectState['syncState'],
    syncCheckpoint: saved as LocalProjectState['syncCheckpoint'], syncRecovery: recovery as LocalProjectState['syncRecovery'] };
}

function projectUploadStatus(value: unknown): string {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return '同步结果未知，请核对状态。';
  const result = value as Record<string, unknown>;
  if (result.cancelled === true) return '已取消，未开始同步。';
  const session = result.session && typeof result.session === 'object' && !Array.isArray(result.session)
    ? result.session as Record<string, unknown> : {};
  const receipt = result.hostApplication && typeof result.hostApplication === 'object' && !Array.isArray(result.hostApplication)
    ? result.hostApplication as Record<string, unknown> : {};
  if (session.state === 'completed' && result.cloudStatus === 'confirmed') {
    if (result.hostStatus === 'applied' && receipt.status === 'applied'
      && Number.isSafeInteger(receipt.targetRevision) && Number.isSafeInteger(receipt.appliedRevision)
      && Number(receipt.targetRevision) >= 0 && Number(receipt.appliedRevision) >= Number(receipt.targetRevision)
      && Array.isArray(receipt.impacts) && receipt.impacts.length === 0) return '同步完成，目标电脑已应用本次项目。';
    if (receipt.status === 'conflict') return `云端已确认，目标电脑存在冲突；恢复点已保留。${projectConflictPaths(receipt.impacts)}`;
    if (receipt.status === 'unknown' || result.hostStatus === 'unknown') return '云端已确认，目标电脑应用状态未知；恢复点已保留。';
    return '云端已确认，等待目标电脑应用。';
  }
  const messages: Record<string, string> = { running: '同步仍在进行，恢复点已保留。', conflict: '发现冲突，本次同步未提交。',
    failed: '同步失败，云端未确认完成。', cancelled: '同步已取消，云端未确认完成。', expired: '同步会话已过期，云端未确认完成。' };
  return typeof session.state === 'string' ? `${messages[session.state] ?? '同步结果待确认。'}${session.state === 'conflict' ? projectConflictPaths(result.conflicts) : ''}` : '同步结果未知，请核对状态。';
}

function projectConflictPaths(value: unknown): string {
  if (!Array.isArray(value)) return '';
  const paths = value.slice(0, 8).flatMap((item: unknown) => {
    if (!item || typeof item !== 'object' || Array.isArray(item)) return [];
    const path = (item as Record<string, unknown>).path;
    if (path === '.') return ['项目根目录'];
    if (typeof path !== 'string' || path.length > 4096 || path.includes('\\') || hasControlCharacters(path)
      || path.split('/').some((part) => !part || part === '.' || part === '..')) return [];
    return [path];
  });
  return paths.length ? ` 受影响路径：${paths.join('、')}。` : '';
}

function validateProjectName(value: string): { name: string; error: string | null } {
  const name = value.normalize('NFC');
  if (!name || name.trim() !== name || [...name].length > 120 || name === '.' || name === '..'
    || /[\\/<>:"|?*]/u.test(name) || hasControlCharacters(name) || /[. ]$/.test(name)) {
    return { name, error: '请输入有效的项目目录名（最多 120 个字符，不能包含路径或系统保留符号）。' };
  }
  const base = name.split('.', 1)[0]!.toUpperCase();
  if (['CON', 'PRN', 'AUX', 'NUL'].includes(base) || /^(?:COM|LPT)[1-9]$/.test(base)) {
    return { name, error: '该项目名为系统保留名称，请换一个名称。' };
  }
  return { name, error: null };
}

function workspaceDiffRestorePaths(changes: WorkspaceRevisionDiff['changes']): string[] {
  const paths = new Map<string, string>();
  for (const change of changes) {
    for (const path of [change.path, change.before?.path, change.after?.path]) {
      if (path) paths.set(path.toLowerCase(), path);
    }
  }
  return [...paths.values()]
    .filter((path, _, all) => !all.some((parent) => parent !== path && path.toLowerCase().startsWith(`${parent.toLowerCase()}/`)))
    .sort((left, right) => left.toLowerCase().localeCompare(right.toLowerCase()));
}

function workspaceDiffVersionSummary(version: WorkspaceRevisionDiff['changes'][number]['before']): string {
  if (!version) return '—';
  if (version.kind === 'directory') return '目录';
  const size = version.size_bytes === undefined ? '大小未知' : `${version.size_bytes.toLocaleString('zh-CN')} B`;
  const hash = version.sha256 ?? '哈希未知';
  const mode = version.mode === undefined ? '权限未知' : `权限 ${version.mode.toString(8).padStart(3, '0')}`;
  return `文件 · ${size} · SHA-256 ${hash} · ${mode}`;
}

function pendingCreateKey(userID: string) {
  return `morde.pending-workspace-create.${userID}`;
}

function pendingRestoreKey(userID: string) {
  return 'morde.pending-workspace-restore.' + userID;
}

function readPendingWorkspaceRestore(userID?: string): WorkspaceRestoreRecovery {
  if (!userID) return { pending: null, invalid: false };
  try {
    const raw = localStorage.getItem(pendingRestoreKey(userID));
    if (!raw) return { pending: null, invalid: false };
    const value: unknown = JSON.parse(raw);
    return isPendingWorkspaceRestore(value) && value.ownerID === userID
      ? { pending: value, invalid: false }
      : { pending: null, invalid: true };
  } catch {
    return { pending: null, invalid: true };
  }
}

function isPendingWorkspaceCreate(value: unknown): value is PendingWorkspaceCreate {
  if (!value || typeof value !== 'object') return false;
  const pending = value as Partial<PendingWorkspaceCreate>;
  return pending.version === 1 && typeof pending.name === 'string' && pending.name.trim() === pending.name
    && pending.name.length > 0 && pending.name.length <= 100
    && typeof pending.idempotencyKey === 'string' && /^[A-Za-z0-9._~-]{8,128}$/.test(pending.idempotencyKey);
}

function readPendingWorkspaceCreate(userID?: string): { pending: PendingWorkspaceCreate | null; invalid: boolean } {
  if (!userID) return { pending: null, invalid: false };
  try {
    const raw = localStorage.getItem(pendingCreateKey(userID));
    if (!raw) return { pending: null, invalid: false };
    const value: unknown = JSON.parse(raw);
    return isPendingWorkspaceCreate(value) ? { pending: value, invalid: false } : { pending: null, invalid: true };
  } catch {
    return { pending: null, invalid: true };
  }
}

export function Workspaces() {
  const { user } = useAuth();
  return <WorkspacesContent key={user?.id ?? 'anonymous'} user={user} />;
}

function WorkspacesContent({ user }: { user: ReturnType<typeof useAuth>['user'] }) {
  const client = useQueryClient();
  const { notify } = useToast();
  const [searchParams] = useSearchParams();
  const computerID = searchParams.get('host') ?? '';
  const [selected, setSelected] = useState<Workspace | null>(null);
  const [selectedBinding, setSelectedBinding] = useState<{ host: Host; binding: Binding } | null>(null);
  const [restored] = useState(() => readPendingWorkspaceCreate(user?.id));
  const [name, setName] = useState(restored.pending?.name ?? '');
  const [validationError, setValidationError] = useState('');
  const pendingWorkspaceCreate = useRef<PendingWorkspaceCreate | null>(restored.pending);
  const [recoveryPending, setRecoveryPending] = useState(!!restored.pending);
  const [recoveryError, setRecoveryError] = useState(restored.invalid);
  const [hostTargets, setHostTargets] = useState<Record<string, string>>({});
  const [progressWorkspaceID, setProgressWorkspaceID] = useState('');
  const [transferWorkspaceID, setTransferWorkspaceID] = useState('');
  const [revisionWorkspaceID, setRevisionWorkspaceID] = useState('');
  const [revisionFromInput, setRevisionFromInput] = useState('0');
  const [revisionToInput, setRevisionToInput] = useState('0');
  const [revisionCompareLoading, setRevisionCompareLoading] = useState(false);
  const [revisionCompareError, setRevisionCompareError] = useState('');
  const [revisionDiff, setRevisionDiff] = useState<WorkspaceRevisionDiff | null>(null);
  const [revisionDiffPage, setRevisionDiffPage] = useState(0);
  const [restoreConfirmationOpen, setRestoreConfirmationOpen] = useState(false);
  const [restoreRecovery, setRestoreRecovery] = useState(() => readPendingWorkspaceRestore(user?.id));
  const pendingRestoreRef = useRef(restoreRecovery.pending);
  const [restoreWorkflowBusy, setRestoreWorkflowBusy] = useState(false);
  const [restoreWorkflowError, setRestoreWorkflowError] = useState('');
  const [writersStoppedConfirmed, setWritersStoppedConfirmed] = useState(false);
  const projectBridge = portalProjectBridge();
  const projectBridgeAvailable = Boolean(projectBridge?.projectUploadTargets && projectBridge.selectProjectDirectory);
  const [selectedProjectTargetKey, setSelectedProjectTargetKey] = useState('');
  const [projectDirectorySelection, setProjectDirectorySelection] = useState<ProjectDirectorySelection | null>(null);
  const [projectNameDraft, setProjectNameDraft] = useState('');
  const [confirmedProjectName, setConfirmedProjectName] = useState('');
  const [projectDirectoryBusy, setProjectDirectoryBusy] = useState(false);
  const [projectDirectoryError, setProjectDirectoryError] = useState('');
  const [projectDirectoryNotice, setProjectDirectoryNotice] = useState('');
  const [projectNameError, setProjectNameError] = useState('');
  const [localProjectState, setLocalProjectState] = useState<LocalProjectState | null>(null);
  const [localProjectError, setLocalProjectError] = useState('');
  const [projectUploadBusy, setProjectUploadBusy] = useState(false);
  const [projectStopBusy, setProjectStopBusy] = useState(false);
  const [projectUploadMessage, setProjectUploadMessage] = useState('');
  const projectStateRevision = useRef(0);
  const projectSelectionRevision = useRef(0);
  const currentProjectOwner = useRef(user?.id);
  const revisionRequestID = useRef(0);
  useEffect(() => {
    currentProjectOwner.current = user?.id;
    return () => { currentProjectOwner.current = undefined; };
  }, [user?.id]);
  useEffect(() => {
    if (!projectBridge?.state || !user?.id) return;
    let active = true;
    const apply = (value: unknown) => {
      if (!active) return;
      projectStateRevision.current += 1;
      try { setLocalProjectState(parseLocalProjectState(value)); setLocalProjectError(''); }
      catch { setLocalProjectState(null); setLocalProjectError('无法确认本机同步状态，请重试。'); }
    };
    const stopState = projectBridge.onState?.(apply);
    const stopSync = projectBridge.onSync?.((value) => {
      if (!active || !value || typeof value !== 'object' || Array.isArray(value)) return;
      const event = value as Record<string, unknown>;
      if (event.mode !== 'project_upload') return;
      if (event.state === 'running') setProjectUploadMessage('同步进行中…');
      else if (event.state === 'shutdown_unknown' || event.state === 'offline') setProjectUploadMessage('同步状态未知，恢复点已保留。');
    });
    const startedAt = projectStateRevision.current;
    void projectBridge.state().then((value) => { if (active && startedAt === projectStateRevision.current) apply(value); })
      .catch(() => { if (active && startedAt === projectStateRevision.current) {
        setLocalProjectState(null); setLocalProjectError('无法读取本机同步状态，请重试。');
      } });
    return () => { active = false; stopState?.(); stopSync?.(); };
  }, [projectBridge, user?.id]);
  const projectCapabilities = useQuery({
    queryKey: ['project-upload-capabilities'],
    queryFn: portalApi.capabilities,
    enabled: projectBridgeAvailable,
    retry: false,
  });
  const supportedFeatures = projectCapabilities.data?.supported_features ?? [];
  const projectUploadGateEnabled = projectCapabilities.isSuccess && !projectCapabilities.isError
    && supportedFeatures.includes('workspace_sync') && supportedFeatures.includes('project_upload');
  const projectPreferences = useQuery({
    queryKey: ['user-preferences'],
    queryFn: portalApi.preferences,
    enabled: projectUploadGateEnabled,
    retry: false,
  });
  const projectTargets = useQuery({
    queryKey: ['project-upload-targets', user?.id],
    queryFn: async () => {
      if (!projectBridge?.projectUploadTargets) throw new Error('当前桌面客户端不支持项目目录选择。');
      return parseProjectUploadTargets(await projectBridge.projectUploadTargets());
    },
    enabled: projectBridgeAvailable && projectCapabilities.isSuccess && projectUploadGateEnabled,
    retry: false,
  });
  const hosts = useQuery({
    queryKey: ['hosts'], queryFn: portalApi.hosts,
    refetchInterval: (query) => query.state.data?.items.some((host) => host.state === 'creating' || host.state === 'deleting' || host.state === 'stopping'
      || host.workspaces.some((binding) => binding.state === 'preparing' || binding.state === 'active' || binding.state === 'unbinding')) ? 3000 : false,
  });
  const configuredDefaultHost = hosts.data?.items.find((host) => host.owner_id === user?.id
    && host.id === projectPreferences.data?.default_host_id);
  const configuredDefaultWorkspace = configuredDefaultHost?.workspaces.find((binding) => binding.state !== 'detached');
  const configuredDefaultProjectTarget = configuredDefaultHost && configuredDefaultWorkspace
    ? projectTargets.data?.find((target) => target.hostId === configuredDefaultHost.id
      && target.workspaceId === configuredDefaultWorkspace.workspace.id)
    : undefined;
  const selectedProjectTarget = projectTargets.data?.find((target) => `${target.hostId}/${target.workspaceId}` === selectedProjectTargetKey)
    ?? configuredDefaultProjectTarget
    ?? (projectPreferences.isSuccess && !projectPreferences.data.default_host_id ? projectTargets.data?.[0] : undefined);
  const scopedProjectState = localProjectState;
  const scopedProjectSelection = projectDirectorySelection;
  const savedProjectSelection = scopedProjectState?.projectMappings.find((item) => item.hostId === selectedProjectTarget?.hostId
    && item.workspaceId === selectedProjectTarget.workspaceId);
  const projectSelectionMatchesTarget = Boolean(selectedProjectTarget && scopedProjectSelection
    && scopedProjectSelection.hostId === selectedProjectTarget.hostId
    && scopedProjectSelection.workspaceId === selectedProjectTarget.workspaceId);
  useEffect(() => {
    if (!savedProjectSelection || scopedProjectSelection || !user?.id) return;
    // Restore a saved native directory selection into the editable form once it arrives.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    setProjectDirectorySelection(savedProjectSelection);
    setProjectNameDraft(savedProjectSelection.projectName);
    setConfirmedProjectName('');
  }, [savedProjectSelection, scopedProjectSelection, user?.id]);
  const projectCheckpoint = scopedProjectState?.syncCheckpoint;
  const checkpointMatchesSelection = Boolean(projectCheckpoint && scopedProjectSelection
    && projectCheckpoint.workspaceId === scopedProjectSelection.workspaceId
    && projectCheckpoint.projectSelectionID === scopedProjectSelection.selectionID
    && projectCheckpoint.projectName === confirmedProjectName);
  const receiptSelection = projectCheckpoint?.sessionState === 'completed' && projectCheckpoint.projectName
    ? scopedProjectState?.projectMappings.find((item) => item.selectionID === projectCheckpoint.projectSelectionID
      && item.workspaceId === projectCheckpoint.workspaceId && item.projectName === projectCheckpoint.projectName) : undefined;
  const projectNameValidation = validateProjectName(projectNameDraft);
  const query = useQuery({
    queryKey: ['workspaces'], queryFn: portalApi.workspaces,
    refetchInterval: (current) => current.state.data?.items.some((workspace) => workspace.state === 'deleting')
      || hosts.data?.items.some((host) => host.state === 'creating' || host.state === 'deleting' || host.state === 'stopping'
        || host.workspaces.some((binding) => binding.state === 'preparing' || binding.state === 'unbinding'
          || (binding.state === 'active' && binding.applied_revision !== binding.desired_revision))) ? 3000 : false,
  });
  const computer = computerID ? hosts.data?.items.find((host) => host.id === computerID && host.owner_id === user?.id) : undefined;
  const computerWorkspaceIDs = new Set(computer?.workspaces.filter((binding) => binding.state !== 'detached').map((binding) => binding.workspace.id) ?? []);
  const visibleWorkspaces = (query.data?.items ?? []).filter((item) => !computerID || computerWorkspaceIDs.has(item.id));
  const revisionWorkspace = visibleWorkspaces.find((item) => item.id === revisionWorkspaceID) ?? null;
  const transferWorkspace = visibleWorkspaces.find((item) => item.id === transferWorkspaceID) ?? null;
  const transferHost = transferWorkspace
    ? hosts.data?.items.find((host) => host.workspaces.some((binding) => binding.workspace.id === transferWorkspace.id && binding.state !== 'detached'))
    : undefined;
  const transferBinding = transferWorkspace
    ? transferHost?.workspaces.find((binding) => binding.workspace.id === transferWorkspace.id && binding.state !== 'detached')
    : undefined;
  const revisionBindings = revisionWorkspace ? hosts.data?.items.flatMap((host) => host.workspaces
    .filter((binding) => binding.workspace.id === revisionWorkspace.id && binding.state !== 'detached')
    .map((binding) => ({ host, binding }))) ?? [] : [];
  const revisionBoundHost = revisionBindings.length === 1 && revisionBindings[0]!.host.owner_id === user?.id
    ? revisionBindings[0] : null;
  const revisionHostRestoreReady = revisionBindings.length === 0 || Boolean(revisionBoundHost
    && revisionBoundHost.host.state === 'running' && revisionBoundHost.binding.state === 'active'
    && revisionBoundHost.binding.sync_state === 'healthy' && revisionBoundHost.binding.sync_conflicts.length === 0
    && revisionWorkspace && revisionBoundHost.binding.applied_revision === revisionWorkspace.revision
    && revisionBoundHost.binding.desired_revision === revisionWorkspace.revision);
  const canRestoreRevision = Boolean(revisionWorkspace && revisionDiff && hosts.isSuccess && revisionHostRestoreReady
    && !restoreRecovery.pending && !restoreRecovery.invalid
    && revisionDiff.workspace_id === revisionWorkspace.id && revisionDiff.to_revision === revisionWorkspace.revision
    && revisionDiff.from_revision < revisionWorkspace.revision && revisionDiff.changes.length > 0);
  const restoreBlockedReason = restoreRecovery.invalid ? '本机恢复记录无法读取；核对 Host 与 Workspace 状态前不能开始新的恢复。'
    : restoreRecovery.pending ? '已有恢复流程待核对；请先继续或完成该流程。'
      : revisionDiff && revisionWorkspace && revisionDiff.to_revision !== revisionWorkspace.revision ? '恢复前请将目标 revision 设为当前版本。'
        : !hosts.isSuccess ? '正在确认主机绑定状态，暂不能恢复。'
          : revisionBindings.length > 1 ? '检测到多个 Host 绑定；状态需要先核对，暂不能恢复。'
            : revisionBindings.length === 1 && !revisionBoundHost ? '绑定 Host 不属于当前账号；无法由此页面安全解绑和恢复。'
              : revisionBoundHost && revisionBoundHost.host.state !== 'running' ? '安全恢复需要先在运行中的 Host 完成解绑；请先恢复 Host 再重试。'
                : revisionBoundHost && (revisionBoundHost.binding.state !== 'active' || revisionBoundHost.binding.sync_state !== 'healthy'
                  || revisionBoundHost.binding.applied_revision !== revisionWorkspace?.revision
                  || revisionBoundHost.binding.desired_revision !== revisionWorkspace?.revision
                  || revisionBoundHost.binding.sync_conflicts.length > 0) ? 'Host Workspace 绑定或同步状态未就绪；解决冲突后再恢复。'
                  : revisionDiff?.changes.length === 0 ? '当前没有可恢复的差异路径。' : '';
  const transferCheckpointReady = transferWorkspace?.state === 'ready' && (!transferHost || (
    transferHost.state === 'stopped' && transferBinding?.state === 'active' && transferBinding.sync_state === 'healthy'
    && transferBinding.applied_revision === transferWorkspace.revision
    && transferBinding.desired_revision === transferWorkspace.revision
  ));
  const deviceProgress = useQuery({
    queryKey: ['workspace-device-sync-progress', progressWorkspaceID],
    queryFn: () => portalApi.workspaceDeviceSyncProgress(progressWorkspaceID),
    enabled: Boolean(progressWorkspaceID),
    refetchInterval: () => progressWorkspaceID ? 3000 : false,
  });
  const ownedRunningHosts = hosts.data?.items.filter((host) => host.owner_id === user?.id && host.state === 'running') ?? [];
  const refreshWorkspaceState = () => Promise.all([
    client.invalidateQueries({ queryKey: ['workspaces'] }),
    client.invalidateQueries({ queryKey: ['hosts'] }),
  ]);
  const create = useMutation({
    mutationFn: (request: { name: string; idempotencyKey: string }) => portalApi.createWorkspace({ name: request.name }, request.idempotencyKey),
        onSuccess: async (_workspace, request) => {
      if (user) {
        try { localStorage.removeItem(pendingCreateKey(user.id)); } catch { /* Replaying the same key remains safe if local cleanup fails. */ }
      }
      if (pendingWorkspaceCreate.current?.idempotencyKey === request.idempotencyKey) pendingWorkspaceCreate.current = null;
      setRecoveryPending(false);
      setRecoveryError(false);
      setName('');
      setValidationError('');
      await client.invalidateQueries({ queryKey: ['workspaces'] });
      notify('工作区已在服务端创建并保存。', 'success');
    },
    onError: (error) => {
      setRecoveryPending(true);
      notify(errorDetails(error).message, 'error');
    },
  });
  const bind = useMutation({
    mutationFn: ({ host, workspace }: { host: Host; workspace: Workspace }) => portalApi.bindWorkspaceToHost(host.id, {
      workspace_id: workspace.id,
      expected_revision: workspace.revision,
      expected_binding_version: host.workspaces.find((binding) => binding.workspace.id === workspace.id)?.binding_version ?? 0,
    }),
    onSuccess: async (binding, request) => {
      if (binding.state === 'failed') notify('云端文件未能应用到电脑，请检查电脑文件状态。', 'error');
      else if (binding.state === 'active' && binding.applied_revision === binding.desired_revision) notify('服务端已确认云端文件应用到电脑。', 'success');
      else notify('服务端已接受关联，正在等待电脑应用云端文件。', 'success');
      setHostTargets((current) => current[request.workspace.id] === request.host.id
        ? { ...current, [request.workspace.id]: '' } : current);
      await refreshWorkspaceState();
    },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });
  const unbind = useMutation({
    mutationFn: ({ host, binding }: { host: Host; binding: Binding }) => portalApi.unbindWorkspaceFromHost(host.id, binding.workspace.id, {
      expected_binding_version: binding.binding_version,
      writers_stopped: true,
    }),
    onSuccess: async (binding) => {
      setSelectedBinding(null);
      await refreshWorkspaceState();
      notify(binding.state === 'unbinding' ? '已提交解除关联，服务端正在等待操作完成并校验文件。' : '已解除关联，服务端文件和电脑磁盘副本均已保留。', 'success');
    },
    onError: async (error) => {
      setSelectedBinding(null);
      await refreshWorkspaceState();
      notify(errorDetails(error).message, 'error');
    },
  });
  const retryHostSync = useMutation({
    mutationFn: ({ host, binding }: { host: Host; binding: Binding }) => portalApi.retryHostWorkspaceSync(host.id, binding.workspace.id, binding.binding_version),
    onSuccess: async () => {
      await client.invalidateQueries({ queryKey: ['hosts'] });
      notify('已提交电脑文件同步重试。', 'success');
    },
    onError: async (error) => {
      await client.invalidateQueries({ queryKey: ['hosts'] });
      notify(errorDetails(error).message, 'error');
    },
  });
  const remove = useMutation({
    mutationFn: portalApi.deleteWorkspace,
    onSuccess: () => { setSelected(null); notify('已提交工作区删除请求。', 'success'); void client.invalidateQueries({ queryKey: ['workspaces'] }); },
    onError: async (error) => {
      setSelected(null);
      await refreshWorkspaceState();
      notify(errorDetails(error).message, 'error');
    },
  });
  const saveRestoreOperation = (operation: PendingWorkspaceRestore) => {
    if (!user || operation.ownerID !== user.id) throw new Error('当前账号与本机恢复记录不匹配。');
    localStorage.setItem(pendingRestoreKey(user.id), JSON.stringify(operation));
    pendingRestoreRef.current = operation;
    setRestoreRecovery({ pending: operation, invalid: false });
  };
  const resumeRestoreWorkflow = async (operation: PendingWorkspaceRestore) => {
    if (!user || operation.ownerID !== user.id || restoreWorkflowBusy) return;
    setRestoreWorkflowBusy(true);
    setRestoreWorkflowError('');
    setRestoreConfirmationOpen(false);
    try {
      await continueWorkspaceRestore(operation, saveRestoreOperation);
      const restoredRevision = pendingRestoreRef.current?.restoredRevision;
      localStorage.removeItem(pendingRestoreKey(user.id));
      pendingRestoreRef.current = null;
      setRestoreRecovery({ pending: null, invalid: false });
      setWritersStoppedConfirmed(false);
      setRevisionWorkspaceID('');
      setRevisionDiff(null);
      setRevisionDiffPage(0);
      await refreshWorkspaceState();
      notify(import.meta.env.VITE_MOCK_API === '1'
        ? '本地演示已模拟完成安全恢复流程；没有写入真实云端文件。'
        : '云端文件已恢复为版本 v' + String(restoredRevision) + (operation.hostID
          ? '，云端电脑与新版本应用状态均已确认。' : '，新版本文件已确认。'), 'success');
    } catch (error) {
      setRestoreWorkflowError(errorDetails(error).message);
      await refreshWorkspaceState();
      notify('安全恢复已暂停；状态未确认前不会继续。' + errorDetails(error).message, 'error');
    } finally {
      setRestoreWorkflowBusy(false);
    }
  };
  const createWorkspace = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (create.isPending || recoveryError || !user) return;
    const cleanName = name.trim();
    if (!cleanName) { setValidationError('请填写工作区名称。'); return; }
    setValidationError('');
    if (!pendingWorkspaceCreate.current || pendingWorkspaceCreate.current.name !== cleanName) {
      pendingWorkspaceCreate.current = { version: 1, name: cleanName, idempotencyKey: newIdempotencyKey() };
    }
    try { localStorage.setItem(pendingCreateKey(user.id), JSON.stringify(pendingWorkspaceCreate.current)); }
    catch {
      notify('浏览器无法保存创建恢复信息，请检查站点存储后重试。', 'error');
      return;
    }
    create.mutate({ name: cleanName, idempotencyKey: pendingWorkspaceCreate.current!.idempotencyKey });
  };
  const openRevisionDiff = (workspace: Workspace) => {
    revisionRequestID.current++;
    setRestoreConfirmationOpen(false);
    setWritersStoppedConfirmed(false);
    setRestoreWorkflowError('');
    setRevisionWorkspaceID(workspace.id);
    setRevisionFromInput(String(Math.max(0, workspace.revision - 1)));
    setRevisionToInput(String(workspace.revision));
    setRevisionCompareLoading(false);
    setRevisionCompareError('');
    setRevisionDiff(null);
    setRevisionDiffPage(0);
  };
  const closeRevisionDiff = () => {
    revisionRequestID.current++;
    setRestoreConfirmationOpen(false);
    setWritersStoppedConfirmed(false);
    setRevisionWorkspaceID('');
    setRevisionCompareLoading(false);
    setRevisionCompareError('');
    setRevisionDiff(null);
    setRevisionDiffPage(0);
  };
  const compareRevisionDiff = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!revisionWorkspace) return;
    const fromRevision = Number(revisionFromInput);
    const toRevision = Number(revisionToInput);
    if (!/^\d+$/.test(revisionFromInput.trim()) || !/^\d+$/.test(revisionToInput.trim())
      || !Number.isSafeInteger(fromRevision) || !Number.isSafeInteger(toRevision)
      || fromRevision < 0 || toRevision < 0
      || fromRevision > revisionWorkspace.revision || toRevision > revisionWorkspace.revision) {
      setRevisionDiff(null);
      setRevisionCompareError(`revision 必须是 0 到当前版本 v${revisionWorkspace.revision} 之间的整数。`);
      return;
    }

    const requestID = ++revisionRequestID.current;
    setRevisionCompareLoading(true);
    setRevisionCompareError('');
    setRevisionDiff(null);
    setRevisionDiffPage(0);
    try {
      const diff = await portalApi.workspaceDiff(revisionWorkspace.id, fromRevision, toRevision);
      if (!diff || diff.workspace_id !== revisionWorkspace.id || diff.from_revision !== fromRevision
        || diff.to_revision !== toRevision || !Array.isArray(diff.changes) || diff.changes.length > 500
        || diff.changes.some((change) => !change || typeof change.path !== 'string' || !change.path.trim())) {
        throw new Error('历史差异响应的工作区、revision 或路径格式无效。');
      }
      if (requestID === revisionRequestID.current) {
        setRevisionDiff(diff);
      }
    } catch (error) {
      if (requestID === revisionRequestID.current) {
        const details = errorDetails(error);
        setRevisionCompareError(details.code === 'conflict' || details.code === 'workspace_conflict'
          ? `历史 revision 冲突：${details.message}` : `读取历史 revision 失败：${details.message}`);
      }
    } finally {
      if (requestID === revisionRequestID.current) setRevisionCompareLoading(false);
    }
  };
  const confirmWorkspaceRestore = () => {
    if (!writersStoppedConfirmed) return;
    const pending = pendingRestoreRef.current;
    if (pending) {
      void resumeRestoreWorkflow(pending);
      return;
    }
    if (!canRestoreRevision || !revisionWorkspace || !revisionDiff) return;
    if (!user) return;
    const paths = workspaceDiffRestorePaths(revisionDiff.changes);
    if (paths.length === 0) return;
    const operation: PendingWorkspaceRestore = {
      version: 1,
      ownerID: user.id,
      workspaceID: revisionWorkspace.id,
      hostID: revisionBoundHost?.host.id ?? null,
      targetRevision: revisionDiff.from_revision,
      expectedCurrentRevision: revisionWorkspace.revision,
      paths,
      phase: revisionBoundHost ? 'unbind' : 'restore_workspace',
      unbindKey: newIdempotencyKey(),
      restoreKey: newIdempotencyKey(),
      bindKey: newIdempotencyKey(),
      restoreRequested: false,
    };
    try {
      saveRestoreOperation(operation);
      void resumeRestoreWorkflow(operation);
    } catch (error) {
      setRestoreWorkflowError(errorDetails(error).message);
      notify('无法保存本机恢复记录；未开始解绑或恢复。', 'error');
    }
  };
  const abandonWorkspaceRestoreRecord = () => {
    if (!user || restoreWorkflowBusy || !restoreRecovery.invalid) return;
    try {
      localStorage.removeItem(pendingRestoreKey(user.id));
      pendingRestoreRef.current = null;
      setRestoreRecovery({ pending: null, invalid: false });
      setRestoreWorkflowError('');
    } catch {
      notify('无法清除本机的恢复记录。', 'error');
    }
  };
  const abandonRecovery = () => {
    if (!user) return;
    try {
      localStorage.removeItem(pendingCreateKey(user.id));
      pendingWorkspaceCreate.current = null;
      setRecoveryPending(false);
      setRecoveryError(false);
      setName('');
      create.reset();
    } catch {
      notify('无法清除本机的创建恢复记录。', 'error');
    }
  };
  const openHost = async (host: Host) => {
    try { await enterHostInNewTab(host.id); }
    catch (error) { notify(errorDetails(error).message, 'error'); }
  };
  const chooseProjectDirectory = async () => {
    if (!selectedProjectTarget || !projectUploadGateEnabled || !projectBridge?.selectProjectDirectory) return;
    const target = selectedProjectTarget;
    const ownerID = user?.id;
    const revision = ++projectSelectionRevision.current;
    setProjectDirectoryBusy(true);
    setProjectDirectoryError('');
    setProjectDirectoryNotice('');
    try {
      const selection = parseProjectDirectorySelection(
        await projectBridge.selectProjectDirectory(target.hostId, target.workspaceId), target,
      );
      if (revision !== projectSelectionRevision.current || ownerID !== currentProjectOwner.current) return;
      if (!selection) {
        setProjectDirectoryNotice('已取消本机目录选择。');
        return;
      }
      setProjectDirectorySelection(selection);
      setProjectNameDraft(selection.projectName);
      setConfirmedProjectName('');
      setProjectNameError('');
      setProjectDirectoryNotice(`已选择本机目录“${selection.directoryName}”。`);
    } catch (error) {
      if (revision === projectSelectionRevision.current && ownerID === currentProjectOwner.current) setProjectDirectoryError(`选择本机目录失败：${errorDetails(error).message}`);
    } finally {
      if (ownerID === currentProjectOwner.current) setProjectDirectoryBusy(false);
    }
  };
  const confirmProjectName = () => {
    const validation = validateProjectName(projectNameDraft);
    setProjectNameError(validation.error ?? '');
    if (!validation.error && scopedProjectSelection) setConfirmedProjectName(validation.name);
  };
  const refreshLocalProjectState = async () => {
    if (!projectBridge?.state || !user?.id) return;
    const ownerID = user.id;
    const revision = projectStateRevision.current;
    try {
      const value = await projectBridge.state();
      if (ownerID !== currentProjectOwner.current || revision !== projectStateRevision.current) return;
      setLocalProjectState(parseLocalProjectState(value));
      setLocalProjectError('');
    } catch {
      if (ownerID !== currentProjectOwner.current || revision !== projectStateRevision.current) return;
      setLocalProjectState(null);
      setLocalProjectError('无法读取本机同步状态，请重试。');
    }
  };
  const startProjectUpload = async (selection: ProjectDirectorySelection, name: string, receiptOnly = false) => {
    if (!projectBridge?.uploadProject || !scopedProjectState || projectUploadBusy || !user?.id) return;
    if (!receiptOnly && (!projectUploadGateEnabled || !projectSelectionMatchesTarget || selection.selectionID !== scopedProjectSelection?.selectionID
      || !name || name !== confirmedProjectName || scopedProjectState.syncState !== 'idle'
      || (projectCheckpoint && !checkpointMatchesSelection) || scopedProjectState.syncRecovery?.state === 'unknown')) return;
    if (receiptOnly && (scopedProjectState.syncState === 'running' || projectCheckpoint?.sessionState !== 'completed' || receiptSelection?.selectionID !== selection.selectionID
      || projectCheckpoint.projectName !== name)) return;
    const ownerID = user.id;
    setProjectUploadBusy(true);
    setProjectUploadMessage(receiptOnly ? '正在核对目标电脑应用状态…' : '正在准备同步…');
    try {
      const result = await projectBridge.uploadProject(selection.selectionID, name);
      if (ownerID === currentProjectOwner.current) setProjectUploadMessage(projectUploadStatus(result));
    } catch (error) {
      if (ownerID === currentProjectOwner.current) setProjectUploadMessage(`同步未确认：${errorDetails(error).message}`);
    } finally {
      if (ownerID === currentProjectOwner.current) {
        await refreshLocalProjectState();
        setProjectUploadBusy(false);
      }
    }
  };
  const stopProjectUpload = async () => {
    const selectionID = projectCheckpoint?.projectSelectionID;
    if (!selectionID || !projectBridge?.stopProjectSync || projectStopBusy || !user?.id) return;
    const ownerID = user.id;
    setProjectStopBusy(true);
    try {
      const stopped = await projectBridge.stopProjectSync(selectionID);
      if (ownerID === currentProjectOwner.current) setProjectUploadMessage(stopped === true
        ? '同步已停止，恢复点已保留。' : '同步停止状态未知，恢复点已保留。');
    } catch (error) {
      if (ownerID === currentProjectOwner.current) setProjectUploadMessage(`同步停止未确认：${errorDetails(error).message}`);
    } finally {
      if (ownerID === currentProjectOwner.current) { await refreshLocalProjectState(); setProjectStopBusy(false); }
    }
  };

  return <section className="workspace-route">
    {import.meta.env.VITE_MOCK_API === '1' && <FloatingNotice className="notice-info" data-notice-label="演示模式">当前使用本机 MSW；“本地差异演示工作区”的差异内容与恢复结果均为合成数据，不会写入真实云端文件。</FloatingNotice>}
    {computerID && hosts.isSuccess && !computer && <FloatingNotice className="notice-warning">无法确认这台云端电脑的文件访问权限。</FloatingNotice>}
    {restoreRecovery.invalid && <FloatingError>本机文件恢复记录无法读取。请先在云端电脑列表核对状态，不能直接开始另一项恢复。<button type="button" className="button button-secondary" disabled={restoreWorkflowBusy} onClick={abandonWorkspaceRestoreRecord}>我已核对服务端状态，清除损坏记录</button></FloatingError>}
    {restoreRecovery.pending && <FloatingNotice className="notice-warning">
      <p>云端文件安全恢复在“{restorePhaseLabel[restoreRecovery.pending.phase]}”阶段待核对。继续前请再次确认所有外部写入者仍已停止；系统会先读取服务端状态，再复用原幂等键。</p>
      {restoreWorkflowError && <FloatingError>{restoreWorkflowError}</FloatingError>}
      <button type="button" className="button button-secondary" disabled={restoreWorkflowBusy} onClick={() => {
        setWritersStoppedConfirmed(false);
        setRestoreConfirmationOpen(true);
      }}>{restoreWorkflowBusy ? '正在核对并恢复…' : '核对状态并继续恢复'}</button>
    </FloatingNotice>}
    <section className="design-panel" aria-labelledby="project-upload-title">
      <div className="design-panel-header"><div><h2 id="project-upload-title">文件同步</h2><p>选择云端电脑和本机文件夹，手动单向同步到电脑的项目目录。</p></div></div>
      {!projectBridgeAvailable && <p className="muted" role="status">本机文件同步需要 Morde 桌面客户端，请在桌面客户端继续。</p>}
      {projectBridgeAvailable && projectCapabilities.isPending && <FloatingNotice className="notice-info">正在确认服务端文件同步能力…</FloatingNotice>}
      {projectBridgeAvailable && projectCapabilities.isError && <FloatingError>无法确认服务端项目同步能力。为保护云端数据，项目目录和同步操作已关闭。<button type="button" className="button button-secondary button-small" onClick={() => void projectCapabilities.refetch()}>重试</button></FloatingError>}
      {projectBridgeAvailable && projectCapabilities.isSuccess && !projectUploadGateEnabled && <p className="muted" role="status">当前服务尚未开放本机文件同步。</p>}
      {projectBridgeAvailable && !projectBridge?.state && <p className="muted" role="status">当前桌面客户端不支持读取本机同步状态，请更新客户端。</p>}
      {localProjectError && <p className="muted" role="status">{localProjectError} <button type="button" className="button button-secondary button-small" onClick={() => void refreshLocalProjectState()}>重试</button></p>}
      {receiptSelection && projectCheckpoint?.projectName && <div className="inline-actions">
        <span role="status">云端已确认，目标电脑应用状态待核对。</span>
        <button type="button" className="button button-secondary button-small" disabled={projectUploadBusy || scopedProjectState?.syncState === 'running' || !projectBridge?.uploadProject}
          onClick={() => void startProjectUpload(receiptSelection, projectCheckpoint.projectName!, true)}>核对电脑应用状态</button>
      </div>}
      {scopedProjectState?.syncState === 'running' && projectCheckpoint?.projectSelectionID && <button type="button" className="button button-secondary button-small"
        disabled={!projectBridge?.stopProjectSync || projectStopBusy} onClick={() => void stopProjectUpload()}>{projectStopBusy ? '正在停止…' : '停止同步'}</button>}
      {projectBridgeAvailable && projectUploadGateEnabled && <>
        {projectPreferences.isPending && <FloatingNotice className="notice-info">正在读取设置中的默认电脑与 Workspace…</FloatingNotice>}
        {projectPreferences.isError && <FloatingError>无法读取默认电脑设置。请重试，或在下方手动选择已授权目标。<button type="button" className="button button-secondary button-small" onClick={() => void projectPreferences.refetch()}>重试</button></FloatingError>}
        {projectTargets.isPending && <FloatingNotice className="notice-info">正在读取当前账号可用的云端电脑与工作区…</FloatingNotice>}
        {projectTargets.isError && <FloatingError>无法读取云端电脑列表。请检查桌面连接后重试。 <button type="button" className="button button-secondary button-small" onClick={() => void projectTargets.refetch()}>重试</button></FloatingError>}
        {projectTargets.isSuccess && !projectTargets.data.length && <p className="empty-card">当前账号没有可用于项目同步的云端电脑工作区。</p>}
        {projectTargets.isSuccess && projectTargets.data.length > 0 && <>
          {projectPreferences.isSuccess && projectPreferences.data.default_host_id && (hosts.isSuccess || hosts.isError) && !configuredDefaultProjectTarget && <p className="notice notice-warning" role="status">
            {hosts.isError
              ? '无法核对设置中的默认电脑和 Workspace。请先重试读取主机状态，或手动选择已授权目标；系统不会自动切换目标。'
              : '设置中的默认电脑或其 Workspace 当前不可用于目录选择。请手动选择一个可用目标；系统不会自动切换目标。'}
          </p>}
          <ScreenPagination
            items={projectTargets.data}
            getKey={(target) => `${target.hostId}/${target.workspaceId}`}
            estimatedItemHeight={62}
            height="min(max(calc(100dvh - 470px), 74px), 180px)"
            gap={6}
            ariaLabel="项目同步目标"
            className="project-target-list"
            renderItem={(target) => {
              const key = `${target.hostId}/${target.workspaceId}`;
              const isSelected = (selectedProjectTarget?.hostId === target.hostId && selectedProjectTarget.workspaceId === target.workspaceId);
              const isConfiguredDefault = configuredDefaultHost?.id === target.hostId
                && configuredDefaultWorkspace?.workspace.id === target.workspaceId;
              return <article className="schedule-card" style={{ padding: '8px 10px' }}>
                <div className="schedule-card-heading">
                  <div><strong>{target.hostName}{isConfiguredDefault && <small className="table-sub"> · 默认</small>}</strong><span>{target.hostState === 'running' ? '运行中' : '已停止'} · {target.workspaceName} · 已有项目 {target.folders.length} 个</span></div>
                  <button type="button" className={`button ${isSelected ? 'button-primary' : 'button-secondary'} button-small`} aria-pressed={isSelected}
                    disabled={projectDirectoryBusy || projectUploadBusy || scopedProjectState?.syncState === 'running'}
                    onClick={() => {
                      if (selectedProjectTargetKey === key) return;
                      projectSelectionRevision.current += 1;
                      setSelectedProjectTargetKey(key);
                      setProjectDirectorySelection(null);
                      setProjectNameDraft('');
                      setConfirmedProjectName('');
                      setProjectNameError('');
                      setProjectDirectoryError('');
                      setProjectDirectoryNotice('');
                    }}>使用此目标</button>
                </div>
                {target.blockedMessage && isSelected && <small className="table-sub">{target.blockedMessage}</small>}
              </article>;
            }}
          />
          {selectedProjectTarget && <div className="form-stack" style={{ marginTop: 10 }}>
            {selectedProjectTarget.blockedMessage && <p className="notice notice-warning" role="status">{selectedProjectTarget.blockedMessage}</p>}
            <div className="inline-actions">
              <button type="button" className="button button-secondary" disabled={projectDirectoryBusy || projectUploadBusy || scopedProjectState?.syncState === 'running' || Boolean(selectedProjectTarget.blockedMessage)} onClick={() => void chooseProjectDirectory()}>
                {projectDirectoryBusy ? '正在打开目录选择器…' : projectSelectionMatchesTarget ? '重新选择本机目录' : '选择本机目录'}
              </button>
              {projectSelectionMatchesTarget && scopedProjectSelection && <span role="status">已选：{scopedProjectSelection.directoryName}</span>}
            </div>
            {projectDirectoryNotice && <FloatingNotice className="notice-info">{projectDirectoryNotice}</FloatingNotice>}
            {projectDirectoryError && <FloatingError>{projectDirectoryError}</FloatingError>}
            {projectSelectionMatchesTarget && scopedProjectSelection && <>
              <label htmlFor="project-upload-name">项目名</label>
              <input id="project-upload-name" value={projectNameDraft} maxLength={120} disabled={projectUploadBusy || scopedProjectState?.syncState === 'running'} onChange={(event) => {
                setProjectNameDraft(event.target.value);
                setConfirmedProjectName('');
                setProjectNameError('');
              }} />
              {projectNameError && <FloatingError>{projectNameError}</FloatingError>}
              <div className="inline-actions">
                <button type="button" className="button button-secondary" disabled={!projectNameDraft || confirmedProjectName === projectNameValidation.name} onClick={confirmProjectName}>确认项目名</button>
                {confirmedProjectName && <span role="status">项目名已确认：{confirmedProjectName}</span>}
                {confirmedProjectName && <button type="button" className="button button-primary"
                  disabled={!projectBridge?.uploadProject || !scopedProjectState || projectUploadBusy || scopedProjectState.syncState !== 'idle'
                    || Boolean(projectCheckpoint && !checkpointMatchesSelection)
                    || (scopedProjectState.syncRecovery?.state === 'unknown' && projectCheckpoint?.sessionState !== 'completed')}
                  onClick={() => void startProjectUpload(scopedProjectSelection, confirmedProjectName, projectCheckpoint?.sessionState === 'completed')}>
                  {projectUploadBusy ? '正在处理…' : projectCheckpoint?.sessionState === 'completed' && checkpointMatchesSelection ? '核对电脑应用状态' : checkpointMatchesSelection ? '恢复同步' : '开始同步'}
                </button>}
              </div>
            </>}
          </div>}
        </>}
      </>}
      <p className="project-sync-status" role="status" aria-live="polite">{projectUploadMessage || (scopedProjectState?.syncState === 'running' ? '同步进行中…'
        : scopedProjectState?.syncState === 'shutdown_unknown' ? '同步状态未知，恢复点已保留。'
          : projectCheckpoint ? '存在待处理的同步恢复点。' : '仅在点击同步后更新云端；云端变更不会下载到本机。')}</p>
      <LocalWorkspaceRecovery key={user?.id} bridge={projectBridge} workspaceNames={Object.fromEntries((query.data?.items ?? []).map((item) => [item.id, item.name]))} />
    </section>
    <section className="design-panel" aria-labelledby="workspace-list-title">
      <div className="design-panel-header"><div><h2 id="workspace-list-title">{computer ? `${computer.name || '云端电脑'} · 文件与版本` : '云端文件与版本'}</h2><p>{computer ? '查看这台电脑已保存的文件和历史版本。' : '查看云端文件和历史版本。'}</p></div>{!computerID && !recoveryPending && !recoveryError && <Link className="button button-primary" to="/hosts/new">＋ 创建云端电脑</Link>}</div>
    {!computerID && (recoveryPending || recoveryError) && <form className="form-card form-stack workspace-create-form" aria-label="恢复历史文件空间创建" onSubmit={createWorkspace}>
      <h2>恢复历史文件创建请求</h2>
      {create.isError && <FloatingError className="error-inline" role="alert">创建失败：{errorDetails(create.error).message}</FloatingError>}
      {validationError && <FloatingError>{validationError}</FloatingError>}
      {recoveryError && <FloatingError>无法读取本机上次创建记录。为避免重复创建，请先在工作区列表核实结果，再选择清除记录。<button type="button" className="button button-secondary" onClick={abandonRecovery}>我已核实，清除记录</button></FloatingError>}
      {recoveryPending && !create.isPending && !recoveryError && <div className="notice notice-warning" role="status">上次历史文件创建结果尚未确认。继续会使用相同请求编号；若已在列表核实结果，可清除这条旧请求记录。<button type="button" className="button button-secondary" onClick={abandonRecovery}>我已核实，清除旧请求</button></div>}
      <label htmlFor="workspace-name">历史文件名称</label>
      <input id="workspace-name" required maxLength={100} value={name} disabled={create.isPending || recoveryPending || recoveryError} onChange={(event) => { setName(event.target.value); create.reset(); setValidationError(''); }} placeholder="例如：产品项目" />
      <div className="form-actions"><button type="submit" className="button button-primary" disabled={create.isPending || recoveryError}>{create.isPending ? '正在创建…' : recoveryPending ? '继续上次创建' : '创建工作区'}</button></div>
    </form>}
    {query.isPending ? <div className="loading-card">正在加载云端文件…</div> : query.isError ? <FloatingError className="error-card" role="alert">{errorDetails(query.error).message}<button className="button button-secondary" type="button" onClick={() => void query.refetch()}>重试</button></FloatingError> : !visibleWorkspaces.length ? <div className="empty-card"><h2>{computer ? '这台电脑暂无关联文件' : '还没有云端文件'}</h2><p>{computer ? '旧电脑的数据可以从电脑管理入口核对后再关联。' : '创建云端电脑后，文件会与电脑一同管理。'}</p></div> : <ScreenPagination
      items={visibleWorkspaces}
      getKey={(item) => item.id}
      estimatedItemHeight={270}
      height="min(max(calc(100dvh - clamp(320px, 50dvh, 450px)), 36px), 620px)"
      gap={10}
      ariaLabel="云端工作区"
      className="schedule-list workspace-list"
      renderItem={(item) => {
        const bindings = hosts.data?.items.flatMap((host) => host.workspaces
          .filter((binding) => binding.workspace.id === item.id && binding.state !== 'detached')
          .map((binding) => ({ host, binding }))) ?? [];
        const availableHosts = ownedRunningHosts.filter((host) => host.workspaces.every((binding) => binding.state === 'detached'));
        const selectedHostID = hostTargets[item.id] ?? (availableHosts.length === 1 ? availableHosts[0]?.id ?? '' : '');
        const selectedHost = availableHosts.find((host) => host.id === selectedHostID);
        const canDelete = !hosts.isPending && !hosts.isFetching && !hosts.isError && item.state === 'ready' && bindings.length === 0;
        return <article className="schedule-card workspace-schedule-card" aria-label={`工作区 ${item.name || item.id}`}>
          <div className="schedule-card-heading">
            <div><h3>{item.name || item.id}</h3><small className="table-sub mono">{item.id}</small></div>
            <span className={`status status-${item.state}`}>{workspaceStateLabel[item.state]}</span>
          </div>
          <dl className="schedule-facts">
            <div><dt>已用 / 配额</dt><dd>{formatBytes(item.used_bytes)} / {formatBytes(item.quota_bytes)}</dd></div>
            <div><dt>版本</dt><dd>v{item.revision}{item.revision === 0 && <small className="table-sub">初始版本，暂无历史版本</small>}</dd></div>
            <div><dt>绑定主机</dt><dd>{bindings.length ? bindings.map(({ host }) => <small className="table-sub" key={`${host.id}-${item.id}`}>{host.name || host.id}{host.owner_id === user?.id ? ` · ${hostStateLabel[host.state] || host.state}` : ' · 无权打开'}</small>) : hosts.isError ? '主机绑定状态暂不可用' : hosts.isPending ? '正在读取…' : '未绑定'}</dd></div>
            <div><dt>电脑状态</dt><dd>{bindings.length ? bindings.map(({ host, binding }) => {
            const conflicts = binding.sync_conflicts;
            const waitingForHost = host.owner_id === user?.id && host.state === 'creating' && binding.state === 'preparing' && binding.sync_state !== 'conflict';
            return <div className="workspace-host-status" key={`${host.id}-${binding.workspace.id}`}>
              {waitingForHost ? <small>{host.state_reason === 'restore_unknown' ? '电脑恢复状态待核实，工作区尚未挂载。' : '电脑创建中，工作区等待挂载。'}</small> : <>
              <small>{host.owner_id !== user?.id ? '主机应用状态不可见' : `应用：${bindingStateLabel[binding.state] || binding.state}`}</small>
              <small>同步：{workspaceSyncLabel(item, host, binding)}</small>
            {binding.sync_state === 'conflict' && <div className="workspace-host-sync-conflict notice notice-warning" role="status">
                <strong>云端电脑文件冲突</strong>
                {conflicts.slice(0, 5).map((path, index) => <small key={`${path}-${index}`}>{path}</small>)}
                {conflicts.length > 5 && <small>另有 {conflicts.length - 5} 个冲突路径未展开</small>}
                {host.owner_id === user?.id && <button type="button" className="button button-secondary button-small" aria-label={`重试电脑文件同步 ${item.name || item.id} 到 ${host.name || host.id}`} disabled={retryHostSync.isPending} onClick={() => retryHostSync.mutate({ host, binding })}>{retryHostSync.isPending ? '正在重试…' : '重试同步'}</button>}
              </div>}
              <small>{host.owner_id !== user?.id ? '项目：状态不可见' : `项目：${workspaceProjectLabel(item, host, binding)}`}</small>
              </>}
            </div>;
          }) : '—'}</dd></div>
            <div><dt>更新时间</dt><dd>{formatDate(item.updated_at)}</dd></div>
          </dl>
          <div className="inline-actions schedule-actions">
            {!computerID && <button type="button" className="button button-secondary button-small" aria-label={`查看设备同步进度 ${item.name || item.id}`} onClick={() => {
              if (progressWorkspaceID === item.id) void deviceProgress.refetch();
              else setProgressWorkspaceID(item.id);
            }}>设备进度</button>}
            {item.state === 'ready' && <button type="button" className="button button-secondary button-small" aria-label={`比较云端文件版本 ${item.name || item.id}`} disabled={revisionCompareLoading || item.revision === 0} title={item.revision === 0 ? '当前只有初始版本，暂无可比较的历史版本' : undefined} onClick={() => openRevisionDiff(item)}>版本差异</button>}
            {!computerID && item.state === 'ready' && <button type="button" className="button button-secondary button-small" aria-label={`预演复制与移交 ${item.name || item.id}`} onClick={() => setTransferWorkspaceID(item.id)}>复制/移交演示</button>}
            {bindings.filter(({ host }) => host.owner_id === user?.id).map(({ host, binding }) => <button type="button" className="button button-secondary button-small" key={host.id} aria-label={`打开云端电脑 ${host.name || host.id}`} disabled={!canOpenWorkspaceHost(item, host, binding)} title={!canOpenWorkspaceHost(item, host, binding) ? '云端电脑运行环境在线且当前文件版本已应用后才能打开' : undefined} onClick={() => void openHost(host)}>打开电脑</button>)}
            {!computerID && bindings.filter(({ host, binding }) => host.owner_id === user?.id && host.state === 'running' && binding.state !== 'unbinding').map(({ host, binding }) => <button type="button" className="button button-secondary button-small" key={`unbind-${host.id}`} aria-label={`解除云端文件与电脑 ${host.name || host.id} 的关联`} disabled={unbind.isPending} onClick={() => setSelectedBinding({ host, binding })}>解除关联</button>)}
            {!computerID && item.state === 'ready' && <div className="workspace-bind-actions">
              {hosts.isPending ? <small className="muted">正在读取主机…</small> : hosts.isError ? <small className="muted">主机状态不可用，暂不能绑定</small> : availableHosts.length ? <>
                <label className="sr-only" htmlFor={`workspace-host-${item.id}`}>选择绑定主机</label>
                <select id={`workspace-host-${item.id}`} aria-label={`工作区 ${item.name || item.id} 的绑定主机`} value={selectedHostID} onChange={(event) => setHostTargets((current) => ({ ...current, [item.id]: event.target.value }))}>
                  <option value="">选择运行中的主机</option>{availableHosts.map((host) => <option value={host.id} key={host.id}>{host.name || host.id}</option>)}
                </select>
                <button type="button" className="button button-secondary button-small" disabled={!selectedHost || bind.isPending} onClick={() => selectedHost && bind.mutate({ host: selectedHost, workspace: item })}>{bind.isPending && bind.variables?.workspace.id === item.id ? '绑定中…' : '绑定'}</button>
              </> : <small className="muted">暂无未绑定工作区的运行中主机</small>}
            </div>}
            {!computerID && <button type="button" className="button button-danger-quiet button-small" aria-label={`删除工作区 ${item.name || item.id}`} disabled={!canDelete || remove.isPending} title={!canDelete ? '文件仍有关联电脑，或状态尚未确认' : undefined} onClick={() => setSelected(item)}>删除</button>}
          </div>
        </article>;
      }} />}
    </section>
    {revisionWorkspace && <section className="form-card form-stack" aria-label="历史 revision 差异">
      <div className="host-card-heading"><h2>历史 revision 差异 · {revisionWorkspace.name || revisionWorkspace.id}</h2><button type="button" className="button button-secondary button-small" aria-label="关闭历史 revision 差异" onClick={closeRevisionDiff}>关闭</button></div>
      <p className="muted">差异由服务端按已提交版本计算。小型文本可展开查看；二进制或过大的文件只显示元数据。恢复会创建新 revision，不改写历史。</p>
      <form className="form-stack" onSubmit={(event) => void compareRevisionDiff(event)}>
        <div className="form-row">
          <div><label htmlFor="workspace-revision-from">起始 revision</label><input id="workspace-revision-from" type="text" inputMode="numeric" value={revisionFromInput} disabled={revisionCompareLoading} onChange={(event) => { setRevisionFromInput(event.target.value); setRevisionCompareError(''); setRevisionDiff(null); }} /></div>
          <div><label htmlFor="workspace-revision-to">目标 revision</label><input id="workspace-revision-to" type="text" inputMode="numeric" value={revisionToInput} disabled={revisionCompareLoading} onChange={(event) => { setRevisionToInput(event.target.value); setRevisionCompareError(''); setRevisionDiff(null); }} /></div>
        </div>
        <div className="form-actions"><button type="submit" className="button button-primary" disabled={revisionCompareLoading}>{revisionCompareLoading ? '正在读取差异…' : '比较版本'}</button></div>
      </form>
      {revisionCompareError && <FloatingError>{revisionCompareError}</FloatingError>}
      {revisionCompareLoading && <p role="status">正在读取服务端版本差异…</p>}
      {revisionDiff && <div aria-label="历史 revision 差异结果">
        <h3>v{revisionDiff.from_revision} → v{revisionDiff.to_revision}</h3>
        <p role="status">共 {revisionDiff.changes.length} 个路径差异；按路径分页显示。</p>
        {revisionDiff.changes.length === 0 ? <p>这两个 revision 的路径和文件元数据相同。</p> : <>
          <div className="table-wrap"><table><thead><tr><th>变化</th><th>路径</th><th>起始版本</th><th>目标版本</th><th>内容状态</th></tr></thead><tbody>
            {revisionDiff.changes.slice(revisionDiffPage * revisionDiffPageSize, (revisionDiffPage + 1) * revisionDiffPageSize).map((entry) => <tr key={`${entry.path}-${entry.action}`}>
              <td>{workspaceDiffActionLabel[entry.action]}</td>
              <td className="mono">{entry.path}</td>
              <td>{workspaceDiffVersionSummary(entry.before)}</td>
              <td>{workspaceDiffVersionSummary(entry.after)}</td>
              <td>{workspaceDiffDisplayLabel[entry.display_status]}
                {entry.display_status === 'text' && (entry.before_text !== null || entry.after_text !== null) && <details>
                  <summary>展开文本</summary>
                  {entry.before_text !== null && <><strong>起始版本</strong><pre>{entry.before_text}</pre></>}
                  {entry.after_text !== null && <><strong>目标版本</strong><pre>{entry.after_text}</pre></>}
                </details>}
              </td>
            </tr>)}
          </tbody></table></div>
          {revisionDiff.changes.length > revisionDiffPageSize && <nav className="form-actions" aria-label="历史差异分页">
            <button type="button" className="button button-secondary button-small" disabled={revisionDiffPage === 0} onClick={() => setRevisionDiffPage((page) => Math.max(0, page - 1))}>上一页差异</button>
            <span>第 {revisionDiffPage + 1} / {Math.ceil(revisionDiff.changes.length / revisionDiffPageSize)} 页</span>
            <button type="button" className="button button-secondary button-small" disabled={(revisionDiffPage + 1) * revisionDiffPageSize >= revisionDiff.changes.length} onClick={() => setRevisionDiffPage((page) => page + 1)}>下一页差异</button>
          </nav>}
          {!canRestoreRevision && restoreBlockedReason && <p className="muted" role="status">{restoreBlockedReason}</p>}
          <button type="button" className="button button-danger" disabled={!canRestoreRevision || restoreWorkflowBusy}
            onClick={() => {
              setWritersStoppedConfirmed(false);
              setRestoreWorkflowError('');
              setRestoreConfirmationOpen(true);
            }}>
            {revisionBoundHost ? '安全恢复并重启 Host' : '从 v' + String(revisionDiff.from_revision) + ' 恢复 ' + String(workspaceDiffRestorePaths(revisionDiff.changes).length) + ' 个路径'}
          </button>
        </>}
      </div>}
    </section>}
    {restoreConfirmationOpen && restoreRecovery.pending && <ConfirmDialog
      title="继续云端文件安全恢复"
      description={'流程当前停在“' + restorePhaseLabel[restoreRecovery.pending.phase] + '”。继续前请确认外部写入者仍已停止。系统会先核对服务端状态，只有确认前置步骤后才执行下一步。'}
      confirmLabel="核对状态并继续"
      danger
      busy={restoreWorkflowBusy}
      confirmDisabled={!writersStoppedConfirmed}
      onClose={() => setRestoreConfirmationOpen(false)}
      onConfirm={confirmWorkspaceRestore}
    >
      <label className="checkbox-row">
        <input type="checkbox" checked={writersStoppedConfirmed} onChange={(event) => setWritersStoppedConfirmed(event.target.checked)} />
        我已关闭可能修改这些云端文件的 AI 工具、IDE、PTY 和其他应用，并会在恢复流程完成前保持关闭。
      </label>
      {restoreWorkflowError && <FloatingError>{restoreWorkflowError}</FloatingError>}
    </ConfirmDialog>}
    {restoreConfirmationOpen && !restoreRecovery.pending && revisionWorkspace && revisionDiff && <ConfirmDialog
      title="恢复云端文件版本"
      description={`将从 v${revisionDiff.from_revision} 恢复 ${workspaceDiffRestorePaths(revisionDiff.changes).length} 个路径，并以当前 v${revisionWorkspace.revision} 为条件创建新 revision。服务端会在主机解绑且没有在途写入时执行。`}
      confirmLabel={revisionBoundHost ? '解除关联、恢复文件并重启电脑' : '恢复文件并创建新版本'}
      danger
      busy={restoreWorkflowBusy}
      confirmDisabled={!writersStoppedConfirmed}
      onClose={() => { setRestoreConfirmationOpen(false); setWritersStoppedConfirmed(false); }}
      onConfirm={confirmWorkspaceRestore}
    >
      <p>系统不能检测所有外部写入者。电脑流程会先解除关联并等待服务端确认，再释放计算资源、恢复云端文件、重新关联并恢复电脑；释放时可能终止终端进程。任一步状态未知都会暂停，不会继续。</p>
      <label className="checkbox-row">
        <input type="checkbox" checked={writersStoppedConfirmed} onChange={(event) => setWritersStoppedConfirmed(event.target.checked)} />
        我已关闭可能修改这些云端文件的 AI 工具、IDE、PTY 和其他应用，并会在恢复流程完成前保持关闭。
      </label>
      <details><summary>查看本次恢复的路径</summary><ul>{workspaceDiffRestorePaths(revisionDiff.changes).map((path) => <li className="mono" key={path}>{path}</li>)}</ul></details>
      {restoreWorkflowError && <FloatingError>{restoreWorkflowError}</FloatingError>}
    </ConfirmDialog>}
    {transferWorkspace && <section className="form-card form-stack" aria-label="Workspace 复制与移交演示">
      <div className="host-card-heading">
        <h2>复制与移交演示 · {transferWorkspace.name || transferWorkspace.id}</h2>
        <button type="button" className="button button-secondary button-small" aria-label="关闭复制与移交演示" onClick={() => setTransferWorkspaceID('')}>关闭</button>
      </div>
      <WorkspaceTransferPreview source={{
        id: transferWorkspace.id,
        name: transferWorkspace.name,
        revision: transferWorkspace.revision,
        usedBytes: transferWorkspace.used_bytes,
        runtimeState: transferHost?.state === 'stopped' || !transferHost ? 'stopped' : 'running',
        checkpointReady: transferCheckpointReady,
        snapshotType: 'workspace',
      }} ownerLabel={user?.name ?? '当前所有者'} />
    </section>}
    {progressWorkspaceID && <section className="notice notice-info" aria-live="polite">
      <div className="host-card-heading"><h2>设备同步进度 · {visibleWorkspaces.find((item) => item.id === progressWorkspaceID)?.name || progressWorkspaceID}</h2><button type="button" className="button button-secondary button-small" aria-label="关闭设备同步进度" onClick={() => setProgressWorkspaceID('')}>关闭</button></div>
      {deviceProgress.isPending ? <p>正在读取同步状态…</p> : deviceProgress.isError ? <FloatingError className="error-inline" role="alert">读取失败：{errorDetails(deviceProgress.error).message}<button type="button" className="button button-secondary button-small" onClick={() => void deviceProgress.refetch()}>重试</button></FloatingError> : !deviceProgress.data.items.length ? <p>当前没有设备正在同步。设备连接后，仍可能有待同步的文件。</p> : <ScreenPagination
        items={deviceProgress.data.items}
        getKey={(item) => item.session_id}
        estimatedItemHeight={82}
        height="min(30dvh, 240px)"
        ariaLabel="设备同步进度"
        renderItem={(item) => <div className="workspace-host-status">
        <strong>设备 {item.device_id} · {syncDirectionLabel[item.direction]} · {syncStateLabel[item.state]}</strong>
        <small>阶段：{syncStageLabel[item.stage]}</small>
        <small>已确认传输：{formatBytes(item.confirmed_bytes)} / {formatBytes(item.planned_bytes)}</small>
        {item.direction !== 'upload' && <small>已应用文件动作：{item.download_actions_done} / {item.download_actions_total}</small>}
        <small>最近更新：{formatDate(item.updated_at)}</small>
        </div>}
      />}
    </section>}
    {hosts.isError && <FloatingError className="error-inline" role="alert">无法读取主机绑定状态，工作区删除和绑定操作已停用：{errorDetails(hosts.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void hosts.refetch()}>重试</button></FloatingError>}
    {selectedBinding && <ConfirmDialog title={`解绑工作区与主机 ${selectedBinding.host.name || selectedBinding.host.id}`} description="Morde 无法自动检测外部写入者。解绑前请先关闭会写入该工作区的 AI、IDE、终端任务和其他应用；继续即表示确认这些写入者均已停止。服务端会等待进行中的操作结束并校验稳定快照。解绑不会删除服务端工作区或主机磁盘副本。" confirmLabel="已停止写入者，确认解绑" danger busy={unbind.isPending} onClose={() => setSelectedBinding(null)} onConfirm={() => unbind.mutate(selectedBinding)} />}
    {selected && <ConfirmDialog title="删除工作区" description={`确定删除“${selected.name || selected.id}”吗？这会永久删除 Morde 云端工作区及其已同步数据。本地文件夹和未同步的文件会保留。`} confirmLabel="删除工作区" danger busy={remove.isPending} onClose={() => setSelected(null)} onConfirm={() => remove.mutate(selected.id)} />}
  </section>;
}

const syncDirectionLabel: Record<components['schemas']['DeviceWorkspaceSyncProgress']['direction'], string> = {
  upload: '上传', download: '下载', bidirectional: '双向同步',
};
const syncStateLabel: Record<components['schemas']['DeviceWorkspaceSyncProgress']['state'], string> = {
  planned: '准备中', transferring: '传输中', verifying: '校验中', applying: '应用中',
};
const syncStageLabel: Record<components['schemas']['DeviceWorkspaceSyncProgress']['stage'], string> = {
  scan: '扫描', compare: '比较', transfer: '传输', verify: '校验', apply: '应用', complete: '完成',
};

function workspaceSyncLabel(workspace: Workspace, host: Host, binding: Binding) {
  if (binding.sync_state === 'conflict') return '检测到冲突';
  if (binding.state === 'failed') return '同步失败';
  if (binding.state === 'detached') return '未绑定';
  if (host.workspace_runtime_online !== true) {
    if (binding.state === 'preparing') return `电脑运行环境未在线确认 · 等待应用 v${binding.desired_revision}`;
    if (binding.state === 'unbinding') return '电脑运行环境未在线确认 · 等待解除关联';
    return binding.applied_revision >= 0
      ? `电脑运行环境未在线确认 · 最近确认 v${binding.applied_revision}`
      : '电脑运行环境未在线确认 · 尚无应用确认';
  }
  if (binding.state === 'preparing') return `正在应用 v${binding.desired_revision}`;
  if (binding.state === 'unbinding') return '正在解绑';
  if (binding.applied_revision !== binding.desired_revision) return `同步中 v${binding.applied_revision}/${binding.desired_revision}`;
  if (binding.applied_revision !== workspace.revision) return `待应用：电脑 v${binding.applied_revision} / 文件 v${workspace.revision}`;
  return `已同步 v${binding.applied_revision}`;
}

function canOpenWorkspaceHost(workspace: Workspace, host: Host, binding: Binding) {
  return host.state === 'running' && host.workspace_runtime_online === true && binding.state === 'active'
    && binding.applied_revision === binding.desired_revision
    && binding.applied_revision === workspace.revision;
}

function workspaceProjectLabel(workspace: Workspace, host: Host, binding: Binding) {
  if (canOpenWorkspaceHost(workspace, host, binding)) return '文件已就绪';
  if (binding.state === 'preparing') return '等待电脑应用文件';
  if (binding.state === 'failed') return '电脑应用文件失败';
  if (binding.state === 'unbinding') return '正在解绑';
  if (host.state !== 'running') return '等待电脑运行';
  return '等待电脑应用文件';
}
