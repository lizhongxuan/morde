import { Link, useParams } from 'react-router';
import { useQuery } from '@tanstack/react-query';
import { errorDetails, portalApi } from '../../app/api';
import { formatCpu, formatDate, formatMemory, hostStateLabel } from '../../app/format';

export function AdminHostDetail() {
  const { id = '' } = useParams();
  const hosts = useQuery({ queryKey: ['admin', 'hosts'], queryFn: portalApi.adminHosts });
  const users = useQuery({ queryKey: ['admin', 'users'], queryFn: portalApi.adminUsers });
  if (hosts.isPending || users.isPending) return <div className="loading-card" role="status">正在读取电脑信息…</div>;
  if (hosts.isError || users.isError) {
    const error = hosts.isError ? hosts.error : users.error;
    return <div className="error-card" role="alert">{errorDetails(error).message}<button className="button button-secondary" type="button" onClick={() => { void hosts.refetch(); void users.refetch(); }}>重试</button></div>;
  }
  const host = hosts.data.items.find((item) => item.id === id);
  if (!host) return <div className="empty-card"><h2>找不到这台电脑</h2><p>电脑可能已被移除，或当前账号无权查看。</p><Link className="button button-secondary" to="/admin/hosts">返回电脑列表</Link></div>;
  const owner = users.data.items.find((user) => user.id === host.owner_id);

  return <section>
    <Link className="back-link" to="/admin/hosts">‹ 返回电脑列表</Link>
    <div className="page-heading"><div><p className="eyebrow">云端电脑详情</p><h1>{host.name || '未命名主机'}</h1><p className="muted mono">{host.id} · {host.template_id}</p></div><span className={`status status-${host.state}`}>{hostStateLabel[host.state] || host.state}</span></div>
    <div className="admin-detail-facts">
      <article><span>所有者</span><strong>{owner ? <Link to={`/admin/users/${encodeURIComponent(owner.id)}`}>{owner.name}</Link> : host.owner_id}</strong></article>
      <article><span>CPU / 内存</span><strong>{formatCpu(host.resources.cpu_limit_milli)} / {formatMemory(host.resources.mem_limit_mib)}</strong></article>
      <article><span>创建时间</span><strong>{formatDate(host.created_at)}</strong></article>
      <article><span>到期时间</span><strong>{formatDate(host.expires_at)}</strong></article>
    </div>
    <section className="admin-detail-section">
      <div className="dashboard-section-heading"><div><h2>绑定工作区</h2><p>服务端保存和电脑应用状态分别核对。</p></div></div>
      {!host.workspaces.length ? <div className="empty-card compact"><p>这台电脑当前没有绑定工作区。</p></div> : <div className="table-wrap"><table><thead><tr><th>工作区</th><th>应用状态</th><th>服务端版本</th><th>电脑已应用版本</th><th>同步状态</th></tr></thead><tbody>
        {host.workspaces.map((binding) => <tr key={binding.workspace.id}><td><Link className="admin-table-link" to={`/admin/workspaces/${encodeURIComponent(binding.workspace.id)}`}><strong>{binding.workspace.name || binding.workspace.id}</strong><small className="table-sub mono">{binding.workspace.id}</small></Link></td><td>{binding.state}</td><td>v{binding.desired_revision}</td><td>{binding.applied_revision > 0 ? `v${binding.applied_revision}` : '尚未应用'}</td><td>{binding.sync_state || '未提供'}</td></tr>)}
      </tbody></table></div>}
    </section>
    <p className="notice notice-info admin-detail-note">此页面只查看电脑、所有者和工作区绑定元数据。当前管理接口不提供停止、冻结或隔离能力。</p>
  </section>;
}
