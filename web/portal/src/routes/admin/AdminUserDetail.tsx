import { Link, useParams } from 'react-router';
import { useInfiniteQuery, useQuery } from '@tanstack/react-query';
import { errorDetails, portalApi } from '../../app/api';
import { formatBytes, formatCpu, formatDate, formatMemory, hostStateLabel } from '../../app/format';

export function AdminUserDetail() {
  const { id = '' } = useParams();
  const users = useQuery({ queryKey: ['admin', 'users'], queryFn: () => portalApi.adminUsers() });
  const hosts = useQuery({ queryKey: ['admin', 'hosts'], queryFn: portalApi.adminHosts });
  const workspaces = useInfiniteQuery({
    queryKey: ['admin', 'workspaces', 'user', id],
    queryFn: ({ pageParam }) => portalApi.adminWorkspaces(pageParam),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (page) => page.next_cursor ?? undefined,
  });
  if (users.isPending || hosts.isPending || workspaces.isPending) return <div className="loading-card" role="status">正在读取用户信息…</div>;
  if (users.isError || hosts.isError || (workspaces.isError && !workspaces.data)) {
    const error = users.isError ? users.error : hosts.isError ? hosts.error : workspaces.error;
    return <div className="error-card" role="alert">{errorDetails(error).message}<button className="button button-secondary" type="button" onClick={() => { void users.refetch(); void hosts.refetch(); void workspaces.refetch(); }}>重试</button></div>;
  }
  const user = users.data.items.find((item) => item.id === id);
  if (!user) return <div className="empty-card"><h2>找不到这个用户</h2><p>用户可能已被移除，或当前账号无权查看。</p><Link className="button button-secondary" to="/admin/users">返回用户列表</Link></div>;
  const ownedHosts = hosts.data.items.filter((host) => host.owner_id === user.id);
  const ownedWorkspaces = workspaces.data.pages.flatMap((page) => page.items).filter((workspace) => workspace.owner_id === user.id);

  return <section>
    <Link className="back-link" to="/admin/users">‹ 返回用户列表</Link>
    <div className="page-heading"><div><p className="eyebrow">用户详情</p><h1>{user.name}</h1><p className="muted mono">{user.id}</p></div><span className={`status status-${user.status === 'approved' ? 'ready' : 'disabled'}`}>{user.status === 'approved' ? '登录正常' : user.status === 'disabled' ? '已禁用登录' : user.status === 'pending' ? '等待审核' : '审核未通过'}</span></div>
    <div className="admin-detail-facts">
      <article><span>角色</span><strong>{user.role === 'super_admin' ? '超级管理员' : user.role === 'admin' ? '管理员' : '普通用户'}</strong></article>
      <article><span>云端电脑</span><strong>{ownedHosts.length}{user.quota ? ` / ${user.quota.hosts} 台` : ' 台'}</strong></article>
      <article><span>CPU 使用 / 配额</span><strong>{user.usage && user.quota ? `${formatCpu(user.usage.cpu_milli)} / ${formatCpu(user.quota.cpu_milli)}` : '未提供'}</strong></article>
      <article><span>内存使用 / 配额</span><strong>{user.usage && user.quota ? `${formatMemory(user.usage.mem_mib)} / ${formatMemory(user.quota.mem_mib)}` : '未提供'}</strong></article>
      <article><span>创建时间</span><strong>{formatDate(user.created_at)}</strong></article>
    </div>
    <section className="admin-detail-section">
      <div className="dashboard-section-heading"><div><h2>所属云端电脑</h2><p>这里显示该用户拥有的电脑及当前运行状态。</p></div><Link to="/admin/hosts">全部电脑</Link></div>
      {!ownedHosts.length ? <div className="empty-card compact"><p>该用户还没有云端电脑。</p></div> : <div className="table-wrap"><table><thead><tr><th>电脑</th><th>运行状态</th><th>规格</th><th>绑定工作区</th><th>创建时间</th></tr></thead><tbody>
        {ownedHosts.map((host) => <tr key={host.id}><td><Link className="admin-table-link" to={`/admin/hosts/${encodeURIComponent(host.id)}`}><strong>{host.name || host.id}</strong><small className="table-sub mono">{host.id}</small></Link></td><td><span className={`status status-${host.state}`}>{hostStateLabel[host.state] || host.state}</span></td><td>{formatCpu(host.resources.cpu_limit_milli)} / {formatMemory(host.resources.mem_limit_mib)}</td><td>{host.workspaces.length ? host.workspaces.map((binding) => binding.workspace.name || binding.workspace.id).join('、') : '未绑定'}</td><td>{formatDate(host.created_at)}</td></tr>)}
      </tbody></table></div>}
    </section>
    <section className="admin-detail-section">
      <div className="dashboard-section-heading"><div><h2>所属工作区</h2><p>查看此用户的工作区版本、用量和绑定状态。</p></div><Link to={`/admin/workspaces?owner=${encodeURIComponent(user.id)}`}>工作区列表</Link></div>
      {!ownedWorkspaces.length ? <div className="empty-card compact"><p>{workspaces.hasNextPage ? '已加载的记录中暂未找到该用户的工作区，可继续查找。' : '该用户还没有工作区。'}</p></div> : <div className="table-wrap"><table><thead><tr><th>工作区</th><th>状态</th><th>用量</th><th>版本</th><th>绑定电脑</th><th>最近更新</th></tr></thead><tbody>
        {ownedWorkspaces.map((workspace) => <tr key={workspace.id}><td><Link className="admin-table-link" to={`/admin/workspaces/${encodeURIComponent(workspace.id)}`}><strong>{workspace.name || workspace.id}</strong><small className="table-sub mono">{workspace.id}</small></Link></td><td><span className={`status status-${workspace.state}`}>{workspace.state === 'ready' ? '可用' : '删除中'}</span></td><td>{formatBytes(workspace.used_bytes)} / {formatBytes(workspace.quota_bytes)}</td><td>v{workspace.revision}</td><td>{workspace.bindings.length ? workspace.bindings.map((binding) => <Link className="admin-binding-link" to={`/admin/hosts/${encodeURIComponent(binding.host_id)}`} key={binding.host_id}>{binding.host_name || binding.host_id}<small>{binding.state === 'active' && binding.applied_revision === binding.desired_revision ? '已应用' : binding.state === 'preparing' ? '正在应用' : binding.state}</small></Link>) : '未绑定'}</td><td>{formatDate(workspace.updated_at)}</td></tr>)}
      </tbody></table></div>}
      {workspaces.isFetchNextPageError && <div className="error-card admin-pagination-error" role="alert">{errorDetails(workspaces.error).message}<button className="button button-secondary" type="button" onClick={() => void workspaces.fetchNextPage()}>重试</button></div>}
      {workspaces.hasNextPage && !workspaces.isFetchNextPageError && <button className="button button-secondary admin-load-more" type="button" disabled={workspaces.isFetchingNextPage} onClick={() => void workspaces.fetchNextPage()}>{workspaces.isFetchingNextPage ? '正在加载…' : ownedWorkspaces.length ? '加载更多工作区' : '继续查找工作区'}</button>}
    </section>
    <p className="notice notice-info admin-detail-note">管理后台展示账号和资源元数据。禁用登录不代表云端电脑、任务或工作区已经停止或隔离。</p>
  </section>;
}
