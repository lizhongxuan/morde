import { useState } from 'react';
import { Link } from 'react-router';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { errorDetails, enterHostInNewTab, portalApi } from '../app/api';
import type { HostMemberPermission } from '../app/api';
import { useAuth } from '../app/auth';
import { formatCpu, formatDate, formatMemory, hostStateLabel } from '../app/format';
import { useToast } from '../app/toast';
import { ConfirmDialog } from '../components/ConfirmDialog';

type Host = Awaited<ReturnType<typeof portalApi.hosts>>['items'][number];
const unsettled = new Set(['creating', 'stopping', 'deleting']);
const sharingPermissionLabels: Record<HostMemberPermission, string> = {
  'desktop:view': '查看桌面',
  'desktop:control': '操作桌面',
  'terminal:control': '终端操作',
  'mcp:control': 'MCP 操作',
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
  const enter = async (host: Host) => {
    try { await enterHostInNewTab(host.id); }
    catch (error) { notify(errorDetails(error).message, 'error'); }
  };

  const sharedHosts = useQuery({ queryKey: ['shared-hosts'], queryFn: portalApi.sharedHosts });
  const enterShared = async (host: Awaited<ReturnType<typeof portalApi.sharedHosts>>['items'][number], entryTarget: 'desktop' | 'terminal') => {
    try { enterHostInNewTab(host.id, entryTarget); }
    catch (error) { notify(errorDetails(error).message, 'error'); }
  };

  return <section>
    <div className="page-heading"><div><p className="eyebrow">你的云端电脑</p><h1>云端电脑</h1><p className="muted">电脑与文件都保存在你的云端空间。</p></div><div className="inline-actions"><Link className="button button-secondary" to="/workspaces">工作目录</Link>{atLimit ? <button type="button" className="button button-primary" onClick={() => notify('达到云端电脑上限,无法再创建', 'error')}>创建电脑</button> : <Link className="button button-primary" to="/hosts/new">创建电脑</Link>}</div></div>
    {hosts.isPending ? <div className="loading-card" aria-live="polite">正在加载主机…</div> : hosts.isError ? <div className="error-card" role="alert">{errorDetails(hosts.error).message}<button type="button" className="button button-secondary" onClick={() => void hosts.refetch()}>重试</button></div> : !hosts.data?.items.length ? <div className="empty-card"><h2>还没有主机</h2><p>创建一台主机开始使用浏览器、终端和文件工具。</p><Link to="/hosts/new" className="button button-primary">创建第一台主机</Link></div> : <div className="host-list">
      {hosts.data.items.map((host) => {
        return <HostCard key={host.id} host={host} busy={update.isPending && update.variables?.host.id === host.id} canEnter={host.owner_id === user?.id} onEnter={() => void enter(host)} onRenew={() => update.mutate({ action: 'renew', host })} onRelease={() => setLifecycleAction({ host, action: 'release' })} onRestore={() => setLifecycleAction({ host, action: 'restore' })} onDelete={() => setDeleting(host)} />;
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
              return <article className="host-card" key={host.id}>
              <div className="host-card-heading"><div><h3>{host.name || '未命名主机'}</h3><p className="muted mono">{host.id}</p></div><span className={`status status-${host.state}`}><span aria-hidden="true" className="status-dot" />{hostStateLabel[host.state] || host.state}</span></div>
              <p className="muted">权限：{host.permissions.map((permission) => sharingPermissionLabels[permission]).join('、') || '无'}</p>
              <div className="host-actions">
                {canViewDesktop && <button type="button" className="button button-primary" disabled={host.state !== 'running'} onClick={() => void enterShared(host, 'desktop')}>{host.state === 'running' ? '查看桌面' : '主机运行后可查看'}</button>}
                {canControlTerminal && <button type="button" className="button button-secondary" disabled={host.state !== 'running'} onClick={() => void enterShared(host, 'terminal')}>{host.state === 'running' ? '打开终端' : '主机运行后可打开终端'}</button>}
                {host.permissions.includes('mcp:control') && <Link className="button button-secondary" to="/apikeys">配置 MCP API Key</Link>}
              </div>
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

function HostCard({ host, busy, canEnter, onEnter, onRenew, onRelease, onRestore, onDelete }: {
  host: Host; busy: boolean; canEnter: boolean; onEnter: () => void; onRenew: () => void; onRelease: () => void; onRestore: () => void; onDelete: () => void;
}) {
  const name = host.name || host.id;
  const enterHelpID = `host-enter-help-${host.id}`;
  return <article className="host-card">
    <div className="host-card-heading"><div><h2>{host.name || '未命名主机'}</h2><p className="muted mono">{host.id} · {host.template_id}</p></div><span className={`status status-${host.state}`}><span aria-hidden="true" className="status-dot" />{hostStateLabel[host.state] || host.state}</span></div>
    <div className="host-facts">
      <div><span>规格</span><strong>{formatCpu(host.resources?.cpu_limit_milli)} / {formatMemory(host.resources?.mem_limit_mib)}</strong></div>
      <div><span>到期时间</span><strong>{formatDate(host.expires_at)}</strong></div>
      <div><span>工作区</span><strong>{host.workspaces.length ? host.workspaces.map((binding) => binding.workspace.name || binding.workspace.id).join('、') : '无'}</strong>
        {host.workspaces.map((binding) => <small className="table-sub" key={binding.workspace.id}>{binding.state === 'active' && binding.applied_revision === binding.desired_revision ? `已应用 v${binding.applied_revision}` : binding.state === 'preparing' ? `正在应用 v${binding.desired_revision}` : binding.state === 'unbinding' ? '正在解绑' : binding.state === 'failed' ? '应用失败' : binding.state}</small>)}
      </div>
      <div><span>运行状态</span><strong>{hostReasonLabel[host.state_reason] || host.state_reason || (host.state === 'running' ? '运行正常' : host.state === 'stopped' ? '可恢复' : '等待状态更新')}</strong></div>
    </div>
    <div className="host-actions">
      <button type="button" className="button button-primary" onClick={onEnter} disabled={!canEnter || host.state !== 'running' || busy} aria-label={`进入主机 ${name}`} aria-describedby={!canEnter ? enterHelpID : undefined}>进入</button>
      {!canEnter && <span className="muted" id={enterHelpID}>仅主机所有者可以进入工作环境。</span>}
      <button type="button" className="button button-secondary" onClick={onRenew} disabled={host.state !== 'running' || busy} aria-label={`续期主机 ${name}`}>续期</button>
      {host.state === 'running' && <button type="button" className="button button-secondary" onClick={onRelease} disabled={busy} aria-label={`释放运行资源 ${name}`}>释放运行资源</button>}
      {host.state === 'stopped' && <button type="button" className="button button-primary" onClick={onRestore} disabled={busy} aria-label={`恢复主机 ${name}`}>恢复</button>}
      <button type="button" className="button button-danger-quiet" onClick={onDelete} disabled={host.state === 'deleting' || host.state === 'creating' || host.state === 'stopping' || busy} aria-label={`删除主机 ${name}`}>删除</button>
    </div>
  </article>;
}
