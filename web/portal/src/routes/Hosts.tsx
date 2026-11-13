import { useEffect, useMemo, useState } from 'react';
import { Link, useLocation } from 'react-router';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { errorDetails, enterHostInNewTab, newIdempotencyKey, portalApi } from '../app/api';
import type { HostMemberPermission } from '../app/api';
import type { components } from '../api/schema';
import { useAuth } from '../app/auth';
import { formatCpu, formatDate, formatMemory, hostStateLabel } from '../app/format';
import { useToast } from '../app/toast';
import { ConfirmDialog } from '../components/ConfirmDialog';
import { HostMemberManager } from '../components/HostMemberManager';
import { ScreenPagination } from '../components/ScreenPagination';
import './Hosts.css';
import { FloatingError } from '../components/FloatingError';

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
  'workspace:file:read': '读取云端电脑文件',
};
const hostReasonLabel: Record<string, string> = {
  user_release: '正在释放计算资源',
  restore_pending: '正在恢复运行环境',
  restore_unknown: '恢复结果待核实，请联系管理员',
  restore_failed: '恢复失败，可以重试',
};

export function Hosts() {
  const queryClient = useQueryClient();
  const location = useLocation();
  const { user } = useAuth();
  const { notify } = useToast();
  const [listSelection, setListSelection] = useState<{ mode: 'owned' | 'shared'; hash: string } | null>(null);
  const hosts = useQuery({
    queryKey: ['hosts'], queryFn: portalApi.hosts,
    refetchInterval: (query) => query.state.data?.items.some((host) => unsettled.has(host.state)) ? 3000 : query.state.data?.items.length ? 12000 : false,
    refetchIntervalInBackground: false,
  });
  const preferences = useQuery({ queryKey: ['user-preferences'], queryFn: portalApi.preferences, retry: false });
  const ownedHosts = hosts.data?.items.filter((host) => host.owner_id === user?.id) ?? [];
  const defaultHost = ownedHosts.find((host) => host.id === preferences.data?.default_host_id);
  const defaultWorkspace = defaultHost?.workspaces.find((binding) => binding.state !== 'detached');
  const setDefaultHost = useMutation({
    mutationFn: (hostID: string) => portalApi.ensureDefaultHost({ host_id: hostID }),
    onSuccess: async () => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ['user-preferences'] }),
        queryClient.invalidateQueries({ queryKey: ['onboarding-status'] }),
      ]);
      notify('默认云端电脑已更新。', 'success');
    },
    onError: async (error) => {
      await queryClient.invalidateQueries({ queryKey: ['user-preferences'] });
      notify(errorDetails(error).message, 'error');
    },
  });
  const hashHostID = location.hash.startsWith('#host-') ? location.hash.slice('#host-'.length) : '';
  const displayedHosts = useMemo(() => prioritizeHostForHash(hosts.data?.items ?? [], hashHostID), [hosts.data?.items, hashHostID]);
  const workspaces = useQuery({
    queryKey: ['workspaces'], queryFn: portalApi.workspaces,
    enabled: hosts.data?.items.some((host) => host.owner_id === user?.id && host.state === 'stopped'
      && !host.workspaces.some((binding) => binding.state !== 'detached')) ?? false,
  });
  const atLimit = !!user && (hosts.data?.items.length ?? 0) >= user.quota.hosts;
  const [deleting, setDeleting] = useState<Host | null>(null);
  const [lifecycleAction, setLifecycleAction] = useState<{ host: Host; action: 'release' | 'restore' } | null>(null);
  const [copyDialog, setCopyDialog] = useState<{ host: Host; name: string; idempotencyKey: string } | null>(null);
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
  const copyHost = useMutation({
    mutationFn: ({ host, name, idempotencyKey }: { host: Host; name: string; idempotencyKey: string }) => {
      const binding = host.workspaces.find((item) => item.state !== 'detached');
      if (!binding) throw new Error('找不到可复制的云端文件版本。');
      return portalApi.copyHost(host.id, { name: name.trim(), expected_current_revision: binding.workspace.revision }, idempotencyKey);
    },
    onSuccess: async (result) => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ['hosts'] }),
        queryClient.invalidateQueries({ queryKey: ['workspaces'] }),
      ]);
      notify(`已创建云端电脑“${result.host.name || result.host.id}”。`, 'success');
      setCopyDialog(null);
    },
    onError: async (error) => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ['hosts'] }),
        queryClient.invalidateQueries({ queryKey: ['workspaces'] }),
      ]);
      notify(`${errorDetails(error).message} 若结果不确定，请保持名称不变并在此窗口重试。`, 'error');
    },
  });
  const enter = async (host: Host) => {
    try { await enterHostInNewTab(host.id); }
    catch (error) { notify(errorDetails(error).message, 'error'); }
  };

  const sharedHosts = useQuery({ queryKey: ['shared-hosts'], queryFn: portalApi.sharedHosts });
  const displayedSharedHosts = useMemo(() => prioritizeHostForHash(sharedHosts.data?.items ?? [], hashHostID), [sharedHosts.data?.items, hashHostID]);
  const listMode = listSelection?.hash === location.hash ? listSelection.mode
    : hashHostID && sharedHosts.data?.items.some((host) => host.id === hashHostID) ? 'shared'
      : hashHostID && hosts.data?.items.some((host) => host.id === hashHostID) ? 'owned'
        : hosts.isSuccess && !hosts.data.items.length && sharedHosts.data?.items.length ? 'shared' : 'owned';
  const selectList = (mode: 'owned' | 'shared') => setListSelection({ mode, hash: location.hash });
  const enterShared = async (host: Awaited<ReturnType<typeof portalApi.sharedHosts>>['items'][number], entryTarget: 'desktop' | 'terminal' | 'workspace') => {
    try { enterHostInNewTab(host.id, entryTarget); }
    catch (error) { notify(errorDetails(error).message, 'error'); }
  };

  return <section className="hosts-page">
    <nav className="inline-actions hosts-list-toggle" aria-label="电脑列表">
      <button type="button" className={`button ${listMode === 'owned' ? 'button-primary' : 'button-secondary'}`} aria-pressed={listMode === 'owned'} onClick={() => selectList('owned')}>我的电脑</button>
      <button type="button" className={`button ${listMode === 'shared' ? 'button-primary' : 'button-secondary'}`} aria-pressed={listMode === 'shared'} onClick={() => selectList('shared')}>共享给我的</button>
    </nav>
    {listMode === 'owned' ? <section className="design-panel hosts-panel" aria-labelledby="my-hosts-title">
    <div className="design-panel-header"><div><h2 id="my-hosts-title">我的电脑</h2><p>管理你的云端电脑、文件和运行状态。</p></div><div className="inline-actions">{atLimit ? <button type="button" className="button button-primary" onClick={() => notify('达到云端电脑上限,无法再创建', 'error')}>＋ 创建电脑</button> : <Link className="button button-primary" to="/hosts/new">＋ 创建电脑</Link>}</div></div>
    {hosts.isSuccess && <div className="host-default-settings" aria-label="默认云端电脑设置">
      <label htmlFor="default-cloud-host">默认云端电脑
        <select id="default-cloud-host" value={preferences.data?.default_host_id ?? ''}
          disabled={preferences.isPending || preferences.isError || setDefaultHost.isPending || !ownedHosts.length}
          onChange={(event) => { if (event.target.value) setDefaultHost.mutate(event.target.value); }}>
          <option value="" disabled>{preferences.isPending ? '正在读取设置…' : ownedHosts.length ? '选择一台电脑' : '创建电脑后可设置'}</option>
          {preferences.data?.default_host_id && !ownedHosts.some((host) => host.id === preferences.data?.default_host_id)
            && <option value={preferences.data.default_host_id}>当前默认电脑不可用</option>}
          {ownedHosts.map((host) => <option key={host.id} value={host.id}>{host.name || host.id}</option>)}
        </select>
      </label>
      <div className="host-default-workspace">
        <span>默认 Workspace</span>
        {defaultWorkspace ? <strong title={defaultWorkspace.workspace.name}>{defaultWorkspace.workspace.name}</strong>
          : <strong>{defaultHost ? '未检测到已绑定 Workspace' : '选择电脑后显示'}</strong>}
        {defaultHost && <Link to={`/workspaces?host=${encodeURIComponent(defaultHost.id)}`}>管理 Workspace</Link>}
      </div>
      {preferences.isError && <FloatingError className="error-inline" role="alert">默认设置暂不可用。
        <button type="button" className="button button-small button-secondary" onClick={() => void preferences.refetch()}>重试</button>
      </FloatingError>}
    </div>}
    {hosts.isPending ? <div className="loading-card" aria-live="polite">正在加载主机…</div> : hosts.isError ? <FloatingError className="error-card" role="alert">{errorDetails(hosts.error).message}<button type="button" className="button button-secondary" onClick={() => void hosts.refetch()}>重试</button></FloatingError> : !hosts.data?.items.length ? <div className="empty-card"><h2>还没有主机</h2><p>创建一台主机开始使用浏览器、终端和文件工具。</p><Link to="/hosts/new" className="button button-primary">创建第一台主机</Link></div> : <ScreenPagination
      items={displayedHosts}
      getKey={(host) => host.id}
      estimatedItemHeight={72}
      height={560}
      gap={10}
      className="hosts-pagination"
      ariaLabel="我的电脑"
      renderItem={(host) => {
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
        const copyReason = atLimit ? '已达到云端电脑数量上限。'
          : host.state !== 'stopped' ? '请先停止源电脑。'
            : host.control_state !== 'ai' ? '请先将电脑控制权交还给云端 AI。'
              : !workspaceReady ? '源电脑文件尚未全部确认或仍有冲突。' : undefined;
        return <HostCard key={host.id} host={host} busy={update.isPending && update.variables?.host.id === host.id}
          canEnter={ownsHost && workspaceReady}
          enterReason={!ownsHost ? '仅主机所有者可以进入工作环境。' : blockedBinding
            ? '云端电脑的文件正在准备、同步或存在冲突；打开和移交操作暂不可用。'
            : !workspaceReady ? '请先关联并准备好这台电脑的文件，再打开电脑。' : undefined}
          workspaceRestorable={workspaceRestorable}
          canBindWorkspace={canBindWorkspace}
          bindableWorkspaces={bindableWorkspaces}
          bindWorkspacesLoading={workspaces.isPending}
          bindWorkspacesError={workspaces.isError ? errorDetails(workspaces.error).message : undefined}
          bindPending={bindWorkspace.isPending && bindWorkspace.variables?.host.id === host.id}
          onBindWorkspace={(workspace) => bindWorkspace.mutate({ host, workspace })}
          canShowCopy={ownsHost} canCopy={ownsHost && !copyReason} copyReason={ownsHost ? copyReason : undefined}
          copyPending={copyHost.isPending && copyHost.variables?.host.id === host.id}
          onCopy={() => setCopyDialog({ host, name: `${(host.name || '云端电脑').slice(0, 96)} 副本`, idempotencyKey: newIdempotencyKey() })}
          onEnter={() => void enter(host)} onRenew={() => update.mutate({ action: 'renew', host })} onRelease={() => setLifecycleAction({ host, action: 'release' })} onRestore={() => setLifecycleAction({ action: 'restore', host })} onDelete={() => setDeleting(host)} />;
      }}
    />}
    </section> : <section className="shared-hosts-section design-panel">
      <div className="design-panel-header"><h2>共享给我的电脑</h2></div>
      {sharedHosts.isPending ? <div className="loading-card" role="status">正在加载共享电脑…</div>
        : sharedHosts.isError ? <FloatingError className="error-card" role="alert">{errorDetails(sharedHosts.error).message}<button type="button" className="button button-secondary" onClick={() => void sharedHosts.refetch()}>重试</button></FloatingError>
          : !sharedHosts.data.items.length ? <div className="empty-card compact"><p>还没有人与你共享云端电脑。</p></div>
            : <ScreenPagination
              items={displayedSharedHosts}
              getKey={(host) => host.id}
              estimatedItemHeight={180}
              height={560}
              gap={10}
              className="hosts-pagination"
              ariaLabel="共享给我的电脑"
              renderItem={(host) => {
              const canViewDesktop = host.permissions.includes('desktop:view') || host.permissions.includes('desktop:control');
              const canControlTerminal = host.permissions.includes('terminal:control');
              const canReadWorkspace = host.permissions.includes('workspace:file:read');
              return <article className="host-card host-card-compact" id={`host-${host.id}`} key={host.id}>
              <div className="host-card-heading"><div><h3>{host.name || '未命名主机'}</h3><p className="muted mono">{host.id}</p></div><span className={`status status-${host.state}`}><span aria-hidden="true" className="status-dot" />{hostStateLabel[host.state] || host.state}</span></div>
              <p className="muted host-permissions">权限：{host.permissions.map((permission) => sharingPermissionLabels[permission]).join('、') || '无'}</p>
              <div className="host-actions">
                {canViewDesktop && <button type="button" className="button button-primary" disabled={host.state !== 'running'} onClick={() => void enterShared(host, 'desktop')}>{host.state === 'running' ? '查看桌面' : '主机运行后可查看'}</button>}
                {canControlTerminal && <button type="button" className="button button-secondary" disabled={host.state !== 'running'} onClick={() => void enterShared(host, 'terminal')}>{host.state === 'running' ? '打开终端' : '主机运行后可打开终端'}</button>}
                {canReadWorkspace && <button type="button" className="button button-secondary" disabled={host.state !== 'running'} onClick={() => void enterShared(host, 'workspace')}>{host.state === 'running' ? '查看电脑文件' : '电脑运行后可查看文件'}</button>}
                {host.permissions.includes('mcp:control') && <Link className="button button-secondary" to="/apikeys">配置 MCP API Key</Link>}
              </div>
              {canViewDesktop && host.state === 'running' && <HostControlDisclosure
                host={host}
                canControl={host.permissions.includes('desktop:control')}
                canReleaseAnyLease={false}
              />}
            </article>;
              }}
            />}
    </section>}
    {deleting && <ConfirmDialog title={`删除云端电脑 ${deleting.name || deleting.id}`} description="删除后无法恢复。云端电脑的运行环境和专属资料卷会被删除，正在运行的终端命令及服务会中止。关联的云端文件会自动解除关联并保留在服务端，不随电脑删除。" confirmLabel="删除电脑" danger busy={update.isPending} onClose={() => setDeleting(null)} onConfirm={() => update.mutate({ action: 'delete', host: deleting })} />}
    {copyDialog && <ConfirmDialog
      title={`复制云端电脑 ${copyDialog.host.name || copyDialog.host.id}`}
      description={`将复制已确认的云端文件版本；其中可能包含隐藏文件、配置和密钥。新电脑拥有独立文件空间，源电脑保持不变。运行环境、应用状态、平台凭据、任务和本机同步关联不会复制。`}
      confirmLabel="创建副本"
      busy={copyHost.isPending}
      confirmDisabled={!copyDialog.name.trim()}
      onClose={() => setCopyDialog(null)}
      onConfirm={() => copyHost.mutate(copyDialog)}
    >
      <label htmlFor="host-copy-name">新电脑名称</label>
      <input id="host-copy-name" value={copyDialog.name} maxLength={100} onChange={(event) => setCopyDialog({ ...copyDialog, name: event.target.value })} />
      <p className="muted">提交后如需重试，请保留相同名称；本次操作会继续使用同一个幂等键。</p>
    </ConfirmDialog>}
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

