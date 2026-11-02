import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { errorDetails, portalApi } from '../../app/api';

type UsageDays = 7 | 30 | 90;
type UsageData = Awaited<ReturnType<typeof portalApi.adminUsage>>;
type StatsData = Awaited<ReturnType<typeof portalApi.adminStats>>;

const formatNumber = (value: number) => new Intl.NumberFormat('zh-CN').format(value);

function percent(numerator: number, denominator: number) {
  return denominator > 0 ? `${((numerator / denominator) * 100).toFixed(1)}%` : '—';
}

function UsageError({ error, retry }: { error: unknown; retry: () => void }) {
  return <div className="error-card" role="alert">
    {errorDetails(error).message}
    <button type="button" className="button button-secondary" onClick={retry}>重试</button>
  </div>;
}

function UsageMetrics({ data }: { data: UsageData }) {
  const metrics = [
    ['账本调用数', data.summary.calls],
    ['已结算调用', data.summary.settled_calls],
    ['待核对调用', data.summary.unknown_calls],
    ['输入 tokens', data.summary.input_tokens],
    ['输出 tokens', data.summary.output_tokens],
    ['活跃账户', data.summary.active_accounts],
  ] as const;
  return <div className="admin-detail-facts">
    {metrics.map(([label, value]) => <article key={label}><span>{label}</span><strong>{formatNumber(value)}</strong></article>)}
  </div>;
}

function UsageByDay({ data }: { data: UsageData }) {
  return <section className="admin-detail-section" aria-labelledby="usage-by-day-heading">
    <h2 id="usage-by-day-heading">每日用量</h2>
    <p className="muted">调用数与 token 统计来自 model_call_ledger。活跃账户为该日至少有一笔调用的去重账号。</p>
    {!data.by_day.length ? <div className="empty-card compact" role="status"><p>所选区间没有每日用量记录。</p></div>
      : <div className="table-wrap"><table><thead><tr><th>日期</th><th>调用数</th><th>输入 tokens</th><th>输出 tokens</th><th>活跃账户</th></tr></thead><tbody>
        {data.by_day.map((item) => <tr key={item.date}>
          <td>{item.date}</td><td>{formatNumber(item.calls)}</td><td>{formatNumber(item.input_tokens)}</td>
          <td>{formatNumber(item.output_tokens)}</td><td>{formatNumber(item.active_accounts)}</td>
        </tr>)}
      </tbody></table></div>}
  </section>;
}

function UsageByModel({ data }: { data: UsageData }) {
  return <section className="admin-detail-section" aria-labelledby="usage-by-model-heading">
    <h2 id="usage-by-model-heading">按模型</h2>
    <p className="muted">调用数与 token 统计来自 model_call_ledger。</p>
    {!data.by_model.length ? <div className="empty-card compact" role="status"><p>所选区间没有模型用量记录。</p></div>
      : <div className="table-wrap"><table><thead><tr><th>模型</th><th>调用数</th><th>输入 tokens</th><th>输出 tokens</th></tr></thead><tbody>
        {data.by_model.map((item) => <tr key={item.model_id}>
          <td>{item.model_id}</td><td>{formatNumber(item.calls)}</td><td>{formatNumber(item.input_tokens)}</td><td>{formatNumber(item.output_tokens)}</td>
        </tr>)}
      </tbody></table></div>}
  </section>;
}

