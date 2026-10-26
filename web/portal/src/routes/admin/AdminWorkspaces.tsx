import { useMemo, useState } from 'react';
import { Link, useSearchParams } from 'react-router';
import { useInfiniteQuery } from '@tanstack/react-query';
import { errorDetails, portalApi } from '../../app/api';
import { formatBytes, formatDate } from '../../app/format';

export function AdminWorkspaces() {
  const [searchParams] = useSearchParams();
  const [filter, setFilter] = useState(() => searchParams.get('owner') ?? '');
  const query = useInfiniteQuery({
    queryKey: ['admin', 'workspaces'],
    queryFn: ({ pageParam }) => portalApi.adminWorkspaces(pageParam),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (page) => page.next_cursor ?? undefined,
  });
  const workspaces = useMemo(() => query.data?.pages.flatMap((page) => page.items) ?? [], [query.data]);
  const normalizedFilter = filter.trim().toLocaleLowerCase();
  const filtered = workspaces.filter((workspace) => !normalizedFilter || [workspace.name, workspace.id, workspace.owner_name, workspace.owner_id,
    ...workspace.bindings.flatMap((binding) => [binding.host_name, binding.host_id])]
    .some((value) => value.toLocaleLowerCase().includes(normalizedFilter)));

  return <section className="admin-content">
    <div className="page-heading"><div><p className="eyebrow">存储元数据</p><h1>工作区</h1><p className="muted">只查看名称、所有者、版本、用量和电脑绑定，不展示用户文件内容。</p></div></div>
    <div className="admin-list-toolbar"><label htmlFor="admin-workspace-search">搜索工作区、用户或电脑</label><input id="admin-workspace-search" type="search" value={filter} onChange={(event) => setFilter(event.target.value)} placeholder="输入名称或 ID" /></div>
    {query.isPending ? <div className="loading-card" role="status">正在加载工作区…</div>
      : query.isError && !query.data ? <div className="error-card" role="alert">{errorDetails(query.error).message}<button className="button button-secondary" type="button" onClick={() => void query.refetch()}>重试</button></div>
        : !filtered.length ? <div className="empty-card compact"><p>{normalizedFilter ? query.hasNextPage ? '已加载的工作区中没有匹配项；可继续加载后查找。' : '没有匹配的工作区。' : '当前没有工作区。'}</p></div>
          : <div className="table-wrap"><table><thead><tr><th>工作区</th><th>所有者</th><th>状态</th><th>逻辑用量</th><th>版本</th><th>绑定电脑</th><th>最近更新</th></tr></thead><tbody>
            {filtered.map((workspace) => <tr key={workspace.id}><td><Link className="admin-table-link" to={`/admin/workspaces/${encodeURIComponent(workspace.id)}`}><strong>{workspace.name || workspace.id}</strong><small className="table-sub mono">{workspace.id}</small></Link></td><td><Link className="admin-table-link" to={`/admin/users/${encodeURIComponent(workspace.owner_id)}`}>{workspace.owner_name || workspace.owner_id}<small className="table-sub mono">{workspace.owner_id}</small></Link></td><td><span className={`status status-${workspace.state}`}>{workspace.state === 'ready' ? '可用' : workspace.state === 'deleting' ? '删除中' : workspace.state}</span></td><td>{formatBytes(workspace.used_bytes)} / {formatBytes(workspace.quota_bytes)}</td><td>v{workspace.revision}</td><td>{workspace.bindings.length ? workspace.bindings.map((binding) => <Link className="admin-binding-link" to={`/admin/hosts/${encodeURIComponent(binding.host_id)}`} key={binding.host_id}>{binding.host_name || binding.host_id}<small>{binding.state === 'active' && binding.applied_revision === binding.desired_revision ? '已应用' : binding.state === 'preparing' ? '正在应用' : binding.state}</small></Link>) : '未绑定'}</td><td>{formatDate(workspace.updated_at)}</td></tr>)}
          </tbody></table></div>}
    {query.isError && query.data && <div className="error-card admin-pagination-error" role="alert">{errorDetails(query.error).message}<button className="button button-secondary" type="button" onClick={() => void query.refetch()}>重试</button></div>}
    {query.hasNextPage && <button className="button button-secondary admin-load-more" type="button" disabled={query.isFetchingNextPage} onClick={() => void query.fetchNextPage()}>{query.isFetchingNextPage ? '正在加载…' : '加载更多'}</button>}
  </section>;
}
