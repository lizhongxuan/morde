import { useQuery } from '@tanstack/react-query';
import { errorDetails, portalApi } from '../../app/api';
import { formatCpu, formatDate, formatMemory, hostStateLabel } from '../../app/format';
import { FloatingError } from '../../components/FloatingError';

export function AdminOverview() {
  const query = useQuery({ queryKey: ['admin', 'overview'], queryFn: portalApi.adminOverview, refetchInterval: 30000 });
  if (query.isPending) return <div className="loading-card" role="status">正在加载管理总览…</div>;
  if (query.isError) return <FloatingError className="error-card" role="alert">{errorDetails(query.error).message}<button type="button" className="button button-secondary" onClick={() => void query.refetch()}>重试</button></FloatingError>;

  const { health, hosts_by_state: hosts, agents_by_state: agents } = query.data;
  const hostStates = Object.entries(hosts);
  const agentStates = Object.entries(agents);
  return <section className="admin-content">
    <section className="design-panel" aria-labelledby="admin-overview-title">
    <div className="design-panel-header"><div><h2 id="admin-overview-title">平台概况</h2><p>服务与资源的当前状态。</p></div><button type="button" className="button button-secondary" disabled={query.isFetching} onClick={() => void query.refetch()}>刷新数据</button></div>
    <div className="admin-detail-facts">
      <article><span>服务就绪状态</span><strong><span className={`status ${health.ready ? 'status-ready' : 'status-failed'}`}>{health.ready ? '就绪' : '未就绪'}</span></strong>{health.problem && <p className="muted">{health.problem}</p>}</article>
      <article><span>在线安装数</span><strong>{query.data.online_installations.toLocaleString()}</strong><p className="muted">当前 Hub 进程内的 Client Installation，不代表 Agent 或 Host 数量。</p></article>
      <article><span>检查时间</span><strong>{formatDate(query.data.checked_at)}</strong></article>
      <article><span>CPU 已分配 / 上限</span><strong>{formatCpu(query.data.allocated.cpu_milli)} / {formatCpu(query.data.limit.cpu_milli)}</strong></article>
      <article><span>内存已分配 / 上限</span><strong>{formatMemory(query.data.allocated.mem_mib)} / {formatMemory(query.data.limit.mem_mib)}</strong></article>
    </div>
    </section>
    <section className="admin-detail-section" aria-labelledby="admin-host-states">
      <h2 id="admin-host-states">Host 状态</h2>
      <div className="state-grid">{hostStates.length
        ? hostStates.map(([state, count]) => <div className="state-count" key={state}><span>{hostStateLabel[state] ?? state}</span><strong>{count.toLocaleString()}</strong></div>)
        : <div className="empty-card compact" role="status"><p>暂无 Host 状态数据。</p></div>}
      </div>
    </section>
    <section className="admin-detail-section" aria-labelledby="admin-agent-states">
      <h2 id="admin-agent-states">Agent 状态</h2>
      <div className="state-grid">{agentStates.length
        ? agentStates.map(([state, count]) => <div className="state-count" key={state}><span>{state}</span><strong>{count.toLocaleString()}</strong></div>)
        : <div className="empty-card compact" role="status"><p>暂无 Agent 状态数据。</p></div>}
      </div>
    </section>
  </section>;
}
