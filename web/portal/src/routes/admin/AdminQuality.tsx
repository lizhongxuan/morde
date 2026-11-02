import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { errorDetails, portalApi } from '../../app/api';

type QualityDays = 7 | 30 | 90;
type QualityData = Awaited<ReturnType<typeof portalApi.adminQuality>>;

function QualityError({ error, retry }: { error: unknown; retry: () => void }) {
  return <div className="error-card" role="alert">
    {errorDetails(error).message}
    <button type="button" className="button button-secondary" onClick={retry}>重试</button>
  </div>;
}

function QualityAvailability({ data }: { data: QualityData }) {
  return <section className="admin-detail-section" aria-labelledby="admin-quality-source-heading">
    <h2 id="admin-quality-source-heading">质量与风险数据源</h2>
    {data.available
      ? <div className="admin-detail-facts"><article><span>数据源状态</span><strong>可用</strong><p className="muted">管理接口目前只提供来源状态，未返回指标明细；不会从普通会话推导质量数据。</p></article></div>
      : <div className="empty-card compact" role="status"><h3>当前不可用</h3><p>{data.unavailable_reason || '服务端未提供不可用原因。'}</p><p className="muted">未授权样本不会被读取，普通会话正文不会作为替代数据。</p></div>}
  </section>;
}

export function AdminQuality() {
  const [days, setDays] = useState<QualityDays>(7);
  const query = useQuery({ queryKey: ['admin', 'quality', days], queryFn: () => portalApi.adminQuality(days) });

  return <section className="admin-content">
    <div className="page-heading"><div><p className="eyebrow">管理数据</p><h1>质量与风险</h1><p className="muted">仅展示服务端明确提供且符合授权条件的数据。</p></div></div>
    <div className="device-manager-actions">
      <label htmlFor="admin-quality-days">统计区间</label>
      <select id="admin-quality-days" value={days} onChange={(event) => setDays(Number(event.target.value) as QualityDays)}>
        <option value={7}>近 7 天</option><option value={30}>近 30 天</option><option value={90}>近 90 天</option>
      </select>
    </div>
    <section className="admin-detail-section" aria-labelledby="admin-quality-heading">
      <h2 id="admin-quality-heading">质量与风险统计</h2>
      {query.data && <p className="muted">统计区间：近 {query.data.days} 天</p>}
      {query.isPending ? <div className="loading-card" role="status">正在加载质量与风险数据…</div>
        : query.isError ? <QualityError error={query.error} retry={() => void query.refetch()} />
          : <QualityAvailability data={query.data} />}
    </section>
  </section>;
}
