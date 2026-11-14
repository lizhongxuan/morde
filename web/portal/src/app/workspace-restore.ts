import { portalApi } from './api';

type WorkspacePage = Awaited<ReturnType<typeof portalApi.workspaces>>;
type Workspace = WorkspacePage['items'][number];

export type WorkspaceRestorePhase =
  | 'unbind'
  | 'wait_detached'
  | 'release_host'
  | 'wait_stopped'
  | 'restore_workspace'
  | 'wait_projection'
  | 'bind_workspace'
  | 'restore_host'
  | 'wait_running';

export type PendingWorkspaceRestore = {
  version: 1;
  ownerID: string;
  workspaceID: string;
  hostID: string | null;
  targetRevision: number;
  expectedCurrentRevision: number;
  paths: string[];
  phase: WorkspaceRestorePhase;
  unbindKey: string;
  restoreKey: string;
  bindKey: string;
  restoreRequested: boolean;
  restoredRevision?: number;
  bindExpectedVersion?: number;
};

export type WorkspaceRestoreAPI = Pick<typeof portalApi, 'workspaces' | 'restoreWorkspacePaths'>;

type Snapshot = { workspace: Workspace };

const idempotencyKeyPattern = /^[A-Za-z0-9._~-]{8,128}$/;
const restorePhases: WorkspaceRestorePhase[] = [
  'unbind', 'wait_detached', 'release_host', 'wait_stopped', 'restore_workspace',
  'wait_projection', 'bind_workspace', 'restore_host', 'wait_running',
];

export function isPendingWorkspaceRestore(value: unknown): value is PendingWorkspaceRestore {
  if (!value || typeof value !== 'object') return false;
  const item = value as Partial<PendingWorkspaceRestore>;
  return item.version === 1
    && typeof item.ownerID === 'string' && /^u[a-z0-9]{7}$/.test(item.ownerID)
    && typeof item.workspaceID === 'string' && /^w[a-z0-9]{7}$/.test(item.workspaceID)
    && (item.hostID === null || (typeof item.hostID === 'string' && /^h[a-z0-9]{7}$/.test(item.hostID)))
    && Number.isSafeInteger(item.targetRevision) && Number(item.targetRevision) >= 0
    && Number.isSafeInteger(item.expectedCurrentRevision) && Number(item.expectedCurrentRevision) > Number(item.targetRevision)
    && Array.isArray(item.paths) && item.paths.length > 0 && item.paths.length <= 1000
    && item.paths.every((path) => typeof path === 'string' && path.length > 0 && path.length <= 4096
      && !path.startsWith('/') && !path.includes('\\') && !path.split('/').some((part) => !part || part === '.' || part === '..'))
    && restorePhases.includes(item.phase as WorkspaceRestorePhase)
    && typeof item.unbindKey === 'string' && idempotencyKeyPattern.test(item.unbindKey)
    && typeof item.restoreKey === 'string' && idempotencyKeyPattern.test(item.restoreKey)
    && typeof item.bindKey === 'string' && idempotencyKeyPattern.test(item.bindKey)
    && typeof item.restoreRequested === 'boolean'
    && (item.restoredRevision === undefined || (Number.isSafeInteger(item.restoredRevision) && Number(item.restoredRevision) > Number(item.expectedCurrentRevision)))
    && (item.bindExpectedVersion === undefined || (Number.isSafeInteger(item.bindExpectedVersion) && Number(item.bindExpectedVersion) >= 0));
}

async function snapshot(api: WorkspaceRestoreAPI, operation: PendingWorkspaceRestore): Promise<Snapshot> {
  const workspaces = await api.workspaces();
  const workspace = workspaces.items.find((item) => item.id === operation.workspaceID);
  if (!workspace) throw new Error('恢复对象已不存在；未继续后续步骤。');
  return { workspace };
}

async function pollSnapshot(
  api: WorkspaceRestoreAPI,
  operation: PendingWorkspaceRestore,
  check: (value: Snapshot) => 'ready' | 'retry' | null,
  waitingMessage: string,
): Promise<{ snapshot: Snapshot; result: 'ready' | 'retry' }> {
  for (let attempt = 0; attempt < 60; attempt++) {
    const current = await snapshot(api, operation);
    const result = check(current);
    if (result) return { snapshot: current, result };
    if (attempt < 59) await new Promise((resolve) => setTimeout(resolve, 1000));
  }
  throw new Error(`${waitingMessage}；状态未确认，流程已暂停。可重新核对状态后继续。`);
}

export async function continueWorkspaceRestore(
  initial: PendingWorkspaceRestore,
  save: (operation: PendingWorkspaceRestore) => void,
  api: WorkspaceRestoreAPI = portalApi,
): Promise<void> {
  if (!isPendingWorkspaceRestore(initial)) throw new Error('本机恢复记录无效，未执行任何操作。');
  if (initial.hostID || ['wait_detached', 'release_host', 'wait_stopped', 'bind_workspace', 'restore_host', 'wait_running'].includes(initial.phase)) {
    throw new Error('旧版电脑文件恢复依赖解绑或重绑电脑存储，当前服务已停用；恢复记录已保留，请先人工核对云端电脑和文件状态。');
  }
  let operation = initial;
  const checkpoint = (next: PendingWorkspaceRestore) => { operation = next; save(next); };

  for (let step = 0; step < 40; step++) {
    if (operation.phase === 'unbind') {
      checkpoint({ ...operation, phase: 'restore_workspace' });
      continue;
    }

    if (operation.phase === 'restore_workspace') {
      const current = await snapshot(api, operation);
      if (current.workspace.state !== 'ready') throw new Error('Workspace 未就绪；未提交恢复。');
      if (!operation.restoreRequested && current.workspace.revision !== operation.expectedCurrentRevision) {
        throw new Error('Workspace 当前 revision 已变化；请重新比较版本。');
      }
      if (!operation.restoreRequested) checkpoint({ ...operation, restoreRequested: true });
      const result = await api.restoreWorkspacePaths(operation.workspaceID, {
        target_revision: operation.targetRevision,
        expected_current_revision: operation.expectedCurrentRevision,
        paths: operation.paths,
      }, operation.restoreKey);
      if (result.workspace_id !== operation.workspaceID || result.target_revision !== operation.targetRevision
        || result.expected_current_revision !== operation.expectedCurrentRevision || result.revision <= operation.expectedCurrentRevision) {
        throw new Error('服务端恢复回执与请求不一致；恢复状态待核对。');
      }
      checkpoint({ ...operation, phase: 'wait_projection', restoredRevision: result.revision });
      continue;
    }

    if (operation.phase === 'wait_projection') {
      if (operation.restoredRevision === undefined) throw new Error('恢复 revision 缺失；未继续绑定。');
      const restoredRevision = operation.restoredRevision;
      const result = await pollSnapshot(api, operation, (current) => {
        if (current.workspace.state !== 'ready') throw new Error('Workspace projection 未就绪；未继续绑定。');
        if (current.workspace.revision === restoredRevision) return 'ready';
        if (current.workspace.revision > restoredRevision) throw new Error('Workspace revision 再次变化；未继续绑定 Host。');
        return null;
      }, '等待 Workspace projection');
      if (result.result === 'ready') return;
    }
  }
  throw new Error('恢复步骤过多，流程已暂停；请重新核对服务端状态。');
}