function PlatformStats({ data }: { data: StatsData }) {
  return <>
    <section className="admin-detail-section" aria-labelledby="admin-user-counts-heading">
      <h2 id="admin-user-counts-heading">用户统计</h2>
      <div className="admin-detail-facts">
        <article><span>用户总数</span><strong>{formatNumber(data.users.total)}</strong></article>
        <article><span>已通过</span><strong>{formatNumber(data.users.approved)}</strong></article>
        <article><span>待审核</span><strong>{formatNumber(data.users.pending)}</strong></article>
        <article><span>已拒绝</span><strong>{formatNumber(data.users.rejected)}</strong></article>
        <article><span>已禁用</span><strong>{formatNumber(data.users.disabled)}</strong></article>
      </div>
    </section>
    <section className="admin-detail-section" aria-labelledby="admin-platforms-heading">
      <h2 id="admin-platforms-heading">平台安装</h2>
      <p className="muted">安装数包含已撤权记录；活跃安装要求未撤权、未过期且最近 30 天有 last_used_at。用户数为每个平台去重账号。</p>
      {!data.platforms.length ? <div className="empty-card compact" role="status"><p>暂无平台安装统计。</p></div>
        : <div className="table-wrap"><table><thead><tr><th>平台</th><th>安装数</th><th>活跃安装</th><th>用户数</th></tr></thead><tbody>
          {data.platforms.map((item) => <tr key={item.platform}>
            <td>{item.platform}</td><td>{formatNumber(item.installations)}</td>
            <td>{formatNumber(item.active_installations)}</td><td>{formatNumber(item.users)}</td>
          </tr>)}
        </tbody></table></div>}
    </section>
    <section className="admin-detail-section" aria-labelledby="admin-cohorts-heading">
      <h2 id="admin-cohorts-heading">注册周与安装</h2>
      <p className="muted">按注册周归组，展示注册后曾有安装的账号比例。</p>
      {!data.cohorts.length ? <div className="empty-card compact" role="status"><p>暂无注册周统计。</p></div>
        : <div className="table-wrap"><table><thead><tr><th>注册周</th><th>注册账号</th><th>曾有安装账号</th><th>曾有安装比例</th></tr></thead><tbody>
          {data.cohorts.map((item) => <tr key={item.week}>
            <td>{item.week}</td><td>{formatNumber(item.signups)}</td><td>{formatNumber(item.with_installation)}</td>
            <td>{percent(item.with_installation, item.signups)}</td>
          </tr>)}
        </tbody></table></div>}
    </section>
    <section className="admin-detail-section" aria-labelledby="admin-version-tracking-heading">
      <h2 id="admin-version-tracking-heading">客户端版本</h2>
      {data.version_tracking_available
        ? <p className="muted">版本采集已启用；当前统计接口未返回版本明细。</p>
        : <div className="empty-card compact" role="status"><p>暂不可采集客户端版本。服务端目前没有可用的版本统计数据。</p></div>}
    </section>
  </>;
}

export function AdminUsage() {
  const [days, setDays] = useState<UsageDays>(7);
  const usage = useQuery({ queryKey: ['admin', 'usage', days], queryFn: () => portalApi.adminUsage(days) });
  const stats = useQuery({ queryKey: ['admin', 'stats'], queryFn: portalApi.adminStats });

  return <section className="admin-content">
    <div className="page-heading"><div><p className="eyebrow">平台统计</p><h1>用量与统计</h1><p className="muted">按账本展示模型调用用量，以及用户、平台安装和注册周统计。</p></div></div>
    <div className="device-manager-actions">
      <label htmlFor="admin-usage-days">用量区间</label>
      <select id="admin-usage-days" value={days} onChange={(event) => setDays(Number(event.target.value) as UsageDays)}>
        <option value={7}>近 7 天</option><option value={30}>近 30 天</option><option value={90}>近 90 天</option>
      </select>
    </div>
    <section className="admin-detail-section" aria-labelledby="admin-model-usage-heading">
      <div className="page-heading"><div><h2 id="admin-model-usage-heading">平台模型用量</h2>
        <p className="muted">调用数与 token 来源于 model_call_ledger；活跃账户为所选区间至少有一笔调用的去重账号。</p>
        {usage.data && <p className="muted">统计区间：{usage.data.from} 至 {usage.data.to}</p>}
      </div></div>
      {usage.isPending ? <div className="loading-card" role="status">正在加载平台模型用量…</div>
        : usage.isError ? <UsageError error={usage.error} retry={() => void usage.refetch()} />
          : <>
            <UsageMetrics data={usage.data} />
            <UsageByDay data={usage.data} />
            <UsageByModel data={usage.data} />
          </>}
    </section>
    <section className="admin-detail-section" aria-labelledby="admin-platform-stats-heading">
      <h2 id="admin-platform-stats-heading">平台统计</h2>
      {stats.isPending ? <div className="loading-card" role="status">正在加载平台统计…</div>
        : stats.isError ? <UsageError error={stats.error} retry={() => void stats.refetch()} />
          : <PlatformStats data={stats.data} />}
    </section>
  </section>;
}
