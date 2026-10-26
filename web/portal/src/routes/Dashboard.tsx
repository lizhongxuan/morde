import { Link } from 'react-router';
import { useQuery } from '@tanstack/react-query';
import { errorDetails, enterHostInNewTab, portalApi } from '../app/api';
import { useAuth } from '../app/auth';
import { formatBytes, formatDate, formatMemory, hostStateLabel } from '../app/format';
import { useToast } from '../app/toast';
import { isActiveTaskStatus, taskStatusLabel } from './Tasks';

export function Dashboard() {
  const { user } = useAuth();
  const { notify } = useToast();
  const hosts = useQuery({ queryKey: ['hosts'], queryFn: portalApi.hosts });
  const workspaces = useQuery({ queryKey: ['workspaces'], queryFn: portalApi.workspaces });
  const tasks = useQuery({ queryKey: ['tasks', 'dashboard'], queryFn: () => portalApi.tasks() });
  const hostItems = hosts.data?.items ?? [];
  const workspaceItems = workspaces.data?.items ?? [];
  const taskItems = tasks.data?.items ?? [];
  const activeTasks = taskItems.filter((task) => isActiveTaskStatus(task.status));
  const needsAttentionTask = taskItems.find((task) => task.status === 'waiting_user');
  const runningHosts = hostItems.filter((host) => host.state === 'running');
  const recentTasks = taskItems.slice(0, 5);

  const openHost = async (id: string) => {
    try { await enterHostInNewTab(id); }
    catch (error) { notify(errorDetails(error).message, 'error'); }
  };

  return <section className="dashboard-page">
    <div className="page-heading dashboard-heading">
      <div><p className="eyebrow">MORDE · PC 工作台</p><h1>工作台</h1><p className="muted">在浏览器中管理云端电脑、工作区和 AI 任务。</p></div>
      <div className="inline-actions"><Link className="button button-secondary" to="/workspaces">查看工作区</Link><Link className="button button-primary" to="/assistant">交办新任务</Link></div>
    </div>

    {needsAttentionTask && <section className="dashboard-priority" aria-label="需要你处理的任务">
      <div><span className="status status-waiting_user"><span className="status-dot" aria-hidden="true" />需要你处理</span><h2>{needsAttentionTask.objective}</h2><p>这项云端任务正在等待你确认或补充信息。</p></div>
      <Link className="button button-primary" to={`/tasks/${encodeURIComponent(needsAttentionTask.id)}`}>查看任务</Link>
    </section>}

    <section className="dashboard-summary" aria-label="当前资源概况">
      <article className="dashboard-metric"><span>云端电脑</span><strong>{hosts.isPending ? '…' : hosts.isError ? '—' : runningHosts.length}<small> / {hostItems.length} 台运行中</small></strong>{hosts.isError && <small className="dashboard-metric-error">{errorDetails(hosts.error).message}</small>}</article>
      <article className="dashboard-metric"><span>工作区</span><strong>{workspaces.isPending ? '…' : workspaces.isError ? '—' : workspaceItems.length}<small> 个已保存</small></strong>{!workspaces.isError && !workspaces.isPending && <small>逻辑用量 {formatBytes(workspaceItems.reduce((total, item) => total + item.used_bytes, 0))}</small>}{workspaces.isError && <small className="dashboard-metric-error">{errorDetails(workspaces.error).message}</small>}</article>
      <article className="dashboard-metric"><span>进行中的任务</span><strong>{tasks.isPending ? '…' : tasks.isError ? '—' : activeTasks.length}<small> 项</small></strong>{tasks.isError && <small className="dashboard-metric-error">{errorDetails(tasks.error).message}</small>}</article>
    </section>

    <section className="dashboard-hero">
      <div><span className="dashboard-hero-kicker">云端工作空间</span><h2>从已保存的工作区继续</h2><p>Morde 当前支持管理云端电脑、保存服务端工作区，并跟进云端任务。本机文件夹同步尚未开放。</p><div className="inline-actions"><Link className="button button-primary" to="/hosts">查看云端电脑</Link><Link className="button button-secondary" to="/workspaces">管理工作区</Link></div></div>
      <div className="dashboard-hero-art" aria-hidden="true"><span>W</span><i>↔</i><span>M</span></div>
    </section>

    <div className="dashboard-columns">
      <section className="dashboard-section">
        <div className="dashboard-section-heading"><div><h2>云端电脑</h2><p>进入一台运行中的电脑，继续使用已绑定的工作区。</p></div><Link to="/hosts">全部电脑</Link></div>
        {hosts.isPending ? <div className="loading-card" role="status">正在读取云端电脑…</div>
          : hosts.isError ? <div className="error-card" role="alert">{errorDetails(hosts.error).message}<button className="button button-secondary" type="button" onClick={() => void hosts.refetch()}>重试</button></div>
            : !hostItems.length ? <div className="empty-card compact"><h2>还没有云端电脑</h2><p>创建一台电脑后，可将工作区绑定到云端环境继续工作。</p><Link className="button button-primary" to="/hosts/new">创建电脑</Link></div>
              : <div className="dashboard-host-list">{hostItems.slice(0, 4).map((host) => <article className="dashboard-host-card" key={host.id}>
                <div className="dashboard-host-icon" aria-hidden="true">M</div>
                <div className="dashboard-host-main"><div className="dashboard-host-title"><strong>{host.name || '未命名主机'}</strong><span className={`status status-${host.state}`}><span className="status-dot" aria-hidden="true" />{hostStateLabel[host.state] || host.state}</span></div><small>{host.workspaces.length ? host.workspaces.map((binding) => binding.workspace.name || binding.workspace.id).join('、') : '尚未绑定工作区'} · {formatMemory(host.resources.mem_limit_mib)}</small></div>
                <button className="button button-secondary button-small" type="button" disabled={host.state !== 'running' || host.owner_id !== user?.id} title={host.owner_id !== user?.id ? '共享电脑的访问入口在云端电脑页面' : undefined} onClick={() => void openHost(host.id)}>{host.owner_id !== user?.id ? '共享电脑' : host.state === 'running' ? '进入' : '等待运行'}</button>
              </article>)}</div>}
      </section>

      <section className="dashboard-section">
        <div className="dashboard-section-heading"><div><h2>最近任务</h2><p>查看 AI 任务状态和最近更新。</p></div><Link to="/tasks">全部任务</Link></div>
        {tasks.isPending ? <div className="loading-card" role="status">正在读取任务…</div>
          : tasks.isError ? <div className="error-card" role="alert">{errorDetails(tasks.error).message}<button className="button button-secondary" type="button" onClick={() => void tasks.refetch()}>重试</button></div>
            : !recentTasks.length ? <div className="empty-card compact"><p>还没有任务。描述目标后，Morde 可以在云端电脑上继续处理。</p><Link className="button button-primary" to="/assistant">开始新任务</Link></div>
              : <div className="dashboard-task-list">{recentTasks.map((task) => <Link className="dashboard-task-row" to={`/tasks/${encodeURIComponent(task.id)}`} key={task.id}><span className={`task-step-marker task-step-${task.status}`} aria-hidden="true" /><span className="dashboard-task-copy"><strong>{task.objective}</strong><small>{formatDate(task.updated_at)}</small></span><span className={`status status-${task.status}`}>{taskStatusLabel[task.status] || task.status}</span></Link>)}</div>}
      </section>
    </div>

    {workspaces.isSuccess && workspaceItems.length > 0 && <section className="dashboard-section dashboard-workspace-section">
      <div className="dashboard-section-heading"><div><h2>工作区</h2><p>服务端保存版本与 Host 应用状态分开显示。</p></div><Link to="/workspaces">管理工作区</Link></div>
      <div className="dashboard-workspace-list">{workspaceItems.slice(0, 4).map((workspace) => <Link to="/workspaces" className="dashboard-workspace-card" key={workspace.id}><span className="dashboard-folder-icon" aria-hidden="true">▱</span><span><strong>{workspace.name || workspace.id}</strong><small>{formatBytes(workspace.used_bytes)} · v{workspace.revision}</small></span><span className={`status status-${workspace.state}`}>{workspace.state === 'ready' ? '服务端已保存' : workspace.state}</span></Link>)}</div>
    </section>}
  </section>;
}
