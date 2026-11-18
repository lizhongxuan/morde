import { useMemo, useState } from 'react';
import { Link, useLocation } from 'react-router';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { errorDetails, enterHostInNewTab, newIdempotencyKey, portalApi } from '../app/api';
import type { HostMemberPermission } from '../app/api';
import { useAuth } from '../app/auth';
import { formatCpu, formatDate, formatMemory, hostStateLabel } from '../app/format';
import { useToast } from '../app/toast';
import { useNetworkReadNotice } from '../app/use-network-read-notice';
import { isTransientNetworkError } from '../api/errors';
import { ConfirmDialog } from '../components/ConfirmDialog';
import { HostControlPanel } from '../components/HostControlPanel';
import { HostMemberManager } from '../components/HostMemberManager';
import { MordeIcon } from '../components/MordeIcon';
import { ScreenPagination } from '../components/ScreenPagination';
import { ApiKeys } from './ApiKeys';
import './Hosts.css';
import { FloatingError } from '../components/FloatingError';

type Host = Awaited<ReturnType<typeof portalApi.hosts>>['items'][number];
const unsettled = new Set(['creating', 'stopping', 'deleting']);
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
  const requestedView = new URLSearchParams(location.search).get('view');
  const isApiKeyView = requestedView === 'apikeys';
  const hosts = useQuery({
    queryKey: ['hosts'], queryFn: portalApi.hosts,
    enabled: !isApiKeyView,
    refetchInterval: (query) => query.state.data?.items.some((host) => unsettled.has(host.state)) ? 3000 : query.state.data?.items.length ? 12000 : false,
    refetchIntervalInBackground: false,
  });
  const preferences = useQuery({ queryKey: ['user-preferences'], queryFn: portalApi.preferences, retry: false, enabled: !isApiKeyView });
  const ownedHosts = useMemo(() => hosts.data?.items.filter((host) => host.owner_id === user?.id) ?? [], [hosts.data?.items, user?.id]);
  const defaultHost = ownedHosts.find((host) => host.id === preferences.data?.default_host_id);
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
  const displayedHosts = useMemo(() => prioritizeHostForHash(ownedHosts, hashHostID), [ownedHosts, hashHostID]);
  const atLimit = !!user && ownedHosts.length >= user.quota.hosts;
  const [deleting, setDeleting] = useState<Host | null>(null);
  const [lifecycleAction, setLifecycleAction] = useState<{ host: Host; action: 'release' | 'restore' } | null>(null);
  const [copyDialog, setCopyDialog] = useState<{ host: Host; name: string; idempotencyKey: string } | null>(null);
  const update = useMutation({
    mutationFn: async ({ action, host }: { action: 'renew' | 'delete' | 'release' | 'restore'; host: Host }) => {
      if (action === 'renew') return portalApi.renewHost(host.id);
      if (action === 'release') return portalApi.releaseHost(host.id, true);
      if (action === 'restore') return portalApi.restoreHost(host.id);
      return portalApi.deleteHost(host.id, true);
    },
    onSuccess: (_, variables) => {
      void queryClient.invalidateQueries({ queryKey: ['hosts'] });
      if (variables.action === 'delete') {
        notify('已提交删除请求；云端电脑和专属资料目录会在后台一并清理。', 'success'); setDeleting(null);
      }
      else if (variables.action === 'release') { notify('已提交释放请求，文件和工作目录会保留。', 'success'); setLifecycleAction(null); }
      else if (variables.action === 'restore') { notify('已提交恢复请求。', 'success'); setLifecycleAction(null); }
      else notify('主机已续期。', 'success');
    },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });
  const copyHost = useMutation({
    mutationFn: ({ host, name, idempotencyKey }: { host: Host; name: string; idempotencyKey: string }) => {
      const binding = host.workspaces.find((item) => item.state !== 'detached');
      if (!binding) throw new Error('找不到可复制的云端文件版本。');
      return portalApi.copyHost(host.id, { name: name.trim(), expected_current_revision: binding.workspace.revision }, idempotencyKey);
    },
    onSuccess: async (result) => {
      await queryClient.invalidateQueries({ queryKey: ['hosts'] });
      notify(`已创建云端电脑“${result.host.name || result.host.id}”。`, 'success');
      setCopyDialog(null);
    },
    onError: async (error) => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ['hosts'] }),
      ]);
      notify(`${errorDetails(error).message} 若结果不确定，请保持名称不变并在此窗口重试。`, 'error');
    },
  });
  const enter = async (host: Host) => {
    try { await enterHostInNewTab(host.id); }
    catch (error) { notify(errorDetails(error).message, 'error'); }
  };

  const sharedHosts = useQuery({ queryKey: ['shared-hosts'], queryFn: portalApi.sharedHosts, enabled: !isApiKeyView });
  const displayedSharedHosts = useMemo(() => prioritizeHostForHash(sharedHosts.data?.items ?? [], hashHostID), [sharedHosts.data?.items, hashHostID]);
  const listMode = requestedView === 'owned' ? 'owned' : requestedView === 'shared' ? 'shared'
    : hashHostID && sharedHosts.data?.items.some((host) => host.id === hashHostID) ? 'shared'
      : hashHostID && ownedHosts.some((host) => host.id === hashHostID) ? 'owned'
        : hosts.isSuccess && !ownedHosts.length && sharedHosts.data?.items.length ? 'shared' : 'owned';
  useNetworkReadNotice([
    { failed: listMode === 'owned' && hosts.isError, error: hosts.error, retry: hosts.refetch },
    { failed: listMode === 'owned' && preferences.isError, error: preferences.error, retry: preferences.refetch },
    { failed: listMode === 'shared' && sharedHosts.isError, error: sharedHosts.error, retry: sharedHosts.refetch },
  ], '云端电脑信息读取失败，已保留当前内容。');
  const enterShared = async (host: Awaited<ReturnType<typeof portalApi.sharedHosts>>['items'][number], entryTarget: 'desktop' | 'terminal' | 'workspace') => {
    try { enterHostInNewTab(host.id, entryTarget); }
    catch (error) { notify(errorDetails(error).message, 'error'); }
  };

  return <section className="hosts-page">
    <header className="hosts-page-heading">
      {isApiKeyView ? <div><h2 id="api-keys-title">API Key</h2><p>连接 AI 对话工具并管理密钥。</p></div>
        : listMode === 'owned' ? <><div><h2 id="my-hosts-title">我的电脑</h2><p>管理你的云端电脑、文件和运行状态。</p></div><div className="inline-actions">{atLimit ? <button type="button" className="button button-primary" onClick={() => notify('达到云端电脑上限,无法再创建', 'error')}>＋ 创建电脑</button> : <Link className="button button-primary" to="/hosts/new">＋ 创建电脑</Link>}</div></>
        : <div><h2 id="shared-hosts-title">共享给我的电脑</h2><p>查看其他人共享给你的云端电脑。</p></div>}
    </header>
    <nav className="inline-actions hosts-list-toggle" aria-label="云端电脑视图">
      <Link className={`button ${!isApiKeyView && listMode === 'owned' ? 'button-primary' : 'button-secondary'}`} aria-current={!isApiKeyView && listMode === 'owned' ? 'page' : undefined} to="/hosts?view=owned">我的电脑</Link>
      <Link className={`button ${!isApiKeyView && listMode === 'shared' ? 'button-primary' : 'button-secondary'}`} aria-current={!isApiKeyView && listMode === 'shared' ? 'page' : undefined} to="/hosts?view=shared">共享给我的</Link>
      <Link className={`button ${isApiKeyView ? 'button-primary' : 'button-secondary'}`} aria-current={isApiKeyView ? 'page' : undefined} to="/hosts?view=apikeys">API Key</Link>
    </nav>
    {isApiKeyView ? <div className="hosts-api-keys"><ApiKeys /></div>
      : listMode === 'owned' ? <section className="design-panel hosts-panel" aria-labelledby="my-hosts-title">
    <h3 className="hosts-list-title">电脑列表</h3>
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
        <span>电脑数据目录</span>
        <strong>{defaultHost ? '~/workspace/（随电脑持久保存）' : '选择电脑后显示'}</strong>
        {defaultHost && <Link to="/hosts?sync=1">文件同步</Link>}
      </div>
      {preferences.isError && !isTransientNetworkError(preferences.error) && <FloatingError className="error-inline" role="alert">默认设置暂不可用。
        <button type="button" className="button button-small button-secondary" onClick={() => void preferences.refetch()}>重试</button>
      </FloatingError>}
    </div>}
    {hosts.isPending || (!hosts.data && hosts.isError && isTransientNetworkError(hosts.error)) ? <div className="loading-card" aria-live="polite">正在加载主机…</div> : hosts.isError && !isTransientNetworkError(hosts.error) ? <FloatingError className="error-card" role="alert">{errorDetails(hosts.error).message}<button type="button" className="button button-secondary" onClick={() => void hosts.refetch()}>重试</button></FloatingError> : !ownedHosts.length ? <div className="empty-card hosts-empty"><span className="hosts-empty-icon" aria-hidden="true"><MordeIcon name="monitor" /></span><h3>还没有主机</h3><p>创建一台主机开始使用浏览器、终端和文件工具。</p></div> : <ScreenPagination
      items={displayedHosts}
      getKey={(host) => host.id}
      estimatedItemHeight={72}
      height={560}
      gap={10}
      className="hosts-pagination"
      ariaLabel="我的电脑"
      renderItem={(host) => {
        const ownsHost = host.owner_id === user?.id;
        const filesReady = hasReadyComputerFiles(host);
        const filesRestorable = hasRestorableComputerFiles(host);
        const blockedFiles = host.workspaces.find((binding) => binding.state !== 'detached' && (
          binding.state !== 'active' || binding.workspace.state !== 'ready' || binding.sync_state !== 'healthy'
          || binding.sync_conflicts.length > 0 || binding.applied_revision !== binding.desired_revision
          || binding.applied_revision !== binding.workspace.revision
        ));
        const copyReason = atLimit ? undefined
          : host.state !== 'stopped' ? '请先停止源电脑。'
            : host.control_state !== 'ai' ? '请先将电脑控制权交还给云端 AI。'
              : !filesReady ? '源电脑文件尚未全部确认或仍有冲突。' : undefined;
        return <HostCard key={host.id} host={host} isDefaultHost={host.id === preferences.data?.default_host_id} busy={update.isPending && update.variables?.host.id === host.id}
          canEnter={ownsHost && filesReady}
          enterReason={!ownsHost ? '仅电脑所有者可以进入工作环境。' : blockedFiles
            ? '电脑文件正在准备、同步或存在冲突；打开和移交操作暂不可用。'
            : !filesReady ? '电脑文件尚未准备好，暂不能打开。' : undefined}
          filesRestorable={filesRestorable}
          canShowCopy={ownsHost} canCopy={ownsHost && !atLimit && !copyReason} copyReason={ownsHost ? copyReason : undefined}
          copyPending={copyHost.isPending && copyHost.variables?.host.id === host.id}
          onCopy={() => setCopyDialog({ host, name: `${(host.name || '云端电脑').slice(0, 96)} 副本`, idempotencyKey: newIdempotencyKey() })}
          onEnter={() => void enter(host)} onRenew={() => update.mutate({ action: 'renew', host })} onRelease={() => setLifecycleAction({ host, action: 'release' })} onRestore={() => setLifecycleAction({ action: 'restore', host })} onDelete={() => setDeleting(host)} />;
      }}
    />}
    </section> : <section className="shared-hosts-section design-panel" aria-labelledby="shared-hosts-title">
      <h3 className="hosts-list-title">电脑列表</h3>
      {sharedHosts.isPending || (!sharedHosts.data && sharedHosts.isError && isTransientNetworkError(sharedHosts.error)) ? <div className="loading-card" role="status">正在加载共享电脑…</div>
        : sharedHosts.isError && !isTransientNetworkError(sharedHosts.error) ? <FloatingError className="error-card" role="alert">{errorDetails(sharedHosts.error).message}<button type="button" className="button button-secondary" onClick={() => void sharedHosts.refetch()}>重试</button></FloatingError>
          : !sharedHosts.data?.items.length ? <div className="empty-card hosts-empty"><span className="hosts-empty-icon" aria-hidden="true"><MordeIcon name="monitor" /></span><h3>还没有共享电脑</h3><p>还没有人与你共享云端电脑。</p></div>
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
                {host.permissions.includes('mcp:control') && <Link className="button button-secondary" to="/hosts?view=apikeys">配置 MCP API Key</Link>}
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
    {deleting && <ConfirmDialog title={`永久删除云端电脑 ${deleting.name || deleting.id}`} description="继续后会永久删除这台电脑及其专属 ~/workspace/ 目录（包括项目文件和其他电脑文件）。此操作无法恢复。本机 PC 文件和其他云端电脑不受影响。" confirmLabel="永久删除电脑和数据" danger busy={update.isPending} onClose={() => setDeleting(null)} onConfirm={() => update.mutate({ action: 'delete', host: deleting })} />}
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

function HostCard({ host, isDefaultHost, busy, canEnter, enterReason, filesRestorable, canShowCopy, canCopy, copyReason, copyPending, onCopy, onEnter, onRenew, onRelease, onRestore, onDelete }: {
  host: Host; isDefaultHost: boolean; busy: boolean; canEnter: boolean; enterReason?: string; filesRestorable: boolean; canShowCopy: boolean; canCopy: boolean; copyReason?: string; copyPending: boolean;
  onCopy: () => void; onEnter: () => void; onRenew: () => void; onRelease: () => void; onRestore: () => void; onDelete: () => void;
}) {
  const [detailsOpen, setDetailsOpen] = useState(false);
  const name = host.name || host.id;
  const enterHelpID = `host-enter-help-${host.id}`;
  const copyHelpID = `host-copy-help-${host.id}`;
  const runtimeState = host.workspace_runtime_online === true ? '在线' : host.workspace_runtime_online === false ? '离线' : '未知';
  const fileBindings = host.workspaces.filter((binding) => binding.state !== 'detached');
  const fileBinding = fileBindings[0];
  const fileState = !fileBinding ? '专属目录尚未确认' : fileBindings.length !== 1 ? '电脑文件状态待核对'
    : fileBinding.sync_state === 'conflict' || fileBinding.sync_conflicts.length
    ? '文件同步冲突（打开、恢复和移交已锁定）'
    : fileBinding.state === 'active' && fileBinding.workspace.state === 'ready' && fileBinding.sync_state === 'healthy'
      && fileBinding.applied_revision === fileBinding.desired_revision && fileBinding.applied_revision === fileBinding.workspace.revision ? `已就绪 · v${fileBinding.applied_revision}`
      : fileBinding.state === 'preparing' ? `正在准备文件 v${fileBinding.desired_revision}`
        : fileBinding.state === 'failed' ? '电脑文件准备失败'
          : fileBinding.workspace.state !== 'ready' ? '电脑专属目录暂不可用' : '电脑文件状态：' + fileBinding.state;
  return <article className="host-card host-card-compact" id={`host-${host.id}`}>
    <div className="host-card-heading"><h2>{host.name || '未命名主机'}</h2><span className={`status status-${host.state}`}><span aria-hidden="true" className="status-dot" />{hostStateLabel[host.state] || host.state}</span></div>
    <div className="host-actions host-primary-actions">
      <button type="button" className="button button-primary" onClick={onEnter} disabled={!canEnter || host.state !== 'running' || busy || host.control_state === 'taking_over' || host.control_state === 'returning'} aria-label={`${host.control_state === 'user' ? '操作' : host.control_state === 'taking_over' || host.control_state === 'returning' ? '控制切换中' : '查看'}云端电脑 ${name}`}>{host.control_state === 'user' ? '操作电脑' : host.control_state === 'taking_over' || host.control_state === 'returning' ? '控制切换中' : '查看电脑'}</button>
      <details className="host-more-actions">
        <summary aria-label={`更多操作 ${name}`}>更多操作</summary>
        <div>
          {isDefaultHost ? <Link className="button button-secondary" to="/hosts?sync=1">项目目录与同步</Link>
            : <span className="muted">本机文件同步固定使用默认电脑</span>}
          {!canEnter && <p className="host-action-reason" id={enterHelpID} role="status">{enterReason}</p>}
          <button type="button" className="button button-secondary" aria-controls={`host-details-${host.id}`} aria-expanded={detailsOpen} onClick={(event) => {
            setDetailsOpen((value) => !value);
            const actions = event.currentTarget.closest<HTMLDetailsElement>('.host-more-actions');
            if (actions) { actions.open = false; actions.querySelector('summary')?.focus(); }
          }}>{detailsOpen ? '收起详情与管理' : '详情与管理'}</button>
          <button type="button" className="button button-secondary" onClick={onRenew} disabled={host.state !== 'running' || busy} aria-label={`续期主机 ${name}`}>续期</button>
          {host.state === 'running' && <button type="button" className="button button-secondary" onClick={onRelease} disabled={busy} aria-label={`释放运行资源 ${name}`}>释放运行资源</button>}
          {host.state === 'stopped' && <button type="button" className="button button-primary" onClick={onRestore} disabled={busy || !filesRestorable} aria-label={`恢复云端电脑 ${name}`} title={!filesRestorable ? '电脑专属数据目录尚未准备好' : undefined}>恢复</button>}
          {host.state === 'stopped' && !filesRestorable && <span className="muted" role="status">电脑专属数据目录尚未准备好，暂不能恢复。</span>}
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
      <div><span>电脑专属文件</span><strong>{fileState}</strong>{isDefaultHost && <Link to="/hosts?sync=1">管理本机项目同步</Link>}</div>
      <div><span>运行状态</span><strong>{hostReasonLabel[host.state_reason] || host.state_reason || (host.state === 'running' ? '运行正常' : host.state === 'stopped' ? '可恢复' : '等待状态更新')}</strong></div>
      <div><span>电脑文件服务</span><strong>{runtimeState}</strong><small className="table-sub">最近观测：{host.workspace_runtime_last_seen_at ? formatDate(host.workspace_runtime_last_seen_at) : '未知'}</small></div>
      <div><span>桌面连接</span><strong>由 noVNC 工作台单独检测</strong></div>
    </div>
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

function hasReadyComputerFiles(host: Host) {
  const bindings = host.workspaces.filter((binding) => binding.state !== 'detached');
  const binding = bindings[0];
  return bindings.length === 1 && binding?.state === 'active' && binding.workspace.state === 'ready' &&
    binding.sync_state === 'healthy' && binding.sync_conflicts.length === 0 && binding.applied_revision === binding.desired_revision &&
    binding.applied_revision === binding.workspace.revision;
}

function hasRestorableComputerFiles(host: Host) {
  const bindings = host.workspaces.filter((binding) => binding.state !== 'detached');
  const binding = bindings[0];
  return bindings.length === 1 && !!binding && binding.workspace.state === 'ready'
    && binding.workspace.revision === binding.desired_revision && binding.sync_state === 'healthy' && binding.sync_conflicts.length === 0
    && (binding.state === 'preparing' || (binding.state === 'active' && binding.applied_revision === binding.desired_revision));
}
