import { useRef } from 'react';
import { useInfiniteQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { errorDetails, newIdempotencyKey, portalApi } from '../app/api';
import { useToast } from '../app/toast';
import { isTransientNetworkError } from '../api/errors';
import { TaskApprovalCard, type TaskApprovalDecision } from '../components/TaskApprovalCard';
import type { components } from '../api/schema';
import './Reviews.css';

type TaskApprovalItem = components['schemas']['TaskApprovalItem'];

function ReviewsError({ title, message, onRetry }: { title: string; message: string; onRetry: () => void }) {
  return <div className="reviews-error" role="alert">
    <span className="reviews-error-mark" aria-hidden="true">!</span>
    <div className="reviews-error-copy"><span className="reviews-error-title">{title}</span><p>{message}</p></div>
    <button type="button" className="button button-small button-secondary" onClick={onRetry}>重试</button>
  </div>;
}

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
  const { fetchNextPage, isFetchNextPageError, refetch } = approvals;
  const retryApprovals = () => void (isFetchNextPageError ? fetchNextPage() : refetch());
  const approvalsNetworkError = approvals.isError && isTransientNetworkError(approvals.error);
  const refreshApprovalState = (item: TaskApprovalItem) => Promise.all([
    client.invalidateQueries({ queryKey: ['task-approvals'] }),
    client.invalidateQueries({ queryKey: ['task', item.task_id] }),
    client.invalidateQueries({ queryKey: ['conversation-tasks', item.conversation_id] }),
    client.invalidateQueries({ queryKey: ['assistant-account-tasks'] }),
  ]);
  const respond = useMutation({
    mutationFn: ({ item, decision, key }: { item: TaskApprovalItem; decision: TaskApprovalDecision; key: string }) =>
      portalApi.respondToTaskApproval(item.task_id, item.approval.id,
        { version: item.approval.version, decision }, key),
    onSuccess: async (_response, request) => {
      actionKeys.current.delete(`approval:${request.item.task_id}:${request.item.approval.id}`);
      await refreshApprovalState(request.item);
      notify('审核已受理，任务后续状态等待更新。', 'success');
    },
    onError: (error, request) => {
      void refreshApprovalState(request.item);
      notify(`审核未确认：${errorDetails(error).message}`, 'error');
    },
  });
  const pages = approvals.data?.pages ?? [];
  const items = [...new Map(pages.flatMap((page) => page.items).map((item) => [item.approval.id, item])).values()];
  const pending = items.filter((item) => item.approval.status === 'pending');
  const handled = items.filter((item) => item.approval.status !== 'pending');
  const pendingCount = pages[0]?.pending_count;
  const lastCursor = pages.at(-1)?.next_cursor;
  const cursorLoop = Boolean(lastCursor && pages.slice(0, -1).some((page) => page.next_cursor === lastCursor));
  const approvalKey = (item: TaskApprovalItem, decision: TaskApprovalDecision) => {
    const scope = `approval:${item.task_id}:${item.approval.id}`;
    const fingerprint = JSON.stringify({ version: item.approval.version, decision });
    const previous = actionKeys.current.get(scope);
    if (previous?.fingerprint === fingerprint) return previous.key;
    const key = newIdempotencyKey();
    actionKeys.current.set(scope, { fingerprint, key });
    return key;
  };
  const renderItem = (item: TaskApprovalItem) => <TaskApprovalCard key={item.approval.id} compact approval={item.approval}
      submitting={respond.isPending && respond.variables?.item.approval.id === item.approval.id}
      onDecision={(decision) => respond.mutate({ item, decision, key: approvalKey(item, decision) })} />;

  const showApprovalData = approvals.isSuccess || ((approvalsNetworkError || approvals.isFetchNextPageError) && Boolean(approvals.data));
  const listError = approvals.isError && !approvals.isFetchNextPageError;
  const listErrorHasData = approvalsNetworkError && Boolean(approvals.data);
  const listErrorDetails = approvals.isError ? errorDetails(approvals.error) : undefined;
  const listErrorTitle = listErrorHasData ? '连接中断，已加载内容仍保留'
    : listErrorDetails?.code === 'forbidden' ? '当前账号无权读取审核事项'
      : listErrorDetails?.code === 'not_found' ? '审核列表暂不可用' : '审核事项暂时无法读取';
  const listErrorMessage = listErrorHasData ? '待处理数量和最新审核结果尚未确认，请恢复连接后重试。'
    : listErrorDetails?.code === 'forbidden' ? '请联系管理员确认审核权限。待处理数量和审核结果尚未确认。'
      : listErrorDetails?.code === 'not_found' ? '审核服务未返回列表，请联系管理员检查服务配置。待处理数量和审核结果尚未确认。'
        : `${listErrorDetails?.message ?? '读取审核事项失败'}。待处理数量和审核结果尚未确认。`;
  return <div className="reviews-page">
    <header className="reviews-heading"><div><h1>审核</h1><p>只显示需要你允许或拒绝的操作；任务追问、等待和异常状态请在任务动态查看。</p></div>
      {pendingCount !== undefined && <span className="reviews-pending-count"
        aria-label={listErrorHasData ? `上次确认 ${pendingCount} 项待你确认` : `${pendingCount} 项待你确认`}>
        {listErrorHasData ? `上次确认 ${pendingCount} 项待你确认` : `${pendingCount} 项待你确认`}
      </span>}
    </header>
    {approvals.isPending && <p className="reviews-loading" role="status">正在读取审核事项…</p>}
    {listError && <ReviewsError title={listErrorTitle} message={listErrorMessage} onRetry={retryApprovals} />}
    {showApprovalData && <>
      {pendingCount === undefined && <ReviewsError title="待处理数量尚未确认"
        message="列表已加载，但服务端未返回待处理总数。刷新后再核对。" onRetry={() => void refetch()} />}
      <section className="reviews-section" aria-label="待你确认的操作">
        <h2>待你确认{pendingCount === undefined ? '' : ` · ${pendingCount}`}</h2>
        {pending.length ? <div className="reviews-list">{pending.map(renderItem)}</div>
          : pendingCount === 0 ? <p className="reviews-empty">当前没有需要你确认的操作。</p>
            : <p className="reviews-empty" role="status">列表中尚未确认待处理事项；请刷新核对。</p>}
      </section>
      {handled.length > 0 && <details className="reviews-history">
        <summary>已处理的操作 · {handled.length}</summary>
        <section className="reviews-section" aria-label="已处理审核">
          <div className="reviews-list">{handled.map(renderItem)}</div>
        </section>
      </details>}
      {cursorLoop && <ReviewsError title="审核列表需要刷新"
        message="分页游标重复，尚未加载的事项状态未确认。" onRetry={() => void refetch()} />}
      {approvals.hasNextPage && <div className="reviews-load-more">
        {approvals.isFetchNextPageError ? <ReviewsError title="更多审核事项未能加载"
          message={`${errorDetails(approvals.error).message}。已加载内容仍保留。`} onRetry={() => void fetchNextPage()} />
          : <button type="button" className="button button-secondary" disabled={approvals.isFetchingNextPage}
            onClick={() => void approvals.fetchNextPage()}>{approvals.isFetchingNextPage ? '正在加载…' : '加载更多审核事项'}</button>}
      </div>}
    </>}
  </div>;
}
