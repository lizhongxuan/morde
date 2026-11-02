import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { errorDetails, portalApi } from '../../app/api';

type ActivityDays = 7 | 30 | 90;
type ActivityData = Awaited<ReturnType<typeof portalApi.adminActivity>>;
type PlacesData = Awaited<ReturnType<typeof portalApi.adminPlaces>>;

const formatNumber = (value: number) => new Intl.NumberFormat('zh-CN').format(value);

function ActivityError({ error, retry }: { error: unknown; retry: () => void }) {
  return <div className="error-card" role="alert">
    {errorDetails(error).message}
    <button type="button" className="button button-secondary" onClick={retry}>重试</button>
  </div>;
}

function ActivitySummary({ data }: { data: ActivityData }) {
  const totals = data.by_day.reduce((result, day) => ({
    hub_operations: result.hub_operations + day.hub_operations,
    task_events: result.task_events + day.task_events,
    audit_events: result.audit_events + day.audit_events,
  }), { hub_operations: 0, task_events: 0, audit_events: 0 });

  return <div className="admin-detail-facts">
    <article><span>Hub 操作</span><strong>{formatNumber(totals.hub_operations)}</strong></article>
    <article><span>Task 事件</span><strong>{formatNumber(totals.task_events)}</strong></article>
    <article><span>审计事件</span><strong>{formatNumber(totals.audit_events)}</strong></article>
  </div>;
}

function ActivityByDay({ data }: { data: ActivityData }) {
  return <section className="admin-detail-section" aria-labelledby="admin-activity-days-heading">
    <h2 id="admin-activity-days-heading">每日活动</h2>
    {!data.by_day.length ? <div className="empty-card compact" role="status"><p>所选区间没有每日活动统计。</p></div>
      : <div className="table-wrap"><table><thead><tr><th>日期</th><th>Hub 操作</th><th>Task 事件</th><th>审计事件</th></tr></thead><tbody>
        {data.by_day.map((item) => <tr key={item.date}>
          <td>{item.date}</td><td>{formatNumber(item.hub_operations)}</td><td>{formatNumber(item.task_events)}</td><td>{formatNumber(item.audit_events)}</td>
        </tr>)}
      </tbody></table></div>}
  </section>;
}

function PlacesAvailability({ data }: { data: PlacesData }) {
  return <section className="admin-detail-section" aria-labelledby="admin-places-heading">
    <h2 id="admin-places-heading">活动地点</h2>
    {data.available
      ? <p className="muted">聚合位置来源可用；当前接口没有返回地点明细。</p>
      : <div className="empty-card compact" role="status"><p>{data.unavailable_reason || '当前没有合规的聚合位置数据源。'}</p><p className="muted">不采集设备定位，也不根据 IP 推断地点。</p></div>}
  </section>;
}

function ActivityBreakdown({ data }: { data: ActivityData }) {
  return <>
    <section className="admin-detail-section" aria-labelledby="admin-hub-activity-heading">
      <h2 id="admin-hub-activity-heading">Hub 操作分类</h2>
      {!data.hub_operations.length ? <div className="empty-card compact" role="status"><p>暂无 Hub 操作统计。</p></div>
        : <div className="table-wrap"><table><thead><tr><th>动作</th><th>状态</th><th>次数</th></tr></thead><tbody>
          {data.hub_operations.map((item) => <tr key={`${item.action}:${item.status}`}>
            <td><code>{item.action}</code></td><td><code>{item.status}</code></td><td>{formatNumber(item.count)}</td>
          </tr>)}
        </tbody></table></div>}
    </section>
    <section className="admin-detail-section" aria-labelledby="admin-task-activity-heading">
      <h2 id="admin-task-activity-heading">Task 事件分类</h2>
      {!data.task_events.length ? <div className="empty-card compact" role="status"><p>暂无 Task 事件统计。</p></div>
        : <div className="table-wrap"><table><thead><tr><th>事件类型</th><th>次数</th></tr></thead><tbody>
          {data.task_events.map((item) => <tr key={item.type}><td><code>{item.type}</code></td><td>{formatNumber(item.count)}</td></tr>)}
        </tbody></table></div>}
    </section>
    <section className="admin-detail-section" aria-labelledby="admin-audit-activity-heading">
      <h2 id="admin-audit-activity-heading">审计事件分类</h2>
      {!data.audit_events.length ? <div className="empty-card compact" role="status"><p>暂无审计事件统计。</p></div>
        : <div className="table-wrap"><table><thead><tr><th>操作类型</th><th>次数</th></tr></thead><tbody>
          {data.audit_events.map((item) => <tr key={item.action}><td><code>{item.action}</code></td><td>{formatNumber(item.count)}</td></tr>)}
        </tbody></table></div>}
    </section>
  </>;
}

export function AdminActivity() {
  const [days, setDays] = useState<ActivityDays>(7);
  const query = useQuery({ queryKey: ['admin', 'activity', days], queryFn: () => portalApi.adminActivity(days) });
  const placesQuery = useQuery({ queryKey: ['admin', 'places', days], queryFn: () => portalApi.adminPlaces(days) });

  return <section className="admin-content">
    <div className="page-heading"><div><p className="eyebrow">管理数据</p><h1>活动</h1><p className="muted">仅展示按事件类别和日期汇总的计数，不显示用户标识、地址或事件内容。</p></div></div>
    <div className="device-manager-actions">
      <label htmlFor="admin-activity-days">统计区间</label>
      <select id="admin-activity-days" value={days} onChange={(event) => setDays(Number(event.target.value) as ActivityDays)}>
        <option value={7}>近 7 天</option><option value={30}>近 30 天</option><option value={90}>近 90 天</option>
      </select>
    </div>
    {query.isPending ? <div className="loading-card" role="status">正在加载活动统计…</div>
      : query.isError ? <ActivityError error={query.error} retry={() => void query.refetch()} />
        : <>
          <section className="admin-detail-section" aria-labelledby="admin-activity-summary-heading">
            <div className="page-heading"><div><h2 id="admin-activity-summary-heading">活动概览</h2><p className="muted">统计区间：{query.data.from} 至 {query.data.to}</p></div></div>
            <ActivitySummary data={query.data} />
          </section>
          <ActivityByDay data={query.data} />
          <ActivityBreakdown data={query.data} />
          {placesQuery.isPending ? <section className="admin-detail-section"><h2>活动地点</h2><div className="loading-card" role="status">正在加载位置汇总…</div></section>
            : placesQuery.isError ? <section className="admin-detail-section"><h2>活动地点</h2><ActivityError error={placesQuery.error} retry={() => void placesQuery.refetch()} /></section>
              : <PlacesAvailability data={placesQuery.data} />}
        </>}
  </section>;
}
