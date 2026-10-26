import { useState } from 'react';
import type { FormEvent } from 'react';
import { useQuery } from '@tanstack/react-query';
import { errorDetails, portalApi } from '../../app/api';
import { desktopCapabilityNames, getDesktopDownloads } from '../../app/desktopDownloads';
import { formatDate } from '../../app/format';

export function AdminAudit() {
  const [actor, setActor] = useState('');
  const [action, setAction] = useState('');
  const [target, setTarget] = useState('');
  const [since, setSince] = useState('');
  const [filters, setFilters] = useState(new URLSearchParams({ limit: '100' }));
  const query = useQuery({ queryKey: ['admin', 'audit', filters.toString()], queryFn: () => portalApi.audit(filters) });
  const releases = useQuery({ queryKey: ['desktopDownloads'], queryFn: () => getDesktopDownloads() });
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const next = new URLSearchParams({ limit: '100' });
    if (actor.trim()) next.set('actor', actor.trim());
    if (action.trim()) next.set('action', action.trim());
    if (target.trim()) next.set('target', target.trim());
    if (since) next.set('since', new Date(since).toISOString());
    setFilters(next);
  };
  return <section className="admin-content">
    <div className="page-heading"><div><p className="eyebrow">管理员操作记录</p><h1>审计</h1><p className="muted">筛选已记录的操作及其结果。</p></div></div>
    <form className="audit-filters form-card" onSubmit={(event) => void submit(event)}>
      <div><label htmlFor="audit-actor">操作人</label><input id="audit-actor" value={actor} onChange={(event) => setActor(event.target.value)} /></div>
      <div><label htmlFor="audit-action">操作类型</label><input id="audit-action" value={action} onChange={(event) => setAction(event.target.value)} placeholder="如 host.create" /></div>
      <div><label htmlFor="audit-target">目标</label><input id="audit-target" value={target} onChange={(event) => setTarget(event.target.value)} /></div>
      <div><label htmlFor="audit-since">开始时间</label><input id="audit-since" type="datetime-local" value={since} onChange={(event) => setSince(event.target.value)} /></div>
      <button className="button button-primary" type="submit">筛选</button>
    </form>
    <section className="form-card release-audit" aria-labelledby="release-audit-heading">
      <h2 id="release-audit-heading">桌面发行验收</h2>
      {releases.isPending ? <p role="status">正在读取已验收发行…</p> : releases.isError ? <div className="error-card" role="alert">{releases.error instanceof Error ? releases.error.message : '暂时无法读取桌面发行记录'}</div> : releases.data.artifacts.length === 0 ? <p className="muted">当前没有通过平台验收的桌面发行包。</p> : <div className="table-wrap"><table><thead><tr><th>版本</th><th>平台</th><th>验收时间</th><th>签名</th><th>已验收能力</th><th>证据编号</th></tr></thead><tbody>
        {releases.data.artifacts.map((item) => <tr key={`${item.platform}:${item.architecture}`}>
          <td>{releases.data.version}</td>
          <td>{item.platform} · {item.architecture}</td>
          <td>{formatDate(item.acceptance.verified_at)}</td>
          <td><code>{item.acceptance.signature_status}</code></td>
          <td>{item.acceptance.capabilities.map((capability) => desktopCapabilityNames[capability]).join('、')}</td>
          <td><code>{item.acceptance.evidence_ref}</code></td>
        </tr>)}
      </tbody></table></div>}
    </section>
    {query.isPending ? <div className="loading-card">正在加载审计记录…</div> : query.isError ? <div className="error-card" role="alert">{errorDetails(query.error).message}<button className="button button-secondary" type="button" onClick={() => void query.refetch()}>重试</button></div> : !query.data?.items.length ? <div className="empty-card compact"><p>没有匹配的审计记录。</p></div> : <div className="table-wrap"><table><thead><tr><th>时间</th><th>操作人</th><th>操作类型</th><th>目标</th><th>来源 IP</th><th>详情</th></tr></thead><tbody>
      {query.data.items.map((item) => <tr key={item.id}><td>{formatDate(item.at)}</td><td className="mono">{item.actor_id}<small className="table-sub">{item.actor_kind}</small></td><td><code>{item.action}</code></td><td className="mono">{item.target}</td><td className="mono">{item.ip}</td><td><details><summary>查看</summary><pre className="audit-detail">{JSON.stringify(item.detail, null, 2)}</pre></details></td></tr>)}
    </tbody></table></div>}
  </section>;
}
