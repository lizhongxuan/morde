import { useQuery } from '@tanstack/react-query';
import { errorDetails, portalApi } from '../../app/api';
import { formatCpu, formatMemory } from '../../app/format';

export function AdminCapacity() {
  const query = useQuery({ queryKey: ['admin', 'capacity'], queryFn: portalApi.capacity, refetchInterval: 30000 });
  if (query.isPending) return <div className="loading-card">正在加载集群容量…</div>;
  if (query.isError) return <div className="error-card" role="alert">{errorDetails(query.error).message}<button type="button" className="button button-secondary" onClick={() => void query.refetch()}>重试</button></div>;
  const data = query.data;
  const states = Object.entries(data.hosts_by_state);
  return <section className="admin-content">
    <div className="page-heading"><div><p className="eyebrow">系统状态</p><h1>总览</h1><p className="muted">当前容量和云端电脑状态来自管理接口实时汇总。</p></div></div>
    <div className="capacity-grid">
      <article className="metric-card"><h2>CPU</h2><strong>{formatCpu(data.allocated.cpu_milli)} <span>/ {formatCpu(data.limit.cpu_milli)}</span></strong><progress aria-label="CPU 已分配比例" max={Math.max(data.limit.cpu_milli, 1)} value={Math.min(data.allocated.cpu_milli, data.limit.cpu_milli)} /></article>
      <article className="metric-card"><h2>内存</h2><strong>{formatMemory(data.allocated.mem_mib)} <span>/ {formatMemory(data.limit.mem_mib)}</span></strong><progress aria-label="内存已分配比例" max={Math.max(data.limit.mem_mib, 1)} value={Math.min(data.allocated.mem_mib, data.limit.mem_mib)} /></article>
    </div>
    <h2 className="section-title">主机状态</h2>
    <div className="state-grid">{states.length
      ? states.map(([state, count]) => <div className="state-count" key={state}><span>{state}</span><strong>{count}</strong></div>)
      : <div className="empty-card compact" role="status"><p>当前没有云端电脑</p></div>}
    </div>
  </section>;
}
