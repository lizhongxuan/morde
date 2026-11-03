import { useEffect, useState } from 'react';
import { Link } from 'react-router';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { errorDetails, enterHostInNewTab, newIdempotencyKey, portalApi } from '../app/api';
import type { HostMemberPermission } from '../app/api';
import type { components } from '../api/schema';
import { useAuth } from '../app/auth';
import { formatCpu, formatDate, formatMemory, hostStateLabel } from '../app/format';
import { useToast } from '../app/toast';
import { ConfirmDialog } from '../components/ConfirmDialog';
import { HostMemberManager } from '../components/HostMemberManager';

type Host = Awaited<ReturnType<typeof portalApi.hosts>>['items'][number];
type Workspace = Awaited<ReturnType<typeof portalApi.workspaces>>['items'][number];
type ControlActionRequest = components['schemas']['ControlActionRequest'];
type ControlStatus = Awaited<ReturnType<typeof portalApi.hostControl>>;
const unsettled = new Set(['creating', 'stopping', 'deleting']);
const controlStateLabels: Record<ControlStatus['control_state'], string> = {
  ai: '云端 AI 控制', taking_over: '正在接管', user: '人工控制', returning: '正在交还 AI',
};
const controlPhaseLabels: Record<components['schemas']['ControlTransition']['phase'], string> = {
  blocking: '阻止新的 AI 操作', draining: '等待在途工具停止', drained: '工具已停止，正在切换控制权', completed: '已完成',
};
const controlFailureLabels: Record<string, string> = {
  operations_pending: '仍有运行中的工具操作，接管会继续等待停止确认。',
  runtime_unavailable: '云端运行环境暂时不可用，请稍后重试。',
  runtime_quiesce_failed: '运行中的工具尚未确认停止，请重试或检查主机状态。',
  runtime_fence_mismatch: '云端控制状态发生变化，正在重新读取。',
  audit_unavailable: '操作记录服务暂时不可用。',
  task_snapshot_failed: '无法确认当前任务状态，请稍后重试。',
  transition_persistence_failed: '控制状态尚未保存，请核对状态后重试。',
  receipt_invalid: '云端停止回执未通过校验，请联系管理员。',
  runtime_control_failed: '云端运行环境未确认切换控制权，请重试。',
  invalid_action: '当前控制操作无效，请刷新主机状态。',
};
const sharingPermissionLabels: Record<HostMemberPermission, string> = {
  'desktop:view': '查看桌面',
  'desktop:control': '操作桌面',
  'terminal:control': '终端操作',
  'mcp:control': 'MCP 操作',
  'workspace:file:read': '读取工作区文件',
};
const hostReasonLabel: Record<string, string> = {
  user_release: '正在释放计算资源',
  restore_pending: '正在恢复运行环境',
  restore_unknown: '恢复结果待核实，请联系管理员',
  restore_failed: '恢复失败，可以重试',
};

