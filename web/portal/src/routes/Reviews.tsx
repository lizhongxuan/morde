import { useRef } from 'react';
import { Link } from 'react-router';
import { useInfiniteQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { errorDetails, newIdempotencyKey, portalApi } from '../app/api';
import { formatDate } from '../app/format';
import { useToast } from '../app/toast';
import { TaskApprovalCard, type TaskApprovalDecision } from '../components/TaskApprovalCard';
import { FloatingError } from '../components/FloatingError';
import type { components } from '../api/schema';

type TaskApprovalItem = components['schemas']['TaskApprovalItem'];

export function Reviews() {
  const client = useQueryClient();
  const { notify } = useToast();
  const actionKeys = useRef(new Map<string, { fingerprint: string; key: string }>());
  const approvals = useInfiniteQuery({
    queryKey: ['task-approvals', 'all'],
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam }) => portalApi.taskApprovals({ cursor: pageParam }),
    getNextPageParam: (page, pages) => page.next_cursor && !pages.slice(0, -1).some((item) => item.next_cursor === page.next_cursor)
      ? page.next_cursor : undefined,
    refetchInterval: (query) => query.state.data?.pages.some((page) => page.items.some((item) => item.approval.status === 'pending')) ? 5000 : 30000,
  });
  const respond = useMutation({
    mutationFn: ({ item, decision, key }: { item: TaskApprovalItem; decision: TaskApprovalDecision; key: string }) =>
      portalApi.respondToTaskApproval(item.task_id, item.approval.id,
        { version: item.approval.version, decision }, key),
    onSuccess: async (_response, request) => {
      actionKeys.current.delete(`approval:${request.item.task_id}:${request.item.approval.id}`);
      await Promise.all([
        client.invalidateQueries({ queryKey: ['task-approvals'] }),
        client.invalidateQueries({ queryKey: ['task', request.item.task_id] }),
        client.invalidateQueries({ queryKey: ['conversation-tasks', request.item.conversation_id] }),
        client.invalidateQueries({ queryKey: ['assistant-account-tasks'] }),
      ]);
      notify('审核已受理，任务后续状态等待更新。', 'success');
    },
    onError: (error) => notify(`审核未确认：${errorDetails(error).message}`, 'error'),
  });
  const pages = approvals.data?.pages ?? [];
  const items = [...new Map(pages.flatMap((page) => page.items).map((item) => [item.approval.id, item])).values()];
  const pending = items.filter((item) => item.approval.status === 'pending');
  const handled = items.filter((item) => item.approval.status !== 'pending');
  const pendingCount = pages[0]?.pending_count;
  const lastCursor = pages.at(-1)?.next_cursor;
  const cursorLoop = Boolean(lastCursor && pages.slice(0, -1).some((page) => page.next_cursor === lastCursor));
  const approvalStatus = (item: TaskApprovalItem) => item.approval.status === 'pending' ? '待审核'
    : ({ approved: '已允许', rejected: '已拒绝', cancelled: '已取消', expired: '已过期' } as const)[item.approval.status];
  const approvalKey = (item: TaskApprovalItem, decision: TaskApprovalDecision) => {
    const scope = `approval:${item.task_id}:${item.approval.id}`;
    const fingerprint = JSON.stringify({ version: item.approval.version, decision });
    const previous = actionKeys.current.get(scope);
    if (previous?.fingerprint === fingerprint) return previous.key;
    const key = newIdempotencyKey();
    actionKeys.current.set(scope, { fingerprint, key });
    return key;
  };
  const renderItem = (item: TaskApprovalItem) => <article className="review-item" key={item.approval.id}>
    <header className="review-item-heading">
      <strong>{item.approval.title}</strong><span>{approvalStatus(item)}</span>
    </header>
    <p className="review-item-context">{item.approval.object} · 任务更新于 {formatDate(item.task_updated_at)}</p>
    <TaskApprovalCard approval={item.approval}
      submitting={respond.isPending && respond.variables?.item.approval.id === item.approval.id}
      onDecision={(decision) => respond.mutate({ item, decision, key: approvalKey(item, decision) })} />
    <Link className="button button-small button-secondary review-open-conversation" to="/assistant"
      state={{ conversationID: item.conversation_id }}>打开所属对话</Link>
  </article>;

  return <div className="reviews-page">
    <header className="reviews-heading"><div><h1>审核</h1><p>查看并处理需要你确认的操作。</p></div>
      {pendingCount !== undefined && <span className="reviews-pending-count" aria-label={`${pendingCount} 项待处理`}>{pendingCount} 项待处理</span>}
    </header>
    {approvals.isPending && <p className="muted" role="status">正在读取审核事项…</p>}
    {approvals.isError && <FloatingError className="error-inline" role="alert">审核事项暂不可用，待处理数量和结果均未确认。
      <button type="button" className="button button-small button-secondary" onClick={() => void approvals.refetch()}>重试</button></FloatingError>}
    {approvals.isSuccess && <>
      {pendingCount === undefined && <FloatingError className="error-inline" role="alert">待处理数量暂不可用。</FloatingError>}
      <section className="reviews-section" aria-label="待处理审核">
        <h2>待处理 {pendingCount === undefined ? '' : `· ${pendingCount}`}</h2>
        {pending.length ? <div className="reviews-list">{pending.map(renderItem)}</div>
          : pendingCount === 0 ? <p className="muted">当前没有待处理审核。</p>
            : <p className="muted" role="status">列表中尚未确认待处理事项；请刷新核对。</p>}
      </section>
      <section className="reviews-section" aria-label="已处理审核">
        <h2>已处理</h2>
        {handled.length ? <div className="reviews-list">{handled.map(renderItem)}</div>
          : <p className="muted">还没有已处理的审核记录。</p>}
      </section>
      {cursorLoop && <FloatingError className="error-inline" role="alert">审核列表分页游标重复，尚未加载的事项状态未确认。</FloatingError>}
      {approvals.hasNextPage && <div className="reviews-load-more">
        {approvals.isFetchNextPageError && <FloatingError className="error-inline" role="alert">更多审核事项暂不可用。</FloatingError>}
        <button type="button" className="button button-secondary" disabled={approvals.isFetchingNextPage}
          onClick={() => void approvals.fetchNextPage()}>{approvals.isFetchingNextPage ? '正在加载…'
            : approvals.isFetchNextPageError ? '重试加载更多' : '加载更多审核事项'}</button>
      </div>}
    </>}
  </div>;
}
