import { useState } from 'react';
import { Link } from 'react-router';
import { useQuery } from '@tanstack/react-query';
import type { components } from '../../api/schema';
import { errorDetails, portalApi } from '../../app/api';
import { formatDate } from '../../app/format';

const PAGE_SIZE = 25;
const statusLabels: Record<components['schemas']['AccountStatus'], string> = {
  pending: '待审核', rejected: '已拒绝', approved: '已通过', disabled: '已停用',
};

export function AdminInvites() {
  const [searchInput, setSearchInput] = useState('');
  const [search, setSearch] = useState('');
  const [offset, setOffset] = useState(0);
  const query = useQuery({
    queryKey: ['admin', 'invites', search, offset],
    queryFn: () => portalApi.adminInvites(search, PAGE_SIZE, offset),
  });
  const data = query.data;

  function submitSearch(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setSearch(searchInput.trim());
    setOffset(0);
  }

  function clearSearch() {
    setSearchInput('');
    setSearch('');
    setOffset(0);
  }

  return <section className="admin-content">
    <div className="page-heading"><div><p className="eyebrow">账号关系</p><h1>邀请记录</h1><p className="muted">只读查看邀请人、被邀请人及账号状态；邀请不绕过审核，本期不会自动发放奖励。</p></div>
      <button type="button" className="button button-secondary" disabled={query.isFetching} onClick={() => void query.refetch()}>{query.isFetching ? '正在刷新…' : '刷新数据'}</button>
    </div>

    <form className="admin-list-toolbar" role="search" onSubmit={submitSearch}>
      <label htmlFor="admin-invite-search">搜索邀请人或被邀请人</label>
      <input id="admin-invite-search" type="search" value={searchInput} onChange={(event) => setSearchInput(event.target.value)} placeholder="输入名称或用户 ID" />
      <button type="submit" className="button button-primary button-small">搜索</button>
      {!!search && <button type="button" className="button button-secondary button-small" onClick={clearSearch}>清除筛选</button>}
    </form>

    {query.isPending ? <div className="loading-card" role="status">正在加载邀请记录…</div>
      : query.isError && !data ? <div className="error-card" role="alert">{errorDetails(query.error).message}<button type="button" className="button button-secondary" onClick={() => void query.refetch()}>重试</button></div>
        : data && <>
          <div className="admin-detail-facts">
            <article><span>邀请关系总数</span><strong>{new Intl.NumberFormat('zh-CN').format(data.total)}</strong></article>
            <article><span>有邀请记录的邀请人</span><strong>{new Intl.NumberFormat('zh-CN').format(data.total_inviters)}</strong></article>
            <article><span>当前结果</span><strong>{data.items.length ? `${offset + 1}–${offset + data.items.length}` : '0'} / {new Intl.NumberFormat('zh-CN').format(data.total)}</strong></article>
          </div>
          {!data.items.length ? <div className="empty-card compact" role="status"><p>{search ? '没有符合当前筛选条件的邀请记录。' : '当前没有邀请记录。'}</p></div>
            : <div className="table-wrap"><table>
              <thead><tr><th>邀请人</th><th>被邀请人</th><th>账号状态</th><th>邀请人累计邀请</th><th>建立时间</th></tr></thead>
              <tbody>{data.items.map((invite) => <tr key={`${invite.inviter_id}:${invite.invitee_id}:${invite.created_at}`}>
                <td><Link className="admin-table-link" to={`/admin/users/${encodeURIComponent(invite.inviter_id)}`}><strong>{invite.inviter_name || invite.inviter_id}</strong><small className="table-sub mono">{invite.inviter_id}</small></Link></td>
                <td><Link className="admin-table-link" to={`/admin/users/${encodeURIComponent(invite.invitee_id)}`}><strong>{invite.invitee_name || invite.invitee_id}</strong><small className="table-sub mono">{invite.invitee_id}</small></Link></td>
                <td><span className={`status status-${invite.invitee_status}`}>{statusLabels[invite.invitee_status]}</span></td>
                <td>{new Intl.NumberFormat('zh-CN').format(invite.inviter_invitees)}</td>
                <td>{formatDate(invite.created_at)}</td>
              </tr>)}</tbody>
            </table></div>}
          <div className="inline-actions admin-pagination">
            <button type="button" className="button button-secondary" disabled={offset === 0 || query.isFetching} onClick={() => setOffset(Math.max(0, offset - PAGE_SIZE))}>上一页</button>
            <button type="button" className="button button-secondary" disabled={offset + data.items.length >= data.total || query.isFetching} onClick={() => setOffset(offset + PAGE_SIZE)}>下一页</button>
          </div>
          {query.isError && <div className="error-card admin-pagination-error" role="alert">{errorDetails(query.error).message}<button type="button" className="button button-secondary" onClick={() => void query.refetch()}>重试</button></div>}
        </>}
  </section>;
}