export function Hosts() {
  const queryClient = useQueryClient();
  const { user } = useAuth();
  const { notify } = useToast();
  const hosts = useQuery({
    queryKey: ['hosts'], queryFn: portalApi.hosts,
    refetchInterval: (query) => query.state.data?.items.some((host) => unsettled.has(host.state)) ? 3000 : query.state.data?.items.length ? 30000 : false,
  });
  const workspaces = useQuery({
    queryKey: ['workspaces'], queryFn: portalApi.workspaces,
    enabled: hosts.data?.items.some((host) => host.owner_id === user?.id && host.state === 'stopped'
      && !host.workspaces.some((binding) => binding.state !== 'detached')) ?? false,
  });
  const atLimit = !!user && (hosts.data?.items.length ?? 0) >= user.quota.hosts;
  const [deleting, setDeleting] = useState<Host | null>(null);
  const [lifecycleAction, setLifecycleAction] = useState<{ host: Host; action: 'release' | 'restore' } | null>(null);
  const update = useMutation({
    mutationFn: async ({ action, host }: { action: 'renew' | 'delete' | 'release' | 'restore'; host: Host }) => {
      if (action === 'renew') return portalApi.renewHost(host.id);
      if (action === 'release') return portalApi.releaseHost(host.id, true);
      if (action === 'restore') return portalApi.restoreHost(host.id);
      return portalApi.deleteHost(host.id);
    },
    onSuccess: (_, variables) => {
      void queryClient.invalidateQueries({ queryKey: ['hosts'] });
      if (variables.action === 'delete' || variables.action === 'release' || variables.action === 'restore') void queryClient.invalidateQueries({ queryKey: ['workspaces'] });
      if (variables.action === 'delete') { notify('已提交删除请求。', 'success'); setDeleting(null); }
      else if (variables.action === 'release') { notify('已提交释放请求，文件和工作目录会保留。', 'success'); setLifecycleAction(null); }
      else if (variables.action === 'restore') { notify('已提交恢复请求。', 'success'); setLifecycleAction(null); }
      else notify('主机已续期。', 'success');
    },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });
  const bindWorkspace = useMutation({
    mutationFn: ({ host, workspace }: { host: Host; workspace: Workspace }) => portalApi.bindWorkspaceToHost(host.id, {
      workspace_id: workspace.id,
      expected_revision: workspace.revision,
      expected_binding_version: host.workspaces.find((binding) => binding.workspace.id === workspace.id)?.binding_version ?? 0,
    }),
    onSuccess: async (binding) => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ['hosts'] }),
        queryClient.invalidateQueries({ queryKey: ['workspaces'] }),
      ]);
      notify(binding.state === 'preparing'
        ? `已绑定 ${binding.workspace.name}；恢复电脑后会继续应用。`
        : `已绑定 ${binding.workspace.name}。`, 'success');
    },
    onError: async (error) => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ['hosts'] }),
        queryClient.invalidateQueries({ queryKey: ['workspaces'] }),
      ]);
      notify(errorDetails(error).message, 'error');
    },
  });
  const enter = async (host: Host) => {
    try { await enterHostInNewTab(host.id); }
    catch (error) { notify(errorDetails(error).message, 'error'); }
  };

  const sharedHosts = useQuery({ queryKey: ['shared-hosts'], queryFn: portalApi.sharedHosts });
  const enterShared = async (host: Awaited<ReturnType<typeof portalApi.sharedHosts>>['items'][number], entryTarget: 'desktop' | 'terminal' | 'workspace') => {
    try { enterHostInNewTab(host.id, entryTarget); }
    catch (error) { notify(errorDetails(error).message, 'error'); }
  };

  return <section>
    <div className="page-heading"><div><p className="eyebrow">你的云端电脑</p><h1>云端电脑</h1><p className="muted">电脑与文件都保存在你的云端空间。</p></div><div className="inline-actions"><Link className="button button-secondary" to="/workspaces">工作目录</Link>{atLimit ? <button type="button" className="button button-primary" onClick={() => notify('达到云端电脑上限,无法再创建', 'error')}>创建电脑</button> : <Link className="button button-primary" to="/hosts/new">创建电脑</Link>}</div></div>
    {hosts.isPending ? <div className="loading-card" aria-live="polite">正在加载主机…</div> : hosts.isError ? <div className="error-card" role="alert">{errorDetails(hosts.error).message}<button type="button" className="button button-secondary" onClick={() => void hosts.refetch()}>重试</button></div> : !hosts.data?.items.length ? <div className="empty-card"><h2>还没有主机</h2><p>创建一台主机开始使用浏览器、终端和文件工具。</p><Link to="/hosts/new" className="button button-primary">创建第一台主机</Link></div> : <div className="host-list">
      {hosts.data.items.map((host) => {
        const ownsHost = host.owner_id === user?.id;
        const workspaceReady = hasSingleReadyWorkspace(host);
        const workspaceRestorable = hasSingleRestorableWorkspace(host);
        const blockedBinding = host.workspaces.find((binding) => binding.state !== 'detached' && (
          binding.state !== 'active' || binding.workspace.state !== 'ready' || binding.sync_state !== 'healthy'
          || binding.sync_conflicts.length > 0 || binding.applied_revision !== binding.desired_revision
          || binding.applied_revision !== binding.workspace.revision
        ));
        const bindableWorkspaces = workspaces.data?.items.filter((workspace) => workspace.state === 'ready'
          && !hosts.data.items.some((other) => other.id !== host.id && other.workspaces.some((binding) => binding.workspace.id === workspace.id && binding.state !== 'detached'))) ?? [];
        const canBindWorkspace = ownsHost && host.state === 'stopped' && !host.workspaces.some((binding) => binding.state !== 'detached');
        return <HostCard key={host.id} host={host} busy={update.isPending && update.variables?.host.id === host.id}
          canEnter={ownsHost && workspaceReady}
          enterReason={!ownsHost ? '仅主机所有者可以进入工作环境。' : blockedBinding
            ? 'Workspace 正在准备、同步或存在冲突；打开和移交操作暂不可用。'
            : !workspaceReady ? '请先绑定恰好一个已就绪的工作区，再打开这台主机。' : undefined}
          workspaceRestorable={workspaceRestorable}
          canBindWorkspace={canBindWorkspace}
          bindableWorkspaces={bindableWorkspaces}
          bindWorkspacesLoading={workspaces.isPending}
          bindWorkspacesError={workspaces.isError ? errorDetails(workspaces.error).message : undefined}
          bindPending={bindWorkspace.isPending && bindWorkspace.variables?.host.id === host.id}
          onBindWorkspace={(workspace) => bindWorkspace.mutate({ host, workspace })}
          onEnter={() => void enter(host)} onRenew={() => update.mutate({ action: 'renew', host })} onRelease={() => setLifecycleAction({ host, action: 'release' })} onRestore={() => setLifecycleAction({ action: 'restore', host })} onDelete={() => setDeleting(host)} />;
      })}
    </div>}
    <div className="shared-hosts-section">
      <h2 className="section-title">共享给我的电脑</h2>
      {sharedHosts.isPending ? <div className="loading-card" role="status">正在加载共享电脑…</div>
        : sharedHosts.isError ? <div className="error-card" role="alert">{errorDetails(sharedHosts.error).message}<button type="button" className="button button-secondary" onClick={() => void sharedHosts.refetch()}>重试</button></div>
          : !sharedHosts.data.items.length ? <div className="empty-card compact"><p>还没有人与你共享云端电脑。</p></div>
            : <div className="host-list">{sharedHosts.data.items.map((host) => {
              const canViewDesktop = host.permissions.includes('desktop:view') || host.permissions.includes('desktop:control');
              const canControlTerminal = host.permissions.includes('terminal:control');
              const canReadWorkspace = host.permissions.includes('workspace:file:read');
              return <article className="host-card" key={host.id}>
              <div className="host-card-heading"><div><h3>{host.name || '未命名主机'}</h3><p className="muted mono">{host.id}</p></div><span className={`status status-${host.state}`}><span aria-hidden="true" className="status-dot" />{hostStateLabel[host.state] || host.state}</span></div>
              <p className="muted">权限：{host.permissions.map((permission) => sharingPermissionLabels[permission]).join('、') || '无'}</p>
              <div className="host-actions">
                {canViewDesktop && <button type="button" className="button button-primary" disabled={host.state !== 'running'} onClick={() => void enterShared(host, 'desktop')}>{host.state === 'running' ? '查看桌面' : '主机运行后可查看'}</button>}
                {canControlTerminal && <button type="button" className="button button-secondary" disabled={host.state !== 'running'} onClick={() => void enterShared(host, 'terminal')}>{host.state === 'running' ? '打开终端' : '主机运行后可打开终端'}</button>}
                {canReadWorkspace && <button type="button" className="button button-secondary" disabled={host.state !== 'running'} onClick={() => void enterShared(host, 'workspace')}>{host.state === 'running' ? '查看工作区' : '主机运行后可查看工作区'}</button>}
                {host.permissions.includes('mcp:control') && <Link className="button button-secondary" to="/apikeys">配置 MCP API Key</Link>}
              </div>
              {canViewDesktop && host.state === 'running' && <HostControlPanel
                host={host}
                canControl={host.permissions.includes('desktop:control')}
                canReleaseAnyLease={false}
              />}
            </article>;
            })}</div>}
    </div>
    {deleting && <ConfirmDialog title={`删除主机 ${deleting.name || deleting.id}`} description="删除后无法恢复。云端电脑的运行环境和专属资料卷会被删除，正在运行的终端命令及服务会中止。绑定的独立工作区会自动解绑并保留在服务端，不随主机删除。" confirmLabel="删除主机" danger busy={update.isPending} onClose={() => setDeleting(null)} onConfirm={() => update.mutate({ action: 'delete', host: deleting })} />}
    {lifecycleAction && <ConfirmDialog
      title={lifecycleAction.action === 'release' ? `释放主机 ${lifecycleAction.host.name || lifecycleAction.host.id} 的运行资源` : `恢复主机 ${lifecycleAction.host.name || lifecycleAction.host.id}`}
      description={lifecycleAction.action === 'release'
        ? '这会停止云端运行环境并释放计算资源。主机身份、工作目录和文件会保留。终端窗口断开后，里面的命令仍可能继续运行；确认释放后，运行中的终端命令、AI / 桌面应用与网站服务都会中止。请先确认没有需要保留的工作。'
        : '这会重新创建云端运行环境并保留原有文件。主机恢复后，需要重新打开浏览器或应用。'}
      confirmLabel={lifecycleAction.action === 'release' ? '释放运行资源' : '恢复主机'}
      busy={update.isPending}
      onClose={() => setLifecycleAction(null)}
      onConfirm={() => update.mutate({ action: lifecycleAction.action, host: lifecycleAction.host })}
    />}
  </section>;
}

