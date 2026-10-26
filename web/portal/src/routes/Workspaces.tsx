import { useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import type { components } from '../api/schema';
import { enterHostInNewTab, errorDetails, newIdempotencyKey, portalApi } from '../app/api';
import { useAuth } from '../app/auth';
import { formatBytes, formatDate, hostStateLabel } from '../app/format';
import { useToast } from '../app/toast';
import { ConfirmDialog } from '../components/ConfirmDialog';

type Workspace = Awaited<ReturnType<typeof portalApi.workspaces>>['items'][number];
type Host = Awaited<ReturnType<typeof portalApi.hosts>>['items'][number];
type Binding = Host['workspaces'][number];
type PendingWorkspaceCreate = { version: 1; name: string; idempotencyKey: string };
const workspaceStateLabel: Record<Workspace['state'], string> = { ready: '服务端已保存', deleting: '删除中', deleted: '已删除' };
const bindingStateLabel: Record<Binding['state'], string> = {
  preparing: '正在应用', active: '已应用', unbinding: '正在解绑', detached: '未绑定', failed: '应用失败',
};

function pendingCreateKey(userID: string) {
  return `morde.pending-workspace-create.${userID}`;
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
  const client = useQueryClient();
  const { user } = useAuth();
  const { notify } = useToast();
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
  const hosts = useQuery({
    queryKey: ['hosts'], queryFn: portalApi.hosts,
    refetchInterval: (query) => query.state.data?.items.some((host) => host.state === 'creating' || host.state === 'deleting' || host.state === 'stopping'
      || host.workspaces.some((binding) => binding.state === 'preparing' || binding.state === 'active' || binding.state === 'unbinding')) ? 3000 : false,
  });
  const query = useQuery({
    queryKey: ['workspaces'], queryFn: portalApi.workspaces,
    refetchInterval: (current) => current.state.data?.items.some((workspace) => workspace.state === 'deleting')
      || hosts.data?.items.some((host) => host.state === 'creating' || host.state === 'deleting' || host.state === 'stopping'
        || host.workspaces.some((binding) => binding.state === 'preparing' || binding.state === 'unbinding'
          || (binding.state === 'active' && binding.applied_revision !== binding.desired_revision))) ? 3000 : false,
  });
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
      if (binding.state === 'failed') notify('服务端报告工作区未能应用到主机，请检查绑定状态。', 'error');
      else if (binding.state === 'active' && binding.applied_revision === binding.desired_revision) notify('服务端已确认工作区应用到主机。', 'success');
      else notify('服务端已接受绑定，正在等待主机应用工作区。', 'success');
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
      notify(binding.state === 'unbinding' ? '已提交解绑，服务端正在等待操作完成并校验工作区。' : '工作区已解绑，服务端数据和主机磁盘副本均已保留。', 'success');
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
      notify('已提交 Host 工作区同步重试。', 'success');
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

  return <section>
    <div className="page-heading"><div><p className="eyebrow">资源管理</p><h1>我的工作区</h1><p className="muted">工作区独立保存在服务端。删除主机只会解绑工作区，不会删除其中的数据。</p></div></div>
    <form className="form-card form-stack workspace-create-form" aria-label="创建独立工作区" onSubmit={createWorkspace}>
      <h2>创建工作区</h2>
      {create.isError && <p className="form-error" role="alert">创建失败：{errorDetails(create.error).message}</p>}
      {validationError && <p className="form-error" role="alert">{validationError}</p>}
      {recoveryError && <div className="notice notice-warning" role="alert">无法读取本机上次创建记录。为避免重复创建，请先在工作区列表核实结果，再选择清除记录。<button type="button" className="button button-secondary" onClick={abandonRecovery}>我已核实，清除记录</button></div>}
      {recoveryPending && !create.isPending && !recoveryError && <div className="notice notice-warning" role="status">上次创建结果尚未确认。继续会使用相同请求编号；若已在工作区列表确认结果，可以放弃恢复并重新填写。<button type="button" className="button button-secondary" onClick={abandonRecovery}>我已核实，放弃恢复并重新填写</button></div>}
      <label htmlFor="workspace-name">工作区名称</label>
      <input id="workspace-name" required maxLength={100} value={name} disabled={create.isPending || recoveryPending || recoveryError} onChange={(event) => { setName(event.target.value); create.reset(); setValidationError(''); }} placeholder="例如：产品项目" />
      <div className="form-actions"><button type="submit" className="button button-primary" disabled={create.isPending || recoveryError}>{create.isPending ? '正在创建…' : recoveryPending ? '继续上次创建' : '创建工作区'}</button></div>
    </form>
    {query.isPending ? <div className="loading-card">正在加载工作区…</div> : query.isError ? <div className="error-card" role="alert">{errorDetails(query.error).message}<button className="button button-secondary" type="button" onClick={() => void query.refetch()}>重试</button></div> : !query.data?.items.length ? <div className="empty-card"><h2>还没有工作区</h2><p>也可以在创建主机时新建工作区，或选择已有工作区。</p></div> : <div className="table-wrap"><table><thead><tr><th>名称</th><th>状态</th><th>已用 / 配额</th><th>版本</th><th>绑定主机</th><th>Host 应用 / 同步状态</th><th>更新时间</th><th>操作</th></tr></thead><tbody>
      {query.data.items.map((item) => {
        const bindings = hosts.data?.items.flatMap((host) => host.workspaces
          .filter((binding) => binding.workspace.id === item.id && binding.state !== 'detached')
          .map((binding) => ({ host, binding }))) ?? [];
        const availableHosts = ownedRunningHosts.filter((host) => !bindings.some(({ host: boundHost }) => boundHost.id === host.id));
        const selectedHostID = hostTargets[item.id] ?? (availableHosts.length === 1 ? availableHosts[0]?.id ?? '' : '');
        const selectedHost = availableHosts.find((host) => host.id === selectedHostID);
        const canDelete = !hosts.isPending && !hosts.isFetching && !hosts.isError && item.state === 'ready' && bindings.length === 0;
        return <tr key={item.id}>
          <td><strong>{item.name || item.id}</strong><small className="table-sub mono">{item.id}</small></td>
          <td><span className={`status status-${item.state}`}>{workspaceStateLabel[item.state]}</span></td>
          <td>{formatBytes(item.used_bytes)} / {formatBytes(item.quota_bytes)}</td>
          <td>v{item.revision}</td>
          <td>{bindings.length ? bindings.map(({ host, binding }) => <small className="table-sub" key={`${host.id}-${binding.workspace.id}`}>{host.name || host.id}{host.owner_id === user?.id ? ` · ${hostStateLabel[host.state] || host.state}` : ' · 无权打开'}</small>) : hosts.isError ? '主机绑定状态暂不可用' : hosts.isPending ? '正在读取…' : '未绑定'}</td>
          <td>{bindings.length ? bindings.map(({ host, binding }) => {
            const conflicts = binding.sync_conflicts;
            return <div className="workspace-host-status" key={`${host.id}-${binding.workspace.id}`}>
              <small>{host.owner_id !== user?.id ? '主机应用状态不可见' : `应用：${bindingStateLabel[binding.state] || binding.state}`}</small>
              <small>同步：{workspaceSyncLabel(item, binding)}</small>
              {binding.sync_state === 'conflict' && <div className="workspace-host-sync-conflict notice notice-warning" role="status">
                <strong>Host 工作区同步冲突</strong>
                {conflicts.slice(0, 5).map((path, index) => <small key={`${path}-${index}`}>{path}</small>)}
                {conflicts.length > 5 && <small>另有 {conflicts.length - 5} 个冲突路径未展开</small>}
                {host.owner_id === user?.id && <button type="button" className="button button-secondary button-small" aria-label={`重试 Host 同步 ${item.name || item.id} 到 ${host.name || host.id}`} disabled={retryHostSync.isPending} onClick={() => retryHostSync.mutate({ host, binding })}>{retryHostSync.isPending ? '正在重试…' : '重试同步'}</button>}
              </div>}
              <small>{host.owner_id !== user?.id ? '项目：状态不可见' : `项目：${workspaceProjectLabel(item, host, binding)}`}</small>
            </div>;
          }) : '—'}</td>
          <td>{formatDate(item.updated_at)}</td>
          <td><div className="workspace-actions">
            <button type="button" className="button button-secondary button-small" aria-label={`查看设备同步进度 ${item.name || item.id}`} onClick={() => {
              if (progressWorkspaceID === item.id) void deviceProgress.refetch();
              else setProgressWorkspaceID(item.id);
            }}>设备进度</button>
            {bindings.filter(({ host }) => host.owner_id === user?.id).map(({ host, binding }) => <button type="button" className="button button-secondary button-small" key={host.id} aria-label={`打开主机 ${host.name || host.id}`} disabled={!canOpenWorkspaceHost(item, host, binding)} title={!canOpenWorkspaceHost(item, host, binding) ? '主机运行且工作区当前版本已应用后才能打开' : undefined} onClick={() => void openHost(host)}>打开主机</button>)}
            {bindings.filter(({ host, binding }) => host.owner_id === user?.id && host.state === 'running' && binding.state !== 'unbinding').map(({ host, binding }) => <button type="button" className="button button-secondary button-small" key={`unbind-${host.id}`} aria-label={`解绑工作区 ${item.name || item.id} 与主机 ${host.name || host.id}`} disabled={unbind.isPending} onClick={() => setSelectedBinding({ host, binding })}>解绑</button>)}
            {item.state === 'ready' && <div className="workspace-bind-actions">
              {hosts.isPending ? <small className="muted">正在读取主机…</small> : hosts.isError ? <small className="muted">主机状态不可用，暂不能绑定</small> : availableHosts.length ? <>
                <label className="sr-only" htmlFor={`workspace-host-${item.id}`}>选择绑定主机</label>
                <select id={`workspace-host-${item.id}`} aria-label={`工作区 ${item.name || item.id} 的绑定主机`} value={selectedHostID} onChange={(event) => setHostTargets((current) => ({ ...current, [item.id]: event.target.value }))}>
                  <option value="">选择运行中的主机</option>{availableHosts.map((host) => <option value={host.id} key={host.id}>{host.name || host.id}</option>)}
                </select>
                <button type="button" className="button button-secondary button-small" disabled={!selectedHost || bind.isPending} onClick={() => selectedHost && bind.mutate({ host: selectedHost, workspace: item })}>{bind.isPending && bind.variables?.workspace.id === item.id ? '绑定中…' : '绑定'}</button>
              </> : <small className="muted">暂无可绑定的运行中主机</small>}
            </div>}
            <button type="button" className="button button-danger-quiet button-small" aria-label={`删除工作区 ${item.name || item.id}`} disabled={!canDelete || remove.isPending} title={!canDelete ? '工作区仍有主机绑定，或绑定状态尚未确认' : undefined} onClick={() => setSelected(item)}>删除</button>
          </div></td>
        </tr>;
      })}
    </tbody></table></div>}
    {progressWorkspaceID && <section className="notice notice-info" aria-live="polite">
      <div className="host-card-heading"><h2>设备同步进度 · {query.data?.items.find((item) => item.id === progressWorkspaceID)?.name || progressWorkspaceID}</h2><button type="button" className="button button-secondary button-small" aria-label="关闭设备同步进度" onClick={() => setProgressWorkspaceID('')}>关闭</button></div>
      {deviceProgress.isPending ? <p>正在读取同步状态…</p> : deviceProgress.isError ? <p role="alert">读取失败：{errorDetails(deviceProgress.error).message} <button type="button" className="button button-secondary button-small" onClick={() => void deviceProgress.refetch()}>重试</button></p> : !deviceProgress.data.items.length ? <p>当前没有活动的设备同步会话；这不代表两端没有待同步差异。</p> : deviceProgress.data.items.map((item) => <div className="workspace-host-status" key={item.session_id}>
        <strong>设备 {item.device_id} · {syncDirectionLabel[item.direction]} · {syncStateLabel[item.state]}</strong>
        <small>阶段：{syncStageLabel[item.stage]}</small>
        <small>已确认传输：{formatBytes(item.confirmed_bytes)} / {formatBytes(item.planned_bytes)}</small>
        {item.direction !== 'upload' && <small>已应用文件动作：{item.download_actions_done} / {item.download_actions_total}</small>}
        <small>最近更新：{formatDate(item.updated_at)}</small>
      </div>)}
    </section>}
    {hosts.isError && <p className="notice notice-info" role="status">无法读取主机绑定状态，工作区删除和绑定操作已停用。{errorDetails(hosts.error).message}</p>}
    <p className="muted">Host 应用与设备传输进度分别显示。设备进度仅包含活动会话；无活动会话不代表两端完全一致。</p>
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

function workspaceSyncLabel(workspace: Workspace, binding: Binding) {
  if (binding.sync_state === 'conflict') return '检测到冲突';
  if (binding.state === 'failed') return '同步失败';
  if (binding.state === 'preparing') return `正在应用 v${binding.desired_revision}`;
  if (binding.state === 'unbinding') return '正在解绑';
  if (binding.state === 'detached') return '未绑定';
  if (binding.applied_revision !== binding.desired_revision) return `同步中 v${binding.applied_revision}/${binding.desired_revision}`;
  if (binding.applied_revision !== workspace.revision) return `待同步：主机 v${binding.applied_revision} / 工作区 v${workspace.revision}`;
  return `已同步 v${binding.applied_revision}`;
}

function canOpenWorkspaceHost(workspace: Workspace, host: Host, binding: Binding) {
  return host.state === 'running' && binding.state === 'active'
    && binding.applied_revision === binding.desired_revision
    && binding.applied_revision === workspace.revision;
}

function workspaceProjectLabel(workspace: Workspace, host: Host, binding: Binding) {
  if (canOpenWorkspaceHost(workspace, host, binding)) return '文件已就绪';
  if (binding.state === 'preparing') return '等待主机应用';
  if (binding.state === 'failed') return '主机应用失败';
  if (binding.state === 'unbinding') return '正在解绑';
  if (host.state !== 'running') return '等待主机运行';
  return '等待文件同步';
}
