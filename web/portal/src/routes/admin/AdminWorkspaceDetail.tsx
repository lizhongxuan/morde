import { Link, useParams } from 'react-router';
import { useQuery } from '@tanstack/react-query';
import { errorDetails, portalApi } from '../../app/api';
import { formatBytes, formatDate } from '../../app/format';

export function AdminWorkspaceDetail() {
  const { id = '' } = useParams();
  const query = useQuery({ queryKey: ['admin', 'workspace', id], queryFn: () => portalApi.adminWorkspace(id) });
  if (query.isPending) return <div className="loading-card" role="status">正在读取工作区…</div>;
  if (query.isError) return <div className="error-card" role="alert">{errorDetails(query.error).message}<button className="button button-secondary" type="button" onClick={() => void query.refetch()}>重试</button></div>;
  const workspace = query.data;

  return <section>
    <Link className="back-link" to="/admin/workspaces">‹ 返回工作区列表</Link>
    <div className="page-heading detail-heading"><div><p className="eyebrow">工作区详情</p><h2>{workspace.name || workspace.id}</h2><p className="muted mono">{workspace.id}</p></div><span className={`status status-${workspace.state}`}>{workspace.state === 'ready' ? '可用' : workspace.state === 'deleting' ? '删除中' : workspace.state}</span></div>
    <div className="admin-detail-facts">
      <article><span>所有者</span><strong><Link to={`/admin/users/${encodeURIComponent(workspace.owner_id)}`}>{workspace.owner_name || workspace.owner_id}</Link></strong></article>
      <article><span>逻辑用量 / 配额</span><strong>{formatBytes(workspace.used_bytes)} / {formatBytes(workspace.quota_bytes)}</strong></article>
      <article><span>当前版本</span><strong>v{workspace.revision}</strong></article>
      <article><span>创建时间</span><strong>{formatDate(workspace.created_at)}</strong></article>
      <article><span>最近更新</span><strong>{formatDate(workspace.updated_at)}</strong></article>
    </div>
    <section className="admin-detail-section">
      <div className="dashboard-section-heading"><div><h2>电脑绑定</h2><p>工作区服务端保存和电脑应用状态分别展示。</p></div></div>
      {!workspace.bindings.length ? <div className="empty-card compact"><p>该工作区未绑定电脑，数据仍保存在服务端。</p></div> : <div className="table-wrap"><table><thead><tr><th>云端电脑</th><th>绑定状态</th><th>服务端期望版本</th><th>电脑已应用版本</th><th>同步状态</th></tr></thead><tbody>
        {workspace.bindings.map((binding) => <tr key={binding.host_id}><td><Link className="admin-table-link" to={`/admin/hosts/${encodeURIComponent(binding.host_id)}`}><strong>{binding.host_name || binding.host_id}</strong><small className="table-sub mono">{binding.host_id}</small></Link></td><td><span className={`status status-${binding.state}`}>{binding.state === 'active' ? '已绑定' : binding.state === 'preparing' ? '正在应用' : binding.state}</span></td><td>v{binding.desired_revision}</td><td>{binding.applied_revision > 0 ? `v${binding.applied_revision}` : '尚未应用'}</td><td>{binding.sync_state === 'healthy' ? '正常' : binding.sync_state === 'conflict' ? '有冲突' : binding.sync_state}</td></tr>)}
      </tbody></table></div>}
    </section>
    <p className="notice notice-info admin-detail-note">管理员仅能查看工作区元数据；本页不包含目录路径、文件正文或恢复操作。</p>
  </section>;
}