function prioritizeHostForHash<T extends { id: string }>(items: readonly T[], hostID: string): readonly T[] {
  if (!/^h[a-z0-9]{7}$/.test(hostID)) return items;
  const index = items.findIndex((host) => host.id === hostID);
  if (index <= 0) return items;
  const [target] = items.slice(index, index + 1);
  return target ? [target, ...items.slice(0, index), ...items.slice(index + 1)] : items;
}

function HostCard({ host, busy, canEnter, enterReason, workspaceRestorable, canBindWorkspace, bindableWorkspaces, bindWorkspacesLoading, bindWorkspacesError, bindPending, canShowCopy, canCopy, copyReason, copyPending, onBindWorkspace, onCopy, onEnter, onRenew, onRelease, onRestore, onDelete }: {
  host: Host; busy: boolean; canEnter: boolean; enterReason?: string; workspaceRestorable: boolean; canBindWorkspace: boolean;
  bindableWorkspaces: Workspace[]; bindWorkspacesLoading: boolean; bindWorkspacesError?: string; bindPending: boolean; canShowCopy: boolean; canCopy: boolean; copyReason?: string; copyPending: boolean;
  onBindWorkspace: (workspace: Workspace) => void; onCopy: () => void; onEnter: () => void; onRenew: () => void; onRelease: () => void; onRestore: () => void; onDelete: () => void;
}) {
  const [workspaceID, setWorkspaceID] = useState('');
  const [detailsOpen, setDetailsOpen] = useState(false);
  const name = host.name || host.id;
  const enterHelpID = `host-enter-help-${host.id}`;
  const copyHelpID = `host-copy-help-${host.id}`;
  const hasWorkspaceBinding = host.workspaces.some((binding) => binding.state !== 'detached');
  const selectedWorkspace = bindableWorkspaces.find((workspace) => workspace.id === workspaceID);
  const runtimeState = host.workspace_runtime_online === true ? '在线' : host.workspace_runtime_online === false ? '离线' : '未知';
  return <article className="host-card host-card-compact" id={`host-${host.id}`}>
    <div className="host-card-heading"><h2>{host.name || '未命名主机'}</h2><span className={`status status-${host.state}`}><span aria-hidden="true" className="status-dot" />{hostStateLabel[host.state] || host.state}</span></div>
    <div className="host-actions host-primary-actions">
      <button type="button" className="button button-primary" onClick={onEnter} disabled={!canEnter || host.state !== 'running' || busy || host.control_state === 'taking_over' || host.control_state === 'returning'} aria-label={`${host.control_state === 'user' ? '操作' : host.control_state === 'taking_over' || host.control_state === 'returning' ? '控制切换中' : '查看'}云端电脑 ${name}`}>{host.control_state === 'user' ? '操作电脑' : host.control_state === 'taking_over' || host.control_state === 'returning' ? '控制切换中' : '查看电脑'}</button>
      <details className="host-more-actions">
        <summary aria-label={`更多操作 ${name}`}>更多操作</summary>
        <div>
          <Link className="button button-secondary" to={`/workspaces?host=${encodeURIComponent(host.id)}`}>文件与版本</Link>
          {!canEnter && <p className="host-action-reason" id={enterHelpID} role="status">{enterReason}{enterReason?.startsWith('请先') && <> <Link to={`/workspaces?host=${encodeURIComponent(host.id)}`}>管理电脑文件</Link></>}</p>}
          <button type="button" className="button button-secondary" aria-controls={`host-details-${host.id}`} aria-expanded={detailsOpen} onClick={(event) => {
            setDetailsOpen((value) => !value);
            const actions = event.currentTarget.closest<HTMLDetailsElement>('.host-more-actions');
            if (actions) { actions.open = false; actions.querySelector('summary')?.focus(); }
          }}>{detailsOpen ? '收起详情与管理' : '详情与管理'}</button>
          <button type="button" className="button button-secondary" onClick={onRenew} disabled={host.state !== 'running' || busy} aria-label={`续期主机 ${name}`}>续期</button>
          {host.state === 'running' && <button type="button" className="button button-secondary" onClick={onRelease} disabled={busy} aria-label={`释放运行资源 ${name}`}>释放运行资源</button>}
          {host.state === 'stopped' && <button type="button" className="button button-primary" onClick={onRestore} disabled={busy || !workspaceRestorable} aria-label={`恢复云端电脑 ${name}`} title={!workspaceRestorable ? '恢复前需要关联并准备好云端文件' : undefined}>恢复</button>}
          {host.state === 'stopped' && !workspaceRestorable && <span className="muted" role="status">恢复前需准备好云端文件。<Link to={`/workspaces?host=${encodeURIComponent(host.id)}`}>查看电脑文件</Link></span>}
          {canShowCopy && <button type="button" className="button button-secondary" onClick={onCopy} disabled={!canCopy || copyPending} aria-label={`复制云端电脑 ${name}`} aria-describedby={copyReason ? copyHelpID : undefined}>复制电脑</button>}
          {copyReason && <span className="muted" id={copyHelpID}>{copyReason}</span>}
          <button type="button" className="button button-danger-quiet" onClick={onDelete} disabled={host.state === 'deleting' || host.state === 'creating' || host.state === 'stopping' || busy} aria-label={`删除云端电脑 ${name}`}>删除</button>
        </div>
      </details>
    </div>
    <section className="host-secondary-details" id={`host-details-${host.id}`} aria-label="详情与管理" hidden={!detailsOpen}>
      {detailsOpen && <div className="host-secondary-content">
    <div className="host-facts">
      <div><span>规格</span><strong>{formatCpu(host.resources?.cpu_limit_milli)} / {formatMemory(host.resources?.mem_limit_mib)}</strong></div>
      <div><span>到期时间</span><strong>{formatDate(host.expires_at)}</strong></div>
      <div><span>云端文件</span><strong>{host.workspaces.length ? host.workspaces.map((binding) => binding.workspace.name || binding.workspace.id).join('、') : '暂无文件空间'}</strong>
        {host.workspaces.map((binding) => <small className="table-sub" key={binding.workspace.id}>{binding.sync_state === 'conflict' || binding.sync_conflicts.length
          ? '文件同步冲突（打开、恢复和移交已锁定）'
          : binding.state === 'active' && binding.applied_revision === binding.desired_revision ? `已应用 v${binding.applied_revision}`
            : binding.state === 'preparing' ? `正在准备文件 v${binding.desired_revision}`
              : binding.state === 'unbinding' ? '正在解除关联（操作已锁定）'
                : binding.state === 'failed' ? '准备失败（操作已锁定）' : binding.state}</small>)}
        <small className="table-sub">旧数据迁移状态未知</small>
      </div>
      <div><span>运行状态</span><strong>{hostReasonLabel[host.state_reason] || host.state_reason || (host.state === 'running' ? '运行正常' : host.state === 'stopped' ? '可恢复' : '等待状态更新')}</strong></div>
      <div><span>Workspace Runtime</span><strong>{runtimeState}</strong><small className="table-sub">最近观测：{host.workspace_runtime_last_seen_at ? formatDate(host.workspace_runtime_last_seen_at) : '未知'}</small></div>
      <div><span>桌面连接</span><strong>由 noVNC 工作台单独检测</strong></div>
    </div>
    {canBindWorkspace && !hasWorkspaceBinding && <form onSubmit={(event) => { event.preventDefault(); if (selectedWorkspace) onBindWorkspace(selectedWorkspace); }} className="host-workspace-bind">
      <label htmlFor={`bind-workspace-${host.id}`}>恢复前关联已有云端文件</label>
      <select id={`bind-workspace-${host.id}`} value={workspaceID} onChange={(event) => setWorkspaceID(event.target.value)} disabled={bindWorkspacesLoading || bindPending}>
        <option value="">选择已保存的文件</option>
        {bindableWorkspaces.map((workspace) => <option key={workspace.id} value={workspace.id}>{workspace.name} · v{workspace.revision}</option>)}
      </select>
      <button type="submit" className="button button-secondary" disabled={!selectedWorkspace || bindPending}>{bindPending ? '关联中…' : '关联所选文件'}</button>
      {bindWorkspacesLoading && <span className="muted" role="status">正在读取可关联的文件…</span>}
      {bindWorkspacesError && <FloatingError className="error-inline" role="alert">无法读取文件：{bindWorkspacesError}</FloatingError>}
      {!bindWorkspacesLoading && !bindWorkspacesError && !bindableWorkspaces.length && <span className="muted">没有可关联的已保存文件。</span>}
    </form>}
    {canEnter && <HostMemberManager hostId={host.id} />}
    {canEnter && host.state === 'running' && <HostControlPanel host={host} canControl canReleaseAnyLease />}
      </div>}
    </section>
  </article>;
}

function HostControlDisclosure({ host, canControl, canReleaseAnyLease }: {
  host: { id: string; name?: string; control_state?: Host['control_state'] }; canControl: boolean; canReleaseAnyLease: boolean;
}) {
  const [open, setOpen] = useState(false);
  return <details className="host-secondary-details host-shared-control" open={open}>
    <summary onClick={(event) => { event.preventDefault(); setOpen((value) => !value); }}>查看控制权状态</summary>
    {open && <HostControlPanel host={host} canControl={canControl} canReleaseAnyLease={canReleaseAnyLease} />}
  </details>;
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
    {control.isError && <FloatingError className="error-inline" role="alert">{errorDetails(control.error).message}</FloatingError>}
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
