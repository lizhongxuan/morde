import { portalApi } from './api';

type HostPage = Awaited<ReturnType<typeof portalApi.hosts>>;
type Host = HostPage['items'][number];
type WorkspacePage = Awaited<ReturnType<typeof portalApi.workspaces>>;
type Workspace = WorkspacePage['items'][number];
type Binding = Host['workspaces'][number];

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

export type WorkspaceRestoreAPI = Pick<typeof portalApi,
  'hosts' | 'workspaces' | 'unbindWorkspaceFromHost' | 'releaseHost' | 'restoreWorkspacePaths' | 'bindWorkspaceToHost' | 'restoreHost'>;

type Snapshot = { host: Host | null; workspace: Workspace; binding: Binding | null };

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
  const [hosts, workspaces] = await Promise.all([api.hosts(), api.workspaces()]);
  const workspace = workspaces.items.find((item) => item.id === operation.workspaceID);
  const host = operation.hostID ? hosts.items.find((item) => item.id === operation.hostID) ?? null : null;
  if (!workspace || (operation.hostID && !host)) throw new Error('恢复对象已不存在；未继续后续步骤。');
  const binding = host?.workspaces.find((item) => item.workspace.id === operation.workspaceID) ?? null;
  return { host, workspace, binding };
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
  let operation = initial;
  const checkpoint = (next: PendingWorkspaceRestore) => { operation = next; save(next); };

  for (let step = 0; step < 40; step++) {
    if (operation.phase === 'unbind') {
      if (!operation.hostID) { checkpoint({ ...operation, phase: 'restore_workspace' }); continue; }
      const current = await snapshot(api, operation);
      if (current.binding?.state === 'detached') { checkpoint({ ...operation, phase: 'release_host' }); continue; }
      if (current.binding?.state === 'unbinding') { checkpoint({ ...operation, phase: 'wait_detached' }); continue; }
      if (!current.host || current.host.state !== 'running' || current.binding?.state !== 'active'
        || current.binding.sync_state !== 'healthy' || current.binding.applied_revision !== operation.expectedCurrentRevision
        || current.binding.desired_revision !== operation.expectedCurrentRevision || current.binding.sync_conflicts.length > 0) {
        throw new Error('Host 或 Workspace 绑定状态与恢复记录不一致；未继续解绑。');
      }
      await api.unbindWorkspaceFromHost(operation.hostID, operation.workspaceID, {
        expected_binding_version: current.binding.binding_version,
        writers_stopped: true,
      }, operation.unbindKey);
      checkpoint({ ...operation, phase: 'wait_detached' });
      continue;
    }

    if (operation.phase === 'wait_detached') {
      const result = await pollSnapshot(api, operation, (current) => {
        if (current.binding?.state === 'detached') return 'ready';
        if (current.binding?.state === 'failed' || !current.binding) throw new Error('解绑失败或绑定记录消失；未释放 Host。');
        if (current.binding.state === 'active') return 'retry';
        if (current.binding.state !== 'unbinding') throw new Error('绑定状态未知；未释放 Host。');
        return null;
      }, '等待 Workspace 确认解绑');
      checkpoint({ ...operation, phase: result.result === 'ready' ? 'release_host' : 'unbind' });
      continue;
    }

    if (operation.phase === 'release_host') {
      const current = await snapshot(api, operation);
      if (!current.host || (current.binding && current.binding.state !== 'detached')) throw new Error('Host 尚未确认 Workspace detached；未释放计算资源。');
      if (current.host.state === 'stopped') { checkpoint({ ...operation, phase: 'restore_workspace' }); continue; }
      if (current.host.state === 'stopping') { checkpoint({ ...operation, phase: 'wait_stopped' }); continue; }
      if (current.host.state !== 'running') throw new Error('Host 状态未知；未释放计算资源。');
      await api.releaseHost(current.host.id, true);
      checkpoint({ ...operation, phase: 'wait_stopped' });
      continue;
    }

    if (operation.phase === 'wait_stopped') {
      const result = await pollSnapshot(api, operation, (current) => {
        if (current.binding && current.binding.state !== 'detached') throw new Error('Host 未解绑；未恢复 Workspace。');
        if (current.host?.state === 'stopped') return 'ready';
        if (current.host?.state === 'running') return 'retry';
        if (current.host?.state !== 'stopping') throw new Error('Host 停止状态未知；未恢复 Workspace。');
        return null;
      }, '等待 Host 停止');
      checkpoint({ ...operation, phase: result.result === 'ready' ? 'restore_workspace' : 'release_host' });
      continue;
    }

    if (operation.phase === 'restore_workspace') {
      const current = await snapshot(api, operation);
      if (current.workspace.state !== 'ready' || (current.binding && current.binding.state !== 'detached')) {
        throw new Error('Workspace 未就绪或仍绑定 Host；未提交恢复。');
      }
      if (operation.hostID && current.host?.state !== 'stopped') throw new Error('Host 尚未停止；未提交 Workspace 恢复。');
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
      checkpoint({ ...operation, phase: operation.hostID ? 'bind_workspace' : 'wait_projection' });
      if (!operation.hostID && result.result === 'ready') return;
      continue;
    }

    if (operation.phase === 'bind_workspace') {
      if (!operation.hostID || operation.restoredRevision === undefined) throw new Error('恢复记录缺少 Host 或目标 revision；未继续绑定。');
      const current = await snapshot(api, operation);
      if (current.workspace.revision !== operation.restoredRevision || current.workspace.state !== 'ready' || current.host?.state !== 'stopped') {
        throw new Error('Workspace revision 或 Host 状态已变化；未继续绑定。');
      }
      if (current.binding && current.binding.state !== 'detached') {
        if (current.binding.state === 'failed') throw new Error('Host Workspace 绑定失败；可核对状态后恢复。');
        checkpoint({ ...operation, phase: 'restore_host' });
        continue;
      }
      const expectedVersion = operation.bindExpectedVersion ?? current.binding?.binding_version ?? 0;
      if (operation.bindExpectedVersion === undefined) checkpoint({ ...operation, bindExpectedVersion: expectedVersion });
      const binding = await api.bindWorkspaceToHost(operation.hostID, {
        workspace_id: operation.workspaceID,
        expected_revision: operation.restoredRevision,
        expected_binding_version: expectedVersion,
      }, operation.bindKey);
      if (binding.workspace.id !== operation.workspaceID || binding.state === 'failed') throw new Error('Host Workspace 绑定未被确认；恢复流程已暂停。');
      checkpoint({ ...operation, phase: 'restore_host' });
      continue;
    }

    if (operation.phase === 'restore_host') {
      const current = await snapshot(api, operation);
      if (!current.host || !current.binding || current.binding.state === 'detached' || current.binding.state === 'failed') {
        throw new Error('Workspace 尚未绑定到 Host；未启动 Host。');
      }
      if (current.host.state === 'running' || (current.host.state === 'creating'
        && (current.host.state_reason === 'restore_pending' || current.host.state_reason === 'restore_unknown'))) {
        checkpoint({ ...operation, phase: 'wait_running' });
        continue;
      }
      if (current.host.state !== 'stopped') throw new Error('Host 不处于可恢复状态；未继续启动。');
      await api.restoreHost(operation.hostID!);
      checkpoint({ ...operation, phase: 'wait_running' });
      continue;
    }

    if (operation.phase === 'wait_running') {
      const result = await pollSnapshot(api, operation, (current) => {
        if (!current.host || !current.binding || current.binding.state === 'detached' || current.binding.state === 'failed') {
          throw new Error('Host Workspace 绑定状态异常；恢复流程已暂停。');
        }
        if (current.host.state === 'running' && current.host.workspace_runtime_online === true
          && current.binding.state === 'active' && current.binding.sync_state === 'healthy'
          && current.binding.applied_revision === operation.restoredRevision
          && current.binding.desired_revision === operation.restoredRevision && current.binding.sync_conflicts.length === 0) return 'ready';
        if (current.host.state === 'stopped') return 'retry';
        if (current.host.state !== 'creating' && current.host.state !== 'running' && current.host.state !== 'stopping') {
          throw new Error('Host 恢复状态未知；未报告 Workspace 已就绪。');
        }
        return null;
      }, '等待 Host 和 Workspace 应用恢复 revision');
      if (result.result === 'ready') return;
      checkpoint({ ...operation, phase: 'restore_host' });
      continue;
    }
  }
  throw new Error('恢复步骤过多，流程已暂停；请重新核对服务端状态。');
}