function HostCard({ host, busy, canEnter, enterReason, workspaceRestorable, canBindWorkspace, bindableWorkspaces, bindWorkspacesLoading, bindWorkspacesError, bindPending, onBindWorkspace, onEnter, onRenew, onRelease, onRestore, onDelete }: {
  host: Host; busy: boolean; canEnter: boolean; enterReason?: string; workspaceRestorable: boolean; canBindWorkspace: boolean;
  bindableWorkspaces: Workspace[]; bindWorkspacesLoading: boolean; bindWorkspacesError?: string; bindPending: boolean;
  onBindWorkspace: (workspace: Workspace) => void; onEnter: () => void; onRenew: () => void; onRelease: () => void; onRestore: () => void; onDelete: () => void;
}) {
  const [workspaceID, setWorkspaceID] = useState('');
  const name = host.name || host.id;
  const enterHelpID = `host-enter-help-${host.id}`;
  const hasWorkspaceBinding = host.workspaces.some((binding) => binding.state !== 'detached');
  const selectedWorkspace = bindableWorkspaces.find((workspace) => workspace.id === workspaceID);
  return <article className="host-card">
    <div className="host-card-heading"><div><h2>{host.name || '未命名主机'}</h2><p className="muted mono">{host.id} · {host.template_id}</p></div><span className={`status status-${host.state}`}><span aria-hidden="true" className="status-dot" />{hostStateLabel[host.state] || host.state}</span></div>
    <div className="host-facts">
      <div><span>规格</span><strong>{formatCpu(host.resources?.cpu_limit_milli)} / {formatMemory(host.resources?.mem_limit_mib)}</strong></div>
      <div><span>到期时间</span><strong>{formatDate(host.expires_at)}</strong></div>
      <div><span>工作区</span><strong>{host.workspaces.length ? host.workspaces.map((binding) => binding.workspace.name || binding.workspace.id).join('、') : '无'}</strong>
        {host.workspaces.map((binding) => <small className="table-sub" key={binding.workspace.id}>{binding.sync_state === 'conflict' || binding.sync_conflicts.length
          ? '同步冲突（打开、恢复和移交已锁定）'
          : binding.state === 'active' && binding.applied_revision === binding.desired_revision ? `已应用 v${binding.applied_revision}`
            : binding.state === 'preparing' ? `正在应用 v${binding.desired_revision}`
              : binding.state === 'unbinding' ? '正在解绑（操作已锁定）'
                : binding.state === 'failed' ? '应用失败（操作已锁定）' : binding.state}</small>)}
      </div>
      <div><span>运行状态</span><strong>{hostReasonLabel[host.state_reason] || host.state_reason || (host.state === 'running' ? '运行正常' : host.state === 'stopped' ? '可恢复' : '等待状态更新')}</strong></div>
      <div><span>默认工作/下载目录</span><strong>/home/gem/workspace</strong></div>
      <div><span>旧容器数据迁移</span><strong>状态未知</strong><small className="table-sub">当前服务未提供迁移状态，不能据此确认旧数据已进入 Workspace。</small></div>
    </div>
    <div className="host-actions">
      <button type="button" className="button button-primary" onClick={onEnter} disabled={!canEnter || host.state !== 'running' || busy || host.control_state === 'taking_over' || host.control_state === 'returning'} aria-label={`${host.control_state === 'user' ? '操作' : host.control_state === 'taking_over' || host.control_state === 'returning' ? '控制切换中' : '查看'}主机 ${name}`} aria-describedby={!canEnter ? enterHelpID : undefined}>{host.control_state === 'user' ? '操作电脑' : host.control_state === 'taking_over' || host.control_state === 'returning' ? '控制切换中' : '查看电脑'}</button>
      {!canEnter && <span className="muted" id={enterHelpID}>{enterReason}{enterReason?.startsWith('请先') && <> <Link to="/workspaces">管理工作区</Link></>}</span>}
      <button type="button" className="button button-secondary" onClick={onRenew} disabled={host.state !== 'running' || busy} aria-label={`续期主机 ${name}`}>续期</button>
      {host.state === 'running' && <button type="button" className="button button-secondary" onClick={onRelease} disabled={busy} aria-label={`释放运行资源 ${name}`}>释放运行资源</button>}
      {host.state === 'stopped' && <button type="button" className="button button-primary" onClick={onRestore} disabled={busy || !workspaceRestorable} aria-label={`恢复主机 ${name}`} title={!workspaceRestorable ? '恢复前需要选择一个就绪工作区' : undefined}>恢复</button>}
      {host.state === 'stopped' && !workspaceRestorable && <span className="muted" role="status">恢复前需绑定并准备好一个工作区。<Link to="/workspaces">查看工作区</Link></span>}
      <button type="button" className="button button-danger-quiet" onClick={onDelete} disabled={host.state === 'deleting' || host.state === 'creating' || host.state === 'stopping' || busy} aria-label={`删除主机 ${name}`}>删除</button>
    </div>
    {canBindWorkspace && !hasWorkspaceBinding && <form onSubmit={(event) => { event.preventDefault(); if (selectedWorkspace) onBindWorkspace(selectedWorkspace); }} className="host-workspace-bind">
      <label htmlFor={`bind-workspace-${host.id}`}>恢复前选择一个工作区</label>
      <select id={`bind-workspace-${host.id}`} value={workspaceID} onChange={(event) => setWorkspaceID(event.target.value)} disabled={bindWorkspacesLoading || bindPending}>
        <option value="">选择已就绪的工作区</option>
        {bindableWorkspaces.map((workspace) => <option key={workspace.id} value={workspace.id}>{workspace.name} · v{workspace.revision}</option>)}
      </select>
      <button type="submit" className="button button-secondary" disabled={!selectedWorkspace || bindPending}>{bindPending ? '绑定中…' : '绑定所选工作区'}</button>
      {bindWorkspacesLoading && <span className="muted" role="status">正在读取可用工作区…</span>}
      {bindWorkspacesError && <span className="panel-error" role="alert">无法读取工作区：{bindWorkspacesError}</span>}
      {!bindWorkspacesLoading && !bindWorkspacesError && !bindableWorkspaces.length && <span className="muted">没有可绑定的已就绪工作区。<Link to="/workspaces">创建工作区</Link></span>}
    </form>}
    {canEnter && <HostMemberManager hostId={host.id} />}
    {canEnter && host.state === 'running' && <HostControlPanel host={host} canControl canReleaseAnyLease />}
  </article>;
}

