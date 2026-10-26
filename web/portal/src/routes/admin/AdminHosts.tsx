import { useQuery } from '@tanstack/react-query';
import { Link } from 'react-router';
import { portalApi, errorDetails } from '../../app/api';
import { formatCpu, formatDate, formatMemory, hostStateLabel } from '../../app/format';

export function AdminHosts() {
  const query = useQuery({ queryKey: ['admin', 'hosts'], queryFn: portalApi.adminHosts });
  return <section className="admin-content">
    <div className="page-heading"><div><p className="eyebrow">资源概况</p><h1>云端电脑</h1><p className="muted">查看所有者、运行状态、规格和工作区绑定。</p></div></div>
    {query.isPending ? <div className="loading-card">正在加载全部主机…</div>
      : query.isError ? <div className="error-card" role="alert">{errorDetails(query.error).message}<button type="button" className="button button-secondary" onClick={() => void query.refetch()}>重试</button></div>
        : !query.data?.items.length ? <div className="empty-card compact"><p>当前没有主机。</p></div> : <div className="table-wrap"><table><thead><tr><th>主机</th><th>用户</th><th>状态</th><th>规格</th><th>绑定工作区</th><th>到期时间</th><th>创建时间</th></tr></thead><tbody>
          {query.data.items.map((host) => <tr key={host.id}><td><Link className="admin-table-link" to={`/admin/hosts/${encodeURIComponent(host.id)}`}><strong>{host.name || host.id}</strong><small className="table-sub mono">{host.id} · {host.template_id}</small></Link></td><td><Link className="admin-table-link mono" to={`/admin/users/${encodeURIComponent(host.owner_id)}`}>{host.owner_id}</Link></td><td><span className={`status status-${host.state}`}>{hostStateLabel[host.state] || host.state}</span></td><td>{formatCpu(host.resources.cpu_limit_milli)} / {formatMemory(host.resources.mem_limit_mib)}</td><td>{host.workspaces.length ? host.workspaces.map((binding) => binding.workspace.name || binding.workspace.id).join('、') : '未绑定'}</td><td>{formatDate(host.expires_at)}</td><td>{formatDate(host.created_at)}</td></tr>)}
        </tbody></table></div>}
  </section>;
}
