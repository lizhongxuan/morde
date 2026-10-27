import { useQuery } from '@tanstack/react-query';
import { errorDetails, portalApi } from '../../app/api';

export function AdminCapacity() {
  const query = useQuery({ queryKey: ['admin', 'capacity'], queryFn: portalApi.capacity, refetchInterval: 30000 });
  if (query.isPending) return <div className="loading-card">正在加载集群容量…</div>;
  if (query.isError) return <div className="error-card" role="alert">{errorDetails(query.error).message}<button type="button" className="button button-secondary" onClick={() => void query.refetch()}>重试</button></div>;
  const data = query.data;
  const states = Object.entries(data.hosts_by_state);
  return <section className="admin-content">
    <h2 className="section-title">主机状态</h2>
    <div className="state-grid">{states.length
      ? states.map(([state, count]) => <div className="state-count" key={state}><span>{state}</span><strong>{count}</strong></div>)
      : <div className="empty-card compact" role="status"><p>当前没有云端电脑</p></div>}
    </div>
  </section>;
}
