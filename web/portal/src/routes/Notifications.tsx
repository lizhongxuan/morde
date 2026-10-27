import { useState } from 'react';
import { useInfiniteQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import type { components } from '../api/schema';
import { errorDetails, portalApi } from '../app/api';
import { formatDate } from '../app/format';

type Notification = components['schemas']['Notification'];
const kindLabel: Record<Notification['kind'], string> = { completed: '已完成', failed: '失败', waiting_user: '待处理（已停用）' };

export function Notifications() {
  const client = useQueryClient();
  const [unreadOnly, setUnreadOnly] = useState(false);
  const notifications = useInfiniteQuery({
    queryKey: ['notifications', unreadOnly], queryFn: ({ pageParam }) => portalApi.notifications(pageParam, unreadOnly),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (page) => page.next_cursor ?? undefined,
  });
  const markRead = useMutation({
    mutationFn: (id: string) => portalApi.markNotificationRead(id),
    onSuccess: async () => { await client.invalidateQueries({ queryKey: ['notifications'] }); },
  });
  const items = notifications.data?.pages.flatMap((page) => page.items) ?? [];
  const unreadCount = items.filter((item) => item.read_at === null).length;

  return <section>
    <div className="page-heading"><div><p className="eyebrow">站内消息</p><h1>通知</h1><p className="muted">这里保留历史通知，标记已读不会触发任何操作。</p></div></div>
    <div className="notification-filters" aria-label="通知筛选">
      <button type="button" className={`button button-small ${unreadOnly ? 'button-secondary' : 'button-primary'}`} aria-pressed={!unreadOnly} onClick={() => setUnreadOnly(false)}>全部</button>
      <button type="button" className={`button button-small ${unreadOnly ? 'button-primary' : 'button-secondary'}`} aria-pressed={unreadOnly} onClick={() => setUnreadOnly(true)}>未读</button>
    </div>
    {markRead.isError && <p className="form-error notification-error" role="alert">标记已读失败：{errorDetails(markRead.error).message}</p>}
    {notifications.isPending ? <div className="loading-card" role="status">正在加载通知…</div>
      : notifications.isError && !notifications.data ? <div className="error-card" role="alert">无法读取通知：{errorDetails(notifications.error).message}<button type="button" className="button button-secondary" onClick={() => void notifications.refetch()}>重试</button></div>
        : !items.length ? <div className="empty-card"><h2>{unreadOnly ? '没有未读通知' : '还没有通知'}</h2><p>目前没有可查看的历史消息。</p></div>
          : <>
            <p className="muted notification-count">{unreadOnly ? `当前已加载 ${items.length} 条未读通知。` : `当前已加载 ${items.length} 条通知，未读 ${unreadCount} 条。`}</p>
            <div className="notification-list">{items.map((item) => <article className={`notification-card ${item.read_at ? 'notification-read' : 'notification-unread'}`} key={item.id}>
              <header><div><span className={`status ${item.read_at ? 'status-disabled' : 'status-running'}`}><span aria-hidden="true" className="status-dot" />{kindLabel[item.kind]}</span>{item.read_at && <span className="muted">已读</span>}</div><time className="muted" dateTime={item.created_at}>{formatDate(item.created_at)}</time></header>
              <p>{item.redacted || item.summary === null ? '相关内容因当前权限而隐藏。' : item.summary}</p>
              <footer>{!item.read_at && <button type="button" className="button button-quiet button-small" disabled={markRead.isPending} onClick={() => markRead.mutate(item.id)}>标记已读</button>}</footer>
            </article>)}</div>
            {notifications.isFetchNextPageError && <div className="error-card notification-pagination-error" role="alert">加载更多通知失败：{errorDetails(notifications.error).message}<button type="button" className="button button-secondary" onClick={() => void notifications.fetchNextPage()}>重试</button></div>}
            {notifications.hasNextPage && !notifications.isFetchNextPageError && <button type="button" className="button button-secondary notification-load-more" disabled={notifications.isFetchingNextPage} onClick={() => void notifications.fetchNextPage()}>{notifications.isFetchingNextPage ? '正在加载…' : '加载更多通知'}</button>}
          </>}
  </section>;
}