function hasSingleReadyWorkspace(host: Host) {
  const bindings = host.workspaces.filter((binding) => binding.state !== 'detached');
  const binding = bindings[0];
  return bindings.length === 1 && binding?.state === 'active' && binding.workspace.state === 'ready' &&
    binding.sync_state === 'healthy' && binding.sync_conflicts.length === 0 && binding.applied_revision === binding.desired_revision &&
    binding.applied_revision === binding.workspace.revision;
}

function hasSingleRestorableWorkspace(host: Host) {
  const bindings = host.workspaces.filter((binding) => binding.state !== 'detached');
  const binding = bindings[0];
  return bindings.length === 1 && !!binding && binding.workspace.state === 'ready'
    && binding.workspace.revision === binding.desired_revision && binding.sync_state === 'healthy' && binding.sync_conflicts.length === 0
    && (binding.state === 'preparing' || (binding.state === 'active' && binding.applied_revision === binding.desired_revision));
}

function HostControlPanel({ host, canControl, canReleaseAnyLease }: {
  host: { id: string; name?: string; control_state?: Host['control_state'] }; canControl: boolean; canReleaseAnyLease: boolean;
}) {
  const queryClient = useQueryClient();
  const { user } = useAuth();
  const { notify } = useToast();
  const [confirmAction, setConfirmAction] = useState<ControlActionRequest | null>(null);
  const [pendingOperationCount, setPendingOperationCount] = useState(0);
  const control = useQuery({
    queryKey: ['host-control', host.id],
    queryFn: () => portalApi.hostControl(host.id),
    refetchInterval: (query) => {
      const value = query.state.data;
      return value && (value.active_transition || value.control_state === 'taking_over' || value.control_state === 'returning') ? 1500 : false;
    },
  });
  const updateControl = useMutation({
    mutationFn: (body: ControlActionRequest) => portalApi.hostControlAction(host.id, body),
    onSuccess: async (result, body) => {
      setPendingOperationCount(result.pending_operation_ids.length);
      queryClient.setQueryData<ControlStatus>(['host-control', host.id], {
        host_id: result.host_id,
        control_state: result.control_state,
        control_version: result.control_version,
        control_actor_user_id: result.control_actor_user_id,
        control_lease_expires_at: result.control_lease_expires_at,
        control_lease_active: result.control_lease_active,
        control_lease_held_by_me: result.control_lease_held_by_me,
        active_transition: result.active_transition,
      });
      setConfirmAction(null);
      void queryClient.invalidateQueries({ queryKey: ['host-control', host.id] });
      void queryClient.invalidateQueries({ queryKey: ['hosts'] });
      if (body.action === 'release') void queryClient.invalidateQueries({ queryKey: ['workspaces'] });
      if (result.active_transition || result.control_state === 'taking_over' || result.control_state === 'returning') {
        notify(body.action === 'take_over' ? '接管请求已受理，等待云端工具停止确认；完成前桌面操作仍受限制。' : '交还请求已受理，等待云端确认控制权切换。', 'info');
      } else if (body.action === 'take_over' && result.control_state === 'user') {
        notify('接管已完成，现在可以操作电脑。', 'success');
      } else if (body.action === 'release' && result.control_state === 'ai') {
        notify('已交还 AI，主机和工作区版本已刷新。', 'success');
      } else {
        notify('控制状态已更新，正在核对主机状态。', 'info');
      }
    },
    onError: (error) => {
      const details = errorDetails(error);
      if (details.code === 'conflict') {
        setConfirmAction(null);
        notify('控制状态已变化，已刷新最新状态，请重新操作。', 'error');
      } else {
        notify(details.message, 'error');
      }
      void queryClient.invalidateQueries({ queryKey: ['host-control', host.id] });
      void queryClient.invalidateQueries({ queryKey: ['hosts'] });
    },
  });
  const status = control.data;
  const refetchControl = control.refetch;
  const state = status?.control_state ?? host.control_state;
  const transition = status?.active_transition;
  const transitioning = state === 'taking_over' || state === 'returning';
  const operator = status?.control_lease_active && status.control_actor_user_id
    ? status.control_actor_user_id === user?.id ? '你' : `账号 ${status.control_actor_user_id}`
    : '未知';
  const canRelease = canControl && state === 'user' && !!status
    && (canReleaseAnyLease || status.control_lease_held_by_me);

  useEffect(() => {
    if (state !== 'user' || !status?.control_lease_active || !status.control_lease_held_by_me) return;
    let renewing = false;
    const timer = window.setInterval(() => {
      if (renewing) return;
      renewing = true;
      void portalApi.renewHostControlLease(host.id, status.control_version)
        .then((next) => queryClient.setQueryData<ControlStatus>(['host-control', host.id], next))
        .catch(() => void refetchControl())
        .finally(() => { renewing = false; });
    }, 10_000);
    return () => window.clearInterval(timer);
  }, [host.id, queryClient, refetchControl, state, status?.control_lease_active, status?.control_lease_held_by_me, status?.control_version]);

  const startAction = (action: ControlActionRequest['action']) => {
    if (!status) return;
    setConfirmAction({ action, request_id: newIdempotencyKey(), expected_control_version: status.control_version });
  };
  const resumeTransition = () => {
    if (!transition) return;
    updateControl.mutate({
      action: transition.action,
      request_id: transition.request_id,
      expected_control_version: transition.expected_control_version,
    });
  };

  return <section className="host-control" aria-label={`电脑控制状态 ${host.name || host.id}`}>
    <div className="host-control-summary">
      <div>
        <strong>电脑控制：{state ? controlStateLabels[state] : '读取中'}</strong>
        <p className="muted">{!state
          ? '正在读取控制状态；状态确认前不会开放人工操作。'
          : state === 'ai'
            ? '当前由云端 AI 控制。工作台仅供查看；接管完成并确认工具停止后，才允许人工操作。'
            : state === 'user'
              ? status?.control_lease_active
                ? `当前由${operator}人工控制。交还完成后，云端 AI 才能恢复操作。`
                : '操作租约已失效；工作台保持只读，请重新读取控制状态。'
              : '控制权切换期间，工作台操作会保持受限。'}</p>
        {state === 'user' && status && <p className="muted">操作租约：{status.control_lease_active
          ? `${status.control_lease_held_by_me ? '由你持有' : `由${operator}持有`}，到期时间 ${status.control_lease_expires_at ? formatDate(status.control_lease_expires_at) : '未知'}`
          : '当前无有效租约'}</p>}
        {status && <p className="muted">控制版本：v{status.control_version}</p>}
        <p className="muted">桌面画面可能显示工作区内容，获准查看桌面的共享成员也能看到。</p>
      </div>
      {canControl && !control.isPending && !control.isError && status && !transitioning && state === 'ai' && <button type="button" className="button button-secondary" disabled={updateControl.isPending} onClick={() => startAction('take_over')}>接管操作</button>}
      {canRelease && !transitioning && <button type="button" className="button button-secondary" disabled={updateControl.isPending} onClick={() => startAction('release')}>交还 AI</button>}
      {canControl && transition && <button type="button" className="button button-secondary" disabled={updateControl.isPending} onClick={resumeTransition}>{transition.action === 'take_over' ? '继续接管确认' : '继续交还确认'}</button>}
      {control.isError && <button type="button" className="button button-secondary" onClick={() => void control.refetch()}>重试读取控制状态</button>}
      {control.isPending && <span className="muted" role="status">正在读取控制状态…</span>}
    </div>
    {transition && <p className="host-control-transition" role="status">
      {transition.action === 'take_over' ? '接管尚未完成' : '交还尚未完成'}：{controlPhaseLabels[transition.phase]}。
      {transition.failure_reason && ` ${controlFailureLabels[transition.failure_reason] || '云端尚未确认控制切换。'}`}
      {pendingOperationCount > 0 && ` 尚有 ${pendingOperationCount} 项操作等待停止确认。`}
      {transition.failure_reason === 'operations_pending' && pendingOperationCount === 0 && (transition.action === 'take_over' ? ' 云端仍有未完成操作，继续等待停止回执后再接管。' : ' 云端仍有未完成操作，继续等待工具结束后再交还。')}
    </p>}
    {transitioning && !transition && <p className="host-control-transition" role="status">控制状态正在切换，尚未收到最终确认；请等待刷新。</p>}
    {control.isError && <p className="host-control-transition" role="alert">{errorDetails(control.error).message}</p>}
    {confirmAction && <ConfirmDialog
      title={confirmAction.action === 'take_over' ? `接管主机 ${host.name || host.id}` : `交还主机 ${host.name || host.id} 给 AI`}
      description={confirmAction.action === 'take_over'
        ? '接管会先阻止新的 AI 操作，并等待在途工具确认停止。等待期间工作台保持只读；只有云端确认停止并完成控制权切换后，才允许人工操作。'
        : '交还会把主机控制权恢复给云端 AI。完成后页面会重新读取主机和工作区版本，以核对最新文件状态。'}
      confirmLabel={confirmAction.action === 'take_over' ? '开始接管' : '交还给 AI'}
      busy={updateControl.isPending}
      onClose={() => setConfirmAction(null)}
      onConfirm={() => updateControl.mutate(confirmAction)}
    />}
  </section>;
}
